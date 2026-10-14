// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package workspace

import (
	"context"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/accumulo"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
)

// TestNarrowingCarriesExecutorBinding: workspace narrowing rebuilds the
// decision field by field, and must keep its executor binding (#391) without
// gaining an on-behalf-of chain. A dropped binding would refuse the bound
// worker, since the role requires one, rather than narrowing it.
func TestNarrowingCarriesExecutorBinding(t *testing.T) {
	base, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "worker", Actor: "worker",
		AuthorizationDomain: []byte("domain"),
		AllowedOperations: []auth.Operation{
			auth.OperationExecute, auth.OperationValidate,
		},
		PermittedSourceIDs:     [][]byte{[]byte("source-a"), []byte("source-b")},
		PermittedPolicyIDs:     [][]byte{[]byte("policy-a")},
		PolicyGeneration:       7,
		AuthenticationExpires:  testNow.Add(time.Hour),
		RequestID:              "request",
		ServiceRole:            auth.ServiceRoleActionExecution,
		ServiceCeilingIdentity: "ceiling-execute",
		ExecutorBinding:        "executor-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	ceiling, err := auth.NewServiceCeiling(auth.ServiceCeilingConfig{
		Identity: "ceiling-execute",
		Role:     auth.ServiceRoleActionExecution,
		Authorizations: accumulo.NewAuthorizations(
			[]byte("svc:" + string(auth.ServiceRoleActionExecution))),
	})
	if err != nil {
		t.Fatal(err)
	}
	settings := Settings{
		WorkspaceID: "workspace", Owner: base.Subject(),
		AuthorizationDomain: base.AuthorizationDomain(),
		SettingsID: settingsIdentity(
			"workspace", base.Subject(), base.AuthorizationDomain()),
		Revision: 1, LastMutationID: "one",
		Narrowing: Narrowing{
			AllowedOperations: OperationSelection{
				Present: true,
				Values:  []auth.Operation{auth.OperationExecute},
			},
			PermittedSourceIDs: IDSelection{
				Present: true, Values: [][]byte{[]byte("source-a")},
			},
		},
	}
	effective, err := DeriveEffectiveDecision(
		context.Background(), base, settings, ApplyOptions{
			Operation: auth.OperationExecute,
			Now:       testNow, BaseLimits: testLimits(),
			ServiceCeiling: &ceiling,
		},
	)
	if err != nil {
		t.Fatalf("DeriveEffectiveDecision: %v", err)
	}
	narrowed := effective.Decision()
	if len(narrowed.PermittedSourceIDs()) != 1 {
		t.Fatal("settings did not narrow; the test is vacuous")
	}
	if narrowed.ExecutorBinding() != "executor-a" {
		t.Fatalf("narrowed binding = %q", narrowed.ExecutorBinding())
	}
	if len(narrowed.OnBehalfOf()) != 0 {
		t.Fatalf("narrowed decision gained a chain: %v", narrowed.OnBehalfOf())
	}
}
