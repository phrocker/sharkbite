// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied. See the License for the
// specific language governing permissions and limitations
// under the License.

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/effectsgateway"
	dispatchapi "github.com/phrocker/shoal-oss/pkg/dispatch/api"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// The effects gateway's dispatch client, driven against the explorer exactly
// as openService composes it: the embedded store, the real lifecycle recorder
// and event publisher, behind the real authenticated webapi handler, over a
// real socket. Nothing on the server side is a double.
//
// The worker's principal is the enqueuer's. A gateway that claims work it did
// not enqueue needs an OperationExecute grant that nothing can mint until #480,
// so this is the enqueuer-is-claimant path the design names as the one
// testable before that lands. It exercises every wire shape the client
// depends on; what it cannot exercise is the cross-principal authorization.

const gatewayTestRoutes = `[
 {"action":"charge","method":"POST","path":"/v1/accounts/{account}/charges",
  "effects":["external","egresses-content"],"idempotency":"key",
  "conflict":{"status":[400],"pointer":"/error/type","equals":["idempotency_error"]},
  "reference":{"pointer":"/id","pattern":"ch_[0-9]+"}}
]`

type gatewayHarness struct {
	t         *testing.T
	opened    openedService
	server    *httptest.Server
	authority *auth.Authority
	requests  atomic.Uint64
	routes    *effectsgateway.RouteTable
	agentID   shoal.ID
}

func newGatewayHarness(t *testing.T, extra ...fleet.Action) *gatewayHarness {
	t.Helper()
	root := t.TempDir()
	h := &gatewayHarness{t: t, authority: auth.NewAuthority(), agentID: "effects-gateway"}

	executors, err := newConfiguredFleetExecutors([]string{"gateway"})
	if err != nil {
		t.Fatal(err)
	}
	// What -fleet-external-egress-executor-refs=gateway composes: a
	// reference that may mutate externally and transmit, and runs nothing in
	// process.
	if err := bindExternalFleetEffects(executors, externalFleetEffectBindings{
		transmitting: []string{"gateway"},
	}); err != nil {
		t.Fatal(err)
	}
	h.opened, err = openService(context.Background(), serviceConfig{
		backend: "embedded", data: filepath.Join(root, "corpus"),
		policyDir: filepath.Join(root, "policy"),
		resolver:  h.authority.Resolver(), clock: time.Now,
		executors: executors,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.opened.close() })

	h.routes, err = effectsgateway.ParseRoutes([]byte(gatewayTestRoutes),
		dispatchapi.Effects{dispatchapi.EffectEgressesContent, dispatchapi.EffectMutatesExternal})
	if err != nil {
		t.Fatal(err)
	}
	charge, _ := h.routes.Lookup("charge")

	registerCtx, err := h.authority.Binder().Bind(context.Background(),
		h.decision("operator", "register", auth.OperationAgentRegister))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.opened.fleetRegistry.Register(registerCtx, fleet.RegisterRequest{
		Context: fleet.RequestContext{
			RequestID: "register", ReasonCode: "test",
			Deadline: time.Now().UTC().Add(time.Minute),
		},
		RegistrationKey: "gateway-registration",
		Spec: fleet.Spec{
			ID: h.agentID, AuthorizationDomain: workspaceAuthorizationDomain,
			Scopes: []fleet.Scope{{
				SourceID: workspaceSourceID, PolicyID: workspaceGrantPolicyID,
			}},
			ExecutorRef: "gateway",
			Capabilities: []fleet.Capability{{
				Name: effectsgateway.DefaultCapability,
				Actions: append([]fleet.Action{{
					Name:         "charge",
					InputSchema:  charge.InputSchema(),
					OutputSchema: effectsgateway.OutputSchema(),
					Effects:      fleetEffectsOf(charge.Effects()),
				}}, extra...),
			}},
			LeaseExpiresAt: time.Now().UTC().Add(time.Hour),
		},
	}); err != nil {
		t.Fatalf("register the gateway descriptor: %v", err)
	}

	h.server = httptest.NewUnstartedServer(nil)
	authenticator := webapi.AuthenticatorFunc(func(request *http.Request) (auth.Decision, error) {
		token := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
		if token != "gateway" {
			return auth.Decision{}, shoal.NewError(shoal.ErrorUnauthorized, "unknown test token")
		}
		return h.decision("gateway",
			"gateway-request-"+strconv.FormatUint(h.requests.Add(1), 10),
			auth.OperationDispatch, auth.OperationInvoke, auth.OperationAgentResolve), nil
	})
	handler, err := webapi.NewAuthenticatedHandler(h.opened.service, authenticator,
		h.authority.Binder(), h.server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	fleetHandler, err := webapi.NewFleetHandler(h.opened.fleetRegistry, h.opened.fleetDispatch)
	if err != nil {
		t.Fatal(err)
	}
	if err := handler.MountAuthenticated(webapi.FleetRoutePrefix, fleetHandler); err != nil {
		t.Fatal(err)
	}
	h.server.Config.Handler = handler
	h.server.Start()
	t.Cleanup(h.server.Close)
	return h
}

func (h *gatewayHarness) decision(actor string, requestID string, operations ...auth.Operation) auth.Decision {
	h.t.Helper()
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "owner", Actor: shoal.ID(actor),
		AuthorizationDomain:   workspaceAuthorizationDomain,
		AllowedOperations:     operations,
		PermittedSourceIDs:    [][]byte{workspaceSourceID},
		PermittedPolicyIDs:    [][]byte{workspaceGrantPolicyID},
		PolicyGeneration:      workspacePolicyGeneration,
		AuthenticationExpires: time.Now().Add(time.Hour),
		RequestID:             shoal.ID(requestID),
		CorrelationID:         shoal.ID(requestID + "-correlation"),
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return decision
}

func (h *gatewayHarness) client() *effectsgateway.DispatchClient {
	h.t.Helper()
	base, err := url.Parse(h.server.URL)
	if err != nil {
		h.t.Fatal(err)
	}
	client, err := effectsgateway.NewDispatchClient(base,
		effectsgateway.NewExplorerClient(10*time.Second),
		func() (string, error) { return "gateway", nil }, time.Now)
	if err != nil {
		h.t.Fatal(err)
	}
	// The harness descriptor names the "gateway" ref; a client resolves only its own.
	bound, err := client.BindExecutor("gateway")
	if err != nil {
		h.t.Fatal(err)
	}
	return bound
}

// enqueue is the agent's side, over the same HTTP surface and principal.
func (h *gatewayHarness) enqueue(id, input string) {
	h.t.Helper()
	h.enqueueAction(id, "charge", input)
}

func (h *gatewayHarness) enqueueAction(id, action, input string) {
	h.t.Helper()
	body, err := json.Marshal(map[string]any{
		"context": map[string]any{
			"request_id": encodeTestID(shoal.ID("enqueue-" + id)), "reason_code": "agent_request",
			"deadline": time.Now().UTC().Add(time.Hour),
		},
		"id":              encodeTestID(shoal.ID(id)),
		"idempotency_key": encodeTestID(shoal.ID("intent-" + id)),
		"agent_id":        encodeTestID(h.agentID), "agent_generation": 1,
		"capability": effectsgateway.DefaultCapability, "action": action,
		"source_id": workspaceSourceID, "policy_id": workspaceGrantPolicyID,
		"object_id": encodeTestID(shoal.ID("object")),
		"input":     json.RawMessage(input),
	})
	if err != nil {
		h.t.Fatal(err)
	}
	response := h.rawPost("/api/v1/fleet/actions", body)
	if response.StatusCode != http.StatusCreated {
		h.t.Fatalf("enqueue %s: %d %s", id, response.StatusCode, readAll(response))
	}
	response.Body.Close()
}

func (h *gatewayHarness) rawPost(path string, body []byte) *http.Response {
	h.t.Helper()
	request, err := http.NewRequest(http.MethodPost, h.server.URL+path, bytes.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer gateway")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		h.t.Fatal(err)
	}
	return response
}

func readAll(response *http.Response) string {
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	return string(data)
}

func requestContext(t *testing.T, reason string) effectsgateway.RequestContext {
	t.Helper()
	id, err := effectsgateway.NewRequestID(nil)
	if err != nil {
		t.Fatal(err)
	}
	return effectsgateway.RequestContext{
		RequestID: id, ReasonCode: reason, Deadline: time.Now().UTC().Add(30 * time.Second),
	}
}

func pulled(t *testing.T, client *effectsgateway.DispatchClient, id string) (effectsgateway.Action, effectsgateway.PullPage) {
	t.Helper()
	page, err := client.Pull(context.Background(), requestContext(t, "gateway_pull"), "", 256)
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	for _, action := range page.Actions {
		if string(action.ID) == id {
			return action, page
		}
	}
	t.Fatalf("pull did not offer %q", id)
	return effectsgateway.Action{}, page
}

func TestEffectsGatewayClientAgainstTheRealDispatchHandler(t *testing.T) {
	h := newGatewayHarness(t)
	client := h.client()
	ctx := context.Background()

	// Startup: resolve once and verify the descriptor against the routes.
	descriptor, err := client.Resolve(ctx, []byte(h.agentID), requestContext(t, "gateway_startup"))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	actions, ok := descriptor.Actions(effectsgateway.DefaultCapability)
	if !ok {
		t.Fatal("the resolved descriptor lacks the gateway capability")
	}
	if err := effectsgateway.VerifyDescriptor(h.routes, actions); err != nil {
		t.Fatalf("the registered descriptor does not verify: %v", err)
	}

	// Target: a real listener reached through a name the egress policy
	// resolves, so the dialer, binder, classifier and client compose.
	var targetHits atomic.Int64
	var seenKey atomic.Value
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		seenKey.Store(r.Header.Get("Idempotency-Key"))
		if r.Header.Get("Authorization") != "Bearer target-secret" ||
			r.URL.EscapedPath() != "/v1/accounts/acct%2F7/charges" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"ch_42","object":"charge"}`)
	}))
	defer target.Close()
	_, port, _ := strings.Cut(strings.TrimPrefix(target.URL, "http://"), ":")
	targetBase, _ := url.Parse("http://target.test:" + port)
	policy, err := effectsgateway.NewEgressPolicy(targetBase, true,
		func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	targetClient, err := effectsgateway.NewTargetClient(policy, 0)
	if err != nil {
		t.Fatal(err)
	}
	binder, err := effectsgateway.NewBinder(targetBase, "Idempotency-Key")
	if err != nil {
		t.Fatal(err)
	}
	route, _ := h.routes.Lookup("charge")

	const input = `{"path":{"account":"acct/7"},"body":{"amount":100}}`
	h.enqueue("action-ok", input)

	// PULL: the page carries no input and no key, and a Date header the
	// PRECHECK arithmetic can use.
	offered, page := pulled(t, client, "action-ok")
	if len(offered.Input) != 0 || offered.ExecutorKey.HasKey() {
		t.Fatal("the pull page carried the input or the executor key")
	}
	if string(offered.AgentID) != string(h.agentID) || offered.Action != "charge" ||
		offered.State != dispatchapi.DispatchQueued {
		t.Fatalf("offered = %#v", offered)
	}
	serverNow, err := effectsgateway.ServerNow(page.Header, page.ReceivedAt, time.Now())
	if err != nil {
		t.Fatalf("the pull response has no usable Date header: %v", err)
	}
	if !effectsgateway.DeadlineAdmits(serverNow, offered.Deadline, time.Minute) ||
		!effectsgateway.RetentionCovers(offered.CreatedAt, offered.Deadline, 24*time.Hour) {
		t.Fatal("PRECHECK refused an action with an hour of deadline")
	}

	// CLAIM: the only response with the input and the key.
	claimID, nonce, err := effectsgateway.NewClaimID("pod-0", nil)
	if err != nil {
		t.Fatal(err)
	}
	if nonce == (effectsgateway.ClaimNonce{}) {
		t.Fatal("claim nonce is zero")
	}
	sent := time.Now()
	claimRequest := effectsgateway.ClaimRequest{
		Context: offered.Correlate(requestContext(t, "gateway_claim")), ExpectedVersion: offered.Version,
		ClaimID: claimID, Lease: time.Minute,
	}
	claimed, err := client.Claim(ctx, offered.ID, claimRequest)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if string(claimed.Input) != input && !jsonEqual(t, claimed.Input, input) {
		t.Fatalf("claimed input = %s", claimed.Input)
	}
	if !claimed.ExecutorKey.HasKey() || claimed.ClaimFence != 1 || !claimed.EffectPossible ||
		claimed.Version != offered.Version+1 {
		t.Fatalf("claimed = %#v", claimed)
	}
	// A transport retry of the same claim is answered by the replay branch.
	claimRequest.Context = offered.Correlate(requestContext(t, "gateway_claim"))
	replayed, err := client.Claim(ctx, offered.ID, claimRequest)
	if err != nil || replayed.Version != claimed.Version || !replayed.ExecutorKey.Equal(claimed.ExecutorKey) {
		t.Fatalf("claim replay = %#v, %v", replayed, err)
	}
	// A second claimant, with a fresh claim ID, is refused: not found or
	// conflict, and either means "re-pull", never "gone".
	rivalID, _, _ := effectsgateway.NewClaimID("pod-1", nil)
	_, err = client.Claim(ctx, offered.ID, effectsgateway.ClaimRequest{
		Context: offered.Correlate(requestContext(t, "gateway_claim")), ExpectedVersion: offered.Version,
		ClaimID: rivalID, Lease: time.Minute,
	})
	if kind := effectsgateway.DispatchKind(err); kind != effectsgateway.DispatchNotFound &&
		kind != effectsgateway.DispatchConflict {
		t.Fatalf("rival claim = %v", err)
	}

	anchored, err := effectsgateway.Anchor(sent, claimed.ClaimTimes())
	if err != nil {
		t.Fatal(err)
	}
	gate := effectsgateway.SendGate{OperationTimeout: 30 * time.Second}
	if refusal := gate.Check(anchored, time.Now(), false); refusal != effectsgateway.GateOpen {
		t.Fatalf("send gate = %q on a fresh one-minute claim", refusal)
	}

	// VALIDATE + SEND + CLASSIFY.
	bound, err := binder.Bind(route, claimed.Input, claimed.ExecutorKey)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	request, err := effectsgateway.NewTargetRequest(ctx, bound, "Authorization", "Bearer target-secret")
	if err != nil {
		t.Fatal(err)
	}
	response, err := targetClient.Do(request)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	body, oversize, readErr := effectsgateway.ReadBounded(response.Body, 64<<10)
	response.Body.Close()
	classified := effectsgateway.Classify(route, effectsgateway.Observation{
		Written: true, Response: &effectsgateway.Response{
			Status: response.StatusCode, Header: response.Header, Body: body,
			Oversize: oversize, BodyErr: readErr,
		},
	})
	if classified.Outcome != effectsgateway.OutcomeSucceeded {
		t.Fatalf("classified = %#v", classified)
	}
	if seenKey.Load() != claimed.ExecutorKey.HeaderValue() || targetHits.Load() != 1 {
		t.Fatalf("the target saw key %v in %d requests", seenKey.Load(), targetHits.Load())
	}

	// COMPLETE, under the claim, with the closed output.
	completion := effectsgateway.Completion{
		Context: claimed.Correlate(requestContext(t, "gateway_complete")), ExpectedVersion: claimed.Version, ClaimFence: claimed.ClaimFence,
		ClaimID: claimID, Output: classified.Output,
	}
	completed, err := client.Complete(ctx, claimed.ID, completion)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if completed.State != dispatchapi.DispatchSucceeded ||
		!jsonEqual(t, completed.Output, `{"status":200,"idempotency":"key","reference":"ch_42"}`) {
		t.Fatalf("completed = %#v (%s)", completed, completed.Output)
	}
	// A lost reply is resent with the same body and gets the committed record.
	completion.Context = claimed.Correlate(requestContext(t, "gateway_complete"))
	again, err := client.Complete(ctx, claimed.ID, completion)
	if err != nil || again.Version != completed.Version {
		t.Fatalf("complete replay = %#v, %v", again, err)
	}
	// A report under a claim that is not the one held is refused and visible.
	stale := completion
	stale.ClaimID = rivalID
	stale.Context = claimed.Correlate(requestContext(t, "gateway_complete"))
	if _, err := client.Complete(ctx, claimed.ID, stale); err == nil {
		t.Fatal("a completion under a claim that was never held was accepted")
	} else if kind := effectsgateway.DispatchKind(err); kind != effectsgateway.DispatchNotFound &&
		kind != effectsgateway.DispatchConflict {
		t.Fatalf("stale completion = %v", err)
	}
}

// TestEffectsGatewayClientRecordsAFailureInOneRequest drives a failed
// completion through the real handler.
//
// This test used to pin the opposite, under the name
// ...RecordsAFailureDespiteTheWire500: a durably recorded failure was
// answered HTTP 500, because the /complete handler discarded the committed
// record CompleteClaim returned alongside its error. Its own comment said to
// flip it to 2xx when #492 landed, which is what this is.
//
// The flip matters beyond a status code. The worker could not tell "committed
// as failed" from "your report was rejected, nothing happened", and the only
// way to find out was to resend the identical body and read the replay — an
// extra request on the path a gateway takes immediately after an irreversible
// external effect, which is where an extra request is least welcome.
func TestEffectsGatewayClientRecordsAFailureInOneRequest(t *testing.T) {
	h := newGatewayHarness(t)
	client := h.client()
	ctx := context.Background()
	const input = `{"path":{"account":"a"},"body":{"amount":1}}`

	// The raw wire, first: one request, 200, and the record.
	h.enqueue("action-raw", input)
	offered, _ := pulled(t, client, "action-raw")
	rawClaim, _, _ := effectsgateway.NewClaimID("pod-0", nil)
	claimed, err := client.Claim(ctx, offered.ID, effectsgateway.ClaimRequest{
		Context: offered.Correlate(requestContext(t, "gateway_claim")), ExpectedVersion: offered.Version,
		ClaimID: rawClaim, Lease: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	failure, _ := json.Marshal(map[string]any{
		"context": map[string]any{
			"request_id": encodeTestID("raw-complete"), "reason_code": "gateway_complete",
			"deadline": time.Now().UTC().Add(30 * time.Second),
		},
		"expected_version": claimed.Version,
		"claim_id":         base64.RawURLEncoding.EncodeToString(rawClaim),
		"error_code":       "target_rejected_422", "failed": true,
	})
	path := "/api/v1/fleet/actions/" + base64.RawURLEncoding.EncodeToString(claimed.ID) + "/complete"
	first := h.rawPost(path, failure)
	firstBody := readAll(first)
	first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("a recorded failure answered %d (%s): the write landed and "+
			"the worker was told it was refused",
			first.StatusCode, firstBody)
	}
	// The status alone would be worse than the error it replaced if the body
	// said nothing — the worker would learn neither outcome.
	if !strings.Contains(firstBody, `"state":"failed"`) ||
		!strings.Contains(firstBody, `"error_code":"target_rejected_422"`) {
		t.Fatalf("the first answer carries no outcome: %s", firstBody)
	}
	// The replay still agrees, which is what makes the single request safe to
	// rely on: a worker that loses the response can still ask again.
	second := h.rawPost(path, failure)
	secondBody := readAll(second)
	second.Body.Close()
	if second.StatusCode != http.StatusOK ||
		!strings.Contains(secondBody, `"state":"failed"`) {
		t.Fatalf("the replay did not return the recorded failure: %d %s",
			second.StatusCode, secondBody)
	}

	// The client hides that: one call, the recorded record.
	h.enqueue("action-failed", input)
	offered, _ = pulled(t, client, "action-failed")
	claimID, _, _ := effectsgateway.NewClaimID("pod-0", nil)
	claimed, err = client.Claim(ctx, offered.ID, effectsgateway.ClaimRequest{
		Context: offered.Correlate(requestContext(t, "gateway_claim")), ExpectedVersion: offered.Version,
		ClaimID: claimID, Lease: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	failed, err := client.Complete(ctx, claimed.ID, effectsgateway.Completion{
		Context: claimed.Correlate(requestContext(t, "gateway_complete")), ExpectedVersion: claimed.Version, ClaimFence: claimed.ClaimFence,
		ClaimID: claimID, Failed: true, ErrorCode: effectsgateway.TargetRejected(422),
	})
	if err != nil {
		t.Fatalf("a recorded failure was reported as an error: %v", err)
	}
	if failed.State != dispatchapi.DispatchFailed || failed.ErrorCode != "target_rejected_422" ||
		!failed.EffectPossible {
		t.Fatalf("failed = %#v", failed)
	}

	// Refused before anything is sent: an off-vocabulary code and an
	// over-ceiling lease.
	if _, err := client.Complete(ctx, claimed.ID, effectsgateway.Completion{
		Context: claimed.Correlate(requestContext(t, "gateway_complete")), ExpectedVersion: claimed.Version, ClaimFence: claimed.ClaimFence,
		ClaimID: claimID, Failed: true, ErrorCode: "the target said: no",
	}); effectsgateway.DispatchKind(err) != effectsgateway.DispatchRefusedLocal {
		t.Fatalf("off-vocabulary code = %v", err)
	}
	if _, err := client.Claim(ctx, offered.ID, effectsgateway.ClaimRequest{
		Context: offered.Correlate(requestContext(t, "gateway_claim")), ExpectedVersion: offered.Version,
		ClaimID: claimID, Lease: fleet.MaxActionClaimTTL + time.Nanosecond,
	}); effectsgateway.DispatchKind(err) != effectsgateway.DispatchRefusedLocal {
		t.Fatalf("over-ceiling lease = %v", err)
	}
	// A request context the explorer would refuse is refused here first.
	expired := requestContext(t, "gateway_pull")
	expired.Deadline = time.Now().Add(-time.Second)
	if _, err := client.Pull(ctx, expired, "", 1); effectsgateway.DispatchKind(err) != effectsgateway.DispatchRefusedLocal {
		t.Fatalf("expired context = %v", err)
	}
	missingReason := requestContext(t, "x")
	missingReason.ReasonCode = ""
	if _, err := client.Pull(ctx, missingReason, "", 1); effectsgateway.DispatchKind(err) != effectsgateway.DispatchRefusedLocal {
		t.Fatalf("missing reason = %v", err)
	}

	// And an unauthenticated client is told so, not shown an empty queue.
	base, _ := url.Parse(h.server.URL)
	stranger, _ := effectsgateway.NewDispatchClient(base, effectsgateway.NewExplorerClient(5*time.Second),
		func() (string, error) { return "stranger", nil }, time.Now)
	if _, err := stranger.Pull(ctx, requestContext(t, "gateway_pull"), "", 1); effectsgateway.DispatchKind(err) !=
		effectsgateway.DispatchUnauthorized {
		t.Fatalf("unknown token = %v", err)
	}
}

// TestEffectsGatewayClientSurfacesAnOutcomeRecordedOtherwise: an output the
// explorer refuses is recorded as failed. The client must not report that as
// a refusal that wrote nothing — the record is terminal — and must not report
// it as the success it sent.
//
// Before #492 the handler answered this 400 and the client recovered the
// record by resending. The handler now answers 2xx with the record, and
// recorded_otherwise is the permanent part — which is why this test reads
// identically on both sides of that change. The client still has a 400/500
// resend path; it is no longer reached against a server with #492, and is
// kept for one talking to a server without it.
func TestEffectsGatewayClientSurfacesAnOutcomeRecordedOtherwise(t *testing.T) {
	h := newGatewayHarness(t, fleet.Action{
		Name:        "strict",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		OutputSchema: json.RawMessage(
			`{"type":"object","properties":{"x":{"type":"string"}},"required":["x"],"additionalProperties":false}`),
		Effects: fleet.Effects{fleet.EffectEgressesContent, fleet.EffectMutatesExternal},
	})
	client := h.client()
	ctx := context.Background()
	h.enqueueAction("action-strict", "strict", `{}`)
	offered, _ := pulled(t, client, "action-strict")
	claimID, _, _ := effectsgateway.NewClaimID("pod-0", nil)
	claimed, err := client.Claim(ctx, offered.ID, effectsgateway.ClaimRequest{
		Context: offered.Correlate(requestContext(t, "gateway_claim")), ExpectedVersion: offered.Version,
		ClaimID: claimID, Lease: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	recorded, err := client.Complete(ctx, claimed.ID, effectsgateway.Completion{
		Context: claimed.Correlate(requestContext(t, "gateway_complete")), ExpectedVersion: claimed.Version, ClaimFence: claimed.ClaimFence,
		ClaimID: claimID, Output: json.RawMessage(`{"status":200,"idempotency":"key"}`),
	})
	if effectsgateway.DispatchKind(err) != effectsgateway.DispatchRecordedOtherwise {
		t.Fatalf("complete = %#v, %v; want the terminal record and recorded_otherwise", recorded, err)
	}
	if recorded.State != dispatchapi.DispatchFailed || recorded.ErrorCode != "invalid_executor_output" {
		t.Fatalf("recorded = %#v", recorded)
	}
}

func jsonEqual(t *testing.T, got json.RawMessage, want string) bool {
	t.Helper()
	var a, b any
	if json.Unmarshal(got, &a) != nil || json.Unmarshal([]byte(want), &b) != nil {
		return false
	}
	ga, _ := json.Marshal(a)
	gb, _ := json.Marshal(b)
	return bytes.Equal(ga, gb)
}

// fleetEffectsOf converts a route's declared effects into the fleet's type,
// for registering the descriptor the gateway will resolve.
func fleetEffectsOf(effects dispatchapi.Effects) fleet.Effects {
	converted := make(fleet.Effects, len(effects))
	for i, effect := range effects {
		converted[i] = fleet.Effect(effect)
	}
	return converted
}
