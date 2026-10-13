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

// The browser upload route's public form of a historical refusal (#674), and
// the commit outcome of a batch that fails after an earlier file went
// through.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// refusingClient is the real Explorer, except that an ingest of refused
// content fails with err, standing in for the authorized client's refusal.
type refusingClient struct {
	*explorer.Explorer
	refused string
	err     error
}

func (c *refusingClient) Ingest(
	ctx context.Context, source explorer.Source,
) (explorer.IngestResult, error) {
	if source.Content == c.refused {
		return explorer.IngestResult{}, c.err
	}
	return c.Explorer.Ingest(ctx, source)
}

func historicalRefusal() error {
	return shoal.WrapError(shoal.ErrorConflict,
		"the content matches a non-current revision of the document; "+
			"the document was not changed",
		authorized.ErrHistoricalRevision)
}

type uploadOutcome struct {
	status  int
	header  string
	code    string
	message string
	marked  bool
}

func newRefusingUploadServer(t *testing.T, refused string, err error) (*httptest.Server, *explorer.Explorer) {
	t.Helper()
	corpus, openErr := explorer.Open(t.TempDir())
	if openErr != nil {
		t.Fatal(openErr)
	}
	t.Cleanup(func() { _ = corpus.Close() })
	embedded, serviceErr := NewEmbeddedService(
		&refusingClient{Explorer: corpus, refused: refused, err: err})
	if serviceErr != nil {
		t.Fatal(serviceErr)
	}
	server := httptest.NewUnstartedServer(nil)
	handler, handlerErr := NewHandler(embedded, server.Listener.Addr().String())
	if handlerErr != nil {
		server.Close()
		t.Fatal(handlerErr)
	}
	server.Config.Handler = handler
	server.Start()
	t.Cleanup(server.Close)
	return server, corpus
}

func postUpload(t *testing.T, server *httptest.Server, parts ...reasonPart) uploadOutcome {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, part := range parts {
		target, err := writer.CreateFormFile(part.field, part.filename)
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
	request, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/ingest", &body)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("X-Shoal-Workspace-Request", "1")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Code          string `json:"code"`
		Message       string `json:"message"`
		Indeterminate bool   `json:"indeterminate"`
	}
	if response.StatusCode != http.StatusOK {
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("body %q: %v", raw, err)
		}
	}
	return uploadOutcome{
		status: response.StatusCode, header: response.Header.Get(CommitOutcomeHeader),
		code: payload.Code, message: payload.Message, marked: payload.Indeterminate,
	}
}

func filePart(name, content string) reasonPart {
	return reasonPart{field: "files", filename: name, content: content}
}

const historicalPublicMessage = `upload file "old.txt" matches an earlier ` +
	`revision of its document, so the document was left unchanged; change ` +
	`the content to upload it`

// TestHTTPIngestNamesTheHistoricalFile pins the public form of the refusal
// on a single-file batch: a definite 409 that names the caller's file and
// says what to do, carrying no rule, label or internal identity.
func TestHTTPIngestNamesTheHistoricalFile(t *testing.T) {
	server, corpus := newRefusingUploadServer(t, "old content\n", historicalRefusal())
	got := postUpload(t, server, filePart("old.txt", "old content\n"))
	if got.status != http.StatusConflict || got.code != string(shoal.ErrorConflict) ||
		got.message != "conflict: "+historicalPublicMessage ||
		got.header != "" || got.marked {
		t.Fatalf("single-file historical refusal = %+v", got)
	}
	if documents, err := corpus.Documents(context.Background()); err != nil || len(documents) != 0 {
		t.Fatalf("a refused upload ingested %+v (%v)", documents, err)
	}
}

// TestHTTPIngestNamesOnlyAConflictRefusal pins that the named message is
// keyed on the conflict refusal, not on the sentinel alone: the sentinel
// wrapped under another code (here an internal fault) keeps that code and
// the generic public message, so no file is named for an error that is not
// the refusal.
func TestHTTPIngestNamesOnlyAConflictRefusal(t *testing.T) {
	server, _ := newRefusingUploadServer(t, "old content\n", shoal.WrapError(
		shoal.ErrorInternal, "injected", authorized.ErrHistoricalRevision))
	got := postUpload(t, server, filePart("old.txt", "old content\n"))
	if got.status != http.StatusInternalServerError ||
		got.code != string(shoal.ErrorInternal) ||
		got.message != "internal: upload failed" {
		t.Fatalf("non-conflict sentinel = %+v", got)
	}
}

// TestHTTPIngestMarksAPartlyAppliedBatchIndeterminate pins that a refusal
// after an earlier file of the batch went through reports the indeterminate
// outcome (so a caller does not believe nothing happened), still naming the
// file, while the earlier file stays applied. The same holds for any
// per-file failure; another conflict stands in for the rest.
func TestHTTPIngestMarksAPartlyAppliedBatchIndeterminate(t *testing.T) {
	for _, test := range []struct {
		name    string
		err     error
		message string
	}{
		{"historical", historicalRefusal(), historicalPublicMessage},
		{"other conflict", shoal.NewError(shoal.ErrorConflict, "injected"), "upload failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, corpus := newRefusingUploadServer(t, "old content\n", test.err)
			got := postUpload(t, server,
				filePart("fresh.txt", "fresh content\n"),
				filePart("old.txt", "old content\n"))
			if got.status != http.StatusServiceUnavailable ||
				got.header != CommitOutcomeIndeterminate || !got.marked ||
				!strings.HasSuffix(got.message, test.message) {
				t.Fatalf("mid-batch refusal = %+v", got)
			}
			documents, err := corpus.Documents(context.Background())
			if err != nil || len(documents) != 1 ||
				documents[0].SourceURI != "upload://workspace/fresh.txt" {
				t.Fatalf("the earlier file was not applied: %+v (%v)", documents, err)
			}
			// The same refusal on the first file of a batch is definite.
			first := postUpload(t, server,
				filePart("old.txt", "old content\n"),
				filePart("later.txt", "later content\n"))
			if first.header != "" || first.marked ||
				first.status != http.StatusConflict ||
				!strings.HasSuffix(first.message, test.message) {
				t.Fatalf("first-file refusal = %+v", first)
			}
		})
	}
}

// TestEmbeddedIngestMarksOnlyAPartlyAppliedBatch pins the same rule below
// HTTP, for the MCP tool and the remote route alike, and that the error
// still identifies the historical refusal.
func TestEmbeddedIngestMarksOnlyAPartlyAppliedBatch(t *testing.T) {
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	embedded, err := NewEmbeddedService(
		&refusingClient{Explorer: corpus, refused: "old\n", err: historicalRefusal()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = embedded.Ingest(context.Background(), IngestRequest{Files: []UploadFile{
		{Name: "old.txt", Content: []byte("old\n")},
	}})
	if !errors.Is(err, authorized.ErrHistoricalRevision) ||
		!shoal.IsErrorCode(err, shoal.ErrorConflict) ||
		explorer.IsIndeterminateCommit(err) {
		t.Fatalf("first-file refusal = %v", err)
	}
	_, err = embedded.Ingest(context.Background(), IngestRequest{Files: []UploadFile{
		{Name: "new.txt", Content: []byte("new\n")},
		{Name: "old.txt", Content: []byte("old\n")},
	}})
	if !errors.Is(err, authorized.ErrHistoricalRevision) ||
		!shoal.IsErrorCode(err, shoal.ErrorConflict) ||
		!explorer.IsIndeterminateCommit(err) {
		t.Fatalf("mid-batch refusal = %v", err)
	}
}
