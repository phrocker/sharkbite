// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package auth

import (
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func provenanceTestConfig(t *testing.T, withOntology bool) DecisionConfig {
	t.Helper()
	config := DecisionConfig{
		Subject: "oidc:https://issuer.example#alice", Actor: "shoal-explore-web-oidc",
		ClientID:            "oidc:https://issuer.example#console",
		OnBehalfOf:          nil,
		AuthorizationDomain: []byte("shoal-explore-web"),
		AllowedOperations:   []Operation{OperationList, OperationRead},
		PermittedSourceIDs:  [][]byte{[]byte("shoal-explore-web/workspace")},
		PermittedPolicyIDs:  [][]byte{[]byte("shoal-explore-web/workspace-grant")},
		PolicyGeneration:    1,
		AuthenticationExpires: time.Date(
			2030, 1, 1, 0, 0, 0, 0, time.UTC),
		RequestID: "request",
	}
	if withOntology {
		identity, err := ontology.NewOntologyIdentityFromIDs(
			shoal.ID("schema:"+strings.Repeat("a", 64)),
			shoal.ID("ontology-version:"+strings.Repeat("b", 64)))
		if err != nil {
			t.Fatal(err)
		}
		config.SelectedOntology = identity
	}
	return config
}

// TestFingerprintWithoutProvenanceIsUnchanged pins the fingerprint of a
// decision that carries no grant provenance to the bytes the encoder produced
// before provenance existed. Fingerprints are persisted on action and approval
// records and compared on replay, so an encoder change that moved them would
// make every stored record look like a different principal's.
func TestFingerprintWithoutProvenanceIsUnchanged(t *testing.T) {
	for _, probe := range []struct {
		name     string
		ontology bool
		want     string
	}{
		{"plain", false,
			"auth-sha256:89e9dd693e464a66e704ced9da5e77adb90125c15bc9980378c3cb4dea90e51e"},
		{"with ontology", true,
			"auth-sha256:cf9c569f7043fbb154015c83e8615282acacb0cbcb97b015059376e8e10da05e"},
	} {
		decision, err := NewDecision(provenanceTestConfig(t, probe.ontology))
		if err != nil {
			t.Fatal(err)
		}
		fingerprint, err := AuthorizationFingerprint(decision)
		if err != nil {
			t.Fatal(err)
		}
		if fingerprint.String() != probe.want {
			t.Errorf("%s fingerprint = %s, want %s", probe.name,
				fingerprint.String(), probe.want)
		}
	}
}

func approverProvenance() GrantProvenance {
	return GrantProvenance{
		Issuer: "https://issuer.example", Subject: "alice",
		ClaimPath:     []string{"realm_access", "roles"},
		MatchedValue:  "shoal-approvers",
		MappingDigest: DigestBytes("test-mapping", []byte("v1")),
	}
}

// TestGrantProvenanceIsCarriedAndFingerprinted pins that a set provenance
// survives every clone, is returned as an independent copy, and changes the
// fingerprint field by field — so an audit pinned to a fingerprint is pinned
// to the mapping and the claim that granted it.
func TestGrantProvenanceIsCarriedAndFingerprinted(t *testing.T) {
	base, err := NewDecision(provenanceTestConfig(t, false))
	if err != nil {
		t.Fatal(err)
	}
	baseFingerprint, err := AuthorizationFingerprint(base)
	if err != nil {
		t.Fatal(err)
	}
	if base.GrantProvenance().Set() {
		t.Fatal("a decision minted without provenance reports one")
	}
	config := provenanceTestConfig(t, false)
	config.GrantProvenance = approverProvenance()
	decision, err := NewDecision(config)
	if err != nil {
		t.Fatal(err)
	}
	config.GrantProvenance.ClaimPath[0] = "mutated"
	got := decision.GrantProvenance()
	if !got.Equal(approverProvenance()) {
		t.Fatalf("provenance aliases its config: %+v", got)
	}
	got.ClaimPath[0] = "mutated"
	if !decision.GrantProvenance().Equal(approverProvenance()) {
		t.Fatal("provenance accessor returns an alias")
	}
	cloned, err := decision.cloneValidated()
	if err != nil || !cloned.GrantProvenance().Equal(approverProvenance()) {
		t.Fatalf("clone dropped provenance: %v", err)
	}
	withProvenance, err := AuthorizationFingerprint(decision)
	if err != nil {
		t.Fatal(err)
	}
	if withProvenance == baseFingerprint {
		t.Fatal("provenance does not reach the fingerprint")
	}
	seen := map[Fingerprint]string{withProvenance: "original"}
	for name, edit := range map[string]func(*GrantProvenance){
		"issuer":  func(p *GrantProvenance) { p.Issuer += "/" },
		"subject": func(p *GrantProvenance) { p.Subject = "bob" },
		"path":    func(p *GrantProvenance) { p.ClaimPath = []string{"realm_access.roles"} },
		"value":   func(p *GrantProvenance) { p.MatchedValue = "Shoal-Approvers" },
		"digest": func(p *GrantProvenance) {
			p.MappingDigest = DigestBytes("test-mapping", []byte("v2"))
		},
	} {
		changed := provenanceTestConfig(t, false)
		changed.GrantProvenance = approverProvenance()
		edit(&changed.GrantProvenance)
		other, err := NewDecision(changed)
		if err != nil {
			t.Fatal(err)
		}
		fingerprint, err := AuthorizationFingerprint(other)
		if err != nil {
			t.Fatal(err)
		}
		if previous, ok := seen[fingerprint]; ok {
			t.Fatalf("changing the %s collides with %s", name, previous)
		}
		seen[fingerprint] = name
	}
	// With an ontology lens too, both optional sections stay distinct.
	both := provenanceTestConfig(t, true)
	both.GrantProvenance = approverProvenance()
	decision, err = NewDecision(both)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := AuthorizationFingerprint(decision)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := seen[fingerprint]; ok {
		t.Fatal("ontology plus provenance collides with provenance alone")
	}
}

func TestGrantProvenanceMustBeComplete(t *testing.T) {
	for name, edit := range map[string]func(*GrantProvenance){
		"issuer":       func(p *GrantProvenance) { p.Issuer = "" },
		"subject":      func(p *GrantProvenance) { p.Subject = "" },
		"path":         func(p *GrantProvenance) { p.ClaimPath = nil },
		"empty seg":    func(p *GrantProvenance) { p.ClaimPath = []string{""} },
		"long path":    func(p *GrantProvenance) { p.ClaimPath = make([]string, 9) },
		"value":        func(p *GrantProvenance) { p.MatchedValue = "" },
		"control":      func(p *GrantProvenance) { p.MatchedValue = "a\x00b" },
		"digest":       func(p *GrantProvenance) { p.MappingDigest = Digest{} },
		"invalid utf8": func(p *GrantProvenance) { p.Subject = "\xff" },
	} {
		config := provenanceTestConfig(t, false)
		config.GrantProvenance = approverProvenance()
		edit(&config.GrantProvenance)
		if _, err := NewDecision(config); err == nil {
			t.Errorf("an incomplete provenance (%s) was accepted", name)
		}
	}
}
