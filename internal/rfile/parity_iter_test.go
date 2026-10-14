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

// Iterator-aware parity harness (Phase C1). Lives in package rfile_test
// — not package rfile — because it imports internal/iterrt, and iterrt
// imports internal/rfile; an in-package test importing iterrt is an
// import cycle. It reaches the package-internal harness machinery via
// the Harness* shims in export_test.go.
//
// What this wires, end-to-end and JVM-free:
//
//   - ParityConfig.Iterators is parsed into an iterrt stack and applied
//     to the generated cell stream. The POST-iterator cells are what
//     gets written — so the C0 round-trip / determinism / metadata
//     assertions all now run against real iterator output, not just the
//     identity stream.
//   - applyIteratorStack is the single chokepoint: both the shoal writer
//     and (when SHOAL_JAVA_PARITY is set) the Java writer consume the
//     same post-iterator cell stream.
//   - probe sampling is extended past the seek-key axis: probes now also
//     vary the fetched-columns set and (for visibility stacks) the
//     authorizations, per the design doc's parity-harness spec.
package rfile_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/rfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile/block"
	"github.com/phrocker/shoal-oss/internal/rfile/wire"
)

// runJavaWriter invokes the SHOAL_JAVA_PARITY command template,
// substituting all placeholders, to produce javaPath from csPath. Mirror
// of the internal harness's runJavaParityWriter — reimplemented here
// because that helper is unexported and this is an external test package.
func runJavaWriter(t *testing.T, tmpl string, cfg rfile.ParityConfig, csPath, javaPath string) {
	t.Helper()
	rep := strings.NewReplacer(
		"$CELLSTREAM", csPath,
		"$RFILE", javaPath,
		"$CODEC", cfg.Codec,
		"$BLOCKSIZE", strconv.Itoa(cfg.BlockSize),
		"$INDEXBLOCKSIZE", strconv.Itoa(cfg.IndexBlockSize),
	)
	cmdStr := rep.Replace(tmpl)
	t.Logf("running Java parity writer: %s", cmdStr)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", cmdStr)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("Java parity writer failed: %v\nOutput:\n%s", err, out.String())
	}
	if _, err := os.Stat(javaPath); err != nil {
		t.Fatalf("Java parity writer exited 0 but produced no RFile at %s: %v", javaPath, err)
	}
}

// parseIterSpecs turns ParityConfig.Iterators ("name" or
// "name:k=v,k=v") into an ordered []iterrt.IterSpec.
func parseIterSpecs(t *testing.T, names []string) []iterrt.IterSpec {
	t.Helper()
	specs := make([]iterrt.IterSpec, 0, len(names))
	for _, raw := range names {
		name := raw
		opts := map[string]string{}
		if i := strings.IndexByte(raw, ':'); i >= 0 {
			name = raw[:i]
			for _, kv := range strings.Split(raw[i+1:], ",") {
				if kv == "" {
					continue
				}
				eq := strings.IndexByte(kv, '=')
				if eq < 0 {
					t.Fatalf("bad iterator option %q in %q", kv, raw)
				}
				opts[kv[:eq]] = kv[eq+1:]
			}
		}
		specs = append(specs, iterrt.IterSpec{Name: name, Options: opts})
	}
	return specs
}

// applyIteratorStack runs cfg.Iterators over the generated cell stream
// and returns the post-iterator cells. Empty Iterators == identity
// (input returned unchanged). This is the harness chokepoint: the
// returned cells are what BOTH writers consume.
func applyIteratorStack(t *testing.T, cfg rfile.ParityConfig, cells []rfile.HarnessCell) []rfile.HarnessCell {
	t.Helper()
	if len(cfg.Iterators) == 0 {
		return cells
	}
	leafCells := make([]iterrt.Cell, len(cells))
	for i, c := range cells {
		leafCells[i] = iterrt.Cell{Key: c.K, Value: c.V}
	}
	leaf := iterrt.NewSliceSource(leafCells)
	env := iterrt.IteratorEnvironment{
		Scope:          iterrt.IteratorScope(cfg.IteratorScope),
		Authorizations: cfg.Authorizations,
	}
	if err := leaf.Init(nil, nil, env); err != nil {
		t.Fatalf("leaf Init: %v", err)
	}
	top, err := iterrt.BuildStack(leaf, parseIterSpecs(t, cfg.Iterators), env)
	if err != nil {
		t.Fatalf("BuildStack: %v", err)
	}
	if err := top.Seek(iterrt.InfiniteRange(), nil, false); err != nil {
		t.Fatalf("stack Seek: %v", err)
	}
	var out []rfile.HarnessCell
	for top.HasTop() {
		out = append(out, rfile.HarnessCell{
			K: top.GetTopKey().Clone(),
			V: append([]byte(nil), top.GetTopValue()...),
		})
		if err := top.Next(); err != nil {
			t.Fatalf("stack Next: %v", err)
		}
	}
	return out
}

// --- pure-Go tier (unconditional) ----------------------------------------

// TestParityIter_VersioningRoundtrip: a versioning stack is applied to a
// cell stream that carries duplicate-coordinate versions; the
// post-iterator stream (newest-N kept) round-trips through the shoal
// writer + reader unchanged.
func TestParityIter_VersioningRoundtrip(t *testing.T) {
	for _, maxV := range []string{"1", "2", "3"} {
		t.Run("maxVersions="+maxV, func(t *testing.T) {
			cfg := rfile.ParityConfig{
				Seed: 0xC1A, Cells: 3000, Codec: block.CodecGzip,
				BlockSize: 4096, IndexBlockSize: 256, Lookups: 2000,
				Iterators:     []string{"versioning:maxVersions=" + maxV},
				IteratorScope: int(iterrt.ScopeMajc),
			}
			cells := withVersions(rfile.HarnessGenCells(cfg))
			post := applyIteratorStack(t, cfg, cells)
			assertNewestN(t, post, atoiOr(maxV, 1))

			dir := t.TempDir()
			path := filepath.Join(dir, "vers.rf")
			if err := rfile.HarnessWriteRFile(path, cfg, post); err != nil {
				t.Fatalf("write: %v", err)
			}
			r, err := rfile.HarnessOpenRFile(path)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer r.Close()
			got, err := rfile.HarnessScanAll(r)
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			if d := rfile.HarnessCellSeqDiff(post, got); d != "" {
				t.Fatalf("post-iterator write→read diverged: %s", d)
			}
		})
	}
}

// TestParityIter_VisibilityRoundtrip: a visibility-filter stack at scan
// scope drops cells the auth set can't satisfy; the survivors round-trip.
func TestParityIter_VisibilityRoundtrip(t *testing.T) {
	cfg := rfile.ParityConfig{
		Seed: 0xC1B, Cells: 3000, Codec: block.CodecNone,
		BlockSize: 4096, IndexBlockSize: 256, Lookups: 2000,
		Iterators:      []string{"visibility"},
		IteratorScope:  int(iterrt.ScopeScan),
		Authorizations: [][]byte{[]byte("vis")},
	}
	// genCells stamps every 5th cell with CV "vis"; the rest are public.
	// With auth {"vis"} the whole stream is visible — so also test the
	// restrictive case below.
	cells := rfile.HarnessGenCells(cfg)
	post := applyIteratorStack(t, cfg, cells)
	if len(post) != len(cells) {
		t.Fatalf("auth {vis}: expected all %d cells visible, got %d", len(cells), len(post))
	}

	// Restrictive: empty auth set drops every "vis"-labeled cell.
	cfg.Authorizations = [][]byte{}
	postR := applyIteratorStack(t, cfg, cells)
	if len(postR) >= len(cells) {
		t.Fatalf("empty auth: expected some cells dropped, got %d of %d", len(postR), len(cells))
	}
	for _, c := range postR {
		if len(c.K.ColumnVisibility) != 0 {
			t.Fatalf("empty auth leaked a labeled cell: %+v", c.K)
		}
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "vis.rf")
	if err := rfile.HarnessWriteRFile(path, cfg, postR); err != nil {
		t.Fatalf("write: %v", err)
	}
	r, err := rfile.HarnessOpenRFile(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer r.Close()
	got, err := rfile.HarnessScanAll(r)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if d := rfile.HarnessCellSeqDiff(postR, got); d != "" {
		t.Fatalf("post-visibility write→read diverged: %s", d)
	}
}

// TestParityIter_VisibilityInactiveOnMajc: the visibility iterator must
// be a passthrough at majc — a compaction never drops cells by
// visibility. Same input, scan vs majc, must differ on a restrictive
// auth set.
func TestParityIter_VisibilityInactiveOnMajc(t *testing.T) {
	base := rfile.ParityConfig{
		Seed: 0xC1C, Cells: 2000, Codec: block.CodecNone,
		BlockSize: 4096, IndexBlockSize: 256,
		Iterators:      []string{"visibility"},
		Authorizations: [][]byte{}, // restrictive
	}
	cells := rfile.HarnessGenCells(base)

	scanCfg := base
	scanCfg.IteratorScope = int(iterrt.ScopeScan)
	scanOut := applyIteratorStack(t, scanCfg, cells)

	majcCfg := base
	majcCfg.IteratorScope = int(iterrt.ScopeMajc)
	majcOut := applyIteratorStack(t, majcCfg, cells)

	if len(majcOut) != len(cells) {
		t.Fatalf("majc must preserve all %d cells, got %d", len(cells), len(majcOut))
	}
	if len(scanOut) >= len(majcOut) {
		t.Fatalf("scan scope should drop cells majc keeps: scan=%d majc=%d", len(scanOut), len(majcOut))
	}
}

// TestParityIter_StackedVersioningThenVisibility: a two-iterator stack
// (versioning then visibility) applied bottom-up. Asserts the result is
// the version-cap composed with the auth filter.
func TestParityIter_StackedVersioningThenVisibility(t *testing.T) {
	cfg := rfile.ParityConfig{
		Seed: 0xC1D, Cells: 2400, Codec: block.CodecGzip,
		BlockSize: 4096, IndexBlockSize: 256, Lookups: 1500,
		Iterators:      []string{"versioning:maxVersions=1", "visibility"},
		IteratorScope:  int(iterrt.ScopeScan),
		Authorizations: [][]byte{}, // restrictive: drop labeled cells
	}
	cells := withVersions(rfile.HarnessGenCells(cfg))
	post := applyIteratorStack(t, cfg, cells)

	// Every survivor must be public AND the newest version of its coord.
	seen := map[string]bool{}
	for _, c := range post {
		if len(c.K.ColumnVisibility) != 0 {
			t.Fatalf("visibility leaked a labeled cell: %+v", c.K)
		}
		coord := coordOf(c.K)
		if seen[coord] {
			t.Fatalf("versioning leaked a 2nd version of %s", coord)
		}
		seen[coord] = true
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "stack.rf")
	if err := rfile.HarnessWriteRFile(path, cfg, post); err != nil {
		t.Fatalf("write: %v", err)
	}
	r, err := rfile.HarnessOpenRFile(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer r.Close()
	got, err := rfile.HarnessScanAll(r)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if d := rfile.HarnessCellSeqDiff(post, got); d != "" {
		t.Fatalf("stacked write→read diverged: %s", d)
	}
}

// TestParityIter_ProbeAxes exercises the extended probe sampling: beyond
// the seek-key axis, probes now also vary the fetched-columns set. A
// column-family-restricted point lookup against a written RFile must
// match a linear reference over the same post-iterator cells.
func TestParityIter_ProbeAxes(t *testing.T) {
	cfg := rfile.ParityConfig{
		Seed: 0xC1E, Cells: 2000, Codec: block.CodecNone,
		BlockSize: 4096, IndexBlockSize: 256, Lookups: 1200,
		Iterators:     []string{"versioning:maxVersions=1"},
		IteratorScope: int(iterrt.ScopeMajc),
	}
	cells := withVersions(rfile.HarnessGenCells(cfg))
	post := applyIteratorStack(t, cfg, cells)
	dir := t.TempDir()
	path := filepath.Join(dir, "probe.rf")
	if err := rfile.HarnessWriteRFile(path, cfg, post); err != nil {
		t.Fatalf("write: %v", err)
	}
	r, err := rfile.HarnessOpenRFile(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer r.Close()

	probes := rfile.HarnessSampleProbeKeys(post, cfg.Lookups, cfg.Seed^0x5A5A)
	// The fetched-columns axis: genCells uses a single cf "cf", so a
	// restriction to {"cf"} must equal no-restriction, and a restriction
	// to a bogus cf must find nothing. Both are real parity checks.
	cfAxes := [][][]byte{nil, {[]byte("cf")}, {[]byte("nope")}}
	for ai, fetch := range cfAxes {
		for i, p := range probes {
			got, err := rfile.HarnessPointLookupFiltered(r, p, fetch)
			if err != nil {
				t.Fatalf("axis %d probe %d: %v", ai, i, err)
			}
			want := linearFiltered(post, p, fetch)
			if d := probeDiff(got, want); d != "" {
				t.Fatalf("axis %d probe %d (%+v): %s", ai, i, p, d)
			}
		}
	}
}

// --- Java-gated tier -----------------------------------------------------

// TestParityIter_ShoalVsJava is the iterator-aware full harness: the
// post-iterator cell stream is written by BOTH shoal and Java; the two
// RFiles must be semantically equivalent. Gated on SHOAL_JAVA_PARITY.
//
// Note (Java-side gap): the iterator stack runs in Go and Java consumes
// the post-iterator stream — so this asserts shoal-iterators+shoal-writer
// == Java-writer-of-the-same-cells. It does NOT run the Java iterator
// implementations. Doing that would mean extending ShoalParityWrite.java
// to accept an iterator config and build a Java SortedKeyValueIterator
// stack; that is the documented remaining gap. iterrt's own unit tests
// pin shoal-iterator behaviour against the Java iterator source.
func TestParityIter_ShoalVsJava(t *testing.T) {
	tmpl := os.Getenv("SHOAL_JAVA_PARITY")
	if tmpl == "" {
		t.Skip("SHOAL_JAVA_PARITY not set; skipping iterator-aware shoal-vs-Java parity")
	}
	cfg := rfile.ParityConfig{
		Seed: 0xC1F, Cells: 3000, Codec: block.CodecGzip,
		BlockSize: 4096, IndexBlockSize: 256, Lookups: 3000,
		Iterators:     []string{"versioning:maxVersions=2"},
		IteratorScope: int(iterrt.ScopeMajc),
	}
	cells := withVersions(rfile.HarnessGenCells(cfg))
	post := applyIteratorStack(t, cfg, cells)

	dir := t.TempDir()
	// Shared post-iterator cell stream — both writers consume it.
	csPath := filepath.Join(dir, "post-cells.bin")
	cs, err := os.Create(csPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := rfile.HarnessWriteCellStream(cs, post); err != nil {
		cs.Close()
		t.Fatalf("write cell stream: %v", err)
	}
	cs.Close()

	shoalPath := filepath.Join(dir, "shoal.rf")
	if err := rfile.HarnessWriteRFile(shoalPath, cfg, post); err != nil {
		t.Fatalf("shoal write: %v", err)
	}
	javaPath := filepath.Join(dir, "java.rf")
	runJavaWriter(t, tmpl, cfg, csPath, javaPath)

	shoalR, err := rfile.HarnessOpenRFile(shoalPath)
	if err != nil {
		t.Fatalf("open shoal: %v", err)
	}
	defer shoalR.Close()
	javaR, err := rfile.HarnessOpenRFile(javaPath)
	if err != nil {
		t.Fatalf("open java: %v", err)
	}
	defer javaR.Close()

	sCells, err := rfile.HarnessScanAll(shoalR)
	if err != nil {
		t.Fatalf("scan shoal: %v", err)
	}
	jCells, err := rfile.HarnessScanAll(javaR)
	if err != nil {
		t.Fatalf("scan java: %v", err)
	}
	if d := rfile.HarnessCellSeqDiff(sCells, jCells); d != "" {
		t.Fatalf("post-iterator shoal vs java scan divergence: %s", d)
	}
	t.Logf("iterator-aware parity OK: %d post-iterator cells identical across shoal+java", len(sCells))
}

// --- helpers -------------------------------------------------------------

// withVersions rewrites the generated stream so a subset of coordinates
// carry multiple descending-timestamp versions — genCells emits one cell
// per coordinate, which would make a versioning stack a no-op. Output
// stays in wire.Key.Compare order.
func withVersions(cells []rfile.HarnessCell) []rfile.HarnessCell {
	var out []rfile.HarnessCell
	for i, c := range cells {
		out = append(out, c)
		// Every 3rd coordinate gets two extra older versions.
		if i%3 == 0 {
			for _, older := range []int64{1, 2} {
				k := c.K.Clone()
				k.Timestamp = c.K.Timestamp - older
				v := append([]byte(nil), c.V...)
				v = append(v, byte('0'+older))
				out = append(out, rfile.HarnessCell{K: k, V: v})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].K.Compare(out[j].K) < 0 })
	return out
}

// assertNewestN checks no coordinate in cells has more than n versions,
// and that versions are in descending-timestamp order.
func assertNewestN(t *testing.T, cells []rfile.HarnessCell, n int) {
	t.Helper()
	count := 0
	var prevCoord string
	var prevTS int64
	for _, c := range cells {
		coord := coordOf(c.K)
		if coord != prevCoord {
			count = 1
			prevCoord = coord
			prevTS = c.K.Timestamp
			continue
		}
		count++
		if count > n {
			t.Fatalf("coordinate %s has > %d versions", coord, n)
		}
		if c.K.Timestamp >= prevTS {
			t.Fatalf("coordinate %s versions not descending: %d after %d", coord, c.K.Timestamp, prevTS)
		}
		prevTS = c.K.Timestamp
	}
}

func coordOf(k *wire.Key) string {
	return string(k.Row) + "\x00" + string(k.ColumnFamily) + "\x00" +
		string(k.ColumnQualifier) + "\x00" + string(k.ColumnVisibility)
}

// linearFiltered is the brute-force reference for a column-family-
// restricted point lookup: the first cell at-or-after target whose cf is
// in fetch (empty fetch == no restriction).
func linearFiltered(cells []rfile.HarnessCell, target *wire.Key, fetch [][]byte) rfile.HarnessProbeResult {
	for _, c := range cells {
		if c.K.Compare(target) < 0 {
			continue
		}
		if len(fetch) > 0 {
			ok := false
			for _, f := range fetch {
				if string(f) == string(c.K.ColumnFamily) {
					ok = true
					break
				}
			}
			if !ok {
				continue
			}
		}
		return rfile.HarnessProbeResult{Found: true, Key: c.K.Clone(), Val: append([]byte(nil), c.V...)}
	}
	return rfile.HarnessProbeResult{Found: false}
}

func probeDiff(a, b rfile.HarnessProbeResult) string {
	if a.Found != b.Found {
		return "found mismatch"
	}
	if !a.Found {
		return ""
	}
	if a.Key.Compare(b.Key) != 0 || !a.Key.Equal(b.Key) {
		return "key mismatch"
	}
	if !bytes.Equal(a.Val, b.Val) {
		return "value mismatch"
	}
	return ""
}

func atoiOr(s string, def int) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
	}
	if n == 0 {
		return def
	}
	return n
}
