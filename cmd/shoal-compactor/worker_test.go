package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/compactexec"
	"github.com/phrocker/shoal-oss/internal/compactjob"
	"github.com/phrocker/shoal-oss/internal/managerclient"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/client"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/compactioncoordinator"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/security"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/tabletserver"
)

type staticTableOptions struct {
	options compactjob.Options
	err     error
	calls   atomic.Int32
}

func (s *staticTableOptions) Options(context.Context, string) (compactjob.Options, error) {
	s.calls.Add(1)
	return s.options, s.err
}

type memoryJournal struct {
	mu     sync.Mutex
	record *completionRecord
}

func (j *memoryJournal) Load() (*completionRecord, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.record == nil {
		return nil, nil
	}
	copy := *j.record
	return &copy, nil
}
func (j *memoryJournal) Save(record *completionRecord) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	copy := *record
	j.record = &copy
	return nil
}
func (j *memoryJournal) Clear() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.record = nil
	return nil
}

type fakeExecutor struct {
	result       *compactexec.Result
	err          error
	waitForStop  bool
	executeCalls atomic.Int32
	cleanupCalls atomic.Int32
}

func (e *fakeExecutor) Execute(ctx context.Context, _ *compactjob.Plan) (*compactexec.Result, error) {
	e.executeCalls.Add(1)
	if e.waitForStop {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return e.result, e.err
}
func (e *fakeExecutor) Cleanup(context.Context, *compactexec.Result) error {
	e.cleanupCalls.Add(1)
	return nil
}

type workerCoordinator struct {
	compactioncoordinator.CompactionCoordinatorService
	mu             sync.Mutex
	completionErr  error
	completions    int
	completedECID  string
	completedStats *tabletserver.TCompactionStats
	statuses       int
	running        map[string]*compactioncoordinator.TExternalCompaction
	completed      map[string]*compactioncoordinator.TExternalCompaction
}

func (c *workerCoordinator) CompactionCompleted(
	_ context.Context, _ *client.TInfo, _ *security.TCredentials, ecid string, _ *data.TKeyExtent, stats *tabletserver.TCompactionStats,
) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.completions++
	c.completedECID = ecid
	c.completedStats = stats
	return c.completionErr
}
func (c *workerCoordinator) UpdateCompactionStatus(
	context.Context, *client.TInfo, *security.TCredentials, string,
	*compactioncoordinator.TCompactionStatusUpdate, int64,
) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.statuses++
	return nil
}
func (c *workerCoordinator) GetRunningCompactions(
	context.Context, *client.TInfo, *security.TCredentials,
) (*compactioncoordinator.TExternalCompactionMap, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return &compactioncoordinator.TExternalCompactionMap{Compactions: c.running}, nil
}
func (c *workerCoordinator) GetCompletedCompactions(
	context.Context, *client.TInfo, *security.TCredentials,
) (*compactioncoordinator.TExternalCompactionMap, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return &compactioncoordinator.TExternalCompactionMap{Compactions: c.completed}, nil
}

func workerResult(job *tabletserver.TExternalCompactionJob) *compactexec.Result {
	return &compactexec.Result{
		ECID: job.GetExternalCompactionId(), Extent: job.GetExtent(), OutputFile: job.GetOutputFile(),
		Stats: compactexec.Stats{EntriesRead: 40, EntriesWritten: 20, FileSize: 100, FinishedAt: time.Now()},
	}
}

func testWorker(t *testing.T, config tableOptionsSource, exec *fakeExecutor, journal completionJournal) *worker {
	t.Helper()
	logger, _ := testLogger()
	return &worker{
		logger: logger, creds: security.NewTCredentials(), config: config, journal: journal,
		limits: compactjob.DefaultLimits(), metrics: &workerMetrics{},
		newExecutor: func(compactexec.Reporter) (executor, error) {
			return exec, nil
		},
	}
}

func TestWorkerCompletesExactlyOnce(t *testing.T) {
	job := translatableJob(newECID())
	exec := &fakeExecutor{result: workerResult(job)}
	journal := &memoryJournal{}
	config := &staticTableOptions{options: compactjob.Options{Limits: compactjob.DefaultLimits()}}
	coordinator := &workerCoordinator{}
	w := testWorker(t, config, exec, journal)

	outcome := w.process(context.Background(), coordinator, job)
	if !outcome.completed || outcome.ambiguous || outcome.class != "" {
		t.Fatalf("outcome = %+v", outcome)
	}
	if coordinator.completions != 1 || exec.executeCalls.Load() != 1 {
		t.Fatalf("completion/execution calls = %d/%d", coordinator.completions, exec.executeCalls.Load())
	}
	if coordinator.completedECID != job.GetExternalCompactionId() ||
		coordinator.completedStats.GetEntriesRead() != 40 ||
		coordinator.completedStats.GetEntriesWritten() != 20 ||
		coordinator.completedStats.GetFileSize() != 100 {
		t.Fatalf("completion identity/stats = %q %+v", coordinator.completedECID, coordinator.completedStats)
	}
	if record, _ := journal.Load(); record != nil {
		t.Fatal("completion journal was not cleared")
	}
}

func TestWorkerDoesNotTreatExecutorCancellationAsShutdown(t *testing.T) {
	job := translatableJob(newECID())
	exec := &fakeExecutor{err: context.Canceled}
	config := &staticTableOptions{options: compactjob.Options{Limits: compactjob.DefaultLimits()}}
	w := testWorker(t, config, exec, &memoryJournal{})

	outcome := w.process(context.Background(), &workerCoordinator{}, job)
	if outcome.class != compactjob.ClassExecutionFailed || outcome.coordinatorReleased {
		t.Fatalf("outcome = %+v", outcome)
	}
	if w.metrics.cancelled.Load() != 0 || w.metrics.failed.Load() != 1 {
		t.Fatalf("cancelled/failed = %d/%d, want 0/1",
			w.metrics.cancelled.Load(), w.metrics.failed.Load())
	}
}

func TestWorkerRefusesConfigFailureBeforeExecution(t *testing.T) {
	job := translatableJob(newECID())
	exec := &fakeExecutor{result: workerResult(job)}
	config := &staticTableOptions{err: errors.New("configuration unavailable")}
	w := testWorker(t, config, exec, &memoryJournal{})

	outcome := w.process(context.Background(), &workerCoordinator{}, job)
	if outcome.class != compactjob.ClassConfigurationUnavailable {
		t.Fatalf("outcome = %+v", outcome)
	}
	if exec.executeCalls.Load() != 0 {
		t.Fatal("executor ran before configuration resolved")
	}
}

func TestWorkerReconcilesAmbiguousCompletionWithoutSecondCall(t *testing.T) {
	job := translatableJob(newECID())
	exec := &fakeExecutor{result: workerResult(job)}
	journal := &memoryJournal{}
	config := &staticTableOptions{options: compactjob.Options{Limits: compactjob.DefaultLimits()}}
	coordinator := &workerCoordinator{
		completionErr: errors.New("connection reset"),
		running: map[string]*compactioncoordinator.TExternalCompaction{
			job.GetExternalCompactionId(): {},
		},
	}
	w := testWorker(t, config, exec, journal)

	outcome := w.process(context.Background(), coordinator, job)
	if !outcome.ambiguous || coordinator.completions != 1 {
		t.Fatalf("outcome/calls = %+v/%d", outcome, coordinator.completions)
	}
	coordinator.mu.Lock()
	coordinator.running = nil
	coordinator.completed = map[string]*compactioncoordinator.TExternalCompaction{
		job.GetExternalCompactionId(): {},
	}
	coordinator.mu.Unlock()
	pending, err := w.reconcilePending(context.Background(), coordinator)
	if err != nil || pending {
		t.Fatalf("reconcile pending=%v err=%v", pending, err)
	}
	if coordinator.completions != 1 {
		t.Fatalf("compactionCompleted called %d times", coordinator.completions)
	}
	if exec.cleanupCalls.Load() != 0 {
		t.Fatal("accepted output was cleaned up")
	}
}

func TestWorkerCancellationStopsExecutionAndSkipsCompletion(t *testing.T) {
	job := translatableJob(newECID())
	exec := &fakeExecutor{waitForStop: true}
	config := &staticTableOptions{options: compactjob.Options{Limits: compactjob.DefaultLimits()}}
	coordinator := &workerCoordinator{}
	w := testWorker(t, config, exec, &memoryJournal{})
	w.cancelEvery = time.Millisecond
	w.isRunning = func(context.Context, string) (bool, error) { return false, nil }

	outcome := w.process(context.Background(), coordinator, job)
	if !outcome.coordinatorReleased || outcome.class != "" {
		t.Fatalf("outcome = %+v", outcome)
	}
	if coordinator.completions != 0 {
		t.Fatal("cancelled job reported completion")
	}
}

func TestWorkerDoesNotCleanupDuringCompletionMapTransition(t *testing.T) {
	job := translatableJob(newECID())
	exec := &fakeExecutor{result: workerResult(job)}
	journal := &memoryJournal{}
	config := &staticTableOptions{options: compactjob.Options{Limits: compactjob.DefaultLimits()}}
	coordinator := &workerCoordinator{completionErr: errors.New("timeout")}
	w := testWorker(t, config, exec, journal)
	w.reconcileGrace = time.Minute

	outcome := w.process(context.Background(), coordinator, job)
	if !outcome.ambiguous || exec.cleanupCalls.Load() != 0 {
		t.Fatalf("transition outcome=%+v cleanup=%d", outcome, exec.cleanupCalls.Load())
	}
	coordinator.mu.Lock()
	coordinator.completed = map[string]*compactioncoordinator.TExternalCompaction{
		job.GetExternalCompactionId(): {},
	}
	coordinator.mu.Unlock()
	pending, err := w.reconcilePending(context.Background(), coordinator)
	if err != nil || pending || exec.cleanupCalls.Load() != 0 {
		t.Fatalf("reconcile pending=%v cleanup=%d err=%v", pending, exec.cleanupCalls.Load(), err)
	}
}

func TestWorkerRestartCompletesUnattemptedIntentOnce(t *testing.T) {
	job := translatableJob(newECID())
	journal := &memoryJournal{record: &completionRecord{Result: workerResult(job)}}
	exec := &fakeExecutor{}
	config := &staticTableOptions{options: compactjob.Options{Limits: compactjob.DefaultLimits()}}
	coordinator := &workerCoordinator{}
	w := testWorker(t, config, exec, journal)

	pending, err := w.reconcilePending(context.Background(), coordinator)
	if err != nil || pending {
		t.Fatalf("reconcile pending=%v err=%v", pending, err)
	}
	pending, err = w.reconcilePending(context.Background(), coordinator)
	if err != nil || pending || coordinator.completions != 1 {
		t.Fatalf("duplicate reconcile pending=%v calls=%d err=%v", pending, coordinator.completions, err)
	}
}

func TestHDFSPlanValidatorRejectsWrongAuthority(t *testing.T) {
	job := translatableJob(newECID())
	plan, err := compactjob.Translate(job, compactjob.Options{Limits: compactjob.DefaultLimits()})
	if err != nil {
		t.Fatal(err)
	}
	if err := hdfsPlanValidator("other:8020")(plan); err == nil {
		t.Fatal("wrong HDFS authority accepted")
	}
}

func TestStableTablePropertiesRejectsConfigurationRace(t *testing.T) {
	var calls int
	_, err := stableTableProperties(context.Background(), func(context.Context) (map[string]string, error) {
		calls++
		return map[string]string{"table.file.compress.type": map[int]string{1: "gz", 2: "snappy"}[calls]}, nil
	})
	if err == nil {
		t.Fatal("configuration changed during resolution but was accepted")
	}
}

func TestStableVersionedTablePropertiesRejectsGenerationRace(t *testing.T) {
	var versions int
	_, err := stableVersionedTableProperties(
		context.Background(),
		func(context.Context) (managerclient.VersionedProperties, error) {
			versions++
			return managerclient.VersionedProperties{Version: int64(versions)}, nil
		},
		func(context.Context) (map[string]string, error) {
			return map[string]string{"table.file.type": "rf"}, nil
		},
	)
	if err == nil {
		t.Fatal("table generation changed during resolution but was accepted")
	}
}

func TestWorkerReadinessAndMetrics(t *testing.T) {
	metrics := &workerMetrics{}
	handler := workerOperationsHandler(metrics)
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("not-ready status = %d", resp.Code)
	}
	metrics.ready.Store(true)
	metrics.accepting.Store(true)
	metrics.storageReady.Store(true)
	metrics.roleServiceReady.Store(true)
	metrics.journalReady.Store(true)
	metrics.completed.Add(2)
	resp = httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("ready status = %d", resp.Code)
	}
	resp = httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if body := resp.Body.String(); body == "" || !strings.Contains(body, "shoal_compactor_jobs_completed_total 2") {
		t.Fatalf("metrics body = %q", body)
	}
}

func TestFileCompletionJournalFencesAttemptAcrossUpdates(t *testing.T) {
	dir, err := os.MkdirTemp(".", ".journal-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	job := translatableJob(newECID())
	journal := &fileCompletionJournal{path: filepath.Join(dir, "completion.json")}
	record := &completionRecord{Result: workerResult(job)}
	if err := journal.Save(record); err != nil {
		t.Fatal(err)
	}
	record.Attempted = true
	if err := journal.Save(record); err != nil {
		t.Fatal(err)
	}
	loaded, err := journal.Load()
	if err != nil || loaded == nil || !loaded.Attempted {
		t.Fatalf("loaded=%+v err=%v", loaded, err)
	}
	if err := journal.Clear(); err != nil {
		t.Fatal(err)
	}
	if loaded, err := journal.Load(); err != nil || loaded != nil {
		t.Fatalf("after clear loaded=%+v err=%v", loaded, err)
	}
}
