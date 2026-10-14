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

// Package code defines parser-neutral source-code and AST ingestion contracts.
package code

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"strings"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// ID is an opaque canonical stable identifier.
type ID struct {
	value string
}

var reservedIDNamespaces = map[string]struct{}{
	"source":       {},
	"syntax":       {},
	"symbol":       {},
	"external":     {},
	"relationship": {},
	"ingest":       {},
	"parse-result": {},
}

// NewStableID deterministically derives an extension ID from length-delimited
// identity components. Namespaces owned by this package are reserved and can
// only be produced by their typed constructors.
func NewStableID(namespace string, parts ...string) (ID, error) {
	if _, reserved := reservedIDNamespaces[namespace]; reserved {
		return ID{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "ID namespace is reserved for a typed constructor")
	}
	return deriveID(namespace, parts...)
}

// ParseID parses a serialized canonical ID. Typed constructors do not accept
// parsed IDs, so parsing cannot override an entity's derived identity.
func ParseID(value string) (ID, error) {
	id := ID{value: value}
	if err := id.Validate(); err != nil {
		return ID{}, err
	}
	return id, nil
}

// Validate checks the canonical namespace and SHA-256 digest representation.
func (id ID) Validate() error {
	if strings.Count(id.value, ":") != 1 {
		return shoal.NewError(shoal.ErrorInvalidArgument, "invalid stable ID")
	}
	namespace, digest, _ := strings.Cut(id.value, ":")
	if !validNamespace(namespace) || len(digest) != sha256.Size*2 {
		return shoal.NewError(shoal.ErrorInvalidArgument, "invalid stable ID")
	}
	decoded, err := hex.DecodeString(digest)
	if err != nil || hex.EncodeToString(decoded) != digest {
		return shoal.NewError(shoal.ErrorInvalidArgument, "invalid stable ID")
	}
	return nil
}

// Namespace returns the ID's namespace, or an empty string for an invalid ID.
func (id ID) Namespace() string {
	namespace, _, found := strings.Cut(id.value, ":")
	if !found {
		return ""
	}
	return namespace
}

func (id ID) String() string {
	return id.value
}

func deriveID(namespace string, parts ...string) (ID, error) {
	if !validNamespace(namespace) {
		return ID{}, shoal.NewError(shoal.ErrorInvalidArgument, "invalid ID namespace")
	}
	if len(parts) == 0 {
		return ID{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "stable ID requires an identity component")
	}

	digest := sha256.New()
	var length [8]byte
	for _, part := range parts {
		binary.BigEndian.PutUint64(length[:], uint64(len(part)))
		_, _ = digest.Write(length[:])
		_, _ = digest.Write([]byte(part))
	}
	return ID{value: namespace + ":" + hex.EncodeToString(digest.Sum(nil))}, nil
}

func validNamespace(namespace string) bool {
	if namespace == "" {
		return false
	}
	for i, character := range namespace {
		if character >= 'a' && character <= 'z' {
			continue
		}
		if i > 0 && ((character >= '0' && character <= '9') ||
			character == '.' || character == '_' || character == '-') {
			continue
		}
		return false
	}
	return true
}
