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
	policy, err := weightsbundle.DecodeRecord(read("python-policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	return CandidateInput{Manifest: manifest, Pin: pin, Receipt: receipt, Predictions: predictions, Bundle: bundle,
		Weights: read(filepath.Join("python-bundle", "model.json")),
		Exports: []ExportInput{{Manifest: read("python-export-manifest.json"), Dataset: read("python-dataset.json")}}, Policy: policy}
}

// resealed returns the fixture policy with one change, sealed again, so the
// record verifies but says something else.
func resealed(t *testing.T, in CandidateInput, mutate func(*EvaluationPolicy)) weightsbundle.Record {
	t.Helper()
	policy, _, err := DecodePolicyRecord(in.Policy)
	if err != nil {
		t.Fatal(err)
	}
	mutate(&policy)
	record, err := weightsbundle.Seal(weightsbundle.KindEvaluationPolicy, policy.Fields())
	if err != nil {
		t.Fatal(err)
	}
	return record
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
			m := in.Exports[0].Manifest
			in.Exports[0].Manifest = append([]byte(nil), m[:len(m)-1]...)
		},
		"dataset altered": func(in *CandidateInput) {
			in.Exports[0].Dataset = append(append([]byte(nil), in.Exports[0].Dataset...), ' ')
		},
		"export missing":            func(in *CandidateInput) { in.Exports = nil },
		"export presented twice":    func(in *CandidateInput) { in.Exports = append(in.Exports, in.Exports[0]) },
		"evaluation cohort foreign": func(in *CandidateInput) { in.EvaluationCohort = "cohort-9" },
		"policy tampered": func(in *CandidateInput) {
			in.Policy = cloneRecord(in.Policy)
			in.Policy["minimum_positives"] = 1
			in.Policy["name"] = "edited after sealing"
		},
		"policy without split": func(in *CandidateInput) {
			in.Policy = resealed(t, *in, func(p *EvaluationPolicy) { p.Split = "" })
		},
		"policy recall over one": func(in *CandidateInput) {
			in.Policy = resealed(t, *in, func(p *EvaluationPolicy) { p.MinimumRecall = 1.5 })
		},
		"policy specificity zero": func(in *CandidateInput) {
			in.Policy = resealed(t, *in, func(p *EvaluationPolicy) { p.MinimumSpecificity = 0 })
		},
		"policy label unknown": func(in *CandidateInput) {
			in.Policy = resealed(t, *in, func(p *EvaluationPolicy) { p.PositiveLabel = "maybe" })
		},
		"split not reserved": func(in *CandidateInput) {
			in.Policy = resealed(t, *in, func(p *EvaluationPolicy) { p.Split = "nope" })
		},
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

func cloneRecord(r weightsbundle.Record) weightsbundle.Record {
	out := make(weightsbundle.Record, len(r))
	for k, v := range r {
		out[k] = v
	}
	return out
}

func fixturePolicy(t *testing.T, in CandidateInput) EvaluationPolicy {
	t.Helper()
	policy, _, err := DecodePolicyRecord(in.Policy)
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

// A gate the policy declares that the student does not clear, or a leak
// between training and evaluation membership, is a reason and a hold; the
// evaluation never authorizes anything beyond full review, and it records
// the policy it was held to.
func TestEvaluationHoldsOnGatesAndLeaks(t *testing.T) {
	in := fixtureInput(t)
	in.Policy = resealed(t, in, func(p *EvaluationPolicy) { p.MinimumPositives = 5 })
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
	recorded, _ := set.Evaluation["policy"].(map[string]any)
	if recorded["minimum_positives"] != json.Number("5") || set.Evaluation["evaluation_policy_id"] != in.Policy["id"] || set.Candidate["evaluation_policy_id"] != in.Policy["id"] {
		t.Fatalf("policy not recorded on the evaluation: %v", set.Evaluation)
	}
	student, err := DecodeLinearStudent(in.Weights)
	if err != nil {
		t.Fatal(err)
	}
	policy := fixturePolicy(t, fixtureInput(t))
	leaked, err := Evaluate(student, in.Exports, "cohort-1", []string{"row-4", "row-0"}, nil, policy)
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
	if err := json.Unmarshal(in.Exports[0].Manifest, &export); err != nil {
		t.Fatal(err)
	}
	rows := export["rows"].([]any)
	byID := map[string]map[string]any{}
	for _, r := range rows {
		row := r.(map[string]any)
		byID[row["id"].(string)] = row
	}
	byID["row-4"]["sources"] = byID["row-0"]["sources"]
	shared, _ := json.Marshal(export)
	withShared := []ExportInput{{Manifest: shared, Dataset: in.Exports[0].Dataset}}
	content, err := Evaluate(student, withShared, "cohort-1", []string{"row-0", "row-1", "row-2"}, nil, policy)
	if err != nil {
		t.Fatal(err)
	}
	if content.Disposition != "hold" || !strings.Contains(strings.Join(content.Reasons, ";"), "content overlap: row-4") {
		t.Fatalf("%+v", content)
	}
	byID["row-4"]["sources"] = []any{map[string]any{"Digest": "elsewhere"}}
	byID["row-4"]["feature_sha256"] = byID["row-0"]["feature_sha256"]
	shared, _ = json.Marshal(export)
	withShared = []ExportInput{{Manifest: shared, Dataset: in.Exports[0].Dataset}}
	if same, err := Evaluate(student, withShared, "cohort-1", []string{"row-0", "row-1", "row-2"}, nil, policy); err != nil || same.Disposition != "shadow_candidate" {
		t.Fatalf("feature equality treated as a leak: %+v %v", same, err)
	}
	in.Policy = resealed(t, fixtureInput(t), func(p *EvaluationPolicy) { p.MinimumRecall, p.MinimumSpecificity = 1, 1 })
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

// A student that answers the same label for every row clears the recall
// gate or the specificity gate, never both; the metrics record which rows
// it got wrong on each side.
func TestConstantStudentCannotPassBothQualityGates(t *testing.T) {
	in := fixtureInput(t)
	policy := fixturePolicy(t, in)
	student, err := DecodeLinearStudent(in.Weights)
	if err != nil {
		t.Fatal(err)
	}
	alwaysYes := student
	alwaysYes.Coefficients = make([]float64, len(student.Coefficients))
	alwaysYes.Intercept, alwaysYes.Threshold = 100, 0
	yes, err := Evaluate(alwaysYes, in.Exports, "cohort-1", nil, nil, policy)
	if err != nil {
		t.Fatal(err)
	}
	if yes.Disposition != "hold" || !strings.Contains(strings.Join(yes.Reasons, ";"), "specificity below") || yes.Metrics[0]["recall"] != 1.0 || yes.Metrics[0]["specificity"] != 0.0 {
		t.Fatalf("always-positive student passed: %+v", yes)
	}
	if fp, _ := yes.Metrics[0]["false_positive_ids"].([]string); len(fp) != 1 || fp[0] != "row-5" {
		t.Fatalf("false positives not recorded: %+v", yes.Metrics[0])
	}
	alwaysNo := alwaysYes
	alwaysNo.Intercept = -100
	no, err := Evaluate(alwaysNo, in.Exports, "cohort-1", nil, nil, policy)
	if err != nil {
		t.Fatal(err)
	}
	if no.Disposition != "hold" || !strings.Contains(strings.Join(no.Reasons, ";"), "recall below") || no.Metrics[0]["specificity"] != 1.0 {
		t.Fatalf("always-negative student passed: %+v", no)
	}
	// Flipping the positive label does not help: the constant answer is
	// then the negative class and fails the specificity gate instead.
	flipped := policy
	flipped.PositiveLabel = "no"
	if out, err := Evaluate(alwaysNo, in.Exports, "cohort-1", nil, nil, flipped); err != nil || out.Disposition != "hold" || !strings.Contains(strings.Join(out.Reasons, ";"), "specificity below") {
		t.Fatalf("flipped label let a constant student through: %+v %v", out, err)
	}
}

// A run that trained on several cohorts is checked for leaks against every
// one of them: a reserved row of the evaluation cohort whose source content
// is behind a training row of another cohort is a leak. (A training cohort
// whose export is not presented refuses the seal; the receipt verifier
// already binds the actual cohorts to the manifest, so that refusal is
// exercised through the export-missing case above.)
func TestLeaksAreCheckedAcrossEveryTrainingCohort(t *testing.T) {
	in := fixtureInput(t)
	policy := fixturePolicy(t, in)
	student, err := DecodeLinearStudent(in.Weights)
	if err != nil {
		t.Fatal(err)
	}
	// A second cohort whose only row carries the source content of the
	// evaluation cohort's reserved row-4, under an id the first export
	// never saw.
	var export map[string]any
	if err := json.Unmarshal(in.Exports[0].Manifest, &export); err != nil {
		t.Fatal(err)
	}
	var dataset map[string]any
	if err := json.Unmarshal(in.Exports[0].Dataset, &dataset); err != nil {
		t.Fatal(err)
	}
	rows := export["rows"].([]any)
	var row4 map[string]any
	for _, r := range rows {
		if r.(map[string]any)["id"] == "row-4" {
			row4 = r.(map[string]any)
		}
	}
	otherRow := map[string]any{}
	for k, v := range row4 {
		otherRow[k] = v
	}
	otherRow["id"] = "b-row-9"
	otherRow["family_ids"] = []any{"family-b"}
	otherRow["exclusion_reasons"] = []any{}
	dataRows := dataset["rows"].([]any)
	var dataRow4 map[string]any
	for _, r := range dataRows {
		if r.(map[string]any)["id"] == "row-4" {
			dataRow4 = r.(map[string]any)
		}
	}
	otherData := map[string]any{}
	for k, v := range dataRow4 {
		otherData[k] = v
	}
	otherData["id"] = "b-row-9"
	otherData["split"] = "train"
	dataset["rows"] = []any{otherData}
	otherDataset, _ := json.Marshal(dataset)
	export["rows"] = []any{otherRow}
	export["cohort_id"] = "cohort-2"
	export["dataset_sha256"] = sha256Hex(otherDataset)
	otherExport, _ := json.Marshal(export)
	exports := append(in.Exports, ExportInput{Manifest: otherExport, Dataset: otherDataset})
	// Without the second export the leak is invisible; with it, it is a hold.
	blind, err := Evaluate(student, in.Exports, "cohort-1", []string{"row-0", "row-1", "row-2", "b-row-9"}, nil, policy)
	if err != nil || blind.Disposition != "shadow_candidate" {
		t.Fatalf("baseline: %+v %v", blind, err)
	}
	seen, err := Evaluate(student, exports, "cohort-1", []string{"row-0", "row-1", "row-2", "b-row-9"}, nil, policy)
	if err != nil {
		t.Fatal(err)
	}
	if seen.Disposition != "hold" || !strings.Contains(strings.Join(seen.Reasons, ";"), "content overlap: row-4") {
		t.Fatalf("cross-cohort content leak missed: %+v", seen)
	}
	if _, err := Evaluate(student, exports, "", nil, nil, policy); err == nil {
		t.Fatal("no evaluation cohort named among several")
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
