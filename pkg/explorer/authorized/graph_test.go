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
	"reflect"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestGraphAuthorizationReachabilityAndConjunction(t *testing.T) {
	f := newFixture(t)
	admin := f.admin(t)
	first, err := f.clientA.Ingest(admin, explorer.Source{
		URI: "file:///graph-a.txt", MediaType: explorer.MediaTypeText,
		Content: "graph a",
	})
	if err != nil {
		t.Fatal(err)
	}
	hidden, err := f.clientB.Ingest(admin, explorer.Source{
		URI: "file:///graph-hidden.txt", MediaType: explorer.MediaTypeText,
		Content: "hidden intermediary",
	})
	if err != nil {
		t.Fatal(err)
	}
	last, err := f.clientA.Ingest(admin, explorer.Source{
		URI: "file:///graph-c.txt", MediaType: explorer.MediaTypeText,
		Content: "graph c",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, edge := range []graph.Edge{
		{
			ID: "a-to-hidden", From: first.Document.ID, To: hidden.Document.ID,
			Type: "link", Weight: 1,
		},
		{
			ID: "hidden-to-c", From: hidden.Document.ID, To: last.Document.ID,
			Type: "link", Weight: 1,
		},
	} {
		edge.ID = appEdgeID(t, f.clientA, admin, edge.ID)
		if err := f.clientA.Connect(admin, edge); err != nil {
			t.Fatal(err)
		}
	}
	storedEdge, ok, err := f.store.Edge(context.Background(),
		appEdgeID(t, f.clientA, admin, "a-to-hidden"))
	if err != nil || !ok {
		t.Fatalf("stored edge: ok=%v err=%v", ok, err)
	}
	aliceDecision := f.decision(
		t,
		"edge-local-only",
		[][]byte{f.sourceA},
		[][]byte{f.policyA},
		allOperations,
	)
	if err := storedEdge.Rule.Authorize(
		aliceDecision, auth.OperationNeighborhood, f.clock.Now()); err != nil {
		t.Fatalf("edge record flattened endpoint rules: %v", err)
	}

	nodeIDs := []shoal.ID{first.Document.ID}
	edgeTypes := []string{"link"}
	nodeCopy := append([]shoal.ID(nil), nodeIDs...)
	typeCopy := append([]string(nil), edgeTypes...)
	neighborhood, err := f.clientA.Neighborhood(
		f.alice(t),
		explorer.NeighborhoodRequest{
			NodeIDs: nodeIDs, Depth: 2, EdgeTypes: edgeTypes,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(nodeIDs, nodeCopy) ||
		!reflect.DeepEqual(edgeTypes, typeCopy) {
		t.Fatal("Neighborhood mutated caller slices")
	}
	if hasNode(neighborhood, hidden.Document.ID) ||
		hasNode(neighborhood, last.Document.ID) ||
		len(neighborhood.Edges) != 0 {
		t.Fatalf("hidden intermediary bridged graph: %#v", neighborhood)
	}

	deniedEdge := graph.Edge{
		ID:   appEdgeID(t, f.clientA, f.alice(t), "denied-endpoint"),
		From: first.Document.ID, To: hidden.Document.ID,
		Type: "denied", Weight: 1, Properties: shoal.Metadata{"secret": "caller"},
	}
	propertiesCopy := cloneMetadata(deniedEdge.Properties)
	err = f.clientA.Connect(f.alice(t), deniedEdge)
	if !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("hidden endpoint error = %v", err)
	}
	if !reflect.DeepEqual(deniedEdge.Properties, propertiesCopy) {
		t.Fatal("Connect mutated caller properties")
	}
	raw, err := f.base.Neighborhood(context.Background(), explorer.NeighborhoodRequest{
		NodeIDs:   []shoal.ID{first.Document.ID},
		Depth:     1,
		EdgeTypes: []string{"denied"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(raw.Edges) != 0 {
		t.Fatalf("denied endpoint reached base: %#v", raw.Edges)
	}

	edgePolicyB, err := authorized.NewStaticPolicySelector(f.sourceB, f.policyB)
	if err != nil {
		t.Fatal(err)
	}
	guardedClient := f.newClient(
		t, f.base, f.store, f.sourceA, f.policyA, edgePolicyB)
	guarded := graph.Edge{
		ID:   appEdgeID(t, guardedClient, admin, "edge-conjunction"),
		From: first.Document.ID, To: last.Document.ID,
		Type: "guarded", Weight: 1,
	}
	if err := guardedClient.Connect(admin, guarded); err != nil {
		t.Fatal(err)
	}
	aliceGraph, err := guardedClient.Neighborhood(
		f.alice(t),
		explorer.NeighborhoodRequest{
			NodeIDs: []shoal.ID{first.Document.ID}, Depth: 1,
			EdgeTypes: []string{"guarded"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(aliceGraph.Edges) != 0 || hasNode(aliceGraph, last.Document.ID) {
		t.Fatalf("edge policy conjunction was bypassed: %#v", aliceGraph)
	}
	adminGraph, err := guardedClient.Neighborhood(
		admin,
		explorer.NeighborhoodRequest{
			NodeIDs: []shoal.ID{first.Document.ID}, Depth: 1,
			EdgeTypes: []string{"guarded"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(adminGraph.Edges) != 1 || !hasNode(adminGraph, last.Document.ID) {
		t.Fatalf("authorized edge missing: %#v", adminGraph)
	}
}

func TestConnectCatalogFailureRetryReconciles(t *testing.T) {
	f := newFixture(t)
	first, err := f.clientA.Ingest(f.admin(t), explorer.Source{
		URI: "file:///edge-retry-a.txt", MediaType: explorer.MediaTypeText,
		Content: "edge retry a",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.clientA.Ingest(f.admin(t), explorer.Source{
		URI: "file:///edge-retry-b.txt", MediaType: explorer.MediaTypeText,
		Content: "edge retry b",
	})
	if err != nil {
		t.Fatal(err)
	}
	failing := &failStore{PolicyStore: f.store, edgeFailures: 1}
	client := f.newClient(t, f.base, failing, f.sourceA, f.policyA, nil)
	edge := graph.Edge{
		ID:   appEdgeID(t, client, f.admin(t), "edge-retry"),
		From: first.Document.ID, To: second.Document.ID,
		Type: "retry", Weight: 1,
	}
	if err := client.Connect(f.admin(t), edge); !shoal.IsErrorCode(
		err, shoal.ErrorUnavailable,
	) {
		t.Fatalf("first edge catalog failure = %v", err)
	}
	other := f.newClient(t, f.base, f.store, f.sourceB, f.policyB, nil)
	if err := other.Connect(f.admin(t), edge); !shoal.IsErrorCode(
		err, shoal.ErrorConflict,
	) {
		t.Fatalf("different-policy edge seizure = %v", err)
	}
	if err := client.Connect(f.admin(t), edge); err != nil {
		t.Fatal(err)
	}

	neighborhood, err := client.Neighborhood(
		f.alice(t),
		explorer.NeighborhoodRequest{
			NodeIDs: []shoal.ID{first.Document.ID},
			Depth:   1, EdgeTypes: []string{"retry"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(neighborhood.Edges) != 1 {
		t.Fatalf("reconciled edge missing: %#v", neighborhood)
	}
}

func TestConnectPinsEndpointsThroughBaseMutation(t *testing.T) {
	f := newFixture(t)
	source := explorer.Source{
		URI: "file:///pinned-endpoint.txt", MediaType: explorer.MediaTypeText,
		Content: "pinned endpoint",
	}
	endpoint, err := f.clientA.Ingest(f.admin(t), source)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	hooked := &hookClient{
		Client: f.base,
		connect: func(ctx context.Context, edge graph.Edge) error {
			close(started)
			<-release
			return f.base.Connect(ctx, edge)
		},
	}
	client := f.newClient(t, hooked, f.store, f.sourceA, f.policyA, nil)
	connectErr := make(chan error, 1)
	go func() {
		connectErr <- client.Connect(f.admin(t), graph.Edge{
			ID:   appEdgeID(t, client, f.admin(t), "pinned-edge"),
			From: endpoint.Document.ID,
			To:   endpoint.Document.ID, Type: "link", Weight: 1,
		})
	}()
	<-started
	ingestErr := make(chan error, 1)
	go func() {
		_, err := f.clientB.Ingest(f.admin(t), explorer.Source{
			URI: source.URI, MediaType: source.MediaType,
			Content: "new endpoint policy",
		})
		ingestErr <- err
	}()
	select {
	case err := <-ingestErr:
		close(release)
		t.Fatalf("endpoint mutation was not fenced: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-connectErr; err != nil {
		t.Fatal(err)
	}
	if err := <-ingestErr; err != nil {
		t.Fatal(err)
	}
}

func TestNeighborhoodHydratesEachDocumentOnce(t *testing.T) {
	f := newFixture(t)
	result, err := f.clientA.Ingest(f.admin(t), explorer.Source{
		URI:       "file:///batched-neighborhood.md",
		MediaType: explorer.MediaTypeMarkdown,
		Content:   "# One\n\nalpha\n\n## Two\n\nbeta\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	documentCalls, documentsCalls := 0, 0
	hooked := &hookClient{
		Client: f.base,
		documents: func(ctx context.Context) ([]explorer.DocumentSummary, error) {
			documentsCalls++
			return f.base.Documents(ctx)
		},
		document: func(
			ctx context.Context, documentID, revisionID shoal.ID,
		) (explorer.DocumentView, error) {
			documentCalls++
			return f.base.Document(ctx, documentID, revisionID)
		},
	}
	client := f.newClient(t, hooked, f.store, f.sourceA, f.policyA, nil)
	neighborhood, err := client.Neighborhood(
		f.alice(t),
		explorer.NeighborhoodRequest{
			NodeIDs: []shoal.ID{result.Document.ID}, Depth: 4,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(neighborhood.Nodes) < 4 {
		t.Fatalf("neighborhood did not exercise batching: %#v", neighborhood)
	}
	if documentsCalls != 2 || documentCalls != 2 {
		t.Fatalf(
			"canonical hydration calls: Documents=%d Document=%d",
			documentsCalls, documentCalls,
		)
	}
}

func TestExistingEdgeUsesCurrentEndpointRules(t *testing.T) {
	f := newFixture(t)
	admin := f.admin(t)
	first, err := f.clientA.Ingest(admin, explorer.Source{
		URI: "file:///dynamic-edge-a.txt", MediaType: explorer.MediaTypeText,
		Content: "dynamic edge a",
	})
	if err != nil {
		t.Fatal(err)
	}

	secondSource := explorer.Source{
		URI: "file:///dynamic-edge-b.txt", MediaType: explorer.MediaTypeText,
		Content: "dynamic edge b",
	}
	second, err := f.clientA.Ingest(admin, secondSource)
	if err != nil {
		t.Fatal(err)
	}
	edge := graph.Edge{
		ID:   appEdgeID(t, f.clientA, admin, "dynamic-endpoint-edge"),
		From: first.Document.ID, To: second.Document.ID,
		Type: "dynamic", Weight: 1,
	}
	if err := f.clientA.Connect(admin, edge); err != nil {
		t.Fatal(err)
	}
	stored, ok, err := f.store.Edge(context.Background(), edge.ID)
	if err != nil || !ok {
		t.Fatalf("stored edge: ok=%v err=%v", ok, err)
	}
	aliceDecision := f.decision(
		t,
		"edge-local-alice",
		[][]byte{f.sourceA},
		[][]byte{f.policyA},
		allOperations,
	)
	if err := stored.Rule.Authorize(
		aliceDecision, auth.OperationNeighborhood, f.clock.Now()); err != nil {
		t.Fatalf("stored edge rule contains endpoint policies: %v", err)
	}
	before, err := f.clientA.Neighborhood(
		f.alice(t),
		explorer.NeighborhoodRequest{
			NodeIDs: []shoal.ID{first.Document.ID},
			Depth:   1, EdgeTypes: []string{"dynamic"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Edges) != 1 || !hasNode(before, second.Document.ID) {
		t.Fatalf("edge missing before endpoint change: %#v", before)
	}

	if _, err := f.clientB.Ingest(admin, explorer.Source{
		URI: secondSource.URI, MediaType: secondSource.MediaType,
		Content: "endpoint now requires policy b",
	}); err != nil {
		t.Fatal(err)
	}
	after, err := f.clientA.Neighborhood(
		f.alice(t),
		explorer.NeighborhoodRequest{
			NodeIDs: []shoal.ID{first.Document.ID},
			Depth:   1, EdgeTypes: []string{"dynamic"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Edges) != 0 || hasNode(after, second.Document.ID) {
		t.Fatalf("stale endpoint authorization leaked edge: %#v", after)
	}
	adminGraph, err := f.clientA.Neighborhood(
		admin,
		explorer.NeighborhoodRequest{
			NodeIDs: []shoal.ID{first.Document.ID},
			Depth:   1, EdgeTypes: []string{"dynamic"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(adminGraph.Edges) != 1 || !hasNode(adminGraph, second.Document.ID) {
		t.Fatalf("current endpoint grants did not admit edge: %#v", adminGraph)
	}
}

func TestGraphRejectsUncatalogedCurrentRevision(t *testing.T) {
	f := newFixture(t)
	source := explorer.Source{
		URI: "file:///stale-graph.txt", Title: "Registered",
		MediaType: explorer.MediaTypeText, Content: "registered content",
	}
	registered, err := f.clientA.Ingest(f.admin(t), source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.base.Ingest(context.Background(), explorer.Source{
		URI: source.URI, Title: "Uncataloged",
		MediaType: source.MediaType, Content: "uncataloged replacement",
	}); err != nil {
		t.Fatal(err)
	}
	_, err = f.clientA.Neighborhood(f.alice(t), explorer.NeighborhoodRequest{
		NodeIDs: []shoal.ID{registered.Document.ID},
	})
	if !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("stale neighborhood seed error = %v", err)
	}
	err = f.clientA.Connect(f.alice(t), graph.Edge{
		ID:   appEdgeID(t, f.clientA, f.alice(t), "stale-connect"),
		From: registered.Document.ID,
		To:   registered.Document.ID, Type: "link", Weight: 1,
	})
	if !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("stale connect endpoint error = %v", err)
	}
}

func TestCrossDomainConnectIsForbidden(t *testing.T) {
	f := newFixture(t)
	first, err := f.clientA.Ingest(f.admin(t), explorer.Source{
		URI: "file:///domain-one.txt", MediaType: explorer.MediaTypeText,
		Content: "domain one",
	})
	if err != nil {
		t.Fatal(err)
	}
	domainTwo := []byte("domain-two")
	sourceTwo := []byte("source-two")
	policyTwo := []byte("policy-two")
	f.reader.Set(domainTwo, 1)
	selector, err := authorized.NewStaticPolicySelector(sourceTwo, policyTwo)
	if err != nil {
		t.Fatal(err)
	}
	clientTwo, err := authorized.NewClient(authorized.Config{
		Base:             f.base,
		Resolver:         f.authority.Resolver(),
		PolicySelector:   selector,
		PolicyStore:      f.store,
		GenerationReader: f.reader,
		Clock:            f.clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	decisionTwo, err := auth.NewDecision(auth.DecisionConfig{
		Subject:               "domain-two-user",
		Actor:                 "domain-two-actor",
		AuthorizationDomain:   domainTwo,
		AllowedOperations:     allOperations,
		PermittedSourceIDs:    [][]byte{sourceTwo},
		PermittedPolicyIDs:    [][]byte{policyTwo},
		PolicyGeneration:      1,
		AuthenticationExpires: f.clock.Now().Add(time.Hour),
		RequestID:             "domain-two-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctxTwo, err := f.authority.Binder().Bind(context.Background(), decisionTwo)
	if err != nil {
		t.Fatal(err)
	}
	second, err := clientTwo.Ingest(ctxTwo, explorer.Source{
		URI: "file:///domain-two.txt", MediaType: explorer.MediaTypeText,
		Content: "domain two",
	})
	if err != nil {
		t.Fatal(err)
	}
	err = f.clientA.Connect(f.admin(t), graph.Edge{
		ID:   appEdgeID(t, f.clientA, f.admin(t), "cross-domain"),
		From: first.Document.ID, To: second.Document.ID,
		Type: "cross", Weight: 1,
	})
	if !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("cross-domain connect error = %v", err)
	}
}

func hasNode(neighborhood explorer.Neighborhood, id shoal.ID) bool {
	for _, node := range neighborhood.Nodes {
		if node.ID == id {
			return true
		}
	}
	return false
}
