package metadatacas

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/phrocker/shoal-oss/internal/embeddingsidecar"
	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/mincauthority"
	"github.com/phrocker/shoal-oss/internal/rfile"
	"github.com/phrocker/shoal-oss/internal/storage"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
	"github.com/phrocker/shoal-oss/internal/walauthority"
)

type markerFailureStore struct {
	storage.ExclusiveCreator
	fail bool
}

func (s *markerFailureStore) CreateExclusive(ctx context.Context, path string) (storage.Writer, error) {
	if s.fail && strings.HasSuffix(path, ".published") {
		s.fail = false
		return nil, fmt.Errorf("lost marker create")
	}
	return s.ExclusiveCreator.CreateExclusive(ctx, path)
}

func sidecarRFile(t *testing.T, state embeddingspace.FileState, value string) []byte {
	t.Helper()
	var output bytes.Buffer
	w, err := rfile.NewWriter(&output, rfile.WriterOptions{EmbeddingSpace: state})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Append(&rfile.Key{Row: []byte("r"), ColumnFamily: []byte("cf"), Timestamp: 1}, []byte(value)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func hostedSidecarTest(t *testing.T) (*Authority, *fakeCluster, *memory.Backend, *markerFailureStore, mincauthority.MetadataCommit) {
	t.Helper()
	cluster := newFakeCluster()
	authority, fence := testAuthority(t, cluster)
	backend := memory.New()
	store := &markerFailureStore{ExclusiveCreator: backend}
	keyFile := filepath.Join(t.TempDir(), "keyring.json")
	material := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	encoded := fmt.Sprintf(`{"version":1,"namespace":"test-instance","current":"k1","keys":[{"id":"k1","material_base64":"%s"}]}`, material)
	if err := os.WriteFile(keyFile, []byte(encoded), 0o600); err != nil {
		t.Fatal(err)
	}
	keys, err := embeddingsidecar.LoadHMACKeyring(keyFile, "test-instance")
	if err != nil {
		t.Fatal(err)
	}
	authority.factory.cfg.Sidecar = &SidecarConfig{
		Backend: store, Source: backend, Prefix: "sidecar", Namespace: "test-instance", Authenticator: keys,
	}
	state := embeddingspace.Has("space-a")
	output := sidecarRFile(t, state, "v")
	opHash := sha256.Sum256([]byte("known-op"))
	filePath := "file:///tables/5/t-1/" + hex.EncodeToString(opHash[:]) + ".rf"
	backend.Put(filePath, output)
	fileHash := sha256.Sum256(output)
	ref := walauthority.Reference{ID: uuid.NewString()}
	ref.Path = "file:///wal/shoal+9997/" + ref.ID
	ref.Qualifier = "-/" + ref.Path
	if err := authority.EnsureReference(context.Background(), authority.extent, fence, ref); err != nil {
		t.Fatal(err)
	}
	request := mincauthority.MetadataCommit{
		OperationID: "known-op", Extent: authority.extent, Fence: fence,
		File: mincauthority.DataFile{Path: filePath, Format: "rfile", Size: int64(len(output)), Entries: 1,
			Checksum: hex.EncodeToString(fileHash[:]), Embedding: state},
		RemoveWALs: []walauthority.Reference{ref}, TabletTime: "M1",
	}
	return authority, cluster, backend, store, request
}

func TestHostedKnownEmbeddingPublishesOnlyAfterMarker(t *testing.T) {
	authority, cluster, _, _, request := hostedSidecarTest(t)
	cluster.ambiguous = true
	outcome, err := authority.Commit(context.Background(), request)
	if outcome != mincauthority.CommitApplied {
		t.Fatalf("ambiguous native CAS reconciled to %v: %v", outcome, err)
	}
	state, err := authority.Read(context.Background(), authority.extent)
	if err != nil || len(state.Files) != 1 || state.Files[0].Embedding != request.File.Embedding || len(state.WALs) != 0 {
		t.Fatalf("resolved state = %+v, %v", state, err)
	}
	qualifier, value, err := encodeFile(request.File)
	if err != nil {
		t.Fatal(err)
	}
	cluster.mu.Lock()
	_, custom := cluster.cells[cell(metadata.CFFileEmbedding, string(qualifier))]
	cluster.cells[cell(metadata.CFFile, string(qualifier))] = []byte("999,1")
	cluster.mu.Unlock()
	if custom {
		t.Fatal("published Shoal-only Accumulo metadata")
	}
	if bytes.Equal(value, []byte("999,1")) {
		t.Fatal("replacement did not change native value")
	}
	state, err = authority.Read(context.Background(), authority.extent)
	if err != nil || len(state.Files) != 1 || state.Files[0].Embedding != embeddingspace.Unknown() {
		t.Fatalf("replaced file retained known sidecar state: %+v, %v", state, err)
	}
}

func TestHostedKnownEmbeddingRetryAfterMarkerFailure(t *testing.T) {
	authority, _, _, store, request := hostedSidecarTest(t)
	store.fail = true
	outcome, err := authority.Commit(context.Background(), request)
	if outcome != mincauthority.CommitUnknown || err == nil {
		t.Fatalf("marker failure = %v, %v", outcome, err)
	}
	state, err := authority.Read(context.Background(), authority.extent)
	if err != nil || len(state.Files) != 1 || state.Files[0].Embedding != embeddingspace.Unknown() {
		t.Fatalf("unpublished native file promoted: %+v, %v", state, err)
	}
	outcome, err = authority.Commit(context.Background(), request)
	if outcome != mincauthority.CommitApplied {
		t.Fatalf("same operation retry = %v, %v", outcome, err)
	}
	state, err = authority.Read(context.Background(), authority.extent)
	if err != nil || len(state.Files) != 1 || state.Files[0].Embedding != request.File.Embedding {
		t.Fatalf("retried publication = %+v, %v", state, err)
	}
}

func TestHostedKnownEmbeddingRefusesSameFooterReplacement(t *testing.T) {
	authority, _, backend, _, request := hostedSidecarTest(t)
	if outcome, err := authority.Commit(context.Background(), request); outcome != mincauthority.CommitApplied {
		t.Fatalf("initial commit = %v, %v", outcome, err)
	}
	backend.Put(request.File.Path, sidecarRFile(t, request.File.Embedding, "different cell"))
	state, err := authority.Read(context.Background(), authority.extent)
	if err != nil || len(state.Files) != 1 || state.Files[0].Embedding != embeddingspace.Unknown() {
		t.Fatalf("same-footer replacement inherited state: %+v, %v", state, err)
	}
}

func TestHostedKnownEmbeddingChecksSourceBeforeNativeCAS(t *testing.T) {
	authority, cluster, backend, _, request := hostedSidecarTest(t)
	backend.Put(request.File.Path, sidecarRFile(t, embeddingspace.Unknown(), "v"))
	outcome, err := authority.Commit(context.Background(), request)
	if outcome != mincauthority.CommitRejected || err == nil {
		t.Fatalf("mismatched source = %v, %v", outcome, err)
	}
	qualifier, _, err := encodeFile(request.File)
	if err != nil {
		t.Fatal(err)
	}
	cluster.mu.Lock()
	_, fileCommitted := cluster.cells[cell(metadata.CFFile, string(qualifier))]
	_, walPresent := cluster.cells[cell(metadata.CFLog, request.RemoveWALs[0].Qualifier)]
	cluster.mu.Unlock()
	if fileCommitted || !walPresent {
		t.Fatalf("source preflight changed native metadata: file=%t wal=%t", fileCommitted, walPresent)
	}
}

func TestHostedNoEmbeddingsStillRequiresScanProof(t *testing.T) {
	authority, cluster, _, _, request := hostedSidecarTest(t)
	request.File.Embedding = embeddingspace.NoEmbeddings()
	outcome, err := authority.Commit(context.Background(), request)
	if outcome != mincauthority.CommitRejected || !errors.Is(err, ErrEmbeddingSidecarRequired) {
		t.Fatalf("no_embeddings without scan proof = %v, %v", outcome, err)
	}
	cluster.mu.Lock()
	defer cluster.mu.Unlock()
	if _, exists := cluster.cells[cell(metadata.CFFile, string(mustFileQualifier(t, request.File)))]; exists {
		t.Fatal("no_embeddings committed a native file")
	}
}

func mustFileQualifier(t *testing.T, file mincauthority.DataFile) []byte {
	t.Helper()
	qualifier, _, err := encodeFile(file)
	if err != nil {
		t.Fatal(err)
	}
	return qualifier
}

func TestHostedKnownEmbeddingSplitDoesNotRelabelOldExtent(t *testing.T) {
	authority, cluster, _, _, request := hostedSidecarTest(t)
	if outcome, err := authority.Commit(context.Background(), request); outcome != mincauthority.CommitApplied {
		t.Fatalf("initial commit = %v, %v", outcome, err)
	}
	cluster.mu.Lock()
	cluster.prevRow = []byte("m")
	cluster.mu.Unlock()
	if state, err := authority.Read(context.Background(), authority.extent); err == nil {
		t.Fatalf("split tablet retained old extent state: %+v", state)
	}
}
