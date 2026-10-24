package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/internal/embeddinginventory"
)

func TestInventoryServerRejectsNonLoopbackBind(t *testing.T) {
	if err := serveEmbeddingInventory("0.0.0.0:0", demoInventory, &bytes.Buffer{}); err == nil {
		t.Fatal("inventory server accepted a non-loopback listener")
	}
}

func TestInventoryViewIsLoopbackScopedReadOnlyAndMarkedAsSample(t *testing.T) {
	handler := inventoryHandler("127.0.0.1:8047", demoInventory)
	request := func(method, path, host string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, "http://127.0.0.1:8047"+path, nil)
		req.Host = host
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	if response := request(http.MethodGet, "/", "attacker.example"); response.Code != http.StatusMisdirectedRequest {
		t.Fatalf("foreign Host status = %d", response.Code)
	}
	if response := request(http.MethodPost, "/api/inventory", "127.0.0.1:8047"); response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("write request status = %d", response.Code)
	}
	response := request(http.MethodGet, "/", "127.0.0.1:8047")
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), "Sample data") ||
		!strings.Contains(response.Body.String(), "Backfill is read-only") ||
		!strings.Contains(response.Body.String(), "Why unknown") {
		t.Fatalf("operator page status=%d body=%s", response.Code, response.Body.String())
	}
	jsonResponse := request(http.MethodGet, "/api/inventory", "127.0.0.1:8047")
	var report embeddinginventory.Report
	if jsonResponse.Code != http.StatusOK || json.Unmarshal(jsonResponse.Body.Bytes(), &report) != nil ||
		!report.Demo || report.ApplyAvailable || report.Counts.Unknown != 2 {
		t.Fatalf("inventory JSON status=%d body=%s", jsonResponse.Code, jsonResponse.Body.String())
	}
}
