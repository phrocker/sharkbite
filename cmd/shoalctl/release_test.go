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

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/internal/weightsbundle"
)

func sealRecord(t *testing.T, kind string, fields map[string]any) weightsbundle.Record {
	t.Helper()
	r, err := weightsbundle.Seal(kind, fields)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

type connected struct {
	registryDir, bundleRoot, datasetManifestPath, evaluationPath string
	release                                                      weightsbundle.Release
}

// connectedSide promotes a bundle into a registry directory and returns what
// an export needs. model varies the weights so releases differ.
func connectedSide(t *testing.T, model string) connected {
	t.Helper()
	base := t.TempDir()
	bundleRoot := filepath.Join(base, "bundle")
	if err := os.Mkdir(bundleRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleRoot, "model.json"), []byte(model), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, err := weightsbundle.Build(bundleRoot, map[string]string{"model.json": weightsbundle.RoleWeights})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := weightsbundle.Stage(bundleRoot, manifest); err != nil {
		t.Fatal(err)
	}
	datasetManifest := []byte(`{"schema":1,"kind":"authorized-numeric-training-export","rows":[{"id":"r1","sources":[{"Digest":"src-1"}]}]}`)
	datasetManifestPath := filepath.Join(base, "dataset-manifest.json")
	if err := os.WriteFile(datasetManifestPath, datasetManifest, 0o600); err != nil {
		t.Fatal(err)
	}
	lineage, _ := weightsbundle.SourcesFromExportManifest(datasetManifest)
	weights, _ := manifest.File(weightsbundle.RoleWeights)
	candidate := sealRecord(t, weightsbundle.KindCandidate, map[string]any{"dataset_id": "d", "evaluation_policy_id": "p", "training_ids": []string{"r1"}, "model_digest": weights.SHA256, "policy_predeclared": true, "task_ids": []string{"task"}, "dataset_manifest_sha256": lineage.DatasetManifestSHA256})
	evaluation := sealRecord(t, weightsbundle.KindEvaluation, map[string]any{"candidate_id": candidate["id"], "dataset_id": "d", "evaluation_policy_id": "p", "ledger_id": "l", "disposition": "shadow_candidate", "reasons": []string{}})
	artifact, _ := weightsbundle.NewArtifact(candidate["id"].(string), evaluation["id"].(string), "d", manifest, "runtime")
	approval, _ := weightsbundle.NewPromotionApproval(candidate["id"].(string), evaluation["id"].(string), "owner")
	registryDir := filepath.Join(base, "registry")
	registry, err := openRegistry(context.Background(), registryDir)
	if err != nil {
		t.Fatal(err)
	}
	release, err := registry.Promote(context.Background(), weightsbundle.PromotionInput{Candidate: candidate, Evaluation: evaluation, Artifact: artifact, Approval: approval}, lineage)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := weightsbundle.EncodeRecord(evaluation)
	evaluationPath := filepath.Join(base, "evaluation.json")
	if err := os.WriteFile(evaluationPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return connected{registryDir, bundleRoot, datasetManifestPath, evaluationPath, release}
}

func (c connected) exportArgs(image, output string) []string {
	return []string{"release", "export", "-registry", c.registryDir, "-bundle-root", c.bundleRoot, "-dataset-manifest", c.datasetManifestPath, "-evaluation", c.evaluationPath, "-image", image, "-output", output}
}

func TestReleaseExportImportRollbackCLI(t *testing.T) {
	first := connectedSide(t, `{"kind":"linear-student","v":1}`)
	output := filepath.Join(t.TempDir(), "offline")
	var stdout, stderr bytes.Buffer
	image := "student-runtime=ghcr.io/example/shoal-student-runtime@sha256:" + strings.Repeat("a", 64)
	if err := run(first.exportArgs(image, output), &stdout, &stderr); err != nil {
		t.Fatalf("%v\n%s", err, stderr.String())
	}
	var exported map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &exported); err != nil || exported["release_id"] != first.release.ID {
		t.Fatalf("%v %s", err, stdout.String())
	}
	bundleDigest := exported["bundle_digest"].(string)
	stdout.Reset()
	if err := run([]string{"release", "verify", "-bundle", output}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), `"verified":true`) {
		t.Fatal(stdout.String())
	}
	if err := run(first.exportArgs("x=repo:tag", output+"2"), &stdout, &stderr); err == nil {
		t.Fatal("image by tag accepted")
	}

	// Disconnected side.
	offlineRegistry := filepath.Join(t.TempDir(), "registry")
	serving := filepath.Join(t.TempDir(), "releases")
	stdout.Reset()
	if err := run([]string{"release", "import", "-bundle", output, "-registry", offlineRegistry, "-serving-root", serving, "-importer", "op"}, &stdout, &stderr); err != nil {
		t.Fatalf("%v %s", err, stdout.String())
	}
	if !strings.Contains(stdout.String(), `"activated":true`) {
		t.Fatal(stdout.String())
	}
	if _, _, err := weightsbundle.Load(filepath.Join(serving, bundleDigest)); err != nil {
		t.Fatal(err)
	}

	// Alter one byte in a fresh export; import names the entry and writes nothing.
	altered := filepath.Join(t.TempDir(), "altered")
	if err := run(first.exportArgs(image, altered), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	modelPath := filepath.Join(altered, "bundle", "model.json")
	b, _ := os.ReadFile(modelPath)
	b[0] = '['
	os.WriteFile(modelPath, b, 0o600)
	stdout.Reset()
	err := run([]string{"release", "import", "-bundle", altered, "-registry", offlineRegistry, "-serving-root", serving, "-importer", "op"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), `altered (digest) "bundle/model.json"`) || !strings.Contains(stdout.String(), `"verified":false`) {
		t.Fatalf("%v %s", err, stdout.String())
	}

	// A bundle root changed after promotion no longer matches its release: export refuses.
	drifted := connectedSide(t, `{"kind":"linear-student","v":2}`)
	os.WriteFile(filepath.Join(drifted.bundleRoot, "model.json"), []byte(`{"kind":"linear-student","v":3}`), 0o600)
	if err := run(drifted.exportArgs(image, filepath.Join(t.TempDir(), "x")), &stdout, &stderr); err == nil {
		t.Fatal("export of a bundle that no longer matches its release was accepted")
	}

	// A second release, then rollback on the disconnected side with no new import.
	second := connectedSide(t, `{"kind":"linear-student","v":4}`)
	secondOut := filepath.Join(t.TempDir(), "second")
	if err := run(second.exportArgs(image, secondOut), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"release", "import", "-bundle", secondOut, "-registry", offlineRegistry, "-serving-root", serving, "-importer", "op"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if err := run([]string{"release", "rollback", "-registry", offlineRegistry, "-prior", first.release.ID, "-owner", "op"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), `"rollback_of":"`+second.release.ID+`"`) || !strings.Contains(stdout.String(), bundleDigest) {
		t.Fatal(stdout.String())
	}
	reopened, _ := openRegistry(context.Background(), offlineRegistry)
	if active, _ := reopened.Active(); active.BundleDigest == nil || *active.BundleDigest != bundleDigest {
		t.Fatal("rollback not durable")
	}
	if err := run([]string{"release", "bogus"}, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), releaseUsage) {
		t.Fatal(err)
	}
}
