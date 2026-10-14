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

package harness

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestCachedGeneratorHitsAndMissesBySafeIdentity(t *testing.T) {
	cache, err := NewMemoryCache(MemoryCacheConfig{MaxEntries: 16, MaxBytes: 1 << 20, MaxEntryBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	pack, _, _ := fixture(t)
	runner := &countingStopRunner{}
	g := cachedGenerator(t, runner, pack, budgets(), nil, cache)
	first, err := g.Generate(context.Background(), pack)
	if err != nil {
		t.Fatal(err)
	}
	second, err := g.Generate(context.Background(), pack)
	if err != nil {
		t.Fatal(err)
	}
	if runner.starts != 1 {
		t.Fatalf("runner starts = %d, want one cache miss", runner.starts)
	}
	if first.ID() != second.ID() {
		t.Fatal("cached result identity changed")
	}

	tests := []struct {
		name       string
		pack       inference.ContextPack
		budgets    Budgets
		provider   string
		modelName  string
		toolPolicy string
	}{
		{name: "snapshot", pack: packWithSnapshot(t, pack, "other-snapshot", pack.Snapshot().AsOf()), budgets: budgets()},
		{name: "snapshot-as-of", pack: packWithSnapshot(t, pack, pack.Snapshot().ID(), pack.Snapshot().AsOf().Add(time.Second)), budgets: budgets()},
		{name: "authorization", pack: packWithAuth(t, pack, "other-auth"), budgets: budgets()},
		{name: "provider", pack: pack, budgets: budgets(), provider: "other-provider"},
		{name: "model", pack: pack, budgets: budgets(), modelName: "other-model"},
		{name: "budgets", pack: pack, budgets: budgetsWithSteps(7)},
		{name: "context", pack: packWithQuery(t, pack, "answer a different question"), budgets: budgets()},
		{name: "tool-policy", pack: pack, budgets: budgets(), toolPolicy: "other-tools-v1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := runner.starts
			g := cachedGeneratorWithIdentity(t, runner, tc.pack, tc.budgets, tc.provider, tc.modelName, tc.toolPolicy, cache)
			if _, err := g.Generate(context.Background(), tc.pack); err != nil {
				t.Fatal(err)
			}
			if runner.starts != before+1 {
				t.Fatalf("identity difference reused cache; starts %d -> %d", before, runner.starts)
			}
		})
	}
}

func TestCacheRejectsUnsafeSecretMaterial(t *testing.T) {
	cache, err := NewMemoryCache(MemoryCacheConfig{MaxEntries: 4, MaxBytes: 1 << 20, MaxEntryBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	pack, _, _ := fixture(t)
	runner := &countingStopRunner{}
	modelName := "deterministic"
	model, prompt := provenanceParts(t)
	model, err = inference.NewModelProvenance(
		model.Provider(), modelName, model.Version(),
		shoal.Metadata{"api_key": "super-secret-token"}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	provenance, err := NewProvenance("fake-harness", model, prompt, "grounded-tools-v1")
	if err != nil {
		t.Fatal(err)
	}
	g, err := NewCachedGenerator(runner, &fakeTools{pack: pack}, budgets(), provenance, &captureRecorder{}, cache)
	if err != nil {
		t.Fatal(err)
	}
	g.now = func() time.Time { return fixedTime }
	if _, err := g.Generate(context.Background(), pack); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Generate(context.Background(), pack); err != nil {
		t.Fatal(err)
	}
	if runner.starts != 2 {
		t.Fatalf("unsafe identity was cached; starts = %d", runner.starts)
	}
	if cache.Len() != 0 {
		t.Fatalf("unsafe cache entry stored: %d", cache.Len())
	}
	encoded := fmt.Sprintf("%+v", cache)
	if strings.Contains(encoded, "super-secret-token") {
		t.Fatal("secret material entered cache storage")
	}

	cache, _ = NewMemoryCache(MemoryCacheConfig{MaxEntries: 4, MaxBytes: 1 << 20, MaxEntryBytes: 1 << 20})
	secretRunner := &secretActionRunner{}
	g = cachedGenerator(t, secretRunner, pack, budgets(), nil, cache)
	if _, err := g.Generate(context.Background(), pack); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Generate(context.Background(), pack); err != nil {
		t.Fatal(err)
	}
	if secretRunner.starts != 2 || cache.Len() != 0 {
		t.Fatalf("secret action/result material was cached; starts=%d entries=%d", secretRunner.starts, cache.Len())
	}
	custom := &recordingCache{}
	secretRunner = &secretActionRunner{}
	g = cachedGenerator(t, secretRunner, pack, budgets(), nil, custom)
	if _, err := g.Generate(context.Background(), pack); err != nil {
		t.Fatal(err)
	}
	if custom.puts != 0 {
		t.Fatalf("unsafe record reached custom cache: puts=%d", custom.puts)
	}
}

func TestMemoryCacheEvictionAndEntryBounds(t *testing.T) {
	pack, _, _ := fixture(t)
	secondPack := packWithQuery(t, pack, "second grounded question")
	cache, err := NewMemoryCache(MemoryCacheConfig{MaxEntries: 1, MaxBytes: 1 << 20, MaxEntryBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	runner := &countingStopRunner{}
	if _, err := cachedGenerator(t, runner, pack, budgets(), nil, cache).Generate(context.Background(), pack); err != nil {
		t.Fatal(err)
	}
	if _, err := cachedGenerator(t, runner, secondPack, budgets(), nil, cache).Generate(context.Background(), secondPack); err != nil {
		t.Fatal(err)
	}
	if _, err := cachedGenerator(t, runner, pack, budgets(), nil, cache).Generate(context.Background(), pack); err != nil {
		t.Fatal(err)
	}
	if runner.starts != 3 {
		t.Fatalf("deterministic LRU eviction failed; starts = %d", runner.starts)
	}
	if cache.Len() != 1 {
		t.Fatalf("cache entries = %d", cache.Len())
	}

	small, err := NewMemoryCache(MemoryCacheConfig{MaxEntries: 2, MaxBytes: 256, MaxEntryBytes: 256})
	if err != nil {
		t.Fatal(err)
	}
	runner = &countingStopRunner{}
	g := cachedGenerator(t, runner, pack, budgets(), nil, small)
	if _, err := g.Generate(context.Background(), pack); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Generate(context.Background(), pack); err != nil {
		t.Fatal(err)
	}
	if runner.starts != 2 || small.Len() != 0 {
		t.Fatalf("oversized entry was cached; starts=%d entries=%d", runner.starts, small.Len())
	}
}

func TestCacheHitRechecksAuthorizationBeforeReturn(t *testing.T) {
	cache, err := NewMemoryCache(MemoryCacheConfig{MaxEntries: 4, MaxBytes: 1 << 20, MaxEntryBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	pack, _, _ := fixture(t)
	runner := &countingStopRunner{}
	g := cachedGenerator(t, runner, pack, budgets(), nil, cache)
	if _, err := g.Generate(context.Background(), pack); err != nil {
		t.Fatal(err)
	}
	calls := 0
	g.now = func() time.Time {
		calls++
		if calls >= 4 {
			return pack.Authorization().ExpiresAt()
		}
		return fixedTime
	}
	if _, err := g.Run(context.Background(), pack); !errors.Is(err, ErrInvalid) {
		t.Fatalf("cache hit after authorization expiry error = %v", err)
	}
	if runner.starts != 1 {
		t.Fatalf("cache hit should not restart runner; starts=%d", runner.starts)
	}
}

func TestCacheWriteRechecksAuthorizationBeforeReturn(t *testing.T) {
	cache := &expiringPutCache{}
	pack, _, _ := fixture(t)
	runner := &countingStopRunner{}
	g := cachedGenerator(t, runner, pack, budgets(), nil, cache)
	g.now = func() time.Time {
		if cache.puts > 0 {
			return pack.Authorization().ExpiresAt()
		}
		return fixedTime
	}
	if _, err := g.Run(context.Background(), pack); !errors.Is(err, ErrInvalid) {
		t.Fatalf("cache write after authorization expiry error = %v", err)
	}
	if cache.puts != 1 {
		t.Fatalf("cache writes = %d, want 1", cache.puts)
	}
}

func TestCacheKeyRejectsUnsetIdentity(t *testing.T) {
	if err := (CacheKey{}).Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unset key error = %v", err)
	}
}

type countingStopRunner struct {
	starts int
}

func (r *countingStopRunner) Start(_ context.Context, request SessionRequest) (Session, error) {
	r.starts++
	return countingStopSession{request: request}, nil
}

func (r *countingStopRunner) CacheIdentity() (string, error) {
	return "counting-stop-runner-v1", nil
}

func (f *fakeTools) CacheIdentity() (string, error) {
	return "fake-tools-v1", nil
}

type countingStopSession struct {
	request SessionRequest
}

type secretActionRunner struct{ starts int }

func (r *secretActionRunner) Start(context.Context, SessionRequest) (Session, error) {
	r.starts++
	return &secretActionSession{request: r.starts}, nil
}

func (r *secretActionRunner) CacheIdentity() (string, error) { return "unsafe-output-runner-v1", nil }

type recordingCache struct{ puts int }

func (c *recordingCache) Get(context.Context, CacheKey) (Record, bool, error) {
	return Record{}, false, nil
}

func (c *recordingCache) Put(context.Context, CacheKey, Record) error {
	c.puts++
	return nil
}

type expiringPutCache struct{ puts int }

func (c *expiringPutCache) Get(context.Context, CacheKey) (Record, bool, error) {
	return Record{}, false, nil
}

func (c *expiringPutCache) Put(context.Context, CacheKey, Record) error {
	c.puts++
	return nil
}

type secretActionSession struct {
	request int
	step    int
}

func (s *secretActionSession) Next(_ context.Context, transcript Transcript) (Action, error) {
	s.step++
	if s.step == 1 {
		request, err := NewRetrieveRequest("client_secret=redacted", 1)
		if err != nil {
			return Action{}, err
		}
		return NewRetrieveAction("secret-retrieve", request, Usage{})
	}
	value, _ := ontology.NewStringValue("secret generated value")
	model, err := inference.NewModelProvenance("fake-provider", "fake-model", "v1", nil, nil)
	if err != nil {
		return Action{}, err
	}
	prompt, err := inference.NewPromptProvenance("agent", "v1", "sha256:"+strings.Repeat("a", 64))
	if err != nil {
		return Action{}, err
	}
	claim, err := inference.NewClaim(
		"subject", "predicate", value, 1, []shoal.ID{transcript.Context().Evidence()[0].ID()},
		inference.ClaimInferred, model, prompt, nil,
	)
	if err != nil {
		return Action{}, err
	}
	result, err := inference.NewInferenceResult(transcript.Context(), []inference.Claim{claim}, nil, fixedTime, nil)
	if err != nil {
		return Action{}, err
	}
	return NewStopAction("secret-stop", result, Usage{})
}

func (s countingStopSession) Next(_ context.Context, transcript Transcript) (Action, error) {
	evidence := transcript.Context().Evidence()[0]
	result, err := resultForProvenance(transcript.Context(), s.request.provenance, evidence)
	if err != nil {
		return Action{}, err
	}
	return NewStopAction(shoal.ID("stop-"+strconvID(s.request.id)), result, Usage{InputTokens: 1, OutputTokens: 1})
}

func cachedGenerator(t *testing.T, runner Runner, pack inference.ContextPack, b Budgets, modelName *string, cache Cache) *Generator {
	name := ""
	if modelName != nil {
		name = *modelName
	}
	return cachedGeneratorWithIdentity(t, runner, pack, b, "", name, "", cache)
}

func cachedGeneratorWithIdentity(t *testing.T, runner Runner, pack inference.ContextPack, b Budgets, providerName, modelName, toolPolicy string, cache Cache) *Generator {
	t.Helper()
	model, prompt := provenanceParts(t)
	if providerName == "" {
		providerName = model.Provider()
	}
	if modelName == "" {
		modelName = model.Model()
	}
	var err error
	model, err = inference.NewModelProvenance(providerName, modelName, model.Version(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if toolPolicy == "" {
		toolPolicy = "grounded-tools-v1"
	}
	provenance, err := NewProvenance("fake-harness", model, prompt, toolPolicy)
	if err != nil {
		t.Fatal(err)
	}
	g, err := NewCachedGenerator(runner, &fakeTools{pack: pack}, b, provenance, &captureRecorder{}, cache)
	if err != nil {
		t.Fatal(err)
	}
	g.now = func() time.Time { return fixedTime }
	return g
}

func resultForProvenance(pack inference.ContextPack, provenance Provenance, evidence inference.EvidenceAnchor) (inference.InferenceResult, error) {
	value, _ := ontology.NewStringValue("grounded")
	claim, err := inference.NewClaim(
		"subject", "predicate", value, 1, []shoal.ID{evidence.ID()},
		inference.ClaimInferred, provenance.model, provenance.prompt, nil,
	)
	if err != nil {
		return inference.InferenceResult{}, err
	}
	return inference.NewInferenceResult(pack, []inference.Claim{claim}, nil, fixedTime, nil)
}

func packWithSnapshot(t *testing.T, pack inference.ContextPack, id shoal.ID, asOf time.Time) inference.ContextPack {
	t.Helper()
	snapshot, err := inference.NewSnapshotPin(id, asOf)
	if err != nil {
		t.Fatal(err)
	}
	return repack(t, pack.Query(), pack.Evidence(), snapshot, pack.Authorization(), pack.Metadata())
}

func packWithAuth(t *testing.T, pack inference.ContextPack, fingerprint shoal.ID) inference.ContextPack {
	t.Helper()
	auth, err := inference.NewAuthPin(fingerprint, pack.Authorization().ExpiresAt())
	if err != nil {
		t.Fatal(err)
	}
	return repack(t, pack.Query(), pack.Evidence(), pack.Snapshot(), auth, pack.Metadata())
}

func packWithQuery(t *testing.T, pack inference.ContextPack, query string) inference.ContextPack {
	t.Helper()
	return repack(t, query, pack.Evidence(), pack.Snapshot(), pack.Authorization(), pack.Metadata())
}

func repack(t *testing.T, query string, anchors []inference.EvidenceAnchor, snapshot inference.SnapshotPin, auth inference.AuthPin, metadata shoal.Metadata) inference.ContextPack {
	t.Helper()
	pack, err := inference.NewContextPack(query, anchors, nil, snapshot, auth, metadata)
	if err != nil {
		t.Fatal(err)
	}
	return pack
}

func budgetsWithSteps(steps int) Budgets {
	b := budgets()
	b.MaxSteps = steps
	return b
}

func strconvID(id shoal.ID) string {
	value := string(id)
	if len(value) > 16 {
		return value[:16]
	}
	return value
}
