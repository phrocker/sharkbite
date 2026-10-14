// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package main

// End-to-end tests for the OIDC approver mapping (#451 slice 2).
//
// GUIDANCE FOR NEW END-TO-END TESTS: drive them through the real
// authenticator. Every request in this file is a signed token from the fake
// issuer, sent over real HTTP to the authenticated handler the binary builds,
// authenticated by the real OIDC authenticator and bound by the real binder
// into the real bound providers. Nothing here injects a bound context, calls
// h.as(), or re-mints the authenticator's decision to patch a field in.
//
// The reason is #524. Every fleet route required a correlation ID that
// neither shipped authenticator minted, so the whole dispatch, admission and
// approval surface was unreachable — and every end-to-end test passed,
// because each one injected a hand-built decision (or re-minted the real one
// with the missing field added) instead of authenticating. A test that skips
// the authenticator cannot see a gap in the authenticator. Use
// oidcApprovalWorld here, or the same shape elsewhere: a real server, a real
// token, a real authenticator.
//
// The only decisions not exactly as the authenticator minted them are the two
// labelled counterfactuals, and even those are the real authenticator's
// output with one field changed, still sent over HTTP and still bound by the
// real binder — the counterfactual authenticator wraps the real one rather
// than replacing the transport.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/gob"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type oidcApprovalWorld struct {
	t      *testing.T
	h      *approvalHarness
	issuer *fakeOIDCIssuer
	edit   func(*oidcConfig)
	authn  atomic.Pointer[oidcAuthenticator]
	digest atomic.Value // auth.Digest
	// migrateFrom is -oidc-identity-scheme-migrate for the next open (#526).
	migrateFrom coordination.Digest

	// server is a real listener. Its handler is rebuilt for every request
	// from the service currently open, so a restart (h.reopen) and a
	// reconfigured authenticator both take effect exactly as in the binary.
	server  *httptest.Server
	host    string
	current atomic.Value // http.Handler

	mu      sync.Mutex
	written []fleet.ApprovalRecord
	tokens  []string
}

// newOIDCApprovalWorld opens the service and registers the gateway agent as
// an OIDC operator. registrant is any extra claim that operator's token must
// carry under edit's configuration — with -oidc-delegation-claim set, every
// workspace token must name a delegation chain.
func newOIDCApprovalWorld(
	t *testing.T, edit func(*oidcConfig), registrant jwt.MapClaims,
) *oidcApprovalWorld {
	t.Helper()
	return newOIDCApprovalWorldWith(t, edit, registrant, nil, approverMappingDocument)
}

// newOIDCApprovalWorldWith is newOIDCApprovalWorld with the issuer's
// discovery subject types (nil keeps ["public"]) and the mapping document
// chosen by the caller.
func newOIDCApprovalWorldWith(
	t *testing.T, edit func(*oidcConfig), registrant jwt.MapClaims,
	subjectTypes []string, document func(issuer string) map[string]any,
) *oidcApprovalWorld {
	t.Helper()
	return newOIDCApprovalWorldOn(t, edit, registrant, subjectTypes, document, nil)
}

// newOIDCApprovalWorldOn is newOIDCApprovalWorldWith over the executor
// references given (nil: "local" alone).
func newOIDCApprovalWorldOn(
	t *testing.T, edit func(*oidcConfig), registrant jwt.MapClaims,
	subjectTypes []string, document func(issuer string) map[string]any,
	executors configuredFleetExecutors,
) *oidcApprovalWorld {
	t.Helper()
	w := &oidcApprovalWorld{t: t, issuer: newFakeOIDCIssuer(t), edit: edit}
	if subjectTypes != nil {
		w.issuer.subjectTypes = subjectTypes
	}
	h := &approvalHarness{t: t, root: t.TempDir(), executors: executors}
	h.clock.Store(time.Now().UTC().Add(time.Minute).Truncate(time.Second).UnixNano())
	authority, err := auth.NewAuthorityWithClock(h.now)
	if err != nil {
		t.Fatal(err)
	}
	h.authority = authority
	h.reader = &mutableFleetGeneration{}
	h.reader.value.Store(workspacePolicyGeneration)
	w.h = h
	w.configure(document(w.issuer.server.URL))
	h.mapping = func(context.Context) (auth.Digest, error) {
		return w.digest.Load().(auth.Digest), nil
	}
	h.wrap = func(store fleet.ApprovalStore) fleet.ApprovalStore {
		return hookedApprovalStore{ApprovalStore: store, before: func(
			mutation fleet.ApprovalMutation,
		) error {
			w.mu.Lock()
			w.written = append(w.written, fleet.CloneApprovalRecord(mutation.Record))
			w.mu.Unlock()
			return nil
		}}
	}
	h.open()
	t.Cleanup(h.close)

	w.server = httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			w.current.Load().(http.Handler).ServeHTTP(writer, request)
		}))
	t.Cleanup(w.server.Close)
	w.host = strings.TrimPrefix(w.server.URL, "http://")

	// The agent the held requests target, registered by an OIDC operator
	// over the registry route rather than by an injected registrant.
	w.register(w.fleetToken("owner", registrant), "gateway", "")
	return w
}

// configure is what an operator does to the mapping file: the
// authenticator is rebuilt from it and the in-force digest follows. Taking
// effect in the service needs a restart (h.reopen), as in the binary.
func (w *oidcApprovalWorld) configure(document map[string]any) {
	w.t.Helper()
	config := approverTestConfig(w.t, w.issuer, w.h.now, document)
	if w.edit != nil {
		w.edit(&config)
	}
	authenticator := newTestOIDCAuthenticator(w.t, config)
	w.authn.Store(authenticator)
	w.digest.Store(authenticator.approverMappingDigest())
	// The identity scheme the binary would stamp and enforce (#526), taking
	// effect at the next open, as the mapping does.
	w.h.scheme = &identitySchemeConfig{
		scheme: authenticator.identityScheme(), migrateFrom: w.migrateFrom,
	}
}

func (w *oidcApprovalWorld) sign(claims jwt.MapClaims) string {
	w.t.Helper()
	token := w.issuer.signRS256(w.t, testKID, claims)
	w.mu.Lock()
	w.tokens = append(w.tokens, token)
	w.mu.Unlock()
	return token
}

// fleetToken is a human's workspace token under the fleet mapping.
func (w *oidcApprovalWorld) fleetToken(subject string, extra jwt.MapClaims) string {
	claims := w.issuer.defaultClaims(w.h.now())
	claims["sub"] = subject
	claims["access"] = []string{"fleet"}
	for key, value := range extra {
		claims[key] = value
	}
	return w.sign(claims)
}

// approverToken is a human's token on the approver audience.
func (w *oidcApprovalWorld) approverToken(subject string) string {
	return w.sign(approverClaims(w.issuer, w.h.now(), subject))
}

func (w *oidcApprovalWorld) identity(subject string) shoal.ID {
	return shoal.ID("oidc:" + w.issuer.server.URL + "#" + subject)
}

// realAuthenticator is the OIDC authenticator currently configured, read at
// request time so a reconfiguration applies to the next request.
func (w *oidcApprovalWorld) realAuthenticator() webapi.Authenticator {
	return webapi.AuthenticatorFunc(func(request *http.Request) (auth.Decision, error) {
		return w.authn.Load().Authenticate(request)
	})
}

// counterfactual is the real authenticator with one field of its decision
// changed. It exists for the two checks that ask what would happen to a
// decision no shipped authenticator mints; everything else about the
// request — transport, authentication, binding, providers — is real.
func (w *oidcApprovalWorld) counterfactual(
	change func(*auth.DecisionConfig),
) webapi.Authenticator {
	return webapi.AuthenticatorFunc(func(request *http.Request) (auth.Decision, error) {
		decision, err := w.authn.Load().Authenticate(request)
		if err != nil {
			return auth.Decision{}, err
		}
		selected, _ := decision.SelectedOntology()
		config := auth.DecisionConfig{
			Subject:                decision.Subject(),
			Actor:                  decision.Actor(),
			ClientID:               decision.ClientID(),
			OnBehalfOf:             decision.OnBehalfOf(),
			AuthorizationDomain:    decision.AuthorizationDomain(),
			AllowedOperations:      decision.AllowedOperations(),
			PermittedSourceIDs:     decision.PermittedSourceIDs(),
			PermittedPolicyIDs:     decision.PermittedPolicyIDs(),
			PolicyGeneration:       decision.PolicyGeneration(),
			AuthenticationExpires:  decision.AuthenticationExpires(),
			RequestID:              decision.RequestID(),
			CorrelationID:          decision.CorrelationID(),
			AuditPurpose:           decision.AuditPurpose(),
			ServiceRole:            decision.ServiceRole(),
			ServiceCeilingIdentity: decision.ServiceCeilingIdentity(),
			SelectedOntology:       selected,
			GrantProvenance:        decision.GrantProvenance(),
			ExecutorBinding:        decision.ExecutorBinding(),
		}
		change(&config)
		return auth.NewDecision(config)
	})
}

// handlerFor is the authenticated handler main.go composes, over the service
// currently open: the fleet routes and the approval routes, each mounted
// through MountAuthenticated.
func (w *oidcApprovalWorld) handlerFor(authenticator webapi.Authenticator) http.Handler {
	w.t.Helper()
	h := w.h
	handler, err := webapi.NewAuthenticatedHandler(
		h.opened.service, authenticator, h.authority.Binder(), w.host)
	if err != nil {
		w.t.Fatal(err)
	}
	// As main.go composes it: the presentation route is mounted when the
	// service was opened with an attestation trust.
	var fleetHandler http.Handler
	if h.opened.attestation != nil {
		fleetHandler, err = webapi.NewFleetHandlerWithAttestation(
			h.opened.fleetRegistry, h.opened.fleetDispatch, h.opened.attestation)
	} else {
		fleetHandler, err = webapi.NewFleetHandler(
			h.opened.fleetRegistry, h.opened.fleetDispatch)
	}
	if err != nil {
		w.t.Fatal(err)
	}
	if err := handler.MountAuthenticated(
		webapi.FleetRoutePrefix, fleetHandler); err != nil {
		w.t.Fatal(err)
	}
	approvalHandler, err := webapi.NewFleetApprovalHandler(h.opened.approvals)
	if err != nil {
		w.t.Fatal(err)
	}
	if err := handler.MountAuthenticated(
		webapi.FleetApprovalRoutePrefix, approvalHandler); err != nil {
		w.t.Fatal(err)
	}
	return handler
}

// call is one caller: a bearer token and, when correlation is set, the
// Shoal-Correlation-ID header threading an upstream trace.
type call struct {
	token       string
	correlation string
	authn       webapi.Authenticator // nil: the real OIDC authenticator
}

// answer is every response shape these routes produce, decoded together.
type answer struct {
	status           int
	raw              []byte
	Code             string `json:"code"`
	Message          string `json:"message"`
	ID               string `json:"id"`
	State            string `json:"state"`
	StoredState      string `json:"stored_state"`
	Condition        string `json:"condition"`
	RequestDigest    string `json:"request_digest"`
	PolicyGeneration int64  `json:"policy_generation"`
	RequesterActor   string `json:"requester_actor"`
	Approver         string `json:"approver"`
	ApproverActor    string `json:"approver_actor"`
	// Action is the action name on an approval and the materialized action
	// object on a receipt; see materialized.
	Action json.RawMessage `json:"action"`
}

// materializedAction is the action a 201 receipt carries.
type materializedAction struct {
	ID            string `json:"id"`
	State         string `json:"state"`
	RequestID     string `json:"request_id"`
	CorrelationID string `json:"correlation_id"`
}

func (a answer) materialized() *materializedAction {
	if len(a.Action) == 0 || a.Action[0] != '{' {
		return nil
	}
	var action materializedAction
	if err := json.Unmarshal(a.Action, &action); err != nil {
		return nil
	}
	return &action
}

func (w *oidcApprovalWorld) post(caller call, path string, body any) answer {
	w.t.Helper()
	authenticator := caller.authn
	if authenticator == nil {
		authenticator = w.realAuthenticator()
	}
	w.current.Store(w.handlerFor(authenticator))
	encoded, err := json.Marshal(body)
	if err != nil {
		w.t.Fatal(err)
	}
	request, err := http.NewRequest(
		http.MethodPost, w.server.URL+path, bytes.NewReader(encoded))
	if err != nil {
		w.t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+caller.token)
	if caller.correlation != "" {
		request.Header.Set(CorrelationIDHeader, caller.correlation)
	}
	response, err := w.server.Client().Do(request)
	if err != nil {
		w.t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		w.t.Fatal(err)
	}
	result := answer{status: response.StatusCode, raw: raw}
	if err := json.Unmarshal(raw, &result); err != nil {
		w.t.Fatalf("%s answered non-JSON %d: %s", path, response.StatusCode, raw)
	}
	// The #524 gate sits above every route here. If it is hit, everything
	// the test asserts after it is about a request that never arrived.
	if strings.Contains(result.Message, "correlation ID is required") {
		w.t.Fatalf("%s was refused for a missing correlation ID (#524): %d %s",
			path, response.StatusCode, raw)
	}
	return result
}

func b64(value []byte) string { return base64.RawURLEncoding.EncodeToString(value) }

func (w *oidcApprovalWorld) contextWire(deadline time.Time) map[string]any {
	return map[string]any{
		"request_id":  b64([]byte("body-request")),
		"reason_code": "test",
		"deadline":    deadline.Format(time.RFC3339Nano),
	}
}

func (w *oidcApprovalWorld) register(token string, id, parent shoal.ID) {
	w.t.Helper()
	w.registerWith(call{token: token}, id, parent)
}

// registerWith is register through the caller's authenticator.
func (w *oidcApprovalWorld) registerWith(caller call, id, parent shoal.ID) {
	w.t.Helper()
	descriptor := map[string]any{
		"id":                   b64([]byte(id)),
		"authorization_domain": workspaceAuthorizationDomain,
		"scopes": []fleet.Scope{{
			SourceID: workspaceSourceID, PolicyID: workspaceGrantPolicyID,
		}},
		"executor_ref":     "local",
		"capabilities":     approvalActions(true),
		"lease_expires_at": w.h.now().Add(20 * time.Hour).Format(time.RFC3339Nano),
	}
	if parent != "" {
		descriptor["parent_id"] = b64([]byte(parent))
	}
	got := w.post(caller, "/api/v1/fleet/agents", map[string]any{
		"context":          w.contextWire(w.h.now().Add(time.Minute)),
		"registration_key": b64([]byte("registration-" + string(id))),
		"descriptor":       descriptor,
	})
	if got.status != http.StatusCreated {
		w.t.Fatalf("register %s = %d %s", id, got.status, got.raw)
	}
}

func (w *oidcApprovalWorld) request(
	caller call, request heldRequest, agent shoal.ID,
) answer {
	w.t.Helper()
	return w.post(caller, "/api/v1/fleet/approvals/request", map[string]any{
		"context":          w.contextWire(request.deadline),
		"id":               b64(request.id),
		"idempotency_key":  b64(append([]byte("key-"), request.id...)),
		"agent_id":         b64([]byte(agent)),
		"agent_generation": 1,
		"capability":       "ops", "action": request.action,
		"source_id": workspaceSourceID, "policy_id": request.policy,
		"object_id": b64([]byte(request.object)),
		"input":     json.RawMessage(request.input),
	})
}

func (w *oidcApprovalWorld) mustHold(
	caller call, request heldRequest, agent shoal.ID,
) answer {
	w.t.Helper()
	receipt := w.request(caller, request, agent)
	if receipt.status != http.StatusAccepted || receipt.State != "pending" {
		w.t.Fatalf("request %s = %d %s, want 202 pending",
			request.id, receipt.status, receipt.raw)
	}
	return receipt
}

func (w *oidcApprovalWorld) decide(caller call, receipt answer) answer {
	w.t.Helper()
	return w.post(caller, "/api/v1/fleet/approvals/"+receipt.ID+"/decide",
		map[string]any{
			"context":           w.contextWire(w.h.now().Add(time.Minute)),
			"request_digest":    receipt.RequestDigest,
			"policy_generation": receipt.PolicyGeneration,
			"verdict":           fleet.ApprovalVerdictApprove,
		})
}

func (w *oidcApprovalWorld) mustDecide(caller call, receipt answer) answer {
	w.t.Helper()
	decided := w.decide(caller, receipt)
	if decided.status != http.StatusOK || decided.State != "approved" {
		w.t.Fatalf("decide = %d %s, want 200 approved", decided.status, decided.raw)
	}
	return decided
}

func (w *oidcApprovalWorld) status(caller call, id []byte) answer {
	w.t.Helper()
	got := w.post(caller, "/api/v1/fleet/approvals/"+b64(id)+"/status",
		w.contextWire(w.h.now().Add(time.Minute)))
	if got.status != http.StatusOK {
		w.t.Fatalf("status %s = %d %s", id, got.status, got.raw)
	}
	return got
}

// assertNotWork: the held request is not an action its requester can see.
func (w *oidcApprovalWorld) assertNotWork(caller call, request heldRequest) {
	w.t.Helper()
	got := w.post(caller, "/api/v1/fleet/actions/"+b64(request.id)+"/status",
		w.contextWire(w.h.now().Add(time.Minute)))
	if got.status != http.StatusNotFound {
		w.t.Fatalf("dispatch status of held request = %d %s, want 404",
			got.status, got.raw)
	}
}

// records is every approval record the service wrote for one request, in
// order: the durable side of what the HTTP answers summarise.
func (w *oidcApprovalWorld) records(id []byte) []fleet.ApprovalRecord {
	w.mu.Lock()
	defer w.mu.Unlock()
	var result []fleet.ApprovalRecord
	for _, record := range w.written {
		if bytes.Equal(record.ID, id) {
			result = append(result, fleet.CloneApprovalRecord(record))
		}
	}
	return result
}

// decisionRecord is the approved transition written for one request.
func (w *oidcApprovalWorld) decisionRecord(id []byte) fleet.ApprovalRecord {
	w.t.Helper()
	for _, record := range w.records(id) {
		if record.State == fleet.ApprovalApproved {
			return record
		}
	}
	w.t.Fatalf("no approved record was written for %s", id)
	return fleet.ApprovalRecord{}
}

func assertNotIndependent(t *testing.T, name string, got answer) {
	t.Helper()
	if got.status != http.StatusUnauthorized ||
		got.Code != string(shoal.ErrorUnauthorized) ||
		!strings.Contains(got.Message, "not independent") {
		t.Fatalf("%s: decide = %d %s, want the independence refusal",
			name, got.status, got.raw)
	}
}

func assertMappingMoved(t *testing.T, name string, got answer) {
	t.Helper()
	if got.status != http.StatusConflict ||
		got.Code != string(shoal.ErrorConflict) ||
		// The wire carries the wrapping message, not the sentinel; this is
		// the one message approverMappingMoved writes.
		!strings.Contains(got.Message,
			"approver mapping a decision was made under is no longer in force") {
		t.Fatalf("%s = %d %s, want approver mapping moved", name, got.status, got.raw)
	}
}

// TestOIDCApproverApprovesAnOIDCRequest is the positive case end to end, and
// the shared-actor finding that motivated approver actor = subject. The
// requester and the approver each thread a supplied correlation, and the
// records and the materialized action carry exactly those.
func TestOIDCApproverApprovesAnOIDCRequest(t *testing.T) {
	t.Parallel()
	const aliceTrace, bobTrace = "upstream-alice-451", "upstream-bob-451"
	w := newOIDCApprovalWorld(t, nil, nil)
	h := w.h
	alice := call{token: w.fleetToken("alice", nil), correlation: aliceTrace}
	request := h.held("oidc-held-1")
	receipt := w.mustHold(alice, request, "gateway")
	w.assertNotWork(alice, request)

	bobToken := w.approverToken("bob")

	// The finding. Before approver actor = subject an OIDC approver could only
	// have been minted with the shared actor every OIDC token carries; the
	// approval service counts the requester's actor as involved, so that
	// approver overlapped every OIDC requester — and was refused under a
	// message blaming independence. COUNTERFACTUAL: bob's real decision with
	// that one field changed.
	if got := w.status(alice, request.id); got.RequesterActor != b64([]byte(oidcActor)) {
		t.Fatalf("an OIDC requester's actor is %q; the finding assumes %q",
			got.RequesterActor, oidcActor)
	}
	assertNotIndependent(t, "an approver with the shared OIDC actor",
		w.decide(call{token: bobToken, authn: w.counterfactual(
			func(config *auth.DecisionConfig) { config.Actor = oidcActor })}, receipt))

	// Self-approval: alice holds the approver mapping too, and her approver
	// identity is her requester identity.
	assertNotIndependent(t, "the requester as approver",
		w.decide(call{token: w.approverToken("alice")}, receipt))

	// Bob, independent, sees it and approves it, threading his own trace.
	bob := call{token: bobToken, correlation: bobTrace}
	if got := w.status(bob, request.id); got.State != "pending" {
		t.Fatalf("bob's view = %q", got.State)
	}
	decided := w.mustDecide(bob, receipt)
	if decided.Approver != b64([]byte(w.identity("bob"))) ||
		decided.ApproverActor != b64([]byte(w.identity("bob"))) {
		t.Fatalf("decided = %s", decided.raw)
	}
	digest := w.digest.Load().(auth.Digest)
	wantProvenance := auth.GrantProvenance{
		Issuer: w.issuer.server.URL, Subject: "bob",
		ClaimPath:     []string{"realm_access", "roles"},
		MatchedValue:  testApproverValue,
		MappingDigest: digest,
	}
	record := w.decisionRecord(request.id)
	if record.ApproverClientID != w.identity(testApproverClient) ||
		record.ApproverMappingDigest != digest ||
		!record.ApproverProvenance.Equal(wantProvenance) {
		t.Fatalf("decision record = client %q digest %x provenance %+v",
			record.ApproverClientID, record.ApproverMappingDigest,
			record.ApproverProvenance)
	}
	// Both supplied correlations, each on its own side of the record.
	if record.Request.CorrelationID != aliceTrace ||
		record.DecisionCorrelationID != bobTrace {
		t.Fatalf("record correlations = request %q decision %q, want %q and %q",
			record.Request.CorrelationID, record.DecisionCorrelationID,
			aliceTrace, bobTrace)
	}

	// The requester comes back for it — with no header this time, so the
	// materializing call's correlation is generated — and it becomes work
	// exactly once, carrying the original request's supplied correlation.
	again := w.request(call{token: alice.token}, request, "gateway")
	if again.status != http.StatusCreated || again.State != "enqueued" ||
		again.materialized() == nil ||
		again.materialized().CorrelationID != b64([]byte(aliceTrace)) {
		t.Fatalf("materialization = %d %s", again.status, again.raw)
	}
	if replay := w.request(alice, request, "gateway"); replay.status != http.StatusCreated ||
		replay.materialized() == nil ||
		replay.materialized().ID != again.materialized().ID {
		t.Fatalf("a repeated request = %d %s", replay.status, replay.raw)
	}
	var materialized *fleet.ApprovalRecord
	for _, written := range w.records(request.id) {
		if written.State == fleet.ApprovalEnqueued {
			written := written
			materialized = &written
		}
	}
	if materialized == nil || materialized.ApproverSubject != w.identity("bob") {
		t.Fatalf("no enqueued record approved by bob: %+v", materialized)
	}

	// The requester sees who approved their own request — accountability
	// for that request, and intended — and nothing about the mapping.
	view := w.status(alice, request.id)
	if view.Approver != b64([]byte(w.identity("bob"))) {
		t.Fatalf("the requester's status does not name the approver: %s", view.raw)
	}
	for _, hidden := range []string{
		testApproverValue, "realm_access", testApproverAudience,
		hex.EncodeToString(digest[:]), digest.String(),
		b64([]byte(testApproverValue)),
		base64.StdEncoding.EncodeToString(digest[:]),
	} {
		if bytes.Contains(view.raw, []byte(hidden)) {
			t.Fatalf("approval status discloses mapping detail %q: %s", hidden, view.raw)
		}
	}

	// The audit: every approval transition the service wrote holds the
	// provenance it was decided under, and no byte of any token.
	w.mu.Lock()
	written := append([]fleet.ApprovalRecord(nil), w.written...)
	tokens := append([]string(nil), w.tokens...)
	w.mu.Unlock()
	sawDecision := false
	for _, record := range written {
		if record.Verdict == fleet.ApprovalVerdictApprove {
			sawDecision = true
			if !record.ApproverProvenance.Equal(wantProvenance) {
				t.Fatalf("an approved record lost its provenance: %+v", record)
			}
			if record.DecisionCorrelationID != bobTrace {
				t.Fatalf("an approved record lost the approver's correlation: %q",
					record.DecisionCorrelationID)
			}
		}
		var encoded bytes.Buffer
		if err := gob.NewEncoder(&encoded).Encode(record); err != nil {
			t.Fatal(err)
		}
		assertNoTokenBytes(t, "an approval record",
			append(encoded.Bytes(), []byte(fmt.Sprintf("%#v", record))...), tokens)
	}
	if !sawDecision {
		t.Fatal("no approved record was written")
	}

	// The interaction audit of the decision is attributed to bob, names the
	// decision's request ID, and carries bob's correlation (#532) — the same
	// one the durable record holds.
	h.close()
	corpus, err := explorer.Open(filepath.Join(h.root, "corpus"))
	if err != nil {
		t.Fatal(err)
	}
	summaries, err := corpus.Interactions(context.Background())
	if err != nil {
		corpus.Close()
		t.Fatal(err)
	}
	audited := false
	for _, summary := range summaries {
		if summary.AuthorizationOperation != string(auth.OperationActionApprove) {
			continue
		}
		session, err := corpus.Interaction(context.Background(), summary.SessionID)
		if err != nil {
			corpus.Close()
			t.Fatal(err)
		}
		if session.Actor.SubjectID == w.identity("bob") &&
			session.RequestID == record.DecisionRequestID &&
			session.CorrelationID == bobTrace &&
			len(session.Turns) == 1 && session.Turns[0].ToolCall != nil &&
			session.Turns[0].ToolCall.Kind ==
				"fleet.action_approve.approval_decision" {
			audited = true
		}
	}
	corpus.Close()
	if !audited {
		t.Fatalf("no action_approve decision audit attributed to bob under "+
			"request %q among %d interactions", record.DecisionRequestID,
			len(summaries))
	}

	// And nothing on disk — corpus, interaction audit, policy, approvals —
	// carries a token segment.
	if err := filepath.WalkDir(h.root, func(
		path string, entry fs.DirEntry, err error,
	) error {
		if err != nil || entry.IsDir() {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		assertNoTokenBytes(t, path, content, tokens)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestOIDCMalformedCorrelationIsRefusedAtTheEdge: a caller that meant to
// thread a trace and sent a value the authenticator refuses learns so at
// authentication, before any approval route runs.
//
// The 401 body is the generic "authentication required", so on its own it
// would also pass for a broken token. The control sends the SAME token with a
// valid header and requires success, which makes the refusal attributable to
// the header alone.
func TestOIDCMalformedCorrelationIsRefusedAtTheEdge(t *testing.T) {
	t.Parallel()
	w := newOIDCApprovalWorld(t, nil, nil)
	request := w.h.held("oidc-malformed-trace")
	token := w.fleetToken("alice", nil)
	got := w.request(call{token: token, correlation: "two words"}, request, "gateway")
	if got.status != http.StatusUnauthorized {
		t.Fatalf("a malformed correlation = %d %s, want 401", got.status, got.raw)
	}
	if records := w.records(request.id); len(records) != 0 {
		t.Fatalf("a refused request wrote %d approval records", len(records))
	}

	// Control: the same token, the same request, a valid header.
	w.mustHold(call{token: token, correlation: "upstream-control-451"},
		request, "gateway")
	records := w.records(request.id)
	if len(records) == 0 ||
		records[0].Request.CorrelationID != "upstream-control-451" {
		t.Fatalf("the control request's records = %+v", records)
	}
}

// TestOIDCApproverWithoutTheHumanAssertionCannotDecide: a token on the
// approver audience that is otherwise valid — signed, mapped issuer,
// audience, client and approver value — but does not carry the human
// assertion, or carries it with the wrong value, is not an approver. It is
// refused at authentication, so /decide answers 401 and nothing is decided
// or written. The control is the same claims with the assertion intact,
// which decides; without it the 401 could be any broken token.
//
// Before this test, removing mapping.human.holds from mintApprover survived
// every end-to-end test.
func TestOIDCApproverWithoutTheHumanAssertionCannotDecide(t *testing.T) {
	t.Parallel()
	w := newOIDCApprovalWorld(t, nil, nil)
	h := w.h
	alice := call{token: w.fleetToken("alice", nil)}
	request := h.held("oidc-not-human")
	receipt := w.mustHold(alice, request, "gateway")
	held := len(w.records(request.id))

	for _, probe := range []struct {
		name  string
		claim func(jwt.MapClaims)
	}{
		{"absent", func(claims jwt.MapClaims) { delete(claims, testHumanClaim) }},
		{"wrong value", func(claims jwt.MapClaims) { claims[testHumanClaim] = "service" }},
	} {
		t.Run(probe.name, func(t *testing.T) {
			claims := approverClaims(w.issuer, h.now(), "bob")
			probe.claim(claims)
			got := w.decide(call{token: w.sign(claims)}, receipt)
			if got.status != http.StatusUnauthorized ||
				got.Code != string(shoal.ErrorUnauthorized) {
				t.Fatalf("a non-human approver token = %d %s, want 401",
					got.status, got.raw)
			}
			if records := w.records(request.id); len(records) != held {
				t.Fatalf("a refused decision wrote %d approval records",
					len(records)-held)
			}
			for _, record := range w.records(request.id) {
				if record.Verdict != "" || record.ApproverSubject != "" {
					t.Fatalf("a refused decision was recorded: %+v", record)
				}
			}
			if status := w.status(alice, request.id); status.State != "pending" ||
				status.Approver != "" {
				t.Fatalf("after a refused decision = %s", status.raw)
			}
		})
	}

	// Control: the same claims with the assertion intact decide.
	decided := w.mustDecide(
		call{token: w.sign(approverClaims(w.issuer, h.now(), "bob"))}, receipt)
	if decided.Approver != b64([]byte(w.identity("bob"))) {
		t.Fatalf("the control decision = %s", decided.raw)
	}
}

// assertNoTokenBytes refuses any JWT segment of any token. The header
// segment is shared by every token from one key, so finding none of it is
// the strongest form of the check.
func assertNoTokenBytes(t *testing.T, where string, content []byte, tokens []string) {
	t.Helper()
	for _, token := range tokens {
		for index, segment := range strings.Split(token, ".") {
			if len(segment) < 16 {
				continue
			}
			if bytes.Contains(content, []byte(segment)) {
				t.Fatalf("%s holds JWT segment %d of a token", where, index)
			}
		}
	}
}

// TestOIDCApproverDelegationChainIsRefused: an approver in the request's own
// delegation chain, or in the agent's registration chain, is refused — the
// same #489 rule, now reachable by OIDC principals. Every correlation here is
// generated by the authenticator.
func TestOIDCApproverDelegationChainIsRefused(t *testing.T) {
	t.Parallel()
	assertGenerated := func(t *testing.T, w *oidcApprovalWorld, id []byte) {
		t.Helper()
		record := w.decisionRecord(id)
		if !strings.HasPrefix(string(record.Request.CorrelationID), "oidc-correlation-") ||
			!strings.HasPrefix(string(record.DecisionCorrelationID), "oidc-correlation-") ||
			record.Request.CorrelationID == record.DecisionCorrelationID {
			t.Fatalf("generated correlations = request %q decision %q",
				record.Request.CorrelationID, record.DecisionCorrelationID)
		}
	}
	t.Run("agent registration chain", func(t *testing.T) {
		w := newOIDCApprovalWorld(t, nil, nil)
		h := w.h
		carol := w.fleetToken("carol", nil)
		w.register(carol, "oidc-parent", "")
		w.register(carol, "oidc-child", "oidc-parent")
		request := h.held("oidc-chain-1")
		receipt := w.mustHold(
			call{token: w.fleetToken("alice", nil)}, request, "oidc-child")
		assertNotIndependent(t, "the agents' registrant",
			w.decide(call{token: w.approverToken("carol")}, receipt))
		w.mustDecide(call{token: w.approverToken("bob")}, receipt)
		assertGenerated(t, w, request.id)
	})
	t.Run("request delegation chain", func(t *testing.T) {
		w := newOIDCApprovalWorld(t, func(config *oidcConfig) {
			config.delegationClaim = "on_behalf_of"
		}, jwt.MapClaims{"on_behalf_of": []string{"operator-team"}})
		h := w.h
		alice := call{token: w.fleetToken("alice", jwt.MapClaims{
			"on_behalf_of": []string{"dan"},
		})}
		request := h.held("oidc-obo-1")
		receipt := w.mustHold(alice, request, "gateway")
		records := w.records(request.id)
		if len(records) == 0 {
			t.Fatal("the held request wrote no record")
		}
		if got := records[0].Request.OnBehalfOf; len(got) != 1 ||
			got[0] != w.identity("dan") {
			t.Fatalf("request on-behalf-of = %v", got)
		}
		assertNotIndependent(t, "the principal the request is made for",
			w.decide(call{token: w.approverToken("dan")}, receipt))
		w.mustDecide(call{token: w.approverToken("bob")}, receipt)
		assertGenerated(t, w, request.id)
	})
}

// TestOIDCApproverMappingMovedAcrossRestart: an approval given under one
// mapping does not survive a restart under another. The approved request
// becomes unresolvable/approver_mapping_moved and never work; a pending
// request is unaffected until decided, and is then decided under the new
// mapping only.
func TestOIDCApproverMappingMovedAcrossRestart(t *testing.T) {
	t.Parallel()
	w := newOIDCApprovalWorld(t, nil, nil)
	h := w.h
	// The requester threads one supplied trace across the restart.
	const trace = "upstream-restart-451"
	alice := call{token: w.fleetToken("alice", nil), correlation: trace}
	approved, pending := h.held("moved-approved"), h.held("moved-pending")
	approvedReceipt := w.mustHold(alice, approved, "gateway")
	pendingReceipt := w.mustHold(alice, pending, "gateway")
	// Bob's token under the first mapping. It is still a validly signed,
	// unexpired token after the restart; what moved is the mapping.
	oldBob := call{token: w.approverToken("bob")}
	w.mustDecide(oldBob, approvedReceipt)
	before := w.digest.Load().(auth.Digest)
	oldAuthenticator := w.authn.Load()

	// The operator changes who is an approver, and the service restarts.
	document := approverMappingDocument(w.issuer.server.URL)
	document["values"] = []string{testApproverValue, "release-approvers"}
	w.configure(document)
	h.reopen()
	if after := w.digest.Load().(auth.Digest); after == before {
		t.Fatal("the mapping change did not move the digest")
	}

	assertMappingMoved(t, "materializing under a moved mapping",
		w.request(alice, approved, "gateway"))
	status := w.status(alice, approved.id)
	if status.State != string(fleet.ApprovalUnresolvable) ||
		status.Condition != string(fleet.ApprovalConditionApproverMappingMoved) ||
		status.StoredState != string(fleet.ApprovalApproved) {
		t.Fatalf("status = %s", status.raw)
	}
	w.assertNotWork(alice, approved)

	// A decision minted under the old mapping cannot decide now. That is
	// what a replica still running the previous configuration presents: the
	// previous real authenticator, unmodified, authenticating bob's token.
	assertMappingMoved(t, "a decision under the old mapping",
		w.decide(call{token: oldBob.token, authn: webapi.AuthenticatorFunc(
			oldAuthenticator.Authenticate)}, pendingReceipt))
	// Nor can an approver no mapping granted, now that one is in force.
	// COUNTERFACTUAL: no shipped authenticator mints approve without the
	// mapping, so this is bob's real decision with its provenance removed.
	assertMappingMoved(t, "an unmapped approver under a mapping",
		w.decide(call{token: w.approverToken("bob"), authn: w.counterfactual(
			func(config *auth.DecisionConfig) {
				config.GrantProvenance = auth.GrantProvenance{}
			})}, pendingReceipt))
	// The pending request was unaffected, and a current approver decides it.
	if got := w.status(alice, pending.id); got.State != "pending" || got.Condition != "" {
		t.Fatalf("pending request after the change = %s", got.raw)
	}
	w.mustDecide(call{token: w.approverToken("bob")}, pendingReceipt)
	receipt := w.request(alice, pending, "gateway")
	if receipt.status != http.StatusCreated || receipt.State != "enqueued" ||
		receipt.materialized() == nil ||
		receipt.materialized().CorrelationID != b64([]byte(trace)) {
		t.Fatalf("materializing under the current mapping = %d %s",
			receipt.status, receipt.raw)
	}
	// Under the current mapping, with the current digest.
	if record := w.decisionRecord(pending.id); record.ApproverMappingDigest !=
		w.digest.Load().(auth.Digest) || record.Request.CorrelationID != trace {
		t.Fatalf("decision record digest %x correlation %q",
			record.ApproverMappingDigest, record.Request.CorrelationID)
	}
}
