package accumulo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/ingestclient"
	"github.com/phrocker/shoal-oss/internal/metadata"
	clientpkg "github.com/phrocker/shoal-oss/internal/thrift/gen/client"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
)

type fakeBatchWriterIngest struct {
	mu       sync.Mutex
	sessions []*fakeBatchWriterSession
	starts   []string
}

func (f *fakeBatchWriterIngest) start(
	_ context.Context,
	address string,
	_ ingestclient.Durability,
) (batchWriterSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	session := &fakeBatchWriterSession{}
	f.starts = append(f.starts, address)
	f.sessions = append(f.sessions, session)
	return session, nil
}

type blockingBatchWriterWalker struct {
	entered chan struct{}
}

func (w *blockingBatchWriterWalker) LocateTable(
	ctx context.Context,
	_ string,
) ([]metadata.TabletInfo, error) {
	close(w.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}

type sequencedBatchWriterWalker struct {
	mu        sync.Mutex
	snapshots [][]metadata.TabletInfo
	calls     int
}

func (w *sequencedBatchWriterWalker) LocateTable(
	ctx context.Context,
	_ string,
) ([]metadata.TabletInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	index := w.calls
	w.calls++
	if index >= len(w.snapshots) {
		index = len(w.snapshots) - 1
	}
	return append([]metadata.TabletInfo(nil), w.snapshots[index]...), nil
}

type batchWriterApply struct {
	extent    *data.TKeyExtent
	mutations []*data.TMutation
}

type fakeBatchWriterSession struct {
	mu sync.Mutex

	applies       []batchWriterApply
	closeCalls    int
	cancelCalls   int
	closeResult   *data.UpdateErrors
	closeErr      error
	applyErr      error
	cancelResult  bool
	cancelErr     error
	applyEntered  chan struct{}
	applyRelease  <-chan struct{}
	applyStarted  func()
	applyFinished func()
	closeEntered  chan struct{}
	cancelContext error
}

func (f *fakeBatchWriterSession) Apply(
	ctx context.Context,
	extent *data.TKeyExtent,
	mutations []*data.TMutation,
) error {
	f.mu.Lock()
	f.applies = append(f.applies, batchWriterApply{
		extent:    cloneThriftExtent(extent),
		mutations: cloneThriftMutations(mutations),
	})
	entered := f.applyEntered
	release := f.applyRelease
	started := f.applyStarted
	finished := f.applyFinished
	err := f.applyErr
	f.mu.Unlock()
	if started != nil {
		started()
	}
	if finished != nil {
		defer finished()
	}
	if entered != nil {
		entered <- struct{}{}
	}
	if release != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
		}
	}
	return err
}

func (f *fakeBatchWriterSession) Close(context.Context) (*data.UpdateErrors, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeCalls++
	if f.closeEntered != nil {
		f.closeEntered <- struct{}{}
	}
	return f.closeResult, f.closeErr
}

func (f *fakeBatchWriterSession) Cancel(ctx context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelCalls++
	f.cancelContext = ctx.Err()
	return f.cancelResult, f.cancelErr
}

func newBatchWriterTestWriter(
	t *testing.T,
	tablets []metadata.TabletInfo,
	options BatchWriterOptions,
	ingest *fakeBatchWriterIngest,
) *BatchWriter {
	t.Helper()
	writer := newBatchWriterTestWriterWithLocator(
		t,
		&fakeTabletWalker{tablets: map[string][]metadata.TabletInfo{"1": tablets}},
		options,
	)
	writer.startSession = ingest.start
	return writer
}

func newBatchWriterTestWriterWithLocator(
	t *testing.T,
	locator interface {
		LocateTable(context.Context, string) ([]metadata.TabletInfo, error)
	},
	options BatchWriterOptions,
) *BatchWriter {
	t.Helper()
	connector := testConnectorWithDiscovery(
		t,
		locator,
		&fakeTableNames{
			byName: map[string]string{"events": "1"},
			byID:   map[string]string{"1": "events"},
		},
	)
	writer, err := connector.NewBatchWriter(
		Table{Name: "events"},
		options,
	)
	if err != nil {
		t.Fatal(err)
	}
	return writer
}

func TestBatchWriterRoutesBatchesAndCopiesMutation(t *testing.T) {
	ingest := &fakeBatchWriterIngest{}
	writer := newBatchWriterTestWriter(t, discoveryTablets(), BatchWriterOptions{
		MaxMemoryBytes:  1 << 20,
		MaxBatchBytes:   1,
		MaxWriteThreads: 1,
		Durability:      DurabilitySync,
	}, ingest)

	row := []byte("a")
	value := []byte("one")
	mutation, err := NewMutation(row)
	if err != nil {
		t.Fatal(err)
	}
	mutation.PutLatest([]byte("cf"), []byte("cq"), nil, value)
	if err := writer.Add(context.Background(), mutation); err != nil {
		t.Fatal(err)
	}
	row[0] = 'z'
	value[0] = 'x'
	mutation.PutLatest([]byte("cf"), []byte("later"), nil, []byte("ignored"))

	for _, row := range []string{"b", "n", "q"} {
		mutation, err := NewMutation([]byte(row))
		if err != nil {
			t.Fatal(err)
		}
		mutation.PutLatest([]byte("cf"), []byte("cq"), nil, []byte(row))
		if err := writer.Add(context.Background(), mutation); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	ingest.mu.Lock()
	defer ingest.mu.Unlock()
	if got, want := ingest.starts, []string{"ts1:9997", "ts2:9997", "ts3:9997"}; !equalStrings(got, want) {
		t.Fatalf("starts = %v, want %v", got, want)
	}
	if len(ingest.sessions[0].applies) != 2 {
		t.Fatalf("ts1 apply calls = %d, want 2 bounded batches", len(ingest.sessions[0].applies))
	}
	first := ingest.sessions[0].applies[0].mutations[0]
	if !bytes.Equal(first.Row, []byte("a")) {
		t.Fatalf("first row = %q, want a", first.Row)
	}
	expected, _ := cclient.NewMutation([]byte("a"))
	expected.PutLatest([]byte("cf"), []byte("cq"), nil, []byte("one"))
	expectedWire, err := expected.ToThrift()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Data, expectedWire.Data) ||
		!equalByteSlices(first.Values, expectedWire.Values) ||
		first.Entries != expectedWire.Entries {
		t.Fatalf("snapshotted mutation = %+v, want original mutation", first)
	}
}

func TestBatchWriterSizeTracksBufferedMutations(t *testing.T) {
	writer := newBatchWriterTestWriter(t, discoveryTablets(), BatchWriterOptions{
		MaxMemoryBytes: 1 << 20,
	}, &fakeBatchWriterIngest{})
	for index, row := range []string{"a", "b"} {
		mutation, _ := NewMutation([]byte(row))
		mutation.PutLatest([]byte("cf"), []byte("cq"), nil, []byte("value"))
		if err := writer.Add(context.Background(), mutation); err != nil {
			t.Fatal(err)
		}
		size, err := writer.Size(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if size != index+1 {
			t.Fatalf("size = %d, want %d", size, index+1)
		}
	}
	if err := writer.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if size, err := writer.Size(context.Background()); err != nil || size != 0 {
		t.Fatalf("size after flush = %d, %v; want 0, nil", size, err)
	}
	if err := writer.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Size(context.Background()); !errors.Is(err, ErrBatchWriterClosed) {
		t.Fatalf("size after close error = %v, want ErrBatchWriterClosed", err)
	}
}

func TestBatchWriterMemoryBoundFlushesSynchronously(t *testing.T) {
	ingest := &fakeBatchWriterIngest{}
	writer := newBatchWriterTestWriter(t, discoveryTablets(), BatchWriterOptions{
		MaxMemoryBytes: 1,
	}, ingest)
	mutation, _ := NewMutation([]byte("a"))
	mutation.PutLatest([]byte("cf"), []byte("cq"), nil, []byte("value"))
	if err := writer.Add(context.Background(), mutation); err != nil {
		t.Fatal(err)
	}
	ingest.mu.Lock()
	defer ingest.mu.Unlock()
	if len(ingest.sessions) != 1 || ingest.sessions[0].closeCalls != 1 {
		t.Fatalf("sessions = %d close calls = %d, want synchronous submission", len(ingest.sessions), ingest.sessions[0].closeCalls)
	}
}

func TestBatchWriterAutomaticFlushesAtDeadline(t *testing.T) {
	session := &fakeBatchWriterSession{closeEntered: make(chan struct{}, 1)}
	writer := newBatchWriterTestWriterWithLocator(
		t,
		&fakeTabletWalker{tablets: map[string][]metadata.TabletInfo{
			"1": discoveryTablets(),
		}},
		BatchWriterOptions{MaxLatency: 10 * time.Millisecond},
	)
	writer.startSession = func(
		_ context.Context,
		_ string,
		_ ingestclient.Durability,
	) (batchWriterSession, error) {
		return session, nil
	}
	addBatchWriterMutation(t, writer, "a")
	waitForBatchWriterSignal(t, session.closeEntered)
	if err := writer.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, want := appliedMutationRows(session), []string{"a"}; !equalStrings(got, want) {
		t.Fatalf("automatic flush rows = %v, want %v", got, want)
	}
	waitForBatchWriterSignal(t, writer.autoFlushDone)
}

func TestBatchWriterAutomaticFlushLeavesIdleWriterEmpty(t *testing.T) {
	started := make(chan struct{}, 1)
	writer := newBatchWriterTestWriterWithLocator(
		t,
		&fakeTabletWalker{tablets: map[string][]metadata.TabletInfo{
			"1": discoveryTablets(),
		}},
		BatchWriterOptions{MaxLatency: 5 * time.Millisecond},
	)
	writer.startSession = func(
		_ context.Context,
		_ string,
		_ ingestclient.Durability,
	) (batchWriterSession, error) {
		started <- struct{}{}
		return &fakeBatchWriterSession{}, nil
	}
	time.Sleep(25 * time.Millisecond)
	select {
	case <-started:
		t.Fatal("idle writer started an ingest session")
	default:
	}
	if err := writer.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForBatchWriterSignal(t, writer.autoFlushDone)
}

func TestBatchWriterExplicitFlushResetsAutomaticDeadline(t *testing.T) {
	const latency = time.Hour
	ingest := &fakeBatchWriterIngest{}
	writer := newBatchWriterTestWriter(t, discoveryTablets(), BatchWriterOptions{
		MaxLatency: latency,
	}, ingest)
	addBatchWriterMutation(t, writer, "a")
	firstStart := batchWriterPendingStart(t, writer)
	if err := writer.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	addBatchWriterMutation(t, writer, "b")
	secondStart := batchWriterPendingStart(t, writer)
	if !secondStart.After(firstStart) {
		t.Fatalf("new deadline start = %v, want after %v", secondStart, firstStart)
	}

	delay, active := writer.automaticFlushStep(
		context.Background(),
		firstStart.Add(latency),
	)
	if !active || delay <= 0 {
		t.Fatalf("old deadline active = %v delay = %v, want future reset deadline", active, delay)
	}
	ingest.mu.Lock()
	if len(ingest.sessions) != 1 {
		ingest.mu.Unlock()
		t.Fatalf("sessions at old deadline = %d, want one explicit flush", len(ingest.sessions))
	}
	ingest.mu.Unlock()

	if _, active := writer.automaticFlushStep(
		context.Background(),
		secondStart.Add(latency),
	); active {
		t.Fatal("automatic deadline remained active after flushing")
	}
	if err := writer.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	ingest.mu.Lock()
	defer ingest.mu.Unlock()
	if len(ingest.sessions) != 2 {
		t.Fatalf("sessions = %d, want explicit and automatic flushes", len(ingest.sessions))
	}
	if got, want := appliedMutationRows(ingest.sessions[1]), []string{"b"}; !equalStrings(got, want) {
		t.Fatalf("reset deadline rows = %v, want %v", got, want)
	}
}

func TestBatchWriterCloseWaitsForAutomaticFlush(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	session := &fakeBatchWriterSession{
		applyEntered: entered,
		applyRelease: release,
	}
	writer := newBatchWriterTestWriterWithLocator(
		t,
		&fakeTabletWalker{tablets: map[string][]metadata.TabletInfo{
			"1": discoveryTablets(),
		}},
		BatchWriterOptions{MaxLatency: 5 * time.Millisecond},
	)
	writer.startSession = func(
		_ context.Context,
		_ string,
		_ ingestclient.Durability,
	) (batchWriterSession, error) {
		return session, nil
	}
	addBatchWriterMutation(t, writer, "a")
	waitForBatchWriterSignal(t, entered)
	closed := make(chan error, 1)
	go func() {
		closed <- writer.Close(context.Background())
	}()
	select {
	case err := <-closed:
		t.Fatalf("Close returned during automatic flush: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	waitForBatchWriterSignal(t, writer.autoFlushDone)
	if got, want := appliedMutationRows(session), []string{"a"}; !equalStrings(got, want) {
		t.Fatalf("automatic flush rows = %v, want %v", got, want)
	}
}

func TestBatchWriterSafeAutomaticFlushErrorIsSticky(t *testing.T) {
	startErr := errors.New("start unavailable")
	starts := 0
	writer := newBatchWriterTestWriterWithLocator(
		t,
		&fakeTabletWalker{tablets: map[string][]metadata.TabletInfo{
			"1": discoveryTablets(),
		}},
		BatchWriterOptions{
			MaxLatency:   time.Hour,
			MaxRetries:   1,
			RetryBackoff: time.Nanosecond,
		},
	)
	writer.startSession = func(
		_ context.Context,
		_ string,
		_ ingestclient.Durability,
	) (batchWriterSession, error) {
		starts++
		return nil, startErr
	}
	addBatchWriterMutation(t, writer, "a")
	writer.automaticFlushStep(
		context.Background(),
		batchWriterPendingStart(t, writer).Add(time.Hour),
	)

	err := writer.Flush(context.Background())
	if !errors.Is(err, ErrBatchWriterAutoFlush) ||
		!errors.Is(err, ErrBatchWriterRetryExhausted) ||
		errors.Is(err, ErrBatchWriterFailed) ||
		!errors.Is(err, startErr) {
		t.Fatalf("Flush = %v, want safe sticky automatic failure", err)
	}
	mutation, _ := NewMutation([]byte("b"))
	mutation.PutLatest([]byte("cf"), []byte("cq"), nil, []byte("b"))
	if addErr := writer.Add(context.Background(), mutation); addErr != err {
		t.Fatalf("Add error = %v, want stored error %v", addErr, err)
	}
	if closeErr := writer.Close(context.Background()); closeErr != err {
		t.Fatalf("Close error = %v, want stored error %v", closeErr, err)
	}
	if starts != 2 {
		t.Fatalf("start calls = %d, want initial attempt and one retry", starts)
	}
	waitForBatchWriterSignal(t, writer.autoFlushDone)
}

func TestBatchWriterAmbiguousAutomaticFlushErrorIsSticky(t *testing.T) {
	applyErr := errors.New("apply interrupted")
	session := &fakeBatchWriterSession{
		applyErr:     applyErr,
		cancelResult: true,
	}
	writer := newBatchWriterTestWriterWithLocator(
		t,
		&fakeTabletWalker{tablets: map[string][]metadata.TabletInfo{
			"1": discoveryTablets(),
		}},
		BatchWriterOptions{MaxLatency: time.Hour},
	)
	writer.startSession = func(
		_ context.Context,
		_ string,
		_ ingestclient.Durability,
	) (batchWriterSession, error) {
		return session, nil
	}
	addBatchWriterMutation(t, writer, "a")
	writer.automaticFlushStep(
		context.Background(),
		batchWriterPendingStart(t, writer).Add(time.Hour),
	)

	err := writer.Flush(context.Background())
	if !errors.Is(err, ErrBatchWriterAutoFlush) ||
		!errors.Is(err, ErrBatchWriterFailed) ||
		!errors.Is(err, applyErr) {
		t.Fatalf("Flush = %v, want ambiguous sticky automatic failure", err)
	}
	if closeErr := writer.Close(context.Background()); closeErr != err {
		t.Fatalf("Close error = %v, want stored error %v", closeErr, err)
	}
	waitForBatchWriterSignal(t, writer.autoFlushDone)
}

func TestBatchWriterAutomaticFlushLifecycleDoesNotLeak(t *testing.T) {
	const writerCount = 32
	for index := 0; index < writerCount; index++ {
		ingest := &fakeBatchWriterIngest{}
		writer := newBatchWriterTestWriter(t, discoveryTablets(), BatchWriterOptions{
			MaxLatency: time.Hour,
		}, ingest)
		addBatchWriterMutation(t, writer, fmt.Sprintf("row-%d", index))
		if err := writer.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		waitForBatchWriterSignal(t, writer.autoFlushDone)
	}
}

func TestBatchWriterRetryOptionsValidation(t *testing.T) {
	if _, err := normalizeBatchWriterOptions(BatchWriterOptions{
		MaxLatency: -time.Nanosecond,
	}); err == nil {
		t.Fatal("expected negative MaxLatency error")
	}
	if _, err := normalizeBatchWriterOptions(BatchWriterOptions{
		MaxWriteThreads: -1,
	}); err == nil {
		t.Fatal("expected negative MaxWriteThreads error")
	}
	if _, err := normalizeBatchWriterOptions(BatchWriterOptions{
		MaxRetries: -1,
	}); err == nil {
		t.Fatal("expected negative MaxRetries error")
	}
	if _, err := normalizeBatchWriterOptions(BatchWriterOptions{
		RetryBackoff: -time.Nanosecond,
	}); err == nil {
		t.Fatal("expected negative RetryBackoff error")
	}
	options, err := normalizeBatchWriterOptions(BatchWriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if options.maxWriteThreads != 3 {
		t.Fatalf("default max write threads = %d, want 3", options.maxWriteThreads)
	}
	if options.maxLatency != 0 {
		t.Fatalf("default max latency = %v, want disabled", options.maxLatency)
	}
}

func TestBatchWriterAddRollsBackBeforeSubmission(t *testing.T) {
	ingest := &fakeBatchWriterIngest{}
	walker := &blockingBatchWriterWalker{entered: make(chan struct{})}
	connector := testConnectorWithDiscovery(
		t,
		walker,
		&fakeTableNames{
			byName: map[string]string{"events": "1"},
			byID:   map[string]string{"1": "events"},
		},
	)
	writer, err := connector.NewBatchWriter(
		Table{Name: "events"},
		BatchWriterOptions{MaxMemoryBytes: 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	writer.startSession = ingest.start
	mutation, _ := NewMutation([]byte("a"))
	mutation.PutLatest([]byte("cf"), []byte("cq"), nil, []byte("value"))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- writer.Add(ctx, mutation)
	}()
	<-walker.entered
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Add = %v, want context.Canceled", err)
	}
	if len(writer.pending) != 0 || writer.pendingBytes != 0 {
		t.Fatalf("failed Add retained %d mutations and %d bytes", len(writer.pending), writer.pendingBytes)
	}
}

func TestBatchWriterConcurrentAdds(t *testing.T) {
	ingest := &fakeBatchWriterIngest{}
	writer := newBatchWriterTestWriter(t, discoveryTablets(), BatchWriterOptions{}, ingest)
	const mutationCount = 32
	errs := make(chan error, mutationCount)
	var callers sync.WaitGroup
	callers.Add(mutationCount)
	for index := 0; index < mutationCount; index++ {
		go func(index int) {
			defer callers.Done()
			mutation, err := NewMutation([]byte(fmt.Sprintf("row-%02d", index)))
			if err == nil {
				mutation.PutLatest([]byte("cf"), []byte("cq"), nil, []byte{byte(index)})
				err = writer.Add(context.Background(), mutation)
			}
			errs <- err
		}(index)
	}
	callers.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	ingest.mu.Lock()
	defer ingest.mu.Unlock()
	submitted := 0
	rows := make(map[string]struct{}, mutationCount)
	for _, session := range ingest.sessions {
		for _, apply := range session.applies {
			submitted += len(apply.mutations)
			for _, mutation := range apply.mutations {
				rows[string(mutation.Row)] = struct{}{}
			}
		}
	}
	if submitted != mutationCount {
		t.Fatalf("submitted = %d, want %d", submitted, mutationCount)
	}
	if len(rows) != mutationCount {
		t.Fatalf("unique rows = %d, want %d", len(rows), mutationCount)
	}
}

func TestBatchWriterBoundsParallelServerSubmission(t *testing.T) {
	writer := newBatchWriterTestWriterWithLocator(
		t,
		&fakeTabletWalker{tablets: map[string][]metadata.TabletInfo{
			"1": batchWriterServerTablets("ts1:9997", "ts2:9997", "ts3:9997", "ts4:9997"),
		}},
		BatchWriterOptions{MaxWriteThreads: 2},
	)
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	var trackerMu sync.Mutex
	active := 0
	maxActive := 0
	starts := 0
	writer.startSession = func(
		_ context.Context,
		_ string,
		_ ingestclient.Durability,
	) (batchWriterSession, error) {
		trackerMu.Lock()
		starts++
		trackerMu.Unlock()
		return &fakeBatchWriterSession{
			applyEntered: entered,
			applyRelease: release,
			applyStarted: func() {
				trackerMu.Lock()
				defer trackerMu.Unlock()
				active++
				if active > maxActive {
					maxActive = active
				}
			},
			applyFinished: func() {
				trackerMu.Lock()
				defer trackerMu.Unlock()
				active--
			},
		}, nil
	}
	for _, row := range []string{"a", "b", "c", "d"} {
		addBatchWriterMutation(t, writer, row)
	}
	done := make(chan error, 1)
	go func() {
		done <- writer.Flush(context.Background())
	}()
	for range 2 {
		waitForBatchWriterSignal(t, entered)
	}
	trackerMu.Lock()
	if starts != 2 || maxActive != 2 {
		t.Fatalf("starts = %d max active = %d, want bounded parallelism 2", starts, maxActive)
	}
	trackerMu.Unlock()
	releaseOnce.Do(func() { close(release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	trackerMu.Lock()
	defer trackerMu.Unlock()
	if starts != 4 || maxActive != 2 || active != 0 {
		t.Fatalf(
			"starts = %d max active = %d active = %d, want 4/2/0",
			starts,
			maxActive,
			active,
		)
	}
}

func TestBatchWriterPreservesSameServerBatchOrder(t *testing.T) {
	ingest := &fakeBatchWriterIngest{}
	writer := newBatchWriterTestWriter(t, []metadata.TabletInfo{
		{
			TableID:  "1",
			Location: &metadata.Location{HostPort: "ts1:9997"},
		},
	}, BatchWriterOptions{
		MaxBatchBytes:   1,
		MaxWriteThreads: 3,
	}, ingest)
	for _, row := range []string{"c", "a", "b"} {
		addBatchWriterMutation(t, writer, row)
	}
	if err := writer.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	ingest.mu.Lock()
	defer ingest.mu.Unlock()
	if len(ingest.sessions) != 1 {
		t.Fatalf("sessions = %d, want one session for one server", len(ingest.sessions))
	}
	if got, want := appliedMutationRows(ingest.sessions[0]), []string{"c", "a", "b"}; !equalStrings(got, want) {
		t.Fatalf("applied rows = %v, want %v", got, want)
	}
}

func TestBatchWriterParallelCancellationWaitsForWorkers(t *testing.T) {
	writer := newBatchWriterTestWriterWithLocator(
		t,
		&fakeTabletWalker{tablets: map[string][]metadata.TabletInfo{
			"1": batchWriterServerTablets("ts1:9997", "ts2:9997"),
		}},
		BatchWriterOptions{MaxWriteThreads: 2},
	)
	entered := make(chan struct{}, 2)
	blocked := make(chan struct{})
	sessions := map[string]*fakeBatchWriterSession{
		"ts1:9997": {
			applyEntered: entered,
			applyRelease: blocked,
			cancelResult: true,
		},
		"ts2:9997": {
			applyEntered: entered,
			applyRelease: blocked,
			cancelResult: true,
		},
	}
	writer.startSession = func(
		_ context.Context,
		address string,
		_ ingestclient.Durability,
	) (batchWriterSession, error) {
		return sessions[address], nil
	}
	addBatchWriterMutation(t, writer, "a")
	addBatchWriterMutation(t, writer, "b")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- writer.Flush(ctx)
	}()
	waitForBatchWriterSignal(t, entered)
	waitForBatchWriterSignal(t, entered)
	cancel()
	err := <-done
	if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrBatchWriterFailed) {
		t.Fatalf("Flush = %v, want sticky cancellation", err)
	}
	for address, session := range sessions {
		session.mu.Lock()
		if session.cancelCalls != 1 || session.cancelContext != nil {
			t.Fatalf(
				"%s cancel calls = %d cleanup context = %v",
				address,
				session.cancelCalls,
				session.cancelContext,
			)
		}
		session.mu.Unlock()
	}
}

func TestBatchWriterAggregatesParallelErrorsInPlanOrder(t *testing.T) {
	writer := newBatchWriterTestWriterWithLocator(
		t,
		&fakeTabletWalker{tablets: map[string][]metadata.TabletInfo{
			"1": batchWriterServerTablets("ts1:9997", "ts2:9997"),
		}},
		BatchWriterOptions{MaxWriteThreads: 2},
	)
	entered := make(chan struct{}, 2)
	firstRelease := make(chan struct{})
	first := &fakeBatchWriterSession{
		applyEntered: entered,
		applyRelease: firstRelease,
		applyErr:     errors.New("first-server-error"),
		cancelResult: true,
	}
	second := &fakeBatchWriterSession{
		applyEntered: entered,
		applyErr:     errors.New("second-server-error"),
		cancelResult: true,
	}
	writer.startSession = func(
		_ context.Context,
		address string,
		_ ingestclient.Durability,
	) (batchWriterSession, error) {
		if address == "ts1:9997" {
			return first, nil
		}
		return second, nil
	}
	addBatchWriterMutation(t, writer, "a")
	addBatchWriterMutation(t, writer, "b")
	done := make(chan error, 1)
	go func() {
		done <- writer.Flush(context.Background())
	}()
	waitForBatchWriterSignal(t, entered)
	waitForBatchWriterSignal(t, entered)
	close(firstRelease)
	err := <-done
	if !errors.Is(err, ErrBatchWriterFailed) {
		t.Fatalf("Flush = %v, want ErrBatchWriterFailed", err)
	}
	message := err.Error()
	firstIndex := strings.Index(message, "first-server-error")
	secondIndex := strings.Index(message, "second-server-error")
	if firstIndex < 0 || secondIndex < 0 || firstIndex > secondIndex {
		t.Fatalf("error order = %q, want first server before second server", message)
	}
}

func TestBatchWriterCloseLifecycle(t *testing.T) {
	ingest := &fakeBatchWriterIngest{}
	writer := newBatchWriterTestWriter(t, discoveryTablets(), BatchWriterOptions{}, ingest)
	mutation, _ := NewMutation([]byte("a"))
	mutation.PutLatest([]byte("cf"), []byte("cq"), nil, []byte("value"))
	if err := writer.Add(context.Background(), mutation); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := writer.Add(context.Background(), mutation); !errors.Is(err, ErrBatchWriterClosed) {
		t.Fatalf("Add after Close = %v, want ErrBatchWriterClosed", err)
	}
	if err := writer.Flush(context.Background()); !errors.Is(err, ErrBatchWriterClosed) {
		t.Fatalf("Flush after Close = %v, want ErrBatchWriterClosed", err)
	}
	ingest.mu.Lock()
	defer ingest.mu.Unlock()
	if len(ingest.sessions) != 1 || ingest.sessions[0].closeCalls != 1 {
		t.Fatalf("sessions = %d close calls = %d, want one submission", len(ingest.sessions), ingest.sessions[0].closeCalls)
	}
}

func TestBatchWriterWaitingCallerHonorsContext(t *testing.T) {
	ingest := &fakeBatchWriterIngest{}
	writer := newBatchWriterTestWriter(t, discoveryTablets(), BatchWriterOptions{}, ingest)
	writer.startSession = func(
		_ context.Context,
		_ string,
		_ ingestclient.Durability,
	) (batchWriterSession, error) {
		return &fakeBatchWriterSession{}, nil
	}
	if err := writer.lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer writer.unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := writer.Flush(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Flush = %v, want DeadlineExceeded", err)
	}
}

func TestBatchWriterCancellationCleansUpAndFailsSticky(t *testing.T) {
	ingest := &fakeBatchWriterIngest{}
	writer := newBatchWriterTestWriter(t, discoveryTablets(), BatchWriterOptions{}, ingest)
	session := &fakeBatchWriterSession{
		applyEntered: make(chan struct{}, 1),
		applyRelease: make(chan struct{}),
		cancelResult: true,
	}
	writer.startSession = func(
		_ context.Context,
		_ string,
		_ ingestclient.Durability,
	) (batchWriterSession, error) {
		return session, nil
	}
	mutation, _ := NewMutation([]byte("a"))
	mutation.PutLatest([]byte("cf"), []byte("cq"), nil, []byte("value"))
	if err := writer.Add(context.Background(), mutation); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- writer.Flush(ctx)
	}()
	<-session.applyEntered
	cancel()
	err := <-done
	if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrBatchWriterFailed) {
		t.Fatalf("Flush = %v, want canceled sticky failure", err)
	}
	session.mu.Lock()
	if session.cancelCalls != 1 || session.cancelContext != nil {
		t.Fatalf("cancel calls = %d cleanup context = %v", session.cancelCalls, session.cancelContext)
	}
	session.mu.Unlock()
	if err := writer.Add(context.Background(), mutation); !errors.Is(err, ErrBatchWriterFailed) {
		t.Fatalf("Add after failure = %v, want ErrBatchWriterFailed", err)
	}
}

func TestBatchWriterSurfacesAccumuloUpdateErrors(t *testing.T) {
	ingest := &fakeBatchWriterIngest{}
	writer := newBatchWriterTestWriter(t, discoveryTablets(), BatchWriterOptions{}, ingest)
	session := &fakeBatchWriterSession{
		closeResult: &data.UpdateErrors{
			FailedExtents: map[*data.TKeyExtent]int64{
				&data.TKeyExtent{Table: []byte("1"), EndRow: []byte("k")}: 1,
			},
			ViolationSummaries: []*data.TConstraintViolationSummary{
				{
					ConstrainClass:             "example.Constraint",
					ViolationCode:              7,
					ViolationDescription:       "rejected",
					NumberOfViolatingMutations: 2,
				},
			},
			AuthorizationFailures: map[*data.TKeyExtent]clientpkg.SecurityErrorCode{
				&data.TKeyExtent{
					Table:  []byte("1"),
					EndRow: []byte("k"),
				}: clientpkg.SecurityErrorCode_PERMISSION_DENIED,
			},
		},
	}
	writer.startSession = func(
		_ context.Context,
		_ string,
		_ ingestclient.Durability,
	) (batchWriterSession, error) {
		return session, nil
	}
	for _, row := range []string{"a", "b"} {
		mutation, _ := NewMutation([]byte(row))
		mutation.PutLatest([]byte("cf"), []byte("cq"), nil, []byte(row))
		if err := writer.Add(context.Background(), mutation); err != nil {
			t.Fatal(err)
		}
	}
	err := writer.Flush(context.Background())
	var rejection *MutationRejectionError
	if !errors.Is(err, ErrBatchWriterFailed) || !errors.As(err, &rejection) {
		t.Fatalf("Flush = %v, want MutationRejectionError", err)
	}
	if got := rejection.FailedExtents[0]; got.Submitted != 2 || got.Committed != 1 {
		t.Fatalf("failed extent = %+v", got)
	}
	if rejection.AuthorizationFailures[0].Code != "PERMISSION_DENIED" {
		t.Fatalf("authorization code = %q", rejection.AuthorizationFailures[0].Code)
	}
}

func TestMalformedUpdateErrorsIncludeTabletServer(t *testing.T) {
	const server = "ts-malformed:9997"
	extent := func(endRow string) *data.TKeyExtent {
		return &data.TKeyExtent{
			Table:  []byte("1"),
			EndRow: []byte(endRow),
		}
	}
	plan := serverPlan{
		address: server,
		extents: []extentPlan{
			{
				extent:    extent("k"),
				mutations: []bufferedMutation{{row: []byte("a")}},
			},
		},
	}
	cases := []struct {
		name   string
		errors *data.UpdateErrors
	}{
		{
			name: "nil failed extent",
			errors: &data.UpdateErrors{
				FailedExtents: map[*data.TKeyExtent]int64{nil: 0},
			},
		},
		{
			name: "unknown failed extent",
			errors: &data.UpdateErrors{
				FailedExtents: map[*data.TKeyExtent]int64{extent("z"): 0},
			},
		},
		{
			name: "invalid committed count",
			errors: &data.UpdateErrors{
				FailedExtents: map[*data.TKeyExtent]int64{extent("k"): 2},
			},
		},
		{
			name: "duplicate failed extent",
			errors: &data.UpdateErrors{
				FailedExtents: map[*data.TKeyExtent]int64{
					extent("k"): 0,
					extent("k"): 0,
				},
			},
		},
		{
			name: "nil authorization extent",
			errors: &data.UpdateErrors{
				AuthorizationFailures: map[*data.TKeyExtent]clientpkg.SecurityErrorCode{
					nil: clientpkg.SecurityErrorCode_PERMISSION_DENIED,
				},
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := decodeMutationUpdateErrors(plan, testCase.errors)
			if err == nil {
				t.Fatal("expected malformed update error")
			}
			if !strings.Contains(err.Error(), server) {
				t.Fatalf("error = %q, want tablet server %q", err, server)
			}
		})
	}
}

func TestBatchWriterRelocatesExplicitlyUncommittedSuffix(t *testing.T) {
	walker := &sequencedBatchWriterWalker{
		snapshots: [][]metadata.TabletInfo{
			{
				{
					TableID: "1",
					EndRow:  []byte("k"),
					Location: &metadata.Location{
						HostPort: "ts1:9997",
						Session:  "old",
					},
				},
			},
			{
				{
					TableID: "1",
					EndRow:  []byte("k"),
					Location: &metadata.Location{
						HostPort: "ts2:9997",
						Session:  "new",
					},
				},
			},
		},
	}
	writer := newBatchWriterTestWriterWithLocator(t, walker, BatchWriterOptions{
		MaxRetries:   1,
		RetryBackoff: time.Nanosecond,
	})
	first := &fakeBatchWriterSession{
		closeResult: &data.UpdateErrors{
			FailedExtents: map[*data.TKeyExtent]int64{
				&data.TKeyExtent{Table: []byte("1"), EndRow: []byte("k")}: 1,
			},
		},
	}
	second := &fakeBatchWriterSession{}
	var starts []string
	writer.startSession = func(
		_ context.Context,
		address string,
		_ ingestclient.Durability,
	) (batchWriterSession, error) {
		starts = append(starts, address)
		switch address {
		case "ts1:9997":
			return first, nil
		case "ts2:9997":
			return second, nil
		default:
			return nil, fmt.Errorf("unexpected tablet server %q", address)
		}
	}
	for _, row := range []string{"a", "b"} {
		mutation, _ := NewMutation([]byte(row))
		mutation.PutLatest([]byte("cf"), []byte("cq"), nil, []byte(row))
		if err := writer.Add(context.Background(), mutation); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"ts1:9997", "ts2:9997"}; !equalStrings(starts, want) {
		t.Fatalf("starts = %v, want %v", starts, want)
	}
	if got, want := appliedMutationRows(first), []string{"a", "b"}; !equalStrings(got, want) {
		t.Fatalf("initial rows = %v, want %v", got, want)
	}
	if got, want := appliedMutationRows(second), []string{"b"}; !equalStrings(got, want) {
		t.Fatalf("relocated rows = %v, want only uncommitted suffix %v", got, want)
	}
}

func TestBatchWriterParallelRetryDoesNotReplaySuccessfulServer(t *testing.T) {
	oldTablets := []metadata.TabletInfo{
		{
			TableID: "1",
			EndRow:  []byte("m"),
			Location: &metadata.Location{
				HostPort: "ts1:9997",
				Session:  "old",
			},
		},
		{
			TableID: "1",
			PrevRow: []byte("m"),
			Location: &metadata.Location{
				HostPort: "ts2:9997",
				Session:  "stable",
			},
		},
	}
	newTablets := []metadata.TabletInfo{
		{
			TableID: "1",
			EndRow:  []byte("m"),
			Location: &metadata.Location{
				HostPort: "ts3:9997",
				Session:  "new",
			},
		},
		oldTablets[1],
	}
	writer := newBatchWriterTestWriterWithLocator(
		t,
		&sequencedBatchWriterWalker{snapshots: [][]metadata.TabletInfo{
			oldTablets,
			newTablets,
		}},
		BatchWriterOptions{
			MaxWriteThreads: 2,
			MaxRetries:      1,
			RetryBackoff:    time.Nanosecond,
		},
	)
	oldSession := &fakeBatchWriterSession{
		closeResult: &data.UpdateErrors{
			FailedExtents: map[*data.TKeyExtent]int64{
				&data.TKeyExtent{Table: []byte("1"), EndRow: []byte("m")}: 1,
			},
		},
	}
	stableSession := &fakeBatchWriterSession{}
	newSession := &fakeBatchWriterSession{}
	var startsMu sync.Mutex
	starts := make(map[string]int)
	writer.startSession = func(
		_ context.Context,
		address string,
		_ ingestclient.Durability,
	) (batchWriterSession, error) {
		startsMu.Lock()
		starts[address]++
		startsMu.Unlock()
		switch address {
		case "ts1:9997":
			return oldSession, nil
		case "ts2:9997":
			return stableSession, nil
		case "ts3:9997":
			return newSession, nil
		default:
			return nil, fmt.Errorf("unexpected server %q", address)
		}
	}
	for _, row := range []string{"a", "b", "z"} {
		addBatchWriterMutation(t, writer, row)
	}
	if err := writer.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, want := appliedMutationRows(oldSession), []string{"a", "b"}; !equalStrings(got, want) {
		t.Fatalf("old server rows = %v, want %v", got, want)
	}
	if got, want := appliedMutationRows(stableSession), []string{"z"}; !equalStrings(got, want) {
		t.Fatalf("stable server rows = %v, want one submission %v", got, want)
	}
	if got, want := appliedMutationRows(newSession), []string{"b"}; !equalStrings(got, want) {
		t.Fatalf("new server rows = %v, want only suffix %v", got, want)
	}
	startsMu.Lock()
	defer startsMu.Unlock()
	if starts["ts1:9997"] != 1 || starts["ts2:9997"] != 1 || starts["ts3:9997"] != 1 {
		t.Fatalf("server starts = %v, want each server exactly once", starts)
	}
}

func TestBatchWriterAcceptsFullyCommittedFailedExtent(t *testing.T) {
	ingest := &fakeBatchWriterIngest{}
	writer := newBatchWriterTestWriter(t, discoveryTablets(), BatchWriterOptions{
		MaxRetries:   1,
		RetryBackoff: time.Nanosecond,
	}, ingest)
	session := &fakeBatchWriterSession{
		closeResult: &data.UpdateErrors{
			FailedExtents: map[*data.TKeyExtent]int64{
				&data.TKeyExtent{Table: []byte("1"), EndRow: []byte("k")}: 2,
			},
		},
	}
	starts := 0
	writer.startSession = func(
		_ context.Context,
		_ string,
		_ ingestclient.Durability,
	) (batchWriterSession, error) {
		starts++
		return session, nil
	}
	for _, row := range []string{"a", "b"} {
		mutation, _ := NewMutation([]byte(row))
		mutation.PutLatest([]byte("cf"), []byte("cq"), nil, []byte(row))
		if err := writer.Add(context.Background(), mutation); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if starts != 1 {
		t.Fatalf("start calls = %d, want no empty retry", starts)
	}
}

func TestBatchWriterRetriesMovedTabletBeforeAcceptance(t *testing.T) {
	walker := &sequencedBatchWriterWalker{
		snapshots: [][]metadata.TabletInfo{
			{
				{
					TableID:  "1",
					Location: &metadata.Location{HostPort: "ts1:9997"},
				},
			},
			{
				{
					TableID:  "1",
					Location: &metadata.Location{HostPort: "ts2:9997"},
				},
			},
		},
	}
	writer := newBatchWriterTestWriterWithLocator(t, walker, BatchWriterOptions{
		MaxRetries:   1,
		RetryBackoff: time.Nanosecond,
	})
	var starts []string
	success := &fakeBatchWriterSession{}
	writer.startSession = func(
		_ context.Context,
		address string,
		_ ingestclient.Durability,
	) (batchWriterSession, error) {
		starts = append(starts, address)
		if address == "ts1:9997" {
			return nil, errors.New("tablet server moved")
		}
		return success, nil
	}
	mutation, _ := NewMutation([]byte("row"))
	mutation.PutLatest([]byte("cf"), []byte("cq"), nil, []byte("value"))
	if err := writer.Add(context.Background(), mutation); err != nil {
		t.Fatal(err)
	}
	if err := writer.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"ts1:9997", "ts2:9997"}; !equalStrings(starts, want) {
		t.Fatalf("starts = %v, want %v", starts, want)
	}
	if got := appliedMutationRows(success); !equalStrings(got, []string{"row"}) {
		t.Fatalf("successful rows = %v, want row", got)
	}
}

func TestBatchWriterPreAcceptanceRetryExhaustionIsNotSticky(t *testing.T) {
	ingest := &fakeBatchWriterIngest{}
	writer := newBatchWriterTestWriter(t, discoveryTablets(), BatchWriterOptions{
		MaxRetries:   1,
		RetryBackoff: time.Nanosecond,
	}, ingest)
	attempts := 0
	success := &fakeBatchWriterSession{}
	writer.startSession = func(
		_ context.Context,
		_ string,
		_ ingestclient.Durability,
	) (batchWriterSession, error) {
		attempts++
		if attempts <= 2 {
			return nil, errors.New("dial unavailable")
		}
		return success, nil
	}
	mutation, _ := NewMutation([]byte("a"))
	mutation.PutLatest([]byte("cf"), []byte("cq"), nil, []byte("value"))
	if err := writer.Add(context.Background(), mutation); err != nil {
		t.Fatal(err)
	}
	err := writer.Flush(context.Background())
	if !errors.Is(err, ErrBatchWriterRetryExhausted) {
		t.Fatalf("Flush = %v, want ErrBatchWriterRetryExhausted", err)
	}
	if errors.Is(err, ErrBatchWriterFailed) {
		t.Fatalf("pre-acceptance failure became sticky: %v", err)
	}
	if len(writer.pending) != 1 {
		t.Fatalf("pending = %d, want retained mutation", len(writer.pending))
	}
	if err := writer.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := appliedMutationRows(success); !equalStrings(got, []string{"a"}) {
		t.Fatalf("successful rows = %v, want a", got)
	}
}

func TestBatchWriterExplicitRetryExhaustionIsSticky(t *testing.T) {
	ingest := &fakeBatchWriterIngest{}
	writer := newBatchWriterTestWriter(t, discoveryTablets(), BatchWriterOptions{
		MaxRetries:   1,
		RetryBackoff: time.Nanosecond,
	}, ingest)
	starts := 0
	writer.startSession = func(
		_ context.Context,
		_ string,
		_ ingestclient.Durability,
	) (batchWriterSession, error) {
		starts++
		return &fakeBatchWriterSession{
			closeResult: &data.UpdateErrors{
				FailedExtents: map[*data.TKeyExtent]int64{
					&data.TKeyExtent{Table: []byte("1"), EndRow: []byte("k")}: 0,
				},
			},
		}, nil
	}
	mutation, _ := NewMutation([]byte("a"))
	mutation.PutLatest([]byte("cf"), []byte("cq"), nil, []byte("value"))
	if err := writer.Add(context.Background(), mutation); err != nil {
		t.Fatal(err)
	}
	err := writer.Flush(context.Background())
	var rejection *MutationRejectionError
	if !errors.Is(err, ErrBatchWriterRetryExhausted) ||
		!errors.Is(err, ErrBatchWriterFailed) ||
		!errors.As(err, &rejection) {
		t.Fatalf("Flush = %v, want sticky retry exhaustion with rejection details", err)
	}
	if starts != 2 {
		t.Fatalf("start calls = %d, want initial attempt plus one retry", starts)
	}
	if got := rejection.FailedExtents[0]; got.Submitted != 1 || got.Committed != 0 {
		t.Fatalf("failed extent = %+v", got)
	}
}

func TestBatchWriterDoesNotRetryAmbiguousApplyFailure(t *testing.T) {
	ingest := &fakeBatchWriterIngest{}
	writer := newBatchWriterTestWriter(t, discoveryTablets(), BatchWriterOptions{
		MaxRetries:   3,
		RetryBackoff: time.Nanosecond,
	}, ingest)
	starts := 0
	session := &fakeBatchWriterSession{
		applyErr:     errors.New("apply response lost"),
		cancelResult: true,
	}
	writer.startSession = func(
		_ context.Context,
		_ string,
		_ ingestclient.Durability,
	) (batchWriterSession, error) {
		starts++
		return session, nil
	}
	mutation, _ := NewMutation([]byte("a"))
	mutation.PutLatest([]byte("cf"), []byte("cq"), nil, []byte("value"))
	if err := writer.Add(context.Background(), mutation); err != nil {
		t.Fatal(err)
	}
	err := writer.Flush(context.Background())
	if !errors.Is(err, ErrBatchWriterFailed) {
		t.Fatalf("Flush = %v, want ErrBatchWriterFailed", err)
	}
	if starts != 1 {
		t.Fatalf("start calls = %d, want no ambiguous retry", starts)
	}
}

func TestBatchWriterRetryBackoffHonorsCancellation(t *testing.T) {
	ingest := &fakeBatchWriterIngest{}
	writer := newBatchWriterTestWriter(t, discoveryTablets(), BatchWriterOptions{
		MaxRetries:   3,
		RetryBackoff: time.Hour,
	}, ingest)
	closeEntered := make(chan struct{}, 1)
	session := &fakeBatchWriterSession{
		closeEntered: closeEntered,
		closeResult: &data.UpdateErrors{
			FailedExtents: map[*data.TKeyExtent]int64{
				&data.TKeyExtent{Table: []byte("1"), EndRow: []byte("k")}: 0,
			},
		},
	}
	starts := 0
	writer.startSession = func(
		_ context.Context,
		_ string,
		_ ingestclient.Durability,
	) (batchWriterSession, error) {
		starts++
		return session, nil
	}
	mutation, _ := NewMutation([]byte("a"))
	mutation.PutLatest([]byte("cf"), []byte("cq"), nil, []byte("value"))
	if err := writer.Add(context.Background(), mutation); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- writer.Flush(ctx)
	}()
	<-closeEntered
	cancel()
	err := <-done
	if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrBatchWriterFailed) {
		t.Fatalf("Flush = %v, want sticky context cancellation", err)
	}
	if starts != 1 {
		t.Fatalf("start calls = %d, want cancellation before retry", starts)
	}
}

func TestBatchWriterCloseCleanupErrorIsStable(t *testing.T) {
	ingest := &fakeBatchWriterIngest{}
	writer := newBatchWriterTestWriter(t, discoveryTablets(), BatchWriterOptions{}, ingest)
	closeFailure := errors.New("close transport failed")
	cleanupFailure := errors.New("cancel transport failed")
	session := &fakeBatchWriterSession{
		closeErr:  closeFailure,
		cancelErr: cleanupFailure,
	}
	writer.startSession = func(
		_ context.Context,
		_ string,
		_ ingestclient.Durability,
	) (batchWriterSession, error) {
		return session, nil
	}
	mutation, _ := NewMutation([]byte("a"))
	mutation.PutLatest([]byte("cf"), []byte("cq"), nil, []byte("value"))
	if err := writer.Add(context.Background(), mutation); err != nil {
		t.Fatal(err)
	}
	first := writer.Close(context.Background())
	second := writer.Close(context.Background())
	var cleanup *BatchWriterCleanupError
	if !errors.Is(first, ErrBatchWriterFailed) ||
		!errors.Is(first, closeFailure) ||
		!errors.Is(first, cleanupFailure) ||
		!errors.As(first, &cleanup) {
		t.Fatalf("Close = %v, want sticky cleanup error", first)
	}
	if first != second {
		t.Fatalf("repeated Close returned a different error")
	}
}

func cloneThriftExtent(extent *data.TKeyExtent) *data.TKeyExtent {
	if extent == nil {
		return nil
	}
	return &data.TKeyExtent{
		Table:      cloneRow(extent.Table),
		EndRow:     cloneRow(extent.EndRow),
		PrevEndRow: cloneRow(extent.PrevEndRow),
	}
}

func cloneThriftMutations(mutations []*data.TMutation) []*data.TMutation {
	cloned := make([]*data.TMutation, len(mutations))
	for index, mutation := range mutations {
		cloned[index] = &data.TMutation{
			Row:     cloneRow(mutation.Row),
			Data:    cloneRow(mutation.Data),
			Values:  cloneByteSlices(mutation.Values),
			Entries: mutation.Entries,
			Sources: append([]string(nil), mutation.Sources...),
		}
	}
	return cloned
}

func appliedMutationRows(session *fakeBatchWriterSession) []string {
	session.mu.Lock()
	defer session.mu.Unlock()
	var rows []string
	for _, apply := range session.applies {
		for _, mutation := range apply.mutations {
			rows = append(rows, string(mutation.Row))
		}
	}
	return rows
}

func batchWriterServerTablets(addresses ...string) []metadata.TabletInfo {
	tablets := make([]metadata.TabletInfo, len(addresses))
	for index, address := range addresses {
		tablets[index] = metadata.TabletInfo{
			TableID:  "1",
			Location: &metadata.Location{HostPort: address},
		}
		if index > 0 {
			tablets[index].PrevRow = []byte{byte('a' + index - 1)}
		}
		if index < len(addresses)-1 {
			tablets[index].EndRow = []byte{byte('a' + index)}
		}
	}
	return tablets
}

func addBatchWriterMutation(t *testing.T, writer *BatchWriter, row string) {
	t.Helper()
	mutation, err := NewMutation([]byte(row))
	if err != nil {
		t.Fatal(err)
	}
	mutation.PutLatest([]byte("cf"), []byte("cq"), nil, []byte(row))
	if err := writer.Add(context.Background(), mutation); err != nil {
		t.Fatal(err)
	}
}

func batchWriterPendingStart(t *testing.T, writer *BatchWriter) time.Time {
	t.Helper()
	if err := writer.lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer writer.unlock()
	return writer.pendingSince
}

func waitForBatchWriterSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for BatchWriter worker")
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func equalByteSlices(left, right [][]byte) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if !bytes.Equal(left[index], right[index]) {
			return false
		}
	}
	return true
}
