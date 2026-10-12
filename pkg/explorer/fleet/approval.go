// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package fleet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math/rand/v2"
	"sort"
	"strings"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Approval holds an operation for a human decision (#451).
//
// A held request is not an action. It is its own record, in its own key space,
// and no ActionRecord exists for it until an approver distinct from the
// requester has approved that exact request and the requester has come back for
// it. That is the whole of the safety argument, and it is the same one the
// admission seam makes for never passing through the queued state: Pull, Claim,
// Invoke and ExecuteClaim all start from an ActionRecord, so work that has no
// ActionRecord cannot be pulled, claimed or performed through any of them,
// across restart and retry, without a single edit to any of them.
//
// A new DispatchState was the alternative and it was refused. A "held" state
// would fall through Claim's checks into applyClaim unless every one of them
// learned about it, and the approver's transition would land in the requester's
// outbox — the mixed-identity outbox #480 describes, which no single principal
// can drain.
//
// The flow is three steps, each a compare-and-set on the approval record:
//
//   - Request, by the requester under dispatch. Builds exactly the version-1
//     record an enqueue would commit, stores it as the request, and holds it
//     pending until ExpiresAt.
//   - Decide, by an approver under OperationActionApprove. Approves or refuses
//     the pending request, naming the digest and the policy generation it
//     reviewed.
//   - Materialize, by the requester re-requesting. The rebuilt request must
//     equal the stored one; the approval moves approved → enqueued, and only
//     then is the ActionRecord created, from the stored request plus the
//     approval provenance, through the same audit and publication path an
//     enqueue uses.
//
// An approval grants the approver nothing, and it grants the requester nothing
// beyond the one queued record. The work still runs only through that record's
// principal and the claim fence: a ZTAT is a record, not a grant.
var (
	// ErrApprovalRequired reports an attempt to enqueue or invoke an action
	// whose registration requires approval through any route but the
	// approval one. See approvalRequired for the transport code it carries.
	ErrApprovalRequired = errors.New("fleet dispatch: action requires approval")
	// ErrApprovalNotFound is the store's sentinel for an absent approval. It
	// never reaches a caller: every service path normalises it to the
	// not-found shape every other absent record gets.
	ErrApprovalNotFound = errors.New("fleet approval: approval not found")
	// ErrApprovalConflict reports a request or a decision that is not the one
	// already on the record: a changed request under the same identity, a
	// digest the approver did not review, or a decision that differs from
	// one already made.
	ErrApprovalConflict = errors.New("fleet approval: approval conflict")
	// ErrApprovalExpired reports a decision attempted after the approval
	// window closed. The expiry is written before this is returned.
	ErrApprovalExpired = errors.New("fleet approval: approval window has closed")
	// ErrApprovalSuperseded reports a decision made under a policy
	// generation other than the one the request was made under.
	ErrApprovalSuperseded = errors.New(
		"fleet approval: policy generation has moved since the request")
	// ErrApproverMappingMoved reports a decision, or a materialization of
	// one, under an approver mapping other than the one in force: the
	// approver's credential was minted under a mapping that has since
	// changed, or the mapping changed between the approval and its use.
	ErrApproverMappingMoved = errors.New(
		"fleet approval: approver mapping has moved since the decision")
	// ErrIdentitySchemeMoved reports a decision on a request made under an
	// identity scheme other than the approver's (#526): the host changed how
	// it names principals between the request and the decision, so the
	// requester and the approver are not named in one identity space and
	// cannot be compared for independence. Such a request can only expire.
	ErrIdentitySchemeMoved = errors.New(
		"fleet approval: identity scheme has moved since the request")
	// ErrApprovalWithdrawn reports a decision on a request its requester
	// has withdrawn (#655). Withdrawal is final; the request will never be
	// decided.
	ErrApprovalWithdrawn = errors.New(
		"fleet approval: the request has been withdrawn")
	// ErrApprovalStranded reports a decision or a withdrawal on a request
	// that has been recorded as stranded (#655): its target, policy
	// generation or action identity moved permanently, and it can never
	// become work.
	ErrApprovalStranded = errors.New(
		"fleet approval: the request can no longer progress")
	// ErrApprovalTerminalStatesDisabled reports a withdrawal, or a sweep, on
	// a host whose operator has not enabled the withdrawn and stranded
	// states (ApprovalConfig.TerminalStates, #655). A build before #655
	// cannot read either state, so they are written only once the operator
	// says every replica can.
	ErrApprovalTerminalStatesDisabled = errors.New(
		"fleet approval: withdrawal and stranding are not enabled on this host")
)

// approvalRequired is what enqueue and invoke return for an action that
// requires approval.
//
// Conflict, deliberately, and every client will branch on it, so the choice
// is worth stating. The request is well formed (not invalid_argument) and the
// caller is authorized to make it (not unauthorized — the action may well
// happen, pending somebody else's decision). Nothing is unavailable, so a
// retry of the same request will never succeed (not unavailable). What remains
// is the closest thing this code set has to a failed precondition: the request
// conflicts with the registered state of the action, and the remedy is a
// different route, not a different request. The message names that route.
func approvalRequired() error {
	return shoal.WrapError(
		shoal.ErrorConflict,
		"action requires approval; request it through the approval route",
		ErrApprovalRequired)
}

const (
	// DefaultApprovalWindow is how long a request stays decidable when the
	// host configures nothing. It is further clamped to the request's own
	// deadline.
	DefaultApprovalWindow = time.Hour
	// MaxApprovalWindow bounds the configured window. A request cannot
	// outlive its action deadline, which MaxActionDeadline already bounds.
	MaxApprovalWindow = MaxActionDeadline
	// MaxApprovalListResults bounds one page of pending approvals.
	MaxApprovalListResults = 64
)

// ApprovalState is where a held request is.
type ApprovalState string

const (
	// ApprovalPending is held and decidable until ExpiresAt.
	ApprovalPending ApprovalState = "pending"
	// ApprovalApproved has been approved and not yet materialized.
	ApprovalApproved ApprovalState = "approved"
	// ApprovalRefused has been refused. It is final.
	ApprovalRefused ApprovalState = "refused"
	// ApprovalExpired closed without becoming work: undecided at ExpiresAt,
	// or approved and not materialized before it. Verdict says which. Final.
	ApprovalExpired ApprovalState = "expired"
	// ApprovalEnqueued has committed to become an ActionRecord at the same
	// identity. Final, except that one whose action can provably never be
	// written is recorded as stranded (#655).
	ApprovalEnqueued ApprovalState = "enqueued"
	// ApprovalWithdrawn was withdrawn by its requester while pending (#655).
	// Final.
	ApprovalWithdrawn ApprovalState = "withdrawn"
	// ApprovalStranded can never progress, for the permanent reason its
	// StrandedCause records (#655). Final. It is written only for a cause
	// that is a durable fact no later event can undo; see approvalStrandable.
	ApprovalStranded ApprovalState = "stranded"
)

func (s ApprovalState) valid() bool {
	switch s {
	case ApprovalPending, ApprovalApproved, ApprovalRefused,
		ApprovalExpired, ApprovalEnqueued, ApprovalWithdrawn,
		ApprovalStranded:
		return true
	default:
		return false
	}
}

// ApprovalTransitionAllowed reports whether an approval record may move from
// one state to another in one compare-and-set (#655).
//
// The table is the whole lifecycle. Nothing leaves refused, expired,
// withdrawn or stranded, so a terminal record can never be re-enabled, and
// nothing reaches enqueued except from approved, so nothing materializes from
// a terminal state. Enqueued may still be recorded as stranded, which is a
// statement that its action can never be written, never a way back.
//
// Exported for the durable store, which refuses any write outside it: the
// service builds every transition to fit, and this makes that a property of
// the store rather than of every call site.
func ApprovalTransitionAllowed(from, to ApprovalState) bool {
	switch from {
	case ApprovalPending:
		switch to {
		case ApprovalApproved, ApprovalRefused, ApprovalExpired,
			ApprovalWithdrawn, ApprovalStranded:
			return true
		}
	case ApprovalApproved:
		switch to {
		case ApprovalEnqueued, ApprovalExpired, ApprovalStranded:
			return true
		}
	case ApprovalEnqueued:
		return to == ApprovalStranded
	}
	return false
}

// terminal reports whether a stored state is final.
func (s ApprovalState) terminal() bool {
	switch s {
	case ApprovalRefused, ApprovalExpired, ApprovalWithdrawn, ApprovalStranded:
		return true
	default:
		return false
	}
}

// ApprovalVerdict is what an approver decided.
type ApprovalVerdict string

const (
	ApprovalVerdictApprove ApprovalVerdict = "approve"
	ApprovalVerdictRefuse  ApprovalVerdict = "refuse"
)

func (v ApprovalVerdict) validate() error {
	switch v {
	case ApprovalVerdictApprove, ApprovalVerdictRefuse:
		return nil
	default:
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "approval verdict is invalid")
	}
}

// ApprovalRecord is the durable statement of one held request and what became
// of it.
//
// Request is the exact version-1 queued record the requester's decision built,
// stored rather than rebuilt so that what was approved is what materializes:
// a materialization copies it, it never recomputes it. RequestDigest is a
// digest over the fields equivalentEnqueue compares, and it is what an
// approver names to say which request it reviewed.
//
// Everything a dataset of adjudications (#401, #419) needs is here: the exact
// request, the requester, the approver, the verdict, the time, and the policy
// generation both were made under. Exporting it is deferred; storing it is not.
type ApprovalRecord struct {
	ID               []byte
	Version          uint64
	State            ApprovalState
	Request          ActionRecord
	RequestDigest    []byte
	PolicyGeneration int64
	RequestedAt      time.Time
	ExpiresAt        time.Time
	UpdatedAt        time.Time
	// Verdict and the approver fields are set together, by Decide, and are
	// kept when an approved request expires unmaterialized: an approval that
	// was given and never used is still an adjudication.
	Verdict                  ApprovalVerdict
	ApproverSubject          shoal.ID
	ApproverActor            shoal.ID
	ApproverClientID         shoal.ID
	ApproverFingerprint      auth.Fingerprint
	ApproverPolicyGeneration int64
	DecidedAt                time.Time
	DecisionRequestID        shoal.ID
	DecisionCorrelationID    shoal.ID
	// ApproverMappingDigest is the operator approver mapping the decision
	// was made under, and ApproverProvenance is the claim that mapped the
	// approver to the role (#451). Both are zero for a decision made by an
	// approver no mapping granted, and for every record written before they
	// existed: they are additive, so an older record decodes unchanged. An
	// approved record is materialized only while ApproverMappingDigest is
	// still the digest in force. Neither ever holds token bytes.
	ApproverMappingDigest auth.Digest
	ApproverProvenance    auth.GrantProvenance
	// IdentityScheme is the identity scheme the requester was named under
	// when the request was made (#526): IdentityScheme.Digest when the
	// requester's subject is in the scheme in force, and zero otherwise. It
	// is creation-time requester state, not approver provenance, and the
	// durable store refuses any write that changes it. It is additive: a
	// record written before it existed decodes with it zero, which is the
	// legacy scheme — the one every such record was in fact made under — and
	// never "different from every scheme", which would strand every pending
	// request on upgrade.
	IdentityScheme auth.Digest
	// MaterializedAt is when the approved request was committed to become
	// work. It is written by the approved → enqueued compare-and-set, before
	// the ActionRecord exists, and is the UpdatedAt the ActionRecord is
	// created with, so a retry after a crash between the two writes builds
	// the same record.
	MaterializedAt time.Time
	// WithdrawnAt and WithdrawalReason are written once, by the requester's
	// withdrawal (#655), and only on a withdrawn record. The reason is the
	// withdrawal request's own, redacted as every request reason is: a code
	// and a digest of the free-form detail.
	WithdrawnAt      time.Time
	WithdrawalReason interaction.Reason
	// StrandedCause is why a stranded record can never progress (#655), and
	// is set on a stranded record only. It is one of the permanent causes
	// approvalStrandable names. Additive like every field after Request:
	// a record a previous build wrote decodes with all three zero.
	StrandedCause ApprovalCondition
}

// Validate checks a record's shape, including that the stored digest is the
// digest of the stored request. A record whose digest does not describe its
// request would let an approver review one request and approve another.
func (r ApprovalRecord) Validate() error {
	if err := validateOpaque("approval ID", r.ID, false); err != nil {
		return err
	}
	if reservedAdmissionID(r.ID) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "approval ID is reserved")
	}
	if r.Version == 0 {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "approval version is required")
	}
	if !r.State.valid() {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "approval state is invalid")
	}
	if err := r.Request.Validate(); err != nil {
		return err
	}
	if r.Request.State != DispatchQueued || r.Request.Version != 1 ||
		!bytes.Equal(r.Request.ID, r.ID) || r.Request.isAdmission() ||
		len(r.Request.ApprovalRequestDigest) != 0 {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"approval request is not a fresh queued record")
	}
	if !bytes.Equal(r.RequestDigest, ApprovalRequestDigest(r.Request)) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"approval digest does not describe its request")
	}
	if r.PolicyGeneration != r.Request.PolicyGeneration {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"approval policy generation does not match its request")
	}
	for name, value := range map[string]time.Time{
		"requested": r.RequestedAt, "expiry": r.ExpiresAt, "updated": r.UpdatedAt,
	} {
		if value.IsZero() || value.Location() != time.UTC {
			return shoal.NewError(
				shoal.ErrorInvalidArgument, "approval "+name+" time must be UTC")
		}
	}
	if !r.RequestedAt.Equal(r.Request.CreatedAt) ||
		!r.RequestedAt.Before(r.ExpiresAt) ||
		r.ExpiresAt.After(r.Request.Deadline) ||
		r.UpdatedAt.Before(r.RequestedAt) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "approval timestamps are inconsistent")
	}
	decided := r.Verdict != ""
	switch r.State {
	case ApprovalPending:
		if decided {
			return shoal.NewError(
				shoal.ErrorInvalidArgument, "a pending approval carries a verdict")
		}
	case ApprovalApproved, ApprovalEnqueued:
		if r.Verdict != ApprovalVerdictApprove {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"an approved request requires an approving verdict")
		}
	case ApprovalRefused:
		if r.Verdict != ApprovalVerdictRefuse {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"a refused request requires a refusing verdict")
		}
	case ApprovalExpired:
		// Either never decided, or approved and never materialized. A
		// refusal is already final and does not expire.
		if decided && r.Verdict != ApprovalVerdictApprove {
			return shoal.NewError(
				shoal.ErrorInvalidArgument, "an expired approval verdict is invalid")
		}
	case ApprovalWithdrawn:
		// Withdrawn only while pending, so never decided.
		if decided {
			return shoal.NewError(
				shoal.ErrorInvalidArgument, "a withdrawn approval carries a verdict")
		}
	case ApprovalStranded:
		// Stranded from pending, approved or enqueued; never from refused.
		if decided && r.Verdict != ApprovalVerdictApprove {
			return shoal.NewError(
				shoal.ErrorInvalidArgument, "a stranded approval verdict is invalid")
		}
	}
	if err := r.validateWithdrawal(); err != nil {
		return err
	}
	if err := r.validateStranding(); err != nil {
		return err
	}
	if decided {
		if err := r.validateDecision(); err != nil {
			return err
		}
	} else if r.ApproverSubject != "" || r.ApproverActor != "" ||
		r.ApproverClientID != "" ||
		r.ApproverFingerprint != (auth.Fingerprint{}) ||
		r.ApproverPolicyGeneration != 0 || !r.DecidedAt.IsZero() ||
		r.DecisionRequestID != "" || r.DecisionCorrelationID != "" ||
		r.ApproverMappingDigest != (auth.Digest{}) ||
		r.ApproverProvenance.Set() {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"an undecided approval carries approver provenance")
	}
	materialized := r.State == ApprovalEnqueued ||
		(r.State == ApprovalStranded && strandedAfterMaterializing(r.StrandedCause))
	if materialized == r.MaterializedAt.IsZero() {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"approval materialization time is inconsistent with its state")
	}
	if !r.MaterializedAt.IsZero() &&
		(r.MaterializedAt.Location() != time.UTC ||
			r.MaterializedAt.Before(r.DecidedAt) ||
			!r.MaterializedAt.Before(r.ExpiresAt)) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"approval materialization time is outside its window")
	}
	return nil
}

func (r ApprovalRecord) validateDecision() error {
	if err := shoal.ValidateRequiredID("approver", r.ApproverSubject); err != nil {
		return err
	}
	if err := shoal.ValidateRequiredID(
		"approver actor", r.ApproverActor); err != nil {
		return err
	}
	if err := shoal.ValidateOptionalID(
		"approver client", r.ApproverClientID); err != nil {
		return err
	}
	if err := shoal.ValidateRequiredID(
		"approval decision request ID", r.DecisionRequestID); err != nil {
		return err
	}
	if err := shoal.ValidateOptionalID(
		"approval decision correlation ID", r.DecisionCorrelationID); err != nil {
		return err
	}
	if r.ApproverFingerprint == (auth.Fingerprint{}) ||
		r.ApproverPolicyGeneration != r.PolicyGeneration ||
		r.DecidedAt.IsZero() || r.DecidedAt.Location() != time.UTC ||
		r.DecidedAt.Before(r.RequestedAt) || !r.DecidedAt.Before(r.ExpiresAt) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "approval decision provenance is incomplete")
	}
	// The mapping digest and the provenance are written together: a digest
	// with no claim behind it, or a claim under a different digest, is not a
	// record any decide wrote.
	if err := r.ApproverProvenance.Validate(); err != nil {
		return err
	}
	if r.ApproverProvenance.MappingDigest != r.ApproverMappingDigest {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"approval mapping digest does not match its provenance")
	}
	return nil
}

// validateWithdrawal checks the withdrawal fields: present, consistent and
// inside the window on a withdrawn record, and absent on every other.
func (r ApprovalRecord) validateWithdrawal() error {
	if r.State != ApprovalWithdrawn {
		if !r.WithdrawnAt.IsZero() || r.WithdrawalReason != (interaction.Reason{}) {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"only a withdrawn approval carries a withdrawal")
		}
		return nil
	}
	if r.WithdrawnAt.IsZero() || r.WithdrawnAt.Location() != time.UTC ||
		r.WithdrawnAt.Before(r.RequestedAt) || !r.WithdrawnAt.Before(r.ExpiresAt) ||
		!r.UpdatedAt.Equal(r.WithdrawnAt) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "approval withdrawal time is inconsistent")
	}
	if r.WithdrawalReason.Code == "" {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "approval withdrawal reason is required")
	}
	return r.WithdrawalReason.Validate()
}

// ApprovalStrandGrace is how long after an enqueued request's action
// deadline it is recorded as stranded when its action was never written
// (#655). Every re-request runs under a context whose deadline is the action
// deadline, because the action deadline is the request context's deadline
// (queuedRecord) and a re-request must name the same one (equivalentEnqueue).
// So past the deadline no call that could still write the action is running,
// on a replica whose clock agrees; the grace is the replica clock skew the
// event window already allows (ApprovalEventSkewAllowance).
//
// What the grace does not bound is a write that call already handed to the
// coordination runtime. A materialization whose publish was interrupted
// (indeterminate, a crash) is a durable intent that recovery rolls forward
// later, with no time check, so its ActionRecord can still land after the
// approval was recorded stranded/deadline_passed. It lands past its own
// deadline, which every claim refuses (Claim, ExecuteClaim), so it can never
// run; docs/approval.md states it as a residual.
const ApprovalStrandGrace = time.Hour

// strandedAfterMaterializing reports whether a stranding cause applies to a
// request that had committed to become work (enqueued), as opposed to one
// still pending or approved.
func strandedAfterMaterializing(cause ApprovalCondition) bool {
	return cause == ApprovalConditionIdentityTaken ||
		cause == ApprovalConditionDeadlinePassed
}

// validateStranding checks the stranding cause: one of the permanent causes,
// consistent with the state it was stranded from, on a stranded record only.
func (r ApprovalRecord) validateStranding() error {
	if r.State != ApprovalStranded {
		if r.StrandedCause != ApprovalConditionNone {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"only a stranded approval carries a stranding cause")
		}
		return nil
	}
	switch r.StrandedCause {
	case ApprovalConditionTargetMoved, ApprovalConditionPolicyMoved:
		// Stranded while pending or approved, which is possible only inside
		// the window: past it the request is expired, not stranded.
		if !r.UpdatedAt.Before(r.ExpiresAt) {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"a request whose window has closed is expired, not stranded")
		}
	case ApprovalConditionIdentityTaken:
	case ApprovalConditionDeadlinePassed:
		if r.UpdatedAt.Before(r.Request.Deadline.Add(ApprovalStrandGrace)) {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"an enqueued request is stranded on its deadline only after the grace")
		}
	default:
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "approval stranding cause is invalid")
	}
	if strandedAfterMaterializing(r.StrandedCause) &&
		r.Verdict != ApprovalVerdictApprove {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"only an approved request can be stranded after materializing")
	}
	return nil
}

// CloneApprovalRecord returns an independent copy.
func CloneApprovalRecord(input ApprovalRecord) ApprovalRecord {
	result := input
	result.ID = append([]byte(nil), input.ID...)
	result.Request = cloneActionRecord(input.Request)
	result.RequestDigest = append([]byte(nil), input.RequestDigest...)
	result.ApproverProvenance = input.ApproverProvenance.Clone()
	return result
}

// ApprovalRequestDigest identifies the exact request an approver reviews.
// Exported for durable stores outside this package, which must be able to
// build and check the records they hold; a client cannot recompute it, since
// it covers the server-side authorization fingerprint.
//
// It covers every field equivalentEnqueue compares and nothing else, so the
// two cannot disagree about whether a re-request is the request that was
// approved: a change that equivalentEnqueue would call a different request
// changes this digest, and one it would call the same request does not. The
// admission fields are omitted because an approval request is never an
// admission and Validate refuses one that is.
//
// Every field is length-framed, so no two distinct requests can collide by
// moving bytes between adjacent fields.
func ApprovalRequestDigest(record ActionRecord) []byte {
	digest := sha256.New()
	writeDispatchTupleField(digest, []byte("shoal.fleet.approval-request.v1"))
	writeDispatchTupleField(digest, record.ID)
	writeDispatchTupleField(digest, record.IdempotencyKey)
	writeDispatchTupleField(digest, []byte(record.AgentID))
	writeApprovalInt64(digest, record.AgentGeneration)
	writeDispatchTupleField(digest, []byte(record.Capability))
	writeDispatchTupleField(digest, []byte(record.Action))
	writeDispatchTupleField(digest, record.SourceID)
	writeDispatchTupleField(digest, record.PolicyID)
	writeDispatchTupleField(digest, []byte(record.ObjectID))
	writeDispatchTupleField(digest, record.Input)
	writeDispatchTupleField(digest, []byte(record.Subject))
	writeDispatchTupleField(digest, []byte(record.Actor))
	writeDispatchTupleField(digest, []byte(record.ClientID))
	writeApprovalInt64(digest, int64(len(record.OnBehalfOf)))
	for _, identity := range record.OnBehalfOf {
		writeDispatchTupleField(digest, []byte(identity))
	}
	writeDispatchTupleField(digest, record.AuthorizationFingerprint[:])
	writeApprovalInt64(digest, record.PolicyGeneration)
	writeDispatchTupleField(digest, []byte(record.Reason.Code))
	writeDispatchTupleField(digest, []byte(record.Reason.Digest))
	writeApprovalInt64(digest, record.Deadline.UnixNano())
	return digest.Sum(nil)
}

func writeApprovalInt64(digest interface{ Write([]byte) (int, error) }, value int64) {
	var raw [8]byte
	binary.BigEndian.PutUint64(raw[:], uint64(value))
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(raw)))
	_, _ = digest.Write(length[:])
	_, _ = digest.Write(raw[:])
}

// ApprovalMutation is one compare-and-set on an approval record.
type ApprovalMutation struct {
	Token           []byte
	ExpectedVersion uint64
	Record          ApprovalRecord
	// Publisher is the principal making this transition (#653). A store that
	// keeps an approval outbox writes the transition's event row, naming it,
	// in the same durable write as Record. Zero writes no row.
	Publisher ApprovalPublisher
}

type ApprovalPage struct {
	Approvals []ApprovalRecord
	Next      []byte
	// Undecodable is set by ApprovalTolerantScanner only: the stored records
	// on this page it skipped because they do not decode.
	Undecodable int
}

// ApprovalStore is the durable approval key space. ApplyApproval must refuse,
// with ErrApprovalConflict, any write whose ExpectedVersion is not the stored
// version, and must accept an identical retry of a write already applied.
type ApprovalStore interface {
	GetApproval(context.Context, []byte) (ApprovalRecord, error)
	ApplyApproval(context.Context, ApprovalMutation) (ApprovalRecord, error)
	ScanApprovals(context.Context, []byte, int) (ApprovalPage, error)
}

// ApprovalAudit is one privileged approval transition, attributed to the
// principal that made it.
//
// The acting identity is carried explicitly rather than read off the record,
// because the record holds two principals — the requester and the approver —
// and which one acted depends on the phase. The action recorder reads the
// actor off the record and is wrong for any caller that is not the enqueuer
// (#480 item 2); this does not repeat that.
type ApprovalAudit struct {
	Phase                    string
	Operation                auth.Operation
	Record                   ApprovalRecord
	Subject                  shoal.ID
	Actor                    shoal.ID
	ClientID                 shoal.ID
	OnBehalfOf               []shoal.ID
	RequestID                shoal.ID
	AuthorizationFingerprint auth.Fingerprint
	AuthorizationExpiresAt   time.Time
	// Provenance is the acting decision's grant provenance: which operator
	// mapping, and which claim value, made it an approver. It is empty for a
	// principal no mapping granted. It never carries token bytes.
	Provenance auth.GrantProvenance
}

type ApprovalRecorder interface {
	// RecordApproval must be idempotent for an identical audit.
	RecordApproval(context.Context, ApprovalAudit) error
}

type ApprovalConfig struct {
	Dispatch *DispatchService
	Store    ApprovalStore
	Recorder ApprovalRecorder
	// Narrowed reports whether the request's decision was derived through a
	// narrowing the host applies, such as workspace settings. Required.
	//
	// The approver-separation rule requires an approver to *fail* dispatch,
	// invoke and execute, and a narrowing can only remove authority — so a
	// principal holding approve and dispatch could narrow its own decision to
	// approve alone and pass. Separation has to be judged on the authority a
	// principal actually holds, so every approver path refuses a narrowed
	// decision. The host supplies the predicate because only it knows how it
	// narrows; a host that never narrows supplies one returning false.
	Narrowed func(context.Context) bool
	// Generations is the current-policy authority. Required. A decision
	// carries the generation it was minted under, which may be older than
	// the one in force; deciding and reporting against the token's would
	// approve, or call live, a request under a superseded policy.
	Generations auth.GenerationReader
	// ApproverMapping returns the digest of the operator approver mapping in
	// force (#451), or the zero digest when none is configured. Optional:
	// nil means no mapping. Decide requires the approver's decision to have
	// been minted under exactly this digest and records it; an approved
	// request is materialized only while it is still the one in force.
	//
	// It is a digest of the mapping rather than a policy generation on
	// purpose. The workspace generation is fixed configuration, and deriving
	// one from a hash could move it backwards; the digest only ever needs to
	// be compared for equality.
	ApproverMapping func(context.Context) (auth.Digest, error)
	// Window is how long a request stays decidable. Zero means
	// DefaultApprovalWindow. It is clamped to each request's deadline.
	Window time.Duration
	// IdentityScheme is how the host names its principals (#526). The zero
	// value configures no namespace rule and stamps nothing: a host whose
	// principals are not OIDC-minted.
	IdentityScheme IdentityScheme
	// Outbox and Events publish approval lifecycle events (#653). They are
	// optional and set together. With neither, transitions are audited and
	// publish nothing, as before. Outbox must be the durable store that
	// writes the rows: Store's ApplyApproval writes them, and a wrapper
	// around Store need not expose them.
	Outbox ApprovalTransitionStore
	Events ApprovalEventPublisher
	// Sweeper binds the host's system observer for one approval's scope
	// (#655). Optional: nil configures no sweep, and Sweep refuses. See
	// ApprovalSweepBinder.
	Sweeper ApprovalSweepBinder
	// TerminalStates enables the withdrawn and stranded states (#655): the
	// requester's withdrawal, the lazy stranding Decide and Status write,
	// and the sweep. Off, nothing writes either state, by any path: Withdraw
	// refuses with ErrApprovalTerminalStatesDisabled, Status and Decide
	// report what they always reported and write nothing new, and Sweep
	// refuses. A build before #655 cannot read either state, nor an outbox
	// row the sweep writes, so the operator turns this on only once every
	// replica runs a build that can; until then rolling back is safe.
	TerminalStates bool
}

// IdentityScheme describes the identity scheme in force (#526).
//
// Separation of duty compares identities, so it is only meaningful while the
// requester and the approver are named in one identity space. A host that
// changes how it names principals changes every identity, and an identity
// under one scheme cannot be compared with one under another: the same human
// can hold one of each. So the rule is not a list of schemes that used to be
// in force — that list cannot be complete across every switch history and
// every mixed rollout — but the converse: within the host's identity family,
// only the namespace in force is acceptable.
type IdentityScheme struct {
	// Digest stamps requests (ApprovalRecord.IdentityScheme). Zero is the
	// default, sub-derived scheme, which is also what every record written
	// before the stamp existed decodes as.
	Digest auth.Digest
	// Prefix is the namespace in force: it begins every identity the scheme
	// in force mints. It must itself begin with one of Family.
	Prefix string
	// Family are the identity prefixes this host's authenticator can mint
	// human principals under, under any scheme and any issuer it has ever
	// been configured with: a deployment has exactly one human OIDC issuer,
	// so an identity of another issuer is one minted before the issuer
	// changed, not an unrelated principal. An identity in the family but
	// outside Prefix was minted under another scheme, and is refused
	// wherever it is involved in an approval. Identities outside the family
	// — service, development, executor and MCP principals — are not this
	// rule's business. Empty, with an empty Prefix and a zero Digest,
	// configures no rule.
	Family []string
	// Flat narrows the namespace in force to Prefix followed by a value
	// with no '#' in it (#546). Without it, every identity beginning with
	// Prefix is in force. The sub-derived scheme's namespace oidc:<iss>#
	// contains, as a string prefix, the namespace of every non-default
	// subject claim, oidc:<iss>#<claim tag>#, so without Flat a human who
	// registered under one could approve under the other. The host refuses
	// '#' in every value it mints under a Flat prefix, so an identity that
	// begins with Prefix and has a later '#' was minted under a nested
	// scheme, and is foreign.
	Flat bool
}

func (s IdentityScheme) validate() error {
	if s.Prefix == "" {
		if s.Digest != (auth.Digest{}) || len(s.Family) > 0 || s.Flat {
			return shoal.NewError(
				shoal.ErrorInvalidArgument, "identity scheme prefix is required")
		}
		return nil
	}
	inFamily := false
	for _, namespace := range s.Family {
		if namespace == "" {
			return shoal.NewError(
				shoal.ErrorInvalidArgument, "an identity namespace is empty")
		}
		if strings.HasPrefix(s.Prefix, namespace) {
			inFamily = true
		}
	}
	if !inFamily {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"the identity scheme prefix must be in its identity family")
	}
	return nil
}

// of is the stamp for an identity: Digest when it is in the namespace in
// force, and zero otherwise.
func (s IdentityScheme) of(identity shoal.ID) auth.Digest {
	if s.inForce(identity) {
		return s.Digest
	}
	return auth.Digest{}
}

// inForce reports whether an identity is in the namespace in force: it
// begins with Prefix and, under a Flat scheme, has no '#' after it.
func (s IdentityScheme) inForce(identity shoal.ID) bool {
	if s.Prefix == "" || !strings.HasPrefix(string(identity), s.Prefix) {
		return false
	}
	return !s.Flat || !strings.Contains(string(identity)[len(s.Prefix):], "#")
}

// foreignNamespace returns the family namespace of the first identity (in
// sorted order, so the answer is stable) that is in the family but not in
// the namespace in force, or "".
func (s IdentityScheme) foreignNamespace(identities []shoal.ID) string {
	if s.Prefix == "" {
		return ""
	}
	sorted := append([]shoal.ID(nil), identities...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	for _, identity := range sorted {
		if s.inForce(identity) {
			continue
		}
		if strings.HasPrefix(string(identity), s.Prefix) {
			// Only under a Flat scheme: a namespace nested inside the one
			// in force. It is named without the segment after Prefix, which
			// in an identity minted before the host refused '#' could be
			// part of a value.
			return s.Prefix + nestedNamespace
		}
		for _, family := range s.Family {
			if strings.HasPrefix(string(identity), family) {
				return namespaceOf(string(identity), family)
			}
		}
	}
	return ""
}

// nestedNamespace stands, in a refusal, for the segment of a namespace
// nested inside a Flat namespace in force.
const nestedNamespace = "<nested>#"

// namespaceOf names the namespace of a family identity for a refusal: the
// family prefix and everything up to and including the first '#' after it,
// which for a minted identity is the issuer (oidc:<iss>#, oidcid:<iss>#)
// and never the value. The host refuses an issuer containing '#' anywhere,
// an empty fragment included, so the first '#' ends the issuer. Without a
// '#' the family prefix alone is named.
func namespaceOf(identity, family string) string {
	rest := identity[len(family):]
	if index := strings.IndexByte(rest, '#'); index >= 0 {
		return family + rest[:index+1]
	}
	return family
}

func cloneIdentityScheme(s IdentityScheme) IdentityScheme {
	s.Family = append([]string(nil), s.Family...)
	return s
}

func identitySchemeMoved() error {
	return shoal.WrapError(
		shoal.ErrorConflict,
		"the request was made under an identity scheme that is no longer in "+
			"force; it cannot be decided and will expire",
		ErrIdentitySchemeMoved)
}

type ApprovalService struct {
	dispatch    *DispatchService
	store       ApprovalStore
	recorder    ApprovalRecorder
	narrowed    func(context.Context) bool
	generations auth.GenerationReader
	mapping     func(context.Context) (auth.Digest, error)
	window      time.Duration
	scheme      IdentityScheme
	events      *approvalEvents
	sweeper     ApprovalSweepBinder
	terminal    bool
	sweep       approvalSweepState
}

func NewApprovalService(config ApprovalConfig) (*ApprovalService, error) {
	if config.Dispatch == nil || config.Store == nil || config.Recorder == nil ||
		config.Narrowed == nil || config.Generations == nil {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "fleet approval dependencies are required")
	}
	window := config.Window
	if window == 0 {
		window = DefaultApprovalWindow
	}
	if window < 0 || window > MaxApprovalWindow {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "approval window is outside its bound")
	}
	if err := config.IdentityScheme.validate(); err != nil {
		return nil, err
	}
	if (config.Outbox == nil) != (config.Events == nil) {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"approval event outbox and publisher are configured together")
	}
	service := &ApprovalService{
		dispatch: config.Dispatch, store: config.Store,
		recorder: config.Recorder, narrowed: config.Narrowed,
		generations: config.Generations, mapping: config.ApproverMapping,
		window: window, scheme: cloneIdentityScheme(config.IdentityScheme),
		sweeper: config.Sweeper, terminal: config.TerminalStates,
	}
	if config.Outbox != nil {
		service.events = &approvalEvents{
			outbox: config.Outbox, publisher: config.Events,
		}
	}
	return service, nil
}

// approverMapping returns the approver mapping digest in force, or the zero
// digest when the host configures none.
func (s *ApprovalService) approverMapping(
	ctx context.Context,
) (auth.Digest, error) {
	if s.mapping == nil {
		return auth.Digest{}, nil
	}
	return s.mapping(ctx)
}

func approverMappingMoved() error {
	return shoal.WrapError(
		shoal.ErrorConflict,
		"the approver mapping a decision was made under is no longer in force",
		ErrApproverMappingMoved)
}

// ApprovalReceipt answers a request. State is where the request is; Action is
// the materialized record and is set only when State is ApprovalEnqueued.
type ApprovalReceipt struct {
	State            ApprovalState
	ID               []byte
	RequestDigest    []byte
	PolicyGeneration int64
	ExpiresAt        time.Time
	Action           ActionRecord
}

type ApprovalDecisionRequest struct {
	ID []byte
	// RequestDigest and PolicyGeneration are what the approver reviewed. Both
	// must match the record: an approval covers one exact request under one
	// policy generation, and an approver must not be able to approve a
	// request it has not seen.
	RequestDigest    []byte
	PolicyGeneration int64
	Verdict          ApprovalVerdict
	Context          RequestContext
}

// ApprovalCondition qualifies an approval's effective state with what a
// reader needs to know and the stored state alone does not say.
type ApprovalCondition string

const (
	// ApprovalConditionNone: the effective state is the whole answer.
	ApprovalConditionNone ApprovalCondition = ""
	// ApprovalConditionWindowClosed: stored as pending or approved, but the
	// window has closed. Expiry is written lazily, by the next request or
	// decision; Status reports it without writing it.
	ApprovalConditionWindowClosed ApprovalCondition = "window_closed"
	// ApprovalConditionTargetMoved: the agent's generation has moved, or the
	// agent is gone, since the request was made. The request names the old
	// generation and can never be decided or materialized again.
	ApprovalConditionTargetMoved ApprovalCondition = "target_moved"
	// ApprovalConditionPolicyMoved: the policy generation has moved since the
	// request was made. Neither the approver nor the requester can act on it.
	ApprovalConditionPolicyMoved ApprovalCondition = "policy_generation_moved"
	// ApprovalConditionApproverMappingMoved: approved, but the operator
	// approver mapping in force is not the one the approval was given under.
	// The approval can never materialize; a pending request is unaffected
	// until it is decided.
	ApprovalConditionApproverMappingMoved ApprovalCondition = "approver_mapping_moved"
	// ApprovalConditionIdentitySchemeMoved: pending or approved, but made
	// under an identity scheme other than the one in force (#526). Nobody
	// can decide or materialize it; it can only expire.
	ApprovalConditionIdentitySchemeMoved ApprovalCondition = "identity_scheme_moved"
	// ApprovalConditionDeadlinePassed: the request's action deadline has
	// passed, so it can no longer be re-requested and so never materialized.
	ApprovalConditionDeadlinePassed ApprovalCondition = "deadline_passed"
	// ApprovalConditionIdentityTaken: committed to become work, but the
	// action identity is occupied by something that is not this approval's
	// work — an action another route enqueued there between the
	// pre-commit check and the commit. The approval can never materialize.
	ApprovalConditionIdentityTaken ApprovalCondition = "identity_taken"
	// ApprovalConditionAwaitingAction: committed to become work (enqueued),
	// but the action has not been written yet — the second write was lost.
	// The requester's next re-request writes it while the target and the
	// deadline still hold.
	ApprovalConditionAwaitingAction ApprovalCondition = "enqueued_without_action"
)

// ApprovalUnresolvable is an effective state only, never stored: a pending,
// approved or enqueued-without-action request that can no longer progress,
// for the reason its Condition gives.
const ApprovalUnresolvable ApprovalState = "unresolvable"

// ApprovalStatus is what Status answers: the stored record, the state it is
// effectively in now, and why the two differ when they do.
type ApprovalStatus struct {
	Approval  ApprovalRecord
	State     ApprovalState
	Condition ApprovalCondition
}

type ApprovalStatusRequest struct {
	ID      []byte
	Context RequestContext
}

type PendingApprovalsRequest struct {
	After   []byte
	Limit   int
	Context RequestContext
}

type PendingApprovalsPage struct {
	Approvals []ApprovalRecord
	Next      []byte
}

// maxApprovalAttempts bounds the re-read loop a lost compare-and-set takes.
// A loss means somebody else moved the record; re-reading once is normally
// enough to see what they did, and the bound makes a pathological race an
// error rather than a spin.
const maxApprovalAttempts = 8

// errMaterializeRaced is internal: a materialization lost to a concurrent one
// that is not yet readable.
var errMaterializeRaced = errors.New("fleet approval: materialization raced")

// materializeRead classifies a failed read or write at the action identity
// during materialization. The durable store answers a read that lands while a
// concurrent write to the same identity is in flight with a not-found that is
// not ErrActionNotFound — the guard head exists and the cell is not yet
// committed — and a write that collides with one as a conflict. Both mean
// "look again shortly", not "this failed", and are retried by Request.
func materializeRead(err error) error {
	if (shoal.IsErrorCode(err, shoal.ErrorNotFound) &&
		!errors.Is(err, ErrActionNotFound)) ||
		shoal.IsErrorCode(err, shoal.ErrorConflict) {
		return errors.Join(errMaterializeRaced, err)
	}
	return err
}

// Request holds an approval-required action for a decision, or reports where
// a request already made has got to, or materializes an approved one.
//
// It is idempotent in the way enqueue is: the same request returns the same
// answer, and a changed one conflicts. A refused or expired request is a
// successful answer carrying that state, not an error — the caller asked
// where its request is, and that is where it is.
func (s *ApprovalService) Request(
	ctx context.Context, request EnqueueRequest,
) (ApprovalReceipt, error) {
	dispatch := s.dispatch
	ctx, cancel := dispatch.deadline(ctx, request.Context)
	defer cancel()
	decision, now, err := dispatch.begin(ctx, auth.OperationDispatch, request.Context)
	if err != nil {
		return ApprovalReceipt{}, err
	}
	if reservedAdmissionID(request.ID) {
		return ApprovalReceipt{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "action ID is reserved")
	}
	// Exactly the record an enqueue would build, under exactly the checks it
	// makes, so the approval route cannot admit an input the dispatch route
	// would refuse.
	base, action, err := dispatch.queuedRecord(
		ctx, decision, request, auth.OperationDispatch, now,
		false,
	)
	if err != nil {
		return ApprovalReceipt{}, err
	}
	if !action.RequiresApproval {
		return ApprovalReceipt{}, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"action does not require approval; enqueue it directly")
	}
	digest := ApprovalRequestDigest(base)
	for attempt := 0; attempt < maxApprovalAttempts; attempt++ {
		current, readErr := s.store.GetApproval(ctx, base.ID)
		if errors.Is(readErr, ErrApprovalNotFound) {
			receipt, err := s.hold(ctx, decision, base, digest, now)
			if errors.Is(err, ErrApprovalConflict) {
				continue
			}
			return receipt, err
		}
		if readErr != nil {
			return ApprovalReceipt{}, readErr
		}
		// The same request, field for field, or nothing. Everything an
		// approver reviewed is in the digest, and the principal, the
		// authorization fingerprint and the policy generation are in it too:
		// a requester whose policy generation has moved is making a different
		// request, and needs a new approval for it.
		if !equivalentEnqueue(current.Request, base) ||
			!bytes.Equal(current.RequestDigest, digest) {
			return ApprovalReceipt{}, approvalConflict()
		}
		// The time of this attempt, not of the request. A retry after a lost
		// compare-and-set or a back-off is later than the first attempt, and
		// using the first attempt's time would let the approved -> enqueued
		// commit land after ExpiresAt while believing it was inside the
		// window. hold keeps the request time, because the stored request's
		// CreatedAt is that time.
		attemptNow := now
		if attempt > 0 {
			if fresh := dispatch.clock().UTC(); fresh.After(attemptNow) {
				attemptNow = fresh
			}
		}
		receipt, err := s.advance(ctx, decision, current, attemptNow)
		if errors.Is(err, ErrApprovalConflict) {
			continue
		}
		if errors.Is(err, errMaterializeRaced) {
			// Jittered so callers that collided do not collide again in
			// lockstep. Bounded by the attempt count and the request context.
			wait := time.Duration(attempt+1)*2*time.Millisecond +
				time.Duration(rand.Int64N(int64(2*time.Millisecond)))
			select {
			case <-ctx.Done():
				return ApprovalReceipt{}, err
			case <-time.After(wait):
			}
			continue
		}
		return receipt, err
	}
	return ApprovalReceipt{}, approvalConflict()
}

// hold writes a new pending approval.
func (s *ApprovalService) hold(
	ctx context.Context,
	decision auth.Decision,
	base ActionRecord,
	digest []byte,
	now time.Time,
) (ApprovalReceipt, error) {
	// An action already at this identity is a conflict, as it is for an
	// enqueue: the identity is the caller's to choose and the materialized
	// record will be written there.
	if _, err := s.dispatch.store.GetAction(ctx, base.ID); err == nil {
		return ApprovalReceipt{}, ErrActionConflict
	} else if !errors.Is(err, ErrActionNotFound) {
		return ApprovalReceipt{}, err
	}
	expires := now.Add(s.window)
	if base.Deadline.Before(expires) {
		expires = base.Deadline
	}
	record := ApprovalRecord{
		ID: append([]byte(nil), base.ID...), Version: 1, State: ApprovalPending,
		Request: cloneActionRecord(base), RequestDigest: digest,
		PolicyGeneration: base.PolicyGeneration,
		RequestedAt:      now, ExpiresAt: expires, UpdatedAt: now,
		// Stamped once, here, from the requester's own decision; the store
		// refuses any later write that changes it.
		IdentityScheme: s.scheme.of(decision.Subject()),
	}
	stored, err := s.commit(
		ctx, "approval_request", auth.OperationDispatch, decision, record, 0)
	if err != nil {
		return ApprovalReceipt{}, err
	}
	return receiptFor(stored, ActionRecord{}), nil
}

// advance moves a request the requester already made as far as it can go.
func (s *ApprovalService) advance(
	ctx context.Context,
	decision auth.Decision,
	current ApprovalRecord,
	now time.Time,
) (ApprovalReceipt, error) {
	switch current.State {
	case ApprovalPending:
		if !now.Before(current.ExpiresAt) {
			expired, err := s.expire(
				ctx, auth.OperationDispatch, decision, current, now)
			if err != nil {
				return ApprovalReceipt{}, err
			}
			return receiptFor(expired, ActionRecord{}), nil
		}
		// No stranding here (#655). The target cannot have moved:
		// queuedRecord has just resolved it, pinned, at the request's
		// generation. And the policy can have moved only under a credential
		// minted before it did — equivalentEnqueue compares the generation —
		// whose writes the audit's generation guard refuses. Status, Decide
		// and the sweep record those.
		//
		// A replay: the requester asking where its request is, which is
		// also how it reconciles its own unpublished event (#653).
		return receiptFor(current, ActionRecord{}),
			s.drainApprovalEvents(ctx, current.ID, 0)
	case ApprovalRefused, ApprovalExpired, ApprovalWithdrawn, ApprovalStranded:
		return receiptFor(current, ActionRecord{}),
			s.drainApprovalEvents(ctx, current.ID, 0)
	case ApprovalApproved:
		// Approved and not used in time is expired, not approved. The window
		// bounds when work may begin to exist, not merely when an approver
		// may answer; otherwise an approval would be a standing permission
		// the requester could exercise at any later moment.
		if !now.Before(current.ExpiresAt) {
			expired, err := s.expire(
				ctx, auth.OperationDispatch, decision, current, now)
			if err != nil {
				return ApprovalReceipt{}, err
			}
			return receiptFor(expired, ActionRecord{}), nil
		}
		// Something already at the identity that is not this approval's work
		// — an action enqueued there by another route while the request was
		// held — means the approval can never become work. Found here, before
		// the commit, the request stays approved (and expires) instead of
		// becoming an enqueued approval that names work that does not exist.
		// A squat that lands between this read and the commit below is still
		// possible: the approval then commits enqueued, materialize finds
		// someone else's action and refuses, and Status reports it as
		// unresolvable with condition identity_taken.
		if existing, err := s.dispatch.store.GetAction(ctx, current.ID); err == nil {
			if _, replayErr := s.replayMaterialized(
				ctx, existing, current); replayErr != nil {
				return ApprovalReceipt{}, replayErr
			}
		} else if !errors.Is(err, ErrActionNotFound) {
			return ApprovalReceipt{}, materializeRead(err)
		}
		// The policy the approval was given under must still be the one in
		// force when it is committed to become work. The re-request's own
		// token proved its generation equals the request's, but a token can
		// be older than the policy, and the hosted recorder's interaction pin
		// is a deployment guard, not this service's: refusing a superseded
		// approval must not depend on which recorder is wired. The row stays
		// approved, and Status reports it as policy_generation_moved.
		inForce, err := s.generations.CurrentPolicyGeneration(
			ctx, decision.AuthorizationDomain())
		if err != nil {
			return ApprovalReceipt{}, err
		}
		// Not stranded here, for the reason the pending case gives: only a
		// credential older than the policy reaches this, and it cannot write.
		if inForce != current.PolicyGeneration {
			return ApprovalReceipt{}, shoal.WrapError(
				shoal.ErrorConflict,
				"the policy generation an approval was given under is no "+
					"longer in force", ErrApprovalSuperseded)
		}
		// The identity scheme first: under a moved scheme the requester and
		// the approver were never compared in one identity space, which makes
		// the mapping comparison below meaningless. The row stays approved,
		// and Status reports identity_scheme_moved.
		if s.scheme.Digest != current.IdentityScheme {
			return ApprovalReceipt{}, identitySchemeMoved()
		}
		// And so must the approver mapping. The approver was an approver
		// because a mapping said so; if the operator has changed it since,
		// that statement is no longer one the operator stands behind. The
		// row stays approved, and Status reports approver_mapping_moved.
		mapping, err := s.approverMapping(ctx)
		if err != nil {
			return ApprovalReceipt{}, err
		}
		if mapping != current.ApproverMappingDigest {
			return ApprovalReceipt{}, approverMappingMoved()
		}
		next := CloneApprovalRecord(current)
		next.Version++
		next.State = ApprovalEnqueued
		next.MaterializedAt = notBefore(now, current)
		next.UpdatedAt = next.MaterializedAt
		stored, err := s.commit(
			ctx, "approval_materialize", auth.OperationDispatch,
			decision, next, current.Version)
		// The approval has committed to become work whether or not its
		// event published, so the work is created either way and an
		// unpublished event is answered after it (#653). A retry replays at
		// the enqueued case and publishes the event there.
		eventErr := err
		if !errors.Is(err, ErrApprovalEventUnpublished) {
			eventErr = nil
			if err != nil {
				return ApprovalReceipt{}, err
			}
		}
		action, err := s.materialize(ctx, decision, stored)
		if err != nil {
			return ApprovalReceipt{}, err
		}
		return receiptFor(stored, action), eventErr
	case ApprovalEnqueued:
		// Past ExpiresAt is fine here and deliberate. The decision to make
		// this work was committed inside the window by the write above; a
		// crash between that write and the one below is recovered by the
		// requester re-requesting, and refusing then would turn a crash into
		// a lost approval. Recovery is bounded all the same, not by this
		// window but by whether a re-request can reach here at all: before
		// the action deadline (RequestContext refuses one after it) and while
		// the agent and policy generations hold (queuedRecord resolves the
		// request's generation). Past those the row stays enqueued with no
		// action, and Status reports it as unresolvable.
		action, err := s.materialize(ctx, decision, current)
		if err != nil {
			return ApprovalReceipt{}, err
		}
		return receiptFor(current, action),
			s.drainApprovalEvents(ctx, current.ID, 0)
	default:
		return ApprovalReceipt{}, shoal.NewError(
			shoal.ErrorInternal, "approval state is invalid")
	}
}

// materialize creates the ActionRecord an enqueued approval describes, or
// returns the one already created.
//
// It is the second of two writes and is safe to repeat. Everything that
// identifies the work comes from the stored approval rather than from this
// call, so a retry after a crash between the two writes builds the same
// request; only the provenance of the materializing call itself — which
// credential it ran under — is this call's.
func (s *ApprovalService) materialize(
	ctx context.Context,
	decision auth.Decision,
	approval ApprovalRecord,
) (ActionRecord, error) {
	dispatch := s.dispatch
	record := cloneActionRecord(approval.Request)
	record.ApprovalRequestDigest = append([]byte(nil), approval.RequestDigest...)
	record.ApprovalPolicyGeneration = approval.ApproverPolicyGeneration
	record.ApproverSubject = approval.ApproverSubject
	record.ApproverActor = approval.ApproverActor
	record.ApproverClientID = approval.ApproverClientID
	record.ApprovedAt = approval.DecidedAt
	record.UpdatedAt = approval.MaterializedAt
	// The credential that commits the record. The request's own credential
	// will normally have expired by the time an approval arrives, and the
	// audit and publication paths pin the record to a live one; the
	// fingerprint is unchanged, because equivalentEnqueue has just proven
	// this caller's fingerprint is the request's.
	record.AuthorizationExpiresAt = decision.AuthenticationExpires()
	record.TransitionRequestID = decision.RequestID()
	record.TransitionCorrelationID = decision.CorrelationID()
	if err := record.Validate(); err != nil {
		return ActionRecord{}, err
	}
	current, readErr := dispatch.store.GetAction(ctx, record.ID)
	if readErr == nil {
		return s.replayMaterialized(ctx, current, approval)
	}
	if !errors.Is(readErr, ErrActionNotFound) {
		return ActionRecord{}, materializeRead(readErr)
	}
	if err := dispatch.recorder.RecordAction(ctx, ActionAudit{
		Phase: "approval_enqueue", Operation: auth.OperationDispatch, Record: record,
	}); err != nil {
		return ActionRecord{}, errors.Join(ErrRecordingUnavailable, err)
	}
	stored, err := dispatch.store.ApplyAction(ctx, DispatchMutation{
		Token: transitionToken(
			"approval-enqueue", record.ID, record.IdempotencyKey, record.Version),
		ExpectedVersion: 0, TransitionKind: "action.enqueued", Record: record,
	})
	if err != nil {
		// A concurrent retry of this same materialization may have won; it
		// wrote the same work under a different credential, and that is this
		// request's record. Anything else at the identity is not, and
		// replayMaterialized says so.
		//
		// Re-read on any failure, not only on the fleet sentinel: the durable
		// store reports a lost first write through the coordination guard as
		// a shoal conflict ("fleet registry conflict"), which is not
		// ErrActionConflict, so matching the sentinel alone sent concurrent
		// re-requests of one approval home with a conflict for work that had
		// in fact been created. The read decides, not the error.
		if current, readErr := dispatch.store.GetAction(
			context.WithoutCancel(ctx), record.ID); readErr == nil {
			return s.replayMaterialized(ctx, current, approval)
		}
		// A conflict with nothing yet readable is a concurrent
		// materialization still landing, or several that collided and all
		// lost. Either way the next attempt reads what is there, so the
		// caller retries rather than reporting a conflict for work that is
		// about to exist.
		if shoal.IsErrorCode(err, shoal.ErrorConflict) {
			return ActionRecord{}, errors.Join(errMaterializeRaced, err)
		}
		return ActionRecord{}, materializeRead(err)
	}
	if err := dispatch.publishTransition(
		context.WithoutCancel(ctx), "action.enqueued", stored,
	); err != nil {
		return ActionRecord{}, errors.Join(ErrActionCommitted, err)
	}
	return cloneActionRecord(stored), nil
}

// replayMaterialized returns an existing ActionRecord if and only if it is the
// one this approval materialized, wherever its lifecycle has since got to.
func (s *ApprovalService) replayMaterialized(
	ctx context.Context, current ActionRecord, approval ApprovalRecord,
) (ActionRecord, error) {
	if !isMaterializationOf(current, approval) {
		// Something else is at the identity: an action enqueued there by
		// another route while this request was held. The approval cannot
		// become work at an identity it does not own.
		return ActionRecord{}, ErrActionConflict
	}
	if err := s.dispatch.publishTransition(
		context.WithoutCancel(ctx), actionEventKind(current), current,
	); err != nil {
		return ActionRecord{}, errors.Join(ErrActionCommitted, err)
	}
	// Redacted like every other read (#369). A re-request whose work has
	// already run hands the requester a terminal record, and the evidence on
	// it belongs to whoever executed rather than to whoever asked.
	return s.dispatch.readableRecord(ctx, cloneActionRecord(current))
}

// isMaterializationOf reports whether an action is the work this approval
// materialized: the same request, under this approval's digest, approver and
// decision time.
func isMaterializationOf(action ActionRecord, approval ApprovalRecord) bool {
	return equivalentEnqueue(action, approval.Request) &&
		bytes.Equal(action.ApprovalRequestDigest, approval.RequestDigest) &&
		action.ApproverSubject == approval.ApproverSubject &&
		action.ApproverActor == approval.ApproverActor &&
		action.ApproverClientID == approval.ApproverClientID &&
		action.ApprovedAt.Equal(approval.DecidedAt)
}

// expire records that a request closed without becoming work.
func (s *ApprovalService) expire(
	ctx context.Context,
	operation auth.Operation,
	decision auth.Decision,
	current ApprovalRecord,
	now time.Time,
) (ApprovalRecord, error) {
	next := CloneApprovalRecord(current)
	next.Version++
	next.State = ApprovalExpired
	next.UpdatedAt = notBefore(now, current)
	return s.commit(ctx, "approval_expiry", operation, decision, next, current.Version)
}

// strand records that a request can never progress, for a permanent cause
// approvalStrandable established (#655).
func (s *ApprovalService) strand(
	ctx context.Context,
	operation auth.Operation,
	decision auth.Decision,
	current ApprovalRecord,
	cause ApprovalCondition,
	now time.Time,
) (ApprovalRecord, error) {
	next := CloneApprovalRecord(current)
	next.Version++
	next.State = ApprovalStranded
	next.StrandedCause = cause
	next.UpdatedAt = notBefore(now, current)
	return s.commit(ctx, "approval_strand", operation, decision, next, current.Version)
}

// settle writes the terminal state approvalStrandable chose.
func (s *ApprovalService) settle(
	ctx context.Context,
	operation auth.Operation,
	decision auth.Decision,
	current ApprovalRecord,
	state ApprovalState,
	cause ApprovalCondition,
	now time.Time,
) (ApprovalRecord, error) {
	if state == ApprovalExpired {
		return s.expire(ctx, operation, decision, current, now)
	}
	return s.strand(ctx, operation, decision, current, cause, now)
}

// approvalStrandable decides whether a request that is not yet final must
// now be recorded as final, and as what (#655). It answers ("", "") when the
// request may still progress, or when the evidence that it cannot is not
// conclusive.
//
// The rule is that only a durable fact no later event can undo is a reason
// to write a terminal state. A terminal state cannot be left, so writing one
// on weaker evidence — a read that failed, a replica whose configuration is
// behind, a lease that a heartbeat would renew — could end a request that
// would have lived. Weaker evidence waits for the window, which closes for
// every pending or approved request at or before its deadline.
//
//   - Pending or approved, window closed: expired, as every observer writes.
//   - Pending or approved, target moved: the agent's registry row, read
//     successfully, names a higher generation than the request, or is
//     revoked. The request's own rebuild is pinned to its generation
//     (queuedRecord), and generations only grow, so the requester can never
//     re-request it. A stale read can only show an older generation, never a
//     newer one, so it cannot produce this answer. A failed or absent read
//     and a lapsed lease are not evidence.
//   - Pending or approved, policy moved: the generation authority's
//     generation is greater than the request's. Greater, not different: a
//     replica still on an older configuration during a rollout sees a newer
//     request's generation as ahead of its own, and must not strand it.
//   - Enqueued, identity taken: an action exists at the identity and is not
//     this approval's materialization, so materialize can never write there.
//   - Enqueued, no action, past the deadline by ApprovalStrandGrace: no call
//     that could still write the action can be running; see
//     ApprovalStrandGrace. Before that, a moved target or policy is left
//     alone, because a re-request already past its checks may still be
//     landing the action.
//
// A moved approver mapping or identity scheme is deliberately not a cause.
// Replicas legitimately disagree on both during a rollout, so one replica
// would be stranding approvals another honours. Those requests expire.
func (s *ApprovalService) approvalStrandable(
	ctx context.Context, current ApprovalRecord, now time.Time,
) (ApprovalState, ApprovalCondition, error) {
	switch current.State {
	case ApprovalPending, ApprovalApproved:
		if !now.Before(current.ExpiresAt) {
			return ApprovalExpired, ApprovalConditionNone, nil
		}
		if s.dispatch.registry == nil || s.dispatch.registry.store == nil {
			// A composition with no registry has no evidence to offer.
			return "", "", nil
		}
		stored, err := s.dispatch.registry.store.Get(ctx, current.Request.AgentID)
		if shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			// Absent, or a committed value not yet visible: the store does
			// not tell them apart, so neither is evidence.
			return "", "", nil
		}
		if err != nil {
			return "", "", err
		}
		descriptor := stored.Descriptor
		if descriptor.Generation > current.Request.AgentGeneration ||
			(descriptor.Generation == current.Request.AgentGeneration &&
				!descriptor.RevokedAt.IsZero()) {
			return ApprovalStranded, ApprovalConditionTargetMoved, nil
		}
		if descriptor.Generation != current.Request.AgentGeneration {
			// Older than the request: a read that has not caught up. Not
			// evidence of anything.
			return "", "", nil
		}
		inForce, err := s.generations.CurrentPolicyGeneration(
			ctx, descriptor.AuthorizationDomain)
		if err != nil {
			return "", "", err
		}
		if inForce > current.PolicyGeneration {
			return ApprovalStranded, ApprovalConditionPolicyMoved, nil
		}
		return "", "", nil
	case ApprovalEnqueued:
		action, err := s.dispatch.store.GetAction(ctx, current.ID)
		if err == nil {
			if !isMaterializationOf(action, current) {
				return ApprovalStranded, ApprovalConditionIdentityTaken, nil
			}
			return "", "", nil
		}
		if !errors.Is(err, ErrActionNotFound) {
			return "", "", err
		}
		if !now.Before(current.Request.Deadline.Add(ApprovalStrandGrace)) {
			return ApprovalStranded, ApprovalConditionDeadlinePassed, nil
		}
		return "", "", nil
	default:
		return "", "", nil
	}
}

// settleObserved writes, as the calling party, the terminal state
// approvalStrandable chose for a record that party has reached (#655): the
// lazy half of resolving stranded rows, published by its maker as #653's
// expiry is.
//
// It is opportunistic. settled is true only when a write committed (with or
// without its event) or lost a compare-and-set, which the caller answers as
// it answers any race. Every other failure, including no evidence and a write
// the caller's own credential could not make (one minted before the policy
// moved fails its audit's generation guard), leaves the record as it was and
// the caller answering exactly as it did before stranding existed. The sweep
// writes what a party could not.
func (s *ApprovalService) settleObserved(
	ctx context.Context,
	operation auth.Operation,
	decision auth.Decision,
	current ApprovalRecord,
	now time.Time,
) (ApprovalRecord, bool, error) {
	// Off, Status and Decide answer exactly as before #655 and write
	// nothing a party did not already write (TerminalStates).
	if !s.terminal {
		return ApprovalRecord{}, false, nil
	}
	state, cause, err := s.approvalStrandable(ctx, current, now)
	if err != nil || state == "" {
		return ApprovalRecord{}, false, nil
	}
	settled, err := s.settle(ctx, operation, decision, current, state, cause, now)
	switch {
	case err == nil:
		return settled, true, nil
	case errors.Is(err, ErrApprovalEventUnpublished):
		return settled, true, err
	case errors.Is(err, ErrApprovalConflict):
		return ApprovalRecord{}, true, err
	default:
		return ApprovalRecord{}, false, nil
	}
}

func approvalStranded(cause ApprovalCondition) error {
	sentinel := ErrApprovalStranded
	if cause == ApprovalConditionPolicyMoved {
		// Still a superseded generation, which is what callers that predate
		// stranding branch on.
		sentinel = errors.Join(ErrApprovalStranded, ErrApprovalSuperseded)
	}
	return shoal.WrapError(
		shoal.ErrorConflict,
		"the request can no longer progress ("+string(cause)+")", sentinel)
}

func terminalStatesDisabled() error {
	return shoal.WrapError(
		shoal.ErrorConflict,
		"withdrawal and stranding are not enabled on this host",
		ErrApprovalTerminalStatesDisabled)
}

func approvalWithdrawn() error {
	return shoal.WrapError(
		shoal.ErrorConflict, "the request has been withdrawn", ErrApprovalWithdrawn)
}

// Decide approves or refuses a pending request.
//
// The order of the checks is the order of the questions. May this caller
// decide this request at all — approve on its scope, the target still
// resolving, and independent of everyone the request involves. Then has the
// window closed, which is recorded as expiry rather than answered as a
// refusal. Then is the record still undecided, where a decision identical to
// the one already made replays and any other conflicts. Only then is what the
// approver reviewed compared with what is on the record.
func (s *ApprovalService) Decide(
	ctx context.Context, request ApprovalDecisionRequest,
) (ApprovalRecord, error) {
	dispatch := s.dispatch
	ctx, cancel := dispatch.deadline(ctx, request.Context)
	defer cancel()
	decision, now, err := dispatch.begin(
		ctx, auth.OperationActionApprove, request.Context)
	if err != nil {
		return ApprovalRecord{}, err
	}
	if err := validateOpaque("approval ID", request.ID, false); err != nil {
		return ApprovalRecord{}, err
	}
	if len(request.RequestDigest) != sha256.Size {
		return ApprovalRecord{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "approval request digest is not a digest")
	}
	if request.PolicyGeneration <= 0 {
		return ApprovalRecord{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "approval policy generation is invalid")
	}
	if err := request.Verdict.validate(); err != nil {
		return ApprovalRecord{}, err
	}
	for attempt := 0; attempt < maxApprovalAttempts; attempt++ {
		current, err := s.store.GetApproval(ctx, request.ID)
		if err != nil {
			if errors.Is(err, ErrApprovalNotFound) {
				return ApprovalRecord{}, auth.ObjectNotFound()
			}
			return ApprovalRecord{}, err
		}
		if _, err := s.eligibleApprover(ctx, decision, current, now); err != nil {
			return ApprovalRecord{}, err
		}
		result, err := s.decide(ctx, decision, current, request, now)
		if errors.Is(err, errApprovalRaced) {
			continue
		}
		return result, err
	}
	return ApprovalRecord{}, approvalConflict()
}

// ApprovalWithdrawRequest asks for a held request to be withdrawn (#655).
// The context's reason is recorded as the withdrawal's.
type ApprovalWithdrawRequest struct {
	ID      []byte
	Context RequestContext
}

// Withdraw withdraws a pending request, by its requester (#655).
//
// Only the requester, by Status's rule: the request's own principal, still
// holding dispatch on its scope. Anyone else, including every approver, is
// answered as for an approval that does not exist; an approver holds no
// dispatch and is refused by begin before any record is read, so the refusal
// says nothing about which IDs exist.
//
// The withdrawal is a compare-and-set from pending, at the version the
// request was read at, so a decision made concurrently either lands first —
// and the withdrawal then conflicts with it — or loses to the withdrawal and
// is refused as ErrApprovalWithdrawn. Never both. A request whose window has
// closed is expired, as a decision finds it. A withdrawn request replays, so
// a requester whose response was lost can send it again, and that replay is
// how it publishes its own withdrawal event if the first publication failed.
func (s *ApprovalService) Withdraw(
	ctx context.Context, request ApprovalWithdrawRequest,
) (ApprovalRecord, error) {
	dispatch := s.dispatch
	ctx, cancel := dispatch.deadline(ctx, request.Context)
	defer cancel()
	decision, now, err := dispatch.begin(ctx, auth.OperationDispatch, request.Context)
	if err != nil {
		return ApprovalRecord{}, err
	}
	if err := validateOpaque("approval ID", request.ID, false); err != nil {
		return ApprovalRecord{}, err
	}
	// Refused before any record is read, so the answer is the host's and
	// says nothing about which approval IDs exist.
	if !s.terminal {
		return ApprovalRecord{}, terminalStatesDisabled()
	}
	reason, err := interaction.NewReason(
		request.Context.ReasonCode, request.Context.ReasonDetail)
	if err != nil {
		return ApprovalRecord{}, err
	}
	for attempt := 0; attempt < maxApprovalAttempts; attempt++ {
		current, err := s.store.GetApproval(ctx, request.ID)
		if err != nil {
			if errors.Is(err, ErrApprovalNotFound) {
				return ApprovalRecord{}, auth.ObjectNotFound()
			}
			return ApprovalRecord{}, err
		}
		if !requestedBy(decision, current, now) {
			return ApprovalRecord{}, auth.ObjectNotFound()
		}
		attemptNow := now
		if attempt > 0 {
			if fresh := dispatch.clock().UTC(); fresh.After(attemptNow) {
				attemptNow = fresh
			}
		}
		result, err := s.withdraw(ctx, decision, current, reason, attemptNow)
		if errors.Is(err, errApprovalRaced) {
			continue
		}
		return result, err
	}
	return ApprovalRecord{}, approvalConflict()
}

func (s *ApprovalService) withdraw(
	ctx context.Context,
	decision auth.Decision,
	current ApprovalRecord,
	reason interaction.Reason,
	now time.Time,
) (ApprovalRecord, error) {
	switch current.State {
	case ApprovalPending:
		if !now.Before(current.ExpiresAt) {
			if _, err := s.expire(
				ctx, auth.OperationDispatch, decision, current, now,
			); err != nil {
				if errors.Is(err, ErrApprovalConflict) {
					return ApprovalRecord{}, errApprovalRaced
				}
				return ApprovalRecord{}, err
			}
			return ApprovalRecord{}, approvalExpired()
		}
		next := CloneApprovalRecord(current)
		next.Version++
		next.State = ApprovalWithdrawn
		next.WithdrawnAt = notBefore(now, current)
		next.UpdatedAt = next.WithdrawnAt
		next.WithdrawalReason = reason
		stored, err := s.commit(
			ctx, "approval_withdrawal", auth.OperationDispatch,
			decision, next, current.Version)
		if errors.Is(err, ErrApprovalConflict) {
			return ApprovalRecord{}, errApprovalRaced
		}
		return stored, err
	case ApprovalWithdrawn:
		// A replay, which answers for the requester's own unpublished
		// withdrawal event as a re-request does (#653).
		if err := s.drainApprovalEvents(ctx, current.ID, 0); err != nil {
			return ApprovalRecord{}, err
		}
		return CloneApprovalRecord(current), nil
	}
	// Decided, expired, materialized or stranded: nothing to withdraw.
	switch current.State {
	case ApprovalExpired:
		// A withdrawal that found the window closed wrote this expiry and
		// may have answered that its event did not publish; this is the
		// retry it asked for, so it answers for the requester's own rows.
		if err := s.drainApprovalEvents(ctx, current.ID, 0); err != nil {
			return ApprovalRecord{}, err
		}
		return ApprovalRecord{}, approvalExpired()
	case ApprovalStranded:
		s.drainApprovalEventsQuietly(ctx, current.ID)
		return ApprovalRecord{}, approvalStranded(current.StrandedCause)
	default:
		// The requester's own rows are published quietly; this is not the
		// call that owes them.
		s.drainApprovalEventsQuietly(ctx, current.ID)
		return ApprovalRecord{}, shoal.WrapError(
			shoal.ErrorConflict,
			"the request has already been decided and cannot be withdrawn",
			ErrApprovalConflict)
	}
}

// requestedBy is the requester rule Status, VisibleTo and Withdraw share:
// the request's own principal, still holding dispatch on its scope.
func requestedBy(
	decision auth.Decision, current ApprovalRecord, now time.Time,
) bool {
	return sameActionPrincipal(decision, current.Request) &&
		decision.AuthorizeObject(auth.OperationDispatch, auth.ResourceRequest{
			AuthorizationDomain: decision.AuthorizationDomain(),
			SourceID:            current.Request.SourceID,
			PolicyID:            current.Request.PolicyID,
			ObjectID:            shoal.ID(current.ID),
		}, now) == nil
}

// notBefore clamps a transition time to the record's latest, so a replica
// whose clock is behind the one that wrote the record still writes a record
// whose timestamps are ordered.
//
// The clamp moves time forward, never back, and only by the skew between two
// replicas. It never reaches past ExpiresAt: every caller has already checked
// now < ExpiresAt, and every timestamp on the record is also before it.
// Refusing instead — which Validate did — turned a few seconds of skew into a
// 400 for an approver that had done nothing wrong.
func notBefore(now time.Time, record ApprovalRecord) time.Time {
	for _, earlier := range []time.Time{
		record.RequestedAt, record.UpdatedAt, record.DecidedAt,
		record.MaterializedAt,
	} {
		if now.Before(earlier) {
			now = earlier
		}
	}
	return now
}

// errApprovalRaced is internal: the record moved under a compare-and-set and
// the decision has to be re-evaluated against what is there now.
var errApprovalRaced = errors.New("fleet approval: record moved")

func (s *ApprovalService) decide(
	ctx context.Context,
	decision auth.Decision,
	current ApprovalRecord,
	request ApprovalDecisionRequest,
	now time.Time,
) (ApprovalRecord, error) {
	if current.State == ApprovalPending && !now.Before(current.ExpiresAt) {
		if _, err := s.expire(
			ctx, auth.OperationActionApprove, decision, current, now,
		); err != nil {
			if errors.Is(err, ErrApprovalConflict) {
				return ApprovalRecord{}, errApprovalRaced
			}
			return ApprovalRecord{}, err
		}
		return ApprovalRecord{}, approvalExpired()
	}
	if current.State != ApprovalPending {
		// A replay of any kind publishes this approver's own pending events
		// first: a duplicate decision is how an approver reconciles a
		// decision whose event did not publish, and a decide that finds the
		// request expired is how one reconciles an expiry it wrote (#653).
		// Only rows this caller published are touched, and this caller has
		// already passed eligibility.
		if err := s.drainApprovalEvents(ctx, current.ID, 0); err != nil {
			return ApprovalRecord{}, err
		}
		// A duplicate of the decision already on the record replays it, so an
		// approver whose response was lost can safely send it again. It does
		// not perform anything a second time because deciding never performs
		// anything; only the requester's re-request materializes, once.
		if sameApprovalDecision(decision, current, request) {
			return CloneApprovalRecord(current), nil
		}
		switch current.State {
		case ApprovalExpired:
			return ApprovalRecord{}, approvalExpired()
		case ApprovalWithdrawn:
			return ApprovalRecord{}, approvalWithdrawn()
		case ApprovalStranded:
			return ApprovalRecord{}, approvalStranded(current.StrandedCause)
		}
		return ApprovalRecord{}, approvalConflict()
	}
	// A pending request whose target or policy has moved for good can never
	// become work, whatever this approver decides; the approver observed
	// it, so it records it (#655). Opportunistic: an approver whose own
	// credential cannot make the write is answered as before, below.
	if stranded, settled, err := s.settleObserved(
		ctx, auth.OperationActionApprove, decision, current, now,
	); settled {
		if errors.Is(err, ErrApprovalConflict) {
			return ApprovalRecord{}, errApprovalRaced
		}
		if err != nil {
			return ApprovalRecord{}, err
		}
		return ApprovalRecord{}, approvalStranded(stranded.StrandedCause)
	}
	// The generation the approver reviewed under, and the one it holds now,
	// must both be the request's. An approval under a superseded generation
	// would be a statement about a policy that is no longer in force.
	inForce, err := s.generations.CurrentPolicyGeneration(
		ctx, decision.AuthorizationDomain())
	if err != nil {
		return ApprovalRecord{}, err
	}
	if request.PolicyGeneration != current.PolicyGeneration ||
		decision.PolicyGeneration() != current.PolicyGeneration ||
		inForce != current.PolicyGeneration {
		return ApprovalRecord{}, shoal.WrapError(
			shoal.ErrorConflict,
			"approval policy generation does not match the request",
			ErrApprovalSuperseded)
	}
	// The approver must have been made one under the mapping now in force.
	// A credential minted under an earlier mapping is a statement the
	// operator has since withdrawn or changed. Where no mapping is
	// configured both are zero, and a decision without provenance passes;
	// where one is, a decision without provenance does not.
	mapping, err := s.approverMapping(ctx)
	if err != nil {
		return ApprovalRecord{}, err
	}
	provenance := decision.GrantProvenance()
	if provenance.MappingDigest != mapping {
		return ApprovalRecord{}, approverMappingMoved()
	}
	if !bytes.Equal(request.RequestDigest, current.RequestDigest) {
		return ApprovalRecord{}, approvalConflict()
	}
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		return ApprovalRecord{}, err
	}
	next := CloneApprovalRecord(current)
	next.Version++
	next.State = ApprovalApproved
	if request.Verdict == ApprovalVerdictRefuse {
		next.State = ApprovalRefused
	}
	next.Verdict = request.Verdict
	next.ApproverSubject = decision.Subject()
	next.ApproverActor = decision.Actor()
	next.ApproverClientID = decision.ClientID()
	next.ApproverFingerprint = fingerprint
	next.ApproverPolicyGeneration = decision.PolicyGeneration()
	next.DecidedAt = notBefore(now, current)
	next.DecisionRequestID = decision.RequestID()
	next.DecisionCorrelationID = decision.CorrelationID()
	next.ApproverMappingDigest = provenance.MappingDigest
	next.ApproverProvenance = provenance
	next.UpdatedAt = next.DecidedAt
	stored, err := s.commit(
		ctx, "approval_decision", auth.OperationActionApprove,
		decision, next, current.Version)
	if errors.Is(err, ErrApprovalConflict) {
		return ApprovalRecord{}, errApprovalRaced
	}
	return stored, err
}

// sameApprovalDecision reports whether a decided record is this decision by
// this approver, already made.
func sameApprovalDecision(
	decision auth.Decision,
	current ApprovalRecord,
	request ApprovalDecisionRequest,
) bool {
	return current.Verdict == request.Verdict &&
		current.ApproverSubject == decision.Subject() &&
		current.ApproverActor == decision.Actor() &&
		current.ApproverClientID == decision.ClientID() &&
		bytes.Equal(current.RequestDigest, request.RequestDigest)
}

// eligibleApprover decides whether this caller may decide this request.
//
// Three conditions, and each closes a different way to approve your own work.
//
// Approve must be authorized on the request's scope, and the request's target
// must still resolve under it at the generation it names. Both failures answer
// as an absent record does: a caller that may not decide a request is not told
// it exists.
//
// The approver must share no identity with the request: not with the
// requester's subject, actor or delegation chain, and not with the agent that
// would perform it or the principal that registered it. Equality is across
// fields, as in internal/decisionadjudication: acting as somebody's delegate,
// or changing only the client, does not establish independence from them. A
// decision carrying a delegation chain is refused outright — an approval is a
// human's own judgement, and delegation cannot manufacture a second role.
//
// And the approver must fail the operations that create or perform this work
// on the same scope. Holding approve alone separates nothing where scopes are
// shared, which they are for every OIDC-minted principal: two people with
// dispatch and approve on one workspace could each approve the other's request
// and then enqueue anything they liked by trading approvals. Execute is
// included as well as dispatch and invoke, because an approver that may claim
// and perform the work it approved has not been separated from the effect.
func (s *ApprovalService) eligibleApprover(
	ctx context.Context,
	decision auth.Decision,
	record ApprovalRecord,
	now time.Time,
) (Descriptor, error) {
	descriptor, err := s.eligibility(ctx, decision, record, now)
	if !s.narrowed(ctx) {
		return descriptor, err
	}
	// A narrowed caller is refused — but the refusal is only told to a caller
	// that would otherwise be eligible on this record. Refusing every narrowed
	// caller first, as an earlier version did, answered "unauthorized" for an
	// approval ID that exists in any scope and "not found" for one that does
	// not: an existence oracle over every held request. Every other failure
	// therefore answers exactly as an absent record does.
	if err != nil {
		if shoal.IsErrorCode(err, shoal.ErrorUnauthorized) ||
			shoal.IsErrorCode(err, shoal.ErrorNotFound) ||
			errors.Is(err, ErrIdentitySchemeMoved) {
			return Descriptor{}, auth.ObjectNotFound()
		}
		return Descriptor{}, err
	}
	return Descriptor{}, approvalUnderNarrowing()
}

// eligibility is every approver rule except the narrowing one; see
// eligibleApprover, which is the only caller.
func (s *ApprovalService) eligibility(
	ctx context.Context,
	decision auth.Decision,
	record ApprovalRecord,
	now time.Time,
) (Descriptor, error) {
	request := record.Request
	if err := decision.AuthorizeObject(
		auth.OperationActionApprove, auth.ResourceRequest{
			AuthorizationDomain: decision.AuthorizationDomain(),
			SourceID:            request.SourceID,
			PolicyID:            request.PolicyID,
			ObjectID:            shoal.ID(record.ID),
		}, now); err != nil {
		return Descriptor{}, auth.ObjectNotFound()
	}
	descriptor, _, _, err := s.dispatch.registry.resolveActionBinding(
		ctx, decision, request.AgentID, request.AgentGeneration,
		request.Capability, request.Action, request.SourceID, request.PolicyID,
		request.ObjectID, auth.OperationActionApprove, now,
		// Unpinned. The request was made earlier and the approver is deciding
		// now, so the descriptor will have moved for every agent that
		// heartbeats — which would make an approval undecidable for exactly
		// the long-lived agents approvals exist for. See
		// resolveActionBinding.
		false,
	)
	if err != nil {
		if shoal.IsErrorCode(err, shoal.ErrorUnauthorized) ||
			shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			return Descriptor{}, auth.ObjectNotFound()
		}
		return Descriptor{}, err
	}
	if len(decision.OnBehalfOf()) > 0 {
		return Descriptor{}, shoal.NewError(
			shoal.ErrorUnauthorized,
			"an approval cannot be made on behalf of another identity")
	}
	// The approver and the request must be named under one identity scheme
	// (#526), or the identity comparison below compares two unrelated name
	// spaces and proves nothing. An empty stamp is a record written before
	// schemes existed, which is the legacy scheme, so a host that has not
	// switched decides every old request exactly as before.
	if s.scheme.of(decision.Subject()) != record.IdentityScheme {
		return Descriptor{}, identitySchemeMoved()
	}
	involved := map[shoal.ID]struct{}{
		request.Subject: {}, request.Actor: {},
		request.AgentID: {}, descriptor.Subject: {}, descriptor.Actor: {},
	}
	for _, identity := range request.OnBehalfOf {
		involved[identity] = struct{}{}
	}
	// And every ancestor in the agent's delegation chain: its ID and the
	// principal that registered it. A delegated agent acts with authority its
	// parent granted, so the parent agent and the parent's registrant are as
	// much a party to the work as the child's own registrant is. The chain is
	// read from the registry rather than from the request, and a chain that
	// cannot be read refuses rather than passes.
	chain, err := s.dispatch.registry.activeChain(ctx, descriptor.ID, now)
	if err != nil || len(chain) == 0 || chain[0].Generation != descriptor.Generation {
		return Descriptor{}, auth.ObjectNotFound()
	}
	for _, ancestor := range chain {
		involved[ancestor.ID] = struct{}{}
		involved[ancestor.Subject] = struct{}{}
		involved[ancestor.Actor] = struct{}{}
	}
	// Within this host's identity family, only the namespace in force is
	// comparable (#526). An identity minted under any other scheme — the
	// sub-derived one, another stable claim path, legacy Entra — may be this
	// approver under another name: the human who registered an agent as
	// oidc:<iss>#<sub> may approve as oidcid:, and one who registered under
	// one stable claim may approve under another. That holds under every
	// scheme, the default one included, and across a mixed rollout, without
	// listing which schemes came before. The approver's own identity is
	// checked too. Refuse, naming the namespace and never the identity.
	// Adoption (#526 PR2) moves a registration into the namespace in force,
	// which is how approvals for it resume.
	checked := make([]shoal.ID, 0, len(involved)+2)
	for identity := range involved {
		checked = append(checked, identity)
	}
	checked = append(checked, decision.Subject(), decision.Actor())
	if namespace := s.scheme.foreignNamespace(checked); namespace != "" {
		return Descriptor{}, shoal.NewError(
			shoal.ErrorUnauthorized,
			"an identity involved in the request is in namespace "+namespace+
				", not the namespace in force "+s.scheme.Prefix+"; it must be "+
				"adopted into the identity scheme in force before the request "+
				"can be approved")
	}
	// Read this loop as one comparison for an OIDC approver, not two. A
	// mapped approver's decision is minted with actor = subject
	// (oidc:<iss>#<sub>), so both iterations test the same identity. That is
	// not a weakening: before #451 slice 2 every OIDC token's actor was the
	// literal shoal-explore-web-oidc, a constant no principal controlled, and
	// it made this check fire on every OIDC requester/approver pair — no
	// OIDC approver could approve any OIDC request (failing closed, under a
	// refusal that misdescribed the cause). The separation margin for an
	// OIDC approver is exactly "the subject must not overlap anyone
	// involved".
	for _, identity := range []shoal.ID{decision.Subject(), decision.Actor()} {
		if _, overlaps := involved[identity]; overlaps {
			return Descriptor{}, shoal.NewError(
				shoal.ErrorUnauthorized,
				"approver is not independent of the request")
		}
	}
	resource := auth.ResourceRequest{
		AuthorizationDomain: decision.AuthorizationDomain(),
		SourceID:            request.SourceID,
		PolicyID:            request.PolicyID,
		ObjectID:            request.ObjectID,
	}
	for _, operation := range []auth.Operation{
		auth.OperationDispatch, auth.OperationInvoke, auth.OperationExecute,
	} {
		if decision.AuthorizeObject(operation, resource, now) == nil {
			return Descriptor{}, shoal.NewError(
				shoal.ErrorUnauthorized,
				"approver may also create or perform the work it would approve")
		}
	}
	return descriptor, nil
}

// Status returns one approval to the requester or to a principal eligible to
// decide it, and answers everyone else as though it did not exist.
//
// It reports the state the request is effectively in, not merely the one
// stored. Expiry is written lazily, and a request whose target or policy
// generation has moved, or whose deadline has passed, can never progress
// although its row still says pending or approved; reporting the row would
// tell a reader something live that is not. What it reports as final for a
// permanent reason it also records, as the asking party (#655); see
// settleObserved.
func (s *ApprovalService) Status(
	ctx context.Context, request ApprovalStatusRequest,
) (ApprovalStatus, error) {
	dispatch := s.dispatch
	ctx, cancel := dispatch.deadline(ctx, request.Context)
	defer cancel()
	decision, now, err := dispatch.begin(ctx, auth.OperationDispatch, request.Context)
	if err != nil {
		if !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
			return ApprovalStatus{}, err
		}
		decision, now, err = dispatch.begin(
			ctx, auth.OperationActionApprove, request.Context)
		if err != nil {
			return ApprovalStatus{}, err
		}
	}
	if err := validateOpaque("approval ID", request.ID, false); err != nil {
		return ApprovalStatus{}, err
	}
	current, err := s.store.GetApproval(ctx, request.ID)
	if err != nil {
		if errors.Is(err, ErrApprovalNotFound) {
			return ApprovalStatus{}, auth.ObjectNotFound()
		}
		return ApprovalStatus{}, err
	}
	requester := requestedBy(decision, current, now)
	if !requester {
		// The approver path. Eligibility resolves the agent unpinned (a
		// heartbeat moves its generation), so an approver still sees a
		// request whose target generation moved — and records it as
		// stranded below (#655) — and stops seeing it only on a revocation
		// or a lost scope. The requester always sees its own.
		if _, err := s.eligibleApprover(ctx, decision, current, now); err != nil {
			if shoal.IsErrorCode(err, shoal.ErrorNotFound) ||
				errors.Is(err, ErrIdentitySchemeMoved) {
				return ApprovalStatus{}, auth.ObjectNotFound()
			}
			if shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
				if s.narrowed(ctx) {
					return ApprovalStatus{}, err
				}
				return ApprovalStatus{}, auth.ObjectNotFound()
			}
			return ApprovalStatus{}, err
		}
	}
	// What the effective state reports as final for a permanent reason is
	// written down by whoever asked, as itself (#655): the requester under
	// dispatch, an eligible approver under approve. Status is the one party
	// call that reaches a request whose target moved — the requester's
	// re-request is refused by its pinned rebuild first, and that is the
	// request a requester asks Status about. Opportunistic, like every lazy
	// settlement: a write this caller cannot make leaves the report exactly
	// what it was.
	operation := auth.OperationActionApprove
	if requester {
		operation = auth.OperationDispatch
	}
	if !current.State.terminal() {
		settled, wrote, err := s.settleObserved(ctx, operation, decision, current, now)
		switch {
		case wrote && err == nil:
			current = settled
		case wrote && errors.Is(err, ErrApprovalEventUnpublished):
			// Committed, and only its event is owed: the retry this asks
			// for finds the row terminal and drains it below.
			return ApprovalStatus{}, err
		case wrote:
			// Lost a compare-and-set to another writer. Report what is
			// there now.
			reread, readErr := s.store.GetApproval(ctx, request.ID)
			if readErr != nil {
				return ApprovalStatus{}, readErr
			}
			current = reread
		}
	} else if s.terminal &&
		(current.State == ApprovalExpired || current.State == ApprovalStranded) {
		// The kinds Status writes. A Status whose write committed and whose
		// event did not answered 503; this is the retry it asked for, and it
		// publishes the caller's own row. Quietly: Status is a read, and a
		// persistent failure on one row must not make it fail for #653's
		// whole lapse window. Only the call that committed answers for it.
		s.drainApprovalEventsQuietly(ctx, current.ID)
	}
	state, condition, err := s.effectiveState(ctx, current, now)
	if err != nil {
		return ApprovalStatus{}, err
	}
	return ApprovalStatus{
		Approval: CloneApprovalRecord(current), State: state, Condition: condition,
	}, nil
}

// VisibleTo reports whether the principal behind ctx may see this approval's
// lifecycle events (#653). The event service asks it at delivery, under each
// subscriber's own context.
//
// It is Status's rule, so subscribing can never show anyone more than asking
// Status would. The requester always sees its own request. Anyone else must
// be eligible to decide it, under every approver rule. That includes the
// narrowing one, and here a narrowed caller is simply not shown the event.
// Status answers such a caller 401 only when it would otherwise be eligible;
// an event has no response to vary, so there is no oracle to keep and
// nothing to tell. Every refusal is a false answer and never an error, so a
// subscriber cannot tell a request it may not see from one that does not
// exist.
//
// No RequestContext is involved: delivery is not a request to the approval
// service. The decision is resolved from ctx and judged at the service's
// clock.
func (s *ApprovalService) VisibleTo(
	ctx context.Context, id []byte,
) (bool, error) {
	if err := validateOpaque("approval ID", id, false); err != nil {
		return false, nil
	}
	decision, err := s.dispatch.resolver.Resolve(ctx)
	if err != nil {
		if shoal.IsErrorCode(err, shoal.ErrorUnauthorized) ||
			shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			return false, nil
		}
		return false, err
	}
	now := s.dispatch.clock().UTC()
	current, err := s.store.GetApproval(ctx, id)
	if err != nil {
		// Any read failure withholds the event and never fails the page.
		// The read is not about the subscriber: it runs for every subscriber
		// the scope join admits, co-tenants included, so an error here would
		// fail all of their pages for as long as it lasts, and one that lasts
		// (a record that does not decode, a head whose committed cell never
		// becomes visible) would fail them forever and tell each of them that
		// an approval event is there. Withholding costs at most one
		// notification for a party during a store fault. Status, which is the
		// durable truth, still answers them. A cancelled pull is the one
		// error passed on, because it is the subscriber's own.
		//
		// The way a head became permanently unreadable before, another
		// store adopting the approval's guard head through a shared kind
		// letter, is refused by the guard itself since the head-ownership
		// fix. This is the second line, not the first.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, ctxErr
		}
		return false, nil
	}
	if requestedBy(decision, current, now) {
		return true, nil
	}
	// Any failure to establish eligibility is a false answer, whatever its
	// code. Status may answer 503 where the target's binding cannot be
	// resolved (an executor ref the host no longer configures, a moved effect
	// ceiling), but at delivery an error fails the subscriber's whole page and
	// pins its cursor. That would wedge the stream behind one approval event,
	// and would tell a subscriber that should see nothing, such as the agent's
	// registrant, that an approval event is there. Only the approval read
	// itself, which is not about this subscriber, may fail the page, and only
	// while it fails.
	if _, err := s.eligibleApprover(ctx, decision, current, now); err != nil {
		return false, nil
	}
	return true, nil
}

// effectiveState computes what a stored approval amounts to now.
func (s *ApprovalService) effectiveState(
	ctx context.Context,
	current ApprovalRecord,
	now time.Time,
) (ApprovalState, ApprovalCondition, error) {
	switch current.State {
	case ApprovalRefused, ApprovalExpired, ApprovalWithdrawn:
		return current.State, ApprovalConditionNone, nil
	case ApprovalStranded:
		// Final, and the cause it was recorded for is the condition. Spelled
		// out cause by cause, so the pairs this can return are readable from
		// the source (pkg/narrate's parity test reads them).
		switch current.StrandedCause {
		case ApprovalConditionTargetMoved:
			return ApprovalStranded, ApprovalConditionTargetMoved, nil
		case ApprovalConditionPolicyMoved:
			return ApprovalStranded, ApprovalConditionPolicyMoved, nil
		case ApprovalConditionIdentityTaken:
			return ApprovalStranded, ApprovalConditionIdentityTaken, nil
		case ApprovalConditionDeadlinePassed:
			return ApprovalStranded, ApprovalConditionDeadlinePassed, nil
		}
		return "", "", shoal.NewError(
			shoal.ErrorInternal, "approval stranding cause is invalid")
	case ApprovalEnqueued:
		action, err := s.dispatch.store.GetAction(ctx, current.ID)
		if err == nil {
			// Something exists at the identity. It is this approval's work
			// only if it is the materialization of this approval — the same
			// predicate a replay applies — and otherwise the identity was
			// taken and the approval can never become work.
			if !isMaterializationOf(action, current) {
				return ApprovalUnresolvable, ApprovalConditionIdentityTaken, nil
			}
			// The work exists; where it has got to is the action's status.
			return ApprovalEnqueued, ApprovalConditionNone, nil
		}
		if !errors.Is(err, ErrActionNotFound) {
			return "", "", err
		}
		condition, err := s.unreachable(ctx, current, now)
		if err != nil {
			return "", "", err
		}
		if condition != ApprovalConditionNone {
			return ApprovalUnresolvable, condition, nil
		}
		return ApprovalEnqueued, ApprovalConditionAwaitingAction, nil
	}
	// Pending or approved.
	if !now.Before(current.ExpiresAt) {
		return ApprovalExpired, ApprovalConditionWindowClosed, nil
	}
	condition, err := s.unreachable(ctx, current, now)
	if err != nil {
		return "", "", err
	}
	if condition != ApprovalConditionNone {
		return ApprovalUnresolvable, condition, nil
	}
	// Above the mapping: under a moved scheme the mapping comparison means
	// nothing (see advance).
	if s.scheme.Digest != current.IdentityScheme {
		return ApprovalUnresolvable, ApprovalConditionIdentitySchemeMoved, nil
	}
	if current.State == ApprovalApproved {
		mapping, err := s.approverMapping(ctx)
		if err != nil {
			return "", "", err
		}
		if mapping != current.ApproverMappingDigest {
			return ApprovalUnresolvable, ApprovalConditionApproverMappingMoved, nil
		}
	}
	return current.State, ApprovalConditionNone, nil
}

// unreachable reports why a request can no longer progress, or none.
//
// The agent is read without the caller's authority: whether the target still
// exists at the request's generation is a fact about the registry, not about
// who is asking, and the caller has already been admitted to this record.
//
// The policy generation compared is the one in force, read from the
// generation authority, not the caller's token: a reader holding a token
// minted before the policy moved would otherwise be told a superseded request
// is live.
func (s *ApprovalService) unreachable(
	ctx context.Context,
	current ApprovalRecord,
	now time.Time,
) (ApprovalCondition, error) {
	if !now.Before(current.Request.Deadline) {
		return ApprovalConditionDeadlinePassed, nil
	}
	descriptor, err := s.dispatch.registry.active(
		ctx, current.Request.AgentID, now)
	if err != nil || descriptor.Generation != current.Request.AgentGeneration {
		return ApprovalConditionTargetMoved, nil
	}
	inForce, err := s.generations.CurrentPolicyGeneration(
		ctx, descriptor.AuthorizationDomain)
	if err != nil {
		return "", err
	}
	if inForce != current.PolicyGeneration {
		return ApprovalConditionPolicyMoved, nil
	}
	return ApprovalConditionNone, nil
}

// Pending lists requests this caller may decide and that are still decidable.
//
// A request the caller is not eligible to decide is skipped rather than
// refused, including its own: the page is the caller's queue, not a view of
// everyone's. A page may filter down to nothing; only an empty continuation
// ends the scan.
func (s *ApprovalService) Pending(
	ctx context.Context, request PendingApprovalsRequest,
) (PendingApprovalsPage, error) {
	dispatch := s.dispatch
	ctx, cancel := dispatch.deadline(ctx, request.Context)
	defer cancel()
	decision, now, err := dispatch.begin(
		ctx, auth.OperationActionApprove, request.Context)
	if err != nil {
		return PendingApprovalsPage{}, err
	}
	// Refused outright rather than left to the per-record check, which would
	// skip every record and answer with an empty queue — the wrong answer to
	// a caller whose queue is not empty but whose credential is unfit.
	if s.narrowed(ctx) {
		return PendingApprovalsPage{}, approvalUnderNarrowing()
	}
	if request.Limit <= 0 || request.Limit > MaxApprovalListResults {
		return PendingApprovalsPage{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "pending approval limit is outside its bound")
	}
	page, err := s.store.ScanApprovals(ctx, request.After, request.Limit)
	if err != nil {
		return PendingApprovalsPage{}, err
	}
	result := PendingApprovalsPage{Next: append([]byte(nil), page.Next...)}
	for _, record := range page.Approvals {
		if record.State != ApprovalPending || !now.Before(record.ExpiresAt) {
			continue
		}
		if _, err := s.eligibleApprover(ctx, decision, record, now); err != nil {
			if shoal.IsErrorCode(err, shoal.ErrorUnauthorized) ||
				shoal.IsErrorCode(err, shoal.ErrorNotFound) ||
				errors.Is(err, ErrIdentitySchemeMoved) {
				continue
			}
			return PendingApprovalsPage{}, err
		}
		result.Approvals = append(result.Approvals, CloneApprovalRecord(record))
	}
	return result, nil
}

// commit writes one approval transition, audit first, as every dispatch
// mutation is.
func (s *ApprovalService) commit(
	ctx context.Context,
	phase string,
	operation auth.Operation,
	decision auth.Decision,
	record ApprovalRecord,
	expectedVersion uint64,
) (ApprovalRecord, error) {
	if err := record.Validate(); err != nil {
		return ApprovalRecord{}, err
	}
	// The one place every transition passes: whatever path built it, a
	// withdrawn or stranded record is not written unless the operator has
	// enabled them (TerminalStates). Every path also refuses earlier, with
	// its own answer; this is the guarantee, not the answer.
	if !s.terminal &&
		(record.State == ApprovalWithdrawn || record.State == ApprovalStranded) {
		return ApprovalRecord{}, terminalStatesDisabled()
	}
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		return ApprovalRecord{}, err
	}
	publisher, err := approvalPublisherFor(decision, operation, phase)
	if err != nil {
		return ApprovalRecord{}, err
	}
	if err := s.recorder.RecordApproval(ctx, ApprovalAudit{
		Phase: phase, Operation: operation, Record: CloneApprovalRecord(record),
		Subject: decision.Subject(), Actor: decision.Actor(),
		ClientID: decision.ClientID(), OnBehalfOf: decision.OnBehalfOf(),
		RequestID:                decision.RequestID(),
		AuthorizationFingerprint: fingerprint,
		AuthorizationExpiresAt:   decision.AuthenticationExpires(),
		Provenance:               decision.GrantProvenance(),
	}); err != nil {
		return ApprovalRecord{}, errors.Join(ErrRecordingUnavailable, err)
	}
	mutation := ApprovalMutation{
		Token: transitionToken(
			"approval-"+phase, record.ID, nil, record.Version),
		ExpectedVersion: expectedVersion, Record: record,
	}
	if s.events != nil {
		mutation.Publisher = publisher
	}
	stored, err := s.store.ApplyApproval(ctx, mutation)
	if err != nil {
		return ApprovalRecord{}, err
	}
	// The transition is durable, and so is its event row, written with it.
	// Publishing it is this call's own business. A failure is answered as
	// ErrApprovalEventUnpublished beside the committed record, so a caller
	// can tell "committed, retry to publish" from "did not happen" (#653).
	return stored, s.drainApprovalEvents(ctx, stored.ID, stored.Version)
}

func receiptFor(record ApprovalRecord, action ActionRecord) ApprovalReceipt {
	receipt := ApprovalReceipt{
		State: record.State, ID: append([]byte(nil), record.ID...),
		RequestDigest:    append([]byte(nil), record.RequestDigest...),
		PolicyGeneration: record.PolicyGeneration,
		ExpiresAt:        record.ExpiresAt,
	}
	if record.State == ApprovalEnqueued {
		receipt.Action = cloneActionRecord(action)
	}
	return receipt
}

func approvalConflict() error {
	return shoal.WrapError(
		shoal.ErrorConflict, "approval does not match the held request",
		ErrApprovalConflict)
}

func approvalUnderNarrowing() error {
	return shoal.NewError(
		shoal.ErrorUnauthorized,
		"an approval cannot be decided or listed under workspace settings")
}

func approvalExpired() error {
	return shoal.WrapError(
		shoal.ErrorConflict, "approval window has closed", ErrApprovalExpired)
}
