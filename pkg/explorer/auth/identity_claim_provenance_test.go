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

import "testing"

// TestGrantProvenanceIdentityClaimPath (#526): the stable identity claim path
// is optional, bounded, independent of its config, distinguishes two
// provenances, and reaches the fingerprint only when present — a provenance
// without it keeps the fingerprint it had before the field existed.
func TestGrantProvenanceIdentityClaimPath(t *testing.T) {
	config := provenanceTestConfig(t, false)
	config.GrantProvenance = approverProvenance()
	without, err := NewDecision(config)
	if err != nil {
		t.Fatal(err)
	}
	withoutFingerprint, err := AuthorizationFingerprint(without)
	if err != nil {
		t.Fatal(err)
	}
	// Computed before IdentityClaimPath existed.
	const pinned = "auth-sha256:7cf93eed8793e06784849f4e692468df82d4fddc4f307f8198c03adc14b4fd88"
	if withoutFingerprint.String() != pinned {
		t.Fatalf("a provenance without an identity claim path moved its "+
			"fingerprint: %s, want %s", withoutFingerprint, pinned)
	}

	config = provenanceTestConfig(t, false)
	config.GrantProvenance = approverProvenance()
	config.GrantProvenance.IdentityClaimPath = []string{"oid"}
	with, err := NewDecision(config)
	if err != nil {
		t.Fatal(err)
	}
	config.GrantProvenance.IdentityClaimPath[0] = "mutated"
	if got := with.GrantProvenance().IdentityClaimPath; len(got) != 1 || got[0] != "oid" {
		t.Fatalf("identity claim path aliases its config: %v", got)
	}
	if with.GrantProvenance().Equal(without.GrantProvenance()) {
		t.Fatal("provenances differing in identity claim path are equal")
	}
	withFingerprint, err := AuthorizationFingerprint(with)
	if err != nil {
		t.Fatal(err)
	}
	if withFingerprint == withoutFingerprint {
		t.Fatal("the identity claim path does not reach the fingerprint")
	}
	config = provenanceTestConfig(t, false)
	config.GrantProvenance = approverProvenance()
	config.GrantProvenance.IdentityClaimPath = []string{"ext", "oid"}
	nested, err := NewDecision(config)
	if err != nil {
		t.Fatal(err)
	}
	nestedFingerprint, err := AuthorizationFingerprint(nested)
	if err != nil {
		t.Fatal(err)
	}
	if nestedFingerprint == withFingerprint {
		t.Fatal("two identity claim paths share a fingerprint")
	}
	for name, path := range map[string][]string{
		"empty segment": {""},
		"control":       {"o\x00id"},
		"too long":      make([]string, MaxGrantClaimPathSegments+1),
	} {
		config := provenanceTestConfig(t, false)
		config.GrantProvenance = approverProvenance()
		config.GrantProvenance.IdentityClaimPath = path
		if _, err := NewDecision(config); err == nil {
			t.Errorf("an invalid identity claim path (%s) was accepted", name)
		}
	}
	// A path alone is not a provenance.
	config = provenanceTestConfig(t, false)
	config.GrantProvenance = GrantProvenance{IdentityClaimPath: []string{"oid"}}
	if _, err := NewDecision(config); err == nil {
		t.Fatal("an identity claim path without a grant was accepted")
	}
}
