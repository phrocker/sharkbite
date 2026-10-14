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

// Shared extracted entities and relations (#700). Whether a reader sees an
// extracted node or relation must not depend on documents that reader cannot
// read: a reader who can read any document that extracted it sees it, and a
// reader who can read none does not.

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	sharedHiddenURI   = "file:///shared/hidden/SKILL.md"
	sharedAttackerURI = "file:///shared/attacker/SKILL.md"
	sharedControlURI  = "file:///shared/control/SKILL.md"
)

func (w *extractedEdgeWorld) ingestAs(
	ctx context.Context, uri, content string, metadata shoal.Metadata,
) explorer.IngestResult {
	w.t.Helper()
	result, err := w.client.Ingest(ctx, explorer.Source{
		URI: uri, MediaType: explorer.MediaTypeMarkdown,
		Content: content, Metadata: metadata,
	})
	if err != nil {
		w.t.Fatal(err)
	}
	return result
}

func (w *extractedEdgeWorld) extractAs(
	ctx context.Context, result explorer.IngestResult,
) (explorer.ExtractionResult, error) {
	return w.client.ExtractDocument(ctx, explorer.ExtractionRequest{
		DocumentID: result.Document.ID, RevisionID: result.Revision.ID,
		Version: authorizedSkillsOntologyVersion(w.t),
	})
}

func (w *extractedEdgeWorld) relabelSecret(uri, body string) {
	w.t.Helper()
	w.ingest(uri, body+"\nNow secret.\n", shoal.Metadata{"shoal.visibility": "secret"})
}

// nodeVisible reports whether ctx can read nodeID through the graph.
func nodeVisible(t *testing.T, client *authorized.Client, ctx context.Context, nodeID shoal.ID) bool {
	t.Helper()
	around, err := client.Neighborhood(ctx, explorer.NeighborhoodRequest{
		NodeIDs: []shoal.ID{nodeID}, Depth: 1,
	})
	if err != nil {
		if shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			return false
		}
		t.Fatal(err)
	}
	for _, node := range around.Nodes {
		if node.ID == nodeID {
			return true
		}
	}
	return false
}

func visibleCount(t *testing.T, client *authorized.Client, ctx context.Context, ids []shoal.ID) int {
	t.Helper()
	count := 0
	for _, id := range ids {
		if nodeVisible(t, client, ctx, id) {
			count++
		}
	}
	return count
}

// relationVisible is edgeVisible, counting an unreadable source endpoint as
// a hidden relation.
func (w *extractedEdgeWorld) relationVisible(ctx context.Context, from, edgeID shoal.ID) bool {
	w.t.Helper()
	if !nodeVisible(w.t, w.client, ctx, from) {
		return false
	}
	return w.edgeVisible(ctx, from, edgeID)
}

func relationOf(extracted explorer.ExtractionResult) (shoal.ID, shoal.ID) {
	for _, edge := range extracted.GraphEdges {
		if edge.Type == "provides_tool" {
			return edge.ID, edge.From
		}
	}
	return "", ""
}

// TestSharedEntityVisibleThroughAnyOwner is the issue's repro: a public
// document extracts "zeta" and is then relabelled secret. An attacker without
// the label extracts its own document naming "zeta". It must see all of its
// own extracted nodes and relations, exactly as for an entity no hidden
// document names, and the label holder keeps seeing them.
func TestSharedEntityVisibleThroughAnyOwner(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		w := newExtractedEdgeWorld(t, store)
		body := skillBody("zeta", "zeta-cli")
		hidden := w.extract(w.ingest(sharedHiddenURI, body, nil))
		w.relabelSecret(sharedHiddenURI, body)

		attacker := w.f.labelIngester(t, "attacker")
		own, err := w.extractAs(attacker, w.ingestAs(attacker, sharedAttackerURI, body, nil))
		if err != nil {
			t.Fatalf("the attacker's own extraction failed: %v", err)
		}
		if !reflect.DeepEqual(own.EntityNodeIDs, hidden.EntityNodeIDs) {
			t.Fatalf("fixture no longer shares entities: %v vs %v",
				own.EntityNodeIDs, hidden.EntityNodeIDs)
		}
		control, err := w.extractAs(attacker,
			w.ingestAs(attacker, sharedControlURI, skillBody("omega", "omega-cli"), nil))
		if err != nil {
			t.Fatal(err)
		}
		if len(own.EntityNodeIDs) != 3 || len(control.EntityNodeIDs) != 3 {
			t.Fatalf("fixture entity counts = %d, %d; want 3 each",
				len(own.EntityNodeIDs), len(control.EntityNodeIDs))
		}
		reader := w.f.labelIngester(t, "attacker")
		if got := visibleCount(t, w.client, reader, own.EntityNodeIDs); got != 3 {
			t.Fatalf("attacker sees %d/3 of its own extracted nodes (control: %d/3)",
				got, visibleCount(t, w.client, reader, control.EntityNodeIDs))
		}
		if got := visibleCount(t, w.client, reader, control.EntityNodeIDs); got != 3 {
			t.Fatalf("attacker sees %d/3 of the control nodes", got)
		}
		relation, from := relationOf(own)
		if relation == "" {
			t.Fatal("fixture produces no relation")
		}
		if hiddenRelation, _ := relationOf(hidden); hiddenRelation != relation {
			t.Fatalf("fixture no longer shares the relation: %s vs %s", relation, hiddenRelation)
		}
		if !w.edgeVisible(reader, from, relation) {
			t.Fatal("attacker lost its own extracted relation to a hidden asserter")
		}
		holder := w.f.labelIngester(t, "holder", "secret")
		if got := visibleCount(t, w.client, holder, own.EntityNodeIDs); got != 3 {
			t.Fatalf("label holder sees %d/3", got)
		}
		// The hidden document itself stays hidden.
		if _, err := w.client.Document(reader, hidden.DocumentID, ""); err == nil {
			t.Fatal("the attacker reads the relabelled document")
		}
	})
}

// sharedEvidenceMarker is in the hidden document only.
const sharedEvidenceMarker = "SECRETWORD"

// sharedHiddenURIs vary the hidden document, and with it which extraction
// the base attaches its one assertion of the shared relation from (the
// last, in extraction-ID order).
var sharedHiddenURIs = []string{
	"file:///shared/hidden-0/SKILL.md", "file:///shared/hidden-1/SKILL.md",
	"file:///shared/hidden-2/SKILL.md", "file:///shared/hidden-3/SKILL.md",
	"file:///shared/hidden-4/SKILL.md", "file:///shared/hidden-5/SKILL.md",
}

// sharedRelationWorld: a seed mints the entities, so the hidden document and
// the attacker's ground on them and assert the very same relation edge;
// then the hidden document is relabelled secret. It returns the relation and
// its source endpoint, and whether the base's own assertion of it is the
// hidden document's.
func sharedRelationWorld(
	t *testing.T, w *extractedEdgeWorld, hiddenURI string,
) (relation, from shoal.ID, hiddenDocument shoal.ID, baseQuotesHidden bool) {
	t.Helper()
	body := skillBody("zeta", "zeta-cli")
	hiddenBody := body + "\n" + sharedEvidenceMarker + " lives here.\n"
	w.extract(w.ingest("file:///shared/seed/SKILL.md", body, nil))
	hidden := w.extract(w.ingest(hiddenURI, hiddenBody, nil))
	attacker := w.f.labelIngester(t, "attacker")
	own, err := w.extractAs(attacker, w.ingestAs(attacker, sharedAttackerURI, body, nil))
	if err != nil {
		t.Fatal(err)
	}
	relation, from = relationOf(own)
	if again, _ := relationOf(hidden); relation == "" || again != relation {
		t.Fatalf("fixture no longer shares the relation: %s vs %s", relation, again)
	}
	w.ingest(hiddenURI, hiddenBody+"\nNow secret.\n",
		shoal.Metadata{"shoal.visibility": "secret"})
	raw, err := w.f.base.Neighborhood(context.Background(), explorer.NeighborhoodRequest{
		NodeIDs: []shoal.ID{from}, Depth: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, assertion := range raw.Assertions {
		if assertion.Metadata()["shoal.graph.edge_id"] != string(relation) {
			continue
		}
		for _, item := range assertion.Evidence() {
			if item.Citation().DocumentID == hidden.DocumentID {
				baseQuotesHidden = true
			}
		}
	}
	return relation, from, hidden.DocumentID, baseQuotesHidden
}

// readerSees returns the relation's assertions as ctx sees them around from,
// and whether the relation itself is shown, failing on hidden evidence.
func readerSees(
	t *testing.T, client *authorized.Client, ctx context.Context,
	from, relation, hiddenDocument shoal.ID,
) (shown, asserted bool) {
	t.Helper()
	around, err := client.Neighborhood(ctx, explorer.NeighborhoodRequest{
		NodeIDs: []shoal.ID{from}, Depth: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, edge := range around.Edges {
		if edge.ID == relation {
			shown = true
		}
	}
	for _, assertion := range around.Assertions {
		for _, item := range assertion.Evidence() {
			if item.Citation().DocumentID == hiddenDocument ||
				strings.Contains(item.Quote(), sharedEvidenceMarker) {
				t.Fatalf("shown the hidden document's evidence: %+v", item.Citation())
			}
		}
		if assertion.Metadata()["shoal.graph.edge_id"] == string(relation) {
			asserted = true
		}
	}
	return shown, asserted
}

// TestSharedRelationShowsOnlyReadableEvidence: the base attaches one
// assertion to a shared relation, quoting whichever document asserted it.
// A reader who sees the relation through its own document must be shown an
// assertion whose evidence it may read, never the hidden document's; the
// label holder still gets an assertion.
func TestSharedRelationShowsOnlyReadableEvidence(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		exercised := 0
		for _, uri := range sharedHiddenURIs {
			f := newFixture(t)
			selector := f.staticSelector(t, f.sourceA, f.policyA)
			var worldStore authorized.PolicyStore = authorized.NewMemoryPolicyStore()
			if _, durable := store.(*authorized.DurablePolicyStore); durable {
				opened, err := authorized.OpenDurablePolicyStore(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = opened.Close() })
				worldStore = opened
			}
			w := &extractedEdgeWorld{
				t: t, f: f,
				client: f.labelClient(t, f.base, worldStore, selector, selector),
				owner:  f.labelIngester(t, "owner", "secret"),
				reader: f.labelIngester(t, "reader"),
			}
			relation, from, hidden, baseQuotesHidden := sharedRelationWorld(t, w, uri)
			if baseQuotesHidden {
				exercised++
			}
			shown, asserted := readerSees(t, w.client,
				w.f.labelIngester(t, "attacker"), from, relation, hidden)
			if !shown || !asserted {
				t.Fatalf("%s: the attacker's own relation shown = %v, asserted = %v",
					uri, shown, asserted)
			}
			holder := w.f.labelIngester(t, "holder", "secret")
			around, err := w.client.Neighborhood(holder, explorer.NeighborhoodRequest{
				NodeIDs: []shoal.ID{from}, Depth: 1,
			})
			if err != nil {
				t.Fatal(err)
			}
			holderAsserted := false
			for _, assertion := range around.Assertions {
				if assertion.Metadata()["shoal.graph.edge_id"] == string(relation) {
					holderAsserted = true
				}
			}
			if !holderAsserted {
				t.Fatalf("%s: the label holder lost the relation's assertion", uri)
			}
		}
		if exercised == 0 {
			t.Fatal("the base never attached the hidden assertion; the test is vacuous")
		}
	})
}

// TestRecordingOverASharedRelationCitesTheReadersAssertion: a session whose
// graph evidence is a shared relation is recorded with the assertion its
// recorder was shown, even when the base attaches a hidden document's
// assertion to the edge; an assertion nobody made is still refused.
func TestRecordingOverASharedRelationCitesTheReadersAssertion(t *testing.T) {
	exercised := 0
	for index, uri := range sharedHiddenURIs {
		f := newFixture(t)
		selector := f.staticSelector(t, f.sourceA, f.policyA)
		w := &extractedEdgeWorld{
			t: t, f: f,
			client: f.labelClient(t, f.base, authorized.NewMemoryPolicyStore(), selector, selector),
			owner:  f.labelIngester(t, "owner", "secret"),
			reader: f.labelIngester(t, "reader"),
		}
		relation, from, hidden, baseQuotesHidden := sharedRelationWorld(t, w, uri)
		if !baseQuotesHidden {
			continue
		}
		exercised++
		alternatives, err := f.base.ExtractionAssertions(context.Background(), []shoal.ID{relation})
		if err != nil {
			t.Fatal(err)
		}
		var own, theirs []interaction.AssertionReference
		for _, assertion := range alternatives[relation] {
			reference := []interaction.AssertionReference{{
				AssertionID: assertion.ID(), EdgeID: relation, Origin: assertion.Origin(),
			}}
			if assertion.Evidence()[0].Citation().DocumentID == hidden {
				theirs = reference
			} else {
				own = reference
			}
		}
		if own == nil || theirs == nil {
			t.Fatalf("%s: alternatives = %d", uri, len(alternatives[relation]))
		}
		var to shoal.ID
		raw, err := f.base.Neighborhood(context.Background(), explorer.NeighborhoodRequest{
			NodeIDs: []shoal.ID{from}, Depth: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, edge := range raw.Edges {
			if edge.ID == relation {
				to = edge.To
			}
		}
		edge := graph.Edge{ID: relation, From: from, To: to}
		record := func(id shoal.ID, assertions []interaction.AssertionReference) error {
			snapshot, err := w.client.Snapshot(w.f.labelIngester(t, "attacker"))
			if err != nil {
				t.Fatal(err)
			}
			f.clock.Set(snapshot.AsOf.Add(time.Second))
			decision := f.bindingDecision(t, "attacker")
			fingerprint, err := auth.AuthorizationFingerprint(decision)
			if err != nil {
				t.Fatal(err)
			}
			evidence := exactAuthorizedGraphEvidenceWith(t, f.base, edge, assertions)
			_, err = w.client.RecordInteractionResult(f.context(t, decision), interaction.Session{
				ID:                       id,
				RecordedAt:               f.clock.Now().UTC(),
				Operation:                interaction.OperationRetrieval,
				SnapshotID:               shoal.ID(snapshot.ID),
				SnapshotAsOf:             snapshot.AsOf,
				AuthorizationFingerprint: shoal.ID(fingerprint.String()),
				AuthorizationExpiresAt:   decision.AuthenticationExpires(),
				SeedNodeIDs:              evidence.NodeIDs,
				SeedEvidence:             []interaction.EvidenceReference{evidence},
			})
			return err
		}
		if err := record(shoal.ID(fmt.Sprintf("interaction.session_shared_own_%d", index)), own); err != nil {
			t.Fatalf("%s: recording with the reader's own assertion: %v", uri, err)
		}
		never := []interaction.AssertionReference{{
			AssertionID: "assertion:never-made", EdgeID: relation, Origin: theirs[0].Origin,
		}}
		if err := record(shoal.ID(fmt.Sprintf("interaction.session_shared_never_%d", index)), never); err == nil {
			t.Fatalf("%s: recorded a session citing an assertion nobody made", uri)
		}
		if err := record(shoal.ID(fmt.Sprintf("interaction.session_shared_none_%d", index)), nil); err == nil {
			t.Fatalf("%s: recorded a session omitting the relation's assertion", uri)
		}
		both := append(append([]interaction.AssertionReference(nil), own...), theirs...)
		if err := record(shoal.ID(fmt.Sprintf("interaction.session_shared_both_%d", index)), both); err == nil {
			t.Fatalf("%s: recorded a session citing two assertions of one edge", uri)
		}
	}
	if exercised == 0 {
		t.Fatal("the base never attached the hidden assertion; the test is vacuous")
	}
}

// opaqueValidator exposes only ValidateSnapshot, so NewClient promotes no
// trusted reader from it.
type opaqueValidator struct{ authorized.SnapshotValidator }

// TestSharedRelationWithoutAReadableAssertionIsWithheld: without the
// trusted extraction-assertion reader, a shared relation whose base
// assertion quotes a document the reader cannot read is withheld, never
// shown with that evidence.
func TestSharedRelationWithoutAReadableAssertionIsWithheld(t *testing.T) {
	exercised := 0
	for _, uri := range sharedHiddenURIs {
		f := newFixture(t)
		selector := f.staticSelector(t, f.sourceA, f.policyA)
		client, err := authorized.NewClient(authorized.Config{
			Base:                   f.base,
			VectorScorer:           trustedVectorScorer(f.base),
			InteractionWriter:      trustedInteractionWriter(f.base),
			InteractionReader:      trustedInteractionReader(f.base),
			SnapshotValidator:      opaqueValidator{trustedSnapshotValidator(f.base)},
			DerivedAssertionReader: trustedDerivedAssertionReader(f.base),
			FoldStore:              trustedFoldStore(f.base),
			Resolver:               f.authority.Resolver(),
			PolicySelector:         selector,
			EdgePolicySelector:     selector,
			PolicyStore:            authorized.NewMemoryPolicyStore(),
			GenerationReader:       f.reader,
			Clock:                  f.clock.Now,
		})
		if err != nil {
			t.Fatal(err)
		}
		w := &extractedEdgeWorld{
			t: t, f: f, client: client,
			owner:  f.labelIngester(t, "owner", "secret"),
			reader: f.labelIngester(t, "reader"),
		}
		relation, from, hidden, baseQuotesHidden := sharedRelationWorld(t, w, uri)
		shown, _ := readerSees(t, w.client,
			w.f.labelIngester(t, "attacker"), from, relation, hidden)
		if baseQuotesHidden {
			exercised++
			if shown {
				t.Fatalf("%s: shown a relation with no assertion the reader may read", uri)
			}
		} else if !shown {
			t.Fatalf("%s: withheld a relation whose assertion the reader may read", uri)
		}
	}
	if exercised == 0 {
		t.Fatal("the base never attached the hidden assertion; the test is vacuous")
	}
}

// TestExtractionContextOmitsUnreadableEntities: an extraction grounds only
// on entities its principal may read, so what it reports (entity and
// relation IDs, created and reused counts) is the same whether or not a
// document the principal cannot read names the same entities.
func TestExtractionContextOmitsUnreadableEntities(t *testing.T) {
	type outcome struct {
		entities, relations []shoal.ID
		created, reused     int
		visible             int
	}
	run := func(t *testing.T, store authorized.PolicyStore, withHidden bool) outcome {
		w := newExtractedEdgeWorld(t, store)
		body := skillBody("zeta", "zeta-cli")
		if withHidden {
			w.extract(w.ingest(sharedHiddenURI, body, nil))
			w.relabelSecret(sharedHiddenURI, body)
		}
		attacker := w.f.labelIngester(t, "attacker")
		own, err := w.extractAs(attacker, w.ingestAs(attacker, sharedAttackerURI, body, nil))
		if err != nil {
			t.Fatalf("extraction (hidden document: %v): %v", withHidden, err)
		}
		return outcome{
			entities: own.EntityNodeIDs, relations: own.RelationshipEdgeIDs,
			created: own.CreatedEntities, reused: own.ReusedEntities,
			visible: visibleCount(t, w.client, attacker, own.EntityNodeIDs),
		}
	}
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		with := run(t, store, true)
		var fresh authorized.PolicyStore = authorized.NewMemoryPolicyStore()
		if _, durable := store.(*authorized.DurablePolicyStore); durable {
			opened, err := authorized.OpenDurablePolicyStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = opened.Close() })
			fresh = opened
		}
		without := run(t, fresh, false)
		if !reflect.DeepEqual(with, without) {
			t.Fatalf("the attacker's extraction depends on a hidden document:\nwith    %+v\nwithout %+v",
				with, without)
		}
		if with.reused != 0 {
			t.Fatalf("the extraction reused %d hidden entities", with.reused)
		}
	})
}

// TestSharedEntityVisibilityFollowsItsOwners is the multi-owner truth table
// through the client: a seed document mints the entities, D1 and D2 both
// extract them and assert the same relation, and the seed is relabelled. A
// reader without the label sees the entities and the relation iff D1 or D2
// is still unlabelled; the label holder always does.
func TestSharedEntityVisibilityFollowsItsOwners(t *testing.T) {
	for _, row := range []struct {
		name               string
		relabel1, relabel2 bool
		readerSees         bool
	}{
		{"neither relabelled", false, false, true},
		{"first relabelled", true, false, true},
		{"second relabelled", false, true, true},
		{"both relabelled", true, true, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
				w := newExtractedEdgeWorld(t, store)
				body := skillBody("gamma", "gamma-cli")
				const seedURI = "file:///shared/seed/SKILL.md"
				const firstURI = "file:///shared/first/SKILL.md"
				const secondURI = "file:///shared/second/SKILL.md"
				w.extract(w.ingest(seedURI, body, nil))
				first := w.extract(w.ingest(firstURI, body, nil))
				second := w.extract(w.ingest(secondURI, body, nil))
				relation, from := relationOf(first)
				if again, _ := relationOf(second); relation == "" || again != relation {
					t.Fatalf("fixture: the documents assert different relations: %s vs %s", relation, again)
				}
				w.relabelSecret(seedURI, body)
				if row.relabel1 {
					w.relabelSecret(firstURI, body)
				}
				if row.relabel2 {
					w.relabelSecret(secondURI, body)
				}
				reader := w.f.labelIngester(t, "reader")
				want := 0
				if row.readerSees {
					want = len(first.EntityNodeIDs)
				}
				if got := visibleCount(t, w.client, reader, first.EntityNodeIDs); got != want {
					t.Fatalf("reader sees %d entities, want %d", got, want)
				}
				if got := w.relationVisible(reader, from, relation); got != row.readerSees {
					t.Fatalf("reader sees the shared relation = %v, want %v", got, row.readerSees)
				}
				holder := w.f.labelIngester(t, "holder", "secret")
				if got := visibleCount(t, w.client, holder, first.EntityNodeIDs); got != len(first.EntityNodeIDs) {
					t.Fatalf("holder sees %d entities", got)
				}
				if !w.edgeVisible(holder, from, relation) {
					t.Fatal("holder lost the shared relation")
				}
			})
		})
	}
}

// TestSharedEntitySurvivesALabelMigration: a document labelled before labels
// were enforced extracts "zeta", and the startup migration tightens it. An
// attacker's extraction of "zeta" must then succeed, not fail with the
// ObjectNotFound the conflicting rule used to raise, and show all of its
// nodes. The migrated document, extracted again by its holder, stays an
// owner.
func TestSharedEntitySurvivesALabelMigration(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		legacy := &legacyStore{PolicyStore: store}
		w := newExtractedEdgeWorld(t, legacy)
		body := skillBody("zeta", "zeta-cli")
		legacy.setStrip(true)
		labelled := w.ingest(sharedHiddenURI, body, shoal.Metadata{"shoal.visibility": "secret"})
		hidden := w.extract(labelled)
		legacy.setStrip(false)
		if record := migrate(t, w.client); record.Tightened != 1 {
			t.Fatalf("migration = %+v", record)
		}
		attacker := w.f.labelIngester(t, "attacker")
		if nodeVisible(t, w.client, attacker, hidden.EntityNodeIDs[0]) {
			t.Fatal("the migration did not close the labelled document's entities")
		}
		own, err := w.extractAs(attacker, w.ingestAs(attacker, sharedAttackerURI, body, nil))
		if err != nil {
			t.Fatalf("the attacker's extraction failed after the migration: %v", err)
		}
		if !reflect.DeepEqual(own.EntityNodeIDs, hidden.EntityNodeIDs) {
			t.Fatal("fixture no longer shares entities")
		}
		if got := visibleCount(t, w.client, attacker, own.EntityNodeIDs); got != 3 {
			t.Fatalf("attacker sees %d/3 of its own nodes after the migration", got)
		}
		holder := w.f.labelIngester(t, "holder", "secret")
		if _, err := w.extractAs(holder, labelled); err != nil {
			t.Fatalf("re-extracting the migrated document: %v", err)
		}
	})
}

// TestSharedOwnerAddedLaterIsClosedByTheMigration: when the legacy-labelled
// document is the LATER extractor of a shared entity, the migration tightens
// its co-ownership, so relabelling the public first owner then hides the
// entity from readers without the label.
func TestSharedOwnerAddedLaterIsClosedByTheMigration(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		legacy := &legacyStore{PolicyStore: store}
		w := newExtractedEdgeWorld(t, legacy)
		body := skillBody("zeta", "zeta-cli")
		public := w.extract(w.ingest(sharedAttackerURI, body, nil))
		legacy.setStrip(true)
		w.extract(w.ingest(sharedHiddenURI, body, shoal.Metadata{"shoal.visibility": "secret"}))
		legacy.setStrip(false)
		migrate(t, w.client)
		reader := w.f.labelIngester(t, "reader")
		if got := visibleCount(t, w.client, reader, public.EntityNodeIDs); got != 3 {
			t.Fatalf("reader sees %d/3 through the public owner", got)
		}
		w.relabelSecret(sharedAttackerURI, body)
		reader = w.f.labelIngester(t, "reader")
		if got := visibleCount(t, w.client, reader, public.EntityNodeIDs); got != 0 {
			t.Fatalf("reader still sees %d/3 through a migrated, labelled co-owner", got)
		}
		holder := w.f.labelIngester(t, "holder", "secret")
		if got := visibleCount(t, w.client, holder, public.EntityNodeIDs); got != 3 {
			t.Fatalf("holder sees %d/3", got)
		}
	})
}

// TestSessionOverASharedEntityBindsTheRecordersOwner: a session the attacker
// records over its own extracted node is bound to the attacker's revision,
// not to the hidden first extractor's, so the attacker reads it back.
func TestSessionOverASharedEntityBindsTheRecordersOwner(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		w := newExtractedEdgeWorld(t, store)
		body := skillBody("zeta", "zeta-cli")
		w.extract(w.ingest(sharedHiddenURI, body, nil))
		w.relabelSecret(sharedHiddenURI, body)
		attacker := w.f.labelIngester(t, "attacker")
		own, err := w.extractAs(attacker, w.ingestAs(attacker, sharedAttackerURI, body, nil))
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := w.client.Snapshot(attacker)
		if err != nil {
			t.Fatal(err)
		}
		w.f.clock.Set(snapshot.AsOf.Add(time.Second))
		decision := w.f.bindingDecision(t, "attacker")
		fingerprint, err := auth.AuthorizationFingerprint(decision)
		if err != nil {
			t.Fatal(err)
		}
		const session shoal.ID = "interaction.session_shared_entity"
		ctx := w.f.context(t, decision)
		if _, err := w.client.RecordInteractionResult(ctx, interaction.Session{
			ID:                       session,
			RecordedAt:               w.f.clock.Now().UTC(),
			Operation:                interaction.OperationRetrieval,
			SnapshotID:               shoal.ID(snapshot.ID),
			SnapshotAsOf:             snapshot.AsOf,
			AuthorizationFingerprint: shoal.ID(fingerprint.String()),
			AuthorizationExpiresAt:   decision.AuthenticationExpires(),
			SeedNodeIDs:              own.EntityNodeIDs[:1],
		}); err != nil {
			t.Fatalf("record: %v", err)
		}
		if _, err := w.client.InteractionRecord(ctx, session); err != nil {
			t.Fatalf("the attacker cannot read its own session over its own entity: %v", err)
		}
	})
}

// TestSharedOwnersSurviveReopen: co-ownership is durable.
func TestSharedOwnersSurviveReopen(t *testing.T) {
	dir := t.TempDir()
	first, err := authorized.OpenDurablePolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	w := newExtractedEdgeWorld(t, first)
	body := skillBody("zeta", "zeta-cli")
	w.extract(w.ingest(sharedHiddenURI, body, nil))
	own := w.extract(w.ingest(sharedAttackerURI, body, nil))
	w.relabelSecret(sharedHiddenURI, body)
	relation, from := relationOf(own)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := authorized.OpenDurablePolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	selector := w.f.staticSelector(t, w.f.sourceA, w.f.policyA)
	w.client = w.f.labelClient(t, w.f.base, reopened, selector, selector)
	reader := w.f.labelIngester(t, "reader")
	if got := visibleCount(t, w.client, reader, own.EntityNodeIDs); got != 3 {
		t.Fatalf("reader sees %d/3 after reopen", got)
	}
	if !w.edgeVisible(reader, from, relation) {
		t.Fatal("reader lost the shared relation after reopen")
	}
	w.relabelSecret(sharedAttackerURI, body)
	reader = w.f.labelIngester(t, "reader")
	if got := visibleCount(t, w.client, reader, own.EntityNodeIDs); got != 0 {
		t.Fatalf("reader sees %d/3 after every owner was relabelled", got)
	}
}
