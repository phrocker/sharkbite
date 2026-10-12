// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package main

import (
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
)

// The guard on OperationApprovalSweep (#655), modelled on the collector-admin
// one. The sweep operation lets its holder record any held request in its
// scope as expired or stranded, so it is held by exactly one principal: the
// host's system observer, in a decision the host mints in approval_sweep.go
// for one approval at a time. No list a token is minted with may grant it,
// no authenticator may mint it, and nothing else in the command may denote
// it.

func TestNoMintedListGrantsApprovalSweep(t *testing.T) {
	lists := map[string][]auth.Operation{
		"oidcFleetOperations":       oidcFleetOperations,
		"oidcContributorOperations": oidcContributorOperations,
		"oidcReaderOperations":      oidcReaderOperations,
		"workspaceOperations":       workspaceOperations,
		"oidcApproverOperations":    oidcApproverOperations,
		"oidcExecutorOperations":    oidcExecutorOperations,
	}
	covered := make([]string, 0, len(lists))
	for name, set := range lists {
		covered = append(covered, name)
		if len(set) == 0 {
			t.Fatalf("the %s list is empty; a guard over it asserts nothing", name)
		}
		for _, operation := range set {
			if operation == auth.OperationApprovalSweep {
				t.Fatalf("the %s list grants OperationApprovalSweep; every "+
					"principal it mints could then record anyone's held "+
					"request as final", name)
			}
		}
	}
	sort.Strings(covered)
	if defined := operationListsDefinedHere(t); strings.Join(covered, ",") != strings.Join(defined, ",") {
		t.Fatalf("this guard covers %v and the command defines %v; every "+
			"package-level []auth.Operation needs a row here", covered, defined)
	}
	if err := auth.OperationApprovalSweep.Validate(); err != nil {
		t.Fatalf("OperationApprovalSweep is not a valid operation, so this "+
			"guard asserts nothing: %v", err)
	}
}

// TestApprovalSweepIsDenotedOnlyByTheSweepMinter is the source half: the one
// expression in this command's non-test code that denotes approval_sweep is
// the system observer's mint, in approval_sweep.go.
func TestApprovalSweepIsDenotedOnlyByTheSweepMinter(t *testing.T) {
	scanner := newExecuteGrantScanner(t)
	files := scanner.parseDir(t, ".")
	sites := operationSites(t, scanner, files, "OperationApprovalSweep", auth.OperationApprovalSweep)
	if len(sites) != 1 {
		t.Fatalf("approval_sweep is denoted at %v; exactly one site, the "+
			"system observer's mint, may denote it", sites)
	}
	if file := filepath.Base(strings.SplitN(sites[0], ":", 2)[0]); file != "approval_sweep.go" {
		t.Fatalf("approval_sweep is denoted in %s, not by the sweep minter", sites[0])
	}
}

// TestNoShippedAuthenticatorMintsApprovalSweep is the behavioural half:
// every kind of decision this command's authenticators mint, with every
// mapping configured and every claim naming the operation, holds no
// approval_sweep, and neither does the -dev-auth principal.
func TestNoShippedAuthenticatorMintsApprovalSweep(t *testing.T) {
	f := newExecutorFixture(t)
	value := string(auth.OperationApprovalSweep)
	smuggle := func(claims jwt.MapClaims) jwt.MapClaims {
		claims["access"] = []string{"reader", "writer", "fleet", value}
		claims["groups"] = []string{labelGroupSecret, value}
		claims["scope"] = value
		return claims
	}
	tokens := map[string]string{
		"workspace": f.human.signRS256(t, testKID, smuggle(f.human.defaultClaims(f.now))),
		"approver": f.human.signRS256(t, testKID, func(c jwt.MapClaims) jwt.MapClaims {
			c["access"] = []string{"reader", "writer", value}
			return c
		}(approverClaims(f.human, f.now, "bob"))),
		"executor": f.executorToken(t, smuggle(executorClaims(f.executor, f.now, testExecutorSubject))),
	}
	holds := func(d auth.Decision) bool {
		for _, operation := range d.AllowedOperations() {
			if operation == auth.OperationApprovalSweep {
				return true
			}
		}
		return false
	}
	for name, token := range tokens {
		decision, err := f.authn.Authenticate(bearerRequest(token))
		if err != nil {
			t.Fatalf("%s token: %v", name, err)
		}
		if holds(decision) {
			t.Fatalf("the %s decision holds approval_sweep", name)
		}
	}
	development, err := newDevelopmentAuthenticator(func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	decision, err := development.Authenticate(httptest.NewRequest("GET", "/api/v1/meta", nil))
	if err != nil {
		t.Fatal(err)
	}
	if holds(decision) {
		t.Fatal("the -dev-auth decision holds approval_sweep")
	}
}
