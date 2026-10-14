// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied. See the License for the
// specific language governing permissions and limitations
// under the License.

package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/explorer/workspace"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/inference/harness"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/reasoning"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type testAskProvider struct {
	request  webapi.AskRequest
	response webapi.CitationEnvelope
	err      error
}

type capturingAskProvider struct {
	delegate webapi.AskProvider
	response webapi.CitationEnvelope
	err      error
}

type capturingResultSink struct {
	interaction.ResultSink
	lastError error
}

func (s *capturingResultSink) RecordInteraction(
	ctx context.Context, session interaction.Session,
) error {
	_, err := s.RecordInteractionResult(ctx, session)
	return err
}

func (s *capturingResultSink) RecordInteractionResult(
	ctx context.Context, session interaction.Session,
) (interaction.Session, error) {
	result, err := s.ResultSink.RecordInteractionResult(ctx, session)
	s.lastError = err
	return result, err
}

func (p *capturingAskProvider) Ask(
	ctx context.Context, request webapi.AskRequest,
) (webapi.CitationEnvelope, error) {
	response, err := p.delegate.Ask(ctx, request)
	p.response = response
	p.err = err
	return response, err
}

func (p *testAskProvider) Ask(
	_ context.Context, request webapi.AskRequest,
) (webapi.CitationEnvelope, error) {
	p.request = request
	return p.response, p.err
}

type testInteractionProvider struct {
	listed    webapi.ProvenanceListRequest
	inspected shoal.ID
	folded    webapi.ProvenanceFoldRequest
	unfolded  webapi.ProvenanceUnfoldRequest
}

type revokingObservedTool struct {
	revoke   func() error
	sourceID shoal.ID
}

func (*revokingObservedTool) Tool() Tool {
	return Tool{
		Name: "test.revoking_read", Description: "read then revoke",
		InputSchema: json.RawMessage(
			`{"type":"object","properties":{},"additionalProperties":false}`),
		Annotations: readOnlyAnnotations(),
		Execution:   &ToolExecution{TaskSupport: "forbidden"},
	}
}

func (p *revokingObservedTool) Call(
	context.Context, json.RawMessage,
) (any, error) {
	if err := p.revoke(); err != nil {
		return nil, err
	}
	return struct {
		Secret string `json:"secret"`
	}{Secret: "must-not-ship"}, nil
}

func (*revokingObservedTool) ToolAuthorizationOperation() auth.Operation {
	return auth.OperationRead
}

func (p *revokingObservedTool) ObserveToolResult(
	any,
) (ToolObservation, error) {
	return ToolObservation{RetrievedNodeIDs: []shoal.ID{p.sourceID}}, nil
}

func (p *testInteractionProvider) ListProvenance(
	_ context.Context, request webapi.ProvenanceListRequest,
) (webapi.ProvenanceListResponse, error) {
	p.listed = request
	return webapi.ProvenanceListResponse{}, nil
}

func (p *testInteractionProvider) InspectProvenance(
	_ context.Context, id shoal.ID,
) (webapi.ProvenanceSession, error) {
	p.inspected = id
	return webapi.ProvenanceSession{}, nil
}

func (p *testInteractionProvider) FoldProvenance(
	_ context.Context, request webapi.ProvenanceFoldRequest,
) (webapi.ProvenanceFold, error) {
	p.folded = request
	return webapi.ProvenanceFold{}, nil
}

func (p *testInteractionProvider) UnfoldProvenance(
	_ context.Context, request webapi.ProvenanceUnfoldRequest,
) (webapi.ProvenanceFold, error) {
	p.unfolded = request
	return webapi.ProvenanceFold{}, nil
}

func TestProviderToolsExposeStableOperationsAndDelegate(t *testing.T) {
	ask := &testAskProvider{}
	askTool, err := NewAskTool(ask)
	if err != nil {
		t.Fatal(err)
	}
	if askTool.Tool().Name != ToolAsk ||
		askTool.(OptionalToolAuthorizationProvider).
			ToolAuthorizationOperation() != auth.OperationRetrieve ||
		askTool.Tool().Annotations == nil ||
		*askTool.Tool().Annotations.ReadOnlyHint {
		t.Fatalf("ask tool contract = %+v", askTool.Tool())
	}
	if _, err := askTool.Call(
		context.Background(),
		json.RawMessage(`{"question":"What changed?","top_k":3}`),
	); err != nil {
		t.Fatal(err)
	}
	if ask.request.Question != "What changed?" || ask.request.TopK != 3 {
		t.Fatalf("ask request = %+v", ask.request)
	}

	provenance := &testInteractionProvider{}
	tools, err := NewInteractionTools(provenance)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 4 {
		t.Fatalf("interaction tools = %d, want 4", len(tools))
	}
	byName := make(map[string]OptionalToolProvider, len(tools))
	for _, tool := range tools {
		byName[tool.Tool().Name] = tool
		if tool.(OptionalToolAuthorizationProvider).
			ToolAuthorizationOperation() != auth.OperationRead {
			t.Fatalf("%s authorization operation is not read", tool.Tool().Name)
		}
	}
	if _, err := byName[ToolProvenanceList].Call(
		context.Background(),
		json.RawMessage(`{"limit":7,"cursor":"ZgBh"}`),
	); err != nil {
		t.Fatal(err)
	}
	if provenance.listed.Limit != 7 ||
		provenance.listed.Cursor != "ZgBh" {
		t.Fatalf("listed request = %+v", provenance.listed)
	}
	sessionID := shoal.ID("\x00opaque-session")
	encodedSessionID := base64.RawURLEncoding.EncodeToString(
		[]byte(sessionID))
	if _, err := byName[ToolProvenanceInspect].Call(
		context.Background(),
		json.RawMessage(`{"session_id":"`+encodedSessionID+`"}`),
	); err != nil {
		t.Fatal(err)
	}
	if provenance.inspected != sessionID {
		t.Fatalf("inspected ID = %q, want %q",
			provenance.inspected, sessionID)
	}
	if annotations := byName[ToolProvenanceFold].Tool().Annotations; annotations == nil || *annotations.ReadOnlyHint ||
		!*annotations.IdempotentHint || *annotations.DestructiveHint {
		t.Fatalf("fold annotations = %+v", annotations)
	}
}

func TestCitationObservationPreservesCompleteEvidence(t *testing.T) {
	decision := testDecision(t)
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		t.Fatal(err)
	}

	snapshotAt := time.Now().UTC().Add(-time.Minute)
	citation := document.Citation{
		DocumentID: "document", RevisionID: "revision",
		SectionID: "section", SpanID: "span",
		Range: document.SourceRange{
			Start: document.SourcePosition{Offset: 1},
			End:   document.SourcePosition{Offset: 5},
		},
	}
	spaces := []shoal.ID{"space-a", "space-b"}
	aggregate, err := retrieval.EmbeddingSpaceSetID(spaces...)
	if err != nil {
		t.Fatal(err)
	}
	envelope := webapi.CitationEnvelope{
		SnapshotID:               "snapshot",
		SnapshotAsOf:             snapshotAt,
		AuthorizationFingerprint: shoal.ID(fingerprint.String()),
		AuthorizationExpiresAt:   decision.AuthenticationExpires(),
		RequestID:                decision.RequestID(),
		EmbeddingSpaceID:         aggregate,
	}
	projection := webapi.CitationEvidenceProjection{
		RetrievedSourceIDs: []shoal.ID{
			"document", "section", "span", "node-a", "node-b",
		},
		CitedSourceIDs: []shoal.ID{
			"document", "section", "span", "node-a", "node-b",
		},
		EffectiveVisibility: []string{"team", "restricted"},
		EmbeddingSpaceIDs:   spaces,
		Anchors: []webapi.CitationEvidenceAnchor{
			{
				AnchorID: "document-anchor", Use: reasoning.EvidenceCited,
				SourceIDs: []shoal.ID{"document", "section", "span"},
				EdgeIDs:   []shoal.ID{"contains-edge"},
				Citation:  &citation,
			},
			{
				AnchorID: "graph-anchor", Use: reasoning.EvidenceCited,
				SourceIDs: []shoal.ID{"node-a", "node-b"},
				EdgeIDs:   []shoal.ID{"semantic-edge"},
				Assertions: []webapi.CitationEvidenceAssertion{{
					AssertionID: "assertion", EdgeID: "semantic-edge",
					Origin: ontology.AssertionExplicit,
				}},
			},
		},
	}
	evidence := []interaction.EvidenceReference{
		{
			AnchorID: "document-anchor", Kind: interaction.EvidenceDocument,
			Citation: citation,
			NodeIDs:  []shoal.ID{"document", "section", "span"},
		},
		{
			AnchorID: interaction.DerivedID(
				"mcp_citation_path", "document-anchor"),
			Kind:    interaction.EvidenceGraph,
			NodeIDs: []shoal.ID{"document", "section"},
			EdgeIDs: []shoal.ID{"contains-edge"},
		},
		{
			AnchorID: "graph-anchor", Kind: interaction.EvidenceGraph,
			NodeIDs: []shoal.ID{"node-a", "node-b"},
			EdgeIDs: []shoal.ID{"semantic-edge"},
			Assertions: []interaction.AssertionReference{{
				AssertionID: "assertion", EdgeID: "semantic-edge",
				Origin: ontology.AssertionExplicit,
			}},
		},
	}
	observation, err := citationToolObservation(
		envelope, projection, evidence, evidence)
	if err != nil {
		t.Fatal(err)
	}
	observation, err = canonicalToolObservation(
		context.Background(), observation, decision)
	if err != nil {
		t.Fatal(err)
	}
	if len(observation.RetrievedEvidence) != 3 ||
		len(observation.CitedEvidence) != 3 {
		t.Fatalf("evidence cardinality = %d/%d, want 3/3",
			len(observation.RetrievedEvidence),
			len(observation.CitedEvidence))
	}
	var sawCitation, sawPath, sawAssertion bool
	for _, evidence := range observation.CitedEvidence {
		sawCitation = sawCitation ||
			evidence.Kind == interaction.EvidenceDocument &&
				evidence.Citation == citation
		sawPath = sawPath ||
			containsObservedID(evidence.EdgeIDs, "contains-edge")
		sawAssertion = sawAssertion ||
			len(evidence.Assertions) == 1 &&
				evidence.Assertions[0].AssertionID == "assertion" &&
				evidence.Assertions[0].EdgeID == "semantic-edge"
	}
	if !sawCitation || !sawPath || !sawAssertion {
		t.Fatalf("complete citation observation = %+v", observation)
	}

	sink := &testInteractionSink{}
	recorder, err := interaction.NewRecorder(context.Background(), sink)
	if err != nil {
		t.Fatal(err)
	}
	recordedAt := snapshotAt.Add(30 * time.Second)
	if err := recorder.SetClock(func() time.Time { return recordedAt }); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(Config{
		Service: &stubService{}, Authority: auth.NewAuthority(),
		Decisions: DecisionProviderFunc(func(context.Context) (auth.Decision, error) {
			return decision, nil
		}),
		Recorder: recorder,
		Snapshots: testSnapshotProvider{snapshot: explorer.Snapshot{
			ID: string(envelope.SnapshotID), AsOf: envelope.SnapshotAsOf,
		}},
		requestIDFactory: sequentialRequestIDs(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.recordToolOutcome(
		context.Background(), decision,
		explorer.Snapshot{
			ID: string(envelope.SnapshotID), AsOf: envelope.SnapshotAsOf,
		},
		auth.OperationRetrieve, ToolAsk, json.RawMessage(`{"question":"q"}`),
		observation, false, "succeeded", true,
	); err != nil {
		t.Fatal(err)
	}
	if len(sink.sessions) != 1 ||
		len(sink.sessions[0].Turns) != 1 ||
		len(sink.sessions[0].Turns[0].ToolCall.RetrievedEvidence) != 3 ||
		len(sink.sessions[0].CitedEvidence) != 3 ||
		len(sink.sessions[0].TouchedEdgeIDs()) != 2 {
		t.Fatalf("recorded complete evidence = %+v", sink.sessions)
	}
}

func TestHTTPAskAdapterUsesSharedChatAndPersistsCompleteEvidence(t *testing.T) {
	for _, surface := range []string{"mcp", "ask", "stream"} {
		t.Run(surface, func(t *testing.T) {
			testHTTPRecordedChatSurface(t, surface, "")
		})
	}
}

func TestHTTPChatSurfacesWithholdUnrecordedOutput(t *testing.T) {
	for _, surface := range []string{"mcp", "ask", "stream"} {
		for _, operation := range []interaction.Operation{
			interaction.OperationRetrieval,
			interaction.OperationInference,
			interaction.OperationChat,
			interaction.OperationToolCall,
		} {
			if operation == interaction.OperationToolCall && surface != "mcp" {
				continue
			}
			t.Run(surface+"/"+string(operation), func(t *testing.T) {
				testHTTPRecordedChatSurface(t, surface, operation)
			})
		}
	}
}

func testHTTPRecordedChatSurface(t *testing.T, surface string, failOperation interaction.Operation) {
	t.Helper()
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = corpus.Close() })
	if err := corpus.EnsureInteractionSink(context.Background()); err != nil {
		t.Fatal(err)
	}
	authority := auth.NewAuthority()
	sourceID := []byte("source")
	policyID := []byte("policy")
	selector, err := authorized.NewStaticPolicySelector(sourceID, policyID)
	if err != nil {
		t.Fatal(err)
	}
	generations := httpGenerationReader{
		domain: []byte("domain"), generation: 1,
	}
	sink := &chatAcceptanceSink{Explorer: corpus, failOperation: failOperation}
	client, err := authorized.NewClient(authorized.Config{
		Base: corpus, InteractionWriter: sink, InteractionReader: corpus,
		SnapshotValidator: corpus,
		Resolver:          authority.Resolver(), PolicySelector: selector,
		PolicyStore:      authorized.NewMemoryPolicyStore(),
		GenerationReader: generations, Clock: time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	setupDecision := workspaceHTTPDecision(t, "ask-setup")
	setupContext, err := authority.Binder().Bind(
		context.Background(), setupDecision)
	if err != nil {
		t.Fatal(err)
	}
	documentCount := 24
	if failOperation != "" {
		documentCount = 1
	}
	for index := 0; index < documentCount; index++ {
		if _, err := client.Ingest(setupContext, explorer.Source{
			URI:       "file:///mcp-chat-" + strconv.Itoa(index) + ".md",
			MediaType: explorer.MediaTypeMarkdown,
			Content:   "# MCP Chat\n\nDurable evidence is recorded before delivery " + strconv.Itoa(index) + ".\n",
		}); err != nil {
			t.Fatal(err)
		}
	}
	service, err := webapi.NewEmbeddedService(client)
	if err != nil {
		t.Fatal(err)
	}
	provenance, err := inference.NewModelProvenance(
		"fake", "deterministic", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	generator := &chatAcceptanceGenerator{}
	chat, err := webapi.NewChatService(
		context.Background(),
		webapi.ChatConfig{
			Client: client, Resolver: authority.Resolver(),
			Generator: generator,
			Model:     provenance,
			Budgets: harness.Budgets{
				MaxSteps: 8, MaxElapsed: 30 * time.Second,
				MaxInputTokens: 1_000_000, MaxOutputTokens: 8192, MaxEvidence: 128,
				MaxGraphHops: 4, MaxGraphNodes: 128, MaxFanout: 32,
				MaxRepeatedAction: 2,
			},
			RetrievalModes: []retrieval.Mode{retrieval.ModeLexical},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	provenanceProvider, err := webapi.NewInteractionService(client)
	if err != nil {
		t.Fatal(err)
	}
	capturedAsk := &capturingAskProvider{delegate: chat}
	askTool, err := NewAskTool(capturedAsk)
	if err != nil {
		t.Fatal(err)
	}
	provenanceTools, err := NewInteractionTools(provenanceProvider)
	if err != nil {
		t.Fatal(err)
	}
	optional := append([]OptionalToolProvider{askTool}, provenanceTools...)
	capturedSink := &capturingResultSink{ResultSink: client}
	server, err := NewServer(Config{
		Service: service, Authority: authority,
		Decisions:       DecisionProviderFunc(authority.Resolver().Resolve),
		InteractionSink: capturedSink, Snapshots: client, OptionalTools: optional,
		requestIDFactory: sequentialRequestIDs(),
	})
	if err != nil {
		t.Fatal(err)
	}

	settingsStore, err := workspace.OpenDurableStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = settingsStore.Close() })
	settingsProvider, err := workspace.NewProvider(
		settingsStore,
		workspace.ProviderOptions{
			Resolver: authority.Resolver(), GenerationReader: generations,
			Clock: time.Now,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	server.workspaceSettings = settingsProvider
	workspaceID := shoal.ID("ask-workspace")
	topK := uint32(32)
	outputBytes := uint64(1 << 20)
	settings, err := settingsProvider.Update(
		setupContext, workspaceID, workspace.UpdateRequest{
			ExpectedRevision: 0, MutationID: "ask-workspace-create",
			Narrowing: workspace.UpdateNarrowing{
				Budgets: workspace.Budgets{
					RetrievalTopK: &topK, OutputBytes: &outputBytes,
				},
				OutputPolicies: []workspace.OutputPolicySpec{{
					SourceID: sourceID, GrantPolicyID: policyID, Epoch: 1,
				}},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	workspaceHeader := base64.RawURLEncoding.EncodeToString(
		[]byte(workspaceID))
	var requestNumber atomic.Uint64
	var lastDecision auth.Decision
	authenticator := webapi.AuthenticatorFunc(func(
		*http.Request,
	) (auth.Decision, error) {
		lastDecision = workspaceHTTPDecision(
			t,
			shoal.ID("ask-http-"+
				strconv.FormatUint(requestNumber.Add(1), 10)),
		)
		return lastDecision, nil
	})
	httpServer := httptest.NewUnstartedServer(nil)
	handler, err := webapi.NewAuthenticatedHandler(
		service, authenticator, authority.Binder(),
		httpServer.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if err := handler.SetWorkspaceSettingsProvider(settingsProvider); err != nil {
		t.Fatal(err)
	}
	if err := handler.SetChatProvider(chat); err != nil {
		t.Fatal(err)
	}
	if err := handler.SetInteractionProvider(provenanceProvider); err != nil {
		t.Fatal(err)
	}
	transport, err := NewHTTPHandler(HTTPConfig{
		Server: server,
		AllowedOrigins: []string{
			"http://" + httpServer.Listener.Addr().String(),
		},
		RequireWorkspaceSettings: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := handler.MountAuthenticated("/mcp", transport); err != nil {
		t.Fatal(err)
	}
	httpServer.Config.Handler = handler
	httpServer.Start()
	t.Cleanup(httpServer.Close)

	var envelope webapi.CitationEnvelope
	if surface == "mcp" {
		session, _ := initializeHTTPWithWorkspace(
			t, httpServer.Client(), httpServer.URL+"/mcp",
			"ignored", workspaceHeader, "1")
		assertEmptyBody(t, postHTTPMCPWithWorkspace(
			t, httpServer.Client(), httpServer.URL+"/mcp", "ignored",
			workspaceHeader, session, ProtocolVersion,
			`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
		response := postHTTPMCPWithWorkspace(
			t, httpServer.Client(), httpServer.URL+"/mcp", "ignored",
			workspaceHeader, session, ProtocolVersion,
			`{"jsonrpc":"2.0","id":"ask","method":"tools/call",`+
				`"params":{"name":"shoal.ask","arguments":{`+
				`"question":"Durable evidence","top_k":24}}}`)
		decoded := decodeHTTPResponse(t, response)
		result := decodeToolResult(t, decoded)
		if failOperation != "" {
			if decoded.Error != nil || !result.IsError ||
				bytes.Contains(result.StructuredContent, []byte(chatAcceptanceAnswer)) {
				t.Fatalf("unrecorded MCP output was not a structured tool failure: %+v / %s", decoded.Error, result.StructuredContent)
			}
			assertChatRecordingFailure(t, sink, generator)
			return
		}
		if decoded.Error != nil || result.IsError {
			observation, observationErr := observeCitationEnvelope(
				capturedAsk.response)
			projection, projectionErr := capturedAsk.response.EvidenceProjection()
			_, canonicalErr := canonicalToolObservation(
				context.Background(), observation, lastDecision)
			t.Fatalf("MCP ask failed: %+v / %s; provider = %v; observation = %v; projection = %+v/%v; canonical = %v",
				decoded.Error, result.StructuredContent,
				capturedAsk.err, observationErr, projection, projectionErr,
				errors.Join(canonicalErr, capturedSink.lastError))
		}
		if err := json.Unmarshal(result.StructuredContent, &envelope); err != nil {
			t.Fatal(err)
		}
	} else {
		path := "/api/v1/ask"
		if surface == "stream" {
			path = "/api/v1/chat/stream"
		}
		request, err := http.NewRequest(http.MethodPost, httpServer.URL+path,
			strings.NewReader(`{"question":"Durable evidence","top_k":24}`))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set(webapi.WorkspaceIDHeader, workspaceHeader)
		response, err := httpServer.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		if failOperation != "" {
			if response.StatusCode == http.StatusOK ||
				bytes.Contains(body, []byte(chatAcceptanceAnswer)) ||
				bytes.Contains(body, []byte("event: complete")) ||
				strings.Contains(response.Header.Get("Content-Type"), "text/event-stream") {
				t.Fatalf("unrecorded %s output escaped: status=%d body=%s", surface, response.StatusCode, body)
			}
			assertChatRecordingFailure(t, sink, generator)
			return
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s returned %d: %s", surface, response.StatusCode, body)
		}
		if surface == "stream" {
			if response.Header.Get("Content-Type") != "text/event-stream" ||
				!bytes.HasPrefix(body, []byte("event: complete\n")) ||
				bytes.Count(body, []byte("event: ")) != 1 {
				t.Fatalf("expected a single durably finalized SSE event: %s", body)
			}
			_, data, found := bytes.Cut(body, []byte("\ndata: "))
			if !found {
				t.Fatalf("SSE completion omitted structured data: %s", body)
			}
			body = bytes.TrimSpace(data)
		}
		if err := json.Unmarshal(body, &envelope); err != nil {
			t.Fatal(err)
		}
	}
	if err := envelope.Validate(); err != nil {
		t.Fatal(err)
	}
	if !envelope.Finalized || !envelope.DurablyRecorded ||
		envelope.Verification != reasoning.VerificationVerified ||
		envelope.WorkspaceSettingsID != settings.SettingsID || envelope.WorkspaceSettingsRevision != settings.Revision {
		t.Fatalf("chat lost finalization or workspace binding: finalized=%v recorded=%v verification=%s settings=%s/%d",
			envelope.Finalized, envelope.DurablyRecorded, envelope.Verification,
			envelope.WorkspaceSettingsID, envelope.WorkspaceSettingsRevision)
	}
	if envelope.OutputVisibility == "" || envelope.OutputVisibility == "public" ||
		envelope.OutputVisibility != interaction.Expression(envelope.EffectiveVisibility) {
		t.Fatalf("chat lost settings-defined output restriction: %q", envelope.OutputVisibility)
	}
	citedDocuments := make(map[shoal.ID]bool)
	for _, evidence := range envelope.Evidence {
		if evidence.Citation != nil && evidence.Use == reasoning.EvidenceCited {
			if evidence.Status != reasoning.VerificationVerified || evidence.SourceURI == "" {
				t.Fatalf("citation lost verification or source link: %+v", evidence)
			}
			citedDocuments[evidence.Citation.DocumentID] = true
		}
	}
	if len(citedDocuments) != documentCount {
		t.Fatalf("cited document set was truncated: got %d, want %d", len(citedDocuments), documentCount)
	}

	readDecision := workspaceHTTPDecision(t, "ask-read")
	readContext, err := authority.Binder().Bind(
		context.Background(), readDecision)
	if err != nil {
		t.Fatal(err)
	}
	records, err := client.InteractionRecords(readContext)
	if err != nil {
		t.Fatal(err)
	}
	operations := make(map[interaction.Operation]int)
	var outcome interaction.Session
	var chatSession interaction.Session
	for _, record := range records {
		operations[record.Session.Operation]++
		if record.Session.Operation == interaction.OperationChat {
			chatSession = record.Session
		}
		if record.Session.Operation == interaction.OperationToolCall &&
			record.Session.StopReason == "succeeded" &&
			len(record.Session.Turns) == 1 &&
			record.Session.Turns[0].ToolCall != nil &&
			record.Session.Turns[0].ToolCall.Kind == ToolAsk {
			outcome = record.Session
		}
	}
	if operations[interaction.OperationRetrieval] == 0 ||
		operations[interaction.OperationChat] != 1 ||
		operations[interaction.OperationInference] != 1 ||
		(surface == "mcp" && outcome.ID == "") {
		t.Fatalf("recorded MCP/chat operations = %+v", operations)
	}
	if chatSession.ID != envelope.SessionID ||
		chatSession.AuthorizationFingerprint != envelope.AuthorizationFingerprint ||
		chatSession.Actor.SubjectID != "workspace-user" ||
		!reflect.DeepEqual(chatSession.CitedNodeIDs, envelope.CitedSourceIDs) {
		t.Fatalf("durable chat lost response identity, authority, or citations: %+v", chatSession)
	}
	if interaction.Expression(chatSession.RequiredVisibility) != envelope.OutputVisibility {
		t.Fatalf("durable chat lost the settings-defined output label: %q != %q",
			interaction.Expression(chatSession.RequiredVisibility), envelope.OutputVisibility)
	}
	assertWorkspaceChatProvenance(t, httpServer, workspaceHeader, envelope)
	if surface != "mcp" {
		return
	}
	if len(outcome.Turns[0].ToolCall.RetrievedEvidence) == 0 ||
		len(outcome.CitedNodeIDs) != len(envelope.CitedSourceIDs) ||
		len(outcome.TouchedNodeIDs()) != len(envelope.RetrievedSourceIDs) ||
		len(outcome.TouchedEdgeIDs()) == 0 {
		t.Fatalf("MCP ask evidence was not preserved: %+v", outcome)
	}
}

func TestRetrievalToolRecordsIndependentCompleteEvidence(t *testing.T) {
	now := time.Now().UTC()
	citation := document.Citation{
		DocumentID: "document", RevisionID: "revision",
		SectionID: "section", SpanID: "span",
		Range: document.SourceRange{
			Start: document.SourcePosition{Offset: 1},
			End:   document.SourcePosition{Offset: 5},
		},
	}

	service := &stubService{
		retrieve: func(
			context.Context, webapi.RetrievalRequest,
		) (webapi.RetrievalResponse, error) {
			return webapi.RetrievalResponse{
				Snapshot: webapi.Snapshot{ID: "snapshot", AsOf: now},
				Retrieval: retrieval.Response{Results: []retrieval.Result{{
					ID: "span",
					Evidence: []retrieval.Evidence{{
						Citation: citation,
						Path: graph.Path{
							Nodes: []graph.Node{
								{ID: "document"}, {ID: "section"}, {ID: "span"},
							},
							Edges: []graph.Edge{
								{ID: "document-section", From: "document", To: "section", Type: "contains"},
								{ID: "section-span", From: "section", To: "span", Type: "contains"},
							},
						},
					}},
				}}},
			}, nil
		},
	}
	sink := &testInteractionSink{}
	recorder, err := interaction.NewRecorder(context.Background(), sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.SetClock(func() time.Time {
		return now.Add(time.Second)
	}); err != nil {
		t.Fatal(err)
	}
	server, authority := newTestServer(t, service, nil)
	server.recorder = recorder
	server.interactionNow = func() time.Time { return now.Add(time.Second) }
	server.snapshots = testSnapshotProvider{snapshot: explorer.Snapshot{
		ID: "snapshot", AsOf: now,
	}}
	makeReady(t, server)
	response := callToolRequest(
		t, server, ToolRetrieve,
		`{"query":{"text":"evidence","top_k":1,"modes":["lexical"]}}`)
	if result := decodeToolResult(t, response); result.IsError {
		t.Fatalf("retrieval failed: %s", result.StructuredContent)
	}
	if len(sink.sessions) != 2 {
		t.Fatalf("recorded sessions = %d, want 2", len(sink.sessions))
	}
	var retrievalSession, toolSession interaction.Session
	for _, session := range sink.sessions {
		switch session.Operation {
		case interaction.OperationRetrieval:
			retrievalSession = session
		case interaction.OperationToolCall:
			toolSession = session
		}
	}
	if len(retrievalSession.SeedEvidence) != 2 ||
		len(retrievalSession.TouchedEdgeIDs()) != 2 ||
		retrievalSession.AuthorizationOperation != string(auth.OperationRetrieve) {
		t.Fatalf("independent retrieval record = %+v", retrievalSession)
	}
	if len(toolSession.Turns) != 1 ||
		len(toolSession.Turns[0].ToolCall.RetrievedEvidence) != 2 ||
		len(toolSession.TouchedEdgeIDs()) != 2 {
		t.Fatalf("generic tool record = %+v", toolSession)
	}
	_ = authority
}

func TestHTTPToolWithholdsContentAfterPolicyOnlyRevocation(t *testing.T) {
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = corpus.Close() })
	if err := corpus.EnsureInteractionSink(context.Background()); err != nil {
		t.Fatal(err)
	}
	authority := auth.NewAuthority()
	store := authorized.NewMemoryPolicyStore()
	generations := httpGenerationReader{
		domain: []byte("domain"), generation: 1,
	}
	firstSelector, err := authorized.NewStaticPolicySelector(
		[]byte("source"), []byte("policy"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := authorized.NewClient(authorized.Config{
		Base: corpus, InteractionWriter: corpus, InteractionReader: corpus,
		SnapshotValidator: corpus,
		Resolver:          authority.Resolver(), PolicySelector: firstSelector,
		PolicyStore: store, GenerationReader: generations, Clock: time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	secondSelector, err := authorized.NewStaticPolicySelector(
		[]byte("other-source"), []byte("other-policy"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := authorized.NewClient(authorized.Config{
		Base: corpus, InteractionWriter: corpus, InteractionReader: corpus,
		SnapshotValidator: corpus,
		Resolver:          authority.Resolver(), PolicySelector: secondSelector,
		PolicyStore: store, GenerationReader: generations, Clock: time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	alice := httpScopedDecision(
		t, "alice", "policy-read", time.Now().Add(time.Hour),
		[]byte("source"), []byte("policy"))
	aliceContext, err := authority.Binder().Bind(context.Background(), alice)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := first.Ingest(aliceContext, explorer.Source{
		URI:       "file:///revoked-after-read.md",
		MediaType: explorer.MediaTypeMarkdown,
		Content:   "# Secret\n\nThis result must not be delivered.\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	bob := httpScopedDecision(
		t, "bob", "policy-revoke", time.Now().Add(time.Hour),
		[]byte("other-source"), []byte("other-policy"))
	bobContext, err := authority.Binder().Bind(context.Background(), bob)
	if err != nil {
		t.Fatal(err)
	}
	provider := &revokingObservedTool{
		sourceID: receipt.Document.ID,
		revoke: func() error {
			_, replaceErr := second.Ingest(bobContext, explorer.Source{
				URI:       "file:///revoked-after-read.md",
				MediaType: explorer.MediaTypeMarkdown,
				Content:   "# Replaced\n\nThe old policy no longer applies.\n",
			})
			return replaceErr
		},
	}
	service, err := webapi.NewEmbeddedService(first)
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(Config{
		Service: service, Authority: authority,
		Decisions:       DecisionProviderFunc(authority.Resolver().Resolve),
		InteractionSink: first, Snapshots: first,
		OptionalTools:    []OptionalToolProvider{provider},
		requestIDFactory: sequentialRequestIDs(),
	})
	if err != nil {
		t.Fatal(err)
	}
	authenticator := webapi.AuthenticatorFunc(func(
		*http.Request,
	) (auth.Decision, error) {
		return httpScopedDecision(
			t, "alice", "policy-http", time.Now().Add(time.Hour),
			[]byte("source"), []byte("policy")), nil
	})
	httpServer := httptest.NewUnstartedServer(nil)
	transport, err := NewHTTPHandler(HTTPConfig{
		Server: server,
		AllowedOrigins: []string{
			"http://" + httpServer.Listener.Addr().String(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	outer, err := webapi.NewAuthenticatedHandler(
		service, authenticator, authority.Binder(),
		httpServer.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if err := outer.MountAuthenticated("/mcp", transport); err != nil {
		t.Fatal(err)
	}
	httpServer.Config.Handler = outer
	httpServer.Start()
	t.Cleanup(httpServer.Close)

	session, _ := initializeHTTP(
		t, httpServer.Client(), httpServer.URL+"/mcp", "ignored", "1")
	assertEmptyBody(t, postHTTPMCP(
		t, httpServer.Client(), httpServer.URL+"/mcp", "ignored",
		session, ProtocolVersion,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	response := postHTTPMCP(
		t, httpServer.Client(), httpServer.URL+"/mcp", "ignored",
		session, ProtocolVersion,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call",`+
			`"params":{"name":"test.revoking_read","arguments":{}}}`)
	body := decodeHTTPResponse(t, response)
	result := decodeToolResult(t, body)
	if !result.IsError {
		t.Fatalf("revoked result was delivered: %s",
			result.StructuredContent)
	}
	if bytes.Contains(result.StructuredContent, []byte("must-not-ship")) {
		t.Fatalf("revoked content leaked: %s", result.StructuredContent)
	}
}
