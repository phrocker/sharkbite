// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package webapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	admissionapi "github.com/phrocker/shoal-oss/pkg/admission/api"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type stubAdmissionProvider struct {
	resolver auth.Resolver
	seen     fleet.AdmissionRequest
	grant    fleet.AdmissionGrant
	report   fleet.AdmissionReport
	record   fleet.ActionRecord
	page     fleet.OutstandingAdmissionsPage
	err      error
}

func (p *stubAdmissionProvider) Request(
	ctx context.Context, request fleet.AdmissionRequest,
) (fleet.AdmissionGrant, error) {
	if p.resolver != nil {
		if _, err := p.resolver.Resolve(ctx); err != nil {
			return fleet.AdmissionGrant{}, err
		}
	}
	p.seen = request
	return p.grant, p.err
}

func (p *stubAdmissionProvider) Report(
	_ context.Context, report fleet.AdmissionReport,
) (fleet.ActionRecord, error) {
	p.report = report
	return p.record, p.err
}

func (p *stubAdmissionProvider) Outstanding(
	context.Context, fleet.OutstandingAdmissionsRequest,
) (fleet.OutstandingAdmissionsPage, error) {
	return p.page, p.err
}

func admissionTestHandler(
	t *testing.T, provider AdmissionProvider, now time.Time,
) http.Handler {
	t.Helper()
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "subject", Actor: "actor", AuthorizationDomain: []byte("domain"),
		AllowedOperations:  []auth.Operation{auth.OperationInvoke},
		PermittedSourceIDs: [][]byte{[]byte("source")},
		PermittedPolicyIDs: [][]byte{[]byte("policy")}, PolicyGeneration: 1,
		AuthenticationExpires: now.Add(time.Hour), RequestID: "request",
		CorrelationID: "correlation",
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewAuthenticatedHandler(&stubWorkspaceService{},
		AuthenticatorFunc(func(*http.Request) (auth.Decision, error) {
			return decision, nil
		}),
		authority.Binder(), "example.test")
	if err != nil {
		t.Fatal(err)
	}
	admission, err := NewAdmissionHandler(provider)
	if err != nil {
		t.Fatal(err)
	}
	if err := handler.MountAuthenticated(
		AdmissionRoutePrefix, admission); err != nil {
		t.Fatal(err)
	}
	return handler
}

func admissionPost(
	t *testing.T, handler http.Handler, path string, body any,
) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(
		http.MethodPost, "http://example.test"+path, bytes.NewReader(encoded))
	request.Host = "example.test"
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func admissionRequestBody(now time.Time) admissionapi.Request {
	return admissionapi.Request{
		Context: admissionapi.RequestContext{
			RequestID: encodeFleetID("request"), ReasonCode: "proxy_admission",
			CorrelationID: encodeFleetID("correlation"),
			Deadline:      now.Add(time.Minute),
		},
		ID:             base64.RawURLEncoding.EncodeToString([]byte("admission")),
		IdempotencyKey: base64.RawURLEncoding.EncodeToString([]byte("key")),
		TokenID:        base64.RawURLEncoding.EncodeToString([]byte("token")),
		AgentID:        encodeFleetID("agent"), AgentGeneration: 1,
		Capability: "model", Action: "complete",
		SourceID: []byte("source"), PolicyID: []byte("policy"),
		ObjectID: encodeFleetID("object"),
		Effects:  []string{string(fleet.EffectEgressesContent)},
		Input:    json.RawMessage(`{"prompt_digest":"abc"}`),
		Lease:    time.Minute,
	}
}

// TestAdmissionDenialIsASuccessfulResponseWithNoToken pins the transport
// contract the proxy depends on: a policy refusal must be distinguishable from
// an unreachable decision plane, and a refused caller must not be handed
// something it could report an effect against.
func TestAdmissionDenialIsASuccessfulResponseWithNoToken(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	provider := &stubAdmissionProvider{
		grant: fleet.AdmissionGrant{Outcome: fleet.AdmissionDenied},
	}
	handler := admissionTestHandler(t, provider, now)
	response := admissionPost(
		t, handler, "/api/v1/admission/request", admissionRequestBody(now))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	var decoded admissionapi.Grant
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Outcome != admissionapi.OutcomeDenied {
		t.Fatalf("outcome = %q", decoded.Outcome)
	}
	if decoded.Token != nil {
		t.Fatalf("denial carried a token: %#v", decoded.Token)
	}
	if len(decoded.Withhold) != 0 {
		t.Fatalf("denial carried obligations: %#v", decoded.Withhold)
	}
	for _, term := range []string{"policy", "source", "compartment", "domain"} {
		if strings.Contains(
			strings.ToLower(response.Body.String()), term,
		) {
			t.Fatalf("denial body names %q: %s", term, response.Body.String())
		}
	}
}

// TestAdmissionObligationsSurviveTheWire pins that an obligation reaches the
// caller intact. A dropped obligation is not a smaller answer, it is a caller
// told to send content Shoal withheld.
func TestAdmissionObligationsSurviveTheWire(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	provider := &stubAdmissionProvider{grant: fleet.AdmissionGrant{
		Outcome: fleet.AdmissionObligated,
		Token: fleet.AdmissionToken{
			ActionID: []byte{'a', 0, 255}, TokenID: []byte{'t', 0, 255},
			Version: 2, ExpiresAt: now.Add(time.Minute),
		},
		Obligations: fleet.Obligations{
			Withhold: []shoal.ID{"doc-a", "doc-b"},
		},
	}}
	handler := admissionTestHandler(t, provider, now)
	response := admissionPost(
		t, handler, "/api/v1/admission/request", admissionRequestBody(now))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	var decoded admissionapi.Grant
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	// Encoded the same way the request's disclosures are, so a caller can match
	// the obligation against what it declared by equality. A reference whose
	// identity bytes are not printable would otherwise be unmatchable.
	if !reflect.DeepEqual(decoded.Withhold, []string{
		encodeFleetID("doc-a"), encodeFleetID("doc-b"),
	}) {
		t.Fatalf("withhold = %#v", decoded.Withhold)
	}
	round, err := decodeIDs(decoded.Withhold)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(round, []shoal.ID{"doc-a", "doc-b"}) {
		t.Fatalf("withhold round trip = %#v", round)
	}
	if decoded.Token == nil {
		t.Fatal("obligated grant carried no token")
	}
	token, err := decodeAdmissionToken(*decoded.Token)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(token.ActionID, []byte{'a', 0, 255}) ||
		!bytes.Equal(token.TokenID, []byte{'t', 0, 255}) ||
		token.Version != 2 {
		t.Fatalf("token round trip = %#v", token)
	}
}

// TestAdmissionAllowedResponseAlwaysCarriesTheObligationField pins that an
// unobligated allow still emits an empty list. A caller reading a missing key
// as "no obligation" and a missing key meaning "the field was omitted" are the
// same bug waiting for the first response that omits a real one.
func TestAdmissionAllowedResponseAlwaysCarriesTheObligationField(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	provider := &stubAdmissionProvider{grant: fleet.AdmissionGrant{
		Outcome: fleet.AdmissionAllowed,
		Token: fleet.AdmissionToken{
			ActionID: []byte("admission"), TokenID: []byte("token"), Version: 2,
		},
	}}
	handler := admissionTestHandler(t, provider, now)
	response := admissionPost(
		t, handler, "/api/v1/admission/request", admissionRequestBody(now))
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["withhold"]) != "[]" {
		t.Fatalf("withhold field = %s", raw["withhold"])
	}
}

// TestAdmissionPassesAnUnknownEffectThrough pins that the transport does not
// sanitise a class this build does not recognise. Dropping it would turn a
// request for something unclassifiable into a request for nothing, which the
// service would then read as the most permissive declaration there is.
func TestAdmissionPassesAnUnknownEffectThrough(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	provider := &stubAdmissionProvider{
		grant: fleet.AdmissionGrant{Outcome: fleet.AdmissionDenied},
	}
	handler := admissionTestHandler(t, provider, now)
	body := admissionRequestBody(now)
	body.Effects = []string{"invented-class"}
	if response := admissionPost(
		t, handler, "/api/v1/admission/request", body,
	); response.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	if len(provider.seen.Effects) != 1 ||
		provider.seen.Effects[0] != fleet.Effect("invented-class") {
		t.Fatalf("effects reaching the service = %#v", provider.seen.Effects)
	}
}

// TestAdmissionSpentTokenIsAConflict pins that the three ways a token can be
// spent — reported, refused, never issued — reach a caller as one status.
func TestAdmissionSpentTokenIsAConflict(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	provider := &stubAdmissionProvider{err: fleet.ErrAdmissionSpent}
	handler := admissionTestHandler(t, provider, now)
	response := admissionPost(
		t, handler, "/api/v1/admission/report", admissionapi.Report{
			Context: admissionapi.RequestContext{
				RequestID: encodeFleetID("request"), ReasonCode: "proxy_report",
				CorrelationID: encodeFleetID("correlation"),
				Deadline:      now.Add(time.Minute),
			},
			Token: admissionapi.Token{
				ActionID: base64.RawURLEncoding.EncodeToString(
					[]byte("admission")),
				TokenID: base64.RawURLEncoding.EncodeToString([]byte("token")),
				Version: 2,
			},
			Outcome: json.RawMessage(`{"tokens":1}`),
		})
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
}

// TestAdmissionReportPreservesOpaqueTokenBytes pins that a token whose bytes
// are not valid UTF-8 survives the round trip. A token mangled in transit is a
// report that can never be matched to its admission.
func TestAdmissionReportPreservesOpaqueTokenBytes(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	actionID := []byte{'a', 0, 255}
	tokenID := []byte{'t', 0, 254}
	provider := &stubAdmissionProvider{record: fleet.ActionRecord{
		ID: actionID, Version: 3, State: fleet.DispatchSucceeded, UpdatedAt: now,
	}}
	handler := admissionTestHandler(t, provider, now)
	response := admissionPost(
		t, handler, "/api/v1/admission/report", admissionapi.Report{
			Context: admissionapi.RequestContext{
				RequestID: encodeFleetID("request"), ReasonCode: "proxy_report",
				CorrelationID: encodeFleetID("correlation"),
				Deadline:      now.Add(time.Minute),
			},
			Token: admissionapi.Token{
				ActionID: base64.RawURLEncoding.EncodeToString(actionID),
				TokenID:  base64.RawURLEncoding.EncodeToString(tokenID),
				Version:  2,
			},
			Outcome: json.RawMessage(`{"tokens":1}`),
		})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	if !bytes.Equal(provider.report.Token.ActionID, actionID) ||
		!bytes.Equal(provider.report.Token.TokenID, tokenID) ||
		provider.report.Token.Version != 2 {
		t.Fatalf("token reaching the service = %#v", provider.report.Token)
	}
}

// TestAdmissionHandlerRequiresAProvider pins construction, and that the mount
// point is the one startup uses.
func TestAdmissionHandlerRequiresAProvider(t *testing.T) {
	if _, err := NewAdmissionHandler(nil); !shoal.IsErrorCode(
		err, shoal.ErrorInvalidArgument) {
		t.Fatalf("missing provider = %v", err)
	}
	if AdmissionRoutePrefix != "/api/v1/admission/" {
		t.Fatalf("admission route prefix = %q", AdmissionRoutePrefix)
	}
}

// TestAdmissionMountRequiresAuthenticatedTransport pins that the surface cannot
// be reached anonymously. It is the one endpoint whose answer is permission to
// perform an effect.
func TestAdmissionMountRequiresAuthenticatedTransport(t *testing.T) {
	anonymous, err := NewHandler(&stubWorkspaceService{}, "example.test")
	if err != nil {
		t.Fatal(err)
	}
	admission, err := NewAdmissionHandler(&stubAdmissionProvider{})
	if err != nil {
		t.Fatal(err)
	}
	if err := anonymous.MountAuthenticated(
		AdmissionRoutePrefix, admission,
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("anonymous mount = %v", err)
	}
}
