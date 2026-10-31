// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
)

// The operator inbox over the real approval service: a held request appears
// to an eligible approver and not to its requester, the row carries what the
// decide route needs, and a decided request leaves the inbox.
func TestOperatorInboxOverTheRealApprovalService(t *testing.T) {
	t.Parallel()
	h := newApprovalHarness(t)
	inbox, err := webapi.NewInboxHandler(webapi.InboxSurface{Resolver: h.authority.Resolver(), Approvals: h.opened.approvals, Now: h.now})
	if err != nil {
		t.Fatal(err)
	}
	approvalHandler, err := webapi.NewFleetApprovalHandler(h.opened.approvals)
	if err != nil {
		t.Fatal(err)
	}
	encode := func(value []byte) string { return base64.RawURLEncoding.EncodeToString(value) }
	contextWire := func() map[string]any {
		return map[string]any{"request_id": encode([]byte("inbox-" + time.Now().Format(time.RFC3339Nano))), "reason_code": "inbox", "deadline": h.now().Add(time.Hour).Format(time.RFC3339Nano)}
	}
	serve := func(handler http.Handler, ctx context.Context, path string, body any) (int, map[string]any) {
		t.Helper()
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(encoded)).WithContext(ctx)
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		var decoded map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
			t.Fatalf("%s response is not JSON: %s", path, recorder.Body)
		}
		return recorder.Code, decoded
	}
	items := func(out map[string]any) []map[string]any {
		var rows []map[string]any
		for _, g := range out["approvals"].(map[string]any)["groups"].([]any) {
			for _, i := range g.(map[string]any)["items"].([]any) {
				rows = append(rows, i.(map[string]any))
			}
		}
		return rows
	}

	h.mustRequest(requester, h.held("held-1"), fleet.ApprovalPending)
	h.mustRequest(requester, h.held("held-2"), fleet.ApprovalPending)

	status, out := serve(inbox, h.as(approver), webapi.InboxRoute, map[string]any{"context": contextWire()})
	rows := items(out)
	if status != http.StatusOK || out["approvals"].(map[string]any)["available"] != true || len(rows) != 2 {
		t.Fatalf("approver inbox = %d %v", status, out)
	}
	group := out["approvals"].(map[string]any)["groups"].([]any)[0].(map[string]any)
	if group["capability"] != "ops" || group["action"] != "deploy" {
		t.Fatalf("group %v", group)
	}
	row := rows[0]["approval"].(map[string]any)
	if rows[0]["kind"] != "approval" || rows[0]["waiting"] != "decision" || row["input"] != nil || row["request_digest"] == "" || row["state"] != "pending" || strings.Contains(string(inboxJSON(t, out)), `"version":"7"`) {
		t.Fatalf("row %v", rows[0])
	}
	if out["candidates"].(map[string]any)["mounted"] != false {
		t.Fatal("candidates section claims a learning service")
	}

	// The requester is not an approver: the section says so and lists nothing.
	status, out = serve(inbox, h.as(requester), webapi.InboxRoute, map[string]any{"context": contextWire()})
	if status != http.StatusOK || out["approvals"].(map[string]any)["available"] != false || len(items(out)) != 0 {
		t.Fatalf("requester inbox = %d %v", status, out)
	}

	// The row carries what the decide route needs, and nothing else is
	// needed: the approver decides exactly the request they were shown.
	status, decided := serve(approvalHandler, h.as(approver), "/api/v1/fleet/approvals/"+rows[0]["id"].(string)+"/decide", map[string]any{
		"context": contextWire(), "request_digest": row["request_digest"], "policy_generation": row["policy_generation"], "verdict": "approve",
	})
	if status != http.StatusOK || decided["state"] == "pending" {
		t.Fatalf("decide = %d %v", status, decided)
	}
	status, out = serve(inbox, h.as(approver), webapi.InboxRoute, map[string]any{"context": contextWire()})
	if status != http.StatusOK || len(items(out)) != 1 || items(out)[0]["id"] == rows[0]["id"] {
		t.Fatalf("inbox after decision = %d %v", status, out)
	}

	// A stale digest cannot decide the remaining request.
	remaining := items(out)[0]
	status, _ = serve(approvalHandler, h.as(approver), "/api/v1/fleet/approvals/"+remaining["id"].(string)+"/decide", map[string]any{
		"context": contextWire(), "request_digest": row["request_digest"], "policy_generation": row["policy_generation"], "verdict": "approve",
	})
	if status == http.StatusOK {
		t.Fatal("a digest from another row decided a request")
	}
	// Without a context the inbox refuses before reaching the service.
	if status, _ = serve(inbox, h.as(approver), webapi.InboxRoute, map[string]any{}); status != http.StatusBadRequest {
		t.Fatalf("inbox without context = %d", status)
	}
}

func inboxJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
