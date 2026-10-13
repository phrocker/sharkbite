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

package explorer_test

import (
	"context"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/extraction"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const sampleSkillMarkdown = `# Azure Search Skill

Tools:
- Azure AI Search
- azd

Capabilities:
- Hybrid retrieval
- Vector indexing

Dependencies:
- Azure Core Skill
`

func TestExtractDocumentPublishesSkillGraph(t *testing.T) {
	ctx := context.Background()
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = corpus.Close() })
	ingested, err := corpus.Ingest(ctx, explorer.Source{
		URI: "file:///SKILL.md", MediaType: explorer.MediaTypeMarkdown,
		Content: sampleSkillMarkdown,
	})
	if err != nil {
		t.Fatal(err)
	}
	version := skillsOntologyVersion(t)
	extracted, err := corpus.ExtractDocument(ctx, explorer.ExtractionRequest{
		DocumentID: ingested.Document.ID, RevisionID: ingested.Revision.ID,
		Version: version,
	})
	if err != nil {
		t.Fatal(err)
	}
	if extracted.EntityCount != 6 || extracted.RelationCount != 5 {
		t.Fatalf("skill extraction counts = entities:%d relations:%d, want 6 and 5",
			extracted.EntityCount, extracted.RelationCount)
	}
	if extracted.GraphNodeCount != 6 || extracted.GraphEdgeCount != 11 {
		t.Fatalf("published graph counts = nodes:%d edges:%d, want 6 and 11",
			extracted.GraphNodeCount, extracted.GraphEdgeCount)
	}
	neighborhood, err := corpus.Neighborhood(ctx, explorer.NeighborhoodRequest{
		NodeIDs: []shoal.ID{ingested.Document.ID}, Depth: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ontologyNodes := countOntologyNodes(neighborhood.Nodes); ontologyNodes != 6 {
		t.Fatalf("neighborhood ontology nodes = %d, want 6", ontologyNodes)
	}
	if countEdgesOfType(neighborhood.Edges, "provides_tool") != 2 {
		t.Fatalf("neighborhood provides_tool edges = %d, want 2",
			countEdgesOfType(neighborhood.Edges, "provides_tool"))
	}
	if len(neighborhood.Assertions) != 5 {
		t.Fatalf("neighborhood relationship assertions = %d, want 5", len(neighborhood.Assertions))
	}
}

func TestExtractDocumentRerunReusesSkillEntities(t *testing.T) {
	ctx := context.Background()
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = corpus.Close() })
	ingested, err := corpus.Ingest(ctx, explorer.Source{
		URI: "file:///SKILL.md", MediaType: explorer.MediaTypeMarkdown,
		Content: sampleSkillMarkdown,
	})
	if err != nil {
		t.Fatal(err)
	}
	version := skillsOntologyVersion(t)
	first, err := corpus.ExtractDocument(ctx, explorer.ExtractionRequest{
		DocumentID: ingested.Document.ID, RevisionID: ingested.Revision.ID,
		Version: version,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := corpus.ExtractDocument(ctx, explorer.ExtractionRequest{
		DocumentID: ingested.Document.ID, RevisionID: ingested.Revision.ID,
		Version: version,
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.CreatedEntities != 0 || second.ReusedEntities != first.EntityCount {
		t.Fatalf("rerun entity resolution = created:%d reused:%d, want 0 reused %d",
			second.CreatedEntities, second.ReusedEntities, first.EntityCount)
	}
	neighborhood, err := corpus.Neighborhood(ctx, explorer.NeighborhoodRequest{
		NodeIDs: []shoal.ID{ingested.Document.ID}, Depth: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ontologyNodes := countOntologyNodes(neighborhood.Nodes); ontologyNodes != first.EntityCount {
		t.Fatalf("rerun duplicated ontology nodes: got %d, want %d",
			ontologyNodes, first.EntityCount)
	}
}

// TestExtractDocumentContextNodesSelectsGroundingEntities: ContextNodes sees
// the existing entities in ascending order, and only the ones it returns are
// offered as graph context, so an entity it withholds is created afresh
// rather than reported as reused (#700). An error from it fails the plan.
func TestExtractDocumentContextNodesSelectsGroundingEntities(t *testing.T) {
	ctx := context.Background()
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = corpus.Close() })
	ingested, err := corpus.Ingest(ctx, explorer.Source{
		URI: "file:///SKILL.md", MediaType: explorer.MediaTypeMarkdown,
		Content: sampleSkillMarkdown,
	})
	if err != nil {
		t.Fatal(err)
	}
	version := skillsOntologyVersion(t)
	request := explorer.ExtractionRequest{
		DocumentID: ingested.Document.ID, RevisionID: ingested.Revision.ID,
		Version: version,
	}
	first, err := corpus.ExtractDocument(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	var offered []shoal.ID
	request.ContextNodes = func(_ context.Context, ids []shoal.ID) ([]shoal.ID, error) {
		offered = append([]shoal.ID(nil), ids...)
		return nil, nil
	}
	withheld, err := corpus.ExtractDocument(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(offered) != first.EntityCount {
		t.Fatalf("ContextNodes was offered %d entities, want %d", len(offered), first.EntityCount)
	}
	for index := 1; index < len(offered); index++ {
		if shoal.CompareID(offered[index-1], offered[index]) >= 0 {
			t.Fatalf("ContextNodes candidates are not in ascending order: %v", offered)
		}
	}
	if withheld.ReusedEntities != 0 || withheld.CreatedEntities != first.EntityCount {
		t.Fatalf("withheld context = created:%d reused:%d, want every entity created",
			withheld.CreatedEntities, withheld.ReusedEntities)
	}
	request.ContextNodes = func(_ context.Context, ids []shoal.ID) ([]shoal.ID, error) {
		return ids, nil
	}
	kept, err := corpus.ExtractDocument(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if kept.ReusedEntities != first.EntityCount {
		t.Fatalf("offered context reused %d entities, want %d", kept.ReusedEntities, first.EntityCount)
	}
	request.ContextNodes = func(context.Context, []shoal.ID) ([]shoal.ID, error) {
		return nil, shoal.NewError(shoal.ErrorUnavailable, "context unavailable")
	}
	if _, err := corpus.PlanExtractDocument(ctx, request); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("plan with a failing ContextNodes = %v, want its error", err)
	}
}

func TestExtractDocumentCollapsesSharedToolAcrossSkillFiles(t *testing.T) {
	ctx := context.Background()
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = corpus.Close() })
	version := skillsOntologyVersion(t)
	var sharedTool shoal.ID
	var documents []shoal.ID
	for _, skillName := range []string{"Alpha Skill", "Beta Skill", "Gamma Skill"} {
		ingested, err := corpus.Ingest(ctx, explorer.Source{
			URI: "file:///" + skillName + "/SKILL.md", MediaType: explorer.MediaTypeMarkdown,
			Content: "# " + skillName + "\n\nTools:\n- Shared CLI\n\nCapabilities:\n- " + skillName + " feature\n",
		})
		if err != nil {
			t.Fatal(err)
		}
		documents = append(documents, ingested.Document.ID)
		extracted, err := corpus.ExtractDocument(ctx, explorer.ExtractionRequest{
			DocumentID: ingested.Document.ID, RevisionID: ingested.Revision.ID,
			Version: version,
		})
		if err != nil {
			t.Fatal(err)
		}
		toolID := extractedNodeWithKey(t, extracted.GraphNodes, "shared_cli")
		if sharedTool == "" {
			sharedTool = toolID
			continue
		}
		if toolID != sharedTool {
			t.Fatalf("shared tool entity ID changed across skill files: %s != %s",
				toolID, sharedTool)
		}
	}
	neighborhood, err := corpus.Neighborhood(ctx, explorer.NeighborhoodRequest{
		NodeIDs: documents, Depth: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if countNodesWithEntityKey(neighborhood.Nodes, "shared_cli") != 1 {
		t.Fatalf("shared tool node count = %d, want 1",
			countNodesWithEntityKey(neighborhood.Nodes, "shared_cli"))
	}
}

func skillsOntologyVersion(t *testing.T) ontology.OntologyVersion {
	t.Helper()
	name, err := ontology.NewPropertyDefinition(
		"name", "Name", "Human-readable name", ontology.ValueString, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	description, err := ontology.NewPropertyDefinition(
		"description", "Description", "Description", ontology.ValueString, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	skill, err := ontology.NewConceptDefinition(
		"skill", "Skill", "Agent skill", []shoal.ID{name.ID(), description.ID()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tool, err := ontology.NewConceptDefinition(
		"tool", "Tool", "Usable tool", []shoal.ID{name.ID(), description.ID()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	capability, err := ontology.NewConceptDefinition(
		"capability", "Capability", "Provided feature", []shoal.ID{name.ID(), description.ID()}, nil)
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
	dependsOn, err := ontology.NewRelationshipDefinition(
		"depends_on", "Depends on", "Skill depends on another skill",
		[]shoal.ID{skill.ID()}, []shoal.ID{skill.ID()}, nil, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	schema, err := ontology.NewOntologySchema(
		"agent-skills", "Agent Skills", "Uploaded agent skill ontology", nil)
	if err != nil {
		t.Fatal(err)
	}
	version, err := ontology.NewOntologyVersion(
		schema, "v1", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		[]ontology.ConceptDefinition{skill, tool, capability},
		[]ontology.RelationshipDefinition{providesTool, providesCapability, dependsOn},
		[]ontology.PropertyDefinition{name, description},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	return version
}

func extractedNodeWithKey(t *testing.T, nodes []graph.Node, key string) shoal.ID {
	t.Helper()
	for _, node := range nodes {
		if node.Properties[extraction.GraphPropertyEntityKey] == key {
			return node.ID
		}
	}
	t.Fatalf("extracted node with key %q not found in %+v", key, nodes)
	return ""
}

func countNodesWithEntityKey(nodes []graph.Node, key string) int {
	count := 0
	for _, node := range nodes {
		if node.Properties[extraction.GraphPropertyEntityKey] == key {
			count++
		}
	}
	return count
}

func countOntologyNodes(nodes []graph.Node) int {
	count := 0
	for _, node := range nodes {
		if node.Properties[extraction.GraphPropertyOntologyConceptID] != "" {
			count++
		}
	}
	return count
}

func countEdgesOfType(edges []graph.Edge, edgeType string) int {
	count := 0
	for _, edge := range edges {
		if edge.Type == edgeType {
			count++
		}
	}
	return count
}
