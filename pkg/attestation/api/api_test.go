// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientPresent(t *testing.T) {
	var seen Request
	status, body := http.StatusOK, `{"attestation_id":"exattest:1","expires_at":"2026-09-06T13:00:00Z","future":1}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != Route || r.Header.Get("Authorization") != "Bearer t" {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &seen)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()
	client, err := NewClient(Config{BaseURL: server.URL, HTTPClient: server.Client(),
		Token: func(context.Context) (string, error) { return "t", nil }})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := client.Present(context.Background(), "exec", []byte("key"), []byte(`{"a":1}`))
	if err != nil || receipt.AttestationID != "exattest:1" {
		t.Fatalf("present = %+v, %v", receipt, err)
	}
	if seen.ExecutorRef != "exec" || seen.IdempotencyKey != Encode([]byte("key")) || seen.Report != Encode([]byte(`{"a":1}`)) {
		t.Fatalf("request = %+v", seen)
	}

	status, body = http.StatusUnauthorized, `{"code":"unauthorized","message":"unauthorized: attestation refused"}`
	if _, err := client.Present(context.Background(), "exec", []byte("key"), []byte("r")); !errors.Is(err, ErrRefused) {
		t.Fatalf("refusal = %v", err)
	}
	status, body = http.StatusUnauthorized, `{"code":"unauthorized","message":"authentication required"}`
	var httpErr *HTTPError
	if _, err := client.Present(context.Background(), "exec", []byte("key"), []byte("r")); !errors.As(err, &httpErr) || errors.Is(err, ErrRefused) {
		t.Fatalf("authentication failure = %v", err)
	}
	status, body = http.StatusOK, `{"attestation_id":"a","attestation_id":"b","expires_at":"2026-09-06T13:00:00Z"}`
	if _, err := client.Present(context.Background(), "exec", []byte("key"), []byte("r")); err == nil {
		t.Fatal("a duplicate-key response was accepted")
	}
	if _, err := client.Present(context.Background(), "", []byte("key"), []byte("r")); err == nil {
		t.Fatal("an empty ref was sent")
	}
	if _, err := NewClient(Config{BaseURL: "https://x/prefix", Token: func(context.Context) (string, error) { return "t", nil }}); err == nil {
		t.Fatal("a path-prefixed base URL was accepted")
	}
}
