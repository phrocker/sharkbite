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
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/storage"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
)

func baseNames(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = filepath.Base(p)
	}
	return out
}

func putFiles(backend *memory.Backend, dir string, names ...string) []string {
	var paths []string
	for _, name := range names {
		path := filepath.Join(dir, name)
		backend.Put(path, []byte("x"))
		paths = append(paths, path)
	}
	return paths
}

// TestImmutableManifestKeepsRecencyOrder: files.json's active list is the
// tablet's recency order and must round-trip unsorted — a name order that
// disagrees with age (an import's content-addressed name, a compaction
// output's "C" prefix) must not reorder it (#692).
func TestImmutableManifestKeepsRecencyOrder(t *testing.T) {
	backend := memory.New()
	dir := "/tablet"
	order := []string{"F0000000000300.rf", ".shoal-import-zz.rf", "C0000000000100-000.rf", ".shoal-import-aa.rf"}
	paths := putFiles(backend, dir, order...)
	if err := persistImmutableManifest(backend, dir, immutableState{active: paths}); err != nil {
		t.Fatal(err)
	}
	found, err := discoverImmutableFiles(backend, dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(baseNames(found.active)) != fmt.Sprint(order) {
		t.Fatalf("discovered %v, want manifest order %v", baseNames(found.active), order)
	}

	// Unknown files adopted by a refresh are newest, after every listed
	// file, in a deterministic (name) order; retired ones stay out.
	putFiles(backend, dir, "B-new.rf", "A-new.rf", "retired.rf")
	if err := persistImmutableManifest(backend, dir, immutableState{active: paths, obsolete: map[string]struct{}{"retired.rf": {}}}); err != nil {
		t.Fatal(err)
	}
	found, err = discoverImmutableFiles(backend, dir, true)
	if err != nil {
		t.Fatal(err)
	}
	want := append(append([]string(nil), order...), "A-new.rf", "B-new.rf")
	if fmt.Sprint(baseNames(found.active)) != fmt.Sprint(want) {
		t.Fatalf("adopted %v, want %v", baseNames(found.active), want)
	}
}

// TestImmutableManifestDuplicateKeepsLastPosition: a manifest written while
// flush names could collide may list one name twice; the object holds the
// later write, so the name is kept once at its last position.
func TestImmutableManifestDuplicateKeepsLastPosition(t *testing.T) {
	backend := memory.New()
	dir := "/tablet"
	putFiles(backend, dir, "F1.rf", "F2.rf")
	backend.Put(filepath.Join(dir, "files.json"),
		[]byte(`{"version":1,"ordered":true,"active":["F2.rf","F1.rf","F2.rf"]}`))
	found, err := discoverImmutableFiles(backend, dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"F1.rf", "F2.rf"}; fmt.Sprint(baseNames(found.active)) != fmt.Sprint(want) {
		t.Fatalf("discovered %v, want %v", baseNames(found.active), want)
	}
}

// TestLegacyManifestOrderRanksImportsNewest: a manifest without the
// "ordered" marker — written before #692, or rewritten by an older binary
// that dropped the marker and re-sorted active — is not trusted. Its
// best-effort order keeps local F/C names in name order and ranks imported
// files after them (an import into a table with its own files is newer).
func TestLegacyManifestOrderRanksImportsNewest(t *testing.T) {
	names := []string{".shoal-import-bb.rf", ".shoal-import-aa.rf", "F0000000000200.rf", "C0000000000100-000.rf"}
	want := []string{"C0000000000100-000.rf", "F0000000000200.rf", ".shoal-import-aa.rf", ".shoal-import-bb.rf"}
	for _, tc := range []struct {
		name     string
		manifest string
	}{
		{"pre-692", `{"version":1,"active":[".shoal-import-aa.rf",".shoal-import-bb.rf","C0000000000100-000.rf","F0000000000200.rf"]}`},
		{"marker-false", `{"version":1,"ordered":false,"active":["F0000000000200.rf",".shoal-import-bb.rf","C0000000000100-000.rf",".shoal-import-aa.rf"]}`},
		{"no-manifest", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := memory.New()
			dir := "/tablet"
			putFiles(backend, dir, names...)
			if tc.manifest != "" {
				backend.Put(filepath.Join(dir, "files.json"), []byte(tc.manifest))
			}
			found, err := discoverImmutableFiles(backend, dir, false)
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(baseNames(found.active)) != fmt.Sprint(want) {
				t.Fatalf("legacy order %v, want %v", baseNames(found.active), want)
			}
		})
	}
}

// TestManifestWritesOrderedMarker: everything this binary persists carries
// the marker, so its order is trusted on the next open.
func TestManifestWritesOrderedMarker(t *testing.T) {
	backend := memory.New()
	dir := "/tablet"
	paths := putFiles(backend, dir, ".shoal-import-zz.rf", "F0000000000500.rf")
	if err := persistImmutableManifest(backend, dir, immutableState{active: paths, obsolete: map[string]struct{}{"F0000000000900.rf": {}}}); err != nil {
		t.Fatal(err)
	}
	data, err := storage.ReadAll(context.Background(), backend, filepath.Join(dir, "files.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest immutableManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if !manifest.Ordered {
		t.Fatalf("manifest not marked ordered: %s", data)
	}
	if manifest.NameHighWater != 900 {
		t.Fatalf("name high water = %d, want 900 (covers obsolete names)", manifest.NameHighWater)
	}
	if err := PublishImmutableFiles(backend, dir, []ImportedFile{{Path: paths[0]}, {Path: paths[1]}}); err != nil {
		t.Fatal(err)
	}
	found, err := discoverImmutableFiles(backend, dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{".shoal-import-zz.rf", "F0000000000500.rf"}; fmt.Sprint(baseNames(found.active)) != fmt.Sprint(want) {
		t.Fatalf("published order %v, want %v", baseNames(found.active), want)
	}
}

func TestUniqueFlushNameAvoidsExistingNames(t *testing.T) {
	existing := map[string]struct{}{
		"F0000000000100.rf": {},
		"F0000000000101.rf": {},
	}
	if got := uniqueFlushName(100, ".rf", existing); got != "F0000000000102.rf" {
		t.Fatalf("flush name = %s", got)
	}
	if got := uniqueFlushName(99, ".rf", existing); got != "F0000000000099.rf" {
		t.Fatalf("flush name = %s", got)
	}
}

// TestFlushNamesNeverReused: a name whose file a compaction deleted (and so
// forgot from the obsolete set) must not be issued again, even when the
// clock is behind it — a name identifies one content for good, and
// incremental sync skips files it already shipped by name.
func TestFlushNamesNeverReused(t *testing.T) {
	future := time.Now().Add(time.Hour).UnixMilli()
	backend := memory.New()
	dir := t.TempDir()
	// A previous generation used a name an hour ahead of the clock and was
	// compacted away: only the manifest's high-water mark remembers it.
	if err := persistImmutableManifest(backend, dir, immutableState{nameHighWater: future}); err != nil {
		t.Fatal(err)
	}
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
	if err := tb.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := tb.Compact(nil); err != nil {
		t.Fatal(err)
	}
	files := tb.RFiles()
	if err := tb.Close(); err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("files = %v", files)
	}
	ms, ok := nameMillis(filepath.Base(files[0]))
	if !ok || ms <= future {
		t.Fatalf("compaction output %s reuses a name at or below the high-water mark %d", files[0], future)
	}
	reopened, err := Open(dir, Options{Backend: backend})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.nameHighWater < ms {
		t.Fatalf("reopened high water %d below issued name %d", reopened.nameHighWater, ms)
	}
}

// withFrozenClock pins the name clock to ms for the test.
func withFrozenClock(t *testing.T, ms int64) {
	t.Helper()
	saved := wallMillis
	wallMillis = func() int64 { return ms }
	t.Cleanup(func() { wallMillis = saved })
}

func writeAndFlush(t *testing.T, tb *Tablet, value string) {
	t.Helper()
	m, err := cclient.NewMutation([]byte("r"))
	if err != nil {
		t.Fatal(err)
	}
	m.PutLatest([]byte("cf"), []byte("cq"), nil, []byte(value))
	if err := tb.Write([]*cclient.Mutation{m}); err != nil {
		t.Fatal(err)
	}
	if err := tb.Flush(); err != nil {
		t.Fatal(err)
	}
}

// TestIssuedNamesStrictlyIncreaseUnderFrozenClock: with the clock stuck,
// every flush and compaction still issues a name above every earlier one —
// both update the in-memory mark, so neither can land on (or below) a name
// the other just used, including names of files a compaction deleted.
func TestIssuedNamesStrictlyIncreaseUnderFrozenClock(t *testing.T) {
	withFrozenClock(t, 1000)
	tb, err := Open(t.TempDir(), Options{Backend: memory.New()})
	if err != nil {
		t.Fatal(err)
	}
	defer tb.Close()
	var issued []string
	record := func() {
		for _, path := range tb.RFiles() {
			name := filepath.Base(path)
			if len(issued) == 0 || issued[len(issued)-1] != name {
				found := false
				for _, prior := range issued {
					found = found || prior == name
				}
				if !found {
					issued = append(issued, name)
				}
			}
		}
	}
	for i := 0; i < 3; i++ {
		writeAndFlush(t, tb, fmt.Sprintf("v%d", i))
		record()
		writeAndFlush(t, tb, fmt.Sprintf("w%d", i))
		record()
		if err := tb.Compact(nil); err != nil {
			t.Fatal(err)
		}
		record()
	}
	if len(issued) != 9 {
		t.Fatalf("issued %v, want 9 distinct names (two flushes and a compaction, three times)", issued)
	}
	var last int64 = -1
	for _, name := range issued {
		ms, ok := nameMillis(name)
		if !ok || ms <= last {
			t.Fatalf("names not strictly increasing: %v", issued)
		}
		last = ms
	}
}

// TestClockStepBackAfterCompactionDoesNotReissue: two flushes at 1000 (F1000,
// F1001), clock steps back to 500, compaction deletes both. The persisted
// mark must stay at or above 1001, so after a reopen neither name is reissued.
func TestClockStepBackAfterCompactionDoesNotReissue(t *testing.T) {
	backend := memory.New()
	dir := t.TempDir()
	withFrozenClock(t, 1000)
	tb, err := Open(dir, Options{Backend: backend})
	if err != nil {
		t.Fatal(err)
	}
	writeAndFlush(t, tb, "a")
	writeAndFlush(t, tb, "b")
	wallMillis = func() int64 { return 500 }
	if err := tb.Compact(nil); err != nil {
		t.Fatal(err)
	}
	if err := tb.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, Options{Backend: backend})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.nameHighWater < 1001 {
		t.Fatalf("persisted high water %d dropped below the deleted F1000/F1001", reopened.nameHighWater)
	}
	writeAndFlush(t, reopened, "b")
	for _, path := range reopened.RFiles() {
		if ms, _ := nameMillis(filepath.Base(path)); ms <= 1001 {
			t.Fatalf("issued %s at or below a name already used", path)
		}
	}
}

// TestRefreshKeepsHigherInMemoryMark: a refresh must not lower the tablet's
// mark to what files.json says (the in-memory mark can be ahead, e.g. after
// a failed manifest write).
func TestRefreshKeepsHigherInMemoryMark(t *testing.T) {
	backend := memory.New()
	dir := t.TempDir()
	tb, err := Open(dir, Options{Backend: backend})
	if err != nil {
		t.Fatal(err)
	}
	defer tb.Close()
	tb.nameHighWater = 7000
	if _, err := tb.RefreshFiles(); err != nil {
		t.Fatal(err)
	}
	if tb.nameHighWater != 7000 {
		t.Fatalf("refresh lowered the mark to %d", tb.nameHighWater)
	}
	found, err := discoverImmutableFiles(backend, dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if found.nameHighWater < 7000 {
		t.Fatalf("refresh persisted mark %d, want >= 7000", found.nameHighWater)
	}
}

// TestDiscoveryCountsStrayObjects: an object the manifest does not list (a
// flush whose manifest write never landed) still holds its name.
func TestDiscoveryCountsStrayObjects(t *testing.T) {
	backend := memory.New()
	dir := "/tablet"
	paths := putFiles(backend, dir, "F0000000000100.rf")
	if err := persistImmutableManifest(backend, dir, immutableState{active: paths}); err != nil {
		t.Fatal(err)
	}
	putFiles(backend, dir, "F0000000009000.rf")
	found, err := discoverImmutableFiles(backend, dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(found.active) != 1 {
		t.Fatalf("stray adopted: %v", found.active)
	}
	if found.nameHighWater < 9000 {
		t.Fatalf("high water %d ignores the stray F9000", found.nameHighWater)
	}
}

// TestPublishKeepsPriorMarkAndObjects: publishing an import generation over
// a directory keeps the higher of an earlier files.json mark and any object
// present.
func TestPublishKeepsPriorMarkAndObjects(t *testing.T) {
	backend := memory.New()
	dir := "/tablet"
	backend.Put(filepath.Join(dir, "files.json"),
		[]byte(`{"version":1,"ordered":true,"name_high_water":9000,"active":[]}`))
	putFiles(backend, dir, "F0000000004000.rf")
	imported := putFiles(backend, dir, ".shoal-import-aa.rf")
	if err := PublishImmutableFiles(backend, dir, []ImportedFile{{Path: imported[0], Source: "s1"}}); err != nil {
		t.Fatal(err)
	}
	found, err := discoverImmutableFiles(backend, dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if found.nameHighWater < 9000 {
		t.Fatalf("publish dropped the prior mark: %d", found.nameHighWater)
	}
	if found.sources[".shoal-import-aa.rf"] != "s1" {
		t.Fatalf("publish did not record the source: %v", found.sources)
	}

	backend2 := memory.New()
	putFiles(backend2, dir, "F0000000004000.rf")
	if err := PublishImmutableFiles(backend2, dir, nil); err != nil {
		t.Fatal(err)
	}
	found, err = discoverImmutableFiles(backend2, dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if found.nameHighWater < 4000 {
		t.Fatalf("publish ignored a present object: %d", found.nameHighWater)
	}
}

// importPath builds a staged import object name with content digest c.
func importPath(c byte, nonce string) string {
	digest := make([]byte, 64)
	for i := range digest {
		digest[i] = c
	}
	return filepath.Join("/tablet", ".shoal-import-"+string(digest)+"-"+nonce+".rf")
}

// TestRegisterByProducerIdentity: a held producer file never moves and is
// never re-added; an unknown one is appended newest. A producer that toggles
// on, off, on yields three producer files (the third byte-identical to the
// first), all kept in order; re-registering that manifest after a local
// flush changes nothing; growth appends only the new file.
func TestRegisterByProducerIdentity(t *testing.T) {
	backend := memory.New()
	dir := "/tablet"
	tb := &Tablet{dir: dir, backend: backend, obsolete: map[string]struct{}{}}
	register := func(files ...ImportedFile) {
		t.Helper()
		for _, f := range files {
			putFiles(backend, dir, filepath.Base(f.Path))
		}
		if _, err := tb.RegisterImmutableFiles(files); err != nil {
			t.Fatal(err)
		}
	}
	on, off, onAgain := importPath('a', "1"), importPath('b', "1"), importPath('a', "2")
	register(ImportedFile{on, "p/F1"}, ImportedFile{off, "p/F2"}, ImportedFile{onAgain, "p/F3"})
	if want := []string{on, off, onAgain}; fmt.Sprint(tb.files) != fmt.Sprint(want) {
		t.Fatalf("toggle registered %v, want %v", baseNames(tb.files), baseNames(want))
	}
	local := filepath.Join(dir, "F0000000000100.rf")
	tb.files = append(tb.files, local)

	again := []ImportedFile{{importPath('a', "3"), "p/F1"}, {importPath('b', "3"), "p/F2"}, {importPath('a', "4"), "p/F3"}}
	register(again...)
	if want := []string{on, off, onAgain, local}; fmt.Sprint(tb.files) != fmt.Sprint(want) {
		t.Fatalf("unchanged re-register moved files: %v, want %v", baseNames(tb.files), baseNames(want))
	}
	for _, f := range again {
		if _, err := backend.Open(context.Background(), f.Path); err == nil {
			t.Fatalf("redundant incoming object %s not removed", f.Path)
		}
	}

	grown := importPath('c', "5")
	register(append(again[:0:0], ImportedFile{importPath('a', "6"), "p/F1"}, ImportedFile{importPath('b', "6"), "p/F2"},
		ImportedFile{importPath('a', "7"), "p/F3"}, ImportedFile{grown, "p/F4"})...)
	if want := []string{on, off, onAgain, local, grown}; fmt.Sprint(tb.files) != fmt.Sprint(want) {
		t.Fatalf("growth %v, want %v", baseNames(tb.files), baseNames(want))
	}
}

// TestRegisterClaimsLegacyContentOnce: files imported before producer
// identities were recorded carry none and are matched by content — but each
// legacy file is claimed by the FIRST incoming file with its content only.
// A later byte-identical producer file (on, off, on) is a different file and
// is appended newest; a re-register after that is a no-op.
func TestRegisterClaimsLegacyContentOnce(t *testing.T) {
	backend := memory.New()
	dir := "/tablet"
	legacyOn, legacyOff := importPath('a', "1"), importPath('b', "1")
	local := filepath.Join(dir, "F0000000000100.rf")
	putFiles(backend, dir, filepath.Base(legacyOn), filepath.Base(legacyOff), filepath.Base(local))
	tb := &Tablet{dir: dir, backend: backend, obsolete: map[string]struct{}{},
		files: []string{legacyOn, legacyOff, local}}
	manifest := func(nonce string) []ImportedFile {
		files := []ImportedFile{
			{importPath('a', nonce+"a"), "p/F1"},
			{importPath('b', nonce+"b"), "p/F2"},
			{importPath('a', nonce+"c"), "p/F3"},
		}
		for _, f := range files {
			putFiles(backend, dir, filepath.Base(f.Path))
		}
		return files
	}
	first := manifest("x")
	if _, err := tb.RegisterImmutableFiles(first); err != nil {
		t.Fatal(err)
	}
	if want := []string{legacyOn, legacyOff, local, first[2].Path}; fmt.Sprint(tb.files) != fmt.Sprint(want) {
		t.Fatalf("legacy claim %v, want %v", baseNames(tb.files), baseNames(want))
	}
	if tb.sources[filepath.Base(legacyOn)] != "p/F1" || tb.sources[filepath.Base(legacyOff)] != "p/F2" {
		t.Fatalf("legacy files not claimed: %v", tb.sources)
	}
	if _, err := tb.RegisterImmutableFiles(manifest("y")); err != nil {
		t.Fatal(err)
	}
	if want := []string{legacyOn, legacyOff, local, first[2].Path}; fmt.Sprint(tb.files) != fmt.Sprint(want) {
		t.Fatalf("re-register after claim moved files: %v", baseNames(tb.files))
	}

	// Sources survive a manifest round trip.
	found, err := discoverImmutableFiles(backend, dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if found.sources[filepath.Base(first[2].Path)] != "p/F3" || found.sources[filepath.Base(legacyOn)] != "p/F1" {
		t.Fatalf("persisted sources %v", found.sources)
	}
}

// TestRegisterAbsorbedLegacyDigestClaimedOnce: a legacy import that a local
// compaction merged leaves its content digest in the absorbed set. The first
// incoming file with that content claims it (no-op, nothing appended); a
// later byte-identical producer file is a different file and is appended.
func TestRegisterAbsorbedLegacyDigestClaimedOnce(t *testing.T) {
	backend := memory.New()
	dir := "/tablet"
	legacy := importPath('a', "0")
	digest, ok := legacyContentIdentity(legacy)
	if !ok {
		t.Fatal("import path has no content identity")
	}
	if _, ok := legacyContentIdentity(filepath.Join(dir, "F0000000000100.rf")); ok {
		t.Fatal("local file reported a content identity")
	}
	compacted := filepath.Join(dir, "C0000000000200-000.rf")
	tb := &Tablet{dir: dir, backend: backend, obsolete: map[string]struct{}{},
		files: []string{compacted}, absorbed: map[string]struct{}{digest: {}}}
	register := func(f ImportedFile) {
		t.Helper()
		putFiles(backend, dir, filepath.Base(f.Path))
		if _, err := tb.RegisterImmutableFiles([]ImportedFile{f}); err != nil {
			t.Fatal(err)
		}
	}
	register(ImportedFile{importPath('a', "1"), "p/F1"})
	if want := []string{compacted}; fmt.Sprint(tb.files) != fmt.Sprint(want) {
		t.Fatalf("absorbed claim appended: %v", baseNames(tb.files))
	}
	if _, ok := tb.absorbed["p/F1"]; !ok {
		t.Fatalf("claim not recorded: %v", tb.absorbed)
	}
	later := importPath('a', "2")
	register(ImportedFile{later, "p/F3"})
	if want := []string{compacted, later}; fmt.Sprint(tb.files) != fmt.Sprint(want) {
		t.Fatalf("later identical file %v, want appended %v", baseNames(tb.files), baseNames(want))
	}
}
