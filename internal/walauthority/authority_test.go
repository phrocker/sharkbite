package walauthority

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/ingestrouter"
)

type memoryStore struct {
	mu              sync.Mutex
	data            map[string][]byte
	events          *[]string
	appendErr       error
	appendCommitted bool
	syncErr         error
}

func newMemoryStore(events *[]string) *memoryStore {
	return &memoryStore{data: make(map[string][]byte), events: events}
}

func (s *memoryStore) Create(_ context.Context, path string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[path]; ok {
		return errors.New("exists")
	}
	s.data[path] = append([]byte(nil), data...)
	return nil
}
func (s *memoryStore) Append(_ context.Context, path string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.events != nil {
		*s.events = append(*s.events, "append")
	}
	if s.appendErr != nil && !s.appendCommitted {
		return s.appendErr
	}
	s.data[path] = append(s.data[path], data...)
	return s.appendErr
}
func (s *memoryStore) Read(_ context.Context, path string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.data[path]
	if !ok {
		return nil, errors.New("missing")
	}
	return append([]byte(nil), data...), nil
}
func (s *memoryStore) Truncate(_ context.Context, path string, size int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[path] = s.data[path][:size]
	return nil
}
func (s *memoryStore) Sync(context.Context, string) error { return s.syncErr }
func (s *memoryStore) Remove(_ context.Context, path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, path)
	return nil
}

type memoryMetadata struct {
	mu        sync.Mutex
	refs      map[string]Reference
	events    *[]string
	ensureErr error
	removeErr error
}

func newMemoryMetadata(events *[]string) *memoryMetadata {
	return &memoryMetadata{refs: make(map[string]Reference), events: events}
}
func (m *memoryMetadata) EnsureReference(_ context.Context, _ ingestrouter.Extent, _ ingestrouter.Fence, ref Reference) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.events != nil {
		*m.events = append(*m.events, "reference")
	}
	m.refs[ref.Qualifier] = ref
	return m.ensureErr
}
func (m *memoryMetadata) HasReference(_ context.Context, _ ingestrouter.Extent, _ ingestrouter.Fence, ref Reference) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.refs[ref.Qualifier]
	return ok, nil
}
func (m *memoryMetadata) RemoveReference(_ context.Context, _ ingestrouter.Extent, _ ingestrouter.Fence, ref Reference) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.refs, ref.Qualifier)
	return m.removeErr
}
func (m *memoryMetadata) References(context.Context, ingestrouter.Extent) ([]Reference, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	refs := make([]Reference, 0, len(m.refs))
	for _, ref := range m.refs {
		refs = append(refs, ref)
	}
	return refs, nil
}

type verifier struct {
	mu  sync.Mutex
	err error
	n   int
	at  int
}

func (v *verifier) Verify(context.Context, ingestrouter.Fence) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.n++
	if v.at > 0 && v.n >= v.at {
		return errors.New("fence changed")
	}
	return v.err
}

type recordingSink struct {
	mu      sync.Mutex
	applied map[string]int
	ops     []operation
	events  *[]string
}

func newSink(events *[]string) *recordingSink {
	return &recordingSink{applied: make(map[string]int), events: events}
}
func (s *recordingSink) Apply(_ context.Context, id string, seq int64, muts []ingestrouter.Mutation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.events != nil {
		*s.events = append(*s.events, "apply")
	}
	s.applied[id]++
	s.ops = append(s.ops, operation{OperationID: id, Sequence: seq, Mutations: cloneMutations(muts)})
	return nil
}

func testConfig(store Store, metadata MetadataAuthority, sink MutationSink, verify FenceVerifier) Config {
	return Config{
		Root:          "wal",
		ServerAddress: "host.example:9997",
		Extent:        ingestrouter.Extent{TableID: "5", EndRow: []byte("z")},
		Fence: ingestrouter.Fence{
			ServerGeneration:  "server-7",
			ManagerGeneration: "manager-4",
			Assignment:        9,
		},
		Metadata: metadata,
		Store:    store,
		Verifier: verify,
		Sink:     sink,
		NewID:    func() string { return "11111111-1111-4111-8111-111111111111" },
		Now:      func() time.Time { return time.Unix(69, 0) },
	}
}

func testRequest(id string, value byte) ingestrouter.CommitRequest {
	cfg := testConfig(nil, nil, nil, nil)
	return ingestrouter.CommitRequest{
		OperationID: id,
		SessionID:   "session-a",
		RequestID:   "request-b",
		Extent:      cfg.Extent,
		Fence:       cfg.Fence,
		Mutations: []ingestrouter.Mutation{{
			Row: []byte("row"),
			Updates: []ingestrouter.Update{{
				ColumnFamily: []byte("cf"), ColumnQualifier: []byte("cq"),
				Timestamp: ingestrouter.Timestamp{Set: true, Value: 17},
				Value:     []byte{value},
			}},
		}},
	}
}

func TestSealForMinorCompactionMakesAtomicSequenceBoundary(t *testing.T) {
	store := newMemoryStore(nil)
	metadata := newMemoryMetadata(nil)
	sink := newSink(nil)
	cfg := testConfig(store, metadata, sink, &verifier{})
	ids := []string{
		"11111111-1111-4111-8111-111111111111",
		"22222222-2222-4222-8222-222222222222",
	}
	cfg.NewID = func() string {
		id := ids[0]
		ids = ids[1:]
		return id
	}
	tablet, _, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := tablet.Commit(context.Background(), testRequest("before", 1)); err != nil {
		t.Fatal(err)
	}
	var captured MinorCompactionBoundary
	boundary, err := tablet.SealForMinorCompaction(context.Background(), func(value MinorCompactionBoundary) error {
		captured = value
		sink.mu.Lock()
		defer sink.mu.Unlock()
		if len(sink.ops) != 1 || sink.ops[0].OperationID != "before" {
			t.Fatalf("snapshot observed operations %+v", sink.ops)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if boundary.Sequence != 1 || captured.Sequence != 1 || len(boundary.References) != 1 {
		t.Fatalf("boundary=%+v captured=%+v", boundary, captured)
	}
	if err := tablet.Commit(context.Background(), testRequest("after", 2)); err != nil {
		t.Fatal(err)
	}
	refs, err := metadata.References(context.Background(), cfg.Extent)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 {
		t.Fatalf("references=%+v", refs)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.ops) != 2 || sink.ops[1].Sequence != 2 {
		t.Fatalf("operations=%+v", sink.ops)
	}
}

func TestSealForMinorCompactionCallbackFailureDoesNotRoll(t *testing.T) {
	store := newMemoryStore(nil)
	metadata := newMemoryMetadata(nil)
	cfg := testConfig(store, metadata, newSink(nil), &verifier{})
	tablet, _, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := tablet.Commit(context.Background(), testRequest("before", 1)); err != nil {
		t.Fatal(err)
	}
	want := errors.New("snapshot failed")
	if _, err := tablet.SealForMinorCompaction(context.Background(), func(MinorCompactionBoundary) error {
		return want
	}); !errors.Is(err, want) {
		t.Fatalf("got %v, want callback failure", err)
	}
	if err := tablet.Commit(context.Background(), testRequest("after", 2)); err != nil {
		t.Fatal(err)
	}
	refs, err := metadata.References(context.Background(), cfg.Extent)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 {
		t.Fatalf("failed seal rolled WAL: %+v", refs)
	}
}

func openTest(t *testing.T, store *memoryStore, metadata *memoryMetadata, sink *recordingSink, verify *verifier) *Tablet {
	t.Helper()
	tablet, _, err := Open(context.Background(), testConfig(store, metadata, sink, verify))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return tablet
}

func TestCommitOrdersReferenceBeforeDurableAppendAndApply(t *testing.T) {
	var events []string
	store := newMemoryStore(&events)
	metadata := newMemoryMetadata(&events)
	sink := newSink(&events)
	tablet := openTest(t, store, metadata, sink, &verifier{})

	if err := tablet.Commit(context.Background(), testRequest("op-1", 1)); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	want := []string{"reference", "append", "apply"}
	if fmt.Sprint(events) != fmt.Sprint(want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	if sink.ops[0].Sequence != 1 {
		t.Fatalf("sequence = %d", sink.ops[0].Sequence)
	}
}

func TestCommitReconcilesAmbiguousReferenceAndAppend(t *testing.T) {
	store := newMemoryStore(nil)
	store.appendErr = errors.New("connection reset after fsync")
	store.appendCommitted = true
	metadata := newMemoryMetadata(nil)
	metadata.ensureErr = errors.New("metadata response lost")
	sink := newSink(nil)
	tablet := openTest(t, store, metadata, sink, &verifier{})

	if err := tablet.Commit(context.Background(), testRequest("op-ambiguous", 2)); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if sink.applied["op-ambiguous"] != 1 {
		t.Fatalf("apply count = %d", sink.applied["op-ambiguous"])
	}
}

type partialAppendStore struct {
	*memoryStore
	once bool
}

func (s *partialAppendStore) Append(ctx context.Context, path string, data []byte) error {
	if !s.once {
		s.once = true
		s.mu.Lock()
		s.data[path] = append(s.data[path], data[:len(data)/2]...)
		s.mu.Unlock()
		return errors.New("short durable append")
	}
	return s.memoryStore.Append(ctx, path, data)
}

func TestPartialAppendIsTruncatedBeforeRetry(t *testing.T) {
	base := newMemoryStore(nil)
	store := &partialAppendStore{memoryStore: base}
	metadata := newMemoryMetadata(nil)
	sink := newSink(nil)
	tablet, _, err := Open(context.Background(), testConfig(store, metadata, sink, &verifier{}))
	if err != nil {
		t.Fatal(err)
	}
	req := testRequest("partial", 10)
	if err := tablet.Commit(context.Background(), req); err == nil {
		t.Fatal("partial append unexpectedly succeeded")
	}
	if err := tablet.Commit(context.Background(), req); err != nil {
		t.Fatalf("retry Commit: %v", err)
	}
	ref, _ := tablet.Roll(context.Background())
	ops, _, truncated, err := tablet.readWAL(context.Background(), ref)
	if err != nil || truncated || len(ops) != 1 {
		t.Fatalf("readWAL = %d ops, truncated=%v, err=%v", len(ops), truncated, err)
	}
}

type cancelAfterAppendStore struct {
	*memoryStore
	cancel context.CancelFunc
}

func (s *cancelAfterAppendStore) Append(ctx context.Context, path string, data []byte) error {
	if err := s.memoryStore.Append(ctx, path, data); err != nil {
		return err
	}
	s.cancel()
	return ctx.Err()
}

func TestCancellationAfterDurableAppendIsReconciled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	base := newMemoryStore(nil)
	store := &cancelAfterAppendStore{memoryStore: base, cancel: cancel}
	metadata := newMemoryMetadata(nil)
	sink := newSink(nil)
	tablet, _, err := Open(context.Background(), testConfig(store, metadata, sink, &verifier{}))
	if err != nil {
		t.Fatal(err)
	}
	if err := tablet.Commit(ctx, testRequest("cancelled", 8)); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if sink.applied["cancelled"] != 1 {
		t.Fatalf("apply count = %d", sink.applied["cancelled"])
	}
	if err := tablet.Commit(context.Background(), testRequest("cancelled", 8)); err != nil {
		t.Fatalf("retry Commit: %v", err)
	}
	if sink.applied["cancelled"] != 1 {
		t.Fatalf("retry reapplied batch %d times", sink.applied["cancelled"])
	}
}

func TestFenceLossAfterAppendPreventsAcknowledgement(t *testing.T) {
	store := newMemoryStore(nil)
	metadata := newMemoryMetadata(nil)
	sink := newSink(nil)
	verify := &verifier{at: 4} // Open twice, pre-append, then post-append fails.
	tablet := openTest(t, store, metadata, sink, verify)
	err := tablet.Commit(context.Background(), testRequest("lost-after-append", 9))
	if !errors.Is(err, ErrStaleOwner) {
		t.Fatalf("Commit error = %v", err)
	}
	if sink.applied["lost-after-append"] != 0 {
		t.Fatal("stale owner applied mutation after losing fence")
	}
	if len(tablet.ops) != 1 {
		t.Fatal("durable operation was not retained for recovery")
	}
}

func TestCommitRetryIsIdempotentAndConflictIsRejected(t *testing.T) {
	store := newMemoryStore(nil)
	metadata := newMemoryMetadata(nil)
	sink := newSink(nil)
	tablet := openTest(t, store, metadata, sink, &verifier{})
	req := testRequest("same", 3)

	if err := tablet.Commit(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if err := tablet.Commit(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	changed := testRequest("same", 4)
	if err := tablet.Commit(context.Background(), changed); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflict error = %v", err)
	}
}

func TestStaleOwnerRejectedBeforeAppend(t *testing.T) {
	store := newMemoryStore(nil)
	metadata := newMemoryMetadata(nil)
	sink := newSink(nil)
	verify := &verifier{}
	tablet := openTest(t, store, metadata, sink, verify)
	verify.err = errors.New("service lock lost")

	if err := tablet.Commit(context.Background(), testRequest("stale", 1)); !errors.Is(err, ErrStaleOwner) {
		t.Fatalf("Commit error = %v", err)
	}
	if len(metadata.refs) != 0 {
		t.Fatalf("metadata refs = %v", metadata.refs)
	}
}

func TestConcurrentDuplicateAppendsOnce(t *testing.T) {
	store := newMemoryStore(nil)
	metadata := newMemoryMetadata(nil)
	sink := newSink(nil)
	tablet := openTest(t, store, metadata, sink, &verifier{})
	req := testRequest("concurrent", 5)
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- tablet.Commit(context.Background(), req)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	ref := tablet.current.ref
	ops, _, _, err := tablet.readWAL(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 {
		t.Fatalf("WAL operations = %d", len(ops))
	}
}

func TestRecoveryRepairsTruncatedTailAndReplaysOnce(t *testing.T) {
	store := newMemoryStore(nil)
	metadata := newMemoryMetadata(nil)
	firstSink := newSink(nil)
	tablet := openTest(t, store, metadata, firstSink, &verifier{})
	if err := tablet.Commit(context.Background(), testRequest("recover-me", 6)); err != nil {
		t.Fatal(err)
	}
	ref, err := tablet.Roll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.data[ref.Path] = append(store.data[ref.Path], []byte{0, 0, 0, 20, 1, 2}...)
	store.mu.Unlock()

	recoveredSink := newSink(nil)
	recovered, report, err := Open(context.Background(), testConfig(store, metadata, recoveredSink, &verifier{}))
	if err != nil {
		t.Fatalf("recover Open: %v", err)
	}
	if report.Applied != 1 || len(report.Truncated) != 1 || recovered.nextSeq != 2 {
		t.Fatalf("report = %#v, next = %d", report, recovered.nextSeq)
	}
	if err := recovered.Commit(context.Background(), testRequest("recover-me", 6)); err != nil {
		t.Fatal(err)
	}
	if recoveredSink.applied["recover-me"] != 1 {
		t.Fatalf("sink calls = %d, want one replay", recoveredSink.applied["recover-me"])
	}
}

func TestRecoveryRejectsChecksumCorruptionWithoutApplying(t *testing.T) {
	store := newMemoryStore(nil)
	metadata := newMemoryMetadata(nil)
	tablet := openTest(t, store, metadata, newSink(nil), &verifier{})
	if err := tablet.Commit(context.Background(), testRequest("corrupt", 7)); err != nil {
		t.Fatal(err)
	}
	ref, _ := tablet.Roll(context.Background())
	store.mu.Lock()
	store.data[ref.Path][len(store.data[ref.Path])-1] ^= 0xff
	store.mu.Unlock()
	sink := newSink(nil)
	_, _, err := Open(context.Background(), testConfig(store, metadata, sink, &verifier{}))
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open error = %v", err)
	}
	if len(sink.ops) != 0 {
		t.Fatalf("applied %d records from corrupt WAL", len(sink.ops))
	}
}

func TestRolloverAndRetireKeepOldReferenceUntilExplicitCommitBoundary(t *testing.T) {
	ids := []string{
		"11111111-1111-4111-8111-111111111111",
		"22222222-2222-4222-8222-222222222222",
	}
	cfg := testConfig(newMemoryStore(nil), newMemoryMetadata(nil), newSink(nil), &verifier{})
	cfg.NewID = func() string {
		id := ids[0]
		ids = ids[1:]
		return id
	}
	tablet, _, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := tablet.Commit(context.Background(), testRequest("first", 1)); err != nil {
		t.Fatal(err)
	}
	old, err := tablet.Roll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := tablet.Commit(context.Background(), testRequest("second", 2)); err != nil {
		t.Fatal(err)
	}
	refs, _ := cfg.Metadata.References(context.Background(), cfg.Extent)
	if len(refs) != 2 {
		t.Fatalf("references before retire = %d", len(refs))
	}
	if err := tablet.Retire(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	refs, _ = cfg.Metadata.References(context.Background(), cfg.Extent)
	if len(refs) != 1 {
		t.Fatalf("references after retire = %d", len(refs))
	}
}

func TestRandomMutationRoundTripProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(69))
	store := newMemoryStore(nil)
	metadata := newMemoryMetadata(nil)
	tablet := openTest(t, store, metadata, newSink(nil), &verifier{})
	const count = 500
	for i := range count {
		req := testRequest(fmt.Sprintf("op-%03d", i), byte(rng.Intn(256)))
		req.SessionID = fmt.Sprintf("session-%d", rng.Intn(7))
		req.RequestID = fmt.Sprintf("request-%d", rng.Int63())
		req.Mutations[0].Row = []byte{byte(rng.Intn(255) + 1)}
		req.Mutations[0].Updates[0].Delete = rng.Intn(5) == 0
		req.Mutations[0].Updates[0].Timestamp.Value = rng.Int63()
		if err := tablet.Commit(context.Background(), req); err != nil {
			t.Fatalf("Commit %d: %v", i, err)
		}
	}
	_, _ = tablet.Roll(context.Background())
	sink := newSink(nil)
	_, report, err := Open(context.Background(), testConfig(store, metadata, sink, &verifier{}))
	if err != nil {
		t.Fatal(err)
	}
	if report.Applied != count || report.HighestSequence != count {
		t.Fatalf("report = %#v", report)
	}
	for i, op := range sink.ops {
		if op.Sequence != int64(i+1) {
			t.Fatalf("sequence[%d] = %d", i, op.Sequence)
		}
	}
}

func TestLocalStorePersistsAndTruncates(t *testing.T) {
	root := t.TempDir()
	path := root + `\host+9997\11111111-1111-4111-8111-111111111111`
	store := NewLocalStore()
	if err := store.Create(context.Background(), path, []byte("header")); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(context.Background(), path, []byte("record")); err != nil {
		t.Fatal(err)
	}
	if err := store.Truncate(context.Background(), path, 6); err != nil {
		t.Fatal(err)
	}
	data, err := store.Read(context.Background(), path)
	if err != nil || string(data) != "header" {
		t.Fatalf("Read = %q, %v", data, err)
	}
	if err := store.Remove(context.Background(), path); err != nil {
		t.Fatal(err)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	store := newMemoryStore(nil)
	tablet := openTest(t, store, newMemoryMetadata(nil), newSink(nil), &verifier{})
	if err := tablet.Commit(context.Background(), testRequest("before-close", 1)); err != nil {
		t.Fatal(err)
	}
	store.syncErr = errors.New("sync unavailable")
	if err := tablet.Close(context.Background()); err == nil {
		t.Fatal("Close unexpectedly succeeded")
	}
	if tablet.closed {
		t.Fatal("failed Close made tablet terminal")
	}
	store.syncErr = nil
	if err := tablet.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := tablet.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := tablet.Commit(context.Background(), testRequest("closed", 1)); !errors.Is(err, ErrClosed) {
		t.Fatalf("Commit error = %v", err)
	}
}

func TestJoinPathPreservesAccumuloVolumeURI(t *testing.T) {
	got := joinPath("hdfs://namenode:8020/accumulo/wal", "host+9997", "id")
	want := "hdfs://namenode:8020/accumulo/wal/host+9997/id"
	if got != want {
		t.Fatalf("joinPath = %q, want %q", got, want)
	}
}

func TestOpenHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := Open(ctx, testConfig(newMemoryStore(nil), newMemoryMetadata(nil), newSink(nil), &verifier{}))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Open error = %v", err)
	}
}

func TestOpenRejectsUnsafeServerAddress(t *testing.T) {
	for _, address := range []string{"..:9997", `host\child:9997`, "host:not-a-port", "host:0"} {
		cfg := testConfig(newMemoryStore(nil), newMemoryMetadata(nil), newSink(nil), &verifier{})
		cfg.ServerAddress = address
		if _, _, err := Open(context.Background(), cfg); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("Open(%q) error = %v", address, err)
		}
	}
}

func TestReconcileTimeoutDefaultIsFinite(t *testing.T) {
	tablet := openTest(t, newMemoryStore(nil), newMemoryMetadata(nil), newSink(nil), &verifier{})
	ctx, cancel := tablet.reconcileContext()
	defer cancel()
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 6*time.Second {
		t.Fatalf("reconcile deadline = %v, %v", deadline, ok)
	}
}
