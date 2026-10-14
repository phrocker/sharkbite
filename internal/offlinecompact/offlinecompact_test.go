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

package offlinecompact

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/rfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile/block"
	"github.com/phrocker/shoal-oss/internal/rfile/wire"
	"github.com/phrocker/shoal-oss/internal/shadow/itercfg"
)

// --- test doubles -----------------------------------------------------

type fakeEnum struct {
	tablets []metadata.TabletInfo
	err     error
}

func (f fakeEnum) LocateTable(_ context.Context, _ string) ([]metadata.TabletInfo, error) {
	return f.tablets, f.err
}

type fakeResolver struct {
	stack   []iterrt.IterSpec
	skipped []itercfg.SkippedIter
	err     error
}

func (f fakeResolver) Resolve(_ context.Context, tableID string, scope iterrt.IteratorScope) (*itercfg.ResolvedStack, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &itercfg.ResolvedStack{
		TableID: tableID,
		Scope:   scope,
		Stack:   f.stack,
		Skipped: f.skipped,
	}, nil
}

// memStore is an in-memory RFileStore keyed by path.
type memStore struct {
	data     map[string][]byte
	readErr  map[string]error
	writeErr error
	writes   []string
}

func newMemStore() *memStore {
	return &memStore{data: map[string][]byte{}, readErr: map[string]error{}}
}

func (m *memStore) Read(_ context.Context, path string) ([]byte, error) {
	if err := m.readErr[path]; err != nil {
		return nil, err
	}
	b, ok := m.data[path]
	if !ok {
		return nil, fmt.Errorf("memStore: %q not found", path)
	}
	return b, nil
}

func (m *memStore) Write(_ context.Context, path string, data []byte) error {
	if m.writeErr != nil {
		return m.writeErr
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	m.data[path] = cp
	m.writes = append(m.writes, path)
	return nil
}

// --- RFile helpers ----------------------------------------------------

type kv struct {
	row, cf, cq string
	ts          int64
	val         string
}

func mkKey(row, cf, cq string, ts int64) *wire.Key {
	return &wire.Key{
		Row:             []byte(row),
		ColumnFamily:    []byte(cf),
		ColumnQualifier: []byte(cq),
		Timestamp:       ts,
	}
}

func buildRFile(t *testing.T, cells []kv) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := rfile.NewWriter(&buf, rfile.WriterOptions{})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	for i, c := range cells {
		if err := w.Append(mkKey(c.row, c.cf, c.cq, c.ts), []byte(c.val)); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return buf.Bytes()
}

func drainRFile(t *testing.T, image []byte) []kv {
	t.Helper()
	bc, err := bcfile.NewReader(bytes.NewReader(image), int64(len(image)))
	if err != nil {
		t.Fatalf("bcfile.NewReader: %v", err)
	}
	r, err := rfile.Open(bc, block.Default())
	if err != nil {
		t.Fatalf("rfile.Open: %v", err)
	}
	defer r.Close()
	var out []kv
	for {
		k, v, err := r.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		out = append(out, kv{
			row: string(k.Row), cf: string(k.ColumnFamily),
			cq: string(k.ColumnQualifier), ts: k.Timestamp, val: string(v),
		})
	}
}

func fileEntry(path string) metadata.FileEntry {
	return metadata.FileEntry{Path: path, RawQualifier: []byte(path)}
}

const dir = "hdfs://nn/accumulo/tables/2k/t-000abc/"

// seed writes an RFile into the store at dir+name and returns the entry.
func seed(t *testing.T, m *memStore, name string, cells []kv) metadata.FileEntry {
	t.Helper()
	path := dir + name
	m.data[path] = buildRFile(t, cells)
	return fileEntry(path)
}

func fixedName(n string) func() string { return func() string { return n } }

// --- tests ------------------------------------------------------------

func TestRun_MergesMultipleFiles(t *testing.T) {
	m := newMemStore()
	f1 := seed(t, m, "C1.rf", []kv{{row: "a", cf: "cf", cq: "q", ts: 10, val: "A"}})
	f2 := seed(t, m, "C2.rf", []kv{{row: "b", cf: "cf", cq: "q", ts: 10, val: "B"}})

	deps := Deps{
		Tablets: fakeEnum{tablets: []metadata.TabletInfo{{TableID: "2k", Files: []metadata.FileEntry{f1, f2}}}},
		Stacks:  fakeResolver{},
		Files:   m,
	}
	plan, err := Run(context.Background(), "2k", deps, Options{NewFileName: fixedName("Aout.rf")})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(plan.Results) != 1 {
		t.Fatalf("want 1 result, got %d", len(plan.Results))
	}
	res := plan.Results[0]
	if res.OutputPath != dir+"Aout.rf" {
		t.Fatalf("output path = %q", res.OutputPath)
	}
	if res.EntriesWritten != 2 {
		t.Fatalf("entries = %d, want 2", res.EntriesWritten)
	}
	got := drainRFile(t, m.data[res.OutputPath])
	want := []kv{
		{row: "a", cf: "cf", cq: "q", ts: 10, val: "A"},
		{row: "b", cf: "cf", cq: "q", ts: 10, val: "B"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("merged output\n got=%v\nwant=%v", got, want)
	}
	// Inputs preserved for oc-commit (byte-exact qualifier).
	if len(res.Inputs) != 2 || res.Inputs[0].Path != f1.Path {
		t.Fatalf("inputs not preserved: %+v", res.Inputs)
	}
}

func TestRun_SingleFileNoStackIsNoOp(t *testing.T) {
	m := newMemStore()
	f1 := seed(t, m, "C1.rf", []kv{{row: "a", cf: "cf", cq: "q", ts: 10, val: "A"}})

	deps := Deps{
		Tablets: fakeEnum{tablets: []metadata.TabletInfo{{TableID: "2k", Files: []metadata.FileEntry{f1}}}},
		Stacks:  fakeResolver{},
		Files:   m,
	}
	plan, err := Run(context.Background(), "2k", deps, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(plan.Results) != 0 || len(plan.NoOp) != 1 {
		t.Fatalf("want 0 results / 1 noop, got %d / %d", len(plan.Results), len(plan.NoOp))
	}
	if len(m.writes) != 0 {
		t.Fatalf("no-op tablet should not write, wrote %v", m.writes)
	}
}

func TestRun_SingleFileWithStackCompacts(t *testing.T) {
	m := newMemStore()
	// Two versions of the same coordinate; maxVersions=1 keeps the newest.
	// RFile order is Accumulo sort order: same coordinate sorts by
	// timestamp DESCENDING, so the newest (ts=10) is listed first.
	f1 := seed(t, m, "C1.rf", []kv{
		{row: "a", cf: "cf", cq: "q", ts: 10, val: "new"},
		{row: "a", cf: "cf", cq: "q", ts: 5, val: "old"},
	})
	deps := Deps{
		Tablets: fakeEnum{tablets: []metadata.TabletInfo{{TableID: "2k", Files: []metadata.FileEntry{f1}}}},
		Stacks: fakeResolver{stack: []iterrt.IterSpec{
			{Name: iterrt.IterVersioning, Options: map[string]string{iterrt.VersioningOption: "1"}},
		}},
		Files: m,
	}
	plan, err := Run(context.Background(), "2k", deps, Options{NewFileName: fixedName("Aout.rf")})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(plan.Results) != 1 {
		t.Fatalf("want 1 result, got %d", len(plan.Results))
	}
	got := drainRFile(t, m.data[dir+"Aout.rf"])
	if len(got) != 1 || got[0].val != "new" {
		t.Fatalf("versioning not applied: %v", got)
	}
}

func TestRun_AbortsOnSkippedIterator(t *testing.T) {
	m := newMemStore()
	f1 := seed(t, m, "C1.rf", []kv{{row: "a", cf: "cf", cq: "q", ts: 10, val: "A"}})
	f2 := seed(t, m, "C2.rf", []kv{{row: "b", cf: "cf", cq: "q", ts: 10, val: "B"}})
	deps := Deps{
		Tablets: fakeEnum{tablets: []metadata.TabletInfo{{TableID: "2k", Files: []metadata.FileEntry{f1, f2}}}},
		Stacks: fakeResolver{skipped: []itercfg.SkippedIter{
			{Name: "custom", Class: "com.example.MyIterator", Priority: 20},
		}},
		Files: m,
	}
	_, err := Run(context.Background(), "2k", deps, Options{})
	if err == nil {
		t.Fatal("expected abort on skipped iterator")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("com.example.MyIterator")) {
		t.Fatalf("error should name the unported class: %v", err)
	}
	if len(m.writes) != 0 {
		t.Fatalf("must not write on abort, wrote %v", m.writes)
	}
}

func TestRun_ReadErrorAborts(t *testing.T) {
	m := newMemStore()
	f1 := seed(t, m, "C1.rf", []kv{{row: "a", cf: "cf", cq: "q", ts: 10, val: "A"}})
	f2 := fileEntry(dir + "C2.rf")
	m.readErr[f2.Path] = errors.New("boom")
	deps := Deps{
		Tablets: fakeEnum{tablets: []metadata.TabletInfo{{TableID: "2k", Files: []metadata.FileEntry{f1, f2}}}},
		Stacks:  fakeResolver{},
		Files:   m,
	}
	_, err := Run(context.Background(), "2k", deps, Options{})
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("boom")) {
		t.Fatalf("expected read error to abort, got %v", err)
	}
}

func TestRun_MultipleTablets(t *testing.T) {
	m := newMemStore()
	// tablet 1 files
	a1 := seed(t, m, "A1.rf", []kv{{row: "a", cf: "c", cq: "q", ts: 1, val: "1"}})
	a2 := seed(t, m, "A2.rf", []kv{{row: "aa", cf: "c", cq: "q", ts: 1, val: "2"}})
	// tablet 2 files, different dir
	dir2 := "hdfs://nn/accumulo/tables/2k/t-000def/"
	b1p := dir2 + "B1.rf"
	b2p := dir2 + "B2.rf"
	m.data[b1p] = buildRFile(t, []kv{{row: "m", cf: "c", cq: "q", ts: 1, val: "3"}})
	m.data[b2p] = buildRFile(t, []kv{{row: "n", cf: "c", cq: "q", ts: 1, val: "4"}})

	tablets := []metadata.TabletInfo{
		{TableID: "2k", EndRow: []byte("g"), Files: []metadata.FileEntry{a1, a2}},
		{TableID: "2k", PrevRow: []byte("g"), Files: []metadata.FileEntry{fileEntry(b1p), fileEntry(b2p)}},
	}
	deps := Deps{Tablets: fakeEnum{tablets: tablets}, Stacks: fakeResolver{}, Files: m}

	n := 0
	name := func() string { n++; return fmt.Sprintf("Aout%d.rf", n) }
	plan, err := Run(context.Background(), "2k", deps, Options{NewFileName: name})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(plan.Results) != 2 {
		t.Fatalf("want 2 results, got %d", len(plan.Results))
	}
	if plan.Results[0].OutputPath != dir+"Aout1.rf" {
		t.Fatalf("tablet1 output dir wrong: %q", plan.Results[0].OutputPath)
	}
	if plan.Results[1].OutputPath != dir2+"Aout2.rf" {
		t.Fatalf("tablet2 output dir wrong: %q", plan.Results[1].OutputPath)
	}
}

func TestRun_EnumerateErrorPropagates(t *testing.T) {
	deps := Deps{
		Tablets: fakeEnum{err: errors.New("zk down")},
		Stacks:  fakeResolver{},
		Files:   newMemStore(),
	}
	_, err := Run(context.Background(), "2k", deps, Options{})
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("zk down")) {
		t.Fatalf("want enumerate error, got %v", err)
	}
}

func TestRun_MissingDeps(t *testing.T) {
	_, err := Run(context.Background(), "2k", Deps{}, Options{})
	if err == nil {
		t.Fatal("expected error for missing deps")
	}
}

func TestRun_EmptyTableID(t *testing.T) {
	deps := Deps{Tablets: fakeEnum{}, Stacks: fakeResolver{}, Files: newMemStore()}
	if _, err := Run(context.Background(), "", deps, Options{}); err == nil {
		t.Fatal("expected error for empty tableID")
	}
}

func TestOutputPath(t *testing.T) {
	cases := []struct {
		path, leaf, want string
	}{
		{"hdfs://nn/accumulo/tables/2k/t-abc/C1.rf", "Aout.rf", "hdfs://nn/accumulo/tables/2k/t-abc/Aout.rf"},
		{"file:///data/tables/2k/t-abc/C1.rf", "Aout.rf", "file:///data/tables/2k/t-abc/Aout.rf"},
		{"/data/tables/2k/t-abc/C1.rf", "Aout.rf", "/data/tables/2k/t-abc/Aout.rf"},
		{"gs://bkt/tables/2k/t-abc/C1.rf", "Aout.rf", "gs://bkt/tables/2k/t-abc/Aout.rf"},
	}
	for _, c := range cases {
		got, err := outputPath([]metadata.FileEntry{{Path: c.path}}, c.leaf)
		if err != nil {
			t.Fatalf("outputPath(%q): %v", c.path, err)
		}
		if got != c.want {
			t.Fatalf("outputPath(%q) = %q, want %q", c.path, got, c.want)
		}
	}
	if _, err := outputPath([]metadata.FileEntry{{Path: "noslash"}}, "x.rf"); err == nil {
		t.Fatal("expected error for path with no directory")
	}
	if _, err := outputPath(nil, "x.rf"); err == nil {
		t.Fatal("expected error for no inputs")
	}
}

func TestShouldCompact(t *testing.T) {
	stack := []iterrt.IterSpec{{Name: iterrt.IterVersioning}}
	f := func(n int) []metadata.FileEntry { return make([]metadata.FileEntry, n) }
	cases := []struct {
		files int
		stack []iterrt.IterSpec
		want  bool
	}{
		{0, nil, false},
		{0, stack, false},
		{1, nil, false},
		{1, stack, true},
		{2, nil, true},
		{3, stack, true},
	}
	for _, c := range cases {
		if got := shouldCompact(f(c.files), c.stack); got != c.want {
			t.Fatalf("shouldCompact(files=%d, stack=%d) = %v, want %v",
				c.files, len(c.stack), got, c.want)
		}
	}
}

func TestSelectTablets(t *testing.T) {
	// three tablets: (-inf, g], (g, p], (p, +inf)
	tablets := []metadata.TabletInfo{
		{EndRow: []byte("g")},
		{PrevRow: []byte("g"), EndRow: []byte("p")},
		{PrevRow: []byte("p")},
	}
	countRows := func(sel []metadata.TabletInfo) int { return len(sel) }

	if got := countRows(SelectTablets(tablets, nil, nil)); got != 3 {
		t.Fatalf("unbounded want 3, got %d", got)
	}
	// start "h" (in middle tablet) → drops first tablet (endRow g < h).
	sel := SelectTablets(tablets, []byte("h"), nil)
	if len(sel) != 2 || !bytes.Equal(sel[0].EndRow, []byte("p")) {
		t.Fatalf("start=h selection wrong: %+v", sel)
	}
	// end "g" → middle tablet has prevRow g >= g → excluded; last excluded;
	// only first tablet.
	sel = SelectTablets(tablets, nil, []byte("g"))
	if len(sel) != 1 || !bytes.Equal(sel[0].EndRow, []byte("g")) {
		t.Fatalf("end=g selection wrong: %+v", sel)
	}
	// narrow band inside middle tablet only.
	sel = SelectTablets(tablets, []byte("j"), []byte("k"))
	if len(sel) != 1 || !bytes.Equal(sel[0].EndRow, []byte("p")) {
		t.Fatalf("band j..k selection wrong: %+v", sel)
	}
	// band spanning first two tablets.
	sel = SelectTablets(tablets, []byte("a"), []byte("k"))
	if len(sel) != 2 {
		t.Fatalf("band a..k want 2 tablets, got %d", len(sel))
	}
}

func TestUnportedErrorNamesAllClasses(t *testing.T) {
	err := unportedError("2k", []itercfg.SkippedIter{
		{Name: "one", Class: "com.a.One"},
		{Name: "two", Class: "com.b.Two"},
	})
	s := err.Error()
	for _, want := range []string{"com.a.One", "com.b.Two", "2k"} {
		if !bytes.Contains([]byte(s), []byte(want)) {
			t.Fatalf("error missing %q: %v", want, s)
		}
	}
}
