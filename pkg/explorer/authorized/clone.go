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
	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func cloneMetadata(metadata shoal.Metadata) shoal.Metadata {
	if metadata == nil {
		return nil
	}
	cloned := make(shoal.Metadata, len(metadata))
	for key, value := range metadata {
		cloned[key] = value
	}
	return cloned
}

func cloneSource(source explorer.Source) explorer.Source {
	source.Metadata = cloneMetadata(source.Metadata)
	return source
}

func cloneDocumentSummary(summary explorer.DocumentSummary) explorer.DocumentSummary {
	summary.Document = cloneDocument(summary.Document)
	summary.Revision = cloneRevision(summary.Revision)
	return summary
}

func cloneDocumentView(view explorer.DocumentView) explorer.DocumentView {
	view.Document = cloneDocument(view.Document)
	view.Revision = cloneRevision(view.Revision)
	view.Root = cloneSectionView(view.Root)
	return view
}

func cloneSectionView(view explorer.SectionView) explorer.SectionView {
	view.Section = cloneSection(view.Section)
	view.Spans = append([]document.Span(nil), view.Spans...)
	for index := range view.Spans {
		view.Spans[index] = cloneSpan(view.Spans[index])
	}
	view.Children = append([]explorer.SectionView(nil), view.Children...)
	for index := range view.Children {
		view.Children[index] = cloneSectionView(view.Children[index])
	}
	return view
}

func cloneDocument(value document.Document) document.Document {
	value.Metadata = cloneMetadata(value.Metadata)
	return value
}

func cloneRevision(value document.Revision) document.Revision {
	value.Metadata = cloneMetadata(value.Metadata)
	return value
}

func cloneSection(value document.Section) document.Section {
	value.Metadata = cloneMetadata(value.Metadata)
	return value
}

func cloneSpan(value document.Span) document.Span {
	value.Metadata = cloneMetadata(value.Metadata)
	return value
}

func cloneGraphNode(node graph.Node) graph.Node {
	node.Labels = append([]string(nil), node.Labels...)
	node.Properties = cloneMetadata(node.Properties)
	return node
}

func cloneRetrievalResponse(response retrieval.Response) retrieval.Response {
	response.EmbeddingSpaceIDs = append(
		[]shoal.ID(nil), response.EmbeddingSpaceIDs...)
	response.Results = append([]retrieval.Result(nil), response.Results...)
	for resultIndex := range response.Results {
		result := &response.Results[resultIndex]
		result.Evidence = append([]retrieval.Evidence(nil), result.Evidence...)
		for evidenceIndex := range result.Evidence {
			evidence := &result.Evidence[evidenceIndex]
			evidence.Path.Nodes = append([]graph.Node(nil), evidence.Path.Nodes...)
			for nodeIndex := range evidence.Path.Nodes {
				evidence.Path.Nodes[nodeIndex] =
					cloneGraphNode(evidence.Path.Nodes[nodeIndex])
			}
			evidence.Path.Edges = append([]graph.Edge(nil), evidence.Path.Edges...)
			for edgeIndex := range evidence.Path.Edges {
				evidence.Path.Edges[edgeIndex] =
					cloneGraphEdge(evidence.Path.Edges[edgeIndex])
			}
		}
		if result.Explanation != nil {
			explanation := *result.Explanation
			explanation.Modes = append(
				[]retrieval.Mode(nil), explanation.Modes...)
			if explanation.Scores != nil {
				explanation.Scores = make(
					map[string]shoal.Score, len(result.Explanation.Scores))
				for name, score := range result.Explanation.Scores {
					explanation.Scores[name] = score
				}
			}
			result.Explanation = &explanation
		}
	}
	return response
}
