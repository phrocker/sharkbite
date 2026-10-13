// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package main

// The effects gateway end to end (#391, PR8): the real shoal-gateway binary,
// built by this test and run as a child process, against the explorer
// in-process — the real webapi handlers over the durable embedded stores, the
// real OIDC authenticator and executor mint, served on loopback — and a
// payment-style target that performs one effect per idempotency key.
//
// Unlike the in-process worker and command tests beside it, nothing here
// shares a clock or a goroutine with the gateway: it runs on the system
// clock, its signals are real signals, a crash is a real SIGKILL, and its
// lock is a real flock held by another process. The explorer's clock follows
// the system clock (followSystemClock), and its responses carry its Date, as
// a real explorer's do.
//
// Every scenario asserts the effects the target performed and the state of
// the record the explorer holds. The lease and timeouts are short but within
// the binary's validated bounds: L = 6s with renewal, plane timeout L/4, and
// T = 2s. Waits poll with deadlines; nothing sleeps for a fixed time to let
// something happen.

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/effectsgateway"
	"github.com/phrocker/shoal-oss/internal/effectsgateway/gatewaycmd"
	"github.com/phrocker/shoal-oss/internal/executorattest"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
)

// Under -race this package is long enough to approach go test's default
// timeout run serially (#710), so most of its tests call t.Parallel(). Go runs
// the top-level parallel tests together only after every serial one has
// returned. That keeps two kinds of test serial, and correct as such:
//
//   - a test that changes process state: t.Setenv, or a package hook such as
//     listenTCP or lookupListenHost, since a parallel test would observe it;
//   - a test whose pass depends on wall-clock timing: the real-time worker
//     tests in effects_gateway_worker_test.go and the gateway command test,
//     which sleep to give a worker time to act, the binary end to end below,
//     with its real leases, and the tests that start run() and wait a fixed
//     time for it to listen.
//
// Every test made parallel owns its directories, its clock, its stores and
// its servers, and none of the process state it reads changes while the
// parallel tests run.
func TestMain(m *testing.M) {
	code := m.Run()
	if gatewayBinary.dir != "" {
		_ = os.RemoveAll(gatewayBinary.dir)
	}
	os.Exit(code)
}

// gatewayBinary is cmd/shoal-gateway, built once per test binary.
var gatewayBinary struct {
	once sync.Once
	dir  string
	path string
	err  error
}

// buildGatewayBinary builds cmd/shoal-gateway with this test binary's race
// setting, so a race in the gateway fails the scenario that provoked it.
func buildGatewayBinary(t *testing.T) string {
	t.Helper()
	gatewayBinary.once.Do(func() {
		goTool, err := exec.LookPath("go")
		if err != nil {
			goTool = filepath.Join(runtime.GOROOT(), "bin", "go")
		}
		dir, err := os.MkdirTemp("", "shoal-gateway-e2e-")
		if err != nil {
			gatewayBinary.err = err
			return
		}
		gatewayBinary.dir = dir
		path := filepath.Join(dir, "shoal-gateway")
		// No VCS stamping: the build must not depend on the checkout.
		args := []string{"build", "-buildvcs=false", "-o", path}
		if raceBuild {
			args = append(args, "-race")
		}
		args = append(args, "github.com/phrocker/shoal-oss/cmd/shoal-gateway")
		build := exec.Command(goTool, args...)
		if output, err := build.CombinedOutput(); err != nil {
			gatewayBinary.err = fmt.Errorf("go build: %v\n%s", err, output)
			return
		}
		gatewayBinary.path = path
	})
	if gatewayBinary.err != nil {
		t.Fatal(gatewayBinary.err)
	}
	return gatewayBinary.path
}

// The configuration every gateway in this file runs with. The plane timeout
// is what a loaded CI runner needs: under -race, with other packages' tests
// on the same cores, a claim against the durable store has been seen to take
// longer than 1.5s, and a call that outlives its timeout is indeterminate —
// a claim that committed unseen, a completion the worker must wait out. The
// lease is the smallest the binary accepts for it (plane ≤ L/4); it bounds
// the waits for a lapse.
const (
	binaryLease     = 20 * time.Second
	binaryPlane     = 5 * time.Second
	binaryOperation = 2 * time.Second
	// binaryRecovery bounds a wait that spans a lease lapse and the pull
	// backoff that grows while nothing is claimable.
	binaryRecovery = 120 * time.Second
	// binaryParallel is how many scenarios run at once.
	binaryParallel = 3
)

// binaryWorld is the command world with its explorer reachable through a
// front server the test controls, and the files a gateway reads.
type binaryWorld struct {
	*gatewayOps
	t      *testing.T
	target *paymentTarget
	// front is the explorer as every gateway reaches it unless its flags
	// name another (newFront).
	front *explorerFront
	// outage, while set, resets every explorer request except an extension
	// before it reaches the handler: the plane is unreachable.
	outage atomic.Bool

	token      string
	credential string
	verifier   ed25519.PrivateKey
}

type binaryOptions struct {
	attestation bool
}

func newBinaryWorld(t *testing.T, options binaryOptions) *binaryWorld {
	t.Helper()
	buildGatewayBinary(t)
	target := newPaymentTarget(t)
	targetURL, err := url.Parse(target.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	var trust *executorattest.Trust
	var verifier ed25519.PrivateKey
	if options.attestation {
		trust, verifier = binaryAttestationTrust(t)
	}
	g := newCommandWorldWith(t, statusRoutes, targetURL, trust,
		func(world *executorWorld) { followSystemClock(t, world.h) })
	w := &binaryWorld{gatewayOps: g, t: t, target: target, verifier: verifier}
	w.front = w.newFront()
	g.record()

	dir := t.TempDir()
	w.token = filepath.Join(dir, "token")
	w.writeToken(testExecutorSubject)
	w.credential = filepath.Join(dir, "credential")
	if err := os.WriteFile(w.credential, []byte("Bearer sk_test_binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	return w
}

// followSystemClock makes the explorer's clock the system clock, so leases
// lapse in real time and request deadlines mean the same on both sides for a
// gateway that has no other clock. The harness starts a minute ahead; it is
// set back once, before the gateway's descriptor is registered, and from then
// on only moves forward.
func followSystemClock(t *testing.T, h *approvalHarness) {
	h.clock.Store(time.Now().UnixNano())
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case now := <-ticker.C:
				next := now.UnixNano()
				for {
					current := h.clock.Load()
					if next <= current || h.clock.CompareAndSwap(current, next) {
						break
					}
				}
			}
		}
	}()
	t.Cleanup(func() {
		close(stop)
		<-done
	})
}

// binaryAttestationTrust is the trust root -fleet-executor-attestation would
// load for the stripe executor, and the operator key that signs for it.
func binaryAttestationTrust(t *testing.T) (*executorattest.Trust, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	trustFile := filepath.Join(t.TempDir(), "trust.json")
	body, _ := json.Marshal(map[string]any{"executors": map[string]any{
		testExecutorRef: map[string]any{
			"verifiers": []map[string]any{{
				"id": "operator-key:1", "public_key": base64.StdEncoding.EncodeToString(public),
				"max_validity": "1h", "clock_skew": "1m",
			}},
			"image_digests": []string{attestationImage},
		},
	}})
	if err := os.WriteFile(trustFile, body, 0o600); err != nil {
		t.Fatal(err)
	}
	trust, err := loadExecutorAttestationTrust(trustFile,
		externalFleetEffectBindings{mutating: []string{testExecutorRef}})
	if err != nil {
		t.Fatal(err)
	}
	return trust, private
}

// explorerFront is one listener in front of the explorer: the real
// authenticated handler, with the explorer's Date, behind the world's
// switchable outage. It records every request it is sent, so a process given
// a front of its own can be shown to have sent nothing.
type explorerFront struct {
	w      *binaryWorld
	server *httptest.Server

	mu    sync.Mutex
	calls []explorerCall
}

// explorerCall is one request: its last path segment ("pull", "claim",
// "complete", "ambiguity", "attestation", ...) and when it arrived.
type explorerCall struct {
	name string
	at   time.Time
}

func (w *binaryWorld) newFront() *explorerFront {
	front := &explorerFront{w: w}
	front.server = httptest.NewServer(http.HandlerFunc(front.serve))
	w.t.Cleanup(front.server.Close)
	return front
}

func (f *explorerFront) serve(writer http.ResponseWriter, request *http.Request) {
	w := f.w
	path := request.URL.Path
	f.mu.Lock()
	f.calls = append(f.calls, explorerCall{name: path[strings.LastIndex(path, "/")+1:], at: time.Now()})
	f.mu.Unlock()
	if w.outage.Load() && !strings.HasSuffix(request.URL.Path, "/extend") {
		if conn, _, err := writer.(http.Hijacker).Hijack(); err == nil {
			_ = conn.Close()
		}
		return
	}
	// The handler is bound to the explorer listener's host.
	request.Host = w.host
	writer.Header().Set("Date", w.h.now().UTC().Format(http.TimeFormat))
	w.current.Load().(http.Handler).ServeHTTP(writer, request)
}

// requests is every request the front was sent, in order.
func (f *explorerFront) requests() []explorerCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]explorerCall(nil), f.calls...)
}

// explorerCalls is the names of the requests the world's front was sent.
func (w *binaryWorld) explorerCalls() []string {
	var names []string
	for _, call := range w.front.requests() {
		names = append(names, call.name)
	}
	return names
}

func (w *binaryWorld) writeToken(subject string) {
	w.t.Helper()
	if err := os.WriteFile(w.token, []byte(w.executorToken(subject)), 0o600); err != nil {
		w.t.Fatal(err)
	}
}

func (w *binaryWorld) setTargetStatus(status int) {
	w.target.mu.Lock()
	w.target.status = status
	w.target.mu.Unlock()
}

// gatewayFlags is a run's configuration: the command world's descriptor,
// the stripe executor, the front server and the target.
type gatewayFlags struct {
	dir         string
	executorRef string
	token       string
	// front, when set, is the explorer this gateway is pointed at.
	front *explorerFront
	extra []string
}

func (w *binaryWorld) runArgs(flags gatewayFlags) []string {
	ref := flags.executorRef
	if ref == "" {
		ref = testExecutorRef
	}
	token := flags.token
	if token == "" {
		token = w.token
	}
	front := flags.front
	if front == nil {
		front = w.front
	}
	args := []string{"run",
		"-dispatch-url=" + front.server.URL, "-dispatch-token-file=" + token,
		"-agent-id=" + b64([]byte(commandAgent)), "-executor-ref=" + ref,
		"-surface-name=api.stripe.test",
		"-target-base-url=" + w.target.server.URL, "-target-allow-private",
		"-target-credential-file=" + w.credential,
		"-routes=" + statusRoutes,
		"-idempotency-header=Idempotency-Key", "-idempotency-retention=48h",
		"-unrecorded-dir=" + flags.dir, "-pod-name=gw-e2e",
		"-claim-lease=" + binaryLease.String(), "-renew",
		"-plane-timeout=" + binaryPlane.String(),
		"-operation-timeout=" + binaryOperation.String(),
		"-pull-interval=100ms",
	}
	return append(args, flags.extra...)
}

// gatewayProcess is one shoal-gateway child process.
type gatewayProcess struct {
	name   string
	cmd    *exec.Cmd
	stdout *syncBuffer
	stderr *syncBuffer
	done   chan struct{}
	code   int
}

// spawn starts the binary with args. The process is killed, if it is still
// running, when the test ends, and its output is logged if the test failed.
func spawn(t *testing.T, name string, args ...string) *gatewayProcess {
	t.Helper()
	p := &gatewayProcess{name: name, stdout: &syncBuffer{}, stderr: &syncBuffer{},
		done: make(chan struct{})}
	p.cmd = exec.Command(buildGatewayBinary(t), args...)
	// Hermetic: nothing from this process's environment, and a race in
	// the gateway stops it at once with a code no scenario expects.
	p.cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GORACE=halt_on_error=1 exitcode=66"}
	p.cmd.Stdout, p.cmd.Stderr = p.stdout, p.stderr
	// A child outlives nothing: if the test binary dies (a -timeout panic),
	// the kernel kills it.
	p.cmd.SysProcAttr = childProcAttr()
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		_ = p.cmd.Wait()
		p.code = p.cmd.ProcessState.ExitCode()
		close(p.done)
	}()
	t.Cleanup(func() {
		select {
		case <-p.done:
		default:
			_ = p.cmd.Process.Kill()
			<-p.done
		}
		if t.Failed() {
			t.Logf("%s exited %d\nstdout:\n%s\nstderr:\n%s", p.name, p.code, p.stdout, p.stderr)
		}
	})
	return p
}

// startGateway runs the gateway and waits until its worker is about to
// pull: the command prints its serving line after the startup checks.
func (w *binaryWorld) startGateway(name string, flags gatewayFlags) *gatewayProcess {
	w.t.Helper()
	p := spawn(w.t, name, w.runArgs(flags)...)
	deadline := time.Now().Add(binaryRecovery)
	for !strings.Contains(p.stdout.String(), "shoal-gateway: serving capability") {
		select {
		case <-p.done:
			w.t.Fatalf("%s exited %d at startup: %s", name, p.code, p.stderr)
		default:
		}
		if time.Now().After(deadline) {
			w.t.Fatalf("%s did not start", name)
		}
		time.Sleep(5 * time.Millisecond)
	}
	return p
}

// exit waits for the process to exit and returns its code.
func (p *gatewayProcess) exit(t *testing.T) int {
	t.Helper()
	select {
	case <-p.done:
		return p.code
	case <-time.After(binaryRecovery):
		t.Fatalf("%s did not exit", p.name)
	}
	return -1
}

func (p *gatewayProcess) signal(t *testing.T, sig os.Signal) {
	t.Helper()
	if err := p.cmd.Process.Signal(sig); err != nil {
		t.Fatalf("signal %s: %v", p.name, err)
	}
}

// events is the gateway's log events of this kind, decoded.
func (p *gatewayProcess) events(kind string) []map[string]any {
	var out []map[string]any
	scanner := bufio.NewScanner(strings.NewReader(p.stdout.String()))
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		var event map[string]any
		if json.Unmarshal(scanner.Bytes(), &event) == nil && event["event"] == kind {
			out = append(out, event)
		}
	}
	return out
}

// awaitEvent waits for the gateway to log an event of this kind for which
// match holds (nil: any).
func (p *gatewayProcess) awaitEvent(t *testing.T, kind string, match func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.Now().Add(binaryRecovery)
	for time.Now().Before(deadline) {
		for _, event := range p.events(kind) {
			if match == nil || match(event) {
				return event
			}
		}
		select {
		case <-p.done:
			t.Fatalf("%s exited %d before a %s event", p.name, p.code, kind)
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s logged no %s event", p.name, kind)
	return nil
}

func draining(event map[string]any) bool { return event["not_ready"] == "draining" }

// awaitRecord polls the record, as alice reads it, until cond holds.
func (w *binaryWorld) awaitRecord(id, what string, cond func(recordView) bool) recordView {
	w.t.Helper()
	deadline := time.Now().Add(binaryRecovery)
	for {
		view := w.view(id)
		if cond(view) {
			return view
		}
		if time.Now().After(deadline) {
			w.t.Fatalf("timed out waiting for %s: %+v", what, view)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func inState(state fleet.DispatchState) func(recordView) bool {
	return func(view recordView) bool { return view.State == string(state) }
}

// awaitEntered waits until the target holds a request.
func (w *binaryWorld) awaitEntered() {
	w.t.Helper()
	select {
	case <-w.target.entered:
	case <-time.After(binaryRecovery):
		w.t.Fatal("the target saw no request")
	}
}

// listedReport is a line of `shoal-gateway unrecorded list`.
type listedReport struct {
	ID           string `json:"id"`
	ActionID     string `json:"action_id"`
	Fence        uint64 `json:"fence"`
	RouteAction  string `json:"route_action"`
	Outcome      string `json:"outcome"`
	HasReference bool   `json:"has_reference"`
}

// unrecordedList runs `shoal-gateway unrecorded list` on dir.
func (w *binaryWorld) unrecordedList(dir string) []listedReport {
	w.t.Helper()
	p := spawn(w.t, "unrecorded list", "unrecorded", "list", "-unrecorded-dir", dir)
	if code := p.exit(w.t); code != gatewaycmd.ExitOK {
		w.t.Fatalf("unrecorded list exited %d: %s", code, p.stderr)
	}
	var reports []listedReport
	for _, line := range strings.Split(strings.TrimSpace(p.stdout.String()), "\n") {
		if line == "" {
			continue
		}
		var report listedReport
		if err := json.Unmarshal([]byte(line), &report); err != nil {
			w.t.Fatalf("unrecorded list printed %q: %v", line, err)
		}
		reports = append(reports, report)
	}
	return reports
}

func (w *binaryWorld) assertTarget(effects, requests int) []string {
	w.t.Helper()
	gotEffects, gotRequests, keys := w.target.stats()
	if gotEffects != effects || gotRequests != requests {
		w.t.Fatalf("target: %d effects over %d requests, want %d over %d (keys %q)",
			gotEffects, gotRequests, effects, requests, keys)
	}
	return keys
}

// assertOneEffect is for a scenario that crosses a re-claim: exactly the
// effects given, at least the requests given, and every request under one
// idempotency key. A slow plane can add a re-claim (a completion that never
// committed lapses), which the target replays; it can never add an effect.
func (w *binaryWorld) assertOneEffect(minRequests int) []string {
	w.t.Helper()
	effects, requests, keys := w.target.stats()
	if effects != 1 || requests < minRequests {
		w.t.Fatalf("target: %d effects over %d requests, want 1 over at least %d (keys %q)",
			effects, requests, minRequests, keys)
	}
	for _, key := range keys[1:] {
		if key != keys[0] {
			w.t.Fatalf("a re-claim sent a different idempotency key: %q", keys)
		}
	}
	return keys
}

func outputOf(t *testing.T, view recordView) map[string]any {
	t.Helper()
	var output map[string]any
	if err := json.Unmarshal(view.Output, &output); err != nil {
		t.Fatalf("output %s: %v", view.Output, err)
	}
	return output
}

// TestTheGatewayBinaryEndToEnd runs each scenario against its own explorer,
// target and gateway processes, binaryParallel at a time, the two that wait
// out a lease first.
func TestTheGatewayBinaryEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("about a minute of real leases lapsing; run without -short")
	}
	scenarios := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"SIGKILLMidEffect", binarySIGKILLMidEffect},
		{"PlaneOutageDuringCompletion", binaryPlaneOutage},
		{"HappyPath", binaryHappyPath},
		{"TargetRefusal", binaryTargetRefusal},
		{"SIGTERMDrainsInFlightWork", binarySIGTERMDrains},
		{"SIGTERMTwiceIsAHardStop", binarySIGTERMTwice},
		{"SecondInstanceOnTheDirectory", binarySecondInstance},
		{"WrongExecutorClaimsNothing", binaryWrongExecutor},
		{"AttestationBeforeClaim", binaryAttestation},
		{"UnreachablePlaneDenies", binaryUnreachablePlane},
		{"EnqueueWhilePulling", binaryEnqueueWhilePulling},
	}
	slots := make(chan struct{}, binaryParallel)
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			slots <- struct{}{}
			defer func() { <-slots }()
			scenario.run(t)
		})
	}
}

// Enqueued, pulled, claimed, performed once under the action's ExecutorKey,
// and completed: the record is succeeded with the target's reference. A
// SIGTERM with nothing in hand exits 0 and leaves nothing unrecorded.
func binaryHappyPath(t *testing.T) {
	w := newBinaryWorld(t, binaryOptions{})
	w.enqueueCommand("bin-happy")
	dir := filepath.Join(t.TempDir(), "unrecorded")
	gateway := w.startGateway("gateway", gatewayFlags{dir: dir})

	view := w.awaitRecord("bin-happy", "success", inState(fleet.DispatchSucceeded))
	if output := outputOf(t, view); output["reference"] != "ch_1" || output["idempotency"] != "key" {
		t.Fatalf("output = %s", view.Output)
	}
	keys := w.assertTarget(1, 1)
	if len(keys[0]) != 43 {
		t.Fatalf("idempotency key %q is not the encoded ExecutorKey", keys[0])
	}
	if logs := gateway.stdout.String() + gateway.stderr.String(); strings.Contains(logs, "sk_test_binary") {
		t.Fatalf("the target credential reached the log:\n%s", logs)
	}

	gateway.signal(t, syscall.SIGTERM)
	if code := gateway.exit(t); code != gatewaycmd.ExitOK {
		t.Fatalf("SIGTERM exit %d: %s", code, gateway.stderr)
	}
	if reports := w.unrecordedList(dir); len(reports) != 0 {
		t.Fatalf("unrecorded after a clean run: %+v", reports)
	}
	w.assertTarget(1, 1)
}

// A 422 is one request and a failed record carrying the code; the gateway
// never retries it.
func binaryTargetRefusal(t *testing.T) {
	w := newBinaryWorld(t, binaryOptions{})
	w.setTargetStatus(http.StatusUnprocessableEntity)
	w.enqueueCommand("bin-422")
	gateway := w.startGateway("gateway", gatewayFlags{dir: filepath.Join(t.TempDir(), "unrecorded")})

	view := w.awaitRecord("bin-422", "the failure", inState(fleet.DispatchFailed))
	if view.ErrorCode != "target_rejected_422" {
		t.Fatalf("error code %q", view.ErrorCode)
	}
	gateway.signal(t, syscall.SIGTERM)
	if code := gateway.exit(t); code != gatewaycmd.ExitOK {
		t.Fatalf("SIGTERM exit %d: %s", code, gateway.stderr)
	}
	// The process is gone, so nothing more can arrive: one request, ever.
	w.assertTarget(0, 1)
	// The record stays failed with the code. It may carry an
	// outcome_unknown report under the same fence: a completion the plane
	// answered too slowly is indeterminate, and the worker, unable to tell
	// whether it committed, waits out the lease and reports through the
	// ambiguity route, which appends to the record whatever its state
	// (docs/effects-gateway-deploy.md, "The worker loop").
	after := w.view("bin-422")
	if after.State != string(fleet.DispatchFailed) || after.ErrorCode != "target_rejected_422" ||
		after.ClaimFence != view.ClaimFence {
		t.Fatalf("the failed record changed: %+v", after)
	}
	for _, report := range after.AmbiguityReports {
		if report.ClaimFence != view.ClaimFence || report.Outcome != string(fleet.AmbiguityOutcomeUnknown) {
			t.Fatalf("a report other than the indeterminate completion's: %+v", report)
		}
	}
}

// The gateway is SIGKILLed with the effect done and the target's answer not
// yet sent. A restarted gateway on the same directory waits out the lease,
// re-claims, resends under the same ExecutorKey, and the target replays it:
// one effect. The dead claim's completion is refused.
func binarySIGKILLMidEffect(t *testing.T) {
	w := newBinaryWorld(t, binaryOptions{})
	w.target.hold()
	w.enqueueCommand("bin-kill")
	dir := filepath.Join(t.TempDir(), "unrecorded")
	first := w.startGateway("first", gatewayFlags{dir: dir})
	w.awaitEntered()
	dead := w.view("bin-kill")
	if dead.State != string(fleet.DispatchClaimed) {
		t.Fatalf("state with the request in flight = %s", dead.State)
	}

	first.signal(t, syscall.SIGKILL)
	first.exit(t)
	w.target.release()

	second := w.startGateway("second", gatewayFlags{dir: dir})
	view := w.awaitRecord("bin-kill", "recovery", inState(fleet.DispatchSucceeded))
	if view.ClaimFence == dead.ClaimFence {
		t.Fatal("completed under the dead claim's fence")
	}
	w.assertOneEffect(2)
	if output := outputOf(t, view); output["idempotency"] != "replayed" || output["reference"] != "ch_1" {
		t.Fatalf("output = %s", view.Output)
	}

	// The dead claim's completion, were it ever sent, is refused.
	deadClaimID, err := base64.RawURLEncoding.DecodeString(dead.ClaimID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = w.gateway(testExecutorRef).Complete(t.Context(), []byte("bin-kill"), effectsgateway.Completion{
		Context: effectsgateway.RequestContext{RequestID: []byte("dead-complete"),
			CorrelationID: []byte("alice-trace-bin-kill"), ReasonCode: "gateway_complete",
			Deadline: w.h.now().Add(time.Minute)},
		ExpectedVersion: dead.Version, ClaimID: deadClaimID, ClaimFence: dead.ClaimFence,
		Output: json.RawMessage(`{"status":200,"idempotency":"key","reference":"ch_1"}`),
	})
	if err == nil {
		t.Fatal("the dead claim's completion was accepted")
	}
	if after := w.view("bin-kill"); after.Version != view.Version || after.ClaimFence != view.ClaimFence {
		t.Fatalf("the refused completion moved the record: %+v", after)
	}
	second.signal(t, syscall.SIGTERM)
	if code := second.exit(t); code != gatewaycmd.ExitOK {
		t.Fatalf("SIGTERM exit %d: %s", code, second.stderr)
	}
	w.assertOneEffect(2)
}

// SIGTERM with a request in flight: the gateway stops pulling, the request
// finishes, the record is completed, and the exit is 0 with nothing left in
// the unrecorded log.
func binarySIGTERMDrains(t *testing.T) {
	w := newBinaryWorld(t, binaryOptions{})
	w.target.hold()
	w.enqueueCommand("bin-drain")
	dir := filepath.Join(t.TempDir(), "unrecorded")
	gateway := w.startGateway("gateway", gatewayFlags{dir: dir})
	w.awaitEntered()

	gateway.signal(t, syscall.SIGTERM)
	gateway.awaitEvent(t, "readiness", draining)
	w.target.release()
	if code := gateway.exit(t); code != gatewaycmd.ExitOK {
		t.Fatalf("drain exit %d: %s", code, gateway.stderr)
	}
	// Settled: with the plane answering, on the record, and so nothing in
	// the unrecorded log.
	view := w.view("bin-drain")
	if reports := w.unrecordedList(dir); view.State != string(fleet.DispatchSucceeded) || len(reports) != 0 {
		t.Fatalf("after the drain: state %s, unrecorded %+v", view.State, reports)
	}
	w.assertTarget(1, 1)
}

// A second SIGTERM during the drain is a hard stop: exit 4, and the request
// in flight is in the unrecorded log as outcome_unknown under the claim's
// fence. `unrecorded list` shows it and `unrecorded ack` clears it, both
// through the binary.
func binarySIGTERMTwice(t *testing.T) {
	w := newBinaryWorld(t, binaryOptions{})
	w.target.hold()
	w.enqueueCommand("bin-hard")
	dir := filepath.Join(t.TempDir(), "unrecorded")
	gateway := w.startGateway("gateway", gatewayFlags{dir: dir})
	w.awaitEntered()
	claimed := w.view("bin-hard")

	gateway.signal(t, syscall.SIGTERM)
	gateway.awaitEvent(t, "readiness", draining)
	gateway.signal(t, syscall.SIGTERM)
	if code := gateway.exit(t); code != gatewaycmd.ExitHardStop {
		t.Fatalf("hard stop exit %d, want %d: %s", code, gatewaycmd.ExitHardStop, gateway.stderr)
	}

	reports := w.unrecordedList(dir)
	if len(reports) != 1 {
		t.Fatalf("unrecorded after a hard stop: %+v", reports)
	}
	report := reports[0]
	if report.ActionID != b64([]byte("bin-hard")) || report.Fence != claimed.ClaimFence ||
		report.Outcome != string(fleet.AmbiguityOutcomeUnknown) || report.RouteAction != "status" {
		t.Fatalf("unrecorded entry = %+v, claim fence %d", report, claimed.ClaimFence)
	}
	if view := w.view("bin-hard"); view.State != string(fleet.DispatchClaimed) {
		t.Fatalf("a hard stop completed the record: %s", view.State)
	}

	ack := spawn(t, "unrecorded ack", "unrecorded", "ack", "-unrecorded-dir", dir, report.ID)
	if code := ack.exit(t); code != gatewaycmd.ExitOK {
		t.Fatalf("unrecorded ack exited %d: %s", code, ack.stderr)
	}
	if !strings.Contains(ack.stdout.String(), `"event":"unrecorded_cleared"`) {
		t.Fatalf("ack logged no unrecorded_cleared event: %s", ack.stdout)
	}
	if reports := w.unrecordedList(dir); len(reports) != 0 {
		t.Fatalf("unrecorded after the ack: %+v", reports)
	}
	again := spawn(t, "unrecorded ack again", "unrecorded", "ack", "-unrecorded-dir", dir, report.ID)
	if code := again.exit(t); code != gatewaycmd.ExitFailure {
		t.Fatalf("a second ack of the same report exited %d", code)
	}
	w.assertTarget(1, 1)
}

// The plane goes away as the target answers: the completion is
// indeterminate, the gateway waits out its lease so whatever it sent has
// settled, its ambiguity report cannot reach the plane either, and the
// report is held in the unrecorded log. Restarted with the plane back, the
// gateway presents the held report, which the record now carries, and the
// entry clears; the lapsed claim is re-claimed and replayed for one effect.
func binaryPlaneOutage(t *testing.T) {
	w := newBinaryWorld(t, binaryOptions{})
	w.target.hold()
	w.enqueueCommand("bin-outage")
	dir := filepath.Join(t.TempDir(), "unrecorded")
	first := w.startGateway("first", gatewayFlags{dir: dir})
	w.awaitEntered()
	lost := w.view("bin-outage")
	w.outage.Store(true)
	w.target.release()

	first.awaitEvent(t, "dispatch_error", func(event map[string]any) bool {
		return event["dispatch_error"] == string(effectsgateway.DispatchIndeterminate) &&
			event["action_id"] == b64([]byte("bin-outage"))
	})
	held := first.awaitEvent(t, "unrecorded", func(event map[string]any) bool {
		return event["ambiguity"] == string(fleet.AmbiguityEffectObserved)
	})
	if held["fence"] != float64(lost.ClaimFence) {
		t.Fatalf("held under fence %v, claimed under %d", held["fence"], lost.ClaimFence)
	}
	if len(first.events("ambiguity_reported")) != 0 {
		t.Fatal("an ambiguity report reached a plane that is down")
	}
	// The settle wait: an indeterminate completion may still commit, so the
	// report waits out the lease the explorer holds for the fence. The
	// worker's end is anchored on when it sent its last claim or extension,
	// which precedes the explorer's apply by less than the plane timeout, so
	// its first report may come at most that much before the explorer's end
	// — and, without the wait, would come a lease earlier.
	leaseEnd := w.view("bin-outage").ClaimLeaseUntil
	if leaseEnd.IsZero() {
		t.Fatal("the record names no lease end")
	}
	var firstReport time.Time
	for _, call := range w.front.requests() {
		if call.name == "ambiguity" {
			firstReport = call.at
			break
		}
	}
	if firstReport.IsZero() {
		t.Fatal("the gateway never tried to report")
	}
	if earliest := leaseEnd.Add(-binaryPlane); firstReport.Before(earliest) {
		t.Fatalf("the report was tried at %s, %s before the lease the explorer held ended at %s: "+
			"the worker did not wait out the lease", firstReport.Format(time.RFC3339Nano),
			leaseEnd.Sub(firstReport), leaseEnd.Format(time.RFC3339Nano))
	}
	first.signal(t, syscall.SIGTERM)
	if code := first.exit(t); code != gatewaycmd.ExitOK {
		t.Fatalf("drain exit %d: %s", code, first.stderr)
	}
	reports := w.unrecordedList(dir)
	if len(reports) != 1 || reports[0].Outcome != string(fleet.AmbiguityEffectObserved) ||
		!reports[0].HasReference || reports[0].Fence != lost.ClaimFence {
		t.Fatalf("unrecorded = %+v", reports)
	}
	w.outage.Store(false)
	if view := w.view("bin-outage"); view.State != string(fleet.DispatchClaimed) ||
		len(view.AmbiguityReports) != 0 {
		t.Fatalf("the record during the outage: %+v", view)
	}

	second := w.startGateway("second", gatewayFlags{dir: dir})
	second.awaitEvent(t, "unrecorded_cleared", nil)
	view := w.awaitRecord("bin-outage", "recovery", inState(fleet.DispatchSucceeded))
	if len(view.AmbiguityReports) != 1 {
		t.Fatalf("reports on the record = %+v", view.AmbiguityReports)
	}
	report := view.AmbiguityReports[0]
	if report.ClaimFence != lost.ClaimFence || report.Outcome != string(fleet.AmbiguityEffectObserved) ||
		report.Reference != "ch_1" || report.Target != "api.stripe.test" {
		t.Fatalf("report = %+v", report)
	}
	if view.ClaimFence == lost.ClaimFence {
		t.Fatal("the record was completed under the lost fence")
	}
	w.assertOneEffect(2)
	second.signal(t, syscall.SIGTERM)
	if code := second.exit(t); code != gatewaycmd.ExitOK {
		t.Fatalf("drain exit %d: %s", code, second.stderr)
	}
	if reports := w.unrecordedList(dir); len(reports) != 0 {
		t.Fatalf("unrecorded after the replay: %+v", reports)
	}
}

// A second gateway on a running gateway's directory is refused by the lock
// before it talks to the explorer: exit 1, a refused start
// (docs/effects-gateway-deploy.md, "Startup"). The operator's unrecorded
// commands refuse there too, and work once the gateway has stopped.
func binarySecondInstance(t *testing.T) {
	w := newBinaryWorld(t, binaryOptions{})
	// Enqueued before any gateway runs, as in every scenario here: an
	// enqueue racing an executor's pulls has been seen to answer an
	// indeterminate 503 from the explorer, which is not what this tests.
	w.enqueueCommand("bin-lock")
	dir := filepath.Join(t.TempDir(), "unrecorded")
	first := w.startGateway("first", gatewayFlags{dir: dir})

	// Its own front, so what it sends is told apart from the first's.
	secondFront := w.newFront()
	second := spawn(t, "second", w.runArgs(gatewayFlags{dir: dir, front: secondFront})...)
	deadline := time.Now().Add(binaryRecovery)
	for running := true; running; {
		select {
		case <-second.done:
			running = false
		default:
			if strings.Contains(second.stdout.String(), "serving capability") {
				t.Fatal("a second gateway started on a running gateway's directory")
			}
			if time.Now().After(deadline) {
				t.Fatal("the second gateway neither started nor exited")
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	if code := second.exit(t); code != gatewaycmd.ExitFailure {
		t.Fatalf("a second gateway on the directory exited %d, want %d: %s",
			code, gatewaycmd.ExitFailure, second.stderr)
	}
	if !strings.Contains(second.stderr.String(), effectsgateway.ErrGatewayLocked.Error()) {
		t.Fatalf("the refusal does not name the lock: %s", second.stderr)
	}
	if strings.Contains(second.stdout.String(), "serving capability") {
		t.Fatal("the second gateway started serving")
	}
	// Refused before any network I/O: the lock comes before the resolve.
	if calls := secondFront.requests(); len(calls) != 0 {
		t.Fatalf("the refused gateway sent the explorer %d requests: %+v", len(calls), calls)
	}
	list := spawn(t, "unrecorded list", "unrecorded", "list", "-unrecorded-dir", dir)
	if code := list.exit(t); code != gatewaycmd.ExitFailure ||
		!strings.Contains(list.stderr.String(), "a gateway is running") {
		t.Fatalf("list beside a running gateway exited %d: %s", code, list.stderr)
	}

	// The first still works: the refused start took nothing from it.
	w.awaitRecord("bin-lock", "success", inState(fleet.DispatchSucceeded))
	first.signal(t, syscall.SIGTERM)
	if code := first.exit(t); code != gatewaycmd.ExitOK {
		t.Fatalf("SIGTERM exit %d: %s", code, first.stderr)
	}
	if reports := w.unrecordedList(dir); len(reports) != 0 {
		t.Fatalf("unrecorded = %+v", reports)
	}
	w.assertTarget(1, 1)
}

// A gateway holding another executor's credential, or bound to another
// executor ref than its descriptor's, claims nothing: its start is refused
// at the resolve, the action stays queued, and the target sees nothing. The
// right executor then performs it.
func binaryWrongExecutor(t *testing.T) {
	w := newBinaryWorld(t, binaryOptions{})
	w.enqueueCommand("bin-binding")
	other := filepath.Join(t.TempDir(), "other-token")
	if err := os.WriteFile(other, []byte(w.executorToken(testOtherExecutorSubject)), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, wrong := range []gatewayFlags{
		// The ledger executor's own credential and ref.
		{executorRef: testOtherExecutorRef, token: other},
		// The ledger executor's credential, claiming to be stripe.
		{executorRef: testExecutorRef, token: other},
		// The stripe credential, bound to ledger.
		{executorRef: testOtherExecutorRef},
	} {
		wrong.dir = filepath.Join(t.TempDir(), "unrecorded")
		p := spawn(t, "wrong executor "+wrong.executorRef, w.runArgs(wrong)...)
		if code := p.exit(t); code != gatewaycmd.ExitFailure ||
			!strings.Contains(p.stderr.String(), "refusing to start") {
			t.Fatalf("a gateway bound to the wrong executor exited %d: %s", code, p.stderr)
		}
		t.Logf("%s", p.stderr)
	}
	for _, call := range w.explorerCalls() {
		if call == "pull" || call == "claim" {
			t.Fatalf("a wrongly bound gateway reached %s: %q", call, w.explorerCalls())
		}
	}
	if view := w.view("bin-binding"); view.State != string(fleet.DispatchQueued) {
		t.Fatalf("state after the wrong executors = %s", view.State)
	}
	w.assertTarget(0, 0)

	gateway := w.startGateway("right executor", gatewayFlags{dir: filepath.Join(t.TempDir(), "unrecorded")})
	w.awaitRecord("bin-binding", "success", inState(fleet.DispatchSucceeded))
	w.assertTarget(1, 1)
	gateway.signal(t, syscall.SIGTERM)
	if code := gateway.exit(t); code != gatewaycmd.ExitOK {
		t.Fatalf("SIGTERM exit %d: %s", code, gateway.stderr)
	}
}

// The action requires attestation: the gateway presents its operator-signed
// statement over the real presentation route before it claims, never by way
// of a refused claim, and the claim succeeds. Without the attestation flags
// the same descriptor refuses the start.
func binaryAttestation(t *testing.T) {
	w := newBinaryWorld(t, binaryOptions{attestation: true})
	w.enqueueCommand("bin-attest")

	refused := spawn(t, "unattested", w.runArgs(gatewayFlags{dir: filepath.Join(t.TempDir(), "unrecorded")})...)
	if code := refused.exit(t); code != gatewaycmd.ExitFailure ||
		!strings.Contains(refused.stderr.String(), "requires attestation") {
		t.Fatalf("an unattested gateway exited %d: %s", code, refused.stderr)
	}

	dir := t.TempDir()
	statement, key := filepath.Join(dir, "statement"), filepath.Join(dir, "key")
	issued := w.h.now()
	want := executorattest.Expectation{
		Principal: executorattest.Principal{
			Domain:   workspaceAuthorizationDomain,
			Subject:  executorIdentity(w.executorIssuer.server.URL, testExecutorSubject),
			ClientID: executorIdentity(w.executorIssuer.server.URL, testExecutorSubject),
		},
		ExecutorRef: testExecutorRef, Key: []byte("gw-e2e-key"), Now: issued,
	}
	evidence, err := executorattest.SignStatement("operator-key:1", w.verifier,
		executorattest.StatementFor(want, attestationImage, issued, issued.Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statement, evidence, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, []byte("gw-e2e-key"), 0o600); err != nil {
		t.Fatal(err)
	}

	gateway := w.startGateway("attested", gatewayFlags{dir: filepath.Join(t.TempDir(), "unrecorded"),
		extra: []string{"-attestation-statement-file=" + statement, "-attestation-key-file=" + key}})
	w.awaitRecord("bin-attest", "success", inState(fleet.DispatchSucceeded))
	w.assertTarget(1, 1)

	var sequence []string
	for _, call := range w.explorerCalls() {
		switch call {
		case "attestation", "claim", "complete":
			sequence = append(sequence, call)
		}
	}
	if len(sequence) < 3 || sequence[0] != "attestation" || !strings.Contains(strings.Join(sequence, ","), "claim") {
		t.Fatalf("explorer calls = %q: want attestation before any claim", sequence)
	}
	// Never by way of a refused claim: the attestation gate answers 409.
	// (A claim slower than the plane timeout is a repull, which is not
	// this; it re-claims after the lapse.)
	for _, event := range gateway.events("dispatch_error") {
		if event["dispatch_error"] != string(effectsgateway.DispatchConflict) {
			continue
		}
		t.Fatalf("a claim refused on the attested path: %v", event)
	}
	gateway.signal(t, syscall.SIGTERM)
	if code := gateway.exit(t); code != gatewaycmd.ExitOK {
		t.Fatalf("SIGTERM exit %d: %s", code, gateway.stderr)
	}
}

// With the plane unreachable the gateway denies: it cannot resolve its
// descriptor, so it refuses to start, and nothing reaches the target. Back,
// the same gateway performs the action.
func binaryUnreachablePlane(t *testing.T) {
	w := newBinaryWorld(t, binaryOptions{})
	w.enqueueCommand("bin-plane")
	w.outage.Store(true)
	dir := filepath.Join(t.TempDir(), "unrecorded")
	down := spawn(t, "plane down", w.runArgs(gatewayFlags{dir: dir})...)
	if code := down.exit(t); code != gatewaycmd.ExitFailure ||
		!strings.Contains(down.stderr.String(), "refusing to start") {
		t.Fatalf("a gateway with the plane down exited %d: %s", code, down.stderr)
	}
	w.outage.Store(false)
	if view := w.view("bin-plane"); view.State != string(fleet.DispatchQueued) {
		t.Fatalf("state with the plane down = %s", view.State)
	}
	w.assertTarget(0, 0)

	gateway := w.startGateway("plane back", gatewayFlags{dir: dir})
	w.awaitRecord("bin-plane", "success", inState(fleet.DispatchSucceeded))
	w.assertTarget(1, 1)
	gateway.signal(t, syscall.SIGTERM)
	if code := gateway.exit(t); code != gatewaycmd.ExitOK {
		t.Fatalf("SIGTERM exit %d: %s", code, gateway.stderr)
	}
}

// Work enqueued while the gateway is already pulling is performed once. The
// enqueue is not retried: an indeterminate answer from the explorer here is
// the defect this scenario exists to show (#641).
func binaryEnqueueWhilePulling(t *testing.T) {
	// This scenario was added skipped, because 4 runs in 50 failed under
	// -race, GOMAXPROCS=2 and a full CPU load: the enqueue was answered an
	// indeterminate 503 ("fleet action outcome requires reconciliation").
	//
	// #635 closed a committed-value window in the store — a non-nil guard
	// head at epoch N whose committed cell at N was not visible yet, which a
	// concurrent claim or extend opens. MayPublishActionEvent reads the
	// action, and reconcileActionTransitions returns an error from that
	// question rather than skipping it ("an error from the question is not a
	// skip"), so the window surfaced as the enqueuer's own ErrActionCommitted
	// over a row that was not even its own. #641 was #633 arriving on the
	// write path.
	//
	// Re-measured on #635 at -count=50, -race, GOMAXPROCS=2 and four busy
	// cores: 0 failures. Against the 8% baseline that is p ≈ 0.015, so it is
	// strong evidence and not proof — if this flakes again, the remaining
	// suspect is that same unskipped error-from-the-question path, and the
	// narrow fix is to treat a transient there as a skip (#641).
	w := newBinaryWorld(t, binaryOptions{})
	gateway := w.startGateway("gateway", gatewayFlags{dir: filepath.Join(t.TempDir(), "unrecorded")})
	deadline := time.Now().Add(binaryRecovery)
	for !strings.Contains(strings.Join(w.explorerCalls(), ","), "pull") {
		if time.Now().After(deadline) {
			t.Fatal("the gateway never pulled")
		}
		time.Sleep(5 * time.Millisecond)
	}
	w.enqueueCommand("bin-late")
	w.awaitRecord("bin-late", "success", inState(fleet.DispatchSucceeded))
	w.assertTarget(1, 1)
	gateway.signal(t, syscall.SIGTERM)
	if code := gateway.exit(t); code != gatewaycmd.ExitOK {
		t.Fatalf("SIGTERM exit %d: %s", code, gateway.stderr)
	}
}
