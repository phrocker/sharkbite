// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package webapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/explorer/teamoverview"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestFleetDispatchEvidenceWirePreservesExactAnchors(t *testing.T) {
	record := fleet.ActionRecord{
		EvidenceSnapshotID:   "snapshot",
		EvidenceSnapshotAsOf: time.Date(2026, 9, 6, 8, 0, 0, 0, time.UTC),
		Evidence: []fleet.EvidenceRef{
			{
				AnchorID: "document-anchor", Kind: interaction.EvidenceDocument,
				Citation: document.Citation{
					DocumentID: "document", RevisionID: "revision",
					SectionID: "section", SpanID: "span",
					Range: document.SourceRange{
						Start: document.SourcePosition{Offset: 3, Page: 1},
						End:   document.SourcePosition{Offset: 9, Page: 2},
					},
				},
				NodeIDs:    []shoal.ID{"document", "section", "span"},
				Visibility: []string{"restricted"},
			},
			{
				AnchorID: "graph-anchor", Kind: interaction.EvidenceGraph,
				NodeIDs: []shoal.ID{"left", "right"}, EdgeIDs: []shoal.ID{"edge"},
				Assertions: []interaction.AssertionReference{{
					AssertionID: "assertion", EdgeID: "edge",
					Origin: ontology.AssertionDerived,
				}},
				Visibility: []string{"restricted"},
			},
		},
	}
	wire := encodeFleetAction(record)
	// Through the wire: the record encodes the public evidence type and a
	// completion decodes the handler's own, so the round trip crosses both.
	var received []fleetEvidenceWire
	if err := json.Unmarshal([]byte(mustJSON(t, wire.Evidence)), &received); err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeEvidence(received)
	if err != nil {
		t.Fatal(err)
	}
	for index := range decoded {
		decoded[index].NodeIDs = append([]shoal.ID(nil), decoded[index].NodeIDs...)
		decoded[index].EdgeIDs = append([]shoal.ID(nil), decoded[index].EdgeIDs...)
		decoded[index].Assertions = append(
			[]interaction.AssertionReference(nil), decoded[index].Assertions...)
		record.Evidence[index].NodeIDs = append(
			[]shoal.ID(nil), record.Evidence[index].NodeIDs...)
		record.Evidence[index].EdgeIDs = append(
			[]shoal.ID(nil), record.Evidence[index].EdgeIDs...)
		record.Evidence[index].Assertions = append(
			[]interaction.AssertionReference(nil), record.Evidence[index].Assertions...)
	}
	if !reflect.DeepEqual(decoded, record.Evidence) ||
		wire.EvidenceSnapshotID != encodeFleetID("snapshot") ||
		!wire.EvidenceSnapshotAsOf.Equal(record.EvidenceSnapshotAsOf) {
		t.Fatalf("evidence wire lost exact identity: %#v", wire)
	}
}

func TestFleetDispatchMountRequiresAuthenticationAndPreservesOpaqueIDs(t *testing.T) {
	anonymous, err := NewHandler(&stubWorkspaceService{}, "example.test")
	if err != nil {
		t.Fatal(err)
	}
	if err := anonymous.MountFleetDispatch(&stubDispatchProvider{}); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("anonymous mount = %v", err)
	}

	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	authority, _ := auth.NewAuthorityWithClock(func() time.Time { return now })
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "subject", Actor: "actor", AuthorizationDomain: []byte("domain"),
		AllowedOperations:  []auth.Operation{auth.OperationDispatch},
		PermittedSourceIDs: [][]byte{[]byte("source")},
		PermittedPolicyIDs: [][]byte{[]byte("policy")}, PolicyGeneration: 1,
		AuthenticationExpires: now.Add(time.Hour), RequestID: "request",
		CorrelationID: "correlation",
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewAuthenticatedHandler(&stubWorkspaceService{},
		AuthenticatorFunc(func(*http.Request) (auth.Decision, error) { return decision, nil }),
		authority.Binder(), "example.test")
	if err != nil {
		t.Fatal(err)
	}
	provider := &stubDispatchProvider{resolver: authority.Resolver()}
	if err := handler.MountFleetDispatch(provider); err != nil {
		t.Fatal(err)
	}
	actionID := []byte{'a', 0, 255}
	body, _ := json.Marshal(fleetEnqueueWire{
		Context: fleetRequestContextWire{
			RequestID: encodeFleetID("request"), ReasonCode: "operator_request",
			CorrelationID: encodeFleetID("correlation"), Deadline: now.Add(time.Minute),
		},
		ID:             base64.RawURLEncoding.EncodeToString(actionID),
		IdempotencyKey: base64.RawURLEncoding.EncodeToString([]byte{'k', 0, 255}),
		AgentID:        encodeFleetID("agent"), AgentGeneration: 1,
		Capability: "search", Action: "query", SourceID: []byte("source"),
		PolicyID: []byte("policy"), ObjectID: encodeFleetID("object"),
		Input: json.RawMessage(`{"value":1}`), NotBefore: now.Add(30 * time.Second),
	})
	request := httptest.NewRequest(http.MethodPost, "http://example.test/api/v1/fleet/actions", bytes.NewReader(body))
	request.Host = "example.test"
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if !provider.enqueued || !bytes.Equal(provider.actionID, actionID) {
		t.Fatalf("provider action=%x enqueued=%v", provider.actionID, provider.enqueued)
	}
	if !provider.notBefore.Equal(now.Add(30 * time.Second)) {
		t.Fatalf("provider not-before time = %v", provider.notBefore)
	}
	if !strings.Contains(response.Body.String(), `"not_before":"2026-09-06T12:00:30Z"`) {
		t.Fatalf("response omitted schedule: %s", response.Body.String())
	}
}

func TestNewFleetHandlerRequiresBothProviders(t *testing.T) {
	if _, err := NewFleetHandler(nil, &stubDispatchProvider{}); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("missing registry provider = %v", err)
	}
	if _, err := NewFleetHandler(&stubFleetProvider{}, nil); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("missing dispatch provider = %v", err)
	}
	handler, err := NewFleetHandler(&stubFleetProvider{}, &stubDispatchProvider{})
	if err != nil || handler == nil {
		t.Fatalf("fleet handler = %v, %v", handler, err)
	}
	if FleetRoutePrefix != "/api/v1/fleet/" {
		t.Fatalf("fleet route prefix = %q", FleetRoutePrefix)
	}
}

type stubDispatchProvider struct {
	ambiguity      fleet.AmbiguityRequest
	completion     fleet.CompletionRequest
	extend         fleet.ExtendRequest
	resolver       auth.Resolver
	enqueued       bool
	actionID       []byte
	claimCount     int
	claimRequest   fleet.ClaimRequest
	preparedRecord *fleet.ActionRecord
	materialRecord *fleet.ActionRecord
	notBefore      time.Time
}

func (p *stubDispatchProvider) Enqueue(ctx context.Context, request fleet.EnqueueRequest) (fleet.ActionRecord, error) {
	if _, err := p.resolver.Resolve(ctx); err != nil {
		return fleet.ActionRecord{}, err
	}
	p.enqueued = true
	p.actionID = append([]byte(nil), request.ID...)
	p.notBefore = request.NotBefore
	return fleet.ActionRecord{
		ID: request.ID, Version: 1, State: fleet.DispatchQueued,
		AgentID: request.AgentID, AgentGeneration: request.AgentGeneration,
		Capability: request.Capability, Action: request.Action,
		RequestID: request.Context.RequestID, CorrelationID: request.Context.CorrelationID,
		Deadline: request.Context.Deadline, NotBefore: request.NotBefore,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}, nil
}

func (p *stubDispatchProvider) Claim(_ context.Context, request fleet.ClaimRequest) (fleet.ActionRecord, error) {
	p.claimCount++
	p.claimRequest = request
	if p.preparedRecord != nil {
		return *p.preparedRecord, nil
	}
	return fleet.ActionRecord{}, nil
}
func (p *stubDispatchProvider) ClaimMaterial(context.Context, fleet.RecoveryClaimRequest) (fleet.ActionRecord, error) {
	if p.materialRecord != nil {
		return *p.materialRecord, nil
	}
	return fleet.ActionRecord{}, nil
}
func (*stubDispatchProvider) PrepareClaim(context.Context, fleet.PrepareClaimRequest) (fleet.ActionRecord, error) {
	return fleet.ActionRecord{}, nil
}
func (p *stubDispatchProvider) CompleteClaim(
	_ context.Context, request fleet.CompletionRequest,
) (fleet.ActionRecord, error) {
	p.completion = request
	return fleet.ActionRecord{}, nil
}
func (*stubDispatchProvider) Cancel(context.Context, fleet.CancelRequest) (fleet.ActionRecord, error) {
	return fleet.ActionRecord{}, nil
}
func (*stubDispatchProvider) Status(context.Context, fleet.StatusRequest) (fleet.ActionRecord, error) {
	return fleet.ActionRecord{}, nil
}
func (*stubDispatchProvider) Pull(context.Context, fleet.PullActionsRequest) (fleet.ActionPage, error) {
	return fleet.ActionPage{}, nil
}
func (*stubDispatchProvider) Invoke(context.Context, fleet.InvokeRequest) (fleet.ActionRecord, error) {
	return fleet.ActionRecord{}, nil
}

func (p *stubDispatchProvider) ExtendClaim(
	_ context.Context, request fleet.ExtendRequest,
) (fleet.ActionRecord, error) {
	p.extend = request
	return fleet.ActionRecord{}, nil
}

func (p *stubDispatchProvider) ReportAmbiguity(
	_ context.Context, request fleet.AmbiguityRequest,
) (fleet.ActionRecord, error) {
	p.ambiguity = request
	return fleet.ActionRecord{}, nil
}

// Prepared claims and ordinary action projections never expose executable
// material. The claimant receives it only through the separate Material route.
func TestFleetActionWireWithholdsExecutableMaterial(t *testing.T) {
	preparedAt := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	record := fleet.ActionRecord{ID: []byte("action-id"), Version: 3,
		State: fleet.DispatchClaimed, Input: json.RawMessage(`{"secret":"value"}`),
		ExecutorKey: []byte("private-key"), RecoveryCommitment: &fleet.RecoveryCommitment{
			Version: 1, Mode: fleet.RecoveryKey, PreparedFence: 1, PreparedAt: preparedAt,
			RequestDigest: [32]byte{1}, TargetScopeDigest: [32]byte{2},
		}}
	wire := encodeFleetAction(record)
	if len(wire.Input) != 0 || wire.ExecutorKey != "" {
		t.Fatalf("ordinary action projection exposed material: %+v", wire)
	}
	if wire.RecoveryCommitment == nil || wire.RecoveryCommitment.Version != 1 ||
		wire.RecoveryCommitment.Mode != "key" || wire.RecoveryCommitment.PreparedFence != 1 ||
		!wire.RecoveryCommitment.PreparedAt.Equal(preparedAt) {
		t.Fatalf("commitment status missing: %+v", wire.RecoveryCommitment)
	}
	encoded, _ := json.Marshal(wire)
	if bytes.Contains(encoded, []byte("request_digest")) ||
		bytes.Contains(encoded, []byte("target_scope_digest")) ||
		bytes.Contains(encoded, []byte("secret")) {
		t.Fatalf("public action exposed commitment material: %s", encoded)
	}
}

func TestLegacyRemoteClaimRefusesBeforeProviderMutation(t *testing.T) {
	provider := &stubDispatchProvider{}
	handler, err := NewFleetDispatchHandler(provider)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost,
		"/api/v1/fleet/actions/"+base64.RawURLEncoding.EncodeToString([]byte("action"))+"/claim",
		strings.NewReader(`{}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Shoal-Recovery-Refusal"); got != "legacy_claim" {
		t.Fatalf("legacy refusal code = %q", got)
	}
	if provider.claimCount != 0 {
		t.Fatal("legacy route mutated a claim")
	}
}

func TestRecoveryRefusalHeaderDoesNotPublishCommitmentBytes(t *testing.T) {
	response := httptest.NewRecorder()
	writeFleetRecoveryError(response, &fleet.RecoveryRefusalError{
		Code: fleet.RecoveryScopeDrift, Cause: fleet.ErrActionConflict,
	})
	if response.Code != http.StatusConflict ||
		response.Header().Get("Shoal-Recovery-Refusal") != "scope_drift" {
		t.Fatalf("response = %d %v", response.Code, response.Header())
	}
	if bytes.Contains(response.Body.Bytes(), []byte("target_scope_digest")) ||
		bytes.Contains(response.Body.Bytes(), []byte("request_digest")) {
		t.Fatalf("refusal leaked digests: %s", response.Body.String())
	}
	forged := httptest.NewRecorder()
	writeFleetRecoveryError(forged, &fleet.RecoveryRefusalError{
		Code: fleet.RecoveryRefusal("secret-tenant"), Cause: fleet.ErrActionConflict,
	})
	if got := forged.Header().Get("Shoal-Recovery-Refusal"); got != "" {
		t.Fatalf("arbitrary recovery code reached header: %q", got)
	}
}

func TestPreparedClaimWithholdsMaterialAndMaterialOmitsCommitment(t *testing.T) {
	id, claimID := []byte("action"), []byte("claim")
	record := fleet.ActionRecord{ID: id, ClaimID: claimID, ClaimFence: 1,
		RecoveryProtocolVersion: 1, State: fleet.DispatchClaimed, Version: 2,
		Input: json.RawMessage(`{"secret":"value"}`), ExecutorKey: []byte("secret-key"),
		RecoveryCommitment: &fleet.RecoveryCommitment{Version: 1, Mode: fleet.RecoveryKey,
			RequestDigest: [32]byte{1}, TargetScopeDigest: [32]byte{2}}}
	provider := &stubDispatchProvider{preparedRecord: &record, materialRecord: &record}
	handler, err := NewFleetDispatchHandler(provider)
	if err != nil {
		t.Fatal(err)
	}
	contextValue := fleetRequestContextWire{RequestID: encodeFleetID("request"),
		CorrelationID: encodeFleetID("correlation"), ReasonCode: "worker",
		Deadline: time.Now().UTC().Add(time.Minute)}
	call := func(verb string, body any) string {
		t.Helper()
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost,
			"/api/v1/fleet/actions/"+base64.RawURLEncoding.EncodeToString(id)+"/"+verb,
			bytes.NewReader(encoded))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", verb, response.Code, response.Body.String())
		}
		return response.Body.String()
	}
	claim := call("claim-prepared", fleetClaimWire{Context: contextValue,
		ExpectedVersion: 1, ClaimID: base64.RawURLEncoding.EncodeToString(claimID), Lease: time.Minute})
	if provider.claimCount != 1 || !provider.claimRequest.Prepared {
		t.Fatal("prepared route did not select prepared Claim")
	}
	for _, forbidden := range []string{"secret", "input", "executor_key", "request_digest", "target_scope_digest"} {
		if strings.Contains(claim, forbidden) {
			t.Fatalf("prepared claim exposed %s: %s", forbidden, claim)
		}
	}
	material := call("material", fleetRecoveryClaimWire{Context: contextValue,
		ClaimID: base64.RawURLEncoding.EncodeToString(claimID), ClaimFence: 1})
	if !strings.Contains(material, `"input"`) || !strings.Contains(material, `"executor_key"`) {
		t.Fatalf("material lacks executable fields: %s", material)
	}
	for _, forbidden := range []string{"request_digest", "target_scope_digest", "recovery_commitment"} {
		if strings.Contains(material, forbidden) {
			t.Fatalf("material exposed %s: %s", forbidden, material)
		}
	}
}

// TestTheTeamOverviewDoesNotReadActionInput guards the one dispatch read that
// does not require the reader to be the action's originating principal.
//
// DispatchService.TeamActions documents exactly that, and it projects its own
// view rather than returning fleetActionWire. So the execute grant #437
// introduced is the bound on who reads an action's input only for as long as
// that projection stays a projection. If a field of type fleet.ActionRecord
// ever appears in the response, Input and ExecutorKey ship with it: the record
// carries no JSON tags, so encoding/json emits every exported field under its
// Go name.
//
// This walks the real teamoverview.Response rather than grepping for ".Input".
// The grep it replaced could not see the shape that actually matters — the
// record marshalled whole, which names no field at all — and pkg/explorer/mcp
// is the standing proof that the shape occurs: FleetActionToolResult embeds
// fleet.ActionRecord and so already emits both values to its caller.
func TestTheTeamOverviewDoesNotReadActionInput(t *testing.T) {
	record := reflect.TypeOf(fleet.ActionRecord{})
	visited := map[reflect.Type]bool{}
	var walk func(path string, carrier reflect.Type)
	walk = func(path string, carrier reflect.Type) {
		for carrier.Kind() == reflect.Pointer || carrier.Kind() == reflect.Slice ||
			carrier.Kind() == reflect.Array || carrier.Kind() == reflect.Map {
			carrier = carrier.Elem()
		}
		if carrier == record {
			t.Errorf("%s has type fleet.ActionRecord. The team overview is the "+
				"one dispatch read that does not require the reader to be the "+
				"action's principal, and that type has no JSON tags, so "+
				"returning it whole discloses one principal's request "+
				"parameters and its target-facing idempotency key to every "+
				"reader of the team's overview", path)
			return
		}
		if carrier.Kind() != reflect.Struct || visited[carrier] {
			return
		}
		visited[carrier] = true
		for i := range carrier.NumField() {
			field := carrier.Field(i)
			if field.PkgPath != "" {
				continue // unexported, so encoding/json never emits it
			}
			name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
			if name == "" {
				name = field.Name
			}
			if name == "input" || name == "executor_key" ||
				field.Name == "Input" || field.Name == "ExecutorKey" {
				t.Errorf("%s.%s is named %q, so the team overview projects an "+
					"action's input or its executor key to a reader who need "+
					"not be the action's principal", path, field.Name, name)
			}
			walk(path+"."+field.Name, field.Type)
		}
	}
	walk("teamoverview.Response", reflect.TypeOf(teamoverview.Response{}))
	if len(visited) < 2 {
		t.Fatalf("walked %d struct types, so this guard checks nothing", len(visited))
	}
}

// TestTheAmbiguityRouteDecodesAReportAndCommits covers the HTTP seam for #438,
// including the thing that would be silently wrong: whether the route is
// registered as commit-bearing.
//
// It writes to the record before its response is encoded, so an over-budget
// response must be reported as an indeterminate commit rather than a clean
// failure. On a route whose entire purpose is recording an ambiguity, telling
// the caller nothing happened when the report landed would be the worst
// available answer.
func TestTheAmbiguityRouteDecodesAReportAndCommits(t *testing.T) {
	if !requestMayCommit(
		http.MethodPost, "/api/v1/fleet/actions/abc/ambiguity") {
		t.Fatal("the ambiguity route is not commit-bearing, so an over-budget " +
			"response is reported as a plain failure after the report landed")
	}

	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	authority, _ := auth.NewAuthorityWithClock(func() time.Time { return now })
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "subject", Actor: "actor", AuthorizationDomain: []byte("domain"),
		AllowedOperations:  []auth.Operation{auth.OperationDispatch},
		PermittedSourceIDs: [][]byte{[]byte("source")},
		PermittedPolicyIDs: [][]byte{[]byte("policy")}, PolicyGeneration: 1,
		AuthenticationExpires: now.Add(time.Hour), RequestID: "request",
		CorrelationID: "correlation",
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewAuthenticatedHandler(&stubWorkspaceService{},
		AuthenticatorFunc(func(*http.Request) (auth.Decision, error) {
			return decision, nil
		}),
		authority.Binder(), "example.test")
	if err != nil {
		t.Fatal(err)
	}
	provider := &stubDispatchProvider{resolver: authority.Resolver()}
	if err := handler.MountFleetDispatch(provider); err != nil {
		t.Fatal(err)
	}

	actionID := []byte{'a', 0, 255}
	body, _ := json.Marshal(fleetAmbiguityWire{
		Context: fleetRequestContextWire{
			RequestID: encodeFleetID("request"), ReasonCode: "operator_request",
			CorrelationID: encodeFleetID("correlation"),
			Deadline:      now.Add(time.Minute),
		},
		ExpectedVersion: 7, ClaimFence: 3,
		Outcome: "outcome_unknown",
		Target:  "payments.example.test", Reference: "ch_1A2b3C",
	})
	request := httptest.NewRequest(http.MethodPost,
		"http://example.test/api/v1/fleet/actions/"+
			base64.RawURLEncoding.EncodeToString(actionID)+"/ambiguity",
		bytes.NewReader(body))
	request.Host = "example.test"
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}

	got := provider.ambiguity
	if !bytes.Equal(got.ID, actionID) {
		t.Fatalf("action ID = %x, want %x", got.ID, actionID)
	}
	if got.ExpectedVersion != 7 || got.ClaimFence != 3 ||
		got.Outcome != fleet.AmbiguityOutcomeUnknown ||
		got.Target != "payments.example.test" || got.Reference != "ch_1A2b3C" {
		t.Fatalf("the route lost part of the report: %+v", got)
	}
}

// TestTheActionWireCarriesReportsButNotTheClaimHistory pins what #438 does and
// does not publish.
//
// The reports accompany EffectPossible, because the flag says an effect may
// have happened and the reports say what was attempted — an operator reading
// one without the other has the question and not the evidence.
//
// The claim history is deliberately absent. It exists so the service can
// recognise a lapsed claimant; publishing it would hand every execute-holder in
// the scope a list of which principals have held a record and under which
// fences, which is an identity disclosure the route has no need to make.
func TestTheActionWireCarriesReportsButNotTheClaimHistory(t *testing.T) {
	record := fleet.ActionRecord{
		ID: []byte("action"), Version: 4, State: fleet.DispatchClaimed,
		EffectPossible: true,
		ClaimHistory: []fleet.ClaimHolder{{
			Subject: "previous-holder", Actor: "previous-actor",
			ClaimID: []byte("previous-claim"), ClaimFence: 1,
		}},
		AmbiguityReports: []fleet.AmbiguityReport{{
			ClaimFence: 1, Outcome: fleet.AmbiguityOutcomeUnknown,
			Target: "payments.example.test", Reference: "ch_1A2b3C",
			Subject: "worker", Actor: "worker-actor",
		}},
	}

	wire := encodeFleetAction(record)
	if len(wire.AmbiguityReports) != 1 {
		t.Fatalf("the wire drops the reports an operator needs: %+v", wire)
	}
	report := wire.AmbiguityReports[0]
	if report.Reference != "ch_1A2b3C" || report.Outcome != "outcome_unknown" ||
		report.ClaimFence != 1 {
		t.Fatalf("the report lost its content on the wire: %+v", report)
	}

	encoded, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{
		"previous-holder", "previous-actor", "previous-claim",
		"claim_history", "ClaimHistory",
	} {
		if strings.Contains(string(encoded), absent) {
			t.Fatalf("the wire discloses the claim history (%q), which tells "+
				"every execute-holder in the scope which principals have held "+
				"this record: %s", absent, encoded)
		}
	}
}

// TestTheCompletionRouteCarriesTheClaimFence is the seam the fence fix was
// missing, and without it the whole of #438's stranding fix was unreachable.
//
// completeClaim binds a completion to its claim generation when a fence is
// supplied and falls back to comparing the record version exactly when none is
// — the strandable branch, kept only for a caller written against the older
// contract. fleetCompletionWire had no claim_fence field and the handler never
// set one, so *every* remote worker took the fallback. The service-side fix
// shipped with no way for the only surface that can file an ambiguity report to
// use it.
//
// Worse than unreachable: a worker that tried to send the fence it was handed
// on /claim got a 400, because decodeRequest sets DisallowUnknownFields. So the
// correct client behaviour was the one the server rejected. That second
// assertion is here because the first would pass against a wire that accepted
// the field and discarded it.
func TestTheCompletionRouteCarriesTheClaimFence(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	authority, _ := auth.NewAuthorityWithClock(func() time.Time { return now })
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "subject", Actor: "actor", AuthorizationDomain: []byte("domain"),
		AllowedOperations:  []auth.Operation{auth.OperationInvoke},
		PermittedSourceIDs: [][]byte{[]byte("source")},
		PermittedPolicyIDs: [][]byte{[]byte("policy")}, PolicyGeneration: 1,
		AuthenticationExpires: now.Add(time.Hour), RequestID: "request",
		CorrelationID: "correlation",
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewAuthenticatedHandler(&stubWorkspaceService{},
		AuthenticatorFunc(func(*http.Request) (auth.Decision, error) {
			return decision, nil
		}),
		authority.Binder(), "example.test")
	if err != nil {
		t.Fatal(err)
	}
	provider := &stubDispatchProvider{resolver: authority.Resolver()}
	if err := handler.MountFleetDispatch(provider); err != nil {
		t.Fatal(err)
	}

	actionID := []byte{'a', 0, 255}
	route := "http://example.test/api/v1/fleet/actions/" +
		base64.RawURLEncoding.EncodeToString(actionID) + "/complete"
	contextWire := fleetRequestContextWire{
		RequestID: encodeFleetID("request"), ReasonCode: "operator_request",
		CorrelationID: encodeFleetID("correlation"),
		Deadline:      now.Add(time.Minute),
	}

	body, _ := json.Marshal(fleetCompletionWire{
		Context: contextWire, ExpectedVersion: 7, ClaimFence: 3,
		ClaimID: base64.RawURLEncoding.EncodeToString([]byte("claim")),
		Output:  json.RawMessage(`{"ok":true}`),
	})
	request := httptest.NewRequest(http.MethodPost, route, bytes.NewReader(body))
	request.Host = "example.test"
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if got := provider.completion.ClaimFence; got != 3 {
		t.Fatalf("the route dropped the claim fence (got %d): every remote "+
			"worker then takes completeClaim's strandable exact-version "+
			"branch, so #438's stranding fix never applies over HTTP", got)
	}

	// And the field must be accepted by name, not merely tolerated by a struct
	// that happens to have it. DisallowUnknownFields turns an unrecognised
	// claim_fence into a 400, which is what a correct worker would have hit.
	raw := `{"context":` + mustJSON(t, contextWire) + `,` +
		`"expected_version":7,"claim_fence":5,"claim_id":"` +
		base64.RawURLEncoding.EncodeToString([]byte("claim")) + `"}`
	request = httptest.NewRequest(
		http.MethodPost, route, bytes.NewReader([]byte(raw)))
	request.Host = "example.test"
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("a body naming claim_fence was refused (status=%d): %s",
			response.Code, response.Body.String())
	}
	if got := provider.completion.ClaimFence; got != 5 {
		t.Fatalf("claim_fence decoded to %d, want 5", got)
	}
}

// TestTheExtendRouteCarriesTheClaimFence is the renewal half of what
// TestTheCompletionRouteCarriesTheClaimFence pins on completion.
//
// ExtendRequest.ClaimFence is what lets a live holder renew after a write that
// moved the version and left its claim alone — a displaced holder's ambiguity
// report being the case it exists for. A service that accepts the fence with
// no surface able to send it leaves every remote worker on the exact-version
// branch, which is the stranding this fixes, and decodeRequest's
// DisallowUnknownFields turns the only correct client's body into a 400.
//
// Driven as raw JSON rather than through the wire struct, so the field must be
// accepted *by name*: marshalling fleetExtendWire would pass whatever the tag
// says, including a wrong one.
func TestTheExtendRouteCarriesTheClaimFence(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	authority, _ := auth.NewAuthorityWithClock(func() time.Time { return now })
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "subject", Actor: "actor", AuthorizationDomain: []byte("domain"),
		AllowedOperations:  []auth.Operation{auth.OperationInvoke},
		PermittedSourceIDs: [][]byte{[]byte("source")},
		PermittedPolicyIDs: [][]byte{[]byte("policy")}, PolicyGeneration: 1,
		AuthenticationExpires: now.Add(time.Hour), RequestID: "request",
		CorrelationID: "correlation",
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewAuthenticatedHandler(&stubWorkspaceService{},
		AuthenticatorFunc(func(*http.Request) (auth.Decision, error) {
			return decision, nil
		}),
		authority.Binder(), "example.test")
	if err != nil {
		t.Fatal(err)
	}
	provider := &stubDispatchProvider{resolver: authority.Resolver()}
	if err := handler.MountFleetDispatch(provider); err != nil {
		t.Fatal(err)
	}

	actionID := []byte{'a', 0, 255}
	route := "http://example.test/api/v1/fleet/actions/" +
		base64.RawURLEncoding.EncodeToString(actionID) + "/extend"
	contextWire := fleetRequestContextWire{
		RequestID: encodeFleetID("request"), ReasonCode: "operator_request",
		CorrelationID: encodeFleetID("correlation"),
		Deadline:      now.Add(time.Minute),
	}

	raw := `{"context":` + mustJSON(t, contextWire) + `,` +
		`"claim_fence":4,"claim_id":"` +
		base64.RawURLEncoding.EncodeToString([]byte("claim")) + `",` +
		`"lease":60000000000}`
	request := httptest.NewRequest(
		http.MethodPost, route, bytes.NewReader([]byte(raw)))
	request.Host = "example.test"
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("a body naming claim_fence was refused (status=%d): %s",
			response.Code, response.Body.String())
	}
	if got := provider.extend.ClaimFence; got != 4 {
		t.Fatalf("claim_fence decoded to %d, want 4: without it every remote "+
			"worker stays on the exact-version branch, and a renewal after "+
			"an unrelated write ends a claim that never changed", got)
	}
	// A fence alone is a complete renewal request, so the route must not
	// require a version alongside it.
	if got := provider.extend.ExpectedVersion; got != 0 {
		t.Fatalf("expected_version decoded to %d from a body that omitted "+
			"it", got)
	}
}

// TestTheCompletionRouteCarriesTheEffectedVolume is #427 on this route, and
// it exists for the reason the claim-fence test above exists.
//
// applyExecutionResult validates ExecutionResult.Effected, and this is the
// route the effects gateway completes through — the component that performs
// real external effects, so the one most likely to fail after a partial one.
// A field the service validates with no surface able to set it is #435's
// shape: decodeRequest refuses unknown fields, so the only client that could
// use it would get a 400.
//
// Driven as raw JSON rather than through the wire struct, so the field has to
// be accepted *by name*. Marshalling the struct would pass whatever the tag
// says, including a tag nothing sends.
func TestTheCompletionRouteCarriesTheEffectedVolume(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "subject", Actor: "actor",
		AuthorizationDomain: []byte("domain"),
		AllowedOperations:   []auth.Operation{auth.OperationInvoke},
		PermittedSourceIDs:  [][]byte{[]byte("source")},
		PermittedPolicyIDs:  [][]byte{[]byte("policy")}, PolicyGeneration: 1,
		AuthenticationExpires: now.Add(time.Hour), RequestID: "request",
		CorrelationID: "correlation",
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewAuthenticatedHandler(&stubWorkspaceService{},
		AuthenticatorFunc(func(*http.Request) (auth.Decision, error) {
			return decision, nil
		}),
		authority.Binder(), "example.test")
	if err != nil {
		t.Fatal(err)
	}
	provider := &stubDispatchProvider{resolver: authority.Resolver()}
	if err := handler.MountFleetDispatch(provider); err != nil {
		t.Fatal(err)
	}

	actionID := []byte{'a', 0, 255}
	route := "http://example.test/api/v1/fleet/actions/" +
		base64.RawURLEncoding.EncodeToString(actionID) + "/complete"
	contextWire := fleetRequestContextWire{
		RequestID: encodeFleetID("request"), ReasonCode: "operator_request",
		CorrelationID: encodeFleetID("correlation"),
		Deadline:      now.Add(time.Minute),
	}
	post := func(t *testing.T, effected string) {
		t.Helper()
		raw := `{"context":` + mustJSON(t, contextWire) + `,` +
			`"expected_version":7,"claim_fence":3,"failed":true,` +
			`"error_code":"response_truncated","claim_id":"` +
			base64.RawURLEncoding.EncodeToString([]byte("claim")) + `"` +
			effected + `}`
		request := httptest.NewRequest(
			http.MethodPost, route, bytes.NewReader([]byte(raw)))
		request.Host = "example.test"
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("a body naming effected was refused (status=%d): %s; "+
				"a worker that reports how much escaped then cannot complete "+
				"at all", response.Code, response.Body.String())
		}
	}

	post(t, `,"effected":{"bytes":2097152,"chunks":64}`)
	if got := provider.completion.Result.Effected; got !=
		(fleet.EffectedVolume{Bytes: 2097152, Chunks: 64}) {
		t.Fatalf("the route dropped the effected volume (got %#v): the "+
			"service validates this field and nothing could set it", got)
	}

	// Absent means nothing left, which is what a worker written before this
	// field existed sends. It must still complete, and must not acquire a
	// volume it did not report.
	post(t, "")
	if got := provider.completion.Result.Effected; !got.Zero() {
		t.Fatalf("a completion with no effected field got %#v", got)
	}

	// And the record comes back carrying it, so a worker that lost its
	// response reads what was recorded rather than re-deriving it — which it
	// cannot, since the field is write-once and its own retry is compared
	// against the stored value.
	encoded := encodeFleetAction(fleet.ActionRecord{
		ID: actionID, State: fleet.DispatchFailed,
		ErrorCode: "response_truncated",
		Effected:  fleet.EffectedVolume{Bytes: 50},
	})
	body, err := json.Marshal(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte(`"effected":{"bytes":50}`)) {
		t.Fatalf("a failed record's volume is not returned: %s", body)
	}
	// Omitted when nothing left, so an unaffected record's bytes are
	// unchanged from before this field existed.
	clean, err := json.Marshal(encodeFleetAction(fleet.ActionRecord{
		ID: actionID, State: fleet.DispatchSucceeded,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(clean, []byte("effected")) {
		t.Fatalf("a record with no volume names the field anyway: %s", clean)
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// TestFleetDispatchErrorMarksEveryIndeterminateJoin pins the whole arm order,
// because reasoning about it in a comment is exactly what failed.
//
// fleetDispatchError is a switch, so the first matching arm wins, and
// applyExecutionResult does not raise the ambiguity sentinel alone — it joins
// it with whatever it found when it re-read the record after the effect. The
// first version of the indeterminate arm sat below the not-found and conflict
// arms, which shadowed it:
//
//	Join(ErrExecutionAmbiguous, ErrClaimLost)      → 409 conflict, unmarked
//	Join(ErrExecutionAmbiguous, ErrActionNotFound) → 404, unmarked
//	Join(ErrActionCommitted, ErrActionTerminal)    → 409 conflict, unmarked
//
// A 409 is worse than the bare 503 it replaced: it reads as "your request did
// not apply", so a worker retries an external effect that may already have
// happened. The arm's own comment reasoned only about the arm below it.
//
// Every join the service actually constructs is a row here, so adding a new
// one without deciding its marking fails this test rather than shipping.
func TestFleetDispatchErrorMarksEveryIndeterminateJoin(t *testing.T) {
	for _, probe := range []struct {
		name          string
		err           error
		indeterminate bool
		code          shoal.ErrorCode
	}{
		// Raised by applyExecutionResult after the effect. All three must be
		// marked: the effect may have occurred and Shoal cannot tell.
		{"ambiguous alone", fleet.ErrExecutionAmbiguous,
			true, shoal.ErrorUnavailable},
		{"ambiguous joined with a lost claim",
			errors.Join(fleet.ErrExecutionAmbiguous, fleet.ErrClaimLost),
			true, shoal.ErrorUnavailable},
		{"ambiguous joined with a missing record",
			errors.Join(fleet.ErrExecutionAmbiguous, fleet.ErrActionNotFound),
			true, shoal.ErrorUnavailable},
		{"ambiguous joined with a recorder failure",
			errors.Join(fleet.ErrExecutionAmbiguous, fleet.ErrRecordingUnavailable),
			true, shoal.ErrorUnavailable},
		// The transition was durably written and only its publication failed.
		{"committed alone", fleet.ErrActionCommitted,
			true, shoal.ErrorUnavailable},
		{"committed joined with a terminal record",
			errors.Join(fleet.ErrActionCommitted, fleet.ErrActionTerminal),
			true, shoal.ErrorUnavailable},
		// Nothing committed. A clean refusal the caller retries, and marking
		// it would tell a caller to reconcile against a record that does not
		// exist.
		{"a lost claim alone", fleet.ErrClaimLost,
			false, shoal.ErrorConflict},
		{"a conflict alone", fleet.ErrActionConflict,
			false, shoal.ErrorConflict},
		{"cancel refused for uncertain prior claim", fleet.ErrCancelClaimUncertain,
			false, shoal.ErrorConflict},
		{"a terminal record alone", fleet.ErrActionTerminal,
			false, shoal.ErrorConflict},
		{"a missing record alone", fleet.ErrActionNotFound,
			false, shoal.ErrorNotFound},
		{"a recorder failure alone", fleet.ErrRecordingUnavailable,
			false, shoal.ErrorUnavailable},
		{"approval required", fleet.ErrApprovalRequired,
			false, shoal.ErrorConflict},
		// A pre-commit refusal: raised by the gate before any write, never
		// joined with an indeterminate sentinel, so its 409 sits above the
		// indeterminate arm without hiding a commit.
		{"attestation required", fleet.ErrAttestationRequired,
			false, shoal.ErrorConflict},
		{"attestation required as the service returns it",
			shoal.WrapError(shoal.ErrorConflict, "action requires a current executor attestation", fleet.ErrAttestationRequired),
			false, shoal.ErrorConflict},
		// A store failure: nothing written, a clean unavailable.
		{"attestation store unavailable", fleet.ErrAttestationUnavailable,
			false, shoal.ErrorUnavailable},
		{"attestation store unavailable as the service returns it",
			shoal.WrapError(shoal.ErrorUnavailable, "executor attestation is unavailable", fleet.ErrAttestationUnavailable),
			false, shoal.ErrorUnavailable},
	} {
		t.Run(probe.name, func(t *testing.T) {
			mapped := fleetDispatchError(probe.err)
			if got := explorer.IsIndeterminateCommit(
				mapped); got != probe.indeterminate {
				t.Fatalf("indeterminate = %v, want %v: a caller distinguishes "+
					"\"retry, nothing happened\" from \"stop, something may "+
					"have happened\" on exactly this flag", got, probe.indeterminate)
			}
			if got := primaryErrorCode(mapped); got != probe.code {
				t.Fatalf("code = %q, want %q", got, probe.code)
			}
		})
	}
	// The approver mapping moved (#451). Approval sentinels are mapped by
	// fleetApprovalError, which matches them above its fall-through to
	// fleetDispatchError. Nothing was written — the row stays approved and
	// no action exists — so each form is a clean 409, never marked: bare, as
	// the service raises it, and joined.
	for _, probe := range []struct {
		name string
		err  error
	}{
		{"approver mapping moved", fleet.ErrApproverMappingMoved},
		{"approver mapping moved, as the service raises it", shoal.WrapError(
			shoal.ErrorConflict,
			"the approver mapping a decision was made under is no longer in force",
			fleet.ErrApproverMappingMoved)},
		{"approver mapping moved, joined", errors.Join(
			errors.New("context"), fleet.ErrApproverMappingMoved)},
	} {
		t.Run(probe.name, func(t *testing.T) {
			mapped := fleetApprovalError(probe.err)
			if explorer.IsIndeterminateCommit(mapped) {
				t.Fatal("a moved approver mapping is marked indeterminate; " +
					"nothing was committed")
			}
			if got := primaryErrorCode(mapped); got != shoal.ErrorConflict {
				t.Fatalf("code = %q, want conflict", got)
			}
			if !errors.Is(mapped, fleet.ErrApproverMappingMoved) {
				t.Fatal("the mapped error lost its sentinel")
			}
		})
	}
	// The identity scheme moved (#526). Its arm sits above the approver
	// mapping's and the generic conflict arm. Nothing was written — the
	// request stays as it was and can only expire — so it is a clean 409,
	// never marked, in every form; and it keeps its own sentinel, even when
	// joined with a mapping move it would otherwise be read as.
	for _, probe := range []struct {
		name string
		err  error
	}{
		{"identity scheme moved", fleet.ErrIdentitySchemeMoved},
		{"identity scheme moved, as the service raises it", shoal.WrapError(
			shoal.ErrorConflict,
			"the request was made under an identity scheme that is no longer in force",
			fleet.ErrIdentitySchemeMoved)},
		{"identity scheme moved, joined", errors.Join(
			errors.New("context"), fleet.ErrIdentitySchemeMoved)},
		{"identity scheme moved, joined with a mapping move", errors.Join(
			fleet.ErrApproverMappingMoved, fleet.ErrIdentitySchemeMoved)},
	} {
		t.Run(probe.name, func(t *testing.T) {
			mapped := fleetApprovalError(probe.err)
			if explorer.IsIndeterminateCommit(mapped) {
				t.Fatal("a moved identity scheme is marked indeterminate; " +
					"nothing was committed")
			}
			if got := primaryErrorCode(mapped); got != shoal.ErrorConflict {
				t.Fatalf("code = %q, want conflict", got)
			}
			if !errors.Is(mapped, fleet.ErrIdentitySchemeMoved) {
				t.Fatal("the mapped error lost its sentinel")
			}
			// The arm that answered is the scheme's: the transport message
			// names the scheme, not the mapping or a generic conflict.
			if !strings.Contains(mapped.Error(), "identity scheme") {
				t.Fatalf("answered by another arm: %v", mapped)
			}
		})
	}
}

func TestCancelPossibleEffectRefusalIsAnUnmarkedHTTPConflict(t *testing.T) {
	response := serveDispatchGolden(t, dispatchGoldenCase{
		path: dispatchGoldenAction + "cancel",
		body: `{"context":` + dispatchGoldenContext.String() +
			`,"expected_version":3,"mutation_key":"Y2FuY2Vs"}`,
		provider: recordingDispatchProvider{err: fleet.ErrCancelClaimUncertain},
	})
	if !strings.Contains(response, "provider calls: 1\n") ||
		!strings.Contains(response, "status: 409\n") ||
		!strings.Contains(response, "shoal-commit-outcome: \n") {
		t.Fatalf("cancel refusal transport contract:\n%s", response)
	}
}
