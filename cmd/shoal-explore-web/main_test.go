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

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/explorercoord"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/transaction"
	"github.com/phrocker/shoal-oss/pkg/explorer/mcp"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/model"
)

func TestDocumentedWebWorkspaceStartServesMeta(t *testing.T) {
	data := t.TempDir()
	corpus, err := explorer.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := corpus.Ingest(context.Background(), explorer.Source{
		URI: "file:///web-demo.md", MediaType: explorer.MediaTypeMarkdown,
		Content: "# Web Demo\n\nThe web workspace serves Explorer metadata.\n",
	}); err != nil {
		t.Fatal(err)
	}
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	output := &lockedBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{
			"-data", data,
			"-listen", "127.0.0.1:0",
			"-dev-auth",
		}, output)
	}()

	baseURL := waitForListeningURL(t, output)
	body := getMeta(t, baseURL)
	var meta map[string]any
	if err := json.Unmarshal(body, &meta); err != nil {
		t.Fatalf("decode meta %s: %v", string(body), err)
	}
	for _, field := range []string{
		"max_page_size", "max_top_k", "max_depth", "max_fanout", "max_nodes",
	} {
		if _, ok := meta[field]; !ok {
			t.Fatalf("meta missing %q: %s", field, string(body))
		}
	}
	body = postJSON(t, baseURL+"/api/v1/documents", `{"page":{"limit":2}}`)
	var documents map[string]any
	if err := json.Unmarshal(body, &documents); err != nil {
		t.Fatalf("decode documents %s: %v", string(body), err)
	}
	snapshot, ok := documents["snapshot"].(map[string]any)
	if !ok {
		t.Fatalf("documents missing snapshot: %s", string(body))
	}
	for _, field := range []string{"id", "as_of", "frontier"} {
		if _, ok := snapshot[field]; !ok {
			t.Fatalf("snapshot missing %q: %s", field, string(body))
		}
	}
	// The document above was written straight to the corpus, so it had no
	// policy registration. -dev-auth on a loopback listener backfills it for
	// the development principal at startup (issue #284), so it is served.
	if got, ok := documents["documents"].([]any); !ok || len(got) != 1 {
		t.Fatalf("backfilled document was not served: %s", string(body))
	}

	workspaceID := createMCPWorkspace(t, baseURL)
	session := initializeMountedMCP(t, baseURL, workspaceID)
	postMountedMCP(
		t, baseURL, workspaceID, session,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		http.StatusAccepted,
	)
	mcpBody := postMountedMCP(
		t, baseURL, workspaceID, session,
		`{"jsonrpc":"2.0","id":"documents","method":"tools/call",`+
			`"params":{"name":"shoal.documents","arguments":{}}}`,
		http.StatusOK,
	)
	var mcpResponse struct {
		Result mcp.ToolResult `json:"result"`
	}
	if err := json.Unmarshal(mcpBody, &mcpResponse); err != nil {
		t.Fatalf("decode MCP response %s: %v", mcpBody, err)
	}
	if mcpResponse.Result.IsError {
		t.Fatalf("mounted MCP documents failed: %s",
			mcpResponse.Result.StructuredContent)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not shut down")
	}
	reopened, err := explorer.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	interactions, err := reopened.Interactions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(interactions) != 1 ||
		interactions[0].Operation != interaction.OperationToolCall {
		t.Fatalf("persisted MCP interactions = %+v", interactions)
	}
}

func createMCPWorkspace(t *testing.T, baseURL string) string {
	t.Helper()
	workspaceID := base64.RawURLEncoding.EncodeToString(
		[]byte("command-mcp-workspace"))
	mutationID := base64.RawURLEncoding.EncodeToString(
		[]byte("command-mcp-settings-create"))
	request, err := http.NewRequest(
		http.MethodPut,
		baseURL+"/api/v1/workspaces/"+workspaceID+"/settings",
		strings.NewReader(
			`{"expected_revision":0,"mutation_id":"`+
				mutationID+`","settings":{}}`,
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("workspace creation status = %d: %s",
			response.StatusCode, body)
	}
	return workspaceID
}

func initializeMountedMCP(
	t *testing.T, baseURL string, workspaceID string,
) string {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, baseURL+"/mcp",
		strings.NewReader(
			`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{`+
				`"protocolVersion":"`+mcp.ProtocolVersion+`","capabilities":{},`+
				`"clientInfo":{"name":"web-test","version":"1"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set(webapi.WorkspaceIDHeader, workspaceID)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("MCP initialize status = %d: %s", response.StatusCode, body)
	}
	session := response.Header.Get(mcp.SessionHeader)
	if session == "" {
		t.Fatal("MCP initialize omitted session")
	}
	return session
}

func postMountedMCP(
	t *testing.T,
	baseURL string,
	workspaceID string,
	session string,
	body string,
	wantStatus int,
) []byte {
	t.Helper()
	request, err := http.NewRequest(
		http.MethodPost, baseURL+"/mcp", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set(webapi.WorkspaceIDHeader, workspaceID)
	if session != "" {
		request.Header.Set(mcp.SessionHeader, session)
		request.Header.Set(mcp.ProtocolVersionHeader, mcp.ProtocolVersion)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	encoded, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != wantStatus {
		t.Fatalf("MCP status = %d, want %d: %s",
			response.StatusCode, wantStatus, encoded)
	}
	return encoded
}

func TestDefaultListenAddressIsDocumented(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", "docs", "explorer-demo-walkthrough.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(source, []byte("-listen 127.0.0.1:8080")) ||
		!bytes.Contains(source, []byte("The default listen address is also `127.0.0.1:8080`")) {
		t.Fatalf("walkthrough does not document the default listen address")
	}
}

func TestEmbeddedServiceStartsTransactionalRuntimeExclusively(t *testing.T) {
	data := commandTestDirectory(t)
	authority := auth.NewAuthority()
	config := serviceConfig{
		backend: "embedded", data: data, policyDir: commandTestDirectory(t),
		resolver: authority.Resolver(), clock: time.Now,
	}
	first, err := openService(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(data, explorercoord.DefaultCoordinationTable)); err != nil {
		first.close()
		t.Fatalf("coordination table was not created: %v", err)
	}
	if second, err := openService(context.Background(), config); err == nil {
		second.close()
		first.close()
		t.Fatal("second embedded service opened the same runtime directory")
	}
	decision, err := mintDevelopmentDecision(t)
	if err != nil {
		first.close()
		t.Fatal(err)
	}
	ingestContext, err := authority.Binder().Bind(context.Background(), decision)
	if err != nil {
		first.close()
		t.Fatal(err)
	}
	provider, ok := first.service.(webapi.IngestProvider)
	if !ok {
		first.close()
		t.Fatal("embedded service does not provide ingest")
	}
	if _, err := provider.Ingest(ingestContext, webapi.IngestRequest{
		Files: []webapi.UploadFile{{
			Name: "transactional.md", Content: []byte("# Transactional\n\nPublished.\n"),
		}},
	}); err != nil {
		first.close()
		t.Fatal(err)
	}
	first.close()
	runtime, err := explorercoord.Open(explorercoord.Config{
		Directory: data, Domain: workspacePublicationDomain,
		Owner: workspaceRuntimeOwner,
		Authority: transaction.Authority{
			Generation: 1, Fence: 1, Holder: workspaceRuntimeOwner,
			Mode:                coordination.WriterModeEmbeddedPrimary,
			RetentionGeneration: 1, HistoryFloor: 1,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	head, err := runtime.CurrentHead(context.Background())
	if err != nil || head.Frontier == 0 {
		_ = runtime.Close()
		t.Fatalf("command publication frontier = %#v, %v", head, err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openService(context.Background(), config)
	if err != nil {
		t.Fatalf("reopen after exclusive runtime close: %v", err)
	}
	reopened.close()
}

func TestAuthorizedClientUsesRuntimeInteractionDependencies(t *testing.T) {
	embedded, err := explorercoord.OpenExplorer(explorercoord.Config{
		Directory: commandTestDirectory(t),
		Domain:    workspacePublicationDomain,
		Owner:     workspaceRuntimeOwner,
		Authority: transaction.Authority{
			Generation: 1, Fence: 1, Holder: workspaceRuntimeOwner,
			Mode:                coordination.WriterModeEmbeddedPrimary,
			RetentionGeneration: 1, HistoryFloor: 1,
		},
	}, explorer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = embedded.Close() })
	store, err := authorized.OpenDurablePolicyStore(commandTestDirectory(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	authority := auth.NewAuthority()
	client, err := authorizedClient(
		embedded.Explorer, store, authority.Resolver(),
		fixedGenerationReader{
			domain:     workspaceAuthorizationDomain,
			generation: workspacePolicyGeneration,
		},
		time.Now,
		authorized.MosaicBudget{},
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := mintDevelopmentDecision(t)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := authority.Binder().Bind(context.Background(), decision)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.EnsureInteractionSink(ctx); err != nil {
		t.Fatalf("runtime interaction sink is not wired: %v", err)
	}
	sessions, err := client.Interactions(ctx)
	if err != nil {
		t.Fatalf("runtime interaction reader is not wired: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("new runtime already contains %d interactions", len(sessions))
	}
}

func commandTestDirectory(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp(".", ".transaction-runtime-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return directory
}

func TestEmbeddingFlagsRequireAndForwardRemoteDimensions(t *testing.T) {
	for _, provider := range []string{"ollama", "openai"} {
		t.Run(provider, func(t *testing.T) {
			_, err := (embeddingConfig{
				provider: provider, model: "embed-model",
				baseURL: "http://127.0.0.1:11434",
			}).embedder()
			if err == nil {
				t.Fatal("missing embedding dimensions succeeded")
			}

			embedder, err := (embeddingConfig{
				provider: provider, model: "embed-model",
				baseURL: "http://127.0.0.1:11434", dimensions: 8,
			}).embedder()
			if err != nil {
				t.Fatal(err)
			}
			space, ok := embedder.(model.EmbeddingSpaceIdentityProvider)
			if !ok {
				t.Fatal("embedder does not expose embedding space identity")
			}
			identity, err := space.EmbeddingSpaceIdentity()
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(identity, "8") {
				t.Fatalf("identity did not include dimensions: %q", identity)
			}
		})
	}
}

type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func waitForListeningURL(t *testing.T, output *lockedBuffer) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		line := output.String()
		if index := strings.Index(line, "http://"); index >= 0 {
			// Stop at the first whitespace rather than the end of the buffer:
			// the process can report a second listener (the health surface) on
			// a following line, and taking the remainder would return both
			// addresses joined by a newline.
			return strings.TrimSpace(strings.Fields(line[index:])[0])
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server did not report listen address: %q", output.String())
	return ""
}

func getMeta(t *testing.T, baseURL string) []byte {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	client := http.Client{Timeout: 500 * time.Millisecond}
	for time.Now().Before(deadline) {
		response, err := client.Get(baseURL + "/api/v1/meta")
		if err == nil {
			body, readErr := io.ReadAll(response.Body)
			closeErr := response.Body.Close()
			if response.StatusCode == http.StatusOK && readErr == nil && closeErr == nil {
				return body
			}
			lastErr = readErr
			if closeErr != nil {
				lastErr = closeErr
			}
			if response.StatusCode != http.StatusOK {
				lastErr = errStatus(response.Status)
			}
		} else {
			lastErr = err
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("GET /api/v1/meta failed: %v", lastErr)
	return nil
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
		t.Fatalf("POST %s returned %s: %s", url, response.Status, string(data))
	}
	return data
}

type errStatus string

func (e errStatus) Error() string { return string(e) }

func TestEmbeddingProvidersCoverLocalAndHostedOptions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config embeddingConfig
	}{
		{
			name: "lexical",
			config: embeddingConfig{
				provider: "lexical", model: "embed-model", dimensions: 8,
			},
		},
		{
			name: "voyage",
			config: embeddingConfig{
				provider: "voyage", model: "embed-model", dimensions: 8,
				apiKeyEnv: "VOYAGE_API_KEY",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			missing := tc.config
			missing.dimensions = 0
			if _, err := missing.embedder(); err == nil {
				t.Fatal("missing embedding dimensions succeeded")
			}

			embedder, err := tc.config.embedder()
			if err != nil {
				t.Fatal(err)
			}
			space, ok := embedder.(model.EmbeddingSpaceIdentityProvider)
			if !ok {
				t.Fatal("embedder does not expose embedding space identity")
			}
			identity, err := space.EmbeddingSpaceIdentity()
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(identity, tc.name) {
				t.Fatalf("identity did not name the provider: %q", identity)
			}
			if strings.Contains(identity, "VOYAGE_API_KEY") {
				t.Fatalf("identity leaked credential configuration: %q", identity)
			}
		})
	}
}
