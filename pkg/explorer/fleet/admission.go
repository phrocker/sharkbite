// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package fleet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Admission answers the one question no other surface in Shoal answers: an
// out-of-process caller holding a side effect it has not yet performed asking
// whether it may perform it.
//
// Every other enforcement surface here adjudicates work Shoal itself does, and
// every other control *withholds* — it removes evidence from a response that is
// produced anyway. A caller about to post a prompt to a hosted model cannot
// withhold: it does not assemble the response, and by the time content exists
// the transmission has already happened. It needs a decision before the effect,
// which is what this is.
//
// It is deliberately not a new durable object. An admission is a dispatch
// action in the claimed state, and the token is its claim: the same store, the
// same fence, the same lifecycle events, the same audit phases, the same replay
// semantics. That is not an economy — it is the same situation. A remote worker
// that has claimed an action and a proxy that has been admitted are both
// out-of-process actors holding permission Shoal granted and has not yet seen
// the outcome of, and inventing a second record for the second case would have
// given the two different durability, different reconciliation, and a second
// place for the two to disagree.
//
// Nothing here enforces that a caller honours what it is told. It cannot: no
// plane outside the caller's own process can. This is a declaration seam of the
// same kind as the execution boundary, and a caller that ignores an obligation
// is misreporting its own configuration exactly as a host that binds an
// external-effect executor under an evidence-only ceiling is.
var (
	// ErrAdmissionSpent reports a token that no longer names a live admission:
	// already reported, denied, cancelled, expired, or never issued. The cases
	// are deliberately one error. Telling a caller which one it hit would say
	// whether an admission it does not hold exists.
	ErrAdmissionSpent = errors.New("fleet admission: token is spent")
	// ErrAdmissionConflict reports an admission identity already granted to a
	// different token.
	ErrAdmissionConflict = errors.New("fleet admission: admission conflict")
	// ErrAdmissionUnmigrated reports that the store still holds an admission
	// written under the identity scheme that preceded the derived one, which
	// this build cannot adjudicate around without risking a second grant for a
	// request that already has one.
	//
	// It is a whole-store condition rather than a per-caller one, deliberately:
	// see requireMigrated for why no per-record answer is correct. So it says
	// nothing about the caller that received it and cannot be used to probe for
	// anyone's records.
	//
	// Loud, and not a denial. A denial is a policy answer the caller should act
	// on; this is an operator's problem and the caller can do nothing but stop.
	ErrAdmissionUnmigrated = errors.New(
		"fleet admission: the store holds admissions written under the " +
			"superseded identity scheme, which must be drained before " +
			"admissions can be served")
	// ErrAdmissionSpanOccupied reports an ordinary action stored inside the
	// reserved identity span, which was a legal place to put one before the
	// span was reserved.
	//
	// Separate from ErrAdmissionUnmigrated because it is a different rollout
	// condition with a different remedy, and an operator reading one should not
	// go looking for the other. Both are whole-store conditions and neither
	// names a record.
	ErrAdmissionSpanOccupied = errors.New(
		"fleet admission: the store holds ordinary actions inside the " +
			"reserved admission identity span, which must be cleared before " +
			"admissions can be served")
)

// MaxAdmissionDisclosures bounds the corpus references one admission may
// declare. It is the evidence bound, not a new one: an admission declares what
// a single call would carry, and a call carrying more anchors than an action
// may record is not a call this plane can reason about.
const MaxAdmissionDisclosures = MaxActionEvidence

// AdmissionOutcome is what the caller is told. There are three, and the middle
// one is the one that matters: refusing a whole call is blunt, and "you may
// proceed without these" is a decision a caller can comply with instead of
// being stopped.
type AdmissionOutcome string

const (
	// AdmissionDenied means the call must not happen.
	AdmissionDenied AdmissionOutcome = "denied"
	// AdmissionAllowed means it may happen as declared.
	AdmissionAllowed AdmissionOutcome = "allowed"
	// AdmissionObligated means it may happen only with the obligations
	// satisfied.
	AdmissionObligated AdmissionOutcome = "allowed_with_obligations"
)

// AdmissionToken is what a caller returns on the matching report.
//
// It carries no authority of its own. Every field is re-checked against the
// durable action before a report is accepted — the principal through
// authorizedCurrent, the claim through the fence, the version through the
// store's compare-and-set — so a forged or edited token buys a caller nothing
// it could not already do with its own credentials. That is why it is a plain
// structure rather than a sealed blob: a token whose bytes were trusted would
// have to be unforgeable, and one whose bytes are never trusted does not.
type AdmissionToken struct {
	ActionID  []byte    `json:"action_id"`
	TokenID   []byte    `json:"token_id"`
	Version   uint64    `json:"version"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (t AdmissionToken) validate() error {
	if err := validateOpaque("admission action ID", t.ActionID, false); err != nil {
		return err
	}
	if err := validateOpaque("admission token ID", t.TokenID, false); err != nil {
		return err
	}
	if t.Version == 0 {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "admission token version is invalid")
	}
	return nil
}

// Obligations is what an admitted caller must do before it performs the call.
//
// Withhold names only references the caller supplied in its own request, so the
// response discloses nothing the caller did not already know. Nothing here says
// *why* a reference must be withheld, and that is the point: separating "you
// are not authorized for this" from "your co-occurrence budget is spent" would
// turn admission into an authorization oracle over document identities, where a
// caller learns which of a guessed set it may read by reading the reason. The
// authorized read path reports those two classes apart because there the counts
// describe a corpus the caller is already reading; here they would describe
// identities the caller merely named.
type Obligations struct {
	// Withhold is the subset of the request's declared references that must not
	// appear in what the caller sends.
	Withhold []shoal.ID
}

func (o Obligations) empty() bool { return len(o.Withhold) == 0 }

// AdmissionRequest is a caller stating what it is about to do.
//
// Input is the declaration, not the payload. Shoal never receives the prompt:
// it would be a copy of exactly the content the caller is asking permission to
// transmit, held by the plane whose job is to decide whether transmitting it is
// allowed. What the declaration must contain is the action's registered input
// schema, so a host decides what an admission has to say for itself.
type AdmissionRequest struct {
	ID              []byte
	IdempotencyKey  []byte
	TokenID         []byte
	AgentID         shoal.ID
	AgentGeneration int64
	Capability      string
	Action          string
	SourceID        []byte
	PolicyID        []byte
	ObjectID        shoal.ID
	// Effects is what this call would do. It must be non-empty: an admission
	// that declares nothing is unclassifiable, and an unclassifiable input is
	// treated as the dangerous case everywhere else in this package.
	Effects Effects
	Input   json.RawMessage
	// Disclosures are the corpus references the caller says its payload would
	// carry. They are what obligations are computed over and expressed in.
	Disclosures []shoal.ID
	Lease       time.Duration
	Context     RequestContext
}

// AdmissionGrant is the answer.
//
// A denial carries no reason. Naming the control that refused would name a
// policy, and a caller that can enumerate denials by varying its request has
// read the policy out of the decision plane one bit at a time. A caller that
// needs to distinguish "Shoal refused" from "Shoal was unreachable" gets that
// from the transport: a refusal is a successful response carrying
// AdmissionDenied, an unreachable plane is a transport failure, and the two are
// never confusable.
type AdmissionGrant struct {
	Outcome     AdmissionOutcome
	Token       AdmissionToken
	Obligations Obligations
}

// AdmissionReport is the caller coming back with what actually happened.
//
// Without it admission is a stateless gate: the plane would grant permission
// and never learn whether the call occurred, what it cost, or whether the
// obligations were satisfiable. The report is the edge that makes this a loop,
// and it is the input a cross-call accumulator would charge. Nothing here
// accumulates; the record is durable and an accumulator attaches to the same
// lifecycle events every other dispatch transition publishes.
type AdmissionReport struct {
	Token AdmissionToken
	// Outcome is what happened, shaped by the action's registered output
	// schema.
	Outcome json.RawMessage
	// Failed reports that the call did not happen or did not succeed. ErrorCode
	// carries the reason and is required when Failed is set.
	Failed    bool
	ErrorCode string
	// Effected is how much of an irreversible egress happened before the
	// failure, for the one boundary where a failure follows a partial effect
	// (#427).
	//
	// A streamed completion that breaks halfway has already put bytes in
	// front of the caller and they cannot be recalled. Volume is then the
	// only thing left to report about an egress that could not be prevented,
	// and without it three different events arrived identically as
	// response_truncated: nothing left, fifty bytes left, two megabytes
	// left. The first is a non-event and the third is a near-complete
	// disclosure, and #389 consumes these reports to shape the next
	// decision.
	//
	// Valid only on a failure, and only for an action that declares it may
	// egress. Both are refused rather than recorded, so the field cannot
	// become a free-form side channel on an action that egresses nothing.
	Effected EffectedVolume
	Context  RequestContext
}

// EffectedVolume is how much left, in units the plane already understands.
//
// An UPPER BOUND on what may have left, never a receipt. It counts bytes
// handed to the transport, which is the most any sender can know: kernel
// buffers and intermediate proxies sit between the write and the reader, so
// "N bytes were received" is not a statement a gateway can make. Read it as
// "at most N bytes may have reached the caller".
//
// That is also why it is never refined. Both gateways know the final count
// at the moment they report — the LLM gateway after its response handler has
// returned, when the bytes passed to the ResponseWriter can no longer grow;
// the effects gateway when its send completes or fails, with any partial
// write counted by the dialer's wrapped conn — so the first report is the
// whole report, which is what lets the field be write-once.

// Fixed integers rather than a unit/value pair, deliberately. A label would
// be caller-controlled text on a durable record, and this record's byte
// accounting is adjacent to the claim-holder chain whose bound has already
// bricked actions once. Tokens are derived from bytes by a gateway that has a
// tokenizer; that is the gateway's observation for #389 and not something the
// plane carries.
type EffectedVolume struct {
	Bytes  int64 `json:"bytes,omitempty"`
	Chunks int64 `json:"chunks,omitempty"`
}

// Zero reports whether nothing left.
func (v EffectedVolume) Zero() bool { return v.Bytes == 0 && v.Chunks == 0 }

// MaxEffectedBytes and MaxEffectedChunks bound a reported volume. They are
// ceilings on what is representable, not on what may egress: a report above
// them is a report this plane cannot store, and is refused rather than
// clamped, because a clamped number read as exact would understate a
// disclosure.
const (
	MaxEffectedBytes  int64 = 1 << 40
	MaxEffectedChunks int64 = 1 << 24
)

func (v EffectedVolume) validate() error {
	if v.Bytes < 0 || v.Chunks < 0 {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "effected volume is negative")
	}
	if v.Bytes > MaxEffectedBytes || v.Chunks > MaxEffectedChunks {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "effected volume exceeds its bound")
	}
	// Chunks with no bytes is a contradiction: a chunk that left carried
	// something. Bytes with no chunk count is fine — a caller that does not
	// frame its egress still knows how much of it there was.
	if v.Chunks > 0 && v.Bytes == 0 {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"effected volume counts chunks but no bytes")
	}
	return nil
}

// OutstandingAdmission is an admission that was granted and never reported.
//
// It is the observable form of the gap the report closes. A caller that is
// admitted and then goes silent has been told it may perform an effect, and the
// record must not read as though nothing was granted — otherwise a crashed
// proxy and a proxy that never asked are indistinguishable.
type OutstandingAdmission struct {
	ActionID   []byte
	TokenID    []byte
	Version    uint64
	AdmittedAt time.Time
	ExpiresAt  time.Time
	// Expired reports that the token can no longer be reported against. Such an
	// admission is not resolved, it is abandoned: whether the effect happened is
	// unknown and stays unknown.
	Expired bool
}

type OutstandingAdmissionsRequest struct {
	After   []byte
	Limit   int
	Context RequestContext
}

type OutstandingAdmissionsPage struct {
	Admissions []OutstandingAdmission
	Next       []byte
}

// DisclosureRestrictor narrows a caller-declared reference set to what that
// caller may still disclose, and is how an existing withholding control becomes
// an obligation instead of a refusal.
//
// The intended implementation is the authorized client's co-occurrence budget,
// which already computes exactly this and today can only express it by dropping
// documents out of a result it is assembling. A caller that assembles its own
// payload cannot be served that way; it has to be told.
//
// Returning a reference does not admit it: the result is intersected with what
// authorization already permitted, so an implementation can only ever narrow.
// An error is not an empty result — it propagates, because a restrictor that
// cannot answer has not said "withhold nothing".
type DisclosureRestrictor interface {
	RestrictDisclosure(context.Context, []shoal.ID) ([]shoal.ID, error)
}

type AdmissionConfig struct {
	Dispatch *DispatchService
	// Restrictor is optional. Absent, obligations come from authorization alone
	// — which matches the co-occurrence budget's own default, where a zero
	// bound disables the control. A deployment that enables the budget and does
	// not wire it here gets obligations narrower than the control it
	// configured, so the wiring is a deployment invariant rather than something
	// this service can check.
	Restrictor DisclosureRestrictor
}

type AdmissionService struct {
	dispatch   *DispatchService
	restrictor DisclosureRestrictor
	// migrationMu guards migrated, and is held across the verifying scan so a
	// burst of first requests performs one scan rather than one each.
	migrationMu sync.Mutex
	migrated    bool
}

func NewAdmissionService(config AdmissionConfig) (*AdmissionService, error) {
	if config.Dispatch == nil {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "fleet admission dependencies are required")
	}
	return &AdmissionService{
		dispatch: config.Dispatch, restrictor: config.Restrictor,
	}, nil
}

// Request adjudicates a call that has not happened yet.
//
// Every check happens before anything durable is written, and the answer is
// then committed as exactly one record: a grant is a claimed action, a refusal
// is a cancelled one. An admission never passes through the queued state.
//
// That is not tidiness, it is the stop. Writing a queued record first and
// deciding afterwards opens a window in which an ungranted — possibly refused —
// admission exists as an action waiting for a worker. Pull returns it to the
// same principal, Claim on the dispatch surface hands out a live claim against
// it, and under the execution boundary a claim is permission to perform the
// declared effect out of process. A refusal would become permission through a
// different door, which is precisely what this surface exists to prevent.
//
// Cancelling the queued record when a later step fails does not close that
// window. The cancel is a second mutation that can fail for the same reasons
// the first step did, and when it does the orphan is still there with nothing
// to show for the attempt. The only failure mode that is actually safe is
// having written nothing: the caller receives a transport error and fails
// closed, which is what a caller of this surface must do with an unreachable
// decision plane anyway.
func (s *AdmissionService) Request(
	ctx context.Context, request AdmissionRequest,
) (AdmissionGrant, error) {
	dispatch := s.dispatch
	ctx, cancel := dispatch.deadline(ctx, request.Context)
	defer cancel()
	decision, now, err := dispatch.begin(ctx, auth.OperationInvoke, request.Context)
	if err != nil {
		return AdmissionGrant{}, err
	}
	// Before anything else, including any read of the caller's own identity:
	// an admission written under the superseded identity scheme makes every
	// answer here unsound, because the derivation moved the durable key and a
	// request for such a record misses.
	if err := s.requireMigrated(ctx); err != nil {
		return AdmissionGrant{}, err
	}
	if err := validateOpaque("admission token ID", request.TokenID, false); err != nil {
		return AdmissionGrant{}, err
	}
	if request.Lease <= 0 || request.Lease > MaxActionClaimTTL {
		return AdmissionGrant{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "admission lease is outside its bound")
	}
	// An empty declared set is refused rather than read as "nothing". The set is
	// the whole of what an admission is admitting, so an admission that declares
	// nothing would be the cheapest possible request and the most permissive
	// answer.
	if len(request.Effects) == 0 {
		return AdmissionGrant{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "admission must declare an effect")
	}
	declared, err := canonicalEffects(request.Effects)
	if err != nil {
		return AdmissionGrant{}, err
	}
	disclosures, err := canonicalDisclosures(request.Disclosures)
	if err != nil {
		return AdmissionGrant{}, err
	}
	if err := validateOpaque("admission ID", request.ID, false); err != nil {
		return AdmissionGrant{}, err
	}
	// The durable identity, which is not the one the caller supplied. See
	// admissionActionID: the caller names its admission, the durable namespace
	// is global, and without this two principals naming the same admission
	// would be able to detect each other.
	actionID := admissionActionID(decision, request.ID)
	// The same record an enqueue of this request would build, validated
	// identically and not written. It carries the declaration, so what was
	// admitted is part of the record's identity and a retry cannot change it.
	base, action, executorRef, err := dispatch.queuedRecordBinding(ctx, decision, EnqueueRequest{
		ID: actionID, IdempotencyKey: request.IdempotencyKey,
		AgentID: request.AgentID, AgentGeneration: request.AgentGeneration,
		Capability: request.Capability, Action: request.Action,
		SourceID: request.SourceID, PolicyID: request.PolicyID,
		ObjectID: request.ObjectID, Input: request.Input,
		Context: request.Context,
	}, auth.OperationInvoke, now,
		false,
	)
	if err != nil {
		return AdmissionGrant{}, err
	}
	base.AdmittedEffects = declared
	base.AdmittedDisclosures = disclosureDigest(disclosures)
	// Recorded, not inferred. The verdict asks the record which scheme produced
	// its key, because the key cannot answer: a caller-named legacy key may
	// have any shape, including the shape a derived one has.
	base.AdmittedIdentityScheme = AdmittedIdentitySchemeDerived

	// Read at the derived identity, which is why this is a raw store read and
	// not authorizedCurrent. A principal check here would have nothing to
	// refuse: no other principal's record can be at this ID.
	current, readErr := dispatch.store.GetAction(ctx, actionID)
	if readErr != nil && !errors.Is(readErr, ErrActionNotFound) {
		return AdmissionGrant{}, readErr
	}
	if readErr == nil {
		return s.replay(request, disclosures, current, base, now)
	}
	// An action that requires approval cannot be served here (#451), and the
	// answer is a durable denial like any other: no reason, so a caller cannot
	// tell this control from the effect ceiling or anything else that refuses.
	// Holding the call instead is not available on this path — the caller
	// already holds the payload and is waiting on the answer, and a grant
	// issued after a human decided would be a grant for a call whose moment
	// has passed. Approval-required work goes through the approval route,
	// where nothing is performed until an approver has decided.
	//
	// After the replay branch, so an admission granted or denied before the
	// flag was registered keeps answering from its record, exactly as a queued
	// dispatch record does.
	if action.RequiresApproval {
		return s.deny(ctx, base, decision)
	}
	// The declaration is checked against what the descriptor permits, resolved
	// under this decision. The ceiling the executor was bound to is already
	// enforced inside resolveActionBinding, so an action can neither declare
	// more than its executor may do nor be admitted for more than it declares.
	if declared.exceeds(action.Effects) {
		return s.deny(ctx, base, decision)
	}
	obligations, err := s.obligations(ctx, decision, request, disclosures, now)
	if err != nil {
		return AdmissionGrant{}, err
	}
	// The grant is born claimed, so it is a claim grant like any other and
	// goes through applyClaim's attestation gate (#446). Leaving it out would
	// make admission the bypass: Path B grants exactly the permission to
	// perform an external effect that Path A's Claim gates.
	//
	// Read after every other refusal above, so the attestation store is
	// consulted only for a request that would otherwise be granted. A store
	// failure answers unavailable and writes nothing — it is not a denial,
	// because a denial is durable and an outage is not a decision.
	attestation, err := dispatch.claimAttestation(ctx, decision,
		effectiveClaimRequirements(base, action), executorRef, now)
	if err != nil {
		return AdmissionGrant{}, err
	}
	// An admission grant is requested and reported by one identity under
	// invoke, so that is the operation its claim is authorized by.
	record, err := applyClaim(
		base, action, request.TokenID, request.Lease,
		decision, auth.OperationInvoke, now, attestation, executorRef)
	if errors.Is(err, ErrAttestationRequired) {
		// The existing durable denial, with no reason on the wire, exactly as
		// approval and the effect ceiling refuse: a caller cannot tell this
		// control from any other. The reason is audited for the operator.
		dispatch.auditAttestationRefusal(ctx, base, auth.OperationInvoke, decision)
		return s.deny(ctx, base, decision)
	}
	if err != nil {
		return AdmissionGrant{}, err
	}
	record.AdmittedObligation = obligationBitmap(disclosures, obligations)
	granted, err := s.commit(ctx, record)
	if err != nil {
		return AdmissionGrant{}, err
	}
	return grantFor(granted, obligations), nil
}

// requireMigrated refuses to adjudicate while the store is in a state the
// derived identity scheme does not describe.
//
// Two conditions, and neither is read off an identity.
//
// The first is an admission produced by the superseded scheme, which moved the
// durable key: a request for such a record misses, and granting on the miss
// issues a second live token for work already permitted. For a record that is
// already terminal it is worse — a durable denial would be re-adjudicated and
// could come back granted, inverting a refusal that is on the record.
//
// That is settled by the scheme marker rather than by where the identity sorts.
// Under the superseded scheme the key was caller-supplied opaque bytes, so a
// legacy key may lie anywhere, including inside the reserved span and including
// the exact prefix-plus-digest shape a derived key has. An earlier version
// inferred the scheme from the span and therefore certified exactly that record
// as new. The marker is sound for any identity, adversarial ones included,
// and the identity now plays no part.
//
// The second is an ordinary action inside the reserved span. The span was a
// legal dispatch identity space before it was reserved — action identities are
// arbitrary non-empty bytes and always have been — so a record created
// legitimately can sit there. Nothing silently mis-handles it any more: the
// listings exclude admissions by marker rather than by range, and ExecuteClaim
// no longer refuses an identity for its shape. What remains broken is narrow
// and real: enqueue refuses the span unconditionally, so such an action cannot
// be enqueue-replayed, and its idempotent retry fails. Refusing admissions is
// how that becomes visible to an operator instead of staying a latent oddity
// in a store nobody is looking at.
//
// Why whole-store rather than per-record. The derivation treats the
// authorization domain as part of the principal, and an ActionRecord does not
// carry its domain — so a per-record ownership predicate can be built only two
// ways and both are wrong. sameActionPrincipal omits the domain, which hands
// the distinctive error to an identically-named identity elsewhere and reopens
// the existence oracle. Recovering the domain through the agent descriptor is
// blind to any record whose generation has moved, which Heartbeat does on every
// lease renewal. A whole-store verdict has no ownership predicate to get wrong.
//
// Report and Outstanding deliberately do not consult this. A grant already
// issued must stay reportable, or upgrading strands the audit record for an
// effect that may already have happened — and those records are exactly what an
// operator has to find.
//
// The clean verdict is cached: this build writes no unmarked admission and
// refuses to create in the span, so absence once proven stays true. A dirty or
// unreachable verdict is never cached, so clearing the records needs no
// restart. The scan runs under the mutex so a burst of first requests performs
// one scan rather than one each.
func (s *AdmissionService) requireMigrated(ctx context.Context) error {
	s.migrationMu.Lock()
	defer s.migrationMu.Unlock()
	if s.migrated {
		return nil
	}
	// Bounded so the loop terminates, and set far above any plausible store: at
	// a full page each, this is tens of millions of actions. Exhausting it means
	// the state could not be established, which refuses rather than assumes.
	const maxMigrationScans = 1 << 16
	var cursor []byte
	for scanned := 0; scanned < maxMigrationScans; scanned++ {
		page, err := s.dispatch.store.ScanActions(
			ctx, cursor, MaxDispatchListResults)
		if err != nil {
			return err
		}
		for _, record := range page.Actions {
			if record.isAdmission() {
				if record.AdmittedIdentityScheme !=
					AdmittedIdentitySchemeDerived {
					return ErrAdmissionUnmigrated
				}
				continue
			}
			if reservedAdmissionID(record.ID) {
				return ErrAdmissionSpanOccupied
			}
		}
		if len(page.Next) == 0 {
			s.migrated = true
			return nil
		}
		cursor = page.Next
	}
	return shoal.NewError(
		shoal.ErrorUnavailable,
		"admission cannot establish the state of the action identity space")
}

// replay answers a request whose admission identity already exists.
//
// A stored record that is not this request is a conflict, never a second
// adjudication: the action ID and the token are the caller's to choose, and
// reusing them for a different declaration has to fail rather than quietly
// produce a different answer under the same token.
// It takes no context and no decision, and that absence is the point: a replay
// consults nothing. Both were parameters until the obligation became durable,
// and needing neither is how this function now demonstrates that it re-runs no
// control. The caller's authority is still checked — Request resolves the
// action binding under the decision before it ever gets here.
func (s *AdmissionService) replay(
	request AdmissionRequest,
	disclosures []shoal.ID,
	current, base ActionRecord,
	now time.Time,
) (AdmissionGrant, error) {
	if !equivalentEnqueue(current, base) {
		return AdmissionGrant{}, ErrAdmissionConflict
	}
	switch {
	case current.State == DispatchCanceled:
		// A refusal answers from the record rather than being re-adjudicated.
		// Re-running the decision would let a caller retry a denial until the
		// state it depended on moved, and the cancelled record is the durable
		// statement that this admission was refused.
		return AdmissionGrant{Outcome: AdmissionDenied}, nil
	case current.State != DispatchClaimed:
		// Already reported. The queued state also lands here and is currently
		// unreachable — an admission always declares an effect and a dispatch
		// enqueue never does, so equivalentEnqueue above has already refused
		// any queued record. The arm is total anyway, because the alternative
		// is a state this function silently treats as a live grant if the two
		// surfaces ever converge.
		return AdmissionGrant{}, ErrActionTerminal
	}
	// A live claim under a different token is someone else's admission for the
	// same identity and must not be handed over.
	if !bytes.Equal(current.ClaimID, request.TokenID) ||
		!now.Before(current.ClaimLeaseUntil) {
		return AdmissionGrant{}, ErrAdmissionConflict
	}
	// Replayed from the record, never recomputed. The obligation is the
	// decision this token was granted under, and the control it comes from is
	// windowed: recomputing it here would let a caller replay into a weaker
	// obligation while holding the same live token, and would make a grant
	// unrecoverable for as long as the restrictor was unreachable. The digest
	// compared above proves the declared list is the one these indices index.
	return grantFor(
		current, obligationFromBitmap(current.AdmittedObligation, disclosures),
	), nil
}

// obligationBitmap records which of the canonically ordered declared references
// the grant obliged the caller to withhold.
//
// Positions, not identities: the record must not carry another principal's
// corpus references, and a position is meaningless without the list it indexes.
// Nil when nothing is withheld, so an unobligated grant and a dispatched action
// store the same absence.
func obligationBitmap(
	disclosures []shoal.ID, obligations Obligations,
) []byte {
	if len(obligations.Withhold) == 0 {
		return nil
	}
	withheld := make(map[shoal.ID]struct{}, len(obligations.Withhold))
	for _, reference := range obligations.Withhold {
		withheld[reference] = struct{}{}
	}
	bitmap := make([]byte, (len(disclosures)+7)/8)
	for index, reference := range disclosures {
		if _, ok := withheld[reference]; ok {
			bitmap[index/8] |= 1 << (index % 8)
		}
	}
	return bitmap
}

// obligationFromBitmap rebuilds a stored obligation against the declared list
// the caller supplied again.
//
// A bit set beyond the declared list is ignored rather than rejected. The
// digest has already proven this is the same declaration the bitmap was built
// from, so a position past its end cannot come from a caller changing the list;
// it can only come from a tampered record, and the conservative reading of one
// is the obligation it can still express rather than none at all.
func obligationFromBitmap(bitmap []byte, disclosures []shoal.ID) Obligations {
	var result Obligations
	for index, reference := range disclosures {
		if index/8 < len(bitmap) && bitmap[index/8]&(1<<(index%8)) != 0 {
			result.Withhold = append(result.Withhold, reference)
		}
	}
	return result
}

// deny commits the refusal as a cancelled record.
//
// It is written directly rather than queued and then cancelled. A cancellation
// that has to be reached through the queued state is two mutations, and a
// failure between them leaves a refused admission sitting in the queue as
// claimable work.
//
// The cancel mutation key is the tuple digest over the admission's own identity
// — the same construction the executor key uses, reused rather than reinvented
// — so the record a retry would build is byte-identical to the one already
// stored and replays on to it.
func (s *AdmissionService) deny(
	ctx context.Context,
	base ActionRecord,
	decision auth.Decision,
) (AdmissionGrant, error) {
	record := base
	record.State = DispatchCanceled
	// Invoke, because that is what AdmissionService.Request authorized and
	// what commit audits below — and because the publisher reads this field to
	// decide which operation authorizes the action.canceled publication.
	//
	// Left unset, it was empty (nothing sets it at enqueue) while the
	// publisher hardcoded dispatch for action.canceled. The record's
	// AuthorizedOperations is [invoke], so the provenance check failed, the
	// publication failed, and every denial was answered ErrActionCommitted —
	// a 503 "requires reconciliation" for a refusal that had committed and
	// granted nothing. The retry then answered "denied", which is why it went
	// unnoticed: the second attempt looks correct.
	record.TransitionOperation = auth.OperationInvoke
	record.CancelKey = executorKey(
		base.ID, []byte("shoal.fleet.admission-denied.v1"))
	// Cancellation provenance is written even though this record is born
	// cancelled and the enqueue provenance would answer identically. A refusal
	// and a cancellation have to read the same way to an operator; a record
	// missing the fields its neighbours carry is one someone has to reason
	// about before trusting.
	record.CancelAuthorizationFingerprint = base.AuthorizationFingerprint
	record.CancelAuthorizationExpiresAt = decision.AuthenticationExpires()
	record.TransitionRequestID = decision.RequestID()
	record.TransitionCorrelationID = decision.CorrelationID()
	if _, err := s.commit(ctx, record); err != nil {
		return AdmissionGrant{}, err
	}
	return AdmissionGrant{Outcome: AdmissionDenied}, nil
}

// commit writes the one durable record an admission produces.
//
// The audit entry precedes the write, as it does for every other dispatch
// mutation, so no admission can be granted or refused without the privileged
// action record that says so. The transition kind is derived from the record's
// own state rather than passed in: a mismatch between the two is what
// NewActionTransition refuses, and deriving it makes the mismatch
// unconstructible.
func (s *AdmissionService) commit(
	ctx context.Context, record ActionRecord,
) (ActionRecord, error) {
	kind := actionEventKind(record)
	phase := "admission_grant"
	if record.State == DispatchCanceled {
		phase = "admission_denial"
	}
	if err := s.dispatch.recorder.RecordAction(ctx, ActionAudit{
		Phase: phase, Operation: auth.OperationInvoke, Record: record,
	}); err != nil {
		return ActionRecord{}, errors.Join(ErrRecordingUnavailable, err)
	}
	stored, err := s.dispatch.store.ApplyAction(ctx, DispatchMutation{
		Token: transitionToken(
			"admission", record.ID, record.IdempotencyKey, record.Version),
		ExpectedVersion: 0, TransitionKind: kind, Record: record,
	})
	if err != nil {
		return ActionRecord{}, err
	}
	if err := s.dispatch.publishTransition(
		context.WithoutCancel(ctx), actionEventKind(stored), stored,
	); err != nil {
		return ActionRecord{}, errors.Join(ErrActionCommitted, err)
	}
	return stored, nil
}

// admissionIDPrefix reserves the top of the durable action identity space for
// admissions. No caller may name an action inside it; see enqueue.
//
// This is what separates an admission from a dispatch action on the way in, and
// the separation is reachability rather than secrecy. A caller cannot supply a
// durable identity to the admission surface at all — Request derives one from
// the caller's own decision — so the only way to name an arbitrary action is
// through dispatch, and dispatch refuses every name in this span
// unconditionally, whether or not anything is stored there.
//
// It says nothing about what a record *is*. That is the durable marker's job,
// and the distinction cost three review rounds: ownership was once inferred
// from a record with no domain, identity scheme from an identity that was
// caller-chosen, and reachability from a span that predates the reservation.
// The span is a forward rule about creation, nothing more. That is also why it
// carries no readable tag: a tag inside the identifier is an invitation to read
// meaning back out of it, which is the mistake those three rounds were.
//
// Every byte is 0xff, and that is the whole correctness requirement. The
// listings filter admissions out by marker and keep no budget off them by
// position, which is only affordable if a scan meets every ordinary record
// before it meets an admission. A prefix delivers that if and only if it is
// maximal — every byte 0xff — because then any unreserved key differs from it
// at some byte that is necessarily smaller, and so sorts below the entire span.
// The span was briefly "\xffshoal.admission\x00", which is *not* maximal: 's'
// leaves 0x74..0xff above it, so an ordinary \xff\xff sorted after every
// admission and a large admission tail starved it. One byte of 0xff was enough
// to stop admissions sorting first; it was not enough to make them sort last.
//
// Four bytes, not one. Maximality fixes the ordering at any width, so width
// trades only against the rollout gate below: a pre-existing ordinary action
// inside the span blocks admission until an operator clears it, and the span
// was a legal dispatch name space before this release. Action identities are
// opaque bytes with no charset rule, so a client minting random ones puts
// 1-in-256 of them under a one-byte span — enough that admission would refuse
// to start on a sizable store, and live dispatch records cannot always be
// deleted to unblock it. Four bytes makes that 1-in-2^32, under one expected
// collision in a store of a billion random identities, while reserving exactly
// as much of the top of the key space as the ordering argument needs. Length
// here is a collision budget, not a secret: a longer span is not more private,
// only less likely to be already occupied.
//
// A constant, not a variable. It was once an exported []byte, which is mutable
// global state: another package could reassign it or write through the backing
// array, changing the derivation and the reservation together at runtime, and
// racing with any concurrent request while it did.
//
// One constant for the reservation and the derivation both. Two would have to
// agree about which keys dispatch refuses and which keys Request mints, and a
// derivation narrower than the reservation mints names dispatch is entitled to
// refuse.
const admissionIDPrefix = "\xff\xff\xff\xff"

// reservedAdmissionID reports whether an identity falls in the reserved span.
//
// Anchored at the start rather than matched anywhere: a substring test would
// reserve every identity that happens to contain the marker, refusing
// legitimate names a dispatch caller is entitled to use.
func reservedAdmissionID(id []byte) bool {
	return bytes.HasPrefix(id, []byte(admissionIDPrefix))
}

// reservedActionID reports whether a caller may not name a new action with
// id, at dispatch enqueue or an approval request (whose approved record is
// enqueued at the same ID). There are two reserved spaces:
//
//   - The admission span. Admission derives IDs in it.
//   - The explorer document ID shape (explorer.IsDocumentID). An action's
//     guard entity is {'D', action ID}, and so is a document's, at its
//     document ID. A document ID is a digest of its source URI, which anyone
//     can compute. So an action enqueued there first would occupy the
//     document's entity, and that URI's ingestion would conflict forever
//     (#669). Refusing the shape keeps the two ID spaces disjoint.
//
// The check runs only at these two entry points, and it runs before the
// store is read, as the admission check does. So a retried enqueue or
// request at such an ID is refused too. Validate does not check the document
// shape. An action or approval stored at such an ID before #669 still
// decodes, and an action can still be read, claimed, completed or canceled.
// An approval at such an ID can still be read and decided, but an approved
// one never becomes work. Materialization runs only on the requester's
// Request retry, and Request is now refused. Re-enqueueing such an action, or
// the Invoke replay that reports its outcome, is refused too; Status still
// answers. A replica built before #669 still accepts the shape during a
// rolling upgrade. All of this needs a record created at "doc_" and 32
// lowercase hex, which no client is expected to have chosen.
func reservedActionID(id []byte) bool {
	return reservedAdmissionID(id) || explorer.IsDocumentID(id)
}

// admissionActionID derives the durable record identity for a caller-supplied
// admission name.
//
// What this does and does not do is worth stating exactly, because an earlier
// version of this comment claimed more than the code delivered.
//
// It does keep two principals that choose the same name in two different
// records. That is its job, and an unkeyed digest does it: the derivation only
// has to be collision-free across principals, not unpredictable.
//
// It does *not* make another principal's identity unguessable, and it never
// could. The inputs are a workspace and a set of identities, which are knowable
// in most deployments, and the digest is unkeyed — so anyone who knows the
// tuple computes the result. The property the previous version claimed, that
// "neither can address the other's", did not hold: addressing a record does not
// require this surface. A caller could compute the victim's derived ID, submit
// it to dispatch enqueue as an ordinary action ID, and learn from conflict
// versus success whether it was occupied — and squat unheld ones, denying the
// victim its own admission by name.
//
// Keying the digest would have hidden the ID without making it unreachable, and
// an identifier's secrecy is not access control: anything that ever leaks one —
// a log line, an expired token, a record read through another surface — hands
// back the reachability. It would also need a durable secret with a rotation
// story, and rotation re-derives every live admission's identity, orphaning
// outstanding grants whose tokens no longer resolve. A reserved namespace costs
// none of that and gives the stronger property, so the secrecy claim is gone
// and the reserved namespace carries the weight.
//
// Every component of the principal participates, because every component is
// what authorizedCurrent compares when it decides a record belongs to a caller.
// An identity differing only in its delegation chain is a different principal
// there and has to be one here.
func admissionActionID(decision auth.Decision, supplied []byte) []byte {
	digest := sha256.New()
	writeDispatchTupleField(digest, []byte("shoal.fleet.admission-id.v1"))
	writeDispatchTupleField(digest, decision.AuthorizationDomain())
	writeDispatchTupleField(digest, []byte(decision.Subject()))
	writeDispatchTupleField(digest, []byte(decision.Actor()))
	writeDispatchTupleField(digest, []byte(decision.ClientID()))
	for _, identity := range decision.OnBehalfOf() {
		writeDispatchTupleField(digest, []byte(identity))
	}
	writeDispatchTupleField(digest, supplied)
	return append(
		append([]byte(nil), admissionIDPrefix...), digest.Sum(nil)...)
}

// disclosureDigest reduces a canonical declared reference set to the value the
// durable record carries, or nil when nothing was declared.
//
// Length-prefixed per element so no two distinct sets collide by
// concatenation: without the prefix, {"ab","c"} and {"a","bc"} would digest
// identically and a retry could swap one declaration for the other.
func disclosureDigest(disclosures []shoal.ID) []byte {
	if len(disclosures) == 0 {
		return nil
	}
	digest := sha256.New()
	writeDispatchTupleField(digest, []byte("shoal.fleet.admission-disclosures.v1"))
	for _, reference := range disclosures {
		writeDispatchTupleField(digest, []byte(reference))
	}
	return digest.Sum(nil)
}

func grantFor(record ActionRecord, obligations Obligations) AdmissionGrant {
	outcome := AdmissionAllowed
	if !obligations.empty() {
		outcome = AdmissionObligated
	}
	return AdmissionGrant{
		Outcome: outcome, Obligations: obligations,
		Token: AdmissionToken{
			ActionID: append([]byte(nil), record.ID...),
			TokenID:  append([]byte(nil), record.ClaimID...),
			Version:  record.Version, ExpiresAt: record.ClaimLeaseUntil,
		},
	}
}

// obligations computes what the caller must withhold from its payload.
//
// Both stages narrow and neither widens. Authorization runs first, over the
// same object-scoped check the authorized read path applies before a document
// enters a candidate set, and the restrictor then narrows what survived. A
// reference the restrictor returns that authorization already withheld stays
// withheld, so a misbehaving restrictor cannot admit anything.
//
// The first stage is honestly a request-level gate, not a per-reference one. An
// admission names one source and one policy, and only the object identity
// varies across references, so every reference in a request gets the same
// answer: either this caller may retrieve within this scope or it may not.
// Per-reference authorization is the registration rule attached to each
// document, which is exactly what the restrictor resolves. That is why a
// deployment without a restrictor gets weaker obligations than one with it, and
// why the wiring is a deployment invariant this service cannot check.
func (s *AdmissionService) obligations(
	ctx context.Context,
	decision auth.Decision,
	request AdmissionRequest,
	disclosures []shoal.ID,
	now time.Time,
) (Obligations, error) {
	if len(disclosures) == 0 {
		return Obligations{}, nil
	}
	order := make([]shoal.ID, 0, len(disclosures))
	permitted := make(map[shoal.ID]struct{}, len(disclosures))
	for _, reference := range disclosures {
		if err := decision.AuthorizeObject(auth.OperationRetrieve, auth.ResourceRequest{
			AuthorizationDomain: decision.AuthorizationDomain(),
			SourceID:            request.SourceID,
			PolicyID:            request.PolicyID,
			ObjectID:            reference,
		}, now); err != nil {
			continue
		}
		permitted[reference] = struct{}{}
		order = append(order, reference)
	}
	admitted := permitted
	if s.restrictor != nil {
		allowed, err := s.restrictor.RestrictDisclosure(ctx, order)
		if err != nil {
			return Obligations{}, err
		}
		// Rebuilt by intersection rather than by removing entries from what
		// authorization decided. A restrictor is a narrowing control and this
		// is what holds it to that: subtracting from a withheld set would let a
		// restrictor's answer re-admit a reference authorization had already
		// refused, and a restrictor is not an authorization decision.
		narrowed := make(map[shoal.ID]struct{}, len(allowed))
		for _, reference := range allowed {
			if _, ok := permitted[reference]; ok {
				narrowed[reference] = struct{}{}
			}
		}
		admitted = narrowed
	}
	// Emitted in the canonical order of the declared set rather than map order,
	// so the same request produces the same answer and a caller comparing two
	// responses is comparing decisions rather than iteration.
	var result Obligations
	for _, reference := range disclosures {
		if _, ok := admitted[reference]; !ok {
			result.Withhold = append(result.Withhold, reference)
		}
	}
	return result, nil
}

// Report closes the loop for one admission.
//
// A token is one-shot. The dispatch completion path treats a terminal action at
// the reported version under the same claim as the reporter's own lost response
// and replays it, which is right for a worker retrying a delivery and wrong for
// an admission: it would let a caller report one outcome, then report a
// different one against the same token and be told the second was recorded. So
// the terminal case is resolved here first, and only a byte-identical replay of
// what is already committed is accepted. Anything else is spent.
func (s *AdmissionService) Report(
	ctx context.Context, report AdmissionReport,
) (ActionRecord, error) {
	dispatch := s.dispatch
	ctx, cancel := dispatch.deadline(ctx, report.Context)
	defer cancel()
	decision, now, err := dispatch.begin(ctx, auth.OperationInvoke, report.Context)
	if err != nil {
		return ActionRecord{}, err
	}
	if err := report.Token.validate(); err != nil {
		return ActionRecord{}, err
	}
	if report.Failed && report.ErrorCode == "" {
		return ActionRecord{}, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"a failed admission report requires an error code")
	}
	// A report is either an outcome or a failure, and both halves of that have
	// to be enforced. Allowing an outcome to carry an error code would leave the
	// committed record's shape depending on which the completion path resolved
	// first. Allowing a failure to carry an outcome is worse: the completion
	// path discards it, so the durable record says nothing about it, and the
	// replay comparison below would then read any two failures with the same
	// error code as the same report — letting a caller replace the outcome it
	// reported with a different one and be told the second was recorded.
	if !report.Failed && report.ErrorCode != "" {
		return ActionRecord{}, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"a successful admission report carries no error code")
	}
	if report.Failed && len(report.Outcome) > 0 {
		return ActionRecord{}, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"a failed admission report carries no outcome")
	}
	// Effected is the quantity on a failure, so it is meaningless on a
	// success and refused there. The exclusivity above is what keeps the
	// replay comparison well defined, and this field does not weaken it:
	// unlike the outcome it was refused for, Effected is committed and
	// compared, so two failures that differ in how much escaped are
	// different reports (#427).
	if !report.Failed && !report.Effected.Zero() {
		return ActionRecord{}, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"a successful admission report carries no effected volume")
	}
	if err := report.Effected.validate(); err != nil {
		return ActionRecord{}, err
	}
	// Validated here rather than left to the completion path. That path is
	// written for an executor which has already performed the work, so a
	// malformed result has to be recorded as something: it commits
	// invalid_executor_error and returns an error. For a report that is exactly
	// wrong — it spends a live one-shot token on a failure the caller was never
	// told about, and leaves it unable to report the outcome it actually has.
	// Nothing about the underlying call is known from a malformed report, so
	// refusing it before anything is written is the only answer that keeps the
	// token usable.
	if err := validateActionErrorCode(report.ErrorCode); err != nil {
		return ActionRecord{}, err
	}
	// An admission that does not exist and an admission belonging to someone
	// else are the same answer, which authorizedCurrent now guarantees for
	// every path that reaches the store through it. This used to normalise the
	// absent case here; that was a fix in one caller for a property all of them
	// need, and it is gone because the guarantee moved to where the two shapes
	// actually meet.
	// An admission is reported by the principal that requested it — the proxy
	// holds one identity for both halves — so this keeps the principal
	// requirement rather than taking the executor routes #437 added. An
	// admission is also not reclaimable, so there is no second claimant for
	// those routes to serve.
	// Named as a completion phase rather than going through
	// authorizedCurrent, which hardcodes executorPhaseNone.
	//
	// A report *is* a report on a claim already taken — an admission is a
	// dispatch action born claimed — so it must resolve with the tolerations
	// that phase carries, or a lease that lapsed after the admission was
	// granted refuses the report and loses the record of an effect that
	// happened (#577). The phase is what the tolerations are keyed on, so a
	// path that does not declare its phase cannot receive them.
	//
	// executorPhaseComplete also activates #391's binding rule, which is
	// correct and inert here: that applies on the execute route and this
	// resolves under invoke.
	current, _, _, err := dispatch.authorizedCurrentBinding(
		ctx, decision, report.Token.ActionID, auth.OperationInvoke, true, now,
		executorPhaseComplete, 0)
	if err != nil {
		return ActionRecord{}, err
	}
	// A dispatch action is not an admission and cannot be closed here. It would
	// otherwise pass every check above — the principal owns it, the registry
	// resolves it, the claim is live — and be completed under this surface's
	// semantics instead of its own. Those differ where it matters: a reported
	// failure is a receipt here and the executor's error there, so a worker
	// completing through this endpoint would be told its failed work was
	// recorded successfully.
	//
	// Not-found, not unauthorized. The caller may hold the record; what it does
	// not hold is an admission, and saying which would distinguish a dispatch
	// action it owns from an admission identity that does not exist.
	if !current.isAdmission() {
		return ActionRecord{}, auth.ObjectNotFound()
	}
	// Resolved for the OutputSchema the report is validated against.
	// authorizedCurrent has already resolved and authorized this action and
	// discarded what it resolved; this is how the declared schema is obtained,
	// not a second authorization.
	// Lapsing, and reporting on a claim. This is a *report* on an admission
	// already granted: the caller was admitted while the descriptor was
	// live, performed the effect, and is now saying what happened. Refusing
	// it because the operator stopped renewing the lease in the meantime
	// does not un-send the egress — it loses the record of it, which is the
	// one thing a gateway's report exists to produce (#577).
	//
	// This is the third site, and the one a gateway actually hits: Report
	// resolves here rather than through authorizedCurrentBinding, so fixing
	// the two dispatch sites left the admission path still refusing. A
	// revoked descriptor is still refused — activeChainLapsing checks
	// RevokedAt regardless of the flag — which keeps "stopped renewing"
	// and "withdrew the agent" different answers.
	_, action, _, err := dispatch.registry.resolveActionBindingLapsing(
		ctx, decision, current.AgentID, current.AgentGeneration,
		current.Capability, current.Action, current.SourceID, current.PolicyID,
		current.ObjectID, auth.OperationInvoke, now,
		// Unpinned: this record already exists, so the question is
		// whether it is still the same agent, still live, and still
		// authorized for this scope and capability — none of which a
		// heartbeat changes. The requirement itself is checked by
		// approvalGate and attestationGate. See resolveActionBinding.
		false,
		true,
		true,
	)
	if err != nil {
		return ActionRecord{}, err
	}
	// The outcome is validated against the declared schema here for the same
	// reason the error code is, and before the terminal check for the same
	// reason again: the completion path would commit invalid_executor_output
	// and spend the token, and answering "spent" to a malformed report would
	// tell a caller its report was well formed and merely late. A malformed
	// report is malformed whether or not the token is still live.
	// Effected is only meaningful for an action the host declared may send
	// content outside Shoal. Refused rather than recorded on any other
	// action, so a volume cannot be attached to work that egresses nothing —
	// which would make the field a free-form channel on the durable record
	// rather than a measurement of a disclosure (#427).
	//
	// Checked against the declaration resolved above rather than against the
	// record, because the declaration is what the host asserted and the
	// record carries only whether an effect was possible.
	if !report.Effected.Zero() &&
		!action.Effects.contains(EffectEgressesContent) {
		return ActionRecord{}, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"an action that does not declare "+
				string(EffectEgressesContent)+
				" reports no effected volume")
	}
	output, err := reportedOutput(action, report)
	if err != nil {
		return ActionRecord{}, err
	}
	if current.State.terminal() {
		if !sameReportedOutcome(current, report, output) {
			return ActionRecord{}, ErrAdmissionSpent
		}
		if err := dispatch.publishTransition(
			context.WithoutCancel(ctx), actionEventKind(current), current,
		); err != nil {
			return ActionRecord{}, errors.Join(ErrActionCommitted, err)
		}
		return cloneActionRecord(current), nil
	}
	// The unexported completion: this is the admission surface's own path, and
	// it has already applied the validation the exported entry point refuses
	// admissions in order to protect.
	record, err := dispatch.completeClaim(ctx, CompletionRequest{
		ID: report.Token.ActionID, ExpectedVersion: report.Token.Version,
		ClaimID: report.Token.TokenID, Failed: report.Failed,
		Result: ExecutionResult{
			Output: report.Outcome, ErrorCode: report.ErrorCode,
			Effected: report.Effected,
		},
		Context: report.Context,
	}, false)
	if err == nil {
		return record, nil
	}
	if errors.Is(err, ErrClaimLost) {
		return ActionRecord{}, ErrAdmissionSpent
	}
	// What used to be here: a branch converting a reported failure back into a
	// receipt, because "reporting a failure is a successful report" and the
	// completion path answered it with an error. Without it the first response
	// to a reported failure was an error and the identical retry was a
	// receipt, so what a caller saw depended on whether its own report
	// committed — the exact confusion the one-shot token and the replay
	// comparison exist to remove.
	//
	// completeClaim answers that way itself now (#492), so the err == nil
	// return above takes this case and the branch became unreachable:
	// completeClaim returns a non-zero record only when it committed, and
	// then with no error. Keeping it would have implied a guarantee that is
	// no longer held here, which is worse than the duplication it saved —
	// this surface had the right answer before the service did, and the
	// service having it means this one should stop asserting it separately.
	return ActionRecord{}, err
}

// reportedOutput returns the canonical stored form of a report's outcome, so a
// replay can be compared against what was committed rather than against the
// bytes the caller happened to send this time.
func reportedOutput(
	action Action, report AdmissionReport,
) (json.RawMessage, error) {
	if report.Failed {
		return nil, nil
	}
	return validateAgainstSchema(
		action.OutputSchema, report.Outcome,
		"admission outcome", MaxActionOutputBytes)
}

// sameReportedOutcome reports whether a terminal record is this exact report,
// already committed.
//
// Every clause narrows. A record that is terminal for any other reason —
// cancelled, or completed under a different claim, or at a version this token
// never named — is a token that has been spent on something else, and the
// caller is told nothing about which.
func sameReportedOutcome(
	current ActionRecord, report AdmissionReport, output json.RawMessage,
) bool {
	if current.Version != report.Token.Version+1 ||
		!bytes.Equal(current.ClaimID, report.Token.TokenID) {
		return false
	}
	if report.Failed {
		// Effected is part of the comparison, which is the whole reason it
		// may exist on a failure at all. The exclusivity rule above refused
		// an outcome on a failure because the completion path discarded it,
		// so two failures with the same error code compared equal and a
		// caller could replace what it reported and be told the second was
		// recorded. A field that is committed and compared has none of that:
		// two failures reporting different volumes are different reports
		// (#427).
		return current.State == DispatchFailed &&
			current.ErrorCode == report.ErrorCode &&
			current.Effected == report.Effected &&
			len(current.Output) == 0
	}
	return current.State == DispatchSucceeded &&
		current.ErrorCode == "" && bytes.Equal(current.Output, output)
}

// Outstanding lists admissions this caller was granted and has not reported.
//
// Claimed is the whole filter. An admission that was granted and never reported
// is a claimed action, and it stays claimed whether its lease is live or long
// gone — the record never quietly becomes something else. Expired says the
// token can no longer be reported against, which is not a resolution: it is the
// point at which Shoal permitted an effect and will never learn what happened.
func (s *AdmissionService) Outstanding(
	ctx context.Context, request OutstandingAdmissionsRequest,
) (OutstandingAdmissionsPage, error) {
	dispatch := s.dispatch
	ctx, cancel := dispatch.deadline(ctx, request.Context)
	defer cancel()
	decision, now, err := dispatch.begin(ctx, auth.OperationInvoke, request.Context)
	if err != nil {
		return OutstandingAdmissionsPage{}, err
	}
	if request.Limit <= 0 || request.Limit > MaxDispatchListResults {
		return OutstandingAdmissionsPage{}, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"outstanding admission limit is outside its bound")
	}
	page, err := dispatch.store.ScanActions(ctx, request.After, request.Limit)
	if err != nil {
		return OutstandingAdmissionsPage{}, err
	}
	result := OutstandingAdmissionsPage{Next: append([]byte(nil), page.Next...)}
	for _, record := range page.Actions {
		if record.State != DispatchClaimed {
			continue
		}
		// A worker's ordinary claim is not an outstanding admission. Before the
		// record carried a marker the two were genuinely indistinguishable and
		// this listed both, which was defensible then and is not now: Report
		// refuses a dispatch action, so listing one here would name something
		// as outstanding that this surface will not let the caller close.
		if !record.isAdmission() {
			continue
		}
		if !sameActionPrincipal(decision, record) {
			continue
		}
		// Binding, not execution: the caller whose admission this is runs out of
		// process by construction, so demanding a runnable executor would hide
		// every admission from the only principal entitled to see it.
		if _, _, _, authorizeErr := dispatch.registry.resolveActionBinding(
			ctx, decision, record.AgentID, record.AgentGeneration,
			record.Capability, record.Action, record.SourceID, record.PolicyID,
			record.ObjectID, auth.OperationInvoke, now,
			// Unpinned, and here the pin was worse than a refusal: this loop
			// *skips* a record it cannot authorize, so a heartbeat silently
			// emptied the caller's own outstanding list rather than failing
			// visibly. See resolveActionBinding.
			false,
		); authorizeErr != nil {
			if shoal.IsErrorCode(authorizeErr, shoal.ErrorUnauthorized) ||
				shoal.IsErrorCode(authorizeErr, shoal.ErrorNotFound) {
				continue
			}
			return OutstandingAdmissionsPage{}, authorizeErr
		}
		result.Admissions = append(result.Admissions, OutstandingAdmission{
			ActionID: append([]byte(nil), record.ID...),
			TokenID:  append([]byte(nil), record.ClaimID...),
			Version:  record.Version, AdmittedAt: record.UpdatedAt,
			ExpiresAt: record.ClaimLeaseUntil,
			Expired:   !now.Before(record.ClaimLeaseUntil),
		})
	}
	return result, nil
}

// canonicalDisclosures sorts and deduplicates a declared reference set.
//
// Canonical here is what makes an obligation reproducible: the same references
// declared in a different order are the same declaration, and a caller
// comparing two answers must not see a difference that came from its own
// iteration order.
func canonicalDisclosures(values []shoal.ID) ([]shoal.ID, error) {
	if len(values) == 0 {
		return nil, nil
	}
	if len(values) > MaxAdmissionDisclosures {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "admission disclosures exceed their bound")
	}
	result := make([]shoal.ID, 0, len(values))
	for _, value := range values {
		if err := shoal.ValidateRequiredID("admission disclosure", value); err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool {
		return shoal.CompareID(result[i], result[j]) < 0
	})
	deduplicated := result[:0]
	for _, value := range result {
		if len(deduplicated) == 0 ||
			deduplicated[len(deduplicated)-1] != value {
			deduplicated = append(deduplicated, value)
		}
	}
	return deduplicated, nil
}
