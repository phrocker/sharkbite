// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// TestApprovalRequiredOverHTTP pins the transport answer every client will
// branch on, through the real handlers over the real composition.
//
// A direct enqueue or invoke of an approval-required action is 409 with code
// "conflict": the caller is authorized and the request is well formed, but the
// action's registration requires the approval route. It must not be a 500 — a
// bare sentinel through writeError would be — and it must not be 401, because
// the work may well happen once someone else approves it.
//
// The approval route itself answers 202 for a held request, and Path B answers
// a denial.
func TestApprovalRequiredOverHTTP(t *testing.T) {
	h := newApprovalHarness(t)
	fleetHandler, err := webapi.NewFleetHandler(
		h.opened.fleetRegistry, h.opened.fleetDispatch)
	if err != nil {
		t.Fatal(err)
	}
	admissionHandler, err := webapi.NewAdmissionHandler(h.opened.admission)
	if err != nil {
		t.Fatal(err)
	}
	approvalHandler, err := webapi.NewFleetApprovalHandler(h.opened.approvals)
	if err != nil {
		t.Fatal(err)
	}
	encode := func(value []byte) string {
		return base64.RawURLEncoding.EncodeToString(value)
	}
	contextWire := map[string]any{
		"request_id": encode([]byte("http-request")), "reason_code": "test",
		"deadline": h.now().Add(3 * time.Hour).Format(time.RFC3339Nano),
	}
	action := func(id string) map[string]any {
		return map[string]any{
			"context": contextWire, "id": encode([]byte(id)),
			"idempotency_key": encode([]byte("key-" + id)),
			"agent_id":        encode([]byte("gateway")), "agent_generation": 1,
			"capability": "ops", "action": "deploy",
			"source_id": workspaceSourceID, "policy_id": workspaceGrantPolicyID,
			"object_id": encode([]byte("release-7")),
			"input":     json.RawMessage(`{"version":"7"}`),
		}
	}
	serve := func(
		handler http.Handler, ctx context.Context, path string, body any,
	) (int, map[string]any) {
		t.Helper()
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(
			http.MethodPost, path, bytes.NewReader(encoded)).WithContext(ctx)
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		var decoded map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
			t.Fatalf("%s response is not JSON: %s", path, recorder.Body)
		}
		return recorder.Code, decoded
	}

	status, body := serve(fleetHandler, h.as(requester),
		"/api/v1/fleet/actions", action("http-enqueue"))
	if status != http.StatusConflict || body["code"] != string(shoal.ErrorConflict) {
		t.Fatalf("direct enqueue = %d %v, want 409 conflict", status, body)
	}
	status, body = serve(fleetHandler, h.as(requester),
		"/api/v1/fleet/actions/invoke", map[string]any{
			"action": action("http-invoke"), "claim_id": encode([]byte("claim")),
			"lease": int64(time.Minute),
		})
	if status != http.StatusConflict || body["code"] != string(shoal.ErrorConflict) {
		t.Fatalf("direct invoke = %d %v, want 409 conflict", status, body)
	}

	status, body = serve(approvalHandler, h.as(requester),
		"/api/v1/fleet/approvals/request", action("http-held"))
	if status != http.StatusAccepted || body["state"] != "pending" {
		t.Fatalf("approval request = %d %v, want 202 pending", status, body)
	}

	// Status carries the effective state beside the stored one.
	statusBody := map[string]any{
		"request_id": encode([]byte("http-status")), "reason_code": "test",
		"deadline": h.now().Add(2 * time.Hour).Format(time.RFC3339Nano),
	}
	statusPath := "/api/v1/fleet/approvals/" + encode([]byte("http-held")) + "/status"
	status, body = serve(approvalHandler, h.as(requester), statusPath, statusBody)
	if status != http.StatusOK || body["state"] != "pending" ||
		body["stored_state"] != "pending" || body["condition"] != nil {
		t.Fatalf("approval status = %d %v", status, body)
	}
	// Past the window the requester's Status records the expiry it
	// observes (#655), so the stored state is the effective one.
	h.advance(fleet.DefaultApprovalWindow + time.Second)
	status, body = serve(approvalHandler, h.as(requester), statusPath, statusBody)
	if status != http.StatusOK || body["state"] != "expired" ||
		body["stored_state"] != "expired" || body["condition"] != nil {
		t.Fatalf("lapsed approval status = %d %v", status, body)
	}
	contextWire["deadline"] = h.now().Add(3 * time.Hour).Format(time.RFC3339Nano)

	admission := map[string]any{
		"context": contextWire, "id": encode([]byte("http-admission")),
		"idempotency_key": encode([]byte("admission-key")),
		"token_id":        encode([]byte("token")),
		"agent_id":        encode([]byte("gateway")), "agent_generation": 1,
		"capability": "ops", "action": "deploy",
		"source_id": workspaceSourceID, "policy_id": workspaceGrantPolicyID,
		"object_id": encode([]byte("release-7")), "effects": []string{"external"},
		"input": json.RawMessage(`{"version":"7"}`), "lease": int64(time.Minute),
	}
	// Path B answers the denial on the first attempt. This was pinned as a
	// 503, because the denial's cancellation could not be published by the
	// hosted publisher (#505) — fixed, so the first answer is now the 200 a
	// caller acts on.
	//
	// The replay is kept and still asserted: answering identically from the
	// record is the property that masked the defect for three reviews, and it
	// is worth pinning on its own.
	status, body = serve(admissionHandler, h.as(requester),
		"/api/v1/admission/request", admission)
	if status != http.StatusOK || body["outcome"] != "denied" ||
		body["token"] != nil {
		t.Fatalf("path B first answer = %d %v, want 200 denied with no token",
			status, body)
	}
	status, body = serve(admissionHandler, h.as(requester),
		"/api/v1/admission/request", admission)
	if status != http.StatusOK || body["outcome"] != "denied" ||
		body["token"] != nil {
		t.Fatalf("path B = %d %v, want 200 denied with no token", status, body)
	}
}
