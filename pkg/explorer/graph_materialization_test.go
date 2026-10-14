// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file distributed
// with this work for additional information regarding copyright ownership.
package explorer_test

import (
	"context"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestGraphMaterializationIsDeterministicIdempotentAndDurable(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	corpus, err := explorer.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := corpus.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	request := graphRequest(snapshot)
	first, err := corpus.MaterializeGraph(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Disposition != explorer.IngestApplied ||
		first.Snapshot.ID == snapshot.ID {
		t.Fatalf("first result = %#v", first)
	}
	replay, err := corpus.MaterializeGraph(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if replay.Disposition != explorer.IngestUnchanged ||
		replay.MaterializationID != first.MaterializationID {
		t.Fatalf("replay = %#v", replay)
	}
	reordered := request
	reordered.Nodes = []explorer.GraphNodeSpec{request.Nodes[1], request.Nodes[0]}
	reordered.Relations = []explorer.GraphRelationSpec{
		request.Relations[1], request.Relations[0],
	}
	reordered.ExpectedSnapshot = first.Snapshot
	deterministic, err := corpus.MaterializeGraph(ctx, reordered)
	if err != nil {
		t.Fatal(err)
	}
	if deterministic.Disposition != explorer.IngestUnchanged ||
		deterministic.MaterializationID != first.MaterializationID {
		t.Fatalf("reordered replay = %#v", deterministic)
	}
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := explorer.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	neighborhood, err := reopened.Neighborhood(ctx, explorer.NeighborhoodRequest{
		NodeIDs: []shoal.ID{first.Nodes[0].ID}, Depth: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(neighborhood.Nodes) != 2 || len(neighborhood.Edges) != 2 {
		t.Fatalf("durable neighborhood = %#v", neighborhood)
	}
}

func TestGraphMaterializationRejectsConflictReferencesAndBounds(t *testing.T) {
	ctx := context.Background()
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	snapshot, _ := corpus.Snapshot(ctx)
	request := graphRequest(snapshot)
	first, err := corpus.MaterializeGraph(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	divergent := request
	divergent.Nodes = append([]explorer.GraphNodeSpec(nil), request.Nodes...)
	divergent.Nodes[0].Kind = "other"
	divergent.ExpectedSnapshot = first.Snapshot
	if _, err := corpus.MaterializeGraph(ctx, divergent); !shoal.IsErrorCode(
		err, shoal.ErrorConflict,
	) {
		t.Fatalf("divergent error = %v", err)
	}
	missing := graphRequest(first.Snapshot)
	missing.Namespace = []byte("other")
	missing.Relations[0].To = []byte("missing")
	if _, err := corpus.MaterializeGraph(ctx, missing); !shoal.IsErrorCode(
		err, shoal.ErrorInvalidArgument,
	) {
		t.Fatalf("missing reference error = %v", err)
	}
	oversized := graphRequest(first.Snapshot)
	oversized.Namespace = []byte("oversized")
	oversized.Nodes[0].Properties = shoal.Metadata{
		"value": strings.Repeat("x", explorer.MaxGraphMaterializationBytes),
	}
	if _, err := corpus.MaterializeGraph(ctx, oversized); !shoal.IsErrorCode(
		err, shoal.ErrorInvalidArgument,
	) {
		t.Fatalf("oversized error = %v", err)
	}
	tooManyNodes := graphRequest(first.Snapshot)
	tooManyNodes.Namespace = []byte("too-many-nodes")
	tooManyNodes.Nodes = make(
		[]explorer.GraphNodeSpec, explorer.MaxGraphMaterializationNodes+1)
	for index := range tooManyNodes.Nodes {
		tooManyNodes.Nodes[index] = explorer.GraphNodeSpec{
			Key: []byte{byte(index >> 8), byte(index)}, Kind: "person",
		}
	}
	if _, err := corpus.MaterializeGraph(ctx, tooManyNodes); !shoal.IsErrorCode(
		err, shoal.ErrorInvalidArgument,
	) {
		t.Fatalf("node bound error = %v", err)
	}
	tooManyRelations := graphRequest(first.Snapshot)
	tooManyRelations.Namespace = []byte("too-many-relations")
	tooManyRelations.Relations = make(
		[]explorer.GraphRelationSpec,
		explorer.MaxGraphMaterializationRelations+1)
	for index := range tooManyRelations.Relations {
		tooManyRelations.Relations[index] = explorer.GraphRelationSpec{
			Key: []byte{byte(index >> 8), byte(index)}, From: []byte("person"),
			To: []byte("team"), Type: "member_of",
		}
	}
	if _, err := corpus.MaterializeGraph(
		ctx, tooManyRelations,
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("relation bound error = %v", err)
	}
	stale := graphRequest(snapshot)
	stale.Namespace = []byte("stale")
	if _, err := corpus.MaterializeGraph(ctx, stale); !shoal.IsErrorCode(
		err, shoal.ErrorConflict,
	) {
		t.Fatalf("stale snapshot error = %v", err)
	}
}

func graphRequest(snapshot explorer.Snapshot) explorer.GraphMaterializationRequest {
	return explorer.GraphMaterializationRequest{
		Namespace:         []byte{0, 'd', 0xff},
		IdentityNamespace: []byte{0xfe, 'p'}, MutationID: "mutation-1",
		ExpectedSnapshot: snapshot,
		Nodes: []explorer.GraphNodeSpec{
			{Key: []byte("team"), Kind: "team", Properties: shoal.Metadata{"name": "Team"}},
			{Key: []byte("person"), Kind: "person", Properties: shoal.Metadata{"name": "Person"}},
		},
		Relations: []explorer.GraphRelationSpec{
			{Key: []byte("membership"), From: []byte("person"), To: []byte("team"), Type: "member_of"},
			{Key: []byte("reverse"), From: []byte("team"), To: []byte("person"), Type: "contains"},
		},
	}
}
