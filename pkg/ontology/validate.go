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
	"encoding/binary"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func invalid(message string) error {
	return shoal.NewError(shoal.ErrorInvalidArgument, message)
}

func requiredWire(value string) bool {
	return value != "" && utf8.ValidString(value) && strings.TrimSpace(value) == value
}

func optionalWire(value string) bool {
	return value == "" || requiredWire(value)
}

func validOpaqueWire(value string) bool {
	return utf8.ValidString(value)
}

func validateReference(value shoal.ID, name string) error {
	if !requiredWire(string(value)) {
		return invalid(name + " is required and must be valid UTF-8")
	}
	return nil
}

func validateMetadata(metadata shoal.Metadata) error {
	for key, value := range metadata {
		if !requiredWire(key) {
			return invalid("metadata keys must be non-empty canonical UTF-8")
		}
		if !utf8.ValidString(value) {
			return invalid("metadata values must be valid UTF-8")
		}
	}
	return nil
}

func cloneMetadata(metadata shoal.Metadata) shoal.Metadata {
	if metadata == nil {
		return nil
	}
	cloned := make(shoal.Metadata, len(metadata))
	for key, value := range metadata {
		cloned[key] = value
	}
	return cloned
}

func canonicalMetadata(metadata shoal.Metadata) string {
	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys)*2)
	for _, key := range keys {
		parts = append(parts, key, metadata[key])
	}
	return canonicalParts(parts...)
}

func canonicalParts(parts ...string) string {
	var canonical strings.Builder
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(parts)))
	_, _ = canonical.Write(length[:])
	for _, part := range parts {
		binary.BigEndian.PutUint64(length[:], uint64(len(part)))
		_, _ = canonical.Write(length[:])
		canonical.WriteString(part)
	}
	return canonical.String()
}

func canonicalOptional(present bool, value string) string {
	if !present {
		return canonicalParts("0")
	}
	return canonicalParts("1", value)
}

func canonicalFloat(value float64) string {
	return strconv.FormatFloat(value, 'g', -1, 64)
}

func validateFinite(value float64, name string) error {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return invalid(name + " must be finite")
	}
	return nil
}

func normalizeTime(value time.Time) time.Time {
	if value.IsZero() {
		return value
	}
	return value.UTC().Round(0)
}

func validateTime(value time.Time, name string) error {
	if value.IsZero() {
		return invalid(name + " is required")
	}
	normalized := normalizeTime(value)
	year := normalized.Year()
	if year < 1 || year > 9999 || value != normalized {
		return invalid(name + " must be a canonical UTC timestamp")
	}
	return nil
}

func canonicalTime(value time.Time) string {
	return value.Format(time.RFC3339Nano)
}

func cloneIDs(values []shoal.ID) []shoal.ID {
	return append([]shoal.ID(nil), values...)
}

func canonicalizeIDs(values []shoal.ID) []shoal.ID {
	cloned := cloneIDs(values)
	sort.Slice(cloned, func(left, right int) bool {
		return string(cloned[left]) < string(cloned[right])
	})
	return cloned
}

func validateCanonicalIDs(
	values []shoal.ID, namespace, name string, requireNonEmpty bool,
) error {
	if requireNonEmpty && len(values) == 0 {
		return invalid(name + " cannot be empty")
	}
	for index, id := range values {
		if err := validateTypedID(id, namespace); err != nil {
			return err
		}
		if index > 0 && string(values[index-1]) >= string(id) {
			return invalid(name + " must be unique and canonically ordered")
		}
	}
	return nil
}

func canonicalIDs(values []shoal.ID) string {
	parts := make([]string, len(values))
	for index, id := range values {
		parts[index] = string(id)
	}
	return canonicalParts(parts...)
}
