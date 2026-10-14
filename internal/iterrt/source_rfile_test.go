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

package iterrt

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/phrocker/shoal-oss/internal/rfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile/block"
)

// writeTestRFile writes cells through the shoal RFile writer and returns
// the on-disk path. cells must be in wire.Key.Compare order.
func writeTestRFile(t *testing.T, cells []kv) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "src.rf")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	w, err := rfile.NewWriter(f, rfile.WriterOptions{Codec: block.CodecNone, IndexBlockSize: 256})
	if err != nil {
		f.Close()
		t.Fatalf("NewWriter: %v", err)
	}
	for i, c := range cells {
		if err := w.Append(c.k, c.v); err != nil {
			f.Close()
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	if err := w.Close(); err != nil {
		f.Close()
		t.Fatalf("Writer.Close: %v", err)
	}
	f.Close()
	return path
}

// openerFor returns an RFileOpener over path — re-reads the file bytes
// and constructs an independent rfile.Reader each call.
func openerFor(t *testing.T, path string) RFileOpener {
	t.Helper()
	return func() (*rfile.Reader, error) {
		bs, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		bc, err := bcfile.NewReader(bytes.NewReader(bs), int64(len(bs)))
		if err != nil {
			return nil, err
		}
		return rfile.Open(bc, block.Default())
	}
}

// TestRFileSource_ScansEverything: a leaf source over an RFile surfaces
// every cell in key order.
func TestRFileSource_ScansEverything(t *testing.T) {
	cells := versioned()
	path := writeTestRFile(t, cells)
	opener := openerFor(t, path)
	rdr, err := opener()
	if err != nil {
		t.Fatalf("opener: %v", err)
	}
	src := NewRFileSource(rdr, opener)
	if err := src.Init(nil, nil, IteratorEnvironment{}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := src.Seek(InfiniteRange(), nil, false); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	got, err := drain(src)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(got) != len(cells) {
		t.Fatalf("got %d cells, want %d", len(got), len(cells))
	}
	for i := range cells {
		if got[i].k.Compare(cells[i].k) != 0 {
			t.Errorf("cell %d key: got %+v want %+v", i, got[i].k, cells[i].k)
		}
		if !bytes.Equal(got[i].v, cells[i].v) {
			t.Errorf("cell %d value: got %q want %q", i, got[i].v, cells[i].v)
		}
	}
}

// TestRFileSource_SeekRange: a bounded range surfaces only cells inside it.
func TestRFileSource_SeekRange(t *testing.T) {
	cells := versioned()
	path := writeTestRFile(t, cells)
	opener := openerFor(t, path)
	rdr, err := opener()
	if err != nil {
		t.Fatalf("opener: %v", err)
	}
	src := NewRFileSource(rdr, opener)
	if err := src.Init(nil, nil, IteratorEnvironment{}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// Start strictly after r1's cells: a row-only key "r2".
	rng := Range{Start: mk("r2", "", "", "", 0), StartInclusive: true, InfiniteEnd: true}
	if err := src.Seek(rng, nil, false); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	got, err := drain(src)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	for _, c := range got {
		if string(c.k.Row) != "r2" {
			t.Errorf("range leaked row %q", c.k.Row)
		}
	}
	if len(got) != 2 {
		t.Fatalf("got %d cells in [r2,+inf), want 2", len(got))
	}
}

// TestRFileSource_FeedsCompactionStack: RFileSource → MergingIterator →
// VersioningIterator, the full C1 compaction stack over real RFiles.
func TestRFileSource_FeedsCompactionStack(t *testing.T) {
	fileA := []kv{
		{mk("r1", "cf", "a", "", 30), []byte("A-r1a-v30")},
		{mk("r1", "cf", "a", "", 10), []byte("A-r1a-v10")},
		{mk("r2", "cf", "a", "", 5), []byte("A-r2a-v5")},
	}
	fileB := []kv{
		{mk("r1", "cf", "a", "", 20), []byte("B-r1a-v20")},
		{mk("r3", "cf", "a", "", 7), []byte("B-r3a-v7")},
	}
	pathA := writeTestRFile(t, fileA)
	pathB := writeTestRFile(t, fileB)
	openerA := openerFor(t, pathA)
	openerB := openerFor(t, pathB)
	rdrA, err := openerA()
	if err != nil {
		t.Fatalf("openerA: %v", err)
	}
	rdrB, err := openerB()
	if err != nil {
		t.Fatalf("openerB: %v", err)
	}

	m := NewMergingIterator(NewRFileSource(rdrA, openerA), NewRFileSource(rdrB, openerB))
	v := NewVersioningIterator()
	env := IteratorEnvironment{Scope: ScopeMajc}
	if err := v.Init(m, map[string]string{VersioningOption: "1"}, env); err != nil {
		t.Fatalf("vers Init: %v", err)
	}
	if err := m.Init(nil, nil, env); err != nil {
		t.Fatalf("merge Init: %v", err)
	}
	if err := v.Seek(InfiniteRange(), nil, false); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	got, err := drain(v)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	// newest version per coordinate across both files.
	want := []string{"A-r1a-v30", "A-r2a-v5", "B-r3a-v7"}
	if gs := valStrings(got); !eqStr(gs, want) {
		t.Fatalf("compaction stack got %v want %v", gs, want)
	}
}

// TestRFileSource_DeepCopyIndependent: a DeepCopy reads the full file
// without disturbing the original's position.
func TestRFileSource_DeepCopyIndependent(t *testing.T) {
	cells := versioned()
	path := writeTestRFile(t, cells)
	opener := openerFor(t, path)
	rdr, err := opener()
	if err != nil {
		t.Fatalf("opener: %v", err)
	}
	src := NewRFileSource(rdr, opener)
	if err := src.Init(nil, nil, IteratorEnvironment{}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := src.Seek(InfiniteRange(), nil, false); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	if err := src.Next(); err != nil { // consume one
		t.Fatalf("Next: %v", err)
	}
	firstAfter := string(src.GetTopValue())

	cp := src.DeepCopy(IteratorEnvironment{})
	if err := cp.Seek(InfiniteRange(), nil, false); err != nil {
		t.Fatalf("copy Seek: %v", err)
	}
	cpGot, err := drain(cp)
	if err != nil {
		t.Fatalf("copy drain: %v", err)
	}
	if len(cpGot) != len(cells) {
		t.Fatalf("copy saw %d cells, want full %d", len(cpGot), len(cells))
	}
	if !src.HasTop() || string(src.GetTopValue()) != firstAfter {
		t.Fatalf("original disturbed by DeepCopy")
	}
}

// TestRFileSource_LeafRejectsSource: a leaf iterator must be Init'd with
// a nil source.
func TestRFileSource_LeafRejectsSource(t *testing.T) {
	path := writeTestRFile(t, versioned())
	opener := openerFor(t, path)
	rdr, err := opener()
	if err != nil {
		t.Fatalf("opener: %v", err)
	}
	src := NewRFileSource(rdr, opener)
	if err := src.Init(newSliceSource(), nil, IteratorEnvironment{}); err == nil {
		t.Fatal("expected error Init'ing a leaf with a non-nil source")
	}
}
