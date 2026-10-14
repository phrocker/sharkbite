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
	"testing"

	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func serviceFixture(t *testing.T) (string, string, string, string) {
	t.Helper()
	numeric, _, _, m := fixture(t)
	task, _, _, e := serviceDefinitions()
	if e != nil {
		t.Fatal(e)
	}
	modelBytes, e := os.ReadFile(filepath.Join(numeric, "model.json"))
	if e != nil {
		t.Fatal(e)
	}
	var model map[string]any
	if e = json.Unmarshal(modelBytes, &model); e != nil {
		t.Fatal(e)
	}
	model["task_id"] = task.ID()
	modelBytes, _ = json.Marshal(model)
	write(t, numeric, "model.json", modelBytes)
	m.TaskID = string(task.ID())
	m.ModelSHA256 = digest(modelBytes)
	manifestBytes, _ := json.Marshal(m)
	write(t, numeric, "manifest.json", manifestBytes)
	dir := t.TempDir()
	if e = os.Rename(numeric, filepath.Join(dir, "numeric")); e != nil {
		t.Fatal(e)
	}
	if e = os.Mkdir(filepath.Join(dir, "sources"), 0700); e != nil {
		t.Fatal(e)
	}
	// An actual source prefix, with multi-byte text across the quote boundary.
	source := []byte(strings.Repeat("x", 511) + "世界\nfunc risky() { inspect() }\n" + strings.Repeat("original cached code\n", 100))
	write(t, dir, "sources/0000.txt", source)
	sm := sourceManifest{Schema: 1, Kind: "frozen-code-source-registration", NumericManifestSHA256: digest(manifestBytes), NumericModelSHA256: digest(modelBytes), ArchiveSHA256: archiveHash, OriginalInputsSHA256: originalMembers["reserved/inputs-frozen.json"], BuilderSHA256: strings.Repeat("d", 64), Action: "full_review", Rows: []sourceRow{{ID: "s1", SourceFile: "sources/0000.txt", SourceSHA256: digest(source), Bytes: len(source), Path: "fixture.go", PR: 1, Representation: "cached-v9-code-text"}}}
	sb, _ := json.Marshal(sm)
	write(t, dir, "source-manifest.json", sb)
	return dir, digest(modelBytes), digest(manifestBytes), digest(sb)
}
func TestServicePersistsOriginalReceiptsOnRestart(t *testing.T) {
	dir, mh, ph, sh := serviceFixture(t)
	stateDir := filepath.Join(t.TempDir(), "nested", "state")
	first, e := inquireServiceRows(dir, mh, ph, sh, stateDir, 1)
	if e != nil {
		t.Fatal(e)
	}
	if first.ProviderCalls != 1 || first.Matches != 1 || first.Mismatches != 0 || !first.ReplayMatched || !first.AllFullReview || first.OptimizationEnabled || first.ActualHistoricalTimestamps || first.EffectiveDevice != "cpu" {
		t.Fatalf("unexpected first %+v", first)
	}
	second, e := inquireServiceRows(dir, mh, ph, sh, stateDir, 1)
	if e != nil {
		t.Fatal(e)
	}
	if second.ProviderCalls != 0 {
		t.Fatal("restart invoked provider")
	}
	second.ProviderCalls = 1
	if !reflect.DeepEqual(first, second) {
		t.Fatal("restart changed provenance or receipts")
	}
	if _, e = inquireService(dir, mh, ph, sh, stateDir); e == nil {
		t.Fatal("CLI-sized inquiry accepted incomplete cohort")
	}
}
func TestServiceRejectsStateOrInputRebinding(t *testing.T) {
	for _, kind := range []string{"model", "manifest", "source-pin", "source-bytes", "corrupt-state", "missing-state"} {
		t.Run(kind, func(t *testing.T) {
			dir, mh, ph, sh := serviceFixture(t)
			stateDir := t.TempDir()
			if _, e := inquireServiceRows(dir, mh, ph, sh, stateDir, 1); e != nil {
				t.Fatal(e)
			}
			switch kind {
			case "model":
				mh = strings.Repeat("0", 64)
			case "manifest":
				ph = strings.Repeat("0", 64)
			case "source-pin":
				sh = strings.Repeat("0", 64)
			case "source-bytes":
				write(t, dir, "sources/0000.txt", []byte("substituted"))
			case "corrupt-state":
				write(t, stateDir, "service-state.json", []byte(`{"Schema":1,"Schema":1}`))
			case "missing-state":
				if e := os.Remove(filepath.Join(stateDir, "service-state.json")); e != nil {
					t.Fatal(e)
				}
			}
			if _, e := inquireServiceRows(dir, mh, ph, sh, stateDir, 1); e == nil {
				t.Fatal("accepted substitution or missing replay metadata")
			}
		})
	}
}
func TestServiceStatePinsAndDirectoryDurability(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "state")
	pins := []string{strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)}
	first, e := loadServiceState(dir, pins[0], pins[1], pins[2], "test-predictor")
	if e != nil {
		t.Fatal(e)
	}
	for i := range pins {
		changed := append([]string{}, pins...)
		changed[i] = strings.Repeat("d", 64)
		if _, e = loadServiceState(dir, changed[0], changed[1], changed[2], "test-predictor"); e == nil {
			t.Fatal("accepted state pin substitution")
		}
	}
	second, e := loadServiceState(dir, pins[0], pins[1], pins[2], "test-predictor")
	if e != nil || first != second {
		t.Fatal("state identity changed", e)
	}
	calls := []string{}
	injected := errors.New("sync unavailable")
	target := filepath.Join(t.TempDir(), "new", "state")
	if e = makeStateDirectory(target, func(path string) error { calls = append(calls, path); return injected }); !errors.Is(e, injected) {
		t.Fatal(e)
	}
	calls = nil
	if e = makeStateDirectory(target, func(path string) error { calls = append(calls, path); return nil }); e != nil {
		t.Fatal(e)
	}
	if len(calls) < 3 || calls[0] != target || calls[len(calls)-1] != "/" {
		t.Fatal("retry did not sync ancestor chain", calls)
	}
}
func TestLocalRegistrationRechecksRevokedSourceOnRetryAndRead(t *testing.T) {
	dir, mh, ph, sh := serviceFixture(t)
	task, _, _, e := serviceDefinitions()
	if e != nil {
		t.Fatal(e)
	}
	bundle, e := loadBundle(filepath.Join(dir, "numeric"), mh, ph, task, 1)
	if e != nil {
		t.Fatal(e)
	}
	defer bundle.Close()
	sources, e := loadSources(dir, sh, bundle)
	if e != nil {
		t.Fatal(e)
	}
	stateDir := t.TempDir()
	saved, e := loadServiceState(stateDir, mh, ph, sh, bundle.Provider.Identity().ID())
	if e != nil {
		t.Fatal(e)
	}
	session, e := openServiceSession(bundle, sources, saved, stateDir)
	if e != nil {
		t.Fatal(e)
	}
	defer session.engine.Close()
	granted, e := session.authority.Binder().Bind(context.Background(), session.decision)
	if e != nil {
		t.Fatal(e)
	}
	record := session.records[0]
	id := record.Bundle.Request.ID()
	key := serviceKey(sources[0].ID)
	revoked, e := localDecision(sources, map[string]bool{sources[0].ID: true})
	if e != nil {
		t.Fatal(e)
	}
	denied, e := session.authority.Binder().Bind(context.Background(), revoked)
	if e != nil {
		t.Fatal(e)
	}
	if e = session.catalog.Retain(denied, record); !shoal.IsErrorCode(e, shoal.ErrorNotFound) {
		t.Fatalf("unauthorized source admitted: %v", e)
	}
	if e = session.catalog.Retain(granted, record); e != nil {
		t.Fatal(e)
	}
	// Verify exact registration bytes, not merely the source digest in a claim.
	changed := record
	changed.Sources = append(changed.Sources[:0:0], changed.Sources...)
	changed.Sources[0].Bytes = []byte("substituted")
	if e = session.registration.Verify(granted, session.decision, changed); !shoal.IsErrorCode(e, shoal.ErrorNotFound) {
		t.Fatalf("substituted registration admitted: %v", e)
	}
	first, e := session.service.Evaluate(granted, id, key)
	if e != nil {
		t.Fatal(e)
	}
	if first.Receipt.Result == nil || first.Receipt.Result.Status != decision.Completed {
		t.Fatal("not completed")
	}
	for _, call := range []func() error{func() error { _, e := session.service.Evaluate(denied, id, key); return e }, func() error { _, e := session.service.Read(denied, id, key); return e }, func() error { _, e := session.catalog.LoadAuthorized(denied, revoked, id); return e }} {
		if e = call(); !shoal.IsErrorCode(e, shoal.ErrorNotFound) {
			t.Fatalf("revoked source disclosed: %v", e)
		}
	}
	if session.provider.calls != 1 {
		t.Fatal("revoked retry reinvoked provider")
	}
	// Fresh authentication with identical current grants can read original receipt.
	renewed, e := localDecision(sources, nil)
	if e != nil {
		t.Fatal(e)
	}
	ctx, e := session.authority.Binder().Bind(context.Background(), renewed)
	if e != nil {
		t.Fatal(e)
	}
	read, e := session.service.Read(ctx, id, key)
	if e != nil || read.Receipt.ID != first.Receipt.ID {
		t.Fatal("renewed auth did not read original receipt", e)
	}
	source := record.Bundle.Request.Picture().Config().Sources[0]
	if source.ObservedAt != saved.CreatedAt || source.ReceivedAt != saved.CreatedAt {
		t.Fatal("source times were not import times")
	}
	if source.Digest != sources[0].Digest || len(record.Sources[0].Bytes) <= 512 {
		t.Fatal("full source bytes not bound")
	}
}

func TestStateRetryConfirmsFileDurabilityBeforeEngineOpen(t *testing.T) {
	dir := t.TempDir()
	pins := []string{strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)}
	original, e := loadServiceState(dir, pins[0], pins[1], pins[2], "test-predictor")
	if e != nil {
		t.Fatal(e)
	}
	injected := errors.New("file sync failed")
	calls := 0
	if _, e = loadServiceStateWithSync(dir, pins[0], pins[1], pins[2], "test-predictor", func(f *os.File) error {
		calls++
		if filepath.Base(f.Name()) != "service-state.json" {
			t.Fatal("wrong identity file")
		}
		return injected
	}); !errors.Is(e, injected) {
		t.Fatal("file durability uncertainty hidden", e)
	}
	if calls != 1 {
		t.Fatal("existing file not synced")
	}
	if _, e = os.Stat(filepath.Join(dir, "engine")); !os.IsNotExist(e) {
		t.Fatal("engine opened before state confirmation")
	}
	retry, e := loadServiceStateWithSync(dir, pins[0], pins[1], pins[2], "test-predictor", func(f *os.File) error { calls++; return f.Sync() })
	if e != nil || retry != original || calls != 2 {
		t.Fatal("retry changed identity or skipped sync", e)
	}
}

func snapshotServiceFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	e := filepath.WalkDir(dir, func(path string, entry os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if !entry.IsDir() {
			b, e := os.ReadFile(path)
			if e != nil {
				return e
			}
			relative, e := filepath.Rel(dir, path)
			if e != nil {
				return e
			}
			files[relative] = digest(b)
		}
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	return files
}
func TestServiceRejectsRuntimeDriftWithoutEngineMutation(t *testing.T) {
	dir, mh, ph, sh := serviceFixture(t)
	stateDir := t.TempDir()
	first, e := inquireServiceRows(dir, mh, ph, sh, stateDir, 1)
	if e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(stateDir, "service-state.json")
	raw, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	var state serviceState
	if e = json.Unmarshal(raw, &state); e != nil {
		t.Fatal(e)
	}
	if state.Schema != 2 || state.PredictorID != first.PredictorID {
		t.Fatal("state omitted effective predictor identity")
	}
	// Simulate a previous Go/runtime environment, while preserving every model,
	// source, and numeric pin. A new runtime must not silently mint fresh receipts.
	state.PredictorID = shoal.ID("decision:predictor:v1:" + strings.Repeat("0", 64))
	changed, _ := json.Marshal(state)
	write(t, stateDir, "service-state.json", changed)
	before := snapshotServiceFiles(t, stateDir)
	if _, e = inquireServiceRows(dir, mh, ph, sh, stateDir, 1); e == nil || !strings.Contains(e.Error(), "incompatible predictor/runtime") {
		t.Fatalf("runtime drift accepted or wrong error: %v", e)
	}
	if after := snapshotServiceFiles(t, stateDir); !reflect.DeepEqual(before, after) {
		t.Fatal("runtime rejection mutated persistent service files")
	}
	// The original state remains usable; rejection did not reserve new work.
	write(t, stateDir, "service-state.json", raw)
	second, e := inquireServiceRows(dir, mh, ph, sh, stateDir, 1)
	if e != nil || second.ProviderCalls != 0 || !reflect.DeepEqual(first.Rows, second.Rows) {
		t.Fatal("runtime rejection changed receipts", e)
	}
}
func TestServiceRejectsLegacyStateWithoutMigration(t *testing.T) {
	dir, mh, ph, sh := serviceFixture(t)
	stateDir := t.TempDir()
	if _, e := inquireServiceRows(dir, mh, ph, sh, stateDir, 1); e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(stateDir, "service-state.json")
	raw, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	var legacy map[string]any
	if e = json.Unmarshal(raw, &legacy); e != nil {
		t.Fatal(e)
	}
	legacy["Schema"] = 1
	delete(legacy, "PredictorID")
	changed, _ := json.Marshal(legacy)
	write(t, stateDir, "service-state.json", changed)
	before := snapshotServiceFiles(t, stateDir)
	if _, e = inquireServiceRows(dir, mh, ph, sh, stateDir, 1); e == nil || !strings.Contains(e.Error(), "explicit recovery required") {
		t.Fatalf("legacy state silently migrated: %v", e)
	}
	if after := snapshotServiceFiles(t, stateDir); !reflect.DeepEqual(before, after) {
		t.Fatal("legacy rejection mutated persistence")
	}
}
