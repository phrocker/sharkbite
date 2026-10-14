// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// TestIdentityClaimConfigurationRefusals: every configuration under which a
// stable identity would not be the one identity a principal carries, or
// would not be stable, refuses to start.
func TestIdentityClaimConfigurationRefusals(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	base := func() oidcConfig {
		config := issuer.testConfig(fixedClock(time.Now()))
		config.identityClaim = `["oid"]`
		return config
	}
	if _, err := newOIDCAuthenticator(base(), time.Now); err != nil {
		t.Fatalf("the control configuration was refused: %v", err)
	}
	for _, probe := range []struct {
		name string
		edit func(*oidcConfig)
	}{
		{"sub", func(c *oidcConfig) { c.identityClaim = `["sub"]` }},
		{"email", func(c *oidcConfig) { c.identityClaim = `["email"]` }},
		{"Email in another case", func(c *oidcConfig) { c.identityClaim = `["Email"]` }},
		{"preferred_username", func(c *oidcConfig) { c.identityClaim = `["preferred_username"]` }},
		{"upn", func(c *oidcConfig) { c.identityClaim = `["upn"]` }},
		{"unique_name", func(c *oidcConfig) { c.identityClaim = `["unique_name"]` }},
		{"name", func(c *oidcConfig) { c.identityClaim = `["name"]` }},
		{"nested mutable", func(c *oidcConfig) { c.identityClaim = `["profile","email"]` }},
		{"dotted string", func(c *oidcConfig) { c.identityClaim = `"ext.oid"` }},
		{"bare word", func(c *oidcConfig) { c.identityClaim = `oid` }},
		{"empty array", func(c *oidcConfig) { c.identityClaim = `[]` }},
		{"empty segment", func(c *oidcConfig) { c.identityClaim = `[""]` }},
		{"padded segment", func(c *oidcConfig) { c.identityClaim = `[" oid"]` }},
		{"control segment", func(c *oidcConfig) { c.identityClaim = `["o\u0000id"]` }},
		{"non-string segment", func(c *oidcConfig) { c.identityClaim = `[1]` }},
		{"too deep", func(c *oidcConfig) { c.identityClaim = `["a","b","c","d","e","f","g","h","i"]` }},
		{"trailing data", func(c *oidcConfig) { c.identityClaim = `["oid"] ["sub"]` }},
		{"subject claim", func(c *oidcConfig) { c.subjectClaim = "oid" }},
		{"actor claim", func(c *oidcConfig) { c.actorClaim = "act_as" }},
		{"delegation claim", func(c *oidcConfig) { c.delegationClaim = "on_behalf_of" }},
		{"legacy Entra mode", func(c *oidcConfig) {
			c.identityPrefix, c.trimIdentityValues = legacyEntraPrefix, true
			c.subjectFallbackClaim = "sub"
		}},
	} {
		t.Run(probe.name, func(t *testing.T) {
			config := base()
			probe.edit(&config)
			_, err := newOIDCAuthenticator(config, time.Now)
			if err == nil || !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) ||
				!strings.Contains(err.Error(), "-oidc-identity-claim") {
				t.Fatalf("refusal = %v, want an -oidc-identity-claim refusal", err)
			}
		})
	}
	// The legacy Entra flags themselves, through the compatibility layer.
	legacy := applyLegacyEntraCompatibility(oidcConfig{identityClaim: `["oid"]`},
		legacyEntraConfig{tenantID: "tenant", audience: "api://shoal"})
	if _, err := newOIDCAuthenticator(legacy, time.Now); err == nil ||
		!strings.Contains(err.Error(), "-oidc-identity-claim") {
		t.Fatalf("legacy Entra flags with the claim = %v", err)
	}
}

// TestIdentityClaimRefusesMultiTenantEntraIssuers: an object ID is unique
// within one tenant, so common, organizations and the {tenantid} template
// are refused; a tenant issuer is accepted. Each issuer goes through the
// real constructor; only the path differs.
func TestIdentityClaimRefusesMultiTenantEntraIssuers(t *testing.T) {
	for _, probe := range []struct {
		path string
		ok   bool
	}{
		{"/common/v2.0", false},
		{"/organizations/v2.0", false},
		{"/Organizations/v2.0", false},
		{"/{tenantid}/v2.0", false},
		{"/0c5a8f52-7d5e-4f53-9a6d-2b4f3d0b8e11/v2.0", true},
	} {
		t.Run(probe.path, func(t *testing.T) {
			issuer := newFakeOIDCIssuer(t)
			config := issuer.testConfig(fixedClock(time.Now()))
			config.issuer = issuer.server.URL + probe.path
			config.identityClaim = `["oid"]`
			_, err := newOIDCAuthenticator(config, time.Now)
			if probe.ok && err != nil {
				t.Fatalf("a tenant issuer was refused: %v", err)
			}
			if !probe.ok && (err == nil ||
				!strings.Contains(err.Error(), "not one tenant")) {
				t.Fatalf("a multi-tenant issuer = %v, want refusal", err)
			}
			// Without the claim the issuer is not this check's business.
			config.identityClaim = ""
			if _, err := newOIDCAuthenticator(config, time.Now); err != nil {
				t.Fatalf("without the claim: %v", err)
			}
		})
	}
}

// pinnedExampleMappingDigest is the digest of approverMappingDocument for
// https://issuer.example, as computed before identity_claim existed.
const pinnedExampleMappingDigest = "sha256:1abc1b5049c4db62d2a06b3aa847ad41eaff36183de3282997f9bf4691fcc2fa"

// TestIdentityClaimMustBeRestatedByTheMapping: the mapping's identity_claim
// and the flag must be equal, segment for segment, byte for byte, whenever
// either is set; and identity_claim is in the mapping digest only when set,
// so a mapping without it keeps the digest approvals were pinned to.
func TestIdentityClaimMustBeRestatedByTheMapping(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	build := func(flag string, claim any) (*oidcAuthenticator, error) {
		document := approverMappingDocument(issuer.server.URL)
		if claim != nil {
			document["identity_claim"] = claim
		}
		config := approverTestConfig(t, issuer, fixedClock(time.Now()), document)
		config.identityClaim = flag
		return newOIDCAuthenticator(config, time.Now)
	}
	plain, err := build("", nil)
	if err != nil {
		t.Fatal(err)
	}
	stable, err := build(`["oid"]`, []string{"oid"})
	if err != nil {
		t.Fatalf("a restated claim was refused: %v", err)
	}
	if stable.approverMappingDigest() == plain.approverMappingDigest() {
		t.Fatal("identity_claim is not in the mapping digest")
	}
	nested, err := build(`["ext","oid"]`, []string{"ext", "oid"})
	if err != nil {
		t.Fatal(err)
	}
	if nested.approverMappingDigest() == stable.approverMappingDigest() {
		t.Fatal("the identity claim path is not in the mapping digest")
	}
	// Pinned: a mapping without the field keeps the digest it had before the
	// field existed, so no approval pinned to it moves on upgrade.
	raw, err := json.Marshal(approverMappingDocument("https://issuer.example"))
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err := parseApproverMapping(
		raw, "https://issuer.example", []string{testAudience}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := unchanged.digest.String(); got != pinnedExampleMappingDigest {
		t.Fatalf("mapping digest without identity_claim = %s, want %s",
			got, pinnedExampleMappingDigest)
	}
	for _, probe := range []struct {
		name  string
		flag  string
		claim any
	}{
		{"flag without the mapping's", `["oid"]`, nil},
		{"mapping's without the flag", "", []string{"oid"}},
		{"different claim", `["oid"]`, []string{"sub_id"}},
		{"different case", `["oid"]`, []string{"OID"}},
		{"different depth", `["oid"]`, []string{"ext", "oid"}},
		{"dotted against nested", `["ext","oid"]`, []string{"ext.oid"}},
		{"empty list", `["oid"]`, []string{}},
		{"dotted string", `["oid"]`, "oid"},
	} {
		t.Run(probe.name, func(t *testing.T) {
			if _, err := build(probe.flag, probe.claim); err == nil ||
				!strings.Contains(err.Error(), "-oidc-approver-mapping-file is invalid") {
				t.Fatalf("mismatch = %v, want the mapping refusal", err)
			}
		})
	}
}

// TestIdentityClaimWaivesOnlyTheSubjectTypeStatement: with the claim, an
// issuer advertising pairwise subjects backs an approver mapping, at startup
// and at mint; without it, it does not. Discovery must still be readable
// either way.
func TestIdentityClaimWaivesOnlyTheSubjectTypeStatement(t *testing.T) {
	for _, probe := range []struct {
		name      string
		claim     bool
		types     []string
		discovery int
		ok        bool
	}{
		{"claim, pairwise", true, []string{"public", "pairwise"}, http.StatusOK, true},
		{"claim, no statement", true, nil, http.StatusOK, true},
		{"claim, discovery fails", true, []string{"public"}, http.StatusInternalServerError, false},
		{"no claim, pairwise", false, []string{"public", "pairwise"}, http.StatusOK, false},
	} {
		t.Run(probe.name, func(t *testing.T) {
			issuer := newFakeOIDCIssuer(t)
			issuer.mu.Lock()
			issuer.subjectTypes = probe.types
			issuer.discoveryStatus = probe.discovery
			issuer.mu.Unlock()
			now := time.Now()
			document := approverMappingDocument(issuer.server.URL)
			if probe.claim {
				document["identity_claim"] = []string{"oid"}
			}
			config := approverTestConfig(t, issuer, fixedClock(now), document)
			config.jwksURI = issuer.server.URL + "/keys"
			if probe.claim {
				config.identityClaim = `["oid"]`
			}
			authenticator := newTestOIDCAuthenticator(t, config)
			err := authenticator.verifyApproverDiscovery(context.Background())
			if (err == nil) != probe.ok {
				t.Fatalf("startup check = %v, want ok %v", err, probe.ok)
			}
			claims := approverClaims(issuer, now, "bob")
			claims["oid"] = "oid-bob"
			_, err = authenticator.authenticate(bearerRequest(
				issuer.signRS256(t, testKID, claims)))
			if probe.ok && err != nil {
				t.Fatalf("approver mint = %v", err)
			}
			if !probe.ok && !errors.Is(err, errApproverSubjectTypes) {
				t.Fatalf("approver mint = %v, want the discovery refusal", err)
			}
		})
	}
}

// TestIdentitySchemeDigestNamesIssuerPathAndFormat: the scheme digest moves
// with the issuer, the claim path, and between the stable and sub-derived
// formats, and is stable for one configuration.
func TestIdentitySchemeDigestNamesIssuerPathAndFormat(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	scheme := func(edit func(*oidcConfig)) oidcIdentityScheme {
		config := issuer.testConfig(fixedClock(time.Now()))
		edit(&config)
		return newTestOIDCAuthenticator(t, config).identityScheme()
	}
	stable := scheme(func(c *oidcConfig) { c.identityClaim = `["oid"]` })
	again := scheme(func(c *oidcConfig) { c.identityClaim = `["oid"]` })
	if stable.digest != again.digest || stable.approvals.Digest != stable.digest {
		t.Fatal("the scheme digest is not a function of the configuration")
	}
	seen := map[[32]byte]string{stable.digest: "stable oid"}
	for name, edit := range map[string]func(*oidcConfig){
		"sub-derived":   func(c *oidcConfig) {},
		"subject claim": func(c *oidcConfig) { c.subjectClaim = "uid" },
		"nested path":   func(c *oidcConfig) { c.identityClaim = `["ext","oid"]` },
		"dotted path":   func(c *oidcConfig) { c.identityClaim = `["ext.oid"]` },
		"issuer": func(c *oidcConfig) {
			c.issuer = issuer.server.URL + "/tenant"
			c.identityClaim = `["oid"]`
		},
	} {
		got := scheme(edit)
		if previous, ok := seen[got.digest]; ok {
			t.Fatalf("%s has the scheme digest of %s", name, previous)
		}
		seen[got.digest] = name
	}
	// Issuer-agnostic, and without the executor prefix (#391).
	family := []string{oidcIdentityPrefix, oidcStableIdentityPrefix, legacyEntraPrefix}
	for _, namespace := range humanIdentityFamily {
		if strings.HasPrefix("oidcexec:runner", namespace) {
			t.Fatalf("the executor prefix oidcexec: is in the human family via %q", namespace)
		}
	}
	sameFamily := func(got []string) bool {
		if len(got) != len(family) {
			return false
		}
		for index := range got {
			if got[index] != family[index] {
				return false
			}
		}
		return true
	}
	// The default scheme leaves the stamp zero (what unstamped records mean)
	// and still holds approvals to its namespace.
	if legacy := scheme(func(c *oidcConfig) {}); legacy.approvals.Digest != ([32]byte{}) ||
		legacy.approvals.Prefix != oidcIdentityPrefix+issuer.server.URL+"#" ||
		!sameFamily(legacy.approvals.Family) {
		t.Fatalf("the sub-derived scheme tells approvals %+v", legacy.approvals)
	}
	if stable.approvals.Prefix != oidcStableIdentityPrefix+issuer.server.URL+"#"+
		claimPathTag([]string{"oid"})+"#" || !sameFamily(stable.approvals.Family) {
		t.Fatalf("the stable scheme tells approvals %+v", stable.approvals)
	}
	nested := scheme(func(c *oidcConfig) { c.identityClaim = `["ext","oid"]` })
	if nested.approvals.Prefix == stable.approvals.Prefix {
		t.Fatal("two claim paths share a namespace")
	}
}
