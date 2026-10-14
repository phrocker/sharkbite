// Package local is a storage.Backend over the local filesystem. Useful
// for dev cycles where shoal runs against RFiles dumped from a cluster,
// and for tests that want to exercise the bcfile/rfile stack without
// any cloud deps.
package local

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/phrocker/shoal-oss/internal/storage"
)

// Backend opens files from the OS filesystem. Stateless — share a
// single value across goroutines.
type Backend struct{}

// New returns a Backend ready to Open files by path.
func New() *Backend { return &Backend{} }

// UsesLocalPathSemantics reports that local backend paths are OS filesystem
// paths even when their spelling happens to resemble a backend URI.
func (b *Backend) UsesLocalPathSemantics() bool { return true }

// Open opens path read-only and returns a storage.File backed by the
// underlying os.File. Returns an error wrapping storage.ErrNotFound
// when the path doesn't exist.
func (b *Backend) Open(_ context.Context, path string) (storage.File, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", storage.ErrNotFound, path)
		}
		return nil, fmt.Errorf("local: open %s: %w", path, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("local: stat %s: %w", path, err)
	}
	return &file{f: f, size: info.Size()}, nil
}

// Create opens a temporary sibling file for writing and commits it into path
// on Close without first removing or moving an existing target. Existing
// ordinary permission bits and sticky are preserved everywhere, but setuid and
// setgid are intentionally not restored so rewritten bytes mirror the privilege
// stripping of truncating an existing file in place. On Unix we also preserve
// owner/group, and on Linux and Darwin we preserve extended attributes
// (including xattr-backed ACLs such as Linux POSIX ACLs) when the platform
// exposes them. Create rejects final-component symlinks because a portable
// path-based replacement cannot atomically verify the link and publish to its
// referent without risking a concurrent retarget. On platforms without hard-link
// snapshots (Plan 9, js, wasip1),
// and on Unix filesystems that reject hard-link snapshots for same-directory
// siblings, replacement falls back to a best-effort rename-based sequence that
// restores the old file on failure but cannot keep the target continuously
// visible. A crash or ambiguous publish failure in that fallback can leave the
// hidden backup as the only surviving copy until an operator or
// CleanupStaleArtifacts reports it for explicit recovery. New files use 0644
// subject to the process umask. Parent directories are created with 0755 if
// they don't already exist — matches "mkdir -p" behavior so callers don't
// have to pre-create the path tree.
func (b *Backend) Create(_ context.Context, path string) (storage.Writer, error) {
	if isReplacementArtifactName(filepath.Base(path)) {
		return nil, fmt.Errorf("local: destination %s uses a reserved internal namespace", path)
	}
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("local: mkdir %s: %w", dir, err)
		}
	}
	info, err := os.Lstat(path)
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("local: symlink destination %s is not supported for atomic replacement", path)
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("local: inspect existing file %s: %w", path, err)
	}
	target := path
	ops := osReplacementOps{}
	f, err := openReplacementSiblingFile(filepath.Dir(target))
	if err != nil {
		return nil, fmt.Errorf("local: create temporary file for %s: %w", target, err)
	}
	if _, err := preserveExistingMetadata(ops, f.Name(), target); err != nil {
		return nil, errors.Join(err, cleanupTemporaryCreateFile(f, ops))
	}
	return &writer{
		file:   f,
		temp:   f.Name(),
		target: target,
		ops:    ops,
	}, nil
}

// List enumerates the regular files directly under prefix (a directory
// path), returning their full paths. Mirrors the os.ReadDir-based RFile
// discovery the tablet did before storage was abstracted. A non-existent
// prefix yields an empty list, not an error (an empty tablet dir). Internal
// replacement-artifact names are reserved and omitted; pre-existing matching
// files remain accessible through Open and Remove.
func (b *Backend) List(_ context.Context, prefix string) ([]string, error) {
	entries, err := os.ReadDir(prefix)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("local: readdir %s: %w", prefix, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || isReplacementArtifactName(e.Name()) {
			continue
		}
		out = append(out, filepath.Join(prefix, e.Name()))
	}
	return out, nil
}

// CleanupStaleArtifacts removes reserved temporary files directly under prefix
// whose modification time is strictly before cutoff. Reserved backup files are
// reported as recoverable and left in place because their randomized names do
// not identify one safe restore/delete target.
func (b *Backend) CleanupStaleArtifacts(ctx context.Context, prefix string, cutoff time.Time) (storage.ArtifactCleanupResult, error) {
	return cleanupStaleArtifacts(ctx, prefix, cutoff, os.ReadDir, os.Remove)
}

func cleanupStaleArtifacts(
	ctx context.Context,
	prefix string,
	cutoff time.Time,
	readDir func(string) ([]os.DirEntry, error),
	remove func(string) error,
) (storage.ArtifactCleanupResult, error) {
	var result storage.ArtifactCleanupResult
	if err := contextOrBackground(ctx).Err(); err != nil {
		return result, err
	}
	if err := storage.ValidateArtifactCleanupCutoff(time.Now(), cutoff); err != nil {
		return result, fmt.Errorf("local: %w", err)
	}
	entries, err := readDir(prefix)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return result, nil
		}
		return result, fmt.Errorf("local: readdir stale artifacts %s: %w", prefix, err)
	}
	slices.SortFunc(entries, func(a, b os.DirEntry) int {
		return strings.Compare(a.Name(), b.Name())
	})
	type candidate struct {
		entry os.DirEntry
		info  os.FileInfo
	}
	var candidates []candidate
	var cleanupErr error
	for _, entry := range entries {
		if err := contextOrBackground(ctx).Err(); err != nil {
			return result, errors.Join(cleanupErr, err)
		}
		if entry.IsDir() || !isReplacementArtifactName(entry.Name()) {
			continue
		}
		result.Examined++
		info, err := entry.Info()
		if err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("local: stat stale artifact %s: %w", filepath.Join(prefix, entry.Name()), err))
			continue
		}
		candidates = append(candidates, candidate{entry: entry, info: info})
	}
	for _, candidate := range candidates {
		if err := contextOrBackground(ctx).Err(); err != nil {
			return result, errors.Join(cleanupErr, err)
		}
		if !candidate.info.ModTime().Before(cutoff) {
			continue
		}
		if isGeneratedReplacementName(candidate.entry.Name(), replacementBackupPrefix) {
			result.Recoverable = append(result.Recoverable, filepath.Join(prefix, candidate.entry.Name()))
			continue
		}
		artifact := filepath.Join(prefix, candidate.entry.Name())
		if err := remove(artifact); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("local: remove stale artifact %s: %w", artifact, err))
			continue
		}
		result.Removed = append(result.Removed, artifact)
	}
	return result, cleanupErr
}

func contextOrBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

type namedCloser interface {
	Name() string
	Close() error
}

// Remove deletes path. A missing path is not an error.
func (b *Backend) Remove(_ context.Context, path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("local: remove %s: %w", path, err)
	}
	return nil
}

// file is the local-filesystem File implementation. *os.File already
// satisfies io.ReaderAt + io.Closer; we just attach a cached Size.
type file struct {
	f    *os.File
	size int64
}

func (l *file) ReadAt(p []byte, off int64) (int, error) { return l.f.ReadAt(p, off) }
func (l *file) Close() error                            { return l.f.Close() }
func (l *file) Size() int64                             { return l.size }

type writer struct {
	file           *os.File
	temp           string
	target         string
	ops            replacementOps
	closed         bool
	abortRequested bool
	aborted        bool
	fileClosed     bool
	tempDiscarded  bool
	state          replacementState
}

type replacementState uint8

const (
	replacementStaged replacementState = iota
	replacementPublished
	replacementCommitted
)

type replacementOps interface {
	Lstat(string) (os.FileInfo, error)
	Chmod(string, os.FileMode) error
	Remove(string) error
	AtomicReplace(temp, target, backup string, hadOld bool) error
	AtomicRestore(target, backup string) error
	AtomicRestoreIfAbsent(target, backup string) error
}

type durableReplacementOps interface {
	SyncPath(string) error
	SyncParent(string) error
}

type osReplacementOps struct{}

var preservePlatformMetadataFn = preservePlatformMetadata
var sameReplacementFile = os.SameFile
var readReplacementFile = os.ReadFile

func (osReplacementOps) Lstat(name string) (os.FileInfo, error) { return os.Lstat(name) }
func (osReplacementOps) Chmod(name string, mode os.FileMode) error {
	return os.Chmod(name, mode)
}
func (osReplacementOps) Remove(name string) error { return os.Remove(name) }
func (osReplacementOps) AtomicReplace(temp, target, backup string, hadOld bool) error {
	return platformAtomicReplace(temp, target, backup, hadOld)
}
func (osReplacementOps) AtomicRestore(target, backup string) error {
	return platformAtomicRestore(target, backup)
}

func (osReplacementOps) AtomicRestoreIfAbsent(target, backup string) error {
	if err := os.Link(backup, target); err != nil {
		return err
	}
	return os.Remove(backup)
}

const (
	replacementModeMask       = os.ModePerm | os.ModeSticky
	replacementTempPrefix     = ".shoal-tmp-"
	replacementBackupPrefix   = ".shoal-backup-"
	replacementNameTokenBytes = 16
	replacementNameAttempts   = 32
)

var randomReplacementNameToken = func() (string, error) {
	buf := make([]byte, replacementNameTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("local: generate replacement name token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func replacementTargetMode(ops replacementOps, target string) (os.FileMode, bool, error) {
	info, err := ops.Lstat(target)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("local: inspect existing file %s: %w", target, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return 0, false, fmt.Errorf("local: replace %s: symlink destinations are not supported; resolve the symlink target first", target)
	}
	if !info.Mode().IsRegular() {
		return 0, false, fmt.Errorf("local: replace %s: target is not a regular file", target)
	}
	return info.Mode() & replacementModeMask, true, nil
}

func preserveExistingMetadata(ops replacementOps, temp, target string) (bool, error) {
	mode, hadOld, err := replacementTargetMode(ops, target)
	if err != nil || !hadOld {
		return hadOld, err
	}
	if err := preservePlatformMetadataFn(temp, target); err != nil {
		return true, err
	}
	if err := ops.Chmod(temp, mode); err != nil {
		return true, fmt.Errorf("local: preserve mode bits for %s: %w", target, err)
	}
	return true, nil
}

func openReplacementSiblingFile(dir string) (*os.File, error) {
	var lastErr error
	for attempts := 0; attempts < replacementNameAttempts; attempts++ {
		temp, err := nextReplacementSiblingPath(dir, replacementTempPrefix)
		if err != nil {
			return nil, err
		}
		f, err := os.OpenFile(temp, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			return f, nil
		}
		if errors.Is(err, fs.ErrExist) {
			lastErr = err
			continue
		}
		return nil, err
	}
	if lastErr == nil {
		lastErr = fs.ErrExist
	}
	return nil, fmt.Errorf("local: exhausted unique temporary sibling names: %w", lastErr)
}

func cleanupTemporaryCreateFile(file namedCloser, ops replacementOps) error {
	if file == nil {
		return nil
	}
	var cleanupErr error
	if err := file.Close(); err != nil {
		cleanupErr = errors.Join(cleanupErr, fmt.Errorf("local: close temporary file %s: %w", file.Name(), err))
	}
	if err := ops.Remove(file.Name()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		cleanupErr = errors.Join(cleanupErr, fmt.Errorf("local: remove temporary file %s: %w", file.Name(), err))
	}
	return cleanupErr
}

func nextReplacementSiblingPath(dir, prefix string) (string, error) {
	token, err := randomReplacementNameToken()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, prefix+token), nil
}

func isReplacementArtifactName(name string) bool {
	return isGeneratedReplacementName(name, replacementTempPrefix) ||
		isGeneratedReplacementName(name, replacementBackupPrefix)
}

func isGeneratedReplacementName(name, prefix string) bool {
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	return isLowerHexReplacementToken(name[len(prefix):])
}

func isLowerHexReplacementToken(token string) bool {
	if len(token) != replacementNameTokenBytes*2 || token != strings.ToLower(token) {
		return false
	}
	decoded, err := hex.DecodeString(token)
	return err == nil && len(decoded) == replacementNameTokenBytes
}

func (w *writer) publishReplacement(hadOld bool) (string, error) {
	if !hadOld {
		return "", w.ops.AtomicReplace(w.temp, w.target, "", false)
	}

	var lastErr error
	for attempts := 0; attempts < replacementNameAttempts; attempts++ {
		backup, err := nextReplacementSiblingPath(filepath.Dir(w.target), replacementBackupPrefix)
		if err != nil {
			return "", fmt.Errorf("local: create replacement backup path for %s: %w", w.target, err)
		}
		if err := w.ops.AtomicReplace(w.temp, w.target, backup, true); err != nil {
			if errors.Is(err, fs.ErrExist) {
				lastErr = err
				continue
			}
			return backup, err
		}
		return backup, nil
	}
	if lastErr == nil {
		lastErr = fs.ErrExist
	}
	return "", fmt.Errorf("local: exhausted unique replacement backup names for %s: %w", w.target, lastErr)
}

func (w *writer) Write(p []byte) (int, error) {
	if w.closed || w.abortRequested {
		return 0, fmt.Errorf("local: write after close")
	}
	return w.file.Write(p)
}

func (w *writer) Sync() error {
	if w.abortRequested {
		return fmt.Errorf("local: writer already aborted")
	}
	if w.closed {
		return fmt.Errorf("local: writer already closed")
	}
	if err := w.file.Sync(); err != nil {
		return fmt.Errorf("local: sync temporary file %s: %w", w.temp, err)
	}
	return nil
}

func (w *writer) Close() error {
	if w.abortRequested {
		return fmt.Errorf("local: writer already aborted")
	}
	if w.closed {
		return nil
	}
	if !w.fileClosed {
		if err := w.file.Close(); err != nil {
			w.fileClosed = true
			w.tempDiscarded = true
			_ = w.ops.Remove(w.temp)
			return fmt.Errorf("local: close temporary file %s: %w", w.temp, err)
		}
		w.fileClosed = true
	}
	if w.tempDiscarded {
		return fmt.Errorf("local: temporary file %s was discarded after a failed close", w.temp)
	}
	if err := w.commitReplacement(); err != nil {
		if storage.IsCommittedWriteError(err) {
			w.closed = true
		}
		return err
	}
	w.closed = true
	return nil
}

func (w *writer) commitReplacement() error {
	hadOld, err := preserveExistingMetadata(w.ops, w.temp, w.target)
	if err != nil {
		return err
	}
	if err := syncReplacementPath(w.ops, w.temp); err != nil {
		return fmt.Errorf("local: sync replacement file %s: %w", w.temp, err)
	}
	backup, err := w.publishReplacement(hadOld)
	if err != nil {
		publishErr := fmt.Errorf("local: publish %s: %w", w.target, err)
		if hadOld && backup != "" {
			if cleanupErr := w.discardUnusedBackup(backup); cleanupErr != nil {
				publishErr = errors.Join(publishErr, cleanupErr)
			}
		}
		return publishErr
	}
	w.state = replacementPublished
	if err := syncReplacementParent(w.ops, w.target); err != nil {
		w.state = replacementCommitted
		return storage.MarkCommittedWrite(
			fmt.Errorf("local: replacement for %s committed but parent directory sync failed: %w", w.target, err),
		)
	}
	if hadOld {
		var committedErr error
		if err := w.ops.Remove(backup); err != nil && !errors.Is(err, fs.ErrNotExist) {
			committedErr = errors.Join(
				committedErr,
				fmt.Errorf("local: remove replacement backup %s: %w", backup, err),
			)
		}
		if err := syncReplacementParent(w.ops, backup); err != nil {
			committedErr = errors.Join(
				committedErr,
				fmt.Errorf("local: sync parent directory after replacement cleanup for %s: %w", w.target, err),
			)
		}
		if committedErr != nil {
			w.state = replacementCommitted
			return storage.MarkCommittedWrite(
				fmt.Errorf("local: replacement for %s committed but cleanup was incomplete: %w", w.target, committedErr),
			)
		}
	}
	w.state = replacementCommitted
	return nil
}

// discardUnusedBackup restores a missing destination after a failed publish.
// If the destination still exists, it may be the original, the staged
// replacement, or a concurrent writer's file. Only discard the backup once the
// destination is proven to be the preserved original; otherwise retain the
// backup for explicit recovery rather than risking deletion of the old bytes.
func (w *writer) discardUnusedBackup(backup string) error {
	_, err := w.ops.Lstat(w.target)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("local: inspect destination %s after publish failure: %w", w.target, err)
		}
	}
	if errors.Is(err, fs.ErrNotExist) {
		backupPresent, err := replacementPathExists(w.ops, backup)
		if err != nil {
			return fmt.Errorf("local: inspect replacement backup %s after publish failure: %w", backup, err)
		}
		if !backupPresent {
			return nil
		}
		if err := w.ops.AtomicRestoreIfAbsent(w.target, backup); err != nil {
			return fmt.Errorf(
				"local: restore %s from %s after publish failure; backup retained for recovery: %w",
				w.target, backup, err,
			)
		}
		return nil
	}
	backupPresent, err := replacementPathExists(w.ops, backup)
	if err != nil {
		return fmt.Errorf("local: inspect replacement backup %s after publish failure: %w", backup, err)
	}
	if !backupPresent {
		return nil
	}
	reusedOriginal, err := replacementRestoredFromBackup(w.ops, w.target, backup)
	if err != nil {
		return fmt.Errorf(
			"local: verify destination %s against replacement backup %s after publish failure; backup retained for recovery: %w",
			w.target, backup, err,
		)
	}
	if !reusedOriginal {
		return fmt.Errorf(
			"local: destination %s after publish failure does not match replacement backup %s; backup retained for recovery",
			w.target, backup,
		)
	}
	if err := w.ops.Remove(backup); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("local: remove unused replacement backup %s: %w", backup, err)
	}
	return nil
}

func replacementRestoredFromBackup(ops replacementOps, target, backup string) (bool, error) {
	targetInfo, err := ops.Lstat(target)
	if err != nil {
		return false, err
	}
	backupInfo, err := ops.Lstat(backup)
	if err != nil {
		return false, err
	}
	if sameReplacementFile(targetInfo, backupInfo) {
		return true, nil
	}
	if targetInfo.Sys() != nil && backupInfo.Sys() != nil {
		return false, nil
	}
	return replacementMatchesBackupContents(target, backup, targetInfo, backupInfo)
}

func replacementMatchesBackupContents(target, backup string, targetInfo, backupInfo os.FileInfo) (bool, error) {
	if !targetInfo.Mode().IsRegular() || !backupInfo.Mode().IsRegular() {
		return false, nil
	}
	if targetInfo.Mode()&replacementModeMask != backupInfo.Mode()&replacementModeMask {
		return false, nil
	}
	if targetInfo.Size() != backupInfo.Size() {
		return false, nil
	}
	targetData, err := readReplacementFile(target)
	if err != nil {
		return false, err
	}
	backupData, err := readReplacementFile(backup)
	if err != nil {
		return false, err
	}
	return bytes.Equal(targetData, backupData), nil
}

func replacementPathExists(ops replacementOps, path string) (bool, error) {
	if _, err := ops.Lstat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (w *writer) Abort() error {
	if w.aborted {
		return nil
	}
	if w.closed {
		return fmt.Errorf("local: writer already closed")
	}
	if w.state != replacementStaged {
		return fmt.Errorf("local: replacement for %s cannot be safely aborted in state %d", w.target, w.state)
	}
	w.abortRequested = true

	var abortErr error
	if !w.fileClosed {
		err := w.file.Close()
		w.fileClosed = true
		if err != nil {
			abortErr = fmt.Errorf("local: close temporary file %s: %w", w.temp, err)
		}
	}
	if err := w.ops.Remove(w.temp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		abortErr = errors.Join(abortErr, fmt.Errorf("local: remove temporary file %s: %w", w.temp, err))
	}
	if abortErr != nil {
		return abortErr
	}
	w.aborted = true
	return nil
}

func syncReplacementPath(ops replacementOps, path string) error {
	durable, ok := ops.(durableReplacementOps)
	if !ok {
		return nil
	}
	return durable.SyncPath(path)
}

func syncReplacementParent(ops replacementOps, path string) error {
	durable, ok := ops.(durableReplacementOps)
	if !ok {
		return nil
	}
	return durable.SyncParent(path)
}
