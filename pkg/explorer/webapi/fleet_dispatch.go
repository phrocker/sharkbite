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
	ClaimMaterial(context.Context, fleet.RecoveryClaimRequest) (fleet.ActionRecord, error)
	PrepareClaim(context.Context, fleet.PrepareClaimRequest) (fleet.ActionRecord, error)
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
		// The legacy remote route returned executable Input and ExecutorKey
		// without a durable Prepare. Reject it before any Claim mutation.
		w.Header().Set("Shoal-Recovery-Refusal", string(fleet.RecoveryLegacyClaim))
		writeError(w, shoal.NewError(shoal.ErrorInvalidArgument,
			"remote claim requires the prepared gateway protocol"))
	})
	mux.HandleFunc("POST /api/v1/fleet/actions/{action}/claim-prepared", func(w http.ResponseWriter, r *http.Request) {
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
		result, err := provider.Claim(r.Context(), fleet.ClaimRequest{ID: actionID, ExpectedVersion: wire.ExpectedVersion, ClaimID: claimID, Lease: wire.Lease, Prepared: true, Context: contextValue})
		if err != nil {
			writeFleetRecoveryError(w, err)
			return
		}
		writeResponse(w, http.StatusOK, encodeFleetAction(result))
	})
	mux.HandleFunc("POST /api/v1/fleet/actions/{action}/material", func(w http.ResponseWriter, r *http.Request) {
		actionID, request, err := decodeFleetRecoveryClaim(w, r)
		if err != nil {
			writeError(w, fleetDispatchError(err))
			return
		}
		request.ID = actionID
		result, err := provider.ClaimMaterial(r.Context(), request)
		if err != nil {
			writeFleetRecoveryError(w, err)
			return
		}
		writeResponse(w, http.StatusOK, dispatchapi.ClaimMaterial{
			ID:          base64.RawURLEncoding.EncodeToString(result.ID),
			ClaimID:     base64.RawURLEncoding.EncodeToString(result.ClaimID),
			ClaimFence:  result.ClaimFence,
			Input:       append(json.RawMessage(nil), result.Input...),
			ExecutorKey: base64.RawURLEncoding.EncodeToString(result.ExecutorKey),
		})
	})
	mux.HandleFunc("POST /api/v1/fleet/actions/{action}/prepare", func(w http.ResponseWriter, r *http.Request) {
		actionID, err := decodeWireBytes("action ID", r.PathValue("action"), false)
		if err != nil {
			writeError(w, fleetDispatchError(err))
			return
		}
		var wire dispatchapi.PrepareRequest
		if err := decodeRequest(w, r, &wire); err != nil {
			writeError(w, shoal.NewError(shoal.ErrorInvalidArgument, err.Error()))
			return
		}
		contextValue, err := fleetRequestContextWire(wire.Context).decode()
		if err != nil {
			writeError(w, fleetDispatchError(err))
			return
		}
		claimID, err := decodeWireBytes("claim ID", wire.ClaimID, false)
		if err != nil {
			writeError(w, fleetDispatchError(err))
			return
		}
		requestDigest, err := decodeRecoveryDigest("request digest", wire.RequestDigest)
		if err != nil {
			writeError(w, err)
			return
		}
		scopeDigest, err := decodeRecoveryDigest("target scope digest", wire.TargetScopeDigest)
		if err != nil {
			writeError(w, err)
			return
		}
		result, err := provider.PrepareClaim(r.Context(), fleet.PrepareClaimRequest{
			RecoveryClaimRequest: fleet.RecoveryClaimRequest{
				ID: actionID, ClaimID: claimID, ClaimFence: wire.ClaimFence,
				Context: contextValue,
			},
			Mode:          fleet.RecoveryMode(wire.Mode),
			RequestDigest: requestDigest, TargetScopeDigest: scopeDigest,
		})
		if err != nil {
			writeFleetRecoveryError(w, err)
			return
		}
		if result.RecoveryCommitment == nil {
			writeError(w, shoal.NewError(shoal.ErrorInternal, "prepare returned no commitment"))
			return
		}
		writeResponse(w, http.StatusOK, dispatchapi.PrepareReceipt{
			ID:         base64.RawURLEncoding.EncodeToString(result.ID),
			ClaimID:    base64.RawURLEncoding.EncodeToString(result.ClaimID),
			ClaimFence: result.ClaimFence, Version: result.Version,
			Committed:         result.RecoveryCommitment != nil,
			Mode:              string(result.RecoveryCommitment.Mode),
			RequestDigest:     base64.RawURLEncoding.EncodeToString(result.RecoveryCommitment.RequestDigest[:]),
			TargetScopeDigest: base64.RawURLEncoding.EncodeToString(result.RecoveryCommitment.TargetScopeDigest[:]),
		})
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

func writeFleetRecoveryError(w http.ResponseWriter, err error) {
	var refusal *fleet.RecoveryRefusalError
	if errors.As(err, &refusal) && refusal.Code.Valid() {
		w.Header().Set("Shoal-Recovery-Refusal", string(refusal.Code))
	}
	writeError(w, fleetDispatchError(err))
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
	// Cancel refused before its audit or store write. The prior claim may have
	// executed, but this cancellation did not commit.
	case errors.Is(err, fleet.ErrCancelClaimUncertain):
		return shoal.WrapError(shoal.ErrorConflict,
			"cancellation refused: lapsed claim may have executed; reconcile the claim", err)
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
	NotBefore       time.Time               `json:"not_before,omitzero"`
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

type fleetRecoveryClaimWire struct {
	Context    fleetRequestContextWire `json:"context"`
	ClaimID    string                  `json:"claim_id"`
	ClaimFence uint64                  `json:"claim_fence"`
}

func decodeFleetRecoveryClaim(
	w http.ResponseWriter, r *http.Request,
) ([]byte, fleet.RecoveryClaimRequest, error) {
	actionID, err := decodeWireBytes("action ID", r.PathValue("action"), false)
	if err != nil {
		return nil, fleet.RecoveryClaimRequest{}, err
	}
	var wire fleetRecoveryClaimWire
	if err := decodeRequest(w, r, &wire); err != nil {
		return nil, fleet.RecoveryClaimRequest{}, shoal.NewError(shoal.ErrorInvalidArgument, err.Error())
	}
	contextValue, err := wire.Context.decode()
	if err != nil {
		return nil, fleet.RecoveryClaimRequest{}, err
	}
	claimID, err := decodeWireBytes("claim ID", wire.ClaimID, false)
	if err != nil {
		return nil, fleet.RecoveryClaimRequest{}, err
	}
	return actionID, fleet.RecoveryClaimRequest{
		ClaimID: claimID, ClaimFence: wire.ClaimFence, Context: contextValue,
	}, nil
}

func decodeRecoveryDigest(name, encoded string) ([32]byte, error) {
	var digest [32]byte
	value, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(value) != len(digest) {
		return digest, shoal.NewError(shoal.ErrorInvalidArgument, name+" is invalid")
	}
	copy(digest[:], value)
	return digest, nil
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

// fleetActionWire is the public action projection. Input, ExecutorKey and
// recovery commitment digests are deliberately absent on every ordinary
// route, including Pull, Status and the prepared Claim response. Executable
// material is returned only by the authenticated claimant-only Material route;
// its answer is not a send grant. Prepare must durably confirm the exact bound
// request before the gateway may contact the target.
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
		Input:     append(json.RawMessage(nil), w.Input...),
		NotBefore: w.NotBefore, Context: contextValue,
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
	var recovery *dispatchapi.RecoveryStatus
	if committed := record.RecoveryCommitment; committed != nil {
		recovery = &dispatchapi.RecoveryStatus{
			Version: committed.Version, Mode: string(committed.Mode),
			PreparedFence: committed.PreparedFence, PreparedAt: committed.PreparedAt,
		}
	}
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
		NotBefore: record.NotBefore,
		CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
		ClaimID: base64.RawURLEncoding.EncodeToString(record.ClaimID), ClaimFence: record.ClaimFence,
		ClaimLeaseUntil: record.ClaimLeaseUntil, EffectPossible: record.EffectPossible,
		RecoveryProtocolVersion: record.RecoveryProtocolVersion,
		RecoveryCommitment:      recovery,
		AmbiguityReports:        encodeFleetAmbiguityReports(record.AmbiguityReports),
		EvidenceSnapshotID:      encodeFleetID(record.EvidenceSnapshotID),
		EvidenceSnapshotAsOf:    record.EvidenceSnapshotAsOf, Evidence: evidence,
	}
}

// EncodeFleetAction returns the public action receipt used by non-claim
// routes. It omits execution input, idempotency keys, and internal
// authorization metadata. MCP uses this projection so it cannot expose more
// than the HTTP status route for the same action.
func EncodeFleetAction(record fleet.ActionRecord) dispatchapi.Action {
	return encodeFleetAction(record)
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
