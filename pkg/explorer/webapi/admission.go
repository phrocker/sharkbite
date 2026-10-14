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

	admissionapi "github.com/phrocker/shoal-oss/pkg/admission/api"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// AdmissionRoutePrefix is the authenticated mount for the pre-call admission
// seam.
//
// It is an HTTP surface rather than a Go interface because the caller is a
// different binary by construction. A process that holds the prompt and speaks
// to a third-party endpoint must not share an address space with the policy
// store and the corpus: that would put untrusted prompt content inside the
// decision plane.
//
// The wire types are pkg/admission/api's, so the contract an external caller
// compiles against is the one served here. Golden fixtures in
// pkg/admission/api/testdata/wire pin the bytes (admission_golden_test.go).
const AdmissionRoutePrefix = admissionapi.RoutePrefix

type AdmissionProvider interface {
	Request(context.Context, fleet.AdmissionRequest) (fleet.AdmissionGrant, error)
	Report(context.Context, fleet.AdmissionReport) (fleet.ActionRecord, error)
	Outstanding(
		context.Context, fleet.OutstandingAdmissionsRequest,
	) (fleet.OutstandingAdmissionsPage, error)
}

// NewAdmissionHandler returns the admission surface without adding another
// authentication boundary. Mount it through Handler.MountAuthenticated at
// AdmissionRoutePrefix.
func NewAdmissionHandler(provider AdmissionProvider) (http.Handler, error) {
	if provider == nil {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "admission provider is required")
	}
	mux := http.NewServeMux()
	mountAdmission(mux, provider)
	return mux, nil
}

func mountAdmission(mux *http.ServeMux, provider AdmissionProvider) {
	mux.HandleFunc(
		"POST "+admissionapi.RequestRoute,
		func(w http.ResponseWriter, r *http.Request) {
			var wire admissionRequestWire
			if err := decodeRequest(w, r, &wire); err != nil {
				writeError(w, shoal.NewError(shoal.ErrorInvalidArgument, err.Error()))
				return
			}
			request, err := decodeAdmissionRequest(admissionapi.Request(wire))
			if err != nil {
				writeError(w, admissionError(err))
				return
			}
			grant, err := provider.Request(r.Context(), request)
			if err != nil {
				writeError(w, admissionError(err))
				return
			}
			// A refusal is a successful response, not an error status. The
			// caller has to be able to tell "Shoal decided no" from "Shoal could
			// not be reached", because the two demand opposite handling: the
			// first is the answer, the second is an outage the caller must fail
			// closed on itself. Encoding a denial as a 4xx would collapse them.
			writeResponse(w, http.StatusOK, encodeAdmissionGrant(grant))
		})
	mux.HandleFunc(
		"POST "+admissionapi.ReportRoute,
		func(w http.ResponseWriter, r *http.Request) {
			var wire admissionReportWire
			if err := decodeRequest(w, r, &wire); err != nil {
				writeError(w, shoal.NewError(shoal.ErrorInvalidArgument, err.Error()))
				return
			}
			report, err := decodeAdmissionReport(admissionapi.Report(wire))
			if err != nil {
				writeError(w, admissionError(err))
				return
			}
			record, err := provider.Report(r.Context(), report)
			if err != nil {
				writeError(w, admissionError(err))
				return
			}
			writeResponse(w, http.StatusOK, encodeAdmissionReceipt(record))
		})
	mux.HandleFunc(
		"POST "+admissionapi.OutstandingRoute,
		func(w http.ResponseWriter, r *http.Request) {
			var wire admissionOutstandingWire
			if err := decodeRequest(w, r, &wire); err != nil {
				writeError(w, shoal.NewError(shoal.ErrorInvalidArgument, err.Error()))
				return
			}
			contextValue, err := decodeAdmissionContext(wire.Context)
			if err != nil {
				writeError(w, admissionError(err))
				return
			}
			after, err := decodeWireBytes("admission cursor", wire.After, true)
			if err != nil {
				writeError(w, admissionError(err))
				return
			}
			page, err := provider.Outstanding(
				r.Context(), fleet.OutstandingAdmissionsRequest{
					After: after, Limit: wire.Limit, Context: contextValue,
				})
			if err != nil {
				writeError(w, admissionError(err))
				return
			}
			writeResponse(w, http.StatusOK, encodeOutstandingAdmissions(page))
		})
}

// admissionError maps service failures on to transport shapes.
//
// A spent token is a conflict, never a not-found and never an unauthorized. All
// three would be true of different spent tokens — reported, cancelled, never
// issued — and distinguishing them would tell a caller whether an admission it
// does not hold exists.
func admissionError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, fleet.ErrAdmissionSpent),
		errors.Is(err, fleet.ErrAdmissionConflict):
		return shoal.WrapError(
			shoal.ErrorConflict, "admission token is not live", err)
	// These are whole-store rollout conditions, not caller-specific denials
	// or conflicts. New admission requests stay unavailable while any principal has a
	// legacy admission or an ordinary action occupies the reserved span.
	// An operator must clear the condition. The fixed messages identify the
	// rollout condition without naming the affected record or its principal.
	case errors.Is(err, fleet.ErrAdmissionUnmigrated),
		errors.Is(err, fleet.ErrAdmissionSpanOccupied):
		return shoal.WrapError(
			shoal.ErrorUnavailable, err.Error(), err)
	default:
		return fleetDispatchError(err)
	}
}

// The request bodies decode into these local names for the public types, and
// nothing else. encoding/json names the destination type in its error text
// ("cannot unmarshal array into Go value of type webapi.admissionRequestWire"),
// decodeRequest forwards that text in the 400 body, and the golden fixtures
// pin it. Decoding straight into admissionapi.Request would change a response
// byte for no reason. Nested type-mismatch messages name the nested type and
// do change (fleetRequestContextWire and admissionTokenWire became
// api.RequestContext and api.Token); status and code do not.
type (
	admissionRequestWire     admissionapi.Request
	admissionReportWire      admissionapi.Report
	admissionOutstandingWire admissionapi.OutstandingRequest
)

// decodeAdmissionContext reuses the fleet routes' context decoding. The two
// structs are field-for-field identical, which the conversion enforces at
// compile time.
func decodeAdmissionContext(
	wire admissionapi.RequestContext,
) (fleet.RequestContext, error) {
	return fleetRequestContextWire(wire).decode()
}

func decodeAdmissionRequest(w admissionapi.Request) (fleet.AdmissionRequest, error) {
	contextValue, err := decodeAdmissionContext(w.Context)
	if err != nil {
		return fleet.AdmissionRequest{}, err
	}
	id, err := decodeWireBytes("admission ID", w.ID, false)
	if err != nil {
		return fleet.AdmissionRequest{}, err
	}
	key, err := decodeWireBytes("idempotency key", w.IdempotencyKey, false)
	if err != nil {
		return fleet.AdmissionRequest{}, err
	}
	tokenID, err := decodeWireBytes("admission token ID", w.TokenID, false)
	if err != nil {
		return fleet.AdmissionRequest{}, err
	}
	agent, err := decodeID(w.AgentID)
	if err != nil {
		return fleet.AdmissionRequest{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "agent ID "+err.Error())
	}
	object, err := decodeID(w.ObjectID)
	if err != nil {
		return fleet.AdmissionRequest{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "object ID "+err.Error())
	}
	disclosures, err := decodeIDs(w.Disclosures)
	if err != nil {
		return fleet.AdmissionRequest{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "admission disclosure "+err.Error())
	}
	effects := make(fleet.Effects, len(w.Effects))
	for i, effect := range w.Effects {
		// Decoded verbatim, including a class this build does not know. The
		// service refuses an unrecognised declaration; translating one to an
		// empty set or dropping it here would turn a request for something
		// unknown into a request for nothing, which is the most permissive
		// reading of the least classifiable input.
		effects[i] = fleet.Effect(effect)
	}
	return fleet.AdmissionRequest{
		ID: id, IdempotencyKey: key, TokenID: tokenID, AgentID: agent,
		AgentGeneration: w.AgentGeneration, Capability: w.Capability,
		Action:   w.Action,
		SourceID: append([]byte(nil), w.SourceID...),
		PolicyID: append([]byte(nil), w.PolicyID...),
		ObjectID: object, Effects: effects,
		Input:       append(json.RawMessage(nil), w.Input...),
		Disclosures: disclosures, Lease: w.Lease, Context: contextValue,
	}, nil
}

func decodeAdmissionToken(w admissionapi.Token) (fleet.AdmissionToken, error) {
	actionID, err := decodeWireBytes("admission action ID", w.ActionID, false)
	if err != nil {
		return fleet.AdmissionToken{}, err
	}
	tokenID, err := decodeWireBytes("admission token ID", w.TokenID, false)
	if err != nil {
		return fleet.AdmissionToken{}, err
	}
	return fleet.AdmissionToken{
		ActionID: actionID, TokenID: tokenID,
		Version: w.Version, ExpiresAt: w.ExpiresAt,
	}, nil
}

// encodeAdmissionGrant renders the answer.
//
// Withhold is the obligation. It is always present on an allowed response,
// empty when there is nothing to withhold, so a caller that reads the field
// without checking the outcome still sees the full obligation rather than a
// missing key it might read as "no obligation".
//
// Encoded exactly as the request's disclosures are, because the obligation is
// a subset of them and a caller has to be able to match the two by equality.
// Emitting raw identity bytes here against base64url on the way in would make
// every obligation unmatchable for any identity that is not already printable
// ASCII.
func encodeAdmissionGrant(grant fleet.AdmissionGrant) admissionapi.Grant {
	result := admissionapi.Grant{
		Outcome:  admissionapi.Outcome(grant.Outcome),
		Withhold: make([]string, 0, len(grant.Obligations.Withhold)),
	}
	for _, reference := range grant.Obligations.Withhold {
		result.Withhold = append(result.Withhold, encodeFleetID(reference))
	}
	// A denial carries no token. Issuing one would mean a refused caller could
	// report against it, and a report is a statement that an effect occurred.
	if grant.Outcome == fleet.AdmissionDenied {
		return result
	}
	result.Token = &admissionapi.Token{
		ActionID: base64.RawURLEncoding.EncodeToString(grant.Token.ActionID),
		TokenID:  base64.RawURLEncoding.EncodeToString(grant.Token.TokenID),
		Version:  grant.Token.Version, ExpiresAt: grant.Token.ExpiresAt,
	}
	return result
}

func decodeAdmissionReport(w admissionapi.Report) (fleet.AdmissionReport, error) {
	contextValue, err := decodeAdmissionContext(w.Context)
	if err != nil {
		return fleet.AdmissionReport{}, err
	}
	token, err := decodeAdmissionToken(w.Token)
	if err != nil {
		return fleet.AdmissionReport{}, err
	}
	// Absent means nothing left, which is the same thing a caller written
	// before this field existed says by omitting it. A present object with
	// zero bytes is also nothing left; the service refuses it on a success
	// and bounds it on a failure, so neither spelling needs a decision here.
	var effected fleet.EffectedVolume
	if w.Effected != nil {
		effected = fleet.EffectedVolume{
			Bytes: w.Effected.Bytes, Chunks: w.Effected.Chunks,
		}
	}
	return fleet.AdmissionReport{
		Token:   token,
		Outcome: append(json.RawMessage(nil), w.Outcome...),
		Failed:  w.Failed, ErrorCode: w.ErrorCode, Effected: effected,
		Context: contextValue,
	}, nil
}

func encodeAdmissionReceipt(record fleet.ActionRecord) admissionapi.Receipt {
	return admissionapi.Receipt{
		ActionID:   base64.RawURLEncoding.EncodeToString(record.ID),
		Version:    record.Version,
		State:      admissionapi.DispatchState(record.State),
		ReportedAt: record.UpdatedAt,
	}
}

func encodeOutstandingAdmissions(
	page fleet.OutstandingAdmissionsPage,
) admissionapi.OutstandingPage {
	admissions := make([]admissionapi.OutstandingAdmission, len(page.Admissions))
	for i, admission := range page.Admissions {
		admissions[i] = admissionapi.OutstandingAdmission{
			ActionID: base64.RawURLEncoding.EncodeToString(admission.ActionID),
			TokenID:  base64.RawURLEncoding.EncodeToString(admission.TokenID),
			Version:  admission.Version, AdmittedAt: admission.AdmittedAt,
			ExpiresAt: admission.ExpiresAt, Expired: admission.Expired,
		}
	}
	return admissionapi.OutstandingPage{
		Admissions: admissions,
		Next:       base64.RawURLEncoding.EncodeToString(page.Next),
	}
}
