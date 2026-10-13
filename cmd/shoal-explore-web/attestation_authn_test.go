// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	admissionapi "github.com/phrocker/shoal-oss/pkg/admission/api"
	attestationapi "github.com/phrocker/shoal-oss/pkg/attestation/api"
)

// TestTheShippedAuthenticatorReachesTheAttestationGate drives the shipped
// development authenticator, unmodified, through the authenticated transport
// (modelled on TestTheDispatchSurfaceAcceptsAnAuthenticatedRequest, #527).
// It cannot present: no shipped authenticator grants OperationExecute (#480),
// and its fixed principal has no client ID, so it can never be attested. What
// it must show is that its requests reach the gate rather than stopping at
// the correlation check, and are refused there with nothing disclosed.
func TestTheShippedAuthenticatorReachesTheAttestationGate(t *testing.T) {
	t.Parallel()
	h := newAttestationHarness(t)
	development, err := newDevelopmentAuthenticator(h.now)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(nil)
	mountAttestationPlane(t, h, server, development)
	post := func(path string, body any) (int, string) {
		t.Helper()
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		response, err := server.Client().Post(server.URL+path, "application/json", strings.NewReader(string(encoded)))
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		raw, _ := io.ReadAll(response.Body)
		if strings.Contains(string(raw), "correlation ID is required") {
			t.Fatalf("%s stopped at the correlation check: %s", path, raw)
		}
		return response.StatusCode, string(raw)
	}
	encode := func(v string) string { return base64.RawURLEncoding.EncodeToString([]byte(v)) }
	requestContext := map[string]any{
		"request_id": encode("body-request"), "reason_code": "operator_request",
		"deadline": h.now().Add(time.Hour).Format(time.RFC3339Nano),
	}
	status, body := post("/api/v1/fleet/actions", map[string]any{
		"context": requestContext, "id": encode("dev-deploy"), "idempotency_key": encode("dev-key"),
		"agent_id": encode("gateway"), "agent_generation": 1, "capability": "ops", "action": "deploy",
		"source_id": workspaceSourceID, "policy_id": workspaceGrantPolicyID,
		"object_id": encode("release-7"), "input": json.RawMessage(`{"version":"7"}`),
	})
	if status != http.StatusCreated {
		t.Fatalf("enqueue = %d %s", status, body)
	}
	var queued struct {
		Version uint64 `json:"version"`
	}
	if err := json.Unmarshal([]byte(body), &queued); err != nil || queued.Version == 0 {
		t.Fatalf("enqueue response %s: %v", body, err)
	}
	status, body = post("/api/v1/fleet/actions/"+encode("dev-deploy")+"/claim", map[string]any{
		"context": requestContext, "expected_version": queued.Version,
		"claim_id": encode("dev-claim"), "lease": int64(time.Minute),
	})
	if status != http.StatusConflict || !strings.Contains(body, "executor attestation") {
		t.Fatalf("unattested claim through the shipped authenticator = %d %s", status, body)
	}
	for _, leak := range []string{"exattest", "sha256", "operator-key"} {
		if strings.Contains(body, leak) {
			t.Fatalf("refusal leaks %q: %s", leak, body)
		}
	}
	// Path B through the same authenticator: the existing durable denial.
	status, body = post(admissionapi.RequestRoute, map[string]any{
		"context": requestContext, "id": encode("dev-admission"), "idempotency_key": encode("dev-admission-key"),
		"token_id": encode("dev-token"), "agent_id": encode("gateway"), "agent_generation": 1,
		"capability": "ops", "action": "deploy", "source_id": workspaceSourceID,
		"policy_id": workspaceGrantPolicyID, "object_id": encode("release-7"),
		"effects": []string{"external"}, "input": json.RawMessage(`{"version":"7"}`),
		"lease": int64(time.Minute),
	})
	if status != http.StatusOK || !strings.Contains(body, `"denied"`) {
		t.Fatalf("admission through the shipped authenticator = %d %s", status, body)
	}
	// Presentation: refused by authorization (no execute), not by verification.
	wire, err := attestationapi.NewRequest("local", []byte("k"), []byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	status, body = post(attestationapi.Route, wire)
	if status != http.StatusUnauthorized || strings.Contains(body, attestationapi.RefusedMessage) {
		t.Fatalf("presentation without execute = %d %s", status, body)
	}
}
