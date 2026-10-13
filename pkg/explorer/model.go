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

// Package explorer provides an opinionated document and graph exploration
// experience over Shoal's transport-neutral knowledge contracts.
package explorer

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	MediaTypeMarkdown = "text/markdown"
	MediaTypeText     = "text/plain"
	MediaTypeSource   = "text/x-source-code"
)

// Source is one textual source revision to ingest.
type Source struct {
	URI       string
	Title     string
	MediaType string
	Content   string
	Metadata  shoal.Metadata
}

// IngestOptions controls descriptive source-revision values without changing
// publication order. A zero CreatedAt uses the current time; a nonzero value
// is retained exactly after UTC normalization.
type IngestOptions struct {
	CreatedAt time.Time
}

// IngestDisposition reports whether ingestion created a revision or found the
// same immutable revision already present, and if so whether that revision is
// the document's current one. The string values are stable wire encodings.
type IngestDisposition string

const (
	// IngestApplied means the request published a new revision, which is now
	// the document's current revision.
	IngestApplied IngestDisposition = "applied"
	// IngestUnchanged means the content matches the document's current
	// revision. Nothing was written.
	IngestUnchanged IngestDisposition = "unchanged"
	// IngestHistorical means the content matches an earlier, non-current
	// revision of the document. Nothing was written, and that revision was
	// NOT made current again: the document still serves its current revision
	// (#674). It is a successful result, so a delayed retry of an ingest that
	// has since been superseded does not fail. A caller that needs the
	// content to be current must treat it as "not done". There is no restore
	// operation; to make old content current again, change it by any byte.
	//
	// Graph materialization never reports it.
	IngestHistorical IngestDisposition = "historical"
)

// IngestResult identifies the durable artifacts created for one source.
type IngestResult struct {
	Disposition  IngestDisposition
	Document     document.Document
	Revision     document.Revision
	SectionCount int
	SpanCount    int
}

// DocumentSummary is the current revision shown in corpus listings.
type DocumentSummary struct {
	Document        document.Document
	Revision        document.Revision
	SourceURI       string
	SourceMediaType string
}

// SectionView combines one section with its directly attributable spans and
// nested sections.
type SectionView struct {
	Section  document.Section
	Spans    []document.Span
	Children []SectionView
}

// DocumentView is a revision-specific, navigable document.
type DocumentView struct {
	Document        document.Document
	Revision        document.Revision
	SourceURI       string
	SourceMediaType string
	Root            SectionView
}

// NeighborhoodRequest bounds a graph expansion around one or more nodes.
type NeighborhoodRequest struct {
	NodeIDs   []shoal.ID
	Depth     uint32
	EdgeTypes []string
}

// Normalize clones and validates a bounded neighborhood request. Depth zero
// means one. Duplicate seeds and edge types collapse by first occurrence.
func (r NeighborhoodRequest) Normalize() (NeighborhoodRequest, error) {
	normalized := NeighborhoodRequest{
		Depth:     r.Depth,
		NodeIDs:   make([]shoal.ID, 0, len(r.NodeIDs)),
		EdgeTypes: make([]string, 0, len(r.EdgeTypes)),
	}
	if normalized.Depth == 0 {
		normalized.Depth = 1
	}
	if normalized.Depth > 16 {
		return NeighborhoodRequest{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "graph depth cannot exceed 16")
	}
	seenNodes := make(map[shoal.ID]struct{}, len(r.NodeIDs))
	for _, id := range r.NodeIDs {
		if err := shoal.ValidateRequiredID("graph node ID", id); err != nil {
			return NeighborhoodRequest{}, err
		}
		if _, duplicate := seenNodes[id]; duplicate {
			continue
		}
		seenNodes[id] = struct{}{}
		normalized.NodeIDs = append(normalized.NodeIDs, id)
	}
	if len(normalized.NodeIDs) == 0 {
		return NeighborhoodRequest{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "at least one graph node ID is required")
	}
	seenTypes := make(map[string]struct{}, len(r.EdgeTypes))
	for _, edgeType := range r.EdgeTypes {
		if !utf8.ValidString(edgeType) || strings.TrimSpace(edgeType) == "" {
			return NeighborhoodRequest{}, shoal.NewError(
				shoal.ErrorInvalidArgument, "edge types must be valid nonblank UTF-8")
		}
		if err := shoal.ValidateSemanticString("edge type", edgeType); err != nil {
			return NeighborhoodRequest{}, err
		}
		if _, duplicate := seenTypes[edgeType]; duplicate {
			continue
		}
		seenTypes[edgeType] = struct{}{}
		normalized.EdgeTypes = append(normalized.EdgeTypes, edgeType)
	}
	return normalized, nil
}

// Neighborhood is a deterministic graph subgraph.
type Neighborhood struct {
	Nodes           []graph.Node
	Edges           []graph.Edge
	Assertions      []ontology.Assertion
	Interpretations []ontology.AssertionInterpretation
}

// Snapshot identifies one immutable logical corpus frontier.
type Snapshot struct {
	ID       string
	AsOf     time.Time
	Frontier uint64
}

// BoundedNeighborhoodRequest limits graph work as well as returned data.
// Fanout caps adjacency entries examined per expanded node.
type BoundedNeighborhoodRequest struct {
	NodeIDs  []shoal.ID
	Depth    uint32
	Fanout   uint32
	MaxNodes uint32
	// MaxScannedEdges optionally caps total adjacency entries an
	// authorization-filtering wrapper may scan to fill one authorized page.
	// A zero value defaults to Fanout.
	MaxScannedEdges uint32
	EdgeTypes       []string
	Direction       GraphDirection
	AfterEdgeID     shoal.ID
}

// GraphDirection selects which adjacency entries consume bounded fanout.
type GraphDirection string

const (
	GraphDirectionBoth     GraphDirection = "both"
	GraphDirectionOutgoing GraphDirection = "outgoing"
	GraphDirectionIncoming GraphDirection = "incoming"
)

// BoundedNeighborhood reports whether a server bound stopped expansion.
type BoundedNeighborhood struct {
	Neighborhood    Neighborhood
	Truncated       bool
	NextAfterEdgeID shoal.ID
	Continuation    bool
	// ScannedEdges is the number of adjacency entries consumed to produce
	// this page, including entries suppressed before materialization.
	ScannedEdges      uint32
	ScannedEdgesKnown bool
}

// BoundedClient is the backend boundary required by scalable Explorer
// transports. Bounds are enforced before graph materialization.
type BoundedClient interface {
	Client
	Snapshot(context.Context) (Snapshot, error)
	BoundedNeighborhood(
		context.Context, BoundedNeighborhoodRequest,
	) (BoundedNeighborhood, error)
}

// Client is the product-facing Explorer contract. Implementations can be
// embedded or remote without changing document, graph, or retrieval values.
type Client interface {
	retrieval.Retriever
	Ingest(context.Context, Source) (IngestResult, error)
	Documents(context.Context) ([]DocumentSummary, error)
	Document(context.Context, shoal.ID, shoal.ID) (DocumentView, error)
	Connect(context.Context, graph.Edge) error
	Neighborhood(context.Context, NeighborhoodRequest) (Neighborhood, error)
}
