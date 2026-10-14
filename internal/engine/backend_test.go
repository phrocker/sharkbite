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

package engine_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
)

// countRFilesOnDisk walks dir and counts files ending in .rf — used to
// assert that with a non-local backend, flushed RFiles do NOT land on the
// local filesystem (only the WAL does).
func countRFilesOnDisk(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && filepath.Ext(path) == ".rf" {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestEngine_MemoryBackend_FlushReadReopen verifies the storage.Backend
// wiring: flushed RFiles are written to the (in-memory) backend rather
// than local disk, scans read them back through the backend, and a reopen
// recovers the file set via the backend's List (manifest discovery).
func TestEngine_MemoryBackend_FlushReadReopen(t *testing.T) {
	dir := t.TempDir()
	mb := memory.New() // shared across the reopen below

	eng, err := engine.Open(dir, engine.Options{Backend: mb})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.CreateTable("graph", engine.TableOptions{}); err != nil {
		t.Fatal(err)
	}

	const n = 50
	for i := 0; i < n; i++ {
		m, _ := cclient.NewMutation([]byte(fmt.Sprintf("entity:%04d", i)))
		m.PutLatest([]byte("props"), []byte("label"), nil, []byte(fmt.Sprintf("node-%d", i)))
		if err := eng.Write("graph", []*cclient.Mutation{m}); err != nil {
			t.Fatal(err)
		}
	}
	if err := eng.Flush("graph"); err != nil {
		t.Fatal(err)
	}

	// The flushed RFile must live in the backend, not on local disk.
	if got := countRFilesOnDisk(t, dir); got != 0 {
		t.Fatalf("expected 0 .rf files on local disk (backend holds them), found %d", got)
	}
	if len(mb.Keys()) == 0 {
		t.Fatal("expected the memory backend to hold at least one RFile")
	}

	// Scan reads back through the backend.
	if got := scanCount(t, eng); got != n {
		t.Fatalf("scan after flush returned %d cells, want %d", got, n)
	}
	eng.Close()

	// Reopen with the SAME backend instance: recovery must rediscover the
	// RFiles via the backend manifest (List), not os.ReadDir.
	eng2, err := engine.Open(dir, engine.Options{Backend: mb})
	if err != nil {
		t.Fatal(err)
	}
	defer eng2.Close()
	if got := scanCount(t, eng2); got != n {
		t.Fatalf("scan after reopen returned %d cells, want %d", got, n)
	}
}

// TestEngine_MemoryBackend_Compaction exercises the compaction write +
// input-delete path through the backend: after compacting several flushed
// RFiles, the merged data is intact and stale inputs are removed from the
// backend.
func TestEngine_MemoryBackend_Compaction(t *testing.T) {
	dir := t.TempDir()
	mb := memory.New()

	eng, err := engine.Open(dir, engine.Options{Backend: mb})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	if err := eng.CreateTable("graph", engine.TableOptions{}); err != nil {
		t.Fatal(err)
	}

	const flushes = 3
	const perFlush = 20
	for f := 0; f < flushes; f++ {
		for i := 0; i < perFlush; i++ {
			id := f*perFlush + i
			m, _ := cclient.NewMutation([]byte(fmt.Sprintf("entity:%04d", id)))
			m.PutLatest([]byte("props"), []byte("label"), nil, []byte(fmt.Sprintf("node-%d", id)))
			if err := eng.Write("graph", []*cclient.Mutation{m}); err != nil {
				t.Fatal(err)
			}
		}
		if err := eng.Flush("graph"); err != nil {
			t.Fatal(err)
		}
	}
	before := len(mb.Keys())
	if before < flushes {
		t.Fatalf("expected >= %d RFiles in backend before compaction, got %d", flushes, before)
	}

	if err := eng.Compact("graph", nil); err != nil {
		t.Fatal(err)
	}

	// Stale inputs should be removed: a full major compaction leaves one
	// output RFile per tablet.
	after := len(mb.Keys())
	if after >= before {
		t.Fatalf("expected fewer RFiles after compaction (inputs removed): before=%d after=%d", before, after)
	}
	if got := scanCount(t, eng); got != flushes*perFlush {
		t.Fatalf("scan after compaction returned %d cells, want %d", got, flushes*perFlush)
	}
}

func scanCount(t *testing.T, eng *engine.Engine) int {
	t.Helper()
	sc, err := eng.Scan("graph", iterrt.InfiniteRange(), engine.ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	n := 0
	for sc.Next() {
		n++
		if err := sc.Advance(); err != nil {
			t.Fatal(err)
		}
	}
	return n
}
