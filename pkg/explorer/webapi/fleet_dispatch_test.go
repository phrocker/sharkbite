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
	"os"
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
		Input: json.RawMessage(`{"value":1}`),
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
	ambiguity  fleet.AmbiguityRequest
	completion fleet.CompletionRequest
	extend     fleet.ExtendRequest
	resolver   auth.Resolver
	enqueued   bool
	actionID   []byte
}

func (p *stubDispatchProvider) Enqueue(ctx context.Context, request fleet.EnqueueRequest) (fleet.ActionRecord, error) {
	if _, err := p.resolver.Resolve(ctx); err != nil {
		return fleet.ActionRecord{}, err
	}
	p.enqueued = true
	p.actionID = append([]byte(nil), request.ID...)
	return fleet.ActionRecord{
		ID: request.ID, Version: 1, State: fleet.DispatchQueued,
		AgentID: request.AgentID, AgentGeneration: request.AgentGeneration,
		Capability: request.Capability, Action: request.Action,
		RequestID: request.Context.RequestID, CorrelationID: request.Context.CorrelationID,
		Deadline: request.Context.Deadline, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}, nil
}

func (*stubDispatchProvider) Claim(context.Context, fleet.ClaimRequest) (fleet.ActionRecord, error) {
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

// TestFleetActionWireCarriesTheWorkAndItsIdempotencyKey pins the two fields
// whose absence made an out-of-process worker impossible (#435), and pins that
// they ride the claim response alone.
//
// A worker could pull an action, claim it under a fence, and complete it
// without ever receiving the parameters of the operation: Input was on the
// enqueue wire, validated against the action's InputSchema, and stored on the
// record, and this encoder dropped it. ExecutorKey was dropped the same way,
// which forced each worker to invent its own idempotency key — and a
// hand-written digest that omits length prefixes is non-injective, so two
// surfaces can be made to collide and a collision makes a provider return a
// cached success without performing the effect.
//
// Both are asserted byte-for-byte rather than for presence, because the whole
// value of Input is that it is exactly what the enqueuer supplied, and the
// whole value of ExecutorKey is that it equals the key an in-process executor
// receives for the same action.
func TestFleetActionWireCarriesTheWorkAndItsIdempotencyKey(t *testing.T) {
	const input = `{"method":"POST","path":"/v1/messages","body":{"text":"ship it"}}`
	// Chosen so the two base64 alphabets actually differ. An earlier version of
	// this test used bytes that encode identically under StdEncoding and
	// RawURLEncoding, so the alphabet assertion below could not fail and a
	// mutation swapping the encoder survived. These bytes produce "+/++" in the
	// standard alphabet and "-_--" in the URL one.
	executorKey := []byte{0xfb, 0xff, 0xbe, 0xfb, 0xff, 0xbe}
	record := fleet.ActionRecord{
		ID:          []byte("action-id"),
		Version:     3,
		State:       fleet.DispatchClaimed,
		Capability:  "effects.http",
		Action:      "post",
		Input:       json.RawMessage(input),
		ExecutorKey: executorKey,
	}

	wire := encodeClaimedFleetAction(record)

	if string(wire.Input) != input {
		t.Fatalf("input = %s, want it unchanged: a worker that does not receive "+
			"this has nothing to perform", wire.Input)
	}
	if want := base64.RawURLEncoding.EncodeToString(executorKey); wire.ExecutorKey != want {
		t.Fatalf("executor_key = %q, want %q", wire.ExecutorKey, want)
	}
	// Unpadded base64url, like every other opaque identity on this wire. A
	// padded or standard-alphabet spelling would be a second spelling of one
	// identity, and this value is used as a deduplication key at a third party
	// where two spellings are two keys. pkg/explorer/mcp returns the record
	// whole and so emits the padded standard spelling; a worker must therefore
	// compare decoded bytes and never one spelling against the other.
	if strings.ContainsAny(wire.ExecutorKey, "=+/") {
		t.Fatalf("executor_key %q is not unpadded base64url", wire.ExecutorKey)
	}
	// And positively: the URL alphabet's own characters are present, so this
	// asserts the right encoding rather than merely the absence of the wrong
	// one. A test that only checks for absent characters passes against an
	// encoder that emits neither alphabet's distinctive bytes.
	if !strings.ContainsAny(wire.ExecutorKey, "-_") {
		t.Fatalf("executor_key %q does not use the URL alphabet", wire.ExecutorKey)
	}

	// The encoder copies rather than aliases, so a caller mutating the wire
	// cannot reach back into the record it was built from.
	if len(wire.Input) > 0 {
		wire.Input[0] = 'X'
		if record.Input[0] == 'X' {
			t.Fatal("the wire aliases the record's input")
		}
	}

	// The shared encoder carries neither, for the same record. This is the
	// assertion that bounds both the disclosure and the response size: six of
	// the seven routes encoding an action use it, five of those are
	// commit-bearing, and an over-budget response on a commit-bearing route is
	// reported as an indeterminate commit rather than a failure.
	shared := encodeFleetAction(record)
	if len(shared.Input) != 0 {
		t.Fatalf("the shared encoder carries input %s, so every action response "+
			"carries a payload bounded only by MaxActionPayloadBytes", shared.Input)
	}
	if shared.ExecutorKey != "" {
		t.Fatalf("the shared encoder carries executor_key %q", shared.ExecutorKey)
	}
	// Everything else must survive the split, or the claim response would be
	// the only one worth reading.
	shared.Input, shared.ExecutorKey = wire.Input, wire.ExecutorKey
	if !reflect.DeepEqual(shared, wire) {
		t.Fatalf("the claim encoder differs from the shared one beyond the two "+
			"claimant fields:\n claim  = %+v\n shared = %+v", wire, shared)
	}

	// An action with no input encodes nothing rather than a JSON null, because
	// the field is omitempty and a literal null is not an absent value to a
	// client distinguishing the two.
	bare := encodeClaimedFleetAction(fleet.ActionRecord{ID: []byte("x"), Version: 1})
	encoded, err := json.Marshal(bare)
	if err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{`"input"`, `"executor_key"`} {
		if strings.Contains(string(encoded), absent) {
			t.Fatalf("%s is present for a record that has none: %s", absent, encoded)
		}
	}
}

// TestOnlyTheClaimRouteEncodesTheWork pins the call sites, because the bound on
// both the disclosure and the response size is "one record on one route" and
// nothing else in the type system enforces it.
//
// encodeClaimedFleetAction is one line away from being usable anywhere, and the
// cost of using it on the wrong route is not a leak alone. requestMayCommit
// reports enqueue, invoke, claim, complete and cancel as commit-bearing, so
// writeResponse answers an over-budget response there with 503 and
// X-Commit-Outcome: indeterminate. MaxActionPayloadBytes is a fixed 1 MiB and a
// workspace's OutputBytes budget narrows to any non-zero value, so on the
// enqueue echo that would be a successful enqueue reported as an unknown
// outcome, repeatably, for as long as the record existed. On Pull it would be
// up to MaxDispatchListResults — 256 — payloads in one page.
func TestOnlyTheClaimRouteEncodesTheWork(t *testing.T) {
	body, err := os.ReadFile("fleet_dispatch.go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(body)
	// Count call sites, not definitions: the definition's own name and its one
	// internal call to the shared encoder are excluded by requiring the "("
	// and by subtracting the declaration.
	claimed := strings.Count(source, "encodeClaimedFleetAction(") -
		strings.Count(source, "func encodeClaimedFleetAction(")
	if claimed != 1 {
		t.Fatalf("encodeClaimedFleetAction has %d call sites, want exactly 1 "+
			"(the claim route). Every added site is a route whose response "+
			"carries a 1 MiB-bounded payload against a narrowable output "+
			"budget, and five of the seven are commit-bearing", claimed)
	}
	// And it is the claim route specifically. Ordering the handlers by
	// position, the one containing the call must be the claim registration.
	call := strings.Index(source, "writeResponse(w, http.StatusOK, encodeClaimedFleetAction(")
	if call < 0 {
		t.Fatal("no route writes a claimed action; the claimant now gets no input")
	}
	handler := strings.LastIndex(source[:call], `mux.HandleFunc("`)
	if handler < 0 {
		t.Fatal("the claimed-action response is not inside a route registration")
	}
	route := source[handler : handler+strings.Index(source[handler:], "\n")]
	if !strings.Contains(route, "/claim") {
		t.Fatalf("the claimed-action encoder is used by %s, not the claim route", route)
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
