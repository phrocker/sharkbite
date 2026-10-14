package promotion

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	s3sdk "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/phrocker/shoal-oss/accumulo"
	"github.com/phrocker/shoal-oss/internal/engine"
	shstorage "github.com/phrocker/shoal-oss/internal/storage"
	"github.com/phrocker/shoal-oss/internal/storage/azure"
	"github.com/phrocker/shoal-oss/internal/storage/diskcache"
	"github.com/phrocker/shoal-oss/internal/storage/gcs"
	"github.com/phrocker/shoal-oss/internal/storage/hdfs"
	"github.com/phrocker/shoal-oss/internal/storage/local"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
	"github.com/phrocker/shoal-oss/internal/storage/s3"
)

type schemeAwareBackend struct {
	shstorage.Backend
	schemes []string
}

func (b schemeAwareBackend) BackendPathSchemes() []string {
	return b.schemes
}

// readOnlyBackend wraps a storage.Backend but implements only Open,
// deliberately not Create, so it satisfies storage.Backend but fails a
// storage.WritableBackend type-assertion -- mirroring
// internal/storage/storage_test.go's own type of the same name (kept
// separate here since test doubles are unexported and this package
// cannot import that one's).
type readOnlyBackend struct{ inner shstorage.Backend }

func (r readOnlyBackend) Open(ctx context.Context, path string) (shstorage.File, error) {
	return r.inner.Open(ctx, path)
}

type blockingOpenBackend struct {
	*memory.Backend
	once    sync.Once
	started chan struct{}
	release chan struct{}
}

func (b *blockingOpenBackend) Open(ctx context.Context, path string) (shstorage.File, error) {
	b.once.Do(func() {
		close(b.started)
		select {
		case <-b.release:
		case <-ctx.Done():
		}
	})
	return b.Backend.Open(ctx, path)
}

// TestStageBulkDirRejectsReadOnlyDestinationBeforeAnyRead proves
// validateDestinationWritable runs before StageBulkDir ever opens a
// single source file: the manifest here references src paths that do
// not exist at all, so if the writability check ran after (or was
// skipped and the read-only failure only surfaced later, inside
// storage.Copy) this test would instead observe a "source not found"
// style error from engine.VerifyRFileExport, not shstorage.ErrReadOnly.
func TestStageBulkDirRejectsReadOnlyDestinationBeforeAnyRead(t *testing.T) {
	src := memory.New() // deliberately empty: no RFile referenced below exists.
	manifest := &engine.RFileExportManifest{
		Version:     engine.RFileExportManifestVersion,
		SourceTable: "events",
		Tablets:     []engine.RFileExportTablet{{Index: 0}},
		RFiles: []engine.RFileExportFile{
			{TabletIndex: 0, DestinationPath: "export/events/t-0000/F0001.rf", Size: 4, SHA256: "3a6eb0790f39ac87c94f3856b2dd2c5d110e6811602261a9a923d3bb23adc8b7"},
		},
	}
	dst := readOnlyBackend{inner: memory.New()}
	if _, err := StageBulkDir(context.Background(), src, manifest, dst, "hdfs://nn/bulk/events-1"); !errors.Is(err, shstorage.ErrReadOnly) {
		t.Fatalf("StageBulkDir with a read-only destination = %v, want %v", err, shstorage.ErrReadOnly)
	}
}

func TestStageBulkDirFlattensCopiesAndWritesLoadMapping(t *testing.T) {
	src := memory.New()
	data1, file1 := testRFile(t, 0, "export/events/t-0000/F0001.rf", []byte("tablet0-file1"))
	data2, file2 := testRFile(t, 0, "export/events/t-0000/F0002.rf", []byte("tablet0-file2"))
	src.Put(file1.DestinationPath, data1)
	src.Put(file2.DestinationPath, data2)

	manifest := &engine.RFileExportManifest{
		Version:     engine.RFileExportManifestVersion,
		SourceTable: "events",
		Tablets:     []engine.RFileExportTablet{{Index: 0}},
		RFiles:      []engine.RFileExportFile{file1, file2},
	}

	dst := memory.New()
	ctx := context.Background()
	mapping, err := StageBulkDir(ctx, src, manifest, dst, "hdfs://nn/bulk/events-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(mapping) != 1 {
		t.Fatalf("mapping entries = %d, want 1", len(mapping))
	}
	if mapping[0].Tablet.EndRow != nil || mapping[0].Tablet.PrevEndRow != nil {
		t.Fatalf("mapping tablet = %#v, want fully unbounded single tablet", mapping[0].Tablet)
	}

	for _, want := range []string{
		"hdfs://nn/bulk/events-1/F0001.rf",
		"hdfs://nn/bulk/events-1/F0002.rf",
		"hdfs://nn/bulk/events-1/loadmap.json",
	} {
		f, err := dst.Open(ctx, want)
		if err != nil {
			t.Fatalf("expected staged path %s: %v", want, err)
		}
		f.Close()
	}

	f, err := dst.Open(ctx, "hdfs://nn/bulk/events-1/F0001.rf")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	buf := make([]byte, f.Size())
	if _, err := f.ReadAt(buf, 0); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(buf, data1) {
		t.Fatalf("staged content differs from source")
	}

	onDisk, err := ReadLoadMapping(ctx, dst, "hdfs://nn/bulk/events-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(onDisk) != len(mapping) {
		t.Fatalf("on-disk mapping entries = %d, want %d", len(onDisk), len(mapping))
	}
}

func TestStageBulkDirSerializesConcurrentWritersForSameDestination(t *testing.T) {
	const (
		srcPath = "export/events/t-0000/F0001.rf"
		bulkDir = "/bulk/events-1"
	)
	makeManifest := func(data []byte) *engine.RFileExportManifest {
		sum := sha256.Sum256(data)
		return &engine.RFileExportManifest{
			Version:     engine.RFileExportManifestVersion,
			SourceTable: "events",
			Tablets:     []engine.RFileExportTablet{{Index: 0}},
			RFiles: []engine.RFileExportFile{{
				TabletIndex:     0,
				DestinationPath: srcPath,
				Size:            int64(len(data)),
				SHA256:          hex.EncodeToString(sum[:]),
			}},
		}
	}

	firstBytes := validRFileBytes(t, []byte("first-stage"))
	secondBytes := validRFileBytes(t, []byte("second-stage"))
	firstMemory := memory.New()
	firstMemory.Put(srcPath, firstBytes)
	first := &blockingOpenBackend{
		Backend: firstMemory,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	second := memory.New()
	second.Put(srcPath, secondBytes)
	dst := memory.New()

	firstDone := make(chan error, 1)
	go func() {
		_, err := StageBulkDir(context.Background(), first, makeManifest(firstBytes), dst, bulkDir)
		firstDone <- err
	}()
	<-first.started

	secondDone := make(chan error, 1)
	go func() {
		_, err := StageBulkDir(context.Background(), second, makeManifest(secondBytes), dst, bulkDir)
		secondDone <- err
	}()
	select {
	case err := <-secondDone:
		t.Fatalf("second StageBulkDir completed while first still owned bulkDir: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(first.release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first StageBulkDir: %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second StageBulkDir: %v", err)
	}
}

func TestStageBulkDirSerializesAliasedLocalDestinationsAcrossBackendInstances(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	absoluteBulkDir := filepath.Join(root, "bulk")
	relativeBulkDir := "bulk"
	const srcPath = "export/events/t-0000/F0001.rf"
	data := validRFileBytes(t, []byte("stage-data"))
	sum := sha256.Sum256(data)
	manifest := &engine.RFileExportManifest{
		Version:     engine.RFileExportManifestVersion,
		SourceTable: "events",
		Tablets:     []engine.RFileExportTablet{{Index: 0}},
		RFiles: []engine.RFileExportFile{{
			TabletIndex:     0,
			DestinationPath: srcPath,
			Size:            int64(len(data)),
			SHA256:          hex.EncodeToString(sum[:]),
		}},
	}

	firstMemory := memory.New()
	firstMemory.Put(srcPath, data)
	first := &blockingOpenBackend{
		Backend: firstMemory,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	second := memory.New()
	second.Put(srcPath, data)

	firstDone := make(chan error, 1)
	go func() {
		_, err := StageBulkDir(context.Background(), first, manifest, local.New(), absoluteBulkDir)
		firstDone <- err
	}()
	<-first.started

	secondDone := make(chan error, 1)
	go func() {
		_, err := StageBulkDir(context.Background(), second, manifest, local.New(), relativeBulkDir)
		secondDone <- err
	}()
	select {
	case err := <-secondDone:
		t.Fatalf("aliased local StageBulkDir completed while first still owned destination: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(first.release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first StageBulkDir: %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second StageBulkDir: %v", err)
	}
}

func TestAcquireStageBulkDirSerializesRelativeHDFSPathsAcrossBackendInstances(t *testing.T) {
	firstBackend := newTestHDFSBackend(t, "hdfs://nn:8020")
	secondBackend := newTestHDFSBackend(t, "hdfs://nn:8020")

	releaseFirst, err := acquireStageBulkDir(context.Background(), firstBackend, "bulk/events-1")
	if err != nil {
		t.Fatal(err)
	}

	secondAcquired := make(chan func(), 1)
	secondErr := make(chan error, 1)
	go func() {
		release, err := acquireStageBulkDir(context.Background(), secondBackend, "./bulk/events-1")
		if err != nil {
			secondErr <- err
			return
		}
		secondAcquired <- release
	}()

	select {
	case err := <-secondErr:
		t.Fatalf("second acquire failed: %v", err)
	case release := <-secondAcquired:
		release()
		t.Fatal("relative HDFS alias acquired while first backend instance still owned destination")
	case <-time.After(100 * time.Millisecond):
	}

	releaseFirst()
	select {
	case err := <-secondErr:
		t.Fatalf("second acquire after release failed: %v", err)
	case release := <-secondAcquired:
		release()
	case <-time.After(time.Second):
		t.Fatal("second relative HDFS acquire did not proceed after release")
	}
}

func TestAcquireStageBulkDirSerializesEquivalentS3DirectorySpellings(t *testing.T) {
	newBackend := func() *s3.Backend {
		backend, err := s3.New(
			context.Background(),
			s3.WithClient(s3sdk.NewFromConfig(aws.Config{Region: "us-east-1"})),
		)
		if err != nil {
			t.Fatal(err)
		}
		return backend
	}

	firstBackend, secondBackend := newBackend(), newBackend()
	releaseFirst, err := acquireStageBulkDir(context.Background(), firstBackend, "s3://bucket/bulk")
	if err != nil {
		t.Fatal(err)
	}

	secondAcquired := make(chan func(), 1)
	go func() {
		release, err := acquireStageBulkDir(context.Background(), secondBackend, "s3://bucket/bulk/")
		if err != nil {
			return
		}
		secondAcquired <- release
	}()
	select {
	case release := <-secondAcquired:
		release()
		t.Fatal("equivalent S3 directory spelling acquired while first still owned destination")
	case <-time.After(100 * time.Millisecond):
	}

	releaseFirst()
	select {
	case release := <-secondAcquired:
		release()
	case <-time.After(time.Second):
		t.Fatal("equivalent S3 directory spelling did not acquire after release")
	}
}

func TestStageBulkDirRejectsBasenameCollisionBeforeCopying(t *testing.T) {
	src := memory.New()
	src.Put("export/events/t-0000/part-a/F0001.rf", []byte("a"))
	src.Put("export/events/t-0000/part-b/F0001.rf", []byte("b"))

	manifest := &engine.RFileExportManifest{
		Version:     engine.RFileExportManifestVersion,
		SourceTable: "events",
		Tablets:     []engine.RFileExportTablet{{Index: 0}},
		RFiles: []engine.RFileExportFile{
			{TabletIndex: 0, DestinationPath: "export/events/t-0000/part-a/F0001.rf", Size: 1},
			{TabletIndex: 0, DestinationPath: "export/events/t-0000/part-b/F0001.rf", Size: 1},
		},
	}

	dst := memory.New()
	ctx := context.Background()
	if _, err := StageBulkDir(ctx, src, manifest, dst, "/bulk/events-1"); err == nil {
		t.Fatal("StageBulkDir with colliding basenames = nil error, want error")
	}
	if got := dst.Keys(); len(got) != 0 {
		t.Fatalf("StageBulkDir wrote %v on collision error, want no partial writes", got)
	}
}

func TestFlattenNamesAllowsSameDestinationPathTwice(t *testing.T) {
	rfiles := []engine.RFileExportFile{
		{DestinationPath: "export/events/t-0000/F0001.rf"},
		{DestinationPath: "export/events/t-0000/F0001.rf"},
	}
	names, err := flattenNames(rfiles)
	if err != nil {
		t.Fatalf("flattenNames with duplicate identical entries = %v, want nil error", err)
	}
	if names["export/events/t-0000/F0001.rf"] != "F0001.rf" {
		t.Fatalf("flattenNames = %v", names)
	}
}

// TestFlattenNamesRejectsNonLeafBasenames covers the review finding that
// filepath.Base collapses a DestinationPath ending in an empty, ".", or
// ".." segment to a non-leaf name ("." or "..") instead of erroring.
// Left unrejected, joinBulkPath would resolve that entry's write target
// to bulkDir itself or to bulkDir's parent directory rather than a new
// file inside bulkDir.
func TestFlattenNamesRejectsNonLeafBasenames(t *testing.T) {
	for _, destinationPath := range []string{
		"",
		".",
		"..",
		"export/events/t-0000/.",
		"export/events/t-0000/..",
	} {
		t.Run(fmt.Sprintf("path=%q", destinationPath), func(t *testing.T) {
			rfiles := []engine.RFileExportFile{{DestinationPath: destinationPath}}
			if _, err := flattenNames(rfiles); err == nil {
				t.Fatalf("flattenNames(%q) = nil error, want error rejecting non-leaf basename", destinationPath)
			}
		})
	}
}

// TestStageBulkDirRejectsNonLeafBasenameBeforeCopying is the end-to-end
// counterpart to TestFlattenNamesRejectsNonLeafBasenames: it confirms
// StageBulkDir itself refuses a manifest entry that flattens to ".." and
// performs no partial writes, rather than letting the copy loop resolve
// the write target to bulkDir's parent directory.
func TestStageBulkDirRejectsNonLeafBasenameBeforeCopying(t *testing.T) {
	src := memory.New()
	content := []byte("payload")
	src.Put("export/events/t-0000/..", content)
	sum := sha256.Sum256(content)

	manifest := &engine.RFileExportManifest{
		Version:     engine.RFileExportManifestVersion,
		SourceTable: "events",
		Tablets:     []engine.RFileExportTablet{{Index: 0}},
		RFiles: []engine.RFileExportFile{
			{TabletIndex: 0, DestinationPath: "export/events/t-0000/..", Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:])},
		},
	}

	dst := memory.New()
	ctx := context.Background()
	if _, err := StageBulkDir(ctx, src, manifest, dst, "/bulk/events-1"); err == nil {
		t.Fatal("StageBulkDir with a \"..\"-flattening basename = nil error, want error")
	}
	if got := dst.Keys(); len(got) != 0 {
		t.Fatalf("StageBulkDir wrote %v on non-leaf basename error, want no partial writes", got)
	}
}

func TestStageBulkDirRejectsCorruptSourceBeforeCopying(t *testing.T) {
	src := memory.New()
	src.Put("export/events/t-0000/F0001.rf", []byte("actual bytes on disk"))

	manifest := &engine.RFileExportManifest{
		Version:     engine.RFileExportManifestVersion,
		SourceTable: "events",
		Tablets:     []engine.RFileExportTablet{{Index: 0}},
		RFiles: []engine.RFileExportFile{
			{TabletIndex: 0, DestinationPath: "export/events/t-0000/F0001.rf", Size: 999, SHA256: "0000000000000000000000000000000000000000000000000000000000000000"},
		},
	}
	dst := memory.New()
	ctx := context.Background()
	if _, err := StageBulkDir(ctx, src, manifest, dst, "/bulk/events-1"); err == nil {
		t.Fatal("StageBulkDir with mismatched manifest size/sha256 = nil error, want error")
	}
	if got := dst.Keys(); len(got) != 0 {
		t.Fatalf("StageBulkDir wrote %v on verification failure, want no partial writes", got)
	}
}

func TestStageBulkDirDedupesRepeatedDestinationPathCopy(t *testing.T) {
	src := memory.New()
	data, file := testRFile(t, 0, "export/events/t-0000/F0001.rf", []byte("tablet0-file1"))
	src.Put(file.DestinationPath, data)

	manifest := &engine.RFileExportManifest{
		Version:     engine.RFileExportManifestVersion,
		SourceTable: "events",
		Tablets:     []engine.RFileExportTablet{{Index: 0}},
		RFiles:      []engine.RFileExportFile{file, file},
	}
	dst := memory.New()
	ctx := context.Background()
	mapping, err := StageBulkDir(ctx, src, manifest, dst, "/bulk/events-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(mapping) != 1 || len(mapping[0].Files) != 1 {
		t.Fatalf("mapping = %#v, want a single deduped FileEntry", mapping)
	}
	if got := dst.Keys(); len(got) != 2 {
		t.Fatalf("dst.Keys() = %v, want exactly 2 entries (one staged file + loadmap.json)", got)
	}
}

func TestStageBulkDirDedupesPhysicalSourceAliasesBeforeCopying(t *testing.T) {
	tests := []struct {
		name  string
		alias func(realPath string) (string, error)
	}{
		{
			name: "hardlink alias with same basename",
			alias: func(realPath string) (string, error) {
				aliasDir := filepath.Join(filepath.Dir(filepath.Dir(realPath)), "hardlink-alias")
				if err := os.MkdirAll(aliasDir, 0o755); err != nil {
					return "", err
				}
				aliasPath := filepath.Join(aliasDir, filepath.Base(realPath))
				return aliasPath, os.Link(realPath, aliasPath)
			},
		},
		{
			name: "symlinked parent alias with same basename",
			alias: func(realPath string) (string, error) {
				aliasDir := filepath.Join(filepath.Dir(filepath.Dir(realPath)), "symlink-alias")
				if err := os.Symlink(filepath.Dir(realPath), aliasDir); err != nil {
					return "", err
				}
				return filepath.Join(aliasDir, filepath.Base(realPath)), nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			realDir := filepath.Join(root, "real")
			if err := os.MkdirAll(realDir, 0o755); err != nil {
				t.Fatal(err)
			}
			realPath := filepath.Join(realDir, "F0001.rf")
			content := validRFileBytes(t, []byte("physical source bytes"))
			if err := os.WriteFile(realPath, content, 0o644); err != nil {
				t.Fatal(err)
			}
			aliasPath, err := tt.alias(realPath)
			if err != nil {
				t.Skipf("alias setup not supported in this environment: %v", err)
			}

			manifest := localManifestFromFiles(t, realPath, aliasPath)
			dst := memory.New()
			mapping, err := StageBulkDir(context.Background(), local.New(), manifest, dst, "/bulk/events-1")
			if err != nil {
				t.Fatalf("StageBulkDir with physical source aliases: %v", err)
			}
			if len(mapping) != 1 || len(mapping[0].Files) != 1 {
				t.Fatalf("mapping = %#v, want one deduped staged file", mapping)
			}
			if got := dst.Keys(); len(got) != 2 {
				t.Fatalf("dst.Keys() = %v, want one staged file plus loadmap.json", got)
			}
		})
	}
}

func TestStageBulkDirRejectsPhysicalSourceAliasesWithDifferentBasenames(t *testing.T) {
	root := t.TempDir()
	exportDir := filepath.Join(root, "export")
	if err := os.MkdirAll(exportDir, 0o755); err != nil {
		t.Fatal(err)
	}
	aPath := filepath.Join(exportDir, "A.rf")
	if err := os.WriteFile(aPath, []byte("same physical source"), 0o644); err != nil {
		t.Fatal(err)
	}
	bPath := filepath.Join(exportDir, "B.rf")
	if err := os.Link(aPath, bPath); err != nil {
		t.Skipf("hard links not supported in this environment: %v", err)
	}

	manifest := localManifestFromFiles(t, aPath, bPath)
	dst := memory.New()
	if _, err := StageBulkDir(context.Background(), local.New(), manifest, dst, "/bulk/events-1"); err == nil {
		t.Fatal("StageBulkDir with one physical source declared under two basenames = nil error, want error")
	}
	if got := dst.Keys(); len(got) != 0 {
		t.Fatalf("StageBulkDir wrote %v on ambiguous physical source alias, want no writes", got)
	}
}

// TestSourceRefsAliasRequiresExactMatchOnUnrecognizedBackendScheme
// exercises sourceRefsAlias directly: unlike pathsAlias (the
// write-target collision check, where a false positive only causes a
// safe refusal to write), a false positive here causes
// canonicalStageSource to silently drop a distinct manifest entry.
// Two paths on a backend whose scheme none of the built-in
// canonicalizers recognize -- and which isn't local -- must therefore
// only be treated as the same source when they are exactly equal
// strings, never via the trailing-slash-trimmed heuristic that is
// appropriate for the write side.
func TestSourceRefsAliasRequiresExactMatchOnUnrecognizedBackendScheme(t *testing.T) {
	backend := memory.New()
	cache := newPathIdentityCache(0)
	left := newStagePathRef(backend, "custom://bucket/A.rf")
	trailingSlash := newStagePathRef(backend, "custom://bucket/A.rf/")
	if sourceRefsAlias(left, trailingSlash, cache) {
		t.Fatalf("sourceRefsAlias(%q, %q) = true, want false: an unrecognized backend scheme must not merge distinct source keys on a trailing-slash heuristic", left.path, trailingSlash.path)
	}

	exact := newStagePathRef(backend, "custom://bucket/A.rf")
	if !sourceRefsAlias(left, exact, cache) {
		t.Fatalf("sourceRefsAlias(%q, %q) = false, want true: identically-spelled paths on an unrecognized backend scheme are still the same source", left.path, exact.path)
	}
}

// pathsAlias is intentionally more conservative than sourceRefsAlias:
// write-target safety may reject a maybe-aliased unknown-backend path
// pair on spelling alone, but source dedupe must never drop one unless
// the backend's actual semantics confirm the alias.
func TestUnknownBackendWriteSafetyHeuristicsDoNotDriveSourceDedupe(t *testing.T) {
	backend := memory.New()
	cache := newPathIdentityCache(0)
	left := newStagePathRef(backend, "custom://bucket/A.rf")
	right := newStagePathRef(backend, "custom://bucket/A.rf/")

	if !pathsAlias(left, right, cache) {
		t.Fatalf("pathsAlias(%q, %q) = false, want true: write-safety heuristics should conservatively reject maybe-aliased unknown-backend paths", left.path, right.path)
	}
	if sourceRefsAlias(left, right, cache) {
		t.Fatalf("sourceRefsAlias(%q, %q) = true, want false: the same heuristic must not silently dedupe a source", left.path, right.path)
	}
}

// TestStageBulkDirDoesNotSilentlyDedupeDistinctSourcesOnUnrecognizedBackendScheme
// proves the fix for the "custom://bucket/A.rf" vs
// "custom://bucket/A.rf/" scenario end to end. Before the fix,
// sourceRefsAlias's trailing-slash-trimmed fallback treated these two
// manifest entries -- each backed by genuinely different, independently
// verified bytes on an unrecognized backend scheme -- as the same
// source; canonicalStageSource kept only the lexicographically-first
// one and silently dropped the other, so StageBulkDir would have
// succeeded having staged one fewer file than the export actually
// produced. Now that dedupeStageSources correctly keeps both distinct,
// they collide at the destination-flatten step instead (both basenames
// are "A.rf" once the trailing slash is stripped), so StageBulkDir must
// fail closed with an explicit error and write nothing, rather than
// ever silently discarding one source's data.
func TestStageBulkDirDoesNotSilentlyDedupeDistinctSourcesOnUnrecognizedBackendScheme(t *testing.T) {
	aPath := "custom://bucket/A.rf"
	bPath := "custom://bucket/A.rf/"
	aContent := []byte("distinct source A bytes")
	bContent := []byte("distinct source B bytes, must not be lost")

	src := memory.New()
	src.Put(aPath, aContent)
	src.Put(bPath, bContent)

	aSum := sha256.Sum256(aContent)
	bSum := sha256.Sum256(bContent)
	manifest := &engine.RFileExportManifest{
		Version:     engine.RFileExportManifestVersion,
		SourceTable: "events",
		Tablets:     []engine.RFileExportTablet{{Index: 0}},
		RFiles: []engine.RFileExportFile{
			{TabletIndex: 0, DestinationPath: aPath, Size: int64(len(aContent)), SHA256: hex.EncodeToString(aSum[:])},
			{TabletIndex: 0, DestinationPath: bPath, Size: int64(len(bContent)), SHA256: hex.EncodeToString(bSum[:])},
		},
	}

	dst := memory.New()
	if _, err := StageBulkDir(context.Background(), src, manifest, dst, "/bulk/events-1"); err == nil {
		t.Fatal("StageBulkDir with two distinct sources aliased only by a trailing-slash heuristic = nil error, want error")
	}
	if got := dst.Keys(); len(got) != 0 {
		t.Fatalf("StageBulkDir wrote %v before rejecting the ambiguous flatten, want no writes", got)
	}
}

// TestCanonicalBackendPathRequiresMatchingBackendType covers a second,
// distinct gap in the same silent-dedupe bug class as
// TestSourceRefsAliasRequiresExactMatchOnUnrecognizedBackendScheme:
// canonicalBackendPath's scheme-string fallback (reached whenever the
// unwrapped backend isn't a recognized *s3/*gcs/*azure/*hdfs.Backend
// type) applied HDFS's dot-segment path.Clean canonicalization based
// purely on a path *looking* like "hdfs:/..."), with no check that the
// backend actually implements HDFS semantics. On a memory.Backend --
// whose Put/Open/Create/Delete use the literal path string as an
// uninterpreted map key -- "hdfs:/dir/../A.rf" and "hdfs:/A.rf" are two
// genuinely distinct stored objects, but both canonicalized to the
// same "hdfs://A.rf" string, so sourceRefsAlias treated them as the
// same source. The same pair on a *real* hdfs.Backend legitimately does
// alias (HDFS itself resolves the dot segment), so the fix must keep
// that case working while refusing to canonicalize on any backend type
// that isn't actually the corresponding concrete backend.
func TestCanonicalBackendPathRequiresMatchingBackendType(t *testing.T) {
	dotSegmentPath := "hdfs:/dir/../A.rf"
	cleanPath := "hdfs:/A.rf"

	t.Run("unrecognized backend type is not canonicalized from scheme alone", func(t *testing.T) {
		backend := memory.New()
		if _, ok := canonicalBackendPath(newStagePathRef(backend, dotSegmentPath)); ok {
			t.Fatalf("canonicalBackendPath(memory.Backend, %q) ok = true, want false: memory.Backend has no HDFS dot-segment semantics", dotSegmentPath)
		}
		cache := newPathIdentityCache(0)
		left := newStagePathRef(backend, dotSegmentPath)
		right := newStagePathRef(backend, cleanPath)
		if sourceRefsAlias(left, right, cache) {
			t.Fatalf("sourceRefsAlias(%q, %q) on memory.Backend = true, want false: an hdfs-looking scheme on a non-HDFS backend must not be canonicalized", dotSegmentPath, cleanPath)
		}
	})

	t.Run("real hdfs backend still canonicalizes the dot segment", func(t *testing.T) {
		backend := newTestHDFSBackend(t, "hdfs://nn:8020")
		dotSegmentValue, dotSegmentOK := canonicalBackendPath(newStagePathRef(backend, dotSegmentPath))
		cleanValue, cleanOK := canonicalBackendPath(newStagePathRef(backend, cleanPath))
		if !dotSegmentOK || !cleanOK || dotSegmentValue != cleanValue {
			t.Fatalf("canonicalBackendPath on a real hdfs.Backend: (%q, %v) and (%q, %v), want equal canonical values with ok=true", dotSegmentValue, dotSegmentOK, cleanValue, cleanOK)
		}
	})

	t.Run("wrapped hdfs backend still canonicalizes after unwrapping", func(t *testing.T) {
		inner := newTestHDFSBackend(t, "hdfs://nn:8020")
		backend, err := diskcache.New(inner, filepath.Join(t.TempDir(), "cache"), 1<<20)
		if err != nil {
			t.Fatalf("diskcache.New: %v", err)
		}
		dotSegment := newStagePathRef(backend, dotSegmentPath)
		clean := newStagePathRef(backend, cleanPath)
		dotSegmentValue, dotSegmentOK := canonicalBackendPath(dotSegment)
		cleanValue, cleanOK := canonicalBackendPath(clean)
		if !dotSegmentOK || !cleanOK || dotSegmentValue != cleanValue {
			t.Fatalf("canonicalBackendPath on a diskcache-wrapped hdfs.Backend: (%q, %v) and (%q, %v), want equal canonical values with ok=true", dotSegmentValue, dotSegmentOK, cleanValue, cleanOK)
		}
		if !sourceRefsAlias(dotSegment, clean, newPathIdentityCache(0)) {
			t.Fatalf("sourceRefsAlias(%q, %q) on a diskcache-wrapped hdfs.Backend = false, want true after unwrapping the known backend", dotSegment.path, clean.path)
		}
	})
}

// TestStageBulkDirDoesNotSilentlyDedupeDotSegmentSourcesOnUnrecognizedBackendType
// is the end-to-end counterpart of
// TestCanonicalBackendPathRequiresMatchingBackendType: two manifest
// entries with genuinely different, independently verified content on a
// memory.Backend, spelled "hdfs:/dir/../A.rf" and "hdfs:/A.rf". Before
// the fix, canonicalBackendPath's scheme-only fallback canonicalized
// both to the same HDFS-style string purely because the paths looked
// like HDFS URIs, so dedupeStageSources merged them and
// canonicalStageSource silently kept only one, and StageBulkDir would
// have "succeeded" having staged one fewer file than the export
// actually produced. Both paths flatten to the same basename ("A.rf"),
// so once the two sources are correctly kept distinct, StageBulkDir
// must fail closed at the flatten-collision check instead of silently
// dropping either one.
func TestStageBulkDirDoesNotSilentlyDedupeDotSegmentSourcesOnUnrecognizedBackendType(t *testing.T) {
	aPath := "hdfs:/dir/../A.rf"
	bPath := "hdfs:/A.rf"
	aContent := []byte("distinct dot-segment source A bytes")
	bContent := []byte("distinct dot-segment source B bytes, must not be lost")

	src := memory.New()
	src.Put(aPath, aContent)
	src.Put(bPath, bContent)

	aSum := sha256.Sum256(aContent)
	bSum := sha256.Sum256(bContent)
	manifest := &engine.RFileExportManifest{
		Version:     engine.RFileExportManifestVersion,
		SourceTable: "events",
		Tablets:     []engine.RFileExportTablet{{Index: 0}},
		RFiles: []engine.RFileExportFile{
			{TabletIndex: 0, DestinationPath: aPath, Size: int64(len(aContent)), SHA256: hex.EncodeToString(aSum[:])},
			{TabletIndex: 0, DestinationPath: bPath, Size: int64(len(bContent)), SHA256: hex.EncodeToString(bSum[:])},
		},
	}

	dst := memory.New()
	if _, err := StageBulkDir(context.Background(), src, manifest, dst, "/bulk/events-1"); err == nil {
		t.Fatal("StageBulkDir with two dot-segment-distinct sources on a memory.Backend = nil error, want error")
	}
	if got := dst.Keys(); len(got) != 0 {
		t.Fatalf("StageBulkDir wrote %v before rejecting the ambiguous flatten, want no writes", got)
	}
}

func TestStageBulkDirRejectsInvalidBulkDirBeforeCopying(t *testing.T) {
	src := memory.New()
	src.Put("export/events/t-0000/F0001.rf", []byte("data"))
	manifest := &engine.RFileExportManifest{
		Version:     engine.RFileExportManifestVersion,
		SourceTable: "events",
		Tablets:     []engine.RFileExportTablet{{Index: 0}},
		RFiles: []engine.RFileExportFile{
			{TabletIndex: 0, DestinationPath: "export/events/t-0000/F0001.rf", Size: 4, SHA256: "3a6eb0790f39ac87c94f3856b2dd2c5d110e6811602261a9a923d3bb23adc8b7"},
		},
	}

	tests := []struct {
		name    string
		bulkDir string
	}{
		{name: "empty", bulkDir: ""},
		{name: "whitespace padded", bulkDir: " /bulk/events-1 "},
		{name: "local root", bulkDir: "/"},
		{name: "url root", bulkDir: "hdfs://nn/"},
		{name: "uppercase authorityless hdfs root", bulkDir: "HDFS:/"},
		{name: "qualified hdfs dotdot root", bulkDir: "hdfs://nn/tmp/.."},
		{name: "authorityless hdfs dot root", bulkDir: "hdfs:/./"},
		{name: "uppercase authorityless hdfs dotdot root", bulkDir: "HDFS:/tmp/.."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dst := memory.New()
			if _, err := StageBulkDir(context.Background(), src, manifest, dst, tt.bulkDir); !errors.Is(err, accumulo.ErrInvalidBulkDir) {
				t.Fatalf("StageBulkDir error = %v, want ErrInvalidBulkDir", err)
			}
			if got := dst.Keys(); len(got) != 0 {
				t.Fatalf("StageBulkDir wrote %v on invalid bulkDir, want no writes", got)
			}
		})
	}
}

func TestStageBulkDirRejectsUndeclaredTabletIndexBeforeCopying(t *testing.T) {
	src := memory.New()
	src.Put("export/events/t-0000/F0001.rf", []byte("data"))

	manifest := &engine.RFileExportManifest{
		Version:     engine.RFileExportManifestVersion,
		SourceTable: "events",
		Tablets:     []engine.RFileExportTablet{{Index: 0}},
		RFiles: []engine.RFileExportFile{
			{TabletIndex: 1, DestinationPath: "export/events/t-0000/F0001.rf", Size: 4, SHA256: "3a6eb0790f39ac87c94f3856b2dd2c5d110e6811602261a9a923d3bb23adc8b7"},
		},
	}

	dst := memory.New()
	ctx := context.Background()
	if _, err := StageBulkDir(ctx, src, manifest, dst, "/bulk/events-1"); err == nil {
		t.Fatal("StageBulkDir with undeclared tablet index = nil error, want error")
	}
	if got := dst.Keys(); len(got) != 0 {
		t.Fatalf("StageBulkDir wrote %v on undeclared tablet index, want no partial writes", got)
	}
}

func TestStageBulkDirAcceptsMultiTabletManifest(t *testing.T) {
	src := memory.New()
	manifest := twoTabletManifest()
	manifest.RFiles[0].DestinationPath = "events/t-0000/F0001.rf"
	manifest.RFiles[1].DestinationPath = "events/t-0001/F0002.rf"
	populateManifestRFiles(t, src, manifest)

	dst := memory.New()
	ctx := context.Background()
	mapping, err := StageBulkDir(ctx, src, manifest, dst, "hdfs://nn/bulk/events-1")
	if err != nil {
		t.Fatalf("StageBulkDir(multi-tablet manifest) = %v, want success", err)
	}
	if len(mapping) != 2 {
		t.Fatalf("mapping entries = %d, want 2", len(mapping))
	}
	if mapping[0].Tablet.EndRow == nil || string(mapping[0].Tablet.EndRow) != "g" || mapping[0].Tablet.PrevEndRow != nil {
		t.Fatalf("mapping[0].Tablet = %#v, want (nil, %q]", mapping[0].Tablet, "g")
	}
	if mapping[1].Tablet.EndRow != nil || mapping[1].Tablet.PrevEndRow != nil {
		t.Fatalf("mapping[1].Tablet = %#v, want fully unbounded (2-tablet collapse)", mapping[1].Tablet)
	}
	for _, want := range []string{
		"hdfs://nn/bulk/events-1/F0001.rf",
		"hdfs://nn/bulk/events-1/F0002.rf",
		"hdfs://nn/bulk/events-1/loadmap.json",
	} {
		f, err := dst.Open(ctx, want)
		if err != nil {
			t.Fatalf("expected staged path %s: %v", want, err)
		}
		f.Close()
	}
}

// TestStageBulkDirRejectsCrossTabletBasenameCollisionBeforeCopying proves
// the flatten-collision check operates across the whole manifest, not
// per-tablet: two RFiles that belong to *different* tablets but share a
// basename once flattened into the single bulk directory must still be
// rejected before any bytes are copied. flattenNames operates on
// stageManifest.RFiles as a whole (every tablet's files together), so
// this is the same code path as the existing single-tablet collision
// test; this test exists to pin that cross-tablet behavior explicitly
// now that multi-tablet manifests are accepted.
func TestStageBulkDirRejectsCrossTabletBasenameCollisionBeforeCopying(t *testing.T) {
	src := memory.New()
	src.Put("events/t-0000/F0001.rf", []byte("a"))
	src.Put("events/t-0001/F0001.rf", []byte("b"))

	manifest := twoTabletManifest()
	manifest.RFiles[0].DestinationPath = "events/t-0000/F0001.rf"
	manifest.RFiles[1].DestinationPath = "events/t-0001/F0001.rf" // same basename as tablet 0's file

	dst := memory.New()
	ctx := context.Background()
	if _, err := StageBulkDir(ctx, src, manifest, dst, "hdfs://nn/bulk/events-1"); err == nil {
		t.Fatal("StageBulkDir with cross-tablet basename collision = nil error, want error")
	}
	if got := dst.Keys(); len(got) != 0 {
		t.Fatalf("StageBulkDir wrote %v on cross-tablet collision error, want no partial writes", got)
	}
}

// cancelingBackend wraps a *memory.Backend and, the instant its Nth Create
// call (1-indexed, counted across the wrapper's whole lifetime) is
// issued, invokes cancel before delegating to the real Create. storage.Copy
// checks ctx.Err() again immediately after Create returns and before any
// bytes are transferred, so this reproduces "cancellation observed
// mid-operation, after the destination writer was already opened" without
// needing a real slow backend or a race-prone timer.
type cancelingBackend struct {
	*memory.Backend
	cancel   context.CancelFunc
	cancelAt int32
	creates  int32
}

func (b *cancelingBackend) Create(ctx context.Context, path string) (shstorage.Writer, error) {
	n := atomic.AddInt32(&b.creates, 1)
	if n == b.cancelAt {
		b.cancel()
	}
	return b.Backend.Create(ctx, path)
}

// TestStageBulkDirCancellationLeavesNoPartialObjectAndRetrySucceeds covers
// three of the honesty-critical properties for multi-tablet staging under
// interruption: (1) a file whose Create races a mid-copy cancellation is
// aborted rather than left as a partial/corrupt object; (2) StageBulkDir
// is NOT atomic across a multi-file manifest -- an earlier file that had
// already finished copying before the cancellation was observed remains
// staged even though the overall call fails, and no loadmap.json is
// written; (3) retrying the identical call with a fresh, non-cancelled
// context against the SAME destination directory converges to the full,
// correct staged set, because Create replaces any existing object at each
// path and loadmap.json is only written after every file copy succeeds.
// This is deliberately not a claim that StageBulkDir is atomic or
// idempotent in the FATE sense -- only that a caller who retries after an
// interrupted attempt ends up with a complete, correct bulk directory,
// and that a reader who inspects the destination mid-window between the
// failed attempt and the retry can observe a partial (but never
// corrupt-content) set of files.
func TestStageBulkDirCancellationLeavesNoPartialObjectAndRetrySucceeds(t *testing.T) {
	src := memory.New()
	manifest := threeTabletManifest()
	manifest.RFiles[0].DestinationPath = "events/t-0000/F0001.rf"
	manifest.RFiles[1].DestinationPath = "events/t-0001/F0002.rf"
	manifest.RFiles[2].DestinationPath = "events/t-0002/F0003.rf"
	populateManifestRFiles(t, src, manifest)

	realDst := memory.New()
	cancelCtx, cancel := context.WithCancel(context.Background())
	canceling := &cancelingBackend{Backend: realDst, cancel: cancel, cancelAt: 2}

	const bulkDir = "hdfs://nn/bulk/events-1"
	if _, err := StageBulkDir(cancelCtx, src, manifest, canceling, bulkDir); err == nil {
		t.Fatal("StageBulkDir under mid-copy cancellation = nil error, want error")
	} else if !errors.Is(err, context.Canceled) {
		t.Fatalf("StageBulkDir error = %v, want context.Canceled in the chain", err)
	}

	bg := context.Background()
	if f, err := realDst.Open(bg, bulkDir+"/F0001.rf"); err != nil {
		t.Fatalf("F0001.rf (fully copied before cancellation was observed) = %v, want present", err)
	} else {
		f.Close()
	}
	for _, missing := range []string{"/F0002.rf", "/F0003.rf", "/loadmap.json"} {
		if _, err := realDst.Open(bg, bulkDir+missing); err == nil {
			t.Fatalf("%s present after cancelled stage, want absent (no partial/unreached writes)", missing)
		}
	}

	// Retry with a fresh, non-cancelled context against the same
	// destination. The wrapper's cancelAt has already been consumed by
	// the first attempt's second Create call, so this retry's Creates
	// (calls 3-5, or however many the failed attempt reached) never
	// trigger cancel again.
	mapping, err := StageBulkDir(bg, src, manifest, canceling, bulkDir)
	if err != nil {
		t.Fatalf("StageBulkDir retry after cancellation = %v, want success", err)
	}
	if len(mapping) != 3 {
		t.Fatalf("retry mapping entries = %d, want 3", len(mapping))
	}
	for _, name := range []string{"F0001.rf", "F0002.rf", "F0003.rf", "loadmap.json"} {
		f, err := realDst.Open(bg, bulkDir+"/"+name)
		if err != nil {
			t.Fatalf("expected staged path %s after retry: %v", name, err)
		}
		f.Close()
	}
	onDisk, err := ReadLoadMapping(bg, realDst, bulkDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(onDisk) != len(mapping) {
		t.Fatalf("on-disk mapping entries = %d, want %d", len(onDisk), len(mapping))
	}
}

// mutatingSourceBackend wraps a *memory.Backend and, the instant its Nth
// Open call (1-indexed) for one specific tracked path is issued,
// overwrites that path's content in the underlying backend with
// newContent before delegating to the real Open. Used to simulate a
// source object being replaced in the window between StageBulkDir's own
// pre-copy-loop VerifyRFileExport (an earlier Open of the same path) and
// storage.Copy's later, separate Open of that same path inside the copy
// loop, without needing a real concurrent writer or a race-prone timer.
type mutatingSourceBackend struct {
	*memory.Backend
	path       string
	mutateAt   int32
	newContent []byte
	opens      int32
}

func (b *mutatingSourceBackend) Open(ctx context.Context, path string) (shstorage.File, error) {
	if path == b.path {
		if n := atomic.AddInt32(&b.opens, 1); n == b.mutateAt {
			b.Backend.Put(path, b.newContent)
		}
	}
	return b.Backend.Open(ctx, path)
}

// TestStageBulkDirRejectsSourceMutatedBetweenPreflightVerifyAndItsOwnCopy
// covers the copy-time window described in StageBulkDir's own doc
// comment: engine.VerifyRFileExport verifies every RFile once, in a
// single pass, before the copy loop starts; storage.Copy then separately
// re-opens and re-reads each source file when it is actually copied.
// TestPromoteRejectsSourceMutatedDuringAddTableSplits already covers a
// source mutated during the AddTableSplits/ListTableSplits round-trip,
// strictly before this preflight verify even runs; this test instead
// mutates F0001.rf's content on its second Open call -- the first Open
// is the preflight VerifyRFileExport call (which sees and approves the
// original, correct bytes), the second is storage.Copy's own read
// inside the loop (which now sees different bytes than were just
// verified). Without a post-copy verification of the staged destination
// object, this mismatch would go completely undetected and loadmap.json
// would be written describing corrupted data as trustworthy.
func TestStageBulkDirRejectsSourceMutatedBetweenPreflightVerifyAndItsOwnCopy(t *testing.T) {
	const srcPath = "events/t-0000/F0001.rf"
	original := validRFileBytesForRow(t, []byte("a"), []byte("original-bytes"))
	mutated := validRFileBytesForRow(t, []byte("a"), []byte("mutated-bytes!"))
	if len(original) != len(mutated) {
		t.Fatalf("test fixture bug: original (%d) and mutated (%d) must be equal length so storage.Copy's own length bookkeeping cannot itself detect the swap, isolating the destination-verification behavior under test", len(original), len(mutated))
	}

	realSrc := memory.New()
	realSrc.Put(srcPath, original)
	second, secondFile := testRFile(t, 1, "events/t-0001/F0002.rf", []byte("b"))
	realSrc.Put(secondFile.DestinationPath, second)

	originalSum := sha256.Sum256(original)
	manifest := twoTabletManifest()
	manifest.RFiles[0].DestinationPath = srcPath
	manifest.RFiles[0].Size = int64(len(original))
	manifest.RFiles[0].SHA256 = hex.EncodeToString(originalSum[:])
	manifest.RFiles[1].DestinationPath = "events/t-0001/F0002.rf"
	manifest.RFiles[1] = secondFile

	src := &mutatingSourceBackend{Backend: realSrc, path: srcPath, mutateAt: 2, newContent: mutated}
	dst := memory.New()
	ctx := context.Background()

	if _, err := StageBulkDir(ctx, src, manifest, dst, "hdfs://nn/bulk/events-1"); err == nil {
		t.Fatal("StageBulkDir with source mutated between preflight verify and its own copy = nil error, want error")
	}
	if got := atomic.LoadInt32(&src.opens); got < 2 {
		t.Fatalf("source Open calls for %s = %d, want >= 2 (preflight verify + storage.Copy); test fixture did not exercise the intended window", srcPath, got)
	}
	if _, err := dst.Open(ctx, "hdfs://nn/bulk/events-1/loadmap.json"); err == nil {
		t.Fatal("loadmap.json present after rejecting a file mutated during copy, want absent")
	}
	if _, err := dst.Open(ctx, "hdfs://nn/bulk/events-1/F0002.rf"); err == nil {
		t.Fatal("F0002.rf present after F0001.rf failed its post-copy verification, want absent (fails closed before copying later files)")
	}
}

// TestStageBulkDirInvalidatesStaleLoadMappingOnFailedRetry proves the
// round-12-review fix for a bulkDir that already holds a valid
// loadmap.json from an earlier, successful StageBulkDir call. Retrying
// StageBulkDir against that same bulkDir, when the retry's own copy loop
// fails partway (here: F0002.rf's post-copy verifyStagedRFile check,
// simulated the same way
// TestStageBulkDirRejectsSourceMutatedBetweenPreflightVerifyAndItsOwnCopy
// does, via mutatingSourceBackend, just shifted two Opens later to land
// inside the SECOND StageBulkDir call instead of the first), must not
// leave the OLD loadmap.json in place: it no longer reflects the
// bulkDir's actual (now partially overwritten, unverified) contents.
// Without invalidateExistingLoadMapping, this test's final loadmap.json
// would still be present and would still parse as the FIRST, now-stale
// mapping -- silently asserting the directory is complete and correct
// even though F0002.rf's on-disk bytes were just replaced with data that
// failed verification.
func TestStageBulkDirInvalidatesStaleLoadMappingOnFailedRetry(t *testing.T) {
	const srcPath = "events/t-0001/F0002.rf"
	original := validRFileBytes(t, []byte("original-bytes"))
	mutated := validRFileBytes(t, []byte("mutated-bytes!"))
	if len(original) != len(mutated) {
		t.Fatalf("test fixture bug: original (%d) and mutated (%d) must be equal length so storage.Copy's own length bookkeeping cannot itself detect the swap, isolating the destination-verification behavior under test", len(original), len(mutated))
	}

	realSrc := memory.New()
	first, firstFile := testRFileForRow(
		t, 0, "events/t-0000/F0001.rf", []byte("a"), []byte("f0001-bytes"),
	)
	realSrc.Put(firstFile.DestinationPath, first)
	realSrc.Put(srcPath, original)

	originalSum := sha256.Sum256(original)
	manifest := twoTabletManifest()
	manifest.RFiles[0] = firstFile
	manifest.RFiles[1].DestinationPath = srcPath
	manifest.RFiles[1].Size = int64(len(original))
	manifest.RFiles[1].SHA256 = hex.EncodeToString(originalSum[:])

	// mutateAt=4: opens #1 (first call's preflight VerifyRFileExport) and
	// #2 (first call's storage.Copy) both see the original bytes, so the
	// first StageBulkDir call below succeeds cleanly. Open #3 (the
	// retry's own preflight VerifyRFileExport) still sees the original
	// bytes and passes; open #4 (the retry's own storage.Copy) is when
	// the swap fires, so only the retry's post-copy verifyStagedRFile
	// check -- not either call's preflight -- ever observes the mutated
	// bytes.
	src := &mutatingSourceBackend{Backend: realSrc, path: srcPath, mutateAt: 4, newContent: mutated}
	dst := memory.New()
	ctx := context.Background()
	const bulkDir = "hdfs://nn/bulk/events-1"

	firstMapping, err := StageBulkDir(ctx, src, manifest, dst, bulkDir)
	if err != nil {
		t.Fatalf("first StageBulkDir call = %v, want success", err)
	}
	if len(firstMapping) == 0 {
		t.Fatal("first StageBulkDir call returned an empty mapping")
	}
	if _, err := ReadLoadMapping(ctx, dst, bulkDir); err != nil {
		t.Fatalf("loadmap.json after first successful stage: ReadLoadMapping = %v, want success", err)
	}

	if _, err := StageBulkDir(ctx, src, manifest, dst, bulkDir); err == nil {
		t.Fatal("retry StageBulkDir with source mutated during its own copy = nil error, want error")
	}
	if got := atomic.LoadInt32(&src.opens); got < 4 {
		t.Fatalf("source Open calls for %s = %d, want >= 4 (two full preflight+copy passes); test fixture did not exercise the intended retry window", srcPath, got)
	}

	// The crux of the fix: the OLD loadmap.json from the first call must
	// not survive as a stale, still-parseable "complete" marker next to
	// F0002.rf's now-corrupted bytes. memory.Backend implements
	// storage.Remover, so invalidateExistingLoadMapping deletes it
	// outright; loadmap.json must therefore be entirely absent, not
	// merely different.
	if _, err := dst.Open(ctx, bulkDir+"/loadmap.json"); err == nil {
		t.Fatal("loadmap.json present after a failed retry over a previously-staged bulkDir, want absent (stale marker must be invalidated before any RFile is overwritten)")
	} else if !errors.Is(err, shstorage.ErrNotFound) {
		t.Fatalf("loadmap.json open error = %v, want storage.ErrNotFound", err)
	}
}

// nonRemovableBackend wraps a *memory.Backend, delegating Open and Create
// but deliberately not implementing storage.Remover -- even though the
// wrapped *memory.Backend itself does -- to simulate an object-store
// backend (s3/gcs/azure) that exposes no delete capability. Used to
// exercise invalidateExistingLoadMapping's overwrite-with-placeholder
// fallback path.
type nonRemovableBackend struct {
	inner *memory.Backend
}

func (b *nonRemovableBackend) Open(ctx context.Context, path string) (shstorage.File, error) {
	return b.inner.Open(ctx, path)
}

func (b *nonRemovableBackend) Create(ctx context.Context, path string) (shstorage.Writer, error) {
	return b.inner.Create(ctx, path)
}

// TestStageBulkDirOverwritesStaleLoadMappingWithUnparseablePlaceholderWhenBackendCannotDelete
// covers invalidateExistingLoadMapping's fallback for a destination
// backend that cannot delete objects (no storage.Remover, matching
// s3/gcs/azure): a stale loadmap.json from an earlier successful stage
// cannot be removed outright, so it must instead be overwritten with a
// payload that fails to parse as a valid LoadMapping, so no reader can
// mistake it for a still-valid "staging complete" marker.
func TestStageBulkDirOverwritesStaleLoadMappingWithUnparseablePlaceholderWhenBackendCannotDelete(t *testing.T) {
	const srcPath = "events/t-0001/F0002.rf"
	original := validRFileBytes(t, []byte("original-bytes"))
	mutated := validRFileBytes(t, []byte("mutated-bytes!"))

	realSrc := memory.New()
	first, firstFile := testRFileForRow(
		t, 0, "events/t-0000/F0001.rf", []byte("a"), []byte("f0001-bytes"),
	)
	realSrc.Put(firstFile.DestinationPath, first)
	realSrc.Put(srcPath, original)

	originalSum := sha256.Sum256(original)
	manifest := twoTabletManifest()
	manifest.RFiles[0] = firstFile
	manifest.RFiles[1].DestinationPath = srcPath
	manifest.RFiles[1].Size = int64(len(original))
	manifest.RFiles[1].SHA256 = hex.EncodeToString(originalSum[:])

	src := &mutatingSourceBackend{Backend: realSrc, path: srcPath, mutateAt: 4, newContent: mutated}
	dst := &nonRemovableBackend{inner: memory.New()}
	ctx := context.Background()
	const bulkDir = "hdfs://nn/bulk/events-1"

	if _, err := StageBulkDir(ctx, src, manifest, dst, bulkDir); err != nil {
		t.Fatalf("first StageBulkDir call = %v, want success", err)
	}
	if _, err := ReadLoadMapping(ctx, dst, bulkDir); err != nil {
		t.Fatalf("loadmap.json after first successful stage: ReadLoadMapping = %v, want success", err)
	}

	if _, err := StageBulkDir(ctx, src, manifest, dst, bulkDir); err == nil {
		t.Fatal("retry StageBulkDir with source mutated during its own copy = nil error, want error")
	}

	// dst cannot delete, so the stale loadmap.json must still be
	// present -- but overwritten with an unparseable placeholder, never
	// left as the OLD, now-stale-but-well-formed mapping.
	f, err := dst.Open(ctx, bulkDir+"/loadmap.json")
	if err != nil {
		t.Fatalf("loadmap.json open error = %v, want present (overwritten in place, not removed)", err)
	}
	f.Close()
	if _, err := ReadLoadMapping(ctx, dst, bulkDir); err == nil {
		t.Fatal("ReadLoadMapping on invalidated placeholder = nil error, want a parse failure (placeholder must not be mistaken for a valid mapping)")
	}
}

// cancelDuringReadFile is a storage.File whose ReadAt cancels ctx (via a
// stored context.CancelFunc) the moment each call is made, before
// returning its (otherwise normal) chunk of data. size is deliberately
// several 256KB chunks large so a hashing loop that still fails to poll
// ctx.Err() promptly would keep issuing ReadAt calls for every
// remaining chunk instead of stopping after the first one.
type cancelDuringReadFile struct {
	size      int64
	cancel    context.CancelFunc
	readCalls int32
}

func (f *cancelDuringReadFile) ReadAt(p []byte, off int64) (int, error) {
	atomic.AddInt32(&f.readCalls, 1)
	f.cancel()
	for i := range p {
		p[i] = 'x'
	}
	if off+int64(len(p)) >= f.size {
		return len(p), io.EOF
	}
	return len(p), nil
}

func (f *cancelDuringReadFile) Close() error { return nil }
func (f *cancelDuringReadFile) Size() int64  { return f.size }

type cancelDuringReadBackend struct{ file *cancelDuringReadFile }

func (b cancelDuringReadBackend) Open(context.Context, string) (shstorage.File, error) {
	return b.file, nil
}

// openFuncBackend is a storage.Backend whose Open delegates to an
// arbitrary func, letting a test observe (or refuse) an Open call
// without a full backend implementation.
type openFuncBackend func(ctx context.Context, path string) (shstorage.File, error)

func (f openFuncBackend) Open(ctx context.Context, path string) (shstorage.File, error) {
	return f(ctx, path)
}

// TestVerifyStagedRFileStopsPromptlyOnCancellationDuringHash proves a
// round-11 Copilot review fix: verifyStagedRFile's read-and-hash loop
// now polls ctx.Err() before and immediately after every ReadAt, so
// cancellation mid-hash is observed within a single 256KB chunk instead
// of only after the whole (potentially very large) RFile has been read.
// The fake file here is more than 3 chunks large and cancels ctx on its
// very first ReadAt call; if the fix regressed to checking ctx.Err()
// only once (or not polling the loop at all), this test would observe
// readCalls > 1 -- the loop would keep reading every remaining chunk
// before its next (or only) chance to notice cancellation.
func TestVerifyStagedRFileStopsPromptlyOnCancellationDuringHash(t *testing.T) {
	const chunkSize = 256 * 1024
	ctx, cancel := context.WithCancel(context.Background())
	file := &cancelDuringReadFile{size: chunkSize*3 + 100}
	file.cancel = cancel
	dst := cancelDuringReadBackend{file: file}

	err := verifyStagedRFile(ctx, dst, "bulk/F0001.rf", file.size, "irrelevant-sha256")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("verifyStagedRFile error = %v, want context.Canceled in the chain", err)
	}
	if got := atomic.LoadInt32(&file.readCalls); got != 1 {
		t.Fatalf("ReadAt calls = %d, want exactly 1 (cancellation observed after the first 256KB chunk of a %d-byte file, not after reading it all)", got, file.size)
	}
}

// TestVerifyStagedRFileRejectsAlreadyCanceledContextBeforeOpen proves
// verifyStagedRFile checks ctx.Err() before it ever calls dst.Open, so
// an already-canceled context short-circuits before any backend I/O is
// attempted at all, mirroring storage.Copy's own pre-Open poll.
func TestVerifyStagedRFileRejectsAlreadyCanceledContextBeforeOpen(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	opened := false
	dst := openFuncBackend(func(context.Context, string) (shstorage.File, error) {
		opened = true
		return nil, errors.New("dst.Open must not be called once ctx is already canceled")
	})
	if err := verifyStagedRFile(ctx, dst, "bulk/F0001.rf", 4, "irrelevant"); !errors.Is(err, context.Canceled) {
		t.Fatalf("verifyStagedRFile error = %v, want context.Canceled", err)
	}
	if opened {
		t.Fatal("verifyStagedRFile called dst.Open with an already-canceled context, want it to fail before opening")
	}
}

// TestStageBulkDirRejectsInPlaceBulkDirBeforeCopying uses the real local
// filesystem backend (not memory.Backend, which publishes writes
// atomically on Close and so cannot reproduce this hazard) to prove
// StageBulkDir refuses to stage into a bulkDir that aliases the source
// RFile's own path, and — the part that actually matters — that the
// source file on disk survives the rejected call untouched. Without the
// guard, storage.Copy would call local.Backend.Create(dstPath), which
// opens the aliased file with O_TRUNC and destroys it before the copy
// loop ever reads from the (separately opened) source handle.
func TestStageBulkDirRejectsInPlaceBulkDirBeforeCopying(t *testing.T) {
	tabletDir := filepath.Join(t.TempDir(), "export", "events", "t-0000")
	if err := os.MkdirAll(tabletDir, 0o755); err != nil {
		t.Fatal(err)
	}
	srcPath := filepath.Join(tabletDir, "F0001.rf")
	content := []byte("original rfile bytes that must survive a rejected in-place stage")
	if err := os.WriteFile(srcPath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)

	manifest := &engine.RFileExportManifest{
		Version:     engine.RFileExportManifestVersion,
		SourceTable: "events",
		Tablets:     []engine.RFileExportTablet{{Index: 0}},
		RFiles: []engine.RFileExportFile{
			{TabletIndex: 0, DestinationPath: srcPath, Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:])},
		},
	}

	be := local.New()
	ctx := context.Background()
	// bulkDir is the export's own tablet directory: flattening F0001.rf
	// into it resolves to the exact same path the manifest exports from.
	if _, err := StageBulkDir(ctx, be, manifest, be, tabletDir); err == nil {
		t.Fatal("StageBulkDir with bulkDir aliasing the source tablet dir = nil error, want error")
	}

	got, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("source file missing after rejected in-place stage: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("source file corrupted by rejected in-place stage: got %d bytes, want %d bytes intact", len(got), len(content))
	}
}

func TestStageBulkDirRejectsInPlaceBulkDirThroughDiskCacheWrappedLocalSource(t *testing.T) {
	root := t.TempDir()
	tabletDir := filepath.Join(root, "export", "events", "t-0000")
	if err := os.MkdirAll(tabletDir, 0o755); err != nil {
		t.Fatal(err)
	}
	srcPath := filepath.Join(tabletDir, "F0001.rf")
	content := []byte("original rfile bytes that must survive a rejected in-place stage through diskcache")
	if err := os.WriteFile(srcPath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	manifest := &engine.RFileExportManifest{
		Version:     engine.RFileExportManifestVersion,
		SourceTable: "events",
		Tablets:     []engine.RFileExportTablet{{Index: 0}},
		RFiles: []engine.RFileExportFile{
			{TabletIndex: 0, DestinationPath: srcPath, Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:])},
		},
	}

	srcBackend, err := diskcache.New(local.New(), filepath.Join(root, "cache"), 1<<20)
	if err != nil {
		t.Fatalf("diskcache.New: %v", err)
	}
	if _, err := StageBulkDir(context.Background(), srcBackend, manifest, local.New(), tabletDir); err == nil {
		t.Fatal("StageBulkDir with diskcache-wrapped local source aliasing bulkDir = nil error, want error")
	}

	got, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("source file missing after rejected diskcache-wrapped stage: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("source file corrupted by rejected diskcache-wrapped stage: got %d bytes, want %d bytes intact", len(got), len(content))
	}
}

func TestStagePathsAlias(t *testing.T) {
	tests := []struct {
		name string
		src  string
		dst  string
		want bool
	}{
		{name: "identical local paths", src: `C:\data\t-0000\F0001.rf`, dst: `C:\data\t-0000\F0001.rf`, want: true},
		{name: "windows drive path with redundant url-like separators", src: `C://data/F0001.rf`, dst: `C:\data\F0001.rf`, want: true},
		{name: "local paths differing only by separator style", src: `/data/t-0000/F0001.rf`, dst: `/data/t-0000//F0001.rf`, want: true},
		{name: "local paths differing only by unicode normalization", src: `/bulk/events-1/é.rf`, dst: "/bulk/events-1/e\u0301.rf", want: true},
		{name: "distinct local paths", src: `/data/t-0000/F0001.rf`, dst: `/bulk/events-1/F0001.rf`, want: false},
		{name: "identical relative paths", src: "export/events/t-0000/F0001.rf", dst: "export/events/t-0000/F0001.rf", want: true},
		{name: "local paths differing only by case", src: `/bulk/events-1/A.rf`, dst: `/bulk/events-1/a.rf`, want: true},
		{name: "local paths differing only by unicode normalization form", src: "/bulk/events-1/caf\u00e9.rf", dst: "/bulk/events-1/cafe\u0301.rf", want: true},
		{name: "identical url paths", src: "hdfs://nn/export/t-0000/F0001.rf", dst: "hdfs://nn/export/t-0000/F0001.rf", want: true},
		{name: "url paths differing only by trailing slash", src: "hdfs://nn/export/t-0000/F0001.rf", dst: "hdfs://nn/export/t-0000/F0001.rf/", want: true},
		{name: "distinct url paths", src: "hdfs://nn/export/t-0000/F0001.rf", dst: "hdfs://nn/bulk/events-1/F0001.rf", want: false},
		{name: "identical custom url paths", src: "custom+backend://bucket/path/F0001.rf", dst: "custom+backend://bucket/path/F0001.rf", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stagePathsAlias(tt.src, tt.dst); got != tt.want {
				t.Fatalf("stagePathsAlias(%q, %q) = %v, want %v", tt.src, tt.dst, got, tt.want)
			}
		})
	}
}

func TestPathUsesBackendSeparatorJoin(t *testing.T) {
	tests := []struct {
		name string
		path string
		want bool
	}{
		{name: "s3 qualified path", path: "s3://bucket/F0001.rf", want: true},
		{name: "gcs qualified path", path: "gs://bucket/F0001.rf", want: true},
		{name: "azure qualified path", path: "az://container/F0001.rf", want: true},
		{name: "custom scheme qualified path", path: "custom+backend://bucket/F0001.rf", want: true},
		{name: "hdfs qualified path", path: "hdfs://nn/tables/F0001.rf", want: true},
		{name: "hdfs authorityless path", path: "hdfs:/tables/F0001.rf", want: true},
		{name: "uppercase hdfs authorityless path", path: "HDFS:/tables/F0001.rf", want: true},
		{name: "opaque hdfs URI is not a joined backend path", path: "hdfs:tables/F0001.rf", want: false},
		{name: "windows drive path is local", path: `C://data/F0001.rf`, want: false},
		{name: "plain local path is local", path: `C:\data\F0001.rf`, want: false},
		{name: "generic non-builtin backend scheme", path: "memory://bucket/F0001.rf", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pathUsesBackendSeparatorJoin(tt.path); got != tt.want {
				t.Fatalf("pathUsesBackendSeparatorJoin(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestJoinBulkPathTreatsWindowsDrivePathAsLocal(t *testing.T) {
	got := joinBulkPath(local.New(), `C://data`, "F0001.rf")
	if got == `C://data/F0001.rf` {
		t.Fatalf("joinBulkPath treated C://data as a backend URL root: got %q", got)
	}
	if !localPathsLexicallyAlias(got, `C:\data\F0001.rf`) {
		t.Fatalf("joinBulkPath(%q, %q) = %q, want a local Windows-drive path aliasing %q", `C://data`, "F0001.rf", got, `C:\data\F0001.rf`)
	}
}

func TestJoinBulkPathTreatsAuthoritylessHDFSRootCaseInsensitively(t *testing.T) {
	got := joinBulkPath(memory.New(), "HDFS:/bulk", "F0001.rf")
	if got != "HDFS:/bulk/F0001.rf" {
		t.Fatalf("joinBulkPath(HDFS:/bulk) = %q, want %q", got, "HDFS:/bulk/F0001.rf")
	}
}

// TestJoinBulkPathUsesSeparatorJoinForGenericBackendScheme proves
// joinBulkPath joins with a literal "/" for a bulkDir using a backend
// URI scheme it does not know how to canonicalize (contrast the four
// built-in schemes s3/gs/az/hdfs), matching engine's own generic
// joinBackendPath. A filepath.Join fallback here would collapse the
// scheme's "//" (and use OS-native separators), producing a path the
// backend would not recognize as its own root.
func TestJoinBulkPathUsesSeparatorJoinForGenericBackendScheme(t *testing.T) {
	got := joinBulkPath(memory.New(), "memory://bucket", "F0001.rf")
	want := "memory://bucket/F0001.rf"
	if got != want {
		t.Fatalf("joinBulkPath(%q, %q) = %q, want %q", "memory://bucket", "F0001.rf", got, want)
	}
}

func TestJoinBulkPathPreservesCustomSchemeRoot(t *testing.T) {
	got := joinBulkPath(memory.New(), "custom+backend://bucket/prefix", "F0001.rf")
	if got != "custom+backend://bucket/prefix/F0001.rf" {
		t.Fatalf("joinBulkPath(custom scheme) = %q, want %q", got, "custom+backend://bucket/prefix/F0001.rf")
	}
}

func TestPathUsesBackendSeparatorJoinAcceptsDeclaredSingleCharacterScheme(t *testing.T) {
	backend := schemeAwareBackend{Backend: memory.New(), schemes: []string{"x"}}
	if !pathUsesBackendSeparatorJoinOnBackend(backend, "x://bucket/F0001.rf") {
		t.Fatalf("pathUsesBackendSeparatorJoinOnBackend(x scheme backend, x://bucket/F0001.rf) = false, want true")
	}
	if pathUsesBackendSeparatorJoinOnBackend(local.New(), "x://bucket/F0001.rf") {
		t.Fatalf("pathUsesBackendSeparatorJoinOnBackend(local backend, x://bucket/F0001.rf) = true, want false")
	}
}

func TestLocalBackendPathsOverrideBackendSchemeHeuristics(t *testing.T) {
	tests := []struct {
		name string
		be   shstorage.Backend
	}{
		{name: "local backend", be: local.New()},
		{
			name: "diskcache wrapped local backend",
			be: func() shstorage.Backend {
				backend, err := diskcache.New(local.New(), filepath.Join(t.TempDir(), "cache"), 1<<20)
				if err != nil {
					t.Fatalf("diskcache.New: %v", err)
				}
				return backend
			}(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref := stagePathRef{backend: tt.be, path: "hdfs:/bulk/F0001.rf"}
			if !usesLocalFilesystemSemantics(ref) {
				t.Fatalf("usesLocalFilesystemSemantics(%s) = false, want true for a known local backend", ref.path)
			}
			if scheme := explicitBackendSchemeOnBackend(tt.be, ref.path); scheme != "" {
				t.Fatalf("explicitBackendSchemeOnBackend(%s) = %q, want empty for a known local backend", ref.path, scheme)
			}
			if _, ok := canonicalBackendPath(ref); ok {
				t.Fatalf("canonicalBackendPath(%s) unexpectedly treated a known local backend path as remote", ref.path)
			}
			if pathUsesBackendSeparatorJoinOnBackend(tt.be, "hdfs:/bulk") {
				t.Fatalf("pathUsesBackendSeparatorJoinOnBackend(hdfs:/bulk) = true, want false for a known local backend")
			}
		})
	}
}

func TestLocalTargetUsesWindowsADSOnBackend(t *testing.T) {
	tests := []struct {
		name    string
		backend shstorage.Backend
		path    string
		want    bool
	}{
		{name: "drive rooted local path is allowed", backend: local.New(), path: `C:\bulk\A.rf`, want: false},
		{name: "drive relative local path is allowed", backend: local.New(), path: `C:bulk\A.rf`, want: false},
		{name: "double-slash drive path is allowed", backend: local.New(), path: `C://bulk/A.rf`, want: false},
		{name: "default stream short form is rejected", backend: local.New(), path: `C:\bulk\A.rf:$DATA`, want: true},
		{name: "default stream long form is rejected", backend: local.New(), path: `C:\bulk\A.rf::$dAtA`, want: true},
		{name: "named stream is rejected", backend: local.New(), path: `C:\bulk\A.rf:Meta:$DATA`, want: true},
		{name: "intermediate directory stream is rejected", backend: local.New(), path: `C:\bulk:meta\A.rf`, want: true},
		{name: "trailing-dot interaction is rejected", backend: local.New(), path: `C:\bulk\A.rf.::$Data`, want: true},
		{name: "extended-length drive path is allowed", backend: local.New(), path: `\\?\C:\bulk\A.rf`, want: false},
		{name: "extended-length drive path ads is rejected", backend: local.New(), path: `\\?\C:\bulk\A.rf::$DATA`, want: true},
		{name: "extended-length unc path is allowed", backend: local.New(), path: `\\?\UNC\server\share\bulk\A.rf`, want: false},
		{name: "extended-length unc path ads is rejected", backend: local.New(), path: `\\?\UNC\server\share\bulk\A.rf:$DATA`, want: true},
		{name: "remote uri path on s3 backend is not treated as local ads", backend: &s3.Backend{}, path: `s3://bucket/A.rf:$DATA`, want: false},
		{name: "custom uri path on nonlocal backend is not treated as local ads", backend: schemeAwareBackend{Backend: memory.New(), schemes: []string{"custom"}}, path: `custom://bucket/A.rf:meta`, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := localTargetUsesWindowsADSOnBackend(tt.backend, tt.path); got != tt.want {
				t.Fatalf("localTargetUsesWindowsADSOnBackend(%T, %q) = %v, want %v", tt.backend, tt.path, got, tt.want)
			}
		})
	}
}

func TestNormalizeWindowsUNCPublicationPrefix(t *testing.T) {
	tests := []struct {
		name   string
		left   string
		right  string
		wantEq bool
		wantOK bool
	}{
		{name: "standard UNC server and share are case-insensitive", left: `\\SERVER\share\`, right: `\\server\share\`, wantEq: true, wantOK: true},
		{name: "standard UNC server and share normalize unicode", left: "\\\\SERV\u00c9R\\shar\u00e9\\", right: "\\\\serve\u0301r\\share\u0301\\", wantEq: true, wantOK: true},
		{name: "distinct share stays distinct", left: `\\server\share-a\`, right: `\\server\share-b\`, wantEq: false, wantOK: true},
		{name: "distinct server stays distinct", left: `\\server-a\share\`, right: `\\server-b\share\`, wantEq: false, wantOK: true},
		{name: "extended UNC marker and server components normalize case", left: `\\?\UNC\SERVER\share\`, right: `\\?\unc\server\share\`, wantEq: true, wantOK: true},
		{name: "bare extended UNC marker normalizes without inline server or share", left: `\\?\UNC\`, right: `\\?\unc\`, wantEq: true, wantOK: true},
		{name: "bare extended drive prefix normalizes case", left: `\\?\c:\`, right: `\\?\C:\`, wantEq: true, wantOK: true},
		{name: "bare drive-relative prefix normalizes case", left: `c:`, right: `C:`, wantEq: true, wantOK: true},
		{name: "drive-relative and drive-absolute prefixes stay distinct", left: `c:`, right: `C:\`, wantEq: false, wantOK: true},
		{name: "drive-letter prefix is not treated as UNC", left: `C:\bulk\`, right: `C:\bulk\`, wantOK: false},
		{name: "uri prefix is not treated as UNC", left: `s3://bucket`, right: `s3://bucket`, wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			left, leftOK := normalizeWindowsUNCPublicationPrefix(tt.left)
			right, rightOK := normalizeWindowsUNCPublicationPrefix(tt.right)
			if leftOK != tt.wantOK || rightOK != tt.wantOK {
				t.Fatalf("normalizeWindowsUNCPublicationPrefix(%q) ok=%v, normalizeWindowsUNCPublicationPrefix(%q) ok=%v, want %v", tt.left, leftOK, tt.right, rightOK, tt.wantOK)
			}
			if leftOK && rightOK && (left == right) != tt.wantEq {
				t.Fatalf("normalized UNC prefixes %q and %q equality=%v, want %v", left, right, left == right, tt.wantEq)
			}
		})
	}
}

func TestJoinBulkPathPreservesDeclaredSingleCharacterSchemeRoot(t *testing.T) {
	backend := schemeAwareBackend{Backend: memory.New(), schemes: []string{"x"}}
	got := joinBulkPath(backend, "x://bucket/prefix", "F0001.rf")
	if got != "x://bucket/prefix/F0001.rf" {
		t.Fatalf("joinBulkPath(single-char scheme) = %q, want %q", got, "x://bucket/prefix/F0001.rf")
	}
}

func TestIsBackendRootDistinguishesWindowsDrivePaths(t *testing.T) {
	tests := []struct {
		name string
		path string
		want bool
	}{
		{name: "backend root", path: "s3://bucket/", want: true},
		{name: "backend non-root", path: "s3://bucket/path", want: false},
		{name: "custom backend root", path: "custom+backend://bucket/", want: true},
		{name: "custom backend non-root", path: "custom+backend://bucket/path", want: false},
		{name: "hdfs root", path: "hdfs:/", want: true},
		{name: "uppercase hdfs root", path: "HDFS:/", want: true},
		{name: "hdfs bare authority root has no path segment", path: "hdfs://nn", want: true},
		{name: "qualified hdfs dotdot path normalizes to root", path: "hdfs://nn/tmp/..", want: true},
		{name: "lowercase authorityless hdfs dotdot path normalizes to root", path: "hdfs:/tmp/..", want: true},
		{name: "uppercase authorityless hdfs dotdot path normalizes to root", path: "HDFS:/tmp/..", want: true},
		{name: "hdfs non-root path containing a parent dot segment", path: "hdfs://nn/tmp/../keep", want: false},
		{name: "object-store dotdot key is not a root", path: "s3://bucket/tmp/..", want: false},
		{name: "windows drive root with redundant separators", path: `C://`, want: true},
		{name: "windows drive non-root with redundant separators", path: `C://data`, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isBackendRoot(tt.path); got != tt.want {
				t.Fatalf("isBackendRoot(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestIsBackendRootAcceptsDeclaredSingleCharacterScheme(t *testing.T) {
	backend := schemeAwareBackend{Backend: memory.New(), schemes: []string{"x"}}
	if !isBackendRootOnBackend(backend, "x://bucket/") {
		t.Fatalf("isBackendRootOnBackend(x scheme backend, x://bucket/) = false, want true")
	}
	if isBackendRootOnBackend(local.New(), "x://bucket/") {
		t.Fatalf("isBackendRootOnBackend(local backend, x://bucket/) = true, want false")
	}
}

// TestParseDOS83LiteralComponent and TestDOS83AliasFamilyComponent
// exercise dos83PathAliases's underlying component-parsing helpers
// directly. Both helpers are plain string parsers with no
// runtime.GOOS check of their own -- only dos83PathAliases and
// buildLocalPublicationComponentIdentity (its callers) gate short-name
// handling to Windows -- so, unlike the integration-level checks in
// stage_windows_test.go, these run (and give CI real coverage of the
// short-name matching rules) on every platform, which matters because
// CI only runs on ubuntu-latest and therefore never executes any
// //go:build windows test in this package. Inputs are passed through
// normalizeLocalPublicationComponent first, matching how
// buildLocalPublicationComponentIdentity actually feeds these helpers
// in production: both expect an already case-folded component, not an
// arbitrary-case raw string.
func TestParseDOS83LiteralComponent(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		wantPrefix string
		wantExt    string
		wantOK     bool
	}{
		{name: "literal short name with an extension", raw: "LONGFI~1.RF", wantPrefix: "longfi", wantExt: "rf", wantOK: true},
		{name: "literal short name with legal windows punctuation", raw: "LONG$F~1.RF", wantPrefix: "long$f", wantExt: "rf", wantOK: true},
		{name: "literal short name without an extension", raw: "LONGFI~1", wantPrefix: "longfi", wantExt: "", wantOK: true},
		{name: "a plain long name has no tilde and never parses", raw: "Plain.rf", wantOK: false},
		{name: "a leading-zero ordinal is not a valid NTFS short name", raw: "LONGFI~01.RF", wantOK: false},
		{name: "an over-long prefix before the tilde does not parse", raw: "TOOLONGFI~1.RF", wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prefix, ext, ok := parseDOS83LiteralComponent(normalizeLocalPublicationComponent(tt.raw))
			if ok != tt.wantOK || (ok && (prefix != tt.wantPrefix || ext != tt.wantExt)) {
				t.Fatalf("parseDOS83LiteralComponent(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tt.raw, prefix, ext, ok, tt.wantPrefix, tt.wantExt, tt.wantOK)
			}
		})
	}
}

func TestDOS83AliasFamilyComponent(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		wantPrefix string
		wantExt    string
		wantOK     bool
	}{
		{name: "long name derives the six-character alias prefix NTFS would generate", raw: "LongFilename.rf", wantPrefix: "longfi", wantExt: "rf", wantOK: true},
		{name: "windows-valid short-name punctuation is preserved in the alias family", raw: "Long$Filename.rf", wantPrefix: "long$f", wantExt: "rf", wantOK: true},
		{name: "characters outside the conservative 8dot3 set are stripped before truncating the prefix", raw: "My File [v2].rf", wantPrefix: "myfile", wantExt: "rf", wantOK: true},
		{
			// NTFS never generates a distinct short name for a
			// component that already fits 8.3 using only the
			// unambiguously-safe charset, so a plain compliant name
			// can never actually collide via this mechanism and must
			// not be flagged as an alias family.
			name:   "a name that already fits 8.3 gets no distinct alias family",
			raw:    "PLAIN.RF",
			wantOK: false,
		},
		{
			// A tilde is not in the "definitely already compliant"
			// charset isPlainDOS83Component checks, so a literal
			// short-name spelling is conservatively still treated as
			// its own alias family too: nothing here proves NTFS
			// couldn't derive yet another short name for a file
			// actually named "LONGFI~1.RF" verbatim.
			name:       "a literal short-name spelling is conservatively still its own alias family",
			raw:        "LONGFI~1.RF",
			wantPrefix: "longfi",
			wantExt:    "rf",
			wantOK:     true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prefix, ext, ok := dos83AliasFamilyComponent(normalizeLocalPublicationComponent(tt.raw))
			if ok != tt.wantOK || (ok && (prefix != tt.wantPrefix || ext != tt.wantExt)) {
				t.Fatalf("dos83AliasFamilyComponent(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tt.raw, prefix, ext, ok, tt.wantPrefix, tt.wantExt, tt.wantOK)
			}
		})
	}
}

func TestDOS83HelpersSupportNamedWindowsPunctuation(t *testing.T) {
	for _, punct := range []string{"$", "%", "'", "-", "@"} {
		t.Run("literal-"+punct, func(t *testing.T) {
			raw := "LONG" + punct + "F~1.RF"
			want := "long" + punct + "f"
			prefix, ext, ok := parseDOS83LiteralComponent(normalizeLocalPublicationComponent(raw))
			if !ok || prefix != want || ext != "rf" {
				t.Fatalf("parseDOS83LiteralComponent(%q) = (%q, %q, %v), want (%q, %q, true)", raw, prefix, ext, ok, want, "rf")
			}
		})
		t.Run("family-"+punct, func(t *testing.T) {
			raw := "Long" + punct + "Filename.rf"
			want := "long" + punct + "f"
			prefix, ext, ok := dos83AliasFamilyComponent(normalizeLocalPublicationComponent(raw))
			if !ok || prefix != want || ext != "rf" {
				t.Fatalf("dos83AliasFamilyComponent(%q) = (%q, %q, %v), want (%q, %q, true)", raw, prefix, ext, ok, want, "rf")
			}
		})
	}
}

func TestStagePathsAliasBackendCanonicalization(t *testing.T) {
	hdfsBackend := newTestHDFSBackend(t, "hdfs://nn:8020")

	tests := []struct {
		name       string
		srcBackend shstorage.Backend
		srcPath    string
		dstBackend shstorage.Backend
		dstPath    string
		want       bool
	}{
		{
			name:       "s3 scheme-less aliases qualified path",
			srcBackend: &s3.Backend{},
			srcPath:    "bucket/path/F0001.rf",
			dstBackend: &s3.Backend{},
			dstPath:    "s3://bucket/path/F0001.rf",
			want:       true,
		},
		{
			name:       "gcs scheme-less aliases qualified path",
			srcBackend: &gcs.Backend{},
			srcPath:    "bucket/path/F0001.rf",
			dstBackend: &gcs.Backend{},
			dstPath:    "gs://bucket/path/F0001.rf",
			want:       true,
		},
		{
			name:       "azure scheme-less aliases qualified path",
			srcBackend: &azure.Backend{},
			srcPath:    "container/path/F0001.rf",
			dstBackend: &azure.Backend{},
			dstPath:    "az://container/path/F0001.rf",
			want:       true,
		},
		{
			name:       "hdfs authorityless aliases qualified path on same backend",
			srcBackend: hdfsBackend,
			srcPath:    "/tables/1.rf",
			dstBackend: hdfsBackend,
			dstPath:    "hdfs://nn:8020/tables/1.rf",
			want:       true,
		},
		{
			name:       "uppercase hdfs authorityless aliases qualified path on same backend",
			srcBackend: hdfsBackend,
			srcPath:    "HDFS:/tables/1.rf",
			dstBackend: hdfsBackend,
			dstPath:    "hdfs://nn:8020/tables/1.rf",
			want:       true,
		},
		{
			name:       "local path does not alias s3 object spelling",
			srcBackend: local.New(),
			srcPath:    "bucket/path/F0001.rf",
			dstBackend: &s3.Backend{},
			dstPath:    "s3://bucket/path/F0001.rf",
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stagePathsAliasOnBackends(tt.srcBackend, tt.srcPath, tt.dstBackend, tt.dstPath); got != tt.want {
				t.Fatalf(
					"stagePathsAliasOnBackends(%T, %q, %T, %q) = %v, want %v",
					tt.srcBackend, tt.srcPath, tt.dstBackend, tt.dstPath, got, tt.want,
				)
			}
		})
	}
}

// TestStagePathsAliasPhysicalIdentity covers the cases a purely lexical
// path comparison misses: an absolute path and an equivalent relative
// path (resolved against the process's working directory, exactly as
// os.Open/os.OpenFile would) naming the same file, and a symlink naming
// the same file as its target. Both must still be detected as aliasing
// so StageBulkDir doesn't truncate the source through a path form the
// string comparison alone doesn't recognize.
func TestStagePathsAliasPhysicalIdentity(t *testing.T) {
	t.Run("absolute path aliases equivalent relative path", func(t *testing.T) {
		dir := t.TempDir()
		absPath := filepath.Join(dir, "F0001.rf")
		if err := os.WriteFile(absPath, []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)
		relPath := "F0001.rf"

		if !stagePathsAlias(absPath, relPath) {
			t.Fatalf("stagePathsAlias(%q, %q) = false, want true (same physical file via absolute vs. relative path)", absPath, relPath)
		}
	})

	t.Run("symlink aliases its target", func(t *testing.T) {
		dir := t.TempDir()
		realPath := filepath.Join(dir, "F0001.rf")
		if err := os.WriteFile(realPath, []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
		linkPath := filepath.Join(dir, "F0001-link.rf")
		if err := os.Symlink(realPath, linkPath); err != nil {
			t.Skipf("symlink not supported in this environment: %v", err)
		}

		if !stagePathsAlias(realPath, linkPath) {
			t.Fatalf("stagePathsAlias(%q, %q) = false, want true (same physical file via symlink)", realPath, linkPath)
		}
	})

	t.Run("distinct files are not aliased", func(t *testing.T) {
		dir := t.TempDir()
		aPath := filepath.Join(dir, "a.rf")
		bPath := filepath.Join(dir, "b.rf")
		if err := os.WriteFile(aPath, []byte("a"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(bPath, []byte("b"), 0o644); err != nil {
			t.Fatal(err)
		}

		if stagePathsAlias(aPath, bPath) {
			t.Fatalf("stagePathsAlias(%q, %q) = true, want false (distinct files that happen to both exist)", aPath, bPath)
		}
	})

	t.Run("nonexistent destination is not aliased", func(t *testing.T) {
		dir := t.TempDir()
		srcPath := filepath.Join(dir, "F0001.rf")
		if err := os.WriteFile(srcPath, []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
		dstPath := filepath.Join(dir, "bulk", "F0001.rf")

		if stagePathsAlias(srcPath, dstPath) {
			t.Fatalf("stagePathsAlias(%q, %q) = true, want false (destination doesn't exist yet)", srcPath, dstPath)
		}
	})

	t.Run("symlink resolves through a symlinked parent directory to a not-yet-existing file", func(t *testing.T) {
		dir := t.TempDir()
		aliasDir := filepath.Join(dir, "alias")
		if err := os.Symlink(".", aliasDir); err != nil {
			t.Skipf("symlink not supported in this environment: %v", err)
		}
		linkPath := filepath.Join(dir, "A.rf")
		if err := os.Symlink(filepath.Join("alias", "B.rf"), linkPath); err != nil {
			t.Skipf("symlink not supported in this environment: %v", err)
		}
		bPath := filepath.Join(dir, "B.rf")
		// bPath does not exist yet: linkPath's target chain (A.rf ->
		// alias/B.rf, alias -> ".") only physically reaches bPath once
		// the symlinked "alias" parent directory component is followed
		// too, not just linkPath's own, single-hop symlink target.

		if !stagePathsAlias(linkPath, bPath) {
			t.Fatalf("stagePathsAlias(%q, %q) = false, want true (same not-yet-existing physical location via a symlinked parent directory)", linkPath, bPath)
		}
	})
}

func TestStagePathsAliasResolvesChainedParentSymlinksForNonexistentTargets(t *testing.T) {
	root := t.TempDir()
	realBulk := filepath.Join(root, "real-bulk")
	if err := os.MkdirAll(realBulk, 0o755); err != nil {
		t.Fatal(err)
	}

	secondHop := filepath.Join(root, "second-hop")
	if err := os.Symlink(realBulk, secondHop); err != nil {
		t.Skipf("symlink not supported in this environment: %v", err)
	}
	firstHop := filepath.Join(root, "first-hop")
	if err := os.Symlink(secondHop, firstHop); err != nil {
		t.Skipf("symlink not supported in this environment: %v", err)
	}

	left := filepath.Join(firstHop, "nested", "F0001.rf")
	right := filepath.Join(realBulk, "nested", "F0001.rf")
	if !stagePathsAlias(left, right) {
		t.Fatalf("stagePathsAlias(%q, %q) = false, want true after resolving chained parent symlinks for nonexistent targets", left, right)
	}
}

func TestStagePathsAliasResolvesDanglingRelativeSymlinkChains(t *testing.T) {
	root := t.TempDir()
	targetPath := filepath.Join(root, "target.rf")
	link2 := filepath.Join(root, "link2.rf")
	if err := os.Symlink("target.rf", link2); err != nil {
		t.Skipf("symlink not supported in this environment: %v", err)
	}
	link1 := filepath.Join(root, "link1.rf")
	if err := os.Symlink("link2.rf", link1); err != nil {
		t.Skipf("symlink not supported in this environment: %v", err)
	}

	if !stagePathsAlias(link1, targetPath) {
		t.Fatalf("stagePathsAlias(%q, %q) = false, want true after resolving a dangling relative symlink chain lexically", link1, targetPath)
	}
}

func TestStagePathsAliasResolvesDanglingTargetParentSymlinkComponents(t *testing.T) {
	root := t.TempDir()
	bulkDir := filepath.Join(root, "bulk")
	if err := os.MkdirAll(bulkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".", filepath.Join(bulkDir, "dirlink")); err != nil {
		t.Skipf("symlink not supported in this environment: %v", err)
	}
	aPath := filepath.Join(bulkDir, "A.rf")
	if err := os.Symlink(filepath.Join("dirlink", "B.rf"), aPath); err != nil {
		t.Skipf("symlink not supported in this environment: %v", err)
	}
	bPath := filepath.Join(bulkDir, "B.rf")

	if !stagePathsAlias(aPath, bPath) {
		t.Fatalf("stagePathsAlias(%q, %q) = false, want true after resolving parent symlink components in a dangling final target", aPath, bPath)
	}
}

// TestStageBulkDirRejectsInPlaceBulkDirViaRelativePath proves the alias
// guard still catches an aliased bulkDir when it's expressed as a path
// string that is lexically different from the manifest's absolute
// DestinationPath — here, bulkDir is passed as a path relative to the
// process's working directory that happens to resolve to the same
// physical tablet directory the RFile was exported to. A purely lexical
// comparison (the pre-fix behavior) would miss this and let
// storage.Copy truncate the source in place.
func TestStageBulkDirRejectsInPlaceBulkDirViaRelativePath(t *testing.T) {
	root := t.TempDir()
	tabletDir := filepath.Join(root, "export", "events", "t-0000")
	if err := os.MkdirAll(tabletDir, 0o755); err != nil {
		t.Fatal(err)
	}
	srcPath := filepath.Join(tabletDir, "F0001.rf")
	content := []byte("original rfile bytes that must survive a rejected in-place stage")
	if err := os.WriteFile(srcPath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)

	manifest := &engine.RFileExportManifest{
		Version:     engine.RFileExportManifestVersion,
		SourceTable: "events",
		Tablets:     []engine.RFileExportTablet{{Index: 0}},
		RFiles: []engine.RFileExportFile{
			{TabletIndex: 0, DestinationPath: srcPath, Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:])},
		},
	}

	// The manifest's DestinationPath is absolute; pass bulkDir as an
	// equivalent path relative to root instead, to prove the guard
	// doesn't depend on both sides sharing the same path form.
	t.Chdir(root)
	relBulkDir := filepath.Join("export", "events", "t-0000")

	be := local.New()
	ctx := context.Background()
	if _, err := StageBulkDir(ctx, be, manifest, be, relBulkDir); err == nil {
		t.Fatal("StageBulkDir with relative bulkDir aliasing the absolute source tablet dir = nil error, want error")
	}

	got, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("source file missing after rejected in-place stage: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("source file corrupted by rejected in-place stage: got %d bytes, want %d bytes intact", len(got), len(content))
	}
}

func TestStageBulkDirRejectsUnixRelativeCDriveHardlinkAliasBeforeCopying(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("C:/... is a Windows drive path on Windows, not a Unix-relative local path")
	}

	root := t.TempDir()
	t.Chdir(root)

	exportDir := filepath.Join(root, "C:", "export")
	bulkDirPath := filepath.Join(root, "C:", "bulk")
	if err := os.MkdirAll(exportDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(bulkDirPath, 0o755); err != nil {
		t.Fatal(err)
	}

	srcPath := filepath.Join("C:", "export", "F0001.rf")
	content := []byte("original rfile bytes that must survive a rejected unix-relative C-drive hardlink alias stage")
	if err := os.WriteFile(srcPath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	aliasPath := filepath.Join("C:", "bulk", "F0001.rf")
	if err := os.Link(srcPath, aliasPath); err != nil {
		t.Skipf("hard links not supported in this environment: %v", err)
	}

	manifest := localManifestFromFiles(t, srcPath)
	be := local.New()
	if _, err := StageBulkDir(context.Background(), be, manifest, be, filepath.Join("C:", "bulk")); err == nil {
		t.Fatal("StageBulkDir with unix-relative C:/bulk/F0001.rf hard-linked to source C:/export/F0001.rf = nil error, want error")
	}

	got, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("source file missing after rejected unix-relative C-drive stage: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("source file corrupted by rejected unix-relative C-drive stage: got %d bytes, want %d bytes intact", len(got), len(content))
	}
}

func TestStageBulkDirRejectsHDFSSpelledLocalBulkDirAliasBeforeCopying(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("paths containing ':' are not valid local relative paths on Windows")
	}

	root := t.TempDir()
	bulkDir := filepath.Join("hdfs:", "bulk")
	tabletDir := filepath.Join(root, bulkDir)
	if err := os.MkdirAll(tabletDir, 0o755); err != nil {
		t.Fatal(err)
	}
	srcPath := filepath.Join(tabletDir, "F0001.rf")
	content := []byte("original rfile bytes that must survive a rejected hdfs:-spelled local in-place stage")
	if err := os.WriteFile(srcPath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	manifest := &engine.RFileExportManifest{
		Version:     engine.RFileExportManifestVersion,
		SourceTable: "events",
		Tablets:     []engine.RFileExportTablet{{Index: 0}},
		RFiles: []engine.RFileExportFile{
			{TabletIndex: 0, DestinationPath: srcPath, Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:])},
		},
	}

	tests := []struct {
		name string
		src  func(t *testing.T) shstorage.Backend
	}{
		{name: "local source", src: func(t *testing.T) shstorage.Backend { return local.New() }},
		{
			name: "diskcache wrapped local source",
			src: func(t *testing.T) shstorage.Backend {
				backend, err := diskcache.New(local.New(), filepath.Join(root, "cache"), 1<<20)
				if err != nil {
					t.Fatalf("diskcache.New: %v", err)
				}
				return backend
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Chdir(root)
			be := local.New()
			if _, err := StageBulkDir(context.Background(), tt.src(t), manifest, be, bulkDir); err == nil {
				t.Fatal("StageBulkDir with bulkDir hdfs:/bulk aliasing the local source tablet dir = nil error, want error")
			}
			got, err := os.ReadFile(srcPath)
			if err != nil {
				t.Fatalf("source file missing after rejected hdfs:-spelled local stage: %v", err)
			}
			if string(got) != string(content) {
				t.Fatalf("source file corrupted by rejected hdfs:-spelled local stage: got %d bytes, want %d bytes intact", len(got), len(content))
			}
		})
	}
}

// TestStageBulkDirRejectsCrossFileAliasBeforeCopying proves the preflight
// check catches a destination that aliases a *different* manifest
// entry's source file, not just its own. Here bulkDir/A.rf is a symlink
// to B's source file: without checking every destination against every
// unique source, StageBulkDir would only compare A's destination to A's
// own source (finding no alias), start copying, and truncate B's source
// through the symlink before B is ever staged or re-verified.
func TestStageBulkDirRejectsCrossFileAliasBeforeCopying(t *testing.T) {
	root := t.TempDir()
	exportDir := filepath.Join(root, "export")
	if err := os.MkdirAll(exportDir, 0o755); err != nil {
		t.Fatal(err)
	}
	aPath := filepath.Join(exportDir, "A.rf")
	bPath := filepath.Join(exportDir, "B.rf")
	aContent := []byte("A file bytes")
	bContent := []byte("B file bytes that must survive a rejected stage")
	if err := os.WriteFile(aPath, aContent, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bPath, bContent, 0o644); err != nil {
		t.Fatal(err)
	}

	bulkDir := filepath.Join(root, "bulk")
	if err := os.MkdirAll(bulkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A's computed destination (bulkDir/A.rf, since flattenNames uses the
	// basename) is a symlink to B's source -- a different RFile entirely.
	aDstPath := filepath.Join(bulkDir, "A.rf")
	if err := os.Symlink(bPath, aDstPath); err != nil {
		t.Skipf("symlink not supported in this environment: %v", err)
	}

	aSum := sha256.Sum256(aContent)
	bSum := sha256.Sum256(bContent)
	manifest := &engine.RFileExportManifest{
		Version:     engine.RFileExportManifestVersion,
		SourceTable: "events",
		Tablets:     []engine.RFileExportTablet{{Index: 0}},
		RFiles: []engine.RFileExportFile{
			{TabletIndex: 0, DestinationPath: aPath, Size: int64(len(aContent)), SHA256: hex.EncodeToString(aSum[:])},
			{TabletIndex: 0, DestinationPath: bPath, Size: int64(len(bContent)), SHA256: hex.EncodeToString(bSum[:])},
		},
	}

	be := local.New()
	ctx := context.Background()
	if _, err := StageBulkDir(ctx, be, manifest, be, bulkDir); err == nil {
		t.Fatal("StageBulkDir with a destination aliasing a different RFile's source = nil error, want error")
	}

	got, err := os.ReadFile(bPath)
	if err != nil {
		t.Fatalf("source file B missing after rejected stage: %v", err)
	}
	if string(got) != string(bContent) {
		t.Fatalf("source file B corrupted by rejected stage: got %q, want %q", got, bContent)
	}
}

func TestStageBulkDirRejectsSymlinkedWriteTargetsBeforeCopying(t *testing.T) {
	root := t.TempDir()
	exportDir := filepath.Join(root, "export")
	bulkDir := filepath.Join(root, "bulk")
	if err := os.MkdirAll(exportDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(bulkDir, 0o755); err != nil {
		t.Fatal(err)
	}

	aPath := filepath.Join(exportDir, "A.rf")
	bPath := filepath.Join(exportDir, "B.rf")
	aContent := []byte("A source bytes that must survive a rejected stage")
	bContent := []byte("B source bytes that must survive a rejected stage")
	if err := os.WriteFile(aPath, aContent, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bPath, bContent, 0o644); err != nil {
		t.Fatal(err)
	}

	manifest := localManifestFromFiles(t, aPath, bPath)
	aDstPath := filepath.Join(bulkDir, "A.rf")
	bDstPath := filepath.Join(bulkDir, "B.rf")
	if err := os.Symlink(bDstPath, aDstPath); err != nil {
		t.Skipf("symlink not supported in this environment: %v", err)
	}

	be := local.New()
	if _, err := StageBulkDir(context.Background(), be, manifest, be, bulkDir); err == nil {
		t.Fatal("StageBulkDir with a symlinked write target = nil error, want error")
	}

	gotA, err := os.ReadFile(aPath)
	if err != nil {
		t.Fatalf("source file A missing after rejected stage: %v", err)
	}
	if string(gotA) != string(aContent) {
		t.Fatalf("source file A corrupted by rejected stage: got %q, want %q", gotA, aContent)
	}
	gotB, err := os.ReadFile(bPath)
	if err != nil {
		t.Fatalf("source file B missing after rejected stage: %v", err)
	}
	if string(gotB) != string(bContent) {
		t.Fatalf("source file B corrupted by rejected stage: got %q, want %q", gotB, bContent)
	}
	if _, err := os.Stat(bDstPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("symlink target unexpectedly exists after rejected stage: err=%v", err)
	}
}

// TestStageBulkDirRejectsWriteTargetAliasedThroughSymlinkedParentBeforeCopying
// proves the write-target alias guard catches a case one hop deeper
// than TestStageBulkDirRejectsSymlinkedWriteTargetsBeforeCopying: here
// bulkDir/A.rf's own symlink target, "alias/B.rf", is lexically
// different from bulkDir/B.rf and neither exists yet, so a naive
// single-hop symlink resolution sees no alias at all. Only resolving
// the symlinked "alias" *parent* directory component -- which points
// back at bulkDir itself -- reveals that bulkDir/A.rf and bulkDir/B.rf
// both reach the same not-yet-created physical location.
func TestStageBulkDirRejectsWriteTargetAliasedThroughSymlinkedParentBeforeCopying(t *testing.T) {
	root := t.TempDir()
	exportDir := filepath.Join(root, "export")
	bulkDir := filepath.Join(root, "bulk")
	if err := os.MkdirAll(exportDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(bulkDir, 0o755); err != nil {
		t.Fatal(err)
	}

	aPath := filepath.Join(exportDir, "A.rf")
	bPath := filepath.Join(exportDir, "B.rf")
	aContent := []byte("A source bytes that must survive a rejected stage")
	bContent := []byte("B source bytes that must survive a rejected stage")
	if err := os.WriteFile(aPath, aContent, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bPath, bContent, 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := localManifestFromFiles(t, aPath, bPath)

	aliasDir := filepath.Join(bulkDir, "alias")
	if err := os.Symlink(".", aliasDir); err != nil {
		t.Skipf("symlink not supported in this environment: %v", err)
	}
	aDstPath := filepath.Join(bulkDir, "A.rf")
	if err := os.Symlink(filepath.Join("alias", "B.rf"), aDstPath); err != nil {
		t.Skipf("symlink not supported in this environment: %v", err)
	}
	bDstPath := filepath.Join(bulkDir, "B.rf")

	be := local.New()
	if _, err := StageBulkDir(context.Background(), be, manifest, be, bulkDir); err == nil {
		t.Fatal("StageBulkDir with a write target aliased through a symlinked parent directory = nil error, want error")
	}

	gotA, err := os.ReadFile(aPath)
	if err != nil {
		t.Fatalf("source file A missing after rejected stage: %v", err)
	}
	if string(gotA) != string(aContent) {
		t.Fatalf("source file A corrupted by rejected stage: got %q, want %q", gotA, aContent)
	}
	gotB, err := os.ReadFile(bPath)
	if err != nil {
		t.Fatalf("source file B missing after rejected stage: %v", err)
	}
	if string(gotB) != string(bContent) {
		t.Fatalf("source file B corrupted by rejected stage: got %q, want %q", gotB, bContent)
	}
	if _, err := os.Stat(bDstPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("write target aliased through symlinked parent unexpectedly exists after rejected stage: err=%v", err)
	}
}

// TestStageBulkDirRejectsDanglingFinalSymlinkChainAliasBeforeCopying proves
// the write-target alias guard follows a multi-hop *direct* symlink chain
// (bulkDir/A.rf -> mid.rf -> B.rf), not just a single symlink hop, even when
// every link in the chain is dangling (B.rf does not exist yet, since it is
// itself one of the write targets StageBulkDir is about to create).
func TestStageBulkDirRejectsDanglingFinalSymlinkChainAliasBeforeCopying(t *testing.T) {
	root := t.TempDir()
	exportDir := filepath.Join(root, "export")
	bulkDir := filepath.Join(root, "bulk")
	if err := os.MkdirAll(exportDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(bulkDir, 0o755); err != nil {
		t.Fatal(err)
	}

	aPath := filepath.Join(exportDir, "A.rf")
	bPath := filepath.Join(exportDir, "B.rf")
	aContent := []byte("A source bytes")
	bContent := []byte("B source bytes")
	if err := os.WriteFile(aPath, aContent, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bPath, bContent, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink("B.rf", filepath.Join(bulkDir, "mid.rf")); err != nil {
		t.Skipf("symlink not supported in this environment: %v", err)
	}
	if err := os.Symlink("mid.rf", filepath.Join(bulkDir, "A.rf")); err != nil {
		t.Skipf("symlink not supported in this environment: %v", err)
	}

	manifest := localManifestFromFiles(t, aPath, bPath)
	be := local.New()
	if _, err := StageBulkDir(context.Background(), be, manifest, be, bulkDir); err == nil {
		t.Fatal("StageBulkDir with A.rf symlinked through a dangling chain to B.rf = nil error, want error")
	}
	if _, err := os.Stat(filepath.Join(bulkDir, "B.rf")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dangling symlink target unexpectedly exists after rejected stage: err=%v", err)
	}
}

func TestStageBulkDirRejectsDanglingFinalSymlinkTargetWithParentAliasBeforeCopying(t *testing.T) {
	root := t.TempDir()
	exportDir := filepath.Join(root, "export")
	bulkDir := filepath.Join(root, "bulk")
	if err := os.MkdirAll(exportDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(bulkDir, 0o755); err != nil {
		t.Fatal(err)
	}

	aPath := filepath.Join(exportDir, "A.rf")
	bPath := filepath.Join(exportDir, "B.rf")
	aContent := []byte("A source bytes")
	bContent := []byte("B source bytes")
	if err := os.WriteFile(aPath, aContent, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bPath, bContent, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(".", filepath.Join(bulkDir, "dirlink")); err != nil {
		t.Skipf("symlink not supported in this environment: %v", err)
	}
	if err := os.Symlink(filepath.Join("dirlink", "B.rf"), filepath.Join(bulkDir, "A.rf")); err != nil {
		t.Skipf("symlink not supported in this environment: %v", err)
	}

	manifest := localManifestFromFiles(t, aPath, bPath)
	be := local.New()
	if _, err := StageBulkDir(context.Background(), be, manifest, be, bulkDir); err == nil {
		t.Fatal("StageBulkDir with A.rf symlinked to dirlink/B.rf where dirlink aliases bulkDir = nil error, want error")
	}
	if _, err := os.Stat(filepath.Join(bulkDir, "B.rf")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("symlink target unexpectedly exists after rejected stage: err=%v", err)
	}
}

func TestStageBulkDirRejectsLoadMapAliasBeforeCopying(t *testing.T) {
	root := t.TempDir()
	exportDir := filepath.Join(root, "export")
	bulkDir := filepath.Join(root, "bulk")
	if err := os.MkdirAll(exportDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(bulkDir, 0o755); err != nil {
		t.Fatal(err)
	}

	srcPath := filepath.Join(exportDir, "F0001.rf")
	content := []byte("source bytes that must survive a rejected loadmap alias stage")
	if err := os.WriteFile(srcPath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := localManifestFromFiles(t, srcPath)

	loadmapPath := filepath.Join(bulkDir, "loadmap.json")
	if err := os.Link(srcPath, loadmapPath); err != nil {
		t.Skipf("hard links not supported in this environment: %v", err)
	}

	be := local.New()
	if _, err := StageBulkDir(context.Background(), be, manifest, be, bulkDir); err == nil {
		t.Fatal("StageBulkDir with loadmap.json aliasing the source = nil error, want error")
	}

	got, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("source file missing after rejected loadmap alias stage: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("source file corrupted by rejected loadmap alias stage: got %q, want %q", got, content)
	}
	gotLoadmap, err := os.ReadFile(loadmapPath)
	if err != nil {
		t.Fatalf("loadmap alias missing after rejected stage: %v", err)
	}
	if string(gotLoadmap) != string(content) {
		t.Fatalf("loadmap alias content changed after rejected stage: got %q, want %q", gotLoadmap, content)
	}
	if _, err := os.Stat(filepath.Join(bulkDir, "F0001.rf")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staged file exists after rejected loadmap alias stage: err=%v, want not-exist", err)
	}
}

// TestPathIdentityCacheMemoizesStatResults proves pathIdentityCache
// actually memoizes: checkNoStagingAliases's O(N^2) all-pairs comparison
// only stays cheap in filesystem-syscall terms if repeated stat calls for
// the same path are served from cache rather than re-stat'ing. This is
// verified indirectly (mutating the filesystem between two stat calls
// for the same path and confirming the second call still reflects the
// first, cached, result) since the cache sits directly in front of the
// os package and cannot be swapped for a counting fake.
func TestPathIdentityCacheMemoizesStatResults(t *testing.T) {
	t.Run("a successful stat is served from cache after the file is removed", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "f.rf")
		if err := os.WriteFile(path, []byte("data"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		cache := newPathIdentityCache(1)
		first := cache.stat(path)
		if first == nil {
			t.Fatalf("stat(%q) = nil, want a FileInfo for an existing file", path)
		}

		if err := os.Remove(path); err != nil {
			t.Fatalf("Remove: %v", err)
		}
		if _, err := os.Stat(path); err == nil {
			t.Fatalf("path %q still exists after Remove; test setup invalid", path)
		}

		second := cache.stat(path)
		if second == nil {
			t.Fatalf("stat(%q) after removal = nil, want the cached pre-removal FileInfo (a fresh os.Stat would fail here)", path)
		}
		if second != first {
			t.Fatalf("stat(%q) after removal returned a different FileInfo than the first call; cache was not reused", path)
		}
	})

	t.Run("a failed stat is cached too, not retried", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "does-not-exist-yet.rf")

		cache := newPathIdentityCache(1)
		first := cache.stat(path)
		if first != nil {
			t.Fatalf("stat(%q) = %v, want nil for a nonexistent file", path, first)
		}

		if err := os.WriteFile(path, []byte("data"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		second := cache.stat(path)
		if second != nil {
			t.Fatalf("stat(%q) after later creating the file = %v, want the cached nil (a fresh os.Stat would now succeed)", path, second)
		}
	})
}

func TestCheckNoStagingAliasesMemoizesRemoteCanonicalPaths(t *testing.T) {
	const fileCount = 64

	originalParseS3Path := parseS3Path
	originalStagePathBackendKey := stagePathBackendKey
	var parseCalls atomic.Int64
	var backendKeyCalls atomic.Int64
	parseS3Path = func(path string) (string, string, error) {
		parseCalls.Add(1)
		return s3.ParsePath(path)
	}
	stagePathBackendKey = func(backend shstorage.Backend) string {
		backendKeyCalls.Add(1)
		return canonicalPathBackendKey(backend)
	}
	t.Cleanup(func() {
		parseS3Path = originalParseS3Path
		stagePathBackendKey = originalStagePathBackendKey
	})

	flatNames := make(map[string]string, fileCount)
	for i := 0; i < fileCount; i++ {
		name := fmt.Sprintf("F%04d.rf", i)
		flatNames[fmt.Sprintf("s3://bucket/export/%s", name)] = name
	}

	if err := checkNoStagingAliases(&s3.Backend{}, &s3.Backend{}, flatNames, "s3://bucket/bulk"); err != nil {
		t.Fatalf("checkNoStagingAliases(remote manifest): %v", err)
	}

	wantMax := int64(fileCount*2 + 1) // each source, each write target, and loadmap.json once
	if got := parseCalls.Load(); got > wantMax {
		t.Fatalf("parseS3Path called %d times, want <= %d cached canonicalizations", got, wantMax)
	}
	if got := backendKeyCalls.Load(); got > wantMax {
		t.Fatalf("stagePathBackendKey called %d times, want <= %d precomputed backend identities", got, wantMax)
	}
}

// TestStageBulkDirRejectsLoadMappingAliasBeforeCopying proves the
// preflight also protects bulkDir's own loadmap.json write target, not
// just the flattened RFile destinations. Here bulkDir/loadmap.json
// (created before StageBulkDir runs) is a symlink to the sole RFile's
// source: without including the load-mapping path in the checked target
// set, StageBulkDir would successfully copy the RFile, then destroy its
// own just-copied source when WriteLoadMapping opens the symlinked
// loadmap.json path for writing (WriteLoadMapping truncates exactly like
// storage.Copy does, and it runs after every RFile copy, so this would
// corrupt a source the rest of the stage already treated as verified and
// safely staged).
func TestStageBulkDirRejectsLoadMappingAliasBeforeCopying(t *testing.T) {
	root := t.TempDir()
	exportDir := filepath.Join(root, "export")
	if err := os.MkdirAll(exportDir, 0o755); err != nil {
		t.Fatal(err)
	}
	srcPath := filepath.Join(exportDir, "F0001.rf")
	content := []byte("rfile bytes that must survive a rejected stage")
	if err := os.WriteFile(srcPath, content, 0o644); err != nil {
		t.Fatal(err)
	}

	bulkDir := filepath.Join(root, "bulk")
	if err := os.MkdirAll(bulkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// bulkDir/loadmap.json is a symlink to the RFile's own source.
	loadMapPath := filepath.Join(bulkDir, "loadmap.json")
	if err := os.Symlink(srcPath, loadMapPath); err != nil {
		t.Skipf("symlink not supported in this environment: %v", err)
	}

	sum := sha256.Sum256(content)
	manifest := &engine.RFileExportManifest{
		Version:     engine.RFileExportManifestVersion,
		SourceTable: "events",
		Tablets:     []engine.RFileExportTablet{{Index: 0}},
		RFiles: []engine.RFileExportFile{
			{TabletIndex: 0, DestinationPath: srcPath, Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:])},
		},
	}

	be := local.New()
	ctx := context.Background()
	if _, err := StageBulkDir(ctx, be, manifest, be, bulkDir); err == nil {
		t.Fatal("StageBulkDir with loadmap.json aliasing the sole RFile's source = nil error, want error")
	}

	got, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("source file missing after rejected stage: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("source file corrupted by rejected stage: got %q, want %q", got, content)
	}
}

// TestStageBulkDirRejectsHardLinkedWriteTargetsBeforeCopying proves the
// preflight also catches two different flattened destinations that are
// hard-linked to each other, even though neither aliases any manifest
// source. Here bulkDir/A.rf and bulkDir/B.rf (created before StageBulkDir
// runs) are hard-linked to the same physical file: without a
// target-vs-target check, both individually pass the target-vs-source
// check (A's and B's sources are genuinely distinct files), so copying A
// then B would silently overwrite A's just-staged bytes with B's, while
// the load mapping still records both names as independent files with
// independent sizes.
func TestStageBulkDirRejectsHardLinkedWriteTargetsBeforeCopying(t *testing.T) {
	root := t.TempDir()
	exportDir := filepath.Join(root, "export")
	if err := os.MkdirAll(exportDir, 0o755); err != nil {
		t.Fatal(err)
	}
	aPath := filepath.Join(exportDir, "A.rf")
	bPath := filepath.Join(exportDir, "B.rf")
	aContent := []byte("A file bytes")
	bContent := []byte("B file bytes that must survive a rejected stage")
	if err := os.WriteFile(aPath, aContent, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bPath, bContent, 0o644); err != nil {
		t.Fatal(err)
	}

	bulkDir := filepath.Join(root, "bulk")
	if err := os.MkdirAll(bulkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// bulkDir/A.rf and bulkDir/B.rf pre-exist, hard-linked to the same
	// physical file -- distinct names, one underlying file.
	aDstPath := filepath.Join(bulkDir, "A.rf")
	bDstPath := filepath.Join(bulkDir, "B.rf")
	if err := os.WriteFile(aDstPath, []byte("stale placeholder"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(aDstPath, bDstPath); err != nil {
		t.Skipf("hard links not supported in this environment: %v", err)
	}

	aSum := sha256.Sum256(aContent)
	bSum := sha256.Sum256(bContent)
	manifest := &engine.RFileExportManifest{
		Version:     engine.RFileExportManifestVersion,
		SourceTable: "events",
		Tablets:     []engine.RFileExportTablet{{Index: 0}},
		RFiles: []engine.RFileExportFile{
			{TabletIndex: 0, DestinationPath: aPath, Size: int64(len(aContent)), SHA256: hex.EncodeToString(aSum[:])},
			{TabletIndex: 0, DestinationPath: bPath, Size: int64(len(bContent)), SHA256: hex.EncodeToString(bSum[:])},
		},
	}

	be := local.New()
	ctx := context.Background()
	if _, err := StageBulkDir(ctx, be, manifest, be, bulkDir); err == nil {
		t.Fatal("StageBulkDir with two hard-linked write targets = nil error, want error")
	}

	gotA, err := os.ReadFile(aPath)
	if err != nil {
		t.Fatalf("source file A missing after rejected stage: %v", err)
	}
	if string(gotA) != string(aContent) {
		t.Fatalf("source file A corrupted by rejected stage: got %q, want %q", gotA, aContent)
	}
	gotB, err := os.ReadFile(bPath)
	if err != nil {
		t.Fatalf("source file B missing after rejected stage: %v", err)
	}
	if string(gotB) != string(bContent) {
		t.Fatalf("source file B corrupted by rejected stage: got %q, want %q", gotB, bContent)
	}
}

// TestStageBulkDirRejectsHardLinkedSourcesBeforeCopying proves the
// preflight also catches two manifest entries whose DestinationPath
// values are different strings but physically the same file (for
// example a hard link from one per-tablet export directory into
// another). Each entry individually passes VerifyRFileExport and
// flattens to a distinct basename (A.rf, B.rf), so neither the
// target-vs-source nor the target-vs-target comparison has any reason
// to reject them: without a source-vs-source comparison, StageBulkDir
// would "succeed" while copying the same physical file's bytes twice
// under two independent bulk-import filenames, so Accumulo would
// import every row in that file twice.
func TestStageBulkDirRejectsHardLinkedSourcesBeforeCopying(t *testing.T) {
	root := t.TempDir()
	exportDir0 := filepath.Join(root, "export", "t-0000")
	exportDir1 := filepath.Join(root, "export", "t-0001")
	if err := os.MkdirAll(exportDir0, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(exportDir1, 0o755); err != nil {
		t.Fatal(err)
	}

	aPath := filepath.Join(exportDir0, "A.rf")
	bPath := filepath.Join(exportDir1, "B.rf")
	content := []byte("shared physical file bytes")
	if err := os.WriteFile(aPath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(aPath, bPath); err != nil {
		t.Skipf("hard links not supported in this environment: %v", err)
	}

	bulkDir := filepath.Join(root, "bulk")
	if err := os.MkdirAll(bulkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Neither bulkDir/A.rf nor bulkDir/B.rf exists yet: this is not a
	// write-target alias (they flatten to distinct names) but a
	// source-vs-source alias -- the same physical file is referenced by
	// two different manifest entries.

	sum := sha256.Sum256(content)
	manifest := &engine.RFileExportManifest{
		Version:     engine.RFileExportManifestVersion,
		SourceTable: "events",
		Tablets:     []engine.RFileExportTablet{{Index: 0}},
		RFiles: []engine.RFileExportFile{
			{TabletIndex: 0, DestinationPath: aPath, Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:])},
			{TabletIndex: 0, DestinationPath: bPath, Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:])},
		},
	}

	be := local.New()
	ctx := context.Background()
	if _, err := StageBulkDir(ctx, be, manifest, be, bulkDir); err == nil {
		t.Fatal("StageBulkDir with two hard-linked manifest sources = nil error, want error")
	}

	entries, err := os.ReadDir(bulkDir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", bulkDir, err)
	}
	if len(entries) != 0 {
		t.Fatalf("bulkDir has %d entries after rejected stage, want 0 (preflight must run before any copy): %v", len(entries), entries)
	}
}

// TestStageBulkDirRejectsCaseInsensitiveAliasBeforeCopying proves the
// preflight also catches two flattened destinations that differ only by
// case, even though *neither exists yet* when the check runs. Windows
// and macOS (by default) resolve filesystem paths case-insensitively, so
// bulkDir/A.rf and bulkDir/a.rf name the same physical location there --
// but an os.Stat-based physical-identity check cannot detect that: right
// up until the second Create silently resolves onto the first, both
// paths correctly stat as "doesn't exist yet." Without a case-
// insensitive lexical check, StageBulkDir would copy the first source,
// then silently overwrite it when copying the second, while the load
// mapping still records both names (A.rf and a.rf) as independent files
// with independent sizes.
func TestStageBulkDirRejectsCaseInsensitiveAliasBeforeCopying(t *testing.T) {
	root := t.TempDir()
	exportDir0 := filepath.Join(root, "export", "t-0000")
	exportDir1 := filepath.Join(root, "export", "t-0001")
	if err := os.MkdirAll(exportDir0, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(exportDir1, 0o755); err != nil {
		t.Fatal(err)
	}
	upperPath := filepath.Join(exportDir0, "A.rf")
	lowerPath := filepath.Join(exportDir1, "a.rf")
	upperContent := []byte("upper file bytes that must survive a rejected stage")
	lowerContent := []byte("lower file bytes")
	if err := os.WriteFile(upperPath, upperContent, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lowerPath, lowerContent, 0o644); err != nil {
		t.Fatal(err)
	}

	bulkDir := filepath.Join(root, "bulk")
	if err := os.MkdirAll(bulkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Neither bulkDir/A.rf nor bulkDir/a.rf exists yet -- the scenario an
	// os.Stat-based physical-identity check cannot catch, since there's
	// nothing to stat until after the (unsafe) first copy.

	upperSum := sha256.Sum256(upperContent)
	lowerSum := sha256.Sum256(lowerContent)
	manifest := &engine.RFileExportManifest{
		Version:     engine.RFileExportManifestVersion,
		SourceTable: "events",
		Tablets:     []engine.RFileExportTablet{{Index: 0}},
		RFiles: []engine.RFileExportFile{
			{TabletIndex: 0, DestinationPath: upperPath, Size: int64(len(upperContent)), SHA256: hex.EncodeToString(upperSum[:])},
			{TabletIndex: 0, DestinationPath: lowerPath, Size: int64(len(lowerContent)), SHA256: hex.EncodeToString(lowerSum[:])},
		},
	}

	be := local.New()
	ctx := context.Background()
	if _, err := StageBulkDir(ctx, be, manifest, be, bulkDir); err == nil {
		t.Fatal("StageBulkDir with two case-insensitively-aliased write targets = nil error, want error")
	}

	gotUpper, err := os.ReadFile(upperPath)
	if err != nil {
		t.Fatalf("source file A.rf missing after rejected stage: %v", err)
	}
	if string(gotUpper) != string(upperContent) {
		t.Fatalf("source file A.rf corrupted by rejected stage: got %q, want %q", gotUpper, upperContent)
	}
	gotLower, err := os.ReadFile(lowerPath)
	if err != nil {
		t.Fatalf("source file a.rf missing after rejected stage: %v", err)
	}
	if string(gotLower) != string(lowerContent) {
		t.Fatalf("source file a.rf corrupted by rejected stage: got %q, want %q", gotLower, lowerContent)
	}
}

func TestStageBulkDirRejectsUnicodeNormalizedWriteTargetsBeforeCopying(t *testing.T) {
	src, manifest := memoryManifestFromBlobs(map[string][]byte{
		"export/events/t-0000/é.rf":  []byte("composed"),
		"export/events/t-0000/é.rf": []byte("decomposed"),
	})

	bulkDir := filepath.Join(t.TempDir(), "bulk")
	if err := os.MkdirAll(bulkDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := StageBulkDir(context.Background(), src, manifest, local.New(), bulkDir); err == nil {
		t.Fatal("StageBulkDir with Unicode-normalized write-target aliases = nil error, want error")
	}
}

func localManifestFromFiles(t *testing.T, paths ...string) *engine.RFileExportManifest {
	t.Helper()

	rfiles := make([]engine.RFileExportFile, 0, len(paths))
	for _, srcPath := range paths {
		data, err := os.ReadFile(srcPath)
		if err != nil {
			t.Fatalf("ReadFile(%q): %v", srcPath, err)
		}
		sum := sha256.Sum256(data)
		rfiles = append(rfiles, engine.RFileExportFile{
			TabletIndex:     0,
			DestinationPath: srcPath,
			Size:            int64(len(data)),
			SHA256:          hex.EncodeToString(sum[:]),
		})
	}

	return &engine.RFileExportManifest{
		Version:     engine.RFileExportManifestVersion,
		SourceTable: "events",
		Tablets:     []engine.RFileExportTablet{{Index: 0}},
		RFiles:      rfiles,
	}
}

func memoryManifestFromBlobs(blobs map[string][]byte) (*memory.Backend, *engine.RFileExportManifest) {
	src := memory.New()
	paths := make([]string, 0, len(blobs))
	for path := range blobs {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	rfiles := make([]engine.RFileExportFile, 0, len(paths))
	for _, path := range paths {
		data := blobs[path]
		src.Put(path, data)
		sum := sha256.Sum256(data)
		rfiles = append(rfiles, engine.RFileExportFile{
			TabletIndex:     0,
			DestinationPath: path,
			Size:            int64(len(data)),
			SHA256:          hex.EncodeToString(sum[:]),
		})
	}

	return src, &engine.RFileExportManifest{
		Version:     engine.RFileExportManifestVersion,
		SourceTable: "events",
		Tablets:     []engine.RFileExportTablet{{Index: 0}},
		RFiles:      rfiles,
	}
}

func newTestHDFSBackend(t *testing.T, address string) *hdfs.Backend {
	t.Helper()

	backend, err := hdfs.New(address, hdfs.WithClient(noopHDFSClient{}))
	if err != nil {
		t.Fatalf("hdfs.New(%q): %v", address, err)
	}
	return backend
}

type noopHDFSClient struct{}

func (noopHDFSClient) Open(string) (hdfs.Reader, error)        { return nil, errors.New("unused") }
func (noopHDFSClient) Create(string) (shstorage.Writer, error) { return nil, errors.New("unused") }
func (noopHDFSClient) MkdirAll(string, os.FileMode) error      { return errors.New("unused") }
func (noopHDFSClient) ReadDir(string) ([]os.FileInfo, error)   { return nil, errors.New("unused") }
func (noopHDFSClient) Remove(string) error                     { return errors.New("unused") }
func (noopHDFSClient) Rename(string, string) error             { return errors.New("unused") }
func (noopHDFSClient) Close() error                            { return nil }
