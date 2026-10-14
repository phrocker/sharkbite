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

package interaction

import (
	"context"

	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type requiredVisibilityContextKey struct{}

// WithRequiredVisibility adds trusted output labels that every derived
// interaction record created under ctx must carry.
func WithRequiredVisibility(
	ctx context.Context,
	labels []string,
) (context.Context, error) {
	if ctx == nil {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "context is required")
	}
	combined, err := Conjoin(RequiredVisibility(ctx), labels)
	if err != nil {
		return nil, err
	}
	if len(combined) == 0 {
		return ctx, nil
	}
	return context.WithValue(
		ctx, requiredVisibilityContextKey{}, combined), nil
}

// RequiredVisibility returns an independent copy of trusted output labels
// attached to ctx.
func RequiredVisibility(ctx context.Context) []string {
	if ctx == nil {
		return nil
	}
	labels, _ := ctx.Value(
		requiredVisibilityContextKey{}).([]string)
	return append([]string(nil), labels...)
}

// ConjoinSubgraphVisibility applies additional trusted output labels to a
// complete interaction subgraph and every derived node and edge it contains.
func ConjoinSubgraphVisibility(
	subgraph Subgraph,
	additional []string,
) (Subgraph, error) {
	visibility, err := Conjoin(subgraph.Visibility, additional)
	if err != nil {
		return Subgraph{}, err
	}
	result := subgraph
	result.Visibility = visibility
	result.Nodes = make([]graph.Node, len(subgraph.Nodes))
	for index, node := range subgraph.Nodes {
		result.Nodes[index] = node
		result.Nodes[index].Labels = append([]string(nil), node.Labels...)
		result.Nodes[index].Properties = cloneVisibilityMetadata(node.Properties)
		clearVisibilityMetadata(result.Nodes[index].Properties)
		setVisibility(result.Nodes[index].Properties, visibility)
	}
	result.Edges = make([]graph.Edge, len(subgraph.Edges))
	for index, edge := range subgraph.Edges {
		result.Edges[index] = edge
		result.Edges[index].Properties = cloneVisibilityMetadata(edge.Properties)
		clearVisibilityMetadata(result.Edges[index].Properties)
		setVisibility(result.Edges[index].Properties, visibility)
	}
	return result, nil
}

func clearVisibilityMetadata(metadata shoal.Metadata) {
	delete(metadata, PropertyVisibility)
	delete(metadata, PropertyVisibilityDigest)
	delete(metadata, PropertyVisibilityCount)
}

func cloneVisibilityMetadata(metadata shoal.Metadata) shoal.Metadata {
	cloned := make(shoal.Metadata, len(metadata)+1)
	for key, value := range metadata {
		cloned[key] = value
	}
	return cloned
}
