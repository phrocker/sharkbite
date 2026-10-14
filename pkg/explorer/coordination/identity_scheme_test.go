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

import (
	"bytes"
	"reflect"
	"testing"
)

// TestIdentitySchemeRowGoldenAndRoundTrip pins the identity-scheme row key
// (#526) to checked-in bytes, as every other coordination row is: a replica
// that encoded the key differently would read no row, write its own, and
// start beside a replica naming the same human differently. The key is the
// domain alone, so an issuer change cannot find an empty row either.
func TestIdentitySchemeRowGoldenAndRoundTrip(t *testing.T) {
	domain := DomainID{0, 0xff, 'd'}
	row, err := IdentitySchemeRow(domain)
	if err != nil {
		t.Fatal(err)
	}
	row = golden(t, "identity_scheme_row_v1.bin", row)
	decoded, err := ParseIdentitySchemeRow(row)
	if err != nil {
		t.Fatal(err)
	}
	if want := (IdentitySchemeKey{Domain: domain}); !reflect.DeepEqual(decoded, want) {
		t.Fatalf("row round trip differs:\ngot  %#v\nwant %#v", decoded, want)
	}
	if _, err := ParseIdentitySchemeRow(append(append([]byte(nil), row...), 0)); err == nil {
		t.Fatal("trailing row bytes accepted")
	}
	corrupt := append([]byte(nil), row...)
	corrupt[2] ^= 1
	if _, err := ParseIdentitySchemeRow(corrupt); err == nil {
		t.Fatal("partition band mismatch accepted")
	}
	// One row per domain, whatever the issuer: an issuer change finds the
	// row the previous issuer wrote. Distinct from the policy generation's.
	other, err := IdentitySchemeRow(DomainID("another-domain"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(row, other) {
		t.Fatal("two domains share an identity-scheme row")
	}
	generation, err := PolicyGenerationRow(domain, 1)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.HasPrefix(generation, row[:2]) {
		t.Fatal("the identity-scheme row shares the policy generation's row kind")
	}
	if _, err := IdentitySchemeRow(nil); err == nil {
		t.Fatal("an empty domain was accepted")
	}
}

func TestIdentitySchemeValueGoldenAndRoundTrip(t *testing.T) {
	value := IdentitySchemeV1{
		Issuer: []byte("https://login.example/tenant/v2.0"),
		Scheme: testDigest("identity-scheme"),
	}
	encoded, err := MarshalIdentitySchemeV1(value)
	if err != nil {
		t.Fatal(err)
	}
	again, err := MarshalIdentitySchemeV1(value)
	if err != nil || !bytes.Equal(encoded, again) {
		t.Fatal("marshal is not deterministic")
	}
	decoded, err := UnmarshalIdentitySchemeV1(golden(t, "identity_scheme_v1.bin", encoded))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, value) {
		t.Fatalf("round trip differs:\ngot  %#v\nwant %#v", decoded, value)
	}
	if _, err := MarshalIdentitySchemeV1(IdentitySchemeV1{Issuer: value.Issuer}); err == nil {
		t.Fatal("a zero scheme digest was accepted")
	}
	corrupt := append([]byte(nil), encoded...)
	corrupt[len(corrupt)-1] ^= 1
	if _, err := UnmarshalIdentitySchemeV1(corrupt); err == nil {
		t.Fatal("a corrupt value was accepted")
	}
}
