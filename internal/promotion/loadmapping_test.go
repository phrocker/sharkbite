package promotion

import (
	"context"
	"encoding/base64"
	"reflect"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
	"github.com/phrocker/shoal-oss/internal/tablet"
)

func strPtr(s string) *string { return &s }

func singleTabletManifest() *engine.RFileExportManifest {
	return &engine.RFileExportManifest{
		Version:     engine.RFileExportManifestVersion,
		SourceTable: "events",
		Tablets:     []engine.RFileExportTablet{{Index: 0}},
		RFiles: []engine.RFileExportFile{
			{TabletIndex: 0, DestinationPath: "events/t-0000/F0001.rf", Size: 100},
			{TabletIndex: 0, DestinationPath: "events/t-0000/F0002.rf", Size: 50},
		},
	}
}

// twoTabletManifest is the smallest possible split-bearing manifest: two
// tablets sharing the boundary row "g". Widening always collapses a
// 2-tablet chain's second entry to fully unbounded (there is no tablet
// before index 0 to anchor PrevEndRow against), so this fixture doubles as
// the "collapse" edge case; see threeTabletManifest/fourTabletManifest for
// chains long enough to exercise a genuinely bounded PrevEndRow.
func twoTabletManifest() *engine.RFileExportManifest {
	return &engine.RFileExportManifest{
		Version:     engine.RFileExportManifestVersion,
		SourceTable: "events",
		Tablets: []engine.RFileExportTablet{
			{Index: 0, EndRow: strPtr("g")},
			{Index: 1, StartRow: strPtr("g")},
		},
		RFiles: []engine.RFileExportFile{
			{TabletIndex: 0, DestinationPath: "events/t-0000/F0001.rf", Size: 100},
			{TabletIndex: 1, DestinationPath: "events/t-0001/F0002.rf", Size: 50},
		},
	}
}

// threeTabletManifest declares tablets at boundaries "d" and "m". Its
// middle tablet (index 1) still widens to fully unbounded (PrevEndRow
// always anchors to index 0's own StartRow, which is always nil), but
// index 2 gets a genuinely bounded PrevEndRow of "d" — the first index in
// any chain where widening is not indistinguishable from "no bound at
// all".
func threeTabletManifest() *engine.RFileExportManifest {
	return &engine.RFileExportManifest{
		Version:     engine.RFileExportManifestVersion,
		SourceTable: "events",
		Tablets: []engine.RFileExportTablet{
			{Index: 0, EndRow: strPtr("d")},
			{Index: 1, StartRow: strPtr("d"), EndRow: strPtr("m")},
			{Index: 2, StartRow: strPtr("m")},
		},
		RFiles: []engine.RFileExportFile{
			{TabletIndex: 0, DestinationPath: "events/t-0000/F0001.rf", Size: 100},
			{TabletIndex: 1, DestinationPath: "events/t-0001/F0002.rf", Size: 50},
			{TabletIndex: 2, DestinationPath: "events/t-0002/F0003.rf", Size: 25},
		},
	}
}

// fourTabletManifest declares tablets at boundaries "b", "k", and "r",
// giving every resolved extent a distinct, hand-checkable shape: index 0
// bounded on the right only, index 1 collapsed (always unbounded), index 2
// genuinely bounded on both sides, index 3 bounded on the left only.
func fourTabletManifest() *engine.RFileExportManifest {
	return &engine.RFileExportManifest{
		Version:     engine.RFileExportManifestVersion,
		SourceTable: "events",
		Tablets: []engine.RFileExportTablet{
			{Index: 0, EndRow: strPtr("b")},
			{Index: 1, StartRow: strPtr("b"), EndRow: strPtr("k")},
			{Index: 2, StartRow: strPtr("k"), EndRow: strPtr("r")},
			{Index: 3, StartRow: strPtr("r")},
		},
		RFiles: []engine.RFileExportFile{
			{TabletIndex: 0, DestinationPath: "events/t-0000/F0001.rf", Size: 10},
			{TabletIndex: 1, DestinationPath: "events/t-0001/F0002.rf", Size: 20},
			{TabletIndex: 2, DestinationPath: "events/t-0002/F0003.rf", Size: 30},
			{TabletIndex: 3, DestinationPath: "events/t-0003/F0004.rf", Size: 40},
		},
	}
}

// extentEqual reports whether two KeyExtents describe the same
// (PrevEndRow, EndRow] range, comparing nil against nil (not against an
// empty-but-non-nil slice) so widened-to-unbounded assertions are exact.
func extentEqual(t *testing.T, got, want KeyExtent) bool {
	t.Helper()
	return reflect.DeepEqual(got.PrevEndRow, want.PrevEndRow) && reflect.DeepEqual(got.EndRow, want.EndRow)
}

func TestBuildLoadMappingSingleTabletUsesUnboundedExtent(t *testing.T) {
	mapping, err := BuildLoadMapping(singleTabletManifest())
	if err != nil {
		t.Fatal(err)
	}
	if len(mapping) != 1 {
		t.Fatalf("mapping entries = %d, want 1", len(mapping))
	}
	if mapping[0].Tablet.EndRow != nil || mapping[0].Tablet.PrevEndRow != nil {
		t.Fatalf("single-tablet extent = %#v, want fully unbounded", mapping[0].Tablet)
	}
	if len(mapping[0].Files) != 2 {
		t.Fatalf("single-tablet files = %#v, want 2", mapping[0].Files)
	}
	if mapping[0].Files[0].Name != "F0001.rf" || mapping[0].Files[1].Name != "F0002.rf" {
		t.Fatalf("single-tablet files not name-sorted: %#v", mapping[0].Files)
	}
	if mapping[0].Files[0].EstSize != 100 || mapping[0].Files[1].EstSize != 50 {
		t.Fatalf("single-tablet file sizes = %#v", mapping[0].Files)
	}
}

func TestBuildLoadMappingDefaultsSingleTabletForLegacyManifest(t *testing.T) {
	manifest := &engine.RFileExportManifest{
		Version:     engine.RFileExportManifestVersion,
		SourceTable: "events",
		RFiles: []engine.RFileExportFile{
			{TabletIndex: 0, DestinationPath: "events/t-0000/F0001.rf", Size: 10},
			{TabletIndex: 0, DestinationPath: "events/t-0000/F0002.rf", Size: 20},
		},
	}
	mapping, err := BuildLoadMapping(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(mapping) != 1 {
		t.Fatalf("mapping entries = %d, want 1", len(mapping))
	}
	if mapping[0].Tablet.EndRow != nil || mapping[0].Tablet.PrevEndRow != nil {
		t.Fatalf("legacy default tablet = %#v, want fully unbounded", mapping[0].Tablet)
	}
	if len(mapping[0].Files) != 2 {
		t.Fatalf("legacy default files = %#v, want 2", mapping[0].Files)
	}
}

func TestBuildLoadMappingRejectsNilManifest(t *testing.T) {
	if _, err := BuildLoadMapping(nil); err == nil {
		t.Fatal("BuildLoadMapping(nil) = nil error, want error")
	}

}

// TestBuildLoadMappingRejectsUnsupportedManifestVersion is the direct
// unit-level counterpart to
// TestPromoteRejectsUnsupportedManifestVersionBeforeAddTableSplits in
// promote_test.go: it exercises resolveManifestTablets's version check
// through BuildLoadMapping itself, on an otherwise perfectly valid
// multi-tablet manifest, rather than only through Promote's higher-level
// integration path.
func TestBuildLoadMappingRejectsUnsupportedManifestVersion(t *testing.T) {
	manifest := twoTabletManifest()
	manifest.Version = engine.RFileExportManifestVersion + 1
	if _, err := BuildLoadMapping(manifest); err == nil {
		t.Fatal("BuildLoadMapping with an unsupported manifest version = nil error, want error")
	}
}

func TestBuildLoadMappingDedupesRepeatedDestinationPath(t *testing.T) {
	manifest := &engine.RFileExportManifest{
		Version:     engine.RFileExportManifestVersion,
		SourceTable: "events",
		Tablets:     []engine.RFileExportTablet{{Index: 0}},
		RFiles: []engine.RFileExportFile{
			{TabletIndex: 0, DestinationPath: "events/t-0000/F0001.rf", Size: 10},
			{TabletIndex: 0, DestinationPath: "events/t-0000/F0001.rf", Size: 10},
			{TabletIndex: 0, DestinationPath: "events/t-0000/F0002.rf", Size: 20},
		},
	}
	mapping, err := BuildLoadMapping(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(mapping) != 1 {
		t.Fatalf("mapping entries = %d, want 1", len(mapping))
	}
	if len(mapping[0].Files) != 2 {
		t.Fatalf("files = %#v, want 2 (F0001.rf deduped, F0002.rf kept)", mapping[0].Files)
	}
}

func TestBuildLoadMappingAcceptsTwoTabletManifestWithWidenedExtents(t *testing.T) {
	mapping, err := BuildLoadMapping(twoTabletManifest())
	if err != nil {
		t.Fatalf("BuildLoadMapping(two-tablet manifest) = %v, want success", err)
	}
	if len(mapping) != 2 {
		t.Fatalf("mapping entries = %d, want 2: %#v", len(mapping), mapping)
	}
	// Index 0 keeps its own EndRow and is unbounded on the left (nothing
	// precedes the first tablet).
	if !extentEqual(t, mapping[0].Tablet, KeyExtent{EndRow: []byte("g")}) {
		t.Fatalf("mapping[0].Tablet = %#v, want (nil, %q]", mapping[0].Tablet, "g")
	}
	// Index 1 widens PrevEndRow to index 0's own StartRow, which is
	// always nil: a 2-tablet chain's second entry always collapses to
	// fully unbounded. This is the documented, unavoidable "2-tablet
	// collapse" edge case, not a bug.
	if !extentEqual(t, mapping[1].Tablet, KeyExtent{}) {
		t.Fatalf("mapping[1].Tablet = %#v, want fully unbounded", mapping[1].Tablet)
	}
	if len(mapping[0].Files) != 1 || mapping[0].Files[0].Name != "F0001.rf" {
		t.Fatalf("mapping[0].Files = %#v, want [F0001.rf]", mapping[0].Files)
	}
	if len(mapping[1].Files) != 1 || mapping[1].Files[0].Name != "F0002.rf" {
		t.Fatalf("mapping[1].Files = %#v, want [F0002.rf]", mapping[1].Files)
	}
}

func TestBuildLoadMappingAcceptsThreeTabletManifestWithWidenedExtents(t *testing.T) {
	mapping, err := BuildLoadMapping(threeTabletManifest())
	if err != nil {
		t.Fatalf("BuildLoadMapping(three-tablet manifest) = %v, want success", err)
	}
	if len(mapping) != 3 {
		t.Fatalf("mapping entries = %d, want 3: %#v", len(mapping), mapping)
	}
	wantExtents := []KeyExtent{
		{EndRow: []byte("d")},     // index 0: (nil, "d"]
		{EndRow: []byte("m")},     // index 1: (nil, "m"] -- always collapses
		{PrevEndRow: []byte("d")}, // index 2: ("d", nil] -- genuinely bounded
	}
	for i, want := range wantExtents {
		if !extentEqual(t, mapping[i].Tablet, want) {
			t.Fatalf("mapping[%d].Tablet = %#v, want %#v", i, mapping[i].Tablet, want)
		}
	}
}

func TestBuildLoadMappingAcceptsFourTabletManifestWithWidenedExtents(t *testing.T) {
	mapping, err := BuildLoadMapping(fourTabletManifest())
	if err != nil {
		t.Fatalf("BuildLoadMapping(four-tablet manifest) = %v, want success", err)
	}
	if len(mapping) != 4 {
		t.Fatalf("mapping entries = %d, want 4: %#v", len(mapping), mapping)
	}
	wantExtents := []KeyExtent{
		{EndRow: []byte("b")},                          // index 0: (nil, "b"]
		{EndRow: []byte("k")},                          // index 1: (nil, "k"] -- always collapses
		{PrevEndRow: []byte("b"), EndRow: []byte("r")}, // index 2: ("b", "r"]
		{PrevEndRow: []byte("k")},                      // index 3: ("k", nil]
	}
	for i, want := range wantExtents {
		if !extentEqual(t, mapping[i].Tablet, want) {
			t.Fatalf("mapping[%d].Tablet = %#v, want %#v", i, mapping[i].Tablet, want)
		}
	}
}

func TestBuildLoadMappingToleratesOutOfOrderTabletSlice(t *testing.T) {
	manifest := threeTabletManifest()
	// Reverse the physical slice order; .Index still identifies each
	// tablet, so resolution must be robust to slice order, not dependent
	// on it.
	manifest.Tablets = []engine.RFileExportTablet{
		manifest.Tablets[2], manifest.Tablets[0], manifest.Tablets[1],
	}
	mapping, err := BuildLoadMapping(manifest)
	if err != nil {
		t.Fatalf("BuildLoadMapping(out-of-order tablets) = %v, want success", err)
	}
	ordered, err := BuildLoadMapping(threeTabletManifest())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(mapping, ordered) {
		t.Fatalf("out-of-order mapping = %#v, want identical to in-order mapping %#v", mapping, ordered)
	}
}

func TestBuildLoadMappingSkipsEmptyTabletsInMultiTabletChain(t *testing.T) {
	manifest := threeTabletManifest()
	// Drop every RFile for index 1: that tablet contributed no files, and
	// should be silently omitted from the returned LoadMapping, while
	// RequiredDestinationSplits (checked separately) still reports both
	// chain boundaries regardless.
	var kept []engine.RFileExportFile
	for _, rf := range manifest.RFiles {
		if rf.TabletIndex == 1 {
			continue
		}
		kept = append(kept, rf)
	}
	manifest.RFiles = kept

	mapping, err := BuildLoadMapping(manifest)
	if err != nil {
		t.Fatalf("BuildLoadMapping(manifest with empty middle tablet) = %v, want success", err)
	}
	if len(mapping) != 2 {
		t.Fatalf("mapping entries = %d, want 2 (index 1 skipped): %#v", len(mapping), mapping)
	}
	if !extentEqual(t, mapping[0].Tablet, KeyExtent{EndRow: []byte("d")}) {
		t.Fatalf("mapping[0].Tablet = %#v, want (nil, %q]", mapping[0].Tablet, "d")
	}
	if !extentEqual(t, mapping[1].Tablet, KeyExtent{PrevEndRow: []byte("d")}) {
		t.Fatalf("mapping[1].Tablet = %#v, want (%q, nil]", mapping[1].Tablet, "d")
	}
}

// TestRequiredDestinationSplitsIgnoresWhichTabletsHaveFiles proves the
// claim TestBuildLoadMappingSkipsEmptyTabletsInMultiTabletChain's comment
// makes but does not itself check: RequiredDestinationSplits operates on
// the manifest's declared Tablets chain only, not on which tablets ended
// up with RFiles, so dropping index 1's only file does not change the
// required split set at all. This is what lets Promote reconcile the
// destination's splits the same way regardless of which tablets in a
// multi-tablet manifest happen to be empty.
func TestRequiredDestinationSplitsIgnoresWhichTabletsHaveFiles(t *testing.T) {
	full := threeTabletManifest()
	fullSplits, err := RequiredDestinationSplits(full)
	if err != nil {
		t.Fatal(err)
	}

	emptyMiddle := threeTabletManifest()
	var kept []engine.RFileExportFile
	for _, rf := range emptyMiddle.RFiles {
		if rf.TabletIndex == 1 {
			continue
		}
		kept = append(kept, rf)
	}
	emptyMiddle.RFiles = kept

	gotSplits, err := RequiredDestinationSplits(emptyMiddle)
	if err != nil {
		t.Fatalf("RequiredDestinationSplits(manifest with empty middle tablet) = %v, want success", err)
	}
	if !reflect.DeepEqual(gotSplits, fullSplits) {
		t.Fatalf("RequiredDestinationSplits = %#v, want unchanged %#v regardless of which tablet is empty", gotSplits, fullSplits)
	}
	wantSplits := [][]byte{[]byte("d"), []byte("m")}
	if !reflect.DeepEqual(gotSplits, wantSplits) {
		t.Fatalf("RequiredDestinationSplits = %#v, want %#v", gotSplits, wantSplits)
	}
}

func TestBuildLoadMappingRejectsDuplicateTabletIndex(t *testing.T) {
	manifest := twoTabletManifest()
	manifest.Tablets[1].Index = 0 // duplicate of tablet 0's index
	if _, err := BuildLoadMapping(manifest); err == nil {
		t.Fatal("BuildLoadMapping with duplicate tablet index = nil error, want error")
	}
}

func TestBuildLoadMappingRejectsOutOfRangeTabletIndex(t *testing.T) {
	manifest := twoTabletManifest()
	manifest.Tablets[1].Index = 5 // out of range for a 2-tablet manifest
	if _, err := BuildLoadMapping(manifest); err == nil {
		t.Fatal("BuildLoadMapping with out-of-range tablet index = nil error, want error")
	}
}

func TestBuildLoadMappingRejectsNonNilFirstStartRow(t *testing.T) {
	manifest := twoTabletManifest()
	manifest.Tablets[0].StartRow = strPtr("a") // first tablet must start at negative infinity
	if _, err := BuildLoadMapping(manifest); err == nil {
		t.Fatal("BuildLoadMapping with non-nil first StartRow = nil error, want error")
	}
}

func TestBuildLoadMappingRejectsNonNilLastEndRow(t *testing.T) {
	manifest := twoTabletManifest()
	manifest.Tablets[1].EndRow = strPtr("z") // last tablet must end at positive infinity
	if _, err := BuildLoadMapping(manifest); err == nil {
		t.Fatal("BuildLoadMapping with non-nil last EndRow = nil error, want error")
	}
}

func TestBuildLoadMappingRejectsMissingMiddleStartRow(t *testing.T) {
	manifest := threeTabletManifest()
	manifest.Tablets[1].StartRow = nil // middle tablet must declare both boundaries
	if _, err := BuildLoadMapping(manifest); err == nil {
		t.Fatal("BuildLoadMapping with missing middle StartRow = nil error, want error")
	}
}

func TestBuildLoadMappingRejectsMissingMiddleEndRow(t *testing.T) {
	manifest := threeTabletManifest()
	manifest.Tablets[1].EndRow = nil // middle tablet must declare both boundaries
	if _, err := BuildLoadMapping(manifest); err == nil {
		t.Fatal("BuildLoadMapping with missing middle EndRow = nil error, want error")
	}
}

func TestBuildLoadMappingRejectsChainGap(t *testing.T) {
	manifest := threeTabletManifest()
	// Widen tablet 1 so it no longer starts exactly where tablet 0 ends:
	// a gap between "d" and "e" that no tablet covers.
	manifest.Tablets[1].StartRow = strPtr("e")
	if _, err := BuildLoadMapping(manifest); err == nil {
		t.Fatal("BuildLoadMapping with a chain gap = nil error, want error")
	}
}

func TestBuildLoadMappingRejectsChainOverlap(t *testing.T) {
	manifest := threeTabletManifest()
	// Move tablet 1's start before tablet 0's own end: an overlap between
	// "c" and "d" that both tablet 0 and tablet 1 would claim.
	manifest.Tablets[1].StartRow = strPtr("c")
	if _, err := BuildLoadMapping(manifest); err == nil {
		t.Fatal("BuildLoadMapping with a chain overlap = nil error, want error")
	}
}

func TestBuildLoadMappingRejectsDegenerateTabletRange(t *testing.T) {
	manifest := threeTabletManifest()
	// Tablet 1's StartRow >= EndRow: an inverted/degenerate range.
	manifest.Tablets[1].StartRow = strPtr("m")
	manifest.Tablets[1].EndRow = strPtr("d")
	if _, err := BuildLoadMapping(manifest); err == nil {
		t.Fatal("BuildLoadMapping with a degenerate tablet range = nil error, want error")
	}
}

// TestBuildLoadMappingRejectsEmptyBoundaryRow covers a chain shape the
// degenerate-range check above cannot see: it only compares a tablet's
// own StartRow against its own EndRow, and only when *both* are non-nil.
// Tablet 0's StartRow and the last tablet's EndRow are always nil by the
// chain-shape requirement itself, so an empty (non-nil, zero-length)
// value on the *other* boundary of one of those two tablets -- tablet
// 0's EndRow, or the last tablet's StartRow -- never has a same-tablet
// partner to compare against and so never tripped the degenerate check.
// A 2-tablet manifest makes this concrete: tablet 0's StartRow and
// tablet 1's EndRow are both nil unconditionally, so the only boundary
// either tablet carries is the single row they share, and if that
// shared row is "" neither tablet's own degenerate check ever fires --
// this manifest would otherwise reach AddTableSplits, which rejects a
// zero-length split row itself (accumulo.Connector.AddTableSplits's
// normalizeSplitRows), but for a reason expressed in that lower layer's
// own terms instead of this package's manifest-shape validation.
func TestBuildLoadMappingRejectsEmptyBoundaryRow(t *testing.T) {
	manifest := twoTabletManifest()
	manifest.Tablets[0].EndRow = strPtr("")
	manifest.Tablets[1].StartRow = strPtr("")
	if _, err := BuildLoadMapping(manifest); err == nil {
		t.Fatal("BuildLoadMapping with an empty (non-nil) shared boundary row = nil error, want error")
	}
}

func TestBuildLoadMappingRejectsUndeclaredTabletIndexMultiTablet(t *testing.T) {
	manifest := threeTabletManifest()
	manifest.RFiles = append(manifest.RFiles, engine.RFileExportFile{
		TabletIndex:     7,
		DestinationPath: "events/t-0007/F0099.rf",
		Size:            5,
	})
	if _, err := BuildLoadMapping(manifest); err == nil {
		t.Fatal("BuildLoadMapping with undeclared multi-tablet index = nil error, want error")
	}
}

func TestBuildLoadMappingRejectsDuplicateDestinationPathAcrossTabletsMultiTablet(t *testing.T) {
	manifest := threeTabletManifest()
	manifest.RFiles = append(manifest.RFiles, engine.RFileExportFile{
		TabletIndex:     2,
		DestinationPath: manifest.RFiles[0].DestinationPath, // already declared under index 0
		Size:            manifest.RFiles[0].Size,
	})
	if _, err := BuildLoadMapping(manifest); err == nil {
		t.Fatal("BuildLoadMapping with a DestinationPath repeated under a different tablet index = nil error, want error")
	}
}

func TestRequiredDestinationSplitsNilForSingleTabletManifest(t *testing.T) {
	splits, err := RequiredDestinationSplits(singleTabletManifest())
	if err != nil {
		t.Fatal(err)
	}
	if splits != nil {
		t.Fatalf("RequiredDestinationSplits(single-tablet) = %#v, want nil", splits)
	}
}

func TestRequiredDestinationSplitsNilForLegacyManifest(t *testing.T) {
	manifest := &engine.RFileExportManifest{
		Version:     engine.RFileExportManifestVersion,
		SourceTable: "events",
		RFiles: []engine.RFileExportFile{
			{TabletIndex: 0, DestinationPath: "events/t-0000/F0001.rf", Size: 10},
		},
	}
	splits, err := RequiredDestinationSplits(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if splits != nil {
		t.Fatalf("RequiredDestinationSplits(legacy manifest) = %#v, want nil", splits)
	}
}

func TestRequiredDestinationSplitsForTwoTabletManifest(t *testing.T) {
	splits, err := RequiredDestinationSplits(twoTabletManifest())
	if err != nil {
		t.Fatal(err)
	}
	want := [][]byte{[]byte("g")}
	if !reflect.DeepEqual(splits, want) {
		t.Fatalf("RequiredDestinationSplits(two-tablet) = %#v, want %#v", splits, want)
	}
}

func TestRequiredDestinationSplitsForThreeTabletManifest(t *testing.T) {
	splits, err := RequiredDestinationSplits(threeTabletManifest())
	if err != nil {
		t.Fatal(err)
	}
	want := [][]byte{[]byte("d"), []byte("m")}
	if !reflect.DeepEqual(splits, want) {
		t.Fatalf("RequiredDestinationSplits(three-tablet) = %#v, want %#v", splits, want)
	}
}

func TestRequiredDestinationSplitsForFourTabletManifest(t *testing.T) {
	splits, err := RequiredDestinationSplits(fourTabletManifest())
	if err != nil {
		t.Fatal(err)
	}
	want := [][]byte{[]byte("b"), []byte("k"), []byte("r")}
	if !reflect.DeepEqual(splits, want) {
		t.Fatalf("RequiredDestinationSplits(four-tablet) = %#v, want %#v", splits, want)
	}
}

func TestRequiredDestinationSplitsPropagatesChainValidationError(t *testing.T) {
	manifest := threeTabletManifest()
	manifest.Tablets[1].StartRow = strPtr("e") // chain gap, same as BuildLoadMapping rejects
	if _, err := RequiredDestinationSplits(manifest); err == nil {
		t.Fatal("RequiredDestinationSplits with a malformed chain = nil error, want error")
	}
}

// TestRequiredDestinationSplitsRejectsUnsupportedManifestVersion is the
// RequiredDestinationSplits-side counterpart to
// TestBuildLoadMappingRejectsUnsupportedManifestVersion: it exercises
// the same resolveManifestTablets version check through
// RequiredDestinationSplits's own call path. RequiredDestinationSplits's
// doc comment claims it "performs the same" validation BuildLoadMapping
// does, including on a manifest whose Version predates or postdates
// what this build understands, precisely because it is documented as
// safe to call standalone -- typically to pre-create splits through
// accumulo.Connector.AddTableSplits ahead of staging -- so a caller
// following that documented pattern must be able to rely on the version
// check firing here too, not only inside BuildLoadMapping.
func TestRequiredDestinationSplitsRejectsUnsupportedManifestVersion(t *testing.T) {
	manifest := threeTabletManifest()
	manifest.Version = engine.RFileExportManifestVersion + 1
	if _, err := RequiredDestinationSplits(manifest); err == nil {
		t.Fatal("RequiredDestinationSplits with an unsupported manifest version = nil error, want error")
	}
}

// TestRequiredDestinationSplitsRejectsEmptyBoundaryRow is the
// RequiredDestinationSplits-side counterpart to
// TestBuildLoadMappingRejectsEmptyBoundaryRow, proving the same
// zero-length shared boundary is rejected regardless of which of
// resolveManifestTablets's two callers reaches resolveTabletChain first.
func TestRequiredDestinationSplitsRejectsEmptyBoundaryRow(t *testing.T) {
	manifest := twoTabletManifest()
	manifest.Tablets[0].EndRow = strPtr("")
	manifest.Tablets[1].StartRow = strPtr("")
	if _, err := RequiredDestinationSplits(manifest); err == nil {
		t.Fatal("RequiredDestinationSplits with an empty (non-nil) shared boundary row = nil error, want error")
	}
}

func TestRequiredDestinationSplitsRejectsNilManifest(t *testing.T) {
	if _, err := RequiredDestinationSplits(nil); err == nil {
		t.Fatal("RequiredDestinationSplits(nil) = nil error, want error")
	}
}

func TestReadLoadMappingRoundTripsMultiTabletWriteLoadMapping(t *testing.T) {
	mapping, err := BuildLoadMapping(threeTabletManifest())
	if err != nil {
		t.Fatal(err)
	}
	if len(mapping) != 3 {
		t.Fatalf("mapping entries = %d, want 3", len(mapping))
	}
	dst := memory.New()
	ctx := context.Background()
	if err := WriteLoadMapping(ctx, dst, "/bulk/events-1", mapping); err != nil {
		t.Fatal(err)
	}
	got, err := ReadLoadMapping(ctx, dst, "/bulk/events-1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, mapping) {
		t.Fatalf("round-tripped multi-tablet mapping = %#v, want %#v", got, mapping)
	}
}

func TestBuildLoadMappingRejectsSingleTabletBoundaryManifest(t *testing.T) {
	manifest := &engine.RFileExportManifest{
		SourceTable: "events",
		Tablets:     []engine.RFileExportTablet{{Index: 0, EndRow: strPtr("g")}},
		RFiles: []engine.RFileExportFile{
			{TabletIndex: 0, DestinationPath: "events/t-0000/F0001.rf", Size: 10},
		},
	}
	if _, err := BuildLoadMapping(manifest); err == nil {
		t.Fatal("BuildLoadMapping(single tablet with boundary) = nil error, want error")
	}
}

func TestBuildLoadMappingRejectsUndeclaredTabletIndex(t *testing.T) {
	manifest := &engine.RFileExportManifest{
		SourceTable: "events",
		Tablets:     []engine.RFileExportTablet{{Index: 0}},
		RFiles: []engine.RFileExportFile{
			{TabletIndex: 1, DestinationPath: "events/t-0001/F0001.rf", Size: 10},
		},
	}
	if _, err := BuildLoadMapping(manifest); err == nil {
		t.Fatal("BuildLoadMapping with undeclared tablet index = nil error, want error")
	}
}

func TestBuildLoadMappingRejectsAmbiguousLegacyManifest(t *testing.T) {
	manifest := &engine.RFileExportManifest{
		SourceTable: "events",
		RFiles: []engine.RFileExportFile{
			{TabletIndex: 1, DestinationPath: "events/t-0001/F0001.rf", Size: 10},
		},
	}
	if _, err := BuildLoadMapping(manifest); err == nil {
		t.Fatal("BuildLoadMapping with legacy manifest using non-zero tablet index = nil error, want error")
	}
}

func TestWriteLoadMappingMatchesAccumuloJSONShape(t *testing.T) {
	rowNeedingURLSafeEncoding := []byte{0xfb, 0xef, 0xbe}
	mapping := LoadMapping{
		{
			Tablet: KeyExtent{EndRow: rowNeedingURLSafeEncoding},
			Files:  []FileEntry{{Name: "F0001.rf", EstSize: 10, EstEntries: 0}},
		},
		{
			Tablet: KeyExtent{PrevEndRow: rowNeedingURLSafeEncoding},
			Files:  []FileEntry{{Name: "F0002.rf", EstSize: 20, EstEntries: 5}},
		},
	}

	dst := memory.New()
	ctx := context.Background()
	if err := WriteLoadMapping(ctx, dst, "hdfs://nn/bulk/events-1", mapping); err != nil {
		t.Fatal(err)
	}

	raw, err := dst.Open(ctx, "hdfs://nn/bulk/events-1/loadmap.json")
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, raw.Size())
	if _, err := raw.ReadAt(buf, 0); err != nil {
		t.Fatal(err)
	}
	body := string(buf)

	wantURLSafe := base64.URLEncoding.EncodeToString(rowNeedingURLSafeEncoding)
	wantStd := base64.StdEncoding.EncodeToString(rowNeedingURLSafeEncoding)
	if wantURLSafe == wantStd {
		t.Fatal("test fixture bytes do not actually distinguish URL-safe from standard base64")
	}
	if !strings.Contains(body, wantURLSafe) {
		t.Fatalf("loadmap.json missing expected base64url row %q:\n%s", wantURLSafe, body)
	}
	if strings.Contains(body, wantStd) {
		t.Fatalf("loadmap.json used standard base64 %q instead of URL-safe:\n%s", wantStd, body)
	}
	if strings.Contains(body, `"prevEndRow":null`) || strings.Contains(body, `"endRow":null`) {
		t.Fatalf("loadmap.json serialized a null row instead of omitting the key:\n%s", body)
	}
	for _, want := range []string{`"tablet"`, `"endRow"`, `"prevEndRow"`, `"files"`, `"name"`, `"estSize"`, `"estEntries"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("loadmap.json missing expected field %s:\n%s", want, body)
		}
	}
}

func TestReadLoadMappingRoundTripsSingleTabletWriteLoadMapping(t *testing.T) {
	mapping, err := BuildLoadMapping(singleTabletManifest())
	if err != nil {
		t.Fatal(err)
	}
	dst := memory.New()
	ctx := context.Background()
	if err := WriteLoadMapping(ctx, dst, "/bulk/events-1", mapping); err != nil {
		t.Fatal(err)
	}
	got, err := ReadLoadMapping(ctx, dst, "/bulk/events-1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, mapping) {
		t.Fatalf("round-tripped mapping = %#v, want %#v", got, mapping)
	}
}

func TestBuildLoadMappingRejectsParquetAndDerivedFiles(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*engine.RFileExportManifest)
	}{
		{
			name: "parquet table",
			mutate: func(manifest *engine.RFileExportManifest) {
				manifest.FileFormat = tablet.FormatParquet
				manifest.RFileCompatibility = "parquet/shoal"
				manifest.RFiles[0].Format = engine.ExportFormatParquet
			},
		},
		{
			name: "derived rfile",
			mutate: func(manifest *engine.RFileExportManifest) {
				manifest.RFiles[0].Role = engine.ExportRoleDerived
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := singleTabletManifest()
			test.mutate(manifest)
			if _, err := BuildLoadMapping(manifest); err == nil {
				t.Fatal("BuildLoadMapping = nil error, want format/role rejection")
			}
		})
	}
}
