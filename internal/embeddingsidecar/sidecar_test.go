package embeddingsidecar

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/ingestrouter"
	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/storage"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
)

type testVerifier struct {
	stale   bool
	allowed ingestrouter.Extent
	checks  int
	loseAt  int
}

func (v *testVerifier) Verify(_ context.Context, extent ingestrouter.Extent, _ ingestrouter.Fence) error {
	v.checks++
	if v.stale || !equalOptionalBytes(v.allowed.PrevEndRow, extent.PrevEndRow) ||
		!equalOptionalBytes(v.allowed.EndRow, extent.EndRow) ||
		v.allowed.TableID != extent.TableID || (v.loseAt > 0 && v.checks >= v.loseAt) {
		return errors.New("assignment moved")
	}
	return nil
}

type testPublication struct{ confirmed bool }

func (p testPublication) VerifyPublication(context.Context, Record, metadata.TabletInfo) (bool, error) {
	return p.confirmed, nil
}

type cancellingPublication struct{ cancel context.CancelFunc }

func (p cancellingPublication) VerifyPublication(context.Context, Record, metadata.TabletInfo) (bool, error) {
	p.cancel()
	return true, nil
}

type cancellingFence struct {
	extent ingestrouter.Extent
	cancel context.CancelFunc
	checks int
}

func (v *cancellingFence) Verify(_ context.Context, extent ingestrouter.Extent, _ ingestrouter.Fence) error {
	v.checks++
	if !v.extent.Equal(extent) {
		return errors.New("wrong extent")
	}
	if v.checks == 2 {
		v.cancel()
	}
	return nil
}

func sidecarFixture() (Record, metadata.TabletInfo) {
	identity := Identity{
		Extent:    ingestrouter.Extent{TableID: "5", PrevEndRow: []byte("a"), EndRow: []byte("z")},
		FilePath:  "hdfs://nn/accumulo/tables/5/t-1/F1.rf",
		Qualifier: []byte(`{"path":"hdfs://nn/accumulo/tables/5/t-1/F1.rf","startRow":"","endRow":""}`),
		Value:     []byte("100,10"), FooterSHA256: strings.Repeat("a", 64),
	}
	record := Record{
		Version: Version, Identity: identity,
		Fence: ingestrouter.Fence{ServerGeneration: "server-1", ManagerGeneration: "manager-1", Assignment: 1},
		State: embeddingspace.Has("space-1"), Evidence: FooterVerified,
	}
	live := metadata.TabletInfo{
		TableID: "5", PrevRow: []byte("a"), EndRow: []byte("z"), PrevRowSet: true,
		Files: []metadata.FileEntry{{
			Path: identity.FilePath, RawQualifier: identity.Qualifier, RawValue: identity.Value,
		}},
	}
	return record, live
}

func resolve(t *testing.T, store Store, live metadata.TabletInfo, record Record) embeddingspace.FileState {
	t.Helper()
	state, err := store.Resolve(context.Background(), live, record.Identity,
		record.Identity.FooterSHA256, record.State)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestRecordRejectsInvalidUTF8IdentityAndFenceCollisions(t *testing.T) {
	for _, invalid := range []string{string([]byte{0xff}), string([]byte{0xfe})} {
		for _, field := range []string{"table", "path", "server fence", "manager fence"} {
			t.Run(field, func(t *testing.T) {
				record, _ := sidecarFixture()
				switch field {
				case "table":
					record.Identity.Extent.TableID = invalid
				case "path":
					record.Identity.FilePath = invalid
				case "server fence":
					record.Fence.ServerGeneration = invalid
				case "manager fence":
					record.Fence.ManagerGeneration = invalid
				}
				if err := record.validate(); !errors.Is(err, ErrInvalid) {
					t.Fatalf("invalid UTF-8 %s accepted: %v", field, err)
				}
			})
		}
	}
}

func TestResolveExactLiveFileAcrossReplacementAndFailClosedChanges(t *testing.T) {
	ctx := context.Background()
	record, live := sidecarFixture()
	backend := memory.New()
	verifier := &testVerifier{allowed: record.Identity.Extent}
	store := Store{Backend: backend, Prefix: "sidecars", Verifier: verifier,
		Publication: testPublication{confirmed: true}}
	if err := store.Put(ctx, record); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(ctx, record); err != nil {
		t.Fatalf("exact replay: %v", err)
	}
	// A replacement server changes the tablet location/fence, but the
	// original file and its footer remain exact and readable.
	live.Location = &metadata.Location{HostPort: "replacement:9997", Session: "new-lock"}
	if got := resolve(t, store, live, record); got != record.State {
		t.Fatalf("replacement state = %v", got)
	}
	deleted := live
	deleted.Files = nil // GC deleted the file reference; orphan need not be cleaned first.
	if got := resolve(t, store, deleted, record); got.Known() {
		t.Fatalf("deleted file inherited evidence: %v", got)
	}
	split := live
	split.EndRow = []byte("m") // child tablet gets a new exact extent.
	if got := resolve(t, store, split, record); got.Known() {
		t.Fatalf("split child inherited evidence: %v", got)
	}
	replaced := live
	replaced.Files = append([]metadata.FileEntry(nil), live.Files...)
	replaced.Files[0].RawValue = []byte("101,10")
	if got := resolve(t, store, replaced, record); got.Known() {
		t.Fatalf("replaced file entry inherited evidence: %v", got)
	}
	firstTablet := record
	firstTablet.Identity.Extent.PrevEndRow = nil
	firstLive := live
	firstLive.PrevRow = []byte{} // A present empty row differs from nil.
	firstStore := store
	firstStore.Verifier = &testVerifier{allowed: firstTablet.Identity.Extent}
	if err := firstStore.Put(ctx, firstTablet); err != nil {
		t.Fatal(err)
	}
	if got := resolve(t, firstStore, firstLive, firstTablet); got.Known() {
		t.Fatalf("present empty previous row inherited first-tablet evidence: %v", got)
	}
	if got, err := store.Resolve(ctx, live, record.Identity, strings.Repeat("b", 64), record.State); err != nil || got.Known() {
		t.Fatalf("changed footer = %v, %v", got, err)
	}
}

func TestPutRejectsConflictsAndStaleOwner(t *testing.T) {
	ctx := context.Background()
	record, live := sidecarFixture()
	backend := memory.New()
	verifier := &testVerifier{stale: true, allowed: record.Identity.Extent}
	store := Store{Backend: backend, Prefix: "sidecars", Verifier: verifier,
		Publication: testPublication{confirmed: true}}
	if err := store.Put(ctx, record); !errors.Is(err, ErrStale) {
		t.Fatalf("stale owner Put = %v", err)
	}
	if got := resolve(t, store, live, record); got.Known() {
		t.Fatalf("stale owner left promoted evidence: %v", got)
	}
	verifier.stale = false
	crossTablet := record
	crossTablet.Identity.Extent.TableID = "other"
	if err := store.Put(ctx, crossTablet); !errors.Is(err, ErrStale) {
		t.Fatalf("cross-tablet Put = %v", err)
	}
	if err := store.Put(ctx, record); err != nil {
		t.Fatal(err)
	}
	conflict := record
	conflict.State = embeddingspace.Has("different-space")
	if err := store.Put(ctx, conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting state Put = %v", err)
	}
	if got := resolve(t, store, live, record); got != record.State {
		t.Fatalf("conflict changed durable evidence: %v", got)
	}
	bad := record
	bad.State = embeddingspace.NoEmbeddings()
	if err := store.Put(ctx, bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unverified no_embeddings Put = %v", err)
	}
	bad.Evidence = ScanVerified
	if err := bad.validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("scan claim without attestation digest = %v", err)
	}
	bad.AttestationSHA256 = strings.Repeat("c", 64)
	if err := bad.validate(); err != nil {
		t.Fatalf("bounded scan attestation identity = %v", err)
	}
	bad.State = embeddingspace.Has("space-1")
	bad.TrustNoEmbeddings = true
	if err := bad.validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no-vector waiver on present-embedding state = %v", err)
	}
}

func TestAttestedScanMayClassifyUnknownHistoricFooter(t *testing.T) {
	record, live := sidecarFixture()
	record.State = embeddingspace.NoEmbeddings()
	record.Evidence = ScanVerified
	record.AttestationSHA256 = strings.Repeat("c", 64)
	store := Store{Backend: memory.New(), Prefix: "sidecars",
		Verifier:    &testVerifier{allowed: record.Identity.Extent},
		Publication: testPublication{confirmed: true}}
	if err := store.Put(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	state, err := store.Resolve(context.Background(), live, record.Identity,
		record.Identity.FooterSHA256, embeddingspace.Unknown())
	if err != nil || state != record.State {
		t.Fatalf("attested scan over historic unknown footer = %v, %v", state, err)
	}
	state, err = store.Resolve(context.Background(), live, record.Identity,
		record.Identity.FooterSHA256, embeddingspace.Has("contradictory-space"))
	if err != nil || state.Known() {
		t.Fatalf("scan over contradictory known footer = %v, %v", state, err)
	}
	store.Publication = nil
	if got := resolve(t, store, live, record); got.Known() {
		t.Fatalf("scan assertion without durable proof promoted state: %v", got)
	}
}

func TestCallbacksCannotReportSuccessAfterContextCancellation(t *testing.T) {
	record, live := sidecarFixture()
	ctx, cancel := context.WithCancel(context.Background())
	store := Store{Backend: memory.New(), Prefix: "sidecars",
		Verifier: &cancellingFence{extent: record.Identity.Extent, cancel: cancel}}
	if err := store.Put(ctx, record); !errors.Is(err, context.Canceled) {
		t.Fatalf("fence callback canceled context = %v", err)
	}
	readCtx, cancelRead := context.WithCancel(context.Background())
	store.Publication = cancellingPublication{cancel: cancelRead}
	state, err := store.Resolve(readCtx, live, record.Identity,
		record.Identity.FooterSHA256, record.State)
	if !errors.Is(err, context.Canceled) || state.Known() {
		t.Fatalf("publication callback canceled context = %v, %v", state, err)
	}
}

func TestLostFenceAfterCreateNeverActivatesWithoutPublicationProof(t *testing.T) {
	record, live := sidecarFixture()
	store := Store{Backend: memory.New(), Prefix: "sidecars",
		Verifier: &testVerifier{allowed: record.Identity.Extent, loseAt: 2}}
	if err := store.Put(context.Background(), record); !errors.Is(err, ErrStale) {
		t.Fatalf("fence lost after create = %v", err)
	}
	if got := resolve(t, store, live, record); got.Known() {
		t.Fatalf("orphan promoted without publication verifier: %v", got)
	}
}

type lostAnswerBackend struct{ storage.ExclusiveCreator }
type lostAnswerWriter struct{ storage.Writer }

func (w lostAnswerWriter) Close() error {
	if err := w.Writer.Close(); err != nil {
		return err
	}
	return errors.New("answer lost after durable close")
}
func (b lostAnswerBackend) CreateExclusive(ctx context.Context, path string) (storage.Writer, error) {
	w, err := b.ExclusiveCreator.CreateExclusive(ctx, path)
	if err != nil {
		return nil, err
	}
	return lostAnswerWriter{w}, nil
}

func TestAmbiguousCreateReconcilesOnlyExactBytes(t *testing.T) {
	record, live := sidecarFixture()
	backend := memory.New()
	store := Store{Backend: lostAnswerBackend{backend}, Prefix: "sidecars",
		Verifier:    &testVerifier{allowed: record.Identity.Extent},
		Publication: testPublication{confirmed: true}}
	if err := store.Put(context.Background(), record); err != nil {
		t.Fatalf("durable write with lost answer = %v", err)
	}
	if got := resolve(t, store, live, record); got != record.State {
		t.Fatalf("recovered state = %v", got)
	}
	path, err := store.Path(record.Identity)
	if err != nil {
		t.Fatal(err)
	}
	backend.Put(path, []byte("corrupt"))
	if err := store.Put(context.Background(), record); !errors.Is(err, ErrConflict) {
		t.Fatalf("corrupt existing object = %v", err)
	}
	if got := resolve(t, store, live, record); got.Known() {
		t.Fatalf("corrupt sidecar promoted state: %v", got)
	}
	backend.Put(path, bytes.Repeat([]byte("x"), maxRecordBytes+1))
	if err := store.Put(context.Background(), record); !errors.Is(err, ErrConflict) {
		t.Fatalf("oversized existing object = %v", err)
	}
	if got := resolve(t, store, live, record); got.Known() {
		t.Fatalf("oversized sidecar promoted state: %v", got)
	}
	canonical, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	duplicate := strings.Replace(string(canonical), `{"version":1,`, `{"Version":1,"version":1,`, 1)
	backend.Put(path, []byte(duplicate))
	if got := resolve(t, store, live, record); got.Known() {
		t.Fatalf("duplicate JSON key promoted state: %v", got)
	}
}
