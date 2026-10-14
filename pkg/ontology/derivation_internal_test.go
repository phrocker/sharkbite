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

package ontology

import (
	"testing"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestAssertionDerivationCloneDoesNotAliasOptions(t *testing.T) {
	derivation, err := NewAssertionDerivation(
		"text-embedding-3-large",
		"2026-08-01",
		"cosine",
		0.8,
		"cell:17",
		0.91,
		"entity:person-1",
		"entity:project-1",
		"LatentEdgeDiscoveryIterator",
		shoal.Metadata{"maxPairs": "512"},
	)
	if err != nil {
		t.Fatal(err)
	}

	cloned := derivation.clone()
	cloned.iteratorOptions["maxPairs"] = "1"

	if got := derivation.iteratorOptions["maxPairs"]; got != "512" {
		t.Fatalf("clone shared internal options map: maxPairs=%q, want 512", got)
	}
}
