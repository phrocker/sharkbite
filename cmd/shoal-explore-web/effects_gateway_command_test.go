// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package main

// The effects gateway's command (#391, PR6) against the real explorer: the
// command's own Main — flag parsing, the lock, the executor-bound client, the
// startup resolve and descriptor check, the worker, the signal drain — run
// in-process with a minted executor credential in a token file, against the
// executor world's real OIDC authenticator, executor mint, dispatch and
// registry services over the durable embedded stores, and a payment-style
// target. The only seam is the worker's clock, which is the harness's, so the
// explorer and the worker read the same time.

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/effectsgateway"
	"github.com/phrocker/shoal-oss/internal/effectsgateway/gatewaycmd"
	"github.com/phrocker/shoal-oss/internal/executorattest"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// newCommandWorld is newExecutorWorld with the stripe reference bound to an
// external-mutation ceiling, as -fleet-external-executor-refs=stripe binds
// it, and a descriptor registered the way a gateway's must be: one
// capability whose actions are the route table's, effects as derived from a
// loopback target, and the gateway's closed output schema.
func newCommandWorld(t *testing.T, routes string, target *url.URL) *gatewayOps {
	t.Helper()
	return newCommandWorldWith(t, routes, target, nil, nil)
}

// newCommandWorldWith is newCommandWorld with the service opened under an
// executor attestation trust when attestation is set, as
// -fleet-executor-attestation opens it, and every action of the gateway's
// capability then requiring attestation; prepare, when set, runs on the
// opened world before the descriptor is registered.
func newCommandWorldWith(
	t *testing.T, routes string, target *url.URL, attestation *executorattest.Trust,
	prepare func(*executorWorld),
) *gatewayOps {
	t.Helper()
	executorIssuer := newFakeOIDCIssuer(t)
	executors, err := newConfiguredFleetExecutors(
		[]string{"local", testExecutorRef, testOtherExecutorRef})
	if err != nil {
		t.Fatal(err)
	}
	if err := bindExternalFleetEffects(executors, externalFleetEffectBindings{
		mutating: []string{testExecutorRef},
	}); err != nil {
		t.Fatal(err)
	}
	mapping := writeExecutorMapping(t, executorMappingDocument(executorIssuer.server.URL))
	world := &executorWorld{
		oidcApprovalWorld: newOIDCApprovalWorldOn(t, func(config *oidcConfig) {
			config.executorMappingFile = mapping
		}, nil, nil, approverMappingDocument, executors),
		executorIssuer: executorIssuer,
	}
	if prepare != nil {
		prepare(world)
	}
	if attestation != nil {
		world.h.attestation = attestation
		world.h.reopen()
	}
	table, err := effectsgateway.ParseRoutes([]byte(routes), effectsgateway.DerivedEffects(target))
	if err != nil {
		t.Fatal(err)
	}
	actions := []fleet.Action{}
	for _, name := range table.Actions() {
		route, _ := table.Lookup(name)
		actions = append(actions, fleet.Action{
			Name: name, Effects: fleetEffectsOf(route.Effects()),
			InputSchema: route.InputSchema(), OutputSchema: effectsgateway.OutputSchema(),
			RequiresAttestation: attestation != nil,
		})
	}
	got := world.post(call{token: world.fleetToken("owner", nil)}, "/api/v1/fleet/agents", map[string]any{
		"context":          world.contextWire(world.h.now().Add(time.Minute)),
		"registration_key": b64([]byte("registration-effects-agent")),
		"descriptor": map[string]any{
			"id":                   b64([]byte(commandAgent)),
			"authorization_domain": workspaceAuthorizationDomain,
			"scopes":               []fleet.Scope{{SourceID: workspaceSourceID, PolicyID: workspaceGrantPolicyID}},
			"executor_ref":         testExecutorRef,
			"capabilities": []fleet.Capability{{
				Name: effectsgateway.DefaultCapability, Actions: actions,
			}},
			"lease_expires_at": world.h.now().Add(20 * time.Hour).Format(time.RFC3339Nano),
		},
	})
	if got.status != 201 {
		t.Fatalf("register the gateway descriptor = %d %s", got.status, got.raw)
	}
	return &gatewayOps{executorWorld: world}
}

const commandAgent shoal.ID = "effects-agent"

// enqueueCommand queues "status" for the gateway's descriptor, as alice.
func (g *gatewayOps) enqueueCommand(id string) {
	g.t.Helper()
	got := g.post(call{token: g.fleetToken("alice", nil), correlation: "alice-trace-" + id},
		"/api/v1/fleet/actions", map[string]any{
			"context":          g.contextWire(g.h.now().Add(time.Hour)),
			"id":               b64([]byte(id)),
			"idempotency_key":  b64([]byte("key-" + id)),
			"agent_id":         b64([]byte(commandAgent)),
			"agent_generation": 1,
			"capability":       effectsgateway.DefaultCapability, "action": "status",
			"source_id": workspaceSourceID, "policy_id": workspaceGrantPolicyID,
			"object_id": b64([]byte("object-" + id)),
			"input":     map[string]any{},
		})
	if got.status != 201 {
		g.t.Fatalf("enqueue %s = %d %s", id, got.status, got.raw)
	}
}

// TestTheGatewayCommandPerformsAnActionEndToEnd: shoal-gateway run, as the
// binary composes it, takes the lock, resolves its own descriptor with the
// minted executor credential, checks it against the routes, pulls, claims,
// performs the one request under the action's ExecutorKey, completes on the
// fence — and on SIGTERM drains to a clean exit and releases the directory.
func TestTheGatewayCommandPerformsAnActionEndToEnd(t *testing.T) {
	target := newPaymentTarget(t)
	targetURL, err := url.Parse(target.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	g := newCommandWorld(t, statusRoutes, targetURL)
	clock := &harnessClock{now: g.h.now, advance: g.h.advance}
	g.enqueueCommand("cmd-happy")

	token := filepath.Join(t.TempDir(), "token")
	minted := g.executorToken(testExecutorSubject)
	if err := os.WriteFile(token, []byte(minted), 0o600); err != nil {
		t.Fatal(err)
	}
	credential := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(credential, []byte("Bearer sk_test_integration"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "unrecorded")
	g.record()

	signals := make(chan os.Signal, 2)
	stdout, stderr := &syncBuffer{}, &syncBuffer{}
	started := make(chan string, 1)
	exited := make(chan int, 1)
	go func() {
		exited <- gatewaycmd.Main([]string{"run",
			"-dispatch-url=" + g.server.URL, "-dispatch-token-file=" + token,
			"-agent-id=" + b64([]byte(commandAgent)), "-executor-ref=" + testExecutorRef,
			"-surface-name=api.stripe.test",
			"-target-base-url=" + target.server.URL, "-target-allow-private",
			"-target-credential-file=" + credential,
			"-routes=" + statusRoutes,
			"-idempotency-header=Idempotency-Key", "-idempotency-retention=48h",
			"-unrecorded-dir=" + dir, "-pod-name=gw-0", "-health-address=127.0.0.1:0",
		}, gatewaycmd.Env{
			Stdout: stdout, Stderr: stderr, Signals: signals, Clock: clock,
			Started: func(address string) { started <- address },
		})
	}()
	t.Cleanup(func() {
		signals <- syscall.SIGINT
		signals <- syscall.SIGINT
		select {
		case code := <-exited:
			exited <- code
		case <-time.After(20 * time.Second):
			t.Error("the command did not stop")
		}
	})
	select {
	case <-started:
	case code := <-exited:
		exited <- code
		t.Fatalf("the command exited %d at startup: %s", code, stderr)
	case <-time.After(20 * time.Second):
		t.Fatal("the command did not start")
	}
	// The directory is the command's: a second gateway would stop at the lock.
	if _, err := effectsgateway.OpenUnrecordedLog(dir, nil); err != effectsgateway.ErrGatewayLocked {
		t.Fatalf("a second open of the running gateway's directory: %v", err)
	}

	waitFor(t, "success", func() bool { return g.view("cmd-happy").State == string(fleet.DispatchSucceeded) })
	view := g.view("cmd-happy")
	if !strings.Contains(string(view.Output), `"reference":"ch_1"`) ||
		!strings.Contains(string(view.Output), `"idempotency":"key"`) {
		t.Fatalf("output = %s", view.Output)
	}
	effects, requests, keys := target.stats()
	if effects != 1 || requests != 1 || len(keys[0]) != 43 {
		t.Fatalf("target: %d effects, %d requests, keys %q", effects, requests, keys)
	}
	// The completed event is written after the explorer answers, so after
	// the record is visible.
	waitFor(t, "the completed event", func() bool {
		return strings.Contains(stdout.String(), `"event":"completed"`)
	})
	logs := stdout.String()
	for _, event := range []string{"claimed", "sent", "completed"} {
		if !strings.Contains(logs, `"event":"`+event+`"`) {
			t.Fatalf("no %s event:\n%s", event, logs)
		}
	}
	if strings.Contains(logs, "sk_test_integration") || strings.Contains(logs, minted) {
		t.Fatalf("a credential reached the log:\n%s", logs)
	}

	signals <- syscall.SIGTERM
	select {
	case code := <-exited:
		exited <- code
		if code != gatewaycmd.ExitOK {
			t.Fatalf("exit %d: %s", code, stderr)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("SIGTERM did not drain the command")
	}
	log, err := effectsgateway.OpenUnrecordedLog(dir, nil)
	if err != nil {
		t.Fatalf("the exited command kept the directory: %v", err)
	}
	if log.Len() != 0 {
		t.Fatalf("%d unrecorded entries after a clean run", log.Len())
	}
	_ = log.Close()
}
