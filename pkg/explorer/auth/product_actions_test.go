/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements. See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership. The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License. You may obtain a copy of the License at
 *
 *     https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package auth_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

var legacyAuthorizationOperations = []auth.Operation{
	auth.OperationIngest,
	auth.OperationList,
	auth.OperationRead,
	auth.OperationConnect,
	auth.OperationNeighborhood,
	auth.OperationRetrieve,
	auth.OperationValidate,
}

var productActionOperations = []auth.Operation{
	auth.OperationInvoke,
	auth.OperationExecute,
	auth.OperationActionApprove,
	auth.OperationDispatch,
	auth.OperationDelegate,
	auth.OperationAgentRegister,
	auth.OperationAgentHeartbeat,
	auth.OperationAgentRevoke,
	auth.OperationAgentResolve,
	auth.OperationSubscriptionCreate,
	auth.OperationSubscriptionDelete,
	auth.OperationSubscriptionDeliver,
	auth.OperationEventPublish,
	auth.OperationAnalyticsRead,
	auth.OperationTeamOverviewRead,
	auth.OperationWorkspaceSettingsRead,
	auth.OperationWorkspaceSettingsWrite,
	auth.OperationCollectorAdmin,
}

var authorizationOperations = append(
	append([]auth.Operation(nil), legacyAuthorizationOperations...),
	productActionOperations...,
)

var authorizationServiceRoles = []auth.ServiceRole{
	auth.ServiceRoleDataRead,
	auth.ServiceRoleDataWrite,
	auth.ServiceRoleCoordination,
	auth.ServiceRoleDerivation,
	auth.ServiceRoleMigration,
	auth.ServiceRoleSecurityAdmin,
	auth.ServiceRoleActionInvocation,
	auth.ServiceRoleActionExecution,
	auth.ServiceRoleActionApproval,
	auth.ServiceRoleActionDispatch,
	auth.ServiceRoleDelegation,
	auth.ServiceRoleAgentRegistration,
	auth.ServiceRoleAgentRevocation,
	auth.ServiceRoleAgentResolution,
	auth.ServiceRoleSubscription,
	auth.ServiceRoleEventPublication,
	auth.ServiceRoleAnalytics,
	auth.ServiceRoleTeamOverview,
	auth.ServiceRoleWorkspaceSettingsRead,
	auth.ServiceRoleWorkspaceSettingsWrite,
}

var roleOperationCeilings = map[auth.ServiceRole]map[auth.Operation]bool{
	auth.ServiceRoleDataRead: operationSet(
		auth.OperationList,
		auth.OperationRead,
		auth.OperationNeighborhood,
		auth.OperationRetrieve,
		auth.OperationValidate,
	),
	auth.ServiceRoleDataWrite: operationSet(
		auth.OperationIngest,
		auth.OperationConnect,
		auth.OperationValidate,
	),
	auth.ServiceRoleCoordination: operationSet(
		auth.OperationValidate,
	),
	auth.ServiceRoleDerivation: operationSet(
		auth.OperationRead,
		auth.OperationConnect,
		auth.OperationValidate,
	),
	auth.ServiceRoleMigration: operationSet(
		auth.OperationIngest,
		auth.OperationList,
		auth.OperationRead,
		auth.OperationConnect,
		auth.OperationNeighborhood,
		auth.OperationRetrieve,
		auth.OperationValidate,
	),
	auth.ServiceRoleSecurityAdmin: operationSet(
		auth.OperationValidate,
	),
	auth.ServiceRoleActionInvocation: operationSet(
		auth.OperationInvoke,
		auth.OperationValidate,
	),
	// Execute and nothing else. The separation is the point of #437: a
	// principal that may ask an agent to do something must not thereby be able
	// to claim another principal's queued work and read its input. This matrix
	// is where that is actually enforced — a service-level test that builds
	// AllowedOperations directly never exercises ServiceRole.Allows, which is
	// how a mutation granting execute to ServiceRoleActionInvocation survived
	// the dispatch tests entirely.
	// Resolve so a worker can read its own descriptor, which the fleet
	// confines to the executor binding (#391). Never heartbeat or register.
	auth.ServiceRoleActionExecution: operationSet(
		auth.OperationExecute,
		auth.OperationAgentResolve,
		auth.OperationValidate,
	),
	// Approve and nothing else. An approver that could also enqueue, invoke
	// or execute would be a second role in name only (#451).
	auth.ServiceRoleActionApproval: operationSet(
		auth.OperationActionApprove,
		auth.OperationValidate,
	),
	auth.ServiceRoleActionDispatch: operationSet(
		auth.OperationDispatch,
		auth.OperationValidate,
	),
	auth.ServiceRoleDelegation: operationSet(
		auth.OperationDelegate,
		auth.OperationValidate,
	),
	auth.ServiceRoleAgentRegistration: operationSet(
		auth.OperationAgentRegister,
		auth.OperationAgentHeartbeat,
		auth.OperationValidate,
	),
	auth.ServiceRoleAgentRevocation: operationSet(
		auth.OperationAgentRevoke,
		auth.OperationValidate,
	),
	auth.ServiceRoleAgentResolution: operationSet(
		auth.OperationAgentResolve,
		auth.OperationValidate,
	),
	auth.ServiceRoleSubscription: operationSet(
		auth.OperationSubscriptionCreate,
		auth.OperationSubscriptionDelete,
		auth.OperationSubscriptionDeliver,
		auth.OperationValidate,
	),
	auth.ServiceRoleEventPublication: operationSet(
		auth.OperationEventPublish,
		auth.OperationValidate,
	),
	auth.ServiceRoleAnalytics: operationSet(
		auth.OperationAnalyticsRead,
		auth.OperationRetrieve,
		auth.OperationValidate,
	),
	auth.ServiceRoleTeamOverview: operationSet(
		auth.OperationTeamOverviewRead,
		auth.OperationValidate,
	),
	auth.ServiceRoleWorkspaceSettingsRead: operationSet(
		auth.OperationWorkspaceSettingsRead,
		auth.OperationValidate,
	),
	auth.ServiceRoleWorkspaceSettingsWrite: operationSet(
		auth.OperationWorkspaceSettingsWrite,
		auth.OperationValidate,
	),
}

func operationSet(operations ...auth.Operation) map[auth.Operation]bool {
	result := make(map[auth.Operation]bool, len(operations))
	for _, operation := range operations {
		result[operation] = true
	}
	return result
}

func TestProductActionOperationValuesAndParsingAreStable(t *testing.T) {
	expected := map[auth.Operation]string{
		auth.OperationIngest:                 "ingest",
		auth.OperationList:                   "list",
		auth.OperationRead:                   "read",
		auth.OperationConnect:                "connect",
		auth.OperationNeighborhood:           "neighborhood",
		auth.OperationRetrieve:               "retrieve",
		auth.OperationValidate:               "validation",
		auth.OperationInvoke:                 "invoke",
		auth.OperationExecute:                "execute",
		auth.OperationActionApprove:          "action_approve",
		auth.OperationDispatch:               "dispatch",
		auth.OperationDelegate:               "delegate",
		auth.OperationAgentRegister:          "agent_register",
		auth.OperationAgentHeartbeat:         "agent_heartbeat",
		auth.OperationAgentRevoke:            "agent_revoke",
		auth.OperationAgentResolve:           "agent_resolve",
		auth.OperationSubscriptionCreate:     "subscription_create",
		auth.OperationSubscriptionDelete:     "subscription_delete",
		auth.OperationSubscriptionDeliver:    "subscription_deliver",
		auth.OperationEventPublish:           "event_publish",
		auth.OperationAnalyticsRead:          "analytics_read",
		auth.OperationTeamOverviewRead:       "team_overview_read",
		auth.OperationWorkspaceSettingsRead:  "workspace_settings_read",
		auth.OperationWorkspaceSettingsWrite: "workspace_settings_write",
		auth.OperationCollectorAdmin:         "collector_admin",
	}
	if len(expected) != len(authorizationOperations) {
		t.Fatalf("operation inventory = %d, want %d", len(authorizationOperations), len(expected))
	}
	for _, operation := range authorizationOperations {
		serialized, ok := expected[operation]
		if !ok {
			t.Fatalf("operation %q is missing a stable serialized value", operation)
		}
		if string(operation) != serialized {
			t.Errorf("operation value = %q, want %q", operation, serialized)
		}
		if err := operation.Validate(); err != nil {
			t.Errorf("%q.Validate() = %v", operation, err)
		}
		parsed, err := auth.ParseOperation(serialized)
		if err != nil || parsed != operation {
			t.Errorf("ParseOperation(%q) = %q, %v", serialized, parsed, err)
		}
	}

	for _, value := range []string{
		"", "Invoke", " invoke", "invoke ", "agent.register", "workspace_settings",
		"collector-admin", "Collector_Admin", "collector_admin ", "collector", "admin",
	} {
		if parsed, err := auth.ParseOperation(value); !shoal.IsErrorCode(
			err, shoal.ErrorInvalidArgument,
		) || parsed != "" {
			t.Errorf("ParseOperation(%q) = %q, %v", value, parsed, err)
		}
	}
}

func TestProductActionServiceRoleValuesAreStable(t *testing.T) {
	expected := map[auth.ServiceRole]string{
		auth.ServiceRoleDataRead:               "data_read",
		auth.ServiceRoleDataWrite:              "data_write",
		auth.ServiceRoleCoordination:           "coordination",
		auth.ServiceRoleDerivation:             "derivation",
		auth.ServiceRoleMigration:              "migration",
		auth.ServiceRoleSecurityAdmin:          "security_admin",
		auth.ServiceRoleActionInvocation:       "action_invocation",
		auth.ServiceRoleActionExecution:        "action_execution",
		auth.ServiceRoleActionApproval:         "action_approval",
		auth.ServiceRoleActionDispatch:         "action_dispatch",
		auth.ServiceRoleDelegation:             "delegation",
		auth.ServiceRoleAgentRegistration:      "agent_registration",
		auth.ServiceRoleAgentRevocation:        "agent_revocation",
		auth.ServiceRoleAgentResolution:        "agent_resolution",
		auth.ServiceRoleSubscription:           "subscription",
		auth.ServiceRoleEventPublication:       "event_publication",
		auth.ServiceRoleAnalytics:              "analytics",
		auth.ServiceRoleTeamOverview:           "team_overview",
		auth.ServiceRoleWorkspaceSettingsRead:  "workspace_settings_read",
		auth.ServiceRoleWorkspaceSettingsWrite: "workspace_settings_write",
	}
	if len(expected) != len(authorizationServiceRoles) {
		t.Fatalf("role inventory = %d, want %d", len(authorizationServiceRoles), len(expected))
	}
	for _, role := range authorizationServiceRoles {
		serialized, ok := expected[role]
		if !ok {
			t.Fatalf("role %q is missing a stable serialized value", role)
		}
		if string(role) != serialized {
			t.Errorf("role value = %q, want %q", role, serialized)
		}
		if err := role.Validate(); err != nil {
			t.Errorf("%q.Validate() = %v", role, err)
		}
	}
}

func TestServiceRoleOperationMatrixIsExhaustive(t *testing.T) {
	for _, role := range authorizationServiceRoles {
		allowed, ok := roleOperationCeilings[role]
		if !ok {
			t.Fatalf("role %q is missing an expected operation ceiling", role)
		}
		for _, operation := range authorizationOperations {
			if got, want := role.Allows(operation), allowed[operation]; got != want {
				t.Errorf("%q.Allows(%q) = %t, want %t", role, operation, got, want)
			}
		}
		if role.Allows(auth.Operation("unknown")) {
			t.Errorf("%q permits an unknown operation", role)
		}
	}

	for _, role := range []auth.ServiceRole{"", "unknown"} {
		for _, operation := range authorizationOperations {
			if role.Allows(operation) {
				t.Errorf("%q unexpectedly permits %q", role, operation)
			}
		}
	}

	for _, operation := range authorizationOperations {
		permitted := false
		for _, role := range authorizationServiceRoles {
			permitted = permitted || roleOperationCeilings[role][operation]
		}
		// Collector admin is the one operation no service role may carry
		// (#446): it assigns every collector's authority, and a role is a
		// ceiling some service holds by configuration, not an operator's
		// explicit grant. Any role allowing it would mint it for that role.
		if operation == auth.OperationCollectorAdmin {
			if permitted {
				t.Errorf("a service role permits %q", operation)
			}
			continue
		}
		if !permitted {
			t.Errorf("operation %q has no service-role ceiling", operation)
		}
	}
}

// TestNoServiceRoleOrWildcardImpliesCollectorAdmin is the role half of the
// collector-admin guard (#446): no role, including migration's legacy
// wildcard, allows it, so no service decision can be constructed holding it.
func TestNoServiceRoleOrWildcardImpliesCollectorAdmin(t *testing.T) {
	for _, role := range authorizationServiceRoles {
		if role.Allows(auth.OperationCollectorAdmin) {
			t.Errorf("%q allows collector_admin", role)
		}
		config := baseDecisionConfig()
		config.ServiceRole = role
		config.ServiceCeilingIdentity = "collector-admin-ceiling"
		config.AllowedOperations = []auth.Operation{auth.OperationCollectorAdmin}
		if _, err := auth.NewDecision(config); err == nil {
			t.Errorf("a %q decision was minted holding collector_admin", role)
		}
	}
	if err := auth.OperationCollectorAdmin.Validate(); err != nil {
		t.Fatalf("collector_admin is not a valid operation: %v", err)
	}
}

func TestMigrationRoleWildcardMutationGuard(t *testing.T) {
	for _, operation := range legacyAuthorizationOperations {
		if !auth.ServiceRoleMigration.Allows(operation) {
			t.Errorf("migration no longer permits legacy operation %q", operation)
		}
	}
	for _, operation := range productActionOperations {
		if auth.ServiceRoleMigration.Allows(operation) {
			t.Errorf("migration wildcard permits product action %q", operation)
		}

		config := baseDecisionConfig()
		config.ServiceRole = auth.ServiceRoleMigration
		config.ServiceCeilingIdentity = "migration-ceiling"
		config.AllowedOperations = []auth.Operation{operation}
		if _, err := auth.NewDecision(config); !shoal.IsErrorCode(
			err, shoal.ErrorInvalidArgument,
		) {
			t.Errorf("migration decision acquired %q: %v", operation, err)
		}
	}
}

func TestProductActionPrivilegeEscalationMutationGuard(t *testing.T) {
	legacyRoles := []auth.ServiceRole{
		auth.ServiceRoleDataRead,
		auth.ServiceRoleDataWrite,
		auth.ServiceRoleCoordination,
		auth.ServiceRoleDerivation,
		auth.ServiceRoleMigration,
		auth.ServiceRoleSecurityAdmin,
	}
	for _, role := range legacyRoles {
		for _, operation := range productActionOperations {
			if role.Allows(operation) {
				t.Errorf("legacy role %q acquired product action %q", role, operation)
			}
		}
	}

	for _, role := range authorizationServiceRoles[len(legacyRoles):] {
		for _, operation := range authorizationOperations {
			config := baseDecisionConfig()
			config.ServiceRole = role
			config.ServiceCeilingIdentity = "product-action-ceiling"
			config.AllowedOperations = []auth.Operation{operation}
			if role == auth.ServiceRoleActionExecution {
				// The role requires the executor it is bound to, and a bound
				// decision acts only as itself (#391); the operation ceiling
				// is what this guard is about.
				config.ExecutorBinding = "product-action-executor"
				config.OnBehalfOf = nil
			}
			decision, err := auth.NewDecision(config)
			if roleOperationCeilings[role][operation] {
				if err != nil {
					t.Errorf("NewDecision(%q, %q) = %v", role, operation, err)
					continue
				}
				resource := productActionResource()
				if err := decision.AuthorizeObject(operation, resource, testNow); err != nil {
					t.Errorf("AuthorizeObject(%q, %q) = %v", role, operation, err)
				}
			} else if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
				t.Errorf("NewDecision(%q, %q) = %v, want invalid_argument",
					role, operation, err)
			}
		}
	}
}

func TestProductActionsRequireExplicitUserCapability(t *testing.T) {
	readerConfig := baseDecisionConfig()
	readerConfig.ServiceRole = ""
	readerConfig.ServiceCeilingIdentity = ""
	reader := mustDecision(t, readerConfig)
	resource := productActionResource()
	for _, operation := range productActionOperations {
		if err := reader.AuthorizeObject(operation, resource, testNow); !shoal.IsErrorCode(
			err, shoal.ErrorUnauthorized,
		) {
			t.Errorf("ordinary reader acquired %q: %v", operation, err)
		}
	}

	for _, operation := range productActionOperations {
		config := baseDecisionConfig()
		config.ServiceRole = ""
		config.ServiceCeilingIdentity = ""
		config.AllowedOperations = []auth.Operation{operation}
		decision := mustDecision(t, config)
		if err := decision.AuthorizeObject(operation, resource, testNow); err != nil {
			t.Errorf("explicit user capability %q = %v", operation, err)
		}

		outside := resource
		outside.SourceID = []byte("outside-source")
		if err := decision.AuthorizeObject(
			operation, outside, testNow,
		); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			t.Errorf("out-of-scope %q = %v, want non-disclosing not_found", operation, err)
		}
	}
}

func TestProductActionServiceRolesRoundTripThroughPolicy(t *testing.T) {
	primaryOperation := map[auth.ServiceRole]auth.Operation{
		auth.ServiceRoleActionInvocation:       auth.OperationInvoke,
		auth.ServiceRoleActionDispatch:         auth.OperationDispatch,
		auth.ServiceRoleActionApproval:         auth.OperationActionApprove,
		auth.ServiceRoleDelegation:             auth.OperationDelegate,
		auth.ServiceRoleAgentRegistration:      auth.OperationAgentRegister,
		auth.ServiceRoleAgentRevocation:        auth.OperationAgentRevoke,
		auth.ServiceRoleAgentResolution:        auth.OperationAgentResolve,
		auth.ServiceRoleSubscription:           auth.OperationSubscriptionCreate,
		auth.ServiceRoleEventPublication:       auth.OperationEventPublish,
		auth.ServiceRoleAnalytics:              auth.OperationAnalyticsRead,
		auth.ServiceRoleTeamOverview:           auth.OperationTeamOverviewRead,
		auth.ServiceRoleWorkspaceSettingsRead:  auth.OperationWorkspaceSettingsRead,
		auth.ServiceRoleWorkspaceSettingsWrite: auth.OperationWorkspaceSettingsWrite,
	}
	for role, operation := range primaryOperation {
		config := baseDecisionConfig()
		config.ServiceRole = role
		config.ServiceCeilingIdentity = "product-action-ceiling"
		config.AllowedOperations = []auth.Operation{operation}
		decision := mustDecision(t, config)
		policy, err := auth.NewServicePolicy(basePolicyConfig(), decision)
		if err != nil {
			t.Errorf("NewServicePolicy(%q) = %v", role, err)
			continue
		}
		encoded, err := policy.Encode()
		if err != nil {
			t.Errorf("Encode(%q) = %v", role, err)
			continue
		}
		if !bytes.Contains(encoded, []byte("svc:"+string(role))) {
			t.Errorf("encoded policy %q lacks role %q", encoded, role)
		}
		decoded, err := auth.DecodePolicy(encoded)
		if err != nil {
			t.Errorf("DecodePolicy(%q) = %v", role, err)
			continue
		}
		if decoded.ServiceRole() != role {
			t.Errorf("decoded role = %q, want %q", decoded.ServiceRole(), role)
		}
	}
}

func TestProductActionOperationsAreAuditable(t *testing.T) {
	attribute, err := auth.NewAuditAttribute(
		auth.AuditAttributeObject, []byte("private-agent-or-workspace-id"))
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range productActionOperations {
		config := baseDecisionConfig()
		config.ServiceRole = ""
		config.ServiceCeilingIdentity = ""
		config.AllowedOperations = []auth.Operation{operation}
		decision := mustDecision(t, config)
		fingerprint, err := auth.AuthorizationFingerprint(decision)
		if err != nil {
			t.Errorf("AuthorizationFingerprint(%q) = %v", operation, err)
			continue
		}
		event, err := auth.NewAuditEvent(auth.AuditEventConfig{
			OccurredAt:               testNow,
			Operation:                operation,
			Outcome:                  auth.AuditAllowed,
			AuthorizationFingerprint: fingerprint,
			RequestDigest: auth.DigestBytes(
				"product-action-audit-request-v1", []byte(operation)),
			Attributes: []auth.AuditAttribute{attribute},
		})
		if err != nil {
			t.Errorf("NewAuditEvent(%q) = %v", operation, err)
			continue
		}
		if event.Operation() != operation {
			t.Errorf("audit operation = %q, want %q", event.Operation(), operation)
		}
		if strings.Contains(event.String(), "private-agent-or-workspace-id") {
			t.Errorf("audit event exposed the object for %q: %s", operation, event)
		}
	}
}

func productActionResource() auth.ResourceRequest {
	return auth.ResourceRequest{
		AuthorizationDomain: []byte("domain-secret"),
		SourceID:            []byte("source-a"),
		PolicyID:            []byte("policy-a"),
		ObjectID:            "product-action-object",
	}
}
