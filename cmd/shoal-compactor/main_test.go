// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//   https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/compactjob"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/client"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/compactioncoordinator"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/security"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/tabletserver"
	"github.com/phrocker/shoal-oss/internal/zk"
)

// scriptedResolver returns one result per call; the final entry repeats.
type scriptedResolver struct {
	results []resolveResult
	calls   int
}

type resolveResult struct {
	addr string
	err  error
}

func (r *scriptedResolver) Address(context.Context) (string, error) {
	i := r.calls
	if i >= len(r.results) {
		i = len(r.results) - 1
	}
	r.calls++
	return r.results[i].addr, r.results[i].err
}

// fakeCoordinator implements only the two RPCs the poll loop issues; the
// embedded interface leaves the rest nil so an unexpected call panics
// instead of silently succeeding.
type fakeCoordinator struct {
	compactioncoordinator.CompactionCoordinatorService

	getJob func(ecid string) (*compactioncoordinator.TNextCompactionJob, error)
	// onFailed, when set, decides whether each release attempt fails.
	// Attempts are numbered from 1 across the whole fake.
	onFailed func(attempt int) error

	mu           sync.Mutex
	failed       []failedCompaction
	failAttempts int
	completed    int
}

type failedCompaction struct {
	ecid  string
	class string
	state compactioncoordinator.TCompactionState
}

func (f *fakeCoordinator) GetCompactionJob(
	_ context.Context,
	_ *client.TInfo,
	_ *security.TCredentials,
	_ string,
	_ string,
	ecid string,
) (*compactioncoordinator.TNextCompactionJob, error) {
	return f.getJob(ecid)
}

func (f *fakeCoordinator) CompactionFailed(
	_ context.Context,
	_ *client.TInfo,
	_ *security.TCredentials,
	ecid string,
	_ *data.TKeyExtent,
	exceptionClassName string,
	failureState compactioncoordinator.TCompactionState,
) error {
	f.mu.Lock()
	f.failAttempts++
	attempt := f.failAttempts
	onFailed := f.onFailed
	f.mu.Unlock()

	if onFailed != nil {
		if err := onFailed(attempt); err != nil {
			return err
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.failed = append(f.failed, failedCompaction{
		ecid:  ecid,
		class: exceptionClassName,
		state: failureState,
	})
	return nil
}

// CompactionCompleted is the manager's commit path. shoal must never
// call it: recording the call lets tests assert that.
func (f *fakeCoordinator) CompactionCompleted(
	_ context.Context,
	_ *client.TInfo,
	_ *security.TCredentials,
	_ string,
	_ *data.TKeyExtent,
	_ *tabletserver.TCompactionStats,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completed++
	return nil
}

// releases returns a copy of the recorded hand-backs.
func (f *fakeCoordinator) releases() []failedCompaction {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]failedCompaction(nil), f.failed...)
}

func (f *fakeCoordinator) completedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.completed
}

type fakeConn struct {
	svc *fakeCoordinator

	// The poll loop closes the transport from a context watcher while an
	// RPC may still be running on another goroutine, so this bookkeeping
	// has to be race-free.
	mu      sync.Mutex
	closed  int
	onClose func()
}

func (c *fakeConn) Raw() compactioncoordinator.CompactionCoordinatorService { return c.svc }

func (c *fakeConn) Close() error {
	c.mu.Lock()
	c.closed++
	onClose := c.onClose
	c.mu.Unlock()
	if onClose != nil {
		onClose()
	}
	return nil
}

func (c *fakeConn) closeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// idleReply is the coordinator's "no work for this group" answer: a job
// with no externalCompactionId set.
func idleReply() *compactioncoordinator.TNextCompactionJob {
	return &compactioncoordinator.TNextCompactionJob{
		Job:            tabletserver.NewTExternalCompactionJob(),
		CompactorCount: 1,
	}
}

func testLogger() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

func testPollConfig(
	resolver coordinatorResolver,
	dial func(context.Context, string, string, string) (coordinatorConn, error),
) pollConfig {
	return pollConfig{
		resolver:        resolver,
		dial:            dial,
		instanceID:      "uuid-1",
		accumuloVersion: "4.0.0-SNAPSHOT",
		groupName:       "shoal_default",
		advertiseAddr:   "compactor-1:9810",
		creds:           security.NewTCredentials(),
		minWait:         time.Millisecond,
		maxWait:         8 * time.Millisecond,
		releaseTimeout:  2 * time.Second,
		jobOptions:      compactjob.Options{Limits: compactjob.DefaultLimits()},
	}
}

// translatableJob is an assignment shoal can fully reproduce: whole-file
// inputs, a ported iterator, a writable output encoding. Everything past
// translation is what the tests vary.
func translatableJob(ecid string) *tabletserver.TExternalCompactionJob {
	job := tabletserver.NewTExternalCompactionJob()
	job.ExternalCompactionId = ecid
	job.Extent = &data.TKeyExtent{Table: []byte("2"), PrevEndRow: []byte("c"), EndRow: []byte("m")}
	job.Files = []*tabletserver.InputFile{{
		MetadataFileEntry: `{"path":"hdfs://nn/accumulo/tables/2/t-0001/F0001.rf","startRow":"","endRow":""}`,
		Size:              4096,
		Entries:           40,
	}}
	// The coordinator's compaction temp name, which carries this job's
	// own ECID (TabletNameGenerator.getNextDataFilenameForMajc).
	job.OutputFile = "hdfs://nn/accumulo/tables/2/t-0001/C0002.rf_tmp_" + ecid
	job.PropagateDeletes = true
	job.Kind = tabletserver.TCompactionKind_SYSTEM
	job.IteratorSettings = &tabletserver.IteratorConfig{
		Iterators: []*tabletserver.TIteratorSetting{{
			Priority:      20,
			Name:          "vers",
			IteratorClass: "org.apache.accumulo.core.iterators.user.VersioningIterator",
			Properties:    map[string]string{"maxVersions": "1"},
		}},
	}
	// Present and empty, the way a coordinator sends a job with no
	// property overrides. Compactor.initialize dereferences the map
	// before it reads a file, so a missing one is a job Java throws on
	// rather than one with nothing to override.
	job.Overrides = map[string]string{}
	return job
}

// jobReply serves one job and then reports the group idle, which is how
// a drain terminates.
func jobReply(job *tabletserver.TExternalCompactionJob) *compactioncoordinator.TNextCompactionJob {
	return &compactioncoordinator.TNextCompactionJob{Job: job, CompactorCount: 1}
}

// TestRunPollLoopFollowsManagerFailover walks the loop through a manager
// failover: it polls the old primary, rides out the window where no
// manager advertises a coordinator, and then dials the address the new
// primary published — without a restart and without ever dialing during
// the gap.
func TestRunPollLoopFollowsManagerFailover(t *testing.T) {
	resolver := &scriptedResolver{results: []resolveResult{
		{addr: "manager-a:9999"},
		{err: zk.ErrCoordinatorUnavailable},
		{err: zk.ErrCoordinatorUnavailable},
		{addr: "manager-b:9999"},
	}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var dialed []string
	dial := func(_ context.Context, addr, instanceID, accumuloVersion string) (coordinatorConn, error) {
		if instanceID != "uuid-1" || accumuloVersion != "4.0.0-SNAPSHOT" {
			t.Errorf("dial(%q) instance/version = %q/%q", addr, instanceID, accumuloVersion)
		}
		dialed = append(dialed, addr)
		return &fakeConn{svc: &fakeCoordinator{
			getJob: func(string) (*compactioncoordinator.TNextCompactionJob, error) {
				// Stop once the loop has reached the new primary.
				if addr == "manager-b:9999" {
					cancel()
				}
				return idleReply(), nil
			},
		}}, nil
	}

	logger, logs := testLogger()
	runPollLoop(ctx, logger, testPollConfig(resolver, dial))

	want := []string{"manager-a:9999", "manager-b:9999"}
	if strings.Join(dialed, ",") != strings.Join(want, ",") {
		t.Fatalf("dialed = %v, want %v (no dial may happen while the lock is unheld)", dialed, want)
	}
	out := logs.String()
	if !strings.Contains(out, `"coordinator address changed; following manager failover"`) ||
		!strings.Contains(out, `previous=manager-a:9999`) ||
		!strings.Contains(out, `current=manager-b:9999`) {
		t.Fatalf("failover not logged:\n%s", out)
	}
}

// TestRunPollLoopBacksOffWhileCoordinatorUnavailable pins the two
// discovery-failure shapes apart: a missing coordinator descriptor is an
// expected failover window (INFO, keep retrying), while a ZooKeeper
// transport failure is operator-visible (WARN).
func TestRunPollLoopBacksOffWhileCoordinatorUnavailable(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		wantLevel string
	}{
		{"failover window", zk.ErrCoordinatorUnavailable, "level=INFO"},
		{"zookeeper down", errors.New("list manager locks: connection refused"), "level=WARN"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			attempts := 0
			resolver := resolverFunc(func(context.Context) (string, error) {
				attempts++
				if attempts == 3 {
					cancel()
				}
				return "", tt.err
			})
			dial := func(_ context.Context, addr, _, _ string) (coordinatorConn, error) {
				t.Fatalf("dialed %q despite failed discovery", addr)
				return nil, nil
			}

			logger, logs := testLogger()
			runPollLoop(ctx, logger, testPollConfig(resolver, dial))

			if attempts < 3 {
				t.Fatalf("resolver attempts = %d, want retries until cancellation", attempts)
			}
			out := logs.String()
			if !strings.Contains(out, `"coordinator discovery failed; backing off"`) {
				t.Fatalf("discovery failure not logged:\n%s", out)
			}
			if !strings.Contains(out, tt.wantLevel) {
				t.Fatalf("discovery failure not logged at %s:\n%s", tt.wantLevel, out)
			}
			// Backoff must grow so a long outage does not hammer ZooKeeper.
			if got := retryWaits(out); strings.Join(got, ",") != "1ms,2ms" {
				t.Fatalf("backoff = %v, want [1ms 2ms]:\n%s", got, out)
			}
		})
	}
}

type resolverFunc func(context.Context) (string, error)

func (f resolverFunc) Address(ctx context.Context) (string, error) { return f(ctx) }

// retryWaits extracts the retry_in= backoff values, in order, from
// captured slog text output.
func retryWaits(logOutput string) []string {
	var waits []string
	for _, line := range strings.Split(logOutput, "\n") {
		_, rest, found := strings.Cut(line, "retry_in=")
		if !found {
			continue
		}
		wait, _, _ := strings.Cut(rest, " ")
		waits = append(waits, strings.TrimSpace(wait))
	}
	return waits
}

// TestRunPollLoopResetsBackoffAfterReconnect makes sure the failover
// backoff does not leak past recovery: once the loop is talking to a
// manager again, the next outage restarts at minWait.
func TestRunPollLoopResetsBackoffAfterReconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	attempts := 0
	resolver := resolverFunc(func(context.Context) (string, error) {
		attempts++
		if attempts == 4 {
			return "manager-a:9999", nil
		}
		if attempts >= 6 {
			cancel()
		}
		return "", zk.ErrCoordinatorUnavailable
	})
	dial := func(context.Context, string, string, string) (coordinatorConn, error) {
		return &fakeConn{svc: &fakeCoordinator{
			getJob: func(string) (*compactioncoordinator.TNextCompactionJob, error) {
				return idleReply(), nil
			},
		}}, nil
	}

	logger, logs := testLogger()
	runPollLoop(ctx, logger, testPollConfig(resolver, dial))

	// Attempts 1-3 fail (1ms, 2ms, 4ms), attempt 4 reconnects and resets
	// the failure backoff to minWait — the idle poll after it doubles to
	// 2ms, so attempt 5 must report 2ms. Without the reset the loop would
	// still be sitting at the 8ms cap.
	out := logs.String()
	if got := retryWaits(out); strings.Join(got, ",") != "1ms,2ms,4ms,2ms" {
		t.Fatalf("backoff = %v, want [1ms 2ms 4ms 2ms]:\n%s", got, out)
	}
}

// TestRunPollLoopAbortsWedgedRPCOnCancel: thrift transports ignore the
// context once a call is in flight, so a manager that accepts the
// connection and then goes silent would hold the loop until the socket
// timeout expires. Cancellation must close the transport instead, so
// SIGTERM is honoured immediately.
func TestRunPollLoopAbortsWedgedRPCOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		inFlight    sync.Once
		released    sync.Once
		rpcStarted  = make(chan struct{})
		rpcUnblocks = make(chan struct{})
	)
	release := func() { released.Do(func() { close(rpcUnblocks) }) }
	defer release()

	conn := &fakeConn{onClose: release}
	conn.svc = &fakeCoordinator{
		getJob: func(string) (*compactioncoordinator.TNextCompactionJob, error) {
			inFlight.Do(func() { close(rpcStarted) })
			// A wedged read only ends when the transport is closed.
			<-rpcUnblocks
			return nil, errors.New("read tcp 10.0.0.1:9999: use of closed network connection")
		},
	}

	resolver := resolverFunc(func(context.Context) (string, error) { return "manager-a:9999", nil })
	dial := func(context.Context, string, string, string) (coordinatorConn, error) { return conn, nil }

	logger, _ := testLogger()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runPollLoop(ctx, logger, testPollConfig(resolver, dial))
	}()

	select {
	case <-rpcStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("getCompactionJob never started")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runPollLoop did not return after cancellation; a wedged RPC must be aborted by closing the transport")
	}
	if conn.closeCount() == 0 {
		t.Fatal("transport was never closed")
	}
}

// TestRunPollLoopReconnectsAfterRPCTimeout covers what the bounded
// socket timeout buys: a silent manager surfaces as an RPC error, the
// connection is dropped, and the next attempt re-resolves — which is how
// the pool migrates to a new primary mid-poll.
func TestRunPollLoopReconnectsAfterRPCTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	resolver := &scriptedResolver{results: []resolveResult{
		{addr: "manager-a:9999"},
		{addr: "manager-b:9999"},
	}}

	var conns []*fakeConn
	dial := func(_ context.Context, addr, _, _ string) (coordinatorConn, error) {
		conn := &fakeConn{}
		conn.svc = &fakeCoordinator{
			getJob: func(string) (*compactioncoordinator.TNextCompactionJob, error) {
				if addr == "manager-a:9999" {
					// What a bounded socket read surfaces as.
					return nil, errors.New("read tcp 10.0.0.1:9999: i/o timeout")
				}
				cancel()
				return idleReply(), nil
			},
		}
		conns = append(conns, conn)
		return conn, nil
	}

	logger, logs := testLogger()
	runPollLoop(ctx, logger, testPollConfig(resolver, dial))

	if len(conns) != 2 {
		t.Fatalf("dials = %d, want 2 (timed-out connection must be replaced)", len(conns))
	}
	if got := conns[0].closeCount(); got == 0 {
		t.Fatal("timed-out connection was not closed")
	}
	if resolver.calls < 2 {
		t.Fatalf("resolver calls = %d, want re-resolution after the RPC failure", resolver.calls)
	}
	if out := logs.String(); !strings.Contains(out, `"getCompactionJob failed; will reconnect"`) {
		t.Fatalf("RPC failure not logged:\n%s", out)
	}
}

// serveOneJob wires a fake coordinator that hands out a single job (built
// by mk from the ecid the compactor generated) and is idle afterwards.
func serveOneJob(svc *fakeCoordinator, mk func(ecid string) *tabletserver.TExternalCompactionJob) {
	served := false
	svc.getJob = func(ecid string) (*compactioncoordinator.TNextCompactionJob, error) {
		if served {
			return idleReply(), nil
		}
		served = true
		return jobReply(mk(ecid)), nil
	}
}

// TestDrainCoordinatorReleasesTranslatableJob is the commit boundary: a
// job shoal fully understands is still handed back, because the
// manager-side commit RPC does not exist. The plan is logged so the
// translation of a real job is visible, and compactionCompleted — the
// call that would tell the manager a compaction happened — is never
// made.
func TestDrainCoordinatorReleasesTranslatableJob(t *testing.T) {
	svc := &fakeCoordinator{}
	serveOneJob(svc, translatableJob)

	cfg := testPollConfig(&scriptedResolver{results: []resolveResult{{addr: "manager-a:9999"}}}, nil)
	logger, logs := testLogger()
	if idle := drainCoordinator(context.Background(), logger, &fakeConn{svc: svc}, cfg); !idle {
		t.Fatal("drain = busy, want idle after the coordinator ran out of jobs")
	}

	releases := svc.releases()
	if len(releases) != 1 {
		t.Fatalf("compactionFailed calls = %d, want 1", len(releases))
	}
	got := releases[0]
	if got.class != compactjob.ClassCommitUnavailable {
		t.Fatalf("exception class = %q, want %q", got.class, compactjob.ClassCommitUnavailable)
	}
	if got.state != compactioncoordinator.TCompactionState_FAILED {
		t.Fatalf("failure state = %v, want FAILED", got.state)
	}
	if !strings.HasPrefix(got.ecid, ecidPrefix) {
		t.Fatalf("ecid = %q, want %s-prefixed", got.ecid, ecidPrefix)
	}
	if n := svc.completedCount(); n != 0 {
		t.Fatalf("compactionCompleted calls = %d; shoal must never tell the manager a compaction succeeded", n)
	}

	out := logs.String()
	if !strings.Contains(out, "compaction job translated") {
		t.Fatalf("translation not logged:\n%s", out)
	}
	for _, want := range []string{"plan.table=2", "plan.inputs=1", `plan.stack="[vers=versioning]"`, "plan.full_major=false"} {
		if !strings.Contains(out, want) {
			t.Fatalf("plan log missing %q:\n%s", want, out)
		}
	}
}

// TestDrainCoordinatorReleasesRefusedJobsWithPreciseClass: the class the
// manager records has to say which capability was missing. A single
// "shoal failed" for every cause would leave an operator with no way to
// tell an unported iterator from a malformed assignment.
func TestDrainCoordinatorReleasesRefusedJobsWithPreciseClass(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*tabletserver.TExternalCompactionJob)
		wantClass string
		wantLog   string
	}{
		{
			name:      "malformed: no input files",
			mutate:    func(j *tabletserver.TExternalCompactionJob) { j.Files = nil },
			wantClass: compactjob.ClassMalformedJob,
			wantLog:   "field=files",
		},
		{
			name: "row-fenced input file",
			mutate: func(j *tabletserver.TExternalCompactionJob) {
				// The rows travel base64-encoded
				// (ByteArrayToBase64TypeAdapter); "ZA==" and "aw==" are
				// "d" and "k".
				j.Files[0].MetadataFileEntry = `{"path":"hdfs://nn/accumulo/tables/2/t-0001/F0001.rf","startRow":"AWQ=","endRow":"AWs="}`
			},
			wantClass: compactjob.ClassRangedInputFile,
			wantLog:   "field=files[0]",
		},
		{
			name: "unported iterator",
			mutate: func(j *tabletserver.TExternalCompactionJob) {
				j.IteratorSettings.Iterators[0].IteratorClass = "org.apache.accumulo.core.iterators.user.AgeOffFilter"
			},
			wantClass: compactjob.ClassUnsupportedIterator,
			wantLog:   "AgeOffFilter",
		},
		{
			name: "codec shoal cannot write",
			mutate: func(j *tabletserver.TExternalCompactionJob) {
				j.Overrides = map[string]string{"table.file.compress.type": "zstd"}
			},
			wantClass: compactjob.ClassUnsupportedProperty,
			wantLog:   "zstd",
		},
		{
			name: "encrypted table",
			mutate: func(j *tabletserver.TExternalCompactionJob) {
				j.Overrides = map[string]string{"table.crypto.opts.key": "kms://k1"}
			},
			wantClass: compactjob.ClassUnsupportedCrypto,
			wantLog:   "field=overrides[table.crypto.opts.key]",
		},
		{
			name: "job larger than this compactor's budget",
			mutate: func(j *tabletserver.TExternalCompactionJob) {
				j.Files[0].Size = 1 << 40
			},
			wantClass: compactjob.ClassResourceLimitExceeded,
			wantLog:   "field=files",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &fakeCoordinator{}
			serveOneJob(svc, func(ecid string) *tabletserver.TExternalCompactionJob {
				job := translatableJob(ecid)
				tt.mutate(job)
				return job
			})

			cfg := testPollConfig(&scriptedResolver{results: []resolveResult{{addr: "manager-a:9999"}}}, nil)
			logger, logs := testLogger()
			if idle := drainCoordinator(context.Background(), logger, &fakeConn{svc: svc}, cfg); !idle {
				t.Fatal("drain = busy, want idle")
			}

			releases := svc.releases()
			if len(releases) != 1 {
				t.Fatalf("compactionFailed calls = %d, want the slot released exactly once", len(releases))
			}
			if releases[0].class != tt.wantClass {
				t.Fatalf("exception class = %q, want %q", releases[0].class, tt.wantClass)
			}
			out := logs.String()
			if !strings.Contains(out, "compaction job refused") {
				t.Fatalf("refusal not logged:\n%s", out)
			}
			if !strings.Contains(out, tt.wantLog) {
				t.Fatalf("refusal log missing %q:\n%s", tt.wantLog, out)
			}
			if n := svc.completedCount(); n != 0 {
				t.Fatalf("compactionCompleted calls = %d, want 0", n)
			}
		})
	}
}

// TestDrainCoordinatorSurfacesJobsItCannotHandBack covers the one
// refusal that cannot be released.
//
// CompactionCoordinator.compactionFailed starts with
// KeyExtent.fromThrift(extent), which reads the extent's table bytes, so
// a job whose extent is missing — or whose extent has no table — makes
// the hand-back throw on the manager no matter how many times it is
// tried. The pinned protocol offers no reclaim keyed on the ECID alone,
// and inventing an extent would report a failure against a tablet this
// compactor was never assigned. So the RPC must not be attempted at all,
// and the operator must be told why.
func TestDrainCoordinatorSurfacesJobsItCannotHandBack(t *testing.T) {
	for _, tt := range []struct {
		name    string
		mutate  func(*tabletserver.TExternalCompactionJob)
		wantLog string
	}{
		{
			name:    "no extent",
			mutate:  func(j *tabletserver.TExternalCompactionJob) { j.Extent = nil },
			wantLog: "carries no extent",
		},
		{
			name: "extent without a table id",
			mutate: func(j *tabletserver.TExternalCompactionJob) {
				j.Extent = &data.TKeyExtent{PrevEndRow: []byte("c"), EndRow: []byte("m")}
			},
			wantLog: "carries no table id",
		},
		{
			// A decoded Thrift binary can be a non-nil empty slice. It
			// converts, so the RPC would succeed — naming TableId.of("")
			// rather than the assigned tablet, leaving the assignment in
			// place while shoal logged the slot as released.
			name: "extent with an empty table id",
			mutate: func(j *tabletserver.TExternalCompactionJob) {
				j.Extent = &data.TKeyExtent{Table: []byte{}, PrevEndRow: []byte("c"), EndRow: []byte("m")}
			},
			wantLog: "carries no table id",
		},
		{
			// KeyExtent's constructor rejects prevEndRow >= endRow, and
			// compactionFailed runs KeyExtent.fromThrift before it
			// resolves the id, so the RPC throws on the manager every
			// time. Retrying it would spend the whole release budget on
			// a call that cannot succeed.
			name: "extent whose bounds are inverted",
			mutate: func(j *tabletserver.TExternalCompactionJob) {
				j.Extent = &data.TKeyExtent{Table: []byte("2"), PrevEndRow: []byte("m"), EndRow: []byte("c")}
			},
			wantLog: "KeyExtent.fromThrift rejects",
		},
		{
			// The constructor's test is ">=", so equal bounds throw too.
			name: "extent whose bounds are equal",
			mutate: func(j *tabletserver.TExternalCompactionJob) {
				j.Extent = &data.TKeyExtent{Table: []byte("2"), PrevEndRow: []byte("m"), EndRow: []byte("m")}
			},
			wantLog: "KeyExtent.fromThrift rejects",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc := &fakeCoordinator{}
			serveOneJob(svc, func(ecid string) *tabletserver.TExternalCompactionJob {
				job := translatableJob(ecid)
				tt.mutate(job)
				return job
			})

			cfg := testPollConfig(&scriptedResolver{results: []resolveResult{{addr: "manager-a:9999"}}}, nil)
			logger, logs := testLogger()
			if idle := drainCoordinator(context.Background(), logger, &fakeConn{svc: svc}, cfg); !idle {
				t.Fatal("drain = busy, want idle")
			}

			if releases := svc.releases(); len(releases) != 0 {
				t.Fatalf("compactionFailed calls = %d, want none: the coordinator cannot process them",
					len(releases))
			}
			if n := svc.completedCount(); n != 0 {
				t.Fatalf("compactionCompleted calls = %d, want 0", n)
			}
			out := logs.String()
			if !strings.Contains(out, "compaction slot cannot be released") {
				t.Fatalf("the unreleasable slot was not surfaced:\n%s", out)
			}
			if !strings.Contains(out, tt.wantLog) {
				t.Fatalf("log missing %q:\n%s", tt.wantLog, out)
			}
			if !strings.Contains(out, "level=ERROR") {
				t.Fatalf("an unreleasable slot must be logged at error level:\n%s", out)
			}
			if !strings.Contains(out, compactjob.ClassMalformedJob) {
				t.Fatalf("log missing the refusal class:\n%s", out)
			}
		})
	}
}

// TestDrainCoordinatorStopsAfterASlotItCouldNotRelease: an assignment
// left active is the one outcome under which asking for more work makes
// things worse. Every further getCompactionJob can hand out another slot
// that will leak the same way, and each leaked slot costs a tablet a
// compaction cycle until the coordinator's own sweep notices. So the
// drain ends and reports idle, which is what puts the outer loop on its
// backoff instead of straight back on the wire.
func TestDrainCoordinatorStopsAfterASlotItCouldNotRelease(t *testing.T) {
	svc := &fakeCoordinator{}
	handedOut := 0
	svc.getJob = func(ecid string) (*compactioncoordinator.TNextCompactionJob, error) {
		handedOut++
		job := translatableJob(ecid)
		// Unreleasable: compactionFailed cannot convert a missing extent.
		job.Extent = nil
		return jobReply(job), nil
	}

	cfg := testPollConfig(&scriptedResolver{results: []resolveResult{{addr: "manager-a:9999"}}}, nil)
	logger, logs := testLogger()
	if idle := drainCoordinator(context.Background(), logger, &fakeConn{svc: svc}, cfg); !idle {
		t.Fatal("drain = busy, want idle so the outer loop backs off")
	}

	if handedOut != 1 {
		t.Fatalf("getCompactionJob calls = %d, want 1: the drain kept asking for slots it cannot release",
			handedOut)
	}
	if releases := svc.releases(); len(releases) != 0 {
		t.Fatalf("compactionFailed calls = %d, want none: the coordinator cannot process them",
			len(releases))
	}
	if n := svc.completedCount(); n != 0 {
		t.Fatalf("compactionCompleted calls = %d, want 0", n)
	}
	if out := logs.String(); !strings.Contains(out, "compaction slot cannot be released") {
		t.Fatalf("the unreleasable slot was not surfaced:\n%s", out)
	}
}

// TestDrainCoordinatorStillReleasesWhenOnlyTheRowsAreMissing keeps the
// guard from swallowing releasable jobs: KeyExtent.fromThrift maps a
// null endRow or prevEndRow to a null Text, so an extent with only a
// table id converts fine and the slot must still go back.
func TestDrainCoordinatorStillReleasesWhenOnlyTheRowsAreMissing(t *testing.T) {
	svc := &fakeCoordinator{}
	serveOneJob(svc, func(ecid string) *tabletserver.TExternalCompactionJob {
		job := translatableJob(ecid)
		job.Extent = &data.TKeyExtent{Table: []byte("2")}
		return job
	})

	cfg := testPollConfig(&scriptedResolver{results: []resolveResult{{addr: "manager-a:9999"}}}, nil)
	logger, _ := testLogger()
	if idle := drainCoordinator(context.Background(), logger, &fakeConn{svc: svc}, cfg); !idle {
		t.Fatal("drain = busy, want idle")
	}

	releases := svc.releases()
	if len(releases) != 1 {
		t.Fatalf("compactionFailed calls = %d, want the slot released exactly once", len(releases))
	}
	if releases[0].class != compactjob.ClassCommitUnavailable {
		t.Fatalf("exception class = %q, want %q", releases[0].class, compactjob.ClassCommitUnavailable)
	}
}

// TestReleaseJobRetriesOnFreshConnection: the release is the one RPC
// shoal owes the manager, so a transport failure mid-hand-back must not
// strand the job. The retry re-resolves the coordinator, which is also
// what carries the release to a manager that failed over mid-job.
func TestReleaseJobRetriesOnFreshConnection(t *testing.T) {
	svc := &fakeCoordinator{
		onFailed: func(attempt int) error {
			if attempt == 1 {
				return errors.New("write tcp 10.0.0.1:9999: broken pipe")
			}
			return nil
		},
	}
	serveOneJob(svc, translatableJob)

	resolver := &scriptedResolver{results: []resolveResult{
		{addr: "manager-a:9999"},
		{addr: "manager-b:9999"},
	}}
	var redialed []string
	dial := func(_ context.Context, addr, _, _ string) (coordinatorConn, error) {
		redialed = append(redialed, addr)
		return &fakeConn{svc: svc}, nil
	}

	cfg := testPollConfig(resolver, dial)
	original := &fakeConn{svc: svc}
	logger, logs := testLogger()

	if idle := drainCoordinator(context.Background(), logger, original, cfg); idle {
		t.Fatal("drain = idle, want the spent connection reported so the loop reconnects")
	}

	releases := svc.releases()
	if len(releases) != 1 {
		t.Fatalf("recorded releases = %d, want 1 after the retry succeeded", len(releases))
	}
	if releases[0].class != compactjob.ClassCommitUnavailable {
		t.Fatalf("class = %q", releases[0].class)
	}
	if len(redialed) != 1 || redialed[0] != "manager-a:9999" {
		t.Fatalf("redials = %v, want one re-resolved dial", redialed)
	}
	if out := logs.String(); !strings.Contains(out, "release: compactionFailed rpc failed") ||
		!strings.Contains(out, "attempt=2") {
		t.Fatalf("retry not logged:\n%s", out)
	}
}

// TestReleaseJobSurvivesShutdown: SIGTERM arrives while a job is in
// hand. The poll loop's watcher has already closed the connection, so
// the release has to dial its own — on a context the cancellation cannot
// abort — and say the compactor is going away rather than that the job
// is unsupported.
func TestReleaseJobSurvivesShutdown(t *testing.T) {
	svc := &fakeCoordinator{}
	shutdownConn := &fakeConn{svc: svc}

	var redialed []string
	dial := func(ctx context.Context, addr, _, _ string) (coordinatorConn, error) {
		if err := ctx.Err(); err != nil {
			t.Errorf("release dialed with a cancelled context: %v", err)
		}
		redialed = append(redialed, addr)
		return shutdownConn, nil
	}
	cfg := testPollConfig(&scriptedResolver{results: []resolveResult{{addr: "manager-b:9999"}}}, dial)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// The connection the job arrived on: closed by the watcher, so any
	// RPC on it would fail. Using it at all is the bug this guards.
	stale := &fakeConn{svc: &fakeCoordinator{
		onFailed: func(int) error {
			t.Error("release used the connection shutdown had already closed")
			return errors.New("closed")
		},
	}}

	logger, logs := testLogger()
	job := translatableJob(ecidPrefix + "0d1a4b0e-0000-0000-0000-000000000001")
	usable, released := releaseJob(ctx, logger, stale, cfg, job, compactjob.ClassCommitUnavailable)
	if !usable {
		t.Error("releaseJob reported the caller's connection spent; it was never used")
	}
	if !released {
		t.Error("releaseJob reported the slot still assigned; the coordinator accepted the hand-back")
	}

	releases := svc.releases()
	if len(releases) != 1 {
		t.Fatalf("releases = %d, want the job handed back despite shutdown", len(releases))
	}
	if releases[0].class != compactjob.ClassShuttingDown {
		t.Fatalf("class = %q, want %q so the manager can tell this from an unsupported job",
			releases[0].class, compactjob.ClassShuttingDown)
	}
	if len(redialed) != 1 {
		t.Fatalf("redials = %v, want exactly one fresh connection", redialed)
	}
	if shutdownConn.closeCount() != 1 {
		t.Fatalf("release connection closed %d times, want 1", shutdownConn.closeCount())
	}
	if out := logs.String(); !strings.Contains(out, "compaction slot released to coordinator") {
		t.Fatalf("release not logged:\n%s", out)
	}
}

// TestReleaseJobGivesUpWithinItsBudget: an unreachable coordinator must
// not hold shutdown open. The job is left to the coordinator's own
// dead-compactor sweep, loudly.
func TestReleaseJobGivesUpWithinItsBudget(t *testing.T) {
	dials := 0
	dial := func(context.Context, string, string, string) (coordinatorConn, error) {
		dials++
		return nil, errors.New("dial tcp 10.0.0.9:9999: connection refused")
	}
	cfg := testPollConfig(&scriptedResolver{results: []resolveResult{{addr: "manager-a:9999"}}}, dial)
	cfg.releaseTimeout = 150 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	logger, logs := testLogger()
	job := translatableJob(ecidPrefix + "0d1a4b0e-0000-0000-0000-000000000002")

	start := time.Now()
	_, released := releaseJob(ctx, logger, &fakeConn{svc: &fakeCoordinator{}}, cfg, job, compactjob.ClassCommitUnavailable)
	elapsed := time.Since(start)

	if released {
		t.Error("releaseJob reported the slot released; no hand-back ever reached a coordinator")
	}

	if elapsed > time.Second {
		t.Fatalf("release took %s, want it bounded by the %s budget", elapsed, cfg.releaseTimeout)
	}
	if dials < 2 {
		t.Fatalf("dial attempts = %d, want retries inside the budget", dials)
	}
	out := logs.String()
	if !strings.Contains(out, "release: gave up") {
		t.Fatalf("give-up not logged at error level:\n%s", out)
	}
	if !strings.Contains(out, "level=ERROR") {
		t.Fatalf("give-up must be operator-visible:\n%s", out)
	}
}

// TestReleaseJobBoundsASilentCoordinator: a coordinator that accepts the
// connection and then never answers must not hold shutdown open.
//
// The release context alone cannot do this. cclient's transports are not
// context-aware once a call is on the wire, so compactionFailed only
// returns when the socket timeout fires — which is longer than the
// release budget. Only closing the socket enforces the budget, so the
// fake here ignores the context entirely and unblocks solely on Close:
// if the watcher regresses, this test hangs rather than passing.
func TestReleaseJobBoundsASilentCoordinator(t *testing.T) {
	// Closed by the connection watcher; the blocked RPC waits on it the
	// way a real socket read waits on the transport being torn down.
	socketTorn := make(chan struct{})
	svc := &fakeCoordinator{
		onFailed: func(int) error {
			<-socketTorn
			return errors.New("read tcp 10.0.0.1:9999: use of closed network connection")
		},
	}

	var once sync.Once
	cc := &fakeConn{svc: svc}
	cc.onClose = func() { once.Do(func() { close(socketTorn) }) }

	dial := func(context.Context, string, string, string) (coordinatorConn, error) {
		t.Error("release redialed; the budget expired during the first attempt")
		return nil, errors.New("unexpected dial")
	}
	cfg := testPollConfig(&scriptedResolver{results: []resolveResult{{addr: "manager-a:9999"}}}, dial)
	cfg.releaseTimeout = 200 * time.Millisecond

	logger, logs := testLogger()
	job := translatableJob(ecidPrefix + "0d1a4b0e-0000-0000-0000-000000000003")

	type releaseOutcome struct{ usable, released bool }
	done := make(chan releaseOutcome, 1)
	start := time.Now()
	go func() {
		usable, released := releaseJob(context.Background(), logger, cc, cfg, job, compactjob.ClassCommitUnavailable)
		done <- releaseOutcome{usable: usable, released: released}
	}()

	var outcome releaseOutcome
	select {
	case outcome = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("releaseJob never returned; the budget is not enforced on in-flight RPCs")
	}
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("release took %s, want it bounded by the %s budget", elapsed, cfg.releaseTimeout)
	}
	if outcome.usable {
		t.Error("releaseJob reported the connection usable after the watcher closed it")
	}
	if outcome.released {
		t.Error("releaseJob reported the slot released; the coordinator never answered")
	}
	if cc.closeCount() == 0 {
		t.Error("the wedged connection was never closed")
	}
	if len(svc.releases()) != 0 {
		t.Errorf("releases = %v, want none recorded; the coordinator never answered", svc.releases())
	}
	out := logs.String()
	if !strings.Contains(out, "release: gave up") || !strings.Contains(out, "level=ERROR") {
		t.Fatalf("give-up not logged for the operator:\n%s", out)
	}
}

// TestReleaseJobClosesAConnectionDialedAfterTheBudget covers the gap
// between the watcher and a late redial. The watcher fires once and
// exits, so a connection opened after the deadline has no one left to
// close it: without a deadline-aware registration it would go on to
// start an RPC whose only bound is the socket timeout, which is exactly
// the stall the budget exists to prevent.
func TestReleaseJobClosesAConnectionDialedAfterTheBudget(t *testing.T) {
	svc := &fakeCoordinator{
		onFailed: func(int) error {
			t.Error("an RPC started on a connection dialed after the budget expired")
			return nil
		},
	}

	late := &fakeConn{svc: svc}
	dialed := make(chan struct{}, 1)
	dial := func(ctx context.Context, _, _, _ string) (coordinatorConn, error) {
		// Outlast the budget, the way a slow TCP connect would.
		time.Sleep(300 * time.Millisecond)
		dialed <- struct{}{}
		return late, nil
	}

	cfg := testPollConfig(&scriptedResolver{results: []resolveResult{
		{addr: "manager-a:9999"}, {addr: "manager-a:9999"},
	}}, dial)
	cfg.releaseTimeout = 100 * time.Millisecond

	// A cancelled parent makes releaseJob skip the caller's connection
	// and dial, which is the path that can outrun the watcher.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	logger, logs := testLogger()
	job := translatableJob(ecidPrefix + "0d1a4b0e-0000-0000-0000-000000000004")
	caller := &fakeConn{svc: svc}

	usable, released := releaseJob(ctx, logger, caller, cfg, job, compactjob.ClassCommitUnavailable)

	select {
	case <-dialed:
	default:
		t.Fatal("the test never reached the late dial it is meant to cover")
	}
	if late.closeCount() == 0 {
		t.Error("the late connection was never closed; it would leak and its RPC would only be bounded by the socket timeout")
	}
	if !usable {
		t.Error("the caller's connection was reported spent; releaseJob never touched it")
	}
	if released {
		t.Error("releaseJob reported the slot released; the budget was gone before any RPC started")
	}
	if n := len(svc.releases()); n != 0 {
		t.Errorf("releases = %d, want none; the budget was already gone", n)
	}
	if out := logs.String(); !strings.Contains(out, "release: gave up") {
		t.Fatalf("give-up not logged for the operator:\n%s", out)
	}
}

// TestReleaseJobReportsShutdownThatStartsMidRelease: the shutdown class
// is the manager's only record of *why* a slot came back. Sampling the
// parent context once, before the first attempt, reports a stale reason
// for a job that was abandoned while the first attempt was on the wire.
func TestReleaseJobReportsShutdownThatStartsMidRelease(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	svc := &fakeCoordinator{}
	svc.onFailed = func(attempt int) error {
		if attempt == 1 {
			// SIGTERM lands while the first hand-back is in flight.
			cancel()
			return errors.New("write tcp 10.0.0.1:9999: broken pipe")
		}
		return nil
	}

	fresh := &fakeConn{svc: svc}
	dial := func(context.Context, string, string, string) (coordinatorConn, error) {
		return fresh, nil
	}
	cfg := testPollConfig(&scriptedResolver{results: []resolveResult{
		{addr: "manager-a:9999"}, {addr: "manager-a:9999"},
	}}, dial)

	logger, _ := testLogger()
	job := translatableJob(ecidPrefix + "0d1a4b0e-0000-0000-0000-000000000005")

	usable, released := releaseJob(ctx, logger, &fakeConn{svc: svc}, cfg, job, compactjob.ClassCommitUnavailable)
	if usable {
		t.Error("the caller's connection failed an RPC but was still reported usable")
	}
	if !released {
		t.Error("the retry handed the slot back; releaseJob reported it still assigned")
	}

	rel := svc.releases()
	if len(rel) != 1 {
		t.Fatalf("releases = %+v, want exactly the retry to have been recorded", rel)
	}
	if rel[0].class != compactjob.ClassShuttingDown {
		t.Fatalf("class = %q, want %q; shutdown began before this attempt",
			rel[0].class, compactjob.ClassShuttingDown)
	}
	if rel[0].state != compactioncoordinator.TCompactionState_FAILED {
		t.Fatalf("state = %v, want FAILED", rel[0].state)
	}
	if n := svc.completedCount(); n != 0 {
		t.Fatalf("compactionCompleted calls = %d; shoal must never tell the manager a compaction succeeded", n)
	}
}

// TestDrainCoordinatorStopsAfterJobBudget: a coordinator that keeps
// offering jobs shoal declines must not become a hot loop. The drain
// yields after its budget and reports idle so the outer backoff applies.
func TestDrainCoordinatorStopsAfterJobBudget(t *testing.T) {
	svc := &fakeCoordinator{
		getJob: func(ecid string) (*compactioncoordinator.TNextCompactionJob, error) {
			return jobReply(translatableJob(ecid)), nil
		},
	}

	cfg := testPollConfig(&scriptedResolver{results: []resolveResult{{addr: "manager-a:9999"}}}, nil)
	logger, logs := testLogger()

	done := make(chan bool, 1)
	go func() {
		done <- drainCoordinator(context.Background(), logger, &fakeConn{svc: svc}, cfg)
	}()

	select {
	case idle := <-done:
		if !idle {
			t.Fatal("drain = busy, want idle so the outer loop backs off")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("drain never yielded: an endlessly re-offered job spins the compactor")
	}

	if n := len(svc.releases()); n != maxJobsPerDrain {
		t.Fatalf("releases = %d, want the per-drain budget of %d", n, maxJobsPerDrain)
	}
	if !strings.Contains(logs.String(), "released the per-drain job budget") {
		t.Fatalf("budget exhaustion not logged:\n%s", logs.String())
	}
}

// TestRunPollLoopReleasesJobWhenCancelledMidDrain is the shutdown race:
// the signal lands while a job is being handled, so the watcher closes
// the connection underneath the release. Under -race this also covers
// the watcher's concurrent Close against the release's own dial.
func TestRunPollLoopReleasesJobWhenCancelledMidDrain(t *testing.T) {
	svc := &fakeCoordinator{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	dials := 0
	dial := func(_ context.Context, addr, _, _ string) (coordinatorConn, error) {
		mu.Lock()
		dials++
		first := dials == 1
		mu.Unlock()
		conn := &fakeConn{svc: svc}
		if first {
			// The job arrives, then the signal — cancellation is racing
			// the hand-back on purpose.
			svc.getJob = func(ecid string) (*compactioncoordinator.TNextCompactionJob, error) {
				job := translatableJob(ecid)
				cancel()
				return jobReply(job), nil
			}
		}
		return conn, nil
	}

	logger, _ := testLogger()
	cfg := testPollConfig(&scriptedResolver{results: []resolveResult{{addr: "manager-a:9999"}}}, dial)
	runPollLoop(ctx, logger, cfg)

	releases := svc.releases()
	if len(releases) != 1 {
		t.Fatalf("releases = %d, want the in-hand job released before exit", len(releases))
	}
	if releases[0].class != compactjob.ClassShuttingDown {
		t.Fatalf("class = %q, want %q", releases[0].class, compactjob.ClassShuttingDown)
	}
	if svc.completedCount() != 0 {
		t.Fatal("shoal reported a completed compaction")
	}
}

// TestDrainCoordinatorRejectsMismatchedECID guards against acting on a
// job this compactor did not request: the connection is dropped and no
// state is reported for the foreign compaction.
func TestDrainCoordinatorRejectsMismatchedECID(t *testing.T) {
	svc := &fakeCoordinator{
		getJob: func(string) (*compactioncoordinator.TNextCompactionJob, error) {
			job := tabletserver.NewTExternalCompactionJob()
			job.ExternalCompactionId = ecidPrefix + "0d1a4b0e-0000-0000-0000-000000000000"
			return &compactioncoordinator.TNextCompactionJob{Job: job, CompactorCount: 1}, nil
		},
	}

	cfg := testPollConfig(&scriptedResolver{results: []resolveResult{{addr: "manager-a:9999"}}}, nil)
	logger, logs := testLogger()
	if idle := drainCoordinator(context.Background(), logger, &fakeConn{svc: svc}, cfg); idle {
		t.Fatal("drain = idle, want reconnect after a mismatched ecid")
	}
	if len(svc.failed) != 0 {
		t.Fatalf("reported state for a foreign compaction: %+v", svc.failed)
	}
	if !strings.Contains(logs.String(), "mismatched ecid") {
		t.Fatalf("mismatch not logged:\n%s", logs.String())
	}
}

// lockReader is a fixed ZooKeeper view of the manager lock.
type lockReader struct {
	children []string
	data     []byte
}

func (l lockReader) InstancePath() string { return "/accumulo/uuid-1" }

func (l lockReader) Children(context.Context, string) ([]string, error) { return l.children, nil }

func (l lockReader) GetRaw(context.Context, string) ([]byte, error) { return l.data, nil }

// TestZKCoordinatorResolverReadsManagerLock pins discovery to the
// address the manager itself published — shoal never elects one.
func TestZKCoordinatorResolverReadsManagerLock(t *testing.T) {
	const lockNode = "zlock#407b5e9b-9d92-4aaa-bff0-d9ba9be206a6#0000000001"
	resolver := zkCoordinatorResolver{locator: lockReader{
		children: []string{lockNode},
		data: []byte(`{"descriptors":[
			{"service":"MANAGER","address":"manager-a:9999"},
			{"service":"COORDINATOR","address":"manager-a:9999"}]}`),
	}}
	addr, err := resolver.Address(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if addr != "manager-a:9999" {
		t.Fatalf("addr = %q, want manager-a:9999", addr)
	}

	// Bootstrap state: the manager holds the lock but has not published
	// its coordinator yet.
	resolver = zkCoordinatorResolver{locator: lockReader{
		children: []string{lockNode},
		data:     []byte(`{"descriptors":[{"service":"NONE","address":"0.0.0.0:0"}]}`),
	}}
	if _, err := resolver.Address(context.Background()); !errors.Is(err, zk.ErrCoordinatorUnavailable) {
		t.Fatalf("bootstrap error = %v, want ErrCoordinatorUnavailable", err)
	}
}

func TestStaticCoordinatorResolverPinsAddress(t *testing.T) {
	addr, err := staticCoordinatorResolver("manager-a:9999").Address(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if addr != "manager-a:9999" {
		t.Fatalf("addr = %q, want manager-a:9999", addr)
	}
	if _, err := staticCoordinatorResolver("").Address(context.Background()); err == nil {
		t.Fatal("empty pinned address = nil error, want failure")
	}
}

func TestNewECIDMatchesAccumuloFormat(t *testing.T) {
	first, second := newECID(), newECID()
	if !strings.HasPrefix(first, ecidPrefix) {
		t.Fatalf("ecid = %q, want %s prefix", first, ecidPrefix)
	}
	if first == second {
		t.Fatal("ecid repeated across calls")
	}
	// ExternalCompactionId.of parses the suffix with UUID.fromString and
	// throws on anything else, so an id the compactor generates but the
	// coordinator cannot parse would fail every poll. Translate applies
	// the same rule, which makes it the local proxy for that check.
	job := translatableJob(first)
	if _, err := compactjob.Translate(job, compactjob.Options{Limits: compactjob.DefaultLimits()}); err != nil {
		t.Fatalf("Translate(%q) = %v; the id this compactor generates must be one Accumulo accepts", first, err)
	}
}

// TestValidateLimitsRejectsNegativeBudgets: a negative budget is not a
// tighter limit, it is no limit — both the admission check and
// compaction.Compact read "not positive" as unlimited. A typo that
// silently removes a memory guard has to fail at startup instead.
func TestValidateLimitsRejectsNegativeBudgets(t *testing.T) {
	for _, tt := range []struct {
		name       string
		files      int
		inBytes    int64
		outBytes   int64
		wantErrFor string
	}{
		{name: "defaults are accepted",
			files: compactjob.DefaultMaxInputFiles, inBytes: compactjob.DefaultMaxTotalInputBytes,
			outBytes: compactjob.DefaultMaxOutputBytes},
		{name: "zero means unlimited everywhere"},
		{name: "negative input files", files: -1, wantErrFor: "-max-input-files"},
		{name: "negative input bytes", inBytes: -1, wantErrFor: "-max-input-bytes"},
		{name: "negative output bytes", outBytes: -1, wantErrFor: "-max-output-bytes"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := validateLimits(tt.files, tt.inBytes, tt.outBytes)
			if tt.wantErrFor == "" {
				if err != nil {
					t.Fatalf("validateLimits = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateLimits accepted a negative %s", tt.wantErrFor)
			}
			if !strings.Contains(err.Error(), tt.wantErrFor) {
				t.Fatalf("error %q does not name %s", err, tt.wantErrFor)
			}
		})
	}
}
