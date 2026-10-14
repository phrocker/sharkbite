// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package main

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

// TestApproverMappingRequiresPublicSubjects: under the pairwise subject type
// one human has a different sub per client, so a requester and an approver
// could be the same person under two identities. An approver mapping is
// therefore refused — at startup, and on every approver mint — unless
// discovery states subject_types_supported ["public"] and nothing else. A
// missing statement, or a discovery that cannot be read, is a refusal. The
// workspace audience is unaffected.
func TestApproverMappingRequiresPublicSubjects(t *testing.T) {
	for _, probe := range []struct {
		name      string
		types     []string
		discovery int
		ok        bool
	}{
		{"public only", []string{"public"}, http.StatusOK, true},
		{"pairwise", []string{"pairwise"}, http.StatusOK, false},
		{"public and pairwise", []string{"public", "pairwise"}, http.StatusOK, false},
		{"pairwise and public", []string{"pairwise", "public"}, http.StatusOK, false},
		{"missing", nil, http.StatusOK, false},
		{"empty", []string{}, http.StatusOK, false},
		{"Public in another case", []string{"Public"}, http.StatusOK, false},
		{"discovery fails", []string{"public"}, http.StatusInternalServerError, false},
	} {
		t.Run(probe.name, func(t *testing.T) {
			issuer := newFakeOIDCIssuer(t)
			issuer.mu.Lock()
			issuer.subjectTypes = probe.types
			issuer.discoveryStatus = probe.discovery
			issuer.mu.Unlock()
			now := time.Now()
			config := approverTestConfig(t, issuer, fixedClock(now),
				approverMappingDocument(issuer.server.URL))
			// Keys are served statically so a failed discovery does not also
			// fail signature validation: the refusal must be this check's.
			config.jwksURI = issuer.server.URL + "/keys"
			authenticator := newTestOIDCAuthenticator(t, config)

			err := authenticator.verifyApproverDiscovery(context.Background())
			if (err == nil) != probe.ok {
				t.Fatalf("startup check = %v, want ok %v", err, probe.ok)
			}
			_, err = authenticator.authenticate(bearerRequest(issuer.signRS256(
				t, testKID, approverClaims(issuer, now, "bob"))))
			if probe.ok && err != nil {
				t.Fatalf("approver mint = %v", err)
			}
			if !probe.ok && !errors.Is(err, errApproverSubjectTypes) {
				t.Fatalf("approver mint = %v, want the subject-type refusal", err)
			}
			if _, err := authenticator.authenticate(bearerRequest(
				issuer.signRS256(t, testKID, issuer.defaultClaims(now)))); err != nil {
				t.Fatalf("workspace mint affected: %v", err)
			}
		})
	}
	// Without a mapping the check is not applied at all.
	issuer := newFakeOIDCIssuer(t)
	issuer.subjectTypes = []string{"pairwise"}
	plain := newTestOIDCAuthenticator(t, issuer.testConfig(fixedClock(time.Now())))
	if err := plain.verifyApproverDiscovery(context.Background()); err != nil {
		t.Fatalf("no mapping, pairwise issuer: %v", err)
	}
}

// TestKeycloakDiscoveryIsRefusedForApprovers pins that every Keycloak realm
// is refused at startup. Keycloak's OIDCWellKnownProvider
// (services/src/main/java/org/keycloak/protocol/oidc/OIDCWellKnownProvider.java)
// declares DEFAULT_SUBJECT_TYPES_SUPPORTED = ["public", "pairwise"] (around
// line 81) and sets it on every well-known document unconditionally (around
// line 151), whatever mappers the realm configures. A realm may therefore
// issue pairwise subjects, so Keycloak cannot back an approver mapping until
// the stable-identity follow-up (#526).
func TestKeycloakDiscoveryIsRefusedForApprovers(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	issuer.subjectTypes = []string{"public", "pairwise"}
	now := time.Now()
	authenticator := newTestOIDCAuthenticator(t, approverTestConfig(
		t, issuer, fixedClock(now), approverMappingDocument(issuer.server.URL)))
	if err := authenticator.verifyApproverDiscovery(
		context.Background()); !errors.Is(err, errApproverSubjectTypes) {
		t.Fatalf("Keycloak's default discovery at startup = %v, want refusal", err)
	}
}
