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
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/accumulo"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// visibilityUniverse is every (domain, source, policy) the equivalence tests
// enumerate. The decisions below hold domain-secret, source-a/b and
// policy-a/b, so every axis has members inside and outside the grants.
var (
	visibilityDomains  = []string{"domain-secret", "domain-other"}
	visibilitySources  = []string{"source-a", "source-b", "source-c"}
	visibilityPolicies = []string{"policy-a", "policy-b", "policy-c"}
)

const visibilityEpoch = 7

type universePolicy struct {
	domain, source, grant string
	role                  auth.ServiceRole
	policy                auth.Policy
}

// policyAt builds a real auth.Policy. A service role is attached by decoding
// the canonical expression, the same path a stored service policy takes, so a
// service policy can be built for pairs outside any decision.
func policyAt(
	t *testing.T, domain, source, grant string, epoch int64, role auth.ServiceRole,
) auth.Policy {
	t.Helper()
	policy := mustPolicy(t, auth.PolicyConfig{
		AuthorizationDomain: []byte(domain),
		SourceID:            []byte(source),
		GrantPolicyID:       []byte(grant),
		Epoch:               epoch,
	})
	if role == "" {
		return policy
	}
	encoded, err := policy.Encode()
	if err != nil {
		t.Fatalf("Encode() = %v", err)
	}
	visibility, err := accumulo.NewColumnVisibility(
		[]byte(string(encoded) + "&svc:" + string(role)))
	if err != nil {
		t.Fatalf("NewColumnVisibility() = %v", err)
	}
	decoded, err := auth.DecodePolicy(visibility.Flatten())
	if err != nil {
		t.Fatalf("DecodePolicy(service) = %v", err)
	}
	return decoded
}

// policyLabels is the stored-term form of a policy: its canonical expression
// split into its conjunction.
func policyLabels(t *testing.T, policy auth.Policy) []string {
	t.Helper()
	encoded, err := policy.Encode()
	if err != nil {
		t.Fatalf("Encode() = %v", err)
	}
	return auth.SplitVisibilityConjunction(string(encoded))
}

func universe(t *testing.T, epoch int64, roles ...auth.ServiceRole) []universePolicy {
	t.Helper()
	roles = append([]auth.ServiceRole{""}, roles...)
	var policies []universePolicy
	for _, domain := range visibilityDomains {
		for _, source := range visibilitySources {
			for _, grant := range visibilityPolicies {
				for _, role := range roles {
					policies = append(policies, universePolicy{
						domain: domain, source: source, grant: grant, role: role,
						policy: policyAt(t, domain, source, grant, epoch, role),
					})
				}
			}
		}
	}
	return policies
}

func encoded(t *testing.T, value string) string {
	t.Helper()
	out, err := auth.EncodeComponent([]byte(value))
	if err != nil {
		t.Fatalf("EncodeComponent(%q) = %v", value, err)
	}
	return out
}

func grantLabel(t *testing.T, grant string, epoch int) string {
	t.Helper()
	return "g:" + encoded(t, grant) + ":e:" + strconv.Itoa(epoch)
}

// fullCeilingLabels holds every label of the universe at epoch, plus the role.
func fullCeilingLabels(t *testing.T, epoch int, role auth.ServiceRole) []string {
	t.Helper()
	labels := []string{"svc:" + string(role)}
	for _, domain := range visibilityDomains {
		labels = append(labels, "d:"+encoded(t, domain))
	}
	for _, source := range visibilitySources {
		labels = append(labels, "s:"+encoded(t, source))
	}
	for _, grant := range visibilityPolicies {
		labels = append(labels, grantLabel(t, grant, epoch))
	}
	return labels
}

func mustCeiling(
	t *testing.T, identity shoal.ID, role auth.ServiceRole, labels []string,
) auth.ServiceCeiling {
	t.Helper()
	ceiling, err := auth.NewServiceCeiling(auth.ServiceCeilingConfig{
		Identity:       identity,
		Role:           role,
		Authorizations: accumulo.NewAuthorizationStrings(labels...),
	})
	if err != nil {
		t.Fatalf("NewServiceCeiling() = %v", err)
	}
	return ceiling
}

func userDecisionConfig() auth.DecisionConfig {
	config := baseDecisionConfig()
	config.ServiceRole = ""
	config.ServiceCeilingIdentity = ""
	return config
}

func coordinationDecisionConfig() auth.DecisionConfig {
	config := baseDecisionConfig()
	config.AllowedOperations = []auth.Operation{auth.OperationValidate}
	config.ServiceRole = auth.ServiceRoleCoordination
	config.ServiceCeilingIdentity = "ceiling-coordination"
	return config
}

type serviceCase struct {
	name      string
	decision  auth.Decision
	operation auth.Operation
	ceiling   auth.ServiceCeiling
}

func serviceCases(t *testing.T) []serviceCase {
	t.Helper()
	read := mustDecision(t, baseDecisionConfig())
	coordination := mustDecision(t, coordinationDecisionConfig())
	return []serviceCase{
		{
			name: "data_read full ceiling", decision: read, operation: auth.OperationRead,
			ceiling: mustCeiling(t, "ceiling-read", auth.ServiceRoleDataRead,
				fullCeilingLabels(t, visibilityEpoch, auth.ServiceRoleDataRead)),
		},
		{
			name: "data_read narrow ceiling", decision: read, operation: auth.OperationRetrieve,
			ceiling: mustCeiling(t, "ceiling-read", auth.ServiceRoleDataRead, []string{
				"svc:data_read",
				"d:" + encoded(t, "domain-secret"),
				"s:" + encoded(t, "source-a"),
				grantLabel(t, "policy-a", visibilityEpoch),
				grantLabel(t, "policy-b", visibilityEpoch),
			}),
		},
		{
			name: "coordination full ceiling", decision: coordination,
			operation: auth.OperationValidate,
			ceiling: mustCeiling(t, "ceiling-coordination", auth.ServiceRoleCoordination,
				fullCeilingLabels(t, visibilityEpoch, auth.ServiceRoleCoordination)),
		},
	}
}

func derives(
	decision auth.Decision, operation auth.Operation,
	policy auth.Policy, ceiling auth.ServiceCeiling,
) bool {
	_, err := auth.DeriveScannerAuthorizations(decision, operation, policy, ceiling, testNow)
	return err == nil
}

func serviceVisible(
	t *testing.T, decision auth.Decision, labels []string, ceiling auth.ServiceCeiling,
) bool {
	t.Helper()
	got, err := auth.VisibilityPermittedForService(decision, labels, ceiling, testNow)
	if err != nil {
		t.Fatalf("VisibilityPermittedForService(%v) = %v", labels, err)
	}
	return got
}

func userVisible(t *testing.T, decision auth.Decision, labels []string) bool {
	t.Helper()
	got, err := auth.VisibilityPermittedForUser(decision, labels, testNow)
	if err != nil {
		t.Fatalf("VisibilityPermittedForUser(%v) = %v", labels, err)
	}
	return got
}

// For a trusted service holding the operation, VisibilityPermittedForService
// over the labels of a policy is exactly "DeriveScannerAuthorizations covers
// that policy", over every (domain, source, policy) pair, with and without
// service visibility, at the ceiling's epoch and at older and newer ones.
func TestVisibilityPermittedForServiceEqualsScannerDerivation(t *testing.T) {
	for _, tc := range serviceCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			permitted, refused := 0, 0
			var entries []universePolicy
			for _, epoch := range []int64{visibilityEpoch - 1, visibilityEpoch, visibilityEpoch + 1} {
				entries = append(entries, universe(t, epoch,
					auth.ServiceRoleDataRead, auth.ServiceRoleCoordination)...)
			}
			for _, entry := range entries {
				want := derives(tc.decision, tc.operation, entry.policy, tc.ceiling)
				got := serviceVisible(t, tc.decision, policyLabels(t, entry.policy), tc.ceiling)
				if got != want {
					t.Fatalf("%s/%s/%s svc=%q: visible = %v, derivation = %v",
						entry.domain, entry.source, entry.grant, entry.role, got, want)
				}
				if got {
					permitted++
				} else {
					refused++
				}
			}
			if permitted == 0 || refused == 0 {
				t.Fatalf("vacuous table: permitted=%d refused=%d", permitted, refused)
			}
		})
	}
}

// The documented divergence: visibility is coarser than authorization and
// never consults the operation. DeriveScannerAuthorizations preflights the
// decision's allowed operations and the role's operations; visibility does
// not, because the consumer has already authorized the operation on the
// record. So a decision that does not hold the operation is refused by the
// derivation and permitted exactly where the same grants holding it would be.
func TestVisibilityPermittedForServiceIgnoresOperation(t *testing.T) {
	ceiling := mustCeiling(t, "ceiling-read", auth.ServiceRoleDataRead,
		fullCeilingLabels(t, visibilityEpoch, auth.ServiceRoleDataRead))
	holds := mustDecision(t, baseDecisionConfig())
	lacksConfig := baseDecisionConfig()
	lacksConfig.AllowedOperations = []auth.Operation{auth.OperationValidate}
	lacks := mustDecision(t, lacksConfig)

	diverged := 0
	for _, entry := range universe(t, visibilityEpoch, auth.ServiceRoleDataRead) {
		labels := policyLabels(t, entry.policy)
		reference := derives(holds, auth.OperationRead, entry.policy, ceiling)

		// The decision does not hold read.
		if derives(lacks, auth.OperationRead, entry.policy, ceiling) {
			t.Fatal("derivation permitted an operation the decision lacks")
		}
		// The role does not allow ingest, though nothing else changed.
		if derives(holds, auth.OperationIngest, entry.policy, ceiling) {
			t.Fatal("derivation permitted an operation the role lacks")
		}
		if got := serviceVisible(t, lacks, labels, ceiling); got != reference {
			t.Fatalf("%s/%s/%s: operation-less visibility = %v, want %v",
				entry.domain, entry.source, entry.grant, got, reference)
		}
		if got := serviceVisible(t, holds, labels, ceiling); got != reference {
			t.Fatalf("%s/%s/%s: visibility = %v, want %v",
				entry.domain, entry.source, entry.grant, got, reference)
		}
		if reference {
			diverged++
		}
	}
	if diverged == 0 {
		t.Fatal("no case exercised the operation divergence")
	}

	// A decision holding a different single operation, and a role (coordination)
	// that allows no data operation at all: the derivation refuses every data
	// operation, and visibility still answers from the grants alone.
	retrieveOnlyConfig := baseDecisionConfig()
	retrieveOnlyConfig.AllowedOperations = []auth.Operation{auth.OperationRetrieve}
	retrieveOnly := mustDecision(t, retrieveOnlyConfig)
	coordination := mustDecision(t, coordinationDecisionConfig())
	coordinationCeiling := mustCeiling(t, "ceiling-coordination", auth.ServiceRoleCoordination,
		fullCeilingLabels(t, visibilityEpoch, auth.ServiceRoleCoordination))
	readable := policyAt(t, "domain-secret", "source-a", "policy-a", visibilityEpoch, "")
	coordinated := policyAt(t, "domain-secret", "source-a", "policy-a",
		visibilityEpoch, auth.ServiceRoleCoordination)
	for _, operation := range []auth.Operation{
		auth.OperationRead, auth.OperationList, auth.OperationValidate,
	} {
		if derives(retrieveOnly, operation, readable, ceiling) {
			t.Fatalf("derivation permitted %s to a retrieve-only decision", operation)
		}
	}
	if !serviceVisible(t, retrieveOnly, policyLabels(t, readable), ceiling) {
		t.Fatal("retrieve-only decision refused visibility")
	}
	for _, operation := range []auth.Operation{
		auth.OperationRead, auth.OperationRetrieve, auth.OperationIngest,
	} {
		if derives(coordination, operation, coordinated, coordinationCeiling) {
			t.Fatalf("derivation permitted %s to a coordination role", operation)
		}
	}
	if !derives(coordination, auth.OperationValidate, coordinated, coordinationCeiling) {
		t.Fatal("coordination derivation refused its own operation")
	}
	if !serviceVisible(t, coordination, policyLabels(t, coordinated), coordinationCeiling) {
		t.Fatal("coordination role refused visibility")
	}
}

// The same holds for users: a user decision lacking every data operation still
// sees what its grants cover.
func TestVisibilityPermittedForUserIgnoresOperation(t *testing.T) {
	config := userDecisionConfig()
	labels := policyLabels(t, policyAt(t, "domain-secret", "source-a", "policy-a", 7, ""))
	for _, operation := range []auth.Operation{
		auth.OperationWorkspaceSettingsRead, auth.OperationRead, auth.OperationValidate,
	} {
		config.AllowedOperations = []auth.Operation{operation}
		if !userVisible(t, mustDecision(t, config), labels) {
			t.Fatalf("user visibility consulted the operation (holding only %s)", operation)
		}
	}
}

// DeriveScannerAuthorizations refuses every user decision (ceiling.go requires
// a trusted service), so it cannot be the oracle for the readers this exists
// for: every hosted reader is a user. The expected table is instead the
// decision's own grant check — the domain, source and policy checks
// authorizeResource makes, which is what workspace.authorizeOutputPolicy
// applies to a user — and a user never sees service-only data.
func TestVisibilityPermittedForUserExpectedTable(t *testing.T) {
	decision := mustDecision(t, userDecisionConfig())
	anyCeiling := mustCeiling(t, "ceiling-read", auth.ServiceRoleDataRead,
		fullCeilingLabels(t, visibilityEpoch, auth.ServiceRoleDataRead))
	granted := func(values []string, value string) bool {
		for _, candidate := range values {
			if candidate == value {
				return true
			}
		}
		return false
	}
	permitted := 0
	for _, entry := range universe(t, visibilityEpoch, auth.ServiceRoleDataRead) {
		want := entry.domain == "domain-secret" &&
			granted([]string{"source-a", "source-b"}, entry.source) &&
			granted([]string{"policy-a", "policy-b"}, entry.grant) &&
			entry.role == ""
		if got := userVisible(t, decision, policyLabels(t, entry.policy)); got != want {
			t.Fatalf("%s/%s/%s svc=%q: visible = %v, want %v",
				entry.domain, entry.source, entry.grant, entry.role, got, want)
		}
		if derives(decision, auth.OperationRead, entry.policy, anyCeiling) {
			t.Fatal("derivation accepted a user decision")
		}
		if want {
			permitted++
		}
	}
	if permitted != 4 {
		t.Fatalf("permitted = %d, want 4", permitted)
	}
}

func TestVisibilityPermittedEpochs(t *testing.T) {
	user := mustDecision(t, userDecisionConfig())
	service := mustDecision(t, baseDecisionConfig())
	ceiling := mustCeiling(t, "ceiling-read", auth.ServiceRoleDataRead,
		fullCeilingLabels(t, visibilityEpoch, auth.ServiceRoleDataRead))
	at := func(epoch int64) (auth.Policy, []string) {
		policy := policyAt(t, "domain-secret", "source-a", "policy-a", epoch, "")
		return policy, policyLabels(t, policy)
	}

	// Users: a decision carries logical grants only, and AccessRule.Authorize
	// does not let physical epochs participate, so every epoch passes.
	for _, epoch := range []int64{1, visibilityEpoch - 1, visibilityEpoch, visibilityEpoch + 1} {
		if _, labels := at(epoch); !userVisible(t, user, labels) {
			t.Fatalf("user epoch %d refused", epoch)
		}
	}

	// Services: ceiling membership is exact for every term, as at the tablet
	// and in DeriveScannerAuthorizations. Neither an older nor a newer epoch
	// than the ceiling holds is within it, and the answer equals the
	// derivation at every epoch.
	for _, epoch := range []int64{1, visibilityEpoch - 1, visibilityEpoch, visibilityEpoch + 1} {
		policy, labels := at(epoch)
		want := epoch == visibilityEpoch
		if got := serviceVisible(t, service, labels, ceiling); got != want {
			t.Fatalf("service epoch %d against ceiling e:%d = %v, want %v",
				epoch, visibilityEpoch, got, want)
		}
		if got := derives(service, auth.OperationRead, policy, ceiling); got != want {
			t.Fatalf("derivation epoch %d = %v, want %v", epoch, got, want)
		}
	}

	// Revocation: the operator rotated policy-a to e:7 and removed e:3 from
	// the account. The tablet refuses e:3 cells, so a stored e:3 record is
	// refused too, although the decision still grants policy-a logically and
	// a user holding policy-a still sees it.
	rotated := mustCeiling(t, "ceiling-read", auth.ServiceRoleDataRead, []string{
		"svc:data_read",
		"d:" + encoded(t, "domain-secret"),
		"s:" + encoded(t, "source-a"),
		grantLabel(t, "policy-a", 7),
	})
	revoked, revokedLabels := at(3)
	if serviceVisible(t, service, revokedLabels, rotated) {
		t.Fatal("revoked epoch e:3 admitted by a ceiling holding only e:7")
	}
	if derives(service, auth.OperationRead, revoked, rotated) {
		t.Fatal("derivation admitted a revoked epoch")
	}
	if !userVisible(t, user, revokedLabels) {
		t.Fatal("the user grant check must stay epoch-agnostic")
	}

	// Another policy's epoch in the ceiling does not cover this one.
	other := mustCeiling(t, "ceiling-read", auth.ServiceRoleDataRead, []string{
		"svc:data_read",
		"d:" + encoded(t, "domain-secret"),
		"s:" + encoded(t, "source-a"),
		grantLabel(t, "policy-a", 3),
		grantLabel(t, "policy-b", 99),
	})
	if _, labels := at(4); serviceVisible(t, service, labels, other) {
		t.Fatal("another policy's epoch covered this policy")
	}
	if _, labels := at(3); !serviceVisible(t, service, labels, other) {
		t.Fatal("exact ceiling epoch refused")
	}
}

func TestVisibilityPermittedUnlabelledSet(t *testing.T) {
	user := mustDecision(t, userDecisionConfig())
	service := mustDecision(t, baseDecisionConfig())
	ceiling := mustCeiling(t, "ceiling-read", auth.ServiceRoleDataRead,
		fullCeilingLabels(t, visibilityEpoch, auth.ServiceRoleDataRead))
	for _, labels := range [][]string{nil, {}, auth.SplitVisibilityConjunction("")} {
		if !userVisible(t, user, labels) {
			t.Fatalf("user unlabelled %#v refused", labels)
		}
		if !serviceVisible(t, service, labels, ceiling) {
			t.Fatalf("service unlabelled %#v refused", labels)
		}
	}

	// Unlabelled data is visible to every role, as unlabelled cells are at
	// the tablet: the own-svc:<role> requirement applies only to a non-empty
	// set. Every role is checked, including those that require it.
	roles := []auth.ServiceRole{
		auth.ServiceRoleDataRead, auth.ServiceRoleDataWrite,
		auth.ServiceRoleCoordination, auth.ServiceRoleDerivation,
		auth.ServiceRoleMigration, auth.ServiceRoleSecurityAdmin,
		auth.ServiceRoleActionInvocation, auth.ServiceRoleActionExecution,
		auth.ServiceRoleActionApproval, auth.ServiceRoleActionDispatch,
		auth.ServiceRoleDelegation, auth.ServiceRoleAgentRegistration,
		auth.ServiceRoleAgentRevocation, auth.ServiceRoleAgentResolution,
		auth.ServiceRoleSubscription, auth.ServiceRoleEventPublication,
		auth.ServiceRoleAnalytics, auth.ServiceRoleTeamOverview,
		auth.ServiceRoleWorkspaceSettingsRead, auth.ServiceRoleWorkspaceSettingsWrite,
	}
	labelled := policyLabels(t, policyAt(t, "domain-secret", "source-a", "policy-a",
		visibilityEpoch, ""))
	for _, role := range roles {
		config := baseDecisionConfig()
		config.AllowedOperations = []auth.Operation{auth.OperationValidate}
		config.ServiceRole = role
		config.ServiceCeilingIdentity = shoal.ID("ceiling-" + strings.ReplaceAll(string(role), "_", "-"))
		if role == auth.ServiceRoleActionExecution {
			// An execution decision must be bound to one executor and
			// cannot act on behalf of another identity.
			config.ExecutorBinding = "executor-a"
			config.OnBehalfOf = nil
		}
		decision := mustDecision(t, config)
		roleCeiling := mustCeiling(t, config.ServiceCeilingIdentity, role,
			fullCeilingLabels(t, visibilityEpoch, role))
		if !serviceVisible(t, decision, nil, roleCeiling) {
			t.Fatalf("role %s refused unlabelled data", role)
		}
		// The same role's own-svc requirement still binds a labelled set.
		requiresService := role != auth.ServiceRoleDataRead && role != auth.ServiceRoleDataWrite
		if got := serviceVisible(t, decision, labelled, roleCeiling); got == requiresService {
			t.Fatalf("role %s labelled set without svc term = %v", role, got)
		}
	}
}

// Free-form and malformed terms are false and never an error, alone or
// alongside terms the reader holds.
func TestVisibilityPermittedRefusesFreeFormAndMalformedTerms(t *testing.T) {
	user := mustDecision(t, userDecisionConfig())
	service := mustDecision(t, baseDecisionConfig())
	ceiling := mustCeiling(t, "ceiling-read", auth.ServiceRoleDataRead,
		fullCeilingLabels(t, visibilityEpoch, auth.ServiceRoleDataRead))
	domain := "d:" + encoded(t, "domain-secret")
	source := "s:" + encoded(t, "source-a")
	grant := grantLabel(t, "policy-a", visibilityEpoch)
	held := []string{domain, grant, source}
	if !userVisible(t, user, held) || !serviceVisible(t, service, held, ceiling) {
		t.Fatal("held structured labels refused")
	}
	bad := []string{
		"secret", "PUBLIC", "", " ", "d:", "s:", "g:", "svc:",
		"d:" + strings.ToUpper(encoded(t, "domain-secret")),
		domain + "=", " " + domain, domain + " ",
		domain + "|" + source, "(" + domain + ")", "(" + domain, source + ")",
		"g:" + encoded(t, "policy-a"),
		"g:" + encoded(t, "policy-a") + ":e:0",
		"g:" + encoded(t, "policy-a") + ":e:-1",
		"g:" + encoded(t, "policy-a") + ":e:07",
		"g:" + encoded(t, "policy-a") + ":e:",
		"g:" + encoded(t, "policy-a") + ":e:9223372036854775808",
		"svc:root", "svc:DATA_READ",
		"x:" + encoded(t, "domain-secret"),
	}
	for _, term := range bad {
		for _, labels := range [][]string{{term}, append(append([]string(nil), held...), term)} {
			got, err := auth.VisibilityPermittedForUser(user, labels, testNow)
			if err != nil || got {
				t.Fatalf("user %q = %v, %v; want false, nil", term, got, err)
			}
			got, err = auth.VisibilityPermittedForService(service, labels, ceiling, testNow)
			if err != nil || got {
				t.Fatalf("service %q = %v, %v; want false, nil", term, got, err)
			}
		}
	}
}

func TestSplitVisibilityConjunctionKeepsUnionAndGroupingMalformed(t *testing.T) {
	cases := map[string][]string{
		"":          nil,
		"a":         {"a"},
		"a&b":       {"a", "b"},
		"a|b":       {"a|b"},
		"(a&b)":     {"(a", "b)"},
		"(a|b)&c":   {"(a|b)", "c"},
		"a&&b":      {"a", "", "b"},
		"&a":        {"", "a"},
		"a&":        {"a", ""},
		" a & b ":   {" a ", " b "},
		"\"a\"&b":   {"\"a\"", "b"},
		"a&b|c&d":   {"a", "b|c", "d"},
		"((a))&(b)": {"((a))", "(b)"},
	}
	for expression, want := range cases {
		if got := auth.SplitVisibilityConjunction(expression); !reflect.DeepEqual(got, want) {
			t.Fatalf("split(%q) = %#v, want %#v", expression, got, want)
		}
	}

	// A disjunction or grouping of terms the reader holds is still refused.
	user := mustDecision(t, userDecisionConfig())
	domain := "d:" + encoded(t, "domain-secret")
	source := "s:" + encoded(t, "source-a")
	for _, expression := range []string{
		domain + "|" + source,
		"(" + domain + "&" + source + ")",
		"(" + domain + ")&" + source,
		domain + "&&" + source,
		domain + "&" + source + "&",
	} {
		if userVisible(t, user, auth.SplitVisibilityConjunction(expression)) {
			t.Fatalf("expression %q permitted", expression)
		}
	}
	if !userVisible(t, user, auth.SplitVisibilityConjunction(domain+"&"+source)) {
		t.Fatal("canonical conjunction refused")
	}
}

func TestVisibilityPermittedForServiceCeilingBounds(t *testing.T) {
	service := mustDecision(t, baseDecisionConfig())
	labels := policyLabels(t, policyAt(t, "domain-secret", "source-b", "policy-b", 7, ""))
	full := mustCeiling(t, "ceiling-read", auth.ServiceRoleDataRead,
		fullCeilingLabels(t, visibilityEpoch, auth.ServiceRoleDataRead))
	if !serviceVisible(t, service, labels, full) {
		t.Fatal("covered labels refused")
	}
	// The decision grants source-b, but the ceiling does not hold it.
	lacking := mustCeiling(t, "ceiling-read", auth.ServiceRoleDataRead, []string{
		"svc:data_read",
		"d:" + encoded(t, "domain-secret"),
		"s:" + encoded(t, "source-a"),
		grantLabel(t, "policy-b", visibilityEpoch),
	})
	if serviceVisible(t, service, labels, lacking) {
		t.Fatal("ceiling skipped: a label outside the ceiling was permitted")
	}
	// An unconfigured ceiling, or one the decision does not name, is false.
	if serviceVisible(t, service, labels, auth.ServiceCeiling{}) {
		t.Fatal("zero ceiling permitted")
	}
	otherIdentity := mustCeiling(t, "ceiling-other", auth.ServiceRoleDataRead,
		fullCeilingLabels(t, visibilityEpoch, auth.ServiceRoleDataRead))
	if serviceVisible(t, service, labels, otherIdentity) {
		t.Fatal("another account's ceiling permitted")
	}
	otherRole := mustCeiling(t, "ceiling-read", auth.ServiceRoleDataWrite,
		fullCeilingLabels(t, visibilityEpoch, auth.ServiceRoleDataWrite))
	if serviceVisible(t, service, labels, otherRole) {
		t.Fatal("another role's ceiling permitted")
	}
	// svc:<role> passes only for the decision's own role.
	if serviceVisible(t, service, append(labels, "svc:data_write"), full) {
		t.Fatal("another role's service label permitted")
	}
}

func TestVisibilityPermittedModeAndDecisionErrors(t *testing.T) {
	user := mustDecision(t, userDecisionConfig())
	service := mustDecision(t, baseDecisionConfig())
	ceiling := mustCeiling(t, "ceiling-read", auth.ServiceRoleDataRead,
		fullCeilingLabels(t, visibilityEpoch, auth.ServiceRoleDataRead))
	expired := testNow.Add(2 * time.Hour)
	unauthorized := func(name string, err error) {
		t.Helper()
		if !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
			t.Fatalf("%s = %v, want unauthorized", name, err)
		}
	}
	for _, labels := range [][]string{nil, {"secret"}} {
		_, err := auth.VisibilityPermittedForUser(service, labels, testNow)
		unauthorized("user mode with trusted service", err)
		_, err = auth.VisibilityPermittedForService(user, labels, ceiling, testNow)
		unauthorized("service mode with user", err)
		_, err = auth.VisibilityPermittedForUser(auth.Decision{}, labels, testNow)
		unauthorized("user invalid decision", err)
		_, err = auth.VisibilityPermittedForService(auth.Decision{}, labels, ceiling, testNow)
		unauthorized("service invalid decision", err)
		_, err = auth.VisibilityPermittedForUser(user, labels, expired)
		unauthorized("user expired", err)
		_, err = auth.VisibilityPermittedForService(service, labels, ceiling, expired)
		unauthorized("service expired", err)
		_, err = auth.VisibilityPermittedForUser(user, labels, user.AuthenticationExpires())
		unauthorized("user at expiry", err)
		_, err = auth.VisibilityPermittedForUser(user, labels, time.Time{})
		unauthorized("user zero time", err)
		_, err = auth.VisibilityPermittedForService(service, labels, auth.ServiceCeiling{}, expired)
		unauthorized("service expired with zero ceiling", err)
	}
}
