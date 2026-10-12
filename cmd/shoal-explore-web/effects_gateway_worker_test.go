// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package main

// The effects gateway's worker loop (#391, PR5) against the real explorer:
// the executor world's real OIDC authenticator, executor mint, dispatch and
// registry services over the durable embedded stores, reached over HTTP by
// the worker's own dispatch client holding the minted executor credential;
// and a payment-style target with an idempotency-key store. The worker's
// clock is the harness clock, so a test moves the explorer's time and the
// worker's together.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/effectsgateway"
	dispatchapi "github.com/phrocker/shoal-oss/pkg/dispatch/api"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
)

// harnessClock is the approval harness's clock with timers: Advance moves the
// explorer's time and fires every worker timer it passes.
type harnessClock struct {
	now     func() time.Time
	advance func(time.Duration)

	mu      sync.Mutex
	waiters []harnessWaiter
}

type harnessWaiter struct {
	at time.Time
	ch chan time.Time
}

func (c *harnessClock) Now() time.Time { return c.now() }

func (c *harnessClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	at := c.now().Add(d)
	if d <= 0 {
		ch <- at
		return ch
	}
	c.waiters = append(c.waiters, harnessWaiter{at: at, ch: ch})
	return ch
}

func (c *harnessClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.advance(d)
	now := c.now()
	kept := c.waiters[:0]
	for _, waiter := range c.waiters {
		if !waiter.at.After(now) {
			waiter.ch <- waiter.at
			continue
		}
		kept = append(kept, waiter)
	}
	c.waiters = kept
}

// dateTransport stamps each explorer response with the harness's time, as
// the explorer's own Date would read on a clock that is not frozen.
type dateTransport struct {
	now  func() time.Time
	next http.RoundTripper
	// drop, when it returns true for a request, forwards it and then loses
	// the answer: the request is processed, the worker never hears.
	drop func(*http.Request) bool
}

func (d dateTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := d.next.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	if d.drop != nil && d.drop(request) {
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		return nil, errors.New("connection reset")
	}
	response.Header.Set("Date", d.now().UTC().Format(http.TimeFormat))
	return response, nil
}

// paymentTarget performs one effect per new idempotency key and replays the
// answer for a key it has seen.
type paymentTarget struct {
	server *httptest.Server

	mu       sync.Mutex
	effects  int
	requests int
	keys     []string
	cache    map[string]string
	status   int
	gate     chan struct{}
	entered  chan struct{}
	drop     bool
}

func newPaymentTarget(t *testing.T) *paymentTarget {
	target := &paymentTarget{cache: map[string]string{}, entered: make(chan struct{}, 16)}
	target.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		key := r.Header.Get("Idempotency-Key")
		target.mu.Lock()
		target.requests++
		n := target.requests
		target.keys = append(target.keys, key)
		if target.status != 0 {
			status := target.status
			target.mu.Unlock()
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"declined"}`))
			return
		}
		cached, seen := target.cache[key]
		if !seen {
			target.effects++
			cached = fmt.Sprintf(`{"id":"ch_%d"}`, target.effects)
			target.cache[key] = cached
		}
		gate, drop := target.gate, target.drop && n == 1
		target.mu.Unlock()
		if gate != nil {
			target.entered <- struct{}{}
			<-gate
		}
		if drop {
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				_ = conn.Close()
			}
			return
		}
		if seen {
			w.Header().Set("Idempotent-Replayed", "true")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, cached)
	}))
	t.Cleanup(target.server.Close)
	// Cleanups run last-in first-out: a held request is released before the
	// server's Close waits for it.
	t.Cleanup(target.release)
	return target
}

func (p *paymentTarget) stats() (effects, requests int, keys []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.effects, p.requests, append([]string(nil), p.keys...)
}

func (p *paymentTarget) hold() {
	p.mu.Lock()
	p.gate = make(chan struct{})
	p.mu.Unlock()
}

func (p *paymentTarget) release() {
	p.mu.Lock()
	gate := p.gate
	p.gate = nil
	p.mu.Unlock()
	if gate != nil {
		close(gate)
	}
}

// advanceUntil moves the clock a step at a time until cond holds, giving the
// worker time to act between steps, and never steps once it does.
func advanceUntil(t *testing.T, clock *harnessClock, step time.Duration, cond func() bool) {
	t.Helper()
	for i := 0; i < 10; i++ {
		if cond() {
			return
		}
		clock.Advance(step)
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if cond() {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	t.Fatal("the clock ran on and the condition never held")
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

const statusRoutes = `[{"action":"status","method":"POST","path":"/v1/status","effects":["external"],` +
	`"idempotency":"key","reference":{"pointer":"/id","pattern":"^ch_[0-9]+$"}}]`

// gatewayWorker is one worker process: its clock, log directory and logs.
type gatewayWorker struct {
	worker *effectsgateway.Worker
	log    *effectsgateway.UnrecordedLog
	logs   *syncBuffer
	cancel context.CancelFunc
	done   chan error
}

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type workerOptions struct {
	dir         string
	maxInFlight int
	drop        func(*http.Request) bool
}

// startWorker composes a worker the way the command will: the bound
// dispatch client with the minted executor credential, the egress-restricted
// target client, the route table and the unrecorded log, and runs it.
func (g *gatewayOps) startWorker(clock *harnessClock, target *paymentTarget, options workerOptions) *gatewayWorker {
	t := g.t
	t.Helper()
	g.record()
	base, err := url.Parse(g.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	httpClient := *g.server.Client()
	httpClient.Transport = dateTransport{now: g.h.now, next: g.server.Client().Transport, drop: options.drop}
	client, err := effectsgateway.NewDispatchClient(base, &httpClient,
		func() (string, error) { return g.executorToken(testExecutorSubject), nil }, g.h.now)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := client.BindExecutor(testExecutorRef)
	if err != nil {
		t.Fatal(err)
	}
	return startWorkerWith(t, bound, clock, target, statusRoutes, "stripe-agent", "ops", options, nil)
}

func startWorkerWith(
	t *testing.T, dispatch effectsgateway.Dispatcher, clock *harnessClock, target *paymentTarget,
	routes, agent, capability string, options workerOptions, mutate func(*effectsgateway.WorkerConfig),
) *gatewayWorker {
	t.Helper()
	targetURL, err := url.Parse(target.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	table, err := effectsgateway.ParseRoutes([]byte(routes), effectsgateway.DerivedEffects(targetURL))
	if err != nil {
		t.Fatal(err)
	}
	binder, err := effectsgateway.NewBinder(targetURL, "Idempotency-Key")
	if err != nil {
		t.Fatal(err)
	}
	policy, err := effectsgateway.NewEgressPolicy(targetURL, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	targetClient, err := effectsgateway.NewTargetClient(policy, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	log, err := effectsgateway.OpenUnrecordedLog(options.dir, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	config := effectsgateway.WorkerConfig{
		Dispatch: dispatch, Target: targetClient, Binder: binder, Routes: table,
		TargetAuthorization: func() (string, string, error) {
			return "Authorization", "Bearer sk_test_integration", nil
		},
		AgentID: []byte(agent), Capability: capability, SurfaceName: "api.stripe.test",
		Pod: "gw-0", IdempotencyRetention: 48 * time.Hour,
		ClaimLease: 60 * time.Second, OperationTimeout: 64 * time.Second, Renew: true,
		PlaneTimeout: 5 * time.Second, MaxResponseBytes: 64 << 10, PullLimit: 64,
		PullInterval: time.Second, MaxInFlight: options.maxInFlight,
		Unrecorded: log, Logger: effectsgateway.NewLogger(logs, clock.Now), Clock: clock,
	}
	if mutate != nil {
		mutate(&config)
	}
	worker, err := effectsgateway.NewWorker(config)
	if err != nil {
		_ = log.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	running := &gatewayWorker{worker: worker, log: log, logs: logs, cancel: cancel, done: make(chan error, 1)}
	go func() { running.done <- worker.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		worker.Kill()
		running.wait(t)
		_ = log.Close()
	})
	return running
}

func (w *gatewayWorker) wait(t *testing.T) error {
	select {
	case err := <-w.done:
		w.done <- err
		return err
	case <-time.After(20 * time.Second):
		t.Fatal("the worker did not stop")
	}
	return nil
}

// stop drains the worker and releases its directory, as a clean exit does.
func (w *gatewayWorker) stop(t *testing.T) error {
	t.Helper()
	w.cancel()
	err := w.wait(t)
	_ = w.log.Close()
	return err
}

// crash abandons everything, as SIGKILL does; the kernel drops the flock.
func (w *gatewayWorker) crash(t *testing.T) {
	t.Helper()
	w.worker.Kill()
	_ = w.wait(t)
	_ = w.log.Close()
}

func (w *gatewayWorker) has(event string) bool {
	return strings.Contains(w.logs.String(), `"event":"`+event+`"`)
}

// recordView is the dispatch status wire, as alice (the enqueuer) reads it.
type recordView struct {
	State            string          `json:"state"`
	Version          uint64          `json:"version"`
	ErrorCode        string          `json:"error_code"`
	Output           json.RawMessage `json:"output"`
	ClaimID          string          `json:"claim_id"`
	ClaimFence       uint64          `json:"claim_fence"`
	ClaimLeaseUntil  time.Time       `json:"claim_lease_until"`
	CorrelationID    string          `json:"correlation_id"`
	AmbiguityReports []struct {
		ClaimFence uint64 `json:"claim_fence"`
		Outcome    string `json:"outcome"`
		Target     string `json:"target"`
		Reference  string `json:"reference"`
	} `json:"ambiguity_reports"`
}

func (g *gatewayOps) view(id string) recordView {
	g.t.Helper()
	read := func() answer {
		got, _ := g.send(call{token: g.fleetToken("alice", nil)},
			"/api/v1/fleet/actions/"+b64([]byte(id))+"/status", g.contextWire(g.h.now().Add(time.Minute)))
		return got
	}
	got := read()
	// #633: a concurrent claim or extend write can briefly make the live
	// agent read as not found, so this test-side status read retries a 404 a
	// few times. It is the explorer's gap, not the gateway's, and nothing in
	// the gateway masks it.
	for attempt := 0; got.status == http.StatusNotFound && attempt < 20; attempt++ {
		time.Sleep(10 * time.Millisecond)
		got = read()
	}
	if got.status != http.StatusOK {
		g.t.Fatalf("status %s = %d %s", id, got.status, got.raw)
	}
	var view recordView
	if err := json.Unmarshal(got.raw, &view); err != nil {
		g.t.Fatal(err)
	}
	return view
}

func newWorld(t *testing.T) (*gatewayOps, *harnessClock, *paymentTarget) {
	g := newGatewayOps(t)
	clock := &harnessClock{now: g.h.now, advance: g.h.advance}
	return g, clock, newPaymentTarget(t)
}

func entered(t *testing.T, target *paymentTarget) {
	t.Helper()
	select {
	case <-target.entered:
	case <-time.After(20 * time.Second):
		t.Fatal("the target saw no request")
	}
	time.Sleep(50 * time.Millisecond)
}

// TestTheWorkerPerformsAnActionEndToEnd: enqueued by alice, pulled, claimed,
// sent under the action's ExecutorKey, and completed on the fence by the
// minted executor; the record is succeeded with the gateway's closed output.
func TestTheWorkerPerformsAnActionEndToEnd(t *testing.T) {
	g, clock, target := newWorld(t)
	g.enqueueUntil("w-happy", "alice-trace-happy", g.h.now().Add(time.Hour))
	worker := g.startWorker(clock, target, workerOptions{dir: t.TempDir()})
	waitFor(t, "success", func() bool { return g.view("w-happy").State == string(fleet.DispatchSucceeded) })
	view := g.view("w-happy")
	var output map[string]any
	if err := json.Unmarshal(view.Output, &output); err != nil || output["reference"] != "ch_1" ||
		output["idempotency"] != "key" {
		t.Fatalf("output = %s", view.Output)
	}
	if effects, requests, keys := target.stats(); effects != 1 || requests != 1 || len(keys[0]) != 43 {
		t.Fatalf("target: %d effects, %d requests, keys %q", effects, requests, keys)
	}
	if ready, reason := worker.worker.Ready(); !ready {
		t.Fatalf("not ready: %s", reason)
	}
	if err := worker.stop(t); err != nil {
		t.Fatal(err)
	}
}

// TestTheWorkerRecordsATargetRefusal: a 422 is one request and a failed
// record, target_rejected_422.
func TestTheWorkerRecordsATargetRefusal(t *testing.T) {
	g, clock, target := newWorld(t)
	target.status = http.StatusUnprocessableEntity
	g.enqueueUntil("w-422", "alice-trace-422", g.h.now().Add(time.Hour))
	g.startWorker(clock, target, workerOptions{dir: t.TempDir()})
	waitFor(t, "failure", func() bool { return g.view("w-422").State == string(fleet.DispatchFailed) })
	if view := g.view("w-422"); view.ErrorCode != "target_rejected_422" {
		t.Fatalf("error code %q", view.ErrorCode)
	}
	if _, requests, _ := target.stats(); requests != 1 {
		t.Fatalf("%d requests", requests)
	}
}

// TestTheWorkerResendsALostTargetResponseForOneEffect: the target performs
// the effect and the response is lost; the resend under the same key is
// replayed. One effect, a succeeded record.
func TestTheWorkerResendsALostTargetResponseForOneEffect(t *testing.T) {
	g, clock, target := newWorld(t)
	target.drop = true
	g.enqueueUntil("w-lost", "alice-trace-lost", g.h.now().Add(time.Hour))
	g.startWorker(clock, target, workerOptions{dir: t.TempDir()})
	waitFor(t, "the first request", func() bool { _, n, _ := target.stats(); return n >= 1 })
	for i := 0; i < 10 && g.view("w-lost").State != string(fleet.DispatchSucceeded); i++ {
		time.Sleep(50 * time.Millisecond)
		clock.Advance(time.Second)
	}
	waitFor(t, "success", func() bool { return g.view("w-lost").State == string(fleet.DispatchSucceeded) })
	effects, requests, keys := target.stats()
	var output map[string]any
	_ = json.Unmarshal(g.view("w-lost").Output, &output)
	if effects != 1 || requests != 2 || keys[0] != keys[1] || output["idempotency"] != "replayed" {
		t.Fatalf("%d effects, %d requests, output %v", effects, requests, output)
	}
}

// TestTheWorkerReportsAnExtensionRefusedMidFlight: the descriptor is rebound
// to another executor ref while the request is in flight, so the explorer
// refuses the extension; the request finishes and the worker reports
// effect_observed, which the explorer records under the claim's ref.
func TestTheWorkerReportsAnExtensionRefusedMidFlight(t *testing.T) {
	g, clock, target := newWorld(t)
	target.hold()
	g.enqueueUntil("w-rebind", "alice-trace-rebind", g.h.now().Add(time.Hour))
	worker := g.startWorker(clock, target, workerOptions{dir: t.TempDir()})
	entered(t, target)
	got := g.post(call{token: g.fleetToken("owner", nil)}, "/api/v1/fleet/agents", map[string]any{
		"context":             g.contextWire(g.h.now().Add(time.Minute)),
		"registration_key":    b64([]byte("rebind-stripe-agent")),
		"expected_generation": 1,
		"descriptor": map[string]any{
			"id":                   b64([]byte("stripe-agent")),
			"authorization_domain": workspaceAuthorizationDomain,
			"scopes":               []fleet.Scope{{SourceID: workspaceSourceID, PolicyID: workspaceGrantPolicyID}},
			"executor_ref":         testOtherExecutorRef,
			"capabilities":         approvalActions(true),
			"lease_expires_at":     g.h.now().Add(20 * time.Hour).Format(time.RFC3339Nano),
		},
	})
	if got.status != http.StatusOK && got.status != http.StatusCreated {
		t.Fatalf("rebind = %d %s", got.status, got.raw)
	}
	clock.Advance(30 * time.Second)
	waitFor(t, "the fence to be lost", func() bool { return worker.has("fence_lost") })
	target.release()
	waitFor(t, "the report", func() bool { return len(g.view("w-rebind").AmbiguityReports) == 1 })
	view := g.view("w-rebind")
	report := view.AmbiguityReports[0]
	if report.Outcome != string(fleet.AmbiguityEffectObserved) || report.Reference != "ch_1" ||
		report.Target != "api.stripe.test" || report.ClaimFence != view.ClaimFence {
		t.Fatalf("report = %+v", report)
	}
	if view.State != string(fleet.DispatchClaimed) {
		t.Fatalf("state %s: a lost fence was completed", view.State)
	}
	if worker.worker.UnrecordedEntries() != 0 {
		t.Fatal("a recorded report was held as unrecorded")
	}
}

// writeHeldEntries fills dir's unrecorded log with n entries for actions the
// explorer has never seen, in the log's documented line format.
func writeHeldEntries(t *testing.T, dir string, n int) {
	t.Helper()
	var lines strings.Builder
	for i := 0; i < n; i++ {
		line, _ := json.Marshal(map[string]any{
			"action_id": base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("held-%04d", i))),
			"fence":     1, "route_action": "status", "method": "POST", "path_template": "/v1/status",
			"outcome": "outcome_unknown", "correlation_id": base64.RawURLEncoding.EncodeToString([]byte("held")),
			"first_at": time.Now().UTC(), "last_at": time.Now().UTC(), "attempts": 1,
		})
		lines.Write(line)
		lines.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(dir, effectsgateway.UnrecordedFileName), []byte(lines.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestARefusedReportIsHeldAndAFullLogStopsClaiming: the per-record report
// budget is spent by others (#514), so the worker's report is refused; it is
// held in the unrecorded log, which is now full: the worker is not ready and
// claims nothing more, and a restarted worker still holds it until the
// operator acknowledges it — after which work resumes.
func TestARefusedReportIsHeldAndAFullLogStopsClaiming(t *testing.T) {
	g, clock, target := newWorld(t)
	dir := t.TempDir()
	writeHeldEntries(t, dir, effectsgateway.UnrecordedMaxEntries-1)
	target.hold()
	g.enqueueUntil("w-spent", "alice-trace-spent", g.h.now().Add(time.Hour))
	worker := g.startWorker(clock, target, workerOptions{dir: dir, maxInFlight: 1})
	entered(t, target)

	// Spend the record's report budget under this claim's fence.
	fence := g.view("w-spent").ClaimFence
	client := g.gateway(testExecutorRef)
	for index := 1; index <= fleet.MaxActionAmbiguityReports; index++ {
		if _, err := client.ReportAmbiguity(context.Background(), []byte("w-spent"), effectsgateway.AmbiguityReport{
			Context: effectsgateway.RequestContext{RequestID: []byte(fmt.Sprintf("spend-%d", index)),
				CorrelationID: []byte("alice-trace-spent"), ReasonCode: "spend", Deadline: g.h.now().Add(time.Minute)},
			ClaimFence: fence, Outcome: dispatchapi.AmbiguityOutcomeUnknown, Reference: fmt.Sprintf("spent_%d", index),
		}); err != nil {
			t.Fatalf("spend %d: %v", index, err)
		}
	}
	// The local lease runs out with the request in flight.
	clock.Advance(61 * time.Second)
	waitFor(t, "the fence to be lost", func() bool { return worker.has("fence_lost") })
	target.release()
	waitFor(t, "the held report", func() bool {
		return worker.worker.UnrecordedEntries() == effectsgateway.UnrecordedMaxEntries
	})
	waitFor(t, "not-ready", func() bool {
		_, reason := worker.worker.Ready()
		return reason == effectsgateway.NotReadyUnrecordedFull
	})
	var held effectsgateway.UnrecordedEntry
	for _, entry := range worker.log.Entries() {
		if string(entry.ActionID) == "w-spent" {
			held = entry
		}
	}
	if held.Fence != fence || held.Outcome != dispatchapi.AmbiguityEffectObserved || held.Reference != "ch_1" ||
		held.Status != http.StatusBadRequest || held.RouteAction != "status" || held.PathTemplate != "/v1/status" {
		t.Fatalf("held = %+v", held)
	}

	g.enqueueUntil("w-after", "alice-trace-after", g.h.now().Add(time.Hour))
	for i := 0; i < 3; i++ {
		clock.Advance(30 * time.Second)
		time.Sleep(50 * time.Millisecond)
	}
	if state := g.view("w-after").State; state != string(fleet.DispatchQueued) {
		t.Fatalf("claimed with a full log: %s", state)
	}
	if err := worker.stop(t); err != nil {
		t.Fatal(err)
	}

	// Restarted: every entry is retried; the refused one is still refused.
	restarted := g.startWorker(clock, target, workerOptions{dir: dir, maxInFlight: 1})
	waitFor(t, "the restart's retries", func() bool {
		_, reason := restarted.worker.Ready()
		return reason == effectsgateway.NotReadyUnrecordedFull
	})
	if restarted.worker.UnrecordedEntries() != effectsgateway.UnrecordedMaxEntries {
		t.Fatalf("%d entries after the restart", restarted.worker.UnrecordedEntries())
	}
	if removed, err := restarted.worker.AckUnrecorded([]byte("w-spent"), fence); err != nil || !removed {
		t.Fatalf("ack = %v, %v", removed, err)
	}
	advanceUntil(t, clock, 30*time.Second, func() bool { return g.view("w-after").State != string(fleet.DispatchQueued) })
	waitFor(t, "work to resume", func() bool { return g.view("w-after").State == string(fleet.DispatchSucceeded) })
}

// TestALostReportAnswerIsHeldAndClearedByTheReplayAfterRestart: every answer
// on the ambiguity route is lost, though the explorer recorded the report;
// the worker holds it as unrecorded. A restarted worker presents it again,
// the explorer answers the identical replay (#542), and the entry clears,
// with one report on the record.
func TestALostReportAnswerIsHeldAndClearedByTheReplayAfterRestart(t *testing.T) {
	g, clock, target := newWorld(t)
	dir := t.TempDir()
	target.hold()
	g.enqueueUntil("w-replay", "alice-trace-replay", g.h.now().Add(time.Hour))
	worker := g.startWorker(clock, target, workerOptions{dir: dir, maxInFlight: 1,
		drop: func(request *http.Request) bool { return strings.HasSuffix(request.URL.Path, "/ambiguity") }})
	entered(t, target)
	clock.Advance(61 * time.Second)
	waitFor(t, "the fence to be lost", func() bool { return worker.has("fence_lost") })
	target.release()
	waitFor(t, "the held report", func() bool { return worker.worker.UnrecordedEntries() == 1 })
	if entry := worker.log.Entries()[0]; entry.DispatchError != effectsgateway.DispatchIndeterminate {
		t.Fatalf("entry = %+v", entry)
	}
	if err := worker.stop(t); err != nil {
		t.Fatal(err)
	}

	restarted := g.startWorker(clock, target, workerOptions{dir: dir, maxInFlight: 1})
	waitFor(t, "the entry to clear", func() bool { return restarted.worker.UnrecordedEntries() == 0 })
	waitFor(t, "the unrecorded_cleared event", func() bool { return restarted.has("unrecorded_cleared") })
	if reports := g.view("w-replay").AmbiguityReports; len(reports) != 1 ||
		reports[0].Outcome != string(fleet.AmbiguityEffectObserved) {
		t.Fatalf("reports on the record = %+v", reports)
	}
}

// TestTheWorkerDrainsInFlightWorkOnShutdown: shutdown stops pulling and goes
// not-ready; the request in flight finishes and completes.
func TestTheWorkerDrainsInFlightWorkOnShutdown(t *testing.T) {
	g, clock, target := newWorld(t)
	target.hold()
	g.enqueueUntil("w-drain", "alice-trace-drain", g.h.now().Add(time.Hour))
	worker := g.startWorker(clock, target, workerOptions{dir: t.TempDir()})
	entered(t, target)
	worker.cancel()
	waitFor(t, "draining", func() bool {
		_, reason := worker.worker.Ready()
		return reason == effectsgateway.NotReadyDraining
	})
	target.release()
	if err := worker.stop(t); err != nil {
		t.Fatal(err)
	}
	if state := g.view("w-drain").State; state != string(fleet.DispatchSucceeded) {
		t.Fatalf("state after the drain = %s", state)
	}
}

// TestACrashMidEffectIsRecoveredWithOneEffect: the worker dies with the
// target's effect done and unanswered. A second instance cannot start while
// the first holds the directory; once it is gone, the restarted worker waits
// out the lapse, re-claims, resends under the same ExecutorKey — which the
// target replays — and completes. One effect, and the dead claim's
// completion is refused.
func TestACrashMidEffectIsRecoveredWithOneEffect(t *testing.T) {
	g, clock, target := newWorld(t)
	dir := t.TempDir()
	target.hold()
	g.enqueueUntil("w-crash", "alice-trace-crash", g.h.now().Add(time.Hour))
	first := g.startWorker(clock, target, workerOptions{dir: dir})
	entered(t, target)
	dead := g.view("w-crash")

	if _, err := effectsgateway.OpenUnrecordedLog(dir, nil); !errors.Is(err, effectsgateway.ErrGatewayLocked) {
		t.Fatalf("a second instance on a held directory = %v", err)
	}
	first.crash(t)
	target.release()

	second := g.startWorker(clock, target, workerOptions{dir: dir})
	time.Sleep(50 * time.Millisecond)
	if state := g.view("w-crash").State; state != string(fleet.DispatchClaimed) {
		t.Fatalf("state before the lapse = %s", state)
	}
	// Past the lapse, and no further once the second worker holds a claim:
	// its lease is then its own.
	advanceUntil(t, clock, 31*time.Second, func() bool { return g.view("w-crash").ClaimFence != dead.ClaimFence })
	waitFor(t, "recovery", func() bool { return g.view("w-crash").State == string(fleet.DispatchSucceeded) })
	view := g.view("w-crash")
	effects, requests, keys := target.stats()
	if effects != 1 || requests != 2 || keys[0] != keys[1] {
		t.Fatalf("%d effects over %d requests, keys %q", effects, requests, keys)
	}
	if view.ClaimFence == dead.ClaimFence {
		t.Fatal("completed under the dead claim's fence")
	}
	var output map[string]any
	if err := json.Unmarshal(view.Output, &output); err != nil || output["idempotency"] != "replayed" {
		t.Fatalf("output = %s", view.Output)
	}

	// The dead claim's completion, were it ever sent, is refused.
	deadClaimID, err := base64.RawURLEncoding.DecodeString(dead.ClaimID)
	if err != nil {
		t.Fatal(err)
	}
	client := g.gateway(testExecutorRef)
	_, err = client.Complete(context.Background(), []byte("w-crash"), effectsgateway.Completion{
		Context: effectsgateway.RequestContext{RequestID: []byte("dead-complete"),
			CorrelationID: []byte("alice-trace-crash"), ReasonCode: "gateway_complete",
			Deadline: g.h.now().Add(time.Minute)},
		ExpectedVersion: dead.Version, ClaimID: deadClaimID, ClaimFence: dead.ClaimFence,
		Output: json.RawMessage(`{"status":200,"idempotency":"key","reference":"ch_1"}`),
	})
	if err == nil {
		t.Fatal("the dead claim's completion was accepted")
	}
	if after := g.view("w-crash"); after.Version != view.Version || after.ClaimFence != view.ClaimFence {
		t.Fatalf("the refused completion moved the record: %+v", after)
	}
	if err := second.stop(t); err != nil {
		t.Fatal(err)
	}
}
