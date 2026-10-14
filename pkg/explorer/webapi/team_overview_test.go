// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file distributed
// with this work for additional information regarding copyright ownership.
package webapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer/teamoverview"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestTeamOverviewHTTPIsBoundedAndPropagatesAuthorization(t *testing.T) {
	provider := &teamOverviewProviderStub{
		response: teamoverview.Response{
			Team: teamoverview.Team{ID: "team-1", Name: "Demo"},
		},
	}
	handler, err := webapi.NewTeamOverviewHandler(provider)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{
		"team_id":"team-1",
		"source_id":"c291cmNl",
		"policy_id":"cG9saWN5",
		"history_days":14,
		"limit":25
	}`)
	request := httptest.NewRequest(
		http.MethodPost, webapi.TeamOverviewRoute, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	if provider.request.TeamID != "team-1" ||
		string(provider.request.SourceID) != "source" ||
		string(provider.request.PolicyID) != "policy" ||
		provider.request.HistoryDays != 14 || provider.request.Limit != 25 {
		t.Fatalf("request = %#v", provider.request)
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("cache control = %q", response.Header().Get("Cache-Control"))
	}

	provider.err = shoal.NewError(
		shoal.ErrorUnauthorized, "team overview is not authorized")
	response = httptest.NewRecorder()
	request = httptest.NewRequest(
		http.MethodPost, webapi.TeamOverviewRoute, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("authorization status = %d, body = %s",
			response.Code, response.Body)
	}
	var envelope struct {
		Code shoal.ErrorCode `json:"code"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Code != shoal.ErrorUnauthorized {
		t.Fatalf("error code = %q", envelope.Code)
	}
}

type teamOverviewProviderStub struct {
	request  teamoverview.Request
	response teamoverview.Response
	err      error
}

func (s *teamOverviewProviderStub) Overview(
	_ context.Context, request teamoverview.Request,
) (teamoverview.Response, error) {
	s.request = request
	return s.response, s.err
}
