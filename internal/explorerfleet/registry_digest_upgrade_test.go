// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package explorerfleet

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/atpltest"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// The v1 registry mutation digests a build before #521 recorded for
// upgradeSpec, registered exactly as upgradeLifecycle does. They were computed
// on origin/main at 6cf88b0c by driving that build's fleet.Service through
// internal/atpltest and reading the digest its recorder was handed. They are
// literals so that nothing in this build produces the stored side: a test that
// recomputed v1 here would still pass with the version switch wired backwards.
const (
	upgradeV1Plain = "e10974c0961e69614adeecb7d7771547" +
		"49fa84eb66ef6e56c8f69af46aea6637"
	upgradeV1Approval = "1fa9069c3eeb9ab30df730b30bbf955d" +
		"07de96fc4b94682428a2abc4b62b2a47"
	// The v1 receipt identities that build derived for the same lifecycles
	// (v1LifecycleSessionID hashes the whole lifecycle, digest included). A
	// v1 receipt is found only at this exact key, so it is pinned too: a
	// drifted identity derivation would otherwise silently stop finding
	// every v1 receipt.
	upgradeV1PlainReceiptID    = "interaction.session_629e0702168d59de7a4fd9a6dc005d54"
	upgradeV1ApprovalReceiptID = "interaction.session_d30b7643cfad468a86fcf0e42e4a6b5a"
)

func upgradeSpec(requireApproval bool) fleet.Spec {
	return fleet.Spec{
		ID: "upgrade-agent", AuthorizationDomain: []byte(atpltest.Domain),
		Scopes: []fleet.Scope{{
			SourceID: []byte("source"), PolicyID: []byte("policy"),
		}},
		ExecutorRef: "local",
		Capabilities: []fleet.Capability{{Name: "ops", Actions: []fleet.Action{{
			Name:             "deploy",
			InputSchema:      json.RawMessage(`{"type":"object"}`),
			OutputSchema:     json.RawMessage(`{"type":"object"}`),
			Effects:          fleet.Effects{fleet.EffectMutatesExternal},
			RequiresApproval: requireApproval,
		}}}},
		LeaseExpiresAt: time.Date(2030, 1, 1, 1, 0, 0, 0, time.UTC),
	}
}

// upgradeLifecycle registers upgradeSpec through this build's real
// fleet.Service and returns the lifecycle it handed its recorder.
//
// The request ID is fixed (atpltest's own decisions draw a random one)
// because a v1 receipt identity hashes the whole lifecycle, request ID
// included; the old build's identities below were computed with this exact
// harness.
func upgradeLifecycle(t *testing.T, requireApproval bool) fleet.Lifecycle {
	t.Helper()
	return registeredLifecycle(t, upgradeSpec(requireApproval))
}

func registeredLifecycle(t *testing.T, spec fleet.Spec) fleet.Lifecycle {
	t.Helper()
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	registry := atpltest.NewRegistry(t, atpltest.NewClock(now),
		atpltest.Executors{"local": atpltest.Executor{
			Max: fleet.Effects{fleet.EffectMutatesExternal},
		},
			// For descriptors with an action that declares no effects, which
			// an executor that may reach outside Shoal refuses.
			"corpus": atpltest.Executor{
				Max: fleet.Effects{fleet.EffectReadsCorpus},
			}},
		[]string{"source"}, []string{"policy"})
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "owner", Actor: "owner-actor",
		AuthorizationDomain: []byte(atpltest.Domain),
		AllowedOperations: []auth.Operation{
			auth.OperationAgentRegister, auth.OperationAgentResolve,
		},
		PermittedSourceIDs:    [][]byte{[]byte("source")},
		PermittedPolicyIDs:    [][]byte{[]byte("policy")},
		PolicyGeneration:      1,
		AuthenticationExpires: now.Add(72 * time.Hour),
		RequestID:             "upgrade-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := registry.Authority.Binder().Bind(context.Background(), decision)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Service.Register(ctx, fleet.RegisterRequest{
		Context: fleet.RequestContext{
			RequestID: "upgrade-request", ReasonCode: "test",
			Deadline: now.Add(time.Minute),
		},
		RegistrationKey: "upgrade-key", Spec: spec,
	}); err != nil {
		t.Fatal(err)
	}
	records := registry.Recorder.Records()
	if len(records) != 1 {
		t.Fatalf("lifecycle records = %d, want 1", len(records))
	}
	return records[0]
}

func mustDigest(t *testing.T, value string) [32]byte {
	t.Helper()
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != 32 {
		t.Fatalf("bad digest literal %q", value)
	}
	var digest [32]byte
	copy(digest[:], decoded)
	return digest
}

// TestRegistryDigestUpgradeReconcilesV1Receipt is the upgrade test for #521.
// A receipt the previous build wrote, carrying the literal v1 digest, must
// still be accepted as the same mutation when the request is retried after
// the upgrade, and a changed descriptor must still be refused.
func TestRegistryDigestUpgradeReconcilesV1Receipt(t *testing.T) {
	upgraded := upgradeLifecycle(t, false)
	storedV1 := mustDigest(t, upgradeV1Plain)
	// The service hands the recorder the v1 digest the old build computed,
	// and writes a different (v2) one.
	if upgraded.LegacyMutationDigest != storedV1 {
		t.Fatalf("legacy digest = %x, want the old build's %s",
			upgraded.LegacyMutationDigest, upgradeV1Plain)
	}
	if upgraded.MutationDigest == storedV1 {
		t.Fatal("the upgraded build still writes the v1 digest")
	}
	asserted, err := fleet.CallerAssertedRegistryReason(
		upgraded.ReasonCode, upgraded.ReasonDetail)
	if err != nil {
		t.Fatal(err)
	}
	// What the old build recorded for this request: the same lifecycle, with
	// the v1 digest it computed, under the identity it wrote.
	old := upgraded
	old.MutationDigest = storedV1
	old.LegacyMutationDigest = [32]byte{}

	changedSource := upgradeLifecycle(t, true)
	if changedSource.LegacyMutationDigest != mustDigest(t, upgradeV1Approval) {
		t.Fatalf("legacy digest of the changed descriptor = %x, want %s",
			changedSource.LegacyMutationDigest, upgradeV1Approval)
	}
	// The same request retried with a changed descriptor.
	changed := upgraded
	changed.MutationDigest = changedSource.MutationDigest
	changed.LegacyMutationDigest = changedSource.LegacyMutationDigest

	for _, prior := range []struct {
		name    string
		receipt interaction.Session
	}{
		{"v3", func() interaction.Session {
			receipt := trustedReceipt(old, asserted)
			receipt.ID = v3LifecycleSessionID(old)
			return receipt
		}()},
		// Stored at the key the old build derived, not at one this build
		// derives, so the recorder must recompute that key exactly to find it.
		{"v1", func() interaction.Session {
			receipt := trustedReceipt(old, interaction.CallerAssertedReason{})
			receipt.ID = upgradeV1PlainReceiptID
			return receipt
		}()},
	} {
		store := &reconcilingLifecycleStore{
			trustedLifecycleRecorder: trustedLifecycleRecorder{lifecycle: upgraded},
			stored:                   prior.receipt,
		}
		recorder, err := NewLifecycleRecorderWithReader(store, store)
		if err != nil {
			t.Fatal(err)
		}
		if err := recorder.RecordLifecycle(
			context.Background(), upgraded,
		); err != nil {
			t.Fatalf("%s receipt: retry across the upgrade = %v", prior.name, err)
		}
		if store.stored.ID != prior.receipt.ID {
			t.Fatalf("%s receipt: the retry replaced the stored receipt",
				prior.name)
		}
		// Reconciled, and the mutation actually applied is now recorded
		// under v4 with its v2 digest.
		requireV4Receipt(t, store, upgraded)
		// A changed descriptor under the same request conflicts: with the
		// v3 receipt directly, and with the v4 receipt where the v1 one is
		// keyed by a digest it does not share.
		err = recorder.RecordLifecycle(context.Background(), changed)
		if !shoal.IsErrorCode(err, shoal.ErrorConflict) {
			t.Fatalf("%s receipt: changed descriptor across the upgrade = %v",
				prior.name, err)
		}
	}
	// The changed descriptor's v1 identity is the old build's too.
	legacyChanged := lifecycleWithV1RegistryDigest(changed)
	if got := v1LifecycleSessionID(legacyChanged); got != upgradeV1ApprovalReceiptID {
		t.Fatalf("v1 receipt identity = %s, want the old build's %s",
			got, upgradeV1ApprovalReceiptID)
	}
}

// TestRegistryDigestLegacyReconcileRecordsTheAppliedMutation is the repro
// for the v1 collision reaching the receipt path (#521). A and B are both
// registrable and share a v1 digest. A v3 receipt records A; the request is
// retried with B. B reconciles with A's receipt, since v1 cannot tell them
// apart, but if A's store write never landed, B is what gets applied. So
// the reconciliation must also write a v4 receipt recording B by its v2
// digest, after which A under the same request conflicts.
func TestRegistryDigestLegacyReconcileRecordsTheAppliedMutation(t *testing.T) {
	schema := json.RawMessage(`{"type":"object"}`)
	spec := func(actions []fleet.Action) fleet.Spec {
		spec := upgradeSpec(false)
		spec.ExecutorRef = "corpus"
		spec.Capabilities = []fleet.Capability{{Name: "ops", Actions: actions}}
		return spec
	}
	// Both registered through the real service, under the same request.
	lifecycleA := registeredLifecycle(t, spec([]fleet.Action{
		{Name: "a", InputSchema: schema, OutputSchema: schema,
			RequiresApproval: true},
		{Name: string(fleet.EffectReadsCorpus), InputSchema: schema,
			OutputSchema: schema},
	}))
	lifecycleB := registeredLifecycle(t, spec([]fleet.Action{
		{Name: "a", InputSchema: schema, OutputSchema: schema},
		{Name: "shoal.fleet.requires-approval.v1",
			Effects:     fleet.Effects{fleet.EffectReadsCorpus},
			InputSchema: schema, OutputSchema: schema},
	}))
	if lifecycleA.LegacyMutationDigest != lifecycleB.LegacyMutationDigest {
		t.Fatal("the repro descriptors no longer collide under v1")
	}
	if lifecycleA.MutationDigest == lifecycleB.MutationDigest {
		t.Fatal("the repro descriptors collide under v2")
	}
	asserted, err := fleet.CallerAssertedRegistryReason(
		lifecycleA.ReasonCode, lifecycleA.ReasonDetail)
	if err != nil {
		t.Fatal(err)
	}
	oldA := lifecycleWithV1RegistryDigest(lifecycleA)
	receiptA := trustedReceipt(oldA, asserted)
	receiptA.ID = v3LifecycleSessionID(oldA)
	store := &reconcilingLifecycleStore{
		trustedLifecycleRecorder: trustedLifecycleRecorder{lifecycle: lifecycleA},
		stored:                   receiptA,
	}
	recorder, err := NewLifecycleRecorderWithReader(store, store)
	if err != nil {
		t.Fatal(err)
	}
	// B, retried under A's request, reconciles with A's v1-era receipt...
	if err := recorder.RecordLifecycle(context.Background(), lifecycleB); err != nil {
		t.Fatalf("B retry against A's v3 receipt = %v", err)
	}
	// ...and is recorded under v4 by its own v2 digest.
	requireV4Receipt(t, store, lifecycleB)
	// B again reconciles with its own v4 receipt.
	if err := recorder.RecordLifecycle(context.Background(), lifecycleB); err != nil {
		t.Fatalf("repeated B retry = %v", err)
	}
	// A, which v1 cannot tell from B, now conflicts against v4.
	err = recorder.RecordLifecycle(context.Background(), lifecycleA)
	if !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("A retry after B was recorded under v4 = %v", err)
	}
}

// TestRegistryDigestUpgradeWritesV2 checks the other half: a request first
// seen after the upgrade is written under the v4 identity with the v2 digest,
// reconciles with itself, and refuses a changed descriptor.
func TestRegistryDigestUpgradeWritesV2(t *testing.T) {
	upgraded := upgradeLifecycle(t, false)
	asserted, err := fleet.CallerAssertedRegistryReason(
		upgraded.ReasonCode, upgraded.ReasonDetail)
	if err != nil {
		t.Fatal(err)
	}
	store := &reconcilingLifecycleStore{
		trustedLifecycleRecorder: trustedLifecycleRecorder{lifecycle: upgraded},
	}
	recorder, err := NewLifecycleRecorderWithReader(store, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.RecordLifecycle(context.Background(), upgraded); err != nil {
		t.Fatal(err)
	}
	if store.stored.ID != LifecycleReceiptID(
		upgraded.Operation, upgraded.RequestID, upgraded.AgentID,
	) || store.stored.ID == v3LifecycleSessionID(upgraded) {
		t.Fatalf("new receipt identity = %s, want the v4 identity", store.stored.ID)
	}
	if store.stored.QueryDigest != lifecycleQueryDigest(upgraded, asserted) ||
		store.stored.QueryDigest == lifecycleQueryDigest(
			lifecycleWithV1RegistryDigest(upgraded), asserted) {
		t.Fatal("new receipt does not carry the v2 registry digest")
	}
	if err := recorder.RecordLifecycle(context.Background(), upgraded); err != nil {
		t.Fatalf("retry of a v4 receipt = %v", err)
	}
	changedSource := upgradeLifecycle(t, true)
	changed := upgraded
	changed.MutationDigest = changedSource.MutationDigest
	changed.LegacyMutationDigest = changedSource.LegacyMutationDigest
	err = recorder.RecordLifecycle(context.Background(), changed)
	if !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("changed descriptor against a v4 receipt = %v", err)
	}
}
