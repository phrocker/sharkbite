// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisionlearning

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/releasebundle"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
	"github.com/phrocker/shoal-oss/internal/weightsbundle"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// runFixture is a fixture service whose clock the test moves.
type runFixture struct {
	*fixtureService
	now time.Time
}

func newRunFixture(t *testing.T) *runFixture {
	t.Helper()
	ctx := actorCtx(context.Background())
	store := memory.New()
	registry, err := weightsbundle.OpenRegistry(ctx, store, "learning/releases")
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	authority := fixtureAuthority{dataset: fixtureFile(t, "python-dataset.json"), manifest: fixtureFile(t, "python-export-manifest.json")}
	f := &runFixture{now: time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)}
	svc, err := New(Config{
		Registry: registry, Store: store, Prefix: "learning",
		BundlesDir: filepath.Join(base, "bundles"), ServingRoot: filepath.Join(base, "serving"),
		Cohorts: authority, Exporter: authority, Egress: fakeEgress{},
		Images: []releasebundle.ImageRef{{Name: "student-runtime", Repository: "ghcr.io/example/student", Digest: "sha256:" + strings.Repeat("a", 64)}},
		Now:    func() time.Time { return f.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	f.fixtureService = &fixtureService{t: t, svc: svc, ctx: ctx, base: base, serving: filepath.Join(base, "serving")}
	f.decode(svc.RequestExport(ctx, "cohort-1"))
	return f
}

func (f *runFixture) run(pin string) map[string]any {
	f.t.Helper()
	view := f.decode(f.svc.Job(f.ctx, pin))
	run, _ := view["run"].(map[string]any)
	if run == nil {
		f.t.Fatalf("job view has no run: %v", view)
	}
	return run
}

func expectCode(t *testing.T, err error, code shoal.ErrorCode, what string) {
	t.Helper()
	if err == nil || !shoal.IsErrorCode(err, code) {
		t.Fatalf("%s: err = %v, want %s", what, err, code)
	}
}

func TestRunRequestsNeedAPolicyAndPrecedeTheReceipt(t *testing.T) {
	f := newRunFixture(t)
	first, _ := fixtureRuns(t)
	f.decode(f.svc.SubmitJob(f.ctx, first.manifest))

	_, err := f.svc.RequestRun(f.ctx, first.pin, nil)
	expectCode(t, err, shoal.ErrorConflict, "run before policy")
	if run := f.run(first.pin); run["requested"] != false || run["runnable"] != false {
		t.Fatalf("run view before request = %v", run)
	}

	f.decode(f.svc.DeclarePolicy(f.ctx, first.pin, json.RawMessage(fixturePolicyBody)))
	_, err = f.svc.RequestRun(f.ctx, first.pin, json.RawMessage(`{"extra":1}`))
	expectCode(t, err, shoal.ErrorInvalidArgument, "run with fields")
	request := f.decode(f.svc.RequestRun(f.ctx, first.pin, json.RawMessage(`{}`)))
	if request["requested_by"] != "owner" || request["manifest_pin"] != first.pin {
		t.Fatalf("request = %v", request)
	}
	again := f.decode(f.svc.RequestRun(f.ctx, first.pin, nil))
	if again["requested_at"] != request["requested_at"] {
		t.Fatalf("second request did not return the first: %v / %v", again, request)
	}
	run := f.run(first.pin)
	if run["requested"] != true || run["runnable"] != true || run["claim"] != nil || run["attempts"] != float64(0) {
		t.Fatalf("run view after request = %v", run)
	}

	// Once a receipt is recorded there is nothing to run.
	f.decode(f.svc.SubmitPredictions(f.ctx, first.pin, first.predictions))
	f.decode(f.svc.SubmitReceipt(f.ctx, first.pin, first.receipt))
	_, err = f.svc.ClaimRun(f.ctx, first.pin, json.RawMessage(`{"worker_id":"w1"}`))
	expectCode(t, err, shoal.ErrorConflict, "claim after receipt")
	if run := f.run(first.pin); run["runnable"] != false {
		t.Fatalf("runnable with a receipt: %v", run)
	}
	_, err = f.svc.RequestRun(f.ctx, "deadbeef", nil)
	expectCode(t, err, shoal.ErrorInvalidArgument, "bad pin")
	_, err = f.svc.RequestRun(f.ctx, strings.Repeat("0", 64), nil)
	expectCode(t, err, shoal.ErrorNotFound, "unknown job")
}

func TestClaimsAreLeasedOneAtATimeAndReleasedByTerminalProgress(t *testing.T) {
	f := newRunFixture(t)
	first, _ := fixtureRuns(t)
	f.decode(f.svc.SubmitJob(f.ctx, first.manifest))
	f.decode(f.svc.DeclarePolicy(f.ctx, first.pin, json.RawMessage(fixturePolicyBody)))

	_, err := f.svc.ClaimRun(f.ctx, first.pin, json.RawMessage(`{"worker_id":"w1"}`))
	expectCode(t, err, shoal.ErrorConflict, "claim without a request")
	f.decode(f.svc.RequestRun(f.ctx, first.pin, nil))

	for _, bad := range []string{`{}`, `{"worker_id":"bad id"}`, `{"worker_id":"w1","lease_seconds":-1}`, `{"worker_id":"w1","lease_seconds":99999}`, `{"worker_id":"w1","other":1}`} {
		_, err := f.svc.ClaimRun(f.ctx, first.pin, json.RawMessage(bad))
		expectCode(t, err, shoal.ErrorInvalidArgument, "claim body "+bad)
	}

	claimed := f.decode(f.svc.ClaimRun(f.ctx, first.pin, json.RawMessage(`{"worker_id":"w1","lease_seconds":60}`)))
	claim := claimed["claim"].(map[string]any)
	if claim["worker_id"] != "w1" || claim["attempt"] != float64(1) || claim["expires_at"] != "2026-10-23T12:01:00Z" {
		t.Fatalf("claim = %v", claim)
	}
	if exports, _ := claimed["exports"].([]any); len(exports) != 1 {
		t.Fatalf("claim carries no exports: %v", claimed)
	}
	claimID := claim["id"].(string)

	// A second worker is refused while the lease is live.
	_, err = f.svc.ClaimRun(f.ctx, first.pin, json.RawMessage(`{"worker_id":"w2"}`))
	expectCode(t, err, shoal.ErrorConflict, "second claim while live")
	run := f.run(first.pin)
	if run["runnable"] != false || run["claim"].(map[string]any)["live"] != true || run["attempts"] != float64(1) {
		t.Fatalf("run view while claimed = %v", run)
	}

	// Progress must name the live claim and a known, non-terminal-start phase.
	for _, bad := range []string{`{"claim_id":"` + claimID + `","phase":"claimed"}`, `{"claim_id":"` + claimID + `","phase":"nope"}`, `{"claim_id":"zz","phase":"training"}`, `{"claim_id":"` + claimID + `","phase":"training","detail":[1]}`} {
		_, err := f.svc.ReportProgress(f.ctx, first.pin, json.RawMessage(bad))
		expectCode(t, err, shoal.ErrorInvalidArgument, "progress body "+bad)
	}
	_, err = f.svc.ReportProgress(f.ctx, first.pin, json.RawMessage(`{"claim_id":"`+strings.Repeat("1", 64)+`","phase":"training"}`))
	expectCode(t, err, shoal.ErrorConflict, "progress under a foreign claim")
	progress := f.decode(f.svc.ReportProgress(f.ctx, first.pin, json.RawMessage(`{"claim_id":"`+claimID+`","phase":"training","message":"epoch 1","detail":{"epoch":1}}`)))
	if progress["seq"] != float64(1) || progress["phase"] != "training" {
		t.Fatalf("progress = %v", progress)
	}
	if run := f.run(first.pin); run["progress"].(map[string]any)["message"] != "epoch 1" {
		t.Fatalf("run view progress = %v", run)
	}

	// Failure releases the claim: a new claim is the second attempt, and
	// progress under the ended claim is refused.
	f.decode(f.svc.ReportProgress(f.ctx, first.pin, json.RawMessage(`{"claim_id":"`+claimID+`","phase":"failed","message":"trainer exited 1"}`)))
	_, err = f.svc.ReportProgress(f.ctx, first.pin, json.RawMessage(`{"claim_id":"`+claimID+`","phase":"sealing"}`))
	expectCode(t, err, shoal.ErrorConflict, "progress after failure")
	run = f.run(first.pin)
	if run["runnable"] != true || run["claim"].(map[string]any)["live"] != false || run["progress"].(map[string]any)["phase"] != "failed" {
		t.Fatalf("run view after failure = %v", run)
	}
	second := f.decode(f.svc.ClaimRun(f.ctx, first.pin, json.RawMessage(`{"worker_id":"w2","lease_seconds":60}`)))
	secondClaim := second["claim"].(map[string]any)
	if secondClaim["attempt"] != float64(2) || secondClaim["worker_id"] != "w2" {
		t.Fatalf("second claim = %v", secondClaim)
	}

	// An expired lease releases the claim without any progress.
	f.now = f.now.Add(2 * time.Minute)
	if run := f.run(first.pin); run["runnable"] != true || run["claim"].(map[string]any)["live"] != false {
		t.Fatalf("run view after expiry = %v", run)
	}
	third := f.decode(f.svc.ClaimRun(f.ctx, first.pin, json.RawMessage(`{"worker_id":"w3"}`)))
	if third["claim"].(map[string]any)["attempt"] != float64(3) {
		t.Fatalf("third claim = %v", third)
	}
	// The expired claim's holder can no longer report under it.
	_, err = f.svc.ReportProgress(f.ctx, first.pin, json.RawMessage(`{"claim_id":"`+secondClaim["id"].(string)+`","phase":"training"}`))
	expectCode(t, err, shoal.ErrorConflict, "progress under an expired claim")

	// Done releases too, and the jobs list carries the run.
	f.decode(f.svc.ReportProgress(f.ctx, first.pin, json.RawMessage(`{"claim_id":"`+third["claim"].(map[string]any)["id"].(string)+`","phase":"done"}`)))
	jobs := f.decode(f.svc.Jobs(f.ctx))
	listed := jobs["jobs"].([]any)[0].(map[string]any)["run"].(map[string]any)
	if listed["attempts"] != float64(3) || listed["progress"].(map[string]any)["phase"] != "done" {
		t.Fatalf("jobs list run = %v", listed)
	}
}
