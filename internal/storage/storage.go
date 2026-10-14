// Package storage abstracts the underlying file store that shoal reads
// RFiles from. Each backend (local FS, in-memory test fixture, GCS) lives
// in its own sub-package — callers who only need local don't pull in the
// GCS client deps.
//
// Why no Alluxio? The cluster historically routed RFile reads through
// Alluxio as a cache layer over GCS, but that adds an operational
// dependency (Alluxio master/workers) that shoal's read-fleet design
// is supposed to remove. Direct-to-GCS is the V0 target. If we want
// caching later we'll do it in shoal's own block cache, where we can
// pair it with the prefetcher.
//
// Backend usage:
//
//	be := gcs.New(ctx, ...)         // or local.New(), memory.New()
//	f, err := be.Open(ctx, path)
//	defer f.Close()
//	bc, err := bcfile.NewReader(f, f.Size())
//	r, err := rfile.Open(bc, block.Default())
//
// File satisfies io.ReaderAt + io.Closer + Size, which is exactly what
// bcfile.NewReader needs.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

const transferChunkSize = 64 * 1024

// File is an open backend object. Random-access reads via ReadAt; total
// length via Size; resource release via Close.
//
// ReadAt semantics follow io.ReaderAt: reads exactly len(p) bytes or
// returns a non-nil error. Short reads at end-of-file return io.EOF
// alongside the partial fill — the same contract as os.File.ReadAt.
type File interface {
	io.ReaderAt
	io.Closer

	// Size returns the total byte length of the underlying object.
	// Zero is a valid size (empty file).
	Size() int64
}

// Backend opens a File by path. Path semantics are backend-specific:
//
//   - local:  filesystem path (relative or absolute)
//   - memory: an arbitrary string key registered via Put
//   - gcs:    "gs://bucket/object/path" or just "bucket/object/path"
//
// Implementations are safe for concurrent Open calls. Returned Files
// are independently safe for concurrent ReadAt — no shared mutable state.
type Backend interface {
	Open(ctx context.Context, path string) (File, error)
}

// Writer is a write-only handle to a backend object. Streaming writes
// via io.Writer; close to flush. Not all backends support writing; use
// the WritableBackend type-assertion / interface to discover.
type Writer interface {
	io.Writer
	io.Closer
}

// Aborter is an optional Writer capability that abandons an in-progress write
// without publishing it. Copy, WriteAll, CleanupUnsuccessfulWrite, and direct
// Writer users that call AbortOnError invoke Abort on unsuccessful paths when
// available; writers that do not implement Aborter retain their Close
// semantics during cleanup.
type Aborter interface {
	Abort() error
}

// WriteCleanupState tracks whether a caller already attempted Close on a
// Writer before deferred cleanup runs. Call MarkCloseAttempted immediately
// before any explicit Close call when also deferring AbortOnError.
type WriteCleanupState struct {
	closeAttempted bool
}

// MarkCloseAttempted records that the caller is about to invoke Close on the
// writer. Cleanup helpers will not retry Close on legacy non-Aborter writers
// once this has been set.
func (s *WriteCleanupState) MarkCloseAttempted() {
	if s == nil {
		return
	}
	s.closeAttempted = true
}

// WritableBackend is a Backend that also supports creating + replacing
// objects. Returned Writers commit on Close; if they also implement Aborter,
// callers can abandon an unsuccessful write without publishing partial data.
type WritableBackend interface {
	Backend
	// Create opens path for writing. Replaces any existing object with
	// the same path. Returned Writer must be Closed to commit; until
	// then Open(path) may not see the new bytes.
	Create(ctx context.Context, path string) (Writer, error)
}

// Lister is a Backend that can enumerate object paths under a prefix.
// Used for manifest discovery (which RFiles a tablet owns) on startup.
// Local + Memory implement it. Cloud backends satisfy it via a list API.
//
// List returns the full paths (keys) of objects directly under prefix.
// Ordering is unspecified; callers sort if they need determinism.
type Lister interface {
	Backend
	List(ctx context.Context, prefix string) ([]string, error)
}

// ArtifactCleanupResult reports completed internal-artifact cleanup. Removed
// contains backend-qualified paths in deterministic discovery order.
// Recoverable contains reserved backup artifacts that were intentionally left
// in place because automatic deletion or restoration would be unsafe. If
// cleanup returns an error, Removed still reports every deletion that
// completed before or alongside the partial failure.
type ArtifactCleanupResult struct {
	Examined    int
	Removed     []string
	Recoverable []string
}

// ArtifactCleaner is an optional backend lifecycle capability for explicitly
// removing crash-leftover staging and replacement artifacts. Cleanup only
// considers names in the backend's reserved internal namespace whose backend
// modification time is strictly before cutoff. Callers must choose a cutoff
// at least MinArtifactCleanupAge older than the current time, and should
// normally use RecommendedArtifactCleanupAge or larger. Implementations
// preserve recent artifacts, never return artifacts as user data, honor ctx,
// report partial deletion failures, and surface any backup artifacts that need
// manual recovery through ArtifactCleanupResult.Recoverable.
type ArtifactCleaner interface {
	Backend
	CleanupStaleArtifacts(ctx context.Context, prefix string, cutoff time.Time) (ArtifactCleanupResult, error)
}

const (
	// MinArtifactCleanupAge is the smallest safety window accepted by artifact
	// cleanup helpers. Smaller cutoffs risk racing still-active writers.
	MinArtifactCleanupAge = time.Minute
	// RecommendedArtifactCleanupAge is the documented default floor callers
	// should use when scheduling periodic artifact cleanup.
	RecommendedArtifactCleanupAge = 15 * time.Minute
)

// ErrArtifactCleanerUnsupported reports that a backend does not expose stale
// artifact cleanup.
var ErrArtifactCleanerUnsupported = errors.New("storage: backend does not support stale artifact cleanup")

// ErrArtifactCleanupCutoffTooRecent reports that a cleanup cutoff is not old
// enough to safely avoid active writers.
var ErrArtifactCleanupCutoffTooRecent = errors.New("storage: stale artifact cutoff is too recent")

// ValidateArtifactCleanupCutoff rejects zero or overly recent cutoffs.
func ValidateArtifactCleanupCutoff(now, cutoff time.Time) error {
	if cutoff.IsZero() {
		return fmt.Errorf("%w: cutoff must be non-zero", ErrArtifactCleanupCutoffTooRecent)
	}
	if now.Sub(cutoff) < MinArtifactCleanupAge {
		return fmt.Errorf(
			"%w: cutoff must be at least %v before now",
			ErrArtifactCleanupCutoffTooRecent,
			MinArtifactCleanupAge,
		)
	}
	return nil
}

// CleanupStaleArtifacts invokes the optional ArtifactCleaner capability through
// a shared helper so callers can use one public janitor entry point.
func CleanupStaleArtifacts(
	ctx context.Context,
	b Backend,
	prefix string,
	cutoff time.Time,
) (ArtifactCleanupResult, error) {
	if err := ctx.Err(); err != nil {
		return ArtifactCleanupResult{}, err
	}
	cleaner, ok := b.(ArtifactCleaner)
	if !ok {
		return ArtifactCleanupResult{}, ErrArtifactCleanerUnsupported
	}
	return cleaner.CleanupStaleArtifacts(ctx, prefix, cutoff)
}

// Remover is a Backend that can delete an object by path. Used to drop a
// compaction's input RFiles once the merged output is durable. Deleting a
// non-existent path is not an error.
type Remover interface {
	Backend
	Remove(ctx context.Context, path string) error
}

var ErrRemoverUnsupported = errors.New("storage: remover unsupported")

// ErrNotFound is the sentinel for "the requested path doesn't exist."
// Backends should wrap their own not-found errors with errors.Join or
// fmt.Errorf("%w: ...", ErrNotFound, ...) so callers can errors.Is-test
// without backend-specific imports.
var ErrNotFound = errors.New("storage: not found")

// ErrReadOnly is returned when Create is called on a Backend that does
// not implement WritableBackend.
var ErrReadOnly = errors.New("storage: backend is read-only")

type committedWriteError struct {
	err error
}

func (e committedWriteError) Error() string { return e.err.Error() }
func (e committedWriteError) Unwrap() error { return e.err }

// MarkCommittedWrite marks err as describing a write that already committed.
// Callers can still return the error, but cleanup helpers will avoid aborting a
// write that can no longer be rolled back.
func MarkCommittedWrite(err error) error {
	if err == nil || IsCommittedWriteError(err) {
		return err
	}
	return committedWriteError{err: err}
}

// IsCommittedWriteError reports whether err describes a write that already
// committed and therefore must not be aborted during deferred cleanup.
func IsCommittedWriteError(err error) bool {
	var committed committedWriteError
	return errors.As(err, &committed)
}

// Copy copies an object from src to dst across (potentially different)
// backends. Useful for "pull a small RFile from GCS to local disk for
// debugging" workflows. dst must be a WritableBackend.
//
// Reads are issued in 64KB chunks via ReadAt; backends that bill per
// request (GCS) absorb at most ceil(size/64KB) round trips. Increase
// the chunk size for large files if perf matters. ctx is polled before
// and immediately after each backend read and write; if a backend call is
// already blocked, Copy waits for it to return before observing ctx.Err().
// On an unsuccessful path, Copy aborts the destination writer when it
// supports Aborter.
func Copy(ctx context.Context, src Backend, srcPath string, dst Backend, dstPath string) (written int64, err error) {
	wb, ok := dst.(WritableBackend)
	if !ok {
		return 0, ErrReadOnly
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	in, err := src.Open(ctx, srcPath)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return 0, fmt.Errorf("copy: open src %s: %w", srcPath, joinContextCallError(err, ctxErr))
		}
		return 0, fmt.Errorf("copy: open src %s: %w", srcPath, err)
	}
	defer in.Close()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	out, err := wb.Create(ctx, dstPath)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return 0, fmt.Errorf("copy: create dst %s: %w", dstPath, joinContextCallError(err, ctxErr))
		}
		return 0, fmt.Errorf("copy: create dst %s: %w", dstPath, err)
	}
	var cleanupState WriteCleanupState
	defer AbortOnError(&err, out, &cleanupState)

	buf := make([]byte, transferChunkSize)
	for written < in.Size() {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		readOff := written
		want := int64(transferChunkSize)
		if written+want > in.Size() {
			want = in.Size() - written
		}
		n, err := in.ReadAt(buf[:want], readOff)
		prematureEOF := errors.Is(err, io.EOF) && readOff+int64(n) < in.Size()
		if ctxErr := ctx.Err(); ctxErr != nil {
			if prematureEOF {
				return written, fmt.Errorf("copy: read off=%d: %w", readOff, joinContextCallError(io.ErrUnexpectedEOF, ctxErr))
			}
			return written, fmt.Errorf("copy: read off=%d: %w", readOff, joinContextCallError(err, ctxErr))
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return written, fmt.Errorf("copy: read off=%d: %w", readOff, err)
		}
		if n > 0 {
			writeOff := written
			wrote, werr := out.Write(buf[:n])
			written += int64(wrote)
			if ctxErr := ctx.Err(); ctxErr != nil {
				if werr != nil {
					return written, fmt.Errorf("copy: write off=%d: %w", writeOff, joinContextCallError(werr, ctxErr))
				}
				if wrote != n {
					return written, fmt.Errorf("copy: write off=%d: %w", writeOff, joinContextCallError(io.ErrShortWrite, ctxErr))
				}
				return written, fmt.Errorf("copy: write off=%d: %w", writeOff, ctxErr)
			}
			if werr != nil {
				return written, fmt.Errorf("copy: write off=%d: %w", writeOff, werr)
			}
			if wrote != n {
				return written, fmt.Errorf("copy: write off=%d: %w", writeOff, io.ErrShortWrite)
			}
		}
		if prematureEOF {
			return written, fmt.Errorf("copy: read off=%d: %w", readOff, io.ErrUnexpectedEOF)
		}
		if errors.Is(err, io.EOF) {
			break
		}
	}
	if written != in.Size() {
		return written, fmt.Errorf("copy: read src %s: %w", srcPath, io.ErrUnexpectedEOF)
	}
	if err := ctx.Err(); err != nil {
		return written, err
	}
	// Close (not defer) so a flush/commit failure on the destination is
	// reported instead of silently discarded — mirrors WriteAll. Several
	// WritableBackend implementations (e.g. object-storage backends that
	// buffer and upload on Close) can fail here even though every prior
	// Write succeeded.
	cleanupState.MarkCloseAttempted()
	if err := out.Close(); err != nil {
		return written, fmt.Errorf("copy: close dst %s: %w", dstPath, err)
	}
	return written, nil
}

// ReadAll opens path on b and reads the whole object into a single byte
// slice via ReadAt. This is the "pull-through" read used when an RFile is
// faulted into the local byte cache: fully resident after bounded range reads.
// For large objects where only a few blocks are needed, prefer wiring the
// File's ReadAt directly into the reader instead of ReadAll. Reads are bounded
// to 64KB, and ctx is polled before and immediately after each backend read.
// Once a read is already blocked, ReadAll waits for it to return before
// observing ctx.Err().
func ReadAll(ctx context.Context, b Backend, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := b.Open(ctx, path)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, joinContextCallError(err, ctxErr)
		}
		return nil, err
	}
	defer f.Close()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	size := f.Size()
	if size == 0 {
		return []byte{}, nil
	}
	buf := make([]byte, size)
	var off int64
	for off < size {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		readOff := off
		end := min(off+transferChunkSize, size)
		var n int
		if contextual, ok := f.(interface {
			ReadAtContext(context.Context, []byte, int64) (int, error)
		}); ok {
			n, err = contextual.ReadAtContext(ctx, buf[off:end], off)
		} else {
			n, err = f.ReadAt(buf[off:end], off)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("readall: read %s off=%d: %w", path, readOff, joinContextCallError(err, ctxErr))
		}
		off += int64(n)
		if err != nil {
			if errors.Is(err, io.EOF) && off >= size {
				break
			}
			return nil, fmt.Errorf("readall: read %s off=%d: %w", path, off, err)
		}
	}
	return buf, nil
}

// WriteAll creates path on b (which must be a WritableBackend) and writes
// data in one shot, committing on Close. Used to publish an immutable
// RFile produced by a flush or compaction. Returns ErrReadOnly if b can't
// write. ctx is polled before and immediately after each chunk write; a
// write already blocked in the backend may still finish its current chunk,
// and if that completes the object, WriteAll still waits for that backend
// call to return before observing ctx.Err(). On an unsuccessful path,
// WriteAll aborts the writer when it supports Aborter.
func WriteAll(ctx context.Context, b Backend, path string, data []byte) (err error) {
	wb, ok := b.(WritableBackend)
	if !ok {
		return ErrReadOnly
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	w, err := wb.Create(ctx, path)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("writeall: create %s: %w", path, joinContextCallError(err, ctxErr))
		}
		return fmt.Errorf("writeall: create %s: %w", path, err)
	}
	var cleanupState WriteCleanupState
	defer AbortOnError(&err, w, &cleanupState)
	for off := 0; off < len(data); {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := min(off+transferChunkSize, len(data))
		writeOff := off
		n, err := w.Write(data[off:end])
		off += n
		if ctxErr := ctx.Err(); ctxErr != nil {
			if err != nil {
				return fmt.Errorf("writeall: write %s off=%d: %w", path, writeOff, joinContextCallError(err, ctxErr))
			}
			if n != end-writeOff {
				return fmt.Errorf("writeall: write %s off=%d: %w", path, writeOff, joinContextCallError(io.ErrShortWrite, ctxErr))
			}
			return fmt.Errorf("writeall: write %s off=%d: %w", path, writeOff, ctxErr)
		}
		if err != nil {
			return fmt.Errorf("writeall: write %s off=%d: %w", path, writeOff, err)
		}
		if n != end-writeOff {
			return fmt.Errorf("writeall: write %s off=%d: %w", path, writeOff, io.ErrShortWrite)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	cleanupState.MarkCloseAttempted()
	if err := w.Close(); err != nil {
		return fmt.Errorf("writeall: close %s: %w", path, err)
	}
	return nil
}

func joinContextCallError(callErr, ctxErr error) error {
	if ctxErr == nil {
		return callErr
	}
	if callErr == nil {
		return ctxErr
	}
	return errors.Join(callErr, ctxErr)
}

// CleanupUnsuccessfulWrite abandons a writer after a failed write operation
// and joins any cleanup failure with primaryErr. Abort-capable transactional
// writers are never closed (and therefore never committed) on this path.
// When state reports that Close was already attempted, legacy non-Aborter
// writers are not closed again.
func CleanupUnsuccessfulWrite(primaryErr error, w Writer, states ...*WriteCleanupState) error {
	if primaryErr == nil {
		return nil
	}
	if IsCommittedWriteError(primaryErr) {
		return primaryErr
	}
	var state *WriteCleanupState
	if len(states) > 0 {
		state = states[0]
	}
	var cleanupErr error
	if aborter, ok := w.(Aborter); ok {
		cleanupErr = aborter.Abort()
	} else if state == nil || !state.closeAttempted {
		cleanupErr = w.Close()
	}
	if cleanupErr == nil {
		return primaryErr
	}
	return errors.Join(primaryErr, cleanupErr)
}

// AbortOnError abandons w when errp points at a non-nil error. It is intended
// for use in deferred cleanup by direct Writer users:
//
//	var cleanupState storage.WriteCleanupState
//	defer func() { storage.AbortOnError(&err, w, &cleanupState) }()
//
// Writers that do not implement Aborter fall back to Close during cleanup
// unless Close was already attempted.
func AbortOnError(errp *error, w Writer, states ...*WriteCleanupState) {
	if errp == nil || *errp == nil || w == nil {
		return
	}
	*errp = CleanupUnsuccessfulWrite(*errp, w, states...)
}
