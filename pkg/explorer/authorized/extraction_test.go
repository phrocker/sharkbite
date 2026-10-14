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

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/extraction"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const authorizedSkillMarkdown = `# Private Skill

Tools:
- private-cli

Capabilities:
- Secret graph extraction
`

func TestAuthorizedExtractDocumentSameTenantSharedEntityCollapses(t *testing.T) {
	f := newFixture(t)
	version := authorizedSkillsOntologyVersion(t)
	first := ingestAuthorizedSkill(t, f.clientA, f.admin(t), "alpha", "shared-cli")
	firstExtracted, err := f.clientA.ExtractDocument(f.alice(t), explorer.ExtractionRequest{
		DocumentID: first.Document.ID, RevisionID: first.Revision.ID, Version: version,
	})
	if err != nil {
		t.Fatal(err)
	}
	second := ingestAuthorizedSkill(t, f.clientA, f.admin(t), "beta", "shared-cli")
	secondExtracted, err := f.clientA.ExtractDocument(f.alice(t), explorer.ExtractionRequest{
		DocumentID: second.Document.ID, RevisionID: second.Revision.ID, Version: version,
	})
	if err != nil {
		t.Fatalf("same-tenant shared extraction failed; shared entities must merge under the same rule: %v", err)
	}
	firstTool := extractedNodeWithKey(t, firstExtracted.GraphNodes, "shared_cli")
	secondTool := extractedNodeWithKey(t, secondExtracted.GraphNodes, "shared_cli")
	if firstTool != secondTool {
		t.Fatalf("same-tenant shared entity IDs differ: %s != %s", firstTool, secondTool)
	}
	neighborhood, err := f.clientA.Neighborhood(f.alice(t), explorer.NeighborhoodRequest{
		NodeIDs: []shoal.ID{first.Document.ID, second.Document.ID}, Depth: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if countNodesWithEntityKey(neighborhood.Nodes, "shared_cli") != 1 {
		t.Fatalf("same-tenant shared entity node count = %d, want 1",
			countNodesWithEntityKey(neighborhood.Nodes, "shared_cli"))
	}
}

func TestAuthorizedExtractDocumentCrossTenantSharedEntityGetsDistinctNodes(t *testing.T) {
	f := newFixture(t)
	version := authorizedSkillsOntologyVersion(t)
	aliceDoc := ingestAuthorizedSkill(t, f.clientA, f.admin(t), "alice", "shared-cli")
	aliceExtracted, err := f.clientA.ExtractDocument(f.alice(t), explorer.ExtractionRequest{
		DocumentID: aliceDoc.Document.ID, RevisionID: aliceDoc.Revision.ID, Version: version,
	})
	if err != nil {
		t.Fatal(err)
	}
	bobDoc := ingestAuthorizedSkill(t, f.clientB, f.admin(t), "bob", "shared-cli")
	bobExtracted, err := f.clientB.ExtractDocument(f.bob(t), explorer.ExtractionRequest{
		DocumentID: bobDoc.Document.ID, RevisionID: bobDoc.Revision.ID, Version: version,
	})
	if err != nil {
		t.Fatalf("cross-tenant shared extraction failed; entity namespace should isolate policy registrations: %v", err)
	}
	aliceTool := extractedNodeWithKey(t, aliceExtracted.GraphNodes, "shared_cli")
	bobTool := extractedNodeWithKey(t, bobExtracted.GraphNodes, "shared_cli")
	if aliceTool == bobTool {
		t.Fatalf("cross-tenant shared entity ID = %s, want tenant-scoped IDs", aliceTool)
	}
	aliceGraph, err := f.clientA.Neighborhood(f.alice(t), explorer.NeighborhoodRequest{
		NodeIDs: []shoal.ID{aliceDoc.Document.ID}, Depth: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if hasNode(aliceGraph, bobDoc.Document.ID) || hasNode(aliceGraph, bobTool) {
		t.Fatalf("alice graph leaked bob node IDs: %+v", aliceGraph.Nodes)
	}
	bobNamespace := bobExtracted.GraphNodes[0].Properties[extraction.GraphPropertyEntityNamespace]
	if got := countNodesWithEntityKey(aliceGraph.Nodes, "shared_cli"); got != 1 {
		t.Fatalf("alice graph shared entity count = %d, want 1", got)
	}
	for _, node := range aliceGraph.Nodes {
		if node.Properties[extraction.GraphPropertyEntityNamespace] == bobNamespace {
			t.Fatalf("alice graph leaked bob entity namespace property: %+v", node)
		}
	}
	for _, edge := range aliceGraph.Edges {
		if edge.From == bobDoc.Document.ID || edge.To == bobDoc.Document.ID ||
			edge.From == bobTool || edge.To == bobTool {
			t.Fatalf("alice graph leaked bob edge endpoint: %+v", edge)
		}
	}
	for _, assertion := range aliceGraph.Assertions {
		target, _ := assertion.Object().ReferenceValue()
		if assertion.Subject() == bobTool || target == bobTool {
			t.Fatalf("alice graph leaked bob assertion: %+v", assertion)
		}
	}
}

func TestExtractDocumentPolicyConflictUsesNonDisclosingError(t *testing.T) {
	f := newFixture(t)
	ordinaryHidden, err := f.clientB.Ingest(f.admin(t), explorer.Source{
		URI: "file:///hidden/SKILL.md", MediaType: explorer.MediaTypeMarkdown,
		Content: authorizedSkillMarkdown,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, ordinary := f.clientA.ExtractDocument(f.alice(t), explorer.ExtractionRequest{
		DocumentID: ordinaryHidden.Document.ID,
		RevisionID: ordinaryHidden.Revision.ID,
		Version:    authorizedSkillsOntologyVersion(t),
	})
	if !shoal.IsErrorCode(ordinary, shoal.ErrorNotFound) {
		t.Fatalf("ordinary unreadable extraction error = %v", ordinary)
	}
	readable := ingestAuthorizedSkill(t, f.clientA, f.admin(t), "conflict", "foreign-owned-key")
	conflictClient := f.newClient(t, f.base, conflictPutNodeStore{PolicyStore: f.store}, f.sourceA, f.policyA, nil)
	_, conflict := conflictClient.ExtractDocument(f.alice(t), explorer.ExtractionRequest{
		DocumentID: readable.Document.ID,
		RevisionID: readable.Revision.ID,
		Version:    authorizedSkillsOntologyVersion(t),
	})
	if fmt.Sprint(conflict) != fmt.Sprint(ordinary) ||
		!shoal.IsErrorCode(conflict, shoal.ErrorNotFound) {
		t.Fatalf("conflict error %q differs from ordinary unreadable error %q",
			conflict, ordinary)
	}
}

func TestExtractDocumentRegistrationFailureDoesNotCommitGraph(t *testing.T) {
	f := newFixture(t)
	readable := ingestAuthorizedSkill(t, f.clientA, f.admin(t), "atomic", "atomic-cli")
	failing := f.newClient(
		t, f.base,
		failingPutNodeStore{
			PolicyStore: f.store,
			err:         shoal.NewError(shoal.ErrorUnavailable, "forced put node failure"),
		},
		f.sourceA, f.policyA, nil,
	)
	_, err := failing.ExtractDocument(f.alice(t), explorer.ExtractionRequest{
		DocumentID: readable.Document.ID,
		RevisionID: readable.Revision.ID,
		Version:    authorizedSkillsOntologyVersion(t),
	})
	if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("forced registration failure error = %v", err)
	}
	raw, err := f.base.Neighborhood(context.Background(), explorer.NeighborhoodRequest{
		NodeIDs: []shoal.ID{readable.Document.ID}, Depth: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := countNodesWithEntityKey(raw.Nodes, "atomic_cli"); got != 0 {
		t.Fatalf("registration failure committed %d orphaned derived node(s)", got)
	}
}

func TestExtractDocumentAuthorizationControlsDerivedGraph(t *testing.T) {
	f := newFixture(t)
	visible, err := f.clientA.Ingest(f.admin(t), explorer.Source{
		URI: "file:///visible.md", MediaType: explorer.MediaTypeMarkdown,
		Content: "# Visible\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	hidden, err := f.clientB.Ingest(f.admin(t), explorer.Source{
		URI: "file:///hidden/SKILL.md", MediaType: explorer.MediaTypeMarkdown,
		Content: authorizedSkillMarkdown,
	})
	if err != nil {
		t.Fatal(err)
	}
	version := authorizedSkillsOntologyVersion(t)
	extracted, err := f.clientB.ExtractDocument(f.bob(t), explorer.ExtractionRequest{
		DocumentID: hidden.Document.ID, RevisionID: hidden.Revision.ID,
		Version: version,
	})
	if err != nil {
		t.Fatal(err)
	}
	if extracted.EntityCount == 0 || len(extracted.EntityNodeIDs) == 0 {
		t.Fatalf("authorized extraction produced no entities: %+v", extracted)
	}
	bobGraph, err := f.clientB.Neighborhood(f.bob(t), explorer.NeighborhoodRequest{
		NodeIDs: []shoal.ID{hidden.Document.ID}, Depth: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !hasNode(bobGraph, extracted.EntityNodeIDs[0]) {
		t.Fatalf("authorized reader did not see extracted entity %s", extracted.EntityNodeIDs[0])
	}
	if countAuthorizedEdgesOfType(bobGraph.Edges, "provides_capability") != 1 {
		t.Fatalf("authorized reader relationship edges = %+v, want provides_capability", bobGraph.Edges)
	}
	if len(bobGraph.Assertions) != 2 {
		t.Fatalf("authorized reader relationship assertions = %d, want 2", len(bobGraph.Assertions))
	}
	bobSeedGraph, err := f.clientB.Neighborhood(f.bob(t), explorer.NeighborhoodRequest{
		NodeIDs: []shoal.ID{extracted.EntityNodeIDs[0]}, Depth: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !hasNode(bobSeedGraph, extracted.EntityNodeIDs[0]) {
		t.Fatalf("authorized direct seed missed extracted entity %s", extracted.EntityNodeIDs[0])
	}
	err = f.clientA.Connect(f.admin(t), graph.Edge{
		ID:   appEdgeID(t, f.clientA, f.admin(t), "visible-to-hidden-extracted-skill"),
		From: visible.Document.ID,
		To:   extracted.EntityNodeIDs[0], Type: "mentions", Weight: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	aliceGraph, err := f.clientA.Neighborhood(f.alice(t), explorer.NeighborhoodRequest{
		NodeIDs: []shoal.ID{visible.Document.ID}, Depth: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if hasNode(aliceGraph, extracted.EntityNodeIDs[0]) ||
		countAuthorizedEdgesOfType(aliceGraph.Edges, "mentions") != 0 {
		t.Fatalf("unauthorized extracted entity leaked through endpoint filtering: %+v", aliceGraph)
	}
	if _, err := f.clientA.Neighborhood(f.alice(t), explorer.NeighborhoodRequest{
		NodeIDs: []shoal.ID{extracted.EntityNodeIDs[0]}, Depth: 1,
	}); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("unauthorized extracted entity seed error = %v", err)
	}
}

func TestExtractDocumentRejectsUnreadableSource(t *testing.T) {
	f := newFixture(t)
	hidden, err := f.clientB.Ingest(f.admin(t), explorer.Source{
		URI: "file:///hidden/SKILL.md", MediaType: explorer.MediaTypeMarkdown,
		Content: authorizedSkillMarkdown,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.clientA.ExtractDocument(f.alice(t), explorer.ExtractionRequest{
		DocumentID: hidden.Document.ID, RevisionID: hidden.Revision.ID,
		Version: authorizedSkillsOntologyVersion(t),
	})
	if !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("unreadable source extraction error = %v", err)
	}
}

func authorizedSkillsOntologyVersion(t *testing.T) ontology.OntologyVersion {
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
	dependsOn, err := ontology.NewRelationshipDefinition(
		"depends_on", "Depends on", "Skill depends on another skill",
		[]shoal.ID{skill.ID()}, []shoal.ID{skill.ID()}, nil, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	schema, err := ontology.NewOntologySchema(
		"agent-skills-auth", "Agent Skills Auth", "Authorized skill ontology", nil)
	if err != nil {
		t.Fatal(err)
	}
	version, err := ontology.NewOntologyVersion(
		schema, "v1", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		[]ontology.ConceptDefinition{skill, tool, capability},
		[]ontology.RelationshipDefinition{providesTool, providesCapability, dependsOn},
		[]ontology.PropertyDefinition{name}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	return version
}

func countAuthorizedEdgesOfType(edges []graph.Edge, edgeType string) int {
	count := 0
	for _, edge := range edges {
		if edge.Type == edgeType {
			count++
		}
	}
	return count
}

func ingestAuthorizedSkill(
	t *testing.T,
	client *authorized.Client,
	ctx context.Context,
	name string,
	tool string,
) explorer.IngestResult {
	t.Helper()
	result, err := client.Ingest(ctx, explorer.Source{
		URI:       "file:///" + name + "/SKILL.md",
		MediaType: explorer.MediaTypeMarkdown,
		Content:   "# " + name + " Skill\n\nTools:\n- " + tool + "\n\nCapabilities:\n- " + name + " feature\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func extractedNodeWithKey(t *testing.T, nodes []graph.Node, key string) shoal.ID {
	t.Helper()
	for _, node := range nodes {
		if node.Properties[extraction.GraphPropertyEntityKey] == key {
			return node.ID
		}
	}
	t.Fatalf("node with entity key %q not found in %+v", key, nodes)
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

type conflictPutNodeStore struct {
	authorized.PolicyStore
}

func (s conflictPutNodeStore) PutNode(
	context.Context,
	shoal.ID,
	authorized.NodeRegistration,
) error {
	return shoal.NewError(shoal.ErrorConflict, "authorization policy catalog conflict")
}

type failingPutNodeStore struct {
	authorized.PolicyStore
	err error
}

func (s failingPutNodeStore) PutNode(
	context.Context,
	shoal.ID,
	authorized.NodeRegistration,
) error {
	return s.err
}
