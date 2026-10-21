// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package distillation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/internal/weightsbundle"
)

func fixtureInput(t *testing.T) CandidateInput {
	t.Helper()
	read := func(name string) []byte {
		raw, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	manifest, err := Decode(read("python-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	pin, _ := manifest.Pin()
	var receipt Receipt
	if err := weightsbundle.StrictUnmarshal(read("python-receipt.json"), &receipt); err != nil {
		t.Fatal(err)
	}
	var predictions PredictionSet
	if err := weightsbundle.StrictUnmarshal(read("python-predictions.json"), &predictions); err != nil {
		t.Fatal(err)
	}
	bundle, _, err := weightsbundle.Load(filepath.Join("testdata", "python-bundle"))
	if err != nil {
		t.Fatal(err)
	}
	policy, err := DecodeEvaluationPolicy(read("python-policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	return CandidateInput{Manifest: manifest, Pin: pin, Receipt: receipt, Predictions: predictions, Bundle: bundle,
		Weights: read(filepath.Join("python-bundle", "model.json")), ExportManifest: read("python-export-manifest.json"),
		Dataset: read("python-dataset.json"), Policy: policy}
}

// Every input the set is sealed from is bound to the others: a bundle the
// receipt did not produce, a foreign prediction set, an export the manifest
// did not pin, a student trained under another manifest, and a policy that
// gates nothing are refused before anything is sealed.
func TestCandidateSetRefusesInputsThatDoNotBelongTogether(t *testing.T) {
	for name, mutate := range map[string]func(*CandidateInput){
		"foreign pin":            func(in *CandidateInput) { in.Pin = strings.Repeat("0", 64) },
		"receipt for a bundle":   func(in *CandidateInput) { in.Receipt.BundleDigest = strings.Repeat("1", 64) },
		"run not completed":      func(in *CandidateInput) { in.Receipt.Status = StatusFailed },
		"predictions of another": func(in *CandidateInput) { in.Predictions.Predictions = in.Predictions.Predictions[:1] },
		"weights swapped":        func(in *CandidateInput) { in.Weights = []byte(`{"schema":1,"kind":"linear-student"}`) },
		"export not pinned": func(in *CandidateInput) {
			in.ExportManifest = append([]byte(nil), in.ExportManifest[:len(in.ExportManifest)-1]...)
		},
		"dataset altered":        func(in *CandidateInput) { in.Dataset = append(append([]byte(nil), in.Dataset...), ' ') },
		"policy without split":   func(in *CandidateInput) { in.Policy.Split = "" },
		"policy recall over one": func(in *CandidateInput) { in.Policy.MinimumRecall = 1.5 },
		"policy label unknown":   func(in *CandidateInput) { in.Policy.PositiveLabel = "maybe" },
		"split not reserved":     func(in *CandidateInput) { in.Policy.Split = "nope" },
	} {
		in := fixtureInput(t)
		mutate(&in)
		if _, err := BuildCandidateSet(in); err == nil {
			t.Errorf("%s: sealed", name)
		}
	}
	if _, err := BuildCandidateSet(fixtureInput(t)); err != nil {
		t.Fatal(err)
	}
}

// A gate the policy declares that the student does not clear, or a leak
// between training and evaluation membership, is a reason and a hold; the
// evaluation never authorizes anything beyond full review.
func TestEvaluationHoldsOnGatesAndLeaks(t *testing.T) {
	in := fixtureInput(t)
	in.Policy.MinimumPositives = 5
	set, err := BuildCandidateSet(in)
	if err != nil {
		t.Fatal(err)
	}
	reasons, _ := set.Evaluation["reasons"].([]any)
	if set.Evaluation["disposition"] != "hold" || len(reasons) != 1 || !strings.Contains(reasons[0].(string), "sample gate") {
		t.Fatalf("%v", set.Evaluation)
	}
	if set.Evaluation["permitted_action"] != PermittedAction || set.Evaluation["optimization_enabled"] != false {
		t.Fatalf("%v", set.Evaluation)
	}
	student, err := DecodeLinearStudent(in.Weights)
	if err != nil {
		t.Fatal(err)
	}
	leaked, err := Evaluate(student, in.Dataset, in.ExportManifest, []string{"row-4", "row-0"}, nil, in.Policy)
	if err != nil {
		t.Fatal(err)
	}
	if leaked.Disposition != "hold" || !strings.Contains(strings.Join(leaked.Reasons, ";"), "membership overlap: row-4") {
		t.Fatalf("%+v", leaked)
	}
	if len(leaked.Comparisons) != 0 {
		t.Fatalf("no teacher given, comparison produced: %+v", leaked.Comparisons)
	}
	// Content identity is the source digest, not the feature vector: a
	// reserved row whose source is also behind a training row is a leak,
	// while two rows that merely share a feature vector are not.
	var export map[string]any
	if err := json.Unmarshal(in.ExportManifest, &export); err != nil {
		t.Fatal(err)
	}
	rows := export["rows"].([]any)
	byID := map[string]map[string]any{}
	for _, r := range rows {
		row := r.(map[string]any)
		byID[row["id"].(string)] = row
	}
	byID["row-4"]["sources"] = byID["row-0"]["sources"]
	policy := fixtureInput(t).Policy
	shared, _ := json.Marshal(export)
	content, err := Evaluate(student, in.Dataset, shared, []string{"row-0", "row-1", "row-2"}, nil, policy)
	if err != nil {
		t.Fatal(err)
	}
	if content.Disposition != "hold" || !strings.Contains(strings.Join(content.Reasons, ";"), "content overlap: row-4") {
		t.Fatalf("%+v", content)
	}
	byID["row-4"]["sources"] = []any{map[string]any{"Digest": "elsewhere"}}
	byID["row-4"]["feature_sha256"] = byID["row-0"]["feature_sha256"]
	shared, _ = json.Marshal(export)
	if same, err := Evaluate(student, in.Dataset, shared, []string{"row-0", "row-1", "row-2"}, nil, policy); err != nil || same.Disposition != "shadow_candidate" {
		t.Fatalf("feature equality treated as a leak: %+v %v", same, err)
	}
	in.Policy.MinimumPositives = 1
	in.Policy.MinimumRecall = 1
	passed, err := BuildCandidateSet(in)
	if err != nil {
		t.Fatal(err)
	}
	comparisons, _ := passed.Evaluation["comparisons"].([]any)
	if passed.Evaluation["disposition"] != "shadow_candidate" || len(comparisons) != 1 {
		t.Fatalf("%v", passed.Evaluation)
	}
	// The same inputs seal to the same identities every time.
	again, _ := BuildCandidateSet(in)
	if again.Evaluation["id"] != passed.Evaluation["id"] || again.Candidate["id"] != passed.Candidate["id"] || again.Artifact["id"] != passed.Artifact["id"] {
		t.Fatal("sealing is not deterministic")
	}
}

// The student decoder and predictor refuse what the runtime refuses: an
// unbounded or non-finite model, and a feature vector of another width.
func TestLinearStudentBounds(t *testing.T) {
	for name, raw := range map[string]string{
		"wrong kind":       `{"schema":1,"kind":"svm","coefficients":[1],"intercept":0,"threshold":0,"labels":["a","b"],"task_id":"t","distillation_manifest_pin":"` + strings.Repeat("a", 64) + `"}`,
		"no coefficients":  `{"schema":1,"kind":"linear-student","coefficients":[],"intercept":0,"threshold":0,"labels":["a","b"],"task_id":"t","distillation_manifest_pin":"` + strings.Repeat("a", 64) + `"}`,
		"one label":        `{"schema":1,"kind":"linear-student","coefficients":[1],"intercept":0,"threshold":0,"labels":["a","a"],"task_id":"t","distillation_manifest_pin":"` + strings.Repeat("a", 64) + `"}`,
		"overflowing":      `{"schema":1,"kind":"linear-student","coefficients":[1e400],"intercept":0,"threshold":0,"labels":["a","b"],"task_id":"t","distillation_manifest_pin":"` + strings.Repeat("a", 64) + `"}`,
		"unknown member":   `{"schema":1,"kind":"linear-student","coefficients":[1],"intercept":0,"threshold":0,"labels":["a","b"],"task_id":"t","distillation_manifest_pin":"` + strings.Repeat("a", 64) + `","extra":1}`,
		"pin not a digest": `{"schema":1,"kind":"linear-student","coefficients":[1],"intercept":0,"threshold":0,"labels":["a","b"],"task_id":"t","distillation_manifest_pin":"x"}`,
	} {
		if _, err := DecodeLinearStudent([]byte(raw)); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
	student, err := DecodeLinearStudent([]byte(`{"schema":1,"kind":"linear-student","coefficients":[2,-1],"intercept":0.5,"threshold":0,"labels":["no","yes"],"task_id":"t","feature_schema_id":"f","distillation_manifest_pin":"` + strings.Repeat("a", 64) + `","teacher":"x","base":"y","epochs":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if label, margin, err := student.Predict([]float64{1, 1}); err != nil || label != "yes" || margin != 1.5 {
		t.Fatalf("%s %v %v", label, margin, err)
	}
	if label, _, err := student.Predict([]float64{-1, 1}); err != nil || label != "no" {
		t.Fatalf("%s %v", label, err)
	}
	if _, _, err := student.Predict([]float64{1}); err == nil {
		t.Fatal("width mismatch accepted")
	}
}
