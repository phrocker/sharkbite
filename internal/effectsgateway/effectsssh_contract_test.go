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

// The gateway's half of the effects-ssh contract (#450).
//
// extensions/effects-ssh is an HTTP target of this gateway. Its answers and
// the route that classifies them are pinned as fixtures in that extension's
// testdata/contract; the extension's own tests assert it writes exactly those
// bytes, and these assert the gateway, configured with exactly that route,
// classifies each one as the extension intends. The extension cannot import
// this package (it is internal), and this package must not import the
// extension (the import boundary), so the fixtures are files both read.
//
// The property the pair exists for: an answer meaning "this command may have
// run and nothing says how it ended" is never a definite failure and never
// sent again, and an answer meaning "it ran" is never sent again either.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	dispatchapi "github.com/phrocker/shoal-oss/pkg/dispatch/api"
)

var effectsSSHContract = filepath.Join("..", "..", "extensions", "effects-ssh", "testdata", "contract")

type sshAnswerFixture struct {
	Name    string            `json:"name"`
	Status  int               `json:"status"`
	Header  map[string]string `json:"header"`
	Body    string            `json:"body"`
	Gateway struct {
		Outcome           string          `json:"outcome"`
		Kind              string          `json:"kind"`
		Status            int             `json:"status"`
		ErrorCode         string          `json:"error_code"`
		Output            json.RawMessage `json:"output"`
		RetryAfterSeconds int             `json:"retry_after_seconds"`
	} `json:"gateway"`
}

func readSSHContract(t *testing.T) (*RouteTable, *Route, map[string]sshAnswerFixture) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(effectsSSHContract, "route.json"))
	if err != nil {
		t.Fatalf("the effects-ssh route fixture is missing: %v", err)
	}
	loopback, _ := url.Parse("http://127.0.0.1:8443")
	table, err := ParseRoutes(raw, DerivedEffects(loopback))
	if err != nil {
		t.Fatalf("the shipped effects-ssh route does not parse: %v", err)
	}
	actions := table.Actions()
	if len(actions) != 1 {
		t.Fatalf("route fixture declares %d routes", len(actions))
	}
	route, _ := table.Lookup(actions[0])
	answersRaw, err := os.ReadFile(filepath.Join(effectsSSHContract, "answers.json"))
	if err != nil {
		t.Fatal(err)
	}
	var list []sshAnswerFixture
	if err := json.Unmarshal(answersRaw, &list); err != nil {
		t.Fatal(err)
	}
	answers := map[string]sshAnswerFixture{}
	for _, answer := range list {
		answers[answer.Name] = answer
	}
	return table, route, answers
}

func (f sshAnswerFixture) response() *Response {
	header := http.Header{}
	for name, value := range f.Header {
		header.Set(name, value)
	}
	return &Response{Status: f.Status, Header: header, Body: []byte(f.Body)}
}

// TestEffectsSSHRouteIsAKeyRoute: the shipped route is a key route on a
// literal path, so the agent's input chooses nothing, and it declares the
// rules the 409 and 503 answers need.
func TestEffectsSSHRouteIsAKeyRoute(t *testing.T) {
	table, route, _ := readSSHContract(t)
	if !table.RequiresKey() || route.Idempotency() != IdempotencyKey || route.Method() != http.MethodPost {
		t.Fatalf("route %s %s idempotency %s", route.Method(), route.PathTemplate(), route.Idempotency())
	}
	if len(route.params) != 0 || len(route.query) != 0 {
		t.Fatal("the route takes path or query input; the command must be fixed by the route")
	}
	if !strings.HasPrefix(route.PathTemplate(), "/v1/commands/") {
		t.Fatalf("path %s", route.PathTemplate())
	}
	if !reflect.DeepEqual(route.Effects(), dispatchapi.Effects{dispatchapi.EffectMutatesExternal}) {
		t.Fatalf("effects %v", route.Effects())
	}
	// The action registers this input schema; VerifyDescriptor requires it.
	var schema map[string]any
	_ = json.Unmarshal(route.InputSchema(), &schema)
	if err := VerifyDescriptor(table, []DescriptorAction{{
		Name: route.Action(), Effects: route.Effects(),
		InputSchema: route.InputSchema(), OutputSchema: OutputSchema(),
	}}); err != nil {
		t.Fatal(err)
	}
}

// TestEffectsSSHAnswersClassifyAsPinned: every answer the extension can give,
// classified by the shipped route, is what the fixture says.
func TestEffectsSSHAnswersClassifyAsPinned(t *testing.T) {
	_, route, answers := readSSHContract(t)
	if len(answers) < 20 {
		t.Fatalf("only %d answers pinned", len(answers))
	}
	for name, fixture := range answers {
		got := Classify(route, Observation{Written: true, Response: fixture.response()})
		want := fixture.Gateway
		if got.Outcome.String() != want.Outcome || string(got.Kind) != want.Kind ||
			got.Status != want.Status || got.ErrorCode != want.ErrorCode {
			t.Errorf("%s: classified %s/%s/%d/%q, fixture says %s/%s/%d/%q", name,
				got.Outcome, got.Kind, got.Status, got.ErrorCode,
				want.Outcome, want.Kind, want.Status, want.ErrorCode)
			continue
		}
		if want.Output != nil || got.Output != nil {
			var gotOutput, wantOutput any
			_ = json.Unmarshal(got.Output, &gotOutput)
			_ = json.Unmarshal(want.Output, &wantOutput)
			if !reflect.DeepEqual(gotOutput, wantOutput) {
				t.Errorf("%s: output %s, fixture says %s", name, got.Output, want.Output)
			}
		}
		if wantRetry := time.Duration(want.RetryAfterSeconds) * time.Second; wantRetry > 0 &&
			(!got.HasRetryAfter || got.RetryAfter != wantRetry) {
			t.Errorf("%s: retry after %v, fixture says %v", name, got.RetryAfter, wantRetry)
		}
	}
}

// TestEffectsSSHOutcomeUnknownIsNeverAFailureOrARetry is the property the
// contract exists for, stated over every 409 body and every answer: a 409 is
// outcome_unknown unless its body is exactly the done marker, and nothing the
// extension answers for a command that may have run is a target rejection.
func TestEffectsSSHOutcomeUnknownIsNeverAFailureOrARetry(t *testing.T) {
	_, route, answers := readSSHContract(t)
	for _, body := range []string{
		`{"state":"outcome_unknown"}`, `{"state":"in_progress"}`, `{}`, ``, `not json`,
		`{"state":"COMPLETED"}`, `{"state":["completed"]}`, strings.Repeat("x", 100),
	} {
		got := Classify(route, Observation{Written: true, Response: &Response{
			Status: http.StatusConflict, Header: http.Header{}, Body: []byte(body)}})
		if got.Outcome != OutcomeFailed || got.ErrorCode != ErrorOutcomeUnknown {
			t.Errorf("409 %q classified %s/%s", body, got.Outcome, got.ErrorCode)
		}
	}
	for _, name := range []string{"outcome_unknown", "outcome_unknown_replayed", "key_conflict"} {
		got := Classify(route, Observation{Written: true, Response: answers[name].response()})
		if got.Outcome != OutcomeFailed || got.ErrorCode != ErrorOutcomeUnknown {
			t.Fatalf("%s classified %s/%s", name, got.Outcome, got.ErrorCode)
		}
	}
	// A body cut off by the response bound is not the marker either.
	got := Classify(route, Observation{Written: true, Response: &Response{
		Status: http.StatusConflict, Header: http.Header{}, Body: []byte(`{"state":"comp`), Oversize: true}})
	if got.ErrorCode != ErrorOutcomeUnknown {
		t.Fatalf("oversize 409 classified %s", got.ErrorCode)
	}
}

// sshTarget models the extension's journal behind the pinned answers: a new
// key runs once (effects counts it) and answers completed; a repeat of a
// finished key answers completed_replayed; a key the script marks unknown
// answers outcome_unknown. script(n), when it returns a name, answers request
// n with that fixture instead; "drop" runs the command and loses the answer.
type sshTarget struct {
	t       *testing.T
	server  *httptest.Server
	answers map[string]sshAnswerFixture

	mu       sync.Mutex
	requests int
	effects  int
	journal  map[string]string
	seen     []*http.Request
	bodies   [][]byte
	script   func(n int) string
}

func newSSHTarget(t *testing.T, answers map[string]sshAnswerFixture) *sshTarget {
	target := &sshTarget{t: t, answers: answers, journal: map[string]string{}}
	target.server = httptest.NewServer(http.HandlerFunc(target.serve))
	t.Cleanup(target.server.Close)
	return target
}

func (s *sshTarget) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	key := r.Header.Get("Idempotency-Key")
	s.mu.Lock()
	s.requests++
	n := s.requests
	s.seen = append(s.seen, r.Clone(context.Background()))
	s.bodies = append(s.bodies, body)
	name := ""
	if s.script != nil {
		name = s.script(n)
	}
	if name == "" || name == "drop" {
		switch s.journal[key] {
		case "":
			s.journal[key] = "finished"
			s.effects++
			if name == "" {
				name = "completed"
			}
		case "finished":
			name = "completed_replayed"
		case "unknown":
			name = "outcome_unknown"
		}
	}
	s.mu.Unlock()
	if name == "drop" {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
		return
	}
	fixture, ok := s.answers[name]
	if !ok {
		s.t.Errorf("no fixture %q", name)
		return
	}
	for header, value := range fixture.Header {
		w.Header().Set(header, value)
	}
	w.WriteHeader(fixture.Status)
	_, _ = io.WriteString(w, fixture.Body)
}

func (s *sshTarget) stats() (requests, effects int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests, s.effects
}

func (s *sshTarget) markUnknown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, state := range s.journal {
		if state == "finished" {
			s.journal[key] = "unknown"
		}
	}
}

type sshHarness struct {
	t        *testing.T
	clock    *timerClock
	explorer *fakeExplorer
	target   *sshTarget
	route    *Route
	worker   *Worker
	log      *UnrecordedLog
	cancel   context.CancelFunc
	done     chan error
}

const sshCapability = "effects.ssh"

func newSSHHarness(t *testing.T, clock *timerClock, explorer *fakeExplorer, target *sshTarget) *sshHarness {
	t.Helper()
	table, route, _ := readSSHContract(t)
	base, _ := url.Parse(target.server.URL)
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
	log, err := OpenUnrecordedLog(t.TempDir(), clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := NewWorker(WorkerConfig{
		Dispatch: explorer, Target: client, Binder: binder, Routes: table,
		TargetAuthorization: func() (string, string, error) {
			return "Authorization", "Bearer effects-ssh-caller-token-0123456789abcdef", nil
		},
		AgentID: []byte(testAgent), Capability: sshCapability, SurfaceName: "web-fleet",
		Pod: "gw-0", IdempotencyRetention: 24 * time.Hour,
		ClaimLease: 60 * time.Second, OperationTimeout: 64 * time.Second, Renew: true,
		PlaneTimeout: 5 * time.Second, MaxResponseBytes: 64 << 10, PullLimit: 32,
		PullInterval: time.Second, Unrecorded: log,
		Logger: NewLogger(io.Discard, clock.Now), Clock: clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	h := &sshHarness{t: t, clock: clock, explorer: explorer, target: target, route: route,
		worker: worker, log: log}
	t.Cleanup(func() {
		if h.cancel != nil {
			h.cancel()
			worker.Kill()
			select {
			case <-h.done:
			case <-time.After(10 * time.Second):
			}
		}
		_ = log.Close()
	})
	return h
}

// start runs the worker. Work is enqueued first: the worker's first pull
// then finds it, rather than waiting on a pull backoff the hand-advanced
// clock would have to release.
func (h *sshHarness) start() {
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel, h.done = cancel, make(chan error, 1)
	worker := h.worker
	go func() { h.done <- worker.Run(ctx) }()
}

// enqueue adds an action and starts the worker.
func (h *sshHarness) enqueue(id string) {
	h.explorer.enqueueFor(id, testAgent, sshCapability, h.route.Action(), `{}`, workerEpoch.Add(time.Hour))
	h.start()
}

// until advances the clock in steps until cond holds.
func (h *sshHarness) until(what string, cond func() bool) {
	h.t.Helper()
	for i := 0; i < 400 && !cond(); i++ {
		settle()
		h.clock.Advance(time.Second)
	}
	eventually(h.t, what, cond)
}

func newSSHScenario(t *testing.T, script func(int) string) (*sshHarness, map[string]sshAnswerFixture) {
	_, _, answers := readSSHContract(t)
	clock := newTimerClock()
	explorer := newFakeExplorer(t, clock)
	target := newSSHTarget(t, answers)
	target.script = script
	return newSSHHarness(t, clock, explorer, target), answers
}

func outputOf(t *testing.T, completion Completion) map[string]any {
	t.Helper()
	var output map[string]any
	if err := json.Unmarshal(completion.Output, &output); err != nil {
		t.Fatalf("output %s: %v", completion.Output, err)
	}
	return output
}

func lastCompletion(h *sshHarness) Completion {
	h.explorer.mu.Lock()
	defer h.explorer.mu.Unlock()
	if len(h.explorer.completions) == 0 {
		h.t.Fatal("no completion")
	}
	return h.explorer.completions[len(h.explorer.completions)-1]
}

// TestEffectsSSHEndToEndCompleted: the real worker sends the bound request —
// POST to the literal command path, no body, the executor key, the caller
// credential — and records the exit status the extension reports.
func TestEffectsSSHEndToEndCompleted(t *testing.T) {
	h, _ := newSSHScenario(t, nil)
	h.enqueue("a1")
	eventually(t, "the action to succeed", func() bool {
		return h.explorer.record("a1").State == dispatchapi.DispatchSucceeded
	})
	output := outputOf(t, lastCompletion(h))
	if output["reference"] != "exit-0" || output["idempotency"] != "key" || output["status"] != float64(200) {
		t.Fatalf("output %v", output)
	}
	requests, effects := h.target.stats()
	if requests != 1 || effects != 1 {
		t.Fatalf("requests %d effects %d", requests, effects)
	}
	h.target.mu.Lock()
	request, body := h.target.seen[0], h.target.bodies[0]
	h.target.mu.Unlock()
	if request.Method != http.MethodPost || request.URL.Path != h.route.PathTemplate() ||
		request.URL.RawQuery != "" || len(body) != 0 || request.Header.Get("Content-Type") != "" {
		t.Fatalf("request %s %s?%s body %q", request.Method, request.URL.Path, request.URL.RawQuery, body)
	}
	if request.Header.Get("Idempotency-Key") != testExecutorKey(t, "a1").HeaderValue() ||
		request.Header.Get("Authorization") != "Bearer effects-ssh-caller-token-0123456789abcdef" {
		t.Fatal("the request lacks the executor key or the caller credential")
	}
}

// TestEffectsSSHEndToEndLostAnswerIsReplayed: the command ran and its answer
// was lost; the resend under the same key is a replay, and it ran once.
func TestEffectsSSHEndToEndLostAnswerIsReplayed(t *testing.T) {
	h, _ := newSSHScenario(t, func(n int) string {
		if n == 1 {
			return "drop"
		}
		return ""
	})
	h.enqueue("a1")
	h.until("the action to succeed", func() bool {
		return h.explorer.record("a1").State == dispatchapi.DispatchSucceeded
	})
	if output := outputOf(t, lastCompletion(h)); output["idempotency"] != "replayed" || output["reference"] != "exit-0" {
		t.Fatalf("output %v", output)
	}
	if requests, effects := h.target.stats(); requests != 2 || effects != 1 {
		t.Fatalf("requests %d effects %d", requests, effects)
	}
}

// TestEffectsSSHEndToEndOutcomeUnknownIsFinal: the answer was lost and the
// extension restarted before the command finished; the resend answers
// outcome_unknown, which is recorded as such — effect possible, not a
// rejection — and is not sent a third time.
func TestEffectsSSHEndToEndOutcomeUnknownIsFinal(t *testing.T) {
	var h *sshHarness
	h, _ = newSSHScenario(t, func(n int) string {
		if n == 1 {
			return "drop"
		}
		return ""
	})
	h.enqueue("a1")
	eventually(t, "the first request", func() bool { requests, _ := h.target.stats(); return requests == 1 })
	h.target.markUnknown()
	h.until("the action to fail", func() bool {
		return h.explorer.record("a1").State == dispatchapi.DispatchFailed
	})
	completion := lastCompletion(h)
	if completion.ErrorCode != ErrorOutcomeUnknown || !h.explorer.record("a1").EffectPossible {
		t.Fatalf("completion %+v", completion)
	}
	for i := 0; i < 20; i++ {
		settle()
		h.clock.Advance(5 * time.Second)
	}
	if requests, effects := h.target.stats(); requests != 2 || effects != 1 {
		t.Fatalf("requests %d effects %d", requests, effects)
	}
}

// TestEffectsSSHEndToEndInProgressThenDone: a resend that finds the command
// still running is told to retry; the next resend is the replay.
func TestEffectsSSHEndToEndInProgressThenDone(t *testing.T) {
	h, _ := newSSHScenario(t, func(n int) string {
		switch n {
		case 1:
			return "drop"
		case 2:
			return "in_progress"
		}
		return ""
	})
	h.enqueue("a1")
	h.until("the action to succeed", func() bool {
		return h.explorer.record("a1").State == dispatchapi.DispatchSucceeded
	})
	if requests, effects := h.target.stats(); requests != 3 || effects != 1 {
		t.Fatalf("requests %d effects %d", requests, effects)
	}
}

// TestEffectsSSHEndToEndForeverInProgressIsRetryExhausted: a command that
// outlives every attempt ends as retry_exhausted, which says the request was
// written; it is not a rejection.
func TestEffectsSSHEndToEndForeverInProgressIsRetryExhausted(t *testing.T) {
	h, _ := newSSHScenario(t, func(n int) string {
		if n == 1 {
			return "drop"
		}
		return "in_progress"
	})
	h.enqueue("a1")
	h.until("the action to fail", func() bool {
		return h.explorer.record("a1").State == dispatchapi.DispatchFailed
	})
	if code := lastCompletion(h).ErrorCode; code != ErrorRetryExhausted {
		t.Fatalf("error code %s", code)
	}
	if requests, _ := h.target.stats(); requests != DefaultMaxSendAttempts {
		t.Fatalf("requests %d", requests)
	}
}

// TestEffectsSSHEndToEndRefusalsAreRecordedOnce: answers given before
// anything ran are definite failures, sent once.
func TestEffectsSSHEndToEndRefusalsAreRecordedOnce(t *testing.T) {
	for name, code := range map[string]string{
		"not_run":                     "target_rejected_502",
		"refused_unknown_command":     "target_rejected_404",
		"refused_unauthorized":        "target_rejected_401",
		"refused_journal_full":        "target_rejected_507",
		"refused_body_not_allowed":    "target_rejected_400",
		"refused_journal_unavailable": "target_rejected_500",
	} {
		t.Run(name, func(t *testing.T) {
			h, _ := newSSHScenario(t, func(int) string { return name })
			h.enqueue("a1")
			eventually(t, "the action to fail", func() bool {
				return h.explorer.record("a1").State == dispatchapi.DispatchFailed
			})
			if got := lastCompletion(h).ErrorCode; got != code {
				t.Fatalf("error code %s, want %s", got, code)
			}
			if requests, _ := h.target.stats(); requests != 1 {
				t.Fatalf("requests %d", requests)
			}
		})
	}
}

// TestEffectsSSHEndToEndBusyIsRetried: busy is answered before anything is
// journaled, so the retry is a first attempt.
func TestEffectsSSHEndToEndBusyIsRetried(t *testing.T) {
	h, _ := newSSHScenario(t, func(n int) string {
		if n == 1 {
			return "busy"
		}
		return ""
	})
	h.enqueue("a1")
	h.until("the action to succeed", func() bool {
		return h.explorer.record("a1").State == dispatchapi.DispatchSucceeded
	})
	if output := outputOf(t, lastCompletion(h)); output["idempotency"] != "key" {
		t.Fatalf("output %v", output)
	}
	if requests, effects := h.target.stats(); requests != 2 || effects != 1 {
		t.Fatalf("requests %d effects %d", requests, effects)
	}
}

// TestEffectsSSHEndToEndWorkerRestart: the worker dies with the request
// written; a new worker re-claims the action after the lease and resends the
// same key; the extension replays, and the command ran once.
func TestEffectsSSHEndToEndWorkerRestart(t *testing.T) {
	_, _, answers := readSSHContract(t)
	clock := newTimerClock()
	explorer := newFakeExplorer(t, clock)
	target := newSSHTarget(t, answers)
	target.script = func(n int) string {
		if n == 1 {
			return "drop"
		}
		return ""
	}
	first := newSSHHarness(t, clock, explorer, target)
	first.enqueue("a1")
	eventually(t, "the first request", func() bool { requests, _ := target.stats(); return requests == 1 })
	first.worker.Kill()
	select {
	case <-first.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the first worker did not stop")
	}
	second := newSSHHarness(t, clock, explorer, target)
	second.start()
	second.until("the action to succeed", func() bool {
		return explorer.record("a1").State == dispatchapi.DispatchSucceeded
	})
	requests, effects := target.stats()
	if effects != 1 || requests < 2 {
		t.Fatalf("requests %d effects %d", requests, effects)
	}
	var keys []string
	target.mu.Lock()
	for _, request := range target.seen {
		keys = append(keys, request.Header.Get("Idempotency-Key"))
	}
	target.mu.Unlock()
	for _, key := range keys {
		if key != keys[0] {
			t.Fatalf("keys differ across workers: %q", keys)
		}
	}
	if output := outputOf(t, lastCompletion(second)); output["idempotency"] != "replayed" {
		t.Fatalf("output %v", output)
	}
}

// TestEffectsSSHRouteWithoutItsRulesIsUnsafe is the mutation check on the
// route: drop the conflict rule and outcome_unknown becomes a definite
// rejection; drop 503 from retryable and in_progress becomes one too. The
// shipped route carries both, which the tests above rely on.
func TestEffectsSSHRouteWithoutItsRulesIsUnsafe(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(effectsSSHContract, "route.json"))
	if err != nil {
		t.Fatal(err)
	}
	_, _, answers := readSSHContract(t)
	loopback, _ := url.Parse("http://127.0.0.1:8443")
	for _, mutation := range []struct {
		drop, answer string
	}{{`"conflict"`, "outcome_unknown"}, {`"retryable"`, "in_progress"}} {
		var routes []map[string]json.RawMessage
		_ = json.Unmarshal(raw, &routes)
		delete(routes[0], strings.Trim(mutation.drop, `"`))
		mutated, _ := json.Marshal(routes)
		table, err := ParseRoutes(mutated, DerivedEffects(loopback))
		if err != nil {
			t.Fatal(err)
		}
		route, _ := table.Lookup(table.Actions()[0])
		got := Classify(route, Observation{Written: true, Response: answers[mutation.answer].response()})
		if !strings.HasPrefix(got.ErrorCode, "target_rejected_") {
			t.Fatalf("without %s, %s classified %s/%s; the mutation check is vacuous",
				mutation.drop, mutation.answer, got.Outcome, got.ErrorCode)
		}
	}
	if !bytes.Contains(raw, []byte(`"conflict"`)) || !bytes.Contains(raw, []byte(`"retryable"`)) {
		t.Fatal("the shipped route lost a rule")
	}
}
