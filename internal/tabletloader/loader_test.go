package tabletloader

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/tserver"
)

const testGeneration Generation = "/accumulo/i/tservers/default/host:9997/zlock#11111111-1111-1111-1111-111111111111#0000000007$abc"

var testExtent = tserver.Extent{TableID: "5", PrevEndRow: []byte("a"), EndRow: []byte("z")}

type fakeAuthority struct {
	mu      sync.RWMutex
	current Generation
	checks  int
}

func (a *fakeAuthority) Capture(context.Context, tserver.Extent) (Generation, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.current, nil
}

func (a *fakeAuthority) Validate(_ context.Context, _ tserver.Extent, got Generation) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.checks++
	if got != a.current {
		return errors.New("generation changed")
	}
	return nil
}

func (a *fakeAuthority) set(g Generation) {
	a.mu.Lock()
	a.current = g
	a.mu.Unlock()
}

type fakeMetadata struct {
	mu       sync.Mutex
	snapshot MetadataSnapshot
	errs     []error
	reads    atomic.Int32
}

func (f *fakeMetadata) ReadTablet(context.Context, tserver.Extent) (MetadataSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads.Add(1)
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		if err != nil {
			return MetadataSnapshot{}, err
		}
	}
	return f.snapshot, nil
}

type fakeConfig struct {
	snapshot ConfigurationSnapshot
	calls    atomic.Int32
}

func (f *fakeConfig) ReadTableConfiguration(context.Context, string) (ConfigurationSnapshot, error) {
	f.calls.Add(1)
	return f.snapshot, nil
}

type racingFiles struct {
	StrictReferenceResolver
	once      sync.Once
	authority *fakeAuthority
}

func (r *racingFiles) ResolveDataFile(ctx context.Context, table string, entry metadata.FileEntry) (DataFile, error) {
	out, err := r.StrictReferenceResolver.ResolveDataFile(ctx, table, entry)
	r.once.Do(func() { r.authority.set("replacement-generation") })
	return out, err
}

func validMetadata() MetadataSnapshot {
	return MetadataSnapshot{
		Revision:   "mzxid:44",
		Generation: testGeneration,
		Tablet: metadata.TabletInfo{
			TableID: "5", PrevRow: []byte("a"), PrevRowSet: true, EndRow: []byte("z"),
			Directory: "t-00001", Time: "M17",
			FutureLocation: &metadata.Location{HostPort: "host:9997", Session: string(testGeneration)},
			Files: []metadata.FileEntry{
				{Path: "hdfs://nn/tables/5/z.rf", Size: 20, NumEntries: 2, Time: -1, RawQualifier: []byte("z")},
				{Path: "hdfs://nn/tables/5/a.rf", Size: 10, NumEntries: 1, Time: 4, RawQualifier: []byte("a")},
			},
			Logs: []metadata.LogEntry{
				{UUID: "22222222-2222-2222-2222-222222222222", Path: "file:///wal/b+2/22222222-2222-2222-2222-222222222222", WALPath: "file:///wal/b+2/22222222-2222-2222-2222-222222222222", Server: "b:2", RawQualifier: []byte("b")},
				{UUID: "11111111-1111-1111-1111-111111111111", Path: "file:///wal/a+1/11111111-1111-1111-1111-111111111111", WALPath: "file:///wal/a+1/11111111-1111-1111-1111-111111111111", Server: "a:1", RawQualifier: []byte("a")},
			},
		},
	}
}

func validConfig() ConfigurationSnapshot {
	return ConfigurationSnapshot{
		TableID: "5", Generation: 9,
		Properties: map[string]string{"table.file.type": "rf", "table.bloom.enabled": "false"},
	}
}

func newTestLoader(t *testing.T, authority *fakeAuthority, source MetadataSource, files FileResolver) *Loader {
	t.Helper()
	if files == nil {
		files = StrictReferenceResolver{}
	}
	loader, err := New(Config{
		Authority: authority,
		Metadata:  source,
		Config:    &fakeConfig{snapshot: validConfig()},
		Files:     files,
		Logs:      StrictReferenceResolver{},
		Retry:     RetryPolicy{MaxAttempts: 3, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	return loader
}

func TestLoadBuildsDeterministicSpecificationFromFakeMetadata(t *testing.T) {
	auth := &fakeAuthority{current: testGeneration}
	source := &fakeMetadata{snapshot: validMetadata()}
	loader := newTestLoader(t, auth, source, nil)

	first, err := loader.Load(context.Background(), testExtent)
	if err != nil {
		t.Fatal(err)
	}
	second, err := loader.Load(context.Background(), testExtent)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Equal(second) {
		t.Fatalf("repeated loads differ:\n%+v\n%+v", first, second)
	}
	if got := first.Properties[0].Name; got != "table.bloom.enabled" {
		t.Fatalf("first property = %q", got)
	}
	if got := first.Files[0].Path; got != "hdfs://nn/tables/5/a.rf" {
		t.Fatalf("first file = %q", got)
	}
	if got := first.Logs[0].UUID; got != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("first WAL = %q", got)
	}

	source.snapshot.Tablet.Files[0].Path = "mutated"
	source.snapshot.Tablet.Logs[0].Peers = []string{"mutated"}
	if first.Files[1].Path == "mutated" || len(first.Logs[1].Peers) != 0 {
		t.Fatal("specification aliases metadata source buffers")
	}
}

func TestLoadRetriesWholeTransactionOnTemporaryMetadataFailure(t *testing.T) {
	auth := &fakeAuthority{current: testGeneration}
	source := &fakeMetadata{
		snapshot: validMetadata(),
		errs:     []error{Retryable(errors.New("metadata transport reset"))},
	}
	loader := newTestLoader(t, auth, source, nil)
	if _, err := loader.Load(context.Background(), testExtent); err != nil {
		t.Fatal(err)
	}
	if source.reads.Load() != 2 {
		t.Fatalf("metadata reads = %d, want 2", source.reads.Load())
	}
}

func TestLoadFailsClosedWhenGenerationChangesDuringFileResolution(t *testing.T) {
	auth := &fakeAuthority{current: testGeneration}
	source := &fakeMetadata{snapshot: validMetadata()}
	loader := newTestLoader(t, auth, source, &racingFiles{authority: auth})

	_, err := loader.Load(context.Background(), testExtent)
	if !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("Load error = %v, want ErrStaleGeneration", err)
	}
}

func TestLoadCancellationStopsRetryBackoff(t *testing.T) {
	auth := &fakeAuthority{current: testGeneration}
	source := &fakeMetadata{
		snapshot: validMetadata(),
		errs:     []error{Retryable(errors.New("unavailable")), Retryable(errors.New("unavailable"))},
	}
	loader, err := New(Config{
		Authority: auth, Metadata: source,
		Config: &fakeConfig{snapshot: validConfig()},
		Files:  StrictReferenceResolver{}, Logs: StrictReferenceResolver{},
		Retry: RetryPolicy{MaxAttempts: 10, InitialBackoff: time.Hour, MaxBackoff: time.Hour},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := loader.Load(ctx, testExtent)
		done <- err
	}()
	for source.reads.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Load error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Load did not stop after cancellation")
	}
}

func TestLoadRejectsMissingAndCorruptMetadata(t *testing.T) {
	tests := []struct {
		name string
		edit func(*MetadataSnapshot)
		want error
	}{
		{"missing", func(s *MetadataSnapshot) { s.Tablet = metadata.TabletInfo{} }, ErrMissingMetadata},
		{"wrong table", func(s *MetadataSnapshot) { s.Tablet.TableID = "6" }, ErrCorruptMetadata},
		{"missing prev row", func(s *MetadataSnapshot) { s.Tablet.PrevRowSet = false }, ErrCorruptMetadata},
		{"missing directory", func(s *MetadataSnapshot) { s.Tablet.Directory = "" }, ErrCorruptMetadata},
		{"wrong generation", func(s *MetadataSnapshot) { s.Generation = "old" }, ErrStaleGeneration},
		{"wrong location generation", func(s *MetadataSnapshot) { s.Tablet.FutureLocation.Session = "old" }, ErrStaleGeneration},
		{"current and future", func(s *MetadataSnapshot) {
			s.Tablet.Location = &metadata.Location{HostPort: "old:9997", Session: "old"}
		}, ErrCorruptMetadata},
		{"duplicate file", func(s *MetadataSnapshot) { s.Tablet.Files = append(s.Tablet.Files, s.Tablet.Files[0]) }, ErrCorruptMetadata},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := validMetadata()
			tc.edit(&snapshot)
			auth := &fakeAuthority{current: testGeneration}
			loader := newTestLoader(t, auth, &fakeMetadata{snapshot: snapshot}, nil)
			_, err := loader.Load(context.Background(), testExtent)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Load error = %v, want %v", err, tc.want)
			}
		})
	}
}
