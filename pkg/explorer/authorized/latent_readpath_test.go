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
	"strconv"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type countingCanonicalBase struct {
	*explorer.Explorer
	documentsCalls int
	documentCalls  int
}

type pagedCountingCanonicalBase struct {
	*countingCanonicalBase
	pages     int
	pageCalls int
}

type mutatingDocumentsCanonicalBase struct {
	*explorer.Explorer
	documentsCalls int
	mutated        bool
}

func (b *countingCanonicalBase) Documents(
	ctx context.Context,
) ([]explorer.DocumentSummary, error) {
	b.documentsCalls++
	return b.Explorer.Documents(ctx)
}

func (b *countingCanonicalBase) Document(
	ctx context.Context, documentID, revisionID shoal.ID,
) (explorer.DocumentView, error) {
	b.documentCalls++
	return b.Explorer.Document(ctx, documentID, revisionID)
}

func (b *mutatingDocumentsCanonicalBase) Documents(
	ctx context.Context,
) ([]explorer.DocumentSummary, error) {
	documents, err := b.Explorer.Documents(ctx)
	b.documentsCalls++
	if err != nil || b.mutated || b.documentsCalls < 2 {
		return documents, err
	}
	b.mutated = true
	return documents, nil
}

func (b *mutatingDocumentsCanonicalBase) Snapshot(
	ctx context.Context,
) (explorer.Snapshot, error) {
	snapshot, err := b.Explorer.Snapshot(ctx)
	if err == nil && b.mutated {
		snapshot.ID += "-after-documents"
		snapshot.Frontier++
	}
	return snapshot, err
}

func (b *pagedCountingCanonicalBase) BoundedNeighborhood(
	ctx context.Context,
	request explorer.BoundedNeighborhoodRequest,
) (explorer.BoundedNeighborhood, error) {
	sourceRequest := request
	sourceRequest.AfterEdgeID = ""
	result, err := b.Explorer.BoundedNeighborhood(ctx, sourceRequest)
	if err != nil {
		return explorer.BoundedNeighborhood{}, err
	}
	b.pageCalls++
	result.ScannedEdges = uint32(len(result.Neighborhood.Edges))
	result.ScannedEdgesKnown = true
	result.Continuation = b.pageCalls < b.pages
	result.Truncated = result.Continuation
	result.NextAfterEdgeID = ""
	if result.Continuation {
		result.NextAfterEdgeID = shoal.ID(
			"forced-canonical-page-" + strconv.Itoa(b.pageCalls))
	}
	return result, nil
}

func TestAuthorizedDerivedAssertionsShareCanonicalNodeHydration(t *testing.T) {
	f := newFixture(t)
	admin := f.admin(t)
	source, err := f.clientA.Ingest(admin, explorer.Source{
		URI: "file:///batched-assertion-source.txt", MediaType: explorer.MediaTypeText,
		Content: "batched assertion source",
	})
	if err != nil {
		t.Fatal(err)
	}
	target, err := f.clientA.Ingest(admin, explorer.Source{
		URI: "file:///batched-assertion-target.txt", MediaType: explorer.MediaTypeText,
		Content: strings.Repeat("batched assertion target\n\n", 16),
	})
	if err != nil {
		t.Fatal(err)
	}
	targetView, err := f.base.Document(
		admin, target.Document.ID, target.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	targetIDs := []shoal.ID{
		target.Document.ID, targetView.Root.Section.ID,
	}
	for _, span := range targetView.Root.Spans {
		targetIDs = append(targetIDs, span.ID)
	}
	cells := make([]explorer.LatentLinkCell, 0, len(targetIDs))
	for _, targetID := range targetIDs {
		cells = append(cells, authorizedLatentCell(
			source.Document.ID, targetID, int64(len(cells)+1)))
	}
	if err := f.base.PutLatentLinkCells(admin, cells); err != nil {
		t.Fatal(err)
	}
	counted := &countingCanonicalBase{Explorer: f.base}
	base := &pagedCountingCanonicalBase{
		countingCanonicalBase: counted,
		pages:                 1,
	}
	client := f.newClient(t, base, f.store, f.sourceA, f.policyA, nil)
	result, err := client.BoundedNeighborhood(
		f.alice(t),
		explorer.BoundedNeighborhoodRequest{
			NodeIDs: []shoal.ID{source.Document.ID},
			Depth:   2, Fanout: 64, MaxNodes: 64,
			MaxScannedEdges: 256,
			EdgeTypes:       []string{authorizedLatentEdgeType(t)},
			Direction:       explorer.GraphDirectionBoth,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Neighborhood.Assertions) != len(cells) {
		t.Fatalf(
			"authorized assertions = %d, want %d",
			len(result.Neighborhood.Assertions), len(cells),
		)
	}
	// Depth two expands every target with its own bounded scan (#698); the
	// canonical hydration below must still be shared across those scans.
	if base.pageCalls != len(cells)+1 {
		t.Fatalf("bounded scan calls = %d, want %d", base.pageCalls, len(cells)+1)
	}
	if counted.documentsCalls != 2 || counted.documentCalls != 3 {
		t.Fatalf(
			"canonical hydration calls = Documents:%d Document:%d, want 2 and 3",
			counted.documentsCalls, counted.documentCalls,
		)
	}
}

func TestAuthorizedBoundedSnapshotPrecedesCanonicalIndex(t *testing.T) {
	f := newFixture(t)
	source, err := f.clientA.Ingest(f.admin(t), explorer.Source{
		URI:       "file:///snapshot-ordering-source.txt",
		MediaType: explorer.MediaTypeText,
		Content:   "snapshot ordering source",
	})
	if err != nil {
		t.Fatal(err)
	}
	base := &mutatingDocumentsCanonicalBase{
		Explorer: f.base,
	}
	client := f.newClient(t, base, f.store, f.sourceA, f.policyA, nil)
	_, err = client.BoundedNeighborhood(
		f.alice(t),
		explorer.BoundedNeighborhoodRequest{
			NodeIDs:         []shoal.ID{source.Document.ID},
			Depth:           1,
			Fanout:          10,
			MaxNodes:        10,
			MaxScannedEdges: 10,
			Direction:       explorer.GraphDirectionBoth,
		},
	)
	if !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("bounded snapshot ordering error = %v, want conflict", err)
	}
	if !base.mutated {
		t.Fatal("canonical index did not trigger test mutation")
	}
}

func TestAuthorizedLatentAssertionWithholdsUnauthorizedTarget(t *testing.T) {
	f := newFixture(t)
	admin := f.admin(t)
	visible, err := f.clientA.Ingest(admin, explorer.Source{
		URI: "file:///latent-visible-source.txt", MediaType: explorer.MediaTypeText,
		Content: "visible source",
	})
	if err != nil {
		t.Fatal(err)
	}
	hidden, err := f.clientB.Ingest(admin, explorer.Source{
		URI: "file:///latent-hidden-target.txt", MediaType: explorer.MediaTypeText,
		Content: "hidden target",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.base.PutLatentLinkCells(admin, []explorer.LatentLinkCell{
		authorizedLatentCell(visible.Document.ID, hidden.Document.ID, 42),
	}); err != nil {
		t.Fatal(err)
	}

	alice, err := f.clientA.BoundedNeighborhood(
		f.alice(t),
		explorer.BoundedNeighborhoodRequest{
			NodeIDs: []shoal.ID{visible.Document.ID},
			Depth:   1, Fanout: 8, MaxNodes: 8,
			EdgeTypes: []string{authorizedLatentEdgeType(t)},
			Direction: explorer.GraphDirectionOutgoing,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(alice.Neighborhood.Edges) != 0 ||
		len(alice.Neighborhood.Assertions) != 0 ||
		hasNode(alice.Neighborhood, hidden.Document.ID) {
		t.Fatalf("unauthorized target leaked through latent assertion: %+v", alice.Neighborhood)
	}

	all, err := f.clientA.BoundedNeighborhood(
		admin,
		explorer.BoundedNeighborhoodRequest{
			NodeIDs: []shoal.ID{visible.Document.ID},
			Depth:   1, Fanout: 8, MaxNodes: 8,
			EdgeTypes: []string{authorizedLatentEdgeType(t)},
			Direction: explorer.GraphDirectionOutgoing,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Neighborhood.Assertions) != 1 ||
		all.Neighborhood.Assertions[0].Origin() != ontology.AssertionDerived ||
		!hasNode(all.Neighborhood, hidden.Document.ID) {
		t.Fatalf("admin did not see derived assertion: %+v", all.Neighborhood)
	}
}

func TestAuthorizedLatentAssertionWithholdsUnauthorizedSource(t *testing.T) {
	f := newFixture(t)
	admin := f.admin(t)
	hidden, err := f.clientB.Ingest(admin, explorer.Source{
		URI: "file:///latent-hidden-source.txt", MediaType: explorer.MediaTypeText,
		Content: "hidden source",
	})
	if err != nil {
		t.Fatal(err)
	}
	visible, err := f.clientA.Ingest(admin, explorer.Source{
		URI: "file:///latent-visible-target.txt", MediaType: explorer.MediaTypeText,
		Content: "visible target",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.base.PutLatentLinkCells(admin, []explorer.LatentLinkCell{
		authorizedLatentCell(hidden.Document.ID, visible.Document.ID, 42),
	}); err != nil {
		t.Fatal(err)
	}

	alice, err := f.clientA.BoundedNeighborhood(
		f.alice(t),
		explorer.BoundedNeighborhoodRequest{
			NodeIDs: []shoal.ID{visible.Document.ID},
			Depth:   1, Fanout: 8, MaxNodes: 8,
			EdgeTypes: []string{authorizedLatentEdgeType(t)},
			Direction: explorer.GraphDirectionIncoming,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(alice.Neighborhood.Edges) != 0 ||
		len(alice.Neighborhood.Assertions) != 0 ||
		hasNode(alice.Neighborhood, hidden.Document.ID) {
		t.Fatalf("unauthorized source leaked through latent assertion: %+v", alice.Neighborhood)
	}

	all, err := f.clientA.BoundedNeighborhood(
		admin,
		explorer.BoundedNeighborhoodRequest{
			NodeIDs: []shoal.ID{visible.Document.ID},
			Depth:   1, Fanout: 8, MaxNodes: 8,
			EdgeTypes: []string{authorizedLatentEdgeType(t)},
			Direction: explorer.GraphDirectionIncoming,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Neighborhood.Assertions) != 1 ||
		all.Neighborhood.Assertions[0].Origin() != ontology.AssertionDerived ||
		!hasNode(all.Neighborhood, hidden.Document.ID) {
		t.Fatalf("admin did not see incoming derived assertion: %+v", all.Neighborhood)
	}
}

func TestAuthorizedProducerProvenanceDoesNotAggregateHiddenAssertions(t *testing.T) {
	f := newFixture(t)
	admin := f.admin(t)
	source, err := f.clientA.Ingest(admin, explorer.Source{
		URI: "file:///producer-visible-source.txt", MediaType: explorer.MediaTypeText,
		Content: "visible producer source",
	})
	if err != nil {
		t.Fatal(err)
	}
	target, err := f.clientA.Ingest(admin, explorer.Source{
		URI: "file:///producer-visible-target.txt", MediaType: explorer.MediaTypeText,
		Content: "visible producer target",
	})
	if err != nil {
		t.Fatal(err)
	}
	bobSource, err := f.clientB.Ingest(admin, explorer.Source{
		URI: "file:///producer-bob-source.txt", MediaType: explorer.MediaTypeText,
		Content: "bob producer source",
	})
	if err != nil {
		t.Fatal(err)
	}
	bobTarget, err := f.clientB.Ingest(admin, explorer.Source{
		URI: "file:///producer-bob-target.txt", MediaType: explorer.MediaTypeText,
		Content: "bob producer target",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.base.PutLatentLinkCells(admin, []explorer.LatentLinkCell{
		authorizedLatentCell(source.Document.ID, target.Document.ID, 42),
		authorizedLatentCell(bobSource.Document.ID, bobTarget.Document.ID, 43),
	}); err != nil {
		t.Fatal(err)
	}

	aliceSource, err := f.clientA.BoundedNeighborhood(
		f.alice(t),
		explorer.BoundedNeighborhoodRequest{
			NodeIDs: []shoal.ID{source.Document.ID},
			Depth:   1, Fanout: 8, MaxNodes: 8,
			EdgeTypes: []string{authorizedLatentEdgeType(t)},
			Direction: explorer.GraphDirectionOutgoing,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(aliceSource.Neighborhood.Assertions) != 1 {
		t.Fatalf("alice source graph = %+v, want one derived assertion",
			aliceSource.Neighborhood)
	}
	assertionID := aliceSource.Neighborhood.Assertions[0].ID()
	aliceAssertion, err := f.clientA.BoundedNeighborhood(
		f.alice(t),
		explorer.BoundedNeighborhoodRequest{
			NodeIDs: []shoal.ID{assertionID},
			Depth:   1, Fanout: 8, MaxNodes: 8,
			EdgeTypes: []string{graph.EdgeTypeProduced},
			Direction: explorer.GraphDirectionIncoming,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(aliceAssertion.Neighborhood.Edges) != 1 ||
		len(aliceAssertion.Neighborhood.Assertions) != 1 {
		t.Fatalf("alice assertion provenance = %+v, want one producer edge",
			aliceAssertion.Neighborhood)
	}
	producerID := aliceAssertion.Neighborhood.Edges[0].From
	aliceProducer, err := f.clientA.BoundedNeighborhood(
		f.alice(t),
		explorer.BoundedNeighborhoodRequest{
			NodeIDs: []shoal.ID{producerID},
			Depth:   1, Fanout: 8, MaxNodes: 8,
			EdgeTypes: []string{graph.EdgeTypeProduced},
			Direction: explorer.GraphDirectionOutgoing,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(aliceProducer.Neighborhood.Edges) != 1 ||
		len(aliceProducer.Neighborhood.Assertions) != 1 ||
		aliceProducer.Neighborhood.Assertions[0].ID() != assertionID {
		t.Fatalf("alice producer graph = %+v, want only her produced assertion",
			aliceProducer.Neighborhood)
	}

	bobProducer, err := f.clientA.BoundedNeighborhood(
		f.bob(t),
		explorer.BoundedNeighborhoodRequest{
			NodeIDs: []shoal.ID{producerID},
			Depth:   1, Fanout: 8, MaxNodes: 8,
			EdgeTypes: []string{graph.EdgeTypeProduced},
			Direction: explorer.GraphDirectionOutgoing,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(bobProducer.Neighborhood.Edges) != 1 ||
		len(bobProducer.Neighborhood.Assertions) != 1 ||
		bobProducer.Neighborhood.Assertions[0].ID() == assertionID {
		t.Fatalf("bob producer graph = %+v, want only bob's produced assertion",
			bobProducer.Neighborhood)
	}
	adminProducer, err := f.clientA.BoundedNeighborhood(
		admin,
		explorer.BoundedNeighborhoodRequest{
			NodeIDs: []shoal.ID{producerID},
			Depth:   1, Fanout: 8, MaxNodes: 8,
			EdgeTypes: []string{graph.EdgeTypeProduced},
			Direction: explorer.GraphDirectionOutgoing,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(adminProducer.Neighborhood.Edges) != 2 ||
		len(adminProducer.Neighborhood.Assertions) != 2 {
		t.Fatalf("admin producer graph = %+v, want both produced assertions",
			adminProducer.Neighborhood)
	}
	noAccess := f.context(t, f.decision(
		t, "no-access", nil, nil, allOperations))
	_, err = f.clientA.BoundedNeighborhood(
		noAccess,
		explorer.BoundedNeighborhoodRequest{
			NodeIDs: []shoal.ID{producerID},
			Depth:   1, Fanout: 8, MaxNodes: 8,
			EdgeTypes: []string{graph.EdgeTypeProduced},
			Direction: explorer.GraphDirectionOutgoing,
		},
	)
	if !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("no-access producer error = %v, want not found instead of empty results", err)
	}
	empty, err := f.clientA.BoundedNeighborhood(
		f.alice(t),
		explorer.BoundedNeighborhoodRequest{
			NodeIDs: []shoal.ID{target.Document.ID},
			Depth:   1, Fanout: 8, MaxNodes: 8,
			EdgeTypes: []string{graph.EdgeTypeProduced},
			Direction: explorer.GraphDirectionOutgoing,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.Neighborhood.Edges) != 0 {
		t.Fatalf("genuine empty provenance graph = %+v, want no edges", empty.Neighborhood)
	}
}

func TestAuthorizedUnboundedNeighborhoodProducerSeedRequiresAuthorization(t *testing.T) {
	f := newFixture(t)
	admin := f.admin(t)
	source, err := f.clientA.Ingest(admin, explorer.Source{
		URI: "file:///unbounded-producer-visible-source.txt", MediaType: explorer.MediaTypeText,
		Content: "visible unbounded producer source",
	})
	if err != nil {
		t.Fatal(err)
	}
	target, err := f.clientA.Ingest(admin, explorer.Source{
		URI: "file:///unbounded-producer-visible-target.txt", MediaType: explorer.MediaTypeText,
		Content: "visible unbounded producer target",
	})
	if err != nil {
		t.Fatal(err)
	}
	bobSource, err := f.clientB.Ingest(admin, explorer.Source{
		URI: "file:///unbounded-producer-bob-source.txt", MediaType: explorer.MediaTypeText,
		Content: "bob unbounded producer source",
	})
	if err != nil {
		t.Fatal(err)
	}
	bobTarget, err := f.clientB.Ingest(admin, explorer.Source{
		URI: "file:///unbounded-producer-bob-target.txt", MediaType: explorer.MediaTypeText,
		Content: "bob unbounded producer target",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.base.PutLatentLinkCells(admin, []explorer.LatentLinkCell{
		authorizedLatentCell(source.Document.ID, target.Document.ID, 42),
		authorizedLatentCell(bobSource.Document.ID, bobTarget.Document.ID, 43),
	}); err != nil {
		t.Fatal(err)
	}
	aliceSource, err := f.clientA.Neighborhood(
		f.alice(t),
		explorer.NeighborhoodRequest{
			NodeIDs: []shoal.ID{source.Document.ID},
			Depth:   1,
			EdgeTypes: []string{
				authorizedLatentEdgeType(t),
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(aliceSource.Assertions) != 1 {
		t.Fatalf("alice unbounded source graph = %+v, want one derived assertion",
			aliceSource)
	}
	assertionID := aliceSource.Assertions[0].ID()
	aliceAssertion, err := f.clientA.Neighborhood(
		f.alice(t),
		explorer.NeighborhoodRequest{
			NodeIDs:   []shoal.ID{assertionID},
			Depth:     1,
			EdgeTypes: []string{graph.EdgeTypeProduced},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(aliceAssertion.Edges) != 1 || len(aliceAssertion.Assertions) != 1 {
		t.Fatalf("alice unbounded assertion provenance = %+v, want one producer edge",
			aliceAssertion)
	}
	producerID := aliceAssertion.Edges[0].From
	aliceProducer, err := f.clientA.Neighborhood(
		f.alice(t),
		explorer.NeighborhoodRequest{
			NodeIDs:   []shoal.ID{producerID},
			Depth:     1,
			EdgeTypes: []string{graph.EdgeTypeProduced},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(aliceProducer.Edges) != 1 ||
		len(aliceProducer.Assertions) != 1 ||
		aliceProducer.Assertions[0].ID() != assertionID {
		t.Fatalf("alice unbounded producer graph = %+v, want only her produced assertion",
			aliceProducer)
	}

	noAccess := f.context(t, f.decision(
		t, "unbounded-no-access", nil, nil, allOperations))
	_, err = f.clientA.Neighborhood(
		noAccess,
		explorer.NeighborhoodRequest{
			NodeIDs:   []shoal.ID{producerID},
			Depth:     1,
			EdgeTypes: []string{graph.EdgeTypeProduced},
		},
	)
	if !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("no-access unbounded producer error = %v, want not found instead of empty results", err)
	}
}

func TestAuthorizedFabricatedProducerPrefixDoesNotBypassAuthorization(t *testing.T) {
	f := newFixture(t)
	prefixedID := shoal.ID("producer_not-a-real-producer")
	ordinaryID := shoal.ID("not-a-real-node")
	_, prefixedErr := f.clientA.Neighborhood(
		f.alice(t),
		explorer.NeighborhoodRequest{
			NodeIDs:   []shoal.ID{prefixedID},
			Depth:     1,
			EdgeTypes: []string{graph.EdgeTypeProduced},
		},
	)
	_, ordinaryErr := f.clientA.Neighborhood(
		f.alice(t),
		explorer.NeighborhoodRequest{
			NodeIDs:   []shoal.ID{ordinaryID},
			Depth:     1,
			EdgeTypes: []string{graph.EdgeTypeProduced},
		},
	)
	if !shoal.IsErrorCode(prefixedErr, shoal.ErrorNotFound) ||
		!shoal.IsErrorCode(ordinaryErr, shoal.ErrorNotFound) ||
		prefixedErr.Error() != ordinaryErr.Error() {
		t.Fatalf("fabricated producer error = %v, ordinary missing error = %v; want same not-found shape",
			prefixedErr, ordinaryErr)
	}
}

func authorizedLatentEdgeType(t *testing.T) string {
	t.Helper()
	projection, err := explorer.DefaultLatentLinkAssertionProjection()
	if err != nil {
		t.Fatal(err)
	}
	return string(projection.Predicate)
}

func authorizedLatentCell(source, target shoal.ID, timestamp int64) explorer.LatentLinkCell {
	return explorer.LatentLinkCell{
		Row:             []byte("cell-a:" + string(source)),
		ColumnFamily:    []byte("link"),
		ColumnQualifier: []byte(target),
		Timestamp:       timestamp,
		Value:           []byte("0.91"),
	}
}
