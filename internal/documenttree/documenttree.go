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

// Package documenttree reconstructs bounded hierarchical documents from
// documentschema structural records.
package documenttree

import (
	"fmt"
	"sort"

	"github.com/phrocker/shoal-oss/internal/documentschema"
	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	DefaultMaxNodes = 10_000
	DefaultMaxDepth = 64
)

// Limits bounds the resources used while reconstructing a document tree. Zero
// values select the package defaults.
type Limits struct {
	MaxNodes int
	MaxDepth int
}

// NodeRecord is one revision-specific structural node cell.
type NodeRecord struct {
	Qualifier []byte
	Value     []byte
}

// ChildRecord is one revision-specific parent-to-child structural cell.
type ChildRecord struct {
	Qualifier []byte
}

// Input contains the revision-scoped records needed to reconstruct one
// document.
type Input struct {
	Document document.Document
	Nodes    []NodeRecord
	Children []ChildRecord
}

// SectionNode is a section and its ordered structural children.
type SectionNode struct {
	Section  document.Section
	Children []Child
}

// Child contains exactly one nested section or span.
type Child struct {
	Section *SectionNode
	Span    *document.Span
}

type structuralNode struct {
	id      shoal.ID
	kind    documentschema.StructureKind
	parent  shoal.ID
	order   uint32
	source  document.SourceRange
	section document.Section
	span    document.Span
}

type depthNode struct {
	node  *structuralNode
	depth int
}

// Reconstruct validates and materializes one revision-specific document tree.
func Reconstruct(input Input, limits Limits) (*SectionNode, error) {
	limits, err := normalizeLimits(limits)
	if err != nil {
		return nil, err
	}
	if input.Document.ID == "" {
		return nil, fmt.Errorf("documenttree: document ID is required")
	}
	if input.Document.RevisionID == "" {
		return nil, fmt.Errorf("documenttree: revision ID is required")
	}
	if input.Document.RootSectionID == "" {
		return nil, fmt.Errorf("documenttree: root section ID is required")
	}
	if len(input.Nodes) == 0 {
		return nil, fmt.Errorf("documenttree: no structural nodes")
	}
	if len(input.Nodes) > limits.MaxNodes {
		return nil, fmt.Errorf(
			"documenttree: node count %d exceeds limit %d", len(input.Nodes), limits.MaxNodes)
	}
	if len(input.Children) > limits.MaxNodes {
		return nil, fmt.Errorf(
			"documenttree: child count %d exceeds limit %d", len(input.Children), limits.MaxNodes)
	}

	nodes := make(map[shoal.ID]*structuralNode, len(input.Nodes))
	nodeList := make([]*structuralNode, 0, len(input.Nodes))
	for i, record := range input.Nodes {
		revisionID, nodeID, ok := documentschema.ParseStructureNodeCQ(record.Qualifier)
		if !ok {
			return nil, fmt.Errorf("documenttree: malformed node qualifier at record %d", i)
		}
		if shoal.ID(revisionID) != input.Document.RevisionID {
			return nil, fmt.Errorf(
				"documenttree: node record %d has revision %q, want %q",
				i, revisionID, input.Document.RevisionID)
		}
		if nodeID == "" {
			return nil, fmt.Errorf("documenttree: node record %d has an empty stable ID", i)
		}
		id := shoal.ID(nodeID)
		if _, exists := nodes[id]; exists {
			return nil, fmt.Errorf("documenttree: duplicate stable node ID %q", id)
		}
		encoded, err := documentschema.DecodeStructureNode(record.Value)
		if err != nil {
			return nil, fmt.Errorf("documenttree: decode node %q: %w", id, err)
		}
		source, err := sourceRange(encoded)
		if err != nil {
			return nil, fmt.Errorf("documenttree: node %q: %w", id, err)
		}

		node := &structuralNode{
			id:     id,
			kind:   encoded.Kind,
			parent: shoal.ID(encoded.ParentID),
			order:  encoded.Order,
			source: source,
		}
		switch node.kind {
		case documentschema.StructureSection:
			node.section = document.Section{
				ID:         id,
				DocumentID: input.Document.ID,
				RevisionID: input.Document.RevisionID,
				ParentID:   node.parent,
				Order:      node.order,
				Range:      source,
			}
		case documentschema.StructureSpan:
			node.span = document.Span{
				ID:         id,
				DocumentID: input.Document.ID,
				RevisionID: input.Document.RevisionID,
				SectionID:  node.parent,
				Order:      node.order,
				Range:      source,
			}
		}
		nodes[id] = node
		nodeList = append(nodeList, node)
	}

	root, ok := nodes[input.Document.RootSectionID]
	if !ok {
		return nil, fmt.Errorf(
			"documenttree: root section %q has no node record", input.Document.RootSectionID)
	}
	if root.kind != documentschema.StructureSection {
		return nil, fmt.Errorf(
			"documenttree: root %q is not a section", input.Document.RootSectionID)
	}
	if root.parent != "" {
		return nil, fmt.Errorf(
			"documenttree: root section %q has parent %q", root.id, root.parent)
	}

	childrenByParent := make(map[shoal.ID][]*structuralNode)
	relations := make(map[shoal.ID]struct{}, len(input.Children))
	ordersByParent := make(map[shoal.ID]map[uint32]shoal.ID)
	for i, record := range input.Children {
		revisionID, parentID, order, childID, ok :=
			documentschema.ParseStructureChildCQ(record.Qualifier)
		if !ok {
			return nil, fmt.Errorf("documenttree: malformed child qualifier at record %d", i)
		}
		if shoal.ID(revisionID) != input.Document.RevisionID {
			return nil, fmt.Errorf(
				"documenttree: child record %d has revision %q, want %q",
				i, revisionID, input.Document.RevisionID)
		}
		if childID == "" {
			return nil, fmt.Errorf("documenttree: child record %d has an empty stable ID", i)
		}
		child, exists := nodes[shoal.ID(childID)]
		if !exists {
			return nil, fmt.Errorf(
				"documenttree: orphan child record %q has no node", childID)
		}
		if _, exists := relations[child.id]; exists {
			return nil, fmt.Errorf(
				"documenttree: node %q has multiple child records", child.id)
		}
		parent := shoal.ID(parentID)
		if child.parent != parent || child.order != order {
			return nil, fmt.Errorf(
				"documenttree: child record for %q is inconsistent with its node", child.id)
		}
		if parent == "" {
			if child.id != input.Document.RootSectionID {
				return nil, fmt.Errorf(
					"documenttree: non-root node %q has an empty parent", child.id)
			}
		} else {
			parentNode, exists := nodes[parent]
			if !exists {
				return nil, fmt.Errorf(
					"documenttree: node %q has orphan parent %q", child.id, parent)
			}
			if parentNode.kind != documentschema.StructureSection {
				return nil, fmt.Errorf(
					"documenttree: node %q has non-section parent %q", child.id, parent)
			}
			if !rangeContains(parentNode.source, child.source) {
				return nil, fmt.Errorf(
					"documenttree: node %q source range is outside parent %q", child.id, parent)
			}
		}
		if ordersByParent[parent] == nil {
			ordersByParent[parent] = make(map[uint32]shoal.ID)
		}
		if sibling, exists := ordersByParent[parent][order]; exists {
			return nil, fmt.Errorf(
				"documenttree: siblings %q and %q share order %d", sibling, child.id, order)
		}
		ordersByParent[parent][order] = child.id
		childrenByParent[parent] = append(childrenByParent[parent], child)
		relations[child.id] = struct{}{}
	}

	for _, node := range nodeList {
		if _, ok := relations[node.id]; !ok {
			return nil, fmt.Errorf("documenttree: orphan node %q has no child record", node.id)
		}
	}
	for parent := range childrenByParent {
		sort.Slice(childrenByParent[parent], func(i, j int) bool {
			return childrenByParent[parent][i].order < childrenByParent[parent][j].order
		})
	}

	if err := rejectCycles(nodeList, nodes); err != nil {
		return nil, err
	}

	preorder := make([]*structuralNode, 0, len(nodeList))
	visited := make(map[shoal.ID]struct{}, len(nodeList))
	stack := []depthNode{{node: root, depth: 1}}
	for len(stack) > 0 {
		last := len(stack) - 1
		current := stack[last]
		stack = stack[:last]
		if current.depth > limits.MaxDepth {
			return nil, fmt.Errorf(
				"documenttree: depth %d exceeds limit %d at node %q",
				current.depth, limits.MaxDepth, current.node.id)
		}
		if _, exists := visited[current.node.id]; exists {
			return nil, fmt.Errorf("documenttree: cycle reaches node %q", current.node.id)
		}
		visited[current.node.id] = struct{}{}
		preorder = append(preorder, current.node)
		children := childrenByParent[current.node.id]
		for i := len(children) - 1; i >= 0; i-- {
			stack = append(stack, depthNode{node: children[i], depth: current.depth + 1})
		}
	}
	if len(visited) != len(nodes) {
		return nil, fmt.Errorf(
			"documenttree: %d structural nodes are disconnected from root %q",
			len(nodes)-len(visited), root.id)
	}

	sections := make(map[shoal.ID]*SectionNode)
	for i := len(preorder) - 1; i >= 0; i-- {
		node := preorder[i]
		if node.kind != documentschema.StructureSection {
			continue
		}
		section := &SectionNode{
			Section:  node.section,
			Children: make([]Child, 0, len(childrenByParent[node.id])),
		}
		for _, child := range childrenByParent[node.id] {
			if child.kind == documentschema.StructureSection {
				section.Children = append(section.Children, Child{Section: sections[child.id]})
			} else {
				section.Children = append(section.Children, Child{Span: &child.span})
			}
		}
		sections[node.id] = section
	}
	return sections[root.id], nil
}

func normalizeLimits(limits Limits) (Limits, error) {
	if limits.MaxNodes < 0 || limits.MaxDepth < 0 {
		return Limits{}, fmt.Errorf("documenttree: limits cannot be negative")
	}
	if limits.MaxNodes == 0 {
		limits.MaxNodes = DefaultMaxNodes
	}
	if limits.MaxDepth == 0 {
		limits.MaxDepth = DefaultMaxDepth
	}
	return limits, nil
}

func sourceRange(node documentschema.StructureNode) (document.SourceRange, error) {
	const (
		maxOffset = uint64(1<<63 - 1)
		maxPage   = uint32(1<<31 - 1)
	)
	if node.StartOffset > maxOffset || node.EndOffset > maxOffset {
		return document.SourceRange{}, fmt.Errorf("source offset exceeds int64")
	}
	if node.StartPage > maxPage || node.EndPage > maxPage {
		return document.SourceRange{}, fmt.Errorf("source page exceeds int32")
	}
	source := document.SourceRange{
		Start: document.SourcePosition{Offset: int64(node.StartOffset), Page: int32(node.StartPage)},
		End:   document.SourcePosition{Offset: int64(node.EndOffset), Page: int32(node.EndPage)},
	}
	if err := source.Validate(); err != nil {
		return document.SourceRange{}, fmt.Errorf("invalid source range: %w", err)
	}
	return source, nil
}

func rangeContains(parent, child document.SourceRange) bool {
	if child.Start.Offset < parent.Start.Offset || child.End.Offset > parent.End.Offset {
		return false
	}
	return pageWithin(parent, child.Start.Page) && pageWithin(parent, child.End.Page)
}

func pageWithin(parent document.SourceRange, page int32) bool {
	if page == 0 {
		return true
	}
	if parent.Start.Page > 0 && page < parent.Start.Page {
		return false
	}
	return parent.End.Page == 0 || page <= parent.End.Page
}

func rejectCycles(nodeList []*structuralNode, nodes map[shoal.ID]*structuralNode) error {
	const (
		visiting byte = 1
		visited  byte = 2
	)
	state := make(map[shoal.ID]byte, len(nodeList))
	for _, start := range nodeList {
		if state[start.id] != 0 {
			continue
		}
		path := make([]*structuralNode, 0)
		current := start
		for current != nil && state[current.id] == 0 {
			state[current.id] = visiting
			path = append(path, current)
			current = nodes[current.parent]
		}
		if current != nil && state[current.id] == visiting {
			return fmt.Errorf("documenttree: cycle includes node %q", current.id)
		}
		for _, node := range path {
			state[node.id] = visited
		}
	}
	return nil
}
