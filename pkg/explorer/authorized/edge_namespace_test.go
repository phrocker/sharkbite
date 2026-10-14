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

// Connect's result must not depend on edges the caller cannot read (#699).
// Before the fix an ID collision with a hidden edge was Conflict and a fresh
// ID was success, so a reader with one readable node could probe for hidden
// application edge IDs and, through the intrinsic "contains" edge IDs that
// are derived from a document's URI, for hidden documents by URI.

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const namespaceProbeEdgeType = "namespace_probe"

// intrinsicContainsEdgeID is what a prober computes from a URI alone: the ID
// of the "contains" edge from the document at uri to its root section
// (pkg/explorer/parse.go).
func intrinsicContainsEdgeID(uri string) shoal.ID {
	stable := func(kind string, parts ...string) string {
		hash := sha256.New()
		var length [8]byte
		write := func(value string) {
			binary.BigEndian.PutUint64(length[:], uint64(len(value)))
			_, _ = hash.Write(length[:])
			_, _ = hash.Write([]byte(value))
		}
		write(kind)
		for _, part := range parts {
			write(part)
		}
		return kind + "_" + hex.EncodeToString(hash.Sum(nil)[:16])
	}
	document := string(explorer.SourceDocumentID(uri))
	root := stable("section", document, "root")
	return shoal.ID(stable("edge", document, "contains", root))
}

// probe is the reader's Connect of a self-edge on the one node it can read.
func (w *bindingWorld) probe(id shoal.ID) error {
	return w.client.Connect(w.as(w.reader), graph.Edge{
		ID: id, From: w.open.Document.ID, To: w.open.Document.ID,
		Type: namespaceProbeEdgeType, Weight: 1,
	})
}

// probeEdges are the probe edges the corpus holds, read as trusted.
func (w *bindingWorld) probeEdges() map[shoal.ID]bool {
	w.t.Helper()
	around, err := w.f.base.Neighborhood(context.Background(), explorer.NeighborhoodRequest{
		NodeIDs: []shoal.ID{w.open.Document.ID}, Depth: 1,
		EdgeTypes: []string{namespaceProbeEdgeType},
	})
	if err != nil {
		w.t.Fatal(err)
	}
	edges := map[shoal.ID]bool{}
	for _, edge := range around.Edges {
		edges[edge.ID] = true
	}
	return edges
}

func sameConnectResult(t *testing.T, name string, got, absent error) {
	t.Helper()
	if (got == nil) != (absent == nil) ||
		(got != nil && got.Error() != absent.Error()) {
		t.Fatalf("%s: Connect = %v, but for an absent ID it is %v", name, got, absent)
	}
}

// TestConnectProbesCannotTellHiddenEdgesFromAbsentOnes is the issue's
// reproduction, on the memory and durable catalogs.
func TestConnectProbesCannotTellHiddenEdgesFromAbsentOnes(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		w := newBindingWorld(t, newFixture(t), store)
		w.connect(w.client, bindingEdge, w.open.Document.ID, w.closed.Document.ID)
		reader := w.as(w.reader)
		// The probes are not vacuous: the labelled document and the
		// holder's edge exist, and the reader can see neither.
		if _, err := w.client.Document(reader, w.closed.Document.ID, ""); !shoal.IsErrorCode(
			err, shoal.ErrorNotFound) {
			t.Fatalf("reader Document(closed) = %v, want not_found", err)
		}
		if w.edgeVisible(w.client, w.reader, bindingEdge) {
			t.Fatal("the reader sees the holder's edge")
		}
		hiddenIntrinsic := intrinsicContainsEdgeID(bindingClosedURI)
		inside, err := w.f.base.Neighborhood(context.Background(), explorer.NeighborhoodRequest{
			NodeIDs: []shoal.ID{w.closed.Document.ID}, Depth: 1,
			EdgeTypes: []string{"contains"},
		})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, edge := range inside.Edges {
			found = found || edge.ID == hiddenIntrinsic
		}
		if !found {
			t.Fatalf("the URI-derived ID %s is not the hidden document's edge", hiddenIntrinsic)
		}

		// A hidden document by URI, and an absent URI.
		absentIntrinsic := intrinsicContainsEdgeID("file:///binding/never-ingested.txt")
		sameConnectResult(t, "hidden document by URI",
			w.probe(hiddenIntrinsic), w.probe(absentIntrinsic))
		// The holder's hidden edge by its exact ID, and an absent ID in
		// the holder's namespace.
		hiddenApplication := w.edgeID(w.client, bindingEdge)
		absentApplication := w.edgeID(w.client, "edge-never-connected")
		sameConnectResult(t, "hidden application edge",
			w.probe(hiddenApplication), w.probe(absentApplication))
		// Both are refused, the same way, whatever the ID: neither is in
		// the reader's namespace.
		if err := w.probe(absentIntrinsic); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
			t.Fatalf("probe outside the reader's namespace = %v, want invalid_argument", err)
		}
		if refused := w.probeEdges(); len(refused) != 0 {
			t.Fatalf("a refused probe created edges: %v", refused)
		}

		// In its own namespace the reader's ID cannot collide with the
		// holder's edge: the holder's local ID and a fresh one both
		// succeed, and the holder's edge is untouched.
		ownSameLocal := appEdgeID(t, w.client, reader, bindingEdge)
		ownFresh := appEdgeID(t, w.client, reader, "edge-never-connected")
		if ownSameLocal == hiddenApplication {
			t.Fatal("two principals share an application edge ID")
		}
		sameConnectResult(t, "own namespace", w.probe(ownSameLocal), w.probe(ownFresh))
		if err := w.probe(ownSameLocal); err != nil {
			t.Fatalf("own namespace probe = %v", err)
		}
		if created := w.probeEdges(); len(created) != 2 ||
			!created[ownSameLocal] || !created[ownFresh] {
			t.Fatalf("own-namespace probes created %v", created)
		}
		if !w.edgeVisible(w.client, w.holder, bindingEdge) {
			t.Fatal("the holder's edge is gone")
		}
	})
}

// TestConnectRetryBySameWriterIsIdempotent: a writer's retry of the same edge
// succeeds, a different edge under the same ID conflicts, and another writer
// using the same local ID gets its own edge, on both catalogs and across a
// reopen of the client.
func TestConnectRetryBySameWriterIsIdempotent(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		w := newBindingWorld(t, newFixture(t), store)
		holder := w.as(w.holder)
		edge := graph.Edge{
			ID:   w.edgeID(w.client, "retried-edge"),
			From: w.open.Document.ID, To: w.open.Document.ID,
			Type: namespaceProbeEdgeType, Weight: 1,
		}
		if err := w.client.Connect(holder, edge); err != nil {
			t.Fatal(err)
		}
		if err := w.client.Connect(holder, edge); err != nil {
			t.Fatalf("idempotent retry = %v", err)
		}
		reopened := w.newClient(store)
		if again := appEdgeID(t, reopened, w.as(w.holder), "retried-edge"); again != edge.ID {
			t.Fatalf("the namespace moved: %s, then %s", edge.ID, again)
		}
		if err := reopened.Connect(w.as(w.holder), edge); err != nil {
			t.Fatalf("retry through a new client = %v", err)
		}
		different := edge
		different.Weight = 2
		if err := w.client.Connect(holder, different); !shoal.IsErrorCode(
			err, shoal.ErrorConflict) {
			t.Fatalf("different edge under the writer's own ID = %v, want conflict", err)
		}
		readerEdge := edge
		readerEdge.ID = appEdgeID(t, w.client, w.as(w.reader), "retried-edge")
		if err := w.client.Connect(w.as(w.reader), readerEdge); err != nil {
			t.Fatalf("another writer's same local ID = %v", err)
		}
		if edges := w.probeEdges(); len(edges) != 2 ||
			!edges[edge.ID] || !edges[readerEdge.ID] {
			t.Fatalf("edges = %v", edges)
		}
	})
}

// TestConnectRetryAcrossRelabelStillConflicts keeps #695's behaviour: the
// writer's retry of an edge bound to a revision since relabelled conflicts
// (fail closed), and the reader still cannot see the edge.
func TestConnectRetryAcrossRelabelStillConflicts(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		w := newBindingWorld(t, newFixture(t), store)
		w.connect(w.client, bindingEdge, w.open.Document.ID, w.closed.Document.ID)
		w.relabel(w.client)
		err := w.client.Connect(w.as(w.holder), graph.Edge{
			ID:   w.edgeID(w.client, bindingEdge),
			From: w.open.Document.ID, To: w.closed.Document.ID,
			Type: "references", Weight: 1,
		})
		if !shoal.IsErrorCode(err, shoal.ErrorConflict) {
			t.Fatalf("retry across the relabel = %v, want conflict", err)
		}
		if w.edgeVisible(w.client, w.reader, bindingEdge) {
			t.Fatal("the reader sees the edge after the relabel")
		}
	})
}

// TestConnectRefusesIDsOutsideTheCallersNamespace: everything outside the
// namespace is the same InvalidArgument, before any endpoint is read; the
// bare namespace with no local ID is outside it.
func TestConnectRefusesIDsOutsideTheCallersNamespace(t *testing.T) {
	f := newFixture(t)
	admin := f.admin(t)
	namespaced := appEdgeID(t, f.clientA, admin, "x")
	bare := strings.TrimSuffix(string(namespaced), "x")
	for _, id := range []shoal.ID{
		"plain-edge", "edge_00000000000000000000000000000000",
		shoal.ID(bare), shoal.ID(strings.TrimSuffix(bare, "/")) + "x",
		shoal.ID("app-edge_00000000000000000000000000000000/x"),
	} {
		// The endpoints do not exist: the refusal comes first.
		err := f.clientA.Connect(admin, graph.Edge{
			ID: id, From: "missing", To: "missing", Type: "link", Weight: 1,
		})
		if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) ||
			!strings.Contains(err.Error(), "application edge namespace") {
			t.Fatalf("Connect(%q) = %v, want the namespace refusal", id, err)
		}
	}
	err := f.clientA.Connect(admin, graph.Edge{
		ID: namespaced, From: "missing", To: "missing", Type: "link", Weight: 1,
	})
	if !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("namespaced ID with missing endpoints = %v, want not_found", err)
	}
	if _, err := f.clientA.ApplicationEdgeID(admin, ""); !shoal.IsErrorCode(
		err, shoal.ErrorInvalidArgument) {
		t.Fatalf("empty local ID = %v", err)
	}
	long := shoal.ID(strings.Repeat("l", shoal.MaxIDBytes))
	if _, err := f.clientA.ApplicationEdgeID(admin, long); !shoal.IsErrorCode(
		err, shoal.ErrorInvalidArgument) {
		t.Fatalf("over-long local ID = %v", err)
	}
}

// TestApplicationEdgeNamespaceIsTheWritersIdentity: every identity component
// moves the namespace; the request, the grants and the generation do not.
func TestApplicationEdgeNamespaceIsTheWritersIdentity(t *testing.T) {
	f := newFixture(t)
	otherDomain := []byte("domain-other")
	f.reader.Set(otherDomain, 1)
	base := auth.DecisionConfig{
		Subject: "writer", Actor: "writer-app",
		AuthorizationDomain:   f.domain,
		AllowedOperations:     allOperations,
		PermittedSourceIDs:    [][]byte{f.sourceA},
		PermittedPolicyIDs:    [][]byte{f.policyA},
		PolicyGeneration:      1,
		AuthenticationExpires: f.clock.Now().Add(time.Hour),
		RequestID:             "request-one",
	}
	id := func(change func(*auth.DecisionConfig)) shoal.ID {
		t.Helper()
		config := base
		change(&config)
		decision, err := auth.NewDecision(config)
		if err != nil {
			t.Fatal(err)
		}
		return appEdgeID(t, f.clientA, f.context(t, decision), "edge")
	}
	original := id(func(*auth.DecisionConfig) {})
	same := map[string]func(*auth.DecisionConfig){
		"request":     func(c *auth.DecisionConfig) { c.RequestID = "request-two" },
		"correlation": func(c *auth.DecisionConfig) { c.CorrelationID = "correlation" },
		"grants": func(c *auth.DecisionConfig) {
			c.PermittedSourceIDs = [][]byte{f.sourceA, f.sourceB}
			c.PermittedPolicyIDs = [][]byte{f.policyA, f.policyB}
		},
		"operations": func(c *auth.DecisionConfig) {
			c.AllowedOperations = []auth.Operation{auth.OperationConnect}
		},
	}
	for name, change := range same {
		if got := id(change); got != original {
			t.Errorf("%s moved the namespace: %s, want %s", name, got, original)
		}
	}
	different := map[string]func(*auth.DecisionConfig){
		"subject":   func(c *auth.DecisionConfig) { c.Subject = "other" },
		"actor":     func(c *auth.DecisionConfig) { c.Actor = "other-app" },
		"client":    func(c *auth.DecisionConfig) { c.ClientID = "client" },
		"on behalf": func(c *auth.DecisionConfig) { c.OnBehalfOf = []shoal.ID{"principal"} },
		"on behalf of another": func(c *auth.DecisionConfig) {
			c.OnBehalfOf = []shoal.ID{"another-principal"}
		},
		"service": func(c *auth.DecisionConfig) {
			c.ServiceRole, c.ServiceCeilingIdentity = auth.ServiceRoleDataWrite, "ceiling"
			c.AllowedOperations = []auth.Operation{auth.OperationConnect}
		},
		// The same strings, on either side of the delegation chain's end.
		"service in the delegation chain": func(c *auth.DecisionConfig) {
			c.OnBehalfOf = []shoal.ID{"principal", "derivation", "ceiling"}
		},
		"delegation then service": func(c *auth.DecisionConfig) {
			c.OnBehalfOf = []shoal.ID{"principal"}
			c.ServiceRole, c.ServiceCeilingIdentity = auth.ServiceRoleDerivation, "ceiling"
			c.AllowedOperations = []auth.Operation{auth.OperationConnect}
		},
		"service role": func(c *auth.DecisionConfig) {
			c.ServiceRole, c.ServiceCeilingIdentity = auth.ServiceRoleDerivation, "ceiling"
			c.AllowedOperations = []auth.Operation{auth.OperationConnect}
		},
		"service ceiling": func(c *auth.DecisionConfig) {
			c.ServiceRole, c.ServiceCeilingIdentity = auth.ServiceRoleDataWrite, "other-ceiling"
			c.AllowedOperations = []auth.Operation{auth.OperationConnect}
		},
		"domain":      func(c *auth.DecisionConfig) { c.AuthorizationDomain = otherDomain },
		"length cuts": func(c *auth.DecisionConfig) { c.Subject, c.Actor = "writerw", "riter-app" },
	}
	seen := map[shoal.ID]string{original: "original"}
	for name, change := range different {
		got := id(change)
		if previous, ok := seen[got]; ok {
			t.Errorf("%s shares the namespace of %s: %s", name, previous, got)
		}
		seen[got] = name
	}
	decision, err := auth.NewDecision(base)
	if err != nil {
		t.Fatal(err)
	}
	if local, ok := authorized.ApplicationEdgeLocalID(decision, original); !ok || local != "edge" {
		t.Fatalf("ApplicationEdgeLocalID = %q, %v", local, ok)
	}
	other := base
	other.Subject = "other"
	otherDecision, err := auth.NewDecision(other)
	if err != nil {
		t.Fatal(err)
	}
	if local, ok := authorized.ApplicationEdgeLocalID(otherDecision, original); ok {
		t.Fatalf("another identity's ID parsed as its own: %q", local)
	}
	if _, ok := authorized.ApplicationEdgeLocalID(decision, "plain-edge"); ok {
		t.Fatal("an ID outside every namespace parsed")
	}
}
