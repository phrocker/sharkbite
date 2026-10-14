// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	inventory "github.com/phrocker/shoal-oss/internal/decisioninventorystore"
	outcomes "github.com/phrocker/shoal-oss/internal/decisionoutcomes"
	"github.com/phrocker/shoal-oss/internal/explorercoord"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
)

type finalPermissionAuthority struct {
	registeredOutcomeAuthority
	triggered bool
}

func (a *finalPermissionAuthority) Verify(ctx context.Context, d auth.Decision, material []outcomes.RegisteredMaterial) error {
	previous := a.r.now
	defer func() { a.r.now = previous }()
	calls := 0
	a.r.now = func() time.Time {
		calls++
		// For one original receipt: initial inventory permission, retained
		// source permission, inventory recheck, then final no-IO permission.
		if calls == 4 {
			a.triggered = true
			a.r.readable = false
		}
		return previous()
	}
	return a.registeredOutcomeAuthority.Verify(ctx, d, material)
}

func TestRegisteredReaderRevocationInsideFinalPermissionClock(t *testing.T) {
	r, eng, capability, ctx, err := prepareRegistry(filepath.Join(t.TempDir(), "fixture"), "source")
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	v := r.rows[r.cohort.Members[0].ID]
	snapshot, err := r.inventories.Load(ctx, inventory.Scope{Domain: []byte(domain)}, r.inventoryBinding(v))
	if err != nil {
		t.Fatal(err)
	}
	backend, err := explorercoord.NewEngineStore(eng, outcomes.Table)
	if err != nil {
		t.Fatal(err)
	}
	authority := &finalPermissionAuthority{registeredOutcomeAuthority: registeredOutcomeAuthority{r}}
	reader, err := outcomes.NewRegisteredReader(outcomes.RegisteredReaderConfig{Backend: backend, Resolver: capability.Resolver(), Authority: authority, Clock: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := reader.ReadRegistered(ctx, outcomes.RegisteredReference{TargetID: v.target, InventoryID: snapshot.ID, ReceiptID: v.outcome.ID})
	if !authority.triggered {
		t.Fatal("final permission callback was not exercised")
	}
	if err == nil || receipt.ID != "" {
		t.Fatal("final clock-triggered revocation disclosed original receipt", err)
	}
}
