package metadatacas

import (
	"context"
	"errors"
	"sync"
	"testing"

	gozk "github.com/go-zookeeper/zk"
	"github.com/google/uuid"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/ingestclient"
	"github.com/phrocker/shoal-oss/internal/ingestrouter"
	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/mincauthority"
	"github.com/phrocker/shoal-oss/internal/tabletloader"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
	"github.com/phrocker/shoal-oss/internal/tserver"
	"github.com/phrocker/shoal-oss/internal/walauthority"
	"github.com/phrocker/shoal-oss/internal/zk"
)

type fakeCluster struct {
	mu          sync.Mutex
	cells       map[string][]byte
	ambiguous   bool
	reject      bool
	locateCalls int
}

func newFakeCluster() *fakeCluster {
	return &fakeCluster{cells: map[string][]byte{
		cell(metadata.CFFutureLocation, "abc"):                    []byte("shoal:9997"),
		cell(metadata.CFTabletSection, metadata.CQPrevRow):        {0},
		cell(metadata.CFServer, metadata.CQDirectory):             []byte("t-1"),
		cell(metadata.CFServer, metadata.CQTime):                  []byte("M0"),
		cell(metadata.CFCurrentLocation, "metadata-java-session"): []byte("md:9997"),
	}}
}

func cell(cf, cq string) string { return cf + "\x00" + cq }

func (f *fakeCluster) LocateTable(_ context.Context, tableID string) ([]metadata.TabletInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.locateCalls++
	if tableID == metadata.MetadataTableID {
		return []metadata.TabletInfo{{
			TableID: metadata.MetadataTableID, PrevRowSet: true,
			Location: &metadata.Location{HostPort: "md:9997", Session: "metadata-java-session"},
		}}, nil
	}
	if tableID != "5" {
		return nil, nil
	}
	info := metadata.TabletInfo{
		TableID: "5", PrevRowSet: true, Directory: "t-1",
		Time:       string(f.cells[cell(metadata.CFServer, metadata.CQTime)]),
		ServerLock: string(f.cells[cell(metadata.CFServer, metadata.CQLock)]),
	}
	for key, value := range f.cells {
		cf, cq := splitCell(key)
		switch cf {
		case metadata.CFCurrentLocation:
			if cq != "metadata-java-session" {
				info.Location = &metadata.Location{HostPort: string(value), Session: cq}
			}
		case metadata.CFFutureLocation:
			info.FutureLocation = &metadata.Location{HostPort: string(value), Session: cq}
		case metadata.CFLog:
			entry, err := metadata.DecodeLogEntry([]byte(cq))
			if err != nil {
				return nil, err
			}
			info.Logs = append(info.Logs, entry)
		case metadata.CFFile:
			file, err := metadata.DecodeStoredTabletFile([]byte(cq))
			if err != nil {
				return nil, err
			}
			size, entries, tm, err := metadata.DecodeDataFileValue(value)
			if err != nil {
				return nil, err
			}
			info.Files = append(info.Files, metadata.FileEntry{
				Path: file.Path, StartRow: file.StartRow, EndRow: file.EndRow,
				Size: size, NumEntries: entries, Time: tm, RawQualifier: []byte(cq),
				// Mirrors metadata.AggregateRows: a file entry with no
				// file.embedding column is unknown until the column loop
				// below decodes one for it (issue #274).
				Embedding: embeddingspace.Unknown(),
			})
		}
	}
	for key, value := range f.cells {
		cf, cq := splitCell(key)
		if cf != metadata.CFFileEmbedding {
			continue
		}
		state, err := embeddingspace.Decode(value)
		if err != nil {
			return nil, err
		}
		for i := range info.Files {
			if string(info.Files[i].RawQualifier) == cq {
				info.Files[i].Embedding = state
			}
		}
	}
	return []metadata.TabletInfo{info}, nil
}

func splitCell(key string) (string, string) {
	for index := range key {
		if key[index] == 0 {
			return key[:index], key[index+1:]
		}
	}
	return key, ""
}

func (f *fakeCluster) RootTabletLocation(context.Context) (*zk.Location, error) {
	return &zk.Location{HostPort: "root:9997"}, nil
}

func (f *fakeCluster) ConditionalWrite(
	_ context.Context,
	address, tableID string,
	_ *data.TKeyExtent,
	mutation *data.TConditionalMutation,
) (ingestclient.ConditionalStatus, error) {
	if address != "md:9997" || tableID != metadata.MetadataTableID {
		return ingestclient.ConditionalUnknown, errors.New("wrong conditional destination")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reject {
		return ingestclient.ConditionalRejected, nil
	}
	for _, condition := range mutation.Conditions {
		value, ok := f.cells[cell(string(condition.Cf), string(condition.Cq))]
		if condition.Val == nil {
			if ok {
				return ingestclient.ConditionalRejected, nil
			}
		} else if !ok || string(value) != string(condition.Val) {
			return ingestclient.ConditionalRejected, nil
		}
	}
	decoded, err := cclient.FromThrift(mutation.Mutation)
	if err != nil {
		return ingestclient.ConditionalUnknown, err
	}
	for _, entry := range decoded.Entries() {
		key := cell(string(entry.ColFamily), string(entry.ColQualifier))
		if entry.Deleted {
			delete(f.cells, key)
		} else {
			f.cells[key] = append([]byte(nil), entry.Value...)
		}
	}
	if f.ambiguous {
		f.ambiguous = false
		return ingestclient.ConditionalUnknown, errors.New("lost response")
	}
	return ingestclient.ConditionalAccepted, nil
}

type unusedRoot struct{}

func (unusedRoot) Get(string) ([]byte, *gozk.Stat, error) {
	return nil, nil, errors.New("unused")
}
func (unusedRoot) Set(string, []byte, int32) (*gozk.Stat, error) {
	return nil, errors.New("unused")
}

type fakeRootStore struct {
	mu        sync.Mutex
	data      []byte
	version   int32
	ambiguous bool
}

func (f *fakeRootStore) Get(string) ([]byte, *gozk.Stat, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.data...), &gozk.Stat{Version: f.version}, nil
}

func (f *fakeRootStore) Set(_ string, value []byte, version int32) (*gozk.Stat, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if version != f.version {
		return nil, gozk.ErrBadVersion
	}
	f.data = append([]byte(nil), value...)
	f.version++
	if f.ambiguous {
		f.ambiguous = false
		return nil, errors.New("lost ZooKeeper response")
	}
	return &gozk.Stat{Version: f.version}, nil
}

func testAuthority(t *testing.T, cluster *fakeCluster) (*Authority, ingestrouter.Fence) {
	t.Helper()
	host := tserver.NewHost()
	server := tserver.LockID{UUID: uuid.NewString(), Sequence: 7}
	manager := tserver.LockID{UUID: uuid.NewString(), Sequence: 3}
	if err := host.AdoptLock(server); err != nil {
		t.Fatal(err)
	}
	if err := host.ObserveManagerLock(manager); err != nil {
		t.Fatal(err)
	}
	attempt, err := host.Assign(tserver.Fence{Server: server, Manager: manager},
		tserver.Extent{TableID: "5"})
	if err != nil {
		t.Fatal(err)
	}
	factory, err := NewFactory(Config{
		Reader: cluster, RootLocator: cluster, Conditional: cluster, RootStore: unusedRoot{},
		Host: host, InstancePath: "/accumulo/iid", Address: "shoal:9997",
		Group: tserver.DefaultResourceGroup, Session: "abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	fence := ingestrouter.Fence{
		ServerGeneration: "abc", ManagerGeneration: manager.String(),
		Assignment: attempt.Assignment(),
	}
	opened, err := factory.Open(context.Background(), tabletloader.Specification{
		Extent: tserver.Extent{TableID: "5"}, Generation: "abc",
	}, fence)
	if err != nil {
		t.Fatal(err)
	}
	return opened.(*Authority), fence
}

func TestAuthorityClaimsOwnerAndReconcilesAmbiguousWALMutations(t *testing.T) {
	cluster := newFakeCluster()
	authority, fence := testAuthority(t, cluster)
	ref := walauthority.Reference{
		ID: uuid.NewString(),
	}
	ref.Path = "file:///wal/shoal+9997/" + ref.ID
	ref.Qualifier = "-/" + ref.Path
	cluster.ambiguous = true
	if err := authority.EnsureReference(context.Background(), authority.extent, fence, ref); err != nil {
		t.Fatal(err)
	}
	present, err := authority.HasReference(context.Background(), authority.extent, fence, ref)
	if err != nil || !present {
		t.Fatalf("HasReference = %t, %v", present, err)
	}
	cluster.ambiguous = true
	if err := authority.RemoveReference(context.Background(), authority.extent, fence, ref); err != nil {
		t.Fatal(err)
	}
	present, err = authority.HasReference(context.Background(), authority.extent, fence, ref)
	if err != nil || present {
		t.Fatalf("retired reference = %t, %v", present, err)
	}
	cluster.ambiguous = true
	if err := authority.Release(context.Background(), authority.extent, fence); err != nil {
		t.Fatal(err)
	}
	cluster.mu.Lock()
	_, stillHosted := cluster.cells[cell(metadata.CFCurrentLocation, "abc")]
	cluster.mu.Unlock()
	if stillHosted {
		t.Fatal("ambiguous release left current location")
	}
}

func TestAuthorityConcurrentReferencesAndAmbiguousMincCommit(t *testing.T) {
	cluster := newFakeCluster()
	authority, fence := testAuthority(t, cluster)
	refs := make([]walauthority.Reference, 8)
	var wg sync.WaitGroup
	for index := range refs {
		refs[index].ID = uuid.NewString()
		refs[index].Path = "file:///wal/shoal+9997/" + refs[index].ID
		refs[index].Qualifier = "-/" + refs[index].Path
		wg.Add(1)
		go func(ref walauthority.Reference) {
			defer wg.Done()
			if err := authority.EnsureReference(context.Background(), authority.extent, fence, ref); err != nil {
				t.Errorf("EnsureReference: %v", err)
			}
		}(refs[index])
	}
	wg.Wait()
	got, err := authority.References(context.Background(), authority.extent)
	if err != nil || len(got) != len(refs) {
		t.Fatalf("References = %d, %v", len(got), err)
	}
	cluster.ambiguous = true
	file := mincauthority.DataFile{
		Path: "file:///tables/5/t-1/F0001.rf", Format: "rfile",
		Size: 42, Entries: 8, StartRow: []byte("a"), EndRow: []byte("z"),
		Embedding: embeddingspace.Has("space-a"),
	}
	outcome, err := authority.Commit(context.Background(), mincauthority.MetadataCommit{
		Extent: authority.extent, Fence: fence, File: file, RemoveWALs: refs,
		TabletTime: "M42",
	})
	if outcome != mincauthority.CommitApplied {
		t.Fatalf("Commit = %v, %v", outcome, err)
	}
	state, err := authority.Read(context.Background(), authority.extent)
	if err != nil || len(state.Files) != 1 || len(state.WALs) != 0 || state.Time != "M42" {
		t.Fatalf("state = %#v, %v", state, err)
	}
	if len(state.Files[0].StartRow) != 0 || len(state.Files[0].EndRow) != 0 {
		t.Fatalf("minor compaction installed a fenced file reference: %#v", state.Files[0])
	}
	if state.Files[0].Embedding != embeddingspace.Has("space-a") {
		t.Fatalf("embedding metadata = %+v", state.Files[0].Embedding)
	}
	fileQualifier, _, err := encodeFile(file)
	if err != nil {
		t.Fatal(err)
	}
	cluster.mu.Lock()
	_, fileColumn := cluster.cells[cell(metadata.CFFile, string(fileQualifier))]
	embeddingColumn := cluster.cells[cell(metadata.CFFileEmbedding, string(fileQualifier))]
	cluster.mu.Unlock()
	if !fileColumn || len(embeddingColumn) == 0 {
		t.Fatalf("file and embedding columns were not committed atomically: file=%t embedding=%q", fileColumn, embeddingColumn)
	}
}

func TestAuthorityRejectsStaleOwnerAndConcurrentMetadataChange(t *testing.T) {
	cluster := newFakeCluster()
	authority, fence := testAuthority(t, cluster)
	cluster.mu.Lock()
	delete(cluster.cells, cell(metadata.CFCurrentLocation, "abc"))
	cluster.cells[cell(metadata.CFCurrentLocation, "other")] = []byte("other:9997")
	cluster.mu.Unlock()
	ref := walauthority.Reference{ID: uuid.NewString()}
	ref.Path = "file:///wal/shoal+9997/" + ref.ID
	ref.Qualifier = "-/" + ref.Path
	if err := authority.EnsureReference(context.Background(), authority.extent, fence, ref); !errors.Is(err, ErrStaleOwner) {
		t.Fatalf("stale EnsureReference error = %v", err)
	}
}

func TestRootAuthorityVersionCASRestartAndAmbiguousResponse(t *testing.T) {
	host := tserver.NewHost()
	server := tserver.LockID{UUID: uuid.NewString(), Sequence: 9}
	manager := tserver.LockID{UUID: uuid.NewString(), Sequence: 4}
	if err := host.AdoptLock(server); err != nil {
		t.Fatal(err)
	}
	if err := host.ObserveManagerLock(manager); err != nil {
		t.Fatal(err)
	}
	attempt, err := host.Assign(tserver.Fence{Server: server, Manager: manager},
		tserver.Extent{TableID: metadata.RootTableID})
	if err != nil {
		t.Fatal(err)
	}
	root := &fakeRootStore{data: []byte(`{"version":1,"columnValues":{"future":{"abc":"shoal:9997"},"srv":{"dir":"root_tablet","time":"M0"},"~tab":{"~pr":"\u0000"}}}`)}
	cluster := newFakeCluster()
	factory, err := NewFactory(Config{
		Reader: cluster, RootLocator: cluster, Conditional: cluster, RootStore: root,
		Host: host, InstancePath: "/accumulo/iid", Address: "shoal:9997",
		Group: tserver.DefaultResourceGroup, Session: "abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	fence := ingestrouter.Fence{
		ServerGeneration: "abc", ManagerGeneration: manager.String(),
		Assignment: attempt.Assignment(),
	}
	open := func() *Authority {
		t.Helper()
		opened, err := factory.Open(context.Background(), tabletloader.Specification{
			Extent: tserver.Extent{TableID: metadata.RootTableID}, Generation: "abc",
		}, fence)
		if err != nil {
			t.Fatal(err)
		}
		return opened.(*Authority)
	}
	first := open()
	ref := walauthority.Reference{ID: uuid.NewString()}
	ref.Path = "file:///wal/shoal+9997/" + ref.ID
	ref.Qualifier = "-/" + ref.Path
	root.ambiguous = true
	if err := first.EnsureReference(context.Background(), first.extent, fence, ref); err != nil {
		t.Fatal(err)
	}
	second := open()
	refs, err := second.References(context.Background(), second.extent)
	if err != nil || len(refs) != 1 || refs[0] != ref {
		t.Fatalf("restarted references = %#v, %v", refs, err)
	}
}
