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

package harness

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/phrocker/shoal-oss/pkg/contextpack"
	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type ExplorerToolHost struct {
	Client           explorer.Client
	BoundedClient    explorer.BoundedClient
	Builder          contextpack.Builder
	PolicyID         shoal.ID
	Metadata         shoal.Metadata
	RetrievalModes   []retrieval.Mode
	RetrievalExplain bool

	ClientIdentity         string
	BoundedClientIdentity  string
	BuilderReaderIdentity  string
	TokenEstimatorIdentity string
}

type embeddingParticipationCollector struct {
	mu            sync.Mutex
	observed      bool
	participating []string
	err           error
}

func (c *embeddingParticipationCollector) observe(
	event explorer.EmbeddingQueryEvent,
) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.observed = true
	if c.err != nil {
		return
	}
	identities := append(
		append([]string(nil), c.participating...),
		event.Participating...,
	)
	set, err := interaction.NewEmbeddingSpaceSet(identities)
	if err != nil {
		c.err = err
		return
	}
	c.participating = set.Identities
}

func (c *embeddingParticipationCollector) result(
	requireParticipation bool,
) (interaction.EmbeddingSpaceSet, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return interaction.EmbeddingSpaceSet{}, c.err
	}
	if requireParticipation && (!c.observed || len(c.participating) == 0) {
		return interaction.EmbeddingSpaceSet{}, invalid(
			"vector retrieval result lacks embedding participation provenance")
	}
	return interaction.NewEmbeddingSpaceSet(c.participating)
}

func NewExplorerToolHost(client explorer.Client, builder contextpack.Builder) (*ExplorerToolHost, error) {
	if client == nil {
		return nil, invalid("explorer client is required")
	}
	if builder.Reader == nil {
		builder.Reader = client
	}
	host := &ExplorerToolHost{Client: client, Builder: builder}
	if bounded, ok := client.(explorer.BoundedClient); ok {
		if available, ok := client.(interface{ BoundedAvailable() bool }); !ok || available.BoundedAvailable() {
			host.BoundedClient = bounded
		}
	}
	return host, nil
}

func (h *ExplorerToolHost) CacheIdentity() (string, error) {
	if err := h.validate(); err != nil {
		return "", err
	}
	clientIdentity, err := configuredHarnessIdentity(h.ClientIdentity, h.Client, "explorer client")
	if err != nil {
		return "", err
	}
	boundedIdentity := "no-bounded-client"
	if h.BoundedClient != nil {
		boundedIdentity, err = configuredHarnessIdentity(h.BoundedClientIdentity, h.BoundedClient, "bounded explorer client")
		if err != nil {
			return "", err
		}
	}
	readerIdentity := "no-builder-reader"
	if h.Builder.Reader != nil {
		readerIdentity, err = configuredHarnessIdentity(h.BuilderReaderIdentity, h.Builder.Reader, "context builder reader")
		if err != nil {
			return "", err
		}
	}
	estimatorIdentity := "no-token-estimator"
	if h.Builder.TokenEstimator != nil {
		estimatorIdentity, err = configuredHarnessIdentity(h.TokenEstimatorIdentity, h.Builder.TokenEstimator, "context token estimator")
		if err != nil {
			return "", err
		}
	}
	metadataKeys := make([]string, 0, len(h.Metadata))
	for key := range h.Metadata {
		metadataKeys = append(metadataKeys, key)
	}
	sort.Strings(metadataKeys)
	parts := []string{
		"explorer-tool-host-v2",
		clientIdentity,
		boundedIdentity,
		readerIdentity,
		estimatorIdentity,
		string(h.PolicyID),
		strconv.FormatBool(h.RetrievalExplain),
		"retrieval-modes",
		strconv.Itoa(len(h.RetrievalModes)),
	}
	for _, mode := range h.RetrievalModes {
		parts = append(parts, string(mode))
	}
	parts = append(parts, "metadata", strconv.Itoa(len(metadataKeys)))
	for _, key := range metadataKeys {
		parts = append(parts, key, h.Metadata[key])
	}
	parts = append(parts, "builder-limits")
	limits := h.Builder.Limits
	parts = append(parts,
		strconv.Itoa(limits.MaxResults),
		strconv.Itoa(limits.MaxAnchors),
		strconv.Itoa(limits.MaxDocuments),
		strconv.Itoa(limits.MaxSections),
		strconv.Itoa(limits.MaxSpans),
		strconv.Itoa(limits.MaxGraphNodes),
		strconv.Itoa(limits.MaxGraphEdges),
		strconv.Itoa(limits.MaxPathNodes),
		strconv.Itoa(limits.MaxContextBytes),
		strconv.Itoa(limits.MaxHydrationBytes),
		strconv.Itoa(limits.MaxContextTokens),
		strconv.Itoa(limits.MaxQuoteBytes),
		strconv.Itoa(limits.MaxProvenanceBytes),
		strconv.FormatUint(uint64(limits.MaxHierarchyDepth), 10),
	)
	if unsafeIdentityParts(parts) {
		return "", ErrCacheIdentityUnsafe
	}
	return framed(parts...), nil
}

func (h *ExplorerToolHost) Retrieve(
	ctx context.Context,
	call ToolContext,
	request RetrieveRequest,
) (ToolResult, error) {
	if err := h.validate(); err != nil {
		return ToolResult{}, err
	}
	if err := h.validateSnapshot(ctx, call); err != nil {
		return ToolResult{}, err
	}
	if err := h.validateAuthorization(ctx, call); err != nil {
		return ToolResult{}, err
	}
	retrievalRequest := retrieval.Request{
		Text:    request.Query(),
		TopK:    uint32(request.Limit()),
		Modes:   h.RetrievalModes,
		AsOf:    call.Snapshot().AsOf(),
		Explain: h.RetrievalExplain,
	}
	clientRequest := retrievalRequest
	if h.BoundedClient != nil {
		clientRequest.AsOf = time.Time{}
	}
	vectorRequest := clientRequest.HasMode(retrieval.ModeVector)
	participation := &embeddingParticipationCollector{}
	retrieveCtx := ctx
	if vectorRequest {
		retrieveCtx = explorer.WithEmbeddingQueryObserver(
			ctx, participation.observe)
	}
	response, err := h.Client.Retrieve(retrieveCtx, clientRequest)
	if err != nil {
		return ToolResult{}, err
	}
	if err := validateRetrievalResponseShape(response, request.Limit(), call.Budgets()); err != nil {
		return ToolResult{}, err
	}
	if err := response.ValidateFor(clientRequest); err != nil {
		return ToolResult{}, err
	}
	if err := h.validateSnapshot(ctx, call); err != nil {
		return ToolResult{}, err
	}
	if err := h.validateAuthorization(ctx, call); err != nil {
		return ToolResult{}, err
	}
	if err := h.verifyRetrievalPaths(ctx, response, call.Budgets()); err != nil {
		return ToolResult{}, err
	}
	embeddingSpaces, err := participation.result(
		vectorRequest && len(response.Results) > 0)
	if err != nil {
		return ToolResult{}, err
	}
	if len(response.Results) == 0 &&
		len(embeddingSpaces.Identities) > 0 {
		return ToolResult{}, invalid(
			"empty vector retrieval reported participating embedding spaces")
	}
	if len(response.Results) == 0 {
		return NewToolResultWithEmbeddingSpaces(
			call.CorrelationID(), ActionRetrieve, nil,
			call.Snapshot(), call.Authorization(), embeddingSpaces,
		)
	}
	neighborhoods, err := neighborhoodsFromRetrievalResponse(response)
	if err != nil {
		return ToolResult{}, err
	}
	pack, err := h.builderFor(call.Budgets().MaxFanout, call.Budgets().MaxGraphNodes).Build(ctx, contextpack.InitialRequest{
		Request:       retrievalRequest,
		Response:      response,
		Neighborhoods: neighborhoods,
		Pins: contextpack.Pins{
			Snapshot:      call.Snapshot(),
			Authorization: call.Authorization(),
			PolicyID:      h.PolicyID,
		},
		Metadata: h.Metadata,
	})
	if err != nil {
		return ToolResult{}, err
	}
	if err := h.validateSnapshot(ctx, call); err != nil {
		return ToolResult{}, err
	}
	if err := h.validateAuthorization(ctx, call); err != nil {
		return ToolResult{}, err
	}
	return NewToolResultWithEmbeddingSpaces(
		call.CorrelationID(), ActionRetrieve, pack.Evidence(),
		call.Snapshot(), call.Authorization(), embeddingSpaces,
	)
}

func (h *ExplorerToolHost) OpenSection(
	ctx context.Context,
	call ToolContext,
	request OpenSectionRequest,
) (ToolResult, error) {
	if err := h.validate(); err != nil {
		return ToolResult{}, err
	}
	if err := h.validateSnapshot(ctx, call); err != nil {
		return ToolResult{}, err
	}
	if err := h.validateAuthorization(ctx, call); err != nil {
		return ToolResult{}, err
	}
	view, err := h.Client.Document(ctx, request.DocumentID(), request.RevisionID())
	if err != nil {
		return ToolResult{}, err
	}
	if err := h.validateSnapshot(ctx, call); err != nil {
		return ToolResult{}, err
	}
	if err := h.validateAuthorization(ctx, call); err != nil {
		return ToolResult{}, err
	}
	anchors, err := sectionAnchors(ctx, view, request, call.Budgets().MaxEvidence)
	if err != nil {
		return ToolResult{}, err
	}
	return NewToolResult(
		call.CorrelationID(), ActionOpenSection, anchors,
		call.Snapshot(), call.Authorization())
}

func (h *ExplorerToolHost) Neighbors(
	ctx context.Context,
	call ToolContext,
	request NeighborsRequest,
) (ToolResult, error) {
	if err := h.validate(); err != nil {
		return ToolResult{}, err
	}
	if h.BoundedClient == nil {
		return ToolResult{}, invalid("bounded explorer client is required for neighbors")
	}
	maxNodes := call.Budgets().MaxGraphNodes
	if maxNodes <= 0 {
		maxNodes = request.Fanout() + request.Hops()
	} else {
		maxNodes = remainingGraphNodeCap(call.Context(), request.NodeID(), maxNodes)
	}
	maxAnchors := call.Budgets().MaxEvidence
	if maxAnchors <= 0 {
		maxAnchors = request.Fanout() * request.Hops()
	}
	if err := h.validateSnapshot(ctx, call); err != nil {
		return ToolResult{}, err
	}
	if err := h.validateAuthorization(ctx, call); err != nil {
		return ToolResult{}, err
	}
	bounded, err := h.BoundedClient.BoundedNeighborhood(ctx, explorer.BoundedNeighborhoodRequest{
		NodeIDs:         []shoal.ID{request.NodeID()},
		Depth:           uint32(request.Hops()),
		Fanout:          uint32(request.Fanout()),
		MaxNodes:        uint32(maxNodes),
		MaxScannedEdges: uint32(authorizationScanEdgeLimit(request.Fanout(), maxNodes)),
		Direction:       explorer.GraphDirectionOutgoing,
	})
	if err != nil {
		return ToolResult{}, err
	}
	if err := validateBoundedNeighborhoodResponse(
		explorer.BoundedNeighborhoodRequest{
			NodeIDs: []shoal.ID{request.NodeID()}, Depth: uint32(request.Hops()),
			Fanout: uint32(request.Fanout()), MaxNodes: uint32(maxNodes),
			Direction: explorer.GraphDirectionOutgoing,
		},
		bounded.Neighborhood,
	); err != nil {
		return ToolResult{}, err
	}
	if err := h.validateSnapshot(ctx, call); err != nil {
		return ToolResult{}, err
	}
	if err := h.validateAuthorization(ctx, call); err != nil {
		return ToolResult{}, err
	}
	anchors, err := graphAnchorsFromNeighborhood(
		request.NodeID(), bounded.Neighborhood, request.Hops(), maxAnchors)
	if err != nil {
		return ToolResult{}, err
	}
	return NewToolResult(
		call.CorrelationID(), ActionNeighbors, anchors,
		call.Snapshot(), call.Authorization())
}

func remainingGraphNodeCap(pack inference.ContextPack, seed shoal.ID, maxNodes int) int {
	existing := graphNodeSet(pack.Evidence())
	alreadyCounted := len(existing)
	if _, ok := existing[seed]; ok && alreadyCounted > 0 {
		alreadyCounted--
	}
	remaining := maxNodes - alreadyCounted
	if remaining < 1 {
		return 1
	}
	return remaining
}

func validateBoundedNeighborhoodResponse(
	request explorer.BoundedNeighborhoodRequest,
	neighborhood explorer.Neighborhood,
) error {
	if uint32(len(neighborhood.Nodes)) > request.MaxNodes {
		return budget("graph nodes")
	}
	typeFilter := make(map[string]struct{}, len(request.EdgeTypes))
	for _, edgeType := range request.EdgeTypes {
		typeFilter[edgeType] = struct{}{}
	}
	counts := make(map[shoal.ID]uint32)
	for _, edge := range neighborhood.Edges {
		if len(typeFilter) > 0 {
			if _, ok := typeFilter[edge.Type]; !ok {
				return invalid("bounded neighborhood returned an edge outside the requested type filter")
			}
		}
		switch request.Direction {
		case explorer.GraphDirectionIncoming:
			counts[edge.To]++
		case explorer.GraphDirectionBoth, "":
			counts[edge.From]++
			if edge.To != edge.From {
				counts[edge.To]++
			}
		default:
			counts[edge.From]++
		}
	}
	for _, count := range counts {
		if count > request.Fanout {
			return budget("graph fanout")
		}
	}
	return nil
}

func (h *ExplorerToolHost) validateAuthorization(ctx context.Context, call ToolContext) error {
	validator, ok := h.Client.(interface {
		ValidateAuthorization(context.Context, inference.AuthPin) error
	})
	if !ok {
		return nil
	}
	return validator.ValidateAuthorization(ctx, call.Authorization())
}

func validateRetrievalResponseShape(response retrieval.Response, requestLimit int, budgets Budgets) error {
	if len(response.Results) > requestLimit {
		return budget("retrieval result")
	}
	maxEvidence := budgets.MaxEvidence
	if maxEvidence <= 0 {
		maxEvidence = requestLimit
	}
	maxPathNodes := budgets.MaxGraphNodes
	if maxPathNodes <= 0 {
		maxPathNodes = requestLimit + 1
	}
	maxPathEdges := maxPathNodes - 1
	totalEvidence := 0
	for _, result := range response.Results {
		if len(result.Evidence) > maxEvidence {
			return budget("retrieval evidence")
		}
		totalEvidence += len(result.Evidence)
		if totalEvidence > maxEvidence {
			return budget("retrieval evidence")
		}
		for _, evidence := range result.Evidence {
			pathNodes := len(evidence.Path.Nodes)
			pathEdges := len(evidence.Path.Edges)
			if pathNodes == 0 && pathEdges == 0 {
				continue
			}
			if pathNodes > maxPathNodes {
				return budget("retrieval graph path")
			}
			if pathEdges > maxPathEdges {
				return budget("retrieval graph path")
			}
		}
	}
	return nil
}

func (h *ExplorerToolHost) validateSnapshot(ctx context.Context, call ToolContext) error {
	if h.BoundedClient == nil {
		return nil
	}
	snapshot, err := h.BoundedClient.Snapshot(ctx)
	if err != nil {
		return err
	}
	if shoal.ID(snapshot.ID) != call.Snapshot().ID() ||
		!snapshot.AsOf.UTC().Equal(call.Snapshot().AsOf()) {
		return invalid("explorer snapshot does not match the pinned context")
	}
	return nil
}

func (h *ExplorerToolHost) verifyRetrievalPaths(ctx context.Context, response retrieval.Response, budgets Budgets) error {
	if h.BoundedClient == nil {
		return nil
	}
	for _, result := range response.Results {
		for _, evidence := range result.Evidence {
			for _, node := range evidence.Path.Nodes {
				if err := h.verifyNode(ctx, node, budgets); err != nil {
					return err
				}
			}
			for _, edge := range evidence.Path.Edges {
				if err := h.verifyEdge(ctx, edge, budgets); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (h *ExplorerToolHost) verifyNode(ctx context.Context, expected graph.Node, budgets Budgets) error {
	fanout, maxNodes, _ := graphVerificationBounds(budgets)
	bounded, err := h.BoundedClient.BoundedNeighborhood(ctx, explorer.BoundedNeighborhoodRequest{
		NodeIDs:         []shoal.ID{expected.ID},
		Depth:           1,
		Fanout:          uint32(fanout),
		MaxNodes:        uint32(maxNodes),
		MaxScannedEdges: uint32(authorizationScanEdgeLimit(fanout, maxNodes)),
		Direction:       explorer.GraphDirectionOutgoing,
	})
	if err != nil {
		return err
	}
	for _, node := range bounded.Neighborhood.Nodes {
		if node.ID == expected.ID {
			if !reflect.DeepEqual(node, expected) {
				return invalid("retrieval graph node does not match Explorer data")
			}
			return nil
		}
	}
	return invalid("retrieval graph node is absent from Explorer data")
}

func (h *ExplorerToolHost) verifyEdge(ctx context.Context, expected graph.Edge, budgets Budgets) error {
	fanout, maxNodes, maxPages := graphVerificationBounds(budgets)
	after := shoal.ID("")
	for page := 0; page < maxPages; page++ {
		bounded, err := h.BoundedClient.BoundedNeighborhood(ctx, explorer.BoundedNeighborhoodRequest{
			NodeIDs:         []shoal.ID{expected.From},
			Depth:           1,
			Fanout:          uint32(fanout),
			MaxNodes:        uint32(maxNodes),
			MaxScannedEdges: uint32(authorizationScanEdgeLimit(fanout, maxNodes)),
			Direction:       explorer.GraphDirectionOutgoing,
			AfterEdgeID:     after,
		})
		if err != nil {
			return err
		}
		for _, edge := range bounded.Neighborhood.Edges {
			if edge.ID == expected.ID {
				if !reflect.DeepEqual(edge, expected) {
					return invalid("retrieval graph edge does not match Explorer data")
				}
				return nil
			}
		}
		if !bounded.Continuation {
			return invalid("retrieval graph edge is absent from Explorer data")
		}
		after = bounded.NextAfterEdgeID
	}
	return budget("graph verification")
}

func graphVerificationBounds(budgets Budgets) (int, int, int) {
	fanout := budgets.MaxFanout
	if fanout <= 0 {
		fanout = 1
	}
	maxNodes := budgets.MaxGraphNodes
	if maxNodes <= 0 {
		maxNodes = fanout + 1
	}
	maxPages := budgets.MaxFanout
	if maxPages <= 0 {
		maxPages = 1
	}
	return fanout, maxNodes, maxPages
}

func authorizationScanEdgeLimit(fanout, maxNodes int) int {
	if fanout <= 0 {
		fanout = 1
	}
	if maxNodes <= 0 {
		maxNodes = fanout + 1
	}
	return fanout * maxNodes
}

func sectionAnchors(
	ctx context.Context,
	view explorer.DocumentView,
	request OpenSectionRequest,
	maxAnchors int,
) ([]inference.EvidenceAnchor, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := view.Document.Validate(); err != nil {
		return nil, err
	}
	if err := view.Revision.Validate(); err != nil {
		return nil, err
	}
	if view.Document.ID != request.DocumentID() || view.Revision.ID != request.RevisionID() {
		return nil, invalid("hydrated document identity does not match request")
	}
	if maxAnchors <= 0 {
		maxAnchors = inference.MaxEvidenceAnchors
	}
	var section *explorer.SectionView
	var find func(*explorer.SectionView) error
	find = func(current *explorer.SectionView) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if section != nil {
			return nil
		}
		if current.Section.ID == request.SectionID() {
			section = current
			return nil
		}
		for i := range current.Children {
			if err := find(&current.Children[i]); err != nil {
				return err
			}
		}
		return nil
	}
	if err := find(&view.Root); err != nil {
		return nil, err
	}
	if section == nil {
		return nil, invalid("requested section is absent from hydrated document")
	}
	capacity := len(section.Spans)
	if capacity > maxAnchors {
		capacity = maxAnchors
	}
	anchors := make([]inference.EvidenceAnchor, 0, capacity)
	for _, span := range section.Spans {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if span.DocumentID != view.Document.ID ||
			span.RevisionID != view.Revision.ID ||
			span.SectionID != section.Section.ID {
			return nil, invalid("hydrated span ownership does not match section")
		}
		if span.Text == "" {
			continue
		}
		if len(anchors) >= maxAnchors {
			return nil, budget("section evidence")
		}
		anchor, err := inference.NewDocumentAnchor(document.Citation{
			DocumentID: span.DocumentID,
			RevisionID: span.RevisionID,
			SectionID:  span.SectionID,
			SpanID:     span.ID,
			Range:      span.Range,
		}, span.Text)
		if err != nil {
			return nil, err
		}
		anchors = append(anchors, anchor)
	}
	return anchors, nil
}

func (h *ExplorerToolHost) validate() error {
	if h == nil || h.Client == nil {
		return invalid("explorer client is required")
	}
	if h.Builder.Reader == nil {
		if h.Client == nil {
			return invalid("explorer reader is required")
		}
	}
	if _, err := (retrieval.Request{Text: "validate", Modes: h.RetrievalModes}).Normalize(); err != nil {
		return err
	}
	return nil
}

func (h *ExplorerToolHost) builder() contextpack.Builder {
	return h.builderFor(0, 0)
}

func (h *ExplorerToolHost) builderFor(fanout, maxNodes int) contextpack.Builder {
	builder := h.Builder
	if h.BoundedClient != nil {
		builder.Reader = boundedContextReader{
			documents: h.Client,
			graph:     h.BoundedClient,
			fanout:    fanout,
			maxNodes:  maxNodes,
		}
	} else if builder.Reader == nil {
		builder.Reader = h.Client
	}
	return builder
}

type boundedContextReader struct {
	documents explorer.Client
	graph     explorer.BoundedClient
	fanout    int
	maxNodes  int
}

func (r boundedContextReader) Document(ctx context.Context, documentID, revisionID shoal.ID) (explorer.DocumentView, error) {
	return r.documents.Document(ctx, documentID, revisionID)
}

func (r boundedContextReader) Neighborhood(ctx context.Context, request explorer.NeighborhoodRequest) (explorer.Neighborhood, error) {
	normalized, err := request.Normalize()
	if err != nil {
		return explorer.Neighborhood{}, err
	}
	fanout := r.fanout
	if fanout <= 0 {
		fanout = 1
	}
	maxNodes := r.maxNodes
	if maxNodes <= 0 {
		maxNodes = fanout + len(normalized.NodeIDs)
	}
	boundedRequest := explorer.BoundedNeighborhoodRequest{
		NodeIDs:         normalized.NodeIDs,
		Depth:           normalized.Depth,
		Fanout:          uint32(fanout),
		MaxNodes:        uint32(maxNodes),
		MaxScannedEdges: uint32(authorizationScanEdgeLimit(fanout, maxNodes)),
		EdgeTypes:       normalized.EdgeTypes,
		Direction:       explorer.GraphDirectionOutgoing,
	}
	bounded, err := r.graph.BoundedNeighborhood(ctx, boundedRequest)
	if err != nil {
		return explorer.Neighborhood{}, err
	}
	if err := validateBoundedNeighborhoodResponse(boundedRequest, bounded.Neighborhood); err != nil {
		return explorer.Neighborhood{}, err
	}
	return bounded.Neighborhood, nil
}

func neighborhoodsFromRetrievalResponse(response retrieval.Response) ([]explorer.Neighborhood, error) {
	nodes := make(map[shoal.ID]graph.Node)
	edges := make(map[shoal.ID]graph.Edge)
	for _, result := range response.Results {
		for _, evidence := range result.Evidence {
			path := evidence.Path
			for _, node := range path.Nodes {
				if err := node.Validate(); err != nil {
					return nil, err
				}
				if existing, duplicate := nodes[node.ID]; duplicate && !reflect.DeepEqual(existing, node) {
					return nil, invalid("retrieval graph path has duplicate node ID")
				}
				nodes[node.ID] = node
			}
			for _, edge := range path.Edges {
				if err := edge.Validate(); err != nil {
					return nil, err
				}
				if existing, duplicate := edges[edge.ID]; duplicate && !reflect.DeepEqual(existing, edge) {
					return nil, invalid("retrieval graph path has duplicate edge ID")
				}
				edges[edge.ID] = edge
			}
		}
	}
	if len(nodes) == 0 && len(edges) == 0 {
		return nil, nil
	}
	neighborhood := explorer.Neighborhood{
		Nodes: make([]graph.Node, 0, len(nodes)),
		Edges: make([]graph.Edge, 0, len(edges)),
	}
	for _, node := range nodes {
		neighborhood.Nodes = append(neighborhood.Nodes, node)
	}
	for _, edge := range edges {
		neighborhood.Edges = append(neighborhood.Edges, edge)
	}
	sort.Slice(neighborhood.Nodes, func(i, j int) bool {
		return shoal.CompareID(neighborhood.Nodes[i].ID, neighborhood.Nodes[j].ID) < 0
	})
	sort.Slice(neighborhood.Edges, func(i, j int) bool {
		return shoal.CompareID(neighborhood.Edges[i].ID, neighborhood.Edges[j].ID) < 0
	})
	return []explorer.Neighborhood{neighborhood}, nil
}

func graphAnchorsFromNeighborhood(
	seed shoal.ID,
	neighborhood explorer.Neighborhood,
	maxHops int,
	maxAnchors int,
) ([]inference.EvidenceAnchor, error) {
	if maxHops <= 0 || maxAnchors <= 0 {
		return nil, invalid("neighbors hops and fanout must be positive")
	}
	nodes := make(map[shoal.ID]graph.Node, len(neighborhood.Nodes))
	for _, node := range neighborhood.Nodes {
		if err := node.Validate(); err != nil {
			return nil, err
		}
		if _, duplicate := nodes[node.ID]; duplicate {
			return nil, invalid("bounded neighborhood has duplicate node ID")
		}
		nodes[node.ID] = node
	}
	if _, ok := nodes[seed]; !ok {
		return nil, invalid("bounded neighborhood omitted the requested node")
	}
	edges := append([]graph.Edge(nil), neighborhood.Edges...)
	sort.Slice(edges, func(i, j int) bool {
		return shoal.CompareID(edges[i].ID, edges[j].ID) < 0
	})
	outgoing := make(map[shoal.ID][]graph.Edge)
	edgesByID := make(map[shoal.ID]graph.Edge, len(edges))
	seenEdges := make(map[shoal.ID]struct{}, len(edges))
	for _, edge := range edges {
		if err := edge.Validate(); err != nil {
			return nil, err
		}
		if _, duplicate := seenEdges[edge.ID]; duplicate {
			return nil, invalid("bounded neighborhood has duplicate edge ID")
		}
		seenEdges[edge.ID] = struct{}{}
		edgesByID[edge.ID] = edge
		outgoing[edge.From] = append(outgoing[edge.From], edge)
	}
	assertionsByEdge, err := graphAssertionsByEdge(
		neighborhood.Assertions, edgesByID)
	if err != nil {
		return nil, err
	}
	type queuedPath struct {
		nodes []graph.Node
		edges []graph.Edge
		seen  map[shoal.ID]struct{}
	}
	anchors := make([]inference.EvidenceAnchor, 0, len(edges))
	seenAnchors := make(map[shoal.ID]struct{})
	queue := []queuedPath{{
		nodes: []graph.Node{nodes[seed]},
		seen:  map[shoal.ID]struct{}{seed: struct{}{}},
	}}
	for len(queue) > 0 && len(anchors) < maxAnchors {
		current := queue[0]
		queue = queue[1:]
		if len(current.edges) >= maxHops {
			continue
		}
		last := current.nodes[len(current.nodes)-1]
		for _, edge := range outgoing[last.ID] {
			if len(anchors) >= maxAnchors {
				break
			}
			next, ok := nodes[edge.To]
			if !ok {
				return nil, invalid("bounded neighborhood edge has a missing endpoint")
			}
			if _, cycle := current.seen[next.ID]; cycle {
				continue
			}
			nextSeen := make(map[shoal.ID]struct{}, len(current.seen)+1)
			for id := range current.seen {
				nextSeen[id] = struct{}{}
			}
			nextSeen[next.ID] = struct{}{}
			nextPath := queuedPath{
				nodes: append(append([]graph.Node(nil), current.nodes...), next),
				edges: append(append([]graph.Edge(nil), current.edges...), edge),
				seen:  nextSeen,
			}
			path := graph.Path{
				Nodes: nextPath.nodes,
				Edges: nextPath.edges,
			}
			var assertions []interaction.AssertionReference
			for _, pathEdge := range nextPath.edges {
				assertions = append(assertions, assertionsByEdge[pathEdge.ID]...)
			}
			anchor, err := inference.NewGraphAnchorWithAssertions(
				path, assertions)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
			}
			if _, duplicate := seenAnchors[anchor.ID()]; !duplicate {
				seenAnchors[anchor.ID()] = struct{}{}
				anchors = append(anchors, anchor)
			}
			if len(nextPath.edges) < maxHops && len(anchors) < maxAnchors {
				queue = append(queue, nextPath)
			}
		}
	}
	for _, edge := range edges {
		if _, fromOK := nodes[edge.From]; !fromOK {
			return nil, invalid("bounded neighborhood edge has a missing endpoint")
		}
		if _, toOK := nodes[edge.To]; !toOK {
			return nil, invalid("bounded neighborhood edge has a missing endpoint")
		}
	}
	return anchors, nil
}

func graphAssertionsByEdge(
	assertions []ontology.Assertion,
	edges map[shoal.ID]graph.Edge,
) (map[shoal.ID][]interaction.AssertionReference, error) {
	const assertionEdgeIDProperty = "shoal.graph.edge_id"
	result := make(map[shoal.ID][]interaction.AssertionReference)
	for _, assertion := range assertions {
		if err := assertion.Validate(); err != nil {
			return nil, err
		}
		if assertion.Origin() == ontology.AssertionDerived {
			return nil, invalid(
				"derived assertion cannot be used as source graph evidence")
		}
		matched := make(map[shoal.ID]struct{})
		if edgeID := shoal.ID(
			assertion.Metadata()[assertionEdgeIDProperty]); edgeID != "" {
			if edge, ok := edges[edgeID]; ok {
				if !graphAssertionMatchesEdge(assertion, edge) {
					return nil, invalid(
						"graph assertion does not match its authoritative edge")
				}
				matched[edgeID] = struct{}{}
			}
		}
		if edge, ok := edges[assertion.ID()]; ok {
			if !graphAssertionMatchesEdge(assertion, edge) {
				return nil, invalid(
					"graph assertion does not match its authoritative edge")
			}
			matched[assertion.ID()] = struct{}{}
		}
		if len(matched) == 0 {
			return nil, invalid(
				"graph assertion has no authoritative neighborhood edge")
		}
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
	return result, nil
}

func graphAssertionMatchesEdge(
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
