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

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// citingObservedTool reports, as the evidence its result exposed, whatever
// citation the test sets: an extension (or a buggy one) can report any.
type citingObservedTool struct {
	evidence interaction.EvidenceReference
}

func (*citingObservedTool) Tool() Tool {
	return Tool{
		Name: "test.citing_read", Description: "report a citation",
		InputSchema: json.RawMessage(
			`{"type":"object","properties":{},"additionalProperties":false}`),
		Annotations: readOnlyAnnotations(),
		Execution:   &ToolExecution{TaskSupport: "forbidden"},
	}
}

func (*citingObservedTool) Call(context.Context, json.RawMessage) (any, error) {
	return struct {
		OK bool `json:"ok"`
	}{OK: true}, nil
}

func (*citingObservedTool) ToolAuthorizationOperation() auth.Operation {
	return auth.OperationRead
}

func (p *citingObservedTool) ObserveToolResult(any) (ToolObservation, error) {
	return ToolObservation{
		RetrievedNodeIDs:  append([]shoal.ID(nil), p.evidence.NodeIDs...),
		RetrievedEvidence: []interaction.EvidenceReference{p.evidence},
	}, nil
}

// revisionWithholdingStore registers no revision withheld: what a reader the
// catalog denies a revision sees of it.
type revisionWithholdingStore struct {
	authorized.PolicyStore
	withheld shoal.ID
}

func (s *revisionWithholdingStore) Revision(
	ctx context.Context, documentID, revisionID shoal.ID,
) (authorized.RevisionRegistration, bool, error) {
	if revisionID == s.withheld {
		return authorized.RevisionRegistration{}, false, nil
	}
	return s.PolicyStore.Revision(ctx, documentID, revisionID)
}

func (s *revisionWithholdingStore) RevisionRules(
	ctx context.Context, references []authorized.RevisionReference,
) (map[shoal.ID][]authorized.RevisionRule, error) {
	rules, err := s.PolicyStore.RevisionRules(ctx, references)
	if err != nil {
		return nil, err
	}
	for documentID, list := range rules {
		var kept []authorized.RevisionRule
		for _, rule := range list {
			if rule.RevisionID != s.withheld {
				kept = append(kept, rule)
			}
		}
		rules[documentID] = kept
	}
	return rules, nil
}

func mcpCitation(
	t *testing.T, documentID, revision shoal.ID, span document.Span, shift int64,
) interaction.EvidenceReference {
	t.Helper()
	citation := document.Citation{
		DocumentID: documentID, RevisionID: revision,
		SectionID: span.SectionID, SpanID: span.ID, Range: span.Range,
	}
	citation.Range.Start.Offset += shift
	citation.Range.End.Offset += shift
	anchor, err := inference.NewDocumentAnchor(citation, span.Text)
	if err != nil {
		t.Fatal(err)
	}
	reference, err := anchor.EvidenceReference()
	if err != nil {
		t.Fatal(err)
	}
	return reference
}

// TestMCPRecordingRefusesAHiddenRevisionCitedThroughCurrentSpans is #714
// through MCP: the tool's recorded evidence cites an older revision the
// caller may not read, through a span ID it reads in the current revision.
// A true probe, a false probe and a revision that never existed answer the
// caller identically and record nothing, while citing the current revision
// records.
func TestMCPRecordingRefusesAHiddenRevisionCitedThroughCurrentSpans(t *testing.T) {
	const uri = "file:///mcp-citation.txt"
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = corpus.Close() })
	if err := corpus.EnsureInteractionSink(context.Background()); err != nil {
		t.Fatal(err)
	}
	authority := auth.NewAuthority()
	store := &revisionWithholdingStore{PolicyStore: authorized.NewMemoryPolicyStore()}
	generations := httpGenerationReader{domain: []byte("domain"), generation: 1}
	selector, err := authorized.NewStaticPolicySelector([]byte("source"), []byte("policy"))
	if err != nil {
		t.Fatal(err)
	}
	client, err := authorized.NewClient(authorized.Config{
		Base: corpus, InteractionWriter: corpus, InteractionReader: corpus,
		SnapshotValidator: corpus,
		Resolver:          authority.Resolver(), PolicySelector: selector,
		PolicyStore: store, GenerationReader: generations, Clock: time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	bind := func(decision auth.Decision) context.Context {
		ctx, err := authority.Binder().Bind(context.Background(), decision)
		if err != nil {
			t.Fatal(err)
		}
		return ctx
	}
	expires := time.Now().Add(time.Hour)
	setup := func() context.Context {
		return bind(httpScopedDecision(t, "alice", "setup", expires,
			[]byte("source"), []byte("policy")))
	}
	// R2 keeps R1's first line. The catalog then withholds R1 from alice,
	// as a relabel would (the label cases are in pkg/explorer/authorized):
	// she reads R2 and not R1.
	hidden, err := client.Ingest(setup(), explorer.Source{
		URI: uri, MediaType: explorer.MediaTypeText,
		Content: "the shared opening line\n\nthe hidden plan is to sell\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	current, err := client.Ingest(setup(), explorer.Source{
		URI: uri, MediaType: explorer.MediaTypeText,
		Content: "preface\n\nthe shared opening line\n\nnothing to see here\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	store.withheld = hidden.Revision.ID
	hiddenView, err := corpus.Document(context.Background(), hidden.Document.ID, hidden.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	currentView, err := corpus.Document(context.Background(), current.Document.ID, current.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	var shared, currentShared document.Span
	for _, span := range hiddenView.Root.Spans {
		for _, candidate := range currentView.Root.Spans {
			if span.ID == candidate.ID {
				shared, currentShared = span, candidate
			}
		}
	}
	if shared.ID == "" || shared.Range == currentShared.Range {
		t.Fatal("fixture shares no span at a different range")
	}
	requests := 0
	alice := func() auth.Decision {
		requests++
		return httpScopedDecision(t, "alice", shoal.ID(fmt.Sprintf("alice-%d", requests)),
			expires, []byte("source"), []byte("policy"))
	}
	if _, err := client.Document(bind(alice()), hidden.Document.ID,
		hidden.Revision.ID); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("alice's Document(R1) = %v, want not_found", err)
	}
	tool := &citingObservedTool{}
	service, err := webapi.NewEmbeddedService(client)
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(Config{
		Service: service, Authority: authority,
		Decisions: DecisionProviderFunc(func(context.Context) (auth.Decision, error) {
			return alice(), nil
		}),
		InteractionSink: client, Snapshots: client,
		OptionalTools:    []OptionalToolProvider{tool},
		requestIDFactory: sequentialRequestIDs(),
	})
	if err != nil {
		t.Fatal(err)
	}
	makeReady(t, server)
	call := func(evidence interaction.EvidenceReference) ToolResult {
		tool.evidence = evidence
		return decodeToolResult(t, callToolRequest(t, server, "test.citing_read", `{}`))
	}
	recorded := func() int {
		records, err := corpus.InteractionRecords(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, record := range records {
			for _, turn := range record.Session.Turns {
				if turn.ToolCall != nil && len(turn.ToolCall.RetrievedEvidence) > 0 {
					count++
				}
			}
		}
		return count
	}
	never := call(mcpCitation(t, hidden.Document.ID, "rev_never_existed", shared, 0))
	truth := call(mcpCitation(t, hidden.Document.ID, hidden.Revision.ID, shared, 0))
	falsehood := call(mcpCitation(t, hidden.Document.ID, hidden.Revision.ID, shared, 1))
	if !never.IsError {
		t.Fatalf("a never-existing revision was recorded: %s", never.StructuredContent)
	}
	for name, result := range map[string]ToolResult{"true probe": truth, "false probe": falsehood} {
		got, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		want, err := json.Marshal(never)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Fatalf("%s of R1 answered %s, but a never-existing revision %s", name, got, want)
		}
	}
	if got := recorded(); got != 0 {
		t.Fatalf("refused citations recorded %d sessions with evidence", got)
	}
	if result := call(mcpCitation(t, current.Document.ID, current.Revision.ID,
		currentShared, 0)); result.IsError {
		t.Fatalf("citing the current revision failed: %s", result.StructuredContent)
	}
	if got := recorded(); got != 1 {
		t.Fatalf("citing the current revision recorded %d sessions with evidence", got)
	}
}
