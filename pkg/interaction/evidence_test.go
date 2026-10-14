package interaction_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestDocumentEvidenceRetainsResolvedSourceRoles(t *testing.T) {
	citation := document.Citation{
		DocumentID: "document",
		RevisionID: "revision",
		SpanID:     "span",
		Range: document.SourceRange{
			Start: document.SourcePosition{Offset: 0},
			End:   document.SourcePosition{Offset: 1},
		},
	}
	reference := interaction.EvidenceReference{
		AnchorID: "anchor",
		Kind:     interaction.EvidenceDocument,
		Citation: citation,
		NodeIDs:  []shoal.ID{"span", "document", "section"},
	}
	canonical, err := reference.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if canonical.Citation.SectionID != "" ||
		!reflect.DeepEqual(
			canonical.NodeIDs,
			[]shoal.ID{"document", "section", "span"},
		) {
		t.Fatalf("canonical document evidence = %+v", canonical)
	}

	incomplete := reference
	incomplete.NodeIDs = append(incomplete.NodeIDs, "untrusted-extra")
	if err := incomplete.Validate(); err == nil {
		t.Fatal("too many resolved source nodes were accepted")
	}
}

func TestSessionCanonicalPreservesExactEvidence(t *testing.T) {
	evidence := interaction.EvidenceReference{
		AnchorID: "anchor-1", Kind: interaction.EvidenceGraph,
		NodeIDs: []shoal.ID{"node-b", "node-a"},
		EdgeIDs: []shoal.ID{"edge-1"},
		Assertions: []interaction.AssertionReference{{
			AssertionID: "assertion-1", EdgeID: "edge-1",
			Origin: ontology.AssertionInferred,
		}},
	}
	session := interaction.Session{
		ID:           interaction.DerivedID("session", "exact-evidence"),
		RecordedAt:   time.Unix(1700000000, 0).UTC(),
		Operation:    interaction.OperationRetrieval,
		SeedNodeIDs:  []shoal.ID{"node-a", "node-b"},
		SeedEvidence: []interaction.EvidenceReference{evidence},
	}
	canonical, err := session.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if len(canonical.SeedEvidence) != 1 ||
		len(canonical.SeedEvidence[0].Assertions) != 1 ||
		canonical.SeedEvidence[0].Assertions[0].Origin !=
			ontology.AssertionInferred ||
		len(canonical.TouchedEdgeIDs()) != 1 {
		t.Fatalf("canonical evidence = %+v", canonical.SeedEvidence)
	}
	bad := evidence
	bad.Assertions[0].EdgeID = "missing-edge"
	session.SeedEvidence = []interaction.EvidenceReference{bad}
	if err := session.Validate(); err == nil {
		t.Fatal("assertion detached from its authoritative edge was accepted")
	}
}

func TestSessionSubgraphRequiresAndConjoinsExactEdgeVisibility(t *testing.T) {
	session := interaction.Session{
		ID:          interaction.DerivedID("session", "edge-visibility"),
		RecordedAt:  time.Unix(1700000000, 0).UTC(),
		Operation:   interaction.OperationRetrieval,
		SeedNodeIDs: []shoal.ID{"node-a", "node-b"},
		SeedEvidence: []interaction.EvidenceReference{{
			AnchorID: "anchor-1", Kind: interaction.EvidenceGraph,
			NodeIDs: []shoal.ID{"node-a", "node-b"},
			EdgeIDs: []shoal.ID{"edge-1"},
		}},
	}
	nodeResolver := func(shoal.ID) ([]string, error) {
		return []string{"node-label"}, nil
	}
	if _, err := session.Subgraph(nodeResolver); err == nil {
		t.Fatal("edge-backed evidence was accepted without an edge resolver")
	}
	subgraph, err := session.SubgraphWithEvidence(
		nodeResolver,
		func(id shoal.ID) ([]string, error) {
			if id != "edge-1" {
				t.Fatalf("resolved unexpected edge %q", id)
			}
			return []string{"edge-label"}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := interaction.Expression(subgraph.Visibility); got !=
		"edge-label&node-label" {
		t.Fatalf("visibility = %q", got)
	}
	if len(subgraph.TouchedEdgeIDs) != 1 ||
		subgraph.TouchedEdgeIDs[0] != "edge-1" {
		t.Fatalf("touched edges = %v", subgraph.TouchedEdgeIDs)
	}
}

func TestSessionRejectsEvidenceWithoutDeclaredSourceNodes(t *testing.T) {
	recordedAt := time.Unix(1700000000, 0).UTC()
	seeded := interaction.Session{
		ID:         interaction.DerivedID("session", "seed-evidence-only"),
		RecordedAt: recordedAt,
		Operation:  interaction.OperationRetrieval,
		SeedEvidence: []interaction.EvidenceReference{{
			AnchorID: "anchor-1", Kind: interaction.EvidenceGraph,
			NodeIDs: []shoal.ID{"restricted-node"},
		}},
	}
	if err := seeded.Validate(); err == nil {
		t.Fatal("seed evidence without declared seed nodes was accepted")
	}
	cited := interaction.Session{
		ID:         interaction.DerivedID("session", "cited-evidence-only"),
		RecordedAt: recordedAt,
		Operation:  interaction.OperationRetrieval,
		CitedEvidence: []interaction.EvidenceReference{{
			AnchorID: "anchor-1", Kind: interaction.EvidenceGraph,
			NodeIDs: []shoal.ID{"restricted-node"},
		}},
	}
	if err := cited.Validate(); err == nil {
		t.Fatal("cited evidence without declared cited nodes was accepted")
	}
	invalidEvidence := interaction.Session{
		ID:         interaction.DerivedID("session", "invalid-evidence"),
		RecordedAt: recordedAt,
		Operation:  interaction.OperationRetrieval,
		SeedEvidence: []interaction.EvidenceReference{{
			Kind: interaction.EvidenceGraph,
		}},
	}
	if err := invalidEvidence.Validate(); err == nil {
		t.Fatal("unvalidated seed evidence was accepted")
	}
}

func TestSessionRejectsConflictingEvidenceAnchorAcrossGroups(t *testing.T) {
	session := interaction.Session{
		ID:          interaction.DerivedID("session", "anchor-conflict"),
		RecordedAt:  time.Unix(1700000000, 0).UTC(),
		Operation:   interaction.OperationRetrieval,
		SeedNodeIDs: []shoal.ID{"node-a"},
		SeedEvidence: []interaction.EvidenceReference{{
			AnchorID: "anchor-x", Kind: interaction.EvidenceGraph,
			NodeIDs: []shoal.ID{"node-a"},
		}},
		CitedNodeIDs: []shoal.ID{"node-b"},
		CitedEvidence: []interaction.EvidenceReference{{
			AnchorID: "anchor-x", Kind: interaction.EvidenceGraph,
			NodeIDs: []shoal.ID{"node-b"},
		}},
	}
	if err := session.Validate(); err == nil {
		t.Fatal("conflicting evidence anchor across groups was accepted")
	}
	if _, err := session.EvidenceReferences(); err == nil {
		t.Fatal("ambiguous evidence union was reported as absent evidence")
	}
}
