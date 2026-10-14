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

package webapi

import (
	"testing"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func sampleDerivationDetail() DerivationDetail {
	return DerivationDetail{
		AssertionID:           shoal.ID("assertion:sample"),
		DerivationID:          shoal.ID("derivation:sample"),
		Origin:                "derived",
		Score:                 0.91,
		EmbeddingModel:        "unit-embed",
		EmbeddingModelVersion: "v1",
		SimilarityMetric:      "cosine",
		Threshold:             0.85,
		TessellationCell:      "cell-a",
		IteratorName:          "latent-similar-to",
		IteratorOptions: shoal.Metadata{
			"similarityThreshold": "0.85",
			"maxPairsPerCell":     "128",
			"maxCellBuffer":       "4096",
			"edgeCF":              "link",
			"embeddingCF":         "embedding",
			"embeddingCQ":         "vector",
		},
		Provider:     "unit-provider",
		Model:        "unit-model",
		ModelVersion: "v2",
	}
}

// TestRecomputeDigestIsStableAcrossCalls pins the determinism guarantee that the
// whole Recompute feature rests on: the digest must not depend on Go's
// randomized map iteration order over iterator options. Removing the sort in
// canonicalDerivationMetadata makes this fail with overwhelming probability
// because the six-key map is re-serialized on every call.
func TestRecomputeDigestIsStableAcrossCalls(t *testing.T) {
	detail := sampleDerivationDetail()
	if len(detail.IteratorOptions) < 3 {
		t.Fatalf("fixture needs at least three iterator options to exercise ordering")
	}
	first := derivationDigest(detail)
	for iteration := 0; iteration < 512; iteration++ {
		if got := derivationDigest(detail); got != first {
			t.Fatalf("digest changed across calls at iteration %d: %q vs %q",
				iteration, got, first)
		}
	}
}

// TestRecomputeDigestChangesWithInputs pins that every derivation input the
// inspector reports is folded into the digest, so a reviewer who deletes any
// writeField line is caught: the mutated detail must produce a different digest.
func TestRecomputeDigestChangesWithInputs(t *testing.T) {
	baseline := derivationDigest(sampleDerivationDetail())
	mutations := map[string]func(*DerivationDetail){
		"assertion_id":            func(d *DerivationDetail) { d.AssertionID = shoal.ID("assertion:other") },
		"derivation_id":           func(d *DerivationDetail) { d.DerivationID = shoal.ID("derivation:other") },
		"origin":                  func(d *DerivationDetail) { d.Origin = "asserted" },
		"score":                   func(d *DerivationDetail) { d.Score = 0.92 },
		"embedding_model":         func(d *DerivationDetail) { d.EmbeddingModel = "other-embed" },
		"embedding_model_version": func(d *DerivationDetail) { d.EmbeddingModelVersion = "v9" },
		"similarity_metric":       func(d *DerivationDetail) { d.SimilarityMetric = "dot" },
		"threshold":               func(d *DerivationDetail) { d.Threshold = 0.5 },
		"tessellation_cell":       func(d *DerivationDetail) { d.TessellationCell = "cell-b" },
		"iterator_name":           func(d *DerivationDetail) { d.IteratorName = "other-iterator" },
		"provider":                func(d *DerivationDetail) { d.Provider = "other-provider" },
		"model":                   func(d *DerivationDetail) { d.Model = "other-model" },
		"model_version":           func(d *DerivationDetail) { d.ModelVersion = "v9" },
		"iterator_option_value":   func(d *DerivationDetail) { d.IteratorOptions["edgeCF"] = "other" },
		"iterator_option_key":     func(d *DerivationDetail) { d.IteratorOptions["extraKey"] = "1" },
	}
	for name, mutate := range mutations {
		detail := sampleDerivationDetail()
		mutate(&detail)
		if got := derivationDigest(detail); got == baseline {
			t.Fatalf("mutating %s did not change the digest", name)
		}
	}
}

// TestRecomputeDigestSeparatesMetadataBoundaries pins the length prefixes in
// canonicalDerivationMetadata: iterator option keys and values are dynamic, so
// without them a delimiter buried inside a value could impersonate the boundary
// between one option and the next. These two option maps share the same naive
// "key=value|" concatenation and must still fold to different digests.
func TestRecomputeDigestSeparatesMetadataBoundaries(t *testing.T) {
	left := sampleDerivationDetail()
	left.IteratorOptions = shoal.Metadata{"a": "b|c", "d": "e"}
	right := sampleDerivationDetail()
	right.IteratorOptions = shoal.Metadata{"a": "b", "c|d": "e"}
	if derivationDigest(left) == derivationDigest(right) {
		t.Fatalf("iterator option maps collided across an unprefixed boundary")
	}
}
