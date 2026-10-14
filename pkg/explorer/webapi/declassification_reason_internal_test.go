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

// The declassification reason on the upload surfaces (#664).

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// capturingIngestService records what the HTTP route decoded and delegates
// to the real embedded service.
type capturingIngestService struct {
	*EmbeddedService
	mu       sync.Mutex
	requests []IngestRequest
}

func (s *capturingIngestService) Ingest(
	ctx context.Context, request IngestRequest,
) (IngestResponse, error) {
	s.mu.Lock()
	s.requests = append(s.requests, request)
	s.mu.Unlock()
	return s.EmbeddedService.Ingest(ctx, request)
}

func (s *capturingIngestService) captured() []IngestRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]IngestRequest(nil), s.requests...)
}

type reasonUploadWorld struct {
	corpus  *explorer.Explorer
	service *capturingIngestService
	server  *httptest.Server
}

func newReasonUploadWorld(t *testing.T) *reasonUploadWorld {
	t.Helper()
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = corpus.Close() })
	embedded, err := NewEmbeddedService(corpus)
	if err != nil {
		t.Fatal(err)
	}
	service := &capturingIngestService{EmbeddedService: embedded}
	server := httptest.NewUnstartedServer(nil)
	handler, err := NewHandler(service, server.Listener.Addr().String())
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	server.Config.Handler = handler
	server.Start()
	t.Cleanup(server.Close)
	return &reasonUploadWorld{corpus: corpus, service: service, server: server}
}

// reasonPart is one multipart part of a test upload.
type reasonPart struct {
	field, filename, content string
}

func (w *reasonUploadWorld) post(t *testing.T, parts ...reasonPart) (int, string) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, part := range parts {
		var target io.Writer
		var err error
		if part.filename != "" {
			target, err = writer.CreateFormFile(part.field, part.filename)
		} else {
			target, err = writer.CreateFormField(part.field)
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := target.Write([]byte(part.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(
		http.MethodPost, w.server.URL+"/api/v1/ingest", &body)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("X-Shoal-Workspace-Request", "1")
	response, err := w.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, string(raw)
}

func (w *reasonUploadWorld) documentCount(t *testing.T) int {
	t.Helper()
	documents, err := w.corpus.Documents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return len(documents)
}

var reasonFile = reasonPart{field: "files", filename: "notes.txt", content: "hello\n"}

func reasonField(value string) reasonPart {
	return reasonPart{field: DeclassificationReasonField, content: value}
}

// TestHTTPIngestDecodesDeclassificationReason pins that the browser upload
// route carries the reason field, in any position, and that a request
// without one carries none.
func TestHTTPIngestDecodesDeclassificationReason(t *testing.T) {
	world := newReasonUploadWorld(t)
	if status, body := world.post(t, reasonField("ticket:SEC-1"), reasonFile); status != http.StatusOK {
		t.Fatalf("upload with a leading reason = %d: %s", status, body)
	}
	other := reasonPart{field: "files", filename: "other.txt", content: "other\n"}
	if status, body := world.post(t, other, reasonField("a/b.c_d-e")); status != http.StatusOK {
		t.Fatalf("upload with a trailing reason = %d: %s", status, body)
	}
	// An empty field is no reason, as an empty browser input sends.
	third := reasonPart{field: "files", filename: "third.txt", content: "third\n"}
	if status, body := world.post(t, third, reasonField("")); status != http.StatusOK {
		t.Fatalf("upload with an empty reason = %d: %s", status, body)
	}
	fourth := reasonPart{field: "files", filename: "fourth.txt", content: "fourth\n"}
	if status, body := world.post(t, fourth); status != http.StatusOK {
		t.Fatalf("upload without a reason = %d: %s", status, body)
	}
	// Any other non-file field is still ignored.
	fifth := reasonPart{field: "files", filename: "fifth.txt", content: "fifth\n"}
	if status, body := world.post(t, reasonPart{field: "note", content: "x y"}, fifth); status != http.StatusOK {
		t.Fatalf("upload with an unrelated field = %d: %s", status, body)
	}
	captured := world.service.captured()
	want := []string{"ticket:SEC-1", "a/b.c_d-e", "", "", ""}
	if len(captured) != len(want) {
		t.Fatalf("captured %d requests, want %d", len(captured), len(want))
	}
	for index, request := range captured {
		if request.DeclassificationReason != want[index] || len(request.Files) != 1 {
			t.Fatalf("request %d = reason %q, %d file(s); want %q",
				index, request.DeclassificationReason, len(request.Files), want[index])
		}
	}
}

// TestHTTPIngestRefusesInvalidDeclassificationReason pins that an invalid
// reason is a 400 with a stable public message, and that nothing in the batch
// is ingested.
func TestHTTPIngestRefusesInvalidDeclassificationReason(t *testing.T) {
	world := newReasonUploadWorld(t)
	for _, test := range []struct {
		name    string
		parts   []reasonPart
		message string
	}{
		{"space", []reasonPart{reasonFile, reasonField("has space")},
			invalidDeclassificationReasonMessage},
		{"punctuation", []reasonPart{reasonFile, reasonField("semi;colon")},
			invalidDeclassificationReasonMessage},
		{"non-ASCII", []reasonPart{reasonFile, reasonField("café")},
			invalidDeclassificationReasonMessage},
		{"newline", []reasonPart{reasonFile, reasonField("a\nb")},
			invalidDeclassificationReasonMessage},
		{"one byte over the bound", []reasonPart{reasonFile, reasonField(
			strings.Repeat("a", authorized.MaxDeclassificationReasonBytes+1))},
			invalidDeclassificationReasonMessage},
		{"far over the bound", []reasonPart{reasonFile, reasonField(
			strings.Repeat("a", 1<<20))},
			invalidDeclassificationReasonMessage},
		{"repeated", []reasonPart{reasonField("a"), reasonFile, reasonField("b")},
			repeatedDeclassificationReasonMessage},
	} {
		t.Run(test.name, func(t *testing.T) {
			status, body := world.post(t, test.parts...)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d: %s", status, body)
			}
			var failure struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal([]byte(body), &failure); err != nil {
				t.Fatalf("body %q: %v", body, err)
			}
			if failure.Code != string(shoal.ErrorInvalidArgument) ||
				!strings.HasSuffix(failure.Message, test.message) {
				t.Fatalf("failure = %+v, want %q", failure, test.message)
			}
			if count := world.documentCount(t); count != 0 {
				t.Fatalf("a refused batch ingested %d document(s)", count)
			}
		})
	}
	// The longest valid reason is accepted.
	if status, body := world.post(t, reasonFile, reasonField(
		strings.Repeat("a", authorized.MaxDeclassificationReasonBytes))); status != http.StatusOK {
		t.Fatalf("the longest reason = %d: %s", status, body)
	}
}

// TestHTTPIngestReasonFieldWithAFilenameIsAFile pins that a file part keeps
// file semantics whatever its field name, so a file can never be read as the
// reason and its content dropped.
func TestHTTPIngestReasonFieldWithAFilenameIsAFile(t *testing.T) {
	world := newReasonUploadWorld(t)
	status, body := world.post(t, reasonPart{
		field: DeclassificationReasonField, filename: "named.txt",
		content: "file content\n",
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	captured := world.service.captured()
	if len(captured) != 1 || captured[0].DeclassificationReason != "" ||
		len(captured[0].Files) != 1 || captured[0].Files[0].Name != "named.txt" {
		t.Fatalf("captured = %+v", captured)
	}
}

// TestEmbeddedIngestRefusesInvalidReasonBeforeIngesting pins the service
// check that every surface reaches (HTTP, MCP and remote all end here).
func TestEmbeddedIngestRefusesInvalidReasonBeforeIngesting(t *testing.T) {
	world := newReasonUploadWorld(t)
	_, err := world.service.EmbeddedService.Ingest(context.Background(), IngestRequest{
		Files:                  []UploadFile{{Name: "notes.txt", Content: []byte("hello\n")}},
		DeclassificationReason: "not valid",
	})
	if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) ||
		!strings.Contains(err.Error(), invalidDeclassificationReasonMessage) {
		t.Fatalf("invalid reason = %v", err)
	}
	if count := world.documentCount(t); count != 0 {
		t.Fatalf("a refused request ingested %d document(s)", count)
	}
}

// TestRemoteIngestForwardsDeclassificationReason sends a remote ingest
// through the real upload route, so the remote encoding and the route's
// decoding are checked against each other.
func TestRemoteIngestForwardsDeclassificationReason(t *testing.T) {
	world := newReasonUploadWorld(t)
	remote, err := NewRemoteService(world.server.URL, world.server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remote.Ingest(context.Background(), IngestRequest{
		Files:                  []UploadFile{{Name: "notes.txt", Content: []byte("hello\n")}},
		DeclassificationReason: "ticket:SEC-2",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := remote.Ingest(context.Background(), IngestRequest{
		Files: []UploadFile{{Name: "other.txt", Content: []byte("other\n")}},
	}); err != nil {
		t.Fatal(err)
	}
	captured := world.service.captured()
	if len(captured) != 2 || captured[0].DeclassificationReason != "ticket:SEC-2" ||
		captured[1].DeclassificationReason != "" {
		t.Fatalf("captured = %+v", captured)
	}
	// An invalid reason is refused locally, before anything is sent, with
	// the embedded error.
	_, err = remote.Ingest(context.Background(), IngestRequest{
		Files:                  []UploadFile{{Name: "third.txt", Content: []byte("third\n")}},
		DeclassificationReason: "not valid",
	})
	if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) ||
		explorer.IsIndeterminateCommit(err) ||
		!strings.Contains(err.Error(), invalidDeclassificationReasonMessage) {
		t.Fatalf("invalid remote reason = %v", err)
	}
	if len(world.service.captured()) != 2 {
		t.Fatal("an invalid reason was sent upstream")
	}
}
