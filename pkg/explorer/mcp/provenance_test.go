// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
)

// TestToolCallCarriesGrantProvenance: the per-call decision authorizedContext
// re-mints from the provider's template keeps the template's grant
// provenance (#451), as bindHTTPSessionDecision does. Dropping it would make
// a mapped approver's decision fingerprint as an unmapped one and fail the
// approval service's mapping check.
func TestToolCallCarriesGrantProvenance(t *testing.T) {
	authority := auth.NewAuthority()
	resolver := authority.Resolver()
	provenance := auth.GrantProvenance{
		Issuer: "https://issuer.example", Subject: "bob",
		ClaimPath: []string{"groups"}, MatchedValue: "approvers",
		MappingDigest: auth.DigestBytes("mapping", []byte("v1")),
	}
	template, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "subject", Actor: "actor",
		AuthorizationDomain: []byte("domain"),
		AllowedOperations: []auth.Operation{
			auth.OperationList, auth.OperationRead,
		},
		PermittedSourceIDs:    [][]byte{[]byte("source")},
		PermittedPolicyIDs:    [][]byte{[]byte("policy")},
		PolicyGeneration:      1,
		AuthenticationExpires: time.Now().Add(time.Hour),
		RequestID:             "template-request",
		GrantProvenance:       provenance,
	})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	service := &stubService{
		documents: func(
			ctx context.Context, _ webapi.DocumentsRequest,
		) (webapi.DocumentsResponse, error) {
			decision, err := resolver.Resolve(ctx)
			if err != nil {
				return webapi.DocumentsResponse{}, err
			}
			calls++
			if decision.RequestID() == template.RequestID() {
				t.Fatal("the template was bound unchanged; the test is vacuous")
			}
			if !decision.GrantProvenance().Equal(provenance) {
				t.Fatalf("bound provenance = %+v", decision.GrantProvenance())
			}
			return webapi.DocumentsResponse{}, nil
		},
	}
	server, err := newRecordedTestServer(t, Config{
		Service: service, Authority: authority,
		Decisions: DecisionProviderFunc(func(context.Context) (auth.Decision, error) {
			return template, nil
		}),
		requestIDFactory: sequentialRequestIDs(),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	makeReady(t, server)
	response := callToolRequest(t, server, ToolDocuments, `{}`)
	if response.Error != nil || decodeToolResult(t, response).IsError {
		t.Fatalf("tools/call failed: %+v", response)
	}
	if calls != 1 {
		t.Fatalf("service calls = %d", calls)
	}
}
