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
	"github.com/phrocker/shoal-oss/internal/storage/local"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
	"github.com/phrocker/shoal-oss/internal/weightsbundle"
	decisionapi "github.com/phrocker/shoal-oss/pkg/decision/api"
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

// fakeEgress resolves the fixture's providers: local models are loopback,
// hosted ones leave the host, anything else is unknown.
type fakeEgress struct{}

func (fakeEgress) ResolveEgress(_ context.Context, id distillation.ModelIdentity) (string, error) {
	switch id.Provider {
	case "local":
		return distillation.EgressLoopback, nil
	case "hosted":
		return distillation.EgressOffHost, nil
	}
	return "", errors.New("unknown provider")
}

// actorCtx is what the transport binds for every write.
func actorCtx(ctx context.Context) context.Context {
	return decisionapi.WithLearningActor(ctx, decisionapi.LearningActor{Subject: "subject", Actor: "owner"})
}

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
		Egress: fakeEgress{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, svc: svc, ctx: actorCtx(ctx), serving: filepath.Join(base, "serving"), pin: weightsbundle.SHA256Hex([]byte(exportManifest))}
}

func TestRequestExportRejectsCorruptManifestAtPin(t *testing.T) {
	h := newHarness(t)
	w, err := h.svc.cfg.Store.Create(h.ctx, h.svc.recordPath(kindExportManifest, h.pin))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(`{"corrupt":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.RequestExport(h.ctx, "cohort-1"); !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("corrupt manifest acknowledged: %v", err)
	}
	if _, err := h.svc.get(h.ctx, kindExport, h.pin); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("export recorded after corrupt manifest: %v", err)
	}
}

// predictions records the chosen teacher's outputs for the fixture job.
func (h *harness) predictions(m distillation.Manifest, pin string) string {
	h.t.Helper()
	set, err := distillation.NewPredictionSet(m, []distillation.ExportDescriptor{{CohortID: "cohort-1", TaskID: "task-v1", LabelPolicyID: "labels:1", ManifestSHA256: h.pin, ExampleIDs: []string{"ex-1", "ex-2"}}},
		[]distillation.Prediction{{ExampleID: "ex-1", Kind: distillation.PredictionKind, Teacher: "t", Output: map[string]any{"label": "yes"}}, {ExampleID: "ex-2", Kind: distillation.PredictionKind, Teacher: "t", Output: map[string]any{"label": "no"}}})
	if err != nil {
		h.t.Fatal(err)
	}
	raw, _ := json.Marshal(set)
	out := h.decode(h.svc.SubmitPredictions(h.ctx, pin, raw))
	id, _ := set.ID()
	if out["predictions_id"] != id || out["count"] != 2.0 {
		h.t.Fatalf("%v", out)
	}
	return id
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
	if view["receipt"] != nil || view["manifest"] == nil || view["predictions_id"] != nil {
		t.Fatalf("%v", view)
	}
	// Declared egress is a claim the host confirms: a manifest declaring a
	// hosted teacher as loopback is refused, and an honestly off-host
	// teacher is refused unless the host authorizes off-host generation.
	lying := h.manifest()
	lying.Teacher.Eligible[0].Identity.Provider = "hosted"
	lying.Teacher.Eligible[0].Identity.CacheIdentity = "hosted:big"
	raw, _ = lying.Encode()
	if _, err := h.svc.SubmitJob(ctx, raw); !shoal.IsErrorCode(err, shoal.ErrorConflict) || !strings.Contains(err.Error(), `declares egress "loopback" but this host resolves it as "off_host"`) {
		t.Fatalf("declared egress: %v", err)
	}
	honest := lying
	honest.Teacher.Eligible[0].Identity.Egress = distillation.EgressOffHost
	honest.TeacherEgressAtGeneration = distillation.EgressOffHost
	raw, _ = honest.Encode()
	if _, err := h.svc.SubmitJob(ctx, raw); !shoal.IsErrorCode(err, shoal.ErrorConflict) || !strings.Contains(err.Error(), "does not authorize off-host models") {
		t.Fatalf("off-host teacher: %v", err)
	}
	offBase := h.manifest()
	offBase.Base.Eligible[0].Identity.Provider = "hosted"
	offBase.Base.Eligible[0].Identity.CacheIdentity = "hosted:small"
	offBase.Base.Eligible[0].Identity.Egress = distillation.EgressOffHost
	raw, _ = offBase.Encode()
	if _, err := h.svc.SubmitJob(ctx, raw); !shoal.IsErrorCode(err, shoal.ErrorConflict) || !strings.Contains(err.Error(), "chosen base is fine-tuned off host") {
		t.Fatalf("off-host base: %v", err)
	}
	unknown := h.manifest()
	unknown.Base.Eligible[0].Identity.Provider = "mystery"
	raw, _ = unknown.Encode()
	if _, err := h.svc.SubmitJob(ctx, raw); !shoal.IsErrorCode(err, shoal.ErrorConflict) || !strings.Contains(err.Error(), "could not be resolved") {
		t.Fatalf("unknown provider: %v", err)
	}

	m, digest := h.publishBundle(`{"kind":"linear-student"}`)
	teacher := manifest.Teacher.Eligible[0].Identity
	base := manifest.Base.Eligible[0].Identity
	receipt := distillation.Receipt{Schema: 1, Kind: distillation.KindReceipt, ManifestPin: pin, RequestedParameters: manifest.Data.GenerationParameters, ActualParameters: manifest.Data.GenerationParameters, RequestedCohorts: []string{"cohort-1"}, ActualCohorts: []string{"cohort-1"}, RequestedTeacher: teacher, ActualTeacher: teacher, RequestedBase: base, ActualBase: base, RuntimeIdentities: map[string]string{"python": "3.12"}, Executor: distillation.Executor{ID: "worker"}, BundleDigest: digest, TeacherPredictionsID: strings.Repeat("e", 64), TrainedExampleCount: 2, Status: "completed", Deviations: []distillation.Deviation{}, CandidateOnly: true,
		AuthorizedExports: []distillation.AuthorizedExport{{CohortID: "cohort-1", TaskID: "task-v1", LabelPolicyID: "labels:1", ManifestSHA256: h.pin}}}
	raw, _ = json.Marshal(receipt)
	// A completed receipt needs its prediction set recorded first, and the
	// id it names must be that set's.
	if _, err := h.svc.SubmitReceipt(ctx, pin, raw); !shoal.IsErrorCode(err, shoal.ErrorConflict) || !strings.Contains(err.Error(), "submit the predictions first") {
		t.Fatalf("receipt before predictions: %v", err)
	}
	predictionsID := h.predictions(manifest, pin)
	if _, err := h.svc.SubmitReceipt(ctx, pin, raw); !shoal.IsErrorCode(err, shoal.ErrorConflict) || !strings.Contains(err.Error(), "the recorded set for this manifest is") {
		t.Fatalf("receipt naming another set: %v", err)
	}
	// A candidate naming this manifest before a receipt exists is refused.
	early, _, earlyArtifact := h.candidate(m, pin)
	body, _ := json.Marshal(map[string]any{"candidate": early, "artifact": earlyArtifact})
	if _, err := h.svc.SubmitCandidate(ctx, body); !shoal.IsErrorCode(err, shoal.ErrorConflict) || !strings.Contains(err.Error(), "no recorded receipt") {
		t.Fatalf("candidate before receipt: %v", err)
	}
	receipt.TeacherPredictionsID = predictionsID
	receipt.TrainedExampleCount = 3
	raw, _ = json.Marshal(receipt)
	if _, err := h.svc.SubmitReceipt(ctx, pin, raw); !shoal.IsErrorCode(err, shoal.ErrorConflict) || !strings.Contains(err.Error(), "prediction set covers 2") {
		t.Fatalf("overclaimed count: %v", err)
	}
	receipt.TrainedExampleCount = 2
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
	receipt.ImprovementClaimed = false
	view = h.decode(h.svc.Job(ctx, pin))
	if view["executor"].(map[string]any)["id"] != "worker" || view["actual"].(map[string]any)["bundle_digest"] != digest {
		t.Fatalf("%v", view)
	}

	candidate, evaluation, artifact := h.candidate(m, pin)
	body, _ = json.Marshal(map[string]any{"candidate": candidate, "artifact": artifact})
	recorded := h.decode(h.svc.SubmitCandidate(ctx, body))
	if recorded["bundle_digest"] != digest {
		t.Fatalf("%v", recorded)
	}
	// Naming an unrecorded manifest pin is not a way around the scope check.
	unbound, _, unboundArtifact := h.candidate(m, strings.Repeat("9", 64))
	body, _ = json.Marshal(map[string]any{"candidate": unbound, "artifact": unboundArtifact})
	if _, err := h.svc.SubmitCandidate(ctx, body); !shoal.IsErrorCode(err, shoal.ErrorConflict) || !strings.Contains(err.Error(), "not recorded on this host") {
		t.Fatalf("unrecorded pin: %v", err)
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
	if release["approval_id"] == "" {
		t.Fatal("approval not sealed")
	}
	// The approval's owner is the authenticated actor: a body may agree with
	// it, never choose another, and no actor means no write.
	body, _ = json.Marshal(map[string]any{"candidate_id": candidate["id"], "evaluation_id": evaluation["id"], "owner": "someone-else"})
	if _, err := h.svc.Promote(ctx, body); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) || !strings.Contains(err.Error(), "must be the authenticated actor") {
		t.Fatalf("owner not the actor: %v", err)
	}
	body, _ = json.Marshal(map[string]any{"candidate_id": candidate["id"], "evaluation_id": evaluation["id"]})
	if _, err := h.svc.Promote(context.Background(), body); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("write without an actor: %v", err)
	}
	if _, err := h.svc.Rollback(context.Background(), []byte(`{"prior_release_id":"x"}`)); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("rollback without an actor: %v", err)
	}
	if _, err := h.svc.ImportBundle(context.Background(), []byte(`{"name":"x"}`)); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("import without an actor: %v", err)
	}
	releases := h.decode(h.svc.Releases(ctx))
	active := releases["active"].(map[string]any)
	if active["bundle_digest"] != digest || releases["disclaimer"] != Disclaimer || releases["serving_note"] == nil {
		t.Fatalf("%v", releases)
	}

	// The offline bundle carries the manifest bytes recorded when the export
	// was requested. An authority that has since regenerated its cohorts, or
	// is no longer mounted, does not block the export of a recorded release.
	exporter := h.svc.cfg.Exporter
	h.svc.cfg.Exporter = fakeAuthority{err: shoal.NewError(shoal.ErrorNotFound, "cohort regenerated under a new identity")}
	bundles := h.decode(h.svc.ExportBundle(ctx, []byte(`{"release_id":"`+pure.ID+`","name":"offline-1"}`)))
	if bundles["verified"] != true {
		t.Fatalf("%v", bundles)
	}
	h.svc.cfg.Exporter = nil
	if second := h.decode(h.svc.ExportBundle(ctx, []byte(`{"release_id":"`+pure.ID+`","name":"offline-2"}`))); second["verified"] != true {
		t.Fatalf("export without a mounted exporter: %v", second)
	}
	h.svc.cfg.Exporter = exporter
	listed := h.decode(h.svc.Bundles(ctx))
	if len(listed["bundles"].([]any)) != 2 {
		t.Fatalf("%v", listed)
	}
	if _, err := h.svc.ExportBundle(ctx, []byte(`{"release_id":"x","name":"../escape"}`)); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("%v", err)
	}

	// Importing into a second, disconnected service from the same bundle dir.
	imported := h.decode(h.svc.ImportBundle(ctx, []byte(`{"name":"offline-1"}`)))
	if imported["already_present"] != true || imported["activated"] != false || imported["importer"] != "owner" || imported["serving_path"] != nil {
		t.Fatalf("re-import of the active release: %v", imported)
	}
	if _, err := h.svc.ImportBundle(ctx, []byte(`{"name":"offline-1","expected_index_digest":"`+strings.Repeat("0", 64)+`"}`)); !shoal.IsErrorCode(err, shoal.ErrorConflict) || !strings.Contains(err.Error(), "index digest") {
		t.Fatalf("pinned index: %v", err)
	}
	// Alter a byte: the entry is named and nothing is activated.
	target := filepath.Join(h.svc.cfg.BundlesDir, "offline-1", "bundle", "model.json")
	b, _ := os.ReadFile(target)
	b[0] = ' '
	os.WriteFile(target, b, 0o600)
	raw, err = h.svc.ImportBundle(ctx, []byte(`{"name":"offline-1"}`))
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

	// Rollback with a second release: the second bundle comes from its own
	// completed run under a second manifest.
	m2, digest2 := h.publishBundle(`{"kind":"linear-student","v":2}`)
	manifest2 := h.manifest()
	manifest2.Data.PromptVersion = "p2"
	raw, _ = manifest2.Encode()
	h.decode(h.svc.SubmitJob(ctx, raw))
	pin2, _ := manifest2.Pin()
	predictions2 := h.predictions(manifest2, pin2)
	receipt2 := receipt
	receipt2.ManifestPin, receipt2.BundleDigest, receipt2.TeacherPredictionsID = pin2, digest2, predictions2
	raw, _ = json.Marshal(receipt2)
	h.decode(h.svc.SubmitReceipt(ctx, pin2, raw))
	candidate2, evaluation2, artifact2 := h.candidate(m2, pin2)
	body, _ = json.Marshal(map[string]any{"candidate": candidate2, "artifact": artifact2})
	h.decode(h.svc.SubmitCandidate(ctx, body))
	raw2, _ := weightsbundle.EncodeRecord(evaluation2)
	h.decode(h.svc.SubmitEvaluation(ctx, raw2))
	body, _ = json.Marshal(map[string]any{"candidate_id": candidate2["id"], "evaluation_id": evaluation2["id"], "owner": "owner"})
	second := h.decode(h.svc.Promote(ctx, body))
	if second["release"].(map[string]any)["bundle_digest"] != digest2 {
		t.Fatal("second promotion")
	}
	// A prior release whose bundle left the serving root cannot be rolled
	// back to: the runtime would have nothing to load.
	if err := os.Rename(filepath.Join(h.serving, digest), filepath.Join(h.serving, "parked")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Rollback(ctx, []byte(`{"prior_release_id":"`+pure.ID+`"}`)); !shoal.IsErrorCode(err, shoal.ErrorConflict) || !strings.Contains(err.Error(), "not on the serving root") {
		t.Fatalf("rollback to a pruned bundle: %v", err)
	}
	os.Rename(filepath.Join(h.serving, "parked"), filepath.Join(h.serving, digest))
	rolled := h.decode(h.svc.Rollback(ctx, []byte(`{"prior_release_id":"`+pure.ID+`","owner":"owner"}`)))
	if rolled["release"].(map[string]any)["bundle_digest"] != digest {
		t.Fatalf("%v", rolled)
	}
	if _, err := h.svc.Rollback(ctx, []byte(`{"prior_release_id":"nope"}`)); !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("%v", err)
	}
}

// Record ids arriving in bodies are used as store keys. Against the
// on-disk store the host actually wires, a traversal-shaped id must be
// refused before the store sees it, and a stray JSON file under the prefix
// must never be read as a record.
func TestRecordIDsNeverReachTheFilesystemUnvalidated(t *testing.T) {
	ctx := actorCtx(context.Background())
	base := t.TempDir()
	store := local.New()
	registry, err := weightsbundle.OpenRegistry(ctx, store, filepath.Join(base, "state", "releases"))
	if err != nil {
		t.Fatal(err)
	}
	svc, err := New(Config{Registry: registry, Store: store, Prefix: filepath.Join(base, "state"), BundlesDir: filepath.Join(base, "bundles"), ServingRoot: filepath.Join(base, "serving"), Egress: fakeEgress{}})
	if err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(base, "secret.json")
	os.WriteFile(secret, []byte(`{"kind":"candidate","id":"x","note":"not yours"}`), 0o600)
	os.WriteFile(filepath.Join(base, "garbage.json"), []byte("garbage"), 0o600)
	// Records under the prefix but of another kind, or under another key,
	// are not the record the key names.
	os.MkdirAll(filepath.Join(base, "state", "records", "candidate"), 0o700)
	evaluation, _ := weightsbundle.Seal(weightsbundle.KindEvaluation, map[string]any{"candidate_id": "c"})
	encoded, _ := weightsbundle.EncodeRecord(evaluation)
	os.WriteFile(filepath.Join(base, "state", "records", "candidate", strings.Repeat("a", 64)+".json"), encoded, 0o600)
	for name, id := range map[string]string{
		"traversal to a JSON object": "../../../secret",
		"traversal to garbage":       "../../../garbage",
		"traversal missing":          "../../../missing",
		"nested":                     "x/y",
		"short":                      "abc",
		"uppercase":                  strings.ToUpper(strings.Repeat("a", 64)),
	} {
		body, _ := json.Marshal(map[string]any{"candidate_id": id, "evaluation_id": strings.Repeat("b", 64)})
		_, err := svc.Promote(ctx, body)
		if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) || !strings.Contains(err.Error(), "content address") {
			t.Errorf("%s: %v", name, err)
		}
		raw, _ := weightsbundle.EncodeRecord(mustSeal(t, weightsbundle.KindEvaluation, map[string]any{"candidate_id": id}))
		if _, err := svc.SubmitEvaluation(ctx, raw); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
			t.Errorf("evaluation for %s: %v", name, err)
		}
	}
	body, _ := json.Marshal(map[string]any{"candidate_id": strings.Repeat("a", 64), "evaluation_id": strings.Repeat("b", 64)})
	if _, err := svc.Promote(ctx, body); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("record of another kind under the key: %v", err)
	}
	// No host path reaches the wire: a half-made bundle directory, a broken
	// evidence file, and a failed import are reported by role.
	os.MkdirAll(filepath.Join(base, "bundles", "half"), 0o700)
	bundles, err := svc.Bundles(ctx)
	if err != nil || strings.Contains(string(bundles), base) || !strings.Contains(string(bundles), `\u003cbundles\u003e`) {
		t.Fatalf("bundle view leaked a path: %v %s", err, bundles)
	}
	svc.cfg.EvidenceDir = filepath.Join(base, "evidence")
	os.MkdirAll(filepath.Join(base, "evidence", "quality"), 0o700)
	os.WriteFile(filepath.Join(base, "evidence", "quality", "r.json"), []byte("{"), 0o600)
	evidence, err := svc.Evidence(ctx, "quality")
	if err != nil || strings.Contains(string(evidence), base) {
		t.Fatalf("evidence leaked a path: %v %s", err, evidence)
	}
	raw, err := svc.ImportBundle(ctx, []byte(`{"name":"half"}`))
	if err == nil || strings.Contains(err.Error(), base) || strings.Contains(string(raw), base) {
		t.Fatalf("import leaked a path: %v %s", err, raw)
	}
	if !strings.Contains(err.Error(), "<bundles>") {
		t.Fatalf("import error not redacted by role: %v", err)
	}
	// A candidate whose bundle is absent from the serving root is refused by
	// role, never by the host path the registry failed to open.
	staging := t.TempDir()
	os.WriteFile(filepath.Join(staging, "model.json"), []byte(`{"w":1}`), 0o600)
	m, err := weightsbundle.Build(staging, map[string]string{"model.json": weightsbundle.RoleWeights})
	if err != nil {
		t.Fatal(err)
	}
	weights, _ := m.File(weightsbundle.RoleWeights)
	candidate := mustSeal(t, weightsbundle.KindCandidate, map[string]any{"dataset_id": "d", "evaluation_policy_id": "p", "training_ids": []string{"x"}, "model_digest": weights.SHA256, "policy_predeclared": true, "task_ids": []string{"t"}})
	evaluation = mustSeal(t, weightsbundle.KindEvaluation, map[string]any{"candidate_id": candidate["id"]})
	artifact, err := weightsbundle.NewArtifact(candidate["id"].(string), evaluation["id"].(string), "d", m, "runtime")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = json.Marshal(map[string]any{"candidate": candidate, "artifact": artifact})
	_, err = svc.SubmitCandidate(ctx, body)
	if err == nil || !shoal.IsErrorCode(err, shoal.ErrorConflict) || strings.Contains(err.Error(), base) || !strings.Contains(err.Error(), "<serving>") {
		t.Fatalf("unpublished-bundle refusal leaked a path or lost its role: %v", err)
	}
	if err := classify(errors.New("open /var/lib/secret: permission denied")); !strings.HasSuffix(err.Error(), GenericUnavailable) || !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("internal cause leaked: %v", err)
	}
}

func mustSeal(t *testing.T, kind string, fields map[string]any) weightsbundle.Record {
	t.Helper()
	r, err := weightsbundle.Seal(kind, fields)
	if err != nil {
		t.Fatal(err)
	}
	return r
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
	// Without an egress resolver no job can be recorded: declared egress
	// would be the only word on where a teacher sends examples.
	if _, err := svc.SubmitJob(actorCtx(ctx), []byte(`{"schema":1}`)); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("%v", err)
	}
	h := newHarness(t)
	h.svc.cfg.Egress = nil
	encoded, _ := h.manifest().Encode()
	if _, err := h.svc.SubmitJob(h.ctx, encoded); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) || !strings.Contains(err.Error(), "no model egress resolver") {
		t.Fatalf("%v", err)
	}
	if err := classify(weightsbundle.ErrNoActiveRelease); !shoal.IsErrorCode(err, shoal.ErrorConflict) || !errors.Is(weightsbundle.ErrNoActiveRelease, weightsbundle.ErrNoActiveRelease) {
		t.Fatalf("classify: %v", err)
	}
}
