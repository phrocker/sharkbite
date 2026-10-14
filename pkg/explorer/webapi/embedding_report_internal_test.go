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

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func testEmbeddingReport() authorized.EmbeddingQueryReport {
	return authorized.EmbeddingQueryReport{
		Spaces: []authorized.EmbeddingSpaceReport{{
			ID:     shoal.ID([]byte{0x00, 0xff, 0x10}),
			Status: authorized.EmbeddingSpaceUnavailable,
		}},
		FanoutLimit: 8, Observed: true, Degraded: true,
	}
}

func TestRetrievalResponseEmbeddingReportUsesOpaqueIDCodec(t *testing.T) {
	report := testEmbeddingReport()
	response := RetrievalResponse{
		Snapshot: Snapshot{
			ID: "snapshot", AsOf: time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC),
			Frontier: 1,
		},
		Retrieval: retrieval.Response{},
		Embedding: &report,
	}

	payload, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}

	encodedID := base64.RawURLEncoding.EncodeToString([]byte(report.Spaces[0].ID))
	if !bytes.Contains(payload, []byte(`"id":"`+encodedID+`"`)) {
		t.Fatalf("wire report did not use opaque ID codec: %s", payload)
	}

	if bytes.Contains(payload, []byte{0x00, 0xff, 0x10}) {
		t.Fatalf("wire report emitted raw opaque identifier bytes: %q", payload)
	}
	var decoded RetrievalResponse
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Embedding == nil ||
		!reflect.DeepEqual(*decoded.Embedding, report) {
		t.Fatalf("round-tripped embedding report = %+v, want %+v",
			decoded.Embedding, report)
	}
}

func TestRetrievalResponseOmitsNilEmbeddingReport(t *testing.T) {
	payload, err := json.Marshal(RetrievalResponse{
		Snapshot: Snapshot{
			ID:       "snapshot",
			AsOf:     time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC),
			Frontier: 1,
		},
		Retrieval: retrieval.Response{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(payload, []byte(`"embedding"`)) {
		t.Fatalf("nil embedding report was serialized: %s", payload)
	}
}

func TestRetrievalResponseEmbeddingReportBoundsOpaqueIDs(t *testing.T) {
	for _, test := range []struct {
		name    string
		size    int
		wantErr bool
	}{
		{name: "empty", wantErr: true},
		{name: "one opaque byte", size: 1},
		{name: "maximum opaque ID", size: shoal.MaxIDBytes},
		{name: "oversized opaque ID", size: shoal.MaxIDBytes + 1, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			report := testEmbeddingReport()
			report.Spaces[0].ID = shoal.ID(bytes.Repeat([]byte{0xff}, test.size))
			response := RetrievalResponse{
				Snapshot: Snapshot{
					ID: "snapshot", AsOf: time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC),
					Frontier: 1,
				},
				Retrieval: retrieval.Response{},
				Embedding: &report,
			}
			payload, err := json.Marshal(response)
			if err != nil {
				t.Fatal(err)
			}
			var decoded RetrievalResponse
			err = json.Unmarshal(payload, &decoded)
			if test.wantErr {
				if err == nil {
					t.Fatalf("accepted embedding space ID with %d bytes", test.size)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if decoded.Embedding == nil || len(decoded.Embedding.Spaces) != 1 ||
				decoded.Embedding.Spaces[0].ID != report.Spaces[0].ID {
				t.Fatal("valid opaque embedding space ID was not preserved")
			}
		})
	}
}

func TestEmbeddingQueryErrorWireAndRemoteRoundTrip(t *testing.T) {
	report := testEmbeddingReport()
	recorder := httptest.NewRecorder()
	writeError(
		recorder,
		newEmbeddingQueryError(
			shoal.NewError(
				shoal.ErrorUnavailable,
				"an authorized embedding provider is unavailable",
			),
			report,
		),
	)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
	if !strings.Contains(recorder.Body.String(), `"embedding"`) ||
		!strings.Contains(recorder.Body.String(), `"status":"unavailable"`) {
		t.Fatalf("error wire omitted embedding report: %s", recorder.Body.String())
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		switch request.URL.Path {
		case "/api/v1/meta":
			writeResponse(writer, http.StatusOK, MetadataResponse{
				MaxPageSize: MaxPageSize, MaxTopK: MaxTopK,
				MaxDepth: MaxDepth, MaxFanout: MaxFanout,
				MaxNodes: MaxNodes,
				Capabilities: Capabilities{
					Documents: true, Document: true, Retrieve: true,
					Neighborhood: true, Path: true,
				},
			})
		case "/api/v1/retrieve":
			writeError(
				writer,
				newEmbeddingQueryError(
					shoal.NewError(
						shoal.ErrorUnavailable,
						"an authorized embedding provider is unavailable",
					),
					report,
				),
			)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer upstream.Close()
	remote, err := NewRemoteService(upstream.URL, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = remote.Retrieve(context.Background(), RetrievalRequest{
		Query: retrieval.Request{
			Text: "query", Modes: []retrieval.Mode{retrieval.ModeVector},
			Scope: retrieval.Scope{DocumentIDs: []shoal.ID{"document"}},
		},
	})
	if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("remote error = %v", err)
	}
	var reported *EmbeddingQueryError
	if !errors.As(err, &reported) {
		t.Fatalf("remote error lost embedding report: %T %v", err, err)
	}
	if actual := reported.EmbeddingQueryReport(); !reflect.DeepEqual(actual, report) {
		t.Fatalf("remote report = %+v, want %+v", actual, report)
	}
}

type reportingClientStub struct {
	explorer.BoundedClient
	snapshot explorer.Snapshot
	response retrieval.Response
	report   authorized.RetrievalReport
	err      error
}

func (c *reportingClientStub) Snapshot(context.Context) (explorer.Snapshot, error) {
	return c.snapshot, nil
}

func (c *reportingClientStub) RetrieveWithReport(
	context.Context,
	retrieval.Request,
) (retrieval.Response, authorized.RetrievalReport, error) {
	return c.response, c.report, c.err
}

func TestEmbeddedServicePreservesAuthorizedEmbeddingReport(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	report := testEmbeddingReport()
	client := &reportingClientStub{
		snapshot: explorer.Snapshot{ID: "snapshot", AsOf: now, Frontier: 1},
		response: retrieval.Response{},
		report: authorized.RetrievalReport{
			Disclosure: authorized.Disclosure{Suppressed: 2},
			Embedding:  &report,
		},
	}
	service, err := NewEmbeddedService(client)
	if err != nil {
		t.Fatal(err)
	}
	response, err := service.Retrieve(context.Background(), RetrievalRequest{
		Query: retrieval.Request{
			Text: "query", Modes: []retrieval.Mode{retrieval.ModeVector},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Suppressed != 2 ||
		response.Embedding == nil ||
		!reflect.DeepEqual(*response.Embedding, report) {
		t.Fatalf("service response = %+v", response)
	}

	client.err = shoal.NewError(
		shoal.ErrorUnavailable,
		"an authorized embedding provider is unavailable",
	)
	_, err = service.Retrieve(context.Background(), RetrievalRequest{
		Query: retrieval.Request{
			Text: "query", Modes: []retrieval.Mode{retrieval.ModeVector},
		},
	})
	var reported *EmbeddingQueryError
	if !errors.As(err, &reported) {
		t.Fatalf("service error lost embedding report: %T %v", err, err)
	}
	if actual := reported.EmbeddingQueryReport(); !reflect.DeepEqual(actual, report) {
		t.Fatalf("service error report = %+v, want %+v", actual, report)
	}
}
