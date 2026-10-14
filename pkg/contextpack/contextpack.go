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

// Package contextpack constructs bounded, verified inference context from
// Explorer retrieval, document, and graph contracts. It performs no model or
// provider calls.
package contextpack

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	DefaultMaxResults         = 256
	DefaultMaxAnchors         = 1024
	DefaultMaxDocuments       = 256
	DefaultMaxSections        = 4096
	DefaultMaxSpans           = 8192
	DefaultMaxGraphNodes      = 4096
	DefaultMaxGraphEdges      = 8192
	DefaultMaxPathNodes       = 256
	DefaultMaxContextBytes    = 4 * 1024 * 1024
	DefaultMaxHydrationBytes  = 8 * 1024 * 1024
	DefaultMaxQuoteBytes      = 1024 * 1024
	DefaultMaxProvenanceBytes = 64 * 1024
	DefaultMaxHierarchyDepth  = 16

	metadataBuilderKey         = "shoal.context.builder"
	metadataRequestKey         = "shoal.context.retrieval_request_id"
	metadataRetrievalKey       = "shoal.context.retrieval_identity"
	metadataPolicyKey          = "shoal.context.policy_id"
	metadataEmbeddingSpaceKey  = "shoal.context.embedding_space_id"
	metadataEmbeddingSpacesKey = "shoal.context.embedding_space_ids"
	builderVersion             = "explorer-context/v1"
)

// Reader is the stable, authorization-enforcing Explorer seam needed for
// hydration. Implementations must return absent and unauthorized resources
// with the same externally observable semantics.
type Reader interface {
	Document(context.Context, shoal.ID, shoal.ID) (explorer.DocumentView, error)
	Neighborhood(context.Context, explorer.NeighborhoodRequest) (explorer.Neighborhood, error)
}

// Limits are fail-closed construction ceilings. Zero fields select defaults,
// except MaxContextTokens, where zero disables tokenizer-specific accounting.
type Limits struct {
	MaxResults         int
	MaxAnchors         int
	MaxDocuments       int
	MaxSections        int
	MaxSpans           int
	MaxGraphNodes      int
	MaxGraphEdges      int
	MaxPathNodes       int
	MaxContextBytes    int
	MaxHydrationBytes  int
	MaxContextTokens   int
	MaxQuoteBytes      int
	MaxProvenanceBytes int
	MaxHierarchyDepth  uint32
}

// Pins are trusted snapshot, policy, authorization, and optional ontology
// identities supplied by the caller. The builder never derives authorization.
type Pins struct {
	Snapshot      inference.SnapshotPin
	Authorization inference.AuthPin
	PolicyID      shoal.ID
	// EmbeddingSpaceID is the opaque identity of the vector space used to
	// assemble the evidence. For mixed-space retrieval it is the caller's
	// stable identity for that exact set. Empty means no vector space
	// participated.
	EmbeddingSpaceID shoal.ID
	// EmbeddingSpaceIDs are the canonical opaque constituent identities used
	// to derive EmbeddingSpaceID. They allow later tool calls to union spaces
	// without re-hashing already-aggregated set IDs.
	EmbeddingSpaceIDs []shoal.ID
	Ontology          *inference.OntologyIdentity
}

// InitialRequest contains one retrieval operation and optional pre-hydrated
// public views. Missing views are fetched through Builder.Reader.
type InitialRequest struct {
	Request       retrieval.Request
	Response      retrieval.Response
	Documents     []explorer.DocumentView
	Neighborhoods []explorer.Neighborhood
	Selection     EvidenceSelection
	Pins          Pins
	Metadata      shoal.Metadata
}

// EvidenceSelection chooses which exact retrieval evidence variants enter the
// pack. A zero value includes both document citations and graph paths.
type EvidenceSelection struct {
	Documents bool
	Paths     bool
}

// OpenSectionRequest explicitly selects sections and a bounded descendant
// depth. Depth zero includes only the selected sections.
type OpenSectionRequest struct {
	DocumentID shoal.ID
	RevisionID shoal.ID
	SectionIDs []shoal.ID
	Depth      uint32
}

// ExpandNeighborsRequest explicitly selects graph seeds and a bounded
// neighborhood. No implicit crawling is performed.
type ExpandNeighborsRequest struct {
	NodeIDs   []shoal.ID
	Depth     uint32
	EdgeTypes []string
}

// Builder creates immutable inference packs.
type Builder struct {
	Reader         Reader
	Limits         Limits
	TokenEstimator TokenEstimator
}

// TokenEstimator provides provider-neutral token accounting without invoking
// a model. Callers should supply the tokenizer used by their future generator.
type TokenEstimator interface {
	EstimateTokens(context.Context, inference.ContextPack) (int, error)
}

// Build creates an initial context pack from exact retrieval evidence.
func (b Builder) Build(ctx context.Context, input InitialRequest) (inference.ContextPack, error) {
	if err := contextError(ctx); err != nil {
		return inference.ContextPack{}, err
	}
	limits, err := normalizeLimits(b.Limits)
	if err != nil {
		return inference.ContextPack{}, err
	}
	request, err := input.Request.Normalize()
	if err != nil {
		return inference.ContextPack{}, err
	}
	if !request.AsOf.IsZero() && !request.AsOf.Equal(input.Pins.Snapshot.AsOf()) {
		return inference.ContextPack{}, invalid(
			"retrieval as_of does not match the trusted snapshot pin")
	}
	selection := input.Selection
	if !selection.Documents && !selection.Paths {
		selection.Documents = true
		selection.Paths = true
	}
	if err := preflightResponse(input.Response, selection, limits); err != nil {
		return inference.ContextPack{}, err
	}
	response := cloneResponse(input.Response)
	retrieval.SortResults(&response)
	if err := response.ValidateFor(request); err != nil {
		return inference.ContextPack{}, fmt.Errorf("context retrieval response: %w", err)
	}
	if len(response.Results) == 0 {
		return inference.ContextPack{}, invalid("context retrieval returned no evidence")
	}

	verifier, err := newVerifier(ctx, b.Reader, limits, input.Documents, input.Neighborhoods)
	if err != nil {
		return inference.ContextPack{}, err
	}
	anchors := make([]inference.EvidenceAnchor, 0)
	for _, result := range response.Results {
		for _, evidence := range result.Evidence {
			documentAnchor, err := verifier.documentAnchor(evidence.Citation, evidence.Quote)
			if err != nil {
				return inference.ContextPack{}, fmt.Errorf(
					"result %q document evidence: %w", result.ID, err)
			}
			if selection.Documents {
				anchors = append(anchors, documentAnchor)
			}
			if pathPresent(evidence.Path) {
				graphAnchor, err := verifier.graphAnchor(evidence.Path)
				if err != nil {
					return inference.ContextPack{}, fmt.Errorf(
						"result %q graph evidence: %w", result.ID, err)
				}
				if selection.Paths {
					anchors = append(anchors, graphAnchor)
				}
			}
		}
	}
	anchors, err = canonicalAnchors(anchors, limits.MaxAnchors)
	if err != nil {
		return inference.ContextPack{}, err
	}
	embeddingSpaceID := input.Pins.EmbeddingSpaceID
	embeddingSpaceIDs, err := normalizeEmbeddingSpaceIDs(
		input.Pins.EmbeddingSpaceIDs)
	if err != nil {
		return inference.ContextPack{}, err
	}
	hasPinnedEmbeddingSpace := embeddingSpaceID != "" ||
		len(embeddingSpaceIDs) > 0
	if hasPinnedEmbeddingSpace &&
		(embeddingSpaceID == "" || len(embeddingSpaceIDs) == 0) {
		return inference.ContextPack{}, invalid(
			"trusted embedding-space pin requires aggregate and constituents")
	}
	if !request.HasMode(retrieval.ModeVector) && hasPinnedEmbeddingSpace {
		return inference.ContextPack{}, invalid(
			"non-vector retrieval cannot carry an embedding-space pin")
	}
	if hasPinnedEmbeddingSpace {
		expected, deriveErr := retrieval.EmbeddingSpaceSetID(
			embeddingSpaceIDs...)
		if deriveErr != nil {
			return inference.ContextPack{}, deriveErr
		}
		if embeddingSpaceID != expected {
			return inference.ContextPack{}, invalid(
				"embedding space set identity is not canonical")
		}
	}
	if request.HasMode(retrieval.ModeVector) {
		if hasPinnedEmbeddingSpace &&
			(embeddingSpaceID != response.EmbeddingSpaceID ||
				!equalIDs(embeddingSpaceIDs, response.EmbeddingSpaceIDs)) {
			return inference.ContextPack{}, invalid(
				"retrieval embedding space does not match the trusted pin")
		}
		embeddingSpaceID = response.EmbeddingSpaceID
		embeddingSpaceIDs = append(
			[]shoal.ID(nil), response.EmbeddingSpaceIDs...)
	}
	embeddingSpaceIDs, err = normalizeEmbeddingSpaceIDs(embeddingSpaceIDs)
	if err != nil {
		return inference.ContextPack{}, err
	}
	if (embeddingSpaceID == "") != (len(embeddingSpaceIDs) == 0) {
		return inference.ContextPack{}, invalid(
			"embedding space aggregate and constituents must be present together")
	}
	if len(embeddingSpaceIDs) > 0 {
		expected, deriveErr := retrieval.EmbeddingSpaceSetID(
			embeddingSpaceIDs...)
		if deriveErr != nil {
			return inference.ContextPack{}, deriveErr
		}
		if embeddingSpaceID != expected {
			return inference.ContextPack{}, invalid(
				"embedding space set identity is not canonical")
		}
	}
	input.Pins.EmbeddingSpaceID = embeddingSpaceID
	input.Pins.EmbeddingSpaceIDs = embeddingSpaceIDs
	metadata, err := provenanceMetadata(
		input.Metadata, request, response, input.Pins.PolicyID,
		embeddingSpaceID, embeddingSpaceIDs, limits)
	if err != nil {
		return inference.ContextPack{}, err
	}
	return buildPack(
		ctx, request.Text, anchors, input.Pins, metadata, limits, b.TokenEstimator)
}

// OpenSection returns a new pack containing the existing verified evidence and
// exact span-backed citations from explicitly selected sections.
func (b Builder) OpenSection(
	ctx context.Context,
	pack inference.ContextPack,
	request OpenSectionRequest,
) (inference.ContextPack, error) {
	if err := contextError(ctx); err != nil {
		return inference.ContextPack{}, err
	}
	if err := pack.Validate(); err != nil {
		return inference.ContextPack{}, err
	}
	limits, err := normalizeLimits(b.Limits)
	if err != nil {
		return inference.ContextPack{}, err
	}
	if b.Reader == nil {
		return inference.ContextPack{}, invalid("section hydration requires an Explorer reader")
	}
	if err := shoal.ValidateRequiredID("document ID", request.DocumentID); err != nil {
		return inference.ContextPack{}, err
	}
	if err := shoal.ValidateRequiredID("revision ID", request.RevisionID); err != nil {
		return inference.ContextPack{}, err
	}
	if request.Depth > limits.MaxHierarchyDepth {
		return inference.ContextPack{}, invalid("section expansion exceeds the hierarchy depth bound")
	}
	if len(request.SectionIDs) > limits.MaxSections {
		return inference.ContextPack{}, invalid("section selection exceeds the section bound")
	}
	if err := validateUniqueIDs("section ID", request.SectionIDs); err != nil {
		return inference.ContextPack{}, err
	}
	if len(request.SectionIDs) == 0 {
		return inference.ContextPack{}, invalid("section expansion requires explicit section IDs")
	}
	view, err := b.Reader.Document(ctx, request.DocumentID, request.RevisionID)
	if err != nil {
		return inference.ContextPack{}, err
	}
	verifier, err := newVerifier(ctx, b.Reader, limits, []explorer.DocumentView{view}, nil)
	if err != nil {
		return inference.ContextPack{}, err
	}
	if err := verifier.verifyExisting(pack.Evidence()); err != nil {
		return inference.ContextPack{}, err
	}
	key := documentKey{request.DocumentID, request.RevisionID}
	index := verifier.documents[key]
	if index == nil {
		return inference.ContextPack{}, invalid("hydrated document identity does not match request")
	}
	selected, err := index.selectSections(request.SectionIDs, request.Depth, limits)
	if err != nil {
		return inference.ContextPack{}, err
	}
	anchors, err := canonicalAnchors(pack.Evidence(), limits.MaxAnchors)
	if err != nil {
		return inference.ContextPack{}, err
	}
	anchorIDs := anchorMap(anchors)
	for _, sectionID := range selected {
		for _, span := range index.spansBySection[sectionID] {
			if len(span.Text) == 0 {
				continue
			}
			citation := document.Citation{
				DocumentID: span.DocumentID,
				RevisionID: span.RevisionID,
				SectionID:  span.SectionID,
				SpanID:     span.ID,
				Range:      span.Range,
			}
			anchor, err := verifier.documentAnchor(citation, span.Text)
			if err != nil {
				return inference.ContextPack{}, err
			}
			if err := appendAnchor(&anchors, anchorIDs, anchor, limits.MaxAnchors); err != nil {
				return inference.ContextPack{}, err
			}
		}
	}
	return rebuildPack(ctx, pack, anchors, limits, b.TokenEstimator)
}

// ExpandNeighbors returns a new pack containing verified existing evidence and
// exact one-node or one-edge paths from an explicitly requested neighborhood.
func (b Builder) ExpandNeighbors(
	ctx context.Context,
	pack inference.ContextPack,
	request ExpandNeighborsRequest,
) (inference.ContextPack, error) {
	if err := contextError(ctx); err != nil {
		return inference.ContextPack{}, err
	}
	if err := pack.Validate(); err != nil {
		return inference.ContextPack{}, err
	}
	limits, err := normalizeLimits(b.Limits)
	if err != nil {
		return inference.ContextPack{}, err
	}
	if b.Reader == nil {
		return inference.ContextPack{}, invalid("graph hydration requires an Explorer reader")
	}
	if len(request.NodeIDs) > limits.MaxGraphNodes {
		return inference.ContextPack{}, invalid("graph seeds exceed the node bound")
	}
	if err := validateUniqueIDs("graph node ID", request.NodeIDs); err != nil {
		return inference.ContextPack{}, err
	}
	if len(request.NodeIDs) == 0 {
		return inference.ContextPack{}, invalid("graph expansion requires explicit node IDs")
	}
	normalized, err := (explorer.NeighborhoodRequest{
		NodeIDs: request.NodeIDs, Depth: request.Depth, EdgeTypes: request.EdgeTypes,
	}).Normalize()
	if err != nil {
		return inference.ContextPack{}, err
	}
	neighborhood, err := b.Reader.Neighborhood(ctx, normalized)
	if err != nil {
		return inference.ContextPack{}, err
	}
	if err := validateNeighborhoodResponse(normalized, neighborhood, limits); err != nil {
		return inference.ContextPack{}, err
	}
	verifier, err := newVerifier(ctx, b.Reader, limits, nil, []explorer.Neighborhood{neighborhood})
	if err != nil {
		return inference.ContextPack{}, err
	}
	if err := verifier.verifyExisting(pack.Evidence()); err != nil {
		return inference.ContextPack{}, err
	}
	anchors, err := canonicalAnchors(pack.Evidence(), limits.MaxAnchors)
	if err != nil {
		return inference.ContextPack{}, err
	}
	anchorIDs := anchorMap(anchors)
	for _, seed := range normalized.NodeIDs {
		node, ok := verifier.nodes[seed]
		if !ok {
			return inference.ContextPack{}, invalid("hydrated neighborhood omitted a requested node")
		}
		anchor, err := verifier.graphAnchor(graph.Path{Nodes: []graph.Node{node}})
		if err != nil {
			return inference.ContextPack{}, err
		}
		if err := appendAnchor(&anchors, anchorIDs, anchor, limits.MaxAnchors); err != nil {
			return inference.ContextPack{}, err
		}
	}
	for _, edge := range sortedEdges(neighborhood.Edges) {
		from, fromOK := verifier.nodes[edge.From]
		to, toOK := verifier.nodes[edge.To]
		if !fromOK || !toOK {
			return inference.ContextPack{}, invalid("hydrated neighborhood edge has a missing endpoint")
		}
		anchor, err := verifier.graphAnchor(graph.Path{
			Nodes: []graph.Node{from, to},
			Edges: []graph.Edge{edge},
		})
		if err != nil {
			return inference.ContextPack{}, err
		}
		if err := appendAnchor(&anchors, anchorIDs, anchor, limits.MaxAnchors); err != nil {
			return inference.ContextPack{}, err
		}
	}
	return rebuildPack(ctx, pack, anchors, limits, b.TokenEstimator)
}

type verifier struct {
	ctx             context.Context
	reader          Reader
	limits          Limits
	documents       map[documentKey]*documentIndex
	nodes           map[shoal.ID]graph.Node
	edges           map[shoal.ID]graph.Edge
	assertions      map[shoal.ID][]interaction.AssertionReference
	assertionStates map[shoal.ID]assertionHydration
	sections        int
	spans           int
	bytes           int
}

type documentKey struct {
	documentID shoal.ID
	revisionID shoal.ID
}

type assertionHydration struct {
	assertion ontology.Assertion
	edgeIDs   []shoal.ID
}

type documentIndex struct {
	view           explorer.DocumentView
	sections       map[shoal.ID]document.Section
	spans          map[shoal.ID]document.Span
	children       map[shoal.ID][]shoal.ID
	spansBySection map[shoal.ID][]document.Span
	bytes          int
}

func newVerifier(
	ctx context.Context,
	reader Reader,
	limits Limits,
	views []explorer.DocumentView,
	neighborhoods []explorer.Neighborhood,
) (*verifier, error) {
	v := &verifier{
		ctx: ctx, reader: reader, limits: limits,
		documents:       make(map[documentKey]*documentIndex),
		nodes:           make(map[shoal.ID]graph.Node),
		edges:           make(map[shoal.ID]graph.Edge),
		assertions:      make(map[shoal.ID][]interaction.AssertionReference),
		assertionStates: make(map[shoal.ID]assertionHydration),
	}
	if len(views) > limits.MaxDocuments {
		return nil, invalid("hydrated documents exceed the document bound")
	}
	for _, view := range views {
		if err := v.addDocument(view); err != nil {
			return nil, err
		}
	}
	for _, neighborhood := range neighborhoods {
		if err := v.addNeighborhood(neighborhood); err != nil {
			return nil, err
		}
	}
	return v, nil
}

func (v *verifier) documentAnchor(
	citation document.Citation,
	quote string,
) (inference.EvidenceAnchor, error) {
	if err := contextError(v.ctx); err != nil {
		return inference.EvidenceAnchor{}, err
	}
	if err := citation.Validate(); err != nil {
		return inference.EvidenceAnchor{}, err
	}
	key := documentKey{citation.DocumentID, citation.RevisionID}
	index := v.documents[key]
	if index == nil {
		if v.reader == nil {
			return inference.EvidenceAnchor{}, invalid(
				"citation hydration is required for verification")
		}
		view, err := v.reader.Document(v.ctx, citation.DocumentID, citation.RevisionID)
		if err != nil {
			return inference.EvidenceAnchor{}, err
		}
		if err := v.addDocument(view); err != nil {
			return inference.EvidenceAnchor{}, err
		}
		index = v.documents[key]
		if index == nil {
			return inference.EvidenceAnchor{}, invalid(
				"hydrated document identity does not match citation")
		}
	}
	if err := index.validateCitation(citation, quote); err != nil {
		return inference.EvidenceAnchor{}, err
	}
	return inference.NewDocumentAnchor(citation, quote)
}

func (v *verifier) graphAnchor(path graph.Path) (inference.EvidenceAnchor, error) {
	if err := contextError(v.ctx); err != nil {
		return inference.EvidenceAnchor{}, err
	}
	if len(path.Nodes) > v.limits.MaxPathNodes {
		return inference.EvidenceAnchor{}, invalid("graph path exceeds the builder path bound")
	}
	if err := path.Validate(); err != nil {
		return inference.EvidenceAnchor{}, err
	}
	missing := false
	nodeIDs := make([]shoal.ID, 0, len(path.Nodes))
	path = clonePath(path)
	for index, node := range path.Nodes {
		node = canonicalNode(node)
		path.Nodes[index] = node
		nodeIDs = append(nodeIDs, node.ID)
		if existing, ok := v.nodes[node.ID]; !ok || !canonicalEqual(existing, node) {
			missing = true
		}
	}
	for _, edge := range path.Edges {
		if existing, ok := v.edges[edge.ID]; !ok || !canonicalEqual(existing, edge) {
			missing = true
		}
	}
	if missing {
		if v.reader == nil {
			return inference.EvidenceAnchor{}, invalid(
				"graph hydration is required for verification")
		}
		neighborhood, err := v.reader.Neighborhood(v.ctx, explorer.NeighborhoodRequest{
			NodeIDs: nodeIDs,
			Depth:   1,
		})
		if err != nil {
			return inference.EvidenceAnchor{}, err
		}
		request := explorer.NeighborhoodRequest{NodeIDs: nodeIDs, Depth: 1}
		if err := validateNeighborhoodResponse(request, neighborhood, v.limits); err != nil {
			return inference.EvidenceAnchor{}, err
		}
		if err := v.addNeighborhood(neighborhood); err != nil {
			return inference.EvidenceAnchor{}, err
		}
	}
	for _, node := range path.Nodes {
		existing, ok := v.nodes[node.ID]
		if !ok || !canonicalEqual(existing, node) {
			return inference.EvidenceAnchor{}, invalid(
				"graph path node does not match hydrated Explorer data")
		}
	}
	for _, edge := range path.Edges {
		existing, ok := v.edges[edge.ID]
		if !ok || !canonicalEqual(existing, edge) {
			return inference.EvidenceAnchor{}, invalid(
				"graph path edge does not match hydrated Explorer data")
		}
	}
	var assertions []interaction.AssertionReference
	for _, edge := range path.Edges {
		for _, assertion := range v.assertions[edge.ID] {
			switch assertion.Origin {
			case ontology.AssertionExplicit, ontology.AssertionInferred:
				assertions = append(assertions, assertion)
			case ontology.AssertionDerived:
				return inference.EvidenceAnchor{}, invalid(
					"graph path contains non-source ontology assertion")
			default:
				return inference.EvidenceAnchor{}, invalid(
					"graph path contains assertion with unknown origin")
			}
		}
	}
	return inference.NewGraphAnchorWithAssertions(path, assertions)
}

func (v *verifier) verifyExisting(anchors []inference.EvidenceAnchor) error {
	for _, anchor := range anchors {
		switch anchor.Kind() {
		case inference.AnchorDocument:
			citation, quote, ok := anchor.Document()
			if !ok {
				return invalid("document anchor variant is unavailable")
			}
			verified, err := v.documentAnchor(citation, quote)
			if err != nil {
				return fmt.Errorf("existing document anchor %q: %w", anchor.ID(), err)
			}
			if verified.ID() != anchor.ID() {
				return invalid("existing document anchor identity is not authoritative")
			}
		case inference.AnchorGraph:
			path, ok := anchor.Path()
			if !ok {
				return invalid("graph anchor variant is unavailable")
			}
			verified, err := v.graphAnchor(path)
			if err != nil {
				return fmt.Errorf("existing graph anchor %q: %w", anchor.ID(), err)
			}
			if verified.ID() != anchor.ID() {
				return invalid(
					"existing graph anchor omits authoritative assertion identity")
			}
		default:
			return invalid("unknown evidence anchor kind")
		}
	}
	return nil
}

func (v *verifier) addDocument(view explorer.DocumentView) error {
	key := documentKey{view.Document.ID, view.Revision.ID}
	if existing := v.documents[key]; existing != nil {
		index, err := indexDocument(
			view, v.limits.MaxSections, v.limits.MaxSpans, v.limits.MaxHydrationBytes)
		if err != nil {
			return err
		}
		if !canonicalEqual(existing.view, index.view) {
			return invalid("duplicate hydrated document has different content")
		}
		return nil
	}
	remaining := v.limits.MaxSections - v.sections
	remainingSpans := v.limits.MaxSpans - v.spans
	remainingBytes := v.limits.MaxHydrationBytes - v.bytes
	index, err := indexDocument(view, remaining, remainingSpans, remainingBytes)
	if err != nil {
		return err
	}
	if len(v.documents)+1 > v.limits.MaxDocuments {
		return invalid("hydrated documents exceed the document bound")
	}
	if v.sections+len(index.sections) > v.limits.MaxSections {
		return invalid("hydrated documents exceed the section bound")
	}
	v.sections += len(index.sections)
	v.spans += len(index.spans)
	v.bytes += index.bytes
	v.documents[key] = index
	return nil
}

func (v *verifier) addNeighborhood(neighborhood explorer.Neighborhood) error {
	if len(neighborhood.Nodes) > v.limits.MaxGraphNodes {
		return invalid("hydrated graph exceeds the node bound")
	}
	if len(neighborhood.Edges) > v.limits.MaxGraphEdges {
		return invalid("hydrated graph exceeds the edge bound")
	}
	if len(neighborhood.Assertions) > v.limits.MaxGraphEdges {
		return invalid("hydrated graph exceeds the assertion bound")
	}
	payloadBytes, err := neighborhoodPayloadBytes(neighborhood)
	if err != nil {
		return err
	}
	if payloadBytes > v.limits.MaxHydrationBytes {
		return invalid("hydrated graph exceeds the byte bound")
	}
	localNodes := make(map[shoal.ID]graph.Node, len(neighborhood.Nodes))
	for _, node := range neighborhood.Nodes {
		node = canonicalNode(node)
		if existing, ok := localNodes[node.ID]; ok {
			if !canonicalEqual(existing, node) {
				return invalid("duplicate hydrated graph node has different content")
			}
			continue
		}
		localNodes[node.ID] = node
	}
	localEdges := make(map[shoal.ID]graph.Edge, len(neighborhood.Edges))
	for _, edge := range neighborhood.Edges {
		edge = cloneEdge(edge)
		if _, ok := localNodes[edge.From]; !ok {
			return invalid("hydrated graph edge source is missing")
		}
		if _, ok := localNodes[edge.To]; !ok {
			return invalid("hydrated graph edge target is missing")
		}
		if existing, ok := localEdges[edge.ID]; ok {
			if !canonicalEqual(existing, edge) {
				return invalid("duplicate hydrated graph edge has different content")
			}
			continue
		}
		localEdges[edge.ID] = edge
	}
	localAssertions, localAssertionStates, err := assertionsByNeighborhoodEdge(
		neighborhood.Assertions, localEdges)
	if err != nil {
		return err
	}
	for assertionID, state := range localAssertionStates {
		if existing, ok := v.assertionStates[assertionID]; ok &&
			!assertionsSemanticallyEqual(existing.assertion, state.assertion) {
			return invalid("hydrated graph assertion conflicts with prior content")
		}
	}
	additionalAssertions := 0
	for assertionID := range localAssertionStates {
		if _, exists := v.assertionStates[assertionID]; exists {
			continue
		}
		additionalAssertions++
		if len(v.assertionStates)+additionalAssertions > v.limits.MaxGraphEdges {
			return invalid("hydrated graph exceeds the assertion bound")
		}
	}
	additionalBytes := 0
	additionalNodes := 0
	for id, node := range localNodes {
		if existing, ok := v.nodes[id]; ok {
			if !canonicalEqual(existing, node) {
				return invalid("hydrated graph node conflicts with prior content")
			}
			continue
		}
		additionalNodes++
		if len(v.nodes)+additionalNodes > v.limits.MaxGraphNodes {
			return invalid("hydrated graph exceeds the node bound")
		}
		var ok bool
		additionalBytes, ok = addBounded(
			additionalBytes, nodePayloadBytes(node), v.limits.MaxHydrationBytes-v.bytes)
		if !ok {
			return invalid("hydrated graph exceeds the byte bound")
		}
	}
	additionalEdges := 0
	for id, edge := range localEdges {
		if existing, ok := v.edges[id]; ok {
			if !canonicalEqual(existing, edge) {
				return invalid("hydrated graph edge conflicts with prior content")
			}
			for edgeID, assertions := range localAssertions {
				existing := v.assertions[edgeID]
				if len(existing) != 0 &&
					!reflect.DeepEqual(existing, assertions) {
					return invalid("hydrated graph assertion conflicts with prior content")
				}
			}
			continue
		}
		additionalEdges++
		if len(v.edges)+additionalEdges > v.limits.MaxGraphEdges {
			return invalid("hydrated graph exceeds the edge bound")
		}
		var ok bool
		additionalBytes, ok = addBounded(
			additionalBytes, edgePayloadBytes(edge), v.limits.MaxHydrationBytes-v.bytes)
		if !ok {
			return invalid("hydrated graph exceeds the byte bound")
		}
	}
	assertionIDs := make([]shoal.ID, 0, len(localAssertionStates))
	for assertionID := range localAssertionStates {
		assertionIDs = append(assertionIDs, assertionID)
	}
	sort.Slice(assertionIDs, func(i, j int) bool {
		return shoal.CompareID(assertionIDs[i], assertionIDs[j]) < 0
	})
	for _, assertionID := range assertionIDs {
		state := localAssertionStates[assertionID]
		existing, exists := v.assertionStates[assertionID]
		if !exists {
			var ok bool
			additionalBytes, ok = addBounded(
				additionalBytes,
				assertionHydrationPayloadBytes(state),
				v.limits.MaxHydrationBytes-v.bytes,
			)
			if !ok {
				return invalid("hydrated graph assertion exceeds the byte bound")
			}
			continue
		}
		existingEdges := make(map[shoal.ID]struct{}, len(existing.edgeIDs))
		for _, edgeID := range existing.edgeIDs {
			existingEdges[edgeID] = struct{}{}
		}
		for _, edgeID := range state.edgeIDs {
			if _, retained := existingEdges[edgeID]; retained {
				continue
			}
			var ok bool
			additionalBytes, ok = addBounded(
				additionalBytes,
				assertionReferencePayloadBytes(
					assertionID, edgeID, state.assertion.Origin()),
				v.limits.MaxHydrationBytes-v.bytes,
			)
			if !ok {
				return invalid("hydrated graph assertion exceeds the byte bound")
			}
		}
	}
	for id, node := range localNodes {
		if _, exists := v.nodes[id]; !exists {
			v.nodes[id] = cloneNode(node)
		}
	}
	for id, edge := range localEdges {
		if _, exists := v.edges[id]; !exists {
			v.edges[id] = cloneEdge(edge)
		}
	}
	for edgeID, assertions := range localAssertions {
		if existing, exists := v.assertions[edgeID]; exists {
			v.assertions[edgeID] = mergeAssertionReferences(existing, assertions)
		} else {
			v.assertions[edgeID] = append(
				[]interaction.AssertionReference(nil), assertions...)
		}
	}
	for assertionID, state := range localAssertionStates {
		if existing, exists := v.assertionStates[assertionID]; exists {
			existing.edgeIDs = mergeSortedIDs(existing.edgeIDs, state.edgeIDs)
			v.assertionStates[assertionID] = existing
		} else {
			v.assertionStates[assertionID] = assertionHydration{
				assertion: state.assertion,
				edgeIDs:   append([]shoal.ID(nil), state.edgeIDs...),
			}
		}
	}
	v.bytes += additionalBytes
	return nil
}

func assertionsByNeighborhoodEdge(
	assertions []ontology.Assertion,
	edges map[shoal.ID]graph.Edge,
) (
	map[shoal.ID][]interaction.AssertionReference,
	map[shoal.ID]assertionHydration,
	error,
) {
	const (
		assertionEdgeIDProperty = "shoal.graph.edge_id"
		assertionIDProperty     = "ontology.assertion.id"
	)
	result := make(map[shoal.ID][]interaction.AssertionReference)
	byID := make(map[shoal.ID]assertionHydration, len(assertions))
	for _, assertion := range assertions {
		if err := assertion.Validate(); err != nil {
			return nil, nil, err
		}
		matched := make(map[shoal.ID]struct{})
		if edgeID := shoal.ID(
			assertion.Metadata()[assertionEdgeIDProperty]); edgeID != "" {
			if edge, ok := edges[edgeID]; ok {
				if !assertionMatchesEvidenceEdge(assertion, edge) {
					return nil, nil, invalid(
						"hydrated assertion does not match its graph edge")
				}
				matched[edgeID] = struct{}{}
			}
		}
		if edge, ok := edges[assertion.ID()]; ok {
			if !assertionMatchesEvidenceEdge(assertion, edge) {
				return nil, nil, invalid(
					"hydrated assertion does not match its graph edge")
			}
			matched[assertion.ID()] = struct{}{}
		}
		for edgeID, edge := range edges {
			if shoal.ID(edge.Properties[assertionIDProperty]) == assertion.ID() {
				if assertion.Origin() != ontology.AssertionDerived {
					return nil, nil, invalid(
						"non-derived assertion has a derivation edge")
				}
				matched[edgeID] = struct{}{}
			}
		}
		if len(matched) == 0 {
			return nil, nil, invalid(
				"hydrated graph assertion has no authoritative edge")
		}
		edgeIDs := make([]shoal.ID, 0, len(matched))
		for edgeID := range matched {
			edgeIDs = append(edgeIDs, edgeID)
		}
		sort.Slice(edgeIDs, func(i, j int) bool {
			return shoal.CompareID(edgeIDs[i], edgeIDs[j]) < 0
		})
		state := assertionHydration{
			assertion: assertion,
			edgeIDs:   edgeIDs,
		}
		if existing, duplicate := byID[assertion.ID()]; duplicate {
			if !assertionsSemanticallyEqual(existing.assertion, state.assertion) ||
				!reflect.DeepEqual(existing.edgeIDs, state.edgeIDs) {
				return nil, nil, invalid(
					"hydrated graph repeats an assertion identity with different content")
			}
			continue
		}
		byID[assertion.ID()] = state
		for edgeID := range matched {
			result[edgeID] = append(
				result[edgeID],
				interaction.AssertionReference{
					AssertionID: assertion.ID(),
					EdgeID:      edgeID,
					Origin:      assertion.Origin(),
				},
			)
		}
	}
	for edgeID := range result {
		sort.Slice(result[edgeID], func(i, j int) bool {
			if compared := shoal.CompareID(
				result[edgeID][i].AssertionID,
				result[edgeID][j].AssertionID,
			); compared != 0 {
				return compared < 0
			}
			return result[edgeID][i].Origin < result[edgeID][j].Origin
		})
	}
	return result, byID, nil
}

func mergeSortedIDs(left, right []shoal.ID) []shoal.ID {
	merged := append(append([]shoal.ID(nil), left...), right...)
	sort.Slice(merged, func(i, j int) bool {
		return shoal.CompareID(merged[i], merged[j]) < 0
	})
	result := merged[:0]
	for _, id := range merged {
		if len(result) == 0 || result[len(result)-1] != id {
			result = append(result, id)
		}
	}
	return result
}

func mergeAssertionReferences(
	left, right []interaction.AssertionReference,
) []interaction.AssertionReference {
	merged := append(
		append([]interaction.AssertionReference(nil), left...), right...)
	sort.Slice(merged, func(i, j int) bool {
		if compared := shoal.CompareID(
			merged[i].AssertionID, merged[j].AssertionID); compared != 0 {
			return compared < 0
		}
		if compared := shoal.CompareID(
			merged[i].EdgeID, merged[j].EdgeID); compared != 0 {
			return compared < 0
		}
		return merged[i].Origin < merged[j].Origin
	})
	result := merged[:0]
	for _, reference := range merged {
		if len(result) == 0 || result[len(result)-1] != reference {
			result = append(result, reference)
		}
	}
	return result
}

func assertionsSemanticallyEqual(
	left, right ontology.Assertion,
) bool {
	if left.ID() != right.ID() ||
		!metadataSemanticallyEqual(left.Metadata(), right.Metadata()) {
		return false
	}
	leftEvidence := left.Evidence()
	rightEvidence := right.Evidence()
	if len(leftEvidence) != len(rightEvidence) {
		return false
	}
	for index := range leftEvidence {
		if leftEvidence[index].ID() != rightEvidence[index].ID() ||
			!metadataSemanticallyEqual(
				leftEvidence[index].Metadata(),
				rightEvidence[index].Metadata(),
			) {
			return false
		}
	}
	return true
}

func metadataSemanticallyEqual(left, right shoal.Metadata) bool {
	if len(left) != len(right) {
		return false
	}
	for key, leftValue := range left {
		rightValue, ok := right[key]
		if !ok || rightValue != leftValue {
			return false
		}
	}
	return true
}

func assertionMatchesEvidenceEdge(
	assertion ontology.Assertion,
	edge graph.Edge,
) bool {
	target, ok := assertion.Object().ReferenceValue()
	return ok &&
		edge.From == assertion.Subject() &&
		edge.To == target &&
		edge.Type == string(assertion.Predicate()) &&
		math.Float64bits(float64(edge.Weight)) ==
			math.Float64bits(float64(assertion.Confidence()))
}

func indexDocument(
	view explorer.DocumentView,
	maxSections, maxSpans, maxBytes int,
) (*documentIndex, error) {
	if err := view.Document.Validate(); err != nil {
		return nil, err
	}
	if err := view.Revision.Validate(); err != nil {
		return nil, err
	}
	if view.Document.RevisionID != view.Revision.ID ||
		view.Revision.DocumentID != view.Document.ID {
		return nil, invalid("hydrated document and revision ownership do not match")
	}
	sections, spans, hydrationBytes := sectionViewStats(view)
	if maxSections <= 0 || sections > maxSections {
		return nil, invalid("hydrated documents exceed the section bound")
	}
	if maxSpans <= 0 || spans > maxSpans {
		return nil, invalid("hydrated documents exceed the span bound")
	}
	if maxBytes <= 0 || hydrationBytes > maxBytes {
		return nil, invalid("hydrated documents exceed the byte bound")
	}
	index := &documentIndex{
		view:           cloneView(view),
		sections:       make(map[shoal.ID]document.Section),
		spans:          make(map[shoal.ID]document.Span),
		children:       make(map[shoal.ID][]shoal.ID),
		spansBySection: make(map[shoal.ID][]document.Span),
		bytes:          hydrationBytes,
	}
	var walk func(explorer.SectionView, shoal.ID) error
	walk = func(sectionView explorer.SectionView, expectedParent shoal.ID) error {
		section := sectionView.Section
		if err := section.Validate(); err != nil {
			return err
		}
		if section.DocumentID != view.Document.ID ||
			section.RevisionID != view.Revision.ID ||
			section.ParentID != expectedParent {
			return invalid("hydrated section ownership or parent does not match document")
		}
		if _, duplicate := index.sections[section.ID]; duplicate {
			return invalid("hydrated document has duplicate section IDs")
		}
		index.sections[section.ID] = section
		if expectedParent != "" {
			index.children[expectedParent] = append(index.children[expectedParent], section.ID)
		}
		for _, span := range sectionView.Spans {
			if err := span.Validate(); err != nil {
				return err
			}
			if span.DocumentID != view.Document.ID ||
				span.RevisionID != view.Revision.ID ||
				span.SectionID != section.ID {
				return invalid("hydrated span ownership does not match section")
			}
			if !rangeContains(section.Range, span.Range) {
				return invalid("hydrated span range is outside its section")
			}
			if span.Range.End.Offset-span.Range.Start.Offset != int64(len(span.Text)) {
				return invalid("hydrated span text length does not match its range")
			}
			if _, duplicate := index.spans[span.ID]; duplicate {
				return invalid("hydrated document has duplicate span IDs")
			}
			index.spans[span.ID] = span
			index.spansBySection[section.ID] = append(index.spansBySection[section.ID], span)
		}
		for _, child := range sectionView.Children {
			if !rangeContains(section.Range, child.Section.Range) {
				return invalid("hydrated child section range is outside its parent")
			}
			if err := walk(child, section.ID); err != nil {
				return err
			}
		}
		return nil
	}
	if view.Root.Section.ID != view.Document.RootSectionID {
		return nil, invalid("hydrated document root section does not match document")
	}
	if err := walk(view.Root, ""); err != nil {
		return nil, err
	}
	for id := range index.children {
		sort.Slice(index.children[id], func(i, j int) bool {
			return shoal.CompareID(index.children[id][i], index.children[id][j]) < 0
		})
	}
	for id := range index.spansBySection {
		sort.Slice(index.spansBySection[id], func(i, j int) bool {
			return shoal.CompareID(
				index.spansBySection[id][i].ID,
				index.spansBySection[id][j].ID,
			) < 0
		})
	}
	return index, nil
}

func (d *documentIndex) validateCitation(citation document.Citation, quote string) error {
	if !utf8.ValidString(quote) || len(quote) == 0 {
		return invalid("citation quote must be nonempty valid UTF-8")
	}
	if citation.DocumentID != d.view.Document.ID ||
		citation.RevisionID != d.view.Revision.ID {
		return invalid("citation ownership does not match hydrated revision")
	}
	if citation.SectionID != "" {
		section, ok := d.sections[citation.SectionID]
		if !ok {
			return shoal.NewError(shoal.ErrorNotFound, "cited section was not found")
		}
		if !rangeContains(section.Range, citation.Range) {
			return invalid("citation range is outside cited section")
		}
	}
	if citation.SpanID != "" {
		span, ok := d.spans[citation.SpanID]
		if !ok {
			return shoal.NewError(shoal.ErrorNotFound, "cited span was not found")
		}
		if citation.SectionID != "" && span.SectionID != citation.SectionID {
			return invalid("cited span does not belong to cited section")
		}
		return validateQuoteInSpan(span, citation.Range, quote)
	}
	for _, span := range d.spansBySection[citation.SectionID] {
		if rangeContains(span.Range, citation.Range) {
			return validateQuoteInSpan(span, citation.Range, quote)
		}
	}
	return invalid("section citation cannot be verified from one public span")
}

func (d *documentIndex) selectSections(
	ids []shoal.ID,
	depth uint32,
	limits Limits,
) ([]shoal.ID, error) {
	selected := make(map[shoal.ID]struct{})
	frontier := append([]shoal.ID(nil), ids...)
	for _, id := range frontier {
		if _, ok := d.sections[id]; !ok {
			return nil, shoal.NewError(shoal.ErrorNotFound, "requested section was not found")
		}
	}
	for level := uint32(0); len(frontier) > 0 && level <= depth; level++ {
		nextSet := make(map[shoal.ID]struct{})
		for _, id := range frontier {
			if _, exists := selected[id]; exists {
				continue
			}
			selected[id] = struct{}{}
			if level < depth {
				for _, child := range d.children[id] {
					if _, exists := selected[child]; !exists {
						nextSet[child] = struct{}{}
					}
				}
			}
		}
		if len(selected)+len(nextSet) > limits.MaxSections {
			return nil, invalid("section expansion exceeds the section bound")
		}
		next := make([]shoal.ID, 0, len(nextSet))
		for id := range nextSet {
			next = append(next, id)
		}
		sort.Slice(next, func(i, j int) bool {
			return shoal.CompareID(next[i], next[j]) < 0
		})
		frontier = next
	}
	result := make([]shoal.ID, 0, len(selected))
	for id := range selected {
		result = append(result, id)
	}
	sort.Slice(result, func(i, j int) bool {
		return shoal.CompareID(result[i], result[j]) < 0
	})
	return result, nil
}

func validateQuoteInSpan(span document.Span, quotedRange document.SourceRange, quote string) error {
	if !rangeContains(span.Range, quotedRange) {
		return invalid("citation range is outside cited span")
	}
	start := quotedRange.Start.Offset - span.Range.Start.Offset
	end := quotedRange.End.Offset - span.Range.Start.Offset
	if start < 0 || end < start || end > int64(len(span.Text)) {
		return invalid("citation range exceeds hydrated span text")
	}
	if !utf8.ValidString(span.Text) ||
		(start > 0 && start < int64(len(span.Text)) && !utf8.RuneStart(span.Text[start])) ||
		(end > 0 && end < int64(len(span.Text)) && !utf8.RuneStart(span.Text[end])) {
		return invalid("citation range is not a UTF-8 boundary")
	}
	if span.Text[start:end] != quote {
		return invalid("citation quote does not match hydrated source bytes")
	}
	return nil
}

func provenanceMetadata(
	input shoal.Metadata,
	request retrieval.Request,
	response retrieval.Response,
	policyID shoal.ID,
	embeddingSpaceID shoal.ID,
	embeddingSpaceIDs []shoal.ID,
	limits Limits,
) (shoal.Metadata, error) {
	if err := shoal.ValidateRequiredID("policy ID", policyID); err != nil {
		return nil, err
	}
	if err := shoal.ValidateOptionalID(
		"embedding space ID", embeddingSpaceID,
	); err != nil {
		return nil, err
	}
	for _, id := range embeddingSpaceIDs {
		if err := shoal.ValidateRequiredID(
			"embedding space constituent ID", id,
		); err != nil {
			return nil, err
		}
	}
	metadata := cloneMetadata(input)
	if metadata == nil {
		metadata = make(shoal.Metadata)
	}
	for _, key := range []string{
		metadataBuilderKey, metadataRequestKey, metadataRetrievalKey,
		metadataPolicyKey,
	} {
		if _, exists := metadata[key]; exists {
			return nil, invalid("context metadata uses a reserved builder key")
		}
	}
	identity, err := retrievalIdentity(request, response)
	if err != nil {
		return nil, err
	}
	metadata[metadataBuilderKey] = builderVersion
	metadata[metadataRequestKey] = encodeID(response.RequestID)
	metadata[metadataRetrievalKey] = identity
	metadata[metadataPolicyKey] = encodeID(policyID)
	if embeddingSpaceID != "" {
		metadata[metadataEmbeddingSpaceKey] = encodeID(embeddingSpaceID)
	}
	if len(embeddingSpaceIDs) > 0 {
		metadata[metadataEmbeddingSpacesKey] =
			encodeIDs(embeddingSpaceIDs)
	}
	if metadataBytes(metadata) > limits.MaxProvenanceBytes {
		return nil, invalid("context metadata and provenance exceed the byte bound")
	}
	if err := shoal.ValidateMetadata("context metadata", metadata); err != nil {
		return nil, err
	}
	return metadata, nil
}

// EmbeddingSpaceID returns the trusted opaque vector-space identity pinned by
// Builder when the context was assembled. The boolean is false for non-vector
// contexts. Malformed reserved metadata fails closed.
func EmbeddingSpaceID(pack inference.ContextPack) (shoal.ID, bool, error) {
	metadata := pack.Metadata()
	identity, found, err := embeddingSpaceIDFromMetadata(metadata)
	if err != nil {
		return "", false, err
	}
	constituents, err := embeddingSpaceIDsFromMetadata(metadata)
	if err != nil {
		return "", false, err
	}
	_, constituentsPresent := metadata[metadataEmbeddingSpacesKey]
	if found != constituentsPresent ||
		(found && len(constituents) == 0) {
		return "", false, invalid(
			"context embedding space aggregate and constituents are inconsistent")
	}
	if found {
		expected, deriveErr := retrieval.EmbeddingSpaceSetID(constituents...)
		if deriveErr != nil {
			return "", false, deriveErr
		}
		if !found || identity != expected {
			return "", false, invalid(
				"context embedding space set identity is not canonical")
		}
	}
	return identity, found, nil
}

// EmbeddingSpaceIDs returns the canonical constituent space identities pinned
// by Builder. The values are opaque digests, never provider credentials.
func EmbeddingSpaceIDs(pack inference.ContextPack) ([]shoal.ID, error) {
	if _, _, err := EmbeddingSpaceID(pack); err != nil {
		return nil, err
	}
	return embeddingSpaceIDsFromMetadata(pack.Metadata())
}

func embeddingSpaceIDFromMetadata(
	metadata shoal.Metadata,
) (shoal.ID, bool, error) {
	value := metadata[metadataEmbeddingSpaceKey]
	if value == "" {
		return "", false, nil
	}
	const prefix = "hex:"
	if !strings.HasPrefix(value, prefix) {
		return "", false, invalid("context embedding space identity is invalid")
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(value, prefix))
	if err != nil {
		return "", false, invalid("context embedding space identity is invalid")
	}
	id := shoal.ID(decoded)
	if err := shoal.ValidateRequiredID("embedding space ID", id); err != nil {
		return "", false, err
	}
	return id, true, nil
}

func embeddingSpaceIDsFromMetadata(
	metadata shoal.Metadata,
) ([]shoal.ID, error) {
	value := metadata[metadataEmbeddingSpacesKey]
	if value == "" {
		return nil, nil
	}
	parts := strings.Split(value, ",")
	ids := make([]shoal.ID, len(parts))
	for index, part := range parts {
		const prefix = "hex:"
		if !strings.HasPrefix(part, prefix) {
			return nil, invalid(
				"context embedding space constituents are invalid")
		}
		decoded, err := hex.DecodeString(strings.TrimPrefix(part, prefix))
		if err != nil {
			return nil, invalid(
				"context embedding space constituents are invalid")
		}
		ids[index] = shoal.ID(decoded)
		if err := shoal.ValidateRequiredID(
			"embedding space constituent ID", ids[index],
		); err != nil {
			return nil, err
		}
		if index > 0 && shoal.CompareID(ids[index-1], ids[index]) >= 0 {
			return nil, invalid(
				"context embedding space constituents are not canonical")
		}
	}
	return ids, nil
}

func normalizeEmbeddingSpaceIDs(ids []shoal.ID) ([]shoal.ID, error) {
	normalized := append([]shoal.ID(nil), ids...)
	for _, id := range normalized {
		if err := shoal.ValidateRequiredID(
			"embedding space constituent ID", id,
		); err != nil {
			return nil, err
		}
	}
	sort.Slice(normalized, func(i, j int) bool {
		return shoal.CompareID(normalized[i], normalized[j]) < 0
	})
	unique := normalized[:0]
	for _, id := range normalized {
		if len(unique) > 0 && unique[len(unique)-1] == id {
			continue
		}
		unique = append(unique, id)
	}
	return unique, nil
}

func equalIDs(left, right []shoal.ID) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// MergeEmbeddingSpaceMetadata returns independently owned context metadata
// whose embedding-space pin covers both the existing context and one new tool
// result. Repeated identities are idempotent; distinct identities produce a
// stable opaque set identity.
func MergeEmbeddingSpaceMetadata(
	metadata shoal.Metadata, next []shoal.ID,
) (shoal.Metadata, error) {
	for _, id := range next {
		if err := shoal.ValidateRequiredID(
			"embedding space constituent ID", id,
		); err != nil {
			return nil, err
		}
	}
	merged := cloneMetadata(metadata)
	current, err := embeddingSpaceIDsFromMetadata(merged)
	if err != nil {
		return nil, err
	}
	currentIdentity, found, err := embeddingSpaceIDFromMetadata(merged)
	if err != nil {
		return nil, err
	}
	_, constituentsPresent := merged[metadataEmbeddingSpacesKey]
	if found != constituentsPresent ||
		(found && len(current) == 0) {
		return nil, invalid(
			"context embedding space aggregate and constituents are inconsistent")
	}
	if found {
		expected, deriveErr := retrieval.EmbeddingSpaceSetID(current...)
		if deriveErr != nil {
			return nil, deriveErr
		}
		if !found || currentIdentity != expected {
			return nil, invalid(
				"context embedding space set identity is not canonical")
		}
	} else if found {
		return nil, invalid(
			"aggregate-only embedding space provenance cannot be merged")
	}
	if len(next) == 0 {
		return merged, nil
	}
	constituents := append(current, next...)
	sort.Slice(constituents, func(i, j int) bool {
		return shoal.CompareID(constituents[i], constituents[j]) < 0
	})
	unique := constituents[:0]
	for _, id := range constituents {
		if len(unique) > 0 && unique[len(unique)-1] == id {
			continue
		}
		unique = append(unique, id)
	}
	identity, err := retrieval.EmbeddingSpaceSetID(unique...)
	if err != nil {
		return nil, err
	}
	if merged == nil {
		merged = make(shoal.Metadata)
	}
	merged[metadataEmbeddingSpaceKey] = encodeID(identity)
	merged[metadataEmbeddingSpacesKey] = encodeIDs(unique)
	if err := shoal.ValidateMetadata("context metadata", merged); err != nil {
		return nil, err
	}
	return merged, nil
}

func preflightResponse(
	response retrieval.Response,
	selection EvidenceSelection,
	limits Limits,
) error {
	if len(response.Results) > limits.MaxResults {
		return invalid("context retrieval exceeds the result bound")
	}
	evidenceItems := 0
	selectedAnchors := 0
	quoteBytes := 0
	graphNodes := 0
	graphEdges := 0
	graphBytes := 0
	explanationBytes := 0
	for _, result := range response.Results {
		for _, evidence := range result.Evidence {
			evidenceItems++
			if evidenceItems > limits.MaxAnchors {
				return invalid("retrieval evidence exceeds the hydration bound")
			}
			quoteBytes += len(evidence.Quote)
			if quoteBytes > limits.MaxQuoteBytes {
				return invalid("retrieval evidence exceeds the total quote byte bound")
			}
			if selection.Documents {
				selectedAnchors++
			}
			if pathPresent(evidence.Path) {
				if err := evidence.Path.Validate(); err != nil {
					return err
				}
				if len(evidence.Path.Nodes) > limits.MaxPathNodes {
					return invalid("retrieval graph path exceeds the path bound")
				}
				graphNodes += len(evidence.Path.Nodes)
				graphEdges += len(evidence.Path.Edges)
				if graphNodes > limits.MaxGraphNodes {
					return invalid("retrieval evidence exceeds the graph node bound")
				}
				if graphEdges > limits.MaxGraphEdges {
					return invalid("retrieval evidence exceeds the graph edge bound")
				}
				var ok bool
				graphBytes, ok = addBounded(
					graphBytes, pathPayloadBytes(evidence.Path), limits.MaxHydrationBytes)
				if !ok {
					return invalid("retrieval graph evidence exceeds the hydration byte bound")
				}
				if selection.Paths {
					selectedAnchors++
				}
			}
			if selectedAnchors > limits.MaxAnchors {
				return invalid("context evidence exceeds the anchor bound")
			}
		}
		if result.Explanation != nil {
			bytes, err := explanationPreflightBytes(*result.Explanation)
			if err != nil {
				return err
			}
			var ok bool
			explanationBytes, ok = addBounded(
				explanationBytes, bytes, limits.MaxProvenanceBytes)
			if !ok {
				return invalid("retrieval explanation exceeds the provenance byte bound")
			}
		}
	}
	return nil
}

func explanationPreflightBytes(explanation retrieval.Explanation) (int, error) {
	total := 24 + len(explanation.Summary)
	if len(explanation.Summary) > retrieval.MaxExplanationBytes {
		return 0, invalid("explanation summary exceeds the public byte bound")
	}
	for _, mode := range explanation.Modes {
		switch mode {
		case retrieval.ModeLexical, retrieval.ModeVector,
			retrieval.ModeTree, retrieval.ModeGraph:
		default:
			return 0, invalid("explanation contains an unknown mode")
		}
		var ok bool
		total, ok = addBounded(total, 8+len(mode), int(^uint(0)>>1))
		if !ok {
			return 0, invalid("retrieval explanation byte size overflows")
		}
	}
	for name, score := range explanation.Scores {
		if err := shoal.ValidateSemanticString("explanation score name", name); err != nil {
			return 0, err
		}
		if err := shoal.ValidateFiniteScore("explanation score", score); err != nil {
			return 0, err
		}
		var ok bool
		total, ok = addBounded(total, 16+len(name), int(^uint(0)>>1))
		if !ok {
			return 0, invalid("retrieval explanation byte size overflows")
		}
	}
	return total, nil
}

func retrievalIdentity(request retrieval.Request, response retrieval.Response) (string, error) {
	digest := sha256.New()
	writePart(digest, []byte(builderVersion))
	writePart(digest, []byte(request.Text))
	writeUint64(digest, uint64(request.TopK))
	modes := append([]retrieval.Mode(nil), request.Modes...)
	sort.Slice(modes, func(i, j int) bool { return modes[i] < modes[j] })
	writeUint64(digest, uint64(len(modes)))
	for _, mode := range modes {
		writePart(digest, []byte(mode))
	}
	documentIDs := append([]shoal.ID(nil), request.Scope.DocumentIDs...)
	nodeIDs := append([]shoal.ID(nil), request.Scope.NodeIDs...)
	sort.Slice(documentIDs, func(i, j int) bool {
		return shoal.CompareID(documentIDs[i], documentIDs[j]) < 0
	})
	sort.Slice(nodeIDs, func(i, j int) bool {
		return shoal.CompareID(nodeIDs[i], nodeIDs[j]) < 0
	})
	writeIDs(digest, documentIDs)
	writeIDs(digest, nodeIDs)
	writeBool(digest, !request.AsOf.IsZero())
	if !request.AsOf.IsZero() {
		writePart(digest, []byte(canonicalTime(request.AsOf)))
	}
	writeBool(digest, request.Explain)
	writePart(digest, []byte(response.RequestID))
	writePart(digest, []byte(response.EmbeddingSpaceID))
	writeIDs(digest, response.EmbeddingSpaceIDs)
	writeUint64(digest, uint64(len(response.Results)))
	for _, result := range response.Results {
		writePart(digest, []byte(result.ID))
		writeScore(digest, result.Score)
		writeUint64(digest, uint64(len(result.Evidence)))
		for _, evidence := range result.Evidence {
			writeCitation(digest, evidence.Citation)
			writePart(digest, []byte(evidence.Quote))
			writePath(digest, evidence.Path)
			writeScore(digest, evidence.Score)
		}
		if result.Explanation == nil {
			writeBool(digest, false)
			continue
		}
		writeBool(digest, true)
		modes := append([]retrieval.Mode(nil), result.Explanation.Modes...)
		sort.Slice(modes, func(i, j int) bool { return modes[i] < modes[j] })
		writeUint64(digest, uint64(len(modes)))
		for _, mode := range modes {
			writePart(digest, []byte(mode))
		}
		writePart(digest, []byte(result.Explanation.Summary))
		keys := sortedMetadataKeys(result.Explanation.Scores)
		writeUint64(digest, uint64(len(keys)))
		for _, key := range keys {
			writePart(digest, []byte(key))
			writeScore(digest, result.Explanation.Scores[key])
		}
	}
	return "sha256:" + hex.EncodeToString(digest.Sum(nil)), nil
}

func buildPack(
	ctx context.Context,
	query string,
	anchors []inference.EvidenceAnchor,
	pins Pins,
	metadata shoal.Metadata,
	limits Limits,
	estimator TokenEstimator,
) (inference.ContextPack, error) {
	normalizedQuery := strings.Join(strings.Fields(query), " ")
	if err := enforcePackBounds(normalizedQuery, anchors, pins, metadata, limits); err != nil {
		return inference.ContextPack{}, err
	}
	pack, err := inference.NewContextPack(
		normalizedQuery, anchors, pins.Ontology, pins.Snapshot, pins.Authorization, metadata)
	if err != nil {
		return inference.ContextPack{}, err
	}
	if limits.MaxContextTokens > 0 {
		if estimator == nil {
			return inference.ContextPack{}, invalid(
				"a token estimator is required when a context token limit is configured")
		}
		tokens, err := estimator.EstimateTokens(ctx, pack)
		if err != nil {
			return inference.ContextPack{}, fmt.Errorf("estimate context tokens: %w", err)
		}
		if tokens < 0 {
			return inference.ContextPack{}, invalid(
				"token estimator returned a negative count")
		}
		if tokens > limits.MaxContextTokens {
			return inference.ContextPack{}, invalid("context pack exceeds the token bound")
		}
	}
	return pack, nil
}

func rebuildPack(
	ctx context.Context,
	pack inference.ContextPack,
	anchors []inference.EvidenceAnchor,
	limits Limits,
	estimator TokenEstimator,
) (inference.ContextPack, error) {
	var ontology *inference.OntologyIdentity
	if value, ok := pack.Ontology(); ok {
		ontology = &value
	}
	return buildPack(ctx, pack.Query(), anchors, Pins{
		Snapshot: pack.Snapshot(), Authorization: pack.Authorization(), Ontology: ontology,
	}, pack.Metadata(), limits, estimator)
}

func canonicalAnchors(
	input []inference.EvidenceAnchor,
	maximum int,
) ([]inference.EvidenceAnchor, error) {
	byID := make(map[shoal.ID]inference.EvidenceAnchor, len(input))
	for _, anchor := range input {
		if err := anchor.Validate(); err != nil {
			return nil, err
		}
		if existing, duplicate := byID[anchor.ID()]; duplicate {
			if !anchorsEqual(existing, anchor) {
				return nil, invalid("duplicate anchor identity has different content")
			}
			continue
		}
		byID[anchor.ID()] = anchor
	}
	if len(byID) > maximum {
		return nil, invalid("context evidence exceeds the anchor bound")
	}
	result := make([]inference.EvidenceAnchor, 0, len(byID))
	for _, anchor := range byID {
		result = append(result, anchor)
	}
	sort.Slice(result, func(i, j int) bool {
		return shoal.CompareID(result[i].ID(), result[j].ID()) < 0
	})
	return result, nil
}

func anchorMap(anchors []inference.EvidenceAnchor) map[shoal.ID]inference.EvidenceAnchor {
	result := make(map[shoal.ID]inference.EvidenceAnchor, len(anchors))
	for _, anchor := range anchors {
		result[anchor.ID()] = anchor
	}
	return result
}

func appendAnchor(
	anchors *[]inference.EvidenceAnchor,
	seen map[shoal.ID]inference.EvidenceAnchor,
	anchor inference.EvidenceAnchor,
	maximum int,
) error {
	if existing, duplicate := seen[anchor.ID()]; duplicate {
		if !anchorsEqual(existing, anchor) {
			return invalid("duplicate anchor identity has different content")
		}
		return nil
	}
	if len(*anchors) >= maximum {
		return invalid("context evidence exceeds the anchor bound")
	}
	seen[anchor.ID()] = anchor
	*anchors = append(*anchors, anchor)
	return nil
}

func anchorsEqual(left, right inference.EvidenceAnchor) bool {
	if left.ID() != right.ID() || left.Kind() != right.Kind() {
		return false
	}
	if left.Kind() == inference.AnchorDocument {
		leftCitation, leftQuote, leftOK := left.Document()
		rightCitation, rightQuote, rightOK := right.Document()
		return leftOK && rightOK &&
			canonicalEqual(leftCitation, rightCitation) && leftQuote == rightQuote
	}
	leftPath, leftOK := left.Path()
	rightPath, rightOK := right.Path()
	return leftOK && rightOK && canonicalEqual(leftPath, rightPath)
}

func enforcePackBounds(
	query string,
	anchors []inference.EvidenceAnchor,
	pins Pins,
	metadata shoal.Metadata,
	limits Limits,
) error {
	if len(anchors) > limits.MaxAnchors {
		return invalid("context evidence exceeds the anchor bound")
	}
	if metadataBytes(metadata) > limits.MaxProvenanceBytes {
		return invalid("context metadata and provenance exceed the byte bound")
	}
	contextBytes, quoteBytes, graphNodes, graphEdges, err :=
		contextPackByteSize(query, anchors, pins, metadata, limits.MaxPathNodes)
	if err != nil {
		return err
	}
	if quoteBytes > limits.MaxQuoteBytes {
		return invalid("context evidence exceeds the total quote byte bound")
	}
	if graphNodes > limits.MaxGraphNodes {
		return invalid("context evidence exceeds the graph node bound")
	}
	if graphEdges > limits.MaxGraphEdges {
		return invalid("context evidence exceeds the graph edge bound")
	}
	if contextBytes > limits.MaxContextBytes {
		return invalid("context pack exceeds the builder byte bound")
	}
	return nil
}

func contextPackByteSize(
	query string,
	anchors []inference.EvidenceAnchor,
	pins Pins,
	metadata shoal.Metadata,
	maxPathNodes int,
) (int, int, int, int, error) {
	quoteBytes := 0
	graphNodes := 0
	graphEdges := 0
	payloadBytes := len(query) + metadataBytes(metadata)
	anchorIDs := make([]string, 0, len(anchors))
	for _, anchor := range anchors {
		anchorIDs = append(anchorIDs, string(anchor.ID()))
		if citation, quote, ok := anchor.Document(); ok {
			quoteBytes += len(quote)
			payloadBytes += len(anchor.ID()) + len(quote) +
				len(citation.DocumentID) + len(citation.RevisionID) +
				len(citation.SectionID) + len(citation.SpanID) + 24
		} else if path, ok := anchor.Path(); ok {
			if len(path.Nodes) > maxPathNodes {
				return 0, 0, 0, 0, invalid("context graph path exceeds the path bound")
			}
			graphNodes += len(path.Nodes)
			graphEdges += len(path.Edges)
			payloadBytes += len(anchor.ID()) + pathPayloadBytes(path)
		}
	}
	ontologyLength := 0
	if pins.Ontology != nil {
		ontologyLength = canonicalPartsLength(
			len(pins.Ontology.SchemaID()), len(pins.Ontology.VersionID()))
	}
	anchorIDLengths := make([]int, len(anchorIDs))
	for index, id := range anchorIDs {
		anchorIDLengths[index] = len(id)
	}
	canonicalLength := canonicalPartsLength(
		len(query),
		canonicalPartsLength(anchorIDLengths...),
		ontologyLength,
		len(pins.Snapshot.ID()),
		len(canonicalTime(pins.Snapshot.AsOf())),
		len(pins.Authorization.Fingerprint()),
		len(canonicalTime(pins.Authorization.ExpiresAt())),
		canonicalMetadataLength(metadata),
	)
	return payloadBytes + canonicalLength, quoteBytes, graphNodes, graphEdges, nil
}

func normalizeLimits(input Limits) (Limits, error) {
	limits := input
	defaultInt(&limits.MaxResults, DefaultMaxResults)
	defaultInt(&limits.MaxAnchors, DefaultMaxAnchors)
	defaultInt(&limits.MaxDocuments, DefaultMaxDocuments)
	defaultInt(&limits.MaxSections, DefaultMaxSections)
	defaultInt(&limits.MaxSpans, DefaultMaxSpans)
	defaultInt(&limits.MaxGraphNodes, DefaultMaxGraphNodes)
	defaultInt(&limits.MaxGraphEdges, DefaultMaxGraphEdges)
	defaultInt(&limits.MaxPathNodes, DefaultMaxPathNodes)
	defaultInt(&limits.MaxContextBytes, DefaultMaxContextBytes)
	defaultInt(&limits.MaxHydrationBytes, DefaultMaxHydrationBytes)
	defaultInt(&limits.MaxQuoteBytes, DefaultMaxQuoteBytes)
	defaultInt(&limits.MaxProvenanceBytes, DefaultMaxProvenanceBytes)
	if limits.MaxHierarchyDepth == 0 {
		limits.MaxHierarchyDepth = DefaultMaxHierarchyDepth
	}
	for name, value := range map[string]int{
		"result": limits.MaxResults, "anchor": limits.MaxAnchors,
		"document": limits.MaxDocuments, "section": limits.MaxSections,
		"span":       limits.MaxSpans,
		"graph node": limits.MaxGraphNodes, "graph edge": limits.MaxGraphEdges,
		"path node": limits.MaxPathNodes, "context byte": limits.MaxContextBytes,
		"hydration byte": limits.MaxHydrationBytes,
		"quote byte":     limits.MaxQuoteBytes, "provenance byte": limits.MaxProvenanceBytes,
	} {
		if value <= 0 {
			return Limits{}, invalid(name + " limit must be positive")
		}
		if limits.MaxContextTokens < 0 {
			return Limits{}, invalid("context token limit cannot be negative")
		}
	}
	if limits.MaxResults > int(retrieval.MaxTopK) {
		return Limits{}, invalid("result limit exceeds the retrieval contract")
	}
	if limits.MaxAnchors > inference.MaxEvidenceAnchors {
		return Limits{}, invalid("anchor limit exceeds the inference contract")
	}
	if limits.MaxPathNodes > inference.MaxPathNodes {
		return Limits{}, invalid("path limit exceeds the inference contract")
	}
	if limits.MaxContextBytes > inference.MaxContextPackBytes {
		return Limits{}, invalid("context byte limit exceeds the inference contract")
	}
	if limits.MaxHierarchyDepth > 16 {
		return Limits{}, invalid("hierarchy depth limit exceeds the Explorer contract")
	}
	if limits.MaxSections > document.MaxSectionsPerRevision {
		return Limits{}, invalid("section limit exceeds the document contract")
	}
	if limits.MaxSpans > document.MaxSpansPerRevision {
		return Limits{}, invalid("span limit exceeds the document contract")
	}
	return limits, nil
}

func defaultInt(value *int, fallback int) {
	if *value == 0 {
		*value = fallback
	}
}

func validateUniqueIDs(name string, ids []shoal.ID) error {
	seen := make(map[shoal.ID]struct{}, len(ids))
	for _, id := range ids {
		if err := shoal.ValidateRequiredID(name, id); err != nil {
			return err
		}
		if _, duplicate := seen[id]; duplicate {
			return invalid("duplicate " + name)
		}
		seen[id] = struct{}{}
	}
	return nil
}

func cloneResponse(response retrieval.Response) retrieval.Response {
	cloned := retrieval.Response{
		RequestID:        response.RequestID,
		EmbeddingSpaceID: response.EmbeddingSpaceID,
		EmbeddingSpaceIDs: append(
			[]shoal.ID(nil), response.EmbeddingSpaceIDs...),
	}
	cloned.Results = make([]retrieval.Result, len(response.Results))
	for i, result := range response.Results {
		cloned.Results[i] = retrieval.Result{
			ID: result.ID, Score: result.Score, Explanation: cloneExplanation(result.Explanation),
		}
		cloned.Results[i].Evidence = make([]retrieval.Evidence, len(result.Evidence))
		for j, evidence := range result.Evidence {
			cloned.Results[i].Evidence[j] = retrieval.Evidence{
				Citation: evidence.Citation, Quote: evidence.Quote,
				Path: clonePath(evidence.Path), Score: evidence.Score,
			}
		}
	}
	return cloned
}

func cloneExplanation(explanation *retrieval.Explanation) *retrieval.Explanation {
	if explanation == nil {
		return nil
	}
	cloned := &retrieval.Explanation{
		Modes:   append([]retrieval.Mode(nil), explanation.Modes...),
		Summary: explanation.Summary,
		Scores:  make(map[string]shoal.Score, len(explanation.Scores)),
	}
	for key, value := range explanation.Scores {
		cloned.Scores[key] = value
	}
	return cloned
}

func cloneView(view explorer.DocumentView) explorer.DocumentView {
	view.Document.Metadata = cloneMetadata(view.Document.Metadata)
	view.Revision.Metadata = cloneMetadata(view.Revision.Metadata)
	if !view.Revision.CreatedAt.IsZero() {
		view.Revision.CreatedAt = view.Revision.CreatedAt.UTC()
	}
	view.Root = cloneSectionView(view.Root)
	return view
}

func cloneSectionView(view explorer.SectionView) explorer.SectionView {
	view.Section.Metadata = cloneMetadata(view.Section.Metadata)
	view.Spans = append([]document.Span(nil), view.Spans...)
	for index := range view.Spans {
		view.Spans[index].Metadata = cloneMetadata(view.Spans[index].Metadata)
	}
	view.Children = append([]explorer.SectionView(nil), view.Children...)
	for index := range view.Children {
		view.Children[index] = cloneSectionView(view.Children[index])
	}
	return view
}

func cloneNode(node graph.Node) graph.Node {
	node.Labels = append([]string(nil), node.Labels...)
	node.Properties = cloneMetadata(node.Properties)
	return node
}

func canonicalNode(node graph.Node) graph.Node {
	node = cloneNode(node)
	sort.Strings(node.Labels)
	return node
}

func cloneEdge(edge graph.Edge) graph.Edge {
	edge.Properties = cloneMetadata(edge.Properties)
	return edge
}

func clonePath(path graph.Path) graph.Path {
	cloned := graph.Path{
		Nodes: make([]graph.Node, len(path.Nodes)),
		Edges: make([]graph.Edge, len(path.Edges)),
	}
	for i, node := range path.Nodes {
		cloned.Nodes[i] = cloneNode(node)
	}
	for i, edge := range path.Edges {
		cloned.Edges[i] = cloneEdge(edge)
	}
	return cloned
}

func cloneMetadata(metadata shoal.Metadata) shoal.Metadata {
	if len(metadata) == 0 {
		return nil
	}
	cloned := make(shoal.Metadata, len(metadata))
	for key, value := range metadata {
		cloned[key] = value
	}
	return cloned
}

func sortedEdges(edges []graph.Edge) []graph.Edge {
	result := make([]graph.Edge, len(edges))
	for i, edge := range edges {
		result[i] = cloneEdge(edge)
	}
	sort.Slice(result, func(i, j int) bool {
		return shoal.CompareID(result[i].ID, result[j].ID) < 0
	})
	return result
}

func canonicalEqual(left, right any) bool {
	switch typedLeft := left.(type) {
	case graph.Edge:
		typedRight, ok := right.(graph.Edge)
		if !ok ||
			math.Float64bits(float64(typedLeft.Weight)) !=
				math.Float64bits(float64(typedRight.Weight)) {
			return false
		}
		typedLeft.Weight = 0
		typedRight.Weight = 0
		return reflect.DeepEqual(typedLeft, typedRight)
	case graph.Path:
		typedRight, ok := right.(graph.Path)
		if !ok ||
			len(typedLeft.Nodes) != len(typedRight.Nodes) ||
			len(typedLeft.Edges) != len(typedRight.Edges) ||
			!reflect.DeepEqual(typedLeft.Nodes, typedRight.Nodes) {
			return false
		}
		for index := range typedLeft.Edges {
			if !canonicalEqual(typedLeft.Edges[index], typedRight.Edges[index]) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(left, right)
}

func metadataBytes(metadata shoal.Metadata) int {
	total := 0
	for key, value := range metadata {
		total += len(key) + len(value)
	}
	return total
}

func pathPayloadBytes(path graph.Path) int {
	total := 0
	for _, node := range path.Nodes {
		total += nodePayloadBytes(node)
	}
	for _, edge := range path.Edges {
		total += edgePayloadBytes(edge)
	}
	return total
}

func neighborhoodPayloadBytes(neighborhood explorer.Neighborhood) (int, error) {
	total := 0
	for _, node := range neighborhood.Nodes {
		if err := node.Validate(); err != nil {
			return 0, err
		}
		var ok bool
		total, ok = addBounded(total, nodePayloadBytes(node), int(^uint(0)>>1))
		if !ok {
			return 0, invalid("hydrated graph byte size overflows")
		}
	}
	for _, edge := range neighborhood.Edges {
		if err := edge.Validate(); err != nil {
			return 0, err
		}
		var ok bool
		total, ok = addBounded(total, edgePayloadBytes(edge), int(^uint(0)>>1))
		if !ok {
			return 0, invalid("hydrated graph byte size overflows")
		}
	}
	for _, assertion := range neighborhood.Assertions {
		if err := assertion.Validate(); err != nil {
			return 0, err
		}
		var ok bool
		total, ok = addBounded(
			total, assertionPayloadBytes(assertion), int(^uint(0)>>1))
		if !ok {
			return 0, invalid("hydrated graph byte size overflows")
		}
	}
	return total, nil
}

func nodePayloadBytes(node graph.Node) int {
	total := len(node.ID) + len(node.Kind) + metadataBytes(node.Properties)
	for _, label := range node.Labels {
		total += len(label)
	}
	return total
}

func edgePayloadBytes(edge graph.Edge) int {
	return len(edge.ID) + len(edge.From) + len(edge.To) + len(edge.Type) +
		float64PayloadBytes + metadataBytes(edge.Properties)
}

const float64PayloadBytes = 8

func assertionPayloadBytes(assertion ontology.Assertion) int {
	object := assertion.Object()
	total := len(assertion.ID()) + len(assertion.Subject()) +
		len(assertion.Predicate()) + len(assertion.Origin()) +
		len(object.Type()) + ontologyValuePayloadBytes(object) +
		float64PayloadBytes +
		metadataBytes(assertion.Metadata())
	if subjectType, ok := assertion.SubjectType(); ok {
		total += len(subjectType)
	}
	if objectType, ok := assertion.ObjectType(); ok {
		total += len(objectType)
	}
	if identity, ok := assertion.Ontology(); ok {
		total += len(identity.SchemaID()) + len(identity.VersionID())
	}
	provenance := assertion.Provenance()
	total += len(provenance.Provider()) + len(provenance.Model()) +
		len(provenance.ModelVersion()) + len(provenance.Prompt()) +
		len(provenance.PromptVersion()) + len(provenance.Extractor()) +
		len(provenance.ExtractorVersion()) + metadataBytes(provenance.Metadata())
	for _, evidence := range assertion.Evidence() {
		total += evidenceRefPayloadBytes(evidence)
	}
	return total
}

func ontologyValuePayloadBytes(value ontology.Value) int {
	switch value.Type() {
	case ontology.ValueString:
		text, _ := value.StringValue()
		return len(text)
	case ontology.ValueReference:
		reference, _ := value.ReferenceValue()
		return len(reference)
	case ontology.ValueInteger, ontology.ValueNumber, ontology.ValueTimestamp:
		return 8
	case ontology.ValueBoolean:
		return 1
	default:
		return 0
	}
}

func evidenceRefPayloadBytes(evidence ontology.EvidenceRef) int {
	total := len(evidence.ID()) + len(evidence.Quote()) +
		metadataBytes(evidence.Metadata())
	citation := evidence.Citation()
	total += len(citation.DocumentID) + len(citation.RevisionID) +
		len(citation.SectionID) + len(citation.SpanID) + 24
	if path, ok := evidence.Path(); ok {
		total += pathPayloadBytes(path)
	}
	if derivation, ok := evidence.Derivation(); ok {
		total += len(derivation.ID()) + len(derivation.EmbeddingModel()) +
			len(derivation.EmbeddingModelVersion()) +
			len(derivation.SimilarityMetric()) +
			len(derivation.TessellationCell()) +
			len(derivation.SourceEndpoint()) +
			len(derivation.TargetEndpoint()) +
			len(derivation.IteratorName()) + 2*float64PayloadBytes +
			metadataBytes(derivation.IteratorOptions())
	}
	return total
}

func assertionHydrationPayloadBytes(state assertionHydration) int {
	total := assertionPayloadBytes(state.assertion)
	for _, edgeID := range state.edgeIDs {
		total += assertionReferencePayloadBytes(
			state.assertion.ID(), edgeID, state.assertion.Origin())
	}
	return total
}

func assertionReferencePayloadBytes(
	assertionID, edgeID shoal.ID,
	origin ontology.AssertionOrigin,
) int {
	return len(assertionID) + 2*len(edgeID) + len(origin)
}

func addBounded(total, addition, limit int) (int, bool) {
	if addition < 0 || total < 0 || total > limit || addition > limit-total {
		return total, false
	}
	return total + addition, true
}

func canonicalPartsLength(partLengths ...int) int {
	total := 0
	for _, length := range partLengths {
		total += len(strconv.Itoa(length)) + 1 + length
	}
	return total
}

func canonicalMetadataLength(metadata shoal.Metadata) int {
	lengths := make([]int, 0, len(metadata)*2)
	for _, key := range sortedMetadataKeys(metadata) {
		lengths = append(lengths, len(key), len(metadata[key]))
	}
	return canonicalPartsLength(lengths...)
}

func canonicalTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func sortedMetadataKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func writePart(writer hash.Hash, value []byte) {
	writeUint64(writer, uint64(len(value)))
	_, _ = writer.Write(value)
}

func writeUint64(writer hash.Hash, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = writer.Write(encoded[:])
}

func writeInt64(writer hash.Hash, value int64) {
	writeUint64(writer, uint64(value))
}

func writeBool(writer hash.Hash, value bool) {
	if value {
		_, _ = writer.Write([]byte{1})
		return
	}
	_, _ = writer.Write([]byte{0})
}

func writeScore(writer hash.Hash, value shoal.Score) {
	writeUint64(writer, math.Float64bits(float64(value)))
}

func writeIDs(writer hash.Hash, ids []shoal.ID) {
	writeUint64(writer, uint64(len(ids)))
	for _, id := range ids {
		writePart(writer, []byte(id))
	}
}

func writeCitation(writer hash.Hash, citation document.Citation) {
	writePart(writer, []byte(citation.DocumentID))
	writePart(writer, []byte(citation.RevisionID))
	writePart(writer, []byte(citation.SectionID))
	writePart(writer, []byte(citation.SpanID))
	writeInt64(writer, citation.Range.Start.Offset)
	writeInt64(writer, int64(citation.Range.Start.Page))
	writeInt64(writer, citation.Range.End.Offset)
	writeInt64(writer, int64(citation.Range.End.Page))
}

func writePath(writer hash.Hash, path graph.Path) {
	writeUint64(writer, uint64(len(path.Nodes)))
	for _, node := range path.Nodes {
		writePart(writer, []byte(node.ID))
		writePart(writer, []byte(node.Kind))
		labels := append([]string(nil), node.Labels...)
		sort.Strings(labels)
		writeUint64(writer, uint64(len(labels)))
		for _, label := range labels {
			writePart(writer, []byte(label))
		}
		writeMetadata(writer, node.Properties)
	}
	writeUint64(writer, uint64(len(path.Edges)))
	for _, edge := range path.Edges {
		writePart(writer, []byte(edge.ID))
		writePart(writer, []byte(edge.From))
		writePart(writer, []byte(edge.To))
		writePart(writer, []byte(edge.Type))
		writeScore(writer, edge.Weight)
		writeMetadata(writer, edge.Properties)
	}
}

func writeMetadata(writer hash.Hash, metadata shoal.Metadata) {
	keys := sortedMetadataKeys(metadata)
	writeUint64(writer, uint64(len(keys)))
	for _, key := range keys {
		writePart(writer, []byte(key))
		writePart(writer, []byte(metadata[key]))
	}
}

func encodeID(id shoal.ID) string {
	return "hex:" + hex.EncodeToString([]byte(id))
}

func encodeIDs(ids []shoal.ID) string {
	encoded := make([]string, len(ids))
	for index, id := range ids {
		encoded[index] = encodeID(id)
	}
	return strings.Join(encoded, ",")
}

func rangeContains(outer, inner document.SourceRange) bool {
	return outer.Start.Offset <= inner.Start.Offset && inner.End.Offset <= outer.End.Offset
}

func sectionViewStats(view explorer.DocumentView) (int, int, int) {
	sections := 0
	spans := 0
	totalBytes := len(view.Document.ID) + len(view.Document.RevisionID) +
		len(view.Document.Title) + len(view.Document.RootSectionID) +
		metadataBytes(view.Document.Metadata) +
		len(view.Revision.ID) + len(view.Revision.DocumentID) +
		len(view.Revision.SourceVersion) + metadataBytes(view.Revision.Metadata) +
		len(view.SourceURI)
	stack := []*explorer.SectionView{&view.Root}
	for len(stack) > 0 {
		last := len(stack) - 1
		current := stack[last]
		stack = stack[:last]
		sections++
		totalBytes += len(current.Section.ID) + len(current.Section.DocumentID) +
			len(current.Section.RevisionID) + len(current.Section.ParentID) +
			len(current.Section.Heading) + metadataBytes(current.Section.Metadata) + 28
		spans += len(current.Spans)
		for _, span := range current.Spans {
			totalBytes += len(span.ID) + len(span.DocumentID) + len(span.RevisionID) +
				len(span.SectionID) + len(span.Text) + metadataBytes(span.Metadata) + 28
		}
		for index := range current.Children {
			stack = append(stack, &current.Children[index])
		}
	}
	return sections, spans, totalBytes
}

func validateNeighborhoodResponse(
	request explorer.NeighborhoodRequest,
	neighborhood explorer.Neighborhood,
	limits Limits,
) error {
	if len(neighborhood.Nodes) > limits.MaxGraphNodes {
		return invalid("hydrated graph exceeds the node bound")
	}
	if len(neighborhood.Edges) > limits.MaxGraphEdges {
		return invalid("hydrated graph exceeds the edge bound")
	}
	nodes := make(map[shoal.ID]struct{}, len(neighborhood.Nodes))
	for _, node := range neighborhood.Nodes {
		nodes[node.ID] = struct{}{}
	}
	seen := make(map[shoal.ID]struct{}, len(request.NodeIDs))
	frontier := make(map[shoal.ID]struct{}, len(request.NodeIDs))
	for _, id := range request.NodeIDs {
		if _, ok := nodes[id]; !ok {
			return invalid("hydrated neighborhood omitted a requested node")
		}
		seen[id] = struct{}{}
		frontier[id] = struct{}{}
	}
	typeFilter := make(map[string]struct{}, len(request.EdgeTypes))
	for _, edgeType := range request.EdgeTypes {
		typeFilter[edgeType] = struct{}{}
	}
	selectedEdges := make(map[shoal.ID]struct{}, len(neighborhood.Edges))
	for depth := uint32(0); depth < request.Depth && len(frontier) > 0; depth++ {
		next := make(map[shoal.ID]struct{})
		for _, edge := range neighborhood.Edges {
			if len(typeFilter) > 0 {
				if _, ok := typeFilter[edge.Type]; !ok {
					continue
				}
			}
			_, from := frontier[edge.From]
			_, to := frontier[edge.To]
			if !from && !to {
				continue
			}
			selectedEdges[edge.ID] = struct{}{}
			for _, id := range []shoal.ID{edge.From, edge.To} {
				if _, exists := seen[id]; !exists {
					seen[id] = struct{}{}
					next[id] = struct{}{}
				}
			}
		}
		frontier = next
	}
	if len(seen) != len(neighborhood.Nodes) ||
		len(selectedEdges) != len(neighborhood.Edges) {
		return invalid("hydrated neighborhood exceeds the requested scope")
	}
	return nil
}

func pathPresent(path graph.Path) bool {
	return len(path.Nodes) > 0 || len(path.Edges) > 0
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return invalid("context is required")
	}
	if err := ctx.Err(); err != nil {
		return shoal.WrapError(shoal.ErrorUnavailable, "context construction canceled", err)
	}
	return nil
}

func invalid(message string) error {
	return shoal.NewError(shoal.ErrorInvalidArgument, strings.TrimSpace(message))
}
