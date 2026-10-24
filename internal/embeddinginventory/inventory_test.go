package embeddinginventory

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/embeddingsidecar"
	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/ingestrouter"
	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
)

type inventoryTablets struct {
	first, second []metadata.TabletInfo
	reads         int
}

func (r *inventoryTablets) LocateTable(context.Context, string) ([]metadata.TabletInfo, error) {
	r.reads++
	if r.reads == 1 {
		return r.first, nil
	}
	return r.second, nil
}

type inventoryFooter struct{ state embeddingspace.FileState }

func (r inventoryFooter) ReadFooter(context.Context, string) (string, embeddingspace.FileState, error) {
	return "digest", r.state, nil
}

type inventoryResolver struct{ state embeddingspace.FileState }

func (r inventoryResolver) Resolve(context.Context, metadata.TabletInfo,
	embeddingsidecar.Identity, string, embeddingspace.FileState) (embeddingspace.FileState, error) {
	return r.state, nil
}

func inventoryTablet() metadata.TabletInfo {
	return metadata.TabletInfo{
		TableID: "7", PrevRowSet: true,
		Files: []metadata.FileEntry{{Path: "hdfs://cluster/F.rf",
			RawQualifier: []byte(`{"path":"hdfs://cluster/F.rf"}`),
			RawValue:     []byte("100,2")}},
	}
}

func TestInventoryNeverPromotesFooterWithoutVerifiedSidecar(t *testing.T) {
	tablet := inventoryTablet()
	reader := &inventoryTablets{first: []metadata.TabletInfo{tablet}, second: []metadata.TabletInfo{tablet}}
	report, err := Collect(context.Background(), Config{
		TableID: "7", Tablets: reader,
		Footers:  inventoryFooter{state: embeddingspace.Has("space")},
		Sidecars: inventoryResolver{state: embeddingspace.Unknown()},
		Now:      func() time.Time { return time.Unix(1, 0) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Counts.Unknown != 1 || report.Counts.HasEmbeddings != 0 ||
		report.Files[0].UnknownReason == "" || report.Files[0].FooterHint != embeddingspace.Has("space") ||
		report.ApplyAvailable {
		t.Fatalf("footer was promoted or unknown was hidden: %#v", report)
	}
}

func TestInventoryDemotesResolvedStateWhenFileChangesDuringRead(t *testing.T) {
	first := inventoryTablet()
	changed := inventoryTablet()
	changed.Files[0].RawValue = []byte("101,2")
	reader := &inventoryTablets{first: []metadata.TabletInfo{first}, second: []metadata.TabletInfo{changed}}
	report, err := Collect(context.Background(), Config{
		TableID: "7", Tablets: reader,
		Footers:  inventoryFooter{state: embeddingspace.Has("space")},
		Sidecars: inventoryResolver{state: embeddingspace.Has("space")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Counts.Unknown != 1 || report.Files[0].State.Known() ||
		report.Files[0].UnknownReason == "" {
		t.Fatalf("raced file retained known state: %#v", report)
	}
}

type failingFooter struct{}

func (failingFooter) ReadFooter(context.Context, string) (string, embeddingspace.FileState, error) {
	return "", embeddingspace.FileState{}, errors.New("object unavailable")
}

func TestInventoryNamesUnreadableFileWithoutInvokingResolver(t *testing.T) {
	tablet := inventoryTablet()
	reader := &inventoryTablets{first: []metadata.TabletInfo{tablet}, second: []metadata.TabletInfo{tablet}}
	report, err := Collect(context.Background(), Config{
		TableID: "7", Tablets: reader, Footers: failingFooter{},
		Sidecars: inventoryResolver{state: embeddingspace.Has("space")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Counts.Unknown != 1 || report.Files[0].State.Known() ||
		report.Files[0].UnknownReason == "" {
		t.Fatalf("unreadable footer was promoted: %#v", report)
	}
}

type inventoryFence struct{}

func (inventoryFence) Verify(context.Context, ingestrouter.Extent, ingestrouter.Fence) error {
	return nil
}

type inventoryOwned struct{ live metadata.TabletInfo }

func (r inventoryOwned) ReadOwned(context.Context, ingestrouter.Extent,
	ingestrouter.Fence) (metadata.TabletInfo, error) {
	return r.live, nil
}

type digestFooter struct {
	digest string
	state  embeddingspace.FileState
}

func (f digestFooter) ReadFooter(context.Context, string) (string, embeddingspace.FileState, error) {
	return f.digest, f.state, nil
}

func TestInventoryPromotesOnlyAuthenticatedPublishedSidecar(t *testing.T) {
	ctx := context.Background()
	backend := memory.New()
	path := "hdfs://cluster/tables/7/F1.rf"
	qualifier := []byte(`{"path":"hdfs://cluster/tables/7/F1.rf","startRow":"","endRow":""}`)
	value := []byte("100,2")
	live := metadata.TabletInfo{
		TableID: "7", PrevRowSet: true,
		Files: []metadata.FileEntry{{Path: path, RawQualifier: qualifier, RawValue: value}},
	}
	footer := digestFooter{digest: strings.Repeat("a", 64), state: embeddingspace.Has("space")}
	keyFile := filepath.Join(t.TempDir(), "keyring.json")
	key := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))
	keyDocument := fmt.Sprintf(`{"version":1,"namespace":"cluster-a","current":"key-1","keys":[{"id":"key-1","material_base64":%q}]}`, key)
	if err := os.WriteFile(keyFile, []byte(keyDocument), 0o600); err != nil {
		t.Fatal(err)
	}
	auth, err := embeddingsidecar.LoadHMACKeyring(keyFile, "cluster-a")
	if err != nil {
		t.Fatal(err)
	}
	store := embeddingsidecar.Store{Backend: backend, Prefix: "sidecars", Verifier: inventoryFence{}}
	publication := embeddingsidecar.PublicationStore{
		Candidates: store, Namespace: "cluster-a", Owned: inventoryOwned{live: live},
		Footer: footer, Authenticator: auth,
	}
	identity := embeddingsidecar.Identity{
		Extent: ingestrouter.Extent{TableID: "7"}, FilePath: path,
		Qualifier: qualifier, Value: value, FooterSHA256: footer.digest,
	}
	record := embeddingsidecar.Record{
		Version: embeddingsidecar.Version, Identity: identity,
		Fence: ingestrouter.Fence{
			ServerGeneration: "server-1", ManagerGeneration: "manager-1", Assignment: 1,
		},
		State: footer.state, Evidence: embeddingsidecar.FooterVerified,
	}
	if err := store.Put(ctx, record); err != nil {
		t.Fatal(err)
	}
	reader := &inventoryTablets{first: []metadata.TabletInfo{live}, second: []metadata.TabletInfo{live}}
	store.Publication = publication
	config := Config{TableID: "7", Tablets: reader, Footers: footer, Sidecars: store}
	before, err := Collect(ctx, config)
	if err != nil || before.Counts.Unknown != 1 {
		t.Fatalf("unpublished candidate was trusted: %#v, %v", before, err)
	}
	if err := publication.Publish(ctx, record, "backfill-operation"); err != nil {
		t.Fatal(err)
	}
	reader.reads = 0
	after, err := Collect(ctx, config)
	if err != nil || after.Counts.HasEmbeddings != 1 || after.Counts.Unknown != 0 {
		t.Fatalf("authenticated publication was not recognized: %#v, %v", after, err)
	}
}
