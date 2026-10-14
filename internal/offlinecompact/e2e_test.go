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

// End-to-end integration of the offline-compaction pipeline, exercising
// the real Run → verify → Commit → apply → "online + full scan" cycle from
// docs/offline-compaction-design.md §8. A live/Mini Accumulo cluster is
// not available in CI (and offline compaction never talks to a tserver
// anyway), so this stands in a faithful in-process model for the pieces a
// cluster would provide: an RFile store (memStore), a tablet enumerator
// and majc-stack resolver (the fakes from offlinecompact_test.go), an
// accumulo.metadata model with a conditional applier (modelMeta /
// modelApplier), and the REAL fence continuity logic driven off a mutable
// table-state (modelFence uses requireOffline + verifyContinuity, the same
// code paths ZKTableFence runs in production).
//
// The oc-e2e follow-up in the design doc calls for a cluster-backed test
// as well; that lives outside the unit suite (it needs a real OFFLINE
// table + Ample applier) and is tracked separately. This test is the
// hermetic pipeline gate that runs on every build.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/shadow/itercfg"
	"github.com/phrocker/shoal-oss/internal/zk"
)

// --- metadata model + conditional applier -----------------------------

// modelMeta models accumulo.metadata for one table: per tablet extent, the
// set of live "file:" refs (keyed by path — memStore/fileEntry use the
// path as the RawQualifier, so the plan's DeleteQualifiers are path bytes).
type modelMeta struct {
	live map[string]map[string]bool // extentKey -> set of live file paths
}

func extentKey(endRow []byte) string {
	if endRow == nil {
		return "<default>"
	}
	return string(endRow)
}

func newModelMeta(tablets []metadata.TabletInfo) *modelMeta {
	m := &modelMeta{live: map[string]map[string]bool{}}
	for _, t := range tablets {
		set := map[string]bool{}
		for _, f := range t.Files {
			set[f.Path] = true
		}
		m.live[extentKey(t.EndRow)] = set
	}
	return m
}

// apply performs one tablet's conditional mutation: every DeleteQualifier
// must currently be live (else the conditional would be rejected), then the
// single Add becomes the tablet's only new ref. It is deliberately strict
// so a mis-projected plan (stale delete, wrong extent) fails loudly.
func (m *modelMeta) apply(tc TabletCommit) error {
	key := extentKey(tc.EndRow)
	set, ok := m.live[key]
	if !ok {
		return fmt.Errorf("apply: unknown extent %q", key)
	}
	for _, dq := range tc.DeleteQualifiers {
		p := string(dq)
		if !set[p] {
			return fmt.Errorf("apply: delete of absent/duplicate file %q in extent %q", p, key)
		}
		delete(set, p)
	}
	if tc.Add.Path == "" {
		return fmt.Errorf("apply: empty add path in extent %q", key)
	}
	set[tc.Add.Path] = true
	return nil
}

// modelApplier is the ModeDirect MetadataCommitter seam over modelMeta.
type modelApplier struct{ meta *modelMeta }

func (a *modelApplier) Commit(_ context.Context, tc TabletCommit) error {
	return a.meta.apply(tc)
}

// --- fence over a mutable table-state ---------------------------------

// modelFence drives the production fence predicates (requireOffline +
// verifyContinuity) off a table-state the test can mutate between Fence and
// Verify to simulate an ONLINE round-trip or a dropped session.
type modelFence struct {
	tableID string
	st      *zk.TableStateResult
	session int64
}

func (f *modelFence) Fence(context.Context) (FenceToken, error) {
	tok, err := requireOffline(*f.st, f.tableID)
	if err != nil {
		return FenceToken{}, err
	}
	tok.Session = f.session
	return tok, nil
}

func (f *modelFence) Verify(_ context.Context, minted FenceToken) error {
	return verifyContinuity(minted, *f.st, f.session)
}

// --- scan model -------------------------------------------------------

// scanTablet reads every live file of one extent through the store and
// merges their cells in sort order — the moral equivalent of onlining the
// table and scanning the tablet.
func scanTablet(t *testing.T, m *memStore, meta *modelMeta, endRow []byte) []kv {
	t.Helper()
	var cells []kv
	for path := range meta.live[extentKey(endRow)] {
		cells = append(cells, drainRFile(t, m.data[path])...)
	}
	sort.SliceStable(cells, func(i, j int) bool {
		// Accumulo key order: row, cf, cq, then timestamp DESCENDING.
		if cells[i].row != cells[j].row {
			return cells[i].row < cells[j].row
		}
		if cells[i].cf != cells[j].cf {
			return cells[i].cf < cells[j].cf
		}
		if cells[i].cq != cells[j].cq {
			return cells[i].cq < cells[j].cq
		}
		return cells[i].ts > cells[j].ts
	})
	return cells
}

func liveCount(meta *modelMeta, endRow []byte) int {
	return len(meta.live[extentKey(endRow)])
}

// versioning1 is a maxVersions=1 majc stack (the canonical default table
// iterator), so compaction collapses duplicate coordinate versions.
func versioning1() []iterrt.IterSpec {
	return []iterrt.IterSpec{
		{Name: iterrt.IterVersioning, Options: map[string]string{iterrt.VersioningOption: "1"}},
	}
}

// twoTabletFixture seeds a two-tablet table: tablet A (extent endRow "g")
// with two files under dir, tablet B (default extent, prevRow "g") with two
// files under dir2. Each has duplicate coordinate versions so a
// maxVersions=1 compaction is observable.
func twoTabletFixture(t *testing.T, m *memStore) []metadata.TabletInfo {
	t.Helper()
	a1 := seed(t, m, "A1.rf", []kv{
		{row: "a", cf: "cf", cq: "q", ts: 10, val: "a-new"},
		{row: "a", cf: "cf", cq: "q", ts: 5, val: "a-old"},
	})
	a2 := seed(t, m, "A2.rf", []kv{{row: "b", cf: "cf", cq: "q", ts: 1, val: "b"}})

	dir2 := "hdfs://nn/accumulo/tables/2k/t-000def/"
	b1 := dir2 + "B1.rf"
	b2 := dir2 + "B2.rf"
	m.data[b1] = buildRFile(t, []kv{{row: "m", cf: "cf", cq: "q", ts: 2, val: "m"}})
	m.data[b2] = buildRFile(t, []kv{
		{row: "n", cf: "cf", cq: "q", ts: 9, val: "n-new"},
		{row: "n", cf: "cf", cq: "q", ts: 3, val: "n-old"},
	})

	return []metadata.TabletInfo{
		{TableID: "2k", EndRow: []byte("g"), Files: []metadata.FileEntry{a1, a2}},
		{TableID: "2k", PrevRow: []byte("g"), Files: []metadata.FileEntry{fileEntry(b1), fileEntry(b2)}},
	}
}

func fixedNames(names ...string) func() string {
	i := 0
	return func() string {
		if i >= len(names) {
			panic(fmt.Sprintf("fixedNames exhausted: Run requested more than the %d output name(s) the test provided; add names to match the fixture", len(names)))
		}
		n := names[i]
		i++
		return n
	}
}

// assertScans checks the post-compaction full-table scan and single-file
// invariant for the twoTabletFixture data.
func assertScans(t *testing.T, m *memStore, meta *modelMeta) {
	t.Helper()
	if c := liveCount(meta, []byte("g")); c != 1 {
		t.Fatalf("tablet A should have exactly one file after compaction, got %d", c)
	}
	if c := liveCount(meta, nil); c != 1 {
		t.Fatalf("tablet B should have exactly one file after compaction, got %d", c)
	}
	gotA := scanTablet(t, m, meta, []byte("g"))
	wantA := []kv{
		{row: "a", cf: "cf", cq: "q", ts: 10, val: "a-new"},
		{row: "b", cf: "cf", cq: "q", ts: 1, val: "b"},
	}
	if fmt.Sprint(gotA) != fmt.Sprint(wantA) {
		t.Fatalf("tablet A scan\n got=%v\nwant=%v", gotA, wantA)
	}
	gotB := scanTablet(t, m, meta, nil)
	wantB := []kv{
		{row: "m", cf: "cf", cq: "q", ts: 2, val: "m"},
		{row: "n", cf: "cf", cq: "q", ts: 9, val: "n-new"},
	}
	if fmt.Sprint(gotB) != fmt.Sprint(wantB) {
		t.Fatalf("tablet B scan\n got=%v\nwant=%v", gotB, wantB)
	}
}

// --- tests ------------------------------------------------------------

// TestE2E_ModeDirect_FullCycle runs the whole pipeline end to end in Mode D:
// fence an OFFLINE table, compact + verify every tablet, apply the metadata
// delta through the committer, then "online" and full-scan to prove every
// original cell survived (post-versioning) and each tablet collapsed to a
// single file.
func TestE2E_ModeDirect_FullCycle(t *testing.T) {
	m := newMemStore()
	tablets := twoTabletFixture(t, m)
	meta := newModelMeta(tablets)

	st := &zk.TableStateResult{State: zk.TableStateOffline, Version: 7, Exists: true}
	fence := &modelFence{tableID: "2k", st: st, session: 42}

	minted, err := fence.Fence(context.Background())
	if err != nil {
		t.Fatalf("fence: %v", err)
	}

	deps := Deps{Tablets: fakeEnum{tablets: tablets}, Stacks: fakeResolver{stack: versioning1()}, Files: m}
	plan, err := Run(context.Background(), "2k", deps, Options{
		Verify:      true,
		NewFileName: fixedNames("Aout-a.rf", "Aout-b.rf"),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(plan.Results) != 2 {
		t.Fatalf("want 2 compacted tablets, got %d", len(plan.Results))
	}
	for _, res := range plan.Results {
		if res.Verify == nil || !res.Verify.OK() {
			t.Fatalf("tablet %q failed verification: %+v", extentKey(res.Tablet.EndRow), res.Verify)
		}
	}

	applier := &modelApplier{meta: meta}
	cp, err := Commit(context.Background(), plan, fence, minted, ModeDirect, false, applier)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if cp.Mode != "direct" || len(cp.Tablets) != 2 {
		t.Fatalf("commit plan mismatch: %+v", cp)
	}

	assertScans(t, m, meta)
}

// TestE2E_ModePlan_ApplierConsumesPlan proves the conservative default path:
// Commit emits a machine-readable CommitPlan (no writes), which is then
// marshaled, unmarshaled, and applied by an out-of-band applier (the Ample
// path in production). The resulting metadata + scan must match Mode D.
func TestE2E_ModePlan_ApplierConsumesPlan(t *testing.T) {
	m := newMemStore()
	tablets := twoTabletFixture(t, m)
	meta := newModelMeta(tablets)

	st := &zk.TableStateResult{State: zk.TableStateOffline, Version: 1, Exists: true}
	fence := &modelFence{tableID: "2k", st: st, session: 1}
	minted, err := fence.Fence(context.Background())
	if err != nil {
		t.Fatalf("fence: %v", err)
	}

	deps := Deps{Tablets: fakeEnum{tablets: tablets}, Stacks: fakeResolver{stack: versioning1()}, Files: m}
	plan, err := Run(context.Background(), "2k", deps, Options{
		Verify:      true,
		NewFileName: fixedNames("Aout-a.rf", "Aout-b.rf"),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Mode P: Commit returns the plan and performs no writes.
	cp, err := Commit(context.Background(), plan, fence, minted, ModePlan, false, nil)
	if err != nil {
		t.Fatalf("Commit(plan): %v", err)
	}
	// The plan survives a JSON round-trip (the operator hand-off format).
	raw, err := MarshalCommitPlan(cp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back CommitPlan
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// The out-of-band applier consumes the plan.
	for _, tc := range back.Tablets {
		if err := meta.apply(tc); err != nil {
			t.Fatalf("apply plan tablet: %v", err)
		}
	}
	assertScans(t, m, meta)
}

// TestE2E_OnlineTableAbortsAtFence covers the negative case where the table
// is left ONLINE: the fence refuses to mint a token, so nothing is compacted
// or committed.
func TestE2E_OnlineTableAbortsAtFence(t *testing.T) {
	st := &zk.TableStateResult{State: "ONLINE", Version: 1, Exists: true}
	fence := &modelFence{tableID: "2k", st: st, session: 1}
	if _, err := fence.Fence(context.Background()); err == nil {
		t.Fatal("fence must refuse an ONLINE table")
	}
}

// TestE2E_OnlineRoundTripTripsCommit covers the race the version-guard is
// for: the table was OFFLINE at fence time but went ONLINE→OFFLINE during
// the (long) compaction, bumping the state-znode version. Commit must trip
// and touch neither metadata nor the store's live set.
func TestE2E_OnlineRoundTripTripsCommit(t *testing.T) {
	m := newMemStore()
	tablets := twoTabletFixture(t, m)
	meta := newModelMeta(tablets)

	st := &zk.TableStateResult{State: zk.TableStateOffline, Version: 3, Exists: true}
	fence := &modelFence{tableID: "2k", st: st, session: 9}
	minted, err := fence.Fence(context.Background())
	if err != nil {
		t.Fatalf("fence: %v", err)
	}

	deps := Deps{Tablets: fakeEnum{tablets: tablets}, Stacks: fakeResolver{stack: versioning1()}, Files: m}
	plan, err := Run(context.Background(), "2k", deps, Options{NewFileName: fixedNames("Aout-a.rf", "Aout-b.rf")})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Simulate an ONLINE round-trip during compaction: same OFFLINE state,
	// but the znode version advanced.
	st.Version = 5

	applier := &modelApplier{meta: meta}
	_, err = Commit(context.Background(), plan, fence, minted, ModeDirect, false, applier)
	if err == nil {
		t.Fatal("Commit must trip on a version change")
	}
	var trip *FenceTrip
	if !errors.As(err, &trip) {
		t.Fatalf("want *FenceTrip, got %T: %v", err, err)
	}
	// Metadata untouched: both tablets still have their original two files.
	if liveCount(meta, []byte("g")) != 2 || liveCount(meta, nil) != 2 {
		t.Fatalf("fence trip must not mutate metadata: A=%d B=%d",
			liveCount(meta, []byte("g")), liveCount(meta, nil))
	}
}

// TestE2E_UnportedIteratorAborts covers a table whose majc stack contains an
// iterator with no Go port: Run aborts (naming the class) before writing any
// output, so there is nothing to commit.
func TestE2E_UnportedIteratorAborts(t *testing.T) {
	m := newMemStore()
	tablets := twoTabletFixture(t, m)

	deps := Deps{
		Tablets: fakeEnum{tablets: tablets},
		Stacks: fakeResolver{skipped: []itercfg.SkippedIter{
			{Name: "secret", Class: "com.example.SecretSquirrelIterator", Priority: 20},
		}},
		Files: m,
	}
	_, err := Run(context.Background(), "2k", deps, Options{})
	if err == nil {
		t.Fatal("Run must abort on an unported iterator")
	}
	if !strings.Contains(err.Error(), "com.example.SecretSquirrelIterator") {
		t.Fatalf("error should name the unported class: %v", err)
	}
	if len(m.writes) != 0 {
		t.Fatalf("no output must be written on abort, wrote %v", m.writes)
	}
}
