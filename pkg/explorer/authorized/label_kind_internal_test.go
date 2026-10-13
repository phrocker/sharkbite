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
	"testing"

	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// The pre-RegistrationKind record layouts, byte-for-byte as they were
// persisted before the Kind field existed. gob matches fields by name, so
// these encode exactly what an existing store holds.
type legacyPersistedEdge struct {
	Seq        uint64
	Tombstone  bool
	Edge       persistedGraphEdge
	DocumentID string
	RevisionID string
	Rule       persistedRule
}

type legacyPersistedNode struct {
	Seq        uint64
	NodeID     string
	DocumentID string
	RevisionID string
	Node       persistedGraphNode
	Rule       persistedRule
}

func writeLegacyRecord(t *testing.T, dir string, row []byte, kind byte, value any) {
	t.Helper()
	encoded, err := encodePolicyRecord(kind, value)
	if err != nil {
		t.Fatal(err)
	}
	corruptPolicyRow(t, dir, row, encoded)
}

// TestRegistrationKindLegacyDecode pins the explicit legacy path: records
// persisted before RegistrationKind existed decode with a kind inferred once
// at load, and records written now round-trip their stored kind.
func TestRegistrationKindLegacyDecode(t *testing.T) {
	dir := t.TempDir()
	created, err := OpenDurablePolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := created.Close(); err != nil {
		t.Fatal(err)
	}
	rule := mustPersistRuleForTest(t)
	const materialized = legacyMaterializationOwnerPrefix + "abc"
	writeLegacyRecord(t, dir, policyNodeRow("legacy-entity"), policyKindNode,
		legacyPersistedNode{Seq: 1, NodeID: "legacy-entity", DocumentID: "doc_x",
			RevisionID: "rev_x", Rule: rule,
			Node: persistedGraphNode{ID: "legacy-entity", Kind: "skill"}})
	writeLegacyRecord(t, dir, policyNodeRow("legacy-team"), policyKindNode,
		legacyPersistedNode{Seq: 2, NodeID: "legacy-team", DocumentID: materialized,
			RevisionID: "mutation", Rule: rule,
			Node: persistedGraphNode{ID: "legacy-team", Kind: "team"}})
	writeLegacyRecord(t, dir, policyEdgeRow("legacy-edge"), policyKindEdge,
		legacyPersistedEdge{Seq: 3, Rule: rule, Edge: persistedGraphEdge{
			ID: "legacy-edge", From: "legacy-entity", To: "legacy-team",
			Type: "provides_tool", Weight: 1}})

	store, err := OpenDurablePolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for nodeID, want := range map[shoal.ID]RegistrationKind{
		"legacy-entity": RegistrationExtracted,
		"legacy-team":   RegistrationMaterialized,
	} {
		registration, ok, err := store.Node(ctx, nodeID)
		if err != nil || !ok || registration.Kind != want {
			t.Fatalf("legacy node %s = %+v (%v, %v), want kind %d", nodeID, registration, ok, err, want)
		}
	}
	edge, ok, err := store.Edge(ctx, "legacy-edge")
	if err != nil || !ok || edge.Kind != RegistrationApplication {
		t.Fatalf("legacy edge = %+v (%v, %v), want application", edge, ok, err)
	}

	// Re-extracting a legacy edge binds it to its asserting document: the
	// extracted registration replaces the document-less one.
	extracted := EdgeRegistration{
		Edge: edge.Edge, DocumentID: "doc_x", RevisionID: "rev_x",
		Rule: edge.Rule, Kind: RegistrationExtracted,
	}
	if err := store.PutEdge(ctx, extracted); err != nil {
		t.Fatalf("re-extracting a legacy edge: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenDurablePolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	edge, ok, err = reopened.Edge(ctx, "legacy-edge")
	if err != nil || !ok || edge.Kind != RegistrationExtracted ||
		edge.DocumentID != "doc_x" || edge.RevisionID != "rev_x" {
		t.Fatalf("re-extracted edge after reload = %+v (%v, %v)", edge, ok, err)
	}
}

func TestRegistrationKindIsRequiredAndTyped(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryPolicyStore()
	rule := batchTestRule(t, "source", "policy")
	other := batchTestRule(t, "source", "other-policy")
	node := graph.Node{ID: "entity", Kind: "skill"}
	if err := store.PutNode(ctx, "entity", NodeRegistration{
		DocumentID: "doc", RevisionID: "rev", Node: node, Rule: rule,
	}); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("PutNode without a kind = %v, want invalid argument", err)
	}
	if err := store.PutNode(ctx, "entity", NodeRegistration{
		DocumentID: "doc", RevisionID: "rev", Node: node, Rule: rule,
		Kind: RegistrationDocument,
	}); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("PutNode as a document node = %v, want invalid argument", err)
	}
	edge := graph.Edge{ID: "relation", From: "a", To: "b", Type: "provides_tool", Weight: 1}
	if err := store.PutEdge(ctx, EdgeRegistration{
		Edge: edge, Rule: rule, Kind: RegistrationExtracted,
	}); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("extracted edge without its document = %v, want invalid argument", err)
	}

	first := EdgeRegistration{Edge: edge, DocumentID: "doc-1", RevisionID: "rev-1",
		Rule: rule, Kind: RegistrationExtracted}
	if err := store.PutEdge(ctx, first); err != nil {
		t.Fatal(err)
	}
	// A second asserter leaves the first in place and becomes a co-owner
	// (#700).
	second := first
	second.DocumentID, second.RevisionID = "doc-2", "rev-2"
	if err := store.PutEdge(ctx, second); err != nil {
		t.Fatalf("second asserter of a shared edge: %v", err)
	}
	stored, _, err := store.Edge(ctx, "relation")
	if err != nil || stored.DocumentID != "doc-1" || len(stored.CoOwners) != 1 ||
		stored.CoOwners[0].DocumentID != "doc-2" {
		t.Fatalf("shared edge owners = %+v (%v), want doc-1 then doc-2", stored, err)
	}
	// The same asserter under a different rule, or an application edge over
	// an extracted one, conflicts.
	differentRule := second
	differentRule.Rule = other
	if err := store.PutEdge(ctx, differentRule); !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("shared edge under another rule = %v, want conflict", err)
	}
	if err := store.PutEdge(ctx, EdgeRegistration{
		Edge: edge, Rule: rule, Kind: RegistrationApplication,
	}); !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("application edge over an extracted one = %v, want conflict", err)
	}
}
