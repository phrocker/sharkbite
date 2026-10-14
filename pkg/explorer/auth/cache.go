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

package auth

import (
	"bytes"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	// MaxCacheDimensions bounds additional limit and index-generation maps.
	MaxCacheDimensions = 64
)

// CacheKeyConfig supplies every partition dimension for an authorized
// projection. Result polarity is deliberately absent so positive and negative
// entries use the same partition key.
type CacheKeyConfig struct {
	Decision            Decision
	AuthorizationDomain []byte
	PolicyCopyPin       []byte
	SnapshotFrontier    uint64
	HistoryFloor        uint64
	RetentionGeneration int64
	Request             retrieval.Request
	Limits              map[string]uint64
	IndexGenerations    map[string][]byte
}

// CacheKey is an immutable, non-disclosing authorized-projection cache key.
// Raw query, scope IDs, labels, and generation handles are hashed and discarded
// during construction.
type CacheKey struct {
	domainFingerprint        Fingerprint
	authorizationFingerprint Fingerprint
	policyGeneration         int64
	policyCopyPinDigest      Digest
	snapshotFrontier         uint64
	historyFloor             uint64
	retentionGeneration      int64
	requestDigest            Digest
	limitsDigest             Digest
	indexGenerationsDigest   Digest
	ontologySchemaDigest     Digest
	ontologyVersionDigest    Digest
	digest                   Digest
	set                      bool
}

// NewCacheKey validates and clones its inputs, normalizes the public retrieval
// request, and deterministically hashes every partition dimension.
func NewCacheKey(config CacheKeyConfig) (CacheKey, error) {
	decision, err := config.Decision.cloneValidated()
	if err != nil {
		return CacheKey{}, err
	}
	if err := validatePolicyComponent(
		"authorization domain", config.AuthorizationDomain, true,
	); err != nil {
		return CacheKey{}, err
	}
	if !bytes.Equal(decision.domain, config.AuthorizationDomain) {
		return CacheKey{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "cache authorization domain does not match decision")
	}
	if len(config.PolicyCopyPin) == 0 || len(config.PolicyCopyPin) > shoal.MaxIDBytes {
		return CacheKey{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "policy-copy pin is outside the public byte bound")
	}
	if config.HistoryFloor > config.SnapshotFrontier {
		return CacheKey{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "history floor exceeds snapshot frontier")
	}
	if config.RetentionGeneration <= 0 {
		return CacheKey{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "retention generation must be positive")
	}
	normalizedRequest, err := config.Request.Normalize()
	if err != nil {
		return CacheKey{}, err
	}
	limitsDigest, err := digestLimits(config.Limits)
	if err != nil {
		return CacheKey{}, err
	}
	indexDigest, err := digestIndexGenerations(config.IndexGenerations)
	if err != nil {
		return CacheKey{}, err
	}
	authorizationFingerprint, err := AuthorizationFingerprint(decision)
	if err != nil {
		return CacheKey{}, err
	}
	key := CacheKey{
		domainFingerprint:        domainFingerprint(config.AuthorizationDomain),
		authorizationFingerprint: authorizationFingerprint,
		policyGeneration:         decision.policyGeneration,
		policyCopyPinDigest: DigestBytes(
			"explorer-policy-copy-pin-v1", cloneBytes(config.PolicyCopyPin)),
		snapshotFrontier:       config.SnapshotFrontier,
		historyFloor:           config.HistoryFloor,
		retentionGeneration:    config.RetentionGeneration,
		requestDigest:          digestRequest(normalizedRequest),
		limitsDigest:           limitsDigest,
		indexGenerationsDigest: indexDigest,
		set:                    true,
	}
	if identity, ok := decision.SelectedOntology(); ok {
		key.ontologySchemaDigest = DigestBytes(
			"explorer-ontology-schema-v1", []byte(identity.SchemaID()))
		key.ontologyVersionDigest = DigestBytes(
			"explorer-ontology-version-v1", []byte(identity.VersionID()))
	}
	encoder := newDigestEncoder("explorer-cache-key-v1")
	encoder.bytes(key.domainFingerprint[:])
	encoder.bytes(key.authorizationFingerprint[:])
	encoder.int64(key.policyGeneration)
	encoder.bytes(key.policyCopyPinDigest[:])
	encoder.uint64(key.snapshotFrontier)
	encoder.uint64(key.historyFloor)
	encoder.int64(key.retentionGeneration)
	encoder.bytes(key.requestDigest[:])
	encoder.bytes(key.limitsDigest[:])
	encoder.bytes(key.indexGenerationsDigest[:])
	if decision.selectedOntology.Known() {
		encoder.bytes(key.ontologySchemaDigest[:])
		encoder.bytes(key.ontologyVersionDigest[:])
	}
	key.digest = encoder.sum()
	return key, nil
}

// Validate checks that the key was produced by NewCacheKey.
func (k CacheKey) Validate() error {
	if !k.set || k.policyGeneration <= 0 || k.retentionGeneration <= 0 ||
		k.historyFloor > k.snapshotFrontier {
		return shoal.NewError(shoal.ErrorInvalidArgument, "cache key is invalid")
	}
	return nil
}

// Clone returns an independent immutable value copy.
func (k CacheKey) Clone() CacheKey { return k }

// Digest returns the complete cache partition digest.
func (k CacheKey) Digest() Digest { return k.digest }

// PartitionDigest is the shared positive/negative partition identity.
func (k CacheKey) PartitionDigest() Digest { return k.digest }

// DomainFingerprint returns the non-disclosing domain identity.
func (k CacheKey) DomainFingerprint() Fingerprint { return k.domainFingerprint }

// AuthorizationFingerprint returns the exact authorized-projection identity.
func (k CacheKey) AuthorizationFingerprint() Fingerprint {
	return k.authorizationFingerprint
}

// PolicyGeneration returns the decision generation in the partition.
func (k CacheKey) PolicyGeneration() int64 { return k.policyGeneration }

// SnapshotFrontier returns the pinned stable frontier.
func (k CacheKey) SnapshotFrontier() uint64 { return k.snapshotFrontier }

// HistoryFloor returns the pinned history floor.
func (k CacheKey) HistoryFloor() uint64 { return k.historyFloor }

// RetentionGeneration returns the pinned retention generation.
func (k CacheKey) RetentionGeneration() int64 { return k.retentionGeneration }

// PolicyCopyPinDigest returns the hashed policy-copy pin.
func (k CacheKey) PolicyCopyPinDigest() Digest { return k.policyCopyPinDigest }

// RequestDigest returns the normalized query/modes/scope/limit digest.
func (k CacheKey) RequestDigest() Digest { return k.requestDigest }

// LimitsDigest returns the sorted additional-limit digest.
func (k CacheKey) LimitsDigest() Digest { return k.limitsDigest }

// IndexGenerationsDigest returns the aggregate sorted IGEN digest.
func (k CacheKey) IndexGenerationsDigest() Digest {
	return k.indexGenerationsDigest
}

func (k CacheKey) OntologySchemaDigest() Digest  { return k.ontologySchemaDigest }
func (k CacheKey) OntologyVersionDigest() Digest { return k.ontologyVersionDigest }

// String never renders raw query, scope, labels, IDs, or generation handles.
func (k CacheKey) String() string { return "cache-key:" + k.digest.String() }

func digestRequest(request retrieval.Request) Digest {
	encoder := newDigestEncoder("explorer-cache-request-v1")
	encoder.text(request.Text)
	encoder.uint64(uint64(request.TopK))
	encoder.uint64(uint64(len(request.Modes)))
	for _, mode := range request.Modes {
		encoder.text(string(mode))
	}
	encoder.uint64(uint64(len(request.Scope.DocumentIDs)))
	for _, id := range request.Scope.DocumentIDs {
		encoder.text(string(id))
	}
	encoder.uint64(uint64(len(request.Scope.NodeIDs)))
	for _, id := range request.Scope.NodeIDs {
		encoder.text(string(id))
	}
	if request.AsOf.IsZero() {
		encoder.boolean(false)
	} else {
		encoder.boolean(true)
		encoded, _ := request.AsOf.MarshalBinary()
		encoder.bytes(encoded)
	}
	encoder.boolean(request.Explain)
	return encoder.sum()
}

func digestLimits(limits map[string]uint64) (Digest, error) {
	if len(limits) > MaxCacheDimensions {
		return Digest{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "cache limits exceed the dimension bound")
	}
	names := make([]string, 0, len(limits))
	for name := range limits {
		if err := validateCacheDimensionName("cache limit", name); err != nil {
			return Digest{}, err
		}
		names = append(names, name)
	}
	sort.Strings(names)
	encoder := newDigestEncoder("explorer-cache-limits-v1")
	encoder.uint64(uint64(len(names)))
	for _, name := range names {
		encoder.text(name)
		encoder.uint64(limits[name])
	}
	return encoder.sum(), nil
}

func digestIndexGenerations(generations map[string][]byte) (Digest, error) {
	if len(generations) > MaxCacheDimensions {
		return Digest{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "index generations exceed the dimension bound")
	}
	names := make([]string, 0, len(generations))
	for name, generation := range generations {
		if err := validateCacheDimensionName("index family", name); err != nil {
			return Digest{}, err
		}
		if len(generation) == 0 || len(generation) > shoal.MaxIDBytes {
			return Digest{}, shoal.NewError(
				shoal.ErrorInvalidArgument,
				"index generation is outside the public byte bound",
			)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	encoder := newDigestEncoder("explorer-cache-index-generations-v1")
	encoder.uint64(uint64(len(names)))
	for _, name := range names {
		encoder.text(name)
		encoder.bytes(cloneBytes(generations[name]))
	}
	return encoder.sum(), nil
}

func validateCacheDimensionName(kind, name string) error {
	if !utf8.ValidString(name) || strings.TrimSpace(name) == "" {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, kind+" name must be valid nonblank UTF-8")
	}
	return shoal.ValidateSemanticString(kind+" name", name)
}
