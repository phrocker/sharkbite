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

package explorer

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type snapshotAssertionFixture struct {
	corpus       *Explorer
	directory    string
	path         graph.Path
	assertion    ontology.Assertion
	record       persistedExtraction
	publications int
}

func newSnapshotAssertionFixture(t *testing.T) *snapshotAssertionFixture {
	t.Helper()
	f := &snapshotAssertionFixture{directory: t.TempDir()}
	var err error
	f.corpus, err = Open(f.directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.corpus.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx := context.Background()
	receipt, err := f.corpus.Ingest(ctx, Source{
		URI: "memory://snapshot-assertion", MediaType: MediaTypeText,
		Content: "source evidence",
	})
	if err != nil {
		t.Fatal(err)
	}
	view, err := f.corpus.Document(ctx, receipt.Document.ID, receipt.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	neighborhood, err := f.corpus.Neighborhood(ctx, NeighborhoodRequest{
		NodeIDs: []shoal.ID{receipt.Document.ID}, Depth: 2,
	})
	if err != nil || len(neighborhood.Edges) == 0 {
		t.Fatalf("source graph = %#v, %v", neighborhood, err)
	}
	var edge graph.Edge
	for _, candidate := range neighborhood.Edges {
		if candidate.From == view.Root.Spans[0].ID ||
			candidate.To == view.Root.Spans[0].ID {
			edge = candidate
			break
		}
	}
	if edge.ID == "" {
		t.Fatal("source graph has no span edge")
	}
	nodes := make(map[shoal.ID]graph.Node)
	for _, node := range neighborhood.Nodes {
		nodes[node.ID] = node
	}
	f.path = graph.Path{
		Nodes: []graph.Node{nodes[edge.From], nodes[edge.To]},
		Edges: []graph.Edge{edge},
	}
	predicate, err := ontology.NewPropertyDefinition(
		"related", "Related", "", ontology.ValueReference, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	schema, err := ontology.NewOntologySchema("snapshot", "Snapshot", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	version, err := ontology.NewOntologyVersion(
		schema, "1", view.Revision.CreatedAt, nil, nil,
		[]ontology.PropertyDefinition{predicate}, nil)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := ontology.NewOntologyIdentity(version)
	if err != nil {
		t.Fatal(err)
	}
	value, err := ontology.NewReferenceValue(edge.To)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := ontology.NewEvidenceRef(document.Citation{
		DocumentID: receipt.Document.ID, RevisionID: receipt.Revision.ID,
		SectionID: view.Root.Section.ID, Range: view.Root.Section.Range,
	}, "source evidence", nil)
	if err != nil {
		t.Fatal(err)
	}
	provenance, err := ontology.NewExtractionProvenance(
		"provider", "model", "1", "prompt", "1", "extractor", "1", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.assertion, err = ontology.NewAssertion(
		edge.From, predicate.ID(), value, ontology.AssertionExplicit, 1,
		[]ontology.EvidenceRef{evidence}, provenance,
		shoal.Metadata{graphAssertionEdgeIDMetadata: string(edge.ID)},
		ontology.WithAssertionOntology(identity),
	)
	if err != nil {
		t.Fatal(err)
	}
	f.record = persistedExtraction{
		ID:         "assertion-only-extraction",
		DocumentID: receipt.Document.ID, RevisionID: receipt.Revision.ID,
		OntologySchemaID: schema.ID(), OntologyVersionID: version.ID(),
		PublishedAt: view.Revision.CreatedAt,
	}
	return f
}

func (f *snapshotAssertionFixture) publishLocked(t *testing.T) {
	t.Helper()
	f.publications++
	f.record.ID = shoal.ID(fmt.Sprintf("assertion-extraction-%08d", f.publications))
	f.record.PublishedAt = f.record.PublishedAt.Add(time.Second)
	assertion, err := persistAssertion(f.path.Edges[0].ID, f.assertion)
	if err != nil {
		t.Fatal(err)
	}
	f.record.Assertions = []persistedExtractionAssertion{assertion}
	if err := validatePersistedExtraction(f.record); err != nil {
		t.Fatal(err)
	}
	if err := f.corpus.writeRecord(
		extractionRecordRow(f.record.ID), embeddedRecordExtraction, f.record,
	); err != nil {
		t.Fatal(err)
	}
	record := f.record
	f.corpus.extractions[record.ID] = &record
	if err := f.corpus.rebuildCurrentGraphLocked(); err != nil {
		t.Fatal(err)
	}
}

func (f *snapshotAssertionFixture) publish(t *testing.T) {
	t.Helper()
	f.corpus.mu.Lock()
	defer f.corpus.mu.Unlock()
	f.publishLocked(t)
}

func (f *snapshotAssertionFixture) snapshot(t *testing.T) Snapshot {
	t.Helper()
	snapshot, err := f.corpus.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func (f *snapshotAssertionFixture) reference(t *testing.T) interaction.EvidenceReference {
	t.Helper()
	assertions := []interaction.AssertionReference{{
		AssertionID: f.assertion.ID(), EdgeID: f.path.Edges[0].ID,
		Origin: f.assertion.Origin(),
	}}
	anchor, err := inference.NewGraphAnchorWithAssertions(f.path, assertions)
	if err != nil {
		t.Fatal(err)
	}
	return interaction.EvidenceReference{
		Kind: interaction.EvidenceGraph, AnchorID: anchor.ID(),
		NodeIDs: []shoal.ID{f.path.Nodes[0].ID, f.path.Nodes[1].ID},
		EdgeIDs: []shoal.ID{f.path.Edges[0].ID}, Assertions: assertions,
	}
}

func (f *snapshotAssertionFixture) validate(t *testing.T, snapshot Snapshot, conflict bool) {
	t.Helper()
	reference := f.reference(t)
	err := f.corpus.ValidateEvidenceSnapshot(
		context.Background(), shoal.ID(snapshot.ID), snapshot.AsOf,
		reference.NodeIDs, reference.EdgeIDs, []interaction.EvidenceReference{reference},
	)
	if conflict {
		if !shoal.IsErrorCode(err, shoal.ErrorConflict) {
			t.Fatalf("historically impossible assertion = %v, want conflict", err)
		}
	} else if err != nil {
		t.Fatalf("historically valid assertion = %v", err)
	}
}

func (f *snapshotAssertionFixture) reopen(t *testing.T) {
	t.Helper()
	if err := f.corpus.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	f.corpus, err = Open(f.directory)
	if err != nil {
		t.Fatal(err)
	}
}

func (f *snapshotAssertionFixture) annotate(t *testing.T) {
	t.Helper()
	metadata := f.assertion.Metadata()
	metadata["annotation"] = "changed without changing assertion identity"
	identity, _ := f.assertion.Ontology()
	updated, err := ontology.NewAssertion(
		f.assertion.Subject(), f.assertion.Predicate(), f.assertion.Object(),
		f.assertion.Origin(), f.assertion.Confidence(), f.assertion.Evidence(),
		f.assertion.Provenance(), metadata,
		ontology.WithAssertionOntology(identity),
	)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID() != f.assertion.ID() {
		t.Fatal("annotation unexpectedly changed the assertion ID")
	}
	f.assertion = updated
}

func TestHistoricalSnapshotBindsAssertionAdditionAcrossRestart(t *testing.T) {
	f := newSnapshotAssertionFixture(t)
	before := f.snapshot(t)
	f.publish(t)
	f.validate(t, before, true)
	after := f.snapshot(t)
	if before.ID == after.ID {
		t.Fatal("assertion-only publication did not change snapshot identity")
	}
	f.validate(t, after, false)
	if _, err := f.corpus.Ingest(context.Background(), Source{
		URI: "memory://unrelated", MediaType: MediaTypeText, Content: "unrelated",
	}); err != nil {
		t.Fatal(err)
	}
	latest := f.snapshot(t)
	f.validate(t, after, false)
	f.reopen(t)
	if got := f.snapshot(t); got != latest {
		t.Fatalf("assertion snapshot changed across restart: %+v != %+v", got, latest)
	}
	f.validate(t, before, true)
	f.validate(t, after, false)
}

func TestHistoricalSnapshotBindsAssertionAnnotationsAndRemoval(t *testing.T) {
	f := newSnapshotAssertionFixture(t)
	f.publish(t)
	before := f.snapshot(t)
	f.annotate(t)
	f.publish(t)
	changed := f.snapshot(t)
	if before.ID == changed.ID {
		t.Fatal("assertion metadata did not change snapshot identity")
	}
	f.validate(t, before, true)
	f.validate(t, changed, false)
	f.reopen(t)
	f.validate(t, before, true)
	f.validate(t, changed, false)
	if _, err := f.corpus.Ingest(context.Background(), Source{
		URI: "memory://snapshot-assertion", MediaType: MediaTypeText,
		Content: "replacement source with a different span",
	}); err != nil {
		t.Fatal(err)
	}
	removed := f.snapshot(t)
	if removed.ID == changed.ID {
		t.Fatal("assertion removal did not change snapshot identity")
	}
	delta := f.corpus.snapshotHistory[removed.ID]
	if len(delta.RemovedAssertionEdgeIDs) != 1 ||
		delta.RemovedAssertionEdgeIDs[0] != f.path.Edges[0].ID {
		t.Fatalf("assertion removal was not persisted: %#v", delta)
	}
	f.reopen(t)
	f.validate(t, changed, true)
}

func TestInteractionWriteRechecksAssertionStateUnderLock(t *testing.T) {
	f := newSnapshotAssertionFixture(t)
	f.publish(t)
	pin := f.snapshot(t)
	f.validate(t, pin, false)
	reference := f.reference(t)
	session := interaction.Session{
		ID: "interaction.session_assertion-race", Operation: interaction.OperationRetrieval,
		RecordedAt: pin.AsOf.Add(time.Minute),
		SnapshotID: shoal.ID(pin.ID), SnapshotAsOf: pin.AsOf,
		AuthorizationFingerprint: "fingerprint",
		AuthorizationExpiresAt:   pin.AsOf.Add(time.Hour),
		SeedNodeIDs:              reference.NodeIDs,
		SeedEvidence:             []interaction.EvidenceReference{reference},
	}
	done := make(chan error, 1)
	func() {
		f.corpus.mu.Lock()
		defer f.corpus.mu.Unlock()
		go func() {
			_, err := f.corpus.RecordInteractionResult(context.Background(), session)
			done <- err
		}()
		f.annotate(t)
		f.publishLocked(t)
	}()
	select {
	case err := <-done:
		if !shoal.IsErrorCode(err, shoal.ErrorConflict) {
			t.Fatalf("write accepted changed historical assertion: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("interaction write did not finish")
	}
	f.reopen(t)
	if _, err := f.corpus.Interaction(
		context.Background(), session.ID,
	); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("rejected interaction was persisted: %v", err)
	}
}

func TestLegacySnapshotCannotProveAssertionState(t *testing.T) {
	f := newSnapshotAssertionFixture(t)
	f.publish(t)
	pin := f.snapshot(t)
	stored := f.corpus.snapshotHistory[pin.ID]
	legacy := struct {
		ID             shoal.ID
		AsOf           time.Time
		ParentID       shoal.ID
		AddedNodeIDs   []shoal.ID
		RemovedNodeIDs []shoal.ID
		NodeStates     []persistedSnapshotObject
		RemovedEdgeIDs []shoal.ID
		EdgeStates     []persistedSnapshotObject
	}{
		ID: shoal.ID(strings.Repeat("0", 64)), AsOf: stored.AsOf,
		AddedNodeIDs: stored.AddedNodeIDs, NodeStates: stored.NodeStates,
		EdgeStates: stored.EdgeStates,
	}
	if err := f.corpus.writeRecord(
		snapshotRecordRow(legacy.ID), embeddedRecordSnapshot, legacy,
	); err != nil {
		t.Fatal(err)
	}
	f.reopen(t)
	oldPin := Snapshot{ID: string(legacy.ID), AsOf: legacy.AsOf}
	reference := f.reference(t)
	if err := f.corpus.ValidateEvidenceSnapshot(
		context.Background(), legacy.ID, legacy.AsOf,
		reference.NodeIDs, reference.EdgeIDs, nil,
	); err != nil {
		t.Fatalf("legacy node and edge proof was lost: %v", err)
	}
	f.validate(t, oldPin, true)
	f.validate(t, pin, false)
}

func TestSnapshotReusesIdenticalHistoricalAssertionState(t *testing.T) {
	f := newSnapshotAssertionFixture(t)
	f.publish(t)
	first := f.snapshot(t)
	original := f.assertion
	f.annotate(t)
	f.publish(t)
	changed := f.snapshot(t)
	f.assertion = original
	f.publish(t)
	if got := f.snapshot(t); got != first {
		t.Fatalf("identical logical assertion state changed frontier: %+v != %+v", got, first)
	}
	f.validate(t, first, false)
	f.validate(t, changed, true)
	f.reopen(t)
	if got := f.snapshot(t); got != first {
		t.Fatalf("reopened repeated frontier changed: %+v != %+v", got, first)
	}
	f.validate(t, first, false)
}

func TestGraphSnapshotEvidenceCannotOmitAuthoritativeAssertions(t *testing.T) {
	f := newSnapshotAssertionFixture(t)
	f.publish(t)
	pin := f.snapshot(t)
	f.validate(t, pin, false)
	full := f.reference(t)
	plain, err := inference.NewGraphAnchor(f.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, anchorID := range []shoal.ID{full.AnchorID, plain.ID()} {
		reference := full
		reference.AnchorID = anchorID
		reference.Assertions = nil
		err := f.corpus.ValidateEvidenceSnapshot(
			context.Background(), shoal.ID(pin.ID), pin.AsOf,
			reference.NodeIDs, reference.EdgeIDs,
			[]interaction.EvidenceReference{reference},
		)
		if !shoal.IsErrorCode(err, shoal.ErrorConflict) {
			t.Fatalf("omitted assertion with anchor %q = %v, want conflict", anchorID, err)
		}
	}
}

func TestSnapshotAssertionDeltaValidationAndEquality(t *testing.T) {
	state := persistedSnapshotObject{ID: "edge", Digest: strings.Repeat("a", 64)}
	for _, test := range []struct {
		name   string
		record persistedSnapshot
		valid  bool
	}{
		{"legacy", persistedSnapshot{}, true},
		{"addition", persistedSnapshot{AssertionStates: []persistedSnapshotObject{state}}, true},
		{"removal", persistedSnapshot{RemovedAssertionEdgeIDs: []shoal.ID{"edge"}}, true},
		{"duplicate", persistedSnapshot{AssertionStates: []persistedSnapshotObject{state, state}}, false},
		{"overlap", persistedSnapshot{
			AssertionStates:         []persistedSnapshotObject{state},
			RemovedAssertionEdgeIDs: []shoal.ID{"edge"},
		}, false},
		{"invalid digest", persistedSnapshot{AssertionStates: []persistedSnapshotObject{{
			ID: "edge", Digest: "invalid",
		}}}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateSnapshotDelta(test.record); (err == nil) != test.valid {
				t.Fatalf("validation = %v, valid = %v", err, test.valid)
			}
		})
	}
	base := persistedSnapshot{ID: "snapshot", AsOf: time.Unix(1, 0).UTC()}
	added := base
	added.AssertionStates = []persistedSnapshotObject{state}
	removed := base
	removed.RemovedAssertionEdgeIDs = []shoal.ID{"edge"}
	for _, record := range []persistedSnapshot{added, removed} {
		if persistedSnapshotsEqual(base, record) {
			t.Fatal("snapshot equality ignored assertion state")
		}
		encoded, err := encodeEmbeddedRecord(embeddedRecordSnapshot, record)
		if err != nil {
			t.Fatal(err)
		}
		var restored persistedSnapshot
		if err := decodeEmbeddedRecord(
			encoded, embeddedRecordSnapshot, &restored,
		); err != nil {
			t.Fatal(err)
		}
		if !persistedSnapshotsEqual(record, restored) {
			t.Fatal("assertion state changed during snapshot encoding")
		}
	}
}
