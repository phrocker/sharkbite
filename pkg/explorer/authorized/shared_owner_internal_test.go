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
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/dirlock"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/extraction"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Store- and projection-level tests of shared extracted registrations
// (#700): every extracting revision owns a shared node or relation, and a
// reader sees it through any owner it can read.

const sharedNode shoal.ID = "entity-zeta"

func sharedEntity() graph.Node {
	return graph.Node{
		ID: sharedNode, Kind: "ontology:skill", Labels: []string{"skill", "zeta"},
		Properties: shoal.Metadata{
			extraction.GraphPropertyOntologyConceptID:  "concept-skill",
			extraction.GraphPropertyOntologyConceptKey: "skill",
			extraction.GraphPropertyEntityKey:          "zeta",
		},
	}
}

func sharedRelation() graph.Edge {
	return graph.Edge{
		ID: "relation-zeta", From: sharedNode, To: sharedNode,
		Type: "provides_tool", Weight: 0.5,
	}
}

func extractedBy(document, revision shoal.ID, rule AccessRule) NodeRegistration {
	return NodeRegistration{
		DocumentID: document, RevisionID: revision, Node: sharedEntity(),
		Rule: rule, Kind: RegistrationExtracted,
	}
}

func assertedBy(document, revision shoal.ID, rule AccessRule) EdgeRegistration {
	return EdgeRegistration{
		Edge: sharedRelation(), DocumentID: document, RevisionID: revision,
		Rule: rule, Kind: RegistrationExtracted,
	}
}

func putSharedRevision(
	t *testing.T, store PolicyStore, document, revision shoal.ID,
	rule AccessRule, current bool,
) {
	t.Helper()
	if err := store.PutRevision(context.Background(), RevisionRegistration{
		DocumentID: document, RevisionID: revision,
		NodeIDs:       []shoal.ID{document},
		ContentDigest: tightenDigest(string(document) + "/" + string(revision)),
		Rule:          rule, Current: current,
	}); err != nil {
		t.Fatal(err)
	}
}

func ownerKeys(owners []ExtractionOwner) []string {
	keys := make([]string, len(owners))
	for index, owner := range owners {
		keys[index] = string(owner.DocumentID) + "@" + string(owner.RevisionID)
	}
	return keys
}

// TestPutNodeRecordsEveryExtractingRevision: the first extractor keeps the
// registration, every other revision becomes a co-owner exactly once, a
// revision that already owns it under another rule conflicts, and different
// content conflicts. Co-owners survive a reopen.
func TestPutNodeRecordsEveryExtractingRevision(t *testing.T) {
	withTightenStores(t, func(t *testing.T, store PolicyStore, reopen func() PolicyStore) {
		w := newTightenWorld(t)
		ctx := context.Background()
		put := func(registration NodeRegistration) error {
			return store.PutNode(ctx, sharedNode, registration)
		}
		if err := put(extractedBy("doc-1", "rev-1", w.bare)); err != nil {
			t.Fatal(err)
		}
		for _, registration := range []NodeRegistration{
			extractedBy("doc-1", "rev-1", w.bare),
			extractedBy("doc-3", "rev-3", w.secret),
			extractedBy("doc-2", "rev-2", w.bare),
			extractedBy("doc-2", "rev-2", w.bare),
		} {
			if err := put(registration); err != nil {
				t.Fatalf("put %s: %v", registration.DocumentID, err)
			}
		}
		for name, registration := range map[string]NodeRegistration{
			"first owner, other rule": extractedBy("doc-1", "rev-1", w.secret),
			"co-owner, other rule":    extractedBy("doc-2", "rev-2", w.secret),
		} {
			if err := put(registration); !shoal.IsErrorCode(err, shoal.ErrorConflict) {
				t.Fatalf("%s: err = %v, want a conflict", name, err)
			}
		}
		different := extractedBy("doc-4", "rev-4", w.bare)
		different.Node.Labels = []string{"skill", "Zeta"}
		if err := put(different); !shoal.IsErrorCode(err, shoal.ErrorConflict) {
			t.Fatalf("different content: err = %v, want a conflict", err)
		}
		withOwners := extractedBy("doc-5", "rev-5", w.bare)
		withOwners.CoOwners = []ExtractionOwner{{DocumentID: "doc-6", RevisionID: "rev-6", Rule: w.bare}}
		if err := put(withOwners); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
			t.Fatalf("caller-supplied co-owners: err = %v, want refused", err)
		}

		check := func(store PolicyStore, when string) {
			t.Helper()
			registration, ok, err := store.Node(ctx, sharedNode)
			if err != nil || !ok {
				t.Fatalf("%s: node: %v %v", when, ok, err)
			}
			if registration.DocumentID != "doc-1" || !registration.Rule.equal(w.bare) {
				t.Fatalf("%s: first owner = %s, want doc-1", when, registration.DocumentID)
			}
			got := strings.Join(ownerKeys(registration.CoOwners), ",")
			if got != "doc-2@rev-2,doc-3@rev-3" {
				t.Fatalf("%s: co-owners = %s", when, got)
			}
			if !registration.CoOwners[1].Rule.equal(w.secret) {
				t.Fatalf("%s: co-owner rule not kept", when)
			}
		}
		check(store, "before reopen")
		// A read is a copy: changing it changes nothing stored.
		read, _, err := store.Node(ctx, sharedNode)
		if err != nil {
			t.Fatal(err)
		}
		read.CoOwners[0].DocumentID = "doc-forged"
		check(store, "after changing a read")
		check(reopen(), "after reopen")
	})
}

// TestPutEdgeRecordsEveryAssertingRevision is the relation counterpart.
func TestPutEdgeRecordsEveryAssertingRevision(t *testing.T) {
	withTightenStores(t, func(t *testing.T, store PolicyStore, reopen func() PolicyStore) {
		w := newTightenWorld(t)
		ctx := context.Background()
		for _, registration := range []EdgeRegistration{
			assertedBy("doc-1", "rev-1", w.bare),
			assertedBy("doc-2", "rev-2", w.secret),
			assertedBy("doc-2", "rev-2", w.secret),
			assertedBy("doc-1", "rev-1", w.bare),
		} {
			if err := store.PutEdge(ctx, registration); err != nil {
				t.Fatal(err)
			}
		}
		if err := store.PutEdge(ctx, assertedBy("doc-2", "rev-2", w.bare)); !shoal.IsErrorCode(err, shoal.ErrorConflict) {
			t.Fatalf("co-owner, other rule: err = %v, want a conflict", err)
		}
		weaker := assertedBy("doc-3", "rev-3", w.bare)
		weaker.Edge.Weight = 0.9
		if err := store.PutEdge(ctx, weaker); !shoal.IsErrorCode(err, shoal.ErrorConflict) {
			t.Fatalf("different content: err = %v, want a conflict", err)
		}
		check := func(store PolicyStore, when string) {
			t.Helper()
			registration, ok, err := store.Edge(ctx, sharedRelation().ID)
			if err != nil || !ok {
				t.Fatalf("%s: edge: %v %v", when, ok, err)
			}
			if registration.DocumentID != "doc-1" ||
				strings.Join(ownerKeys(registration.CoOwners), ",") != "doc-2@rev-2" {
				t.Fatalf("%s: owners = %s + %v", when, registration.DocumentID,
					ownerKeys(registration.CoOwners))
			}
		}
		check(store, "before reopen")
		check(reopen(), "after reopen")
	})
}

type sharedReaders struct {
	now    time.Time
	plain  *registrationReader
	holder *registrationReader
}

func newSharedReaders(t *testing.T) sharedReaders {
	t.Helper()
	now := time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC)
	secretID, err := LabelPolicyID([]byte("source-a"), "secret")
	if err != nil {
		t.Fatal(err)
	}
	decision := func(subject string, policies ...[]byte) *registrationReader {
		decision, err := auth.NewDecision(auth.DecisionConfig{
			Subject: shoal.ID(subject), Actor: shoal.ID(subject),
			AuthorizationDomain:   []byte("domain"),
			AllowedOperations:     []auth.Operation{auth.OperationRead},
			PermittedSourceIDs:    [][]byte{[]byte("source-a")},
			PermittedPolicyIDs:    policies,
			PolicyGeneration:      1,
			AuthenticationExpires: now.Add(time.Hour),
			RequestID:             shoal.ID("request-" + subject),
		})
		if err != nil {
			t.Fatal(err)
		}
		return readerFor(decision, auth.OperationRead, now)
	}
	return sharedReaders{
		now:    now,
		plain:  decision("plain", []byte("policy-a")),
		holder: decision("holder", []byte("policy-a"), secretID),
	}
}

// sees reports whether reader is allowed the shared node, and through which
// owner it was projected.
func sees(
	t *testing.T, client *Client, reader *registrationReader,
) (bool, shoal.ID) {
	t.Helper()
	resolved, err := client.resolveNodes(
		context.Background(), []shoal.ID{sharedNode}, reader)
	if err != nil {
		t.Fatal(err)
	}
	registration, ok := resolved[sharedNode]
	if !ok {
		return false, ""
	}
	if len(registration.CoOwners) != 0 {
		t.Fatal("a projected registration still carries co-owners")
	}
	if reader == nil {
		return true, registration.DocumentID
	}
	allowed, err := ruleAllows(registration.Rule, reader.decision, reader.operation, reader.now)
	if err != nil {
		t.Fatal(err)
	}
	return allowed, registration.DocumentID
}

// TestSharedNodeVisibilityTruthTable: a node extracted by doc-1 and doc-2 is
// visible to a reader iff the reader can read at least one owner under that
// owner's effective rule, and the projection names the owner it was read
// through.
func TestSharedNodeVisibilityTruthTable(t *testing.T) {
	w := newTightenWorld(t)
	readers := newSharedReaders(t)
	for _, row := range []struct {
		name string
		// relabel moves a document to a secret current revision.
		relabel1, relabel2 bool
		plainSees          bool
		plainThrough       shoal.ID
	}{
		{"neither relabelled", false, false, true, "doc-1"},
		{"first owner relabelled", true, false, true, "doc-2"},
		{"co-owner relabelled", false, true, true, "doc-1"},
		{"both relabelled", true, true, false, ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			store := NewMemoryPolicyStore()
			client := &Client{policyStore: store}
			putSharedRevision(t, store, "doc-1", "rev-1", w.bare, true)
			putSharedRevision(t, store, "doc-2", "rev-2", w.bare, true)
			if err := store.PutNode(context.Background(), sharedNode,
				extractedBy("doc-1", "rev-1", w.bare)); err != nil {
				t.Fatal(err)
			}
			if err := store.PutNode(context.Background(), sharedNode,
				extractedBy("doc-2", "rev-2", w.bare)); err != nil {
				t.Fatal(err)
			}
			if row.relabel1 {
				putSharedRevision(t, store, "doc-1", "rev-1b", w.secret, true)
			}
			if row.relabel2 {
				putSharedRevision(t, store, "doc-2", "rev-2b", w.secret, true)
			}
			visible, through := sees(t, client, readers.plain)
			if visible != row.plainSees {
				t.Fatalf("plain reader sees = %v, want %v", visible, row.plainSees)
			}
			if visible && through != row.plainThrough {
				t.Fatalf("plain reader read it through %s, want %s", through, row.plainThrough)
			}
			if visible, _ := sees(t, client, readers.holder); !visible {
				t.Fatal("the label holder lost the shared node")
			}
			if visible, through := sees(t, client, nil); !visible || through != "doc-1" {
				t.Fatalf("reader-free projection = %v through %s, want doc-1", visible, through)
			}
		})
	}
}

// TestSharedOwnerContributesOnlyWhileItsRevisionCounts: an owner whose
// document has no current revision, or whose own revision is not registered,
// contributes nothing; the node stays visible only through the other owner,
// and disappears with it.
func TestSharedOwnerContributesOnlyWhileItsRevisionCounts(t *testing.T) {
	w := newTightenWorld(t)
	readers := newSharedReaders(t)
	for _, row := range []struct {
		name  string
		setup func(t *testing.T, store *MemoryPolicyStore)
	}{
		{"co-owner's document has no current revision", func(t *testing.T, store *MemoryPolicyStore) {
			putSharedRevision(t, store, "doc-2", "rev-2", w.bare, false)
		}},
		{"co-owner's revision is not registered", func(t *testing.T, store *MemoryPolicyStore) {
			putSharedRevision(t, store, "doc-2", "rev-other", w.bare, true)
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			store := NewMemoryPolicyStore()
			client := &Client{policyStore: store}
			putSharedRevision(t, store, "doc-1", "rev-1", w.bare, true)
			row.setup(t, store)
			for _, registration := range []NodeRegistration{
				extractedBy("doc-1", "rev-1", w.bare),
				extractedBy("doc-2", "rev-2", w.bare),
			} {
				if err := store.PutNode(context.Background(), sharedNode, registration); err != nil {
					t.Fatal(err)
				}
			}
			if visible, through := sees(t, client, readers.plain); !visible || through != "doc-1" {
				t.Fatalf("visible = %v through %s, want doc-1", visible, through)
			}
			putSharedRevision(t, store, "doc-1", "rev-1b", w.secret, true)
			if visible, _ := sees(t, client, readers.plain); visible {
				t.Fatal("the node stayed visible through an owner that contributes nothing")
			}
		})
	}
}

// TestSharedOwnerHeldToItsRevisionsRegisteredRule: a co-owner whose stored
// rule missed a tightening of its revision (a catalog rolled back to a binary
// that does not know co-owners) is still held to the revision's rule.
func TestSharedOwnerHeldToItsRevisionsRegisteredRule(t *testing.T) {
	w := newTightenWorld(t)
	readers := newSharedReaders(t)
	store := NewMemoryPolicyStore()
	client := &Client{policyStore: store}
	putSharedRevision(t, store, "doc-1", "rev-1", w.secret, true)
	putSharedRevision(t, store, "doc-2", "rev-2", w.bare, true)
	ctx := context.Background()
	if err := store.PutNode(ctx, sharedNode, extractedBy("doc-1", "rev-1", w.secret)); err != nil {
		t.Fatal(err)
	}
	if err := store.PutNode(ctx, sharedNode, extractedBy("doc-2", "rev-2", w.bare)); err != nil {
		t.Fatal(err)
	}
	if visible, through := sees(t, client, readers.plain); !visible || through != "doc-2" {
		t.Fatalf("visible = %v through %s, want doc-2", visible, through)
	}
	// What an older binary's TightenRule leaves: the revision (current) is
	// tightened in place, the co-owner it does not know is not.
	key := revisionKey{documentID: "doc-2", revisionID: "rev-2"}
	store.mu.Lock()
	tightened := store.revisions[key]
	tightened.Rule = w.secret
	store.revisions[key] = tightened
	store.mu.Unlock()
	if visible, _ := sees(t, client, readers.plain); visible {
		t.Fatal("a stale co-owner rule reopened a tightened revision")
	}
	if visible, _ := sees(t, client, readers.holder); !visible {
		t.Fatal("the label holder lost the shared node")
	}
}

// TestTightenRuleTightensCoOwners: TightenRule rewrites the rule of every
// co-owner it covers (and only those), persists it, and leaves a shared
// endpoint's application edges alone.
func TestTightenRuleTightensCoOwners(t *testing.T) {
	withTightenStores(t, func(t *testing.T, store PolicyStore, reopen func() PolicyStore) {
		w := newTightenWorld(t)
		ctx := context.Background()
		putSharedRevision(t, store, "doc-1", "rev-1", w.bare, true)
		putSharedRevision(t, store, "doc-2", "rev-2a", w.bare, false)
		putSharedRevision(t, store, "doc-2", "rev-2", w.bare, true)
		for _, registration := range []NodeRegistration{
			extractedBy("doc-1", "rev-1", w.bare),
			extractedBy("doc-2", "rev-2", w.bare),
			extractedBy("doc-2", "rev-2a", w.bare),
		} {
			if err := store.PutNode(ctx, sharedNode, registration); err != nil {
				t.Fatal(err)
			}
		}
		for _, registration := range []EdgeRegistration{
			assertedBy("doc-1", "rev-1", w.bare),
			assertedBy("doc-2", "rev-2", w.bare),
		} {
			if err := store.PutEdge(ctx, registration); err != nil {
				t.Fatal(err)
			}
		}
		connected := EdgeRegistration{
			Edge: graph.Edge{ID: "connected", From: sharedNode, To: sharedNode, Type: "references", Weight: 1},
			Rule: w.bare, Kind: RegistrationApplication,
		}
		if err := store.PutEdge(ctx, connected); err != nil {
			t.Fatal(err)
		}
		changed, err := tightenFor(store, "doc-2", "rev-2", "file:///doc-2", w.bare, w.secret)
		if err != nil || !changed {
			t.Fatalf("tighten = %v, %v", changed, err)
		}
		if again, err := tightenFor(store, "doc-2", "rev-2", "file:///doc-2", w.bare, w.secret); err != nil || again {
			t.Fatalf("repeated tighten = %v, %v; want a no-op", again, err)
		}
		check := func(store PolicyStore, when string) {
			t.Helper()
			node, _, err := store.Node(ctx, sharedNode)
			if err != nil {
				t.Fatal(err)
			}
			if !node.Rule.equal(w.bare) {
				t.Fatalf("%s: the first owner (another document) was tightened", when)
			}
			for _, owner := range node.CoOwners {
				if !owner.Rule.equal(w.secret) {
					t.Fatalf("%s: co-owner %s@%s rule not tightened", when,
						owner.DocumentID, owner.RevisionID)
				}
			}
			edge, _, err := store.Edge(ctx, sharedRelation().ID)
			if err != nil {
				t.Fatal(err)
			}
			if !edge.Rule.equal(w.bare) || len(edge.CoOwners) != 1 ||
				!edge.CoOwners[0].Rule.equal(w.secret) {
				t.Fatalf("%s: relation owners not tightened as expected", when)
			}
			application, _, err := store.Edge(ctx, "connected")
			if err != nil {
				t.Fatal(err)
			}
			if !application.Rule.equal(w.bare) {
				t.Fatalf("%s: an application edge on a shared endpoint was tightened", when)
			}
		}
		check(store, "before reopen")
		reopened := reopen()
		check(reopened, "after reopen")
		// Tightening the first owner leaves the application edge on the
		// shared endpoint alone too.
		if changed, err := tightenFor(reopened, "doc-1", "rev-1", "file:///doc-1", w.bare, w.secret); err != nil || !changed {
			t.Fatalf("tighten the first owner = %v, %v", changed, err)
		}
		application, _, err := reopened.Edge(ctx, "connected")
		if err != nil {
			t.Fatal(err)
		}
		if !application.Rule.equal(w.bare) {
			t.Fatal("tightening the first owner tightened an application edge on a shared endpoint")
		}
		// Re-extraction by a tightened co-owner is idempotent.
		if err := reopened.PutNode(ctx, sharedNode, extractedBy("doc-2", "rev-2", w.secret)); err != nil {
			t.Fatalf("re-extraction after tightening: %v", err)
		}
	})
}

// TestDurableCoOwnerDecodingIsStrict: a co-owner row that does not match its
// record, names no extracted registration, repeats the first owner, or sits
// in a catalog without the format floor refuses the open.
func TestDurableCoOwnerDecodingIsStrict(t *testing.T) {
	w := newTightenWorld(t)
	rule, err := ruleToPersisted(w.bare)
	if err != nil {
		t.Fatal(err)
	}
	owner := func(object, document, revision string) persistedOwner {
		return persistedOwner{
			Seq: 1 << 40, ObjectID: object, DocumentID: document,
			RevisionID: revision, Rule: rule,
		}
	}
	keyOf := func(record persistedOwner) ownerKey {
		return ownerKey{
			objectID: shoal.ID(record.ObjectID), documentID: shoal.ID(record.DocumentID),
			revisionID: shoal.ID(record.RevisionID),
		}
	}
	for _, row := range []struct {
		name   string
		row    func(persistedOwner) []byte
		record persistedOwner
		kind   byte
	}{
		{"row does not match", func(persistedOwner) []byte {
			return policyOwnerRow(policyRowNodeOwner, ownerKey{"other", "doc-2", "rev-2"})
		}, owner(string(sharedNode), "doc-2", "rev-2"), policyKindNodeOwner},
		{"no such node", nil, owner("entity-missing", "doc-2", "rev-2"), policyKindNodeOwner},
		{"no such edge", nil, owner("relation-missing", "doc-2", "rev-2"), policyKindEdgeOwner},
		{"repeats the first owner", nil, owner(string(sharedNode), "doc-1", "rev-1"), policyKindNodeOwner},
		{"wrong kind", nil, owner(string(sharedNode), "doc-2", "rev-2"), policyKindEdgeOwner},
	} {
		t.Run(row.name, func(t *testing.T) {
			dir := t.TempDir()
			store, err := OpenDurablePolicyStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.PutNode(context.Background(), sharedNode,
				extractedBy("doc-1", "rev-1", w.bare)); err != nil {
				t.Fatal(err)
			}
			prefix := policyRowNodeOwner
			if row.kind == policyKindEdgeOwner {
				prefix = policyRowEdgeOwner
			}
			key := policyOwnerRow(prefix, keyOf(row.record))
			if row.row != nil {
				key = row.row(row.record)
			}
			if row.name == "wrong kind" {
				key = policyOwnerRow(policyRowNodeOwner, keyOf(row.record))
			}
			store.mu.Lock()
			err = store.writeRow(key, row.kind, row.record)
			store.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenDurablePolicyStore(dir)
			if err == nil {
				_ = reopened.Close()
				t.Fatal("a corrupt co-owner row opened")
			}
			if !strings.Contains(err.Error(), "co-owner") {
				t.Fatalf("err = %v, want a co-owner corruption", err)
			}
		})
	}
	t.Run("no format floor", func(t *testing.T) {
		dir := t.TempDir()
		lock, err := dirlock.Acquire(dir, policyLockFile)
		if err != nil {
			t.Fatal(err)
		}
		store, err := openLockedDurablePolicyStore(dir, lock, false)
		if err != nil {
			t.Fatal(err)
		}
		record := owner(string(sharedNode), "doc-2", "rev-2")
		store.mu.Lock()
		err = store.writeRow(policyNodeRow(sharedNode), policyKindNode, persistedNode{
			Seq: 1, NodeID: string(sharedNode), DocumentID: "doc-1", RevisionID: "rev-1",
			Node: graphNodeToPersisted(sharedEntity()), Rule: rule,
			Kind: uint8(RegistrationExtracted),
		})
		if err == nil {
			err = store.writeRow(policyOwnerRow(policyRowNodeOwner, keyOf(record)),
				policyKindNodeOwner, record)
		}
		store.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		if reopened, err := OpenDurablePolicyStore(dir); err == nil {
			_ = reopened.Close()
			t.Fatal("a co-owner row without the format floor opened")
		} else if !strings.Contains(err.Error(), "co-owner") ||
			!strings.Contains(err.Error(), "corrupt") {
			t.Fatalf("err = %v, want a co-owner corruption", err)
		}
	})
}

// TestCatalogWithoutCoOwnersIsSingleOwner: a catalog written before #700 has
// no co-owner rows, and each registration keeps its one owner.
func TestCatalogWithoutCoOwnersIsSingleOwner(t *testing.T) {
	w := newTightenWorld(t)
	readers := newSharedReaders(t)
	dir := t.TempDir()
	store, err := OpenDurablePolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	putSharedRevision(t, store, "doc-1", "rev-1", w.bare, true)
	if err := store.PutNode(context.Background(), sharedNode,
		extractedBy("doc-1", "rev-1", w.bare)); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenDurablePolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	registration, ok, err := reopened.Node(context.Background(), sharedNode)
	if err != nil || !ok || registration.DocumentID != "doc-1" || len(registration.CoOwners) != 0 {
		t.Fatalf("legacy registration = %+v, %v, %v", registration, ok, err)
	}
	if visible, through := sees(t, &Client{policyStore: reopened}, readers.plain); !visible || through != "doc-1" {
		t.Fatalf("legacy registration visible = %v through %s", visible, through)
	}
	// The next extraction by another revision adds a co-owner to it.
	putSharedRevision(t, reopened, "doc-2", "rev-2", w.bare, true)
	if err := reopened.PutNode(context.Background(), sharedNode,
		extractedBy("doc-2", "rev-2", w.bare)); err != nil {
		t.Fatal(err)
	}
	registration, _, _ = reopened.Node(context.Background(), sharedNode)
	if registration.DocumentID != "doc-1" ||
		strings.Join(ownerKeys(registration.CoOwners), ",") != "doc-2@rev-2" {
		t.Fatalf("migrated registration owners = %s + %v",
			registration.DocumentID, ownerKeys(registration.CoOwners))
	}
}

// TestSharedNodeOutlivesItsFirstOwner: a first owner whose document has no
// current revision contributes nothing, and a co-owner still does.
func TestSharedNodeOutlivesItsFirstOwner(t *testing.T) {
	w := newTightenWorld(t)
	readers := newSharedReaders(t)
	store := NewMemoryPolicyStore()
	client := &Client{policyStore: store}
	putSharedRevision(t, store, "doc-1", "rev-1", w.bare, false)
	putSharedRevision(t, store, "doc-2", "rev-2", w.bare, true)
	for _, registration := range []NodeRegistration{
		extractedBy("doc-1", "rev-1", w.bare),
		extractedBy("doc-2", "rev-2", w.bare),
	} {
		if err := store.PutNode(context.Background(), sharedNode, registration); err != nil {
			t.Fatal(err)
		}
	}
	for name, reader := range map[string]*registrationReader{
		"plain": readers.plain, "reader-free": nil,
	} {
		if visible, through := sees(t, client, reader); !visible || through != "doc-2" {
			t.Fatalf("%s: visible = %v through %s, want doc-2", name, visible, through)
		}
	}
}

// TestOwnersAreKeptPerDocument: a document that extracts the entity again
// from a newer revision moves its owner entry there instead of adding one,
// for the first owner and a co-owner alike, durably.
func TestOwnersAreKeptPerDocument(t *testing.T) {
	withTightenStores(t, func(t *testing.T, store PolicyStore, reopen func() PolicyStore) {
		w := newTightenWorld(t)
		ctx := context.Background()
		for _, registration := range []NodeRegistration{
			extractedBy("doc-1", "rev-1", w.bare),
			extractedBy("doc-2", "rev-2", w.bare),
			extractedBy("doc-3", "rev-3", w.bare),
			extractedBy("doc-2", "rev-2b", w.secret),
			extractedBy("doc-1", "rev-1b", w.secret),
		} {
			if err := store.PutNode(ctx, sharedNode, registration); err != nil {
				t.Fatalf("put %s@%s: %v", registration.DocumentID, registration.RevisionID, err)
			}
		}
		check := func(store PolicyStore, when string) {
			t.Helper()
			registration, ok, err := store.Node(ctx, sharedNode)
			if err != nil || !ok {
				t.Fatalf("%s: %v %v", when, ok, err)
			}
			if registration.DocumentID != "doc-1" || registration.RevisionID != "rev-1b" ||
				!registration.Rule.equal(w.secret) {
				t.Fatalf("%s: first owner = %s@%s", when, registration.DocumentID, registration.RevisionID)
			}
			if got := strings.Join(ownerKeys(registration.CoOwners), ","); got != "doc-2@rev-2b,doc-3@rev-3" {
				t.Fatalf("%s: co-owners = %s", when, got)
			}
			if !registration.CoOwners[0].Rule.equal(w.secret) {
				t.Fatalf("%s: moved co-owner kept its old rule", when)
			}
		}
		check(store, "before reopen")
		check(reopen(), "after reopen")
	})
}

// TestTighteningOneOwnerLeavesTheOthers: a tightening of the first owner's
// document rewrites no co-owner of another document.
func TestTighteningOneOwnerLeavesTheOthers(t *testing.T) {
	withTightenStores(t, func(t *testing.T, store PolicyStore, reopen func() PolicyStore) {
		w := newTightenWorld(t)
		ctx := context.Background()
		putSharedRevision(t, store, "doc-1", "rev-1", w.bare, true)
		putSharedRevision(t, store, "doc-2", "rev-2", w.bare, true)
		for _, registration := range []NodeRegistration{
			extractedBy("doc-1", "rev-1", w.bare),
			extractedBy("doc-2", "rev-2", w.bare),
		} {
			if err := store.PutNode(ctx, sharedNode, registration); err != nil {
				t.Fatal(err)
			}
		}
		for _, registration := range []EdgeRegistration{
			assertedBy("doc-1", "rev-1", w.bare),
			assertedBy("doc-2", "rev-2", w.bare),
		} {
			if err := store.PutEdge(ctx, registration); err != nil {
				t.Fatal(err)
			}
		}
		if changed, err := tightenFor(store, "doc-1", "rev-1", "file:///doc-1", w.bare, w.secret); err != nil || !changed {
			t.Fatalf("tighten = %v, %v", changed, err)
		}
		check := func(store PolicyStore) {
			t.Helper()
			node, _, err := store.Node(ctx, sharedNode)
			if err != nil {
				t.Fatal(err)
			}
			if !node.Rule.equal(w.secret) || !node.CoOwners[0].Rule.equal(w.bare) {
				t.Fatal("tightening doc-1 did not tighten exactly doc-1's ownership of the node")
			}
			edge, _, err := store.Edge(ctx, sharedRelation().ID)
			if err != nil {
				t.Fatal(err)
			}
			if !edge.Rule.equal(w.secret) || !edge.CoOwners[0].Rule.equal(w.bare) {
				t.Fatal("tightening doc-1 did not tighten exactly doc-1's ownership of the relation")
			}
		}
		check(store)
		check(reopen())
	})
}

// TestSharedContentIsCanonical: an extracted entity or relation is stored as
// its identity alone, so extractions that present it differently share it,
// and a stored record from before that rule loads canonical.
func TestSharedContentIsCanonical(t *testing.T) {
	withTightenStores(t, func(t *testing.T, store PolicyStore, reopen func() PolicyStore) {
		w := newTightenWorld(t)
		ctx := context.Background()
		const id shoal.ID = "inferred-entity:0000000000000000000000000000000000000000000000000000000000000001"
		presented := func(document, revision shoal.ID, name, confidence string) NodeRegistration {
			node := sharedEntity()
			node.ID = id
			node.Labels = []string{"skill", name}
			node.Properties = cloneMetadata(node.Properties)
			node.Properties["confidence"] = confidence
			registration := extractedBy(document, revision, w.bare)
			registration.Node = node
			return registration
		}
		if err := store.PutNode(ctx, id, presented("doc-1", "rev-1", "zeta", "0.9")); err != nil {
			t.Fatal(err)
		}
		if err := store.PutNode(ctx, id, presented("doc-2", "rev-2", "ZETA!", "0.1")); err != nil {
			t.Fatalf("a differently presented extraction was refused: %v", err)
		}
		const relation shoal.ID = "inferred-edge:0000000000000000000000000000000000000000000000000000000000000001"
		weighed := func(document, revision shoal.ID, weight shoal.Score) EdgeRegistration {
			registration := assertedBy(document, revision, w.bare)
			registration.Edge = graph.Edge{
				ID: relation, From: id, To: id, Type: "provides_tool", Weight: weight,
				Properties: shoal.Metadata{"ontology_relationship_id": "rel-1", "note": "x"},
			}
			return registration
		}
		if err := store.PutEdge(ctx, weighed("doc-1", "rev-1", 0.9)); err != nil {
			t.Fatal(err)
		}
		if err := store.PutEdge(ctx, weighed("doc-2", "rev-2", 0.2)); err != nil {
			t.Fatalf("a differently weighed relation was refused: %v", err)
		}
		check := func(store PolicyStore) {
			t.Helper()
			node, _, err := store.Node(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := node.Node.Properties["confidence"]; ok ||
				strings.Join(node.Node.Labels, ",") != "skill,zeta" || len(node.CoOwners) != 1 {
				t.Fatalf("stored node is not canonical: %+v", node)
			}
			edge, _, err := store.Edge(ctx, relation)
			if err != nil {
				t.Fatal(err)
			}
			if edge.Edge.Weight != 1 || len(edge.Edge.Properties) != 1 || len(edge.CoOwners) != 1 {
				t.Fatalf("stored relation is not canonical: %+v", edge)
			}
		}
		check(store)
		check(reopen())
	})
	t.Run("legacy durable row", func(t *testing.T) {
		w := newTightenWorld(t)
		dir := t.TempDir()
		store, err := OpenDurablePolicyStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		rule, err := ruleToPersisted(w.bare)
		if err != nil {
			t.Fatal(err)
		}
		node := sharedEntity()
		node.ID = "inferred-entity:0000000000000000000000000000000000000000000000000000000000000001"
		node.Labels = []string{"Zeta", "skill"}
		node.Properties = cloneMetadata(node.Properties)
		node.Properties["confidence"] = "0.500000"
		store.mu.Lock()
		err = store.writeRow(policyNodeRow(node.ID), policyKindNode, persistedNode{
			Seq: 1 << 40, NodeID: string(node.ID), DocumentID: "doc-1", RevisionID: "rev-1",
			Node: graphNodeToPersisted(node), Rule: rule, Kind: uint8(RegistrationExtracted),
		})
		store.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := OpenDurablePolicyStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close()
		loaded, _, err := reopened.Node(context.Background(), node.ID)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(loaded.Node.Labels, ",") != "skill,zeta" {
			t.Fatalf("legacy node loaded as %+v", loaded.Node)
		}
		if err := reopened.PutNode(context.Background(), node.ID, extractedByNode(
			"doc-2", "rev-2", w.bare, loaded.Node)); err != nil {
			t.Fatalf("a co-owner of a legacy node was refused: %v", err)
		}
	})
}

func extractedByNode(document, revision shoal.ID, rule AccessRule, node graph.Node) NodeRegistration {
	registration := extractedBy(document, revision, rule)
	registration.Node = node
	return registration
}

// TestRelationOwnersAreKeptPerDocument is the relation counterpart: a
// document's newer revision moves its owner entry, and takes that revision's
// rule with it. Keeping the older rule would widen a relation whose document
// was relabelled and extracted again.
func TestRelationOwnersAreKeptPerDocument(t *testing.T) {
	withTightenStores(t, func(t *testing.T, store PolicyStore, reopen func() PolicyStore) {
		w := newTightenWorld(t)
		ctx := context.Background()
		for _, registration := range []EdgeRegistration{
			assertedBy("doc-1", "rev-1", w.bare),
			assertedBy("doc-2", "rev-2", w.bare),
			assertedBy("doc-2", "rev-2b", w.secret),
			assertedBy("doc-1", "rev-1b", w.secret),
		} {
			if err := store.PutEdge(ctx, registration); err != nil {
				t.Fatalf("put %s@%s: %v", registration.DocumentID, registration.RevisionID, err)
			}
		}
		check := func(store PolicyStore, when string) {
			t.Helper()
			registration, ok, err := store.Edge(ctx, sharedRelation().ID)
			if err != nil || !ok {
				t.Fatalf("%s: %v %v", when, ok, err)
			}
			if registration.DocumentID != "doc-1" || registration.RevisionID != "rev-1b" ||
				!registration.Rule.equal(w.secret) {
				t.Fatalf("%s: first owner = %s@%s under %s", when,
					registration.DocumentID, registration.RevisionID, registration.Rule)
			}
			if got := strings.Join(ownerKeys(registration.CoOwners), ","); got != "doc-2@rev-2b" ||
				!registration.CoOwners[0].Rule.equal(w.secret) {
				t.Fatalf("%s: co-owners = %s", when, got)
			}
		}
		check(store, "before reopen")
		check(reopen(), "after reopen")
	})
}
