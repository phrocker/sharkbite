// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file distributed
// with this work for additional information regarding copyright ownership.
package webapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type graphMaterializationHTTPService struct {
	request GraphMaterializeRequest
}

func (*graphMaterializationHTTPService) Documents(context.Context, DocumentsRequest) (DocumentsResponse, error) {
	return DocumentsResponse{}, nil
}
func (*graphMaterializationHTTPService) Document(context.Context, DocumentRequest) (DocumentResponse, error) {
	return DocumentResponse{}, nil
}
func (*graphMaterializationHTTPService) Retrieve(context.Context, RetrievalRequest) (RetrievalResponse, error) {
	return RetrievalResponse{}, nil
}
func (*graphMaterializationHTTPService) Neighborhood(context.Context, NeighborhoodRequest) (NeighborhoodResponse, error) {
	return NeighborhoodResponse{}, nil
}
func (*graphMaterializationHTTPService) Path(context.Context, PathRequest) (PathResponse, error) {
	return PathResponse{}, nil
}
func (s *graphMaterializationHTTPService) MaterializeGraph(
	_ context.Context, request GraphMaterializeRequest,
) (GraphMaterializeResponse, error) {
	s.request = request
	return GraphMaterializeResponse{
		MaterializationID: encodeID(shoal.ID([]byte{0, 0xff})),
		MutationID:        request.MutationID, Disposition: explorer.IngestApplied,
		Snapshot: request.Snapshot,
		Nodes: []GraphMaterializeIdentity{{
			Key: request.Nodes[0].Key, ID: encodeID("node"),
		}},
	}, nil
}

func TestGraphMaterializationHTTPPreservesOpaqueKeys(t *testing.T) {
	service := &graphMaterializationHTTPService{}
	server := httptest.NewUnstartedServer(nil)
	handler, err := NewHandler(service, server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server.Config.Handler = handler
	server.Start()
	defer server.Close()
	opaque := []byte{0, 0xfe, 0xff}
	body, err := json.Marshal(GraphMaterializeRequest{
		Namespace:  base64.RawURLEncoding.EncodeToString(opaque),
		SourceID:   encodeID("source"),
		PolicyID:   encodeID("policy"),
		MutationID: encodeID("mutation"),
		Snapshot: Snapshot{
			ID: "snapshot", AsOf: time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC),
			Frontier: 1,
		},
		Nodes: []GraphMaterializeNode{{
			Key: base64.RawURLEncoding.EncodeToString(opaque), Kind: "team",
			Properties: shoal.Metadata{"name": "Demo"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.Client().Post(
		server.URL+"/api/v1/graph/materialize", "application/json",
		bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if service.request.Namespace != base64.RawURLEncoding.EncodeToString(opaque) {
		t.Fatalf("namespace = %q", service.request.Namespace)
	}
	var decoded GraphMaterializeResponse
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	id, err := base64.RawURLEncoding.DecodeString(decoded.MaterializationID)
	if err != nil || !bytes.Equal(id, []byte{0, 0xff}) {
		t.Fatalf("materialization ID = %q, %v", decoded.MaterializationID, err)
	}
}
