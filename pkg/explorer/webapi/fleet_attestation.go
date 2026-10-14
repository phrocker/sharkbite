// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package webapi

import (
	"context"
	"errors"
	"net/http"

	attestationapi "github.com/phrocker/shoal-oss/pkg/attestation/api"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// FleetAttestationProvider serves executor attestation presentations. The
// principal is the authenticated caller; nothing in the request names one.
type FleetAttestationProvider interface {
	Present(context.Context, fleet.AttestationPresentation) (fleet.AttestationReceipt, error)
}

// NewFleetHandlerWithAttestation is NewFleetHandler plus the executor
// attestation presentation route, mounted on the same authenticated fleet
// prefix as the dispatch (execute) routes and gated the same way: the
// provider requires OperationExecute and a correlation ID.
func NewFleetHandlerWithAttestation(
	registry FleetRegistryProvider,
	dispatch FleetDispatchProvider,
	attestation FleetAttestationProvider,
) (http.Handler, error) {
	if registry == nil || dispatch == nil || isAbsentInterface(attestation) {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument,
			"fleet registry, dispatch and attestation providers are required")
	}
	mux := http.NewServeMux()
	mountFleetRegistry(mux, registry)
	mountFleetDispatch(mux, dispatch)
	mountFleetAttestation(mux, attestation)
	return mux, nil
}

func mountFleetAttestation(mux *http.ServeMux, provider FleetAttestationProvider) {
	mux.HandleFunc("POST "+attestationapi.Route, func(w http.ResponseWriter, r *http.Request) {
		var wire attestationapi.Request
		if err := decodeRequest(w, r, &wire); err != nil {
			writeError(w, shoal.NewError(shoal.ErrorInvalidArgument,
				"invalid attestation request"))
			return
		}
		key, keyErr := attestationapi.Decode(wire.IdempotencyKey)
		report, reportErr := attestationapi.Decode(wire.Report)
		if keyErr != nil || reportErr != nil {
			writeError(w, shoal.NewError(shoal.ErrorInvalidArgument,
				"invalid attestation request"))
			return
		}
		receipt, err := provider.Present(r.Context(), fleet.AttestationPresentation{
			ExecutorRef: wire.ExecutorRef, IdempotencyKey: key, Report: report,
		})
		if err != nil {
			writeError(w, fleetAttestationError(err))
			return
		}
		writeResponse(w, http.StatusOK, attestationapi.Receipt{
			AttestationID: string(receipt.AttestationID),
			ExpiresAt:     receipt.ExpiresAt.UTC(),
		})
	})
}

// fleetAttestationError keeps every verification refusal to one opaque
// answer: unauthorized, "attestation refused". It names no verifier, digest
// or reason.
func fleetAttestationError(err error) error {
	switch {
	case errors.Is(err, fleet.ErrAttestationRefused):
		return shoal.NewError(shoal.ErrorUnauthorized, attestationapi.RefusedMessage)
	case errors.Is(err, fleet.ErrRecordingUnavailable):
		return shoal.NewError(shoal.ErrorUnavailable,
			"executor attestation recording is unavailable")
	case errors.Is(err, fleet.ErrAttestationUnavailable):
		return shoal.NewError(shoal.ErrorUnavailable, "executor attestation is unavailable")
	default:
		return err
	}
}
