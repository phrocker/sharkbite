// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	dispatchapi "github.com/phrocker/shoal-oss/pkg/dispatch/api"
	"github.com/phrocker/shoal-oss/pkg/sdk"
)

// TestAnExtensionCanSpeakDispatch proves the worker's dispatch routes are
// reachable through the public surface alone: this module imports only
// pkg/sdk and the allowlisted pkg/dispatch/api, and builds with GOWORK=off.
func TestAnExtensionCanSpeakDispatch(t *testing.T) {
	var correlation string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != dispatchapi.PullRoute {
			http.NotFound(w, r)
			return
		}
		correlation = r.Header.Get(dispatchapi.CorrelationIDHeader)
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = io.WriteString(w, `{"actions":[{"id":"YQ","version":1,"state":"queued",`+
			`"agent_id":"","agent_generation":1,"capability":"c","action":"a",`+
			`"request_id":"","deadline":"2026-09-06T12:00:00Z","created_at":"2026-09-06T12:00:00Z",`+
			`"updated_at":"2026-09-06T12:00:00Z","effect_possible":false}]}`+"\n")
	}))
	defer server.Close()
	client, err := sdk.New(sdk.Config{
		BaseURL: server.URL, HTTPClient: server.Client(),
		Token: func(context.Context) (string, error) { return "token", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	page, err := client.Dispatch().Pull(context.Background(), dispatchapi.Context{
		RequestID: []byte("request"), CorrelationID: []byte("poll-1"),
		ReasonCode: "example", Deadline: time.Now().Add(time.Minute),
	}, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 1 || string(page.Records[0].ID) != "a" ||
		page.Records[0].State != dispatchapi.DispatchQueued || correlation != "poll-1" {
		t.Fatalf("page = %+v, correlation %q", page, correlation)
	}
}
