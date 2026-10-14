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

package tablet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/storage"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
)

// TestMultiRunCompactionKeepsCrossRunTombstone: a tombstone in one embedding
// run that deletes a cell in another run must survive a mixed-state
// compaction. Each run compacts to its own output, so neither output is the
// tablet's whole data: dropping the tombstone (FullMajorCompaction) in run
// B's output would resurrect the cell run A's output still holds (#704).
func TestMultiRunCompactionKeepsCrossRunTombstone(t *testing.T) {
	tb, err := Open(t.TempDir(), Options{Backend: memory.New()})
	if err != nil {
		t.Fatal(err)
	}
	defer tb.Close()
	write := func(state embeddingspace.FileState, mutate func(m *cclient.Mutation)) {
		t.Helper()
		if err := tb.SetDefaultEmbedding(state); err != nil {
			t.Fatal(err)
		}
		m, err := cclient.NewMutation([]byte("row"))
		if err != nil {
			t.Fatal(err)
		}
		mutate(m)
		if err := tb.Write([]*cclient.Mutation{m}); err != nil {
			t.Fatal(err)
		}
		if err := tb.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	a, b := embeddingspace.Has("space-a"), embeddingspace.Has("space-b")
	write(a, func(m *cclient.Mutation) { m.Put([]byte("cf"), []byte("cq"), nil, 1, []byte("doomed")) })
	write(b, func(m *cclient.Mutation) { m.Delete([]byte("cf"), []byte("cq"), nil, 2) })
	visible := func() int {
		t.Helper()
		src, closeAll, err := tb.Source(iterrt.IteratorEnvironment{Scope: iterrt.ScopeScan})
		if err != nil {
			t.Fatal(err)
		}
		defer closeAll()
		del := iterrt.NewDeletingIterator()
		if err := del.Init(src, nil, iterrt.IteratorEnvironment{Scope: iterrt.ScopeScan}); err != nil {
			t.Fatal(err)
		}
		if err := del.Seek(iterrt.InfiniteRange(), nil, false); err != nil {
			t.Fatal(err)
		}
		n := 0
		for del.HasTop() {
			if !del.GetTopKey().Deleted {
				n++
			}
			if err := del.Next(); err != nil {
				t.Fatal(err)
			}
		}
		return n
	}
	if n := visible(); n != 0 {
		t.Fatalf("before compaction %d cells visible, want 0", n)
	}
	if err := tb.Compact(nil); err != nil {
		t.Fatal(err)
	}
	if got := tb.FileCount(); got != 2 {
		t.Fatalf("mixed-state compaction produced %d files, want one per run (2)", got)
	}
	if n := visible(); n != 0 {
		t.Fatalf("after compaction %d cells visible, want 0 (tombstone dropped across runs)", n)
	}
}

// strayFixture opens a tablet under a frozen clock and drops an untracked
// object at the name the clock would give the next file — what an export
// without a ProducerID shipping into this directory looks like.
func strayFixture(t *testing.T, stray string) (*Tablet, *memory.Backend, string) {
	t.Helper()
	withFrozenClock(t, 5000)
	backend := memory.New()
	dir := t.TempDir()
	tb, err := Open(dir, Options{Backend: backend})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tb.Close() })
	path := filepath.Join(dir, stray)
	backend.Put(path, []byte("shipped"))
	return tb, backend, path
}

func objectBytes(t *testing.T, b storage.Backend, path string) []byte {
	t.Helper()
	data, err := storage.ReadAll(context.Background(), b, path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestFlushSkipsStrayObjectName: a flush never writes over an object shipped
// into its directory after Open (#704, low).
func TestFlushSkipsStrayObjectName(t *testing.T) {
	tb, backend, stray := strayFixture(t, fmt.Sprintf("F%013d.rf", 5000))
	writeAndFlush(t, tb, "v")
	if got := objectBytes(t, backend, stray); !bytes.Equal(got, []byte("shipped")) {
		t.Fatal("flush replaced the shipped object")
	}
	if files := tb.RFiles(); len(files) != 1 || files[0] == stray {
		t.Fatalf("files = %v", files)
	}
}

// TestCompactionSkipsStrayObjectName: likewise for a compaction output.
func TestCompactionSkipsStrayObjectName(t *testing.T) {
	tb, backend, _ := strayFixture(t, "unused")
	writeAndFlush(t, tb, "v1")
	writeAndFlush(t, tb, "v2")
	stray := filepath.Join(tb.dir, fmt.Sprintf("C%013d-000.rf", tb.nextNameMillis()))
	backend.Put(stray, []byte("shipped"))
	if err := tb.Compact(nil); err != nil {
		t.Fatal(err)
	}
	if got := objectBytes(t, backend, stray); !bytes.Equal(got, []byte("shipped")) {
		t.Fatal("compaction replaced the shipped object")
	}
	if files := tb.RFiles(); len(files) != 1 || files[0] == stray {
		t.Fatalf("files = %v", files)
	}
}

// TestCompactionSkipsStrayObjectAtLaterOutputIndex: with several runs, the
// probe covers every output name, not just the first.
func TestCompactionSkipsStrayObjectAtLaterOutputIndex(t *testing.T) {
	tb, backend, _ := strayFixture(t, "unused")
	for _, space := range []string{"space-a", "space-b"} {
		if err := tb.SetDefaultEmbedding(embeddingspace.Has(space)); err != nil {
			t.Fatal(err)
		}
		writeAndFlush(t, tb, space)
	}
	stray := filepath.Join(tb.dir, fmt.Sprintf("C%013d-001.rf", tb.nextNameMillis()))
	backend.Put(stray, []byte("shipped"))
	if err := tb.Compact(nil); err != nil {
		t.Fatal(err)
	}
	if got := objectBytes(t, backend, stray); !bytes.Equal(got, []byte("shipped")) {
		t.Fatal("compaction replaced the shipped object at output index 1")
	}
	files := tb.RFiles()
	if len(files) != 2 {
		t.Fatalf("files = %v, want one per run", files)
	}
	for _, f := range files {
		if f == stray {
			t.Fatalf("files = %v include the stray", files)
		}
	}
}

// probeFailBackend fails Open of any path that does not exist yet — what a
// name probe asks — with a transient error, while failing is set. Reads of
// existing objects still work.
type probeFailBackend struct {
	*memory.Backend
	failing bool
}

var errSlowDown = errors.New("503 slow down")

func (b *probeFailBackend) Open(ctx context.Context, path string) (storage.File, error) {
	f, err := b.Backend.Open(ctx, path)
	if b.failing && errors.Is(err, storage.ErrNotFound) {
		return nil, errSlowDown
	}
	return f, err
}

// TestFlushFailsOnProbeError: a probe that cannot tell whether a name is
// free fails the flush instead of assuming it is: no file is added, and the
// write stays in the memtable and the WAL until a later flush succeeds.
func TestFlushFailsOnProbeError(t *testing.T) {
	backend := &probeFailBackend{Backend: memory.New()}
	dir := t.TempDir()
	tb, err := Open(dir, Options{Backend: backend})
	if err != nil {
		t.Fatal(err)
	}
	m, err := cclient.NewMutation([]byte("r"))
	if err != nil {
		t.Fatal(err)
	}
	m.PutLatest([]byte("cf"), []byte("cq"), nil, []byte("v"))
	if err := tb.Write([]*cclient.Mutation{m}); err != nil {
		t.Fatal(err)
	}
	backend.failing = true
	if err := tb.Flush(); !errors.Is(err, errSlowDown) {
		t.Fatalf("flush error = %v, want the probe error", err)
	}
	if got := tb.FileCount(); got != 0 {
		t.Fatalf("files after failed flush = %d", got)
	}
	if got := tb.MemtableSize(); got != 1 {
		t.Fatalf("memtable after failed flush = %d cells, want 1", got)
	}
	// The WAL still holds the write: a reopen replays it.
	_ = tb.Close() // its flush fails the same way
	backend.failing = false
	reopened, err := Open(dir, Options{Backend: backend})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := reopened.MemtableSize(); got != 1 {
		t.Fatalf("memtable after reopen = %d cells, want the WAL's 1", got)
	}
	if err := reopened.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := reopened.FileCount(); got != 1 {
		t.Fatalf("files after recovery = %d, want 1", got)
	}
}

// TestCompactionFailsOnProbeError: likewise a compaction fails, leaving the
// file set and the name high-water mark as they were.
func TestCompactionFailsOnProbeError(t *testing.T) {
	backend := &probeFailBackend{Backend: memory.New()}
	tb, err := Open(t.TempDir(), Options{Backend: backend})
	if err != nil {
		t.Fatal(err)
	}
	defer tb.Close()
	writeAndFlush(t, tb, "a")
	writeAndFlush(t, tb, "b")
	before, mark := tb.RFiles(), tb.nameHighWater
	backend.failing = true
	if err := tb.Compact(nil); !errors.Is(err, errSlowDown) {
		t.Fatalf("compaction error = %v, want the probe error", err)
	}
	if got := tb.RFiles(); !reflect.DeepEqual(got, before) {
		t.Fatalf("files after failed compaction = %v, want %v", got, before)
	}
	if tb.nameHighWater != mark {
		t.Fatalf("name high-water mark moved from %d to %d", mark, tb.nameHighWater)
	}
	backend.failing = false
	if err := tb.Compact(nil); err != nil {
		t.Fatal(err)
	}
	if got := tb.FileCount(); got != 1 {
		t.Fatalf("files after recovery = %d, want 1", got)
	}
}
