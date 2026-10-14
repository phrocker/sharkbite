// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/decisiondatasets"
	inventory "github.com/phrocker/shoal-oss/internal/decisioninventorystore"
	outcomes "github.com/phrocker/shoal-oss/internal/decisionoutcomes"
	"github.com/phrocker/shoal-oss/internal/explorercoord"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func durableStatuses(t *testing.T, b decisiondatasets.Bundle) map[string]string {
	t.Helper()
	var d struct {
		Rows []struct {
			ID     string `json:"id"`
			Status string `json:"label_status"`
		}
	}
	if err := json.Unmarshal(b.Dataset, &d); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, row := range d.Rows {
		out[row.ID] = row.Status
	}
	return out
}
func durableVerified(status map[string]string) int {
	n := 0
	for _, s := range status {
		if s == "verified" {
			n++
		}
	}
	return n
}
func durableAppend(t *testing.T, r *registry, a *auth.Authority, role, key string) (outcomes.Receipt, error) {
	t.Helper()
	ctx, _, err := bindRole(a, role)
	if err != nil {
		t.Fatal(err)
	}
	v := r.rows[r.cohort.Members[0].ID]
	cfg := v.outcome.ObservationConfig
	cfg.ObservedAt = time.Now().UTC()
	cfg.AssertedProvenance.ReporterID = shoal.ID(role)
	return r.outcomes.Append(ctx, cfg.RequestID, cfg.PredictionID, []byte(key), cfg)
}

type durableFailedPublication struct {
	outcomes.Admission
	fail bool
}

func (a *durableFailedPublication) Publish(ctx context.Context, d auth.Decision, p decision.PredictionRecord, o decision.OutcomeObservation, r outcomes.Receipt) error {
	if a.fail {
		return errors.New("simulated publication outage")
	}
	return a.Admission.Publish(ctx, d, p, o, r)
}

func durableBundle(t *testing.T, r *registry, ctx context.Context) decisiondatasets.Bundle {
	t.Helper()
	b, err := freshExport(ctx, r, r.resolver, r.cohort.ID)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func durableSnapshot(t *testing.T, r *registry, v *registered) inventory.Snapshot {
	t.Helper()
	s, err := r.inventories.Load(context.Background(), inventory.Scope{Domain: []byte(domain)}, r.inventoryBinding(v))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestDurableInventoryLateAppendAloneQuarantinesOriginalLabel(t *testing.T) {
	for _, task := range []string{"source", "review"} {
		t.Run(task, func(t *testing.T) {
			r, eng, a, ctx, err := prepareRegistry(filepath.Join(t.TempDir(), "bundle"), task)
			if err != nil {
				t.Fatal(err)
			}
			defer eng.Close()
			before := durableBundle(t, r, ctx)
			if durableVerified(durableStatuses(t, before)) != 14 {
				t.Fatal("initial verified cohort changed")
			}
			v := r.rows[r.cohort.Members[0].ID]
			oldSnapshot := durableSnapshot(t, r, v)
			oldBasis := v.basis.ID()
			enumeration := v.basis.Config().EnumerationID
			prefix := "fixture-inventory-capture:"
			if !strings.HasPrefix(string(enumeration), prefix) {
				t.Fatal("basis missing retained capture identity")
			}
			capturePath := filepath.Join(r.dir, "captures", strings.TrimPrefix(string(enumeration), prefix)+".json")
			captureBytes, err := os.ReadFile(capturePath)
			if err != nil {
				t.Fatal(err)
			}
			var artifact struct{ Snapshot []byte }
			if err = json.Unmarshal(captureBytes, &artifact); err != nil {
				t.Fatal(err)
			}
			decoded, err := inventory.DecodeSnapshot(inventory.Scope{Domain: []byte(domain)}, artifact.Snapshot)
			if err != nil || !reflect.DeepEqual(decoded, oldSnapshot) {
				t.Fatalf("retained artifact cannot reconstruct exact original snapshot: %v", err)
			}

			late, err := durableAppend(t, r, a, "fixture-reporter", "late-alone")
			if err != nil {
				t.Fatal(err)
			}
			current := durableSnapshot(t, r, v)
			if !current.Complete() || len(current.Entries) != 2 || current.ID == oldSnapshot.ID {
				t.Fatal("late ordinary append did not publish new inventory")
			}
			found := false
			for _, entry := range current.Entries {
				if entry.Intent.ReceiptID == late.ID {
					found = true
				}
			}
			if !found {
				t.Fatal("late receipt absent from durable inventory")
			}
			after := durableBundle(t, r, ctx)
			statuses := durableStatuses(t, after)
			if durableVerified(statuses) != 13 || statuses[string(v.fixture.ID)] != "unknown" {
				t.Fatalf("late report did not automatically quarantine original label: %+v", statuses)
			}
			if v.basis.ID() != oldBasis || v.basis.Config().EnumerationID != enumeration {
				t.Fatal("late report rewrote old admission")
			}
			retained, err := os.ReadFile(capturePath)
			if err != nil || !reflect.DeepEqual(retained, captureBytes) {
				t.Fatal("old capture changed with current inventory")
			}
			// The old assertion still requires its exact retained capture artifact.
			if err = os.WriteFile(capturePath, []byte("substituted capture"), 0600); err != nil {
				t.Fatal(err)
			}
			if b, err := freshExport(ctx, r, r.resolver, r.cohort.ID); err == nil || len(b.Dataset) != 0 {
				t.Fatal("corrupt historical capture exported")
			}
			if err = os.WriteFile(capturePath, captureBytes, 0600); err != nil {
				t.Fatal(err)
			}
			if durableVerified(durableStatuses(t, durableBundle(t, r, ctx))) != 13 {
				t.Fatal("capture restoration did not restore verifiable history")
			}
		})
	}
}

func TestDurableInventoryPublicationOutageBlocksExportThenRepairs(t *testing.T) {
	r, eng, a, ctx, err := prepareRegistry(filepath.Join(t.TempDir(), "bundle"), "source")
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	v := r.rows[r.cohort.Members[0].ID]
	reportCtx, _, err := bindRole(a, "fixture-reporter")
	if err != nil {
		t.Fatal(err)
	}
	backend, err := explorercoord.NewEngineStore(eng, outcomes.Table)
	if err != nil {
		t.Fatal(err)
	}
	hook := &durableFailedPublication{Admission: r.admission, fail: true}
	faulty, err := outcomes.New(outcomes.Config{Backend: backend, Resolver: r.resolver, Authority: outcomeAuthority{r}, Admission: hook, Clock: r.now})
	if err != nil {
		t.Fatal(err)
	}
	cfg := v.outcome.ObservationConfig
	cfg.ObservedAt = time.Now().UTC()
	key := []byte("lost-publication")
	if got, err := faulty.Append(reportCtx, cfg.RequestID, cfg.PredictionID, key, cfg); !errors.Is(err, outcomes.ErrIndeterminate) || got.ID != "" {
		t.Fatalf("publication failure lost uncertainty: %v", err)
	}
	pending := durableSnapshot(t, r, v)
	if pending.Complete() || len(pending.Entries) != 2 {
		t.Fatal("unpublished durable outcome did not block inventory")
	}
	if b, err := freshExport(ctx, r, r.resolver, r.cohort.ID); err == nil || len(b.Dataset) != 0 {
		t.Fatal("pending admission exported partial dataset")
	}
	original, err := faulty.Read(reportCtx, cfg.RequestID, cfg.PredictionID, key)
	if err != nil {
		t.Fatal(err)
	}
	repaired, err := r.outcomes.Append(reportCtx, cfg.RequestID, cfg.PredictionID, key, cfg)
	if err != nil || !reflect.DeepEqual(repaired, original) {
		t.Fatalf("repair did not publish original immutable receipt: %v", err)
	}
	if !durableSnapshot(t, r, v).Complete() {
		t.Fatal("repair remained pending")
	}
	if durableVerified(durableStatuses(t, durableBundle(t, r, ctx))) != 13 {
		t.Fatal("repaired late outcome did not quarantine old label")
	}
}

func TestDurableInventoryCrossReporterUnionAndConcurrentExports(t *testing.T) {
	r, eng, a, ctx, err := prepareRegistry(filepath.Join(t.TempDir(), "bundle"), "review")
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	v := r.rows[r.cohort.Members[0].ID]
	second, err := durableAppend(t, r, a, "fixture-reporter-secondary", "second-principal")
	if err != nil {
		t.Fatal(err)
	}
	snapshot := durableSnapshot(t, r, v)
	if len(snapshot.Entries) != 2 || !snapshot.Complete() || second.SubmitterID == v.outcome.SubmitterID {
		t.Fatal("second principal did not join same target inventory")
	}
	const n = 4
	var wg sync.WaitGroup
	results := make([]decisiondatasets.Bundle, n)
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) { defer wg.Done(); results[i], errs[i] = freshExport(ctx, r, r.resolver, r.cohort.ID) }(i)
	}
	wg.Wait()
	for i := range results {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if durableVerified(durableStatuses(t, results[i])) != 13 {
			t.Fatal("concurrent export sessions leaked or reset state")
		}
		if i > 0 && !reflect.DeepEqual(results[0].Dataset, results[i].Dataset) {
			t.Fatal("unchanged inventories produced inconsistent datasets")
		}
	}
	// A session captured before another actual report must not silently adopt the
	// newer session's vector; either it rejects the stale cut or stays immutable.
	service, _, err := exportSession(ctx, r, r.resolver, r.cohort.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = durableAppend(t, r, a, "fixture-reporter", "third-report"); err != nil {
		t.Fatal(err)
	}
	b, err := service.Export(ctx, r.cohort.ID)
	if err == nil || len(b.Dataset) != 0 {
		t.Fatal("stale export session silently rebound to new inventory vector")
	}
}
