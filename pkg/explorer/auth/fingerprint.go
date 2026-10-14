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
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
)

// Digest is a non-reversible SHA-256 cache, policy, or audit identity.
type Digest [sha256.Size]byte

// Bytes returns an independent byte representation.
func (d Digest) Bytes() []byte { return append([]byte(nil), d[:]...) }

// String returns only the lowercase digest, never hashed raw values.
func (d Digest) String() string { return "sha256:" + hex.EncodeToString(d[:]) }

// DigestBytes hashes an opaque value under a domain-separation tag.
func DigestBytes(tag string, value []byte) Digest {
	encoder := newDigestEncoder(tag)
	encoder.bytes(value)
	return encoder.sum()
}

// Fingerprint identifies an exact authorized projection without exposing its
// raw identity or grant values.
type Fingerprint [sha256.Size]byte

// Bytes returns an independent byte representation.
func (f Fingerprint) Bytes() []byte { return append([]byte(nil), f[:]...) }

// String returns only the lowercase digest.
func (f Fingerprint) String() string {
	return "auth-sha256:" + hex.EncodeToString(f[:])
}

// AuthorizationFingerprint deterministically hashes the exact canonical
// domain, identity, delegation, operation, source, policy, generation, and
// service-ceiling grants, the grant provenance when one is set, and the
// executor binding when one is set. Expiry and request/audit identifiers are excluded
// because they do not change the authorized projection.
func AuthorizationFingerprint(decision Decision) (Fingerprint, error) {
	cloned, err := decision.cloneValidated()
	if err != nil {
		return Fingerprint{}, err
	}
	encoder := newDigestEncoder("explorer-authorization-fingerprint-v1")
	encoder.bytes(cloned.domain)
	encoder.int64(cloned.policyGeneration)
	encoder.text(string(cloned.subject))
	encoder.text(string(cloned.actor))
	encoder.text(string(cloned.clientID))
	encoder.uint64(uint64(len(cloned.onBehalfOf)))
	for _, identity := range cloned.onBehalfOf {
		encoder.text(string(identity))
	}
	encoder.uint64(uint64(len(cloned.operations)))
	for _, operation := range cloned.operations {
		encoder.text(string(operation))
	}
	encoder.uint64(uint64(len(cloned.sources)))
	for _, source := range cloned.sources {
		encoder.bytes(source)
	}
	encoder.uint64(uint64(len(cloned.policies)))
	for _, policy := range cloned.policies {
		encoder.bytes(policy)
	}
	encoder.text(string(cloned.serviceRole))
	encoder.text(string(cloned.serviceCeilingIdentity))
	if cloned.selectedOntology.Known() {
		encoder.boolean(true)
		encoder.text(string(cloned.selectedOntology.SchemaID()))
		encoder.text(string(cloned.selectedOntology.VersionID()))
	}
	if cloned.grantProvenance.Set() {
		encoder.grantProvenance(cloned.grantProvenance)
	}
	// The optional sections follow the fixed fields above, which are all
	// length-prefixed or counted and so end at a position a reader can find.
	// Each optional section is present only when set, appears in this fixed
	// order, opens with its own marker (ontology 1, provenance 2, executor
	// binding 3), and is itself length-prefixed throughout. After the fixed
	// fields a reader therefore sees either the end of input or a marker that
	// names exactly one section, parses that section to its known end, and
	// repeats; no section can be read as another, an absent section cannot be
	// read as a present one, and two decisions differing only in which
	// optional sections they carry, or in their contents, hash different
	// inputs. A decision with none of them hashes the bytes it did before any
	// existed, which keeps persisted fingerprints stable.
	if cloned.executorBinding != "" {
		encoder.executorBinding(cloned.executorBinding)
	}
	return Fingerprint(encoder.sum()), nil
}

func domainFingerprint(domain []byte) Fingerprint {
	return Fingerprint(DigestBytes("explorer-authorization-domain-v1", domain))
}

type digestEncoder struct {
	hash hash.Hash
}

func newDigestEncoder(tag string) *digestEncoder {
	encoder := &digestEncoder{hash: sha256.New()}
	encoder.text(tag)
	return encoder
}

func (e *digestEncoder) bytes(value []byte) {
	e.uint64(uint64(len(value)))
	_, _ = e.hash.Write(value)
}

func (e *digestEncoder) text(value string) {
	e.bytes([]byte(value))
}

func (e *digestEncoder) uint64(value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = e.hash.Write(encoded[:])
}

func (e *digestEncoder) int64(value int64) {
	e.uint64(uint64(value))
}

func (e *digestEncoder) boolean(value bool) {
	if value {
		e.uint64(1)
		return
	}
	e.uint64(0)
}

func (e *digestEncoder) sum() Digest {
	var digest Digest
	copy(digest[:], e.hash.Sum(nil))
	return digest
}
