/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements. See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership. The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License. You may obtain a copy of the License at
 *
 *     https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package authorized

// The durable revision binding (#695): what is written, in what order, how
// strictly it is read back, and what a binary from before it does with it.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// rawPolicyCell returns the newest record cell stored at row.
func rawPolicyCell(t *testing.T, dir string, row []byte) ([]byte, bool) {
	t.Helper()
	eng, err := engine.Open(dir, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	scanner, err := eng.Scan(policyTable, iterrt.InfiniteRange(), engine.ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer scanner.Close()
	var value []byte
	found := false
	for scanner.Next() {
		if bytes.Equal(scanner.Key().Row, row) && !found {
			value = append([]byte(nil), scanner.Value()...)
			found = true
		}
		if err := scanner.Advance(); err != nil {
			t.Fatal(err)
		}
	}
	return value, found
}

func openDurableForTest(t *testing.T, dir string) *DurablePolicyStore {
	t.Helper()
	store, err := OpenDurablePolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func closeDurableForTest(t *testing.T, store *DurablePolicyStore) {
	t.Helper()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

var bindingTestRevisions = []RevisionReference{
	{DocumentID: "document", RevisionID: "revision"},
}

// requireOldBinaryRefuses checks the format floor the way every durable
// binary before #695 reads it: as a row under the source-claim prefix, which
// such a binary decodes strictly as a source claim, so it refuses to open.
func requireOldBinaryRefuses(t *testing.T, dir string) {
	t.Helper()
	value, ok := rawPolicyCell(t, dir, []byte(policyRowFormatFloor))
	if !ok {
		t.Fatal("the catalog holds a revision binding but no format floor")
	}
	if !strings.HasPrefix(policyRowFormatFloor, policyRowSourceClaim) {
		t.Fatal("the format floor is not under the source-claim prefix")
	}
	var claim persistedSourceClaim
	err := decodePolicyRecord(value, policyKindSourceClaim, &claim)
	if err == nil || !strings.Contains(err.Error(), "kind") {
		t.Fatalf("an old binary would decode the floor as a source claim: %v", err)
	}
	var floor persistedFormatFloor
	if err := decodePolicyRecord(value, policyKindFormatFloor, &floor); err != nil ||
		floor.Floor != formatFloorRevisionBindings {
		t.Fatalf("floor = %+v, %v", floor, err)
	}
}

// preFloorCatalog is a copy of a catalog written before the format floor
// existed (the #667 lineage fixture), with the lock file a workspace of this
// version leaves, so an offline listing may open it.
func preFloorCatalog(t *testing.T) string {
	t.Helper()
	dir := copyLineageFixture(t)
	if err := os.WriteFile(filepath.Join(dir, policyLockFile), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := rawPolicyCell(t, dir, []byte(policyRowFormatFloor)); ok {
		t.Fatal("the pre-floor fixture already holds a format floor")
	}
	return dir
}

// TestFormatFloorIsWrittenAtReadWriteOpen: the first read-write open by this
// binary writes the floor, so an older binary refuses the catalog from then
// on whether or not anything was bound yet. An offline listing writes
// nothing, the floor included.
func TestFormatFloorIsWrittenAtReadWriteOpen(t *testing.T) {
	dir := preFloorCatalog(t)
	before := catalogSnapshot(t, dir)
	listing, err := OpenExistingDurablePolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	closeDurableForTest(t, listing)
	requireUnchanged(t, dir, before)
	if _, ok := rawPolicyCell(t, dir, []byte(policyRowFormatFloor)); ok {
		t.Fatal("an offline listing wrote the format floor")
	}

	store := openDurableForTest(t, dir)
	closeDurableForTest(t, store)
	requireOldBinaryRefuses(t, dir)
	// Reopening keeps the one floor and still opens.
	closeDurableForTest(t, openDurableForTest(t, dir))
}

// TestFormatFloorPrecedesABindingWithoutAReadWriteOpen: a store opened
// without writing the floor still writes it before its first binding.
func TestFormatFloorPrecedesABindingWithoutAReadWriteOpen(t *testing.T) {
	dir := preFloorCatalog(t)
	store, err := OpenExistingDurablePolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutInteractionRevisions(
		context.Background(), "session", bindingTestRevisions); err != nil {
		t.Fatal(err)
	}
	closeDurableForTest(t, store)
	requireOldBinaryRefuses(t, dir)
}

// TestInteractionBindingsRoundTrip: bindings are persisted as their union
// and survive a reopen.
func TestInteractionBindingsRoundTrip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := openDurableForTest(t, dir)
	seedDurableStore(t, store)
	if err := store.PutInteractionRevisions(
		ctx, "session", bindingTestRevisions); err != nil {
		t.Fatal(err)
	}
	more := []RevisionReference{{DocumentID: "document", RevisionID: "older"}}
	if err := store.PutInteractionRevisions(ctx, "session", more); err != nil {
		t.Fatal(err)
	}
	closeDurableForTest(t, store)
	if _, ok := rawPolicyCell(t, dir, policyInteractionRevisionsRow("session")); !ok {
		t.Fatal("the interaction binding was not persisted")
	}
	store = openDurableForTest(t, dir)
	defer closeDurableForTest(t, store)
	got, err := store.InteractionRevisions(ctx, []shoal.ID{"session", "other"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[shoal.ID][]RevisionReference{"session": {
		{DocumentID: "document", RevisionID: "older"},
		{DocumentID: "document", RevisionID: "revision"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reopened bindings = %+v, want the union %+v", got, want)
	}
}

// TestBoundEdgeWritesFormatFloorAndRoundTrips: a bound edge reservation is
// only ever written after the floor, and the binding is restored exactly.
func TestBoundEdgeWritesFormatFloorAndRoundTrips(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := openDurableForTest(t, dir)
	seedDurableStore(t, store)
	registration := EdgeRegistration{
		Edge: graph.Edge{
			ID: "bound-edge", From: "document", To: "node-a",
			Type: "links", Weight: 1,
		},
		Rule: batchTestRule(t, "source", "policy"), Kind: RegistrationApplication,
		SourceRevisions: bindingTestRevisions, SourceRevisionsBound: true,
	}
	if err := store.ReserveEdge(ctx, registration); err != nil {
		t.Fatal(err)
	}
	closeDurableForTest(t, store)
	requireOldBinaryRefuses(t, dir)

	store = openDurableForTest(t, dir)
	if err := store.PutEdge(ctx, registration); err != nil {
		t.Fatal(err)
	}
	closeDurableForTest(t, store)
	store = openDurableForTest(t, dir)
	defer closeDurableForTest(t, store)
	stored, ok, err := store.Edge(ctx, "bound-edge")
	if err != nil || !ok {
		t.Fatalf("Edge = %v, %v", ok, err)
	}
	if !stored.SourceRevisionsBound ||
		!reflect.DeepEqual(stored.SourceRevisions, bindingTestRevisions) {
		t.Fatalf("reopened binding = %+v", stored)
	}
	legacy, ok, err := store.Edge(ctx, "application-edge")
	if err != nil || !ok || legacy.SourceRevisionsBound || legacy.SourceRevisions != nil {
		t.Fatalf("an edge written without a binding reopened bound: %+v", legacy)
	}
}

// TestEdgeBindingIsPartOfTheRegistration: a reservation and its commit must
// carry the same binding, and an extracted edge that replaces a bound
// application edge keeps its binding, so replacing can only narrow.
func TestEdgeBindingIsPartOfTheRegistration(t *testing.T) {
	ctx := context.Background()
	durable := openDurableForTest(t, t.TempDir())
	defer closeDurableForTest(t, durable)
	for name, store := range map[string]PolicyStore{
		"memory": NewMemoryPolicyStore(), "durable": durable,
	} {
		rule := batchTestRule(t, "source", "policy")
		edge := graph.Edge{ID: "edge", From: "a", To: "b", Type: "links", Weight: 1}
		bound := EdgeRegistration{
			Edge: edge, Rule: rule, Kind: RegistrationApplication,
			SourceRevisions: bindingTestRevisions, SourceRevisionsBound: true,
		}
		loose := bound
		loose.SourceRevisionsBound = false
		if err := store.ReserveEdge(ctx, loose); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
			t.Fatalf("%s: revisions without a bound registration = %v", name, err)
		}
		materialized := bound
		materialized.Kind = RegistrationMaterialized
		if err := store.PutEdge(ctx, materialized); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
			t.Fatalf("%s: a bound materialized edge = %v", name, err)
		}
		if err := store.ReserveEdge(ctx, bound); err != nil {
			t.Fatal(err)
		}
		other := bound
		other.SourceRevisions = []RevisionReference{
			{DocumentID: "document", RevisionID: "other"},
		}
		if err := store.PutEdge(ctx, other); !shoal.IsErrorCode(err, shoal.ErrorConflict) {
			t.Fatalf("%s: a commit with another binding = %v, want conflict", name, err)
		}
		if err := store.PutEdge(ctx, unboundEdge(bound)); !shoal.IsErrorCode(err, shoal.ErrorConflict) {
			t.Fatalf("%s: an unbound commit = %v, want conflict", name, err)
		}
		if err := store.PutEdge(ctx, bound); err != nil {
			t.Fatal(err)
		}
		extracted := EdgeRegistration{
			Edge: edge, Rule: rule, Kind: RegistrationExtracted,
			DocumentID: "extracting", RevisionID: "extracting-revision",
		}
		if err := store.PutEdge(ctx, extracted); err != nil {
			t.Fatal(err)
		}
		stored, ok, err := store.Edge(ctx, "edge")
		if err != nil || !ok {
			t.Fatalf("%s: Edge = %v, %v", name, ok, err)
		}
		if stored.Kind != RegistrationExtracted || !stored.SourceRevisionsBound ||
			!reflect.DeepEqual(stored.SourceRevisions, bindingTestRevisions) {
			t.Fatalf("%s: replacing dropped the binding: %+v", name, stored)
		}
		stored.SourceRevisions[0].RevisionID = "mutated"
		again, _, _ := store.Edge(ctx, "edge")
		if !reflect.DeepEqual(again.SourceRevisions, bindingTestRevisions) {
			t.Fatalf("%s: a read aliases the stored binding", name)
		}
	}
}

func unboundEdge(registration EdgeRegistration) EdgeRegistration {
	registration.SourceRevisions = nil
	registration.SourceRevisionsBound = false
	return registration
}

// TestRevisionBindingRowsAreDecodedStrictly: every malformed binding fails
// the open, as every other malformed policy record does.
func TestRevisionBindingRowsAreDecodedStrictly(t *testing.T) {
	floor := func(t *testing.T, value uint64) []byte {
		return mustEncodePolicyRecord(t, policyKindFormatFloor,
			persistedFormatFloor{Seq: 900, Floor: value})
	}
	interactionRow := func(t *testing.T, session string, refs ...persistedRevisionReference) []byte {
		return mustEncodePolicyRecord(t, policyKindInteractionRevisions,
			persistedInteractionRevisions{Seq: 901, SessionID: session, Revisions: refs})
	}
	edgeRow := func(t *testing.T, kind RegistrationKind, bound bool, refs ...persistedRevisionReference) []byte {
		return mustEncodePolicyRecord(t, policyKindEdge, persistedEdge{
			Seq: 902,
			Edge: persistedGraphEdge{
				ID: "raw-edge", From: "document", To: "node-a",
				Type: "links", Weight: 1,
			},
			Rule: mustPersistRuleForTest(t), Kind: uint8(kind),
			SourceRevisions: refs, SourceRevisionsBound: bound,
		})
	}
	a := persistedRevisionReference{DocumentID: "document", RevisionID: "a"}
	b := persistedRevisionReference{DocumentID: "document", RevisionID: "b"}
	type row struct {
		key   []byte
		value func(*testing.T) []byte
	}
	floorRow := row{[]byte(policyRowFormatFloor), func(t *testing.T) []byte { return floor(t, 1) }}
	sessionRow := func(value func(*testing.T) []byte) row {
		return row{policyInteractionRevisionsRow("session"), value}
	}
	rawEdgeRow := func(value func(*testing.T) []byte) row {
		return row{policyEdgeRow("raw-edge"), value}
	}
	for _, test := range []struct {
		name     string
		rows     []row
		accepted bool
		preFloor bool
	}{
		{"valid interaction binding", []row{floorRow, sessionRow(func(t *testing.T) []byte {
			return interactionRow(t, "session", a, b)
		})}, true, false},
		{"valid bound edge", []row{floorRow, rawEdgeRow(func(t *testing.T) []byte {
			return edgeRow(t, RegistrationApplication, true, a, b)
		})}, true, false},
		{"valid bound edge with no revisions", []row{floorRow, rawEdgeRow(func(t *testing.T) []byte {
			return edgeRow(t, RegistrationApplication, true)
		})}, true, false},
		{"floor alone", []row{floorRow}, true, false},
		{"unknown floor", []row{{[]byte(policyRowFormatFloor), func(t *testing.T) []byte {
			return floor(t, 2)
		}}}, false, false},
		{"floor of the wrong kind", []row{{[]byte(policyRowFormatFloor), func(t *testing.T) []byte {
			return mustEncodePolicyRecord(t, policyKindSourceClaim, persistedSourceClaim{
				Seq: 1, SourceURI: "file:///x", Version: 1, Rule: mustPersistRuleForTest(t),
			})
		}}}, false, false},
		{"interaction binding without floor", []row{sessionRow(func(t *testing.T) []byte {
			return interactionRow(t, "session", a)
		})}, false, true},
		{"bound edge without floor", []row{rawEdgeRow(func(t *testing.T) []byte {
			return edgeRow(t, RegistrationApplication, true, a)
		})}, false, true},
		{"pre-floor catalog with an unbound edge", []row{rawEdgeRow(func(t *testing.T) []byte {
			return edgeRow(t, RegistrationApplication, false)
		})}, true, true},
		{"empty interaction binding", []row{floorRow, sessionRow(func(t *testing.T) []byte {
			return interactionRow(t, "session")
		})}, false, false},
		{"unsorted interaction binding", []row{floorRow, sessionRow(func(t *testing.T) []byte {
			return interactionRow(t, "session", b, a)
		})}, false, false},
		{"duplicated interaction binding", []row{floorRow, sessionRow(func(t *testing.T) []byte {
			return interactionRow(t, "session", a, a)
		})}, false, false},
		{"interaction binding with an empty revision", []row{floorRow, sessionRow(func(t *testing.T) []byte {
			return interactionRow(t, "session", persistedRevisionReference{DocumentID: "document"})
		})}, false, false},
		{"interaction binding under another session's row", []row{floorRow, sessionRow(func(t *testing.T) []byte {
			return interactionRow(t, "another", a)
		})}, false, false},
		{"interaction binding of the wrong kind", []row{floorRow, sessionRow(func(t *testing.T) []byte {
			return mustEncodePolicyRecord(t, policyKindEdge, persistedInteractionRevisions{
				Seq: 1, SessionID: "session", Revisions: []persistedRevisionReference{a},
			})
		})}, false, false},
		{"edge revisions without binding", []row{floorRow, rawEdgeRow(func(t *testing.T) []byte {
			return edgeRow(t, RegistrationApplication, false, a)
		})}, false, false},
		{"unsorted edge binding", []row{floorRow, rawEdgeRow(func(t *testing.T) []byte {
			return edgeRow(t, RegistrationApplication, true, b, a)
		})}, false, false},
		{"bound materialized edge", []row{floorRow, rawEdgeRow(func(t *testing.T) []byte {
			return edgeRow(t, RegistrationMaterialized, true, a)
		})}, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var dir string
			if test.preFloor {
				dir = preFloorCatalog(t)
			} else {
				dir = t.TempDir()
				store := openDurableForTest(t, dir)
				seedDurableStore(t, store)
				closeDurableForTest(t, store)
			}
			for _, row := range test.rows {
				corruptPolicyRow(t, dir, row.key, row.value(t))
			}
			reopened, err := OpenDurablePolicyStore(dir)
			if err == nil {
				_ = reopened.Close()
			}
			if (err == nil) != test.accepted {
				t.Fatalf("open = %v, want accepted=%v", err, test.accepted)
			}
			if err != nil && !shoal.IsErrorCode(err, shoal.ErrorInternal) {
				t.Fatalf("open = %v, want internal", err)
			}
		})
	}
}

// TestInteractionRevisionsOnlyGrow: a second recording unions with the first
// in both stores, and never removes a revision.
func TestInteractionRevisionsOnlyGrow(t *testing.T) {
	ctx := context.Background()
	durable := openDurableForTest(t, t.TempDir())
	defer closeDurableForTest(t, durable)
	for name, store := range map[string]PolicyStore{
		"memory": NewMemoryPolicyStore(), "durable": durable,
	} {
		first := []RevisionReference{{DocumentID: "d", RevisionID: "r2"}}
		second := []RevisionReference{
			{DocumentID: "d", RevisionID: "r1"}, {DocumentID: "d", RevisionID: "r1"},
		}
		if err := store.PutInteractionRevisions(ctx, "s", first); err != nil {
			t.Fatal(err)
		}
		if err := store.PutInteractionRevisions(ctx, "s", second); err != nil {
			t.Fatal(err)
		}
		if err := store.PutInteractionRevisions(ctx, "s", nil); err != nil {
			t.Fatal(err)
		}
		got, err := store.InteractionRevisions(ctx, []shoal.ID{"s"})
		if err != nil {
			t.Fatal(err)
		}
		want := []RevisionReference{
			{DocumentID: "d", RevisionID: "r1"}, {DocumentID: "d", RevisionID: "r2"},
		}
		if !reflect.DeepEqual(got["s"], want) {
			t.Fatalf("%s: recorded %+v, want %+v", name, got["s"], want)
		}
		if err := store.PutInteractionRevisions(ctx, "s", []RevisionReference{
			{DocumentID: "d"},
		}); err == nil {
			t.Fatalf("%s: an incomplete revision was recorded", name)
		}
	}
}

// TestDocumentRevisionRulesListsEveryRevision: every registered revision is
// reported, ordered by revision ID, with only the current one marked, and in
// a reopened durable store too.
func TestDocumentRevisionRulesListsEveryRevision(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	durable := openDurableForTest(t, dir)
	memory := NewMemoryPolicyStore()
	rules := []AccessRule{
		batchTestRule(t, "source", "one"), batchTestRule(t, "source", "two"),
	}
	for _, store := range []PolicyStore{memory, durable} {
		for index, revision := range []shoal.ID{"rev-one", "rev-two"} {
			if err := store.PutRevision(ctx, RevisionRegistration{
				DocumentID: "document", RevisionID: revision,
				NodeIDs:       []shoal.ID{"document"},
				ContentDigest: [32]byte{byte(index + 1)},
				Rule:          rules[index], Current: true,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	closeDurableForTest(t, durable)
	durable = openDurableForTest(t, dir)
	defer closeDurableForTest(t, durable)
	for name, store := range map[string]PolicyStore{"memory": memory, "durable": durable} {
		got, err := store.DocumentRevisionRules(ctx,
			[]shoal.ID{"document", "absent", "document"})
		if err != nil {
			t.Fatal(err)
		}
		listed := got["document"]
		if len(got) != 1 || len(listed) != 2 ||
			listed[0].RevisionID != "rev-one" || listed[0].Current ||
			!listed[0].Rule.equal(rules[0]) ||
			listed[1].RevisionID != "rev-two" || !listed[1].Current ||
			!listed[1].Rule.equal(rules[1]) {
			t.Fatalf("%s: DocumentRevisionRules = %+v", name, got)
		}
		// RevisionRules: the referenced revision and the current one only.
		partial, err := store.RevisionRules(ctx, []RevisionReference{
			{DocumentID: "document", RevisionID: "rev-one"},
			{DocumentID: "document", RevisionID: "unregistered"},
			{DocumentID: "absent", RevisionID: "rev-one"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(revisionIDsOf(partial), map[shoal.ID][]string{
			"document": {"rev-one", "rev-two*"},
		}) {
			t.Fatalf("%s: RevisionRules = %+v", name, revisionIDsOf(partial))
		}
		current, err := store.RevisionRules(ctx, []RevisionReference{
			{DocumentID: "document", RevisionID: "rev-two"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(revisionIDsOf(current), map[shoal.ID][]string{
			"document": {"rev-two*"},
		}) {
			t.Fatalf("%s: RevisionRules(current) = %+v", name, revisionIDsOf(current))
		}
		if _, err := store.RevisionRules(ctx, []RevisionReference{
			{DocumentID: "document"},
		}); err == nil {
			t.Fatalf("%s: an incomplete reference was accepted", name)
		}
	}
}

// revisionIDsOf renders rules as revision IDs, the current one starred.
func revisionIDsOf(rules map[shoal.ID][]RevisionRule) map[shoal.ID][]string {
	rendered := make(map[shoal.ID][]string, len(rules))
	for documentID, listed := range rules {
		for _, rule := range listed {
			id := string(rule.RevisionID)
			if rule.Current {
				id += "*"
			}
			rendered[documentID] = append(rendered[documentID], id)
		}
	}
	return rendered
}

// fakePublications is a PublicationReader with fixed answers.
type fakePublications struct {
	revisions map[shoal.ID]map[shoal.ID]explorer.RevisionPublication
	connected map[shoal.ID]bool
}

func (f fakePublications) RevisionPublications(
	_ context.Context, documentIDs []shoal.ID,
) (map[shoal.ID]map[shoal.ID]explorer.RevisionPublication, error) {
	out := make(map[shoal.ID]map[shoal.ID]explorer.RevisionPublication)
	for _, id := range documentIDs {
		if revisions, ok := f.revisions[id]; ok {
			out[id] = revisions
		}
	}
	return out, nil
}

func (f fakePublications) ConnectedEdges(
	_ context.Context, edgeIDs []shoal.ID,
) (map[shoal.ID]bool, error) {
	connected := make(map[shoal.ID]bool)
	for _, id := range edgeIDs {
		if f.connected[id] {
			connected[id] = true
		}
	}
	return connected, nil
}

// legacyHistory registers r1 (open), r2 (labelled) and r3 (current, under
// its own rule) of one document, with the given publications.
func legacyHistory(
	t *testing.T, publications map[shoal.ID]explorer.RevisionPublication,
) (*MemoryPolicyStore, fakePublications, AccessRule, AccessRule) {
	t.Helper()
	ctx := context.Background()
	store := NewMemoryPolicyStore()
	labelled := batchTestRule(t, "source", "labelled")
	open := batchTestRule(t, "source", "policy")
	for index, revision := range []struct {
		id   shoal.ID
		rule AccessRule
	}{{"r1", open}, {"r2", labelled}, {"r3", legacyCurrentRule(t)}} {
		if err := store.PutRevision(ctx, RevisionRegistration{
			DocumentID: "document", RevisionID: revision.id,
			NodeIDs:       []shoal.ID{"document", "node"},
			ContentDigest: [32]byte{byte(index + 1)},
			Rule:          revision.rule, Current: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return store, fakePublications{
		revisions: map[shoal.ID]map[shoal.ID]explorer.RevisionPublication{
			"document": publications,
		},
	}, open, labelled
}

func legacyCurrentRule(t *testing.T) AccessRule {
	return batchTestRule(t, "source", "current")
}

// TestLegacyEdgesAreHeldToEveryRevision: an unbound application edge the
// corpus connected is held to every revision of its endpoint's document,
// whatever the publication clocks say (#695 review: an edge connected at
// 90 minutes over a labelled revision published at 2 hours by a stepped
// clock must stay closed). One the corpus never connected (an extracted or
// materialized edge persisted before RegistrationKind) is left as
// registered, unless there is no reader to say so.
func TestLegacyEdgesAreHeldToEveryRevision(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store, reader, open, labelled := legacyHistory(t,
		map[shoal.ID]explorer.RevisionPublication{
			"r1": {PublishedAt: base.Add(time.Hour), Sequence: 1},
			"r2": {PublishedAt: base.Add(2 * time.Hour), Sequence: 2},
			"r3": {PublishedAt: base.Add(3 * time.Hour), Sequence: 3},
		})
	reader.connected = map[shoal.ID]bool{"connected": true}
	edgeRule := batchTestRule(t, "source", "edge")
	edges := map[shoal.ID]EdgeRegistration{}
	for _, id := range []shoal.ID{"connected", "unconnected"} {
		edges[id] = EdgeRegistration{
			Edge: graph.Edge{ID: id, From: "document", To: "node", Type: "links", Weight: 1},
			Rule: edgeRule, Kind: RegistrationApplication,
		}
	}
	everything, err := conjoinRules(edgeRule, open)
	if err != nil {
		t.Fatal(err)
	}
	if everything, err = conjoinRules(everything, labelled); err != nil {
		t.Fatal(err)
	}
	if everything, err = conjoinRules(everything, legacyCurrentRule(t)); err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		client *Client
		want   map[shoal.ID]AccessRule
	}{
		"with a reader": {&Client{policyStore: store, publications: reader},
			map[shoal.ID]AccessRule{"connected": everything, "unconnected": edgeRule}},
		"without a reader": {&Client{policyStore: store},
			map[shoal.ID]AccessRule{"connected": everything, "unconnected": everything}},
	} {
		got, err := test.client.bindEdgeSourceRevisions(ctx, edges)
		if err != nil {
			t.Fatal(err)
		}
		for id, want := range test.want {
			if !got[id].Rule.equal(want) {
				t.Fatalf("%s: %s rule = %v, want %v", name, id, got[id].Rule, want)
			}
		}
	}
}

// TestLegacySessionBoundIsByPublicationSequence: a session's wall-clock
// snapshot time only picks the newest revision published by then; the
// publication sequence decides the rest, so a stepped clock cannot drop a
// revision published before the one the session saw.
func TestLegacySessionBoundIsByPublicationSequence(t *testing.T) {
	ctx := context.Background()
	at := func(seconds int) time.Time {
		return time.Date(2026, 1, 1, 0, 0, seconds, 0, time.UTC)
	}
	type publication = explorer.RevisionPublication
	pub := func(seconds int, sequence uint64) publication {
		return publication{PublishedAt: at(seconds), Sequence: sequence}
	}
	for _, test := range []struct {
		name         string
		publications map[shoal.ID]publication
		asOf         time.Time
		labelled     bool // whether r2's labelled rule is required
	}{
		{"labelled revision published after the snapshot", map[shoal.ID]publication{
			"r1": pub(100, 1), "r2": pub(200, 2), "r3": pub(300, 3),
		}, at(150), false},
		{"bound is inclusive", map[shoal.ID]publication{
			"r1": pub(100, 1), "r2": pub(200, 2), "r3": pub(300, 3),
		}, at(200), true},
		{"clock stepped back after the labelled revision", map[shoal.ID]publication{
			"r1": pub(100, 1), "r2": pub(200, 2), "r3": pub(150, 3),
		}, at(150), true},
		{"a revision with no sequence counts", map[shoal.ID]publication{
			"r1": pub(100, 1), "r2": pub(200, 0), "r3": pub(300, 3),
		}, at(150), true},
		{"a revision with no time does not set the bound", map[shoal.ID]publication{
			"r1": pub(100, 1), "r2": publication{Sequence: 2}, "r3": pub(300, 3),
		}, at(150), false},
		{"nothing published by the snapshot: every revision", map[shoal.ID]publication{
			"r1": pub(100, 1), "r2": pub(200, 2), "r3": pub(300, 3),
		}, at(50), true},
		{"no snapshot time: every revision", map[shoal.ID]publication{
			"r1": pub(100, 1), "r2": pub(200, 2), "r3": pub(300, 3),
		}, time.Time{}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, reader, _, labelled := legacyHistory(t, test.publications)
			client := &Client{policyStore: store, publications: reader}
			cache := client.newRevisionRuleCache()
			if err := cache.loadDocuments(ctx, []shoal.ID{"document"}); err != nil {
				t.Fatal(err)
			}
			rules, ok := cache.documentRules(
				"document", cache.sequenceBound("document", test.asOf))
			if !ok {
				t.Fatal("no current revision")
			}
			required, current := false, false
			for _, rule := range rules {
				required = required || rule.equal(labelled)
				current = current || rule.equal(legacyCurrentRule(t))
			}
			if required != test.labelled {
				t.Fatalf("labelled revision required = %v, want %v", required, test.labelled)
			}
			if !current {
				t.Fatal("the current revision is not required")
			}
		})
	}
}

// TestRevisionGateKeysTheLegacyBoundBySequence: one read deciding two
// sessions with different bounds over one document decides each by its own
// bound, the zero and epoch times included.
func TestRevisionGateKeysTheLegacyBoundBySequence(t *testing.T) {
	ctx := context.Background()
	at := func(seconds int) time.Time {
		return time.Date(2026, 1, 1, 0, 0, seconds, 0, time.UTC)
	}
	store, reader, _, _ := legacyHistory(t, map[shoal.ID]explorer.RevisionPublication{
		"r1": {PublishedAt: at(100), Sequence: 1},
		"r2": {PublishedAt: at(200), Sequence: 2},
		"r3": {PublishedAt: at(300), Sequence: 3},
	})
	client := &Client{policyStore: store, publications: reader}
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "subject", Actor: "actor",
		AuthorizationDomain: []byte("domain"),
		AllowedOperations:   []auth.Operation{auth.OperationNeighborhood},
		PermittedSourceIDs:  [][]byte{[]byte("source")},
		PermittedPolicyIDs:  [][]byte{[]byte("policy"), []byte("current")},
		PolicyGeneration:    1, AuthenticationExpires: at(400).Add(time.Hour),
		RequestID: "request",
	})
	if err != nil {
		t.Fatal(err)
	}
	gate := client.newRevisionGate(decision, auth.OperationNeighborhood, at(400))
	if err := gate.cache.loadDocuments(ctx, []shoal.ID{"document"}); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		asOf time.Time
		want bool
	}{
		{at(150), true}, {time.Time{}, false}, {time.Unix(0, 0).UTC(), false},
		{at(150), true}, {at(250), false},
	} {
		got, err := gate.documentAllows("document", step.asOf)
		if err != nil {
			t.Fatal(err)
		}
		if got != step.want {
			t.Fatalf("asOf %v: allowed = %v, want %v", step.asOf, got, step.want)
		}
	}
}
