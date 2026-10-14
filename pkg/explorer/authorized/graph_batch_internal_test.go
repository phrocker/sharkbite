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
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// recordingNodeStore captures the exact identifier slice a batch lookup was
// asked for and returns a caller-supplied result, so deduplication and the
// handling of partial or over-broad results can be asserted directly.
type recordingNodeStore struct {
	PolicyStore
	requests         [][]shoal.ID
	result           map[shoal.ID]NodeRegistration
	err              error
	edgeRequests     [][]shoal.ID
	edgeResult       map[shoal.ID]EdgeRegistration
	edgeErr          error
	revisionRequests [][]shoal.ID
	revisionResult   map[shoal.ID]RevisionRegistration
	revisionErr      error
	nodeCalls        int
	edgeCalls        int
}

// Node and Edge are overridden purely so a point lookup can be counted rather
// than reaching the nil embedded store, which lets a test assert that a path
// issued no unbatched lookups at all.
func (s *recordingNodeStore) Node(
	_ context.Context,
	nodeID shoal.ID,
) (NodeRegistration, bool, error) {
	s.nodeCalls++
	registration, ok := s.result[nodeID]
	return registration, ok, s.err
}

func (s *recordingNodeStore) Edge(
	_ context.Context,
	edgeID shoal.ID,
) (EdgeRegistration, bool, error) {
	s.edgeCalls++
	registration, ok := s.edgeResult[edgeID]
	return registration, ok, s.edgeErr
}

func (s *recordingNodeStore) Nodes(
	_ context.Context,
	nodeIDs []shoal.ID,
) (map[shoal.ID]NodeRegistration, error) {
	s.requests = append(s.requests, append([]shoal.ID(nil), nodeIDs...))
	if s.err != nil {
		return nil, s.err
	}
	return s.result, nil
}

func (s *recordingNodeStore) Edges(
	_ context.Context,
	edgeIDs []shoal.ID,
) (map[shoal.ID]EdgeRegistration, error) {
	s.edgeRequests = append(s.edgeRequests, append([]shoal.ID(nil), edgeIDs...))
	if s.edgeErr != nil {
		return nil, s.edgeErr
	}
	return s.edgeResult, nil
}

func (s *recordingNodeStore) CurrentRevisions(
	_ context.Context,
	documentIDs []shoal.ID,
) (map[shoal.ID]RevisionRegistration, error) {
	s.revisionRequests = append(
		s.revisionRequests, append([]shoal.ID(nil), documentIDs...))
	if s.revisionErr != nil {
		return nil, s.revisionErr
	}
	return s.revisionResult, nil
}

func batchTestRule(t *testing.T, source, policy string) AccessRule {
	t.Helper()
	authPolicy, err := auth.NewPolicy(auth.PolicyConfig{
		AuthorizationDomain: []byte("domain"),
		SourceID:            []byte(source),
		GrantPolicyID:       []byte(policy),
		Epoch:               1,
	})
	if err != nil {
		t.Fatal(err)
	}
	rule, err := NewAccessRule(authPolicy)
	if err != nil {
		t.Fatal(err)
	}
	return rule
}

func batchTestDecision(t *testing.T, now time.Time) auth.Decision {
	t.Helper()
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
	return decision
}

// TestEdgeAllowsResolvedDeniesEndpointMissingFromPage is the core fail-closed
// assertion for batching: an endpoint that the batch result did not report is
// denied exactly as a point lookup reporting !ok denies it, whichever endpoint
// is missing and even when the edge rule itself allows.
func TestEdgeAllowsResolvedDeniesEndpointMissingFromPage(t *testing.T) {
	now := time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)
	rule := batchTestRule(t, "source", "policy")
	decision := batchTestDecision(t, now)
	registration := EdgeRegistration{
		Edge: graph.Edge{
			ID: "edge", From: "node-from", To: "node-to", Type: "link", Weight: 1,
		},
		Rule: rule,
	}
	from := NodeRegistration{Rule: rule}
	to := NodeRegistration{Rule: rule}
	for name, resolved := range map[string]registeredNodes{
		"empty page":     {},
		"nil page":       nil,
		"missing source": {"node-to": to},
		"missing target": {"node-from": from},
	} {
		allowed, err := edgeAllowsResolved(
			resolved, registration, decision, auth.OperationNeighborhood, now)
		if err != nil {
			t.Fatalf("%s error = %v", name, err)
		}
		if allowed {
			t.Fatalf("%s authorized an edge whose endpoint was never resolved", name)
		}
	}
	allowed, err := edgeAllowsResolved(
		registeredNodes{"node-from": from, "node-to": to},
		registration, decision, auth.OperationNeighborhood, now)
	if err != nil || !allowed {
		t.Fatalf("fully resolved edge allowed = %v err = %v", allowed, err)
	}
}

// TestEdgeAllowsResolvedDeniesUnauthorizedEndpointRule confirms batching did not
// drop the endpoint rule evaluation itself.
func TestEdgeAllowsResolvedDeniesUnauthorizedEndpointRule(t *testing.T) {
	now := time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)
	allowedRule := batchTestRule(t, "source", "policy")
	deniedRule := batchTestRule(t, "other-source", "other-policy")
	decision := batchTestDecision(t, now)
	registration := EdgeRegistration{
		Edge: graph.Edge{
			ID: "edge", From: "node-from", To: "node-to", Type: "link", Weight: 1,
		},
		Rule: allowedRule,
	}
	for name, resolved := range map[string]registeredNodes{
		"denied source": {
			"node-from": {Rule: deniedRule},
			"node-to":   {Rule: allowedRule},
		},
		"denied target": {
			"node-from": {Rule: allowedRule},
			"node-to":   {Rule: deniedRule},
		},
	} {
		allowed, err := edgeAllowsResolved(
			resolved, registration, decision, auth.OperationNeighborhood, now)
		if err != nil {
			t.Fatalf("%s error = %v", name, err)
		}
		if allowed {
			t.Fatalf("%s authorized an edge across an unauthorized endpoint", name)
		}
	}
}

// TestResolveNodesDeduplicatesAndConfinesResults asserts the batch carries one
// entry per distinct identifier and that registrations for identifiers that
// were never requested are discarded rather than admitted.
func TestResolveNodesDeduplicatesAndConfinesResults(t *testing.T) {
	rule := batchTestRule(t, "source", "policy")
	store := &recordingNodeStore{result: map[shoal.ID]NodeRegistration{
		"node-a":          {Rule: rule},
		"node-b":          {Rule: rule},
		"never-asked-for": {Rule: rule},
	}}
	client := &Client{policyStore: store}
	resolved, err := client.resolveNodes(context.Background(), []shoal.ID{
		"node-a", "node-b", "node-a", "node-c", "node-b", "node-a",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.requests) != 1 {
		t.Fatalf("batch lookups = %d, want exactly one", len(store.requests))
	}
	wantRequest := []shoal.ID{"node-a", "node-b", "node-c"}
	if !reflect.DeepEqual(store.requests[0], wantRequest) {
		t.Fatalf("batched identifiers = %#v, want %#v", store.requests[0], wantRequest)
	}
	if _, ok := resolved["never-asked-for"]; ok {
		t.Fatal("batch result admitted a registration that was never requested")
	}
	if _, ok := resolved["node-c"]; ok {
		t.Fatal("identifier absent from the batch result was reported as resolved")
	}
	if len(resolved) != 2 {
		t.Fatalf("resolved = %#v, want node-a and node-b only", resolved)
	}
}

func TestResolveCurrentRevisionsDeduplicatesAndConfinesResults(t *testing.T) {
	store := &recordingNodeStore{
		revisionResult: map[shoal.ID]RevisionRegistration{
			"document-a": {DocumentID: "document-a", RevisionID: "revision-a"},
			"document-b": {DocumentID: "document-b", RevisionID: "revision-b"},
			"extra":      {DocumentID: "extra", RevisionID: "revision-extra"},
		},
	}
	client := &Client{policyStore: store}
	resolved, err := client.resolveCurrentRevisions(
		context.Background(),
		[]shoal.ID{"document-a", "document-b", "document-a", "missing"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.revisionRequests) != 1 {
		t.Fatalf("batch lookups = %d, want exactly one", len(store.revisionRequests))
	}
	wantRequest := []shoal.ID{"document-a", "document-b", "missing"}
	if !reflect.DeepEqual(store.revisionRequests[0], wantRequest) {
		t.Fatalf("batched identifiers = %#v, want %#v",
			store.revisionRequests[0], wantRequest)
	}
	if _, ok := resolved["extra"]; ok {
		t.Fatal("batch result admitted an unrequested revision")
	}
	if _, ok := resolved["missing"]; ok {
		t.Fatal("omitted document was reported as resolved")
	}
	if len(resolved) != 2 {
		t.Fatalf("resolved = %#v, want document-a and document-b only", resolved)
	}
}

func TestResolveCurrentRevisionsSkipsEmptyBatch(t *testing.T) {
	store := &recordingNodeStore{}
	client := &Client{policyStore: store}
	resolved, err := client.resolveCurrentRevisions(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.revisionRequests) != 0 {
		t.Fatalf("batch lookups = %d, want none", len(store.revisionRequests))
	}
	if len(resolved) != 0 {
		t.Fatalf("resolved = %#v, want empty", resolved)
	}
}

func TestResolveCurrentRevisionsPropagatesStoreFailureAsRead(t *testing.T) {
	store := &recordingNodeStore{revisionErr: catalogUnavailable()}
	client := &Client{policyStore: store}
	if _, err := client.resolveCurrentRevisions(
		context.Background(), []shoal.ID{"document-a"},
	); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("batch failure error = %v", err)
	}
}

// TestResolveNodesSkipsEmptyBatch keeps a page with nothing to resolve from
// spending a round trip.
func TestResolveNodesSkipsEmptyBatch(t *testing.T) {
	store := &recordingNodeStore{}
	client := &Client{policyStore: store}
	resolved, err := client.resolveNodes(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.requests) != 0 {
		t.Fatalf("batch lookups = %d, want none", len(store.requests))
	}
	if len(resolved) != 0 {
		t.Fatalf("resolved = %#v, want empty", resolved)
	}
}

// TestResolveNodesPropagatesStoreFailureAsRead confirms a failed batch surfaces
// as a catalog read failure instead of silently resolving to an empty page,
// which would otherwise read as a blanket deny with no error.
func TestResolveNodesPropagatesStoreFailureAsRead(t *testing.T) {
	store := &recordingNodeStore{err: catalogUnavailable()}
	client := &Client{policyStore: store}
	if _, err := client.resolveNodes(
		context.Background(), []shoal.ID{"node-a"}, nil,
	); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("batch failure error = %v", err)
	}
}

// TestMemoryPolicyStoreNodesMatchesNodeLoop pins the batch implementation to be
// exactly equivalent to calling Node for each identifier.
func TestMemoryPolicyStoreNodesMatchesNodeLoop(t *testing.T) {
	ctx := context.Background()
	rule := batchTestRule(t, "source", "policy")
	store := NewMemoryPolicyStore()
	if err := store.PutRevision(ctx, RevisionRegistration{
		DocumentID: "document",
		RevisionID: "revision",
		NodeIDs:    []shoal.ID{"document", "node-a"},
		ContentDigest: auth.DigestBytes(
			"test-content", []byte("revision")),
		Rule:    rule,
		Current: true,
	}); err != nil {
		t.Fatal(err)
	}

	requested := []shoal.ID{"document", "node-a", "node-a", "unregistered"}
	batch, err := store.Nodes(ctx, requested)
	if err != nil {
		t.Fatal(err)
	}
	expected := make(map[shoal.ID]NodeRegistration)
	for _, nodeID := range requested {
		registration, ok, err := store.Node(ctx, nodeID)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			if _, present := batch[nodeID]; present {
				t.Fatalf("batch reported unregistered node %q", nodeID)
			}
			continue
		}
		expected[nodeID] = registration
	}
	if !reflect.DeepEqual(batch, expected) {
		t.Fatalf("batch = %#v, want %#v", batch, expected)
	}
	if len(batch) != 2 {
		t.Fatalf("batch = %#v, want one entry per distinct registered node", batch)
	}
}

func TestMemoryPolicyStoreCurrentRevisionsMatchesPointLoop(t *testing.T) {
	ctx := context.Background()
	rule := batchTestRule(t, "source", "policy")
	store := NewMemoryPolicyStore()
	opaqueDocumentID := shoal.ID(string([]byte{'d', 0, 0xff}))
	opaqueRevisionID := shoal.ID(string([]byte{'r', 0, 0xfe}))
	for _, registration := range []RevisionRegistration{
		{
			DocumentID: "document-a", RevisionID: "revision-a",
			NodeIDs: []shoal.ID{"document-a"},
			ContentDigest: auth.DigestBytes(
				"test-content", []byte("revision-a")),
			Rule: rule, Current: true,
		},
		{
			DocumentID: "document-b", RevisionID: "revision-b",
			NodeIDs: []shoal.ID{"document-b"},
			ContentDigest: auth.DigestBytes(
				"test-content", []byte("revision-b")),
			Rule: rule, Current: true,
		},
		{
			DocumentID: opaqueDocumentID, RevisionID: opaqueRevisionID,
			NodeIDs: []shoal.ID{opaqueDocumentID},
			ContentDigest: auth.DigestBytes(
				"test-content", []byte("opaque-revision")),
			Rule: rule, Current: true,
		},
	} {
		if err := store.PutRevision(ctx, registration); err != nil {
			t.Fatal(err)
		}
	}
	requested := []shoal.ID{
		"document-a", opaqueDocumentID, "document-b", "document-a",
		opaqueDocumentID, "unregistered",
	}
	batch, err := store.CurrentRevisions(ctx, requested)
	if err != nil {
		t.Fatal(err)
	}
	expected := make(map[shoal.ID]RevisionRegistration)
	for _, documentID := range requested {
		registration, ok, err := store.CurrentRevision(ctx, documentID)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			if _, present := batch[documentID]; present {
				t.Fatalf("batch reported unregistered document %q", documentID)
			}
			continue
		}
		expected[documentID] = registration
	}
	if !reflect.DeepEqual(batch, expected) {
		t.Fatalf("batch = %#v, want %#v", batch, expected)
	}
	if len(batch) != 3 {
		t.Fatalf("batch = %#v, want one entry per distinct registered document", batch)
	}
	if batch[opaqueDocumentID].RevisionID != opaqueRevisionID {
		t.Fatalf("opaque revision identifier changed: got %q, want %q",
			batch[opaqueDocumentID].RevisionID, opaqueRevisionID)
	}
}

// TestMemoryPolicyStoreNodesRejectsInvalidInput mirrors Node's argument and
// context validation so batching cannot become a laxer entry point.
func TestMemoryPolicyStoreNodesRejectsInvalidInput(t *testing.T) {
	store := NewMemoryPolicyStore()
	if _, err := store.Nodes(
		context.Background(), []shoal.ID{"node-a", ""},
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("empty identifier error = %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Nodes(cancelled, []shoal.ID{"node-a"}); err == nil {
		t.Fatal("cancelled context batch lookup succeeded")
	}
	var nilStore *MemoryPolicyStore
	if _, err := nilStore.Nodes(
		context.Background(), []shoal.ID{"node-a"},
	); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("nil store batch lookup error = %v", err)
	}
}

// TestMemoryPolicyStoreBatchFailureOrderMatchesPointLoop pins the failure
// ordering the batch doc comments promise. A Node loop over {"node-a", ""}
// against a nil store fails on the first identifier with catalog-unavailable,
// so the batch must report the same rather than validating the whole slice
// first and surfacing the later invalid identifier instead.
func TestMemoryPolicyStoreBatchFailureOrderMatchesPointLoop(t *testing.T) {
	ctx := context.Background()
	var nilStore *MemoryPolicyStore
	if _, err := nilStore.Nodes(
		ctx, []shoal.ID{"node-a", ""},
	); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("nil store node batch error = %v, want catalog unavailable", err)
	}
	if _, err := nilStore.Edges(
		ctx, []shoal.ID{"edge-a", ""},
	); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("nil store edge batch error = %v, want catalog unavailable", err)
	}
	if _, err := nilStore.CurrentRevisions(
		ctx, []shoal.ID{"document-a", ""},
	); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("nil store revision batch error = %v, want catalog unavailable", err)
	}
	// An empty request makes no per-identifier call, so the receiver check is
	// all that remains and must still reject.
	if _, err := nilStore.Nodes(ctx, nil); !shoal.IsErrorCode(
		err, shoal.ErrorUnavailable,
	) {
		t.Fatalf("nil store empty node batch error = %v", err)
	}
	if _, err := nilStore.Edges(ctx, nil); !shoal.IsErrorCode(
		err, shoal.ErrorUnavailable,
	) {
		t.Fatalf("nil store empty edge batch error = %v", err)
	}
	if _, err := nilStore.CurrentRevisions(ctx, nil); !shoal.IsErrorCode(
		err, shoal.ErrorUnavailable,
	) {
		t.Fatalf("nil store empty revision batch error = %v", err)
	}
	// A live store still reports an invalid identifier, so moving the receiver
	// check does not weaken argument validation.
	store := NewMemoryPolicyStore()
	if _, err := store.Nodes(
		ctx, []shoal.ID{"node-a", ""},
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("invalid node identifier error = %v", err)
	}
	if _, err := store.Edges(
		ctx, []shoal.ID{"edge-a", ""},
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("invalid edge identifier error = %v", err)
	}
	if _, err := store.CurrentRevisions(
		ctx, []shoal.ID{"document-a", ""},
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("invalid document identifier error = %v", err)
	}
}

// TestResolveEdgesDeduplicatesAndConfinesResults is the edge counterpart of the
// node batch guard: repeated identifiers cost one round trip, and a store that
// answers with identifiers the page never asked for cannot widen the page.
func TestResolveEdgesDeduplicatesAndConfinesResults(t *testing.T) {
	rule := batchTestRule(t, "source", "policy")
	store := &recordingNodeStore{edgeResult: map[shoal.ID]EdgeRegistration{
		"edge-a":          {Rule: rule},
		"edge-b":          {Rule: rule},
		"never-asked-for": {Rule: rule},
	}}
	client := &Client{policyStore: store}
	resolved, err := client.resolveEdges(context.Background(), []shoal.ID{
		"edge-a", "edge-b", "edge-a", "edge-c", "edge-b", "edge-a",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.edgeRequests) != 1 {
		t.Fatalf("batch lookups = %d, want exactly one", len(store.edgeRequests))
	}
	wantRequest := []shoal.ID{"edge-a", "edge-b", "edge-c"}
	if !reflect.DeepEqual(store.edgeRequests[0], wantRequest) {
		t.Fatalf("batched identifiers = %#v, want %#v", store.edgeRequests[0], wantRequest)
	}
	if _, ok := resolved["never-asked-for"]; ok {
		t.Fatal("batch result admitted a registration that was never requested")
	}
	if _, ok := resolved["edge-c"]; ok {
		t.Fatal("identifier absent from the batch result was reported as resolved")
	}
	if len(resolved) != 2 {
		t.Fatalf("resolved = %#v, want edge-a and edge-b only", resolved)
	}
}

// TestResolveEdgesSkipsEmptyBatch keeps a page with no candidate edges from
// spending a round trip.
func TestResolveEdgesSkipsEmptyBatch(t *testing.T) {
	store := &recordingNodeStore{}
	client := &Client{policyStore: store}
	resolved, err := client.resolveEdges(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.edgeRequests) != 0 {
		t.Fatalf("batch lookups = %d, want none", len(store.edgeRequests))
	}
	if len(resolved) != 0 {
		t.Fatalf("resolved = %#v, want empty", resolved)
	}
}

// TestResolveEdgesPropagatesStoreFailureAsRead confirms a failed edge batch
// surfaces as a catalog read failure rather than an empty result that would be
// indistinguishable from every edge being unregistered.
func TestResolveEdgesPropagatesStoreFailureAsRead(t *testing.T) {
	store := &recordingNodeStore{edgeErr: catalogUnavailable()}
	client := &Client{policyStore: store}
	if _, err := client.resolveEdges(
		context.Background(), []shoal.ID{"edge-a"}, nil,
	); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("batch failure error = %v", err)
	}
}

// TestMemoryPolicyStoreEdgesMatchesEdgeLoop pins the batch edge implementation
// to be equivalent to calling Edge for each identifier, including omitting
// unregistered identifiers rather than reporting a zero registration.
func TestMemoryPolicyStoreEdgesMatchesEdgeLoop(t *testing.T) {
	ctx := context.Background()
	rule := batchTestRule(t, "source", "policy")
	store := NewMemoryPolicyStore()
	if err := store.PutRevision(ctx, RevisionRegistration{
		DocumentID:    "document",
		RevisionID:    "revision",
		NodeIDs:       []shoal.ID{"document", "node-a"},
		ContentDigest: auth.DigestBytes("test-content", []byte("revision")),
		Rule:          rule,
		Current:       true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutEdge(ctx, EdgeRegistration{
		Edge: graph.Edge{
			ID: "edge-a", From: "document", To: "node-a", Type: "link",
		},
		DocumentID: "document", RevisionID: "revision", Rule: rule,
	}); err != nil {
		t.Fatal(err)
	}
	requested := []shoal.ID{"edge-a", "edge-a", "unregistered"}
	batch, err := store.Edges(ctx, requested)
	if err != nil {
		t.Fatal(err)
	}
	expected := make(map[shoal.ID]EdgeRegistration)
	for _, edgeID := range requested {
		registration, ok, err := store.Edge(ctx, edgeID)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			if _, present := batch[edgeID]; present {
				t.Fatalf("batch reported unregistered edge %q", edgeID)
			}
			continue
		}
		expected[edgeID] = registration
	}
	if !reflect.DeepEqual(batch, expected) {
		t.Fatalf("batch = %#v, want %#v", batch, expected)
	}
	if len(batch) != 1 {
		t.Fatalf("batch = %#v, want one entry per distinct registered edge", batch)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := store.Edges(cancelled, []shoal.ID{"edge-a"}); err == nil {
		t.Fatal("cancelled context batch edge lookup succeeded")
	}
}

// countdownContext reports success for a fixed number of Err checks and then
// reports cancellation, so a context that becomes cancelled partway through a
// batch can be exercised deterministically.
type countdownContext struct {
	context.Context
	remaining *int
}

func (c countdownContext) Err() error {
	if *c.remaining > 0 {
		*c.remaining--
		return nil
	}
	return context.Canceled
}

// corruptRule is a rule that cannot be cloned, because cloning rebuilds the
// rule from its policies and an empty conjunction is rejected. Storing one
// makes a registration that Node and Edge report as catalog-unavailable, which
// is the failure a batch must surface in request order rather than after
// validating every identifier first.
func corruptRule() AccessRule { return AccessRule{} }

// TestMemoryPolicyStoreBatchFailsOnTheFirstFailingIdentifier pins the ordering
// the batch doc comments promise. A Node loop over a corrupt registration
// followed by a malformed identifier fails on the first with
// catalog-unavailable, so validating the whole slice up front and surfacing the
// later invalid identifier instead would break the documented equivalence.
func TestMemoryPolicyStoreBatchFailsOnTheFirstFailingIdentifier(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryPolicyStore()
	store.initialize()
	store.nodes["corrupt-node"] = NodeRegistration{Rule: corruptRule()}
	store.edges["corrupt-edge"] = EdgeRegistration{
		Edge: graph.Edge{ID: "corrupt-edge", From: "a", To: "b", Type: "link"},
		Rule: corruptRule(),
	}
	store.current["corrupt-document"] = revisionKey{
		documentID: "corrupt-document",
		revisionID: "missing-revision",
	}

	// The point lookups establish the behaviour the batch must reproduce.
	if _, _, err := store.Node(ctx, "corrupt-node"); !shoal.IsErrorCode(
		err, shoal.ErrorUnavailable,
	) {
		t.Fatalf("corrupt node point lookup error = %v", err)
	}
	if _, _, err := store.Edge(ctx, "corrupt-edge"); !shoal.IsErrorCode(
		err, shoal.ErrorUnavailable,
	) {
		t.Fatalf("corrupt edge point lookup error = %v", err)
	}
	if _, _, err := store.CurrentRevision(
		ctx, "corrupt-document",
	); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("corrupt revision point lookup error = %v", err)
	}

	if _, err := store.Nodes(
		ctx, []shoal.ID{"corrupt-node", ""},
	); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("node batch error = %v, want the earlier corrupt registration", err)
	}
	if _, err := store.Edges(
		ctx, []shoal.ID{"corrupt-edge", ""},
	); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("edge batch error = %v, want the earlier corrupt registration", err)
	}
	if _, err := store.CurrentRevisions(
		ctx, []shoal.ID{"corrupt-document", ""},
	); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf(
			"revision batch error = %v, want the earlier corrupt registration", err)
	}
	// Reversing the order reverses the reported failure, which is what makes
	// this a request-order guarantee rather than a fixed precedence.
	if _, err := store.Nodes(
		ctx, []shoal.ID{"", "corrupt-node"},
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("node batch error = %v, want the earlier invalid identifier", err)
	}
	if _, err := store.Edges(
		ctx, []shoal.ID{"", "corrupt-edge"},
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("edge batch error = %v, want the earlier invalid identifier", err)
	}
	if _, err := store.CurrentRevisions(
		ctx, []shoal.ID{"", "corrupt-document"},
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf(
			"revision batch error = %v, want the earlier invalid identifier", err)
	}
}

// TestMemoryPolicyStoreBatchSpendsContextChecksLikeAPointLoop compares a batch
// against the equivalent point loop under a context that tolerates a fixed
// number of Err checks. Asserting the two agree for every budget pins both
// halves of the contract at once: the context must be rechecked for every
// identifier rather than once before the batch, and each identifier must be
// charged exactly one check rather than the first paying twice for a precheck.
func TestMemoryPolicyStoreBatchSpendsContextChecksLikeAPointLoop(t *testing.T) {
	nodeIDs := []shoal.ID{"node-a", "node-b", "node-c"}
	edgeIDs := []shoal.ID{"edge-a", "edge-b", "edge-c"}
	documentIDs := []shoal.ID{"document-a", "document-b", "document-c"}
	for budget := 0; budget <= len(nodeIDs)+1; budget++ {
		for size := 1; size <= len(nodeIDs); size++ {
			store := NewMemoryPolicyStore()

			remaining := budget
			loopCtx := countdownContext{
				Context: context.Background(), remaining: &remaining,
			}
			var wantNodeErr error
			for _, nodeID := range nodeIDs[:size] {
				if _, _, err := store.Node(loopCtx, nodeID); err != nil {
					wantNodeErr = err
					break
				}
			}

			remaining = budget
			_, gotNodeErr := store.Nodes(loopCtx, nodeIDs[:size])
			if !sameErrorCode(gotNodeErr, wantNodeErr) {
				t.Fatalf(
					"budget %d size %d: Nodes error = %v, Node loop error = %v",
					budget, size, gotNodeErr, wantNodeErr)
			}

			remaining = budget
			var wantEdgeErr error
			for _, edgeID := range edgeIDs[:size] {
				if _, _, err := store.Edge(loopCtx, edgeID); err != nil {
					wantEdgeErr = err
					break
				}
			}

			remaining = budget
			_, gotEdgeErr := store.Edges(loopCtx, edgeIDs[:size])
			if !sameErrorCode(gotEdgeErr, wantEdgeErr) {
				t.Fatalf(
					"budget %d size %d: Edges error = %v, Edge loop error = %v",
					budget, size, gotEdgeErr, wantEdgeErr)
			}

			remaining = budget
			var wantRevisionErr error
			for _, documentID := range documentIDs[:size] {
				if _, _, err := store.CurrentRevision(
					loopCtx, documentID,
				); err != nil {
					wantRevisionErr = err
					break
				}
			}

			remaining = budget
			_, gotRevisionErr := store.CurrentRevisions(
				loopCtx, documentIDs[:size])
			if !sameErrorCode(gotRevisionErr, wantRevisionErr) {
				t.Fatalf(
					"budget %d size %d: CurrentRevisions error = %v, "+
						"CurrentRevision loop error = %v",
					budget, size, gotRevisionErr, wantRevisionErr)
			}
		}
	}
}

// TestMemoryPolicyStoreEmptyBatchStillChecksContext covers the one case the
// per-identifier check cannot reach. With nothing to iterate, a cancelled
// context must still be reported rather than answered with an empty result,
// which would read as "nothing is registered" and silently deny.
func TestMemoryPolicyStoreEmptyBatchStillChecksContext(t *testing.T) {
	store := NewMemoryPolicyStore()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Nodes(cancelled, nil); !shoal.IsErrorCode(
		err, shoal.ErrorCanceled,
	) {
		t.Fatalf("empty node batch error = %v, want cancellation", err)
	}
	if _, err := store.Edges(cancelled, nil); !shoal.IsErrorCode(
		err, shoal.ErrorCanceled,
	) {
		t.Fatalf("empty edge batch error = %v, want cancellation", err)
	}
	if _, err := store.CurrentRevisions(cancelled, nil); !shoal.IsErrorCode(
		err, shoal.ErrorCanceled,
	) {
		t.Fatalf("empty revision batch error = %v, want cancellation", err)
	}
	// A live empty request under a healthy context resolves to an empty result
	// rather than an error.
	resolvedNodes, err := store.Nodes(context.Background(), nil)
	if err != nil || len(resolvedNodes) != 0 {
		t.Fatalf("empty node batch = %#v err = %v", resolvedNodes, err)
	}
	resolvedEdges, err := store.Edges(context.Background(), nil)
	if err != nil || len(resolvedEdges) != 0 {
		t.Fatalf("empty edge batch = %#v err = %v", resolvedEdges, err)
	}
	resolvedRevisions, err := store.CurrentRevisions(context.Background(), nil)
	if err != nil || len(resolvedRevisions) != 0 {
		t.Fatalf("empty revision batch = %#v err = %v", resolvedRevisions, err)
	}
	// A nil receiver reports catalog-unavailable even with nothing to look up.
	var nilStore *MemoryPolicyStore
	if _, err := nilStore.Nodes(context.Background(), nil); !shoal.IsErrorCode(
		err, shoal.ErrorUnavailable,
	) {
		t.Fatalf("nil store empty node batch error = %v", err)
	}
	if _, err := nilStore.Edges(context.Background(), nil); !shoal.IsErrorCode(
		err, shoal.ErrorUnavailable,
	) {
		t.Fatalf("nil store empty edge batch error = %v", err)
	}
	if _, err := nilStore.CurrentRevisions(
		context.Background(), nil,
	); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("nil store empty revision batch error = %v", err)
	}
}

// sameErrorCode compares two results by shoal error code, treating success as
// its own outcome, which is the granularity these contracts are stated at.
func sameErrorCode(left, right error) bool {
	if (left == nil) != (right == nil) {
		return false
	}
	if left == nil {
		return true
	}
	return shoalErrorCode(left) == shoalErrorCode(right)
}

func shoalErrorCode(err error) shoal.ErrorCode {
	var typed *shoal.Error
	if errors.As(err, &typed) && typed != nil {
		return typed.Code
	}
	return ""
}

// TestEdgeAllowsBatchesEndpointsInOneRequest guards the edgeAllows path that
// retrieval validation still uses. The neighborhood round-trip tests only cover
// edgeAllowsResolved, so without this a revert of edgeAllows to two Node point
// lookups would break no test. Both endpoints must be resolved in a single
// deduplicated batch and no point lookup may be issued.
func TestEdgeAllowsBatchesEndpointsInOneRequest(t *testing.T) {
	now := time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)
	rule := batchTestRule(t, "source", "policy")
	decision := batchTestDecision(t, now)
	for name, testCase := range map[string]struct {
		from, to    shoal.ID
		wantRequest []shoal.ID
	}{
		"distinct endpoints": {
			from: "node-from", to: "node-to",
			wantRequest: []shoal.ID{"node-from", "node-to"},
		},
		"self loop": {
			from: "node-self", to: "node-self",
			wantRequest: []shoal.ID{"node-self"},
		},
	} {
		store := &recordingNodeStore{result: map[shoal.ID]NodeRegistration{
			testCase.from: {Rule: rule},
			testCase.to:   {Rule: rule},
		}}
		client := &Client{policyStore: store}
		registration := EdgeRegistration{
			Edge: graph.Edge{
				ID: "edge", From: testCase.from, To: testCase.to,
				Type: "link", Weight: 1,
			},
			Rule: rule,
		}
		allowed, err := client.edgeAllows(
			context.Background(), registration, decision,
			auth.OperationNeighborhood, now)
		if err != nil {
			t.Fatalf("%s error = %v", name, err)
		}
		if !allowed {
			t.Fatalf("%s denied a fully registered edge", name)
		}
		if store.nodeCalls != 0 {
			t.Fatalf("%s issued %d unbatched Node lookups, want none",
				name, store.nodeCalls)
		}
		if len(store.requests) != 1 {
			t.Fatalf("%s issued %d batch lookups, want exactly one",
				name, len(store.requests))
		}
		if !reflect.DeepEqual(store.requests[0], testCase.wantRequest) {
			t.Fatalf("%s batched %#v, want %#v",
				name, store.requests[0], testCase.wantRequest)
		}
	}
}

// TestEdgeAllowsDeniesEndpointMissingFromBatch keeps the retained edgeAllows
// path fail-closed: an endpoint the batch omits denies, and the denial is not
// recovered by falling back to a point lookup.
func TestEdgeAllowsDeniesEndpointMissingFromBatch(t *testing.T) {
	now := time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)
	rule := batchTestRule(t, "source", "policy")
	decision := batchTestDecision(t, now)
	store := &recordingNodeStore{result: map[shoal.ID]NodeRegistration{
		"node-from": {Rule: rule},
	}}
	client := &Client{policyStore: store}
	allowed, err := client.edgeAllows(
		context.Background(),
		EdgeRegistration{
			Edge: graph.Edge{
				ID: "edge", From: "node-from", To: "node-to", Type: "link",
			},
			Rule: rule,
		},
		decision, auth.OperationNeighborhood, now)
	if err != nil {
		t.Fatal(err)
	}
	if allowed {
		t.Fatal("edgeAllows authorized an edge whose endpoint the batch omitted")
	}
	if store.nodeCalls != 0 {
		t.Fatalf("edgeAllows fell back to %d point lookups", store.nodeCalls)
	}
}
