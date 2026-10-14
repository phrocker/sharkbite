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

// Package codematerializer deterministically maps parser-neutral code
// ingestion values onto Explorer's public document and graph values.
//
// The current graph package has no public association value. Materialization
// therefore exposes additive Association records and emits deterministic
// association edges. It also emits a code-source graph node when an exact
// public relationship endpoint is the source ID; otherwise graph.Edge could
// not preserve that endpoint byte-for-byte. Code attributes are copied only
// when they fit shoal.Metadata and do not collide with materializer-owned
// property keys. Incompatible values return invalid_argument rather than
// being truncated or silently omitted.
package codematerializer

import (
	"fmt"
	pathpkg "path"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	codeast "github.com/phrocker/shoal-oss/pkg/code"
	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	// Version is the Section 6.9 deterministic public materializer version.
	Version = "explorer-code-v1"

	// MaxGraphNodes and MaxGraphEdges mirror the Section 10.5 canonical codec
	// transaction bounds until that shared codec package is available.
	MaxGraphNodes = 1_000_000
	MaxGraphEdges = 1_000_000
	// MaxAssociations mirrors the Section 10.5 canonical association bound.
	MaxAssociations = 2_000_000

	metadataPrefix = "shoal.code."
)

// AssociationTarget identifies whether a citation is attributable to a graph
// node or a graph edge.
type AssociationTarget string

const (
	AssociationNode AssociationTarget = "node"
	AssociationEdge AssociationTarget = "edge"
)

// Association binds one graph entity to exact immutable document evidence.
// The value uses private fields so returned materializations cannot be
// mutated through it.
type Association struct {
	target   AssociationTarget
	targetID shoal.ID
	citation document.Citation
}

func (a Association) Target() AssociationTarget {
	return a.target
}

func (a Association) TargetID() shoal.ID {
	return a.targetID
}

func (a Association) Citation() document.Citation {
	return a.citation
}

// Validate checks the public identity and citation shape. Revision-bound
// containment and UTF-8 boundary checks are performed by Materialization.
func (a Association) Validate() error {
	switch a.target {
	case AssociationNode, AssociationEdge:
	default:
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "invalid code materialization association target")
	}
	if err := shoal.ValidateRequiredID("association target ID", a.targetID); err != nil {
		return err
	}
	return a.citation.Validate()
}

// Materialization is the complete immutable-ish explorer-code-v1 public
// projection. Slice and map getters return defensive copies.
type Materialization struct {
	version        string
	idempotencyKey codeast.ID
	sourceID       codeast.ID
	source         explorer.Source
	document       document.Document
	revision       document.Revision
	sections       []document.Section
	spans          []document.Span
	nodes          []graph.Node
	edges          []graph.Edge
	associations   []Association
	artifacts      []codeast.ArtifactRef
}

func (m Materialization) Version() string {
	return m.version
}

func (m Materialization) Source() explorer.Source {
	return cloneExplorerSource(m.source)
}

func (m Materialization) Document() document.Document {
	return cloneDocument(m.document)
}

func (m Materialization) Revision() document.Revision {
	return cloneRevision(m.revision)
}

func (m Materialization) Sections() []document.Section {
	return cloneSections(m.sections)
}

func (m Materialization) Spans() []document.Span {
	return cloneSpans(m.spans)
}

func (m Materialization) Nodes() []graph.Node {
	return cloneNodes(m.nodes)
}

func (m Materialization) Edges() []graph.Edge {
	return cloneEdges(m.edges)
}

func (m Materialization) Associations() []Association {
	return append([]Association(nil), m.associations...)
}

func (m Materialization) Artifacts() []codeast.ArtifactRef {
	return append([]codeast.ArtifactRef(nil), m.artifacts...)
}

// IngestResult constructs the exact ordered document-then-graph public result
// for this materialization.
func (m Materialization) IngestResult(
	request codeast.IngestRequest,
	disposition codeast.IngestDisposition,
) (codeast.IngestResult, error) {
	if err := m.ValidateFor(request); err != nil {
		return codeast.IngestResult{}, err
	}
	return codeast.NewIngestResult(request, disposition, m.Artifacts())
}

type syntaxRecord struct {
	node      codeast.SyntaxNode
	sectionID shoal.ID
	spanID    shoal.ID
}

type projectedCounts struct {
	nodes        int
	edges        int
	associations int
}

// Materialize validates the ingestion request and the independently supplied
// exact source bytes, then builds the explorer-code-v1 public projection.
// Public URI is repository locator + "#/" + canonical source path, without
// normalizing either component; title is path.Base(canonical source path).
// Source.Ref is deliberately excluded because it is not part of Source.ID.
// The source byte slice and all caller-owned maps and slices remain untouched.
func Materialize(
	request codeast.IngestRequest,
	exactSource []byte,
) (Materialization, error) {
	if uint64(len(exactSource)) > uint64(document.MaxRevisionSourceBytes) ||
		request.ContentSize() > uint64(document.MaxRevisionSourceBytes) {
		return Materialization{}, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"code materialization source exceeds the public byte bound",
		)
	}
	source := request.Source()
	if uint64(len(exactSource)) != source.SizeBytes() {
		return Materialization{}, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"source size does not match materialization bytes",
		)
	}
	if codeast.HashContent(exactSource) != source.ContentHash() {
		return Materialization{}, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"source hash does not match materialization bytes",
		)
	}
	if !request.ContentEqual(exactSource) {
		return Materialization{}, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"materialization bytes do not match the ingestion parse request",
		)
	}
	if !utf8.Valid(exactSource) {
		return Materialization{}, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"code materialization source must be valid UTF-8",
		)
	}
	inputCounts := request.ParseResultCounts()
	if inputCounts.SyntaxNodes+1 > document.MaxSectionsPerRevision {
		return Materialization{}, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"code materialization has too many document sections",
		)
	}
	if inputCounts.SyntaxNodes > document.MaxSpansPerRevision {
		return Materialization{}, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"code materialization has too many document spans",
		)
	}
	counts, err := preflightProjectedCounts(inputCounts)
	if err != nil {
		return Materialization{}, err
	}
	if err := request.Validate(); err != nil {
		return Materialization{}, err
	}

	parseResult := request.ParseResult()
	syntaxNodes := parseResult.Nodes()
	symbols := parseResult.Symbols()
	externals := parseResult.Externals()
	relationships := parseResult.Relationships()
	roots := parseResult.Roots()

	documentID, err := stableID(
		"shoal.document", source.Repository().Locator(), source.Path())
	if err != nil {
		return Materialization{}, err
	}
	revisionID, err := stableID(
		"shoal.revision",
		string(documentID),
		source.ID().String(),
		request.IdempotencyKey().String(),
	)
	if err != nil {
		return Materialization{}, err
	}
	rootSectionID, err := stableID(
		"shoal.section", string(revisionID), "root")
	if err != nil {
		return Materialization{}, err
	}
	graphArtifactID, err := stableID(
		"shoal.graph-publication", request.IdempotencyKey().String())
	if err != nil {
		return Materialization{}, err
	}

	sourceURI := canonicalSourceURI(source)
	title := pathpkg.Base(source.Path())
	publicMetadata, err := materializationMetadata(
		source, parseResult, request.IdempotencyKey())
	if err != nil {
		return Materialization{}, err
	}
	publicSource := explorer.Source{
		URI:       sourceURI,
		Title:     title,
		MediaType: explorer.MediaTypeText,
		Content:   string(exactSource),
		Metadata:  cloneMetadata(publicMetadata),
	}
	doc := document.Document{
		ID:            documentID,
		RevisionID:    revisionID,
		Title:         title,
		RootSectionID: rootSectionID,
		Metadata:      cloneMetadata(publicMetadata),
	}
	revision := document.Revision{
		ID:            revisionID,
		DocumentID:    documentID,
		SourceVersion: source.Revision(),
		Metadata:      cloneMetadata(publicMetadata),
	}
	fullRange := document.SourceRange{
		Start: document.SourcePosition{Offset: 0},
		End:   document.SourcePosition{Offset: int64(len(exactSource))},
	}
	rootMetadata, err := mergeMetadata(nil, shoal.Metadata{
		metadataPrefix + "materializer_version": Version,
		metadataPrefix + "path":                 source.Path(),
	})
	if err != nil {
		return Materialization{}, err
	}
	sections := make([]document.Section, 1, len(syntaxNodes)+1)
	sections[0] = document.Section{
		ID:         rootSectionID,
		DocumentID: documentID,
		RevisionID: revisionID,
		Heading:    title,
		Range:      fullRange,
		Metadata:   rootMetadata,
	}
	spans := make([]document.Span, 0, len(syntaxNodes))
	records := make([]syntaxRecord, 0, len(syntaxNodes))
	recordByID := make(map[codeast.ID]syntaxRecord, len(syntaxNodes))
	nodeByID := make(map[codeast.ID]codeast.SyntaxNode, len(syntaxNodes))
	for _, node := range syntaxNodes {
		nodeByID[node.ID()] = node
	}

	var appendSyntax func(codeast.ID, shoal.ID, uint32) error
	appendSyntax = func(nodeID codeast.ID, parentID shoal.ID, order uint32) error {
		node, exists := nodeByID[nodeID]
		if !exists {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"validated syntax traversal references an unknown node",
			)
		}
		sourceRange, err := publicRange(node.Range(), publicSource.Content)
		if err != nil {
			return fmt.Errorf("syntax node %s: %w", node.ID(), err)
		}
		sectionID, err := stableID(
			"shoal.section", string(revisionID), node.ID().String())
		if err != nil {
			return err
		}
		spanID, err := stableID(
			"shoal.span", string(revisionID), node.ID().String())
		if err != nil {
			return err
		}
		syntaxMetadata, err := mergeMetadata(nil, shoal.Metadata{
			metadataPrefix + "syntax_id":  node.ID().String(),
			metadataPrefix + "kind":       node.Kind(),
			metadataPrefix + "occurrence": strconv.FormatUint(uint64(node.Occurrence()), 10),
		})
		if err != nil {
			return err
		}
		sections = append(sections, document.Section{
			ID:         sectionID,
			DocumentID: documentID,
			RevisionID: revisionID,
			ParentID:   parentID,
			Order:      order,
			Heading:    node.Kind(),
			Range:      sourceRange,
			Metadata:   cloneMetadata(syntaxMetadata),
		})
		spans = append(spans, document.Span{
			ID:         spanID,
			DocumentID: documentID,
			RevisionID: revisionID,
			SectionID:  sectionID,
			Order:      0,
			Range:      sourceRange,
			Text:       string(exactSource[sourceRange.Start.Offset:sourceRange.End.Offset]),
			Metadata:   cloneMetadata(syntaxMetadata),
		})
		record := syntaxRecord{node: node, sectionID: sectionID, spanID: spanID}
		records = append(records, record)
		recordByID[node.ID()] = record
		for index, childID := range node.Children() {
			if err := appendSyntax(childID, sectionID, uint32(index+1)); err != nil {
				return err
			}
		}
		return nil
	}
	for index, rootID := range roots {
		if err := appendSyntax(rootID, rootSectionID, uint32(index)); err != nil {
			return Materialization{}, err
		}
	}

	nodes, err := materializeNodes(
		doc, revision, sections, spans, source, records,
		symbols, externals, counts.nodes)
	if err != nil {
		return Materialization{}, err
	}
	edges, err := materializeEdges(
		doc, sections, spans, source, records, recordByID,
		symbols, relationships, counts.edges)
	if err != nil {
		return Materialization{}, err
	}
	associations, err := materializeAssociations(
		doc, revision, rootSectionID, records, recordByID,
		symbols, relationships, counts.associations)
	if err != nil {
		return Materialization{}, err
	}

	documentArtifact, err := codeast.NewArtifactRef(
		codeast.ArtifactDocument, documentID)
	if err != nil {
		return Materialization{}, err
	}
	graphArtifact, err := codeast.NewArtifactRef(
		codeast.ArtifactGraph, graphArtifactID)
	if err != nil {
		return Materialization{}, err
	}
	materialization := Materialization{
		version:        Version,
		idempotencyKey: request.IdempotencyKey(),
		sourceID:       source.ID(),
		source:         publicSource,
		document:       doc,
		revision:       revision,
		sections:       sections,
		spans:          spans,
		nodes:          nodes,
		edges:          edges,
		associations:   associations,
		artifacts:      []codeast.ArtifactRef{documentArtifact, graphArtifact},
	}
	if err := materialization.ValidateFor(request); err != nil {
		return Materialization{}, err
	}
	return materialization, nil
}

// ValidateFor validates all public values, exact IDs, ownership, graph
// endpoint integrity, citation resolution, uniqueness, and deterministic
// ordering against request.
func (m Materialization) ValidateFor(request codeast.IngestRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	if m.version != Version {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "invalid code materializer version")
	}
	source := request.Source()
	if m.idempotencyKey != request.IdempotencyKey() || m.sourceID != source.ID() {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"code materialization does not match ingestion request",
		)
	}
	if err := validateExplorerSource(m.source, request); err != nil {
		return err
	}
	if m.source.URI != canonicalSourceURI(source) ||
		m.source.Title != pathpkg.Base(source.Path()) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"materialized source URI or title is not canonical",
		)
	}
	if _, present := m.source.Metadata[metadataPrefix+"ref"]; present {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"materialized source metadata must not contain source ref",
		)
	}
	for _, node := range m.nodes {
		if _, present := node.Properties[metadataPrefix+"ref"]; present {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"materialized graph properties must not contain source ref",
			)
		}
	}
	counts, err := preflightProjectedCounts(request.ParseResultCounts())
	if err != nil {
		return err
	}
	if len(m.nodes) != counts.nodes ||
		len(m.edges) != counts.edges ||
		len(m.associations) != counts.associations {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"materialized graph cardinality does not match canonical projection",
		)
	}

	expectedDocumentID, err := stableID(
		"shoal.document", source.Repository().Locator(), source.Path())
	if err != nil {
		return err
	}
	expectedRevisionID, err := stableID(
		"shoal.revision",
		string(expectedDocumentID),
		source.ID().String(),
		request.IdempotencyKey().String(),
	)
	if err != nil {
		return err
	}
	expectedRootID, err := stableID(
		"shoal.section", string(expectedRevisionID), "root")
	if err != nil {
		return err
	}
	expectedGraphArtifactID, err := stableID(
		"shoal.graph-publication", request.IdempotencyKey().String())
	if err != nil {
		return err
	}
	if m.document.ID != expectedDocumentID ||
		m.document.RevisionID != expectedRevisionID ||
		m.document.RootSectionID != expectedRootID ||
		m.revision.ID != expectedRevisionID ||
		m.revision.DocumentID != expectedDocumentID {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"code materialization document identities are not canonical",
		)
	}
	if len(m.artifacts) != 2 ||
		m.artifacts[0].Kind() != codeast.ArtifactDocument ||
		m.artifacts[0].Identifier() != expectedDocumentID ||
		m.artifacts[1].Kind() != codeast.ArtifactGraph ||
		m.artifacts[1].Identifier() != expectedGraphArtifactID {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"code materialization artifacts are not canonical document-then-graph refs",
		)
	}
	for _, artifact := range m.artifacts {
		if err := artifact.Validate(); err != nil {
			return err
		}
	}
	if err := document.ValidateRevisionContent(
		m.source.Content, m.document, m.revision, m.sections, m.spans,
	); err != nil {
		return err
	}
	if err := validateDocumentOrder(
		m.document.RootSectionID, m.sections, m.spans); err != nil {
		return err
	}
	nodeIDs, err := validateGraphNodes(m.nodes)
	if err != nil {
		return err
	}
	edgeIDs, err := validateGraphEdges(m.edges, nodeIDs)
	if err != nil {
		return err
	}
	if err := validateCodeGraphProjection(
		request.ParseResult(), nodeIDs, edgeIDs, m.edges); err != nil {
		return err
	}
	if err := validateAssociations(
		m.source.Content, m.document, m.revision,
		m.sections, m.spans, m.associations, nodeIDs, edgeIDs,
	); err != nil {
		return err
	}
	return nil
}

func materializationMetadata(
	source codeast.Source,
	result codeast.ParseResult,
	idempotencyKey codeast.ID,
) (shoal.Metadata, error) {
	language := result.Language()
	parser := result.Parser()
	return mergeMetadata(nil, shoal.Metadata{
		metadataPrefix + "materializer_version":      Version,
		metadataPrefix + "repository":                source.Repository().Locator(),
		metadataPrefix + "path":                      source.Path(),
		metadataPrefix + "revision":                  source.Revision(),
		metadataPrefix + "content_hash":              source.ContentHash().String(),
		metadataPrefix + "source_id":                 source.ID().String(),
		metadataPrefix + "language":                  language.ID(),
		metadataPrefix + "language_version":          language.Version(),
		metadataPrefix + "language_dialect":          language.Dialect(),
		metadataPrefix + "parser":                    parser.Name(),
		metadataPrefix + "parser_version":            parser.Version(),
		metadataPrefix + "parser_configuration_hash": parser.ConfigurationHash().String(),
		metadataPrefix + "ingestion_idempotency_key": idempotencyKey.String(),
	})
}

func materializeNodes(
	doc document.Document,
	revision document.Revision,
	sections []document.Section,
	spans []document.Span,
	source codeast.Source,
	records []syntaxRecord,
	symbols []codeast.SemanticSymbol,
	externals []codeast.ExternalEntity,
	projectedNodeCount int,
) ([]graph.Node, error) {
	nodes := make([]graph.Node, 0, projectedNodeCount)
	appendNode := func(node graph.Node) error {
		if err := node.Validate(); err != nil {
			return err
		}
		nodes = append(nodes, node)
		return nil
	}
	documentProperties, err := mergeMetadata(nil, shoal.Metadata{
		metadataPrefix + "title":       doc.Title,
		metadataPrefix + "revision_id": string(revision.ID),
	})
	if err != nil {
		return nil, err
	}
	if err := appendNode(graph.Node{
		ID: doc.ID, Kind: "document", Labels: []string{"document"},
		Properties: documentProperties,
	}); err != nil {
		return nil, err
	}
	sourceProperties, err := mergeMetadata(nil, shoal.Metadata{
		metadataPrefix + "repository":   source.Repository().Locator(),
		metadataPrefix + "path":         source.Path(),
		metadataPrefix + "revision":     source.Revision(),
		metadataPrefix + "content_hash": source.ContentHash().String(),
	})
	if err != nil {
		return nil, err
	}
	if err := appendNode(graph.Node{
		ID: shoal.ID(source.ID().String()), Kind: "code.source",
		Labels: []string{"code", "source"}, Properties: sourceProperties,
	}); err != nil {
		return nil, err
	}
	for _, section := range sections {
		properties, err := mergeMetadata(nil, shoal.Metadata{
			metadataPrefix + "document_id": string(section.DocumentID),
			metadataPrefix + "revision_id": string(section.RevisionID),
			metadataPrefix + "parent_id":   string(section.ParentID),
			metadataPrefix + "heading":     section.Heading,
			metadataPrefix + "range_start": strconv.FormatInt(section.Range.Start.Offset, 10),
			metadataPrefix + "range_end":   strconv.FormatInt(section.Range.End.Offset, 10),
		})
		if err != nil {
			return nil, err
		}
		labels := []string{"document", "section"}
		if section.ParentID == "" {
			labels = append(labels, "root")
		}
		if err := appendNode(graph.Node{
			ID: section.ID, Kind: "document.section",
			Labels: labels, Properties: properties,
		}); err != nil {
			return nil, err
		}
	}
	for _, span := range spans {
		properties, err := mergeMetadata(nil, shoal.Metadata{
			metadataPrefix + "document_id": string(span.DocumentID),
			metadataPrefix + "revision_id": string(span.RevisionID),
			metadataPrefix + "section_id":  string(span.SectionID),
			metadataPrefix + "range_start": strconv.FormatInt(span.Range.Start.Offset, 10),
			metadataPrefix + "range_end":   strconv.FormatInt(span.Range.End.Offset, 10),
		})
		if err != nil {
			return nil, err
		}
		if err := appendNode(graph.Node{
			ID: span.ID, Kind: "document.span",
			Labels:     []string{"document", "span", "evidence"},
			Properties: properties,
		}); err != nil {
			return nil, err
		}
	}
	for _, record := range records {
		node := record.node
		properties, err := mergeMetadata(node.Attributes(), shoal.Metadata{
			metadataPrefix + "entity_kind": node.Kind(),
			metadataPrefix + "source_id":   node.SourceID().String(),
			metadataPrefix + "range_start": strconv.FormatUint(
				node.Range().Start().ByteOffset(), 10),
			metadataPrefix + "range_end": strconv.FormatUint(
				node.Range().End().ByteOffset(), 10),
			metadataPrefix + "occurrence": strconv.FormatUint(
				uint64(node.Occurrence()), 10),
		})
		if err != nil {
			return nil, err
		}
		if err := appendNode(graph.Node{
			ID: shoal.ID(node.ID().String()), Kind: "code.syntax",
			Labels: []string{"code", "syntax"}, Properties: properties,
		}); err != nil {
			return nil, err
		}
	}
	for _, symbol := range symbols {
		fixed := shoal.Metadata{
			metadataPrefix + "entity_kind":    symbol.Kind(),
			metadataPrefix + "source_id":      symbol.SourceID().String(),
			metadataPrefix + "name":           symbol.Name(),
			metadataPrefix + "qualified_name": symbol.QualifiedName(),
			metadataPrefix + "range_start": strconv.FormatUint(
				symbol.Definition().Start().ByteOffset(), 10),
			metadataPrefix + "range_end": strconv.FormatUint(
				symbol.Definition().End().ByteOffset(), 10),
			metadataPrefix + "occurrence": strconv.FormatUint(
				uint64(symbol.Occurrence()), 10),
		}
		if syntaxID, present := symbol.SyntaxNodeID(); present {
			fixed[metadataPrefix+"syntax_id"] = syntaxID.String()
		}
		properties, err := mergeMetadata(symbol.Attributes(), fixed)
		if err != nil {
			return nil, err
		}
		if err := appendNode(graph.Node{
			ID: shoal.ID(symbol.ID().String()), Kind: "code.symbol",
			Labels: []string{"code", "symbol"}, Properties: properties,
		}); err != nil {
			return nil, err
		}
	}
	for _, external := range externals {
		properties, err := mergeMetadata(external.Attributes(), shoal.Metadata{
			metadataPrefix + "entity_kind":    external.Kind(),
			metadataPrefix + "canonical_name": external.CanonicalName(),
		})
		if err != nil {
			return nil, err
		}
		if err := appendNode(graph.Node{
			ID: shoal.ID(external.ID().String()), Kind: "code.external",
			Labels: []string{"code", "external"}, Properties: properties,
		}); err != nil {
			return nil, err
		}
	}
	sort.Slice(nodes, func(i, j int) bool {
		return shoal.CompareID(nodes[i].ID, nodes[j].ID) < 0
	})
	return nodes, nil
}

func materializeEdges(
	doc document.Document,
	sections []document.Section,
	spans []document.Span,
	source codeast.Source,
	records []syntaxRecord,
	recordByID map[codeast.ID]syntaxRecord,
	symbols []codeast.SemanticSymbol,
	relationships []codeast.Relationship,
	projectedEdgeCount int,
) ([]graph.Edge, error) {
	edges := make([]graph.Edge, 0, projectedEdgeCount)
	appendDerived := func(edgeType string, from, to shoal.ID) error {
		edge, err := derivedEdge(edgeType, from, to)
		if err != nil {
			return err
		}
		edges = append(edges, edge)
		return nil
	}
	if err := appendDerived(
		"represents_source", doc.ID, shoal.ID(source.ID().String())); err != nil {
		return nil, err
	}
	for _, section := range sections {
		parentID := section.ParentID
		if parentID == "" {
			parentID = doc.ID
		}
		if err := appendDerived("contains", parentID, section.ID); err != nil {
			return nil, err
		}
	}
	for _, span := range spans {
		if err := appendDerived("contains", span.SectionID, span.ID); err != nil {
			return nil, err
		}
	}
	for _, record := range records {
		syntaxID := shoal.ID(record.node.ID().String())
		if err := appendDerived(
			"associated_with", record.sectionID, syntaxID); err != nil {
			return nil, err
		}
		if err := appendDerived(
			"associated_with", record.spanID, syntaxID); err != nil {
			return nil, err
		}
	}
	for _, symbol := range symbols {
		syntaxID, present := symbol.SyntaxNodeID()
		if !present {
			continue
		}
		if _, exists := recordByID[syntaxID]; !exists {
			return nil, shoal.NewError(
				shoal.ErrorInvalidArgument,
				"semantic symbol declaration syntax was not materialized",
			)
		}
		if err := appendDerived(
			"declared_at",
			shoal.ID(symbol.ID().String()),
			shoal.ID(syntaxID.String()),
		); err != nil {
			return nil, err
		}
	}
	for _, relationship := range relationships {
		properties := cloneMetadata(relationship.Attributes())
		if sourceRange, present := relationship.Range(); present {
			var err error
			properties, err = mergeMetadata(properties, shoal.Metadata{
				metadataPrefix + "range_start": strconv.FormatUint(
					sourceRange.Start().ByteOffset(), 10),
				metadataPrefix + "range_end": strconv.FormatUint(
					sourceRange.End().ByteOffset(), 10),
			})
			if err != nil {
				return nil, err
			}
		} else if err := shoal.ValidateMetadata(
			"code relationship properties", properties); err != nil {
			return nil, err
		}
		edge := graph.Edge{
			ID:         shoal.ID(relationship.ID().String()),
			From:       shoal.ID(relationship.From().ID().String()),
			To:         shoal.ID(relationship.To().ID().String()),
			Type:       string(relationship.Kind()),
			Weight:     1,
			Properties: properties,
		}
		if err := edge.Validate(); err != nil {
			return nil, err
		}
		edges = append(edges, edge)
	}
	sort.Slice(edges, func(i, j int) bool {
		return shoal.CompareID(edges[i].ID, edges[j].ID) < 0
	})
	return edges, nil
}

func materializeAssociations(
	doc document.Document,
	revision document.Revision,
	rootSectionID shoal.ID,
	records []syntaxRecord,
	recordByID map[codeast.ID]syntaxRecord,
	symbols []codeast.SemanticSymbol,
	relationships []codeast.Relationship,
	projectedAssociationCount int,
) ([]Association, error) {
	associations := make([]Association, 0, projectedAssociationCount)
	for _, record := range records {
		publicRange, err := rangeWithoutSource(record.node.Range())
		if err != nil {
			return nil, err
		}
		associations = append(associations, Association{
			target:   AssociationNode,
			targetID: shoal.ID(record.node.ID().String()),
			citation: document.Citation{
				DocumentID: doc.ID,
				RevisionID: revision.ID,
				SectionID:  record.sectionID,
				SpanID:     record.spanID,
				Range:      publicRange,
			},
		})
	}
	for _, symbol := range symbols {
		syntaxID, present := symbol.SyntaxNodeID()
		if !present {
			continue
		}
		record, exists := recordByID[syntaxID]
		if !exists {
			return nil, shoal.NewError(
				shoal.ErrorInvalidArgument,
				"semantic symbol declaration syntax was not materialized",
			)
		}
		publicRange, err := rangeWithoutSource(symbol.Definition())
		if err != nil {
			return nil, err
		}
		associations = append(associations, Association{
			target:   AssociationNode,
			targetID: shoal.ID(symbol.ID().String()),
			citation: document.Citation{
				DocumentID: doc.ID,
				RevisionID: revision.ID,
				SectionID:  record.sectionID,
				SpanID:     record.spanID,
				Range:      publicRange,
			},
		})
	}
	for _, relationship := range relationships {
		sourceRange, present := relationship.Range()
		if !present {
			continue
		}
		publicRange, err := rangeWithoutSource(sourceRange)
		if err != nil {
			return nil, err
		}
		sectionID := rootSectionID
		var spanID shoal.ID
		for _, record := range records {
			if record.node.Range().Contains(sourceRange) {
				sectionID = record.sectionID
				spanID = record.spanID
			}
		}
		associations = append(associations, Association{
			target:   AssociationEdge,
			targetID: shoal.ID(relationship.ID().String()),
			citation: document.Citation{
				DocumentID: doc.ID,
				RevisionID: revision.ID,
				SectionID:  sectionID,
				SpanID:     spanID,
				Range:      publicRange,
			},
		})
	}
	sort.Slice(associations, func(i, j int) bool {
		return compareAssociation(associations[i], associations[j]) < 0
	})
	return associations, nil
}

func canonicalSourceURI(source codeast.Source) string {
	return source.Repository().Locator() + "#/" + source.Path()
}

func preflightProjectedCounts(
	input codeast.ParseResultCounts,
) (projectedCounts, error) {
	nodeCount, ok := checkedCountSum(
		3,
		checkedCountProduct(input.SyntaxNodes, 3),
		input.Symbols,
		input.Externals,
	)
	if !ok {
		return projectedCounts{}, countOverflowError("graph node")
	}
	edgeCount, ok := checkedCountSum(
		2,
		checkedCountProduct(input.SyntaxNodes, 4),
		input.DeclaredSymbols,
		input.Relationships,
	)
	if !ok {
		return projectedCounts{}, countOverflowError("graph edge")
	}
	associationCount, ok := checkedCountSum(
		input.SyntaxNodes, input.DeclaredSymbols, input.RangedRelationships)
	if !ok {
		return projectedCounts{}, countOverflowError("association")
	}
	if nodeCount > MaxGraphNodes {
		return projectedCounts{}, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"code materialization exceeds the graph node bound",
		)
	}
	if edgeCount > MaxGraphEdges {
		return projectedCounts{}, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"code materialization exceeds the graph edge bound",
		)
	}
	if associationCount > MaxAssociations {
		return projectedCounts{}, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"code materialization exceeds the association bound",
		)
	}
	return projectedCounts{
		nodes:        int(nodeCount),
		edges:        int(edgeCount),
		associations: int(associationCount),
	}, nil
}

func checkedCountProduct(value, factor uint64) uint64 {
	if factor != 0 && value > ^uint64(0)/factor {
		return ^uint64(0)
	}
	return value * factor
}

func checkedCountSum(values ...uint64) (uint64, bool) {
	total := uint64(0)
	for _, value := range values {
		if value == ^uint64(0) || total > ^uint64(0)-value {
			return 0, false
		}
		total += value
	}
	return total, true
}

func countOverflowError(kind string) error {
	return shoal.NewError(
		shoal.ErrorInvalidArgument,
		"code materialization "+kind+" count overflows",
	)
}

func stableID(namespace string, parts ...string) (shoal.ID, error) {
	id, err := codeast.NewStableID(namespace, parts...)
	if err != nil {
		return "", err
	}
	return shoal.ID(id.String()), nil
}

func derivedEdge(edgeType string, from, to shoal.ID) (graph.Edge, error) {
	id, err := stableID(
		"shoal.graph-edge", Version, edgeType, string(from), string(to))
	if err != nil {
		return graph.Edge{}, err
	}
	edge := graph.Edge{
		ID: id, From: from, To: to, Type: edgeType, Weight: 1,
	}
	if err := edge.Validate(); err != nil {
		return graph.Edge{}, err
	}
	return edge, nil
}

func publicRange(
	sourceRange codeast.Range, source string,
) (document.SourceRange, error) {
	value, err := rangeWithoutSource(sourceRange)
	if err != nil {
		return document.SourceRange{}, err
	}
	if value.End.Offset > int64(len(source)) {
		return document.SourceRange{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "source range exceeds exact source bytes")
	}
	if err := value.ValidateSource(source); err != nil {
		return document.SourceRange{}, err
	}
	return value, nil
}

func rangeWithoutSource(sourceRange codeast.Range) (document.SourceRange, error) {
	start := sourceRange.Start().ByteOffset()
	end := sourceRange.End().ByteOffset()
	maxInt64 := uint64(^uint64(0) >> 1)
	if start > maxInt64 || end > maxInt64 {
		return document.SourceRange{}, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"source range cannot be represented by public document offsets",
		)
	}
	return document.SourceRange{
		Start: document.SourcePosition{Offset: int64(start)},
		End:   document.SourcePosition{Offset: int64(end)},
	}, nil
}

func validateExplorerSource(
	source explorer.Source, request codeast.IngestRequest,
) error {
	if !utf8.ValidString(source.URI) || strings.TrimSpace(source.URI) == "" {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "materialized source URI is invalid")
	}
	if !utf8.ValidString(source.Title) || strings.TrimSpace(source.Title) == "" {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "materialized source title is invalid")
	}
	if err := shoal.ValidateSemanticString("materialized source title", source.Title); err != nil {
		return err
	}
	if source.MediaType != explorer.MediaTypeText {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "materialized code source must be text/plain")
	}
	if !utf8.ValidString(source.Content) ||
		uint64(len(source.Content)) != request.ContentSize() ||
		!request.ContentStringEqual(source.Content) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"materialized source content does not match exact parse bytes",
		)
	}
	return shoal.ValidateMetadata("materialized source metadata", source.Metadata)
}

func validateDocumentOrder(
	rootID shoal.ID,
	sections []document.Section,
	spans []document.Span,
) error {
	if len(sections) == 0 || sections[0].ID != rootID {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"materialized sections must begin with the document root",
		)
	}
	sectionByID := make(map[shoal.ID]document.Section, len(sections))
	children := make(map[shoal.ID][]document.Section)
	for _, section := range sections {
		sectionByID[section.ID] = section
		if section.ParentID != "" {
			children[section.ParentID] = append(children[section.ParentID], section)
		}
	}
	for parentID := range children {
		sort.Slice(children[parentID], func(i, j int) bool {
			return children[parentID][i].Order < children[parentID][j].Order
		})
		parent := sectionByID[parentID]
		firstOrder := uint32(0)
		if parent.ParentID != "" {
			firstOrder = 1
		}
		for index, child := range children[parentID] {
			if child.Order != firstOrder+uint32(index) {
				return shoal.NewError(
					shoal.ErrorInvalidArgument,
					"materialized syntax section order is not canonical",
				)
			}
		}
	}
	expected := make([]shoal.ID, 0, len(sections))
	var visit func(shoal.ID)
	visit = func(id shoal.ID) {
		expected = append(expected, id)
		for _, child := range children[id] {
			visit(child.ID)
		}
	}
	visit(rootID)
	if len(expected) != len(sections) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"materialized section preorder is incomplete",
		)
	}
	for index, id := range expected {
		if sections[index].ID != id {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"materialized sections are not in syntax preorder",
			)
		}
	}
	if len(spans) != len(sections)-1 {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"materialized syntax sections require exactly one span each",
		)
	}
	for index, span := range spans {
		if span.SectionID != sections[index+1].ID || span.Order != 0 {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"materialized spans are not in syntax preorder",
			)
		}
	}
	return nil
}

func validateGraphNodes(nodes []graph.Node) (map[shoal.ID]struct{}, error) {
	ids := make(map[shoal.ID]struct{}, len(nodes))
	for index, node := range nodes {
		if err := node.Validate(); err != nil {
			return nil, err
		}
		if _, duplicate := ids[node.ID]; duplicate {
			return nil, shoal.NewError(
				shoal.ErrorInvalidArgument, "duplicate materialized graph node ID")
		}
		if index > 0 && shoal.CompareID(nodes[index-1].ID, node.ID) >= 0 {
			return nil, shoal.NewError(
				shoal.ErrorInvalidArgument,
				"materialized graph nodes are not in deterministic ID order",
			)
		}
		ids[node.ID] = struct{}{}
	}
	return ids, nil
}

func validateGraphEdges(
	edges []graph.Edge,
	nodeIDs map[shoal.ID]struct{},
) (map[shoal.ID]struct{}, error) {
	ids := make(map[shoal.ID]struct{}, len(edges))
	for index, edge := range edges {
		if err := edge.Validate(); err != nil {
			return nil, err
		}
		if _, duplicate := ids[edge.ID]; duplicate {
			return nil, shoal.NewError(
				shoal.ErrorInvalidArgument, "duplicate materialized graph edge ID")
		}
		if _, exists := nodeIDs[edge.From]; !exists {
			return nil, shoal.NewError(
				shoal.ErrorInvalidArgument,
				"materialized graph edge has an unknown from endpoint",
			)
		}
		if _, exists := nodeIDs[edge.To]; !exists {
			return nil, shoal.NewError(
				shoal.ErrorInvalidArgument,
				"materialized graph edge has an unknown to endpoint",
			)
		}
		if index > 0 && shoal.CompareID(edges[index-1].ID, edge.ID) >= 0 {
			return nil, shoal.NewError(
				shoal.ErrorInvalidArgument,
				"materialized graph edges are not in deterministic ID order",
			)
		}
		ids[edge.ID] = struct{}{}
	}
	return ids, nil
}

func validateCodeGraphProjection(
	result codeast.ParseResult,
	nodeIDs, edgeIDs map[shoal.ID]struct{},
	edges []graph.Edge,
) error {
	requiredNodes := []shoal.ID{shoal.ID(result.Source().ID().String())}
	for _, node := range result.Nodes() {
		requiredNodes = append(requiredNodes, shoal.ID(node.ID().String()))
	}
	for _, symbol := range result.Symbols() {
		requiredNodes = append(requiredNodes, shoal.ID(symbol.ID().String()))
	}
	for _, external := range result.Externals() {
		requiredNodes = append(requiredNodes, shoal.ID(external.ID().String()))
	}
	for _, id := range requiredNodes {
		if _, exists := nodeIDs[id]; !exists {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"materialized graph omitted a code entity node",
			)
		}
	}
	edgeByID := make(map[shoal.ID]graph.Edge, len(edges))
	for _, edge := range edges {
		edgeByID[edge.ID] = edge
	}
	for _, relationship := range result.Relationships() {
		id := shoal.ID(relationship.ID().String())
		if _, exists := edgeIDs[id]; !exists {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"materialized graph omitted a code relationship",
			)
		}
		edge := edgeByID[id]
		if edge.From != shoal.ID(relationship.From().ID().String()) ||
			edge.To != shoal.ID(relationship.To().ID().String()) ||
			edge.Type != string(relationship.Kind()) {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"materialized code relationship changed its public identity",
			)
		}
	}
	return nil
}

func validateAssociations(
	source string,
	doc document.Document,
	revision document.Revision,
	sections []document.Section,
	spans []document.Span,
	associations []Association,
	nodeIDs, edgeIDs map[shoal.ID]struct{},
) error {
	for index, association := range associations {
		if err := association.Validate(); err != nil {
			return err
		}
		switch association.Target() {
		case AssociationNode:
			if _, exists := nodeIDs[association.TargetID()]; !exists {
				return shoal.NewError(
					shoal.ErrorInvalidArgument,
					"association references an unknown graph node",
				)
			}
		case AssociationEdge:
			if _, exists := edgeIDs[association.TargetID()]; !exists {
				return shoal.NewError(
					shoal.ErrorInvalidArgument,
					"association references an unknown graph edge",
				)
			}
		}
		if _, err := document.ResolveCitationQuote(
			source, doc, revision, sections, spans, association.Citation(),
		); err != nil {
			return err
		}
		if index > 0 &&
			compareAssociation(associations[index-1], association) >= 0 {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"materialized associations are not in deterministic order",
			)
		}
	}
	return nil
}

func compareAssociation(left, right Association) int {
	if left.Target() < right.Target() {
		return -1
	}
	if left.Target() > right.Target() {
		return 1
	}
	if compared := shoal.CompareID(left.TargetID(), right.TargetID()); compared != 0 {
		return compared
	}
	leftCitation := left.Citation()
	rightCitation := right.Citation()
	for _, compared := range []int{
		shoal.CompareID(leftCitation.SectionID, rightCitation.SectionID),
		shoal.CompareID(leftCitation.SpanID, rightCitation.SpanID),
		compareInt64(leftCitation.Range.Start.Offset, rightCitation.Range.Start.Offset),
		compareInt64(leftCitation.Range.End.Offset, rightCitation.Range.End.Offset),
	} {
		if compared != 0 {
			return compared
		}
	}
	return 0
}

func compareInt64(left, right int64) int {
	if left < right {
		return -1
	}
	if left > right {
		return 1
	}
	return 0
}

func mergeMetadata(
	base, fixed shoal.Metadata,
) (shoal.Metadata, error) {
	merged := cloneMetadata(base)
	if merged == nil && len(fixed) > 0 {
		merged = make(shoal.Metadata, len(fixed))
	}
	for key, value := range fixed {
		if _, collision := merged[key]; collision {
			return nil, shoal.NewError(
				shoal.ErrorInvalidArgument,
				"code attributes collide with materializer-owned graph properties",
			)
		}
		merged[key] = value
	}
	if err := shoal.ValidateMetadata("code materialization metadata", merged); err != nil {
		return nil, err
	}
	return merged, nil
}

func cloneMetadata(value shoal.Metadata) shoal.Metadata {
	if len(value) == 0 {
		return nil
	}
	cloned := make(shoal.Metadata, len(value))
	for key, item := range value {
		cloned[key] = item
	}
	return cloned
}

func cloneExplorerSource(value explorer.Source) explorer.Source {
	value.Metadata = cloneMetadata(value.Metadata)
	return value
}

func cloneDocument(value document.Document) document.Document {
	value.Metadata = cloneMetadata(value.Metadata)
	return value
}

func cloneRevision(value document.Revision) document.Revision {
	value.Metadata = cloneMetadata(value.Metadata)
	return value
}

func cloneSections(values []document.Section) []document.Section {
	cloned := append([]document.Section(nil), values...)
	for index := range cloned {
		cloned[index].Metadata = cloneMetadata(cloned[index].Metadata)
	}
	return cloned
}

func cloneSpans(values []document.Span) []document.Span {
	cloned := append([]document.Span(nil), values...)
	for index := range cloned {
		cloned[index].Metadata = cloneMetadata(cloned[index].Metadata)
	}
	return cloned
}

func cloneNodes(values []graph.Node) []graph.Node {
	cloned := append([]graph.Node(nil), values...)
	for index := range cloned {
		cloned[index].Labels = append([]string(nil), cloned[index].Labels...)
		cloned[index].Properties = cloneMetadata(cloned[index].Properties)
	}
	return cloned
}

func cloneEdges(values []graph.Edge) []graph.Edge {
	cloned := append([]graph.Edge(nil), values...)
	for index := range cloned {
		cloned[index].Properties = cloneMetadata(cloned[index].Properties)
	}
	return cloned
}
