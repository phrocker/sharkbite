// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	admissionapi "github.com/phrocker/shoal-oss/pkg/admission/api"
	"github.com/phrocker/shoal-oss/pkg/sdk"
)

// TestAnExtensionCanAskForAdmission proves the admission seam is reachable
// through the public surface alone: this module imports only pkg/sdk and the
// allowlisted pkg/admission/api, and builds with GOWORK=off.
func TestAnExtensionCanAskForAdmission(t *testing.T) {
	var seen admissionapi.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != admissionapi.RequestRoute {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &seen)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = io.WriteString(w, `{"outcome":"denied","withhold":[]}`+"\n")
	}))
	defer server.Close()
	client, err := sdk.New(sdk.Config{
		BaseURL: server.URL, HTTPClient: server.Client(),
		Token: func(context.Context) (string, error) { return "token", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	request := admissionapi.Request{
		Context: admissionapi.RequestContext{
			RequestID: admissionapi.EncodeID([]byte("request")), ReasonCode: "example",
			Deadline: time.Now().Add(time.Minute),
		},
		ID:             admissionapi.EncodeID([]byte("admission")),
		IdempotencyKey: admissionapi.EncodeID([]byte("key")),
		TokenID:        admissionapi.EncodeID([]byte("token")),
		AgentID:        admissionapi.EncodeID([]byte("collector")), AgentGeneration: 1,
		Capability: "collect", Action: "tail",
		ObjectID: admissionapi.EncodeID([]byte("file")),
		Effects:  []string{admissionapi.EffectReadsCorpus},
		Input:    json.RawMessage(`{}`), Lease: time.Minute,
	}
	grant, err := client.Admission().Request(context.Background(), request,
		admissionapi.RequestOptions{MinReportWindow: 5 * time.Second})
	if !errors.Is(err, admissionapi.ErrDenied) || grant.Token != nil {
		t.Fatalf("grant = %#v, %v", grant, err)
	}
	if seen.TokenID != request.TokenID {
		t.Fatalf("plane saw %#v", seen)
	}
}
