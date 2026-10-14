// Package promotion implements the client-side half of Accumulo's Bulk
// Import V2 protocol, used to promote an already-exported local Shoal
// table into an existing Accumulo cluster.
//
// Accumulo's manager is the sole authority over tablet metadata: promotion
// never edits accumulo.metadata or ZooKeeper directly. Instead this
// package:
//
//  1. Validates a manifest's declared tablet chain (one implicit tablet
//     for legacy manifests with no Tablets entries, or any number of
//     explicitly declared tablets forming a single gapless chain) and
//     derives a destination KeyExtent for each.
//
//     Shoal's local tablets use [StartRow, EndRow) (inclusive start,
//     exclusive end — see engine/table.go's routeTablet), while
//     Accumulo's KeyExtent is (PrevEndRow, EndRow] (exclusive start,
//     inclusive end — see KeyExtent.java's contains()). Copying split
//     boundaries verbatim would make rows whose value exactly equals a
//     split point land on opposite sides of the boundary in the two
//     systems, so each tablet's destination KeyExtent is deliberately
//     widened instead: PrevEndRow is set to the *previous* tablet's own
//     StartRow (one tablet further back), not to the tablet's own
//     StartRow, which provably never excludes a row that genuinely
//     belongs to that tablet. See RequiredDestinationSplits and
//     docs/promotion.md §3 for the full derivation and its safety
//     argument.
//
//     A single-tablet (or legacy) manifest still yields exactly one fully
//     unbounded KeyExtent, unchanged from before.
//
//  2. Stages those RFiles flat into a bulk directory — Accumulo's bulk
//     import lists the bulk directory non-recursively, so files must sit
//     directly inside it, unlike ExportRFiles' per-tablet nesting — and
//     writes the load mapping as loadmap.json, in the exact
//     Gson-compatible JSON shape Accumulo's BulkSerialize /
//     LoadMappingIterator expect.
//
//  3. For a multi-tablet manifest, the widened KeyExtents this package
//     computes only pass Accumulo's own server-side
//     PrepBulkImport.validateLoadMapping check if the destination
//     table's splits, at or before the last boundary row
//     RequiredDestinationSplits reports, are exactly those rows — no
//     fewer and, just as importantly, no more (an extra pre-existing
//     split in that range is not harmless here; see
//     RequiredDestinationSplits's own doc comment for why). Promote
//     reconciles the "no fewer" half by submitting those rows through
//     the existing accumulo.Connector.AddTableSplitsForTable — itself a
//     manager TABLE_SPLIT FATE operation, the same protocol
//     TableOperations.addSplits uses, plus a pinned-table-identity check
//     — and the "no more" half by then confirming
//     accumulo.Connector.ListTableSplits reports nothing else in that
//     range, both before staging or submitting the bulk import.
//     BuildLoadMapping and StageBulkDir themselves never call Accumulo: a
//     caller invoking them directly for a multi-tablet manifest, outside
//     of Promote, is responsible for that same reconciliation, or the
//     subsequent BulkImport call will fail closed (not silently) with a
//     concurrent-merge-style rejection.
//
//  4. Submits the TABLE_BULK_IMPORT2 FATE operation via
//     accumulo.Connector.BulkImport — the only other step that talks to
//     Accumulo, and it does so exclusively through the manager's FATE
//     machinery, preserving the manager as the sole authority over the
//     promoted table's resulting tablet layout and file set.
//
// See REFERENCES.md for the exact upstream Java sources this package
// mirrors, and docs/promotion.md for the protocol writeup, what this
// bounded slice covers, and what is intentionally deferred.
package promotion

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/storage"
	"github.com/phrocker/shoal-oss/internal/tablet"
)

// bulkLoadMappingFile is the fixed filename Accumulo's BulkSerialize reads
// the load mapping from (Constants.BULK_LOAD_MAPPING in Java).
const bulkLoadMappingFile = "loadmap.json"

// KeyExtent is a destination tablet range (PrevEndRow, EndRow]. A nil EndRow
// means "the last tablet" (positive infinity); a nil PrevEndRow means "the
// first tablet" (negative infinity) — matching Accumulo's own KeyExtent
// (core/.../dataImpl/KeyExtent.java) with the table ID omitted, since the
// FATE TABLE_BULK_IMPORT2 call already carries the destination table ID as
// its own argument.
type KeyExtent struct {
	EndRow     []byte
	PrevEndRow []byte
}

// FileEntry is one RFile contributed to a Mapping's KeyExtent. Name is the
// basename only, as the file will sit in the flat bulk directory.
// EstEntries is always 0: unlike Accumulo's own client, this package never
// opens RFiles to count entries, mirroring the estimate-free
// Bulk.FileInfo(Path, estSize) convenience constructor Accumulo itself
// falls back to when entry counts aren't available.
type FileEntry struct {
	Name       string
	EstSize    int64
	EstEntries int64
}

// Mapping is one destination KeyExtent's worth of files.
type Mapping struct {
	Tablet KeyExtent
	Files  []FileEntry
}

// LoadMapping is a complete Bulk Import V2 load mapping, ordered the way
// Accumulo's LoadMappingIterator requires: ascending by EndRow, with a nil
// (unbounded) EndRow sorting last.
type LoadMapping []Mapping

// BuildLoadMapping derives a Bulk Import V2 load mapping from an
// RFileExportManifest. A manifest is accepted when it either omits
// Tablets entirely and every RFile uses TabletIndex 0 (the legacy default
// tablet), or its declared Tablets form a single gapless chain: indexes
// exactly {0, ..., N-1} in any slice order, the first tablet's StartRow
// and the last tablet's EndRow both nil, every other boundary present,
// each tablet's own StartRow strictly less than its own EndRow, and
// tablet i's EndRow exactly equal to tablet i+1's StartRow for every
// adjacent pair. Anything else — an out-of-range or duplicate index, a
// missing or misplaced nil boundary, a degenerate/inverted range, or a
// chain mismatch (a gap or an overlap) — is rejected before staging or
// submission.
//
// A single-tablet (or legacy) manifest yields one fully unbounded
// KeyExtent{PrevEndRow: nil, EndRow: nil}, unchanged from before. A
// multi-tablet manifest yields one Mapping per tablet index that has at
// least one file (an index with zero files is silently omitted — there is
// nothing to place there), each keyed by a deliberately widened KeyExtent:
// EndRow is the tablet's own EndRow (always safe — the tablet's own
// exported data never reaches that row, by construction of Shoal's
// exclusive-end routing), but PrevEndRow is the *previous* tablet's own
// StartRow rather than this tablet's own StartRow, so that a row exactly
// equal to this tablet's inclusive start is never excluded by Accumulo's
// exclusive-start convention. See RequiredDestinationSplits — including
// why the destination must not have any *extra* split in the range that
// widening covers — and docs/promotion.md §3 for the full derivation.
// That widening means each
// returned Mapping's KeyExtent generally spans up to two adjacent source
// tablets' worth of destination range (index 1 widens all the way to
// unbounded, since there is no tablet before index 0 to anchor to — an
// acknowledged limitation, not a bug), so Accumulo may legitimately load
// a file against more destination tablets than the source tablet it came
// from; that is always safe, never a correctness issue, only a modest
// over-registration.
//
// RFiles sharing the same DestinationPath (e.g. a manifest listing one
// physical file more than once under the same tablet) contribute a single
// FileEntry: Accumulo's Bulk.Files is name-keyed and rejects duplicate
// filenames outright. A repeated DestinationPath under different
// TabletIndex values is rejected as ambiguous before any staging can omit
// it from loadmap.json.
//
// Legacy manifests with no Tablets entries (RFileExportManifest.Tablets
// nil/empty) map every RFile (TabletIndex 0) to a single unbounded tablet,
// mirroring RFileExportManifest.tabletCount()'s documented default.
// Manifests that omit Tablets but use any other TabletIndex are rejected as
// ambiguous legacy input.
//
// manifest.Version is checked first (inside resolveManifestTablets,
// shared with RequiredDestinationSplits below), against the supported legacy
// and current export versions, before any chain or RFile
// validation: a manifest from an unsupported export format is rejected
// here, in Promote's own preflight call to this function, rather than
// only later inside StageBulkDir's call to engine.VerifyRFileExport --
// which, unlike this one, runs after AddTableSplitsForTable has already
// reconciled the destination's splits (see Promote's doc comment).
func BuildLoadMapping(manifest *engine.RFileExportManifest) (LoadMapping, error) {
	if manifest == nil {
		return nil, fmt.Errorf("promotion: nil export manifest")
	}
	if manifest.FileFormat == tablet.FormatParquet ||
		(manifest.RFileCompatibility != "" && manifest.RFileCompatibility != "accumulo-rfile/shoal") {
		return nil, fmt.Errorf("promotion: manifest compatibility %q with format %q cannot be registered as Accumulo tablet files",
			manifest.RFileCompatibility, manifest.FileFormat)
	}
	for _, file := range manifest.RFiles {
		format := file.Format
		if format == "" {
			format = engine.ExportFormatRFile
		}
		role := file.Role
		if role == "" {
			role = engine.ExportRoleAuthoritative
		}
		if format != engine.ExportFormatRFile || role != engine.ExportRoleAuthoritative {
			return nil, fmt.Errorf("promotion: file %q has format %q and role %q; Accumulo bulk import requires authoritative RFiles",
				file.DestinationPath, format, role)
		}
	}
	tablets, declared, err := resolveManifestTablets(manifest)
	if err != nil {
		return nil, err
	}

	filesByTablet := make(map[int][]FileEntry, len(tablets))
	seen := make(map[string]int, len(manifest.RFiles))
	for _, rf := range manifest.RFiles {
		if _, ok := declared[rf.TabletIndex]; !ok {
			return nil, fmt.Errorf("promotion: rfile %q references undeclared tablet index %d", rf.DestinationPath, rf.TabletIndex)
		}
		if priorIndex, ok := seen[rf.DestinationPath]; ok {
			if priorIndex != rf.TabletIndex {
				return nil, fmt.Errorf(
					"promotion: rfile %q is declared under multiple tablet indexes (%d and %d)",
					rf.DestinationPath, priorIndex, rf.TabletIndex,
				)
			}
			continue
		}
		seen[rf.DestinationPath] = rf.TabletIndex
		filesByTablet[rf.TabletIndex] = append(filesByTablet[rf.TabletIndex], FileEntry{
			Name:    filepath.Base(rf.DestinationPath),
			EstSize: rf.Size,
		})
	}

	mapping := make(LoadMapping, 0, len(tablets))
	for _, tablet := range tablets {
		files := filesByTablet[tablet.index]
		if len(files) == 0 {
			continue
		}
		sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
		mapping = append(mapping, Mapping{Tablet: tablet.extent, Files: files})
	}
	if len(mapping) == 0 {
		return nil, nil
	}
	return mapping, nil
}

// resolvedTablet is one manifest tablet index after full chain validation,
// paired with the widened Accumulo KeyExtent BuildLoadMapping should use
// for any RFiles declared under it.
type resolvedTablet struct {
	index  int
	extent KeyExtent
}

// resolveManifestTablets validates manifest's declared tablet chain and
// returns one resolvedTablet per index, in ascending index order
// (0..N-1), plus the set of indexes BuildLoadMapping may accept RFiles
// under. See resolveTabletChain for the exact chain-shape requirements and
// BuildLoadMapping's doc comment for the widening rule.
//
// manifest.Version is checked first against the supported legacy and current
// export versions: both of resolveManifestTablets's
// two callers (BuildLoadMapping and RequiredDestinationSplits) document
// themselves as performing "the same" validation, so the version check
// belongs here, once, rather than duplicated (or, worse, present in one
// and silently missing from the other -- RequiredDestinationSplits is
// exported and documented as safe to call standalone specifically to
// pre-create splits through AddTableSplitsForTable, so it must reject an
// unsupported version just as eagerly as BuildLoadMapping does).
func resolveManifestTablets(manifest *engine.RFileExportManifest) ([]resolvedTablet, map[int]struct{}, error) {
	if manifest.Version != engine.RFileExportManifestLegacyVersion &&
		manifest.Version != engine.RFileExportManifestVersion {
		return nil, nil, fmt.Errorf("promotion: unsupported manifest version %d", manifest.Version)
	}
	if len(manifest.Tablets) == 0 {
		for _, rf := range manifest.RFiles {
			if rf.TabletIndex != 0 {
				return nil, nil, fmt.Errorf(
					"promotion: legacy manifest without tablets is ambiguous: rfile %q references tablet index %d",
					rf.DestinationPath, rf.TabletIndex,
				)
			}
		}
		return []resolvedTablet{{index: 0}}, map[int]struct{}{0: {}}, nil
	}

	chain, err := resolveTabletChain(manifest.Tablets)
	if err != nil {
		return nil, nil, err
	}

	declared := make(map[int]struct{}, len(chain))
	resolved := make([]resolvedTablet, len(chain))
	for i, tablet := range chain {
		declared[tablet.Index] = struct{}{}
		var prevEndRow []byte
		if i >= 1 {
			// Widen to the *previous* tablet's own StartRow, not this
			// tablet's own StartRow: Shoal's inclusive tablet start would
			// otherwise fall outside Accumulo's exclusive-start extent.
			prevEndRow = rowBytes(chain[i-1].StartRow)
		}
		resolved[i] = resolvedTablet{
			index: tablet.Index,
			extent: KeyExtent{
				EndRow:     rowBytes(tablet.EndRow),
				PrevEndRow: prevEndRow,
			},
		}
	}
	return resolved, declared, nil
}

// resolveTabletChain validates that tablets describes a single table's
// entire keyspace as one unambiguous, gapless chain, independent of the
// slice's own physical order, and returns it reordered so chain[i].Index
// == i.
//
// Required shape: indexes are exactly {0, ..., N-1} with no duplicates,
// gaps, or out-of-range values; tablet 0's StartRow is nil (negative
// infinity); tablet N-1's EndRow is nil (positive infinity); every other
// tablet declares both boundaries; every tablet's own StartRow is
// strictly less than its own EndRow whenever both are set (a plain Go
// string comparison, rejecting degenerate/inverted ranges — safe even
// for non-UTF-8 row values, since RFileExportTablet's *string fields are
// guaranteed to hold their exact original bytes; see rowBytes); and
// tablet i's EndRow exactly equals tablet i+1's StartRow for every
// adjacent pair (rejecting both gaps and overlaps — anything short of
// exact equality is one or the other).
//
// A non-nil but empty (zero-length) StartRow or EndRow is rejected
// outright, at any chain position: an empty row string is never a
// meaningful Accumulo split point (there is nothing before it to bound),
// and accumulo.Connector.AddTableSplitsForTable's own normalizeSplitRows
// (shared with AddTableSplits; see addTableSplits in
// table_add_splits.go) rejects a zero-length split unconditionally, so
// a manifest containing one would otherwise pass every check above
// only to have AddTableSplitsForTable reject it later for a reason
// expressed in terms of that lower layer's own row-normalization
// contract instead of this package's own manifest-shape validation.
func resolveTabletChain(tablets []engine.RFileExportTablet) ([]engine.RFileExportTablet, error) {
	n := len(tablets)
	byIndex := make(map[int]engine.RFileExportTablet, n)
	for _, tablet := range tablets {
		if tablet.Index < 0 || tablet.Index >= n {
			return nil, fmt.Errorf(
				"promotion: tablet index %d is out of range for a %d-tablet manifest",
				tablet.Index, n,
			)
		}
		if _, ok := byIndex[tablet.Index]; ok {
			return nil, fmt.Errorf("promotion: tablet index %d is declared more than once", tablet.Index)
		}
		byIndex[tablet.Index] = tablet
	}
	// Every tablet.Index is distinct and inside [0, n), and there are
	// exactly n of them, so byIndex necessarily holds one entry per index
	// in [0, n): a full 0..n-1 cover, regardless of tablets' own order.
	chain := make([]engine.RFileExportTablet, n)
	for i := 0; i < n; i++ {
		chain[i] = byIndex[i]
	}

	if chain[0].StartRow != nil {
		return nil, fmt.Errorf("promotion: first tablet (index %d) must have a nil StartRow", chain[0].Index)
	}
	if chain[n-1].EndRow != nil {
		return nil, fmt.Errorf("promotion: last tablet (index %d) must have a nil EndRow", chain[n-1].Index)
	}
	for i, tablet := range chain {
		if i > 0 && tablet.StartRow == nil {
			return nil, fmt.Errorf("promotion: tablet index %d is missing its StartRow", tablet.Index)
		}
		if i < n-1 && tablet.EndRow == nil {
			return nil, fmt.Errorf("promotion: tablet index %d is missing its EndRow", tablet.Index)
		}
		if tablet.StartRow != nil && len(*tablet.StartRow) == 0 {
			return nil, fmt.Errorf("promotion: tablet index %d has an empty StartRow, which Accumulo cannot use as a split row", tablet.Index)
		}
		if tablet.EndRow != nil && len(*tablet.EndRow) == 0 {
			return nil, fmt.Errorf("promotion: tablet index %d has an empty EndRow, which Accumulo cannot use as a split row", tablet.Index)
		}
		if tablet.StartRow != nil && tablet.EndRow != nil && !(*tablet.StartRow < *tablet.EndRow) {
			return nil, fmt.Errorf(
				"promotion: tablet index %d has a degenerate or inverted range [%s, %s)",
				tablet.Index, formatRow(tablet.StartRow), formatRow(tablet.EndRow),
			)
		}
		if i > 0 {
			prev := chain[i-1]
			if prev.EndRow == nil || tablet.StartRow == nil || *prev.EndRow != *tablet.StartRow {
				return nil, fmt.Errorf(
					"promotion: tablet chain mismatch: tablet index %d ends at %s but tablet index %d starts at %s",
					prev.Index, formatRow(prev.EndRow), tablet.Index, formatRow(tablet.StartRow),
				)
			}
		}
	}
	return chain, nil
}

func formatRow(row *string) string {
	if row == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%q", *row)
}

// RequiredDestinationSplits returns the destination table's split rows, in
// ascending order, that a Bulk Import V2 submission built from
// manifest's load mapping (BuildLoadMapping) requires to already exist
// — and, for a multi-tablet manifest, requires the destination to have
// no *other* splits at or before the last of these rows — before
// Accumulo's own server-side PrepBulkImport.validateLoadMapping check
// will pass. It performs the same tablet-chain validation
// BuildLoadMapping does, and fails the same way on a malformed
// manifest, but never touches storage or Accumulo itself: it is a pure
// function, safe to call standalone — for example to pre-create splits
// through accumulo.Connector.AddTableSplitsForTable before staging, which is
// exactly what Promote does.
//
// A single-tablet (or legacy) manifest requires no destination splits at
// all: BuildLoadMapping's fully unbounded KeyExtent matches Accumulo's
// outermost tablet boundaries (both nil) regardless of how many splits
// the destination already has, so this returns (nil, nil) for that case.
//
// For an N-tablet manifest (N>1), the required splits are exactly the N-1
// boundary rows shared by adjacent tablets (tablet i's EndRow, equivalently
// tablet i+1's StartRow, for i in 0..N-2): those are the only rows any
// widened KeyExtent BuildLoadMapping computes can ever reference. It is
// tempting to conclude from this that any *additional* pre-existing
// destination splits between these points are therefore harmless, since
// Accumulo's validateLoadMapping does let a single load-mapping entry
// span several destination tablets when that entry's declared
// PrevEndRow/EndRow are each matched by some existing tablet boundary.
// That conclusion is false for the overlapping, widened extents this
// package builds, and callers must not rely on it: validateLoadMapping
// walks the destination's tablets with a single, shared, forward-only
// iterator across every load-mapping entry in submission order, so a
// destination split strictly *before* the last required row leaves
// that iterator stopped one real tablet further forward than the next
// entry's widened PrevEndRow — always the row two boundaries back, by
// construction; see BuildLoadMapping — can match. The iterator can
// never rewind, so the whole submission is rejected with a spurious
// BULK_CONCURRENT_MERGE-style error even though nothing actually
// merged. A split strictly *after* the last required row is harmless
// with respect to *this* validation walk specifically: the final
// Mapping entry is always fully unbounded (nil EndRow) and absorbs it
// regardless of how many further splits exist beyond it, so it can
// never reproduce the iterator-can't-rewind failure above. That is not
// the same as harmless in every sense a bulk import can fail, though:
// Accumulo's PrepBulkImport.validateLoadMapping separately enforces
// table.bulk.max.tablets (default 100, admin-configurable, since
// Accumulo 2.1.0) by counting how many real destination tablets the
// final entry's files overlap, a count trailing splits inflate
// directly — see Promote's own doc comment and docs/promotion.md §5
// item 3 for the full reasoning on why this package does not enforce
// that limit itself.
//
// This function only reports what rows are required; it cannot detect
// an unsafe pre-existing split on its own, since it is a pure function
// over the manifest alone with no view of the destination's actual
// state. Promote separately verifies, via
// accumulo.Connector.ListTableSplits immediately after AddTableSplitsForTable,
// that the destination's splits at or before the last required row are
// exactly these rows — no more, no fewer — failing closed with a
// specific, actionable error before any staging or BulkImport call
// otherwise (see verifyNoUnexpectedDestinationSplits and Promote's own
// doc comment for the full mechanism). A caller invoking
// RequiredDestinationSplits or BuildLoadMapping directly, outside of
// Promote, is responsible for that same reconciliation itself, or for
// accepting that the subsequent BulkImport call may fail.
//
// Even with that verification in place, this says nothing about
// whether the destination will still have exactly these splits by the
// time a subsequent BulkImport call is validated server side: a
// concurrent split or merge on the destination between verification
// and bulk-import submission is a real, acknowledged race this package
// cannot close on its own (see docs/promotion.md §5) — Accumulo's own
// FATE validation rejects that case outright rather than corrupting
// data, so this residual window is a safe-failure gap, not a
// correctness one.
func RequiredDestinationSplits(manifest *engine.RFileExportManifest) ([][]byte, error) {
	if manifest == nil {
		return nil, fmt.Errorf("promotion: nil export manifest")
	}
	resolved, _, err := resolveManifestTablets(manifest)
	if err != nil {
		return nil, err
	}
	if len(resolved) <= 1 {
		return nil, nil
	}
	splits := make([][]byte, 0, len(resolved)-1)
	for i := 0; i < len(resolved)-1; i++ {
		splits = append(splits, resolved[i].extent.EndRow)
	}
	return splits, nil
}

// rowBytes returns row's raw bytes, or nil if row is nil.
//
// This is safe even for row values that are not valid UTF-8:
// RFileExportTablet.StartRow/EndRow are declared as *string purely as a
// byte-container convention, not as text (see rfile_export.go's
// RFileExportTablet doc comment). Their JSON wire encoding is
// base64url, not encoding/json's default UTF-8 string handling, so —
// unlike a plain Go string field marshaled the ordinary way, which
// would silently replace an invalid byte sequence with U+FFFD on the
// way to disk — a manifest's declared row boundaries round-trip
// through JSON with their exact original bytes intact.
func rowBytes(s *string) []byte {
	if s == nil {
		return nil
	}
	return []byte(*s)
}

// jsonTablet/jsonFileInfo/jsonMapping mirror Accumulo's
// clientImpl.bulk.Bulk.{Tablet,FileInfo,Mapping} Gson shape field-for-field
// (field names are case-sensitive and unmodified by any Gson naming
// policy). endRow/prevEndRow are base64url strings, omitted entirely when
// nil rather than serialized as null — Gson's ByteArrayToBase64TypeAdapter
// uses java.util.Base64.getUrlEncoder() (padded, URL-safe alphabet, i.e.
// Go's base64.URLEncoding) and Gson's default GsonBuilder suppresses null
// fields rather than emitting them.
type jsonTablet struct {
	EndRow     *string `json:"endRow,omitempty"`
	PrevEndRow *string `json:"prevEndRow,omitempty"`
}

type jsonFileInfo struct {
	Name       string `json:"name"`
	EstSize    int64  `json:"estSize"`
	EstEntries int64  `json:"estEntries"`
}

type jsonMapping struct {
	Tablet jsonTablet     `json:"tablet"`
	Files  []jsonFileInfo `json:"files"`
}

// WriteLoadMapping serializes mapping to <bulkDir>/loadmap.json on dst, as
// a top-level JSON array of {tablet:{endRow,prevEndRow},
// files:[{name,estSize,estEntries}]} objects — the exact shape Accumulo's
// clientImpl.bulk.LoadMappingIterator/BulkSerialize read.
func WriteLoadMapping(ctx context.Context, dst storage.Backend, bulkDir string, mapping LoadMapping) error {
	data, err := marshalLoadMapping(mapping)
	if err != nil {
		return err
	}
	path := joinBulkPath(dst, bulkDir, bulkLoadMappingFile)
	if err := storage.WriteAll(ctx, dst, path, data); err != nil {
		return fmt.Errorf("promotion: write load mapping %s: %w", path, err)
	}
	return nil
}

// ReadLoadMapping parses a loadmap.json previously written by
// WriteLoadMapping (or a real Accumulo client) at <bulkDir>/loadmap.json on
// src. Intended for tests and operator verification.
func ReadLoadMapping(ctx context.Context, src storage.Backend, bulkDir string) (LoadMapping, error) {
	path := joinBulkPath(src, bulkDir, bulkLoadMappingFile)
	data, err := storage.ReadAll(ctx, src, path)
	if err != nil {
		return nil, fmt.Errorf("promotion: read load mapping %s: %w", path, err)
	}
	return unmarshalLoadMapping(data)
}

func marshalLoadMapping(mapping LoadMapping) ([]byte, error) {
	out := make([]jsonMapping, len(mapping))
	for i, m := range mapping {
		files := make([]jsonFileInfo, len(m.Files))
		for j, f := range m.Files {
			files[j] = jsonFileInfo{Name: f.Name, EstSize: f.EstSize, EstEntries: f.EstEntries}
		}
		out[i] = jsonMapping{
			Tablet: jsonTablet{
				EndRow:     base64RowPtr(m.Tablet.EndRow),
				PrevEndRow: base64RowPtr(m.Tablet.PrevEndRow),
			},
			Files: files,
		}
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("promotion: marshal load mapping: %w", err)
	}
	return append(data, '\n'), nil
}

func unmarshalLoadMapping(data []byte) (LoadMapping, error) {
	var raw []jsonMapping
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("promotion: decode load mapping: %w", err)
	}
	mapping := make(LoadMapping, len(raw))
	for i, m := range raw {
		endRow, err := base64RowValue(m.Tablet.EndRow)
		if err != nil {
			return nil, fmt.Errorf("promotion: decode endRow: %w", err)
		}
		prevEndRow, err := base64RowValue(m.Tablet.PrevEndRow)
		if err != nil {
			return nil, fmt.Errorf("promotion: decode prevEndRow: %w", err)
		}
		files := make([]FileEntry, len(m.Files))
		for j, f := range m.Files {
			files[j] = FileEntry{Name: f.Name, EstSize: f.EstSize, EstEntries: f.EstEntries}
		}
		mapping[i] = Mapping{Tablet: KeyExtent{EndRow: endRow, PrevEndRow: prevEndRow}, Files: files}
	}
	return mapping, nil
}

func base64RowPtr(row []byte) *string {
	if row == nil {
		return nil
	}
	s := base64.URLEncoding.EncodeToString(row)
	return &s
}

func base64RowValue(s *string) ([]byte, error) {
	if s == nil {
		return nil, nil
	}
	return base64.URLEncoding.DecodeString(*s)
}

// joinBulkPath mirrors engine's joinBackendPath: URL-style backend roots
// (scheme://..., plus HDFS's hdfs:/... authorityless form) join with a
// literal "/". Ambiguous one-character roots such as x://... are treated
// as backend URLs only when dst declares that scheme; otherwise they keep
// local Windows-drive semantics.
func joinBulkPath(dst storage.Backend, bulkDir, name string) string {
	if pathUsesBackendSeparatorJoinOnBackend(dst, bulkDir) {
		return strings.TrimRight(bulkDir, `/\`) + "/" + name
	}
	return filepath.Join(bulkDir, name)
}
