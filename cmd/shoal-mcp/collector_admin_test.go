// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package main

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
)

// TestTheProcessIdentityNeverHoldsCollectorAdmin is shoal-mcp's half of the
// collector-admin guard (#446). The explicit -identity-operations list is
// parsed with Operation.Validate, which accepts collector_admin, so the
// refusal has to be explicit: neither the flag, the environment variable,
// the default read identity nor -dev-auth yields it.
func TestTheProcessIdentityNeverHoldsCollectorAdmin(t *testing.T) {
	for _, list := range []string{
		"collector_admin",
		"read,collector_admin",
		" collector_admin ",
	} {
		if _, err := configureIdentity(identityOptions{
			subject: "subject", actor: "actor", domain: "domain",
			sourceID: "source", policyID: "policy", operations: list,
			policyGeneration: 1, lifetime: time.Hour,
		}); err == nil || !strings.Contains(err.Error(), "cannot be granted") {
			t.Fatalf("operations %q: %v", list, err)
		}
	}
	environment := map[string]string{
		"SHOAL_MCP_IDENTITY_SUBJECT":    "subject",
		"SHOAL_MCP_IDENTITY_ACTOR":      "actor",
		"SHOAL_MCP_IDENTITY_DOMAIN":     "domain",
		"SHOAL_MCP_IDENTITY_SOURCE":     "source",
		"SHOAL_MCP_IDENTITY_POLICY":     "policy",
		"SHOAL_MCP_IDENTITY_OPERATIONS": "list,collector_admin",
	}
	if _, err := parseCommandConfig(nil, io.Discard, func(key string) string {
		return environment[key]
	}); err == nil || !strings.Contains(err.Error(), "cannot be granted") {
		t.Fatalf("environment grant: %v", err)
	}
	// Nor approval_sweep (#655): the host's system observer's operation.
	if _, err := parseOperations(string(auth.OperationApprovalSweep)); err == nil ||
		!strings.Contains(err.Error(), "cannot be granted") {
		t.Fatalf("approval_sweep grant: %v", err)
	}

	for name, options := range map[string]identityOptions{
		"default":   {subject: "subject", actor: "actor", domain: "domain", sourceID: "source", policyID: "policy", policyGeneration: 1, lifetime: time.Hour},
		"-dev-auth": {development: true, policyGeneration: 1, lifetime: time.Hour},
	} {
		config, err := configureIdentity(options)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		identity, err := newProcessIdentity(config, time.Now)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		decision, err := identity.Decision(context.Background())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, operation := range decision.AllowedOperations() {
			if operation == auth.OperationCollectorAdmin {
				t.Fatalf("the %s process identity holds collector_admin", name)
			}
		}
	}
}
