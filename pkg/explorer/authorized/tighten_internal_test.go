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

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// The store-level TightenRule tests (#570, PR4). They build the catalog a
// document labelled before labels were enforced left behind, directly through
// the PolicyStore API, and check exactly which registrations a tighten
// rewrites and which it must leave alone. Every test runs on the memory and
// the durable store.

const tightenURI = "file:///tighten/doc-1"

type tightenWorld struct {
	source      auth.Policy
	bare        AccessRule
	secret      AccessRule // bare + (source, secret)
	secretX     AccessRule // bare + (source, secret) + (source, x)
	onlyX       AccessRule // bare + (source, x)
	xyz         AccessRule // bare + (source, x), (source, y), (source, z)
	otherBare   AccessRule // another source's bare rule
	otherSecret AccessRule // otherBare + (source-b, secret)
}

func newTightenWorld(t *testing.T) tightenWorld {
	t.Helper()
	newPolicy := func(source, grant string) auth.Policy {
		policy, err := auth.NewPolicy(auth.PolicyConfig{
			AuthorizationDomain: []byte("domain"),
			SourceID:            []byte(source),
			GrantPolicyID:       []byte(grant),
			Epoch:               1,
		})
		if err != nil {
			t.Fatal(err)
		}
		return policy
	}
	mustRule := func(rule AccessRule, err error) AccessRule {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return rule
	}
	source := newPolicy("source-a", "policy-a")
	other := newPolicy("source-b", "policy-b")
	return tightenWorld{
		source:      source,
		bare:        mustRule(NewAccessRule(source)),
		secret:      mustRule(LabelRule(source, []string{"secret"})),
		secretX:     mustRule(LabelRule(source, []string{"secret", "x"})),
		onlyX:       mustRule(LabelRule(source, []string{"x"})),
		xyz:         mustRule(LabelRule(source, []string{"x", "y", "z"})),
		otherBare:   mustRule(NewAccessRule(other)),
		otherSecret: mustRule(LabelRule(other, []string{"secret"})),
	}
}

func withTightenStores(t *testing.T, run func(t *testing.T, store PolicyStore, reopen func() PolicyStore)) {
	t.Run("memory", func(t *testing.T) {
		store := NewMemoryPolicyStore()
		run(t, store, func() PolicyStore { return store })
	})
	t.Run("durable", func(t *testing.T) {
		dir := t.TempDir()
		store, err := OpenDurablePolicyStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		current := store
		t.Cleanup(func() { _ = current.Close() })
		run(t, store, func() PolicyStore {
			if err := current.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenDurablePolicyStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			current = reopened
			return reopened
		})
	})
}

// tightenFor calls TightenRule without a prebuilt index.
func tightenFor(
	store PolicyStore, documentID, revisionID shoal.ID, uri string, from, to AccessRule,
) (bool, error) {
	return store.TightenRule(context.Background(), nil, RuleTightening{
		DocumentID: documentID, RevisionID: revisionID, SourceURI: uri,
		From: from, To: to,
	})
}

func tightenDigest(name string) auth.Digest {
	return auth.DigestBytes("tighten-test", []byte(name))
}

// seedLegacyCatalog writes the pre-enforcement catalog of doc-1: two
// revisions, both under the bare rule, with extracted nodes and edges bound to
// each, a materialized node that names doc-1 as its owner (only its kind
// keeps it out of scope), application edges with and without an endpoint
// among doc-1's extracted nodes, another document's extracted
// registrations, and a committed source claim.
func seedLegacyCatalog(t *testing.T, store PolicyStore, w tightenWorld) {
	t.Helper()
	ctx := context.Background()
	for _, registration := range []RevisionRegistration{
		{
			DocumentID: "doc-1", RevisionID: "rev-1",
			NodeIDs: []shoal.ID{"doc-1", "sec-1"},
			IntrinsicEdges: []graph.Edge{{
				ID: "contains-1", From: "doc-1", To: "sec-1", Type: "contains", Weight: 1,
			}},
			ContentDigest: tightenDigest("rev-1"), Rule: w.bare, Current: true,
		},
		{
			DocumentID: "doc-1", RevisionID: "rev-2",
			NodeIDs: []shoal.ID{"doc-1", "sec-2"},
			IntrinsicEdges: []graph.Edge{{
				ID: "contains-2", From: "doc-1", To: "sec-2", Type: "contains", Weight: 1,
			}},
			ContentDigest: tightenDigest("rev-2"), Rule: w.bare, Current: true,
		},
		{
			DocumentID: "doc-2", RevisionID: "rev-9",
			NodeIDs:       []shoal.ID{"doc-2"},
			ContentDigest: tightenDigest("rev-9"), Rule: w.bare, Current: true,
		},
	} {
		if err := store.PutRevision(ctx, registration); err != nil {
			t.Fatal(err)
		}
	}
	for id, registration := range map[shoal.ID]NodeRegistration{
		"entity-1":     {DocumentID: "doc-1", RevisionID: "rev-1", Rule: w.bare, Kind: RegistrationExtracted},
		"entity-2":     {DocumentID: "doc-1", RevisionID: "rev-2", Rule: w.bare, Kind: RegistrationExtracted},
		"entity-other": {DocumentID: "doc-2", RevisionID: "rev-9", Rule: w.bare, Kind: RegistrationExtracted},
		"materialized": {DocumentID: "doc-1", RevisionID: "rev-2", Rule: w.bare, Kind: RegistrationMaterialized},
	} {
		if err := store.PutNode(ctx, id, registration); err != nil {
			t.Fatal(err)
		}
	}
	for _, registration := range []EdgeRegistration{
		{Edge: graph.Edge{ID: "rel-1", From: "entity-1", To: "entity-2", Type: "uses", Weight: 1},
			DocumentID: "doc-1", RevisionID: "rev-1", Rule: w.bare, Kind: RegistrationExtracted},
		// A relation between two entities owned elsewhere, asserted by doc-1:
		// its document binding is what tightens it.
		{Edge: graph.Edge{ID: "rel-shared", From: "entity-other", To: "elsewhere", Type: "uses", Weight: 1},
			DocumentID: "doc-1", RevisionID: "rev-2", Rule: w.bare, Kind: RegistrationExtracted},
		{Edge: graph.Edge{ID: "rel-other", From: "entity-other", To: "entity-other", Type: "uses", Weight: 1},
			DocumentID: "doc-2", RevisionID: "rev-9", Rule: w.bare, Kind: RegistrationExtracted},
		// A legacy (pre-kind) extracted relation decodes as application.
		{Edge: graph.Edge{ID: "legacy-touching", From: "entity-2", To: "elsewhere", Type: "uses", Weight: 1},
			Rule: w.bare, Kind: RegistrationApplication},
		{Edge: graph.Edge{ID: "legacy-elsewhere", From: "entity-other", To: "elsewhere", Type: "uses", Weight: 1},
			Rule: w.bare, Kind: RegistrationApplication},
		{Edge: graph.Edge{ID: "materialized-edge", From: "materialized", To: "materialized", Type: "m", Weight: 1},
			Rule: w.bare, Kind: RegistrationMaterialized},
	} {
		if err := store.PutEdge(ctx, registration); err != nil {
			t.Fatal(err)
		}
	}
	claim, err := store.CompareAndSwapSourceClaim(ctx, tightenURI, nil, w.bare)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CommitSourceClaim(ctx, claim); err != nil {
		t.Fatal(err)
	}
}

type tightenExpectation struct {
	revisions map[shoal.ID]AccessRule
	nodes     map[shoal.ID]AccessRule
	edges     map[shoal.ID]AccessRule
	claim     AccessRule
}

func checkTightened(t *testing.T, store PolicyStore, want tightenExpectation) {
	t.Helper()
	ctx := context.Background()
	for revision, rule := range want.revisions {
		registration, ok, err := store.Revision(ctx, "doc-1", revision)
		if err != nil || !ok {
			t.Fatalf("revision %s: %v %v", revision, ok, err)
		}
		if !registration.Rule.equal(rule) {
			t.Errorf("revision %s rule = %s, want %s", revision, registration.Rule, rule)
		}
	}
	for node, rule := range want.nodes {
		registration, ok, err := store.Node(ctx, node)
		if err != nil || !ok {
			t.Fatalf("node %s: %v %v", node, ok, err)
		}
		if !registration.Rule.equal(rule) {
			t.Errorf("node %s rule = %s, want %s", node, registration.Rule, rule)
		}
	}
	for edge, rule := range want.edges {
		registration, ok, err := store.Edge(ctx, edge)
		if err != nil || !ok {
			t.Fatalf("edge %s: %v %v", edge, ok, err)
		}
		if !registration.Rule.equal(rule) {
			t.Errorf("edge %s rule = %s, want %s", edge, registration.Rule, rule)
		}
	}
	claim, ok, err := store.SourceClaim(ctx, tightenURI)
	if err != nil || !ok {
		t.Fatalf("source claim: %v %v", ok, err)
	}
	if !claim.Rule.equal(want.claim) {
		t.Errorf("source claim rule = %s, want %s", claim.Rule, want.claim)
	}
}

// TestTightenRuleRewritesEveryRegistrationOfTheDocument tightens through the
// current revision and checks every registration, then that a second call is
// a no-op, and that a reopened durable store kept all of it.
func TestTightenRuleRewritesEveryRegistrationOfTheDocument(t *testing.T) {
	withTightenStores(t, func(t *testing.T, store PolicyStore, reopen func() PolicyStore) {
		w := newTightenWorld(t)
		seedLegacyCatalog(t, store, w)
		ctx := context.Background()
		before, _, err := store.SourceClaim(ctx, tightenURI)
		if err != nil {
			t.Fatal(err)
		}

		changed, err := tightenFor(store, "doc-1", "rev-2", tightenURI, w.bare, w.secret)
		if err != nil || !changed {
			t.Fatalf("TightenRule = %v, %v", changed, err)
		}
		want := tightenExpectation{
			revisions: map[shoal.ID]AccessRule{
				// The historical revision is tightened, not only the current.
				"rev-1": w.secret, "rev-2": w.secret,
			},
			nodes: map[shoal.ID]AccessRule{
				// The current projection.
				"doc-1": w.secret, "sec-2": w.secret,
				// Extracted nodes from every revision of the document.
				"entity-1": w.secret, "entity-2": w.secret,
				// Another document's extraction, and a materialized node
				// whose owner field happens to read doc-1.
				"entity-other": w.bare, "materialized": w.bare,
			},
			edges: map[shoal.ID]AccessRule{
				"contains-2":      w.secret,
				"rel-1":           w.secret,
				"rel-shared":      w.secret,
				"rel-other":       w.bare,
				"legacy-touching": w.secret,
				// Nothing ties this legacy relation to doc-1.
				"legacy-elsewhere":  w.bare,
				"materialized-edge": w.bare,
			},
			claim: w.secret,
		}
		checkTightened(t, store, want)
		after, _, err := store.SourceClaim(ctx, tightenURI)
		if err != nil {
			t.Fatal(err)
		}
		if after.Version <= before.Version {
			t.Errorf("claim version %d did not advance past %d", after.Version, before.Version)
		}
		current, _, err := store.CurrentRevision(ctx, "doc-1")
		if err != nil || current.RevisionID != "rev-2" || !current.Rule.equal(w.secret) {
			t.Fatalf("current revision = %+v, %v", current, err)
		}
		node, _, err := store.Node(ctx, "doc-1")
		if err != nil || node.Kind != RegistrationDocument || node.RevisionID != "rev-2" {
			t.Fatalf("document projection = %+v, %v", node, err)
		}

		// Idempotent: the same call again changes nothing.
		changed, err = tightenFor(store, "doc-1", "rev-2", tightenURI, w.bare, w.secret)
		if err != nil || changed {
			t.Fatalf("repeated TightenRule = %v, %v, want a no-op", changed, err)
		}
		// So is a sweep from the bare rule of an already tightened document.
		changed, err = tightenFor(store, "doc-1", "rev-2", tightenURI, w.bare, w.secret)
		if err != nil || changed {
			t.Fatalf("sweep = %v, %v, want a no-op", changed, err)
		}

		reopened := reopen()
		checkTightened(t, reopened, want)
		node, _, err = reopened.Node(ctx, "doc-1")
		if err != nil || !node.Rule.equal(w.secret) || node.Kind != RegistrationDocument {
			t.Fatalf("document projection after reopen = %+v, %v", node, err)
		}
	})
}

// TestTightenRuleHistoricalRevisionScope tightens a historical revision by
// its own labels: only that revision and what was extracted from it change.
func TestTightenRuleHistoricalRevisionScope(t *testing.T) {
	withTightenStores(t, func(t *testing.T, store PolicyStore, reopen func() PolicyStore) {
		w := newTightenWorld(t)
		seedLegacyCatalog(t, store, w)
		changed, err := tightenFor(store, "doc-1", "rev-1", "", w.bare, w.onlyX)
		if err != nil || !changed {
			t.Fatalf("TightenRule = %v, %v", changed, err)
		}
		want := tightenExpectation{
			revisions: map[shoal.ID]AccessRule{"rev-1": w.onlyX, "rev-2": w.bare},
			nodes: map[shoal.ID]AccessRule{
				"doc-1": w.bare, "entity-1": w.onlyX, "entity-2": w.bare,
			},
			edges: map[shoal.ID]AccessRule{
				"rel-1": w.onlyX, "rel-shared": w.bare, "legacy-touching": w.bare,
			},
			claim: w.bare,
		}
		checkTightened(t, store, want)
		checkTightened(t, reopen(), want)
	})
}

// TestTightenRuleRefusesWidening pins that TightenRule never widens: any `to`
// that is not a strict superset of `from` is refused, and a stale `from` that
// is not the stored rule conflicts. Nothing changes in either case.
func TestTightenRuleRefusesWidening(t *testing.T) {
	withTightenStores(t, func(t *testing.T, store PolicyStore, _ func() PolicyStore) {
		w := newTightenWorld(t)
		seedLegacyCatalog(t, store, w)
		if _, err := tightenFor(store, "doc-1", "rev-2", tightenURI, w.bare, w.secretX); err != nil {
			t.Fatal(err)
		}
		for name, c := range map[string]struct {
			from, to AccessRule
			code     shoal.ErrorCode
		}{
			"drops a label":     {w.secretX, w.secret, shoal.ErrorInvalidArgument},
			"drops every label": {w.secretX, w.bare, shoal.ErrorInvalidArgument},
			// Larger, but without secret: a superset by count only.
			"drops a label, adds two": {w.secretX, w.xyz, shoal.ErrorInvalidArgument},
			"no change":               {w.secretX, w.secretX, shoal.ErrorInvalidArgument},
			"swaps the source":        {w.secretX, w.otherBare, shoal.ErrorInvalidArgument},
			"stale from, no subset":   {w.otherBare, w.bare, shoal.ErrorInvalidArgument},
			// A strict superset of a rule the document does not have.
			"stale from": {w.otherBare, w.otherSecret, shoal.ErrorConflict},
		} {
			_, err := tightenFor(store, "doc-1", "rev-2", tightenURI, c.from, c.to)
			if !shoal.IsErrorCode(err, c.code) {
				t.Errorf("%s: err = %v, want %v", name, err, c.code)
			}
		}
		if _, err := tightenFor(store, "doc-1", "rev-404", tightenURI, w.bare, w.secret); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			t.Errorf("unknown revision: err = %v, want not found", err)
		}
		if _, err := tightenFor(store, "doc-2", "rev-9", "", w.bare, w.secret); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
			t.Errorf("current revision without a source URI: err = %v, want invalid argument", err)
		}
		checkTightened(t, store, tightenExpectation{
			revisions: map[shoal.ID]AccessRule{"rev-1": w.secretX, "rev-2": w.secretX},
			nodes:     map[shoal.ID]AccessRule{"doc-1": w.secretX, "entity-1": w.secretX},
			edges:     map[shoal.ID]AccessRule{"rel-1": w.secretX},
			claim:     w.secretX,
		})
	})
}

// TestTightenRulePendingClaim narrows the PreviousRule of a pending source
// claim, so a retry of the interrupted mutation must hold the labels too,
// and keeps its Rule, so the retry can still select it.
func TestTightenRulePendingClaim(t *testing.T) {
	withTightenStores(t, func(t *testing.T, store PolicyStore, reopen func() PolicyStore) {
		w := newTightenWorld(t)
		seedLegacyCatalog(t, store, w)
		ctx := context.Background()
		committed, _, err := store.SourceClaim(ctx, tightenURI)
		if err != nil {
			t.Fatal(err)
		}
		token, err := store.CompareAndSwapSourceClaim(ctx, tightenURI, &committed, w.bare)
		if err != nil {
			t.Fatal(err)
		}
		// Held by an in-flight mutation: refuse.
		if _, err := tightenFor(store, "doc-1", "rev-2", tightenURI, w.bare, w.secret); !shoal.IsErrorCode(err, shoal.ErrorConflict) {
			t.Fatalf("held claim: err = %v, want conflict", err)
		}
		if err := store.PendSourceClaim(ctx, token); err != nil {
			t.Fatal(err)
		}
		if _, err := tightenFor(store, "doc-1", "rev-2", tightenURI, w.bare, w.secret); err != nil {
			t.Fatal(err)
		}
		// Read the live store before reopening: a closed durable store
		// serves no reads.
		for index, s := range []PolicyStore{store, nil} {
			if index == 1 {
				s = reopen()
			}
			claim, ok, err := s.SourceClaim(ctx, tightenURI)
			if err != nil || !ok || !claim.Pending || claim.PreviousRule == nil {
				t.Fatalf("pending claim = %+v, %v, %v", claim, ok, err)
			}
			// The retry must still select exactly the in-flight Rule, so it
			// is kept; the labels go onto PreviousRule, which the retry must
			// also satisfy.
			if !claim.Rule.equal(w.bare) || !claim.PreviousRule.equal(w.secret) {
				t.Errorf("pending claim rules = %s / %s, want %s / %s",
					claim.Rule, claim.PreviousRule, w.bare, w.secret)
			}
		}
	})
}

// TestLabelMigrationMarkerPersists pins the marker round trip.
func TestLabelMigrationMarkerPersists(t *testing.T) {
	withTightenStores(t, func(t *testing.T, store PolicyStore, reopen func() PolicyStore) {
		ctx := context.Background()
		if _, ok, err := store.LabelMigration(ctx); err != nil || ok {
			t.Fatalf("fresh marker = %v, %v", ok, err)
		}
		want := LabelMigrationRecord{
			Version: LabelMigrationVersion, Documents: 3, Tightened: 1,
			Untranslatable: []UntranslatableLabel{{
				DocumentID: "doc-1", RevisionID: "rev-1", SourceURI: tightenURI,
				EscapedLabel: `"bad label!"`, Reason: "does not parse",
			}},
		}
		if err := store.PutLabelMigration(ctx, want); err != nil {
			t.Fatal(err)
		}
		got, ok, err := reopen().LabelMigration(ctx)
		if err != nil || !ok || got.Version != want.Version || got.Documents != 3 ||
			got.Tightened != 1 || len(got.Untranslatable) != 1 ||
			got.Untranslatable[0] != want.Untranslatable[0] {
			t.Fatalf("marker after reopen = %+v, %v, %v", got, ok, err)
		}
	})
}
