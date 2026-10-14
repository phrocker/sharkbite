// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied. See the License for the
// specific language governing permissions and limitations
// under the License.

package interaction

import (
	"context"
	"reflect"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestRequiredVisibilityContextAndSubgraphConjunction(t *testing.T) {
	ctx, err := WithRequiredVisibility(
		context.Background(), []string{"policy:b", "policy:a"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, err = WithRequiredVisibility(ctx, []string{"policy:a", "policy:c"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"policy:a", "policy:b", "policy:c"}
	if got := RequiredVisibility(ctx); !reflect.DeepEqual(got, want) {
		t.Fatalf("required visibility = %v, want %v", got, want)
	}
	subgraph, err := ConjoinSubgraphVisibility(Subgraph{
		Visibility: []string{"source:a"},
		Nodes: []graph.Node{{
			ID: "node", Labels: []string{"interaction"},
			Properties: shoal.Metadata{
				"existing":               "value",
				PropertyVisibilityDigest: "stale",
				PropertyVisibilityCount:  "99",
			},
		}},
		Edges: []graph.Edge{{
			ID: "edge", From: "node", To: "source",
			Type: "derived", Properties: shoal.Metadata{
				PropertyVisibilityDigest: "stale",
				PropertyVisibilityCount:  "99",
			},
		}},
	}, RequiredVisibility(ctx))
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"policy:a", "policy:b", "policy:c", "source:a"}
	if !reflect.DeepEqual(subgraph.Visibility, want) {
		t.Fatalf("subgraph visibility = %v, want %v",
			subgraph.Visibility, want)
	}
	for _, metadata := range []shoal.Metadata{
		subgraph.Nodes[0].Properties,
		subgraph.Edges[0].Properties,
	} {
		if metadata[PropertyVisibility] != Expression(want) {
			t.Fatalf("derived visibility metadata = %#v", metadata)
		}
		if metadata[PropertyVisibilityDigest] != "" ||
			metadata[PropertyVisibilityCount] != "" {
			t.Fatalf("stale visibility metadata = %#v", metadata)
		}
	}
}
