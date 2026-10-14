// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
)

// TestNoAuthenticatorMintsAnApprover is the behavioural line behind
// TestNoMintedPrincipalCanApprove. It mints a real decision through every
// authenticator the command can select — each OIDC mapping alone, all of them
// together, the unmapped fallback, and the -dev-auth principal — and asks the
// decision itself whether it authorizes action_approve on the workspace scope.
// A grant that is not a package-level list, or a mapping that appends an
// operation outside any list, is invisible to the parse test and caught here.
func TestNoAuthenticatorMintsAnApprover(t *testing.T) {
	now := time.Now()
	resource := auth.ResourceRequest{
		AuthorizationDomain: workspaceAuthorizationDomain,
		SourceID:            workspaceSourceID,
		PolicyID:            workspaceGrantPolicyID,
		ObjectID:            "approval",
	}
	assertNotApprover := func(name string, decision auth.Decision) {
		t.Helper()
		if len(decision.AllowedOperations()) == 0 {
			t.Fatalf("%s minted a decision with no operations; the "+
				"assertion below would be vacuous", name)
		}
		if operationsContain(decision.AllowedOperations(),
			auth.OperationActionApprove) {
			t.Fatalf("%s mints action_approve", name)
		}
		if err := decision.AuthorizeObject(
			auth.OperationActionApprove, resource, now,
		); err == nil {
			t.Fatalf("%s mints a decision that authorizes action_approve", name)
		}
	}

	issuer := newFakeOIDCIssuer(t)
	config := issuer.testConfig(fixedClock(now))
	config.fleetValues = []string{"fleet"}
	authenticator := newTestOIDCAuthenticator(t, config)
	for _, mapping := range []struct {
		name   string
		access []string
	}{
		{"OIDC reader mapping", []string{"reader"}},
		{"OIDC contributor mapping", []string{"writer"}},
		{"OIDC fleet mapping", []string{"fleet"}},
		{"OIDC combined mappings", []string{"reader", "writer", "fleet"}},
	} {
		claims := issuer.defaultClaims(now)
		claims["access"] = mapping.access
		decision, err := authenticator.Authenticate(
			bearerRequest(issuer.signRS256(t, testKID, claims)))
		if err != nil {
			t.Fatalf("%s: %v", mapping.name, err)
		}
		assertNotApprover(mapping.name, decision)
	}

	unmapped := issuer.testConfig(fixedClock(now))
	unmapped.allowUnmappedAuthorization = true
	fallback := newTestOIDCAuthenticator(t, unmapped)
	claims := issuer.defaultClaims(now)
	claims["access"] = []string{"nothing-mapped"}
	decision, err := fallback.Authenticate(
		bearerRequest(issuer.signRS256(t, testKID, claims)))
	if err != nil {
		t.Fatalf("OIDC unmapped fallback: %v", err)
	}
	assertNotApprover("OIDC unmapped fallback", decision)

	development, err := newDevelopmentAuthenticator(fixedClock(now))
	if err != nil {
		t.Fatal(err)
	}
	decision, err = development.Authenticate(
		httptest.NewRequest(http.MethodGet, "/api/v1/meta", nil))
	if err != nil {
		t.Fatal(err)
	}
	assertNotApprover("-dev-auth workspace principal", decision)

	// With an approver mapping in force, the workspace audience is
	// unchanged: every workspace mapping, alone and together, and a token
	// that also carries the approver claim and value, mints no approve. Only
	// the approver audience can, and the approver tests cover it.
	mapped := newTestOIDCAuthenticator(t, approverTestConfig(
		t, issuer, fixedClock(now), approverMappingDocument(issuer.server.URL)))
	for _, mapping := range []struct {
		name   string
		access []string
	}{
		{"mapped: reader", []string{"reader"}},
		{"mapped: contributor", []string{"writer"}},
		{"mapped: fleet", []string{"fleet"}},
		{"mapped: combined", []string{"reader", "writer", "fleet"}},
		{"mapped: the approver value as a workspace value",
			[]string{"reader", testApproverValue}},
	} {
		claims := issuer.defaultClaims(now)
		claims["access"] = mapping.access
		claims["azp"] = testApproverClient
		claims["realm_access"] = map[string]any{
			"roles": []any{testApproverValue},
		}
		decision, err := mapped.Authenticate(
			bearerRequest(issuer.signRS256(t, testKID, claims)))
		if err != nil {
			t.Fatalf("%s: %v", mapping.name, err)
		}
		assertNotApprover(mapping.name, decision)
		if decision.GrantProvenance().Set() {
			t.Fatalf("%s: a workspace decision carries grant provenance",
				mapping.name)
		}
	}
}
