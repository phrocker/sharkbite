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
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestServeNegotiatesVersionAndEnforcesInitializationOrdering(t *testing.T) {
	server, _ := newTestServer(t, &stubService{}, nil)
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":1e2,"method":"initialize","params":{"protocolVersion":"2099-01-01","capabilities":{},"clientInfo":{"name":"test","title":"Test Client","version":"1","description":"test","icons":[{"src":"data:image/png;base64,AA==","mimeType":"image/png","sizes":["16x16"]}],"websiteUrl":"https://example.test"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":"after","method":"tools/list"}`,
	}, "\n") + "\n"
	var output bytes.Buffer
	if err := server.Serve(context.Background(), strings.NewReader(input), &output); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	lines := nonemptyLines(output.String())
	if len(lines) != 4 {
		t.Fatalf("responses = %d, want 4:\n%s", len(lines), output.String())
	}
	var before Response
	mustUnmarshal(t, lines[0], &before)
	if before.Error == nil || before.Error.Code != codeInvalidRequest {
		t.Fatalf("pre-initialize response = %+v", before)
	}
	if !strings.Contains(lines[1], `"id":1e2`) {
		t.Fatalf("initialize did not preserve raw numeric ID: %s", lines[1])
	}
	var initialized Response
	mustUnmarshal(t, lines[1], &initialized)
	if initialized.Error != nil {
		t.Fatalf("initialize error = %+v", initialized.Error)
	}
	var result InitializeResult
	mustUnmarshal(t, string(initialized.Result), &result)
	if result.ProtocolVersion != ProtocolVersion {
		t.Fatalf("negotiated version = %q, want %q", result.ProtocolVersion, ProtocolVersion)
	}
	if result.Capabilities.Tools == nil || result.Capabilities.Tools.ListChanged {
		t.Fatalf("tools capability = %+v", result.Capabilities.Tools)
	}
	if !strings.Contains(result.Instructions, "durably recorded") ||
		strings.Contains(result.Instructions, "recording is deferred") {
		t.Fatalf("instructions do not describe active recording: %q", result.Instructions)
	}
	var awaitingNotification Response
	mustUnmarshal(t, lines[2], &awaitingNotification)
	if awaitingNotification.Error == nil ||
		awaitingNotification.Error.Code != codeInvalidRequest {
		t.Fatalf("pre-notification response = %+v", awaitingNotification)
	}
	var after Response
	mustUnmarshal(t, lines[3], &after)
	if after.Error != nil {
		t.Fatalf("post-initialization tools/list error = %+v", after.Error)
	}
}

func TestInitializeNegotiatesOnlySupportedHandshakeVersion(t *testing.T) {
	if ProtocolVersion != "2025-11-25" {
		t.Fatalf("protocol version = %q", ProtocolVersion)
	}
	for _, requested := range []string{
		ProtocolVersion,
		"2024-11-05",
		"2026-07-28",
		"2099-01-01",
	} {
		server, _ := newTestServer(t, &stubService{}, nil)
		response := server.handle(context.Background(), Request{
			JSONRPC: jsonRPCVersion,
			ID:      json.RawMessage("1"),
			Method:  "initialize",
			Params: json.RawMessage(
				`{"protocolVersion":"` + requested +
					`","capabilities":{},"clientInfo":{"name":"test","version":"1"}}`,
			),
		})
		if response == nil || response.Error != nil {
			t.Fatalf("initialize(%q) = %+v", requested, response)
		}
		var result InitializeResult
		mustUnmarshal(t, string(response.Result), &result)
		if result.ProtocolVersion != ProtocolVersion {
			t.Fatalf(
				"initialize(%q) negotiated %q, want %q",
				requested, result.ProtocolVersion, ProtocolVersion,
			)
		}
	}
}

func TestInvalidInitializeDoesNotAdvanceState(t *testing.T) {
	server, _ := newTestServer(t, &stubService{}, nil)
	invalid := server.handle(context.Background(), Request{
		JSONRPC: jsonRPCVersion,
		ID:      json.RawMessage("1"),
		Method:  "initialize",
		Params:  json.RawMessage(`{"protocolVersion":"2025-11-25"}`),
	})
	if invalid == nil || invalid.Error == nil ||
		invalid.Error.Code != codeInvalidParams {
		t.Fatalf("invalid initialize = %+v", invalid)
	}
	before := server.handle(context.Background(), Request{
		JSONRPC: jsonRPCVersion,
		ID:      json.RawMessage("2"),
		Method:  "tools/list",
	})
	if before == nil || before.Error == nil ||
		before.Error.Code != codeInvalidRequest {
		t.Fatalf("state advanced after invalid initialize: %+v", before)
	}
	makeReady(t, server)
}

func TestInitializedNotificationIsSilentAndCannotRunEarly(t *testing.T) {
	server, _ := newTestServer(t, &stubService{}, nil)
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
	}, "\n") + "\n"
	var output bytes.Buffer
	if err := server.Serve(context.Background(), strings.NewReader(input), &output); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	lines := nonemptyLines(output.String())
	if len(lines) != 1 {
		t.Fatalf("responses = %d, want 1: %s", len(lines), output.String())
	}
	var response Response
	mustUnmarshal(t, lines[0], &response)
	if response.Error == nil || response.Error.Code != codeInvalidRequest {
		t.Fatalf("response = %+v", response)
	}
}

func TestExplicitNullIDIsInvalidAndReadableErrorIDsArePreserved(t *testing.T) {
	server, _ := newTestServer(t, &stubService{}, nil)
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":null,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":"bad-\u0031","method":7}`,
	}, "\n") + "\n"
	var output bytes.Buffer
	if err := server.Serve(
		context.Background(), strings.NewReader(input), &output,
	); err != nil {
		t.Fatal(err)
	}
	lines := nonemptyLines(output.String())
	if len(lines) != 2 {
		t.Fatalf("responses = %d, want 2: %s", len(lines), output.String())
	}
	if !strings.Contains(lines[0], `"id":null`) {
		t.Fatalf("null-ID error did not carry null ID: %s", lines[0])
	}
	var nullID Response
	mustUnmarshal(t, lines[0], &nullID)
	if nullID.Error == nil ||
		nullID.Error.Code != codeInvalidRequest ||
		nullID.Error.Message != "invalid request ID" {
		t.Fatalf("null-ID response = %+v", nullID)
	}
	if !strings.Contains(lines[1], `"id":"bad-\u0031"`) {
		t.Fatalf("readable raw ID was not preserved: %s", lines[1])
	}
	var invalidShape Response
	mustUnmarshal(t, lines[1], &invalidShape)
	if invalidShape.Error == nil ||
		invalidShape.Error.Code != codeInvalidRequest {
		t.Fatalf("invalid-shape response = %+v", invalidShape)
	}
}

func TestPingIsAvailableThroughoutHandshake(t *testing.T) {
	server, _ := newTestServer(t, &stubService{}, nil)
	ping := func(id string) {
		response := server.handle(context.Background(), Request{
			JSONRPC: jsonRPCVersion, ID: json.RawMessage(id), Method: "ping",
		})
		if response == nil || response.Error != nil || string(response.Result) != "{}" {
			t.Fatalf("ping response = %+v", response)
		}
	}
	ping("1")
	makeReady(t, server)
	ping("2")

	repeated := server.handle(context.Background(), Request{
		JSONRPC: jsonRPCVersion, ID: json.RawMessage("3"),
		Method: "initialize",
		Params: json.RawMessage(
			`{"protocolVersion":"` + ProtocolVersion +
				`","capabilities":{},"clientInfo":{"name":"test","version":"1"}}`),
	})
	if repeated == nil || repeated.Error == nil ||
		repeated.Error.Code != codeInvalidRequest {
		t.Fatalf("repeated initialize = %+v", repeated)
	}
}

func TestToolAdvertisementTracksOptionalProviders(t *testing.T) {
	base := &stubService{}
	server, _ := newTestServer(t, base, nil)
	makeReady(t, server)
	names := listedToolNames(t, server)
	assertStrings(t, names, []string{
		ToolDocument,
		ToolDocuments,
		ToolNeighborhood,
		ToolPath,
		ToolRetrieve,
	})

	optional := optionalProvider{
		tool: Tool{
			Name: "shoal.compress_context", Description: "Compress context.",
			InputSchema: json.RawMessage(
				`{"type":"object","additionalProperties":false}`),
		},
	}
	server, _ = newTestServer(t, base, []OptionalToolProvider{optional})
	makeReady(t, server)
	names = listedToolNames(t, server)
	assertStrings(t, names, []string{
		"shoal.compress_context",
		ToolDocument,
		ToolDocuments,
		ToolNeighborhood,
		ToolPath,
		ToolRetrieve,
	})
	for _, absent := range []string{ToolIngest, ToolExtract, ToolRecompute} {
		if containsString(names, absent) {
			t.Fatalf("unimplemented optional tool %q was advertised", absent)
		}
	}
}

func TestOptionalToolsRespectRuntimeCapabilities(t *testing.T) {
	service := &limitedOptionalService{allOptionalService: &allOptionalService{
		stubService: &stubService{}, calls: make(map[string]int),
	}}
	server, _ := newTestServer(t, service, nil)
	makeReady(t, server)
	names := listedToolNames(t, server)
	for _, name := range []string{ToolIngest, ToolExtract, ToolChanges} {
		if containsString(names, name) {
			t.Fatalf("unavailable optional tool %q was advertised: %v", name, names)
		}
	}
	if !containsString(names, ToolRecompute) {
		t.Fatalf("available recompute tool was omitted: %v", names)
	}
}

func TestChangeToolRequiresServiceChangeProvider(t *testing.T) {
	server, _ := newTestServer(t, &stubService{}, nil)
	makeReady(t, server)
	names := listedToolNames(t, server)
	if containsString(names, ToolChanges) {
		t.Fatalf("change tool advertised for service without ChangeProvider")
	}
	response := callToolRequest(t, server, ToolChanges, `{}`)
	if response.Error == nil || response.Error.Code != codeInvalidParams {
		t.Fatalf("absent change tool response = %+v", response)
	}

	server, _ = newTestServer(
		t, &changesService{stubService: &stubService{}}, nil)
	makeReady(t, server)
	names = listedToolNames(t, server)
	if !containsString(names, ToolChanges) {
		t.Fatalf("change tool omitted for service implementing ChangeProvider")
	}
	response = callToolRequest(t, server, ToolChanges, `{}`)
	if response.Error != nil {
		t.Fatalf("change tool protocol error = %+v", response.Error)
	}
	result := decodeToolResult(t, response)
	if result.IsError {
		t.Fatalf("change tool error = %s", result.StructuredContent)
	}
	if !strings.Contains(string(result.StructuredContent), `"next_cursor":"next"`) {
		t.Fatalf("change result = %s", result.StructuredContent)
	}

	if _, ok := any(&webapi.RemoteService{}).(webapi.ChangeProvider); ok {
		t.Fatal("RemoteService unexpectedly implements ChangeProvider")
	}
}

func TestChangeToolForwardsOnlyPublicationCursor(t *testing.T) {
	var received webapi.ChangesRequest
	server, _ := newTestServer(t, &changesService{
		stubService: &stubService{},
		changes: func(
			_ context.Context,
			request webapi.ChangesRequest,
		) (webapi.ChangesResponse, error) {
			received = request
			return webapi.ChangesResponse{NextCursor: "next"}, nil
		},
	}, nil)
	makeReady(t, server)

	response := callToolRequest(
		t, server, ToolChanges, `{"cursor":"sealed-publication-cursor","limit":7}`)
	if response.Error != nil || decodeToolResult(t, response).IsError {
		t.Fatalf("change call failed: %+v", response)
	}
	if received.Cursor != "sealed-publication-cursor" || received.Limit != 7 {
		t.Fatalf("change request = %+v", received)
	}
	response = callToolRequest(
		t, server, ToolChanges,
		`{"cursor":"sealed-publication-cursor","frontier":"9"}`,
	)
	if response.Error != nil {
		t.Fatalf("invalid arguments became a protocol error: %+v", response.Error)
	}
	if result := decodeToolResult(t, response); !result.IsError {
		t.Fatalf("snapshot frontier was accepted as a change cursor: %+v", result)
	}
}

func TestMandatoryToolsDispatchToServiceReadPaths(t *testing.T) {
	called := make(map[string]int)
	service := &stubService{
		documents: func(
			context.Context,
			webapi.DocumentsRequest,
		) (webapi.DocumentsResponse, error) {
			called[ToolDocuments]++
			return webapi.DocumentsResponse{}, nil
		},
		document: func(
			context.Context,
			webapi.DocumentRequest,
		) (webapi.DocumentResponse, error) {
			called[ToolDocument]++
			return webapi.DocumentResponse{}, nil
		},
		retrieve: func(
			context.Context,
			webapi.RetrievalRequest,
		) (webapi.RetrievalResponse, error) {
			called[ToolRetrieve]++
			return webapi.RetrievalResponse{}, nil
		},
		neighborhood: func(
			context.Context,
			webapi.NeighborhoodRequest,
		) (webapi.NeighborhoodResponse, error) {
			called[ToolNeighborhood]++
			return webapi.NeighborhoodResponse{}, nil
		},
		path: func(
			context.Context,
			webapi.PathRequest,
		) (webapi.PathResponse, error) {
			called[ToolPath]++
			return webapi.PathResponse{}, nil
		},
	}
	server, _ := newTestServer(t, service, nil)
	makeReady(t, server)
	calls := []struct {
		name      string
		arguments string
	}{
		{ToolDocuments, `{}`},
		{ToolDocument, `{"document_id":"ZG9j"}`},
		{ToolRetrieve, `{"query":{"text":"query"}}`},
		{ToolNeighborhood, `{"node_ids":["bm9kZQ"]}`},
		{ToolPath, `{"from":"ZnJvbQ","to":"dG8"}`},
	}
	for _, call := range calls {
		response := callToolRequest(t, server, call.name, call.arguments)
		if response.Error != nil {
			t.Fatalf("%s protocol error = %+v", call.name, response.Error)
		}
		if result := decodeToolResult(t, response); result.IsError {
			t.Fatalf("%s tool error = %s", call.name, result.StructuredContent)
		}
	}
	for _, call := range calls {
		if called[call.name] != 1 {
			t.Fatalf("%s calls = %d, want 1", call.name, called[call.name])
		}
	}
}

func TestOptionalProviderAbsenceFailsClosed(t *testing.T) {
	calls := 0
	server, _ := newTestServer(t, &stubService{}, nil)
	server.decisions = DecisionProviderFunc(func(context.Context) (auth.Decision, error) {
		calls++
		return testDecision(t), nil
	})
	makeReady(t, server)
	names := listedToolNames(t, server)
	if containsString(names, ToolChanges) {
		t.Fatalf("change tool advertised without ChangeProvider")
	}
	response := callToolRequest(t, server, ToolChanges, `{}`)
	if response.Error == nil || response.Error.Code != codeInvalidParams {
		t.Fatalf("absent optional tool response = %+v", response)
	}
	if calls != 0 {
		t.Fatalf("decision calls = %d, want 0", calls)
	}
}

func TestToolResultsAreStructuredAndExecutionErrorsAreNotProtocolErrors(t *testing.T) {
	var documentsErr error
	service := &stubService{
		documents: func(
			context.Context,
			webapi.DocumentsRequest,
		) (webapi.DocumentsResponse, error) {
			if documentsErr != nil {
				return webapi.DocumentsResponse{}, documentsErr
			}
			return webapi.DocumentsResponse{
				Snapshot:  webapi.Snapshot{ID: "snapshot", Frontier: 7},
				Documents: nil,
			}, nil
		},
	}
	server, _ := newTestServer(t, service, nil)
	makeReady(t, server)

	response := callToolRequest(t, server, ToolDocuments, `{}`)
	if response.Error != nil {
		t.Fatalf("success protocol error = %+v", response.Error)
	}
	result := decodeToolResult(t, response)
	if result.IsError {
		t.Fatalf("success marked as error: %s", result.StructuredContent)
	}
	var success map[string]any
	mustUnmarshal(t, string(result.StructuredContent), &success)
	if success["snapshot"] == nil || len(result.Content) != 1 {
		t.Fatalf("structured success = %#v, content = %#v", success, result.Content)
	}

	documentsErr = shoal.NewError(shoal.ErrorNotFound, "document set not found")
	response = callToolRequest(t, server, ToolDocuments, `{}`)
	if response.Error != nil {
		t.Fatalf("tool failure became protocol error = %+v", response.Error)
	}
	result = decodeToolResult(t, response)
	if !result.IsError {
		t.Fatalf("tool failure not marked as error: %s", result.StructuredContent)
	}
	var failure structuredToolFailure
	mustUnmarshal(t, string(result.StructuredContent), &failure)
	if failure.Error.Code != string(shoal.ErrorNotFound) ||
		failure.Error.Message != "document set not found" {
		t.Fatalf("structured failure = %+v", failure)
	}

	documentsErr = errors.New("private backend detail")
	response = callToolRequest(t, server, ToolDocuments, `{}`)
	result = decodeToolResult(t, response)
	if strings.Contains(string(result.StructuredContent), "private backend detail") {
		t.Fatalf("unsanitized tool error: %s", result.StructuredContent)
	}
}

func TestEveryToolCallBindsFreshDecisionAndRequestID(t *testing.T) {
	authority := auth.NewAuthority()
	resolver := authority.Resolver()
	var decisions int
	var observed []shoal.ID
	selected := testOntologyIdentity(t)
	template := decisionWithExpiryAndOntology(
		t, time.Now().Add(time.Hour), selected)
	service := &stubService{
		documents: func(
			ctx context.Context,
			_ webapi.DocumentsRequest,
		) (webapi.DocumentsResponse, error) {
			decision, err := resolver.Resolve(ctx)
			if err != nil {
				return webapi.DocumentsResponse{}, err
			}
			observed = append(observed, decision.RequestID())
			if decision.Subject() != "subject" || decision.Actor() != "actor" {
				t.Fatalf("bound identity = %q/%q", decision.Subject(), decision.Actor())
			}
			if decision.PolicyGeneration() != 1 {
				t.Fatalf(
					"bound policy generation = %d", decision.PolicyGeneration())
			}
			ontologyIdentity, known := decision.SelectedOntology()
			if !known || ontologyIdentity != selected {
				t.Fatalf("bound ontology = %v, known = %v", ontologyIdentity, known)
			}
			return webapi.DocumentsResponse{}, nil
		},
	}
	server, err := newRecordedTestServer(t, Config{
		Service: service, Authority: authority,
		Decisions: DecisionProviderFunc(func(context.Context) (auth.Decision, error) {
			decisions++
			return template, nil
		}),
		requestIDFactory: sequentialRequestIDs(),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	makeReady(t, server)
	for range 2 {
		response := callToolRequest(t, server, ToolDocuments, `{}`)
		if response.Error != nil || decodeToolResult(t, response).IsError {
			t.Fatalf("tools/call failed: %+v", response)
		}
	}
	if decisions != 2 {
		t.Fatalf("decision resolutions = %d, want 2", decisions)
	}
	assertIDs(t, observed, []shoal.ID{"request-1", "request-2"})
	if observed[0] == template.RequestID() {
		t.Fatalf("template request ID was reused")
	}
}

func TestToolCallPreservesNoLensDefault(t *testing.T) {
	authority := auth.NewAuthority()
	resolver := authority.Resolver()
	template := testDecision(t)
	service := &stubService{
		documents: func(
			ctx context.Context,
			_ webapi.DocumentsRequest,
		) (webapi.DocumentsResponse, error) {
			decision, err := resolver.Resolve(ctx)
			if err != nil {
				return webapi.DocumentsResponse{}, err
			}
			if _, known := decision.SelectedOntology(); known {
				t.Fatal("tool call invented an ontology selection")
			}
			if decision.RequestID() == template.RequestID() {
				t.Fatal("tool call reused the template request ID")
			}
			return webapi.DocumentsResponse{}, nil
		},
	}
	server, err := newRecordedTestServer(t, Config{
		Service: service, Authority: authority,
		Decisions: DecisionProviderFunc(func(context.Context) (auth.Decision, error) {
			return template, nil
		}),
		requestIDFactory: sequentialRequestIDs(),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	makeReady(t, server)
	response := callToolRequest(t, server, ToolDocuments, `{}`)
	if response.Error != nil || decodeToolResult(t, response).IsError {
		t.Fatalf("tools/call failed: %+v", response)
	}
}

func TestToolArgumentValidationIsStructured(t *testing.T) {
	serviceCalls := 0
	service := &stubService{
		documents: func(
			context.Context,
			webapi.DocumentsRequest,
		) (webapi.DocumentsResponse, error) {
			serviceCalls++
			return webapi.DocumentsResponse{}, nil
		},
	}
	server, _ := newTestServer(t, service, nil)
	makeReady(t, server)
	response := callToolRequest(t, server, ToolDocuments, `{"unexpected":true}`)
	if response.Error != nil {
		t.Fatalf("argument error became protocol error = %+v", response.Error)
	}
	result := decodeToolResult(t, response)
	if !result.IsError ||
		!strings.Contains(string(result.StructuredContent), "invalid_argument") {
		t.Fatalf("argument result = %+v", result)
	}
	if serviceCalls != 0 {
		t.Fatalf("service called %d times for invalid arguments", serviceCalls)
	}

	params := json.RawMessage(`{"name":"shoal.documents","arguments":[]}`)
	responsePointer := server.handle(context.Background(), Request{
		JSONRPC: jsonRPCVersion, ID: json.RawMessage("9"),
		Method: "tools/call", Params: params,
	})
	if responsePointer == nil || responsePointer.Error == nil ||
		responsePointer.Error.Code != codeInvalidParams {
		t.Fatalf("malformed call response = %+v", responsePointer)
	}
}

func TestUnknownWrongAndOversizedToolArgumentsDoNotInvokeService(t *testing.T) {
	calls := make(map[string]int)
	base := &stubService{
		documents: func(
			context.Context,
			webapi.DocumentsRequest,
		) (webapi.DocumentsResponse, error) {
			calls[ToolDocuments]++
			return webapi.DocumentsResponse{}, nil
		},
		document: func(
			context.Context,
			webapi.DocumentRequest,
		) (webapi.DocumentResponse, error) {
			calls[ToolDocument]++
			return webapi.DocumentResponse{}, nil
		},
		retrieve: func(
			context.Context,
			webapi.RetrievalRequest,
		) (webapi.RetrievalResponse, error) {
			calls[ToolRetrieve]++
			return webapi.RetrievalResponse{}, nil
		},
		neighborhood: func(
			context.Context,
			webapi.NeighborhoodRequest,
		) (webapi.NeighborhoodResponse, error) {
			calls[ToolNeighborhood]++
			return webapi.NeighborhoodResponse{}, nil
		},
		path: func(
			context.Context,
			webapi.PathRequest,
		) (webapi.PathResponse, error) {
			calls[ToolPath]++
			return webapi.PathResponse{}, nil
		},
	}
	service := &allOptionalService{stubService: base, calls: calls}
	server, _ := newTestServer(t, service, nil)
	makeReady(t, server)

	unknown := callToolRequest(t, server, "shoal.unknown", `{}`)
	if unknown.Error == nil || unknown.Error.Code != codeInvalidParams {
		t.Fatalf("unknown tool response = %+v", unknown)
	}
	wrongShape := server.handle(context.Background(), Request{
		JSONRPC: jsonRPCVersion,
		ID:      json.RawMessage("9"),
		Method:  "tools/call",
		Params: json.RawMessage(
			`{"name":"shoal.documents","arguments":[]}`,
		),
	})
	if wrongShape == nil || wrongShape.Error == nil ||
		wrongShape.Error.Code != codeInvalidParams {
		t.Fatalf("wrong argument shape response = %+v", wrongShape)
	}

	oversizedID := base64.RawURLEncoding.EncodeToString(
		[]byte(strings.Repeat("x", shoal.MaxIDBytes+1)))
	largeContent := base64.StdEncoding.EncodeToString(
		[]byte(strings.Repeat("x", int(webapi.MaxUploadFileBytes)+1)))
	largeFilename := strings.Repeat("x", maxUploadFilenameRunes+1)
	cases := []struct {
		name      string
		arguments string
	}{
		{ToolDocuments, `{"page":{"limit":101}}`},
		{ToolDocument, `{"document_id":"` + oversizedID + `"}`},
		{ToolRetrieve, `{"query":{"text":"query","top_k":51}}`},
		{ToolNeighborhood, `{"node_ids":["bm9kZQ"],"max_nodes":251}`},
		{ToolPath, `{"from":"ZnJvbQ","to":"dG8","fanout":51}`},
		{ToolIngest, `{"files":[{"name":"large.txt","content":"` + largeContent + `"}]}`},
		{ToolIngest, `{"files":[{"name":"` + largeFilename + `","content":""}]}`},
		{ToolExtract, `{"document_id":"` + oversizedID + `"}`},
		{ToolRecompute, `{"assertion_id":"` + oversizedID + `"}`},
		{ToolChanges, `{"limit":101}`},
	}
	for _, test := range cases {
		response := callToolRequest(t, server, test.name, test.arguments)
		if response.Error != nil {
			t.Fatalf("%s validation became protocol error: %+v", test.name, response.Error)
		}
		if result := decodeToolResult(t, response); !result.IsError {
			t.Fatalf("%s oversized arguments were accepted: %+v", test.name, result)
		}
	}
	for name, count := range calls {
		if count != 0 {
			t.Fatalf("%s service calls = %d, want 0", name, count)
		}
	}
}

func TestAuthorizationFailuresDoNotInvokeService(t *testing.T) {
	tests := []struct {
		name      string
		decisions DecisionProvider
		requestID func() (shoal.ID, error)
		wantCode  shoal.ErrorCode
	}{
		{
			name: "provider failure",
			decisions: DecisionProviderFunc(func(context.Context) (auth.Decision, error) {
				return auth.Decision{}, errors.New("private identity detail")
			}),
			wantCode: shoal.ErrorUnauthorized,
		},
		{
			name: "expired decision",
			decisions: DecisionProviderFunc(func(context.Context) (auth.Decision, error) {
				return decisionWithExpiry(t, time.Now().Add(-time.Minute)), nil
			}),
			wantCode: shoal.ErrorUnauthorized,
		},
		{
			name: "request ID failure",
			decisions: DecisionProviderFunc(func(context.Context) (auth.Decision, error) {
				return testDecision(t), nil
			}),
			requestID: func() (shoal.ID, error) {
				return "", errors.New("entropy unavailable")
			},
			wantCode: shoal.ErrorUnavailable,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			serviceCalls := 0
			server, err := newRecordedTestServer(t, Config{
				Service: &stubService{
					documents: func(
						context.Context,
						webapi.DocumentsRequest,
					) (webapi.DocumentsResponse, error) {
						serviceCalls++
						return webapi.DocumentsResponse{}, nil
					},
				},
				Authority:        auth.NewAuthority(),
				Decisions:        test.decisions,
				requestIDFactory: test.requestID,
			})
			if err != nil {
				t.Fatal(err)
			}
			makeReady(t, server)
			result := decodeToolResult(
				t, callToolRequest(t, server, ToolDocuments, `{}`))
			if !result.IsError {
				t.Fatalf("authorization failure succeeded: %+v", result)
			}
			var failure structuredToolFailure
			mustUnmarshal(t, string(result.StructuredContent), &failure)
			if failure.Error.Code != string(test.wantCode) {
				t.Fatalf("failure = %+v, want %q", failure, test.wantCode)
			}
			if strings.Contains(
				string(result.StructuredContent), "private identity detail",
			) || strings.Contains(
				string(result.StructuredContent), "entropy unavailable",
			) {
				t.Fatalf("private error leaked: %s", result.StructuredContent)
			}
			if serviceCalls != 0 {
				t.Fatalf("service calls = %d, want 0", serviceCalls)
			}
		})
	}
}

func TestToolCallRateLimitRunsBeforeAuthorizationAndDispatch(t *testing.T) {
	now := time.Date(2026, 9, 5, 15, 0, 0, 0, time.UTC)
	decisionCalls := 0
	serviceCalls := 0
	server, err := newRecordedTestServer(t, Config{
		Service: &stubService{
			documents: func(
				context.Context,
				webapi.DocumentsRequest,
			) (webapi.DocumentsResponse, error) {
				serviceCalls++
				return webapi.DocumentsResponse{}, nil
			},
		},
		Authority: auth.NewAuthority(),
		Decisions: DecisionProviderFunc(func(context.Context) (auth.Decision, error) {
			decisionCalls++
			return testDecision(t), nil
		}),
		ToolCallsPerMinute: 1,
		requestIDFactory:   sequentialRequestIDs(),
		toolCallClock:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	makeReady(t, server)

	first := decodeToolResult(t, callToolRequest(t, server, ToolDocuments, `{}`))
	if first.IsError {
		t.Fatalf("first rate-limited call failed: %s", first.StructuredContent)
	}
	second := decodeToolResult(t, callToolRequest(t, server, ToolDocuments, `{}`))
	if !second.IsError ||
		!strings.Contains(string(second.StructuredContent), "rate limit exceeded") {
		t.Fatalf("second rate-limited call = %+v", second)
	}
	if decisionCalls != 1 || serviceCalls != 1 {
		t.Fatalf("calls after rejection: decisions=%d service=%d", decisionCalls, serviceCalls)
	}
	unknown := callToolRequest(t, server, "shoal.unknown", `{}`)
	if unknown.Error == nil || unknown.Error.Code != codeInvalidParams {
		t.Fatalf("rate-limited unknown tool response = %+v", unknown)
	}
	if decisionCalls != 1 || serviceCalls != 1 {
		t.Fatalf(
			"unknown tool reached authorization or dispatch: decisions=%d service=%d",
			decisionCalls, serviceCalls,
		)
	}
	now = now.Add(time.Minute)
	third := decodeToolResult(t, callToolRequest(t, server, ToolDocuments, `{}`))
	if third.IsError || decisionCalls != 2 || serviceCalls != 2 {
		t.Fatalf(
			"calls after reset: result=%+v decisions=%d service=%d",
			third, decisionCalls, serviceCalls,
		)
	}
}

func TestInitializeRejectsMalformedStandardClientCapabilities(t *testing.T) {
	server, _ := newTestServer(t, &stubService{}, nil)
	response := server.handle(context.Background(), Request{
		JSONRPC: jsonRPCVersion,
		ID:      json.RawMessage("1"),
		Method:  "initialize",
		Params: json.RawMessage(`{
			"protocolVersion":"2025-11-25",
			"capabilities":{"roots":5,"vendor.extension":true},
			"clientInfo":{"name":"test","version":"1"}
		}`),
	})
	if response == nil || response.Error == nil ||
		response.Error.Code != codeInvalidParams {
		t.Fatalf("malformed capabilities response = %+v", response)
	}
	response = server.handle(context.Background(), Request{
		JSONRPC: jsonRPCVersion,
		ID:      json.RawMessage("2"),
		Method:  "initialize",
		Params: json.RawMessage(`{
			"protocolVersion":"2025-11-25",
			"capabilities":{"roots":{},"vendor.extension":true},
			"clientInfo":{"name":"test","version":"1"}
		}`),
	})
	if response == nil || response.Error != nil {
		t.Fatalf("valid capabilities response = %+v", response)
	}
}

func TestToolResultCompressionRunsOnResponsePath(t *testing.T) {
	compressor := &recordingCompressor{}
	authority := auth.NewAuthority()
	service := &stubService{
		documents: func(
			context.Context,
			webapi.DocumentsRequest,
		) (webapi.DocumentsResponse, error) {
			return webapi.DocumentsResponse{
				Snapshot: webapi.Snapshot{ID: strings.Repeat("s", 64)},
			}, nil
		},
	}
	server, err := newRecordedTestServer(t, Config{
		Service: service, Authority: authority,
		Decisions: DecisionProviderFunc(func(context.Context) (auth.Decision, error) {
			return testDecision(t), nil
		}),
		ContextCompressor:  compressor,
		ContextBudgetBytes: 8,
		requestIDFactory:   sequentialRequestIDs(),
	})
	if err != nil {
		t.Fatal(err)
	}
	makeReady(t, server)

	response := callToolRequest(t, server, ToolDocuments, `{}`)
	result := decodeToolResult(t, response)
	if result.IsError {
		t.Fatalf("compressed success became an error: %s", result.StructuredContent)
	}
	if len(result.Content) != 0 {
		t.Fatalf("oversized compatibility mirror was not compressed: %+v", result.Content)
	}
	var structured webapi.DocumentsResponse
	if err := json.Unmarshal(result.StructuredContent, &structured); err != nil {
		t.Fatalf("decode structured result: %v", err)
	}
	if structured.Snapshot.ID != strings.Repeat("s", 64) {
		t.Fatalf("structured result was changed: %+v", structured)
	}
	if len(compressor.inputs) != 1 ||
		compressor.inputs[0].BudgetBytes != 8 ||
		len(compressor.inputs[0].Items) != 1 ||
		compressor.inputs[0].Items[0].IsError {
		t.Fatalf("compression input = %+v", compressor.inputs)
	}
}

func TestCompressionFailurePreservesStructuredSuccess(t *testing.T) {
	serviceCalls := 0
	server, err := newRecordedTestServer(t, Config{
		Service: &stubService{
			documents: func(
				context.Context,
				webapi.DocumentsRequest,
			) (webapi.DocumentsResponse, error) {
				serviceCalls++
				return webapi.DocumentsResponse{
					Snapshot: webapi.Snapshot{ID: "snapshot"},
				}, nil
			},
		},
		Authority: auth.NewAuthority(),
		Decisions: DecisionProviderFunc(func(context.Context) (auth.Decision, error) {
			return testDecision(t), nil
		}),
		ContextCompressor: failingCompressor{},
		requestIDFactory:  sequentialRequestIDs(),
	})
	if err != nil {
		t.Fatal(err)
	}
	makeReady(t, server)

	result := decodeToolResult(t, callToolRequest(t, server, ToolDocuments, `{}`))
	if result.IsError || len(result.Content) != 0 ||
		!strings.Contains(string(result.StructuredContent), `"id":"snapshot"`) {
		t.Fatalf("compression failure changed successful result: %+v", result)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"content":[]`) {
		t.Fatalf("compression fallback content is not an array: %s", encoded)
	}
	if serviceCalls != 1 {
		t.Fatalf("service calls = %d, want 1", serviceCalls)
	}
}

func TestCompressedRetrievalPreservesCitationsAndOpaqueIDs(t *testing.T) {
	resultID := shoal.ID(string([]byte{0xff, 0x00, 'r'}))
	documentID := shoal.ID(string([]byte{0xfe, 0x00, 'd'}))
	revisionID := shoal.ID(string([]byte{0xfd, 0x00, 'v'}))
	sectionID := shoal.ID(string([]byte{0xfc, 0x00, 's'}))
	service := &stubService{
		retrieve: func(
			context.Context,
			webapi.RetrievalRequest,
		) (webapi.RetrievalResponse, error) {
			return webapi.RetrievalResponse{
				Retrieval: retrieval.Response{
					RequestID: "retrieval-request",
					Results: []retrieval.Result{{
						ID: resultID,
						Evidence: []retrieval.Evidence{{
							Citation: document.Citation{
								DocumentID: documentID,
								RevisionID: revisionID,
								SectionID:  sectionID,
							},
						}},
					}},
				},
			}, nil
		},
	}
	authority := auth.NewAuthority()
	server, err := newRecordedTestServer(t, Config{
		Service: service, Authority: authority,
		Decisions: DecisionProviderFunc(func(context.Context) (auth.Decision, error) {
			return testDecision(t), nil
		}),
		ContextBudgetBytes: 8,
		requestIDFactory:   sequentialRequestIDs(),
	})
	if err != nil {
		t.Fatal(err)
	}
	makeReady(t, server)

	result := decodeToolResult(
		t,
		callToolRequest(
			t, server, ToolRetrieve, `{"query":{"text":"query"}}`,
		),
	)
	if result.IsError || len(result.Content) != 0 {
		t.Fatalf("compressed retrieval = %+v", result)
	}
	var decoded webapi.RetrievalResponse
	if err := json.Unmarshal(result.StructuredContent, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Retrieval.Results) != 1 ||
		decoded.Retrieval.Results[0].ID != resultID ||
		len(decoded.Retrieval.Results[0].Evidence) != 1 {
		t.Fatalf("retrieval provenance was changed: %+v", decoded.Retrieval)
	}
	citation := decoded.Retrieval.Results[0].Evidence[0].Citation
	if citation.DocumentID != documentID ||
		citation.RevisionID != revisionID ||
		citation.SectionID != sectionID {
		t.Fatalf("citation IDs were changed: %+v", citation)
	}
}

func TestToolErrorsBypassCompressionAndPreserveErrorSemantics(t *testing.T) {
	compressor := &recordingCompressor{}
	authority := auth.NewAuthority()
	server, err := newRecordedTestServer(t, Config{
		Service: &stubService{
			documents: func(
				context.Context,
				webapi.DocumentsRequest,
			) (webapi.DocumentsResponse, error) {
				return webapi.DocumentsResponse{}, shoal.NewError(
					shoal.ErrorNotFound, "not found")
			},
		},
		Authority: authority,
		Decisions: DecisionProviderFunc(func(context.Context) (auth.Decision, error) {
			return testDecision(t), nil
		}),
		ContextCompressor:  compressor,
		ContextBudgetBytes: 1,
		requestIDFactory:   sequentialRequestIDs(),
	})
	if err != nil {
		t.Fatal(err)
	}
	makeReady(t, server)

	result := decodeToolResult(t, callToolRequest(t, server, ToolDocuments, `{}`))
	if !result.IsError || len(result.Content) != 1 {
		t.Fatalf("tool error was compressed away: %+v", result)
	}
	if len(compressor.inputs) != 0 {
		t.Fatalf("tool error was sent through compression: %+v", compressor.inputs)
	}
}

func TestToolWireConversionsPreserveOpaqueIDsAndMetadata(t *testing.T) {
	documentID := shoal.ID(string([]byte{0xff, 0x00, 'd'}))
	revisionID := shoal.ID(string([]byte{0xfe, 0x00, 'r'}))
	rootID := shoal.ID(string([]byte{0xfd, 0x00, 's'}))
	metadataKey := string([]byte{0xfc, 0x00, 'k'})
	metadataValue := string([]byte{0xfb, 0x00, 'v'})
	var received shoal.ID
	service := &stubService{
		document: func(
			_ context.Context,
			request webapi.DocumentRequest,
		) (webapi.DocumentResponse, error) {
			received = request.DocumentID
			return webapi.DocumentResponse{}, nil
		},
		documents: func(
			context.Context,
			webapi.DocumentsRequest,
		) (webapi.DocumentsResponse, error) {
			return webapi.DocumentsResponse{
				Documents: []explorer.DocumentSummary{{
					Document: document.Document{
						ID: documentID, RevisionID: revisionID,
						RootSectionID: rootID,
						Metadata:      shoal.Metadata{metadataKey: metadataValue},
					},
					Revision: document.Revision{
						ID: revisionID, DocumentID: documentID,
					},
				}},
			}, nil
		},
	}
	server, _ := newTestServer(t, service, nil)
	makeReady(t, server)

	encodedDocumentID := base64.RawURLEncoding.EncodeToString([]byte(documentID))
	result := decodeToolResult(
		t,
		callToolRequest(
			t, server, ToolDocument,
			`{"snapshot":{},"document_id":"`+encodedDocumentID+`"}`,
		),
	)
	if result.IsError || received != documentID {
		t.Fatalf("opaque request ID = %q, result = %+v", received, result)
	}

	result = decodeToolResult(t, callToolRequest(t, server, ToolDocuments, `{}`))
	var wire struct {
		Documents []struct {
			Document struct {
				ID       string `json:"id"`
				Metadata []struct {
					Key   string `json:"key"`
					Value string `json:"value"`
				} `json:"metadata"`
			} `json:"document"`
		} `json:"documents"`
	}
	if err := json.Unmarshal(result.StructuredContent, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Documents) != 1 ||
		wire.Documents[0].Document.ID != encodedDocumentID ||
		len(wire.Documents[0].Document.Metadata) != 1 ||
		wire.Documents[0].Document.Metadata[0].Key !=
			base64.RawURLEncoding.EncodeToString([]byte(metadataKey)) ||
		wire.Documents[0].Document.Metadata[0].Value !=
			base64.RawURLEncoding.EncodeToString([]byte(metadataValue)) {
		t.Fatalf("opaque wire result = %+v", wire)
	}
}

func TestServeRejectsNilContextAndTypedNilStreams(t *testing.T) {
	server, _ := newTestServer(t, &stubService{}, nil)
	if err := server.Serve(nil, strings.NewReader(""), io.Discard); err == nil {
		t.Fatal("nil context was accepted")
	}
	var typedNilReader *bytes.Reader
	if err := server.Serve(
		context.Background(), typedNilReader, io.Discard,
	); err == nil {
		t.Fatal("typed-nil input was accepted")
	}
	var typedNilWriter *bytes.Buffer
	if err := server.Serve(
		context.Background(), strings.NewReader(""), typedNilWriter,
	); err == nil {
		t.Fatal("typed-nil output was accepted")
	}
}

func TestServeRejectsOversizedInputWithProtocolError(t *testing.T) {
	server, _ := newTestServer(t, &stubService{}, nil)
	var output bytes.Buffer
	err := server.Serve(
		context.Background(),
		strings.NewReader(strings.Repeat("x", maxMessageBytes+1)),
		&output,
	)
	if !errors.Is(err, errMessageTooLarge) {
		t.Fatalf("Serve error = %v", err)
	}
	lines := nonemptyLines(output.String())
	if len(lines) != 1 {
		t.Fatalf("oversize responses = %d: %q", len(lines), output.String())
	}
	var response Response
	mustUnmarshal(t, lines[0], &response)
	if response.Error == nil ||
		response.Error.Code != codeInvalidRequest ||
		response.Error.Message != "message exceeds maximum size" ||
		string(response.ID) != "null" {
		t.Fatalf("oversize response = %+v", response)
	}
}

func TestServeRespondsToInvalidIDLessRequest(t *testing.T) {
	server, _ := newTestServer(t, &stubService{}, nil)
	var output bytes.Buffer
	if err := server.Serve(
		context.Background(),
		strings.NewReader("{\"jsonrpc\":\"2.0\"}\n"),
		&output,
	); err != nil {
		t.Fatal(err)
	}
	lines := nonemptyLines(output.String())
	if len(lines) != 1 {
		t.Fatalf("invalid request responses = %d: %q", len(lines), output.String())
	}
	var response Response
	mustUnmarshal(t, lines[0], &response)
	if response.Error == nil ||
		response.Error.Code != codeInvalidRequest ||
		string(response.ID) != "null" {
		t.Fatalf("invalid request response = %+v", response)
	}
}

func TestNewServerRejectsInvalidExtensionConfiguration(t *testing.T) {
	authority := auth.NewAuthority()
	decisions := DecisionProviderFunc(func(context.Context) (auth.Decision, error) {
		return testDecision(t), nil
	})
	invalidSchema := optionalProvider{tool: Tool{
		Name: "invalid", Description: "invalid schema",
		InputSchema: json.RawMessage(`{"type":"array"}`),
	}}
	if _, err := newRecordedTestServer(t, Config{
		Service: &stubService{}, Authority: authority, Decisions: decisions,
		OptionalTools: []OptionalToolProvider{invalidSchema},
	}); err == nil {
		t.Fatal("non-object tool input schema was accepted")
	}
	invalidProperties := optionalProvider{tool: Tool{
		Name: "invalid-properties", Description: "invalid properties",
		InputSchema: json.RawMessage(
			`{"type":"object","properties":5}`),
	}}
	if _, err := newRecordedTestServer(t, Config{
		Service: &stubService{}, Authority: authority, Decisions: decisions,
		OptionalTools: []OptionalToolProvider{invalidProperties},
	}); err == nil {
		t.Fatal("malformed tool properties schema was accepted")
	}
	outputSchema := optionalProvider{tool: Tool{
		Name: "output", Description: "unsupported output schema",
		InputSchema:  json.RawMessage(`{"type":"object"}`),
		OutputSchema: json.RawMessage(`{"type":"object"}`),
	}}
	if _, err := newRecordedTestServer(t, Config{
		Service: &stubService{}, Authority: authority, Decisions: decisions,
		OptionalTools: []OptionalToolProvider{outputSchema},
	}); err == nil {
		t.Fatal("unsupported tool output schema was accepted")
	}
	for _, mode := range []string{"", "optional", "required", "unknown"} {
		taskTool := optionalProvider{tool: Tool{
			Name: "task", Description: "unsupported task mode",
			InputSchema: json.RawMessage(`{"type":"object"}`),
			Execution:   &ToolExecution{TaskSupport: mode},
		}}
		if _, err := newRecordedTestServer(t, Config{
			Service: &stubService{}, Authority: authority, Decisions: decisions,
			OptionalTools: []OptionalToolProvider{taskTool},
		}); err == nil {
			t.Fatalf("unsupported task mode %q was accepted", mode)
		}
	}
	synchronous := optionalProvider{tool: Tool{
		Name: "synchronous", Description: "explicit synchronous tool",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Execution:   &ToolExecution{TaskSupport: "forbidden"},
	}}
	if _, err := newRecordedTestServer(t, Config{
		Service: &stubService{}, Authority: authority, Decisions: decisions,
		OptionalTools: []OptionalToolProvider{synchronous},
	}); err != nil {
		t.Fatalf("explicit forbidden task mode was rejected: %v", err)
	}
	unscoped := unscopedOptionalProvider{tool: Tool{
		Name: "unscoped", Description: "missing authorization operation",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Annotations: readOnlyAnnotations(),
	}}
	if _, err := newRecordedTestServer(t, Config{
		Service: &stubService{}, Authority: authority, Decisions: decisions,
		OptionalTools: []OptionalToolProvider{unscoped},
	}); err == nil {
		t.Fatal("optional tool without authorization operation was accepted")
	}
	spoofedChanges := optionalProvider{tool: Tool{
		Name: ToolChanges, Description: "spoofed changes",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}}
	if _, err := newRecordedTestServer(t, Config{
		Service: &stubService{}, Authority: authority, Decisions: decisions,
		OptionalTools: []OptionalToolProvider{spoofedChanges},
	}); err == nil {
		t.Fatal("reserved change tool was accepted without ChangeProvider")
	}
	var typedNilCompressor *recordingCompressor
	if _, err := newRecordedTestServer(t, Config{
		Service: &stubService{}, Authority: authority, Decisions: decisions,
		ContextCompressor: typedNilCompressor,
	}); err == nil {
		t.Fatal("typed-nil context compressor was accepted")
	}
	if _, err := newRecordedTestServer(t, Config{
		Service: &stubService{}, Authority: authority, Decisions: decisions,
		ContextBudgetBytes: maxContextBudgetBytes + 1,
	}); err == nil {
		t.Fatal("oversized context budget was accepted")
	}
	if _, err := newRecordedTestServer(t, Config{
		Service: &stubService{}, Authority: authority, Decisions: decisions,
		ToolCallsPerMinute: MaxToolCallsPerMinute + 1,
	}); err == nil {
		t.Fatal("oversized tool call rate limit was accepted")
	}
}

func TestNewServerRequiresUsableInteractionRecorder(t *testing.T) {
	base := Config{
		Service: &stubService{}, Authority: auth.NewAuthority(),
		Decisions: DecisionProviderFunc(func(context.Context) (auth.Decision, error) {
			return testDecision(t), nil
		}),
		Snapshots: testSnapshotProvider{snapshot: explorer.Snapshot{
			ID: "snapshot", AsOf: time.Now().UTC(),
		}},
	}
	if _, err := NewServer(base); err == nil {
		t.Fatal("server accepted missing interaction recorder")
	}
	var typedNil *testInteractionSink
	base.InteractionSink = typedNil
	if _, err := NewServer(base); err == nil {
		t.Fatal("server accepted typed-nil interaction sink")
	}
}

func TestOptionalToolArgumentsAreValidatedBeforeDispatch(t *testing.T) {
	calls := 0
	provider := optionalProvider{
		tool: Tool{
			Name: "bounded", Description: "bounded optional tool",
			InputSchema: json.RawMessage(`{
				"type":"object",
				"properties":{
					"limit":{"type":"integer","minimum":1,"maximum":3},
					"mode":{"type":"string","enum":["brief","full"]},
					"tags":{"type":"array","items":{"type":"string"},"maxItems":2}
				},
				"required":["limit"],
				"additionalProperties":false
			}`),
		},
		call: func(context.Context, json.RawMessage) (any, error) {
			calls++
			return struct {
				Accepted bool `json:"accepted"`
			}{Accepted: true}, nil
		},
	}
	server, _ := newTestServer(
		t, &stubService{}, []OptionalToolProvider{provider})
	makeReady(t, server)
	for _, arguments := range []string{
		`{}`,
		`{"limit":"2"}`,
		`{"limit":4}`,
		`{"limit":2,"mode":"unknown"}`,
		`{"limit":2,"tags":["a","b","c"]}`,
		`{"limit":2,"extra":true}`,
	} {
		response := callToolRequest(t, server, "bounded", arguments)
		if response.Error != nil {
			t.Fatalf("invalid arguments became protocol error: %+v", response.Error)
		}
		if result := decodeToolResult(t, response); !result.IsError {
			t.Fatalf("invalid arguments %s were accepted", arguments)
		}
	}
	if calls != 0 {
		t.Fatalf("provider calls after invalid arguments = %d", calls)
	}
	response := callToolRequest(
		t, server, "bounded",
		`{"limit":2,"mode":"brief","tags":["one","two"]}`,
	)
	if response.Error != nil {
		t.Fatalf("valid arguments protocol error = %+v", response.Error)
	}
	if result := decodeToolResult(t, response); result.IsError {
		t.Fatalf("valid arguments tool error = %s", result.StructuredContent)
	}
	if calls != 1 {
		t.Fatalf("provider calls = %d, want 1", calls)
	}
}

func TestToolErrorResultBoundsPublicFailures(t *testing.T) {
	server, _ := newTestServer(t, &stubService{}, nil)
	result := server.toolErrorResult(shoal.NewError(
		shoal.ErrorInvalidArgument,
		strings.Repeat("x", maxToolFailureMessageBytes+1),
	))
	if !result.IsError ||
		uint64(len(result.StructuredContent)) > webapi.MaxResponseBytes {
		t.Fatalf("bounded error result = %+v", result)
	}
	var failure structuredToolFailure
	mustUnmarshal(t, string(result.StructuredContent), &failure)
	if failure.Error.Code != string(shoal.ErrorInternal) ||
		failure.Error.Message != "tool execution failed" {
		t.Fatalf("bounded failure = %+v", failure)
	}
}

func TestByteBoundedSchemasDoNotAdvertiseCharacterLimits(t *testing.T) {
	for name, schema := range map[string]json.RawMessage{
		"retrieval":  retrieveSchema,
		"extraction": extractSchema,
	} {
		text := string(schema)
		if strings.Contains(text, `"maxLength":16384`) ||
			strings.Contains(text, `"maxLength":65536`) ||
			!strings.Contains(text, "maximum ") ||
			!strings.Contains(text, " bytes") {
			t.Fatalf("%s schema does not describe its byte bound: %s", name, text)
		}
	}
}

func TestToolAnnotationsPreserveOmittedHints(t *testing.T) {
	tool := Tool{
		Name: "annotations", Description: "annotation serialization",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Annotations: &ToolAnnotations{ReadOnlyHint: boolHint(true)},
	}
	encoded, err := json.Marshal(tool)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	if !strings.Contains(text, `"readOnlyHint":true`) {
		t.Fatalf("read-only hint missing: %s", encoded)
	}
	for _, omitted := range []string{
		`"destructiveHint"`, `"idempotentHint"`, `"openWorldHint"`,
	} {
		if strings.Contains(text, omitted) {
			t.Fatalf("omitted hint %s was serialized: %s", omitted, encoded)
		}
	}

	service := &allOptionalService{
		stubService: &stubService{}, calls: make(map[string]int),
	}
	server, _ := newTestServer(t, service, nil)
	var ingest *Tool
	for index := range server.tools {
		if server.tools[index].definition.Name == ToolIngest {
			ingest = &server.tools[index].definition
			break
		}
	}
	if ingest == nil || ingest.Annotations == nil ||
		ingest.Annotations.DestructiveHint == nil ||
		!*ingest.Annotations.DestructiveHint {
		t.Fatalf("ingest destructive annotation = %+v", ingest)
	}
}

type stubService struct {
	documents func(
		context.Context,
		webapi.DocumentsRequest,
	) (webapi.DocumentsResponse, error)
	document func(
		context.Context,
		webapi.DocumentRequest,
	) (webapi.DocumentResponse, error)
	retrieve func(
		context.Context,
		webapi.RetrievalRequest,
	) (webapi.RetrievalResponse, error)
	neighborhood func(
		context.Context,
		webapi.NeighborhoodRequest,
	) (webapi.NeighborhoodResponse, error)
	path func(
		context.Context,
		webapi.PathRequest,
	) (webapi.PathResponse, error)
}

func (s *stubService) Documents(
	ctx context.Context,
	request webapi.DocumentsRequest,
) (webapi.DocumentsResponse, error) {
	if s.documents != nil {
		return s.documents(ctx, request)
	}
	return webapi.DocumentsResponse{}, nil
}

func (s *stubService) Document(
	ctx context.Context,
	request webapi.DocumentRequest,
) (webapi.DocumentResponse, error) {
	if s.document != nil {
		return s.document(ctx, request)
	}
	return webapi.DocumentResponse{}, nil
}

func (s *stubService) Retrieve(
	ctx context.Context,
	request webapi.RetrievalRequest,
) (webapi.RetrievalResponse, error) {
	if s.retrieve != nil {
		return s.retrieve(ctx, request)
	}
	return webapi.RetrievalResponse{}, nil
}

func (s *stubService) Neighborhood(
	ctx context.Context,
	request webapi.NeighborhoodRequest,
) (webapi.NeighborhoodResponse, error) {
	if s.neighborhood != nil {
		return s.neighborhood(ctx, request)
	}
	return webapi.NeighborhoodResponse{}, nil
}

func (s *stubService) Path(
	ctx context.Context,
	request webapi.PathRequest,
) (webapi.PathResponse, error) {
	if s.path != nil {
		return s.path(ctx, request)
	}
	return webapi.PathResponse{}, nil
}

type changesService struct {
	*stubService
	changes func(
		context.Context,
		webapi.ChangesRequest,
	) (webapi.ChangesResponse, error)
}

func (s *changesService) Changes(
	ctx context.Context,
	request webapi.ChangesRequest,
) (webapi.ChangesResponse, error) {
	if s.changes != nil {
		return s.changes(ctx, request)
	}
	return webapi.ChangesResponse{NextCursor: "next"}, nil
}

type allOptionalService struct {
	*stubService
	calls map[string]int
}

type limitedOptionalService struct {
	*allOptionalService
}

func (*limitedOptionalService) IngestAvailable() bool {
	return false
}

func (*limitedOptionalService) ExtractionAvailable() bool {
	return false
}

func (*limitedOptionalService) ChangesAvailable() bool {
	return false
}

func (s *allOptionalService) Ingest(
	context.Context,
	webapi.IngestRequest,
) (webapi.IngestResponse, error) {
	s.calls[ToolIngest]++
	return webapi.IngestResponse{}, nil
}

func (s *allOptionalService) Extract(
	context.Context,
	webapi.ExtractRequest,
) (webapi.ExtractResponse, error) {
	s.calls[ToolExtract]++
	return webapi.ExtractResponse{}, nil
}

func (s *allOptionalService) Recompute(
	context.Context,
	webapi.RecomputeDerivationRequest,
) (webapi.RecomputeDerivationResponse, error) {
	s.calls[ToolRecompute]++
	return webapi.RecomputeDerivationResponse{}, nil
}

func (s *allOptionalService) Changes(
	context.Context,
	webapi.ChangesRequest,
) (webapi.ChangesResponse, error) {
	s.calls[ToolChanges]++
	return webapi.ChangesResponse{}, nil
}

type optionalProvider struct {
	tool      Tool
	operation auth.Operation
	call      func(context.Context, json.RawMessage) (any, error)
}

type unscopedOptionalProvider struct {
	tool Tool
}

func (p unscopedOptionalProvider) Tool() Tool {
	return p.tool
}

func (unscopedOptionalProvider) Call(
	context.Context, json.RawMessage,
) (any, error) {
	return struct{}{}, nil
}

func (p optionalProvider) Tool() Tool {
	return p.tool
}

func (p optionalProvider) ToolAuthorizationOperation() auth.Operation {
	if p.operation == "" {
		return auth.OperationRetrieve
	}
	return p.operation
}

func (p optionalProvider) Call(
	ctx context.Context,
	arguments json.RawMessage,
) (any, error) {
	if p.call != nil {
		return p.call(ctx, arguments)
	}
	return struct {
		Compressed bool `json:"compressed"`
	}{Compressed: true}, nil
}

type recordingCompressor struct {
	inputs []CompressionInput
}

func (c *recordingCompressor) CompressContext(
	input CompressionInput,
) (CompressionOutput, error) {
	c.inputs = append(c.inputs, input)
	return CompressContext(input)
}

type failingCompressor struct{}

func (failingCompressor) CompressContext(
	CompressionInput,
) (CompressionOutput, error) {
	return CompressionOutput{}, errors.New("compression failed")
}

func newTestServer(
	t *testing.T,
	service webapi.Service,
	optional []OptionalToolProvider,
) (*Server, *auth.Authority) {
	t.Helper()
	authority := auth.NewAuthority()
	server, err := newRecordedTestServer(t, Config{
		Service: service, Authority: authority,
		Decisions: DecisionProviderFunc(func(context.Context) (auth.Decision, error) {
			return testDecision(t), nil
		}),
		OptionalTools:    optional,
		requestIDFactory: sequentialRequestIDs(),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return server, authority
}

type testInteractionSink struct {
	sessions []interaction.Session
	err      error
}

func (*testInteractionSink) EnsureInteractionSink(context.Context) error {
	return nil
}

func (s *testInteractionSink) RecordInteraction(
	ctx context.Context, session interaction.Session,
) error {
	_, err := s.RecordInteractionResult(ctx, session)
	return err
}

func (s *testInteractionSink) RecordInteractionResult(
	_ context.Context, session interaction.Session,
) (interaction.Session, error) {
	if s.err != nil {
		return interaction.Session{}, s.err
	}
	s.sessions = append(s.sessions, session)
	return session, nil
}

type testSnapshotProvider struct {
	snapshot explorer.Snapshot
	err      error
}

func (s testSnapshotProvider) Snapshot(
	context.Context,
) (explorer.Snapshot, error) {
	if s.err != nil {
		return explorer.Snapshot{}, s.err
	}
	return s.snapshot, nil
}

func newRecordedTestServer(t *testing.T, config Config) (*Server, error) {
	t.Helper()
	if config.Recorder == nil {
		recorder, err := interaction.NewRecorder(
			context.Background(), &testInteractionSink{})
		if err != nil {
			t.Fatalf("NewRecorder: %v", err)
		}
		config.Recorder = recorder
	}
	if isAbsent(config.Snapshots) {
		config.Snapshots = testSnapshotProvider{snapshot: explorer.Snapshot{
			ID:   "snapshot",
			AsOf: time.Now().UTC(),
		}}
	}
	return NewServer(config)
}

func testDecision(t *testing.T) auth.Decision {
	t.Helper()
	return decisionWithExpiry(t, time.Now().Add(time.Hour))
}

func decisionWithExpiry(t *testing.T, expires time.Time) auth.Decision {
	t.Helper()
	return decisionWithExpiryAndOntology(
		t, expires, ontology.UnknownOntology())
}

func decisionWithExpiryAndOntology(
	t *testing.T,
	expires time.Time,
	selected ontology.OntologyIdentity,
) auth.Decision {
	t.Helper()
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "subject", Actor: "actor",
		AuthorizationDomain: []byte("domain"),
		AllowedOperations: []auth.Operation{
			auth.OperationList,
			auth.OperationRead,
			auth.OperationRetrieve,
			auth.OperationNeighborhood,
			auth.OperationIngest,
			auth.OperationConnect,
			auth.OperationValidate,
		},
		PermittedSourceIDs:    [][]byte{[]byte("source")},
		PermittedPolicyIDs:    [][]byte{[]byte("policy")},
		PolicyGeneration:      1,
		AuthenticationExpires: expires,
		RequestID:             "template-request",
		SelectedOntology:      selected,
	})
	if err != nil {
		t.Fatalf("NewDecision: %v", err)
	}
	return decision
}

func testOntologyIdentity(t *testing.T) ontology.OntologyIdentity {
	t.Helper()
	schema, err := ontology.NewOntologySchema("mcp-test", "MCP Test", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	version, err := ontology.NewOntologyVersion(
		schema, "1", time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC),
		nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := ontology.NewOntologyIdentity(version)
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

func sequentialRequestIDs() func() (shoal.ID, error) {
	next := 0
	return func() (shoal.ID, error) {
		next++
		return shoal.ID("request-" + string(rune('0'+next))), nil
	}
}

func makeReady(t *testing.T, server *Server) {
	t.Helper()
	params := json.RawMessage(
		`{"protocolVersion":"` + ProtocolVersion +
			`","capabilities":{},"clientInfo":{"name":"test","version":"1"}}`)
	response := server.handle(context.Background(), Request{
		JSONRPC: jsonRPCVersion, ID: json.RawMessage("1"),
		Method: "initialize", Params: params,
	})
	if response == nil || response.Error != nil {
		t.Fatalf("initialize = %+v", response)
	}
	if response := server.handle(context.Background(), Request{
		JSONRPC: jsonRPCVersion, Method: "notifications/initialized",
	}); response != nil {
		t.Fatalf("initialized notification returned %+v", response)
	}
}

func listedToolNames(t *testing.T, server *Server) []string {
	t.Helper()
	response := server.handle(context.Background(), Request{
		JSONRPC: jsonRPCVersion, ID: json.RawMessage("2"),
		Method: "tools/list",
	})
	if response == nil || response.Error != nil {
		t.Fatalf("tools/list = %+v", response)
	}
	var result ListToolsResult
	mustUnmarshal(t, string(response.Result), &result)
	names := make([]string, len(result.Tools))
	for index := range result.Tools {
		names[index] = result.Tools[index].Name
	}
	return names
}

func callToolRequest(
	t *testing.T,
	server *Server,
	name string,
	arguments string,
) Response {
	t.Helper()
	params, err := json.Marshal(CallToolParams{
		Name: name, Arguments: json.RawMessage(arguments),
	})
	if err != nil {
		t.Fatalf("marshal call params: %v", err)
	}
	response := server.handle(context.Background(), Request{
		JSONRPC: jsonRPCVersion, ID: json.RawMessage(`"call"`),
		Method: "tools/call", Params: params,
	})
	if response == nil {
		t.Fatal("tools/call returned no response")
	}
	return *response
}

func decodeToolResult(t *testing.T, response Response) ToolResult {
	t.Helper()
	var result ToolResult
	mustUnmarshal(t, string(response.Result), &result)
	return result
}

func mustUnmarshal(t *testing.T, value string, target any) {
	t.Helper()
	if err := json.Unmarshal([]byte(value), target); err != nil {
		t.Fatalf("unmarshal %q: %v", value, err)
	}
}

func nonemptyLines(value string) []string {
	var result []string
	for _, line := range strings.Split(value, "\n") {
		if strings.TrimSpace(line) != "" {
			result = append(result, line)
		}
	}
	return result
}

func assertStrings(t *testing.T, actual, expected []string) {
	t.Helper()
	if len(actual) != len(expected) {
		t.Fatalf("values = %q, want %q", actual, expected)
	}
	for index := range expected {
		if actual[index] != expected[index] {
			t.Fatalf("values = %q, want %q", actual, expected)
		}
	}
}

func assertIDs(t *testing.T, actual, expected []shoal.ID) {
	t.Helper()
	if len(actual) != len(expected) {
		t.Fatalf("IDs = %q, want %q", actual, expected)
	}
	for index := range expected {
		if actual[index] != expected[index] {
			t.Fatalf("IDs = %q, want %q", actual, expected)
		}
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
