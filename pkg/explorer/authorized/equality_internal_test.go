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
	"testing"

	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestGraphEqualityDistinguishesMissingMetadataFromEmptyValue(t *testing.T) {
	leftNode := graph.Node{
		ID: "node", Kind: "entity", Properties: shoal.Metadata{"left": ""},
	}
	rightNode := graph.Node{
		ID: "node", Kind: "entity", Properties: shoal.Metadata{"right": ""},
	}
	if graphNodesEqual(leftNode, rightNode) {
		t.Fatal("nodes with different empty-valued metadata keys compared equal")
	}

	leftEdge := graph.Edge{
		ID: "edge", From: "from", To: "to", Type: "related", Weight: 1,
		Properties: shoal.Metadata{"left": ""},
	}
	rightEdge := graph.Edge{
		ID: "edge", From: "from", To: "to", Type: "related", Weight: 1,
		Properties: shoal.Metadata{"right": ""},
	}
	if graphEdgesEqual(leftEdge, rightEdge) {
		t.Fatal("edges with different empty-valued metadata keys compared equal")
	}
}
