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

package auth

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	// MaxPolicyComponentBytes is the maximum decoded domain, source, or grant
	// policy identity accepted by the visibility codec.
	MaxPolicyComponentBytes = 128
	// MaxEncodedPolicyComponentBytes is the corresponding no-padding base32
	// bound.
	MaxEncodedPolicyComponentBytes = (MaxPolicyComponentBytes*8 + 4) / 5
	// MaxPolicyExpressionBytes bounds one flattened Accumulo visibility.
	MaxPolicyExpressionBytes = 4 * 1024
	// MaxPolicyTerms bounds one flattened Accumulo visibility.
	MaxPolicyTerms = 64
	// MaxOnBehalfOfEntries bounds one trusted delegation chain.
	MaxOnBehalfOfEntries = 64
	// MaxDecisionOperations bounds trusted constructor work before operation
	// deduplication.
	MaxDecisionOperations = 64
	// MaxDecisionGrantIDs applies the repository's public scope-ID bound to
	// each trusted source and policy grant set.
	MaxDecisionGrantIDs = retrieval.MaxScopeIDs
)

// Operation is an exact Explorer authorization action.
type Operation string

const (
	OperationIngest           Operation = "ingest"
	OperationList             Operation = "list"
	OperationRead             Operation = "read"
	OperationConnect          Operation = "connect"
	OperationGraphMaterialize Operation = "graph_materialize"
	OperationNeighborhood     Operation = "neighborhood"
	OperationRetrieve         Operation = "retrieve"
	OperationValidate         Operation = "validation"

	OperationInvoke Operation = "invoke"
	// OperationExecute is permission to take an agent's queued work and finish
	// it — pull, claim and complete — as distinct from OperationInvoke, which
	// is permission to ask that agent to do something.
	//
	// They were one operation, and that is what made an out-of-process
	// executor impossible (#437). Claiming was authorized under invoke and
	// additionally required the claimant to be the principal that enqueued, so
	// an agent could enqueue work and a gateway could never take it: every
	// record failed the predicate and was silently skipped, leaving a ready
	// worker and a growing queue.
	//
	// Separating them is what lets the enqueuer and the executor be different
	// principals without making every invoker an executor. An agent that may
	// ask a gateway to post a message does not thereby gain the ability to
	// claim another agent's queued work and read its input.
	OperationExecute Operation = "execute"
	// OperationActionApprove is permission to decide a held request for an
	// action whose registration requires human approval (#451).
	//
	// It is a decision about somebody else's request and nothing more. It
	// grants no ability to enqueue, invoke, claim or execute the work: an
	// approved request still runs only through the requester's own record and
	// the claim fence, so an approval is a record (a ZTAT), not a grant.
	//
	// Holding it is necessary and not sufficient. The approval service also
	// requires the approver to fail the operations that create or perform the
	// work on the same scope, and to share no identity with the requester or
	// the agent — because under shared scopes, holding approve alone separates
	// nothing.
	OperationActionApprove          Operation = "action_approve"
	OperationDispatch               Operation = "dispatch"
	OperationDelegate               Operation = "delegate"
	OperationAgentRegister          Operation = "agent_register"
	OperationAgentHeartbeat         Operation = "agent_heartbeat"
	OperationAgentRevoke            Operation = "agent_revoke"
	OperationAgentResolve           Operation = "agent_resolve"
	OperationSubscriptionCreate     Operation = "subscription_create"
	OperationSubscriptionDelete     Operation = "subscription_delete"
	OperationSubscriptionDeliver    Operation = "subscription_deliver"
	OperationEventPublish           Operation = "event_publish"
	OperationAnalyticsRead          Operation = "analytics_read"
	OperationTeamOverviewRead       Operation = "team_overview_read"
	OperationWorkspaceSettingsRead  Operation = "workspace_settings_read"
	OperationWorkspaceSettingsWrite Operation = "workspace_settings_write"
	// OperationCollectorAdmin is permission to provision, revoke and read an
	// external collector's registration over HTTP (#446): to bind a principal
	// to a collector ID with an authority ceiling, a control and a mode.
	//
	// It is the authority behind every collector's authority, so it is
	// granted by nothing that grants anything else. No OIDC mapping, no
	// workspace or approver audience, no executor mint, no development
	// principal and no delegation mints it, and no service role allows it;
	// workspace narrowing can only remove it. It reaches a decision only
	// through a host that mints its own decisions for the routes it mounts
	// (webapi.MountCollectorAdmin), which is the same operator trust as the
	// Go provisioning API. cmd/shoal-explore-web/oidc_collector_admin_grant_test.go
	// and the auth package's inventory tests assert all of that.
	//
	// It implies nothing either: an admin cannot enroll or submit as a
	// collector, and cannot provision its own principal as one.
	OperationCollectorAdmin Operation = "collector_admin"
	// OperationApprovalSweep is permission to write down that a held
	// approval request (#451) can no longer progress: to move a request
	// whose window has closed to expired, and one whose target, policy
	// generation or action identity has permanently moved to stranded
	// (#655). It is a statement of a fact the approval service re-derives
	// from durable state before every write, never a decision about the
	// request, and it grants nothing else: no read of a request's input, no
	// decision, no enqueue and no materialization.
	//
	// It is held by exactly one principal, the host's own system observer
	// (fleet.ApprovalSweeperSubject), in a decision the host mints for one
	// approval's scope at a time and binds with its own binder. Like
	// collector_admin it is granted by nothing that grants anything else: no
	// OIDC mapping, no workspace, approver or executor audience, no
	// development principal and no delegation mints it, and no service role
	// allows it. cmd/shoal-explore-web/approval_sweep_grant_test.go and the
	// auth package's inventory tests assert that.
	OperationApprovalSweep Operation = "approval_sweep"
)

// ParseOperation accepts one exact canonical serialized operation.
func ParseOperation(value string) (Operation, error) {
	operation := Operation(value)
	if err := operation.Validate(); err != nil {
		return "", err
	}
	return operation, nil
}

// Validate rejects unknown operations.
func (o Operation) Validate() error {
	switch o {
	case OperationIngest, OperationList, OperationRead, OperationConnect,
		OperationGraphMaterialize,
		OperationNeighborhood, OperationRetrieve, OperationValidate,
		OperationInvoke, OperationExecute, OperationActionApprove,
		OperationDispatch, OperationDelegate,
		OperationAgentRegister, OperationAgentHeartbeat, OperationAgentRevoke,
		OperationAgentResolve, OperationSubscriptionCreate,
		OperationSubscriptionDelete, OperationSubscriptionDeliver,
		OperationEventPublish,
		OperationAnalyticsRead, OperationTeamOverviewRead,
		OperationWorkspaceSettingsRead,
		OperationWorkspaceSettingsWrite,
		OperationCollectorAdmin, OperationApprovalSweep:
		return nil
	default:
		return shoal.NewError(shoal.ErrorInvalidArgument, "unknown authorization operation")
	}
}

// ServiceRole identifies one least-privilege Accumulo service-account role.
type ServiceRole string

const (
	ServiceRoleDataRead      ServiceRole = "data_read"
	ServiceRoleDataWrite     ServiceRole = "data_write"
	ServiceRoleCoordination  ServiceRole = "coordination"
	ServiceRoleDerivation    ServiceRole = "derivation"
	ServiceRoleMigration     ServiceRole = "migration"
	ServiceRoleSecurityAdmin ServiceRole = "security_admin"

	ServiceRoleActionInvocation ServiceRole = "action_invocation"
	// ServiceRoleActionExecution is the role a worker holds: it may take and
	// finish queued work, and resolve the one descriptor its executor binding
	// names (#391), and do nothing else. Deliberately not granted by
	// ServiceRoleActionInvocation, so a principal that may enqueue cannot
	// thereby claim.
	ServiceRoleActionExecution ServiceRole = "action_execution"
	// ServiceRoleActionApproval is the role an approver holds: it may decide
	// held requests and do nothing else. It deliberately grants none of
	// invoke, dispatch or execute, because an approver that can also create or
	// perform the work it approves is not a second role.
	ServiceRoleActionApproval         ServiceRole = "action_approval"
	ServiceRoleActionDispatch         ServiceRole = "action_dispatch"
	ServiceRoleDelegation             ServiceRole = "delegation"
	ServiceRoleAgentRegistration      ServiceRole = "agent_registration"
	ServiceRoleAgentRevocation        ServiceRole = "agent_revocation"
	ServiceRoleAgentResolution        ServiceRole = "agent_resolution"
	ServiceRoleSubscription           ServiceRole = "subscription"
	ServiceRoleEventPublication       ServiceRole = "event_publication"
	ServiceRoleAnalytics              ServiceRole = "analytics"
	ServiceRoleTeamOverview           ServiceRole = "team_overview"
	ServiceRoleWorkspaceSettingsRead  ServiceRole = "workspace_settings_read"
	ServiceRoleWorkspaceSettingsWrite ServiceRole = "workspace_settings_write"
)

// Validate rejects an empty or unknown service role.
func (r ServiceRole) Validate() error {
	switch r {
	case ServiceRoleDataRead, ServiceRoleDataWrite, ServiceRoleCoordination,
		ServiceRoleDerivation, ServiceRoleMigration, ServiceRoleSecurityAdmin,
		ServiceRoleActionInvocation, ServiceRoleActionExecution,
		ServiceRoleActionApproval, ServiceRoleActionDispatch,
		ServiceRoleDelegation, ServiceRoleAgentRegistration,
		ServiceRoleAgentRevocation, ServiceRoleAgentResolution,
		ServiceRoleSubscription, ServiceRoleEventPublication,
		ServiceRoleAnalytics, ServiceRoleTeamOverview,
		ServiceRoleWorkspaceSettingsRead,
		ServiceRoleWorkspaceSettingsWrite:
		return nil
	default:
		return shoal.NewError(shoal.ErrorInvalidArgument, "unknown service role")
	}
}

// Allows reports whether the role may be used for an Explorer operation.
func (r ServiceRole) Allows(operation Operation) bool {
	if operation.Validate() != nil {
		return false
	}
	switch r {
	case ServiceRoleDataRead:
		return operation == OperationList || operation == OperationRead ||
			operation == OperationNeighborhood || operation == OperationRetrieve ||
			operation == OperationValidate
	case ServiceRoleDataWrite:
		return operation == OperationIngest || operation == OperationConnect ||
			operation == OperationGraphMaterialize ||
			operation == OperationValidate
	case ServiceRoleCoordination, ServiceRoleSecurityAdmin:
		return operation == OperationValidate
	case ServiceRoleDerivation:
		return operation == OperationRead || operation == OperationConnect ||
			operation == OperationValidate
	case ServiceRoleMigration:
		return isLegacyMigrationOperation(operation)
	case ServiceRoleActionInvocation:
		return operation == OperationInvoke || operation == OperationValidate
	case ServiceRoleActionExecution:
		// agent_resolve so a worker can read the one descriptor it is bound
		// to; the fleet confines it to that descriptor (#391). Never
		// heartbeat: a worker cannot truthfully assert a descriptor's
		// liveness.
		//
		// Not retrieve, and adding it is not free. The fleet's
		// refuseUnconfinedRetrieval (#561) asks the invoking decision what
		// it may retrieve, and today this role's answer is unauthorized,
		// which it reads as "cannot widen" and lets through. With
		// OperationRetrieve here the check becomes live for worker
		// credentials: any worker whose permitted sources or policies are
		// wider than an action's scope would be refused at invoke unless
		// its executor confines retrieval. Whoever adds retrieve must handle
		// that first.
		return operation == OperationExecute ||
			operation == OperationAgentResolve ||
			operation == OperationValidate
	case ServiceRoleActionApproval:
		return operation == OperationActionApprove ||
			operation == OperationValidate
	case ServiceRoleActionDispatch:
		return operation == OperationDispatch || operation == OperationValidate
	case ServiceRoleDelegation:
		return operation == OperationDelegate || operation == OperationValidate
	case ServiceRoleAgentRegistration:
		return operation == OperationAgentRegister ||
			operation == OperationAgentHeartbeat ||
			operation == OperationValidate
	case ServiceRoleAgentRevocation:
		return operation == OperationAgentRevoke || operation == OperationValidate
	case ServiceRoleAgentResolution:
		return operation == OperationAgentResolve || operation == OperationValidate
	case ServiceRoleSubscription:
		return operation == OperationSubscriptionCreate ||
			operation == OperationSubscriptionDelete ||
			operation == OperationSubscriptionDeliver ||
			operation == OperationValidate
	case ServiceRoleEventPublication:
		return operation == OperationEventPublish || operation == OperationValidate
	case ServiceRoleAnalytics:
		return operation == OperationAnalyticsRead ||
			operation == OperationRetrieve ||
			operation == OperationValidate
	case ServiceRoleTeamOverview:
		return operation == OperationTeamOverviewRead ||
			operation == OperationValidate
	case ServiceRoleWorkspaceSettingsRead:
		return operation == OperationWorkspaceSettingsRead ||
			operation == OperationValidate
	case ServiceRoleWorkspaceSettingsWrite:
		return operation == OperationWorkspaceSettingsWrite ||
			operation == OperationValidate
	default:
		return false
	}
}

func isLegacyMigrationOperation(operation Operation) bool {
	switch operation {
	case OperationIngest, OperationList, OperationRead, OperationConnect,
		OperationNeighborhood, OperationRetrieve, OperationValidate:
		return true
	default:
		return false
	}
}

func roleRequiresServiceVisibility(role ServiceRole) bool {
	return role != ServiceRoleDataRead && role != ServiceRoleDataWrite
}

// ResourceRequest is an untrusted logical resource description. Metadata and
// scope are cloned and validated but never interpreted as authorization grants.
type ResourceRequest struct {
	AuthorizationDomain []byte
	SourceID            []byte
	PolicyID            []byte
	ObjectID            shoal.ID
	Metadata            shoal.Metadata
	Scope               []shoal.ID
}

// Normalize returns an independently owned, validated resource request.
func (r ResourceRequest) Normalize() (ResourceRequest, error) {
	if err := validatePolicyComponent(
		"authorization domain", r.AuthorizationDomain, true,
	); err != nil {
		return ResourceRequest{}, err
	}
	if err := validatePolicyComponent("source identity", r.SourceID, false); err != nil {
		return ResourceRequest{}, err
	}
	if err := validatePolicyComponent("policy identity", r.PolicyID, false); err != nil {
		return ResourceRequest{}, err
	}
	if err := shoal.ValidateOptionalID("object ID", r.ObjectID); err != nil {
		return ResourceRequest{}, err
	}
	if err := shoal.ValidateMetadata("resource metadata", r.Metadata); err != nil {
		return ResourceRequest{}, err
	}
	if len(r.Scope) > retrieval.MaxScopeIDs {
		return ResourceRequest{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "resource scope has too many IDs")
	}

	normalized := ResourceRequest{
		AuthorizationDomain: cloneBytes(r.AuthorizationDomain),
		SourceID:            cloneBytes(r.SourceID),
		PolicyID:            cloneBytes(r.PolicyID),
		ObjectID:            r.ObjectID,
		Metadata:            cloneMetadata(r.Metadata),
		Scope:               make([]shoal.ID, 0, len(r.Scope)),
	}
	seen := make(map[shoal.ID]struct{}, len(r.Scope))
	for _, id := range r.Scope {
		if err := shoal.ValidateRequiredID("resource scope ID", id); err != nil {
			return ResourceRequest{}, err
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		normalized.Scope = append(normalized.Scope, id)
	}
	return normalized, nil
}

// Validate checks resource invariants.
func (r ResourceRequest) Validate() error {
	_, err := r.Normalize()
	return err
}

func validatePolicyComponent(name string, value []byte, required bool) error {
	if len(value) == 0 {
		if required {
			return shoal.NewError(shoal.ErrorInvalidArgument, name+" is required")
		}
		return nil
	}
	if len(value) > MaxPolicyComponentBytes {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, name+" exceeds the policy component byte bound")
	}
	return nil
}

func validateOptionalSemantic(name, value string) error {
	if value == "" {
		return nil
	}
	if !utf8.ValidString(value) || strings.TrimSpace(value) == "" {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, name+" must be valid nonblank UTF-8")
	}
	return shoal.ValidateSemanticString(name, value)
}

func cloneBytes(value []byte) []byte {
	if value == nil {
		return nil
	}
	return append([]byte(nil), value...)
}

func cloneByteSlices(values [][]byte) [][]byte {
	if values == nil {
		return nil
	}
	cloned := make([][]byte, len(values))
	for index, value := range values {
		cloned[index] = cloneBytes(value)
	}
	return cloned
}

func cloneMetadata(value shoal.Metadata) shoal.Metadata {
	if value == nil {
		return nil
	}
	cloned := make(shoal.Metadata, len(value))
	for key, item := range value {
		cloned[key] = item
	}
	return cloned
}

func sortedUniqueBytes(
	name string,
	values [][]byte,
	requiredComponents bool,
) ([][]byte, error) {
	cloned := cloneByteSlices(values)
	for _, value := range cloned {
		if err := validatePolicyComponent(name, value, requiredComponents); err != nil {
			return nil, err
		}
	}
	sort.Slice(cloned, func(left, right int) bool {
		return bytes.Compare(cloned[left], cloned[right]) < 0
	})
	result := cloned[:0]
	for _, value := range cloned {
		if len(result) > 0 && bytes.Equal(result[len(result)-1], value) {
			continue
		}
		result = append(result, value)
	}
	if len(result) == 0 {
		return nil, nil
	}
	return result, nil
}

func containsBytes(sorted [][]byte, value []byte) bool {
	index := sort.Search(len(sorted), func(index int) bool {
		return bytes.Compare(sorted[index], value) >= 0
	})
	return index < len(sorted) && bytes.Equal(sorted[index], value)
}

func contextFailure(ctx context.Context) error {
	if ctx == nil {
		return shoal.NewError(shoal.ErrorInvalidArgument, "context is required")
	}
	switch err := ctx.Err(); {
	case errors.Is(err, context.Canceled):
		return shoal.WrapError(shoal.ErrorCanceled, "operation canceled", err)
	case errors.Is(err, context.DeadlineExceeded):
		return shoal.WrapError(shoal.ErrorDeadline, "operation deadline exceeded", err)
	default:
		return nil
	}
}
