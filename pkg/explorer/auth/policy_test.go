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
	"time"

	"github.com/phrocker/shoal-oss/accumulo"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func basePolicyConfig() auth.PolicyConfig {
	return auth.PolicyConfig{
		AuthorizationDomain: []byte("domain-secret"),
		SourceID:            []byte("source-a"),
		GrantPolicyID:       []byte("policy-a"),
		Epoch:               7,
	}
}

func mustPolicy(t *testing.T, config auth.PolicyConfig) auth.Policy {
	t.Helper()
	policy, err := auth.NewPolicy(config)
	if err != nil {
		t.Fatalf("NewPolicy() = %v", err)
	}
	return policy
}

func TestBase32ComponentRoundTripsOpaqueBytes(t *testing.T) {
	opaque := []byte{0x00, 0xff, '&', ' ', '"', ':', 0x80, 0x7f}
	encoded, err := auth.EncodeComponent(opaque)
	if err != nil {
		t.Fatalf("EncodeComponent() = %v", err)
	}
	if encoded != strings.ToLower(encoded) ||
		strings.ContainsAny(encoded, "=&| \t\r\n\"'") {
		t.Fatalf("encoded opaque component is unsafe: %q", encoded)
	}
	decoded, err := auth.DecodeComponent(encoded)
	if err != nil {
		t.Fatalf("DecodeComponent() = %v", err)
	}
	if !bytes.Equal(decoded, opaque) {
		t.Fatalf("decoded = %x, want %x", decoded, opaque)
	}
	decoded[0] = 'X'
	again, err := auth.DecodeComponent(encoded)
	if err != nil || !bytes.Equal(again, opaque) {
		t.Fatalf("DecodeComponent() did not own bytes: %x, %v", again, err)
	}

	for _, invalid := range []string{"", "ME", "me======", "m1", "m e", "m&e"} {
		if _, err := auth.DecodeComponent(invalid); !shoal.IsErrorCode(
			err, shoal.ErrorInvalidArgument,
		) {
			t.Fatalf("DecodeComponent(%q) = %v", invalid, err)
		}
		maximum := bytes.Repeat([]byte{0xff}, auth.MaxPolicyComponentBytes)
		maximumEncoded, err := auth.EncodeComponent(maximum)
		if err != nil {
			t.Fatalf("EncodeComponent(maximum) = %v", err)
		}
		if decoded, err := auth.DecodeComponent(maximumEncoded); err != nil ||
			!bytes.Equal(decoded, maximum) {
			t.Fatalf("maximum component roundtrip = %x, %v", decoded, err)
		}
		if _, err := auth.EncodeComponent(
			bytes.Repeat([]byte{'x'}, auth.MaxPolicyComponentBytes+1),
		); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
			t.Fatalf("oversized EncodeComponent() = %v", err)
		}
		if _, err := auth.DecodeComponent(
			strings.Repeat("a", auth.MaxEncodedPolicyComponentBytes+1),
		); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
			t.Fatalf("oversized DecodeComponent() = %v", err)
		}
	}
}

func TestConjoinPoliciesEnforcesTermAndExpressionBounds(t *testing.T) {
	tooMany := make([]auth.Policy, 0, 22)
	for index := 0; index < 22; index++ {
		tooMany = append(tooMany, mustPolicy(t, auth.PolicyConfig{
			AuthorizationDomain: []byte{byte('a' + index)},
			SourceID:            []byte{byte('A' + index)},
			GrantPolicyID:       []byte{byte(index + 1)},
			Epoch:               int64(index + 1),
		}))
	}
	if _, err := auth.ConjoinPolicies(tooMany...); !shoal.IsErrorCode(
		err, shoal.ErrorInvalidArgument,
	) {
		t.Fatalf("ConjoinPolicies(66 terms) = %v", err)
	}

	oversized := make([]auth.Policy, 0, 21)
	for index := 0; index < 21; index++ {
		domain := bytes.Repeat([]byte{byte(index + 1)}, auth.MaxPolicyComponentBytes)
		source := bytes.Repeat([]byte{byte(index + 22)}, auth.MaxPolicyComponentBytes)
		grant := bytes.Repeat([]byte{byte(index + 43)}, auth.MaxPolicyComponentBytes)
		oversized = append(oversized, mustPolicy(t, auth.PolicyConfig{
			AuthorizationDomain: domain,
			SourceID:            source,
			GrantPolicyID:       grant,
			Epoch:               int64(index + 1),
		}))
	}
	if _, err := auth.ConjoinPolicies(oversized...); !shoal.IsErrorCode(
		err, shoal.ErrorInvalidArgument,
	) {
		t.Fatalf("ConjoinPolicies(oversized expression) = %v", err)
	}
}

func TestPolicyEncodingIsExactCanonicalVisibility(t *testing.T) {
	policy := mustPolicy(t, basePolicyConfig())
	domain, _ := auth.EncodeComponent([]byte("domain-secret"))
	source, _ := auth.EncodeComponent([]byte("source-a"))
	grant, _ := auth.EncodeComponent([]byte("policy-a"))
	want := "d:" + domain + "&g:" + grant + ":e:7&s:" + source

	encoded, err := policy.Encode()
	if err != nil {
		t.Fatalf("Encode() = %v", err)
	}
	if string(encoded) != want {
		t.Fatalf("Encode() = %q, want %q", encoded, want)
	}
	visibility, err := accumulo.NewColumnVisibility(encoded)
	if err != nil {
		t.Fatalf("NewColumnVisibility() = %v", err)
	}
	if !bytes.Equal(visibility.Flatten(), encoded) {
		t.Fatalf("Flatten() = %q, want %q", visibility.Flatten(), encoded)
	}
	decoded, err := auth.DecodePolicy(encoded)
	if err != nil {
		t.Fatalf("DecodePolicy() = %v", err)
	}
	if !bytes.Equal(decoded.AuthorizationDomain(), []byte("domain-secret")) ||
		!bytes.Equal(decoded.SourceID(), []byte("source-a")) ||
		!bytes.Equal(decoded.GrantPolicyID(), []byte("policy-a")) ||
		decoded.Epoch() != 7 ||
		decoded.ServiceRole() != "" {
		t.Fatalf("DecodePolicy() = %v", decoded)
	}
}

func TestPolicyDecoderRejectsInjectionAndNoncanonicalForms(t *testing.T) {
	policy := mustPolicy(t, basePolicyConfig())
	encoded, err := policy.Encode()
	if err != nil {
		t.Fatalf("Encode() = %v", err)
	}
	terms := strings.Split(string(encoded), "&")
	alternate := terms[2] + "&" + terms[0] + "&" + terms[1]
	cases := [][]byte{
		nil,
		[]byte("public"),
		[]byte(string(encoded) + "|svc:data_read"),
		[]byte(string(encoded) + " "),
		[]byte("\"" + string(encoded) + "\""),
		[]byte(string(encoded) + "&" + terms[0]),
		[]byte(alternate),
		[]byte(strings.Replace(string(encoded), "d:", "D:", 1)),
		[]byte(strings.Replace(string(encoded), ":e:7", ":e:07", 1)),
	}
	for _, expression := range cases {
		if _, err := auth.DecodePolicy(expression); !shoal.IsErrorCode(
			err, shoal.ErrorInvalidArgument,
		) {
			t.Fatalf("DecodePolicy(%q) = %v", expression, err)
		}
	}
}

func TestPolicyLogicalDigestIsStableAcrossEpochs(t *testing.T) {
	first := mustPolicy(t, basePolicyConfig())
	secondConfig := basePolicyConfig()
	secondConfig.Epoch = 8
	second := mustPolicy(t, secondConfig)

	firstLogical, err := first.LogicalPolicyDigest()
	if err != nil {
		t.Fatalf("first LogicalPolicyDigest() = %v", err)
	}
	secondLogical, err := second.LogicalPolicyDigest()
	if err != nil {
		t.Fatalf("second LogicalPolicyDigest() = %v", err)
	}
	if firstLogical != secondLogical {
		t.Fatalf("logical digests differ: %s != %s", firstLogical, secondLogical)
	}
	firstVisibility, _ := first.VisibilityDigest()
	secondVisibility, _ := second.VisibilityDigest()
	if firstVisibility == secondVisibility {
		t.Fatalf("visibility digests did not change: %s", firstVisibility)
	}
	for _, raw := range []string{"domain-secret", "source-a", "policy-a"} {
		if strings.Contains(first.String(), raw) {
			t.Fatalf("Policy.String() exposed %q in %q", raw, first.String())
		}
	}
}

func TestConjoinPoliciesUsesConjunctionAndDeduplicatesSharedLabels(t *testing.T) {
	first := mustPolicy(t, basePolicyConfig())
	secondConfig := basePolicyConfig()
	secondConfig.SourceID = []byte("source-b")
	secondConfig.GrantPolicyID = []byte("policy-b")
	secondConfig.Epoch = 9
	second := mustPolicy(t, secondConfig)

	expression, err := auth.ConjoinPolicies(first, second)
	if err != nil {
		t.Fatalf("ConjoinPolicies() = %v", err)
	}
	if strings.Contains(string(expression), "|") || strings.Count(string(expression), "d:") != 1 {
		t.Fatalf("ConjoinPolicies() = %q", expression)
	}
	if got := len(strings.Split(string(expression), "&")); got != 5 {
		t.Fatalf("term count = %d, want 5: %q", got, expression)
	}
	visibility, err := accumulo.NewColumnVisibility(expression)
	if err != nil || !bytes.Equal(visibility.Flatten(), expression) {
		t.Fatalf("conjunction is not canonical: %q, %v", expression, err)
	}
}

func TestScannerCeilingDerivesOnlyRequiredLabels(t *testing.T) {
	policy := mustPolicy(t, basePolicyConfig())
	encoded, _ := policy.Encode()
	required := strings.Split(string(encoded), "&")
	ceilingAuths := accumulo.NewAuthorizationStrings(
		required[0], required[1], required[2], "svc:data_read")
	ceiling, err := auth.NewServiceCeiling(auth.ServiceCeilingConfig{
		Identity:       "ceiling-read",
		Role:           auth.ServiceRoleDataRead,
		Authorizations: ceilingAuths,
	})
	if err != nil {
		t.Fatalf("NewServiceCeiling() = %v", err)
	}
	ceilingAuths.Remove([]byte(required[0]))

	decision := mustDecision(t, baseDecisionConfig())
	derived, err := auth.DeriveScannerAuthorizations(
		decision, auth.OperationRead, policy, ceiling, testNow)
	if err != nil {
		t.Fatalf("DeriveScannerAuthorizations() = %v", err)
	}
	if got := derived.Strings(); strings.Join(got, "&") != string(encoded) {
		t.Fatalf("derived authorizations = %v, want %q", got, encoded)
	}
	if derived.Contains([]byte("svc:data_read")) {
		t.Fatal("normal data scan received an unnecessary service label")
	}
	derived.Remove([]byte(required[0]))
	again, err := auth.DeriveScannerAuthorizations(
		decision, auth.OperationRead, policy, ceiling, testNow)
	if err != nil || !again.Contains([]byte(required[0])) {
		t.Fatalf("returned authorization set mutated ceiling: %v, %v", again, err)
	}

	missing, err := auth.NewServiceCeiling(auth.ServiceCeilingConfig{
		Identity: "ceiling-read",
		Role:     auth.ServiceRoleDataRead,
		Authorizations: accumulo.NewAuthorizationStrings(
			required[0], required[2], "svc:data_read"),
	})
	if err != nil {
		t.Fatalf("NewServiceCeiling(missing grant) = %v", err)
	}
	if _, err := auth.DeriveScannerAuthorizations(
		decision, auth.OperationRead, policy, missing, testNow,
	); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("derive outside ceiling = %v", err)
	}
}

func TestScannerCeilingRejectsDisallowedAndMismatchedServiceRoles(t *testing.T) {
	policy := mustPolicy(t, basePolicyConfig())
	encoded, _ := policy.Encode()
	required := strings.Split(string(encoded), "&")

	writeConfig := baseDecisionConfig()
	writeConfig.AllowedOperations = []auth.Operation{auth.OperationIngest}
	writeConfig.ServiceRole = auth.ServiceRoleDataWrite
	writeConfig.ServiceCeilingIdentity = "ceiling-write"
	writeDecision := mustDecision(t, writeConfig)
	writeCeiling, err := auth.NewServiceCeiling(auth.ServiceCeilingConfig{
		Identity: "ceiling-write",
		Role:     auth.ServiceRoleDataWrite,
		Authorizations: accumulo.NewAuthorizationStrings(
			required[0], required[1], required[2], "svc:data_write"),
	})
	if err != nil {
		t.Fatalf("NewServiceCeiling(write) = %v", err)
	}
	if _, err := auth.DeriveScannerAuthorizations(
		writeDecision, auth.OperationRead, policy, writeCeiling, testNow,
	); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("data-write read derivation = %v", err)
	}

	coordConfig := baseDecisionConfig()
	coordConfig.AllowedOperations = []auth.Operation{auth.OperationValidate}
	coordConfig.ServiceRole = auth.ServiceRoleCoordination
	coordConfig.ServiceCeilingIdentity = "ceiling-coordination"
	coordDecision := mustDecision(t, coordConfig)
	servicePolicy, err := auth.NewServicePolicy(basePolicyConfig(), coordDecision)
	if err != nil {
		t.Fatalf("NewServicePolicy() = %v", err)
	}
	outsideConfig := basePolicyConfig()
	outsideConfig.SourceID = []byte("source-hidden")
	if _, err := auth.NewServicePolicy(
		outsideConfig, coordDecision,
	); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("NewServicePolicy(outside decision) = %v", err)
	}
	serviceEncoded, _ := servicePolicy.Encode()
	serviceCeiling, err := auth.NewServiceCeiling(auth.ServiceCeilingConfig{
		Identity: "ceiling-coordination",
		Role:     auth.ServiceRoleCoordination,
		Authorizations: accumulo.NewAuthorizationStrings(
			strings.Split(string(serviceEncoded), "&")...),
	})
	if err != nil {
		t.Fatalf("NewServiceCeiling(coordination) = %v", err)
	}
	if _, err := auth.DeriveScannerAuthorizations(
		coordDecision, auth.OperationValidate, servicePolicy, serviceCeiling, testNow,
	); err != nil {
		t.Fatalf("coordination service derivation = %v", err)
	}
	if _, err := auth.DeriveScannerAuthorizations(
		coordDecision, auth.OperationValidate, policy, serviceCeiling, testNow,
	); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("coordination derivation without svc label = %v", err)
	}

	readDecision := mustDecision(t, baseDecisionConfig())
	readCeiling, err := auth.NewServiceCeiling(auth.ServiceCeilingConfig{
		Identity: "ceiling-read",
		Role:     auth.ServiceRoleDataRead,
		Authorizations: accumulo.NewAuthorizationStrings(
			append(strings.Split(string(serviceEncoded), "&"), "svc:data_read")...),
	})
	if err != nil {
		t.Fatalf("NewServiceCeiling(read mismatch) = %v", err)
	}
	if _, err := auth.DeriveScannerAuthorizations(
		readDecision, auth.OperationRead, servicePolicy, readCeiling, testNow,
	); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("mismatched service policy role = %v", err)
	}
}

func TestExpiredServiceDecisionCannotDeriveScannerAuthorizations(t *testing.T) {
	policy := mustPolicy(t, basePolicyConfig())
	encoded, _ := policy.Encode()
	required := strings.Split(string(encoded), "&")
	ceiling, err := auth.NewServiceCeiling(auth.ServiceCeilingConfig{
		Identity: "ceiling-read",
		Role:     auth.ServiceRoleDataRead,
		Authorizations: accumulo.NewAuthorizationStrings(
			required[0], required[1], required[2], "svc:data_read"),
	})
	if err != nil {
		t.Fatalf("NewServiceCeiling() = %v", err)
	}
	decision := mustDecision(t, baseDecisionConfig())
	if _, err := auth.DeriveScannerAuthorizations(
		decision, auth.OperationRead, policy, ceiling, testNow.Add(2*time.Hour),
	); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("expired derive = %v", err)
	}
}
