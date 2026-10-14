/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements. See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership. The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License. You may obtain a copy of the License at
 *
 *     https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package webapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/model"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type countedChatGenerator struct {
	calls atomic.Int32
	model.FakeGenerator
}

type revokingInteractionExplorer struct {
	*explorer.Explorer
	chatCaptured atomic.Bool
}

func (e *revokingInteractionExplorer) RecordInteractionResult(
	ctx context.Context, session interaction.Session,
) (interaction.Session, error) {
	recorded, err := e.Explorer.RecordInteractionResult(ctx, session)
	if err == nil && session.Operation == interaction.OperationChat {
		e.chatCaptured.Store(true)
	}
	return recorded, err
}

type postCaptureResolver struct {
	delegate auth.Resolver
	revoked  auth.Decision
	captured *atomic.Bool
}

func (r postCaptureResolver) Resolve(ctx context.Context) (auth.Decision, error) {
	if r.captured.Load() {
		return r.revoked, nil
	}
	return r.delegate.Resolve(ctx)
}

func (g *countedChatGenerator) Generate(ctx context.Context, request model.GenerateRequest) (model.GenerateResult, error) {
	g.calls.Add(1)
	return g.FakeGenerator.Generate(ctx, request)
}

func TestChatServiceRecordsVerifiedAuthorizedResponse(t *testing.T) {
	service, client, corpus, authority, generator := chatServiceFixture(t)
	ctx, err := authority.Binder().Bind(context.Background(), chatPrincipal(t))
	if err != nil {
		t.Fatal(err)
	}
	response, err := service.Ask(ctx, webapi.AskRequest{Question: "What does the guide say about durable evidence?"})
	if err != nil {
		t.Fatal(err)
	}
	if err := response.Validate(); err != nil {
		t.Fatal(err)
	}
	if generator.calls.Load() == 0 || len(response.RetrievedSourceIDs) == 0 {
		t.Fatal("chat did not invoke the model on authorized evidence")
	}
	session, err := client.Interaction(ctx, response.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if session.Operation != interaction.OperationChat || session.Actor.SubjectID != "granted" {
		t.Fatalf("persisted chat identity = %+v", session)
	}
	summaries, err := client.Interactions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	operations := make(map[interaction.Operation]int)
	var retrievalSessions []interaction.Session
	for _, summary := range summaries {
		operations[summary.Operation]++
		if summary.Operation == interaction.OperationRetrieval {
			retrievalSession, readErr := client.Interaction(ctx, summary.SessionID)
			err = readErr
			if err != nil {
				t.Fatal(err)
			}
			retrievalSessions = append(retrievalSessions, retrievalSession)
		}
	}
	if operations[interaction.OperationRetrieval] != 2 ||
		operations[interaction.OperationInference] != 1 ||
		operations[interaction.OperationChat] != 1 {
		t.Fatalf("recorded operation set = %v", operations)
	}
	chatSources := make(map[shoal.ID]struct{}, len(session.TouchedNodeIDs()))
	for _, id := range session.TouchedNodeIDs() {
		chatSources[id] = struct{}{}
	}
	nonemptyRetrievals := 0
	for _, retrievalSession := range retrievalSessions {
		if len(retrievalSession.SeedNodeIDs) > 0 {
			nonemptyRetrievals++
		}
		for _, id := range retrievalSession.SeedNodeIDs {
			if _, ok := chatSources[id]; !ok {
				t.Fatalf("retrieval evidence %q was omitted from chat evidence", id)
			}
		}
	}
	if nonemptyRetrievals == 0 {
		t.Fatal("all independently recorded retrievals omitted their evidence sets")
	}
	before := generator.calls.Load()
	unvalidated, err := authority.Binder().Bind(context.Background(), authnPrincipal(t, "granted"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Ask(unvalidated, webapi.AskRequest{Question: "durable evidence"}); err == nil {
		t.Fatal("caller without evidence validation permission received chat")
	}
	if generator.calls.Load() != before {
		t.Fatal("caller without evidence validation permission reached the model")
	}
	denied, err := authority.Binder().Bind(context.Background(), authnPrincipal(t, "no-retrieve"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Ask(denied, webapi.AskRequest{Question: "durable evidence"}); err == nil {
		t.Fatal("caller without retrieval permission received chat")
	}
	if generator.calls.Load() != before {
		t.Fatal("denied caller reached the model")
	}
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Ask(ctx, webapi.AskRequest{Question: "durable evidence"}); err == nil {
		t.Fatal("unavailable recording sink returned chat success")
	}
	if generator.calls.Load() != before {
		t.Fatal("unavailable recording sink was discovered only after invoking model")
	}
}

func TestAuthenticatedHandlerUsesProductionChatAndProvenance(t *testing.T) {
	service, client, _, authority, generator := chatServiceFixture(t)
	embedded, err := webapi.NewEmbeddedService(client)
	if err != nil {
		t.Fatal(err)
	}

	handler, err := webapi.NewAuthenticatedHandler(
		embedded,
		webapi.AuthenticatorFunc(func(request *http.Request) (auth.Decision, error) {
			return chatPrincipalNamed(t, request.Header.Get("X-Test-Chat-Subject")), nil
		}),
		authority.Binder(),
		"example.test",
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := handler.SetChatProvider(service); err != nil {
		t.Fatal(err)
	}
	provenance, err := webapi.NewInteractionService(client)
	if err != nil {
		t.Fatal(err)
	}
	if err := handler.SetInteractionProvider(provenance); err != nil {
		t.Fatal(err)
	}

	ask := func(subject, path string) webapi.CitationEnvelope {
		t.Helper()
		body := []byte(`{
			"question":"What does the guide say about durable evidence?",
			"actor":"forged-actor",
			"reason":{"kind":"forged","detail":"forged"},
			"correlation_id":"forged-correlation",
			"operation":"tool_call"
		}`)
		request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		request.Host = "example.test"
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Test-Chat-Subject", subject)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s for %s returned %d: %s",
				path, subject, response.Code, response.Body.String())
		}
		if path == "/api/v1/chat/stream" {
			var envelope webapi.CitationEnvelope
			payload := response.Body.String()
			const marker = "\ndata: "
			index := bytes.Index([]byte(payload), []byte(marker))
			if !strings.HasPrefix(payload, "event: complete\n") || index < 0 {
				t.Fatalf("stream did not contain a final complete event: %q", payload)
			}
			line := payload[index+len(marker):]
			for index, value := range []byte(line) {
				if value == '\n' {
					line = line[:index]
					break
				}
			}
			if err := json.Unmarshal([]byte(line), &envelope); err != nil {
				t.Fatalf("decode stream completion: %v", err)
			}
			return envelope
		}
		var envelope webapi.CitationEnvelope
		if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("decode ask response: %v", err)
		}
		return envelope
	}

	first := ask("first", "/api/v1/ask")
	second := ask("second", "/api/v1/chat/stream")
	for subject, envelope := range map[string]webapi.CitationEnvelope{
		"first": first, "second": second,
	} {
		if !envelope.Finalized || !envelope.DurablyRecorded ||
			envelope.Verification != "verified" {
			t.Fatalf("%s response was not finalized: %+v", subject, envelope)
		}
		session, err := client.Interaction(
			mustBindChat(t, authority, chatPrincipalNamed(t, subject)),
			envelope.SessionID,
		)
		if err != nil {
			t.Fatal(err)
		}
		if session.Actor.SubjectID != shoal.ID(subject) ||
			session.Actor.ActorID != shoal.ID(subject+"-actor") ||
			session.Operation != interaction.OperationChat {
			t.Fatalf("%s persisted forged provenance: %+v", subject, session)
		}
	}
	if first.SessionID == second.SessionID || generator.calls.Load() < 2 {
		t.Fatalf("independent requests were conflated: first=%q second=%q calls=%d",
			first.SessionID, second.SessionID, generator.calls.Load())
	}

	request := httptest.NewRequest(http.MethodGet, "/api/v1/provenance", nil)
	request.Host = "example.test"
	request.Header.Set("X-Test-Chat-Subject", "first")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("provenance list returned %d: %s", response.Code, response.Body.String())
	}
	var listed webapi.ProvenanceListResponse
	if err := json.Unmarshal(response.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	foundFirst := false
	foundSecond := false
	for _, summary := range listed.Interactions {
		foundFirst = foundFirst || summary.SessionID == encodeTestID(first.SessionID)
		foundSecond = foundSecond || summary.SessionID == encodeTestID(second.SessionID)
	}
	if !foundFirst || !foundSecond {
		t.Fatalf("authorized provenance omitted recorded chats: %+v", listed.Interactions)
	}

	before := generator.calls.Load()
	deniedRequest := httptest.NewRequest(
		http.MethodPost, "/api/v1/ask",
		bytes.NewBufferString(`{"question":"durable evidence"}`),
	)
	deniedRequest.Host = "example.test"
	deniedRequest.Header.Set("Content-Type", "application/json")
	deniedRequest.Header.Set("X-Test-Chat-Subject", "denied")
	deniedResponse := httptest.NewRecorder()
	handler.ServeHTTP(deniedResponse, deniedRequest)
	if deniedResponse.Code == http.StatusOK ||
		bytes.Contains(deniedResponse.Body.Bytes(), []byte("durable evidence is recorded")) {
		t.Fatalf("denied request leaked content: %d %s",
			deniedResponse.Code, deniedResponse.Body.String())
	}
	if generator.calls.Load() != before {
		t.Fatal("denied HTTP request reached the model")
	}
}

func TestChatWithholdsContentWhenAuthorityChangesAfterDurableCapture(t *testing.T) {
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = corpus.Close() })
	base := &revokingInteractionExplorer{Explorer: corpus}
	authority := auth.NewAuthority()
	original := chatPrincipal(t)
	revoked, err := auth.NewDecision(auth.DecisionConfig{
		Subject: original.Subject(), Actor: original.Actor(),
		AuthorizationDomain:   original.AuthorizationDomain(),
		AllowedOperations:     []auth.Operation{auth.OperationValidate},
		PermittedSourceIDs:    original.PermittedSourceIDs(),
		PermittedPolicyIDs:    original.PermittedPolicyIDs(),
		PolicyGeneration:      original.PolicyGeneration(),
		AuthenticationExpires: original.AuthenticationExpires(),
		RequestID:             original.RequestID(),
	})
	if err != nil {
		t.Fatal(err)
	}
	resolver := postCaptureResolver{
		delegate: authority.Resolver(), revoked: revoked,
		captured: &base.chatCaptured,
	}
	selector, err := authorized.NewStaticPolicySelector(
		authnSourceGranted, authnPolicyGranted)
	if err != nil {
		t.Fatal(err)
	}
	client, err := authorized.NewClient(authorized.Config{
		Base: base, Resolver: resolver, PolicySelector: selector,
		InteractionWriter: base, InteractionReader: base,
		SnapshotValidator: base,
		PolicyStore:       authorized.NewMemoryPolicyStore(),
		GenerationReader:  authnGenerationReader{}, Clock: time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := authority.Binder().Bind(context.Background(), original)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Ingest(ctx, explorer.Source{
		URI:       "https://example.test/chat-guide.md",
		MediaType: explorer.MediaTypeMarkdown,
		Content:   "# Guide\n\nDurable evidence is recorded before delivery.\n",
	}); err != nil {
		t.Fatal(err)
	}
	provenance, err := inference.NewModelProvenance(
		"fake", "deterministic", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	service, err := webapi.NewChatService(context.Background(), webapi.ChatConfig{
		Client: client, Resolver: resolver,
		Generator: &countedChatGenerator{
			FakeGenerator: model.FakeGenerator{Model: "deterministic"},
		},
		Model: provenance,
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err := service.Ask(ctx, webapi.AskRequest{
		Question: "What does the guide say?",
	})
	if err == nil || !explorer.IsCommittedInteraction(err) {
		t.Fatalf("post-capture revocation error = %v", err)
	}
	if response.ID != "" || len(response.Evidence) != 0 ||
		len(response.Claims) != 0 || len(response.Sources) != 0 {
		t.Fatalf("post-capture revocation leaked response content: %+v", response)
	}
	summaries, err := corpus.Interactions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	foundChat := false
	for _, summary := range summaries {
		foundChat = foundChat || summary.Operation == interaction.OperationChat
	}
	if !foundChat {
		t.Fatal("chat was not durable before final authority rejection")
	}
}

func mustBindChat(
	t *testing.T, authority *auth.Authority, decision auth.Decision,
) context.Context {
	t.Helper()
	ctx, err := authority.Binder().Bind(context.Background(), decision)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

func chatServiceFixture(t *testing.T) (*webapi.ChatService, *authorized.Client, *explorer.Explorer, *auth.Authority, *countedChatGenerator) {
	t.Helper()
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = corpus.Close() })
	authority := auth.NewAuthority()
	selector, err := authorized.NewStaticPolicySelector(authnSourceGranted, authnPolicyGranted)
	if err != nil {
		t.Fatal(err)
	}
	client, err := authorized.NewClient(authorized.Config{
		Base: corpus, Resolver: authority.Resolver(), PolicySelector: selector,
		InteractionWriter: corpus, InteractionReader: corpus,
		SnapshotValidator: corpus,
		PolicyStore:       authorized.NewMemoryPolicyStore(),
		GenerationReader:  authnGenerationReader{}, Clock: time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := authority.Binder().Bind(context.Background(), chatPrincipal(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Ingest(ctx, explorer.Source{
		URI: "file:///chat-guide.md", MediaType: explorer.MediaTypeMarkdown,
		Content: "# Guide\n\nThe guide says durable evidence is recorded before a response is delivered.\n",
	}); err != nil {
		t.Fatal(err)
	}
	provenance, err := inference.NewModelProvenance("fake", "deterministic", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	generator := &countedChatGenerator{FakeGenerator: model.FakeGenerator{Model: "deterministic"}}
	service, err := webapi.NewChatService(context.Background(), webapi.ChatConfig{
		Client: client, Resolver: authority.Resolver(), Generator: generator, Model: provenance,
	})
	if err != nil {
		t.Fatal(err)
	}
	return service, client, corpus, authority, generator
}

func chatPrincipal(t *testing.T) auth.Decision {
	return chatPrincipalNamed(t, "granted")
}

func chatPrincipalNamed(t *testing.T, subject string) auth.Decision {
	t.Helper()
	if subject == "" {
		subject = "granted"
	}
	operations := append([]auth.Operation(nil), authnAllOperations...)
	if subject == "denied" {
		operations = []auth.Operation{auth.OperationRead, auth.OperationList}
	}
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: shoal.ID(subject), Actor: shoal.ID(subject + "-actor"),
		AuthorizationDomain:   authnDomain,
		AllowedOperations:     append(operations, auth.OperationValidate),
		PermittedSourceIDs:    [][]byte{authnSourceGranted},
		PermittedPolicyIDs:    [][]byte{authnPolicyGranted},
		PolicyGeneration:      1,
		AuthenticationExpires: time.Now().Add(time.Hour),
		RequestID:             shoal.ID(subject + "-request"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return decision
}
