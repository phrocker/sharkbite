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
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestAccessRuleConjunctionCanonicalization(t *testing.T) {
	policyA := mustPolicy(t, "domain", "source-a", "policy-a")
	policyB := mustPolicyEpoch(t, "domain", "source-b", "policy-b", 2)
	rule, err := authorized.NewAccessRule(policyB, policyA, policyA)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 26, 20, 0, 0, 0, time.UTC)
	onlyA := mustDecision(
		t, now, "rule-a",
		[]byte("domain"),
		[][]byte{[]byte("source-a")},
		[][]byte{[]byte("policy-a")},
	)
	if err := rule.Authorize(
		onlyA, auth.OperationRead, now); !shoal.IsErrorCode(
		err, shoal.ErrorUnauthorized,
	) {
		t.Fatalf("partial conjunction error = %v", err)
	}
	both := mustDecisionAtGeneration(
		t, now, "rule-both",
		[]byte("domain"),
		[][]byte{[]byte("source-a"), []byte("source-b")},
		[][]byte{[]byte("policy-a"), []byte("policy-b")},
		3,
	)
	if err := rule.Authorize(both, auth.OperationRead, now); err != nil {
		t.Fatal(err)
	}
	rendered := rule.String()
	for _, sensitive := range []string{"domain", "source-a", "policy-b"} {
		if strings.Contains(rendered, sensitive) {
			t.Fatalf("rule String disclosed %q: %s", sensitive, rendered)
		}
	}
	otherDomain := mustPolicy(t, "other-domain", "source-a", "policy-a")
	if _, err := authorized.NewAccessRule(
		policyA, otherDomain); !shoal.IsErrorCode(
		err, shoal.ErrorInvalidArgument,
	) {
		t.Fatalf("cross-domain rule error = %v", err)
	}
	sameLogicalNewEpoch := mustPolicyEpoch(
		t, "domain", "source-a", "policy-a", 9)
	deduplicated, err := authorized.NewAccessRule(
		sameLogicalNewEpoch, policyA)
	if err != nil {
		t.Fatal(err)
	}
	single, err := authorized.NewAccessRule(policyA)
	if err != nil {
		t.Fatal(err)
	}
	if deduplicated.String() != single.String() {
		t.Fatalf("physical epochs changed logical rule: %s != %s",
			deduplicated.String(), single.String())
	}
}

func TestStaticPolicySelectorDefensivelyOwnsConfiguration(t *testing.T) {
	source := []byte("configured-source")
	policy := []byte("configured-policy")
	selector, err := authorized.NewStaticPolicySelector(source, policy)
	if err != nil {
		t.Fatal(err)
	}
	source[0] = 'X'
	policy[0] = 'X'
	now := time.Date(2026, time.August, 26, 20, 0, 0, 0, time.UTC)
	decision := mustDecision(
		t, now, "static-selector", []byte("domain"),
		[][]byte{[]byte("configured-source")},
		[][]byte{[]byte("configured-policy")},
	)
	selected, err := selector.SelectPolicy(
		context.Background(), decision, explorer.Source{
			URI: "file:///forged-policy", Metadata: shoal.Metadata{
				"source": "other", "policy": "other",
			},
		})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(selected.SourceID(), []byte("configured-source")) ||
		!bytes.Equal(selected.GrantPolicyID(), []byte("configured-policy")) {
		t.Fatalf("selected policy used caller/request data: %s", selected.String())
	}
}

func TestMemoryPolicyStoreAtomicIdempotentAndDefensive(t *testing.T) {
	ctx := context.Background()
	store := authorized.NewMemoryPolicyStore()
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
	nodes := []shoal.ID{"document", "node-a"}
	intrinsic := graph.Edge{
		ID: "intrinsic", From: "document", To: "node-a",
		Type: "contains", Weight: 1,
		Properties: shoal.Metadata{"key": "value"},
	}
	registration := authorized.RevisionRegistration{
		DocumentID:     "document",
		RevisionID:     "revision-a",
		NodeIDs:        nodes,
		IntrinsicEdges: []graph.Edge{intrinsic},
		ContentDigest:  auth.DigestBytes("test-content", []byte("revision-a")),
		Rule:           ruleA,
		Current:        true,
	}
	if err := store.PutRevision(ctx, registration); err != nil {
		t.Fatal(err)
	}
	if err := store.PutRevision(ctx, registration); err != nil {
		t.Fatalf("identical revision was not idempotent: %v", err)
	}
	nodes[0] = "mutated"
	intrinsic.Properties["key"] = "mutated"
	registration.NodeIDs[1] = "mutated-node"
	registration.IntrinsicEdges[0].Properties["key"] = "mutated-again"

	stored, ok, err := store.Revision(ctx, "document", "revision-a")
	if err != nil || !ok {
		t.Fatalf("stored revision: ok=%v err=%v", ok, err)
	}
	if stored.NodeIDs[0] != "document" || stored.NodeIDs[1] != "node-a" ||
		stored.IntrinsicEdges[0].Properties["key"] != "value" {
		t.Fatalf("store retained caller aliases: %#v", stored)
	}
	stored.NodeIDs[0] = "returned-mutation"
	stored.IntrinsicEdges[0].Properties["key"] = "returned-mutation"
	again, ok, err := store.Revision(ctx, "document", "revision-a")
	if err != nil || !ok {
		t.Fatalf("stored revision reread: ok=%v err=%v", ok, err)
	}
	if again.NodeIDs[0] != "document" ||
		again.IntrinsicEdges[0].Properties["key"] != "value" {
		t.Fatalf("store returned internal aliases: %#v", again)
	}

	conflict := authorized.RevisionRegistration{
		DocumentID: "document",
		RevisionID: "revision-a",
		NodeIDs:    []shoal.ID{"document", "node-a"},
		IntrinsicEdges: []graph.Edge{{
			ID: "intrinsic", From: "document", To: "node-a",
			Type: "contains", Weight: 1,
			Properties: shoal.Metadata{"key": "value"},
		}},
		ContentDigest: auth.DigestBytes("test-content", []byte("revision-a")),
		Rule:          ruleB,
		Current:       true,
	}
	if err := store.PutRevision(ctx, conflict); !shoal.IsErrorCode(
		err, shoal.ErrorConflict,
	) {
		t.Fatalf("policy reuse conflict = %v", err)
	}
	identityReuse := conflict
	identityReuse.DocumentID = "other-document"
	identityReuse.NodeIDs = []shoal.ID{"other-document", "other-node"}
	identityReuse.IntrinsicEdges = nil
	identityReuse.Rule = ruleA
	if err := store.PutRevision(ctx, identityReuse); !shoal.IsErrorCode(
		err, shoal.ErrorConflict,
	) {
		t.Fatalf("revision identity reuse = %v", err)
	}

	next := authorized.RevisionRegistration{
		DocumentID:    "document",
		RevisionID:    "revision-b",
		NodeIDs:       []shoal.ID{"document", "node-b"},
		ContentDigest: auth.DigestBytes("test-content", []byte("revision-b")),
		Rule:          ruleA,
		Current:       true,
	}
	if err := store.PutRevision(ctx, next); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.Node(ctx, "node-a"); err != nil || ok {
		t.Fatalf("old current node remained: ok=%v err=%v", ok, err)
	}
	node, ok, err := store.Node(ctx, "node-b")
	if err != nil || !ok || node.RevisionID != "revision-b" {
		t.Fatalf("new current node = %#v ok=%v err=%v", node, ok, err)
	}
	historical, ok, err := store.Revision(ctx, "document", "revision-a")
	if err != nil || !ok || historical.Current {
		t.Fatalf("historical revision = %#v ok=%v err=%v", historical, ok, err)
	}

	edge := authorized.EdgeRegistration{
		Edge: graph.Edge{
			ID: "application-edge", From: "document", To: "node-b",
			Type: "links", Weight: 1,
			Properties: shoal.Metadata{"edge": "value"},
		},
		Rule: ruleA,
	}
	if err := store.PutEdge(ctx, edge); err != nil {
		t.Fatal(err)
	}
	if err := store.PutEdge(ctx, edge); err != nil {
		t.Fatalf("identical edge was not idempotent: %v", err)
	}
	newEpochRule, err := authorized.NewAccessRule(
		mustPolicyEpoch(t, "domain", "source-a", "policy-a", 7))
	if err != nil {
		t.Fatal(err)
	}
	newEpochEdge := edge
	newEpochEdge.Rule = newEpochRule
	if err := store.PutEdge(ctx, newEpochEdge); err != nil {
		t.Fatalf("logical edge policy changed across epoch: %v", err)
	}
	edge.Edge.Properties["edge"] = "mutated"
	storedEdge, ok, err := store.Edge(ctx, "application-edge")
	if err != nil || !ok ||
		storedEdge.Edge.Properties["edge"] != "value" {
		t.Fatalf("stored edge = %#v ok=%v err=%v", storedEdge, ok, err)
	}
	edgeConflict := storedEdge
	edgeConflict.Rule = ruleB
	if err := store.PutEdge(ctx, edgeConflict); !shoal.IsErrorCode(
		err, shoal.ErrorConflict,
	) {
		t.Fatalf("edge policy reuse conflict = %v", err)
	}
}

func TestMemoryPolicyStoreConcurrentAccess(t *testing.T) {
	store := authorized.NewMemoryPolicyStore()
	rule, err := authorized.NewAccessRule(
		mustPolicy(t, "domain", "source", "policy"))
	if err != nil {
		t.Fatal(err)
	}
	registration := authorized.RevisionRegistration{
		DocumentID:    "document",
		RevisionID:    "revision",
		NodeIDs:       []shoal.ID{"document", "node"},
		ContentDigest: auth.DigestBytes("concurrent-content", []byte("revision")),
		Rule:          rule,
		Current:       true,
	}
	var wait sync.WaitGroup
	errorsSeen := make(chan error, 128)
	for index := 0; index < 64; index++ {
		wait.Add(2)
		go func() {
			defer wait.Done()
			errorsSeen <- store.PutRevision(context.Background(), registration)
		}()
		go func() {
			defer wait.Done()
			_, _, err := store.CurrentRevision(context.Background(), "document")
			errorsSeen <- err
		}()
	}
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestMemoryPolicyStoreSourceClaimCAS(t *testing.T) {
	ctx := context.Background()
	store := authorized.NewMemoryPolicyStore()
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
	const uri = "file:///claimed.txt"
	first, err := store.CompareAndSwapSourceClaim(ctx, uri, nil, ruleA)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Pending || first.PreviousRule != nil {
		t.Fatalf("new source claim token = %#v", first)
	}
	if _, err := store.CompareAndSwapSourceClaim(
		ctx, uri, nil, ruleB,
	); !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("concurrent source claim = %v", err)
	}
	if err := store.PendSourceClaim(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := store.PendSourceClaim(ctx, first); err != nil {
		t.Fatalf("repeated pend = %v", err)
	}
	if err := store.CommitSourceClaim(ctx, first); !shoal.IsErrorCode(
		err, shoal.ErrorConflict,
	) {
		t.Fatalf("different finish after pend = %v", err)
	}
	pending, ok, err := store.SourceClaim(ctx, uri)
	if err != nil || !ok || !pending.Pending ||
		pending.PreviousRule != nil {
		t.Fatalf("pending source claim = %#v ok=%v err=%v", pending, ok, err)
	}
	recovery, err := store.CompareAndSwapSourceClaim(
		ctx, uri, &pending, ruleA)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CommitSourceClaim(ctx, recovery); err != nil {
		t.Fatal(err)
	}
	if err := store.CommitSourceClaim(ctx, recovery); err != nil {
		t.Fatalf("repeated commit = %v", err)
	}
	if err := store.PendSourceClaim(ctx, recovery); !shoal.IsErrorCode(
		err, shoal.ErrorConflict,
	) {
		t.Fatalf("different finish after commit = %v", err)
	}
	if err := store.RollbackSourceClaim(ctx, recovery); !shoal.IsErrorCode(
		err, shoal.ErrorConflict,
	) {
		t.Fatalf("rollback after commit = %v", err)
	}
	stored, ok, err := store.SourceClaim(ctx, uri)
	if err != nil || !ok || stored.Pending || stored.PreviousRule != nil {
		t.Fatalf("committed source claim = %#v ok=%v err=%v", stored, ok, err)
	}
	transitioned, err := store.CompareAndSwapSourceClaim(
		ctx, uri, &stored, ruleB)
	if err != nil {
		t.Fatal(err)
	}
	if !transitioned.Pending || transitioned.PreviousRule == nil ||
		transitioned.PreviousRule.String() != ruleA.String() {
		t.Fatalf("transition token = %#v", transitioned)
	}
	if err := store.RollbackSourceClaim(ctx, transitioned); err != nil {
		t.Fatal(err)
	}
	if err := store.RollbackSourceClaim(ctx, transitioned); err != nil {
		t.Fatalf("repeated rollback = %v", err)
	}
	if err := store.CommitSourceClaim(ctx, transitioned); !shoal.IsErrorCode(
		err, shoal.ErrorConflict,
	) {
		t.Fatalf("commit after rollback = %v", err)
	}
	if err := store.PendSourceClaim(ctx, transitioned); !shoal.IsErrorCode(
		err, shoal.ErrorConflict,
	) {
		t.Fatalf("pend after rollback = %v", err)
	}
	restored, ok, err := store.SourceClaim(ctx, uri)
	if err != nil || !ok || restored.Version != stored.Version ||
		restored.Pending || restored.PreviousRule != nil ||
		restored.Rule.String() != ruleA.String() {
		t.Fatalf("restored source claim = %#v ok=%v err=%v", restored, ok, err)
	}
	next, err := store.CompareAndSwapSourceClaim(ctx, uri, &restored, ruleB)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RollbackSourceClaim(ctx, transitioned); !shoal.IsErrorCode(
		err, shoal.ErrorConflict,
	) {
		t.Fatalf("stale rollback affected newer acquisition = %v", err)
	}
	if err := store.RollbackSourceClaim(ctx, next); err != nil {
		t.Fatal(err)
	}

	const newURI = "file:///rollback-to-absent.txt"
	created, err := store.CompareAndSwapSourceClaim(ctx, newURI, nil, ruleA)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RollbackSourceClaim(ctx, created); err != nil {
		t.Fatal(err)
	}
	if err := store.RollbackSourceClaim(ctx, created); err != nil {
		t.Fatalf("repeated rollback to absence = %v", err)
	}
	if _, ok, err := store.SourceClaim(ctx, newURI); err != nil || ok {
		t.Fatalf("rollback-to-absence state: ok=%v err=%v", ok, err)
	}
}

func mustPolicy(
	t *testing.T,
	domain, source, policy string,
) auth.Policy {
	return mustPolicyEpoch(t, domain, source, policy, 1)
}

func mustPolicyEpoch(
	t *testing.T,
	domain, source, policy string,
	epoch int64,
) auth.Policy {
	t.Helper()
	value, err := auth.NewPolicy(auth.PolicyConfig{
		AuthorizationDomain: []byte(domain),
		SourceID:            []byte(source),
		GrantPolicyID:       []byte(policy),
		Epoch:               epoch,
	})
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func mustDecision(
	t *testing.T,
	now time.Time,
	subject string,
	domain []byte,
	sources, policies [][]byte,
) auth.Decision {
	return mustDecisionAtGeneration(
		t, now, subject, domain, sources, policies, 1)
}

func mustDecisionAtGeneration(
	t *testing.T,
	now time.Time,
	subject string,
	domain []byte,
	sources, policies [][]byte,
	generation int64,
) auth.Decision {
	t.Helper()
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject:               shoal.ID(subject),
		Actor:                 shoal.ID(subject + "-actor"),
		AuthorizationDomain:   domain,
		AllowedOperations:     allOperations,
		PermittedSourceIDs:    sources,
		PermittedPolicyIDs:    policies,
		PolicyGeneration:      generation,
		AuthenticationExpires: now.Add(time.Hour),
		RequestID:             shoal.ID(subject + "-request"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return decision
}

// Reading a claim must never confer the ability to finalize it. The sealed
// finalization token is a capability held only by the acquiring caller, so an
// observer that can call SourceClaim must not be able to commit, pend, or roll
// back an in-flight claim it does not own.
func TestSourceClaimReadIsNotAFinalizationToken(t *testing.T) {
	ctx := context.Background()
	store := authorized.NewMemoryPolicyStore()
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
	const uri = "file:///observed.txt"

	// Establish a committed claim under ruleA, then begin a reclassification
	// to ruleB so an observer sees a held claim mid-transition.
	initial, err := store.CompareAndSwapSourceClaim(ctx, uri, nil, ruleA)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CommitSourceClaim(ctx, initial); err != nil {
		t.Fatal(err)
	}
	committed, ok, err := store.SourceClaim(ctx, uri)
	if err != nil || !ok {
		t.Fatalf("committed read ok=%v err=%v", ok, err)
	}
	inFlight, err := store.CompareAndSwapSourceClaim(
		ctx, uri, &committed, ruleB)
	if err != nil {
		t.Fatal(err)
	}

	observed, ok, err := store.SourceClaim(ctx, uri)
	if err != nil || !ok {
		t.Fatalf("in-flight read ok=%v err=%v", ok, err)
	}
	// The observable claim still reports the in-flight transition, but it
	// must not be usable as the holder's finalization token.
	if !observed.Pending || observed.PreviousRule == nil ||
		observed.PreviousRule.String() != ruleA.String() ||
		observed.Rule.String() != ruleB.String() {
		t.Fatalf("observed claim = %#v", observed)
	}
	for name, finish := range map[string]func(
		context.Context, authorized.SourcePolicyClaim,
	) error{
		"commit":   store.CommitSourceClaim,
		"pend":     store.PendSourceClaim,
		"rollback": store.RollbackSourceClaim,
	} {
		if err := finish(ctx, observed); !shoal.IsErrorCode(
			err, shoal.ErrorInvalidArgument,
		) {
			t.Fatalf("%s with observed claim = %v", name, err)
		}
	}

	// The rightful holder still owns the transition and can finalize it, and
	// the observer never displaced the claim.
	if err := store.CommitSourceClaim(ctx, inFlight); err != nil {
		t.Fatalf("holder commit = %v", err)
	}
	final, ok, err := store.SourceClaim(ctx, uri)
	if err != nil || !ok || final.Pending ||
		final.Rule.String() != ruleB.String() {
		t.Fatalf("final claim = %#v ok=%v err=%v", final, ok, err)
	}
}
