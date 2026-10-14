package promotion

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/accumulo"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/storage"
	"github.com/phrocker/shoal-oss/internal/storage/local"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
)

// fakePromoter records AddTableSplitsForTable and BulkImport calls instead of
// talking to Accumulo, so Promote's orchestration can be tested without
// any of accumulo's own discovery/manager fakes.
type fakePromoter struct {
	calls   int
	table   string
	bulkDir string
	opts    accumulo.BulkImportOptions
	err     error

	splitCalls int
	splitTable string
	// splitTableID records the accumulo.Table.ID AddTableSplitsForTable
	// was called with -- the pinnedTableID Promote captured via
	// ResolveTableID before calling it. Tests that need to simulate
	// the real accumulo.Connector.AddTableSplitsForTable rejecting a
	// stale pin set splitErr directly to an error wrapping
	// accumulo.ErrTableIdentityChanged; this fake does not re-derive
	// that check itself, since the check is accumulo's own
	// responsibility and already has its own dedicated tests there.
	splitTableID string
	splitRows    [][]byte
	splitErr     error

	listSplitsCalls    int
	listSplitsTable    string
	listSplitsOverride [][]byte
	listSplitsSet      bool
	listSplitsErr      error

	// tableID is the table ID ResolveTableID returns; defaults to a
	// fixed sentinel when empty, so existing tests that never set it
	// see one stable identity across every call, exactly as a real,
	// unchanged destination table would.
	tableID             string
	resolveTableIDCalls int
	resolveTableIDTable string
	resolveTableIDErr   error
	// onResolveTableID, when set, runs as a side effect of
	// ResolveTableID after it has already captured this call's return
	// value but before returning it, receiving the 1-based call number.
	// A hook that mutates tableID here therefore only changes what a
	// later call returns, not this one -- used to simulate an external
	// actor deleting and recreating the destination table under the
	// same name in between Promote's pre-AddTableSplits and
	// pre-BulkImport identity checks.
	onResolveTableID func(callNum int)

	// onAddTableSplits, when set, runs as a side effect of
	// AddTableSplitsForTable before it returns. Used to simulate an external
	// actor mutating src while a real AddTableSplits round-trip to the
	// manager would be in flight, proving StageBulkDir's own
	// re-verification (not just stagingPreflight's, which already ran
	// before this call) catches a source that changed in between.
	onAddTableSplits func()
}

// ResolveTableID defaults to "stable-table-id" when tableID is unset,
// so tests that never set either field see one consistent identity
// across every call -- matching a real, unchanged destination table.
func (f *fakePromoter) ResolveTableID(_ context.Context, tableName string) (string, error) {
	f.resolveTableIDCalls++
	f.resolveTableIDTable = tableName
	id := f.tableID
	if id == "" {
		id = "stable-table-id"
	}
	callNum := f.resolveTableIDCalls
	if f.onResolveTableID != nil {
		f.onResolveTableID(callNum)
	}
	if f.resolveTableIDErr != nil {
		return "", f.resolveTableIDErr
	}
	return id, nil
}

func (f *fakePromoter) AddTableSplitsForTable(_ context.Context, table accumulo.Table, splits [][]byte) error {
	f.splitCalls++
	f.splitTable = table.Name
	f.splitTableID = table.ID
	f.splitRows = splits
	if f.onAddTableSplits != nil {
		f.onAddTableSplits()
	}
	return f.splitErr
}

// ListTableSplits defaults to echoing back whatever AddTableSplits was
// last asked to add, i.e. it simulates a destination that now has
// exactly the required splits and nothing else -- the happy path every
// existing test other than the ones exercising this check explicitly
// relies on. Tests exercising verifyNoUnexpectedDestinationSplits set
// listSplitsOverride to simulate a destination with extra, unrelated
// splits.
func (f *fakePromoter) ListTableSplits(_ context.Context, tableName string) ([][]byte, error) {
	f.listSplitsCalls++
	f.listSplitsTable = tableName
	if f.listSplitsErr != nil {
		return nil, f.listSplitsErr
	}
	if f.listSplitsSet {
		return f.listSplitsOverride, nil
	}
	return f.splitRows, nil
}

func (f *fakePromoter) BulkImport(_ context.Context, tableName, bulkDir string, opts accumulo.BulkImportOptions) error {
	f.calls++
	f.table = tableName
	f.bulkDir = bulkDir
	f.opts = opts
	return f.err
}

func TestPromoteStagesThenSubmitsBulkImport(t *testing.T) {
	src := memory.New()
	data, file := testRFile(t, 0, "export/events/t-0000/F0001.rf", []byte("data"))
	src.Put(file.DestinationPath, data)
	manifest := &engine.RFileExportManifest{
		Version:     engine.RFileExportManifestVersion,
		SourceTable: "events",
		Tablets:     []engine.RFileExportTablet{{Index: 0}},
		RFiles:      []engine.RFileExportFile{file},
	}
	dst := memory.New()
	importer := &fakePromoter{}

	mapping, err := Promote(context.Background(), src, manifest, dst, "hdfs://nn/bulk/events-1", importer, "events", Options{SetTime: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(mapping) != 1 {
		t.Fatalf("mapping entries = %d, want 1", len(mapping))
	}
	if importer.calls != 1 {
		t.Fatalf("BulkImport calls = %d, want 1", importer.calls)
	}
	if importer.table != "events" {
		t.Fatalf("BulkImport table = %q, want %q", importer.table, "events")
	}
	if importer.bulkDir != "hdfs://nn/bulk/events-1" {
		t.Fatalf("BulkImport bulkDir = %q, want %q", importer.bulkDir, "hdfs://nn/bulk/events-1")
	}
	if !importer.opts.SetTime {
		t.Fatal("BulkImport opts.SetTime = false, want true (propagated from Options)")
	}
	if _, err := dst.Open(context.Background(), "hdfs://nn/bulk/events-1/F0001.rf"); err != nil {
		t.Fatalf("expected staged file before BulkImport call: %v", err)
	}
}

func TestPromoteDoesNotSubmitWhenStagingFails(t *testing.T) {
	src := memory.New()
	manifest := &engine.RFileExportManifest{
		Version:     engine.RFileExportManifestVersion,
		SourceTable: "events",
		Tablets:     []engine.RFileExportTablet{{Index: 0}},
		RFiles: []engine.RFileExportFile{
			{TabletIndex: 0, DestinationPath: "export/events/t-0000/F0001.rf", Size: 4},
		},
	}
	dst := memory.New()
	importer := &fakePromoter{}

	if _, err := Promote(context.Background(), src, manifest, dst, "/bulk/events-1", importer, "events", Options{}); err == nil {
		t.Fatal("Promote with missing source RFile = nil error, want error")
	}
	if importer.calls != 0 {
		t.Fatalf("BulkImport calls = %d, want 0 (staging failed before submission)", importer.calls)
	}
}

func TestPromotePropagatesBulkImportError(t *testing.T) {
	src := memory.New()
	data, file := testRFile(t, 0, "export/events/t-0000/F0001.rf", []byte("data"))
	src.Put(file.DestinationPath, data)
	manifest := &engine.RFileExportManifest{
		Version:     engine.RFileExportManifestVersion,
		SourceTable: "events",
		Tablets:     []engine.RFileExportTablet{{Index: 0}},
		RFiles:      []engine.RFileExportFile{file},
	}
	dst := memory.New()
	importer := &fakePromoter{err: accumulo.ErrTableNotFound}

	if _, err := Promote(context.Background(), src, manifest, dst, "/bulk/events-1", importer, "events", Options{}); err != accumulo.ErrTableNotFound {
		t.Fatalf("Promote error = %v, want %v", err, accumulo.ErrTableNotFound)
	}
	if importer.calls != 1 {
		t.Fatalf("BulkImport calls = %d, want 1", importer.calls)
	}
}

func TestPromoteSkipsSubmissionForEmptyMapping(t *testing.T) {
	src := memory.New()
	manifest := &engine.RFileExportManifest{
		Version:     engine.RFileExportManifestVersion,
		SourceTable: "events",
		Tablets:     []engine.RFileExportTablet{{Index: 0}},
		RFiles:      nil,
	}
	dst := memory.New()
	importer := &fakePromoter{}

	mapping, err := Promote(context.Background(), src, manifest, dst, "hdfs://nn/bulk/events-1", importer, "events", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(mapping) != 0 {
		t.Fatalf("mapping = %#v, want empty", mapping)
	}
	if importer.calls != 0 {
		t.Fatalf("BulkImport calls = %d, want 0 (empty mapping must not submit FATE)", importer.calls)
	}
	if _, err := dst.Open(context.Background(), "hdfs://nn/bulk/events-1/loadmap.json"); err != nil {
		t.Fatalf("expected loadmap.json to be written even for an empty mapping: %v", err)
	}
}

func TestPromoteRejectsInvalidDestinationInputsBeforeStagingOrSubmitting(t *testing.T) {
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
		name      string
		tableName string
		bulkDir   string
		want      error
	}{
		{name: "empty table", tableName: "", bulkDir: "/bulk/events-1", want: accumulo.ErrInvalidTableName},
		{name: "whitespace padded table", tableName: " events ", bulkDir: "/bulk/events-1", want: accumulo.ErrInvalidTableName},
		{name: "empty bulk dir", tableName: "events", bulkDir: "", want: accumulo.ErrInvalidBulkDir},
		{name: "backend root bulk dir", tableName: "events", bulkDir: "/", want: accumulo.ErrInvalidBulkDir},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dst := memory.New()
			importer := &fakePromoter{}
			if _, err := Promote(context.Background(), src, manifest, dst, tt.bulkDir, importer, tt.tableName, Options{}); !errors.Is(err, tt.want) {
				t.Fatalf("Promote error = %v, want %v", err, tt.want)
			}
			if importer.calls != 0 {
				t.Fatalf("BulkImport calls = %d, want 0 (invalid destination input must fail before submission)", importer.calls)
			}
			if got := dst.Keys(); len(got) != 0 {
				t.Fatalf("dst.Keys() = %v, want no staged files when destination validation fails first", got)
			}
		})
	}
}

func TestPromoteRejectsMalformedManifestBeforeStagingOrSubmitting(t *testing.T) {
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
	importer := &fakePromoter{}
	if _, err := Promote(context.Background(), src, manifest, dst, "/bulk/events-1", importer, "events", Options{}); err == nil {
		t.Fatal("Promote with undeclared tablet index = nil error, want error")
	}
	if importer.calls != 0 {
		t.Fatalf("BulkImport calls = %d, want 0 (malformed manifest must fail before submission)", importer.calls)
	}
	if got := dst.Keys(); len(got) != 0 {
		t.Fatalf("dst.Keys() = %v, want no staged files when manifest validation fails first", got)
	}
}

// TestPromoteRejectsUndeclaredTabletIndexBeforeAddTableSplitsForMultiTabletManifest
// proves the gap this package's preflight BuildLoadMapping call in
// Promote closes: a manifest whose tablet *chain* is entirely
// well-formed (so RequiredDestinationSplits succeeds and would
// otherwise let AddTableSplits proceed) but whose RFiles reference an
// undeclared tablet index is rejected before AddTableSplits is ever
// called, not merely before BulkImport. Unlike
// TestPromoteRejectsMalformedManifestBeforeStagingOrSubmitting's
// single-tablet manifest (which requires zero splits and so never
// exercises this ordering at all), this manifest is a valid two-tablet
// chain that does require AddTableSplits, making the ordering gap
// observable.
func TestPromoteRejectsUndeclaredTabletIndexBeforeAddTableSplitsForMultiTabletManifest(t *testing.T) {
	src := memory.New()
	src.Put("events/t-0000/F0001.rf", []byte("a"))
	manifest := twoTabletManifest()
	manifest.RFiles[0].DestinationPath = "events/t-0000/F0001.rf"
	manifest.RFiles[0].Size = 1
	manifest.RFiles[0].SHA256 = "ca978112ca1bbdcafac231b39a23dc4da786eff8147c4e72b9807785afee48bb"
	// Tablet index 2 is not declared anywhere in manifest.Tablets (only
	// 0 and 1 exist), even though the chain those two tablets form is
	// itself perfectly valid.
	manifest.RFiles[1].TabletIndex = 2
	manifest.RFiles[1].DestinationPath = "events/t-0002/F0002.rf"

	dst := memory.New()
	importer := &fakePromoter{}
	if _, err := Promote(context.Background(), src, manifest, dst, "/bulk/events-1", importer, "events", Options{}); err == nil {
		t.Fatal("Promote with an RFile referencing an undeclared tablet index = nil error, want error")
	}
	if importer.splitCalls != 0 {
		t.Fatalf("AddTableSplitsForTable calls = %d, want 0 (an RFile-level manifest error must fail before any split reconciliation)", importer.splitCalls)
	}
	if importer.calls != 0 {
		t.Fatalf("BulkImport calls = %d, want 0", importer.calls)
	}
	if got := dst.Keys(); len(got) != 0 {
		t.Fatalf("dst.Keys() = %v, want no staged files when manifest validation fails first", got)
	}
}

// TestPromoteRejectsDestinationPathDeclaredUnderConflictingTabletIndexes
// covers the other RFile-level error BuildLoadMapping's preflight now
// catches before AddTableSplits: the same DestinationPath appearing
// twice under two different TabletIndex values, which is ambiguous
// about which destination tablet the physical file actually belongs to.
func TestPromoteRejectsDestinationPathDeclaredUnderConflictingTabletIndexes(t *testing.T) {
	src := memory.New()
	src.Put("events/t-0000/F0001.rf", []byte("a"))
	manifest := twoTabletManifest()
	manifest.RFiles[0].DestinationPath = "events/t-0000/F0001.rf"
	manifest.RFiles[0].Size = 1
	manifest.RFiles[0].SHA256 = "ca978112ca1bbdcafac231b39a23dc4da786eff8147c4e72b9807785afee48bb"
	// The same DestinationPath as RFiles[0], but declared under tablet
	// index 1 instead of 0 -- an unresolvable conflict about which
	// tablet the file belongs to, not a legitimate duplicate.
	manifest.RFiles[1].TabletIndex = 1
	manifest.RFiles[1].DestinationPath = "events/t-0000/F0001.rf"

	dst := memory.New()
	importer := &fakePromoter{}
	if _, err := Promote(context.Background(), src, manifest, dst, "/bulk/events-1", importer, "events", Options{}); err == nil {
		t.Fatal("Promote with a DestinationPath declared under conflicting tablet indexes = nil error, want error")
	}
	if importer.splitCalls != 0 {
		t.Fatalf("AddTableSplitsForTable calls = %d, want 0 (an RFile-level manifest error must fail before any split reconciliation)", importer.splitCalls)
	}
	if importer.calls != 0 {
		t.Fatalf("BulkImport calls = %d, want 0", importer.calls)
	}
}

// TestPromoteRejectsUnsupportedManifestVersionBeforeAddTableSplits covers
// the third RFile-level check BuildLoadMapping's preflight now catches
// before AddTableSplits: a manifest whose Version does not match
// engine.RFileExportManifestVersion, even when the declared Tablets chain
// and every RFile are otherwise perfectly valid. Without this check,
// engine.VerifyRFileExport (called only later, from inside StageBulkDir)
// would have been the sole place a version mismatch was ever caught --
// after AddTableSplits had already reconciled the destination's splits.
func TestPromoteRejectsUnsupportedManifestVersionBeforeAddTableSplits(t *testing.T) {
	src := memory.New()
	src.Put("events/t-0000/F0001.rf", []byte("a"))
	manifest := twoTabletManifest()
	manifest.RFiles[0].DestinationPath = "events/t-0000/F0001.rf"
	manifest.RFiles[0].Size = 1
	manifest.RFiles[0].SHA256 = "ca978112ca1bbdcafac231b39a23dc4da786eff8147c4e72b9807785afee48bb"
	manifest.Version = engine.RFileExportManifestVersion + 1

	dst := memory.New()
	importer := &fakePromoter{}
	if _, err := Promote(context.Background(), src, manifest, dst, "/bulk/events-1", importer, "events", Options{}); err == nil {
		t.Fatal("Promote with an unsupported manifest version = nil error, want error")
	}
	if importer.splitCalls != 0 {
		t.Fatalf("AddTableSplitsForTable calls = %d, want 0 (an unsupported manifest version must fail before any split reconciliation)", importer.splitCalls)
	}
	if importer.calls != 0 {
		t.Fatalf("BulkImport calls = %d, want 0", importer.calls)
	}
	if got := dst.Keys(); len(got) != 0 {
		t.Fatalf("dst.Keys() = %v, want no staged files when manifest validation fails first", got)
	}
}

// TestPromoteRejectsCrossTabletBasenameCollisionBeforeAddTableSplits
// covers the staging-level check stagingPreflight now catches before
// AddTableSplits: a two-tablet manifest whose tablet chain and RFile
// index references are both individually well-formed (so BuildLoadMapping's
// own preflight passes), but whose two RFiles -- one per tablet, so
// BuildLoadMapping's DestinationPath-conflict check never sees the same
// path declared twice -- flatten to the identical bulk-directory
// basename. Without stagingPreflight, this was only ever caught later,
// inside StageBulkDir's own call to flattenNames -- after AddTableSplits
// had already reconciled the destination's splits.
func TestPromoteRejectsCrossTabletBasenameCollisionBeforeAddTableSplits(t *testing.T) {
	src := memory.New()
	src.Put("events/t-0000/F0001.rf", []byte("a"))
	src.Put("events/t-0001/F0001.rf", []byte("b"))
	manifest := twoTabletManifest()
	manifest.RFiles[0].DestinationPath = "events/t-0000/F0001.rf"
	manifest.RFiles[1].DestinationPath = "events/t-0001/F0001.rf" // same basename as tablet 0's file

	dst := memory.New()
	importer := &fakePromoter{}
	if _, err := Promote(context.Background(), src, manifest, dst, "/bulk/events-1", importer, "events", Options{}); err == nil {
		t.Fatal("Promote with a cross-tablet basename collision = nil error, want error")
	}
	if importer.splitCalls != 0 {
		t.Fatalf("AddTableSplitsForTable calls = %d, want 0 (a cross-tablet basename collision must fail before any split reconciliation)", importer.splitCalls)
	}
	if importer.calls != 0 {
		t.Fatalf("BulkImport calls = %d, want 0", importer.calls)
	}
	if got := dst.Keys(); len(got) != 0 {
		t.Fatalf("dst.Keys() = %v, want no staged files when manifest validation fails first", got)
	}
}

// TestPromoteRejectsNonLeafBasenameBeforeAddTableSplits is the
// stagingPreflight-side counterpart to
// TestPromoteRejectsCrossTabletBasenameCollisionBeforeAddTableSplits, for
// flattenNames's other rejection: a DestinationPath that collapses to a
// non-leaf basename ("." or "..", here via a two-tablet manifest so
// AddTableSplits is reachable at all, unlike
// TestStageBulkDirRejectsNonLeafBasenameBeforeCopying's single-tablet
// fixture).
func TestPromoteRejectsNonLeafBasenameBeforeAddTableSplits(t *testing.T) {
	src := memory.New()
	src.Put("events/t-0000/F0001.rf", []byte("a"))
	src.Put("events/t-0001/..", []byte("b"))
	manifest := twoTabletManifest()
	manifest.RFiles[0].DestinationPath = "events/t-0000/F0001.rf"
	manifest.RFiles[1].DestinationPath = "events/t-0001/.."

	dst := memory.New()
	importer := &fakePromoter{}
	if _, err := Promote(context.Background(), src, manifest, dst, "/bulk/events-1", importer, "events", Options{}); err == nil {
		t.Fatal("Promote with a non-leaf-flattening basename = nil error, want error")
	}
	if importer.splitCalls != 0 {
		t.Fatalf("AddTableSplitsForTable calls = %d, want 0 (a non-leaf-flattening basename must fail before any split reconciliation)", importer.splitCalls)
	}
	if importer.calls != 0 {
		t.Fatalf("BulkImport calls = %d, want 0", importer.calls)
	}
	if got := dst.Keys(); len(got) != 0 {
		t.Fatalf("dst.Keys() = %v, want no staged files when manifest validation fails first", got)
	}
}

// TestPromoteRejectsLoadMapAliasBeforeAddTableSplits is the third
// stagingPreflight-side counterpart. The two tests above both fail
// inside flattenNames, before stagingPreflight ever reaches
// checkNoStagingAliases; this test proves checkNoStagingAliases
// specifically is also run, and enforced, before AddTableSplits: a
// two-tablet manifest whose files and flattened basenames are otherwise
// all individually valid and non-colliding, but whose bulk directory
// already contains a loadmap.json hard-linked to one of the manifest's
// own source files. Uses the local backend, since
// checkNoStagingAliases's alias detection for local paths relies on
// os.SameFile, which the in-memory backend used by the other Promote
// tests in this file cannot exercise (compare
// TestStageBulkDirRejectsLoadMapAliasBeforeCopying, StageBulkDir's own
// single-tablet analogue of this same check).
func TestPromoteRejectsLoadMapAliasBeforeAddTableSplits(t *testing.T) {
	root := t.TempDir()
	exportDir := filepath.Join(root, "export")
	bulkDir := filepath.Join(root, "bulk")
	if err := os.MkdirAll(filepath.Join(exportDir, "t-0000"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(exportDir, "t-0001"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(bulkDir, 0o755); err != nil {
		t.Fatal(err)
	}

	path0 := filepath.Join(exportDir, "t-0000", "F0001.rf")
	path1 := filepath.Join(exportDir, "t-0001", "F0002.rf")
	if err := os.WriteFile(path0, []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path1, []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}

	loadmapPath := filepath.Join(bulkDir, "loadmap.json")
	if err := os.Link(path0, loadmapPath); err != nil {
		t.Skipf("hard links not supported in this environment: %v", err)
	}

	manifest := twoTabletManifest()
	manifest.RFiles[0].DestinationPath = path0
	manifest.RFiles[1].DestinationPath = path1

	be := local.New()
	importer := &fakePromoter{}
	if _, err := Promote(context.Background(), be, manifest, be, bulkDir, importer, "events", Options{}); err == nil {
		t.Fatal("Promote with loadmap.json aliasing a manifest source file = nil error, want error")
	}
	if importer.splitCalls != 0 {
		t.Fatalf("AddTableSplitsForTable calls = %d, want 0 (a staging write-target alias must fail before any split reconciliation)", importer.splitCalls)
	}
	if importer.calls != 0 {
		t.Fatalf("BulkImport calls = %d, want 0", importer.calls)
	}

	gotSrc, err := os.ReadFile(path0)
	if err != nil {
		t.Fatalf("source file missing after rejected loadmap alias promotion: %v", err)
	}
	if string(gotSrc) != "a" {
		t.Fatalf("source file corrupted by rejected loadmap alias promotion: got %q, want %q", gotSrc, "a")
	}
	if _, err := os.Stat(filepath.Join(bulkDir, "F0001.rf")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staged file exists after rejected loadmap alias promotion: err=%v, want not-exist", err)
	}
}

// TestPromoteRejectsCorruptExportBeforeAddTableSplits is the fourth
// stagingPreflight-side counterpart: engine.VerifyRFileExport's own
// rejection (mismatched size/SHA256, or a missing source object; see
// TestStageBulkDirRejectsCorruptSourceBeforeCopying for that check's own
// direct coverage) must also fire before AddTableSplits for a
// multi-tablet manifest, not just inside StageBulkDir's later call to
// the same function.
func TestPromoteRejectsCorruptExportBeforeAddTableSplits(t *testing.T) {
	src := memory.New()
	manifest := twoTabletManifest()
	manifest.RFiles[0].DestinationPath = "events/t-0000/F0001.rf"
	manifest.RFiles[1].DestinationPath = "events/t-0001/F0002.rf"
	populateManifestRFiles(t, src, manifest)
	manifest.RFiles[1].SHA256 = "0000000000000000000000000000000000000000000000000000000000000000"

	dst := memory.New()
	importer := &fakePromoter{}
	if _, err := Promote(context.Background(), src, manifest, dst, "/bulk/events-1", importer, "events", Options{}); err == nil {
		t.Fatal("Promote with a SHA256 mismatch against the manifest = nil error, want error")
	}
	if importer.splitCalls != 0 {
		t.Fatalf("AddTableSplitsForTable calls = %d, want 0 (a corrupt/mismatched export must fail before any split reconciliation)", importer.splitCalls)
	}
	if importer.calls != 0 {
		t.Fatalf("BulkImport calls = %d, want 0", importer.calls)
	}
	if got := dst.Keys(); len(got) != 0 {
		t.Fatalf("dst.Keys() = %v, want no staged files when export verification fails first", got)
	}
}

// TestPromoteRejectsSourceMutatedDuringAddTableSplits proves the second
// half of stagingPreflight's verify-twice tradeoff (see stagingPreflight
// and Promote's own doc comments): stagingPreflight's verification, run
// before AddTableSplits, cannot see a source object that is replaced
// later, while AddTableSplits/ListTableSplits are themselves in flight.
// StageBulkDir's own re-verification -- which Promote deliberately does
// not skip, even though stagingPreflight just verified the identical
// manifest -- is what actually catches this.
func TestPromoteRejectsSourceMutatedDuringAddTableSplits(t *testing.T) {
	src := memory.New()
	manifest := twoTabletManifest()
	manifest.RFiles[0].DestinationPath = "events/t-0000/F0001.rf"
	manifest.RFiles[1].DestinationPath = "events/t-0001/F0002.rf"
	populateManifestRFiles(t, src, manifest)

	dst := memory.New()
	importer := &fakePromoter{}
	// Simulates an external actor overwriting the source object with
	// different (but same-length, so only the hash check -- not a
	// size check -- can catch it) bytes while AddTableSplits' manager
	// round-trip is itself in flight, i.e. strictly after
	// stagingPreflight already verified the original "a" content.
	importer.onAddTableSplits = func() {
		row := representativeTabletRow(t, manifest, manifest.RFiles[0].TabletIndex)
		src.Put("events/t-0000/F0001.rf", validRFileBytesForRow(t, row, []byte("X")))
	}
	if _, err := Promote(context.Background(), src, manifest, dst, "/bulk/events-1", importer, "events", Options{}); err == nil {
		t.Fatal("Promote with src mutated during AddTableSplits = nil error, want error")
	}
	if importer.splitCalls != 1 {
		t.Fatalf("AddTableSplitsForTable calls = %d, want 1 (the mutation happens as its own side effect, so it must have been called)", importer.splitCalls)
	}
	if importer.calls != 0 {
		t.Fatalf("BulkImport calls = %d, want 0", importer.calls)
	}
	if got := dst.Keys(); len(got) != 0 {
		t.Fatalf("dst.Keys() = %v, want no staged files when the post-AddTableSplits re-verification fails", got)
	}
}

func TestPromoteReconcilesSplitsThenStagesThenSubmitsForMultiTabletManifest(t *testing.T) {
	src := memory.New()
	manifest := twoTabletManifest()
	manifest.RFiles[0].DestinationPath = "events/t-0000/F0001.rf"
	manifest.RFiles[1].DestinationPath = "events/t-0001/F0002.rf"
	populateManifestRFiles(t, src, manifest)

	dst := memory.New()
	importer := &fakePromoter{}
	mapping, err := Promote(context.Background(), src, manifest, dst, "hdfs://nn/bulk/events-1", importer, "events", Options{})
	if err != nil {
		t.Fatalf("Promote(two-tablet manifest) = %v, want success", err)
	}
	if len(mapping) != 2 {
		t.Fatalf("mapping entries = %d, want 2", len(mapping))
	}
	if importer.splitCalls != 1 {
		t.Fatalf("AddTableSplitsForTable calls = %d, want 1", importer.splitCalls)
	}
	if importer.splitTable != "events" {
		t.Fatalf("AddTableSplitsForTable table = %q, want %q", importer.splitTable, "events")
	}
	if importer.splitTableID != "stable-table-id" {
		t.Fatalf("AddTableSplitsForTable table.ID = %q, want %q (the pinnedTableID captured via ResolveTableID must be passed straight into AddTableSplitsForTable, not just checked separately later)", importer.splitTableID, "stable-table-id")
	}
	wantSplits := [][]byte{[]byte("g")}
	if !reflect.DeepEqual(importer.splitRows, wantSplits) {
		t.Fatalf("AddTableSplitsForTable rows = %#v, want %#v", importer.splitRows, wantSplits)
	}
	if importer.listSplitsCalls != 1 {
		t.Fatalf("ListTableSplits calls = %d, want 1 (Promote must verify reconciled splits before staging)", importer.listSplitsCalls)
	}
	if importer.calls != 1 {
		t.Fatalf("BulkImport calls = %d, want 1", importer.calls)
	}
	for _, name := range []string{"F0001.rf", "F0002.rf"} {
		if _, err := dst.Open(context.Background(), "hdfs://nn/bulk/events-1/"+name); err != nil {
			t.Fatalf("expected staged file %s: %v", name, err)
		}
	}
}

func TestPromoteSkipsAddTableSplitsForSingleTabletManifest(t *testing.T) {
	src := memory.New()
	data, file := testRFile(t, 0, "export/events/t-0000/F0001.rf", []byte("data"))
	src.Put(file.DestinationPath, data)
	manifest := &engine.RFileExportManifest{
		Version:     engine.RFileExportManifestVersion,
		SourceTable: "events",
		Tablets:     []engine.RFileExportTablet{{Index: 0}},
		RFiles:      []engine.RFileExportFile{file},
	}
	dst := memory.New()
	importer := &fakePromoter{}
	if _, err := Promote(context.Background(), src, manifest, dst, "/bulk/events-1", importer, "events", Options{}); err != nil {
		t.Fatal(err)
	}
	if importer.splitCalls != 0 {
		t.Fatalf("AddTableSplitsForTable calls = %d, want 0 for a single-tablet manifest", importer.splitCalls)
	}
	if importer.listSplitsCalls != 0 {
		t.Fatalf("ListTableSplits calls = %d, want 0 for a single-tablet manifest (no splits required, nothing to verify)", importer.listSplitsCalls)
	}
}

func TestPromoteAbortsBeforeStagingWhenAddTableSplitsFails(t *testing.T) {
	src := memory.New()
	manifest := twoTabletManifest()
	manifest.RFiles[0].DestinationPath = "events/t-0000/F0001.rf"
	manifest.RFiles[1].DestinationPath = "events/t-0001/F0002.rf"
	populateManifestRFiles(t, src, manifest)

	dst := memory.New()
	importer := &fakePromoter{splitErr: accumulo.ErrTableOffline}
	if _, err := Promote(context.Background(), src, manifest, dst, "/bulk/events-1", importer, "events", Options{}); !errors.Is(err, accumulo.ErrTableOffline) {
		t.Fatalf("Promote error = %v, want %v", err, accumulo.ErrTableOffline)
	}
	if importer.splitCalls != 1 {
		t.Fatalf("AddTableSplitsForTable calls = %d, want 1", importer.splitCalls)
	}
	if importer.calls != 0 {
		t.Fatalf("BulkImport calls = %d, want 0 (AddTableSplits failure must abort before submission)", importer.calls)
	}
	if got := dst.Keys(); len(got) != 0 {
		t.Fatalf("dst.Keys() = %v, want no staged files when AddTableSplits fails before staging", got)
	}
}

func TestPromoteEndToEndThreeTabletSuccess(t *testing.T) {
	src := memory.New()
	manifest := threeTabletManifest()
	populateManifestRFiles(t, src, manifest)

	dst := memory.New()
	importer := &fakePromoter{}
	mapping, err := Promote(context.Background(), src, manifest, dst, "hdfs://nn/bulk/events-1", importer, "events", Options{})
	if err != nil {
		t.Fatalf("Promote(three-tablet manifest) = %v, want success", err)
	}
	if len(mapping) != 3 {
		t.Fatalf("mapping entries = %d, want 3", len(mapping))
	}
	wantSplits := [][]byte{[]byte("d"), []byte("m")}
	if !reflect.DeepEqual(importer.splitRows, wantSplits) {
		t.Fatalf("AddTableSplitsForTable rows = %#v, want %#v", importer.splitRows, wantSplits)
	}
	if importer.calls != 1 {
		t.Fatalf("BulkImport calls = %d, want 1", importer.calls)
	}
	for i, name := range []string{"F0001.rf", "F0002.rf", "F0003.rf"} {
		staged, err := storage.ReadAll(context.Background(), dst, "hdfs://nn/bulk/events-1/"+name)
		if err != nil {
			t.Fatalf("expected staged file %s: %v", name, err)
		}
		before, err := storage.ReadAll(context.Background(), src, manifest.RFiles[i].DestinationPath)
		if err != nil {
			t.Fatalf("read source file %s: %v", name, err)
		}
		if !reflect.DeepEqual(staged, before) {
			t.Fatalf("promoted file %s differs from source: got %x want %x", name, staged, before)
		}
	}
}

// populatedThreeTabletManifest returns threeTabletManifest with every
// RFile's size/hash filled in and staged into src, so a test can call
// Promote against it directly without repeating the fixture setup
// TestPromoteEndToEndThreeTabletSuccess already establishes.
func populatedThreeTabletManifest(t *testing.T, src *memory.Backend) *engine.RFileExportManifest {
	t.Helper()
	manifest := threeTabletManifest()
	populateManifestRFiles(t, src, manifest)
	return manifest
}

// TestPromoteRejectsExtraDestinationSplitBeforeLastRequiredRow is the
// regression test for the exact counter-example that broke
// BuildLoadMapping's widening rule (see docs/promotion.md §3.2/§5): a
// destination that already has an extra, unrelated split strictly
// before the manifest's required splits. threeTabletManifest requires
// splits at "d" and "m"; a pre-existing split at "c" (< "d") forces
// Accumulo's real PrepBulkImport.validateLoadMapping walk past the one
// tablet in the whole table whose real prevEndRow is nil after
// resolving the first entry, so the second entry's widened
// prevEndRow=nil can never re-match — a spurious "concurrent merge"
// rejection that has nothing to do with an actual concurrent merge.
// Promote must catch this itself and fail closed before ever staging or
// submitting anything.
func TestPromoteRejectsExtraDestinationSplitBeforeLastRequiredRow(t *testing.T) {
	src := memory.New()
	manifest := populatedThreeTabletManifest(t, src)
	dst := memory.New()
	importer := &fakePromoter{
		listSplitsSet:      true,
		listSplitsOverride: [][]byte{[]byte("c"), []byte("d"), []byte("m")},
	}
	_, err := Promote(context.Background(), src, manifest, dst, "hdfs://nn/bulk/events-1", importer, "events", Options{})
	if err == nil {
		t.Fatal("Promote with an unexpected extra destination split before the last required row = nil error, want error")
	}
	if !strings.Contains(err.Error(), `"c"`) {
		t.Fatalf("error %q does not name the offending split row", err.Error())
	}
	if importer.splitCalls != 1 {
		t.Fatalf("AddTableSplitsForTable calls = %d, want 1 (reconciliation still runs before verification)", importer.splitCalls)
	}
	if importer.listSplitsCalls != 1 {
		t.Fatalf("ListTableSplits calls = %d, want 1", importer.listSplitsCalls)
	}
	if importer.calls != 0 {
		t.Fatalf("BulkImport calls = %d, want 0 (must fail closed before submission)", importer.calls)
	}
	if got := dst.Keys(); len(got) != 0 {
		t.Fatalf("dst.Keys() = %v, want no staged files when the extra-split check fails before staging", got)
	}
}

// TestPromoteRejectsWhenDestinationTableChangesIdentityDuringStaging is
// the regression test for round 10's table-identity finding: tableName
// resolves to a different real table by the time Promote is about to
// call BulkImport than it did when Promote pinned it before
// AddTableSplits, simulating an external actor deleting and recreating
// the destination under the same name while StageBulkDir's copying was
// in flight. Promote must fail closed and never submit the bulk import
// against what is now a different table than the one splits were
// reconciled against.
func TestPromoteRejectsWhenDestinationTableChangesIdentityDuringStaging(t *testing.T) {
	src := memory.New()
	manifest := twoTabletManifest()
	manifest.RFiles[0].DestinationPath = "events/t-0000/F0001.rf"
	manifest.RFiles[1].DestinationPath = "events/t-0001/F0002.rf"
	populateManifestRFiles(t, src, manifest)

	dst := memory.New()
	importer := &fakePromoter{tableID: "original-id"}
	importer.onResolveTableID = func(callNum int) {
		if callNum == 1 {
			// Runs as a side effect of the first ResolveTableID call
			// (Promote's pre-AddTableSplits pin); only the *second*
			// call, immediately before BulkImport, observes this.
			importer.tableID = "recreated-id"
		}
	}
	_, err := Promote(context.Background(), src, manifest, dst, "hdfs://nn/bulk/events-1", importer, "events", Options{})
	if err == nil {
		t.Fatal("Promote with destination table identity changed during staging = nil error, want error")
	}
	if !strings.Contains(err.Error(), "original-id") || !strings.Contains(err.Error(), "recreated-id") {
		t.Fatalf("error %q does not name both the pinned and current table IDs", err.Error())
	}
	if importer.resolveTableIDCalls != 2 {
		t.Fatalf("ResolveTableID calls = %d, want 2 (pre-AddTableSplits pin + pre-BulkImport check)", importer.resolveTableIDCalls)
	}
	if importer.calls != 0 {
		t.Fatalf("BulkImport calls = %d, want 0 (identity check must fail closed before submission)", importer.calls)
	}
	// Staging itself must have already succeeded: the mismatch is only
	// detected after StageBulkDir completes, so the staged files remain
	// on dst even though the promotion as a whole failed.
	for _, name := range []string{"F0001.rf", "F0002.rf"} {
		if _, err := dst.Open(context.Background(), "hdfs://nn/bulk/events-1/"+name); err != nil {
			t.Fatalf("expected staged file %s despite the later identity-check failure: %v", name, err)
		}
	}
}

// TestPromoteRejectsWhenDestinationTableChangesIdentityBeforeAddTableSplits
// is the regression test for round 11's anchored finding: the pin
// Promote captures via ResolveTableID immediately before
// AddTableSplitsForTable must actually reach it and be checked there,
// not merely be threaded through as an unused value. This fake does
// not re-derive accumulo.Connector.AddTableSplitsForTable's own
// resolve-and-compare logic -- that belongs to, and is already
// covered by, the accumulo package's own
// TestAddTableSplitsForTableRejectsMismatchedPinBeforeAnyManagerCall.
// Instead, splitErr simulates AddTableSplitsForTable itself returning
// the accumulo.ErrTableIdentityChanged it would return in that case,
// so this test can focus purely on proving Promote propagates that
// failure and aborts before ever staging a file or calling
// ListTableSplits/BulkImport.
func TestPromoteRejectsWhenDestinationTableChangesIdentityBeforeAddTableSplits(t *testing.T) {
	src := memory.New()
	manifest := twoTabletManifest()
	manifest.RFiles[0].DestinationPath = "events/t-0000/F0001.rf"
	manifest.RFiles[1].DestinationPath = "events/t-0001/F0002.rf"
	populateManifestRFiles(t, src, manifest)

	dst := memory.New()
	wantErr := errors.Join(accumulo.ErrTableIdentityChanged, errors.New(`accumulo: table "events" changed identity (was table ID "original-id", is now "recreated-id") before AddTableSplits`))
	importer := &fakePromoter{tableID: "original-id", splitErr: wantErr}

	_, err := Promote(context.Background(), src, manifest, dst, "hdfs://nn/bulk/events-1", importer, "events", Options{})
	if err == nil {
		t.Fatal("Promote when AddTableSplitsForTable rejects a stale pin = nil error, want error")
	}
	if !errors.Is(err, accumulo.ErrTableIdentityChanged) {
		t.Fatalf("Promote error = %v, want it to wrap accumulo.ErrTableIdentityChanged", err)
	}
	if importer.splitCalls != 1 {
		t.Fatalf("AddTableSplitsForTable calls = %d, want 1", importer.splitCalls)
	}
	if importer.splitTableID != "original-id" {
		t.Fatalf("AddTableSplitsForTable table.ID = %q, want the pin Promote resolved (%q)", importer.splitTableID, "original-id")
	}
	if importer.listSplitsCalls != 0 {
		t.Fatalf("ListTableSplits calls = %d, want 0 (must not run once AddTableSplitsForTable itself already failed)", importer.listSplitsCalls)
	}
	if importer.calls != 0 {
		t.Fatalf("BulkImport calls = %d, want 0", importer.calls)
	}
	// Nothing should have been staged either: AddTableSplitsForTable
	// failing must abort before StageBulkDir ever runs, not merely
	// before BulkImport.
	for _, name := range []string{"F0001.rf", "F0002.rf"} {
		if _, openErr := dst.Open(context.Background(), "hdfs://nn/bulk/events-1/"+name); openErr == nil {
			t.Fatalf("expected %s to not be staged when AddTableSplitsForTable fails before it", name)
		}
	}
}

// TestPromoteRejectsIdentityChangeEvenWithEmptyLoadMapping is the
// regression test for round 11's other suppressed finding: the
// destination-identity check that runs after StageBulkDir must not be
// skippable by the len(mapping) == 0 early return that follows it.
// twoTabletManifest's chain (two declared tablets, boundary "g") is
// used unmodified except for RFiles, which is cleared entirely: with
// no RFiles at all, BuildLoadMapping returns (nil, nil) for every
// caller, including StageBulkDir -- see BuildLoadMapping's own doc
// comment on what "every declared tablet ends up with zero files"
// produces. RequiredDestinationSplits, unlike BuildLoadMapping, derives
// its splits purely from the declared tablet chain shape and is
// unaffected by RFiles being empty, so this manifest still reports one
// required split row ("g"), which still makes Promote resolve and pin
// the destination's table ID and call AddTableSplitsForTable before
// ever reaching StageBulkDir. Before this round's reordering fix,
// Promote's identity check ran only in the branch guarded by
// len(mapping) != 0, so a destination deleted and recreated while
// StageBulkDir ran (here simulated between the pre-AddTableSplits pin
// and the post-staging check, exactly like
// TestPromoteRejectsWhenDestinationTableChangesIdentityDuringStaging)
// would have been silently reported as success: (nil, nil), with no
// indication the splits AddTableSplitsForTable just reconciled belong
// to a table that no longer exists.
func TestPromoteRejectsIdentityChangeEvenWithEmptyLoadMapping(t *testing.T) {
	src := memory.New() // never read: the manifest below declares zero RFiles.
	manifest := twoTabletManifest()
	manifest.RFiles = nil

	dst := memory.New()
	importer := &fakePromoter{tableID: "original-id"}
	importer.onResolveTableID = func(callNum int) {
		if callNum == 1 {
			// Runs once the pre-AddTableSplitsForTable pin has already
			// captured "original-id"; only the second ResolveTableID
			// call, in the post-staging identity check, observes this.
			importer.tableID = "recreated-id"
		}
	}

	mapping, err := Promote(context.Background(), src, manifest, dst, "hdfs://nn/bulk/events-1", importer, "events", Options{})
	if err == nil {
		t.Fatalf("Promote with destination identity changed and an empty load mapping = (%v, nil), want an error", mapping)
	}
	if !strings.Contains(err.Error(), "original-id") || !strings.Contains(err.Error(), "recreated-id") {
		t.Fatalf("error %q does not name both the pinned and current table IDs", err.Error())
	}
	if importer.resolveTableIDCalls != 2 {
		t.Fatalf("ResolveTableID calls = %d, want 2 (pre-AddTableSplitsForTable pin + the post-staging identity check this test covers)", importer.resolveTableIDCalls)
	}
	if importer.splitCalls != 1 {
		t.Fatalf("AddTableSplitsForTable calls = %d, want 1 (splits are still reconciled even though the mapping ends up empty)", importer.splitCalls)
	}
	if importer.calls != 0 {
		t.Fatalf("BulkImport calls = %d, want 0", importer.calls)
	}
}

// TestPromoteAbortsBeforeAddTableSplitsWhenResolveTableIDFails proves
// the pre-AddTableSplits identity pin itself fails closed: if Promote
// cannot even resolve the destination table's current ID, it must not
// go on to reconcile splits against a table it never confirmed still
// exists as expected.
func TestPromoteAbortsBeforeAddTableSplitsWhenResolveTableIDFails(t *testing.T) {
	src := memory.New()
	manifest := twoTabletManifest()
	manifest.RFiles[0].DestinationPath = "events/t-0000/F0001.rf"
	manifest.RFiles[1].DestinationPath = "events/t-0001/F0002.rf"
	populateManifestRFiles(t, src, manifest)

	dst := memory.New()
	importer := &fakePromoter{resolveTableIDErr: accumulo.ErrTableOffline}
	_, err := Promote(context.Background(), src, manifest, dst, "/bulk/events-1", importer, "events", Options{})
	if !errors.Is(err, accumulo.ErrTableOffline) {
		t.Fatalf("Promote error = %v, want %v", err, accumulo.ErrTableOffline)
	}
	if importer.resolveTableIDCalls != 1 {
		t.Fatalf("ResolveTableID calls = %d, want 1", importer.resolveTableIDCalls)
	}
	if importer.splitCalls != 0 {
		t.Fatalf("AddTableSplitsForTable calls = %d, want 0 (ResolveTableID failure must abort before reconciliation)", importer.splitCalls)
	}
	if importer.calls != 0 {
		t.Fatalf("BulkImport calls = %d, want 0", importer.calls)
	}
	if got := dst.Keys(); len(got) != 0 {
		t.Fatalf("dst.Keys() = %v, want no staged files when ResolveTableID fails before AddTableSplits", got)
	}
}

// TestPromoteRejectsReadOnlyDestinationBeforeAddTableSplits is the
// regression test for round 10's writability finding: a multi-tablet
// manifest against a read-only dst must be rejected by
// validatePromotionDestination before AddTableSplits ever mutates the
// real destination table's splits, not left to fail later, deep inside
// StageBulkDir's first storage.Copy call.
func TestPromoteRejectsReadOnlyDestinationBeforeAddTableSplits(t *testing.T) {
	src := memory.New()
	src.Put("events/t-0000/F0001.rf", []byte("a"))
	src.Put("events/t-0001/F0002.rf", []byte("b"))
	manifest := twoTabletManifest()
	manifest.RFiles[0].DestinationPath = "events/t-0000/F0001.rf"
	manifest.RFiles[0].Size = 1
	manifest.RFiles[0].SHA256 = "ca978112ca1bbdcafac231b39a23dc4da786eff8147c4e72b9807785afee48bb"
	manifest.RFiles[1].DestinationPath = "events/t-0001/F0002.rf"
	manifest.RFiles[1].Size = 1
	manifest.RFiles[1].SHA256 = "3e23e8160039594a33894f6564e1b1348bbd7a0088d42c4acb73eeaed59c009d"

	dst := readOnlyBackend{inner: memory.New()}
	importer := &fakePromoter{}
	_, err := Promote(context.Background(), src, manifest, dst, "/bulk/events-1", importer, "events", Options{})
	if !errors.Is(err, storage.ErrReadOnly) {
		t.Fatalf("Promote error = %v, want %v", err, storage.ErrReadOnly)
	}
	if importer.splitCalls != 0 {
		t.Fatalf("AddTableSplitsForTable calls = %d, want 0 (read-only destination must be rejected before reconciliation)", importer.splitCalls)
	}
	if importer.resolveTableIDCalls != 0 {
		t.Fatalf("ResolveTableID calls = %d, want 0 (destination validation runs first)", importer.resolveTableIDCalls)
	}
	if importer.calls != 0 {
		t.Fatalf("BulkImport calls = %d, want 0", importer.calls)
	}
}

// TestPromoteAllowsTrailingDestinationSplitAfterLastRequiredRow proves
// the check above is not over-conservative: a trailing extra split
// strictly after the last required row falls entirely inside the
// manifest's final, always-unbounded entry, which can legitimately
// absorb any number of additional real trailing tablets, so it must not
// be rejected.
func TestPromoteAllowsTrailingDestinationSplitAfterLastRequiredRow(t *testing.T) {
	src := memory.New()
	manifest := populatedThreeTabletManifest(t, src)
	dst := memory.New()
	importer := &fakePromoter{
		listSplitsSet:      true,
		listSplitsOverride: [][]byte{[]byte("d"), []byte("m"), []byte("z")},
	}
	mapping, err := Promote(context.Background(), src, manifest, dst, "hdfs://nn/bulk/events-1", importer, "events", Options{})
	if err != nil {
		t.Fatalf("Promote with a trailing extra split after the last required row = %v, want success", err)
	}
	if len(mapping) != 3 {
		t.Fatalf("mapping entries = %d, want 3", len(mapping))
	}
	if importer.calls != 1 {
		t.Fatalf("BulkImport calls = %d, want 1", importer.calls)
	}
}

// TestPromoteRejectsWhenDestinationIsMissingARequiredSplit exercises the
// defensive "too few" side of the same check: it should never happen in
// practice (a successful AddTableSplits is supposed to guarantee the
// required rows exist), but if a concurrent merge removes a
// just-reconciled split in the narrow window before ListTableSplits
// observes it, Promote must fail closed rather than proceed with a
// mapping that no longer matches the destination's real splits.
func TestPromoteRejectsWhenDestinationIsMissingARequiredSplit(t *testing.T) {
	src := memory.New()
	manifest := populatedThreeTabletManifest(t, src)
	dst := memory.New()
	importer := &fakePromoter{
		listSplitsSet:      true,
		listSplitsOverride: [][]byte{[]byte("d")},
	}
	_, err := Promote(context.Background(), src, manifest, dst, "hdfs://nn/bulk/events-1", importer, "events", Options{})
	if err == nil {
		t.Fatal("Promote with a destination missing a required split = nil error, want error")
	}
	if importer.calls != 0 {
		t.Fatalf("BulkImport calls = %d, want 0 (must fail closed before submission)", importer.calls)
	}
	if got := dst.Keys(); len(got) != 0 {
		t.Fatalf("dst.Keys() = %v, want no staged files when the extra-split check fails before staging", got)
	}
}
