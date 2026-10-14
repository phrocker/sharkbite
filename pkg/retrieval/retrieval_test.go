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

package retrieval_test

import (
	"context"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type stubRetriever struct{}

func (stubRetriever) Retrieve(context.Context, retrieval.Request) (retrieval.Response, error) {
	return retrieval.Response{RequestID: "request-1"}, nil
}

func TestRetrieverUsesTransportNeutralValues(t *testing.T) {
	var client retrieval.Retriever = stubRetriever{}
	response, err := client.Retrieve(context.Background(), retrieval.Request{
		Text:  "why did the deployment fail?",
		TopK:  5,
		Modes: []retrieval.Mode{retrieval.ModeTree, retrieval.ModeGraph},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.RequestID != "request-1" {
		t.Fatalf("unexpected request ID %q", response.RequestID)
	}
}

func TestRequestNormalizeClonesDefaultsAndDeduplicates(t *testing.T) {
	asOf := time.Date(2026, 8, 26, 9, 0, 0, 0, time.FixedZone("offset", -4*60*60))
	request := retrieval.Request{
		Text:  "query",
		Modes: []retrieval.Mode{retrieval.ModeGraph, retrieval.ModeLexical, retrieval.ModeGraph},
		Scope: retrieval.Scope{
			DocumentIDs: []shoal.ID{"doc\x00", "doc\x00", shoal.ID("\xff")},
			NodeIDs:     []shoal.ID{"node", "node"},
		},
		AsOf: asOf,
	}
	normalized, err := request.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if normalized.TopK != retrieval.DefaultTopK {
		t.Fatalf("TopK = %d", normalized.TopK)
	}
	if !reflect.DeepEqual(
		normalized.Modes,
		[]retrieval.Mode{retrieval.ModeGraph, retrieval.ModeLexical},
	) {
		t.Fatalf("modes = %#v", normalized.Modes)
	}
	if !reflect.DeepEqual(
		normalized.Scope.DocumentIDs,
		[]shoal.ID{"doc\x00", shoal.ID("\xff")},
	) || !reflect.DeepEqual(normalized.Scope.NodeIDs, []shoal.ID{"node"}) {
		t.Fatalf("scope = %#v", normalized.Scope)
	}
	if normalized.AsOf.Location() != time.UTC || !normalized.AsOf.Equal(asOf) {
		t.Fatalf("AsOf = %v", normalized.AsOf)
	}
	normalized.Modes[0] = retrieval.ModeTree
	normalized.Scope.DocumentIDs[0] = "changed"
	if request.Modes[0] != retrieval.ModeGraph ||
		request.Scope.DocumentIDs[0] != "doc\x00" {
		t.Fatal("Normalize mutated caller-owned slices")
	}

	idempotent, err := normalized.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(idempotent, normalized) {
		t.Fatalf("second normalization changed request: %#v", idempotent)
	}
}

func TestRequestNormalizeDefaultsToLexical(t *testing.T) {
	normalized, err := (retrieval.Request{Text: "query"}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(normalized.Modes, []retrieval.Mode{retrieval.ModeLexical}) {
		t.Fatalf("modes = %#v", normalized.Modes)
	}
}

func TestRequestValidationBounds(t *testing.T) {
	tests := map[string]retrieval.Request{
		"unknown mode": {Text: "query", Modes: []retrieval.Mode{"cells"}},
		"invalid UTF-8 text": {
			Text: string([]byte{0xff}),
		},
		"oversized query": {
			Text: strings.Repeat("x", retrieval.MaxQueryBytes+1),
		},
		"oversized top k": {
			Text: "query", TopK: retrieval.MaxTopK + 1,
		},
		"empty scope ID": {
			Text: "query", Scope: retrieval.Scope{DocumentIDs: []shoal.ID{""}},
		},
		"oversized scope ID": {
			Text: "query",
			Scope: retrieval.Scope{
				DocumentIDs: []shoal.ID{
					shoal.ID(strings.Repeat("x", shoal.MaxIDBytes+1)),
				},
			},
		},
	}
	for name, request := range tests {
		t.Run(name, func(t *testing.T) {
			if err := request.Validate(); !shoal.IsErrorCode(
				err, shoal.ErrorInvalidArgument,
			) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestRequestRejectsAsOfOutsideProtobufRange(t *testing.T) {
	for _, asOf := range []time.Time{
		time.Date(0, time.December, 31, 23, 59, 59, 0, time.UTC),
		time.Date(10000, time.January, 1, 0, 0, 0, 0, time.UTC),
	} {
		request := retrieval.Request{Text: "query", AsOf: asOf}
		if err := request.Validate(); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
			t.Fatalf("Validate() for %v = %v, want invalid argument", asOf, err)
		}
	}
}

func TestSeedPlanRejectsUnboundedStandaloneModes(t *testing.T) {
	for _, request := range []retrieval.Request{
		{Text: "query", Modes: []retrieval.Mode{retrieval.ModeTree}},
		{Text: "query", Modes: []retrieval.Mode{retrieval.ModeGraph}},
		{
			Text:  "query",
			Modes: []retrieval.Mode{retrieval.ModeTree, retrieval.ModeGraph},
		},
		{
			Text: "query", Modes: []retrieval.Mode{retrieval.ModeGraph},
			Scope: retrieval.Scope{DocumentIDs: []shoal.ID{"doc"}},
		},
	} {
		if err := request.ValidateSeedPlan(false); !shoal.IsErrorCode(
			err, shoal.ErrorUnavailable,
		) {
			t.Fatalf("request %#v error = %v", request, err)
		}
	}
	for _, request := range []retrieval.Request{
		{Text: "query"},
		{
			Text: "query", Modes: []retrieval.Mode{retrieval.ModeTree},
			Scope: retrieval.Scope{DocumentIDs: []shoal.ID{"doc"}},
		},
		{
			Text: "query", Modes: []retrieval.Mode{retrieval.ModeGraph},
			Scope: retrieval.Scope{NodeIDs: []shoal.ID{"node"}},
		},
		{
			Text: "query",
			Modes: []retrieval.Mode{
				retrieval.ModeGraph, retrieval.ModeTree, retrieval.ModeLexical,
			},
		},
	} {
		if err := request.ValidateSeedPlan(false); err != nil {
			t.Fatalf("bounded request %#v: %v", request, err)
		}
	}
}

func TestEvidenceAndResponseValidation(t *testing.T) {
	evidence := testEvidence("doc", "span", 1)
	if err := evidence.Validate(); err != nil {
		t.Fatalf("evidence: %v", err)
	}
	evidence.Score = shoal.Score(math.NaN())
	if err := evidence.Validate(); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("non-finite evidence error = %v", err)
	}

	response := retrieval.Response{
		RequestID: "request",
		Results: []retrieval.Result{
			{ID: shoal.ID("\x00b"), Score: 1, Evidence: []retrieval.Evidence{
				testEvidence("doc", "b", 2),
				testEvidence("doc", "a", 2),
			}},
			{ID: shoal.ID("\xff"), Score: 1},
			{ID: "lower", Score: 0.5},
		},
	}
	retrieval.SortResults(&response)
	if response.Results[0].ID != shoal.ID("\x00b") ||
		response.Results[1].ID != shoal.ID("\xff") ||
		response.Results[0].Evidence[0].Citation.SpanID != "a" {
		t.Fatalf("deterministic order = %#v", response.Results)
	}
	if err := response.ValidateFor(retrieval.Request{
		Text: "query", TopK: 3,
	}); err != nil {
		t.Fatalf("response validation: %v", err)
	}
}

func TestResponseRejectsDuplicatesOrderAndTopK(t *testing.T) {
	tests := map[string]retrieval.Response{
		"duplicate": {
			Results: []retrieval.Result{
				{ID: "same", Score: 2}, {ID: "same", Score: 1},
			},
		},
		"order": {
			Results: []retrieval.Result{
				{ID: "low", Score: 1}, {ID: "high", Score: 2},
			},
		},
		"top k": {
			Results: []retrieval.Result{
				{ID: "one", Score: 1}, {ID: "two", Score: 0},
			},
		},
	}

	for name, response := range tests {
		t.Run(name, func(t *testing.T) {
			topK := uint32(2)
			if name == "top k" {
				topK = 1
			}
			if err := response.ValidateFor(retrieval.Request{
				Text: "query", TopK: topK,
			}); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestResponseRejectsDuplicateEvidenceOrderingKey(t *testing.T) {
	first := testEvidence("doc", "span", 1)
	second := first
	second.Quote = "different serialized evidence"
	response := retrieval.Response{Results: []retrieval.Result{{
		ID:       "result",
		Score:    1,
		Evidence: []retrieval.Evidence{first, second},
	}}}
	if err := response.ValidateFor(retrieval.Request{
		Text: "query", TopK: 1,
	}); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("duplicate evidence ordering key error = %v", err)
	}

}

func testEvidence(documentID, spanID shoal.ID, score shoal.Score) retrieval.Evidence {
	return retrieval.Evidence{
		Citation: document.Citation{
			DocumentID: documentID,
			RevisionID: "revision",
			SectionID:  "section",
			SpanID:     spanID,
			Range: document.SourceRange{
				Start: document.SourcePosition{Offset: 0},
				End:   document.SourcePosition{Offset: 1},
			},
		},
		Path: graph.Path{
			Nodes: []graph.Node{{ID: "a"}, {ID: "b"}},
			Edges: []graph.Edge{{ID: "ab", From: "a", To: "b", Type: "links"}},
		},
		Score: score,
	}
}
