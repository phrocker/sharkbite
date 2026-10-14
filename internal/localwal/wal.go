// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//   https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

// Package localwal is a simplified write-ahead log for the embedded engine.
//
// Unlike the distributed qwal package (which reads WAL segments from a
// quorum sidecar via gRPC), localwal writes directly to a local file and
// replays on startup for crash recovery. The on-disk format is deliberately
// simple — length-prefixed mutation records — because:
//
//  1. We don't need quorum agreement (single-process embedded mode).
//  2. We don't need to interop with JVM tserver WAL segments.
//  3. We want a configurable fsync policy: fast fsync-per-batch by
//     default, with deferred / group-commit tiers (see SyncMode) for
//     workloads that prefer lower write latency over power-loss
//     durability.
//
// When the tablet flushes its memtable to an RFile, the WAL is truncated.
// Recovery on startup: replay the WAL into a fresh memtable.
package localwal

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/rfile/wire"
)

// SyncMode controls how aggressively the WAL forces data to stable
// storage. It trades durability against write latency, mirroring the
// durability tiers exposed by engines such as SQLite (PRAGMA synchronous).
type SyncMode int

const (
	// SyncFull fsyncs after every Append. A committed write survives an
	// OS or power crash. This is the safe default.
	SyncFull SyncMode = iota

	// SyncNormal writes each batch to the WAL file but defers the fsync
	// to flush/checkpoint time (Sync, Truncate, Close). A committed write
	// survives a process crash (the bytes are in the OS page cache and
	// replayed on restart) but may be lost on an OS or power crash. This
	// matches SQLite's WAL-mode synchronous=NORMAL and removes the
	// per-commit fsync from the hot write path.
	SyncNormal

	// SyncOff never fsyncs from the WAL itself (data reaches stable
	// storage only when the OS chooses to). Fastest, least durable;
	// intended for bulk-load / rebuildable data.
	SyncOff
)

// Option configures a WAL at Open time.
type Option func(*WAL)

// WithSyncMode sets the WAL durability tier. Default is SyncFull.
func WithSyncMode(m SyncMode) Option {
	return func(w *WAL) { w.mode = m }
}

// WithSyncInterval enables group commit: instead of fsyncing on every
// Append (SyncFull) or only at checkpoint (SyncNormal), a background
// goroutine fsyncs at most once per interval whenever there are
// unsynced writes. This bounds the worst-case data-loss window to
// roughly interval while keeping fsync off the per-write hot path.
//
// An interval <= 0 disables the ticker (the SyncMode alone decides).
// The ticker only does useful work under SyncNormal/SyncOff; under
// SyncFull every Append already fsyncs, so it is a no-op there.
func WithSyncInterval(d time.Duration) Option {
	return func(w *WAL) { w.interval = d }
}

// WAL is a single-file write-ahead log. Goroutine-safe for Append;
// not safe for concurrent Append + Replay.
type WAL struct {
	mu   sync.Mutex
	f    *os.File
	path string
	seq  int64
	mode SyncMode

	// dirty is true when batches have been written but not yet fsynced
	// (only possible under SyncNormal/SyncOff). Sync/Truncate/Close and
	// the optional background ticker use it to fsync once before
	// relinquishing or discarding the file.
	dirty bool

	// interval, when > 0, runs a background group-commit fsync ticker.
	interval time.Duration
	stop     chan struct{}
	wg       sync.WaitGroup

	// buf is a reusable serialization scratch reused across Appends so a
	// batch is encoded entirely in memory and flushed with a single
	// Write syscall + one fsync. Profiling showed the previous
	// field-at-a-time write path spent ~77% of write time in tiny
	// per-field writeFile syscalls (33 per 3-cell mutation); buffering
	// collapses that to one write. Guarded by mu.
	buf []byte
}

// Open opens or creates the WAL file at path. The file is opened in
// append mode; existing content is preserved for replay. Options may
// override the default durability tier (SyncFull).
func Open(path string, opts ...Option) (*WAL, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("localwal: open %s: %w", path, err)
	}
	w := &WAL{f: f, path: path}
	for _, opt := range opts {
		opt(w)
	}
	if w.interval > 0 {
		w.stop = make(chan struct{})
		w.wg.Add(1)
		go w.syncLoop()
	}
	return w, nil
}

// syncLoop periodically fsyncs pending writes (group commit). It runs
// only when WithSyncInterval set a positive interval.
func (w *WAL) syncLoop() {
	defer w.wg.Done()
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-w.stop:
			return
		case <-t.C:
			_ = w.Sync()
		}
	}
}

// Append writes a batch of mutations to the WAL. Whether the batch is
// fsynced before returning depends on the configured SyncMode:
//   - SyncFull   fsyncs before returning (durable across power loss).
//   - SyncNormal/SyncOff record the bytes and return immediately; the
//     fsync happens at the next Sync/Truncate/Close or background tick.
//
// Each mutation is serialized as: [4-byte row length][row]
// [4-byte entry count] then for each entry: [4-byte cf len][cf]
// [4-byte cq len][cq][4-byte cv len][cv][8-byte timestamp]
// [1-byte deleted][4-byte val len][val].
//
// Returns the WAL sequence number assigned to this batch.
func (w *WAL) Append(mutations []*cclient.Mutation) (int64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// Serialize the whole batch into the reusable buffer, then flush it
	// with one Write. Encoding in memory avoids the dozens of tiny
	// per-field write syscalls the field-at-a-time path incurred.
	w.buf = w.buf[:0]
	for _, m := range mutations {
		w.buf = appendMutation(w.buf, m)
	}
	if len(w.buf) > 0 {
		if _, err := w.f.Write(w.buf); err != nil {
			return w.seq, fmt.Errorf("localwal: write: %w", err)
		}
		w.dirty = true
	}
	if w.mode == SyncFull {
		if err := w.syncLocked(); err != nil {
			return w.seq, err
		}
	}
	w.seq++
	return w.seq - 1, nil
}

// Sync flushes any buffered, unsynced writes to stable storage. It is a
// no-op when nothing is pending. Use it to force durability under
// SyncNormal/SyncOff (e.g., before a checkpoint).
func (w *WAL) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.syncLocked()
}

// syncLocked fsyncs the file if there are pending writes. Caller holds mu.
func (w *WAL) syncLocked() error {
	if !w.dirty {
		return nil
	}
	if err := w.f.Sync(); err != nil {
		return fmt.Errorf("localwal: sync: %w", err)
	}
	w.dirty = false
	return nil
}

// Replay reads all mutations from the WAL and calls fn for each one.
// Used at startup to rebuild the memtable from unflushed writes.
// Returns the number of mutations replayed.
func (w *WAL) Replay(fn func(m *cclient.Mutation) error) (int, error) {
	if _, err := w.f.Seek(0, io.SeekStart); err != nil {
		return 0, fmt.Errorf("localwal: seek: %w", err)
	}

	count := 0
	for {
		m, err := readMutation(w.f)
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if err != nil {
			return count, fmt.Errorf("localwal: replay entry %d: %w", count, err)
		}
		if err := fn(m); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

// Truncate discards all WAL data. Called after a successful flush, which
// has already made the data durable in an RFile, so any pending unsynced
// WAL bytes can be dropped without an fsync.
// On Windows, os.File.Truncate on an open file may fail, so we close,
// recreate, and reopen the file.
func (w *WAL) Truncate() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	// The data is durable elsewhere now; nothing left to sync.
	w.dirty = false

	// Try the fast path first (works on Linux/macOS).
	if err := w.f.Truncate(0); err == nil {
		_, seekErr := w.f.Seek(0, io.SeekStart)
		return seekErr
	}

	// Fallback for Windows: close → recreate → reopen.
	path := w.path
	w.f.Close()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("localwal: truncate: %w", err)
	}
	w.f = f
	return nil
}

// Close stops the background sync ticker (if any), fsyncs any pending
// writes, and closes the WAL file.
func (w *WAL) Close() error {
	if w.stop != nil {
		close(w.stop)
		w.wg.Wait()
		w.stop = nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	syncErr := w.syncLocked()
	closeErr := w.f.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

// Path returns the WAL file path.
func (w *WAL) Path() string { return w.path }

// appendMutation serializes one mutation into dst and returns the grown
// slice. Encoding is identical to the prior field-at-a-time writer; only
// the destination changed (in-memory buffer instead of direct file
// writes), so existing WAL files replay unchanged.
func appendMutation(dst []byte, m *cclient.Mutation) []byte {
	entries := m.Entries()

	dst = appendBytes(dst, m.Row())
	dst = appendUint32(dst, uint32(len(entries)))
	for _, e := range entries {
		dst = appendBytes(dst, e.ColFamily)
		dst = appendBytes(dst, e.ColQualifier)
		dst = appendBytes(dst, e.ColVisibility)
		dst = appendInt64(dst, e.Timestamp)
		del := byte(0)
		if e.Deleted {
			del = 1
		}
		dst = append(dst, del)
		dst = appendBytes(dst, e.Value)
	}
	return dst
}

func appendBytes(dst, b []byte) []byte {
	dst = appendUint32(dst, uint32(len(b)))
	return append(dst, b...)
}

func appendUint32(dst []byte, v uint32) []byte {
	return binary.BigEndian.AppendUint32(dst, v)
}

func appendInt64(dst []byte, v int64) []byte {
	return binary.BigEndian.AppendUint64(dst, uint64(v))
}

// readMutation deserializes one mutation from a reader.
func readMutation(r io.Reader) (*cclient.Mutation, error) {
	row, err := readBytes(r)
	if err != nil {
		return nil, err
	}
	m, err := cclient.NewMutation(row)
	if err != nil {
		return nil, err
	}
	count, err := readUint32(r)
	if err != nil {
		return nil, err
	}
	for i := uint32(0); i < count; i++ {
		cf, err := readBytes(r)
		if err != nil {
			return nil, err
		}
		cq, err := readBytes(r)
		if err != nil {
			return nil, err
		}
		cv, err := readBytes(r)
		if err != nil {
			return nil, err
		}
		ts, err := readInt64(r)
		if err != nil {
			return nil, err
		}
		var delBuf [1]byte
		if _, err := io.ReadFull(r, delBuf[:]); err != nil {
			return nil, err
		}
		val, err := readBytes(r)
		if err != nil {
			return nil, err
		}
		if delBuf[0] == 1 {
			m.Delete(cf, cq, cv, ts)
		} else {
			m.Put(cf, cq, cv, ts, val)
		}
	}
	return m, nil
}

// Wire helpers — simple length-prefixed encoding.

func readBytes(r io.Reader) ([]byte, error) {
	n, err := readUint32(r)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, nil
	}
	buf := make([]byte, n)
	_, err = io.ReadFull(r, buf)
	return buf, err
}

func readUint32(r io.Reader) (uint32, error) {
	var buf [4]byte
	_, err := io.ReadFull(r, buf[:])
	return binary.BigEndian.Uint32(buf[:]), err
}

func readInt64(r io.Reader) (int64, error) {
	var buf [8]byte
	_, err := io.ReadFull(r, buf[:])
	return int64(binary.BigEndian.Uint64(buf[:])), err
}

// Ensure wire.Key is importable (used by tablet layer, not directly here).
var _ = wire.Key{}
