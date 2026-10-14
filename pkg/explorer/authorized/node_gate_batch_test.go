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
	"sync"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/explorer/evidencelabels"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// gateCountingStore counts the catalog reads the node gate makes.
type gateCountingStore struct {
	authorized.PolicyStore
	mu                                      sync.Mutex
	nodes, edges, revisions, currentBatches int
}

func (s *gateCountingStore) Nodes(ctx context.Context, ids []shoal.ID) (map[shoal.ID]authorized.NodeRegistration, error) {
	s.mu.Lock()
	s.nodes++
	s.mu.Unlock()
	return s.PolicyStore.Nodes(ctx, ids)
}

func (s *gateCountingStore) Edges(ctx context.Context, ids []shoal.ID) (map[shoal.ID]authorized.EdgeRegistration, error) {
	s.mu.Lock()
	s.edges++
	s.mu.Unlock()
	return s.PolicyStore.Edges(ctx, ids)
}

func (s *gateCountingStore) Revision(
	ctx context.Context, document, revision shoal.ID,
) (authorized.RevisionRegistration, bool, error) {
	s.mu.Lock()
	s.revisions++
	s.mu.Unlock()
	return s.PolicyStore.Revision(ctx, document, revision)
}

func (s *gateCountingStore) CurrentRevisions(
	ctx context.Context, ids []shoal.ID,
) (map[shoal.ID]authorized.RevisionRegistration, error) {
	s.mu.Lock()
	s.currentBatches++
	s.mu.Unlock()
	return s.PolicyStore.CurrentRevisions(ctx, ids)
}

func (s *gateCountingStore) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nodes, s.edges, s.revisions, s.currentBatches = 0, 0, 0, 0
}

type gateCorpus struct {
	f      *fixture
	client *authorized.Client
	store  *gateCountingStore
}

func newGateCorpus(t *testing.T) gateCorpus {
	t.Helper()
	f := newFixture(t)
	store := &gateCountingStore{PolicyStore: authorized.NewMemoryPolicyStore()}
	selector := f.staticSelector(t, f.sourceA, f.policyA)
	return gateCorpus{f: f, store: store,
		client: f.labelClient(t, f.base, store, selector, selector)}
}

// documentGraph ingests uri with content and labels and returns the graph a
// document reference to its first span names.
func (c gateCorpus) documentGraph(t *testing.T, uri, content, labels string) evidencelabels.Graph {
	t.Helper()
	owner := c.f.labelIngester(t, "owner", "secret")
	receipt, err := c.client.Ingest(owner, labelledSource(uri, content, labels))
	if err != nil {
		t.Fatal(err)
	}
	view, err := c.client.Document(owner, receipt.Document.ID, receipt.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	span := firstSpanID(t, view)
	var section shoal.ID
	var visit func(explorer.SectionView)
	visit = func(node explorer.SectionView) {
		for _, candidate := range node.Spans {
			if candidate.ID == span {
				section = candidate.SectionID
			}
		}
		for _, child := range node.Children {
			visit(child)
		}
	}
	visit(view.Root)
	return evidencelabels.Graph{
		NodeIDs:    []shoal.ID{receipt.Document.ID, section, span},
		DocumentID: receipt.Document.ID, RevisionID: receipt.Revision.ID,
	}
}

func (c gateCorpus) decide(t *testing.T, ctx context.Context, graphs ...evidencelabels.Graph) []bool {
	t.Helper()
	verdicts, err := c.client.NodeGate().GraphsVisibleToReader(ctx, graphs)
	if err != nil {
		t.Fatal(err)
	}
	return verdicts
}

// TestTheNodeGateRequiresTheCitedRevisionsOwnRule: section and span
// identities do not change with the revision, so a reference citing a
// labelled historical revision names the same nodes as one citing an
// unlabelled current revision with the same text. The cited revision's own
// rule must be asked too, as historical document reads ask it (#585).
func TestTheNodeGateRequiresTheCitedRevisionsOwnRule(t *testing.T) {
	c := newGateCorpus(t)
	const uri, content = "file:///revision/history.txt", "unchanged span text"
	historical := c.documentGraph(t, uri, content, "secret")
	current := c.documentGraph(t, uri, content, "")
	if historical.RevisionID == current.RevisionID {
		t.Fatal("the relabel did not make a new revision")
	}
	for index, id := range historical.NodeIDs {
		if current.NodeIDs[index] != id {
			t.Fatalf("node %d changed with the revision (%s, %s); the probe "+
				"needs revision-independent identities", index, id, current.NodeIDs[index])
		}
	}
	outsider := c.f.labelIngester(t, "outsider")
	holder := c.f.labelIngester(t, "holder", "secret")
	unknown := current
	unknown.RevisionID = "revision-never-registered"
	if got := c.decide(t, outsider, historical, current, unknown); got[0] || !got[1] || got[2] {
		t.Fatalf("outsider: historical, current, unknown = %v, want false, true, false", got)
	}
	if got := c.decide(t, holder, historical, current); !got[0] || !got[1] {
		t.Fatalf("holder: historical, current = %v, want true, true", got)
	}
}

// TestTheNodeGateDecidesAPageInBoundedCatalogReads: deciding many references
// costs the same chunked catalog reads as deciding one, never a read per
// reference.
func TestTheNodeGateDecidesAPageInBoundedCatalogReads(t *testing.T) {
	c := newGateCorpus(t)
	first := c.documentGraph(t, "file:///batch/one.txt", "first batch document", "secret")
	second := c.documentGraph(t, "file:///batch/two.txt", "second batch document", "")
	holder := c.f.labelIngester(t, "holder", "secret")

	measure := func(graphs []evidencelabels.Graph) (int, int, int, int) {
		c.store.reset()
		c.decide(t, holder, graphs...)
		return c.store.nodes, c.store.edges, c.store.revisions, c.store.currentBatches
	}
	nodesOne, edgesOne, revisionsOne, currentOne := measure([]evidencelabels.Graph{first, second})
	var many []evidencelabels.Graph
	for range 256 {
		many = append(many, first, second)
	}
	nodesMany, edgesMany, revisionsMany, currentMany := measure(many)
	if nodesMany != nodesOne || edgesMany != edgesOne ||
		revisionsMany != revisionsOne || currentMany != currentOne {
		t.Fatalf("512 references cost nodes=%d edges=%d revisions=%d current=%d "+
			"catalog reads; 2 cost nodes=%d edges=%d revisions=%d current=%d",
			nodesMany, edgesMany, revisionsMany, currentMany,
			nodesOne, edgesOne, revisionsOne, currentOne)
	}
	if nodesOne > 1 || revisionsOne > 2 {
		t.Fatalf("2 references cost %d node reads and %d revision reads", nodesOne, revisionsOne)
	}
}
