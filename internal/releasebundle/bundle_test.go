// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package releasebundle

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/storage/memory"
	"github.com/phrocker/shoal-oss/internal/weightsbundle"
)

type fixture struct {
	root            string
	release         weightsbundle.Release
	lineage         weightsbundle.DatasetLineage
	evaluation      weightsbundle.Record
	datasetManifest []byte
	images          []ImageRef
}

func seal(t *testing.T, kind string, fields map[string]any) weightsbundle.Record {
	t.Helper()
	r, err := weightsbundle.Seal(kind, fields)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	root := filepath.Join(t.TempDir(), "bundle")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"model.json": `{"kind":"linear-student"}`, "runtime.json": `{"max_input_tokens":64}`} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifest, err := weightsbundle.Build(root, map[string]string{"model.json": weightsbundle.RoleWeights, "runtime.json": weightsbundle.RoleRuntime})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := weightsbundle.Stage(root, manifest); err != nil {
		t.Fatal(err)
	}
	datasetManifest := []byte(`{"schema":1,"kind":"authorized-numeric-training-export","rows":[{"id":"r1","sources":[{"Digest":"src-1"}]}]}`)
	lineage, err := weightsbundle.SourcesFromExportManifest(datasetManifest)
	if err != nil {
		t.Fatal(err)
	}
	weights, _ := manifest.File(weightsbundle.RoleWeights)
	candidate := seal(t, weightsbundle.KindCandidate, map[string]any{"dataset_id": "dataset:1", "evaluation_policy_id": "policy:1", "training_ids": []string{"r1"}, "model_digest": weights.SHA256, "policy_predeclared": true, "task_ids": []string{"task-v1"}, "dataset_manifest_sha256": lineage.DatasetManifestSHA256, "training_manifest_sha256": strings.Repeat("7", 64)})
	evaluation := seal(t, weightsbundle.KindEvaluation, map[string]any{"candidate_id": candidate["id"], "dataset_id": "dataset:1", "evaluation_policy_id": "policy:1", "ledger_id": "ledger:1", "disposition": "shadow_candidate", "reasons": []string{}, "metrics": []any{}})
	artifact, err := weightsbundle.NewArtifact(candidate["id"].(string), evaluation["id"].(string), "dataset:1", manifest, "runtime:1")
	if err != nil {
		t.Fatal(err)
	}
	approval, err := weightsbundle.NewPromotionApproval(candidate["id"].(string), evaluation["id"].(string), "owner")
	if err != nil {
		t.Fatal(err)
	}
	release, err := weightsbundle.Promote(weightsbundle.PromotionInput{Candidate: candidate, Evaluation: evaluation, Artifact: artifact, Approval: approval})
	if err != nil {
		t.Fatal(err)
	}
	return fixture{root: root, release: release, lineage: lineage, evaluation: evaluation, datasetManifest: datasetManifest, images: []ImageRef{{Name: "student-runtime", Repository: "ghcr.io/example/shoal-student-runtime", Digest: "sha256:" + strings.Repeat("a", 64)}}}
}

func (f fixture) input() ExportInput {
	return ExportInput{Release: f.release, Lineage: f.lineage, BundleRoot: f.root, DatasetManifest: f.datasetManifest, Evaluation: f.evaluation, Images: f.images, Now: time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)}
}

func TestExportVerifyImportAndRollbackOffline(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	dest := filepath.Join(t.TempDir(), "release-bundle")
	index, err := Export(f.input(), dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(index.Entries) != 7 || index.ReleaseID != f.release.ID {
		t.Fatalf("%+v", index)
	}
	verified, err := Verify(dest)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range verified.Results {
		if !r.Verified {
			t.Fatalf("%+v", r)
		}
	}
	if _, err := Export(f.input(), dest); !errors.Is(err, ErrRefused) {
		t.Fatalf("re-export over an existing bundle: %v", err)
	}

	// Disconnected side: its own registry and serving root, no network.
	store := memory.New()
	registry, _ := weightsbundle.OpenRegistry(ctx, store, "offline")
	serving := filepath.Join(t.TempDir(), "serving")
	result, err := Import(ctx, dest, ImportOptions{Registry: registry, ServingRoot: serving, Importer: "operator-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Activated || result.AlreadyThere {
		t.Fatalf("%+v", result)
	}
	if _, digest, err := weightsbundle.Load(result.ServingPath); err != nil || digest != index.BundleDigest {
		t.Fatalf("serving path: %v %s", err, digest)
	}
	active, err := registry.Resolve()
	if err != nil || active.ID != f.release.ID {
		t.Fatalf("%v", err)
	}
	events := registry.Events()
	if len(events) != 1 || events[0].Kind != weightsbundle.EventImported || !strings.Contains(events[0].Detail, "operator-1") || !strings.Contains(events[0].Detail, verified.IndexDigest) {
		t.Fatalf("%+v", events)
	}
	// Importing the active release again is a no-op, not a second pointer advance.
	again, err := Import(ctx, dest, ImportOptions{Registry: registry, ServingRoot: serving, Importer: "operator-1"})
	if err != nil || again.Activated || !again.AlreadyThere || len(registry.History()) != 1 {
		t.Fatalf("%+v %v", again, err)
	}

	// A second release arrives; rollback to the first works from what was imported.
	second := newFixture(t)
	secondDest := filepath.Join(t.TempDir(), "second")
	if _, err := Export(second.input(), secondDest); err != nil {
		t.Fatal(err)
	}
	if _, err := Import(ctx, secondDest, ImportOptions{Registry: registry, ServingRoot: serving, Importer: "operator-1"}); err != nil {
		t.Fatal(err)
	}
	if active, _ := registry.Active(); active.ID != second.release.ID {
		t.Fatal("second import did not activate")
	}
	approval, _ := weightsbundle.NewRollbackApproval(second.release.ID, f.release.ID, "operator-1")
	rolled, err := registry.Rollback(ctx, f.release.ID, approval)
	if err != nil {
		t.Fatal(err)
	}
	if *rolled.BundleDigest != index.BundleDigest {
		t.Fatal("rollback did not restore the imported bundle digest")
	}
	if _, _, err := weightsbundle.Load(filepath.Join(serving, index.BundleDigest)); err != nil {
		t.Fatal("prior imported bundle no longer on the serving path")
	}
	// Re-importing a prior (non-active) release is refused: roll back instead.
	if _, err := Import(ctx, secondDest, ImportOptions{Registry: registry, ServingRoot: serving, Importer: "operator-1"}); !errors.Is(err, weightsbundle.ErrRefused) {
		t.Fatalf("%v", err)
	}
}

func TestOneAlteredByteFailsImportWithTheEntryNamed(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	dest := filepath.Join(t.TempDir(), "rb")
	if _, err := Export(f.input(), dest); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		mutate func()
		want   string
	}{
		"weights byte": {func() {
			p := filepath.Join(dest, BundleDir, "model.json")
			b, _ := os.ReadFile(p)
			b[len(b)-2] ^= 0x01
			os.WriteFile(p, b, 0o600)
		}, `altered (digest) "bundle/model.json"`},
		"release record": {func() {
			p := filepath.Join(dest, ReleaseName)
			b, _ := os.ReadFile(p)
			os.WriteFile(p, []byte(strings.Replace(string(b), "active", "Active", 1)), 0o600)
		}, `altered (digest) "release.json"`},
		"missing evaluation": {func() { os.Remove(filepath.Join(dest, EvaluationName)) }, `missing "evaluation.json"`},
		"extra file":         {func() { os.WriteFile(filepath.Join(dest, "notes.txt"), []byte("x"), 0o600) }, `extra "notes.txt"`},
		"truncated manifest": {func() {
			p := filepath.Join(dest, DatasetManifestName)
			b, _ := os.ReadFile(p)
			os.WriteFile(p, b[:len(b)-1], 0o600)
		}, `altered (size) "dataset-manifest.json"`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fresh := filepath.Join(t.TempDir(), "rb")
			if _, err := Export(f.input(), fresh); err != nil {
				t.Fatal(err)
			}
			dest = fresh
			tc.mutate()
			serving := filepath.Join(t.TempDir(), "serving")
			registry := weightsbundle.NewRegistry()
			result, err := Import(ctx, dest, ImportOptions{Registry: registry, ServingRoot: serving, Importer: "op"})
			if !errors.Is(err, ErrVerification) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %s", err, tc.want)
			}
			if _, statErr := os.Stat(serving); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatal("serving path written before verification")
			}
			if _, ok := registry.Active(); ok || len(registry.Events()) != 0 {
				t.Fatal("registry touched by a failed import")
			}
			var failed int
			for _, r := range result.Verified.Results {
				if !r.Verified {
					failed++
				}
			}
			if failed != 1 {
				t.Fatalf("expected exactly one failed entry, got %+v", result.Verified.Results)
			}
		})
	}
}

func TestExportRefusesIncompletePromotion(t *testing.T) {
	f := newFixture(t)
	cases := map[string]struct {
		mutate func(in *ExportInput)
		want   string
	}{
		"legacy release without bundle": {func(in *ExportInput) {
			in.Release.BundleDigest = nil
			in.Release.ID = ""
		}, "identity"},
		"image by tag": {func(in *ExportInput) {
			in.Images = []ImageRef{{Name: "x", Repository: "ghcr.io/example/x", Digest: "latest"}}
		}, "not a tag"},
		"no images": {func(in *ExportInput) { in.Images = nil }, "at least one image"},
		"wrong evaluation": {func(in *ExportInput) {
			in.Evaluation = seal(t, weightsbundle.KindEvaluation, map[string]any{"disposition": "shadow_candidate"})
		}, "not the release's evaluation"},
		"held evaluation": {func(in *ExportInput) {
			e := map[string]any{}
			for k, v := range in.Evaluation {
				if k != "id" && k != "kind" && k != "schema" {
					e[k] = v
				}
			}
			e["disposition"] = "hold"
			in.Evaluation = seal(t, weightsbundle.KindEvaluation, e)
		}, "not the release's evaluation"},
		"dataset manifest drift": {func(in *ExportInput) { in.DatasetManifest = []byte(`{}`) }, "not the lineage pin"},
		"bundle not the release's": {func(in *ExportInput) {
			os.WriteFile(filepath.Join(in.BundleRoot, "model.json"), []byte(`{"kind":"other"}`), 0o600)
		}, "altered"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			in := f.input()
			tc.mutate(&in)
			_, err := Export(in, filepath.Join(t.TempDir(), "x"))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %s", err, tc.want)
			}
		})
	}
	_ = f
}
