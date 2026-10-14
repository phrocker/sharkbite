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

package coordination

// The identity-scheme row (#526) records which identity scheme a
// deployment's human principals are named under: a digest of the issuer, the
// claim path the identity is read from, and the format it is written in. It
// sits beside the policy generation in the coordination table because it is
// the same kind of fact — one every replica must agree on before it serves. A
// replica that would name the same human differently refuses to start, so
// one human cannot request on one replica and approve their own request on
// another.
//
// There is one row per domain, not per issuer. A deployment has one human
// issuer, and changing it (Entra v1 to v2, a hostname move) renames every
// human exactly as changing the claim does, so it must be seen as a scheme
// change. A row per issuer would let a new issuer find no row and record
// itself unchallenged.
//
// The value is a digest, never the configuration: the row says whether two
// replicas agree, not what either is configured with.

// IdentitySchemeKey identifies the identity-scheme row of one domain.
type IdentitySchemeKey struct {
	Domain DomainID
}

// IdentitySchemeRow is the row holding the identity scheme in force for a
// domain.
func IdentitySchemeRow(domain DomainID) ([]byte, error) {
	if err := domain.Validate(); err != nil {
		return nil, err
	}
	row := rowPrefix(RowKind('I'), B8('I', domain))
	return append(row, E(domain)...), nil
}

// ParseIdentitySchemeRow is the inverse of IdentitySchemeRow.
func ParseIdentitySchemeRow(row []byte) (IdentitySchemeKey, error) {
	domain, offset, err := parseTableRowPrefix(row, 'I')
	if err != nil {
		return IdentitySchemeKey{}, err
	}
	if offset != len(row) || row[2] != B8('I', domain) {
		return IdentitySchemeKey{}, invalid(
			"identity-scheme row has malformed or trailing components")
	}
	return IdentitySchemeKey{Domain: domain}, nil
}

// IdentitySchemeV1 is the value of an identity-scheme row: the issuer the
// recorded scheme is for, and the digest of the scheme, which covers that
// issuer. The issuer is restated so a refusal can say an issuer changed, and
// so a value whose issuer disagrees with a matching digest is recognised as
// corrupt rather than accepted.
type IdentitySchemeV1 struct {
	Issuer []byte
	Scheme Digest
}

func (s IdentitySchemeV1) Validate() error {
	if err := validateOpaque(
		"identity scheme issuer", s.Issuer, MaxOpaqueIDBytes, true); err != nil {
		return err
	}
	return s.Scheme.Validate("identity scheme digest")
}

func encodeIdentityScheme(e *encoder, s IdentitySchemeV1) {
	e.bytes("identity scheme issuer", s.Issuer)
	e.digest(s.Scheme)
}

func MarshalIdentitySchemeV1(s IdentitySchemeV1) ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return marshalEnvelope(KindIdentityScheme, VersionIdentitySchemeV1, MaxRootBytes,
		func(e *encoder) { encodeIdentityScheme(e, s) })
}

func UnmarshalIdentitySchemeV1(data []byte) (IdentitySchemeV1, error) {
	payload, err := verifyEnvelope(
		data, KindIdentityScheme, VersionIdentitySchemeV1, MaxRootBytes)
	if err != nil {
		return IdentitySchemeV1{}, err
	}
	d := &decoder{data: payload}
	s := IdentitySchemeV1{
		Issuer: d.bytes("identity scheme issuer", MaxOpaqueIDBytes, true),
		Scheme: d.digest("identity scheme digest"),
	}
	if d.err != nil {
		return IdentitySchemeV1{}, d.err
	}
	if err := s.Validate(); err != nil {
		return IdentitySchemeV1{}, err
	}
	if err := finishDecode(d, func(e *encoder) { encodeIdentityScheme(e, s) },
		MaxRootBytes); err != nil {
		return IdentitySchemeV1{}, err
	}
	return s, nil
}
