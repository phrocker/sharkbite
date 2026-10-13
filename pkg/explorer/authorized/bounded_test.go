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

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestReachableThroughHonorsDirection(t *testing.T) {
	frontier := map[shoal.ID]struct{}{"node-a": struct{}{}}
	reverse := graph.Edge{ID: "edge-b-a", From: "node-b", To: "node-a", Type: "link", Weight: 1}
	if got := reachableThrough(reverse, frontier, explorer.GraphDirectionOutgoing); len(got) != 0 {
		t.Fatalf("outgoing traversed reverse edge: %#v", got)
	}
	if got := reachableThrough(reverse, frontier, explorer.GraphDirectionIncoming); len(got) != 1 || got[0] != "node-b" {
		t.Fatalf("incoming did not traverse reverse edge: %#v", got)
	}
	if got := reachableThrough(reverse, frontier, explorer.GraphDirectionBoth); len(got) != 1 || got[0] != "node-b" {
		t.Fatalf("both did not traverse reverse edge: %#v", got)
	}
}

func TestBoundedNeighborhoodPagesPastHiddenEdges(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, time.August, 28, 18, 0, 0, 0, time.UTC)
	policy, err := auth.NewPolicy(auth.PolicyConfig{
		AuthorizationDomain: []byte("domain"),
		SourceID:            []byte("source"),
		GrantPolicyID:       []byte("policy"),
		Epoch:               1,
	})
	if err != nil {
		t.Fatal(err)
	}
	rule, err := NewAccessRule(policy)
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryPolicyStore()
	view := explorer.DocumentView{
		Document: document.Document{
			ID: "node-seed", RevisionID: "revision", Title: "Seed",
			RootSectionID: "node-visible",
		},
		Revision: document.Revision{
			ID: "revision", DocumentID: "node-seed", CreatedAt: now,
		},
		SourceURI: "memory://seed",
		Root: explorer.SectionView{Section: document.Section{
			ID: "node-visible", DocumentID: "node-seed", RevisionID: "revision",
			Heading: "Visible",
		}},
	}
	digest, err := documentViewDigest(view)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := buildCanonicalRetrievalDocument(view, RevisionRegistration{
		DocumentID:     "node-seed",
		RevisionID:     "revision",
		NodeIDs:        []shoal.ID{"node-seed", "node-visible"},
		IntrinsicEdges: []graph.Edge{{ID: "contains", From: "node-seed", To: "node-visible", Type: "contains", Weight: 1}},
		ContentDigest:  digest,
		Rule:           rule,
		Current:        true,
	})
	if err != nil {
		t.Fatal(err)
	}
	visibleEdge := graph.Edge{
		ID: "edge-b-visible", From: "node-seed", To: "node-visible",
		Type: "related", Weight: 1,
	}
	if err := store.PutRevision(ctx, RevisionRegistration{
		DocumentID:     "node-seed",
		RevisionID:     "revision",
		NodeIDs:        []shoal.ID{"node-seed", "node-visible"},
		IntrinsicEdges: []graph.Edge{{ID: "contains", From: "node-seed", To: "node-visible", Type: "contains", Weight: 1}},
		ContentDigest:  digest,
		Rule:           rule,
		Current:        true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutEdge(ctx, EdgeRegistration{Edge: visibleEdge, Rule: rule}); err != nil {
		t.Fatal(err)
	}
	selector, err := NewStaticPolicySelector([]byte("source"), []byte("policy"))
	if err != nil {
		t.Fatal(err)
	}
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject:               "subject",
		Actor:                 "actor",
		AuthorizationDomain:   []byte("domain"),
		AllowedOperations:     []auth.Operation{auth.OperationNeighborhood},
		PermittedSourceIDs:    [][]byte{[]byte("source")},
		PermittedPolicyIDs:    [][]byte{[]byte("policy")},
		PolicyGeneration:      1,
		AuthenticationExpires: now.Add(time.Hour),
		RequestID:             "request",
	})
	if err != nil {
		t.Fatal(err)
	}
	base := &pagedBoundedBase{view: view, nodes: canonical.nodes}
	client, err := NewClient(Config{
		Base:           base,
		Resolver:       resolverFunc(func(context.Context) (auth.Decision, error) { return decision, nil }),
		PolicySelector: selector,
		PolicyStore:    store,
		GenerationReader: generationReaderFunc(func(context.Context, []byte) (int64, error) {
			return 1, nil
		}),
		Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.BoundedNeighborhood(ctx, explorer.BoundedNeighborhoodRequest{
		NodeIDs: []shoal.ID{"node-seed"}, Depth: 1, Fanout: 1, MaxNodes: 2,
		MaxScannedEdges: 2, Direction: explorer.GraphDirectionOutgoing,
	})
	if err != nil {
		t.Fatal(err)
	}
	if base.calls != 1 {
		t.Fatalf("base scan calls = %d, want 1", base.calls)
	}
	if len(got.Neighborhood.Edges) != 1 || got.Neighborhood.Edges[0].ID != visibleEdge.ID {
		t.Fatalf("visible edge after hidden edge was not returned: %#v", got)
	}
	if got.Continuation || got.Truncated || got.NextAfterEdgeID != "" {
		t.Fatalf("hidden adjacency leaked through result flags: %#v", got)
	}
	if got.ScannedEdgesKnown || got.ScannedEdges != 0 {
		t.Fatalf("scan accounting leaked hidden adjacency: %#v", got)
	}
}

func TestVerifyDocumentViewRegistrationAcceptsLegacyDigest(t *testing.T) {
	now := time.Date(2026, time.August, 29, 12, 0, 0, 0, time.UTC)
	view := explorer.DocumentView{
		Document: document.Document{
			ID: "legacy-doc", RevisionID: "legacy-revision", Title: "Legacy",
			RootSectionID: "legacy-root",
		},
		Revision: document.Revision{
			ID: "legacy-revision", DocumentID: "legacy-doc", CreatedAt: now,
		},
		SourceURI:       "file:///legacy.txt",
		SourceMediaType: explorer.MediaTypeText,
		Root: explorer.SectionView{Section: document.Section{
			ID: "legacy-root", DocumentID: "legacy-doc", RevisionID: "legacy-revision",
			Heading: "Legacy",
		}},
	}
	legacyDigest, err := legacyDocumentViewDigestV1(view)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := verifyDocumentViewRegistrationMode(view, RevisionRegistration{
		DocumentID: "legacy-doc", RevisionID: "legacy-revision",
		NodeIDs: []shoal.ID{"legacy-doc", "legacy-root"}, ContentDigest: legacyDigest,
	})
	if err != nil {
		t.Fatalf("legacy digest verification = %v", err)
	}
	if !legacy {
		t.Fatal("legacy digest was not reported")
	}
	policy, err := auth.NewPolicy(auth.PolicyConfig{
		AuthorizationDomain: []byte("domain"),
		SourceID:            []byte("source"),
		GrantPolicyID:       []byte("policy"),
		Epoch:               1,
	})
	if err != nil {
		t.Fatal(err)
	}
	rule, err := NewAccessRule(policy)
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryPolicyStore()
	if err := store.PutRevision(context.Background(), RevisionRegistration{
		DocumentID: "legacy-doc", RevisionID: "legacy-revision",
		NodeIDs: []shoal.ID{"legacy-doc", "legacy-root"}, ContentDigest: legacyDigest,
		Rule: rule, Current: true,
	}); err != nil {
		t.Fatal(err)
	}
	selector, err := NewStaticPolicySelector([]byte("source"), []byte("policy"))
	if err != nil {
		t.Fatal(err)
	}
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject:               "subject",
		Actor:                 "actor",
		AuthorizationDomain:   []byte("domain"),
		AllowedOperations:     []auth.Operation{auth.OperationList, auth.OperationRead},
		PermittedSourceIDs:    [][]byte{[]byte("source")},
		PermittedPolicyIDs:    [][]byte{[]byte("policy")},
		PolicyGeneration:      1,
		AuthenticationExpires: now.Add(time.Hour),
		RequestID:             "request",
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(Config{
		Base:           &pagedBoundedBase{view: view},
		Resolver:       resolverFunc(func(context.Context) (auth.Decision, error) { return decision, nil }),
		PolicySelector: selector,
		PolicyStore:    store,
		GenerationReader: generationReaderFunc(func(context.Context, []byte) (int64, error) {
			return 1, nil
		}),
		Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.Document(context.Background(), "legacy-doc", "legacy-revision")
	if err != nil {
		t.Fatal(err)
	}
	if got.SourceMediaType != "" {
		t.Fatalf("legacy document exposed unauthenticated media type %q", got.SourceMediaType)
	}
	summaries, err := client.Documents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].SourceMediaType != "" {
		t.Fatalf("legacy summary exposed unauthenticated media type: %+v", summaries)
	}
}

// TestBoundedNeighborhoodStreamsSuppressedAdjacencyInGrowingChunks pins how
// suppressed adjacency is scanned (#698): from the cursor, in chunks that
// start at the visible edges a page needs plus a fixed slack and double up
// to the chunk cap,
// never stopping on a work budget, whatever MaxScannedEdges says. A base that
// keeps paging past the node adjacency it reported is refused.
func TestBoundedNeighborhoodStreamsSuppressedAdjacencyInGrowingChunks(t *testing.T) {
	client, base := authorizedPaginationClient(t, true)
	base.adjacency = 12
	_, err := client.BoundedNeighborhood(context.Background(),
		explorer.BoundedNeighborhoodRequest{
			NodeIDs: []shoal.ID{"node-seed"}, Depth: 1, Fanout: 1, MaxNodes: 2,
			MaxScannedEdges: 1, Direction: explorer.GraphDirectionOutgoing,
			AfterEdgeID: "edge-a",
		})
	if !shoal.IsErrorCode(err, shoal.ErrorInternal) {
		t.Fatalf("paging past reported adjacency error = %v", err)
	}
	if base.calls != 14 {
		t.Fatalf("scan calls = %d, want reported adjacency + 2", base.calls)
	}
	want := []uint32{66, 132, 264, 528, 1024, 1024}
	for index, chunk := range want {
		if base.requests[index].Fanout != chunk ||
			base.requests[index].MaxNodes != chunk+1 {
			t.Fatalf("chunk %d = %#v, want fanout %d", index, base.requests[index], chunk)
		}
	}
	if base.requests[0].AfterEdgeID != "edge-a" {
		t.Fatalf("stream did not start at the cursor: %#v", base.requests[0])
	}
}

// TestBoundedNeighborhoodChargesSuppressedEdgesAgainstScanLimit pins that
// suppressed adjacency still bounds the scan (#698), now through the one
// uniform cap: a node whose reported total adjacency exceeds
// MaxBoundedNodeAdjacency fails after a single chunk, with one error,
// whatever fanout, cursor or MaxScannedEdges the caller passes, so neither
// can be swept to count the suppressed edges. At the cap it is streamed.
func TestBoundedNeighborhoodChargesSuppressedEdgesAgainstScanLimit(t *testing.T) {
	const limit = 7
	var messages []string
	for _, request := range []explorer.BoundedNeighborhoodRequest{
		{Fanout: 1, MaxNodes: 2, MaxScannedEdges: 2},
		{Fanout: 3, MaxNodes: 4},
		{Fanout: 64, MaxNodes: 64, MaxScannedEdges: 1 << 20},
		{Fanout: 1, MaxNodes: 2, AfterEdgeID: "edge-omitted-0001"},
	} {
		client, base := authorizedPaginationClient(t, false)
		client.maxNodeAdjacency = limit
		base.omittedOnly = true
		base.adjacency = limit + 1
		request.NodeIDs = []shoal.ID{"node-seed"}
		request.Depth = 1
		request.Direction = explorer.GraphDirectionOutgoing
		_, err := client.BoundedNeighborhood(context.Background(), request)
		if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
			t.Fatalf("over-cap error = %v", err)
		}
		messages = append(messages, err.Error())
		if base.calls != 1 {
			t.Fatalf("over-cap scan calls = %d, want 1", base.calls)
		}
		// At the cap the same suppressed adjacency is streamed, bounded by
		// the reported adjacency.
		client, base = authorizedPaginationClient(t, false)
		client.maxNodeAdjacency = limit
		base.omittedOnly = true
		base.adjacency = limit
		request.AfterEdgeID = ""
		_, err = client.BoundedNeighborhood(context.Background(), request)
		if !shoal.IsErrorCode(err, shoal.ErrorInternal) || base.calls != limit+2 {
			t.Fatalf("at-cap stream = %v after %d calls", err, base.calls)
		}
	}
	for _, message := range messages[1:] {
		if message != messages[0] {
			t.Fatalf("cap errors differ by request: %q", messages)
		}
	}
}

func TestBoundedNeighborhoodRejectsContinuationThatDoesNotAdvance(t *testing.T) {
	client, base := authorizedPaginationClient(t, false)
	base.underreportedScans = true
	_, err := client.BoundedNeighborhood(
		context.Background(),
		explorer.BoundedNeighborhoodRequest{
			NodeIDs: []shoal.ID{"node-seed"}, Depth: 1,
			Fanout: 2, MaxNodes: 2, MaxScannedEdges: 2,
			Direction: explorer.GraphDirectionOutgoing,
		},
	)
	if !shoal.IsErrorCode(err, shoal.ErrorInternal) {
		t.Fatalf("stalled continuation error = %v", err)
	}
	if base.calls != 2 {
		t.Fatalf("stalled continuation calls = %d, want 2", base.calls)
	}
}

func TestBoundedNeighborhoodRejectsScanCountAbovePageFanout(t *testing.T) {
	client, base := authorizedPaginationClient(t, false)
	base.overreportedScans = true
	_, err := client.BoundedNeighborhood(
		context.Background(),
		explorer.BoundedNeighborhoodRequest{
			NodeIDs: []shoal.ID{"node-seed"}, Depth: 1,
			Fanout: 1, MaxNodes: 2, MaxScannedEdges: 2,
			Direction: explorer.GraphDirectionOutgoing,
		},
	)
	if !shoal.IsErrorCode(err, shoal.ErrorInternal) {
		t.Fatalf("overreported scan error = %v", err)
	}
	if base.requests[0].Fanout != 66 {
		t.Fatalf("scan fanout = %d, want fanout + 1 + slack", base.requests[0].Fanout)
	}
	if base.calls != 1 {
		t.Fatalf("overreported scan calls = %d, want 1", base.calls)
	}
}

func TestBoundedNeighborhoodRejectsPageAboveChunk(t *testing.T) {
	client, base := authorizedPaginationClient(t, false)
	base.oversizedPage = true
	_, err := client.BoundedNeighborhood(
		context.Background(),
		explorer.BoundedNeighborhoodRequest{
			NodeIDs: []shoal.ID{"node-seed"}, Depth: 1,
			Fanout: 1, MaxNodes: 4, Direction: explorer.GraphDirectionOutgoing,
		},
	)
	if !shoal.IsErrorCode(err, shoal.ErrorInternal) {
		t.Fatalf("oversized base page error = %v", err)
	}
}

func TestBoundedNeighborhoodRejectsEdgeAtOrBeforeCursor(t *testing.T) {
	client, base := authorizedPaginationClient(t, false)
	_, err := client.BoundedNeighborhood(
		context.Background(),
		explorer.BoundedNeighborhoodRequest{
			NodeIDs: []shoal.ID{"node-seed"}, Depth: 1,
			Fanout: 1, MaxNodes: 4, Direction: explorer.GraphDirectionOutgoing,
			AfterEdgeID: "edge-z",
		},
	)
	if !shoal.IsErrorCode(err, shoal.ErrorInternal) || base.calls != 1 {
		t.Fatalf("edge before cursor = %v after %d calls", err, base.calls)
	}
}

func TestBoundedNeighborhoodRequiresReportedNodeAdjacency(t *testing.T) {
	client, base := authorizedPaginationClient(t, false)
	base.unknownAdjacency = true
	_, err := client.BoundedNeighborhood(
		context.Background(),
		explorer.BoundedNeighborhoodRequest{
			NodeIDs: []shoal.ID{"node-seed"}, Depth: 1,
			Fanout: 1, MaxNodes: 2, Direction: explorer.GraphDirectionOutgoing,
		},
	)
	if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("unreported adjacency error = %v", err)
	}
}

// TestBoundedNeighborhoodNeverReportsBaseScanCount pins that an authorized
// result reports no scan count and no node adjacency (#698): both count
// adjacency the reader may not see, even an exact zero.
func TestBoundedNeighborhoodNeverReportsBaseScanCount(t *testing.T) {
	client, base := authorizedPaginationClient(t, false)
	base.knownZero = true
	result, err := client.BoundedNeighborhood(
		context.Background(),
		explorer.BoundedNeighborhoodRequest{
			NodeIDs: []shoal.ID{"node-seed"}, Depth: 1,
			Fanout: 1, MaxNodes: 2, MaxScannedEdges: 1,
			Direction:   explorer.GraphDirectionOutgoing,
			AfterEdgeID: "edge-prior",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.ScannedEdgesKnown || result.ScannedEdges != 0 ||
		result.NodeAdjacencyKnown || result.NodeAdjacency != 0 {
		t.Fatalf("base scan count was reported = %#v", result)
	}
}

func TestBoundedNeighborhoodRejectsKnownZeroAdvancingContinuation(t *testing.T) {
	client, base := authorizedPaginationClient(t, false)
	base.knownZeroContinuation = true
	_, err := client.BoundedNeighborhood(
		context.Background(),
		explorer.BoundedNeighborhoodRequest{
			NodeIDs: []shoal.ID{"node-seed"}, Depth: 1,
			Fanout: 1, MaxNodes: 2, MaxScannedEdges: 1,
			Direction: explorer.GraphDirectionOutgoing,
		},
	)
	if !shoal.IsErrorCode(err, shoal.ErrorInternal) {
		t.Fatalf("known-zero advancing continuation error = %v", err)
	}
	if base.calls != 1 {
		t.Fatalf("malformed provider calls = %d, want 1", base.calls)
	}
}

func TestBoundedNeighborhoodRejectsAdvanceWithoutContinuation(t *testing.T) {
	client, base := authorizedPaginationClient(t, false)
	base.advanceWithoutContinuation = true
	_, err := client.BoundedNeighborhood(
		context.Background(),
		explorer.BoundedNeighborhoodRequest{
			NodeIDs: []shoal.ID{"node-seed"}, Depth: 1,
			Fanout: 1, MaxNodes: 2, MaxScannedEdges: 1,
			Direction:   explorer.GraphDirectionOutgoing,
			AfterEdgeID: "edge-prior",
		},
	)
	if !shoal.IsErrorCode(err, shoal.ErrorInternal) {
		t.Fatalf("cursor advance without continuation error = %v", err)
	}
	if base.calls != 1 {
		t.Fatalf("malformed provider calls = %d, want 1", base.calls)
	}
}

// TestBoundedNeighborhoodFailsClosedOnIncompleteBaseAdjacency pins that a
// base reporting truncation without a cursor is refused rather than served as
// a truncated page: a one-seed depth-one chunk is never node-bounded, and the
// visible page cannot be computed from an incomplete adjacency without
// depending on what the base left out.
func TestBoundedNeighborhoodFailsClosedOnIncompleteBaseAdjacency(t *testing.T) {
	client, base := authorizedPaginationClient(t, false)
	base.truncatedWithoutContinuation = true
	_, err := client.BoundedNeighborhood(
		context.Background(),
		explorer.BoundedNeighborhoodRequest{
			NodeIDs: []shoal.ID{"node-seed"}, Depth: 1,
			Fanout: 1, MaxNodes: 1, MaxScannedEdges: 1,
			Direction: explorer.GraphDirectionOutgoing,
		},
	)
	if !shoal.IsErrorCode(err, shoal.ErrorInternal) {
		t.Fatalf("incomplete base adjacency error = %v", err)
	}
}

// TestBoundedNeighborhoodAdjacencyChangeMidStreamIsConflict pins that the
// streamed node's reported adjacency changing between chunks is a conflict
// (a concurrent write), not an internal error, even when the base snapshot
// does not move, so it cannot single out a hidden write on the node.
func TestBoundedNeighborhoodAdjacencyChangeMidStreamIsConflict(t *testing.T) {
	client, base := authorizedPaginationClient(t, true)
	base.adjacency = 50
	base.adjacencyGrows = true
	_, err := client.BoundedNeighborhood(context.Background(),
		explorer.BoundedNeighborhoodRequest{
			NodeIDs: []shoal.ID{"node-seed"}, Depth: 1, Fanout: 1, MaxNodes: 2,
			Direction: explorer.GraphDirectionOutgoing,
		})
	if !shoal.IsErrorCode(err, shoal.ErrorConflict) || base.calls != 2 {
		t.Fatalf("adjacency change mid-stream = %v after %d calls, want conflict after 2",
			err, base.calls)
	}
}

// TestBoundedNeighborhoodProvenanceSeedAboveCapIsNotFound pins the ordering
// for a provenance seed (#698): its visibility is only learnt by filtering its
// adjacency, which the cap forbids, so a provenance seed above the cap is
// answered as an invisible one would be, for every reader, rather than with
// the cap error that would confirm it exists.
func TestBoundedNeighborhoodProvenanceSeedAboveCapIsNotFound(t *testing.T) {
	for _, depth := range []uint32{1, 2} {
		client, base := authorizedPaginationClient(t, false)
		client.maxNodeAdjacency = 1
		base.adjacency = 2
		_, err := client.BoundedNeighborhood(context.Background(),
			explorer.BoundedNeighborhoodRequest{
				NodeIDs: []shoal.ID{"producer_hidden"}, Depth: depth,
				Fanout: 1, MaxNodes: 2,
			})
		if !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			t.Fatalf("depth %d provenance seed above cap = %v, want not found", depth, err)
		}
		_, err = client.BoundedNeighborhood(context.Background(),
			explorer.BoundedNeighborhoodRequest{
				NodeIDs: []shoal.ID{"node-seed"}, Depth: depth,
				Fanout: 1, MaxNodes: 2,
			})
		if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
			t.Fatalf("depth %d source seed above cap = %v, want unavailable", depth, err)
		}
	}
}

func TestBoundedNeighborhoodRejectsZeroLimitsBeforeNormalization(t *testing.T) {
	client, _ := authorizedPaginationClient(t, false)
	for _, request := range []explorer.BoundedNeighborhoodRequest{
		{NodeIDs: []shoal.ID{"node-seed"}, Depth: 0, Fanout: 1, MaxNodes: 1},
		{NodeIDs: []shoal.ID{"node-seed"}, Depth: 1, Fanout: 0, MaxNodes: 1},
		{NodeIDs: []shoal.ID{"node-seed"}, Depth: 1, Fanout: 1, MaxNodes: 0},
	} {
		if _, err := client.BoundedNeighborhood(context.Background(), request); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
			t.Fatalf("zero limit request %#v error = %v", request, err)
		}
	}
}

func TestAuthorizedBoundedPageUsesNormalizedSeedCount(t *testing.T) {
	result := authorizedBoundedPage(
		map[shoal.ID]graph.Node{"node-seed": {ID: "node-seed", Kind: "entity"}},
		nil,
		nil,
		explorer.BoundedNeighborhoodRequest{
			NodeIDs: []shoal.ID{"node-seed", "node-seed"},
			Depth:   1, Fanout: 1, MaxNodes: 1,
		},
		1,
	)
	if result.Truncated {
		t.Fatalf("duplicate raw seed was treated as missing normalized seed: %#v", result)
	}
}

func TestAuthorizedVectorAvailabilityKeyLengthPrefixesIDs(t *testing.T) {
	left := authorizedVectorAvailabilityKey(
		auth.Decision{},
		[]shoal.ID{"a"},
		map[shoal.ID]RevisionRegistration{
			"a": {RevisionID: "b|c@d"},
		},
	)
	right := authorizedVectorAvailabilityKey(
		auth.Decision{},
		[]shoal.ID{"a@b", "c"},
		map[shoal.ID]RevisionRegistration{
			"a@b": {RevisionID: "c"},
			"c":   {RevisionID: "d"},
		},
	)
	if left == right {
		t.Fatalf("distinct visibility sets produced same key %q", left)
	}
}

func TestBoundedNeighborhoodRechecksGenerationAfterOntologyLens(t *testing.T) {
	for _, depth := range []uint32{1, 2} {
		t.Run(fmt.Sprintf("depth-%d", depth), func(t *testing.T) {
			client, base := authorizedPaginationClient(t, false)
			schema, _ := ontology.NewOntologySchema("guard", "Guard", "", nil)
			version, _ := ontology.NewOntologyVersion(
				schema, "1", time.Date(2026, time.September, 6, 0, 0, 0, 0, time.UTC),
				nil, nil, nil, nil)
			selected, _ := ontology.NewOntologyIdentity(version)
			now := time.Date(2026, time.September, 6, 0, 0, 0, 0, time.UTC)
			decision, err := auth.NewDecision(auth.DecisionConfig{
				Subject: "subject", Actor: "actor",
				AuthorizationDomain: []byte("domain"),
				AllowedOperations:   []auth.Operation{auth.OperationNeighborhood},
				PermittedSourceIDs:  [][]byte{[]byte("source")},
				PermittedPolicyIDs:  [][]byte{[]byte("policy")},
				PolicyGeneration:    1, AuthenticationExpires: now.Add(time.Hour),
				RequestID: "request", SelectedOntology: selected,
			})
			if err != nil {
				t.Fatal(err)
			}
			generation := int64(1)
			client.resolver = resolverFunc(func(context.Context) (auth.Decision, error) {
				return decision, nil
			})
			client.generationReader = generationReaderFunc(
				func(context.Context, []byte) (int64, error) {
					return generation, nil
				})
			client.clock = func() time.Time { return now }
			client.ontologyInterpreter = base
			base.interpret = func() { generation = 2 }
			_, err = client.BoundedNeighborhood(
				context.Background(), explorer.BoundedNeighborhoodRequest{
					NodeIDs: []shoal.ID{"node-seed"}, Depth: depth,
					Fanout: 1, MaxNodes: 2, MaxScannedEdges: 2,
				})
			if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
				t.Fatalf("generation change after lens = %v, want unavailable", err)
			}
		})
	}
}

func TestOntologyLensRequiresExplicitTrustedInterpreter(t *testing.T) {
	client, base := authorizedPaginationClient(t, false)
	schema, _ := ontology.NewOntologySchema("trusted", "Trusted", "", nil)
	version, _ := ontology.NewOntologyVersion(
		schema, "1", time.Date(2026, time.September, 6, 0, 0, 0, 0, time.UTC),
		nil, nil, nil, nil)
	selected, _ := ontology.NewOntologyIdentity(version)
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "subject", Actor: "actor",
		AuthorizationDomain: []byte("domain"),
		AllowedOperations:   []auth.Operation{auth.OperationNeighborhood},
		PermittedSourceIDs:  [][]byte{[]byte("source")},
		PermittedPolicyIDs:  [][]byte{[]byte("policy")},
		PolicyGeneration:    1,
		AuthenticationExpires: time.Date(
			2026, time.September, 6, 1, 0, 0, 0, time.UTC),
		RequestID: "request", SelectedOntology: selected,
	})
	if err != nil {
		t.Fatal(err)
	}
	called := false
	base.interpret = func() { called = true }
	result, err := client.applyOntologyLens(
		context.Background(), explorer.Neighborhood{}, decision)
	if err != nil {
		t.Fatal(err)
	}
	if called || len(result.Interpretations) != 0 {
		t.Fatal("untrusted base interpreter supplied ontology results")
	}
	client.ontologyInterpreter = base
	if _, err := client.applyOntologyLens(
		context.Background(), explorer.Neighborhood{}, decision); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("explicit trusted interpreter was not invoked")
	}
}

func TestOntologyLensPropagatesTrustedInterpreterUnavailability(t *testing.T) {
	client, base := authorizedPaginationClient(t, false)
	schema, _ := ontology.NewOntologySchema("uncertain", "Uncertain", "", nil)
	version, _ := ontology.NewOntologyVersion(
		schema, "1", time.Date(2026, time.September, 6, 0, 0, 0, 0, time.UTC),
		nil, nil, nil, nil)
	selected, _ := ontology.NewOntologyIdentity(version)
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "subject", Actor: "actor",
		AuthorizationDomain: []byte("domain"),
		AllowedOperations:   []auth.Operation{auth.OperationNeighborhood},
		PermittedSourceIDs:  [][]byte{[]byte("source")},
		PermittedPolicyIDs:  [][]byte{[]byte("policy")},
		PolicyGeneration:    1,
		AuthenticationExpires: time.Date(
			2026, time.September, 6, 1, 0, 0, 0, time.UTC),
		RequestID: "request", SelectedOntology: selected,
	})
	if err != nil {
		t.Fatal(err)
	}
	base.interpretErr = shoal.NewError(
		shoal.ErrorUnavailable, "ontology mutation outcome is indeterminate")
	client.ontologyInterpreter = base
	if _, err := client.applyOntologyLens(
		context.Background(), explorer.Neighborhood{}, decision,
	); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("trusted interpreter unavailability = %v", err)
	}
}

func authorizedPaginationClient(t *testing.T, hiddenOnly bool) (*Client, *pagedBoundedBase) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, time.August, 28, 18, 0, 0, 0, time.UTC)
	policy, err := auth.NewPolicy(auth.PolicyConfig{
		AuthorizationDomain: []byte("domain"),
		SourceID:            []byte("source"),
		GrantPolicyID:       []byte("policy"),
		Epoch:               1,
	})
	if err != nil {
		t.Fatal(err)
	}
	rule, err := NewAccessRule(policy)
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryPolicyStore()
	view := explorer.DocumentView{
		Document: document.Document{
			ID: "node-seed", RevisionID: "revision", Title: "Seed",
			RootSectionID: "node-visible",
		},
		Revision: document.Revision{
			ID: "revision", DocumentID: "node-seed", CreatedAt: now,
		},
		SourceURI: "memory://seed",
		Root: explorer.SectionView{Section: document.Section{
			ID: "node-visible", DocumentID: "node-seed", RevisionID: "revision",
			Heading: "Visible",
		}},
	}
	digest, err := documentViewDigest(view)
	if err != nil {
		t.Fatal(err)
	}
	registration := RevisionRegistration{
		DocumentID:     "node-seed",
		RevisionID:     "revision",
		NodeIDs:        []shoal.ID{"node-seed", "node-visible"},
		IntrinsicEdges: []graph.Edge{{ID: "contains", From: "node-seed", To: "node-visible", Type: "contains", Weight: 1}},
		ContentDigest:  digest,
		Rule:           rule,
		Current:        true,
	}
	canonical, err := buildCanonicalRetrievalDocument(view, registration)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutRevision(ctx, registration); err != nil {
		t.Fatal(err)
	}
	visibleEdge := graph.Edge{
		ID: "edge-b-visible", From: "node-seed", To: "node-visible",
		Type: "related", Weight: 1,
	}
	if err := store.PutEdge(ctx, EdgeRegistration{Edge: visibleEdge, Rule: rule}); err != nil {
		t.Fatal(err)
	}
	selector, err := NewStaticPolicySelector([]byte("source"), []byte("policy"))
	if err != nil {
		t.Fatal(err)
	}
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject:               "subject",
		Actor:                 "actor",
		AuthorizationDomain:   []byte("domain"),
		AllowedOperations:     []auth.Operation{auth.OperationNeighborhood},
		PermittedSourceIDs:    [][]byte{[]byte("source")},
		PermittedPolicyIDs:    [][]byte{[]byte("policy")},
		PolicyGeneration:      1,
		AuthenticationExpires: now.Add(time.Hour),
		RequestID:             "request",
	})
	if err != nil {
		t.Fatal(err)
	}
	base := &pagedBoundedBase{view: view, nodes: canonical.nodes, hiddenOnly: hiddenOnly}
	client, err := NewClient(Config{
		Base:           base,
		Resolver:       resolverFunc(func(context.Context) (auth.Decision, error) { return decision, nil }),
		PolicySelector: selector,
		PolicyStore:    store,
		GenerationReader: generationReaderFunc(func(context.Context, []byte) (int64, error) {
			return 1, nil
		}),
		Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return client, base
}

type pagedBoundedBase struct {
	calls                        int
	requests                     []explorer.BoundedNeighborhoodRequest
	view                         explorer.DocumentView
	nodes                        map[shoal.ID]graph.Node
	hiddenOnly                   bool
	interpret                    func()
	interpretErr                 error
	omittedOnly                  bool
	knownZero                    bool
	knownZeroContinuation        bool
	advanceWithoutContinuation   bool
	underreportedScans           bool
	overreportedScans            bool
	truncatedWithoutContinuation bool
	oversizedPage                bool
	unknownAdjacency             bool
	// adjacency is the node adjacency the fake reports; zero means 2.
	adjacency uint64
	// adjacencyGrows adds one to the reported adjacency on every call, as
	// a write landing on the node mid-stream would.
	adjacencyGrows bool
}

func (b *pagedBoundedBase) InterpretAssertions(
	_ context.Context,
	assertions []ontology.Assertion,
	selected ontology.OntologyIdentity,
) ([]ontology.AssertionInterpretation, error) {
	if b.interpret != nil {
		b.interpret()
	}
	if b.interpretErr != nil {
		return nil, b.interpretErr
	}
	result := make([]ontology.AssertionInterpretation, 0, len(assertions))
	for _, assertion := range assertions {
		result = append(result, ontology.ReadAssertionUnder(assertion, selected))
	}
	return result, nil
}

func (b *pagedBoundedBase) Retrieve(context.Context, retrieval.Request) (retrieval.Response, error) {
	return retrieval.Response{}, nil
}

func (b *pagedBoundedBase) Ingest(context.Context, explorer.Source) (explorer.IngestResult, error) {
	return explorer.IngestResult{}, nil
}

func (b *pagedBoundedBase) Documents(context.Context) ([]explorer.DocumentSummary, error) {
	return []explorer.DocumentSummary{{
		Document: b.view.Document, Revision: b.view.Revision, SourceURI: b.view.SourceURI,
	}}, nil
}

func (b *pagedBoundedBase) Document(context.Context, shoal.ID, shoal.ID) (explorer.DocumentView, error) {
	return b.view, nil
}

func (b *pagedBoundedBase) Connect(context.Context, graph.Edge) error { return nil }

func (b *pagedBoundedBase) Neighborhood(context.Context, explorer.NeighborhoodRequest) (explorer.Neighborhood, error) {
	return explorer.Neighborhood{}, nil
}

func (b *pagedBoundedBase) Snapshot(context.Context) (explorer.Snapshot, error) {
	return explorer.Snapshot{}, nil
}

func (b *pagedBoundedBase) BoundedNeighborhood(
	ctx context.Context,
	request explorer.BoundedNeighborhoodRequest,
) (explorer.BoundedNeighborhood, error) {
	b.calls++
	b.requests = append(b.requests, request)
	result, err := b.boundedPage(ctx, request)
	if err == nil && !b.unknownAdjacency {
		result.NodeAdjacency = b.adjacency
		if result.NodeAdjacency == 0 {
			result.NodeAdjacency = 2
		}
		if b.adjacencyGrows {
			result.NodeAdjacency += uint64(b.calls)
		}
		result.NodeAdjacencyKnown = true
	}
	return result, err
}

func (b *pagedBoundedBase) boundedPage(
	_ context.Context,
	request explorer.BoundedNeighborhoodRequest,
) (explorer.BoundedNeighborhood, error) {
	if b.oversizedPage {
		edges := make([]graph.Edge, 0, request.Fanout+1)
		for index := uint32(0); index <= request.Fanout; index++ {
			edges = append(edges, graph.Edge{
				ID: shoal.ID(fmt.Sprintf("edge-over-%04d", index)), From: "node-seed",
				To: "node-visible", Type: "related", Weight: 1,
			})
		}
		return explorer.BoundedNeighborhood{Neighborhood: explorer.Neighborhood{
			Nodes: []graph.Node{b.nodes["node-seed"], b.nodes["node-visible"]},
			Edges: edges,
		}}, nil
	}
	if b.knownZeroContinuation {
		return explorer.BoundedNeighborhood{
			Neighborhood: explorer.Neighborhood{
				Nodes: []graph.Node{b.nodes["node-seed"]},
			},
			Truncated: true, NextAfterEdgeID: "edge-progress",
			Continuation: true, ScannedEdgesKnown: true,
		}, nil
	}
	if b.underreportedScans {
		return explorer.BoundedNeighborhood{
			Neighborhood: explorer.Neighborhood{
				Nodes: []graph.Node{b.nodes["node-seed"]},
			},
			Truncated: true, NextAfterEdgeID: "edge-progress",
			Continuation: true, ScannedEdges: 1, ScannedEdgesKnown: true,
		}, nil
	}
	if b.overreportedScans {
		return explorer.BoundedNeighborhood{
			Neighborhood: explorer.Neighborhood{
				Nodes: []graph.Node{b.nodes["node-seed"]},
			},
			ScannedEdges: request.Fanout + 1, ScannedEdgesKnown: true,
		}, nil
	}
	if b.advanceWithoutContinuation {
		return explorer.BoundedNeighborhood{
			Neighborhood: explorer.Neighborhood{
				Nodes: []graph.Node{b.nodes["node-seed"]},
			},
			NextAfterEdgeID: "edge-progress",
			ScannedEdges:    1, ScannedEdgesKnown: true,
		}, nil
	}
	if b.knownZero {
		return explorer.BoundedNeighborhood{
			Neighborhood: explorer.Neighborhood{
				Nodes: []graph.Node{b.nodes["node-seed"]},
			},
			ScannedEdgesKnown: true,
		}, nil
	}
	if b.truncatedWithoutContinuation {
		return explorer.BoundedNeighborhood{
			Neighborhood: explorer.Neighborhood{
				Nodes: []graph.Node{b.nodes["node-seed"]},
			},
			Truncated: true,
		}, nil
	}
	if b.omittedOnly {
		edgeID := shoal.ID(fmt.Sprintf("edge-omitted-%04d", b.calls))
		return explorer.BoundedNeighborhood{
			Neighborhood: explorer.Neighborhood{
				Nodes: []graph.Node{b.nodes["node-seed"]},
			},
			Truncated: true, NextAfterEdgeID: edgeID, Continuation: true,
		}, nil
	}
	if request.AfterEdgeID == "" && !b.hiddenOnly && request.Fanout >= 2 {
		return explorer.BoundedNeighborhood{
			Neighborhood: explorer.Neighborhood{
				Nodes: []graph.Node{
					b.nodes["node-seed"],
					{ID: "node-hidden", Kind: "entity"},
					b.nodes["node-visible"],
				},
				Edges: []graph.Edge{{
					ID: "edge-a-hidden", From: "node-seed", To: "node-hidden",
					Type: "related", Weight: 1,
				}, {
					ID: "edge-b-visible", From: "node-seed", To: "node-visible",
					Type: "related", Weight: 1,
				}},
			},
		}, nil
	}
	if request.AfterEdgeID == "" || b.hiddenOnly {
		hiddenEdgeID := shoal.ID("edge-a-hidden")
		if b.hiddenOnly {
			hiddenEdgeID = shoal.ID(fmt.Sprintf("edge-hidden-%04d", b.calls))
		}
		return explorer.BoundedNeighborhood{
			Neighborhood: explorer.Neighborhood{
				Nodes: []graph.Node{
					b.nodes["node-seed"],
					{ID: "node-hidden", Kind: "entity"},
				},
				Edges: []graph.Edge{{
					ID: hiddenEdgeID, From: "node-seed", To: "node-hidden",
					Type: "related", Weight: 1,
				}},
			},
			Truncated: true, NextAfterEdgeID: hiddenEdgeID, Continuation: true,
		}, nil
	}
	return explorer.BoundedNeighborhood{Neighborhood: explorer.Neighborhood{
		Nodes: []graph.Node{
			b.nodes["node-seed"],
			b.nodes["node-visible"],
		},
		Edges: []graph.Edge{{
			ID: "edge-b-visible", From: "node-seed", To: "node-visible",
			Type: "related", Weight: 1,
		}},
	}}, nil
}

type resolverFunc func(context.Context) (auth.Decision, error)

func (f resolverFunc) Resolve(ctx context.Context) (auth.Decision, error) {
	return f(ctx)
}

type generationReaderFunc func(context.Context, []byte) (int64, error)

func (f generationReaderFunc) CurrentPolicyGeneration(ctx context.Context, domain []byte) (int64, error) {
	return f(ctx, domain)
}
