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

package authorized_test

// Ingesting an upload by name must not depend on a document the caller
// cannot read (#712). Before the fix a hidden document's name was refused
// with not_found and an absent name succeeded, over the browser upload route
// and the MCP ingest tool alike, so a reader could test whether a hidden
// document existed under a guessed filename.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/devbackfill"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/explorer/mcp"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	uploadPrincipalHeader = "X-Upload-Test-Principal"

	// Principals. The holder and the collaborator hold label "secret"; the
	// readers hold only the source policy.
	uploadHolder       = "upload-holder"
	uploadCollaborator = "upload-collaborator"
	uploadReader       = "upload-reader"
	uploadReaderTwo    = "upload-reader-two"
)

// uploadWorld is one corpus and catalog behind a real authorized client,
// with the browser upload route and the MCP ingest tool over it.
type uploadWorld struct {
	t         *testing.T
	f         *fixture
	base      explorer.Client
	store     authorized.PolicyStore
	client    *authorized.Client
	decisions map[string]auth.Decision
	server    *httptest.Server
}

func newUploadWorld(
	t *testing.T, f *fixture, store authorized.PolicyStore, requireReason bool,
) *uploadWorld {
	t.Helper()
	return newUploadWorldOver(t, f, f.base, store, requireReason)
}

// newUploadWorldOver is newUploadWorld over another base corpus.
func newUploadWorldOver(
	t *testing.T, f *fixture, base explorer.Client,
	store authorized.PolicyStore, requireReason bool,
) *uploadWorld {
	t.Helper()
	w := &uploadWorld{t: t, f: f, base: base, store: store, decisions: map[string]auth.Decision{
		uploadHolder:       f.bindingDecision(t, uploadHolder, "secret"),
		uploadCollaborator: f.bindingDecision(t, uploadCollaborator, "secret"),
		uploadReader:       f.bindingDecision(t, uploadReader),
		uploadReaderTwo:    f.bindingDecision(t, uploadReaderTwo),
	}}
	w.client = w.newClient(store, requireReason)
	service, err := webapi.NewEmbeddedService(w.client)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(nil)
	handler, err := webapi.NewAuthenticatedHandler(service,
		webapi.AuthenticatorFunc(func(request *http.Request) (auth.Decision, error) {
			decision, ok := w.decisions[request.Header.Get(uploadPrincipalHeader)]
			if !ok {
				return auth.Decision{}, shoal.NewError(
					shoal.ErrorUnauthorized, "no credential presented")
			}
			return decision, nil
		}),
		f.authority.Binder(), server.Listener.Addr().String())
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	server.Config.Handler = handler
	server.Start()
	t.Cleanup(server.Close)
	w.server = server
	return w
}

func (w *uploadWorld) newClient(
	store authorized.PolicyStore, requireReason bool,
) *authorized.Client {
	w.t.Helper()
	base := w.base
	selector := w.f.staticSelector(w.t, w.f.sourceA, w.f.policyA)
	proposals, _ := base.(explorer.OntologyProposalStore)
	client, err := authorized.NewClient(authorized.Config{
		Base:                          base,
		VectorScorer:                  trustedVectorScorer(base),
		InteractionWriter:             trustedInteractionWriter(base),
		InteractionReader:             trustedInteractionReader(base),
		SnapshotValidator:             trustedSnapshotValidator(base),
		DerivedAssertionReader:        trustedDerivedAssertionReader(base),
		FoldStore:                     trustedFoldStore(base),
		OntologyProposalStore:         proposals,
		Resolver:                      w.f.authority.Resolver(),
		PolicySelector:                selector,
		EdgePolicySelector:            selector,
		PolicyStore:                   store,
		GenerationReader:              w.f.reader,
		Clock:                         w.f.clock.Now,
		RequireDeclassificationReason: requireReason,
	})
	if err != nil {
		w.t.Fatal(err)
	}
	return client
}

func (w *uploadWorld) as(subject string) context.Context {
	w.t.Helper()
	decision, ok := w.decisions[subject]
	if !ok {
		w.t.Fatalf("no principal %q", subject)
	}
	return w.f.context(w.t, decision)
}

func uploadSource(name, content, visibility string) explorer.Source {
	source := labelledSource(
		authorized.UploadSourceURIPrefix+url.PathEscape(name), content, visibility)
	source.Title = name
	source.MediaType = explorer.MediaTypeMarkdown
	return source
}

// ingest uploads name by library call, as subject.
func (w *uploadWorld) ingest(
	client *authorized.Client, subject, name, content, visibility string,
) explorer.IngestResult {
	w.t.Helper()
	result, err := client.Ingest(w.as(subject), uploadSource(name, content, visibility))
	if err != nil {
		w.t.Fatalf("%s ingest %s: %v", subject, name, err)
	}
	return result
}

// ownURI is subject's upload namespace URI of name.
func (w *uploadWorld) ownURI(subject, name string) string {
	w.t.Helper()
	uri, err := w.client.OwnUploadSourceURI(w.as(subject), url.PathEscape(name))
	if err != nil {
		w.t.Fatal(err)
	}
	return uri
}

// sourceURIs maps each base document to its source URI, read as trusted.
func (w *uploadWorld) sourceURIs() map[shoal.ID]string {
	w.t.Helper()
	summaries, err := w.base.Documents(context.Background())
	if err != nil {
		w.t.Fatal(err)
	}
	uris := make(map[shoal.ID]string, len(summaries))
	for _, summary := range summaries {
		uris[summary.Document.ID] = summary.SourceURI
	}
	return uris
}

// requireCurrent fails unless subject reads document at revision.
func (w *uploadWorld) requireCurrent(
	subject string, document explorer.IngestResult, when string,
) {
	w.t.Helper()
	view, err := w.client.Document(w.as(subject), document.Document.ID, "")
	if err != nil {
		w.t.Fatalf("%s: %s Document = %v", when, subject, err)
	}
	if view.Revision.ID != document.Revision.ID {
		w.t.Fatalf("%s: current revision %s, want %s", when, view.Revision.ID,
			document.Revision.ID)
	}
}

// probeOutcome is everything a prober learns from uploading a name, with
// the name's own derived identities replaced by whether the document is the
// prober's own document of that name.
type probeOutcome struct {
	Status      int
	Code        string
	Message     string
	FileStatus  webapi.IngestFileStatus
	Reason      webapi.IngestRefusalReason
	Disposition explorer.IngestDisposition
	Sections    int
	Spans       int
	ProbersOwn  bool
	Files       int
	// Others and Ambiguous are the upload's name notice: the documents of
	// the name it left unchanged that the prober may upload to.
	Others    int
	Ambiguous bool
}

type uploadSurface struct {
	name  string
	probe func(w *uploadWorld, subject, name, content, reason string) probeOutcome
}

func uploadSurfaces() []uploadSurface {
	return []uploadSurface{
		{"library", (*uploadWorld).libraryUpload},
		{"http", (*uploadWorld).httpUpload},
		{"mcp", (*uploadWorld).mcpUpload},
	}
}

func (w *uploadWorld) libraryUpload(subject, name, content, reason string) probeOutcome {
	w.t.Helper()
	ctx := w.as(subject)
	if reason != "" {
		ctx = withReason(w.t, ctx, reason)
	}
	result, route, err := w.client.IngestUpload(ctx, uploadSource(name, content, ""))
	if err != nil {
		code := ""
		var failure *shoal.Error
		if errors.As(err, &failure) {
			code = string(failure.Code)
		}
		return probeOutcome{Code: code, Message: strings.ReplaceAll(err.Error(), name, "NAME")}
	}
	return probeOutcome{
		Disposition: result.Disposition, Sections: result.SectionCount,
		Spans: result.SpanCount, Files: 1,
		ProbersOwn: result.Document.ID ==
			explorer.SourceDocumentID(w.ownURI(subject, name)),
		Others: route.OtherDocuments, Ambiguous: route.Ambiguous,
	}
}

// uploadReply is the part of a failed upload route or MCP ingest reply a
// probe compares: the error and the per-file outcomes. A success is decoded
// as webapi.IngestResponse.
type uploadReply struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Error   *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Files []struct {
		Status webapi.IngestFileStatus    `json:"status"`
		Reason webapi.IngestRefusalReason `json:"reason"`
	} `json:"files"`
}

func (w *uploadWorld) outcomeOf(
	status int, raw []byte, subject, name string,
) probeOutcome {
	w.t.Helper()
	if status == http.StatusOK || status == 0 {
		var response webapi.IngestResponse
		if err := json.Unmarshal(raw, &response); err != nil {
			w.t.Fatalf("reply %q: %v", raw, err)
		}
		outcome := probeOutcome{Status: status, Files: len(response.Files)}
		if len(response.Files) == 1 {
			file := response.Files[0]
			outcome.Disposition = file.Disposition
			outcome.Sections, outcome.Spans = file.SectionCount, file.SpanCount
			outcome.ProbersOwn = file.Document.ID ==
				explorer.SourceDocumentID(w.ownURI(subject, name))
			if notice := file.NameNotice; notice != nil {
				wantURI, wantRoute := w.ownURI(subject, name), "own"
				if !outcome.ProbersOwn {
					wantURI, wantRoute = authorized.UploadSourceURIPrefix+url.PathEscape(name), "legacy"
				}
				if notice.SourceURI != wantURI || notice.Route != wantRoute || notice.Message == "" {
					w.t.Fatalf("name notice %+v is not of the document written", notice)
				}
				outcome.Others, outcome.Ambiguous = notice.OtherDocuments, notice.Ambiguous
			}
		}
		return outcome
	}
	var reply uploadReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		w.t.Fatalf("reply %q: %v", raw, err)
	}
	outcome := probeOutcome{Status: status, Code: reply.Code,
		Message: strings.ReplaceAll(reply.Message, name, "NAME"), Files: len(reply.Files)}
	if reply.Error != nil {
		outcome.Code = reply.Error.Code
		outcome.Message = strings.ReplaceAll(reply.Error.Message, name, "NAME")
	}
	if len(reply.Files) == 1 {
		outcome.FileStatus, outcome.Reason = reply.Files[0].Status, reply.Files[0].Reason
	}
	return outcome
}

// httpUpload posts name to the browser upload route as subject.
func (w *uploadWorld) httpUpload(subject, name, content, reason string) probeOutcome {
	w.t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if reason != "" {
		if err := writer.WriteField("declassification_reason", reason); err != nil {
			w.t.Fatal(err)
		}
	}
	part, err := writer.CreateFormFile("files", name)
	if err != nil {
		w.t.Fatal(err)
	}
	if _, err := part.Write([]byte(content)); err != nil {
		w.t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		w.t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, w.server.URL+"/api/v1/ingest", &body)
	if err != nil {
		w.t.Fatal(err)
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("X-Shoal-Workspace-Request", "1")
	request.Header.Set(uploadPrincipalHeader, subject)
	response, err := w.server.Client().Do(request)
	if err != nil {
		w.t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		w.t.Fatal(err)
	}
	return w.outcomeOf(response.StatusCode, raw, subject, name)
}

// mcpUpload calls the MCP ingest tool as subject.
func (w *uploadWorld) mcpUpload(subject, name, content, reason string) probeOutcome {
	w.t.Helper()
	service, err := webapi.NewEmbeddedService(w.client)
	if err != nil {
		w.t.Fatal(err)
	}
	// The server re-mints the decision per call against the wall clock, so
	// the template expires an hour after whichever clock is later; its
	// identity, and so its upload namespace, is the subject's.
	template := w.decisions[subject]
	expires := w.f.clock.Now()
	if now := time.Now(); now.After(expires) {
		expires = now
	}
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: template.Subject(), Actor: template.Actor(),
		AuthorizationDomain:   template.AuthorizationDomain(),
		AllowedOperations:     template.AllowedOperations(),
		PermittedSourceIDs:    template.PermittedSourceIDs(),
		PermittedPolicyIDs:    template.PermittedPolicyIDs(),
		PolicyGeneration:      template.PolicyGeneration(),
		AuthenticationExpires: expires.Add(time.Hour),
		RequestID:             "mcp-template-request",
	})
	if err != nil {
		w.t.Fatal(err)
	}
	// The tool call's own interaction record is not under test here.
	recorder, err := interaction.NewRecorder(context.Background(), discardedInteractions{})
	if err != nil {
		w.t.Fatal(err)
	}
	server, err := mcp.NewServer(mcp.Config{
		Service: service, Authority: w.f.authority,
		Decisions: mcp.DecisionProviderFunc(func(context.Context) (auth.Decision, error) {
			return decision, nil
		}),
		Recorder:  recorder,
		Snapshots: w.client,
	})
	if err != nil {
		w.t.Fatal(err)
	}
	arguments := map[string]any{"files": []map[string]string{{
		"name": name, "content": base64.StdEncoding.EncodeToString([]byte(content)),
	}}}
	if reason != "" {
		arguments["declassification_reason"] = reason
	}
	encoded, err := json.Marshal(arguments)
	if err != nil {
		w.t.Fatal(err)
	}
	call, err := json.Marshal(mcp.CallToolParams{
		Name: mcp.ToolIngest, Arguments: encoded,
	})
	if err != nil {
		w.t.Fatal(err)
	}
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"` +
			mcp.ProtocolVersion +
			`","capabilities":{},"clientInfo":{"name":"upload-test","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":` + string(call) + `}`,
	}, "\n") + "\n"
	var output bytes.Buffer
	if err := server.Serve(context.Background(), strings.NewReader(input), &output); err != nil {
		w.t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		var response struct {
			ID     json.RawMessage `json:"id"`
			Result *mcp.ToolResult `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &response); err != nil {
			w.t.Fatalf("MCP line %q: %v", line, err)
		}
		if string(response.ID) != "2" {
			continue
		}
		if response.Result == nil {
			w.t.Fatalf("MCP ingest protocol error: %s", response.Error)
		}
		status := 0
		if response.Result.IsError {
			status = 1
		}
		return w.outcomeOf(status, response.Result.StructuredContent, subject, name)
	}
	w.t.Fatalf("no MCP ingest reply in %s", output.String())
	return probeOutcome{}
}

type discardedInteractions struct{}

func (discardedInteractions) EnsureInteractionSink(context.Context) error { return nil }

func (discardedInteractions) RecordInteraction(context.Context, interaction.Session) error {
	return nil
}

func (discardedInteractions) RecordInteractionResult(
	_ context.Context, session interaction.Session,
) (interaction.Session, error) {
	return session, nil
}

func requireSameProbe(t *testing.T, what string, got, absent probeOutcome) {
	t.Helper()
	if got != absent {
		t.Fatalf("%s:\n got    %+v\n absent %+v", what, got, absent)
	}
}

// TestUploadProbesCannotTellHiddenUploadsFromAbsentOnes is the issue's
// reproduction on every surface that ingests by name (library, browser
// upload route, MCP ingest tool) and on both catalogs: a reader uploading
// the name of a hidden document, one uploaded under #712 and one stored
// before it, gets exactly what it gets for a name never uploaded, writes its
// own document and leaves the hidden ones as they were.
func TestUploadProbesCannotTellHiddenUploadsFromAbsentOnes(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		w := newUploadWorld(t, newFixture(t), store, false)
		hidden := w.ingest(w.client, uploadHolder, "merger-plan.md",
			"# Merger\n\nthe secret plan\n", "secret")
		legacy, err := authorized.IngestAsBefore712(w.client, w.as(uploadHolder),
			uploadSource("legacy-plan.md", "# Legacy\n\nthe old secret plan\n", "secret"))
		if err != nil {
			t.Fatal(err)
		}
		// The probes are not vacuous: both documents exist where a name
		// used to collide, and the reader can read neither.
		uris := w.sourceURIs()
		if uris[hidden.Document.ID] != w.ownURI(uploadHolder, "merger-plan.md") ||
			uris[legacy.Document.ID] != authorized.UploadSourceURIPrefix+"legacy-plan.md" {
			t.Fatalf("hidden documents are stored at %v", uris)
		}
		for _, document := range []explorer.IngestResult{hidden, legacy} {
			if _, err := w.client.Document(w.as(uploadReader), document.Document.ID, ""); !shoal.IsErrorCode(
				err, shoal.ErrorNotFound) {
				t.Fatalf("reader Document(hidden) = %v, want not_found", err)
			}
		}

		for _, surface := range uploadSurfaces() {
			content := "# Probe\n\nwhat a prober uploads over " + surface.name + "\n"
			absent := surface.probe(w, uploadReader, "never-uploaded-"+surface.name+".md", content, "")
			if absent.Code != "" || absent.Files != 1 ||
				absent.Disposition != explorer.IngestApplied || !absent.ProbersOwn {
				t.Fatalf("%s: absent name = %+v, want applied in the reader's namespace",
					surface.name, absent)
			}
			for _, name := range []string{"merger-plan.md", "legacy-plan.md"} {
				requireSameProbe(t, surface.name+" upload of hidden "+name,
					surface.probe(w, uploadReader, name, content, ""), absent)
			}
		}
		w.requireCurrent(uploadHolder, hidden, "after the probes")
		w.requireCurrent(uploadHolder, legacy, "after the probes")
		if records := declassifications(t, store); len(records) != 0 {
			t.Fatalf("a probe was audited as a declassification: %+v", records)
		}
	})
}

// TestUploadByNameIsSharedAmongItsReaders: principals authorized under a
// document share it by name, so a re-upload, a relabel by another holder of
// its labels (#630, #680), the reason policy (#664/#685) and the historical
// refusal (#674) all act on that one document, over the upload route.
func TestUploadByNameIsSharedAmongItsReaders(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		w := newUploadWorld(t, newFixture(t), store, true)
		first := w.ingest(w.client, uploadHolder, "plan.md", "# Plan\n\nv1\n", "secret")
		second := w.ingest(w.client, uploadHolder, "plan.md", "# Plan\n\nv2\n", "secret")
		if second.Document.ID != first.Document.ID ||
			second.Revision.ID == first.Revision.ID {
			t.Fatalf("the holder's re-upload made %+v, then %+v", first, second)
		}
		// Historical content is refused on the routed document (#674),
		// for the holder and for another holder of its labels.
		for _, subject := range []string{uploadHolder, uploadCollaborator} {
			_, err := w.client.Ingest(w.as(subject),
				uploadSource("plan.md", "# Plan\n\nv1\n", "secret"))
			if !shoal.IsErrorCode(err, shoal.ErrorConflict) ||
				!explorer.IsRefusedWithoutEffect(err) {
				t.Fatalf("%s historical re-upload = %v, want the historical refusal", subject, err)
			}
		}
		w.requireCurrent(uploadHolder, second, "after the historical refusals")

		// The collaborator's unlabelled upload relabels the holder's
		// document: refused without a reason only after it is authorized
		// under the replaced rule (#685), then applied with one.
		refused := w.httpUpload(uploadCollaborator, "plan.md", "# Plan\n\npublic\n", "")
		if refused.Status != http.StatusBadRequest ||
			refused.FileStatus != webapi.IngestFileRefused ||
			refused.Reason != webapi.IngestRefusedDeclassificationReason {
			t.Fatalf("relabel without a reason = %+v", refused)
		}
		w.requireCurrent(uploadHolder, second, "after the refused relabel")
		relabelled := w.httpUpload(uploadCollaborator, "plan.md", "# Plan\n\npublic\n",
			"ticket:SEC-712")
		if relabelled.Status != http.StatusOK || relabelled.Files != 1 ||
			relabelled.Disposition != explorer.IngestApplied || relabelled.ProbersOwn {
			t.Fatalf("collaborator relabel = %+v, want applied to the holder's document",
				relabelled)
		}
		view, err := w.client.Document(w.as(uploadReader), first.Document.ID, "")
		if err != nil || view.Revision.ID == second.Revision.ID {
			t.Fatalf("after the relabel the reader reads %+v (%v)", view.Revision, err)
		}
		holderURI := w.ownURI(uploadHolder, "plan.md")
		records := declassifications(t, store)
		if len(records) != 2 {
			t.Fatalf("declassification records = %+v", records)
		}
		for _, record := range records {
			if record.SourceURI != holderURI || record.DocumentID != first.Document.ID ||
				record.Subject != uploadCollaborator {
				t.Fatalf("declassification record = %+v", record)
			}
		}

		// Now public, the document is shared by name with every reader.
		shared := w.httpUpload(uploadReader, "plan.md", "# Plan\n\nreader edit\n", "")
		if shared.Status != http.StatusOK || shared.ProbersOwn ||
			shared.Disposition != explorer.IngestApplied {
			t.Fatalf("reader edit of the public document = %+v", shared)
		}
		if uris := w.sourceURIs(); len(uris) != 1 || uris[first.Document.ID] != holderURI {
			t.Fatalf("shared edits wrote other documents: %v", uris)
		}
	})
}

// TestUploadByNameNeverRoutesToAnAmbiguousOrHiddenName: a name two readable
// documents of other uploaders share, or one only a hidden document holds,
// routes to the caller's own namespace, where it stays.
func TestUploadByNameNeverRoutesToAnAmbiguousOrHiddenName(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		w := newUploadWorld(t, newFixture(t), store, false)
		holders := w.ingest(w.client, uploadHolder, "dup.md", "# Dup\n\nholder\n", "secret")
		readers := w.ingest(w.client, uploadReader, "dup.md", "# Dup\n\nreader\n", "")
		if readers.Document.ID == holders.Document.ID ||
			readers.Document.ID != explorer.SourceDocumentID(w.ownURI(uploadReader, "dup.md")) {
			t.Fatalf("the reader's upload of a hidden name = %+v", readers)
		}
		own := explorer.SourceDocumentID(w.ownURI(uploadCollaborator, "dup.md"))
		for _, content := range []string{"# Dup\n\nmine\n", "# Dup\n\nmine again\n"} {
			got := w.ingest(w.client, uploadCollaborator, "dup.md", content, "")
			if got.Document.ID != own {
				t.Fatalf("an ambiguous name routed to %s, want the caller's own %s",
					got.Document.ID, own)
			}
		}
		// Another reader sees the reader's and the collaborator's public
		// documents, also two, so it gets its own too.
		other := w.ingest(w.client, uploadReaderTwo, "dup.md", "# Dup\n\ntwo\n", "")
		if other.Document.ID != explorer.SourceDocumentID(w.ownURI(uploadReaderTwo, "dup.md")) {
			t.Fatalf("reader two routed to %s", other.Document.ID)
		}
		w.requireCurrent(uploadHolder, holders, "after the ambiguous uploads")
		w.requireCurrent(uploadReader, readers, "after the ambiguous uploads")

		// The caller's own document of a name comes first: once the
		// holder's hidden one is relabelled public, the reader still edits
		// its own.
		w.ingest(w.client, uploadHolder, "pick.md", "# Pick\n\nsecret\n", "secret")
		w.ingest(w.client, uploadReader, "pick.md", "# Pick\n\nreader\n", "")
		public := w.ingest(w.client, uploadHolder, "pick.md", "# Pick\n\npublic now\n", "")
		if public.Document.ID != explorer.SourceDocumentID(w.ownURI(uploadHolder, "pick.md")) {
			t.Fatal("the holder's relabel left its own document")
		}
		mine := w.ingest(w.client, uploadReader, "pick.md", "# Pick\n\nreader again\n", "")
		if mine.Document.ID != explorer.SourceDocumentID(w.ownURI(uploadReader, "pick.md")) {
			t.Fatalf("the reader's own document lost to another: %s", mine.Document.ID)
		}
	})
}

// TestUploadsStoredBefore712StayWithTheirReaders: documents uploaded before
// #712 keep their un-namespaced URIs, stay readable, and stay re-uploadable
// and relabelable by name by everyone authorized under them, after a
// restart onto this release (a new client; for the durable catalog, a
// reopened store). A reader outside one gets its own document instead.
func TestUploadsStoredBefore712StayWithTheirReaders(t *testing.T) {
	for _, durable := range []bool{false, true} {
		name := "memory"
		if durable {
			name = "durable"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			dir := t.TempDir()
			open := func() authorized.PolicyStore {
				if !durable {
					return f.store
				}
				store, err := authorized.OpenDurablePolicyStore(dir)
				if err != nil {
					t.Fatal(err)
				}
				return store
			}
			before := open()
			old := newUploadWorld(t, f, before, false)
			secret, err := authorized.IngestAsBefore712(old.client, old.as(uploadHolder),
				uploadSource("legacy.md", "# Legacy\n\nv1\n", "secret"))
			if err != nil {
				t.Fatal(err)
			}
			public, err := authorized.IngestAsBefore712(old.client, old.as(uploadReader),
				uploadSource("public.md", "# Public\n\nv1\n", ""))
			if err != nil {
				t.Fatal(err)
			}
			if durable {
				if err := before.(*authorized.DurablePolicyStore).Close(); err != nil {
					t.Fatal(err)
				}
			}
			after := open()
			if durable {
				t.Cleanup(func() { _ = after.(*authorized.DurablePolicyStore).Close() })
			}
			w := newUploadWorld(t, f, after, false)
			w.requireCurrent(uploadReaderTwo, public, "after the upgrade")
			w.requireCurrent(uploadHolder, secret, "after the upgrade")

			// A reader outside the labelled one gets its own document.
			outside := w.ingest(w.client, uploadReader, "legacy.md", "# Legacy\n\nprobe\n", "")
			if outside.Document.ID == secret.Document.ID {
				t.Fatal("a reader outside a pre-#712 upload wrote it")
			}
			w.requireCurrent(uploadHolder, secret, "after the outside upload")
			// Its holder re-uploads it in place; another holder relabels
			// it in place, ahead of the reader's own document of the name.
			again := w.ingest(w.client, uploadHolder, "legacy.md", "# Legacy\n\nv2\n", "secret")
			if again.Document.ID != secret.Document.ID ||
				again.Disposition != explorer.IngestApplied {
				t.Fatalf("the holder's re-upload = %+v, want a revision of %s",
					again, secret.Document.ID)
			}
			relabelled := w.httpUpload(uploadCollaborator, "legacy.md", "# Legacy\n\npublic\n",
				"ticket:SEC-712")
			if relabelled.Status != http.StatusOK || relabelled.Files != 1 ||
				relabelled.Disposition != explorer.IngestApplied || relabelled.ProbersOwn ||
				// The reader's own public document of the name was left as it was.
				relabelled.Others != 1 || relabelled.Ambiguous {
				t.Fatalf("relabel of a pre-#712 upload = %+v", relabelled)
			}
			records := declassifications(t, after)
			if len(records) != 2 ||
				records[0].SourceURI != authorized.UploadSourceURIPrefix+"legacy.md" ||
				records[0].DocumentID != secret.Document.ID {
				t.Fatalf("declassification records = %+v", records)
			}
			// The public one is shared by name, over MCP too.
			edited := w.mcpUpload(uploadReaderTwo, "public.md", "# Public\n\nv2\n", "")
			if edited.Code != "" || edited.Files != 1 || edited.ProbersOwn ||
				edited.Disposition != explorer.IngestApplied {
				t.Fatalf("MCP edit of a pre-#712 public upload = %+v", edited)
			}
			uris := w.sourceURIs()
			if uris[secret.Document.ID] != authorized.UploadSourceURIPrefix+"legacy.md" ||
				uris[public.Document.ID] != authorized.UploadSourceURIPrefix+"public.md" ||
				len(uris) != 3 {
				t.Fatalf("source URIs after the upgrade = %v", uris)
			}
		})
	}
}

// TestUploadNamespaceRefusesOtherNamespaces: a URI in another uploader's
// namespace is a target, written only when the caller is authorized to
// ingest under the document there. A hidden document there and nothing at
// all are refused with the same InvalidArgument, and a malformed URI under
// the upload prefix with another one, before anything is written. Other
// schemes are not routed: they are trusted ingest, and a hidden document
// there is still refused with not_found.
func TestUploadNamespaceRefusesOtherNamespaces(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		w := newUploadWorld(t, newFixture(t), store, false)
		hidden := w.ingest(w.client, uploadHolder, "x.md", "# X\n\nsecret\n", "secret")
		target := w.ingest(w.client, uploadReaderTwo, "t.md", "# T\n\npublic\n", "")
		count := len(w.sourceURIs())
		holderNamespace := strings.TrimSuffix(w.ownURI(uploadHolder, "x.md"), "x.md")
		reader := w.as(uploadReader)
		ingestAt := func(uri string) error {
			source := uploadSource("x.md", "# X\n\nprobe\n", "")
			source.URI = uri
			_, err := w.client.Ingest(reader, source)
			return err
		}
		absentTarget := ingestAt(holderNamespace + "absent.md")
		if !shoal.IsErrorCode(absentTarget, shoal.ErrorInvalidArgument) ||
			!strings.Contains(absentTarget.Error(), "another uploader's namespace") {
			t.Fatalf("absent target = %v, want the target refusal", absentTarget)
		}
		for _, uri := range []string{
			holderNamespace + "x.md",
			authorized.UploadSourceURIPrefix + "u-" + strings.Repeat("0", 32) + "/x.md",
		} {
			if err := ingestAt(uri); err == nil || err.Error() != absentTarget.Error() {
				t.Fatalf("ingest at %q = %v, want %v", uri, err, absentTarget)
			}
		}
		malformed := ingestAt(authorized.UploadSourceURIPrefix)
		if !shoal.IsErrorCode(malformed, shoal.ErrorInvalidArgument) ||
			!strings.Contains(malformed.Error(), "upload namespace") {
			t.Fatalf("malformed upload URI = %v, want the namespace refusal", malformed)
		}
		for _, uri := range []string{
			authorized.UploadSourceURIPrefix + "a/b",
			authorized.UploadSourceURIPrefix + "u-short/x.md",
			authorized.UploadSourceURIPrefix + "u-" + strings.Repeat("A", 32) + "/x.md",
			holderNamespace,
			holderNamespace + "x.md/y",
			w.ownURI(uploadReader, "x.md") + "/y",
			strings.TrimSuffix(w.ownURI(uploadReader, "x.md"), "x.md"),
		} {
			if err := ingestAt(uri); err == nil || err.Error() != malformed.Error() {
				t.Fatalf("ingest at %q = %v, want %v", uri, err, malformed)
			}
		}
		if got := len(w.sourceURIs()); got != count {
			t.Fatalf("refused ingests wrote %d documents", got-count)
		}
		w.requireCurrent(uploadHolder, hidden, "after the refusals")

		// A document of another uploader the caller may upload to is a
		// target by its source URI.
		source := uploadSource("t.md", "# T\n\ntargeted by URI\n", "")
		source.URI = w.ownURI(uploadReaderTwo, "t.md")
		byURI, route, err := w.client.IngestUpload(reader, source)
		if err != nil || byURI.Document.ID != target.Document.ID ||
			route.Kind != authorized.UploadRouteShared {
			t.Fatalf("target by URI = %+v, %+v (%v), want a revision of %s",
				byURI, route, err, target.Document.ID)
		}

		// The caller's own namespaced URI is accepted, as the same
		// document its name routes to.
		if err := ingestAt(w.ownURI(uploadReader, "x.md")); err != nil {
			t.Fatalf("own namespace = %v", err)
		}
		byName := w.ingest(w.client, uploadReader, "x.md", "# X\n\nby name\n", "")
		if byName.Document.ID != explorer.SourceDocumentID(w.ownURI(uploadReader, "x.md")) {
			t.Fatalf("by name routed to %s", byName.Document.ID)
		}

		// A trusted scheme is stored as is, and is refused over a hidden
		// document as before #712 (documented as trusted ingest only).
		if _, err := w.client.Ingest(w.as(uploadHolder),
			labelledSource("file:///upload-namespace/hidden.txt", "secret", "secret")); err != nil {
			t.Fatal(err)
		}
		if _, err := w.client.Ingest(reader,
			labelledSource("file:///upload-namespace/hidden.txt", "probe", "")); !shoal.IsErrorCode(
			err, shoal.ErrorNotFound) {
			t.Fatalf("trusted scheme over a hidden document = %v, want not_found", err)
		}
	})
}

// TestUploadNamespaceIsTheUploadersIdentity: the namespace is a digest of
// the identity alone, the same on every call and for every request, apart
// for every identity, and unrelated to the application edge namespace.
func TestUploadNamespaceIsTheUploadersIdentity(t *testing.T) {
	w := newUploadWorld(t, newFixture(t), authorized.NewMemoryPolicyStore(), false)
	holder := w.ownURI(uploadHolder, "n.md")
	if !strings.HasPrefix(holder, authorized.UploadSourceURIPrefix+"u-") ||
		!strings.HasSuffix(holder, "/n.md") ||
		len(holder) != len(authorized.UploadSourceURIPrefix)+len("u-")+32+len("/n.md") {
		t.Fatalf("namespaced URI = %q", holder)
	}
	// The same identity under another request and other grants.
	again, err := auth.NewDecision(auth.DecisionConfig{
		Subject: uploadHolder, Actor: uploadHolder + "-actor",
		AuthorizationDomain:   w.f.domain,
		AllowedOperations:     []auth.Operation{auth.OperationIngest, auth.OperationConnect},
		PermittedSourceIDs:    [][]byte{w.f.sourceA},
		PermittedPolicyIDs:    [][]byte{w.f.policyA},
		PolicyGeneration:      1,
		AuthenticationExpires: w.f.clock.Now().Add(time.Minute),
		RequestID:             "another-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := w.f.context(t, again)
	if got, err := w.client.OwnUploadSourceURI(ctx, "n.md"); err != nil || got != holder {
		t.Fatalf("the same identity's namespace = %q (%v), want %q", got, err, holder)
	}
	if reader := w.ownURI(uploadReader, "n.md"); reader == holder {
		t.Fatal("two identities share an upload namespace")
	}
	edge, err := w.client.ApplicationEdgeID(ctx, "n.md")
	if err != nil {
		t.Fatal(err)
	}
	edgeDigest := strings.TrimSuffix(strings.TrimPrefix(string(edge), "app-edge_"), "/n.md")
	if strings.Contains(holder, edgeDigest) {
		t.Fatal("the upload namespace reuses the application edge digest")
	}
	for _, name := range []string{"", "a/b"} {
		if _, err := w.client.OwnUploadSourceURI(ctx, name); !shoal.IsErrorCode(
			err, shoal.ErrorInvalidArgument) {
			t.Fatalf("OwnUploadSourceURI(%q) = %v", name, err)
		}
	}
}

// TestUploadByNameSkipsAHalfFinishedRelabelOfAHiddenRule: a relabel whose
// claim never committed leaves the claim pending under the new rule with the
// old rule as its previous one (#665). A reader of the new rule only is
// outside the old one, so the name routes it to its own namespace, exactly
// as for an absent name, even though the new rule and the current
// registration admit it.
func TestUploadByNameSkipsAHalfFinishedRelabelOfAHiddenRule(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		failing := &failFinalizeStore{PolicyStore: store}
		w := newUploadWorld(t, newFixture(t), failing, false)
		w.ingest(w.client, uploadHolder, "pending.md", "# Pending\n\nsecret\n", "secret")
		failing.commitErr = shoal.NewError(shoal.ErrorUnavailable, "injected commit failure")
		if _, err := w.client.Ingest(w.as(uploadHolder),
			uploadSource("pending.md", "# Pending\n\npublic\n", "")); err == nil {
			t.Fatal("the relabel committed through an injected failure")
		}
		failing.commitErr = nil
		claim, ok, err := store.SourceClaim(context.Background(),
			w.ownURI(uploadHolder, "pending.md"))
		if err != nil || !ok || !claim.Pending || claim.PreviousRule == nil {
			t.Fatalf("claim = %+v, %v (%v), want pending with the replaced rule", claim, ok, err)
		}
		absent := w.libraryUpload(uploadReader, "never-pending.md", "# Probe\n", "")
		requireSameProbe(t, "upload of a half-relabelled hidden name",
			w.libraryUpload(uploadReader, "pending.md", "# Probe\n", ""), absent)
		if !absent.ProbersOwn || absent.Disposition != explorer.IngestApplied {
			t.Fatalf("absent name = %+v", absent)
		}
	})
}

// TestUploadByNameSkipsAHalfFinishedNarrowing: a narrowing whose
// registration failed leaves the claim pending under the new, labelled rule
// while the old public registration stays current (#665). A reader of the
// public rule only is outside the claim's rule, so the name routes it to its
// own namespace rather than into a refusal of the pending relabel.
func TestUploadByNameSkipsAHalfFinishedNarrowing(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		failing := &failStore{PolicyStore: store}
		w := newUploadWorld(t, newFixture(t), failing, false)
		public := w.ingest(w.client, uploadHolder, "narrowing.md", "# N\n\npublic\n", "")
		failing.mu.Lock()
		failing.revisionFailures = 1
		failing.mu.Unlock()
		if _, err := w.client.Ingest(w.as(uploadHolder),
			uploadSource("narrowing.md", "# N\n\nsecret\n", "secret")); err == nil {
			t.Fatal("the narrowing registered through an injected failure")
		}
		claim, ok, err := store.SourceClaim(context.Background(),
			w.ownURI(uploadHolder, "narrowing.md"))
		if err != nil || !ok || !claim.Pending || claim.PreviousRule == nil {
			t.Fatalf("claim = %+v, %v (%v), want pending with the replaced rule", claim, ok, err)
		}
		if _, err := w.client.Document(w.as(uploadReader), public.Document.ID, ""); err != nil {
			t.Fatalf("the reader lost the public registration: %v", err)
		}
		got := w.libraryUpload(uploadReader, "narrowing.md", "# Probe\n", "")
		if got.Code != "" || !got.ProbersOwn || got.Disposition != explorer.IngestApplied {
			t.Fatalf("reader upload over a pending narrowing = %+v, want its own document", got)
		}
	})
}

// TestUploadByNameSkipsHiddenUnclaimedLegacyUploads: an upload from a
// catalog older than source claims has a registration and no claim. Hidden,
// it is skipped like any hidden document, also once a relabel of it was left
// half-finished with a pending claim that records no previous rule (#665):
// only its registration then shows the reader is outside it.
func TestUploadByNameSkipsHiddenUnclaimedLegacyUploads(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		failing := &failStore{PolicyStore: store}
		w := newUploadWorld(t, newFixture(t), failing, false)
		unclaimedURI := authorized.UploadSourceURIPrefix + "unclaimed.md"
		halfURI := authorized.UploadSourceURIPrefix + "half.md"
		for _, uri := range []string{unclaimedURI, halfURI} {
			if _, err := w.f.base.Ingest(context.Background(),
				labelledSource(uri, "labelled before claims", "secret")); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := w.client.BackfillExistingDocumentsForDevelopment(
			w.as(uploadHolder), devbackfill.NewCapability()); err != nil {
			t.Fatal(err)
		}
		for _, uri := range []string{unclaimedURI, halfURI} {
			if _, ok, err := store.SourceClaim(context.Background(), uri); err != nil || ok {
				t.Fatalf("backfill created a claim (%v): the legacy path is not exercised", err)
			}
		}
		absent := w.libraryUpload(uploadReader, "never-unclaimed.md", "# Probe\n", "")
		requireSameProbe(t, "upload of a hidden unclaimed name",
			w.libraryUpload(uploadReader, "unclaimed.md", "# Probe\n", ""), absent)

		failing.mu.Lock()
		failing.revisionFailures = 1
		failing.mu.Unlock()
		if _, err := w.client.Ingest(w.as(uploadHolder),
			uploadSource("half.md", "# Half\n\npublic\n", "")); err == nil {
			t.Fatal("the relabel registered through an injected failure")
		}
		claim, ok, err := store.SourceClaim(context.Background(), halfURI)
		if err != nil || !ok || !claim.Pending || claim.PreviousRule != nil {
			t.Fatalf("claim = %+v, %v (%v), want pending with no previous rule", claim, ok, err)
		}
		absent = w.libraryUpload(uploadReader, "never-half.md", "# Probe\n", "")
		requireSameProbe(t, "upload of a half-relabelled unclaimed name",
			w.libraryUpload(uploadReader, "half.md", "# Probe\n", ""), absent)
	})
}

// TestUploadNamespaceBindsTheIdentityNotItsAuthority pins the documented
// residual: a narrowed session of the uploader's identity routes to the same
// namespace, even when the document there is hidden from it and another
// uploader's readable document has the name, so it is refused there as an
// ingest over a hidden document is, rather than writing anyone else's.
func TestUploadNamespaceBindsTheIdentityNotItsAuthority(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		w := newUploadWorld(t, newFixture(t), store, false)
		w.decisions["narrowed-holder"] = w.f.bindingDecision(t, uploadHolder)
		hidden := w.ingest(w.client, uploadHolder, "narrow.md", "# Narrow\n\nsecret\n", "secret")
		readers := w.ingest(w.client, uploadReader, "narrow.md", "# Narrow\n\nreader\n", "")
		if w.ownURI("narrowed-holder", "narrow.md") != w.ownURI(uploadHolder, "narrow.md") {
			t.Fatal("a narrowed session moved the namespace")
		}
		_, err := w.client.Ingest(w.as("narrowed-holder"),
			uploadSource("narrow.md", "# Narrow\n\nnarrowed\n", ""))
		if !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			t.Fatalf("narrowed session = %v, want not_found in its own namespace", err)
		}
		w.requireCurrent(uploadHolder, hidden, "after the narrowed session")
		w.requireCurrent(uploadReader, readers, "after the narrowed session")
	})
}

// TestUploadByNameReportsTheDocumentsItLeftUnchanged: a name that reaches
// several documents the caller may upload to is never silently forked. A
// holder's relabel by name that finds the name ambiguous goes to its own
// document and says so, leaving the labelled document and the audit as they
// were; the holder then reaches the labelled document by its source URI. An
// upload to the caller's own document while others of the name are eligible
// says so too.
func TestUploadByNameReportsTheDocumentsItLeftUnchanged(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		w := newUploadWorld(t, newFixture(t), store, false)
		var labelled explorer.IngestResult
		for _, surface := range uploadSurfaces() {
			name := "r-" + surface.name + ".md"
			document := w.ingest(w.client, uploadHolder, name, "# R\n\nsecret\n", "secret")
			if surface.name == "library" {
				labelled = document
			}
			w.ingest(w.client, uploadReader, name, "# R\n\nreader\n", "")
			got := surface.probe(w, uploadCollaborator, name, "# R\n\nrelabel attempt\n",
				"ticket:SEC-712")
			if got.Code != "" || !got.ProbersOwn || got.Others != 2 || !got.Ambiguous {
				t.Fatalf("%s: ambiguous relabel by name = %+v, want the collaborator's own "+
					"document with an ambiguity notice of 2", surface.name, got)
			}
			w.requireCurrent(uploadHolder, document, "after the ambiguous relabel")
		}
		if _, err := w.client.Document(w.as(uploadReader), labelled.Document.ID, ""); !shoal.IsErrorCode(
			err, shoal.ErrorNotFound) {
			t.Fatalf("an ambiguous relabel opened the labelled document: %v", err)
		}
		if records := declassifications(t, store); len(records) != 0 {
			t.Fatalf("an ambiguous upload was audited: %+v", records)
		}
		// Now the collaborator has its own document of the name, and the
		// two others stay eligible: every upload by name says so.
		again := w.libraryUpload(uploadCollaborator, "r-library.md", "# R\n\nmine\n", "")
		if !again.ProbersOwn || again.Others != 2 || again.Ambiguous {
			t.Fatalf("own document beside two others = %+v", again)
		}
		// The holder's document by its source URI is a target.
		source := uploadSource("r-library.md", "# R\n\npublic\n", "")
		source.URI = w.ownURI(uploadHolder, "r-library.md")
		relabelled, route, err := w.client.IngestUpload(
			withReason(t, w.as(uploadCollaborator), "ticket:SEC-712"), source)
		if err != nil || relabelled.Document.ID != labelled.Document.ID ||
			route.Kind != authorized.UploadRouteShared || route.OtherDocuments != 0 {
			t.Fatalf("relabel by source URI = %+v, %+v (%v)", relabelled, route, err)
		}
		if records := declassifications(t, store); len(records) != 2 ||
			records[0].SourceURI != source.URI {
			t.Fatalf("relabel by source URI audit = %+v", records)
		}
		// With a single other document of the name and none of its own, a
		// reader's upload shares it, with no notice.
		shared := w.libraryUpload(uploadReaderTwo, "solo.md", "# S\n", "")
		if shared.Others != 0 || !shared.ProbersOwn {
			t.Fatalf("first upload of a name = %+v", shared)
		}
		edit := w.httpUpload(uploadReader, "solo.md", "# S\n\nedited\n", "")
		if edit.Status != http.StatusOK || edit.ProbersOwn || edit.Others != 0 {
			t.Fatalf("shared edit = %+v, want no notice", edit)
		}
	})
}

// ingestFailingBase fails the next ingest it is armed for before writing
// anything, as indeterminate, so the claim is left pending with no document.
type ingestFailingBase struct {
	*explorer.Explorer
	fail bool
}

func (b *ingestFailingBase) Ingest(
	ctx context.Context, source explorer.Source,
) (explorer.IngestResult, error) {
	if b.fail {
		b.fail = false
		return explorer.IngestResult{}, explorer.MarkIndeterminateCommit(
			shoal.NewError(shoal.ErrorUnavailable, "injected indeterminate ingest"))
	}
	return b.Explorer.Ingest(ctx, source)
}

// TestUploadByNameRetriesTheCallersOwnPendingClaim: a first upload whose base
// write was indeterminate leaves the caller's own claim pending with no
// document. The retry goes back to that claim, not to another uploader's
// document of the name the caller may upload to.
func TestUploadByNameRetriesTheCallersOwnPendingClaim(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		f := newFixture(t)
		base := &ingestFailingBase{Explorer: f.base}
		w := newUploadWorldOver(t, f, base, store, false)
		base.fail = true
		if _, err := w.client.Ingest(w.as(uploadReader),
			uploadSource("p.md", "# P\n\nmine\n", "")); !explorer.IsIndeterminateCommit(err) {
			t.Fatalf("injected failure = %v", err)
		}
		// The pending claim has no document, so another reader's upload of
		// the name does not see it and starts its own.
		other := w.ingest(w.client, uploadReaderTwo, "p.md", "# P\n\nshared\n", "")
		if other.Document.ID != explorer.SourceDocumentID(w.ownURI(uploadReaderTwo, "p.md")) {
			t.Fatalf("reader two's upload went to %s", other.Document.ID)
		}
		own := w.ownURI(uploadReader, "p.md")
		if claim, ok, err := store.SourceClaim(context.Background(), own); err != nil || !ok ||
			!claim.Pending {
			t.Fatalf("own claim = %+v, %v (%v), want pending", claim, ok, err)
		}
		if _, documented := w.sourceURIs()[explorer.SourceDocumentID(own)]; documented {
			t.Fatal("the failed ingest wrote a document")
		}
		retry := w.ingest(w.client, uploadReader, "p.md", "# P\n\nmine\n", "")
		if retry.Document.ID != explorer.SourceDocumentID(own) {
			t.Fatalf("retry went to %s, want the caller's own pending document", retry.Document.ID)
		}
		w.requireCurrent(uploadReaderTwo, other, "after the retry")
	})
}

// TestUploadByNameKeepsTheCallersOwnRegistration: a registration in the
// caller's own namespace with no claim and no base document (a corpus
// restored from an older backup than its catalog) still holds the name for
// the caller.
func TestUploadByNameKeepsTheCallersOwnRegistration(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		f := newFixture(t)
		old := newUploadWorld(t, f, store, false)
		own := old.ownURI(uploadReader, "q.md")
		if _, err := f.base.Ingest(context.Background(),
			labelledSource(own, "registered before claims", "")); err != nil {
			t.Fatal(err)
		}
		if _, err := old.client.BackfillExistingDocumentsForDevelopment(
			old.as(uploadHolder), devbackfill.NewCapability()); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := store.SourceClaim(context.Background(), own); err != nil || ok {
			t.Fatalf("backfill created a claim (%v)", err)
		}
		restored, err := explorer.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = restored.Close() })
		w := newUploadWorldOver(t, f, restored, store, false)
		other := w.ingest(w.client, uploadReaderTwo, "q.md", "# Q\n\nshared\n", "")
		mine := w.ingest(w.client, uploadReader, "q.md", "# Q\n\nmine\n", "")
		if mine.Document.ID != explorer.SourceDocumentID(own) {
			t.Fatalf("upload went to %s, want the caller's own registration", mine.Document.ID)
		}
		w.requireCurrent(uploadReaderTwo, other, "after the upload")
	})
}
