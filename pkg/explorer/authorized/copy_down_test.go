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

package authorized_test

// Copy-down through operations that name their inputs (#663). Each of them
// stores its output under a rule that can be weaker than its inputs' rules,
// and none of them serves its content to a reader the inputs would refuse,
// because every read re-checks the inputs' current rules (existence oracles
// are #698, #699, #700). These tests pin that binding for
// each operation, with the declared output rule demonstrably weaker, and pin
// that no declassification record is written and that
// RequireDeclassificationReason does not refuse the write. See
// declassification.go, "Copy-down through declared inputs". The binding is to
// the inputs' current rules; binding to the revision an output was derived
// from is #695.

import (
	"context"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// copyDownWorld is one labelled and one unlabelled document, ingested by a
// principal holding the label, behind a client that requires a reason for
// every declassification.
type copyDownWorld struct {
	f      *fixture
	store  authorized.PolicyStore
	client *authorized.Client
	open   explorer.IngestResult
	closed explorer.IngestResult
}

func newCopyDownWorld(t *testing.T, store authorized.PolicyStore) copyDownWorld {
	t.Helper()
	f := newFixture(t)
	client := f.declassificationClient(t, store, true)
	holder := f.labelIngester(t, "holder", "secret")
	open, err := client.Ingest(holder, labelledSource(
		"file:///copy-down/open.txt", "open evidence", ""))
	if err != nil {
		t.Fatal(err)
	}
	closed, err := client.Ingest(holder, labelledSource(
		"file:///copy-down/closed.txt", "secret evidence", "secret"))
	if err != nil {
		t.Fatal(err)
	}
	return copyDownWorld{f: f, store: store, client: client, open: open, closed: closed}
}

func (w copyDownWorld) currentRule(t *testing.T, documentID shoal.ID) authorized.AccessRule {
	t.Helper()
	current, ok, err := w.store.CurrentRevision(context.Background(), documentID)
	if err != nil || !ok {
		t.Fatalf("current revision of %s = %v, %v", documentID, ok, err)
	}
	return current.Rule
}

func (w copyDownWorld) assertNoDeclassification(t *testing.T) {
	t.Helper()
	if records := declassifications(t, w.store); len(records) != 0 {
		t.Fatalf("a copy-down bound to its inputs was recorded as a declassification: %+v",
			records)
	}
}

func neighborhoodHasEdge(neighborhood explorer.Neighborhood, id shoal.ID) bool {
	for _, edge := range neighborhood.Edges {
		if edge.ID == id {
			return true
		}
	}
	return false
}

// TestCopyDownConnectEdgeStaysBoundToItsEndpoints: a label holder connects an
// unlabelled document to a labelled one, with text from the labelled one on
// the edge. The edge is stored under the bare source rule (an edge rule can
// never carry a label, #570), which is weaker than the labelled endpoint's
// rule, yet a reader without the label never sees the edge.
func TestCopyDownConnectEdgeStaysBoundToItsEndpoints(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		w := newCopyDownWorld(t, store)
		holder := w.f.labelIngester(t, "holder", "secret")
		edge := graph.Edge{
			ID: "copy-down-edge", From: w.open.Document.ID, To: w.closed.Document.ID,
			Type: "mentions", Weight: 1,
			Properties: shoal.Metadata{"note": "copied from the secret evidence"},
		}
		// No reason is given and the operator requires one for every
		// declassification: this is not one, so it is not refused.
		if err := w.client.Connect(holder, edge); err != nil {
			t.Fatalf("connect: %v", err)
		}
		registration, ok, err := store.Edge(context.Background(), edge.ID)
		if err != nil || !ok {
			t.Fatalf("edge registration = %v, %v", ok, err)
		}
		openRule, closedRule := w.currentRule(t, w.open.Document.ID),
			w.currentRule(t, w.closed.Document.ID)
		if registration.Rule.String() != openRule.String() ||
			registration.Rule.String() == closedRule.String() {
			t.Fatalf("edge rule %v, want the unlabelled endpoint's %v and not the "+
				"labelled endpoint's %v", registration.Rule, openRule, closedRule)
		}

		reader := w.f.labelIngester(t, "reader")
		around, err := w.client.Neighborhood(reader, explorer.NeighborhoodRequest{
			NodeIDs: []shoal.ID{w.open.Document.ID}, Depth: 1,
		})
		if err != nil {
			t.Fatalf("reader neighborhood of the unlabelled document: %v", err)
		}
		if !hasNode(around, w.open.Document.ID) {
			t.Fatal("the reader lost the unlabelled document")
		}
		if neighborhoodHasEdge(around, edge.ID) || hasNode(around, w.closed.Document.ID) {
			t.Fatalf("the copied-down edge reached a reader without the label: %+v", around)
		}
		seen, err := w.client.Neighborhood(holder, explorer.NeighborhoodRequest{
			NodeIDs: []shoal.ID{w.open.Document.ID}, Depth: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !neighborhoodHasEdge(seen, edge.ID) {
			t.Fatalf("the label holder does not see the edge: %+v", seen)
		}
		w.assertNoDeclassification(t)
	})
}

// TestCopyDownInteractionStaysBoundToItsEvidence: a label holder records an
// interaction over labelled evidence under an output restriction the reader
// satisfies but which lacks the evidence's label. The stored restriction is
// shown to be weaker than one carrying the label, and an interaction over
// unlabelled evidence under the same restriction is readable, so the reader
// is refused by the evidence binding alone, with the same not_found as for
// an interaction never written.
func TestCopyDownInteractionStaysBoundToItsEvidence(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		w := newCopyDownWorld(t, store)
		snapshot, err := w.f.base.Snapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		w.f.clock.Set(snapshot.AsOf.Add(time.Second))
		recorder := w.f.decision(t, "recorder",
			[][]byte{w.f.sourceA, w.f.sourceB},
			[][]byte{w.f.policyA, w.f.policyB, labelPolicy(t, w.f.sourceA, "secret")},
			labelReaderOperations)
		fingerprint, err := auth.AuthorizationFingerprint(recorder)
		if err != nil {
			t.Fatal(err)
		}
		sourcePolicy, err := auth.NewPolicy(auth.PolicyConfig{
			AuthorizationDomain: w.f.domain, SourceID: w.f.sourceA,
			GrantPolicyID: w.f.policyA, Epoch: recorder.PolicyGeneration(),
		})
		if err != nil {
			t.Fatal(err)
		}
		// Structured terms of the source policy, which every reader here
		// holds: a non-empty output restriction without the label.
		sourceTerms, err := sourcePolicy.VisibilityTerms()
		if err != nil {
			t.Fatal(err)
		}
		record := func(name string, seed shoal.ID, required []string) shoal.ID {
			t.Helper()
			session := interaction.Session{
				ID:                       interaction.DerivedID("session", name),
				RecordedAt:               w.f.clock.Now(),
				SnapshotID:               shoal.ID(snapshot.ID),
				SnapshotAsOf:             snapshot.AsOf,
				AuthorizationFingerprint: shoal.ID(fingerprint.String()),
				AuthorizationExpiresAt:   recorder.AuthenticationExpires(),
				SeedNodeIDs:              []shoal.ID{seed},
				RequiredVisibility:       append([]string(nil), required...),
			}
			if err := w.client.RecordInteraction(w.f.context(t, recorder), session); err != nil {
				t.Fatalf("record %s: %v", name, err)
			}
			return session.ID
		}
		copied := record("copy-down", w.closed.Document.ID, sourceTerms)
		carrying := record("carrying", w.closed.Document.ID, []string{"secret"})
		control := record("open-control", w.open.Document.ID, sourceTerms)

		holder := w.f.labelIngester(t, "holder", "secret")
		stored := func(id shoal.ID) []string {
			t.Helper()
			got, err := w.client.InteractionRecord(holder, id)
			if err != nil {
				t.Fatalf("the label holder cannot read %s: %v", id, err)
			}
			return got.Session.RequiredVisibility
		}
		copiedTerms, carryingTerms := stored(copied), stored(carrying)
		if len(copiedTerms) == 0 {
			t.Fatal("the copy-down interaction stored no output restriction")
		}
		held := make(map[string]bool, len(copiedTerms))
		for _, term := range copiedTerms {
			held[term] = true
		}
		missing := 0
		for _, term := range carryingTerms {
			if !held[term] {
				missing++
			}
		}
		if len(carryingTerms) == 0 || missing == 0 {
			t.Fatalf("stored restriction %v is not weaker than the evidence label's %v",
				copiedTerms, carryingTerms)
		}

		reader := w.f.labelIngester(t, "reader")
		if _, err := w.client.InteractionRecord(reader, control); err != nil {
			t.Fatalf("the reader cannot satisfy the output restriction itself: %v", err)
		}
		_, err = w.client.InteractionRecord(reader, copied)
		_, never := w.client.InteractionRecord(reader, interaction.DerivedID("session", "never"))
		if !shoal.IsErrorCode(err, shoal.ErrorNotFound) ||
			!shoal.IsErrorCode(never, shoal.ErrorNotFound) || err.Error() != never.Error() {
			t.Fatalf("reader without the label: %v, never written: %v", err, never)
		}
		listed, err := w.client.Interactions(reader)
		if err != nil {
			t.Fatal(err)
		}
		if len(listed) != 1 || listed[0].SessionID != control {
			t.Fatalf("the reader lists %+v, want only the unlabelled control", listed)
		}
		w.assertNoDeclassification(t)
	})
}

// TestCopyDownExtractionCarriesItsSourceRule: what is extracted from a
// labelled document is registered under that document's rule, so it is never
// weaker, and none of it is served to a reader without the label.
func TestCopyDownExtractionCarriesItsSourceRule(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		f := newFixture(t)
		client := f.declassificationClient(t, store, true)
		holder := f.labelIngester(t, "holder", "secret")
		source, err := client.Ingest(holder, explorer.Source{
			URI: "file:///copy-down/SKILL.md", MediaType: explorer.MediaTypeMarkdown,
			Content:  authorizedSkillMarkdown,
			Metadata: shoal.Metadata{interaction.PropertyVisibility: "secret"},
		})
		if err != nil {
			t.Fatal(err)
		}
		extracted, err := client.ExtractDocument(holder, explorer.ExtractionRequest{
			DocumentID: source.Document.ID, RevisionID: source.Revision.ID,
			Version: authorizedSkillsOntologyVersion(t),
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(extracted.EntityNodeIDs) == 0 || len(extracted.GraphEdges) == 0 {
			t.Fatalf("extraction produced no entity or relation: %+v", extracted)
		}
		current, ok, err := store.CurrentRevision(context.Background(), source.Document.ID)
		if err != nil || !ok {
			t.Fatalf("current revision = %v, %v", ok, err)
		}
		for _, id := range extracted.EntityNodeIDs {
			registration, ok, err := store.Node(context.Background(), id)
			if err != nil || !ok {
				t.Fatalf("entity %s registration = %v, %v", id, ok, err)
			}
			if registration.Rule.String() != current.Rule.String() {
				t.Fatalf("entity %s rule %v, want the source's %v",
					id, registration.Rule, current.Rule)
			}
		}
		for _, edge := range extracted.GraphEdges {
			registration, ok, err := store.Edge(context.Background(), edge.ID)
			if err != nil || !ok {
				t.Fatalf("relation %s registration = %v, %v", edge.ID, ok, err)
			}
			if registration.Rule.String() != current.Rule.String() {
				t.Fatalf("relation %s rule %v, want the source's %v",
					edge.ID, registration.Rule, current.Rule)
			}
		}
		reader := f.labelIngester(t, "reader")
		if _, err := client.Neighborhood(reader, explorer.NeighborhoodRequest{
			NodeIDs: []shoal.ID{extracted.EntityNodeIDs[0]}, Depth: 1,
		}); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			t.Fatalf("reader without the label seeded on an extracted entity: %v", err)
		}
		if records := declassifications(t, store); len(records) != 0 {
			t.Fatalf("extraction was recorded as a declassification: %+v", records)
		}
		// Unlike an interaction or an edge (#695), what was extracted from
		// the labelled revision stays closed when the document is relabelled
		// to no label: its own rule is conjoined with the current one.
		if _, err := client.Ingest(withReason(t, holder, "ticket:SEC-663"), explorer.Source{
			URI: "file:///copy-down/SKILL.md", MediaType: explorer.MediaTypeMarkdown,
			Content: authorizedSkillMarkdown + "\nNow public.\n",
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := client.Neighborhood(reader, explorer.NeighborhoodRequest{
			NodeIDs: []shoal.ID{extracted.EntityNodeIDs[0]}, Depth: 1,
		}); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			t.Fatalf("a relabel opened what was extracted from the labelled revision: %v", err)
		}
		if records := declassifications(t, store); len(records) != 2 {
			t.Fatalf("relabel records = %+v, want its intent and pre_commit only", records)
		}
	})
}
