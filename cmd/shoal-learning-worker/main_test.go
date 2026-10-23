// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/phrocker/shoal-oss/internal/weightsbundle"
)

// fakeHost is the slice of the learning API the worker uses, recording
// every call in order.
type fakeHost struct {
	t        *testing.T
	mu       sync.Mutex
	calls    []string
	bodies   map[string][]byte
	pin      string
	manifest map[string]any
	cohort   string
	export   []byte
	dataset  []byte
	refuseAt string
	receipt  bool
	token    string
}

func newFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	manifest := map[string]any{"schema": "1", "kind": "distillation-manifest", "training_manifest_sha256": strings.Repeat("1", 64), "data": map[string]any{"prompt_version": "prompt:1"}}
	raw, err := weightsbundle.Canonical(manifest)
	if err != nil {
		t.Fatal(err)
	}
	export := []byte(`{"cohort_id":"cohort-1","task_id":"source"}`)
	return &fakeHost{t: t, bodies: map[string][]byte{}, pin: sha256Hex(raw), manifest: manifest, cohort: "cohort-1", export: export, dataset: []byte(`{"rows":[]}`)}
}

func (h *fakeHost) record(r *http.Request) string {
	raw, _ := io.ReadAll(r.Body)
	h.mu.Lock()
	defer h.mu.Unlock()
	key := r.Method + " " + r.URL.Path
	h.calls = append(h.calls, key)
	h.bodies[key] = raw
	return key
}

func (h *fakeHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := h.record(r)
	if h.token != "" && r.Header.Get("Authorization") != "Bearer "+h.token {
		http.Error(w, `{"code":"unauthorized","message":"token"}`, http.StatusUnauthorized)
		return
	}
	if r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") != "" {
		http.Error(w, `{"code":"invalid_argument","message":"cross-origin"}`, http.StatusForbidden)
		return
	}
	if r.Method == http.MethodPost && r.Header.Get("Content-Type") != "application/json" {
		http.Error(w, `{"code":"invalid_argument","message":"content type"}`, http.StatusBadRequest)
		return
	}
	if strings.HasSuffix(key, h.refuseAt) && h.refuseAt != "" {
		http.Error(w, `{"code":"conflict","message":"refused by the fake host"}`, http.StatusConflict)
		return
	}
	reply := func(v any) { json.NewEncoder(w).Encode(v) }
	switch key {
	case "GET /api/v1/learning/jobs":
		var receipt any
		if h.receipt {
			receipt = map[string]any{"status": "completed"}
		}
		reply(map[string]any{"jobs": []any{
			map[string]any{"job": map[string]any{"manifest_pin": strings.Repeat("9", 64)}, "receipt": nil, "run": map[string]any{"requested": false, "runnable": true}},
			map[string]any{"job": map[string]any{"manifest_pin": h.pin}, "receipt": receipt, "run": map[string]any{"requested": true, "runnable": !h.receipt}},
		}})
	case "POST /api/v1/learning/jobs/" + h.pin + "/claim":
		reply(map[string]any{"claim": map[string]any{"id": strings.Repeat("c", 64), "attempt": 1}, "exports": []any{map[string]any{"cohort_id": h.cohort, "manifest_sha256": sha256Hex(h.export)}}})
	case "GET /api/v1/learning/jobs/" + h.pin:
		reply(map[string]any{"manifest": h.manifest, "run": map[string]any{}})
	case "POST /api/v1/dataset-exports":
		var body struct {
			CohortID string `json:"cohort_id"`
		}
		json.Unmarshal(h.bodies[key], &body)
		if body.CohortID != base64.RawURLEncoding.EncodeToString([]byte(h.cohort)) {
			http.Error(w, `{"code":"invalid_argument","message":"cohort id"}`, http.StatusBadRequest)
			return
		}
		reply(map[string]any{"manifest": base64.StdEncoding.EncodeToString(h.export), "dataset": base64.StdEncoding.EncodeToString(h.dataset)})
	case "POST /api/v1/learning/jobs/" + h.pin + "/progress", "POST /api/v1/learning/jobs/" + h.pin + "/predictions", "POST /api/v1/learning/jobs/" + h.pin + "/receipt":
		reply(map[string]any{"ok": true})
	case "POST /api/v1/learning/candidates/seal":
		reply(map[string]any{"disposition": "shadow_candidate", "reasons": []string{}, "bundle_digest": "sha256:abc", "published": true, "candidate": map[string]any{"id": strings.Repeat("a", 64)}, "evaluation": map[string]any{"id": strings.Repeat("b", 64)}, "artifact": map[string]any{"id": strings.Repeat("d", 64)}})
	default:
		http.Error(w, `{"code":"not_found","message":"`+key+`"}`, http.StatusNotFound)
	}
}

// fakeTrainer is a shell script standing in for distill.py: it checks its
// inputs and writes the three outputs.
func fakeTrainer(t *testing.T, dir string) string {
	t.Helper()
	script := filepath.Join(dir, "trainer.sh")
	body := `#!/bin/sh
set -e
out=""; pin=""; manifest=""; exports=""; executor=""
while [ $# -gt 0 ]; do
  case "$1" in
    --output-dir) out="$2"; shift 2;;
    --manifest-pin) pin="$2"; shift 2;;
    --manifest) manifest="$2"; shift 2;;
    --export=*) exports="${1#--export=}"; shift;;
    --executor-id) executor="$2"; shift 2;;
    --executor-attestation) shift 2;;
    --student=*) echo "student $1"; shift;;
    --leak-env) echo "env token=$SHOAL_LEARNING_TOKEN other=$OTHER_SECRET plain=$PLAIN_VALUE"; exit 1;;
    *) echo "unexpected $1" >&2; exit 1;;
  esac
done
[ -n "$out" ] && [ -n "$pin" ] && [ -f "$manifest" ] && [ -n "$exports" ] && [ -n "$executor" ] || { echo "missing inputs" >&2; exit 1; }
cohort="${exports%%=*}"; paths="${exports#*=}"; dataset="${paths%%:*}"; emanifest="${paths#*:}"
[ "$cohort" = "$WANT_COHORT" ] && [ -f "$dataset" ] && [ -f "$emanifest" ] || { echo "bad export $exports" >&2; exit 1; }
printf '%s' "$(sha256sum "$manifest" | cut -d' ' -f1)" | grep -q "^$pin$" || { echo "manifest does not hash to the pin" >&2; exit 1; }
mkdir -p "$out/bundle"
echo '{"files":[]}' > "$out/bundle/bundle.json"
echo '{"weights":[0.5]}' > "$out/bundle/model.json"
echo '{"runtime":"test"}' > "$out/bundle/runtime.json"
echo '{"kind":"prediction","predictions":[]}' > "$out/teacher-predictions.json"
printf '{"kind":"distillation-receipt","executor":{"id":"%s"}}' "$executor" > "$out/distillation-receipt.json"
echo '{"candidate_only":true,"status":"completed"}'
`
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return script
}

func TestWorkerCarriesARequestedJobFromClaimToSeal(t *testing.T) {
	host := newFakeHost(t)
	host.token = "secret"
	// A cohort id with a leading "-" and a ":" reaches the trainer intact
	// through the --export= form.
	host.cohort = "-team:alpha:1"
	server := httptest.NewServer(host)
	defer server.Close()
	t.Setenv("SHOAL_LEARNING_TOKEN", "secret")
	t.Setenv("WANT_COHORT", host.cohort)
	work := t.TempDir()
	var output bytes.Buffer
	code := run(context.Background(), []string{
		"-host", server.URL, "-worker-id", "worker:test", "-work-dir", filepath.Join(work, "w"),
		"-trainer", fakeTrainer(t, work), "-trainer-arg", "--student=averaged-perceptron",
		"-executor-attestation", "att:1", "-once",
	}, &output)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, output.String())
	}
	want := []string{
		"GET /api/v1/learning/jobs",
		"POST /api/v1/learning/jobs/" + host.pin + "/claim",
		"POST /api/v1/learning/jobs/" + host.pin + "/progress", // fetching
		"GET /api/v1/learning/jobs/" + host.pin,
		"POST /api/v1/dataset-exports",
		"POST /api/v1/learning/jobs/" + host.pin + "/progress", // training
		"POST /api/v1/learning/jobs/" + host.pin + "/progress", // recording_predictions
		"POST /api/v1/learning/jobs/" + host.pin + "/predictions",
		"POST /api/v1/learning/jobs/" + host.pin + "/progress", // recording_receipt
		"POST /api/v1/learning/jobs/" + host.pin + "/receipt",
		"POST /api/v1/learning/jobs/" + host.pin + "/progress", // sealing
		"POST /api/v1/learning/candidates/seal",
		"POST /api/v1/learning/jobs/" + host.pin + "/progress", // done
	}
	if strings.Join(host.calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s\nwant:\n%s\noutput: %s", strings.Join(host.calls, "\n"), strings.Join(want, "\n"), output.String())
	}
	var claim struct {
		WorkerID string `json:"worker_id"`
		Lease    int    `json:"lease_seconds"`
	}
	json.Unmarshal(host.bodies["POST /api/v1/learning/jobs/"+host.pin+"/claim"], &claim)
	if claim.WorkerID != "worker:test" || claim.Lease != 600 {
		t.Fatalf("claim body = %+v", claim)
	}
	var progress struct {
		ClaimID string         `json:"claim_id"`
		Phase   string         `json:"phase"`
		Detail  map[string]any `json:"detail"`
	}
	json.Unmarshal(host.bodies["POST /api/v1/learning/jobs/"+host.pin+"/progress"], &progress)
	if progress.Phase != "done" || progress.ClaimID != strings.Repeat("c", 64) || progress.Detail["disposition"] != "shadow_candidate" || progress.Detail["candidate_id"] != strings.Repeat("a", 64) {
		t.Fatalf("final progress = %+v", progress)
	}
	if got := string(host.bodies["POST /api/v1/learning/jobs/"+host.pin+"/receipt"]); !strings.Contains(got, `"id":"worker:test"`) {
		t.Fatalf("receipt body = %s", got)
	}
	var seal struct {
		Pin    string            `json:"manifest_pin"`
		Bundle map[string]string `json:"bundle"`
	}
	json.Unmarshal(host.bodies["POST /api/v1/learning/candidates/seal"], &seal)
	if seal.Pin != host.pin || len(seal.Bundle) != 3 {
		t.Fatalf("seal body = %+v", seal)
	}
	if decoded, _ := base64.StdEncoding.DecodeString(seal.Bundle["model.json"]); !strings.Contains(string(decoded), "weights") {
		t.Fatalf("bundle file not uploaded verbatim: %q", decoded)
	}
	if !strings.Contains(output.String(), "sealed as shadow_candidate") {
		t.Fatalf("output: %s", output.String())
	}
	// The inputs the trainer saw are on disk under the claim's attempt,
	// with its output.
	for _, name := range []string{"export-manifest-1.json", "trainer.log"} {
		if _, err := os.Stat(filepath.Join(work, "w", host.pin, "1", name)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWorkerReportsFailuresUnderTheClaim(t *testing.T) {
	work := t.TempDir()
	trainer := fakeTrainer(t, work)
	t.Setenv("WANT_COHORT", "cohort-1")

	// A trainer failure is reported as failed progress with its output.
	host := newFakeHost(t)
	server := httptest.NewServer(host)
	defer server.Close()
	var output bytes.Buffer
	code := run(context.Background(), []string{"-host", server.URL, "-work-dir", work, "-trainer", trainer, "-trainer-arg", "--bogus", "-once"}, &output)
	if code != 2 {
		t.Fatalf("exit %d: %s", code, output.String())
	}
	var progress struct {
		Phase, Message string
	}
	json.Unmarshal(host.bodies["POST /api/v1/learning/jobs/"+host.pin+"/progress"], &progress)
	if progress.Phase != "failed" || !strings.Contains(progress.Message, "unexpected --bogus") || !strings.Contains(progress.Message, "trainer.log") {
		t.Fatalf("failure progress = %+v", progress)
	}
	for _, call := range host.calls {
		if strings.HasSuffix(call, "/predictions") || strings.HasSuffix(call, "/seal") {
			t.Fatalf("a failed run recorded %s", call)
		}
	}

	// An export whose bytes do not hash to the recorded pin is refused
	// before the trainer runs.
	host = newFakeHost(t)
	server2 := httptest.NewServer(mismatchedExport{host})
	defer server2.Close()
	output.Reset()
	if code := run(context.Background(), []string{"-host", server2.URL, "-work-dir", work, "-trainer", trainer, "-once"}, &output); code != 2 {
		t.Fatalf("exit %d: %s", code, output.String())
	}
	json.Unmarshal(host.bodies["POST /api/v1/learning/jobs/"+host.pin+"/progress"], &progress)
	if progress.Phase != "failed" || !strings.Contains(progress.Message, "not the recorded pin") {
		t.Fatalf("mismatch progress = %+v", progress)
	}

	// A host refusal at the receipt is reported, and nothing is sealed.
	host = newFakeHost(t)
	host.refuseAt = "/receipt"
	server3 := httptest.NewServer(host)
	defer server3.Close()
	output.Reset()
	if code := run(context.Background(), []string{"-host", server3.URL, "-work-dir", work, "-trainer", trainer, "-once"}, &output); code != 2 {
		t.Fatalf("exit %d: %s", code, output.String())
	}
	json.Unmarshal(host.bodies["POST /api/v1/learning/jobs/"+host.pin+"/progress"], &progress)
	if progress.Phase != "failed" || !strings.Contains(progress.Message, "refused by the fake host") {
		t.Fatalf("refusal progress = %+v", progress)
	}

	// Nothing requested: once exits 3 without claiming.
	host = newFakeHost(t)
	host.receipt = true
	server4 := httptest.NewServer(host)
	defer server4.Close()
	output.Reset()
	if code := run(context.Background(), []string{"-host", server4.URL, "-work-dir", work, "-trainer", trainer, "-once"}, &output); code != 3 || len(host.calls) != 1 {
		t.Fatalf("exit %d calls %v: %s", code, host.calls, output.String())
	}

	// A cohort id the trainer's --export cannot carry fails before the
	// trainer runs, naming the reason.
	host = newFakeHost(t)
	host.cohort = "team=alpha"
	server7 := httptest.NewServer(host)
	defer server7.Close()
	output.Reset()
	if code := run(context.Background(), []string{"-host", server7.URL, "-work-dir", work, "-trainer", trainer, "-once"}, &output); code != 2 {
		t.Fatalf("exit %d: %s", code, output.String())
	}
	json.Unmarshal(host.bodies["POST /api/v1/learning/jobs/"+host.pin+"/progress"], &progress)
	if progress.Phase != "failed" || !strings.Contains(progress.Message, `contains "="`) {
		t.Fatalf("cohort with = progress = %+v", progress)
	}

	// A claim another worker won is nothing to run (3), not a failure.
	host = newFakeHost(t)
	host.refuseAt = "/claim"
	server5 := httptest.NewServer(host)
	defer server5.Close()
	output.Reset()
	if code := run(context.Background(), []string{"-host", server5.URL, "-work-dir", work, "-trainer", trainer, "-once"}, &output); code != 3 {
		t.Fatalf("lost claim exit %d: %s", code, output.String())
	}

	// A host that cannot be listed ends a -once run with 2 instead of
	// polling forever.
	host = newFakeHost(t)
	host.token = "needed"
	server6 := httptest.NewServer(host)
	defer server6.Close()
	output.Reset()
	if code := run(context.Background(), []string{"-host", server6.URL, "-work-dir", work, "-trainer", trainer, "-once", "-poll", "50ms"}, &output); code != 2 || len(host.calls) != 1 {
		t.Fatalf("unlistable host exit %d calls %v: %s", code, host.calls, output.String())
	}
}

// The trainer runs without the worker's credential, and the host receives
// the trainer's outcome and last line rather than its output.
func TestTrainerSeesNoCredentialAndTheHostSeesNoOutput(t *testing.T) {
	work := t.TempDir()
	trainer := fakeTrainer(t, work)
	t.Setenv("WANT_COHORT", "cohort-1")
	t.Setenv("SHOAL_LEARNING_TOKEN", "s3cr3t-token")
	t.Setenv("OTHER_SECRET", "also-hidden")
	t.Setenv("PLAIN_VALUE", "visible")
	host := newFakeHost(t)
	server := httptest.NewServer(host)
	defer server.Close()
	// The worker refuses to send a token to a plaintext non-loopback host,
	// and httptest listens on loopback, so the token is accepted here.
	var output bytes.Buffer
	if code := run(context.Background(), []string{"-host", server.URL, "-work-dir", work, "-trainer", trainer, "-trainer-arg", "--leak-env", "-once"}, &output); code != 2 {
		t.Fatalf("exit %d: %s", code, output.String())
	}
	var progress struct{ Phase, Message string }
	json.Unmarshal(host.bodies["POST /api/v1/learning/jobs/"+host.pin+"/progress"], &progress)
	if progress.Phase != "failed" || strings.Contains(progress.Message, "s3cr3t") || strings.Contains(progress.Message, "also-hidden") {
		t.Fatalf("the host saw a credential: %+v", progress)
	}
	if !strings.Contains(progress.Message, "token= other= plain=visible") {
		t.Fatalf("expected the trainer's last line without credentials in the message: %+v", progress)
	}
	logged, err := os.ReadFile(filepath.Join(work, host.pin, "1", "trainer.log"))
	if err != nil || !strings.Contains(string(logged), "plain=visible") {
		t.Fatalf("trainer.log = %q, %v", logged, err)
	}
	env := trainerEnvironment([]string{"PATH=/bin", "SHOAL_LEARNING_TOKEN=x", "MY_API_KEY_SECRET=y", "DB_PASSWORD=z", "AWS_CREDENTIALS=w", "HOME=/h"}, "SHOAL_LEARNING_TOKEN")
	if strings.Join(env, " ") != "PATH=/bin HOME=/h" {
		t.Fatalf("trainer environment = %v", env)
	}
}

// A listed pin that is not a content address is skipped rather than used
// as a path or a URL segment.
func TestWorkerSkipsAJobWhosePinIsNotAContentAddress(t *testing.T) {
	work := t.TempDir()
	trainer := fakeTrainer(t, work)
	base := filepath.Join(work, "a", "b", "work")
	hostPin := "../../escaped"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/learning/jobs" {
			json.NewEncoder(w).Encode(map[string]any{"jobs": []any{map[string]any{"job": map[string]any{"manifest_pin": hostPin}, "receipt": nil, "run": map[string]any{"requested": true, "runnable": true}}}})
			return
		}
		t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer server.Close()
	var output bytes.Buffer
	if code := run(context.Background(), []string{"-host", server.URL, "-work-dir", base, "-trainer", trainer, "-once"}, &output); code != 3 || !strings.Contains(output.String(), "not a content address") {
		t.Fatalf("exit %d: %s", code, output.String())
	}
	if _, err := os.Stat(filepath.Join(work, "a", "escaped")); err == nil {
		t.Fatal("the worker created a directory outside its work dir")
	}
}

// mismatchedExport serves export bytes that differ from the pin the claim
// recorded.
type mismatchedExport struct{ *fakeHost }

func (m mismatchedExport) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == exportsRoute {
		m.record(r)
		json.NewEncoder(w).Encode(map[string]any{"manifest": base64.StdEncoding.EncodeToString([]byte(`{"other":1}`)), "dataset": base64.StdEncoding.EncodeToString(m.dataset)})
		return
	}
	m.fakeHost.ServeHTTP(w, r)
}

func TestWorkerRefusesUnsafeConfiguration(t *testing.T) {
	var output bytes.Buffer
	if code := run(context.Background(), []string{"-host", "http://127.0.0.1:1"}, &output); code != 2 || !strings.Contains(output.String(), "-trainer is required") {
		t.Fatalf("no trainer: %d %s", code, output.String())
	}
	output.Reset()
	if code := run(context.Background(), []string{"-host", "http://127.0.0.1:1", "-trainer", "true", "-work-dir", "/tmp/a:b"}, &output); code != 2 || !strings.Contains(output.String(), "-work-dir") {
		t.Fatalf("work dir with a colon: %d %s", code, output.String())
	}
	t.Setenv("SHOAL_LEARNING_TOKEN", "secret")
	output.Reset()
	if code := run(context.Background(), []string{"-host", "http://example.test:8080", "-trainer", "true"}, &output); code != 2 || !strings.Contains(output.String(), "plaintext") {
		t.Fatalf("token over plaintext: %d %s", code, output.String())
	}
}
