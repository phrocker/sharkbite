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

// Per-file outcomes of an upload batch (#687): a deterministic refusal that
// left nothing committed is reported for its file and the batch goes on;
// any other failure stops it; the top-level error stays what a caller that
// reads only the status, code and commit-outcome header relies on.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// scriptedClient is the real Explorer, except that an ingest whose content
// is a key of errs fails with that error. calls records every content
// ingested, refused or not, so a test can show which files were tried.
type scriptedClient struct {
	*explorer.Explorer
	errs  map[string]error
	calls []string
}

func (c *scriptedClient) Ingest(
	ctx context.Context, source explorer.Source,
) (explorer.IngestResult, error) {
	c.calls = append(c.calls, source.Title)
	if err, ok := c.errs[source.Content]; ok {
		return explorer.IngestResult{}, err
	}
	return c.Explorer.Ingest(ctx, source)
}

func markedHistorical() error {
	return explorer.MarkRefusedWithoutEffect(historicalRefusal())
}

type batchWorld struct {
	server *httptest.Server
	corpus *explorer.Explorer
	client *scriptedClient
}

func newBatchWorld(t *testing.T, errs map[string]error) *batchWorld {
	t.Helper()
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = corpus.Close() })
	client := &scriptedClient{Explorer: corpus, errs: errs}
	embedded, err := NewEmbeddedService(client)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(nil)
	handler, err := NewHandler(embedded, server.Listener.Addr().String())
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	server.Config.Handler = handler
	server.Start()
	t.Cleanup(server.Close)
	return &batchWorld{server: server, corpus: corpus, client: client}
}

type batchOutcome struct {
	status        int
	header        string
	Code          string              `json:"code"`
	Message       string              `json:"message"`
	Indeterminate bool                `json:"indeterminate"`
	Files         []IngestFileOutcome `json:"files"`
	Partial       bool                `json:"partial"`
}

func (w *batchWorld) post(t *testing.T, files ...[2]string) batchOutcome {
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
	request, err := http.NewRequest(http.MethodPost, w.server.URL+"/api/v1/ingest", &body)
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
	outcome := batchOutcome{
		status: response.StatusCode, header: response.Header.Get(CommitOutcomeHeader),
	}
	if response.StatusCode != http.StatusOK {
		if err := json.Unmarshal(raw, &outcome); err != nil {
			t.Fatalf("body %q: %v", raw, err)
		}
	}
	return outcome
}

func (w *batchWorld) sources(t *testing.T) []string {
	t.Helper()
	documents, err := w.corpus.Documents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	uris := make([]string, 0, len(documents))
	for _, summary := range documents {
		uris = append(uris, strings.TrimPrefix(summary.SourceURI, "upload://workspace/"))
	}
	sort.Strings(uris)
	return uris
}

func refusedHistorical(name string) IngestFileOutcome {
	return IngestFileOutcome{
		Name: name, Status: IngestFileRefused,
		Reason:  IngestRefusedHistoricalRevision,
		Message: historicalUploadMessage(name),
	}
}

func refusedReason(name string) IngestFileOutcome {
	return IngestFileOutcome{
		Name: name, Status: IngestFileRefused,
		Reason:  IngestRefusedDeclassificationReason,
		Message: declassificationReasonRequiredMessage,
	}
}

func fileStatus(name string, status IngestFileStatus) IngestFileOutcome {
	return IngestFileOutcome{Name: name, Status: status}
}

func requireFiles(t *testing.T, got, want []IngestFileOutcome) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("files =\n  %+v\nwant\n  %+v", got, want)
	}
}

// TestHTTPIngestGoesOnPastRefusals is a mixed batch: applied, historical,
// historical, applied. Every file is tried, each refusal is named, the last
// file is applied, and since every outcome is known the batch is the first
// refusal's definite, non-2xx status, marked partial because files were
// applied. The
// reason-required refusal, which only the authorized client can produce, is
// covered through the shipped binary (cmd/shoal-explore-web).
func TestHTTPIngestGoesOnPastRefusals(t *testing.T) {
	world := newBatchWorld(t, map[string]error{
		"old\n":   markedHistorical(),
		"older\n": markedHistorical(),
	})
	got := world.post(t,
		[2]string{"one.txt", "one\n"}, [2]string{"old.txt", "old\n"},
		[2]string{"older.txt", "older\n"}, [2]string{"two.txt", "two\n"})
	if got.status != http.StatusConflict ||
		got.header != CommitOutcomePartial || got.Indeterminate || !got.Partial ||
		got.Code != string(shoal.ErrorConflict) ||
		got.Message != "conflict: "+historicalPublicMessage {
		t.Fatalf("mixed batch = %+v", got)
	}
	requireFiles(t, got.Files, []IngestFileOutcome{
		fileStatus("one.txt", IngestFileApplied),
		refusedHistorical("old.txt"),
		refusedHistorical("older.txt"),
		fileStatus("two.txt", IngestFileApplied),
	})
	if sources := world.sources(t); !reflect.DeepEqual(sources, []string{"one.txt", "two.txt"}) {
		t.Fatalf("ingested %v, want one.txt and two.txt", sources)
	}
}

// TestHTTPIngestAllRefusedBatchIsDefinite pins that a batch where every file
// was refused committed nothing: it stays the first refusal's definite
// status, code and message, with no commit-outcome header, exactly as a
// single refused file is, so a client that reads only those is right.
func TestHTTPIngestAllRefusedBatchIsDefinite(t *testing.T) {
	world := newBatchWorld(t, map[string]error{
		"old\n":   markedHistorical(),
		"older\n": markedHistorical(),
	})
	got := world.post(t,
		[2]string{"old.txt", "old\n"}, [2]string{"older.txt", "older\n"})
	if got.status != http.StatusConflict || got.header != "" || got.Indeterminate ||
		got.Partial || got.Code != string(shoal.ErrorConflict) ||
		got.Message != "conflict: "+historicalPublicMessage {
		t.Fatalf("all-refused batch = %+v", got)
	}
	requireFiles(t, got.Files, []IngestFileOutcome{
		refusedHistorical("old.txt"), refusedHistorical("older.txt"),
	})
	if sources := world.sources(t); len(sources) != 0 {
		t.Fatalf("refused files were ingested: %v", sources)
	}
}

// TestHTTPIngestStopsAtAnUnknownFailure pins that an indeterminate failure
// mid-batch still stops it: the later file is not tried, the failing file is
// reported as possibly applied, and the batch is indeterminate.
func TestHTTPIngestStopsAtAnUnknownFailure(t *testing.T) {
	world := newBatchWorld(t, map[string]error{
		"old\n": markedHistorical(),
		"broken\n": explorer.MarkIndeterminateCommit(
			shoal.NewError(shoal.ErrorUnavailable, "catalog unavailable")),
	})
	got := world.post(t,
		[2]string{"old.txt", "old\n"}, [2]string{"broken.txt", "broken\n"},
		[2]string{"later.txt", "later\n"})
	if got.status != http.StatusServiceUnavailable ||
		got.header != CommitOutcomeIndeterminate || !got.Indeterminate ||
		got.Message != "unavailable: upload failed" {
		t.Fatalf("indeterminate failure = %+v", got)
	}
	requireFiles(t, got.Files, []IngestFileOutcome{
		refusedHistorical("old.txt"),
		{Name: "broken.txt", Status: IngestFileFailed, Indeterminate: true},
		fileStatus("later.txt", IngestFileNotAttempted),
	})
	if !reflect.DeepEqual(world.client.calls, []string{"old.txt", "broken.txt"}) {
		t.Fatalf("tried %v, want the batch to stop at broken.txt", world.client.calls)
	}
}

// TestHTTPIngestIsIndeterminateOnlyForAnUnknownOutcome pins that a batch is
// indeterminate only when a file's own outcome is unknown. A definite failure
// stays definite after any file that returned a result: after a refusal or a
// plain historical result nothing took effect, and after an applied or
// unchanged file (whose outcome is known, even an authorized relabel) the
// batch is partial. An indeterminate failure, or a guaranteed refusal that is
// also marked indeterminate, stops the batch and makes it indeterminate.
func TestHTTPIngestIsIndeterminateOnlyForAnUnknownOutcome(t *testing.T) {
	world := newBatchWorld(t, map[string]error{
		"old\n":     markedHistorical(),
		"missing\n": shoal.NewError(shoal.ErrorNotFound, "not found"),
		"unknown\n": explorer.MarkIndeterminateCommit(markedHistorical()),
	})
	for _, content := range []string{"v1\n", "v2\n"} {
		if got := world.post(t, [2]string{"doc.txt", content}); got.status != http.StatusOK {
			t.Fatalf("seed = %+v", got)
		}
	}
	for _, test := range []struct {
		name         string
		first        [2]string
		firstOutcome IngestFileOutcome
		partial      bool
	}{
		{"refused", [2]string{"old.txt", "old\n"}, refusedHistorical("old.txt"), false},
		{"historical", [2]string{"doc.txt", "v1\n"}, fileStatus("doc.txt", IngestFileHistorical), false},
		{"unchanged", [2]string{"doc.txt", "v2\n"}, fileStatus("doc.txt", IngestFileUnchanged), true},
		{"applied", [2]string{"new.txt", "new\n"}, fileStatus("new.txt", IngestFileApplied), true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := world.post(t, test.first,
				[2]string{"missing.txt", "missing\n"}, [2]string{"later.txt", "later\n"})
			requireFiles(t, got.Files, []IngestFileOutcome{
				test.firstOutcome,
				fileStatus("missing.txt", IngestFileFailed),
				fileStatus("later.txt", IngestFileNotAttempted),
			})
			header := ""
			if test.partial {
				header = CommitOutcomePartial
			}
			if got.status != http.StatusNotFound || got.header != header ||
				got.Indeterminate || got.Partial != test.partial ||
				got.Message != "not_found: upload failed" {
				t.Fatalf("failure after %s = %+v, want a definite not found", test.name, got)
			}
		})
	}
	got := world.post(t, [2]string{"fresh.txt", "fresh\n"},
		[2]string{"unknown.txt", "unknown\n"}, [2]string{"later.txt", "later\n"})
	if got.status != http.StatusServiceUnavailable ||
		got.header != CommitOutcomeIndeterminate || !got.Indeterminate || !got.Partial {
		t.Fatalf("indeterminate refusal = %+v, want an indeterminate stop", got)
	}
	requireFiles(t, got.Files, []IngestFileOutcome{
		fileStatus("fresh.txt", IngestFileApplied),
		{Name: "unknown.txt", Status: IngestFileFailed, Indeterminate: true},
		fileStatus("later.txt", IngestFileNotAttempted),
	})
}

// TestHTTPIngestPartialBatchConverges is #687's retry case: a batch with a
// historical file beside a new one answers the same definite, partial 409
// on every retry (the new file comes back unchanged), never an indeterminate
// 503 that a retrying client would chase forever.
func TestHTTPIngestPartialBatchConverges(t *testing.T) {
	world := newBatchWorld(t, map[string]error{"old\n": markedHistorical()})
	for attempt := 1; attempt <= 3; attempt++ {
		got := world.post(t, [2]string{"old.txt", "old\n"}, [2]string{"new.txt", "new\n"})
		status := IngestFileApplied
		if attempt > 1 {
			status = IngestFileUnchanged
		}
		if got.status != http.StatusConflict || got.Code != string(shoal.ErrorConflict) ||
			got.header != CommitOutcomePartial || got.Indeterminate || !got.Partial ||
			got.Message != "conflict: "+historicalPublicMessage {
			t.Fatalf("attempt %d = %+v, want a definite partial 409", attempt, got)
		}
		requireFiles(t, got.Files, []IngestFileOutcome{
			refusedHistorical("old.txt"), fileStatus("new.txt", status),
		})
	}
}

// TestHTTPIngestGoesOnOnlyPastGuaranteedRefusals pins both conditions of
// ingestRefusal: the refusal without the producer's no-effect guarantee
// stops the batch, and so does a guaranteed error of another kind or code.
func TestHTTPIngestGoesOnOnlyPastGuaranteedRefusals(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{"unmarked historical", historicalRefusal()},
		{"marked but joined", errors.Join(markedHistorical(), errors.New("rollback failed"))},
		{"marked other conflict", explorer.MarkRefusedWithoutEffect(
			shoal.NewError(shoal.ErrorConflict, "injected"))},
		{"marked historical under another code", explorer.MarkRefusedWithoutEffect(
			shoal.WrapError(shoal.ErrorInternal, "injected", authorized.ErrHistoricalRevision))},
		{"marked not found", explorer.MarkRefusedWithoutEffect(
			shoal.NewError(shoal.ErrorNotFound, "injected"))},
	} {
		t.Run(test.name, func(t *testing.T) {
			world := newBatchWorld(t, map[string]error{"old\n": test.err})
			got := world.post(t,
				[2]string{"old.txt", "old\n"}, [2]string{"later.txt", "later\n"})
			if got.status == http.StatusOK || got.header != "" || got.Indeterminate {
				t.Fatalf("first-file failure = %+v, want definite", got)
			}
			requireFiles(t, got.Files, []IngestFileOutcome{
				fileStatus("old.txt", IngestFileFailed),
				fileStatus("later.txt", IngestFileNotAttempted),
			})
			if sources := world.sources(t); len(sources) != 0 {
				t.Fatalf("a later file was ingested after a stop: %v", sources)
			}
		})
	}
}

// TestEmbeddedIngestBatchErrorNeverCarriesTheGuarantee pins that one file's
// no-effect guarantee never reads as the batch's: the batch error is never
// explorer.IsRefusedWithoutEffect, while it keeps the refusal's code and
// errors.Is match, and IngestBatchPartial says whether files took effect.
func TestEmbeddedIngestBatchErrorNeverCarriesTheGuarantee(t *testing.T) {
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	embedded, err := NewEmbeddedService(&scriptedClient{Explorer: corpus,
		errs: map[string]error{"old\n": markedHistorical(), "older\n": markedHistorical()}})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		files   []UploadFile
		partial bool
	}{
		{"all refused", []UploadFile{
			{Name: "old.txt", Content: []byte("old\n")},
			{Name: "older.txt", Content: []byte("older\n")},
		}, false},
		{"refused then applied", []UploadFile{
			{Name: "old.txt", Content: []byte("old\n")},
			{Name: "new.txt", Content: []byte("new\n")},
		}, true},
	} {
		_, err := embedded.Ingest(context.Background(), IngestRequest{Files: test.files})
		if err == nil || IngestFileOutcomes(err) == nil || !IngestOutcomeKnown(err) {
			t.Fatalf("%s: error = %v, want a known batch error", test.name, err)
		}
		if explorer.IsRefusedWithoutEffect(err) || explorer.IsIndeterminateCommit(err) {
			t.Fatalf("%s: batch error carries a per-file marker: %v", test.name, err)
		}
		if got := IngestBatchPartial(err); got != test.partial {
			t.Fatalf("%s: partial = %v, want %v", test.name, got, test.partial)
		}
		if !errors.Is(err, authorized.ErrHistoricalRevision) ||
			!shoal.IsErrorCode(err, shoal.ErrorConflict) {
			t.Fatalf("%s: the top-level error lost the refusal: %v", test.name, err)
		}
	}
}

// TestHTTPIngestSuccessIsUnchanged pins that a batch where every file went
// through is the 200 response it always was, with no per-file failure body.
func TestHTTPIngestSuccessIsUnchanged(t *testing.T) {
	world := newBatchWorld(t, nil)
	got := world.post(t, [2]string{"one.txt", "one\n"}, [2]string{"two.txt", "two\n"})
	if got.status != http.StatusOK || got.header != "" || got.Files != nil {
		t.Fatalf("successful batch = %+v", got)
	}
}

// snapshotFailingClient fails the snapshot read once a file was ingested.
type snapshotFailingClient struct {
	*scriptedClient
	ingested bool
}

func (c *snapshotFailingClient) Ingest(
	ctx context.Context, source explorer.Source,
) (explorer.IngestResult, error) {
	result, err := c.scriptedClient.Ingest(ctx, source)
	if err == nil {
		c.ingested = true
	}
	return result, err
}

func (c *snapshotFailingClient) Snapshot(ctx context.Context) (explorer.Snapshot, error) {
	if c.ingested {
		return explorer.Snapshot{}, shoal.NewError(shoal.ErrorUnavailable, "snapshot read unavailable")
	}
	return c.scriptedClient.Explorer.Snapshot(ctx)
}

// lateFailureClient writes the file whose content is "late\n" and then fails,
// as the base Explorer now reports a failure after its durable write: marked
// indeterminate (#687).
type lateFailureClient struct{ *scriptedClient }

func (c *lateFailureClient) Ingest(
	ctx context.Context, source explorer.Source,
) (explorer.IngestResult, error) {
	if source.Content == "late\n" {
		if _, err := c.scriptedClient.Explorer.Ingest(ctx, source); err != nil {
			return explorer.IngestResult{}, err
		}
		return explorer.IngestResult{}, explorer.MarkIndeterminateCommit(shoal.NewError(
			shoal.ErrorConflict, "graph rebuild failed after the write"))
	}
	return c.scriptedClient.Ingest(ctx, source)
}

func newClientWorld(t *testing.T, corpus *explorer.Explorer, client explorer.BoundedClient, scripted *scriptedClient) *batchWorld {
	t.Helper()
	embedded, err := NewEmbeddedService(client)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(nil)
	handler, err := NewHandler(embedded, server.Listener.Addr().String())
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	server.Config.Handler = handler
	server.Start()
	t.Cleanup(server.Close)
	return &batchWorld{server: server, corpus: corpus, client: scripted}
}

// TestHTTPIngestSnapshotFailureAfterTheBatchIsPartial pins that a snapshot
// read failing after every file went through is reported with the files'
// outcomes, and as partial, so a reader of the status and header does not
// take a fully applied batch for nothing written.
func TestHTTPIngestSnapshotFailureAfterTheBatchIsPartial(t *testing.T) {
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	scripted := &scriptedClient{Explorer: corpus}
	world := newClientWorld(t, corpus, &snapshotFailingClient{scriptedClient: scripted}, scripted)
	got := world.post(t, [2]string{"one.txt", "one\n"}, [2]string{"two.txt", "two\n"})
	if got.status != http.StatusServiceUnavailable || got.header != CommitOutcomePartial ||
		!got.Partial || got.Indeterminate {
		t.Fatalf("snapshot failure after the batch = %+v", got)
	}
	requireFiles(t, got.Files, []IngestFileOutcome{
		fileStatus("one.txt", IngestFileApplied), fileStatus("two.txt", IngestFileApplied),
	})
	if sources := world.sources(t); len(sources) != 2 {
		t.Fatalf("ingested %v, want both files", sources)
	}
}

// TestHTTPIngestPostWriteFailureAfterARefusalIsIndeterminate is the review
// repro: a refused file, then one the base wrote before failing. The batch is
// indeterminate and so is that file's entry, although nothing before it
// committed.
func TestHTTPIngestPostWriteFailureAfterARefusalIsIndeterminate(t *testing.T) {
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	scripted := &scriptedClient{Explorer: corpus, errs: map[string]error{"old\n": markedHistorical()}}
	world := newClientWorld(t, corpus, &lateFailureClient{scripted}, scripted)
	got := world.post(t, [2]string{"old.txt", "old\n"}, [2]string{"late.txt", "late\n"})
	if got.status != http.StatusServiceUnavailable ||
		got.header != CommitOutcomeIndeterminate || !got.Indeterminate {
		t.Fatalf("post-write failure after a refusal = %+v", got)
	}
	requireFiles(t, got.Files, []IngestFileOutcome{
		refusedHistorical("old.txt"),
		{Name: "late.txt", Status: IngestFileFailed, Indeterminate: true},
	})
}

// TestRemoteIngestCarriesPerFileOutcomes drives RemoteService against a
// workspace that reports per-file outcomes: they reach the caller, an
// all-refused batch stays definite as it is upstream, and so does a partly
// applied one, which is partial.
func TestRemoteIngestCarriesPerFileOutcomes(t *testing.T) {
	world := newBatchWorld(t, map[string]error{
		"old\n":   markedHistorical(),
		"older\n": markedHistorical(),
	})
	remote, err := NewRemoteService(world.server.URL, world.server.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = remote.Ingest(context.Background(), IngestRequest{Files: []UploadFile{
		{Name: "old.txt", Content: []byte("old\n")},
		{Name: "older.txt", Content: []byte("older\n")},
	}})
	if err == nil || explorer.IsIndeterminateCommit(err) ||
		!shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("remote all-refused batch = %v, want a definite conflict", err)
	}
	requireFiles(t, IngestFileOutcomes(err), []IngestFileOutcome{
		refusedHistorical("old.txt"), refusedHistorical("older.txt"),
	})
	_, err = remote.Ingest(context.Background(), IngestRequest{Files: []UploadFile{
		{Name: "one.txt", Content: []byte("one\n")},
		{Name: "old.txt", Content: []byte("old\n")},
	}})
	if err == nil || explorer.IsIndeterminateCommit(err) || !IngestBatchPartial(err) ||
		!shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("remote partly applied batch = %v, want a definite partial conflict", err)
	}
	requireFiles(t, IngestFileOutcomes(err), []IngestFileOutcome{
		fileStatus("one.txt", IngestFileApplied), refusedHistorical("old.txt"),
	})
}

// TestRemoteIngestDistrustsInvalidOutcomes pins RemoteService's checks of a
// remote's per-file outcomes: outcomes that do not match the request are
// dropped and the multi-file failure is indeterminate as before #687, and
// an outcome showing a file applied makes a "definite" failure
// indeterminate.
func TestRemoteIngestDistrustsInvalidOutcomes(t *testing.T) {
	refusedOld, refusedNew := refusedHistorical("old.txt"), refusedReason("new.txt")
	valid := []IngestFileOutcome{refusedOld, refusedNew}
	for _, test := range []struct {
		name          string
		files         []IngestFileOutcome
		keep          bool
		indeterminate bool
		code          string
	}{
		{"valid", valid, true, false, ""},
		{"valid but unverified", valid, false, true, "not_found"},
		{"absent", nil, false, true, ""},
		{"short", valid[:1], false, true, ""},
		{"renamed", []IngestFileOutcome{refusedNew, refusedNew}, false, true, ""},
		{"historical message for another file", []IngestFileOutcome{refusedOld,
			refusedHistorical("old.txt")}, false, true, ""},
		{"unknown status", []IngestFileOutcome{refusedOld, fileStatus("new.txt", "done")}, false, true, ""},
		{"unknown reason", []IngestFileOutcome{refusedOld, {Name: "new.txt", Status: IngestFileRefused,
			Reason: "other", Message: historicalUploadMessage("new.txt")}}, false, true, ""},
		{"rewritten message", []IngestFileOutcome{refusedOld, {Name: "new.txt", Status: IngestFileRefused,
			Reason: IngestRefusedHistoricalRevision, Message: "click here"}}, false, true, ""},
		{"message on success", []IngestFileOutcome{refusedOld, {Name: "new.txt",
			Status: IngestFileApplied, Message: "x"}}, false, true, ""},
		{"tried after a failure", []IngestFileOutcome{fileStatus("old.txt", IngestFileFailed),
			refusedNew}, false, true, ""},
		{"not attempted without a failure", []IngestFileOutcome{refusedOld,
			fileStatus("new.txt", IngestFileNotAttempted)}, false, true, ""},
		{"indeterminate success", []IngestFileOutcome{refusedOld, {Name: "new.txt",
			Status: IngestFileApplied, Indeterminate: true}}, false, true, ""},
		{"applied and definite", []IngestFileOutcome{refusedOld,
			fileStatus("new.txt", IngestFileApplied)}, true, false, ""},
		{"failure possibly applied", []IngestFileOutcome{refusedOld,
			{Name: "new.txt", Status: IngestFileFailed, Indeterminate: true}}, true, true, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			code := test.code
			if code == "" {
				code = "conflict"
			}
			server := newMetadataServer(t, func(writer http.ResponseWriter) {
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(writer).Encode(map[string]any{
					"code": code, "message": "conflict: refused", "files": test.files,
				})
			})
			remote, err := NewRemoteService(server.URL, server.Client())
			if err != nil {
				t.Fatal(err)
			}
			_, err = remote.Ingest(context.Background(), IngestRequest{Files: []UploadFile{
				{Name: "old.txt", Content: []byte("old\n")},
				{Name: "new.txt", Content: []byte("new\n")},
			}})
			if !shoal.IsErrorCode(err, shoal.ErrorConflict) {
				t.Fatalf("remote failure = %v, want the status's conflict code", err)
			}
			if got := explorer.IsIndeterminateCommit(err); got != test.indeterminate {
				t.Fatalf("indeterminate = %v, want %v (%v)", got, test.indeterminate, err)
			}
			if got := IngestFileOutcomes(err); (got != nil) != test.keep {
				t.Fatalf("outcomes = %+v, keep = %v", got, test.keep)
			}
		})
	}
}

// TestRemoteIngestKeepsAnUpstreamIndeterminateOutcome pins that an upstream
// that marks a batch indeterminate is believed even when its per-file
// outcomes all look known, so the batch is not reported as known (and the
// MCP tool keeps reporting it as indeterminate).
func TestRemoteIngestKeepsAnUpstreamIndeterminateOutcome(t *testing.T) {
	server := newMetadataServer(t, func(writer http.ResponseWriter) {
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set(CommitOutcomeHeader, CommitOutcomeIndeterminate)
		writer.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"code": "unavailable", "message": "unavailable: refused", "indeterminate": true,
			"files": []IngestFileOutcome{refusedHistorical("old.txt"), refusedHistorical("new.txt")},
		})
	})
	remote, err := NewRemoteService(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = remote.Ingest(context.Background(), IngestRequest{Files: []UploadFile{
		{Name: "old.txt", Content: []byte("old\n")},
		{Name: "new.txt", Content: []byte("new\n")},
	}})
	if !explorer.IsIndeterminateCommit(err) || IngestFileOutcomes(err) == nil ||
		IngestOutcomeKnown(err) {
		t.Fatalf("upstream indeterminate batch = %v (known %v)", err, IngestOutcomeKnown(err))
	}
}

// newMetadataServer serves a workspace that supports ingest and answers
// every upload with failure.
func newMetadataServer(t *testing.T, failure func(http.ResponseWriter)) *httptest.Server {
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
	server := httptest.NewUnstartedServer(nil)
	handler, err := NewHandler(embedded, server.Listener.Addr().String())
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasSuffix(request.URL.Path, "/ingest") {
			failure(writer)
			return
		}
		handler.ServeHTTP(writer, request)
	})
	server.Start()
	t.Cleanup(server.Close)
	return server
}
