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
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/executorref"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/evidencelabels"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	MaxActionIDBytes       = 256
	MaxActionPayloadBytes  = 1 << 20
	MaxActionOutputBytes   = 1 << 20
	MaxActionEvidence      = 256
	MaxActionErrorBytes    = 1024
	MaxActionClaimTTL      = 5 * time.Minute
	MaxActionDeadline      = 24 * time.Hour
	MaxDispatchListResults = 256
	// MaxActionClaimHistory bounds how many prior claim holders a record
	// retains, and MaxActionAmbiguityReports how many lost-fence reports it
	// accumulates.
	//
	// Both are capped because both grow on caller-triggered transitions and
	// encodeAction refuses a record over 3*MaxActionPayloadBytes — an
	// unbounded list would eventually make the action *unwritable* rather than
	// merely large, which bricks the record instead of degrading it. Eight is
	// chosen to cover a lease that lapses and is re-taken several times during
	// one long operation.
	//
	// "Far inside that ceiling" is what this said, and a measurement
	// disagreed: eight holders with *unbounded* delegation chains and eight
	// maximal reports reached 18% of the limit on their own, and a record also
	// carrying a maximal input, output and evidence set landed at 86%. That is
	// why MaxClaimHolderChainBytes below exists. Eight is a bound on how many,
	// and the bound on how large is separate.
	//
	// Those figures describe what motivated the bound, not what ships. With
	// the chain bound in place, eight holders carry at most 8 × 4096 = 32 KB
	// of delegation chain against the 512 KB that 64 maximal entries each
	// would have reached — a factor of sixteen (40 KB since the bound also
	// covers each holder's executor reference) — and the encoding ceiling is
	// 3 MB (3 × MaxActionPayloadBytes). So "far inside" is now true, and it is
	// true because of the second bound rather than in spite of its absence.
	//
	// The claim history drops its oldest entry on overflow: the most recent
	// claimants are the ones whose effects may be unreconciled. The report
	// list refuses instead of dropping, because a report is evidence an
	// operator is going to read and silently discarding the first one is worse
	// than refusing the ninth.
	MaxActionClaimHistory     = 8
	MaxActionAmbiguityReports = 8
	// MaxClaimHolderChainBytes bounds one retained holder's delegation chain
	// in total bytes, not only in entries.
	//
	// Measured, because the entry bound alone left the record brickable: the
	// unbounded chains were roughly eleven times everything else the history
	// and the reports contribute — 512 KB against 47 KB — so bounding how
	// many holders are retained without bounding how large each may be left
	// the dominant term unbounded. Those are the pre-bound numbers, which is
	// the point of recording them.
	//
	// It is also enforced where a claimant *enters* the record, in applyClaim,
	// not only on the retained holder. Bounding one and not the other was a
	// brick of its own: ClaimantOnBehalfOf on the live record is capped by
	// entry count alone, so a claimant whose chain fell between the two caps
	// claimed successfully and then the next claim produced a record
	// ActionRecord.Validate refuses — leaving the action unclaimable by
	// anyone, forever.
	//
	// It also covers the executor reference a holder claimed under (#391),
	// because a retained holder carries that too: the bound is on what one
	// holder costs the record, and the reference is part of that cost. It was
	// raised by executorref.MaxExecutorRefBytes when ClaimHolder gained the
	// field — the worst case for the one reference each holder carries — so
	// a holder at the old chain bound is still legal once it also carries a
	// maximum-length reference. Without the raise, a claimant at the old
	// bound would claim and then make its successor's record unwritable,
	// which is the brick described above. Over the whole history that is
	// MaxActionClaimHistory × MaxExecutorRefBytes (8 KB) more, against an
	// encoding ceiling of 3 MB.
	//
	// TestAMaximalHolderHistoryWithMaximalRefsStillReclaims pins it.
	MaxClaimHolderChainBytes = 4096 + executorref.MaxExecutorRefBytes
	// MaxAmbiguityTargetBytes bounds the worker's identifier for the third
	// party it was talking to, and MaxAmbiguityReferenceBytes the opaque
	// handle that party returned.
	//
	// Both are deliberately small. They describe an interaction with a system
	// Shoal does not control, so the bytes are target-controlled. A closed
	// shape with tight bounds gives a hostile or merely verbose target
	// nothing, which is the same reasoning docs/gateway-proxy-design.md
	// applies to Output.
	//
	// Where they actually reach: the durable record, fleetActionWire, and the
	// MCP tool result, which marshals ActionRecord whole. An earlier version
	// of this said "action.* events and the team overview" — neither is true.
	// fleetevents.Event carries no record content and this route publishes no
	// event at all, and teamoverview projects state counts and derived rows
	// with no ActionRecord in its response, which its own guard test pins.
	// The claim was inherited from the design doc's equivalent note about
	// Output, where it is also wrong.
	MaxAmbiguityTargetBytes    = 256
	MaxAmbiguityReferenceBytes = 256
)

var (
	ErrActionNotFound       = errors.New("fleet dispatch: action not found")
	ErrActionConflict       = errors.New("fleet dispatch: action conflict")
	ErrClaimLost            = errors.New("fleet dispatch: claim lost")
	ErrActionTerminal       = errors.New("fleet dispatch: action is terminal")
	ErrExecutionAmbiguous   = errors.New("fleet dispatch: execution may have occurred")
	ErrActionCommitted      = errors.New("fleet dispatch: action state committed")
	ErrRecordingUnavailable = errors.New("fleet dispatch: recording is unavailable")
)

type DispatchState string

const (
	DispatchQueued    DispatchState = "queued"
	DispatchClaimed   DispatchState = "claimed"
	DispatchSucceeded DispatchState = "succeeded"
	DispatchFailed    DispatchState = "failed"
	DispatchCanceled  DispatchState = "canceled"
)

func (s DispatchState) terminal() bool {
	return s == DispatchSucceeded || s == DispatchFailed || s == DispatchCanceled
}

// EvidenceRef is a complete, bounded evidence anchor returned by a trusted
// executor. It preserves the same immutable identity as interaction evidence.
type EvidenceRef struct {
	AnchorID   shoal.ID
	Kind       interaction.EvidenceKind
	Citation   document.Citation
	NodeIDs    []shoal.ID
	EdgeIDs    []shoal.ID
	Assertions []interaction.AssertionReference
	Visibility []string
}

// AmbiguityOutcome is what a worker observed before it lost the right to
// report through complete.
//
// Closed rather than free text, for the reason the design doc gives for
// Output: this value describes an interaction with a third party, so a string
// field is a channel for target-controlled bytes into the durable record and
// every surface that returns it. An operator reconciling under time pressure
// needs to sort records into groups, which an enumeration does and prose does
// not.
//
// The three cases are the three an operator acts on differently, and the
// distinction the design doc records as missing from the existing error
// vocabulary: whether the request left the host at all.
type AmbiguityOutcome string

const (
	// AmbiguityRequestNotSent means the worker had not yet transmitted
	// anything when it lost its claim. The effect did not happen. This is the
	// only one of the three that is good news, and it is worth recording
	// rather than staying silent because it removes a record from the set an
	// operator must reconcile by hand.
	AmbiguityRequestNotSent AmbiguityOutcome = "request_not_sent"
	// AmbiguityOutcomeUnknown means the request left the host and the worker
	// never learned what happened to it. This is the case the route exists
	// for.
	AmbiguityOutcomeUnknown AmbiguityOutcome = "outcome_unknown"
	// AmbiguityEffectObserved means the worker saw the effect succeed but
	// could no longer report it through complete. The work is done; the record
	// cannot say so, because this route deliberately does not reach a terminal
	// state.
	AmbiguityEffectObserved AmbiguityOutcome = "effect_observed"
)

func (o AmbiguityOutcome) validate() error {
	switch o {
	case AmbiguityRequestNotSent, AmbiguityOutcomeUnknown,
		AmbiguityEffectObserved:
		return nil
	default:
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "ambiguity outcome is invalid")
	}
}

// ClaimHolder is one principal that held this record's claim, retained so a
// worker that has since lost the claim can still be recognised.
//
// The record's own claimant fields carry only the *current* holder, and
// applyClaim overwrites them on every re-claim — correctly, since only the
// current holder may complete. That leaves the one caller a lost-fence report
// exists for unable to prove it ever held anything: its lease lapsed and
// another worker took the record, which is the only situation in which a
// worker needs this route at all.
type ClaimHolder struct {
	Subject    shoal.ID
	Actor      shoal.ID
	ClientID   shoal.ID
	OnBehalfOf []shoal.ID
	ClaimID    []byte
	// ClaimFence pins a holder to one attempt. A fence is already monotonic,
	// so two reports from two attempts are distinguishable in the record
	// without any reconciliation of their own.
	ClaimFence uint64
	HeldAt     time.Time
	// AttestationID is the attestation that holder's claim was granted under,
	// carried from ActionRecord.ClaimAttestationID when it is displaced.
	// Empty when the action did not require one. Bounded at
	// MaxClaimAttestationIDBytes.
	AttestationID shoal.ID
	// ExecutorRef is the executor reference that holder's claim was taken
	// under, carried from ActionRecord.ClaimExecutorRef when it is displaced.
	// A displaced holder reporting an ambiguity is judged against the
	// reference it claimed under, not the one the descriptor names now
	// (#391). Empty for a holder retained by a build without the field. It
	// counts toward MaxClaimHolderChainBytes.
	ExecutorRef string
}

// AmbiguityReport is a worker's account of an effect it may have performed
// without being able to report the outcome.
type AmbiguityReport struct {
	// ClaimFence identifies which attempt this report belongs to, and is what
	// the reporter must present and the record must have seen.
	ClaimFence uint64
	Outcome    AmbiguityOutcome
	// Target is the worker's identifier for the third party — a host, a queue
	// name, an endpoint. Bounded and optional.
	Target string
	// Reference is an opaque handle the target returned, if the worker got one
	// before losing the claim. It is the thing an operator takes to the other
	// system, and the reason this route is worth more than EffectPossible
	// alone.
	Reference string
	// Subject and Actor are the reporting principal, recorded from its
	// decision rather than from the request, so the report is attributed
	// rather than self-asserted.
	Subject    shoal.ID
	Actor      shoal.ID
	ReportedAt time.Time
}

func (h ClaimHolder) validate() error {
	if err := shoal.ValidateRequiredID("claim holder", h.Subject); err != nil {
		return err
	}
	if err := shoal.ValidateRequiredID(
		"claim holder actor", h.Actor); err != nil {
		return err
	}
	if err := shoal.ValidateOptionalID(
		"claim holder client", h.ClientID); err != nil {
		return err
	}
	if len(h.OnBehalfOf) > auth.MaxOnBehalfOfEntries {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"claim holder delegation chain exceeds its bound")
	}
	// Bounded in aggregate bytes as well as in entries, which the entry count
	// alone does not do. A measurement of the worst case found the delegation
	// chains were 89% of the record's growth — eight holders with *unbounded*
	// chains reached 18% of the encoding ceiling on their own, and combined
	// with a maximal input, output and evidence set they pushed a record past
	// the limit encodeAction enforces. That is the brick the cap exists to
	// prevent, so bounding the count and not the size left the hole open.
	//
	// applyClaim applies the same bound to the incoming claimant, which is
	// what makes this one safe to enforce here: bounding only the retained
	// copy meant a legal claim could produce an illegal record, and then the
	// refusal landed on whoever claimed next rather than on whoever claimed
	// too widely.
	if h.ExecutorRef != "" {
		if err := executorref.ValidExecutorRef(h.ExecutorRef); err != nil {
			return shoal.NewError(shoal.ErrorInvalidArgument,
				"claim holder executor reference: "+err.Error())
		}
	}
	chainBytes := len(h.ExecutorRef)
	for _, identity := range h.OnBehalfOf {
		if err := shoal.ValidateRequiredID(
			"claim holder delegation identity", identity); err != nil {
			return err
		}
		chainBytes += len(identity)
	}
	if chainBytes > MaxClaimHolderChainBytes {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"claim holder delegation chain exceeds its byte bound")
	}
	if err := validateOpaque("claim holder claim ID", h.ClaimID, false); err != nil {
		return err
	}
	// A fence of zero means no claim was ever taken, so a holder carrying one
	// is a record that cannot be true.
	if h.ClaimFence == 0 {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "claim holder fence is required")
	}
	if h.HeldAt.IsZero() || h.HeldAt.Location() != time.UTC {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "claim holder time is not UTC")
	}
	if err := validateClaimAttestationID(
		"claim holder attestation ID", h.AttestationID); err != nil {
		return err
	}
	return nil
}

// validateClaimAttestationID checks an optional attestation ID against its
// byte bound. The bound is also enforced where the ID enters the fleet
// (DispatchService.claimAttestation); this is the backstop for a stored value.
func validateClaimAttestationID(name string, id shoal.ID) error {
	if id == "" {
		return nil
	}
	if len(id) > MaxClaimAttestationIDBytes {
		return shoal.NewError(shoal.ErrorInvalidArgument, name+" exceeds its bound")
	}
	return shoal.ValidateRequiredID(name, id)
}

func (r AmbiguityReport) validate() error {
	if r.ClaimFence == 0 {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "ambiguity report fence is required")
	}
	if err := r.Outcome.validate(); err != nil {
		return err
	}
	// Bounded, and refused rather than truncated: a truncated target or
	// reference is worse than none, because an operator would take it to the
	// other system and get nothing.
	if len(r.Target) > MaxAmbiguityTargetBytes {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "ambiguity target exceeds its bound")
	}
	if len(r.Reference) > MaxAmbiguityReferenceBytes {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "ambiguity reference exceeds its bound")
	}
	// Printable and single-line. Both values are target-controlled and both
	// reach the durable record, fleetActionWire, and the MCP tool result,
	// which marshals ActionRecord whole — where a control character is at best
	// unreadable and at worst a terminal escape in whatever renders it.
	//
	// Not the team overview and not the event stream. That claim was corrected
	// on MaxAmbiguityTargetBytes above and this third copy of it was left
	// standing, which is the same mistake twice: a correction that fixes the
	// sentence it was written beside and not the ones that repeat it.
	if err := validateAmbiguityText("ambiguity target", r.Target); err != nil {
		return err
	}
	if err := validateAmbiguityText(
		"ambiguity reference", r.Reference); err != nil {
		return err
	}
	if err := shoal.ValidateRequiredID(
		"ambiguity reporter", r.Subject); err != nil {
		return err
	}
	if err := shoal.ValidateRequiredID(
		"ambiguity reporter actor", r.Actor); err != nil {
		return err
	}
	if r.ReportedAt.IsZero() || r.ReportedAt.Location() != time.UTC {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "ambiguity report time is not UTC")
	}
	return nil
}

// validateAmbiguityText refuses anything that is not plainly printable.
//
// unicode.IsControl alone was not enough, and the comment that accompanied it
// claimed "printable and single-line" while checking neither. IsControl is
// false for U+202E RIGHT-TO-LEFT OVERRIDE, the U+2066..U+2069 isolates,
// U+200E/U+200F, the zero-width characters and U+FEFF — all of which were
// accepted into the durable record. U+202E is precisely the case the bound's
// own rationale names: a value that is at worst a terminal escape in whatever
// renders it.
//
// So the test is IsPrint, which admits letters, marks, numbers, punctuation,
// symbols and the ASCII space and excludes every control, format, surrogate
// and unassigned codepoint. Refused rather than sanitised, because a silently
// rewritten target or reference is worse than none: an operator would take it
// to the other system and get nothing.
func validateAmbiguityText(name, value string) error {
	if !utf8.ValidString(value) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, name+" is not valid UTF-8")
	}
	for _, codepoint := range value {
		if !unicode.IsPrint(codepoint) {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				name+" contains a non-printable character")
		}
	}
	return nil
}

// ActionRecord is the durable source of truth for one dispatch: what was
// asked for, who asked for it, who holds it now, what has been observed about
// it, and the provenance of every transition it has been through.
//
// It is what an operator reconciles from when an effect may have happened
// outside Shoal, which is why so much of it exists to be read rather than
// acted on.
type ActionRecord struct {
	ID              []byte
	IdempotencyKey  []byte
	Version         uint64
	State           DispatchState
	AgentID         shoal.ID
	AgentGeneration int64
	Capability      string
	Action          string
	SourceID        []byte
	PolicyID        []byte
	ObjectID        shoal.ID
	Input           json.RawMessage
	Output          json.RawMessage
	ErrorCode       string
	// Effected is how much of an irreversible egress happened before a
	// failure, for the boundary where a failure follows a partial effect
	// (#427). Zero on every other record, including every success: a
	// completed egress's volume is not what this answers.
	//
	// An upper bound on what may have left, never a receipt — see
	// EffectedVolume. A reader states "at most N bytes may have reached the
	// caller", not "N bytes were received".
	//
	// Written exactly once, by the terminal transition, and immutable after —
	// the store enforces that (#461). An operator reconciling a failed
	// external call reads it to tell "nothing left" from "two megabytes
	// left", which arrived identically before.
	Effected EffectedVolume
	// ErrorCodeOrigin says whether the service or the executor decided
	// ErrorCode. Empty on a record written before the field existed.
	ErrorCodeOrigin                ErrorCodeOrigin
	Subject                        shoal.ID
	Actor                          shoal.ID
	ClientID                       shoal.ID
	OnBehalfOf                     []shoal.ID
	AuthorizationFingerprint       auth.Fingerprint
	PolicyGeneration               int64
	AuthorizationExpiresAt         time.Time
	ExecutionFingerprint           auth.Fingerprint
	ExecutionPolicyGeneration      int64
	ExecutionExpiresAt             time.Time
	AuthorizedOperations           []auth.Operation
	RequestID                      shoal.ID
	CorrelationID                  shoal.ID
	TransitionRequestID            shoal.ID
	TransitionCorrelationID        shoal.ID
	CancelAuthorizationFingerprint auth.Fingerprint
	CancelAuthorizationExpiresAt   time.Time
	Reason                         interaction.Reason
	Deadline                       time.Time
	CreatedAt                      time.Time
	UpdatedAt                      time.Time
	ClaimID                        []byte
	// The claimant's own principal chain, recorded when the claim is taken
	// and compared when it is reported on.
	//
	// ClaimID alone cannot do this job. It is caller-supplied with no entropy
	// requirement — validateOpaque accepts one to MaxActionIDBytes bytes — and
	// it is published: Status returns it to every co-principal, and Pull
	// returns a claimed record whose lease has lapsed, ClaimID included, to
	// every principal authorized to execute the descriptor. So it identifies
	// *a* claim, not *who* holds it.
	//
	// That distinction did not matter while claiming was gated on
	// sameActionPrincipal, because the claimant was by construction the
	// enqueuer and the enqueuer's chain was already on the record. #437 made
	// the claimant a different principal, at which point the record stopped
	// carrying any statement of who holds the claim.
	//
	// Compared rather than the claim-time ExecutionFingerprint on purpose.
	// That fingerprint covers policyGeneration, the operation set, the service
	// role and the selected ontology as well as identity, so a routine policy
	// reload would change it for every worker at once and strand every
	// in-flight claim — each one mid-effect, with no way to report. Identity
	// is the question being asked, so identity is what is stored.
	// TransitionOperation is the operation that authorized the transition this
	// record is currently in, as distinct from the operations its enqueuer
	// held.
	//
	// AuthorizedOperations cannot answer this. It accumulates across
	// transitions and, worse, it accumulates a *claim* rather than a fact:
	// applyClaim merges decisionOperations(decision, OperationInvoke), and
	// decisionOperations does not consult the decision at all — it returns the
	// operation it was handed. So before #437, when every claim was authorized
	// under invoke, the field happened to be true; after it, a record claimed
	// under execute still asserts invoke.
	//
	// That mattered beyond the audit trail. The event publisher selects an
	// operation per event kind and then authorizes the publishing decision
	// against it, so a claim authorized under execute was published under
	// invoke — which the claimant does not hold — and the publication failed.
	// A failed publication is reported as ErrActionCommitted, so the write
	// landed and the worker was told to reconcile. Recording the real
	// operation is what lets the publisher ask the right question.
	TransitionOperation auth.Operation
	// ClaimHistory retains prior claim holders, oldest first, so a worker
	// whose claim was taken over can still be recognised by this record. See
	// ClaimHolder.
	ClaimHistory []ClaimHolder
	// AmbiguityReports are lost-fence reports, in the order received. A second
	// report under the same fence is appended rather than replacing the first:
	// two reports from one attempt mean the worker retried, and that is
	// information an operator wants rather than noise to collapse.
	AmbiguityReports   []AmbiguityReport
	ClaimantSubject    shoal.ID
	ClaimantActor      shoal.ID
	ClaimantClientID   shoal.ID
	ClaimantOnBehalfOf []shoal.ID
	ClaimFence         uint64
	ClaimLease         time.Duration
	ClaimLeaseUntil    time.Time
	// ClaimAttestationID is the verified executor attestation the current
	// claim (or its latest extension) was granted under, when the action
	// requires attestation (#446). Empty otherwise.
	//
	// It is claim state, not record identity: a re-claim moves it, and an
	// extension under a newer attestation replaces it. It belongs on #461's
	// mutable list with the claimant fields (ClaimantSubject and friends),
	// not with the fields the store holds immutable. It is a record, not a
	// grant — nothing reads it to authorize anything.
	//
	// Bounded at MaxClaimAttestationIDBytes, at the boundary where it enters
	// (DispatchService.claimAttestation) and again in Validate. A record
	// written before the field existed decodes with it empty; gob ignores it
	// when an earlier build reads a record that carries it.
	ClaimAttestationID shoal.ID
	// ClaimExecutorRef is the descriptor's executor reference at the moment
	// the current claim was taken (#391). An execute-route completion or
	// ambiguity report is judged against it rather than against the
	// descriptor as it is now, so rebinding a descriptor never stops the
	// worker holding a claim from reporting an effect that happened. An
	// extension needs both, so a rebound claim runs only to its lease end.
	//
	// Claim state, like ClaimAttestationID: applyClaim writes it and a
	// re-claim moves it, so the store holds it neither immutable nor
	// monotonic. Empty for a claim taken by a build without the field, which
	// completion and ambiguity accept as legacy and extension refuses.
	ClaimExecutorRef     string
	CancelKey            []byte
	ExecutorKey          []byte
	EvidenceSnapshotID   shoal.ID
	EvidenceSnapshotAsOf time.Time
	Evidence             []EvidenceRef
	EffectPossible       bool
	// AdmittedEffects is the effect set a pre-call admission declared it was
	// about to perform. Empty on an action that was dispatched rather than
	// admitted.
	//
	// It is part of the record's enqueue identity, not decoration. Without it
	// the durable record says only that some admission was granted under this
	// token, and a retry carrying a different declaration is indistinguishable
	// from the original — so a caller could be admitted for one thing and hold
	// a token that reads as permission for another.
	AdmittedEffects Effects
	// AdmittedDisclosures is a digest over the canonical corpus references an
	// admission declared its payload would carry, or nil when it declared none.
	//
	// A digest rather than the references themselves. The references are corpus
	// identities the caller supplied, and copying them into a dispatch record
	// would put a caller's claimed reading list somewhere the team overview
	// reads. What the record has to do is refuse a retry that changes them,
	// which needs only equality.
	AdmittedDisclosures []byte
	// AdmittedObligation is the obligation returned with the grant, as a bitmap
	// over the canonical order of the declared references. Bit i set means the
	// reference at index i must be withheld.
	//
	// The obligation is the decision, and a decision that is recomputed is not
	// the decision that was made. The co-occurrence budget it comes from is
	// windowed and moves as an identity reads, so recomputing on a retry can
	// return a weaker obligation for a token that is already live — a caller
	// could replay its way out of a restriction — and a restrictor that is
	// briefly unreachable would make an already-granted admission impossible to
	// recover at all.
	//
	// Indices rather than identities, for the reason AdmittedDisclosures is a
	// digest: positions disclose nothing without the list they index, and the
	// caller supplies that list again on the retry, where the digest proves it
	// is the same one.
	AdmittedObligation []byte
	// AdmittedIdentityScheme records which identity scheme produced this
	// admission's durable key. Zero means the superseded one, where the key was
	// the caller's own name; AdmittedIdentitySchemeDerived means the key is
	// derived from the principal.
	//
	// It exists because the identity cannot answer this question. Under the
	// superseded scheme the key was caller-supplied opaque bytes, so a legacy
	// record's key may lie anywhere — including inside the reserved span, and
	// including the exact prefix-plus-digest shape a derived key has. Reading
	// the scheme off the key therefore certifies an adversarially-named legacy
	// record as new, a retry derives a different key, and a second live grant is
	// issued for work already permitted.
	//
	// Zero is the right value for a legacy record and gob gives it for free: a
	// record written before this field existed decodes with it absent, which is
	// exactly the claim "produced by the superseded scheme".
	AdmittedIdentityScheme uint32
	// The approval this record was materialized under, when its action
	// requires one (#451). All of these are zero on a record that was not
	// approved, and all of them are set on one that was: Validate refuses a
	// partial set.
	//
	// They are a record, not a grant. Nothing reads them to authorize a claim
	// or an execution — the work still runs only through this record's own
	// principal and the claim fence — so an approval written here cannot widen
	// what anyone may do. What they make durable is which exact request was
	// approved (the digest the approver reviewed, which covers every field
	// equivalentEnqueue compares), under which policy generation, by whom and
	// when.
	ApprovalRequestDigest    []byte
	ApprovalPolicyGeneration int64
	ApproverSubject          shoal.ID
	ApproverActor            shoal.ID
	ApproverClientID         shoal.ID
	ApprovedAt               time.Time
}

// validateApprovalProvenance checks that an approval on a record is complete
// or entirely absent.
func validateApprovalProvenance(record ActionRecord) error {
	if len(record.ApprovalRequestDigest) == 0 {
		if record.ApprovalPolicyGeneration != 0 ||
			record.ApproverSubject != "" || record.ApproverActor != "" ||
			record.ApproverClientID != "" || !record.ApprovedAt.IsZero() {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"action approval provenance is incomplete")
		}
		return nil
	}
	if len(record.ApprovalRequestDigest) != sha256.Size ||
		record.ApprovalPolicyGeneration <= 0 ||
		record.ApprovalPolicyGeneration != record.PolicyGeneration ||
		record.ApprovedAt.IsZero() ||
		record.ApprovedAt.Location() != time.UTC {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"action approval provenance is incomplete")
	}
	if err := shoal.ValidateRequiredID(
		"action approver", record.ApproverSubject); err != nil {
		return err
	}
	if err := shoal.ValidateRequiredID(
		"action approver actor", record.ApproverActor); err != nil {
		return err
	}
	if err := shoal.ValidateOptionalID(
		"action approver client", record.ApproverClientID); err != nil {
		return err
	}
	// An approval is a held request becoming work, never an admission: the
	// admission seam denies an approval-required action rather than holding
	// it, so a record carrying both markers was assembled by something that
	// skipped one of those paths.
	if record.isAdmission() {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"an admission cannot carry an approval")
	}
	return nil
}

// AdmittedIdentitySchemeDerived marks an admission whose durable key is derived
// from the principal rather than supplied by the caller.
const AdmittedIdentitySchemeDerived uint32 = 1

// MaxAdmittedObligationBytes bounds the obligation bitmap: one bit per
// reference the declaration may carry.
const MaxAdmittedObligationBytes = (MaxActionEvidence + 7) / 8

// MaxAdmittedEffects bounds the declared set a durable record may carry. The
// taxonomy has three classes; the bound is larger so a record written by a
// build that knows more of them still decodes here and is refused at
// resolution, which is where an unrecognised class is supposed to be caught.
const MaxAdmittedEffects = 16

// validateAdmittedDeclaration checks the shape of what an admission declared.
//
// Membership is deliberately not checked. A durable decoder reads whatever
// strings are stored, and Effects.exceeds already fails closed on a class it
// does not recognise — refusing to decode the record instead would turn a
// forward-compatible record into an undecodable one and lose the audit trail
// for exactly the admissions most worth reading.
//
// Canonical order is checked, because it is what makes two declarations
// comparable: the same classes in a different order are the same declaration
// and must not produce a record a retry cannot match.
func validateAdmittedDeclaration(record ActionRecord) error {
	if len(record.AdmittedEffects) > MaxAdmittedEffects {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "admitted effects exceed their bound")
	}
	for i := 1; i < len(record.AdmittedEffects); i++ {
		if record.AdmittedEffects[i-1] >= record.AdmittedEffects[i] {
			return shoal.NewError(
				shoal.ErrorInvalidArgument, "admitted effects are not canonical")
		}
	}
	// A scheme marker without an admission marker is incoherent: it claims how
	// an admission's key was produced for a record that is not an admission.
	if record.AdmittedIdentityScheme != 0 && len(record.AdmittedEffects) == 0 {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"admitted identity scheme requires an admitted effect")
	}
	if len(record.AdmittedObligation) > MaxAdmittedObligationBytes {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "admitted obligation exceeds its bound")
	}
	// An obligation names positions in a declared reference list. Without the
	// list there is nothing for it to index, so a record carrying one and no
	// declaration has been assembled by something that skipped the grant path.
	if len(record.AdmittedObligation) > 0 &&
		len(record.AdmittedDisclosures) == 0 {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"admitted obligation requires declared references")
	}
	if len(record.AdmittedDisclosures) == 0 {
		return nil
	}
	if len(record.AdmittedDisclosures) != sha256.Size {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"admitted disclosure digest is not a digest")
	}
	// Declared references without a declared effect is not a shape any
	// admission can produce: Request refuses an empty effect set before it
	// reaches a record. A record carrying one has been assembled by something
	// that skipped that check.
	if len(record.AdmittedEffects) == 0 {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"admitted disclosures require an admitted effect")
	}
	return nil
}

// ActionEventProvenance identifies the authorization and request that
// committed the record's current transition.
type ActionEventProvenance struct {
	RequestID                shoal.ID
	CorrelationID            shoal.ID
	AuthorizationFingerprint auth.Fingerprint
	AuthorizationExpiresAt   time.Time
}

// EventProvenance returns durable transition provenance, falling back to the
// enqueue provenance for records written before transition provenance existed.
func (r ActionRecord) EventProvenance() ActionEventProvenance {
	result := ActionEventProvenance{
		RequestID: r.RequestID, CorrelationID: r.CorrelationID,
		AuthorizationFingerprint: r.AuthorizationFingerprint,
		AuthorizationExpiresAt:   r.AuthorizationExpiresAt,
	}
	if r.TransitionRequestID != "" {
		result.RequestID = r.TransitionRequestID
		result.CorrelationID = r.TransitionCorrelationID
	}
	switch r.State {
	case DispatchCanceled:
		if !r.CancelAuthorizationExpiresAt.IsZero() {
			result.AuthorizationFingerprint = r.CancelAuthorizationFingerprint
			result.AuthorizationExpiresAt = r.CancelAuthorizationExpiresAt
		}
	case DispatchClaimed, DispatchSucceeded, DispatchFailed:
		if !r.ExecutionExpiresAt.IsZero() {
			result.AuthorizationFingerprint = r.ExecutionFingerprint
			result.AuthorizationExpiresAt = r.ExecutionExpiresAt
		}
	}
	return result
}

type EnqueueRequest struct {
	ID              []byte
	IdempotencyKey  []byte
	AgentID         shoal.ID
	AgentGeneration int64
	Capability      string
	Action          string
	SourceID        []byte
	PolicyID        []byte
	ObjectID        shoal.ID
	Input           json.RawMessage
	Context         RequestContext
}

type ClaimRequest struct {
	ID              []byte
	ExpectedVersion uint64
	// ClaimID is both the durable claimant identity and this mutation's
	// caller-supplied idempotency key.
	ClaimID []byte
	Lease   time.Duration
	Context RequestContext
}

type CancelRequest struct {
	ID              []byte
	ExpectedVersion uint64
	// MutationKey must be stable across retries of the same cancellation.
	MutationKey []byte
	Context     RequestContext
}

// CompletionRequest is a remote worker reporting the outcome of work it
// performed out of process.
//
// It is bound to a claim, not to an action. ClaimID and ExpectedVersion must
// name the claim the reporter actually holds: a worker whose lease expired
// while it was working must be refused rather than allowed to overwrite
// whatever happened next, because by then the action may have been reclaimed
// and run again by someone else.
//
// Result carries exactly what an in-process ActionExecutor returns, and is
// validated identically. A remote worker cannot record output or evidence an
// in-process one could not.
type CompletionRequest struct {
	ID              []byte
	ExpectedVersion uint64
	// ClaimID must equal the claim currently held on the action. It is necessary and not
	// sufficient: it is caller-chosen, nothing requires it to be unique
	// across claim generations, and the design doc records that as a
	// worker-side obligation rather than something the service enforces.
	ClaimID []byte
	// ClaimFence binds this completion to the claim *generation* the caller
	// was handed, which is the thing ClaimID cannot identify.
	//
	// Supply it. Without it this route falls back to comparing the record
	// version exactly, which was never a generation check — it only behaved
	// like one because nothing else could advance the version while leaving
	// the claim intact. #438's ambiguity route can, so a version-only
	// completion is both strandable by someone else's report and, if the
	// version comparison is loosened to fix that, acceptable from a stale
	// claim generation. The fence has neither problem: applyClaim increments
	// it on every claim, so it is exactly "which claim", and Claim returns it
	// to the worker that must present it.
	//
	// Zero means not supplied, and keeps the old exact-version behaviour for
	// a caller that predates this field.
	ClaimFence uint64
	Result     ExecutionResult
	// Failed reports that the work did not succeed. Result.ErrorCode carries
	// the reason. The two are separate because a worker that fails with no
	// error code is a protocol error, not a success.
	Failed  bool
	Context RequestContext
}

// ExtendRequest renews a live claim's lease without changing the claim.
type ExtendRequest struct {
	ID []byte
	// ExpectedVersion pins the record version. It is required only when
	// ClaimFence is zero, and is ignored when a fence is presented.
	//
	// Version was never the invariant on this route, for the same reason
	// AmbiguityRequest.ExpectedVersion records: what must not change under a
	// renewal is the *claim*, and the claim is identified by its fence. A
	// version comparison also fails on a write that leaves the claim intact —
	// a lapsed former holder legitimately reporting an ambiguity moves the
	// version without touching the fence, and the current holder cannot learn
	// the new version by any route it is authorized for. Its next extend then
	// conflicted and the claim ran out at its current lease end while the
	// holder still held the fence and could still complete, stranding a
	// long-running effect mid-flight (#391).
	ExpectedVersion uint64
	// ClaimFence optionally binds the renewal to one claim attempt, the way
	// CompleteClaim binds on it. When non-zero it must equal the fence of the
	// claim the record carries now, and the version is not compared.
	//
	// The fence pins what the version was standing in for, and more tightly:
	// it is server-assigned and monotonic where ClaimID is supplied by the
	// caller. Nothing else is lost by skipping the version, because every
	// other precondition this route has is checked explicitly against the
	// freshly read record — terminal state, lease liveness, the action
	// deadline, the approval and attestation gates — and the store's own
	// compare-and-set still pins the version it read.
	//
	// A mismatch is ErrClaimLost rather than ErrActionConflict: with a fence
	// presented, a mismatch means this claim is genuinely gone, so the
	// caller's reading of it is true rather than a guess.
	ClaimFence uint64
	// ClaimID must equal the claim currently held. It is necessary and not
	// sufficient: the caller must also be the claim's holder, because ClaimID
	// is published to co-principals on Status and to every execute-holder on
	// Pull once a lease lapses, so it identifies a claim and not who holds it.
	ClaimID []byte
	// Lease is the new silence budget from now, bounded per call by
	// MaxActionClaimTTL and in total by the action's Deadline. Renewal splits
	// the two things one number used to conflate: how long a worker may be
	// silent before it is assumed gone, and how long the whole operation may
	// take.
	Lease   time.Duration
	Context RequestContext
}

// AmbiguityRequest is a worker recording an effect it may have performed
// without being able to report the outcome through CompleteClaim.
type AmbiguityRequest struct {
	ID []byte
	// ExpectedVersion optionally pins the record version. Zero means "append
	// at whatever the version is now", which is the usual case and the only
	// one the intended caller can express.
	//
	// A lapsed claimant cannot learn the current version by any route it is
	// authorized for. Status requires OperationDispatch and the record's own
	// principal; Pull withholds live-claimed records, so a reclaimed action is
	// absent from its page; and ErrActionConflict carries no version. So
	// requiring a version made this route unusable by the only caller it
	// exists for — verified by execution, and the PR's own test passed only
	// because it used the version the *reclaiming* party had been handed.
	//
	// Dropping the requirement is safe because version was never the
	// invariant here. The report does not transition the action, and what must
	// not change under it is the claim, which is asserted separately through
	// the store's ExpectedFence. A caller that does know the version may still
	// pin it, and two concurrent reports are serialised by the store's
	// compare-and-set either way.
	ExpectedVersion uint64
	// ClaimFence is the attempt this report belongs to. The reporter must have
	// held the claim at this fence — either it still holds it, or the record
	// retains it in ClaimHistory.
	//
	// A fence rather than a claim ID, because the record keeps only the
	// current claim ID while fences are monotonic and retained: the one caller
	// this route exists for has had its claim taken over, so the ID it held is
	// gone from the record while its fence is not.
	ClaimFence uint64
	Outcome    AmbiguityOutcome
	Target     string
	Reference  string
	Context    RequestContext
}

type StatusRequest struct {
	ID      []byte
	Context RequestContext
}

type PullActionsRequest struct {
	After   []byte
	Limit   int
	Context RequestContext
}

type ActionPage struct {
	Actions []ActionRecord
	Next    []byte
}

// TeamActionListRequest asks for a bounded page of action state visible to a
// team-overview reader. The object and agent filters are narrowing only.
type TeamActionListRequest struct {
	After     []byte
	Limit     int
	SourceIDs [][]byte
	PolicyIDs [][]byte
	ObjectIDs []shoal.ID
	AgentIDs  []shoal.ID
	Context   RequestContext
}

type InvokeRequest struct {
	Enqueue EnqueueRequest
	ClaimID []byte
	Lease   time.Duration
}

type Invocation struct {
	ActionID        []byte
	IdempotencyKey  []byte
	ClaimFence      uint64
	AgentID         shoal.ID
	AgentGeneration int64
	Capability      string
	Action          string
	SourceID        []byte
	PolicyID        []byte
	ObjectID        shoal.ID
	Input           json.RawMessage
	Subject         shoal.ID
	Actor           shoal.ID
	ClientID        shoal.ID
	OnBehalfOf      []shoal.ID
	RequestID       shoal.ID
	CorrelationID   shoal.ID
	Deadline        time.Time
}

type ExecutionResult struct {
	Output               json.RawMessage
	ErrorCode            string
	EvidenceSnapshotID   shoal.ID
	EvidenceSnapshotAsOf time.Time
	Evidence             []EvidenceRef
	// Effected is how much of an irreversible egress happened before a
	// failure (#427). Carried here rather than only on the admission report
	// because both completion paths go through applyExecutionResult, and a
	// field validated on one of them is a hole on the other — which is the
	// shape of several defects this package has already had.
	Effected EffectedVolume
}

// ActionTransition is the immutable, durable event outbox entry created in the
// same transaction as an action-state mutation.
type ActionTransition struct {
	ID     []byte
	Kind   string
	Record ActionRecord
}

// NewActionTransition snapshots the exact event-producing state of one
// committed dispatch transition.
func NewActionTransition(
	id []byte, kind string, record ActionRecord,
) (ActionTransition, error) {
	switch kind {
	case "action.enqueued":
	case "action.claimed":
	case "action.completed":
	case "action.failed":
	case "action.canceled":
	default:
		return ActionTransition{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "fleet action transition kind is invalid")
	}
	if actionEventKind(record) != kind {
		return ActionTransition{}, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"fleet action transition kind does not match action state")
	}
	result := ActionTransition{
		ID: append([]byte(nil), id...), Kind: kind, Record: cloneActionRecord(record),
	}
	if err := result.Validate(); err != nil {
		return ActionTransition{}, err
	}
	return result, nil
}

func (t ActionTransition) Validate() error {
	if err := validateOpaque("transition ID", t.ID, false); err != nil {
		return err
	}
	if err := t.Record.Validate(); err != nil {
		return err
	}
	if actionEventKind(t.Record) != t.Kind {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"fleet action transition kind does not match action state")
	}
	return nil
}

type ActionExecutor interface {
	Execute(context.Context, Invocation) (ExecutionResult, error)
}

type DispatchMutation struct {
	Token           []byte
	ExpectedVersion uint64
	ExpectedFence   uint64
	TransitionKind  string
	Record          ActionRecord
}

type DispatchStore interface {
	GetAction(context.Context, []byte) (ActionRecord, error)
	ApplyAction(context.Context, DispatchMutation) (ActionRecord, error)
	ScanActions(context.Context, []byte, int) (ActionPage, error)
}

type ActionTransitionStore interface {
	DispatchStore
	PendingActionTransitions(
		context.Context, []byte, []byte, int,
	) (ActionTransitionPage, error)
	CompleteActionTransition(context.Context, ActionTransition) error
}

type ActionAudit struct {
	Phase       string
	Operation   auth.Operation
	Record      ActionRecord
	EffectError error
}

type ActionRecorder interface {
	// RecordAction must be idempotent for phase, action ID, and record version.
	RecordAction(context.Context, ActionAudit) error
}

type ActionEventPublisher interface {
	PublishActionEvent(context.Context, string, ActionRecord) error
	// MayPublishActionEvent reports whether this caller is entitled to
	// publish this transition, evaluated with the same identity and standing
	// gates PublishActionEvent applies.
	//
	// It exists so that skipping a row the caller may not publish is a
	// decision rather than an inference from an error code (#480 item 3).
	// The alternative was to skip on ErrorUnauthorized, which makes
	// correctness depend on where #398's concealment turns a refusal into
	// ObjectNotFound: a visibility denial arriving as not-found would block
	// the caller's own transition again, and treating not-found as a skip
	// would swallow a row that is genuinely absent.
	//
	// A false answer means "not mine" — the row stays pending for the
	// principal whose authority it carries. An error means the question
	// could not be answered, which is not the same thing and is never a
	// skip.
	MayPublishActionEvent(
		context.Context, string, ActionRecord) (bool, error)
}

type ActionTransitionPage struct {
	Transitions []ActionTransition
	Next        []byte
}

type DispatchConfig struct {
	Store    DispatchStore
	Registry *Service
	Resolver auth.Resolver
	Recorder ActionRecorder
	Events   ActionEventPublisher
	Clock    func() time.Time
	// Attestations reads claimants' current executor attestations. Optional:
	// nil means none is ever current, so every action that requires
	// attestation is refused to every claimant (fail closed).
	Attestations ExecutorAttestations
	// EvidenceVisibility decides whether the reader of a dispatch record may
	// see an evidence reference carrying a visibility expression.
	//
	// Optional, and nil means no reader may — so labelled evidence is
	// withheld from every dispatch read, which is the fail-closed direction
	// for the same reason Attestations takes it. A plane that cannot evaluate
	// a label must not hand out identifiers the label exists to protect
	// (#369).
	EvidenceVisibility EvidenceVisibility
	// EvidenceLabels translates, when an executor's evidence is recorded,
	// the free-form ingest labels on each reference into the structured
	// label-policy terms EvidenceVisibility can decide (#564, #570).
	//
	// Optional. Nil records the executor's visibility as reported, and a
	// reference stored with a free-form label is then withheld from every
	// reader, holders included: fail closed, never open.
	EvidenceLabels evidencelabels.Translator
	// EvidenceNodes decides an evidence reference that names nodes by those
	// nodes' current access rules, so a relabel after recording governs
	// the evidence as it governs the document (#564). Optional, and nil
	// withholds every reference that names a node from every reader.
	EvidenceNodes evidencelabels.NodeGate
}

// EvidenceVisibility answers whether the current reader holds the labels an
// evidence reference carries (#369).
//
// An alias, not a definition: the one definition lives in the leaf package
// evidencelabels, which the event plane also uses for lifecycle delivery
// (#562), so there is exactly one notion of "may this reader see this label"
// for stored evidence. Implemented by the host (authorized.LabelVisibility,
// #564), which decides it from the reader's own decision.
type EvidenceVisibility = evidencelabels.Visibility

func (r ActionRecord) Validate() error {
	if err := validateOpaque("action ID", r.ID, false); err != nil {
		return err
	}
	if err := validateOpaque("action idempotency key", r.IdempotencyKey, false); err != nil {
		return err
	}
	if r.Version == 0 {
		return shoal.NewError(shoal.ErrorInvalidArgument, "action version is required")
	}
	switch r.State {
	case DispatchQueued, DispatchClaimed, DispatchSucceeded, DispatchFailed, DispatchCanceled:
	default:
		return shoal.NewError(shoal.ErrorInvalidArgument, "action state is invalid")
	}
	if err := shoal.ValidateRequiredID("action agent ID", r.AgentID); err != nil {
		return err
	}
	if r.AgentGeneration <= 0 {
		return shoal.NewError(shoal.ErrorInvalidArgument, "action agent generation is invalid")
	}
	if err := validateName("capability", r.Capability); err != nil {
		return err
	}
	if err := validateName("action", r.Action); err != nil {
		return err
	}
	for name, value := range map[string][]byte{"action source": r.SourceID, "action policy": r.PolicyID} {
		if len(value) == 0 || len(value) > auth.MaxPolicyComponentBytes {
			return shoal.NewError(shoal.ErrorInvalidArgument, name+" is outside its byte bound")
		}
	}
	if err := shoal.ValidateRequiredID("action object ID", r.ObjectID); err != nil {
		return err
	}
	if _, _, err := validateJSONDocument("action input", r.Input, MaxActionPayloadBytes); err != nil {
		return err
	}
	if len(r.Output) > 0 {
		if _, _, err := validateJSONDocument("action output", r.Output, MaxActionOutputBytes); err != nil {
			return err
		}
	}
	if err := validateActionRecordError(r.ErrorCode); err != nil {
		return err
	}
	if err := shoal.ValidateRequiredID("action subject", r.Subject); err != nil {
		return err
	}
	if err := shoal.ValidateRequiredID("action actor", r.Actor); err != nil {
		return err
	}
	if err := shoal.ValidateOptionalID("action client", r.ClientID); err != nil {
		return err
	}
	if len(r.OnBehalfOf) > auth.MaxOnBehalfOfEntries {
		return shoal.NewError(shoal.ErrorInvalidArgument, "action delegation chain exceeds its bound")
	}
	for _, identity := range r.OnBehalfOf {
		if err := shoal.ValidateRequiredID("action delegation identity", identity); err != nil {
			return err
		}
	}
	// The claimant's chain is bounded and validated exactly like the
	// enqueuer's. It reaches the record from a decision rather than from a
	// request body, so this is defence in depth and nothing more — an earlier
	// version of this comment claimed it guarded a record decoded from an
	// older encoding, which cannot happen: such a record has no claimant chain
	// at all, not an over-long one. What it does guard is a corrupt or
	// tampered stored value.
	if err := shoal.ValidateOptionalID(
		"action claimant", r.ClaimantSubject); err != nil {
		return err
	}
	if err := shoal.ValidateOptionalID(
		"action claimant actor", r.ClaimantActor); err != nil {
		return err
	}
	if err := shoal.ValidateOptionalID(
		"action claimant client", r.ClaimantClientID); err != nil {
		return err
	}
	if len(r.ClaimantOnBehalfOf) > auth.MaxOnBehalfOfEntries {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"action claimant delegation chain exceeds its bound")
	}
	for _, identity := range r.ClaimantOnBehalfOf {
		if err := shoal.ValidateRequiredID(
			"action claimant delegation identity", identity); err != nil {
			return err
		}
	}
	if err := validateClaimAttestationID(
		"action claim attestation ID", r.ClaimAttestationID); err != nil {
		return err
	}
	if r.ClaimAttestationID != "" && r.ClaimFence == 0 {
		return shoal.NewError(shoal.ErrorInvalidArgument,
			"action carries a claim attestation without a claim")
	}
	if r.ClaimExecutorRef != "" {
		if err := executorref.ValidExecutorRef(r.ClaimExecutorRef); err != nil {
			return shoal.NewError(shoal.ErrorInvalidArgument,
				"action claim executor reference: "+err.Error())
		}
		if r.ClaimFence == 0 {
			return shoal.NewError(shoal.ErrorInvalidArgument,
				"action carries a claim executor reference without a claim")
		}
	}
	if len(r.ClaimHistory) > MaxActionClaimHistory {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "action claim history exceeds its bound")
	}
	for _, holder := range r.ClaimHistory {
		if err := holder.validate(); err != nil {
			return err
		}
	}
	if len(r.AmbiguityReports) > MaxActionAmbiguityReports {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"action ambiguity reports exceed their bound")
	}
	for _, report := range r.AmbiguityReports {
		if err := report.validate(); err != nil {
			return err
		}
	}
	if r.PolicyGeneration <= 0 || r.AuthorizationExpiresAt.IsZero() {
		return shoal.NewError(shoal.ErrorInvalidArgument, "action authorization provenance is incomplete")
	}
	if err := shoal.ValidateRequiredID("action request ID", r.RequestID); err != nil {
		return err
	}
	if err := shoal.ValidateOptionalID("action correlation ID", r.CorrelationID); err != nil {
		return err
	}
	if err := shoal.ValidateOptionalID(
		"action transition request ID", r.TransitionRequestID,
	); err != nil {
		return err
	}
	if err := shoal.ValidateOptionalID(
		"action transition correlation ID", r.TransitionCorrelationID,
	); err != nil {
		return err
	}
	if r.TransitionRequestID == "" && r.TransitionCorrelationID != "" {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"action transition correlation requires a request ID",
		)
	}
	if r.CancelAuthorizationExpiresAt.IsZero() !=
		(r.CancelAuthorizationFingerprint == auth.Fingerprint{}) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"action cancellation authorization provenance is incomplete",
		)
	}
	if err := r.Reason.Validate(); err != nil {
		return err
	}
	for name, value := range map[string]time.Time{
		"deadline": r.Deadline, "created time": r.CreatedAt, "updated time": r.UpdatedAt,
	} {
		if value.IsZero() || value.Location() != time.UTC {
			return shoal.NewError(shoal.ErrorInvalidArgument, "action "+name+" must be UTC")
		}
	}
	if r.UpdatedAt.Before(r.CreatedAt) || !r.CreatedAt.Before(r.Deadline) {
		return shoal.NewError(shoal.ErrorInvalidArgument, "action timestamps are inconsistent")
	}
	if len(r.AuthorizedOperations) == 0 {
		return shoal.NewError(shoal.ErrorInvalidArgument, "authorized action operations are required")
	}
	for i, operation := range r.AuthorizedOperations {
		if err := operation.Validate(); err != nil {
			return err
		}
		if i > 0 && r.AuthorizedOperations[i-1] >= operation {
			return shoal.NewError(shoal.ErrorInvalidArgument, "authorized action operations are not canonical")
		}
	}
	if err := validateOpaque("executor idempotency key", r.ExecutorKey, false); err != nil {
		return err
	}
	if r.State == DispatchCanceled {
		if err := validateOpaque("cancel mutation key", r.CancelKey, false); err != nil {
			return err
		}
	} else if len(r.CancelKey) != 0 {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"non-canceled action carries a cancel mutation key",
		)
	}
	if r.State == DispatchQueued {
		if len(r.ClaimID) != 0 || r.ClaimFence != 0 ||
			r.ClaimLease != 0 || !r.ClaimLeaseUntil.IsZero() {
			return shoal.NewError(shoal.ErrorInvalidArgument, "queued action carries claim state")
		}
	} else if r.State != DispatchCanceled || r.ClaimFence != 0 {
		if err := validateOpaque("action claim ID", r.ClaimID, false); err != nil {
			return err
		}
		if r.ClaimFence == 0 || r.ClaimLease <= 0 ||
			r.ClaimLease > MaxActionClaimTTL || r.ClaimLeaseUntil.IsZero() ||
			r.ClaimLeaseUntil.Location() != time.UTC {
			return shoal.NewError(shoal.ErrorInvalidArgument, "action claim state is incomplete")
		}
	}
	if len(r.ClaimID) == 0 && r.ClaimLease != 0 {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "action claim lease lacks a claim ID")
	}
	// A volume means one thing — how much of an irreversible egress happened
	// before a failure — so it is only coherent on a failed record (#427).
	// Permitting it elsewhere would make it a number attached to work that
	// did not fail, which no reader could interpret.
	if !r.Effected.Zero() {
		if r.State != DispatchFailed {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"a "+string(r.State)+" action carries an effected volume")
		}
		if err := r.Effected.validate(); err != nil {
			return err
		}
	}
	if (r.State == DispatchClaimed || r.State == DispatchSucceeded || r.State == DispatchFailed) &&
		(r.ExecutionPolicyGeneration <= 0 || r.ExecutionExpiresAt.IsZero()) {
		return shoal.NewError(shoal.ErrorInvalidArgument, "action execution authorization is incomplete")
	}
	// EffectPossible is deliberately not required here. It was, and that
	// requirement enforced a falsehood: it made every terminal record assert
	// an effect may have happened, including an action declaring no external
	// effect at all, which is the case applyClaim goes out of its way to
	// leave alone. A reader could not use the flag because it was a constant
	// (#510).
	//
	// Nothing is lost by dropping it. applyClaim sets the flag from the
	// action's declaration, the completion path carries it forward rather
	// than asserting it, and the store refuses to let it fall (#461). So a
	// terminal record carrying false now means something: this action could
	// not have had an external effect.
	if r.State == DispatchSucceeded && len(r.Output) == 0 {
		return shoal.NewError(shoal.ErrorInvalidArgument, "successful action outcome is incomplete")
	}
	if r.State == DispatchFailed && r.ErrorCode == "" {
		return shoal.NewError(shoal.ErrorInvalidArgument, "failed action outcome is incomplete")
	}
	if len(r.Evidence) > 0 {
		if err := shoal.ValidateRequiredID(
			"action evidence snapshot ID", r.EvidenceSnapshotID); err != nil {
			return err
		}
		if r.EvidenceSnapshotAsOf.IsZero() {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"action evidence snapshot time is required")
		}
		if r.UpdatedAt.Before(r.EvidenceSnapshotAsOf) ||
			r.ExecutionExpiresAt.Before(r.EvidenceSnapshotAsOf) {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"action evidence snapshot time is outside execution bounds")
		}
	} else if r.EvidenceSnapshotID != "" ||
		!r.EvidenceSnapshotAsOf.IsZero() {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"action evidence snapshot requires evidence")
	}
	if err := validateAdmittedDeclaration(r); err != nil {
		return err
	}
	if err := validateApprovalProvenance(r); err != nil {
		return err
	}
	return validateEvidence(r.Evidence)
}

// ErrorCodeOrigin says who decided an action's error code.
//
// Without it the field was an unauthenticated string that the service and the
// executor both wrote, through the same assignment, with nothing recording
// which. A worker could report "invalid_executor_output" — the code the
// *service* assigns when it rejects an executor's output — and produce a
// record reading as though Shoal had refused the work when Shoal accepted the
// report without objection. The sharpest case is "invalid_executor_error",
// which the service assigns *because* it rejected the worker's own code.
//
// Derived from which code path wrote the field, never from the request, for
// the reason AmbiguityReport.Subject is: provenance a caller can set is not
// provenance.
type ErrorCodeOrigin string

const (
	// ErrorCodeOriginUnknown is a record written before this field existed.
	// Its code may have come from either, which is exactly what a reader
	// needs to be told rather than left to assume.
	ErrorCodeOriginUnknown ErrorCodeOrigin = ""
	// ErrorCodeOriginService is an adjudication: the service examined what the
	// executor returned and refused it.
	ErrorCodeOriginService ErrorCodeOrigin = "service"
	// ErrorCodeOriginExecutor is the executor's own account of why the work
	// failed. Untrusted, in the sense that the service neither chose it nor
	// can verify it — only that it is bounded and printable.
	ErrorCodeOriginExecutor ErrorCodeOrigin = "executor"
)

// reservedActionErrorCodes are the codes the service assigns itself, which no
// executor may report.
//
// Reserving them is not merely tidiness. Each one is a statement about what
// *Shoal* did, so a worker reporting one is asserting an adjudication that
// never happened — and an operator reconciling a failure reads these to decide
// whether the platform or the target refused the work.
var reservedActionErrorCodes = map[string]struct{}{
	"invalid_executor_output":   {},
	"invalid_executor_evidence": {},
	"invalid_executor_error":    {},
	"invalid_executor_effected": {},
	"executor_error":            {},
}

func validateActionRecordError(value string) error {
	if len(value) > MaxActionErrorBytes || strings.TrimSpace(value) != value {
		return shoal.NewError(shoal.ErrorInvalidArgument, "action error code is invalid")
	}
	return nil
}

// validateReportedActionErrorCode is validateActionRecordError plus the
// reservation, for a code that arrived from an executor rather than from this
// process.
//
// Refused rather than recorded-and-flagged, because the two defences answer
// different questions and both are wanted: the reservation stops a worker
// claiming an adjudication it cannot make, and the origin field tells a reader
// which of the two wrote any code at all — including the next code someone
// adds without remembering to reserve it.
func validateReportedActionErrorCode(value string) error {
	if err := validateActionRecordError(value); err != nil {
		return err
	}
	if _, reserved := reservedActionErrorCodes[value]; reserved {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"action error code "+value+" is assigned by the service and may "+
				"not be reported by an executor")
	}
	return nil
}

func validateOpaque(name string, value []byte, optional bool) error {
	if optional && len(value) == 0 {
		return nil
	}
	if len(value) == 0 || len(value) > MaxActionIDBytes {
		return shoal.NewError(shoal.ErrorInvalidArgument, name+" is outside its byte bound")
	}
	return nil
}

func validateEvidence(values []EvidenceRef) error {
	if len(values) > MaxActionEvidence {
		return shoal.NewError(shoal.ErrorInvalidArgument, "action evidence exceeds its bound")
	}
	totalMembers := 0
	for index, value := range values {
		reference, err := value.interactionReference().Canonical()
		if err != nil {
			return err
		}
		// An empty label set is public: a source that declares no visibility
		// is unrestricted everywhere else in the platform, and evidence drawn
		// from one therefore carries no labels. Requiring a label here made
		// every anchor from an unlabeled corpus unrecordable. A reader
		// matching labels must treat the empty set as matching every reader.
		normalized, err := interaction.Conjoin(value.Visibility)
		if err != nil {
			return err
		}
		if len(normalized) != len(value.Visibility) {
			return shoal.NewError(shoal.ErrorInvalidArgument, "evidence visibility must be canonical")
		}
		for i := range normalized {
			if normalized[i] != value.Visibility[i] {
				return shoal.NewError(shoal.ErrorInvalidArgument, "evidence visibility must be canonical")
			}
		}
		totalMembers += len(reference.NodeIDs) + len(reference.EdgeIDs) +
			len(reference.Assertions)
		if totalMembers > MaxActionEvidence {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"action evidence members exceed their bound")
		}
		for prior := 0; prior < index; prior++ {
			if equalEvidenceRef(values[prior], value) {
				return shoal.NewError(shoal.ErrorInvalidArgument, "action evidence contains duplicates")
			}
		}
	}
	return nil
}

func equalEvidenceRef(left, right EvidenceRef) bool {
	leftCanonical, leftErr := left.interactionReference().Canonical()
	rightCanonical, rightErr := right.interactionReference().Canonical()
	return leftErr == nil && rightErr == nil &&
		reflect.DeepEqual(leftCanonical, rightCanonical) &&
		len(left.Visibility) == len(right.Visibility) &&
		equalEvidenceVisibility(left.Visibility, right.Visibility)
}

func (r EvidenceRef) interactionReference() interaction.EvidenceReference {
	return interaction.EvidenceReference{
		AnchorID: r.AnchorID, Kind: r.Kind, Citation: r.Citation,
		NodeIDs:    append([]shoal.ID(nil), r.NodeIDs...),
		EdgeIDs:    append([]shoal.ID(nil), r.EdgeIDs...),
		Assertions: append([]interaction.AssertionReference(nil), r.Assertions...),
	}
}

func equalEvidenceVisibility(left, right []string) bool {
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func validateJSONDocument(name string, raw json.RawMessage, limit int) (json.RawMessage, any, error) {
	if len(raw) == 0 || len(raw) > limit {
		return nil, nil, shoal.NewError(shoal.ErrorInvalidArgument, name+" is outside its byte bound")
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, nil, shoal.NewError(shoal.ErrorInvalidArgument, name+" is invalid JSON")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, nil, shoal.NewError(shoal.ErrorInvalidArgument, name+" has trailing JSON")
	}
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) > limit {
		return nil, nil, shoal.NewError(shoal.ErrorInvalidArgument, name+" exceeds its byte bound")
	}
	return encoded, value, nil
}

// validateAgainstSchema implements the bounded declarative subset supported by
// fleet descriptors: type, properties, required, items, enum, and
// additionalProperties. Unsupported schema keywords fail closed.
func validateAgainstSchema(schema json.RawMessage, document json.RawMessage, name string, limit int) (json.RawMessage, error) {
	canonical, value, err := validateJSONDocument(name, document, limit)
	if err != nil {
		return nil, err
	}
	var root map[string]any
	decoder := json.NewDecoder(bytes.NewReader(schema))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "registered action schema is invalid")
	}
	if err := validateSchemaValue(root, value, "$", 0); err != nil {
		return nil, shoal.WrapError(shoal.ErrorInvalidArgument, name+" does not match its schema", err)
	}
	return canonical, nil
}

func validateSchemaValue(schema map[string]any, value any, path string, depth int) error {
	if depth > 32 {
		return fmt.Errorf("%s exceeds schema depth", path)
	}
	allowed := map[string]bool{"type": true, "properties": true, "required": true, "items": true, "enum": true, "additionalProperties": true}
	for key := range schema {
		if !allowed[key] {
			return fmt.Errorf("%s uses unsupported schema keyword %q", path, key)
		}
	}
	if rawType, exists := schema["type"]; exists {
		if _, ok := rawType.(string); !ok {
			return fmt.Errorf("%s type must be a string", path)
		}
	}
	if rawProperties, exists := schema["properties"]; exists {
		if _, ok := rawProperties.(map[string]any); !ok {
			return fmt.Errorf("%s properties must be an object", path)
		}
	}
	if rawRequired, exists := schema["required"]; exists {
		if _, ok := rawRequired.([]any); !ok {
			return fmt.Errorf("%s required must be an array", path)
		}
	}
	if rawItems, exists := schema["items"]; exists {
		if _, ok := rawItems.(map[string]any); !ok {
			return fmt.Errorf("%s items must be an object", path)
		}
	}
	if rawAdditional, exists := schema["additionalProperties"]; exists {
		if _, ok := rawAdditional.(bool); !ok {
			return fmt.Errorf("%s additionalProperties must be a boolean", path)
		}
	}
	if enums, ok := schema["enum"].([]any); ok {
		matched := false
		for _, candidate := range enums {
			left, _ := json.Marshal(candidate)
			right, _ := json.Marshal(value)
			if bytes.Equal(left, right) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("%s is not an allowed value", path)
		}
	} else if _, exists := schema["enum"]; exists {
		return fmt.Errorf("%s enum must be an array", path)
	}
	kind, _ := schema["type"].(string)
	if kind == "" {
		if _, ok := schema["items"]; ok {
			kind = "array"
		} else if _, properties := schema["properties"]; properties {
			kind = "object"
		} else if _, required := schema["required"]; required {
			kind = "object"
		} else if _, additional := schema["additionalProperties"]; additional {
			kind = "object"
		}
	}
	switch kind {
	case "", "object", "array", "string", "number", "integer", "boolean", "null":
	default:
		return fmt.Errorf("%s has unsupported type %q", path, kind)
	}
	switch kind {
	case "object":
		object, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%s must be an object", path)
		}
		properties, _ := schema["properties"].(map[string]any)
		required, requiredOK := schema["required"].([]any)
		if _, exists := schema["required"]; exists && !requiredOK {
			return fmt.Errorf("%s required must be an array", path)
		}
		for _, entry := range required {
			key, ok := entry.(string)
			if !ok {
				return fmt.Errorf("%s required must contain strings", path)
			}
			if _, ok := object[key]; !ok {
				return fmt.Errorf("%s.%s is required", path, key)
			}
		}
		additional, hasAdditional := schema["additionalProperties"].(bool)
		for key, child := range object {
			rawChild, found := properties[key]
			if !found {
				if hasAdditional && !additional {
					return fmt.Errorf("%s.%s is not allowed", path, key)
				}
				continue
			}
			childSchema, ok := rawChild.(map[string]any)
			if !ok {
				return fmt.Errorf("%s.%s schema is invalid", path, key)
			}
			if err := validateSchemaValue(childSchema, child, path+"."+key, depth+1); err != nil {
				return err
			}
		}
	case "array":
		array, ok := value.([]any)
		if !ok {
			return fmt.Errorf("%s must be an array", path)
		}
		if rawItems, ok := schema["items"]; ok {
			itemSchema, ok := rawItems.(map[string]any)
			if !ok {
				return fmt.Errorf("%s items schema is invalid", path)
			}
			for index, item := range array {
				if err := validateSchemaValue(itemSchema, item, fmt.Sprintf("%s[%d]", path, index), depth+1); err != nil {
					return err
				}
			}
		}
	case "string":
		if _, ok := value.(string); !ok {
			return fmt.Errorf("%s must be a string", path)
		}
	case "number":
		if _, ok := value.(json.Number); !ok {
			return fmt.Errorf("%s must be a number", path)
		}
	case "integer":
		number, ok := value.(json.Number)
		if !ok || strings.ContainsAny(number.String(), ".eE") {
			return fmt.Errorf("%s must be an integer", path)
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s must be a boolean", path)
		}
	case "null":
		if value != nil {
			return fmt.Errorf("%s must be null", path)
		}
	}
	return nil
}

func cloneActionRecord(input ActionRecord) ActionRecord {
	result := input
	result.ID = append([]byte(nil), input.ID...)
	result.IdempotencyKey = append([]byte(nil), input.IdempotencyKey...)
	result.SourceID = append([]byte(nil), input.SourceID...)
	result.PolicyID = append([]byte(nil), input.PolicyID...)
	result.Input = append(json.RawMessage(nil), input.Input...)
	result.Output = append(json.RawMessage(nil), input.Output...)
	result.OnBehalfOf = append([]shoal.ID(nil), input.OnBehalfOf...)
	result.AuthorizedOperations = append([]auth.Operation(nil), input.AuthorizedOperations...)
	result.ClaimID = append([]byte(nil), input.ClaimID...)
	result.ClaimantOnBehalfOf = append(
		[]shoal.ID(nil), input.ClaimantOnBehalfOf...)
	result.ClaimHistory = make([]ClaimHolder, len(input.ClaimHistory))
	for index, holder := range input.ClaimHistory {
		result.ClaimHistory[index] = holder
		result.ClaimHistory[index].ClaimID = append(
			[]byte(nil), holder.ClaimID...)
		result.ClaimHistory[index].OnBehalfOf = append(
			[]shoal.ID(nil), holder.OnBehalfOf...)
	}
	if input.ClaimHistory == nil {
		result.ClaimHistory = nil
	}
	result.AmbiguityReports = append(
		[]AmbiguityReport(nil), input.AmbiguityReports...)
	result.CancelKey = append([]byte(nil), input.CancelKey...)
	result.ExecutorKey = append([]byte(nil), input.ExecutorKey...)
	result.Evidence = cloneActionEvidence(input.Evidence)
	result.AdmittedEffects = input.AdmittedEffects.clone()
	result.AdmittedDisclosures = append(
		[]byte(nil), input.AdmittedDisclosures...)
	result.AdmittedObligation = append(
		[]byte(nil), input.AdmittedObligation...)
	// AdmittedIdentityScheme needs no line: the struct assignment above copies
	// a scalar, and unlike the slices there is no backing array to share. An
	// explicit copy for it would be a line no mutation could observe.
	result.ApprovalRequestDigest = append(
		[]byte(nil), input.ApprovalRequestDigest...)
	return result
}

// isAdmission reports whether this record was produced by the pre-call
// admission seam rather than by dispatch.
//
// The declared effect set is the marker, and it is a reliable one in both
// directions: an admission is refused before it reaches a record unless it
// declares an effect, and no dispatch enqueue ever sets one.
func (r ActionRecord) isAdmission() bool {
	return len(r.AdmittedEffects) > 0
}

func cloneActionEvidence(input []EvidenceRef) []EvidenceRef {
	result := make([]EvidenceRef, len(input))
	for i, evidence := range input {
		result[i] = evidence
		result[i].NodeIDs = append(
			[]shoal.ID(nil), evidence.NodeIDs...)
		result[i].EdgeIDs = append(
			[]shoal.ID(nil), evidence.EdgeIDs...)
		result[i].Assertions = append(
			[]interaction.AssertionReference(nil), evidence.Assertions...)
		result[i].Visibility = append([]string(nil), evidence.Visibility...)
	}
	return result
}

func canonicalOperations(values []auth.Operation) []auth.Operation {
	result := append([]auth.Operation(nil), values...)
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	out := result[:0]
	for _, operation := range result {
		if len(out) == 0 || out[len(out)-1] != operation {
			out = append(out, operation)
		}
	}
	return out
}
