package compactexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/compaction"
	"github.com/phrocker/shoal-oss/internal/compactjob"
	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/rfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile/block"
	"github.com/phrocker/shoal-oss/internal/rfile/wire"
	"github.com/phrocker/shoal-oss/internal/storage"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/client"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/security"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/tabletserver"
)

type cell struct {
	row, value string
	ts         int64
}

func makeRFile(t *testing.T, cells ...cell) []byte {
	t.Helper()
	sort.Slice(cells, func(i, j int) bool {
		return cellKey(cells[i]).Compare(cellKey(cells[j])) < 0
	})
	var buf bytes.Buffer
	w, err := rfile.NewWriter(&buf, rfile.WriterOptions{Codec: block.CodecNone})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cells {
		if err := w.Append(cellKey(c), []byte(c.value)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func cellKey(c cell) *wire.Key {
	return &wire.Key{Row: []byte(c.row), Timestamp: c.ts}
}

func testPlan(inputs map[string][]byte, output string) *compactjob.Plan {
	files := make([]compactjob.InputFile, 0, len(inputs))
	var entries int64
	names := make([]string, 0, len(inputs))
	for name := range inputs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		files = append(files, compactjob.InputFile{
			Entry: name, Path: name, Size: int64(len(inputs[name])), Entries: 2,
		})
		entries += 2
	}
	return &compactjob.Plan{
		ECID:              compactjob.ECIDPrefix + "00000000-0000-0000-0000-000000000065",
		TableID:           "5",
		Extent:            &data.TKeyExtent{Table: []byte("5"), EndRow: []byte("z")},
		Inputs:            files,
		OutputFile:        output,
		TotalInputEntries: entries,
		Stack: []iterrt.IterSpec{{
			Name: iterrt.IterVersioning, Options: map[string]string{"maxVersions": "1"},
		}},
		Scope:               iterrt.ScopeMajc,
		FullMajorCompaction: true,
		Codec:               block.CodecNone,
	}
}

type recordingReporter struct {
	mu       sync.Mutex
	progress []Progress
	err      error
}

func (r *recordingReporter) Report(_ context.Context, p Progress) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.progress = append(r.progress, p)
	return r.err
}

func (r *recordingReporter) phases() []Phase {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Phase, len(r.progress))
	for i, p := range r.progress {
		out[i] = p.Phase
	}
	return out
}

func TestExecutePublishesDeterministicOutputAndStats(t *testing.T) {
	t.Parallel()
	inputs := map[string][]byte{
		"f1.rf": makeRFile(t, cell{"a", "old", 1}, cell{"b", "b", 1}),
		"f2.rf": makeRFile(t, cell{"a", "new", 2}, cell{"c", "c", 1}),
	}
	backend := memory.New()
	for path, image := range inputs {
		backend.Put(path, image)
	}
	backend.Put("out.rf_tmp_ECID", []byte("orphan"))
	reporter := &recordingReporter{}
	now := time.Unix(100, 0)
	exec, err := New(BackendStore{Backend: backend}, Options{
		Reporter:             reporter,
		ProgressEveryEntries: 1,
		Now: func() time.Time {
			now = now.Add(time.Second)
			return now
		},
		Sleep: func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	plan := testPlan(inputs, "out.rf_tmp_ECID")
	result, err := exec.Execute(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if result.Stats.EntriesRead != 4 || result.Stats.EntriesWritten != 3 {
		t.Fatalf("stats = %+v", result.Stats)
	}
	output, err := storage.ReadAll(context.Background(), backend, plan.OutputFile)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(output)) != result.Stats.FileSize || bytes.Equal(output, []byte("orphan")) {
		t.Fatalf("published output size/content mismatch")
	}
	again, err := compaction.Compact(plan.Spec([]compaction.Input{
		{Name: "f1.rf", Bytes: inputs["f1.rf"]},
		{Name: "f2.rf", Bytes: inputs["f2.rf"]},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output, again.Output) {
		t.Fatal("executor output is not deterministic")
	}
	phases := reporter.phases()
	if len(phases) < 6 || phases[0] != PhaseRecovering || phases[len(phases)-1] != PhaseCompleted {
		t.Fatalf("progress phases = %v", phases)
	}
}

type flakyStore struct {
	base        *memory.Backend
	readFails   atomic.Int32
	writeFails  atomic.Int32
	removeFails atomic.Int32
	reads       atomic.Int32
	writes      atomic.Int32
	removes     atomic.Int32
}

func (s *flakyStore) Read(ctx context.Context, path string) ([]byte, error) {
	s.reads.Add(1)
	if s.readFails.Add(-1) >= 0 {
		return nil, errors.New("transient read")
	}
	return storage.ReadAll(ctx, s.base, path)
}

func (s *flakyStore) Write(ctx context.Context, path string, data []byte) error {
	s.writes.Add(1)
	if s.writeFails.Add(-1) >= 0 {
		s.base.Put(path, []byte("partial"))
		return errors.New("transient write")
	}
	return (BackendStore{Backend: s.base}).Write(ctx, path, data)
}

func (s *flakyStore) Remove(ctx context.Context, path string) error {
	s.removes.Add(1)
	if s.removeFails.Add(-1) >= 0 {
		return errors.New("transient remove")
	}
	return s.base.Remove(ctx, path)
}

func TestExecuteRetriesStorageAndCleansFailedPublication(t *testing.T) {
	t.Parallel()
	image := makeRFile(t, cell{"a", "a", 1}, cell{"b", "b", 1})
	base := memory.New()
	base.Put("in.rf", image)
	store := &flakyStore{base: base}
	store.readFails.Store(1)
	store.writeFails.Store(1)
	exec, err := New(store, Options{
		Retry: RetryPolicy{Attempts: 3},
		Sleep: func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.Execute(context.Background(), testPlan(map[string][]byte{"in.rf": image}, "out.rf_tmp")); err != nil {
		t.Fatal(err)
	}
	if store.reads.Load() != 2 || store.writes.Load() != 2 {
		t.Fatalf("reads=%d writes=%d", store.reads.Load(), store.writes.Load())
	}
	out, err := storage.ReadAll(context.Background(), base, "out.rf_tmp")
	if err != nil || bytes.Equal(out, []byte("partial")) {
		t.Fatalf("output was not recovered after retry: %q %v", out, err)
	}
}

type blockingComposer struct {
	started chan struct{}
}

func (c blockingComposer) Compact(ctx context.Context, _ compaction.Spec, _ func(compaction.Progress)) (*compaction.Result, error) {
	close(c.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

type nilComposer struct{}

func (nilComposer) Compact(context.Context, compaction.Spec, func(compaction.Progress)) (*compaction.Result, error) {
	return nil, nil
}

func TestComposerReturningNilIsAnError(t *testing.T) {
	t.Parallel()
	image := makeRFile(t, cell{"a", "a", 1}, cell{"b", "b", 1})
	backend := memory.New()
	backend.Put("in.rf", image)
	exec, err := NewWithComposer(BackendStore{Backend: backend}, nilComposer{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.Execute(context.Background(), testPlan(map[string][]byte{"in.rf": image}, "out.rf_tmp")); err == nil {
		t.Fatal("expected nil composer result to fail")
	}
}

func TestCancellationNeverPublishes(t *testing.T) {
	t.Parallel()
	image := makeRFile(t, cell{"a", "a", 1}, cell{"b", "b", 1})
	backend := memory.New()
	backend.Put("in.rf", image)
	started := make(chan struct{})
	exec, err := NewWithComposer(BackendStore{Backend: backend}, blockingComposer{started: started}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := exec.Execute(ctx, testPlan(map[string][]byte{"in.rf": image}, "out.rf_tmp"))
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	if _, err := backend.Open(context.Background(), "out.rf_tmp"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("cancelled execution published output: %v", err)
	}
}

func TestCleanupAndRecoveryAreIdempotent(t *testing.T) {
	t.Parallel()
	backend := memory.New()
	backend.Put("out.rf_tmp", []byte("orphan"))
	exec, err := New(BackendStore{Backend: backend}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	plan := &compactjob.Plan{OutputFile: "out.rf_tmp"}
	if err := exec.Recover(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if err := exec.Recover(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	result := &Result{OutputFile: "out.rf_tmp", Stats: Stats{StartedAt: time.Now()}}
	if err := exec.Cleanup(context.Background(), result); err != nil {
		t.Fatal(err)
	}
}

type completionClient struct {
	mu     sync.Mutex
	ecid   string
	extent *data.TKeyExtent
	stats  *tabletserver.TCompactionStats
	err    error
}

func (c *completionClient) CompactionCompleted(_ context.Context, _ *client.TInfo, _ *security.TCredentials, ecid string, extent *data.TKeyExtent, stats *tabletserver.TCompactionStats) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ecid, c.extent, c.stats = ecid, extent, stats
	return c.err
}

func TestCompletionAdapterUsesExistingAccumuloContract(t *testing.T) {
	t.Parallel()
	rpc := &completionClient{}
	adapter, err := NewCompletionAdapter(rpc, security.NewTCredentials())
	if err != nil {
		t.Fatal(err)
	}
	result := &Result{
		ECID:   "ECID-00000000-0000-0000-0000-000000000065",
		Extent: &data.TKeyExtent{Table: []byte("5"), EndRow: []byte("m")},
		Stats:  Stats{EntriesRead: 9, EntriesWritten: 7, FileSize: 1234},
	}
	if err := adapter.Complete(context.Background(), result); err != nil {
		t.Fatal(err)
	}
	result.Extent.Table[0] = 'x'
	if rpc.ecid != result.ECID || string(rpc.extent.GetTable()) != "5" {
		t.Fatalf("identity not preserved: %q %+v", rpc.ecid, rpc.extent)
	}
	if rpc.stats.EntriesRead != 9 || rpc.stats.EntriesWritten != 7 || rpc.stats.FileSize != 1234 {
		t.Fatalf("stats = %+v", rpc.stats)
	}
}

func TestExecutorConcurrentUse(t *testing.T) {
	t.Parallel()
	backend := memory.New()
	exec, err := New(BackendStore{Backend: backend}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	const jobs = 8
	var wg sync.WaitGroup
	errs := make(chan error, jobs)
	for i := 0; i < jobs; i++ {
		i := i
		path := fmt.Sprintf("in-%d.rf", i)
		image := makeRFile(t, cell{fmt.Sprintf("r-%d", i), "v1", 1}, cell{fmt.Sprintf("s-%d", i), "v2", 1})
		backend.Put(path, image)
		plan := testPlan(map[string][]byte{path: image}, fmt.Sprintf("out-%d.rf_tmp", i))
		plan.ECID = fmt.Sprintf("ECID-00000000-0000-0000-0000-%012d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := exec.Execute(context.Background(), plan); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
