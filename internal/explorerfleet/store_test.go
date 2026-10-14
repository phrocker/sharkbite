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

package explorerfleet

import (
	"context"
	"encoding/json"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/explorercoord"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/transaction"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestStoreRejectsGenerationOverflowBeforeRuntimeAccess(t *testing.T) {
	descriptor := testDescriptor("agent", math.MinInt64)
	_, err := (&Store{}).Apply(context.Background(), fleet.Mutation{
		RegistrationKey:    "overflow",
		ExpectedGeneration: math.MaxInt64,
		Descriptor:         descriptor,
	})
	if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("generation overflow = %v", err)
	}
}

func TestStoreCASReplayAndRevocation(t *testing.T) {
	directory := t.TempDir()
	runtime := openRuntime(t, directory)
	defer func() {
		if runtime != nil {
			_ = runtime.Close()
		}
	}()
	store, err := NewStore(runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	base := testDescriptor("agent-\x00-\xff", 1)
	create := fleet.Mutation{RegistrationKey: "create-key", Descriptor: base}
	first, err := store.Apply(context.Background(), create)
	if err != nil {
		t.Fatal(err)
	}
	replayed := base
	replayed.UpdatedAt = replayed.UpdatedAt.Add(time.Second)
	second, err := store.Apply(context.Background(), fleet.Mutation{
		RegistrationKey: "create-key", Descriptor: replayed,
	})
	if err != nil || second.Epoch != first.Epoch {
		t.Fatalf("identical replay = %#v, %v", second, err)
	}
	divergent := replayed
	divergent.ExecutorRef = "executor-b"
	if _, err := store.Apply(context.Background(), fleet.Mutation{
		RegistrationKey: "create-key", Descriptor: divergent,
	}); !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("divergent replay = %v", err)
	}

	left := base
	left.Generation = 2
	left.UpdatedAt = left.UpdatedAt.Add(2 * time.Second)
	left.LeaseExpiresAt = left.LeaseExpiresAt.Add(time.Minute)
	right := left
	right.ExecutorRef = "executor-b"
	var wait sync.WaitGroup
	results := make(chan error, 2)
	for _, mutation := range []fleet.Mutation{
		{RegistrationKey: "left", ExpectedGeneration: 1, Descriptor: left},
		{RegistrationKey: "right", ExpectedGeneration: 1, Descriptor: right},
	} {
		mutation := mutation
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, applyErr := store.Apply(context.Background(), mutation)
			results <- applyErr
		}()
	}
	wait.Wait()
	close(results)
	successes, conflicts := 0, 0
	for applyErr := range results {
		if applyErr == nil {
			successes++
		} else if shoal.IsErrorCode(applyErr, shoal.ErrorConflict) {
			conflicts++
		} else {
			t.Fatalf("competing mutation = %v", applyErr)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d", successes, conflicts)
	}
	current, err := store.Get(context.Background(), base.ID)
	if err != nil || current.Descriptor.Generation != 2 {
		t.Fatalf("current = %#v, %v", current, err)
	}
	if err := runtime.Recover(context.Background()); err != nil {
		t.Fatalf("recover after conflict = %v", err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	runtime = openRuntime(t, directory)
	store, _ = NewStore(runtime, nil)
	current, err = store.Get(context.Background(), base.ID)
	if err != nil || current.Descriptor.Generation != 2 {
		t.Fatalf("post-conflict restart = %#v, %v", current, err)
	}
	revoked := current.Descriptor
	revoked.Generation = 3
	revoked.UpdatedAt = revoked.UpdatedAt.Add(time.Second)
	revoked.RevokedAt = revoked.UpdatedAt
	if _, err := store.Apply(context.Background(), fleet.Mutation{
		RegistrationKey: "revoke", ExpectedGeneration: 2, Descriptor: revoked,
	}); err != nil {
		t.Fatal(err)
	}
	listed, err := store.List(context.Background(), nil, fleet.MaxListResults)
	if err != nil || len(listed.Entries) != 1 ||
		listed.Entries[0].Descriptor.ID != base.ID {
		t.Fatalf("listed = %#v, %v", listed, err)
	}
}

func TestStoreRestartPersistence(t *testing.T) {
	directory := t.TempDir()
	runtime := openRuntime(t, directory)
	store, _ := NewStore(runtime, nil)
	descriptor := testDescriptor("persistent-agent", 1)
	if _, err := store.Apply(context.Background(), fleet.Mutation{
		RegistrationKey: "persistent-create", Descriptor: descriptor,
	}); err != nil {
		t.Fatal(err)
	}

	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	runtime = openRuntime(t, directory)
	defer runtime.Close()
	store, _ = NewStore(runtime, nil)
	restarted, err := store.Get(context.Background(), descriptor.ID)
	if err != nil || restarted.Descriptor.Generation != 1 ||
		restarted.Descriptor.ID != descriptor.ID {
		t.Fatalf("restarted = %#v, %v", restarted, err)
	}
}

func TestStoreListUsesBoundedOpaqueCursor(t *testing.T) {
	runtime := openRuntime(t, t.TempDir())
	defer runtime.Close()
	store, err := NewStore(runtime, nil)
	if err != nil {
		t.Fatal(err)
	}

	for _, id := range []shoal.ID{"page-a", "page-b", "page-c"} {
		if _, err := store.Apply(context.Background(), fleet.Mutation{
			RegistrationKey: shoal.ID("key-" + string(id)),
			Descriptor:      testDescriptor(id, 1),
		}); err != nil {
			t.Fatal(err)
		}
	}

	seen := make(map[shoal.ID]bool)
	var cursor []byte
	for {
		page, err := store.List(context.Background(), cursor, 1)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range page.Entries {
			if seen[item.Descriptor.ID] {
				t.Fatalf("duplicate paged descriptor %q", item.Descriptor.ID)
			}
			seen[item.Descriptor.ID] = true
		}
		if len(page.Next) == 0 {
			break
		}
		cursor = page.Next
	}
	if len(seen) != 3 {
		t.Fatalf("paged descriptors = %v", seen)
	}
	if _, err := store.List(context.Background(), []byte{0}, 1); !shoal.IsErrorCode(
		err, shoal.ErrorInvalidArgument,
	) {
		t.Fatalf("invalid cursor error = %v", err)
	}
}

func TestNewStoreRejectsTypedNilRuntime(t *testing.T) {
	var runtime *explorercoord.Runtime
	if _, err := NewStore(runtime, nil); !shoal.IsErrorCode(
		err, shoal.ErrorInvalidArgument,
	) {
		t.Fatalf("typed-nil runtime error = %v", err)
	}
}

func openRuntime(t *testing.T, directory string) *explorercoord.Runtime {
	t.Helper()
	runtime, err := explorercoord.Open(explorercoord.Config{
		Directory: directory, Domain: coordination.DomainID("fleet-registry"),
		Owner: coordination.OwnerID("fleet-worker"),
		Authority: transaction.Authority{
			Generation: 1, Fence: 1, Holder: coordination.OwnerID("fleet-process"),
			Mode:                coordination.WriterModeEmbeddedPrimary,
			RetentionGeneration: 1, HistoryFloor: 1,
		},
		PhysicalTables: []string{PhysicalTable()}, Lease: time.Minute,
		RecoveryLimit: 16, RecoveryMaxPages: 64,
		RetryBackoff: time.Nanosecond, RecoveryBackoff: time.Nanosecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

func testDescriptor(id shoal.ID, generation int64) fleet.Descriptor {
	now := time.Date(2026, 9, 5, 20, 0, 0, 0, time.UTC)
	return fleet.Descriptor{
		ID: id, Generation: generation, Subject: "subject", Actor: "actor",
		AuthorizationDomain: []byte("domain"),
		Scopes:              []fleet.Scope{{SourceID: []byte("source"), PolicyID: []byte("policy")}},
		ExecutorRef:         "executor-a",
		Capabilities: []fleet.Capability{{
			Name: "search", Actions: []fleet.Action{{
				Name: "query", InputSchema: json.RawMessage(`{"type":"object"}`),
				OutputSchema: json.RawMessage(`{"type":"object"}`),
			}},
		}},
		LeaseExpiresAt: now.Add(time.Hour), UpdatedAt: now,
	}
}
