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

	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// FleetApprovalRoutePrefix is the authenticated mount for held requests
// (#451). It is a more specific subtree of FleetRoutePrefix and is mounted
// beside it, as the fleet-events subtree is.
const FleetApprovalRoutePrefix = "/api/v1/fleet/approvals/"

type FleetApprovalProvider interface {
	Request(context.Context, fleet.EnqueueRequest) (fleet.ApprovalReceipt, error)
	Decide(context.Context, fleet.ApprovalDecisionRequest) (fleet.ApprovalRecord, error)
	Pending(
		context.Context, fleet.PendingApprovalsRequest,
	) (fleet.PendingApprovalsPage, error)
	Status(context.Context, fleet.ApprovalStatusRequest) (fleet.ApprovalStatus, error)
}

// FleetApprovalWithdrawer is the optional withdrawal half of the approval
// surface (#655). A provider that implements it serves
// POST /api/v1/fleet/approvals/{id}/withdraw; one that does not answers that
// route 404, so an existing FleetApprovalProvider keeps compiling and keeps
// serving exactly what it served.
type FleetApprovalWithdrawer interface {
	Withdraw(context.Context, fleet.ApprovalWithdrawRequest) (fleet.ApprovalRecord, error)
}

// NewFleetApprovalHandler returns the approval surface without adding another
// authentication boundary. Mount it through Handler.MountAuthenticated at
// FleetApprovalRoutePrefix.
func NewFleetApprovalHandler(provider FleetApprovalProvider) (http.Handler, error) {
	if provider == nil {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "fleet approval provider is required")
	}
	mux := http.NewServeMux()
	mountFleetApproval(mux, provider)
	return mux, nil
}

func mountFleetApproval(mux *http.ServeMux, provider FleetApprovalProvider) {
	mux.HandleFunc("POST /api/v1/fleet/approvals/request",
		func(w http.ResponseWriter, r *http.Request) {
			var wire fleetEnqueueWire
			if err := decodeRequest(w, r, &wire); err != nil {
				writeError(w, shoal.NewError(shoal.ErrorInvalidArgument, err.Error()))
				return
			}
			request, err := wire.decode(nil)
			if err != nil {
				writeError(w, fleetApprovalError(err))
				return
			}
			receipt, err := provider.Request(r.Context(), request)
			if err != nil {
				writeError(w, fleetApprovalError(err))
				return
			}
			// Held, refused and expired are answers, not errors, for the
			// reason an admission denial is: the caller asked where its
			// request is, and a 4xx would collapse "this is where it is" into
			// "something went wrong". 202 while nothing has been created,
			// 201 once the request is work.
			status := http.StatusAccepted
			if receipt.State == fleet.ApprovalEnqueued {
				status = http.StatusCreated
			}
			writeResponse(w, status, encodeApprovalReceipt(receipt))
		})
	mux.HandleFunc("POST /api/v1/fleet/approvals/pending",
		func(w http.ResponseWriter, r *http.Request) {
			var wire fleetPullWire
			if err := decodeRequest(w, r, &wire); err != nil {
				writeError(w, shoal.NewError(shoal.ErrorInvalidArgument, err.Error()))
				return
			}
			contextValue, err := wire.Context.decode()
			if err != nil {
				writeError(w, fleetApprovalError(err))
				return
			}
			after, err := decodeWireBytes("approval cursor", wire.After, true)
			if err != nil {
				writeError(w, fleetApprovalError(err))
				return
			}
			page, err := provider.Pending(r.Context(), fleet.PendingApprovalsRequest{
				After: after, Limit: wire.Limit, Context: contextValue,
			})
			if err != nil {
				writeError(w, fleetApprovalError(err))
				return
			}
			// Summaries without inputs. A page carries up to
			// MaxApprovalListResults records and an input is bounded at
			// 1 MiB, so inputs on a page would put the response over a
			// narrowed output budget; status returns one record with its
			// input, which is what an approver reviews.
			approvals := make([]fleetApprovalWire, len(page.Approvals))
			for i, record := range page.Approvals {
				approvals[i] = encodeApproval(record, false)
			}
			writeResponse(w, http.StatusOK, struct {
				Approvals []fleetApprovalWire `json:"approvals"`
				Next      string              `json:"next,omitempty"`
			}{
				Approvals: approvals,
				Next:      base64.RawURLEncoding.EncodeToString(page.Next),
			})
		})
	mux.HandleFunc("POST /api/v1/fleet/approvals/{approval}/decide",
		func(w http.ResponseWriter, r *http.Request) {
			id, err := decodeWireBytes("approval ID", r.PathValue("approval"), false)
			if err != nil {
				writeError(w, fleetApprovalError(err))
				return
			}
			var wire fleetApprovalDecisionWire
			if err := decodeRequest(w, r, &wire); err != nil {
				writeError(w, shoal.NewError(shoal.ErrorInvalidArgument, err.Error()))
				return
			}
			contextValue, err := wire.Context.decode()
			if err != nil {
				writeError(w, fleetApprovalError(err))
				return
			}
			digest, err := base64.RawURLEncoding.DecodeString(wire.RequestDigest)
			if err != nil {
				writeError(w, shoal.NewError(
					shoal.ErrorInvalidArgument, "approval request digest is invalid"))
				return
			}
			record, err := provider.Decide(r.Context(), fleet.ApprovalDecisionRequest{
				ID: id, RequestDigest: digest,
				PolicyGeneration: wire.PolicyGeneration,
				Verdict:          wire.Verdict, Context: contextValue,
			})
			if err != nil {
				writeError(w, fleetApprovalError(err))
				return
			}
			writeResponse(w, http.StatusOK, encodeApproval(record, false))
		})
	if withdrawer, ok := provider.(FleetApprovalWithdrawer); ok {
		mux.HandleFunc("POST /api/v1/fleet/approvals/{approval}/withdraw",
			func(w http.ResponseWriter, r *http.Request) {
				id, err := decodeWireBytes(
					"approval ID", r.PathValue("approval"), false)
				if err != nil {
					writeError(w, fleetApprovalError(err))
					return
				}
				var wire fleetRequestContextWire
				if err := decodeRequest(w, r, &wire); err != nil {
					writeError(w, shoal.NewError(
						shoal.ErrorInvalidArgument, err.Error()))
					return
				}
				contextValue, err := wire.decode()
				if err != nil {
					writeError(w, fleetApprovalError(err))
					return
				}
				record, err := withdrawer.Withdraw(
					r.Context(), fleet.ApprovalWithdrawRequest{
						ID: id, Context: contextValue,
					})
				if err != nil {
					writeError(w, fleetApprovalError(err))
					return
				}
				writeResponse(w, http.StatusOK, encodeApproval(record, false))
			})
	}
	mux.HandleFunc("POST /api/v1/fleet/approvals/{approval}/status",
		func(w http.ResponseWriter, r *http.Request) {
			id, err := decodeWireBytes("approval ID", r.PathValue("approval"), false)
			if err != nil {
				writeError(w, fleetApprovalError(err))
				return
			}
			var wire fleetRequestContextWire
			if err := decodeRequest(w, r, &wire); err != nil {
				writeError(w, shoal.NewError(shoal.ErrorInvalidArgument, err.Error()))
				return
			}
			contextValue, err := wire.decode()
			if err != nil {
				writeError(w, fleetApprovalError(err))
				return
			}
			status, err := provider.Status(r.Context(), fleet.ApprovalStatusRequest{
				ID: id, Context: contextValue,
			})
			if err != nil {
				writeError(w, fleetApprovalError(err))
				return
			}
			// state is what the request effectively is now; stored_state is
			// the row. They differ when expiry has not been written yet, or
			// when the request can no longer progress, and condition says
			// which.
			encoded := encodeApproval(status.Approval, true)
			encoded.StoredState = encoded.State
			encoded.State = status.State
			encoded.Condition = status.Condition
			writeResponse(w, http.StatusOK, encoded)
		})
}

// fleetApprovalError maps approval failures on to transport shapes. Every one
// of them is already a shoal error carrying a deliberate code; the sentinels
// are matched here as well so a provider that returns a bare sentinel is not
// answered with a 500.
func fleetApprovalError(err error) error {
	switch {
	case err == nil:
		return nil
	// The transition committed and only its lifecycle event did not publish
	// (#653). Unavailable and deliberately unmarked: nothing external was
	// contacted, and the same request replays the transition and publishes
	// its event, so a retry is exactly right. First, so that whatever the
	// publication failed with (a conflict closing the row, say) cannot be
	// read as the transition itself failing. It never carries a committed
	// effect, since materialization answers the event only after the action
	// write succeeded, but the exclusions keep it from shadowing one if that
	// ever changes.
	case errors.Is(err, fleet.ErrApprovalEventUnpublished) &&
		!errors.Is(err, fleet.ErrActionCommitted) &&
		!errors.Is(err, fleet.ErrExecutionAmbiguous):
		return shoal.WrapError(
			shoal.ErrorUnavailable,
			"fleet approval transition committed; retry to publish its event", err)
	// Above the approver mapping and the generic conflict arm (#526): a
	// moved identity scheme makes the mapping comparison meaningless, and a
	// caller must be told that the request can only expire, not that it
	// conflicts.
	case errors.Is(err, fleet.ErrIdentitySchemeMoved):
		if shoal.IsErrorCode(err, shoal.ErrorConflict) {
			return err
		}
		return shoal.WrapError(
			shoal.ErrorConflict, "fleet approval identity scheme moved", err)
	case errors.Is(err, fleet.ErrApprovalConflict),
		errors.Is(err, fleet.ErrApprovalExpired),
		errors.Is(err, fleet.ErrApprovalWithdrawn),
		errors.Is(err, fleet.ErrApprovalTerminalStatesDisabled),
		errors.Is(err, fleet.ErrApprovalStranded),
		errors.Is(err, fleet.ErrApprovalSuperseded),
		errors.Is(err, fleet.ErrApproverMappingMoved):
		if shoal.IsErrorCode(err, shoal.ErrorConflict) {
			return err
		}
		return shoal.WrapError(shoal.ErrorConflict, "fleet approval conflict", err)
	case errors.Is(err, fleet.ErrApprovalNotFound):
		return shoal.WrapError(shoal.ErrorNotFound, "object not found", err)
	default:
		return fleetDispatchError(err)
	}
}

type fleetApprovalDecisionWire struct {
	Context          fleetRequestContextWire `json:"context"`
	RequestDigest    string                  `json:"request_digest"`
	PolicyGeneration int64                   `json:"policy_generation"`
	Verdict          fleet.ApprovalVerdict   `json:"verdict"`
}

type fleetApprovalReceiptWire struct {
	ID               string              `json:"id"`
	State            fleet.ApprovalState `json:"state"`
	RequestDigest    string              `json:"request_digest"`
	PolicyGeneration int64               `json:"policy_generation"`
	ExpiresAt        time.Time           `json:"expires_at"`
	Action           *fleetActionWire    `json:"action,omitempty"`
}

func encodeApprovalReceipt(receipt fleet.ApprovalReceipt) fleetApprovalReceiptWire {
	result := fleetApprovalReceiptWire{
		ID:               base64.RawURLEncoding.EncodeToString(receipt.ID),
		State:            receipt.State,
		RequestDigest:    base64.RawURLEncoding.EncodeToString(receipt.RequestDigest),
		PolicyGeneration: receipt.PolicyGeneration,
		ExpiresAt:        receipt.ExpiresAt,
	}
	if receipt.State == fleet.ApprovalEnqueued {
		action := encodeFleetAction(receipt.Action)
		result.Action = &action
	}
	return result
}

// fleetApprovalWire is what an approver reviews. RequestDigest and
// PolicyGeneration are what it must send back to decide, so it can only ever
// approve the request it was shown.
type fleetApprovalWire struct {
	ID      string              `json:"id"`
	Version uint64              `json:"version"`
	State   fleet.ApprovalState `json:"state"`
	// StoredState and Condition are set by status alone; see the route.
	StoredState      fleet.ApprovalState     `json:"stored_state,omitempty"`
	Condition        fleet.ApprovalCondition `json:"condition,omitempty"`
	RequestDigest    string                  `json:"request_digest"`
	PolicyGeneration int64                   `json:"policy_generation"`
	AgentID          string                  `json:"agent_id"`
	AgentGeneration  int64                   `json:"agent_generation"`
	Capability       string                  `json:"capability"`
	Action           string                  `json:"action"`
	SourceID         []byte                  `json:"source_id"`
	PolicyID         []byte                  `json:"policy_id"`
	ObjectID         string                  `json:"object_id"`
	// Input is present on status only; see the pending route.
	Input          json.RawMessage       `json:"input,omitempty"`
	Requester      string                `json:"requester"`
	RequesterActor string                `json:"requester_actor"`
	Deadline       time.Time             `json:"deadline"`
	RequestedAt    time.Time             `json:"requested_at"`
	ExpiresAt      time.Time             `json:"expires_at"`
	Verdict        fleet.ApprovalVerdict `json:"verdict,omitempty"`
	Approver       string                `json:"approver,omitempty"`
	ApproverActor  string                `json:"approver_actor,omitempty"`
	DecidedAt      *time.Time            `json:"decided_at,omitempty"`
	// WithdrawnAt is set on a withdrawn request (#655). The withdrawal's
	// reason is recorded and audited and, like the request's own reason,
	// not served.
	WithdrawnAt *time.Time `json:"withdrawn_at,omitempty"`
}

func encodeApproval(record fleet.ApprovalRecord, withInput bool) fleetApprovalWire {
	request := record.Request
	result := fleetApprovalWire{
		ID: base64.RawURLEncoding.EncodeToString(record.ID), Version: record.Version,
		State:            record.State,
		RequestDigest:    base64.RawURLEncoding.EncodeToString(record.RequestDigest),
		PolicyGeneration: record.PolicyGeneration,
		AgentID:          encodeFleetID(request.AgentID),
		AgentGeneration:  request.AgentGeneration,
		Capability:       request.Capability, Action: request.Action,
		SourceID:       append([]byte(nil), request.SourceID...),
		PolicyID:       append([]byte(nil), request.PolicyID...),
		ObjectID:       encodeFleetID(request.ObjectID),
		Requester:      encodeFleetID(request.Subject),
		RequesterActor: encodeFleetID(request.Actor),
		Deadline:       request.Deadline, RequestedAt: record.RequestedAt,
		ExpiresAt: record.ExpiresAt, Verdict: record.Verdict,
		Approver:      encodeFleetID(record.ApproverSubject),
		ApproverActor: encodeFleetID(record.ApproverActor),
	}
	if !record.DecidedAt.IsZero() {
		decided := record.DecidedAt
		result.DecidedAt = &decided
	}
	if !record.WithdrawnAt.IsZero() {
		withdrawn := record.WithdrawnAt
		result.WithdrawnAt = &withdrawn
	}
	if withInput {
		result.Input = append(json.RawMessage(nil), request.Input...)
	}
	return result
}
