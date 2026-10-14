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

// Package graph defines Shoal's public knowledge graph contract.
package graph

import (
	"strings"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	MaxNodeLabels     = 64
	MaxNodeLabelBytes = 256
)

// Node is a schema-neutral knowledge graph node.
type Node struct {
	ID         shoal.ID
	Kind       string
	Labels     []string
	Properties shoal.Metadata
}

// Validate checks node identity, labels, and public static bounds. Kind is
// optional. Cycles and other graph topology are not node-level concerns.
func (n Node) Validate() error {
	if err := shoal.ValidateRequiredID("graph node ID", n.ID); err != nil {
		return err
	}
	if err := shoal.ValidateSemanticString("graph node kind", n.Kind); err != nil {
		return err
	}
	if len(n.Labels) > MaxNodeLabels {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "graph node has too many labels")
	}
	for _, label := range n.Labels {
		if len(label) == 0 {
			return shoal.NewError(
				shoal.ErrorInvalidArgument, "graph node labels cannot be empty")
		}
		if len(label) > MaxNodeLabelBytes {
			return shoal.NewError(
				shoal.ErrorInvalidArgument, "graph node label exceeds the public byte bound")
		}
	}
	return shoal.ValidateMetadata("graph node properties", n.Properties)
}

// Edge is a directed, typed relationship between two nodes.
type Edge struct {
	ID         shoal.ID
	From       shoal.ID
	To         shoal.ID
	Type       string
	Weight     shoal.Score
	Properties shoal.Metadata
}

// Validate checks edge identity, directed endpoints, type, weight, and public
// static bounds. Self-edges and parallel edges are valid.
func (e Edge) Validate() error {
	for name, id := range map[string]shoal.ID{
		"graph edge ID":   e.ID,
		"graph edge from": e.From,
		"graph edge to":   e.To,
	} {
		if err := shoal.ValidateRequiredID(name, id); err != nil {
			return err
		}
	}
	if strings.TrimSpace(e.Type) == "" {
		return shoal.NewError(shoal.ErrorInvalidArgument, "graph edge type is required")
	}
	if err := shoal.ValidateSemanticString("graph edge type", e.Type); err != nil {
		return err
	}
	if err := shoal.ValidateFiniteScore("graph edge weight", e.Weight); err != nil {
		return err
	}
	return shoal.ValidateMetadata("graph edge properties", e.Properties)
}

// Path is an ordered directed graph explanation. Edges[i] connects
// Nodes[i] to Nodes[i+1] from left to right.
type Path struct {
	Nodes []Node
	Edges []Edge
}

// Validate checks that a path is directed, connected, and structurally
// complete. Directed cycles, repeated nodes, self-edges, and parallel edges
// are valid.
func (p Path) Validate() error {
	if len(p.Nodes) == 0 {
		return shoal.NewError(shoal.ErrorInvalidArgument, "graph path requires a node")
	}
	for _, node := range p.Nodes {
		if err := node.Validate(); err != nil {
			return err
		}
	}
	if len(p.Edges) != len(p.Nodes)-1 {
		return shoal.NewError(shoal.ErrorInvalidArgument, "graph path has inconsistent edges")
	}
	for i, edge := range p.Edges {
		if err := edge.Validate(); err != nil {
			return err
		}
		if edge.From != p.Nodes[i].ID || edge.To != p.Nodes[i+1].ID {
			return shoal.NewError(
				shoal.ErrorInvalidArgument, "graph path is not directed and connected")
		}
	}
	return nil
}
