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
	"unicode"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	// MaxGrantProvenanceBytes bounds each text field of a GrantProvenance.
	MaxGrantProvenanceBytes = 1024
	// MaxGrantClaimPathSegments bounds the claim path a grant names.
	MaxGrantClaimPathSegments = 8
)

// GrantProvenance says which operator mapping granted a decision its
// authority, and which token claim satisfied it (#451). It is set by an
// authenticator that maps a role from a claim against an operator-supplied
// mapping, and it is empty for every other decision.
//
// It carries no token bytes: the issuer and subject identify the principal,
// the claim path and matched value say which configured value the token
// carried, and the mapping digest pins the exact mapping in force when the
// decision was minted. A consumer that must refuse once the mapping changes
// compares MappingDigest with the digest now in force.
//
// IdentityClaimPath is set when the principal's identity was derived from an
// operator-configured stable identity claim rather than from sub (#526). It
// names the claim path; Subject still holds the token's raw sub, so the
// record says both which claim named the principal and which per-client
// subject the token carried. It is empty for every decision minted from sub.
type GrantProvenance struct {
	Issuer            string
	Subject           string
	ClaimPath         []string
	MatchedValue      string
	MappingDigest     Digest
	IdentityClaimPath []string
}

// Set reports whether any provenance is present.
func (p GrantProvenance) Set() bool {
	return p.Issuer != "" || p.Subject != "" || len(p.ClaimPath) > 0 ||
		p.MatchedValue != "" || p.MappingDigest != (Digest{}) ||
		len(p.IdentityClaimPath) > 0
}

// Validate requires a set provenance to be complete and bounded. An empty
// provenance is valid.
func (p GrantProvenance) Validate() error {
	if !p.Set() {
		return nil
	}
	invalid := func(what string) error {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "grant provenance "+what+" is invalid")
	}
	if !validProvenanceText(p.Issuer) {
		return invalid("issuer")
	}
	if !validProvenanceText(p.Subject) {
		return invalid("subject")
	}
	if len(p.ClaimPath) == 0 || len(p.ClaimPath) > MaxGrantClaimPathSegments {
		return invalid("claim path")
	}
	for _, segment := range p.ClaimPath {
		if !validProvenanceText(segment) {
			return invalid("claim path")
		}
	}
	if !validProvenanceText(p.MatchedValue) {
		return invalid("matched value")
	}
	if p.MappingDigest == (Digest{}) {
		return invalid("mapping digest")
	}
	// Optional: absent for a principal named by sub.
	if len(p.IdentityClaimPath) > MaxGrantClaimPathSegments {
		return invalid("identity claim path")
	}
	for _, segment := range p.IdentityClaimPath {
		if !validProvenanceText(segment) {
			return invalid("identity claim path")
		}
	}
	return nil
}

// Clone returns an independent copy.
func (p GrantProvenance) Clone() GrantProvenance {
	p.ClaimPath = append([]string(nil), p.ClaimPath...)
	if p.IdentityClaimPath != nil {
		p.IdentityClaimPath = append([]string(nil), p.IdentityClaimPath...)
	}
	return p
}

// Equal reports whether two provenances are the same, field for field.
func (p GrantProvenance) Equal(other GrantProvenance) bool {
	if p.Issuer != other.Issuer || p.Subject != other.Subject ||
		p.MatchedValue != other.MatchedValue ||
		p.MappingDigest != other.MappingDigest ||
		len(p.ClaimPath) != len(other.ClaimPath) {
		return false
	}
	for index := range p.ClaimPath {
		if p.ClaimPath[index] != other.ClaimPath[index] {
			return false
		}
	}
	if len(p.IdentityClaimPath) != len(other.IdentityClaimPath) {
		return false
	}
	for index := range p.IdentityClaimPath {
		if p.IdentityClaimPath[index] != other.IdentityClaimPath[index] {
			return false
		}
	}
	return true
}

func validProvenanceText(value string) bool {
	if value == "" || len(value) > MaxGrantProvenanceBytes ||
		!utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

// GrantProvenance returns an independent copy of the decision's grant
// provenance, which is empty unless an operator mapping granted it.
func (d Decision) GrantProvenance() GrantProvenance {
	return d.grantProvenance.Clone()
}

// grantProvenance appends the provenance to a fingerprint. It is called
// only for a set provenance, so a decision without one keeps the fingerprint
// it had before provenance existed. The leading marker is 2, distinct from
// the selected-ontology marker 1 that may precede it, so the two optional
// sections can never be read as each other.
func (e *digestEncoder) grantProvenance(p GrantProvenance) {
	e.uint64(2)
	e.text(p.Issuer)
	e.text(p.Subject)
	e.uint64(uint64(len(p.ClaimPath)))
	for _, segment := range p.ClaimPath {
		e.text(segment)
	}
	e.text(p.MatchedValue)
	e.bytes(p.MappingDigest[:])
	// The identity claim path (#526) is appended only when present, behind
	// its own marker, so a provenance minted from sub keeps the fingerprint
	// it had before the field existed. The marker is 4: each marker in the
	// fingerprint names one section, and 1 (ontology), 2 (provenance) and 3
	// (the executor binding, #557) are taken.
	if len(p.IdentityClaimPath) > 0 {
		e.uint64(4)
		e.uint64(uint64(len(p.IdentityClaimPath)))
		for _, segment := range p.IdentityClaimPath {
			e.text(segment)
		}
	}
}
