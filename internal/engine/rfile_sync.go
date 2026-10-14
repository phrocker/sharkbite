package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"time"

	"github.com/phrocker/shoal-oss/internal/storage"
	"github.com/phrocker/shoal-oss/internal/tablet"
)

// SyncStateVersion is the on-disk schema version of a SyncState file.
const SyncStateVersion = 1

// SyncState is the persisted watermark for incremental RFile shipping. RFiles
// are immutable and uniquely named (F<millis>.rf / C<millis>.rf), so a file's
// relative path is a stable identity: once shipped it never changes, and
// compaction produces new paths while retiring old ones. Shipped maps each
// already-uploaded RFile's slash-relative path to its manifest entry, so an
// unchanged file can be re-listed in the next manifest without re-reading or
// re-hashing it.
type SyncState struct {
	Version   int                        `json:"version"`
	Table     string                     `json:"table"`
	Sequence  int64                      `json:"sequence"`
	UpdatedAt time.Time                  `json:"updated_at"`
	Shipped   map[string]RFileExportFile `json:"shipped"`
}

// IncrementalExportResult reports what a single sync tick did.
type IncrementalExportResult struct {
	Manifest     *RFileExportManifest
	State        *SyncState
	Uploaded     []string // slash-relative paths newly copied this tick
	Skipped      []string // slash-relative paths already shipped (reused)
	Retired      []string // slash-relative paths shipped before but no longer present
	ManifestPath string   // authoritative latest-manifest path on the destination
}

// exportRelPath computes the slash-relative path used both as the manifest's
// RelativePath and (joined with the destination root) as the destination
// object path. Shared by full and incremental export so they agree byte-for-byte.
//
// When producerID is non-empty the destination *base name* is prefixed with
// "<producer>~" so files minted by different producers at the same millisecond
// (F<ms>.rf / C<ms>.rf) never collide on a shared destination. The file stays
// in its tablet directory and keeps its .rf extension, so import re-discovery
// (os.ReadDir / prefix-list) still finds it.
func (e *Engine) exportRelPath(f tableRFile, tableName, producerID string) string {
	rel, err := filepath.Rel(e.dir, f.Path)
	if err != nil || rel == "." || rel == "" {
		rel = filepath.Join(tableName, filepath.Base(filepath.Dir(f.Path)), filepath.Base(f.Path))
	}
	rel = filepath.ToSlash(rel)
	if producerID != "" {
		dir, base := path.Split(rel)
		rel = dir + producerID + "~" + base
	}
	return rel
}

// ExportRFilesIncremental ships only the RFiles that aren't already at the
// destination, while still writing a complete manifest snapshot describing the
// table's current file set. Pass the prior SyncState (or nil on the first run);
// the returned State should be persisted and fed back on the next tick.
//
// Already-shipped RFiles (matched by relative path against prior.Shipped) are
// neither re-read nor re-uploaded — their manifest entry is reused verbatim.
// This makes repeated ticks cheap: only newly flushed/compacted RFiles incur IO.
// Besides the latest manifest at ManifestPath, a sequence-stamped
// manifest-<NNNNNN>.json snapshot is written for history/audit.
func (e *Engine) ExportRFilesIncremental(ctx context.Context, tableName string, dst storage.Backend, opts RFileExportOptions, prior *SyncState) (*IncrementalExportResult, error) {
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

	priorShipped := map[string]RFileExportFile{}
	var priorSeq int64
	if prior != nil {
		priorSeq = prior.Sequence
		if prior.Shipped != nil {
			priorShipped = prior.Shipped
		}
	}

	state := &SyncState{
		Version:  SyncStateVersion,
		Table:    tableName,
		Sequence: priorSeq + 1,
		Shipped:  map[string]RFileExportFile{},
	}
	files := tbl.rfiles()
	manifest := &RFileExportManifest{
		Version:             RFileExportManifestVersion,
		CreatedAt:           time.Now().UTC(),
		SourceTable:         tableName,
		EngineVersion:       opts.EngineVersion,
		RFileCompatibility:  exportCompatibility(files),
		FileFormat:          configuredFormat,
		CFSchema:            opts.CFSchema,
		VisibilityStamp:     opts.VisibilityStamp,
		AuthorizationsStamp: opts.AuthorizationsStamp,
		Tablets:             tbl.exportTablets(),
	}
	opts.applyStampDefaults(manifest)
	var uploaded, skipped []string
	current := make(map[string]bool, len(files))
	for _, f := range files {
		rel := e.exportRelPath(f, tableName, opts.ProducerID)
		dstPath := joinBackendPath(dst, opts.DestinationRoot, filepath.FromSlash(rel))
		current[rel] = true

		// Skipping by path is sound because a tablet never reuses a file
		// name: local names stay above the persisted name high-water mark
		// (even after deletion or a clock step back), and import names carry
		// a random nonce. A path therefore always names the same bytes.
		if prev, ok := priorShipped[rel]; ok && prev.DestinationPath == dstPath {
			prev.Format = string(fileFormatForPath(f.Path))
			prev.Role = ExportRoleAuthoritative
			manifest.RFiles = append(manifest.RFiles, prev)
			state.Shipped[rel] = prev
			skipped = append(skipped, rel)
			continue
		}

		size, sum, bcVersion, err := copyOrStampRFile(ctx, e.backend, f.Path, dst, dstPath, opts)
		if err != nil {
			return nil, err
		}
		entry := RFileExportFile{
			TabletIndex:     f.TabletIndex,
			SourcePath:      f.Path,
			DestinationPath: dstPath,
			RelativePath:    rel,
			Size:            size,
			SHA256:          sum,
			BCFileVersion:   bcVersion,
			Format:          string(fileFormatForPath(f.Path)),
			Role:            ExportRoleAuthoritative,
		}
		manifest.RFiles = append(manifest.RFiles, entry)
		state.Shipped[rel] = entry
		uploaded = append(uploaded, rel)
	}

	var retired []string
	for rel := range priorShipped {
		if !current[rel] {
			retired = append(retired, rel)
		}
	}

	// Group by tablet but keep each tablet's recency order (from rfiles):
	// an importer registers files in manifest order, and that order is how
	// it resolves keys rewritten with the same timestamp (#692).
	sort.SliceStable(manifest.RFiles, func(i, j int) bool {
		return manifest.RFiles[i].TabletIndex < manifest.RFiles[j].TabletIndex
	})
	manifest.Version = exportManifestVersion(manifest)
	sort.Strings(uploaded)
	sort.Strings(skipped)
	sort.Strings(retired)
	state.UpdatedAt = time.Now().UTC()

	manifestPath := opts.ManifestPath
	if manifestPath == "" {
		manifestPath = joinBackendPath(dst, opts.DestinationRoot, "manifest.json")
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("engine: marshal manifest: %w", err)
	}
	data = append(data, '\n')
	if err := storage.WriteAll(ctx, dst, manifestPath, data); err != nil {
		return nil, fmt.Errorf("engine: write manifest %s: %w", manifestPath, err)
	}
	seqPath := joinBackendPath(dst, opts.DestinationRoot, fmt.Sprintf("manifest-%06d.json", state.Sequence))
	if err := storage.WriteAll(ctx, dst, seqPath, data); err != nil {
		return nil, fmt.Errorf("engine: write manifest snapshot %s: %w", seqPath, err)
	}

	return &IncrementalExportResult{
		Manifest:     manifest,
		State:        state,
		Uploaded:     uploaded,
		Skipped:      skipped,
		Retired:      retired,
		ManifestPath: manifestPath,
	}, nil
}

// LoadSyncState reads a SyncState JSON file. It returns (nil, nil) when the
// file does not exist, so first-run callers can pass the result straight back
// into ExportRFilesIncremental.
func LoadSyncState(path string) (*SyncState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("engine: read sync state %s: %w", path, err)
	}
	var s SyncState
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("engine: decode sync state %s: %w", path, err)
	}
	if s.Shipped == nil {
		s.Shipped = map[string]RFileExportFile{}
	}
	return &s, nil
}

// SaveSyncState writes a SyncState JSON file atomically (write-temp-then-rename),
// creating parent directories as needed.
func SaveSyncState(path string, s *SyncState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("engine: mkdir sync state dir: %w", err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("engine: marshal sync state: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("engine: write sync state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("engine: commit sync state: %w", err)
	}
	return nil
}
