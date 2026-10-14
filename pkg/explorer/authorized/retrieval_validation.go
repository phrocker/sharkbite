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
	"errors"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type canonicalRetrievalCorpus struct {
	documents map[shoal.ID]*canonicalRetrievalDocument
	nodes     map[shoal.ID]graph.Node
}

type canonicalRetrievalDocument struct {
	registration RevisionRegistration
	view         explorer.DocumentView
	sections     map[shoal.ID]document.Section
	spans        map[shoal.ID]document.Span
	nodes        map[shoal.ID]graph.Node
	edges        map[shoal.ID]graph.Edge
	edgePairs    map[canonicalEdgePair]graph.Edge
}

type canonicalEdgePair struct {
	from shoal.ID
	to   shoal.ID
}

// VectorScorer recomputes vector scores for exact citations over a trusted
// corpus. Callers must provide one explicitly; it is not inferred from the
// untrusted retrieval backend.
type VectorScorer interface {
	VectorScores(
		context.Context, explorer.VectorScoreRequest,
	) (map[shoal.ID]shoal.Score, error)
}

func (c *Client) authorizedVectorScoringAvailable() bool {
	return !isNilDependency(c.vectorScorer)
}

func (c *Client) hydrateRetrievalCorpus(
	ctx context.Context,
	documentIDs []shoal.ID,
	registrations map[shoal.ID]RevisionRegistration,
	decision auth.Decision,
	now time.Time,
) (*canonicalRetrievalCorpus, error) {
	corpus := &canonicalRetrievalCorpus{
		documents: make(map[shoal.ID]*canonicalRetrievalDocument, len(documentIDs)),
		nodes:     make(map[shoal.ID]graph.Node),
	}
	for _, documentID := range documentIDs {
		registration, ok := registrations[documentID]
		if !ok {
			return nil, inconsistentRetrieval()
		}
		view, err := c.base.Document(
			ctx, documentID, registration.RevisionID)
		if err != nil {
			if shoal.IsErrorCode(err, shoal.ErrorNotFound) {
				return nil, inconsistentRetrieval()
			}
			return nil, err
		}
		if err := verifyDocumentViewRegistration(view, registration); err != nil {
			return nil, inconsistentRetrieval()
		}
		canonical, err := buildCanonicalRetrievalDocument(view, registration)
		if err != nil {
			return nil, inconsistentRetrieval()
		}
		for nodeID, node := range canonical.nodes {
			if _, duplicate := corpus.nodes[nodeID]; duplicate {
				return nil, inconsistentRetrieval()
			}
			current, ok, err := c.policyStore.Node(ctx, nodeID)
			if err != nil {
				return nil, policyCatalogReadError(ctx, err)
			}
			if !ok || current.DocumentID != documentID ||
				current.RevisionID != registration.RevisionID {
				return nil, inconsistentRetrieval()
			}
			allowed, err := ruleAllows(
				current.Rule, decision, auth.OperationRetrieve, now)
			if err != nil {
				return nil, err
			}
			if !allowed {
				return nil, inconsistentRetrieval()
			}
			corpus.nodes[nodeID] = cloneGraphNode(node)
		}
		for edgeID, edge := range canonical.edges {
			stored, ok, err := c.policyStore.Edge(ctx, edgeID)
			if err != nil {
				return nil, policyCatalogReadError(ctx, err)
			}
			if !ok || stored.DocumentID != documentID ||
				stored.RevisionID != registration.RevisionID ||
				!graphEdgesEqual(stored.Edge, edge) {
				return nil, inconsistentRetrieval()
			}
			allowed, err := c.edgeAllows(
				ctx, stored, decision, auth.OperationRetrieve, now)
			if err != nil {
				return nil, err
			}
			if !allowed {
				return nil, inconsistentRetrieval()
			}
		}
		corpus.documents[documentID] = canonical
	}
	return corpus, nil
}

func buildCanonicalRetrievalDocument(
	view explorer.DocumentView,
	registration RevisionRegistration,
) (*canonicalRetrievalDocument, error) {
	canonical := &canonicalRetrievalDocument{
		registration: registration,
		view:         cloneDocumentView(view),
		sections:     make(map[shoal.ID]document.Section),
		spans:        make(map[shoal.ID]document.Span),
		nodes:        make(map[shoal.ID]graph.Node),
		edges:        make(map[shoal.ID]graph.Edge),
		edgePairs:    make(map[canonicalEdgePair]graph.Edge),
	}
	// The base copies a source's declared visibility onto its document,
	// section and span nodes (parse.go materializeGraph). The canonical
	// reconstruction must carry the same property, or every labelled
	// document fails validation and is unreadable even to a reader holding
	// every label (#570). The labels come from the digest-verified revision
	// metadata, never from the untrusted node being compared.
	labels, err := interaction.ParseVisibility(
		view.Document.Metadata[interaction.PropertyVisibility])
	if err != nil {
		return nil, inconsistentRetrieval()
	}
	visibility := interaction.Expression(labels)
	withVisibility := func(properties shoal.Metadata) shoal.Metadata {
		if visibility != "" {
			properties[interaction.PropertyVisibility] = visibility
		}
		return properties
	}
	canonical.nodes[view.Document.ID] = graph.Node{
		ID: view.Document.ID, Kind: "document", Labels: []string{"document"},
		Properties: withVisibility(shoal.Metadata{
			"title":       view.Document.Title,
			"revision_id": string(view.Revision.ID),
		}),
	}
	expectedEdgePairs := make(map[canonicalEdgePair]struct{})
	var visit func(explorer.SectionView) error
	visit = func(sectionView explorer.SectionView) error {
		section := cloneSection(sectionView.Section)
		if _, duplicate := canonical.sections[section.ID]; duplicate {
			return inconsistentRetrieval()
		}
		canonical.sections[section.ID] = section
		canonical.nodes[section.ID] = graph.Node{
			ID: section.ID, Kind: "section", Labels: []string{"section"},
			Properties: withVisibility(shoal.Metadata{
				"heading":     section.Heading,
				"document_id": string(view.Document.ID),
				"revision_id": string(view.Revision.ID),
			}),
		}
		parentID := section.ParentID
		if parentID == "" {
			parentID = view.Document.ID
		}
		expectedEdgePairs[canonicalEdgePair{
			from: parentID,
			to:   section.ID,
		}] = struct{}{}
		for _, spanValue := range sectionView.Spans {
			span := cloneSpan(spanValue)
			if _, duplicate := canonical.spans[span.ID]; duplicate {
				return inconsistentRetrieval()
			}
			canonical.spans[span.ID] = span
			canonical.nodes[span.ID] = graph.Node{
				ID: span.ID, Kind: "span", Labels: []string{"evidence"},
				Properties: withVisibility(shoal.Metadata{
					"document_id": string(view.Document.ID),
					"revision_id": string(view.Revision.ID),
					"section_id":  string(span.SectionID),
				}),
			}
			expectedEdgePairs[canonicalEdgePair{
				from: section.ID,
				to:   span.ID,
			}] = struct{}{}
		}
		for _, child := range sectionView.Children {
			if err := visit(child); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(view.Root); err != nil {
		return nil, err
	}
	if len(canonical.nodes) != len(registration.NodeIDs) {
		return nil, inconsistentRetrieval()
	}
	for _, nodeID := range registration.NodeIDs {
		if _, ok := canonical.nodes[nodeID]; !ok {
			return nil, inconsistentRetrieval()
		}
	}
	if len(registration.IntrinsicEdges) != len(expectedEdgePairs) {
		return nil, inconsistentRetrieval()
	}
	for _, edgeValue := range registration.IntrinsicEdges {
		edge := cloneGraphEdge(edgeValue)
		if _, ok := canonical.nodes[edge.From]; !ok {
			return nil, inconsistentRetrieval()
		}
		if _, ok := canonical.nodes[edge.To]; !ok {
			return nil, inconsistentRetrieval()
		}
		if _, duplicate := canonical.edges[edge.ID]; duplicate {
			return nil, inconsistentRetrieval()
		}
		pair := canonicalEdgePair{from: edge.From, to: edge.To}
		if _, expected := expectedEdgePairs[pair]; !expected ||
			edge.Type != "contains" ||
			!scoresEqual(edge.Weight, 1) ||
			len(edge.Properties) != 0 {
			return nil, inconsistentRetrieval()
		}
		if _, duplicate := canonical.edgePairs[pair]; duplicate {
			return nil, inconsistentRetrieval()
		}
		canonical.edges[edge.ID] = edge
		canonical.edgePairs[pair] = edge
	}
	return canonical, nil
}

func (c *Client) validateRetrievedResponse(
	ctx context.Context,
	response retrieval.Response,
	request retrieval.Request,
	corpus *canonicalRetrievalCorpus,
	decision auth.Decision,
	now time.Time,
) error {
	return c.validateRetrievedResponseInternal(
		ctx, response, request, corpus, decision, now, nil, false)
}

func (c *Client) validateRetrievedResponseWithVectorScores(
	ctx context.Context,
	response retrieval.Response,
	request retrieval.Request,
	corpus *canonicalRetrievalCorpus,
	decision auth.Decision,
	now time.Time,
	vectorScores map[shoal.ID]shoal.Score,
) error {
	return c.validateRetrievedResponseInternal(
		ctx, response, request, corpus, decision, now, vectorScores, true)
}

func (c *Client) validateRetrievedResponseInternal(
	ctx context.Context,
	response retrieval.Response,
	request retrieval.Request,
	corpus *canonicalRetrievalCorpus,
	decision auth.Decision,
	now time.Time,
	vectorScores map[shoal.ID]shoal.Score,
	vectorScoresProvided bool,
) error {
	analyzer := retrieval.UnicodeTermAnalyzer{}
	scorer := retrieval.CoverageFusionScorer{}
	queryTerms := analyzer.Analyze(request.Text)
	nodeScope := make(map[shoal.ID]struct{}, len(request.Scope.NodeIDs))
	for _, nodeID := range request.Scope.NodeIDs {
		nodeScope[nodeID] = struct{}{}
	}
	if !vectorScoresProvided {
		var err error
		vectorScores, err = c.authorizedVectorScores(
			ctx, request, corpus, nodeScope)
		if err != nil {
			return err
		}
	}
	expectedIDs := make([]shoal.ID, 0)
	expectedResults := make([]retrieval.Result, 0)
	for _, canonical := range corpus.documents {
		for _, span := range canonical.spans {
			if len(nodeScope) > 0 &&
				!canonicalSpanInNodeScope(canonical, span, nodeScope) {
				continue
			}
			path, err := canonicalEvidencePath(canonical, span)
			if err != nil {
				return inconsistentRetrieval()
			}
			scores := canonicalComponentScores(
				canonical, span, path, queryTerms, scorer, analyzer)
			if err := applyCanonicalVectorScore(
				&scores, vectorScores, span.ID, request,
			); err != nil {
				return err
			}
			score := scorer.CombinedScore(request.Modes, scores)
			if score <= 0 {
				continue
			}
			expectedResults = append(expectedResults, retrieval.Result{
				ID: span.ID, Score: score,
			})
		}
	}
	sort.Slice(expectedResults, func(left, right int) bool {
		return retrieval.CompareResult(
			expectedResults[left], expectedResults[right]) < 0
	})
	if uint32(len(expectedResults)) > request.TopK {
		expectedResults = expectedResults[:request.TopK]
	}
	for _, result := range expectedResults {
		expectedIDs = append(expectedIDs, result.ID)
	}
	if len(response.Results) != len(expectedIDs) {
		return inconsistentRetrieval()
	}
	for index, result := range response.Results {
		if result.ID != expectedIDs[index] {
			return inconsistentRetrieval()
		}
	}
	for _, result := range response.Results {
		if len(result.Evidence) != 1 {
			return inconsistentRetrieval()
		}
		evidence := result.Evidence[0]
		citation := evidence.Citation
		canonical, ok := corpus.documents[citation.DocumentID]
		if !ok || canonical.registration.RevisionID != citation.RevisionID {
			return inconsistentRetrieval()
		}
		span, ok := canonical.spans[citation.SpanID]
		if !ok || result.ID != span.ID ||
			citation.SectionID != span.SectionID ||
			citation.Range != span.Range ||
			evidence.Quote != span.Text {
			return inconsistentRetrieval()
		}
		if len(nodeScope) > 0 &&
			!canonicalSpanInNodeScope(canonical, span, nodeScope) {
			return inconsistentRetrieval()
		}
		expectedPath, err := canonicalEvidencePath(canonical, span)
		if err != nil || !graphPathsEqual(expectedPath, evidence.Path) {
			return inconsistentRetrieval()
		}
		for _, node := range evidence.Path.Nodes {
			current, ok, err := c.policyStore.Node(ctx, node.ID)
			if err != nil {
				return policyCatalogReadError(ctx, err)
			}
			if !ok || current.DocumentID != citation.DocumentID ||
				current.RevisionID != citation.RevisionID {
				return inconsistentRetrieval()
			}
			allowed, err := ruleAllows(
				current.Rule, decision, auth.OperationRetrieve, now)
			if err != nil {
				return err
			}
			if !allowed {
				return inconsistentRetrieval()
			}
		}
		for _, edge := range evidence.Path.Edges {
			stored, ok, err := c.policyStore.Edge(ctx, edge.ID)
			if err != nil {
				return policyCatalogReadError(ctx, err)
			}
			if !ok || !graphEdgesEqual(stored.Edge, edge) {
				return inconsistentRetrieval()
			}
			allowed, err := c.edgeAllows(
				ctx, stored, decision, auth.OperationRetrieve, now)
			if err != nil {
				return err
			}
			if !allowed {
				return inconsistentRetrieval()
			}
		}
		scores := canonicalComponentScores(
			canonical, span, expectedPath, queryTerms, scorer, analyzer)
		if err := applyCanonicalVectorScore(
			&scores, vectorScores, span.ID, request,
		); err != nil {
			return err
		}
		expectedScore := scorer.CombinedScore(request.Modes, scores)
		if expectedScore <= 0 ||
			!scoresEqual(result.Score, expectedScore) ||
			!scoresEqual(evidence.Score, expectedScore) {
			return inconsistentRetrieval()
		}
		if !canonicalExplanationEqual(
			result.Explanation, request, scores, analyzer, scorer,
		) {
			return inconsistentRetrieval()
		}
	}
	return nil
}

func (c *Client) authorizedVectorScores(
	ctx context.Context,
	request retrieval.Request,
	corpus *canonicalRetrievalCorpus,
	nodeScope map[shoal.ID]struct{},
) (map[shoal.ID]shoal.Score, error) {
	if !request.HasMode(retrieval.ModeVector) {
		return nil, nil
	}
	if isNilDependency(c.vectorScorer) {
		return nil, shoal.NewError(
			shoal.ErrorUnavailable,
			"authorized vector retrieval requires trusted vector validation",
		)
	}
	var citations []document.Citation
	for _, canonical := range corpus.documents {
		for _, span := range canonical.spans {
			if len(nodeScope) > 0 &&
				!canonicalSpanInNodeScope(canonical, span, nodeScope) {
				continue
			}
			citations = append(citations, document.Citation{
				DocumentID: span.DocumentID,
				RevisionID: span.RevisionID,
				SectionID:  span.SectionID,
				SpanID:     span.ID,
				Range:      span.Range,
			})
		}
	}
	scores, err := c.vectorScorer.VectorScores(ctx, explorer.VectorScoreRequest{
		Text:      request.Text,
		Citations: citations,
	})
	if err != nil {
		return nil, authorizedVectorError(err)
	}
	if len(scores) != len(citations) {
		return nil, inconsistentRetrieval()
	}
	return scores, nil
}

func authorizedVectorError(err error) error {
	switch {
	case errors.Is(err, context.Canceled),
		shoal.IsErrorCode(err, shoal.ErrorCanceled):
		return shoal.WrapError(shoal.ErrorCanceled, "authorized vector retrieval canceled", context.Canceled)
	case errors.Is(err, context.DeadlineExceeded),
		shoal.IsErrorCode(err, shoal.ErrorDeadline):
		return shoal.WrapError(
			shoal.ErrorDeadline,
			"authorized vector retrieval deadline exceeded",
			context.DeadlineExceeded,
		)
	case shoal.IsErrorCode(err, shoal.ErrorInvalidArgument):
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"authorized vector retrieval request is invalid",
		)
	case shoal.IsErrorCode(err, shoal.ErrorConflict):
		return shoal.NewError(
			shoal.ErrorConflict,
			"authorized vector retrieval has incompatible embedding metadata",
		)
	case shoal.IsErrorCode(err, shoal.ErrorUnavailable):
		return shoal.NewError(
			shoal.ErrorUnavailable,
			"an authorized embedding provider is unavailable",
		)
	default:
		return shoal.NewError(
			shoal.ErrorInternal,
			"authorized vector retrieval failed",
		)
	}
}

func applyCanonicalVectorScore(
	scores *retrieval.ComponentScores,
	vectorScores map[shoal.ID]shoal.Score,
	spanID shoal.ID,
	request retrieval.Request,
) error {
	if !request.HasMode(retrieval.ModeVector) {
		return nil
	}
	if scores == nil {
		return inconsistentRetrieval()
	}
	score, ok := vectorScores[spanID]
	if !ok {
		return inconsistentRetrieval()
	}
	scores.Vector = score
	return nil
}

func canonicalEvidencePath(
	canonical *canonicalRetrievalDocument,
	span document.Span,
) (graph.Path, error) {
	var sectionIDs []shoal.ID
	for sectionID := span.SectionID; sectionID != ""; {
		section, ok := canonical.sections[sectionID]
		if !ok {
			return graph.Path{}, inconsistentRetrieval()
		}
		sectionIDs = append(sectionIDs, sectionID)
		sectionID = section.ParentID
	}
	pathIDs := []shoal.ID{canonical.view.Document.ID}
	for index := len(sectionIDs) - 1; index >= 0; index-- {
		pathIDs = append(pathIDs, sectionIDs[index])
	}
	pathIDs = append(pathIDs, span.ID)
	path := graph.Path{
		Nodes: make([]graph.Node, 0, len(pathIDs)),
		Edges: make([]graph.Edge, 0, len(pathIDs)-1),
	}
	for index, nodeID := range pathIDs {
		node, ok := canonical.nodes[nodeID]
		if !ok {
			return graph.Path{}, inconsistentRetrieval()
		}
		path.Nodes = append(path.Nodes, cloneGraphNode(node))
		if index == 0 {
			continue
		}
		edge, ok := canonical.edgePairs[canonicalEdgePair{
			from: pathIDs[index-1],
			to:   nodeID,
		}]
		if !ok {
			return graph.Path{}, inconsistentRetrieval()
		}
		path.Edges = append(path.Edges, cloneGraphEdge(edge))
	}
	return path, nil
}

func canonicalSpanInNodeScope(
	canonical *canonicalRetrievalDocument,
	span document.Span,
	scope map[shoal.ID]struct{},
) bool {
	if _, ok := scope[canonical.view.Document.ID]; ok {
		return true
	}
	if _, ok := scope[span.ID]; ok {
		return true
	}
	for sectionID := span.SectionID; sectionID != ""; {
		if _, ok := scope[sectionID]; ok {
			return true
		}
		section := canonical.sections[sectionID]
		sectionID = section.ParentID
	}
	return false
}

func canonicalComponentScores(
	canonical *canonicalRetrievalDocument,
	span document.Span,
	path graph.Path,
	queryTerms retrieval.TermSet,
	scorer retrieval.CoverageFusionScorer,
	analyzer retrieval.UnicodeTermAnalyzer,
) retrieval.ComponentScores {
	lexical := scorer.Coverage(queryTerms, analyzer.Analyze(span.Text))
	headingText := canonical.view.Document.Title
	for sectionID := span.SectionID; sectionID != ""; {
		section := canonical.sections[sectionID]
		headingText += " " + section.Heading
		sectionID = section.ParentID
	}
	tree := scorer.Coverage(queryTerms, analyzer.Analyze(headingText))
	graphScore := shoal.Score(0)
	if len(path.Edges) > 0 {
		graphScore = scorer.Coverage(
			queryTerms, analyzer.Analyze(canonicalPathSearchText(path, span.Text)))
	}
	return retrieval.ComponentScores{
		Lexical: lexical,
		Tree:    tree,
		Graph:   graphScore,
	}
}

func canonicalPathSearchText(path graph.Path, quote string) string {
	var text strings.Builder
	text.WriteString(quote)
	for _, node := range path.Nodes {
		for _, label := range node.Labels {
			text.WriteByte(' ')
			text.WriteString(label)
		}
		for _, key := range []string{"title", "heading"} {
			if value := node.Properties[key]; value != "" {
				text.WriteByte(' ')
				text.WriteString(value)
			}
		}
	}
	for _, edge := range path.Edges {
		text.WriteByte(' ')
		text.WriteString(edge.Type)
	}
	return text.String()
}

func canonicalExplanationEqual(
	explanation *retrieval.Explanation,
	request retrieval.Request,
	scores retrieval.ComponentScores,
	analyzer retrieval.UnicodeTermAnalyzer,
	scorer retrieval.CoverageFusionScorer,
) bool {
	if !request.Explain {
		return explanation == nil
	}
	if explanation == nil ||
		len(explanation.Modes) != len(request.Modes) ||
		len(explanation.Scores) != len(request.Modes) {
		return false
	}
	for index, mode := range request.Modes {
		if explanation.Modes[index] != mode {
			return false
		}
		actual, ok := explanation.Scores[string(mode)]
		if !ok || !scoresEqual(actual, scorer.ModeScore(mode, scores)) {
			return false
		}
	}
	expectedSummary := "ranked source span using analyzer " + analyzer.Version() +
		" and scorer " + scorer.Version()
	if explanation.Summary == expectedSummary {
		return true
	}
	return request.HasMode(retrieval.ModeVector) &&
		strings.HasPrefix(explanation.Summary, expectedSummary+"; embedding_spaces=")
}

func graphPathsEqual(left, right graph.Path) bool {
	if len(left.Nodes) != len(right.Nodes) ||
		len(left.Edges) != len(right.Edges) {
		return false
	}
	for index := range left.Nodes {
		if !graphNodesEqual(left.Nodes[index], right.Nodes[index]) {
			return false
		}
	}
	for index := range left.Edges {
		if !graphEdgesEqual(left.Edges[index], right.Edges[index]) {
			return false
		}
	}
	return true
}

func graphNodesEqual(left, right graph.Node) bool {
	if left.ID != right.ID || left.Kind != right.Kind ||
		len(left.Labels) != len(right.Labels) ||
		len(left.Properties) != len(right.Properties) {
		return false
	}
	for index := range left.Labels {
		if left.Labels[index] != right.Labels[index] {
			return false
		}
	}
	for key, value := range left.Properties {
		if rightValue, ok := right.Properties[key]; !ok || rightValue != value {
			return false
		}
	}
	return true
}

func scoresEqual(left, right shoal.Score) bool {
	return math.Float64bits(float64(left)) == math.Float64bits(float64(right))
}
