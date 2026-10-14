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

package graph

const (
	// NodeKindProducer identifies a content-addressed producer configuration.
	NodeKindProducer = "shoal.producer"
	// NodeKindDerivedAssertion reifies a derived assertion so graph edges can
	// point at the assertion without inventing an edge-to-edge graph model.
	NodeKindDerivedAssertion = "shoal.derived_assertion"
	// EdgeTypeProduced connects a producer node to a derived assertion node.
	EdgeTypeProduced = "shoal.produced"
)

// IsProvenanceKind reports whether kind is reserved for Shoal provenance.
func IsProvenanceKind(kind string) bool {
	return kind == NodeKindProducer || kind == NodeKindDerivedAssertion
}

// IsProvenanceEdgeType reports whether edgeType is reserved for Shoal provenance.
func IsProvenanceEdgeType(edgeType string) bool {
	return edgeType == EdgeTypeProduced
}
