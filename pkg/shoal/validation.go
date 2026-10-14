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

package shoal

import (
	"bytes"
	"fmt"
	"math"
)

const (
	// MaxIDBytes is the public hard maximum for an opaque identifier.
	MaxIDBytes = 1024

	// MaxMetadataEntries is the maximum number of entries on one object.
	MaxMetadataEntries = 256
	// MaxMetadataKeyBytes is the maximum encoded key length.
	MaxMetadataKeyBytes = 256
	// MaxMetadataValueBytes is the maximum encoded value length.
	MaxMetadataValueBytes = 4 * 1024
	// MaxMetadataBytes is the maximum combined key and value length.
	MaxMetadataBytes = 256 * 1024

	// MaxSemanticStringBytes bounds kinds, types, titles, headings, and source
	// versions unless a more specific public contract applies.
	MaxSemanticStringBytes = 4 * 1024
)

// CompareID compares IDs lexicographically as unsigned raw bytes.
func CompareID(left, right ID) int {
	return bytes.Compare([]byte(left), []byte(right))
}

// ValidateRequiredID checks that an opaque ID is present and bounded without
// changing or interpreting its bytes.
func ValidateRequiredID(name string, id ID) error {
	if len(id) == 0 {
		return NewError(ErrorInvalidArgument, name+" is required")
	}
	return ValidateOptionalID(name, id)
}

// ValidateOptionalID checks the byte bound of an optional opaque ID.
func ValidateOptionalID(name string, id ID) error {
	if len(id) > MaxIDBytes {
		return NewError(
			ErrorInvalidArgument,
			fmt.Sprintf("%s cannot exceed %d bytes", name, MaxIDBytes),
		)
	}
	return nil
}

// ValidateFiniteScore checks that a public score or weight is finite.
func ValidateFiniteScore(name string, score Score) error {
	if math.IsNaN(float64(score)) || math.IsInf(float64(score), 0) {
		return NewError(ErrorInvalidArgument, name+" must be finite")
	}
	return nil
}

// ValidateMetadata checks public metadata size bounds without interpreting or
// normalizing keys and values.
func ValidateMetadata(name string, metadata Metadata) error {
	if len(metadata) > MaxMetadataEntries {
		return NewError(
			ErrorInvalidArgument,
			fmt.Sprintf("%s cannot exceed %d entries", name, MaxMetadataEntries),
		)
	}
	total := 0
	for key, value := range metadata {
		if len(key) > MaxMetadataKeyBytes {
			return NewError(
				ErrorInvalidArgument,
				fmt.Sprintf("%s key cannot exceed %d bytes", name, MaxMetadataKeyBytes),
			)
		}
		if len(value) > MaxMetadataValueBytes {
			return NewError(
				ErrorInvalidArgument,
				fmt.Sprintf("%s value cannot exceed %d bytes", name, MaxMetadataValueBytes),
			)
		}
		total += len(key) + len(value)
		if total > MaxMetadataBytes {
			return NewError(
				ErrorInvalidArgument,
				fmt.Sprintf("%s cannot exceed %d bytes", name, MaxMetadataBytes),
			)
		}
	}
	return nil
}

// ValidateSemanticString checks a bounded semantic string. Empty values are
// accepted when the containing contract makes the field optional.
func ValidateSemanticString(name, value string) error {
	if len(value) > MaxSemanticStringBytes {
		return NewError(
			ErrorInvalidArgument,
			fmt.Sprintf("%s cannot exceed %d bytes", name, MaxSemanticStringBytes),
		)
	}
	return nil
}
