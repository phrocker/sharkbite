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
