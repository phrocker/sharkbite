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

// The label conformance table (#570, PR2). A document ingested with
// metadata["shoal.visibility"] carries one structured policy per label in its
// AccessRule, so every public read path must deny a reader that holds the
// source but not every label, and allow one that holds them all. Each row
// below drives one exported method of *authorized.Client through the real
// client, a real explorer corpus, real auth decisions, and both policy-store
// implementations. TestLabelConformanceCoversEveryClientMethod fails when an
// exported method is added without a row or an explicit, reasoned exclusion.

import (
	"context"
	"reflect"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/lexicon"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const labelHubEdgeType = "label_conformance"

var labelReaderOperations = []auth.Operation{
	auth.OperationIngest,
	auth.OperationList,
	auth.OperationRead,
	auth.OperationConnect,
	auth.OperationGraphMaterialize,
	auth.OperationNeighborhood,
	auth.OperationRetrieve,
	auth.OperationValidate,
	auth.OperationAnalyticsRead,
}

// labelTarget is one document the table reads. Every target has two
// revisions with the same labels, so the historical-revision row has
// something to read.
type labelTarget struct {
	name       string
	labels     string
	uri        string
	token      string
	mention    string
	historical explorer.IngestResult
	current    explorer.IngestResult
	view       explorer.DocumentView
	sessionID  shoal.ID
	foldID     shoal.ID
	proposalID shoal.ID
	// hubEdges are the IDs of the edges from the hub and back (#699).
	hubEdges [2]shoal.ID
}

type labelWorld struct {
	f           *fixture
	store       authorized.PolicyStore
	clientA     *authorized.Client
	clientB     *authorized.Client
	hub         explorer.IngestResult
	targets     map[string]*labelTarget
	schema      ontology.OntologySchema
	ontology    ontology.OntologyVersion
	lexNodes    map[shoal.ID]graph.Node
	lexSnapshot lexicon.Snapshot
	sequence    int
}

func labelPolicy(t testing.TB, source []byte, label string) []byte {
	t.Helper()
	id, err := authorized.LabelPolicyID(source, label)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// labelClient builds a client over the shared corpus and store with every
// trusted dependency the read paths need, including ontology proposals.
func (f *fixture) labelClient(
	t testing.TB,
	base explorer.Client,
	store authorized.PolicyStore,
	selector authorized.PolicySelector,
	edgeSelector authorized.EdgePolicySelector,
) *authorized.Client {
	t.Helper()
	proposals, _ := base.(explorer.OntologyProposalStore)
	client, err := authorized.NewClient(authorized.Config{
		Base:                   base,
		VectorScorer:           trustedVectorScorer(base),
		InteractionWriter:      trustedInteractionWriter(base),
		InteractionReader:      trustedInteractionReader(base),
		SnapshotValidator:      trustedSnapshotValidator(base),
		DerivedAssertionReader: trustedDerivedAssertionReader(base),
		FoldStore:              trustedFoldStore(base),
		OntologyProposalStore:  proposals,
		Resolver:               f.authority.Resolver(),
		PolicySelector:         selector,
		EdgePolicySelector:     edgeSelector,
		PolicyStore:            store,
		GenerationReader:       f.reader,
		Clock:                  f.clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func (f *fixture) staticSelector(t testing.TB, source, policy []byte) *authorized.StaticPolicySelector {
	t.Helper()
	selector, err := authorized.NewStaticPolicySelector(source, policy)
	if err != nil {
		t.Fatal(err)
	}
	return selector
}

// The readers of the table. Each is minted at the current fixture clock.
func (w *labelWorld) labelAdmin(t testing.TB) context.Context {
	f := w.f
	return f.context(t, f.decision(t, "label-admin",
		[][]byte{f.sourceA, f.sourceB},
		[][]byte{
			f.policyA, f.policyB,
			labelPolicy(t, f.sourceA, "secret"),
			labelPolicy(t, f.sourceA, "x"),
			labelPolicy(t, f.sourceB, "secret"),
		},
		labelReaderOperations))
}

func (w *labelWorld) sourceOnly(t testing.TB) context.Context {
	f := w.f
	return f.context(t, f.decision(t, "label-source-only",
		[][]byte{f.sourceA}, [][]byte{f.policyA}, labelReaderOperations))
}

func (w *labelWorld) secretHolder(t testing.TB) context.Context {
	f := w.f
	return f.context(t, f.decision(t, "label-secret",
		[][]byte{f.sourceA},
		[][]byte{f.policyA, labelPolicy(t, f.sourceA, "secret")},
		labelReaderOperations))
}

// crossSource holds both sources, but the secret grant it holds is source
// B's. A grant for (B, secret) must not open (A, secret).
func (w *labelWorld) crossSource(t testing.TB) context.Context {
	f := w.f
	return f.context(t, f.decision(t, "label-cross-source",
		[][]byte{f.sourceA, f.sourceB},
		[][]byte{f.policyA, f.policyB, labelPolicy(t, f.sourceB, "secret")},
		labelReaderOperations))
}

func (w *labelWorld) fullHolder(t testing.TB) context.Context {
	f := w.f
	return f.context(t, f.decision(t, "label-full",
		[][]byte{f.sourceA},
		[][]byte{
			f.policyA,
			labelPolicy(t, f.sourceA, "secret"),
			labelPolicy(t, f.sourceA, "x"),
		},
		labelReaderOperations))
}

func newLabelWorld(t *testing.T, store authorized.PolicyStore) *labelWorld {
	t.Helper()
	f := newFixture(t)
	w := &labelWorld{
		f:        f,
		store:    store,
		targets:  make(map[string]*labelTarget),
		lexNodes: make(map[shoal.ID]graph.Node),
	}
	selectorA := f.staticSelector(t, f.sourceA, f.policyA)
	selectorB := f.staticSelector(t, f.sourceB, f.policyB)
	w.clientA = f.labelClient(t, f.base, store, selectorA, selectorA)
	w.clientB = f.labelClient(t, f.base, store, selectorB, selectorB)
	admin := w.labelAdmin(t)

	hub, err := w.clientA.Ingest(admin, explorer.Source{
		URI: "file:///label/hub.txt", MediaType: explorer.MediaTypeText,
		Content: "label conformance hub",
	})
	if err != nil {
		t.Fatal(err)
	}
	w.hub = hub
	for _, spec := range []struct {
		name, labels string
		client       *authorized.Client
	}{
		{"control", "", w.clientA},
		{"secret", "secret", w.clientA},
		{"both", "secret&x", w.clientA},
		{"secretB", "secret", w.clientB},
	} {
		target := &labelTarget{
			name:    spec.name,
			labels:  spec.labels,
			uri:     "file:///label/" + spec.name + "/SKILL.md",
			token:   "tok" + spec.name + "zz",
			mention: "Mention " + spec.name + " Topic",
		}
		metadata := shoal.Metadata{}
		if spec.labels != "" {
			metadata[interaction.PropertyVisibility] = spec.labels
		}
		target.historical, err = spec.client.Ingest(admin, explorer.Source{
			URI: target.uri, MediaType: explorer.MediaTypeText,
			Content:  "historical revision of " + spec.name,
			Metadata: metadata,
		})
		if err != nil {
			t.Fatalf("ingest %s: %v", spec.name, err)
		}
		// The current revision is skill-shaped so ExtractDocument has
		// something to extract once a reader is allowed to read it.
		target.current, err = spec.client.Ingest(admin, explorer.Source{
			URI: target.uri, MediaType: explorer.MediaTypeMarkdown,
			Content: "# " + spec.name + " Skill\n\nTools:\n- tool-" + spec.name +
				"\n\nCapabilities:\n- " + target.token + " feature\n",
			Metadata: metadata,
		})
		if err != nil {
			t.Fatalf("reingest %s: %v", spec.name, err)
		}
		if target.current.Revision.ID == target.historical.Revision.ID {
			t.Fatalf("%s did not get a second revision", spec.name)
		}
		target.view, err = spec.client.Document(
			admin, target.current.Document.ID, target.current.Revision.ID)
		if err != nil {
			t.Fatal(err)
		}
		// Both endpoints: one edge from the hub to the target and one back.
		for index, edge := range []graph.Edge{
			{ID: shoal.ID("hub-to-" + spec.name), From: hub.Document.ID,
				To: target.current.Document.ID, Type: labelHubEdgeType, Weight: 1},
			{ID: shoal.ID(spec.name + "-to-hub"), From: target.current.Document.ID,
				To: hub.Document.ID, Type: labelHubEdgeType, Weight: 1},
		} {
			edge.ID = appEdgeID(t, w.clientA, admin, edge.ID)
			target.hubEdges[index] = edge.ID
			if err := w.clientA.Connect(admin, edge); err != nil {
				t.Fatalf("connect %s: %v", edge.ID, err)
			}
		}
		node := graph.Node{
			ID: target.current.Document.ID, Kind: "document",
			Properties: shoal.Metadata{"name": target.mention},
		}
		w.lexNodes[node.ID] = node
		w.targets[spec.name] = target
	}
	snapshot, err := f.base.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	w.lexSnapshot = lexicon.Snapshot{
		ID: snapshot.ID, AsOf: snapshot.AsOf, Frontier: snapshot.Frontier,
	}
	w.schema, w.ontology = labelOntologyVersions(t, f.clock.Now())

	// Derived records that cite each target: an interaction, a fold of it,
	// and an ontology proposal whose evidence is the target's root section.
	for _, name := range []string{"control", "secret", "both", "secretB"} {
		target := w.targets[name]
		ctx, session := w.session(t, "recorded-"+name, target)
		client := w.clientA
		if name == "secretB" {
			client = w.clientB
		}
		if err := client.RecordInteraction(ctx, session); err != nil {
			t.Fatalf("record %s: %v", name, err)
		}
		target.sessionID = session.ID
		// Folded through the trusted store, as any build would have left it.
		// The authorized fold refuses a caller who could not read the result
		// (#568), and in the legacy world a label the rule does not yet
		// enforce is untranslatable, so no one could; the "fold
		// interactions" row probes the authorized path itself.
		folded, err := w.f.base.FoldInteractions(ctx, explorer.FoldRequest{
			SessionIDs: []shoal.ID{session.ID},
		})
		if err != nil {
			t.Fatalf("fold %s: %v", name, err)
		}
		target.foldID = folded.FoldID
		proposal := w.proposal(t, target)
		if err := client.CreateOntologyProposal(
			w.labelAdmin(t), proposal, w.ontology); err != nil {
			t.Fatalf("propose %s: %v", name, err)
		}
		target.proposalID = proposal.ID()
	}
	return w
}

// session builds an interaction seeded on target's first span, recorded by
// the admin decision at a fresh snapshot. The clock is moved to the snapshot
// so the decision and the record agree on time.
func (w *labelWorld) session(
	t testing.TB, id string, target *labelTarget,
) (context.Context, interaction.Session) {
	t.Helper()
	return w.sessionFor(t, id, target, func(t testing.TB) auth.Decision {
		f := w.f
		return f.decision(t, "label-admin",
			[][]byte{f.sourceA, f.sourceB},
			[][]byte{
				f.policyA, f.policyB,
				labelPolicy(t, f.sourceA, "secret"),
				labelPolicy(t, f.sourceA, "x"),
				labelPolicy(t, f.sourceB, "secret"),
			},
			labelReaderOperations)
	})
}

func (w *labelWorld) sessionFor(
	t testing.TB,
	id string,
	target *labelTarget,
	decide func(testing.TB) auth.Decision,
) (context.Context, interaction.Session) {
	t.Helper()
	snapshot, err := w.f.base.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if now := w.f.clock.Now(); snapshot.AsOf.Add(time.Second).After(now) {
		w.f.clock.Set(snapshot.AsOf.Add(time.Second))
	}
	decision := decide(t)
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		t.Fatal(err)
	}
	return w.f.context(t, decision), interaction.Session{
		ID:                       interaction.DerivedID("session", id),
		RecordedAt:               w.f.clock.Now(),
		SnapshotID:               shoal.ID(snapshot.ID),
		SnapshotAsOf:             snapshot.AsOf,
		AuthorizationFingerprint: shoal.ID(fingerprint.String()),
		AuthorizationExpiresAt:   decision.AuthenticationExpires(),
		SeedNodeIDs:              []shoal.ID{firstSpanID(t.(*testing.T), target.view)},
	}
}

func (w *labelWorld) proposal(t testing.TB, target *labelTarget) ontology.GovernedProposal {
	t.Helper()
	w.sequence++
	evidence, err := ontology.NewEvidenceRef(document.Citation{
		DocumentID: target.view.Document.ID,
		RevisionID: target.view.Revision.ID,
		SectionID:  target.view.Root.Section.ID,
		Range:      target.view.Root.Section.Range,
	}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	return labelOntologyProposal(
		t, w.schema, w.ontology, w.sequence, evidence, w.f.clock.Now())
}

// labelReader is one row of the decision axis.
type labelReader struct {
	name    string
	context func(*labelWorld, testing.TB) context.Context
}

type labelCase struct {
	reader labelReader
	target string
	want   bool
}

var (
	readerSourceOnly = labelReader{"source only", (*labelWorld).sourceOnly}
	readerSecret     = labelReader{"source+secret", (*labelWorld).secretHolder}
	readerCross      = labelReader{"source+secret for B", (*labelWorld).crossSource}
	readerFull       = labelReader{"source+secret+x", (*labelWorld).fullHolder}
)

// labelCases is the decision table every read row is checked against.
var labelCases = []labelCase{
	// Non-vacuity: an unlabelled document stays readable by the source.
	{readerSourceOnly, "control", true},
	{readerSourceOnly, "secret", false},
	{readerSecret, "secret", true},
	// A grant for (B, secret) does not open (A, secret)...
	{readerCross, "secret", false},
	// ...and does open (B, secret), so the denial above is not vacuous.
	{readerCross, "secretB", true},
	// Superset semantics: {secret, x} needs both.
	{readerSecret, "both", false},
	{readerFull, "both", true},
}

// labelProbe reports whether target is visible to ctx through one read path.
// When it is not, the probe itself asserts that the path withheld it in that
// path's documented shape (ObjectNotFound, dropped, or Suppressed++).
type labelProbe func(t *testing.T, w *labelWorld, ctx context.Context, target *labelTarget) bool

type labelRow struct {
	name   string
	method string
	probe  labelProbe
}

func requireNotFound(t *testing.T, path string, err error) {
	t.Helper()
	if !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("%s denial = %v, want ObjectNotFound", path, err)
	}
}

func requireNoError(t *testing.T, path string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

func summariesContain(summaries []explorer.DocumentSummary, id shoal.ID) bool {
	for _, summary := range summaries {
		if summary.Document.ID == id {
			return true
		}
	}
	return false
}

func responseCites(response retrieval.Response, id shoal.ID) bool {
	for _, result := range response.Results {
		for _, evidence := range result.Evidence {
			if evidence.Citation.DocumentID == id {
				return true
			}
		}
	}
	return false
}

// neighborhoodShowsTarget checks node and edge visibility around the hub
// together: an edge to or from the target may appear only when the target
// node does, and both hub edges must appear when it does.
func neighborhoodShowsTarget(
	t *testing.T, path string, neighborhood explorer.Neighborhood, target *labelTarget,
) bool {
	t.Helper()
	targetID := target.current.Document.ID
	visible := hasNode(neighborhood, targetID)
	touching := map[shoal.ID]bool{}
	for _, edge := range neighborhood.Edges {
		if edge.From == targetID || edge.To == targetID {
			touching[edge.ID] = true
		}
	}
	if !visible && len(touching) != 0 {
		t.Fatalf("%s returned an edge to a hidden endpoint: %v", path, touching)
	}
	if visible && (!touching[target.hubEdges[0]] ||
		!touching[target.hubEdges[1]]) {
		t.Fatalf("%s showed %s without both hub edges: %v", path, target.name, touching)
	}
	return visible
}

func labelBoundedRequest(seed shoal.ID) explorer.BoundedNeighborhoodRequest {
	return explorer.BoundedNeighborhoodRequest{
		NodeIDs: []shoal.ID{seed}, Depth: 1, Fanout: 64, MaxNodes: 64,
		MaxScannedEdges: 64, Direction: explorer.GraphDirectionBoth,
	}
}

func labelRows() []labelRow {
	return []labelRow{
		{"documents", "Documents", func(t *testing.T, w *labelWorld, ctx context.Context, target *labelTarget) bool {
			summaries, err := w.clientA.Documents(ctx)
			requireNoError(t, "Documents", err)
			return summariesContain(summaries, target.current.Document.ID)
		}},
		{"documents with suppressed", "DocumentsWithSuppressed", func(t *testing.T, w *labelWorld, ctx context.Context, target *labelTarget) bool {
			summaries, suppressed, err := w.clientA.DocumentsWithSuppressed(ctx)
			requireNoError(t, "DocumentsWithSuppressed", err)
			visible := summariesContain(summaries, target.current.Document.ID)
			if !visible && suppressed == 0 {
				t.Fatal("hidden labelled document was not counted as suppressed")
			}
			return visible
		}},
		{"documents with disclosure", "DocumentsWithDisclosure", func(t *testing.T, w *labelWorld, ctx context.Context, target *labelTarget) bool {
			summaries, disclosure, err := w.clientA.DocumentsWithDisclosure(ctx)
			requireNoError(t, "DocumentsWithDisclosure", err)
			visible := summariesContain(summaries, target.current.Document.ID)
			if !visible && disclosure.Suppressed == 0 {
				t.Fatal("hidden labelled document was not counted as suppressed")
			}
			return visible
		}},
		{"document current", "Document", func(t *testing.T, w *labelWorld, ctx context.Context, target *labelTarget) bool {
			_, err := w.clientA.Document(ctx, target.current.Document.ID, "")
			if err != nil {
				requireNotFound(t, "Document(current)", err)
				return false
			}
			return true
		}},
		{"document historical revision", "Document", func(t *testing.T, w *labelWorld, ctx context.Context, target *labelTarget) bool {
			_, err := w.clientA.Document(
				ctx, target.historical.Document.ID, target.historical.Revision.ID)
			if err != nil {
				requireNotFound(t, "Document(historical)", err)
				return false
			}
			return true
		}},
		{"retrieve", "Retrieve", func(t *testing.T, w *labelWorld, ctx context.Context, target *labelTarget) bool {
			response, err := w.clientA.Retrieve(ctx, retrieval.Request{Text: target.token, TopK: 10})
			requireNoError(t, "Retrieve", err)
			return responseCites(response, target.current.Document.ID)
		}},
		{"retrieve with suppressed", "RetrieveWithSuppressed", func(t *testing.T, w *labelWorld, ctx context.Context, target *labelTarget) bool {
			response, suppressed, err := w.clientA.RetrieveWithSuppressed(
				ctx, retrieval.Request{Text: target.token, TopK: 10})
			requireNoError(t, "RetrieveWithSuppressed", err)
			visible := responseCites(response, target.current.Document.ID)
			if !visible && suppressed == 0 {
				t.Fatal("hidden labelled document was not counted as suppressed")
			}
			return visible
		}},
		{"retrieve with disclosure", "RetrieveWithDisclosure", func(t *testing.T, w *labelWorld, ctx context.Context, target *labelTarget) bool {
			response, disclosure, err := w.clientA.RetrieveWithDisclosure(
				ctx, retrieval.Request{Text: target.token, TopK: 10})
			requireNoError(t, "RetrieveWithDisclosure", err)
			visible := responseCites(response, target.current.Document.ID)
			if !visible && disclosure.Suppressed == 0 {
				t.Fatal("hidden labelled document was not counted as suppressed")
			}
			return visible
		}},
		{"retrieve with report", "RetrieveWithReport", func(t *testing.T, w *labelWorld, ctx context.Context, target *labelTarget) bool {
			response, report, err := w.clientA.RetrieveWithReport(
				ctx, retrieval.Request{Text: target.token, TopK: 10})
			requireNoError(t, "RetrieveWithReport", err)
			visible := responseCites(response, target.current.Document.ID)
			if !visible && report.Disclosure.Suppressed == 0 {
				t.Fatal("hidden labelled document was not counted as suppressed")
			}
			return visible
		}},
		{"retrieval validation", "Retrieve", func(t *testing.T, w *labelWorld, ctx context.Context, target *labelTarget) bool {
			// A base that returns the admin's hit on the target regardless of
			// the authorized projection: validation must refuse it for a reader
			// that does not hold every label.
			request := retrieval.Request{Text: target.token, TopK: 1}
			honest, err := w.clientA.Retrieve(w.labelAdmin(t), request)
			requireNoError(t, "admin Retrieve", err)
			if !responseCites(honest, target.current.Document.ID) {
				t.Fatalf("admin retrieval missed %s", target.name)
			}
			selector := w.f.staticSelector(t, w.f.sourceA, w.f.policyA)
			forged := w.f.labelClient(t, &maliciousResultBase{
				Client: w.f.base, response: honest,
			}, w.store, selector, selector)
			response, err := forged.Retrieve(ctx, request)
			if err != nil {
				if !shoal.IsErrorCode(err, shoal.ErrorInternal) {
					t.Fatalf("forged hit error = %v, want internal", err)
				}
				return false
			}
			return responseCites(response, target.current.Document.ID)
		}},
		{"neighborhood", "Neighborhood", func(t *testing.T, w *labelWorld, ctx context.Context, target *labelTarget) bool {
			around, err := w.clientA.Neighborhood(ctx, explorer.NeighborhoodRequest{
				NodeIDs: []shoal.ID{w.hub.Document.ID}, Depth: 1,
				EdgeTypes: []string{labelHubEdgeType},
			})
			requireNoError(t, "Neighborhood(hub)", err)
			visible := neighborhoodShowsTarget(t, "Neighborhood", around, target)
			_, err = w.clientA.Neighborhood(ctx, explorer.NeighborhoodRequest{
				NodeIDs: []shoal.ID{target.current.Document.ID}, Depth: 1,
			})
			if visible {
				requireNoError(t, "Neighborhood(target)", err)
			} else {
				requireNotFound(t, "Neighborhood(target)", err)
			}
			return visible
		}},
		{"bounded neighborhood", "BoundedNeighborhood", func(t *testing.T, w *labelWorld, ctx context.Context, target *labelTarget) bool {
			request := labelBoundedRequest(w.hub.Document.ID)
			request.EdgeTypes = []string{labelHubEdgeType}
			around, err := w.clientA.BoundedNeighborhood(ctx, request)
			requireNoError(t, "BoundedNeighborhood(hub)", err)
			visible := neighborhoodShowsTarget(
				t, "BoundedNeighborhood", around.Neighborhood, target)
			_, err = w.clientA.BoundedNeighborhood(
				ctx, labelBoundedRequest(target.current.Document.ID))
			if visible {
				requireNoError(t, "BoundedNeighborhood(target)", err)
			} else {
				requireNotFound(t, "BoundedNeighborhood(target)", err)
			}
			return visible
		}},
		{"analytics materialization", "MaterializeAnalytics", func(t *testing.T, w *labelWorld, ctx context.Context, target *labelTarget) bool {
			request := labelBoundedRequest(w.hub.Document.ID)
			request.EdgeTypes = []string{labelHubEdgeType}
			materialized, err := w.clientA.MaterializeAnalytics(ctx, request, 64, 1<<20)
			requireNoError(t, "MaterializeAnalytics(hub)", err)
			visible := neighborhoodShowsTarget(
				t, "MaterializeAnalytics", materialized.Neighborhood, target)
			_, err = w.clientA.MaterializeAnalytics(
				ctx, labelBoundedRequest(target.current.Document.ID), 64, 1<<20)
			if visible {
				requireNoError(t, "MaterializeAnalytics(target)", err)
			} else {
				requireNotFound(t, "MaterializeAnalytics(target)", err)
			}
			return visible
		}},
		{"changes", "Changes", func(t *testing.T, w *labelWorld, ctx context.Context, target *labelTarget) bool {
			seen := 0
			request := authorized.ChangeFeedRequest{}
			for {
				page, err := w.clientA.Changes(ctx, request)
				requireNoError(t, "Changes", err)
				for _, change := range page.Changes {
					if change.Document.ID == target.current.Document.ID {
						seen++
					}
				}
				if !page.More {
					break
				}
				request.Cursor = page.Cursor
			}
			// Both publications (historical and current) appear together or
			// not at all.
			if seen != 0 && seen != 2 {
				t.Fatalf("Changes showed %d of %s's 2 publications", seen, target.name)
			}
			return seen == 2
		}},
		{"resolve mentions", "ResolveMentions", func(t *testing.T, w *labelWorld, ctx context.Context, target *labelTarget) bool {
			nodes := make([]graph.Node, 0, len(w.lexNodes))
			for _, node := range w.lexNodes {
				nodes = append(nodes, node)
			}
			sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
			bundle, err := lexicon.Build(lexicon.Input{
				Snapshot: w.lexSnapshot, Nodes: nodes,
			}, lexicon.Limits{})
			requireNoError(t, "lexicon.Build", err)
			mentions, err := w.clientA.ResolveMentions(ctx, bundle, "about "+target.mention+" today")
			requireNoError(t, "ResolveMentions", err)
			for _, mention := range mentions {
				for _, id := range mention.NodeIDs {
					if id == target.current.Document.ID {
						return true
					}
				}
			}
			return false
		}},
		{"lexicon scope nodes", "LexiconScopeNodes", func(t *testing.T, w *labelWorld, ctx context.Context, target *labelTarget) bool {
			scoped, err := w.clientA.LexiconScopeNodes(
				ctx, w.lexSnapshot, []graph.Node{w.lexNodes[target.current.Document.ID]})
			requireNoError(t, "LexiconScopeNodes", err)
			return scoped.Len() == 1
		}},
		{"interactions", "Interactions", func(t *testing.T, w *labelWorld, ctx context.Context, target *labelTarget) bool {
			summaries, err := w.clientA.Interactions(ctx)
			requireNoError(t, "Interactions", err)
			for _, summary := range summaries {
				if summary.SessionID == target.sessionID {
					return true
				}
			}
			return false
		}},
		{"interaction records", "InteractionRecords", func(t *testing.T, w *labelWorld, ctx context.Context, target *labelTarget) bool {
			records, err := w.clientA.InteractionRecords(ctx)
			requireNoError(t, "InteractionRecords", err)
			for _, record := range records {
				if record.Summary.SessionID == target.sessionID {
					return true
				}
			}
			return false
		}},
		{"interaction records page", "InteractionRecordsPage", func(t *testing.T, w *labelWorld, ctx context.Context, target *labelTarget) bool {
			page, err := w.clientA.InteractionRecordsPage(ctx, "", explorer.MaxInteractionRecordPageSize)
			requireNoError(t, "InteractionRecordsPage", err)
			for _, record := range page.Records {
				if record.Summary.SessionID == target.sessionID {
					return true
				}
			}
			return false
		}},
		{"interaction record", "InteractionRecord", func(t *testing.T, w *labelWorld, ctx context.Context, target *labelTarget) bool {
			_, err := w.clientA.InteractionRecord(ctx, target.sessionID)
			if err != nil {
				requireNotFound(t, "InteractionRecord", err)
				return false
			}
			return true
		}},
		{"interaction", "Interaction", func(t *testing.T, w *labelWorld, ctx context.Context, target *labelTarget) bool {
			_, err := w.clientA.Interaction(ctx, target.sessionID)
			if err != nil {
				requireNotFound(t, "Interaction", err)
				return false
			}
			return true
		}},
		{"interaction subgraph", "InteractionSubgraph", func(t *testing.T, w *labelWorld, ctx context.Context, target *labelTarget) bool {
			_, err := w.clientA.InteractionSubgraph(ctx, target.sessionID)
			if err != nil {
				requireNotFound(t, "InteractionSubgraph", err)
				return false
			}
			return true
		}},
		{"record interaction evidence", "RecordInteraction", func(t *testing.T, w *labelWorld, ctx context.Context, target *labelTarget) bool {
			// Citing a document is reading it: a reader that cannot see the
			// target cannot record an interaction seeded on it.
			readerCtx, session := w.readerSession(t, ctx, "record", target)
			err := w.clientA.RecordInteraction(readerCtx, session)
			if err != nil {
				requireNotFound(t, "RecordInteraction", err)
				return false
			}
			return true
		}},
		{"record interaction result evidence", "RecordInteractionResult", func(t *testing.T, w *labelWorld, ctx context.Context, target *labelTarget) bool {
			readerCtx, session := w.readerSession(t, ctx, "record-result", target)
			_, err := w.clientA.RecordInteractionResult(readerCtx, session)
			if err != nil {
				requireNotFound(t, "RecordInteractionResult", err)
				return false
			}
			return true
		}},
		{"fold interactions", "FoldInteractions", func(t *testing.T, w *labelWorld, ctx context.Context, target *labelTarget) bool {
			_, err := w.clientA.FoldInteractions(ctx, explorer.FoldRequest{
				SessionIDs: []shoal.ID{target.sessionID},
			})
			if err != nil {
				requireNotFound(t, "FoldInteractions", err)
				return false
			}
			return true
		}},
		{"folds", "Folds", func(t *testing.T, w *labelWorld, ctx context.Context, target *labelTarget) bool {
			folds, err := w.clientA.Folds(ctx)
			requireNoError(t, "Folds", err)
			for _, fold := range folds {
				if fold.FoldID == target.foldID {
					return true
				}
			}
			return false
		}},
		{"folds page", "FoldsPage", func(t *testing.T, w *labelWorld, ctx context.Context, target *labelTarget) bool {
			page, err := w.clientA.FoldsPage(ctx, "", explorer.MaxFoldSummaryPageSize)
			requireNoError(t, "FoldsPage", err)
			for _, fold := range page.Folds {
				if fold.FoldID == target.foldID {
					return true
				}
			}
			return false
		}},
		{"rehydrate fold", "RehydrateFold", func(t *testing.T, w *labelWorld, ctx context.Context, target *labelTarget) bool {
			_, err := w.clientA.RehydrateFold(ctx, target.foldID)
			if err != nil {
				requireNotFound(t, "RehydrateFold", err)
				return false
			}
			return true
		}},
		{"ontology proposals", "OntologyProposals", func(t *testing.T, w *labelWorld, ctx context.Context, target *labelTarget) bool {
			proposals, err := w.clientA.OntologyProposals(ctx)
			requireNoError(t, "OntologyProposals", err)
			for _, proposal := range proposals {
				if proposal.ID() == target.proposalID {
					return true
				}
			}
			return false
		}},
		{"ontology proposal mutation state", "OntologyProposalMutationState", func(t *testing.T, w *labelWorld, ctx context.Context, target *labelTarget) bool {
			state, err := w.clientA.OntologyProposalMutationState(ctx, w.ontology, target.proposalID)
			requireNoError(t, "OntologyProposalMutationState", err)
			return state.ProposalFound()
		}},
		{"create ontology proposal evidence", "CreateOntologyProposal", func(t *testing.T, w *labelWorld, ctx context.Context, target *labelTarget) bool {
			err := w.clientA.CreateOntologyProposal(ctx, w.proposal(t, target), w.ontology)
			if err != nil {
				requireNotFound(t, "CreateOntologyProposal", err)
				return false
			}
			return true
		}},
		{"restrict disclosure", "RestrictDisclosure", func(t *testing.T, w *labelWorld, ctx context.Context, target *labelTarget) bool {
			allowed, err := w.clientA.RestrictDisclosure(
				ctx, []shoal.ID{target.current.Document.ID})
			requireNoError(t, "RestrictDisclosure", err)
			return len(allowed) == 1 && allowed[0] == target.current.Document.ID
		}},
		{"extract document", "ExtractDocument", func(t *testing.T, w *labelWorld, ctx context.Context, target *labelTarget) bool {
			_, err := w.clientA.ExtractDocument(ctx, explorer.ExtractionRequest{
				DocumentID: target.current.Document.ID,
				RevisionID: target.current.Revision.ID,
				Version:    authorizedSkillsOntologyVersion(t),
			})
			if err != nil {
				requireNotFound(t, "ExtractDocument", err)
				return false
			}
			return true
		}},
		{"connect", "Connect", func(t *testing.T, w *labelWorld, ctx context.Context, target *labelTarget) bool {
			w.sequence++
			err := w.clientA.Connect(ctx, graph.Edge{
				ID: appEdgeID(t, w.clientA, ctx,
					shoal.ID("reader-edge-"+strconv.Itoa(w.sequence))),
				From: w.hub.Document.ID, To: target.current.Document.ID,
				Type: "reader_connect", Weight: 1,
			})
			if err != nil {
				requireNotFound(t, "Connect", err)
				return false
			}
			return true
		}},
	}
}

// labelInvariantRows are read paths that return no document-scoped data. The
// table proves the label grant does not change their output at all.
func labelInvariantRows() map[string]func(*testing.T, *labelWorld, context.Context) any {
	return map[string]func(*testing.T, *labelWorld, context.Context) any{
		"Snapshot": func(t *testing.T, w *labelWorld, ctx context.Context) any {
			snapshot, err := w.clientA.Snapshot(ctx)
			requireNoError(t, "Snapshot", err)
			return snapshot
		},
	}
}

// labelExcludedMethods are exported methods with no conformance row, each
// with the reason it cannot disclose a labelled document. Adding a method to
// *authorized.Client without a row or an entry here fails
// TestLabelConformanceCoversEveryClientMethod.
var labelExcludedMethods = map[string]string{
	"Ingest":           "write; label refusal is covered by TestLabelIngest* and the ingest-refusal rows",
	"MaterializeGraph": "write of caller-supplied nodes only; TestLabelGraphMaterialization* pins it",
	"BackfillExistingDocumentsForDevelopment": "development write; TestLabelBackfill* pins it",
	"MigrateLabelledDocuments":                "startup catalog write under an internal capability; TestLabelMigration* runs this whole table after it",
	"EnsureInteractionSink":                   "sink setup; reads no document",
	"AnalyticsInteractionSink":                "sink constructor; reads no document",
	"FleetActionInteractionSink":              "sink constructor; reads no document",
	"TransitionOntologyProposal":              "governance write keyed by a proposal the caller must already be able to read (OntologyProposals row)",
	"TransitionOntologyProposalWithLimits":    "governance write keyed by a proposal the caller must already be able to read (OntologyProposals row)",
	"OntologyActiveState":                     "workspace ontology tip; carries no document data",
	"PublishedOntologyCatalog":                "workspace ontology catalog; carries no document data",
	"AuthorizePublishedOntology":              "workspace ontology authorization; carries no document data",
	"RevalidateAnalytics":                     "admits only the exact decision fingerprint and snapshot of a prior MaterializeAnalytics; returns no data",
	"BoundedAvailable":                        "static capability bit; no context, no data",
	"VectorAvailable":                         "capability bit over the caller's authorized projection; returns no document data",
	"ValidateAuthorization":                   "compares the caller's own fingerprint with a pin; returns no data",
	"LabelVisibility":                         "returns the reader label evaluator, a host wiring seam; reads no data (label_visibility_test.go)",
	"LabelTranslator":                         "returns the record-time label translator, a host wiring seam; reads no data (label_visibility_test.go)",
	"NodeGate":                                "returns the current-rule node gate, a host wiring seam; reads no data (dispatch and delivery relabel tests)",
	"ApplicationEdgeID":                       "a digest of the caller's own identity and the ID it passed; reads no catalog or corpus data (edge_namespace_test.go)",
	"IngestUpload":                            "Ingest with its upload route; the same write and refusals as Ingest (upload_namespace_test.go)",
	"OwnUploadSourceURI":                      "a digest of the caller's own identity and the name it passed; reads no catalog or corpus data (upload_namespace_test.go)",
}

func TestLabelConformanceCoversEveryClientMethod(t *testing.T) {
	covered := map[string]bool{}
	for _, row := range labelRows() {
		covered[row.method] = true
	}
	for method := range labelInvariantRows() {
		covered[method] = true
	}
	clientType := reflect.TypeOf(&authorized.Client{})
	methods := map[string]bool{}
	for index := 0; index < clientType.NumMethod(); index++ {
		name := clientType.Method(index).Name
		methods[name] = true
		_, excluded := labelExcludedMethods[name]
		if covered[name] && excluded {
			t.Errorf("%s has a conformance row and an exclusion", name)
		}
		if !covered[name] && !excluded {
			t.Errorf("exported method %s has no label conformance row "+
				"(add one to labelRows, or a reasoned entry to labelExcludedMethods)", name)
		}
	}
	for name := range covered {
		if !methods[name] {
			t.Errorf("conformance row names %s, which is not a Client method", name)
		}
	}
	for name := range labelExcludedMethods {
		if !methods[name] {
			t.Errorf("exclusion names %s, which is not a Client method", name)
		}
	}
}

func TestLabelConformance(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		w := newLabelWorld(t, store)
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
		for method, read := range labelInvariantRows() {
			t.Run(method, func(t *testing.T) {
				want := read(t, w, w.labelAdmin(t))
				for _, reader := range []labelReader{
					readerSourceOnly, readerSecret, readerCross, readerFull,
				} {
					if got := read(t, w, reader.context(w, t)); !reflect.DeepEqual(got, want) {
						t.Errorf("%s differs for %q: %v vs %v", method, reader.name, got, want)
					}
				}
			})
		}
	})
}

// readerSession builds a session recorded by the reader of ctx itself.
func (w *labelWorld) readerSession(
	t *testing.T, ctx context.Context, kind string, target *labelTarget,
) (context.Context, interaction.Session) {
	t.Helper()
	decision, err := w.f.authority.Resolver().Resolve(ctx)
	if err != nil {
		t.Fatal(err)
	}
	w.sequence++
	return w.sessionFor(t, kind+"-"+strconv.Itoa(w.sequence), target,
		func(t testing.TB) auth.Decision {
			return w.f.decision(t, string(decision.Subject()),
				decision.PermittedSourceIDs(), decision.PermittedPolicyIDs(),
				labelReaderOperations)
		})
}

func labelOntologyVersions(
	t testing.TB, at time.Time,
) (ontology.OntologySchema, ontology.OntologyVersion) {
	t.Helper()
	schema, err := ontology.NewOntologySchema("label-evidence", "Label evidence", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	person, err := ontology.NewConceptDefinition("person", "Person", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	organization, err := ontology.NewConceptDefinition(
		"organization", "Organization", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	member, err := ontology.NewRelationshipDefinition(
		"member_of", "Member of", "",
		[]shoal.ID{person.ID()}, []shoal.ID{organization.ID()}, nil, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	base, err := ontology.NewOntologyVersion(
		schema, "1", at,
		[]ontology.ConceptDefinition{person, organization},
		[]ontology.RelationshipDefinition{member}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return schema, base
}

func labelOntologyProposal(
	t testing.TB,
	schema ontology.OntologySchema,
	base ontology.OntologyVersion,
	version int,
	evidence ontology.EvidenceRef,
	at time.Time,
) ontology.GovernedProposal {
	t.Helper()
	contractor, err := ontology.NewConceptDefinition(
		"contractor", "Contractor", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	concepts := append(base.Concepts(), contractor)
	relationship := base.Relationships()[0]
	widened, err := ontology.NewRelationshipDefinition(
		relationship.Key(), relationship.Name(), relationship.Description(),
		relationship.FromConcepts(),
		append(relationship.ToConcepts(), contractor.ID()),
		relationship.Properties(), relationship.Directed(), relationship.Metadata())
	if err != nil {
		t.Fatal(err)
	}
	target, err := ontology.NewOntologyVersion(
		schema, "label-"+strconv.Itoa(version), at.Add(time.Duration(version)*time.Second),
		concepts, []ontology.RelationshipDefinition{widened}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	morphism, err := ontology.NewOntologyMorphism(ontology.MorphismConfig{
		Kind: ontology.MorphismWiden, SourceVersion: base, TargetVersion: target,
		Sources:   []shoal.ID{relationship.ID()},
		Targets:   []shoal.ID{widened.ID()},
		Evidence:  []ontology.EvidenceRef{evidence},
		Rationale: "label conformance evidence",
	})
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := ontology.NewGovernedProposalWithMorphisms(
		schema, base, target, []ontology.OntologyMorphism{morphism},
		"label-author", "label proposal",
		at.Add(time.Duration(version+10)*time.Second), nil)
	if err != nil {
		t.Fatal(err)
	}
	return proposal
}
