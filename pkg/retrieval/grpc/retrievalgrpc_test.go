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

package retrievalgrpc_test

import (
	"context"
	"errors"
	"math"
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/knowledgepb"
	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	retrievalgrpc "github.com/phrocker/shoal-oss/pkg/retrieval/grpc"
	"github.com/phrocker/shoal-oss/pkg/shoal"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type fakeRetriever struct {
	retrieve func(context.Context, retrieval.Request) (retrieval.Response, error)
}

func (f fakeRetriever) Retrieve(
	ctx context.Context, request retrieval.Request,
) (retrieval.Response, error) {
	return f.retrieve(ctx, request)
}

func TestInProcessAndLoopbackParity(t *testing.T) {
	request := retrieval.Request{
		Text: "why did the deployment fail?",
		TopK: 3,
		Modes: []retrieval.Mode{
			retrieval.ModeLexical, retrieval.ModeVector, retrieval.ModeGraph,
		},
		Scope: retrieval.Scope{
			DocumentIDs: []shoal.ID{"doc-1"},
			NodeIDs:     []shoal.ID{"node-1"},
		},
		AsOf:    time.Date(2026, time.August, 22, 8, 0, 0, 0, time.UTC),
		Explain: true,
	}
	embeddingSpace, err := retrieval.EmbeddingSpaceIdentityID("space-v3")
	if err != nil {
		t.Fatal(err)
	}
	embeddingSpaceSet, err := retrieval.EmbeddingSpaceSetID(embeddingSpace)
	if err != nil {
		t.Fatal(err)
	}
	want := retrieval.Response{
		RequestID:         "request-1",
		EmbeddingSpaceID:  embeddingSpaceSet,
		EmbeddingSpaceIDs: []shoal.ID{embeddingSpace},
		Results: []retrieval.Result{{
			ID:    "result-1",
			Score: 0.91,
			Evidence: []retrieval.Evidence{{
				Citation: document.Citation{
					DocumentID: "doc-1",
					RevisionID: "revision-2",
					SectionID:  "section-3",
					SpanID:     "span-4",
					Range: document.SourceRange{
						Start: document.SourcePosition{Offset: 12, Page: 2},
						End:   document.SourcePosition{Offset: 48, Page: 2},
					},
				},
				Quote: "the rollout was canceled",
				Path: graph.Path{
					Nodes: []graph.Node{
						{
							ID:         "node-1",
							Kind:       "deployment",
							Labels:     []string{"production"},
							Properties: shoal.Metadata{"region": "west"},
						},
						{ID: "node-2", Kind: "incident"},
					},
					Edges: []graph.Edge{{
						ID:         "edge-1",
						From:       "node-1",
						To:         "node-2",
						Type:       "caused",
						Weight:     0.8,
						Properties: shoal.Metadata{"source": "timeline"},
					}},
				},
				Score: 0.95,
			}},
			Explanation: &retrieval.Explanation{
				Modes:   []retrieval.Mode{retrieval.ModeLexical, retrieval.ModeGraph},
				Summary: "matched the rollout and followed its incident edge",
				Scores:  map[string]shoal.Score{"lexical": 0.7, "graph": 0.95},
			},
		}},
	}

	var received []retrieval.Request
	fake := fakeRetriever{retrieve: func(
		_ context.Context, got retrieval.Request,
	) (retrieval.Response, error) {
		received = append(received, got)
		return want, nil
	}}
	remote, stop := startLoopback(t, fake)
	defer stop()

	inProcessResponse, err := fake.Retrieve(context.Background(), request)
	if err != nil {
		t.Fatalf("in-process Retrieve: %v", err)
	}
	loopbackResponse, err := remote.Retrieve(context.Background(), request)
	if err != nil {
		t.Fatalf("loopback Retrieve: %v", err)
	}

	if !reflect.DeepEqual(loopbackResponse, inProcessResponse) {
		t.Fatalf("loopback response = %#v, want %#v", loopbackResponse, inProcessResponse)
	}
	if len(received) != 2 {
		t.Fatalf("retriever received %d requests, want 2", len(received))
	}
	if !reflect.DeepEqual(received[0], received[1]) {
		t.Fatalf("loopback request = %#v, want in-process request %#v", received[1], received[0])
	}
}

func TestLoopbackValidationAndStableErrorCodes(t *testing.T) {
	returned := shoal.NewError(shoal.ErrorInternal, "not configured")
	fake := fakeRetriever{retrieve: func(
		context.Context, retrieval.Request,
	) (retrieval.Response, error) {
		return retrieval.Response{}, returned
	}}
	remote, stop := startLoopback(t, fake)
	defer stop()

	if _, err := remote.Retrieve(context.Background(), retrieval.Request{}); !shoal.IsErrorCode(
		err, shoal.ErrorInvalidArgument,
	) {
		t.Fatalf("empty request error = %v, want invalid argument", err)
	}

	tests := []shoal.ErrorCode{
		shoal.ErrorInvalidArgument,
		shoal.ErrorNotFound,
		shoal.ErrorConflict,
		shoal.ErrorUnauthorized,
		shoal.ErrorUnavailable,
		shoal.ErrorCanceled,
		shoal.ErrorDeadline,
		shoal.ErrorInternal,
	}
	for _, code := range tests {
		t.Run(string(code), func(t *testing.T) {
			returned = shoal.NewError(code, "retrieval failed")
			_, err := remote.Retrieve(context.Background(), retrieval.Request{Text: "query"})
			if !shoal.IsErrorCode(err, code) {
				t.Fatalf("error = %v, want code %q", err, code)
			}
		})
	}

	returned = nil
	_, err := remote.Retrieve(context.Background(), retrieval.Request{Text: "query"})
	if !shoal.IsErrorCode(err, shoal.ErrorInternal) {
		t.Fatalf("typed nil error = %v, want internal", err)
	}
}

func TestClientRejectsMalformedResponse(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*knowledgepb.RetrieveResponse)
	}{
		{
			name: "missing citation",
			mutate: func(response *knowledgepb.RetrieveResponse) {
				response.Results[0].Evidence[0].Citation = nil
			},
		},
		{
			name: "invalid citation",
			mutate: func(response *knowledgepb.RetrieveResponse) {
				response.Results[0].Evidence[0].Citation.RevisionId = ""
			},
		},
		{
			name: "empty present path",
			mutate: func(response *knowledgepb.RetrieveResponse) {
				response.Results[0].Evidence[0].Path = &knowledgepb.GraphPath{}
			},
		},
		{
			name: "nonfinite result score",
			mutate: func(response *knowledgepb.RetrieveResponse) {
				response.Results[0].Score = math.NaN()
			},
		},
		{
			name: "nonfinite evidence score",
			mutate: func(response *knowledgepb.RetrieveResponse) {
				response.Results[0].Evidence[0].Score = math.Inf(1)
			},
		},
		{
			name: "nonfinite path score",
			mutate: func(response *knowledgepb.RetrieveResponse) {
				response.Results[0].Evidence[0].Path = validProtoPath()
				response.Results[0].Evidence[0].Path.Edges[0].Weight = math.NaN()
			},
		},
		{
			name: "nonfinite explanation score",
			mutate: func(response *knowledgepb.RetrieveResponse) {
				response.Results[0].Explanation = &knowledgepb.Explanation{
					Scores: map[string]float64{"rank": math.Inf(-1)},
				}
			},
		},
		{
			name: "unknown explanation mode",
			mutate: func(response *knowledgepb.RetrieveResponse) {
				response.Results[0].Explanation = &knowledgepb.Explanation{
					Modes: []knowledgepb.RetrievalMode{knowledgepb.RetrievalMode(99)},
				}
			},
		},
		{
			name: "duplicate result ID",
			mutate: func(response *knowledgepb.RetrieveResponse) {
				response.Results = append(
					response.Results, validProtoResponse().Results[0])
			},
		},
		{
			name: "nondeterministic result order",
			mutate: func(response *knowledgepb.RetrieveResponse) {
				higher := validProtoResponse().Results[0]
				higher.Id = "result-2"
				higher.Score = response.Results[0].Score + 1
				response.Results = append(response.Results, higher)
			},
		},
		{
			name: "nondeterministic evidence order",
			mutate: func(response *knowledgepb.RetrieveResponse) {
				earlier := validProtoResponse().Results[0].Evidence[0]
				earlier.Citation.DocumentId = "doc-0"
				response.Results[0].Evidence = append(
					response.Results[0].Evidence, earlier)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := validProtoResponse()
			test.mutate(response)
			remote, stop := startRawLoopback(t, response)
			defer stop()

			_, err := remote.Retrieve(context.Background(), retrieval.Request{Text: "query"})
			if !shoal.IsErrorCode(err, shoal.ErrorInternal) {
				t.Fatalf("error = %v, want internal", err)
			}
		})
	}
}

func TestClientRejectsResponseOverTopK(t *testing.T) {
	response := validProtoResponse()
	second := validProtoResponse().Results[0]
	second.Id = "result-2"
	second.Score = response.Results[0].Score - 0.1
	response.Results = append(response.Results, second)
	remote, stop := startRawLoopback(t, response)
	defer stop()

	_, err := remote.Retrieve(context.Background(), retrieval.Request{
		Text: "query", TopK: 1,
	})
	if !shoal.IsErrorCode(err, shoal.ErrorInternal) {
		t.Fatalf("error = %v, want internal", err)
	}
}

func TestServerRejectsInvalidOutgoingResponse(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*retrieval.Response)
	}{
		{
			name: "missing citation",
			mutate: func(response *retrieval.Response) {
				response.Results[0].Evidence[0].Citation = document.Citation{}
			},
		},
		{
			name: "invalid citation",
			mutate: func(response *retrieval.Response) {
				response.Results[0].Evidence[0].Citation.Range.End.Offset = -1
			},
		},
		{
			name: "invalid present path",
			mutate: func(response *retrieval.Response) {
				response.Results[0].Evidence[0].Path = graph.Path{
					Nodes: []graph.Node{{ID: "node-1"}, {ID: "node-2"}},
				}
			},
		},
		{
			name: "nonfinite result score",
			mutate: func(response *retrieval.Response) {
				response.Results[0].Score = shoal.Score(math.NaN())
			},
		},
		{
			name: "nonfinite evidence score",
			mutate: func(response *retrieval.Response) {
				response.Results[0].Evidence[0].Score = shoal.Score(math.Inf(1))
			},
		},
		{
			name: "nonfinite path score",
			mutate: func(response *retrieval.Response) {
				response.Results[0].Evidence[0].Path = validPublicPath()
				response.Results[0].Evidence[0].Path.Edges[0].Weight =
					shoal.Score(math.NaN())
			},
		},
		{
			name: "nonfinite explanation score",
			mutate: func(response *retrieval.Response) {
				response.Results[0].Explanation = &retrieval.Explanation{
					Scores: map[string]shoal.Score{"rank": shoal.Score(math.Inf(-1))},
				}
			},
		},
		{
			name: "unknown explanation mode",
			mutate: func(response *retrieval.Response) {
				response.Results[0].Explanation = &retrieval.Explanation{
					Modes: []retrieval.Mode{"future"},
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := validPublicResponse()
			test.mutate(&response)
			fake := fakeRetriever{retrieve: func(
				context.Context, retrieval.Request,
			) (retrieval.Response, error) {
				return response, nil
			}}
			connection, stop := startGRPCLoopback(t, func(server *grpc.Server) {
				retrievalgrpc.RegisterServer(server, fake)
			})
			defer stop()

			_, err := knowledgepb.NewKnowledgeRetrievalClient(connection).Retrieve(
				context.Background(), &knowledgepb.RetrieveRequest{Text: "query"})
			if status.Code(err) != codes.Internal {
				t.Fatalf("gRPC code = %v, want internal (error: %v)", status.Code(err), err)
			}
		})
	}
}

func TestServerRejectsInvalidUTF8OutgoingResponse(t *testing.T) {
	invalid := string([]byte{0xff})
	tests := []struct {
		name   string
		mutate func(*retrieval.Response)
	}{
		{
			name: "request ID",
			mutate: func(response *retrieval.Response) {
				response.RequestID = shoal.ID(invalid)
			},
		},
		{
			name: "embedding space ID",
			mutate: func(response *retrieval.Response) {
				response.EmbeddingSpaceID = shoal.ID(invalid)
			},
		},
		{
			name: "embedding space constituent ID",
			mutate: func(response *retrieval.Response) {
				constituent := shoal.ID(invalid)
				response.EmbeddingSpaceIDs = []shoal.ID{constituent}
				response.EmbeddingSpaceID, _ =
					retrieval.EmbeddingSpaceSetID(constituent)
			},
		},
		{
			name: "result ID",
			mutate: func(response *retrieval.Response) {
				response.Results[0].ID = shoal.ID(invalid)
			},
		},
		{
			name: "citation document ID",
			mutate: func(response *retrieval.Response) {
				response.Results[0].Evidence[0].Citation.DocumentID = shoal.ID(invalid)
			},
		},
		{
			name: "citation revision ID",
			mutate: func(response *retrieval.Response) {
				response.Results[0].Evidence[0].Citation.RevisionID = shoal.ID(invalid)
			},
		},
		{
			name: "citation section ID",
			mutate: func(response *retrieval.Response) {
				response.Results[0].Evidence[0].Citation.SectionID = shoal.ID(invalid)
			},
		},
		{
			name: "citation span ID",
			mutate: func(response *retrieval.Response) {
				response.Results[0].Evidence[0].Citation.SpanID = shoal.ID(invalid)
			},
		},
		{
			name: "evidence quote",
			mutate: func(response *retrieval.Response) {
				response.Results[0].Evidence[0].Quote = invalid
			},
		},
		{
			name: "node ID",
			mutate: func(response *retrieval.Response) {
				path := validPublicPath()
				path.Nodes[0].ID = shoal.ID(invalid)
				path.Edges[0].From = shoal.ID(invalid)
				response.Results[0].Evidence[0].Path = path
			},
		},
		{
			name: "node kind",
			mutate: func(response *retrieval.Response) {
				path := validPublicPath()
				path.Nodes[0].Kind = invalid
				response.Results[0].Evidence[0].Path = path
			},
		},
		{
			name: "node label",
			mutate: func(response *retrieval.Response) {
				path := validPublicPath()
				path.Nodes[0].Labels = []string{invalid}
				response.Results[0].Evidence[0].Path = path
			},
		},
		{
			name: "node property key",
			mutate: func(response *retrieval.Response) {
				path := validPublicPath()
				path.Nodes[0].Properties = shoal.Metadata{invalid: "value"}
				response.Results[0].Evidence[0].Path = path
			},
		},
		{
			name: "node property value",
			mutate: func(response *retrieval.Response) {
				path := validPublicPath()
				path.Nodes[0].Properties = shoal.Metadata{"key": invalid}
				response.Results[0].Evidence[0].Path = path
			},
		},
		{
			name: "edge ID",
			mutate: func(response *retrieval.Response) {
				path := validPublicPath()
				path.Edges[0].ID = shoal.ID(invalid)
				response.Results[0].Evidence[0].Path = path
			},
		},
		{
			name: "edge endpoint",
			mutate: func(response *retrieval.Response) {
				path := validPublicPath()
				path.Nodes[1].ID = shoal.ID(invalid)
				path.Edges[0].To = shoal.ID(invalid)
				response.Results[0].Evidence[0].Path = path
			},
		},
		{
			name: "edge type",
			mutate: func(response *retrieval.Response) {
				path := validPublicPath()
				path.Edges[0].Type = invalid
				response.Results[0].Evidence[0].Path = path
			},
		},
		{
			name: "edge property key",
			mutate: func(response *retrieval.Response) {
				path := validPublicPath()
				path.Edges[0].Properties = shoal.Metadata{invalid: "value"}
				response.Results[0].Evidence[0].Path = path
			},
		},
		{
			name: "edge property value",
			mutate: func(response *retrieval.Response) {
				path := validPublicPath()
				path.Edges[0].Properties = shoal.Metadata{"key": invalid}
				response.Results[0].Evidence[0].Path = path
			},
		},
		{
			name: "explanation summary",
			mutate: func(response *retrieval.Response) {
				response.Results[0].Explanation = &retrieval.Explanation{Summary: invalid}
			},
		},
		{
			name: "explanation score name",
			mutate: func(response *retrieval.Response) {
				response.Results[0].Explanation = &retrieval.Explanation{
					Scores: map[string]shoal.Score{invalid: 0.5},
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := validPublicResponse()
			test.mutate(&response)
			fake := fakeRetriever{retrieve: func(
				context.Context, retrieval.Request,
			) (retrieval.Response, error) {
				return response, nil
			}}
			connection, stop := startGRPCLoopback(t, func(server *grpc.Server) {
				retrievalgrpc.RegisterServer(server, fake)
			})
			defer stop()

			_, err := knowledgepb.NewKnowledgeRetrievalClient(connection).Retrieve(
				context.Background(), &knowledgepb.RetrieveRequest{Text: "query"})
			if status.Code(err) != codes.Internal {
				t.Fatalf("gRPC code = %v, want internal (error: %v)", status.Code(err), err)
			}
		})
	}
}

func TestServerRejectsUnknownRequestEnum(t *testing.T) {
	called := false
	fake := fakeRetriever{retrieve: func(
		context.Context, retrieval.Request,
	) (retrieval.Response, error) {
		called = true
		return retrieval.Response{}, nil
	}}
	connection, stop := startGRPCLoopback(t, func(server *grpc.Server) {
		retrievalgrpc.RegisterServer(server, fake)
	})
	defer stop()

	_, err := knowledgepb.NewKnowledgeRetrievalClient(connection).Retrieve(
		context.Background(),
		&knowledgepb.RetrieveRequest{
			Text:  "query",
			Modes: []knowledgepb.RetrievalMode{knowledgepb.RetrievalMode(99)},
		},
	)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("gRPC code = %v, want invalid argument (error: %v)", status.Code(err), err)
	}
	if called {
		t.Fatal("retriever was called for an unknown request mode")
	}
}

func TestClientRetainsWireSpecificUTF8Checks(t *testing.T) {
	called := false
	fake := fakeRetriever{retrieve: func(
		context.Context, retrieval.Request,
	) (retrieval.Response, error) {
		called = true
		return retrieval.Response{}, nil
	}}
	remote, stop := startLoopback(t, fake)
	defer stop()

	_, err := remote.Retrieve(context.Background(), retrieval.Request{
		Text: "query",
		Scope: retrieval.Scope{
			DocumentIDs: []shoal.ID{shoal.ID(string([]byte{0xff}))},
		},
	})
	if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("invalid wire ID error = %v", err)
	}
	if called {
		t.Fatal("retriever was called for a non-UTF-8 protobuf string")
	}
}

func TestZeroValueParityAndEmptyNormalization(t *testing.T) {
	response := retrieval.Response{}
	var received []retrieval.Request
	fake := fakeRetriever{retrieve: func(
		_ context.Context, request retrieval.Request,
	) (retrieval.Response, error) {
		received = append(received, request)
		return response, nil
	}}
	remote, stop := startLoopback(t, fake)
	defer stop()

	request := retrieval.Request{Text: "query"}
	normalizedRequest, err := request.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	inProcess, err := fake.Retrieve(context.Background(), normalizedRequest)
	if err != nil {
		t.Fatalf("in-process Retrieve: %v", err)
	}
	loopback, err := remote.Retrieve(context.Background(), request)
	if err != nil {
		t.Fatalf("loopback Retrieve: %v", err)
	}
	if !reflect.DeepEqual(loopback, inProcess) {
		t.Fatalf("loopback response = %#v, want %#v", loopback, inProcess)
	}
	if len(received) != 2 || !reflect.DeepEqual(received[0], received[1]) {
		t.Fatalf("received requests = %#v", received)
	}

	response = retrieval.Response{Results: []retrieval.Result{}}
	emptyRequest := retrieval.Request{
		Text:  "query",
		Modes: []retrieval.Mode{},
		Scope: retrieval.Scope{
			DocumentIDs: []shoal.ID{},
			NodeIDs:     []shoal.ID{},
		},
	}
	normalized, err := remote.Retrieve(context.Background(), emptyRequest)
	if err != nil {
		t.Fatalf("normalized Retrieve: %v", err)
	}
	if normalized.Results != nil {
		t.Fatalf("results = %#v, want normalized nil", normalized.Results)
	}
	gotRequest := received[len(received)-1]
	if gotRequest.TopK != retrieval.DefaultTopK ||
		!reflect.DeepEqual(gotRequest.Modes, []retrieval.Mode{retrieval.ModeLexical}) ||
		gotRequest.Scope.DocumentIDs != nil ||
		gotRequest.Scope.NodeIDs != nil {
		t.Fatalf("request was not normalized: %#v", gotRequest)
	}

	response = validPublicResponse()
	response.Results[0].Evidence[0].Path = graph.Path{Nodes: []graph.Node{}}
	response.Results[0].Explanation = &retrieval.Explanation{}
	normalized, err = remote.Retrieve(context.Background(), request)
	if err != nil {
		t.Fatalf("optional-value Retrieve: %v", err)
	}
	result := normalized.Results[0]
	if result.Explanation == nil {
		t.Fatal("present empty explanation was discarded")
	}
	if result.Explanation.Modes != nil || result.Explanation.Scores != nil {
		t.Fatalf("explanation was not normalized: %#v", result.Explanation)
	}
	if result.Evidence[0].Path.Nodes != nil || result.Evidence[0].Path.Edges != nil {
		t.Fatalf("zero path was not normalized: %#v", result.Evidence[0].Path)
	}
}

func TestNewClientRejectsNilConnection(t *testing.T) {
	if _, err := retrievalgrpc.NewClient(nil); !shoal.IsErrorCode(
		err, shoal.ErrorInvalidArgument,
	) {
		t.Fatalf("nil connection error = %v, want invalid argument", err)
	}

	var connection *grpc.ClientConn
	if _, err := retrievalgrpc.NewClient(connection); !shoal.IsErrorCode(
		err, shoal.ErrorInvalidArgument,
	) {
		t.Fatalf("typed nil connection error = %v, want invalid argument", err)
	}
}

func TestLoopbackPropagatesCancellation(t *testing.T) {
	started := make(chan struct{})
	fake := fakeRetriever{retrieve: func(
		ctx context.Context, _ retrieval.Request,
	) (retrieval.Response, error) {
		close(started)
		<-ctx.Done()
		return retrieval.Response{}, ctx.Err()
	}}
	remote, stop := startLoopback(t, fake)
	defer stop()

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := remote.Retrieve(ctx, retrieval.Request{Text: "query"})
		result <- err
	}()
	<-started
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context canceled", err)
		}
		if !shoal.IsErrorCode(err, shoal.ErrorCanceled) {
			t.Fatalf("error = %v, want public canceled code", err)
		}
	case <-time.After(time.Second):
		t.Fatal("loopback retrieval did not observe cancellation")
	}
}

func TestLoopbackPropagatesDeadline(t *testing.T) {
	fake := fakeRetriever{retrieve: func(
		ctx context.Context, _ retrieval.Request,
	) (retrieval.Response, error) {
		<-ctx.Done()
		return retrieval.Response{}, ctx.Err()
	}}
	remote, stop := startLoopback(t, fake)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := remote.Retrieve(ctx, retrieval.Request{Text: "query"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context deadline exceeded", err)
	}
	if !shoal.IsErrorCode(err, shoal.ErrorDeadline) {
		t.Fatalf("error = %v, want public deadline code", err)
	}
}

func startLoopback(t *testing.T, retriever retrieval.Retriever) (retrieval.Retriever, func()) {
	t.Helper()

	connection, stop := startGRPCLoopback(t, func(server *grpc.Server) {
		retrievalgrpc.RegisterServer(server, retriever)
	})
	client, err := retrievalgrpc.NewClient(connection)
	if err != nil {
		stop()
		t.Fatalf("create retrieval client: %v", err)
	}
	return client, stop
}

func startRawLoopback(
	t *testing.T, response *knowledgepb.RetrieveResponse,
) (retrieval.Retriever, func()) {
	t.Helper()

	connection, stop := startGRPCLoopback(t, func(server *grpc.Server) {
		knowledgepb.RegisterKnowledgeRetrievalServer(
			server, staticKnowledgeServer{response: response})
	})
	client, err := retrievalgrpc.NewClient(connection)
	if err != nil {
		stop()
		t.Fatalf("create retrieval client: %v", err)
	}
	return client, stop
}

func startGRPCLoopback(
	t *testing.T, register func(*grpc.Server),
) (*grpc.ClientConn, func()) {
	t.Helper()

	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	register(server)
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			t.Errorf("serve loopback gRPC: %v", err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	connection, err := grpc.DialContext(
		ctx,
		"bufconn",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return listener.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		server.Stop()
		_ = listener.Close()
		t.Fatalf("dial loopback gRPC: %v", err)
	}

	return connection, func() {
		_ = connection.Close()
		server.Stop()
		_ = listener.Close()
	}
}

type staticKnowledgeServer struct {
	knowledgepb.UnimplementedKnowledgeRetrievalServer
	response *knowledgepb.RetrieveResponse
}

func (s staticKnowledgeServer) Retrieve(
	context.Context, *knowledgepb.RetrieveRequest,
) (*knowledgepb.RetrieveResponse, error) {
	return s.response, nil
}

func validProtoResponse() *knowledgepb.RetrieveResponse {
	return &knowledgepb.RetrieveResponse{
		RequestId: "request-1",
		Results: []*knowledgepb.RetrievalResult{{
			Id:    "result-1",
			Score: 0.9,
			Evidence: []*knowledgepb.Evidence{{
				Citation: &knowledgepb.Citation{
					DocumentId: "doc-1",
					RevisionId: "revision-1",
					SectionId:  "section-1",
					Range: &knowledgepb.SourceRange{
						Start: &knowledgepb.SourcePosition{},
						End:   &knowledgepb.SourcePosition{Offset: 1},
					},
				},
				Score: 0.8,
			}},
		}},
	}
}

func validPublicResponse() retrieval.Response {
	return retrieval.Response{
		RequestID: "request-1",
		Results: []retrieval.Result{{
			ID:    "result-1",
			Score: 0.9,
			Evidence: []retrieval.Evidence{{
				Citation: document.Citation{
					DocumentID: "doc-1",
					RevisionID: "revision-1",
					SectionID:  "section-1",
					Range: document.SourceRange{
						End: document.SourcePosition{Offset: 1},
					},
				},
				Score: 0.8,
			}},
		}},
	}
}

func validProtoPath() *knowledgepb.GraphPath {
	return &knowledgepb.GraphPath{
		Nodes: []*knowledgepb.GraphNode{{Id: "node-1"}, {Id: "node-2"}},
		Edges: []*knowledgepb.GraphEdge{{
			Id: "edge-1", From: "node-1", To: "node-2", Type: "supports", Weight: 0.5,
		}},
	}
}

func validPublicPath() graph.Path {
	return graph.Path{
		Nodes: []graph.Node{{ID: "node-1"}, {ID: "node-2"}},
		Edges: []graph.Edge{{
			ID: "edge-1", From: "node-1", To: "node-2", Type: "supports", Weight: 0.5,
		}},
	}
}

var _ retrieval.Retriever = fakeRetriever{}
