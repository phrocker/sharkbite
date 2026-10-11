// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package fleet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash"
	"slices"
	"sort"
	"sync/atomic"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/evidencelabels"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type DispatchService struct {
	store    DispatchStore
	outbox   ActionTransitionStore
	registry *Service
	resolver auth.Resolver
	recorder ActionRecorder
	events   ActionEventPublisher
	clock    func() time.Time
	// attestations is consulted only for actions that require attestation.
	attestations       ExecutorAttestations
	evidenceVisibility EvidenceVisibility
	evidenceLabels     evidencelabels.Translator
	evidenceNodes      evidencelabels.NodeGate
	// skippedPublications counts transition rows this process left pending
	// because the caller was not entitled to publish them. See
	// SkippedTransitionPublications.
	skippedPublications atomic.Uint64
}

// SkippedTransitionPublications is the number of pending transition rows this
// process has left in place because the reconciling caller was not entitled to
// publish them.
//
// A counter, deliberately, and not a gauge of what is pending (#642). A row
// strands when its owning principal never makes another request on that
// action, and nothing visits it — so no request reaches the skip, and any
// gauge built from this would read healthy exactly when the condition it was
// added to detect is occurring. What this can honestly report is "requests are
// encountering rows they cannot drain", which is the signal that the condition
// is reachable at all.
//
// Process-local and reset by a restart. The durable signal needs a
// pending-only index (#647), and belongs behind authentication rather than on
// the unauthenticated health port, because an age or depth over pending work
// tracks tenant workload rather than this process's own behaviour.
func (s *DispatchService) SkippedTransitionPublications() uint64 {
	return s.skippedPublications.Load()
}

func NewDispatchService(config DispatchConfig) (*DispatchService, error) {
	if config.Store == nil || config.Registry == nil || config.Resolver == nil ||
		config.Recorder == nil || config.Events == nil || config.Clock == nil {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "fleet dispatch dependencies are required")
	}
	outbox, ok := config.Store.(ActionTransitionStore)
	if !ok {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"durable fleet action transition store is required")
	}
	service := &DispatchService{
		store: config.Store, registry: config.Registry, resolver: config.Resolver,
		recorder: config.Recorder, events: config.Events, clock: config.Clock,
		outbox: outbox, attestations: config.Attestations,
		evidenceVisibility: config.EvidenceVisibility,
		evidenceLabels:     config.EvidenceLabels,
		evidenceNodes:      config.EvidenceNodes,
	}
	return service, nil
}

func (s *DispatchService) Enqueue(ctx context.Context, request EnqueueRequest) (ActionRecord, error) {
	return s.enqueue(ctx, request, auth.OperationDispatch, false)
}

func (s *DispatchService) enqueue(
	ctx context.Context,
	request EnqueueRequest,
	operation auth.Operation,
	tolerateLapse bool,
) (ActionRecord, error) {
	ctx, cancel := s.deadline(ctx, request.Context)
	defer cancel()
	decision, now, err := s.begin(ctx, operation, request.Context)
	if err != nil {
		return ActionRecord{}, err
	}
	// The admission region is refused here, and this is the only place it has
	// to be: enqueue is the sole entry point at which a caller names a durable
	// action it does not already own. Everything else takes an ID it must
	// already hold, and answers a foreign or absent one identically.
	//
	// Refused before the store is read, and without regard to what is there,
	// because the answer is the whole point. This read has no principal check —
	// it cannot have one, since a caller legitimately enqueues at an ID nobody
	// holds — so a conditional refusal would still separate occupied from
	// absent. That was the live oracle: an admission ID is derivable from a
	// principal tuple that is knowable, so a caller could compute a victim's
	// admission ID, submit it here, and read occupancy off conflict versus
	// success. It could also squat an unheld one and deny the victim its own
	// admission by name.
	if reservedAdmissionID(request.ID) {
		return ActionRecord{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "action ID is reserved")
	}
	record, action, err := s.queuedRecord(
		ctx, decision, request, operation, now, tolerateLapse)
	if err != nil {
		return ActionRecord{}, err
	}
	if current, readErr := s.store.GetAction(ctx, request.ID); readErr == nil {
		if equivalentEnqueue(current, record) {
			if err := s.publishTransition(
				context.WithoutCancel(ctx), actionEventKind(current), current,
			); err != nil {
				return ActionRecord{}, errors.Join(ErrActionCommitted, err)
			}
			// Redacted like every other read (#369). An idempotent re-enqueue
			// of an action that has since completed hands the enqueuer a
			// terminal record, and the evidence on it was recorded by whoever
			// executed — which since #437 may be an execute-authorized worker
			// retrieving under its own labels.
			return s.readableRecord(ctx, cloneActionRecord(current))
		}
		return ActionRecord{}, ErrActionConflict
	} else if !errors.Is(readErr, ErrActionNotFound) {
		return ActionRecord{}, readErr
	}
	// An action that requires approval is never created here (#451). The only
	// way to it is the approval route, which writes nothing an executor can
	// see until an approver has approved this exact request.
	//
	// After the replay branch on purpose, and that placement is a decision
	// about work that already exists. A record this caller already holds is
	// returned as it is: either it was queued before the flag was registered —
	// in which case registering the flag moved the agent's generation and the
	// record no longer resolves for Pull, Claim or completion — or it is the
	// record an approval materialized, which this call merely reads back. In
	// neither case does the replay create work, so refusing it would only
	// make a caller's retry fail where its first attempt succeeded.
	//
	// Not audited. Nothing is written and no action exists for an audit to
	// name; the refusal is a static property of the registered descriptor that
	// any caller with dispatch on the scope can already read, and the approval
	// route — which is audited at every transition — is where approval-required
	// work is supposed to go. Recording every refused attempt would let any
	// dispatch holder write audit entries at will without producing anything.
	if action.RequiresApproval {
		return ActionRecord{}, approvalRequired()
	}
	if err := s.recorder.RecordAction(ctx, ActionAudit{Phase: "enqueue_admission", Operation: operation, Record: record}); err != nil {
		return ActionRecord{}, errors.Join(ErrRecordingUnavailable, err)
	}
	stored, err := s.store.ApplyAction(ctx, DispatchMutation{
		Token: transitionToken(
			"enqueue", request.ID, request.IdempotencyKey, record.Version),
		ExpectedVersion: 0, TransitionKind: "action.enqueued", Record: record,
	})
	if err != nil {
		return ActionRecord{}, err
	}
	if err := s.publishTransition(
		context.WithoutCancel(ctx), "action.enqueued", stored,
	); err != nil {
		return ActionRecord{}, errors.Join(ErrActionCommitted, err)
	}
	return cloneActionRecord(stored), nil
}

// queuedRecord validates a dispatch request and builds the version-1 record an
// enqueue would commit, without committing anything.
//
// It is split out so admission can reach the same record without going through
// the queued state. Every check an enqueue makes has to be made before an
// admission writes anything, and the alternative — admission repeating the
// validation itself — is how a second entry point ends up admitting an input
// the first would have refused. The resolved Action comes back with it because
// admission needs the declared effect ceiling, and resolving twice would ask
// the registry a question it has already answered.
func (s *DispatchService) queuedRecord(
	ctx context.Context,
	decision auth.Decision,
	request EnqueueRequest,
	operation auth.Operation,
	now time.Time,
	tolerateLapse bool,
) (ActionRecord, Action, error) {
	record, action, _, err := s.queuedRecordBinding(
		ctx, decision, request, operation, now, tolerateLapse)
	return record, action, err
}

// queuedRecordBinding is queuedRecord that also returns the bound
// descriptor's executor ref, which admission needs for the attestation read.
func (s *DispatchService) queuedRecordBinding(
	ctx context.Context,
	decision auth.Decision,
	request EnqueueRequest,
	operation auth.Operation,
	now time.Time,
	// tolerateLapse resolves a descriptor whose lease has lapsed.
	//
	// Set only by Invoke, and only after an authorized read has established
	// that the caller is the principal of an existing *terminal* record at
	// this ID (#578). Under that condition it cannot admit new work: enqueue
	// will either replay the record or conflict with it, never create. And
	// it is not an oracle, because the read it is derived from answers a
	// foreign ID and an absent one identically — so a caller who does not
	// hold the record sees exactly the refusal it saw before.
	//
	// Revocation is still refused: activeChainLapsing checks RevokedAt
	// regardless of this flag.
	tolerateLapse bool,
) (ActionRecord, Action, string, error) {
	if err := validateOpaque("action ID", request.ID, false); err != nil {
		return ActionRecord{}, Action{}, "", err
	}
	if err := validateOpaque("action idempotency key", request.IdempotencyKey, false); err != nil {
		return ActionRecord{}, Action{}, "", err
	}
	if request.AgentGeneration <= 0 {
		return ActionRecord{}, Action{}, "", shoal.NewError(shoal.ErrorInvalidArgument, "agent generation must be positive")
	}
	if err := validateName("capability", request.Capability); err != nil {
		return ActionRecord{}, Action{}, "", err
	}
	if err := validateName("action", request.Action); err != nil {
		return ActionRecord{}, Action{}, "", err
	}
	if request.Context.Deadline.Sub(now) > MaxActionDeadline {
		return ActionRecord{}, Action{}, "", shoal.NewError(shoal.ErrorInvalidArgument, "action deadline exceeds its bound")
	}
	// Binding, not execution: queueing work for an executor that runs out of
	// process must not require it to be runnable here.
	descriptor, action, _, err := s.registry.resolveActionBindingLapsing(
		ctx, decision, request.AgentID, request.AgentGeneration,
		request.Capability, request.Action, request.SourceID, request.PolicyID,
		request.ObjectID, operation, now,
		// Pinned. The dispatcher read a descriptor and built this request
		// from it, so it should fail if the descriptor moved underneath.
		true,
		tolerateLapse,
		tolerateLapse,
	)
	if err != nil {
		return ActionRecord{}, Action{}, "", err
	}
	input, err := validateAgainstSchema(action.InputSchema, request.Input, "action input", MaxActionPayloadBytes)
	if err != nil {
		return ActionRecord{}, Action{}, "", err
	}
	reason, err := interaction.NewReason(request.Context.ReasonCode, request.Context.ReasonDetail)
	if err != nil {
		return ActionRecord{}, Action{}, "", err
	}
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		return ActionRecord{}, Action{}, "", err
	}
	return ActionRecord{
		ID: append([]byte(nil), request.ID...), IdempotencyKey: append([]byte(nil), request.IdempotencyKey...),
		Version: 1, State: DispatchQueued, AgentID: descriptor.ID,
		AgentGeneration: descriptor.Generation, Capability: request.Capability, Action: request.Action,
		SourceID: append([]byte(nil), request.SourceID...), PolicyID: append([]byte(nil), request.PolicyID...),
		ObjectID: request.ObjectID, Input: input, Subject: decision.Subject(), Actor: decision.Actor(),
		ClientID: decision.ClientID(), OnBehalfOf: decision.OnBehalfOf(),
		AuthorizationFingerprint: fingerprint, PolicyGeneration: decision.PolicyGeneration(),
		AuthorizationExpiresAt: decision.AuthenticationExpires(),
		AuthorizedOperations:   decisionOperations(decision, operation),
		RequestID:              decision.RequestID(), CorrelationID: decision.CorrelationID(),
		Reason: reason, Deadline: request.Context.Deadline.UTC(), CreatedAt: now, UpdatedAt: now,
		ExecutorKey: executorKey(request.ID, request.IdempotencyKey),
	}, action, descriptor.ExecutorRef, nil
}

func (s *DispatchService) Invoke(ctx context.Context, request InvokeRequest) (ActionRecord, error) {
	// Whether this caller already holds a terminal record at this ID,
	// established before the enqueue and by an authorized read (#578).
	//
	// A caller retrying Invoke is typically the one that lost the response to
	// a call whose effect already happened, and the replay branch below is
	// its route to learning the outcome. Without this the enqueue's own
	// resolve refuses first when the descriptor's lease has lapsed, so the
	// record is intact and unreachable — the effect stranded at the caller
	// instead of in the record.
	//
	// Why this is not the oracle enqueue defends against. That function
	// refuses the admission span "before the store is read, and without
	// regard to what is there", because an enqueue legitimately names an ID
	// nobody holds and so cannot be principal-checked. This read *is*
	// principal-checked: authorizedCurrentBinding answers a foreign ID and
	// an absent one identically, with ObjectNotFound. So a caller that does
	// not hold the record learns nothing it did not already know, and sees
	// exactly the refusal it saw before.
	//
	// And it cannot admit new work. The flag is set only when a *terminal*
	// record already exists, and enqueue then either replays it or conflicts
	// with it — it never creates. Revocation is still refused, through
	// activeChainLapsing's RevokedAt check.
	tolerateLapse := false
	if decision, now, beginErr := s.begin(
		ctx, auth.OperationInvoke, request.Enqueue.Context,
	); beginErr == nil {
		if existing, _, _, readErr := s.authorizedCurrentBinding(
			ctx, decision, request.Enqueue.ID, auth.OperationInvoke, true,
			now, executorPhaseTerminalReplay, 0,
		); readErr == nil && existing.State.terminal() {
			tolerateLapse = true
		}
	}
	queued, err := s.enqueue(
		ctx, request.Enqueue, auth.OperationInvoke, tolerateLapse)
	if err != nil {
		return ActionRecord{}, err
	}
	if queued.State.terminal() {
		decision, now, authorizeErr := s.begin(ctx, auth.OperationInvoke, request.Enqueue.Context)
		if authorizeErr != nil {
			return ActionRecord{}, authorizeErr
		}
		// The enqueue replay path: the caller is by definition the principal
		// that enqueued, so the principal requirement is kept rather than
		// relaxed with the claimant routes.
		// Named as a terminal replay rather than going through
		// authorizedCurrent, which hardcodes executorPhaseNone and would
		// refuse after a lapse — the refusal this change exists to remove.
		// Reachable now that the pre-read above lets the enqueue resolve
		// (#578); on its own it was a phase nothing could reach, which is
		// why it was reverted from #577.
		current, _, _, currentErr := s.authorizedCurrentBinding(
			ctx, decision, queued.ID, auth.OperationInvoke, true, now,
			executorPhaseTerminalReplay, 0,
		)
		if currentErr != nil {
			return ActionRecord{}, currentErr
		}
		if err := s.publishTransition(
			context.WithoutCancel(ctx), actionEventKind(current), current,
		); err != nil {
			return ActionRecord{}, errors.Join(ErrActionCommitted, err)
		}
		// #369, for the same reason as the enqueue replay above: the caller
		// is by definition the principal that enqueued, and the evidence is
		// whoever executed's.
		return s.readableRecord(ctx, current)
	}
	if queued.State == DispatchClaimed &&
		bytes.Equal(queued.ClaimID, request.ClaimID) &&
		s.clock().UTC().Before(queued.ClaimLeaseUntil) {
		return s.ExecuteClaim(ctx, queued)
	}
	// Refuse a dispatch-only binding *before* claiming, not after (#455).
	//
	// ExecuteClaim asserts the bound reference implements ActionExecutor, and
	// an ExternalEffectBinding deliberately does not: the whole point is that
	// the work reaches a gateway over the dispatch queue and the completion
	// report is what Shoal records, so nothing runs in process. Reached
	// through Invoke, that assertion failed *after* Claim had committed — and
	// Claim sets EffectPossible for an action declaring external mutation or
	// egress.
	//
	// So an accidental synchronous invoke against a gateway reference left a
	// durable record saying an effect may have happened, for an action that
	// provably did nothing: the claim committed and the executor resolution
	// failed in this process, before anything was serialized and before
	// anything left the host. The flag is monotonic, so the damage does not
	// wash out — every such invoke permanently adds a record to the set an
	// operator reconciles by hand.
	//
	// It is also easy to hit by accident rather than adversarially. The
	// invoke route is the obvious one, the descriptor registers fine, and
	// nothing about the configuration says this reference is dispatch-only.
	//
	// Resolving first makes it a clean deterministic refusal with no state
	// written beyond the enqueue, which is not an effect. The same ordering
	// argument as #536's: establish what the caller may do before touching
	// anything that records a consequence.
	decision, now, err := s.begin(ctx, auth.OperationInvoke, request.Enqueue.Context)
	if err != nil {
		return ActionRecord{}, err
	}
	if _, _, _, err := s.registry.resolveAction(
		ctx, decision, queued.AgentID, queued.AgentGeneration,
		queued.Capability, queued.Action, queued.SourceID, queued.PolicyID,
		queued.ObjectID, auth.OperationInvoke, now,
	); err != nil {
		return ActionRecord{}, err
	}
	claimed, err := s.Claim(ctx, ClaimRequest{
		ID: queued.ID, ExpectedVersion: queued.Version, ClaimID: request.ClaimID,
		Lease: request.Lease, Context: request.Enqueue.Context,
	})
	if err != nil {
		return ActionRecord{}, err
	}
	return s.ExecuteClaim(ctx, claimed)
}

func (s *DispatchService) Claim(ctx context.Context, request ClaimRequest) (ActionRecord, error) {
	ctx, cancel := s.deadline(ctx, request.Context)
	defer cancel()
	decision, now, err := s.beginClaimant(ctx, request.Context)
	if err != nil {
		return ActionRecord{}, err
	}
	if err := validateOpaque("action ID", request.ID, false); err != nil {
		return ActionRecord{}, err
	}
	if err := validateOpaque("claim ID", request.ClaimID, false); err != nil {
		return ActionRecord{}, err
	}
	if request.ExpectedVersion == 0 || request.Lease <= 0 || request.Lease > MaxActionClaimTTL {
		return ActionRecord{}, shoal.NewError(shoal.ErrorInvalidArgument, "claim version or lease is invalid")
	}
	current, claimedAction, executorRef, authorizing, err := s.authorizedClaimant(
		ctx, decision, request.ID, now, executorPhaseClaim, 0)
	if err != nil {
		return ActionRecord{}, err
	}
	// An admission is not dispatch work. Merging the two claim paths gave
	// dispatch's reclaim semantics reach over admission records, and a reclaim
	// means nothing for an admission: the grant was made to one caller which was
	// told it may perform an effect, and nobody else can finish that. Only the
	// original caller knows whether the effect happened, so a second party
	// taking the record and reporting an outcome would be recording a fiction.
	//
	// An expired admission is abandoned, not reclaimable, and a claimed record
	// with a lapsed lease is the honest statement of that — it is exactly what
	// Outstanding reports as Expired. Refused as not-found, because the caller
	// does hold the record and saying so would separate an admission it owns
	// from an action ID that does not exist.
	if current.isAdmission() {
		return ActionRecord{}, auth.ObjectNotFound()
	}
	if current.Version != request.ExpectedVersion {
		// A replayed claim: this caller's own claim landed and the response
		// was lost, so return the record it already holds rather than a
		// conflict against its own write.
		//
		// The claimant check belongs here for the same reason it belongs in
		// completeClaim. Without it the branch keys on ClaimID, version and
		// lease alone — all three of which a second execute-holder knows,
		// because Pull returns a claimed record whose lease has lapsed with
		// its ClaimID and version intact. That caller would receive the full
		// record of a live claim it does not hold, including the action's
		// input, which Pull deliberately withholds by excluding live-claimed
		// records from the page.
		if current.State == DispatchClaimed &&
			current.Version == request.ExpectedVersion+1 &&
			bytes.Equal(current.ClaimID, request.ClaimID) &&
			current.ClaimLease == request.Lease &&
			holdsClaimOn(decision, current) {
			if err := s.publishTransition(
				context.WithoutCancel(ctx), "action.claimed", current,
			); err != nil {
				return ActionRecord{}, errors.Join(ErrActionCommitted, err)
			}
			return cloneActionRecord(current), nil
		}
		// A version mismatch is the first answer this route produces, so it is
		// also the first place the record's existence can leak.
		//
		// Concealed only when Pull withholds the record. An earlier version of
		// this concealed it unconditionally on the argument that a caller
		// holding the right version never reaches this branch — which ignores
		// the ordinary claim race. Two execute-holders pull the same page, one
		// claims, and the other arrives here with the version it was handed.
		// It needs a conflict so it re-pulls; a not-found for work it just saw
		// in its own page is both useless and false.
		if concealFrom(decision, current, now) {
			return ActionRecord{}, auth.ObjectNotFound()
		}
		return ActionRecord{}, ErrActionConflict
	}
	// The three remaining answers are each the real reason for a caller with
	// standing, and the answer an absent action gets otherwise. The
	// live-claimed branch had no such check at all until a review found it:
	// Pull withholds a live-claimed record, so claiming one by a guessed ID
	// was returning a distinguishable conflict.
	//
	// concealFrom's observable-through-Pull disjunct is dead in all three,
	// because each branch's own condition already implies Pull withholds the
	// record. Only the version mismatch above exercises it. They use the same
	// predicate anyway so that the rule is stated once rather than three times
	// with one of them subtly different, which is how the live-claimed branch
	// came to have no check in the first place.
	if current.State == DispatchClaimed && now.Before(current.ClaimLeaseUntil) {
		if concealFrom(decision, current, now) {
			return ActionRecord{}, auth.ObjectNotFound()
		}
		return ActionRecord{}, ErrActionConflict
	}
	if current.State.terminal() {
		if concealFrom(decision, current, now) {
			return ActionRecord{}, auth.ObjectNotFound()
		}
		return ActionRecord{}, ErrActionTerminal
	}
	if !now.Before(current.Deadline) {
		if concealFrom(decision, current, now) {
			return ActionRecord{}, auth.ObjectNotFound()
		}
		return ActionRecord{}, ErrClaimLost
	}
	// Resolved for the declared Effect, which decides whether claiming this
	// action already makes an effect possible.
	//
	// The declaration comes from authorizedClaimant, which resolved it under
	// whichever operation authorized this caller. This used to re-resolve with
	// a hardcoded OperationInvoke under a comment saying it added no
	// authorization — true while invoke was the only way in, and false the
	// moment execute existed, because it then refused every worker authorized
	// under execute.
	//
	// The attestation is read here, after every authorization, concealment
	// and state branch above, so a caller without standing never reaches the
	// store and never learns the requirement exists. A store failure answers
	// unavailable; it is never read as "not attested".
	attestation, err := s.claimAttestation(ctx, decision,
		effectiveClaimRequirements(current, claimedAction), executorRef, now)
	if err != nil {
		return ActionRecord{}, err
	}
	next := cloneActionRecord(current)
	next.Version++
	next, err = applyClaim(
		next, claimedAction, request.ClaimID, request.Lease,
		decision, authorizing, now, attestation, executorRef)
	if err != nil {
		if errors.Is(err, ErrAttestationRequired) {
			s.auditAttestationRefusal(ctx, current, authorizing, decision)
		}
		return ActionRecord{}, err
	}
	if err := s.recorder.RecordAction(ctx, ActionAudit{Phase: "claim_admission", Operation: authorizing, Record: next}); err != nil {
		return ActionRecord{}, errors.Join(ErrRecordingUnavailable, err)
	}
	stored, err := s.store.ApplyAction(ctx, DispatchMutation{
		Token:           transitionToken("claim", request.ID, request.ClaimID, next.Version),
		ExpectedVersion: current.Version, ExpectedFence: current.ClaimFence,
		TransitionKind: "action.claimed", Record: next,
	})
	if err != nil {
		return ActionRecord{}, err
	}
	if err := s.publishTransition(
		context.WithoutCancel(ctx), "action.claimed", stored,
	); err != nil {
		return ActionRecord{}, errors.Join(ErrActionCommitted, err)
	}
	return cloneActionRecord(stored), nil
}

// applyClaim turns a record into a claimed one. It is the only place that
// transition is written.
//
// Both callers reach it: the dispatch Claim mutation and the admission grant,
// which is born claimed in a single write rather than queued and then claimed.
// They were separate once, and they drifted within one release — the egress
// correction below landed on Claim and not on admission, so an egress-only
// admission sat outstanding asserting that no effect was possible, which is the
// exact assertion the flag exists to avoid making. A matched pair of conditions
// is a promise someone has to keep; one function is a fact.
//
// The fence is incremented rather than assigned, so a re-claim advances it and
// a record that has never been claimed lands on one.
//
// It is also the one place a claim's attestation is judged (#446). Callers do
// the store read (DispatchService.claimAttestation) and pass the result in;
// the gate refuses unless it is current and expires at or after the lease end
// this claim would carry. ExtendClaim, which renews without going through
// here, applies the same attestationGate to its clamped extended end.
//
// ExecuteClaim needs no re-check, and must not grow one: a claim is granted
// only under an attestation that outlives its lease, so an attestation cannot
// expire while its claim is live, and ExecuteClaim refuses a claim whose lease
// has lapsed. A second gate there would be a second definition to drift.
//
// executorRef is the descriptor's executor reference as the claim's caller
// resolved it, and it becomes the claim's ClaimExecutorRef (#391).
func applyClaim(
	record ActionRecord,
	action Action,
	claimID []byte,
	lease time.Duration,
	decision auth.Decision,
	authorizing auth.Operation,
	now time.Time,
	attestation ExecutorAttestation,
	executorRef string,
) (ActionRecord, error) {
	leaseUntil := claimLeaseEnd(now, lease, record.Deadline)
	required := effectiveClaimRequirements(record, action)
	if err := approvalGate(required, record); err != nil {
		return ActionRecord{}, err
	}
	if err := attestationGate(required, attestation, leaseUntil); err != nil {
		return ActionRecord{}, err
	}
	// The incoming claimant's chain is bounded here, by the bound a *retained*
	// holder's chain is subject to, because this is where the asymmetry between
	// the two became a brick.
	//
	// ClaimantOnBehalfOf on the live record is bounded only by entry count
	// (auth.MaxOnBehalfOfEntries, each up to shoal.MaxIDBytes — 64 KB in the
	// worst case), while a retained ClaimHolder's chain is additionally bounded
	// at MaxClaimHolderChainBytes. A claimant whose chain fell between the two
	// therefore claimed successfully, and then the *next* claim produced a
	// record that ActionRecord.Validate refuses — so encodeAction refused the
	// write, and every subsequent claim by anyone was refused the same way,
	// forever, on an action stuck in DispatchClaimed with a dead lease.
	//
	// Verified by execution: a five-entry 5120-byte chain claims, lapses, and
	// then the next worker is refused "claim holder delegation chain exceeds
	// its byte bound" with the record still at the claimed version. That is
	// precisely the brick MaxClaimHolderChainBytes was added to prevent,
	// caused by MaxClaimHolderChainBytes.
	//
	// Refusing here rather than truncating the retained chain, because
	// heldClaimAt compares the retained chain element for element: a truncated
	// or omitted chain would silently deny the ambiguity route to exactly the
	// delegated worker whose claim was taken over, which is the one caller
	// that route exists for. A chain that cannot be retained is a chain whose
	// holder could never be recognised, so refusing the claim is the honest
	// answer and it arrives before any effect.
	//
	// The executor reference counts too, because the retained holder will
	// carry it (#391) and ClaimHolder.validate counts it.
	chainBytes := len(executorRef)
	for _, identity := range decision.OnBehalfOf() {
		chainBytes += len(identity)
	}
	if chainBytes > MaxClaimHolderChainBytes {
		return ActionRecord{}, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"claimant delegation chain exceeds its byte bound")
	}
	record.State = DispatchClaimed
	record.TransitionOperation = authorizing
	// An external-effect action is possibly-effected from the moment it is
	// claimed, not from the moment it is executed.
	//
	// An in-process executor sets this at execution because this process owns
	// the window between admission and effect. A remote worker owns that window
	// itself: once it holds the claim it may act at any time, and if it then
	// goes silent the record must not say the effect certainly did not happen.
	// Without this a lease that expires after the work was done is
	// indistinguishable from one that expired before it started.
	//
	// It is narrowed by the declaration rather than set for every claim. An
	// action that neither mutates externally nor transmits leaves its whole
	// outcome in Shoal's own record, so nothing has to be assumed about it.
	//
	// Egress counts. An earlier version excluded it on the grounds that
	// transmitting is a disclosure rather than an effect to reconcile against,
	// and that was wrong: a worker can transmit and then go silent, and a
	// record saying no effect was possible asserts the one thing nobody knows.
	// Content that left the host cannot be recalled, which makes an
	// unacknowledged possible egress exactly the kind of uncertainty this flag
	// exists to preserve.
	if action.Effects.contains(EffectMutatesExternal) ||
		action.Effects.contains(EffectEgressesContent) {
		record.EffectPossible = true
	}
	// Read before the assignment below, because the history retains the
	// *outgoing* holder. An earlier version of this block ran after
	// record.ClaimID had already been overwritten, so every retained holder
	// carried its successor's claim ID — no authorization consequence, since
	// heldClaimAt keys on the fence, but wrong in evidence an operator reads.
	outgoingClaimID := append([]byte(nil), record.ClaimID...)
	record.ClaimID = append([]byte(nil), claimID...)
	// The outgoing holder is retained before the incoming one overwrites it.
	//
	// Overwriting is correct for who may *complete* — only the current holder
	// may. It is wrong for who may *report an ambiguity*, because the caller
	// that route exists for is precisely the one whose claim was taken over:
	// its lease lapsed mid-effect and another worker now holds the record. The
	// record would otherwise retain no evidence it ever held anything (#438).
	//
	// Oldest first, oldest dropped on overflow. The most recent holders are
	// the ones whose effects may still be unreconciled, and the cap exists
	// because encodeAction refuses a record past 3*MaxActionPayloadBytes — an
	// unbounded history would make a repeatedly re-claimed action unwritable
	// rather than merely large.
	if record.ClaimantSubject != "" && record.ClaimFence > 0 {
		record.ClaimHistory = appendClaimHolder(record.ClaimHistory, ClaimHolder{
			Subject:    record.ClaimantSubject,
			Actor:      record.ClaimantActor,
			ClientID:   record.ClaimantClientID,
			OnBehalfOf: append([]shoal.ID(nil), record.ClaimantOnBehalfOf...),
			ClaimID:    outgoingClaimID,
			ClaimFence: record.ClaimFence,
			// When that holder took the claim, not when its successor did.
			// HeldAt was `now` — the incoming claimant's time — so every
			// retained holder's timestamp recorded the moment it was
			// *displaced*. The same category as the claim ID this block was
			// just fixed to read before overwriting: no authorization
			// consequence, because heldClaimAt keys on the fence, and wrong in
			// evidence an operator reads.
			HeldAt: record.ClaimLeaseUntil.Add(-record.ClaimLease),
			// The attestation that holder's claim stood on, so the history
			// says which statement covered each attempt.
			AttestationID: record.ClaimAttestationID,
			// The reference that holder claimed under. A displaced holder's
			// ambiguity report is judged against it (#391), so dropping it
			// here would leave that report judged as a legacy claim.
			ExecutorRef: record.ClaimExecutorRef,
		})
	}
	// Who holds the claim, as distinct from which claim is held. A re-claim
	// after a lapsed lease overwrites these, which is correct: the new
	// claimant is the one that may report, and the previous one has already
	// lost the fence.
	record.ClaimantSubject = decision.Subject()
	record.ClaimantActor = decision.Actor()
	record.ClaimantClientID = decision.ClientID()
	record.ClaimantOnBehalfOf = append(
		[]shoal.ID(nil), decision.OnBehalfOf()...)
	record.ClaimFence++
	record.ClaimLease = lease
	record.ClaimLeaseUntil = leaseUntil
	// Claim-scoped: a re-claim moves it, and a claim of an action that does
	// not require attestation carries none.
	record.ClaimAttestationID = ""
	if required.Attestation {
		record.ClaimAttestationID = attestation.ID
	}
	// Claim-scoped as well, and set on every claim whichever route admitted
	// it, so an empty value always means a claim from a build without the
	// field (#391).
	record.ClaimExecutorRef = executorRef
	record.UpdatedAt = now
	// Actor is deliberately not written here. It names the principal the record
	// belongs to, which sameActionPrincipal compares, and it is not a
	// per-transition field however much the two lines below look like company.
	//
	// This used to assign decision.Actor() and was a no-op: the claimant was by
	// construction the enqueuer, because claiming required the principal to
	// match. #437 made a third party able to claim, and the same line then
	// overwrote the record's principal — permanently, since the completion path
	// clones the record forward and never restores it. The enqueuer was then
	// refused Status and Cancel on its own in-flight action by the predicate
	// that had just been handed the claimant's identity, with no fallback:
	// TeamActions needs an operation fleet principals do not hold.
	//
	// So a claim records who holds the claim in ClaimID and the transition
	// provenance below, and leaves the record's own principal alone.
	record.TransitionRequestID = decision.RequestID()
	record.TransitionCorrelationID = decision.CorrelationID()
	// Widened with the operation that authorized *this* claim and nothing
	// else. The publisher needs it there — it authorizes a claim or
	// completion event against record.TransitionOperation and refuses a
	// publication whose operation is absent from this set, and a refused
	// publication comes back as ErrActionCommitted, so an execute-authorized
	// claim that did not widen the set would commit and then tell the worker
	// its outcome needs reconciliation.
	//
	// What it no longer adds is OperationDelegate. decisionOperations appends
	// that whenever the decision carries a non-empty OnBehalfOf, so a
	// delegated claimant permanently added delegate to the AuthorizedOperations
	// of a record it did not enqueue — asserting the enqueue was delegated on
	// the strength of who claimed it. Nothing reads delegate back out of this
	// field (the two authorizeScopes calls that name the operation check the
	// live decision, not the record), so it bought nothing and misdescribed
	// the record's own authority. Delegation of the enqueue is still recorded
	// at creation, where the delegating decision is the enqueuer's own
	// (#460).
	record.AuthorizedOperations = canonicalOperations(append(
		record.AuthorizedOperations, authorizing))
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		return ActionRecord{}, err
	}
	record.ExecutionFingerprint = fingerprint
	record.ExecutionPolicyGeneration = decision.PolicyGeneration()
	record.ExecutionExpiresAt = decision.AuthenticationExpires()
	return record, nil
}

func (s *DispatchService) ExecuteClaim(ctx context.Context, claimed ActionRecord) (ActionRecord, error) {
	now := s.clock().UTC()
	if claimed.State != DispatchClaimed || !now.Before(claimed.ClaimLeaseUntil) ||
		!now.Before(claimed.Deadline) {
		return ActionRecord{}, ErrClaimLost
	}
	decision, err := s.resolver.Resolve(ctx)
	if err != nil {
		return ActionRecord{}, err
	}
	if !sameActionPrincipal(decision, claimed) {
		return ActionRecord{}, shoal.NewError(shoal.ErrorUnauthorized, "execution identity does not match queued action")
	}
	// And the caller must hold the claim it is about to execute, not merely be
	// the record's principal.
	//
	// This is the same gate completeClaim applies, and it has to be here too
	// because Invoke reaches a terminal write without going through
	// completeClaim at all: it calls Claim, and on a record already claimed at
	// the presented ClaimID it calls straight through to here. So narrowing
	// only the completion path left the hole open through a second door, which
	// a review found by walking the entry points rather than the predicate.
	//
	// What it closed: an enqueuer reads the live ClaimID off Status — which
	// Status discloses to every co-principal — and Invokes with it while a
	// worker holds the claim. The in-process executor runs, the record goes
	// terminal, and the worker's own report is then recognised by the replay
	// branch and answered with a 200 carrying the enqueuer's output. A worker
	// that performed the work is told it was recorded, as something else.
	//
	// Verified by execution. An earlier version of this comment scoped it to
	// "only reachable where an in-process executor is bound" and said a
	// gateway reference is refused earlier for want of an ActionExecutor. Both
	// halves are wrong and the second is wrong structurally: the executor type
	// assertion lives in resolveAction, which this function reaches *after*
	// these gates, so nothing about an executor binding can refuse ahead of
	// them. And pkg/explorer/webapi.AskExecutor is a real in-process
	// ActionExecutor for explorer.reason/ask, so the hole was live in the
	// shipped configuration rather than confined to a hypothetical one.
	if !holdsClaimOn(decision, claimed) {
		return ActionRecord{}, shoal.NewError(
			shoal.ErrorUnauthorized, "execution identity does not hold the claim")
	}
	_, action, executor, err := s.registry.resolveAction(
		ctx, decision, claimed.AgentID, claimed.AgentGeneration,
		claimed.Capability, claimed.Action, claimed.SourceID, claimed.PolicyID,
		claimed.ObjectID, auth.OperationInvoke, now,
	)
	if err != nil {
		return ActionRecord{}, err
	}
	// Absence answers as a record the caller may not act on does, which is what
	// closes the enumeration on this path: the store's own sentinel for an
	// absent identity and object-not-found for a stored admission were two
	// answers to the question this surface exists to stop answering.
	//
	// There was a third refusal here, ahead of the read, for any identity in the
	// reserved span. It is gone. It inferred "an admission, or nothing" from the
	// identity, and the span held ordinary actions before it was reserved — so
	// it made a legitimately created action permanently unexecutable. It was
	// also redundant: a reserved identity that is absent is covered by the
	// normalisation below and one that is stored by the marker check, which is
	// why no mutation of it alone was ever observable.
	current, err := s.store.GetAction(ctx, claimed.ID)
	if err != nil {
		if errors.Is(err, ErrActionNotFound) {
			return ActionRecord{}, auth.ObjectNotFound()
		}
		return ActionRecord{}, err
	}
	storedDecision, err := s.resolver.Resolve(ctx)
	if err != nil {
		return ActionRecord{}, err
	}
	// The durable marker identifies admissions regardless of their identity
	// format, including admissions written before the reserved prefix existed.
	// This check rejects those records without treating an ordinary action in
	// the reserved span as an admission.
	//
	// Read from the stored record rather than the argument, so a caller that
	// fabricates a record with the marker stripped does not dodge it. That
	// matters here more than on the other paths: ExecuteClaim takes the record
	// instead of loading it from an ID, and a grant holder knows the ID, the
	// token, the version, the action fields and the deadline, and that a fresh
	// grant sits at fence one.
	if current.isAdmission() {
		return ActionRecord{}, auth.ObjectNotFound()
	}
	// Against the stored record, and above the replay branch below it.
	//
	// The gates at the top of this function compare against the argument, and
	// the argument is the caller's. A caller that writes its own principal
	// into both the action slots and the claimant slots of a fabricated record
	// satisfies them both, and the branch below then returns the *stored*
	// record — its input, its executor key, its claim ID and its claimant —
	// for any (ID, ClaimID, version, fence) tuple it can produce. Pull hands
	// every one of those to any execute-holder once a lease lapses.
	//
	// So the checks that matter are these, not those. The ones above stay as a
	// cheap early refusal on the normal path, where the argument is a record
	// this service just produced.
	//
	// Normalised to not-found rather than unauthorized, because distinguishing
	// them tells a caller that the action exists — the same three-way oracle
	// (absent / present with the wrong claim / present with the right one)
	// that the refusals in Claim were normalised to close.
	if !sameActionPrincipal(storedDecision, current) ||
		!holdsClaimOn(storedDecision, current) {
		return ActionRecord{}, auth.ObjectNotFound()
	}
	// Succeeded or failed only, matching completeClaim's replay branch and for
	// its reason: Cancel also lands on a terminal state at exactly version+1
	// while preserving the ClaimID it cancelled, so accepting any terminal
	// state would hand a late caller the cancelled record and a success.
	// Unreachable through Invoke today, since Cancel refuses a live claim and
	// this function requires one, but it is the same asymmetry and it should
	// not read differently in the two places.
	// Keyed on the fence, not the version. This branch still compared
	// version+1 after the first attempt at the stranding fix, so a report
	// landing after a committed execution turned a retry into ErrClaimLost
	// instead of returning the committed record — the same stranding, in the
	// one place the fix did not reach. ReportAmbiguity does not check state,
	// so a terminal record accepts one.
	if (current.State == DispatchSucceeded || current.State == DispatchFailed) &&
		current.ClaimFence == claimed.ClaimFence &&
		bytes.Equal(current.ClaimID, claimed.ClaimID) {
		if err := s.publishTransition(
			context.WithoutCancel(ctx), actionEventKind(current), current,
		); err != nil {
			return ActionRecord{}, errors.Join(ErrActionCommitted, err)
		}
		return cloneActionRecord(current), nil
	}
	// The fence, the claim, the state and the lease — not the version. This
	// caller holds a record it was handed; what must still be true is that
	// the claim generation is the same one. A report advances the version
	// while leaving all four alone, and so does a renewal, and neither
	// changes whose claim it is.
	if current.ClaimFence != claimed.ClaimFence ||
		!bytes.Equal(current.ClaimID, claimed.ClaimID) ||
		current.State != DispatchClaimed || !now.Before(current.ClaimLeaseUntil) {
		return ActionRecord{}, ErrClaimLost
	}
	// Empty for a record claimed by a build that had no TransitionOperation,
	// where the claimant was by construction the enqueuer and invoke is what
	// authorized it. ActionRecorder.RecordAction validates the operation
	// before anything else, so passing an empty one through would fail the
	// audit and surface as 503 — reachable across an upgrade through Invoke's
	// live-claim shortcut. The publisher has the same fallback for the same
	// reason; this was the one consumer missing it.
	admissionOperation := current.TransitionOperation
	if admissionOperation == "" {
		admissionOperation = auth.OperationInvoke
	}
	if err := s.recorder.RecordAction(ctx, ActionAudit{Phase: "effect_admission", Operation: admissionOperation, Record: current}); err != nil {
		return ActionRecord{}, errors.Join(ErrRecordingUnavailable, err)
	}
	invocationDecision, err := s.resolver.Resolve(ctx)
	if err != nil {
		return ActionRecord{}, err
	}
	if !sameActionPrincipal(invocationDecision, current) {
		return ActionRecord{}, shoal.NewError(shoal.ErrorUnauthorized, "invocation identity changed")
	}
	// Re-checked against the record as stored. The entry gate above compares
	// against a record the caller handed in, so on any path where that record
	// is not the one this service just loaded, this is the check that binds.
	//
	// Either one alone refuses the reachable cases, so a mutation deleting one
	// of them survives and only deleting both is caught — the same property
	// the sameActionPrincipal pair above it has. That is what defence in depth
	// looks like under mutation testing, and it is the reason to say so here:
	// neither is dead code, and the next reader should delete neither.
	if !holdsClaimOn(invocationDecision, current) {
		return ActionRecord{}, shoal.NewError(
			shoal.ErrorUnauthorized, "invocation identity does not hold the claim")
	}
	_, action, executor, err = s.registry.resolveAction(
		ctx, invocationDecision, current.AgentID, current.AgentGeneration,
		current.Capability, current.Action, current.SourceID, current.PolicyID,
		current.ObjectID, auth.OperationInvoke, s.clock().UTC(),
	)
	if err != nil {
		return ActionRecord{}, err
	}
	executionNow := s.clock().UTC()
	current, err = s.store.GetAction(ctx, claimed.ID)
	if err != nil {
		return ActionRecord{}, err
	}
	// Re-read immediately before the effect, on the same key as the gate
	// above it.
	if current.ClaimFence != claimed.ClaimFence ||
		!bytes.Equal(current.ClaimID, claimed.ClaimID) ||
		current.State != DispatchClaimed || !executionNow.Before(current.ClaimLeaseUntil) ||
		!executionNow.Before(current.Deadline) {
		return ActionRecord{}, ErrClaimLost
	}
	executionDeadline := current.ClaimLeaseUntil
	if current.Deadline.Before(executionDeadline) {
		executionDeadline = current.Deadline
	}
	executionContext, cancel := context.WithDeadline(ctx, executionDeadline)
	// Refused before the executor runs, because once it has retrieved there
	// is nothing to undo: the action's record would carry evidence anchors,
	// a snapshot pin and node identifiers from outside its own scope (#370).
	if err := refuseUnconfinedRetrieval(
		invocationDecision, executor, current, executionNow,
	); err != nil {
		cancel()
		return ActionRecord{}, err
	}
	var result ExecutionResult
	var executionErr error
	func() {
		defer cancel()
		result, executionErr = executor.Execute(executionContext, Invocation{
			ActionID: append([]byte(nil), current.ID...), IdempotencyKey: append([]byte(nil), current.ExecutorKey...),
			ClaimFence: current.ClaimFence, AgentID: current.AgentID, AgentGeneration: current.AgentGeneration,
			Capability: current.Capability, Action: current.Action, Input: append(json.RawMessage(nil), current.Input...),
			SourceID: append([]byte(nil), current.SourceID...), PolicyID: append([]byte(nil), current.PolicyID...),
			ObjectID: current.ObjectID,
			Subject:  current.Subject, Actor: current.Actor, ClientID: current.ClientID,
			OnBehalfOf: append([]shoal.ID(nil), current.OnBehalfOf...), RequestID: current.RequestID,
			CorrelationID: current.CorrelationID, Deadline: current.Deadline,
		})
	}()
	// Invoke, because this path requires its caller to be both the record's
	// own principal and the holder of its claim — sameActionPrincipal and
	// holdsClaimOn, both checked against the stored record above. A caller
	// satisfying both is the enqueuer that claimed its own action, and the
	// enqueuer claims under invoke.
	//
	// That also makes applyExecutionResult's write of this value provably
	// redundant here: cloneActionRecord carries the claim's operation forward,
	// and the only caller who can reach this claimed under invoke too. No
	// mutation of that assignment is observable, and it is kept because a
	// future completion route authorized differently would otherwise silently
	// record the claim's operation instead of its own.
	// This caller invoked the work, so the execution outcome is its own error
	// and it propagates. See applyExecutionResult's tail for why the other
	// caller does not.
	record, _, err := s.applyExecutionResult(
		ctx, current, action, result, auth.OperationInvoke, executionErr)
	return record, err
}

// CompleteClaim records the outcome of work a remote executor performed out of
// process.
//
// It exists because dispatch was one-directional. A worker outside this process
// could pull an action, claim it under a fence, perform the work, and then had
// nowhere to report: its lease expired and the action returned to the queue as
// though nothing had happened, while the effect had already occurred. The
// execution boundary (#381) refuses to run external work in process and points
// at dispatch as the alternative; this is the half of dispatch that makes the
// alternative reachable.
//
// Every check ExecuteClaim performs, this performs. The decision is resolved,
// the caller is matched to the claim's holder, the action is re-resolved
// through the registry against the current generation and lease, and the claim
// generation is confirmed before anything is written. The result then goes
// through applyExecutionResult, the same terminal transition the in-process
// path uses, which revalidates the fence after the fact and reports a loss as
// ambiguity rather than overwriting whatever committed in the meantime.
//
// The generation, not the version. This said "the claim fence and version are
// confirmed", which described the predicate before the fence bound it: when a
// fence is supplied the version is not compared at all, because #438's
// ambiguity route can advance it while leaving the claim intact and comparing
// it exactly is what stranded a live claimant. A caller that supplies no fence
// keeps the exact-version comparison, which is the contract it was written
// against.
//
// Reporting twice is safe. A worker that commits and then loses its response
// replays the request and gets the committed record back, exactly as a
// repeated ExecuteClaim does, because a terminal state under the reporter's own
// claim generation is recognised as its own work rather than a conflict.
//
// It refuses an admission, which the admission surface closes through its own
// path. That is not only about a reclaimed record: an admission token carries
// the action ID, the claim ID and the version, which is everything this
// request needs — so the caller that holds a live grant could always have
// completed it here instead of reporting, skipping the one-shot rule, the
// exact-replay comparison and the malformed-report rejection that the
// admission path exists to apply. The expiry the review traced is one way in;
// holding your own token is the other, and it needs no expiry at all.
func (s *DispatchService) CompleteClaim(ctx context.Context, request CompletionRequest) (ActionRecord, error) {
	return s.completeClaim(ctx, request, true)
}

// completeClaim is the shared body. refuseAdmissions is false only for the
// admission surface, which reaches it having already applied its own
// validation; splitting it that way keeps one completion implementation rather
// than a second one that could validate less.
func (s *DispatchService) completeClaim(
	ctx context.Context,
	request CompletionRequest,
	refuseAdmissions bool,
) (ActionRecord, error) {
	ctx, cancel := s.deadline(ctx, request.Context)
	defer cancel()
	decision, now, err := s.beginClaimant(ctx, request.Context)
	if err != nil {
		return ActionRecord{}, err
	}
	if err := validateOpaque("action ID", request.ID, false); err != nil {
		return ActionRecord{}, err
	}
	if err := validateOpaque("claim ID", request.ClaimID, false); err != nil {
		return ActionRecord{}, err
	}
	if request.ExpectedVersion == 0 {
		return ActionRecord{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "completion version is invalid")
	}
	// A failure with no reason is a protocol error. Recording it as a plain
	// failure would lose the only thing the report carried.
	if request.Failed && request.Result.ErrorCode == "" {
		return ActionRecord{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "a failed completion requires an error code")
	}
	// Refused here, at the boundary, rather than left to the validation
	// inside applyExecutionResult — which by then cannot tell a worker's code
	// from one the service has just assigned, because both sit in the same
	// field. This is the only place the code is known to have come from the
	// caller (#508).
	if err := validateReportedActionErrorCode(
		request.Result.ErrorCode); err != nil {
		return ActionRecord{}, err
	}
	current, action, _, authorizing, err := s.authorizedClaimant(
		ctx, decision, request.ID, now, executorPhaseComplete, 0)
	if err != nil {
		return ActionRecord{}, err
	}
	if refuseAdmissions && current.isAdmission() {
		return ActionRecord{}, auth.ObjectNotFound()
	}
	// Only the principal holding the claim may report on it, and this runs
	// before the replay branch rather than after.
	//
	// Ordering is the whole point. The replay branch exists so a worker whose
	// response was lost gets its own committed outcome back instead of a
	// conflict, and it keys on version and ClaimID. Placed after it, this
	// check would still refuse the write — but a hijacker's write would
	// already have landed, and the branch would then hand the real worker a
	// 200 carrying the hijacker's output, telling it the work it actually
	// performed was recorded when the record says something else.
	//
	// Concealed rather than refused, for the same reason every other refusal
	// on these routes is: this one is reachable by every principal authorized
	// to execute the descriptor, so a distinguishable error is the #398
	// oracle. An earlier version of this comment argued the opposite — that
	// the caller demonstrably knows the action exists because it presented a
	// version and a ClaimID — and the code three lines below it already did
	// the concealing. The argument was also wrong: presenting a guessed ID
	// with any version is exactly how the probe works.
	//
	// The record's own principal is accepted only where the claim predates
	// this field. That is narrower than it first shipped, and the narrowing
	// closes a hole rather than tidying one.
	//
	// The reason to accept it at all is backward compatibility. Claiming and
	// completing were both gated on sameActionPrincipal, so the claimant was
	// always the enqueuer and no record needed to say so; every record written
	// before ClaimantSubject existed therefore has an empty claimant chain and
	// a legitimate reporter that is the record's own principal. Requiring the
	// claimant alone would refuse all of them.
	//
	// The reason to accept it *only* then is that the enqueuer is otherwise
	// just another principal that is not the claimant. Two things an earlier
	// version of this comment asserted to excuse it are false, and both were
	// checked: the enqueuer cannot "cancel it at any moment", because Cancel
	// refuses while a claim is live and it must wait for the lease to lapse;
	// and fabricating its own action's outcome does not "harm only itself",
	// because the replay branch below then hands the worker that performed the
	// effect a 200 carrying the fabrication — the identical failure this gate
	// exists to prevent, with a narrower attacker.
	//
	// Nothing needs the wider form. An admission grant is claimed through
	// applyClaim like any other, so it carries a claimant chain, and
	// AdmissionService.Report already demands the record's own principal — so
	// for every admission this build writes, the claimant check alone passes.
	if !holdsClaimOn(decision, current) {
		return ActionRecord{}, auth.ObjectNotFound()
	}
	// A replayed report: the action is already terminal under this reporter's
	// own claim generation, so the work is committed and the response was
	// lost. Republish and return it rather than reporting a conflict against
	// the reporter's own write.
	//
	// Keyed on the generation, not the version. The sentence this replaces
	// said "terminal at the version this reporter expected to produce", which
	// was true of the old predicate and is false on the fence path, where the
	// version is not compared at all — two comments were left stacked here,
	// the first describing the code the second had replaced.
	//
	// Dropping the version without putting the fence in its place was the
	// second defect of the first attempt at this: terminal plus a matching
	// ClaimID is true of a *later* generation's completion, so a worker
	// reusing one ClaimID was handed attempt two's committed outcome as though
	// it were attempt one's. Verified by execution, and covered by nothing in
	// the repository.
	//
	// Only succeeded and failed count, because only applyExecutionResult
	// produces those and only it could have been this reporter's write. Cancel
	// also lands on a terminal state while preserving the ClaimID it
	// cancelled, so accepting any terminal state here would hand a late
	// reporter the cancelled record and a 200 — telling it the work it
	// performed was recorded, when the record says the opposite.
	if (current.State == DispatchSucceeded || current.State == DispatchFailed) &&
		bytes.Equal(current.ClaimID, request.ClaimID) &&
		replayMatchesGeneration(current, request) {
		if err := s.publishTransition(
			context.WithoutCancel(ctx), actionEventKind(current), current,
		); err != nil {
			return ActionRecord{}, errors.Join(ErrActionCommitted, err)
		}
		return cloneActionRecord(current), nil
	}
	// The claim generation, not the version.
	//
	// A completion's right to write this record comes from holding its claim:
	// the ClaimID matches, the fence identifies which claim, the state is
	// still Claimed, the lease is still live (checked below), the caller is
	// the claimant (checked above), and the store compare-and-sets on what it
	// just read.
	//
	// The version was standing in for the fence and doing it badly. It only
	// behaved like a generation check because nothing could advance the
	// version while leaving the claim intact — and #438's ambiguity route
	// does exactly that, which is how a lapsed holder's report came to strand
	// a live claimant with ErrClaimLost on work it had performed.
	//
	// The first attempt at this widened the version comparison to tolerate
	// drift that reports could account for. That was wrong in a way worth
	// recording: the budget was the record's *lifetime* report count while
	// the drift was measured from the caller's read, so reports filed before
	// the read bought slack without adding drift. A worker reusing one
	// ClaimID across two claim generations could then commit attempt one's
	// outcome onto attempt two's generation — an ABA break, verified by
	// execution, where the version was the only thing that had been binding
	// a completion to a generation and widening it removed that binding
	// altogether.
	//
	// So the fence is compared instead, and the version is not compared at
	// all when it is supplied. A caller that does not supply a fence keeps
	// the old exact-version behaviour, which is strandable but is the
	// contract it was written against.
	//
	// Two of the three conditions in the fence branch are defence in depth and
	// are deliberately uncovered: removing `current.State != DispatchClaimed`
	// or the ClaimID comparison each leaves the package green, because the
	// lease check below masks the first and holdsClaimOn above masks the
	// second. Neither is exploitable alone. They are kept because each masking
	// check is a separate decision that could move, and said here because the
	// convention in this file is to say which clauses no test can fail on.
	if request.ClaimFence != 0 {
		if current.ClaimFence != request.ClaimFence ||
			!bytes.Equal(current.ClaimID, request.ClaimID) ||
			current.State != DispatchClaimed {
			return ActionRecord{}, ErrClaimLost
		}
	} else if current.Version != request.ExpectedVersion ||
		!bytes.Equal(current.ClaimID, request.ClaimID) ||
		current.State != DispatchClaimed {
		return ActionRecord{}, ErrClaimLost
	}
	// The lease and the action deadline are both checked here, before anything
	// else is done. A worker reporting after either has passed has lost the
	// right to write this record: the action may already have been reclaimed
	// and run again.
	//
	// applyExecutionResult would catch this too, at the post-effect fence
	// check, but it would report it as ErrExecutionAmbiguous — which says the
	// service got far enough that it cannot tell whether the write landed.
	// Refusing here keeps a plainly-late report a plain ErrClaimLost, and
	// avoids resolving the registry and assembling a record for a report that
	// was never going to be written.
	//
	// The deadline half is currently implied by the lease half: Claim clamps
	// ClaimLeaseUntil to the action deadline, so a live lease always means a
	// live deadline and no test can distinguish the two clauses. It is kept
	// because it costs nothing and the clamp is maintained in a different
	// function, but it is not load-bearing today and should not be read as a
	// second, independent bound.
	if !now.Before(current.ClaimLeaseUntil) || !now.Before(current.Deadline) {
		return ActionRecord{}, ErrClaimLost
	}
	// The *claimant* is confirmed by holdsClaimOn above, which refuses a
	// mismatch as not-found rather than unauthorized so a caller cannot probe
	// for actions belonging to someone else.
	//
	// An earlier version of this said the queued principal is confirmed by
	// authorizedCurrent, and used that to justify omitting a check here. It is
	// false on the execute route: authorizedClaimant passes
	// requirePrincipal=false there, so for a foreign claimant the queued
	// principal is never confirmed at all. What is confirmed is that the
	// caller holds the claim, which is the right question for a completion and
	// is why the check it justified omitting is not needed — but the stated
	// reason was wrong. ExecuteClaim repeats its own checks because it is
	// handed a record instead of loading one; here it
	// would be dead code, and a check no test can distinguish implies a
	// guarantee that is not actually held at this point.
	//
	// Resolved for the Action, which carries the OutputSchema the report is
	// validated against. authorizedCurrent above already resolved it once and
	// refuses a descriptor revoked, re-registered or narrowed since the claim.
	// The declared schema comes back from it rather than from a second resolve
	// with a hardcoded operation, which would refuse a worker authorized under
	// execute.
	var executionErr error
	if request.Failed {
		executionErr = shoal.NewError(shoal.ErrorInternal, "remote executor reported failure")
	}
	record, committed, err := s.applyExecutionResult(
		ctx, current, action, request.Result, authorizing, executionErr)
	if committed {
		// The report was recorded, which is what this operation was asked to
		// do. Whether the work succeeded is in the record's State and
		// ErrorCode, where a worker already has to look to tell one from the
		// other — and where it reads its own rejection off the record rather
		// than inferring it from a status that also means "your request never
		// happened" (#492).
		return record, nil
	}
	return record, err
}

// applyExecutionResult is the terminal transition: it turns an ExecutionResult
// into a committed, audited, published ActionRecord.
//
// Both completion paths go through it — the in-process executor above and the
// remote worker in CompleteClaim — so neither can validate less than the other.
// Splitting this between them is how a remote worker ends up able to record
// output or evidence that an in-process one could not.
//
// The caller has already confirmed the claim is live. This re-confirms it
// anyway: everything after the effect races a lease that may have expired while
// the effect was happening, and the point of the fence is that losing it after
// the fact is reported rather than overwritten.
func (s *DispatchService) applyExecutionResult(
	ctx context.Context,
	current ActionRecord,
	action Action,
	result ExecutionResult,
	authorizing auth.Operation,
	executionErr error,
) (ActionRecord, bool, error) {
	finishNow := s.clock().UTC()
	next := cloneActionRecord(current)
	// The completion is a transition of its own and is authorized separately
	// from the claim, so it records its own operation rather than inheriting
	// the claim's.
	next.TransitionOperation = authorizing
	next.Version++
	next.UpdatedAt = finishNow
	// Carried forward, not asserted.
	//
	// This was `next.EffectPossible = true`, unconditional on every
	// completion — which overwrote the claim path's careful narrowing a few
	// hundred lines above. applyClaim sets the flag only for an action
	// declaring EffectMutatesExternal or EffectEgressesContent, with a
	// comment explaining that an action which "neither mutates externally nor
	// transmits leaves its whole outcome in Shoal's own record, so nothing
	// has to be assumed about it". Completion then assumed it anyway, so
	// every terminal record claimed an effect may have happened and the flag
	// carried no information where an operator actually reads it (#510).
	//
	// cloneActionRecord carries the claim's value, which is the right one:
	// true for a declaring action from the moment it was claimable, false for
	// one whose whole outcome is in this record. The store enforces that it
	// can only ever rise (#461), so a completion cannot withdraw it.
	//
	// What this does *not* give: a declaring action whose request demonstrably
	// never left still reads as possibly-effected, because the claim set it
	// and monotonicity forbids lowering it. That is correct — once claimed, a
	// worker may act at any time — and the only way to assert the negative is
	// an ambiguity report whose outcome is request_not_sent, which is a
	// statement by the worker rather than an inference by the service.
	// Whose code this is, decided by which branch below writes it rather than
	// by anything the caller said. A code present on entry came from the
	// executor; every assignment below is the service adjudicating, and each
	// one overwrites the origin with its own (#508).
	if result.ErrorCode != "" {
		next.ErrorCodeOrigin = ErrorCodeOriginExecutor
	}
	if executionErr == nil {
		output, validateErr := validateAgainstSchema(action.OutputSchema, result.Output, "action output", MaxActionOutputBytes)
		if validateErr != nil {
			executionErr = validateErr
			result.ErrorCode = "invalid_executor_output"
			next.ErrorCodeOrigin = ErrorCodeOriginService
		} else {
			next.Output = output
		}
	}
	if err := validateExecutionEvidence(
		result, finishNow, current.ExecutionExpiresAt,
	); err != nil {
		next.Evidence = nil
		next.EvidenceSnapshotID = ""
		next.EvidenceSnapshotAsOf = time.Time{}
		if executionErr == nil {
			executionErr = err
			result.ErrorCode = "invalid_executor_evidence"
			next.ErrorCodeOrigin = ErrorCodeOriginService
		}
	} else if joined, err := s.evidencePathsJoin(ctx, result); err != nil {
		return ActionRecord{}, false, err
	} else if !joined {
		next.Evidence = nil
		next.EvidenceSnapshotID = ""
		next.EvidenceSnapshotAsOf = time.Time{}
		if executionErr == nil {
			executionErr = shoal.NewError(shoal.ErrorInvalidArgument,
				"graph evidence edges do not join its nodes")
			result.ErrorCode = "invalid_executor_evidence"
			next.ErrorCodeOrigin = ErrorCodeOriginService
		}
	} else {
		evidence, err := s.structuredEvidence(ctx, result.Evidence)
		if err != nil {
			return ActionRecord{}, false, err
		}
		next.Evidence = evidence
		next.EvidenceSnapshotID = result.EvidenceSnapshotID
		next.EvidenceSnapshotAsOf = result.EvidenceSnapshotAsOf.UTC()
	}
	if err := validateActionErrorCode(result.ErrorCode); err != nil {
		executionErr = err
		result.ErrorCode = "invalid_executor_error"
		next.ErrorCodeOrigin = ErrorCodeOriginService
	}

	// How much left before the failure. Validated here so the dispatch
	// completion path cannot record what the admission path refuses, and
	// written only on a failure — a success's volume is not what this field
	// answers, and allowing it there would make it a second, unvalidated way
	// to describe a completed egress (#427).
	//
	// A refusal has to be adjudicated rather than returned, because by this
	// point the effect has happened: the service assigns its own code and
	// drops the unusable number, exactly as it does for an output or evidence
	// the schema refuses.
	if !result.Effected.Zero() {
		switch {
		case result.Effected.validate() != nil,
			!action.Effects.contains(EffectEgressesContent):
			if executionErr == nil {
				executionErr = shoal.NewError(
					shoal.ErrorInvalidArgument,
					"reported effected volume is not usable")
				result.ErrorCode = "invalid_executor_effected"
				next.ErrorCodeOrigin = ErrorCodeOriginService
			}
		default:
			next.Effected = result.Effected
		}
	}
	if executionErr == nil && result.ErrorCode == "" {
		// A success carries no volume, so a caller that reported one on work
		// that then succeeded does not get it recorded.
		next.Effected = EffectedVolume{}
		next.State = DispatchSucceeded
	} else {
		next.State = DispatchFailed
		next.ErrorCode = result.ErrorCode
		if next.ErrorCode == "" {
			next.ErrorCode = "executor_error"
			// The service's own, assigned because the failure arrived with no
			// reason at all.
			next.ErrorCodeOrigin = ErrorCodeOriginService
		}
	}
	latest, readErr := s.store.GetAction(ctx, current.ID)
	if readErr != nil || latest.Version != current.Version ||
		latest.ClaimFence != current.ClaimFence ||
		!bytes.Equal(latest.ClaimID, current.ClaimID) ||
		latest.State != DispatchClaimed ||
		!finishNow.Before(latest.ClaimLeaseUntil) ||
		!finishNow.Before(latest.Deadline) {
		if readErr != nil {
			return ActionRecord{}, false, errors.Join(ErrExecutionAmbiguous, readErr)
		}
		return ActionRecord{}, false, errors.Join(ErrExecutionAmbiguous, ErrClaimLost)
	}
	// A current decision and registry state are required again after the
	// effect. Failure is explicitly ambiguous; it is never described as a
	// rollback and the executor idempotency key remains stable for recovery.
	finalDecision, err := s.resolver.Resolve(ctx)
	if err != nil {
		return ActionRecord{}, false, errors.Join(ErrExecutionAmbiguous, err)
	}
	// Still authorized by either of the routes that let the caller take this
	// work, re-evaluated against a decision resolved after the effect.
	//
	// This was sameActionPrincipal plus a resolveActionBinding with a hardcoded
	// OperationInvoke, and it is the most expensive place that pairing could
	// have been wrong: it runs *after* the external effect, so an executor
	// refused here has performed the work and is then told its completion is
	// ambiguous. A worker authorized under execute would have failed every
	// completion at the last step, which looks exactly like the lease-loss case
	// the ambiguity is reserved for.
	//
	// claimableBy resolves the binding itself, so this is one check rather than
	// two and cannot disagree with the one Pull and Claim use.
	//
	// Under the completion rule, not the claim rule (#391): the record is
	// already claimed, so what the executor binding must match is the
	// reference it was claimed under. A rebind between the claim and this
	// point must not turn a recorded effect into an ambiguous one.
	stillClaimable, authorizeErr := s.claimableBy(
		ctx, finalDecision, current, finishNow, executorPhaseComplete)
	if authorizeErr != nil {
		return ActionRecord{}, false, errors.Join(ErrExecutionAmbiguous, authorizeErr)
	}
	if !stillClaimable {
		return ActionRecord{}, false, errors.Join(ErrExecutionAmbiguous,
			shoal.NewError(shoal.ErrorUnauthorized, "terminal execution identity changed"))
	}
	next.ExecutionFingerprint, err = auth.AuthorizationFingerprint(finalDecision)
	if err != nil {
		return ActionRecord{}, false, errors.Join(ErrExecutionAmbiguous, err)
	}
	next.ExecutionPolicyGeneration = finalDecision.PolicyGeneration()
	next.ExecutionExpiresAt = finalDecision.AuthenticationExpires()
	next.TransitionRequestID = finalDecision.RequestID()
	next.TransitionCorrelationID = finalDecision.CorrelationID()
	if err := s.recorder.RecordAction(ctx, ActionAudit{
		Phase: "effect_outcome", Operation: authorizing, Record: next, EffectError: executionErr,
	}); err != nil {
		return ActionRecord{}, false, errors.Join(ErrExecutionAmbiguous, ErrRecordingUnavailable, err)
	}
	stored, err := s.store.ApplyAction(ctx, DispatchMutation{
		Token:           transitionToken("complete", current.ID, current.ExecutorKey, next.Version),
		ExpectedVersion: current.Version, ExpectedFence: current.ClaimFence,
		TransitionKind: actionEventKind(next), Record: next,
	})
	if err != nil {
		return ActionRecord{}, false, errors.Join(ErrExecutionAmbiguous, err)
	}
	if err := s.publishTransition(
		context.WithoutCancel(ctx), actionEventKind(stored), stored,
	); err != nil {
		return ActionRecord{}, false, errors.Join(ErrActionCommitted, err)
	}
	// Committed. The second return says so, and it is the whole point of the
	// signature: executionErr here is the *outcome* of work that is now
	// durably recorded, not a reason the operation failed. Both are true at
	// once, and the two callers want different halves.
	//
	// ExecuteClaim invoked the work itself, so an execution failure is
	// genuinely its error and it propagates it. CompleteClaim only recorded
	// what a worker reported, and the reporting succeeded — so answering it
	// with an error told a worker that had just performed an irreversible
	// external effect that its report was refused, when the record had
	// landed. It could not tell "committed as failed" from "nothing
	// happened", which is the one ambiguity this whole design exists to
	// remove (#492).
	//
	// Returned as a bool rather than inferred from a non-zero record: every
	// refusal above returns the zero record, so a caller *could* key on that,
	// but then no test can tell a correct caller from one that forgot, and
	// the next return added above would silently join the committed set.
	return cloneActionRecord(stored), true, executionErr
}

func validateExecutionEvidence(
	result ExecutionResult, now, authorizationExpiry time.Time,
) error {
	if err := validateEvidence(result.Evidence); err != nil {
		return err
	}
	if len(result.Evidence) == 0 {
		if result.EvidenceSnapshotID != "" ||
			!result.EvidenceSnapshotAsOf.IsZero() {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"executor evidence snapshot requires evidence")
		}
		return nil
	}
	if err := shoal.ValidateRequiredID(
		"executor evidence snapshot ID", result.EvidenceSnapshotID); err != nil {
		return err
	}
	if result.EvidenceSnapshotAsOf.IsZero() {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"executor evidence snapshot time is required")
	}
	if now.Before(result.EvidenceSnapshotAsOf) ||
		authorizationExpiry.Before(result.EvidenceSnapshotAsOf) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"executor evidence snapshot time is outside execution bounds")
	}
	return nil
}

func (s *DispatchService) Cancel(ctx context.Context, request CancelRequest) (ActionRecord, error) {
	ctx, cancel := s.deadline(ctx, request.Context)
	defer cancel()
	decision, now, err := s.begin(ctx, auth.OperationDispatch, request.Context)
	if err != nil {
		return ActionRecord{}, err
	}
	if err := validateOpaque("cancel mutation key", request.MutationKey, false); err != nil {
		return ActionRecord{}, err
	}
	current, _, err := s.authorizedCurrent(ctx, decision, request.ID, auth.OperationDispatch, true, now)
	if err != nil {
		return ActionRecord{}, err
	}
	// Cancelling an admission here would rewrite an abandoned grant as a
	// refusal, and those are opposite statements: a cancelled admission says
	// Shoal refused, while an expired claimed one says Shoal permitted an effect
	// and never learned what happened. Refused as not-found for the same reason
	// Claim refuses it.
	if current.isAdmission() {
		return ActionRecord{}, auth.ObjectNotFound()
	}
	if current.Version != request.ExpectedVersion {
		if current.State == DispatchCanceled &&
			current.Version == request.ExpectedVersion+1 &&
			bytes.Equal(current.CancelKey, request.MutationKey) {
			if err := s.publishTransition(
				context.WithoutCancel(ctx), "action.canceled", current,
			); err != nil {
				return ActionRecord{}, errors.Join(ErrActionCommitted, err)
			}
			return cloneActionRecord(current), nil
		}
		return ActionRecord{}, ErrActionConflict
	}
	if current.State == DispatchClaimed && now.Before(current.ClaimLeaseUntil) {
		return ActionRecord{}, ErrActionConflict
	}
	if current.State.terminal() {
		return ActionRecord{}, ErrActionTerminal
	}
	next := cloneActionRecord(current)
	next.Version++
	next.State = DispatchCanceled
	// The cancellation's own operation, not the previous transition's.
	//
	// cloneActionRecord carries TransitionOperation forward, so a cancel of a
	// *claimed* record used to leave the claim's operation on it — invoke, or
	// execute since #437 — and a cancel of a queued record left it empty,
	// because nothing sets it at enqueue. Neither describes this transition.
	next.TransitionOperation = auth.OperationDispatch
	next.CancelKey = append([]byte(nil), request.MutationKey...)
	next.UpdatedAt = now
	next.TransitionRequestID = decision.RequestID()
	next.TransitionCorrelationID = decision.CorrelationID()
	// Left as decisionOperations, unlike the claim site. Cancel resolves
	// through authorizedCurrent with requirePrincipal true, so only the
	// record's own principal reaches here — a delegated canceller is
	// therefore the delegated enqueuer, whose delegation creation already
	// recorded. Narrowing this to dispatch alone would change nothing any
	// test can distinguish, which is the reason it is not narrowed: it would
	// read as a fix for a defect this path does not have (#460).
	next.AuthorizedOperations = canonicalOperations(append(
		next.AuthorizedOperations,
		decisionOperations(decision, auth.OperationDispatch)...,
	))
	next.CancelAuthorizationFingerprint, err =
		auth.AuthorizationFingerprint(decision)
	if err != nil {
		return ActionRecord{}, err
	}
	next.CancelAuthorizationExpiresAt = decision.AuthenticationExpires()
	if err := s.recorder.RecordAction(ctx, ActionAudit{Phase: "cancel_admission", Operation: auth.OperationDispatch, Record: next}); err != nil {
		return ActionRecord{}, errors.Join(ErrRecordingUnavailable, err)
	}
	stored, err := s.store.ApplyAction(ctx, DispatchMutation{
		Token: transitionToken(
			"cancel", request.ID, request.MutationKey, next.Version),
		ExpectedVersion: current.Version, ExpectedFence: current.ClaimFence,
		TransitionKind: "action.canceled", Record: next,
	})
	if err != nil {
		return ActionRecord{}, err
	}
	if err := s.publishTransition(
		context.WithoutCancel(ctx), "action.canceled", stored,
	); err != nil {
		return ActionRecord{}, errors.Join(ErrActionCommitted, err)
	}
	return cloneActionRecord(stored), nil
}

// ExtendClaim renews a live claim's lease so the fenced window can cover an
// operation longer than MaxActionClaimTTL (#430).
//
// Without it the fenced window is whatever was asked for at claim time,
// bounded by a hard five minutes that the service refuses rather than clamps.
// A worker whose operation outruns that has done something the record says did
// not happen, which is the outcome #391 exists to prevent — and raising the
// ceiling is not the fix, because a long lease is a long window in which a
// dead worker's action is stuck.
//
// Renewal separates the two concerns the single number conflated.
// MaxActionClaimTTL becomes a heartbeat interval: how long a worker may be
// silent before it is assumed gone. The action's Deadline remains the total
// budget, already set by the enqueuer, already bounded by MaxActionDeadline,
// and already clamping ClaimLeaseUntil. So the total bound needs no new
// concept and no new ceiling.
//
// Four semantics, each decided rather than discovered.
//
// The fence does not increment. ClaimFence identifies *which* claim, and the
// claim has not changed — only its deadline has. Incrementing would invalidate
// the fence the claimant is holding and break the report-under-the-same-fence
// contract the gateway design depends on. The record Version increments
// instead, and ExpectedVersion guards the mutation.
//
// An expired lease is not renewable, and that is the point rather than a
// limitation. Once ClaimLeaseUntil has passed the action may already have been
// reclaimed under a new ClaimID and fence, so renewing would hand two workers
// a live claim. A worker whose extension is refused mid-operation is in the
// ambiguous case — it may be about to perform, or have performed, an effect it
// can no longer report — which is what EffectPossible and the #438 ambiguity
// route exist for. The refusal is ErrClaimLost, distinguishable from a
// transport failure, because a worker must be able to tell "my claim is gone,
// treat this as ambiguous" from "retry the renewal".
//
// Shortening is refused. A lease moving ClaimLeaseUntil backwards is a worker
// bug, and honouring it costs the claim it was trying to keep.
//
// And no lifecycle event is published. A heartbeat on a long operation would
// emit one every few minutes per action and drown the event log in liveness;
// ClaimLeaseUntil and Version already carry it and Status already exposes
// them. TransitionKind is empty, so the store writes without enqueuing an
// outbox row — which also keeps a renewal out of the mixed-identity outbox
// that #480 found can be left undrainable.
func (s *DispatchService) ExtendClaim(
	ctx context.Context, request ExtendRequest,
) (ActionRecord, error) {
	ctx, cancel := s.deadline(ctx, request.Context)
	defer cancel()
	decision, now, err := s.beginClaimant(ctx, request.Context)
	if err != nil {
		return ActionRecord{}, err
	}
	// One of the two bindings is required. A fence-bound renewal needs no
	// version, and a caller that sends neither has pinned nothing.
	if (request.ExpectedVersion == 0 && request.ClaimFence == 0) ||
		request.Lease <= 0 || request.Lease > MaxActionClaimTTL {
		return ActionRecord{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "claim version or lease is invalid")
	}
	if err := validateOpaque(
		"claim ID", request.ClaimID, false); err != nil {
		return ActionRecord{}, err
	}
	current, claimedAction, executorRef, authorizing, err := s.authorizedClaimant(
		ctx, decision, request.ID, now, executorPhaseExtend, 0)
	if err != nil {
		return ActionRecord{}, err
	}
	// An admission's grant is not a claim a worker renews. Refused as
	// not-found for the same reason every other dispatch route refuses one.
	if current.isAdmission() {
		return ActionRecord{}, auth.ObjectNotFound()
	}
	// Only the claim's holder may renew it, and a caller without standing is
	// told what an absent action is told — this route is reachable by every
	// principal authorized to execute the descriptor, so a distinguishable
	// refusal would confirm the action exists.
	if !holdsClaimOn(decision, current) ||
		!bytes.Equal(current.ClaimID, request.ClaimID) {
		return ActionRecord{}, auth.ObjectNotFound()
	}
	// Fence-bound or version-bound, as CompleteClaim does it (the branch at
	// the fence comparison there). Unlike that one this arm compares only the
	// fence: ClaimID is already compared above and the state below, so
	// repeating either here would add a clause no test could fail on.
	//
	// ErrClaimLost, not ErrActionConflict: a fence mismatch means another
	// attempt holds the claim, which is the thing the holder needs to know.
	// The version arm keeps ErrActionConflict, because changing what an
	// existing caller is told is a separate decision from offering a new
	// binding.
	if request.ClaimFence != 0 {
		if current.ClaimFence != request.ClaimFence {
			return ActionRecord{}, ErrClaimLost
		}
	} else if current.Version != request.ExpectedVersion {
		return ActionRecord{}, ErrActionConflict
	}
	// Claimed and live, in that order. A terminal record has no claim to
	// renew; an expired one may already belong to someone else.
	if current.State != DispatchClaimed ||
		!now.Before(current.ClaimLeaseUntil) ||
		!now.Before(current.Deadline) {
		return ActionRecord{}, ErrClaimLost
	}
	extended := now.Add(request.Lease)
	if extended.After(current.Deadline) {
		extended = current.Deadline
	}
	// Refused rather than silently ignored, so a worker that has miscomputed
	// its own budget finds out while it still holds the claim.
	if !extended.After(current.ClaimLeaseUntil) {
		return ActionRecord{}, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"claim extension does not move the lease forward")
	}
	// The same gate applyClaim applies, against the *clamped* end the record
	// will carry: an extension is a grant of more lease, and a claim never
	// outlives its attestation. After every standing and state branch above,
	// so only the holder of a live claim learns of the requirement. A refusal
	// writes nothing, so the claim survives to its current lease end; the
	// worker re-attests and extends again.
	//
	// Stricter wins: a requirement registered while this claim is live
	// applies to its extension. The claim itself is not revoked — it runs to
	// its current lease end — but it is renewed only for an attested holder.
	required := effectiveClaimRequirements(current, claimedAction)
	if err := approvalGate(required, current); err != nil {
		return ActionRecord{}, err
	}
	attestation, err := s.claimAttestation(
		ctx, decision, required, executorRef, now)
	if err != nil {
		return ActionRecord{}, err
	}
	if err := attestationGate(required, attestation, extended); err != nil {
		s.auditAttestationRefusal(ctx, current, authorizing, decision)
		return ActionRecord{}, err
	}
	next := cloneActionRecord(current)
	next.Version++
	next.ClaimLease = request.Lease
	next.ClaimLeaseUntil = extended
	if required.Attestation {
		next.ClaimAttestationID = attestation.ID
	}
	next.UpdatedAt = now
	next.TransitionRequestID = decision.RequestID()
	next.TransitionCorrelationID = decision.CorrelationID()
	if err := s.recorder.RecordAction(ctx, ActionAudit{
		// The operation that authorized *this* call, for the reason
		// ReportAmbiguity's own comment gives: reading the record's field
		// attributes the renewal to whatever claimed the action, and passes an
		// empty operation for a record claimed by a build without the field,
		// which RecordAction validates first and refuses — joined with
		// ErrRecordingUnavailable, so a 503.
		//
		// This route had neither of the two defences its siblings have.
		// ReportAmbiguity uses the authorizing operation; ExecuteClaim keeps
		// the record's and falls back to invoke when it is empty. ExtendClaim
		// used the record's with no fallback, so a worker mid-long-operation
		// across an upgrade could not renew, lost its claim, and landed in the
		// ambiguity case — the exact outcome this route exists to prevent.
		// Reproduced before fixing: "fleet dispatch: recording is
		// unavailable", with errors.Is(err, ErrRecordingUnavailable) true.
		Phase: "claim_extension", Operation: authorizing,
		Record: next,
	}); err != nil {
		return ActionRecord{}, errors.Join(ErrRecordingUnavailable, err)
	}
	stored, err := s.store.ApplyAction(ctx, DispatchMutation{
		Token: transitionToken(
			"extend", request.ID, request.ClaimID, next.Version),
		ExpectedVersion: current.Version,
		// Asserted unchanged, not advanced. This is the invariant the whole
		// route turns on.
		ExpectedFence: current.ClaimFence,
		Record:        next,
	})
	if err != nil {
		return ActionRecord{}, err
	}
	return cloneActionRecord(stored), nil
}

// ReportAmbiguity records what a worker attempted when it can no longer report
// the outcome, which is the one case this dispatch surface could not express
// (#438).
//
// A worker whose lease lapses or whose renewal is refused mid-effect cannot use
// CompleteClaim: that route is gated on the fence it has just lost. The effect
// may have happened, and EffectPossible already says so durably — what was
// missing is *what* was attempted, against which target, and with what handle
// if the target returned one. That handle is the whole value here: it is the
// thing an operator takes to the other system, and it is what EffectPossible
// alone cannot give them.
//
// Three properties make this safe to expose to a caller that has lost its
// claim.
//
// It does not transition the action. The state, the claim and EffectPossible
// are all unchanged — a worker that has lost the fence has no standing to
// assert an outcome, only to say what it tried. The version advances because
// this writes to the record and must serialise against concurrent writers; the
// *fence* deliberately does not, so a report cannot invalidate a live
// claimant's right to complete.
//
// It publishes no event. TransitionKind is empty, so the store writes the
// record without enqueuing an outbox row. That is not merely economy: the five
// lifecycle kinds are closed and the public events route refuses them, and a
// row that only a lapsed claimant could publish is exactly the mixed-identity
// outbox that can be left undrainable by any principal (#480).
//
// And it is attributed rather than self-asserted. The reporting principal is
// recorded from its decision, never from the request, and a report is accepted
// only from a principal the record has actually seen hold the claim at the
// fence it names.
func (s *DispatchService) ReportAmbiguity(
	ctx context.Context, request AmbiguityRequest,
) (ActionRecord, error) {
	ctx, cancel := s.deadline(ctx, request.Context)
	defer cancel()
	decision, now, err := s.beginClaimant(ctx, request.Context)
	if err != nil {
		return ActionRecord{}, err
	}
	if err := request.Outcome.validate(); err != nil {
		return ActionRecord{}, err
	}
	if request.ClaimFence == 0 {
		return ActionRecord{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "ambiguity report fence is required")
	}
	// Bounds and shape are enforced here, at the boundary, rather than being
	// left to ActionRecord.Validate. Record validation runs where a record is
	// encoded, which is not every store, so relying on it let an over-long or
	// control-character-bearing target through in testing — the values are
	// target-controlled and this is the route they enter on, so this is where
	// they have to be refused.
	if err := validateAmbiguityText(
		"ambiguity target", request.Target); err != nil {
		return ActionRecord{}, err
	}
	if err := validateAmbiguityText(
		"ambiguity reference", request.Reference); err != nil {
		return ActionRecord{}, err
	}
	if len(request.Target) > MaxAmbiguityTargetBytes {
		return ActionRecord{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "ambiguity target exceeds its bound")
	}
	if len(request.Reference) > MaxAmbiguityReferenceBytes {
		return ActionRecord{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "ambiguity reference exceeds its bound")
	}
	current, _, _, authorizing, err := s.authorizedClaimant(
		ctx, decision, request.ID, now, executorPhaseAmbiguity,
		request.ClaimFence)
	if err != nil {
		return ActionRecord{}, err
	}
	// An admission is not dispatch work and its grant is reported through
	// AdmissionService.Report, which has its own terminal semantics. Refused
	// as not-found for the same reason every other route refuses it.
	if refuseAmbiguityAdmission(current) {
		return ActionRecord{}, auth.ObjectNotFound()
	}
	// Held the claim at the fence it names — currently, or according to the
	// retained history. Concealed rather than refused, because this route is
	// reachable by every principal authorized to execute the descriptor and a
	// distinguishable error would tell one of them that an action exists and
	// which fences it has been through.
	if !heldClaimAt(decision, current, request.ClaimFence) {
		return ActionRecord{}, auth.ObjectNotFound()
	}
	// Pinned only when the caller asked for it. See AmbiguityRequest's own
	// documentation: the caller this route exists for cannot learn the current
	// version, and version was never the invariant — the claim is, and the
	// store asserts that through ExpectedFence below.
	if request.ExpectedVersion != 0 &&
		current.Version != request.ExpectedVersion {
		return ActionRecord{}, ErrActionConflict
	}
	// An exact duplicate is a replay, not a new report.
	//
	// The append rule below is deliberate and stays: two reports from one
	// attempt mean the worker retried, and an operator wants both rather than
	// the later silently overwriting the earlier. But that reasoning is about
	// a retry carrying *different* information — a changed outcome, or a
	// reference the worker has since obtained. A report identical in every
	// respect to one already stored carries nothing beyond it.
	//
	// Treating it as a replay fixes an honest case and an adversarial one with
	// the same change. The honest case: a worker whose response was lost
	// retries, and the retry consumed budget for a report the record already
	// had — on a route whose whole purpose is recourse for a worker that
	// cannot get its outcome recorded. It now gets success and the record
	// back, which is what completeClaim's replay branch does for the same
	// situation.
	//
	// The adversarial case is #514's report-budget exhaustion, reproduced by
	// filing eight *identical* reports from a lapsed co-tenant until the
	// worker that performed the effect was refused. That reproduction no
	// longer works. This does not close #514: a co-tenant willing to vary its
	// reports still exhausts the budget, and the claim-history eviction half
	// is untouched. Eight distinct and plausible reports from one principal
	// are a much less comfortable thing to write off as organic, which is the
	// most this mitigation claims.
	//
	// ReportedAt is excluded from the comparison on purpose: it is set by the
	// service from its own clock, so a retry always differs there and
	// including it would make every duplicate look new.
	for _, existing := range current.AmbiguityReports {
		if existing.ClaimFence == request.ClaimFence &&
			existing.Outcome == request.Outcome &&
			existing.Target == request.Target &&
			existing.Reference == request.Reference &&
			existing.Subject == decision.Subject() &&
			existing.Actor == decision.Actor() {
			return cloneActionRecord(current), nil
		}
	}
	if len(current.AmbiguityReports) >= MaxActionAmbiguityReports {
		return ActionRecord{}, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"action ambiguity reports exceed their bound")
	}
	next := cloneActionRecord(current)
	next.Version++
	next.UpdatedAt = now
	next.TransitionRequestID = decision.RequestID()
	next.TransitionCorrelationID = decision.CorrelationID()
	// Appended, never replacing an existing report at the same fence. Two
	// reports from one attempt mean the worker retried, and an operator wants
	// both rather than the later one silently overwriting the earlier.
	next.AmbiguityReports = append(next.AmbiguityReports, AmbiguityReport{
		ClaimFence: request.ClaimFence,
		Outcome:    request.Outcome,
		Target:     request.Target,
		Reference:  request.Reference,
		Subject:    decision.Subject(),
		Actor:      decision.Actor(),
		ReportedAt: now,
	})
	// The operation that authorized *this* call, not the one the record's last
	// transition carried. A report is its own authorized act, and
	// authorizedClaimant has just told us which route admitted it — reading
	// the record's field instead would attribute the report to whatever
	// claimed the action, and would pass an empty operation for a record
	// claimed by a build without the field, which RecordAction validates
	// first and refuses as a 503.
	if err := s.recorder.RecordAction(ctx, ActionAudit{
		Phase: "ambiguity_report", Operation: authorizing,
		Record: next,
	}); err != nil {
		return ActionRecord{}, errors.Join(ErrRecordingUnavailable, err)
	}
	stored, err := s.store.ApplyAction(ctx, DispatchMutation{
		Token: transitionToken(
			"ambiguity", request.ID,
			ambiguityMutationKey(request, decision), next.Version),
		ExpectedVersion: current.Version,
		// The fence is asserted unchanged rather than advanced. A report must
		// not disturb whoever currently holds the claim.
		ExpectedFence: current.ClaimFence,
		Record:        next,
	})
	if err != nil {
		return ActionRecord{}, err
	}
	return cloneActionRecord(stored), nil
}

// replayMatchesGeneration reports whether a terminal record is the committed
// outcome of the claim generation this caller is reporting under.
//
// The fence answers it exactly when supplied. Without one, the only thing
// available is the pre-existing version arithmetic — a terminal record at
// exactly the version this caller expected to produce — which is why a caller
// that supplies no fence keeps that behaviour rather than a weaker one.
func replayMatchesGeneration(
	current ActionRecord, request CompletionRequest,
) bool {
	if request.ClaimFence != 0 {
		return current.ClaimFence == request.ClaimFence
	}
	return current.Version == request.ExpectedVersion+1
}

// refuseAmbiguityAdmission keeps the admission namespace out of this route.
func refuseAmbiguityAdmission(record ActionRecord) bool {
	return record.isAdmission()
}

// ambiguityMutationKey derives the store token's discriminator from
// everything that distinguishes one report from another.
//
// It is not a replay guard, and an earlier version of this comment claimed it
// was. A byte-identical retry does not reach the store at all: the service's
// own version handling answers first, so the token's stability is never what
// deduplicates a lost response. What the token must do is differ whenever two
// reports differ, so that transitionToken cannot fold two distinct writes into
// one.
//
// No test can currently observe that, and that is worth saying rather than
// contriving one. transitionToken folds in next.Version, which differs for
// every report because each advances the version — so collapsing this whole
// function to a constant leaves the suite green. The injectivity here is
// defence against a future caller that derives a token without the version,
// not something load-bearing today.
//
// Which also means the real protection is the version, and if that ever stops
// being part of the token this function becomes load-bearing immediately. It
// is written to be correct now so that it does not have to be discovered then.
//
// Every component is length-prefixed through writeDispatchTupleField rather
// than joined with a delimiter. A first version of this concatenated the
// fields with NUL bytes, which is precisely the encoding executorKey's own
// comment records as having been replaced for being non-injective: two
// different tuples can produce one key, and here that would make two genuinely
// different reports collide into one store token so the second would be
// silently dropped as a replay of the first.
func ambiguityMutationKey(
	request AmbiguityRequest, decision auth.Decision,
) []byte {
	digest := sha256.New()
	writeDispatchTupleField(digest, []byte("shoal.fleet.ambiguity-report.v1"))
	writeDispatchTupleField(digest, request.ID)
	writeDispatchTupleField(digest, []byte(decision.Subject()))
	writeDispatchTupleField(digest, []byte(decision.Actor()))
	writeDispatchTupleField(digest, []byte(decision.ClientID()))
	writeDispatchTupleField(
		digest, binary.BigEndian.AppendUint64(nil, request.ClaimFence))
	writeDispatchTupleField(digest, []byte(request.Outcome))
	// Target and Reference are part of what makes two reports different. An
	// earlier version omitted them, so two reports sharing the principal, the
	// fence and the outcome produced one key — the exact collision the
	// length-prefixing above was adopted to avoid. It was masked rather than
	// prevented, because transitionToken folds in the record version.
	writeDispatchTupleField(digest, []byte(request.Target))
	writeDispatchTupleField(digest, []byte(request.Reference))
	return digest.Sum(nil)
}

func (s *DispatchService) Status(ctx context.Context, request StatusRequest) (ActionRecord, error) {
	ctx, cancel := s.deadline(ctx, request.Context)
	defer cancel()
	decision, now, err := s.begin(ctx, auth.OperationDispatch, request.Context)
	if err != nil {
		return ActionRecord{}, err
	}
	// Status keeps the principal requirement, unchanged. #437 separated
	// execute from invoke for *claiming*; whether an executor should be able
	// to read the status of work it holds is a different question with a
	// different operation behind it, and bundling it here would widen dispatch
	// rather than answer it.
	//
	// The consequence is worth naming: a worker cannot read the status of its
	// own claim. It does not need to — Claim and CompleteClaim both return the
	// record — but an operator reconciling goes through TeamActions, which has
	// its own authorization and does not require the reader to be the
	// originating principal.
	current, _, err := s.authorizedCurrent(
		ctx, decision, request.ID, auth.OperationDispatch, true, now)
	if err != nil {
		return ActionRecord{}, err
	}
	// The fourth path, found by looking for a third. Status discloses nothing
	// across principals — authorizedCurrent has already refused a foreign
	// record — so this is not the same class of hole as the terminal paths.
	//
	// It is still the last place a dispatch call answered differently for an
	// admission, and that uniformity is worth having on its own: with this,
	// every dispatch entry point gives one answer for an admission-region ID
	// whether it is absent, another principal's, or the caller's own. An
	// admission's state is Outstanding's to report.
	if current.isAdmission() {
		return ActionRecord{}, auth.ObjectNotFound()
	}
	// Evidence the reader's labels do not cover is removed here rather than
	// in the handler, because the record is authorized on
	// (domain, source, policy, object) and the evidence it carries may come
	// from elsewhere entirely (#369).
	return s.readableRecord(ctx, current)
}

// TeamActions returns a bounded page of action records authorized for the
// read-only team overview. Unlike worker Pull, it includes terminal states and
// does not require the reader to be the action's originating principal.
func (s *DispatchService) TeamActions(
	ctx context.Context,
	request TeamActionListRequest,
) (ActionPage, error) {
	page, err := s.teamActionRecords(ctx, request)
	if err != nil {
		return ActionPage{}, err
	}
	return s.readablePage(ctx, page)
}

// teamActionRecords is TeamActions before evidence redaction.
func (s *DispatchService) teamActionRecords(
	ctx context.Context,
	request TeamActionListRequest,
) (ActionPage, error) {
	ctx, cancel := s.deadline(ctx, request.Context)
	defer cancel()
	now := s.clock().UTC()
	if err := request.Context.validate(now); err != nil {
		return ActionPage{}, err
	}
	decision, err := s.resolver.Resolve(ctx)
	if err != nil {
		return ActionPage{}, err
	}
	if decision.RequestID() != request.Context.RequestID ||
		decision.CorrelationID() != request.Context.CorrelationID {
		return ActionPage{}, shoal.NewError(
			shoal.ErrorUnauthorized,
			"request identity does not match authentication",
		)
	}
	if err := decision.Authorize(
		auth.OperationTeamOverviewRead,
		auth.ResourceRequest{
			AuthorizationDomain: decision.AuthorizationDomain(),
		},
		now,
	); err != nil {
		return ActionPage{}, err
	}
	if request.Limit <= 0 || request.Limit > MaxDispatchListResults {
		return ActionPage{}, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"team action list limit is outside its bound",
		)
	}
	if len(request.SourceIDs) == 0 || len(request.PolicyIDs) == 0 {
		return ActionPage{}, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"team action source and policy filters are required",
		)
	}
	sources, err := decision.IntersectSourceIDs(
		auth.OperationTeamOverviewRead, decision.AuthorizationDomain(),
		request.SourceIDs, now)
	if err != nil {
		return ActionPage{}, err
	}
	policies, err := decision.IntersectPolicyIDs(
		auth.OperationTeamOverviewRead, decision.AuthorizationDomain(),
		request.PolicyIDs, now)
	if err != nil {
		return ActionPage{}, err
	}
	if len(sources) == 0 || len(policies) == 0 {
		return ActionPage{}, nil
	}
	objects, err := canonicalOptionalIDs(
		"team action object", request.ObjectIDs)
	if err != nil {
		return ActionPage{}, err
	}
	agents, err := canonicalOptionalIDs(
		"team action agent", request.AgentIDs)
	if err != nil {
		return ActionPage{}, err
	}
	result := ActionPage{Actions: make([]ActionRecord, 0, request.Limit)}
	cursor := append([]byte(nil), request.After...)
	var continuation []byte
	const maxDiscoveryScans = 4096
	for scanned := 0; scanned < maxDiscoveryScans; scanned++ {
		// Admissions are excluded here, and by the scan rather than by a filter
		// in this loop. TeamActions deliberately does not require the reader to
		// be the action's principal — that is what makes it a team view — so an
		// admission reaching it is handed to another caller along with what its
		// owner declared, the digest of the references it named, and the bitmap
		// of the ones it was obliged to withhold.
		//
		// A filter here was the first attempt and it disabled this surface. It
		// skipped the cursor advance at the bottom of the loop, so the first
		// admission was rescanned four thousand times and nothing past it was
		// ever reached — and admission identities sorted first back then, so
		// that was every deployment with one grant. scanDispatchActions drops
		// them instead, where it cannot interact with this loop's control flow
		// at all.
		//
		// The budget below is affordable because the span is maximal, so this
		// loop reaches every ordinary record before the first admission and
		// spends nothing on them until there is nothing else left to find. The
		// exception is an ordinary action stored inside the span before it was
		// reserved, which sorts among the admissions; a deployment holding one
		// is exactly the deployment ErrAdmissionSpanOccupied is refusing
		// admission to until an operator clears it.
		page, scanErr := s.scanDispatchActions(ctx, cursor, 1)
		if scanErr != nil {
			return ActionPage{}, scanErr
		}
		// Advanced before anything can return or skip. It was at the bottom,
		// which is what let a `continue` strand it; a cursor that only moves on
		// the fall-through path is a cursor that stops moving the first time
		// someone adds an early exit.
		next := append([]byte(nil), page.Next...)
		cursor = next
		// An empty page is not an exhausted scan. scanDispatchActions can
		// filter a page down to nothing and still have more to read — two
		// consecutive admissions outside the reserved region spend both of its
		// attempts — and treating that as the end reported end-of-scan with
		// records still ahead. That was the same outage as the stranded cursor,
		// with a narrower trigger: two pre-prefix admissions rather than one
		// admission of any kind. Only an empty continuation means exhausted.
		if len(page.Actions) == 0 {
			if len(next) == 0 {
				return result, nil
			}
			continue
		}
		record := page.Actions[0]
		visible := containsByteValue(sources, record.SourceID) &&
			containsByteValue(policies, record.PolicyID) &&
			containsIDValue(objects, record.ObjectID) &&
			containsIDValue(agents, record.AgentID) &&
			decision.AuthorizeObject(
				auth.OperationTeamOverviewRead,
				auth.ResourceRequest{
					AuthorizationDomain: decision.AuthorizationDomain(),
					SourceID:            record.SourceID,
					PolicyID:            record.PolicyID,
					ObjectID:            record.ObjectID,
				},
				now,
			) == nil
		if visible {
			if len(result.Actions) == request.Limit {
				result.Next = continuation
				return result, nil
			}
			result.Actions = append(result.Actions, cloneActionRecord(record))
			continuation = append([]byte(nil), next...)
		}
		if len(next) == 0 {
			return result, nil
		}
	}
	result.Next = append([]byte(nil), cursor...)
	return result, nil
}

func canonicalOptionalIDs(name string, values []shoal.ID) ([]shoal.ID, error) {
	result := append([]shoal.ID(nil), values...)
	for _, value := range result {
		if err := shoal.ValidateRequiredID(name, value); err != nil {
			return nil, err
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return shoal.CompareID(result[i], result[j]) < 0
	})
	return slices.Compact(result), nil
}

func containsIDValue(values []shoal.ID, value shoal.ID) bool {
	if len(values) == 0 {
		return true
	}
	index := sort.Search(len(values), func(i int) bool {
		return shoal.CompareID(values[i], value) >= 0
	})
	return index < len(values) && values[index] == value
}

func containsByteValue(values [][]byte, value []byte) bool {
	if len(values) == 0 {
		return false
	}
	index := sort.Search(len(values), func(i int) bool {
		return bytes.Compare(values[i], value) >= 0
	})
	return index < len(values) && bytes.Equal(values[index], value)
}

// scanDispatchActions scans committed actions for a surface that must not see
// admissions, excluding them by their durable marker.
//
// It used to skip them by key range instead, jumping over the reserved span in
// one move. That was unsound for a reason no amount of care inside the jump
// could fix: skipping a range asserts that everything in it is something the
// caller must not see, and the span was a legal dispatch identity space before
// it was reserved — so an ordinary action already stored there was skipped,
// invisible to Pull and TeamActions with nothing to indicate it. Reachability
// is a property of what a record *is*, which only the marker records, not of
// where its identity happens to sort.
//
// The jump existed to keep a caller's scan budget off admissions, which sorted
// first. The budget concern goes with the jump because admissions now sort
// after every ordinary identity, and that holds exactly rather than nearly:
// admissionIDPrefix is maximal — every byte 0xff — so any identity outside the
// span differs from it at a byte that is necessarily smaller and sorts below
// the whole of it. A merely high prefix is not enough. When the span was
// "\xffshoal.admission\x00" an ordinary \xff\xff sorted above every
// admission, so a caller paging toward it spent the budget on grants first and
// a large enough tail of them starved it outright — the same outage the jump
// was covering for, reached from the other end.
//
// What remains is a caller paging to the very end, which filters through the
// span having already been given everything it asked for, and an ordinary
// action stored inside the span before it was reserved, which sorts among the
// admissions rather than below them. The second is the deployment
// ErrAdmissionSpanOccupied refuses admission to.
//
// A page can still filter down to nothing at that tail, and the cursor is
// returned so the caller pages on. An empty page is not an exhausted scan; only
// an empty continuation is.
func (s *DispatchService) scanDispatchActions(
	ctx context.Context, after []byte, limit int,
) (ActionPage, error) {
	page, err := s.store.ScanActions(ctx, after, limit)
	if err != nil {
		return ActionPage{}, err
	}
	result := ActionPage{
		Next:    append([]byte(nil), page.Next...),
		Actions: make([]ActionRecord, 0, len(page.Actions)),
	}
	for _, record := range page.Actions {
		if record.isAdmission() {
			continue
		}
		result.Actions = append(result.Actions, record)
	}
	// Not redacted here. TeamActions scans one record at a time, so redacting
	// per scan asked the label gate once per record, and for records the
	// caller would not even be shown (#564). Each page path redacts the page
	// it returns, once, through readablePage (#369): TeamActions, the widest
	// audience, and Pull, the path whose reader is least likely to hold the
	// labels. Both are pinned by tests that read labelled evidence through
	// them, so dropping either call fails.
	return result, nil
}

func (s *DispatchService) Pull(ctx context.Context, request PullActionsRequest) (ActionPage, error) {
	ctx, cancel := s.deadline(ctx, request.Context)
	defer cancel()
	decision, now, err := s.beginClaimant(ctx, request.Context)
	if err != nil {
		return ActionPage{}, err
	}
	if request.Limit <= 0 || request.Limit > MaxDispatchListResults {
		return ActionPage{}, shoal.NewError(shoal.ErrorInvalidArgument, "dispatch pull limit is outside its bound")
	}
	// An admission is never work a dispatch worker may take. It is only ever
	// claimed, and this loop returns a claimed record whose lease has lapsed —
	// which for an admission is not work waiting to be redone but a grant
	// nobody came back to report on. Claim refuses it anyway; dropping it in
	// the scan keeps it out of the listing that would otherwise offer it.
	//
	// This surface does not refill a page the filter emptied, so an admission
	// ahead of ordinary work would cost a worker an empty page and a retry per
	// grant. It cannot be ahead of any: the span is maximal, so every ordinary
	// identity sorts below every admission.
	page, err := s.scanDispatchActions(ctx, request.After, request.Limit)
	if err != nil {
		return ActionPage{}, err
	}
	result := ActionPage{Next: append([]byte(nil), page.Next...)}
	for _, record := range page.Actions {
		if record.State != DispatchQueued &&
			!(record.State == DispatchClaimed && !now.Before(record.ClaimLeaseUntil)) {
			continue
		}
		if !now.Before(record.Deadline) {
			continue
		}
		// The same two routes authorizedClaimant takes, in the same order and
		// for the same reason: an executor granted execute on this record's
		// descriptor may take work it did not enqueue, and the enqueuing
		// principal under invoke keeps working unchanged.
		//
		// Before #437 this filter required the caller to be the enqueuer, and
		// that is what made a remote worker see an empty page forever — the
		// records were skipped silently, so there was no error, no log line,
		// and nothing to diagnose.
		//
		// Binding, not execution: a worker has no in-process Execute, so
		// resolveActionBinding is the right resolver either way.
		claimable, authorizeErr := s.claimableBy(
			ctx, decision, record, now, executorPhasePull)
		if authorizeErr != nil {
			return ActionPage{}, authorizeErr
		}
		if !claimable {
			continue
		}
		result.Actions = append(result.Actions, cloneActionRecord(record))
	}
	return s.readablePage(ctx, result)
}

// ReconcileActionTransitions publishes and acknowledges every durable pending
// transition for one action. Callers may use a fresh authorized context after
// the original dispatch request has expired or after process restart.
func (s *DispatchService) ReconcileActionTransitions(
	ctx context.Context, actionID []byte,
) error {
	if err := validateOpaque("action ID", actionID, false); err != nil {
		return err
	}
	return s.reconcileActionTransitions(ctx, actionID, "", 0)
}

// reconcileActionTransitions drains the rows this caller may publish and
// leaves the rest pending.
//
// It used to return on the first publication refusal, and that is #480 item
// 3. The gates are per-kind — publisherMatchesTransition wants the enqueuer
// for action.enqueued and the claimant for action.claimed, and both the
// publisher's AuthorizeObject and fleetevents' own authorize run against the
// row's operation — so once an action has a stranded row from one principal
// and a fresh row from another, neither caller can drain the pair in one
// pass.
//
// The outbox is not actually stuck: the enqueuer drains the enqueued row and
// fails on the claimed one, then the claimant drains the claimed one, and
// nothing is left. #480 recorded this as "no principal can drain this
// action's outbox" because its repro stopped at the first failure. What was
// really wrong is that a reconciliation which is achievable looked
// impossible, and — worse — a worker's committed claim was answered
// ErrActionCommitted over somebody else's stranded row, telling a gateway
// that had just performed an irreversible external effect to reconcile an
// outcome that needed no reconciling.
//
// So a row that is not this caller's own and that it is not entitled to
// publish is not an error here. It belongs to another principal's authority
// and will drain when that principal next reconciles. The caller's own
// transition is still an error, which is what ErrActionCommitted is for:
// that one it does need to know about.
//
// ownKind and ownVersion name the caller's own transition, or are zero for a
// reconcile that is not publishing anything of its own (an operator draining
// an action, where every row belongs to someone else).
func (s *DispatchService) reconcileActionTransitions(
	ctx context.Context, actionID []byte, ownKind string, ownVersion uint64,
) error {
	var after []byte
	for {
		page, err := s.outbox.PendingActionTransitions(
			ctx, actionID, after, MaxDispatchListResults)
		if err != nil {
			return err
		}
		for _, transition := range page.Transitions {
			own := ownKind != "" &&
				transition.Kind == ownKind &&
				transition.Record.Version == ownVersion
			// Asked, not inferred. A row this caller may not publish is
			// skipped and stays pending, so the principal whose authority it
			// carries still delivers it — but the caller's own transition is
			// never skipped, because there is nobody else to drain that one
			// and a committed write whose event never published is exactly
			// what ErrActionCommitted reports.
			//
			// An error from the question is not a skip. Only a false answer
			// is.
			if !own {
				may, err := s.events.MayPublishActionEvent(
					ctx, transition.Kind, transition.Record)
				if err != nil {
					return err
				}
				if !may {
					// Counted, not logged: this is the ordinary case for a
					// co-tenant row and logging it per request would be noise
					// at exactly the rate it is useless. #642.
					s.skippedPublications.Add(1)
					continue
				}
			}
			if err := s.events.PublishActionEvent(
				ctx, transition.Kind, transition.Record); err != nil {
				return err
			}
			if err := s.outbox.CompleteActionTransition(
				ctx, transition); err != nil {
				return err
			}
		}
		if len(page.Next) == 0 {
			return nil
		}
		after = append(after[:0], page.Next...)
	}
}

func (s *DispatchService) publishTransition(
	ctx context.Context, kind string, record ActionRecord,
) error {
	if actionEventKind(record) != kind {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"fleet action transition kind does not match action state")
	}
	// The caller's own transition is named, so a refusal on *it* is still an
	// error while another principal's stranded row is not (#480 item 3).
	return s.reconcileActionTransitions(ctx, record.ID, kind, record.Version)
}

// beginClaimant admits a caller holding either of the operations that
// authorize taking work: execute, or invoke for the principal that enqueued.
//
// begin is a domain-level gate — it checks the operation against the
// authorization domain with no object in the request — so a worker granted only
// execute was refused here before any record was ever read. That made the
// record-level routes in authorizedClaimant unreachable, which is the shape of
// bug this whole change exists to remove: an authorization that fails at a door
// nobody was looking at.
//
// Execute is tried first and invoke second, the same order and for the same
// reason as authorizedClaimant: nothing that worked before stops working.
// Failing both returns the invoke error, which is exactly what a caller holding
// neither was told before this existed.
func (s *DispatchService) beginClaimant(
	ctx context.Context, request RequestContext,
) (auth.Decision, time.Time, error) {
	decision, now, err := s.begin(ctx, auth.OperationExecute, request)
	if err == nil {
		return decision, now, nil
	}
	if !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		return auth.Decision{}, time.Time{}, err
	}
	return s.begin(ctx, auth.OperationInvoke, request)
}

func (s *DispatchService) begin(ctx context.Context, operation auth.Operation, request RequestContext) (auth.Decision, time.Time, error) {
	now := s.clock().UTC()
	if err := request.validate(now); err != nil {
		return auth.Decision{}, time.Time{}, err
	}
	if err := shoal.ValidateRequiredID("dispatch correlation ID", request.CorrelationID); err != nil {
		return auth.Decision{}, time.Time{}, err
	}
	decision, err := s.resolver.Resolve(ctx)
	if err != nil {
		return auth.Decision{}, time.Time{}, err
	}
	if decision.RequestID() != request.RequestID || decision.CorrelationID() != request.CorrelationID {
		return auth.Decision{}, time.Time{}, shoal.NewError(shoal.ErrorUnauthorized, "request identity does not match authentication")
	}
	if err := decision.Authorize(operation, auth.ResourceRequest{AuthorizationDomain: decision.AuthorizationDomain()}, now); err != nil {
		return auth.Decision{}, time.Time{}, err
	}
	return decision, now, nil
}

func (s *DispatchService) deadline(ctx context.Context, request RequestContext) (context.Context, context.CancelFunc) {
	now := s.clock().UTC()
	if request.Deadline.IsZero() || !now.Before(request.Deadline) {
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		return canceled, func() {}
	}
	return context.WithTimeout(ctx, request.Deadline.Sub(now))
}

// authorizedClaimant resolves an action for a caller that means to take or
// finish the work, by either of the two routes that authorize it.
//
// An executor granted OperationExecute on the action's descriptor may claim
// work it did not enqueue. That is the whole point of #437: a gateway performs
// effects other agents asked for, and under one operation it could not — the
// claimant had to be the principal that enqueued, so every record failed
// sameActionPrincipal and was silently skipped, leaving a ready worker and a
// growing queue with no error anywhere.
//
// The enqueuing principal with OperationInvoke remains a second route, so a
// deployment that claims its own work keeps working without being regranted.
// That ordering is deliberate: the broader route is tried first, and the
// narrower principal-bound one only if it fails, so nothing that worked before
// stops working.
//
// Both failures normalise to the same auth.ObjectNotFound() that every other
// path through authorizedCurrent returns. Two routes must not become two
// distinguishable answers — the absent-versus-foreign normalisation is what
// stops this surface being an existence oracle (#398), and adding a second way
// in is exactly how that gets reopened.
//
// It also returns the descriptor's executor ref, which is what an attestation
// is keyed by.
//
// phase names the route calling, because the execute route is narrowed to the
// executor reference the caller is bound to and what must match differs by
// phase (executorBindingPermits). fence is read only for the ambiguity phase,
// where the reference that matters is the one the named claim was taken under.
func (s *DispatchService) authorizedClaimant(
	ctx context.Context, decision auth.Decision, id []byte, now time.Time,
	phase executorPhase, fence uint64,
) (ActionRecord, Action, string, auth.Operation, error) {
	record, action, ref, err := s.authorizedCurrentBinding(
		ctx, decision, id, auth.OperationExecute, false, now, phase, fence)
	if err == nil {
		return record, action, ref, auth.OperationExecute, nil
	}
	// Only an authorization answer is worth a second attempt. A malformed ID or
	// a store failure is the same answer either way, and retrying it would turn
	// one fault into two store reads.
	if !shoal.IsErrorCode(err, shoal.ErrorNotFound) &&
		!shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		return ActionRecord{}, Action{}, "", "", err
	}
	record, action, ref, err = s.authorizedCurrentBinding(
		ctx, decision, id, auth.OperationInvoke, true, now, phase, fence)
	return record, action, ref, auth.OperationInvoke, err
}

// authorizedCurrent resolves an existing action for one operation.
//
// requirePrincipal asks whether the caller must also be the principal that
// enqueued the action. It is true for every operation that existed before
// #437 — invoke and dispatch — and false only for OperationExecute, where the
// grant on the descriptor is the authorization and requiring the enqueuer's
// identity is what made an out-of-process executor impossible.
// authorizedCurrent resolves and authorizes an existing action for a route
// that is *not* a claimant route.
//
// It hardcodes executorPhaseNone, and that is the whole hazard: every
// toleration a claimant route needs is keyed on the phase. A route that
// reports on a claim already taken — a completion, an ambiguity report, an
// admission report — must call authorizedCurrentBinding with its own phase,
// or it silently receives the strict behaviour: a descriptor lease that
// lapsed or a reference rebound after the claim refuses the report, and the
// record of an effect that already happened is lost (#577).
//
// Silently is the operative word. There is no error and nothing to notice at
// review; the only symptom is a stranded effect in production. The admission
// report path reached for this wrapper for exactly that reason and was broken
// by it, which is why TestOnlyNonClaimantRoutesUseAuthorizedCurrent pins the
// callers rather than leaving this as advice.
func (s *DispatchService) authorizedCurrent(ctx context.Context, decision auth.Decision, id []byte, operation auth.Operation, requirePrincipal bool, now time.Time) (ActionRecord, Action, error) {
	record, action, _, err := s.authorizedCurrentBinding(
		ctx, decision, id, operation, requirePrincipal, now,
		executorPhaseNone, 0)
	return record, action, err
}

// authorizedCurrentBinding is authorizedCurrent that also returns the bound
// descriptor's executor ref.
//
// On the execute route it also applies the executor binding for phase
// (executeRoutePermits), and for a completion or an ambiguity report it
// tolerates a descriptor whose lease has lapsed since the claim. phase and
// fence are ignored on every other operation.
func (s *DispatchService) authorizedCurrentBinding(ctx context.Context, decision auth.Decision, id []byte, operation auth.Operation, requirePrincipal bool, now time.Time, phase executorPhase, fence uint64) (ActionRecord, Action, string, error) {
	if err := validateOpaque("action ID", id, false); err != nil {
		return ActionRecord{}, Action{}, "", err
	}
	current, err := s.store.GetAction(ctx, id)
	if err != nil {
		// Absence answers exactly as a record the caller may not see does.
		// These were different messages under one status: the store's own
		// sentinel for absent, and the object-not-found shape below for
		// foreign. That difference is #398, and it stops being a cosmetic
		// inconsistency once admission identities are derivable — a caller can
		// compute a victim's admission ID and read existence off the message,
		// which is the enumeration the reserved namespace closed at enqueue,
		// reopened through every ID-taking entry point.
		//
		// Normalised here rather than at each caller because every one of them
		// — Claim, Cancel, Status, CompleteClaim — reaches the store through
		// this function, and a per-caller fix is a fix that the next caller
		// forgets.
		if errors.Is(err, ErrActionNotFound) {
			return ActionRecord{}, Action{}, "", auth.ObjectNotFound()
		}
		return ActionRecord{}, Action{}, "", err
	}
	if requirePrincipal && !sameActionPrincipal(decision, current) {
		return ActionRecord{}, Action{}, "", auth.ObjectNotFound()
	}
	// Operation mapping is explicit: claim/complete/pull use execute and fall
	// back to invoke for the enqueuing principal, while cancel/status use
	// dispatch. Existing-action operations additionally bind
	// authorization to the durable action ID before checking its execution
	// target through registry.resolveAction.
	if err := decision.AuthorizeObject(operation, auth.ResourceRequest{
		AuthorizationDomain: decision.AuthorizationDomain(),
		SourceID:            current.SourceID,
		PolicyID:            current.PolicyID,
		ObjectID:            shoal.ID(current.ID),
	}, now); err != nil {
		return ActionRecord{}, Action{}, "", auth.ObjectNotFound()
	}
	// resolveActionBinding, not resolveAction: claiming, cancelling, inspecting
	// and completing an action must work for an executor that runs out of
	// process and has no in-process Execute. Only ExecuteClaim needs a runnable
	// one. Every other check — generation, scope, authorization, delegation and
	// the declared effect ceiling — is identical either way.
	// Resolved once and returned, rather than resolved here and re-resolved by
	// the caller. Claim and CompleteClaim each used to resolve again with a
	// hardcoded OperationInvoke under a comment saying it "adds no
	// authorization" — which was true while invoke was the only way in and
	// became false the moment execute existed: a worker authorized under
	// execute was refused by the second call. Handing back the declaration
	// removes the duplicate gate instead of teaching it the second operation.
	executeRoute := operation == auth.OperationExecute
	descriptor, resolved, _, err := s.registry.resolveActionBindingLapsing(ctx, decision, current.AgentID, current.AgentGeneration,
		current.Capability, current.Action, current.SourceID, current.PolicyID, current.ObjectID,
		operation, now,
		// Unpinned: this record already exists, so the question is
		// whether it is still the same agent, still live, and still
		// authorized for this scope and capability — none of which a
		// heartbeat changes. The requirement itself is checked by
		// approvalGate and attestationGate. See resolveActionBinding.
		false,
		// Not gated on the route either. #573 scoped this to the execute
		// route to stay contained, and the same argument applies to
		// invoke: synchronous Invoke and the admission report path both
		// complete through completeClaim under executorPhaseComplete, so
		// a gateway's report arriving after the descriptor's lease
		// lapsed lost the record of an effect that happened. A revoked
		// descriptor is still refused — activeChainLapsing checks
		// RevokedAt regardless of this flag.
		phase.reportsOnClaim(),
		// Not gated on the route: a rebind must not strand a completion
		// whichever operation it arrives under (#391 follow-up).
		phase.reportsOnClaim(),
	)
	if err != nil {
		if shoal.IsErrorCode(err, shoal.ErrorUnauthorized) ||
			shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			return ActionRecord{}, Action{}, "", auth.ObjectNotFound()
		}
		// Without the principal check, every failure here is one answer.
		//
		// resolveActionBinding returns a bare ErrorUnavailable when the
		// executor is unregistered or the effect ceiling has narrowed, and that
		// surfaces as a 503 where an absent record is a 404. For the enqueuing
		// principal that distinction is information it already has. For a
		// caller reaching a record on the strength of an execute grant it is an
		// existence oracle, and it reaches the admission namespace — those IDs
		// are computable from a principal tuple, and Claim's isAdmission
		// refusal runs after this, so the 503 would arrive first.
		//
		// That is #398 reopened through a route that did not exist when the
		// normalisation was written, which is why it is collapsed here rather
		// than at the one caller that noticed.
		if !requirePrincipal {
			return ActionRecord{}, Action{}, "", auth.ObjectNotFound()
		}
		return ActionRecord{}, Action{}, "", err
	}
	// The executor binding, on the execute route only (#391). Applied after
	// the resolution because it needs the descriptor's reference as it is
	// now, and before every handler branch, so a caller bound to another
	// reference is told exactly what an absent action is told and never
	// reaches the attestation store.
	if executeRoute && !executeRoutePermits(phase, decision,
		descriptor.ExecutorRef, claimedExecutorRef(current, phase, fence)) {
		return ActionRecord{}, Action{}, "", auth.ObjectNotFound()
	}
	return cloneActionRecord(current), resolved, descriptor.ExecutorRef, nil
}

// executorPhase names which execute-route operation is asking, because the
// executor binding asks a different question of each (#391).
type executorPhase uint8

const (
	// executorPhaseNone is every route that is not a claimant route. The
	// binding permits nothing under it, so a future execute-route caller that
	// forgets to name its phase is refused rather than admitted.
	executorPhaseNone executorPhase = iota
	executorPhasePull
	executorPhaseClaim
	executorPhaseExtend
	executorPhaseComplete
	executorPhaseAmbiguity
	// executorPhaseTerminalReplay is a caller re-sending a request whose
	// action is already terminal, and being handed the record back. It takes
	// no new work and renews nothing, so it belongs with Complete and
	// ReportAmbiguity: refusing it after a lapse strands the effect at the
	// caller rather than in the record (#578).
	executorPhaseTerminalReplay
)

// reportsOnClaim reports whether the phase reports on a claim already taken
// rather than taking or renewing one. Those are judged against the reference
// the claim was taken under, and tolerate a descriptor lease that lapsed after
// the claim: a lapse never strands the report of an effect that happened.
func (p executorPhase) reportsOnClaim() bool {
	return p == executorPhaseComplete ||
		p == executorPhaseAmbiguity ||
		p == executorPhaseTerminalReplay
}

// executorBindingPermits is the executor binding rule (#391), pure so each
// row can be tested and mutated on its own.
//
// binding is the caller's Decision.ExecutorBinding, currentRef the
// descriptor's executor reference now, and claimedRef the reference the claim
// in question was taken under.
//
//   - An empty binding permits nothing. A decision may hold execute without a
//     binding, and before #391 that let it take any descriptor's work in its
//     scope; it now claims nothing on this route.
//   - Pull and Claim take new work, so they need the current reference.
//   - Extend needs the current reference and the claimed one. After a rebind
//     it is refused, so the claim runs to its lease end and no further.
//   - Complete and ReportAmbiguity need the claimed reference only, so a
//     rebind never stops a worker reporting an effect that happened. An empty
//     claimed reference is a claim taken before the field existed and is
//     accepted as legacy here, and only here: Extend refuses it.
func executorBindingPermits(
	phase executorPhase, binding, currentRef, claimedRef string,
) bool {
	if binding == "" {
		return false
	}
	switch phase {
	case executorPhasePull, executorPhaseClaim:
		return currentRef == binding
	case executorPhaseExtend:
		return currentRef == binding && claimedRef == binding
	case executorPhaseComplete, executorPhaseAmbiguity,
		executorPhaseTerminalReplay:
		// Handled explicitly rather than falling to default. default refuses,
		// which would be fail-closed but silent, and a terminal replay is
		// judged against the claimed reference for the same reason a
		// completion is.
		return claimedRef == "" || claimedRef == binding
	default:
		return false
	}
}

// executeRoutePermits applies executorBindingPermits to a decision, and
// refuses a delegated caller taking new work.
//
// auth already refuses a binding together with an on-behalf-of chain, so a
// delegated decision has no binding and the rule above refuses it anyway. The
// fleet refuses it here as well, at Pull and Claim, because a delegated
// execute claim is what would make #546's subject collision a claim takeover:
// heldClaimAt compares chains element by element. A worker acts as itself.
//
// No test can fail on the chain clause alone, and that is stated rather than
// hidden: auth cannot mint a bound decision with a chain, so every delegated
// caller also has an empty binding and is refused by the rule above.
// Removing both is caught (TestADelegatedExecuteHolderIsRefusedAtPullAndClaim);
// the clause is kept so the refusal does not rest on auth's validation alone.
func executeRoutePermits(
	phase executorPhase, decision auth.Decision, currentRef, claimedRef string,
) bool {
	if (phase == executorPhasePull || phase == executorPhaseClaim) &&
		len(decision.OnBehalfOf()) > 0 {
		return false
	}
	return executorBindingPermits(
		phase, decision.ExecutorBinding(), currentRef, claimedRef)
}

// claimedExecutorRef is the reference the claim a phase is about was taken
// under: the current claim's, or for an ambiguity report the claim at the
// fence it names, which may be a displaced holder's. A fence the record has
// not seen yields empty, and heldClaimAt refuses that report regardless.
func claimedExecutorRef(
	record ActionRecord, phase executorPhase, fence uint64,
) string {
	if phase != executorPhaseAmbiguity || fence == record.ClaimFence {
		return record.ClaimExecutorRef
	}
	for _, holder := range record.ClaimHistory {
		if holder.ClaimFence == fence {
			return holder.ExecutorRef
		}
	}
	return ""
}

// claimableBy reports whether this caller may take this record, by either
// route, and separates "not for you" from a fault.
//
// That separation is why this is a function rather than two inline conditions.
// An authorization answer is a skip: the page continues and the caller simply
// does not see the record. A fault on the caller's own record aborts the page,
// because telling a caller its queue is empty is a worse answer than telling
// it the lookup failed.
//
// The bound on how much that buys is worth stating, because an earlier version
// of this comment claimed more. resolveActionBinding collapses every error
// from Service.active into auth.ObjectNotFound(), and active propagates real
// store faults up the delegation chain — so a descriptor-store outage already
// reads here as "no such record" and already produces a silent empty page.
// That is pre-existing behaviour and this function does not change it. What it
// does is stop a *foreign* record's fault from aborting a page it has no
// business aborting, which is a narrower and real guarantee.
//
// phase is Pull's or a completion's, and is applied on the execute route as
// authorizedClaimant applies it (#391).
func (s *DispatchService) claimableBy(
	ctx context.Context, decision auth.Decision, record ActionRecord,
	now time.Time, phase executorPhase,
) (bool, error) {
	for _, route := range []struct {
		operation        auth.Operation
		requirePrincipal bool
	}{
		{auth.OperationExecute, false},
		{auth.OperationInvoke, true},
	} {
		if route.requirePrincipal && !sameActionPrincipal(decision, record) {
			continue
		}
		executeRoute := route.operation == auth.OperationExecute
		descriptor, resolved, _, err := s.registry.resolveActionBindingLapsing(
			ctx, decision, record.AgentID, record.AgentGeneration,
			record.Capability, record.Action, record.SourceID, record.PolicyID,
			record.ObjectID, route.operation, now,
			// Unpinned: this record already exists, so the question is
			// whether it is still the same agent, still live, and still
			// authorized for this scope and capability — none of which a
			// heartbeat changes. The requirement itself is checked by
			// approvalGate and attestationGate. See resolveActionBinding.
			false,
			// Not gated on the route either. #573 scoped this to the execute
			// route to stay contained, and the same argument applies to
			// invoke: synchronous Invoke and the admission report path both
			// complete through completeClaim under executorPhaseComplete, so
			// a gateway's report arriving after the descriptor's lease
			// lapsed lost the record of an effect that happened. A revoked
			// descriptor is still refused — activeChainLapsing checks
			// RevokedAt regardless of this flag.
			phase.reportsOnClaim(),
			// Also here, and not false: claimableBy is on the completion
			// path too — applyExecutionResult re-confirms the claim through
			// it after the effect — so leaving it false would fire the
			// ceiling re-check at completion by this route and strand the
			// effect anyway, with the other call site fixed.
			phase.reportsOnClaim(),
		)
		if err == nil && executeRoute && !executeRoutePermits(
			phase, decision, descriptor.ExecutorRef,
			claimedExecutorRef(record, phase, 0)) {
			// Not this worker's reference: a skip, exactly as an
			// authorization refusal is.
			continue
		}
		if err == nil {
			// Offering work that cannot be claimed is a listing that lies.
			// The claim gates are the authority on this; applying the same
			// requirement here keeps Pull from handing a worker a record its
			// very next call would refuse.
			//
			// The resolved Action is the *current* registration, which is why
			// this needs no extra read: resolveActionBinding has just read the
			// descriptor as it is now.
			if approvalGate(
				effectiveClaimRequirements(record, resolved), record,
			) != nil {
				return false, nil
			}
			return true, nil
		}
		if shoal.IsErrorCode(err, shoal.ErrorUnauthorized) ||
			shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			continue
		}
		// A real fault, and whose record it is decides whether the caller is
		// entitled to hear about it.
		//
		// On the enqueuer's own record it aborts, as it always has: a registry
		// that cannot answer is not the same as a record that is not yours, and
		// swallowing it would turn an outage into an empty queue.
		//
		// On somebody else's it is a skip. Before #437 the principal check ran
		// first and such a record never reached this call, so one foreign
		// action with an unregistered executor could not affect your page. With
		// two routes it would abort the page of every execute-holder in the
		// scope, on every pull, until that record expired — a shared-queue
		// outage any principal able to enqueue could plant.
		if sameActionPrincipal(decision, record) {
			return false, err
		}
	}
	return false, nil
}

// observableThroughPull reports whether Pull's *state* filter would return
// this record.
//
// It mirrors that filter and the two must not drift. It is not the whole of
// Pull's selection: Pull also drops admission records and applies claimableBy
// per record, and this takes no decision so it structurally cannot express the
// second. An earlier version of this comment said "mirrors the filter in Pull
// exactly", which invited a future caller where the difference would matter.
//
// Neither omission is reachable from the callers it has. Claim refuses an
// admission with auth.ObjectNotFound() before any branch that consults this,
// and by the time one is reached authorizedClaimant has already succeeded —
// which is strictly stronger than claimableBy, since it resolves the same
// binding and additionally authorizes the record's own ID. A caller admitted
// through the invoke fallback is the enqueuer, so standingOn holds regardless.
//
// It is the discriminator for whether concealing a refusal buys anything. A
// record Pull already hands this caller cannot be concealed by any answer
// Claim gives, so refusing with the real error discloses nothing and is far
// more useful: the loser of an ordinary claim race needs a conflict, not a
// not-found for work it just saw in its own page. A record Pull withholds is
// the opposite — there, the error is the only channel, so it has to be the
// same error an absent action produces.
//
// Note which states those are, because an earlier version of this reasoning
// got it backwards and said a live-claimed record was already visible through
// Pull. It is not: Pull returns a claimed record only once its lease has
// lapsed. So live-claimed is withheld, and claiming one had been answering
// with a distinguishable ErrActionConflict.
func observableThroughPull(record ActionRecord, now time.Time) bool {
	if record.State != DispatchQueued &&
		!(record.State == DispatchClaimed && !now.Before(record.ClaimLeaseUntil)) {
		return false
	}
	return now.Before(record.Deadline)
}

// concealFrom reports whether a refusal about this record must be replaced with
// the answer an absent action gets, rather than the real reason.
func concealFrom(
	decision auth.Decision, record ActionRecord, now time.Time,
) bool {
	return !standingOn(decision, record) &&
		!observableThroughPull(record, now)
}

// holdsClaimOn reports whether this caller may act as the holder of this
// record's claim — which is what both writing a terminal outcome and replaying
// a claim amount to.
//
// Narrower than standingOn on purpose. standingOn answers "may this caller be
// told the record exists", where the enqueuer always qualifies because it is
// the record's own principal. Reporting is a different question: it is a claim
// to have performed the work, and the only principal that can have done so is
// the one holding the claim.
//
// The enqueuer is admitted only when the record carries no claimant at all.
// Nothing this build writes produces that: applyClaim is the only writer of
// those fields and auth.NewDecision requires a non-empty Subject. So in
// practice it means the claim was taken by a build that had no such field,
// where the claimant was by construction the enqueuer. It is not an invariant
// the type enforces — ActionRecord.Validate accepts an empty claimant on a
// claimed record, deliberately, so a stored record from that build still
// decodes and still validates.
func holdsClaimOn(decision auth.Decision, record ActionRecord) bool {
	if sameClaimantPrincipal(decision, record) {
		return true
	}
	return record.ClaimantSubject == "" && sameActionPrincipal(decision, record)
}

// standingOn reports whether this caller has any standing to be told that this
// record exists: it is either the principal that enqueued it or the principal
// that holds its claim.
//
// Before #437 the question could not arise. Every route resolved through
// authorizedCurrent, which required sameActionPrincipal unconditionally, so a
// caller with no standing was already refused with auth.ObjectNotFound() and
// could not reach a handler at all. The execute routes removed that gate by
// design — a worker must be able to claim work it did not enqueue — and the
// handlers behind them answer with ErrActionTerminal, ErrActionConflict and
// ErrClaimLost, none of which an absent action produces.
//
// That is the #398 existence oracle, reopened through routes that did not
// exist when the normalisation was written. Action IDs are caller-chosen
// opaque bytes, so they are guessable, and the records that leak are exactly
// the ones Pull does not show: terminal, and past their deadline.
func standingOn(decision auth.Decision, record ActionRecord) bool {
	return sameActionPrincipal(decision, record) ||
		sameClaimantPrincipal(decision, record)
}

// appendClaimHolder adds a holder, dropping the oldest entry past the bound.
//
// A holder already present at the same fence is not duplicated: Claim's replay
// branch returns before applyClaim, so this should not arise, but a retained
// history is evidence an operator reads and a duplicate in it reads as two
// attempts where there was one.
func appendClaimHolder(
	history []ClaimHolder, holder ClaimHolder,
) []ClaimHolder {
	for _, existing := range history {
		if existing.ClaimFence == holder.ClaimFence &&
			existing.Subject == holder.Subject {
			return history
		}
	}
	history = append(history, holder)
	if len(history) > MaxActionClaimHistory {
		history = history[len(history)-MaxActionClaimHistory:]
	}
	return history
}

// heldClaimAt reports whether this caller held the record's claim at the fence
// it names.
//
// The current holder is included, because a worker can lose the right to
// complete without the record being re-claimed — its lease lapses and nothing
// has taken it yet, which is the common case for a short operation that
// overran. Both are "held it then, cannot complete now".
func heldClaimAt(
	decision auth.Decision, record ActionRecord, fence uint64,
) bool {
	if fence == 0 {
		return false
	}
	if record.ClaimFence == fence && sameClaimantPrincipal(decision, record) {
		return true
	}
	for _, holder := range record.ClaimHistory {
		if holder.ClaimFence != fence {
			continue
		}
		if decision.Subject() != holder.Subject ||
			decision.Actor() != holder.Actor ||
			decision.ClientID() != holder.ClientID {
			continue
		}
		left, right := decision.OnBehalfOf(), holder.OnBehalfOf
		if len(left) != len(right) {
			continue
		}
		matched := true
		for i := range left {
			if left[i] != right[i] {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

// sameClaimantPrincipal reports whether the caller is the principal that holds
// the record's claim.
//
// This is the gate #437 removed without replacing. Claiming and completing
// were both authorized under OperationInvoke and both additionally required
// sameActionPrincipal, so the claimant was the enqueuer and the enqueuer's
// chain was on the record. Relaxing claiming to OperationExecute left the
// completion predicate as version, state and ClaimID — and ClaimID is
// caller-supplied, unconstrained in entropy, and published to co-principals on
// Status and to every execute-holder on Pull once a lease lapses. So a second
// execute-holder that had never claimed anything could present another
// worker's ClaimID and commit a fabricated outcome; worse, the replay branch
// then handed the real worker a success receipt carrying the fabrication,
// which is the one failure a worker cannot detect.
//
// A record claimed before this field existed has an empty chain, and an empty
// chain matches nothing — shoal.ID("") is not a valid principal and no
// decision carries it. That is the right default for a gob-decoded record from
// an older build: it refuses the completion rather than accepting it, and the
// worker's recourse is to re-claim, which writes the field.
func sameClaimantPrincipal(decision auth.Decision, record ActionRecord) bool {
	if record.ClaimantSubject == "" {
		return false
	}
	if decision.Subject() != record.ClaimantSubject ||
		decision.Actor() != record.ClaimantActor ||
		decision.ClientID() != record.ClaimantClientID {
		return false
	}
	left, right := decision.OnBehalfOf(), record.ClaimantOnBehalfOf
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func sameActionPrincipal(decision auth.Decision, record ActionRecord) bool {
	if decision.Subject() != record.Subject || decision.Actor() != record.Actor ||
		decision.ClientID() != record.ClientID {
		return false
	}
	left, right := decision.OnBehalfOf(), record.OnBehalfOf
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

// resolveAction resolves an action for in-process execution. It is
// resolveActionBinding plus the assertion that the bound reference can actually
// run the work here.
func (s *Service) resolveAction(
	ctx context.Context,
	decision auth.Decision,
	agentID shoal.ID,
	generation int64,
	capabilityName, actionName string,
	sourceID, policyID []byte,
	objectID shoal.ID,
	operation auth.Operation,
	now time.Time,
) (Descriptor, Action, ActionExecutor, error) {
	descriptor, action, raw, err := s.resolveActionBinding(
		ctx, decision, agentID, generation, capabilityName, actionName,
		sourceID, policyID, objectID, operation, now,
		// Unpinned: this record already exists, so the question is
		// whether it is still the same agent, still live, and still
		// authorized for this scope and capability — none of which a
		// heartbeat changes. The requirement itself is checked by
		// approvalGate and attestationGate. See resolveActionBinding.
		false,
	)
	if err != nil {
		return Descriptor{}, Action{}, nil, err
	}
	executor, ok := raw.(ActionExecutor)
	if !ok {
		return Descriptor{}, Action{}, nil, shoal.NewError(shoal.ErrorUnavailable, "registered executor does not implement action execution")
	}
	return descriptor, action, executor, nil
}

// resolveActionBinding resolves an action and its bound executor reference
// without requiring the reference to be runnable in this process.
//
// Authorization, scope, generation and the declared effect ceiling are all
// checked exactly as they are for in-process execution — the only thing it does
// not demand is an ActionExecutor implementation.
//
// That distinction is what makes an out-of-process executor possible. A gateway
// proxy performs its work itself and reports through CompleteClaim; nothing in
// this process ever runs it, so requiring it to supply an Execute method would
// force every remote deployment to bind a stub whose only job is to be refused.
// Claiming, cancelling, inspecting and completing an action therefore resolve
// through here, and only ExecuteClaim demands a runnable executor.
func (s *Service) resolveActionBinding(
	ctx context.Context,
	decision auth.Decision,
	agentID shoal.ID,
	generation int64,
	capabilityName, actionName string,
	sourceID, policyID []byte,
	objectID shoal.ID,
	operation auth.Operation,
	now time.Time,
	// pinned asks for the descriptor to be at exactly the generation the
	// caller names. That is the right question at enqueue and the wrong one
	// afterwards (#486).
	//
	// Heartbeat advances Generation, because the descriptor store requires
	// every mutation to advance it by exactly one and relies on that for its
	// replay detection and compare-and-set. So the counter cannot be made
	// insensitive to a lease renewal without trading a liveness bug for a
	// correctness one underneath. What had to become insensitive is the pin.
	//
	// Pinned everywhere, one heartbeat made every in-flight action on that
	// agent refuse with ObjectNotFound: a worker that had performed an effect
	// and heartbeated on schedule could not report it, a rolling restart
	// stranded a draining replica's effect, and two replicas could not share
	// one descriptor identity.
	//
	// Unpinning alone was tried and is a bypass. Update refuses any change
	// that *grows* the authorization domain, the scopes, the capabilities or
	// the parent — but turning a requirement *on* is a narrowing, so it is
	// permitted, and a record enqueued under the laxer rule then escaped the
	// stricter one. The authority gained was not the descriptor's; it was the
	// record's, which kept the rule it was created under. The pin was
	// preventing that incidentally, by refusing every post-enqueue resolution
	// of a pre-enqueue generation — the same refusal a heartbeat caused.
	//
	// So the requirement is checked directly instead, by approvalGate and
	// attestationGate against the Action this function returns, which is the
	// registration as it is *now*. A pre-flip record is refused on the
	// requirement, which is both correct and a better refusal than
	// ObjectNotFound on a generation number. A heartbeat changes neither side
	// of that test.
	//
	// What remains pinned is the freshness assertion at enqueue: a dispatcher
	// that read a descriptor and built a request from it should fail if the
	// descriptor moved underneath. Losing authority is handled by the scope
	// and capability checks below, against the descriptor just read, and
	// revocation by s.active regardless of generation.
	pinned bool,
) (Descriptor, Action, any, error) {
	return s.resolveActionBindingLapsing(ctx, decision, agentID, generation,
		capabilityName, actionName, sourceID, policyID, objectID, operation,
		now, pinned, false, false)
}

// resolveActionBindingLapsing is resolveActionBinding that, when
// allowLapsed, still resolves a descriptor whose lease has lapsed.
//
// Only an execute-route completion or ambiguity report asks for that (#391):
// a claim taken while the descriptor was live must still be reportable after
// its lease lapses, because a worker never heartbeats and cannot keep it live.
// Revocation still refuses, and so does everything else below. Taking or
// renewing work never asks, so new claims after a lapse are refused as before.
func (s *Service) resolveActionBindingLapsing(
	ctx context.Context,
	decision auth.Decision,
	agentID shoal.ID,
	generation int64,
	capabilityName, actionName string,
	sourceID, policyID []byte,
	objectID shoal.ID,
	operation auth.Operation,
	now time.Time,
	pinned bool,
	allowLapsed bool,
	// reportingOnClaim is set when the caller is reporting on a claim already
	// taken rather than taking or renewing one — Complete or ReportAmbiguity.
	// It suppresses the effect ceiling and floor re-checks below, for the
	// reason executorPhase.reportsOnClaim already states about a lapsed
	// lease: a rebind must never strand the report of an effect that
	// happened.
	//
	// Separate from allowLapsed rather than folded into it, because that one
	// is additionally gated on the execute route at its call site and this
	// one must not be. The effect happened whichever route the completion
	// arrives by.
	reportingOnClaim bool,
) (Descriptor, Action, any, error) {
	// Caller-only authorization first, before anything is looked up.
	//
	// These two checks ask whether *this caller* may act at all: does it hold
	// the operation, and — if it is acting for someone else — may it delegate.
	// Neither question involves the agent, so neither needs it to exist, and
	// running them first is what stops their answers disclosing whether it
	// does (#536).
	//
	// The resource they authorize against is built from the request and the
	// decision only. That matters: it used to take AuthorizationDomain from
	// the descriptor the lookup returned, so reordering naively would have
	// quietly changed what is being authorized. The domain here is the
	// decision's own, and the descriptor's is still required to equal it
	// below — so the conjunction is unchanged and only its order moved.
	//
	// AuthorizeObject conceals some of its own refusals and not others, and
	// its doc says which: a source or policy projection denial becomes
	// ObjectNotFound, while "whole-request failures remain unauthorized". A
	// missing operation or delegate grant is a whole-request failure, and
	// both may now answer honestly — because at this point there is nothing
	// to disclose.
	callerResource := auth.ResourceRequest{
		AuthorizationDomain: decision.AuthorizationDomain(),
		SourceID:            sourceID,
		PolicyID:            policyID,
		ObjectID:            objectID,
	}
	if err := decision.AuthorizeObject(
		operation, callerResource, now); err != nil {
		return Descriptor{}, Action{}, nil, err
	}
	if len(decision.OnBehalfOf()) > 0 {
		if err := decision.AuthorizeObject(
			auth.OperationDelegate, callerResource, now); err != nil {
			return Descriptor{}, Action{}, nil, err
		}
	}

	chain, err := s.activeChainLapsing(ctx, agentID, now, allowLapsed)
	if err != nil {
		return Descriptor{}, Action{}, nil, auth.ObjectNotFound()
	}
	descriptor := chain[0]
	if pinned && descriptor.Generation != generation {
		return Descriptor{}, Action{}, nil, auth.ObjectNotFound()
	}
	if !bytes.Equal(descriptor.AuthorizationDomain, decision.AuthorizationDomain()) {
		return Descriptor{}, Action{}, nil, auth.ObjectNotFound()
	}
	scopeFound := false
	for _, scope := range descriptor.Scopes {
		if bytes.Equal(scope.SourceID, sourceID) && bytes.Equal(scope.PolicyID, policyID) {
			scopeFound = true
			break
		}
	}
	if !scopeFound {
		return Descriptor{}, Action{}, nil, auth.ObjectNotFound()
	}
	var selected *Action
	for _, capability := range descriptor.Capabilities {
		if capability.Name != capabilityName {
			continue
		}
		for _, action := range capability.Actions {
			if action.Name == actionName {
				copy := action
				selected = &copy
				break
			}
		}
	}
	if selected == nil {
		return Descriptor{}, Action{}, nil, auth.ObjectNotFound()
	}
	raw, ok := s.executors.ResolveExecutor(descriptor.ExecutorRef)
	if !ok {
		return Descriptor{}, Action{}, nil, shoal.NewError(shoal.ErrorUnavailable, "agent executor is unavailable")
	}
	// Re-checked at resolution, not only at registration. A host can rebind an
	// executor reference to a narrower ceiling while descriptors registered
	// under the old one are still live, and those must stop resolving rather
	// than keep running against a binding that no longer permits them.
	//
	// "Stop resolving" is true of *taking* work and false of *finishing* it.
	// Completion and an ambiguity report describe an effect that already
	// happened, and refusing them does not un-happen it — it loses the
	// record, which is the failure EffectPossible and the ambiguity route
	// exist to prevent. So both re-checks are skipped when the caller is
	// reporting on a claim already taken, and applied at Pull, Claim, Invoke
	// and Extend, which is the split executorBindingPermits already draws.
	//
	// This grants a worker nothing: selected.Effects is the descriptor's
	// declaration, validated at registration and again when the work was
	// claimed. Skipping the re-check declines to re-litigate a declaration
	// that was admitted when the work was taken.
	//
	// Pinning the claimed reference instead was considered and does not
	// work. ClaimExecutorRef pins the *ref*, not the ceiling: a host that
	// rebinds ref X from a wide executor to a narrow one leaves X resolving
	// to the narrow ceiling, so the refusal returns one step later.
	if !reportingOnClaim && selected.Effects.exceeds(executorCeiling(raw)) {
		return Descriptor{}, Action{}, nil, shoal.NewError(
			shoal.ErrorUnavailable,
			"action declares effects its executor is not bound to perform")
	}
	// Re-checked here too, and for the same reason as the ceiling: a host can
	// rebind a reference to an executor that now always transmits, and a
	// descriptor registered against the old binding must stop resolving rather
	// than keep running while understating what it does.
	if !reportingOnClaim && selected.Effects.omits(executorFloor(raw)) {
		return Descriptor{}, Action{}, nil, shoal.NewError(
			shoal.ErrorUnavailable,
			"action omits effects its executor causes on every invocation")
	}
	return cloneDescriptor(descriptor), *selected, raw, nil
}

func executorKey(actionID, idempotency []byte) []byte {
	// v2 replaces the unshipped NUL-delimited encoding. It applies only when a
	// new action is admitted; replay and restart paths keep the stored key.
	digest := sha256.New()
	writeDispatchTupleField(digest, []byte("shoal.fleet.executor-key.v2"))
	writeDispatchTupleField(digest, actionID)
	writeDispatchTupleField(digest, idempotency)
	return digest.Sum(nil)
}

func transitionToken(kind string, actionID, discriminator []byte, version uint64) []byte {
	// Transition tokens are not persisted in ActionRecord, so the unshipped
	// v1 encoding has no compatibility path. Existing durable transitions keep
	// their stored state; retries reconstruct this canonical v2 tuple.
	digest := sha256.New()
	writeDispatchTupleField(
		digest, []byte("shoal.fleet.dispatch-transition.v2"))
	writeDispatchTupleField(digest, []byte(kind))
	writeDispatchTupleField(digest, actionID)
	writeDispatchTupleField(digest, discriminator)
	var raw [8]byte
	binary.BigEndian.PutUint64(raw[:], version)
	writeDispatchTupleField(digest, raw[:])
	return digest.Sum(nil)
}

func writeDispatchTupleField(digest hash.Hash, value []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = digest.Write(length[:])
	_, _ = digest.Write(value)
}

// equivalentEnqueue reports whether an existing record is the same request as
// the one being written, so a retry replays instead of conflicting.
//
// What an admission declared is part of that identity. Without the two
// Admitted fields below, a caller could ask for an admission carrying corpus
// references, receive obligations restricting them, and then replay the same
// action ID, idempotency key and token with the reference list removed: the
// record would still be recognised as the same request, obligations would be
// recomputed over nothing, and the reply would be an unrestricted allow for a
// token that is already live. The declaration has to be pinned by the record,
// not merely adjudicated on the way past it.
func equivalentEnqueue(current, wanted ActionRecord) bool {
	return equalEffects(current.AdmittedEffects, wanted.AdmittedEffects) &&
		bytes.Equal(current.AdmittedDisclosures, wanted.AdmittedDisclosures) &&
		current.AdmittedIdentityScheme == wanted.AdmittedIdentityScheme &&
		bytes.Equal(current.ID, wanted.ID) &&
		bytes.Equal(current.IdempotencyKey, wanted.IdempotencyKey) &&
		current.AgentID == wanted.AgentID &&
		current.AgentGeneration == wanted.AgentGeneration &&
		current.Capability == wanted.Capability &&
		current.Action == wanted.Action &&
		bytes.Equal(current.SourceID, wanted.SourceID) &&
		bytes.Equal(current.PolicyID, wanted.PolicyID) &&
		current.ObjectID == wanted.ObjectID &&
		bytes.Equal(current.Input, wanted.Input) &&
		current.Subject == wanted.Subject &&
		current.Actor == wanted.Actor &&
		current.ClientID == wanted.ClientID &&
		equalIDs(current.OnBehalfOf, wanted.OnBehalfOf) &&
		current.AuthorizationFingerprint == wanted.AuthorizationFingerprint &&
		current.PolicyGeneration == wanted.PolicyGeneration &&
		current.Reason == wanted.Reason &&
		current.Deadline.Equal(wanted.Deadline)
}

func equalIDs(left, right []shoal.ID) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func decisionOperations(decision auth.Decision, operation auth.Operation) []auth.Operation {
	result := []auth.Operation{operation}
	if len(decision.OnBehalfOf()) > 0 {
		result = append(result, auth.OperationDelegate)
	}
	return canonicalOperations(result)
}

func validateActionErrorCode(value string) error {
	return validateActionRecordError(value)
}

func actionEventKind(record ActionRecord) string {
	switch record.State {
	case DispatchQueued:
		return "action.enqueued"
	case DispatchClaimed:
		return "action.claimed"
	case DispatchSucceeded:
		return "action.completed"
	case DispatchFailed:
		return "action.failed"
	case DispatchCanceled:
		return "action.canceled"
	default:
		return ""
	}
}

// refuseUnconfinedRetrieval refuses an in-process execution whose executor
// cannot confine retrieval to the action's scope, when the invoking decision
// permits more than that scope.
//
// #370. The descriptor's declared scope is a confinement statement, and the
// first production executor could not honour it: AskExecutor passes only the
// question and a top-K to its provider, and ChatService authorizes
// OperationRetrieve domain-wide — so retrieval spanned every source the
// principal may read. Nothing was escalated, since the principal reads
// nothing it could not read directly, but the record ended up a cross-scope
// index of document, section and span identifiers, and the scope an operator
// read on the descriptor described none of it.
//
// Scoped retrieval is the real fix and is not available here: retrieval.Scope
// is {DocumentIDs, NodeIDs} and cannot express a (SourceID, PolicyID) bound,
// and re-minting a decision with narrowed sources would forge authority and
// change the authorization fingerprint the record pins. So this fails closed
// where the statement would otherwise be false, and nowhere else.
//
// Nowhere else matters as much as the refusal. When the decision permits no
// more than the action's own scope, the confinement constrains nothing and
// this returns nil — which is the shipped configuration, where every fleet
// principal is minted with the same domain, source and policy as the
// descriptors it registers. The cost falls on multi-source deployments, which
// are exactly the ones silently widening today.
func refuseUnconfinedRetrieval(
	decision auth.Decision,
	executor Executor,
	record ActionRecord,
	now time.Time,
) error {
	if confiner, ok := executor.(RetrievalConfiner); ok &&
		confiner.ConfinesRetrievalToScope() {
		return nil
	}
	// What the principal could retrieve beyond this action's own scope.
	//
	// An empty requested set asks for every permitted source, which is the
	// question: is there more than one, or one that is not this action's?
	sources, err := decision.IntersectSourceIDs(
		auth.OperationRetrieve, decision.AuthorizationDomain(), nil, now)
	if err != nil {
		// A principal that may not retrieve at all cannot widen retrieval,
		// so there is nothing to confine and the executor's own retrieval
		// would be refused anyway. Only an authorization answer is read this
		// way; any other error is the question failing and is returned.
		if shoal.IsErrorCode(err, shoal.ErrorUnauthorized) ||
			shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			return nil
		}
		return err
	}
	policies, err := decision.IntersectPolicyIDs(
		auth.OperationRetrieve, decision.AuthorizationDomain(), nil, now)
	if err != nil {
		if shoal.IsErrorCode(err, shoal.ErrorUnauthorized) ||
			shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			return nil
		}
		return err
	}
	if withinScope(sources, record.SourceID) &&
		withinScope(withoutOwnLabelPolicies(policies, record.SourceID), record.PolicyID) {
		return nil
	}
	// Names no source it does not already hold: the refusal says the
	// executor cannot confine, which is host configuration, and not which
	// other sources exist.
	return shoal.NewError(
		shoal.ErrorUnauthorized,
		"the bound executor does not confine retrieval to this action's "+
			"scope, and the invoking principal may retrieve beyond it; the "+
			"descriptor's scope would not describe what the record carries")
}

// withoutOwnLabelPolicies drops the canonical label policies (#570) of the
// action's own source from the policy comparison above.
//
// A label narrows and can never widen. Every label rule is
// NewAccessRule(sourcePolicy, labelPolicy...) (authorized.LabelRule): the
// source policy is always in the conjunction, and AccessRule.Authorize
// requires every term. So the documents a label policy on source A governs
// are a strict subset of A's documents, already within the action's scope,
// and #561 refuses only widening. Counting such a grant as a wider policy
// refused every principal cleared for a label an unconfined executor (#564).
//
// Only an ID auth.ParseLabelPolicyID accepts, and whose encoded source is
// the action's own, is dropped. Anything else in the label namespace is
// compared like any other policy, so it still refuses. A canonical label
// policy on another source B is compared too, and is refused when the
// principal may retrieve from B (the sources check). Without B's source, a
// held label on B reaches nothing: every term of a B rule, the source policy
// and the label policy alike, names source B, and AccessRule.Authorize checks
// each term's source against the decision.
func withoutOwnLabelPolicies(ids [][]byte, source []byte) [][]byte {
	kept := make([][]byte, 0, len(ids))
	for _, id := range ids {
		labelSource, _, err := auth.ParseLabelPolicyID(id)
		if err == nil && bytes.Equal(labelSource, source) {
			continue
		}
		kept = append(kept, id)
	}
	return kept
}

// withinScope reports whether permitted contains nothing beyond one identity.
//
// Empty is within any scope, and that reading was checked rather than
// assumed: a first version had it the other way, reasoning that an empty set
// meant "unrestricted". It does not. authorizeResource refuses any non-empty
// SourceID that is not in the decision's set, so a decision permitting no
// sources authorizes *nothing* with a source — and IntersectSourceIDs
// returning empty means either that or every permitted source being hidden.
// Both are principals that can retrieve nothing, which cannot widen anything.
//
// Getting this backwards would have refused exactly the principals with the
// least reach.
func withinScope(permitted [][]byte, scope []byte) bool {
	for _, candidate := range permitted {
		if !bytes.Equal(candidate, scope) {
			return false
		}
	}
	return true
}

// evidencePathsJoin refuses, at record time, a graph reference whose edges
// are not the path its nodes describe, or whose assertions are not the ones
// the corpus records on those edges at the executor's pinned snapshot, as
// the interaction recorder refuses them (#564). Validation already requires
// one edge fewer than nodes and every assertion on a listed edge
// (interaction.EvidenceReference.Validate); only the
// catalog and corpus know each edge's endpoints and assertions, so the rest
// is asked of the node gate. Without a gate nothing can confirm it, and the reference
// is recorded: every read then withholds it, since a nil gate withholds every
// reference naming a graph member.
func (s *DispatchService) evidencePathsJoin(
	ctx context.Context, result ExecutionResult,
) (bool, error) {
	if s.evidenceNodes == nil {
		return true, nil
	}
	references := make([]interaction.EvidenceReference, 0, len(result.Evidence))
	for _, reference := range result.Evidence {
		references = append(references, reference.interactionReference())
	}
	return s.evidenceNodes.GraphEvidenceValid(
		ctx, result.EvidenceSnapshotID, result.EvidenceSnapshotAsOf, references)
}

// structuredEvidence is the record-time half of the label rule: each
// reference's visibility is stored as the structured label-policy terms a
// reader can be shown to hold, translated from the free-form labels the
// executor reported (#564). A translation failure is a catalog read failing,
// so the completion is not recorded and the claim stands; recording the
// untranslated labels instead would withhold the evidence from every reader
// for good.
func (s *DispatchService) structuredEvidence(
	ctx context.Context, evidence []EvidenceRef,
) ([]EvidenceRef, error) {
	if s.evidenceLabels == nil || len(evidence) == 0 {
		return evidence, nil
	}
	translated := make([]EvidenceRef, len(evidence))
	for index, reference := range evidence {
		translated[index] = reference
		if len(reference.Visibility) == 0 {
			continue
		}
		visibility, err := s.evidenceLabels.StructuredVisibility(
			ctx, reference.NodeIDs, reference.EdgeIDs, reference.Visibility)
		if err != nil {
			return nil, err
		}
		canonical, err := interaction.Conjoin(visibility)
		if err != nil {
			return nil, err
		}
		translated[index].Visibility = canonical
	}
	return translated, nil
}

// readableRecord returns the record as this reader may see it, with evidence
// references whose labels the reader does not hold removed.
//
// Dropped whole, and with no count of what was dropped. The alternative #369
// weighed — anchor identity with citation detail elided — leaves the anchor
// ID, which is itself an identifier of material in a source the reader may
// not see, and a returned count that disagrees with the returned list is an
// existence oracle for the rest. #398's rule applies: a standing refusal must
// not be distinguishable from absence.
//
// What this gives up, stated rather than hidden: a reader cannot tell "this
// action recorded no evidence" from "recorded evidence you may not see". That
// is the correct trade — the alternative discloses the thing the label exists
// to protect — but it means a partial evidence list is not a completeness
// claim, and a reader reconciling grounding must not read it as one.
//
// Unlabelled references are returned unchanged. An EvidenceRef with no
// visibility expression carries no label to lack, so withholding it would
// redact something nothing asked to be protected — and that is what keeps
// this from gutting the grounding every deployment without labels relies on.
func (s *DispatchService) readableRecord(
	ctx context.Context, record ActionRecord,
) (ActionRecord, error) {
	readable, err := s.readableRecords(ctx, []ActionRecord{record})
	if err != nil {
		return ActionRecord{}, err
	}
	return readable[0], nil
}

// readableRecords is the one funnel: every dispatch read that returns a
// record passes here, Status, the Pull and TeamActions pages, and the
// enqueue, invoke and approval replays. The rule itself lives in
// evidencelabels.Verdicts, shared with fleet event delivery (#562), so the
// dispatch reads and the event stream cannot answer the same question
// differently. A reference naming nodes, edges or a cited revision is decided
// by their current rules; one naming none, by its stored labels (#564).
//
// Every reference of every record is decided in one batch, so a page costs a
// bounded number of catalog reads however many references it carries.
func (s *DispatchService) readableRecords(
	ctx context.Context, records []ActionRecord,
) ([]ActionRecord, error) {
	var references []EvidenceRef
	for _, record := range records {
		references = append(references, record.Evidence...)
	}
	verdicts, err := evidencelabels.Verdicts(
		ctx, s.evidenceVisibility, s.evidenceNodes, references,
		func(reference EvidenceRef) []string { return reference.Visibility },
		evidenceGraph)
	if err != nil {
		return nil, err
	}
	result := make([]ActionRecord, len(records))
	offset := 0
	for index, record := range records {
		count := len(record.Evidence)
		readable, withheld, err := evidencelabels.Apply(
			record.Evidence, verdicts[offset:offset+count])
		offset += count
		if err != nil {
			return nil, err
		}
		if !withheld {
			result[index] = record
			continue
		}
		redacted := cloneActionRecord(record)
		if len(readable) == 0 {
			// Exactly a record that never had evidence: Validate refuses a
			// snapshot pin without evidence, and the wire emits the pin, so a
			// pin left behind would itself say evidence was withheld (#398).
			// cloneActionEvidence's empty value is how a read returns a
			// record stored without evidence.
			redacted.Evidence = cloneActionEvidence(nil)
			redacted.EvidenceSnapshotID = ""
			redacted.EvidenceSnapshotAsOf = time.Time{}
		} else {
			redacted.Evidence = readable
		}
		result[index] = redacted
	}
	return result, nil
}

// evidenceGraph is what a reference names in the corpus. Every assertion is on
// one of its EdgeIDs (interaction.EvidenceReference.Validate), so the edges
// cover them; a document reference also names the revision it cites. One
// rule over both kinds: a reference is no more visible than the object it
// cites is readable now. A document reference cites a revision, so the
// revision's own rule decides it with its nodes'; a graph reference cites
// nodes and edges.
//
// The AnchorID stays on the protected side with the rest of the reference.
// It is content-derived (it hashes the document ID, titles and quoted text),
// so a reader who can guess the content could use it to confirm the guess.
func evidenceGraph(reference EvidenceRef) evidencelabels.Graph {
	graph := evidencelabels.Graph{NodeIDs: reference.NodeIDs, EdgeIDs: reference.EdgeIDs}
	if reference.Kind == interaction.EvidenceDocument {
		graph.DocumentID = reference.Citation.DocumentID
		graph.RevisionID = reference.Citation.RevisionID
	}
	return graph
}

// readablePage decides every record in a page in one batch.
func (s *DispatchService) readablePage(
	ctx context.Context, page ActionPage,
) (ActionPage, error) {
	readable, err := s.readableRecords(ctx, page.Actions)
	if err != nil {
		return ActionPage{}, err
	}
	copy(page.Actions, readable)
	return page, nil
}
