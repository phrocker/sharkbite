package promotion

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/storage"
	"github.com/phrocker/shoal-oss/internal/storage/azure"
	"github.com/phrocker/shoal-oss/internal/storage/gcs"
	"github.com/phrocker/shoal-oss/internal/storage/hdfs"
	"github.com/phrocker/shoal-oss/internal/storage/local"
	"github.com/phrocker/shoal-oss/internal/storage/s3"
)

var (
	parseS3Path            = s3.ParsePath
	parseGCSPath           = gcs.ParsePath
	parseAzurePath         = azure.ParsePath
	parseCanonicalHDFSPath = url.Parse
	stagePathBackendKey    = canonicalPathBackendKey
	stageBulkDirLocks      = struct {
		sync.Mutex
		entries map[string]*stageBulkDirLock
	}{entries: make(map[string]*stageBulkDirLock)}
)

type stageBulkDirLock struct {
	ready chan struct{}
	refs  int
}

// StageBulkDir copies every RFile referenced by manifest from src (the
// export destination backend, where Engine.ExportRFiles /
// ExportRFilesIncremental already placed them, nested under per-tablet
// directories) into bulkDir on dst as a FLAT directory, and writes the
// corresponding loadmap.json describing which destination KeyExtent each
// file belongs to. Accumulo's Bulk Import V2 lists the bulk directory
// non-recursively and expects every referenced file to sit directly inside
// it (see BulkImport.java's directory listing), unlike ExportRFiles' own
// t-NNNN/ nesting.
//
// Copies (rather than moves) so a failed or interrupted stage never loses
// the source export, and re-running StageBulkDir with the same manifest and
// bulkDir reproduces byte-identical files and an identical loadmap.json —
// safe to retry.
//
// Calls in this process that target the same recognized backend location and
// bulkDir are serialized. Promote holds that ownership through BulkImport, so another
// local promotion cannot overwrite verified files between publication of
// loadmap.json and submission of the import. Distributed callers still must
// assign one immutable bulkDir per promotion; storage.Backend has no portable
// conditional-create primitive from which to build a cross-process lease.
//
// Basenames must be unique across the whole manifest once flattened; a
// collision is reported as an error before any copy happens, rather than
// silently overwriting one file with another (see package docs for the
// deferred auto-rename gap, mirroring Accumulo's own renames.json step).
//
// bulkDir itself is preflight-validated before any read or write: empty,
// whitespace-padded, or backend-root destinations fail before staging can
// mutate dst, and dst itself must implement storage.WritableBackend (see
// validateDestinationWritable) or StageBulkDir fails before its first
// storage.Copy call, rather than deep inside it. BuildLoadMapping likewise
// rejects any manifest whose
// declared tablet chain is malformed (gaps, overlaps, duplicate or
// out-of-range indexes, missing or misplaced boundaries) before staging
// starts.
//
// StageBulkDir accepts multi-tablet manifests, but — unlike Promote — it
// never talks to Accumulo and cannot reconcile the destination's actual
// tablet splits. The widened KeyExtents BuildLoadMapping computes for a
// multi-tablet manifest are only guaranteed to pass Accumulo's own
// server-side load-mapping validation if the destination already has
// splits at RequiredDestinationSplits' reported rows; a caller invoking
// StageBulkDir directly for a multi-tablet manifest, bypassing Promote's
// orchestration, is responsible for ensuring that beforehand, or the
// eventual BulkImport call will fail closed (not silently) rather than
// stage or import anything incorrectly.
//
// The manifest is verified (engine.VerifyRFileExport: every RFile exists at
// src and matches its recorded size/SHA256) before anything is copied, so a
// truncated, corrupted, or stale export manifest fails fast here rather
// than silently staging incomplete or mismatched data for Accumulo to bulk
// import. That whole-manifest verification happens once, before the copy
// loop starts, and so cannot by itself catch a source file that changes
// *during* the loop -- for example a later file mutated while an earlier
// one is still being copied, or the same file mutated in the brief window
// between this check reading it and storage.Copy separately opening it
// moments later. Closing that window would require re-verifying every
// file again immediately before its own copy, which is exactly as
// expensive as just verifying after copying and additionally cannot
// detect corruption introduced by the copy path itself (see the next
// paragraph), so StageBulkDir instead verifies each file's *destination*
// object right after it is written.
//
// Concretely: immediately after each file's storage.Copy returns,
// StageBulkDir re-opens and re-hashes the object it just wrote at dst and
// compares that against the manifest's recorded size/SHA256 for that
// file, before continuing to the next file or writing loadmap.json. This
// is deliberately a check of the object actually sitting at dst, not a
// second read of src: it catches a source mutated after the up-front
// VerifyRFileExport pass but before (or during) its own copy, and it
// additionally catches corruption introduced by the copy/backend path
// itself (storage.Copy's own doc comment notes that some WritableBackend
// implementations buffer and upload on Close, so a copy can still fail or
// corrupt data after every prior Write appeared to succeed). Like the
// cancellation case below, a mismatch here is reported and staging stops,
// but the offending file's already-written (incorrect) bytes are not
// rolled back -- StageBulkDir is not atomic across a multi-file manifest
// in either failure mode, and loadmap.json is only ever written after
// every file has both copied and re-verified successfully, so a caller
// that only trusts a bulkDir with a present, successfully-written
// loadmap.json is never exposed to a manifest describing unverified
// bytes.
//
// That guarantee is about loadmap.json written by *this* call. A bulkDir
// can also already hold a loadmap.json left behind by an earlier,
// different StageBulkDir call -- for example a retry after a prior
// attempt succeeded, or an operator re-running staging against the same
// bulkDir. If this call's own copy loop then fails partway (storage.Copy
// itself failing, or the post-copy verifyStagedRFile check below
// rejecting a mismatched destination object), StageBulkDir returns an
// error without ever reaching WriteLoadMapping -- but without further
// handling, that OLD loadmap.json would be left completely untouched,
// still claiming the directory is complete and correct even though one
// of its RFiles was just overwritten with bytes that failed
// verification. StageBulkDir closes that window by invalidating any
// pre-existing loadmap.json immediately before this loop starts
// mutating any RFile; see invalidateExistingLoadMapping's own doc
// comment for the exact mechanism. A bulkDir with no pre-existing
// loadmap.json -- the common, fresh-destination case this whole
// function is primarily documented for above -- is unaffected by this:
// there is nothing to invalidate, and no extra write occurs.
//
// This means a full Promote call over a multi-tablet manifest reads and
// hashes every RFile up to three times: once in stagingPreflight (before
// AddTableSplits), once more in StageBulkDir's own preflight above (after
// AddTableSplits/ListTableSplits, before any dst write), and once again
// per file immediately after it is copied (reading dst, not src). Each
// pass closes a different, non-overlapping window and none of the three
// is redundant with the others: removing either upfront pass would allow
// an already-corrupt manifest to still cause partial writes to dst
// before being caught; removing the post-copy pass would leave the
// per-file copy-time window this paragraph describes wide open. The
// repeated hashing is a deliberate correctness-over-efficiency tradeoff
// for a manager-authoritative promotion path, consistent with the same
// tradeoff stagingPreflight's own doc comment already describes.
//
// Every write target is also preflighted before any copy starts: every
// flattened RFile destination plus loadmap.json is compared against
// every unique source path in the manifest, and every write target is
// compared against every other write target, and every unique source
// path is also compared against every other unique source path. That
// catches in-place staging (bulkDir is the export's own tablet
// directory), a destination reached through a symlink/hard link to a
// *different* RFile's source, aliased write targets such as two staged
// filenames (or a staged filename and loadmap.json) that resolve to the
// same location, and two manifest sources that are themselves the same
// physical file reached through two different DestinationPath spellings
// (for example a symlink/hard link, or a case/Unicode-normalization
// difference). If those aliases flatten to the same basename,
// StageBulkDir dedupes them to one staged file; if they flatten to
// different basenames, StageBulkDir rejects the manifest as ambiguous
// rather than staging the same physical file twice under different
// names for Accumulo to bulk import.
//
// Those aliases are dangerous because storage.Copy opens the destination
// for writing before it reads the source bytes: on backends like local,
// the first write would truncate the aliased file in place, silently
// corrupting a source export or previously written staging output before
// StageBulkDir ever notices. StageBulkDir rejects every such case up
// front rather than destroying any source or staging path.
func StageBulkDir(
	ctx context.Context,
	src storage.Backend,
	manifest *engine.RFileExportManifest,
	dst storage.Backend,
	bulkDir string,
) (LoadMapping, error) {
	if manifest == nil {
		return nil, fmt.Errorf("promotion: nil export manifest")
	}
	if err := validateBulkDirOnBackend(dst, bulkDir); err != nil {
		return nil, err
	}
	if err := validateDestinationWritable(dst); err != nil {
		return nil, err
	}
	release, err := acquireStageBulkDir(ctx, dst, bulkDir)
	if err != nil {
		return nil, err
	}
	defer release()
	return stageBulkDirLocked(ctx, src, manifest, dst, bulkDir)
}

func stageBulkDirLocked(
	ctx context.Context,
	src storage.Backend,
	manifest *engine.RFileExportManifest,
	dst storage.Backend,
	bulkDir string,
) (LoadMapping, error) {
	if _, _, err := resolveManifestTablets(manifest); err != nil {
		return nil, err
	}
	// Verified again here even though Promote's stagingPreflight already
	// verified this same manifest once, before AddTableSplits: an
	// arbitrary amount of time, including a real manager round-trip for
	// AddTableSplits/ListTableSplits, elapses between that preflight and
	// this call, and src is not guaranteed immutable across it (a local
	// path or an object-store key can be overwritten in place). Skipping
	// this second check would let a source object replaced during that
	// window be staged and bulk-imported without ever being checked
	// against the manifest again -- see stagingPreflight's own doc
	// comment for the corresponding pre-AddTableSplits half of this, and
	// TestPromoteRejectsSourceMutatedDuringAddTableSplits for the
	// regression test proving this specific window is closed.
	if err := engine.VerifyRFileExport(ctx, src, manifest); err != nil {
		return nil, fmt.Errorf("promotion: stage: %w", err)
	}
	stageRFiles, err := dedupeStageSources(src, manifest.RFiles)
	if err != nil {
		return nil, err
	}
	stageManifest := *manifest
	stageManifest.RFiles = stageRFiles

	mapping, err := BuildLoadMapping(&stageManifest)
	if err != nil {
		return nil, err
	}
	flatNames, err := flattenNames(stageManifest.RFiles)
	if err != nil {
		return nil, err
	}
	if err := checkNoStagingAliases(src, dst, flatNames, bulkDir); err != nil {
		return nil, err
	}
	// Neutralize any loadmap.json already sitting in bulkDir from an
	// earlier, unrelated StageBulkDir call before this loop starts
	// mutating RFiles. Without this, a retry against a bulkDir that
	// already holds a valid loadmap.json from a prior successful stage
	// could fail partway through replacing one of its RFiles -- for
	// example on the verifyStagedRFile mismatch below -- leaving that
	// RFile's bytes unverified or wrong while the OLD loadmap.json,
	// never touched by this failed attempt, still asserts the directory
	// is complete and correct by the very marker rule the rest of this
	// function relies on. See invalidateExistingLoadMapping's own doc
	// comment for the mechanism and TestStageBulkDirInvalidatesStaleLoadMappingOnFailedRetry
	// for the regression test.
	if err := invalidateExistingLoadMapping(ctx, dst, bulkDir); err != nil {
		return nil, err
	}
	for _, rf := range stageManifest.RFiles {
		dstPath := joinBulkPath(dst, bulkDir, flatNames[rf.DestinationPath])
		if _, err := storage.Copy(ctx, src, rf.DestinationPath, dst, dstPath); err != nil {
			return nil, fmt.Errorf("promotion: stage %s: %w", rf.DestinationPath, err)
		}
		// Re-verify what actually landed at dst, not what src reported
		// before the copy: this closes the copy-time window described
		// in StageBulkDir's own doc comment above, where a source file
		// (or a later file, while an earlier one is still copying) can
		// change after the whole-manifest VerifyRFileExport preflight
		// above but before (or during) its own storage.Copy, and also
		// catches corruption introduced by the copy/backend path
		// itself. A mismatch fails closed here, before any further file
		// is copied and before loadmap.json is written, but -- like the
		// cancellation case -- does not roll back this file's own
		// already-written (incorrect) bytes.
		if err := verifyStagedRFile(ctx, dst, dstPath, rf.Size, rf.SHA256); err != nil {
			return nil, fmt.Errorf("promotion: stage %s: verify staged copy: %w", rf.DestinationPath, err)
		}
	}
	if err := WriteLoadMapping(ctx, dst, bulkDir, mapping); err != nil {
		return nil, err
	}
	return mapping, nil
}

func acquireStageBulkDir(ctx context.Context, dst storage.Backend, bulkDir string) (func(), error) {
	lockTarget := joinBulkPath(dst, bulkDir, bulkLoadMappingFile)
	key := canonicalPathBackendKey(dst) + "\x00" + lockTarget
	unwrapped := unwrapBackend(dst)
	if _, ok := unwrapped.(*local.Backend); ok {
		cache := newPathIdentityCache(1)
		key = fmt.Sprintf("%T", unwrapped) + "\x00" + cache.publicationKey(lockTarget)
	} else {
		ref := newStagePathRef(dst, lockTarget)
		if canonical, ok := canonicalBackendPath(ref); ok {
			key = stageLockBackendKey(dst) + "\x00" + canonical
		} else if _, ok := unwrapped.(*hdfs.Backend); ok {
			key = fmt.Sprintf("%T", unwrapped) + "\x00" + path.Clean(strings.ReplaceAll(lockTarget, `\`, "/"))
		}
	}

	stageBulkDirLocks.Lock()
	entry := stageBulkDirLocks.entries[key]
	if entry == nil {
		entry = &stageBulkDirLock{ready: make(chan struct{}, 1)}
		entry.ready <- struct{}{}
		stageBulkDirLocks.entries[key] = entry
	}

	entry.refs++
	stageBulkDirLocks.Unlock()

	select {
	case <-ctx.Done():
		releaseStageBulkDirRef(key, entry, false)
		return nil, ctx.Err()
	case <-entry.ready:
		return func() { releaseStageBulkDirRef(key, entry, true) }, nil
	}
}

func stageLockBackendKey(backend storage.Backend) string {
	unwrapped := unwrapBackend(backend)
	switch unwrapped.(type) {
	case *local.Backend, *hdfs.Backend, *s3.Backend, *gcs.Backend, *azure.Backend:
		return fmt.Sprintf("%T", unwrapped)
	default:
		return canonicalPathBackendKey(unwrapped)
	}
}

func releaseStageBulkDirRef(key string, entry *stageBulkDirLock, owned bool) {
	if owned {
		entry.ready <- struct{}{}
	}
	stageBulkDirLocks.Lock()
	entry.refs--
	if entry.refs == 0 {
		delete(stageBulkDirLocks.entries, key)
	}
	stageBulkDirLocks.Unlock()
}

// invalidateExistingLoadMapping detects a loadmap.json already present at
// bulkDir/loadmap.json on dst -- left behind by an earlier, unrelated
// StageBulkDir call against the same bulkDir -- and neutralizes it before
// StageBulkDir's copy loop starts overwriting any RFile.
//
// Without this, retrying StageBulkDir against a bulkDir that already
// holds a valid loadmap.json from a prior successful stage is unsafe: if
// this retry's copy loop fails partway (for example storage.Copy
// succeeds but verifyStagedRFile then rejects a mismatched destination
// object), StageBulkDir returns before ever reaching WriteLoadMapping,
// but the OLD loadmap.json -- written by the earlier, different call --
// is never touched by this failed one. The directory is left containing
// at least one RFile whose bytes were just replaced with something that
// failed verification, sitting next to a loadmap.json that still, by the
// documented "present means complete" marker rule, claims the directory
// is a valid, verified bulk mapping. A caller invoking StageBulkDir
// standalone and trusting that rule without separately checking this
// call's returned error, or a real Accumulo TABLE_BULK_IMPORT2 FATE
// operation reading loadmap.json directly off disk, would have no way to
// tell the directory is now inconsistent.
//
// A bulkDir with no pre-existing loadmap.json -- the common case: a
// fresh destination, or a prior attempt that itself failed before ever
// reaching WriteLoadMapping -- is left untouched; there is nothing to
// invalidate, and this adds no write. When a pre-existing loadmap.json
// is found, dst.Remove deletes it outright if dst implements
// storage.Remover (local, memory, hdfs); otherwise -- object-store
// backends such as s3/gcs/azure that expose no delete capability -- it
// is overwritten in place with a payload that is deliberately not valid
// JSON, so any reader (ReadLoadMapping, or a real Accumulo bulk v2
// client parsing loadmap.json directly) fails closed on it instead of
// silently trusting a mapping that may no longer match this directory's
// in-flight contents. Either way, the directory only regains a loadmap.json
// that can parse as a valid mapping once this call's own copy+verify loop
// below succeeds end-to-end and reaches WriteLoadMapping, exactly as
// WriteLoadMapping already guarantees for a fresh bulkDir.
func invalidateExistingLoadMapping(ctx context.Context, dst storage.Backend, bulkDir string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path := joinBulkPath(dst, bulkDir, bulkLoadMappingFile)
	existing, err := dst.Open(ctx, path)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("promotion: check existing load mapping %s: %w", path, err)
	}
	existing.Close()
	if remover, ok := dst.(storage.Remover); ok {
		if err := remover.Remove(ctx, path); err != nil {
			return fmt.Errorf("promotion: invalidate stale load mapping %s: %w", path, err)
		}
		return nil
	}
	placeholder := []byte("promotion: stage in progress; superseding a prior load mapping, not yet valid\n")
	if err := storage.WriteAll(ctx, dst, path, placeholder); err != nil {
		return fmt.Errorf("promotion: invalidate stale load mapping %s: %w", path, err)
	}
	return nil
}

// verifyStagedRFile re-opens and re-hashes the object just written at
// dstPath on dst and compares it against the manifest's recorded size and
// SHA256 for that RFile. It intentionally checks the destination object
// that will actually be bulk-imported, not a second read of the source,
// so it also catches corruption introduced by the copy/backend path
// itself (for example a WritableBackend that buffers and uploads on
// Close, per storage.Copy's own doc comment, failing or corrupting data
// after every prior Write appeared to succeed). The read-and-hash loop
// mirrors engine's own unexported hashObject helper; it is duplicated
// here in small form rather than exported from engine or added to
// storage's public surface, since this is its only call site.
//
// ctx is polled before Open and both before and immediately after every
// ReadAt, mirroring storage.Copy's own polling pattern, so cancellation
// during a large RFile's re-hash is observed within one 256KB chunk
// rather than only after the whole file has been read. As with
// storage.Copy, any ctx.Err() observed here still returns the
// cancellation itself as the failure, even on the loop's last,
// otherwise-successful read, rather than racing to decide whether the
// read "finished in time" -- keeping this function's cancellation
// semantics identical to Copy's.
func verifyStagedRFile(ctx context.Context, dst storage.Backend, dstPath string, wantSize int64, wantSHA256 string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := dst.Open(ctx, dstPath)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("open staged copy %s: %w", dstPath, errors.Join(err, ctxErr))
		}
		return fmt.Errorf("open staged copy %s: %w", dstPath, err)
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 256*1024)
	var off int64
	for off < f.Size() {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("read staged copy %s: %w", dstPath, err)
		}
		want := int64(len(buf))
		if off+want > f.Size() {
			want = f.Size() - off
		}
		n, rerr := f.ReadAt(buf[:want], off)
		if n > 0 {
			_, _ = h.Write(buf[:n])
			off += int64(n)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			if rerr != nil && !errors.Is(rerr, io.EOF) {
				return fmt.Errorf("read staged copy %s: %w", dstPath, errors.Join(rerr, ctxErr))
			}
			return fmt.Errorf("read staged copy %s: %w", dstPath, ctxErr)
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return fmt.Errorf("read staged copy %s: %w", dstPath, rerr)
		}
	}
	if off != wantSize {
		return fmt.Errorf("staged copy %s: size %d, want %d (source may have changed after the pre-copy manifest verification, or the copy path may have corrupted it)", dstPath, off, wantSize)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if sum != wantSHA256 {
		return fmt.Errorf("staged copy %s: sha256 %s, want %s (source may have changed after the pre-copy manifest verification, or the copy path may have corrupted it)", dstPath, sum, wantSHA256)
	}
	return nil
}

// checkNoStagingAliases rejects the whole stage before any copy starts
// if any write target (every flattened RFile destination plus
// loadmap.json) resolves to the same location as any unique source path,
// if any two write targets resolve to the same location, or if any two
// unique source paths resolve to the same location. Checking only the
// 1:1 source/destination pairing would miss a destination that aliases
// a *different* manifest entry's source (for example via a
// symlink/hard link), while checking only target-vs-source would
// separately miss two aliased write targets (for example two
// hard-linked/symlinked flattened destinations, or loadmap.json
// aliasing one of them) that would overwrite each other mid-stage.
// Checking only the two write-side comparisons would still miss two
// manifest sources that alias each other: each verifies and flattens to
// a distinct basename independently of the other, so neither the
// target-vs-source nor the target-vs-target check has any reason to
// reject them, yet staging both would silently duplicate one physical
// file's rows under two different bulk-import filenames.
//
// The all-pairs comparison is O(N^2) in the number of unique sources and
// write targets across all three comparisons combined, but the
// underlying local-filesystem os.Stat/symlink probes are cached in one
// pathIdentityCache shared across the whole comparison (O(N) local path
// probes overall), and remote backends are compared via backend-aware
// canonicalized path strings only. Manifests are expected to hold at
// most a few thousand RFiles per single-tablet export, so an O(N^2)
// in-memory comparison over already-cached path identities is not a
// practical concern.
func checkNoStagingAliases(src, dst storage.Backend, flatNames map[string]string, bulkDir string) error {
	srcPaths := make([]stagePathRef, 0, len(flatNames))
	targets := make([]stageWriteTarget, 0, len(flatNames)+1)
	for srcPath, flatName := range flatNames {
		srcPaths = append(srcPaths, newStagePathRef(src, srcPath))
		targets = append(targets, stageWriteTarget{
			name: flatName,
			path: joinBulkPath(dst, bulkDir, flatName),
		})
	}
	targets = append(targets, stageWriteTarget{
		name: bulkLoadMappingFile,
		path: joinBulkPath(dst, bulkDir, bulkLoadMappingFile),
	})
	targetRefs := make([]stagePathRef, len(targets))
	for i, target := range targets {
		targetRefs[i] = newStagePathRef(dst, target.path)
	}
	for _, target := range targets {
		if err := validateStagingWriteTarget(dst, target); err != nil {
			return err
		}
	}

	cache := newPathIdentityCache(len(srcPaths) + len(targets))
	for i := range srcPaths {
		for j := i + 1; j < len(srcPaths); j++ {
			if sourceRefsAlias(srcPaths[i], srcPaths[j], cache) {
				return fmt.Errorf(
					"promotion: stage: manifest sources %s and %s resolve to the same physical file; refusing to stage duplicate copies",
					srcPaths[i].path, srcPaths[j].path,
				)
			}
		}
	}
	for i, target := range targets {
		targetRef := targetRefs[i]
		for _, srcPath := range srcPaths {
			if pathsAlias(srcPath, targetRef, cache) {
				return fmt.Errorf(
					"promotion: stage: write target %s (%s) resolves to the same location as source file %s; refusing to write aliased staging output",
					target.name, target.path, srcPath.path,
				)
			}
		}
	}
	for i := range targets {
		left := targetRefs[i]
		for j := i + 1; j < len(targets); j++ {
			right := targetRefs[j]
			if pathsAlias(left, right, cache) {
				return fmt.Errorf(
					"promotion: stage: write targets %s (%s) and %s (%s) resolve to the same location; refusing to write aliased staging outputs",
					targets[i].name, targets[i].path, targets[j].name, targets[j].path,
				)
			}
		}
	}
	return nil
}

// stagingPreflight runs the same non-mutating, storage-probing
// validation StageBulkDir itself performs before it copies a single
// byte or writes loadmap.json, in the same order StageBulkDir runs it:
// verifying every RFile against its recorded size/SHA256
// (engine.VerifyRFileExport), deduping source aliases
// (dedupeStageSources), flattening every RFile to its bulk-directory
// basename (flattenNames), and checking the flattened write targets
// against src and dst for path aliases (checkNoStagingAliases).
//
// Promote calls this, and discards the result, immediately after its
// own BuildLoadMapping preflight and before AddTableSplits ever runs.
// Without it, a manifest that is only invalid at this specific layer —
// a missing file, a size/hash mismatch against the manifest, a
// cross-tablet basename collision, an invalid/non-leaf flattened
// basename, or a source/destination path alias — would only be caught
// later, inside StageBulkDir's own call to these same four functions,
// by which point AddTableSplits would already have reconciled the
// destination's splits (see Promote's own doc comment, which explains
// why the BuildLoadMapping preflight exists for the identical reason).
//
// dedupeStageSources, flattenNames, and checkNoStagingAliases are cheap
// to recompute inside StageBulkDir and never observe a different
// destination state: dedupeStageSources and checkNoStagingAliases only
// probe src/dst path identity (in-memory comparisons for remote/
// object-store backends, cached local os.Stat calls for local ones —
// see their own doc comments), and flattenNames is a pure function of
// the RFile list alone. This mirrors BuildLoadMapping's own preflight,
// which Promote also calls once early and StageBulkDir recomputes
// again further on, for the same reason.
//
// engine.VerifyRFileExport is different: it streams and hashes every
// RFile's actual bytes, so it is not cheap to recompute the way the
// other three checks are -- but unlike them, StageBulkDir is still
// deliberately left to run it again on its own, rather than having
// Promote skip it there. AddTableSplits and ListTableSplits, which run
// between this preflight and StageBulkDir, are a real manager
// round-trip taking arbitrary time, and src is not guaranteed
// immutable across it: a local path or an object-store key can be
// overwritten in place while that call is in flight. Verifying only
// here and trusting it to still hold by the time StageBulkDir copies
// would leave exactly that window unchecked -- a source object
// replaced during AddTableSplits/ListTableSplits would be staged and
// bulk-imported without ever being verified against the manifest it
// actually matches at copy time. So this preflight's verification and
// StageBulkDir's own are deliberately redundant in the common case and
// each close a different, non-overlapping window: this one guards
// AddTableSplits against ever mutating the destination's splits for an
// export that was already corrupt before either call started;
// StageBulkDir's guards the copy itself against one that became
// corrupt (or was replaced) during the calls in between. See
// TestPromoteRejectsSourceMutatedDuringAddTableSplits for the
// regression test proving the latter window specifically.
func stagingPreflight(ctx context.Context, src storage.Backend, manifest *engine.RFileExportManifest, dst storage.Backend, bulkDir string) error {
	if err := engine.VerifyRFileExport(ctx, src, manifest); err != nil {
		return fmt.Errorf("promotion: stage: %w", err)
	}
	stageRFiles, err := dedupeStageSources(src, manifest.RFiles)
	if err != nil {
		return err
	}
	flatNames, err := flattenNames(stageRFiles)
	if err != nil {
		return err
	}
	return checkNoStagingAliases(src, dst, flatNames, bulkDir)
}

func validateStagingWriteTarget(dst storage.Backend, target stageWriteTarget) error {
	if runtime.GOOS == "windows" && localTargetUsesWindowsADSOnBackend(dst, target.path) {
		return fmt.Errorf(
			"promotion: stage: write target %s (%s) uses Windows alternate-data-stream syntax on a local destination; refusing ambiguous staging output",
			target.name, target.path,
		)
	}
	return nil
}

type stagePathRef struct {
	backend    storage.Backend
	backendKey string
	path       string
}

type stageWriteTarget struct {
	name string
	path string
}

type backendUnwrapper interface {
	InnerBackend() storage.Backend
}

type stageSourceAliasGroup struct {
	reference stagePathRef
	members   []engine.RFileExportFile
}

// dedupeStageSources collapses manifest entries that resolve to the same
// already-exported source object before any staging begins. This avoids
// false flatten collisions and duplicate staged output when one source is
// reachable through multiple symlink/hardlink or backend-equivalent path
// spellings. Alias groups are only auto-deduped when every member would
// flatten to the same basename; if the same physical source is advertised
// under multiple flattened filenames, StageBulkDir fails closed rather
// than arbitrarily picking one bulk-import name.
func dedupeStageSources(src storage.Backend, rfiles []engine.RFileExportFile) ([]engine.RFileExportFile, error) {
	cache := newPathIdentityCache(len(rfiles))
	groups := make([]stageSourceAliasGroup, 0, len(rfiles))
	for _, rf := range rfiles {
		ref := newStagePathRef(src, rf.DestinationPath)
		grouped := false
		for i := range groups {
			if sourceRefsAlias(ref, groups[i].reference, cache) {
				groups[i].members = append(groups[i].members, rf)
				grouped = true
				break
			}
		}
		if grouped {
			continue
		}
		groups = append(groups, stageSourceAliasGroup{
			reference: ref,
			members:   []engine.RFileExportFile{rf},
		})
	}

	deduped := make([]engine.RFileExportFile, 0, len(groups))
	for _, group := range groups {
		rf, err := canonicalStageSource(group.members)
		if err != nil {
			return nil, err
		}
		deduped = append(deduped, rf)
	}
	return deduped, nil
}

func canonicalStageSource(members []engine.RFileExportFile) (engine.RFileExportFile, error) {
	if len(members) == 0 {
		return engine.RFileExportFile{}, fmt.Errorf("promotion: empty source alias group")
	}
	sort.Slice(members, func(i, j int) bool {
		if members[i].DestinationPath == members[j].DestinationPath {
			return members[i].TabletIndex < members[j].TabletIndex
		}
		return members[i].DestinationPath < members[j].DestinationPath
	})

	reference := members[0]
	referenceBase := filepath.Base(reference.DestinationPath)
	for _, member := range members[1:] {
		if member.TabletIndex != reference.TabletIndex {
			return engine.RFileExportFile{}, fmt.Errorf(
				"promotion: source alias %q is declared under multiple tablet indexes (%d and %d)",
				reference.DestinationPath, reference.TabletIndex, member.TabletIndex,
			)
		}
		if filepath.Base(member.DestinationPath) != referenceBase {
			return engine.RFileExportFile{}, fmt.Errorf(
				"promotion: source alias %q is also declared as %q with a different flattened filename; refusing ambiguous dedupe",
				reference.DestinationPath, member.DestinationPath,
			)
		}
	}
	return reference, nil
}

// sourceRefsAlias reports whether left and right name the same
// already-exported source object, so dedupeStageSources may safely
// collapse them into one staged entry. This is deliberately more
// conservative than pathsAlias's write-target collision check: a
// false positive there only causes StageBulkDir to refuse a write
// (fail safe), but a false positive here would cause
// canonicalStageSource to silently drop a manifest entry, staging one
// fewer RFile than the export actually produced (fail unsafe). So,
// unlike pathsAlias, this never falls back to the trailing-slash-
// trimmed "looks like a URL" heuristic: on an unrecognized backend
// (canonicalPath not ok) with non-local semantics, two paths are only
// treated as the same source when they are exactly equal strings --
// for example, on a bare in-memory or other custom backend,
// "custom://bucket/A.rf" and "custom://bucket/A.rf/" are kept as
// distinct sources rather than merged on a guess.
func sourceRefsAlias(left, right stagePathRef, cache pathIdentityCache) bool {
	leftCanonical, leftCanonicalOK := cache.canonicalPath(left)
	rightCanonical, rightCanonicalOK := cache.canonicalPath(right)
	if leftCanonicalOK || rightCanonicalOK {
		return leftCanonicalOK && rightCanonicalOK && leftCanonical == rightCanonical
	}

	if usesLocalFilesystemSemantics(left) && usesLocalFilesystemSemantics(right) {
		leftInfo := cache.stat(left.path)
		rightInfo := cache.stat(right.path)
		if leftInfo == nil || rightInfo == nil {
			return false
		}
		return os.SameFile(leftInfo, rightInfo)
	}

	return left.path == right.path
}

// stagePathsAlias reports whether srcPath and dstPath would resolve to
// the same location under the default local/URL heuristics. It exists as
// a lightweight wrapper for tests; StageBulkDir itself uses
// stagePathsAliasOnBackends so backend-aware canonicalization can treat
// equivalent spellings such as s3://bucket/key versus bucket/key as the
// same destination.
func stagePathsAlias(srcPath, dstPath string) bool {
	return pathsAlias(
		newStagePathRef(nil, srcPath),
		newStagePathRef(nil, dstPath),
		newPathIdentityCache(0),
	)
}

func stagePathsAliasOnBackends(src storage.Backend, srcPath string, dst storage.Backend, dstPath string) bool {
	return pathsAlias(
		newStagePathRef(src, srcPath),
		newStagePathRef(dst, dstPath),
		newPathIdentityCache(0),
	)
}

// pathIdentityCache memoizes local-filesystem identity probes and remote
// canonical path identities by path so callers comparing many paths against
// each other (checkNoStagingAliases, dedupeStageSources) stat each unique
// local path once, resolve each local path's existing-parent symlink
// prefixes once, compute each publication key once, and parse each remote
// path into its canonical backend-aware identity once rather than
// re-walking or re-parsing the same inputs on every comparison. A cached
// nil FileInfo means the path could not be stat'd (most commonly because it
// doesn't exist yet); that's distinct from "not yet looked up," so failed
// stats are cached too rather than retried.
type pathIdentityCache struct {
	stats              map[string]os.FileInfo
	resolvedLocalPaths map[string]string
	publicationPaths   map[string]localPublicationIdentity
	canonicalPaths     map[canonicalPathCacheKey]canonicalPathIdentity
}

func newPathIdentityCache(capacity int) pathIdentityCache {
	return pathIdentityCache{
		stats:              make(map[string]os.FileInfo, capacity),
		resolvedLocalPaths: make(map[string]string, capacity),
		publicationPaths:   make(map[string]localPublicationIdentity, capacity),
		canonicalPaths:     make(map[canonicalPathCacheKey]canonicalPathIdentity, capacity),
	}
}

type canonicalPathCacheKey struct {
	backend string
	path    string
}

type canonicalPathIdentity struct {
	value string
	ok    bool
}

func (c pathIdentityCache) stat(path string) os.FileInfo {
	if info, cached := c.stats[path]; cached {
		return info
	}
	var info os.FileInfo
	for _, statPath := range localStatCandidates(path) {
		candidateInfo, err := os.Stat(statPath)
		if err == nil {
			info = candidateInfo
			break
		}
	}
	c.stats[path] = info
	return info
}

func localStatCandidates(path string) []string {
	if normalized, ok := normalizeWindowsDrivePath(path); ok && normalized != path {
		if runtime.GOOS == "windows" {
			return []string{normalized, path}
		}
		return []string{path, normalized}
	}
	return []string{path}
}

func (c pathIdentityCache) publicationKey(path string) string {
	return c.publicationPath(path).normalizedKey
}

func (c pathIdentityCache) resolvedLocalPath(path string) string {
	if resolved, cached := c.resolvedLocalPaths[path]; cached {
		return resolved
	}
	resolved := resolveExistingLocalPathPrefixes(path)
	c.resolvedLocalPaths[path] = resolved
	return resolved
}

func (c pathIdentityCache) publicationPath(path string) localPublicationIdentity {
	if identity, cached := c.publicationPaths[path]; cached {
		return identity
	}
	identity := buildLocalPublicationIdentity(c.resolvedLocalPath(path))
	c.publicationPaths[path] = identity
	return identity
}

func (c pathIdentityCache) canonicalPath(ref stagePathRef) (string, bool) {
	backendKey := ref.backendKey
	if backendKey == "" {
		backendKey = stagePathBackendKey(unwrapBackend(ref.backend))
	}
	key := canonicalPathCacheKey{
		backend: backendKey,
		path:    ref.path,
	}
	if identity, cached := c.canonicalPaths[key]; cached {
		return identity.value, identity.ok
	}
	value, ok := canonicalBackendPath(ref)
	c.canonicalPaths[key] = canonicalPathIdentity{value: value, ok: ok}
	return value, ok
}

func canonicalPathBackendKey(backend storage.Backend) string {
	backend = unwrapBackend(backend)
	if backend == nil {
		return "<nil>"
	}
	value := reflect.ValueOf(backend)
	if value.Kind() == reflect.Pointer && !value.IsNil() {
		return value.Type().String() + ":" + strconv.FormatUint(uint64(value.Pointer()), 16)
	}
	return fmt.Sprintf("%T:%#v", backend, backend)
}

func newStagePathRef(backend storage.Backend, path string) stagePathRef {
	backend = unwrapBackend(backend)
	return stagePathRef{
		backend:    backend,
		backendKey: stagePathBackendKey(backend),
		path:       path,
	}
}

// pathsAlias is StageBulkDir's alias detector, factored out so
// checkNoStagingAliases's O(N^2) comparison can share one
// pathIdentityCache across every call instead of stat'ing the same
// handful of local paths repeatedly.
//
// Remote/object-store paths are canonicalized through the backend-aware
// parsers already used by the built-in storage packages (s3.ParsePath,
// gcs.ParsePath, azure.ParsePath, and HDFS URI parsing), even when the
// backend is wrapped (for example by diskcache.Backend), so equivalent
// qualified vs scheme-less spellings compare equal. Local filesystem
// paths compare a collision-safe publication key first (resolving
// existing parent-prefix symlinks, dangling final symlink chains, and
// conservative case/Unicode/trailing-dot-space equivalence plus Windows
// DOS 8.3 literal-short-name ambiguity for not-yet-created targets),
// then fall back to os.Stat + os.SameFile so existing symlink/hardlink
// aliases are caught too.
func pathsAlias(srcPath, dstPath stagePathRef, cache pathIdentityCache) bool {
	srcCanonical, srcCanonicalOK := cache.canonicalPath(srcPath)
	dstCanonical, dstCanonicalOK := cache.canonicalPath(dstPath)
	if srcCanonicalOK || dstCanonicalOK {
		return srcCanonicalOK && dstCanonicalOK && srcCanonical == dstCanonical
	}

	if usesLocalFilesystemSemantics(srcPath) && usesLocalFilesystemSemantics(dstPath) {
		if localPublicationKeysAlias(srcPath.path, dstPath.path, cache) {
			return true
		}
		srcInfo := cache.stat(srcPath.path)
		if srcInfo == nil {
			return false
		}
		dstInfo := cache.stat(dstPath.path)
		if dstInfo == nil {
			return false
		}
		return os.SameFile(srcInfo, dstInfo)
	}

	if pathLooksURLLikeOnBackend(srcPath.backend, srcPath.path) || pathLooksURLLikeOnBackend(dstPath.backend, dstPath.path) {
		return strings.TrimRight(srcPath.path, `/\`) == strings.TrimRight(dstPath.path, `/\`)
	}

	return srcPath.path == dstPath.path
}

func localPathsLexicallyAlias(srcPath, dstPath string) bool {
	left := buildLocalPublicationIdentity(normalizeLocalPathForAlias(srcPath))
	right := buildLocalPublicationIdentity(normalizeLocalPathForAlias(dstPath))
	return left.normalizedKey == right.normalizedKey || dos83PathAliases(left, right)
}

func localPublicationKeysAlias(srcPath, dstPath string, cache pathIdentityCache) bool {
	if cache.publicationKey(srcPath) == cache.publicationKey(dstPath) {
		return true
	}
	return dos83PathAliases(cache.publicationPath(srcPath), cache.publicationPath(dstPath))
}

func resolveExistingLocalPathPrefixes(path string) string {
	const maxSymlinkHops = 40
	return resolveExistingLocalPathPrefixesWithState(path, &localPathResolutionState{
		hopsRemaining: maxSymlinkHops,
		seen:          make(map[string]struct{}),
	})
}

type localPathResolutionState struct {
	hopsRemaining int
	seen          map[string]struct{}
}

func resolveExistingLocalPathPrefixesWithState(path string, state *localPathResolutionState) string {
	path = normalizeLocalPathForResolution(path)
	prefix, parts := splitLocalPath(path)
	current := prefix
	for i := 0; i < len(parts); {
		candidate := joinLocalPath(current, parts[i])
		info, err := os.Lstat(candidate)
		if err != nil {
			return appendLocalPathParts(current, parts[i:])
		}
		if info.Mode()&os.ModeSymlink != 0 {
			current = resolveLocalSymlinkPathLexically(candidate, state)
			i++
			continue
		}
		if !info.IsDir() && i < len(parts)-1 {
			return appendLocalPathParts(current, parts[i:])
		}
		current = candidate
		i++
	}
	if current == "" {
		return path
	}
	return current
}

// resolveLocalSymlinkPathLexically resolves current -- which the caller
// has already confirmed via os.Lstat is a symlink -- to its final
// target, one hop at a time via os.Readlink. After each hop the new
// target is re-run through resolveExistingLocalPathPrefixesWithState,
// which recursively resolves any symlinked ancestor directory the
// target's own path passes through, not only the target's final
// component; this lets a relative symlink whose target traverses a
// symlinked parent directory (for example a target under a directory
// that is itself "bulk/alias -> .") compare correctly against a
// literal destination path, even when the ultimate file does not exist
// yet. A shared *localPathResolutionState carries a hop budget and a
// "seen" set across every hop and every recursive call, so a symlink
// cycle is detected exactly (returning the repeating path as-is)
// rather than merely bounded by a hop count that could otherwise still
// spin all the way through the cycle before giving up.
//
// This deliberately does not use filepath.EvalSymlinks: that resolves
// the entire input path through the platform's native path-resolution
// APIs, which on Windows can silently rewrite a path's *spelling* even
// for components that are not themselves symlinks -- for example
// expanding a short 8.3-style component such as MARCPA~1 to its long
// form -- which would make an unrelated, never-symlinked path compare
// unequal to another path purely because one of them happened to pass
// through EvalSymlinks. Instead this substitutes a resolved value only
// where a real symlink is actually found, leaving every other
// component's original spelling untouched.
func resolveLocalSymlinkPathLexically(path string, state *localPathResolutionState) string {
	current := normalizeLocalPathForResolution(path)
	for state.hopsRemaining > 0 {
		if _, ok := state.seen[current]; ok {
			return current
		}
		state.seen[current] = struct{}{}
		target, err := os.Readlink(current)
		if err != nil {
			return current
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(current), target)
		}
		state.hopsRemaining--
		target = resolveExistingLocalPathPrefixesWithState(target, state)
		info, err := os.Lstat(target)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			return target
		}
		current = normalizeLocalPathForResolution(target)
	}
	return current
}

func normalizeLocalPathForResolution(path string) string {
	path = normalizeLocalPathForAlias(path)
	if !looksLikeWindowsDrivePath(path) && !filepath.IsAbs(path) {
		if absPath, err := filepath.Abs(path); err == nil {
			path = absPath
		}
	}
	return path
}

func normalizeLocalPublicationPath(path string) string {
	prefix, parts := splitLocalPath(path)
	normalizedParts := make([]string, len(parts))
	for i, part := range parts {
		normalizedParts[i] = normalizeLocalPublicationComponent(part)
	}

	prefix = strings.ReplaceAll(prefix, `\`, "/")
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return prefix + strings.Join(normalizedParts, "/")
}

func splitLocalPath(path string) (string, []string) {
	path = normalizeLocalPathForAlias(path)
	volume := filepath.VolumeName(path)
	rest := strings.TrimPrefix(path, volume)
	prefix := volume
	if strings.HasPrefix(rest, `/`) || strings.HasPrefix(rest, `\`) {
		prefix += string(filepath.Separator)
		rest = strings.TrimLeft(rest, `/\`)
	}
	if rest == "" {
		return prefix, nil
	}
	return prefix, strings.FieldsFunc(rest, func(r rune) bool { return r == '/' || r == '\\' })
}

func joinLocalPath(prefix, part string) string {
	if prefix == "" {
		return part
	}
	return filepath.Join(prefix, part)
}

func appendLocalPathParts(prefix string, parts []string) string {
	current := prefix
	for _, part := range parts {
		current = joinLocalPath(current, part)
	}
	if current == "" {
		return "."
	}
	return current
}

func usesLocalFilesystemSemantics(ref stagePathRef) bool {
	backend := unwrapBackend(ref.backend)
	if storage.UsesLocalPathSemantics(backend) {
		return true
	}
	if explicitBackendSchemeOnBackend(backend, ref.path) != "" {
		return false
	}
	if backend == nil {
		return true
	}
	return false
}

// canonicalBackendPath returns a backend-aware canonical identity for
// ref, or ok=false when no such identity can be safely established.
// Canonicalization is only ever applied when the unwrapped backend is a
// concrete, recognized backend type (*s3.Backend, *gcs.Backend,
// *azure.Backend, *hdfs.Backend): only then do we actually know the
// path-normalization rules the backend enforces (for example HDFS's
// dot-segment resolution). There is deliberately no fallback that
// canonicalizes based on the path's scheme spelling alone: a path that
// merely *looks* like "hdfs:/..." on a backend that is not really HDFS
// (a memory.Backend in tests, or any other backend without HDFS
// semantics) has no guarantee its keys are dot-segment-normalized --
// memory.Backend, for instance, treats "hdfs:/dir/../A.rf" and
// "hdfs:/A.rf" as two entirely distinct, literal map keys. Assuming
// HDFS semantics from spelling alone would make sourceRefsAlias treat
// those two distinct stored objects as the same source and silently
// drop one in canonicalStageSource.
func canonicalBackendPath(ref stagePathRef) (string, bool) {
	backend := unwrapBackend(ref.backend)
	if storage.UsesLocalPathSemantics(backend) {
		return "", false
	}
	scheme := explicitBackendSchemeOnBackend(backend, ref.path)
	switch b := backend.(type) {
	case *s3.Backend:
		if scheme != "" && scheme != "s3" {
			return "", false
		}
		return canonicalS3Path(ref.path)
	case *gcs.Backend:
		if scheme != "" && scheme != "gs" {
			return "", false
		}
		return canonicalGCSPath(ref.path)
	case *azure.Backend:
		if scheme != "" && scheme != "az" {
			return "", false
		}
		return canonicalAzurePath(ref.path)
	case *hdfs.Backend:
		if scheme != "" && scheme != "hdfs" {
			return "", false
		}
		return canonicalHDFSPath(ref.path, b.Authority())
	}

	// No concrete recognized backend type matched (including a nil
	// backend): fall through without canonicalizing, regardless of
	// what scheme the path string happens to spell.
	return "", false
}

func canonicalS3Path(path string) (string, bool) {
	bucket, key, err := parseS3Path(path)
	if err != nil {
		return "", false
	}
	return "s3://" + bucket + "/" + key, true
}

func canonicalGCSPath(path string) (string, bool) {
	bucket, object, err := parseGCSPath(path)
	if err != nil {
		return "", false
	}
	return "gs://" + bucket + "/" + object, true
}

func canonicalAzurePath(path string) (string, bool) {
	container, blob, err := parseAzurePath(path)
	if err != nil {
		return "", false
	}
	return "az://" + container + "/" + blob, true
}

func canonicalHDFSPath(objectPath, authorityHint string) (string, bool) {
	if explicitBackendScheme(objectPath) == "" {
		if !strings.HasPrefix(objectPath, "/") {
			return "", false
		}
		return canonicalHDFSString(authorityHint, path.Clean(objectPath)), true
	}

	u, err := parseCanonicalHDFSPath(objectPath)
	if err != nil {
		return "", false
	}
	if !strings.EqualFold(u.Scheme, "hdfs") || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}

	authority := u.Host
	if authority == "" {
		authority = authorityHint
	}
	resolved := u.Path
	if resolved == "" {
		resolved = "/"
	}
	return canonicalHDFSString(authority, path.Clean(resolved)), true
}

func canonicalHDFSString(authority, resolved string) string {
	if resolved == "." || resolved == "" {
		resolved = "/"
	}
	if !strings.HasPrefix(resolved, "/") {
		resolved = "/" + resolved
	}
	authority = strings.ToLower(authority)
	if authority == "" {
		return "hdfs:" + resolved
	}
	return "hdfs://" + authority + resolved
}

func explicitBackendScheme(path string) string {
	return explicitBackendSchemeOnBackend(nil, path)
}

func explicitBackendSchemeOnBackend(backend storage.Backend, path string) string {
	return storage.ExplicitPathScheme(backend, path)
}

func unwrapBackend(backend storage.Backend) storage.Backend {
	for backend != nil {
		unwrapper, ok := backend.(backendUnwrapper)
		if !ok {
			break
		}
		inner := unwrapper.InnerBackend()
		if inner == nil || inner == backend {
			break
		}
		backend = inner
	}
	return backend
}

// flattenNames validates that every RFile's basename is unique once
// flattened into a single bulk directory and returns the
// DestinationPath -> flat basename mapping. Two manifest entries sharing
// the same DestinationPath (the same file listed twice) are not a
// collision; distinct source paths that happen to share a basename are.
//
// filepath.Base is also rejected when it yields a non-leaf name: "." or
// ".." (from a DestinationPath that is empty, ".", "..", or ends in a
// "." or ".." segment) collapse away rather than naming a real file, and
// joinBulkPath would then resolve the write target to bulkDir itself or
// to bulkDir's parent directory instead of a new file inside it. A bare
// separator (from a DestinationPath consisting only of separators) is
// rejected for the same reason. Left unchecked, staging such a manifest
// entry can truncate or create a file outside the requested bulk
// directory on a local destination before WriteLoadMapping ever runs.
func flattenNames(rfiles []engine.RFileExportFile) (map[string]string, error) {
	names := make(map[string]string, len(rfiles))
	byName := make(map[string]string, len(rfiles))
	for _, rf := range rfiles {
		name := filepath.Base(rf.DestinationPath)
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
			return nil, fmt.Errorf(
				"promotion: rfile %q flattens to invalid bulk-dir basename %q; refusing to stage",
				rf.DestinationPath, name,
			)
		}
		if prior, ok := byName[name]; ok && prior != rf.DestinationPath {
			return nil, fmt.Errorf(
				"promotion: bulk dir flatten collision: %q and %q both flatten to %q",
				prior, rf.DestinationPath, name,
			)
		}
		byName[name] = rf.DestinationPath
		names[rf.DestinationPath] = name
	}
	return names, nil
}
