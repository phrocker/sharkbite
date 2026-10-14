// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/model"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const chatAcceptanceAnswer = "accepted-grounded-answer"

func TestObservedEvidenceNodeSetsIgnoreOrdering(t *testing.T) {
	ids := []shoal.ID{"z", "a", "z", "", shoal.ID("\x00\xff")}
	want := []shoal.ID{shoal.ID("\x00\xff"), "a", "z"}
	if got := canonicalObservedIDs(ids); !reflect.DeepEqual(got, want) {
		t.Fatalf("observed node set = %q, want %q", got, want)
	}
	if ids[0] != "z" {
		t.Fatal("canonicalization mutated caller order")
	}
	evidence := []interaction.EvidenceReference{
		{NodeIDs: []shoal.ID{"z", "a"}},
		{NodeIDs: []shoal.ID{shoal.ID("\x00\xff"), "z"}},
	}
	if got := observedEvidenceNodeIDs(evidence); !reflect.DeepEqual(got, want) {
		t.Fatalf("evidence node set = %q, want %q", got, want)
	}
}

type chatAcceptanceSink struct {
	*explorer.Explorer
	failOperation interaction.Operation
	failures      atomic.Int32
}

func (s *chatAcceptanceSink) RecordInteraction(ctx context.Context, session interaction.Session) error {
	_, err := s.RecordInteractionResult(ctx, session)
	return err
}

func (s *chatAcceptanceSink) RecordInteractionResult(
	ctx context.Context, session interaction.Session,
) (interaction.Session, error) {
	if session.Operation == s.failOperation && session.StopReason != "admitted" {
		s.failures.Add(1)
		return interaction.Session{}, errors.New("acceptance recording failure")
	}
	return s.Explorer.RecordInteractionResult(ctx, session)
}

// Cite every supplied document anchor, unlike FakeGenerator's single citation.
// The production harness still parses and verifies this model output.
type chatAcceptanceGenerator struct {
	calls atomic.Int32
}

func (g *chatAcceptanceGenerator) Generate(ctx context.Context, request model.GenerateRequest) (model.GenerateResult, error) {
	g.calls.Add(1)
	if err := ctx.Err(); err != nil {
		return model.GenerateResult{}, err
	}
	var prompt struct {
		Evidence []struct {
			ID    string `json:"id"`
			Quote string `json:"quote"`
		} `json:"evidence"`
	}
	if err := json.Unmarshal([]byte(request.Prompt), &prompt); err != nil {
		return model.GenerateResult{}, err
	}
	var ids []string
	for _, evidence := range prompt.Evidence {
		if evidence.Quote != "" {
			ids = append(ids, evidence.ID)
		}
	}
	if len(ids) == 0 {
		return model.GenerateResult{}, errors.New("acceptance model received no document evidence")
	}
	payload := map[string]any{
		"action": "stop", "correlation_id": "YWNjZXB0YW5jZS1zdG9w",
		"claims": []map[string]any{{
			"subject": "ZW50aXR5", "predicate": "c3VtbWFyeQ==",
			"object":     map[string]any{"type": "string", "value": chatAcceptanceAnswer},
			"confidence": 1, "evidence_ids": ids,
		}},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return model.GenerateResult{}, err
	}
	return model.GenerateResult{
		Text: string(encoded), Provenance: model.Provenance{Provider: "fake", Model: "deterministic"},
	}, nil
}

func assertChatRecordingFailure(t *testing.T, sink *chatAcceptanceSink, generator *chatAcceptanceGenerator) {
	t.Helper()
	if sink.failures.Load() == 0 {
		t.Fatal("request failed before reaching the selected recording boundary")
	}
	if sink.failOperation == interaction.OperationRetrieval && generator.calls.Load() != 0 {
		t.Fatal("model ran after initial retrieval recording failed")
	}
	if sink.failOperation != interaction.OperationRetrieval && generator.calls.Load() == 0 {
		t.Fatal("test did not exercise recording after model generation")
	}
}

func assertWorkspaceChatProvenance(
	t *testing.T, server *httptest.Server, workspaceID string, envelope webapi.CitationEnvelope,
) {
	t.Helper()
	call := func(method, path, body string, output any) {
		t.Helper()
		request, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set(webapi.WorkspaceIDHeader, workspaceID)
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
			t.Fatalf("%s %s failed with workspace settings: %d %s", method, path, response.StatusCode, data)
		}
		if err := json.Unmarshal(data, output); err != nil {
			t.Fatal(err)
		}
	}
	sessionID := base64.RawURLEncoding.EncodeToString([]byte(envelope.SessionID))
	var listed webapi.ProvenanceListResponse
	call(http.MethodGet, "/api/v1/provenance", "", &listed)
	found := false
	for _, summary := range listed.Interactions {
		found = found || summary.SessionID == sessionID
	}
	if !found {
		t.Fatal("workspace provenance omitted its recorded chat")
	}
	var inspected webapi.ProvenanceSession
	call(http.MethodGet, "/api/v1/provenance/"+sessionID, "", &inspected)
	citedIDs := make([]string, len(envelope.CitedSourceIDs))
	for index, id := range envelope.CitedSourceIDs {
		citedIDs[index] = base64.RawURLEncoding.EncodeToString([]byte(id))
	}
	if !reflect.DeepEqual(inspected.CitedIDs, citedIDs) ||
		inspected.OutputVisibility != envelope.OutputVisibility {
		t.Fatal("workspace provenance inspection lost citations or output restriction")
	}
	var folded, unfolded webapi.ProvenanceFold
	call(http.MethodPost, "/api/v1/provenance/fold",
		`{"session_ids":["`+sessionID+`"]}`, &folded)
	call(http.MethodPost, "/api/v1/provenance/unfold",
		`{"fold_id":"`+folded.FoldID+`"}`, &unfolded)
	if len(unfolded.Members) != 1 || unfolded.Members[0].SessionID != sessionID ||
		!reflect.DeepEqual(unfolded.Members[0].CitedIDs, citedIDs) ||
		interaction.Expression(unfolded.Members[0].OutputVisibility) != envelope.OutputVisibility {
		t.Fatal("workspace fold/unfold lost uncapped citations or output restriction")
	}
}
