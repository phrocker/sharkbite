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
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
)

type routeOutcome struct {
	status        int
	header        string
	Code          string                     `json:"code"`
	Message       string                     `json:"message"`
	Indeterminate bool                       `json:"indeterminate"`
	Files         []webapi.IngestFileOutcome `json:"files"`
	// applied is a successful response's per-file disposition, by name.
	applied map[string]string
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
		return outcome
	}
	var success webapi.IngestResponse
	if err := json.Unmarshal(payload, &success); err != nil {
		t.Fatalf("body %q: %v", payload, err)
	}
	outcome.applied = make(map[string]string, len(success.Files))
	for _, file := range success.Files {
		outcome.applied[file.Name] = string(file.Disposition)
	}
	return outcome
}

// startUploadWorkspace runs the shipped binary over a workspace seeded with
// a labelled declassifiedName, requiring a declassification reason, and
// returns its address and a stop function.
func startUploadWorkspace(t *testing.T) (string, func()) {
	t.Helper()
	root := t.TempDir()
	seeded, _ := seedLabelledUpload(t, root, false)
	seeded.close()

	ctx, cancel := context.WithCancel(context.Background())
	reader, writer := io.Pipe()
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
	return address, func() {
		t.Helper()
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("server exit = %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Error("the server did not shut down")
		}
		_ = writer.Close()
	}
}

func historicalOutcome(name string) webapi.IngestFileOutcome {
	return webapi.IngestFileOutcome{
		Name: name, Status: webapi.IngestFileRefused,
		Reason: webapi.IngestRefusedHistoricalRevision,
		Message: `upload file "` + name + `" matches an earlier revision of its ` +
			`document, so the document was left unchanged; change the content to upload it`,
	}
}

func reasonRequiredOutcome(name string) webapi.IngestFileOutcome {
	return webapi.IngestFileOutcome{
		Name: name, Status: webapi.IngestFileRefused,
		Reason: webapi.IngestRefusedDeclassificationReason,
		Message: "this upload removes visibility labels from a document and " +
			"the workspace requires a declassification_reason",
	}
}

func statusOutcome(name string, status webapi.IngestFileStatus) webapi.IngestFileOutcome {
	return webapi.IngestFileOutcome{Name: name, Status: status}
}

func requireRouteFiles(t *testing.T, label string, got routeOutcome, want ...webapi.IngestFileOutcome) {
	t.Helper()
	if !reflect.DeepEqual(got.Files, want) {
		t.Fatalf("%s files =\n  %+v\nwant\n  %+v", label, got.Files, want)
	}
}

// TestUploadRouteReportsEachFileOfABranchSwitch is #687's scenario on the
// shipped binary with the real authorized client: a directory uploaded on
// branch A, then B, then A again. The third upload tries every file; each
// file whose content went back to A is refused as historical by name, the
// file identical on both branches is unchanged, and none of it blocks the
// rest. Every outcome is known, so the batch is a definite, partial 409, and
// retrying it three times returns the same answer (it converges). Nothing
// was restored: each refused document still serves B.
//
// A mixed batch (applied, historical, reason-required, applied) goes on past
// both refusals and applies its last file, and the refused narrowing left no
// pending claim behind: the same file with a reason then goes through. A
// batch where every file is refused stays a definite non-2xx, so a client
// that reads only the status never mistakes any of these for success.
func TestUploadRouteReportsEachFileOfABranchSwitch(t *testing.T) {
	address, stop := startUploadWorkspace(t)
	defer stop()

	branchA := [][2]string{
		{"docs__a.md", "# A\n\nbranch a\n"},
		{"docs__b.md", "# B\n\nbranch a\n"},
		{"docs__same.md", "# Same\n\nboth branches\n"},
	}
	branchB := [][2]string{
		{"docs__a.md", "# A\n\nbranch b\n"},
		{"docs__b.md", "# B\n\nbranch b\n"},
		{"docs__same.md", "# Same\n\nboth branches\n"},
	}
	for _, branch := range [][][2]string{branchA, branchB} {
		if got := uploadFiles(t, address, branch...); got.status != http.StatusOK {
			t.Fatalf("branch upload = %+v", got)
		}
	}
	// Every outcome is known, so the batch is the first refusal's definite
	// 409, partial because the shared file is unchanged. A client that
	// retries gets the same answer each time: the batch converges.
	var first routeOutcome
	for attempt := 1; attempt <= 3; attempt++ {
		got := uploadFiles(t, address, branchA...)
		if got.status != http.StatusConflict || got.Code != "conflict" ||
			got.header != webapi.CommitOutcomePartial || got.Indeterminate ||
			got.Message != "conflict: "+historicalOutcome("docs__a.md").Message {
			t.Fatalf("branch A again, attempt %d = %+v", attempt, got)
		}
		requireRouteFiles(t, "branch A again", got,
			historicalOutcome("docs__a.md"), historicalOutcome("docs__b.md"),
			statusOutcome("docs__same.md", webapi.IngestFileUnchanged))
		if attempt == 1 {
			first = got
		} else if !reflect.DeepEqual(got, first) {
			t.Fatalf("retry %d = %+v, want the first answer %+v", attempt, got, first)
		}
	}
	// Each refused document still serves B, and its claim is not pending:
	// B's content is unchanged, not historical or refused.
	if again := uploadFiles(t, address, branchB...); again.status != http.StatusOK ||
		again.applied["docs__a.md"] != "unchanged" || again.applied["docs__b.md"] != "unchanged" {
		t.Fatalf("branch B after the refusals = %+v", again)
	}

	const public = "# Secret\n\nnow public\n"
	got := uploadFiles(t, address,
		[2]string{"first.md", "# First\n"}, branchA[0],
		[2]string{declassifiedName, public}, [2]string{"last.md", "# Last\n"})
	if got.status != http.StatusConflict ||
		got.header != webapi.CommitOutcomePartial || got.Indeterminate ||
		got.Message != "conflict: "+historicalOutcome("docs__a.md").Message {
		t.Fatalf("mixed batch = %+v", got)
	}
	requireRouteFiles(t, "mixed batch", got,
		statusOutcome("first.md", webapi.IngestFileApplied),
		historicalOutcome("docs__a.md"),
		reasonRequiredOutcome(declassifiedName),
		statusOutcome("last.md", webapi.IngestFileApplied))
	if again := uploadFiles(t, address, [2]string{"last.md", "# Last\n"}); again.status != http.StatusOK ||
		again.applied["last.md"] != "unchanged" {
		t.Fatalf("the file after the refusals was not applied: %+v", again)
	}

	got = uploadFiles(t, address, branchA[1], [2]string{declassifiedName, public})
	if got.status != http.StatusConflict || got.Code != "conflict" ||
		got.header != "" || got.Indeterminate ||
		got.Message != "conflict: "+historicalOutcome("docs__b.md").Message {
		t.Fatalf("all-refused batch = %+v", got)
	}
	requireRouteFiles(t, "all-refused batch", got,
		historicalOutcome("docs__b.md"), reasonRequiredOutcome(declassifiedName))

	if status, body := uploadWithReason(t, address, declassifiedName, public,
		"legal-hold-release"); status != http.StatusOK {
		t.Fatalf("narrowing with a reason after the refusals = %d %s", status, body)
	}
}

// TestUploadRouteRefusesHistoricalContentByName drives the shipped binary
// with the real authorized client. It shows that:
//   - a single file whose content matches an earlier revision is a definite
//     409 naming that file;
//   - a refusal after an earlier file of the batch went through, the
//     historical one and the missing-reason 400 alike, keeps its definite
//     status and actionable message and reports the partial commit outcome
//     (#687; #684 reported it indeterminate);
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
	if got.status != http.StatusConflict ||
		got.header != webapi.CommitOutcomePartial || got.Indeterminate ||
		got.Message != "conflict: "+named {
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
	if got.status != http.StatusBadRequest ||
		got.header != webapi.CommitOutcomePartial || got.Indeterminate ||
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
