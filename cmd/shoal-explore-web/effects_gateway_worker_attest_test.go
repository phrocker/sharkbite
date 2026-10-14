// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/effectsgateway"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
)

// pathRecorder keeps the path of every explorer request, in order.
type pathRecorder struct {
	mu    sync.Mutex
	paths []string
	next  http.RoundTripper
}

func (p *pathRecorder) RoundTrip(request *http.Request) (*http.Response, error) {
	p.mu.Lock()
	p.paths = append(p.paths, request.URL.Path)
	p.mu.Unlock()
	return p.next.RoundTrip(request)
}

func (p *pathRecorder) sequence() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, path := range p.paths {
		switch {
		case strings.Contains(path, "attestation"):
			out = append(out, "attest")
		case strings.HasSuffix(path, "/claim"):
			out = append(out, "claim")
		case strings.HasSuffix(path, "/complete"):
			out = append(out, "complete")
		}
	}
	return out
}

// TestTheWorkerAttestsBeforeItClaims: an action that requires attestation
// (#446) is claimed only after the worker presents its operator-signed
// statement — once, before the claim, never by way of a refused claim — over
// the real presentation route and the real verifier, and the claim names the
// attestation.
func TestTheWorkerAttestsBeforeItClaims(t *testing.T) {
	h := newAttestationHarness(t)
	server := newAttestationPlane(t, h, map[string]principal{
		"presenter": attestedPresenter, "worker": attestedWorker,
	})
	base, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	recorder := &pathRecorder{next: server.Client().Transport}
	httpClient := *server.Client()
	httpClient.Transport = dateTransport{now: h.now, next: recorder}
	unbound, err := effectsgateway.NewDispatchClient(base, &httpClient,
		func() (string, error) { return "presenter", nil }, h.now)
	if err != nil {
		t.Fatal(err)
	}
	client, err := unbound.BindExecutor("local")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	statementFile, keyFile := filepath.Join(dir, "statement"), filepath.Join(dir, "key")
	if err := os.WriteFile(statementFile, h.statement(attestedPresenter, "k1", h.now(), time.Hour), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, []byte("k1"), 0o600); err != nil {
		t.Fatal(err)
	}

	queued, err := h.opened.fleetDispatch.Enqueue(h.as(attestedWorker), fleet.EnqueueRequest{
		ID: []byte("deploy-worker"), IdempotencyKey: []byte("key-deploy-worker"),
		AgentID: "gateway", AgentGeneration: 1, Capability: "ops", Action: "deploy",
		SourceID: workspaceSourceID, PolicyID: workspaceGrantPolicyID, ObjectID: "release-8",
		Input: json.RawMessage(`{"body":{"version":"8"}}`), Context: h.context(h.now().Add(time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}

	clock := &harnessClock{now: h.now, advance: h.advance}
	target := newPaymentTarget(t)
	routes := `[{"action":"deploy","method":"POST","path":"/v1/deploys","effects":["external"],"idempotency":"key"}]`
	worker := startWorkerWith(t, client, clock, target, routes, "gateway", "ops",
		workerOptions{dir: t.TempDir()}, func(config *effectsgateway.WorkerConfig) {
			config.RequiresAttestation = func(action string) bool { return action == "deploy" }
			config.AttestationStatementFile, config.AttestationKeyFile = statementFile, keyFile
		})
	waitFor(t, "success", func() bool { return h.status(queued.ID).State == fleet.DispatchSucceeded })
	record := h.status(queued.ID)
	if record.ClaimAttestationID == "" {
		t.Fatal("the claim names no attestation")
	}
	if got := strings.Join(recorder.sequence(), ","); got != "attest,claim,complete" {
		t.Fatalf("explorer requests = %s, want attest,claim,complete", got)
	}
	if effects, _, _ := target.stats(); effects != 1 {
		t.Fatalf("%d effects", effects)
	}
	if err := worker.stop(t); err != nil {
		t.Fatal(err)
	}
}
