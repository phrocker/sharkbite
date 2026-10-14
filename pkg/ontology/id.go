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
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"strings"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

var reservedIDNamespaces = map[string]struct{}{
	"assertion":        {},
	"concept":          {},
	"evidence":         {},
	"extraction":       {},
	"morphism":         {},
	"ontology-version": {},
	"property":         {},
	"proposal":         {},
	"relationship":     {},
	"schema":           {},
}

// NewStableID derives an extension ID from length-delimited identity
// components. Namespaces used by typed ontology constructors are reserved.
func NewStableID(namespace string, parts ...string) (shoal.ID, error) {
	if _, reserved := reservedIDNamespaces[namespace]; reserved {
		return "", invalid("ID namespace is reserved for a typed constructor")
	}
	if len(parts) == 0 {
		return "", invalid("stable ID requires an identity component")
	}
	for _, part := range parts {
		if !utf8.ValidString(part) {
			return "", invalid("stable ID components must be valid UTF-8")
		}
	}
	return deriveID(namespace, parts...)
}

// ParseID parses a canonical serialized ID.
func ParseID(value string) (shoal.ID, error) {
	id := shoal.ID(value)
	if err := ValidateID(id); err != nil {
		return "", err
	}
	return id, nil
}

// ValidateID checks the canonical namespace and lowercase SHA-256 digest.
func ValidateID(id shoal.ID) error {
	value := string(id)
	if !utf8.ValidString(value) || strings.Count(value, ":") != 1 {
		return invalid("invalid ontology ID")
	}
	namespace, digest, _ := strings.Cut(value, ":")
	if !validNamespace(namespace) || len(digest) != sha256.Size*2 {
		return invalid("invalid ontology ID")
	}
	decoded, err := hex.DecodeString(digest)
	if err != nil || hex.EncodeToString(decoded) != digest {
		return invalid("invalid ontology ID")
	}
	return nil
}

// IDNamespace returns the ID namespace, or an empty string for an invalid ID.
func IDNamespace(id shoal.ID) string {
	if err := ValidateID(id); err != nil {
		return ""
	}
	namespace, _, found := strings.Cut(string(id), ":")
	if !found {
		return ""
	}
	return namespace
}

func deriveID(namespace string, parts ...string) (shoal.ID, error) {
	if !validNamespace(namespace) {
		return "", invalid("invalid ID namespace")
	}
	if len(parts) == 0 {
		return "", invalid("stable ID requires an identity component")
	}

	digest := sha256.New()
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(parts)))
	_, _ = digest.Write(length[:])
	for _, part := range parts {
		binary.BigEndian.PutUint64(length[:], uint64(len(part)))
		_, _ = digest.Write(length[:])
		_, _ = digest.Write([]byte(part))
	}
	return shoal.ID(namespace + ":" + hex.EncodeToString(digest.Sum(nil))), nil
}

func validateTypedID(id shoal.ID, namespace string) error {
	if err := ValidateID(id); err != nil {
		return err
	}
	if IDNamespace(id) != namespace {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "ontology ID has an unexpected namespace")
	}
	return nil
}

func validNamespace(namespace string) bool {
	if namespace == "" {
		return false
	}
	for index, character := range namespace {
		if character >= 'a' && character <= 'z' {
			continue
		}
		if index > 0 && ((character >= '0' && character <= '9') ||
			character == '.' || character == '_' || character == '-') {
			continue
		}
		return false
	}
	return true
}
