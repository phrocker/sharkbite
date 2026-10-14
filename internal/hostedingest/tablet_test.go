package hostedingest

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/phrocker/shoal-oss/internal/ingestrouter"
	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/mincauthority"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
	"github.com/phrocker/shoal-oss/internal/tabletloader"
	"github.com/phrocker/shoal-oss/internal/tserver"
	"github.com/phrocker/shoal-oss/internal/walauthority"
)

type fakeMetadataFactory struct{ metadata *fakeMetadata }

func (f fakeMetadataFactory) Open(
	context.Context,
	tabletloader.Specification,
	ingestrouter.Fence,
) (MetadataAuthority, error) {
	return f.metadata, nil
}

type systemMetadataFactory struct{ fakeMetadataFactory }

func (systemMetadataFactory) SupportsSystemTablets() bool { return true }

func TestFactoryRefusesSystemTabletWithoutConditionalHosting(t *testing.T) {
	host, attempt := hostAssignment(t)
	factory := testFactory(t, host, &fakeMetadata{}, 1)
	_, err := factory.Open(context.Background(), tabletloader.Specification{
		Extent: tserver.Extent{TableID: metadata.MetadataTableID},
	}, attempt)
	if !errors.Is(err, ErrSystemTabletConditionalUnsupported) {
		t.Fatalf("Open metadata tablet = %v", err)
	}
}

func TestFactoryAllowsSystemTabletWithConditionalHosting(t *testing.T) {
	host, attempt := hostAssignment(t)
	factory := testFactory(t, host, &fakeMetadata{}, 1)
	factory.cfg.Metadata = systemMetadataFactory{
		fakeMetadataFactory{metadata: &fakeMetadata{}},
	}
	tablet, err := factory.Open(context.Background(), tabletloader.Specification{
		Extent: tserver.Extent{TableID: metadata.RootTableID},
	}, attempt)
	if err != nil {
		t.Fatal(err)
	}
	if tablet == nil {
		t.Fatal("Open returned a nil tablet")
	}
}

func TestInitialTabletTimePreservesLogicalAndAdvancesMillis(t *testing.T) {
	kind, current, next, exhausted, err := initialTabletTime("L17", time.UnixMilli(100))
	if err != nil || kind != 'L' || current != 17 || next != 18 || exhausted {
		t.Fatalf("logical time = %q,%d,%d,%t,%v", kind, current, next, exhausted, err)
	}
	kind, current, next, exhausted, err = initialTabletTime("M17", time.UnixMilli(100))
	if err != nil || kind != 'M' || current != 17 || next != 100 || exhausted {
		t.Fatalf("millis time = %q,%d,%d,%t,%v", kind, current, next, exhausted, err)
	}
}

type fakeMetadata struct {
	mu                 sync.Mutex
	refs               []walauthority.Reference
	files              []mincauthority.DataFile
	tabletTime         string
	ambiguousReference bool
	ambiguousCommit    bool
	releases           int
}

func (m *fakeMetadata) EnsureReference(
	_ context.Context,
	_ ingestrouter.Extent,
	_ ingestrouter.Fence,
	ref walauthority.Reference,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.refs {
		if existing == ref {
			return nil
		}
	}
	m.refs = append(m.refs, ref)
	if m.ambiguousReference {
		m.ambiguousReference = false
		return errors.New("lost metadata response")
	}
	return nil
}

func (m *fakeMetadata) HasReference(
	_ context.Context,
	_ ingestrouter.Extent,
	_ ingestrouter.Fence,
	ref walauthority.Reference,
) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.refs {
		if existing == ref {
			return true, nil
		}
	}
	return false, nil
}

func (m *fakeMetadata) RemoveReference(
	_ context.Context,
	_ ingestrouter.Extent,
	_ ingestrouter.Fence,
	ref walauthority.Reference,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, existing := range m.refs {
		if existing == ref {
			m.refs = append(m.refs[:i], m.refs[i+1:]...)
			return nil
		}
	}
	return nil
}

func (m *fakeMetadata) References(context.Context, ingestrouter.Extent) ([]walauthority.Reference, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]walauthority.Reference(nil), m.refs...), nil
}

func (m *fakeMetadata) Release(
	context.Context,
	ingestrouter.Extent,
	ingestrouter.Fence,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.releases++
	return nil
}

func (m *fakeMetadata) Commit(
	_ context.Context,
	request mincauthority.MetadataCommit,
) (mincauthority.CommitOutcome, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, remove := range request.RemoveWALs {
		found := false
		for _, ref := range m.refs {
			if ref == remove {
				found = true
			}
		}
		if !found {
			return mincauthority.CommitRejected, nil
		}
	}
	for _, remove := range request.RemoveWALs {
		for i := 0; i < len(m.refs); i++ {
			if m.refs[i] == remove {
				m.refs = append(m.refs[:i], m.refs[i+1:]...)
				i--
			}
		}
	}
	m.files = append(m.files, request.File)
	m.tabletTime = request.TabletTime
	if m.ambiguousCommit {
		m.ambiguousCommit = false
		return mincauthority.CommitUnknown, errors.New("lost CAS response")
	}
	return mincauthority.CommitApplied, nil
}

func (m *fakeMetadata) Read(
	context.Context,
	ingestrouter.Extent,
) (mincauthority.MetadataState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return mincauthority.MetadataState{
		Files: append([]mincauthority.DataFile(nil), m.files...),
		WALs:  append([]walauthority.Reference(nil), m.refs...),
		Time:  m.tabletTime,
	}, nil
}

func hostAssignment(t *testing.T) (*tserver.Host, tserver.Attempt) {
	t.Helper()
	host := tserver.NewHost()
	server := tserver.LockID{UUID: uuid.NewString(), Sequence: 1}
	manager := tserver.LockID{UUID: uuid.NewString(), Sequence: 2}
	if err := host.AdoptLock(server); err != nil {
		t.Fatal(err)
	}
	if err := host.ObserveManagerLock(manager); err != nil {
		t.Fatal(err)
	}
	attempt, err := host.Assign(tserver.Fence{Server: server, Manager: manager},
		tserver.Extent{TableID: "1", EndRow: []byte("z")})
	if err != nil {
		t.Fatal(err)
	}
	return host, attempt
}

func testFactory(t *testing.T, host *tserver.Host, metadata *fakeMetadata, flushCells int) *Factory {
	t.Helper()
	root := t.TempDir()
	factory, err := NewFactory(Config{
		Host: host, ServerAddress: "127.0.0.1:9997",
		WALRoot: root + "\\wal", MincRoot: "rfiles", StateRoot: root + "\\state",
		WALStore: walauthority.NewLocalStore(), Outputs: memory.New(),
		Metadata: fakeMetadataFactory{metadata: metadata}, FlushCells: flushCells,
	})
	if err != nil {
		t.Fatal(err)
	}
	return factory
}

func TestFactoryRollsBackMetadataClaimWhenPostClaimOpenFails(t *testing.T) {
	host, attempt := hostAssignment(t)
	missing := filepath.Join(t.TempDir(), "missing-wal")
	metadataAuthority := &fakeMetadata{refs: []walauthority.Reference{{
		ID: "missing", Path: missing, Qualifier: "-/" + missing,
	}}}
	factory := testFactory(t, host, metadataAuthority, 1)
	_, err := factory.Open(context.Background(), tabletloader.Specification{
		Extent: tserver.Extent{TableID: "1", EndRow: []byte("z")},
	}, attempt)
	if err == nil {
		t.Fatal("Open unexpectedly succeeded")
	}
	metadataAuthority.mu.Lock()
	defer metadataAuthority.mu.Unlock()
	if metadataAuthority.releases != 1 {
		t.Fatalf("metadata releases = %d, want 1", metadataAuthority.releases)
	}
}

func testRequest(attempt tserver.Attempt) ingestrouter.CommitRequest {
	return ingestrouter.CommitRequest{
		OperationID: "op-1", SessionID: "session", RequestID: "request",
		Extent: ingestrouter.Extent{TableID: "1", EndRow: []byte("z")},
		Mutations: []ingestrouter.Mutation{{
			Row: []byte("a"),
			Updates: []ingestrouter.Update{{
				ColumnFamily: []byte("cf"), ColumnQualifier: []byte("cq"), Value: []byte("v"),
			}},
		}},
	}
}

func TestTabletDurableCommitMinorCompactionAndAmbiguousResponses(t *testing.T) {
	host, attempt := hostAssignment(t)
	metadata := &fakeMetadata{ambiguousReference: true, ambiguousCommit: true}
	factory := testFactory(t, host, metadata, 1)
	opened, err := factory.Open(context.Background(), tabletloader.Specification{
		Extent: tserver.Extent{TableID: "1", EndRow: []byte("z")},
	}, attempt)
	if err != nil {
		t.Fatal(err)
	}
	if err := host.LoadComplete(attempt); err != nil {
		t.Fatal(err)
	}
	tablet := opened.(*Tablet)
	request := testRequest(attempt)
	request.Fence = tablet.Fence()
	if err := tablet.Commit(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	metadata.mu.Lock()
	defer metadata.mu.Unlock()
	if len(metadata.refs) != 0 || len(metadata.files) != 1 || metadata.files[0].Entries != 1 {
		t.Fatalf("metadata refs=%#v files=%#v", metadata.refs, metadata.files)
	}
}

func TestTabletRestartReplaysReferencedWALExactlyOnce(t *testing.T) {
	host, attempt := hostAssignment(t)
	metadata := &fakeMetadata{}
	factory := testFactory(t, host, metadata, 100)
	spec := tabletloader.Specification{Extent: tserver.Extent{TableID: "1", EndRow: []byte("z")}}
	opened, err := factory.Open(context.Background(), spec, attempt)
	if err != nil {
		t.Fatal(err)
	}
	if err := host.LoadComplete(attempt); err != nil {
		t.Fatal(err)
	}
	first := opened.(*Tablet)
	request := testRequest(attempt)
	request.Fence = first.Fence()
	if err := first.Commit(context.Background(), request); err != nil {
		t.Fatal(err)
	}

	reopened, err := factory.Open(context.Background(), spec, attempt)
	if err != nil {
		t.Fatal(err)
	}
	second := reopened.(*Tablet)
	if second.Recovery().Applied != 1 || second.Recovery().HighestSequence != 1 {
		t.Fatalf("recovery = %#v", second.Recovery())
	}
	request.Fence = second.Fence()
	if err := second.Commit(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	second.mu.Lock()
	defer second.mu.Unlock()
	if len(second.active) != 0 || len(second.files) != 1 {
		t.Fatalf("recovery visibility: active=%d files=%d, want flushed exactly once",
			len(second.active), len(second.files))
	}
}

func TestTabletFencesStaleOwnerDuringCommit(t *testing.T) {
	host, attempt := hostAssignment(t)
	factory := testFactory(t, host, &fakeMetadata{}, 100)
	opened, err := factory.Open(context.Background(), tabletloader.Specification{
		Extent: tserver.Extent{TableID: "1", EndRow: []byte("z")},
	}, attempt)
	if err != nil {
		t.Fatal(err)
	}
	if err := host.LoadComplete(attempt); err != nil {
		t.Fatal(err)
	}
	tablet := opened.(*Tablet)
	host.LoseLock(tserver.LockID{UUID: tablet.verifier.server.UUID, Sequence: tablet.verifier.server.Sequence})
	request := testRequest(attempt)
	request.Fence = tablet.Fence()
	if err := tablet.Commit(context.Background(), request); !errors.Is(err, ingestrouter.ErrStaleFence) {
		t.Fatalf("commit error = %v, want stale fence", err)
	}
}

func TestTabletCommitAndCloseAreSerialized(t *testing.T) {
	host, attempt := hostAssignment(t)
	factory := testFactory(t, host, &fakeMetadata{}, 1000)
	opened, err := factory.Open(context.Background(), tabletloader.Specification{
		Extent: tserver.Extent{TableID: "1", EndRow: []byte("z")},
	}, attempt)
	if err != nil {
		t.Fatal(err)
	}
	if err := host.LoadComplete(attempt); err != nil {
		t.Fatal(err)
	}
	tablet := opened.(*Tablet)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			request := testRequest(attempt)
			request.OperationID = "op-" + string(rune('a'+index))
			request.RequestID = request.OperationID
			request.Fence = tablet.Fence()
			err := tablet.Commit(context.Background(), request)
			if err != nil && !errors.Is(err, walauthority.ErrClosed) {
				t.Errorf("commit %d: %v", index, err)
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := tablet.Close(context.Background()); err != nil {
			t.Errorf("close: %v", err)
		}
	}()
	wg.Wait()
}
