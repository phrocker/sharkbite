package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/phrocker/shoal-oss/internal/compaction"
	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/parquetfile"
	"github.com/phrocker/shoal-oss/internal/rfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile/block"
	"github.com/phrocker/shoal-oss/internal/rfile/wire"
	"github.com/phrocker/shoal-oss/internal/storage"
	"github.com/phrocker/shoal-oss/internal/tablet"
)

const (
	RFileExportManifestLegacyVersion = 1
	RFileExportManifestVersion       = 2
)

const verificationSnapshotChunkSize = 256 * 1024

// producerIDRe constrains a fan-in producer id to characters that are safe in
// both object keys and local file names and that exclude the "~" namespacing
// separator used by exportRelPath.
var producerIDRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// validateProducerID rejects producer ids that would break destination naming
// (path separators, "~", or other unsafe characters). Empty is allowed and
// disables namespacing.
func validateProducerID(id string) error {
	if id == "" {
		return nil
	}
	if !producerIDRe.MatchString(id) {
		return fmt.Errorf("engine: invalid producer id %q (must match [A-Za-z0-9_.-]+)", id)
	}
	return nil
}

// RFileExportManifest describes a byte-for-byte RFile table export.
type RFileExportManifest struct {
	Version             int                 `json:"version"`
	CreatedAt           time.Time           `json:"created_at"`
	SourceTable         string              `json:"source_table"`
	EngineVersion       string              `json:"engine_version,omitempty"`
	RFileCompatibility  string              `json:"rfile_compatibility"`
	FileFormat          tablet.FileFormat   `json:"file_format,omitempty"`
	CFSchema            string              `json:"cf_schema,omitempty"`
	VisibilityStamp     string              `json:"visibility_stamp,omitempty"`
	AuthorizationsStamp string              `json:"authorizations_stamp,omitempty"`
	Tablets             []RFileExportTablet `json:"tablets"`
	RFiles              []RFileExportFile   `json:"rfiles"`
}

// RFileExportTablet describes one destination tablet's key-range
// boundaries in an RFile export manifest. StartRow/EndRow are nil for an
// unbounded side (matching Shoal's own [StartRow, EndRow) tablet
// convention; see engine/table.go's routeTablet), and are Go strings
// used purely as byte containers -- Accumulo row keys are arbitrary
// bytes, not necessarily valid UTF-8 text.
//
// MarshalJSON always writes StartRow/EndRow base64-encoded, under the
// dedicated start_row_b64/end_row_b64 JSON keys, instead of emitting them
// as ordinary JSON string literals under start_row/end_row: encoding/
// json's default string handling treats a Go string as UTF-8 text and
// silently replaces any invalid byte sequence with U+FFFD, which would
// irrecoverably corrupt a split row that is not valid UTF-8 the moment
// the manifest round-trips through JSON (see rfile_export_test.go's
// TestRFileExportTabletJSONRoundTripsNonUTF8Rows for a worked example,
// and REFERENCES.md/docs/promotion.md for why promotion depends on this
// byte-for-byte fidelity). This mirrors the same
// ByteArrayToBase64TypeAdapter-equivalent convention this repo already
// uses for Accumulo's own Bulk Import V2 loadmap.json wire format (see
// promotion.base64RowPtr/base64RowValue).
//
// UnmarshalJSON also still accepts the legacy plain start_row/end_row
// string keys, decoding them exactly as a raw string always was before
// this fix (unchanged, including its lossy-for-non-UTF-8-bytes
// behavior). exportTablets has, since long before this type gained
// custom JSON methods, been able to populate a non-nil StartRow/EndRow
// for any table with more than one tablet -- and cmd/shoal-embed's
// export/import commands persist and later re-read exactly this
// manifest shape through a storage backend, so a manifest already
// written to disk (or object storage) by an older build, and not yet
// imported, is a real artifact this package must still be able to read
// correctly, not merely a theoretical concern. Using a distinct JSON key
// for the base64 form (rather than sniffing whether a single field's
// contents happen to look like base64) is deliberate: a legacy raw
// string that happens to already be valid base64 would otherwise
// silently decode to different row bytes instead of failing loudly, the
// exact failure mode this package must not introduce. New manifests
// never round-trip through the legacy keys: MarshalJSON always emits
// only the *_b64 keys.
//
// Version 2 readers consume these byte-safe boundaries directly. Legacy
// version 1 remains accepted only for authoritative RFile manifests.
type RFileExportTablet struct {
	Index    int
	StartRow *string
	EndRow   *string
}

// rfileExportTabletJSON is RFileExportTablet's on-the-wire JSON shape.
// StartRowB64/EndRowB64 is the only form MarshalJSON ever writes;
// StartRow/EndRow (the pre-fix, plain-string keys) are accepted by
// UnmarshalJSON only, purely for reading an already-persisted legacy
// manifest -- see RFileExportTablet's doc comment.
type rfileExportTabletJSON struct {
	Index       int     `json:"index"`
	StartRowB64 *string `json:"start_row_b64,omitempty"`
	EndRowB64   *string `json:"end_row_b64,omitempty"`
	StartRow    *string `json:"start_row,omitempty"`
	EndRow      *string `json:"end_row,omitempty"`
}

// MarshalJSON implements json.Marshaler; see RFileExportTablet's doc
// comment for why StartRow/EndRow are base64-encoded under dedicated
// *_b64 keys.
func (t RFileExportTablet) MarshalJSON() ([]byte, error) {
	return json.Marshal(rfileExportTabletJSON{
		Index:       t.Index,
		StartRowB64: base64RowStringPtr(t.StartRow),
		EndRowB64:   base64RowStringPtr(t.EndRow),
	})
}

// UnmarshalJSON implements json.Unmarshaler; see RFileExportTablet's doc
// comment for the *_b64-preferred, legacy-plain-string-fallback decoding
// this performs.
func (t *RFileExportTablet) UnmarshalJSON(data []byte) error {
	var raw rfileExportTabletJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	startRow, err := decodeExportRow(raw.StartRowB64, raw.StartRow)
	if err != nil {
		return fmt.Errorf("engine: decode tablet %d start_row: %w", raw.Index, err)
	}
	endRow, err := decodeExportRow(raw.EndRowB64, raw.EndRow)
	if err != nil {
		return fmt.Errorf("engine: decode tablet %d end_row: %w", raw.Index, err)
	}
	t.Index = raw.Index
	t.StartRow = startRow
	t.EndRow = endRow
	return nil
}

// decodeExportRow resolves one row boundary from its two possible wire
// forms: b64 (the current, byte-safe representation this package always
// writes) takes priority when present; legacy (the pre-fix plain-string
// representation) is used as-is, unchanged from how every manifest ever
// written before this fix was already read, only when b64 is absent.
// Both being nil means an unbounded boundary; both being non-nil should
// never happen from any manifest this package itself writes, but is
// resolved deterministically (preferring b64) rather than rejected,
// since it is not itself unsafe -- merely redundant.
func decodeExportRow(b64, legacy *string) (*string, error) {
	if b64 != nil {
		return base64RowStringValue(b64)
	}
	if legacy != nil {
		v := *legacy
		return &v, nil
	}
	return nil, nil
}

// base64RowStringPtr base64-encodes row's raw bytes for the JSON wire
// form, or returns nil unchanged (an unbounded side stays absent from
// the JSON object via omitempty).
func base64RowStringPtr(row *string) *string {
	if row == nil {
		return nil
	}
	s := base64.URLEncoding.EncodeToString([]byte(*row))
	return &s
}

// base64RowStringValue reverses base64RowStringPtr.
func base64RowStringValue(encoded *string) (*string, error) {
	if encoded == nil {
		return nil, nil
	}
	decoded, err := base64.URLEncoding.DecodeString(*encoded)
	if err != nil {
		return nil, err
	}
	s := string(decoded)
	return &s, nil
}

type RFileExportFile struct {
	TabletIndex     int    `json:"tablet_index"`
	SourcePath      string `json:"source_path"`
	DestinationPath string `json:"destination_path"`
	RelativePath    string `json:"relative_path"`
	Size            int64  `json:"size"`
	SHA256          string `json:"sha256"`
	BCFileVersion   string `json:"bcfile_version,omitempty"`
	// Empty values are the legacy encoding of rfile/authoritative.
	Format string `json:"format,omitempty"`
	Role   string `json:"role,omitempty"`
}

const (
	ExportFormatRFile   = "rfile"
	ExportFormatParquet = "parquet"

	ExportRoleAuthoritative = "authoritative"
	ExportRoleDerived       = "derived"
)

type RFileExportOptions struct {
	DestinationRoot     string
	CFSchema            string
	VisibilityStamp     string
	AuthorizationsStamp string
	EngineVersion       string
	ManifestPath        string

	// StampVisibilityLabel, when non-empty, rewrites every exported cell's
	// ColumnVisibility to carry this tenant label (via the visibilityStamp
	// compaction iterator) instead of copying RFiles byte-for-byte. This is
	// what lets many independent producers fan their tables into one engine
	// while staying isolated: a scan only surfaces a producer's cells when
	// its Authorizations satisfy the stamped label. The label must be a bare
	// Accumulo CV label ([A-Za-z0-9_:./-]+). When set, it also defaults the
	// manifest's VisibilityStamp/AuthorizationsStamp metadata to the label.
	StampVisibilityLabel string
	// StampMode selects the stamping semantics: "and" (default) requires the
	// label on every cell; "whenEmpty" only stamps cells with no existing CV.
	StampMode string

	// ProducerID, when non-empty, namespaces every exported RFile's
	// destination object name with a "<producer>~" prefix on its base name
	// (e.g. graph/t-0000/agentA~F0001700000000000.rf). Local RFile names are
	// minted from a millisecond clock (F<ms>.rf / C<ms>.rf), so two producers
	// flushing into one shared destination at the same millisecond would
	// otherwise collide on an identical object key and clobber each other.
	// Prefixing with a stable per-producer id makes the destination names
	// globally unique, so many local agents can fan their RFiles into one
	// engine's tablet directories safely (the files still live directly in the
	// tablet dir and end in .rf, so import re-discovery finds them). Must match
	// [A-Za-z0-9_.-]+. It namespaces only the destination/manifest paths, never
	// the source RFiles.
	ProducerID string
}

// applyStampDefaults populates the manifest visibility metadata from the
// stamp label when the caller didn't set explicit values, so the manifest
// documents the enforced authorization.
func (o RFileExportOptions) applyStampDefaults(m *RFileExportManifest) {
	if o.StampVisibilityLabel == "" {
		return
	}
	if m.VisibilityStamp == "" {
		m.VisibilityStamp = o.StampVisibilityLabel
	}
	if m.AuthorizationsStamp == "" {
		m.AuthorizationsStamp = o.StampVisibilityLabel
	}
}

// ExportRFiles flushes table, copies its immutable RFiles to dst, and writes a manifest.
func (e *Engine) ExportRFiles(ctx context.Context, tableName string, dst storage.Backend, opts RFileExportOptions) (*RFileExportManifest, error) {
	if opts.DestinationRoot == "" {
		return nil, fmt.Errorf("engine: export destination root is required")
	}
	if err := validateProducerID(opts.ProducerID); err != nil {
		return nil, err
	}
	if err := e.Flush(tableName); err != nil {
		return nil, err
	}

	e.mu.RLock()
	tbl, ok := e.tables[tableName]
	var configuredFormat tablet.FileFormat
	if ok {
		configuredFormat = tbl.fileFormat()
	}
	e.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("engine: table %q not found", tableName)
	}

	files := tbl.rfiles()
	compatibility := exportCompatibility(files)
	manifest := &RFileExportManifest{
		Version:             RFileExportManifestVersion,
		CreatedAt:           time.Now().UTC(),
		SourceTable:         tableName,
		EngineVersion:       opts.EngineVersion,
		RFileCompatibility:  compatibility,
		FileFormat:          configuredFormat,
		CFSchema:            opts.CFSchema,
		VisibilityStamp:     opts.VisibilityStamp,
		AuthorizationsStamp: opts.AuthorizationsStamp,
		Tablets:             tbl.exportTablets(),
		RFiles:              make([]RFileExportFile, 0, len(files)),
	}
	opts.applyStampDefaults(manifest)
	for _, f := range files {
		rel := e.exportRelPath(f, tableName, opts.ProducerID)
		manifestRel := rel
		dstPath := joinBackendPath(dst, opts.DestinationRoot, filepath.FromSlash(rel))
		size, sum, bcVersion, err := copyOrStampRFile(ctx, e.backend, f.Path, dst, dstPath, opts)
		if err != nil {
			return nil, err
		}
		manifest.RFiles = append(manifest.RFiles, RFileExportFile{
			TabletIndex:     f.TabletIndex,
			SourcePath:      f.Path,
			DestinationPath: dstPath,
			RelativePath:    manifestRel,
			Size:            size,
			SHA256:          sum,
			BCFileVersion:   bcVersion,
			Format:          string(fileFormatForPath(f.Path)),
			Role:            ExportRoleAuthoritative,
		})
	}
	// Group by tablet but keep each tablet's recency order (from rfiles):
	// an importer registers files in manifest order, and that order is how
	// it resolves keys rewritten with the same timestamp (#692).
	sort.SliceStable(manifest.RFiles, func(i, j int) bool {
		return manifest.RFiles[i].TabletIndex < manifest.RFiles[j].TabletIndex
	})
	manifest.Version = exportManifestVersion(manifest)

	manifestPath := opts.ManifestPath
	if manifestPath == "" {
		manifestPath = joinBackendPath(dst, opts.DestinationRoot, "manifest.json")
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("engine: marshal manifest: %w", err)
	}
	if err := storage.WriteAll(ctx, dst, manifestPath, append(data, '\n')); err != nil {
		return nil, fmt.Errorf("engine: write manifest %s: %w", manifestPath, err)
	}
	return manifest, nil
}

// tabletCount is the number of tablet directories an import materializes for
// this manifest. A manifest with no explicit Tablets entries maps to the
// legacy single t-0000 tablet.
func (m *RFileExportManifest) tabletCount() int {
	if len(m.Tablets) == 0 {
		return 1
	}
	return len(m.Tablets)
}

func manifestSplits(manifest *RFileExportManifest) ([][]byte, error) {
	if len(manifest.Tablets) == 0 {
		return nil, nil
	}
	splits := make([][]byte, 0, len(manifest.Tablets)-1)
	var priorEnd *string
	for i, tb := range manifest.Tablets {
		if tb.Index != i {
			return nil, fmt.Errorf("engine: manifest tablet position %d declares index %d", i, tb.Index)
		}
		if !equalRowBoundary(tb.StartRow, priorEnd) {
			return nil, fmt.Errorf("engine: manifest tablet %d start row does not match prior end row", i)
		}
		if i+1 < len(manifest.Tablets) {
			if tb.EndRow == nil {
				return nil, fmt.Errorf("engine: manifest tablet %d has an unbounded end before the final tablet", i)
			}
			splits = append(splits, append([]byte(nil), []byte(*tb.EndRow)...))
		} else if tb.EndRow != nil {
			return nil, fmt.Errorf("engine: final manifest tablet %d must have an unbounded end", i)
		}
		priorEnd = tb.EndRow
	}
	return splits, nil
}

func equalRowBoundary(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func exportManifestVersion(manifest *RFileExportManifest) int {
	if manifest.FileFormat == tablet.FormatParquet ||
		(manifest.RFileCompatibility != "" && manifest.RFileCompatibility != "accumulo-rfile/shoal") {
		return RFileExportManifestVersion
	}
	for _, file := range manifest.RFiles {
		if (file.Format != "" && file.Format != ExportFormatRFile) ||
			(file.Role != "" && file.Role != ExportRoleAuthoritative) {
			return RFileExportManifestVersion
		}
	}
	return RFileExportManifestLegacyVersion
}

func joinBackendPath(dst storage.Backend, root, rel string) string {
	if usesBackendSeparatorJoinRoot(dst, root) {
		return strings.TrimRight(root, `/\`) + "/" + filepath.ToSlash(rel)
	}
	return filepath.Join(root, rel)
}

func usesBackendSeparatorJoinRoot(dst storage.Backend, root string) bool {
	return storage.UsesBackendPathJoin(dst, root)
}

// VerifyRFileExport verifies that every manifest entry is an authoritative,
// structurally valid RFile whose bytes match the recorded size and hash.
// Accumulo promotion and the RFile branch of local manifest import share this
// strict gate; generic local immutable/Parquet policy is intentionally
// separate.
func VerifyRFileExport(ctx context.Context, b storage.Backend, manifest *RFileExportManifest) error {
	return verifyRFileExport(ctx, b, manifest, newVerificationSnapshot)
}

type verificationSnapshot interface {
	io.ReaderAt
	io.Writer
	Sync() error
}

type verificationSnapshotFactory func() (verificationSnapshot, func() error, error)

func newVerificationSnapshot() (verificationSnapshot, func() error, error) {
	f, err := os.CreateTemp("", "shoal-rfile-verify-*.rf")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() error {
		return errors.Join(f.Close(), os.Remove(f.Name()))
	}
	return f, cleanup, nil
}

func verifyRFileExport(
	ctx context.Context,
	b storage.Backend,
	manifest *RFileExportManifest,
	snapshotFactory verificationSnapshotFactory,
) error {
	if err := validateExportManifest(manifest); err != nil {
		return err
	}
	tablets, err := verificationTablets(manifest)
	if err != nil {
		return err
	}
	for _, rf := range manifest.RFiles {
		format, err := validateExportFile(manifest, rf)
		if err != nil {
			return err
		}
		if format != storage.ImmutableFormatRFile {
			return fmt.Errorf("%w: engine: import file %q has format %q; only RFiles are queryable",
				storage.ErrImmutablePolicy, rf.DestinationPath, format)
		}
		if err := storage.ValidateImmutablePath(
			rf.DestinationPath,
			storage.ImmutableRoleAuthoritative,
			storage.ImmutablePurposeRFileImport,
		); err != nil {
			return fmt.Errorf("engine: verify %s: %w", rf.DestinationPath, err)
		}
		if err := verifyRFileExportObject(
			ctx, b, rf, tablets[rf.TabletIndex], snapshotFactory, true,
		); err != nil {
			return err
		}
	}
	return nil
}

func verifyImmutableExport(
	ctx context.Context,
	b storage.Backend,
	manifest *RFileExportManifest,
	snapshotFactory verificationSnapshotFactory,
) error {
	if err := validateExportManifest(manifest); err != nil {
		return err
	}
	tablets, err := verificationTablets(manifest)
	if err != nil {
		return err
	}
	for _, file := range manifest.RFiles {
		format, err := validateExportFile(manifest, file)
		if err != nil {
			return err
		}
		if err := storage.ValidateImmutablePath(
			file.DestinationPath,
			storage.ImmutableRoleAuthoritative,
			storage.ImmutablePurposeLocalStorage,
		); err != nil {
			return fmt.Errorf("engine: verify %s: %w", file.DestinationPath, err)
		}
		if format == storage.ImmutableFormatRFile {
			if err := verifyRFileExportObject(
				ctx, b, file, tablets[file.TabletIndex], snapshotFactory, false,
			); err != nil {
				return err
			}
			continue
		}
		if err := verifyParquetExportObject(
			ctx, b, file, tablets[file.TabletIndex], snapshotFactory,
		); err != nil {
			return err
		}
	}
	return nil
}

func verificationTablets(manifest *RFileExportManifest) ([]RFileExportTablet, error) {
	if _, err := manifestSplits(manifest); err != nil {
		return nil, err
	}
	if len(manifest.Tablets) == 0 {
		return []RFileExportTablet{{Index: 0}}, nil
	}
	return manifest.Tablets, nil
}

func validateExportManifest(manifest *RFileExportManifest) error {
	if manifest == nil {
		return fmt.Errorf("engine: nil import manifest")
	}
	if manifest.Version != RFileExportManifestLegacyVersion && manifest.Version != RFileExportManifestVersion {
		return fmt.Errorf("engine: unsupported manifest version %d", manifest.Version)
	}
	if manifest.Version == RFileExportManifestLegacyVersion &&
		exportManifestVersion(manifest) != RFileExportManifestLegacyVersion {
		return fmt.Errorf("engine: manifest version %d is valid only for authoritative RFile exports", manifest.Version)
	}
	return nil
}

func validateExportFile(manifest *RFileExportManifest, file RFileExportFile) (storage.ImmutableFormat, error) {
	if file.TabletIndex < 0 || file.TabletIndex >= manifest.tabletCount() {
		return "", fmt.Errorf("engine: import file %q references undeclared tablet index %d",
			file.DestinationPath, file.TabletIndex)
	}
	role := file.Role
	if role == "" {
		role = ExportRoleAuthoritative
	}
	if role != ExportRoleAuthoritative {
		return "", fmt.Errorf("engine: import file %q has role %q; only authoritative files are queryable",
			file.DestinationPath, role)
	}
	format := storage.ImmutableFormat(file.Format)
	if format == "" {
		format = storage.ImmutableFormatRFile
	}
	if format != storage.ImmutableFormatRFile && format != storage.ImmutableFormatParquet {
		return "", fmt.Errorf("%w: engine: import file %q has unsupported format %q",
			storage.ErrImmutablePolicy, file.DestinationPath, format)
	}
	pathFormat := storage.ImmutableFormatForPath(file.DestinationPath)
	if format != pathFormat {
		return "", fmt.Errorf("%w: engine: import file %q declares format %q but path has format %q",
			storage.ErrImmutablePolicy, file.DestinationPath, format, pathFormat)
	}
	return format, nil
}

func verifyRFileExportObject(
	ctx context.Context,
	b storage.Backend,
	rf RFileExportFile,
	tablet RFileExportTablet,
	snapshotFactory verificationSnapshotFactory,
	allowNamedLocalityGroups bool,
) (err error) {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("engine: verify %s: %w", rf.DestinationPath, err)
	}
	if rf.Size < 0 {
		return fmt.Errorf("engine: verify %s: negative manifest size %d", rf.DestinationPath, rf.Size)
	}
	f, err := b.Open(ctx, rf.DestinationPath)
	if err != nil {
		return fmt.Errorf("engine: verify open %s: %w", rf.DestinationPath, err)
	}
	sourceOpen := true
	defer func() {
		if sourceOpen {
			if closeErr := f.Close(); closeErr != nil {
				err = errors.Join(err,
					fmt.Errorf("engine: verify close %s: %w", rf.DestinationPath, closeErr))
			}
		}
	}()

	size := f.Size()
	if size != rf.Size {
		return fmt.Errorf("engine: verify %s: size %d, want %d", rf.DestinationPath, size, rf.Size)
	}

	local, cleanup, err := snapshotFactory()
	if err != nil {
		return fmt.Errorf("engine: create verification snapshot for %s: %w", rf.DestinationPath, err)
	}
	defer func() {
		if cleanupErr := cleanup(); cleanupErr != nil {
			err = errors.Join(err,
				fmt.Errorf("engine: cleanup verification snapshot for %s: %w",
					rf.DestinationPath, cleanupErr))
		}
	}()

	copied, sum, copyErr := materializeVerificationSnapshot(ctx, f, local, size)
	closeErr := f.Close()
	sourceOpen = false
	if copyErr != nil {
		err = fmt.Errorf("engine: materialize verification snapshot for %s: %w",
			rf.DestinationPath, copyErr)
		if closeErr != nil {
			err = errors.Join(err,
				fmt.Errorf("engine: verify close %s: %w", rf.DestinationPath, closeErr))
		}
		return err
	}
	if closeErr != nil {
		return fmt.Errorf("engine: verify close %s: %w", rf.DestinationPath, closeErr)
	}
	if copied != size {
		return fmt.Errorf("engine: verify %s: copied size %d, want %d",
			rf.DestinationPath, copied, size)
	}
	if sum != rf.SHA256 {
		return fmt.Errorf("engine: verify %s: sha256 %s, want %s", rf.DestinationPath, sum, rf.SHA256)
	}

	snapshot := &verificationReaderAt{ctx: ctx, source: local}
	version, err := validateRFileSnapshot(
		ctx, snapshot, size, tablet, allowNamedLocalityGroups,
	)
	if err != nil {
		if readErr := snapshot.firstError(); readErr != nil {
			return fmt.Errorf("engine: read verification snapshot for %s: %w",
				rf.DestinationPath, readErr)
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("engine: verify %s: %w", rf.DestinationPath, err)
		}
		if errors.Is(err, errNamedLocalityGroupUnsupported) {
			return fmt.Errorf("%w: engine: verify %s: %w",
				storage.ErrImmutablePolicy, rf.DestinationPath, err)
		}
		return fmt.Errorf("%w: %s is not a structurally valid RFile: %w",
			storage.ErrImmutablePolicy, rf.DestinationPath, err)
	}
	if rf.BCFileVersion != "" && version != rf.BCFileVersion {
		return fmt.Errorf("%w: engine: verify %s: BCFile version %s, want %s",
			storage.ErrImmutablePolicy, rf.DestinationPath, version, rf.BCFileVersion)
	}
	return nil
}

func verifyParquetExportObject(
	ctx context.Context,
	b storage.Backend,
	file RFileExportFile,
	tablet RFileExportTablet,
	snapshotFactory verificationSnapshotFactory,
) (err error) {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("engine: verify %s: %w", file.DestinationPath, err)
	}
	if file.Size < 0 {
		return fmt.Errorf("engine: verify %s: negative manifest size %d", file.DestinationPath, file.Size)
	}
	source, err := b.Open(ctx, file.DestinationPath)
	if err != nil {
		return fmt.Errorf("engine: verify open %s: %w", file.DestinationPath, err)
	}
	sourceOpen := true
	defer func() {
		if sourceOpen {
			if closeErr := source.Close(); closeErr != nil {
				err = errors.Join(err,
					fmt.Errorf("engine: verify close %s: %w", file.DestinationPath, closeErr))
			}
		}
	}()
	if size := source.Size(); size != file.Size {
		return fmt.Errorf("engine: verify %s: size %d, want %d",
			file.DestinationPath, size, file.Size)
	}

	local, cleanup, err := snapshotFactory()
	if err != nil {
		return fmt.Errorf("engine: create verification snapshot for %s: %w", file.DestinationPath, err)
	}
	defer func() {
		if cleanupErr := cleanup(); cleanupErr != nil {
			err = errors.Join(err,
				fmt.Errorf("engine: cleanup verification snapshot for %s: %w",
					file.DestinationPath, cleanupErr))
		}
	}()

	copied, sum, copyErr := materializeVerificationSnapshot(ctx, source, local, file.Size)
	closeErr := source.Close()
	sourceOpen = false
	if copyErr != nil {
		err = fmt.Errorf("engine: materialize verification snapshot for %s: %w",
			file.DestinationPath, copyErr)
		if closeErr != nil {
			err = errors.Join(err,
				fmt.Errorf("engine: verify close %s: %w", file.DestinationPath, closeErr))
		}
		return err
	}
	if closeErr != nil {
		return fmt.Errorf("engine: verify close %s: %w", file.DestinationPath, closeErr)
	}
	if copied != file.Size {
		return fmt.Errorf("engine: verify %s: copied size %d, want %d",
			file.DestinationPath, copied, file.Size)
	}
	if sum != file.SHA256 {
		return fmt.Errorf("engine: verify %s: sha256 %s, want %s",
			file.DestinationPath, sum, file.SHA256)
	}

	snapshot := &verificationReaderAt{ctx: ctx, source: local}
	err = parquetfile.Validate(snapshot, file.Size, func(key *wire.Key) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !rowBelongsToTablet(key.Row, tablet) {
			return fmt.Errorf("row %x outside manifest tablet %d bounds", key.Row, tablet.Index)
		}
		return nil
	})
	if err != nil {
		if readErr := snapshot.firstError(); readErr != nil {
			return fmt.Errorf("engine: read verification snapshot for %s: %w",
				file.DestinationPath, readErr)
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("engine: verify %s: %w", file.DestinationPath, err)
		}
		return fmt.Errorf("%w: %s is not a structurally valid Parquet file: %w",
			storage.ErrImmutablePolicy, file.DestinationPath, err)
	}
	return nil
}

func materializeVerificationSnapshot(
	ctx context.Context,
	src io.ReaderAt,
	dst verificationSnapshot,
	size int64,
) (int64, string, error) {
	h := sha256.New()
	buf := make([]byte, verificationSnapshotChunkSize)
	var off int64
	for off < size {
		if err := ctx.Err(); err != nil {
			return off, "", err
		}
		remaining := size - off
		want := len(buf)
		if remaining < int64(want) {
			want = int(remaining)
		}
		n, readErr := src.ReadAt(buf[:want], off)
		if n < 0 || n > want {
			return off, "", fmt.Errorf("invalid ReadAt count %d for %d-byte request", n, want)
		}
		if n > 0 {
			if err := ctx.Err(); err != nil {
				return off, "", err
			}
			if err := writeVerificationSnapshot(dst, buf[:n]); err != nil {
				return off, "", err
			}
			_, _ = h.Write(buf[:n])
			off += int64(n)
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) && off == size {
				break
			}
			return off, "", readErr
		}
		if n == 0 {
			return off, "", io.ErrNoProgress
		}
	}
	if err := ctx.Err(); err != nil {
		return off, "", err
	}
	if err := dst.Sync(); err != nil {
		return off, "", err
	}
	return off, hex.EncodeToString(h.Sum(nil)), nil
}

func writeVerificationSnapshot(dst io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := dst.Write(p)
		if n < 0 || n > len(p) {
			return fmt.Errorf("invalid Write count %d for %d-byte request", n, len(p))
		}
		if n > 0 {
			p = p[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func validateRFileSnapshot(
	ctx context.Context,
	src io.ReaderAt,
	size int64,
	tablet RFileExportTablet,
	allowNamedLocalityGroups bool,
) (string, error) {
	bc, err := bcfile.NewReader(src, size)
	if err != nil {
		return "", err
	}
	readers, err := rfile.OpenAll(bc, block.Default())
	if err != nil {
		return "", err
	}
	defer func() {
		for _, reader := range readers {
			_ = reader.Close()
		}
	}()
	for group, reader := range readers {
		if !allowNamedLocalityGroups && !reader.LocalityGroup().IsDefault {
			return "", fmt.Errorf(
				"%w: locality group %d contains data that embedded tablet scans cannot read",
				errNamedLocalityGroupUnsupported, group,
			)
		}
		if err := reader.Seek(nil); err != nil {
			return "", fmt.Errorf("seek locality group %d: %w", group, err)
		}
		for {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			key, _, err := reader.Next()
			if err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				return "", fmt.Errorf("scan locality group %d: %w", group, err)
			}
			if !rowBelongsToTablet(key.Row, tablet) {
				return "", fmt.Errorf(
					"locality group %d contains row %x outside manifest tablet %d bounds",
					group, key.Row, tablet.Index,
				)
			}
		}
	}
	return bc.Footer().Version.String(), nil
}

var errNamedLocalityGroupUnsupported = errors.New(
	"named RFile locality groups are unsupported for embedded imports",
)

func rowBelongsToTablet(row []byte, tablet RFileExportTablet) bool {
	if tablet.StartRow != nil && bytes.Compare(row, []byte(*tablet.StartRow)) < 0 {
		return false
	}
	return tablet.EndRow == nil || bytes.Compare(row, []byte(*tablet.EndRow)) < 0
}

type verificationReaderAt struct {
	ctx    context.Context
	source io.ReaderAt

	mu  sync.Mutex
	err error
}

func (r *verificationReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if err := r.ctx.Err(); err != nil {
		r.recordError(err)
		return 0, err
	}
	n, err := r.source.ReadAt(p, off)
	if err != nil {
		r.recordError(err)
	}
	return n, err
}

func (r *verificationReaderAt) recordError(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err == nil {
		r.err = err
	}
}

func (r *verificationReaderAt) firstError() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// ImportRFileManifest verifies the manifest's immutable files and makes them
// queryable in this engine. The files are expected to already be present at
// their DestinationPath on this engine's backend (export places them there).
// Import copies each verified object to an engine-owned, content-addressed path
// and registers that exact copy so later source replacement cannot change the
// imported table.
//
// Fan-in: a second import of a table this engine already serves MERGES the
// manifest's RFiles into the open table instead of dropping them. Because
// RFiles are immutable and uniquely named, merging is just a re-discovery of
// the tablet directories, which is idempotent and deduped — re-importing an
// unchanged manifest is a no-op, and importing a producer's freshly shipped
// RFiles makes them visible without a reopen. This is what lets many local
// agents export the same logical table into one cluster engine safely (with
// per-cell tenant visibility stamps providing isolation; see
// RFileExportOptions.StampVisibilityLabel).
func (e *Engine) ImportRFileManifest(ctx context.Context, manifest *RFileExportManifest) (err error) {
	if manifest == nil {
		return errors.New("engine: nil RFile import manifest")
	}
	if err := validateImportedTableName(manifest.SourceTable); err != nil {
		return err
	}
	if err := verifyImmutableExport(ctx, e.backend, manifest, newVerificationSnapshot); err != nil {
		return err
	}
	splits, err := manifestSplits(manifest)
	if err != nil {
		return err
	}
	format, err := tablet.ParseFileFormat(string(manifest.FileFormat))
	if err != nil {
		return err
	}
	stagedFiles, err := stageVerifiedImportFiles(ctx, e.backend, manifest.RFiles)
	if err != nil {
		return err
	}
	rollback := newStagedImportRollback(e.backend, stagedFiles)
	defer func() {
		if err != nil {
			err = rollback.cleanup(err)
		}
	}()
	tableDir := filepath.Join(e.dir, manifest.SourceTable)
	for _, tb := range manifest.Tablets {
		if err := os.MkdirAll(filepath.Join(tableDir, fmt.Sprintf("t-%04d", tb.Index)), 0o755); err != nil {
			return fmt.Errorf("engine: mkdir imported tablet: %w", err)
		}
	}
	if len(manifest.Tablets) == 0 {
		if err := os.MkdirAll(filepath.Join(tableDir, "t-0000"), 0o755); err != nil {
			return fmt.Errorf("engine: mkdir imported tablet: %w", err)
		}
	}
	filesByTablet := make(map[int][]tablet.ImportedFile)
	for _, file := range stagedFiles {
		filesByTablet[file.TabletIndex] = append(filesByTablet[file.TabletIndex], tablet.ImportedFile{
			Path:   file.DestinationPath,
			Source: importSourceIdentity(file),
		})
	}
	importedPaths := func(files []tablet.ImportedFile) []string {
		paths := make([]string, len(files))
		for i, file := range files {
			paths[i] = file.Path
		}
		return paths
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if existing, exists := e.tables[manifest.SourceTable]; exists {
		if len(splits) != len(existing.splits) {
			return fmt.Errorf("engine: cannot merge import for table %q: manifest has %d tablet(s), open table has %d (divergent splits unsupported)",
				manifest.SourceTable, len(splits)+1, len(existing.tablets))
		}
		for i := range splits {
			if !bytes.Equal(splits[i], existing.splits[i]) {
				return fmt.Errorf("engine: cannot merge import for table %q: split %d is %x, open table has %x",
					manifest.SourceTable, i, splits[i], existing.splits[i])
			}
		}
		for i := range existing.tablets {
			adopted, registerErr := existing.tablets[i].RegisterImmutableFiles(filesByTablet[i])
			rollback.release(adopted)
			if registerErr != nil {
				return fmt.Errorf("engine: merge import for table %q tablet %d: %w",
					manifest.SourceTable, i, registerErr)
			}
		}
		return nil
	}

	if err := writeTableManifest(tableDir, tableManifest{
		Version:    tableManifestVersion,
		Splits:     splits,
		FileFormat: format,
	}); err != nil {
		return err
	}
	for i := 0; i < manifest.tabletCount(); i++ {
		tabletDir := filepath.Join(tableDir, fmt.Sprintf("t-%04d", i))
		publishErr := tablet.PublishImmutableFiles(e.backend, tabletDir, filesByTablet[i])
		if publishErr == nil || storage.IsCommittedWriteError(publishErr) {
			rollback.release(importedPaths(filesByTablet[i]))
		}
		if publishErr != nil {
			return fmt.Errorf("engine: publish imported tablet %d manifest: %w", i, publishErr)
		}
	}
	tbl, err := openTable(tableDir, manifest.SourceTable, e.logger, e.cache, e.walSyncMode, e.walSyncInterval, e.backend, e.publishRFile)
	if err != nil {
		return err
	}
	e.tables[manifest.SourceTable] = tbl
	return nil
}

type stagedImportRollback struct {
	remover storage.Remover
	pending map[string]struct{}
}

func newStagedImportRollback(b storage.Backend, files []RFileExportFile) *stagedImportRollback {
	rollback := &stagedImportRollback{
		pending: make(map[string]struct{}, len(files)),
	}
	rollback.remover, _ = b.(storage.Remover)
	for _, file := range files {
		rollback.pending[file.DestinationPath] = struct{}{}
	}
	return rollback
}

func (r *stagedImportRollback) release(paths []string) {
	for _, path := range paths {
		delete(r.pending, path)
	}
}

func (r *stagedImportRollback) cleanup(cause error) error {
	if len(r.pending) == 0 {
		return cause
	}
	if r.remover == nil {
		return errors.Join(cause, storage.ErrRemoverUnsupported)
	}
	paths := make([]string, 0, len(r.pending))
	for path := range r.pending {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, path := range paths {
		if err := r.remover.Remove(cleanupCtx, path); err != nil {
			cause = errors.Join(cause,
				fmt.Errorf("engine: rollback staged import %s: %w", path, err))
		}
	}
	return cause
}

func validateImportedTableName(name string) error {
	if name == "" || name == "." || name == ".." ||
		filepath.IsAbs(name) || filepath.VolumeName(name) != "" ||
		strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("engine: invalid imported table name %q: must be a non-empty single path component", name)
	}
	return nil
}

func stageVerifiedImportFiles(
	ctx context.Context,
	b storage.Backend,
	files []RFileExportFile,
) ([]RFileExportFile, error) {
	if _, ok := b.(storage.WritableBackend); !ok {
		return nil, fmt.Errorf("%w: engine: importing verified files requires a writable backend",
			storage.ErrReadOnly)
	}
	if _, ok := b.(storage.Remover); !ok {
		return nil, fmt.Errorf("%w: engine: importing verified files requires deletion support",
			storage.ErrRemoverUnsupported)
	}
	staged := append([]RFileExportFile(nil), files...)
	created := make([]string, 0, len(staged))
	for i := range staged {
		sourcePath := staged[i].DestinationPath
		extension := filepath.Ext(sourcePath)
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return nil, cleanupFailedStagedImports(ctx, b, created,
				fmt.Errorf("engine: generate staged import path: %w", err))
		}
		stagedPath := joinBackendPath(
			b,
			backendParentPath(sourcePath),
			".shoal-import-"+staged[i].SHA256+"-"+hex.EncodeToString(nonce[:])+extension,
		)
		created = append(created, stagedPath)
		written, sum, _, err := copyWithSHA256(ctx, b, sourcePath, b, stagedPath)
		if err != nil {
			return nil, cleanupFailedStagedImports(ctx, b, created,
				fmt.Errorf("engine: stage verified import %s: %w", sourcePath, err))
		}
		if written != staged[i].Size || sum != staged[i].SHA256 {
			return nil, cleanupFailedStagedImports(ctx, b, created, fmt.Errorf(
				"engine: stage verified import %s changed after verification: size=%d sha256=%s",
				sourcePath, written, sum,
			))
		}
		storedSize, storedSum, err := hashObject(ctx, b, stagedPath)
		if err != nil {
			return nil, cleanupFailedStagedImports(ctx, b, created,
				fmt.Errorf("engine: verify staged import %s: %w", stagedPath, err))
		}
		if storedSize != staged[i].Size || storedSum != staged[i].SHA256 {
			return nil, cleanupFailedStagedImports(ctx, b, created, fmt.Errorf(
				"engine: staged import %s does not match verified bytes: size=%d sha256=%s",
				stagedPath, storedSize, storedSum,
			))
		}
		staged[i].DestinationPath = stagedPath
	}
	return staged, nil
}

// importSourceIdentity is a manifest file's PRODUCER identity
// (tablet.ImportedFile.Source): which file of which producer table it is,
// plus its content digest. The relative path is relative to the producer's
// engine directory — table, tablet, optional producer prefix and the file
// name — so it survives the producer's data directory moving (the absolute
// source path deliberately does not take part). A producer never reissues a
// file name for different bytes (flush and compaction names stay above a
// persisted high-water mark; import names carry a nonce), so two producer
// files with identical bytes get different identities and the same producer
// file always gets the same one. A manifest entry with no relative path has
// none (empty), and the importer falls back to its content digest.
func importSourceIdentity(file RFileExportFile) string {
	if file.RelativePath == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(file.RelativePath + "\x00" + file.SHA256))
	return "v1:" + hex.EncodeToString(sum[:])
}

func cleanupFailedStagedImports(
	ctx context.Context,
	b storage.Backend,
	paths []string,
	cause error,
) error {
	remover, ok := b.(storage.Remover)
	if !ok {
		return errors.Join(cause, storage.ErrRemoverUnsupported)
	}
	for _, path := range paths {
		if err := remover.Remove(ctx, path); err != nil {
			cause = errors.Join(cause,
				fmt.Errorf("engine: remove failed staged import %s: %w", path, err))
		}
	}
	return cause
}

func backendParentPath(path string) string {
	index := strings.LastIndexAny(path, `/\`)
	if index < 0 {
		return ""
	}
	return path[:index]
}

// copyOrStampRFile copies an RFile byte-for-byte, or — when a tenant
// visibility label is configured — rewrites it through the visibilityStamp
// compaction iterator so every cell carries the label. Returns the written
// size, the destination SHA256, and the destination BCFile version.
func copyOrStampRFile(ctx context.Context, src storage.Backend, srcPath string, dst storage.Backend, dstPath string, opts RFileExportOptions) (int64, string, string, error) {
	if opts.StampVisibilityLabel == "" {
		return copyWithSHA256(ctx, src, srcPath, dst, dstPath)
	}
	return stampRFileObject(ctx, src, srcPath, dst, dstPath, opts.StampVisibilityLabel, opts.StampMode)
}

// stampRFileObject reads the whole source RFile, runs a single-input
// compaction that stamps every cell's ColumnVisibility with label, and
// writes the resulting RFile to dst. The rewrite necessarily changes the
// object bytes (and thus the SHA256), which is expected: the stamped copy is
// a distinct, tenant-scoped artifact, not a byte-identical mirror.
func stampRFileObject(ctx context.Context, src storage.Backend, srcPath string, dst storage.Backend, dstPath, label, mode string) (int64, string, string, error) {
	data, err := storage.ReadAll(ctx, src, srcPath)
	if err != nil {
		return 0, "", "", fmt.Errorf("engine: read export source %s: %w", srcPath, err)
	}
	stack := []iterrt.IterSpec{{
		Name: iterrt.IterVisibilityStamp,
		Options: map[string]string{
			iterrt.VisibilityStampLabelOption: label,
			iterrt.VisibilityStampModeOption:  mode,
		},
	}}
	res, err := compaction.Compact(compaction.Spec{
		Inputs:       []compaction.Input{{Name: srcPath, Bytes: data}},
		Stack:        stack,
		Scope:        iterrt.ScopeMajc,
		Codec:        block.CodecSnappy,
		OutputFormat: string(fileFormatForPath(srcPath)),
	})
	if err != nil {
		return 0, "", "", fmt.Errorf("engine: stamp export source %s: %w", srcPath, err)
	}
	if err := storage.WriteAll(ctx, dst, dstPath, res.Output); err != nil {
		return 0, "", "", fmt.Errorf("engine: write stamped export %s: %w", dstPath, err)
	}
	sum := sha256.Sum256(res.Output)
	bcVersion := ""
	if footer, ferr := bcfile.ReadFooter(bytes.NewReader(res.Output), int64(len(res.Output))); ferr == nil {
		bcVersion = footer.Version.String()
	}
	return int64(len(res.Output)), hex.EncodeToString(sum[:]), bcVersion, nil
}

func fileFormatForPath(path string) tablet.FileFormat {
	if filepath.Ext(path) == ".parquet" {
		return tablet.FormatParquet
	}
	return tablet.FormatRFile
}

func exportCompatibility(files []tableRFile) string {
	var rfile, parquet bool
	for _, file := range files {
		switch fileFormatForPath(file.Path) {
		case tablet.FormatParquet:
			parquet = true
		default:
			rfile = true
		}
	}
	switch {
	case rfile && parquet:
		return "mixed-rfile-parquet/shoal"
	case parquet:
		return "parquet/shoal"
	default:
		return "accumulo-rfile/shoal"
	}
}

func copyWithSHA256(ctx context.Context, src storage.Backend, srcPath string, dst storage.Backend, dstPath string) (written int64, sum string, bcVersion string, err error) {
	wb, ok := dst.(storage.WritableBackend)
	if !ok {
		return 0, "", "", storage.ErrReadOnly
	}
	in, err := src.Open(ctx, srcPath)
	if err != nil {
		return 0, "", "", fmt.Errorf("engine: open export source %s: %w", srcPath, err)
	}
	defer in.Close()
	footer, ferr := bcfile.ReadFooter(in, in.Size())
	if ferr == nil {
		bcVersion = footer.Version.String()
	}
	out, err := wb.Create(ctx, dstPath)
	if err != nil {
		return 0, "", "", fmt.Errorf("engine: create export destination %s: %w", dstPath, err)
	}
	var cleanupState storage.WriteCleanupState
	defer func() { storage.AbortOnError(&err, out, &cleanupState) }()
	h := sha256.New()
	buf := make([]byte, 256*1024)
	for written < in.Size() {
		want := int64(len(buf))
		if written+want > in.Size() {
			want = in.Size() - written
		}
		n, rerr := in.ReadAt(buf[:want], written)
		if n > 0 {
			chunk := buf[:n]
			wrote, werr := out.Write(chunk)
			if wrote > 0 {
				_, _ = h.Write(chunk[:wrote])
				written += int64(wrote)
			}
			if werr != nil {
				return written, "", "", fmt.Errorf("engine: write export %s: %w", dstPath, werr)
			}
			if wrote != len(chunk) {
				return written, "", "", fmt.Errorf("engine: write export %s: %w", dstPath, io.ErrShortWrite)
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				if written < in.Size() {
					return written, "", "", fmt.Errorf("engine: read export %s: %w", srcPath, io.ErrUnexpectedEOF)
				}
				break
			}
			return written, "", "", fmt.Errorf("engine: read export %s: %w", srcPath, rerr)
		}
	}
	if written != in.Size() {
		return written, "", "", fmt.Errorf("engine: read export %s: %w", srcPath, io.ErrUnexpectedEOF)
	}
	sum = hex.EncodeToString(h.Sum(nil))
	cleanupState.MarkCloseAttempted()
	if err = out.Close(); err != nil {
		return written, "", "", fmt.Errorf("engine: close export destination %s: %w", dstPath, err)
	}
	return written, sum, bcVersion, nil
}

func hashObject(ctx context.Context, b storage.Backend, path string) (int64, string, error) {
	f, err := b.Open(ctx, path)
	if err != nil {
		return 0, "", fmt.Errorf("engine: verify open %s: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 256*1024)
	var off int64
	for off < f.Size() {
		want := int64(len(buf))
		if off+want > f.Size() {
			want = f.Size() - off
		}
		n, readErr := f.ReadAt(buf[:want], off)
		if n > 0 {
			_, _ = h.Write(buf[:n])
			off += int64(n)
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return off, "", fmt.Errorf("engine: verify read %s: %w", path, readErr)
		}
	}
	return off, hex.EncodeToString(h.Sum(nil)), nil
}

type tableRFile struct {
	TabletIndex int
	Path        string
}

func (t *table) rfiles() []tableRFile {
	var out []tableRFile
	for i, tab := range t.tablets {
		tabFiles := tab.RFiles()
		for _, p := range tabFiles {
			out = append(out, tableRFile{TabletIndex: i, Path: p})
		}
	}
	// Within a tablet, keep the tablet's recency order (oldest first), not
	// name order: an export manifest lists files in this order and an
	// import registers them in it, so the importing engine resolves keys
	// rewritten with the same timestamp to the same (newest) value (#692).
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].TabletIndex < out[j].TabletIndex
	})
	return out
}

func (t *table) exportTablets() []RFileExportTablet {
	out := make([]RFileExportTablet, len(t.tablets))
	for i := range t.tablets {
		out[i] = RFileExportTablet{Index: i}
		if i > 0 && i-1 < len(t.splits) {
			s := string(t.splits[i-1])
			out[i].StartRow = &s
		}
		if i < len(t.splits) {
			s := string(t.splits[i])
			out[i].EndRow = &s
		}
	}
	return out
}
