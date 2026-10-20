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
	"context"
	"encoding/base64"
	"reflect"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func newObservedTestServer(
	t *testing.T, service webapi.Service, now time.Time,
) (*Server, *testInteractionSink) {
	t.Helper()
	sink := &testInteractionSink{}
	recorder, err := interaction.NewRecorder(context.Background(), sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.SetClock(func() time.Time {
		return now.Add(time.Second)
	}); err != nil {
		t.Fatal(err)
	}
	server, _ := newTestServer(t, service, nil)
	server.recorder = recorder
	server.interactionNow = func() time.Time { return now.Add(time.Second) }
	server.snapshots = testSnapshotProvider{snapshot: explorer.Snapshot{
		ID: "snapshot", AsOf: now,
	}}
	makeReady(t, server)
	return server, sink
}

func recordedToolCall(
	t *testing.T, sink *testInteractionSink,
) interaction.ToolCall {
	t.Helper()
	for _, session := range sink.sessions {
		if session.Operation != interaction.OperationToolCall {
			continue
		}
		for _, turn := range session.Turns {
			if turn.ToolCall != nil && len(turn.ToolCall.RetrievedEvidence) > 0 {
				return *turn.ToolCall
			}
		}
	}
	t.Fatal("no tool call with evidence was recorded")
	return interaction.ToolCall{}
}

// TestNeighborhoodToolRecordsNodesTheEdgesDoNotReach: a neighborhood may hold
// a node no edge reaches, such as a second seed with no neighbors. The
// recorded evidence must still cover every retrieved node, or the
// observation refuses itself as incomplete and the result is withheld. Such a
// node is a single-node graph anchor.
func TestNeighborhoodToolRecordsNodesTheEdgesDoNotReach(t *testing.T) {
	now := time.Now().UTC()
	edge := graph.Edge{
		ID: "edge-a-b", From: "node-a", To: "node-b", Type: "links", Weight: 1,
	}
	service := &stubService{
		neighborhood: func(
			context.Context, webapi.NeighborhoodRequest,
		) (webapi.NeighborhoodResponse, error) {
			return webapi.NeighborhoodResponse{
				Snapshot: webapi.Snapshot{ID: "snapshot", AsOf: now},
				Neighborhood: explorer.Neighborhood{
					Nodes: []graph.Node{
						{ID: "node-a", Kind: "entity"},
						{ID: "node-b", Kind: "entity"},
						{ID: "node-c", Kind: "entity"},
					},
					Edges: []graph.Edge{edge},
				},
			}, nil
		},
	}
	server, sink := newObservedTestServer(t, service, now)
	encoded := func(id string) string {
		return base64.RawURLEncoding.EncodeToString([]byte(id))
	}
	response := callToolRequest(t, server, ToolNeighborhood,
		`{"node_ids":["`+encoded("node-a")+`","`+encoded("node-c")+`"],"depth":1}`)
	if result := decodeToolResult(t, response); result.IsError {
		t.Fatalf("mixed neighborhood was withheld: %s", result.StructuredContent)
	}
	call := recordedToolCall(t, sink)
	if !reflect.DeepEqual(call.RetrievedNodeIDs,
		[]shoal.ID{"node-a", "node-b", "node-c"}) {
		t.Fatalf("retrieved nodes = %v", call.RetrievedNodeIDs)
	}
	var edges, singletons int
	for _, evidence := range call.RetrievedEvidence {
		switch {
		case len(evidence.EdgeIDs) == 1 && evidence.EdgeIDs[0] == edge.ID &&
			reflect.DeepEqual(evidence.NodeIDs, []shoal.ID{edge.From, edge.To}):
			edges++
		case len(evidence.EdgeIDs) == 0 &&
			reflect.DeepEqual(evidence.NodeIDs, []shoal.ID{"node-c"}):
			singletons++
		default:
			t.Fatalf("unexpected evidence %+v", evidence)
		}
	}
	if edges != 1 || singletons != 1 {
		t.Fatalf("recorded %d edge anchors and %d single-node anchors, want 1 and 1",
			edges, singletons)
	}
}

// TestRetrievalToolKeepsARepeatedDocumentAnchorOnce: a service may return
// several evidence paths for one citation. The document anchor is derived
// from the cited bytes, so those paths share it. It is recorded once rather
// than refused as a duplicate, and each path keeps its own graph anchor.
func TestRetrievalToolKeepsARepeatedDocumentAnchorOnce(t *testing.T) {
	now := time.Now().UTC()
	citation := document.Citation{
		DocumentID: "document", RevisionID: "revision",
		SectionID: "section", SpanID: "span",
		Range: document.SourceRange{
			Start: document.SourcePosition{Offset: 1},
			End:   document.SourcePosition{Offset: 5},
		},
	}
	service := &stubService{
		retrieve: func(
			context.Context, webapi.RetrievalRequest,
		) (webapi.RetrievalResponse, error) {
			return webapi.RetrievalResponse{
				Snapshot: webapi.Snapshot{ID: "snapshot", AsOf: now},
				Retrieval: retrieval.Response{Results: []retrieval.Result{{
					ID: "span",
					Evidence: []retrieval.Evidence{
						{
							Citation: citation, Quote: "text",
							Path: graph.Path{
								Nodes: []graph.Node{{ID: "document"}, {ID: "section"}},
								Edges: []graph.Edge{{
									ID: "document-section", From: "document",
									To: "section", Type: "contains",
								}},
							},
						},
						{
							Citation: citation, Quote: "text",
							Path: graph.Path{
								Nodes: []graph.Node{{ID: "document"}, {ID: "span"}},
								Edges: []graph.Edge{{
									ID: "document-span", From: "document",
									To: "span", Type: "contains",
								}},
							},
						},
					},
				}}},
			}, nil
		},
	}
	server, sink := newObservedTestServer(t, service, now)
	response := callToolRequest(t, server, ToolRetrieve,
		`{"query":{"text":"evidence","top_k":1,"modes":["lexical"]}}`)
	if result := decodeToolResult(t, response); result.IsError {
		t.Fatalf("retrieval with a shared citation was withheld: %s",
			result.StructuredContent)
	}
	call := recordedToolCall(t, sink)
	var documents, paths int
	for _, evidence := range call.RetrievedEvidence {
		switch evidence.Kind {
		case interaction.EvidenceDocument:
			documents++
		case interaction.EvidenceGraph:
			paths++
		}
	}
	if documents != 1 || paths != 2 {
		t.Fatalf("recorded %d document anchors and %d graph anchors, want 1 and 2",
			documents, paths)
	}
}
