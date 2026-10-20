// Licensed under the Apache Software Foundation, Version 2.0. See LICENSE and NOTICE.

package fleet

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type delayedRecoveryStore struct {
	DispatchStore
	afterGet   func()
	afterApply func()
}

type delayedRegistryStore struct {
	Store
	afterGet func()
}

func (s delayedRegistryStore) Get(ctx context.Context, id shoal.ID) (Stored, error) {
	stored, err := s.Store.Get(ctx, id)
	if s.afterGet != nil {
		s.afterGet()
	}
	return stored, err
}

func (s delayedRecoveryStore) GetAction(ctx context.Context, id []byte) (ActionRecord, error) {
	record, err := s.DispatchStore.GetAction(ctx, id)
	if s.afterGet != nil {
		s.afterGet()
	}
	return record, err
}

func (s delayedRecoveryStore) ApplyAction(ctx context.Context, mutation DispatchMutation) (ActionRecord, error) {
	record, err := s.DispatchStore.ApplyAction(ctx, mutation)
	if s.afterApply != nil {
		s.afterApply()
	}
	return record, err
}

func TestPreparedMaterialAndPrepareDoNotAcknowledgeAfterSlowStoreExpiresLease(t *testing.T) {
	material := newCompletionFixtureWithPrepared(t, Effects{EffectMutatesExternal}, &remoteBoundExecutor{}, true)
	material.service.store = delayedRecoveryStore{DispatchStore: material.store,
		afterGet: func() { *material.clock = material.clock.Add(2 * time.Minute) }}
	claim := RecoveryClaimRequest{ID: material.claimed.ID, ClaimID: []byte("claim"), ClaimFence: 1,
		Context: dispatchContext(*material.clock, "request")}
	if _, err := material.service.ClaimMaterial(material.ctx, claim); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("slow Material granted expired claim: %v", err)
	}

	prepare := newCompletionFixtureWithPrepared(t, Effects{EffectMutatesExternal}, &remoteBoundExecutor{}, true)
	request := PrepareClaimRequest{RecoveryClaimRequest: RecoveryClaimRequest{
		ID: prepare.claimed.ID, ClaimID: []byte("claim"), ClaimFence: 1,
		Context: dispatchContext(*prepare.clock, "request")},
		Mode: RecoveryKey, RequestDigest: [32]byte{1}, TargetScopeDigest: [32]byte{2}}
	prepare.service.store = delayedRecoveryStore{DispatchStore: prepare.store,
		afterApply: func() { *prepare.clock = prepare.clock.Add(2 * time.Minute) }}
	if _, err := prepare.service.PrepareClaim(prepare.ctx, request); !errors.Is(err, ErrActionCommitted) {
		t.Fatalf("slow committed Prepare returned send grant or unmarked error: %v", err)
	}
	stored, err := prepare.store.GetAction(prepare.ctx, prepare.claimed.ID)
	if err != nil || stored.RecoveryCommitment == nil {
		t.Fatalf("prepare was not durable before refusal: %#v, %v", stored, err)
	}
}

func TestPreparedMaterialRefusesDescriptorThatExpiresDuringRegistryRead(t *testing.T) {
	f := newCompletionFixtureWithPrepared(t, Effects{EffectMutatesExternal}, &remoteBoundExecutor{}, true)
	registryStore := f.service.registry.store.(*memoryStore)
	registryStore.mu.Lock()
	stored := registryStore.records[shoal.ID("agent")]
	stored.Descriptor.LeaseExpiresAt = f.clock.Add(30 * time.Second)
	stored.Digest = descriptorDigest(stored.Descriptor)
	registryStore.records[shoal.ID("agent")] = stored
	registryStore.mu.Unlock()
	advanced := false
	f.service.registry.store = delayedRegistryStore{Store: registryStore, afterGet: func() {
		if !advanced {
			*f.clock = f.clock.Add(40 * time.Second)
			advanced = true
		}
	}}
	request := RecoveryClaimRequest{ID: f.claimed.ID, ClaimID: []byte("claim"), ClaimFence: 1,
		Context: dispatchContext(*f.clock, "request")}
	if _, err := f.service.ClaimMaterial(f.ctx, request); err == nil {
		t.Fatal("Material exposed executable fields after descriptor expired during lookup")
	}
}

func TestPreparedFirstClaimRequiresMatchingLiveCommitment(t *testing.T) {
	f := newCompletionFixtureWithPrepared(t, Effects{EffectMutatesExternal}, &remoteBoundExecutor{}, true)
	if f.claimed.RecoveryProtocolVersion != RecoveryCommitmentVersion || f.claimed.ClaimFence != 1 {
		t.Fatalf("prepared marker/fence = %d/%d", f.claimed.RecoveryProtocolVersion, f.claimed.ClaimFence)
	}
	request := RecoveryClaimRequest{ID: f.claimed.ID, ClaimID: []byte("claim"), ClaimFence: 1,
		Context: dispatchContext(*f.clock, "request")}
	material, err := f.service.ClaimMaterial(f.ctx, request)
	if err != nil || len(material.Input) == 0 || len(material.ExecutorKey) == 0 {
		t.Fatalf("live material: %#v, %v", material, err)
	}
	prepare := PrepareClaimRequest{RecoveryClaimRequest: request, Mode: RecoveryKey,
		RequestDigest: [32]byte{1}, TargetScopeDigest: [32]byte{2}}
	committed, err := f.service.PrepareClaim(f.ctx, prepare)
	if err != nil || committed.RecoveryCommitment == nil || committed.Version != f.claimed.Version+1 {
		t.Fatalf("prepare = %#v, %v", committed, err)
	}
	// A lost Prepare response is retried against the same live claim, with
	// exactly the same commitment and no second version bump.
	retry, err := f.service.PrepareClaim(f.ctx, prepare)
	if err != nil || retry.Version != committed.Version ||
		*retry.RecoveryCommitment != *committed.RecoveryCommitment {
		t.Fatalf("lost-answer retry = %#v, %v", retry, err)
	}
	for _, mutate := range []func(*PrepareClaimRequest){
		func(r *PrepareClaimRequest) { r.Mode = RecoveryNatural },
		func(r *PrepareClaimRequest) { r.RequestDigest = [32]byte{3} },
		func(r *PrepareClaimRequest) { r.TargetScopeDigest = [32]byte{4} },
	} {
		changed := prepare
		mutate(&changed)
		if _, err := f.service.PrepareClaim(f.ctx, changed); !errors.Is(err, ErrActionConflict) {
			t.Fatalf("changed commitment accepted: %v", err)
		}
	}
	// The extra Prepare version does not strand fence-bound completion.
	completion := f.completion()
	completion.ClaimFence = 1
	completed, err := f.service.CompleteClaim(f.ctx, completion)
	if err != nil || completed.State != DispatchSucceeded {
		t.Fatalf("fence-bound completion after Prepare = %#v, %v", completed, err)
	}
}

func TestPreparedProtocolRejectsLegacyAndLostClaims(t *testing.T) {
	legacy := newCompletionFixture(t, Effects{EffectMutatesExternal})
	request := RecoveryClaimRequest{ID: legacy.claimed.ID, ClaimID: []byte("claim"), ClaimFence: 1,
		Context: dispatchContext(*legacy.clock, "request")}
	if _, err := legacy.service.ClaimMaterial(legacy.ctx, request); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("legacy material = %v", err)
	}
	prepare := PrepareClaimRequest{RecoveryClaimRequest: request, Mode: RecoveryKey,
		RequestDigest: [32]byte{1}, TargetScopeDigest: [32]byte{2}}
	if _, err := legacy.service.PrepareClaim(legacy.ctx, prepare); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("legacy Prepare = %v", err)
	}
	*legacy.clock = legacy.clock.Add(2 * time.Minute)
	if _, err := legacy.service.Claim(legacy.ctx, ClaimRequest{
		ID: legacy.claimed.ID, ExpectedVersion: legacy.claimed.Version,
		ClaimID: []byte("later"), Lease: time.Minute, Prepared: true,
		Context: dispatchContext(*legacy.clock, "request"),
	}); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("lapsed legacy claim upgraded = %v", err)
	}
	first := newCompletionFixtureWithPrepared(t, Effects{EffectMutatesExternal}, &remoteBoundExecutor{}, true)
	live := RecoveryClaimRequest{ID: first.claimed.ID, ClaimID: []byte("claim"), ClaimFence: 1,
		Context: dispatchContext(*first.clock, "request")}
	live.ClaimFence = 2
	if _, err := first.service.ClaimMaterial(first.ctx, live); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("different fence material = %v", err)
	}
	live.ClaimFence = 1
	*first.clock = first.clock.Add(2 * time.Minute)
	if _, err := first.service.ClaimMaterial(first.ctx, live); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("expired material = %v", err)
	}
	if _, err := first.service.PrepareClaim(first.ctx, PrepareClaimRequest{
		RecoveryClaimRequest: live, Mode: RecoveryKey,
		RequestDigest: [32]byte{1}, TargetScopeDigest: [32]byte{2},
	}); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("expired Prepare = %v", err)
	}
}

func TestPreparedKeyedRecoveryRequiresExactCommitment(t *testing.T) {
	f := newCompletionFixtureWithPrepared(t, Effects{EffectMutatesExternal}, &remoteBoundExecutor{}, true)
	initial := RecoveryClaimRequest{ID: f.claimed.ID, ClaimID: []byte("claim"), ClaimFence: 1,
		Context: dispatchContext(*f.clock, "request")}
	proposal := PrepareClaimRequest{RecoveryClaimRequest: initial, Mode: RecoveryKey,
		RequestDigest: [32]byte{1}, TargetScopeDigest: [32]byte{2}}
	committed, err := f.service.PrepareClaim(f.ctx, proposal)
	if err != nil {
		t.Fatal(err)
	}
	// A lost first Claim answer cannot be replayed after Prepare advanced the
	// version. Its claimant already had the answer to reach Prepare.
	if _, err := f.service.Claim(f.ctx, ClaimRequest{ID: f.claimed.ID,
		ExpectedVersion: f.claimed.Version - 1, ClaimID: []byte("claim"), Lease: time.Minute,
		Prepared: true, Context: dispatchContext(*f.clock, "request")}); err == nil {
		t.Fatal("obsolete first Claim retry was accepted after Prepare")
	}
	*f.clock = f.clock.Add(2 * time.Minute)
	recovered, err := f.service.Claim(f.ctx, ClaimRequest{ID: f.claimed.ID,
		ExpectedVersion: committed.Version, ClaimID: []byte("recovered"), Lease: time.Minute,
		Prepared: true, Context: dispatchContext(*f.clock, "request")})
	if err != nil || recovered.ClaimFence != 2 || recovered.RecoveryCommitment == nil {
		t.Fatalf("keyed re-claim = %#v, %v", recovered, err)
	}
	later := RecoveryClaimRequest{ID: recovered.ID, ClaimID: []byte("recovered"), ClaimFence: 2,
		Context: dispatchContext(*f.clock, "request")}
	if _, err := f.service.ClaimMaterial(f.ctx, later); err != nil {
		t.Fatalf("later material: %v", err)
	}
	proposal.RecoveryClaimRequest = later
	for _, row := range []struct {
		code   RecoveryRefusal
		mutate func(*PrepareClaimRequest)
	}{
		{RecoveryModeDrift, func(r *PrepareClaimRequest) { r.Mode = RecoveryNatural }},
		{RecoveryRequestDrift, func(r *PrepareClaimRequest) { r.RequestDigest = [32]byte{3} }},
		{RecoveryScopeDrift, func(r *PrepareClaimRequest) { r.TargetScopeDigest = [32]byte{4} }},
	} {
		changed := proposal
		row.mutate(&changed)
		_, err := f.service.PrepareClaim(f.ctx, changed)
		var refusal *RecoveryRefusalError
		if !errors.Is(err, ErrActionConflict) || !errors.As(err, &refusal) || refusal.Code != row.code {
			t.Fatalf("drifted recovery was allowed: %v", err)
		}
	}
	matched, err := f.service.PrepareClaim(f.ctx, proposal)
	if err != nil || matched.Version != recovered.Version ||
		*matched.RecoveryCommitment != *committed.RecoveryCommitment {
		t.Fatalf("identical keyed recovery = %#v, %v", matched, err)
	}
}

func TestPreparedUnprotectedClaimCannotReplay(t *testing.T) {
	f := newCompletionFixtureWithPrepared(t, Effects{EffectMutatesExternal}, &remoteBoundExecutor{}, true)
	_, err := f.service.PrepareClaim(f.ctx, PrepareClaimRequest{
		RecoveryClaimRequest: RecoveryClaimRequest{ID: f.claimed.ID, ClaimID: []byte("claim"),
			ClaimFence: 1, Context: dispatchContext(*f.clock, "request")},
		Mode: RecoveryUnprotected, RequestDigest: [32]byte{1}, TargetScopeDigest: [32]byte{2},
	})
	if err != nil {
		t.Fatal(err)
	}
	*f.clock = f.clock.Add(2 * time.Minute)
	current, err := f.store.GetAction(f.ctx, f.claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Claim(f.ctx, ClaimRequest{ID: current.ID,
		ExpectedVersion: current.Version, ClaimID: []byte("later"), Lease: time.Minute,
		Prepared: true, Context: dispatchContext(*f.clock, "request")}); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("unprotected replay claim = %v", err)
	}
}
