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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/inference/harness"
	"github.com/phrocker/shoal-oss/pkg/model"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestIngestListQueryWorkflow(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "guide.md")
	if err := os.WriteFile(source, []byte(
		"# Guide\n\nUse exponential backoff for retries.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(dir, "data")
	var output bytes.Buffer
	if err := run(context.Background(), []string{
		"ingest", "-data", data, "-file", source,
	}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"Disposition": "applied"`) {
		t.Fatalf("ingest output = %s", output.String())
	}
	output.Reset()
	if err := run(context.Background(), []string{
		"query", "-data", data, "-text", "exponential backoff",
	}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Use exponential backoff for retries.") {
		t.Fatalf("query output = %s", output.String())
	}
}

func TestVectorIngestQueryWorkflowUsesConfiguredEmbedder(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "vector.txt")
	const quote = "Configured fake embeddings make vector retrieval usable."
	if err := os.WriteFile(source, []byte(quote), 0o600); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(dir, "data")
	var output bytes.Buffer
	if err := run(context.Background(), []string{
		"ingest", "-data", data, "-file", source,
		"-embedding-provider", "fake",
		"-embedding-model", "cli-vector",
		"-embedding-dimensions", "8",
	}, &output); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := run(context.Background(), []string{
		"query", "-data", data, "-text", quote, "-modes", "vector",
		"-embedding-provider", "fake",
		"-embedding-model", "cli-vector",
		"-embedding-dimensions", "8",
	}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), quote) {
		t.Fatalf("vector query output = %s", output.String())
	}
}

func TestEmbeddingConfigRequiresAndForwardsRemoteDimensions(t *testing.T) {
	for _, providerName := range []string{"ollama", "openai"} {
		t.Run(providerName, func(t *testing.T) {
			modelName := "embed-model"
			baseURL := "http://127.0.0.1:11434"
			apiKeyEnv := "OPENAI_API_KEY"
			zero := 0
			missing := embeddingFlags{
				provider: &providerName, model: &modelName,
				baseURL: &baseURL, apiKeyEnv: &apiKeyEnv, dimensions: &zero,
			}
			if _, err := missing.embedder(); err == nil {
				t.Fatal("missing embedding dimensions succeeded")
			}

			dimensions := 8
			configured := embeddingFlags{
				provider: &providerName, model: &modelName,
				baseURL: &baseURL, apiKeyEnv: &apiKeyEnv, dimensions: &dimensions,
			}
			embedder, err := configured.embedder()
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

func TestAskWorkflowUsesFakeProviderDeterministically(t *testing.T) {
	data := ingestAskFixture(t)
	args := []string{
		"ask", "-data", data,
		"-provider", "fake",
		"-question", "What keeps grounded answers tied to exact quotes?",
	}
	var first bytes.Buffer
	if err := run(context.Background(), args, &first); err != nil {
		t.Fatal(err)
	}
	var second bytes.Buffer
	if err := run(context.Background(), args, &second); err != nil {
		t.Fatal(err)
	}
	if first.String() != second.String() {
		t.Fatalf("ask output is not deterministic:\nfirst=%s\nsecond=%s", first.String(), second.String())
	}
	if strings.Contains(first.String(), "DetailedTrace") {
		t.Fatalf("default ask output dumped detailed trace: %s", first.String())
	}
	result := decodeJSON[askOutput](t, first.Bytes())
	if result.Answer != "grounded" || result.StopReason != harness.StopReasonStop ||
		len(result.Claims) != 1 || len(result.Evidence) == 0 {
		t.Fatalf("ask result = %+v", result)
	}
	if result.Provenance.Provider != "fake" ||
		result.Provenance.PromptTemplate == "" ||
		result.Trace.Budgets.MaxSteps == 0 ||
		result.Execution.AuthorizationEnforced {
		t.Fatalf("missing provenance, budgets, or local execution disclosure: %+v", result)
	}
	referenced := make(map[string]bool)
	for _, id := range result.Claims[0].EvidenceIDs {
		referenced[string(id)] = true
	}
	foundReference := false
	foundQuote := false
	for _, evidence := range result.Evidence {
		if referenced[string(evidence.ID)] {
			foundReference = true
		}
		if evidence.Citation != nil &&
			strings.Contains(evidence.Quote, "Entity Alpha keeps grounded answers tied to exact quotes.") {
			foundQuote = true
			if evidence.ByteRange == nil ||
				evidence.ByteRange.End.Offset-evidence.ByteRange.Start.Offset != int64(len(evidence.Quote)) {
				t.Fatalf("bad byte range for evidence: %+v", evidence)
			}
		}
	}
	if !foundReference || !foundQuote {
		t.Fatalf("missing referenced evidence or exact quote: %+v", result.Evidence)
	}
	if result.Trace.Iterations == 0 || result.Trace.Evidence == 0 ||
		result.Trace.StopReason != harness.StopReasonStop {
		t.Fatalf("trace summary = %+v", result.Trace)
	}
	for _, tool := range result.Trace.Tools {
		if tool == string(harness.ActionStop) {
			t.Fatalf("stop was reported as a tool: %+v", result.Trace.Tools)
		}
	}
}

func TestAskMarkdownAndNoEvidenceOutput(t *testing.T) {
	data := ingestAskFixture(t)
	var markdown bytes.Buffer
	if err := run(context.Background(), []string{
		"ask", "-data", data,
		"-question", "What keeps grounded answers tied to exact quotes?",
		"-provider", "fake",
		"-format", "markdown",
		"-trace",
	}, &markdown); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"# Answer",
		"Entity Alpha keeps grounded answers tied to exact quotes.",
		"authorization enforced: `false`",
		"budget limits:",
		"### Detailed trace",
	} {
		if !strings.Contains(markdown.String(), want) {
			t.Fatalf("markdown missing %q:\n%s", want, markdown.String())
		}
	}

	var output bytes.Buffer
	if err := run(context.Background(), []string{
		"ask", "-data", data,
		"-question", "unmatched zephyr token",
		"-provider", "fake",
	}, &output); err != nil {
		t.Fatalf("no-evidence ask failed: %v\n%s", err, output.String())
	}
	result := decodeJSON[askOutput](t, output.Bytes())
	if len(result.Claims) != 0 || len(result.Issues) != 1 ||
		result.Issues[0].Kind != "unresolved" ||
		result.StopReason != harness.StopReasonStop {
		t.Fatalf("no-evidence result = %+v", result)
	}
}

func TestAskMarkdownCodePadsBacktickBoundaries(t *testing.T) {
	if rendered := markdownCode(""); rendered != "" {
		t.Fatalf("empty markdown code span = %q", rendered)
	}
	rendered := markdownCode("`model`")
	if rendered != "`` `model` ``" {
		t.Fatalf("markdown code span = %q", rendered)
	}
	rendered = markdownCode("line one\r\n\r<script>alert(1)</script>")
	if rendered != "`line one  <script>alert(1)</script>`" {
		t.Fatalf("multiline markdown code span = %q", rendered)
	}
}

func TestAskMarkdownEscapesOnceAndUsesCodeSpans(t *testing.T) {
	const (
		answer = "Résumé_budget -> <script>alert(\"x\")</script> & [link](javascript:alert(1)) *bold*\n" +
			"# heading\n- item\n1. item\n> quote\nsafe + plus!\nordinary text\n===\nanother line\n--\n" +
			"#hashtag\n---version\n-- -\n###### heading\n####### heading\n" +
			"    *space* <script>alert(2)</script>\n\t# tabbed & [link](javascript:alert(2))"
		errorText = "budget_exhausted ->\n\n<img src=x onerror=alert(1)>"
	)
	response := askOutput{
		Answer: answer,
		Execution: askExecution{
			Authorization: "role_admin -> tenant_日本",
		},
		Claims: []askClaim{{
			Subject:    askID("subject"),
			Predicate:  askID("predicate"),
			Object:     askValue{Value: "budget_exhausted -> <b>unsafe</b>"},
			Confidence: 0.5,
			EvidenceIDs: []askID{
				"evidence_α",
				"evidence_[β]",
			},
		}},
		Issues: []askIssue{{
			Kind:   inference.IssueUnresolved,
			Input:  "input_*_[x]",
			Reason: "<script>alert(1)</script> budget_exhausted ->",
		}},
		Evidence: []askEvidence{{
			ID:   askID("evidence_α"),
			Kind: inference.AnchorGraph,
			Path: &askPath{
				Nodes: []askNode{{ID: askID("node_α")}, {ID: askID("node_[β]")}},
				Edges: []askEdge{{ID: askID("edge_一")}, {ID: askID("edge_*_二")}},
			},
		}},
		DetailedTrace: &askDetailedTrace{
			Iterations: []askIterationTrace{{
				Index:       1,
				Failure:     errorText,
				EvidenceIDs: []askID{"evidence_α", "evidence_[β]"},
			}},
			Failures: []harness.FailureTrace{{
				Iteration: 1,
				Operation: "tool_call",
				Error:     errorText,
			}},
		},
	}

	var output bytes.Buffer
	if err := writeAskMarkdown(&output, response); err != nil {
		t.Fatal(err)
	}
	rendered := output.String()
	indent := strings.Repeat("\u00a0", 4)
	for _, want := range []string{
		"Résumé\\_budget -> \\<script>alert(\"x\")\\</script> \\& \\[link\\](javascript:alert(1)) \\*bold\\*",
		"\\# heading\n\\- item\n1\\. item\n\\> quote\nsafe + plus!\nordinary text\n\\===\nanother line\n\\--\n" +
			"#hashtag\n---version\n\\-- -\n\\###### heading\n####### heading\n" +
			indent + "\\*space\\* \\<script>alert(2)\\</script>\n" +
			indent + "\\# tabbed \\& \\[link\\](javascript:alert(2))",
		"- authorization: `role_admin -> tenant_日本`",
		"- `subject` `predicate` `budget_exhausted -> <b>unsafe</b>` (confidence 0.5; evidence `evidence_α, evidence_[β]`)",
		"- `unresolved`: `input_*_[x]` (`<script>alert(1)</script> budget_exhausted ->`)",
		"- graph path nodes: `node_α -> node_[β]`",
		"- graph path edges: `edge_一 -> edge_*_二`",
		"evidence `evidence_α, evidence_[β]`, failure `budget_exhausted ->  <img src=x onerror=alert(1)>`",
		"- iteration 1 `tool_call`: `budget_exhausted ->  <img src=x onerror=alert(1)>`",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("markdown missing %q:\n%s", want, rendered)
		}
	}
	for _, unwanted := range []string{
		"&lt;",
		"&gt;",
		"&amp;",
		"budget\\_exhausted",
		"\\-&gt;",
		"\n<script>",
		"\n\n<img src=x onerror=alert(1)>",
	} {
		if strings.Contains(rendered, unwanted) {
			t.Errorf("markdown contains unsafe or double-escaped %q:\n%s", unwanted, rendered)
		}
	}
}

func TestAskBudgetExhaustionReportsStopReason(t *testing.T) {
	data := ingestAskFixture(t)
	var output bytes.Buffer
	err := run(context.Background(), []string{
		"ask", "-data", data,
		"-question", "What keeps grounded answers tied to exact quotes?",
		"-provider", "fake",
		"-max-input-tokens", "1",
		"-trace",
	}, &output)
	if !errors.Is(err, harness.ErrBudgetExhausted) {
		t.Fatalf("error = %v\noutput = %s", err, output.String())
	}
	result := decodeJSON[askOutput](t, output.Bytes())
	if result.StopReason != harness.StopReasonBudgetExhausted ||
		result.Trace.StopReason != harness.StopReasonBudgetExhausted ||
		result.DetailedTrace == nil ||
		result.Answer != "No grounded answer could be produced from the available evidence." ||
		len(result.Issues) != 1 ||
		result.Issues[0].Kind != inference.IssueUnresolved ||
		!strings.Contains(result.Issues[0].Reason, string(harness.StopReasonBudgetExhausted)) {
		t.Fatalf("budget result = %+v", result)
	}
}

func TestAskInvalidBudgetsFailBeforeNoEvidenceShortcut(t *testing.T) {
	data := ingestAskFixture(t)
	var output bytes.Buffer
	err := run(context.Background(), []string{
		"ask", "-data", data,
		"-question", "unmatched zephyr token",
		"-provider", "fake",
		"-max-steps", "-1",
	}, &output)
	if !errors.Is(err, harness.ErrInvalid) {
		t.Fatalf("error = %v\noutput = %s", err, output.String())
	}
	if output.Len() != 0 {
		t.Fatalf("invalid budgets should not emit no-evidence output: %s", output.String())
	}
}

func TestAskInvalidFlagAndProviderErrors(t *testing.T) {
	tests := [][]string{
		{"ask", "-question", "q", "extra"},
		{"ask", "-provider", "unknown", "-question", "q"},
		{"ask", "-provider", "openai-compatible", "-question", "q"},
		{"ask", "-question", "q", "-modes", "nope"},
		{"ask", "-question", "q", "-format", "xml"},
		{"ask", "-question", "q", "-top", "0"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var output bytes.Buffer
			if err := run(context.Background(), args, &output); err == nil {
				t.Fatalf("expected error for %v; output=%s", args, output.String())
			}
		})
	}
}

func TestAskDoesNotLeakOpenAICompatibleSecret(t *testing.T) {
	data := ingestAskFixture(t)
	const secret = "test-secret-must-never-appear"
	t.Setenv("SHOAL_TEST_OPENAI_KEY", secret)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+secret {
			t.Fatalf("authorization header = %q", got)
		}
		http.Error(w, "server echoed "+secret, http.StatusUnauthorized)
	}))
	defer server.Close()
	previousClient := askOpenAIHTTPClient
	askOpenAIHTTPClient = server.Client()
	defer func() { askOpenAIHTTPClient = previousClient }()

	var output bytes.Buffer
	err := run(context.Background(), []string{
		"ask", "-data", data,
		"-question", "What keeps grounded answers tied to exact quotes?",
		"-provider", "openai-compatible",
		"-api-base-url", server.URL,
		"-model", "test-model",
		"-api-key-env", "SHOAL_TEST_OPENAI_KEY",
	}, &output)
	if err == nil {
		t.Fatal("expected provider failure")
	}
	if !strings.Contains(err.Error(), "provider authentication failed") {
		t.Fatalf("error did not classify authentication failure: %v", err)
	}
	combined := output.String() + "\n" + err.Error()
	if strings.Contains(combined, secret) {
		t.Fatalf("secret leaked in ask output/error: %s", combined)
	}
}

func TestAskOllamaProviderRunsHarness(t *testing.T) {
	data := ingestAskFixture(t)
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		calls++
		action := `{"action":"retrieve","correlation_id":"` + base64.StdEncoding.EncodeToString([]byte("ollama-retrieve")) + `","query":"entity","limit":1}`
		if calls > 1 {
			evidenceID := firstPromptEvidenceID(t, r)
			action = `{"action":"stop","correlation_id":"` + base64.StdEncoding.EncodeToString([]byte("ollama-stop")) + `","claims":[{"subject":"` + base64.StdEncoding.EncodeToString([]byte("entity:ollama")) + `","predicate":"` + base64.StdEncoding.EncodeToString([]byte("predicate:summary")) + `","object":{"type":"string","value":"ollama grounded"},"confidence":1,"evidence_ids":["` + evidenceID + `"]}]}`
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"response":%q,"prompt_eval_count":1,"eval_count":1}`, action)
	}))
	defer server.Close()

	var output bytes.Buffer
	if err := run(context.Background(), []string{
		"ask", "-data", data,
		"-question", "What keeps grounded answers tied to exact quotes?",
		"-provider", "ollama",
		"-ollama-url", server.URL,
		"-model", "test-ollama",
	}, &output); err != nil {
		t.Fatalf("ollama ask failed: %v\n%s", err, output.String())
	}
	result := decodeJSON[askOutput](t, output.Bytes())
	if result.Answer != "ollama grounded" || result.Provenance.Provider != "ollama" || calls != 2 {
		t.Fatalf("ollama result=%+v calls=%d", result, calls)
	}
}

func TestAskMetadataOutputEncodesOpaqueBytes(t *testing.T) {
	metadata := shoal.Metadata{
		string([]byte{'k', 0xff}): string([]byte{'v', 0xfe}),
		"plain":                   "text",
	}
	encoded := metadataForOutput(metadata)
	if len(encoded) != 2 {
		t.Fatalf("metadata entries = %+v", encoded)
	}
	if encoded[0].Key != askBytes(base64.RawURLEncoding.EncodeToString([]byte{'k', 0xff})) ||
		encoded[0].Value != askBytes(base64.RawURLEncoding.EncodeToString([]byte{'v', 0xfe})) ||
		encoded[1].Key != "cGxhaW4" ||
		encoded[1].Value != "dGV4dA" {
		t.Fatalf("metadata was not losslessly encoded: %+v", encoded)
	}
}

func firstPromptEvidenceID(t *testing.T, r *http.Request) string {
	t.Helper()
	var payload struct {
		Prompt string `json:"prompt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	const marker = `"evidence":[{"id":"`
	start := strings.Index(payload.Prompt, marker)
	if start < 0 {
		t.Fatalf("prompt missing evidence: %s", payload.Prompt)
	}
	start += len(marker)
	end := strings.IndexByte(payload.Prompt[start:], '"')
	if end < 0 {
		t.Fatalf("prompt evidence ID not terminated: %s", payload.Prompt[start:])
	}
	return payload.Prompt[start : start+end]
}

func ingestAskFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "ask-guide.md")
	if err := os.WriteFile(source, []byte(
		"# Ask Guide\n\nEntity Alpha keeps grounded answers tied to exact quotes.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(dir, "data")
	var output bytes.Buffer
	if err := run(context.Background(), []string{
		"ingest", "-data", data, "-file", source,
	}, &output); err != nil {
		t.Fatal(err)
	}
	return data
}

func TestDocumentedDemoWorkflow(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	data := filepath.Join(dir, "explorer-demo")
	guideFile := filepath.Join("..", "..", "docs", "explorer-demo-guide.md")
	releaseFile := filepath.Join("..", "..", "docs", "explorer-demo-release.md")

	var output bytes.Buffer
	if err := run(ctx, []string{
		"ingest", "-data", data, "-file", guideFile,
	}, &output); err != nil {
		t.Fatal(err)
	}
	guideIngest := decodeJSON[explorer.IngestResult](t, output.Bytes())
	if guideIngest.Disposition != explorer.IngestApplied ||
		guideIngest.Document.Title != "Shoal Explorer Demo Guide" ||
		guideIngest.SectionCount != 5 ||
		guideIngest.SpanCount != 4 {
		t.Fatalf("guide ingest = %+v", guideIngest)
	}

	output.Reset()
	if err := run(ctx, []string{
		"ingest", "-data", data, "-file", releaseFile,
	}, &output); err != nil {
		t.Fatal(err)
	}
	releaseIngest := decodeJSON[explorer.IngestResult](t, output.Bytes())
	if releaseIngest.Disposition != explorer.IngestApplied ||
		releaseIngest.Document.Title != "Explorer Release Checklist" ||
		releaseIngest.SectionCount != 3 ||
		releaseIngest.SpanCount != 2 {
		t.Fatalf("release ingest = %+v", releaseIngest)
	}

	output.Reset()
	if err := run(ctx, []string{
		"ingest", "-data", data, "-file", guideFile,
	}, &output); err != nil {
		t.Fatal(err)
	}
	reingest := decodeJSON[explorer.IngestResult](t, output.Bytes())
	if reingest.Disposition != explorer.IngestUnchanged ||
		reingest.Document.ID != guideIngest.Document.ID ||
		reingest.Revision.ID != guideIngest.Revision.ID {
		t.Fatalf("idempotent guide ingest = %+v", reingest)
	}

	output.Reset()
	if err := run(ctx, []string{"list", "-data", data}, &output); err != nil {
		t.Fatal(err)
	}
	documents := decodeJSON[[]explorer.DocumentSummary](t, output.Bytes())
	if len(documents) != 2 ||
		documents[0].Document.Title != "Explorer Release Checklist" ||
		documents[1].Document.Title != "Shoal Explorer Demo Guide" {
		t.Fatalf("documents = %+v", documents)
	}

	output.Reset()
	if err := run(ctx, []string{
		"outline", "-data", data, "-document", string(guideIngest.Document.ID),
	}, &output); err != nil {
		t.Fatal(err)
	}
	view := decodeJSON[explorer.DocumentView](t, output.Bytes())
	if view.Document.ID != guideIngest.Document.ID ||
		view.Root.Section.Heading != "Shoal Explorer Demo Guide" {
		t.Fatalf("guide outline root = %+v", view.Root.Section)
	}
	requireHeadings(t, view.Root,
		"Shoal Explorer Demo Guide",
		"Ingested knowledge",
		"Promotion gate",
		"Relationship target",
	)

	output.Reset()
	if err := run(ctx, []string{
		"query", "-data", data,
		"-text", "canary validation exact citation",
		"-top", "2", "-modes", "lexical,tree,graph", "-explain=true",
	}, &output); err != nil {
		t.Fatal(err)
	}
	response := decodeJSON[retrieval.Response](t, output.Bytes())
	if len(response.Results) == 0 {
		t.Fatalf("query returned no results: %+v", response)
	}
	guideContent, err := os.ReadFile(guideFile)
	if err != nil {
		t.Fatal(err)
	}
	expectedQuote := extractDemoQuote(t, string(guideContent))
	first := response.Results[0]
	if len(first.Evidence) != 1 || first.Evidence[0].Quote != expectedQuote {
		t.Fatalf("evidence = %+v", first.Evidence)
	}
	citation := first.Evidence[0].Citation
	if citation.DocumentID != guideIngest.Document.ID ||
		citation.RevisionID != guideIngest.Revision.ID ||
		citation.SectionID == "" ||
		citation.SpanID == "" {
		t.Fatalf("citation = %+v", citation)
	}
	if got := string(guideContent[citation.Range.Start.Offset:citation.Range.End.Offset]); got != expectedQuote {
		t.Fatalf("citation slice = %q, want %q", got, expectedQuote)
	}
	if first.Explanation == nil {
		t.Fatalf("missing explanation: %+v", first)
	}
	for _, mode := range []retrieval.Mode{
		retrieval.ModeLexical, retrieval.ModeTree, retrieval.ModeGraph,
	} {
		if first.Explanation.Scores[string(mode)] <= 0 {
			t.Fatalf("missing %s score in %+v", mode, first.Explanation)
		}
	}

	output.Reset()
	if err := run(ctx, []string{
		"ask", "-data", data,
		"-provider", "fake",
		"-question", "What keeps release notes grounded?",
	}, &output); err != nil {
		t.Fatal(err)
	}
	ask := decodeJSON[askOutput](t, output.Bytes())
	if ask.Answer != "grounded" ||
		ask.StopReason != harness.StopReasonStop ||
		len(ask.Claims) != 1 ||
		ask.Trace.Iterations == 0 {
		t.Fatalf("ask = %+v", ask)
	}
	foundAskQuote := false
	for _, evidence := range ask.Evidence {
		if evidence.Quote == expectedQuote {
			foundAskQuote = true
			break
		}
	}
	if !foundAskQuote {
		t.Fatalf("ask evidence missing expected quote: %+v", ask.Evidence)
	}

	output.Reset()
	if err := run(ctx, []string{
		"connect", "-data", data,
		"-id", "demo-edge-guide-supports-release",
		"-from", string(guideIngest.Document.ID),
		"-to", string(releaseIngest.Document.ID),
		"-type", "supports",
	}, &output); err != nil {
		t.Fatal(err)
	}
	edge := decodeJSON[graph.Edge](t, output.Bytes())
	if edge.ID != "demo-edge-guide-supports-release" ||
		edge.From != guideIngest.Document.ID ||
		edge.To != releaseIngest.Document.ID ||
		edge.Type != "supports" ||
		edge.Weight != 1 {
		t.Fatalf("edge = %+v", edge)
	}

	output.Reset()
	if err := run(ctx, []string{
		"neighbors", "-data", data,
		"-node", string(guideIngest.Document.ID),
		"-depth", "1", "-edge-types", "supports",
	}, &output); err != nil {
		t.Fatal(err)
	}
	neighborhood := decodeJSON[explorer.Neighborhood](t, output.Bytes())
	if len(neighborhood.Nodes) != 2 ||
		len(neighborhood.Edges) != 1 ||
		neighborhood.Edges[0].ID != edge.ID {
		t.Fatalf("neighborhood = %+v", neighborhood)
	}
	titles := map[string]bool{}
	for _, node := range neighborhood.Nodes {
		titles[node.Properties["title"]] = true
	}
	if !titles["Shoal Explorer Demo Guide"] ||
		!titles["Explorer Release Checklist"] {
		t.Fatalf("neighborhood titles = %+v", titles)
	}
}

func decodeJSON[T any](t *testing.T, data []byte) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("decode %T from %s: %v", value, string(data), err)
	}
	return value
}

func requireHeadings(t *testing.T, root explorer.SectionView, headings ...string) {
	t.Helper()
	found := make(map[string]bool)
	var visit func(explorer.SectionView)
	visit = func(section explorer.SectionView) {
		found[section.Section.Heading] = true
		for _, child := range section.Children {
			visit(child)
		}
	}
	visit(root)
	for _, heading := range headings {
		if !found[heading] {
			t.Fatalf("missing heading %q in %+v", heading, root)
		}
	}
}

func extractDemoQuote(t *testing.T, source string) string {
	t.Helper()
	start := strings.Index(source, "Run the canary validation")
	if start < 0 {
		t.Fatal("demo quote start not found")
	}
	end := strings.Index(source[start:], "\n### Relationship target")
	if end < 0 {
		t.Fatal("demo quote end not found")
	}
	return strings.TrimRight(source[start:start+end], "\r\n")
}
