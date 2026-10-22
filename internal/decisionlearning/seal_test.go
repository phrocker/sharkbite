// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisionlearning

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/distillation"
	"github.com/phrocker/shoal-oss/internal/releasebundle"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
	"github.com/phrocker/shoal-oss/internal/weightsbundle"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// fixtureAuthority serves the Python worker's fixture export: the dataset
// and manifest the recorded run actually trained on.
type fixtureAuthority struct{ dataset, manifest []byte }

func (f fixtureAuthority) ListCohorts(context.Context) ([]Cohort, error) {
	return []Cohort{{ID: "cohort-1", TaskID: "synthetic-task", LabelPolicyID: "policy", Members: 6, Mode: "reconstructed"}}, nil
}

func (f fixtureAuthority) Export(_ context.Context, id shoal.ID) (Export, error) {
	if id != "cohort-1" {
		return Export{}, shoal.NewError(shoal.ErrorNotFound, "unknown cohort")
	}
	return Export{Dataset: f.dataset, Manifest: f.manifest}, nil
}

type fakePredictor struct{ report map[string]any }

func (p fakePredictor) ServingReport(context.Context) (map[string]any, error) { return p.report, nil }
func (p fakePredictor) PredictFeatures(_ context.Context, features []float64) (map[string]any, error) {
	if len(features) != 2 {
		return nil, errors.New("student runtime at /var/run/student refused the width")
	}
	return map[string]any{"status": "completed", "answers": []any{map[string]any{"question_id": "property", "p_true": 1.0}}}, nil
}

func fixtureFile(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "distillation", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// A completed run is turned into recorded candidate, evaluation, and
// artifact in one call, from the records the host already holds plus the
// bundle; the policy is the one declared on the job before its receipt, the
// bundle lands on the serving root, and the sealed records promote. A
// receipt before any policy, a policy after the receipt, a bundle the
// receipt did not produce, and an unrecorded run are refused, and no host
// path reaches the wire.
func TestSealCandidateRecordsARunFromItsOutputs(t *testing.T) {
	ctx := actorCtx(context.Background())
	store := memory.New()
	registry, err := weightsbundle.OpenRegistry(ctx, store, "learning/releases")
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	authority := fixtureAuthority{dataset: fixtureFile(t, "python-dataset.json"), manifest: fixtureFile(t, "python-export-manifest.json")}
	svc, err := New(Config{
		Registry: registry, Store: store, Prefix: "learning", BundlesDir: filepath.Join(base, "bundles"), ServingRoot: filepath.Join(base, "serving"),
		Cohorts: authority, Exporter: authority, Egress: fakeEgress{},
		Images: []releasebundle.ImageRef{{Name: "student-runtime", Repository: "ghcr.io/example/student", Digest: "sha256:" + strings.Repeat("a", 64)}},
		Now:    func() time.Time { return time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	decode := func(raw json.RawMessage, err error) map[string]any {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	rawManifest := fixtureFile(t, "python-manifest.json")
	pin := weightsbundle.SHA256Hex(rawManifest)
	bundle := map[string]string{}
	for _, name := range []string{"bundle.json", "model.json", "runtime.json"} {
		bundle[name] = base64.StdEncoding.EncodeToString(fixtureFile(t, filepath.Join("python-bundle", name)))
	}
	body := func(mutate func(map[string]any)) []byte {
		m := map[string]any{"manifest_pin": pin, "bundle": bundle}
		if mutate != nil {
			mutate(m)
		}
		raw, _ := json.Marshal(m)
		return raw
	}
	// The declaration is the fixture policy's fields; the host seals it
	// bound to the pin and must arrive at the fixture's record identity.
	fixturePolicy, _ := weightsbundle.DecodeRecord(fixtureFile(t, "python-policy.json"))
	declaration := map[string]any{}
	for k, v := range fixturePolicy {
		if k != "id" && k != "kind" && k != "schema" {
			declaration[k] = v
		}
	}
	rawDeclaration, _ := json.Marshal(declaration)
	// Nothing recorded yet: the run must be on the host first.
	if _, err := svc.SealCandidate(ctx, body(nil)); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("unrecorded manifest: %v", err)
	}
	if _, err := svc.DeclarePolicy(ctx, pin, rawDeclaration); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("policy for an unrecorded job: %v", err)
	}
	decode(svc.RequestExport(ctx, "cohort-1"))
	decode(svc.SubmitJob(ctx, rawManifest))
	if _, err := svc.SealCandidate(ctx, body(nil)); !shoal.IsErrorCode(err, shoal.ErrorConflict) || !strings.Contains(err.Error(), "no recorded receipt") {
		t.Fatalf("without receipt: %v", err)
	}
	if _, err := svc.SubmitPredictions(ctx, pin, fixtureFile(t, "python-predictions.json")); !shoal.IsErrorCode(err, shoal.ErrorConflict) || !strings.Contains(err.Error(), "declare an evaluation policy") {
		t.Fatalf("predictions before policy: %v", err)
	}
	// A completed receipt is refused until the policy is declared.
	if _, err := svc.SubmitReceipt(ctx, pin, fixtureFile(t, "python-receipt.json")); !shoal.IsErrorCode(err, shoal.ErrorConflict) || !strings.Contains(err.Error(), "no evaluation policy") {
		t.Fatalf("receipt before policy: %v", err)
	}
	for name, broken := range map[string]string{
		"unknown member": strings.Replace(string(rawDeclaration), `{`, `{"manifest_pin":"`+strings.Repeat("0", 64)+`",`, 1),
		"no specificity": strings.Replace(string(rawDeclaration), `"minimum_specificity":0.5,`, ``, 1),
	} {
		if _, err := svc.DeclarePolicy(ctx, pin, json.RawMessage(broken)); err == nil {
			t.Fatalf("%s: declared", name)
		}
	}
	declared := decode(svc.DeclarePolicy(ctx, pin, rawDeclaration))
	if declared["policy_id"] != fixturePolicy["id"] {
		t.Fatalf("host sealed policy %v, fixture %v", declared["policy_id"], fixturePolicy["id"])
	}
	// Declaring again identically is fine; a different policy is refused.
	decode(svc.DeclarePolicy(ctx, pin, rawDeclaration))
	lenient := strings.Replace(string(rawDeclaration), `"minimum_positives":1`, `"minimum_positives":2`, 1)
	if _, err := svc.DeclarePolicy(ctx, pin, json.RawMessage(lenient)); !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("second policy: %v", err)
	}
	job := decode(svc.Job(ctx, pin))
	if policy, _ := job["evaluation_policy"].(map[string]any); policy["id"] != fixturePolicy["id"] {
		t.Fatalf("job view lacks the policy: %v", job["evaluation_policy"])
	}
	decode(svc.SubmitPredictions(ctx, pin, fixtureFile(t, "python-predictions.json")))
	if _, err := svc.DeclarePolicy(ctx, pin, rawDeclaration); !shoal.IsErrorCode(err, shoal.ErrorConflict) || !strings.Contains(err.Error(), "not predeclared") {
		t.Fatalf("policy after predictions: %v", err)
	}
	decode(svc.SubmitReceipt(ctx, pin, fixtureFile(t, "python-receipt.json")))
	// Once the outputs exist, no policy can be declared, not even the same one.
	if _, err := svc.DeclarePolicy(ctx, pin, rawDeclaration); !shoal.IsErrorCode(err, shoal.ErrorConflict) || !strings.Contains(err.Error(), "not predeclared") {
		t.Fatalf("policy after receipt: %v", err)
	}
	// A bundle that is not the receipt's is refused before any export is
	// fetched, and the serving root stays empty.
	foreign := map[string]string{}
	for k, v := range bundle {
		foreign[k] = v
	}
	foreign["model.json"] = base64.StdEncoding.EncodeToString(append([]byte(nil), fixtureFile(t, filepath.Join("python-bundle", "model.json"))...))
	foreignBundle := fixtureFile(t, filepath.Join("python-bundle", "bundle.json"))
	foreign["bundle.json"] = base64.StdEncoding.EncodeToString([]byte(strings.Replace(string(foreignBundle), `"name"`, `"name "`, 1)))
	_ = foreign
	if _, err := svc.SealCandidate(ctx, body(func(m map[string]any) { m["cohort_id"] = "cohort-9" })); !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("cohort not trained on: %v", err)
	}
	altered := map[string]string{}
	for k, v := range bundle {
		altered[k] = v
	}
	altered["model.json"] = base64.StdEncoding.EncodeToString([]byte(`{"schema":1,"kind":"linear-student"}`))
	if _, err := svc.SealCandidate(ctx, body(func(m map[string]any) { m["bundle"] = altered })); err == nil || strings.Contains(err.Error(), base) {
		t.Fatalf("altered bundle: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(base, "serving")); len(entries) != 0 {
		t.Fatalf("a refused bundle left something on the serving root: %v", entries)
	}
	if _, err := svc.SealCandidate(ctx, body(func(m map[string]any) { m["bundle"] = map[string]string{"../x": bundle["bundle.json"]} })); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("traversal name: %v", err)
	}
	sealed := decode(svc.SealCandidate(ctx, body(nil)))
	if sealed["disposition"] != "shadow_candidate" {
		t.Fatalf("%v", sealed)
	}
	digest := sealed["bundle_digest"].(string)
	if _, got, err := weightsbundle.Load(filepath.Join(base, "serving", digest)); err != nil || got != digest {
		t.Fatalf("bundle not on the serving root: %v", err)
	}
	if evaluation := sealed["evaluation"].(map[string]any); evaluation["evaluation_policy_id"] != fixturePolicy["id"] || evaluation["policy"] == nil {
		t.Fatalf("evaluation not under the declared policy: %v", evaluation)
	}
	expected, _ := weightsbundle.DecodeRecord(fixtureFile(t, "python-candidate-set.json"))
	for _, kind := range []string{"candidate", "evaluation", "artifact"} {
		want := expected[kind].(map[string]any)["id"]
		if got := sealed[kind].(map[string]any)["id"]; got != want {
			t.Fatalf("%s sealed as %v, python sealed %v", kind, got, want)
		}
	}
	candidate := sealed["candidate"].(map[string]any)
	evaluation := sealed["evaluation"].(map[string]any)
	// The records are recorded, the same call is idempotent, and the pair promotes.
	candidates := decode(svc.Candidates(ctx))
	if len(candidates["candidates"].([]any)) != 1 {
		t.Fatalf("%v", candidates)
	}
	evaluations := decode(svc.Evaluations(ctx, candidate["id"].(string)))
	if len(evaluations["evaluations"].([]any)) != 1 {
		t.Fatalf("%v", evaluations)
	}
	again := decode(svc.SealCandidate(ctx, body(nil)))
	if again["candidate"].(map[string]any)["id"] != candidate["id"] {
		t.Fatal("re-sealing produced another candidate")
	}
	// A candidate sealed offline under a policy the host did not declare is
	// refused at the candidate route; the same records under the declared
	// policy are accepted (idempotently, since the seal recorded them).
	artifact := sealed["artifact"].(map[string]any)
	submitCandidate := func(c map[string]any) error {
		raw, _ := json.Marshal(map[string]any{"candidate": c, "artifact": artifact})
		_, err := svc.SubmitCandidate(ctx, raw)
		return err
	}
	if err := submitCandidate(candidate); err != nil {
		t.Fatalf("declared policy refused at the candidate route: %v", err)
	}
	fields := map[string]any{}
	for k, v := range candidate {
		if k != "id" && k != "kind" && k != "schema" {
			fields[k] = v
		}
	}
	fields["evaluation_policy_id"] = strings.Repeat("b", 64)
	elsewhere, _ := weightsbundle.Seal(weightsbundle.KindCandidate, fields)
	published, _, err := weightsbundle.Load(filepath.Join(base, "serving", digest))
	if err != nil {
		t.Fatal(err)
	}
	elsewhereArtifact, err := weightsbundle.NewArtifact(elsewhere["id"].(string), evaluation["id"].(string), "cohort-1", published, artifact["runtime_digest"].(string))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"candidate": elsewhere, "artifact": elsewhereArtifact})
	if _, err := svc.SubmitCandidate(ctx, raw); !shoal.IsErrorCode(err, shoal.ErrorConflict) || !strings.Contains(err.Error(), "not the policy") {
		t.Fatalf("undeclared policy accepted: %v", err)
	}
	// A foreign occupant under the digest refuses a later seal by role only.
	occupied := filepath.Join(base, "serving", digest)
	if err := os.RemoveAll(occupied); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(occupied, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(occupied, "bundle.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SealCandidate(ctx, body(nil)); !shoal.IsErrorCode(err, shoal.ErrorConflict) || strings.Contains(err.Error(), base) {
		t.Fatalf("foreign occupant: %v", err)
	}
	if err := os.RemoveAll(occupied); err != nil {
		t.Fatal(err)
	}
	decode(svc.SealCandidate(ctx, body(nil)))
	promote, _ := json.Marshal(map[string]any{"candidate_id": candidate["id"], "evaluation_id": evaluation["id"]})
	release := decode(svc.Promote(ctx, promote))
	if release["release"].(map[string]any)["bundle_digest"] != digest {
		t.Fatalf("%v", release)
	}
	// Prediction goes to the runtime, or nowhere.
	if _, err := svc.Predict(ctx, []byte(`{"features":[1,2]}`)); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("no runtime: %v", err)
	}
	svc.cfg.Serving = fakePredictor{}
	answer := decode(svc.Predict(ctx, []byte(`{"features":[1,2]}`)))
	if answer["status"] != "completed" {
		t.Fatalf("%v", answer)
	}
	if _, err := svc.Predict(ctx, []byte(`{"features":[1]}`)); err == nil || strings.Contains(err.Error(), "/var/run") {
		t.Fatalf("runtime cause leaked: %v", err)
	}
	if _, err := svc.Predict(ctx, []byte(`{"features":[]}`)); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("empty features: %v", err)
	}
	_ = distillation.StudentKind
}
