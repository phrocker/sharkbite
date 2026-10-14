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
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/lexicon"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// lexiconWorld materializes graph nodes under policy A (visible to alice) and
// policy B (hidden from alice) through the real authorized client, then reads
// them back through a counting wrapper around the same policy store.
type lexiconWorld struct {
	f        *fixture
	store    authorized.PolicyStore
	counting *countingPolicyStore
	client   *authorized.Client
	ids      map[string]shoal.ID
	nodes    []graph.Node
	snapshot lexicon.Snapshot
}

type lexiconSpec struct {
	key        string
	name       string
	properties shoal.Metadata
}

var (
	visibleLexiconSpecs = []lexiconSpec{
		{key: "payments", name: "Payments"},
		{key: "ledger", name: "Ledger Service",
			properties: shoal.Metadata{"shoal.lexicon.alias.0": "PayGW"}},
		{key: "core", name: "Core Platform"},
	}
	hiddenLexiconSpecs = []lexiconSpec{
		{key: "payments-api", name: "Payments API"},
		{key: "router", name: "Secret Router",
			properties: shoal.Metadata{"shoal.lexicon.alias.0": "PayGW"}},
		{key: "secret", name: "Secret Project",
			properties: shoal.Metadata{"shoal.lexicon.alias.0": "Project Nightjar"}},
	}
)

// ghostNode is in the bundle but was never registered: the catalog does not
// know it, which is the state a node deleted after the snapshot is in.
var ghostNode = graph.Node{
	ID: "ghost-node", Kind: "service",
	Properties: shoal.Metadata{"name": "Phantom Widget"},
}

func newLexiconWorld(t testing.TB, store authorized.PolicyStore) *lexiconWorld {
	t.Helper()
	f := newFixture(t)
	w := &lexiconWorld{f: f, store: store, ids: make(map[string]shoal.ID)}
	writerA := f.newClient(t, f.base, store, f.sourceA, f.policyA, nil)
	writerB := f.newClient(t, f.base, store, f.sourceB, f.policyB, nil)
	w.materialize(t, writerA, "visible", f.sourceA, f.policyA, visibleLexiconSpecs)
	w.materialize(t, writerB, "hidden", f.sourceB, f.policyB, hiddenLexiconSpecs)
	w.nodes = append(w.nodes, ghostNode)
	snapshot, err := f.base.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	w.snapshot = lexicon.Snapshot{
		ID: snapshot.ID, AsOf: snapshot.AsOf, Frontier: snapshot.Frontier,
	}
	w.counting = newCountingPolicyStore(store)
	w.client = f.newClient(t, f.base, w.counting, f.sourceA, f.policyA, nil)
	return w
}

func (w *lexiconWorld) materialize(
	t testing.TB,
	client *authorized.Client,
	namespace string,
	source, policy []byte,
	specs []lexiconSpec,
) {
	t.Helper()
	snapshot, err := w.f.base.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	request := explorer.GraphMaterializationRequest{
		Namespace: []byte(namespace), MutationID: shoal.ID("lexicon-" + namespace),
		SourceID: source, PolicyID: policy, ExpectedSnapshot: snapshot,
	}
	for _, spec := range specs {
		properties := shoal.Metadata{"name": spec.name}
		for key, value := range spec.properties {
			properties[key] = value
		}
		request.Nodes = append(request.Nodes, explorer.GraphNodeSpec{
			Key: []byte(spec.key), Kind: "service", Properties: properties,
		})
	}
	result, err := client.MaterializeGraph(w.f.admin(t), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.GraphNodes) != len(specs) {
		t.Fatalf("materialized %d nodes, want %d", len(result.GraphNodes), len(specs))
	}
	for _, node := range result.GraphNodes {
		w.nodes = append(w.nodes, node)
	}
	for _, identity := range result.Nodes {
		w.ids[string(identity.Key)] = identity.ID
	}
}

func (w *lexiconWorld) bundle(t testing.TB) *lexicon.Bundle {
	t.Helper()
	bundle, err := lexicon.Build(lexicon.Input{
		Snapshot: w.snapshot, Nodes: w.nodes,
	}, lexicon.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

type resolveOutcome struct {
	mentions []lexicon.Mention
	err      error
	trips    policyStoreTrips
}

func (w *lexiconWorld) resolve(
	ctx context.Context,
	bundle *lexicon.Bundle,
	text string,
) resolveOutcome {
	w.counting.reset()
	mentions, err := w.client.ResolveMentions(ctx, bundle, text)
	return resolveOutcome{mentions: mentions, err: err, trips: w.counting.snapshot()}
}

func withPolicyStores(t *testing.T, run func(*testing.T, authorized.PolicyStore)) {
	t.Run("memory", func(t *testing.T) {
		run(t, authorized.NewMemoryPolicyStore())
	})
	t.Run("durable", func(t *testing.T) {
		durable, err := authorized.OpenDurablePolicyStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = durable.Close() })
		run(t, durable)
	})
}

func TestResolveMentionsHiddenIsIndistinguishableFromUnknown(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		w := newLexiconWorld(t, store)
		bundle := w.bundle(t)
		alice := w.f.alice(t)

		// "Secret Project" names a hidden node; "Zebras Quietly" names
		// nothing. Both are two tokens of the same byte length.
		hidden := w.resolve(alice, bundle, "is Secret Project near Payments today")
		unknown := w.resolve(alice, bundle, "is Zebras Quietly near Payments today")
		if !reflect.DeepEqual(hidden, unknown) {
			t.Fatalf("hidden and unknown differ:\nhidden  %#v\nunknown %#v", hidden, unknown)
		}
		if hidden.err != nil || len(hidden.mentions) != 1 ||
			!reflect.DeepEqual(hidden.mentions[0].NodeIDs, []shoal.ID{w.ids["payments"]}) {
			t.Fatalf("visible mention lost: %#v", hidden)
		}
		if hidden.trips.nodes != 1 || hidden.trips.perItem() != 0 ||
			hidden.trips.largestNodeBatch != authorized.MaxCandidateIDs ||
			hidden.trips.batchedNodeIDs != authorized.MaxCandidateIDs {
			t.Fatalf("store traffic = %#v", hidden.trips)
		}

		// Nothing matched at all, and a hidden-only text, look the same too.
		onlyHidden := w.resolve(alice, bundle, "Project Nightjar")
		nothing := w.resolve(alice, bundle, "Quietly Shuffled")
		if !reflect.DeepEqual(onlyHidden, nothing) || onlyHidden.mentions != nil {
			t.Fatalf("hidden-only %#v vs nothing %#v", onlyHidden, nothing)
		}

		// The admin, who may see the node, does get it: the bundle holds it.
		admin := w.resolve(w.f.admin(t), bundle, "is Secret Project near Payments today")
		if admin.err != nil || len(admin.mentions) != 2 ||
			admin.mentions[0].NodeIDs[0] != w.ids["secret"] {
			t.Fatalf("admin = %#v", admin)
		}
		if !reflect.DeepEqual(admin.trips, hidden.trips) {
			t.Fatalf("store traffic depends on visibility: %#v vs %#v", admin.trips, hidden.trips)
		}

		// Over-bound input fails identically, before the store is read.
		long := strings.Repeat("word ", authorized.MaxMentionTokens)
		overHidden := w.resolve(alice, bundle, long+"Secret Project")
		overUnknown := w.resolve(alice, bundle, long+"Zebras Quietly")
		if !reflect.DeepEqual(overHidden, overUnknown) ||
			!shoal.IsErrorCode(overHidden.err, shoal.ErrorInvalidArgument) ||
			overHidden.trips.total() != 0 {
			t.Fatalf("over-bound hidden %#v vs unknown %#v", overHidden, overUnknown)
		}
		bytesHidden := w.resolve(alice, bundle,
			strings.Repeat("x", authorized.MaxMentionBytes)+"Secret Project")
		bytesUnknown := w.resolve(alice, bundle,
			strings.Repeat("x", authorized.MaxMentionBytes)+"Zebras Quietly")
		if !reflect.DeepEqual(bytesHidden, bytesUnknown) ||
			!reflect.DeepEqual(bytesHidden.err, overHidden.err) {
			t.Fatalf("byte-bound hidden %#v vs unknown %#v", bytesHidden, bytesUnknown)
		}
	})
}

func TestResolveMentionsSharedAliasAndOverlap(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		w := newLexiconWorld(t, store)
		bundle := w.bundle(t)
		alice := w.f.alice(t)

		// One visible and one hidden node share "PayGW": alice gets the
		// visible one, unambiguously.
		shared := w.resolve(alice, bundle, "who runs PayGW")
		want := []lexicon.Mention{{
			TokenSpan: lexicon.Span{Start: 2, End: 3},
			ByteSpan:  lexicon.Span{Start: 9, End: 14},
			NodeIDs:   []shoal.ID{w.ids["ledger"]}, Kinds: []string{"service"},
		}}
		if shared.err != nil || !reflect.DeepEqual(shared.mentions, want) {
			t.Fatalf("shared alias = %#v", shared)
		}
		admin := w.resolve(w.f.admin(t), bundle, "who runs PayGW")
		if admin.err != nil || len(admin.mentions) != 1 || !admin.mentions[0].Ambiguous ||
			len(admin.mentions[0].NodeIDs) != 2 {
			t.Fatalf("admin shared alias = %#v", admin)
		}

		// A hidden longer name cannot hide the visible shorter one.
		overlap := w.resolve(alice, bundle, "Payments API")
		want = []lexicon.Mention{{
			TokenSpan: lexicon.Span{Start: 0, End: 1},
			ByteSpan:  lexicon.Span{Start: 0, End: 8},
			NodeIDs:   []shoal.ID{w.ids["payments"]}, Kinds: []string{"service"},
		}}
		if overlap.err != nil || !reflect.DeepEqual(overlap.mentions, want) {
			t.Fatalf("overlap = %#v", overlap)
		}
		adminOverlap := w.resolve(w.f.admin(t), bundle, "Payments API")
		if len(adminOverlap.mentions) != 1 ||
			adminOverlap.mentions[0].NodeIDs[0] != w.ids["payments-api"] {
			t.Fatalf("admin overlap = %#v", adminOverlap)
		}
	})
}

// ruleSwapStore reports one node under another node's rule, as a catalog
// would after that node's policy changed following the bundle's snapshot.
type ruleSwapStore struct {
	authorized.PolicyStore
	target shoal.ID
	rule   authorized.AccessRule
}

func (s ruleSwapStore) Nodes(
	ctx context.Context,
	ids []shoal.ID,
) (map[shoal.ID]authorized.NodeRegistration, error) {
	resolved, err := s.PolicyStore.Nodes(ctx, ids)
	if registration, ok := resolved[s.target]; ok {
		registration.Rule = s.rule
		resolved[s.target] = registration
	}
	return resolved, err
}

func TestResolveMentionsUsesCurrentPolicyNotTheSnapshot(t *testing.T) {
	w := newLexiconWorld(t, authorized.NewMemoryPolicyStore())
	bundle := w.bundle(t)
	alice := w.f.alice(t)
	before := w.resolve(alice, bundle, "Core Platform")
	if before.err != nil || len(before.mentions) != 1 {
		t.Fatalf("before = %#v", before)
	}
	hiddenRegistration, ok, err := w.store.Node(context.Background(), w.ids["secret"])
	if err != nil || !ok {
		t.Fatalf("hidden registration: ok=%v err=%v", ok, err)
	}
	swapped := w.f.newClient(t, w.f.base, ruleSwapStore{
		PolicyStore: w.store, target: w.ids["core"], rule: hiddenRegistration.Rule,
	}, w.f.sourceA, w.f.policyA, nil)
	mentions, err := swapped.ResolveMentions(alice, bundle, "Core Platform")
	if err != nil || mentions != nil {
		t.Fatalf("node with a changed rule still matched: %#v %v", mentions, err)
	}

	// A node the catalog does not know (deleted since the snapshot) matches
	// as nothing for everyone, admin included.
	for _, ctx := range []context.Context{alice, w.f.admin(t)} {
		ghost := w.resolve(ctx, bundle, "Phantom Widget")
		if ghost.err != nil || ghost.mentions != nil {
			t.Fatalf("unregistered node matched: %#v", ghost)
		}
	}

	// Revoking alice's grant to policy A drops what she saw before.
	revoked := w.f.context(t, w.f.decision(t, "alice",
		[][]byte{w.f.sourceA}, [][]byte{[]byte("policy-z")}, allOperations))
	after := w.resolve(revoked, bundle, "Core Platform")
	if after.err != nil || after.mentions != nil {
		t.Fatalf("revoked grant still matched: %#v", after)
	}
}

func TestResolveMentionsRequiresNeighborhood(t *testing.T) {
	w := newLexiconWorld(t, authorized.NewMemoryPolicyStore())
	reader := w.f.context(t, w.f.decision(t, "reader",
		[][]byte{w.f.sourceA}, [][]byte{w.f.policyA},
		[]auth.Operation{auth.OperationRead}))
	got := w.resolve(reader, w.bundle(t), "Payments")
	if !shoal.IsErrorCode(got.err, shoal.ErrorUnauthorized) || got.trips.total() != 0 {
		t.Fatalf("without neighborhood = %#v", got)
	}
	if _, err := w.client.ResolveMentions(w.f.alice(t), nil, "x"); err == nil {
		t.Fatal("nil bundle accepted")
	}
}

func TestResolveMentionsRejectsUnboundableBundle(t *testing.T) {
	w := newLexiconWorld(t, authorized.NewMemoryPolicyStore())
	var nodes []graph.Node
	for index := 0; index < lexicon.HardMaxPostingsPerTerm; index++ {
		nodes = append(nodes, graph.Node{
			ID: shoal.ID(fmt.Sprintf("shared-%02d", index)),
			Properties: shoal.Metadata{
				"name":                  fmt.Sprintf("n%02d", index),
				"shoal.lexicon.alias.0": "everyone",
			},
		})
	}
	nodes = append(nodes, graph.Node{ID: "long", Properties: shoal.Metadata{
		"shoal.lexicon.alias.0": "a b c d e f g h i j k l m n o p",
	}})
	wide, err := lexicon.Build(lexicon.Input{
		Snapshot: w.snapshot, Nodes: nodes,
	}, lexicon.Limits{
		MaxTermTokens:      lexicon.HardMaxTermTokens,
		MaxPostingsPerTerm: lexicon.HardMaxPostingsPerTerm,
	})
	if err != nil {
		t.Fatal(err)
	}
	if wide.CandidateBound(authorized.MaxMentionTokens) <= authorized.MaxCandidateIDs {
		t.Fatal("fixture does not exceed the candidate bound")
	}
	got := w.resolve(w.f.alice(t), wide, "anything")
	if !shoal.IsErrorCode(got.err, shoal.ErrorInvalidArgument) || got.trips.total() != 0 {
		t.Fatalf("unboundable bundle = %#v", got)
	}
}

func (w *lexiconWorld) scoped(
	t *testing.T,
	ctx context.Context,
) lexicon.ScopedNodes {
	t.Helper()
	scoped, err := w.client.LexiconScopeNodes(ctx, w.snapshot, w.nodes)
	if err != nil {
		t.Fatal(err)
	}
	return scoped
}

func pinnedBytes(t *testing.T, scoped lexicon.ScopedNodes) (*lexicon.Bundle, []byte) {
	t.Helper()
	bundle, err := lexicon.Build(lexicon.Input{Scoped: &scoped}, lexicon.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	shippable, ok := bundle.ForShipping()
	if !ok {
		t.Fatal("pinned bundle is not shippable")
	}
	return bundle, shippable.Bytes()
}

func TestLexiconScopeBuildHoldsNoHiddenBytes(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		w := newLexiconWorld(t, store)
		aliceScope := w.scoped(t, w.f.alice(t))
		if aliceScope.Len() != len(visibleLexiconSpecs) {
			t.Fatalf("scoped nodes = %d, want %d", aliceScope.Len(), len(visibleLexiconSpecs))
		}
		adminScope := w.scoped(t, w.f.admin(t))
		if adminScope.Len() != len(visibleLexiconSpecs)+len(hiddenLexiconSpecs) {
			t.Fatalf("admin scoped nodes = %d", adminScope.Len())
		}
		aliceBundle, aliceBytes := pinnedBytes(t, aliceScope)
		adminBundle, adminBytes := pinnedBytes(t, adminScope)

		hiddenMarkers := [][]byte{
			[]byte("secret"), []byte("nightjar"), []byte("router"),
			[]byte(w.ids["secret"]), []byte(w.ids["router"]),
			[]byte(w.ids["payments-api"]),
		}
		for _, marker := range hiddenMarkers {
			if bytes.Contains(aliceBytes, marker) {
				t.Fatalf("alice's bundle holds hidden bytes %q", marker)
			}
			// The check has teeth: the admin's scope holds them.
			if !bytes.Contains(adminBytes, marker) {
				t.Fatalf("admin bundle lacks %q", marker)
			}
		}
		// The unregistered node is in nobody's scope.
		for _, data := range [][]byte{aliceBytes, adminBytes} {
			if bytes.Contains(data, []byte(ghostNode.ID)) || bytes.Contains(data, []byte("phantom")) {
				t.Fatal("unregistered node reached a scoped bundle")
			}
		}

		// The scope is computed per caller, not claimed.
		if aliceBundle.Scope() == adminBundle.Scope() ||
			aliceScope.Scope() != aliceBundle.Scope() {
			t.Fatalf("scopes alice %v admin %v", aliceBundle.Scope(), adminBundle.Scope())
		}
		again := w.scoped(t, w.f.alice(t))
		if again.Scope() != aliceScope.Scope() {
			t.Fatal("one caller's scope is not stable")
		}
		otherSnapshot := w.snapshot
		otherSnapshot.Frontier++
		moved, err := w.client.LexiconScopeNodes(w.f.alice(t), otherSnapshot, w.nodes)
		if err != nil || moved.Scope() == aliceScope.Scope() {
			t.Fatalf("snapshot is not part of the scope: %v", err)
		}
		w.f.reader.Set(w.f.domain, 2)
		later, err := w.client.LexiconScopeNodes(w.f.context(t, w.f.decisionAtGeneration(
			t, "alice", [][]byte{w.f.sourceA}, [][]byte{w.f.policyA}, allOperations, 2,
		)), w.snapshot, w.nodes)
		if err != nil || later.Scope() == aliceScope.Scope() {
			t.Fatalf("policy generation is not part of the scope: %v", err)
		}
		w.f.reader.Set(w.f.domain, 1)

		// The scope round-trips through Load.
		loaded, err := lexicon.Load(aliceBytes)
		if err != nil || loaded.Scope() != aliceBundle.Scope() || loaded.ID() != aliceBundle.ID() {
			t.Fatalf("scope lost on load: %v", err)
		}
		// An unfiltered pinned build is impossible: scoped nodes cannot be
		// widened, and a hand-made ScopedNodes is refused.
		if _, err := lexicon.Build(lexicon.Input{
			Scoped: &aliceScope, Nodes: w.nodes,
		}, lexicon.Limits{}); err == nil {
			t.Fatal("scoped nodes widened with unfiltered nodes")
		}
		if _, err := lexicon.Build(lexicon.Input{
			Snapshot: w.snapshot, Scoped: &lexicon.ScopedNodes{},
		}, lexicon.Limits{}); err == nil {
			t.Fatal("hand-made scoped nodes accepted")
		}
		// Loaded bytes never ship, so copying alice's scope label onto the
		// admin's bytes yields a bundle that claims alice's scope but cannot
		// be shipped as hers.
		aliceShippable, _ := aliceBundle.ForShipping()
		adminShippable, _ := adminBundle.ForShipping()
		aliceDigest := aliceShippable.Scope().Digest()
		adminDigest := adminShippable.Scope().Digest()
		forged := bytes.Replace(adminBytes, adminDigest[:], aliceDigest[:], 1)
		if bytes.Equal(forged, adminBytes) {
			t.Fatal("admin digest not found in its bytes")
		}
		relabeled, err := lexicon.Load(forged)
		if err != nil {
			t.Fatal(err)
		}
		if relabeled.Scope() != aliceBundle.Scope() {
			t.Fatal("relabeled bundle does not claim alice's scope")
		}
		if _, ok := relabeled.ForShipping(); ok {
			t.Fatal("relabeled bundle is shippable")
		}
		if _, ok := loaded.ForShipping(); ok {
			t.Fatal("alice's reloaded bundle is shippable")
		}
		// Alice's own bundle still resolves her names.
		if got := w.resolve(w.f.alice(t), loaded, "PayGW"); got.err != nil ||
			len(got.mentions) != 1 || got.mentions[0].Ambiguous {
			t.Fatalf("scoped bundle resolve = %#v", got)
		}
	})
}

// neighborhoodOnly may use the neighborhood operation and nothing else.
func (w *lexiconWorld) neighborhoodOnly(t *testing.T) context.Context {
	t.Helper()
	return w.f.context(t, w.f.decision(t, "walker",
		[][]byte{w.f.sourceA}, [][]byte{w.f.policyA},
		[]auth.Operation{auth.OperationNeighborhood}))
}

func TestLexiconNeedsOnlyNeighborhood(t *testing.T) {
	w := newLexiconWorld(t, authorized.NewMemoryPolicyStore())
	ctx := w.neighborhoodOnly(t)
	got := w.resolve(ctx, w.bundle(t), "Payments and Core Platform")
	if got.err != nil || len(got.mentions) != 2 {
		t.Fatalf("neighborhood-only resolve = %#v", got)
	}
	scoped, err := w.client.LexiconScopeNodes(ctx, w.snapshot, w.nodes)
	if err != nil || scoped.Len() != len(visibleLexiconSpecs) {
		t.Fatalf("neighborhood-only scope = %d nodes, %v", scoped.Len(), err)
	}
}

// generationBumpStore changes the policy generation while a lexicon call is
// reading the catalog, after the call's first generation check.
type generationBumpStore struct {
	authorized.PolicyStore
	bump func()
}

func (s generationBumpStore) Nodes(
	ctx context.Context,
	ids []shoal.ID,
) (map[shoal.ID]authorized.NodeRegistration, error) {
	resolved, err := s.PolicyStore.Nodes(ctx, ids)
	s.bump()
	return resolved, err
}

func TestLexiconCallsCatchAGenerationChangeMidCall(t *testing.T) {
	w := newLexiconWorld(t, authorized.NewMemoryPolicyStore())
	bundle := w.bundle(t)
	client := w.f.newClient(t, w.f.base, generationBumpStore{
		PolicyStore: w.store,
		bump:        func() { w.f.reader.Set(w.f.domain, 2) },
	}, w.f.sourceA, w.f.policyA, nil)

	mentions, err := client.ResolveMentions(w.f.alice(t), bundle, "Payments")
	if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) || mentions != nil {
		t.Fatalf("ResolveMentions across a generation change = %#v, %v", mentions, err)
	}
	w.f.reader.Set(w.f.domain, 1)
	scoped, err := client.LexiconScopeNodes(w.f.alice(t), w.snapshot, w.nodes)
	if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) || scoped.Len() != 0 {
		t.Fatalf("LexiconScopeNodes across a generation change = %d, %v", scoped.Len(), err)
	}
}

func TestLexiconDropsDocumentNodesTheBaseNoLongerHolds(t *testing.T) {
	w := newLexiconWorld(t, authorized.NewMemoryPolicyStore())
	source := explorer.Source{
		URI: "file:///quarterly.txt", Title: "Quarterly",
		MediaType: explorer.MediaTypeText, Content: "quarterly report",
	}
	clientA := w.f.newClient(t, w.f.base, w.store, w.f.sourceA, w.f.policyA, nil)
	registered, err := clientA.Ingest(w.f.admin(t), source)
	if err != nil {
		t.Fatal(err)
	}
	document := graph.Node{
		ID: registered.Document.ID, Kind: "document",
		Properties: shoal.Metadata{"name": "Quarterly Report"},
	}
	w.nodes = append(w.nodes, document)
	bundle := w.bundle(t)
	alice := w.f.alice(t)
	if got := w.resolve(alice, bundle, "Quarterly Report"); got.err != nil ||
		len(got.mentions) != 1 || got.mentions[0].NodeIDs[0] != document.ID {
		t.Fatalf("current document = %#v", got)
	}
	if scoped := w.scoped(t, alice); scoped.Len() != len(visibleLexiconSpecs)+1 {
		t.Fatalf("current document not in scope: %d", scoped.Len())
	}

	// The base moves to an uncataloged revision: Neighborhood no longer finds
	// the node, and neither does the lexicon.
	if _, err := w.f.base.Ingest(context.Background(), explorer.Source{
		URI: source.URI, Title: "Uncataloged", MediaType: source.MediaType,
		Content: "uncataloged replacement",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := clientA.Neighborhood(alice, explorer.NeighborhoodRequest{
		NodeIDs: []shoal.ID{document.ID},
	}); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("diverged neighborhood error = %v", err)
	}
	got := w.resolve(alice, bundle, "Quarterly Report and Payments")
	if got.err != nil || len(got.mentions) != 1 || got.mentions[0].NodeIDs[0] != w.ids["payments"] {
		t.Fatalf("diverged document still matched: %#v", got)
	}
	if scoped := w.scoped(t, alice); scoped.Len() != len(visibleLexiconSpecs) {
		t.Fatalf("diverged document still in scope: %d", scoped.Len())
	}
}

func BenchmarkResolveMentions(b *testing.B) {
	w := newLexiconWorld(b, authorized.NewMemoryPolicyStore())
	bundle := w.bundle(b)
	alice := w.f.alice(b)
	// The counting wrapper is bypassed so only the client and store are timed.
	client := w.f.newClient(b, w.f.base, w.store, w.f.sourceA, w.f.policyA, nil)
	text := "does Payments call PayGW or the Secret Project owned by Core Platform"
	if mentions, err := client.ResolveMentions(alice, bundle, text); err != nil ||
		len(mentions) != 3 {
		b.Fatalf("mentions = %#v err = %v", mentions, err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := client.ResolveMentions(alice, bundle, text); err != nil {
			b.Fatal(err)
		}
	}
}
