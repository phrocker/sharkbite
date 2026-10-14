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

package webapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	exploreranalytics "github.com/phrocker/shoal-oss/pkg/explorer/analytics"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/workspace"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestWorkspaceSettingsClampAnalyticsAndMarkResponseLoss(t *testing.T) {
	request := AnalyticsRequest{
		Scope: exploreranalytics.Scope{
			Depth: 9, Fanout: 10, MaxNodes: 100,
		},
	}
	applyAnalyticsWorkspaceLimits(&request, workspace.Limits{
		GraphDepth: 3, GraphFanout: 4, GraphNodes: 25,
	})
	if request.Scope.Depth != 3 ||
		request.Scope.Fanout != 4 ||
		request.Scope.MaxNodes != 25 {
		t.Fatalf("analytics workspace limits = %#v", request.Scope)
	}
	for _, test := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/v1/analytics"},
		{http.MethodPost, "/api/v1/fleet/agents"},
		{http.MethodPost, "/api/v1/fleet/agents/agent/heartbeat"},
		{http.MethodPost, "/api/v1/fleet/agents/agent/revoke"},
		{http.MethodPost, "/api/v1/fleet/actions"},
		{http.MethodPost, "/api/v1/fleet/actions/invoke"},
		{http.MethodPost, "/api/v1/fleet/actions/action/claim"},
		// Completion writes the terminal record for work an out-of-process
		// worker has already performed. If its response is lost — including to
		// the workspace output limit — the worker must be told the outcome is
		// indeterminate, not that nothing committed: a deterministic failure
		// invites a retry of an external effect that already happened.
		{http.MethodPost, "/api/v1/fleet/actions/action/complete"},
		// A report writes the record before its response is encoded, on a
		// route whose entire purpose is recording that an effect may have
		// happened. Telling its caller nothing committed would be the worst
		// answer this surface can give.
		{http.MethodPost, "/api/v1/fleet/actions/action/ambiguity"},
		// A renewal writes the new lease before its response is encoded. An
		// over-budget response reported as a clean failure would tell a worker
		// its claim is gone while the extension actually landed, so it would
		// abandon work it still holds the fence for.
		{http.MethodPost, "/api/v1/fleet/actions/action/extend"},
		{http.MethodPost, "/api/v1/fleet/actions/action/cancel"},
		// A request commits the grant or the refusal, and a report commits the
		// terminal record for a call the caller has already made. Losing
		// either response to the output limit must read as indeterminate: a
		// deterministic failure tells a caller nothing committed and invites it
		// to retry an egress that already happened.
		{http.MethodPost, "/api/v1/admission/request"},
		{http.MethodPost, "/api/v1/admission/report"},
		// An approval request writes the held record or materializes the
		// action; a decision writes the verdict. Either response lost to the
		// output limit must read as indeterminate.
		{http.MethodPost, "/api/v1/fleet/approvals/request"},
		{http.MethodPost, "/api/v1/fleet/approvals/approval/decide"},
		// A withdrawal writes the withdrawn record (#655).
		{http.MethodPost, "/api/v1/fleet/approvals/approval/withdraw"},
		{http.MethodPost, "/api/v1/fleet/events/subscriptions"},
		{http.MethodDelete, "/api/v1/fleet/events/subscriptions/subscription"},
		{http.MethodPost, "/api/v1/fleet/events/publish"},
	} {
		if !requestMayCommit(test.method, test.path) {
			t.Fatalf("%s %s response loss must be indeterminate",
				test.method, test.path)
		}
	}
	for _, test := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/v1/fleet/actions/pull"},
		{http.MethodPost, "/api/v1/fleet/actions/action/status"},
		// Listing what is outstanding writes nothing.
		{http.MethodPost, "/api/v1/admission/outstanding"},
		// Listing what is decidable writes nothing. Reading one request may
		// record a final state it observes (#655), which any later call
		// re-derives, so a lost response is not an indeterminate commit.
		{http.MethodPost, "/api/v1/fleet/approvals/pending"},
		{http.MethodPost, "/api/v1/fleet/approvals/approval/status"},
		{http.MethodPost, "/api/v1/fleet/agents/resolve"},
		{http.MethodPost, "/api/v1/fleet/agents/agent/resolve"},
	} {
		if requestMayCommit(test.method, test.path) {
			t.Fatalf("%s %s is read-only", test.method, test.path)
		}
	}
}

func TestWorkspaceOperationForRequestUsesRouteOperation(t *testing.T) {
	for _, test := range []struct {
		method    string
		path      string
		operation auth.Operation
		apply     bool
	}{
		{http.MethodPost, "/api/v1/retrieve", auth.OperationRetrieve, true},
		{http.MethodPost, "/api/v1/ask", auth.OperationRetrieve, true},
		{http.MethodPost, "/api/v1/chat/stream", auth.OperationRetrieve, true},
		{http.MethodGet, "/api/v1/ask", "", false},
		{http.MethodGet, "/api/v1/chat/stream", "", false},
		{http.MethodGet, "/api/v1/provenance", auth.OperationRead, true},
		{http.MethodGet, "/api/v1/provenance/c2Vzc2lvbg", auth.OperationRead, true},
		{http.MethodHead, "/api/v1/provenance/c2Vzc2lvbg", auth.OperationRead, true},
		{http.MethodPost, "/api/v1/provenance/fold", auth.OperationConnect, true},
		{http.MethodPost, "/api/v1/provenance/unfold", auth.OperationRead, true},
		{http.MethodGet, "/api/v1/provenance/", "", false},
		{http.MethodGet, "/api/v1/provenance/session/unknown", "", false},
		{http.MethodPost, "/api/v1/provenance/session", "", false},
		{http.MethodPost, "/api/v1/analytics", auth.OperationAnalyticsRead, true},
		{http.MethodPost, TeamOverviewRoute, auth.OperationTeamOverviewRead, true},
		{http.MethodPost, "/api/v1/neighborhood", auth.OperationNeighborhood, true},
		{http.MethodPost, "/api/v1/ingest", auth.OperationIngest, true},
		{http.MethodPost, "/api/v1/documents", auth.OperationList, true},
		{http.MethodPost, "/api/v1/document", auth.OperationRead, true},
		{http.MethodPost, "/api/v1/fleet/actions/invoke", auth.OperationInvoke, true},
		{http.MethodPost, "/api/v1/fleet/actions/action/claim", auth.OperationInvoke, true},
		// A route missing here is not a 404: ServeHTTP consults this table
		// before dispatching, and an unlisted path is refused as not
		// registered for the workspace, so the handler is never reached at all.
		{http.MethodPost, "/api/v1/fleet/actions/action/complete", auth.OperationInvoke, true},
		{http.MethodPost, "/api/v1/fleet/actions/action/cancel", auth.OperationDispatch, true},
		// The ambiguity route, under the same authority as the claim and the
		// completion it sits between. Unlisted, a workspace-scoped worker was
		// refused before the handler ran.
		{http.MethodPost, "/api/v1/fleet/actions/action/ambiguity", auth.OperationInvoke, true},
		// And the renewal, for the same reason. A long operation is precisely
		// the case a renewal exists for, so a workspace-scoped worker that
		// cannot renew loses its claim mid-operation with no way to keep it.
		{http.MethodPost, "/api/v1/fleet/actions/action/extend", auth.OperationInvoke, true},
		// All three admission routes, for the same reason: unlisted here, a
		// caller sending a workspace ID is refused before the handler runs, so
		// the whole surface is invisible to any workspace-scoped client.
		{http.MethodPost, "/api/v1/admission/request", auth.OperationInvoke, true},
		{http.MethodPost, "/api/v1/admission/report", auth.OperationInvoke, true},
		{http.MethodPost, "/api/v1/admission/outstanding", auth.OperationInvoke, true},
		// All four approval routes, each under the authority it actually
		// exercises: the requester's dispatch, the approver's approve.
		{http.MethodPost, "/api/v1/fleet/approvals/request", auth.OperationDispatch, true},
		{http.MethodPost, "/api/v1/fleet/approvals/approval/status", auth.OperationDispatch, true},
		{http.MethodPost, "/api/v1/fleet/approvals/approval/withdraw", auth.OperationDispatch, true},
		{http.MethodPost, "/api/v1/fleet/approvals/pending", auth.OperationActionApprove, true},
		{http.MethodPost, "/api/v1/fleet/approvals/approval/decide", auth.OperationActionApprove, true},
		{http.MethodGet, "/api/v1/fleet/approvals/pending", "", false},
		{http.MethodGet, "/api/v1/admission/request", "", false},
		{http.MethodPost, "/api/v1/admission/unknown", "", false},
		{http.MethodPost, "/api/v1/fleet/agents/agent/heartbeat", auth.OperationAgentHeartbeat, true},
		{http.MethodPost, "/api/v1/fleet/events/publish", auth.OperationEventPublish, true},
		{http.MethodPost, "/api/v1/fleet/events/subscriptions/id/pull", auth.OperationSubscriptionCreate, true},
		{http.MethodGet, "/api/v1/identity", auth.OperationRead, true},
		{http.MethodHead, "/api/v1/identity", auth.OperationRead, true},
		{http.MethodPost, "/mcp", "", false},
	} {
		operation, apply := workspaceOperationForRequest(test.method, test.path)
		if operation != test.operation || apply != test.apply {
			t.Fatalf("%s %s = %q, %v; want %q, %v",
				test.method, test.path, operation, apply,
				test.operation, test.apply)
		}
	}
}

func TestWorkspaceIDFromHeaderRequiresCanonicalSingleton(t *testing.T) {
	for _, test := range []struct {
		name    string
		values  []string
		want    shoal.ID
		present bool
		wantErr bool
	}{
		{name: "absent"},
		{name: "canonical", values: []string{"YQ"}, want: "a", present: true},
		{name: "noncanonical trailing bits", values: []string{"YR"}, wantErr: true},
		{name: "padded", values: []string{"YQ=="}, wantErr: true},
		{name: "whitespace", values: []string{" YQ"}, wantErr: true},
		{name: "duplicate", values: []string{"YQ", "YQ"}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			header := make(http.Header)
			for _, value := range test.values {
				header.Add(WorkspaceIDHeader, value)
			}
			got, present, err := workspaceIDFromHeader(header)
			if (err != nil) != test.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, test.wantErr)
			}
			if got != test.want || present != test.present {
				t.Fatalf(
					"workspace = %q, present = %v; want %q, %v",
					got, present, test.want, test.present,
				)
			}
		})
	}
}

func TestPublicIngestErrorPreservesIndeterminateCommit(t *testing.T) {
	original := explorer.MarkIndeterminateCommit(
		shoal.NewError(shoal.ErrorUnavailable, "private storage detail"))
	public := publicIngestError(original)
	if !explorer.IsIndeterminateCommit(public) ||
		!shoal.IsErrorCode(public, shoal.ErrorUnavailable) ||
		strings.Contains(public.Error(), "private storage detail") {
		t.Fatalf("public ingest error = %v", public)
	}
}

func TestDecodeRemoteErrorPreservesIndeterminateCommit(t *testing.T) {
	for _, test := range []struct {
		name   string
		header bool
		body   bool
	}{
		{name: "header", header: true},
		{name: "body", body: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload, err := json.Marshal(struct {
				Code          shoal.ErrorCode `json:"code"`
				Message       string          `json:"message"`
				Indeterminate bool            `json:"indeterminate,omitempty"`
			}{
				Code:          shoal.ErrorUnavailable,
				Message:       "unavailable: workspace outcome unknown",
				Indeterminate: test.body,
			})
			if err != nil {
				t.Fatal(err)
			}
			response := &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Header:     make(http.Header),
				Body:       io.NopCloser(bytes.NewReader(payload)),
			}
			if test.header {
				response.Header.Set(
					CommitOutcomeHeader, CommitOutcomeIndeterminate)
			}
			decoded := decodeRemoteError(response)
			if !explorer.IsIndeterminateCommit(decoded) ||
				!shoal.IsErrorCode(decoded, shoal.ErrorUnavailable) {
				t.Fatalf("decoded remote error = %v", decoded)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(
	request *http.Request,
) (*http.Response, error) {
	return f(request)
}

func TestRemoteIngestMarksUnknownPostDispatchOutcomes(t *testing.T) {
	for _, test := range []struct {
		name   string
		ingest func() (*http.Response, error)
	}{
		{
			name: "transport",
			ingest: func() (*http.Response, error) {
				return nil, errors.New("response unavailable")
			},
		},
		{
			name: "malformed success",
			ingest: func() (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader("{")),
				}, nil
			},
		},
		{
			name: "malformed failure",
			ingest: func() (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusBadGateway,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader("{")),
				}, nil
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			metadata, err := json.Marshal(MetadataResponse{
				MaxPageSize:         MaxPageSize,
				MaxTopK:             MaxTopK,
				MaxDepth:            MaxDepth,
				MaxFanout:           MaxFanout,
				MaxNodes:            MaxNodes,
				MaxEdgeTypes:        MaxEdgeTypes,
				MaxResponseBytes:    MaxResponseBytes,
				MaxUploadFiles:      MaxUploadFiles,
				MaxUploadFileBytes:  MaxUploadFileBytes,
				MaxUploadTotalBytes: MaxUploadTotalBytes,
				Capabilities:        AllCapabilities(),
			})
			if err != nil {
				t.Fatal(err)
			}
			client := &http.Client{Transport: roundTripFunc(func(
				request *http.Request,
			) (*http.Response, error) {
				if request.URL.Path == "/api/v1/meta" {
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     make(http.Header),
						Body:       io.NopCloser(bytes.NewReader(metadata)),
					}, nil
				}
				return test.ingest()
			})}
			remote, err := NewRemoteService("http://remote.example", client)
			if err != nil {
				t.Fatal(err)
			}
			_, err = remote.Ingest(context.Background(), IngestRequest{
				Files: []UploadFile{{
					Name: "note.txt", Content: []byte("hello"),
				}},
			})
			if !explorer.IsIndeterminateCommit(err) {
				t.Fatalf("remote ingest error = %v", err)
			}
		})
	}
}

func TestRemoteIngestKeepsVerifiedConflictDeterminate(t *testing.T) {
	metadata, err := json.Marshal(MetadataResponse{
		MaxPageSize:         MaxPageSize,
		MaxTopK:             MaxTopK,
		MaxDepth:            MaxDepth,
		MaxFanout:           MaxFanout,
		MaxNodes:            MaxNodes,
		MaxEdgeTypes:        MaxEdgeTypes,
		MaxResponseBytes:    MaxResponseBytes,
		MaxUploadFiles:      MaxUploadFiles,
		MaxUploadFileBytes:  MaxUploadFileBytes,
		MaxUploadTotalBytes: MaxUploadTotalBytes,
		Capabilities:        AllCapabilities(),
	})
	if err != nil {
		t.Fatal(err)
	}
	conflict, err := json.Marshal(struct {
		Code    shoal.ErrorCode `json:"code"`
		Message string          `json:"message"`
	}{
		Code:    shoal.ErrorConflict,
		Message: "conflict: duplicate upload",
	})
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: roundTripFunc(func(
		request *http.Request,
	) (*http.Response, error) {
		if request.URL.Path == "/api/v1/meta" {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(bytes.NewReader(metadata)),
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusConflict,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewReader(conflict)),
		}, nil
	})}
	remote, err := NewRemoteService("http://remote.example", client)
	if err != nil {
		t.Fatal(err)
	}
	_, err = remote.Ingest(context.Background(), IngestRequest{
		Files: []UploadFile{{
			Name: "note.txt", Content: []byte("hello"),
		}},
	})
	if explorer.IsIndeterminateCommit(err) ||
		!shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("verified conflict error = %v", err)
	}
	_, err = remote.Ingest(context.Background(), IngestRequest{
		Files: []UploadFile{
			{Name: "first.txt", Content: []byte("first")},
			{Name: "second.txt", Content: []byte("second")},
		},
	})
	if !explorer.IsIndeterminateCommit(err) ||
		!shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("multi-file verified conflict = %v", err)
	}
}

func TestRemoteAnalyticsMarksUnverifiedPostDispatchOutcomes(t *testing.T) {
	limits := exploreranalytics.DefaultLimits()
	metadata, err := json.Marshal(MetadataResponse{
		MaxPageSize: MaxPageSize, MaxTopK: MaxTopK,
		MaxDepth: MaxDepth, MaxFanout: MaxFanout,
		MaxNodes: MaxNodes, MaxEdgeTypes: MaxEdgeTypes,
		MaxResponseBytes:           MaxResponseBytes,
		AnalyticsLimits:            &limits,
		AnalyticsRecordingRequired: true,
		Capabilities:               Capabilities{Analytics: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := AnalyticsRequest{Scope: exploreranalytics.Scope{
		NodeIDs: []shoal.ID{"node"},
		Depth:   1, Direction: explorer.GraphDirectionBoth,
		Fanout: 1, MaxNodes: 1, MaxEdges: 1,
		MaxScannedEdgesPerNode: 1,
	}}
	for _, test := range []struct {
		name     string
		response func() (*http.Response, error)
	}{
		{
			name: "transport",
			response: func() (*http.Response, error) {
				return nil, errors.New("response unavailable")
			},
		},
		{
			name: "malformed failure",
			response: func() (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusBadGateway,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader("{")),
				}, nil
			},
		},
		{
			name: "malformed success",
			response: func() (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader("{")),
				}, nil
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(
				request *http.Request,
			) (*http.Response, error) {
				if request.URL.Path == "/api/v1/meta" {
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     make(http.Header),
						Body:       io.NopCloser(bytes.NewReader(metadata)),
					}, nil
				}
				return test.response()
			})}
			remote, err := NewRemoteService("http://remote.example", client)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := remote.Analytics(
				context.Background(), request,
			); !explorer.IsIndeterminateCommit(err) {
				t.Fatalf("remote analytics error = %v", err)
			}
		})
	}
}

func TestMutationResponseOverflowIsExplicitlyIndeterminate(t *testing.T) {
	response := httptest.NewRecorder()
	writer := workspaceResponseWriter{
		ResponseWriter:          response,
		maxResponseBytes:        32,
		indeterminateOnOverflow: true,
	}
	writeResponse(writer, http.StatusOK, strings.Repeat("x", 1024))
	if response.Code != http.StatusServiceUnavailable ||
		response.Header().Get(CommitOutcomeHeader) !=
			CommitOutcomeIndeterminate ||
		response.Body.Len() > 32 {
		t.Fatalf("overflow status = %d, header = %q, bytes = %d",
			response.Code, response.Header().Get(CommitOutcomeHeader),
			response.Body.Len())
	}
}

func TestErrorResponseOverflowPreservesDeterministicStatus(t *testing.T) {
	response := httptest.NewRecorder()
	writer := workspaceResponseWriter{
		ResponseWriter:          response,
		maxResponseBytes:        8,
		indeterminateOnOverflow: true,
	}
	writeResponse(writer, http.StatusBadRequest, strings.Repeat("x", 1024))
	if response.Code != http.StatusBadRequest ||
		response.Header().Get(CommitOutcomeHeader) != "" ||
		response.Body.Len() != 0 {
		t.Fatalf("overflow status = %d, header = %q, bytes = %d",
			response.Code, response.Header().Get(CommitOutcomeHeader),
			response.Body.Len())
	}
}

// TestDecodeRequestRefusesAmbiguousKeys pins the seam, not the walk.
//
// The walk itself is internal/strictjson's and has its own tests. What this
// asserts is that decodeRequest uses it — because the defect was never that no
// strict decoder existed, it was that the explorer's shared request decoder
// used encoding/json directly, which accepts duplicate keys with
// last-one-winning and matches field names case-insensitively, including
// Unicode simple folding. DisallowUnknownFields does not help with either: the
// fold happens during field matching, so a case-variant key is a *known* field
// before the unknown-field check can see it (#495).
func TestDecodeRequestRefusesAmbiguousKeys(t *testing.T) {
	type nested struct {
		Value int `json:"value"`
	}
	type body struct {
		Lease  int      `json:"lease"`
		Nested nested   `json:"nested"`
		Items  []nested `json:"items"`
	}
	for _, probe := range []struct {
		name    string
		raw     string
		refused bool
	}{
		{"an ordinary body", `{"lease":1,"nested":{"value":2}}`, false},
		{"an exact duplicate", `{"lease":1,"lease":2}`, true},
		{"a case variant", `{"lease":1,"Lease":2}`, true},
		// U+017F folds to s, so encoding/json would have set Lease from it.
		{"a Unicode fold", "{\"leaſe\":2}", true},
		{"a duplicate one level down", `{"nested":{"value":1,"value":2}}`, true},
		{"a case variant one level down", `{"nested":{"value":1,"Value":2}}`, true},
		{"a duplicate inside an array element",
			`{"items":[{"value":1},{"value":1,"Value":2}]}`, true},
		// Legal shapes that a walk must not refuse: the same key appearing in
		// two different objects is ordinary, and so is an array whose elements
		// share key names.
		{"the same key in two objects",
			`{"nested":{"value":1},"items":[{"value":2}]}`, false},
		{"array elements sharing key names",
			`{"items":[{"value":1},{"value":2}]}`, false},
		{"an empty object", `{}`, false},
		{"an empty array", `{"items":[]}`, false},
	} {
		t.Run(probe.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/v1/thing",
				strings.NewReader(probe.raw))
			request.Header.Set("Content-Type", "application/json")
			var decoded body
			err := decodeRequest(
				httptest.NewRecorder(), request, &decoded)
			if probe.refused && err == nil {
				t.Fatalf("%s was accepted, so the same bytes can read one "+
					"way to a reviewer or a proxy and decode another way "+
					"here", probe.raw)
			}
			if !probe.refused && err != nil {
				t.Fatalf("%s was refused, which breaks an ordinary request "+
					"shape: %v", probe.raw, err)
			}
		})
	}
}
