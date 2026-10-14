// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
)

func boundExecutorDecision(t *testing.T) auth.Decision {
	t.Helper()
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "worker", Actor: "worker",
		AuthorizationDomain:    []byte("domain"),
		AllowedOperations:      []auth.Operation{auth.OperationExecute},
		PolicyGeneration:       1,
		AuthenticationExpires:  time.Now().Add(time.Hour),
		RequestID:              "template-request",
		ServiceRole:            auth.ServiceRoleActionExecution,
		ServiceCeilingIdentity: "ceiling-execute",
		ExecutorBinding:        "executor-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	return decision
}

// TestReMintedDecisionsCarryExecutorBinding: both places the MCP server
// rebuilds a decision field by field, the per-call re-mint in
// authorizedContext and the HTTP session bind, keep the executor binding
// (#391). A dropped binding would refuse the bound worker outright, since the
// role requires one, rather than narrowing it.
func TestReMintedDecisionsCarryExecutorBinding(t *testing.T) {
	authority := auth.NewAuthority()
	resolver := authority.Resolver()
	template := boundExecutorDecision(t)
	server, err := newRecordedTestServer(t, Config{
		Service: &stubService{}, Authority: authority,
		Decisions: DecisionProviderFunc(func(context.Context) (auth.Decision, error) {
			return template, nil
		}),
		requestIDFactory: sequentialRequestIDs(),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ctx, decision, err := server.authorizedContext(context.Background())
	if err != nil {
		t.Fatalf("authorizedContext: %v", err)
	}
	if decision.RequestID() == template.RequestID() {
		t.Fatal("the template was bound unchanged; the test is vacuous")
	}
	if decision.ExecutorBinding() != "executor-a" {
		t.Fatalf("re-minted binding = %q", decision.ExecutorBinding())
	}
	resolved, err := resolver.Resolve(ctx)
	if err != nil || resolved.ExecutorBinding() != "executor-a" {
		t.Fatalf("bound binding = %q, %v", resolved.ExecutorBinding(), err)
	}

	ctx, err = server.bindHTTPSessionDecision(
		context.Background(), template, "session-correlation")
	if err != nil {
		t.Fatalf("bindHTTPSessionDecision: %v", err)
	}
	resolved, err = resolver.Resolve(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.CorrelationID() != "session-correlation" {
		t.Fatal("the session decision was bound unchanged; the test is vacuous")
	}
	if resolved.ExecutorBinding() != "executor-a" {
		t.Fatalf("session binding = %q", resolved.ExecutorBinding())
	}
	if resolved.OnBehalfOf() != nil {
		t.Fatalf("session decision gained a chain: %v", resolved.OnBehalfOf())
	}
}
