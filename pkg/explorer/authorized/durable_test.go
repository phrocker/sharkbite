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
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// TestDurablePolicyStoreSurvivesReopen writes a document catalog, an application
// edge, and a committed source claim, closes the store, and reopens it from
// disk. Because the reopened store is a distinct object reconstructed purely
// from persisted records, every assertion below is evidence the registration
// survived the restart rather than lingering in memory.
func TestDurablePolicyStoreSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	store, err := authorized.OpenDurablePolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ruleA, err := authorized.NewAccessRule(
		mustPolicy(t, "domain", "source-a", "policy-a"))
	if err != nil {
		t.Fatal(err)
	}
	ruleB, err := authorized.NewAccessRule(
		mustPolicy(t, "domain", "source-b", "policy-b"))
	if err != nil {
		t.Fatal(err)
	}

	if err := store.PutRevision(ctx, authorized.RevisionRegistration{
		DocumentID: "document",
		RevisionID: "revision-a",
		NodeIDs:    []shoal.ID{"document", "node-a"},
		IntrinsicEdges: []graph.Edge{{
			ID: "intrinsic", From: "document", To: "node-a",
			Type: "contains", Weight: 1,
			Properties: shoal.Metadata{"key": "value"},
		}},
		ContentDigest: auth.DigestBytes("test-content", []byte("revision-a")),
		Rule:          ruleA,
		Current:       true,
	}); err != nil {
		t.Fatal(err)
	}
	// A second current revision replaces the first, so a faithful reload must
	// drop node-a from the node projection just as replaceCurrent does.
	if err := store.PutRevision(ctx, authorized.RevisionRegistration{
		DocumentID:    "document",
		RevisionID:    "revision-b",
		NodeIDs:       []shoal.ID{"document", "node-b"},
		ContentDigest: auth.DigestBytes("test-content", []byte("revision-b")),
		Rule:          ruleA,
		Current:       true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutEdge(ctx, authorized.EdgeRegistration{
		Edge: graph.Edge{
			ID: "application-edge", From: "document", To: "node-b",
			Type: "links", Weight: 1,
		},
		Rule: ruleA,
	}); err != nil {
		t.Fatal(err)
	}

	const sourceURI = "file:///claimed.txt"
	token, err := store.CompareAndSwapSourceClaim(ctx, sourceURI, nil, ruleA)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CommitSourceClaim(ctx, token); err != nil {
		t.Fatal(err)
	}
	committed, ok, err := store.SourceClaim(ctx, sourceURI)
	if err != nil || !ok {
		t.Fatalf("committed source claim before reopen: ok=%v err=%v", ok, err)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := authorized.OpenDurablePolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()

	historical, ok, err := reopened.Revision(ctx, "document", "revision-a")
	if err != nil || !ok {
		t.Fatalf("historical revision after reopen: ok=%v err=%v", ok, err)
	}
	if historical.Current {
		t.Fatalf("historical revision reported current after reopen: %#v", historical)
	}
	if historical.Rule.String() != ruleA.String() {
		t.Fatalf("historical revision rule = %s, want %s",
			historical.Rule.String(), ruleA.String())
	}
	current, ok, err := reopened.Revision(ctx, "document", "revision-b")
	if err != nil || !ok {
		t.Fatalf("current revision after reopen: ok=%v err=%v", ok, err)
	}
	if !current.Current {
		t.Fatalf("current revision not marked current after reopen: %#v", current)
	}
	currentID, ok, err := reopened.CurrentRevision(ctx, "document")
	if err != nil || !ok || currentID.RevisionID != "revision-b" {
		t.Fatalf("current revision id after reopen = %#v ok=%v err=%v",
			currentID, ok, err)
	}

	nodeB, ok, err := reopened.Node(ctx, "node-b")
	if err != nil || !ok || nodeB.RevisionID != "revision-b" {
		t.Fatalf("node-b after reopen = %#v ok=%v err=%v", nodeB, ok, err)
	}
	if _, ok, err := reopened.Node(ctx, "node-a"); err != nil || ok {
		t.Fatalf("superseded node-a survived reopen: ok=%v err=%v", ok, err)
	}
	edge, ok, err := reopened.Edge(ctx, "application-edge")
	if err != nil || !ok || edge.Edge.To != "node-b" {
		t.Fatalf("application edge after reopen = %#v ok=%v err=%v", edge, ok, err)
	}

	reloadedClaim, ok, err := reopened.SourceClaim(ctx, sourceURI)
	if err != nil || !ok {
		t.Fatalf("source claim after reopen: ok=%v err=%v", ok, err)
	}
	if reloadedClaim.Pending || reloadedClaim.PreviousRule != nil {
		t.Fatalf("reloaded source claim not committed: %#v", reloadedClaim)
	}
	if reloadedClaim.Version != committed.Version {
		t.Fatalf("reloaded claim version = %d, want %d",
			reloadedClaim.Version, committed.Version)
	}
	if reloadedClaim.Rule.String() != ruleA.String() {
		t.Fatalf("reloaded claim rule = %s, want %s",
			reloadedClaim.Rule.String(), ruleA.String())
	}

	// The reloaded committed claim must still drive CAS: a transition keyed on
	// it succeeds and carries the persisted rule as its predecessor, proving
	// the version chain continued across the restart rather than resetting.
	transition, err := store2CAS(t, reopened, ctx, sourceURI, &reloadedClaim, ruleB)
	if transition.PreviousRule == nil ||
		transition.PreviousRule.String() != ruleA.String() {
		t.Fatalf("post-reopen transition token = %#v (err=%v)", transition, err)
	}
	if err := reopened.CommitSourceClaim(ctx, transition); err != nil {
		t.Fatal(err)
	}
	rotated, ok, err := reopened.SourceClaim(ctx, sourceURI)
	if err != nil || !ok || rotated.Rule.String() != ruleB.String() {
		t.Fatalf("rotated claim after reopen = %#v ok=%v err=%v", rotated, ok, err)
	}
	if rotated.Version <= committed.Version {
		t.Fatalf("rotated claim version = %d did not advance past %d",
			rotated.Version, committed.Version)
	}
}

func store2CAS(
	t *testing.T,
	store *authorized.DurablePolicyStore,
	ctx context.Context,
	uri string,
	expected *authorized.SourcePolicyClaim,
	rule authorized.AccessRule,
) (authorized.SourcePolicyClaim, error) {
	t.Helper()
	token, err := store.CompareAndSwapSourceClaim(ctx, uri, expected, rule)
	if err != nil {
		t.Fatalf("compare-and-swap after reopen: %v", err)
	}
	return token, err
}

// TestDurableAuthorizedCorpusVisibleAfterRestart is the regression test for
// issue #284. It ingests a document through an authorized client, then closes
// BOTH the base explorer and the durable policy store and reopens them as fresh
// objects — exactly the process-restart scenario in which a MemoryPolicyStore
// loses every registration and Documents returns {"documents":[]}. With the
// durable store the document remains authorized and listable.
func TestDurableAuthorizedCorpusVisibleAfterRestart(t *testing.T) {
	dataDir := t.TempDir()
	policyDir := t.TempDir()

	base, err := explorer.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := authorized.OpenDurablePolicyStore(policyDir)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, time.September, 3, 19, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: now}
	authority, err := auth.NewAuthorityWithClock(clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	reader := &generationReader{generations: map[string]int64{"restart-domain": 1}}
	selector, err := authorized.NewStaticPolicySelector(
		[]byte("restart-source"), []byte("restart-policy"))
	if err != nil {
		t.Fatal(err)
	}
	newClient := func(
		base explorer.Client, store authorized.PolicyStore,
	) *authorized.Client {
		client, err := authorized.NewClient(authorized.Config{
			Base:             base,
			Resolver:         authority.Resolver(),
			PolicySelector:   selector,
			PolicyStore:      store,
			GenerationReader: reader,
			Clock:            clock.Now,
		})
		if err != nil {
			t.Fatal(err)
		}
		return client
	}
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject:               "restart-user",
		Actor:                 "restart-actor",
		AuthorizationDomain:   []byte("restart-domain"),
		AllowedOperations:     allOperations,
		PermittedSourceIDs:    [][]byte{[]byte("restart-source")},
		PermittedPolicyIDs:    [][]byte{[]byte("restart-policy")},
		PolicyGeneration:      1,
		AuthenticationExpires: now.Add(time.Hour),
		RequestID:             "restart-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := authority.Binder().Bind(context.Background(), decision)
	if err != nil {
		t.Fatal(err)
	}

	client := newClient(base, store)
	result, err := client.Ingest(ctx, explorer.Source{
		URI: "file:///restart.txt", MediaType: explorer.MediaTypeText,
		Content: "survives full process restart",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Full shutdown: the base engine AND the policy store are closed and
	// dropped. Nothing authorized survives in memory.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := base.Close(); err != nil {
		t.Fatal(err)
	}

	reopenedBase, err := explorer.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopenedBase.Close()
	reopenedStore, err := authorized.OpenDurablePolicyStore(policyDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopenedStore.Close()

	restarted := newClient(reopenedBase, reopenedStore)
	summaries, err := restarted.Documents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 {
		t.Fatalf("Documents after restart = %#v, want exactly one", summaries)
	}
	if summaries[0].Document.ID != result.Document.ID {
		t.Fatalf("Documents after restart returned %q, want %q",
			summaries[0].Document.ID, result.Document.ID)
	}
	if _, err := restarted.Document(
		ctx, result.Document.ID, result.Revision.ID); err != nil {
		t.Fatalf("Document after restart: %v", err)
	}
}
