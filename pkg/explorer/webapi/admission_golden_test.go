// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package webapi

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// The admission golden fixtures pin the wire of /api/v1/admission/* exactly:
// status, the headers a client branches on, the response bytes, and what the
// handler hands the provider. They were captured from the handler before its
// wire types moved to pkg/admission/api (#446 slice 2), so that move — and any
// later one — is provably byte-compatible. A change here is a wire change and
// needs its own review; regenerate with
//
//	go test ./pkg/explorer/webapi -run TestAdmissionWireGolden -update
//
// Decoding is deliberately lenient today (duplicate keys last-wins,
// case-insensitive keys, null as zero). The fixtures pin that leniency as it
// is rather than as it should be; tightening it is #495.
var updateAdmissionGolden = flag.Bool(
	"update", false, "rewrite the admission wire golden fixtures")

const admissionGoldenDir = "../../admission/api/testdata/wire/server"

// recordingAdmissionProvider records exactly what the handler decoded, so a
// fixture pins the decode as well as the encode.
type recordingAdmissionProvider struct {
	grant  fleet.AdmissionGrant
	record fleet.ActionRecord
	page   fleet.OutstandingAdmissionsPage
	err    error
	seen   []string
}

func (p *recordingAdmissionProvider) Request(
	_ context.Context, request fleet.AdmissionRequest,
) (fleet.AdmissionGrant, error) {
	p.seen = append(p.seen, goldenGoString(request))
	return p.grant, p.err
}

func (p *recordingAdmissionProvider) Report(
	_ context.Context, report fleet.AdmissionReport,
) (fleet.ActionRecord, error) {
	p.seen = append(p.seen, goldenGoString(report))
	return p.record, p.err
}

func (p *recordingAdmissionProvider) Outstanding(
	_ context.Context, request fleet.OutstandingAdmissionsRequest,
) (fleet.OutstandingAdmissionsPage, error) {
	p.seen = append(p.seen, goldenGoString(request))
	return p.page, p.err
}

// goldenGoString renders a provider input with %#v. A timestamp decoded from
// an offset that happens to equal the machine's local offset is placed in
// time.Local rather than an unnamed fixed zone, so the one spelling that
// depends on the machine is folded onto the other.
func goldenGoString(value any) string {
	return strings.ReplaceAll(
		fmt.Sprintf("%#v", value), "time.Local", `time.Location("")`)
}

// goldenFields is an ordered JSON object, so a case can drop, null, duplicate or
// respell one field and leave every other byte alone.
type goldenFields [][2]string

func (f goldenFields) String() string {
	var out strings.Builder
	out.WriteByte('{')
	for i, field := range f {
		if i > 0 {
			out.WriteByte(',')
		}
		fmt.Fprintf(&out, "%q:%s", field[0], field[1])
	}
	out.WriteByte('}')
	return out.String()
}

func (f goldenFields) with(key, value string) goldenFields {
	result := make(goldenFields, 0, len(f))
	for _, field := range f {
		if field[0] == key {
			field[1] = value
		}
		result = append(result, field)
	}
	return result
}

func (f goldenFields) without(key string) goldenFields {
	result := make(goldenFields, 0, len(f))
	for _, field := range f {
		if field[0] != key {
			result = append(result, field)
		}
	}
	return result
}

func (f goldenFields) plus(key, value string) goldenFields {
	return append(append(goldenFields(nil), f...), [2]string{key, value})
}

func (f goldenFields) renamed(from, to string) goldenFields {
	result := make(goldenFields, 0, len(f))
	for _, field := range f {
		if field[0] == from {
			field[0] = to
		}
		result = append(result, field)
	}
	return result
}

// The canonical bodies. The deadline carries a non-UTC offset because the
// wire does not normalise times, and a refactor that did would change bytes.
var (
	goldenContext = goldenFields{
		{"request_id", `"cmVxdWVzdA"`},
		{"correlation_id", `"Y29ycmVsYXRpb24"`},
		{"reason_code", `"llm_gateway_call"`},
		{"reason_detail", `"detail"`},
		{"deadline", `"2026-09-06T14:01:02.5+02:00"`},
	}
	goldenRequest = goldenFields{
		{"context", goldenContext.String()},
		{"id", `"YWRtaXNzaW9u"`},
		{"idempotency_key", `"a2V5"`},
		{"token_id", `"dG9rZW4"`},
		{"agent_id", `"YWdlbnQ"`},
		{"agent_generation", `3`},
		{"capability", `"llm.gateway"`},
		{"action", `"complete"`},
		{"source_id", `"c291cmNl"`},
		{"policy_id", `"cG9saWN5"`},
		{"object_id", `"b2JqZWN0"`},
		{"effects", `["egresses-content","reads-corpus"]`},
		{"input", `{"prompt_digest":"abc","n":1}`},
		{"disclosures", `["ZG9jLWE","ZG9jLWI"]`},
		{"lease", `60000000000`},
	}
	goldenToken = goldenFields{
		{"action_id", `"YQD_"`},
		{"token_id", `"dAD-"`},
		{"version", `2`},
		{"expires_at", `"2026-09-06T14:02:00+02:00"`},
	}
	goldenReport = goldenFields{
		{"context", goldenContext.with("reason_code", `"llm_gateway_report"`).String()},
		{"token", goldenToken.String()},
		{"outcome", `{"tokens":12}`},
	}
	goldenOutstanding = goldenFields{
		{"context", goldenContext.String()},
		{"after", `"Y3Vyc29y"`},
		{"limit", `10`},
	}
)

var goldenWhen = time.Date(2026, 9, 6, 12, 0, 0, 0, time.FixedZone("", 2*3600))

func goldenGrantAllowed() fleet.AdmissionGrant {
	return fleet.AdmissionGrant{
		Outcome: fleet.AdmissionAllowed,
		Token: fleet.AdmissionToken{
			ActionID: []byte{'a', 0, 255}, TokenID: []byte("token"),
			Version: 2, ExpiresAt: goldenWhen.Add(2 * time.Minute),
		},
	}
}

type admissionGoldenCase struct {
	name        string
	path        string
	contentType string
	body        string
	provider    recordingAdmissionProvider
	// budget, when set, serves the bare handler under a workspace output
	// limit, as the authenticated handler does when settings apply.
	budget uint64
}

func admissionGoldenCases() []admissionGoldenCase {
	const (
		request     = "/api/v1/admission/request"
		report      = "/api/v1/admission/report"
		outstanding = "/api/v1/admission/outstanding"
	)
	allowed := recordingAdmissionProvider{grant: goldenGrantAllowed()}
	denied := recordingAdmissionProvider{
		grant: fleet.AdmissionGrant{Outcome: fleet.AdmissionDenied},
	}
	receipt := recordingAdmissionProvider{record: fleet.ActionRecord{
		ID: []byte{'a', 0, 255}, Version: 3, State: fleet.DispatchSucceeded,
		UpdatedAt: goldenWhen.Add(time.Second),
	}}
	page := func(next []byte) recordingAdmissionProvider {
		return recordingAdmissionProvider{page: fleet.OutstandingAdmissionsPage{
			Admissions: []fleet.OutstandingAdmission{{
				ActionID: []byte{'a', 0, 255}, TokenID: []byte("token"),
				Version: 2, AdmittedAt: goldenWhen,
				ExpiresAt: goldenWhen.Add(time.Minute),
			}, {
				ActionID: []byte("b"), TokenID: []byte("t2"), Version: 4,
				AdmittedAt: goldenWhen.UTC(),
				ExpiresAt:  goldenWhen.UTC().Add(time.Minute), Expired: true,
			}},
			Next: next,
		}}
	}
	failing := func(err error) recordingAdmissionProvider {
		return recordingAdmissionProvider{err: err}
	}
	cases := []admissionGoldenCase{
		{name: "request_allowed", path: request, body: goldenRequest.String(), provider: allowed},
		{name: "request_obligated", path: request, body: goldenRequest.String(),
			provider: recordingAdmissionProvider{grant: fleet.AdmissionGrant{
				Outcome: fleet.AdmissionObligated, Token: goldenGrantAllowed().Token,
				Obligations: fleet.Obligations{Withhold: []shoal.ID{"doc-a", "\x00\xff"}},
			}}},
		{name: "request_denied", path: request, body: goldenRequest.String(), provider: denied},
		// A denial that still carries a token or obligations internally must
		// emit neither the token nor anything but an empty list.
		{name: "request_denied_with_internal_token", path: request, body: goldenRequest.String(),
			provider: recordingAdmissionProvider{grant: fleet.AdmissionGrant{
				Outcome: fleet.AdmissionDenied, Token: goldenGrantAllowed().Token,
			}}},
		{name: "request_unknown_effect", path: request,
			body: goldenRequest.with("effects", `["invented-class"]`).String(), provider: denied},
		{name: "request_minimal", path: request,
			body: goldenRequest.without("disclosures").with("source_id", "null").
				with("policy_id", "null").with("context",
				goldenContext.without("correlation_id").without("reason_detail").
					with("deadline", `"2026-09-06T12:00:00Z"`).String()).String(),
			provider: allowed},
		{name: "request_unknown_field", path: request,
			body: goldenRequest.plus("extra", "1").String(), provider: allowed},
		{name: "request_unknown_context_field", path: request,
			body:     goldenRequest.with("context", goldenContext.plus("extra", "1").String()).String(),
			provider: allowed},
		{name: "request_duplicate_key", path: request,
			body: goldenRequest.plus("lease", "30000000000").String(), provider: allowed},
		{name: "request_duplicate_object_id", path: request,
			body: goldenRequest.plus("object_id", `"b3RoZXI"`).String(), provider: allowed},
		{name: "request_case_variant_key", path: request,
			body: goldenRequest.renamed("lease", "LEASE").String(), provider: allowed},
		{name: "request_case_variant_and_canonical", path: request,
			body: goldenRequest.plus("Lease", "30000000000").String(), provider: allowed},
		{name: "request_trailing_object", path: request,
			body: goldenRequest.String() + " {}", provider: allowed},
		{name: "request_trailing_garbage", path: request,
			body: goldenRequest.String() + "x", provider: allowed},
		{name: "request_not_an_object", path: request, body: `[]`, provider: allowed},
		{name: "request_empty_body", path: request, body: ``, provider: allowed},
		{name: "request_wrong_content_type", path: request, contentType: "text/plain",
			body: goldenRequest.String(), provider: allowed},
		{name: "request_content_type_with_charset", path: request,
			contentType: "application/json; charset=utf-8",
			body:        goldenRequest.String(), provider: allowed},
		{name: "request_padded_id", path: request,
			body: goldenRequest.with("id", `"YWI="`).String(), provider: allowed},
		{name: "request_padded_agent_id", path: request,
			body: goldenRequest.with("agent_id", `"YWI="`).String(), provider: allowed},
		{name: "request_padded_disclosure", path: request,
			body: goldenRequest.with("disclosures", `["YWI="]`).String(), provider: allowed},
		{name: "request_padded_context_request_id", path: request,
			body: goldenRequest.with("context",
				goldenContext.with("request_id", `"YWI="`).String()).String(),
			provider: allowed},
		{name: "request_unpadded_source_id", path: request,
			body: goldenRequest.with("source_id", `"YWI"`).String(), provider: allowed},
		{name: "request_url_safe_source_id", path: request,
			body: goldenRequest.with("source_id", `"_-8="`).String(), provider: allowed},
		{name: "request_overlong_id", path: request,
			body:     goldenRequest.with("id", `"`+strings.Repeat("QUFB", 85)+`QUE"`).String(),
			provider: allowed},
		{name: "request_max_id", path: request,
			body:     goldenRequest.with("id", `"`+strings.Repeat("QUFB", 85)+`QQ"`).String(),
			provider: allowed},
		{name: "request_overflow", path: request, body: goldenRequest.String(),
			provider: allowed, budget: 32},
		{name: "request_overflow_fallback_fits", path: request, body: goldenRequest.String(),
			provider: allowed, budget: 100},
		{name: "report_outcome", path: report, body: goldenReport.String(), provider: receipt},
		{name: "report_failed", path: report,
			body: goldenReport.without("outcome").plus("failed", "true").
				plus("error_code", `"upstream_unreachable"`).String(),
			provider: receipt},
		// #427: the volume of a partial egress reaches the provider. Three
		// cases, because the whole point is that they stop arriving
		// identically: nothing left, a little, and a framed stream.
		{name: "report_failed_effected_absent", path: report,
			body: goldenReport.without("outcome").plus("failed", "true").
				plus("error_code", `"response_truncated"`).String(),
			provider: receipt},
		{name: "report_failed_effected_bytes", path: report,
			body: goldenReport.without("outcome").plus("failed", "true").
				plus("error_code", `"response_truncated"`).
				plus("effected", `{"bytes":50}`).String(),
			provider: receipt},
		{name: "report_failed_effected_chunked", path: report,
			body: goldenReport.without("outcome").plus("failed", "true").
				plus("error_code", `"response_truncated"`).
				plus("effected", `{"bytes":2097152,"chunks":64}`).String(),
			provider: receipt},
		{name: "report_failed_false_with_code", path: report,
			body:     goldenReport.plus("failed", "false").plus("error_code", `""`).String(),
			provider: receipt},
		{name: "report_null_token", path: report,
			body: goldenReport.with("token", "null").String(), provider: receipt},
		{name: "report_missing_token", path: report,
			body: goldenReport.without("token").String(), provider: receipt},
		{name: "report_padded_token_action_id", path: report,
			body:     goldenReport.with("token", goldenToken.with("action_id", `"YWI="`).String()).String(),
			provider: receipt},
		{name: "report_unknown_token_field", path: report,
			body:     goldenReport.with("token", goldenToken.plus("extra", "1").String()).String(),
			provider: receipt},
		{name: "report_spent", path: report, body: goldenReport.String(),
			provider: failing(fleet.ErrAdmissionSpent)},
		{name: "report_overflow", path: report, body: goldenReport.String(),
			provider: receipt, budget: 32},
		{name: "report_overflow_fallback_fits", path: report, body: goldenReport.String(),
			provider: receipt, budget: 92},
		{name: "outstanding_with_next", path: outstanding, body: goldenOutstanding.String(),
			provider: page([]byte{'n', 0, 255})},
		{name: "outstanding_without_next", path: outstanding,
			body: goldenOutstanding.without("after").String(), provider: page(nil)},
		{name: "outstanding_empty", path: outstanding,
			body:     goldenOutstanding.with("after", `""`).String(),
			provider: recordingAdmissionProvider{}},
		{name: "outstanding_padded_after", path: outstanding,
			body: goldenOutstanding.with("after", `"YWI="`).String(), provider: page(nil)},
		{name: "outstanding_null_limit", path: outstanding,
			body: goldenOutstanding.with("limit", "null").String(), provider: page(nil)},
		{name: "outstanding_spent", path: outstanding, body: goldenOutstanding.String(),
			provider: failing(fleet.ErrAdmissionSpent)},
		{name: "outstanding_overflow", path: outstanding, body: goldenOutstanding.String(),
			provider: page(nil), budget: 32},
		{name: "outstanding_overflow_fallback_fits", path: outstanding,
			body: goldenOutstanding.String(), provider: page(nil), budget: 100},
	}
	// Every request field nulled and dropped, one at a time.
	for _, field := range goldenRequest {
		cases = append(cases,
			admissionGoldenCase{name: "request_null_" + field[0], path: request,
				body: goldenRequest.with(field[0], "null").String(), provider: allowed},
			admissionGoldenCase{name: "request_missing_" + field[0], path: request,
				body: goldenRequest.without(field[0]).String(), provider: allowed})
	}
	for _, field := range goldenContext {
		cases = append(cases,
			admissionGoldenCase{name: "request_null_context_" + field[0], path: request,
				body: goldenRequest.with("context",
					goldenContext.with(field[0], "null").String()).String(),
				provider: allowed},
			admissionGoldenCase{name: "request_missing_context_" + field[0], path: request,
				body: goldenRequest.with("context",
					goldenContext.without(field[0]).String()).String(),
				provider: allowed})
	}
	for _, field := range goldenToken {
		cases = append(cases,
			admissionGoldenCase{name: "report_null_token_" + field[0], path: report,
				body: goldenReport.with("token",
					goldenToken.with(field[0], "null").String()).String(),
				provider: receipt},
			admissionGoldenCase{name: "report_missing_token_" + field[0], path: report,
				body: goldenReport.with("token",
					goldenToken.without(field[0]).String()).String(),
				provider: receipt})
	}
	// Every error the handler maps, through the request route.
	for _, sentinel := range []struct {
		name string
		err  error
	}{
		{"spent", fleet.ErrAdmissionSpent},
		{"conflict", fleet.ErrAdmissionConflict},
		{"unmigrated", fleet.ErrAdmissionUnmigrated},
		{"span_occupied", fleet.ErrAdmissionSpanOccupied},
		{"approval_required", fleet.ErrApprovalRequired},
		{"approval_required_conflict", shoal.WrapError(
			shoal.ErrorConflict, "approval needed", fleet.ErrApprovalRequired)},
		{"action_not_found", fleet.ErrActionNotFound},
		{"action_conflict", fleet.ErrActionConflict},
		{"claim_lost", fleet.ErrClaimLost},
		{"action_terminal", fleet.ErrActionTerminal},
		{"execution_ambiguous", fleet.ErrExecutionAmbiguous},
		{"action_committed", fleet.ErrActionCommitted},
		{"recording_unavailable", fleet.ErrRecordingUnavailable},
		{"invalid_argument", shoal.NewError(shoal.ErrorInvalidArgument, "admission lease is invalid")},
		{"unauthorized", shoal.NewError(shoal.ErrorUnauthorized, "not permitted")},
		{"deadline", shoal.NewError(shoal.ErrorDeadline, "deadline exceeded")},
		{"canceled", shoal.NewError(shoal.ErrorCanceled, "canceled")},
		{"unknown", errors.New("boom")},
		{"indeterminate", explorer.MarkIndeterminateCommit(
			shoal.WrapError(shoal.ErrorUnavailable, "commit lost", fleet.ErrActionCommitted))},
		{"indeterminate_spent", explorer.MarkIndeterminateCommit(fleet.ErrAdmissionSpent)},
	} {
		cases = append(cases, admissionGoldenCase{
			name: "request_error_" + sentinel.name, path: request,
			body: goldenRequest.String(), provider: failing(sentinel.err),
		})
	}
	return cases
}

func serveAdmissionGolden(t *testing.T, c admissionGoldenCase) string {
	t.Helper()
	provider := c.provider
	contentType := c.contentType
	if contentType == "" {
		contentType = "application/json"
	}
	request := httptest.NewRequest(http.MethodPost,
		"http://example.test"+c.path, strings.NewReader(c.body))
	request.Host = "example.test"
	request.Header.Set("Content-Type", contentType)
	recorder := httptest.NewRecorder()
	if c.budget != 0 {
		handler, err := NewAdmissionHandler(&provider)
		if err != nil {
			t.Fatal(err)
		}
		handler.ServeHTTP(workspaceResponseWriter{
			ResponseWriter: recorder, maxResponseBytes: c.budget,
			indeterminateOnOverflow: requestMayCommit(http.MethodPost, c.path),
		}, request)
	} else {
		admissionTestHandler(t, &provider, goldenWhen.UTC()).ServeHTTP(
			recorder, request)
	}
	var out strings.Builder
	fmt.Fprintf(&out, "POST %s\n", c.path)
	fmt.Fprintf(&out, "request content-type: %s\n", contentType)
	fmt.Fprintf(&out, "request body: %q\n", c.body)
	fmt.Fprintf(&out, "provider calls: %d\n", len(provider.seen))
	for _, seen := range provider.seen {
		fmt.Fprintf(&out, "provider input: %s\n", seen)
	}
	fmt.Fprintf(&out, "status: %d\n", recorder.Code)
	fmt.Fprintf(&out, "content-type: %s\n", recorder.Header().Get("Content-Type"))
	fmt.Fprintf(&out, "shoal-commit-outcome: %s\n",
		recorder.Header().Get(CommitOutcomeHeader))
	fmt.Fprintf(&out, "body: %q\n", recorder.Body.String())
	return out.String()
}

// TestAdmissionWireGolden is the compatibility contract of the admission
// surface. See updateAdmissionGolden.
func TestAdmissionWireGolden(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range admissionGoldenCases() {
		if seen[c.name] {
			t.Fatalf("duplicate golden case %s", c.name)
		}
		seen[c.name] = true
		t.Run(c.name, func(t *testing.T) {
			got := serveAdmissionGolden(t, c)
			checkAdmissionGolden(t, filepath.Join(admissionGoldenDir, c.name+".golden"), got)
		})
	}
	// A fixture with no case is a pin nobody checks any more.
	entries, err := os.ReadDir(admissionGoldenDir)
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

func checkAdmissionGolden(t *testing.T, path, got string) {
	t.Helper()
	if *updateAdmissionGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing golden fixture (run with -update): %v", err)
	}
	if !bytes.Equal(want, []byte(got)) {
		t.Fatalf("admission wire changed for %s\n--- want\n%s\n--- got\n%s",
			filepath.Base(path), want, got)
	}
}
