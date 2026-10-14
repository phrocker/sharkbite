// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied. See the License for the
// specific language governing permissions and limitations
// under the License.

// Package atpltest builds a real fleet.Service over in-memory dependencies so
// pkg/atpl and shoalctl can test policy compilation against the registry's
// actual Register rather than a model of it. It is for tests only.
package atpltest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Domain is the authorization domain every test decision carries.
const Domain = "domain"

// Clock is a settable clock shared by the service and the authority.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

func NewClock(now time.Time) *Clock { return &Clock{now: now.UTC()} }

func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *Clock) Set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now.UTC()
}

// Executor is a host executor binding with an effect ceiling and floor.
type Executor struct {
	Max fleet.Effects
	Min fleet.Effects
}

func (e Executor) MaxEffects() fleet.Effects { return e.Max }
func (e Executor) MinEffects() fleet.Effects { return e.Min }

// Executors is a host executor registry.
type Executors map[string]fleet.Executor

func (e Executors) ResolveExecutor(ref string) (fleet.Executor, bool) {
	executor, ok := e[ref]
	return executor, ok
}

type trustAll Executors

func (t trustAll) Configured(ref string) bool {
	_, ok := t[ref]
	return ok
}

// Registry is a fleet.Service with everything it depends on held in memory.
type Registry struct {
	Service   *fleet.Service
	Authority *auth.Authority
	Store     *Store
	Recorder  *Recorder
	Clock     *Clock
	// Sources and Policies are the corpus grants every decision carries.
	Sources  []string
	Policies []string
}

// NewRegistry builds a registry whose decisions grant sources and policies.
func NewRegistry(t testing.TB, clock *Clock, executors Executors, sources, policies []string) *Registry {
	t.Helper()
	authority, err := auth.NewAuthorityWithClock(clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	registry := &Registry{
		Authority: authority, Store: NewStore(), Recorder: &Recorder{},
		Clock: clock, Sources: sources, Policies: policies,
	}
	registry.Service, err = fleet.NewService(fleet.Config{
		Store: registry.Store, Resolver: authority.Resolver(), Recorder: registry.Recorder,
		Snapshots: snapshot{clock}, Executors: executors, Clock: clock.Now,
		// Every host executor has an attestation trust root here, so a
		// policy requiring attestation registers as it would on a host
		// configured with -fleet-executor-attestation. The no-trust-root
		// refusal is pinned in pkg/explorer/fleet.
		AttestationTrust: trustAll(executors),
	})
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

// Decision returns a fresh decision for the test subject.
func (r *Registry) Decision(t testing.TB) auth.Decision {
	t.Helper()
	sources := make([][]byte, len(r.Sources))
	for i, source := range r.Sources {
		sources[i] = []byte(source)
	}
	policies := make([][]byte, len(r.Policies))
	for i, policy := range r.Policies {
		policies[i] = []byte(policy)
	}
	raw := make([]byte, 8)
	_, _ = rand.Read(raw)
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "owner", Actor: "owner-actor", AuthorizationDomain: []byte(Domain),
		AllowedOperations: []auth.Operation{
			auth.OperationAgentRegister, auth.OperationAgentHeartbeat,
			auth.OperationAgentRevoke, auth.OperationAgentResolve, auth.OperationDelegate,
		},
		PermittedSourceIDs: sources, PermittedPolicyIDs: policies,
		PolicyGeneration:      1,
		AuthenticationExpires: r.Clock.Now().Add(72 * time.Hour),
		RequestID:             shoal.ID("request-" + hex.EncodeToString(raw)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return decision
}

// Context binds a fresh decision and returns it with its request context.
func (r *Registry) Context(t testing.TB) (context.Context, fleet.RequestContext) {
	t.Helper()
	decision := r.Decision(t)
	ctx, err := r.Authority.Binder().Bind(context.Background(), decision)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, fleet.RequestContext{
		RequestID: decision.RequestID(), ReasonCode: "test",
		Deadline: r.Clock.Now().Add(time.Minute),
	}
}

// Register registers spec directly through the service.
func (r *Registry) Register(t testing.TB, spec fleet.Spec, expectedGeneration int64, key string) (fleet.Descriptor, error) {
	t.Helper()
	ctx, requestContext := r.Context(t)
	return r.Service.Register(ctx, fleet.RegisterRequest{
		Context: requestContext, RegistrationKey: shoal.ID(key),
		ExpectedGeneration: expectedGeneration, Spec: spec,
	})
}

// Live lists every registration the test subject can see, as plan and export
// read it.
func (r *Registry) Live(t testing.TB) map[shoal.ID]fleet.Descriptor {
	t.Helper()
	result := make(map[shoal.ID]fleet.Descriptor)
	var cursor []byte
	for {
		ctx, requestContext := r.Context(t)
		page, err := r.Service.List(ctx, fleet.ListRequest{
			Context: requestContext, Cursor: cursor, Limit: fleet.MaxListResults,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, descriptor := range page.Descriptors {
			result[descriptor.ID] = descriptor
		}
		if len(page.Next) == 0 {
			return result
		}
		cursor = page.Next
	}
}

// Handler serves the real fleet registry routes, authenticating a bearer
// token and binding a fresh decision per request, and taking the request
// identity from that decision as the hosted binary does.
func (r *Registry) Handler(t testing.TB, token string) http.Handler {
	t.Helper()
	registry, err := webapi.NewFleetRegistryHandler(boundRegistry{r})
	if err != nil {
		t.Fatal(err)
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+token {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusUnauthorized)
			_, _ = writer.Write([]byte(`{"code":"unauthorized","message":"authentication failed"}`))
			return
		}
		ctx, err := r.Authority.Binder().Bind(request.Context(), r.Decision(t))
		if err != nil {
			t.Error(err)
			return
		}
		registry.ServeHTTP(writer, request.WithContext(ctx))
	})
}

type boundRegistry struct{ registry *Registry }

func (b boundRegistry) bind(ctx context.Context, request fleet.RequestContext) (fleet.RequestContext, error) {
	decision, err := b.registry.Authority.Resolver().Resolve(ctx)
	if err != nil {
		return fleet.RequestContext{}, err
	}
	request.RequestID = decision.RequestID()
	request.CorrelationID = decision.CorrelationID()
	return request, nil
}

func (b boundRegistry) Register(ctx context.Context, request fleet.RegisterRequest) (fleet.Descriptor, error) {
	bound, err := b.bind(ctx, request.Context)
	if err != nil {
		return fleet.Descriptor{}, err
	}
	request.Context = bound
	return b.registry.Service.Register(ctx, request)
}

func (b boundRegistry) Heartbeat(ctx context.Context, request fleet.HeartbeatRequest) (fleet.Descriptor, error) {
	bound, err := b.bind(ctx, request.Context)
	if err != nil {
		return fleet.Descriptor{}, err
	}
	request.Context = bound
	return b.registry.Service.Heartbeat(ctx, request)
}

func (b boundRegistry) Revoke(ctx context.Context, request fleet.RevokeRequest) (fleet.Descriptor, error) {
	bound, err := b.bind(ctx, request.Context)
	if err != nil {
		return fleet.Descriptor{}, err
	}
	request.Context = bound
	return b.registry.Service.Revoke(ctx, request)
}

func (b boundRegistry) Resolve(ctx context.Context, request fleet.ResolveRequest) (fleet.Resolved, error) {
	bound, err := b.bind(ctx, request.Context)
	if err != nil {
		return fleet.Resolved{}, err
	}
	request.Context = bound
	return b.registry.Service.Resolve(ctx, request)
}

func (b boundRegistry) List(ctx context.Context, request fleet.ListRequest) (fleet.ListPage, error) {
	bound, err := b.bind(ctx, request.Context)
	if err != nil {
		return fleet.ListPage{}, err
	}
	request.Context = bound
	return b.registry.Service.List(ctx, request)
}

type snapshot struct{ clock *Clock }

func (s snapshot) InteractionSnapshot(context.Context) (explorer.Snapshot, error) {
	return explorer.Snapshot{ID: "snapshot", AsOf: s.clock.Now(), Frontier: 1}, nil
}

// Recorder keeps every lifecycle record.
type Recorder struct {
	mu      sync.Mutex
	records []fleet.Lifecycle
}

func (r *Recorder) RecordLifecycle(_ context.Context, lifecycle fleet.Lifecycle) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, lifecycle)
	return nil
}

// Records returns a copy of every lifecycle record so far.
func (r *Recorder) Records() []fleet.Lifecycle {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]fleet.Lifecycle(nil), r.records...)
}

// Store is an in-memory fleet.Store with the CAS and same-key replay semantics
// the interface requires.
type Store struct {
	mu      sync.Mutex
	records map[shoal.ID]fleet.Stored
}

func NewStore() *Store { return &Store{records: make(map[shoal.ID]fleet.Stored)} }

func digest(descriptor fleet.Descriptor) [32]byte {
	encoded, _ := json.Marshal(descriptor)
	return sha256.Sum256(encoded)
}

func (s *Store) Apply(_ context.Context, mutation fleet.Mutation) (fleet.Stored, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.records[mutation.Descriptor.ID]
	keyDigest := sha256.Sum256([]byte(mutation.RegistrationKey))
	if exists && current.Descriptor.Generation == mutation.ExpectedGeneration+1 &&
		current.RegistrationDigest == keyDigest && current.Digest == digest(mutation.Descriptor) {
		return current, nil
	}
	if (!exists && mutation.ExpectedGeneration != 0) ||
		(exists && current.Descriptor.Generation != mutation.ExpectedGeneration) {
		return fleet.Stored{}, shoal.NewError(shoal.ErrorConflict, "generation conflict")
	}
	stored := fleet.Stored{
		Descriptor: mutation.Descriptor, RegistrationDigest: keyDigest,
		Epoch: mutation.Descriptor.Generation, Digest: digest(mutation.Descriptor),
	}
	s.records[mutation.Descriptor.ID] = stored
	return stored, nil
}

func (s *Store) Get(_ context.Context, id shoal.ID) (fleet.Stored, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.records[id]
	if !ok {
		return fleet.Stored{}, shoal.NewError(shoal.ErrorNotFound, "not found")
	}
	return stored, nil
}

func (s *Store) List(_ context.Context, cursor []byte, limit int) (fleet.StoredPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.records))
	for id := range s.records {
		ids = append(ids, string(id))
	}
	sort.Strings(ids)
	page := fleet.StoredPage{}
	after := string(cursor)
	for _, id := range ids {
		if len(cursor) > 0 && strings.Compare(id, after) <= 0 {
			continue
		}
		if len(page.Entries) == limit {
			page.Next = []byte(page.Entries[len(page.Entries)-1].Descriptor.ID)
			break
		}
		page.Entries = append(page.Entries, s.records[shoal.ID(id)])
	}
	return page, nil
}
