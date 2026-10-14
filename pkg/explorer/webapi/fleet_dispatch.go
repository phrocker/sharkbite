// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package webapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	dispatchapi "github.com/phrocker/shoal-oss/pkg/dispatch/api"
	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type FleetDispatchProvider interface {
	Enqueue(context.Context, fleet.EnqueueRequest) (fleet.ActionRecord, error)
	Claim(context.Context, fleet.ClaimRequest) (fleet.ActionRecord, error)
	Cancel(context.Context, fleet.CancelRequest) (fleet.ActionRecord, error)
	Status(context.Context, fleet.StatusRequest) (fleet.ActionRecord, error)
	Pull(context.Context, fleet.PullActionsRequest) (fleet.ActionPage, error)
	Invoke(context.Context, fleet.InvokeRequest) (fleet.ActionRecord, error)
	CompleteClaim(context.Context, fleet.CompletionRequest) (fleet.ActionRecord, error)
	ReportAmbiguity(context.Context, fleet.AmbiguityRequest) (fleet.ActionRecord, error)
	ExtendClaim(context.Context, fleet.ExtendRequest) (fleet.ActionRecord, error)
}

// NewFleetDispatchHandler returns the fleet dispatch HTTP surface without
// adding another authentication boundary. Mount it through
// Handler.MountAuthenticated at /api/v1/fleet/.
func NewFleetDispatchHandler(provider FleetDispatchProvider) (http.Handler, error) {
	if provider == nil {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "fleet dispatch provider is required")
	}
	mux := http.NewServeMux()
	mountFleetDispatch(mux, provider)
	return mux, nil
}

// MountFleetDispatch is retained for dispatch-only tests and compatibility.
// Hosted startup must instead mount NewFleetHandler once at FleetRoutePrefix.
func (h *Handler) MountFleetDispatch(provider FleetDispatchProvider) error {
	if h == nil || provider == nil {
		return shoal.NewError(shoal.ErrorInvalidArgument, "fleet dispatch provider is required")
	}
	if isAbsentInterface(h.authenticator) || isAbsentInterface(h.binder) {
		return shoal.NewError(shoal.ErrorInvalidArgument, "fleet dispatch requires authenticated transport")
	}
	mountFleetDispatch(h.mux, provider)
	return nil
}

func mountFleetDispatch(mux *http.ServeMux, provider FleetDispatchProvider) {
	mux.HandleFunc("POST /api/v1/fleet/actions", func(w http.ResponseWriter, r *http.Request) {
		var wire fleetEnqueueWire
		if err := decodeRequest(w, r, &wire); err != nil {
			writeError(w, shoal.NewError(shoal.ErrorInvalidArgument, err.Error()))
			return
		}
		request, err := wire.decode(nil)
		if err != nil {
			writeError(w, fleetDispatchError(err))
			return
		}
		result, err := provider.Enqueue(r.Context(), request)
		if err != nil {
			writeError(w, fleetDispatchError(err))
			return
		}
		writeResponse(w, http.StatusCreated, encodeFleetAction(result))
	})
	mux.HandleFunc("POST /api/v1/fleet/actions/invoke", func(w http.ResponseWriter, r *http.Request) {
		var wire fleetInvokeWire
		if err := decodeRequest(w, r, &wire); err != nil {
			writeError(w, shoal.NewError(shoal.ErrorInvalidArgument, err.Error()))
			return
		}
		enqueue, err := wire.Action.decode(nil)
		if err != nil {
			writeError(w, fleetDispatchError(err))
			return
		}
		claimID, err := decodeWireBytes("claim ID", wire.ClaimID, false)
		if err != nil {
			writeError(w, fleetDispatchError(err))
			return
		}
		result, err := provider.Invoke(r.Context(), fleet.InvokeRequest{Enqueue: enqueue, ClaimID: claimID, Lease: wire.Lease})
		if err != nil {
			writeError(w, fleetDispatchError(err))
			return
		}
		writeResponse(w, http.StatusOK, encodeFleetAction(result))
	})
	mux.HandleFunc("POST /api/v1/fleet/actions/pull", func(w http.ResponseWriter, r *http.Request) {
		var wire fleetPullWire
		if err := decodeRequest(w, r, &wire); err != nil {
			writeError(w, shoal.NewError(shoal.ErrorInvalidArgument, err.Error()))
			return
		}
		contextValue, err := wire.Context.decode()
		if err != nil {
			writeError(w, fleetDispatchError(err))
			return
		}
		after, err := decodeWireBytes("action cursor", wire.After, true)
		if err != nil {
			writeError(w, fleetDispatchError(err))
			return
		}
		page, err := provider.Pull(r.Context(), fleet.PullActionsRequest{After: after, Limit: wire.Limit, Context: contextValue})
		if err != nil {
			writeError(w, fleetDispatchError(err))
			return
		}
		actions := make([]fleetActionWire, len(page.Actions))
		for i := range page.Actions {
			actions[i] = encodeFleetAction(page.Actions[i])
		}
		writeResponse(w, http.StatusOK, dispatchapi.Page{
			Actions: actions, Next: base64.RawURLEncoding.EncodeToString(page.Next),
		})
	})
	mux.HandleFunc("POST /api/v1/fleet/actions/{action}/claim", func(w http.ResponseWriter, r *http.Request) {
		actionID, err := decodeWireBytes("action ID", r.PathValue("action"), false)
		if err != nil {
			writeError(w, fleetDispatchError(err))
			return
		}
		var wire fleetClaimWire
		if err := decodeRequest(w, r, &wire); err != nil {
			writeError(w, shoal.NewError(shoal.ErrorInvalidArgument, err.Error()))
			return
		}
		contextValue, err := wire.Context.decode()
		if err != nil {
			writeError(w, fleetDispatchError(err))
			return
		}
		claimID, err := decodeWireBytes("claim ID", wire.ClaimID, false)
		if err != nil {
			writeError(w, fleetDispatchError(err))
			return
		}
		result, err := provider.Claim(r.Context(), fleet.ClaimRequest{ID: actionID, ExpectedVersion: wire.ExpectedVersion, ClaimID: claimID, Lease: wire.Lease, Context: contextValue})
		if err != nil {
			writeError(w, fleetDispatchError(err))
			return
		}
		writeResponse(w, http.StatusOK, encodeClaimedFleetAction(result))
	})
	mux.HandleFunc("POST /api/v1/fleet/actions/{action}/complete", func(w http.ResponseWriter, r *http.Request) {
		actionID, err := decodeWireBytes("action ID", r.PathValue("action"), false)
		if err != nil {
			writeError(w, fleetDispatchError(err))
			return
		}
		var wire fleetCompletionWire
		if err := decodeRequest(w, r, &wire); err != nil {
			writeError(w, shoal.NewError(shoal.ErrorInvalidArgument, err.Error()))
			return
		}
		contextValue, err := wire.Context.decode()
		if err != nil {
			writeError(w, fleetDispatchError(err))
			return
		}
		claimID, err := decodeWireBytes("claim ID", wire.ClaimID, false)
		if err != nil {
			writeError(w, fleetDispatchError(err))
			return
		}
		snapshotID, err := decodeOptionalID(wire.EvidenceSnapshotID)
		if err != nil {
			writeError(w, fleetDispatchError(err))
			return
		}
		evidence, err := decodeEvidence(wire.Evidence)
		if err != nil {
			writeError(w, fleetDispatchError(err))
			return
		}
		result, err := provider.CompleteClaim(r.Context(), fleet.CompletionRequest{
			ID: actionID, ExpectedVersion: wire.ExpectedVersion, ClaimID: claimID,
			ClaimFence: wire.ClaimFence,
			Failed:     wire.Failed, Context: contextValue,
			Result: fleet.ExecutionResult{
				Output: wire.Output, ErrorCode: wire.ErrorCode,
				Effected:             decodeEffected(wire.Effected),
				EvidenceSnapshotID:   snapshotID,
				EvidenceSnapshotAsOf: wire.EvidenceSnapshotAsOf,
				Evidence:             evidence,
			},
		})
		if err != nil {
			writeError(w, fleetDispatchError(err))
			return
		}
		writeResponse(w, http.StatusOK, encodeFleetAction(result))
	})
	// Renewing a live claim so the fenced window can cover an operation longer
	// than MaxActionClaimTTL (#430). The fence does not advance, so the
	// claimant keeps the fence it reports under.
	mux.HandleFunc("POST /api/v1/fleet/actions/{action}/extend", func(w http.ResponseWriter, r *http.Request) {
		actionID, err := decodeWireBytes("action ID", r.PathValue("action"), false)
		if err != nil {
			writeError(w, fleetDispatchError(err))
			return
		}
		var wire fleetExtendWire
		if err := decodeRequest(w, r, &wire); err != nil {
			writeError(w, shoal.NewError(shoal.ErrorInvalidArgument, err.Error()))
			return
		}
		contextValue, err := wire.Context.decode()
		if err != nil {
			writeError(w, fleetDispatchError(err))
			return
		}
		claimID, err := decodeWireBytes("claim ID", wire.ClaimID, false)
		if err != nil {
			writeError(w, fleetDispatchError(err))
			return
		}
		result, err := provider.ExtendClaim(r.Context(), fleet.ExtendRequest{
			ID: actionID, ExpectedVersion: wire.ExpectedVersion,
			ClaimFence: wire.ClaimFence,
			ClaimID:    claimID, Lease: wire.Lease, Context: contextValue,
		})
		if err != nil {
			writeError(w, fleetDispatchError(err))
			return
		}
		writeResponse(w, http.StatusOK, encodeFleetAction(result))
	})
	// A worker that lost its fence mid-effect cannot use /complete, which is
	// gated on the fence it lost. This route records what it attempted without
	// transitioning the action (#438).
	mux.HandleFunc("POST /api/v1/fleet/actions/{action}/ambiguity", func(w http.ResponseWriter, r *http.Request) {
		actionID, err := decodeWireBytes("action ID", r.PathValue("action"), false)
		if err != nil {
			writeError(w, fleetDispatchError(err))
			return
		}
		var wire fleetAmbiguityWire
		if err := decodeRequest(w, r, &wire); err != nil {
			writeError(w, shoal.NewError(shoal.ErrorInvalidArgument, err.Error()))
			return
		}
		contextValue, err := wire.Context.decode()
		if err != nil {
			writeError(w, fleetDispatchError(err))
			return
		}
		result, err := provider.ReportAmbiguity(r.Context(), fleet.AmbiguityRequest{
			ID: actionID, ExpectedVersion: wire.ExpectedVersion,
			ClaimFence: wire.ClaimFence,
			Outcome:    fleet.AmbiguityOutcome(wire.Outcome),
			Target:     wire.Target, Reference: wire.Reference,
			Context: contextValue,
		})
		if err != nil {
			writeError(w, fleetDispatchError(err))
			return
		}
		writeResponse(w, http.StatusOK, encodeFleetAction(result))
	})
	mux.HandleFunc("POST /api/v1/fleet/actions/{action}/cancel", func(w http.ResponseWriter, r *http.Request) {
		actionID, err := decodeWireBytes("action ID", r.PathValue("action"), false)
		if err != nil {
			writeError(w, fleetDispatchError(err))
			return
		}
		var wire fleetCancelWire
		if err := decodeRequest(w, r, &wire); err != nil {
			writeError(w, shoal.NewError(shoal.ErrorInvalidArgument, err.Error()))
			return
		}
		contextValue, err := wire.Context.decode()
		if err != nil {
			writeError(w, fleetDispatchError(err))
			return
		}
		mutationKey, err := decodeWireBytes(
			"cancel mutation key", wire.MutationKey, false)
		if err != nil {
			writeError(w, fleetDispatchError(err))
			return
		}
		result, err := provider.Cancel(r.Context(), fleet.CancelRequest{
			ID: actionID, ExpectedVersion: wire.ExpectedVersion,
			MutationKey: mutationKey, Context: contextValue,
		})
		if err != nil {
			writeError(w, fleetDispatchError(err))
			return
		}
		writeResponse(w, http.StatusOK, encodeFleetAction(result))
	})
	mux.HandleFunc("POST /api/v1/fleet/actions/{action}/status", func(w http.ResponseWriter, r *http.Request) {
		actionID, err := decodeWireBytes("action ID", r.PathValue("action"), false)
		if err != nil {
			writeError(w, fleetDispatchError(err))
			return
		}
		var wire fleetRequestContextWire
		if err := decodeRequest(w, r, &wire); err != nil {
			writeError(w, shoal.NewError(shoal.ErrorInvalidArgument, err.Error()))
			return
		}
		contextValue, err := wire.decode()
		if err != nil {
			writeError(w, fleetDispatchError(err))
			return
		}
		result, err := provider.Status(r.Context(), fleet.StatusRequest{ID: actionID, Context: contextValue})
		if err != nil {
			writeError(w, fleetDispatchError(err))
			return
		}
		writeResponse(w, http.StatusOK, encodeFleetAction(result))
	})
}

func fleetDispatchError(err error) error {
	switch {
	case err == nil:
		return nil
	// Before the generic conflict arm, and explicit even though the service
	// already returns it as a conflict: writeError starts from 500, so a bare
	// sentinel from any provider would otherwise surface as an internal error.
	// Conflict is deliberate — the caller is authorized and the request is
	// well formed, but the action's registration requires the approval route
	// (see fleet.approvalRequired). Not unauthorized: the work may well
	// happen, pending somebody else's decision.
	case errors.Is(err, fleet.ErrApprovalRequired):
		if shoal.IsErrorCode(err, shoal.ErrorConflict) {
			return err
		}
		return shoal.WrapError(
			shoal.ErrorConflict,
			"action requires approval; request it through the approval route",
			err)
	// Beside approval, above the indeterminate arm, and safe there for one
	// reason: ErrAttestationRequired is a pre-commit refusal. Claim,
	// ExtendClaim and the admission grant raise it from the gate before any
	// audit or store write, and nothing joins it with ErrExecutionAmbiguous or
	// ErrActionCommitted — so matching it first can never hide a write that
	// landed. TestFleetDispatchErrorMarksEveryIndeterminateJoin asserts it is
	// unmarked. Conflict for approval's reason: the caller has standing and
	// the request is well formed; the remedy is to present an attestation
	// that covers the lease and retry. The message names no verifier, digest
	// or attestation.
	case errors.Is(err, fleet.ErrAttestationRequired):
		if shoal.IsErrorCode(err, shoal.ErrorConflict) {
			return err
		}
		return shoal.WrapError(
			shoal.ErrorConflict,
			"action requires a current executor attestation covering the claim lease; "+
				"present one and retry",
			err)
	// A store failure is unavailable, unmarked: nothing was written, so a
	// retry is safe, and it is never read as "not attested".
	case errors.Is(err, fleet.ErrAttestationUnavailable):
		if shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
			return err
		}
		return shoal.WrapError(shoal.ErrorUnavailable,
			"executor attestation is unavailable", err)
	// The two sentinels that mean something durable may have happened, marked
	// so writeError sets Shoal-Commit-Outcome: indeterminate and the body
	// flag. Before this arm existed all three below shared one, so a caller
	// received a bare 503 and could not tell "retry, nothing happened" from
	// "stop, something may have happened" — which is the single most important
	// distinction this surface has to communicate, and the one a worker's
	// behaviour differs most sharply on.
	//
	// ErrExecutionAmbiguous: the effect may have occurred and Shoal cannot
	// tell. ErrActionCommitted: the transition was durably written and only
	// its publication failed, so the write landed.
	//
	// Ordering. This arm has to come before *every* other arm that could
	// match the same error, and the first version of it did not: it sat below
	// the not-found and conflict arms, which shadowed the two cases that
	// matter most.
	//
	// applyExecutionResult raises, after the external effect, either
	// errors.Join(ErrExecutionAmbiguous, readErr) or
	// errors.Join(ErrExecutionAmbiguous, ErrClaimLost) — a lease that lapsed
	// while the effect was in flight. Measured against the old order:
	//
	//	ErrExecutionAmbiguous                        → 503, marked
	//	Join(ErrExecutionAmbiguous, ErrClaimLost)    → 409 conflict, unmarked
	//	Join(ErrExecutionAmbiguous, ErrActionNotFound) → 404, unmarked
	//	Join(ErrActionCommitted, ErrActionTerminal)  → 409 conflict, unmarked
	//
	// So the single case the ambiguity sentinel exists for was answered 409
	// "conflict", which is worse than the bare 503 this change set out to
	// replace: a conflict reads as "your request did not apply", and the
	// caller retries an external effect that may already have happened.
	//
	// It also has to come before the recording arm, because
	// applyExecutionResult's post-effect recorder failure joins
	// ErrExecutionAmbiguous and ErrRecordingUnavailable together and must be
	// marked.
	case errors.Is(err, fleet.ErrExecutionAmbiguous),
		errors.Is(err, fleet.ErrActionCommitted):
		return explorer.MarkIndeterminateCommit(shoal.WrapError(
			shoal.ErrorUnavailable,
			"fleet action outcome requires reconciliation", err))
	// Deliberately *not* marked, and the first version of this comment got
	// the reason wrong in a way worth recording.
	//
	// It said every site raises this from a RecordAction failure sitting
	// immediately before the matching store write, "so the transition did not
	// commit". There are ten sites, not the nine it claimed, one of them is a
	// RecordApproval rather than a RecordAction failure, and one of the ten
	// falsifies the sentence outright: ApprovalService.advance commits the
	// approved → enqueued transition and then falls through to materialize,
	// whose audit failure is raised with that approval write already durable.
	//
	// The answer is still an unmarked 503, for the reason the comment should
	// have given. The marker means an *effect* may have happened — that is
	// what Shoal-Commit-Outcome: indeterminate tells a caller, and what it
	// must act on. At every one of the ten sites nothing external was
	// contacted, and the one durable partial is resumable: the approval row
	// sits at ApprovalEnqueued, a re-request re-enters at that case, and the
	// materialization completes. Marking it would send a caller to reconcile
	// against a target that was never reached.
	//
	// Both halves of that are asserted rather than argued.
	// TestARecorderFailureLeavesTheRecordUnchanged covers the dispatch phases
	// and TestAMaterializationRecorderFailureIsResumable covers the approval
	// one, including that the partial commits and that the retry resolves it.
	//
	// The genuinely post-effect recorder failure carries
	// ErrExecutionAmbiguous and is caught by the arm above.
	case errors.Is(err, fleet.ErrActionNotFound):
		return shoal.WrapError(shoal.ErrorNotFound, "fleet action not found", err)
	case errors.Is(err, fleet.ErrActionConflict), errors.Is(err, fleet.ErrClaimLost), errors.Is(err, fleet.ErrActionTerminal):
		return shoal.WrapError(shoal.ErrorConflict, "fleet action conflict", err)
	case errors.Is(err, fleet.ErrRecordingUnavailable):
		return shoal.WrapError(shoal.ErrorUnavailable, "fleet action outcome requires reconciliation", err)
	default:
		return err
	}
}

type fleetEnqueueWire struct {
	Context         fleetRequestContextWire `json:"context"`
	ID              string                  `json:"id"`
	IdempotencyKey  string                  `json:"idempotency_key"`
	AgentID         string                  `json:"agent_id"`
	AgentGeneration int64                   `json:"agent_generation"`
	Capability      string                  `json:"capability"`
	Action          string                  `json:"action"`
	SourceID        []byte                  `json:"source_id"`
	PolicyID        []byte                  `json:"policy_id"`
	ObjectID        string                  `json:"object_id"`
	Input           json.RawMessage         `json:"input"`
}

type fleetInvokeWire struct {
	Action  fleetEnqueueWire `json:"action"`
	ClaimID string           `json:"claim_id"`
	Lease   time.Duration    `json:"lease"`
}

// The worker's request bodies decode into these private structs, not into
// pkg/dispatch/api's request types, although the shapes are the same: the
// error text decodeRequest forwards in a 400 names the destination types
// ("Go struct field fleetClaimWire.context of type
// webapi.fleetRequestContextWire"), and the golden fixtures in
// pkg/dispatch/api/testdata/wire pin those bytes. dispatch_parity_test.go
// holds each one to its public counterpart field by field, and responses are
// encoded from the public types directly.
type fleetClaimWire struct {
	Context         fleetRequestContextWire `json:"context"`
	ExpectedVersion uint64                  `json:"expected_version"`
	ClaimID         string                  `json:"claim_id"`
	Lease           time.Duration           `json:"lease"`
}

type fleetPullWire struct {
	Context fleetRequestContextWire `json:"context"`
	After   string                  `json:"after,omitempty"`
	Limit   int                     `json:"limit"`
}

type fleetCancelWire struct {
	Context         fleetRequestContextWire `json:"context"`
	ExpectedVersion uint64                  `json:"expected_version"`
	MutationKey     string                  `json:"mutation_key"`
}

// fleetCompletionWire is a remote worker reporting an outcome. It carries the
// same fields an in-process ActionExecutor returns, and the service validates
// them identically.
// fleetExtendWire renews a live claim. It carries the claim ID so the renewal
// is tied to the claim being held, not merely to the action.
type fleetExtendWire struct {
	Context         fleetRequestContextWire `json:"context"`
	ExpectedVersion uint64                  `json:"expected_version"`
	// ClaimFence binds the renewal to one claim attempt instead of to a
	// version, so a write that left the claim intact does not end it. See
	// fleet.ExtendRequest.ClaimFence. Optional, and omitted by callers that
	// still pin the version.
	ClaimFence uint64        `json:"claim_fence,omitempty"`
	ClaimID    string        `json:"claim_id"`
	Lease      time.Duration `json:"lease"`
}

// fleetAmbiguityWire is a lost-fence report.
//
// It names a fence rather than a claim ID: the record retains only the current
// claim ID, and the caller this route exists for has had its claim taken over,
// so the ID it held is gone from the record while its fence is not.
type fleetAmbiguityWire struct {
	Context         fleetRequestContextWire `json:"context"`
	ExpectedVersion uint64                  `json:"expected_version"`
	ClaimFence      uint64                  `json:"claim_fence"`
	Outcome         string                  `json:"outcome"`
	Target          string                  `json:"target,omitempty"`
	Reference       string                  `json:"reference,omitempty"`
}

type fleetCompletionWire struct {
	Context         fleetRequestContextWire `json:"context"`
	ExpectedVersion uint64                  `json:"expected_version"`
	ClaimID         string                  `json:"claim_id"`
	// ClaimFence is what binds this completion to the claim generation the
	// worker was handed, and this field is why the binding exists at all.
	//
	// Without it a remote worker is structurally unable to supply a fence, so
	// every HTTP completion takes completeClaim's legacy exact-version branch
	// — the strandable one that #438's ambiguity route made reachable. The
	// service-side fix shipped with no way for the only surface that can file
	// a report to use it, and a worker that tried to send the fence it was
	// handed on /claim got a 400, because decodeRequest sets
	// DisallowUnknownFields.
	//
	// Optional on the wire so a worker written against the previous shape
	// still completes, with the behaviour it was written against.
	ClaimFence uint64          `json:"claim_fence,omitempty"`
	Output     json.RawMessage `json:"output,omitempty"`
	ErrorCode  string          `json:"error_code,omitempty"`
	Failed     bool            `json:"failed,omitempty"`
	// Effected is how much of an irreversible egress happened before the
	// failure (#427).
	//
	// On this route and not only on /report, because this is the route the
	// effects gateway completes through — it is the component that performs
	// real external effects, so it is the one most likely to fail after a
	// partial one. A field validated in applyExecutionResult with no surface
	// able to set it is the shape of #435: the service-side mechanism ships
	// and the only client that can use it gets a 400, because decodeRequest
	// refuses unknown fields.
	//
	// Optional, so a worker written against the previous shape completes
	// exactly as before.
	Effected             *fleetEffectedWire  `json:"effected,omitempty"`
	EvidenceSnapshotID   string              `json:"evidence_snapshot_id,omitempty"`
	EvidenceSnapshotAsOf time.Time           `json:"evidence_snapshot_as_of,omitempty"`
	Evidence             []fleetEvidenceWire `json:"evidence,omitempty"`
}

// fleetEffectedWire is the volume of a partial egress. Fixed integers, for
// the reason fleet.EffectedVolume gives: a unit label would be
// caller-controlled text on a durable record.
//
// An upper bound on what may have left — bytes handed to the transport —
// never a receipt that they arrived. See fleet.EffectedVolume.
type fleetEffectedWire struct {
	Bytes  int64 `json:"bytes"`
	Chunks int64 `json:"chunks,omitempty"`
}

// fleetAmbiguityReportWire is one report as a record carries it.
type fleetAmbiguityReportWire = dispatchapi.AmbiguityReport

type fleetEvidenceWire struct {
	AnchorID   string                   `json:"anchor_id"`
	Kind       interaction.EvidenceKind `json:"kind"`
	Citation   *fleetCitationWire       `json:"citation,omitempty"`
	NodeIDs    []string                 `json:"node_ids"`
	EdgeIDs    []string                 `json:"edge_ids,omitempty"`
	Assertions []fleetAssertionWire     `json:"assertions,omitempty"`
	Visibility []string                 `json:"visibility"`
}

type fleetCitationWire struct {
	DocumentID string `json:"document_id"`
	RevisionID string `json:"revision_id"`
	SectionID  string `json:"section_id,omitempty"`
	SpanID     string `json:"span_id,omitempty"`
	Start      int64  `json:"start"`
	End        int64  `json:"end"`
	StartPage  int32  `json:"start_page,omitempty"`
	EndPage    int32  `json:"end_page,omitempty"`
}

type fleetAssertionWire struct {
	AssertionID string                   `json:"assertion_id"`
	EdgeID      string                   `json:"edge_id"`
	Origin      ontology.AssertionOrigin `json:"origin"`
}

// fleetActionWire is the record every dispatch route returns: the public
// dispatchapi.Action. What its fields carry, and why:
//
// Input is what the work actually is, and without it an out-of-process
// worker can pull an action, claim it under a fence and complete it without
// ever receiving the operation's parameters. It was absent from this wire
// while being present on the enqueue wire, validated against the action's
// InputSchema at enqueue, and stored on the record — so the data was
// correct and simply never read back out (#435).
//
// Nothing on a worker's side recovers this. A worker reconstructing a
// plausible request from the capability name would be fabricating the
// parameters of an effect Shoal cannot undo, which is why the gap had to be
// closed here rather than worked around there.
//
// Populated on the claim response alone — see encodeClaimedFleetAction, the
// one caller that sets it. Every other route uses encodeFleetAction and
// leaves it empty, which is both an exposure bound and a size bound.
//
// The exposure bound: a claim is the point at which a principal takes
// responsibility for performing the work, so it is the one transition whose
// caller must have the parameters. An earlier version of this field was
// emitted unconditionally on the argument that every consumer of this wire
// is scoped to the action's own principal, since Pull filtered on
// sameActionPrincipal. That was true when written and false as of #437,
// which lets a principal granted OperationExecute on the descriptor pull
// work it did not enqueue. Verified by composing the two branches and
// reading the input back as a non-enqueuing executor.
//
// The size bound is the reason it is not merely narrowed to Pull as well.
// MaxActionPayloadBytes is a fixed 1 MiB and a workspace's OutputBytes
// budget narrows to any non-zero value (workspace.ValidateBudgets rejects
// only zero), so a response carrying an input can exceed the budget that
// governs it. Pull returns up to MaxDispatchListResults — 256 — records,
// which would have put a quarter-gigabyte page against a 64 MiB default
// ceiling; and unlike Output, an Input is populated from enqueue onward, so
// it would have been present on every record of every page rather than only
// on completed ones. writeResponse answers an overflow on a route
// requestMayCommit reports as commit-bearing with 503 and
// X-Commit-Outcome: indeterminate, which on the enqueue echo would have
// meant a successful enqueue reported as an unknown outcome, repeatably,
// for as long as the record existed. One record on one route is the bound
// that keeps the page size independent of the payload ceiling.
//
// ErrorCodeOrigin says whether the service or the executor decided
// ErrorCode, which the code alone cannot tell a reader (#508). Omitted on
// a record written before the field existed, which a reader must treat as
// "either" rather than as "executor".
//
// Effected accompanies a failure that followed a partial irreversible
// egress (#427). Returned as well as accepted, so a worker that lost its
// response reads back what was recorded rather than re-deriving it —
// which it cannot, since the record is write-once and its own retry would
// be compared against it.
//
// ExecutorKey is the idempotency key an in-process executor already
// receives (dispatch_service.go hands it over as IdempotencyKey), derived
// at enqueue as a length-prefixed digest over a domain tag, the action ID
// and the caller's idempotency key. An out-of-process worker performing an
// irreversible effect needs exactly that value for exactly that purpose: it
// is the stable identity that lets a target deduplicate a re-claim after a
// lease lapse.
//
// Exposing it is strictly better than having each worker invent a key. A
// hand-written digest over the same inputs is easy to get subtly wrong —
// omitting the length prefixes makes it non-injective, so a caller able to
// enqueue against two surfaces can make one key collide with another's, and
// a collision makes a provider return a cached success without performing
// the effect. That is worse than a duplicate, because nothing records it.
//
// Set on the claim response alone, for the same reason as Input: it is the
// claimant that performs the effect and therefore needs the key. It is not
// a new disclosure — pkg/explorer/mcp returns fleet.ActionRecord whole, and
// that type carries no JSON tags, so the MCP enqueue and invoke tools
// already emit this value to their caller as "ExecutorKey" in padded
// standard base64. This wire spells it raw-URL like every other byte field
// here. Both decode to the same key, so a worker must compare decoded bytes
// and never the two spellings against each other.
//
// AmbiguityReports accompany EffectPossible wherever it appears, because
// the flag says an effect may have happened and these say what was
// attempted. An operator reading one without the other has the question
// and not the evidence.
//
// The claim history is deliberately *not* on this wire. It exists so the
// service can recognise a lapsed claimant, and publishing it would hand
// every execute-holder in the scope a list of which principals have held a
// record and under which fences — an identity disclosure the route does
// not need to make.
type fleetActionWire = dispatchapi.Action

func (w fleetEnqueueWire) decode(pathID []byte) (fleet.EnqueueRequest, error) {
	contextValue, err := w.Context.decode()
	if err != nil {
		return fleet.EnqueueRequest{}, err
	}
	id, err := decodeWireBytes("action ID", w.ID, false)
	if err != nil {
		return fleet.EnqueueRequest{}, err
	}
	if len(pathID) != 0 && string(id) != string(pathID) {
		return fleet.EnqueueRequest{}, shoal.NewError(shoal.ErrorInvalidArgument, "action ID does not match path")
	}
	key, err := decodeWireBytes("idempotency key", w.IdempotencyKey, false)
	if err != nil {
		return fleet.EnqueueRequest{}, err
	}
	agent, err := decodeID(w.AgentID)
	if err != nil {
		return fleet.EnqueueRequest{}, shoal.NewError(shoal.ErrorInvalidArgument, "agent ID "+err.Error())
	}
	object, err := decodeID(w.ObjectID)
	if err != nil {
		return fleet.EnqueueRequest{}, shoal.NewError(shoal.ErrorInvalidArgument, "object ID "+err.Error())
	}
	return fleet.EnqueueRequest{
		ID: id, IdempotencyKey: key, AgentID: agent, AgentGeneration: w.AgentGeneration,
		Capability: w.Capability, Action: w.Action, SourceID: append([]byte(nil), w.SourceID...),
		PolicyID: append([]byte(nil), w.PolicyID...), ObjectID: object,
		Input: append(json.RawMessage(nil), w.Input...), Context: contextValue,
	}, nil
}

func decodeWireBytes(name, encoded string, optional bool) ([]byte, error) {
	if encoded == "" && optional {
		return nil, nil
	}
	value, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(value) == 0 || len(value) > fleet.MaxActionIDBytes {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, name+" is invalid")
	}
	return value, nil
}

func decodeEvidence(values []fleetEvidenceWire) ([]fleet.EvidenceRef, error) {
	result := make([]fleet.EvidenceRef, len(values))
	for i, value := range values {
		var err error
		if result[i].AnchorID, err = decodeOptionalID(value.AnchorID); err != nil {
			return nil, err
		}
		result[i].Kind = value.Kind
		result[i].NodeIDs = make([]shoal.ID, len(value.NodeIDs))
		for index, id := range value.NodeIDs {
			if result[i].NodeIDs[index], err = decodeOptionalID(id); err != nil {
				return nil, err
			}
		}
		result[i].EdgeIDs = make([]shoal.ID, len(value.EdgeIDs))
		for index, id := range value.EdgeIDs {
			if result[i].EdgeIDs[index], err = decodeOptionalID(id); err != nil {
				return nil, err
			}
		}
		result[i].Assertions = make(
			[]interaction.AssertionReference, len(value.Assertions))
		for index, assertion := range value.Assertions {
			if result[i].Assertions[index].AssertionID, err =
				decodeOptionalID(assertion.AssertionID); err != nil {
				return nil, err
			}
			if result[i].Assertions[index].EdgeID, err =
				decodeOptionalID(assertion.EdgeID); err != nil {
				return nil, err
			}
			result[i].Assertions[index].Origin = assertion.Origin
		}
		if value.Citation != nil {
			citation := document.Citation{Range: document.SourceRange{
				Start: document.SourcePosition{
					Offset: value.Citation.Start, Page: value.Citation.StartPage},
				End: document.SourcePosition{
					Offset: value.Citation.End, Page: value.Citation.EndPage},
			}}
			if citation.DocumentID, err =
				decodeOptionalID(value.Citation.DocumentID); err != nil {
				return nil, err
			}
			if citation.RevisionID, err =
				decodeOptionalID(value.Citation.RevisionID); err != nil {
				return nil, err
			}
			if citation.SectionID, err =
				decodeOptionalID(value.Citation.SectionID); err != nil {
				return nil, err
			}
			if citation.SpanID, err =
				decodeOptionalID(value.Citation.SpanID); err != nil {
				return nil, err
			}
			result[i].Citation = citation
		}
		result[i].Visibility = append([]string(nil), value.Visibility...)
	}
	return result, nil
}

func encodeFleetAction(record fleet.ActionRecord) fleetActionWire {
	evidence := make([]dispatchapi.Evidence, len(record.Evidence))
	for i, item := range record.Evidence {
		evidence[i] = dispatchapi.Evidence{
			AnchorID: encodeFleetID(item.AnchorID), Kind: string(item.Kind),
			NodeIDs: encodeFleetIDs(item.NodeIDs), EdgeIDs: encodeFleetIDs(item.EdgeIDs),
			Assertions: make([]dispatchapi.Assertion, len(item.Assertions)),
			Visibility: append([]string(nil), item.Visibility...),
		}
		for index, assertion := range item.Assertions {
			evidence[i].Assertions[index] = dispatchapi.Assertion{
				AssertionID: encodeFleetID(assertion.AssertionID),
				EdgeID:      encodeFleetID(assertion.EdgeID), Origin: string(assertion.Origin),
			}
		}
		if item.Kind == interaction.EvidenceDocument {
			evidence[i].Citation = &dispatchapi.Citation{
				DocumentID: encodeFleetID(item.Citation.DocumentID),
				RevisionID: encodeFleetID(item.Citation.RevisionID),
				SectionID:  encodeFleetID(item.Citation.SectionID),
				SpanID:     encodeFleetID(item.Citation.SpanID),
				Start:      item.Citation.Range.Start.Offset,
				End:        item.Citation.Range.End.Offset,
				StartPage:  item.Citation.Range.Start.Page,
				EndPage:    item.Citation.Range.End.Page,
			}
		}
	}
	return fleetActionWire{
		ID: base64.RawURLEncoding.EncodeToString(record.ID), Version: record.Version,
		State:   dispatchapi.DispatchState(record.State),
		AgentID: encodeFleetID(record.AgentID), AgentGeneration: record.AgentGeneration,
		Capability: record.Capability, Action: record.Action,
		Output:          append(json.RawMessage(nil), record.Output...),
		ErrorCode:       record.ErrorCode,
		ErrorCodeOrigin: string(record.ErrorCodeOrigin),
		Effected:        encodeEffected(record.Effected),
		RequestID:       encodeFleetID(record.RequestID),
		CorrelationID:   encodeFleetID(record.CorrelationID), Deadline: record.Deadline,
		CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
		ClaimID: base64.RawURLEncoding.EncodeToString(record.ClaimID), ClaimFence: record.ClaimFence,
		ClaimLeaseUntil: record.ClaimLeaseUntil, EffectPossible: record.EffectPossible,
		AmbiguityReports:     encodeFleetAmbiguityReports(record.AmbiguityReports),
		EvidenceSnapshotID:   encodeFleetID(record.EvidenceSnapshotID),
		EvidenceSnapshotAsOf: record.EvidenceSnapshotAsOf, Evidence: evidence,
	}
}

// encodeClaimedFleetAction is encodeFleetAction plus the two fields a claimant
// needs in order to perform the work: the operation's parameters and the
// idempotency key to present to the target. Only the claim route calls it.
//
// Keeping these off encodeFleetAction is deliberate rather than tidy. Six of
// the seven routes that encode an action — enqueue, invoke, pull, complete,
// cancel and status — hand the record back to a caller that either supplied
// the input already or has no business performing the effect, and five of
// those are commit-bearing under requestMayCommit, where an over-budget
// response is reported as an indeterminate commit rather than a failure. So
// the default has to be the encoder that cannot carry a payload, and the
// exposure has to be one visible call site.
func encodeClaimedFleetAction(record fleet.ActionRecord) fleetActionWire {
	wire := encodeFleetAction(record)
	wire.Input = append(json.RawMessage(nil), record.Input...)
	wire.ExecutorKey = base64.RawURLEncoding.EncodeToString(record.ExecutorKey)
	return wire
}

func encodeFleetAmbiguityReports(
	reports []fleet.AmbiguityReport,
) []fleetAmbiguityReportWire {
	if len(reports) == 0 {
		return nil
	}
	result := make([]fleetAmbiguityReportWire, len(reports))
	for index, report := range reports {
		result[index] = fleetAmbiguityReportWire{
			ClaimFence: report.ClaimFence,
			Outcome:    dispatchapi.AmbiguityOutcome(report.Outcome),
			Target:     report.Target, Reference: report.Reference,
			Subject:    encodeFleetID(report.Subject),
			Actor:      encodeFleetID(report.Actor),
			ReportedAt: report.ReportedAt,
		}
	}
	return result
}

// decodeEffected reads an optional volume. Absent means nothing left, which
// is what a worker written before this field existed says by omitting it.
func decodeEffected(wire *fleetEffectedWire) fleet.EffectedVolume {
	if wire == nil {
		return fleet.EffectedVolume{}
	}
	return fleet.EffectedVolume{Bytes: wire.Bytes, Chunks: wire.Chunks}
}

func encodeEffected(value fleet.EffectedVolume) *dispatchapi.Effected {
	if value.Zero() {
		return nil
	}
	return &dispatchapi.Effected{Bytes: value.Bytes, Chunks: value.Chunks}
}

func encodeFleetIDs(values []shoal.ID) []string {
	result := make([]string, len(values))
	for index, value := range values {
		result[index] = encodeFleetID(value)
	}
	return result
}
