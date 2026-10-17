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
	"encoding/base64"
	"fmt"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// TestReadToolsRecordAuthoritativeEvidenceAgainstTheExplorer is #137: the
// Explorer's recorder recomputes every evidence anchor from the cited bytes
// or the exact graph path and refuses any other identity, after which the
// tool result is withheld. The server must therefore record the anchors the
// recorder will recompute: a document anchor over the citation and its
// quote, and a graph anchor over the path in its own node order, for the
// retrieve, neighborhood and path tools alike. Only the real Explorer checks
// this; the package fakes accept any anchor.
func TestReadToolsRecordAuthoritativeEvidenceAgainstTheExplorer(t *testing.T) {
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = corpus.Close() })
	if err := corpus.EnsureInteractionSink(context.Background()); err != nil {
		t.Fatal(err)
	}
	authority := auth.NewAuthority()
	generations := httpGenerationReader{domain: []byte("domain"), generation: 1}
	selector, err := authorized.NewStaticPolicySelector([]byte("source"), []byte("policy"))
	if err != nil {
		t.Fatal(err)
	}
	client, err := authorized.NewClient(authorized.Config{
		Base: corpus, InteractionWriter: corpus, InteractionReader: corpus,
		SnapshotValidator: corpus,
		Resolver:          authority.Resolver(), PolicySelector: selector,
		PolicyStore: authorized.NewMemoryPolicyStore(), GenerationReader: generations,
		Clock: time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour)
	requests := 0
	alice := func() auth.Decision {
		requests++
		return httpScopedDecision(t, "alice", shoal.ID(fmt.Sprintf("alice-%d", requests)),
			expires, []byte("source"), []byte("policy"))
	}
	bound := func() context.Context {
		ctx, err := authority.Binder().Bind(context.Background(), alice())
		if err != nil {
			t.Fatal(err)
		}
		return ctx
	}
	guide, err := client.Ingest(bound(), explorer.Source{
		URI: "file:///guide.md", MediaType: explorer.MediaTypeMarkdown,
		Content: "# Guide\n\nRun the canary validation before promotion.\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	release, err := client.Ingest(bound(), explorer.Source{
		URI: "file:///release.md", MediaType: explorer.MediaTypeMarkdown,
		Content: "# Release\n\nPromotion follows the canary.\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	connecting := bound()
	edgeID, err := client.ApplicationEdgeID(connecting, "guide-supports-release")
	if err != nil {
		t.Fatal(err)
	}
	edge := graph.Edge{
		ID: edgeID, From: guide.Document.ID,
		To: release.Document.ID, Type: "supports", Weight: 1,
	}
	if err := client.Connect(connecting, edge); err != nil {
		t.Fatal(err)
	}

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
		requestIDFactory: sequentialRequestIDs(),
	})
	if err != nil {
		t.Fatal(err)
	}
	makeReady(t, server)
	call := func(name, arguments string) ToolResult {
		t.Helper()
		result := decodeToolResult(t, callToolRequest(t, server, name, arguments))
		if result.IsError {
			t.Fatalf("%s %s was withheld: %s", name, arguments, result.StructuredContent)
		}
		return result
	}
	encoded := func(id shoal.ID) string {
		return base64.RawURLEncoding.EncodeToString([]byte(id))
	}

	call(ToolRetrieve,
		`{"query":{"text":"canary validation","top_k":1,"modes":["lexical","tree","graph"]}}`)
	call(ToolNeighborhood,
		`{"node_ids":["`+encoded(guide.Document.ID)+`"],"depth":1}`)
	call(ToolPath,
		`{"from":"`+encoded(guide.Document.ID)+`","to":"`+encoded(release.Document.ID)+`"}`)

	records, err := corpus.InteractionRecords(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var documents, paths, edges int
	for _, record := range records {
		for _, turn := range record.Session.Turns {
			if turn.ToolCall == nil {
				continue
			}
			for _, evidence := range turn.ToolCall.RetrievedEvidence {
				switch evidence.Kind {
				case interaction.EvidenceDocument:
					if evidence.Citation.DocumentID == guide.Document.ID {
						documents++
					}
				case interaction.EvidenceGraph:
					if len(evidence.EdgeIDs) == 1 && evidence.EdgeIDs[0] == edge.ID {
						if len(evidence.NodeIDs) != 2 ||
							evidence.NodeIDs[0] != edge.From ||
							evidence.NodeIDs[1] != edge.To {
							t.Fatalf("edge recorded out of path order: %+v", evidence)
						}
						edges++
					} else {
						paths++
					}
				}
			}
		}
	}
	// The neighborhood and the path each record the edge once.
	if documents == 0 || paths == 0 || edges != 2 {
		t.Fatalf("recorded evidence: %d citations of the guide, %d retrieval paths, %d anchors on the edge; want >0, >0, 2",
			documents, paths, edges)
	}
}

// TestNeighborhoodRecordsExtractedRelationsAgainstTheExplorer: an extracted
// relation's edge carries the relationship's key as its type and a canonical
// weight, while the relationship ID and the extraction's confidence stay on
// its assertion. The Explorer binds the two through the assertion's edge
// metadata alone. A recorder that compares them field for field refuses
// every extracted relation the Explorer serves, and the neighborhood is
// withheld. The recorded anchor over such an edge must cite its assertion.
func TestNeighborhoodRecordsExtractedRelationsAgainstTheExplorer(t *testing.T) {
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = corpus.Close() })
	if err := corpus.EnsureInteractionSink(context.Background()); err != nil {
		t.Fatal(err)
	}
	authority := auth.NewAuthority()
	generations := httpGenerationReader{domain: []byte("domain"), generation: 1}
	selector, err := authorized.NewStaticPolicySelector([]byte("source"), []byte("policy"))
	if err != nil {
		t.Fatal(err)
	}
	client, err := authorized.NewClient(authorized.Config{
		Base: corpus, InteractionWriter: corpus, InteractionReader: corpus,
		SnapshotValidator: corpus,
		Resolver:          authority.Resolver(), PolicySelector: selector,
		PolicyStore: authorized.NewMemoryPolicyStore(), GenerationReader: generations,
		Clock: time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour)
	requests := 0
	alice := func() auth.Decision {
		requests++
		return httpScopedDecision(t, "alice", shoal.ID(fmt.Sprintf("alice-%d", requests)),
			expires, []byte("source"), []byte("policy"))
	}
	bound := func() context.Context {
		ctx, err := authority.Binder().Bind(context.Background(), alice())
		if err != nil {
			t.Fatal(err)
		}
		return ctx
	}
	skill, err := client.Ingest(bound(), explorer.Source{
		URI: "file:///skills/SKILL.md", MediaType: explorer.MediaTypeMarkdown,
		Content: "# Release Skill\n\nTools:\n- release-cli\n\nCapabilities:\n- Canary promotion\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	extracted, err := client.ExtractDocument(bound(), explorer.ExtractionRequest{
		DocumentID: skill.Document.ID, RevisionID: skill.Revision.ID,
		Version: skillsOntologyVersion(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	if extracted.RelationCount == 0 {
		t.Fatalf("extraction materialized no relations: %+v", extracted)
	}

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
		requestIDFactory: sequentialRequestIDs(),
	})
	if err != nil {
		t.Fatal(err)
	}
	makeReady(t, server)
	arguments := `{"node_ids":["` +
		base64.RawURLEncoding.EncodeToString([]byte(skill.Document.ID)) +
		`"],"depth":2}`
	result := decodeToolResult(t, callToolRequest(t, server, ToolNeighborhood, arguments))
	if result.IsError {
		t.Fatalf("neighborhood over extracted relations was withheld: %s",
			result.StructuredContent)
	}

	records, err := corpus.InteractionRecords(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	relations := make(map[shoal.ID]struct{}, len(extracted.RelationshipEdgeIDs))
	for _, id := range extracted.RelationshipEdgeIDs {
		relations[id] = struct{}{}
	}
	asserted := 0
	for _, record := range records {
		for _, turn := range record.Session.Turns {
			if turn.ToolCall == nil {
				continue
			}
			for _, evidence := range turn.ToolCall.RetrievedEvidence {
				if evidence.Kind != interaction.EvidenceGraph ||
					len(evidence.EdgeIDs) != 1 {
					continue
				}
				if _, ok := relations[evidence.EdgeIDs[0]]; !ok {
					continue
				}
				if len(evidence.Assertions) != 1 ||
					evidence.Assertions[0].EdgeID != evidence.EdgeIDs[0] {
					t.Fatalf("extracted relation recorded without its assertion: %+v", evidence)
				}
				asserted++
			}
		}
	}
	if asserted == 0 {
		t.Fatalf("no anchor over an extracted relation was recorded; relations %v",
			extracted.RelationshipEdgeIDs)
	}
}

// skillsOntologyVersion is the small agent-skills ontology the Explorer's
// deterministic extractor materializes from a SKILL.md document.
func skillsOntologyVersion(t *testing.T) ontology.OntologyVersion {
	t.Helper()
	name, err := ontology.NewPropertyDefinition(
		"name", "Name", "Human-readable name", ontology.ValueString, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	skill, err := ontology.NewConceptDefinition(
		"skill", "Skill", "Agent skill", []shoal.ID{name.ID()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tool, err := ontology.NewConceptDefinition(
		"tool", "Tool", "Usable tool", []shoal.ID{name.ID()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	capability, err := ontology.NewConceptDefinition(
		"capability", "Capability", "Provided feature", []shoal.ID{name.ID()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	providesTool, err := ontology.NewRelationshipDefinition(
		"provides_tool", "Provides tool", "Skill provides tool",
		[]shoal.ID{skill.ID()}, []shoal.ID{tool.ID()}, nil, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	providesCapability, err := ontology.NewRelationshipDefinition(
		"provides_capability", "Provides capability", "Skill provides capability",
		[]shoal.ID{skill.ID()}, []shoal.ID{capability.ID()}, nil, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	schema, err := ontology.NewOntologySchema(
		"agent-skills-mcp", "Agent Skills MCP", "Skill ontology for MCP recording tests", nil)
	if err != nil {
		t.Fatal(err)
	}
	version, err := ontology.NewOntologyVersion(
		schema, "v1", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		[]ontology.ConceptDefinition{skill, tool, capability},
		[]ontology.RelationshipDefinition{providesTool, providesCapability},
		[]ontology.PropertyDefinition{name}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	return version
}
