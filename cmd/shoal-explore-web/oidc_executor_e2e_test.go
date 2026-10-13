// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package main

// End-to-end tests for the executor mint (#391, PR3), in the shape
// oidc_approver_e2e_test.go asks for: every request is a token signed by a
// fake issuer, sent over real HTTP to the authenticated handler the binary
// builds, authenticated by the real OIDC authenticator, bound by the real
// binder, and served by the real dispatch and approval services over the
// durable embedded stores. The executor tokens come from a second fake
// issuer, as a projected ServiceAccount token comes from the cluster's
// service-account issuer rather than the human IdP.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type executorWorld struct {
	*oidcApprovalWorld
	executorIssuer *fakeOIDCIssuer
}

// newExecutorWorld opens the service with three executor references —
// "local" for the approval world's gateway agent, and the two the executor
// mapping binds — and registers one agent per mapped reference, by an OIDC
// operator over the registry route.
func newExecutorWorld(t *testing.T) *executorWorld {
	t.Helper()
	executorIssuer := newFakeOIDCIssuer(t)
	executors, err := newConfiguredFleetExecutors(
		[]string{"local", testExecutorRef, testOtherExecutorRef})
	if err != nil {
		t.Fatal(err)
	}
	mapping := writeExecutorMapping(t, executorMappingDocument(executorIssuer.server.URL))
	w := &executorWorld{
		oidcApprovalWorld: newOIDCApprovalWorldOn(t, func(config *oidcConfig) {
			config.executorMappingFile = mapping
		}, nil, nil, approverMappingDocument, executors),
		executorIssuer: executorIssuer,
	}
	owner := w.fleetToken("owner", nil)
	w.registerOn(owner, "stripe-agent", testExecutorRef)
	w.registerOn(owner, "ledger-agent", testOtherExecutorRef)
	return w
}

func (w *executorWorld) registerOn(token string, id shoal.ID, ref string) {
	w.t.Helper()
	got := w.post(call{token: token}, "/api/v1/fleet/agents", map[string]any{
		"context":          w.contextWire(w.h.now().Add(time.Minute)),
		"registration_key": b64([]byte("registration-" + string(id))),
		"descriptor": map[string]any{
			"id":                   b64([]byte(id)),
			"authorization_domain": workspaceAuthorizationDomain,
			"scopes": []fleet.Scope{{
				SourceID: workspaceSourceID, PolicyID: workspaceGrantPolicyID,
			}},
			"executor_ref":     ref,
			"capabilities":     approvalActions(true),
			"lease_expires_at": w.h.now().Add(20 * time.Hour).Format(time.RFC3339Nano),
		},
	})
	if got.status != http.StatusCreated {
		w.t.Fatalf("register %s = %d %s", id, got.status, got.raw)
	}
}

// executorToken is a projected service-account token from the executor
// issuer.
func (w *executorWorld) executorToken(subject string) string {
	w.t.Helper()
	return w.executorIssuer.signRS256(w.t, testKID,
		executorClaims(w.executorIssuer, w.h.now(), subject))
}

// enqueue queues "status" (no approval) for agent, as alice.
func (w *executorWorld) enqueue(caller call, id string, agent shoal.ID) {
	w.t.Helper()
	got := w.post(caller, "/api/v1/fleet/actions", map[string]any{
		"context":          w.contextWire(w.h.now().Add(time.Hour)),
		"id":               b64([]byte(id)),
		"idempotency_key":  b64([]byte("key-" + id)),
		"agent_id":         b64([]byte(agent)),
		"agent_generation": 1,
		"capability":       "ops", "action": "status",
		"source_id": workspaceSourceID, "policy_id": workspaceGrantPolicyID,
		"object_id": b64([]byte("object-" + id)),
		"input":     json.RawMessage(`{}`),
	})
	if got.status != http.StatusCreated {
		w.t.Fatalf("enqueue %s = %d %s", id, got.status, got.raw)
	}
}

// actionWire is the dispatch wire this file reads.
type actionWire struct {
	ID            string `json:"id"`
	Version       uint64 `json:"version"`
	State         string `json:"state"`
	AgentID       string `json:"agent_id"`
	CorrelationID string `json:"correlation_id"`
	ClaimID       string `json:"claim_id"`
	ClaimFence    uint64 `json:"claim_fence"`
}

// send is post without its #524 fatal, so a test can observe that refusal,
// returning the decoded action as well.
func (w *executorWorld) send(caller call, path string, body any) (answer, actionWire) {
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
	var action actionWire
	_ = json.Unmarshal(raw, &result)
	_ = json.Unmarshal(raw, &action)
	return result, action
}

func (w *executorWorld) pull(caller call) []actionWire {
	w.t.Helper()
	got, _ := w.send(caller, "/api/v1/fleet/actions/pull", map[string]any{
		"context": w.contextWire(w.h.now().Add(time.Minute)), "limit": 64,
	})
	if got.status != http.StatusOK {
		w.t.Fatalf("pull = %d %s", got.status, got.raw)
	}
	var page struct {
		Actions []actionWire `json:"actions"`
	}
	if err := json.Unmarshal(got.raw, &page); err != nil {
		w.t.Fatal(err)
	}
	return page.Actions
}

func (w *executorWorld) claim(caller call, id string, version uint64, claimID string) (answer, actionWire) {
	w.t.Helper()
	return w.send(caller, "/api/v1/fleet/actions/"+b64([]byte(id))+"/claim", map[string]any{
		"context":          w.contextWire(w.h.now().Add(time.Minute)),
		"expected_version": version,
		"claim_id":         b64([]byte(claimID)),
		"lease":            time.Minute,
	})
}

func (w *executorWorld) extend(caller call, id string, version uint64, claimID string) (answer, actionWire) {
	w.t.Helper()
	return w.send(caller, "/api/v1/fleet/actions/"+b64([]byte(id))+"/extend", map[string]any{
		"context":          w.contextWire(w.h.now().Add(time.Minute)),
		"expected_version": version,
		"claim_id":         b64([]byte(claimID)),
		"lease":            2 * time.Minute,
	})
}

func (w *executorWorld) complete(caller call, id string, version uint64, claimID string, fence uint64) (answer, actionWire) {
	w.t.Helper()
	return w.send(caller, "/api/v1/fleet/actions/"+b64([]byte(id))+"/complete", map[string]any{
		"context":          w.contextWire(w.h.now().Add(time.Minute)),
		"expected_version": version,
		"claim_id":         b64([]byte(claimID)),
		"claim_fence":      fence,
		"output":           json.RawMessage(`{"ok":true}`),
	})
}

// work runs pull, claim, extend and complete for id as caller, asserting each.
func (w *executorWorld) work(caller call, id string) actionWire {
	w.t.Helper()
	var offered *actionWire
	for _, action := range w.pull(caller) {
		if action.ID == b64([]byte(id)) {
			copied := action
			offered = &copied
		}
	}
	if offered == nil {
		w.t.Fatalf("pull did not offer %s", id)
	}
	got, claimed := w.claim(caller, id, offered.Version, "claim-"+id)
	if got.status != http.StatusOK || claimed.ClaimFence == 0 {
		w.t.Fatalf("claim %s = %d %s", id, got.status, got.raw)
	}
	got, extended := w.extend(caller, id, claimed.Version, "claim-"+id)
	if got.status != http.StatusOK || extended.ClaimFence != claimed.ClaimFence {
		w.t.Fatalf("extend %s = %d %s", id, got.status, got.raw)
	}
	got, completed := w.complete(caller, id, extended.Version, "claim-"+id, claimed.ClaimFence)
	if got.status != http.StatusOK || completed.State != string(fleet.DispatchSucceeded) {
		w.t.Fatalf("complete %s = %d %s", id, got.status, got.raw)
	}
	return completed
}

func assertConcealed(t *testing.T, name string, got answer) {
	t.Helper()
	if got.status != http.StatusNotFound || got.Code != string(shoal.ErrorNotFound) {
		t.Fatalf("%s = %d %s, want not found", name, got.status, got.raw)
	}
}

func assertUnauthenticated(t *testing.T, name string, got answer) {
	t.Helper()
	// The HTTP surface answers every authentication failure alike; the
	// specific reasons are the unit tests'.
	if got.status != http.StatusUnauthorized ||
		got.Code != string(shoal.ErrorUnauthorized) {
		t.Fatalf("%s = %d %s, want the generic denial", name, got.status, got.raw)
	}
}

// TestAMappedServiceAccountWorksItsOwnReference: a mapped projected token
// pulls, claims, extends and completes its own reference's work through the
// real dispatch routes, and is refused another reference's — as not found,
// on every route, including work it can name exactly.
func TestAMappedServiceAccountWorksItsOwnReference(t *testing.T) {
	t.Parallel()
	w := newExecutorWorld(t)
	alice := call{token: w.fleetToken("alice", nil)}
	w.enqueue(alice, "stripe-1", "stripe-agent")
	w.enqueue(alice, "ledger-1", "ledger-agent")

	// A supplied correlation is threaded; without one the mint makes one
	// (#527), so the routes are reachable either way — send would show the
	// #524 refusal otherwise.
	stripe := call{token: w.executorToken(testExecutorSubject), correlation: "stripe-trace-391"}
	ledger := call{token: w.executorToken(testOtherExecutorSubject)}

	for _, action := range w.pull(stripe) {
		if action.ID != b64([]byte("stripe-1")) {
			t.Fatalf("the stripe worker was offered %s", action.ID)
		}
	}
	// Ledger's work, named exactly: refused as not found before and after
	// ledger claims it.
	queued := w.dispatchStatus(alice, "ledger-1")
	got, _ := w.claim(stripe, "ledger-1", queued.Version, "stolen")
	assertConcealed(t, "claiming another reference's work", got)

	w.work(stripe, "stripe-1")

	var offered actionWire
	for _, action := range w.pull(ledger) {
		if action.ID == b64([]byte("ledger-1")) {
			offered = action
		}
	}
	got, claimed := w.claim(ledger, "ledger-1", offered.Version, "claim-ledger")
	if got.status != http.StatusOK {
		t.Fatalf("ledger claim = %d %s", got.status, got.raw)
	}
	got, _ = w.extend(stripe, "ledger-1", claimed.Version, "claim-ledger")
	assertConcealed(t, "extending another reference's claim", got)
	got, _ = w.complete(stripe, "ledger-1", claimed.Version, "claim-ledger", claimed.ClaimFence)
	assertConcealed(t, "completing another reference's claim", got)
	got, completed := w.complete(ledger, "ledger-1", claimed.Version, "claim-ledger", claimed.ClaimFence)
	if got.status != http.StatusOK || completed.State != string(fleet.DispatchSucceeded) {
		t.Fatalf("ledger complete = %d %s", got.status, got.raw)
	}

	// Alice, the enqueuer, sees both performed.
	for _, id := range []string{"stripe-1", "ledger-1"} {
		if got := w.dispatchStatus(alice, id); got.State != string(fleet.DispatchSucceeded) {
			t.Fatalf("%s is %q to its enqueuer", id, got.State)
		}
	}
}

// TestTheExecutorRouteNeedsTheMintedCorrelation is #527 on this branch: the
// executor decision exactly as minted reaches the routes, and the same
// decision without its correlation ID does not. COUNTERFACTUAL for the
// second half: the real decision with one field changed.
func TestTheExecutorRouteNeedsTheMintedCorrelation(t *testing.T) {
	t.Parallel()
	w := newExecutorWorld(t)
	stripe := call{token: w.executorToken(testExecutorSubject)}
	if got, _ := w.send(stripe, "/api/v1/fleet/actions/pull", map[string]any{
		"context": w.contextWire(w.h.now().Add(time.Minute)), "limit": 8,
	}); got.status != http.StatusOK {
		t.Fatalf("pull with the minted correlation = %d %s", got.status, got.raw)
	}
	stripe.authn = w.counterfactual(func(config *auth.DecisionConfig) {
		config.CorrelationID = ""
	})
	got, _ := w.send(stripe, "/api/v1/fleet/actions/pull", map[string]any{
		"context": w.contextWire(w.h.now().Add(time.Minute)), "limit": 8,
	})
	if got.status == http.StatusOK ||
		!strings.Contains(got.Message, "correlation ID is required") {
		t.Fatalf("pull without a correlation = %d %s, want the #524 refusal",
			got.status, got.raw)
	}
}

// TestNonExecutorTokensAreRefusedOnTheExecutorRoutes: a human token on the
// executor audience, a token whose service assertion is missing or wrong, a
// token carrying both audiences, a token the wrong issuer's key signed, and
// a human's fleet token, all over HTTP.
func TestNonExecutorTokensAreRefusedOnTheExecutorRoutes(t *testing.T) {
	t.Parallel()
	w := newExecutorWorld(t)
	alice := call{token: w.fleetToken("alice", nil)}
	w.enqueue(alice, "stripe-1", "stripe-agent")
	now := w.h.now()
	pull := func(token string) answer {
		got, _ := w.send(call{token: token}, "/api/v1/fleet/actions/pull", map[string]any{
			"context": w.contextWire(now.Add(time.Minute)), "limit": 8,
		})
		return got
	}
	sign := func(issuer *fakeOIDCIssuer, claims jwt.MapClaims) string {
		return issuer.signRS256(t, testKID, claims)
	}
	valid := func() jwt.MapClaims {
		return executorClaims(w.executorIssuer, now, testExecutorSubject)
	}

	human := w.issuer.defaultClaims(now)
	human["aud"] = []string{testExecutorAudience}
	human["sub"] = testExecutorSubject
	assertUnauthenticated(t, "a human token on the executor audience", pull(sign(w.issuer, human)))

	missing := valid()
	delete(missing, "kubernetes.io")
	assertUnauthenticated(t, "no service assertion", pull(sign(w.executorIssuer, missing)))
	wrong := valid()
	wrong["kubernetes.io"] = map[string]any{"namespace": "default"}
	assertUnauthenticated(t, "the wrong service assertion", pull(sign(w.executorIssuer, wrong)))

	both := valid()
	both["aud"] = []string{testExecutorAudience, testAudience}
	assertUnauthenticated(t, "both audiences", pull(sign(w.executorIssuer, both)))
	humanBoth := w.issuer.defaultClaims(now)
	humanBoth["aud"] = []string{testAudience, testExecutorAudience}
	humanBoth["access"] = []string{"fleet"}
	assertUnauthenticated(t, "a human fleet token on both audiences", pull(sign(w.issuer, humanBoth)))

	assertUnauthenticated(t, "an executor token the human issuer signed",
		pull(sign(w.issuer, valid())))
	delegated := valid()
	delegated["act"] = map[string]any{"sub": "alice"}
	assertUnauthenticated(t, "a delegated executor token", pull(sign(w.executorIssuer, delegated)))

	// A human's fleet token authenticates and holds no execute, so it
	// cannot claim queued work it did not enqueue as an executor would.
	bob := call{token: w.fleetToken("bob", nil)}
	queued := w.dispatchStatus(alice, "stripe-1")
	got, _ := w.claim(bob, "stripe-1", queued.Version, "bob-claim")
	if got.status == http.StatusOK {
		t.Fatalf("a fleet token claimed another principal's work: %s", got.raw)
	}
	// Control: the mapped token works.
	if got := pull(w.executorToken(testExecutorSubject)); got.status != http.StatusOK {
		t.Fatalf("the mapped token = %d %s", got.status, got.raw)
	}
}

// TestApprovalOfExecutorTouchedWorkIsUnaffected: alice holds a request on
// the stripe agent, bob approves it, and the stripe executor performs the
// materialized action. The executor identity is outside the human family, so
// it never enters the approval service's namespace rule; a second approval
// on the same agent, after the executor has worked it, still succeeds.
func TestApprovalOfExecutorTouchedWorkIsUnaffected(t *testing.T) {
	t.Parallel()
	w := newExecutorWorld(t)
	h := w.h
	alice := call{token: w.fleetToken("alice", nil)}
	bob := call{token: w.approverToken("bob")}
	stripe := call{token: w.executorToken(testExecutorSubject)}

	first := h.held("stripe-held-1")
	w.mustDecide(bob, w.mustHold(alice, first, "stripe-agent"))
	w.work(stripe, w.materialize(alice, first, "stripe-agent"))

	second := h.held("stripe-held-2")
	w.mustDecide(bob, w.mustHold(alice, second, "stripe-agent"))
	w.work(stripe, w.materialize(alice, second, "stripe-agent"))

	// The executor is never an approver or a requester: its token on the
	// approval routes is refused (it holds neither approve nor dispatch).
	third := h.held("stripe-held-3")
	receipt := w.mustHold(alice, third, "stripe-agent")
	if got := w.decide(stripe, receipt); got.status == http.StatusOK {
		t.Fatalf("an executor decided an approval: %s", got.raw)
	}
	w.mustDecide(bob, receipt)
}

// materialize is the requester coming back for an approved request: it
// becomes work, whose action ID this returns.
func (w *executorWorld) materialize(caller call, request heldRequest, agent shoal.ID) string {
	w.t.Helper()
	got := w.request(caller, request, agent)
	if got.status != http.StatusCreated || got.materialized() == nil {
		w.t.Fatalf("materialize %s = %d %s", request.id, got.status, got.raw)
	}
	id, err := base64.RawURLEncoding.DecodeString(got.materialized().ID)
	if err != nil {
		w.t.Fatal(err)
	}
	return string(id)
}

// dispatchStatus is the dispatch status route as caller.
func (w *executorWorld) dispatchStatus(caller call, id string) actionWire {
	w.t.Helper()
	got, action := w.send(caller, "/api/v1/fleet/actions/"+b64([]byte(id))+"/status",
		w.contextWire(w.h.now().Add(time.Minute)))
	if got.status != http.StatusOK {
		w.t.Fatalf("status %s = %d %s", id, got.status, got.raw)
	}
	return action
}
