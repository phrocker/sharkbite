// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/internal/weightsbundle"
)

// The CLI seals the same records as the Python worker and the Go contract
// test from the same fixture, and publishes the bundle without replacing an
// occupant.
func TestLearningCandidateSealsTheFixtureIdentically(t *testing.T) {
	fixture := filepath.Join("..", "..", "internal", "distillation", "testdata")
	rawManifest, err := os.ReadFile(filepath.Join(fixture, "python-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	pin := weightsbundle.SHA256Hex(rawManifest)
	out := t.TempDir()
	serving := filepath.Join(t.TempDir(), "serving")
	args := []string{"learning", "candidate",
		"-manifest", filepath.Join(fixture, "python-manifest.json"), "-manifest-pin", pin,
		"-receipt", filepath.Join(fixture, "python-receipt.json"), "-predictions", filepath.Join(fixture, "python-predictions.json"),
		"-bundle", filepath.Join(fixture, "python-bundle"), "-export-manifest", filepath.Join(fixture, "python-export-manifest.json"),
		"-dataset", filepath.Join(fixture, "python-dataset.json"), "-policy", filepath.Join(fixture, "python-policy.json"),
		"-output", out, "-serving-root", serving}
	var stdout, stderr bytes.Buffer
	if err := run(args, &stdout, &stderr); err != nil {
		t.Fatalf("%v\n%s", err, stderr.String())
	}
	var summary map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	if summary["disposition"] != "shadow_candidate" || summary["bundle"] != "published" {
		t.Fatalf("%v", summary)
	}
	got, err := os.ReadFile(filepath.Join(out, "candidate-set.json"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join(fixture, "python-candidate-set.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("CLI records differ from the Python fixture\ncli:    %s\npython: %s", got, want)
	}
	if _, digest, err := weightsbundle.Load(filepath.Join(serving, summary["bundle_digest"].(string))); err != nil || digest != summary["bundle_digest"] {
		t.Fatalf("bundle not published: %v %s", err, digest)
	}
	// A second run finds the bundle present and seals the same records.
	stdout.Reset()
	if err := run(args, &stdout, &stderr); err != nil {
		t.Fatalf("%v\n%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"bundle":"present"`) {
		t.Fatalf("second run: %s", stdout.String())
	}
	// A policy written by hand is not the host's sealed record and is
	// refused; a missing policy is a usage error; one -dataset per
	// -export-manifest; and a foreign pin is refused before anything is sealed.
	handWritten := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(handWritten, []byte(`{"name":"mine","split":"test","positive_label":"yes","minimum_recall":0.5,"minimum_specificity":0.5,"minimum_positives":1,"minimum_families":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	unsealed := append([]string(nil), args...)
	for i, a := range unsealed {
		if a == "-policy" {
			unsealed[i+1] = handWritten
		}
	}
	if err := run(unsealed, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "evaluation_policy identity") {
		t.Fatalf("hand-written policy: %v", err)
	}
	if err := run([]string{"learning", "candidate", "-manifest", filepath.Join(fixture, "python-manifest.json")}, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "requires") {
		t.Fatalf("missing flags: %v", err)
	}
	if err := run(append(append([]string(nil), args...), "-dataset", filepath.Join(fixture, "python-dataset.json")), &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "one -dataset per -export-manifest") {
		t.Fatalf("unpaired dataset: %v", err)
	}
	wrong := append([]string(nil), args...)
	wrong[5] = strings.Repeat("0", 64)
	if err := run(wrong, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "not the declared pin") {
		t.Fatalf("foreign pin: %v", err)
	}
}
