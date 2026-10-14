// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestIdentityClaimRefusesEveryUnstableClaim (#526): each claim that does
// not name one human stably — editable and profile fields, per-session and
// per-token claims, per-client claims — is refused at startup, as the last
// segment of the path and in any case. The list is spelled out here rather
// than read from the map, so removing an entry fails this test.
func TestIdentityClaimRefusesEveryUnstableClaim(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	refused := []string{
		"email", "preferred_username", "upn", "unique_name", "name",
		"nickname", "given_name", "family_name", "locale", "picture",
		"website", "zoneinfo",
		"sid", "jti", "session_state", "nonce", "at_hash", "c_hash",
		"auth_time", "iat", "exp", "nbf", "acr", "amr",
		"azp", "client_id", "cid",
	}
	build := func(path []string) error {
		encoded, err := json.Marshal(path)
		if err != nil {
			t.Fatal(err)
		}
		config := issuer.testConfig(fixedClock(time.Now()))
		config.identityClaim = string(encoded)
		_, err = newOIDCAuthenticator(config, time.Now)
		return err
	}
	for _, name := range refused {
		for _, path := range [][]string{
			{name}, {strings.ToUpper(name)}, {"ext", name},
		} {
			if err := build(path); err == nil ||
				!strings.Contains(err.Error(), "does not name one human stably") {
				t.Errorf("identity claim %v = %v, want the unstable-claim refusal", path, err)
			}
		}
	}
	// Controls: stable per-user identifiers, and a refused name that is not
	// the last segment (the check is on the claim, not its parent — the
	// parent is the operator's assertion).
	for _, path := range [][]string{{"oid"}, {"user_id"}, {"sub_id"}, {"email", "id"}} {
		if err := build(path); err != nil {
			t.Errorf("identity claim %v was refused: %v", path, err)
		}
	}
}

// TestIssuerMustNotContainHash: '#' separates the issuer from the value in
// every identity (oidc:<iss>#<value>), so it is refused anywhere in the
// issuer — an empty fragment ("https://x/#"), which url.Parse reports as no
// fragment at all, included.
func TestIssuerMustNotContainHash(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	for _, raw := range []string{
		issuer.server.URL + "/#",
		issuer.server.URL + "#",
		issuer.server.URL + "/#tenant",
		issuer.server.URL + "/realms/a%23b#",
	} {
		config := issuer.testConfig(fixedClock(time.Now()))
		config.issuer = raw
		if _, err := newOIDCAuthenticator(config, time.Now); err == nil {
			t.Errorf("issuer %q was accepted", raw)
		}
		config.identityClaim = `["oid"]`
		if _, err := newOIDCAuthenticator(config, time.Now); err == nil {
			t.Errorf("issuer %q was accepted with a stable claim", raw)
		}
	}
	if err := validateOIDCEndpoint("issuer", "https://x/#", false); err == nil ||
		!strings.Contains(err.Error(), "'#'") {
		t.Fatalf(`"https://x/#" = %v, want the '#' refusal`, err)
	}
	// Control: the same issuer without it.
	config := issuer.testConfig(fixedClock(time.Now()))
	config.issuer = issuer.server.URL + "/"
	if _, err := newOIDCAuthenticator(config, time.Now); err != nil {
		t.Fatalf("a plain issuer was refused: %v", err)
	}
}
