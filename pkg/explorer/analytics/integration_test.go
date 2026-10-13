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

package analytics_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/analytics"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/explorer/mcp"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestAuthorizedAnalyticsHiddenTopologyDoesNotInfluenceOutput(t *testing.T) {
	fixture := newAnalyticsFixture(t)
	a1 := fixture.ingest(t, fixture.clientA, "memory://a1", "alpha")
	a2 := fixture.ingest(t, fixture.clientA, "memory://a2", "bravo")
	a3 := fixture.ingest(t, fixture.clientA, "memory://a3", "charlie")
	fixture.connect(t, fixture.clientA, "edge-a1-a2", a1, a2)
	fixture.connect(t, fixture.clientA, "edge-a1-a3", a1, a3)
	fixture.connect(t, fixture.clientA, "edge-a2-a3", a2, a3)

	service := fixture.service(t)
	request := analyticsRequest(a1)
	alice := fixture.readContext(t, "alice", fixture.sourceA, fixture.policyA, ontology.OntologyIdentity{})
	before, err := service.Run(alice, request)
	if err != nil {
		t.Fatal(err)
	}

	hidden := fixture.ingest(t, fixture.clientB, "memory://hidden", "hidden")
	fixture.connect(t, fixture.clientA, "edge-a1-hidden", a1, hidden)
	after, err := service.Run(alice, request)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("hidden topology changed authorized analytics:\nbefore=%#v\nafter=%#v", before, after)
	}

	admin, err := service.Run(fixture.adminContext(t), request)
	if err != nil {
		t.Fatal(err)
	}
	if admin.Scope.NodeCount != before.Scope.NodeCount+1 ||
		admin.Scope.EdgeCount != before.Scope.EdgeCount+1 {
		t.Fatalf("intentional wider subgraph = %#v, narrow = %#v", admin.Scope, before.Scope)
	}
	if reflect.DeepEqual(admin.Nodes, before.Nodes) {
		t.Fatal("different authorized subgraphs produced identical node analytics")
	}

	visible := fixture.ingest(t, fixture.clientA, "memory://a4", "delta")
	fixture.connect(t, fixture.clientA, "edge-a3-a4", a3, visible)
	pinned := request
	pinned.SnapshotID = before.Scope.SnapshotID
	if _, err := service.Run(alice, pinned); !shoal.IsErrorCode(
		err, shoal.ErrorConflict,
	) {
		t.Fatalf("stale authorized snapshot error = %v", err)
	}
}

func TestAuthorizedAnalyticsSeparatesOntologyLensesAndRevocation(t *testing.T) {
	fixture := newAnalyticsFixture(t)
	a1 := fixture.ingest(t, fixture.clientA, "memory://a1", "alpha")
	a2 := fixture.ingest(t, fixture.clientA, "memory://a2", "bravo")
	fixture.connect(t, fixture.clientA, "edge-a1-a2", a1, a2)
	schema, err := ontology.NewOntologySchema("analytics", "Analytics", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	firstVersion, err := ontology.NewOntologyVersion(
		schema, "1", fixture.now, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	secondVersion, err := ontology.NewOntologyVersion(
		schema, "2", fixture.now.Add(time.Second), nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	firstLens, _ := ontology.NewOntologyIdentity(firstVersion)
	secondLens, _ := ontology.NewOntologyIdentity(secondVersion)
	service := fixture.service(t)
	request := analyticsRequest(a1)
	first, err := service.Run(
		fixture.readContext(t, "alice", fixture.sourceA, fixture.policyA, firstLens),
		request,
	)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Run(
		fixture.readContext(t, "alice", fixture.sourceA, fixture.policyA, secondLens),
		request,
	)
	if err != nil {
		t.Fatal(err)
	}
	if first.Scope.Ontology == nil || second.Scope.Ontology == nil ||
		first.Scope.Ontology.VersionID == second.Scope.Ontology.VersionID ||
		first.Scope.AuthorizationFingerprint == second.Scope.AuthorizationFingerprint ||
		first.Scope.SnapshotID == second.Scope.SnapshotID {
		t.Fatalf("ontology lenses were not isolated:\nfirst=%#v\nsecond=%#v", first.Scope, second.Scope)
	}
	if !reflect.DeepEqual(first.Nodes, second.Nodes) {
		t.Fatal("ontology identity changed topology-only analytics")
	}

	fixture.generations.Set(fixture.domain, 2)
	_, err = service.Run(
		fixture.readContextAtGeneration(
			t, "stale", fixture.sourceA, fixture.policyA,
			ontology.OntologyIdentity{}, 1,
		),
		request,
	)
	if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("revoked generation error = %v", err)
	}
}

func TestAuthorizedAnalyticsRejectsIncompleteMaterialization(t *testing.T) {
	fixture := newAnalyticsFixture(t)
	a1 := fixture.ingest(t, fixture.clientA, "memory://a1", "alpha")
	a2 := fixture.ingest(t, fixture.clientA, "memory://a2", "bravo")
	a3 := fixture.ingest(t, fixture.clientA, "memory://a3", "charlie")
	fixture.connect(t, fixture.clientA, "edge-a1-a2", a1, a2)
	fixture.connect(t, fixture.clientA, "edge-a1-a3", a1, a3)
	for name, mutate := range map[string]func(*analytics.Request){
		"fanout": func(request *analytics.Request) {
			request.Scope.Fanout = 1
		},
		"nodes": func(request *analytics.Request) {
			request.Scope.MaxNodes = 2
		},
		"edges": func(request *analytics.Request) {
			request.Scope.MaxEdges = 1
		},
	} {
		t.Run(name, func(t *testing.T) {
			request := analyticsRequest(a1)
			mutate(&request)
			_, err := fixture.service(t).Run(
				fixture.readContext(
					t, "alice", fixture.sourceA, fixture.policyA,
					ontology.OntologyIdentity{},
				),
				request,
			)
			if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
				t.Fatalf("incomplete materialization error = %v", err)
			}
		})
	}
}

func TestAuthorizedAnalyticsRequiresAnalyticsOperation(t *testing.T) {
	fixture := newAnalyticsFixture(t)
	seed := fixture.ingest(t, fixture.clientA, "memory://a1", "alpha")
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "reader", Actor: "reader-actor",
		AuthorizationDomain:   fixture.domain,
		AllowedOperations:     []auth.Operation{auth.OperationNeighborhood},
		PermittedSourceIDs:    [][]byte{fixture.sourceA},
		PermittedPolicyIDs:    [][]byte{fixture.policyA},
		PolicyGeneration:      1,
		AuthenticationExpires: fixture.now.Add(time.Hour),
		RequestID:             "reader-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := fixture.authority.Binder().Bind(context.Background(), decision)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service(t).Run(
		ctx, analyticsRequest(seed),
	); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("missing analytics operation error = %v", err)
	}
}

func TestAuthorizedAnalyticsRecordingSeamIsExplicit(t *testing.T) {
	fixture := newAnalyticsFixture(t)
	seed := fixture.ingest(t, fixture.clientA, "memory://a1", "alpha")
	if _, err := analytics.NewService(analytics.Config{
		Source: fixture.clientA, Limits: analytics.DefaultLimits(),
		RequireRecording: true,
	}); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("missing required recorder error = %v", err)
	}
	recorder := &analyticsRecorder{}
	service, err := analytics.NewService(analytics.Config{
		Source: fixture.clientA, Limits: analytics.DefaultLimits(),
		Recorder: recorder, RequireRecording: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Run(
		fixture.readContext(
			t, "alice", fixture.sourceA, fixture.policyA, ontology.OntologyIdentity{}),
		analyticsRequest(seed),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Recording.Recorded || !result.Recording.Required ||
		result.Recording.InteractionID != "interaction-test" ||
		recorder.calls != 1 ||
		len(recorder.recorded.Materialization.Neighborhood.Nodes) == 0 {
		t.Fatalf("recording status = %#v, recorder = %#v", result.Recording, recorder)
	}
}

func TestEmbeddedAnalyticsDurablyRecordsCompleteAuthorizedEvidence(t *testing.T) {
	fixture := newAnalyticsFixture(t)
	a1 := fixture.ingest(t, fixture.clientA, "memory://a1", "alpha")
	a2 := fixture.ingest(t, fixture.clientA, "memory://a2", "bravo")
	a3 := fixture.ingest(t, fixture.clientA, "memory://a3", "charlie")
	fixture.connect(t, fixture.clientA, "edge-a1-a2", a1, a2)
	fixture.connect(t, fixture.clientB, "edge-a2-a3", a2, a3)
	schema, err := ontology.NewOntologySchema("analytics", "Analytics", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	version, err := ontology.NewOntologyVersion(
		schema, "1", fixture.now, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	lens, _ := ontology.NewOntologyIdentity(version)
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "analytics-subject", Actor: "analytics-actor",
		ClientID:              "analytics-client",
		OnBehalfOf:            []shoal.ID{"fleet"},
		AuthorizationDomain:   fixture.domain,
		AllowedOperations:     []auth.Operation{auth.OperationAnalyticsRead, auth.OperationRetrieve},
		PermittedSourceIDs:    [][]byte{fixture.sourceA, fixture.sourceB},
		PermittedPolicyIDs:    [][]byte{fixture.policyA, fixture.policyB},
		PolicyGeneration:      1,
		AuthenticationExpires: fixture.now.Add(time.Hour),
		RequestID:             "analytics-request",
		AuditPurpose:          "rank authorized incident evidence",
		SelectedOntology:      lens,
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := webapi.NewEmbeddedService(fixture.clientA)
	if err != nil {
		t.Fatal(err)
	}
	if !service.AnalyticsAvailable() {
		t.Fatal("embedded analytics was advertised without its required recorder")
	}
	request := analyticsRequest(a1)
	wireRequest := webapi.AnalyticsRequest{
		Scope: request.Scope, PageRank: request.PageRank,
	}
	body, err := json.Marshal(wireRequest)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := webapi.NewAuthenticatedHandler(
		service,
		webapi.AuthenticatorFunc(func(*http.Request) (auth.Decision, error) {
			return decision, nil
		}),
		fixture.authority.Binder(),
		"workspace.test",
	)
	if err != nil {
		t.Fatal(err)
	}

	httpRequest := httptest.NewRequest(
		http.MethodPost,
		"http://workspace.test/api/v1/analytics",
		bytes.NewReader(body),
	)
	httpRequest.Host = "workspace.test"
	httpRequest.Header.Set("Content-Type", "application/json")
	httpResponse := httptest.NewRecorder()
	handler.ServeHTTP(httpResponse, httpRequest)
	if httpResponse.Code != http.StatusOK {
		t.Fatalf("analytics HTTP status = %d: %s",
			httpResponse.Code, httpResponse.Body.String())
	}

	var response webapi.AnalyticsResponse
	if err := json.Unmarshal(httpResponse.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Analytics.Recording.Recorded ||
		!response.Analytics.Recording.Required ||
		response.Analytics.Recording.InteractionID == "" {
		t.Fatalf("recording status = %#v", response.Analytics.Recording)
	}
	recorded, err := fixture.base.Interaction(
		context.Background(), response.Analytics.Recording.InteractionID)
	if err != nil {
		t.Fatal(err)
	}
	if recorded.Operation != interaction.OperationToolCall ||
		recorded.Actor.SubjectID != decision.Subject() ||
		recorded.Actor.ActorID != decision.Actor() ||
		recorded.Actor.ClientID != decision.ClientID() ||
		!reflect.DeepEqual(recorded.Actor.OnBehalfOf, decision.OnBehalfOf()) ||
		recorded.Reason.Code != "audit_purpose" ||
		recorded.Reason.Digest != interaction.Digest(decision.AuditPurpose()) ||
		recorded.AuthorizationExpiresAt != decision.AuthenticationExpires() ||
		recorded.AuthorizationOperation != string(auth.OperationAnalyticsRead) ||
		recorded.OntologySchemaID != lens.SchemaID() ||
		recorded.OntologyVersionID != lens.VersionID() {
		t.Fatalf("trusted interaction metadata = %+v", recorded)
	}
	if len(recorded.Turns) != 1 || recorded.Turns[0].ToolCall == nil ||
		len(recorded.Turns[0].ToolCall.RetrievedNodeIDs) != 3 ||
		len(recorded.Turns[0].ToolCall.RetrievedEvidence) == 0 ||
		len(recorded.TouchedEdgeIDs()) != 2 {
		t.Fatalf("recorded evidence = %+v", recorded)
	}
	if err := fixture.base.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := explorer.Open(fixture.corpusDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	recovered, err := reopened.Interaction(context.Background(), recorded.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(
		recovered.Turns[0].ToolCall.RetrievedEvidence,
		recorded.Turns[0].ToolCall.RetrievedEvidence,
	) {
		t.Fatalf("recovered interaction evidence = %+v", recovered)
	}
	if recovered.OntologySchemaID != lens.SchemaID() ||
		recovered.OntologyVersionID != lens.VersionID() {
		t.Fatalf("recovered ontology pin = %+v", recovered)
	}
}

func TestMCPAnalyticsDurablyRecordsBeforeSuccess(t *testing.T) {
	fixture := newAnalyticsFixture(t)
	seed := fixture.ingest(t, fixture.clientA, "memory://a1", "alpha")
	service, err := webapi.NewEmbeddedService(fixture.clientA)
	if err != nil {
		t.Fatal(err)
	}
	limits, available := service.AnalyticsLimits()
	if !available {
		t.Fatal("embedded analytics recorder is unavailable")
	}
	tool, err := mcp.NewAnalyticsTool(service, limits)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "mcp-subject", Actor: "mcp-actor",
		AuthorizationDomain:   fixture.domain,
		AllowedOperations:     []auth.Operation{auth.OperationAnalyticsRead, auth.OperationRetrieve},
		PermittedSourceIDs:    [][]byte{fixture.sourceA},
		PermittedPolicyIDs:    [][]byte{fixture.policyA},
		PolicyGeneration:      1,
		AuthenticationExpires: fixture.now.Add(time.Hour),
		RequestID:             "mcp-template-request",
		AuditPurpose:          "analyze authorized workspace",
	})
	if err != nil {
		t.Fatal(err)
	}
	server, err := mcp.NewServer(mcp.Config{
		Service: service, Authority: fixture.authority,
		Decisions: mcp.DecisionProviderFunc(func(
			context.Context,
		) (auth.Decision, error) {
			return decision, nil
		}),
		InteractionSink: fixture.clientA,
		Snapshots:       fixture.clientA,
		OptionalTools:   []mcp.OptionalToolProvider{tool},
	})
	if err != nil {
		t.Fatal(err)
	}
	arguments, err := json.Marshal(webapi.AnalyticsRequest{
		Scope: analyticsRequest(seed).Scope,
	})
	if err != nil {
		t.Fatal(err)
	}
	call, err := json.Marshal(mcp.CallToolParams{
		Name: mcp.ToolAnalytics, Arguments: arguments,
	})
	if err != nil {
		t.Fatal(err)
	}
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"` +
			mcp.ProtocolVersion +
			`","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":` +
			string(call) + `}`,
	}, "\n") + "\n"
	var output bytes.Buffer
	if err := server.Serve(
		context.Background(), strings.NewReader(input), &output,
	); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"recorded":true`) ||
		!strings.Contains(output.String(), `"interaction_id"`) {
		t.Fatalf("MCP analytics did not return a durable receipt: %s", output.String())
	}
	summaries, err := fixture.base.Interactions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) == 0 {
		t.Fatalf("recorded MCP interactions = %+v", summaries)
	}
}

func TestAnalyticsInteractionSinkReauthorizesExactEdgeEvidence(t *testing.T) {
	fixture := newAnalyticsFixture(t)
	from := fixture.ingest(t, fixture.clientA, "memory://a1", "alpha")
	to := fixture.ingest(t, fixture.clientA, "memory://a2", "bravo")
	exactID, err := fixture.clientA.ApplicationEdgeID(fixture.adminContext(t), "edge-a1-a2")
	if err != nil {
		t.Fatal(err)
	}
	exact := graph.Edge{
		ID: exactID, From: from, To: to, Type: "related", Weight: 1,
	}
	if err := fixture.clientA.Connect(fixture.adminContext(t), exact); err != nil {
		t.Fatal(err)
	}
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "analytics-subject", Actor: "analytics-actor",
		AuthorizationDomain:   fixture.domain,
		AllowedOperations:     []auth.Operation{auth.OperationAnalyticsRead, auth.OperationRetrieve},
		PermittedSourceIDs:    [][]byte{fixture.sourceA},
		PermittedPolicyIDs:    [][]byte{fixture.policyA},
		PolicyGeneration:      1,
		AuthenticationExpires: fixture.now.Add(time.Hour),
		RequestID:             "analytics-edge-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := fixture.authority.Binder().Bind(context.Background(), decision)
	if err != nil {
		t.Fatal(err)
	}
	materialized, err := fixture.clientA.MaterializeAnalytics(
		ctx,
		explorer.BoundedNeighborhoodRequest{
			NodeIDs: []shoal.ID{from}, Depth: 1, Fanout: 4,
			MaxNodes: 4, MaxScannedEdges: 16,
			EdgeTypes: []string{"related"},
			Direction: explorer.GraphDirectionOutgoing,
		},
		4,
		analytics.HardMaxEvidenceBytes,
	)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		t.Fatal(err)
	}
	sink := fixture.clientA.AnalyticsInteractionSink()
	if sink == nil {
		t.Fatal("analytics interaction sink is unavailable")
	}
	recorder, err := interaction.NewRecorder(context.Background(), sink)
	if err != nil {
		t.Fatal(err)
	}
	recordedAt := fixture.now.Add(time.Minute)
	sessionID, err := interaction.OperationSessionID(
		interaction.OperationToolCall, decision.RequestID(), recordedAt)
	if err != nil {
		t.Fatal(err)
	}
	nodesByID := make(map[shoal.ID]graph.Node, len(materialized.Neighborhood.Nodes))
	for _, node := range materialized.Neighborhood.Nodes {
		nodesByID[node.ID] = node
	}
	anchor, err := inference.NewGraphAnchor(graph.Path{
		Nodes: []graph.Node{nodesByID[from], nodesByID[to]},
		Edges: []graph.Edge{exact},
	})
	if err != nil {
		t.Fatal(err)
	}
	session := interaction.Session{
		ID:                       sessionID,
		RecordedAt:               recordedAt,
		Operation:                interaction.OperationToolCall,
		SnapshotID:               shoal.ID(materialized.Snapshot.ID),
		SnapshotAsOf:             materialized.Snapshot.AsOf,
		AuthorizationFingerprint: shoal.ID(fingerprint.String()),
		AuthorizationExpiresAt:   decision.AuthenticationExpires(),
		RequestID:                decision.RequestID(),
		SeedNodeIDs:              []shoal.ID{from},
		Turns: []interaction.Turn{{
			Index: 0,
			ToolCall: &interaction.ToolCall{
				Kind:             "analytics",
				RetrievedNodeIDs: []shoal.ID{from, to},
				RetrievedEvidence: []interaction.EvidenceReference{{
					AnchorID: anchor.ID(),
					Kind:     interaction.EvidenceGraph,
					NodeIDs:  []shoal.ID{from, to},
					EdgeIDs:  []shoal.ID{"missing-edge"},
				}},
			},
		}},
	}
	if _, err := recorder.Record(ctx, session); !shoal.IsErrorCode(
		err, shoal.ErrorNotFound,
	) {
		t.Fatalf("altered edge evidence error = %v", err)
	}
	session.Turns[0].ToolCall.RetrievedEvidence[0].EdgeIDs =
		[]shoal.ID{exact.ID}
	recorded, err := recorder.Record(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	if len(recorded.TouchedEdgeIDs()) != 1 ||
		recorded.TouchedEdgeIDs()[0] != exact.ID {
		t.Fatalf("recorded exact edges = %v", recorded.TouchedEdgeIDs())
	}
}

func TestAuthorizedAnalyticsRevalidatesGenerationAfterRecording(t *testing.T) {
	fixture := newAnalyticsFixture(t)
	seed := fixture.ingest(t, fixture.clientA, "memory://a1", "alpha")
	recorder := recorderFunc(func(
		context.Context,
		analytics.Record,
	) (analytics.RecordingReceipt, error) {
		fixture.generations.Set(fixture.domain, 2)
		return analytics.RecordingReceipt{InteractionID: "interaction-test"}, nil
	})
	service, err := analytics.NewService(analytics.Config{
		Source: fixture.clientA, Limits: analytics.DefaultLimits(),
		Recorder: recorder, RequireRecording: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Run(
		fixture.readContext(
			t, "alice", fixture.sourceA, fixture.policyA, ontology.OntologyIdentity{}),
		analyticsRequest(seed),
	)
	if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) ||
		!explorer.IsIndeterminateCommit(err) {
		t.Fatalf("generation change after recording error = %v", err)
	}
}

func TestAuthorizedInteractionSinkMarksPostWriteRevocationIndeterminate(t *testing.T) {
	fixture := newAnalyticsFixture(t)
	seed := fixture.ingest(t, fixture.clientA, "memory://a1", "alpha")
	selector, err := authorized.NewStaticPolicySelector(
		fixture.sourceA, fixture.policyA)
	if err != nil {
		t.Fatal(err)
	}
	recordBase := &generationChangingRecordBase{
		Explorer: fixture.base, generations: fixture.generations,
		domain: fixture.domain,
	}
	client, err := authorized.NewClient(authorized.Config{
		Base:              recordBase,
		InteractionWriter: recordBase,
		InteractionReader: fixture.base,
		SnapshotValidator: fixture.base,
		Resolver:          fixture.authority.Resolver(),
		PolicySelector:    selector, PolicyStore: fixture.store,
		GenerationReader: fixture.generations,
		Clock:            func() time.Time { return fixture.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	sink := client.AnalyticsInteractionSink()
	if sink == nil {
		t.Fatal("analytics interaction sink is unavailable")
	}
	shared, err := interaction.NewRecorder(context.Background(), sink)
	if err != nil {
		t.Fatal(err)
	}
	recorder, err := analytics.NewInteractionRecorder(
		shared, func() time.Time { return fixture.now.Add(time.Minute) })
	if err != nil {
		t.Fatal(err)
	}
	service, err := analytics.NewService(analytics.Config{
		Source: client, Limits: analytics.DefaultLimits(),
		Recorder: recorder, RequireRecording: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Run(
		fixture.readContext(
			t, "alice", fixture.sourceA, fixture.policyA,
			ontology.OntologyIdentity{},
		),
		analyticsRequest(seed),
	)
	if !explorer.IsIndeterminateCommit(err) ||
		!shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("post-write generation error = %v", err)
	}
	summaries, readErr := fixture.base.Interactions(context.Background())
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(summaries) != 1 {
		t.Fatalf("durably recorded interactions = %+v", summaries)
	}
}

func TestAuthorizedAnalyticsRechecksGenerationAfterOntologyLens(t *testing.T) {
	fixture := newAnalyticsFixture(t)
	seed := fixture.ingest(t, fixture.clientA, "memory://a1", "alpha")
	schema, err := ontology.NewOntologySchema("analytics", "Analytics", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	version, err := ontology.NewOntologyVersion(
		schema, "1", fixture.now, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	lens, _ := ontology.NewOntologyIdentity(version)
	selector, err := authorized.NewStaticPolicySelector(
		fixture.sourceA, fixture.policyA)
	if err != nil {
		t.Fatal(err)
	}
	interpreter := &generationChangingLensBase{
		Explorer: fixture.base, generations: fixture.generations,
		domain: fixture.domain,
	}
	client, err := authorized.NewClient(authorized.Config{
		Base:                interpreter,
		OntologyInterpreter: interpreter,
		Resolver:            fixture.authority.Resolver(),
		PolicySelector:      selector,
		PolicyStore:         fixture.store,
		GenerationReader:    fixture.generations,
		Clock:               func() time.Time { return fixture.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := analytics.NewService(analytics.Config{
		Source: client, Limits: analytics.DefaultLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Run(
		fixture.readContext(
			t, "alice", fixture.sourceA, fixture.policyA, lens),
		analyticsRequest(seed),
	)
	if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("post-lens generation change error = %v", err)
	}
	if interpreter.calls != 1 {
		t.Fatalf("ontology interpreter calls = %d, want 1", interpreter.calls)
	}
}

type analyticsFixture struct {
	now         time.Time
	corpusDir   string
	base        *explorer.Explorer
	store       *authorized.MemoryPolicyStore
	authority   *auth.Authority
	generations *analyticsGenerationReader
	clientA     *authorized.Client
	clientB     *authorized.Client
	domain      []byte
	sourceA     []byte
	policyA     []byte
	sourceB     []byte
	policyB     []byte
}

type analyticsRecorder struct {
	calls    int
	recorded analytics.Record
}

type recorderFunc func(
	context.Context,
	analytics.Record,
) (analytics.RecordingReceipt, error)

func (f recorderFunc) RecordAnalytics(
	ctx context.Context,
	record analytics.Record,
) (analytics.RecordingReceipt, error) {
	return f(ctx, record)
}

type generationChangingLensBase struct {
	*explorer.Explorer
	generations *analyticsGenerationReader
	domain      []byte
	calls       int
}

type generationChangingRecordBase struct {
	*explorer.Explorer
	generations *analyticsGenerationReader
	domain      []byte
}

func (b *generationChangingRecordBase) RecordInteractionResult(
	ctx context.Context,
	session interaction.Session,
) (interaction.Session, error) {
	recorded, err := b.Explorer.RecordInteractionResult(ctx, session)
	if err == nil {
		b.generations.Set(b.domain, 2)
	}
	return recorded, err
}

func (b *generationChangingLensBase) InterpretAssertions(
	ctx context.Context,
	assertions []ontology.Assertion,
	selected ontology.OntologyIdentity,
) ([]ontology.AssertionInterpretation, error) {
	b.calls++
	b.generations.Set(b.domain, 2)
	return b.Explorer.InterpretAssertions(ctx, assertions, selected)
}

func (r *analyticsRecorder) RecordAnalytics(
	ctx context.Context,
	record analytics.Record,
) (analytics.RecordingReceipt, error) {
	if err := ctx.Err(); err != nil {
		return analytics.RecordingReceipt{}, err
	}
	r.calls++
	r.recorded = record
	return analytics.RecordingReceipt{InteractionID: "interaction-test"}, nil
}

func newAnalyticsFixture(t *testing.T) *analyticsFixture {
	t.Helper()
	now := time.Now().UTC().Add(time.Minute).Truncate(time.Second)
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	corpusDir := t.TempDir()
	base, err := explorer.Open(corpusDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = base.Close() })
	fixture := &analyticsFixture{
		now: now, corpusDir: corpusDir, base: base,
		store:     authorized.NewMemoryPolicyStore(),
		authority: authority,
		generations: &analyticsGenerationReader{
			values: make(map[string]int64),
		},
		domain: []byte("domain"), sourceA: []byte("source-a"),
		policyA: []byte("policy-a"), sourceB: []byte("source-b"),
		policyB: []byte("policy-b"),
	}
	fixture.generations.Set(fixture.domain, 1)
	fixture.clientA = fixture.client(t, fixture.sourceA, fixture.policyA)
	fixture.clientB = fixture.client(t, fixture.sourceB, fixture.policyB)
	return fixture
}

func (f *analyticsFixture) client(
	t *testing.T,
	source []byte,
	policy []byte,
) *authorized.Client {
	t.Helper()
	selector, err := authorized.NewStaticPolicySelector(source, policy)
	if err != nil {
		t.Fatal(err)
	}
	client, err := authorized.NewClient(authorized.Config{
		Base:              f.base,
		InteractionWriter: f.base,
		InteractionReader: f.base,
		SnapshotValidator: f.base,
		Resolver:          f.authority.Resolver(),
		PolicySelector:    selector, PolicyStore: f.store,
		GenerationReader: f.generations, Clock: func() time.Time { return f.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func (f *analyticsFixture) ingest(
	t *testing.T,
	client *authorized.Client,
	uri string,
	content string,
) shoal.ID {
	t.Helper()
	result, err := client.Ingest(f.adminContext(t), explorer.Source{
		URI: uri, MediaType: explorer.MediaTypeText, Content: content,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result.Document.ID
}

func (f *analyticsFixture) connect(
	t *testing.T,
	client *authorized.Client,
	edgeID string,
	from shoal.ID,
	to shoal.ID,
) {
	t.Helper()
	// Connect accepts only IDs in the caller's namespace (#699).
	id, err := client.ApplicationEdgeID(f.adminContext(t), shoal.ID(edgeID))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Connect(f.adminContext(t), graph.Edge{
		ID: id, From: from, To: to, Type: "related", Weight: 1,
	}); err != nil {
		t.Fatal(err)
	}
}

func (f *analyticsFixture) service(t *testing.T) *analytics.Service {
	t.Helper()
	service, err := analytics.NewService(analytics.Config{
		Source: f.clientA, Limits: analytics.DefaultLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func (f *analyticsFixture) adminContext(t *testing.T) context.Context {
	t.Helper()
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "admin", Actor: "admin-actor",
		AuthorizationDomain: f.domain,
		AllowedOperations: []auth.Operation{
			auth.OperationIngest, auth.OperationConnect, auth.OperationAnalyticsRead,
		},
		PermittedSourceIDs: [][]byte{f.sourceA, f.sourceB},
		PermittedPolicyIDs: [][]byte{f.policyA, f.policyB},
		PolicyGeneration:   1, AuthenticationExpires: f.now.Add(time.Hour),
		RequestID: "admin-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := f.authority.Binder().Bind(context.Background(), decision)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

func (f *analyticsFixture) readContext(
	t *testing.T,
	subject string,
	source []byte,
	policy []byte,
	lens ontology.OntologyIdentity,
) context.Context {
	return f.readContextAtGeneration(t, subject, source, policy, lens, 1)
}

func (f *analyticsFixture) readContextAtGeneration(
	t *testing.T,
	subject string,
	source []byte,
	policy []byte,
	lens ontology.OntologyIdentity,
	generation int64,
) context.Context {
	t.Helper()
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: shoal.ID(subject), Actor: shoal.ID(subject + "-actor"),
		AuthorizationDomain:   f.domain,
		AllowedOperations:     []auth.Operation{auth.OperationAnalyticsRead, auth.OperationRetrieve},
		PermittedSourceIDs:    [][]byte{source},
		PermittedPolicyIDs:    [][]byte{policy},
		PolicyGeneration:      generation,
		AuthenticationExpires: f.now.Add(time.Hour),
		RequestID:             shoal.ID(subject + "-request"),
		SelectedOntology:      lens,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := f.authority.Binder().Bind(context.Background(), decision)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

func analyticsRequest(seed shoal.ID) analytics.Request {
	return analytics.Request{
		Scope: analytics.Scope{
			NodeIDs: []shoal.ID{seed}, Depth: 3,
			Direction: explorer.GraphDirectionOutgoing,
			Fanout:    8, MaxNodes: 16, MaxEdges: 32,
			MaxScannedEdgesPerNode: 256,
			EdgeTypes:              []string{"related"},
		},
	}
}

type analyticsGenerationReader struct {
	mu     sync.RWMutex
	values map[string]int64
}

func (r *analyticsGenerationReader) CurrentPolicyGeneration(
	ctx context.Context,
	domain []byte,
) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.values[string(domain)], nil
}

func (r *analyticsGenerationReader) Set(domain []byte, generation int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.values[string(bytes.Clone(domain))] = generation
}
