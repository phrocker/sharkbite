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
	"bytes"
	"context"
	"reflect"
	"testing"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// seedDurableStore fills a fresh durable store with one current revision (two
// nodes and one intrinsic edge) plus one application edge, so the reload path
// has nodes, intrinsic edges, application edges, and current-projection state
// to reconstruct.
func seedDurableStore(t *testing.T, store *DurablePolicyStore) {
	t.Helper()
	ctx := context.Background()
	rule := batchTestRule(t, "source", "policy")
	if err := store.PutRevision(ctx, RevisionRegistration{
		DocumentID: "document",
		RevisionID: "revision",
		NodeIDs:    []shoal.ID{"document", "node-a"},
		IntrinsicEdges: []graph.Edge{{
			ID: "intrinsic", From: "document", To: "node-a",
			Type: "contains", Weight: 1,
			Properties: shoal.Metadata{"key": "value"},
		}},
		ContentDigest: auth.DigestBytes("test-content", []byte("revision")),
		Rule:          rule,
		Current:       true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutEdge(ctx, EdgeRegistration{
		Edge: graph.Edge{
			ID: "application-edge", From: "document", To: "node-a",
			Type: "links", Weight: 1,
			Properties: shoal.Metadata{"edge": "value"},
		},
		Rule: rule,
	}); err != nil {
		t.Fatal(err)
	}
}

// TestDurablePolicyStoreReloadPreservesBatchSemantics reopens a persisted
// store and proves the reconstructed catalog answers batched revision, node,
// and edge requests exactly as their point-by-point loops would.
func TestDurablePolicyStoreReloadPreservesBatchSemantics(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenDurablePolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	seedDurableStore(t, store)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenDurablePolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	ctx := context.Background()

	requestedDocuments := []shoal.ID{"document", "document", "unregistered"}
	batchRevisions, err := reopened.CurrentRevisions(ctx, requestedDocuments)
	if err != nil {
		t.Fatal(err)
	}
	expectedRevisions := make(map[shoal.ID]RevisionRegistration)
	for _, documentID := range requestedDocuments {
		registration, ok, err := reopened.CurrentRevision(ctx, documentID)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			if _, present := batchRevisions[documentID]; present {
				t.Fatalf(
					"batch reported unregistered document %q after reload",
					documentID)
			}
			continue
		}
		expectedRevisions[documentID] = registration
	}
	if !reflect.DeepEqual(batchRevisions, expectedRevisions) {
		t.Fatalf("reloaded revision batch = %#v, want %#v",
			batchRevisions, expectedRevisions)
	}
	if len(batchRevisions) != 1 {
		t.Fatalf("reloaded revision batch = %#v, want one document",
			batchRevisions)
	}

	requestedNodes := []shoal.ID{"document", "node-a", "node-a", "unregistered"}
	batchNodes, err := reopened.Nodes(ctx, requestedNodes)
	if err != nil {
		t.Fatal(err)
	}
	expectedNodes := make(map[shoal.ID]NodeRegistration)
	for _, nodeID := range requestedNodes {
		registration, ok, err := reopened.Node(ctx, nodeID)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			if _, present := batchNodes[nodeID]; present {
				t.Fatalf("batch reported unregistered node %q after reload", nodeID)
			}
			continue
		}
		expectedNodes[nodeID] = registration
	}
	if !reflect.DeepEqual(batchNodes, expectedNodes) {
		t.Fatalf("reloaded node batch = %#v, want %#v", batchNodes, expectedNodes)
	}
	if len(batchNodes) != 2 {
		t.Fatalf("reloaded node batch = %#v, want two distinct nodes", batchNodes)
	}

	requestedEdges := []shoal.ID{
		"intrinsic", "application-edge", "application-edge", "unregistered",
	}
	batchEdges, err := reopened.Edges(ctx, requestedEdges)
	if err != nil {
		t.Fatal(err)
	}
	expectedEdges := make(map[shoal.ID]EdgeRegistration)
	for _, edgeID := range requestedEdges {
		registration, ok, err := reopened.Edge(ctx, edgeID)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			if _, present := batchEdges[edgeID]; present {
				t.Fatalf("batch reported unregistered edge %q after reload", edgeID)
			}
			continue
		}
		expectedEdges[edgeID] = registration
	}
	if !reflect.DeepEqual(batchEdges, expectedEdges) {
		t.Fatalf("reloaded edge batch = %#v, want %#v", batchEdges, expectedEdges)
	}
	if len(batchEdges) != 2 {
		t.Fatalf("reloaded edge batch = %#v, want two distinct edges", batchEdges)
	}
}

// TestDurablePolicyStoreReloadRejectsInvalidBatchInput proves the reconstructed
// store keeps the batch entry points exactly as strict as the point lookups:
// an empty identifier and a cancelled context both fail rather than resolving.
func TestDurablePolicyStoreReloadRejectsInvalidBatchInput(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenDurablePolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	seedDurableStore(t, store)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenDurablePolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()

	if _, err := reopened.Nodes(
		context.Background(), []shoal.ID{"node-a", ""},
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("empty node identifier error = %v", err)
	}
	if _, err := reopened.Edges(
		context.Background(), []shoal.ID{"application-edge", ""},
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("empty edge identifier error = %v", err)
	}
	if _, err := reopened.CurrentRevisions(
		context.Background(), []shoal.ID{"document", ""},
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("empty document identifier error = %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := reopened.Nodes(cancelled, []shoal.ID{"document"}); err == nil {
		t.Fatal("cancelled context batch lookup succeeded")
	}
	if _, err := reopened.CurrentRevisions(
		cancelled, []shoal.ID{"document"},
	); err == nil {
		t.Fatal("cancelled context revision batch lookup succeeded")
	}
}

// corruptPolicyRow overwrites the record cell at row with raw bytes, so a
// reopen must reject the store rather than silently drop the row. The store is
// expected to be closed before this is called.
func corruptPolicyRow(t *testing.T, dir string, row []byte, value []byte) {
	t.Helper()
	eng, err := engine.Open(dir, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	mutation, err := cclient.NewMutation(row)
	if err != nil {
		eng.Close()
		t.Fatal(err)
	}
	mutation.PutLatest([]byte(policyRecordCF), []byte(policyRecordCQ), nil, value)
	if err := eng.Write(policyTable, []*cclient.Mutation{mutation}); err != nil {
		eng.Close()
		t.Fatal(err)
	}
	if err := eng.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestDurablePolicyStoreFailsClosedOnCorruptRecord proves a persisted record
// whose bytes do not decode makes the store refuse to open. A store that cannot
// open cannot serve a corpus, so a corrupt catalog denies rather than serving
// unauthorized or partial state.
func TestDurablePolicyStoreFailsClosedOnCorruptRecord(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenDurablePolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	seedDurableStore(t, store)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// Sanity: the intact store reopens. This makes the failure below
	// attributable to the corruption rather than a broken fixture.
	intact, err := OpenDurablePolicyStore(dir)
	if err != nil {
		t.Fatalf("intact store failed to reopen: %v", err)
	}
	if err := intact.Close(); err != nil {
		t.Fatal(err)
	}
	corruptPolicyRow(
		t, dir,
		policyRevisionRow(revisionKey{documentID: "document", revisionID: "revision"}),
		[]byte("this is not a valid policy record envelope at all"),
	)
	reopened, err := OpenDurablePolicyStore(dir)
	if err == nil {
		reopened.Close()
		t.Fatal("store opened over a corrupt record instead of failing closed")
	}
	if !shoal.IsErrorCode(err, shoal.ErrorInternal) {
		t.Fatalf("corrupt record error = %v, want internal", err)
	}
}

// TestDurablePolicyStoreFailsClosedOnTruncatedRecord proves a record truncated
// below the envelope header — a partial write — is rejected on reopen rather
// than read as empty or partial state.
func TestDurablePolicyStoreFailsClosedOnTruncatedRecord(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenDurablePolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	seedDurableStore(t, store)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// A valid magic followed by nothing else is shorter than the envelope
	// header, exercising the truncation guard specifically.
	corruptPolicyRow(
		t, dir,
		policyEdgeRow("application-edge"),
		[]byte(policyRecordMagic),
	)
	reopened, err := OpenDurablePolicyStore(dir)
	if err == nil {
		reopened.Close()
		t.Fatal("store opened over a truncated record instead of failing closed")
	}
	if !shoal.IsErrorCode(err, shoal.ErrorInternal) {
		t.Fatalf("truncated record error = %v, want internal", err)
	}
}

// TestDurablePolicyStoreFailsClosedOnWrongKindRecord proves a record whose
// envelope kind does not match the kind its row demands is rejected: a source
// claim cell cannot masquerade as a revision, so a tampered kind byte denies.
func TestDurablePolicyStoreFailsClosedOnWrongKindRecord(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenDurablePolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	seedDurableStore(t, store)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	wrongKind, err := encodePolicyRecord(policyKindSourceClaim, persistedSourceClaim{
		Seq: 1, SourceURI: "file:///x", Version: 1,
		Rule: mustPersistRuleForTest(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	corruptPolicyRow(
		t, dir,
		policyRevisionRow(revisionKey{documentID: "document", revisionID: "revision"}),
		wrongKind,
	)
	reopened, err := OpenDurablePolicyStore(dir)
	if err == nil {
		reopened.Close()
		t.Fatal("store opened over a wrong-kind record instead of failing closed")
	}
	if !shoal.IsErrorCode(err, shoal.ErrorInternal) {
		t.Fatalf("wrong-kind record error = %v, want internal", err)
	}
}

func mustPersistRuleForTest(t *testing.T) persistedRule {
	t.Helper()
	record, err := ruleToPersisted(batchTestRule(t, "source", "policy"))
	if err != nil {
		t.Fatal(err)
	}
	return record
}

// TestDurablePolicyStoreFailsClosedOnChecksumMismatch flips a single payload
// byte while leaving the envelope header (magic, version, kind, length) intact,
// so only the SHA-256 checksum guard can reject it. Without a verified
// checksum, a silently mutated record would decode into an authorization
// registration that never existed.
func TestDurablePolicyStoreFailsClosedOnChecksumMismatch(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenDurablePolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	seedDurableStore(t, store)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	valid, err := encodePolicyRecord(policyKindEdge, persistedEdge{
		Seq:  1,
		Edge: graphEdgeToPersisted(graph.Edge{ID: "application-edge", Type: "linkskindmarker", Weight: 1}),
		Rule: mustPersistRuleForTest(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(valid) <= policyEnvelopeHeader {
		t.Fatalf("encoded record has no payload to corrupt: %d bytes", len(valid))
	}
	// Flip one byte inside the edge Type string. gob still decodes the record
	// and the mutated Type is still a non-empty string that passes every
	// downstream validation, so the SHA-256 checksum is the only guard that can
	// notice the record was altered. This isolates the checksum specifically.
	marker := bytes.Index(valid, []byte("linkskindmarker"))
	if marker < policyEnvelopeHeader {
		t.Fatalf("marker not found in payload at %d", marker)
	}
	valid[marker] ^= 0xFF
	corruptPolicyRow(t, dir, policyEdgeRow("application-edge"), valid)

	reopened, err := OpenDurablePolicyStore(dir)
	if err == nil {
		reopened.Close()
		t.Fatal("store opened over a checksum mismatch instead of failing closed")
	}
	if !shoal.IsErrorCode(err, shoal.ErrorInternal) {
		t.Fatalf("checksum mismatch error = %v, want internal", err)
	}
}
