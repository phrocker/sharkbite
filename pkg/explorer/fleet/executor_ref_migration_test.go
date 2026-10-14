// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package fleet

import (
	"context"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// TestStoredDescriptorWithANowInvalidExecutorRefStillResolves: the shared
// executor-reference rule (#391) tightens registration only, like #544's
// floor. A descriptor stored before the rule, whose ref the rule now refuses,
// keeps resolving; the refusal comes at its next Register, where an operator
// renames the ref.
func TestStoredDescriptorWithANowInvalidExecutorRefStillResolves(t *testing.T) {
	const legacyRef = "exec legacy" // NBSP: accepted before, refused now
	now := time.Date(2026, 9, 6, 8, 0, 0, 0, time.UTC)
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	store := newMemoryStore()
	service, err := NewService(Config{
		Store: store, Resolver: authority.Resolver(),
		Recorder: &memoryRecorder{}, Snapshots: fixedSnapshot{now},
		Executors: executorMap{"exec": struct{}{}, legacyRef: struct{}{}},
		Clock:     func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	as := func(requestID string) context.Context {
		return bindDecision(t, authority, testDecision(
			t, "owner", "owner-actor", requestID, [][]byte{[]byte("source-a")}))
	}
	ctx := as("legacy")
	request := registerRequest(now, "legacy", "legacy-agent", "", "source-a")
	registered, err := service.Register(ctx, request)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate a descriptor written before the rule existed.
	store.mu.Lock()
	stored := store.records[registered.ID]
	stored.Descriptor.ExecutorRef = legacyRef
	stored.Digest = descriptorDigest(stored.Descriptor)
	store.records[registered.ID] = stored
	store.mu.Unlock()

	resolved, err := service.Resolve(as("legacy-resolve"), ResolveRequest{
		Context: requestContext(now, "legacy-resolve"), ID: registered.ID,
	})
	if err != nil {
		t.Fatalf("a stored legacy descriptor no longer resolves: %v", err)
	}
	if resolved.Descriptor.ExecutorRef != legacyRef {
		t.Fatalf("resolved ref = %q", resolved.Descriptor.ExecutorRef)
	}

	reRegister := registerRequest(now, "legacy-2", "legacy-agent", "", "source-a")
	reRegister.RegistrationKey = "key-legacy-agent-2"
	reRegister.ExpectedGeneration = registered.Generation
	reRegister.Spec.ExecutorRef = legacyRef
	if _, err := service.Register(as("legacy-2"), reRegister); !shoal.IsErrorCode(
		err, shoal.ErrorInvalidArgument) {
		t.Fatalf("re-registering a now-invalid ref = %v, want invalid_argument", err)
	}
	// Renaming the ref is the migration, and it is accepted.
	reRegister.Spec.ExecutorRef = "exec"
	if _, err := service.Register(as("legacy-2"), reRegister); err != nil {
		t.Fatalf("re-registering under a valid ref = %v", err)
	}
}
