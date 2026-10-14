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

package retrievalgrpc

import (
	"fmt"

	"github.com/phrocker/shoal-oss/internal/knowledgepb"
	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func requestToProto(request retrieval.Request) (*knowledgepb.RetrieveRequest, error) {
	normalized, err := request.Normalize()
	if err != nil {
		return nil, err
	}
	request = normalized
	if err := validateRequestWire(request); err != nil {
		return nil, err
	}

	var modes []knowledgepb.RetrievalMode
	if len(request.Modes) > 0 {
		modes = make([]knowledgepb.RetrievalMode, len(request.Modes))
		for i, mode := range request.Modes {
			protoMode, err := modeToProto(mode)
			if err != nil {
				return nil, err
			}
			modes[i] = protoMode
		}
	}

	var asOf *timestamppb.Timestamp
	if !request.AsOf.IsZero() {
		asOf = timestamppb.New(request.AsOf)
		if err := asOf.CheckValid(); err != nil {
			return nil, shoal.WrapError(
				shoal.ErrorInvalidArgument, "retrieval as_of is invalid", err)
		}
	}

	protoRequest := &knowledgepb.RetrieveRequest{
		Text:    request.Text,
		TopK:    request.TopK,
		Modes:   modes,
		AsOf:    asOf,
		Explain: request.Explain,
	}
	if len(request.Scope.DocumentIDs) > 0 || len(request.Scope.NodeIDs) > 0 {
		protoRequest.Scope = &knowledgepb.RetrievalScope{
			DocumentIds: idsToStrings(request.Scope.DocumentIDs),
			NodeIds:     idsToStrings(request.Scope.NodeIDs),
		}
	}
	return protoRequest, nil
}

func requestFromProto(request *knowledgepb.RetrieveRequest) (retrieval.Request, error) {
	if request == nil {
		return retrieval.Request{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "retrieval request is required")
	}

	var modes []retrieval.Mode
	if len(request.GetModes()) > 0 {
		modes = make([]retrieval.Mode, len(request.GetModes()))
		for i, mode := range request.GetModes() {
			publicMode, err := modeFromProto(mode)
			if err != nil {
				return retrieval.Request{}, err
			}
			modes[i] = publicMode
		}
	}

	var asOf = request.GetAsOf()
	if asOf != nil {
		if err := asOf.CheckValid(); err != nil {
			return retrieval.Request{}, shoal.WrapError(
				shoal.ErrorInvalidArgument, "retrieval as_of is invalid", err)
		}
	}

	publicRequest := retrieval.Request{
		Text:    request.GetText(),
		TopK:    request.GetTopK(),
		Modes:   modes,
		Explain: request.GetExplain(),
	}
	if asOf != nil {
		publicRequest.AsOf = asOf.AsTime()
	}
	if scope := request.GetScope(); scope != nil {
		publicRequest.Scope = retrieval.Scope{
			DocumentIDs: stringsToIDs(scope.GetDocumentIds()),
			NodeIDs:     stringsToIDs(scope.GetNodeIds()),
		}
	}
	if err := validateRequestWire(publicRequest); err != nil {
		return retrieval.Request{}, err
	}
	return publicRequest.Normalize()
}

func responseToProto(
	request retrieval.Request, response retrieval.Response,
) (*knowledgepb.RetrieveResponse, error) {
	if err := validateResponse(request, response); err != nil {
		return nil, err
	}
	var results []*knowledgepb.RetrievalResult
	if len(response.Results) > 0 {
		results = make([]*knowledgepb.RetrievalResult, len(response.Results))
		for i, result := range response.Results {
			protoResult, err := resultToProto(result)
			if err != nil {
				return nil, err
			}
			results[i] = protoResult
		}
	}
	return &knowledgepb.RetrieveResponse{
		RequestId:         string(response.RequestID),
		Results:           results,
		EmbeddingSpaceId:  string(response.EmbeddingSpaceID),
		EmbeddingSpaceIds: idsToStrings(response.EmbeddingSpaceIDs),
	}, nil
}

func responseFromProto(
	request retrieval.Request, response *knowledgepb.RetrieveResponse,
) (retrieval.Response, error) {
	if err := validateProtoResponse(response); err != nil {
		return retrieval.Response{}, err
	}
	var results []retrieval.Result
	if len(response.GetResults()) > 0 {
		results = make([]retrieval.Result, len(response.GetResults()))
		for i, result := range response.GetResults() {
			publicResult, err := resultFromProto(result)
			if err != nil {
				return retrieval.Response{}, err
			}
			results[i] = publicResult
		}
	}
	publicResponse := retrieval.Response{
		RequestID:         shoal.ID(response.GetRequestId()),
		EmbeddingSpaceID:  shoal.ID(response.GetEmbeddingSpaceId()),
		EmbeddingSpaceIDs: stringsToIDs(response.GetEmbeddingSpaceIds()),
		Results:           results,
	}
	if err := validateResponse(request, publicResponse); err != nil {
		return retrieval.Response{}, err
	}
	return publicResponse, nil
}

func resultToProto(result retrieval.Result) (*knowledgepb.RetrievalResult, error) {
	var evidence []*knowledgepb.Evidence
	if len(result.Evidence) > 0 {
		evidence = make([]*knowledgepb.Evidence, len(result.Evidence))
		for i, item := range result.Evidence {
			evidence[i] = evidenceToProto(item)
		}
	}

	var explanation *knowledgepb.Explanation
	if result.Explanation != nil {
		var modes []knowledgepb.RetrievalMode
		if len(result.Explanation.Modes) > 0 {
			modes = make([]knowledgepb.RetrievalMode, len(result.Explanation.Modes))
			for i, mode := range result.Explanation.Modes {
				protoMode, err := modeToProto(mode)
				if err != nil {
					return nil, err
				}
				modes[i] = protoMode
			}
		}
		explanation = &knowledgepb.Explanation{
			Modes:   modes,
			Summary: result.Explanation.Summary,
			Scores:  scoresToFloat64(result.Explanation.Scores),
		}
	}

	return &knowledgepb.RetrievalResult{
		Id:          string(result.ID),
		Score:       float64(result.Score),
		Evidence:    evidence,
		Explanation: explanation,
	}, nil
}

func resultFromProto(result *knowledgepb.RetrievalResult) (retrieval.Result, error) {
	if result == nil {
		return retrieval.Result{}, fmt.Errorf("retrieval result is required")
	}
	var evidence []retrieval.Evidence
	if len(result.GetEvidence()) > 0 {
		evidence = make([]retrieval.Evidence, len(result.GetEvidence()))
		for i, item := range result.GetEvidence() {
			evidence[i] = evidenceFromProto(item)
		}
	}

	var explanation *retrieval.Explanation
	if protoExplanation := result.GetExplanation(); protoExplanation != nil {
		var modes []retrieval.Mode
		if len(protoExplanation.GetModes()) > 0 {
			modes = make([]retrieval.Mode, len(protoExplanation.GetModes()))
			for i, mode := range protoExplanation.GetModes() {
				publicMode, err := modeFromProto(mode)
				if err != nil {
					return retrieval.Result{}, err
				}
				modes[i] = publicMode
			}
		}
		explanation = &retrieval.Explanation{
			Modes:   modes,
			Summary: protoExplanation.GetSummary(),
			Scores:  float64ToScores(protoExplanation.GetScores()),
		}
	}

	return retrieval.Result{
		ID:          shoal.ID(result.GetId()),
		Score:       shoal.Score(result.GetScore()),
		Evidence:    evidence,
		Explanation: explanation,
	}, nil
}

func evidenceToProto(evidence retrieval.Evidence) *knowledgepb.Evidence {
	return &knowledgepb.Evidence{
		Citation: citationToProto(evidence.Citation),
		Quote:    evidence.Quote,
		Path:     pathToProto(evidence.Path),
		Score:    float64(evidence.Score),
	}
}

func evidenceFromProto(evidence *knowledgepb.Evidence) retrieval.Evidence {
	if evidence == nil {
		return retrieval.Evidence{}
	}
	return retrieval.Evidence{
		Citation: citationFromProto(evidence.GetCitation()),
		Quote:    evidence.GetQuote(),
		Path:     pathFromProto(evidence.GetPath()),
		Score:    shoal.Score(evidence.GetScore()),
	}
}

func citationToProto(citation document.Citation) *knowledgepb.Citation {
	return &knowledgepb.Citation{
		DocumentId: string(citation.DocumentID),
		RevisionId: string(citation.RevisionID),
		SectionId:  string(citation.SectionID),
		SpanId:     string(citation.SpanID),
		Range:      rangeToProto(citation.Range),
	}
}

func citationFromProto(citation *knowledgepb.Citation) document.Citation {
	if citation == nil {
		return document.Citation{}
	}
	return document.Citation{
		DocumentID: shoal.ID(citation.GetDocumentId()),
		RevisionID: shoal.ID(citation.GetRevisionId()),
		SectionID:  shoal.ID(citation.GetSectionId()),
		SpanID:     shoal.ID(citation.GetSpanId()),
		Range:      rangeFromProto(citation.GetRange()),
	}
}

func rangeToProto(sourceRange document.SourceRange) *knowledgepb.SourceRange {
	return &knowledgepb.SourceRange{
		Start: positionToProto(sourceRange.Start),
		End:   positionToProto(sourceRange.End),
	}
}

func rangeFromProto(sourceRange *knowledgepb.SourceRange) document.SourceRange {
	if sourceRange == nil {
		return document.SourceRange{}
	}
	return document.SourceRange{
		Start: positionFromProto(sourceRange.GetStart()),
		End:   positionFromProto(sourceRange.GetEnd()),
	}
}

func positionToProto(position document.SourcePosition) *knowledgepb.SourcePosition {
	return &knowledgepb.SourcePosition{Offset: position.Offset, Page: position.Page}
}

func positionFromProto(position *knowledgepb.SourcePosition) document.SourcePosition {
	if position == nil {
		return document.SourcePosition{}
	}
	return document.SourcePosition{Offset: position.GetOffset(), Page: position.GetPage()}
}

func pathToProto(path graph.Path) *knowledgepb.GraphPath {
	if !pathPresent(path) {
		return nil
	}
	var nodes []*knowledgepb.GraphNode
	if len(path.Nodes) > 0 {
		nodes = make([]*knowledgepb.GraphNode, len(path.Nodes))
		for i, node := range path.Nodes {
			nodes[i] = &knowledgepb.GraphNode{
				Id:         string(node.ID),
				Kind:       node.Kind,
				Labels:     cloneStrings(node.Labels),
				Properties: cloneMetadata(node.Properties),
			}
		}
	}
	var edges []*knowledgepb.GraphEdge
	if len(path.Edges) > 0 {
		edges = make([]*knowledgepb.GraphEdge, len(path.Edges))
		for i, edge := range path.Edges {
			edges[i] = &knowledgepb.GraphEdge{
				Id:         string(edge.ID),
				From:       string(edge.From),
				To:         string(edge.To),
				Type:       edge.Type,
				Weight:     float64(edge.Weight),
				Properties: cloneMetadata(edge.Properties),
			}
		}
	}
	return &knowledgepb.GraphPath{Nodes: nodes, Edges: edges}
}

func pathFromProto(path *knowledgepb.GraphPath) graph.Path {
	if path == nil {
		return graph.Path{}
	}
	var nodes []graph.Node
	if len(path.GetNodes()) > 0 {
		nodes = make([]graph.Node, len(path.GetNodes()))
		for i, node := range path.GetNodes() {
			nodes[i] = graph.Node{
				ID:         shoal.ID(node.GetId()),
				Kind:       node.GetKind(),
				Labels:     cloneStrings(node.GetLabels()),
				Properties: cloneMetadata(node.GetProperties()),
			}
		}
	}
	var edges []graph.Edge
	if len(path.GetEdges()) > 0 {
		edges = make([]graph.Edge, len(path.GetEdges()))
		for i, edge := range path.GetEdges() {
			edges[i] = graph.Edge{
				ID:         shoal.ID(edge.GetId()),
				From:       shoal.ID(edge.GetFrom()),
				To:         shoal.ID(edge.GetTo()),
				Type:       edge.GetType(),
				Weight:     shoal.Score(edge.GetWeight()),
				Properties: cloneMetadata(edge.GetProperties()),
			}
		}
	}
	return graph.Path{Nodes: nodes, Edges: edges}
}

func modeToProto(mode retrieval.Mode) (knowledgepb.RetrievalMode, error) {
	switch mode {
	case retrieval.ModeLexical:
		return knowledgepb.RetrievalMode_RETRIEVAL_MODE_LEXICAL, nil
	case retrieval.ModeVector:
		return knowledgepb.RetrievalMode_RETRIEVAL_MODE_VECTOR, nil
	case retrieval.ModeTree:
		return knowledgepb.RetrievalMode_RETRIEVAL_MODE_TREE, nil
	case retrieval.ModeGraph:
		return knowledgepb.RetrievalMode_RETRIEVAL_MODE_GRAPH, nil
	default:
		return knowledgepb.RetrievalMode_RETRIEVAL_MODE_UNSPECIFIED,
			shoal.NewError(shoal.ErrorInvalidArgument, "unknown retrieval mode")
	}
}

func modeFromProto(mode knowledgepb.RetrievalMode) (retrieval.Mode, error) {
	switch mode {
	case knowledgepb.RetrievalMode_RETRIEVAL_MODE_LEXICAL:
		return retrieval.ModeLexical, nil
	case knowledgepb.RetrievalMode_RETRIEVAL_MODE_VECTOR:
		return retrieval.ModeVector, nil
	case knowledgepb.RetrievalMode_RETRIEVAL_MODE_TREE:
		return retrieval.ModeTree, nil
	case knowledgepb.RetrievalMode_RETRIEVAL_MODE_GRAPH:
		return retrieval.ModeGraph, nil
	default:
		return "", shoal.NewError(shoal.ErrorInvalidArgument, "unknown retrieval mode")
	}
}

func idsToStrings(ids []shoal.ID) []string {
	if len(ids) == 0 {
		return nil
	}
	values := make([]string, len(ids))
	for i, id := range ids {
		values[i] = string(id)
	}
	return values
}

func stringsToIDs(values []string) []shoal.ID {
	if len(values) == 0 {
		return nil
	}
	ids := make([]shoal.ID, len(values))
	for i, value := range values {
		ids[i] = shoal.ID(value)
	}
	return ids
}

func scoresToFloat64(scores map[string]shoal.Score) map[string]float64 {
	if len(scores) == 0 {
		return nil
	}
	values := make(map[string]float64, len(scores))
	for name, score := range scores {
		values[name] = float64(score)
	}
	return values
}

func float64ToScores(values map[string]float64) map[string]shoal.Score {
	if len(values) == 0 {
		return nil
	}
	scores := make(map[string]shoal.Score, len(values))
	for name, value := range values {
		scores[name] = shoal.Score(value)
	}
	return scores
}

func cloneMetadata(metadata map[string]string) map[string]string {
	if len(metadata) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(metadata))
	for key, value := range metadata {
		cloned[key] = value
	}
	return cloned
}

func cloneStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	return append([]string(nil), values...)
}
