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
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
	"time"

	dispatchapi "github.com/phrocker/shoal-oss/pkg/dispatch/api"
)

// The worker loop (#391):
//
//	PULL → FILTER → PRECHECK → [ATTEST] → CLAIM → BIND → SEND → CLASSIFY → COMPLETE
//
// with FENCE_LOST reachable from every state after CLAIM.
//
//   - FILTER: the record's agent, capability and action must be this
//     gateway's and have a route. The pull page has no server-side filter,
//     and a claim is what sets EffectPossible, so nothing else is claimed.
//   - PRECHECK uses only the pull page: for a key route, the action's
//     deadline − created_at must lie within the target's idempotency
//     retention; and the explorer's estimated now + T + ReportWindow must be
//     before the deadline. A refused record is skipped, never claimed.
//   - ATTEST: when the action requires attestation and the attestation in
//     hand expires before the explorer's now + L + ReportWindow, present
//     again before claiming.
//   - CLAIM: a fresh claim ID per attempt. A repull, 404 or 409 is a signal
//     to pull again, never "gone".
//   - BIND, SEND: the binder's pure request, through the egress-restricted
//     client, gated before every attempt by the send gate.
//   - COMPLETE: bound on the claim fence, with the effected volume on a
//     failure that may have left (#427).
//
// The lease is extended every L/2 from the anchor of the last claim or
// extension, re-attesting first if needed, and the local lease end is always
// the explorer's (clamped) end anchored on the local clock, never the lease
// requested. A refused extension, or the local lease end arriving, is
// FENCE_LOST: with nothing sent the worker reports request_not_sent and lets
// the claim lapse; with a request in flight it lets the request finish and
// reports effect_observed or outcome_unknown. At most one report is made per
// fence, and a report the explorer does not record goes to the unrecorded log.

// Dispatcher is the part of DispatchClient the worker uses.
type Dispatcher interface {
	Pull(ctx context.Context, request RequestContext, after string, limit int) (PullPage, error)
	Claim(ctx context.Context, actionID []byte, request ClaimRequest) (Action, error)
	Extend(ctx context.Context, actionID []byte, request ExtendRequest) (Action, error)
	Complete(ctx context.Context, actionID []byte, completion Completion) (Action, error)
	ReportAmbiguity(ctx context.Context, actionID []byte, report AmbiguityReport) (Action, error)
	PresentAttestation(ctx context.Context, statementFile, keyFile string) (time.Time, error)
}

var _ Dispatcher = (*DispatchClient)(nil)

// WorkerClock is the worker's local monotonic clock and its timers. Tests
// substitute one they advance by hand.
type WorkerClock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time                         { return time.Now() }
func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// NotReadyReason is why the worker is not ready. Closed.
type NotReadyReason string

const (
	NotReadyStarting       NotReadyReason = "starting"
	NotReadyDraining       NotReadyReason = "draining"
	NotReadyUnrecordedFull NotReadyReason = "unrecorded_log_full"
	NotReadyStopped        NotReadyReason = "stopped"
)

const (
	// DefaultMaxInFlight is how many claims a worker holds at once.
	DefaultMaxInFlight = 4
	// MaxBackoff caps the pull backoff and every retry delay.
	MaxBackoff = 30 * time.Second
	// DefaultMaxSendAttempts bounds attempts at one action's request.
	DefaultMaxSendAttempts = 5
	// targetBackoffBase is the first retry delay against the target when it
	// gave no Retry-After.
	targetBackoffBase = 500 * time.Millisecond
	// reportAttempts is how many times the worker calls ReportAmbiguity for
	// one report; the client resends once inside each call.
	reportAttempts = FallbackReportAttempts
)

// WorkerConfig is everything the loop needs. The command (PR6) builds it from
// Config; nothing here reads a flag.
type WorkerConfig struct {
	Dispatch Dispatcher
	// Target is the egress-restricted client (NewTargetClient).
	Target *http.Client
	Binder *Binder
	Routes *RouteTable
	// TargetAuthorization returns the credential header and value for one
	// request (Config.TargetAuthorization).
	TargetAuthorization func() (header, value string, err error)

	AgentID     []byte
	Capability  string
	SurfaceName string
	// Pod names this replica in its claim IDs.
	Pod string

	IdempotencyRetention time.Duration
	ClaimLease           time.Duration
	OperationTimeout     time.Duration
	PlaneTimeout         time.Duration
	MaxResponseBytes     int64
	PullLimit            int
	// PullInterval is the first backoff after an empty page or an error;
	// it doubles, with jitter, to MaxBackoff.
	PullInterval time.Duration
	// Renew selects the renewing send gate (Config.SendGate). The worker
	// extends at L/2 either way; without it the gate also requires the lease
	// to hold the whole operation and its report.
	Renew bool

	MaxInFlight     int
	MaxSendAttempts int
	// MaxRetryAfter caps a target's Retry-After; at most MaxBackoff.
	MaxRetryAfter time.Duration

	// RequiresAttestation reports whether an action of the gateway's
	// capability requires executor attestation (from the resolved
	// descriptor: AttestationRequirements).
	RequiresAttestation func(action string) bool
	// AttestationStatementFile and AttestationKeyFile are presented when an
	// action requires attestation.
	AttestationStatementFile string
	AttestationKeyFile       string

	// Unrecorded is the opened unrecorded log; its directory lock is the
	// single-replica guarantee, so the worker refuses to run without it.
	Unrecorded *UnrecordedLog
	Logger     *Logger
	Clock      WorkerClock
	Random     io.Reader
}

// AttestationRequirements reads, from a resolved descriptor, which of the
// capability's actions require attestation.
func AttestationRequirements(descriptor Descriptor, capability string) func(string) bool {
	required := map[string]bool{}
	for _, declared := range descriptor.Capabilities {
		if declared.Name != capability {
			continue
		}
		for _, action := range declared.Actions {
			if action.RequiresAttestation {
				required[action.Name] = true
			}
		}
	}
	return func(action string) bool { return required[action] }
}

// Worker runs the loop. Create with NewWorker, run with Run.
type Worker struct {
	cfg   WorkerConfig
	gate  SendGate
	clock WorkerClock
	log   *Logger

	slots chan struct{}

	readyMu  sync.Mutex
	notReady NotReadyReason

	draining atomic.Bool
	drainCh  chan struct{}
	drainMu  sync.Once

	hard   context.Context
	kill   context.CancelFunc
	killed atomic.Bool

	// attestSem serializes presentations; a waiter gives up when its
	// context ends, so a cancelled renewal never queues behind one.
	attestSem    chan struct{}
	attestExpiry time.Time

	serverMu     sync.Mutex
	serverHeader http.Header
	serverAt     time.Time

	skipMu  sync.Mutex
	skipped map[string]uint64

	// runs are the claims in hand, for abandonment.
	//
	// The invariant every stop path relies on: a run that has been
	// attempted — a request handed to the target client, so the effect may
	// have happened — leaves runs ONLY once it is settled, that is, its
	// outcome is recorded on the plane or written to the unrecorded log.
	// handle's exit keeps an attempted, unsettled run here; abandon removes
	// the runs it wrote. So a snapshot of runs taken at any moment contains
	// every effect not yet accounted for, whatever the run's goroutine is
	// doing, and no stop needs to win a race against it.
	runsMu sync.Mutex
	runs   map[*claimRun]struct{}
	// abandonMu serializes abandonment with Run's return. HardStop holds it
	// from storing killed to the end of its write; Run takes it last and
	// marks the worker finished, so a HardStop that saw killed finishes its
	// write before Run returns, and one that starts after Run has returned
	// does nothing.
	abandonMu sync.Mutex
	finished  bool
	// unwritten is set when an abandonment's write failed: an effect that
	// may have happened is on no record and in no log.
	unwritten atomic.Bool

	work sync.WaitGroup
}

// NewWorker validates the configuration.
func NewWorker(cfg WorkerConfig) (*Worker, error) {
	switch {
	case cfg.Dispatch == nil || cfg.Target == nil || cfg.Binder == nil || cfg.Routes == nil:
		return nil, errors.New("worker requires a dispatcher, a target client, a binder and routes")
	case cfg.TargetAuthorization == nil:
		return nil, errors.New("worker requires a target credential source")
	case cfg.Unrecorded == nil:
		return nil, errors.New("worker requires the unrecorded log, whose lock is the single-replica guarantee")
	case len(cfg.AgentID) == 0 || cfg.Capability == "" || cfg.Pod == "":
		return nil, errors.New("worker requires an agent ID, a capability and a pod name")
	case len(cfg.SurfaceName) > dispatchapi.MaxAmbiguityTargetBytes || !printableAmbiguityText(cfg.SurfaceName):
		return nil, errors.New("surface name must be printable and fit an ambiguity report's target")
	case cfg.ClaimLease <= 0 || cfg.ClaimLease > dispatchapi.MaxActionClaimTTL:
		return nil, errors.New("claim lease must be positive and at most " + dispatchapi.MaxActionClaimTTL.String())
	case cfg.OperationTimeout <= 0 || cfg.PlaneTimeout <= 0:
		return nil, errors.New("operation and plane timeouts must be positive")
	case cfg.MaxResponseBytes <= 0:
		return nil, errors.New("max response bytes must be positive")
	case cfg.PullLimit <= 0 || cfg.PullLimit > dispatchapi.MaxDispatchListResults:
		return nil, errors.New("pull limit is outside its bound")
	case cfg.PullInterval <= 0:
		return nil, errors.New("pull interval must be positive")
	}
	if cfg.Routes.RequiresKey() && cfg.IdempotencyRetention <= 0 {
		return nil, errors.New("key routes require the idempotency retention")
	}
	if _, _, err := NewClaimID(cfg.Pod, bytes.NewReader(make([]byte, ClaimNonceBytes))); err != nil {
		return nil, err
	}
	if cfg.MaxInFlight <= 0 {
		cfg.MaxInFlight = DefaultMaxInFlight
	}
	if cfg.MaxSendAttempts <= 0 {
		cfg.MaxSendAttempts = DefaultMaxSendAttempts
	}
	if cfg.MaxRetryAfter <= 0 || cfg.MaxRetryAfter > MaxBackoff {
		cfg.MaxRetryAfter = MaxBackoff
	}
	if cfg.RequiresAttestation == nil {
		cfg.RequiresAttestation = func(string) bool { return false }
	}
	if cfg.Clock == nil {
		cfg.Clock = systemClock{}
	}
	if cfg.Random == nil {
		cfg.Random = rand.Reader
	}
	w := &Worker{
		cfg: cfg, clock: cfg.Clock, log: cfg.Logger,
		gate: SendGate{
			OperationTimeout: cfg.OperationTimeout, Renew: cfg.Renew,
			RenewAfter: cfg.ClaimLease / 2,
		},
		slots: make(chan struct{}, cfg.MaxInFlight), notReady: NotReadyStarting,
		drainCh: make(chan struct{}), skipped: map[string]uint64{},
		runs: map[*claimRun]struct{}{}, attestSem: make(chan struct{}, 1),
	}
	w.hard, w.kill = context.WithCancel(context.Background())
	return w, nil
}

// Ready reports readiness, and why not.
func (w *Worker) Ready() (bool, NotReadyReason) {
	w.readyMu.Lock()
	defer w.readyMu.Unlock()
	return w.notReady == "", w.notReady
}

// UnrecordedEntries is the gauge: reports awaiting reconciliation.
func (w *Worker) UnrecordedEntries() int { return w.cfg.Unrecorded.Len() }

// InFlight is how many claims the worker holds: claimed and not yet
// completed, reported or abandoned.
func (w *Worker) InFlight() int {
	w.runsMu.Lock()
	defer w.runsMu.Unlock()
	return len(w.runs)
}

func (w *Worker) setReady(reason NotReadyReason) {
	w.readyMu.Lock()
	changed := w.notReady != reason
	// Draining and stopped are final.
	if w.notReady == NotReadyDraining && reason != NotReadyStopped {
		changed = false
	}
	if changed {
		w.notReady = reason
	}
	w.readyMu.Unlock()
	if changed {
		w.log.Log(LogRecord{Event: EventReadiness, NotReady: reason})
	}
}

// Kill abandons everything at once, as a crash would: in-flight requests are
// cancelled and nothing more is reported or written. It is SIGKILL, for tests
// of crash recovery; a process that can still act uses HardStop.
func (w *Worker) Kill() {
	w.killed.Store(true)
	w.kill()
}

// HardStop is the second signal: it stops at once, without draining, and
// does what a drain that runs out does (abandon). Nothing more is claimed or
// sent, every run is marked abandoned, everything in flight is cancelled, and
// then every run whose request may have reached the target, and whose outcome
// is not yet on the record or in the log, goes to the unrecorded log as
// outcome_unknown in one durable rewrite. It returns how many runs were
// unfinished. The write is bounded only by the disk: a stuck fsync holds it.
func (w *Worker) HardStop() int {
	w.abandonMu.Lock()
	defer w.abandonMu.Unlock()
	if w.finished {
		// Run has returned: by the runs invariant nothing attempted is
		// unaccounted for, and the caller may have closed the log.
		return 0
	}
	// killed is stored and runs snapshotted under both locks. A claim
	// registers under runsMu only if not killed, and a run sends only if not
	// killed or abandoned (attempt), so the snapshot holds every run that
	// can ever have sent; and by the invariant on runs, one that has sent
	// and not settled is still there whatever its goroutine has done since.
	w.runsMu.Lock()
	w.killed.Store(true)
	stopYield("hardstop:killed")
	runs := w.snapshotLocked()
	w.runsMu.Unlock()
	stopYield("hardstop:snapshot")
	return w.abandonRuns(runs)
}

// stopYield is a scheduling seam for the stop-timing stress test, which
// sleeps at random here to widen the windows between a stop's steps and the
// runs racing it. It does nothing in production.
var stopYieldHook atomic.Pointer[func(string)]

func stopYield(point string) {
	if hook := stopYieldHook.Load(); hook != nil {
		(*hook)(point)
	}
}

func (w *Worker) snapshotLocked() []*claimRun {
	runs := make([]*claimRun, 0, len(w.runs))
	for run := range w.runs {
		runs = append(runs, run)
	}
	return runs
}

// Run retries the unrecorded log, then pulls and works actions until ctx is
// done, then drains: it stops pulling and goes not-ready; claims with nothing
// sent are reported request_not_sent and lapse; requests in flight finish and
// complete, falling back to an ambiguity report and then to the unrecorded
// log. The drain is bounded by GracePeriod(T, planeTimeout); a run still
// unfinished when it ends is abandoned (ErrDrainAbandoned), and one whose
// request may have reached the target is written to the unrecorded log first.
// Run returns when the drain ends or Kill is called.
func (w *Worker) Run(ctx context.Context) error {
	defer w.kill()
	// Deferred after kill, so it runs first: an abandonment under way (a
	// HardStop from another goroutine) finishes its write before Run returns.
	defer w.finish()
	// The start-up retry stops on SIGTERM as well as on Kill; whatever it
	// did not reach stays on disk for the next start.
	retryCtx, stopRetry := context.WithCancel(ctx)
	unhook := context.AfterFunc(w.hard, stopRetry)
	w.RetryUnrecorded(retryCtx)
	unhook()
	stopRetry()
	if w.killed.Load() {
		return errors.New("worker killed")
	}
	w.refreshReadiness()
	w.pullLoop(ctx)
	abandoned := w.drain()
	// Wait out any abandonment under way before deciding the outcome: its
	// write is part of it.
	w.finish()
	// Ahead of every other outcome, a hard stop included: whatever else
	// happened, an effect is on no record and in no log.
	if w.unwritten.Load() {
		return ErrUnrecordedUnwritten
	}
	if w.killed.Load() {
		return errors.New("worker killed")
	}
	if w.unaccounted() > 0 {
		return ErrUnrecordedUnwritten
	}
	if abandoned > 0 {
		return ErrDrainAbandoned
	}
	return nil
}

// finish waits for an abandonment under way (a HardStop from another
// goroutine) to end, and marks the worker finished, so a later HardStop does
// nothing. It is idempotent.
func (w *Worker) finish() {
	w.abandonMu.Lock()
	w.finished = true
	w.abandonMu.Unlock()
}

// ErrUnrecordedUnwritten says the drain ended with an outcome that could not
// be written to the unrecorded log even on the retry: it is in the process's
// logs (the dispatch_error event) and nowhere durable.
var ErrUnrecordedUnwritten = errors.New("an outcome could not be written to the unrecorded log")

// unaccounted is how many runs in hand were attempted and are not settled:
// effects that may have happened and are on no record and in no log.
func (w *Worker) unaccounted() int {
	w.runsMu.Lock()
	defer w.runsMu.Unlock()
	n := 0
	for run := range w.runs {
		run.mu.Lock()
		if run.attempted && !run.settled {
			n++
		}
		run.mu.Unlock()
	}
	return n
}

// withTimeout is context.WithTimeout on the worker's clock, so every bound
// the worker spends is measured by the same clock GracePeriod is budgeted on.
func (w *Worker) withTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if _, system := w.clock.(systemClock); system {
		return context.WithTimeout(parent, d)
	}
	ctx, cancel := context.WithCancel(parent)
	expired := w.clock.After(d)
	go func() {
		select {
		case <-expired:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

// extensionMayHaveApplied: an Extend answered by anything but a definite
// refusal may have moved the lease.
func extensionMayHaveApplied(err error) bool {
	switch DispatchKind(err) {
	case DispatchFenceLost, DispatchInvalid, DispatchUnauthorized,
		DispatchRefusedLocal, DispatchNoCredential:
		return false
	}
	return true
}

// ErrDrainAbandoned says the grace period ended with runs unfinished. Each
// one whose request may have reached the target is in the unrecorded log.
var ErrDrainAbandoned = errors.New("the drain ended with work unfinished")

func (w *Worker) refreshReadiness() {
	if w.draining.Load() {
		return
	}
	if w.cfg.Unrecorded.Full() {
		w.setReady(NotReadyUnrecordedFull)
		return
	}
	w.setReady("")
}

// drain stops pulling and waits for the runs in hand, for at most the grace
// period. It returns how many it abandoned.
func (w *Worker) drain() int {
	w.draining.Store(true)
	w.drainMu.Do(func() { close(w.drainCh) })
	w.setReady(NotReadyDraining)
	done := make(chan struct{})
	go func() {
		w.work.Wait()
		close(done)
	}()
	abandoned := 0
	select {
	case <-done:
		// Every run has finished, but one whose report could not be written
		// is still held (the invariant on runs): retry its entry now, once,
		// rather than return as if nothing were left only in memory.
		if w.unaccounted() > 0 {
			w.abandon()
		}
	case <-w.clock.After(DrainBound(w.cfg.OperationTimeout, w.cfg.PlaneTimeout)):
		// The grace period's exit margin is all that is left before the
		// kubelet's SIGKILL, and it belongs to this: nothing that may have
		// happened is left only in memory.
		abandoned = w.abandon()
	case <-w.hard.Done():
	}
	w.setReady(NotReadyStopped)
	return abandoned
}

// abandon gives up on every unfinished run: each one whose request may have
// reached the target, and whose outcome is not yet on the record or in the
// log, is written to the unrecorded log as outcome_unknown. Then everything
// still running is cancelled. It returns how many runs were unfinished.
//
// It is the one abandonment path: the drain bound running out and HardStop
// both come here. A run already abandoned is not written twice.
func (w *Worker) abandon() int {
	w.abandonMu.Lock()
	defer w.abandonMu.Unlock()
	w.runsMu.Lock()
	runs := w.snapshotLocked()
	w.runsMu.Unlock()
	return w.abandonRuns(runs)
}

// abandonRuns does the work of abandon on a snapshot. The caller holds
// abandonMu.
func (w *Worker) abandonRuns(runs []*claimRun) int {
	// Mark every run first: from here none sends (attempt refuses an
	// abandoned run) and none reports.
	var written []*claimRun
	var entries []UnrecordedEntry
	for _, run := range runs {
		run.mu.Lock()
		write := run.attempted && !run.settled && !run.abandoned
		run.abandoned, run.reported = true, true
		pending := run.pending
		run.mu.Unlock()
		if write {
			written = append(written, run)
			if pending != nil {
				// The report's own entry, which a failed write left here.
				entries = append(entries, *pending)
				continue
			}
			entries = append(entries, run.unrecorded(w.cfg.SurfaceName,
				dispatchapi.AmbiguityOutcomeUnknown, "", nil))
		}
	}
	// Then cancel, so requests in flight stop now rather than after the
	// write. Cancelling does not unsend: every run that may have reached
	// the target is still written below.
	w.kill()
	// One durable rewrite for all of them: the exit margin is short. It is
	// bounded by the disk; a stuck fsync holds it, and nothing short of
	// dropping the entries could do better.
	err := w.cfg.Unrecorded.AppendAll(entries)
	if err != nil && len(entries) > 0 {
		w.unwritten.Store(true)
	}
	for _, run := range written {
		record := w.record(run, EventUnrecorded)
		record.Ambiguity = dispatchapi.AmbiguityOutcomeUnknown
		if err != nil {
			record.Event = EventDispatch
		}
		record.Unrecorded = w.cfg.Unrecorded.Len()
		w.log.Log(record)
	}
	if err == nil {
		// Written, so settled: they may leave runs now.
		w.runsMu.Lock()
		for _, run := range written {
			run.settle()
			delete(w.runs, run)
		}
		w.runsMu.Unlock()
	}
	w.log.Log(LogRecord{Event: EventAbandoned, Abandoned: len(runs),
		Unrecorded: w.cfg.Unrecorded.Len()})
	return len(runs)
}

// RetryUnrecorded presents every held report again. One the explorer now
// records — including an identical replay (#542) — leaves the log; the rest
// stay until acknowledged.
func (w *Worker) RetryUnrecorded(ctx context.Context) {
	var failed []UnrecordedEntry
	for _, entry := range w.cfg.Unrecorded.Entries() {
		if ctx.Err() != nil {
			break
		}
		request, err := w.requestContext("gateway_ambiguity_retry")
		if err != nil {
			return
		}
		callCtx, cancel := w.withTimeout(ctx, w.cfg.PlaneTimeout)
		_, err = w.cfg.Dispatch.ReportAmbiguity(callCtx, entry.ActionID, entry.Report(request))
		cancel()
		record := LogRecord{ActionID: entry.ActionID, Fence: entry.Fence,
			ClaimNonce: entry.ClaimNonce, Ambiguity: entry.Outcome}
		if err == nil {
			if removed, removeErr := w.cfg.Unrecorded.cleared(entry.ActionID, entry.Fence); removeErr == nil && removed {
				record.Event, record.Unrecorded = EventUnrecordedCleared, w.cfg.Unrecorded.Len()
				w.log.Log(record)
			}
			continue
		}
		entry.Status, entry.DispatchError = dispatchStatus(err), DispatchKind(err)
		failed = append(failed, entry)
		record.Event, record.DispatchError = EventUnrecorded, DispatchKind(err)
		record.Unrecorded = w.cfg.Unrecorded.Len()
		w.log.Log(record)
	}
	// One durable write for every attempt noted, not one per entry.
	if err := w.cfg.Unrecorded.recordAttempts(failed); err != nil {
		w.log.Log(LogRecord{Event: EventDispatch, Unrecorded: w.cfg.Unrecorded.Len()})
	}
}

// AckUnrecorded is the explicit clearing API.
func (w *Worker) AckUnrecorded(actionID []byte, fence uint64) (bool, error) {
	removed, err := w.cfg.Unrecorded.Ack(actionID, fence)
	if err == nil && removed {
		w.log.Log(LogRecord{Event: EventUnrecordedCleared, ActionID: actionID, Fence: fence,
			Unrecorded: w.cfg.Unrecorded.Len()})
		w.refreshReadiness()
	}
	return removed, err
}

func dispatchStatus(err error) int {
	var dispatchErr *DispatchError
	if errors.As(err, &dispatchErr) {
		return dispatchErr.Status
	}
	return 0
}

// ---- PULL, FILTER, PRECHECK, CLAIM ----

// backoff is jittered exponential: each delay is drawn from [d/2, d], and d
// doubles from base to MaxBackoff.
type backoff struct {
	base, current time.Duration
	random        io.Reader
}

func (b *backoff) next() time.Duration {
	if b.current <= 0 {
		b.current = b.base
	} else {
		b.current *= 2
	}
	if b.current > MaxBackoff {
		b.current = MaxBackoff
	}
	return jitter(b.current, b.random)
}

func (b *backoff) reset() { b.current = 0 }

func jitter(d time.Duration, random io.Reader) time.Duration {
	if d <= 1 {
		return d
	}
	var raw [8]byte
	if _, err := io.ReadFull(random, raw[:]); err != nil {
		return d
	}
	half := d / 2
	return half + time.Duration(binary.BigEndian.Uint64(raw[:])%uint64(d-half+1))
}

// ticket is a held slot and a held unrecorded-log reservation.
type ticket struct{ held bool }

func (w *Worker) acquire(ctx context.Context) (*ticket, bool) {
	select {
	case w.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, false
	case <-w.hard.Done():
		return nil, false
	}
	if !w.cfg.Unrecorded.Reserve() {
		<-w.slots
		w.setReady(NotReadyUnrecordedFull)
		return nil, true
	}
	w.refreshReadiness()
	return &ticket{held: true}, true
}

func (w *Worker) tryAcquire() *ticket {
	select {
	case w.slots <- struct{}{}:
	default:
		return nil
	}
	if !w.cfg.Unrecorded.Reserve() {
		<-w.slots
		w.setReady(NotReadyUnrecordedFull)
		return nil
	}
	return &ticket{held: true}
}

func (w *Worker) release(t *ticket) {
	if t == nil || !t.held {
		return
	}
	t.held = false
	w.cfg.Unrecorded.Release()
	<-w.slots
}

func (w *Worker) wait(ctx context.Context, d time.Duration) bool {
	select {
	case <-w.clock.After(d):
		return true
	case <-ctx.Done():
		return false
	case <-w.hard.Done():
		return false
	}
}

func (w *Worker) pullLoop(ctx context.Context) {
	pause := &backoff{base: w.cfg.PullInterval, random: w.cfg.Random}
	cursor := ""
	for ctx.Err() == nil && !w.killed.Load() {
		held, ok := w.acquire(ctx)
		if !ok {
			return
		}
		if held == nil {
			// The unrecorded log cannot promise room for another report:
			// stop claiming until an entry clears.
			if !w.wait(ctx, pause.next()) {
				return
			}
			continue
		}
		claimed, next, err := w.pullOnce(ctx, held, cursor)
		cursor = ""
		switch {
		case err != nil:
			if !w.wait(ctx, pause.next()) {
				return
			}
		case claimed:
			pause.reset()
		case next != "":
			// Nothing claimable on this page; the next one may have some.
			cursor = next
		default:
			if !w.wait(ctx, pause.next()) {
				return
			}
		}
	}
}

// pullOnce pulls one page and claims what it can. It consumes held: the
// ticket goes to a claimed action or is released. It returns whether
// anything was claimed and the cursor to continue with when the page offered
// nothing claimable.
func (w *Worker) pullOnce(ctx context.Context, held *ticket, cursor string) (bool, string, error) {
	defer func() { w.release(held) }()
	if w.killed.Load() {
		// Stopped: nothing more is pulled or claimed.
		return false, "", errors.New("worker stopped")
	}
	request, err := w.requestContext("gateway_pull")
	if err != nil {
		return false, "", err
	}
	callCtx, cancel := w.withTimeout(ctx, w.cfg.PlaneTimeout)
	page, err := w.cfg.Dispatch.Pull(callCtx, request, cursor, w.cfg.PullLimit)
	cancel()
	if err != nil {
		w.log.Log(LogRecord{Event: EventDispatch, DispatchError: DispatchKind(err)})
		return false, "", err
	}
	w.noteServerTime(page.Header, page.ReceivedAt)
	claimedAny := false
	for _, offered := range page.Actions {
		if ctx.Err() != nil || w.draining.Load() {
			break
		}
		route, ok := w.filter(offered)
		if !ok {
			continue
		}
		if refusal := w.precheck(offered, route); refusal != GateOpen {
			w.logSkip(offered, route, refusal)
			continue
		}
		if held == nil {
			if held = w.tryAcquire(); held == nil {
				break
			}
		}
		if w.killed.Load() {
			// Stopped: nothing more is claimed.
			break
		}
		if w.cfg.RequiresAttestation(offered.Action) {
			if err := w.ensureAttestation(ctx, false); err != nil {
				return claimedAny, "", err
			}
		}
		run, err := w.claim(ctx, offered, route)
		if err != nil {
			// Repull, 404 and 409 all mean "pull again"; anything else is
			// logged by claim and backs off.
			if isRepull(err) {
				return claimedAny, "", nil
			}
			return claimedAny, "", err
		}
		stopYield("pull:claimed")
		// Registered under runsMu with the killed check, so a HardStop's
		// snapshot either includes this run or this run is never registered.
		// An unregistered claim has sent nothing, and lapses.
		w.runsMu.Lock()
		if w.killed.Load() {
			w.runsMu.Unlock()
			run.renewCancel()
			return claimedAny, "", errors.New("worker stopped")
		}
		claimedAny = true
		run.ticket, held = held, nil
		w.runs[run] = struct{}{}
		w.work.Add(1)
		w.runsMu.Unlock()
		go w.handle(run)
	}
	if claimedAny {
		return true, "", nil
	}
	return false, page.Next, nil
}

func isRepull(err error) bool {
	switch DispatchKind(err) {
	case DispatchRepull, DispatchNotFound, DispatchConflict:
		return true
	}
	return false
}

func (w *Worker) filter(offered Action) (*Route, bool) {
	if !bytes.Equal(offered.AgentID, w.cfg.AgentID) || offered.Capability != w.cfg.Capability {
		return nil, false
	}
	return w.cfg.Routes.Lookup(offered.Action)
}

func (w *Worker) precheck(offered Action, route *Route) GateRefusal {
	if route.Idempotency() == IdempotencyKey &&
		!RetentionCovers(offered.CreatedAt, offered.Deadline, w.cfg.IdempotencyRetention) {
		return GateRetention
	}
	now, err := w.serverNow()
	if err != nil || !DeadlineAdmits(now, offered.Deadline, w.cfg.OperationTimeout) {
		return GateDeadline
	}
	return GateOpen
}

// logSkip logs a skipped record once per version, so a record that stays on
// the page is not logged on every poll.
func (w *Worker) logSkip(offered Action, route *Route, refusal GateRefusal) {
	w.skipMu.Lock()
	key := string(offered.ID)
	seen, ok := w.skipped[key]
	if !ok || seen != offered.Version {
		if len(w.skipped) >= 4096 {
			w.skipped = map[string]uint64{}
		}
		w.skipped[key] = offered.Version
	}
	w.skipMu.Unlock()
	if !ok || seen != offered.Version {
		w.log.Log(LogRecord{Event: EventSkipped, ActionID: offered.ID, Route: route, Gate: refusal})
	}
}

func (w *Worker) noteServerTime(header http.Header, receivedAt time.Time) {
	if header == nil || header.Get("Date") == "" {
		return
	}
	w.serverMu.Lock()
	w.serverHeader, w.serverAt = header.Clone(), receivedAt
	w.serverMu.Unlock()
}

func (w *Worker) serverNow() (time.Time, error) {
	w.serverMu.Lock()
	header, at := w.serverHeader, w.serverAt
	w.serverMu.Unlock()
	if header == nil {
		return time.Time{}, errors.New("no explorer Date header has been seen")
	}
	return ServerNow(header, at, w.clock.Now())
}

// ensureAttestation presents the attestation unless the one in hand
// outlives the explorer's now + L + ReportWindow.
func (w *Worker) ensureAttestation(ctx context.Context, force bool) error {
	select {
	case w.attestSem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-w.attestSem }()
	now, err := w.serverNow()
	if err != nil {
		return err
	}
	needed := now.Add(w.cfg.ClaimLease + ReportWindow)
	if !force && !w.attestExpiry.IsZero() && !w.attestExpiry.Before(needed) {
		return nil
	}
	callCtx, cancel := w.withTimeout(ctx, w.cfg.PlaneTimeout)
	expires, err := w.cfg.Dispatch.PresentAttestation(callCtx,
		w.cfg.AttestationStatementFile, w.cfg.AttestationKeyFile)
	cancel()
	if err != nil {
		w.log.Log(LogRecord{Event: EventDispatch, DispatchError: DispatchKind(err)})
		return err
	}
	w.attestExpiry = expires
	w.log.Log(LogRecord{Event: EventAttested})
	if expires.Before(needed) {
		return errors.New("the attestation expires before a lease would end")
	}
	return nil
}

func (w *Worker) forgetAttestation() {
	w.attestSem <- struct{}{}
	w.attestExpiry = time.Time{}
	<-w.attestSem
}

func (w *Worker) requestContext(reason string) (RequestContext, error) {
	id, err := NewRequestID(w.cfg.Random)
	if err != nil {
		return RequestContext{}, err
	}
	return RequestContext{
		RequestID: id, ReasonCode: reason,
		Deadline: w.clock.Now().Add(w.cfg.PlaneTimeout).UTC(),
	}, nil
}

// claimRun is one claim attempt, from CLAIM to its last report.
type claimRun struct {
	action  Action
	route   *Route
	claimID []byte
	nonce   ClaimNonce
	ticket  *ticket

	mu       sync.Mutex
	anchored Anchored
	anchorAt time.Time
	// extensionUnknown: some renewal's outcome was never read, so the
	// explorer may hold a lease end the worker never saw.
	extensionUnknown bool
	lost             bool
	lostCh           chan struct{}
	inFlight         bool
	reported         bool
	// attempted: a request was handed to the target client, so the effect
	// may have happened. settled: the outcome is on the record or in the
	// unrecorded log. abandoned: the drain gave up on this run.
	attempted bool
	settled   bool
	abandoned bool
	// pending is the entry a report could not write, for abandon to retry.
	pending   *UnrecordedEntry
	stopRenew chan struct{}
	renewDone chan struct{}
	// renewCtx bounds every call the renewal makes; stopping the renewal
	// cancels it first, so the stop never waits on a call in flight.
	renewCtx    context.Context
	renewCancel context.CancelFunc
}

func (r *claimRun) markLost() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.lost {
		r.lost = true
		close(r.lostCh)
	}
}

func (r *claimRun) settle() {
	r.mu.Lock()
	r.settled = true
	r.mu.Unlock()
}

// unrecorded is this run's entry in the unrecorded log; err is the report's
// last failure, nil when no report was attempted.
func (r *claimRun) unrecorded(target string, outcome dispatchapi.AmbiguityOutcome, ref string, err error) UnrecordedEntry {
	return UnrecordedEntry{
		ActionID: r.action.ID, Fence: r.action.ClaimFence, ClaimNonce: r.nonce,
		RouteAction: r.route.Action(), Method: r.route.Method(),
		PathTemplate: r.route.PathTemplate(), Outcome: outcome,
		Target: target, Reference: ref, CorrelationID: r.action.CorrelationID,
		Status: dispatchStatus(err), DispatchError: DispatchKind(err),
	}
}

// settleAt is when whatever this claim had in flight has settled: the end of
// the lease as anchored — or, when a renewal's outcome was never read, the
// latest local instant of the action's deadline (DeadlineLocalLatest), since
// that renewal may have granted any end up to the deadline and nothing on the
// worker's side bounds when the explorer applied it. A request deadline is
// not such a bound: it is a local wall reading the explorer compares with its
// own clock, and a write already under way can commit after it.
func (r *claimRun) settleAt() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.extensionUnknown && r.anchored.DeadlineLocalLatest.After(r.anchored.LeaseLocal) {
		return r.anchored.DeadlineLocalLatest
	}
	return r.anchored.LeaseLocal
}

func (r *claimRun) isLost() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lost
}

func (r *claimRun) anchor() (Anchored, time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.anchored, r.anchorAt
}

func (w *Worker) claim(ctx context.Context, offered Action, route *Route) (*claimRun, error) {
	claimID, nonce, err := NewClaimID(w.cfg.Pod, w.cfg.Random)
	if err != nil {
		return nil, err
	}
	request, err := w.requestContext("gateway_claim")
	if err != nil {
		return nil, err
	}
	sent := w.clock.Now()
	callCtx, cancel := w.withTimeout(ctx, w.cfg.PlaneTimeout)
	claimed, err := w.cfg.Dispatch.Claim(callCtx, offered.ID, ClaimRequest{
		Context: offered.Correlate(request), ExpectedVersion: offered.Version,
		ClaimID: claimID, Lease: w.cfg.ClaimLease,
	})
	cancel()
	if err != nil {
		if DispatchKind(err) == DispatchConflict && w.cfg.RequiresAttestation(offered.Action) {
			// The attestation gate answers 409 too; present afresh next time.
			w.forgetAttestation()
		}
		w.log.Log(LogRecord{Event: EventDispatch, ActionID: offered.ID, ClaimNonce: nonce,
			Route: route, DispatchError: DispatchKind(err)})
		return nil, err
	}
	run := &claimRun{
		action: claimed, route: route, claimID: claimID, nonce: nonce,
		lostCh: make(chan struct{}), stopRenew: make(chan struct{}),
		renewDone: make(chan struct{}),
	}
	run.renewCtx, run.renewCancel = context.WithCancel(w.hard)
	if len(run.action.CorrelationID) == 0 {
		run.action.CorrelationID = append([]byte(nil), offered.CorrelationID...)
	}
	anchored, err := AnchorAnswer(sent, w.clock.Now(), claimed.ClaimTimes())
	if err != nil {
		// A claim whose bounds cannot be placed on the local clock is held
		// for an unknown time: treat it as already lost.
		run.anchored = Anchored{LeaseLocal: sent, DeadlineLocal: sent}
		run.markLost()
	} else {
		run.anchored = anchored
	}
	run.anchorAt = sent
	w.log.Log(w.record(run, EventClaimed))
	return run, nil
}

func (w *Worker) record(run *claimRun, event Event) LogRecord {
	return LogRecord{Event: event, ActionID: run.action.ID, Fence: run.action.ClaimFence,
		ClaimNonce: run.nonce, Route: run.route}
}

// ---- EXTEND ----

// renew extends the lease every L/2 from the last anchor until stopped. A
// refusal, or the local lease end arriving first, marks the claim lost.
func (w *Worker) renew(run *claimRun) {
	defer close(run.renewDone)
	attestRetried := false
	retryAt := time.Time{}
	for {
		anchored, anchorAt := run.anchor()
		if run.isLost() {
			return
		}
		next := anchorAt.Add(w.cfg.ClaimLease / 2)
		if !retryAt.IsZero() {
			next = retryAt
		}
		// A lease the explorer clamped to the deadline cannot be extended:
		// wait for its end.
		atDeadline := !anchored.LeaseLocal.Before(anchored.DeadlineLocal)
		if atDeadline || !next.Before(anchored.LeaseLocal) {
			next = anchored.LeaseLocal
		}
		if delay := next.Sub(w.clock.Now()); delay > 0 {
			select {
			case <-w.clock.After(delay):
			case <-run.stopRenew:
				return
			case <-run.renewCtx.Done():
				return
			}
		}
		select {
		case <-run.stopRenew:
			return
		default:
		}
		now := w.clock.Now()
		if !now.Before(anchored.LeaseLocal) {
			w.log.Log(LogRecord{Event: EventFenceLost, ActionID: run.action.ID,
				Fence: run.action.ClaimFence, ClaimNonce: run.nonce, Route: run.route, Gate: GateLease})
			run.markLost()
			return
		}
		if atDeadline {
			continue
		}
		if w.cfg.RequiresAttestation(run.action.Action) {
			// Best effort: a refused presentation shows up as the
			// extension's own 409.
			_ = w.ensureAttestation(run.renewCtx, false)
		}
		_, err := w.extend(run)
		switch {
		case err == nil:
			retryAt, attestRetried = time.Time{}, false
			w.log.Log(w.record(run, EventExtended))
		case DispatchKind(err) == DispatchFenceLost:
			if dispatchStatus(err) == http.StatusConflict &&
				w.cfg.RequiresAttestation(run.action.Action) && !attestRetried {
				attestRetried = true
				_ = w.ensureAttestation(run.renewCtx, true)
				retryAt = w.clock.Now()
				continue
			}
			record := w.record(run, EventFenceLost)
			record.DispatchError, record.Status = DispatchFenceLost, dispatchStatus(err)
			w.log.Log(record)
			run.markLost()
			return
		case run.renewCtx.Err() != nil:
			// Stopped: the extension's outcome is recorded (extensionUnknown);
			// the cancellation itself is not a dispatch failure.
			return
		default:
			// Indeterminate or unreachable: keep the lease end already held
			// and try again shortly; the local lease end still bounds it.
			record := w.record(run, EventDispatch)
			record.DispatchError = DispatchKind(err)
			w.log.Log(record)
			retryAt = w.clock.Now().Add(w.cfg.PlaneTimeout)
		}
	}
}

// extend sends one renewal and re-anchors on the explorer's granted end.
func (w *Worker) extend(run *claimRun) (Action, error) {
	request, err := w.requestContext("gateway_extend")
	if err != nil {
		return Action{}, err
	}
	sent := w.clock.Now()
	callCtx, cancel := w.withTimeout(run.renewCtx, w.cfg.PlaneTimeout)
	extended, err := w.cfg.Dispatch.Extend(callCtx, run.action.ID, ExtendRequest{
		Context: run.action.Correlate(request), ClaimID: run.claimID,
		ClaimFence: run.action.ClaimFence, Lease: w.cfg.ClaimLease,
	})
	cancel()
	if err != nil {
		if extensionMayHaveApplied(err) {
			run.mu.Lock()
			run.extensionUnknown = true
			run.mu.Unlock()
		}
		return Action{}, err
	}
	// The explorer's end, clamped to the deadline — never sent + L.
	anchored, anchorErr := AnchorAnswer(sent, w.clock.Now(), extended.ClaimTimes())
	if anchorErr != nil {
		return Action{}, &DispatchError{Op: "extend", Kind: DispatchProtocol,
			reason: "extend response cannot be anchored"}
	}
	// Adopted as granted, even if it ends earlier than the end already held:
	// the explorer's answer is the lease.
	run.mu.Lock()
	// Every answer gives a valid late bound on the deadline; keep the
	// tightest.
	if previous := run.anchored.DeadlineLocalLatest; !previous.IsZero() &&
		previous.Before(anchored.DeadlineLocalLatest) {
		anchored.DeadlineLocalLatest = previous
	}
	run.anchored = anchored
	run.anchorAt = sent
	if extended.Version > run.action.Version {
		run.action.Version = extended.Version
	}
	run.mu.Unlock()
	return extended, nil
}

// ---- BIND, SEND, CLASSIFY, COMPLETE ----

// sendResult is what SEND ends with.
type sendResult struct {
	// sent is false when no attempt may have reached the target.
	sent bool
	// stopped is why nothing (more) was attempted, when the loop ended
	// without a final classification.
	stopped GateRefusal
	lost    bool
	class   Classification
	final   bool
	bytes   int64
	chunks  int64
}

func (w *Worker) handle(run *claimRun) {
	defer w.work.Done()
	defer w.release(run.ticket)
	defer func() {
		// The invariant on runs: an attempted run leaves only once settled.
		// One that ends unsettled (stopped, or its report could not be
		// written) stays for abandon to account for.
		w.runsMu.Lock()
		run.mu.Lock()
		keep := run.attempted && !run.settled
		run.mu.Unlock()
		if !keep {
			delete(w.runs, run)
		}
		w.runsMu.Unlock()
	}()
	go w.renew(run)
	// Cancel, then wait: a renewal call in flight ends at once rather than
	// at its own timeout, which GracePeriod does not budget for.
	stopRenewal := func() {
		run.renewCancel()
		select {
		case <-run.stopRenew:
		default:
			close(run.stopRenew)
		}
		<-run.renewDone
	}
	defer stopRenewal()

	bound, err := w.cfg.Binder.Bind(run.route, run.action.Input, run.action.ExecutorKey)
	if err != nil {
		// Nothing was sent. The record says so, then fails the action.
		w.report(run, dispatchapi.AmbiguityRequestNotSent, "")
		if !run.isLost() {
			w.complete(run, Classification{Outcome: OutcomeFailed, Kind: KindNotSent,
				ErrorCode: ErrorInputInvalid}, sendResult{}, stopRenewal)
		}
		return
	}
	result := w.send(run, bound)
	if w.killed.Load() {
		return
	}
	switch {
	case !result.final && !result.sent:
		switch {
		case result.lost || result.stopped == GateDraining || result.stopped == GateLease:
			// FENCE_LOST, or draining, with nothing sent: say so and let
			// the claim lapse for another attempt.
			w.report(run, dispatchapi.AmbiguityRequestNotSent, "")
		default:
			// The deadline cannot hold the operation, or the credential is
			// unavailable: nothing left, and the fence is still held.
			w.complete(run, Exhausted(false), result, stopRenewal)
		}
	case run.isLost():
		w.report(run, lostOutcome(result), reference(result.class))
	default:
		w.complete(run, result.class, result, stopRenewal)
	}
}

// lostOutcome is the ambiguity outcome for what SEND observed.
func lostOutcome(result sendResult) dispatchapi.AmbiguityOutcome {
	switch {
	case result.final && result.class.Outcome == OutcomeSucceeded:
		return dispatchapi.AmbiguityEffectObserved
	case !result.sent:
		return dispatchapi.AmbiguityRequestNotSent
	}
	return dispatchapi.AmbiguityOutcomeUnknown
}

func reference(class Classification) string {
	if class.Outcome != OutcomeSucceeded || len(class.Output) == 0 {
		return ""
	}
	var document output
	if err := json.Unmarshal(class.Output, &document); err != nil {
		return ""
	}
	return document.Reference
}

func (w *Worker) send(run *claimRun, bound BoundRequest) sendResult {
	var result sendResult
	retry := &backoff{base: targetBackoffBase, random: w.cfg.Random}
	for attempt := 0; ; attempt++ {
		if run.isLost() {
			result.lost = true
			if result.sent {
				result.class, result.final = Exhausted(true), true
			}
			return result
		}
		anchored, _ := run.anchor()
		if refusal := w.gate.Check(anchored, w.clock.Now(), w.draining.Load()); refusal != GateOpen {
			record := w.record(run, EventRefused)
			record.Gate = refusal
			w.log.Log(record)
			result.stopped = refusal
			if result.sent {
				result.class, result.final = Exhausted(true), true
			}
			return result
		}
		header, credential, err := w.cfg.TargetAuthorization()
		if err != nil {
			result.stopped = GateMisconfigured
			if result.sent {
				result.class, result.final = Exhausted(true), true
			}
			return result
		}
		// The last look before bytes leave: a fence lost, or a drain begun,
		// while the credential was read stops the send here.
		if run.isLost() || w.draining.Load() {
			result.lost = run.isLost()
			result.stopped = GateDraining
			if result.sent {
				result.class, result.final = Exhausted(true), true
			}
			return result
		}
		observation, requestBytes, responseBytes, duration := w.attempt(run, bound, header, credential)
		if w.killed.Load() {
			return result
		}
		if observation.Written {
			result.sent = true
			result.bytes += requestBytes
			result.chunks++
		}
		class := Classify(run.route, observation)
		record := w.record(run, EventSent)
		record.Status, record.Classification = class.Status, class.Kind
		record.Failure = ClassifyFailure(observation.Err)
		record.RequestBytes, record.ResponseBytes, record.Duration = requestBytes, responseBytes, duration
		w.log.Log(record)
		if class.Outcome != OutcomeRetry {
			result.class, result.final = class, true
			return result
		}
		if attempt+1 >= w.cfg.MaxSendAttempts {
			result.class, result.final = Exhausted(result.sent), true
			return result
		}
		delay := retry.next()
		if class.HasRetryAfter {
			delay = class.RetryAfter
		}
		if delay > w.cfg.MaxRetryAfter {
			delay = w.cfg.MaxRetryAfter
		}
		select {
		case <-w.clock.After(delay):
		case <-run.lostCh:
		case <-w.drainCh:
		case <-w.hard.Done():
			return result
		}
	}
}

// attempt performs one request. Written is set from GotConn onward, which
// errs towards written: a write misreported as never-sent would be retried.
func (w *Worker) attempt(run *claimRun, bound BoundRequest, header, credential string) (
	Observation, int64, int64, time.Duration,
) {
	var written atomic.Bool
	ctx, cancel := w.withTimeout(w.hard, w.cfg.OperationTimeout)
	defer cancel()
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) { written.Store(true) },
	})
	request, err := NewTargetRequest(ctx, bound, header, credential)
	if err != nil {
		return Observation{Err: err}, 0, 0, 0
	}
	run.mu.Lock()
	if run.abandoned || w.killed.Load() {
		// abandon decided what to write from attempted; a run it gave up on,
		// or one the worker stopped before abandon saw it, sends nothing.
		run.mu.Unlock()
		return Observation{Err: context.Canceled}, 0, 0, 0
	}
	run.inFlight, run.attempted = true, true
	run.mu.Unlock()
	defer func() {
		run.mu.Lock()
		run.inFlight = false
		run.mu.Unlock()
	}()
	started := w.clock.Now()
	response, err := w.cfg.Target.Do(request)
	stopYield("attempt:returned")
	if err != nil {
		return Observation{Written: written.Load(), Err: err}, int64(len(bound.Body)), 0,
			w.clock.Now().Sub(started)
	}
	defer response.Body.Close()
	body, oversize, readErr := ReadBounded(response.Body, w.cfg.MaxResponseBytes)
	return Observation{Written: true, Response: &Response{
			Status: response.StatusCode, Header: response.Header, Body: body,
			Oversize: oversize, BodyErr: readErr,
		}}, int64(len(bound.Body)), int64(len(body)),
		w.clock.Now().Sub(started)
}

// complete reports the outcome under the fence. A definite refusal is
// FENCE_LOST; an answer that may have committed waits out the lease (no
// longer renewed) and then reports through the ambiguity route, which
// appends with expected_version 0.
func (w *Worker) complete(run *claimRun, class Classification, result sendResult, stopRenewal func()) {
	run.mu.Lock()
	abandoned := run.abandoned
	run.mu.Unlock()
	if abandoned {
		// The drain gave up on this run and wrote what it knew.
		return
	}
	request, err := w.requestContext("gateway_complete")
	if err != nil {
		w.report(run, lostOutcome(result), reference(class))
		return
	}
	completion := Completion{
		Context: run.action.Correlate(request), ClaimID: run.claimID,
		ClaimFence: run.action.ClaimFence,
	}
	run.mu.Lock()
	completion.ExpectedVersion = run.action.Version
	run.mu.Unlock()
	if class.Outcome == OutcomeSucceeded {
		completion.Output = class.Output
	} else {
		completion.Failed, completion.ErrorCode = true, class.ErrorCode
		completion.Effected = w.effected(run.route, result)
	}
	// CompletionBudget is the same figure GracePeriod reserves for it.
	callCtx, cancel := w.withTimeout(w.hard, CompletionBudget(w.cfg.PlaneTimeout))
	_, err = w.cfg.Dispatch.Complete(callCtx, run.action.ID, completion)
	cancel()
	record := w.record(run, EventCompleted)
	record.Classification, record.Status = class.Kind, class.Status
	switch {
	case err == nil:
		run.settle()
		w.log.Log(record)
		return
	case DispatchKind(err) == DispatchRecordedOtherwise:
		// Final: the record says something else under this claim.
		run.settle()
		record.DispatchError = DispatchRecordedOtherwise
		w.log.Log(record)
		return
	case w.killed.Load():
		return
	}
	record.Event, record.DispatchError = EventDispatch, DispatchKind(err)
	w.log.Log(record)
	if DispatchKind(err) == DispatchIndeterminate {
		// The completion may have committed (#506). Stop renewing and wait
		// for the lease to end, so whatever was in flight has settled, then
		// report through the ambiguity route. While draining, the grace
		// period does not wait for the lease.
		stopRenewal()
		if delay := run.settleAt().Sub(w.clock.Now()); delay > 0 {
			select {
			case <-w.clock.After(delay):
			case <-w.drainCh:
			case <-w.hard.Done():
				return
			}
		}
	}
	w.report(run, lostOutcome(result), reference(class))
}

// effected is the volume a failure may have let out (#427): an upper bound,
// the request body of every attempt that may have been written. Only on an
// egress route, and never chunks without bytes.
func (w *Worker) effected(route *Route, result sendResult) dispatchapi.Effected {
	if !result.sent || result.bytes <= 0 {
		return dispatchapi.Effected{}
	}
	egress := false
	for _, effect := range route.Effects() {
		if effect == dispatchapi.EffectEgressesContent {
			egress = true
		}
	}
	if !egress {
		return dispatchapi.Effected{}
	}
	volume := dispatchapi.Effected{Bytes: result.bytes, Chunks: result.chunks}
	if volume.Bytes > dispatchapi.MaxEffectedBytes {
		volume.Bytes = dispatchapi.MaxEffectedBytes
	}
	if volume.Chunks > dispatchapi.MaxEffectedChunks {
		volume.Chunks = dispatchapi.MaxEffectedChunks
	}
	return volume
}

// report makes the one lost-fence report this fence gets. A report the
// explorer does not record — refused, not found, or indeterminate after the
// retries — goes to the unrecorded log; if even that fails, the failure is
// logged, never dropped silently.
func (w *Worker) report(run *claimRun, outcome dispatchapi.AmbiguityOutcome, ref string) {
	run.mu.Lock()
	if run.reported {
		run.mu.Unlock()
		return
	}
	run.reported = true
	run.mu.Unlock()
	report := AmbiguityReport{
		ClaimFence: run.action.ClaimFence, Outcome: outcome,
		Target: w.cfg.SurfaceName, Reference: ref,
	}
	var err error
	for attempt := 0; attempt < reportAttempts; attempt++ {
		var request RequestContext
		if request, err = w.requestContext("gateway_ambiguity"); err != nil {
			break
		}
		report.Context = run.action.Correlate(request)
		callCtx, cancel := w.withTimeout(w.hard, ReportWindow)
		_, err = w.cfg.Dispatch.ReportAmbiguity(callCtx, run.action.ID, report)
		cancel()
		if err == nil || errors.Is(err, ErrAmbiguityUnrecorded) || w.killed.Load() {
			break
		}
	}
	if w.killed.Load() {
		return
	}
	record := w.record(run, EventAmbiguity)
	record.Ambiguity = outcome
	if err == nil {
		run.settle()
		w.log.Log(record)
		return
	}
	entry := run.unrecorded(w.cfg.SurfaceName, outcome, ref, err)
	if appendErr := w.cfg.Unrecorded.Append(entry); appendErr != nil {
		// Unsettled, so the run stays in runs; the drain retries this
		// entry rather than an outcome_unknown in its place.
		run.mu.Lock()
		run.pending = &entry
		run.mu.Unlock()
		record.Event, record.DispatchError = EventDispatch, DispatchKind(err)
		w.log.Log(record)
		return
	}
	run.settle()
	record.Event, record.DispatchError = EventUnrecorded, DispatchKind(err)
	record.Unrecorded = w.cfg.Unrecorded.Len()
	w.log.Log(record)
}
