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

package webapi_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/model"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestServiceContractParity(t *testing.T) {
	service, corpus, first, second := testService(t)
	defer corpus.Close()
	assertServiceBoundsAndSnapshot(t, "embedded", service, first, second)

	remote, cleanup, first, second := testRemoteService(t)
	defer cleanup()
	assertServiceBoundsAndSnapshot(t, "remote", remote, first, second)
}

func TestRemoteServiceMatchesEmbeddedResponses(t *testing.T) {
	embedded, corpus, _, _ := testService(t)
	defer corpus.Close()
	remote, cleanup := serveRemoteService(t, embedded)
	defer cleanup()
	ctx := context.Background()

	documentsRequest := webapi.DocumentsRequest{Page: webapi.PageRequest{Limit: 1}}
	embeddedDocuments, err := embedded.Documents(ctx, documentsRequest)
	if err != nil {
		t.Fatal(err)
	}
	remoteDocuments, err := remote.Documents(ctx, documentsRequest)
	if err != nil {
		t.Fatal(err)
	}
	assertEqualJSON(t, "documents", embeddedDocuments, remoteDocuments)

	documentRequest := webapi.DocumentRequest{
		Snapshot:   embeddedDocuments.Snapshot,
		DocumentID: embeddedDocuments.Documents[0].Document.ID,
		RevisionID: embeddedDocuments.Documents[0].Revision.ID,
	}
	embeddedDocument, err := embedded.Document(ctx, documentRequest)
	if err != nil {
		t.Fatal(err)
	}
	remoteDocument, err := remote.Document(ctx, documentRequest)
	if err != nil {
		t.Fatal(err)
	}
	assertEqualJSON(t, "document", embeddedDocument, remoteDocument)

	retrievalRequest := webapi.RetrievalRequest{
		Snapshot: embeddedDocuments.Snapshot,
		Query: retrieval.Request{
			Text: "bounded retries", TopK: 2,
			Modes: []retrieval.Mode{retrieval.ModeLexical, retrieval.ModeTree},
			AsOf:  embeddedDocuments.Snapshot.AsOf, Explain: true,
		},
	}
	embeddedRetrieval, err := embedded.Retrieve(ctx, retrievalRequest)
	if err != nil {
		t.Fatal(err)
	}
	remoteRetrieval, err := remote.Retrieve(ctx, retrievalRequest)
	if err != nil {
		t.Fatal(err)
	}
	assertEqualJSON(t, "retrieval", embeddedRetrieval, remoteRetrieval)

	ingestRequest := webapi.IngestRequest{
		Files: []webapi.UploadFile{{
			Name:    "remote.go",
			Content: []byte("package remote\n\nconst ExactCitation = \"remote upload\"\n"),
		}},
	}
	remoteIngest, err := remote.Ingest(ctx, ingestRequest)
	if err != nil {
		t.Fatal(err)
	}
	if len(remoteIngest.Files) != 1 ||
		remoteIngest.Files[0].Disposition != explorer.IngestApplied ||
		remoteIngest.Files[0].MediaType != explorer.MediaTypeSource {
		t.Fatalf("remote ingest = %+v", remoteIngest)
	}
	remoteUnchanged, err := remote.Ingest(ctx, ingestRequest)
	if err != nil {
		t.Fatal(err)
	}
	if len(remoteUnchanged.Files) != 1 ||
		remoteUnchanged.Files[0].Disposition != explorer.IngestUnchanged {
		t.Fatalf("remote repeated ingest = %+v", remoteUnchanged)
	}
	// Superseding the upload makes its first content historical (#674): the
	// stable "historical" wire value round-trips and the remote client
	// accepts it.
	superseded, err := remote.Ingest(ctx, webapi.IngestRequest{
		Files: []webapi.UploadFile{{
			Name:    "remote.go",
			Content: []byte("package remote\n\nconst ExactCitation = \"superseded\"\n"),
		}},
	})
	if err != nil || len(superseded.Files) != 1 ||
		superseded.Files[0].Disposition != explorer.IngestApplied {
		t.Fatalf("remote superseding ingest = %+v, %v", superseded, err)
	}
	remoteHistorical, err := remote.Ingest(ctx, ingestRequest)
	if err != nil {
		t.Fatal(err)
	}
	if len(remoteHistorical.Files) != 1 ||
		remoteHistorical.Files[0].Disposition != explorer.IngestHistorical ||
		remoteHistorical.Files[0].Revision.ID != remoteIngest.Files[0].Revision.ID {
		t.Fatalf("remote historical ingest = %+v", remoteHistorical)
	}
	encoded, err := json.Marshal(remoteHistorical)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"disposition":"historical"`) {
		t.Fatalf("historical wire encoding = %s", encoded)
	}
}

func assertServiceBoundsAndSnapshot(
	t *testing.T,
	name string,
	service webapi.Service,
	first explorer.IngestResult,
	second explorer.IngestResult,
) {
	t.Helper()
	ctx := context.Background()
	documents, err := service.Documents(ctx, webapi.DocumentsRequest{
		Page: webapi.PageRequest{Limit: 1},
	})
	if err != nil {
		t.Fatalf("%s documents: %v", name, err)
	}
	if len(documents.Documents) != 1 || documents.NextCursor == "" ||
		documents.Snapshot.ID == "" || documents.Snapshot.AsOf.IsZero() {
		t.Fatalf("%s documents = %+v", name, documents)
	}
	next, err := service.Documents(ctx, webapi.DocumentsRequest{
		Snapshot: documents.Snapshot,
		Page:     webapi.PageRequest{Limit: 1, Cursor: documents.NextCursor},
	})
	if err != nil {
		t.Fatalf("%s next documents: %v", name, err)
	}
	if len(next.Documents) != 1 || next.Documents[0].Document.ID == documents.Documents[0].Document.ID {
		t.Fatalf("%s next page = %+v", name, next)
	}
	_, err = service.Documents(ctx, webapi.DocumentsRequest{
		Page: webapi.PageRequest{Limit: webapi.MaxPageSize + 1},
	})
	if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("%s page bound error = %v", name, err)
	}
	_, err = service.Retrieve(ctx, webapi.RetrievalRequest{
		Snapshot: documents.Snapshot,
		Query:    retrieval.Request{Text: "bounded", TopK: webapi.MaxTopK + 1},
	})
	if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("%s top-k bound error = %v", name, err)
	}
	_, err = service.Retrieve(ctx, webapi.RetrievalRequest{
		Query: retrieval.Request{
			Text: "bounded", TopK: 1, AsOf: documents.Snapshot.AsOf,
		},
	})
	if err != nil {
		t.Fatalf("%s unpinned retrieval as_of: %v", name, err)
	}
	neighborhood, err := service.Neighborhood(ctx, webapi.NeighborhoodRequest{
		Snapshot: documents.Snapshot,
		NodeIDs:  []shoal.ID{first.Document.ID, second.Document.ID},
		Depth:    2, Fanout: 1, MaxNodes: 2,
	})
	if err != nil {
		t.Fatalf("%s neighborhood: %v", name, err)
	}
	if len(neighborhood.Neighborhood.Nodes) > 2 ||
		len(neighborhood.Neighborhood.Edges) > 1 {
		t.Fatalf("%s unbounded neighborhood = %+v", name, neighborhood)
	}
	nodeIDs := map[shoal.ID]bool{}
	for _, node := range neighborhood.Neighborhood.Nodes {
		nodeIDs[node.ID] = true
	}
	if !nodeIDs[first.Document.ID] || !nodeIDs[second.Document.ID] {
		t.Fatalf("%s requested seeds were not reserved: %+v", name, neighborhood)
	}
	firstPage, err := service.Neighborhood(ctx, webapi.NeighborhoodRequest{
		Snapshot: documents.Snapshot,
		NodeIDs:  []shoal.ID{first.Document.ID, first.Document.ID},
		Depth:    1, Fanout: 10, MaxNodes: 2,
	})
	if err != nil {
		t.Fatalf("%s first graph page: %v", name, err)
	}
	if firstPage.NextCursor == "" || len(firstPage.Neighborhood.Edges) != 1 {
		t.Fatalf("%s first graph page = %+v", name, firstPage)
	}
	secondPage, err := service.Neighborhood(ctx, webapi.NeighborhoodRequest{
		Snapshot: documents.Snapshot,
		NodeIDs:  []shoal.ID{first.Document.ID, first.Document.ID},
		Depth:    1, Fanout: 10, MaxNodes: 2, Cursor: firstPage.NextCursor,
	})
	if err != nil {
		t.Fatalf("%s second graph page: %v", name, err)
	}
	if len(secondPage.Neighborhood.Edges) != 1 ||
		secondPage.Neighborhood.Edges[0].ID == firstPage.Neighborhood.Edges[0].ID {
		t.Fatalf("%s second graph page = %+v", name, secondPage)
	}
	path, err := service.Path(ctx, webapi.PathRequest{
		Snapshot: documents.Snapshot, From: first.Document.ID,
		To: second.Document.ID, MaxDepth: 2, Fanout: 2,
	})
	if err != nil {
		t.Fatalf("%s path: %v", name, err)
	}
	if err := path.Path.Validate(); err != nil {
		t.Fatalf("%s path: %v", name, err)
	}
}

func TestSnapshotChangesWhenGraphChanges(t *testing.T) {
	service, corpus, first, second := testService(t)
	defer corpus.Close()
	ctx := context.Background()
	documents, err := service.Documents(ctx, webapi.DocumentsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if err := corpus.Connect(ctx, graph.Edge{
		ID: "reverse-edge", From: second.Document.ID, To: first.Document.ID,
		Type: "references", Weight: 1,
	}); err != nil {
		t.Fatal(err)
	}
	_, err = service.Neighborhood(ctx, webapi.NeighborhoodRequest{
		Snapshot: documents.Snapshot, NodeIDs: []shoal.ID{first.Document.ID},
	})
	if !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("stale graph snapshot error = %v", err)
	}
}

func TestPathFanoutCountsOnlyOutgoingEdges(t *testing.T) {
	service, corpus, first, second := testService(t)
	defer corpus.Close()
	ctx := context.Background()
	firstView, err := corpus.Document(ctx, first.Document.ID, first.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	secondView, err := corpus.Document(ctx, second.Document.ID, second.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	from := firstSpanID(t, firstView.Root)
	to := firstSpanID(t, secondView.Root)
	if err := corpus.Connect(ctx, graph.Edge{
		ID: "a-incoming", From: to, To: from, Type: "noise", Weight: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := corpus.Connect(ctx, graph.Edge{
		ID: "z-outgoing", From: from, To: to, Type: "path", Weight: 1,
	}); err != nil {
		t.Fatal(err)
	}
	documents, err := service.Documents(ctx, webapi.DocumentsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	path, err := service.Path(ctx, webapi.PathRequest{
		Snapshot: documents.Snapshot, From: from, To: to,
		MaxDepth: 1, Fanout: 1, EdgeTypes: []string{"noise", "path"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(path.Path.Edges) != 1 || path.Path.Edges[0].ID != "z-outgoing" {
		t.Fatalf("path = %+v", path.Path)
	}
}

func TestHTTPIDsAreBinarySafeAndReversible(t *testing.T) {
	rawID := shoal.ID([]byte{0xff, 0x00, 'x'})
	encoded, err := json.Marshal(webapi.NeighborhoodResponse{
		Neighborhood: explorer.Neighborhood{
			Nodes: []graph.Node{{
				ID: rawID, Kind: "binary",
				Properties: shoal.Metadata{string([]byte{0xfe}): string([]byte{0xfd})},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if bytes.Contains(encoded, []byte("\ufffd")) {
		t.Fatalf("JSON replaced opaque ID bytes: %s", encoded)
	}
	var wire struct {
		Neighborhood struct {
			Nodes []struct {
				ID string `json:"id"`
			} `json:"nodes"`
		} `json:"neighborhood"`
	}
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	requestJSON, err := json.Marshal(map[string]any{
		"node_ids":  []string{wire.Neighborhood.Nodes[0].ID},
		"depth":     1,
		"fanout":    1,
		"max_nodes": 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	var request webapi.NeighborhoodRequest
	if err := json.Unmarshal(requestJSON, &request); err != nil {
		t.Fatal(err)
	}
	if len(request.NodeIDs) != 1 || request.NodeIDs[0] != rawID {
		t.Fatalf("ID round trip = %q", request.NodeIDs)
	}
}

func TestPathResponseRoundTripsOntologyInterpretationReports(t *testing.T) {
	response := webapi.PathResponse{
		OntologyInterpretations: []webapi.OntologyInterpretationReport{{
			AssertionID: "assertion-1", SchemaID: "schema-1", VersionID: "version-2",
			Reading: ontology.OntologyOtherVersion, Status: ontology.InterpretationResolved,
			OriginalPredicate: "relationship-old", Predicate: "relationship-new",
			AppliedMorphisms: []string{"morphism-1"},
		}},
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var decoded webapi.PathResponse
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.OntologyInterpretations) != 1 ||
		decoded.OntologyInterpretations[0].Predicate != "relationship-new" ||
		decoded.OntologyInterpretations[0].Status != ontology.InterpretationResolved {
		t.Fatalf("interpretation round trip = %#v", decoded.OntologyInterpretations)
	}
}

func TestSnapshotFrontierJSONRoundTrip(t *testing.T) {
	snapshot := webapi.Snapshot{
		ID: "snapshot", AsOf: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Frontier: math.MaxUint64,
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, []byte(`"frontier":"18446744073709551615"`)) {
		t.Fatalf("frontier is not an exact string: %s", encoded)
	}
	var decoded webapi.Snapshot
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded != snapshot {
		t.Fatalf("snapshot round trip = %+v", decoded)
	}
}

func TestEvidenceSerializationPreservesCitationAndExplanation(t *testing.T) {
	service, corpus, _, _ := testService(t)
	defer corpus.Close()
	documents, err := service.Documents(context.Background(), webapi.DocumentsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	response, err := service.Retrieve(context.Background(), webapi.RetrievalRequest{
		Snapshot: documents.Snapshot,
		Query: retrieval.Request{
			Text: "bounded retries", TopK: 2,
			Modes:   []retrieval.Mode{retrieval.ModeLexical, retrieval.ModeTree},
			Explain: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Retrieval struct {
			Results []struct {
				ID       string `json:"id"`
				Evidence []struct {
					Quote    string `json:"quote"`
					Citation struct {
						DocumentID string `json:"document_id"`
					} `json:"citation"`
				} `json:"evidence"`
				Explanation json.RawMessage `json:"explanation"`
			} `json:"results"`
		} `json:"retrieval"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Retrieval.Results) == 0 ||
		len(decoded.Retrieval.Results[0].Evidence) == 0 {
		t.Fatalf("missing evidence: %s", encoded)
	}
	evidence := decoded.Retrieval.Results[0].Evidence[0]
	if evidence.Quote == "" || evidence.Citation.DocumentID == "" ||
		len(decoded.Retrieval.Results[0].Explanation) == 0 {
		t.Fatalf("lost evidence fields: %+v", decoded)
	}
}

func TestHTTPHandlerValidationAndWorkspace(t *testing.T) {
	service, corpus, _, _ := testService(t)
	defer corpus.Close()
	server := httptest.NewUnstartedServer(nil)
	handler, err := webapi.NewHandler(service, server.Listener.Addr().String())
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	server.Config.Handler = handler
	server.Start()
	defer server.Close()

	response, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK ||
		!strings.HasPrefix(response.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("workspace response = %s %s", response.Status, response.Header)
	}

	response, err = http.Post(
		server.URL+"/api/v1/documents", "text/plain",
		bytes.NewBufferString(`{"page":{"limit":1}}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("plain-text request status = %s", response.Status)
	}

	response, err = http.Post(
		server.URL+"/api/v1/documents", "application/json",
		bytes.NewBufferString(`{"page":{"limit":1},"unexpected":true}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("validation status = %s", response.Status)
	}

	response, err = http.Post(
		server.URL+"/api/v1/documents", "application/json",
		bytes.NewBufferString(`{"page":{"limit":1}}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("documents status = %s", response.Status)
	}
	if response.Header.Get("Content-Security-Policy") == "" {
		t.Fatal("missing content security policy")
	}

	upload := postMultipartWithoutWorkspaceHeader(t, server.URL+"/api/v1/ingest", uploadFixture{
		name: "csrf.txt", content: "cross site upload",
	})
	if upload.StatusCode != http.StatusUnauthorized {
		t.Fatalf("upload without workspace header status = %s: %s", upload.Status, upload.body)
	}

	request, err := http.NewRequest(http.MethodGet, server.URL+"/api/v1/meta", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = "attacker.example"
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("forged host status = %s", response.Status)
	}
}

func TestHTTPIngestUploadsDocumentsAndSourceCode(t *testing.T) {
	dataDir := t.TempDir()
	corpus, err := explorer.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	service, err := webapi.NewEmbeddedService(corpus)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(nil)
	handler, err := webapi.NewHandler(service, server.Listener.Addr().String())
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	server.Config.Handler = handler
	server.Start()
	defer server.Close()

	uploads := []uploadFixture{
		{name: "guide.md", content: "# Guide\n\nMarkdown evidence survives.\n", mediaType: explorer.MediaTypeMarkdown},
		{name: "notes.txt", content: "Plain text exact citation.\n", mediaType: explorer.MediaTypeText},
		{name: "main.go", content: "package main\n\nfunc main() {\n\tprintln(\"code exact citation\")\n}\n", mediaType: explorer.MediaTypeSource},
	}
	response := postMultipart(t, server.URL+"/api/v1/ingest", uploads...)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("upload status = %s: %s", response.Status, response.body)
	}
	var ingested webapi.IngestResponse
	if err := json.Unmarshal(response.body, &ingested); err != nil {
		t.Fatal(err)
	}
	if len(ingested.Files) != len(uploads) || ingested.Snapshot.ID == "" {
		t.Fatalf("ingest response = %+v", ingested)
	}
	for index, upload := range uploads {
		item := ingested.Files[index]
		if item.Name != upload.name ||
			item.MediaType != upload.mediaType ||
			item.Disposition != explorer.IngestApplied ||
			item.Document.Title != upload.name ||
			item.SectionCount == 0 ||
			item.SpanCount == 0 {
			t.Fatalf("ingest item %d = %+v", index, item)
		}
		view, err := service.Document(context.Background(), webapi.DocumentRequest{
			Snapshot:   ingested.Snapshot,
			DocumentID: item.Document.ID,
			RevisionID: item.Revision.ID,
		})
		if err != nil {
			t.Fatal(err)
		}
		assertViewSpansMatchSource(t, view.Document.Root, upload.content)
		if view.Document.SourceMediaType != upload.mediaType {
			t.Fatalf("document media type = %q, want %q", view.Document.SourceMediaType, upload.mediaType)
		}
	}

	repeated := postMultipart(t, server.URL+"/api/v1/ingest", uploads[2])
	if repeated.StatusCode != http.StatusOK {
		t.Fatalf("repeat upload status = %s: %s", repeated.Status, repeated.body)
	}
	var unchanged webapi.IngestResponse
	if err := json.Unmarshal(repeated.body, &unchanged); err != nil {
		t.Fatal(err)
	}
	if len(unchanged.Files) != 1 || unchanged.Files[0].Disposition != explorer.IngestUnchanged {
		t.Fatalf("repeat response = %+v", unchanged)
	}

	documents := postJSON(t, server.URL+"/api/v1/documents", `{"page":{"limit":10}}`)
	if bytes.Contains(documents, []byte(dataDir)) ||
		bytes.Contains(response.body, []byte(dataDir)) {
		t.Fatalf("response leaked a host path: ingest=%s documents=%s", response.body, documents)
	}
	if !bytes.Contains(documents, []byte(`"source_media_type":"text/x-source-code"`)) {
		t.Fatalf("documents response did not expose source media type: %s", documents)
	}
}

func TestHTTPIngestRecognizesSkillFiles(t *testing.T) {
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	service, err := webapi.NewEmbeddedService(corpus)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(nil)
	handler, err := webapi.NewHandler(service, server.Listener.Addr().String())
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	server.Config.Handler = handler
	server.Start()
	defer server.Close()

	response := postMultipart(t, server.URL+"/api/v1/ingest",
		uploadFixture{
			name: "skills__alpha__SKILL.md",
			content: "---\n" +
				"name: alpha\n" +
				"description: Alpha skill for upload testing.\n" +
				"---\n\n" +
				"# Alpha\n",
		},
		uploadFixture{
			name: "skills__broken__SKILL.md",
			content: "---\n" +
				"name: broken\n" +
				"---\n\n" +
				"# Broken\n",
		},
	)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("skill upload status = %s: %s", response.Status, response.body)
	}
	var ingested webapi.IngestResponse
	if err := json.Unmarshal(response.body, &ingested); err != nil {
		t.Fatal(err)
	}
	if len(ingested.Files) != 2 {
		t.Fatalf("skill upload response file count = %d, want 2: %+v", len(ingested.Files), ingested)
	}
	valid := ingested.Files[0].SkillFile
	if valid == nil || !valid.Expected || !valid.Recognized ||
		valid.Name != "alpha" || valid.Description != "Alpha skill for upload testing." {
		t.Fatalf("valid skill metadata was not recognized: %+v", valid)
	}
	invalid := ingested.Files[1].SkillFile
	if invalid == nil || !invalid.Expected || invalid.Recognized ||
		!strings.Contains(invalid.Message, "frontmatter must include non-empty name and description") {
		t.Fatalf("invalid SKILL.md did not get actionable metadata: %+v", invalid)
	}
}

func TestHTTPIngestRejectsUntrustedUploads(t *testing.T) {
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	service, err := webapi.NewEmbeddedService(corpus)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(nil)
	handler, err := webapi.NewHandler(service, server.Listener.Addr().String())
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	server.Config.Handler = handler
	server.Start()
	defer server.Close()

	cases := []struct {
		name    string
		uploads []uploadFixture
	}{
		{
			name: "oversized",
			uploads: []uploadFixture{{
				name: "large.txt", content: strings.Repeat("a", int(webapi.MaxUploadFileBytes)+1),
			}},
		},
		{
			name: "too many files",
			uploads: func() []uploadFixture {
				files := make([]uploadFixture, webapi.MaxUploadFiles+1)
				for index := range files {
					files[index] = uploadFixture{
						name:    fmt.Sprintf("file-%02d.txt", index),
						content: "bounded",
					}
				}
				return files
			}(),
		},
		{
			name: "disallowed media type",
			uploads: []uploadFixture{{
				name: "image.png", content: "not an accepted text extension",
			}},
		},
		{
			name: "invalid utf8",
			uploads: []uploadFixture{{
				name: "bad.txt", content: string([]byte{0xff, 0xfe}),
			}},
		},
		{
			name: "binary control after sniff prefix",
			uploads: []uploadFixture{{
				name: "payload.go", content: strings.Repeat("a", 600) + "\x00",
			}},
		},
		{
			name: "traversal filename",
			uploads: []uploadFixture{{
				name: "..\\secret.txt", content: "secret",
			}},
		},
		{
			name: "absolute filename",
			uploads: []uploadFixture{{
				name: "C:\\Users\\build\\secret.txt", content: "secret",
			}},
		},
		{
			name: "embedded separator",
			uploads: []uploadFixture{{
				name: "nested/secret.txt", content: "secret",
			}},
		},
		{
			name: "format control filename",
			uploads: []uploadFixture{{
				name: "safe\u202egnp.txt", content: "secret",
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := postMultipart(t, server.URL+"/api/v1/ingest", tc.uploads...)
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %s: %s", response.Status, response.body)
			}
			if bytes.Contains(response.body, []byte(`C:\Users`)) ||
				bytes.Contains(response.body, []byte(`/secret.txt`)) ||
				bytes.Contains(response.body, []byte(`..\secret`)) {
				t.Fatalf("error leaked a hostile path: %s", response.body)
			}
		})
	}
}

func TestHTTPIngestRedactsProviderFailures(t *testing.T) {
	service, corpus, _, _ := testService(t)
	defer corpus.Close()
	for _, tc := range []struct {
		name   string
		err    error
		status int
	}{
		{
			name: "internal",
			err: shoal.NewError(
				shoal.ErrorInternal,
				`write C:\private\shoal\tablet.rf failed`,
			),
			status: http.StatusInternalServerError,
		},
		{
			name: "invalid argument",
			err: shoal.NewError(
				shoal.ErrorInvalidArgument,
				`invalid C:\private\source.txt upload`,
			),
			status: http.StatusBadRequest,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewUnstartedServer(nil)
			handler, err := webapi.NewHandler(failingIngestService{
				Service: service,
				err:     tc.err,
			}, server.Listener.Addr().String())
			if err != nil {
				server.Close()
				t.Fatal(err)
			}
			server.Config.Handler = handler
			server.Start()
			defer server.Close()

			response := postMultipart(t, server.URL+"/api/v1/ingest", uploadFixture{
				name: "safe.txt", content: "safe text",
			})
			if response.StatusCode != tc.status {
				t.Fatalf("status = %s: %s", response.Status, response.body)
			}
			if bytes.Contains(response.body, []byte(`C:\private`)) ||
				!bytes.Contains(response.body, []byte(`upload failed`)) {
				t.Fatalf("provider failure was not redacted: %s", response.body)
			}
		})
	}
}

func TestMetadataAdvertisesCapabilities(t *testing.T) {
	service, corpus, _, _ := testService(t)
	defer corpus.Close()
	server := httptest.NewUnstartedServer(nil)
	handler, err := webapi.NewHandler(service, server.Listener.Addr().String())
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	server.Config.Handler = handler
	server.Start()
	defer server.Close()

	response, err := http.Get(server.URL + "/api/v1/meta")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("meta status = %s", response.Status)
	}
	var metadata webapi.MetadataResponse
	if err := json.NewDecoder(response.Body).Decode(&metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.MaxPageSize != webapi.MaxPageSize ||
		metadata.MaxResponseBytes != webapi.MaxResponseBytes ||
		metadata.MaxUploadFiles != webapi.MaxUploadFiles ||
		metadata.MaxUploadFileBytes != webapi.MaxUploadFileBytes ||
		!metadata.Capabilities.Supports(webapi.CapabilityRetrieve) ||
		!metadata.Capabilities.Supports(webapi.CapabilityPath) ||
		metadata.Capabilities.Supports(webapi.CapabilityVector) ||
		!metadata.Capabilities.Supports(webapi.CapabilityIngest) ||
		metadata.Capabilities.Supports(webapi.CapabilityChanges) {
		t.Fatalf("metadata = %+v", metadata)
	}

	capabilityOnly := capabilityOnlyService{Service: service}
	server = httptest.NewUnstartedServer(nil)
	handler, err = webapi.NewHandler(capabilityOnly, server.Listener.Addr().String())
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	server.Config.Handler = handler
	server.Start()
	defer server.Close()
	response, err = http.Get(server.URL + "/api/v1/meta")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if err := json.NewDecoder(response.Body).Decode(&metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Capabilities.Supports(webapi.CapabilityIngest) ||
		metadata.Capabilities.Supports(webapi.CapabilityChanges) {
		t.Fatalf("optional capabilities did not fail closed: %+v", metadata.Capabilities)
	}
}

func TestEmbeddedExtractionAvailabilityRequiresOntology(t *testing.T) {
	service, corpus, _, _ := testService(t)
	defer corpus.Close()
	capabilities, err := service.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if capabilities.Supports(webapi.CapabilityExtraction) ||
		capabilities.Supports(webapi.CapabilityChanges) {
		t.Fatalf("unconfigured extraction capability = %+v", capabilities)
	}
	if err := service.SetOntologyVersion(webapiSkillsOntologyVersion(t)); err != nil {
		t.Fatal(err)
	}
	capabilities, err = service.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !capabilities.Supports(webapi.CapabilityExtraction) {
		t.Fatalf("configured extraction capability = %+v", capabilities)
	}
}

func TestMetadataAdvertisesVectorOnlyWhenAvailable(t *testing.T) {
	dataDir := t.TempDir()
	corpus, err := explorer.OpenWithOptions(dataDir, explorer.Options{
		Embedder: model.FakeEmbedder{Model: "webapi", Dimensions: 8},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := corpus.Ingest(context.Background(), explorer.Source{
		URI:       "file:///vector.txt",
		MediaType: explorer.MediaTypeText,
		Content:   "vector capability is available",
	}); err != nil {
		t.Fatal(err)
	}
	service, err := webapi.NewEmbeddedService(corpus)
	if err != nil {
		t.Fatal(err)
	}
	capabilities, err := service.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !capabilities.Supports(webapi.CapabilityVector) {
		t.Fatalf("vector capabilities = %+v", capabilities)
	}
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := explorer.OpenWithOptions(dataDir, explorer.Options{
		Embedder: model.FakeEmbedder{Model: "other", Dimensions: 8},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	service, err = webapi.NewEmbeddedService(reopened)
	if err != nil {
		t.Fatal(err)
	}
	capabilities, err = service.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if capabilities.Supports(webapi.CapabilityVector) {
		t.Fatalf("mismatched vector capabilities = %+v", capabilities)
	}
}

func TestRemoteServiceCapabilityNegotiation(t *testing.T) {
	pathCalled := false
	retrieveCalled := false
	ingestCalled := false
	upstream := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/meta":
			writeJSON(t, writer, webapi.MetadataResponse{
				MaxPageSize: webapi.MaxPageSize, MaxTopK: webapi.MaxTopK,
				MaxDepth: webapi.MaxDepth, MaxFanout: webapi.MaxFanout,
				MaxNodes: webapi.MaxNodes,
				Capabilities: webapi.Capabilities{
					Documents: true, Document: true, Retrieve: true,
					Neighborhood: true, Path: false,
				},
			})
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/path":
			pathCalled = true
			http.Error(writer, "path should not be called", http.StatusInternalServerError)
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/retrieve":
			retrieveCalled = true
			http.Error(writer, "retrieve should not be called", http.StatusInternalServerError)
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/ingest":
			ingestCalled = true
			http.Error(writer, "ingest should not be called", http.StatusInternalServerError)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer upstream.Close()
	service, err := webapi.NewRemoteService(upstream.URL, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}

	capabilities, err := service.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if capabilities.Supports(webapi.CapabilityPath) {
		t.Fatalf("capabilities = %+v", capabilities)
	}
	_, err = service.Path(context.Background(), webapi.PathRequest{
		From: "from", To: "to", MaxDepth: 1, Fanout: 1,
	})
	if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("unsupported path error = %v", err)
	}
	if pathCalled {
		t.Fatal("remote path endpoint was called")
	}
	_, err = service.Retrieve(context.Background(), webapi.RetrievalRequest{
		Query: retrieval.Request{
			Text: "query", Modes: []retrieval.Mode{retrieval.ModeVector},
		},
	})
	if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) || retrieveCalled {
		t.Fatalf("unsupported vector retrieve error = %v, called = %t", err, retrieveCalled)
	}
	_, err = service.Ingest(context.Background(), webapi.IngestRequest{
		Files: []webapi.UploadFile{{Name: "guide.md", Content: []byte("# Guide\n")}},
	})
	if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("unsupported ingest error = %v", err)
	}
	if ingestCalled {
		t.Fatal("remote ingest endpoint was called")
	}

	documentsCalled := false
	missingCapabilities := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/meta":
			writeJSON(t, writer, map[string]any{
				"max_page_size": webapi.MaxPageSize,
				"max_top_k":     webapi.MaxTopK,
				"max_depth":     webapi.MaxDepth,
				"max_fanout":    webapi.MaxFanout,
				"max_nodes":     webapi.MaxNodes,
			})
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/documents":
			documentsCalled = true
			http.Error(writer, "documents should not be called", http.StatusInternalServerError)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer missingCapabilities.Close()
	missing, err := webapi.NewRemoteService(
		missingCapabilities.URL, missingCapabilities.Client())
	if err != nil {
		t.Fatal(err)
	}
	capabilities, err = missing.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if capabilities.Supports(webapi.CapabilityDocuments) {
		t.Fatalf("missing capabilities defaulted open: %+v", capabilities)
	}
	_, err = missing.Documents(context.Background(), webapi.DocumentsRequest{
		Page: webapi.PageRequest{Limit: 1},
	})
	if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) || documentsCalled {
		t.Fatalf("missing capabilities error = %v, documentsCalled = %t", err, documentsCalled)
	}

	insufficientBounds := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		if request.Method == http.MethodGet && request.URL.Path == "/api/v1/meta" {
			writeJSON(t, writer, webapi.MetadataResponse{
				MaxPageSize: webapi.MaxPageSize - 1, MaxTopK: webapi.MaxTopK,
				MaxDepth: webapi.MaxDepth, MaxFanout: webapi.MaxFanout,
				MaxNodes: webapi.MaxNodes, MaxUploadFiles: webapi.MaxUploadFiles,
				MaxUploadFileBytes:  webapi.MaxUploadFileBytes,
				MaxUploadTotalBytes: webapi.MaxUploadTotalBytes,
				Capabilities:        webapi.AllCapabilities(),
			})
			return
		}
		http.NotFound(writer, request)
	}))
	defer insufficientBounds.Close()
	insufficient, err := webapi.NewRemoteService(
		insufficientBounds.URL, insufficientBounds.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = insufficient.Capabilities(context.Background())
	if !shoal.IsErrorCode(err, shoal.ErrorInternal) {
		t.Fatalf("insufficient bounds error = %v", err)
	}

	insufficientUploadBounds := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		if request.Method == http.MethodGet && request.URL.Path == "/api/v1/meta" {
			writeJSON(t, writer, webapi.MetadataResponse{
				MaxPageSize: webapi.MaxPageSize, MaxTopK: webapi.MaxTopK,
				MaxDepth: webapi.MaxDepth, MaxFanout: webapi.MaxFanout,
				MaxNodes: webapi.MaxNodes, MaxEdgeTypes: webapi.MaxEdgeTypes,
				MaxResponseBytes:    webapi.MaxResponseBytes,
				MaxUploadFiles:      webapi.MaxUploadFiles,
				MaxUploadFileBytes:  webapi.MaxUploadFileBytes - 1,
				MaxUploadTotalBytes: webapi.MaxUploadTotalBytes,
				Capabilities:        webapi.AllCapabilities(),
			})
			return
		}
		http.NotFound(writer, request)
	}))
	defer insufficientUploadBounds.Close()
	insufficientUpload, err := webapi.NewRemoteService(
		insufficientUploadBounds.URL, insufficientUploadBounds.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = insufficientUpload.Capabilities(context.Background())
	if !shoal.IsErrorCode(err, shoal.ErrorInternal) {
		t.Fatalf("insufficient upload bounds error = %v", err)
	}
}

func TestRemoteServiceAllowsScopedVectorWhenGlobalCapabilityIsUnavailable(t *testing.T) {
	retrieveCalled := false
	now := time.Date(2026, time.August, 29, 12, 0, 0, 0, time.UTC)
	embeddingSpace, err := retrieval.EmbeddingSpaceIdentityID("space-v3")
	if err != nil {
		t.Fatal(err)
	}
	embeddingSpaceSet, err := retrieval.EmbeddingSpaceSetID(embeddingSpace)
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/meta":
			writeJSON(t, writer, webapi.MetadataResponse{
				MaxPageSize: webapi.MaxPageSize,
				MaxTopK:     webapi.MaxTopK,
				MaxDepth:    webapi.MaxDepth,
				MaxFanout:   webapi.MaxFanout,
				MaxNodes:    webapi.MaxNodes,
				Capabilities: webapi.Capabilities{
					Documents: true, Document: true, Retrieve: true,
					Neighborhood: true, Path: true, Vector: false,
				},
			})
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/retrieve":
			retrieveCalled = true
			writeJSON(t, writer, webapi.RetrievalResponse{
				Snapshot: webapi.Snapshot{
					ID: "snapshot", AsOf: now, Frontier: 1,
				},
				Retrieval: retrieval.Response{
					EmbeddingSpaceID:  embeddingSpaceSet,
					EmbeddingSpaceIDs: []shoal.ID{embeddingSpace},
				},
			})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer upstream.Close()
	service, err := webapi.NewRemoteService(upstream.URL, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	response, err := service.Retrieve(context.Background(), webapi.RetrievalRequest{
		Query: retrieval.Request{
			Text:  "query",
			Modes: []retrieval.Mode{retrieval.ModeVector},
			Scope: retrieval.Scope{DocumentIDs: []shoal.ID{
				"document",
			}},
		},
	})
	if err != nil {
		t.Fatalf("scoped vector retrieve: %v", err)
	}
	if response.Retrieval.EmbeddingSpaceID != embeddingSpaceSet ||
		len(response.Retrieval.EmbeddingSpaceIDs) != 1 ||
		response.Retrieval.EmbeddingSpaceIDs[0] != embeddingSpace {
		t.Fatalf("remote embedding space = %q",
			response.Retrieval.EmbeddingSpaceID)
	}
	if !retrieveCalled {
		t.Fatal("scoped vector request did not reach remote endpoint")
	}
}

func TestRemoteServiceErrorMappingAndLocalBounds(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/meta":
			writeJSON(t, writer, webapi.MetadataResponse{
				MaxPageSize: webapi.MaxPageSize, MaxTopK: webapi.MaxTopK,
				MaxDepth: webapi.MaxDepth, MaxFanout: webapi.MaxFanout,
				MaxNodes: webapi.MaxNodes, MaxUploadFiles: webapi.MaxUploadFiles,
				MaxUploadFileBytes:  webapi.MaxUploadFileBytes,
				MaxUploadTotalBytes: webapi.MaxUploadTotalBytes,
				Capabilities:        webapi.AllCapabilities(),
			})
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/documents":
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusBadRequest)
			writeJSON(t, writer, map[string]any{
				"code":    shoal.ErrorInvalidArgument,
				"message": "invalid_argument: remote invalid argument",
			})
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/retrieve":
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusGatewayTimeout)
			writeJSON(t, writer, map[string]any{
				"code":    shoal.ErrorDeadline,
				"message": "deadline_exceeded: remote deadline",
			})
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/path":
			writer.WriteHeader(http.StatusGatewayTimeout)
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/neighborhood":
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusInternalServerError)
			writeJSON(t, writer, map[string]any{
				"code":    shoal.ErrorNotFound,
				"message": "not_found: contradictory status",
			})
		default:
			http.NotFound(writer, request)
		}
	}))
	service, err := webapi.NewRemoteService(upstream.URL, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}

	_, err = service.Retrieve(context.Background(), webapi.RetrievalRequest{
		Query: retrieval.Request{Text: "bounded", TopK: webapi.MaxTopK + 1},
	})
	if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("local top-k bound error = %v", err)
	}
	_, err = service.Path(context.Background(), webapi.PathRequest{
		From: "from", To: "to", MaxDepth: 1, Fanout: 1,
		EdgeTypes: []string{string([]byte{0xff})},
	})
	if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("local path edge-type error = %v", err)
	}
	_, err = service.Documents(context.Background(), webapi.DocumentsRequest{
		Page: webapi.PageRequest{Limit: 1},
	})
	if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("remote invalid-argument error = %v", err)
	}
	if got, want := err.Error(), "invalid_argument: remote invalid argument"; got != want {
		t.Fatalf("remote error = %q, want %q", got, want)
	}
	_, err = service.Retrieve(context.Background(), webapi.RetrievalRequest{
		Query: retrieval.Request{Text: "bounded", TopK: 1},
	})
	if !shoal.IsErrorCode(err, shoal.ErrorDeadline) ||
		!errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("remote deadline error = %v", err)
	}
	_, err = service.Path(context.Background(), webapi.PathRequest{
		From: "from", To: "to", MaxDepth: 1, Fanout: 1,
	})
	if !shoal.IsErrorCode(err, shoal.ErrorDeadline) ||
		!errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("remote deadline fallback error = %v", err)
	}
	_, err = service.Neighborhood(context.Background(), webapi.NeighborhoodRequest{
		NodeIDs: []shoal.ID{"node"}, Depth: 1, Fanout: 1, MaxNodes: 1,
	})
	if !shoal.IsErrorCode(err, shoal.ErrorInternal) ||
		shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("contradictory error mapping = %v", err)
	}

	upstream.Close()
	_, err = service.Documents(context.Background(), webapi.DocumentsRequest{
		Page: webapi.PageRequest{Limit: 1},
	})
	if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("unavailable error = %v", err)
	}
}

func TestRemoteServiceRejectsNonconformingResponses(t *testing.T) {
	snapshot := webapi.Snapshot{
		ID: "snapshot", AsOf: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		Frontier: 7,
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/meta":
			writeJSON(t, writer, webapi.MetadataResponse{
				MaxPageSize: webapi.MaxPageSize, MaxTopK: webapi.MaxTopK,
				MaxDepth: webapi.MaxDepth, MaxFanout: webapi.MaxFanout,
				MaxNodes: webapi.MaxNodes, MaxUploadFiles: webapi.MaxUploadFiles,
				MaxUploadFileBytes:  webapi.MaxUploadFileBytes,
				MaxUploadTotalBytes: webapi.MaxUploadTotalBytes,
				Capabilities:        webapi.AllCapabilities(),
			})
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/documents":
			writeJSON(t, writer, webapi.DocumentsResponse{
				Snapshot: snapshot,
				Documents: []explorer.DocumentSummary{
					documentSummary("one", snapshot.AsOf),
					documentSummary("two", snapshot.AsOf),
				},
			})
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/document":
			var body webapi.DocumentRequest
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			view := documentView("one", snapshot.AsOf)
			if body.DocumentID == "blank-source" {
				view = documentView("blank-source", snapshot.AsOf)
				view.SourceURI = ""
			}
			if body.DocumentID == "invalid-media" {
				view = documentView("invalid-media", snapshot.AsOf)
				view.SourceMediaType = "application/x-hostile"
			}
			writeJSON(t, writer, webapi.DocumentResponse{
				Snapshot: snapshot,
				Document: view,
			})
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/neighborhood":
			writeJSON(t, writer, webapi.NeighborhoodResponse{
				Snapshot: snapshot,
				Neighborhood: explorer.Neighborhood{Nodes: []graph.Node{
					{ID: "one", Kind: "test"}, {ID: "two", Kind: "test"},
				}},
			})
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/path":
			var body struct {
				EdgeTypes []string `json:"edge_types"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			from := shoal.ID("other")
			if len(body.EdgeTypes) > 0 {
				from = "from"
			}
			if len(body.EdgeTypes) > 0 && body.EdgeTypes[0] == "duplicate" {
				writeJSON(t, writer, webapi.PathResponse{
					Snapshot: snapshot,
					Path: graph.Path{
						Nodes: []graph.Node{{ID: "from"}, {ID: "mid"}, {ID: "to"}},
						Edges: []graph.Edge{
							{ID: "edge", From: "from", To: "mid", Type: "duplicate", Weight: 1},
							{ID: "edge", From: "mid", To: "to", Type: "duplicate", Weight: 1},
						},
					},
				})
				return
			}
			writeJSON(t, writer, webapi.PathResponse{
				Snapshot: snapshot,
				Path: graph.Path{
					Nodes: []graph.Node{{ID: from}, {ID: "to"}},
					Edges: []graph.Edge{{
						ID: "edge", From: from, To: "to", Type: "test", Weight: 1,
					}},
				},
			})
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/ingest":
			summary := documentSummary("one", snapshot.AsOf)
			writeJSON(t, writer, webapi.IngestResponse{
				Snapshot: snapshot,
				Files: []webapi.IngestFileResult{{
					Name: "remote.go", MediaType: explorer.MediaTypeSource,
					Disposition: explorer.IngestApplied, Document: summary.Document,
					Revision: summary.Revision, SectionCount: 1, SpanCount: 1,
				}},
			})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer upstream.Close()
	service, err := webapi.NewRemoteService(upstream.URL, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}

	_, err = service.Documents(context.Background(), webapi.DocumentsRequest{
		Page: webapi.PageRequest{Limit: 1},
	})
	if !shoal.IsErrorCode(err, shoal.ErrorInternal) {
		t.Fatalf("oversized document page error = %v", err)
	}
	_, err = service.Documents(context.Background(), webapi.DocumentsRequest{
		Snapshot: webapi.Snapshot{
			ID: "other", AsOf: snapshot.AsOf, Frontier: snapshot.Frontier,
		},
		Page: webapi.PageRequest{Limit: 10},
	})
	if !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("mismatched snapshot error = %v", err)
	}
	_, err = service.Document(context.Background(), webapi.DocumentRequest{
		Snapshot: snapshot, DocumentID: "two",
	})
	if !shoal.IsErrorCode(err, shoal.ErrorInternal) {
		t.Fatalf("mismatched document error = %v", err)
	}
	_, err = service.Document(context.Background(), webapi.DocumentRequest{
		Snapshot: snapshot, DocumentID: "blank-source",
	})
	if !shoal.IsErrorCode(err, shoal.ErrorInternal) {
		t.Fatalf("blank document source URI error = %v", err)
	}
	_, err = service.Document(context.Background(), webapi.DocumentRequest{
		Snapshot: snapshot, DocumentID: "invalid-media",
	})
	if !shoal.IsErrorCode(err, shoal.ErrorInternal) {
		t.Fatalf("invalid document media type error = %v", err)
	}
	_, err = service.Neighborhood(context.Background(), webapi.NeighborhoodRequest{
		Snapshot: snapshot, NodeIDs: []shoal.ID{"one"}, Depth: 1,
		Fanout: 1, MaxNodes: 1,
	})
	if !shoal.IsErrorCode(err, shoal.ErrorInternal) {
		t.Fatalf("oversized neighborhood error = %v", err)
	}
	_, err = service.Neighborhood(context.Background(), webapi.NeighborhoodRequest{
		Snapshot: snapshot, NodeIDs: []shoal.ID{"one"}, Depth: 1,
		Fanout: 1, MaxNodes: 2,
	})
	if !shoal.IsErrorCode(err, shoal.ErrorInternal) {
		t.Fatalf("disconnected neighborhood error = %v", err)
	}
	_, err = service.Path(context.Background(), webapi.PathRequest{
		Snapshot: snapshot, From: "from", To: "to", MaxDepth: 1, Fanout: 1,
	})
	if !shoal.IsErrorCode(err, shoal.ErrorInternal) {
		t.Fatalf("mismatched path error = %v", err)
	}
	_, err = service.Path(context.Background(), webapi.PathRequest{
		Snapshot: snapshot, From: "from", To: "to", MaxDepth: 1, Fanout: 1,
		EdgeTypes: []string{"allowed"},
	})
	if !shoal.IsErrorCode(err, shoal.ErrorInternal) {
		t.Fatalf("excluded path edge type error = %v", err)
	}
	_, err = service.Path(context.Background(), webapi.PathRequest{
		Snapshot: snapshot, From: "from", To: "to", MaxDepth: 2, Fanout: 2,
		EdgeTypes: []string{"duplicate"},
	})
	if !shoal.IsErrorCode(err, shoal.ErrorInternal) {
		t.Fatalf("duplicate path edge error = %v", err)
	}
	_, err = service.Ingest(context.Background(), webapi.IngestRequest{
		Files: []webapi.UploadFile{{
			Name: "remote.go", Content: []byte("package remote\n"),
		}},
	})
	if !shoal.IsErrorCode(err, shoal.ErrorInternal) {
		t.Fatalf("mismatched ingest identity error = %v", err)
	}
}

func testService(
	t *testing.T,
) (*webapi.EmbeddedService, *explorer.Explorer, explorer.IngestResult, explorer.IngestResult) {
	t.Helper()
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	first, err := corpus.IngestWithOptions(ctx, explorer.Source{
		URI: "file:///guide.md", MediaType: explorer.MediaTypeMarkdown,
		Content: "# Guide\n\nUse bounded retries.\n\n## Details\n\nKeep exact evidence.",
	}, explorer.IngestOptions{CreatedAt: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		corpus.Close()
		t.Fatal(err)
	}
	second, err := corpus.IngestWithOptions(ctx, explorer.Source{
		URI: "file:///service.md", MediaType: explorer.MediaTypeMarkdown,
		Content: "# Service\n\nThe service consumes the guide.",
	}, explorer.IngestOptions{CreatedAt: time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		corpus.Close()
		t.Fatal(err)
	}
	if err := corpus.Connect(ctx, graph.Edge{
		ID: "guide-to-service", From: first.Document.ID, To: second.Document.ID,
		Type: "informs", Weight: 1,
	}); err != nil {
		corpus.Close()
		t.Fatal(err)
	}
	service, err := webapi.NewEmbeddedService(corpus)
	if err != nil {
		corpus.Close()
		t.Fatal(err)
	}
	return service, corpus, first, second
}

func testRemoteService(
	t *testing.T,
) (*webapi.RemoteService, func(), explorer.IngestResult, explorer.IngestResult) {
	t.Helper()
	embedded, corpus, first, second := testService(t)
	remote, serverCleanup := serveRemoteService(t, embedded)
	cleanup := func() {
		serverCleanup()
		corpus.Close()
	}
	return remote, cleanup, first, second
}

func serveRemoteService(
	t *testing.T,
	service webapi.Service,
) (*webapi.RemoteService, func()) {
	t.Helper()
	server := httptest.NewUnstartedServer(nil)
	handler, err := webapi.NewHandler(service, server.Listener.Addr().String())
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	server.Config.Handler = handler
	server.Start()
	remote, err := webapi.NewRemoteService(server.URL, server.Client())
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	return remote, server.Close
}

func writeJSON(t *testing.T, writer http.ResponseWriter, value any) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		t.Fatal(err)
	}
}

func assertEqualJSON(t *testing.T, name string, left, right any) {
	t.Helper()
	leftJSON, err := json.Marshal(left)
	if err != nil {
		t.Fatalf("%s left JSON: %v", name, err)
	}
	rightJSON, err := json.Marshal(right)
	if err != nil {
		t.Fatalf("%s right JSON: %v", name, err)
	}
	if !bytes.Equal(leftJSON, rightJSON) {
		t.Fatalf("%s mismatch:\nleft:  %s\nright: %s", name, leftJSON, rightJSON)
	}
}

func documentSummary(id string, createdAt time.Time) explorer.DocumentSummary {
	documentID := shoal.ID(id)
	revisionID := shoal.ID(id + "-revision")
	return explorer.DocumentSummary{
		Document: document.Document{
			ID: documentID, RevisionID: revisionID,
			Title: id, RootSectionID: shoal.ID(id + "-root"),
		},
		Revision: document.Revision{
			ID: revisionID, DocumentID: documentID, CreatedAt: createdAt,
		},
		SourceURI: "file:///" + id,
	}
}

func documentView(id string, createdAt time.Time) explorer.DocumentView {
	summary := documentSummary(id, createdAt)
	return explorer.DocumentView{
		Document:  summary.Document,
		Revision:  summary.Revision,
		SourceURI: summary.SourceURI,
		Root: explorer.SectionView{Section: document.Section{
			ID: summary.Document.RootSectionID, DocumentID: summary.Document.ID,
			RevisionID: summary.Revision.ID, Heading: id,
		}},
	}
}

func firstSpanID(t *testing.T, view explorer.SectionView) shoal.ID {
	t.Helper()
	if len(view.Spans) > 0 {
		return view.Spans[0].ID
	}
	for _, child := range view.Children {
		if id := firstSpanIDOptional(child); id != "" {
			return id
		}
	}
	t.Fatal("document has no span")
	return ""
}

func firstSpanIDOptional(view explorer.SectionView) shoal.ID {
	if len(view.Spans) > 0 {
		return view.Spans[0].ID
	}
	for _, child := range view.Children {
		if id := firstSpanIDOptional(child); id != "" {
			return id
		}
	}
	return ""
}

type uploadFixture struct {
	name      string
	content   string
	mediaType string
}

type responseBody struct {
	StatusCode int
	Status     string
	body       []byte
}

type failingIngestService struct {
	webapi.Service
	err error
}

type capabilityOnlyService struct {
	webapi.Service
}

func (s capabilityOnlyService) Capabilities(context.Context) (webapi.Capabilities, error) {
	return webapi.AllCapabilities(), nil
}

func (s failingIngestService) Capabilities(context.Context) (webapi.Capabilities, error) {
	return webapi.AllCapabilities(), nil
}

func (s failingIngestService) Ingest(
	context.Context, webapi.IngestRequest,
) (webapi.IngestResponse, error) {
	return webapi.IngestResponse{}, s.err
}

func postMultipart(t *testing.T, url string, uploads ...uploadFixture) responseBody {
	t.Helper()
	return postMultipartWithHeader(t, url, true, uploads...)
}

func postMultipartWithoutWorkspaceHeader(
	t *testing.T, url string, uploads ...uploadFixture,
) responseBody {
	t.Helper()
	return postMultipartWithHeader(t, url, false, uploads...)
}

func postMultipartWithHeader(
	t *testing.T, url string, workspaceHeader bool, uploads ...uploadFixture,
) responseBody {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, upload := range uploads {
		part, err := writer.CreateFormFile("files", upload.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write([]byte(upload.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, url, &body)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Accept", "application/json")
	if workspaceHeader {
		request.Header.Set("X-Shoal-Workspace-Request", "1")
	}
	client := http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return responseBody{StatusCode: response.StatusCode, Status: response.Status, body: data}
}

func postJSON(t *testing.T, url string, body string) []byte {
	t.Helper()
	client := http.Client{Timeout: 5 * time.Second}
	response, err := client.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("POST %s returned %s: %s", url, response.Status, data)
	}
	return data
}

func assertViewSpansMatchSource(t *testing.T, view explorer.SectionView, source string) {
	t.Helper()
	found := false
	var walk func(explorer.SectionView)
	walk = func(current explorer.SectionView) {
		for _, span := range current.Spans {
			found = true
			start := span.Range.Start.Offset
			end := span.Range.End.Offset
			if start < 0 || end < start || end > int64(len(source)) {
				t.Fatalf("span range outside source: %+v source length %d", span.Range, len(source))
			}
			if span.Text != source[start:end] {
				t.Fatalf("span text %q does not match source range %d-%d %q",
					span.Text, start, end, source[start:end])
			}
			if span.DocumentID == "" || span.RevisionID == "" || span.SectionID == "" {
				t.Fatalf("span lost citation identity: %+v", span)
			}
		}
		for _, child := range current.Children {
			walk(child)
		}
	}
	walk(view)
	if !found {
		t.Fatal("document view did not contain any exact spans")
	}
}

func TestHTTPExtractPublishesUploadedSkillGraph(t *testing.T) {
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	service, err := webapi.NewEmbeddedService(corpus)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.SetOntologyVersion(webapiSkillsOntologyVersion(t)); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(nil)
	handler, err := webapi.NewHandler(service, server.Listener.Addr().String())
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	server.Config.Handler = handler
	server.Start()
	defer server.Close()

	uploaded := postMultipart(t, server.URL+"/api/v1/ingest", uploadFixture{
		name: "SKILL.md", content: "# Demo Skill\n\nTools:\n- demo-cli\n\nCapabilities:\n- Graph extraction\n",
		mediaType: explorer.MediaTypeMarkdown,
	})
	if uploaded.StatusCode != http.StatusOK {
		t.Fatalf("upload status = %s: %s", uploaded.Status, uploaded.body)
	}
	var ingested webapi.IngestResponse
	if err := json.Unmarshal(uploaded.body, &ingested); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(webapi.ExtractRequest{
		Snapshot:   ingested.Snapshot,
		DocumentID: ingested.Files[0].Document.ID,
		RevisionID: ingested.Files[0].Revision.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	data := postJSON(t, server.URL+"/api/v1/extract", string(body))
	var extracted webapi.ExtractResponse
	if err := json.Unmarshal(data, &extracted); err != nil {
		t.Fatal(err)
	}
	if extracted.EntityCount != 3 || extracted.RelationCount != 2 {
		t.Fatalf("extract response counts = entities:%d relations:%d, want 3 and 2",
			extracted.EntityCount, extracted.RelationCount)
	}
	if extracted.GraphNodeCount != 3 || extracted.GraphEdgeCount != 5 {
		t.Fatalf("extract graph counts = nodes:%d edges:%d, want 3 and 5",
			extracted.GraphNodeCount, extracted.GraphEdgeCount)
	}
}

// TestHTTPExtractRoundTripsWireEncodedIdentifiers pins the exact bytes the
// browser exchanges with /api/v1/extract. The static client forwards the
// identifiers it received from /api/v1/ingest verbatim, so extract must accept
// the same wire encoding every other endpoint accepts and must answer with
// identifiers that can be fed straight back in as neighborhood seeds.
func TestHTTPExtractRoundTripsWireEncodedIdentifiers(t *testing.T) {
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	service, err := webapi.NewEmbeddedService(corpus)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.SetOntologyVersion(webapiSkillsOntologyVersion(t)); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(nil)
	handler, err := webapi.NewHandler(service, server.Listener.Addr().String())
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	server.Config.Handler = handler
	server.Start()
	defer server.Close()

	uploaded := postMultipart(t, server.URL+"/api/v1/ingest", uploadFixture{
		name: "SKILL.md", content: "# Demo Skill\n\nTools:\n- demo-cli\n\nCapabilities:\n- Graph extraction\n",
		mediaType: explorer.MediaTypeMarkdown,
	})
	if uploaded.StatusCode != http.StatusOK {
		t.Fatalf("upload status = %s: %s", uploaded.Status, uploaded.body)
	}
	var ingestWire struct {
		Snapshot json.RawMessage `json:"snapshot"`
		Files    []struct {
			Document struct {
				ID string `json:"id"`
			} `json:"document"`
			Revision struct {
				ID string `json:"id"`
			} `json:"revision"`
		} `json:"files"`
	}
	if err := json.Unmarshal(uploaded.body, &ingestWire); err != nil {
		t.Fatal(err)
	}
	if len(ingestWire.Files) != 1 {
		t.Fatalf("ingest files = %d, want 1", len(ingestWire.Files))
	}
	documentID := ingestWire.Files[0].Document.ID
	revisionID := ingestWire.Files[0].Revision.ID
	decodedDocumentID, err := base64.RawURLEncoding.DecodeString(documentID)
	if err != nil {
		t.Fatalf("ingest document id %q is not unpadded base64url: %v", documentID, err)
	}
	if string(decodedDocumentID) == documentID {
		t.Fatalf("ingest document id %q is not wire encoded", documentID)
	}

	body := fmt.Sprintf(`{"snapshot":%s,"document_id":%q,"revision_id":%q}`,
		ingestWire.Snapshot, documentID, revisionID)
	var extractWire struct {
		Snapshot            json.RawMessage `json:"snapshot"`
		DocumentID          string          `json:"document_id"`
		RevisionID          string          `json:"revision_id"`
		ExtractionID        string          `json:"extraction_id"`
		EntityCount         int             `json:"entity_count"`
		EntityNodeIDs       []string        `json:"entity_node_ids"`
		RelationCount       int             `json:"relation_count"`
		RelationshipEdgeIDs []string        `json:"relationship_edge_ids"`
	}
	if err := json.Unmarshal(postJSON(t, server.URL+"/api/v1/extract", body), &extractWire); err != nil {
		t.Fatal(err)
	}
	if extractWire.DocumentID != documentID {
		t.Fatalf("extract response document_id = %q, want the wire form %q",
			extractWire.DocumentID, documentID)
	}
	if extractWire.RevisionID != revisionID {
		t.Fatalf("extract response revision_id = %q, want the wire form %q",
			extractWire.RevisionID, revisionID)
	}
	if extractWire.EntityCount != 3 || len(extractWire.EntityNodeIDs) != 3 {
		t.Fatalf("extract response entities = %d, node ids = %d, want 3 and 3",
			extractWire.EntityCount, len(extractWire.EntityNodeIDs))
	}
	if extractWire.RelationCount != 2 || len(extractWire.RelationshipEdgeIDs) != 2 {
		t.Fatalf("extract response relations = %d, edge ids = %d, want 2 and 2",
			extractWire.RelationCount, len(extractWire.RelationshipEdgeIDs))
	}
	encoded := append([]string{extractWire.ExtractionID}, extractWire.EntityNodeIDs...)
	encoded = append(encoded, extractWire.RelationshipEdgeIDs...)
	for _, id := range encoded {
		decoded, err := base64.RawURLEncoding.DecodeString(id)
		if err != nil {
			t.Fatalf("extract response id %q is not unpadded base64url: %v", id, err)
		}
		if len(decoded) == 0 || string(decoded) == id {
			t.Fatalf("extract response id %q is not wire encoded", id)
		}
	}

	seeds, err := json.Marshal(extractWire.EntityNodeIDs)
	if err != nil {
		t.Fatal(err)
	}
	neighborhood := fmt.Sprintf(`{"snapshot":%s,"node_ids":%s,"depth":1}`,
		extractWire.Snapshot, seeds)
	var neighborhoodWire struct {
		Neighborhood struct {
			Nodes []struct {
				ID string `json:"id"`
			} `json:"nodes"`
		} `json:"neighborhood"`
	}
	if err := json.Unmarshal(
		postJSON(t, server.URL+"/api/v1/neighborhood", neighborhood), &neighborhoodWire,
	); err != nil {
		t.Fatal(err)
	}
	if len(neighborhoodWire.Neighborhood.Nodes) < len(extractWire.EntityNodeIDs) {
		t.Fatalf("neighborhood seeded by extract returned %d nodes, want at least %d",
			len(neighborhoodWire.Neighborhood.Nodes), len(extractWire.EntityNodeIDs))
	}
}

func webapiSkillsOntologyVersion(t *testing.T) ontology.OntologyVersion {
	t.Helper()
	name, err := ontology.NewPropertyDefinition(
		"name", "Name", "Human-readable name", ontology.ValueString, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	skill, err := ontology.NewConceptDefinition(
		"skill", "Skill", "Agent skill", []shoal.ID{name.ID()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tool, err := ontology.NewConceptDefinition(
		"tool", "Tool", "Usable tool", []shoal.ID{name.ID()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	capability, err := ontology.NewConceptDefinition(
		"capability", "Capability", "Provided capability", []shoal.ID{name.ID()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	providesTool, err := ontology.NewRelationshipDefinition(
		"provides_tool", "Provides tool", "Skill provides tool",
		[]shoal.ID{skill.ID()}, []shoal.ID{tool.ID()}, nil, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	providesCapability, err := ontology.NewRelationshipDefinition(
		"provides_capability", "Provides capability", "Skill provides capability",
		[]shoal.ID{skill.ID()}, []shoal.ID{capability.ID()}, nil, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	dependsOn, err := ontology.NewRelationshipDefinition(
		"depends_on", "Depends on", "Skill depends on skill",
		[]shoal.ID{skill.ID()}, []shoal.ID{skill.ID()}, nil, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	schema, err := ontology.NewOntologySchema(
		"agent-skills-web", "Agent Skills Web", "Web API skill ontology", nil)
	if err != nil {
		t.Fatal(err)
	}
	version, err := ontology.NewOntologyVersion(
		schema, "v1", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		[]ontology.ConceptDefinition{skill, tool, capability},
		[]ontology.RelationshipDefinition{providesTool, providesCapability, dependsOn},
		[]ontology.PropertyDefinition{name}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	return version
}
