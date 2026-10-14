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

package shoal_test

import (
	"math"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestCompareIDUsesUnsignedRawBytes(t *testing.T) {
	if shoal.CompareID(shoal.ID("\x00z"), shoal.ID("\x01")) >= 0 {
		t.Fatal("NUL-containing ID did not retain raw byte order")
	}
	if shoal.CompareID(shoal.ID("\x80"), shoal.ID("\xff")) >= 0 {
		t.Fatal("non-UTF-8 ID did not use unsigned byte order")
	}
	if shoal.CompareID("same", "same") != 0 {
		t.Fatal("equal IDs did not compare equal")
	}
}

func TestSharedValidationPreservesOpaqueValues(t *testing.T) {
	id := shoal.ID("a\x00\xff")
	if err := shoal.ValidateRequiredID("test ID", id); err != nil {
		t.Fatalf("opaque ID rejected: %v", err)
	}
	metadata := shoal.Metadata{"\xff": "\x00"}
	if err := shoal.ValidateMetadata("test metadata", metadata); err != nil {
		t.Fatalf("opaque metadata rejected: %v", err)
	}
	if string(id) != "a\x00\xff" || metadata["\xff"] != "\x00" {
		t.Fatal("validation changed opaque bytes")
	}
}

func TestSharedValidationBoundsAndFiniteScores(t *testing.T) {
	if err := shoal.ValidateRequiredID("test ID", ""); !shoal.IsErrorCode(
		err, shoal.ErrorInvalidArgument,
	) {
		t.Fatalf("empty ID error = %v", err)
	}
	if err := shoal.ValidateRequiredID(
		"test ID", shoal.ID(strings.Repeat("x", shoal.MaxIDBytes+1)),
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("oversized ID error = %v", err)
	}
	if err := shoal.ValidateFiniteScore(
		"test score", shoal.Score(math.NaN()),
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("non-finite score error = %v", err)
	}
	if err := shoal.ValidateMetadata("test metadata", shoal.Metadata{
		"key": strings.Repeat("x", shoal.MaxMetadataValueBytes+1),
	}); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("oversized metadata error = %v", err)
	}
}
