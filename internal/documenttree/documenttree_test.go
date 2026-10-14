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

package documenttree

import (
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/internal/documentschema"
	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const testRevision = shoal.ID("revision-7")

func TestReconstructNestedOrderedTree(t *testing.T) {
	input := testInput(
		[]NodeRecord{
			nodeRecord("nested", documentschema.StructureSection, "root", 2, 20, 90),
			nodeRecord("nested-span-b", documentschema.StructureSpan, "nested", 4, 50, 80),
			nodeRecord("root", documentschema.StructureSection, "", 0, 0, 100),
			nodeRecord("root-span", documentschema.StructureSpan, "root", 1, 5, 15),
			nodeRecord("nested-span-a", documentschema.StructureSpan, "nested", 1, 25, 40),
		},
		[]ChildRecord{
			childRecord("nested", 4, "nested-span-b"),
			childRecord("root", 2, "nested"),
			childRecord("", 0, "root"),
			childRecord("nested", 1, "nested-span-a"),
			childRecord("root", 1, "root-span"),
		},
	)

	root, err := Reconstruct(input, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if root.Section.ID != "root" ||
		root.Section.DocumentID != "document-1" ||
		root.Section.RevisionID != testRevision {
		t.Fatalf("root section = %+v", root.Section)
	}
	if len(root.Children) != 2 {
		t.Fatalf("root children = %d, want 2", len(root.Children))
	}
	if root.Children[0].Span == nil || root.Children[0].Span.ID != "root-span" {
		t.Fatalf("first root child = %+v, want root-span", root.Children[0])
	}
	nested := root.Children[1].Section
	if nested == nil || nested.Section.ID != "nested" {
		t.Fatalf("second root child = %+v, want nested section", root.Children[1])
	}
	if len(nested.Children) != 2 ||
		nested.Children[0].Span == nil || nested.Children[0].Span.ID != "nested-span-a" ||
		nested.Children[1].Span == nil || nested.Children[1].Span.ID != "nested-span-b" {
		t.Fatalf("nested children are not in structural order: %+v", nested.Children)
	}
}

func TestReconstructRejectsMalformedRecords(t *testing.T) {
	t.Run("node qualifier", func(t *testing.T) {
		input := minimalInput()
		input.Nodes[0].Qualifier = documentschema.EventCQ("TITLE", "root")
		requireErrorContains(t, input, Limits{}, "malformed node qualifier")
	})
	t.Run("node value", func(t *testing.T) {
		input := minimalInput()
		input.Nodes[0].Value = []byte{1}
		requireErrorContains(t, input, Limits{}, "decode node")
	})
	t.Run("child qualifier", func(t *testing.T) {
		input := minimalInput()
		input.Children[0].Qualifier = documentschema.StructureNodeCQ(string(testRevision), "root")
		requireErrorContains(t, input, Limits{}, "malformed child qualifier")
	})
}

func TestReconstructRejectsOrphans(t *testing.T) {
	t.Run("child without node", func(t *testing.T) {
		input := minimalInput()
		input.Children = append(input.Children, childRecord("root", 1, "missing"))
		requireErrorContains(t, input, Limits{}, "orphan child record")
	})
	t.Run("node without child record", func(t *testing.T) {
		input := minimalInput()
		input.Nodes = append(input.Nodes,
			nodeRecord("orphan", documentschema.StructureSpan, "root", 1, 1, 2))
		requireErrorContains(t, input, Limits{}, "orphan node")
	})
	t.Run("missing parent", func(t *testing.T) {
		input := minimalInput()
		input.Nodes = append(input.Nodes,
			nodeRecord("orphan", documentschema.StructureSpan, "missing", 1, 1, 2))
		input.Children = append(input.Children, childRecord("missing", 1, "orphan"))
		requireErrorContains(t, input, Limits{}, "orphan parent")
	})
}

func TestReconstructRejectsCycle(t *testing.T) {
	input := testInput(
		[]NodeRecord{
			nodeRecord("root", documentschema.StructureSection, "", 0, 0, 100),
			nodeRecord("a", documentschema.StructureSection, "b", 0, 10, 90),
			nodeRecord("b", documentschema.StructureSection, "a", 0, 10, 90),
		},
		[]ChildRecord{
			childRecord("", 0, "root"),
			childRecord("b", 0, "a"),
			childRecord("a", 0, "b"),
		},
	)
	requireErrorContains(t, input, Limits{}, "cycle")
}

func TestReconstructEnforcesConsistency(t *testing.T) {
	t.Run("duplicate stable ID", func(t *testing.T) {
		input := minimalInput()
		input.Nodes = append(input.Nodes, input.Nodes[0])
		requireErrorContains(t, input, Limits{}, "duplicate stable node ID")
	})
	t.Run("parent mismatch", func(t *testing.T) {
		input := testInput(
			[]NodeRecord{
				nodeRecord("root", documentschema.StructureSection, "", 0, 0, 100),
				nodeRecord("span", documentschema.StructureSpan, "root", 1, 1, 2),
			},
			[]ChildRecord{
				childRecord("", 0, "root"),
				childRecord("other", 1, "span"),
			},
		)
		requireErrorContains(t, input, Limits{}, "inconsistent with its node")
	})
	t.Run("duplicate sibling order", func(t *testing.T) {
		input := testInput(
			[]NodeRecord{
				nodeRecord("root", documentschema.StructureSection, "", 0, 0, 100),
				nodeRecord("a", documentschema.StructureSpan, "root", 1, 1, 2),
				nodeRecord("b", documentschema.StructureSpan, "root", 1, 3, 4),
			},
			[]ChildRecord{
				childRecord("", 0, "root"),
				childRecord("root", 1, "a"),
				childRecord("root", 1, "b"),
			},
		)
		requireErrorContains(t, input, Limits{}, "share order")
	})
	t.Run("source outside parent", func(t *testing.T) {
		input := testInput(
			[]NodeRecord{
				nodeRecord("root", documentschema.StructureSection, "", 0, 10, 20),
				nodeRecord("span", documentschema.StructureSpan, "root", 1, 5, 15),
			},
			[]ChildRecord{
				childRecord("", 0, "root"),
				childRecord("root", 1, "span"),
			},
		)
		requireErrorContains(t, input, Limits{}, "outside parent")
	})
	t.Run("known child start page after parent", func(t *testing.T) {
		input := testInput(
			[]NodeRecord{
				nodeRecordWithPages(
					"root", documentschema.StructureSection, "", 0, 10, 20, 1, 2),
				nodeRecordWithPages(
					"span", documentschema.StructureSpan, "root", 1, 12, 18, 3, 0),
			},
			[]ChildRecord{
				childRecord("", 0, "root"),
				childRecord("root", 1, "span"),
			},
		)
		requireErrorContains(t, input, Limits{}, "outside parent")
	})
	t.Run("known child end page before parent", func(t *testing.T) {
		input := testInput(
			[]NodeRecord{
				nodeRecordWithPages(
					"root", documentschema.StructureSection, "", 0, 10, 20, 2, 3),
				nodeRecordWithPages(
					"span", documentschema.StructureSpan, "root", 1, 12, 18, 0, 1),
			},
			[]ChildRecord{
				childRecord("", 0, "root"),
				childRecord("root", 1, "span"),
			},
		)
		requireErrorContains(t, input, Limits{}, "outside parent")
	})
	t.Run("mixed revision", func(t *testing.T) {
		input := minimalInput()
		input.Children[0].Qualifier =
			documentschema.StructureChildCQ("revision-8", "", 0, "root")
		requireErrorContains(t, input, Limits{}, "has revision")
	})
}

func TestReconstructRevisionNamespaceSupportsRemovalAndReordering(t *testing.T) {
	revision1 := shoal.ID("revision-1")
	input1 := testInputForRevision(
		revision1,
		[]NodeRecord{
			nodeRecordForRevision(
				revision1, "root", documentschema.StructureSection, "", 0, 0, 100),
			nodeRecordForRevision(
				revision1, "a", documentschema.StructureSpan, "root", 1, 10, 20),
			nodeRecordForRevision(
				revision1, "b", documentschema.StructureSpan, "root", 2, 30, 40),
		},
		[]ChildRecord{
			childRecordForRevision(revision1, "", 0, "root"),
			childRecordForRevision(revision1, "root", 1, "a"),
			childRecordForRevision(revision1, "root", 2, "b"),
		},
	)

	revision2 := shoal.ID("revision-2")
	input2 := testInputForRevision(
		revision2,
		[]NodeRecord{
			nodeRecordForRevision(
				revision2, "root", documentschema.StructureSection, "", 0, 0, 100),
			nodeRecordForRevision(
				revision2, "b", documentschema.StructureSpan, "root", 1, 30, 40),
		},
		[]ChildRecord{
			childRecordForRevision(revision2, "", 0, "root"),
			childRecordForRevision(revision2, "root", 1, "b"),
		},
	)

	root1, err := Reconstruct(input1, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	root2, err := Reconstruct(input2, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(root1.Children) != 2 ||
		root1.Children[0].Span.ID != "a" || root1.Children[1].Span.ID != "b" {
		t.Fatalf("revision 1 children = %+v", root1.Children)
	}
	if len(root2.Children) != 1 || root2.Children[0].Span == nil ||
		root2.Children[0].Span.ID != "b" || root2.Children[0].Span.Order != 1 {
		t.Fatalf("revision 2 children = %+v", root2.Children)
	}
	if root1.Children[1].Span.RevisionID != revision1 ||
		root2.Children[0].Span.RevisionID != revision2 {
		t.Fatal("reconstructed spans lost their explicit revision identity")
	}
	if string(input1.Children[1].Qualifier) == string(input2.Children[1].Qualifier) {
		t.Fatal("reordered child relationship reused the prior revision qualifier")
	}
}

func TestReconstructEnforcesLimits(t *testing.T) {
	input := testInput(
		[]NodeRecord{
			nodeRecord("root", documentschema.StructureSection, "", 0, 0, 100),
			nodeRecord("nested", documentschema.StructureSection, "root", 0, 10, 90),
		},
		[]ChildRecord{
			childRecord("", 0, "root"),
			childRecord("root", 0, "nested"),
		},
	)
	requireErrorContains(t, input, Limits{MaxNodes: 1}, "node count")
	requireErrorContains(t, input, Limits{MaxDepth: 1}, "depth")
}

func minimalInput() Input {
	return testInput(
		[]NodeRecord{
			nodeRecord("root", documentschema.StructureSection, "", 0, 0, 100),
		},
		[]ChildRecord{
			childRecord("", 0, "root"),
		},
	)
}

func testInput(nodes []NodeRecord, children []ChildRecord) Input {
	return testInputForRevision(testRevision, nodes, children)
}

func testInputForRevision(
	revisionID shoal.ID, nodes []NodeRecord, children []ChildRecord,
) Input {
	return Input{
		Document: document.Document{
			ID:            "document-1",
			RevisionID:    revisionID,
			RootSectionID: "root",
		},
		Nodes:    nodes,
		Children: children,
	}
}

func nodeRecord(
	id string, kind documentschema.StructureKind, parent string, order uint32,
	start, end uint64,
) NodeRecord {
	return nodeRecordForRevision(testRevision, id, kind, parent, order, start, end)
}

func nodeRecordWithPages(
	id string, kind documentschema.StructureKind, parent string, order uint32,
	start, end uint64, startPage, endPage uint32,
) NodeRecord {
	return NodeRecord{
		Qualifier: documentschema.StructureNodeCQ(string(testRevision), id),
		Value: documentschema.StructureNode{
			Kind:        kind,
			ParentID:    parent,
			Order:       order,
			StartOffset: start,
			EndOffset:   end,
			StartPage:   startPage,
			EndPage:     endPage,
		}.Encode(),
	}
}

func nodeRecordForRevision(
	revisionID shoal.ID, id string, kind documentschema.StructureKind, parent string,
	order uint32, start, end uint64,
) NodeRecord {
	return NodeRecord{
		Qualifier: documentschema.StructureNodeCQ(string(revisionID), id),
		Value: documentschema.StructureNode{
			Kind:        kind,
			ParentID:    parent,
			Order:       order,
			StartOffset: start,
			EndOffset:   end,
		}.Encode(),
	}
}

func childRecord(parent string, order uint32, child string) ChildRecord {
	return childRecordForRevision(testRevision, parent, order, child)
}

func childRecordForRevision(
	revisionID shoal.ID, parent string, order uint32, child string,
) ChildRecord {
	return ChildRecord{
		Qualifier: documentschema.StructureChildCQ(
			string(revisionID), parent, order, child),
	}
}

func requireErrorContains(t *testing.T, input Input, limits Limits, want string) {
	t.Helper()
	_, err := Reconstruct(input, limits)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Reconstruct() error = %v, want containing %q", err, want)
	}
}
