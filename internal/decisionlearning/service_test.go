// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisionlearning

import (
	"context"
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

type fakeAuthority struct {
	manifest []byte
	err      error
}

func (f fakeAuthority) ListCohorts(context.Context) ([]Cohort, error) {
	return []Cohort{{ID: "cohort-1", TaskID: "task-v1", LabelPolicyID: "labels:1", Members: 3, Mode: "reconstructed"}}, nil
}

func (f fakeAuthority) Export(_ context.Context, id shoal.ID) (Export, error) {
	if f.err != nil {
		return Export{}, f.err
	}
	if id != "cohort-1" {
		return Export{}, shoal.NewError(shoal.ErrorNotFound, "unknown cohort")
	}
	return Export{Dataset: []byte(`{"rows":[]}`), Manifest: f.manifest}, nil
}

const exportManifest = `{"schema":1,"kind":"authorized-numeric-training-export","cohort_id":"cohort-1","task_id":"task-v1","label_policy_id":"labels:1","dataset_sha256":"` + "0000000000000000000000000000000000000000000000000000000000000000" + `","rows":[{"id":"ex-1","exclusion_reasons":[],"sources":[{"Digest":"src-1"}]},{"id":"ex-2","exclusion_reasons":[],"sources":[{"Digest":"src-2"}]},{"id":"ex-3","exclusion_reasons":["label_withdrawn"],"sources":[{"Digest":"src-3"}]}]}`

type harness struct {
	t       *testing.T
	svc     *Service
	ctx     context.Context
	serving string
	pin     string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	store := memory.New()
	registry, err := weightsbundle.OpenRegistry(ctx, store, "learning/releases")
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	evidence := filepath.Join(base, "evidence", "quality")
	os.MkdirAll(evidence, 0o700)
	report, _ := weightsbundle.Seal("quality_report", map[string]any{"metric": 1})
	raw, _ := weightsbundle.EncodeRecord(report)
	os.WriteFile(filepath.Join(evidence, "q1.json"), raw, 0o600)
	os.WriteFile(filepath.Join(evidence, "broken.json"), []byte("{"), 0o600)
	svc, err := New(Config{
		Registry: registry, Store: store, Prefix: "learning", EvidenceDir: filepath.Join(base, "evidence"),
		BundlesDir: filepath.Join(base, "bundles"), ServingRoot: filepath.Join(base, "serving"),
		Cohorts: fakeAuthority{manifest: []byte(exportManifest)}, Exporter: fakeAuthority{manifest: []byte(exportManifest)},
		Images: []releasebundle.ImageRef{{Name: "student-runtime", Repository: "ghcr.io/example/student", Digest: "sha256:" + strings.Repeat("a", 64)}},
		Now:    func() time.Time { return time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, svc: svc, ctx: ctx, serving: filepath.Join(base, "serving"), pin: weightsbundle.SHA256Hex([]byte(exportManifest))}
}

func (h *harness) decode(raw json.RawMessage, err error) map[string]any {
	h.t.Helper()
	if err != nil {
		h.t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		h.t.Fatal(err)
	}
	return out
}

func (h *harness) publishBundle(content string) (weightsbundle.Manifest, string) {
	h.t.Helper()
	staging := filepath.Join(h.t.TempDir(), "staging")
	os.Mkdir(staging, 0o700)
	os.WriteFile(filepath.Join(staging, "model.json"), []byte(content), 0o600)
	m, err := weightsbundle.Build(staging, map[string]string{"model.json": weightsbundle.RoleWeights})
	if err != nil {
		h.t.Fatal(err)
	}
	digest, err := weightsbundle.Stage(staging, m)
	if err != nil {
		h.t.Fatal(err)
	}
	os.MkdirAll(h.serving, 0o700)
	if err := weightsbundle.PublishLocal(staging, filepath.Join(h.serving, digest)); err != nil {
		h.t.Fatal(err)
	}
	return m, digest
}

func (h *harness) manifest() distillation.Manifest {
	m := func(v float64) []distillation.Measurement {
		return []distillation.Measurement{{Metric: "dev_recall", Value: v, Split: distillation.SplitDevelopment, EvidenceID: "e"}}
	}
	id := func(p, name, egress string) distillation.ModelIdentity {
		return distillation.ModelIdentity{Provider: p, Model: name, Version: "v1", CacheIdentity: p + ":" + name, Egress: egress}
	}
	min := 0.5
	rule := distillation.Rule{Metric: "dev_recall", Direction: distillation.Maximize, Minimum: &min}
	return distillation.Manifest{
		Schema: 1, Kind: distillation.Kind, TrainingManifestSHA256: strings.Repeat("1", 64),
		Teacher:                   distillation.ModelSelection{Role: distillation.RoleTeacher, Eligible: []distillation.Candidate{{Alias: "t", Identity: id("local", "big", distillation.EgressLoopback), Measurements: m(0.9)}}, Rule: rule, Chosen: "t"},
		Base:                      distillation.ModelSelection{Role: distillation.RoleBase, Eligible: []distillation.Candidate{{Alias: "b", Identity: id("local", "small", distillation.EgressLoopback), Measurements: m(0.6)}}, Rule: rule, Chosen: "b"},
		TeacherEgressAtGeneration: distillation.EgressLoopback,
		Data:                      distillation.DataSelection{Tasks: []distillation.TaskSelection{{TaskID: "task-v1", CohortIDs: []string{"cohort-1"}, LabelPolicyIDs: []string{"labels:1"}}}, EvidenceFamilies: []string{"code"}, PromptVersion: "p1", GenerationParameters: map[string]any{"epochs": 3}, HeldOutExampleIDs: []string{"ex-9"}},
		Exports:                   []distillation.ExportPin{{CohortID: "cohort-1", ManifestSHA256: h.pin}},
	}
}

func (h *harness) candidate(m weightsbundle.Manifest, pin string) (weightsbundle.Record, weightsbundle.Record, weightsbundle.Record) {
	h.t.Helper()
	weights, _ := m.File(weightsbundle.RoleWeights)
	candidate, _ := weightsbundle.Seal(weightsbundle.KindCandidate, map[string]any{"dataset_id": "d", "evaluation_policy_id": "p", "training_ids": []string{"ex-1"}, "model_digest": weights.SHA256, "policy_predeclared": true, "task_ids": []string{"task-v1"}, "dataset_manifest_sha256": h.pin, "training_manifest_sha256": pin})
	evaluation, _ := weightsbundle.Seal(weightsbundle.KindEvaluation, map[string]any{"candidate_id": candidate["id"], "dataset_id": "d", "evaluation_policy_id": "p", "ledger_id": "l", "disposition": "shadow_candidate", "reasons": []string{}, "metrics": []any{map[string]any{"assessor": "r", "positives": 2, "recall": 1.0, "missed_ids": []string{}, "unknown_lowered_ids": []string{}}}, "families": 2, "permitted_action": "full_review"})
	artifact, _ := weightsbundle.NewArtifact(candidate["id"].(string), evaluation["id"].(string), "d", m, "runtime")
	return candidate, evaluation, artifact
}

func TestLearningLoopThroughTheService(t *testing.T) {
	h := newHarness(t)
	ctx := h.ctx

	datasets := h.decode(h.svc.Datasets(ctx))
	if datasets["authority_mounted"] != true || len(datasets["cohorts"].([]any)) != 1 {
		t.Fatalf("%v", datasets)
	}
	export := h.decode(h.svc.RequestExport(ctx, "cohort-1"))
	if export["manifest_sha256"] != h.pin || export["quarantine"].(map[string]any)["label_withdrawn"] != 1.0 || len(export["eligible_example_ids"].([]any)) != 2 {
		t.Fatalf("%v", export)
	}
	if _, err := h.svc.RequestExport(ctx, "cohort-9"); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("%v", err)
	}
	datasets = h.decode(h.svc.Datasets(ctx))
	if datasets["cohorts"].([]any)[0].(map[string]any)["latest_export"] == nil {
		t.Fatal("export not attached to cohort")
	}

	manifest := h.manifest()
	encoded, _ := manifest.Encode()
	job := h.decode(h.svc.SubmitJob(ctx, encoded))
	pin, _ := manifest.Pin()
	if job["manifest_pin"] != pin || job["status"] != "submitted" {
		t.Fatalf("%v", job)
	}
	if _, err := h.svc.SubmitJob(ctx, encoded); !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("duplicate job: %v", err)
	}
	widened := h.manifest()
	widened.Data.Tasks[0].CohortIDs = []string{"cohort-1", "cohort-2"}
	widened.Exports = append(widened.Exports, distillation.ExportPin{CohortID: "cohort-2", ManifestSHA256: strings.Repeat("2", 64)})
	raw, _ := widened.Encode()
	if _, err := h.svc.SubmitJob(ctx, raw); !shoal.IsErrorCode(err, shoal.ErrorConflict) || !strings.Contains(err.Error(), "no recorded export") {
		t.Fatalf("unauthorized cohort: %v", err)
	}
	leaky := h.manifest()
	leaky.Data.HeldOutExampleIDs = []string{"ex-1"}
	raw, _ = leaky.Encode()
	if _, err := h.svc.SubmitJob(ctx, raw); !shoal.IsErrorCode(err, shoal.ErrorConflict) || !strings.Contains(err.Error(), "held-out") {
		t.Fatalf("held-out leak: %v", err)
	}
	view := h.decode(h.svc.Job(ctx, pin))
	if view["receipt"] != nil || view["manifest"] == nil {
		t.Fatalf("%v", view)
	}

	m, digest := h.publishBundle(`{"kind":"linear-student"}`)
	teacher := manifest.Teacher.Eligible[0].Identity
	base := manifest.Base.Eligible[0].Identity
	receipt := distillation.Receipt{Schema: 1, Kind: distillation.KindReceipt, ManifestPin: pin, RequestedParameters: manifest.Data.GenerationParameters, ActualParameters: manifest.Data.GenerationParameters, RequestedCohorts: []string{"cohort-1"}, ActualCohorts: []string{"cohort-1"}, RequestedTeacher: teacher, ActualTeacher: teacher, RequestedBase: base, ActualBase: base, RuntimeIdentities: map[string]string{"python": "3.12"}, Executor: distillation.Executor{ID: "worker"}, BundleDigest: digest, TeacherPredictionsID: strings.Repeat("e", 64), TrainedExampleCount: 2, Status: "completed", Deviations: []string{}, CandidateOnly: true}
	raw, _ = json.Marshal(receipt)
	submitted := h.decode(h.svc.SubmitReceipt(ctx, pin, raw))
	if submitted["status"] != "completed" || submitted["candidate_only"] != true {
		t.Fatalf("%v", submitted)
	}
	receipt.ImprovementClaimed = true
	raw, _ = json.Marshal(receipt)
	if _, err := h.svc.SubmitReceipt(ctx, pin, raw); !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("improvement claim: %v", err)
	}
	view = h.decode(h.svc.Job(ctx, pin))
	if view["executor"].(map[string]any)["id"] != "worker" || view["actual"].(map[string]any)["bundle_digest"] != digest {
		t.Fatalf("%v", view)
	}

	candidate, evaluation, artifact := h.candidate(m, pin)
	body, _ := json.Marshal(map[string]any{"candidate": candidate, "artifact": artifact})
	recorded := h.decode(h.svc.SubmitCandidate(ctx, body))
	if recorded["bundle_digest"] != digest {
		t.Fatalf("%v", recorded)
	}
	outside, _ := weightsbundle.Seal(weightsbundle.KindCandidate, map[string]any{"dataset_id": "d", "evaluation_policy_id": "p", "training_ids": []string{"ex-1"}, "model_digest": str(candidate, "model_digest"), "policy_predeclared": true, "task_ids": []string{"task-v2"}, "training_manifest_sha256": pin})
	outsideArtifact, _ := weightsbundle.NewArtifact(outside["id"].(string), evaluation["id"].(string), "d", m, "runtime")
	body, _ = json.Marshal(map[string]any{"candidate": outside, "artifact": outsideArtifact})
	if _, err := h.svc.SubmitCandidate(ctx, body); !shoal.IsErrorCode(err, shoal.ErrorConflict) || !strings.Contains(err.Error(), "outside the selection") {
		t.Fatalf("scope: %v", err)
	}

	// Promotion without its evaluation is refused with the reason verbatim.
	body, _ = json.Marshal(map[string]any{"candidate_id": candidate["id"], "owner": "owner"})
	_, err := h.svc.Promote(ctx, body)
	if !shoal.IsErrorCode(err, shoal.ErrorConflict) || !strings.Contains(err.Error(), "promotion requires its evaluation") {
		t.Fatalf("%v", err)
	}
	evaluationRaw, _ := weightsbundle.EncodeRecord(evaluation)
	h.decode(h.svc.SubmitEvaluation(ctx, evaluationRaw))
	evaluations := h.decode(h.svc.Evaluations(ctx, candidate["id"].(string)))
	if len(evaluations["evaluations"].([]any)) != 1 || evaluations["disclaimer"] != Disclaimer {
		t.Fatalf("%v", evaluations)
	}
	candidates := h.decode(h.svc.Candidates(ctx))
	if len(candidates["candidates"].([]any)) != 1 {
		t.Fatalf("%v", candidates)
	}
	body, _ = json.Marshal(map[string]any{"candidate_id": candidate["id"], "evaluation_id": evaluation["id"], "owner": "owner"})
	promoted := h.decode(h.svc.Promote(ctx, body))
	release := promoted["release"].(map[string]any)
	if release["bundle_digest"] != digest || release["approval_id"] == "" {
		t.Fatalf("%v", promoted)
	}
	// The route and the pure record constructor agree.
	approval, _ := weightsbundle.NewPromotionApproval(candidate["id"].(string), evaluation["id"].(string), "owner")
	pure, _ := weightsbundle.Promote(weightsbundle.PromotionInput{Candidate: candidate, Evaluation: evaluation, Artifact: artifact, Approval: approval})
	if release["id"] != pure.ID {
		t.Fatalf("service release %v differs from record %s", release["id"], pure.ID)
	}
	body, _ = json.Marshal(map[string]any{"candidate_id": candidate["id"], "evaluation_id": evaluation["id"], "owner": ""})
	if _, err := h.svc.Promote(ctx, body); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("unowned approval: %v", err)
	}
	releases := h.decode(h.svc.Releases(ctx))
	active := releases["active"].(map[string]any)
	if active["bundle_digest_served_now"] != digest || releases["disclaimer"] != Disclaimer || releases["serving_note"] == nil {
		t.Fatalf("%v", releases)
	}

	bundles := h.decode(h.svc.ExportBundle(ctx, []byte(`{"release_id":"`+pure.ID+`","name":"offline-1"}`)))
	if bundles["verified"] != true {
		t.Fatalf("%v", bundles)
	}
	listed := h.decode(h.svc.Bundles(ctx))
	if len(listed["bundles"].([]any)) != 1 {
		t.Fatalf("%v", listed)
	}
	if _, err := h.svc.ExportBundle(ctx, []byte(`{"release_id":"x","name":"../escape"}`)); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("%v", err)
	}

	// Importing into a second, disconnected service from the same bundle dir.
	imported := h.decode(h.svc.ImportBundle(ctx, []byte(`{"name":"offline-1","importer":"op"}`)))
	if imported["already_present"] != true || imported["activated"] != false {
		t.Fatalf("re-import of the active release: %v", imported)
	}
	// Alter a byte: the entry is named and nothing is activated.
	target := filepath.Join(h.svc.cfg.BundlesDir, "offline-1", "bundle", "model.json")
	b, _ := os.ReadFile(target)
	b[0] = ' '
	os.WriteFile(target, b, 0o600)
	raw, err = h.svc.ImportBundle(ctx, []byte(`{"name":"offline-1","importer":"op"}`))
	if !shoal.IsErrorCode(err, shoal.ErrorConflict) || !strings.Contains(err.Error(), `"bundle/model.json"`) {
		t.Fatalf("%v", err)
	}
	var failed map[string]any
	json.Unmarshal(raw, &failed)
	if failed["error"] == nil {
		t.Fatal("verification result not reported")
	}
	listed = h.decode(h.svc.Bundles(ctx))
	if listed["bundles"].([]any)[0].(map[string]any)["verified"] != false {
		t.Fatal("altered bundle still listed as verified")
	}

	evidence := h.decode(h.svc.Evidence(ctx, "quality"))
	reports := evidence["reports"].([]any)
	if len(reports) != 2 || evidence["read_only"] != true {
		t.Fatalf("%v", evidence)
	}
	if _, err := h.svc.Evidence(ctx, "secrets"); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatal(err)
	}

	// Rollback with a second release.
	m2, digest2 := h.publishBundle(`{"kind":"linear-student","v":2}`)
	candidate2, evaluation2, artifact2 := h.candidate(m2, pin)
	body, _ = json.Marshal(map[string]any{"candidate": candidate2, "artifact": artifact2})
	h.decode(h.svc.SubmitCandidate(ctx, body))
	raw2, _ := weightsbundle.EncodeRecord(evaluation2)
	h.decode(h.svc.SubmitEvaluation(ctx, raw2))
	body, _ = json.Marshal(map[string]any{"candidate_id": candidate2["id"], "evaluation_id": evaluation2["id"], "owner": "owner"})
	second := h.decode(h.svc.Promote(ctx, body))
	if second["release"].(map[string]any)["bundle_digest"] != digest2 {
		t.Fatal("second promotion")
	}
	rolled := h.decode(h.svc.Rollback(ctx, []byte(`{"prior_release_id":"`+pure.ID+`","owner":"owner"}`)))
	if rolled["release"].(map[string]any)["bundle_digest"] != digest {
		t.Fatalf("%v", rolled)
	}
	if _, err := h.svc.Rollback(ctx, []byte(`{"prior_release_id":"nope","owner":"owner"}`)); !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("%v", err)
	}
}

func TestServiceReportsAbsentAuthorities(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	registry, _ := weightsbundle.OpenRegistry(ctx, store, "r")
	svc, err := New(Config{Registry: registry, Store: store, BundlesDir: t.TempDir(), ServingRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var datasets map[string]any
	raw, err := svc.Datasets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(raw, &datasets)
	if datasets["authority_mounted"] != false || datasets["note"] == nil {
		t.Fatalf("%v", datasets)
	}
	if _, err := svc.RequestExport(ctx, "c"); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("%v", err)
	}
	if _, err := svc.Evidence(ctx, "quality"); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("%v", err)
	}
	if _, err := New(Config{}); err == nil {
		t.Fatal("incomplete config accepted")
	}
	if err := classify(weightsbundle.ErrNoActiveRelease); !shoal.IsErrorCode(err, shoal.ErrorConflict) || !errors.Is(weightsbundle.ErrNoActiveRelease, weightsbundle.ErrNoActiveRelease) {
		t.Fatalf("classify: %v", err)
	}
}
