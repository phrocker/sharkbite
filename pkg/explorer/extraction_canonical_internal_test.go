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

package explorer

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/extraction"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// TestGraphCanonicalizesStoredExtractions: extraction records stored before
// shared content was canonical (#700) present an entity under a display
// name and confidence, and a relation under a weight. Two such records of
// the same entity and relation build one canonical node and edge, never a
// conflict, and never either record's presentation.
func TestGraphCanonicalizesStoredExtractions(t *testing.T) {
	corpus, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = corpus.Close() })
	ctx := context.Background()
	var documents []IngestResult
	for _, uri := range []string{"file:///a.md", "file:///b.md"} {
		result, err := corpus.Ingest(ctx, Source{
			URI: uri, MediaType: MediaTypeMarkdown, Content: "# zeta\n",
		})
		if err != nil {
			t.Fatal(err)
		}
		documents = append(documents, result)
	}
	hex := strings.Repeat("0", 63)
	entity := shoal.ID("inferred-entity:" + hex + "1")
	tool := shoal.ID("inferred-entity:" + hex + "2")
	relation := shoal.ID("inferred-edge:" + hex + "3")
	legacy := func(document IngestResult, id shoal.ID, display, confidence string, weight shoal.Score) *persistedExtraction {
		node := func(nodeID shoal.ID, key string) graph.Node {
			return graph.Node{
				ID: nodeID, Kind: "ontology:skill", Labels: []string{display, "skill"},
				Properties: shoal.Metadata{
					extraction.GraphPropertyOntologyConceptID:  "concept:skill",
					extraction.GraphPropertyOntologyConceptKey: "skill",
					extraction.GraphPropertyEntityKey:          key,
					"confidence":                               confidence,
				},
			}
		}
		return &persistedExtraction{
			ID: id, DocumentID: document.Document.ID, RevisionID: document.Revision.ID,
			OntologySchemaID: "schema", OntologyVersionID: "version",
			PublishedAt: time.Unix(1700000000, 0).UTC(),
			Nodes:       []graph.Node{node(entity, "zeta"), node(tool, "zeta-cli")},
			Edges: []graph.Edge{{
				ID: relation, From: entity, To: tool, Type: "provides_tool", Weight: weight,
				Properties: shoal.Metadata{"ontology_relationship_id": "relationship:provides"},
			}},
		}
	}
	corpus.mu.Lock()
	corpus.extractions["extraction-a"] = legacy(documents[0], "extraction-a", "Zeta", "0.900000", 0.9)
	corpus.extractions["extraction-b"] = legacy(documents[1], "extraction-b", "ZETA", "0.100000", 0.1)
	err = corpus.rebuildCurrentGraphLocked()
	node, edge := corpus.graphNodes[entity], corpus.graphEdges[relation]
	corpus.mu.Unlock()
	if err != nil {
		t.Fatalf("two presentations of one entity conflict: %v", err)
	}
	if strings.Join(node.Labels, ",") != "skill,zeta" {
		t.Fatalf("graph node labels = %v, want the canonical skill,zeta", node.Labels)
	}
	if _, ok := node.Properties["confidence"]; ok {
		t.Fatalf("graph node keeps a confidence: %v", node.Properties)
	}
	if edge.Weight != 1 || len(edge.Properties) != 1 {
		t.Fatalf("graph relation = %+v, want weight 1 and only its relationship", edge)
	}
}
