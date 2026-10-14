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

// Extracted relation edges are bound to the document revision that asserted
// them (#570). Shared entities collapse into one node owned by every document
// that extracted them (#700), and relation-edge IDs do not depend on the
// document, so an edge between two shared entities must carry its own
// asserting documents; checking the endpoints' owners is not enough.

import (
	"context"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type extractedEdgeWorld struct {
	t       *testing.T
	f       *fixture
	client  *authorized.Client
	owner   context.Context
	reader  context.Context
	version func() explorer.ExtractionRequest
}

func newExtractedEdgeWorld(t *testing.T, store authorized.PolicyStore) *extractedEdgeWorld {
	f := newFixture(t)
	selector := f.staticSelector(t, f.sourceA, f.policyA)
	return &extractedEdgeWorld{
		t: t, f: f,
		client: f.labelClient(t, f.base, store, selector, selector),
		owner:  f.labelIngester(t, "owner", "secret"),
		reader: f.labelIngester(t, "reader"),
	}
}

func skillBody(name, tool string) string {
	return "# " + name + " Skill\n\nTools:\n- " + tool +
		"\n\nCapabilities:\n- " + name + " feature\n"
}

func (w *extractedEdgeWorld) ingest(uri, content string, metadata shoal.Metadata) explorer.IngestResult {
	w.t.Helper()
	result, err := w.client.Ingest(w.owner, explorer.Source{
		URI: uri, MediaType: explorer.MediaTypeMarkdown,
		Content: content, Metadata: metadata,
	})
	if err != nil {
		w.t.Fatal(err)
	}
	return result
}

func (w *extractedEdgeWorld) extract(result explorer.IngestResult) explorer.ExtractionResult {
	w.t.Helper()
	extracted, err := w.client.ExtractDocument(w.reader, explorer.ExtractionRequest{
		DocumentID: result.Document.ID, RevisionID: result.Revision.ID,
		Version: authorizedSkillsOntologyVersion(w.t),
	})
	if err != nil {
		w.t.Fatal(err)
	}
	return extracted
}

func providesTool(t *testing.T, extracted explorer.ExtractionResult) (shoal.ID, shoal.ID) {
	t.Helper()
	for _, edge := range extracted.GraphEdges {
		if edge.Type == "provides_tool" {
			return edge.ID, edge.From
		}
	}
	t.Fatal("extraction produced no provides_tool relation")
	return "", ""
}

// edgeVisible reports whether ctx sees edgeID around from, failing the test
// on any error.
func (w *extractedEdgeWorld) edgeVisible(ctx context.Context, from, edgeID shoal.ID) bool {
	w.t.Helper()
	around, err := w.client.Neighborhood(ctx, explorer.NeighborhoodRequest{
		NodeIDs: []shoal.ID{from}, Depth: 1,
	})
	if err != nil {
		w.t.Fatal(err)
	}
	for _, edge := range around.Edges {
		if edge.ID == edgeID {
			return true
		}
	}
	return false
}

// TestLabelRelabelClosesRelationBetweenSharedEntities is the review repro:
// the relation alpha -provides_tool-> shared-cli is stated only by D2, while
// both endpoint entities are owned by public documents (alpha by D1,
// shared-cli by D3). Relabelling D2 must close the relation.
func TestLabelRelabelClosesRelationBetweenSharedEntities(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		w := newExtractedEdgeWorld(t, store)
		d1 := w.extract(w.ingest("file:///alpha/SKILL.md", skillBody("alpha", "other-cli"), nil))
		d3 := w.extract(w.ingest("file:///beta/SKILL.md", skillBody("beta", "shared-cli"), nil))
		const uri2 = "file:///alpha2/SKILL.md"
		d2 := w.extract(w.ingest(uri2, skillBody("alpha", "shared-cli"), nil))

		owned := map[shoal.ID]bool{}
		for _, node := range append(d1.GraphNodes, d3.GraphNodes...) {
			owned[node.ID] = true
		}
		earlier := map[shoal.ID]bool{}
		for _, edge := range append(d1.GraphEdges, d3.GraphEdges...) {
			earlier[edge.ID] = true
		}
		var relation, from shoal.ID
		for _, edge := range d2.GraphEdges {
			if edge.Type == "provides_tool" && owned[edge.From] && owned[edge.To] && !earlier[edge.ID] {
				relation, from = edge.ID, edge.From
			}
		}
		if relation == "" {
			t.Fatal("fixture no longer produces a D2-only relation between shared entities")
		}
		if !w.edgeVisible(w.reader, from, relation) {
			t.Fatal("relation not visible before the relabel")
		}

		w.ingest(uri2, skillBody("alpha", "shared-cli")+"\nNow secret.\n",
			shoal.Metadata{"shoal.visibility": "secret"})
		reader := w.f.labelIngester(t, "reader")
		if w.edgeVisible(reader, from, relation) {
			t.Fatal("source-only reader still sees a relation only the relabelled document states")
		}
		if !w.edgeVisible(w.f.labelIngester(t, "holder", "secret"), from, relation) {
			t.Fatal("label holder lost the relation")
		}
	})
}

// TestLabelSharedRelationVisibleThroughAnyAsserter defines what happens when
// two documents state the same relation and one of them is later labelled.
// Every asserter owns the relation (#700), so whichever document asserted it
// first, relabelling the other leaves it visible: a public document still
// states it. (Before #700 the registration belonged to the first asserter
// alone, so a relabelled first asserter closed the relation for readers of
// the public one, which told them that a document they cannot read states
// it.)
func TestLabelSharedRelationVisibleThroughAnyAsserter(t *testing.T) {
	for _, test := range []struct {
		name          string
		publicFirst   bool
		readerSeesAll bool
	}{
		{"public asserter first", true, true},
		{"relabelled asserter first", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
				w := newExtractedEdgeWorld(t, store)
				const publicURI = "file:///shared-public/SKILL.md"
				const secretURI = "file:///shared-secret/SKILL.md"
				body := skillBody("gamma", "gamma-cli")
				// A seed document mints the entities. Later extractions
				// reference them as existing nodes, so both documents below
				// assert the very same relation edge ID.
				w.extract(w.ingest("file:///shared-seed/SKILL.md", body, nil))
				public := w.ingest(publicURI, body, nil)
				secret := w.ingest(secretURI, body, nil)
				var first, second explorer.ExtractionResult
				if test.publicFirst {
					first, second = w.extract(public), w.extract(secret)
				} else {
					first, second = w.extract(secret), w.extract(public)
				}
				relation, from := providesTool(t, first)
				if again, _ := providesTool(t, second); again != relation {
					t.Fatalf("the two documents assert different relations: %s vs %s", relation, again)
				}

				w.ingest(secretURI, body+"\nNow secret.\n",
					shoal.Metadata{"shoal.visibility": "secret"})
				reader := w.f.labelIngester(t, "reader")
				if got := w.edgeVisible(reader, from, relation); got != test.readerSeesAll {
					t.Fatalf("source-only reader sees the shared relation = %v, want %v", got, test.readerSeesAll)
				}
				if !w.edgeVisible(w.f.labelIngester(t, "holder", "secret"), from, relation) {
					t.Fatal("label holder lost the shared relation")
				}
			})
		})
	}
}
