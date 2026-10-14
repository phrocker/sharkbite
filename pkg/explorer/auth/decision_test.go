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
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

var testNow = time.Date(2026, 8, 26, 20, 0, 0, 0, time.UTC)

func baseDecisionConfig() auth.DecisionConfig {
	return auth.DecisionConfig{
		Subject:                "subject-secret",
		Actor:                  "actor-secret",
		ClientID:               "client-secret",
		OnBehalfOf:             []shoal.ID{"delegate-one", "delegate-two"},
		AuthorizationDomain:    []byte("domain-secret"),
		AllowedOperations:      []auth.Operation{auth.OperationRead, auth.OperationRetrieve},
		PermittedSourceIDs:     [][]byte{[]byte("source-a"), []byte("source-b")},
		PermittedPolicyIDs:     [][]byte{[]byte("policy-a"), []byte("policy-b")},
		PolicyGeneration:       7,
		AuthenticationExpires:  testNow.Add(time.Hour),
		RequestID:              "request-secret",
		CorrelationID:          "correlation-secret",
		AuditPurpose:           "support investigation",
		ServiceRole:            auth.ServiceRoleDataRead,
		ServiceCeilingIdentity: "ceiling-read",
	}
}

func mustDecision(t *testing.T, config auth.DecisionConfig) auth.Decision {
	t.Helper()
	decision, err := auth.NewDecision(config)
	if err != nil {
		t.Fatalf("NewDecision() = %v", err)
	}
	return decision
}

func TestOperationAndServiceRoleValidation(t *testing.T) {
	operations := []auth.Operation{
		auth.OperationIngest,
		auth.OperationList,
		auth.OperationRead,
		auth.OperationConnect,
		auth.OperationNeighborhood,
		auth.OperationRetrieve,
		auth.OperationValidate,
	}
	for _, operation := range operations {
		if err := operation.Validate(); err != nil {
			t.Fatalf("%q.Validate() = %v", operation, err)
		}
	}
	if err := auth.Operation("admin").Validate(); !shoal.IsErrorCode(
		err, shoal.ErrorInvalidArgument,
	) {
		t.Fatalf("unknown operation error = %v", err)
	}

	matrix := map[auth.ServiceRole]map[auth.Operation]bool{
		auth.ServiceRoleDataRead: {
			auth.OperationList:         true,
			auth.OperationRead:         true,
			auth.OperationNeighborhood: true,
			auth.OperationRetrieve:     true,
			auth.OperationValidate:     true,
		},
		auth.ServiceRoleDataWrite: {
			auth.OperationIngest:   true,
			auth.OperationConnect:  true,
			auth.OperationValidate: true,
		},
		auth.ServiceRoleCoordination: {
			auth.OperationValidate: true,
		},
		auth.ServiceRoleDerivation: {
			auth.OperationRead:     true,
			auth.OperationConnect:  true,
			auth.OperationValidate: true,
		},
		auth.ServiceRoleMigration: {
			auth.OperationIngest:       true,
			auth.OperationList:         true,
			auth.OperationRead:         true,
			auth.OperationConnect:      true,
			auth.OperationNeighborhood: true,
			auth.OperationRetrieve:     true,
			auth.OperationValidate:     true,
		},
		auth.ServiceRoleSecurityAdmin: {
			auth.OperationValidate: true,
		},
	}
	for role, allowed := range matrix {
		if err := role.Validate(); err != nil {
			t.Fatalf("%q.Validate() = %v", role, err)
		}
		for _, operation := range operations {
			if got, want := role.Allows(operation), allowed[operation]; got != want {
				t.Errorf("%q.Allows(%q) = %t, want %t", role, operation, got, want)
			}
		}
		if role.Allows(auth.Operation("invalid")) {
			t.Errorf("%q permits an invalid operation", role)
		}
	}
	for _, role := range []auth.ServiceRole{"", "invalid"} {
		if err := role.Validate(); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
			t.Errorf("%q.Validate() = %v", role, err)
		}
		for _, operation := range operations {
			if role.Allows(operation) {
				t.Errorf("%q unexpectedly permits %q", role, operation)
			}
		}
	}
}

func TestDecisionEnforcesServiceRoleOperationMatrix(t *testing.T) {
	operations := []auth.Operation{
		auth.OperationIngest,
		auth.OperationList,
		auth.OperationRead,
		auth.OperationConnect,
		auth.OperationNeighborhood,
		auth.OperationRetrieve,
		auth.OperationValidate,
	}
	matrix := map[auth.ServiceRole]map[auth.Operation]bool{
		auth.ServiceRoleDataRead: {
			auth.OperationList:         true,
			auth.OperationRead:         true,
			auth.OperationNeighborhood: true,
			auth.OperationRetrieve:     true,
			auth.OperationValidate:     true,
		},
		auth.ServiceRoleDataWrite: {
			auth.OperationIngest:   true,
			auth.OperationConnect:  true,
			auth.OperationValidate: true,
		},
		auth.ServiceRoleCoordination: {
			auth.OperationValidate: true,
		},
		auth.ServiceRoleDerivation: {
			auth.OperationRead:     true,
			auth.OperationConnect:  true,
			auth.OperationValidate: true,
		},
		auth.ServiceRoleMigration: {
			auth.OperationIngest:       true,
			auth.OperationList:         true,
			auth.OperationRead:         true,
			auth.OperationConnect:      true,
			auth.OperationNeighborhood: true,
			auth.OperationRetrieve:     true,
			auth.OperationValidate:     true,
		},
		auth.ServiceRoleSecurityAdmin: {
			auth.OperationValidate: true,
		},
	}
	for role, allowed := range matrix {
		for _, operation := range operations {
			config := baseDecisionConfig()
			config.ServiceRole = role
			config.ServiceCeilingIdentity = "matrix-ceiling"
			config.AllowedOperations = []auth.Operation{operation}
			decision, err := auth.NewDecision(config)
			if allowed[operation] {
				if err != nil {
					t.Errorf("NewDecision(%q, %q) = %v", role, operation, err)
					continue
				}
				resource := auth.ResourceRequest{
					AuthorizationDomain: []byte("domain-secret"),
				}
				if err := decision.Authorize(operation, resource, testNow); err != nil {
					t.Errorf("Authorize(%q, %q) = %v", role, operation, err)
				}
			} else if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
				t.Errorf("NewDecision(%q, %q) = %v, want invalid_argument",
					role, operation, err)
			}
		}
	}

	for _, operation := range operations {
		config := baseDecisionConfig()
		config.ServiceRole = ""
		config.ServiceCeilingIdentity = ""
		config.AllowedOperations = []auth.Operation{operation}
		if _, err := auth.NewDecision(config); err != nil {
			t.Errorf("non-service NewDecision(%q) = %v", operation, err)
		}
	}

	invalid := baseDecisionConfig()
	invalid.ServiceRole = auth.ServiceRole("invalid")
	invalid.AllowedOperations = []auth.Operation{auth.OperationRead}
	if _, err := auth.NewDecision(invalid); !shoal.IsErrorCode(
		err, shoal.ErrorInvalidArgument,
	) {
		t.Fatalf("NewDecision(invalid role) = %v", err)
	}
}

func TestDecisionDefensivelyOwnsGrantMaterial(t *testing.T) {
	config := baseDecisionConfig()
	domain := config.AuthorizationDomain
	operations := config.AllowedOperations
	sources := config.PermittedSourceIDs
	policies := config.PermittedPolicyIDs
	chain := config.OnBehalfOf

	decision := mustDecision(t, config)
	domain[0] = 'X'
	operations[0] = auth.OperationIngest
	sources[0][0] = 'X'
	policies[0][0] = 'X'
	chain[0] = "changed"

	if got := string(decision.AuthorizationDomain()); got != "domain-secret" {
		t.Fatalf("AuthorizationDomain() = %q", got)
	}
	if got := decision.AllowedOperations(); len(got) != 2 ||
		got[0] != auth.OperationRead || got[1] != auth.OperationRetrieve {
		t.Fatalf("AllowedOperations() = %v", got)
	}
	if got := decision.PermittedSourceIDs(); string(got[0]) != "source-a" {
		t.Fatalf("PermittedSourceIDs() = %q", got)
	}
	if got := decision.PermittedPolicyIDs(); string(got[0]) != "policy-a" {
		t.Fatalf("PermittedPolicyIDs() = %q", got)
	}
	if got := decision.OnBehalfOf(); got[0] != "delegate-one" {
		t.Fatalf("OnBehalfOf() = %v", got)
	}

	returnedDomain := decision.AuthorizationDomain()
	returnedSources := decision.PermittedSourceIDs()
	returnedOperations := decision.AllowedOperations()
	returnedChain := decision.OnBehalfOf()
	returnedDomain[0] = 'Y'
	returnedSources[0][0] = 'Y'
	returnedOperations[0] = auth.OperationIngest
	returnedChain[0] = "mutated"
	if string(decision.AuthorizationDomain()) != "domain-secret" ||
		string(decision.PermittedSourceIDs()[0]) != "source-a" ||
		decision.AllowedOperations()[0] != auth.OperationRead ||
		decision.OnBehalfOf()[0] != "delegate-one" {
		t.Fatal("decision getter handed out mutable ownership")
	}
}

func TestResourceNormalizationDoesNotTurnMetadataOrScopeIntoGrants(t *testing.T) {
	decision := mustDecision(t, baseDecisionConfig())
	metadata := shoal.Metadata{"source": "source-a", "owner": "subject-secret"}
	scope := []shoal.ID{"scope-a", "scope-a"}
	resource := auth.ResourceRequest{
		AuthorizationDomain: []byte("domain-secret"),
		SourceID:            []byte("source-hidden"),
		PolicyID:            []byte("policy-a"),
		ObjectID:            "object-secret",
		Metadata:            metadata,
		Scope:               scope,
	}
	normalized, err := resource.Normalize()
	if err != nil {
		t.Fatalf("Normalize() = %v", err)
	}
	metadata["source"] = "changed"
	scope[0] = "changed"
	resource.AuthorizationDomain[0] = 'X'
	if normalized.Metadata["source"] != "source-a" ||
		normalized.Scope[0] != "scope-a" ||
		string(normalized.AuthorizationDomain) != "domain-secret" {
		t.Fatal("Normalize() did not defensively own resource values")
	}
	if len(normalized.Scope) != 1 {
		t.Fatalf("normalized scope = %v", normalized.Scope)
	}
	if err := decision.Authorize(auth.OperationRead, normalized, testNow); !shoal.IsErrorCode(
		err, shoal.ErrorUnauthorized,
	) {
		t.Fatalf("Authorize() with metadata/scope grant claims = %v", err)
	}
}

func TestAuthorizeFailsClosedAndObjectDenialIsNonDisclosing(t *testing.T) {
	decision := mustDecision(t, baseDecisionConfig())
	allowed := auth.ResourceRequest{
		AuthorizationDomain: []byte("domain-secret"),
		SourceID:            []byte("source-a"),
		PolicyID:            []byte("policy-a"),
		ObjectID:            "object-secret",
	}
	if err := decision.Authorize(auth.OperationRead, allowed, testNow); err != nil {
		t.Fatalf("Authorize(allowed) = %v", err)
	}
	for name, testCase := range map[string]struct {
		operation auth.Operation
		resource  auth.ResourceRequest
		now       time.Time
	}{
		"wrong operation": {auth.OperationIngest, allowed, testNow},
		"wrong domain": {
			auth.OperationRead,
			auth.ResourceRequest{AuthorizationDomain: []byte("other")},
			testNow,
		},
		"expired":   {auth.OperationRead, allowed, testNow.Add(2 * time.Hour)},
		"zero time": {auth.OperationRead, allowed, time.Time{}},
	} {
		if err := decision.Authorize(
			testCase.operation, testCase.resource, testCase.now,
		); !shoal.IsErrorCode(
			err, shoal.ErrorUnauthorized,
		) {
			t.Fatalf("%s: Authorize() = %v", name, err)
		}
	}
	var zero auth.Decision
	if err := zero.Authorize(auth.OperationRead, allowed, testNow); !shoal.IsErrorCode(
		err, shoal.ErrorUnauthorized,
	) {
		t.Fatalf("zero Decision.Authorize() = %v", err)
	}
	if err := zero.Authorize(auth.OperationRead, auth.ResourceRequest{}, testNow); !shoal.IsErrorCode(
		err, shoal.ErrorUnauthorized,
	) {
		t.Fatalf("zero Decision.Authorize(malformed resource) = %v", err)
	}

	hidden := allowed
	hidden.SourceID = []byte("source-hidden")
	ordinaryDenial := decision.Authorize(auth.OperationRead, hidden, testNow)
	if !shoal.IsErrorCode(ordinaryDenial, shoal.ErrorUnauthorized) {
		t.Fatalf("Authorize(hidden) = %v", ordinaryDenial)
	}
	objectDenial := decision.AuthorizeObject(auth.OperationRead, hidden, testNow)
	if !shoal.IsErrorCode(objectDenial, shoal.ErrorNotFound) {
		t.Fatalf("AuthorizeObject(hidden) = %v", objectDenial)
	}
	if objectDenial.Error() != auth.ObjectNotFound().Error() {
		t.Fatalf("hidden error %q differs from absent error %q",
			objectDenial, auth.ObjectNotFound())
	}
	wrongDomain := allowed
	wrongDomain.AuthorizationDomain = []byte("other")
	if err := decision.AuthorizeObject(
		auth.OperationRead, wrongDomain, testNow,
	); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("AuthorizeObject(wrong domain) = %v", err)
	}
}

// TestAuthorizeChecksDomainBeforeSource guards the ordering in
// authorizeResource: the authorization domain is a fail-closed tenancy
// primitive and must be checked before source or policy membership. The
// discriminating request is wrong-domain AND hidden-source at once. Because
// AuthorizeObject maps a domain (request-level) denial to Unauthorized but a
// source (resource-level) denial to a non-disclosing NotFound, domain-first
// ordering yields Unauthorized here; if source were checked first this would
// leak as NotFound. Reordering the two checks makes this test fail.
func TestAuthorizeChecksDomainBeforeSource(t *testing.T) {
	decision := mustDecision(t, baseDecisionConfig())
	wrongDomainHiddenSource := auth.ResourceRequest{
		AuthorizationDomain: []byte("other-domain"),
		SourceID:            []byte("source-hidden"),
		PolicyID:            []byte("policy-a"),
		ObjectID:            "object-secret",
	}
	if err := decision.AuthorizeObject(
		auth.OperationRead, wrongDomainHiddenSource, testNow,
	); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf(
			"AuthorizeObject(wrong domain + hidden source) = %v, want "+
				"Unauthorized (domain checked first)", err,
		)
	}
	// A hidden source within the correct domain is the resource-level case and
	// must instead be a non-disclosing NotFound, confirming the two denial
	// classes are genuinely distinguished rather than collapsed.
	hiddenSourceRightDomain := wrongDomainHiddenSource
	hiddenSourceRightDomain.AuthorizationDomain = []byte("domain-secret")
	if err := decision.AuthorizeObject(
		auth.OperationRead, hiddenSourceRightDomain, testNow,
	); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf(
			"AuthorizeObject(hidden source, right domain) = %v, want NotFound",
			err,
		)
	}
}

func TestDecisionIntersectsRequestedProjectionWithoutDisclosingHiddenGrants(t *testing.T) {
	decision := mustDecision(t, baseDecisionConfig())
	sources, err := decision.IntersectSourceIDs(
		auth.OperationRetrieve,
		[]byte("domain-secret"),
		[][]byte{[]byte("source-hidden"), []byte("source-b"), []byte("source-b")},
		testNow,
	)
	if err != nil {
		t.Fatalf("IntersectSourceIDs() = %v", err)
	}
	if len(sources) != 1 || string(sources[0]) != "source-b" {
		t.Fatalf("source intersection = %q", sources)
	}
	sources[0][0] = 'X'
	allSources, err := decision.IntersectSourceIDs(
		auth.OperationRetrieve, []byte("domain-secret"), nil, testNow)
	if err != nil {
		t.Fatalf("IntersectSourceIDs(all) = %v", err)
	}
	if len(allSources) != 2 || string(allSources[1]) != "source-b" {
		t.Fatalf("all source grants = %q", allSources)
	}

	policies, err := decision.IntersectPolicyIDs(
		auth.OperationRetrieve,
		[]byte("domain-secret"),
		[][]byte{[]byte("policy-b"), []byte("policy-hidden")},
		testNow,
	)
	if err != nil {
		t.Fatalf("IntersectPolicyIDs() = %v", err)
	}
	if len(policies) != 1 || string(policies[0]) != "policy-b" {
		t.Fatalf("policy intersection = %q", policies)
	}
	if _, err := decision.IntersectSourceIDs(
		auth.OperationIngest, []byte("domain-secret"), nil, testNow,
	); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("IntersectSourceIDs(wrong operation) = %v", err)
	}
}

func TestAuthorityCapabilitiesRejectForgingAcrossAuthorities(t *testing.T) {
	now := testNow
	clock := func() time.Time { return now }
	first, err := auth.NewAuthorityWithClock(clock)
	if err != nil {
		t.Fatalf("NewAuthorityWithClock(first) = %v", err)
	}
	second, err := auth.NewAuthorityWithClock(clock)
	if err != nil {
		t.Fatalf("NewAuthorityWithClock(second) = %v", err)
	}
	decision := mustDecision(t, baseDecisionConfig())

	bound, err := first.Binder().Bind(context.Background(), decision)
	if err != nil {
		t.Fatalf("Bind() = %v", err)
	}
	resolved, err := first.Resolver().Resolve(bound)
	if err != nil {
		t.Fatalf("matching Resolve() = %v", err)
	}
	if resolved.Subject() != decision.Subject() {
		t.Fatalf("resolved subject = %q", resolved.Subject())
	}
	if _, err := second.Resolver().Resolve(bound); !shoal.IsErrorCode(
		err, shoal.ErrorUnauthorized,
	) {
		t.Fatalf("cross-authority Resolve() = %v", err)
	}

	forged := context.WithValue(context.Background(), struct{ name string }{"decision"}, decision)
	if _, err := first.Resolver().Resolve(forged); !shoal.IsErrorCode(
		err, shoal.ErrorUnauthorized,
	) {
		t.Fatalf("forged Resolve() = %v", err)
	}
	if _, err := first.Resolver().Resolve(context.Background()); !shoal.IsErrorCode(
		err, shoal.ErrorUnauthorized,
	) {
		t.Fatalf("missing Resolve() = %v", err)
	}
}

func TestResolversRecheckExpiryAndMapCancellation(t *testing.T) {
	now := testNow
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewAuthorityWithClock() = %v", err)
	}
	config := baseDecisionConfig()
	config.AuthenticationExpires = testNow.Add(time.Minute)
	decision := mustDecision(t, config)
	bound, err := authority.Binder().Bind(context.Background(), decision)
	if err != nil {
		t.Fatalf("Bind() = %v", err)
	}
	now = config.AuthenticationExpires
	if _, err := authority.Resolver().Resolve(bound); !shoal.IsErrorCode(
		err, shoal.ErrorUnauthorized,
	) {
		t.Fatalf("Resolve(expired) = %v", err)
	}

	now = testNow
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := authority.Binder().Bind(canceled, decision); !errors.Is(
		err, context.Canceled,
	) || !shoal.IsErrorCode(err, shoal.ErrorCanceled) {
		t.Fatalf("Bind(canceled) = %v", err)
	}
	bound, err = authority.Binder().Bind(context.Background(), decision)
	if err != nil {
		t.Fatalf("Bind(second) = %v", err)
	}
	canceledBound, cancelBound := context.WithCancel(bound)
	cancelBound()
	if _, err := authority.Resolver().Resolve(canceledBound); !errors.Is(
		err, context.Canceled,
	) || !shoal.IsErrorCode(err, shoal.ErrorCanceled) {
		t.Fatalf("Resolve(canceled) = %v", err)
	}

	deadline, cancelDeadline := context.WithDeadline(
		context.Background(), testNow.Add(-time.Hour))
	defer cancelDeadline()
	if _, err := authority.Resolver().Resolve(deadline); !errors.Is(
		err, context.DeadlineExceeded,
	) || !shoal.IsErrorCode(err, shoal.ErrorDeadline) {
		t.Fatalf("Resolve(deadline) = %v", err)
	}

	static, err := auth.NewStaticResolverWithClock(decision, func() time.Time {
		return testNow
	})
	if err != nil {
		t.Fatalf("NewStaticResolverWithClock() = %v", err)
	}
	staticDecision, err := static.Resolve(context.Background())
	if err != nil {
		t.Fatalf("static Resolve() = %v", err)
	}
	domain := staticDecision.AuthorizationDomain()
	domain[0] = 'X'
	if bytes.Equal(domain, staticDecision.AuthorizationDomain()) {
		t.Fatal("static resolver returned shared decision bytes")
	}
}

func TestTrustedHostResolverValidatesDecisionsAndFailureCategories(t *testing.T) {
	decision := mustDecision(t, baseDecisionConfig())
	host, err := auth.NewHostResolverWithClock(
		func(context.Context) (auth.Decision, error) {
			return decision, nil
		},
		func() time.Time { return testNow },
	)
	if err != nil {
		t.Fatalf("NewHostResolverWithClock() = %v", err)
	}
	if _, err := host.Resolve(context.Background()); err != nil {
		t.Fatalf("host Resolve() = %v", err)
	}

	missing, err := auth.NewHostResolverWithClock(
		func(context.Context) (auth.Decision, error) {
			return auth.Decision{}, shoal.NewError(
				shoal.ErrorUnauthorized, "host detail that must not escape")
		},
		func() time.Time { return testNow },
	)
	if err != nil {
		t.Fatalf("NewHostResolverWithClock(missing) = %v", err)
	}
	if _, err := missing.Resolve(context.Background()); !shoal.IsErrorCode(
		err, shoal.ErrorUnauthorized,
	) || strings.Contains(err.Error(), "host detail") {
		t.Fatalf("missing host Resolve() = %v", err)
	}

	invalid, err := auth.NewHostResolverWithClock(
		func(context.Context) (auth.Decision, error) {
			return auth.Decision{}, nil
		},
		func() time.Time { return testNow },
	)
	if err != nil {
		t.Fatalf("NewHostResolverWithClock(invalid) = %v", err)
	}
	if _, err := invalid.Resolve(context.Background()); !shoal.IsErrorCode(
		err, shoal.ErrorUnauthorized,
	) {
		t.Fatalf("invalid host Resolve() = %v", err)
	}
}

func TestDecisionStringDoesNotExposeRawIdentityOrGrants(t *testing.T) {
	decision := mustDecision(t, baseDecisionConfig())
	rendered := decision.String()
	for _, raw := range []string{
		"subject-secret",
		"actor-secret",
		"domain-secret",
		"source-a",
		"policy-a",
		"request-secret",
		"ceiling-read",
	} {
		if strings.Contains(rendered, raw) {
			t.Fatalf("Decision.String() exposed %q in %q", raw, rendered)
		}
	}
}
