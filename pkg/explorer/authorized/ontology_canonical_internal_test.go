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

package authorized

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/extraction"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// TestProposalEvidenceIsExactAtCreateAndCanonicalWhenStored: an ontology
// proposal's graph evidence over an extracted node must match the catalog's
// canonical content exactly when it is proposed, so a proposer cannot store
// and serve its own kind, labels or properties on the node (#700). A
// proposal stored before shared content was canonical names the node in its
// old rendering, and stays valid.
func TestProposalEvidenceIsExactAtCreateAndCanonicalWhenStored(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, time.September, 6, 0, 0, 0, 0, time.UTC)
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	policy, err := auth.NewPolicy(auth.PolicyConfig{
		AuthorizationDomain: []byte("domain"), SourceID: []byte("source-a"),
		GrantPolicyID: []byte("policy"), Epoch: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	rule, err := NewAccessRule(policy)
	if err != nil {
		t.Fatal(err)
	}
	policyStore := NewMemoryPolicyStore()
	view, _ := registerOntologyEvidenceDocument(
		t, policyStore, "document-a", "revision-a", "section-a", "contains-a", rule, at)
	entity := shoal.ID("inferred-entity:" + strings.Repeat("0", 63) + "1")
	canonical, _ := explorer.CanonicalExtractedNode(graph.Node{
		ID: entity, Kind: "ontology:skill", Labels: []string{"skill", "zeta"},
		Properties: shoal.Metadata{
			extraction.GraphPropertyOntologyConceptID:  "concept:skill",
			extraction.GraphPropertyOntologyConceptKey: "skill",
			extraction.GraphPropertyEntityKey:          "zeta",
		},
	})
	if err := policyStore.PutNode(ctx, entity, NodeRegistration{
		DocumentID: "document-a", RevisionID: "revision-a", Node: canonical,
		Rule: rule, Kind: RegistrationExtracted,
	}); err != nil {
		t.Fatal(err)
	}
	base := &ontologyEvidenceBase{
		Explorer: corpus,
		views:    map[shoal.ID]explorer.DocumentView{view.Document.ID: view},
	}
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "subject", Actor: "actor", AuthorizationDomain: []byte("domain"),
		AllowedOperations:     []auth.Operation{auth.OperationRead, auth.OperationIngest},
		PermittedSourceIDs:    [][]byte{[]byte("source-a")},
		PermittedPolicyIDs:    [][]byte{[]byte("policy")},
		PolicyGeneration:      1,
		AuthenticationExpires: at.Add(time.Hour),
		RequestID:             "request",
	})
	if err != nil {
		t.Fatal(err)
	}
	selector, err := NewStaticPolicySelector([]byte("source-a"), []byte("policy"))
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(Config{
		Base:                  &untrustedOntologyEvidenceBase{ontologyEvidenceBase: base},
		OntologyProposalStore: base,
		Resolver: resolverFunc(func(context.Context) (auth.Decision, error) {
			return decision, nil
		}),
		PolicySelector: selector, PolicyStore: policyStore,
		GenerationReader: generationReaderFunc(
			func(context.Context, []byte) (int64, error) { return 1, nil }),
		Clock: func() time.Time { return at.Add(time.Minute) },
	})
	if err != nil {
		t.Fatal(err)
	}
	schema, baseVersion := ontologyEvidenceVersions(t, at)
	withNode := func(node graph.Node, version int) (proposalID shoal.ID, create func() error, store func() error) {
		path := graph.Path{Nodes: []graph.Node{node}}
		evidence := ontologyEvidenceRef(t, view, &path)
		proposal, _ := ontologyEvidenceProposal(t, schema, baseVersion, version, evidence, at)
		return proposal.ID(),
			func() error { return client.CreateOntologyProposal(ctx, proposal, baseVersion) },
			func() error { return corpus.CreateOntologyProposal(ctx, proposal, baseVersion) }
	}

	_, create, _ := withNode(canonical, 2)
	if err := create(); err != nil {
		t.Fatalf("a proposal over the canonical node: %v", err)
	}
	forged := cloneGraphNode(canonical)
	forged.Kind = "ontology:anything"
	forged.Labels = []string{"APPROVED BY SECURITY"}
	forged.Properties["reviewer"] = "security"
	_, create, _ = withNode(forged, 3)
	if err := create(); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("a proposal presenting its own content on an extracted node = %v, want refused", err)
	}
	legacy := cloneGraphNode(canonical)
	legacy.Labels = []string{"Zeta", "skill"}
	legacy.Properties["confidence"] = "0.500000"
	legacyID, _, store := withNode(legacy, 4)
	if err := store(); err != nil {
		t.Fatal(err)
	}
	visible, err := client.OntologyProposals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, proposal := range visible {
		found = found || proposal.ID() == legacyID
	}
	if !found {
		t.Fatal("a proposal stored over the old rendering of an extracted node is no longer valid")
	}
}
