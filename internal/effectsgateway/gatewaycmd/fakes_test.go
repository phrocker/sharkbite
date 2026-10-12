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

package gatewaycmd

import (
	"encoding/base64"
	"encoding/json"
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
)

// The command's tests run Run against a scripted explorer: the dispatch
// routes the worker calls, answering on the wire the client parses. The
// worker loop itself is tested in the package; what is under test here is the
// composition, its order and its exits. The real explorer is in
// cmd/shoal-explore-web/effects_gateway_command_test.go.

const testRoutes = `[{"action":"status","method":"POST","path":"/v1/status",` +
	`"effects":["external"],"idempotency":"key","reference":{"pointer":"/id","pattern":"^ch_[0-9]+$"}}]`

var testAgent = []byte("stripe-agent")

type fakeExplorer struct {
	t      *testing.T
	server *httptest.Server

	mu            sync.Mutex
	paths         []string
	resolveStatus int
	executorRef   string
	capabilities  []dispatchapi.Capability
	// offer is the one action the pull page offers until it is claimed.
	offer     *offeredAction
	claimed   bool
	completed []json.RawMessage
}

type offeredAction struct {
	id       []byte
	deadline time.Time
	created  time.Time
}

func newFakeExplorer(t *testing.T, target *url.URL) *fakeExplorer {
	t.Helper()
	table, err := effectsgateway.ParseRoutes([]byte(testRoutes), effectsgateway.DerivedEffects(target))
	if err != nil {
		t.Fatal(err)
	}
	route, _ := table.Lookup("status")
	explorer := &fakeExplorer{
		t: t, executorRef: "stripe",
		capabilities: []dispatchapi.Capability{{
			Name: effectsgateway.DefaultCapability,
			Actions: []dispatchapi.DescriptorAction{{
				Name: "status", Effects: route.Effects(),
				InputSchema: route.InputSchema(), OutputSchema: effectsgateway.OutputSchema(),
			}},
		}},
	}
	explorer.server = httptest.NewServer(http.HandlerFunc(explorer.serve))
	t.Cleanup(explorer.server.Close)
	return explorer
}

func (f *fakeExplorer) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.paths...)
}

func (f *fakeExplorer) completions() []json.RawMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]json.RawMessage(nil), f.completed...)
}

func (f *fakeExplorer) offerAction(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now().UTC()
	f.offer = &offeredAction{id: []byte(id), created: now, deadline: now.Add(time.Hour)}
}

func b64(raw []byte) string { return base64.RawURLEncoding.EncodeToString(raw) }

func (f *fakeExplorer) actionWire(offer *offeredAction, version uint64, state dispatchapi.DispatchState) map[string]any {
	return map[string]any{
		"id": b64(offer.id), "version": version, "state": state,
		"agent_id": b64(testAgent), "agent_generation": 1,
		"capability": effectsgateway.DefaultCapability, "action": "status",
		"deadline": offer.deadline, "created_at": offer.created,
		"updated_at": time.Now().UTC(), "effect_possible": false,
		"correlation_id": b64([]byte("trace-" + string(offer.id))),
	}
}

func (f *fakeExplorer) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.paths = append(f.paths, r.URL.Path)
	f.mu.Unlock()
	var body map[string]json.RawMessage
	raw, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(raw, &body)
	reply := func(status int, value any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(value)
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/resolve"):
		f.mu.Lock()
		status, ref, capabilities := f.resolveStatus, f.executorRef, f.capabilities
		f.mu.Unlock()
		if status != 0 {
			reply(status, map[string]any{"code": "forbidden"})
			return
		}
		reply(http.StatusOK, map[string]any{
			"id": b64(testAgent), "generation": 1, "executor_ref": ref,
			"capabilities": capabilities,
		})
	case r.URL.Path == "/api/v1/fleet/actions/pull":
		f.mu.Lock()
		actions := []any{}
		if f.offer != nil && !f.claimed {
			actions = append(actions, f.actionWire(f.offer, 1, dispatchapi.DispatchQueued))
		}
		f.mu.Unlock()
		reply(http.StatusOK, map[string]any{"actions": actions})
	case strings.HasSuffix(r.URL.Path, "/claim"):
		var claimID string
		var lease time.Duration
		_ = json.Unmarshal(body["claim_id"], &claimID)
		_ = json.Unmarshal(body["lease"], &lease)
		f.mu.Lock()
		offer := f.offer
		f.claimed = true
		f.mu.Unlock()
		record := f.actionWire(offer, 2, dispatchapi.DispatchClaimed)
		now := time.Now().UTC()
		record["updated_at"] = now
		record["claim_id"] = claimID
		record["claim_fence"] = 1
		record["claim_lease_until"] = now.Add(lease)
		record["input"] = json.RawMessage(`{}`)
		record["executor_key"] = b64([]byte(strings.Repeat("k", 32)))
		record["effect_possible"] = true
		reply(http.StatusOK, record)
	case strings.HasSuffix(r.URL.Path, "/complete"):
		f.mu.Lock()
		offer := f.offer
		f.completed = append(f.completed, raw)
		f.mu.Unlock()
		var expected uint64
		var claimID, errorCode string
		var failed bool
		_ = json.Unmarshal(body["expected_version"], &expected)
		_ = json.Unmarshal(body["claim_id"], &claimID)
		_ = json.Unmarshal(body["error_code"], &errorCode)
		_ = json.Unmarshal(body["failed"], &failed)
		state := dispatchapi.DispatchSucceeded
		if failed {
			state = dispatchapi.DispatchFailed
		}
		record := f.actionWire(offer, expected+1, state)
		record["claim_id"] = claimID
		record["claim_fence"] = 1
		if failed {
			record["error_code"] = errorCode
		} else {
			record["output"] = body["output"]
		}
		reply(http.StatusOK, record)
	default:
		reply(http.StatusNotFound, map[string]any{"code": "not_found"})
	}
}

// fakeTarget is the operational surface: one effect per request, and
// optionally holding each request until released or cancelled.
type fakeTarget struct {
	server  *httptest.Server
	mu      sync.Mutex
	hits    int
	hold    bool
	entered chan struct{}
	release chan struct{}
}

func newFakeTarget(t *testing.T) *fakeTarget {
	target := &fakeTarget{entered: make(chan struct{}, 8), release: make(chan struct{})}
	target.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		target.mu.Lock()
		target.hits++
		hold := target.hold
		target.mu.Unlock()
		if hold {
			target.entered <- struct{}{}
			select {
			case <-target.release:
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"ch_1"}`)
	}))
	t.Cleanup(target.server.Close)
	t.Cleanup(func() {
		select {
		case <-target.release:
		default:
			close(target.release)
		}
	})
	return target
}

func (f *fakeTarget) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits
}

type world struct {
	t        *testing.T
	explorer *fakeExplorer
	target   *fakeTarget
	dir      string
	token    string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	target := newFakeTarget(t)
	targetURL, err := url.Parse(target.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("executor-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &world{
		t: t, explorer: newFakeExplorer(t, targetURL), target: target,
		dir: filepath.Join(t.TempDir(), "unrecorded"), token: token,
	}
}

// args is a complete, valid run command line for this world, with changes
// applied ("<unset>" removes a flag).
func (w *world) args(changes map[string]string) []string {
	values := map[string]string{
		"dispatch-url":          w.explorer.server.URL,
		"dispatch-token-file":   w.token,
		"agent-id":              b64(testAgent),
		"executor-ref":          "stripe",
		"surface-name":          "api.stripe.test",
		"target-base-url":       w.target.server.URL,
		"target-allow-private":  "true",
		"target-credential-env": "SHOAL_GATEWAY_TEST_UNSET_CREDENTIAL",
		"routes":                testRoutes,
		"idempotency-header":    "Idempotency-Key",
		"idempotency-retention": "48h",
		"unrecorded-dir":        w.dir,
		"pod-name":              "gw-0",
		"health-address":        "127.0.0.1:0",
		"pull-interval":         "100ms",
	}
	for name, value := range changes {
		if value == "<unset>" {
			delete(values, name)
			continue
		}
		values[name] = value
	}
	args := make([]string, 0, len(values))
	for name, value := range values {
		args = append(args, "-"+name+"="+value)
	}
	return args
}

// running is one in-process run of the command.
type running struct {
	t       *testing.T
	signals chan os.Signal
	code    chan int
	health  string
	stdout  *syncBuffer
	stderr  *syncBuffer
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

// start runs the command and waits until the worker is about to run, or the
// command has exited.
func start(t *testing.T, args []string, clock effectsgateway.WorkerClock) *running {
	t.Helper()
	run := &running{
		t: t, signals: make(chan os.Signal, 2), code: make(chan int, 1),
		stdout: &syncBuffer{}, stderr: &syncBuffer{},
	}
	started := make(chan string, 1)
	go func() {
		run.code <- Main(append([]string{"run"}, args...), Env{
			Stdout: run.stdout, Stderr: run.stderr, Signals: run.signals, Clock: clock,
			Started: func(address string) { started <- address },
		})
	}()
	select {
	case run.health = <-started:
	case code := <-run.code:
		run.code <- code
	case <-time.After(20 * time.Second):
		t.Fatal("the command neither started nor exited")
	}
	t.Cleanup(func() {
		select {
		case run.signals <- os.Interrupt:
		default:
		}
		select {
		case run.signals <- os.Interrupt:
		default:
		}
		select {
		case code := <-run.code:
			run.code <- code
		case <-time.After(20 * time.Second):
			t.Error("the command did not stop at cleanup")
		}
	})
	return run
}

// exit waits up to bound for the command's exit code.
func (r *running) exit(bound time.Duration) (int, bool) {
	select {
	case code := <-r.code:
		r.code <- code
		return code, true
	case <-time.After(bound):
		return 0, false
	}
}

func (r *running) get(path string) (int, string) {
	r.t.Helper()
	response, err := http.Get("http://" + r.health + path)
	if err != nil {
		r.t.Fatalf("GET %s: %v", path, err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(body)
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

// manualClock is a worker clock a test advances by hand. It records every
// timer's duration so a test can wait for the one it means to fire.
type manualClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []manualWaiter
	asked   []time.Duration
}

type manualWaiter struct {
	at time.Time
	d  time.Duration
	ch chan time.Time
}

func newManualClock() *manualClock { return &manualClock{now: time.Now()} }

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asked = append(c.asked, d)
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, manualWaiter{at: c.now.Add(d), d: d, ch: ch})
	return ch
}

// Fire releases only the timers asked for exactly d, leaving every other
// timer pending and the clock's reading where it is. A test uses it to pass
// one deadline (the drain's bound) without also passing the shorter ones a
// plain Advance would fire with it, which would race the deadline under test.
func (c *manualClock) Fire(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	kept := c.waiters[:0]
	for _, waiter := range c.waiters {
		if waiter.d == d {
			waiter.ch <- waiter.at
			continue
		}
		kept = append(kept, waiter)
	}
	c.waiters = kept
}

func (c *manualClock) Advance(d time.Duration) {
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

func (c *manualClock) wasAsked(d time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, asked := range c.asked {
		if asked == d {
			return true
		}
	}
	return false
}
