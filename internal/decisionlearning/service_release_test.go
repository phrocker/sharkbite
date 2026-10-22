// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisionlearning

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/releasebundle"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
	"github.com/phrocker/shoal-oss/internal/weightsbundle"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// fixtureRun is one of the Python worker's completed runs on the fixture
// export: the manifest bytes (their digest is the pin), receipt, prediction
// set, and bundle directory.
type fixtureRun struct {
	manifest, receipt, predictions []byte
	pin, bundleDir                 string
}

func fixtureRuns(t *testing.T) (fixtureRun, fixtureRun) {
	t.Helper()
	root := filepath.Join("..", "distillation", "testdata")
	first := fixtureRun{manifest: fixtureFile(t, "python-manifest.json"), receipt: fixtureFile(t, "python-receipt.json"), predictions: fixtureFile(t, "python-predictions.json"), bundleDir: filepath.Join(root, "python-bundle")}
	first.pin = weightsbundle.SHA256Hex(first.manifest)
	second := fixtureRun{manifest: fixtureFile(t, filepath.Join("python-run2", "manifest.json")), receipt: fixtureFile(t, filepath.Join("python-run2", "receipt.json")), predictions: fixtureFile(t, filepath.Join("python-run2", "predictions.json")), bundleDir: filepath.Join(root, "python-run2", "bundle")}
	second.pin = weightsbundle.SHA256Hex(second.manifest)
	return first, second
}

func (r fixtureRun) bundleBody(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(r.bundleDir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(r.bundleDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = base64.StdEncoding.EncodeToString(raw)
	}
	return out
}

type fixtureService struct {
	t       *testing.T
	svc     *Service
	ctx     context.Context
	base    string
	serving string
}

func newFixtureService(t *testing.T) *fixtureService {
	t.Helper()
	ctx := actorCtx(context.Background())
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
	authority := fixtureAuthority{dataset: fixtureFile(t, "python-dataset.json"), manifest: fixtureFile(t, "python-export-manifest.json")}
	svc, err := New(Config{
		Registry: registry, Store: store, Prefix: "learning", EvidenceDir: filepath.Join(base, "evidence"),
		BundlesDir: filepath.Join(base, "bundles"), ServingRoot: filepath.Join(base, "serving"),
		Cohorts: authority, Exporter: authority, Egress: fakeEgress{},
		Images: []releasebundle.ImageRef{{Name: "student-runtime", Repository: "ghcr.io/example/student", Digest: "sha256:" + strings.Repeat("a", 64)}},
		Now:    func() time.Time { return time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	f := &fixtureService{t: t, svc: svc, ctx: ctx, base: base, serving: filepath.Join(base, "serving")}
	f.decode(svc.RequestExport(ctx, "cohort-1"))
	return f
}

func (f *fixtureService) decode(raw json.RawMessage, err error) map[string]any {
	f.t.Helper()
	if err != nil {
		f.t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		f.t.Fatal(err)
	}
	return out
}

// record puts a completed run on the host under a policy: job, policy,
// predictions, receipt.
func (f *fixtureService) record(run fixtureRun, policy string) {
	f.t.Helper()
	f.decode(f.svc.SubmitJob(f.ctx, run.manifest))
	f.decode(f.svc.DeclarePolicy(f.ctx, run.pin, json.RawMessage(policy)))
	f.decode(f.svc.SubmitPredictions(f.ctx, run.pin, run.predictions))
	f.decode(f.svc.SubmitReceipt(f.ctx, run.pin, run.receipt))
}

// seal seals a run through the route and returns the response decoded as
// records (numbers kept as literals), so the sealed candidate, evaluation,
// and artifact re-encode to exactly the bytes the host sealed.
func (f *fixtureService) seal(run fixtureRun) map[string]any {
	f.t.Helper()
	raw, _ := json.Marshal(map[string]any{"manifest_pin": run.pin, "bundle": run.bundleBody(f.t)})
	out, err := f.svc.SealCandidate(f.ctx, raw)
	if err != nil {
		f.t.Fatal(err)
	}
	record, err := weightsbundle.DecodeRecord(out)
	if err != nil {
		f.t.Fatal(err)
	}
	return record
}

const fixturePolicyBody = `{"name":"fixture-evaluation-policy","split":"test","positive_label":"yes","minimum_recall":0.5,"minimum_specificity":0.5,"minimum_positives":1,"minimum_families":1}`

func stripSeal(record map[string]any) map[string]any {
	fields := map[string]any{}
	for k, v := range record {
		if k != "id" && k != "kind" && k != "schema" {
			fields[k] = v
		}
	}
	return fields
}

// The candidate and evaluation routes accept only what the host seals from
// its own records and the published bundle: its own records are accepted
// (idempotently), a forged evaluation under the declared policy is refused
// even though it verifies and names the right candidate, and a candidate
// with altered lineage is refused. A release is then built, exported
// offline, imported back, succeeded by a second real run, and rolled back.
func TestReleasesAreBuiltOnlyFromRunsTheHostCanRebuild(t *testing.T) {
	first, second := fixtureRuns(t)
	f := newFixtureService(t)
	ctx := f.ctx
	f.record(first, fixturePolicyBody)
	sealed := f.seal(first)
	if sealed["disposition"] != "shadow_candidate" || sealed["published"] != true {
		t.Fatalf("%v", sealed)
	}
	candidate := sealed["candidate"].(map[string]any)
	evaluation := sealed["evaluation"].(map[string]any)
	artifact := sealed["artifact"].(map[string]any)
	digest := sealed["bundle_digest"].(string)

	submitCandidate := func(c, a map[string]any) error {
		raw, _ := json.Marshal(map[string]any{"candidate": c, "artifact": a})
		_, err := f.svc.SubmitCandidate(ctx, raw)
		return err
	}
	submitEvaluation := func(e map[string]any) error {
		raw, _ := json.Marshal(e)
		_, err := f.svc.SubmitEvaluation(ctx, raw)
		return err
	}
	if err := submitCandidate(candidate, artifact); err != nil {
		t.Fatalf("the host's own candidate refused: %v", err)
	}
	if err := submitEvaluation(evaluation); err != nil {
		t.Fatalf("the host's own evaluation refused: %v", err)
	}
	// A forged evaluation: same candidate, same declared policy, loosened
	// gates and a clean disposition. It verifies as a record; it is not the
	// host's measurement.
	forgedFields := stripSeal(evaluation)
	forgedFields["reasons"] = []any{}
	forgedFields["policy"] = map[string]any{"minimum_positives": 0}
	forgedFields["metrics"] = []any{}
	forged, _ := weightsbundle.Seal(weightsbundle.KindEvaluation, forgedFields)
	if err := submitEvaluation(forged); !shoal.IsErrorCode(err, shoal.ErrorConflict) || !strings.Contains(err.Error(), "not the evaluation this host seals") {
		t.Fatalf("forged evaluation accepted: %v", err)
	}
	// A forged candidate with altered lineage, bound to a matching artifact.
	alteredFields := stripSeal(candidate)
	alteredFields["training_ids"] = []any{}
	altered, _ := weightsbundle.Seal(weightsbundle.KindCandidate, alteredFields)
	published, _, err := weightsbundle.Load(filepath.Join(f.serving, digest))
	if err != nil {
		t.Fatal(err)
	}
	alteredArtifact, _ := weightsbundle.NewArtifact(altered["id"].(string), evaluation["id"].(string), "cohort-1", published, artifact["runtime_digest"].(string))
	if err := submitCandidate(altered, alteredArtifact); !shoal.IsErrorCode(err, shoal.ErrorConflict) || !strings.Contains(err.Error(), "not the candidate this host seals") {
		t.Fatalf("altered candidate accepted: %v", err)
	}
	// An artifact binding a foreign evaluation to the real candidate.
	foreignArtifact, _ := weightsbundle.NewArtifact(candidate["id"].(string), forged["id"].(string), "cohort-1", published, artifact["runtime_digest"].(string))
	if err := submitCandidate(candidate, foreignArtifact); !shoal.IsErrorCode(err, shoal.ErrorConflict) || !strings.Contains(err.Error(), "not the artifact this host seals") {
		t.Fatalf("artifact binding a foreign evaluation accepted: %v", err)
	}
	// Promotion requires the evaluation; the approval is the actor's.
	if _, err := f.svc.Promote(ctx, []byte(`{"candidate_id":"`+candidate["id"].(string)+`"}`)); !shoal.IsErrorCode(err, shoal.ErrorConflict) || !strings.Contains(err.Error(), "promotion requires its evaluation") {
		t.Fatalf("%v", err)
	}
	promote := func(c, e map[string]any, owner string) (json.RawMessage, error) {
		body := map[string]any{"candidate_id": c["id"], "evaluation_id": e["id"]}
		if owner != "" {
			body["owner"] = owner
		}
		raw, _ := json.Marshal(body)
		return f.svc.Promote(ctx, raw)
	}
	if _, err := promote(candidate, forged, ""); !shoal.IsErrorCode(err, shoal.ErrorConflict) || !strings.Contains(err.Error(), "is not recorded") {
		t.Fatalf("promotion with the forged evaluation: %v", err)
	}
	if _, err := promote(candidate, evaluation, "someone-else"); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("owner not the actor: %v", err)
	}
	promoted := f.decode(promote(candidate, evaluation, "owner"))
	release := promoted["release"].(map[string]any)
	if release["bundle_digest"] != digest || release["approval_id"] == "" {
		t.Fatalf("%v", promoted)
	}
	releaseID := release["id"].(string)
	approval, _ := weightsbundle.NewPromotionApproval(candidate["id"].(string), evaluation["id"].(string), "owner")
	pure, err := weightsbundle.Promote(weightsbundle.PromotionInput{Candidate: candidate, Evaluation: evaluation, Artifact: artifact, Approval: approval})
	if err != nil || pure.ID != releaseID {
		t.Fatalf("service release %s differs from the record %s: %v", releaseID, pure.ID, err)
	}
	releases := f.decode(f.svc.Releases(ctx))
	if releases["active"].(map[string]any)["bundle_digest"] != digest {
		t.Fatalf("%v", releases)
	}

	// The offline bundle carries the manifest bytes recorded when the export
	// was requested; an authority that has since regenerated its cohorts, or
	// is no longer mounted, does not block the export of a recorded release.
	exporter := f.svc.cfg.Exporter
	f.svc.cfg.Exporter = fixtureAuthority{}
	if out := f.decode(f.svc.ExportBundle(ctx, []byte(`{"release_id":"`+releaseID+`","name":"offline-1"}`))); out["verified"] != true {
		t.Fatalf("%v", out)
	}
	f.svc.cfg.Exporter = nil
	if out := f.decode(f.svc.ExportBundle(ctx, []byte(`{"release_id":"`+releaseID+`","name":"offline-2"}`))); out["verified"] != true {
		t.Fatalf("export without a mounted exporter: %v", out)
	}
	f.svc.cfg.Exporter = exporter
	if listed := f.decode(f.svc.Bundles(ctx)); len(listed["bundles"].([]any)) != 2 {
		t.Fatalf("%v", listed)
	}
	imported := f.decode(f.svc.ImportBundle(ctx, []byte(`{"name":"offline-1"}`)))
	if imported["already_present"] != true || imported["activated"] != false || imported["importer"] != "owner" {
		t.Fatalf("re-import of the active release: %v", imported)
	}
	if _, err := f.svc.ImportBundle(ctx, []byte(`{"name":"offline-1","expected_index_digest":"`+strings.Repeat("0", 64)+`"}`)); !shoal.IsErrorCode(err, shoal.ErrorConflict) || !strings.Contains(err.Error(), "index digest") {
		t.Fatalf("pinned index: %v", err)
	}
	target := filepath.Join(f.svc.cfg.BundlesDir, "offline-1", "bundle", "model.json")
	b, _ := os.ReadFile(target)
	b[0] = ' '
	os.WriteFile(target, b, 0o600)
	raw, err := f.svc.ImportBundle(ctx, []byte(`{"name":"offline-1"}`))
	if !shoal.IsErrorCode(err, shoal.ErrorConflict) || !strings.Contains(err.Error(), `"bundle/model.json"`) {
		t.Fatalf("%v", err)
	}
	var failed map[string]any
	json.Unmarshal(raw, &failed)
	if failed["error"] == nil {
		t.Fatal("verification result not reported")
	}
	if listed := f.decode(f.svc.Bundles(ctx)); listed["bundles"].([]any)[0].(map[string]any)["verified"] != false {
		t.Fatal("altered bundle still listed as verified")
	}

	// A second release from a second real run, then rollback.
	f.record(second, fixturePolicyBody)
	sealed2 := f.seal(second)
	if sealed2["disposition"] != "shadow_candidate" {
		t.Fatalf("%v", sealed2)
	}
	digest2 := sealed2["bundle_digest"].(string)
	promoted2 := f.decode(promote(sealed2["candidate"].(map[string]any), sealed2["evaluation"].(map[string]any), ""))
	if promoted2["release"].(map[string]any)["bundle_digest"] != digest2 {
		t.Fatalf("%v", promoted2)
	}
	if err := os.Rename(filepath.Join(f.serving, digest), filepath.Join(f.serving, "parked")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Rollback(ctx, []byte(`{"prior_release_id":"`+releaseID+`"}`)); !shoal.IsErrorCode(err, shoal.ErrorConflict) || !strings.Contains(err.Error(), "not on the serving root") {
		t.Fatalf("rollback to a pruned bundle: %v", err)
	}
	os.Rename(filepath.Join(f.serving, "parked"), filepath.Join(f.serving, digest))
	rolled := f.decode(f.svc.Rollback(ctx, []byte(`{"prior_release_id":"`+releaseID+`","owner":"owner"}`)))
	if rolled["release"].(map[string]any)["bundle_digest"] != digest {
		t.Fatalf("%v", rolled)
	}
}

// A held run is recorded with its reasons but its bundle is not placed on
// the serving root, so no candidate route can bind it and nothing a caller
// seals for it is accepted.
func TestHeldRunsAreRecordedButNotPublished(t *testing.T) {
	first, _ := fixtureRuns(t)
	f := newFixtureService(t)
	strict := strings.Replace(fixturePolicyBody, `"minimum_positives":1`, `"minimum_positives":50`, 1)
	f.record(first, strict)
	sealed := f.seal(first)
	if sealed["disposition"] != "hold" || sealed["published"] != false {
		t.Fatalf("%v", sealed)
	}
	digest := sealed["bundle_digest"].(string)
	if _, err := os.Stat(filepath.Join(f.serving, digest)); !os.IsNotExist(err) {
		t.Fatalf("held bundle on the serving root: %v", err)
	}
	if entries, _ := os.ReadDir(f.serving); len(entries) != 0 {
		t.Fatalf("serving root not empty: %v", entries)
	}
	if evaluations := f.decode(f.svc.Evaluations(f.ctx, sealed["candidate"].(map[string]any)["id"].(string))); len(evaluations["evaluations"].([]any)) != 1 {
		t.Fatalf("held evaluation not recorded: %v", evaluations)
	}
	// Even the host's own records for a held run cannot be re-submitted
	// through the candidate route: there is no published bundle to rebuild
	// from, and a clean evaluation sealed by a caller is not the host's.
	raw, _ := json.Marshal(map[string]any{"candidate": sealed["candidate"], "artifact": sealed["artifact"]})
	if _, err := f.svc.SubmitCandidate(f.ctx, raw); !shoal.IsErrorCode(err, shoal.ErrorConflict) || !strings.Contains(err.Error(), "not published") {
		t.Fatalf("held candidate accepted: %v", err)
	}
	clean := stripSeal(sealed["evaluation"].(map[string]any))
	clean["reasons"], clean["disposition"] = []any{}, "shadow_candidate"
	forged, _ := weightsbundle.Seal(weightsbundle.KindEvaluation, clean)
	forgedRaw, _ := json.Marshal(forged)
	if _, err := f.svc.SubmitEvaluation(f.ctx, forgedRaw); !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("forged clean evaluation for a held run accepted: %v", err)
	}
	body, _ := json.Marshal(map[string]any{"candidate_id": sealed["candidate"].(map[string]any)["id"], "evaluation_id": forged["id"]})
	if _, err := f.svc.Promote(f.ctx, body); err == nil {
		t.Fatal("held run promoted")
	}
}
