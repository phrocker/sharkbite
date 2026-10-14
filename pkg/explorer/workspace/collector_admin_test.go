// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package workspace

import (
	"context"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
)

// TestNarrowingCannotAddCollectorAdmin is the workspace half of the
// collector-admin guard (#446). Workspace settings parse operation names with
// auth.ParseOperation, which accepts collector_admin, and a narrowing replaces
// the decision's operation list outright. Only the subset check stands
// between a workspace owner's stored settings and a decision holding the
// operation, so it is asserted for this operation specifically.
func TestNarrowingCannotAddCollectorAdmin(t *testing.T) {
	base, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "owner", Actor: "owner",
		AuthorizationDomain: []byte("domain"),
		AllowedOperations: []auth.Operation{
			auth.OperationRead, auth.OperationWorkspaceSettingsWrite,
		},
		PolicyGeneration:      1,
		AuthenticationExpires: testNow.Add(time.Hour),
		RequestID:             "request",
	})
	if err != nil {
		t.Fatal(err)
	}
	settings := func(operations ...auth.Operation) Settings {
		return Settings{
			WorkspaceID: "workspace", Owner: base.Subject(),
			AuthorizationDomain: base.AuthorizationDomain(),
			SettingsID: settingsIdentity(
				"workspace", base.Subject(), base.AuthorizationDomain()),
			Revision: 1, LastMutationID: "one",
			Narrowing: Narrowing{AllowedOperations: OperationSelection{
				Present: true, Values: operations,
			}},
		}
	}
	options := ApplyOptions{Operation: auth.OperationRead, Now: testNow, BaseLimits: testLimits()}
	// Control: a narrowing within the base derives.
	if _, err := DeriveEffectiveDecision(context.Background(), base, settings(auth.OperationRead), options); err != nil {
		t.Fatalf("control narrowing: %v", err)
	}
	effective, err := DeriveEffectiveDecision(context.Background(), base,
		settings(auth.OperationRead, auth.OperationCollectorAdmin), options)
	if err == nil {
		for _, operation := range effective.Decision().AllowedOperations() {
			if operation == auth.OperationCollectorAdmin {
				t.Fatal("workspace narrowing minted collector_admin")
			}
		}
		t.Fatal("a widening narrowing derived a decision")
	}
	if _, err := auth.ParseOperation(string(auth.OperationCollectorAdmin)); err != nil {
		t.Fatalf("collector_admin no longer parses, so this guard is vacuous: %v", err)
	}
}
