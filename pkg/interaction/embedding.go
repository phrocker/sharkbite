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

package interaction

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const MaxEmbeddingSpaceIdentities = 1024

// EmbeddingSpaceSet pins the stable full identities of every embedding space
// consulted by one interaction. Digest is derived from the sorted unique set.
// Process-keyed authorization-report pseudonyms are not stable identities and
// must never be stored here.
type EmbeddingSpaceSet struct {
	Identities []string
	Digest     string
}

// NewEmbeddingSpaceSet validates, sorts, deduplicates, and hashes stable
// embedding-space identities reported by the core query observer.
func NewEmbeddingSpaceSet(identities []string) (EmbeddingSpaceSet, error) {
	normalized, err := normalizeEmbeddingSpaceIdentities(identities)
	if err != nil {
		return EmbeddingSpaceSet{}, err
	}
	if len(normalized) == 0 {
		return EmbeddingSpaceSet{}, nil
	}
	hash := sha256.New()
	var length [8]byte
	write := func(value string) {
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(value))
	}
	write("shoal-interaction-embedding-space-set-v1")
	for _, identity := range normalized {
		write(identity)
	}
	return EmbeddingSpaceSet{
		Identities: normalized,
		Digest:     hex.EncodeToString(hash.Sum(nil)),
	}, nil
}

func (s EmbeddingSpaceSet) Validate() error {
	expected, err := NewEmbeddingSpaceSet(s.Identities)
	if err != nil {
		return err
	}
	if len(expected.Identities) == 0 {
		if s.Digest != "" {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"empty embedding space set cannot carry a digest",
			)
		}
		return nil
	}
	if err := validateDigest(
		"interaction embedding space set digest", s.Digest, false,
	); err != nil {
		return err
	}
	if s.Digest != expected.Digest {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"interaction embedding space set digest is not canonical",
		)
	}
	return nil
}

func (s EmbeddingSpaceSet) Canonical() (EmbeddingSpaceSet, error) {
	if err := s.Validate(); err != nil {
		return EmbeddingSpaceSet{}, err
	}
	return NewEmbeddingSpaceSet(s.Identities)
}

func normalizeEmbeddingSpaceIdentities(
	identities []string,
) ([]string, error) {
	if len(identities) > MaxEmbeddingSpaceIdentities {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"interaction embedding space set exceeds the public bound",
		)
	}
	normalized := append([]string(nil), identities...)
	for _, identity := range normalized {
		if !utf8.ValidString(identity) ||
			strings.TrimSpace(identity) != identity ||
			identity == "" {
			return nil, shoal.NewError(
				shoal.ErrorInvalidArgument,
				"interaction embedding space identity is invalid",
			)
		}
		if err := shoal.ValidateSemanticString(
			"interaction embedding space identity", identity,
		); err != nil {
			return nil, err
		}
	}
	sort.Strings(normalized)
	unique := normalized[:0]
	for _, identity := range normalized {
		if len(unique) > 0 && unique[len(unique)-1] == identity {
			continue
		}
		unique = append(unique, identity)
	}
	return unique, nil
}
