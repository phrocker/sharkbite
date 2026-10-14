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
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/effectsgateway"
)

func TestMainDispatchesSubcommands(t *testing.T) {
	for _, row := range []struct {
		args []string
		code int
	}{
		{nil, ExitUsage},
		{[]string{"serve"}, ExitUsage},
		{[]string{"help"}, ExitOK},
		{[]string{"run", "-h"}, ExitOK},
		{[]string{"unrecorded"}, ExitUsage},
		{[]string{"unrecorded", "purge"}, ExitUsage},
		{[]string{"grace-period", "stray"}, ExitUsage},
	} {
		if code := Main(row.args, Env{}); code != row.code {
			t.Errorf("%q: exit %d, want %d", row.args, code, row.code)
		}
	}
}

// TestRunRefusesMisconfigurationBeforeAnything: a refused flag is a usage
// exit with nothing touched — no lock taken, no explorer request.
func TestRunRefusesMisconfigurationBeforeAnything(t *testing.T) {
	w := newWorld(t)
	for name, changes := range map[string]map[string]string{
		"no executor ref":      {"executor-ref": "<unset>"},
		"no unrecorded dir":    {"unrecorded-dir": "<unset>"},
		"route table invalid":  {"routes": `[{"action":"status","method":"GET","path":"/x","effects":["external"],"idempotency":"key"}]`},
		"route effects wrong":  {"routes": strings.ReplaceAll(testRoutes, `["external"]`, `["external","egresses-content"]`)},
		"lease cannot cover T": {"operation-timeout": "3m59s"},
	} {
		stderr := &syncBuffer{}
		code := Main(append([]string{"run"}, w.args(changes)...), Env{Stderr: stderr})
		if code != ExitUsage {
			t.Errorf("%s: exit %d, want %d (%s)", name, code, ExitUsage, stderr)
		}
	}
	if got := w.explorer.requests(); len(got) != 0 {
		t.Fatalf("a refused configuration reached the explorer: %v", got)
	}
	if _, err := os.Stat(w.dir); !os.IsNotExist(err) {
		t.Fatalf("a refused configuration created the unrecorded directory: %v", err)
	}
}

// TestRunTakesTheLockBeforeAnyNetworkIO: with another gateway holding the
// directory, the command refuses having sent nothing at all — not even the
// startup resolve. Two replicas never both talk to the explorer.
func TestRunTakesTheLockBeforeAnyNetworkIO(t *testing.T) {
	w := newWorld(t)
	held, err := effectsgateway.OpenUnrecordedLog(w.dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	stderr := &syncBuffer{}
	code := Main(append([]string{"run"}, w.args(nil)...), Env{Stderr: stderr})
	if code != ExitFailure {
		t.Fatalf("exit %d, want %d (%s)", code, ExitFailure, stderr)
	}
	if !strings.Contains(stderr.String(), "another effects gateway holds") {
		t.Fatalf("the refusal does not name the lock: %s", stderr)
	}
	if got := w.explorer.requests(); len(got) != 0 {
		t.Fatalf("the explorer was called before the lock was taken: %v", got)
	}
}

// TestRunRefusesAResolveRefusal: the explorer refusing the startup resolve
// is a refused start — no pull, a nonzero exit, and the lock released.
func TestRunRefusesAResolveRefusal(t *testing.T) {
	w := newWorld(t)
	w.explorer.resolveStatus = http.StatusForbidden
	stderr := &syncBuffer{}
	code := Main(append([]string{"run"}, w.args(nil)...), Env{Stderr: stderr})
	if code != ExitFailure {
		t.Fatalf("exit %d, want %d (%s)", code, ExitFailure, stderr)
	}
	requests := w.explorer.requests()
	if len(requests) != 1 || !strings.HasSuffix(requests[0], "/resolve") {
		t.Fatalf("requests %v, want the resolve alone", requests)
	}
	if !strings.Contains(stderr.String(), "refusing to start") {
		t.Fatalf("stderr: %s", stderr)
	}
	log, err := effectsgateway.OpenUnrecordedLog(w.dir, nil)
	if err != nil {
		t.Fatalf("the refused start kept the lock: %v", err)
	}
	_ = log.Close()
}

// TestRunRefusesADescriptorTheRoutesDoNotMatch: startup fails closed on any
// difference between the route table and the resolved descriptor, and on a
// descriptor bound to another executor ref.
func TestRunRefusesADescriptorTheRoutesDoNotMatch(t *testing.T) {
	for name, edit := range map[string]func(*fakeExplorer){
		"route action not declared": func(f *fakeExplorer) { f.capabilities[0].Actions[0].Name = "refund" },
		"capability not declared":   func(f *fakeExplorer) { f.capabilities[0].Name = "other" },
		"output schema differs": func(f *fakeExplorer) {
			f.capabilities[0].Actions[0].OutputSchema = json.RawMessage(`{"type":"object"}`)
		},
		"descriptor of another executor": func(f *fakeExplorer) { f.executorRef = "ledger" },
		"attestation required, none configured": func(f *fakeExplorer) {
			f.capabilities[0].Actions[0].RequiresAttestation = true
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			edit(w.explorer)
			stderr := &syncBuffer{}
			code := Main(append([]string{"run"}, w.args(nil)...), Env{Stderr: stderr})
			if code != ExitFailure {
				t.Fatalf("exit %d, want %d (%s)", code, ExitFailure, stderr)
			}
			for _, path := range w.explorer.requests() {
				if !strings.HasSuffix(path, "/resolve") {
					t.Fatalf("the refused start went on to %s", path)
				}
			}
		})
	}
	t.Run("attestation configured, none required", func(t *testing.T) {
		w := newWorld(t)
		stderr := &syncBuffer{}
		code := Main(append([]string{"run"}, w.args(map[string]string{
			"attestation-statement-file": "/run/statement", "attestation-key-file": "/run/key",
		})...), Env{Stderr: stderr})
		if code != ExitFailure || !strings.Contains(stderr.String(), "no action") {
			t.Fatalf("exit %d: %s", code, stderr)
		}
	})
}

// TestSignalDrainsAndExitsCleanly: an idle gateway is ready, reports its
// gauges, and the first signal drains it to a clean exit.
func TestSignalDrainsAndExitsCleanly(t *testing.T) {
	w := newWorld(t)
	run := start(t, w.args(nil), nil)
	if status, body := run.get("/healthz"); status != http.StatusOK {
		t.Fatalf("/healthz = %d %s", status, body)
	}
	waitFor(t, "ready", func() bool { status, _ := run.get("/readyz"); return status == http.StatusOK })
	_, metrics := run.get("/metrics")
	for _, want := range []string{
		"shoal_effects_gateway_unrecorded_entries 0",
		"shoal_effects_gateway_in_flight 0",
		"shoal_effects_gateway_grace_period_seconds 225",
	} {
		if !strings.Contains(metrics, want) {
			t.Fatalf("/metrics lacks %q:\n%s", want, metrics)
		}
	}
	if !strings.Contains(run.stdout.String(), "terminationGracePeriodSeconds must be at least 225") {
		t.Fatalf("the grace period was not logged at start: %s", run.stdout)
	}
	run.signals <- syscall.SIGTERM
	code, ok := run.exit(20 * time.Second)
	if !ok || code != ExitOK {
		t.Fatalf("exit %d (exited %v): %s", code, ok, run.stderr)
	}
	log, err := effectsgateway.OpenUnrecordedLog(w.dir, nil)
	if err != nil {
		t.Fatalf("the clean exit kept the lock: %v", err)
	}
	_ = log.Close()
}

// TestReadinessNamesWhyNot: /readyz is 503 with the worker's reason while
// draining, and the in-flight gauge counts the claim being drained.
func TestReadinessNamesWhyNot(t *testing.T) {
	w := newWorld(t)
	w.target.hold = true
	w.explorer.offerAction("act-drain")
	run := start(t, w.args(nil), nil)
	select {
	case <-w.target.entered:
	case <-time.After(20 * time.Second):
		t.Fatal("the target saw no request")
	}
	run.signals <- syscall.SIGTERM
	waitFor(t, "draining", func() bool {
		status, body := run.get("/readyz")
		return status == http.StatusServiceUnavailable && strings.Contains(body, `"detail":"draining"`)
	})
	if _, metrics := run.get("/metrics"); !strings.Contains(metrics, "shoal_effects_gateway_in_flight 1") {
		t.Fatalf("in-flight gauge:\n%s", metrics)
	}
	close(w.target.release)
	code, ok := run.exit(20 * time.Second)
	if !ok || code != ExitOK {
		t.Fatalf("exit %d (exited %v): %s", code, ok, run.stderr)
	}
	if got := w.explorer.completions(); len(got) != 1 || !strings.Contains(string(got[0]), `"ch_1"`) {
		t.Fatalf("completions %s", got)
	}
}

// TestASecondSignalIsAHardStop: with a request in flight the first signal
// drains, which would wait up to the operation timeout; the second stops at
// once, with its own exit code, and reports nothing.
func TestASecondSignalIsAHardStop(t *testing.T) {
	w := newWorld(t)
	w.target.hold = true
	w.explorer.offerAction("act-hard")
	run := start(t, w.args(nil), nil)
	select {
	case <-w.target.entered:
	case <-time.After(20 * time.Second):
		t.Fatal("the target saw no request")
	}
	run.signals <- syscall.SIGTERM
	if _, exited := run.exit(300 * time.Millisecond); exited {
		t.Fatal("the first signal stopped the gateway with a request in flight")
	}
	run.signals <- syscall.SIGINT
	code, ok := run.exit(5 * time.Second)
	if !ok {
		t.Fatal("the second signal did not stop the gateway: it is still draining")
	}
	if code != ExitHardStop {
		t.Fatalf("exit %d, want %d: %s", code, ExitHardStop, run.stderr)
	}
	if got := w.explorer.completions(); len(got) != 0 {
		t.Fatalf("a hard stop completed: %s", got)
	}
	// The request had reached the target: the hard stop wrote it to the
	// unrecorded log before cancelling, as an outcome nobody knows.
	stdout := &syncBuffer{}
	if code := Main([]string{"unrecorded", "list", "-unrecorded-dir", w.dir}, Env{Stdout: stdout}); code != ExitOK {
		t.Fatalf("list exit %d", code)
	}
	if !strings.Contains(stdout.String(), `"outcome":"outcome_unknown"`) ||
		!strings.Contains(stdout.String(), `"action_id":"`+b64([]byte("act-hard"))+`"`) ||
		strings.Count(stdout.String(), "\n") != 1 {
		t.Fatalf("the hard stop left no unrecorded entry for the request in flight: %q", stdout)
	}
}

// TestADrainThatRunsOutExitsDistinctly: the request hangs at the target, the
// drain bound passes on the worker's clock with the run still in flight, and
// the run is abandoned to the unrecorded log as outcome_unknown; the exit
// code says so, and the log holds the entry.
//
// The run must still be in flight when the bound fires, or this is not the
// abandon path. The bound (the grace period less the exit margin) is longer
// than every timer the run arms on the same clock (the attempt's operation
// timeout, the completion budget, the renewal), so advancing the clock by
// the bound fires those too, and they race the drain. An earlier version let
// the target answer and hung the completion: Advance(bound) also expired the
// completion's budget, and when that goroutine won, the run reported through
// the ambiguity route (the fake explorer has none), wrote its own
// effect_observed entry and settled before abandon looked. Abandon then
// rightly wrote nothing more (a run's own report keeps its real outcome
// rather than an outcome_unknown in its place), and the test saw
// effect_observed. So the target holds the request until it is cancelled,
// and only the drain's own timer is fired: nothing else can move the run
// before abandon takes it.
func TestADrainThatRunsOutExitsDistinctly(t *testing.T) {
	w := newWorld(t)
	w.target.hold = true
	w.explorer.offerAction("act-abandon")
	clock := newManualClock()
	run := start(t, w.args(nil), clock)
	select {
	case <-w.target.entered:
	case <-time.After(20 * time.Second):
		t.Fatal("the target saw no request")
	}
	run.signals <- syscall.SIGTERM
	bound := effectsgateway.DrainBound(effectsgateway.DefaultOperationTimeout, effectsgateway.DefaultPlaneTimeout)
	waitFor(t, "the drain's bound", func() bool { return clock.wasAsked(bound) })
	clock.Fire(bound)
	code, ok := run.exit(20 * time.Second)
	if !ok || code != ExitDrainAbandoned {
		t.Fatalf("exit %d (exited %v), want %d: %s", code, ok, ExitDrainAbandoned, run.stderr)
	}
	if got := w.explorer.completions(); len(got) != 0 {
		t.Fatalf("an abandoned run completed: %s", got)
	}
	stdout := &syncBuffer{}
	if code := Main([]string{"unrecorded", "list", "-unrecorded-dir", w.dir}, Env{Stdout: stdout}); code != ExitOK {
		t.Fatalf("list exit %d", code)
	}
	if !strings.Contains(stdout.String(), `"outcome":"outcome_unknown"`) ||
		!strings.Contains(stdout.String(), `"action_id":"`+b64([]byte("act-abandon"))+`"`) ||
		strings.Count(stdout.String(), "\n") != 1 {
		t.Fatalf("the abandoned run is not in the log: %s", stdout)
	}
}

// TestRenewIsWired: -renew admits T = 10m under L = 1m, and the worker sends
// under the renewing gate. Ignored, the gate would require the lease to hold
// T + 5s and nothing would be sent.
func TestRenewIsWired(t *testing.T) {
	w := newWorld(t)
	w.explorer.offerAction("act-renew")
	run := start(t, w.args(map[string]string{
		"renew": "true", "claim-lease": "1m", "operation-timeout": "10m", "plane-timeout": "15s",
	}), nil)
	waitFor(t, "the completion", func() bool { return len(w.explorer.completions()) == 1 })
	if hits := w.target.count(); hits != 1 {
		t.Fatalf("target hits %d", hits)
	}
	var completion struct {
		Failed bool `json:"failed"`
	}
	_ = json.Unmarshal(w.explorer.completions()[0], &completion)
	if completion.Failed {
		t.Fatalf("completion failed: %s", w.explorer.completions()[0])
	}
	if !strings.Contains(run.stdout.String(), "at least 660") {
		t.Fatalf("grace for T = 10m: %s", run.stdout)
	}
	run.signals <- syscall.SIGTERM
	if code, ok := run.exit(20 * time.Second); !ok || code != ExitOK {
		t.Fatalf("exit %d", code)
	}
}

func TestGracePeriodCommand(t *testing.T) {
	for _, row := range []struct {
		args []string
		want string
	}{
		{nil, "225\n"},
		{[]string{"-operation-timeout", "10m", "-plane-timeout", "15s"}, "660\n"},
		{[]string{"-operation-timeout", "1m", "-plane-timeout", "500ms"}, "80\n"},
	} {
		stdout := &syncBuffer{}
		if code := Main(append([]string{"grace-period"}, row.args...), Env{Stdout: stdout}); code != ExitOK {
			t.Fatalf("%q: exit %d", row.args, code)
		}
		if stdout.String() != row.want {
			t.Fatalf("%q: %q, want %q", row.args, stdout, row.want)
		}
	}
	if code := Main([]string{"grace-period", "-plane-timeout", "0s"}, Env{}); code != ExitUsage {
		t.Fatalf("a zero plane timeout: exit %d", code)
	}
}

// TestAHardStopThatCannotWriteIsAFailure: exit 4 says every sent request is
// accounted for. With the unrecorded directory unwritable, the hard stop's
// write fails, so the command exits 1, not 4.
func TestAHardStopThatCannotWriteIsAFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes to a read-only directory")
	}
	w := newWorld(t)
	w.target.hold = true
	w.explorer.offerAction("act-unwritable")
	run := start(t, w.args(nil), nil)
	select {
	case <-w.target.entered:
	case <-time.After(20 * time.Second):
		t.Fatal("the target saw no request")
	}
	if err := os.Chmod(w.dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(w.dir, 0o700) })
	run.signals <- syscall.SIGTERM
	run.signals <- syscall.SIGINT
	code, ok := run.exit(10 * time.Second)
	if !ok {
		t.Fatal("the hard stop did not stop the gateway")
	}
	if code != ExitFailure || !strings.Contains(run.stderr.String(), "no record and in no log") {
		t.Fatalf("exit %d, want %d: %s", code, ExitFailure, run.stderr)
	}
}
