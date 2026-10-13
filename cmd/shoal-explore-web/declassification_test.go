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

// The declassification reason on the shipped upload route, the
// -require-declassification-reason flag, and the offline
// -list-declassifications listing (#664).

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	declassifiedName = "secret.md"
	declassifiedURI  = "upload://workspace/" + declassifiedName
)

var secretDevelopmentLabel = string(workspaceSourceID) + "=secret"

// openDeclassificationWorkspace opens the workspace state under root and
// returns a context bound, by the authority the service resolves with, to
// the development principal holding the "secret" label.
func openDeclassificationWorkspace(
	t *testing.T, root string, require bool,
) (openedService, context.Context) {
	t.Helper()
	authority := auth.NewAuthority()
	opened, err := openService(context.Background(), serviceConfig{
		backend:   "embedded",
		data:      filepath.Join(root, "corpus"),
		policyDir: filepath.Join(root, "policy"),
		resolver:  authority.Resolver(),
		clock:     time.Now,

		requireDeclassificationReason: require,
	})
	if err != nil {
		t.Fatal(err)
	}
	development, err := newDevelopmentAuthenticatorWithLabels(
		time.Now, secretDevelopmentLabel)
	if err != nil {
		opened.close()
		t.Fatal(err)
	}
	decision, err := development.Authenticate(
		httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/v1/ingest", nil))
	if err != nil {
		opened.close()
		t.Fatal(err)
	}
	ctx, err := authority.Binder().Bind(context.Background(), decision)
	if err != nil {
		opened.close()
		t.Fatal(err)
	}
	return opened, ctx
}

// seedLabelledUpload ingests declassifiedURI labelled "secret" into the
// workspace state under root and leaves it open. No shipped upload route
// carries a label, so this is the one step that goes around the product
// surface; the narrowing that follows does not.
func seedLabelledUpload(t *testing.T, root string, require bool) (openedService, context.Context) {
	t.Helper()
	opened, ctx := openDeclassificationWorkspace(t, root, require)
	if _, err := opened.client.Ingest(ctx, explorer.Source{
		URI: declassifiedURI, Title: declassifiedName,
		MediaType: explorer.MediaTypeMarkdown,
		Content:   "# Secret\n\nlabelled body\n",
		Metadata:  shoal.Metadata{interaction.PropertyVisibility: "secret"},
	}); err != nil {
		opened.close()
		t.Fatal(err)
	}
	return opened, ctx
}

// uploadThroughService narrows declassifiedURI to no label through the
// shipped upload service (the browser route's and MCP tool's), with reason.
func uploadThroughService(
	ctx context.Context, opened openedService, reason string,
) error {
	provider, ok := opened.service.(webapi.IngestProvider)
	if !ok {
		return shoal.NewError(shoal.ErrorUnavailable, "no ingest provider")
	}
	_, err := provider.Ingest(ctx, webapi.IngestRequest{
		Files: []webapi.UploadFile{{
			Name: declassifiedName, Content: []byte("# Secret\n\npublic\n"),
		}},
		DeclassificationReason: reason,
	})
	return err
}

// uploadWithReason posts one file to the running workspace's upload route,
// with the reason field when reason is not empty.
func uploadWithReason(
	t *testing.T, address, name, content, reason string,
) (int, string) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if reason != "" {
		if err := writer.WriteField("declassification_reason", reason); err != nil {
			t.Fatal(err)
		}
	}
	part, err := writer.CreateFormFile("files", name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte(content)); err != nil {
		t.Fatal(err)
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
	return response.StatusCode, string(payload)
}

// TestRequireDeclassificationReasonOverTheUploadRoute is the end-to-end proof
// of #664's first part on the shipped binary: with
// -require-declassification-reason, a browser upload that removes a label is
// refused with a stable, actionable 400 until it carries a reason, and the
// reason lands on the audit that -list-declassifications prints.
func TestRequireDeclassificationReasonOverTheUploadRoute(t *testing.T) {
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

	// The upload carries no label, so it relabels the document from
	// {secret} to none: a declassification.
	status, body := uploadWithReason(t, address, declassifiedName,
		"# Secret\n\nnow public\n", "")
	if status != http.StatusBadRequest {
		t.Fatalf("narrowing without a reason = %d: %s", status, body)
	}
	var failure struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(body), &failure); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if failure.Code != string(shoal.ErrorInvalidArgument) ||
		!strings.Contains(failure.Message, "declassification_reason") {
		t.Fatalf("refusal = %+v, want the actionable reason-required error", failure)
	}
	// An invalid reason is its own stable 400.
	status, body = uploadWithReason(t, address, declassifiedName,
		"# Secret\n\nnow public\n", "not valid")
	if status != http.StatusBadRequest ||
		!strings.Contains(body, "declassification reason is invalid") {
		t.Fatalf("invalid reason = %d: %s", status, body)
	}
	status, body = uploadWithReason(t, address, declassifiedName,
		"# Secret\n\nnow public\n", "ticket:SEC-42")
	if status != http.StatusOK {
		t.Fatalf("narrowing with a reason = %d: %s", status, body)
	}
	// An ordinary upload of a new document needs no reason.
	if status, body := uploadWithReason(t, address, "plain.md",
		"# Plain\n", ""); status != http.StatusOK {
		t.Fatalf("an ordinary upload = %d: %s", status, body)
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

	var listing bytes.Buffer
	if err := run(context.Background(),
		[]string{"-list-declassifications", "-state-dir", root}, &listing); err != nil {
		t.Fatal(err)
	}
	text := listing.String()
	for _, want := range []string{
		"2 record(s), 1 attempt(s), 0 without a pre_commit record",
		" intent at ", " pre_commit at ",
		`source "` + declassifiedURI + `"`,
		`reason "ticket:SEC-42"`,
		`removed labels: "secret"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("listing lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "reason (none)") {
		t.Fatalf("a refused attempt was recorded:\n%s", text)
	}
}

// TestDeclassificationReasonIsOptionalByDefault pins the default: without
// the requirement a narrowing needs no reason, and is audited all the same.
func TestDeclassificationReasonIsOptionalByDefault(t *testing.T) {
	opened, ctx := seedLabelledUpload(t, t.TempDir(), false)
	defer opened.close()
	if err := uploadThroughService(ctx, opened, ""); err != nil {
		t.Fatalf("narrowing without a reason, not required = %v", err)
	}
	records, err := opened.policyStore.Declassifications(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].ReasonCode != "" ||
		strings.Join(records[0].RemovedLabels(), ",") != "secret" {
		t.Fatalf("records = %+v", records)
	}
}

// TestUploadServiceAttachesTheReason pins, below HTTP, that the upload
// service hands the reason to the authorized client: the requirement is met
// by it, and it is the audit's reason code.
func TestUploadServiceAttachesTheReason(t *testing.T) {
	opened, ctx := seedLabelledUpload(t, t.TempDir(), true)
	defer opened.close()
	err := uploadThroughService(ctx, opened, "")
	if !authorized.IsDeclassificationReasonRequired(err) {
		t.Fatalf("narrowing without a reason = %v, want the requirement", err)
	}
	if err := uploadThroughService(ctx, opened, "change/CR-7"); err != nil {
		t.Fatalf("narrowing with a reason = %v", err)
	}
	records, err := opened.policyStore.Declassifications(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].ReasonCode != "change/CR-7" ||
		records[1].ReasonCode != "change/CR-7" {
		t.Fatalf("records = %+v", records)
	}
}

// TestListDeclassificationsIsOffline pins that the listing writes nothing
// and refuses what listUntranslatableLabels refuses: no catalog, and one with
// unflushed writes.
func TestListDeclassificationsIsOffline(t *testing.T) {
	root := t.TempDir()
	policy := filepath.Join(root, "policy")
	var output bytes.Buffer
	err := run(context.Background(),
		[]string{"-list-declassifications", "-state-dir", root}, &output)
	if err == nil || !strings.Contains(err.Error(), "no policy catalog") {
		t.Fatalf("listing without a catalog = %v", err)
	}
	if _, statErr := os.Stat(policy); !os.IsNotExist(statErr) {
		t.Fatalf("the listing created a policy catalog: %v", statErr)
	}

	// A catalog still open in a running workspace is refused.
	opened, _ := seedLabelledUpload(t, root, false)
	err = run(context.Background(),
		[]string{"-list-declassifications", "-state-dir", root}, &output)
	if err == nil || !strings.Contains(err.Error(), "unflushed writes") {
		opened.close()
		t.Fatalf("listing an open catalog = %v", err)
	}
	opened.close()

	// A cleanly closed catalog with no record lists none, and is unchanged.
	before := directoryEntries(t, policy)
	output.Reset()
	if err := run(context.Background(),
		[]string{"-list-declassifications", "-state-dir", root}, &output); err != nil {
		t.Fatal(err)
	}
	if after := directoryEntries(t, policy); strings.Join(before, "\n") != strings.Join(after, "\n") {
		t.Fatalf("the listing changed the policy directory:\n%q\n->\n%q", before, after)
	}
	if !strings.Contains(output.String(), "0 record(s), 0 attempt(s)") {
		t.Fatalf("listing = %q", output.String())
	}

	// The two listings are separate.
	if err := run(context.Background(), []string{
		"-list-declassifications", "-list-untranslatable-labels",
		"-state-dir", root,
	}, &output); err == nil {
		t.Fatal("both listings at once were accepted")
	}
}

// TestWriteDeclassificationsQuotesAndListsEveryRemovedComponent pins the
// listing's rendering: every caller-influenced string is Go-quoted, so a
// control character cannot rewrite the operator's terminal; a removed
// component that is not a label is listed too; and an attempt with no
// pre_commit record is counted.
func TestWriteDeclassificationsQuotesAndListsEveryRemovedComponent(t *testing.T) {
	policy := func(grant string) auth.Policy {
		t.Helper()
		value, err := auth.NewPolicy(auth.PolicyConfig{
			AuthorizationDomain: workspaceAuthorizationDomain,
			SourceID:            workspaceSourceID, GrantPolicyID: []byte(grant),
			Epoch: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	prior, err := authorized.LabelRule(policy("old-grant"), []string{"secret", "pii"})
	if err != nil {
		t.Fatal(err)
	}
	next, err := authorized.LabelRule(policy("new-grant"), []string{"pii"})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 12, 1, 2, 3, 4, time.UTC)
	records := []authorized.DeclassificationRecord{
		{
			AttemptID: "attempt-1", Phase: authorized.DeclassificationIntent,
			OccurredAt: at, SourceURI: "upload://workspace/x\x1b[2J",
			PriorRules: []authorized.AccessRule{prior}, Rule: next,
			Subject: "subject\nforged line", ReasonCode: "ticket:SEC-1",
		},
		{
			AttemptID: "attempt-1", Phase: authorized.DeclassificationPreCommit,
			OccurredAt: at, SourceURI: "upload://workspace/x\x1b[2J",
			DocumentID: "doc", RevisionID: "rev",
			PriorRules: []authorized.AccessRule{prior}, Rule: next,
		},
		{
			AttemptID: "attempt-2", Phase: authorized.DeclassificationIntent,
			OccurredAt: at, SourceURI: "upload://workspace/y",
			PriorRules: []authorized.AccessRule{prior}, Rule: next,
		},
	}
	var output bytes.Buffer
	writeDeclassifications(&output, "/policy", records)
	text := output.String()
	for _, want := range []string{
		"3 record(s), 2 attempt(s), 1 without a pre_commit record",
		`source "upload://workspace/x\x1b[2J"`,
		`subject "subject\nforged line"`,
		`reason "ticket:SEC-1"`,
		"reason (none)",
		`removed labels: "secret"`,
		`grant "old-grant"`,
		`document "doc" prior revision (none) revision "rev"`,
		"2026-10-12T01:02:03.000000004Z",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("listing lacks %q:\n%s", want, text)
		}
	}
	if strings.ContainsAny(text, "\x1b") || strings.Contains(text, "\nforged line") {
		t.Fatalf("the listing printed a control character raw:\n%q", text)
	}
	if strings.Contains(text, `"pii"`) {
		t.Fatalf("a kept label was listed as removed:\n%s", text)
	}
}
