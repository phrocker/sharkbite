// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package fleet

import (
	"context"
	"crypto/sha256"
	"errors"
	"time"

	"github.com/phrocker/shoal-oss/pkg/executorref"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Executor attestation required by policy (#446 slice 3).
//
// An action registered with RequiresAttestation may be claimed only by a
// principal that holds a current verified attestation for the descriptor's
// executor ref, and only for a lease the attestation covers. This file holds
// the seam the fleet consumes (ExecutorAttestations, AttestationTrust), the
// one gate predicate every claim grant passes through (attestationGate), and
// the presentation service behind POST /api/v1/fleet/executors/attestation.
//
// The fleet does not verify anything itself. internal/executorattest does,
// and cmd/shoal-explore-web adapts it to these interfaces, so this package
// stays free of the trust file, the verifier keys and the store.
var (
	// ErrAttestationRequired reports a claim, an extension or an admission
	// grant refused because the claimant holds no current attestation that
	// covers the lease it would be granted.
	//
	// It is a pre-commit refusal: it is raised before any write, and never
	// joined with ErrExecutionAmbiguous or ErrActionCommitted. That is what
	// makes it safe to map to a plain 409 above the indeterminate arm in the
	// transport (webapi.fleetDispatchError).
	ErrAttestationRequired = errors.New(
		"fleet dispatch: executor attestation required")
	// ErrAttestationUnavailable reports that the attestation store could not
	// answer. It is never "not attested": a caller cannot tell an outage from
	// a refusal otherwise, and reading an outage as a refusal would make a
	// flapping store look like a policy decision.
	ErrAttestationUnavailable = errors.New(
		"fleet dispatch: executor attestation is unavailable")
	// ErrAttestationRefused is the one public answer to a presentation that
	// did not verify, whatever the reason. The reason is audited and never
	// returned.
	ErrAttestationRefused = errors.New("fleet: executor attestation refused")
)

const (
	// MaxClaimAttestationIDBytes bounds ActionRecord.ClaimAttestationID and
	// ClaimHolder.AttestationID. internal/executorattest IDs are 73 bytes
	// ("exattest:" and 64 hex). The bound is enforced where an ID enters the
	// fleet (DispatchService.claimAttestation), not only in Validate, so an
	// adapter returning an oversized ID refuses the claim instead of producing
	// a record encodeAction then refuses — which would brick the action.
	MaxClaimAttestationIDBytes = 128
	// MaxAttestationReportBytes bounds a presented report (the signed
	// envelope).
	MaxAttestationReportBytes = 64 << 10
)

// AttestationTrust reports whether the host has an attestation trust root
// for an executor ref. Register consults it; a nil one configures none.
type AttestationTrust interface {
	Configured(executorRef string) bool
}

// AttestationPrincipal is who an attestation is for. It always comes from
// the authentication decision, never from a request body.
type AttestationPrincipal struct {
	Domain   []byte
	Subject  shoal.ID
	ClientID shoal.ID
}

// ExecutorAttestation is a principal's current verified attestation for one
// executor ref, as the store reports it at one instant. OK is false when there
// is none (absent, expired, or no longer matching the trust in force).
type ExecutorAttestation struct {
	ID        shoal.ID
	ExpiresAt time.Time
	OK        bool
}

// ExecutorAttestations is the read the fleet makes before granting a claim on
// an action that requires attestation. Current must judge "now" by the clock
// it is handed and report a store failure as an error, never as OK false.
type ExecutorAttestations interface {
	Current(ctx context.Context, principal AttestationPrincipal,
		executorRef string, now time.Time) (ExecutorAttestation, error)
}

func attestationRequired() error {
	// Static text: the refusal names no verifier, digest, attestation ID or
	// expiry, so it tells a caller with standing only what to do next.
	return shoal.WrapError(shoal.ErrorConflict,
		"action requires a current executor attestation covering the claim lease; "+
			"present one and retry", ErrAttestationRequired)
}

func attestationUnavailable() error {
	return shoal.WrapError(shoal.ErrorUnavailable,
		"executor attestation is unavailable", ErrAttestationUnavailable)
}

// claimRequirements is what a claim grant must satisfy, after combining what
// the record carries with what the action's *current* registration requires.
// It is one small shape so #486 can add approval to it the same way.
type claimRequirements struct {
	Attestation bool
	// Approval is #486's term. Unlike Attestation, whose record side is
	// claim-scoped, this one's is record-scoped and immutable: an action
	// materialized from an approval carries its digest for life.
	Approval bool
}

// effectiveClaimRequirements combines the record's snapshot with the current
// registration, and the stricter wins.
//
// current is the Action resolveActionBinding returned, which it reads from
// the stored descriptor as it is now — not from anything the record carried
// at enqueue. So a requirement registered after a record was enqueued governs
// that record whenever it resolves, independently of generation pinning.
// Pinning stops such a record resolving today; if #486 unpins it, the
// requirement still applies.
//
// The record's side is the attestation its current claim was granted under:
// a claim that stood on an attestation keeps requiring one for its renewal
// and its re-claim. The record carries no enqueue-time snapshot of the
// requirement; a record-level snapshot, if #486 adds one, ORs in here.
func effectiveClaimRequirements(record ActionRecord, current Action) claimRequirements {
	return claimRequirements{
		Attestation: current.RequiresAttestation || record.ClaimAttestationID != "",
		Approval: current.RequiresApproval ||
			len(record.ApprovalRequestDigest) > 0,
	}
}

// attestationGate is the one predicate a claim grant passes. applyClaim
// applies it to every claim (dispatch Claim and the admission grant), and
// ExtendClaim applies it to the clamped extended lease end; nothing else
// grants a claim.
//
// The lease end it is handed must be the one the record will carry: clamped
// to the action's deadline. A claim never outlives its attestation, so a
// lapse coincides with a lease lapse and the fence covers it.
func attestationGate(required claimRequirements, attestation ExecutorAttestation, leaseUntil time.Time) error {
	if !required.Attestation {
		return nil
	}
	if !attestation.OK || attestation.ID == "" ||
		attestation.ExpiresAt.Before(leaseUntil) {
		return attestationRequired()
	}
	return nil
}

// approvalGate refuses a claim on work the action's current registration
// requires approval for, when the record carries no approval.
//
// This is what lets the generation pin come off. Before it, a record enqueued
// while its capability did not require approval stayed claimable after the
// requirement was turned on — and the only thing preventing that was
// resolveActionBinding refusing *every* post-enqueue resolution of a
// pre-enqueue generation, which is the same refusal a heartbeat caused. The
// pin was preventing the bypass incidentally; this prevents it on purpose.
//
// It also refuses for the right reason. The pin's refusal was
// ObjectNotFound on a generation number, which tells a worker the action does
// not exist; this says approval is required, which is true and actionable.
//
// Stricter-wins, so there is no direction in which a flip loses: turning the
// requirement on governs records enqueued before it, and an already-approved
// record keeps its approval if the requirement is later turned off.
func approvalGate(required claimRequirements, record ActionRecord) error {
	if !required.Approval {
		return nil
	}
	if len(record.ApprovalRequestDigest) == 0 {
		return approvalRequiredForClaim()
	}
	return nil
}

// approvalRequiredForClaim mirrors attestationRequired: a conflict whose text
// is static, naming no approver, digest or decision, so it tells a caller with
// standing what to do next and nothing else.
//
// Distinguishable from ObjectNotFound, which is a deliberate departure from
// this surface's usual rule that every standing refusal is indistinguishable
// (#398). The sibling attestation gate made the same choice for the same
// reason and the two must agree, because a caller cannot be expected to learn
// that one requirement refuses visibly and the other invisibly.
//
// What it does concede: a caller holding execute on the descriptor, who guesses
// an action ID, learns the action exists. That is narrower than it looks —
// claimableBy filters such a record out of Pull, so a legitimate worker never
// reaches this gate and only a guesser does — and it is the same concession
// attestation already makes. Raised as a question about both gates rather than
// resolved differently in one of them.
func approvalRequiredForClaim() error {
	return shoal.WrapError(shoal.ErrorConflict,
		"action requires approval; it must be requested through the approval "+
			"route before it can be claimed", ErrApprovalRequired)
}

// claimLeaseEnd is the lease end a claim taken at now for lease would carry:
// clamped to the action's deadline. applyClaim and the gate must agree on it.
func claimLeaseEnd(now time.Time, lease time.Duration, deadline time.Time) time.Time {
	end := now.Add(lease)
	if end.After(deadline) {
		return deadline
	}
	return end
}

// claimAttestation reads the claimant's current attestation for ref, and only
// when the action requires one. Callers do the read and hand the result to the
// gate, so the gate itself is pure and a new caller cannot skip it by
// forgetting a store read: without a read it is handed the zero value, which
// the gate refuses.
//
// A delegated decision (one acting on behalf of others) and one without a
// client are never attested. The row key is (domain, subject, client); an
// attestation describes the executor process that presented it as itself,
// and letting it stand for a chain it did not present under would let one
// attested process lend its attestation to every principal it acts for.
func (s *DispatchService) claimAttestation(
	ctx context.Context, decision auth.Decision, required claimRequirements,
	executorRef string, now time.Time,
) (ExecutorAttestation, error) {
	if !required.Attestation {
		return ExecutorAttestation{}, nil
	}
	if nilDependency(s.attestations) || len(decision.OnBehalfOf()) > 0 ||
		decision.ClientID() == "" {
		return ExecutorAttestation{}, nil
	}
	attestation, err := s.attestations.Current(ctx, AttestationPrincipal{
		Domain: decision.AuthorizationDomain(), Subject: decision.Subject(),
		ClientID: decision.ClientID(),
	}, executorRef, now)
	if err != nil {
		return ExecutorAttestation{}, attestationUnavailable()
	}
	if !attestation.OK {
		return ExecutorAttestation{}, nil
	}
	// The boundary bound. An ID that cannot be retained is an adapter fault,
	// not an absent attestation, so it answers unavailable rather than 409.
	if shoal.ValidateRequiredID("claim attestation ID", attestation.ID) != nil ||
		len(attestation.ID) > MaxClaimAttestationIDBytes ||
		attestation.ExpiresAt.IsZero() {
		return ExecutorAttestation{}, attestationUnavailable()
	}
	attestation.ExpiresAt = attestation.ExpiresAt.UTC()
	return attestation, nil
}

// ClaimRefusedAttestationPhase is the audit phase of a claim, extension or
// admission grant refused for want of attestation.
const ClaimRefusedAttestationPhase = "claim_refused_attestation"

// auditAttestationRefusal records a claim refused for want of attestation.
// Best effort by design: the refusal stands whether or not it can be
// recorded, because the alternative — answering a recorder outage instead —
// would turn a policy refusal into a retryable 503 and invite the retry.
//
// The audited record is a copy that names the *refused* caller: the claimant
// fields and the transition request and correlation IDs come from the
// refusing decision. Auditing the stored record would name the enqueuer and
// the previous holder, never who was refused, and every refusal at one
// version would collapse into one session. The copy is never persisted; only
// the audit sees it. The recorder keys this phase's session by the
// transition request ID as well (explorerfleet.actionSessionID), so two
// refusals are two entries.
func (s *DispatchService) auditAttestationRefusal(
	ctx context.Context, record ActionRecord, operation auth.Operation,
	decision auth.Decision,
) {
	refused := refusedClaimAudit(record, decision)
	_ = s.recorder.RecordAction(ctx, ActionAudit{
		Phase: ClaimRefusedAttestationPhase, Operation: operation, Record: refused,
	})
}

// refusedClaimAudit is the audit-only copy auditAttestationRefusal records.
func refusedClaimAudit(record ActionRecord, decision auth.Decision) ActionRecord {
	refused := cloneActionRecord(record)
	refused.ClaimantSubject = decision.Subject()
	refused.ClaimantActor = decision.Actor()
	refused.ClaimantClientID = decision.ClientID()
	refused.ClaimantOnBehalfOf = append([]shoal.ID(nil), decision.OnBehalfOf()...)
	refused.TransitionRequestID = decision.RequestID()
	refused.TransitionCorrelationID = decision.CorrelationID()
	return refused
}

// AttestationPresentation is one presentation. The principal is not here: it
// comes from the authentication decision, so a caller can only attest itself.
type AttestationPresentation struct {
	ExecutorRef    string
	IdempotencyKey []byte
	Report         []byte
}

// AttestationReceipt is the verified result a presentation recorded.
type AttestationReceipt struct {
	AttestationID shoal.ID
	ExpiresAt     time.Time
}

// AttestationRefusal is a presenter's refusal. Reason is for audit only; the
// caller is answered with ErrAttestationRefused alone.
type AttestationRefusal struct{ Reason string }

func (r *AttestationRefusal) Error() string { return ErrAttestationRefused.Error() }
func (r *AttestationRefusal) Unwrap() error { return ErrAttestationRefused }

// AttestationPresenter verifies and records presentations. Verify is pure;
// Record verifies again under the trust in force and stores the result as the
// principal's latest. Either refuses with an *AttestationRefusal; any other
// error is a store or configuration failure.
type AttestationPresenter interface {
	Verify(principal AttestationPrincipal, presentation AttestationPresentation,
		now time.Time) (AttestationReceipt, error)
	Record(ctx context.Context, principal AttestationPrincipal,
		presentation AttestationPresentation, now time.Time) (AttestationReceipt, error)
}

// AttestationAudit is one audited presentation outcome.
type AttestationAudit struct {
	// Phase is "attestation_presented" or "attestation_refused".
	Phase       string
	Principal   AttestationPrincipal
	Actor       shoal.ID
	RequestID   shoal.ID
	ExecutorRef string
	// KeyDigest identifies the presentation without retaining its key.
	KeyDigest     [sha256.Size]byte
	AttestationID shoal.ID
	ExpiresAt     time.Time
	// Reason is the typed refusal reason, for a refusal.
	Reason string
	// The presenting decision's authorization provenance.
	AuthorizationFingerprint auth.Fingerprint
	AuthorizationExpiresAt   time.Time
}

// AttestationRecorder records presentation outcomes.
type AttestationRecorder interface {
	RecordAttestation(context.Context, AttestationAudit) error
}

type AttestationConfig struct {
	Presenter AttestationPresenter
	Resolver  auth.Resolver
	Recorder  AttestationRecorder
	Clock     func() time.Time
}

// AttestationService serves executor attestation presentations.
type AttestationService struct {
	presenter AttestationPresenter
	resolver  auth.Resolver
	recorder  AttestationRecorder
	clock     func() time.Time
}

func NewAttestationService(config AttestationConfig) (*AttestationService, error) {
	if nilDependency(config.Presenter) || nilDependency(config.Resolver) ||
		nilDependency(config.Recorder) || config.Clock == nil {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument,
			"fleet attestation dependencies are required")
	}
	return &AttestationService{
		presenter: config.Presenter, resolver: config.Resolver,
		recorder: config.Recorder, clock: config.Clock,
	}, nil
}

func attestationRefused() error {
	return shoal.WrapError(shoal.ErrorUnauthorized, "attestation refused",
		ErrAttestationRefused)
}

// Present verifies a presentation for the authenticated caller and records it
// as the caller's latest attestation for the executor ref.
//
// It is gated exactly as the other execute routes are: the decision must carry
// a correlation ID (as DispatchService.begin requires) and OperationExecute in
// its own domain. Every presentation is audited — refused or accepted — and a
// refusal stands even when its audit cannot be recorded. An accepted one is
// audited before it is stored, so no attestation becomes usable without the
// record that says who presented it.
func (s *AttestationService) Present(
	ctx context.Context, presentation AttestationPresentation,
) (AttestationReceipt, error) {
	now := s.clock().UTC()
	decision, err := s.resolver.Resolve(ctx)
	if err != nil {
		return AttestationReceipt{}, err
	}
	if err := shoal.ValidateRequiredID(
		"dispatch correlation ID", decision.CorrelationID()); err != nil {
		return AttestationReceipt{}, err
	}
	if err := decision.Authorize(auth.OperationExecute, auth.ResourceRequest{
		AuthorizationDomain: decision.AuthorizationDomain(),
	}, now); err != nil {
		return AttestationReceipt{}, err
	}
	ref := presentation.ExecutorRef
	if executorref.ValidExecutorRef(ref) != nil ||
		len(presentation.IdempotencyKey) == 0 ||
		len(presentation.IdempotencyKey) > shoal.MaxIDBytes ||
		len(presentation.Report) == 0 ||
		len(presentation.Report) > MaxAttestationReportBytes {
		return AttestationReceipt{}, shoal.NewError(shoal.ErrorInvalidArgument,
			"attestation presentation is outside its bounds")
	}
	principal := AttestationPrincipal{
		Domain: decision.AuthorizationDomain(), Subject: decision.Subject(),
		ClientID: decision.ClientID(),
	}
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		return AttestationReceipt{}, err
	}
	audit := AttestationAudit{
		Principal: principal, Actor: decision.Actor(),
		RequestID: decision.RequestID(), ExecutorRef: ref,
		KeyDigest:                sha256.Sum256(presentation.IdempotencyKey),
		AuthorizationFingerprint: fingerprint,
		AuthorizationExpiresAt:   decision.AuthenticationExpires(),
	}
	refuse := func(reason string) (AttestationReceipt, error) {
		refused := audit
		refused.Phase, refused.Reason = "attestation_refused", reason
		_ = s.recorder.RecordAttestation(ctx, refused)
		return AttestationReceipt{}, attestationRefused()
	}
	// See DispatchService.claimAttestation: such an attestation could never
	// be used, and refusing it here says so at presentation rather than at
	// every claim.
	if len(decision.OnBehalfOf()) > 0 {
		return refuse("delegated")
	}
	if principal.ClientID == "" {
		return refuse("no-client")
	}
	// A worker attests only for the executor it is bound to (#391). The claim
	// it means to take is narrowed to that reference, so an attestation for
	// any other could never be used by it — and accepting one would let a
	// worker hold standing statements for executors it may not act for. An
	// unbound decision has no reference to attest for: it claims nothing on
	// the execute route.
	if ref != decision.ExecutorBinding() {
		return refuse("executor-binding")
	}
	verified, err := s.presenter.Verify(principal, presentation, now)
	if err != nil {
		return refuse(refusalReason(err))
	}
	accepted := audit
	accepted.Phase = "attestation_presented"
	accepted.AttestationID, accepted.ExpiresAt = verified.AttestationID, verified.ExpiresAt
	if err := s.recorder.RecordAttestation(ctx, accepted); err != nil {
		return AttestationReceipt{}, shoal.WrapError(shoal.ErrorUnavailable,
			"executor attestation recording is unavailable", ErrRecordingUnavailable)
	}
	stored, err := s.presenter.Record(ctx, principal, presentation, now)
	if err != nil {
		var refusal *AttestationRefusal
		if errors.As(err, &refusal) {
			return refuse(refusal.Reason)
		}
		return AttestationReceipt{}, attestationUnavailable()
	}
	return stored, nil
}

// refusalReason is the audit reason for a Verify failure. A non-refusal error
// from a pure verification is a malformed expectation; it is still a refusal
// to the caller.
func refusalReason(err error) string {
	var refusal *AttestationRefusal
	if errors.As(err, &refusal) && refusal.Reason != "" {
		return refusal.Reason
	}
	return "malformed"
}
