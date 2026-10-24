package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/phrocker/shoal-oss/internal/cred"
	"github.com/phrocker/shoal-oss/internal/embeddinginventory"
	"github.com/phrocker/shoal-oss/internal/embeddingsidecar"
	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/storage"
	"github.com/phrocker/shoal-oss/internal/zk"
)

//go:embed inventory.html
var inventoryHTML embed.FS

var inventoryPage = template.Must(template.ParseFS(inventoryHTML, "inventory.html"))

func runEmbeddingInventory(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("embedding-inventory", flag.ContinueOnError)
	flags.SetOutput(stderr)
	zkServers := flags.String("zk", "", "comma-separated ZooKeeper quorum")
	instance := flags.String("instance", "accumulo", "Accumulo instance name")
	table := flags.String("table", "", "table ID")
	scheme := flags.String("storage", "gs", "RFile and sidecar backend: gs, s3, azure, hdfs, memory")
	user := flags.String("user", "root", "principal for metadata reads")
	accVersion := flags.String("accumulo-version", "4.0.0-SNAPSHOT", "server major.minor must match")
	prefix := flags.String("sidecar-prefix", "", "sidecar object prefix")
	namespace := flags.String("sidecar-namespace", "", "sidecar authority namespace")
	keyringFile := flags.String("keyring", "", "owner-only sidecar HMAC keyring")
	listen := flags.String("listen", "", "serve a read-only browser view on a loopback host:port")
	demo := flags.Bool("demo", false, "serve an explicitly marked sample inventory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("embedding-inventory accepts no positional arguments")
	}
	if *demo {
		if *listen == "" {
			return errors.New("--demo requires --listen")
		}
		if *zkServers != "" || *table != "" || *prefix != "" || *namespace != "" || *keyringFile != "" {
			return errors.New("--demo cannot be combined with cluster or sidecar settings")
		}
		return serveEmbeddingInventory(*listen, demoInventory, stdout)
	}
	if *zkServers == "" || *table == "" || *prefix == "" || *namespace == "" || *keyringFile == "" {
		return errors.New("--zk, --table, --sidecar-prefix, --sidecar-namespace and --keyring are required")
	}
	password := os.Getenv("SHOAL_PASSWORD")
	if password == "" {
		return errors.New("SHOAL_PASSWORD is required")
	}
	keyring, err := embeddingsidecar.LoadHMACKeyring(*keyringFile, *namespace)
	if err != nil {
		return fmt.Errorf("sidecar keyring: %w", err)
	}
	ctx := context.Background()
	locator, err := zk.NewWithAuth(strings.Split(*zkServers, ","), *instance, 30*time.Second, password)
	if err != nil {
		return fmt.Errorf("zookeeper: %w", err)
	}
	defer locator.Close()
	backend, closeBackend, err := openBackfillBackend(ctx, *scheme)
	if err != nil {
		return fmt.Errorf("storage backend: %w", err)
	}
	defer closeBackend()
	creator, createOK := backend.(storage.ExclusiveCreator)
	snapshot, snapshotOK := backend.(storage.SnapshotOpener)
	if !createOK || !snapshotOK {
		return errors.New("storage backend must support immutable sidecar objects and pinned RFile snapshots")
	}
	footer := embeddingsidecar.RFileEvidenceReader{Backend: snapshot}
	store := embeddingsidecar.Store{Backend: creator, Prefix: *prefix}
	store.Publication = embeddingsidecar.PublicationStore{
		Candidates: store, Namespace: *namespace, Footer: footer, Authenticator: keyring,
	}
	reader := metadata.NewWalker(locator,
		cred.NewPasswordCreds(*user, password, locator.InstanceID()), *accVersion)
	config := embeddinginventory.Config{
		TableID: *table, Tablets: reader, Footers: footer, Sidecars: store,
	}
	collect := func(ctx context.Context) (embeddinginventory.Report, error) {
		return embeddinginventory.Collect(ctx, config)
	}
	if *listen != "" {
		return serveEmbeddingInventory(*listen, collect, stdout)
	}
	report, err := collect(ctx)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

type inventoryCollector func(context.Context) (embeddinginventory.Report, error)

func serveEmbeddingInventory(address string, collect inventoryCollector, output io.Writer) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("--listen: %w", err)
	}
	parsed := net.ParseIP(host)
	if parsed == nil || !parsed.IsLoopback() {
		return errors.New("--listen must use a numeric loopback address, such as 127.0.0.1:8080")
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	defer listener.Close()
	actual := listener.Addr().String()
	server := &http.Server{
		Handler: inventoryHandler(actual, collect), ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout: 35 * time.Second,
	}
	fmt.Fprintf(output, "Read-only embedding inventory: http://%s/\n", actual)
	return server.Serve(listener)
}

func inventoryHandler(authority string, collect inventoryCollector) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		if r.Host != authority {
			http.Error(w, "misdirected request", http.StatusMisdirectedRequest)
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path != "/" && r.URL.Path != "/api/inventory" {
			http.NotFound(w, r)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		report, err := collect(ctx)
		if err != nil {
			http.Error(w, "Inventory unavailable: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		if r.URL.Path == "/api/inventory" {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_ = json.NewEncoder(w).Encode(report)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = inventoryPage.Execute(w, report)
	})
}

func demoInventory(context.Context) (embeddinginventory.Report, error) {
	return embeddinginventory.Report{
		Demo: true, TableID: "sample-table", At: time.Now().UTC(),
		Counts: embeddinginventory.Counts{HasEmbeddings: 1, Unknown: 2},
		Files: []embeddinginventory.File{
			{Path: "hdfs://sample/tables/7/F001.rf", Entry: "F001.rf",
				State: embeddingspace.Has("catalog-v2"), FooterHint: embeddingspace.Has("catalog-v2"),
				DryRun: "Verified; no backfill needed.", Extent: "first tablet"},
			{Path: "hdfs://sample/tables/7/F002.rf", Entry: "F002.rf",
				State: embeddingspace.Unknown(), FooterHint: embeddingspace.Has("catalog-v2"),
				UnknownReason: "Sample: no matching, authenticated sidecar publication.",
				DryRun:        "Verify ownership and publication before backfill.", Extent: "first tablet"},
			{Path: "hdfs://sample/tables/7/F003.rf", Entry: "F003.rf",
				State: embeddingspace.Unknown(), FooterHint: embeddingspace.NoEmbeddings(),
				UnknownReason: "Sample: a no-embeddings footer is not verified sidecar evidence.",
				DryRun:        "A complete scan and operator approval are required.", Extent: "second tablet"},
		},
		ApplyAvailable:    false,
		ApplyPrerequisite: "Applying changes requires verified publication for each live file and a complete source scan.",
	}, nil
}
