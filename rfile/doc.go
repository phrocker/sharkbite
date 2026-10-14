// Package rfile is Shoal's public API for Accumulo RFiles: writing one,
// reading one back sequentially or by seeking, and reading several as a single
// merged stream.
//
// It is the Go equivalent of Sharkbite's RFileOperations, SequentialRFile,
// RFile, KeyValueIterator, Seekable and StreamRelocation surfaces, and it
// speaks the pinned Accumulo 4 RFile format through Shoal's own reader and
// writer.
//
// Lifecycle. Every reader and writer owns the file handles it opens and
// releases them in Close, which is idempotent. Operations after Close report
// ErrClosed rather than touching a released handle.
//
// Cancellation. Every operation that can touch the file takes a context and
// checks it before doing work, so a cancelled caller stops at the next
// operation instead of reading the rest of the file.
//
// Concurrency. A reader or writer is a cursor. Its methods are individually
// safe to call from multiple goroutines — each one holds the cursor's lock, so
// a concurrent Close can never free a handle mid-read — but two goroutines
// sharing one reader interleave their Next calls and each sees only part of
// the stream. Give each goroutine its own reader; independent readers over the
// same path are fully parallel because each opens its own handle.
//
// Memory. Nothing is borrowed. Keys and values handed to a writer are copied
// before they are encoded, and keys and values returned by a reader are copies
// that stay valid after the cursor advances or closes, so a caller can never
// observe a freed or reused buffer. Rows, families, qualifiers, visibilities
// and values are arbitrary binary data: NUL bytes and invalid UTF-8 round-trip
// unchanged.
//
// Deletes. An RFile can hold tombstones, so Entry carries the deleted flag
// that accumulo.Key does not. Reading several files with MergeOptions.
// ApplyDeletes applies them the way Accumulo's DeletingIterator does.
package rfile
