// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/adjudicatedfixture"
	"github.com/phrocker/shoal-oss/internal/decisionlearning"
	"github.com/phrocker/shoal-oss/internal/releasebundle"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
	"github.com/phrocker/shoal-oss/internal/weightsbundle"
	decisionapi "github.com/phrocker/shoal-oss/pkg/decision/api"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// The development cohort authority closes the console's dataset gap: with
// -learning-fixture-cohorts the datasets route lists a real sealed cohort
// and an export request records a pinned, source-bearing manifest.
func TestFixtureCohortsFeedTheLearningService(t *testing.T) {
	if _, err := parseFixtureCohortTasks("source,review,source"); err == nil {
		t.Fatal("duplicate task accepted")
	}
	if _, err := parseFixtureCohortTasks("other"); err == nil {
		t.Fatal("unknown task accepted")
	}
	tasks, err := parseFixtureCohortTasks(" source ")
	if err != nil || len(tasks) != 1 {
		t.Fatalf("%v %v", tasks, err)
	}
	state := t.TempDir()
	var output bytes.Buffer
	cohorts, err := mountFixtureCohorts(state, tasks, &output)
	if err != nil {
		t.Fatal(err)
	}
	defer cohorts.Close()
	ctx := decisionapi.WithLearningActor(context.Background(), decisionapi.LearningActor{Subject: "s", Actor: "owner"})
	store := memory.New()
	registry, err := weightsbundle.OpenRegistry(ctx, store, "learning/releases")
	if err != nil {
		t.Fatal(err)
	}
	svc, err := decisionlearning.New(decisionlearning.Config{
		Registry: registry, Store: store, Prefix: "learning", BundlesDir: filepath.Join(state, "bundles"), ServingRoot: filepath.Join(state, "serving"),
		Cohorts: cohorts, Exporter: cohorts, Egress: hostEgressResolver{"local": "loopback"},
		Images: []releasebundle.ImageRef{{Name: "student-runtime", Repository: "ghcr.io/example/student", Digest: "sha256:0000000000000000000000000000000000000000000000000000000000000000"}},
		Now:    func() time.Time { return time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := svc.Datasets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var datasets struct {
		Mounted bool `json:"authority_mounted"`
		Cohorts []struct {
			Cohort decisionlearning.Cohort `json:"cohort"`
		} `json:"cohorts"`
	}
	if err := json.Unmarshal(raw, &datasets); err != nil || !datasets.Mounted || len(datasets.Cohorts) != 1 || datasets.Cohorts[0].Cohort.Members != 16 || datasets.Cohorts[0].Cohort.Mode != "reconstructed" {
		t.Fatalf("%v %s", err, raw)
	}
	id := datasets.Cohorts[0].Cohort.ID
	raw, err = svc.RequestExport(ctx, string(id))
	if err != nil {
		t.Fatal(err)
	}
	var export struct {
		ManifestSHA256 string   `json:"manifest_sha256"`
		Rows           int      `json:"rows"`
		SourceIDs      []string `json:"source_ids"`
		Eligible       []string `json:"eligible_example_ids"`
	}
	if err := json.Unmarshal(raw, &export); err != nil || len(export.ManifestSHA256) != 64 || export.Rows != 16 || len(export.SourceIDs) == 0 || len(export.Eligible) == 0 {
		t.Fatalf("%v %s", err, raw)
	}
	if _, err := svc.RequestExport(ctx, "not-a-cohort"); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("unknown cohort: %v", err)
	}
	// Exports are re-obtainable at the same pin, which is what an offline
	// bundle export needs, and the dataset-exports provider serves the same
	// bytes so a trainer fetches exactly what was recorded.
	again, err := cohorts.Export(ctx, id)
	if err != nil || weightsbundle.SHA256Hex(again.Manifest) != export.ManifestSHA256 {
		t.Fatalf("re-export drifted: %v", err)
	}
	served, err := fixtureDatasetProvider{cohorts}.Export(ctx, id)
	if err != nil || served.ManifestSHA256 != export.ManifestSHA256 || !bytes.Equal(served.Manifest, again.Manifest) || len(served.Dataset) == 0 {
		t.Fatalf("dataset route bytes differ: %v", err)
	}
	// The export was sealed at mount time, under the demo's one-hour
	// exporter decision, so nothing on the request path depends on it.
	if cohorts.entries[id].sealed.ManifestSHA256 != export.ManifestSHA256 {
		t.Fatal("export not sealed at mount")
	}
	// A second preparation is a new cohort identity: a recorded export from
	// an earlier start is never attached to the cohort of this one.
	second, err := mountFixtureCohorts(t.TempDir(), tasks, &output)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if _, same := second.entries[id]; same {
		t.Fatal("cohort identity reused across preparations")
	}
	if !strings.HasPrefix(string(second.order[0]), "sealed-fixture-cohort:source:") {
		t.Fatalf("cohort id %s", second.order[0])
	}
	// The demo's own CLI keeps its stable identity.
	if r, eng, _, _, err := adjudicatedfixture.Prepare(filepath.Join(t.TempDir(), "demo"), "source"); err != nil {
		t.Fatal(err)
	} else {
		eng.Close()
		if r.Cohort().ID != "sealed-fixture-cohort:source" {
			t.Fatalf("demo cohort id %s", r.Cohort().ID)
		}
	}
}
