// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package webapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// The dispatch golden fixtures pin the worker-facing wire of the fleet
// dispatch surface exactly: pull, claim, extend, complete, ambiguity and
// status, plus the descriptor resolve the effects gateway starts with. Each
// fixture holds the status, the headers a client branches on, the response
// bytes and what the handler handed the provider. They were captured from the
// handler before its wire types moved to pkg/dispatch/api (#446 slice 4), so
// that move — and any later one — is provably byte-compatible. A change here
// is a wire change and needs its own review; regenerate with
//
//	go test ./pkg/explorer/webapi -run TestFleetDispatchWireGolden -update
//
// Decoding goes through decodeRequest (strictjson: duplicate, case-variant and
// unknown keys and trailing data are refused). The fixtures pin that as it is.
const dispatchGoldenDir = "../../dispatch/api/testdata/wire/server"

// recordingDispatchProvider records exactly what the handler decoded.
type recordingDispatchProvider struct {
	record fleet.ActionRecord
	page   fleet.ActionPage
	err    error
	seen   []string
}

func (p *recordingDispatchProvider) answer(request any) (fleet.ActionRecord, error) {
	p.seen = append(p.seen, goldenGoString(request))
	return p.record, p.err
}

func (p *recordingDispatchProvider) Enqueue(_ context.Context, r fleet.EnqueueRequest) (fleet.ActionRecord, error) {
	return p.answer(r)
}

func (p *recordingDispatchProvider) Claim(_ context.Context, r fleet.ClaimRequest) (fleet.ActionRecord, error) {
	return p.answer(r)
}

func (p *recordingDispatchProvider) Cancel(_ context.Context, r fleet.CancelRequest) (fleet.ActionRecord, error) {
	return p.answer(r)
}

func (p *recordingDispatchProvider) Status(_ context.Context, r fleet.StatusRequest) (fleet.ActionRecord, error) {
	return p.answer(r)
}

func (p *recordingDispatchProvider) Pull(_ context.Context, r fleet.PullActionsRequest) (fleet.ActionPage, error) {
	p.seen = append(p.seen, goldenGoString(r))
	return p.page, p.err
}

func (p *recordingDispatchProvider) Invoke(_ context.Context, r fleet.InvokeRequest) (fleet.ActionRecord, error) {
	return p.answer(r)
}

func (p *recordingDispatchProvider) CompleteClaim(_ context.Context, r fleet.CompletionRequest) (fleet.ActionRecord, error) {
	return p.answer(r)
}

func (p *recordingDispatchProvider) ReportAmbiguity(_ context.Context, r fleet.AmbiguityRequest) (fleet.ActionRecord, error) {
	return p.answer(r)
}

func (p *recordingDispatchProvider) ExtendClaim(_ context.Context, r fleet.ExtendRequest) (fleet.ActionRecord, error) {
	return p.answer(r)
}

// recordingResolveProvider serves only Resolve; the other registry routes are
// not part of the worker's wire.
type recordingResolveProvider struct {
	descriptor fleet.Descriptor
	err        error
	seen       []string
}

func (p *recordingResolveProvider) Register(context.Context, fleet.RegisterRequest) (fleet.Descriptor, error) {
	return fleet.Descriptor{}, errors.New("not served")
}

func (p *recordingResolveProvider) Heartbeat(context.Context, fleet.HeartbeatRequest) (fleet.Descriptor, error) {
	return fleet.Descriptor{}, errors.New("not served")
}

func (p *recordingResolveProvider) Revoke(context.Context, fleet.RevokeRequest) (fleet.Descriptor, error) {
	return fleet.Descriptor{}, errors.New("not served")
}

func (p *recordingResolveProvider) Resolve(_ context.Context, r fleet.ResolveRequest) (fleet.Resolved, error) {
	p.seen = append(p.seen, goldenGoString(r))
	return fleet.Resolved{Descriptor: p.descriptor}, p.err
}

func (p *recordingResolveProvider) List(context.Context, fleet.ListRequest) (fleet.ListPage, error) {
	return fleet.ListPage{}, errors.New("not served")
}

// The canonical bodies. Each is the shape internal/effectsgateway sends, with
// the deadline at a non-UTC offset because the handler does not normalise
// times and a refactor that did would change what reaches the provider.
var (
	dispatchGoldenContext = goldenFields{
		{"request_id", `"cmVxdWVzdA"`},
		{"correlation_id", `"Y29ycmVsYXRpb24"`},
		{"reason_code", `"gateway_claim"`},
		{"reason_detail", `"detail"`},
		{"deadline", `"2026-09-06T14:01:02.5+02:00"`},
	}
	dispatchGoldenPull = goldenFields{
		{"context", dispatchGoldenContext.String()},
		{"after", `"Y3Vyc29y"`},
		{"limit", `10`},
	}
	dispatchGoldenClaim = goldenFields{
		{"context", dispatchGoldenContext.String()},
		{"expected_version", `3`},
		{"claim_id", `"Z3cxfHBvZHxjbGFpbQ"`},
		{"lease", `60000000000`},
	}
	dispatchGoldenExtend = goldenFields{
		{"context", dispatchGoldenContext.String()},
		{"claim_fence", `2`},
		{"claim_id", `"Z3cxfHBvZHxjbGFpbQ"`},
		{"lease", `60000000000`},
	}
	dispatchGoldenComplete = goldenFields{
		{"context", dispatchGoldenContext.String()},
		{"expected_version", `4`},
		{"claim_id", `"Z3cxfHBvZHxjbGFpbQ"`},
		{"claim_fence", `2`},
		{"output", `{"ok":true,"n":1}`},
	}
	dispatchGoldenCompleteFailed = dispatchGoldenComplete.without("output").
					plus("error_code", `"outcome_unknown"`).plus("failed", `true`)
	dispatchGoldenEvidence = `[{"anchor_id":"YW5jaG9y","kind":"document",` +
		`"citation":{"document_id":"ZG9j","revision_id":"cmV2","section_id":"c2Vj",` +
		`"span_id":"c3Bhbg","start":1,"end":9,"start_page":2,"end_page":3},` +
		`"node_ids":["bm9kZQ"],"edge_ids":["ZWRnZQ"],` +
		`"assertions":[{"assertion_id":"YXNz","edge_id":"ZWRnZQ","origin":"extracted"}],` +
		`"visibility":["public"]},` +
		`{"anchor_id":"","kind":"graph","node_ids":[],"visibility":[]}]`
	dispatchGoldenAmbiguity = goldenFields{
		{"context", dispatchGoldenContext.String()},
		{"expected_version", `0`},
		{"claim_fence", `2`},
		{"outcome", `"outcome_unknown"`},
		{"target", `"api.example"`},
		{"reference", `"ch_1"`},
	}
)

var dispatchGoldenWhen = time.Date(2026, 9, 6, 12, 0, 0, 0, time.FixedZone("", 2*3600))

// dispatchGoldenRecord populates every field the wire carries, at mixed
// offsets, so a reordered, retagged or retyped field moves bytes.
func dispatchGoldenRecord(state fleet.DispatchState) fleet.ActionRecord {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i * 7)
	}
	return fleet.ActionRecord{
		ID: []byte{'a', 0, 255}, IdempotencyKey: []byte("never-on-the-wire"),
		Version: 5, State: state, AgentID: "agent", AgentGeneration: 3,
		Capability: "effects.gateway", Action: "charge",
		SourceID: []byte("source"), PolicyID: []byte("policy"), ObjectID: "object",
		Input:  []byte(`{"account":"a1","amount":5}`),
		Output: []byte(`{"reference":"ch_1"}`), ErrorCode: "outcome_unknown",
		ErrorCodeOrigin: fleet.ErrorCodeOriginExecutor,
		Effected:        fleet.EffectedVolume{Bytes: 2048, Chunks: 3},
		Subject:         "subject", Actor: "actor",
		RequestID: "request", CorrelationID: "correlation",
		Deadline:    dispatchGoldenWhen.Add(10 * time.Minute),
		CreatedAt:   dispatchGoldenWhen.UTC(),
		UpdatedAt:   dispatchGoldenWhen.Add(time.Second),
		ExecutorKey: key, ClaimID: []byte("gw1|pod|claim"), ClaimFence: 2,
		ClaimLeaseUntil: dispatchGoldenWhen.Add(time.Minute).UTC(),
		EffectPossible:  true,
		AmbiguityReports: []fleet.AmbiguityReport{{
			ClaimFence: 1, Outcome: fleet.AmbiguityOutcomeUnknown,
			Target: "api.example", Reference: "ch_0",
			Subject: "subject", Actor: "actor", ReportedAt: dispatchGoldenWhen,
		}, {
			ClaimFence: 2, Outcome: fleet.AmbiguityRequestNotSent,
			Subject: "other", Actor: "\x00\xff", ReportedAt: dispatchGoldenWhen.UTC(),
		}},
		EvidenceSnapshotID:   "snapshot",
		EvidenceSnapshotAsOf: dispatchGoldenWhen.Add(-time.Hour),
		Evidence: []fleet.EvidenceRef{{
			AnchorID: "anchor", Kind: interaction.EvidenceDocument,
			Citation: document.Citation{
				DocumentID: "doc", RevisionID: "rev", SectionID: "sec", SpanID: "span",
				Range: document.SourceRange{
					Start: document.SourcePosition{Offset: 1, Page: 2},
					End:   document.SourcePosition{Offset: 9, Page: 3},
				},
			},
			NodeIDs: []shoal.ID{"node"}, EdgeIDs: []shoal.ID{"edge"},
			Assertions: []interaction.AssertionReference{{
				AssertionID: "ass", EdgeID: "edge", Origin: ontology.AssertionOrigin("extracted"),
			}},
			Visibility: []string{"public"},
		}, {
			Kind: interaction.EvidenceGraph,
		}},
	}
}

// dispatchGoldenMinimalRecord is a fresh claim: no output, no reports, no
// evidence, no effected volume — the omitempty paths.
func dispatchGoldenMinimalRecord() fleet.ActionRecord {
	return fleet.ActionRecord{
		ID: []byte("b"), Version: 2, State: fleet.DispatchClaimed,
		AgentID: "agent", AgentGeneration: 1, Capability: "effects.gateway",
		Action: "charge", RequestID: "request",
		Deadline:  dispatchGoldenWhen.UTC().Add(time.Hour),
		CreatedAt: dispatchGoldenWhen.UTC(), UpdatedAt: dispatchGoldenWhen.UTC(),
		ClaimID: []byte("claim"), ClaimFence: 1,
		ClaimLeaseUntil: dispatchGoldenWhen.UTC().Add(time.Minute),
	}
}

func dispatchGoldenDescriptor() fleet.Descriptor {
	return fleet.Descriptor{
		ID: "agent", Generation: 7, Subject: "subject", Actor: "actor",
		AuthorizationDomain: []byte("domain"),
		Scopes:              []fleet.Scope{{SourceID: []byte("source"), PolicyID: []byte("policy")}},
		ExecutorRef:         "gateway",
		Capabilities: []fleet.Capability{{
			Name: "effects.gateway",
			Actions: []fleet.Action{{
				Name: "charge", InputSchema: []byte(`{"type":"object"}`),
				OutputSchema:        []byte(`{"type":"object"}`),
				Effects:             fleet.Effects{fleet.EffectEgressesContent, fleet.EffectMutatesExternal},
				RequiresAttestation: true,
			}, {
				Name: "read", InputSchema: []byte(`{}`), OutputSchema: []byte(`{}`),
				Effects: fleet.Effects{fleet.EffectReadsCorpus}, RequiresApproval: true,
			}, {
				Name: "noop", InputSchema: []byte(`{}`), OutputSchema: []byte(`{}`),
			}},
		}},
		LeaseExpiresAt: dispatchGoldenWhen.Add(time.Hour),
		UpdatedAt:      dispatchGoldenWhen.UTC(),
	}
}

type dispatchGoldenCase struct {
	name        string
	path        string
	contentType string
	body        string
	provider    recordingDispatchProvider
	resolve     *recordingResolveProvider
	budget      uint64
}

const dispatchGoldenAction = "/api/v1/fleet/actions/YQD_/"

func dispatchGoldenCases() []dispatchGoldenCase {
	const (
		pull      = "/api/v1/fleet/actions/pull"
		claim     = dispatchGoldenAction + "claim"
		extend    = dispatchGoldenAction + "extend"
		complete  = dispatchGoldenAction + "complete"
		ambiguity = dispatchGoldenAction + "ambiguity"
		status    = dispatchGoldenAction + "status"
		resolve   = "/api/v1/fleet/agents/YWdlbnQ/resolve"
	)
	claimed := recordingDispatchProvider{record: dispatchGoldenRecord(fleet.DispatchClaimed)}
	minimal := recordingDispatchProvider{record: dispatchGoldenMinimalRecord()}
	succeeded := recordingDispatchProvider{record: dispatchGoldenRecord(fleet.DispatchSucceeded)}
	failed := recordingDispatchProvider{record: dispatchGoldenRecord(fleet.DispatchFailed)}
	page := func(next []byte, records ...fleet.ActionRecord) recordingDispatchProvider {
		return recordingDispatchProvider{page: fleet.ActionPage{Actions: records, Next: next}}
	}
	failing := func(err error) recordingDispatchProvider { return recordingDispatchProvider{err: err} }
	resolved := func() *recordingResolveProvider {
		return &recordingResolveProvider{descriptor: dispatchGoldenDescriptor()}
	}

	type route struct {
		name string
		path string
		body goldenFields
		ok   recordingDispatchProvider
	}
	routes := []route{
		{"pull", pull, dispatchGoldenPull, page([]byte{'n', 0, 255}, dispatchGoldenRecord(fleet.DispatchQueued))},
		{"claim", claim, dispatchGoldenClaim, claimed},
		{"extend", extend, dispatchGoldenExtend, claimed},
		{"complete", complete, dispatchGoldenComplete, succeeded},
		{"ambiguity", ambiguity, dispatchGoldenAmbiguity, claimed},
		{"status", status, dispatchGoldenContext, claimed},
	}

	cases := []dispatchGoldenCase{
		// Encodings: a claim is the one route that carries input and the
		// executor key; every other route leaves both off.
		{name: "claim_minimal_record", path: claim, body: dispatchGoldenClaim.String(), provider: minimal},
		{name: "extend_minimal_record", path: extend, body: dispatchGoldenExtend.String(), provider: minimal},
		{name: "extend_with_expected_version", path: extend,
			body: dispatchGoldenExtend.plus("expected_version", `9`).String(), provider: claimed},
		{name: "extend_without_fence", path: extend,
			body:     dispatchGoldenExtend.without("claim_fence").plus("expected_version", `9`).String(),
			provider: claimed},
		{name: "complete_failed", path: complete, body: dispatchGoldenCompleteFailed.String(), provider: failed},
		{name: "complete_failed_effected", path: complete,
			body:     dispatchGoldenCompleteFailed.plus("effected", `{"bytes":50,"chunks":2}`).String(),
			provider: failed},
		{name: "complete_failed_effected_bytes_only", path: complete,
			body:     dispatchGoldenCompleteFailed.plus("effected", `{"bytes":50}`).String(),
			provider: failed},
		{name: "complete_effected_null", path: complete,
			body: dispatchGoldenCompleteFailed.plus("effected", `null`).String(), provider: failed},
		{name: "complete_without_fence", path: complete,
			body: dispatchGoldenComplete.without("claim_fence").String(), provider: succeeded},
		{name: "complete_failed_false", path: complete,
			body:     dispatchGoldenComplete.plus("failed", `false`).plus("error_code", `""`).String(),
			provider: succeeded},
		{name: "complete_evidence", path: complete,
			body: dispatchGoldenComplete.plus("evidence_snapshot_id", `"c25hcHNob3Q"`).
				plus("evidence_snapshot_as_of", `"2026-09-06T11:00:00+02:00"`).
				plus("evidence", dispatchGoldenEvidence).String(),
			provider: succeeded},
		{name: "complete_evidence_padded_anchor", path: complete,
			body: dispatchGoldenComplete.plus("evidence",
				`[{"anchor_id":"YWI=","kind":"graph","node_ids":[],"visibility":[]}]`).String(),
			provider: succeeded},
		{name: "complete_evidence_unknown_field", path: complete,
			body: dispatchGoldenComplete.plus("evidence",
				`[{"anchor_id":"","kind":"graph","node_ids":[],"visibility":[],"extra":1}]`).String(),
			provider: succeeded},
		{name: "complete_evidence_padded_citation", path: complete,
			body: dispatchGoldenComplete.plus("evidence",
				`[{"anchor_id":"","kind":"document","citation":{"document_id":"YWI=","revision_id":"","start":0,"end":0},"node_ids":[],"visibility":[]}]`).String(),
			provider: succeeded},
		{name: "ambiguity_minimal", path: ambiguity,
			body: dispatchGoldenAmbiguity.without("target").without("reference").String(), provider: claimed},
		{name: "ambiguity_unknown_outcome", path: ambiguity,
			body: dispatchGoldenAmbiguity.with("outcome", `"invented"`).String(), provider: claimed},
		{name: "pull_without_after", path: pull,
			body: dispatchGoldenPull.without("after").String(), provider: page(nil, dispatchGoldenMinimalRecord())},
		{name: "pull_empty_after", path: pull,
			body: dispatchGoldenPull.with("after", `""`).String(), provider: page(nil)},
		{name: "pull_empty_page", path: pull, body: dispatchGoldenPull.String(),
			provider: recordingDispatchProvider{}},
		{name: "pull_padded_after", path: pull,
			body: dispatchGoldenPull.with("after", `"YWI="`).String(), provider: page(nil)},
		{name: "pull_overlong_after", path: pull,
			body:     dispatchGoldenPull.with("after", `"`+strings.Repeat("QUFB", 85)+`QUE"`).String(),
			provider: page(nil)},
		{name: "pull_two_records", path: pull, body: dispatchGoldenPull.String(),
			provider: page([]byte("n"), dispatchGoldenRecord(fleet.DispatchClaimed), dispatchGoldenMinimalRecord())},
		{name: "status_terminal", path: status, body: dispatchGoldenContext.String(), provider: failed},

		// Resolve, served by the registry handler mounted beside dispatch.
		{name: "resolve", path: resolve, body: dispatchGoldenContext.String(), resolve: resolved()},
		{name: "resolve_padded_agent", path: "/api/v1/fleet/agents/YWI=/resolve",
			body: dispatchGoldenContext.String(), resolve: resolved()},
		{name: "resolve_unknown_field", path: resolve,
			body: dispatchGoldenContext.plus("extra", `1`).String(), resolve: resolved()},
		{name: "resolve_not_found", path: resolve, body: dispatchGoldenContext.String(),
			resolve: &recordingResolveProvider{err: shoal.NewError(shoal.ErrorNotFound, "agent not found")}},
		{name: "resolve_unauthorized", path: resolve, body: dispatchGoldenContext.String(),
			resolve: &recordingResolveProvider{err: shoal.NewError(shoal.ErrorUnauthorized, "not permitted")}},

		// Path IDs.
		{name: "claim_padded_path_id", path: "/api/v1/fleet/actions/YWI=/claim",
			body: dispatchGoldenClaim.String(), provider: claimed},
		{name: "claim_overlong_path_id", path: "/api/v1/fleet/actions/" + strings.Repeat("QUFB", 85) + "QUE/claim",
			body: dispatchGoldenClaim.String(), provider: claimed},
		{name: "claim_max_path_id", path: "/api/v1/fleet/actions/" + strings.Repeat("QUFB", 85) + "QQ/claim",
			body: dispatchGoldenClaim.String(), provider: claimed},
		{name: "complete_padded_path_id", path: "/api/v1/fleet/actions/YWI=/complete",
			body: dispatchGoldenComplete.String(), provider: succeeded},
		{name: "claim_get_method", path: claim, body: dispatchGoldenClaim.String(), provider: claimed,
			contentType: "GET"},

		// Field bounds and spellings.
		{name: "claim_padded_claim_id", path: claim,
			body: dispatchGoldenClaim.with("claim_id", `"YWI="`).String(), provider: claimed},
		{name: "claim_overlong_claim_id", path: claim,
			body:     dispatchGoldenClaim.with("claim_id", `"`+strings.Repeat("QUFB", 85)+`QUE"`).String(),
			provider: claimed},
		{name: "claim_max_claim_id", path: claim,
			body:     dispatchGoldenClaim.with("claim_id", `"`+strings.Repeat("QUFB", 85)+`QQ"`).String(),
			provider: claimed},
		{name: "claim_lease_string", path: claim,
			body: dispatchGoldenClaim.with("lease", `"1m"`).String(), provider: claimed},
		{name: "claim_negative_version", path: claim,
			body: dispatchGoldenClaim.with("expected_version", `-1`).String(), provider: claimed},
		{name: "claim_padded_context_request_id", path: claim,
			body: dispatchGoldenClaim.with("context",
				dispatchGoldenContext.with("request_id", `"YWI="`).String()).String(),
			provider: claimed},
		{name: "claim_padded_context_correlation_id", path: claim,
			body: dispatchGoldenClaim.with("context",
				dispatchGoldenContext.with("correlation_id", `"YWI="`).String()).String(),
			provider: claimed},
		{name: "claim_utc_deadline", path: claim,
			body: dispatchGoldenClaim.with("context",
				dispatchGoldenContext.with("deadline", `"2026-09-06T12:01:02Z"`).String()).String(),
			provider: claimed},
		{name: "claim_context_not_object", path: claim,
			body: dispatchGoldenClaim.with("context", `[]`).String(), provider: claimed},
		{name: "complete_output_not_json_object", path: complete,
			body: dispatchGoldenComplete.with("output", `"text"`).String(), provider: succeeded},
	}

	// Request-shape edge cases, through every route.
	for _, r := range routes {
		ok := r.ok
		add := func(suffix, body string, contentType ...string) {
			c := dispatchGoldenCase{name: r.name + "_" + suffix, path: r.path, body: body, provider: ok}
			if len(contentType) > 0 {
				c.contentType = contentType[0]
			}
			cases = append(cases, c)
		}
		first := r.body[0][0]
		add("ok", r.body.String())
		add("unknown_field", r.body.plus("extra", "1").String())
		add("duplicate_key", r.body.plus(first, r.body[0][1]).String())
		add("case_variant_key", r.body.renamed(first, strings.ToUpper(first)).String())
		add("case_variant_and_canonical", r.body.plus(strings.ToUpper(first), r.body[0][1]).String())
		add("trailing_object", r.body.String()+" {}")
		add("trailing_garbage", r.body.String()+"x")
		add("trailing_whitespace", r.body.String()+" \n")
		add("not_an_object", `[]`)
		add("null_body", `null`)
		add("empty_body", ``)
		add("invalid_utf8", strings.Replace(r.body.String(), "detail", "det\xffail", 1))
		add("wrong_content_type", r.body.String(), "text/plain")
		add("content_type_with_charset", r.body.String(), "application/json; charset=utf-8")
		if r.name != "status" {
			add("unknown_context_field", r.body.with("context",
				dispatchGoldenContext.plus("extra", "1").String()).String())
		}
		cases = append(cases, dispatchGoldenCase{
			name: r.name + "_overflow", path: r.path, body: r.body.String(), provider: ok, budget: 32,
		}, dispatchGoldenCase{
			name: r.name + "_overflow_fallback_fits", path: r.path, body: r.body.String(),
			provider: ok, budget: 100,
		})
		// Every field nulled and dropped, one at a time.
		for _, field := range r.body {
			cases = append(cases,
				dispatchGoldenCase{name: r.name + "_null_" + field[0], path: r.path,
					body: r.body.with(field[0], "null").String(), provider: ok},
				dispatchGoldenCase{name: r.name + "_missing_" + field[0], path: r.path,
					body: r.body.without(field[0]).String(), provider: ok})
		}
		if r.name != "status" {
			for _, field := range dispatchGoldenContext {
				cases = append(cases,
					dispatchGoldenCase{name: r.name + "_null_context_" + field[0], path: r.path,
						body: r.body.with("context",
							dispatchGoldenContext.with(field[0], "null").String()).String(),
						provider: ok},
					dispatchGoldenCase{name: r.name + "_missing_context_" + field[0], path: r.path,
						body: r.body.with("context",
							dispatchGoldenContext.without(field[0]).String()).String(),
						provider: ok})
			}
		}
		// Every error the handler maps, through every route.
		for _, sentinel := range dispatchGoldenSentinels() {
			cases = append(cases, dispatchGoldenCase{
				name: r.name + "_error_" + sentinel.name, path: r.path,
				body: r.body.String(), provider: failing(sentinel.err),
			})
		}
	}
	return cases
}

type dispatchGoldenSentinel struct {
	name string
	err  error
}

func dispatchGoldenSentinels() []dispatchGoldenSentinel {
	return []dispatchGoldenSentinel{
		{"approval_required", fleet.ErrApprovalRequired},
		{"approval_required_conflict", shoal.WrapError(
			shoal.ErrorConflict, "approval needed", fleet.ErrApprovalRequired)},
		{"attestation_required", fleet.ErrAttestationRequired},
		{"attestation_required_conflict", shoal.WrapError(
			shoal.ErrorConflict, "attest first", fleet.ErrAttestationRequired)},
		{"attestation_unavailable", fleet.ErrAttestationUnavailable},
		{"attestation_unavailable_coded", shoal.WrapError(
			shoal.ErrorUnavailable, "store down", fleet.ErrAttestationUnavailable)},
		{"execution_ambiguous", fleet.ErrExecutionAmbiguous},
		{"execution_ambiguous_claim_lost", errors.Join(fleet.ErrExecutionAmbiguous, fleet.ErrClaimLost)},
		{"execution_ambiguous_not_found", errors.Join(fleet.ErrExecutionAmbiguous, fleet.ErrActionNotFound)},
		{"execution_ambiguous_recording", errors.Join(fleet.ErrExecutionAmbiguous, fleet.ErrRecordingUnavailable)},
		{"action_committed", fleet.ErrActionCommitted},
		{"action_committed_terminal", errors.Join(fleet.ErrActionCommitted, fleet.ErrActionTerminal)},
		{"action_not_found", fleet.ErrActionNotFound},
		{"action_conflict", fleet.ErrActionConflict},
		{"claim_lost", fleet.ErrClaimLost},
		{"action_terminal", fleet.ErrActionTerminal},
		{"recording_unavailable", fleet.ErrRecordingUnavailable},
		{"invalid_argument", shoal.NewError(shoal.ErrorInvalidArgument, "claim lease is invalid")},
		{"unauthorized", shoal.NewError(shoal.ErrorUnauthorized, "not permitted")},
		{"not_found_coded", shoal.NewError(shoal.ErrorNotFound, "fleet action not found")},
		{"deadline", shoal.NewError(shoal.ErrorDeadline, "request deadline has elapsed")},
		{"canceled", shoal.NewError(shoal.ErrorCanceled, "canceled")},
		{"unavailable", shoal.NewError(shoal.ErrorUnavailable, "store unavailable")},
		{"unknown", errors.New("boom")},
		{"indeterminate", explorer.MarkIndeterminateCommit(
			shoal.WrapError(shoal.ErrorUnavailable, "commit lost", fleet.ErrActionCommitted))},
		{"indeterminate_not_found", explorer.MarkIndeterminateCommit(fleet.ErrActionNotFound)},
	}
}

func dispatchGoldenHandler(
	t *testing.T, dispatch FleetDispatchProvider, registry FleetRegistryProvider, now time.Time,
) http.Handler {
	t.Helper()
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "subject", Actor: "actor", AuthorizationDomain: []byte("domain"),
		AllowedOperations:  []auth.Operation{auth.OperationDispatch, auth.OperationExecute},
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
	fleetHandler, err := NewFleetHandler(registry, dispatch)
	if err != nil {
		t.Fatal(err)
	}
	if err := handler.MountAuthenticated(FleetRoutePrefix, fleetHandler); err != nil {
		t.Fatal(err)
	}
	return handler
}

func serveDispatchGolden(t *testing.T, c dispatchGoldenCase) string {
	t.Helper()
	provider := c.provider
	registry := c.resolve
	if registry == nil {
		registry = &recordingResolveProvider{}
	}
	method := http.MethodPost
	contentType := c.contentType
	if contentType == "GET" {
		method, contentType = http.MethodGet, ""
	}
	if contentType == "" {
		contentType = "application/json"
	}
	request := httptest.NewRequest(method, "http://example.test"+c.path, strings.NewReader(c.body))
	request.Host = "example.test"
	request.Header.Set("Content-Type", contentType)
	recorder := httptest.NewRecorder()
	if c.budget != 0 {
		handler, err := NewFleetHandler(registry, &provider)
		if err != nil {
			t.Fatal(err)
		}
		handler.ServeHTTP(workspaceResponseWriter{
			ResponseWriter: recorder, maxResponseBytes: c.budget,
			indeterminateOnOverflow: requestMayCommit(method, c.path),
		}, request)
	} else {
		dispatchGoldenHandler(t, &provider, registry, dispatchGoldenWhen.UTC()).ServeHTTP(recorder, request)
	}
	var out strings.Builder
	fmt.Fprintf(&out, "%s %s\n", method, c.path)
	fmt.Fprintf(&out, "request content-type: %s\n", contentType)
	fmt.Fprintf(&out, "request body: %q\n", c.body)
	seen := append(append([]string(nil), provider.seen...), registry.seen...)
	fmt.Fprintf(&out, "provider calls: %d\n", len(seen))
	for _, input := range seen {
		fmt.Fprintf(&out, "provider input: %s\n", input)
	}
	fmt.Fprintf(&out, "status: %d\n", recorder.Code)
	fmt.Fprintf(&out, "content-type: %s\n", recorder.Header().Get("Content-Type"))
	fmt.Fprintf(&out, "shoal-commit-outcome: %s\n", recorder.Header().Get(CommitOutcomeHeader))
	fmt.Fprintf(&out, "body: %q\n", recorder.Body.String())
	return out.String()
}

// TestFleetDispatchWireGolden is the compatibility contract of the worker's
// dispatch surface. See dispatchGoldenDir.
func TestFleetDispatchWireGolden(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range dispatchGoldenCases() {
		if seen[c.name] {
			t.Fatalf("duplicate golden case %s", c.name)
		}
		seen[c.name] = true
		t.Run(c.name, func(t *testing.T) {
			got := serveDispatchGolden(t, c)
			checkAdmissionGolden(t, filepath.Join(dispatchGoldenDir, c.name+".golden"), got)
		})
	}
	if *updateAdmissionGolden {
		return
	}
	entries, err := os.ReadDir(dispatchGoldenDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := strings.TrimSuffix(entry.Name(), ".golden")
		if !seen[name] {
			t.Errorf("orphaned golden fixture %s", entry.Name())
		}
	}
}
