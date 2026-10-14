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

package graph_test

import (
	"math"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestPathRequiresDirectedConnectedEdges(t *testing.T) {
	valid := graph.Path{
		Nodes: []graph.Node{{ID: "a"}, {ID: "b"}},
		Edges: []graph.Edge{{ID: "edge-1", From: "a", To: "b", Type: "supports"}},
	}

	if err := valid.Validate(); err != nil {
		t.Fatalf("expected valid path: %v", err)
	}

	reverse := valid
	reverse.Edges = []graph.Edge{{ID: "edge-1", From: "b", To: "a", Type: "supports"}}
	if err := reverse.Validate(); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("reverse path error = %v", err)
	}

	mixed := graph.Path{
		Nodes: []graph.Node{{ID: "b"}, {ID: "a"}, {ID: "c"}},
		Edges: []graph.Edge{
			{ID: "ab", From: "a", To: "b", Type: "links"},
			{ID: "ac", From: "a", To: "c", Type: "links"},
		},
	}
	if err := mixed.Validate(); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("B<-A->C path error = %v", err)
	}
}

func TestEdgeRejectsBlankType(t *testing.T) {
	edge := graph.Edge{ID: "edge", From: "a", To: "b", Type: " \t "}
	if err := edge.Validate(); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("blank type error = %v", err)
	}
}

func TestPathAllowsDirectedCyclesSelfAndParallelEdges(t *testing.T) {
	cycle := graph.Path{
		Nodes: []graph.Node{{ID: "a"}, {ID: "b"}, {ID: "a"}, {ID: "a"}},
		Edges: []graph.Edge{
			{ID: "ab-1", From: "a", To: "b", Type: "links"},
			{ID: "ba", From: "b", To: "a", Type: "links"},
			{ID: "aa", From: "a", To: "a", Type: "links"},
		},
	}
	if err := cycle.Validate(); err != nil {
		t.Fatalf("directed cycle/self-edge rejected: %v", err)
	}
	parallelChoice := graph.Path{
		Nodes: []graph.Node{{ID: "a"}, {ID: "b"}},
		Edges: []graph.Edge{{ID: "ab-2", From: "a", To: "b", Type: "links"}},
	}
	if err := parallelChoice.Validate(); err != nil {
		t.Fatalf("parallel edge choice rejected: %v", err)
	}
}

func TestGraphValidationBoundsAndFiniteWeights(t *testing.T) {
	if err := (graph.Node{ID: "node"}).Validate(); err != nil {
		t.Fatalf("optional node kind rejected: %v", err)
	}
	if err := (graph.Edge{
		ID: "edge", From: "node", To: "node", Type: "self",
		Weight: shoal.Score(math.NaN()),
	}).Validate(); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("non-finite edge error = %v", err)
	}
}
