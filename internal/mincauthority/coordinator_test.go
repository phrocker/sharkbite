package mincauthority

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/phrocker/shoal-oss/internal/ingestrouter"
	"github.com/phrocker/shoal-oss/internal/rfile"
	"github.com/phrocker/shoal-oss/internal/walauthority"
)

var (
	testExtent = ingestrouter.Extent{TableID: "5", PrevEndRow: []byte("a"), EndRow: []byte("z")}
	testFence  = ingestrouter.Fence{ServerGeneration: "server", ManagerGeneration: "manager", Assignment: 7}
	testWAL1   = walauthority.Reference{ID: "w1", Path: "wal/w1", Qualifier: "-/wal/w1"}
	testWAL2   = walauthority.Reference{ID: "w2", Path: "wal/w2", Qualifier: "-/wal/w2"}
	extraWAL   = walauthority.Reference{ID: "later", Path: "wal/later", Qualifier: "-/wal/later"}
)

type fakeSnapshotter struct {
	mu            sync.Mutex
	snapshot      Snapshot
	prepareCalls  int
	completeCalls int
	completeErr   error
}

func (f *fakeSnapshotter) Prepare(_ context.Context, _ string, _ ingestrouter.Extent, _ ingestrouter.Fence) (Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prepareCalls++
	return cloneSnapshot(f.snapshot), nil
}

func (f *fakeSnapshotter) Complete(_ context.Context, id string, _ DataFile) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id != f.snapshot.ID {
		return errors.New("wrong snapshot")
	}
	f.completeCalls++
	err := f.completeErr
	f.completeErr = nil
	return err
}

type fakeVerifier struct {
	mu    sync.Mutex
	stale bool
	calls int
}

func (v *fakeVerifier) Verify(context.Context, ingestrouter.Fence) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls++
	if v.stale {
		return errors.New("unloaded")
	}
	return nil
}

type fakeOutput struct {
	mu                   sync.Mutex
	data                 map[string][]byte
	publishCalls         int
	publishAfterWriteErr error
	corruptOnPublish     bool
}

func (o *fakeOutput) Publish(_ context.Context, path string, data []byte) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.publishCalls++
	if o.data == nil {
		o.data = make(map[string][]byte)
	}
	if prior, ok := o.data[path]; ok && !bytes.Equal(prior, data) {
		return errors.New("immutable collision")
	}
	stored := append([]byte(nil), data...)
	if o.corruptOnPublish && len(stored) > 0 {
		stored[len(stored)/2] ^= 0xff
	}
	o.data[path] = stored
	err := o.publishAfterWriteErr
	o.publishAfterWriteErr = nil
	return err
}

func (o *fakeOutput) Read(_ context.Context, path string) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	data, ok := o.data[path]
	if !ok {
		return nil, os.ErrNotExist
	}
	return append([]byte(nil), data...), nil
}

func (o *fakeOutput) Remove(_ context.Context, path string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.data, path)
	return nil
}

type fakeMetadata struct {
	mu           sync.Mutex
	state        MetadataState
	calls        int
	outcome      CommitOutcome
	err          error
	beforeCommit func()
}

func (m *fakeMetadata) Commit(_ context.Context, req MetadataCommit) (CommitOutcome, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.beforeCommit != nil {
		m.beforeCommit()
	}
	for _, file := range m.state.Files {
		if file.Path == req.File.Path {
			if equalFile(file, req.File) && nonePresent(m.state.WALs, req.RemoveWALs) {
				return CommitApplied, m.err
			}
			return CommitRejected, m.err
		}
	}
	if !allPresent(m.state.WALs, req.RemoveWALs) {
		return CommitRejected, m.err
	}
	outcome := m.outcome
	if outcome == 0 {
		outcome = CommitApplied
	}
	if outcome != CommitRejected {
		m.state.Files = append(m.state.Files, cloneFile(req.File))
		m.state.WALs = removeRefs(m.state.WALs, req.RemoveWALs)
	}
	return outcome, m.err
}

func (m *fakeMetadata) Read(context.Context, ingestrouter.Extent) (MetadataState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return MetadataState{Files: append([]DataFile(nil), m.state.Files...), WALs: cloneRefs(m.state.WALs)}, nil
}

type memoryStates struct {
	mu        sync.Mutex
	states    map[string]State
	failPhase Phase
	failOnce  bool
	onSave    func(State)
}

func (s *memoryStates) Load(_ context.Context, id string) (*State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.states[id]
	if !ok {
		return nil, nil
	}
	copy := cloneState(state)
	return &copy, nil
}

func (s *memoryStates) Save(_ context.Context, state State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.onSave != nil {
		s.onSave(state)
	}
	if s.failOnce && state.Phase == s.failPhase {
		s.failOnce = false
		return errors.New("simulated crash")
	}
	if s.states == nil {
		s.states = make(map[string]State)
	}
	s.states[state.OperationID] = cloneState(state)
	return nil
}

func TestCoordinatorCommitsNativeRFileAndOnlyCoveredWALs(t *testing.T) {
	fixture := newFixture(t)
	file, err := fixture.coordinator.Run(context.Background(), "op-1")
	if err != nil {
		t.Fatal(err)
	}
	if file.Format != "rfile" || file.Entries != 3 || file.Boundary != 42 {
		t.Fatalf("unexpected file: %+v", file)
	}
	state, _ := fixture.metadata.Read(context.Background(), testExtent)
	if len(state.Files) != 1 || len(state.WALs) != 1 || state.WALs[0] != extraWAL {
		t.Fatalf("metadata did not preserve only unrelated WAL: %+v", state)
	}
	if fixture.snapshots.completeCalls != 1 {
		t.Fatalf("complete calls = %d", fixture.snapshots.completeCalls)
	}
	if _, err := fixture.coordinator.Run(context.Background(), "op-1"); err != nil {
		t.Fatal(err)
	}
	if fixture.metadata.calls != 1 || fixture.snapshots.completeCalls != 1 {
		t.Fatalf("duplicate run repeated side effects: commits=%d completes=%d",
			fixture.metadata.calls, fixture.snapshots.completeCalls)
	}
}

func TestCoordinatorReconcilesAmbiguousPublishAndMetadata(t *testing.T) {
	fixture := newFixture(t)
	fixture.outputs.publishAfterWriteErr = errors.New("lost publish response")
	fixture.metadata.outcome = CommitUnknown
	fixture.metadata.err = errors.New("lost CAS response")
	if _, err := fixture.coordinator.Run(context.Background(), "op-ambiguous"); err != nil {
		t.Fatal(err)
	}
	if fixture.metadata.calls != 1 {
		t.Fatalf("commit calls = %d", fixture.metadata.calls)
	}
}

func TestCoordinatorRejectsCorruptPublishedOutput(t *testing.T) {
	fixture := newFixture(t)
	fixture.outputs.corruptOnPublish = true
	_, err := fixture.coordinator.Run(context.Background(), "op-corrupt")
	if !errors.Is(err, ErrCorruptOutput) {
		t.Fatalf("got %v, want ErrCorruptOutput", err)
	}
	if fixture.metadata.calls != 0 {
		t.Fatal("corrupt output reached metadata")
	}
}

func TestCoordinatorRejectsConcurrentCoveredWALChange(t *testing.T) {
	fixture := newFixture(t)
	fixture.metadata.state.WALs = []walauthority.Reference{testWAL1, extraWAL}
	fixture.metadata.outcome = CommitRejected
	_, err := fixture.coordinator.Run(context.Background(), "op-race")
	if !errors.Is(err, ErrMetadataInconsistent) {
		t.Fatalf("got %v, want inconsistent metadata", err)
	}
	if fixture.snapshots.completeCalls != 0 {
		t.Fatal("snapshot completed after rejected CAS")
	}
}

func TestCoordinatorPreservesConcurrentUnrelatedMetadata(t *testing.T) {
	fixture := newFixture(t)
	unrelated := DataFile{Path: "hdfs://accumulo/tables/5/existing.rf", Format: "rfile", Checksum: "existing"}
	fixture.metadata.state.Files = append(fixture.metadata.state.Files, unrelated)
	if _, err := fixture.coordinator.Run(context.Background(), "op-unrelated"); err != nil {
		t.Fatal(err)
	}
	state, _ := fixture.metadata.Read(context.Background(), testExtent)
	if len(state.Files) != 2 || state.Files[0].Path != unrelated.Path {
		t.Fatalf("unrelated metadata was not preserved: %+v", state.Files)
	}
}

func TestCoordinatorFailsClosedWhenOwnerBecomesStaleBeforeCAS(t *testing.T) {
	fixture := newFixture(t)
	fixture.states.onSave = func(state State) {
		if state.Phase == PhaseValidated {
			fixture.verifier.mu.Lock()
			fixture.verifier.stale = true
			fixture.verifier.mu.Unlock()
		}
	}
	_, err := fixture.coordinator.Run(context.Background(), "op-unload")
	if !errors.Is(err, ErrStaleOwner) {
		t.Fatalf("got %v, want stale owner", err)
	}
	if fixture.metadata.calls != 0 {
		t.Fatal("stale owner reached metadata CAS")
	}
}

func TestCoordinatorLosesOwnershipAtMetadataCAS(t *testing.T) {
	fixture := newFixture(t)
	fixture.metadata.beforeCommit = func() {
		fixture.metadata.outcome = CommitRejected
		fixture.metadata.err = errors.New("conditional owner mismatch")
	}
	_, err := fixture.coordinator.Run(context.Background(), "op-cas-fence")
	if !errors.Is(err, ErrConcurrentMetadataChange) {
		t.Fatalf("got %v, want concurrent metadata change", err)
	}
	if fixture.snapshots.completeCalls != 0 {
		t.Fatal("snapshot completed after fenced CAS rejection")
	}
}

func TestCoordinatorRecoversAfterCrashAtEveryCheckpoint(t *testing.T) {
	for _, phase := range []Phase{
		PhaseSnapshotted, PhasePublished, PhaseValidated, PhaseCommitted, PhaseComplete,
	} {
		t.Run(fmt.Sprintf("phase-%d", phase), func(t *testing.T) {
			fixture := newFixture(t)
			fixture.states.failPhase, fixture.states.failOnce = phase, true
			if _, err := fixture.coordinator.Run(context.Background(), "op-crash"); err == nil {
				t.Fatal("expected simulated crash")
			}
			restarted, err := New(fixture.config)
			if err != nil {
				t.Fatal(err)
			}
			file, err := restarted.Run(context.Background(), "op-crash")
			if err != nil {
				t.Fatal(err)
			}
			if file.Format != "rfile" {
				t.Fatalf("format = %q", file.Format)
			}
			state, _ := fixture.states.Load(context.Background(), "op-crash")
			if state == nil || state.Phase != PhaseComplete {
				t.Fatalf("final state = %+v", state)
			}
			metadata, _ := fixture.metadata.Read(context.Background(), testExtent)
			if len(metadata.Files) != 1 || len(metadata.WALs) != 1 || metadata.WALs[0] != extraWAL {
				t.Fatalf("metadata after recovery = %+v", metadata)
			}
		})
	}
}

func TestCoordinatorConcurrentRetriesAreIdempotent(t *testing.T) {
	fixture := newFixture(t)
	const workers = 16
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := fixture.coordinator.Run(context.Background(), "same-op")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if fixture.metadata.calls != 1 || fixture.snapshots.completeCalls != 1 {
		t.Fatalf("side effects: commits=%d completes=%d", fixture.metadata.calls, fixture.snapshots.completeCalls)
	}
}

func TestCoordinatorRandomizedRFileRangeProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(69))
	for iteration := 0; iteration < 50; iteration++ {
		fixture := newFixture(t)
		count := 1 + rng.Intn(80)
		cells := make([]Cell, count)
		for i := range cells {
			row := []byte{byte('b' + rng.Intn(24))}
			cells[i] = Cell{
				Key: rfile.Key{
					Row: row, ColumnFamily: []byte{byte(rng.Intn(5))},
					ColumnQualifier: []byte{byte(rng.Intn(11))}, Timestamp: rng.Int63(),
					Deleted: rng.Intn(7) == 0,
				},
				Value: []byte{byte(i), byte(rng.Intn(256))},
			}
		}
		fixture.snapshots.snapshot.Cells = cells
		file, err := fixture.coordinator.Run(context.Background(), fmt.Sprintf("property-%d", iteration))
		if err != nil {
			t.Fatalf("iteration %d: %v", iteration, err)
		}
		if file.Entries != int64(count) || bytes.Compare(file.StartRow, []byte("a")) <= 0 ||
			bytes.Compare(file.EndRow, []byte("z")) > 0 {
			t.Fatalf("iteration %d: invalid file %+v", iteration, file)
		}
	}
}

func TestFileStateStoreDetectsCorruptionAndPersists(t *testing.T) {
	dir := t.TempDir()
	store := &FileStateStore{Dir: dir}
	state := State{
		OperationID: "stable", Extent: testExtent, Fence: testFence, Phase: PhaseValidated,
		SnapshotCells: []Cell{{Key: rfile.Key{Row: []byte("row")}, Value: []byte("value")}},
	}
	if err := store.Save(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	loaded, err := (&FileStateStore{Dir: dir}).Load(context.Background(), "stable")
	if err != nil || loaded == nil || loaded.Phase != PhaseValidated ||
		len(loaded.SnapshotCells) != 1 || !bytes.Equal(loaded.SnapshotCells[0].Value, []byte("value")) {
		t.Fatalf("load: state=%+v err=%v", loaded, err)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 {
		t.Fatalf("files=%v err=%v", files, err)
	}
	path := filepath.Join(dir, files[0].Name())
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 1
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(context.Background(), "stable"); !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("got %v, want invalid snapshot", err)
	}
}

func TestFileStateStoreAdvancesWithoutReplacingCheckpoints(t *testing.T) {
	dir := t.TempDir()
	store := &FileStateStore{Dir: dir}
	for phase := PhaseSnapshotted; phase <= PhaseComplete; phase++ {
		if err := store.Save(context.Background(), State{
			OperationID: "advance", Extent: testExtent, Fence: testFence, Phase: phase,
		}); err != nil {
			t.Fatalf("phase %d: %v", phase, err)
		}
	}
	loaded, err := (&FileStateStore{Dir: dir}).Load(context.Background(), "advance")
	if err != nil || loaded == nil || loaded.Phase != PhaseComplete {
		t.Fatalf("load: state=%+v err=%v", loaded, err)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != int(PhaseComplete) {
		t.Fatalf("files=%d err=%v", len(files), err)
	}
}

func TestFileStateStoreDiscoversPendingOperationsAfterRestart(t *testing.T) {
	dir := t.TempDir()
	store := &FileStateStore{Dir: dir}
	for _, state := range []State{
		{OperationID: "later", Extent: testExtent, Fence: testFence, Phase: PhaseCommitted},
		{OperationID: "earlier", Extent: testExtent, Fence: testFence, Phase: PhasePublished},
		{OperationID: "other-fence", Extent: testExtent, Fence: ingestrouter.Fence{ServerGeneration: "other"}, Phase: PhaseValidated},
	} {
		if err := store.Save(context.Background(), state); err != nil {
			t.Fatal(err)
		}
	}
	for phase := PhaseSnapshotted; phase <= PhaseComplete; phase++ {
		if err := store.Save(context.Background(), State{
			OperationID: "complete", Extent: testExtent, Fence: testFence, Phase: phase,
		}); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := (&FileStateStore{Dir: dir}).Pending(context.Background(), testExtent, testFence)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 ||
		pending[0].OperationID != "earlier" || pending[0].Phase != PhasePublished ||
		pending[1].OperationID != "later" || pending[1].Phase != PhaseCommitted {
		t.Fatalf("pending = %+v", pending)
	}
}

type fixture struct {
	config      Config
	coordinator *Coordinator
	snapshots   *fakeSnapshotter
	verifier    *fakeVerifier
	outputs     *fakeOutput
	metadata    *fakeMetadata
	states      *memoryStates
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	snapshots := &fakeSnapshotter{snapshot: Snapshot{
		ID: "snapshot-1", Extent: testExtent, Fence: testFence, Boundary: 42,
		Cells: []Cell{
			{Key: rfile.Key{Row: []byte("m"), ColumnFamily: []byte("cf"), ColumnQualifier: []byte("q"), Timestamp: 2}, Value: []byte("two")},
			{Key: rfile.Key{Row: []byte("b"), ColumnFamily: []byte("cf"), ColumnQualifier: []byte("q"), Timestamp: 1}, Value: []byte("one")},
			{Key: rfile.Key{Row: []byte("y"), ColumnFamily: []byte("cf"), ColumnQualifier: []byte("q"), Timestamp: 3, Deleted: true}},
		},
		CoveredWALs: []walauthority.Reference{testWAL1, testWAL2},
	}}
	verifier := &fakeVerifier{}
	outputs := &fakeOutput{data: make(map[string][]byte)}
	metadata := &fakeMetadata{state: MetadataState{WALs: []walauthority.Reference{testWAL1, testWAL2, extraWAL}}}
	states := &memoryStates{states: make(map[string]State)}
	cfg := Config{
		Root: "hdfs://accumulo/tables", Extent: testExtent, Fence: testFence,
		Snapshots: snapshots, Verifier: verifier, Metadata: metadata, Outputs: outputs, States: states,
	}
	coordinator, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{
		config: cfg, coordinator: coordinator, snapshots: snapshots, verifier: verifier,
		outputs: outputs, metadata: metadata, states: states,
	}
}

func cloneSnapshot(in Snapshot) Snapshot {
	return Snapshot{
		ID: in.ID, Extent: cloneExtent(in.Extent), Fence: in.Fence, Boundary: in.Boundary,
		Cells: cloneCells(in.Cells), CoveredWALs: cloneRefs(in.CoveredWALs),
		Embedding: in.Embedding,
	}
}

func cloneState(in State) State {
	in.Extent = cloneExtent(in.Extent)
	in.CoveredWALs = cloneRefs(in.CoveredWALs)
	in.File = cloneFile(in.File)
	return in
}

func allPresent(current, wanted []walauthority.Reference) bool {
	for _, want := range wanted {
		found := false
		for _, ref := range current {
			found = found || ref == want
		}
		if !found {
			return false
		}
	}
	return true
}

func nonePresent(current, wanted []walauthority.Reference) bool {
	for _, want := range wanted {
		for _, ref := range current {
			if ref == want {
				return false
			}
		}
	}
	return true
}

func removeRefs(current, remove []walauthority.Reference) []walauthority.Reference {
	out := current[:0]
	for _, ref := range current {
		keep := true
		for _, deleted := range remove {
			keep = keep && ref != deleted
		}
		if keep {
			out = append(out, ref)
		}
	}
	return append([]walauthority.Reference(nil), out...)
}
