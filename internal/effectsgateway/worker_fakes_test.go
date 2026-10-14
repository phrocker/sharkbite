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

package effectsgateway

// Scripted fakes for the worker's unit tests: a clock advanced by hand, an
// explorer that keeps records the way the dispatch service does (and can be
// told to answer otherwise), and a target with an idempotency-key store.
// The real explorer is exercised by the integration tests in
// cmd/shoal-explore-web.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	dispatchapi "github.com/phrocker/shoal-oss/pkg/dispatch/api"
)

var workerEpoch = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

// timerClock is advanced by hand; a timer fires when Advance passes it.
type timerClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []fakeWaiter
}

type fakeWaiter struct {
	at time.Time
	ch chan time.Time
}

func newTimerClock() *timerClock { return &timerClock{now: workerEpoch} }

func (c *timerClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *timerClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	at := c.now.Add(d)
	if d <= 0 {
		ch <- at
		return ch
	}
	c.waiters = append(c.waiters, fakeWaiter{at: at, ch: ch})
	return ch
}

// waiting reports whether a timer is pending that fires d from now: how a
// test knows the code under test has armed the timer it is about to pass.
func (c *timerClock) waiting(d time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, waiter := range c.waiters {
		if waiter.at.Equal(c.now.Add(d)) {
			return true
		}
	}
	return false
}

func (c *timerClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.waiters[:0]
	for _, waiter := range c.waiters {
		if !waiter.at.After(c.now) {
			waiter.ch <- waiter.at
			continue
		}
		kept = append(kept, waiter)
	}
	c.waiters = kept
}

// eventually polls cond in real time.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// settle lets goroutines woken by an Advance run.
func settle() { time.Sleep(20 * time.Millisecond) }

const (
	testAgent      = "stripe-agent"
	testCapability = "effects.http"
	testSurface    = "api.stripe.test"
)

// fakeRecord is one action as the fake explorer holds it.
type fakeRecord struct {
	action     Action
	input      json.RawMessage
	key        ExecutorKey
	leaseUntil time.Time
}

// fakeExplorer is a scripted Dispatcher.
type fakeExplorer struct {
	t     *testing.T
	clock *timerClock

	mu      sync.Mutex
	records map[string]*fakeRecord
	order   []string
	fence   uint64

	pulls       int
	claims      []ClaimRequest
	extends     []ExtendRequest
	completions []Completion
	reports     []AmbiguityReport
	reportIDs   [][]byte
	attests     int
	calls       []string

	attestValidity time.Duration
	// gate, when set, runs first on every call, outside the lock; a non-nil
	// error is the call's answer. It is how a test makes a call block until
	// its context ends.
	gate func(ctx context.Context, op string) error
	// loseExtendAnswer applies an extension and then answers it as lost;
	// with extendApplyDelay the application happens that long after the
	// answer, as a write already under way when the caller gave up would.
	loseExtendAnswer bool
	extendApplyDelay time.Duration
	// skew puts the explorer's clock this far behind the worker's.
	skew time.Duration
	// reportAt is the clock reading at each ambiguity report's start.
	reportAt []time.Time
	// Hooks answer instead of the default when they return handled.
	onClaim     func(n int, id []byte, request ClaimRequest) (Action, error, bool)
	onExtend    func(n int, id []byte, request ExtendRequest) (Action, error, bool)
	onComplete  func(n int, id []byte, completion Completion) (Action, error, bool)
	onAmbiguity func(n int, id []byte, report AmbiguityReport) (Action, error, bool)
}

func newFakeExplorer(t *testing.T, clock *timerClock) *fakeExplorer {
	return &fakeExplorer{t: t, clock: clock, records: map[string]*fakeRecord{},
		attestValidity: time.Hour}
}

func testExecutorKey(t *testing.T, id string) ExecutorKey {
	t.Helper()
	sum := sha256.Sum256([]byte("key|" + id))
	key, err := ParseExecutorKey(base64.RawURLEncoding.EncodeToString(sum[:]))
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// enqueue adds a pending action for this gateway's agent and capability.
func (e *fakeExplorer) enqueue(id, action string, input string, deadline time.Time) {
	e.enqueueFor(id, testAgent, testCapability, action, input, deadline)
}

func (e *fakeExplorer) enqueueFor(id, agent, capability, action, input string, deadline time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.serverNow()
	e.records[id] = &fakeRecord{
		action: Action{
			ID: []byte(id), Version: 1, State: dispatchapi.DispatchQueued, AgentID: []byte(agent),
			Capability: capability, Action: action, Deadline: deadline.UTC(),
			CreatedAt: now.UTC(), UpdatedAt: now.UTC(), CorrelationID: []byte("trace-" + id),
		},
		input: json.RawMessage(input), key: testExecutorKey(e.t, id),
	}
	e.order = append(e.order, id)
}

func (e *fakeExplorer) serverNow() time.Time { return e.clock.Now().Add(-e.skew) }

func (e *fakeExplorer) gateFn() func(context.Context, string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.gate
}

func (e *fakeExplorer) setGate(gate func(context.Context, string) error) {
	e.mu.Lock()
	e.gate = gate
	e.mu.Unlock()
}

func (e *fakeExplorer) reportTimes() []time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]time.Time(nil), e.reportAt...)
}

func (e *fakeExplorer) note(call string) {
	e.calls = append(e.calls, call)
}

func (e *fakeExplorer) callLog() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.calls...)
}

func (e *fakeExplorer) record(id string) Action {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.records[id].action
}

func (e *fakeExplorer) counts() (pulls, claims, extends, completions, reports int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.pulls, len(e.claims), len(e.extends), len(e.completions), len(e.reports)
}

func (e *fakeExplorer) Pull(ctx context.Context, request RequestContext, after string, limit int) (PullPage, error) {
	if gate := e.gateFn(); gate != nil {
		if err := gate(ctx, "pull"); err != nil {
			return PullPage{}, err
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pulls++
	e.note("pull")
	now := e.serverNow()
	page := PullPage{Header: http.Header{}, ReceivedAt: now}
	page.Header.Set("Date", now.UTC().Format(http.TimeFormat))
	for _, id := range e.order {
		record := e.records[id]
		switch {
		case record.action.State == dispatchapi.DispatchQueued,
			record.action.State == dispatchapi.DispatchClaimed && !now.Before(record.leaseUntil):
			offered := record.action
			offered.Input, offered.ExecutorKey = nil, ExecutorKey{}
			page.Actions = append(page.Actions, offered)
		}
	}
	return page, nil
}

func conflict(op string, status int) error {
	kind := DispatchConflict
	if status == http.StatusNotFound {
		kind = DispatchNotFound
	}
	return &DispatchError{Op: op, Kind: kind, Status: status}
}

func (e *fakeExplorer) Claim(ctx context.Context, id []byte, request ClaimRequest) (Action, error) {
	if gate := e.gateFn(); gate != nil {
		if err := gate(ctx, "claim"); err != nil {
			return Action{}, err
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.claims = append(e.claims, request)
	e.note("claim")
	if e.onClaim != nil {
		if action, err, handled := e.onClaim(len(e.claims), id, request); handled {
			return action, err
		}
	}
	record, ok := e.records[string(id)]
	now := e.serverNow()
	if !ok || record.action.Version != request.ExpectedVersion {
		return Action{}, conflict("claim", http.StatusConflict)
	}
	if record.action.State != dispatchapi.DispatchQueued &&
		!(record.action.State == dispatchapi.DispatchClaimed && !now.Before(record.leaseUntil)) {
		return Action{}, conflict("claim", http.StatusNotFound)
	}
	e.fence++
	lease := now.Add(request.Lease)
	if lease.After(record.action.Deadline) {
		lease = record.action.Deadline
	}
	record.action.State, record.action.Version = dispatchapi.DispatchClaimed, record.action.Version+1
	record.action.ClaimID = append([]byte(nil), request.ClaimID...)
	record.action.ClaimFence, record.action.ClaimLeaseUntil = e.fence, lease.UTC()
	record.action.UpdatedAt, record.action.EffectPossible = now.UTC(), true
	record.leaseUntil = lease
	claimed := record.action
	claimed.Input, claimed.ExecutorKey = record.input, record.key
	return claimed, nil
}

func (e *fakeExplorer) Extend(ctx context.Context, id []byte, request ExtendRequest) (Action, error) {
	if gate := e.gateFn(); gate != nil {
		if err := gate(ctx, "extend"); err != nil {
			return Action{}, err
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.extends = append(e.extends, request)
	e.note("extend")
	if e.onExtend != nil {
		if action, err, handled := e.onExtend(len(e.extends), id, request); handled {
			return action, err
		}
	}
	record := e.records[string(id)]
	now := e.serverNow()
	if record == nil || record.action.ClaimFence != request.ClaimFence ||
		record.action.State != dispatchapi.DispatchClaimed || !now.Before(record.leaseUntil) {
		return Action{}, &DispatchError{Op: "extend", Kind: DispatchFenceLost, Status: http.StatusConflict}
	}
	apply := func(now time.Time) {
		lease := now.Add(request.Lease)
		if lease.After(record.action.Deadline) {
			lease = record.action.Deadline
		}
		record.leaseUntil = lease
		record.action.ClaimLeaseUntil, record.action.UpdatedAt = lease.UTC(), now.UTC()
		record.action.Version++
	}
	if e.loseExtendAnswer && e.extendApplyDelay > 0 {
		later := e.clock.After(e.extendApplyDelay)
		go func() {
			at := <-later
			e.mu.Lock()
			apply(at.Add(-e.skew))
			e.mu.Unlock()
		}()
		return Action{}, &DispatchError{Op: "extend", Kind: DispatchIndeterminate}
	}
	apply(now)
	if e.loseExtendAnswer {
		return Action{}, &DispatchError{Op: "extend", Kind: DispatchIndeterminate}
	}
	return record.action, nil
}

func (e *fakeExplorer) Complete(ctx context.Context, id []byte, completion Completion) (Action, error) {
	if gate := e.gateFn(); gate != nil {
		if err := gate(ctx, "complete"); err != nil {
			return Action{}, err
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.completions = append(e.completions, completion)
	e.note("complete")
	if e.onComplete != nil {
		if action, err, handled := e.onComplete(len(e.completions), id, completion); handled {
			return action, err
		}
	}
	record := e.records[string(id)]
	if record == nil || record.action.ClaimFence != completion.ClaimFence ||
		record.action.State != dispatchapi.DispatchClaimed {
		return Action{}, conflict("complete", http.StatusConflict)
	}
	record.action.Version++
	if completion.Failed {
		record.action.State, record.action.ErrorCode = dispatchapi.DispatchFailed, completion.ErrorCode
		record.action.Effected = completion.Effected
	} else {
		record.action.State, record.action.Output = dispatchapi.DispatchSucceeded, completion.Output
	}
	return record.action, nil
}

func (e *fakeExplorer) ReportAmbiguity(ctx context.Context, id []byte, report AmbiguityReport) (Action, error) {
	{
		e.mu.Lock()
		e.reportAt = append(e.reportAt, e.clock.Now())
		e.mu.Unlock()
	}
	if gate := e.gateFn(); gate != nil {
		if err := gate(ctx, "ambiguity"); err != nil {
			return Action{}, err
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.reports = append(e.reports, report)
	e.reportIDs = append(e.reportIDs, append([]byte(nil), id...))
	e.note("ambiguity:" + string(report.Outcome))
	if e.onAmbiguity != nil {
		if action, err, handled := e.onAmbiguity(len(e.reports), id, report); handled {
			return action, err
		}
	}
	record := e.records[string(id)]
	if record == nil {
		return Action{}, &DispatchError{Op: "ambiguity", Kind: DispatchAmbiguityUnrecorded, Status: 404}
	}
	record.action.Version++
	record.action.AmbiguityReports = append(record.action.AmbiguityReports, RecordedAmbiguity{
		ClaimFence: report.ClaimFence, Outcome: report.Outcome,
		Target: report.Target, Reference: report.Reference,
	})
	return record.action, nil
}

func (e *fakeExplorer) PresentAttestation(ctx context.Context, _, _ string) (time.Time, error) {
	if gate := e.gateFn(); gate != nil {
		if err := gate(ctx, "attest"); err != nil {
			return time.Time{}, err
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.attests++
	e.note("attest")
	return e.serverNow().Add(e.attestValidity), nil
}

// fakeTarget is a payment-style API: a request under a new idempotency key
// performs one effect and is answered 200 {"id":"ch_N"}; the same key again
// replays that answer with Idempotent-Replayed: true.
type fakeTarget struct {
	t      *testing.T
	server *httptest.Server

	mu       sync.Mutex
	effects  int
	requests int
	keys     []string
	paths    []string
	cache    map[string]string
	// script answers request n (1-based) instead of the default when it
	// returns true. It runs before any effect.
	script func(n int, w http.ResponseWriter, r *http.Request) bool
	// gate, when set, holds every request after its effect until closed;
	// entered receives once per held request.
	gate    chan struct{}
	entered chan struct{}
	// dropFirst performs the first request's effect and then drops the
	// connection without answering: a lost response.
	dropFirst bool
}

func newFakeTarget(t *testing.T) *fakeTarget {
	target := &fakeTarget{t: t, cache: map[string]string{}, entered: make(chan struct{}, 16)}
	target.server = httptest.NewServer(http.HandlerFunc(target.serve))
	t.Cleanup(target.server.Close)
	return target
}

func (f *fakeTarget) serve(w http.ResponseWriter, r *http.Request) {
	_, _ = io.ReadAll(r.Body)
	key := r.Header.Get("Idempotency-Key")
	f.mu.Lock()
	f.requests++
	n := f.requests
	f.keys = append(f.keys, key)
	f.paths = append(f.paths, r.URL.Path)
	script := f.script
	f.mu.Unlock()
	if script != nil && script(n, w, r) {
		return
	}
	f.mu.Lock()
	cached, seen := f.cache[key]
	if !seen {
		f.effects++
		cached = fmt.Sprintf(`{"id":"ch_%d"}`, f.effects)
		f.cache[key] = cached
	}
	drop := f.dropFirst && n == 1
	gate := f.gate
	f.mu.Unlock()
	if gate != nil {
		f.entered <- struct{}{}
		<-gate
	}
	if drop {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			f.t.Error("target cannot hijack")
			return
		}
		conn, _, err := hijacker.Hijack()
		if err == nil {
			_ = conn.Close()
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if seen {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	_, _ = io.WriteString(w, cached)
}

func (f *fakeTarget) stats() (effects, requests int, keys []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.effects, f.requests, append([]string(nil), f.keys...)
}

const workerRoutes = `[{"action":"charge","method":"POST","path":"/v1/customers/{customer}/charges",` +
	`"effects":["external","egresses-content"],"idempotency":"key","retryable":[503],` +
	`"reference":{"pointer":"/id","pattern":"^ch_[0-9]+$"}},` +
	`{"action":"refund","method":"POST","path":"/v1/refunds","effects":["external","egresses-content"],` +
	`"idempotency":"unprotected"}]`

const chargeInput = `{"path":{"customer":"cus_SECRET42"},"body":{"amount":4200,"memo":"SECRET-MEMO"}}`

// workerHarness wires a worker to the fakes.
type workerHarness struct {
	t        *testing.T
	clock    *timerClock
	explorer *fakeExplorer
	target   *fakeTarget
	dir      string
	logs     *lockedBuffer
	log      *UnrecordedLog
	worker   *Worker
	cfg      WorkerConfig

	cancel context.CancelFunc
	done   chan error
	// credential, when set, is called by TargetAuthorization.
	credential func()
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// events returns the log lines' events, in order.
func (l *lockedBuffer) lines() []map[string]any {
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(l.String()), "\n") {
		if line == "" {
			continue
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(line), &decoded); err == nil {
			out = append(out, decoded)
		}
	}
	return out
}

func (l *lockedBuffer) has(event string) bool {
	for _, line := range l.lines() {
		if line["event"] == event {
			return true
		}
	}
	return false
}

func newWorkerHarness(t *testing.T, mutate func(*WorkerConfig)) *workerHarness {
	t.Helper()
	clock := newTimerClock()
	h := &workerHarness{t: t, clock: clock, explorer: newFakeExplorer(t, clock),
		target: newFakeTarget(t), dir: t.TempDir(), logs: &lockedBuffer{}}
	h.build(mutate)
	return h
}

func (h *workerHarness) build(mutate func(*WorkerConfig)) {
	t := h.t
	t.Helper()
	base, err := url.Parse(h.target.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	routes, err := ParseRoutes([]byte(workerRoutes), remoteEffects)
	if err != nil {
		t.Fatal(err)
	}
	binder, err := NewBinder(base, "Idempotency-Key")
	if err != nil {
		t.Fatal(err)
	}
	policy, err := NewEgressPolicy(base, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewTargetClient(policy, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	if h.log == nil {
		if h.log, err = OpenUnrecordedLog(h.dir, h.clock.Now); err != nil {
			t.Fatal(err)
		}
	}
	h.cfg = WorkerConfig{
		Dispatch: h.explorer, Target: client, Binder: binder, Routes: routes,
		TargetAuthorization: func() (string, string, error) {
			if h.credential != nil {
				h.credential()
			}
			return "Authorization", "Bearer sk_test_SECRETCRED", nil
		},
		AgentID: []byte(testAgent), Capability: testCapability, SurfaceName: testSurface,
		Pod: "gw-0", IdempotencyRetention: 24 * time.Hour,
		ClaimLease: 60 * time.Second, OperationTimeout: 64 * time.Second, Renew: true,
		PlaneTimeout: 5 * time.Second, MaxResponseBytes: 64 << 10, PullLimit: 32,
		PullInterval: time.Second, Unrecorded: h.log,
		Logger: NewLogger(h.logs, h.clock.Now), Clock: h.clock,
	}
	if mutate != nil {
		mutate(&h.cfg)
	}
	if h.worker, err = NewWorker(h.cfg); err != nil {
		t.Fatal(err)
	}
}

func (h *workerHarness) start() {
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel, h.done = cancel, make(chan error, 1)
	worker := h.worker
	go func() { h.done <- worker.Run(ctx) }()
	h.t.Cleanup(func() {
		cancel()
		worker.Kill()
		select {
		case <-h.done:
		case <-time.After(10 * time.Second):
		}
		_ = h.log.Close()
	})
}

// stop drains and waits for Run to return.
func (h *workerHarness) stop() error {
	h.t.Helper()
	h.cancel()
	select {
	case err := <-h.done:
		h.done <- err
		return err
	case <-time.After(10 * time.Second):
		h.t.Fatal("worker did not stop")
	}
	return nil
}

func (h *workerHarness) completion(n int) Completion {
	h.explorer.mu.Lock()
	defer h.explorer.mu.Unlock()
	if len(h.explorer.completions) < n {
		h.t.Fatalf("%d completions, want at least %d", len(h.explorer.completions), n)
	}
	return h.explorer.completions[n-1]
}

func (h *workerHarness) reportsMade() []AmbiguityReport {
	h.explorer.mu.Lock()
	defer h.explorer.mu.Unlock()
	return append([]AmbiguityReport(nil), h.explorer.reports...)
}
