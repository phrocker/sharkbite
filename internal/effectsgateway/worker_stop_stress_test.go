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

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dispatchapi "github.com/phrocker/shoal-oss/pkg/dispatch/api"
)

// TestEveryEffectIsAccountedForWhateverTheStopTiming is the stop paths'
// invariant, tested by randomized timing rather than one window at a time:
// N actions run concurrently against a target that pauses at random before
// its effect and between the effect and its answer, and an explorer that
// pauses at random on every call; SIGTERM arrives at a random moment and a
// second signal (HardStop) at a random moment after it, often zero. However
// the stop lands — during the claim, the send or the completion — every
// effect the target performed must be accounted for: a completion recorded
// at the explorer, an ambiguity report it accepted, or an unrecorded-log
// entry.
//
// The check is "every effect is accounted for", not "requests equal
// accountings". The stop is deliberately conservative: a run abandoned while
// its completion is in flight is written to the log as outcome_unknown even
// if the completion then commits, so one effect can be accounted for twice.
// An effect accounted for nowhere is the failure.
//
// The test cannot pass vacuously: it counts, over the whole run, the effects
// performed, those accounted for by a recorded completion, and those
// accounted for only by an unrecorded-log entry, and requires all three to
// have occurred.
//
// What it cannot reach is the keep in handle's exit (an attempted, unsettled
// run stays in runs): with killed stored and runs snapshotted atomically, no
// stop it can schedule leaves a sent run unsettled outside the snapshot, so
// the two defences overlap. TestAnAttemptedRunLeavesRunsOnlyOnceSettled
// covers the keep directly.
//
// SHOAL_STOP_STRESS_ITERATIONS overrides the iteration count (500; 50 with
// -short). Each iteration logs its seed on failure.
func TestEveryEffectIsAccountedForWhateverTheStopTiming(t *testing.T) {
	iterations := 500
	if testing.Short() {
		iterations = 50
	}
	if raw := os.Getenv("SHOAL_STOP_STRESS_ITERATIONS"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			iterations = n
		}
	}
	base := uint64(time.Now().UnixNano())
	t.Cleanup(func() { stopYieldHook.Store(nil) })
	var totals stressTotals
	for i := 0; i < iterations; i++ {
		seed := base + uint64(i)
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) { stopStressIteration(t, seed, &totals) })
		if t.Failed() {
			return
		}
	}
	t.Logf("%d iterations: %d with an effect, %d effects; %d accounted for by a completion, "+
		"%d by an accepted report, %d by the unrecorded log alone",
		iterations, totals.iterationsWithEffect, totals.effects, totals.completed,
		totals.reported, totals.logOnly)
	if totals.effects == 0 || totals.completed == 0 || totals.logOnly == 0 {
		t.Fatalf("the run exercised too little to mean anything: %+v", totals)
	}
}

// stressTotals are counted across the run so the test cannot pass vacuously.
type stressTotals struct {
	iterationsWithEffect, effects, completed, reported, logOnly int
}

type lockedRand struct {
	mu sync.Mutex
	r  *rand.Rand
}

// delay is zero a third of the time, else up to 4ms.
func (l *lockedRand) delay() time.Duration { return l.upTo(4 * time.Millisecond) }

// upTo is zero a third of the time, else uniform up to max.
func (l *lockedRand) upTo(max time.Duration) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.r.IntN(3) == 0 {
		return 0
	}
	return time.Duration(l.r.Int64N(int64(max)))
}

func (l *lockedRand) intN(n int) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.r.IntN(n)
}

// pause waits d, or until ctx ends; it reports whether d elapsed.
func pause(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func stopStressIteration(t *testing.T, seed uint64, totals *stressTotals) {
	random := &lockedRand{r: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))}
	h := newWorkerHarness(t, nil)
	// Widen the windows between a stop's steps and the runs racing it: the
	// stop's own steps by up to 10ms, every other seam by up to 2ms.
	hook := func(point string) {
		if strings.HasPrefix(point, "hardstop:") {
			time.Sleep(random.upTo(10 * time.Millisecond))
			return
		}
		time.Sleep(random.upTo(2 * time.Millisecond))
	}
	stopYieldHook.Store(&hook)
	defer stopYieldHook.Store(nil)

	var mu sync.Mutex
	effects := map[string]bool{}
	var active atomic.Int32
	h.target.script = func(_ int, w http.ResponseWriter, r *http.Request) bool {
		active.Add(1)
		defer active.Add(-1)
		key := r.Header.Get("Idempotency-Key")
		if !pause(r.Context(), random.delay()) {
			return true // gone before the effect: nothing happened
		}
		mu.Lock()
		effects[key] = true
		mu.Unlock()
		// The effect has happened; the answer may be lost.
		_ = pause(r.Context(), random.delay())
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"ch_1"}`)
		return true
	}
	h.explorer.setGate(func(ctx context.Context, op string) error {
		limit := 4 * time.Millisecond
		if op == "claim" {
			// A claim in flight when the signals land is what races the stop.
			limit = 8 * time.Millisecond
		}
		if !pause(ctx, random.upTo(limit)) {
			return ctx.Err()
		}
		return nil
	})
	ids := make([]string, 1+random.intN(4))
	for i := range ids {
		ids[i] = fmt.Sprintf("a%d", i)
		h.explorer.enqueue(ids[i], "charge", chargeInput, workerEpoch.Add(time.Hour))
	}
	h.start()
	// SIGTERM anywhere from before the first pull to after the last answer;
	// the second signal at once, or a little later.
	// Biased late enough that most iterations reach the target: before it,
	// an iteration has no effect to account for.
	time.Sleep(5*time.Millisecond + random.upTo(15*time.Millisecond))
	switch random.intN(5) {
	case 0:
		h.cancel() // SIGTERM alone: the drain finishes the work
	case 1:
		// HardStop is the Worker's contract at any moment, not only after a
		// drain has begun: here it lands while the loop is still pulling
		// and claiming, and SIGTERM follows.
		stopped := make(chan struct{})
		go func() { h.worker.HardStop(); close(stopped) }()
		time.Sleep(random.upTo(6 * time.Millisecond))
		h.cancel()
		<-stopped
	case 2:
		// Both at once: the signal handler's two steps race, so the hard
		// stop may land before the drain has stopped the pulls.
		stopped := make(chan struct{})
		go func() { h.worker.HardStop(); close(stopped) }()
		h.cancel()
		<-stopped
	default:
		h.cancel() // SIGTERM, then the second signal
		time.Sleep(random.upTo(6 * time.Millisecond))
		h.worker.HardStop()
	}
	_ = h.runResult()
	// A target handler that has not yet noticed the cancellation may still
	// perform its effect: wait for every one to finish before counting.
	eventually(t, "the target's handlers to finish", func() bool { return active.Load() == 0 })

	entries := map[string]bool{}
	for _, entry := range h.log.Entries() {
		entries[string(entry.ActionID)] = true
	}
	hadEffect := false
	for _, id := range ids {
		mu.Lock()
		performed := effects[testExecutorKey(t, id).HeaderValue()]
		mu.Unlock()
		if !performed {
			continue
		}
		hadEffect = true
		totals.effects++
		record := h.explorer.record(id)
		completed := record.State == dispatchapi.DispatchSucceeded || record.State == dispatchapi.DispatchFailed
		reported := len(record.AmbiguityReports) > 0
		switch {
		case completed:
			totals.completed++
		case reported:
			totals.reported++
		case entries[id]:
			totals.logOnly++
		}
		if !completed && !reported && !entries[id] {
			t.Fatalf("seed %d: %s's effect happened and is accounted for nowhere: "+
				"record %s, no accepted report, no unrecorded entry\n%s",
				seed, id, record.State, h.logs)
		}
	}
	if hadEffect {
		totals.iterationsWithEffect++
	}
}
