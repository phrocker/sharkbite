package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T) (string, string, string, manifest) {
	t.Helper()
	dir := t.TempDir()
	if e := os.Mkdir(filepath.Join(dir, "inputs"), 0700); e != nil {
		t.Fatal(e)
	}
	task, e := taskSpec()
	if e != nil {
		t.Fatal(e)
	}
	model, _ := json.Marshal(map[string]any{"schema": 1, "kind": "linear-svm", "task_id": task.ID(), "question_id": question, "feature_schema_id": featureSchema, "dataset_sha256": strings.Repeat("a", 64), "recipe_sha256": strings.Repeat("b", 64), "training_runtime_sha256": strings.Repeat("c", 64), "labels": []string{"lower_priority", "retain"}, "coefficients": []float64{2, -1}, "intercept": 0, "threshold": 0})
	write(t, dir, "model.json", model)
	input := []byte(`{"schema":1,"feature_schema_id":"` + featureSchema + `","subjects":[{"id":"s1","features":[2,1]}]}`)
	write(t, dir, "inputs/0000.json", input)
	m := manifest{Schema: 1, ModelSHA256: digest(model), TaskID: string(task.ID()), QuestionID: question, FeatureSchemaID: featureSchema, Rows: []row{{ID: "s1", InputFile: "inputs/0000.json", InputSHA256: digest(input), ExpectedLabel: "retain", OriginalScore: 0.9}}, Provenance: fixtureProvenance()}
	b, _ := json.Marshal(m)
	write(t, dir, "manifest.json", b)
	return dir, digest(model), digest(b), m
}
func write(t *testing.T, dir, name string, b []byte) {
	t.Helper()
	if e := os.WriteFile(filepath.Join(dir, name), b, 0600); e != nil {
		t.Fatal(e)
	}
}
func TestReplayDeterministicAndFullCohortCLI(t *testing.T) {
	dir, mh, ph, _ := fixture(t)
	first, e := replay(dir, mh, ph, 1)
	if e != nil {
		t.Fatal(e)
	}
	second, e := replay(dir, mh, ph, 1)
	if e != nil {
		t.Fatal(e)
	}
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(second)
	if !bytes.Equal(a, b) || first.Matches != 1 || first.Mismatches != 0 || !first.AllFullReview || first.OptimizationEnabled || first.Labels[0] != "retain" {
		t.Fatalf("unexpected replay %s", a)
	}
	if e = run([]string{"replay", "--bundle", dir, "--model-sha256", mh, "--manifest-sha256", ph}, &bytes.Buffer{}); e == nil {
		t.Fatal("CLI accepted incomplete cohort")
	}
}
func TestRejectPins(t *testing.T) {
	dir, mh, ph, _ := fixture(t)
	for _, pins := range [][2]string{{"", ph}, {mh, ""}, {strings.Repeat("0", 64), ph}, {mh, strings.Repeat("0", 64)}} {
		if _, e := replay(dir, pins[0], pins[1], 1); e == nil {
			t.Fatal("accepted substituted digest")
		}
	}
	write(t, dir, "inputs/0000.json", []byte(`{}`))
	if _, e := replay(dir, mh, ph, 1); e == nil {
		t.Fatal("accepted substituted input")
	}
}
func TestRejectManifestAndMembership(t *testing.T) {
	for name, mutate := range map[string]func(*manifest){"path": func(m *manifest) { m.Rows[0].InputFile = "../model.json" }, "index": func(m *manifest) { m.Rows[0].InputFile = "inputs/0001.json" }, "task": func(m *manifest) { m.TaskID = "substituted" }, "subject": func(m *manifest) { m.Rows[0].ID = "other" }, "label": func(m *manifest) { m.Rows[0].ExpectedLabel = "other" }, "score": func(m *manifest) { m.Rows[0].OriginalScore = 2 }, "cohort": func(m *manifest) { m.Rows = nil }, "provenance": func(m *manifest) { m.Provenance = nil }} {
		t.Run(name, func(t *testing.T) {
			dir, mh, _, m := fixture(t)
			mutate(&m)
			b, _ := json.Marshal(m)
			write(t, dir, "manifest.json", b)
			if _, e := replay(dir, mh, digest(b), 1); e == nil {
				t.Fatal("accepted invalid manifest")
			}
		})
	}
}
func TestMismatchesReported(t *testing.T) {
	dir, mh, _, m := fixture(t)
	m.Rows[0].ExpectedLabel = "lower_priority"
	m.Rows[0].OriginalScore = 0.1
	b, _ := json.Marshal(m)
	write(t, dir, "manifest.json", b)
	out, e := replay(dir, mh, digest(b), 1)
	if e != nil {
		t.Fatal(e)
	}
	if out.Matches != 0 || out.Mismatches != 1 {
		t.Fatal(out)
	}
}
func TestRejectSymlinks(t *testing.T) {
	for _, member := range []string{"manifest.json", "model.json", "inputs/0000.json", "inputs"} {
		t.Run(member, func(t *testing.T) {
			dir, mh, ph, _ := fixture(t)
			old := filepath.Join(dir, member)
			target := old + ".real"
			if e := os.Rename(old, target); e != nil {
				t.Fatal(e)
			}
			if e := os.Symlink(target, old); e != nil {
				t.Fatal(e)
			}
			if _, e := replay(dir, mh, ph, 1); e == nil {
				t.Fatal("accepted symlink")
			}
		})
	}
}
func TestStrictManifest(t *testing.T) {
	dir, mh, _, _ := fixture(t)
	raw, e := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if e != nil {
		t.Fatal(e)
	}
	for _, b := range [][]byte{append(append([]byte{}, raw...), []byte(`{}`)...), append([]byte(`{"schema":1,`), raw[1:]...), append([]byte(`{"unknown":true,`), raw[1:]...)} {
		write(t, dir, "manifest.json", b)
		if _, e := replay(dir, mh, digest(b), 1); e == nil {
			t.Fatal("accepted ambiguous manifest")
		}
	}
}

func fixtureProvenance() json.RawMessage {
	members := map[string]any{}
	for path, hash := range originalMembers {
		members[path] = map[string]any{"path": path, "sha256": hash, "bytes": 1}
	}
	b, _ := json.Marshal(map[string]any{"archive_sha256": archiveHash, "action": "full_review", "original_threshold": originalThreshold, "optimization_enabled": false, "training_performed": false, "original_training_runtime_verified": false, "original_members": members, "original_model_id": "model", "original_inputs_id": "inputs", "original_prediction_id": "predictions", "original_selection_id": "selection", "runtime_binding": "import runtime only; original training runtime unavailable"})
	return b
}
func TestRejectProvenance(t *testing.T) {
	for _, key := range []string{"archive_sha256", "action", "original_threshold", "optimization_enabled", "training_performed", "original_training_runtime_verified", "original_members"} {
		t.Run(key, func(t *testing.T) {
			dir, mh, _, m := fixture(t)
			var p map[string]json.RawMessage
			json.Unmarshal(m.Provenance, &p)
			delete(p, key)
			m.Provenance, _ = json.Marshal(p)
			b, _ := json.Marshal(m)
			write(t, dir, "manifest.json", b)
			if _, e := replay(dir, mh, digest(b), 1); e == nil {
				t.Fatal("accepted missing provenance pin")
			}
		})
	}
}

func TestFullWidthNumericInput(t *testing.T) {
	dir, _, _, m := fixture(t)
	modelBytes, e := os.ReadFile(filepath.Join(dir, "model.json"))
	if e != nil {
		t.Fatal(e)
	}
	var model map[string]any
	if e = json.Unmarshal(modelBytes, &model); e != nil {
		t.Fatal(e)
	}
	coeff := make([]float64, 60000)
	coeff[0] = 1
	model["coefficients"] = coeff
	modelBytes, _ = json.Marshal(model)
	write(t, dir, "model.json", modelBytes)
	features := make([]float64, 60000)
	features[0] = 1
	input, _ := json.Marshal(map[string]any{"schema": 1, "feature_schema_id": featureSchema, "subjects": []any{map[string]any{"id": "s1", "features": features}}})
	write(t, dir, "inputs/0000.json", input)
	m.ModelSHA256 = digest(modelBytes)
	m.Rows[0].InputSHA256 = digest(input)
	b, _ := json.Marshal(m)
	write(t, dir, "manifest.json", b)
	out, e := replay(dir, m.ModelSHA256, digest(b), 1)
	if e != nil {
		t.Fatal(e)
	}
	if out.Matches != 1 {
		t.Fatal(out)
	}
}

// Case aliases and null/missing scalar values must not inherit a valid value
// from a second field or the zero value of the Go destination struct.
func TestRejectAmbiguousRequiredFields(t *testing.T) {
	for _, scope := range []string{"manifest", "row", "provenance", "member"} {
		for _, mutation := range []string{"null", "missing", "case-alias", "case-only"} {
			t.Run(scope+"/"+mutation, func(t *testing.T) {
				dir, mh, _, _ := fixture(t)
				b, e := os.ReadFile(filepath.Join(dir, "manifest.json"))
				if e != nil {
					t.Fatal(e)
				}
				var original map[string]any
				if e = json.Unmarshal(b, &original); e != nil {
					t.Fatal(e)
				}
				target := original
				switch scope {
				case "row":
					target = original["rows"].([]any)[0].(map[string]any)
					target["expected_label"] = "lower_priority"
					target["original_score"] = 0.0
				case "provenance":
					target = original["provenance"].(map[string]any)
				case "member":
					for _, m := range original["provenance"].(map[string]any)["original_members"].(map[string]any) {
						target = m.(map[string]any)
						break
					}
				}
				keys := []string{}
				for key := range target {
					keys = append(keys, key)
				}
				for _, key := range keys {
					value := target[key]
					alias := strings.ToUpper(key[:1]) + key[1:]
					switch mutation {
					case "null":
						target[key] = nil
					case "missing":
						delete(target, key)
					case "case-alias":
						target[alias] = value
					case "case-only":
						delete(target, key)
						target[alias] = value
					}
					altered, _ := json.Marshal(original)
					write(t, dir, "manifest.json", altered)
					if _, e := replay(dir, mh, digest(altered), 1); e == nil {
						t.Fatalf("accepted %s %s %s", scope, key, mutation)
					}
					delete(target, alias)
					target[key] = value
				}
			})
		}
	}
}

func TestManifestUnicode(t *testing.T) {
	for _, b := range [][]byte{[]byte(`{"a":"\ud800"}`), []byte(`{"a":"\udc00"}`), []byte(`{"a":"\ud800\u0041"}`), {'"', 0xff, '"'}} {
		if e := validateUnicode(b); e == nil {
			t.Fatalf("accepted invalid Unicode %q", b)
		}
	}
	for _, b := range [][]byte{[]byte(`{"a":"\ud83d\ude00"}`), []byte(`{"a":"\\ud800"}`), []byte(`{"a":"世界"}`)} {
		if e := validateUnicode(b); e != nil {
			t.Fatal(e)
		}
	}
}
