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

// The startup label migration (#570, PR4). Documents labelled before labels
// were enforced carry the bare source rule in the catalog and the free-form
// shoal.visibility property on their nodes, so every holder of the source
// reads them. These tests build exactly that state, run the migration, and
// check that it closes the documents on every read path.

import (
	"context"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/phrocker/shoal-oss/internal/labelmigration"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// legacyStore wraps a policy store. While strip is on it registers every rule
// without its label policies, which is the catalog a pre-#570 build wrote:
// the base still records shoal.visibility on the nodes, but the rule is the
// bare source rule. It can also fail TightenRule on a chosen call, to
// interrupt a migration, and hide the migration marker, to force a rerun.
type legacyStore struct {
	authorized.PolicyStore
	mu            sync.Mutex
	strip         bool
	keepRevisions bool
	// legacyEdgeKinds registers extracted relations the way a store written
	// before RegistrationKind decodes them.
	legacyEdgeKinds bool
	// failPutRevisions fails that many PutRevision calls, leaving the base
	// one revision ahead of the catalog.
	failPutRevisions int
	failTightenAt    int
	tightenCalls     int
	tightenChange    int
}

func (s *legacyStore) setStrip(strip bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.strip = strip
}

func (s *legacyStore) legacy(rule authorized.AccessRule) authorized.AccessRule {
	s.mu.Lock()
	strip := s.strip
	s.mu.Unlock()
	if !strip {
		return rule
	}
	bare, err := authorized.StripLabelPolicies(rule)
	if err != nil {
		panic(err)
	}
	return bare
}

func (s *legacyStore) PutRevision(ctx context.Context, registration authorized.RevisionRegistration) error {
	s.mu.Lock()
	keep := s.keepRevisions
	fail := s.failPutRevisions > 0
	if fail {
		s.failPutRevisions--
	}
	s.mu.Unlock()
	if fail {
		return shoal.NewError(shoal.ErrorUnavailable, "injected registration failure")
	}
	if !keep {
		registration.Rule = s.legacy(registration.Rule)
	}
	return s.PolicyStore.PutRevision(ctx, registration)
}

func (s *legacyStore) PutNode(ctx context.Context, id shoal.ID, registration authorized.NodeRegistration) error {
	registration.Rule = s.legacy(registration.Rule)
	return s.PolicyStore.PutNode(ctx, id, registration)
}

func (s *legacyStore) ReserveEdge(ctx context.Context, registration authorized.EdgeRegistration) error {
	registration.Rule = s.legacy(registration.Rule)
	return s.PolicyStore.ReserveEdge(ctx, registration)
}

func (s *legacyStore) RollbackEdgeReservation(ctx context.Context, registration authorized.EdgeRegistration) error {
	registration.Rule = s.legacy(registration.Rule)
	return s.PolicyStore.RollbackEdgeReservation(ctx, registration)
}

func (s *legacyStore) PutEdge(ctx context.Context, registration authorized.EdgeRegistration) error {
	registration.Rule = s.legacy(registration.Rule)
	s.mu.Lock()
	legacyKinds := s.legacyEdgeKinds
	s.mu.Unlock()
	if legacyKinds && registration.Kind == authorized.RegistrationExtracted {
		// What a record persisted before RegistrationKind decodes to: an
		// application edge naming no document (legacyEdgeKind).
		registration.Kind = authorized.RegistrationApplication
		registration.DocumentID, registration.RevisionID = "", ""
	}
	return s.PolicyStore.PutEdge(ctx, registration)
}

func (s *legacyStore) CompareAndSwapSourceClaim(
	ctx context.Context, uri string, expected *authorized.SourcePolicyClaim, desired authorized.AccessRule,
) (authorized.SourcePolicyClaim, error) {
	return s.PolicyStore.CompareAndSwapSourceClaim(ctx, uri, expected, s.legacy(desired))
}

func (s *legacyStore) TightenRule(
	ctx context.Context, index *authorized.TighteningIndex, tightening authorized.RuleTightening,
) (bool, error) {
	s.mu.Lock()
	s.tightenCalls++
	fail := s.failTightenAt != 0 && s.tightenCalls == s.failTightenAt
	s.mu.Unlock()
	if fail {
		return false, shoal.NewError(shoal.ErrorUnavailable, "injected catalog failure")
	}
	changed, err := s.PolicyStore.TightenRule(ctx, index, tightening)
	if changed {
		s.mu.Lock()
		s.tightenChange++
		s.mu.Unlock()
	}
	return changed, err
}

func migrate(t *testing.T, client *authorized.Client) authorized.LabelMigrationRecord {
	t.Helper()
	record, err := client.MigrateLabelledDocuments(
		context.Background(), labelmigration.NewCapability())
	if err != nil {
		t.Fatalf("migration: %v", err)
	}
	return record
}

// newLegacyLabelWorld builds the conformance world of label_conformance_test
// the way a pre-#570 build would have left it.
func newLegacyLabelWorld(t *testing.T, store authorized.PolicyStore) (*labelWorld, *legacyStore) {
	t.Helper()
	legacy := &legacyStore{PolicyStore: store}
	legacy.setStrip(true)
	w := newLabelWorld(t, legacy)
	legacy.setStrip(false)
	return w, legacy
}

func runLabelConformanceTable(t *testing.T, w *labelWorld) {
	t.Helper()
	for _, row := range labelRows() {
		t.Run(row.name, func(t *testing.T) {
			for _, c := range labelCases {
				got := row.probe(t, w, c.reader.context(w, t), w.targets[c.target])
				if got != c.want {
					t.Errorf("%s: reader %q on %q visible = %v, want %v",
						row.method, c.reader.name, c.target, got, c.want)
				}
			}
		})
	}
}

func sourceOnlySees(t *testing.T, w *labelWorld, id shoal.ID) bool {
	t.Helper()
	summaries, err := w.clientA.Documents(w.sourceOnly(t))
	if err != nil {
		t.Fatal(err)
	}
	return summariesContain(summaries, id)
}

// TestLabelMigrationClosesLegacyDocumentsOnEveryReadPath is the required
// probe: documents labelled the old way are readable by a source-only
// principal before the migration, and after it every row of the label
// conformance table holds, on both stores.
func TestLabelMigrationClosesLegacyDocumentsOnEveryReadPath(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		w, _ := newLegacyLabelWorld(t, store)
		secret := w.targets["secret"].current.Document.ID
		// The #570 leak, reproduced: the legacy rule is the bare source rule.
		if !sourceOnlySees(t, w, secret) {
			t.Fatal("the legacy world does not reproduce the leak; the probe would be vacuous")
		}
		record := migrate(t, w.clientA)
		if record.Version != authorized.LabelMigrationVersion {
			t.Fatalf("migration did not run: %+v", record)
		}
		// secret, both and secretB are labelled; control and the hub are not.
		if record.Tightened != 3 || len(record.Untranslatable) != 0 ||
			record.Unregistered != 0 || record.Unlabelled != record.Documents-3 {
			t.Fatalf("migration report = %+v", record)
		}
		runLabelConformanceTable(t, w)
	})
}

// TestLabelMigrationIsIdempotent pins that a second run is a no-op: with the
// marker it does nothing at all, and even with the marker hidden it rewrites
// nothing.
func TestLabelMigrationIsIdempotent(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		w, legacy := newLegacyLabelWorld(t, store)
		first := migrate(t, w.clientA)
		if first.Tightened != 3 {
			t.Fatalf("first migration = %+v", first)
		}
		legacy.mu.Lock()
		legacy.tightenChange = 0
		legacy.mu.Unlock()
		// Every start runs it again; the second run finds everything tight.
		third := migrate(t, w.clientA)
		if third.Tightened != 0 || third.AlreadyTightened != first.Tightened ||
			third.HistoricalTightened != 0 {
			t.Fatalf("rerun without the marker = %+v", third)
		}
		if legacy.tightenChange != 0 {
			t.Fatalf("rerun rewrote %d documents, want none", legacy.tightenChange)
		}
		runLabelConformanceTable(t, w)
	})
}

// TestLabelMigrationResumesAfterInterruption fails the catalog partway
// through a run. The marker must not be written, the next run must finish the
// job, and every row must then hold.
func TestLabelMigrationResumesAfterInterruption(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		w, legacy := newLegacyLabelWorld(t, store)
		legacy.mu.Lock()
		legacy.failTightenAt = 2
		legacy.mu.Unlock()
		_, err := w.clientA.MigrateLabelledDocuments(
			context.Background(), labelmigration.NewCapability())
		if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
			t.Fatalf("interrupted migration err = %v, want the injected failure", err)
		}
		if _, ok, err := store.LabelMigration(context.Background()); err != nil || ok {
			t.Fatalf("marker after an interrupted run = %v, %v; it must be written last", ok, err)
		}
		// The interruption left at least one labelled document open.
		open := 0
		for _, name := range []string{"secret", "both", "secretB"} {
			target := w.targets[name]
			if _, err := w.clientA.Document(w.sourceOnly(t), target.current.Document.ID,
				target.current.Revision.ID); err == nil {
				open++
			}
		}
		if open == 0 {
			t.Fatal("the interruption closed everything; the resume would be vacuous")
		}
		legacy.mu.Lock()
		legacy.failTightenAt = 0
		legacy.mu.Unlock()
		record := migrate(t, w.clientA)
		if record.Tightened+record.AlreadyTightened != 3 {
			t.Fatalf("resumed migration = %+v", record)
		}
		if _, ok, err := store.LabelMigration(context.Background()); err != nil || !ok {
			t.Fatalf("marker after the resumed run = %v, %v", ok, err)
		}
		runLabelConformanceTable(t, w)
	})
}

// taintedBase serves the stored visibility of one document as a value no
// current build would accept, as a document ingested before the label
// charset existed would carry. inNode controls whether the document node
// carries it too: parse.go drops an unparseable value from the nodes, so a
// pre-charset label may survive only in the revision metadata.
type taintedBase struct {
	explorer.Client
	documentID shoal.ID
	value      string
	inNode     bool
}

func (b *taintedBase) Document(ctx context.Context, documentID, revisionID shoal.ID) (explorer.DocumentView, error) {
	view, err := b.Client.Document(ctx, documentID, revisionID)
	if err == nil && documentID == b.documentID {
		metadata := shoal.Metadata{}
		for key, value := range view.Document.Metadata {
			metadata[key] = value
		}
		metadata[interaction.PropertyVisibility] = b.value
		view.Document.Metadata = metadata
	}
	return view, err
}

func (b *taintedBase) Neighborhood(ctx context.Context, request explorer.NeighborhoodRequest) (explorer.Neighborhood, error) {
	neighborhood, err := b.Client.Neighborhood(ctx, request)
	if err != nil {
		return neighborhood, err
	}
	for index, node := range neighborhood.Nodes {
		if node.ID != b.documentID {
			continue
		}
		properties := shoal.Metadata{}
		for key, value := range node.Properties {
			properties[key] = value
		}
		if b.inNode {
			properties[interaction.PropertyVisibility] = b.value
		} else {
			delete(properties, interaction.PropertyVisibility)
		}
		neighborhood.Nodes[index].Properties = properties
	}
	return neighborhood, nil
}

// TestLabelMigrationUntranslatableLabelsCloseTheDocument pins that a label
// that cannot be translated makes the document unreadable to everyone, even
// a principal holding every grant, and that it is reported while the run
// continues.
func TestLabelMigrationUntranslatableLabelsCloseTheDocument(t *testing.T) {
	manyLabels := make([]string, authorized.MaxLabelsPerRule+1)
	for index := range manyLabels {
		manyLabels[index] = "l" + strconv.Itoa(index)
	}
	for name, c := range map[string]struct {
		value  string
		inNode bool
		reason string
	}{
		"outside the charset":            {"bad label!", true, "does not parse"},
		"pre-charset, metadata only":     {"bad label!", false, "revision visibility does not parse"},
		"over MaxLabelsPerRule":          {strings.Join(manyLabels, "&"), true, "MaxLabelsPerRule"},
		"over the label byte bound":      {strings.Repeat("a", interaction.MaxVisibilityLabelSz+1), true, "does not parse"},
		"policy identity over 128 bytes": {strings.Repeat("a", 120), true, "cannot be translated"},
	} {
		t.Run(name, func(t *testing.T) {
			withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
				w, _ := newLegacyLabelWorld(t, store)
				control := w.targets["control"]
				tainted := &taintedBase{
					Client: w.f.base, documentID: control.current.Document.ID,
					value: c.value, inNode: c.inNode,
				}
				selector := w.f.staticSelector(t, w.f.sourceA, w.f.policyA)
				migrator := w.f.labelClient(t, tainted, w.store, selector, selector)
				record := migrate(t, migrator)
				if len(record.Untranslatable) == 0 {
					t.Fatalf("report = %+v", record)
				}
				entry := record.Untranslatable[0]
				if entry.DocumentID != control.current.Document.ID ||
					entry.RevisionID != control.current.Revision.ID ||
					entry.SourceURI != control.uri ||
					!strings.Contains(entry.Reason, c.reason) ||
					!strings.HasPrefix(entry.EscapedLabel, `"`) {
					t.Fatalf("report entry = %+v", entry)
				}
				if strings.Contains(c.value, "!") &&
					entry.EscapedLabel != strconv.QuoteToASCII(c.value) {
					t.Fatalf("escaped label = %s", entry.EscapedLabel)
				}
				// The run continued: the labelled documents were tightened.
				if record.Tightened < 3 {
					t.Fatalf("run stopped at the untranslatable document: %+v", record)
				}
				// Nobody reads it, not even the principal holding every grant.
				for reader, ctx := range map[string]context.Context{
					"admin": w.labelAdmin(t), "source only": w.sourceOnly(t),
					"full holder": w.fullHolder(t),
				} {
					summaries, err := w.clientA.Documents(ctx)
					requireNoError(t, "Documents", err)
					if summariesContain(summaries, control.current.Document.ID) {
						t.Errorf("%s lists the untranslatable document", reader)
					}
					for _, revision := range []shoal.ID{
						control.current.Revision.ID, control.historical.Revision.ID,
					} {
						_, err := w.clientA.Document(ctx, control.current.Document.ID, revision)
						requireNotFound(t, reader+" Document", err)
					}
				}
				// Its URI refuses every ingest, even by a principal holding
				// every grant: no grant satisfies the untranslatable claim.
				if _, err := w.clientA.Ingest(w.labelAdmin(t), labelledSource(
					control.uri, "a relabel attempt", "secret")); err == nil {
					t.Fatal("the untranslatable document's URI accepted an ingest")
				}
				// The other documents still follow the table.
				summaries, err := w.clientA.Documents(w.secretHolder(t))
				requireNoError(t, "Documents", err)
				if !summariesContain(summaries, w.targets["secret"].current.Document.ID) {
					t.Fatal("the label holder lost a translatable document")
				}
				// The report survives in the marker.
				stored, ok, err := w.store.LabelMigration(context.Background())
				if err != nil || !ok || !reflect.DeepEqual(stored.Untranslatable, record.Untranslatable) {
					t.Fatalf("stored report = %+v, %v, %v", stored, ok, err)
				}
			})
		})
	}
}

// TestLabelMigrationTightensHistoricalRevisions pins that a historical
// revision is narrowed by the current labels and by its own, which may
// differ.
func TestLabelMigrationTightensHistoricalRevisions(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		f := newFixture(t)
		legacy := &legacyStore{PolicyStore: store}
		legacy.setStrip(true)
		selector := f.staticSelector(t, f.sourceA, f.policyA)
		client := f.labelClient(t, f.base, legacy, selector, selector)
		owner := f.labelIngester(t, "owner", "secret", "x")
		const uri = "file:///label/history.txt"
		older, err := client.Ingest(owner, labelledSource(uri, "older, labelled x", "x"))
		if err != nil {
			t.Fatal(err)
		}
		newer, err := client.Ingest(owner, labelledSource(uri, "newer, labelled secret", "secret"))
		if err != nil {
			t.Fatal(err)
		}
		legacy.setStrip(false)
		documentID := newer.Document.ID
		read := func(ctx context.Context, revision shoal.ID) error {
			_, err := client.Document(ctx, documentID, revision)
			return err
		}
		if err := read(f.labelIngester(t, "before"), older.Revision.ID); err != nil {
			t.Fatalf("the legacy historical revision is not open before the migration: %v", err)
		}
		record := migrate(t, client)
		if record.Tightened != 1 || record.HistoricalTightened != 1 {
			t.Fatalf("report = %+v", record)
		}
		for _, c := range []struct {
			name     string
			labels   []string
			revision shoal.ID
			want     bool
		}{
			{"source only, current", nil, newer.Revision.ID, false},
			{"source only, historical", nil, older.Revision.ID, false},
			{"secret, current", []string{"secret"}, newer.Revision.ID, true},
			// The historical revision also needs its own label.
			{"secret, historical", []string{"secret"}, older.Revision.ID, false},
			{"x, historical", []string{"x"}, older.Revision.ID, false},
			{"secret+x, historical", []string{"secret", "x"}, older.Revision.ID, true},
		} {
			err := read(f.labelIngester(t, "reader", c.labels...), c.revision)
			if c.want && err != nil {
				t.Errorf("%s: %v", c.name, err)
			}
			if !c.want && !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
				t.Errorf("%s: err = %v, want not found", c.name, err)
			}
		}
		historical, ok, err := store.Revision(context.Background(), documentID, older.Revision.ID)
		if err != nil || !ok {
			t.Fatal(ok, err)
		}
		current, _, err := store.CurrentRevision(context.Background(), documentID)
		if err != nil {
			t.Fatal(err)
		}
		if !authorized.RuleIncludes(historical.Rule, current.Rule) {
			t.Error("the historical revision rule does not include the current labels")
		}
	})
}

// TestLabelMigrationTightensExtractedNodesAndEdges pins that entities and
// relations extracted from a legacy labelled document close with it.
func TestLabelMigrationTightensExtractedNodesAndEdges(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		f := newFixture(t)
		legacy := &legacyStore{PolicyStore: store}
		legacy.setStrip(true)
		selector := f.staticSelector(t, f.sourceA, f.policyA)
		client := f.labelClient(t, f.base, legacy, selector, selector)
		owner := f.labelIngester(t, "owner", "secret")
		document, err := client.Ingest(owner, explorer.Source{
			URI: "file:///label/legacy/SKILL.md", MediaType: explorer.MediaTypeMarkdown,
			Content:  authorizedSkillMarkdown,
			Metadata: shoal.Metadata{interaction.PropertyVisibility: "secret"},
		})
		if err != nil {
			t.Fatal(err)
		}
		extracted, err := client.ExtractDocument(owner, explorer.ExtractionRequest{
			DocumentID: document.Document.ID, RevisionID: document.Revision.ID,
			Version: authorizedSkillsOntologyVersion(t),
		})
		if err != nil {
			t.Fatal(err)
		}
		legacy.setStrip(false)
		if len(extracted.EntityNodeIDs) == 0 || len(extracted.RelationshipEdgeIDs) == 0 {
			t.Fatalf("extraction produced %d entities, %d relations",
				len(extracted.EntityNodeIDs), len(extracted.RelationshipEdgeIDs))
		}
		entity := extracted.EntityNodeIDs[0]
		neighborhood := func(ctx context.Context) error {
			_, err := client.Neighborhood(ctx, explorer.NeighborhoodRequest{
				NodeIDs: []shoal.ID{entity}, Depth: 1,
			})
			return err
		}
		if err := neighborhood(f.labelIngester(t, "before")); err != nil {
			t.Fatalf("the legacy entity is not open before the migration: %v", err)
		}

		migrate(t, client)

		if err := neighborhood(f.labelIngester(t, "reader")); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			t.Errorf("source-only entity read = %v, want not found", err)
		}
		if err := neighborhood(f.labelIngester(t, "holder", "secret")); err != nil {
			t.Errorf("label holder lost the entity: %v", err)
		}
		holderGraph, err := client.Neighborhood(f.labelIngester(t, "holder", "secret"),
			explorer.NeighborhoodRequest{NodeIDs: []shoal.ID{document.Document.ID}, Depth: 2})
		if err != nil || !hasNode(holderGraph, entity) {
			t.Fatalf("label holder does not reach the entity from its document: %v", err)
		}
		// Every extracted registration now carries the document's rule.
		current, _, err := store.CurrentRevision(context.Background(), document.Document.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range extracted.EntityNodeIDs {
			registration, ok, err := store.Node(context.Background(), id)
			if err != nil || !ok || registration.Kind != authorized.RegistrationExtracted ||
				!authorized.RuleIncludes(registration.Rule, current.Rule) {
				t.Errorf("entity %s = %+v, %v, %v", id, registration, ok, err)
			}
		}
		for _, id := range extracted.RelationshipEdgeIDs {
			registration, ok, err := store.Edge(context.Background(), id)
			if err != nil || !ok || registration.Kind != authorized.RegistrationExtracted ||
				!authorized.RuleIncludes(registration.Rule, current.Rule) {
				t.Errorf("relation %s = %+v, %v, %v", id, registration, ok, err)
			}
		}
	})
}

// TestLabelMigrationSurvivesRestart pins that the migration stays applied
// across a durable reopen and is not run again.
func TestLabelMigrationSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	store, err := authorized.OpenDurablePolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	w, _ := newLegacyLabelWorld(t, store)
	first := migrate(t, w.clientA)
	if first.Tightened != 3 {
		t.Fatalf("migration = %+v", first)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := authorized.OpenDurablePolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	selectorA := w.f.staticSelector(t, w.f.sourceA, w.f.policyA)
	selectorB := w.f.staticSelector(t, w.f.sourceB, w.f.policyB)
	w.store = reopened
	w.clientA = w.f.labelClient(t, w.f.base, reopened, selectorA, selectorA)
	w.clientB = w.f.labelClient(t, w.f.base, reopened, selectorB, selectorB)
	// The restart runs it again, and finds nothing left to narrow.
	again := migrate(t, w.clientA)
	if again.Tightened != 0 || again.AlreadyTightened != first.Tightened {
		t.Fatalf("migration after restart = %+v", again)
	}
	runLabelConformanceTable(t, w)
}

// TestLabelMigrationRequiresTheCapability pins the gate.
func TestLabelMigrationRequiresTheCapability(t *testing.T) {
	f := newFixture(t)
	for name, capability := range map[string]*labelmigration.Capability{
		"nil": nil, "zero": {},
	} {
		_, err := f.clientA.MigrateLabelledDocuments(context.Background(), capability)
		if !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
			t.Errorf("%s capability: err = %v, want unauthorized", name, err)
		}
	}
}

// TestLabelMigrationRepairsATornTighten builds the state a crash can leave
// when the durable batch of a TightenRule is torn: the revision registration
// already carries the labels, but the extracted registrations and the source
// claim do not. The rerun finds the document "already labelled" and must
// still sweep the rest.
func TestLabelMigrationRepairsATornTighten(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		f := newFixture(t)
		legacy := &legacyStore{PolicyStore: store, keepRevisions: true}
		legacy.setStrip(true)
		selector := f.staticSelector(t, f.sourceA, f.policyA)
		client := f.labelClient(t, f.base, legacy, selector, selector)
		owner := f.labelIngester(t, "owner", "secret")
		const uri = "file:///label/torn/SKILL.md"
		document, err := client.Ingest(owner, explorer.Source{
			URI: uri, MediaType: explorer.MediaTypeMarkdown,
			Content:  authorizedSkillMarkdown,
			Metadata: shoal.Metadata{interaction.PropertyVisibility: "secret"},
		})
		if err != nil {
			t.Fatal(err)
		}
		extracted, err := client.ExtractDocument(owner, explorer.ExtractionRequest{
			DocumentID: document.Document.ID, RevisionID: document.Revision.ID,
			Version: authorizedSkillsOntologyVersion(t),
		})
		if err != nil || len(extracted.EntityNodeIDs) == 0 {
			t.Fatalf("extraction: %v, %+v", err, extracted)
		}
		legacy.setStrip(false)
		entity := extracted.EntityNodeIDs[0]
		neighborhood := func(ctx context.Context) error {
			_, err := client.Neighborhood(ctx, explorer.NeighborhoodRequest{
				NodeIDs: []shoal.ID{entity}, Depth: 1,
			})
			return err
		}
		if err := neighborhood(f.labelIngester(t, "before")); err != nil {
			t.Fatalf("the torn state does not leak the entity; the test would be vacuous: %v", err)
		}
		record := migrate(t, client)
		// The revision already carried its labels; the sweep still
		// rewrote the extracted registrations and the claim.
		if record.Tightened != 1 || record.AlreadyTightened != 0 {
			t.Fatalf("report = %+v, want the sweep to have rewritten the rest", record)
		}
		if err := neighborhood(f.labelIngester(t, "reader")); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			t.Errorf("source-only entity read after the repair = %v, want not found", err)
		}
		current, _, err := store.CurrentRevision(context.Background(), document.Document.ID)
		if err != nil {
			t.Fatal(err)
		}
		claim, ok, err := store.SourceClaim(context.Background(), uri)
		if err != nil || !ok || !authorized.RuleIncludes(claim.Rule, current.Rule) {
			t.Fatalf("source claim after the repair = %+v, %v, %v", claim, ok, err)
		}
	})
}
