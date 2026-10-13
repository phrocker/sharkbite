// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

// The shipped upload route's refusal of content that matches an earlier
// revision (#674), and the commit outcome of a batch that fails after an
// earlier file went through.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
)

type routeOutcome struct {
	status        int
	header        string
	Code          string `json:"code"`
	Message       string `json:"message"`
	Indeterminate bool   `json:"indeterminate"`
}

// uploadFiles posts files, as name/content pairs, to the running workspace's
// upload route.
func uploadFiles(t *testing.T, address string, files ...[2]string) routeOutcome {
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
	request, err := http.NewRequest(
		http.MethodPost, "http://"+address+"/api/v1/ingest", &body)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("X-Shoal-Workspace-Request", "1")
	client := http.Client{Timeout: 30 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	outcome := routeOutcome{
		status: response.StatusCode,
		header: response.Header.Get(webapi.CommitOutcomeHeader),
	}
	if response.StatusCode != http.StatusOK {
		if err := json.Unmarshal(payload, &outcome); err != nil {
			t.Fatalf("body %q: %v", payload, err)
		}
	}
	return outcome
}

// TestUploadRouteRefusesHistoricalContentByName drives the shipped binary
// with the real authorized client. It shows that:
//   - a single file whose content matches an earlier revision is a definite
//     409 naming that file;
//   - a refusal after an earlier file of the batch went through, the
//     historical one and the missing-reason 400 alike, reports the
//     indeterminate outcome and keeps the actionable message;
//   - the missing-reason refusal of a lone file stays a definite 400.
func TestUploadRouteRefusesHistoricalContentByName(t *testing.T) {
	root := t.TempDir()
	seeded, _ := seedLabelledUpload(t, root, false)
	seeded.close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader, writer := io.Pipe()
	defer writer.Close()
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{
			"-listen", "127.0.0.1:0", "-dev-auth",
			"-dev-auth-labels", secretDevelopmentLabel,
			"-state-dir", root, "-require-declassification-reason",
		}, writer)
	}()
	address := awaitListenAddress(t, reader)
	go func() { _, _ = io.Copy(io.Discard, reader) }()

	const branchA, branchB = "# Notes\n\nbranch a\n", "# Notes\n\nbranch b\n"
	for _, content := range []string{branchA, branchB} {
		if got := uploadFiles(t, address, [2]string{"notes.md", content}); got.status != http.StatusOK {
			t.Fatalf("upload = %+v", got)
		}
	}
	const named = `upload file "notes.md" matches an earlier revision of its ` +
		`document, so the document was left unchanged; change the content to upload it`
	got := uploadFiles(t, address, [2]string{"notes.md", branchA})
	if got.status != http.StatusConflict || got.Code != "conflict" ||
		got.Message != "conflict: "+named || got.header != "" || got.Indeterminate {
		t.Fatalf("single-file historical upload = %+v", got)
	}
	for _, leak := range []string{"secret", "rev_", "doc_", "policy"} {
		if strings.Contains(got.Message, leak) {
			t.Fatalf("historical refusal leaks %q: %q", leak, got.Message)
		}
	}

	got = uploadFiles(t, address,
		[2]string{"fresh.md", "# Fresh\n"}, [2]string{"notes.md", branchA})
	if got.status != http.StatusServiceUnavailable ||
		got.header != webapi.CommitOutcomeIndeterminate || !got.Indeterminate ||
		!strings.HasSuffix(got.Message, named) {
		t.Fatalf("mid-batch historical upload = %+v", got)
	}

	const public = "# Secret\n\nnow public\n"
	got = uploadFiles(t, address, [2]string{declassifiedName, public})
	if got.status != http.StatusBadRequest || got.header != "" || got.Indeterminate ||
		!strings.Contains(got.Message, "declassification_reason") {
		t.Fatalf("single-file reasonless narrowing = %+v", got)
	}
	got = uploadFiles(t, address,
		[2]string{"another.md", "# Another\n"}, [2]string{declassifiedName, public})
	if got.status != http.StatusServiceUnavailable ||
		got.header != webapi.CommitOutcomeIndeterminate || !got.Indeterminate ||
		!strings.Contains(got.Message, "declassification_reason") {
		t.Fatalf("mid-batch reasonless narrowing = %+v", got)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("server exit = %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the server did not shut down")
	}
}
