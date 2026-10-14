package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/phrocker/shoal-oss/internal/compactexec"
	"github.com/phrocker/shoal-oss/internal/compactjob"
	"github.com/phrocker/shoal-oss/internal/managerclient"
	"github.com/phrocker/shoal-oss/internal/roleops"
	"github.com/phrocker/shoal-oss/internal/storage/hdfs"
	"github.com/phrocker/shoal-oss/internal/tablenames"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/client"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/compactioncoordinator"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/security"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/tabletserver"
	"github.com/phrocker/shoal-oss/internal/zk"
)

type tableOptionsSource interface {
	Options(context.Context, string) (compactjob.Options, error)
}

type executor interface {
	Execute(context.Context, *compactjob.Plan) (*compactexec.Result, error)
	Cleanup(context.Context, *compactexec.Result) error
}

type completionRecord struct {
	Result    *compactexec.Result `json:"result"`
	Attempted bool                `json:"attempted"`
}

type completionJournal interface {
	Load() (*completionRecord, error)
	Save(*completionRecord) error
	Clear() error
}

type fileCompletionJournal struct {
	path string
	mu   sync.Mutex
}

func (j *fileCompletionJournal) Load() (*completionRecord, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	data, err := os.ReadFile(j.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read completion journal: %w", err)
	}
	var record completionRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, fmt.Errorf("decode completion journal: %w", err)
	}
	if record.Result == nil || record.Result.ECID == "" || record.Result.OutputFile == "" {
		return nil, fmt.Errorf("completion journal is incomplete")
	}
	if ecid, err := os.ReadFile(j.path + ".attempted"); err == nil {
		if string(ecid) != record.Result.ECID {
			return nil, fmt.Errorf("completion-attempt fence belongs to %s, not %s", ecid, record.Result.ECID)
		}
		record.Attempted = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read completion-attempt fence: %w", err)
	}
	return &record, nil
}

func (j *fileCompletionJournal) Save(record *completionRecord) error {
	if record == nil || record.Result == nil {
		return fmt.Errorf("completion journal record is required")
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(j.path), 0o700); err != nil {
		return fmt.Errorf("create completion journal directory: %w", err)
	}
	if record.Attempted {
		existing, err := readCompletionRecord(j.path)
		if err != nil {
			return err
		}
		if existing.Result.ECID != record.Result.ECID {
			return fmt.Errorf("completion journal belongs to %s, not %s", existing.Result.ECID, record.Result.ECID)
		}
		fence, err := os.OpenFile(j.path+".attempted", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if errors.Is(err, os.ErrExist) {
			ecid, readErr := os.ReadFile(j.path + ".attempted")
			if readErr != nil {
				return fmt.Errorf("read completion-attempt fence: %w", readErr)
			}
			if string(ecid) != record.Result.ECID {
				return fmt.Errorf("completion-attempt fence belongs to %s, not %s", ecid, record.Result.ECID)
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("create completion-attempt fence: %w", err)
		}
		if _, err := fence.WriteString(record.Result.ECID); err != nil {
			_ = fence.Close()
			return fmt.Errorf("write completion-attempt fence: %w", err)
		}
		if err := fence.Sync(); err != nil {
			_ = fence.Close()
			return fmt.Errorf("sync completion-attempt fence: %w", err)
		}
		return fence.Close()
	}
	if _, err := os.Stat(j.path); err == nil {
		existing, readErr := readCompletionRecord(j.path)
		if readErr != nil {
			return readErr
		}
		if existing.Result.ECID == record.Result.ECID {
			return nil
		}
		return fmt.Errorf("completion journal already belongs to %s", existing.Result.ECID)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Remove(j.path + ".attempted"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale completion-attempt fence: %w", err)
	}
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode completion journal: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(j.path), ".shoal-compactor-*")
	if err != nil {
		return fmt.Errorf("create completion journal: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write completion journal: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync completion journal: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close completion journal: %w", err)
	}
	if err := os.Rename(tmpName, j.path); err != nil {
		return fmt.Errorf("publish completion journal: %w", err)
	}
	return nil
}

func (j *fileCompletionJournal) Clear() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	var combined error
	for _, path := range []string{j.path, j.path + ".attempted"} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			combined = errors.Join(combined, err)
		}
	}
	return combined
}

func readCompletionRecord(path string) (*completionRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read completion journal: %w", err)
	}
	var record completionRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, fmt.Errorf("decode completion journal: %w", err)
	}
	if record.Result == nil || record.Result.ECID == "" || record.Result.OutputFile == "" {
		return nil, fmt.Errorf("completion journal is incomplete")
	}
	return &record, nil
}

type worker struct {
	logger         *slog.Logger
	creds          *security.TCredentials
	config         tableOptionsSource
	store          compactexec.Store
	journal        completionJournal
	cancelEvery    time.Duration
	cleanupTimeout time.Duration
	reconcileGrace time.Duration
	limits         compactjob.Limits
	isRunning      func(context.Context, string) (bool, error)
	newExecutor    func(compactexec.Reporter) (executor, error)
	metrics        *workerMetrics
	validateStore  func(*compactjob.Plan) error
	now            func() time.Time
	role           *compactorRole
}

type jobOutcome struct {
	completed           bool
	coordinatorReleased bool
	ambiguous           bool
	class               string
}

func (w *worker) process(
	ctx context.Context,
	svc compactioncoordinator.CompactionCoordinatorService,
	job *tabletserver.TExternalCompactionJob,
) jobOutcome {
	base := compactjob.Options{Limits: w.limits}
	if _, err := compactjob.Translate(job, base); err != nil {
		return jobOutcome{class: refusalClass(err, compactjob.ClassMalformedJob)}
	}
	jobCtx, cancelJob := context.WithCancel(ctx)
	defer cancelJob()
	var coordinatorCancelled atomic.Bool
	if w.role != nil {
		w.role.begin(job, cancelJob, &coordinatorCancelled)
		defer w.role.end(job.GetExternalCompactionId())
	}

	opts, err := w.config.Options(jobCtx, string(job.GetExtent().GetTable()))
	if err != nil {
		w.logger.Warn("resolve table configuration failed",
			slog.String("ecid", job.GetExternalCompactionId()), slog.String("err", err.Error()))
		return jobOutcome{class: refusalClass(err, compactjob.ClassConfigurationUnavailable)}
	}
	plan, err := compactjob.Translate(job, opts)
	if err != nil {
		return jobOutcome{class: refusalClass(err, compactjob.ClassMalformedJob)}
	}
	if w.validateStore != nil {
		if err := w.validateStore(plan); err != nil {
			w.logger.Warn("storage path refused", slog.String("ecid", plan.ECID), slog.String("err", err.Error()))
			return jobOutcome{class: compactjob.ClassUnsupportedVolume}
		}
	}

	reporter := coordinatorReporter{svc: svc, creds: w.creds, ecid: plan.ECID, metrics: w.metrics}
	exec, err := w.newExecutor(reporter)
	if err != nil {
		return jobOutcome{class: compactjob.ClassExecutionFailed}
	}
	monitorCtx, stopMonitor := context.WithCancel(jobCtx)
	monitorDone := make(chan struct{})
	if w.cancelEvery > 0 && w.isRunning != nil {
		go w.monitorCancellation(monitorCtx, cancelJob, plan.ECID, &coordinatorCancelled, monitorDone)
	} else {
		close(monitorDone)
	}
	w.metrics.activeJobs.Add(1)
	result, err := exec.Execute(jobCtx, plan)
	w.metrics.activeJobs.Add(-1)
	stopMonitor()
	<-monitorDone
	if err != nil {
		w.metrics.failed.Add(1)
		if errors.Is(err, context.Canceled) {
			if coordinatorCancelled.Load() {
				w.metrics.cancelled.Add(1)
				return jobOutcome{coordinatorReleased: true}
			}
			if jobCtx.Err() != nil {
				w.metrics.cancelled.Add(1)
				return jobOutcome{class: compactjob.ClassShuttingDown}
			}
			w.logger.Error("compaction execution returned cancellation while job context remained active",
				slog.String("ecid", plan.ECID), slog.String("err", err.Error()))
			return jobOutcome{class: compactjob.ClassExecutionFailed}
		}
		w.logger.Error("compaction execution failed",
			slog.String("ecid", plan.ECID), slog.String("err", err.Error()))
		return jobOutcome{class: compactjob.ClassExecutionFailed}
	}
	if coordinatorCancelled.Load() || jobCtx.Err() != nil {
		w.metrics.cancelled.Add(1)
		if result != nil {
			_ = w.cleanup(ctx, exec, result)
		}
		if coordinatorCancelled.Load() {
			return jobOutcome{coordinatorReleased: true}
		}
		return jobOutcome{class: compactjob.ClassShuttingDown}
	}
	w.metrics.executed.Add(1)

	record := &completionRecord{Result: result}
	if err := w.journal.Save(record); err != nil {
		_ = w.cleanup(ctx, exec, result)
		w.logger.Error("persist completion intent failed",
			slog.String("ecid", plan.ECID), slog.String("err", err.Error()))
		return jobOutcome{class: compactjob.ClassExecutionFailed}
	}
	record.Attempted = true
	if err := w.journal.Save(record); err != nil {
		w.logger.Error("persist completion-attempt fence failed",
			slog.String("ecid", plan.ECID), slog.String("err", err.Error()))
		return jobOutcome{ambiguous: true}
	}

	adapter, err := compactexec.NewCompletionAdapter(svc, w.creds)
	if err == nil {
		err = adapter.Complete(ctx, result)
	}
	if err == nil {
		if clearErr := w.journal.Clear(); clearErr != nil {
			w.logger.Error("clear completion journal failed",
				slog.String("ecid", plan.ECID), slog.String("err", clearErr.Error()))
			return jobOutcome{ambiguous: true}
		}
		w.metrics.completed.Add(1)
		return jobOutcome{completed: true}
	}

	w.metrics.completionAmbiguous.Add(1)
	w.metrics.retries.Add(1)
	w.logger.Warn("compactionCompleted outcome is ambiguous; retaining output and reconciling",
		slog.String("ecid", plan.ECID), slog.String("err", err.Error()))
	resolved, pending, reconcileErr := w.reconcile(ctx, svc, exec, record)
	if reconcileErr != nil {
		w.logger.Warn("completion reconciliation failed",
			slog.String("ecid", plan.ECID), slog.String("err", reconcileErr.Error()))
		return jobOutcome{ambiguous: true}
	}
	if resolved {
		return jobOutcome{completed: true}
	}
	return jobOutcome{ambiguous: pending}
}

func (w *worker) reconcilePending(
	ctx context.Context,
	svc compactioncoordinator.CompactionCoordinatorService,
) (bool, error) {
	record, err := w.journal.Load()
	if err != nil || record == nil {
		return false, err
	}
	exec, err := w.newExecutor(nil)
	if err != nil {
		return true, err
	}
	if !record.Attempted {
		record.Attempted = true
		if err := w.journal.Save(record); err != nil {
			return true, err
		}
		adapter, err := compactexec.NewCompletionAdapter(svc, w.creds)
		if err == nil {
			err = adapter.Complete(ctx, record.Result)
		}
		if err == nil {
			w.metrics.completed.Add(1)
			return false, w.journal.Clear()
		}
	}
	resolved, pending, err := w.reconcile(ctx, svc, exec, record)
	return pending && !resolved, err
}

func (w *worker) reconcile(
	ctx context.Context,
	svc compactioncoordinator.CompactionCoordinatorService,
	exec executor,
	record *completionRecord,
) (resolved, pending bool, err error) {
	completed, err := svc.GetCompletedCompactions(ctx, client.NewTInfo(), w.creds)
	if err != nil {
		return false, true, err
	}
	if containsCompaction(completed, record.Result.ECID) {
		w.metrics.completed.Add(1)
		return true, false, w.journal.Clear()
	}
	running, err := svc.GetRunningCompactions(ctx, client.NewTInfo(), w.creds)
	if err != nil {
		return false, true, err
	}
	if containsCompaction(running, record.Result.ECID) {
		return false, true, nil
	}
	now := time.Now
	if w.now != nil {
		now = w.now
	}
	grace := w.reconcileGrace
	if grace <= 0 {
		grace = 2 * time.Minute
	}
	if finished := record.Result.Stats.FinishedAt; finished.IsZero() || now().Sub(finished) < grace {
		return false, true, nil
	}
	if err := w.cleanup(ctx, exec, record.Result); err != nil {
		return false, false, err
	}
	return false, false, w.journal.Clear()
}

func (w *worker) cleanup(parent context.Context, exec executor, result *compactexec.Result) error {
	timeout := w.cleanupTimeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), timeout)
	defer cancel()
	return exec.Cleanup(ctx, result)
}

func containsCompaction(m *compactioncoordinator.TExternalCompactionMap, ecid string) bool {
	if m == nil {
		return false
	}
	_, ok := m.GetCompactions()[ecid]
	return ok
}

func (w *worker) monitorCancellation(
	ctx context.Context,
	cancel context.CancelFunc,
	ecid string,
	coordinatorCancelled *atomic.Bool,
	done chan<- struct{},
) {
	defer close(done)
	ticker := time.NewTicker(w.cancelEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			running, err := w.isRunning(ctx, ecid)
			if err != nil {
				w.logger.Debug("cancellation check failed", slog.String("ecid", ecid), slog.String("err", err.Error()))
				continue
			}
			if !running {
				w.logger.Info("coordinator cancelled compaction", slog.String("ecid", ecid))
				coordinatorCancelled.Store(true)
				cancel()
				return
			}
		}
	}
}

func refusalClass(err error, fallback string) string {
	if refusal := compactjob.RefusalOf(err); refusal != nil {
		return refusal.Class
	}
	return fallback
}

type coordinatorReporter struct {
	svc     compactioncoordinator.CompactionCoordinatorService
	creds   *security.TCredentials
	ecid    string
	metrics *workerMetrics
}

func (r coordinatorReporter) Report(ctx context.Context, p compactexec.Progress) error {
	if r.metrics != nil {
		r.metrics.progressReports.Add(1)
		r.metrics.entriesRead.Store(uint64(max(p.EntriesRead, 0)))
		r.metrics.entriesWritten.Store(uint64(max(p.EntriesWritten, 0)))
	}
	state := compactioncoordinator.TCompactionState_IN_PROGRESS
	if p.Phase == compactexec.PhaseRecovering || p.Phase == compactexec.PhaseReading {
		state = compactioncoordinator.TCompactionState_STARTED
	} else if p.Phase == compactexec.PhaseCompleted {
		state = compactioncoordinator.TCompactionState_SUCCEEDED
	}
	return r.svc.UpdateCompactionStatus(ctx, client.NewTInfo(), r.creds, r.ecid,
		&compactioncoordinator.TCompactionStatusUpdate{
			State:                state,
			Message:              string(p.Phase),
			EntriesToBeCompacted: p.EntriesToBeCompacted,
			EntriesRead:          p.EntriesRead,
			EntriesWritten:       p.EntriesWritten,
			CompactionAgeNanos:   p.Age.Nanoseconds(),
		}, time.Now().UnixNano())
}

type effectiveTableOptions struct {
	locator zk.LockReader
	names   *tablenames.Resolver
	manager tableConfigurationClient
	limits  compactjob.Limits
}

type tableConfigurationClient interface {
	GetTableConfiguration(context.Context, string, string) (map[string]string, error)
	GetVersionedTableProperties(context.Context, string, string) (managerclient.VersionedProperties, error)
}

func (s effectiveTableOptions) Options(ctx context.Context, tableID string) (compactjob.Options, error) {
	s.names.Invalidate()
	name, err := s.names.ResolveName(ctx, tableID)
	if err != nil {
		return compactjob.Options{}, err
	}
	addresses, err := zk.ClientServiceAddresses(ctx, s.locator)
	if err != nil {
		return compactjob.Options{}, err
	}
	var combined error
	for _, address := range addresses {
		properties, err := stableVersionedTableProperties(
			ctx,
			func(ctx context.Context) (managerclient.VersionedProperties, error) {
				return s.manager.GetVersionedTableProperties(ctx, address, name)
			},
			func(ctx context.Context) (map[string]string, error) {
				return s.manager.GetTableConfiguration(ctx, address, name)
			},
		)
		if err == nil {
			return compactjob.OptionsFromTableProperties(properties, s.limits)
		}
		combined = errors.Join(combined, err)
	}
	return compactjob.Options{}, fmt.Errorf("resolve table configuration for %s: %w", name, combined)
}

func stableTableProperties(
	ctx context.Context,
	read func(context.Context) (map[string]string, error),
) (map[string]string, error) {
	first, err := read(ctx)
	if err != nil {
		return nil, err
	}
	second, err := read(ctx)
	if err != nil {
		return nil, err
	}
	if !maps.Equal(first, second) {
		return nil, fmt.Errorf("effective table configuration changed during resolution")
	}
	return first, nil
}

func stableVersionedTableProperties(
	ctx context.Context,
	readVersion func(context.Context) (managerclient.VersionedProperties, error),
	readEffective func(context.Context) (map[string]string, error),
) (map[string]string, error) {
	before, err := readVersion(ctx)
	if err != nil {
		return nil, err
	}
	properties, err := stableTableProperties(ctx, readEffective)
	if err != nil {
		return nil, err
	}
	after, err := readVersion(ctx)
	if err != nil {
		return nil, err
	}
	if before.Version != after.Version {
		return nil, fmt.Errorf("table properties changed from generation %d to %d", before.Version, after.Version)
	}
	return properties, nil
}

type workerMetrics struct {
	ready               atomic.Bool
	accepting           atomic.Bool
	storageReady        atomic.Bool
	roleServiceReady    atomic.Bool
	journalReady        atomic.Bool
	activeJobs          atomic.Int64
	executed            atomic.Uint64
	completed           atomic.Uint64
	failed              atomic.Uint64
	cancelled           atomic.Uint64
	completionAmbiguous atomic.Uint64
	progressReports     atomic.Uint64
	entriesRead         atomic.Uint64
	entriesWritten      atomic.Uint64
	retries             atomic.Uint64
}

func hdfsPlanValidator(configuredAddress string) func(*compactjob.Plan) error {
	var configErr error
	if strings.Contains(configuredAddress, "://") {
		configuredAddress, configErr = hdfs.AddressFromPath(configuredAddress)
	}
	return func(plan *compactjob.Plan) error {
		if configErr != nil {
			return configErr
		}
		paths := make([]string, 0, len(plan.Inputs)+1)
		for _, input := range plan.Inputs {
			paths = append(paths, input.Path)
		}
		paths = append(paths, plan.OutputFile)
		for _, objectPath := range paths {
			address, err := hdfs.AddressFromPath(objectPath)
			if err != nil {
				return err
			}
			if address != "" && configuredAddress == "" {
				return fmt.Errorf("qualified HDFS path %q requires -hdfs-namenode", objectPath)
			}
			if address != "" && !strings.EqualFold(address, configuredAddress) {
				return fmt.Errorf("HDFS path authority %q does not match configured namenode %q", address, configuredAddress)
			}
		}
		return nil
	}
}

func workerOperationsHandler(metrics *workerMetrics) http.Handler {
	dependencies := roleops.NewDependencies(
		"coordinator_session", "storage", "completion_journal", "role_service", "job_admission",
	)
	dependencies.SetStarted(true)
	handler := roleops.Handler(dependencies, func(b *strings.Builder) {
		_, _ = fmt.Fprintf(b,
			"# TYPE shoal_compactor_jobs_active gauge\nshoal_compactor_jobs_active %d\n"+
				"# TYPE shoal_compactor_jobs_executed_total counter\nshoal_compactor_jobs_executed_total %d\n"+
				"# TYPE shoal_compactor_jobs_completed_total counter\nshoal_compactor_jobs_completed_total %d\n"+
				"# TYPE shoal_compactor_jobs_failed_total counter\nshoal_compactor_jobs_failed_total %d\n"+
				"# TYPE shoal_compactor_jobs_cancelled_total counter\nshoal_compactor_jobs_cancelled_total %d\n"+
				"# TYPE shoal_compactor_completion_ambiguous_total counter\nshoal_compactor_completion_ambiguous_total %d\n"+
				"# TYPE shoal_compactor_progress_reports_total counter\nshoal_compactor_progress_reports_total %d\n"+
				"# TYPE shoal_compactor_entries_read gauge\nshoal_compactor_entries_read %d\n"+
				"# TYPE shoal_compactor_entries_written gauge\nshoal_compactor_entries_written %d\n"+
				"# TYPE shoal_compactor_retries_total counter\nshoal_compactor_retries_total %d\n",
			metrics.activeJobs.Load(), metrics.executed.Load(), metrics.completed.Load(), metrics.failed.Load(),
			metrics.cancelled.Load(), metrics.completionAmbiguous.Load(), metrics.progressReports.Load(),
			metrics.entriesRead.Load(), metrics.entriesWritten.Load(), metrics.retries.Load())
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dependencies.Set("coordinator_session", metrics.ready.Load(), stateDetail(metrics.ready.Load(), "connected", "unavailable"))
		dependencies.Set("storage", metrics.storageReady.Load(), stateDetail(metrics.storageReady.Load(), "validated", "unavailable"))
		dependencies.Set("completion_journal", metrics.journalReady.Load(), stateDetail(metrics.journalReady.Load(), "writable", "unavailable"))
		dependencies.Set("role_service", metrics.roleServiceReady.Load(), stateDetail(metrics.roleServiceReady.Load(), "serving", "unavailable"))
		dependencies.Set("job_admission", metrics.accepting.Load(), stateDetail(metrics.accepting.Load(), "accepting", "draining"))
		handler.ServeHTTP(w, r)
	})
}

func stateDetail(ready bool, yes, no string) string {
	if ready {
		return yes
	}
	return no
}
