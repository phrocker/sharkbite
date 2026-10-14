// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package main

// End-to-end tests for the operator label grant file (#570, PR3). As
// docs/approval.md's testing note asks, every request is a signed token sent
// over real HTTP to the authenticated handler, authenticated by the real OIDC
// authenticator and bound by the real binder, against the real embedded
// corpus and its authorized client.
//
// One seam is labelled: no shipped upload route carries a visibility label
// (the browser upload and the MCP ingest tool both build their metadata
// themselves), so labelledUploads maps an upload's file name to the label it
// is ingested under and calls the real authorized client with the request's
// own bound context. Everything that decides whether the label is allowed —
// the token, the grant file, the decision, the access rule — is the shipped
// path.

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
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// labelOtherSource is a second configured source the grant file may name.
// The server never ingests into it; it exists so a grant on (other, secret)
// can be shown not to open (workspace, secret).
var labelOtherSource = []byte("shoal-explore-web/other")

const (
	labelGroupSecret      = "secret-readers"
	labelGroupOtherSecret = "other-secret-readers"
)

// labelGrantsDocument is the test grant file: secret on the workspace source
// for one group, secret on the other source for another, and a label for the
// approver role value, so an approver token matches a grant.
func labelGrantsDocument(issuer string) map[string]any {
	return map[string]any{
		"version":    labelGrantsVersion,
		"issuer":     issuer,
		"claim":      []string{"groups"},
		"max_values": 16,
		"grants": map[string]any{
			labelGroupSecret: []map[string]string{
				{"source": string(workspaceSourceID), "label": "secret"},
			},
			labelGroupOtherSecret: []map[string]string{
				{"source": string(labelOtherSource), "label": "secret"},
			},
			testApproverValue: []map[string]string{
				{"source": string(workspaceSourceID), "label": "secret"},
			},
		},
	}
}

func writeLabelGrants(t *testing.T, document any) string {
	t.Helper()
	raw, ok := document.([]byte)
	if !ok {
		var err error
		raw, err = json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), "label-grants.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// labelledUploads is the one seam: an upload named in labels is ingested
// with that visibility label through the real authorized client, under the
// decision the transport bound for this request.
type labelledUploads struct {
	*webapi.EmbeddedService
	client *authorized.Client
	labels map[string]string
}

func (s labelledUploads) Ingest(
	ctx context.Context, request webapi.IngestRequest,
) (webapi.IngestResponse, error) {
	var response webapi.IngestResponse
	for _, file := range request.Files {
		label, labelled := s.labels[file.Name]
		if !labelled {
			return s.EmbeddedService.Ingest(ctx, request)
		}
		result, err := s.client.Ingest(ctx, explorer.Source{
			URI:       "upload://workspace/" + file.Name,
			Title:     file.Name,
			MediaType: explorer.MediaTypeMarkdown,
			Content:   string(file.Content),
			Metadata:  shoal.Metadata{interaction.PropertyVisibility: label},
		})
		if err != nil {
			return webapi.IngestResponse{}, err
		}
		response.Files = append(response.Files, webapi.IngestFileResult{
			Name: file.Name, Document: result.Document, Revision: result.Revision,
		})
	}
	return response, nil
}

// labelWorld is a running workspace behind the real OIDC authenticator.
type labelWorld struct {
	t      *testing.T
	issuer *fakeOIDCIssuer
	authn  *oidcAuthenticator
	server *httptest.Server

	mu        sync.Mutex
	last      auth.Decision
	revisions map[shoal.ID]shoal.ID
}

func newLabelWorld(t *testing.T, edit func(*oidcConfig)) *labelWorld {
	t.Helper()
	w := &labelWorld{
		t: t, issuer: newFakeOIDCIssuer(t), revisions: map[shoal.ID]shoal.ID{},
	}
	config := approverTestConfig(t, w.issuer, time.Now,
		approverMappingDocument(w.issuer.server.URL))
	config.labelGrantsFile = writeLabelGrants(t,
		labelGrantsDocument(w.issuer.server.URL))
	config.labelGrantSources = [][]byte{workspaceSourceID, labelOtherSource}
	if edit != nil {
		edit(&config)
	}
	w.authn = newTestOIDCAuthenticator(t, config)

	authority := auth.NewAuthority()
	root := t.TempDir()
	opened, err := openService(context.Background(), serviceConfig{
		backend:   "embedded",
		data:      filepath.Join(root, "corpus"),
		policyDir: filepath.Join(root, "policy"),
		resolver:  authority.Resolver(),
		clock:     time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(opened.close)
	embedded, ok := opened.service.(*webapi.EmbeddedService)
	if !ok {
		t.Fatalf("the embedded backend serves %T", opened.service)
	}
	service := labelledUploads{
		EmbeddedService: embedded, client: opened.client,
		labels: map[string]string{
			"secret.md": "secret", "secret-2.md": "secret",
			"secret-3.md": "secret", "pii.md": "pii",
		},
	}
	// The recorder keeps the decision the real authenticator minted, so a
	// test can say what a token was granted; it changes nothing.
	recorder := webapi.AuthenticatorFunc(func(request *http.Request) (auth.Decision, error) {
		decision, err := w.authn.Authenticate(request)
		w.mu.Lock()
		w.last = decision
		w.mu.Unlock()
		return decision, err
	})
	var handler http.Handler
	w.server = httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			handler.ServeHTTP(writer, request)
		}))
	t.Cleanup(w.server.Close)
	authenticated, err := webapi.NewAuthenticatedHandler(
		service, recorder, authority.Binder(),
		strings.TrimPrefix(w.server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	handler = authenticated
	return w
}

// token is a workspace token with the given role values and groups.
func (w *labelWorld) token(subject string, access []string, groups []string) string {
	claims := w.issuer.defaultClaims(time.Now())
	claims["sub"] = subject
	claims["access"] = access
	if groups != nil {
		claims["groups"] = groups
	}
	return w.issuer.signRS256(w.t, testKID, claims)
}

func (w *labelWorld) approverToken(subject string, groups []string) string {
	claims := approverClaims(w.issuer, time.Now(), subject)
	claims["groups"] = groups
	return w.issuer.signRS256(w.t, testKID, claims)
}

func (w *labelWorld) lastDecision() auth.Decision {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.last
}

func (w *labelWorld) do(token string, request *http.Request) (int, []byte) {
	w.t.Helper()
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-Shoal-Workspace-Request", "1")
	response, err := w.server.Client().Do(request)
	if err != nil {
		w.t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		w.t.Fatal(err)
	}
	return response.StatusCode, body
}

// upload posts one file to the ingest route and returns the status and the
// document ID on success.
func (w *labelWorld) upload(token, name, content string) (int, shoal.ID) {
	w.t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
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
	request, err := http.NewRequest(
		http.MethodPost, w.server.URL+"/api/v1/ingest", &body)
	if err != nil {
		w.t.Fatal(err)
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	status, raw := w.do(token, request)
	if status != http.StatusOK {
		return status, ""
	}
	var response webapi.IngestResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		w.t.Fatalf("ingest response %s: %v", raw, err)
	}
	if len(response.Files) != 1 {
		w.t.Fatalf("ingest response %s", raw)
	}
	w.mu.Lock()
	w.revisions[response.Files[0].Document.ID] = response.Files[0].Revision.ID
	w.mu.Unlock()
	return status, response.Files[0].Document.ID
}

func (w *labelWorld) post(token, path string, payload any) (int, []byte) {
	w.t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		w.t.Fatal(err)
	}
	request, err := http.NewRequest(
		http.MethodPost, w.server.URL+path, bytes.NewReader(raw))
	if err != nil {
		w.t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	return w.do(token, request)
}

// documents lists what the token may see, by document ID.
func (w *labelWorld) documents(token string) (int, map[shoal.ID]bool) {
	w.t.Helper()
	status, raw := w.post(token, "/api/v1/documents",
		map[string]any{"page": map[string]any{"limit": 100}})
	if status != http.StatusOK {
		return status, nil
	}
	var response webapi.DocumentsResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		w.t.Fatalf("documents response %s: %v", raw, err)
	}
	seen := make(map[shoal.ID]bool, len(response.Documents))
	for _, document := range response.Documents {
		seen[document.Document.ID] = true
	}
	return status, seen
}

// document reads one document by ID at the current snapshot.
func (w *labelWorld) document(token string, id shoal.ID) int {
	w.t.Helper()
	w.mu.Lock()
	revision := w.revisions[id]
	w.mu.Unlock()
	status, raw := w.post(token, "/api/v1/document",
		webapi.DocumentRequest{DocumentID: id, RevisionID: revision})
	if status != http.StatusOK && status != http.StatusNotFound &&
		status != http.StatusUnauthorized {
		w.t.Fatalf("document read = %d %s", status, raw)
	}
	return status
}

func hasLabelPolicy(ids [][]byte) bool {
	for _, id := range ids {
		if auth.IsLabelPolicyID(id) {
			return true
		}
	}
	return false
}

func TestLabelGrantsOverOIDC(t *testing.T) {
	w := newLabelWorld(t, nil)
	writerSecret := w.token("writer-secret",
		[]string{"writer"}, []string{"unrelated", labelGroupSecret})
	writerPlain := w.token("writer-plain", []string{"writer"}, nil)
	readerSecret := w.token("reader-secret",
		[]string{"reader"}, []string{labelGroupSecret})
	readerPlain := w.token("reader-plain", []string{"reader"}, []string{"unrelated"})
	writerOther := w.token("writer-other",
		[]string{"writer"}, []string{labelGroupOtherSecret})
	readerOther := w.token("reader-other",
		[]string{"reader"}, []string{labelGroupOtherSecret})

	// A token mapped to the label ingests a labelled document, and an
	// unlabelled control.
	status, secret := w.upload(writerSecret, "secret.md", "# Secret\n\nthe launch codes\n")
	if status != http.StatusOK {
		t.Fatalf("a writer holding (workspace, secret) could not ingest it: %d", status)
	}
	secretPolicy, err := authorized.LabelPolicyID(workspaceSourceID, "secret")
	if err != nil {
		t.Fatal(err)
	}
	minted := w.lastDecision().PermittedPolicyIDs()
	if len(minted) != 2 || !containsBytes(minted, secretPolicy) ||
		!containsBytes(minted, workspaceGrantPolicyID) {
		t.Fatalf("the writer's decision holds %q", minted)
	}
	status, plain := w.upload(writerSecret, "plain.md", "# Plain\n\nthe lunch menu\n")
	if status != http.StatusOK {
		t.Fatalf("an unlabelled upload = %d", status)
	}

	t.Run("a holder reads it", func(t *testing.T) {
		for name, token := range map[string]string{
			"the ingester": writerSecret, "a reader holding the label": readerSecret,
		} {
			status, seen := w.documents(token)
			if status != http.StatusOK || !seen[secret] || !seen[plain] {
				t.Fatalf("%s lists %d %v", name, status, seen)
			}
			if status := w.document(token, secret); status != http.StatusOK {
				t.Fatalf("%s reads the labelled document: %d", name, status)
			}
		}
	})

	t.Run("a token without the label cannot read it", func(t *testing.T) {
		for name, token := range map[string]string{
			"a writer": writerPlain, "a reader": readerPlain,
		} {
			status, seen := w.documents(token)
			if status != http.StatusOK || seen[secret] || !seen[plain] {
				t.Fatalf("%s lists %d %v", name, status, seen)
			}
			if status := w.document(token, secret); status == http.StatusOK {
				t.Fatalf("%s read the labelled document", name)
			}
			if status := w.document(token, plain); status != http.StatusOK {
				t.Fatalf("%s cannot read the unlabelled control: %d", name, status)
			}
			if hasLabelPolicy(w.lastDecision().PermittedPolicyIDs()) {
				t.Fatalf("%s was minted a label policy", name)
			}
		}
	})

	t.Run("a token without the label cannot ingest it", func(t *testing.T) {
		if status, _ := w.upload(writerPlain, "secret-2.md", "# Two\n\nmore codes\n"); status == http.StatusOK {
			t.Fatal("a writer without the label ingested it")
		}
		// An unlabelled upload still works for the same token, so the
		// refusal is the label's.
		if status, _ := w.upload(writerPlain, "plain-2.md", "# Plain two\n\nsoup\n"); status != http.StatusOK {
			t.Fatalf("the same writer's unlabelled upload = %d", status)
		}
		// A label nobody is granted is ingestible by nobody, the
		// best-granted writer included.
		if status, _ := w.upload(writerSecret, "pii.md", "# PII\n\nnames\n"); status == http.StatusOK {
			t.Fatal("an ungranted label was ingested")
		}
	})

	t.Run("a grant for another source does not open this one", func(t *testing.T) {
		status, seen := w.documents(readerOther)
		if status != http.StatusOK || seen[secret] || !seen[plain] {
			t.Fatalf("a reader holding (other, secret) lists %d %v", status, seen)
		}
		if status := w.document(readerOther, secret); status == http.StatusOK {
			t.Fatal("(other, secret) opened (workspace, secret)")
		}
		otherPolicy, err := authorized.LabelPolicyID(labelOtherSource, "secret")
		if err != nil {
			t.Fatal(err)
		}
		if held := w.lastDecision().PermittedPolicyIDs(); !containsBytes(held, otherPolicy) ||
			containsBytes(held, secretPolicy) {
			t.Fatalf("the other-source reader holds %q", held)
		}
		if status, _ := w.upload(writerOther, "secret-3.md", "# Three\n\ncodes\n"); status == http.StatusOK {
			t.Fatal("(other, secret) let a writer ingest (workspace, secret)")
		}
	})

	t.Run("a label confers no operation", func(t *testing.T) {
		// A reader holding the label still cannot ingest, even unlabelled.
		if status, _ := w.upload(readerSecret, "plain-3.md", "# Plain three\n\nbread\n"); status == http.StatusOK {
			t.Fatal("a label let a reader ingest")
		}
		decision := w.lastDecision()
		if !sameOperations(decision.AllowedOperations(), oidcReaderOperations) {
			t.Fatalf("a labelled reader's operations = %v", decision.AllowedOperations())
		}
		// A token whose only matching value is a label grant is unmapped:
		// the label does not make it a principal.
		labelOnly := w.token("label-only", []string{"nobody"}, []string{labelGroupSecret})
		if status, _ := w.documents(labelOnly); status != http.StatusUnauthorized {
			t.Fatalf("a token with only a label grant = %d, want 401", status)
		}
	})

	t.Run("an approver token gets no labels", func(t *testing.T) {
		approver := w.approverToken("approver-1", []string{labelGroupSecret})
		if status, _ := w.documents(approver); status == http.StatusOK {
			t.Fatal("an approver token listed documents")
		}
		decision := w.lastDecision()
		if got := decision.AllowedOperations(); len(got) != 1 ||
			got[0] != auth.OperationActionApprove {
			t.Fatalf("the approver token was not minted as an approver: %v", got)
		}
		if hasLabelPolicy(decision.PermittedPolicyIDs()) {
			t.Fatalf("an approver token holds %q", decision.PermittedPolicyIDs())
		}
		if status := w.document(approver, secret); status == http.StatusOK {
			t.Fatal("an approver token read the labelled document")
		}
	})

	t.Run("the claim is matched byte for byte and bounded", func(t *testing.T) {
		for name, groups := range map[string]any{
			"case folded": []string{"Secret-Readers"},
			"padded":      []string{" " + labelGroupSecret},
			"an object":   map[string]any{},
		} {
			claims := w.issuer.defaultClaims(time.Now())
			claims["sub"] = "probe"
			claims["access"] = []string{"reader"}
			claims["groups"] = groups
			token := w.issuer.signRS256(t, testKID, claims)
			status, seen := w.documents(token)
			if name == "an object" {
				// An object where a string or array belongs refuses the token.
				if status != http.StatusUnauthorized {
					t.Fatalf("%s = %d, want 401", name, status)
				}
				continue
			}
			if status != http.StatusOK || seen[secret] {
				t.Fatalf("%s lists %d %v", name, status, seen)
			}
		}
		many := make([]string, 17)
		for i := range many {
			many[i] = labelGroupSecret
		}
		claims := w.issuer.defaultClaims(time.Now())
		claims["access"] = []string{"reader"}
		claims["groups"] = many
		if status, _ := w.documents(w.issuer.signRS256(t, testKID, claims)); status != http.StatusUnauthorized {
			t.Fatalf("a groups claim over max_values = %d, want 401", status)
		}
	})
}

// TestLabelGrantsLegacyUnmappedTokensGetNothing: the legacy Entra mode mints
// an unmapped token a list-only decision with no source. A label grant
// matching it must not ride along.
func TestLabelGrantsLegacyUnmappedTokensGetNothing(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	config := issuer.testConfig(time.Now)
	config.allowUnmappedAuthorization = true
	config.labelGrantsFile = writeLabelGrants(t, labelGrantsDocument(issuer.server.URL))
	config.labelGrantSources = [][]byte{workspaceSourceID, labelOtherSource}
	authenticator := newTestOIDCAuthenticator(t, config)
	claims := issuer.defaultClaims(time.Now())
	claims["access"] = []string{"nobody"}
	claims["groups"] = []string{labelGroupSecret}
	decision, err := authenticator.Authenticate(
		bearerRequest(issuer.signRS256(t, testKID, claims)))
	if err != nil {
		t.Fatal(err)
	}
	if len(decision.PermittedSourceIDs()) != 0 || len(decision.PermittedPolicyIDs()) != 0 {
		t.Fatalf("an unmapped legacy token holds sources %q policies %q",
			decision.PermittedSourceIDs(), decision.PermittedPolicyIDs())
	}
}

func containsBytes(values [][]byte, want []byte) bool {
	for _, value := range values {
		if bytes.Equal(value, want) {
			return true
		}
	}
	return false
}

func sameOperations(got, want []auth.Operation) bool {
	if len(got) != len(want) {
		return false
	}
	set := make(map[auth.Operation]bool, len(want))
	for _, operation := range want {
		set[operation] = true
	}
	for _, operation := range got {
		if !set[operation] {
			return false
		}
	}
	return true
}
