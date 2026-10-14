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

// A failure of the real authorized client after its base applied a write is
// indeterminate, so the upload route reports it as such, alone or after an
// earlier file of the batch, instead of as a definite failure (#687).

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
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const poisonContent = "poison content\n"

// postWriteFaults arms one failure for an ingest whose content starts with
// poisonContent, after the
// base has applied it: the next Documents read, or the next registration.
type postWriteFaults struct {
	mu        sync.Mutex
	documents bool
	register  bool
	armed     bool
}

func (f *postWriteFaults) arm() {
	f.mu.Lock()
	f.armed = true
	f.mu.Unlock()
}

func (f *postWriteFaults) take(kind bool) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.armed || !kind {
		return false
	}
	f.armed = false
	return true
}

type postWriteBase struct {
	*explorer.Explorer
	faults *postWriteFaults
}

func (b *postWriteBase) Ingest(
	ctx context.Context, source explorer.Source,
) (explorer.IngestResult, error) {
	result, err := b.Explorer.Ingest(ctx, source)
	if err == nil && strings.HasPrefix(source.Content, poisonContent) {
		b.faults.arm()
	}
	return result, err
}

func (b *postWriteBase) Documents(ctx context.Context) ([]explorer.DocumentSummary, error) {
	if b.faults.take(b.faults.documents) {
		return nil, shoal.NewError(shoal.ErrorUnavailable, "injected read failure")
	}
	return b.Explorer.Documents(ctx)
}

type postWriteStore struct {
	authorized.PolicyStore
	faults *postWriteFaults
}

func (s *postWriteStore) PutRevision(
	ctx context.Context, registration authorized.RevisionRegistration,
) error {
	if s.faults.take(s.faults.register) {
		return shoal.NewError(shoal.ErrorUnavailable, "injected registration failure")
	}
	return s.PolicyStore.PutRevision(ctx, registration)
}

func newPostWriteServer(t *testing.T, faults *postWriteFaults) *httptest.Server {
	t.Helper()
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = corpus.Close() })
	authority := auth.NewAuthority()
	selector, err := authorized.NewStaticPolicySelector(
		authnSourceGranted, authnPolicyGranted)
	if err != nil {
		t.Fatal(err)
	}
	client, err := authorized.NewClient(authorized.Config{
		Base:             &postWriteBase{Explorer: corpus, faults: faults},
		Resolver:         authority.Resolver(),
		PolicySelector:   selector,
		PolicyStore:      &postWriteStore{PolicyStore: authorized.NewMemoryPolicyStore(), faults: faults},
		GenerationReader: authnGenerationReader{},
		Clock:            time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := webapi.NewEmbeddedService(client)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(nil)
	handler, err := webapi.NewAuthenticatedHandler(service,
		webapi.AuthenticatorFunc(authnAuthenticate), authority.Binder(),
		server.Listener.Addr().String())
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	server.Config.Handler = handler
	server.Start()
	t.Cleanup(server.Close)
	return server
}

type postWriteOutcome struct {
	status        int
	header        string
	Code          string                     `json:"code"`
	Indeterminate bool                       `json:"indeterminate"`
	Files         []webapi.IngestFileOutcome `json:"files"`
}

func postWriteUpload(t *testing.T, server *httptest.Server, files ...[2]string) postWriteOutcome {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, file := range files {
		part, err := writer.CreateFormFile("files", file[0])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write([]byte(file[1])); err != nil {
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
	request.Header.Set(authnPrincipalHeader, "granted")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	outcome := postWriteOutcome{
		status: response.StatusCode, header: response.Header.Get(webapi.CommitOutcomeHeader),
	}
	if response.StatusCode != http.StatusOK {
		if err := json.Unmarshal(raw, &outcome); err != nil {
			t.Fatalf("body %q: %v", raw, err)
		}
	}
	return outcome
}

// TestUploadReportsAPostWriteFailureAsIndeterminate drives the real
// authorized client with a base that applies the poisoned file and then
// fails the read after it, or a catalog that fails its registration. Either
// way the file may be applied with its claim pending, so a lone upload and a
// batch stopped by it are both indeterminate, never a definite failure.
func TestUploadReportsAPostWriteFailureAsIndeterminate(t *testing.T) {
	for _, test := range []struct {
		name                string
		documents, register bool
	}{
		{"read after the write", true, false},
		{"registration", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			faults := &postWriteFaults{documents: test.documents, register: test.register}
			server := newPostWriteServer(t, faults)
			got := postWriteUpload(t, server, [2]string{"poison.txt", poisonContent})
			if got.status != http.StatusServiceUnavailable ||
				got.header != webapi.CommitOutcomeIndeterminate || !got.Indeterminate ||
				len(got.Files) != 1 || got.Files[0].Status != webapi.IngestFileFailed ||
				!got.Files[0].Indeterminate {
				t.Fatalf("lone post-write failure = %+v", got)
			}
			got = postWriteUpload(t, server,
				[2]string{"fresh.txt", "fresh content\n"},
				[2]string{"poison-b.txt", poisonContent + "b\n"},
				[2]string{"later.txt", "later content\n"})
			if got.status != http.StatusServiceUnavailable ||
				got.header != webapi.CommitOutcomeIndeterminate || !got.Indeterminate ||
				len(got.Files) != 3 ||
				got.Files[0].Status != webapi.IngestFileApplied ||
				got.Files[1].Status != webapi.IngestFileFailed || !got.Files[1].Indeterminate ||
				got.Files[2].Status != webapi.IngestFileNotAttempted {
				t.Fatalf("mid-batch post-write failure = %+v", got)
			}
			// Unarmed, the same client applies a batch normally.
			if got := postWriteUpload(t, server,
				[2]string{"other.txt", "other content\n"}); got.status != http.StatusOK {
				t.Fatalf("an unarmed upload failed: %+v", got)
			}
		})
	}
}
