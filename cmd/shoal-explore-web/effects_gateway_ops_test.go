// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package main

// The effects gateway's dispatch client as the gateway will run it (#391,
// PR4): an executor-bound credential minted by the real executor mapping from
// a projected service-account token, authenticated by the real OIDC
// authenticator, over real HTTP to the handler the binary mounts, served by
// the real dispatch and registry services over the durable embedded stores.
// Nothing on the server side is a double. The worker acts as itself, and
// never heartbeats.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/effectsgateway"
	dispatchapi "github.com/phrocker/shoal-oss/pkg/dispatch/api"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// sentRequest is what the recorder saw of one gateway request.
type sentRequest struct {
	path        string
	correlation string
}

// gatewayOps is an executor world with the gateway's client pointed at it and
// every request the client sends recorded by its transport on the way to the
// real handler.
type gatewayOps struct {
	*executorWorld
	mu   sync.Mutex
	sent []sentRequest
	n    int
}

func newGatewayOps(t *testing.T) *gatewayOps {
	t.Helper()
	return &gatewayOps{executorWorld: newExecutorWorld(t)}
}

// gateway installs the recording handler and returns a client holding the
// stripe executor's projected token, bound to its ref. Installed again before
// each phase, because the world's own helpers replace the handler.
func (g *gatewayOps) gateway(ref string) *effectsgateway.DispatchClient {
	g.t.Helper()
	return g.gatewayAs(testExecutorSubject, ref)
}

// gatewayAs is gateway for the executor with this service-account subject.
func (g *gatewayOps) gatewayAs(subject, ref string) *effectsgateway.DispatchClient {
	g.t.Helper()
	g.record()
	base, err := url.Parse(g.server.URL)
	if err != nil {
		g.t.Fatal(err)
	}
	httpClient := *g.server.Client()
	httpClient.Transport = recordingTransport{ops: g, next: g.server.Client().Transport}
	client, err := effectsgateway.NewDispatchClient(base, &httpClient,
		func() (string, error) { return g.executorToken(subject), nil }, g.h.now)
	if err != nil {
		g.t.Fatal(err)
	}
	bound, err := client.BindExecutor(ref)
	if err != nil {
		g.t.Fatal(err)
	}
	return bound
}

// record serves the gateway's requests with the real authenticated handler.
func (g *gatewayOps) record() {
	g.current.Store(g.handlerFor(g.realAuthenticator()))
}

// recordingTransport keeps the path and correlation header of every request
// the gateway's client sends, on its way to the real handler.
type recordingTransport struct {
	ops  *gatewayOps
	next http.RoundTripper
}

func (r recordingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	r.ops.mu.Lock()
	r.ops.sent = append(r.ops.sent, sentRequest{
		path: request.URL.Path, correlation: request.Header.Get(CorrelationIDHeader),
	})
	r.ops.mu.Unlock()
	return r.next.RoundTrip(request)
}

// listAgents is the registry's list route as the executor with this subject,
// returning the IDs of the descriptors on the page.
func (g *gatewayOps) listAgents(subject string) []string {
	g.t.Helper()
	got, _ := g.send(call{token: g.executorToken(subject)}, "/api/v1/fleet/agents/resolve",
		map[string]any{"context": g.contextWire(g.h.now().Add(time.Minute)), "limit": fleet.MaxListResults})
	if got.status != http.StatusOK {
		g.t.Fatalf("list as %s = %d %s", subject, got.status, got.raw)
	}
	var page struct {
		Agents []struct {
			ID string `json:"id"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(got.raw, &page); err != nil {
		g.t.Fatal(err)
	}
	ids := make([]string, 0, len(page.Agents))
	for _, agent := range page.Agents {
		id, err := base64.RawURLEncoding.DecodeString(agent.ID)
		if err != nil {
			g.t.Fatal(err)
		}
		ids = append(ids, string(id))
	}
	return ids
}

func (g *gatewayOps) requests() []sentRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]sentRequest(nil), g.sent...)
}

// enqueueUntil queues "status" for the stripe agent as alice, threading
// correlation, with the action's deadline at deadline.
func (g *gatewayOps) enqueueUntil(id, correlation string, deadline time.Time) {
	g.t.Helper()
	got := g.post(call{token: g.fleetToken("alice", nil), correlation: correlation},
		"/api/v1/fleet/actions", map[string]any{
			"context":          g.contextWire(deadline),
			"id":               b64([]byte(id)),
			"idempotency_key":  b64([]byte("key-" + id)),
			"agent_id":         b64([]byte("stripe-agent")),
			"agent_generation": 1,
			"capability":       "ops", "action": "status",
			"source_id": workspaceSourceID, "policy_id": workspaceGrantPolicyID,
			"object_id": b64([]byte("object-" + id)),
			"input":     map[string]any{},
		})
	if got.status != http.StatusCreated {
		g.t.Fatalf("enqueue %s = %d %s", id, got.status, got.raw)
	}
}

func (g *gatewayOps) context(reason string) effectsgateway.RequestContext {
	g.t.Helper()
	g.n++
	return effectsgateway.RequestContext{
		RequestID:  []byte("gateway-" + strconv.Itoa(g.n)),
		ReasonCode: reason, Deadline: g.h.now().Add(30 * time.Second),
	}
}

// pullAndClaim pulls id, checks it carries the enqueuer's correlation, and
// claims it for lease.
func (g *gatewayOps) pullAndClaim(
	client *effectsgateway.DispatchClient, id, correlation string, lease time.Duration,
) (offered, claimed effectsgateway.Action, claimID []byte) {
	g.t.Helper()
	page, err := client.Pull(context.Background(), g.context("gateway_pull"), "", 64)
	if err != nil {
		g.t.Fatalf("pull: %v", err)
	}
	for _, action := range page.Actions {
		if string(action.ID) == id {
			offered = action
		}
	}
	if offered.ID == nil {
		g.t.Fatalf("pull did not offer %s", id)
	}
	if string(offered.CorrelationID) != correlation {
		g.t.Fatalf("pulled correlation = %q, want the enqueuer's %q", offered.CorrelationID, correlation)
	}
	claimID, _, err = effectsgateway.NewClaimID("pod-0", nil)
	if err != nil {
		g.t.Fatal(err)
	}
	claimed, err = client.Claim(context.Background(), offered.ID, effectsgateway.ClaimRequest{
		Context: offered.Correlate(g.context("gateway_claim")), ExpectedVersion: offered.Version,
		ClaimID: claimID, Lease: lease,
	})
	if err != nil {
		g.t.Fatalf("claim %s: %v", id, err)
	}
	return offered, claimed, claimID
}

func (g *gatewayOps) complete(
	client *effectsgateway.DispatchClient, claimed effectsgateway.Action, claimID []byte,
	expectedVersion uint64,
) (effectsgateway.Action, error) {
	return client.Complete(context.Background(), claimed.ID, effectsgateway.Completion{
		Context:         claimed.Correlate(g.context("gateway_complete")),
		ExpectedVersion: expectedVersion, ClaimID: claimID, ClaimFence: claimed.ClaimFence,
		Output: []byte(`{"ok":true}`),
	})
}

// TestTheGatewayExtendsToTheClampedDeadlineAndCompletesOnTheFence: the
// lease end the client returns is the explorer's, clamped to the action's
// deadline, not now plus the lease requested; and a completion sent with the
// claim's version, which the extension has since moved, still binds on the
// fence.
func TestTheGatewayExtendsToTheClampedDeadlineAndCompletesOnTheFence(t *testing.T) {
	t.Parallel()
	g := newGatewayOps(t)
	ctx := context.Background()
	deadline := g.h.now().Add(3 * time.Minute)
	g.enqueueUntil("stripe-clamp", "alice-trace-clamp", deadline)
	client := g.gateway(testExecutorRef)

	// The gateway's own descriptor resolves with the minted credential
	// ([execute agent_resolve], the fleet confining resolve to the binding).
	// Another ref's descriptor is concealed as not found, and so is the
	// "gateway" agent on the local ref.
	descriptor, err := client.Resolve(ctx, []byte("stripe-agent"), g.context("gateway_startup"))
	if err != nil || descriptor.ExecutorRef != testExecutorRef {
		t.Fatalf("resolve own descriptor = %+v, %v", descriptor, err)
	}
	for _, foreign := range []string{"ledger-agent", "gateway"} {
		if _, err := client.Resolve(ctx, []byte(foreign), g.context("gateway_startup")); effectsgateway.DispatchKind(err) !=
			effectsgateway.DispatchNotFound {
			t.Fatalf("resolve %s outside the binding = %v", foreign, err)
		}
	}
	// A client bound to the other ref, holding this credential, refuses the
	// descriptor the explorer hands it rather than adopting it.
	other, err := client.BindExecutor(testOtherExecutorRef)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Resolve(ctx, []byte("stripe-agent"), g.context("gateway_startup")); effectsgateway.DispatchKind(err) !=
		effectsgateway.DispatchRefusedLocal {
		t.Fatalf("a descriptor bound to another ref = %v", err)
	}
	// List is confined the same way: each executor's page holds its own
	// descriptor and nothing outside its binding.
	if listed := g.listAgents(testExecutorSubject); strings.Join(listed, ",") != "stripe-agent" {
		t.Fatalf("the stripe executor listed %q, want only its own descriptor", listed)
	}
	if listed := g.listAgents(testOtherExecutorSubject); strings.Join(listed, ",") != "ledger-agent" {
		t.Fatalf("the ledger executor listed %q, want only its own descriptor", listed)
	}
	g.record()

	_, claimed, claimID := g.pullAndClaim(client, "stripe-clamp", "alice-trace-clamp", time.Minute)
	requested := fleet.MaxActionClaimTTL
	extended, err := client.Extend(ctx, claimed.ID, effectsgateway.ExtendRequest{
		Context: claimed.Correlate(g.context("gateway_extend")),
		ClaimID: claimID, ClaimFence: claimed.ClaimFence, Lease: requested,
	})
	if err != nil {
		t.Fatalf("extend: %v", err)
	}
	if !extended.ClaimLeaseUntil.Equal(extended.Deadline) || !extended.Deadline.Equal(deadline.UTC()) {
		t.Fatalf("lease end = %v, want the deadline %v (requested would be %v)",
			extended.ClaimLeaseUntil, deadline, g.h.now().Add(requested))
	}
	if extended.ClaimFence != claimed.ClaimFence || extended.Version != claimed.Version+1 {
		t.Fatalf("extended = %+v", extended)
	}
	// At the deadline already, a further extension cannot move the lease.
	_, err = client.Extend(ctx, claimed.ID, effectsgateway.ExtendRequest{
		Context: claimed.Correlate(g.context("gateway_extend")),
		ClaimID: claimID, ClaimFence: claimed.ClaimFence, Lease: time.Minute,
	})
	if effectsgateway.DispatchKind(err) != effectsgateway.DispatchInvalid {
		t.Fatalf("an extension past the deadline = %v", err)
	}

	// The claim's own version, stale since the extension: bound on the fence.
	completed, err := g.complete(client, claimed, claimID, claimed.Version)
	if err != nil {
		t.Fatalf("complete with the fence after the version moved: %v", err)
	}
	if completed.State != dispatchapi.DispatchSucceeded || completed.ClaimFence != claimed.ClaimFence {
		t.Fatalf("completed = %+v", completed)
	}
	// A lost reply: the identical resend is answered by the replay branch.
	if again, err := g.complete(client, claimed, claimID, claimed.Version); err != nil ||
		again.Version != completed.Version {
		t.Fatalf("complete replay = %+v, %v", again, err)
	}
}

// TestTheGatewayCannotExtendAfterARebindButStillCompletes: the descriptor is
// re-registered onto another executor ref while the claim is live. The
// extension is refused as fence_lost (the claim runs to its lease end), and
// the completion, judged against the ref the claim was taken under, lands.
func TestTheGatewayCannotExtendAfterARebindButStillCompletes(t *testing.T) {
	t.Parallel()
	g := newGatewayOps(t)
	ctx := context.Background()
	g.enqueueUntil("stripe-rebind", "alice-trace-rebind", g.h.now().Add(time.Hour))
	client := g.gateway(testExecutorRef)
	_, claimed, claimID := g.pullAndClaim(client, "stripe-rebind", "alice-trace-rebind", time.Minute)

	got := g.post(call{token: g.fleetToken("owner", nil)}, "/api/v1/fleet/agents", map[string]any{
		"context":             g.contextWire(g.h.now().Add(time.Minute)),
		"registration_key":    b64([]byte("rebind-stripe-agent")),
		"expected_generation": 1,
		"descriptor": map[string]any{
			"id":                   b64([]byte("stripe-agent")),
			"authorization_domain": workspaceAuthorizationDomain,
			"scopes": []fleet.Scope{{
				SourceID: workspaceSourceID, PolicyID: workspaceGrantPolicyID,
			}},
			"executor_ref":     testOtherExecutorRef,
			"capabilities":     approvalActions(true),
			"lease_expires_at": g.h.now().Add(20 * time.Hour).Format(time.RFC3339Nano),
		},
	})
	if got.status != http.StatusOK && got.status != http.StatusCreated {
		t.Fatalf("rebind = %d %s", got.status, got.raw)
	}
	client = g.gateway(testExecutorRef)

	_, err := client.Extend(ctx, claimed.ID, effectsgateway.ExtendRequest{
		Context: claimed.Correlate(g.context("gateway_extend")),
		ClaimID: claimID, ClaimFence: claimed.ClaimFence, Lease: time.Minute,
	})
	var dispatchErr *effectsgateway.DispatchError
	if effectsgateway.DispatchKind(err) != effectsgateway.DispatchFenceLost ||
		!errors.As(err, &dispatchErr) || dispatchErr.Status != http.StatusNotFound {
		t.Fatalf("extend after a rebind = %v, want fence_lost (404)", err)
	}
	// The descriptor now names another ref, so the old ref's worker cannot
	// resolve it, and the new ref's worker can.
	if _, err := client.Resolve(ctx, []byte("stripe-agent"), g.context("gateway_startup")); effectsgateway.DispatchKind(err) !=
		effectsgateway.DispatchNotFound {
		t.Fatalf("resolving a rebound descriptor for the old ref = %v", err)
	}
	ledger := g.gatewayAs(testOtherExecutorSubject, testOtherExecutorRef)
	if rebound, err := ledger.Resolve(ctx, []byte("stripe-agent"), g.context("gateway_startup")); err != nil ||
		rebound.ExecutorRef != testOtherExecutorRef {
		t.Fatalf("the new ref's worker resolving the rebound descriptor = %+v, %v", rebound, err)
	}
	completed, err := g.complete(client, claimed, claimID, claimed.Version)
	if err != nil || completed.State != dispatchapi.DispatchSucceeded {
		t.Fatalf("complete after a rebind = %+v, %v", completed, err)
	}
}

// TestTheGatewayReportsAmbiguityAndNeverMistakesARefusalForSuccess: a report
// is recorded; its identical replay is accepted without a second entry
// (#542); a report for a fence this principal never held, and one past the
// per-record budget (#514), are ErrAmbiguityUnrecorded; text the explorer
// would refuse is refused before it is sent; and a completion after the
// reports moved the version still binds on the fence.
func TestTheGatewayReportsAmbiguityAndNeverMistakesARefusalForSuccess(t *testing.T) {
	t.Parallel()
	g := newGatewayOps(t)
	ctx := context.Background()
	g.enqueueUntil("stripe-ambiguity", "alice-trace-ambiguity", g.h.now().Add(time.Hour))
	client := g.gateway(testExecutorRef)
	_, claimed, claimID := g.pullAndClaim(client, "stripe-ambiguity", "alice-trace-ambiguity", time.Minute)
	report := func(fence uint64, reference string) (effectsgateway.Action, error) {
		return client.ReportAmbiguity(ctx, claimed.ID, effectsgateway.AmbiguityReport{
			Context:    claimed.Correlate(g.context("gateway_ambiguity")),
			ClaimFence: fence, Outcome: dispatchapi.AmbiguityOutcomeUnknown,
			Target: "api.stripe.test", Reference: reference,
		})
	}

	recorded, err := report(claimed.ClaimFence, "ch_42")
	if err != nil || len(recorded.AmbiguityReports) != 1 ||
		recorded.AmbiguityReports[0].Reference != "ch_42" {
		t.Fatalf("report = %+v, %v", recorded.AmbiguityReports, err)
	}
	replayed, err := report(claimed.ClaimFence, "ch_42")
	if err != nil || len(replayed.AmbiguityReports) != 1 || replayed.Version != recorded.Version {
		t.Fatalf("identical replay = %+v, %v; want accepted with no second entry",
			replayed.AmbiguityReports, err)
	}

	// Never held at this fence: not found, unrecorded.
	_, err = report(claimed.ClaimFence+7, "ch_42")
	var dispatchErr *effectsgateway.DispatchError
	if !errors.Is(err, effectsgateway.ErrAmbiguityUnrecorded) ||
		!errors.As(err, &dispatchErr) || dispatchErr.Status != http.StatusNotFound {
		t.Fatalf("a report at a fence never held = %v, want unrecorded (404)", err)
	}

	// The explorer's text rule, applied first: refused locally, and the
	// explorer refuses the same text when it is sent anyway.
	before := len(g.requests())
	for _, text := range []string{"ch_‮24", "ch\x00", strings.Repeat("r", fleet.MaxAmbiguityReferenceBytes+1)} {
		if _, err := report(claimed.ClaimFence, text); effectsgateway.DispatchKind(err) !=
			effectsgateway.DispatchRefusedLocal {
			t.Fatalf("reference %q = %v, want refused locally", text, err)
		}
		got, _ := g.send(call{token: g.executorToken(testExecutorSubject), correlation: "alice-trace-ambiguity"},
			"/api/v1/fleet/actions/"+b64(claimed.ID)+"/ambiguity", map[string]any{
				"context":          g.contextWire(g.h.now().Add(time.Minute)),
				"expected_version": 0, "claim_fence": claimed.ClaimFence,
				"outcome": "outcome_unknown", "reference": text,
			})
		if got.status != http.StatusBadRequest {
			t.Fatalf("the explorer answered reference %q with %d %s", text, got.status, got.raw)
		}
	}
	if sent := len(g.requests()); sent != before {
		t.Fatalf("%d locally refused reports reached the explorer", sent-before)
	}
	client = g.gateway(testExecutorRef)

	// Spend the per-record budget with distinct reports; the next is refused.
	for index := 2; index <= fleet.MaxActionAmbiguityReports; index++ {
		if _, err := report(claimed.ClaimFence, "ch_"+strconv.Itoa(index)); err != nil {
			t.Fatalf("report %d: %v", index, err)
		}
	}
	_, err = report(claimed.ClaimFence, "ch_over_budget")
	if !errors.Is(err, effectsgateway.ErrAmbiguityUnrecorded) ||
		!errors.As(err, &dispatchErr) || dispatchErr.Status != http.StatusBadRequest {
		t.Fatalf("a report past the budget = %v, want unrecorded (400)", err)
	}

	// Nine version moves since the claim; the fence still binds.
	completed, err := g.complete(client, claimed, claimID, claimed.Version)
	if err != nil || completed.State != dispatchapi.DispatchSucceeded {
		t.Fatalf("complete after the reports = %+v, %v", completed, err)
	}
}

// TestTheGatewayThreadsTheRecordsCorrelation: claim, extend, complete and
// ambiguity send Shoal-Correlation-ID equal to the record's correlation_id;
// each pull sends a correlation the gateway minted for it; a malformed
// correlation is refused before any request leaves.
func TestTheGatewayThreadsTheRecordsCorrelation(t *testing.T) {
	t.Parallel()
	g := newGatewayOps(t)
	ctx := context.Background()
	const trace = "alice-trace-correlation"
	g.enqueueUntil("stripe-trace", trace, g.h.now().Add(time.Hour))
	client := g.gateway(testExecutorRef)
	if _, err := client.Pull(ctx, g.context("gateway_pull"), "", 8); err != nil {
		t.Fatal(err)
	}
	_, claimed, claimID := g.pullAndClaim(client, "stripe-trace", trace, time.Minute)
	extended, err := client.Extend(ctx, claimed.ID, effectsgateway.ExtendRequest{
		Context: claimed.Correlate(g.context("gateway_extend")),
		ClaimID: claimID, ClaimFence: claimed.ClaimFence, Lease: 2 * time.Minute,
	})
	if err != nil {
		t.Fatalf("extend: %v", err)
	}
	if _, err := client.ReportAmbiguity(ctx, claimed.ID, effectsgateway.AmbiguityReport{
		Context:    claimed.Correlate(g.context("gateway_ambiguity")),
		ClaimFence: claimed.ClaimFence, Outcome: dispatchapi.AmbiguityEffectObserved, Reference: "ch_1",
	}); err != nil {
		t.Fatalf("ambiguity: %v", err)
	}

	// Malformed: refused locally on every route, nothing sent.
	before := len(g.requests())
	for _, malformed := range []string{"trace with spaces", "trace\x01", "trace\xff"} {
		request := g.context("gateway_claim")
		request.CorrelationID = []byte(malformed)
		if _, err := client.Claim(ctx, claimed.ID, effectsgateway.ClaimRequest{
			Context: request, ExpectedVersion: claimed.Version, ClaimID: claimID, Lease: time.Minute,
		}); effectsgateway.DispatchKind(err) != effectsgateway.DispatchRefusedLocal {
			t.Fatalf("claim with correlation %q = %v", malformed, err)
		}
		if _, err := client.Complete(ctx, claimed.ID, effectsgateway.Completion{
			Context: request, ExpectedVersion: extended.Version, ClaimID: claimID,
			ClaimFence: claimed.ClaimFence, Output: []byte(`{"ok":true}`),
		}); effectsgateway.DispatchKind(err) != effectsgateway.DispatchRefusedLocal {
			t.Fatalf("complete with correlation %q = %v", malformed, err)
		}
		if _, err := client.Pull(ctx, request, "", 8); effectsgateway.DispatchKind(err) !=
			effectsgateway.DispatchRefusedLocal {
			t.Fatalf("pull with correlation %q = %v", malformed, err)
		}
	}
	if sent := len(g.requests()); sent != before {
		t.Fatalf("%d requests with a malformed correlation reached the explorer", sent-before)
	}
	if _, err := g.complete(client, claimed, claimID, extended.Version); err != nil {
		t.Fatalf("complete: %v", err)
	}

	polls := map[string]bool{}
	routes := map[string]bool{}
	for _, sent := range g.requests() {
		switch {
		case strings.HasSuffix(sent.path, "/actions/pull"):
			if !strings.HasPrefix(sent.correlation, "gw-poll-") || polls[sent.correlation] ||
				interaction.ValidateCorrelationID(shoal.ID(sent.correlation)) != nil {
				t.Fatalf("pull correlation %q is not a fresh minted one (%v)", sent.correlation, polls)
			}
			polls[sent.correlation] = true
		default:
			if sent.correlation != trace {
				t.Fatalf("%s carried correlation %q, want the record's %q", sent.path, sent.correlation, trace)
			}
			routes[sent.path[strings.LastIndex(sent.path, "/")+1:]] = true
		}
	}
	if len(polls) != 2 {
		t.Fatalf("%d polls recorded, want 2", len(polls))
	}
	for _, route := range []string{"claim", "extend", "ambiguity", "complete"} {
		if !routes[route] {
			t.Fatalf("no %s request was recorded (%v)", route, routes)
		}
	}
}
