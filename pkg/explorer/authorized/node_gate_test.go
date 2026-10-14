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

import (
	"context"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/explorer/evidencelabels"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// The current-rule gate the dispatch and event planes decide stored
// evidence by (#564) must decide edges as the interaction read gate does:
// every edge's effective rule (an extracted relation is bound to the
// document that asserted it, #570/#585), both of its endpoints, and every
// node, failing closed on anything the catalog does not know.

type gateWorld struct {
	*extractedEdgeWorld
	relation, from, to       shoal.ID // stated only by d2, between public entities
	publicFrom, publicTo     shoal.ID // d1's own relation's endpoints
	connected                shoal.ID // a public-rule edge into a labelled document
	uri2                     string
	intoD2, edgeFrom, edgeTo shoal.ID // a public-rule edge from d2's document to its span
}

func newGateWorld(t *testing.T) gateWorld {
	t.Helper()
	w := newExtractedEdgeWorld(t, authorized.NewMemoryPolicyStore())
	d1 := w.extract(w.ingest("file:///alpha/SKILL.md", skillBody("alpha", "other-cli"), nil))
	d3 := w.extract(w.ingest("file:///beta/SKILL.md", skillBody("beta", "shared-cli"), nil))
	const uri2 = "file:///alpha2/SKILL.md"
	d2Ingest := w.ingest(uri2, skillBody("alpha", "shared-cli"), nil)
	d2 := w.extract(d2Ingest)
	g := gateWorld{extractedEdgeWorld: w, uri2: uri2}
	owned := map[shoal.ID]bool{}
	for _, node := range append(d1.GraphNodes, d3.GraphNodes...) {
		owned[node.ID] = true
	}
	earlier := map[shoal.ID]bool{}
	for _, edge := range append(d1.GraphEdges, d3.GraphEdges...) {
		earlier[edge.ID] = true
	}
	for _, edge := range d2.GraphEdges {
		if edge.Type == "provides_tool" && owned[edge.From] && owned[edge.To] && !earlier[edge.ID] {
			g.relation, g.from, g.to = edge.ID, edge.From, edge.To
		}
	}
	for _, edge := range d1.GraphEdges {
		if edge.Type == "provides_tool" {
			g.publicFrom, g.publicTo = edge.From, edge.To
		}
	}
	if g.relation == "" || g.publicFrom == "" {
		t.Fatal("fixture no longer produces the relations this test needs")
	}
	// A document labelled secret from the start, and an edge into it whose
	// own rule is the public source policy: only its endpoint is labelled.
	closed := w.ingest("file:///closed/SKILL.md", skillBody("closed", "closed-cli"),
		shoal.Metadata{interaction.PropertyVisibility: "secret"})
	// A public-rule edge from d2's document to its own first span, whose
	// endpoints close when d2 is relabelled. It carries no assertion, so a
	// session can cite it.
	view, err := w.client.Document(w.owner, d2Ingest.Document.ID, d2Ingest.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	g.intoD2 = appEdgeID(t, w.client, w.owner, "edge-into-d2")
	g.edgeFrom, g.edgeTo = d2Ingest.Document.ID, firstSpanID(t, view)
	if err := w.client.Connect(w.owner, graph.Edge{
		ID: g.intoD2, From: g.edgeFrom, To: g.edgeTo, Type: "supports", Weight: 1,
	}); err != nil {
		t.Fatal(err)
	}
	g.connected = appEdgeID(t, w.client, w.owner, "edge-into-closed")
	if err := w.client.Connect(w.owner, graph.Edge{
		ID: g.connected, From: g.publicFrom, To: closed.Document.ID,
		Type: "mentions", Weight: 1,
	}); err != nil {
		t.Fatal(err)
	}
	return g
}

func (g gateWorld) visible(t *testing.T, ctx context.Context, nodes, edges []shoal.ID) bool {
	t.Helper()
	visible, err := g.client.NodeGate().GraphsVisibleToReader(ctx,
		[]evidencelabels.Graph{{NodeIDs: nodes, EdgeIDs: edges}})
	if err != nil {
		t.Fatal(err)
	}
	return visible[0]
}

func (g gateWorld) holder() context.Context   { return g.f.labelIngester(g.t, "holder", "secret") }
func (g gateWorld) outsider() context.Context { return g.f.labelIngester(g.t, "reader") }

// TestTheNodeGateDecidesEdgesEndpointsAndUnknowns pins every way a graph
// reference can name something its nodes do not.
func TestTheNodeGateDecidesEdgesEndpointsAndUnknowns(t *testing.T) {
	g := newGateWorld(t)
	pair := []shoal.ID{g.from, g.to}
	public := []shoal.ID{g.publicFrom, g.publicTo}

	// The edge's endpoints are checked even when the reference's own nodes
	// are public and the edge's rule is public.
	if g.visible(t, g.outsider(), public, []shoal.ID{g.connected}) {
		t.Fatal("an edge into a labelled document was visible to an outsider")
	}
	if !g.visible(t, g.holder(), public, []shoal.ID{g.connected}) {
		t.Fatal("a holder lost an edge into a document it is cleared for")
	}
	// Unknown members deny, never allow.
	if g.visible(t, g.holder(), pair, []shoal.ID{"edge-never-registered"}) {
		t.Fatal("an unknown edge was visible")
	}
	if g.visible(t, g.holder(), []shoal.ID{"node-never-registered"}, nil) {
		t.Fatal("an unknown node was visible")
	}

	// An honest relation stated by a document relabelled afterwards, between
	// public entities, closes with the document.
	if !g.visible(t, g.outsider(), pair, []shoal.ID{g.relation}) {
		t.Fatal("the relation was not visible before the relabel")
	}
	g.ingest(g.uri2, skillBody("alpha", "shared-cli")+"\nNow secret.\n",
		shoal.Metadata{interaction.PropertyVisibility: "secret"})
	if g.visible(t, g.outsider(), pair, []shoal.ID{g.relation}) {
		t.Fatal("a relation only a relabelled document states stayed visible")
	}
	if !g.visible(t, g.holder(), pair, []shoal.ID{g.relation}) {
		t.Fatal("a holder lost the relation")
	}
	// The review repro: public nodes, the labelled relation as the edge.
	if g.visible(t, g.outsider(), public, []shoal.ID{g.relation}) {
		t.Fatal("public nodes carried a labelled relation to an outsider")
	}
}

// relationAssertions is the assertion the corpus records on the relation,
// as graph evidence must carry it.
func (g gateWorld) relationAssertions(t *testing.T) []interaction.AssertionReference {
	t.Helper()
	around, err := g.f.base.Neighborhood(context.Background(), explorer.NeighborhoodRequest{
		NodeIDs: []shoal.ID{g.from}, Depth: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	var result []interaction.AssertionReference
	for _, assertion := range around.Assertions {
		edge := shoal.ID(assertion.Metadata()["shoal.graph.edge_id"])
		if assertion.Origin() == ontology.AssertionDerived {
			edge = assertion.ID()
		}
		if edge == g.relation {
			result = append(result, interaction.AssertionReference{
				AssertionID: assertion.ID(), EdgeID: g.relation, Origin: assertion.Origin(),
			})
		}
	}
	if len(result) == 0 {
		t.Fatal("the relation carries no assertion; the probe would not reach it")
	}
	return result
}

// TestGraphEvidenceValidRequiresAnAuthoritativePath: at record time a graph
// reference's edge i must run from node i to node i+1, and its assertions
// must be exactly the ones the corpus records on those edges at the pinned
// snapshot, checked as the interaction recorder checks them.
func TestGraphEvidenceValidRequiresAnAuthoritativePath(t *testing.T) {
	g := newGateWorld(t)
	gate := g.client.NodeGate()
	snapshot, err := g.f.base.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	own := exactAuthorizedGraphEvidenceWith(t, g.f.base, graph.Edge{
		ID: g.relation, From: g.from, To: g.to,
	}, g.relationAssertions(t))
	mutate := func(change func(*interaction.EvidenceReference)) interaction.EvidenceReference {
		reference := own
		reference.NodeIDs = append([]shoal.ID(nil), own.NodeIDs...)
		reference.EdgeIDs = append([]shoal.ID(nil), own.EdgeIDs...)
		reference.Assertions = append([]interaction.AssertionReference(nil), own.Assertions...)
		change(&reference)
		return reference
	}
	for _, probe := range []struct {
		name      string
		reference interaction.EvidenceReference
		want      bool
	}{
		{"the relation's own path", own, true},
		{"its assertion omitted", mutate(func(r *interaction.EvidenceReference) {
			r.Assertions = nil
		}), false},
		{"an assertion that is not on it", mutate(func(r *interaction.EvidenceReference) {
			r.Assertions[0].AssertionID = "assertion-never-made"
		}), false},
		{"reversed", mutate(func(r *interaction.EvidenceReference) {
			r.NodeIDs[0], r.NodeIDs[1] = r.NodeIDs[1], r.NodeIDs[0]
		}), false},
		{"another path's nodes", mutate(func(r *interaction.EvidenceReference) {
			r.NodeIDs = []shoal.ID{g.publicFrom, g.publicTo}
		}), false},
		{"an unknown edge", mutate(func(r *interaction.EvidenceReference) {
			r.EdgeIDs = []shoal.ID{"edge-never-registered"}
			r.Assertions = nil
		}), false},
	} {
		valid, err := gate.GraphEvidenceValid(context.Background(),
			shoal.ID(snapshot.ID), snapshot.AsOf,
			[]interaction.EvidenceReference{probe.reference})
		if err != nil {
			t.Fatal(err)
		}
		if valid != probe.want {
			t.Fatalf("%s: valid = %v, want %v", probe.name, valid, probe.want)
		}
	}
	valid, err := gate.GraphEvidenceValid(context.Background(),
		"snapshot-never-taken", snapshot.AsOf, []interaction.EvidenceReference{own})
	if err != nil || valid {
		t.Fatalf("an untrusted snapshot pin: valid = %v, err = %v", valid, err)
	}
}

// TestTheNodeGateAndInteractionReadsAgreeOnAnEdge extends the cross-plane
// parity to an edge: the gate's verdict on an edge into d2's document, and
// the interaction read of a session whose evidence is that edge, for the same
// readers across a relabel of d2. As for nodes, the one
// pinned difference is a tightened record, which the explorer refuses to
// everyone until it is re-recorded.
func TestTheNodeGateAndInteractionReadsAgreeOnAnEdge(t *testing.T) {
	g := newGateWorld(t)
	snapshot, err := g.f.base.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	g.f.clock.Set(snapshot.AsOf.Add(time.Second))
	recorder := g.f.decision(t, "edge-recorder",
		[][]byte{g.f.sourceA, g.f.sourceB}, [][]byte{g.f.policyA, g.f.policyB},
		labelReaderOperations)
	fingerprint, err := auth.AuthorizationFingerprint(recorder)
	if err != nil {
		t.Fatal(err)
	}
	evidence := exactAuthorizedGraphEvidence(t, g.f.base, graph.Edge{
		ID: g.intoD2, From: g.edgeFrom, To: g.edgeTo,
	})
	session := interaction.Session{
		ID:                       interaction.DerivedID("session", "gate-edge-parity"),
		RecordedAt:               g.f.clock.Now(),
		Operation:                interaction.OperationToolCall,
		AuthorizationOperation:   string(auth.OperationConnect),
		SnapshotID:               shoal.ID(snapshot.ID),
		SnapshotAsOf:             snapshot.AsOf,
		AuthorizationFingerprint: shoal.ID(fingerprint.String()),
		AuthorizationExpiresAt:   recorder.AuthenticationExpires(),
		SeedNodeIDs:              []shoal.ID{g.edgeFrom, g.edgeTo},
		SeedEvidence:             []interaction.EvidenceReference{evidence},
	}
	if err := g.client.RecordInteraction(g.f.context(t, recorder), session); err != nil {
		t.Fatal(err)
	}
	check := func(stage string, ctx context.Context, wantGate, wantInteraction bool) {
		t.Helper()
		gate := g.visible(t, ctx, []shoal.ID{g.edgeFrom, g.edgeTo}, []shoal.ID{g.intoD2})
		_, err := g.client.InteractionRecord(ctx, session.ID)
		if err != nil && !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			t.Fatal(err)
		}
		if gate != wantGate || (err == nil) != wantInteraction {
			t.Fatalf("%s: gate = %v (want %v), interaction = %v (want %v)",
				stage, gate, wantGate, err == nil, wantInteraction)
		}
	}
	check("outsider before", g.outsider(), true, true)
	check("holder before", g.holder(), true, true)
	// A fresh owner: the clock moved to the snapshot above.
	g.owner = g.f.labelIngester(t, "owner", "secret")
	g.ingest(g.uri2, skillBody("alpha", "shared-cli")+"\nNow secret.\n",
		shoal.Metadata{interaction.PropertyVisibility: "secret"})
	check("outsider after tightening", g.outsider(), false, false)
	check("holder after tightening", g.holder(), true, false)
}

// exactAuthorizedGraphEvidenceWith is exactAuthorizedGraphEvidence for an edge
// that carries assertions: its anchor identity covers them.
func exactAuthorizedGraphEvidenceWith(
	t testing.TB, corpus *explorer.Explorer, edge graph.Edge,
	assertions []interaction.AssertionReference,
) interaction.EvidenceReference {
	t.Helper()
	around, err := corpus.Neighborhood(context.Background(), explorer.NeighborhoodRequest{
		NodeIDs: []shoal.ID{edge.From}, Depth: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	nodes := map[shoal.ID]graph.Node{}
	for _, node := range around.Nodes {
		nodes[node.ID] = node
	}
	var exact graph.Edge
	for _, candidate := range around.Edges {
		if candidate.ID == edge.ID {
			exact = candidate
		}
	}
	if exact.ID == "" || nodes[edge.From].ID == "" || nodes[edge.To].ID == "" {
		t.Fatal("exact graph evidence is unavailable")
	}
	anchor, err := inference.NewGraphAnchorWithAssertions(graph.Path{
		Nodes: []graph.Node{nodes[edge.From], nodes[edge.To]},
		Edges: []graph.Edge{exact},
	}, assertions)
	if err != nil {
		t.Fatal(err)
	}
	reference, err := anchor.EvidenceReference()
	if err != nil {
		t.Fatal(err)
	}
	return reference
}
