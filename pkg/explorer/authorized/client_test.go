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
	"errors"
	"math"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/model"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

var allOperations = []auth.Operation{
	auth.OperationIngest,
	auth.OperationList,
	auth.OperationRead,
	auth.OperationConnect,
	auth.OperationGraphMaterialize,
	auth.OperationNeighborhood,
	auth.OperationRetrieve,
}

func TestGraphMaterializationAuthorizationAndUnauthorizedAbsence(t *testing.T) {
	f := newFixture(t)
	snapshot, err := f.base.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	request := explorer.GraphMaterializationRequest{
		Namespace: []byte{0, 'd'}, MutationID: "graph-mutation",
		SourceID: f.sourceA, PolicyID: f.policyA,
		ExpectedSnapshot: snapshot,
		Nodes: []explorer.GraphNodeSpec{{
			Key: []byte{0xff, 't'}, Kind: "team",
			Properties: shoal.Metadata{"name": "Demo"},
		}},
	}
	denied := f.context(t, f.decision(
		t, "reader", [][]byte{f.sourceA}, [][]byte{f.policyA},
		[]auth.Operation{auth.OperationRead},
	))
	if _, err := f.clientA.MaterializeGraph(denied, request); !shoal.IsErrorCode(
		err, shoal.ErrorUnauthorized,
	) {
		t.Fatalf("unauthorized materialization error = %v", err)
	}
	allowed := f.context(t, f.decision(
		t, "writer", [][]byte{f.sourceA}, [][]byte{f.policyA},
		[]auth.Operation{auth.OperationGraphMaterialize, auth.OperationNeighborhood},
	))
	result, err := f.clientA.MaterializeGraph(allowed, request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Disposition != explorer.IngestApplied {
		t.Fatalf("disposition = %q", result.Disposition)
	}
	neighborhood, err := f.clientA.Neighborhood(
		allowed, explorer.NeighborhoodRequest{
			NodeIDs: []shoal.ID{result.Nodes[0].ID}, Depth: 0,
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(neighborhood.Nodes) != 1 ||
		neighborhood.Nodes[0].Kind != "team" {
		t.Fatalf("neighborhood = %#v", neighborhood)
	}
}

type fakeClock struct {
	mu  sync.RWMutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.now
}

func (c *fakeClock) Set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

type generationReader struct {
	mu          sync.RWMutex
	generations map[string]int64
	err         error
}

func (r *generationReader) CurrentPolicyGeneration(
	ctx context.Context,
	domain []byte,
) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.err != nil {
		return 0, r.err
	}
	return r.generations[string(domain)], nil
}

func (r *generationReader) Set(domain []byte, generation int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.generations[string(domain)] = generation
}

func (r *generationReader) SetError(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

type fixture struct {
	dataDir   string
	base      *explorer.Explorer
	store     *authorized.MemoryPolicyStore
	clock     *fakeClock
	reader    *generationReader
	authority *auth.Authority
	clientA   *authorized.Client
	clientB   *authorized.Client
	domain    []byte
	sourceA   []byte
	policyA   []byte
	sourceB   []byte
	policyB   []byte
}

func newFixture(t testing.TB) *fixture {
	t.Helper()
	now := time.Date(2026, time.August, 26, 18, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: now}
	authority, err := auth.NewAuthorityWithClock(clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	base, err := explorer.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = base.Close() })
	f := &fixture{
		dataDir:   dataDir,
		base:      base,
		store:     authorized.NewMemoryPolicyStore(),
		clock:     clock,
		reader:    &generationReader{generations: make(map[string]int64)},
		authority: authority,
		domain:    []byte("domain-one"),
		sourceA:   []byte("source-a"),
		policyA:   []byte("policy-a"),
		sourceB:   []byte("source-b"),
		policyB:   []byte("policy-b"),
	}
	f.reader.Set(f.domain, 1)
	f.clientA = f.newClient(t, f.base, f.store, f.sourceA, f.policyA, nil)
	f.clientB = f.newClient(t, f.base, f.store, f.sourceB, f.policyB, nil)
	return f
}

func (f *fixture) newClient(
	t testing.TB,
	base explorer.Client,
	store authorized.PolicyStore,
	source, policy []byte,
	edgeSelector authorized.EdgePolicySelector,
) *authorized.Client {
	t.Helper()
	selector, err := authorized.NewStaticPolicySelector(source, policy)
	if err != nil {
		t.Fatal(err)
	}
	client, err := authorized.NewClient(authorized.Config{
		Base:                   base,
		VectorScorer:           trustedVectorScorer(base),
		InteractionWriter:      trustedInteractionWriter(base),
		InteractionReader:      trustedInteractionReader(base),
		SnapshotValidator:      trustedSnapshotValidator(base),
		DerivedAssertionReader: trustedDerivedAssertionReader(base),
		FoldStore:              trustedFoldStore(base),
		Resolver:               f.authority.Resolver(),
		PolicySelector:         selector,
		EdgePolicySelector:     edgeSelector,
		PolicyStore:            store,
		GenerationReader:       f.reader,
		Clock:                  f.clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func trustedVectorScorer(base explorer.Client) authorized.VectorScorer {
	scorer, _ := base.(authorized.VectorScorer)
	return scorer
}

func trustedInteractionReader(base explorer.Client) explorer.InteractionReader {
	reader, _ := base.(explorer.InteractionReader)
	return reader
}

func trustedInteractionWriter(base explorer.Client) explorer.InteractionWriter {
	writer, _ := base.(explorer.InteractionWriter)
	return writer
}

func trustedSnapshotValidator(base explorer.Client) authorized.SnapshotValidator {
	validator, _ := base.(authorized.SnapshotValidator)
	return validator
}

func trustedDerivedAssertionReader(
	base explorer.Client,
) authorized.DerivedAssertionReader {
	reader, _ := base.(authorized.DerivedAssertionReader)
	return reader
}

func trustedFoldStore(base explorer.Client) authorized.FoldStore {
	store, _ := base.(authorized.FoldStore)
	return store
}

func (f *fixture) decision(
	t testing.TB,
	subject string,
	sources, policies [][]byte,
	operations []auth.Operation,
) auth.Decision {
	return f.decisionAtGeneration(
		t, subject, sources, policies, operations, 1)
}

func (f *fixture) decisionAtGeneration(
	t testing.TB,
	subject string,
	sources, policies [][]byte,
	operations []auth.Operation,
	generation int64,
) auth.Decision {
	t.Helper()
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject:               shoal.ID(subject),
		Actor:                 shoal.ID(subject + "-actor"),
		AuthorizationDomain:   f.domain,
		AllowedOperations:     operations,
		PermittedSourceIDs:    sources,
		PermittedPolicyIDs:    policies,
		PolicyGeneration:      generation,
		AuthenticationExpires: f.clock.Now().Add(time.Hour),
		RequestID:             shoal.ID(subject + "-request"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return decision
}

func (f *fixture) context(
	t testing.TB,
	decision auth.Decision,
) context.Context {
	t.Helper()
	ctx, err := f.authority.Binder().Bind(context.Background(), decision)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

func (f *fixture) admin(t testing.TB) context.Context {
	t.Helper()
	return f.context(t, f.decision(
		t,
		"admin",
		[][]byte{f.sourceA, f.sourceB},
		[][]byte{f.policyA, f.policyB},
		allOperations,
	))
}

func (f *fixture) alice(t testing.TB) context.Context {
	t.Helper()
	return f.context(t, f.decision(
		t,
		"alice",
		[][]byte{f.sourceA},
		[][]byte{f.policyA},
		allOperations,
	))
}

func (f *fixture) bob(t testing.TB) context.Context {
	t.Helper()
	return f.context(t, f.decision(
		t,
		"bob",
		[][]byte{f.sourceB},
		[][]byte{f.policyB},
		allOperations,
	))
}

func TestAuthorizedVisibilityAndRetrievalProjection(t *testing.T) {
	f := newFixture(t)
	metadata := shoal.Metadata{
		"authorization_domain": "forged-domain",
		"source_id":            "source-b",
		"policy_id":            "policy-b",
	}
	originalMetadata := cloneMetadata(metadata)
	visible, err := f.clientA.Ingest(f.admin(t), explorer.Source{
		URI:       "file:///visible.txt",
		Title:     "Visible",
		MediaType: explorer.MediaTypeText,
		Content:   "alpha",
		Metadata:  metadata,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(metadata, originalMetadata) {
		t.Fatal("Ingest mutated caller metadata")
	}
	hidden, err := f.clientB.Ingest(f.admin(t), explorer.Source{
		URI:       "file:///hidden.txt",
		Title:     "Hidden alpha beta",
		MediaType: explorer.MediaTypeText,
		Content:   "alpha beta",
	})
	if err != nil {
		t.Fatal(err)
	}

	alice := f.alice(t)
	summaries, err := f.clientA.Documents(alice)
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].Document.ID != visible.Document.ID {
		t.Fatalf("visible summaries = %#v", summaries)
	}

	hiddenView, err := f.clientB.Document(
		f.bob(t), hidden.Document.ID, hidden.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	hiddenSpanID := firstSpanID(t, hiddenView)
	hiddenErr := documentError(
		f.clientA, alice, hidden.Document.ID, hidden.Revision.ID)
	absentErr := documentError(f.clientA, alice, "absent-document", "absent-revision")
	if hiddenErr.Error() != absentErr.Error() ||
		!shoal.IsErrorCode(hiddenErr, shoal.ErrorNotFound) {
		t.Fatalf("hidden error %v differs from absent %v", hiddenErr, absentErr)
	}

	raw, err := f.base.Retrieve(context.Background(), retrieval.Request{
		Text: "alpha beta", TopK: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(raw.Results) != 1 ||
		raw.Results[0].Evidence[0].Citation.DocumentID != hidden.Document.ID {
		t.Fatalf("raw scorer did not prove hidden displacement: %#v", raw)
	}
	response, err := f.clientA.Retrieve(alice, retrieval.Request{
		Text: "alpha beta", TopK: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 1 ||
		response.Results[0].Evidence[0].Citation.DocumentID != visible.Document.ID {
		t.Fatalf("authorized retrieval = %#v", response)
	}
	if response.RequestID != "" {
		t.Fatalf("authorized retrieval exposed backend request ID %q", response.RequestID)
	}

	empty, err := f.clientA.Retrieve(alice, retrieval.Request{
		Text: "alpha beta",
		Scope: retrieval.Scope{DocumentIDs: []shoal.ID{
			hidden.Document.ID,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.Results) != 0 {
		t.Fatalf("hidden document scope broadened: %#v", empty)
	}
	empty, err = f.clientA.Retrieve(alice, retrieval.Request{
		Text: "alpha beta",
		Scope: retrieval.Scope{NodeIDs: []shoal.ID{
			hiddenSpanID,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.Results) != 0 {
		t.Fatalf("hidden node scope broadened: %#v", empty)
	}

	documentScope := []shoal.ID{hidden.Document.ID, visible.Document.ID}
	nodeScope := []shoal.ID{hiddenSpanID}
	modes := []retrieval.Mode{retrieval.ModeLexical}
	request := retrieval.Request{
		Text:  "alpha beta",
		Modes: modes,
		Scope: retrieval.Scope{
			DocumentIDs: documentScope,
			NodeIDs:     nodeScope,
		},
	}
	documentCopy := append([]shoal.ID(nil), documentScope...)
	nodeCopy := append([]shoal.ID(nil), nodeScope...)
	modeCopy := append([]retrieval.Mode(nil), modes...)
	if _, err := f.clientA.Retrieve(alice, request); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(documentScope, documentCopy) ||
		!reflect.DeepEqual(nodeScope, nodeCopy) ||
		!reflect.DeepEqual(modes, modeCopy) {
		t.Fatal("Retrieve mutated caller slices")
	}
}

func TestAuthorizedVectorRetrievalProjection(t *testing.T) {
	f := newFixture(t)
	base, err := explorer.OpenWithOptions(t.TempDir(), explorer.Options{
		Embedder: model.FakeEmbedder{Model: "authorized", Dimensions: 8},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = base.Close() })
	clientA := f.newClient(t, base, f.store, f.sourceA, f.policyA, nil)
	clientB := f.newClient(t, base, f.store, f.sourceB, f.policyB, nil)
	visible, err := clientA.Ingest(f.admin(t), explorer.Source{
		URI:       "file:///visible-vector.txt",
		Title:     "Visible Vector",
		MediaType: explorer.MediaTypeText,
		Content:   "authorized visible vector evidence",
	})
	if err != nil {
		t.Fatal(err)
	}
	hidden, err := clientB.Ingest(f.admin(t), explorer.Source{
		URI:       "file:///hidden-vector.txt",
		Title:     "Hidden Vector",
		MediaType: explorer.MediaTypeText,
		Content:   "hidden vector evidence wins raw ranking",
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base.Retrieve(context.Background(), retrieval.Request{
		Text:  "hidden vector evidence wins raw ranking",
		TopK:  1,
		Modes: []retrieval.Mode{retrieval.ModeVector},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(raw.Results) != 1 ||
		raw.Results[0].Evidence[0].Citation.DocumentID != hidden.Document.ID {
		t.Fatalf("raw vector retrieval = %#v", raw)
	}
	available, err := clientA.VectorAvailable(f.alice(t))
	if err != nil {
		t.Fatal(err)
	}
	if !available {
		t.Fatal("authorized vector capability was not delegated")
	}
	response, err := clientA.Retrieve(f.alice(t), retrieval.Request{
		Text:  "hidden vector evidence wins raw ranking",
		TopK:  1,
		Modes: []retrieval.Mode{retrieval.ModeVector},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 1 ||
		response.Results[0].Evidence[0].Citation.DocumentID != visible.Document.ID ||
		response.RequestID != "" {
		t.Fatalf("authorized vector retrieval = %#v", response)
	}
}

func TestAuthorizedEmptyVectorRetrievalDoesNotProbeProvider(t *testing.T) {
	f := newFixture(t)
	base, err := explorer.OpenWithOptions(t.TempDir(), explorer.Options{
		Embedder: model.FakeEmbedder{Model: "authorized-empty", Dimensions: 8},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = base.Close() })
	clientB := f.newClient(t, base, f.store, f.sourceB, f.policyB, nil)
	if _, err := clientB.Ingest(f.admin(t), explorer.Source{
		URI:       "file:///hidden-vector-only.txt",
		MediaType: explorer.MediaTypeText,
		Content:   "hidden vector evidence",
	}); err != nil {
		t.Fatal(err)
	}
	retrieveCalls := 0
	untrusted := &hookClient{
		Client: base,
		retrieve: func(
			context.Context, retrieval.Request,
		) (retrieval.Response, error) {
			retrieveCalls++
			return retrieval.Response{}, errors.New(
				"empty authorized retrieval reached the untrusted base")
		},
	}
	selector, err := authorized.NewStaticPolicySelector(f.sourceA, f.policyA)
	if err != nil {
		t.Fatal(err)
	}
	scorer := &countingVectorScorer{VectorScorer: base}
	clientA, err := authorized.NewClient(authorized.Config{
		Base: untrusted, VectorScorer: scorer,
		Resolver: f.authority.Resolver(), PolicySelector: selector,
		PolicyStore: f.store, GenerationReader: f.reader, Clock: f.clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err := clientA.Retrieve(f.alice(t), retrieval.Request{
		Text:  "hidden vector evidence",
		TopK:  1,
		Modes: []retrieval.Mode{retrieval.ModeVector},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 0 {
		t.Fatalf("authorized empty vector response = %+v", response)
	}
	if scorer.calls != 0 {
		t.Fatalf("trusted vector scorer calls = %d, want 0", scorer.calls)
	}
	if retrieveCalls != 0 {
		t.Fatalf("untrusted base retrieval calls = %d", retrieveCalls)
	}
}

func TestAuthorizedVectorRetrievalUsesTrustedScorer(t *testing.T) {
	f := newFixture(t)
	base, err := explorer.OpenWithOptions(t.TempDir(), explorer.Options{
		Embedder: model.FakeEmbedder{Model: "authorized-trusted", Dimensions: 8},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = base.Close() })
	clientA := f.newClient(t, base, f.store, f.sourceA, f.policyA, nil)
	if _, err := clientA.Ingest(f.admin(t), explorer.Source{
		URI:       "file:///trusted-vector.txt",
		Title:     "Trusted Vector",
		MediaType: explorer.MediaTypeText,
		Content:   "trusted vector evidence",
	}); err != nil {
		t.Fatal(err)
	}
	request := retrieval.Request{
		Text:    "trusted vector evidence",
		TopK:    1,
		Modes:   []retrieval.Mode{retrieval.ModeVector},
		Explain: true,
	}
	valid, err := clientA.Retrieve(f.alice(t), request)
	if err != nil {
		t.Fatal(err)
	}
	valid.Results[0].Score = shoal.Score(math.Nextafter(
		float64(valid.Results[0].Score), math.Inf(1)))
	valid.Results[0].Evidence[0].Score = valid.Results[0].Score
	malicious := &maliciousVectorClient{
		hookClient: &hookClient{
			Client: base,
			retrieve: func(
				context.Context,
				retrieval.Request,
			) (retrieval.Response, error) {
				return valid, nil
			},
		},
	}
	selector, err := authorized.NewStaticPolicySelector(f.sourceA, f.policyA)
	if err != nil {
		t.Fatal(err)
	}
	client, err := authorized.NewClient(authorized.Config{
		Base:             malicious,
		VectorScorer:     base,
		Resolver:         f.authority.Resolver(),
		PolicySelector:   selector,
		PolicyStore:      f.store,
		GenerationReader: f.reader,
		Clock:            f.clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Retrieve(f.alice(t), request)
	if !shoal.IsErrorCode(err, shoal.ErrorInternal) {
		t.Fatalf("malicious vector response error = %v", err)
	}
	if malicious.scoreCalls != 0 {
		t.Fatalf("untrusted vector scorer was called %d times", malicious.scoreCalls)
	}
}

func TestAuthorizedVectorCapabilityIgnoresHiddenIncompleteCorpus(t *testing.T) {
	f := newFixture(t)
	dataDir := t.TempDir()
	legacyBase, err := explorer.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	legacyClientB := f.newClient(t, legacyBase, f.store, f.sourceB, f.policyB, nil)
	if _, err := legacyClientB.Ingest(f.admin(t), explorer.Source{
		URI:       "file:///hidden-legacy.txt",
		Title:     "Hidden Legacy",
		MediaType: explorer.MediaTypeText,
		Content:   "hidden legacy has no embeddings",
	}); err != nil {
		t.Fatal(err)
	}
	if err := legacyBase.Close(); err != nil {
		t.Fatal(err)
	}

	base, err := explorer.OpenWithOptions(dataDir, explorer.Options{
		Embedder: model.FakeEmbedder{Model: "authorized-visible", Dimensions: 8},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = base.Close() })
	clientA := f.newClient(t, base, f.store, f.sourceA, f.policyA, nil)
	visible, err := clientA.Ingest(f.admin(t), explorer.Source{
		URI:       "file:///visible-complete.txt",
		Title:     "Visible Complete",
		MediaType: explorer.MediaTypeText,
		Content:   "visible complete vector evidence",
	})
	if err != nil {
		t.Fatal(err)
	}
	available, err := clientA.VectorAvailable(f.alice(t))
	if err != nil {
		t.Fatal(err)
	}
	if !available {
		t.Fatal("hidden incomplete corpus disabled visible vector capability")
	}
	response, err := clientA.Retrieve(f.alice(t), retrieval.Request{
		Text:  "visible complete vector evidence",
		TopK:  1,
		Modes: []retrieval.Mode{retrieval.ModeVector},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 1 ||
		response.Results[0].Evidence[0].Citation.DocumentID != visible.Document.ID {
		t.Fatalf("visible vector response = %#v", response)
	}
}

func TestAuthorizedVectorCapabilityCachesVisibleProjection(t *testing.T) {
	f := newFixture(t)
	base, err := explorer.OpenWithOptions(t.TempDir(), explorer.Options{
		Embedder: model.FakeEmbedder{Model: "authorized-cache", Dimensions: 8},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = base.Close() })
	ingestClient := f.newClient(t, base, f.store, f.sourceA, f.policyA, nil)
	if _, err := ingestClient.Ingest(f.admin(t), explorer.Source{
		URI:       "file:///cached-vector.txt",
		Title:     "Cached Vector",
		MediaType: explorer.MediaTypeText,
		Content:   "cached vector evidence",
	}); err != nil {
		t.Fatal(err)
	}
	documentCalls := 0
	hooked := &hookClient{
		Client: base,
		document: func(
			ctx context.Context,
			documentID, revisionID shoal.ID,
		) (explorer.DocumentView, error) {
			documentCalls++
			return base.Document(ctx, documentID, revisionID)
		},
	}
	scorer := &countingVectorScorer{VectorScorer: base}
	selector, err := authorized.NewStaticPolicySelector(f.sourceA, f.policyA)
	if err != nil {
		t.Fatal(err)
	}
	client, err := authorized.NewClient(authorized.Config{
		Base:             hooked,
		VectorScorer:     scorer,
		Resolver:         f.authority.Resolver(),
		PolicySelector:   selector,
		PolicyStore:      f.store,
		GenerationReader: f.reader,
		Clock:            f.clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		available, err := client.VectorAvailable(f.alice(t))
		if err != nil {
			t.Fatal(err)
		}
		if !available {
			t.Fatal("vector unexpectedly unavailable")
		}
	}
	if documentCalls != 1 || scorer.calls != 1 {
		t.Fatalf("cache misses: documents=%d scores=%d", documentCalls, scorer.calls)
	}
	f.clock.Set(f.clock.Now().Add(-time.Minute))
	available, err := client.VectorAvailable(f.alice(t))
	if err != nil {
		t.Fatal(err)
	}
	if !available {
		t.Fatal("vector unexpectedly unavailable after clock rollback")
	}
	if documentCalls != 2 || scorer.calls != 2 {
		t.Fatalf("rollback reused cache: documents=%d scores=%d", documentCalls, scorer.calls)
	}
}

func TestAuthorizedVectorCapabilityRequiresBaseRetrievalSupport(t *testing.T) {
	f := newFixture(t)
	base, err := explorer.OpenWithOptions(t.TempDir(), explorer.Options{
		Embedder: model.FakeEmbedder{Model: "authorized-base-support", Dimensions: 8},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = base.Close() })
	ingestClient := f.newClient(t, base, f.store, f.sourceA, f.policyA, nil)
	if _, err := ingestClient.Ingest(f.admin(t), explorer.Source{
		URI:       "file:///base-support.txt",
		Title:     "Base Support",
		MediaType: explorer.MediaTypeText,
		Content:   "base support vector evidence",
	}); err != nil {
		t.Fatal(err)
	}
	hooked := &hookClient{
		Client: base,
		retrieve: func(
			context.Context,
			retrieval.Request,
		) (retrieval.Response, error) {
			return retrieval.Response{}, shoal.NewError(
				shoal.ErrorUnavailable, "vector unsupported")
		},
	}
	selector, err := authorized.NewStaticPolicySelector(f.sourceA, f.policyA)
	if err != nil {
		t.Fatal(err)
	}
	client, err := authorized.NewClient(authorized.Config{
		Base:             hooked,
		VectorScorer:     base,
		Resolver:         f.authority.Resolver(),
		PolicySelector:   selector,
		PolicyStore:      f.store,
		GenerationReader: f.reader,
		Clock:            f.clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	available, err := client.VectorAvailable(f.alice(t))
	if err != nil {
		t.Fatal(err)
	}
	if available {
		t.Fatal("base without vector retrieval support advertised vector capability")
	}
}

func TestAuthorizedVectorCapabilityFailsClosedForOversizedProjection(t *testing.T) {
	f := newFixture(t)
	base, err := explorer.OpenWithOptions(t.TempDir(), explorer.Options{
		Embedder: model.FakeEmbedder{Model: "authorized-large-projection", Dimensions: 8},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = base.Close() })
	policy, err := auth.NewPolicy(auth.PolicyConfig{
		AuthorizationDomain: f.domain,
		SourceID:            f.sourceA,
		GrantPolicyID:       f.policyA,
		Epoch:               1,
	})
	if err != nil {
		t.Fatal(err)
	}
	rule, err := authorized.NewAccessRule(policy)
	if err != nil {
		t.Fatal(err)
	}
	summaries := make([]explorer.DocumentSummary, 0, retrieval.MaxScopeIDs+1)
	for i := 0; i <= retrieval.MaxScopeIDs; i++ {
		documentID := shoal.ID("large-vector-doc-" + strconv.Itoa(i))
		revisionID := shoal.ID("large-vector-rev-" + strconv.Itoa(i))
		rootID := shoal.ID("large-vector-root-" + strconv.Itoa(i))
		summaries = append(summaries, explorer.DocumentSummary{
			Document: document.Document{
				ID: documentID, RevisionID: revisionID, RootSectionID: rootID,
			},
			Revision: document.Revision{ID: revisionID, DocumentID: documentID},
		})
		if err := f.store.PutRevision(context.Background(), authorized.RevisionRegistration{
			DocumentID:    documentID,
			RevisionID:    revisionID,
			NodeIDs:       []shoal.ID{documentID},
			ContentDigest: auth.DigestBytes("test-large-vector-projection", []byte(documentID)),
			Rule:          rule,
			Current:       true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	documentCalls := 0
	retrieveCalls := 0
	hooked := &hookClient{
		Client: base,
		documents: func(context.Context) ([]explorer.DocumentSummary, error) {
			return summaries, nil
		},
		document: func(
			context.Context, shoal.ID, shoal.ID,
		) (explorer.DocumentView, error) {
			documentCalls++
			return explorer.DocumentView{}, errors.New("unexpected hydrate")
		},
		retrieve: func(
			context.Context,
			retrieval.Request,
		) (retrieval.Response, error) {
			retrieveCalls++
			return retrieval.Response{}, errors.New("unexpected probe")
		},
	}
	selector, err := authorized.NewStaticPolicySelector(f.sourceA, f.policyA)
	if err != nil {
		t.Fatal(err)
	}
	client, err := authorized.NewClient(authorized.Config{
		Base:             hooked,
		VectorScorer:     base,
		Resolver:         f.authority.Resolver(),
		PolicySelector:   selector,
		PolicyStore:      f.store,
		GenerationReader: f.reader,
		Clock:            f.clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	available, err := client.VectorAvailable(f.alice(t))
	if err != nil {
		t.Fatal(err)
	}
	if available {
		t.Fatal("oversized authorized vector projection advertised capability")
	}
	if documentCalls != 0 || retrieveCalls != 0 {
		t.Fatalf("oversized projection probed base: documents=%d retrieve=%d",
			documentCalls, retrieveCalls)
	}
}

func TestHiddenCurrentDocumentIsAuthorizedBeforeBaseRead(t *testing.T) {
	f := newFixture(t)
	hidden, err := f.clientB.Ingest(f.admin(t), explorer.Source{
		URI: "file:///hidden-current.txt", MediaType: explorer.MediaTypeText,
		Content: "hidden current content",
	})
	if err != nil {
		t.Fatal(err)
	}
	documentCalls := 0
	hooked := &hookClient{
		Client: f.base,
		document: func(
			context.Context, shoal.ID, shoal.ID,
		) (explorer.DocumentView, error) {
			documentCalls++
			return explorer.DocumentView{}, shoal.NewError(
				shoal.ErrorUnavailable, "backend detail")
		},
	}
	client := f.newClient(t, hooked, f.store, f.sourceA, f.policyA, nil)
	_, err = client.Document(f.alice(t), hidden.Document.ID, "")
	if !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("hidden current document error = %v", err)
	}
	if documentCalls != 0 {
		t.Fatalf("hidden current document reached base %d times", documentCalls)
	}
}

func TestResolverExpiryOperationAndSelectorFailures(t *testing.T) {
	f := newFixture(t)
	if _, err := f.clientA.Documents(context.Background()); !shoal.IsErrorCode(
		err, shoal.ErrorUnauthorized,
	) {
		t.Fatalf("missing context error = %v", err)
	}

	decision := f.decision(
		t,
		"forged",
		[][]byte{f.sourceA},
		[][]byte{f.policyA},
		allOperations,
	)
	otherAuthority, err := auth.NewAuthorityWithClock(f.clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	forged, err := otherAuthority.Binder().Bind(context.Background(), decision)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.clientA.Documents(forged); !shoal.IsErrorCode(
		err, shoal.ErrorUnauthorized,
	) {
		t.Fatalf("forged context error = %v", err)
	}
	canceled, cancel := context.WithCancel(f.alice(t))
	cancel()
	if _, err := f.clientA.Documents(canceled); !shoal.IsErrorCode(
		err, shoal.ErrorCanceled,
	) {
		t.Fatalf("cancellation error = %v", err)
	}
	deadline, cancelDeadline := context.WithDeadline(
		f.alice(t), time.Now().Add(-time.Second))
	defer cancelDeadline()
	if _, err := f.clientA.Documents(deadline); !shoal.IsErrorCode(
		err, shoal.ErrorDeadline,
	) {
		t.Fatalf("deadline error = %v", err)
	}

	readOnly := f.context(t, f.decision(
		t,
		"reader",
		[][]byte{f.sourceA},
		[][]byte{f.policyA},
		[]auth.Operation{auth.OperationRead},
	))
	if _, err := f.clientA.Documents(readOnly); !shoal.IsErrorCode(
		err, shoal.ErrorUnauthorized,
	) {
		t.Fatalf("operation denial = %v", err)
	}

	expiringDecision, err := auth.NewDecision(auth.DecisionConfig{
		Subject:               "expiring",
		Actor:                 "expiring-actor",
		AuthorizationDomain:   f.domain,
		AllowedOperations:     allOperations,
		PermittedSourceIDs:    [][]byte{f.sourceA},
		PermittedPolicyIDs:    [][]byte{f.policyA},
		PolicyGeneration:      1,
		AuthenticationExpires: f.clock.Now().Add(time.Minute),
		RequestID:             "expiring-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	expiring := f.context(t, expiringDecision)
	originalNow := f.clock.Now()
	f.clock.Set(originalNow.Add(2 * time.Minute))
	if _, err := f.clientA.Documents(expiring); !shoal.IsErrorCode(
		err, shoal.ErrorUnauthorized,
	) {
		t.Fatalf("expiry error = %v", err)
	}
	f.clock.Set(originalNow)

	maliciousSelector := authorized.PolicySelectorFunc(func(
		_ context.Context,
		decision auth.Decision,
		_ explorer.Source,
	) (auth.Policy, error) {
		return auth.NewPolicy(auth.PolicyConfig{
			AuthorizationDomain: decision.AuthorizationDomain(),
			SourceID:            f.sourceB,
			GrantPolicyID:       f.policyB,
			Epoch:               decision.PolicyGeneration(),
		})
	})
	client, err := authorized.NewClient(authorized.Config{
		Base:           f.base,
		Resolver:       f.authority.Resolver(),
		PolicySelector: maliciousSelector,
		EdgePolicySelector: authorized.EdgePolicySelectorFunc(func(
			_ context.Context,
			decision auth.Decision,
			_ graph.Edge,
		) (auth.Policy, error) {
			return auth.NewPolicy(auth.PolicyConfig{
				AuthorizationDomain: decision.AuthorizationDomain(),
				SourceID:            f.sourceA,
				GrantPolicyID:       f.policyA,
				Epoch:               decision.PolicyGeneration(),
			})
		}),
		PolicyStore:      f.store,
		GenerationReader: f.reader,
		Clock:            f.clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	alice := f.alice(t)
	if _, err := client.Ingest(alice, explorer.Source{
		URI: "file:///rejected.txt", MediaType: explorer.MediaTypeText,
		Content: "must not commit",
	}); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("out-of-grant selector error = %v", err)
	}
	summaries, err := f.base.Documents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 0 {
		t.Fatalf("selector denial committed data: %#v", summaries)
	}
}

func TestResolverErrorCategoriesArePreserved(t *testing.T) {
	f := newFixture(t)
	selector, err := authorized.NewStaticPolicySelector(f.sourceA, f.policyA)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		err  error
		code shoal.ErrorCode
	}{
		{
			name: "canceled",
			err:  shoal.NewError(shoal.ErrorCanceled, "sensitive resolver detail"),
			code: shoal.ErrorCanceled,
		},
		{
			name: "deadline",
			err:  shoal.NewError(shoal.ErrorDeadline, "sensitive resolver detail"),
			code: shoal.ErrorDeadline,
		},
		{
			name: "unavailable",
			err:  shoal.NewError(shoal.ErrorUnavailable, "sensitive resolver detail"),
			code: shoal.ErrorUnavailable,
		},
		{
			name: "internal",
			err:  shoal.NewError(shoal.ErrorInternal, "sensitive resolver detail"),
			code: shoal.ErrorInternal,
		},
		{
			name: "unauthorized",
			err:  shoal.NewError(shoal.ErrorUnauthorized, "sensitive resolver detail"),
			code: shoal.ErrorUnauthorized,
		},
		{
			name: "not found denial",
			err:  shoal.NewError(shoal.ErrorNotFound, "sensitive resolver detail"),
			code: shoal.ErrorUnauthorized,
		},
		{
			name: "invalid context denial",
			err: shoal.NewError(
				shoal.ErrorInvalidArgument, "sensitive resolver detail"),
			code: shoal.ErrorUnauthorized,
		},
		{
			name: "unknown infrastructure",
			err:  errors.New("sensitive resolver detail"),
			code: shoal.ErrorUnavailable,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, err := authorized.NewClient(authorized.Config{
				Base: f.base,
				Resolver: resolverFunc(func(
					context.Context,
				) (auth.Decision, error) {
					return auth.Decision{}, test.err
				}),
				PolicySelector:   selector,
				PolicyStore:      f.store,
				GenerationReader: f.reader,
				Clock:            f.clock.Now,
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Documents(context.Background())
			if !shoal.IsErrorCode(err, test.code) {
				t.Fatalf("resolver error = %v, want %s", err, test.code)
			}
			if strings.Contains(err.Error(), "sensitive") {
				t.Fatalf("resolver error disclosed details: %v", err)
			}
		})
	}
}

func TestPolicyStoreFailureRetryAndImmutablePolicyConflict(t *testing.T) {
	f := newFixture(t)
	failing := &failStore{PolicyStore: f.store, revisionFailures: 1}
	client := f.newClient(t, f.base, failing, f.sourceA, f.policyA, nil)
	source := explorer.Source{
		URI: "file:///retry.txt", MediaType: explorer.MediaTypeText,
		Content: "retry reconciliation",
	}
	if _, err := client.Ingest(f.admin(t), source); !shoal.IsErrorCode(
		err, shoal.ErrorUnavailable,
	) {
		t.Fatalf("first catalog failure = %v", err)
	}
	raw, err := f.base.Documents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 1 {
		t.Fatalf("base commit missing after catalog failure: %#v", raw)
	}
	failedRevisionID := raw[0].Revision.ID
	if _, err := f.clientB.Ingest(f.bob(t), source); !shoal.IsErrorCode(
		err, shoal.ErrorNotFound,
	) {
		t.Fatalf("different-policy seizure after catalog failure = %v", err)
	}
	raw, err = f.base.Documents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 1 || raw[0].Revision.ID != failedRevisionID {
		t.Fatalf("denied retry changed base document: %#v", raw)
	}
	result, err := client.Ingest(f.admin(t), source)
	if err != nil {
		t.Fatal(err)
	}
	if result.Disposition != explorer.IngestUnchanged {
		t.Fatalf("retry disposition = %q", result.Disposition)
	}
	if _, err := client.Document(
		f.alice(t), result.Document.ID, result.Revision.ID); err != nil {
		t.Fatal(err)
	}

	conflicting := f.newClient(t, f.base, f.store, f.sourceB, f.policyB, nil)
	if _, err := conflicting.Ingest(f.admin(t), source); !shoal.IsErrorCode(
		err, shoal.ErrorConflict,
	) {
		t.Fatalf("immutable policy reuse error = %v", err)
	}
}

func TestExistingSourceWithoutClaimOrRegistrationIsUnavailable(t *testing.T) {
	f := newFixture(t)
	source := explorer.Source{
		URI: "file:///uncataloged.txt", MediaType: explorer.MediaTypeText,
		Content: "uncataloged base content",
	}
	raw, err := f.base.Ingest(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.clientA.Ingest(
		f.admin(t), source,
	); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("uncataloged existing source error = %v", err)
	}
	current, err := f.base.Documents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(current) != 1 || current[0].Revision.ID != raw.Revision.ID {
		t.Fatalf("unavailable ingest changed base: %#v", current)
	}
}

func TestLegacyRegistrationBackfillsClaimAfterAuthorization(t *testing.T) {
	f := newFixture(t)
	const uri = "file:///legacy-claim.txt"
	original, err := f.clientA.Ingest(f.admin(t), explorer.Source{
		URI: uri, MediaType: explorer.MediaTypeText,
		Content: "legacy policy A content",
	})
	if err != nil {
		t.Fatal(err)
	}
	registration, ok, err := f.store.CurrentRevision(
		context.Background(), original.Document.ID)
	if err != nil || !ok {
		t.Fatalf("original registration: ok=%v err=%v", ok, err)
	}
	legacyStore := authorized.NewMemoryPolicyStore()
	if err := legacyStore.PutRevision(
		context.Background(), registration); err != nil {
		t.Fatal(err)
	}
	legacyA := f.newClient(
		t, f.base, legacyStore, f.sourceA, f.policyA, nil)
	legacyB := f.newClient(
		t, f.base, legacyStore, f.sourceB, f.policyB, nil)

	if _, err := legacyB.Ingest(f.bob(t), explorer.Source{
		URI: uri, MediaType: explorer.MediaTypeText,
		Content: "unauthorized legacy seizure",
	}); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("unauthorized legacy backfill = %v", err)
	}
	if _, ok, err := legacyStore.SourceClaim(
		context.Background(), uri); err != nil || ok {
		t.Fatalf("unauthorized caller created claim: ok=%v err=%v", ok, err)
	}

	reclassified, err := legacyB.Ingest(f.admin(t), explorer.Source{
		URI: uri, MediaType: explorer.MediaTypeText,
		Content: "authorized policy B reclassification",
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, ok, err := legacyStore.SourceClaim(context.Background(), uri)
	if err != nil || !ok {
		t.Fatalf("backfilled source claim: ok=%v err=%v", ok, err)
	}
	if reclassified.Document.ID != original.Document.ID {
		t.Fatalf("reclassification changed document: %#v", reclassified)
	}
	if _, err := legacyA.Document(
		f.alice(t), reclassified.Document.ID, reclassified.Revision.ID,
	); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("old policy retained reclassified document: %v", err)
	}
	if claim.Rule.String() == registration.Rule.String() {
		t.Fatal("authorized reclassification did not transition source claim")
	}
}

func TestConcurrentClientsShareSourceClaim(t *testing.T) {
	f := newFixture(t)
	started := make(chan struct{})
	release := make(chan struct{})
	hooked := &hookClient{
		Client: f.base,
		afterIngest: func() {
			close(started)
			<-release
		},
	}
	first := f.newClient(t, hooked, f.store, f.sourceA, f.policyA, nil)
	source := explorer.Source{
		URI: "file:///concurrent-claim.txt", MediaType: explorer.MediaTypeText,
		Content: "concurrent claim content",
	}
	firstErr := make(chan error, 1)
	go func() {
		_, err := first.Ingest(f.admin(t), source)
		firstErr <- err
	}()
	<-started
	secondErr := make(chan error, 1)
	go func() {
		_, err := f.clientB.Ingest(f.bob(t), source)
		secondErr <- err
	}()
	select {
	case err := <-secondErr:
		close(release)
		t.Fatalf("concurrent ingest was not serialized: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-firstErr; err != nil {
		t.Fatal(err)
	}
	if err := <-secondErr; !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("concurrent different-policy ingest = %v", err)
	}
}

func TestPendingFirstIngestRetriesUnderDesiredRule(t *testing.T) {
	f := newFixture(t)
	const uri = "file:///pending-first-ingest.txt"
	failed := false
	flaky := &ingestOverrideClient{
		Client: f.base,
		ingest: func(
			ctx context.Context,
			source explorer.Source,
		) (explorer.IngestResult, error) {
			if !failed {
				failed = true
				return explorer.IngestResult{}, explorer.MarkIndeterminateCommit(
					shoal.NewError(
						shoal.ErrorUnavailable, "ambiguous first ingest"),
				)
			}
			return f.base.Ingest(ctx, source)
		},
	}
	owner := f.newClient(t, flaky, f.store, f.sourceB, f.policyB, nil)
	source := explorer.Source{
		URI: uri, MediaType: explorer.MediaTypeText,
		Content: "pending first ingest content",
	}
	if _, err := owner.Ingest(
		f.bob(t), source,
	); !explorer.IsIndeterminateCommit(err) {
		t.Fatalf("first ingest error = %v", err)
	}
	pending, ok, err := f.store.SourceClaim(context.Background(), uri)
	if err != nil || !ok || !pending.Pending ||
		pending.PreviousRule != nil {
		t.Fatalf("pending first-ingest claim = %#v ok=%v err=%v", pending, ok, err)
	}
	result, err := owner.Ingest(f.bob(t), source)
	if err != nil {
		t.Fatal(err)
	}
	if result.Disposition != explorer.IngestApplied {
		t.Fatalf("first-ingest recovery = %#v", result)
	}
	resolved, ok, err := f.store.SourceClaim(context.Background(), uri)
	if err != nil || !ok || resolved.Pending ||
		resolved.PreviousRule != nil {
		t.Fatalf("resolved first-ingest claim = %#v ok=%v err=%v", resolved, ok, err)
	}
}

func TestPendingReclassificationAfterCommittedBaseRequiresBothRules(t *testing.T) {
	f := newFixture(t)
	const uri = "file:///indeterminate-reclassification.txt"
	original, err := f.clientA.Ingest(f.admin(t), explorer.Source{
		URI: uri, MediaType: explorer.MediaTypeText,
		Content: "original policy A revision",
	})
	if err != nil {
		t.Fatal(err)
	}
	indeterminateBase := &ingestOverrideClient{
		Client: f.base,
		ingest: func(
			ctx context.Context,
			source explorer.Source,
		) (explorer.IngestResult, error) {
			if _, err := f.base.Ingest(ctx, source); err != nil {
				return explorer.IngestResult{}, err
			}
			return explorer.IngestResult{}, explorer.MarkIndeterminateCommit(
				shoal.NewError(
					shoal.ErrorUnavailable,
					"indeterminate test storage commit",
				),
			)
		},
	}
	reclassifier := f.newClient(
		t, indeterminateBase, f.store, f.sourceB, f.policyB, nil)
	reclassifiedSource := explorer.Source{
		URI: uri, MediaType: explorer.MediaTypeText,
		Content: "committed policy B revision",
	}
	if _, err := reclassifier.Ingest(
		f.admin(t), reclassifiedSource,
	); !explorer.IsIndeterminateCommit(err) ||
		!shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("indeterminate reclassification error = %v", err)
	}
	afterCommit, err := f.base.Documents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(afterCommit) != 1 ||
		afterCommit[0].Revision.ID == original.Revision.ID {
		t.Fatalf("test base did not commit reclassification: %#v", afterCommit)
	}
	committedRevisionID := afterCommit[0].Revision.ID
	pending, ok, err := f.store.SourceClaim(context.Background(), uri)
	if err != nil || !ok || !pending.Pending ||
		pending.PreviousRule == nil {
		t.Fatalf("pending reclassification claim = %#v ok=%v err=%v", pending, ok, err)
	}

	if _, err := f.clientA.Ingest(f.alice(t), explorer.Source{
		URI: uri, MediaType: explorer.MediaTypeText,
		Content: "old policy overwrite attempt",
	}); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("old policy pending recovery = %v", err)
	}
	if _, err := f.clientB.Ingest(f.bob(t), reclassifiedSource); !shoal.IsErrorCode(
		err, shoal.ErrorNotFound,
	) {
		t.Fatalf("desired policy pending recovery = %v", err)
	}
	afterDenied, err := f.base.Documents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(afterDenied) != 1 ||
		afterDenied[0].Revision.ID != committedRevisionID {
		t.Fatalf("denied retry changed base: %#v", afterDenied)
	}

	retried, err := f.clientB.Ingest(f.admin(t), reclassifiedSource)
	if err != nil {
		t.Fatal(err)
	}
	if retried.Disposition != explorer.IngestUnchanged ||
		retried.Revision.ID != committedRevisionID {
		t.Fatalf("authorized retry = %#v", retried)
	}
	afterRetries, err := f.base.Documents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(afterRetries) != 1 ||
		afterRetries[0].Revision.ID != committedRevisionID {
		t.Fatalf("pending recovery changed base: %#v", afterRetries)
	}
	resolved, ok, err := f.store.SourceClaim(context.Background(), uri)
	if err != nil || !ok || resolved.Pending ||
		resolved.PreviousRule != nil {
		t.Fatalf("resolved reclassification claim = %#v ok=%v err=%v", resolved, ok, err)
	}
}

func TestPendingReclassificationBeforeBaseCommitRequiresBothRules(t *testing.T) {
	f := newFixture(t)
	const uri = "file:///precommit-indeterminate.txt"
	original, err := f.clientA.Ingest(f.alice(t), explorer.Source{
		URI: uri, MediaType: explorer.MediaTypeText,
		Content: "original policy A revision",
	})
	if err != nil {
		t.Fatal(err)
	}
	ambiguous := &ingestOverrideClient{
		Client: f.base,
		ingest: func(
			context.Context,
			explorer.Source,
		) (explorer.IngestResult, error) {
			return explorer.IngestResult{}, explorer.MarkIndeterminateCommit(
				shoal.NewError(
					shoal.ErrorUnavailable, "ambiguous before base commit"),
			)
		},
	}
	reclassifier := f.newClient(
		t, ambiguous, f.store, f.sourceB, f.policyB, nil)
	sourceB := explorer.Source{
		URI: uri, MediaType: explorer.MediaTypeText,
		Content: "policy B recovery revision",
	}
	if _, err := reclassifier.Ingest(
		f.admin(t), sourceB,
	); !explorer.IsIndeterminateCommit(err) {
		t.Fatalf("precommit indeterminate error = %v", err)
	}
	if _, err := f.clientA.Ingest(
		f.alice(t), sourceB,
	); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("previous-rule-only recovery = %v", err)
	}
	if _, err := f.clientB.Ingest(
		f.bob(t), sourceB,
	); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("desired-rule-only seizure = %v", err)
	}
	if _, err := f.clientA.Ingest(
		f.admin(t), explorer.Source{
			URI: uri, MediaType: explorer.MediaTypeText,
			Content: "different selected-rule transition",
		},
	); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("pending selected-rule transition = %v", err)
	}
	beforeRecovery, err := f.base.Documents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(beforeRecovery) != 1 ||
		beforeRecovery[0].Revision.ID != original.Revision.ID {
		t.Fatalf("denied recovery changed base: %#v", beforeRecovery)
	}
	recovered, err := f.clientB.Ingest(f.admin(t), sourceB)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Disposition != explorer.IngestApplied {
		t.Fatalf("precommit recovery = %#v", recovered)
	}
}

func TestDefiniteRecoveryFailureRestoresPendingClaim(t *testing.T) {
	f := newFixture(t)
	const uri = "file:///pending-definite-failure.txt"
	if _, err := f.clientA.Ingest(f.alice(t), explorer.Source{
		URI: uri, MediaType: explorer.MediaTypeText,
		Content: "original policy A revision",
	}); err != nil {
		t.Fatal(err)
	}
	sourceB := explorer.Source{
		URI: uri, MediaType: explorer.MediaTypeText,
		Content: "policy B recovery revision",
	}
	ambiguous := &ingestOverrideClient{
		Client: f.base,
		ingest: func(
			context.Context,
			explorer.Source,
		) (explorer.IngestResult, error) {
			return explorer.IngestResult{}, explorer.MarkIndeterminateCommit(
				shoal.NewError(shoal.ErrorUnavailable, "ambiguous write"),
			)
		},
	}
	if _, err := f.newClient(
		t, ambiguous, f.store, f.sourceB, f.policyB, nil,
	).Ingest(f.admin(t), sourceB); !explorer.IsIndeterminateCommit(err) {
		t.Fatalf("initial pending error = %v", err)
	}
	before, ok, err := f.store.SourceClaim(context.Background(), uri)
	if err != nil || !ok || !before.Pending {
		t.Fatalf("initial pending claim = %#v ok=%v err=%v", before, ok, err)
	}
	attempt := 0
	definiteFailure := &ingestOverrideClient{
		Client: f.base,
		ingest: func(
			ctx context.Context,
			source explorer.Source,
		) (explorer.IngestResult, error) {
			attempt++
			switch attempt {
			case 1:
				return explorer.IngestResult{}, shoal.NewError(
					shoal.ErrorUnavailable, "definite precommit failure")
			case 2:
				return explorer.IngestResult{}, explorer.MarkIndeterminateCommit(
					shoal.NewError(
						shoal.ErrorUnavailable, "second ambiguous failure"),
				)
			}
			return f.base.Ingest(ctx, source)
		},
	}
	recovery := f.newClient(
		t, definiteFailure, f.store, f.sourceB, f.policyB, nil)
	if _, err := recovery.Ingest(
		f.admin(t), sourceB,
	); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) ||
		explorer.IsIndeterminateCommit(err) {
		t.Fatalf("definite recovery failure = %v", err)
	}
	restored, ok, err := f.store.SourceClaim(context.Background(), uri)
	if err != nil || !ok || !restored.Pending ||
		restored.Version != before.Version ||
		restored.Rule.String() != before.Rule.String() ||
		restored.PreviousRule == nil {
		t.Fatalf("restored pending claim = %#v ok=%v err=%v", restored, ok, err)
	}
	if _, err := f.clientB.Ingest(
		f.bob(t), sourceB,
	); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("desired-only retry after definite failure = %v", err)
	}
	if _, err := recovery.Ingest(
		f.admin(t), sourceB,
	); !explorer.IsIndeterminateCommit(err) {
		t.Fatalf("subsequent indeterminate recovery = %v", err)
	}
	repended, ok, err := f.store.SourceClaim(context.Background(), uri)
	if err != nil || !ok || !repended.Pending ||
		repended.PreviousRule == nil ||
		repended.Rule.String() != before.Rule.String() {
		t.Fatalf("repended source claim = %#v ok=%v err=%v", repended, ok, err)
	}
	result, err := recovery.Ingest(f.admin(t), sourceB)
	if err != nil {
		t.Fatal(err)
	}
	if result.Disposition != explorer.IngestApplied {
		t.Fatalf("later recovery = %#v", result)
	}
}

func TestConcurrentPendingRecoveryIsSerialized(t *testing.T) {
	f := newFixture(t)
	const uri = "file:///concurrent-pending-recovery.txt"
	if _, err := f.clientA.Ingest(f.alice(t), explorer.Source{
		URI: uri, MediaType: explorer.MediaTypeText,
		Content: "original policy A revision",
	}); err != nil {
		t.Fatal(err)
	}
	sourceB := explorer.Source{
		URI: uri, MediaType: explorer.MediaTypeText,
		Content: "policy B recovery revision",
	}
	ambiguous := &ingestOverrideClient{
		Client: f.base,
		ingest: func(
			context.Context,
			explorer.Source,
		) (explorer.IngestResult, error) {
			return explorer.IngestResult{}, explorer.MarkIndeterminateCommit(
				shoal.NewError(shoal.ErrorUnavailable, "ambiguous write"),
			)
		},
	}
	if _, err := f.newClient(
		t, ambiguous, f.store, f.sourceB, f.policyB, nil,
	).Ingest(f.admin(t), sourceB); !explorer.IsIndeterminateCommit(err) {
		t.Fatalf("initial pending error = %v", err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	hooked := &hookClient{
		Client: f.base,
		afterIngest: func() {
			close(started)
			<-release
		},
	}
	recovery := f.newClient(
		t, hooked, f.store, f.sourceB, f.policyB, nil)
	firstErr := make(chan error, 1)
	go func() {
		_, err := recovery.Ingest(f.admin(t), sourceB)
		firstErr <- err
	}()
	<-started
	secondErr := make(chan error, 1)
	go func() {
		_, err := f.clientB.Ingest(f.admin(t), sourceB)
		secondErr <- err
	}()
	select {
	case err := <-secondErr:
		close(release)
		t.Fatalf("concurrent recovery was not serialized: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-firstErr; err != nil {
		t.Fatal(err)
	}
	if err := <-secondErr; err != nil {
		t.Fatalf("serialized authorized recovery = %v", err)
	}
	summaries, err := f.base.Documents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 {
		t.Fatalf("concurrent recovery base = %#v", summaries)
	}
}

func TestDefiniteBaseFailureRollsBackNewClaim(t *testing.T) {
	f := newFixture(t)
	const uri = "file:///definite-rollback.txt"
	if _, err := f.clientA.Ingest(f.admin(t), explorer.Source{
		URI: uri, MediaType: "application/json", Content: "{}",
	}); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) ||
		explorer.IsIndeterminateCommit(err) {
		t.Fatalf("definite invalid ingest error = %v", err)
	}
	result, err := f.clientB.Ingest(f.bob(t), explorer.Source{
		URI: uri, MediaType: explorer.MediaTypeText,
		Content: "corrected policy B content",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Disposition != explorer.IngestApplied {
		t.Fatalf("corrected retry = %#v", result)
	}
}

func TestIngestSurfacesOriginalAndCleanupFailures(t *testing.T) {
	t.Run("indeterminate and pend", func(t *testing.T) {
		f := newFixture(t)
		store := &failFinalizeStore{
			PolicyStore: f.store,
			pendErr: shoal.NewError(
				shoal.ErrorConflict, "injected pend failure"),
		}
		base := &ingestOverrideClient{
			Client: f.base,
			ingest: func(
				context.Context,
				explorer.Source,
			) (explorer.IngestResult, error) {
				return explorer.IngestResult{},
					explorer.MarkIndeterminateCommit(shoal.NewError(
						shoal.ErrorUnavailable,
						"injected indeterminate failure",
					))
			},
		}
		client := f.newClient(
			t, base, store, f.sourceA, f.policyA, nil)
		if _, err := client.Ingest(f.alice(t), explorer.Source{
			URI:       "file:///cleanup-pend.txt",
			MediaType: explorer.MediaTypeText,
			Content:   "cleanup pend content",
		}); err == nil ||
			!explorer.IsIndeterminateCommit(err) ||
			!shoal.IsErrorCode(err, shoal.ErrorUnavailable) ||
			!shoal.IsErrorCode(err, shoal.ErrorConflict) {
			t.Fatalf("joined indeterminate cleanup error = %v", err)
		}
	})

	t.Run("definite and rollback", func(t *testing.T) {
		f := newFixture(t)
		store := &failFinalizeStore{
			PolicyStore: f.store,
			rollbackErr: shoal.NewError(
				shoal.ErrorConflict, "injected rollback failure"),
		}
		client := f.newClient(
			t, f.base, store, f.sourceA, f.policyA, nil)
		if _, err := client.Ingest(f.alice(t), explorer.Source{
			URI:       "file:///cleanup-rollback.txt",
			MediaType: "application/json",
			Content:   "{}",
		}); err == nil ||
			!shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) ||
			!shoal.IsErrorCode(err, shoal.ErrorConflict) ||
			explorer.IsIndeterminateCommit(err) {
			t.Fatalf("joined definite cleanup error = %v", err)
		}
	})
}

// TestHistoricalRevisionRetryIsRefused is the plain stale retry under the
// same rule: R1, then R2, then a delayed retry of R1. The base reports it
// historical (a success); the authorized client refuses it with
// ErrHistoricalRevision rather than report success for content it did not
// make current (#674). Before #674 it returned IngestUnchanged with R1. The
// current revision and the source claim are unchanged.
func TestHistoricalRevisionRetryIsRefused(t *testing.T) {
	f := newFixture(t)
	admin := f.admin(t)
	firstSource := explorer.Source{
		URI: "file:///history.txt", MediaType: explorer.MediaTypeText,
		Content: "first historical revision",
	}
	if _, err := f.clientA.Ingest(admin, firstSource); err != nil {
		t.Fatal(err)
	}
	second, err := f.clientA.Ingest(admin, explorer.Source{
		URI: "file:///history.txt", MediaType: explorer.MediaTypeText,
		Content: "second current revision",
	})
	if err != nil {
		t.Fatal(err)
	}
	before := readCatalogState(t, f.store, firstSource.URI, second.Document.ID)
	retried, err := f.clientA.Ingest(admin, firstSource)
	requireHistoricalRefusal(t, err)
	if !reflect.DeepEqual(retried, explorer.IngestResult{}) {
		t.Fatalf("historical retry returned a result: %#v", retried)
	}
	if after := readCatalogState(t, f.store, firstSource.URI, second.Document.ID); !reflect.DeepEqual(after, before) {
		t.Fatalf("historical retry changed the catalog:\nbefore %+v\nafter  %+v", before, after)
	}
	summaries, err := f.clientA.Documents(f.alice(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].Revision.ID != second.Revision.ID {
		t.Fatalf("historical retry changed current revision: %#v", summaries)
	}
}

// TestHistoricalRetryCannotTransitionCurrentSourceClaim pins what a failed
// registration after an applied base write leaves behind (#665). The claim
// is pending under the desired rule A with the replaced rule B as its
// previous rule; it is not committed under A. So:
//   - a historical retry under B cannot transition it (a pending claim admits
//     only its own rule);
//   - neither a holder of B alone nor a holder of A alone can finish it
//     (before #665 a holder of A alone could complete the reclassification
//     it never had the authority to start);
//   - a principal authorized under both rules completes it, and only then is
//     the claim committed under A.
func TestHistoricalRetryCannotTransitionCurrentSourceClaim(t *testing.T) {
	f := newFixture(t)
	const uri = "file:///historical-claim-transition.txt"
	oldSource := explorer.Source{
		URI: uri, MediaType: explorer.MediaTypeText,
		Content: "old policy B revision",
	}
	old, err := f.clientB.Ingest(f.bob(t), oldSource)
	if err != nil {
		t.Fatal(err)
	}
	failing := &failStore{PolicyStore: f.store, revisionFailures: 1}
	ruleAClient := f.newClient(
		t, f.base, failing, f.sourceA, f.policyA, nil)
	newSource := explorer.Source{
		URI: uri, MediaType: explorer.MediaTypeText,
		Content: "newer policy A revision",
	}
	if _, err := ruleAClient.Ingest(
		f.admin(t), newSource,
	); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("newer policy A catalog failure = %v", err)
	}
	current, err := f.base.Documents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(current) != 1 || current[0].Revision.ID == old.Revision.ID {
		t.Fatalf("newer base revision was not applied: %#v", current)
	}
	newRevisionID := current[0].Revision.ID
	pending, ok, err := f.store.SourceClaim(context.Background(), uri)
	if err != nil || !ok || !pending.Pending || pending.PreviousRule == nil {
		t.Fatalf("claim after a failed registration = %#v ok=%v err=%v, want pending with the replaced rule",
			pending, ok, err)
	}

	// A historical retry under B is refused and does not transition it.
	if _, err := f.clientB.Ingest(f.admin(t), oldSource); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("historical policy B retry on a pending policy A claim = %v, want not found", err)
	}
	if _, err := f.clientB.Ingest(f.bob(t), explorer.Source{
		URI: uri, MediaType: explorer.MediaTypeText,
		Content: "policy B overwrite attempt",
	}); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("policy B overwrite of a pending claim = %v, want not found", err)
	}
	// A holder of A alone cannot finish a reclassification away from B.
	if _, err := f.clientA.Ingest(f.alice(t), newSource); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("policy A holder finishing a B to A reclassification = %v, want not found", err)
	}
	after, ok, err := f.store.SourceClaim(context.Background(), uri)
	if err != nil || !ok || after.Version != pending.Version || !after.Pending {
		t.Fatalf("refused retries transitioned the claim: before=%#v after=%#v", pending, after)
	}
	current, err = f.base.Documents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(current) != 1 || current[0].Revision.ID != newRevisionID {
		t.Fatalf("refused retries changed the current base: %#v", current)
	}

	reconciled, err := f.clientA.Ingest(f.admin(t), newSource)
	if err != nil {
		t.Fatal(err)
	}
	if reconciled.Disposition != explorer.IngestUnchanged ||
		reconciled.Revision.ID != newRevisionID {
		t.Fatalf("current unchanged reconciliation = %#v", reconciled)
	}
	if _, err := f.clientA.Document(
		f.alice(t), reconciled.Document.ID, reconciled.Revision.ID,
	); err != nil {
		t.Fatalf("reconciled current revision: %v", err)
	}
	resolved, ok, err := f.store.SourceClaim(context.Background(), uri)
	if err != nil || !ok || resolved.Pending || resolved.PreviousRule != nil {
		t.Fatalf("resolved claim = %#v ok=%v err=%v", resolved, ok, err)
	}
}

func TestGenerationChangeAfterBaseOperation(t *testing.T) {
	f := newFixture(t)
	hooked := &hookClient{
		Client: f.base,
		afterIngest: func() {
			f.reader.Set(f.domain, 2)
		},
	}
	client := f.newClient(t, hooked, f.store, f.sourceA, f.policyA, nil)
	_, err := client.Ingest(f.admin(t), explorer.Source{
		URI: "file:///generation.txt", MediaType: explorer.MediaTypeText,
		Content: "generation changes after commit",
	})
	if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("generation change error = %v", err)
	}
}

func TestGenerationChangeBetweenSelectionAndMutationPreventsSideEffects(
	t *testing.T,
) {
	t.Run("ingest", func(t *testing.T) {
		f := newFixture(t)
		counting := &countingClient{Client: f.base}
		static, err := authorized.NewStaticPolicySelector(f.sourceA, f.policyA)
		if err != nil {
			t.Fatal(err)
		}
		selector := authorized.PolicySelectorFunc(func(
			ctx context.Context,
			decision auth.Decision,
			source explorer.Source,
		) (auth.Policy, error) {
			policy, err := static.SelectPolicy(ctx, decision, source)
			f.reader.Set(f.domain, 2)
			return policy, err
		})
		client, err := authorized.NewClient(authorized.Config{
			Base:               counting,
			Resolver:           f.authority.Resolver(),
			PolicySelector:     selector,
			EdgePolicySelector: static,
			PolicyStore:        f.store,
			GenerationReader:   f.reader,
			Clock:              f.clock.Now,
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.Ingest(f.admin(t), explorer.Source{
			URI:       "file:///selection-generation.txt",
			MediaType: explorer.MediaTypeText,
			Content:   "must not mutate",
		})
		if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
			t.Fatalf("generation error = %v", err)
		}
		if counting.IngestCount() != 0 {
			t.Fatalf("base ingest count = %d", counting.IngestCount())
		}
	})

	t.Run("connect", func(t *testing.T) {
		f := newFixture(t)
		first, err := f.clientA.Ingest(f.admin(t), explorer.Source{
			URI: "file:///selection-edge-a.txt", MediaType: explorer.MediaTypeText,
			Content: "edge a",
		})
		if err != nil {
			t.Fatal(err)
		}
		second, err := f.clientA.Ingest(f.admin(t), explorer.Source{
			URI: "file:///selection-edge-b.txt", MediaType: explorer.MediaTypeText,
			Content: "edge b",
		})
		if err != nil {
			t.Fatal(err)
		}
		counting := &countingClient{Client: f.base}
		static, err := authorized.NewStaticPolicySelector(f.sourceA, f.policyA)
		if err != nil {
			t.Fatal(err)
		}
		edgeSelector := authorized.EdgePolicySelectorFunc(func(
			ctx context.Context,
			decision auth.Decision,
			edge graph.Edge,
		) (auth.Policy, error) {
			policy, err := static.SelectEdgePolicy(ctx, decision, edge)
			f.reader.Set(f.domain, 2)
			return policy, err
		})
		client, err := authorized.NewClient(authorized.Config{
			Base:               counting,
			Resolver:           f.authority.Resolver(),
			PolicySelector:     static,
			EdgePolicySelector: edgeSelector,
			PolicyStore:        f.store,
			GenerationReader:   f.reader,
			Clock:              f.clock.Now,
		})
		if err != nil {
			t.Fatal(err)
		}
		err = client.Connect(f.admin(t), graph.Edge{
			ID:   appEdgeID(t, client, f.admin(t), "selection-generation-edge"),
			From: first.Document.ID, To: second.Document.ID,
			Type: "link", Weight: 1,
		})
		if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
			t.Fatalf("generation error = %v", err)
		}
		if counting.ConnectCount() != 0 {
			t.Fatalf("base connect count = %d", counting.ConnectCount())
		}
	})
}

func TestNewGenerationReadsOldLogicalPoliciesAndOldGenerationFailsGuard(
	t *testing.T,
) {
	f := newFixture(t)
	source := explorer.Source{
		URI: "file:///logical-generation.txt", MediaType: explorer.MediaTypeText,
		Content: "logical authorization outlives physical epoch",
	}
	result, err := f.clientA.Ingest(f.admin(t), source)
	if err != nil {
		t.Fatal(err)
	}
	oldContext := f.alice(t)
	f.reader.Set(f.domain, 2)
	if _, err := f.clientA.Documents(oldContext); !shoal.IsErrorCode(
		err, shoal.ErrorUnavailable,
	) {
		t.Fatalf("old generation error = %v", err)
	}
	newDecision := f.decisionAtGeneration(
		t,
		"alice-generation-two",
		[][]byte{f.sourceA},
		[][]byte{f.policyA},
		allOperations,
		2,
	)
	newContext := f.context(t, newDecision)
	if _, err := f.clientA.Document(
		newContext, result.Document.ID, result.Revision.ID); err != nil {
		t.Fatal(err)
	}
	retried, err := f.clientA.Ingest(newContext, source)
	if err != nil {
		t.Fatal(err)
	}
	if retried.Disposition != explorer.IngestUnchanged {
		t.Fatalf("new-generation retry disposition = %q", retried.Disposition)
	}
}

func TestUnreadableGenerationIsUnavailable(t *testing.T) {
	f := newFixture(t)
	if _, err := f.clientA.Ingest(f.admin(t), explorer.Source{
		URI: "file:///generation-unreadable.txt", MediaType: explorer.MediaTypeText,
		Content: "generation reader failure",
	}); err != nil {
		t.Fatal(err)
	}
	f.reader.SetError(errors.New("reader failed"))
	if _, err := f.clientA.Documents(f.alice(t)); !shoal.IsErrorCode(
		err, shoal.ErrorUnavailable,
	) {
		t.Fatalf("unreadable generation error = %v", err)
	}
}

func TestMaliciousRetrievalResultFailsClosed(t *testing.T) {
	f := newFixture(t)
	visible, err := f.clientA.Ingest(f.admin(t), explorer.Source{
		URI: "file:///visible-malicious.txt", MediaType: explorer.MediaTypeText,
		Content: "visible token",
	})
	if err != nil {
		t.Fatal(err)
	}

	_ = visible
	hidden, err := f.clientB.Ingest(f.admin(t), explorer.Source{
		URI: "file:///hidden-malicious.txt", MediaType: explorer.MediaTypeText,
		Content: "hidden token",
	})
	if err != nil {
		t.Fatal(err)
	}
	hiddenResponse, err := f.base.Retrieve(context.Background(), retrieval.Request{
		Text: "hidden token",
		Scope: retrieval.Scope{DocumentIDs: []shoal.ID{
			hidden.Document.ID,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	malicious := &hookClient{
		Client: f.base,
		retrieve: func(
			context.Context,
			retrieval.Request,
		) (retrieval.Response, error) {
			return hiddenResponse, nil
		},
	}
	client := f.newClient(t, malicious, f.store, f.sourceA, f.policyA, nil)
	_, err = client.Retrieve(f.alice(t), retrieval.Request{Text: "hidden token"})
	if !shoal.IsErrorCode(err, shoal.ErrorInternal) {
		t.Fatalf("malicious result error = %v", err)
	}
}

func TestRetrievalRejectsUnseededPlanBeforeCorpusScan(t *testing.T) {
	f := newFixture(t)
	documentsCalls := 0
	hooked := &hookClient{
		Client: f.base,
		documents: func(ctx context.Context) ([]explorer.DocumentSummary, error) {
			documentsCalls++
			return f.base.Documents(ctx)
		},
	}
	client := f.newClient(t, hooked, f.store, f.sourceA, f.policyA, nil)
	_, err := client.Retrieve(f.alice(t), retrieval.Request{
		Text: "tree only", Modes: []retrieval.Mode{retrieval.ModeTree},
	})
	if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("unseeded tree request error = %v", err)
	}
	if documentsCalls != 0 {
		t.Fatalf("unseeded request scanned corpus %d times", documentsCalls)
	}
}

func TestMaliciousRetrievalContentFailsClosed(t *testing.T) {
	f := newFixture(t)
	visible, err := f.clientA.Ingest(f.admin(t), explorer.Source{
		URI:       "file:///retrieval-validation.md",
		Title:     "Alpha beta evidence",
		MediaType: explorer.MediaTypeMarkdown,
		Content:   "# Evidence\n\nalpha beta canonical span\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	request := retrieval.Request{
		Text: "alpha beta",
		Modes: []retrieval.Mode{
			retrieval.ModeLexical,
			retrieval.ModeTree,
			retrieval.ModeGraph,
		},
		Explain: true,
	}
	if _, err := f.clientA.Retrieve(f.alice(t), request); err != nil {
		t.Fatalf("valid reconstructed response: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*retrieval.Response)
	}{
		{
			name: "missing canonical result",
			mutate: func(response *retrieval.Response) {
				response.Results = nil
			},
		},
		{
			name: "quote",
			mutate: func(response *retrieval.Response) {
				response.Results[0].Evidence[0].Quote = "altered quote"
			},
		},
		{
			name: "range",
			mutate: func(response *retrieval.Response) {
				response.Results[0].Evidence[0].Citation.Range.End.Offset--
			},
		},
		{
			name: "node property",
			mutate: func(response *retrieval.Response) {
				node := &response.Results[0].Evidence[0].Path.Nodes[0]
				node.Properties = cloneMetadata(node.Properties)
				node.Properties["title"] = "altered title"
			},
		},
		{
			name: "edge property",
			mutate: func(response *retrieval.Response) {
				edge := &response.Results[0].Evidence[0].Path.Edges[0]
				edge.Properties = cloneMetadata(edge.Properties)
				edge.Properties["altered"] = "property"
			},
		},
		{
			name: "explanation summary",
			mutate: func(response *retrieval.Response) {
				response.Results[0].Explanation.Summary = "untrusted summary"
			},
		},
		{
			name: "result score",
			mutate: func(response *retrieval.Response) {
				score := response.Results[0].Score
				response.Results[0].Score = shoal.Score(math.Nextafter(
					float64(score), math.Inf(1)))
			},
		},
		{
			name: "explanation score",
			mutate: func(response *retrieval.Response) {
				scores := response.Results[0].Explanation.Scores
				value := scores[string(retrieval.ModeLexical)]
				scores[string(retrieval.ModeLexical)] = shoal.Score(math.Nextafter(
					float64(value), math.Inf(1)))
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response, err := f.base.Retrieve(
				context.Background(),
				retrieval.Request{
					Text:    request.Text,
					Modes:   append([]retrieval.Mode(nil), request.Modes...),
					Explain: true,
					Scope: retrieval.Scope{DocumentIDs: []shoal.ID{
						visible.Document.ID,
					}},
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(&response)
			malicious := &hookClient{
				Client: f.base,
				retrieve: func(
					context.Context,
					retrieval.Request,
				) (retrieval.Response, error) {
					return response, nil
				},
			}
			client := f.newClient(
				t, malicious, f.store, f.sourceA, f.policyA, nil)
			_, err = client.Retrieve(f.alice(t), request)
			if !shoal.IsErrorCode(err, shoal.ErrorInternal) {
				t.Fatalf("malicious response error = %v", err)
			}
		})
	}
}

func TestDocumentsReconstructCanonicalSummaries(t *testing.T) {
	f := newFixture(t)
	result, err := f.clientA.Ingest(f.admin(t), explorer.Source{
		URI: "file:///canonical-summary.txt", Title: "Canonical title",
		MediaType: explorer.MediaTypeText, Content: "canonical summary",
	})
	if err != nil {
		t.Fatal(err)
	}
	malicious := &hookClient{
		Client: f.base,
		documents: func(ctx context.Context) ([]explorer.DocumentSummary, error) {
			summaries, err := f.base.Documents(ctx)
			if err == nil {
				summaries[0].Document.Title = "altered title"
				summaries[0].Document.Metadata = shoal.Metadata{"leak": "value"}
				summaries[0].SourceURI = "file:///altered-secret.txt"
				summaries[0].SourceMediaType = "application/x-altered"
			}
			return summaries, err
		},
	}
	client := f.newClient(t, malicious, f.store, f.sourceA, f.policyA, nil)
	summaries, err := client.Documents(f.alice(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 ||
		summaries[0].Document.ID != result.Document.ID ||
		summaries[0].Document.Title != "Canonical title" ||
		summaries[0].SourceURI != "file:///canonical-summary.txt" ||
		summaries[0].SourceMediaType != explorer.MediaTypeText ||
		summaries[0].Document.Metadata["leak"] != "" {
		t.Fatalf("authorized summaries = %#v", summaries)
	}
}

func TestDocumentRejectsAlteredSourceMediaType(t *testing.T) {
	f := newFixture(t)
	result, err := f.clientA.Ingest(f.admin(t), explorer.Source{
		URI: "file:///media-type-integrity.go", MediaType: explorer.MediaTypeSource,
		Content: "package integrity\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	malicious := &hookClient{
		Client: f.base,
		document: func(
			ctx context.Context,
			documentID, revisionID shoal.ID,
		) (explorer.DocumentView, error) {
			view, err := f.base.Document(ctx, documentID, revisionID)
			if err == nil {
				view.SourceMediaType = "application/x-altered"
			}
			return view, err
		},
	}
	client := f.newClient(t, malicious, f.store, f.sourceA, f.policyA, nil)
	_, err = client.Document(f.alice(t), result.Document.ID, result.Revision.ID)
	if !shoal.IsErrorCode(err, shoal.ErrorInternal) {
		t.Fatalf("altered media type error = %v", err)
	}
}

func TestIngestReconstructsCanonicalResult(t *testing.T) {
	f := newFixture(t)
	malicious := &ingestOverrideClient{
		Client: f.base,
		ingest: func(
			ctx context.Context,
			source explorer.Source,
		) (explorer.IngestResult, error) {
			result, err := f.base.Ingest(ctx, source)
			if err == nil {
				result.Document.Title = "altered title"
				result.SectionCount = -1
				result.SpanCount = -1
			}
			return result, err
		},
	}
	client := f.newClient(t, malicious, f.store, f.sourceA, f.policyA, nil)
	result, err := client.Ingest(f.admin(t), explorer.Source{
		URI: "file:///canonical-ingest.txt", Title: "Canonical ingest",
		MediaType: explorer.MediaTypeText, Content: "one canonical span",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Document.Title != "Canonical ingest" ||
		result.SectionCount != 1 || result.SpanCount != 1 {
		t.Fatalf("authorized ingest result = %#v", result)
	}
}

func TestRetrievalHydratesAndVerifiesCatalogedContentBeforeScoring(
	t *testing.T,
) {
	f := newFixture(t)
	if _, err := f.clientA.Ingest(f.admin(t), explorer.Source{
		URI: "file:///hydrate-before-score.txt", MediaType: explorer.MediaTypeText,
		Content: "hydrate canonical content",
	}); err != nil {
		t.Fatal(err)
	}
	retrieveCalls := 0
	malicious := &hookClient{
		Client: f.base,
		document: func(
			ctx context.Context,
			documentID, revisionID shoal.ID,
		) (explorer.DocumentView, error) {
			view, err := f.base.Document(ctx, documentID, revisionID)
			if err == nil {
				view.Document.Title = "altered before scoring"
			}
			return view, err
		},
		retrieve: func(
			ctx context.Context,
			request retrieval.Request,
		) (retrieval.Response, error) {
			retrieveCalls++
			return f.base.Retrieve(ctx, request)
		},
	}
	client := f.newClient(
		t, malicious, f.store, f.sourceA, f.policyA, nil)
	_, err := client.Retrieve(
		f.alice(t), retrieval.Request{Text: "hydrate"})
	if !shoal.IsErrorCode(err, shoal.ErrorInternal) {
		t.Fatalf("hydration mismatch error = %v", err)
	}
	if retrieveCalls != 0 {
		t.Fatalf("base retrieve calls = %d", retrieveCalls)
	}
}

func TestRestartWithReusedPolicyStore(t *testing.T) {
	dataDir := t.TempDir()
	base, err := explorer.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 26, 19, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: now}
	authority, err := auth.NewAuthorityWithClock(clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	reader := &generationReader{generations: map[string]int64{"restart-domain": 1}}
	store := authorized.NewMemoryPolicyStore()
	selector, err := authorized.NewStaticPolicySelector(
		[]byte("restart-source"), []byte("restart-policy"))
	if err != nil {
		t.Fatal(err)
	}
	newClient := func(base explorer.Client) *authorized.Client {
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
	client := newClient(base)
	result, err := client.Ingest(ctx, explorer.Source{
		URI: "file:///restart.txt", MediaType: explorer.MediaTypeText,
		Content: "survives wrapped client restart",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := base.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := explorer.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restarted := newClient(reopened)
	if _, err := restarted.Document(
		ctx, result.Document.ID, result.Revision.ID); err != nil {
		t.Fatal(err)
	}
	summaries, err := restarted.Documents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 {
		t.Fatalf("restarted summaries = %#v", summaries)
	}
}

func TestConcurrentAuthorizedAccess(t *testing.T) {
	f := newFixture(t)
	result, err := f.clientA.Ingest(f.admin(t), explorer.Source{
		URI: "file:///concurrent.txt", MediaType: explorer.MediaTypeText,
		Content: "concurrent authorized access",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := f.alice(t)
	var wait sync.WaitGroup
	errorsSeen := make(chan error, 96)
	for index := 0; index < 32; index++ {
		wait.Add(3)
		go func() {
			defer wait.Done()
			_, err := f.clientA.Documents(ctx)
			errorsSeen <- err
		}()
		go func() {
			defer wait.Done()
			_, err := f.clientA.Document(
				ctx, result.Document.ID, result.Revision.ID)
			errorsSeen <- err
		}()
		go func() {
			defer wait.Done()
			_, err := f.clientA.Retrieve(ctx, retrieval.Request{
				Text: "concurrent",
			})
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

type failStore struct {
	authorized.PolicyStore
	mu               sync.Mutex
	revisionFailures int
	edgeFailures     int
}

type failFinalizeStore struct {
	authorized.PolicyStore
	commitErr   error
	pendErr     error
	rollbackErr error
}

func (s *failFinalizeStore) CommitSourceClaim(
	ctx context.Context,
	claim authorized.SourcePolicyClaim,
) error {
	if s.commitErr != nil {
		return s.commitErr
	}
	return s.PolicyStore.CommitSourceClaim(ctx, claim)
}

func (s *failFinalizeStore) PendSourceClaim(
	ctx context.Context,
	claim authorized.SourcePolicyClaim,
) error {
	if s.pendErr != nil {
		return s.pendErr
	}
	return s.PolicyStore.PendSourceClaim(ctx, claim)
}

func (s *failFinalizeStore) RollbackSourceClaim(
	ctx context.Context,
	claim authorized.SourcePolicyClaim,
) error {
	if s.rollbackErr != nil {
		return s.rollbackErr
	}
	return s.PolicyStore.RollbackSourceClaim(ctx, claim)
}

func (s *failStore) PutRevision(
	ctx context.Context,
	registration authorized.RevisionRegistration,
) error {
	s.mu.Lock()
	if s.revisionFailures > 0 {
		s.revisionFailures--
		s.mu.Unlock()
		return errors.New("catalog unavailable")
	}
	s.mu.Unlock()
	return s.PolicyStore.PutRevision(ctx, registration)
}

func (s *failStore) PutEdge(
	ctx context.Context,
	registration authorized.EdgeRegistration,
) error {
	s.mu.Lock()
	if s.edgeFailures > 0 {
		s.edgeFailures--
		s.mu.Unlock()
		return errors.New("catalog unavailable")
	}
	s.mu.Unlock()
	return s.PolicyStore.PutEdge(ctx, registration)
}

type hookClient struct {
	explorer.Client
	afterIngest func()
	connect     func(context.Context, graph.Edge) error
	retrieve    func(context.Context, retrieval.Request) (retrieval.Response, error)
	documents   func(context.Context) ([]explorer.DocumentSummary, error)
	document    func(
		context.Context, shoal.ID, shoal.ID,
	) (explorer.DocumentView, error)
}

type maliciousVectorClient struct {
	*hookClient
	scoreCalls int
}

func (c *maliciousVectorClient) VectorScores(
	context.Context,
	explorer.VectorScoreRequest,
) (map[shoal.ID]shoal.Score, error) {
	c.scoreCalls++
	return map[shoal.ID]shoal.Score{}, nil
}

type countingVectorScorer struct {
	authorized.VectorScorer
	calls int
}

func (c *countingVectorScorer) VectorScores(
	ctx context.Context,
	request explorer.VectorScoreRequest,
) (map[shoal.ID]shoal.Score, error) {
	c.calls++
	return c.VectorScorer.VectorScores(ctx, request)
}

func (c *hookClient) Connect(ctx context.Context, edge graph.Edge) error {
	if c.connect != nil {
		return c.connect(ctx, edge)
	}
	return c.Client.Connect(ctx, edge)
}

func (c *hookClient) Documents(
	ctx context.Context,
) ([]explorer.DocumentSummary, error) {
	if c.documents != nil {
		return c.documents(ctx)
	}
	return c.Client.Documents(ctx)
}

type ingestOverrideClient struct {
	explorer.Client
	ingest func(
		context.Context, explorer.Source,
	) (explorer.IngestResult, error)
}

func (c *ingestOverrideClient) Ingest(
	ctx context.Context,
	source explorer.Source,
) (explorer.IngestResult, error) {
	return c.ingest(ctx, source)
}

func (c *hookClient) Ingest(
	ctx context.Context,
	source explorer.Source,
) (explorer.IngestResult, error) {
	result, err := c.Client.Ingest(ctx, source)
	if c.afterIngest != nil {
		c.afterIngest()
	}
	return result, err
}

func (c *hookClient) Retrieve(
	ctx context.Context,
	request retrieval.Request,
) (retrieval.Response, error) {
	if c.retrieve != nil {
		return c.retrieve(ctx, request)
	}
	return c.Client.Retrieve(ctx, request)
}

func (c *hookClient) Document(
	ctx context.Context,
	documentID, revisionID shoal.ID,
) (explorer.DocumentView, error) {
	if c.document != nil {
		return c.document(ctx, documentID, revisionID)
	}
	return c.Client.Document(ctx, documentID, revisionID)
}

type resolverFunc func(context.Context) (auth.Decision, error)

func (f resolverFunc) Resolve(ctx context.Context) (auth.Decision, error) {
	return f(ctx)
}

type countingClient struct {
	explorer.Client
	mu           sync.Mutex
	ingestCount  int
	connectCount int
}

func (c *countingClient) Ingest(
	ctx context.Context,
	source explorer.Source,
) (explorer.IngestResult, error) {
	c.mu.Lock()
	c.ingestCount++
	c.mu.Unlock()
	return c.Client.Ingest(ctx, source)
}

func (c *countingClient) Connect(ctx context.Context, edge graph.Edge) error {
	c.mu.Lock()
	c.connectCount++
	c.mu.Unlock()
	return c.Client.Connect(ctx, edge)
}

func (c *countingClient) IngestCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ingestCount
}

func (c *countingClient) ConnectCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connectCount
}

func documentError(
	client explorer.Client,
	ctx context.Context,
	documentID, revisionID shoal.ID,
) error {
	_, err := client.Document(ctx, documentID, revisionID)
	return err
}

func firstSpanID(t *testing.T, view explorer.DocumentView) shoal.ID {
	t.Helper()
	var visit func(explorer.SectionView) shoal.ID
	visit = func(section explorer.SectionView) shoal.ID {
		if len(section.Spans) > 0 {
			return section.Spans[0].ID
		}
		for _, child := range section.Children {
			if id := visit(child); id != "" {
				return id
			}
		}
		return ""
	}
	id := visit(view.Root)
	if id == "" {
		t.Fatal("document has no span")
	}
	return id
}

func cloneMetadata(value shoal.Metadata) shoal.Metadata {
	cloned := make(shoal.Metadata, len(value))
	for key, item := range value {
		cloned[key] = item
	}
	return cloned
}

func TestReingestCannotSeizeAnotherPolicysDocument(t *testing.T) {
	f := newFixture(t)
	const uri = "file:///contested.txt"
	owned, err := f.clientA.Ingest(f.admin(t), explorer.Source{
		URI:       uri,
		Title:     "Owned",
		MediaType: explorer.MediaTypeText,
		Content:   "alpha owned content",
	})
	if err != nil {
		t.Fatal(err)
	}

	// A caller who cannot see the existing document must not learn that the
	// source URI is taken: the refusal matches an absent document exactly.
	_, seizeErr := f.clientB.Ingest(f.bob(t), explorer.Source{
		URI:       uri,
		Title:     "Seized",
		MediaType: explorer.MediaTypeText,
		Content:   "beta seized content",
	})
	absentErr := documentError(f.clientB, f.bob(t), owned.Document.ID, owned.Revision.ID)
	if seizeErr == nil || seizeErr.Error() != absentErr.Error() ||
		!shoal.IsErrorCode(seizeErr, shoal.ErrorNotFound) {
		t.Fatalf("hidden seizure error %v differs from absent %v", seizeErr, absentErr)
	}

	// A caller who can read the document may reclassify it, because that stays
	// within grants the caller already holds.
	if _, err := f.clientB.Ingest(f.admin(t), explorer.Source{
		URI:       uri,
		Title:     "Reclassified",
		MediaType: explorer.MediaTypeText,
		Content:   "gamma reclassified content",
	}); err != nil {
		t.Fatal(err)
	}
	// Restore policy A ownership for the remaining assertions.
	owned, err = f.clientA.Ingest(f.admin(t), explorer.Source{
		URI:       uri,
		Title:     "Owned",
		MediaType: explorer.MediaTypeText,
		Content:   "alpha owned content again",
	})
	if err != nil {
		t.Fatal(err)
	}

	// The owner keeps the document, its revision, and its rule.
	summaries, err := f.clientA.Documents(f.alice(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 ||
		summaries[0].Document.ID != owned.Document.ID ||
		summaries[0].Revision.ID != owned.Revision.ID {
		t.Fatalf("owner lost the contested document: %#v", summaries)
	}
	if _, err := f.clientB.Documents(f.bob(t)); err != nil {
		t.Fatal(err)
	}

	// The rightful owner may still publish a new revision.
	updated, err := f.clientA.Ingest(f.admin(t), explorer.Source{
		URI:       uri,
		Title:     "Owned",
		MediaType: explorer.MediaTypeText,
		Content:   "alpha owned content revised",
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Document.ID != owned.Document.ID ||
		updated.Revision.ID == owned.Revision.ID {
		t.Fatalf("owner revision = %#v", updated)
	}

	// Logical ownership is epoch independent, so a physical generation change
	// does not block the owner's next revision.
	f.reader.Set(f.domain, 2)
	newContext := f.context(t, f.decisionAtGeneration(
		t,
		"admin-generation-two",
		[][]byte{f.sourceA, f.sourceB},
		[][]byte{f.policyA, f.policyB},
		allOperations,
		2,
	))
	if _, err := f.clientA.Ingest(newContext, explorer.Source{
		URI:       uri,
		Title:     "Owned",
		MediaType: explorer.MediaTypeText,
		Content:   "alpha owned content revised again",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestTransientIndeterminateCommitDoesNotRetireSourceURI(t *testing.T) {
	f := newFixture(t)
	const uri = "file:///transient-indeterminate.txt"
	if _, err := f.clientA.Ingest(f.admin(t), explorer.Source{
		URI: uri, MediaType: explorer.MediaTypeText,
		Content: "original policy A revision",
	}); err != nil {
		t.Fatal(err)
	}
	failed := false
	flaky := &ingestOverrideClient{
		Client: f.base,
		ingest: func(
			ctx context.Context,
			source explorer.Source,
		) (explorer.IngestResult, error) {
			if failed {
				return f.base.Ingest(ctx, source)
			}
			failed = true
			return explorer.IngestResult{}, explorer.MarkIndeterminateCommit(
				shoal.NewError(
					shoal.ErrorUnavailable, "transient storage blip"),
			)
		},
	}
	owner := f.newClient(t, flaky, f.store, f.sourceA, f.policyA, nil)
	if _, err := owner.Ingest(f.admin(t), explorer.Source{
		URI: uri, MediaType: explorer.MediaTypeText,
		Content: "policy A revision two",
	}); !explorer.IsIndeterminateCommit(err) {
		t.Fatalf("indeterminate commit error = %v", err)
	}

	// A transient ambiguous write must not retire the source URI: once the
	// base recovers, the authorized owner can still publish.
	result, err := owner.Ingest(f.admin(t), explorer.Source{
		URI: uri, MediaType: explorer.MediaTypeText,
		Content: "policy A revision three",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Disposition != explorer.IngestApplied {
		t.Fatalf("recovered ingest = %#v", result)
	}
	summaries, err := f.clientA.Documents(f.alice(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 ||
		summaries[0].Revision.ID != result.Revision.ID {
		t.Fatalf("recovered document = %#v", summaries)
	}

	// Recovery must not widen access: an unrelated policy still cannot take
	// the URI, and stays indistinguishable from an absent document.
	if _, err := f.clientB.Ingest(f.bob(t), explorer.Source{
		URI: uri, MediaType: explorer.MediaTypeText,
		Content: "policy B seizure attempt",
	}); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("post-recovery seizure error = %v", err)
	}
}
