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

package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
)

// TestIdentityPolicyRefusesLabelNamespace: label grants come only from the
// label grant file, so neither -identity-policy nor the -dev-auth identity
// may name a policy in the label namespace.
func TestIdentityPolicyRefusesLabelNamespace(t *testing.T) {
	emptyEnvironment := func(string) string { return "" }
	for _, policy := range []string{
		"shoal.label/v1/6/source/secret",
		"shoal.label/v1/!untranslatable",
		"shoal.label/v2/anything",
	} {
		if _, err := configureIdentity(identityOptions{
			subject: "subject", actor: "actor", domain: "domain",
			sourceID: "source", policyID: policy,
			policyGeneration: 1, lifetime: time.Hour,
		}); err == nil || !strings.Contains(err.Error(), "label namespace") {
			t.Fatalf("configureIdentity(policy %q) = %v, want refusal", policy, err)
		}
		if _, err := parseCommandConfig(
			[]string{
				"-identity-subject", "subject",
				"-identity-actor", "actor",
				"-identity-domain", "domain",
				"-identity-source", "source",
				"-identity-policy", policy,
			},
			io.Discard,
			emptyEnvironment,
		); err == nil || !strings.Contains(err.Error(), "label namespace") {
			t.Fatalf("-identity-policy %q = %v, want refusal", policy, err)
		}
		if _, err := parseCommandConfig(
			[]string{
				"-identity-subject", "subject",
				"-identity-actor", "actor",
				"-identity-domain", "domain",
				"-identity-source", "source",
			},
			io.Discard,
			func(name string) string {
				if name == "SHOAL_MCP_IDENTITY_POLICY" {
					return policy
				}
				return ""
			},
		); err == nil || !strings.Contains(err.Error(), "label namespace") {
			t.Fatalf("SHOAL_MCP_IDENTITY_POLICY=%q = %v, want refusal", policy, err)
		}
	}
	development, err := configureIdentity(identityOptions{
		development: true, policyGeneration: 1, lifetime: time.Hour,
	})
	if err != nil {
		t.Fatalf("configureIdentity(-dev-auth) = %v", err)
	}
	if auth.IsLabelPolicyID(development.policyID) ||
		auth.IsLabelPolicyID([]byte(defaultPolicyID)) {
		t.Fatalf("-dev-auth identity policy %q is in the label namespace",
			development.policyID)
	}
}

// TestIdentityLabelsGrantLabelPoliciesOnTheIdentitySource: -identity-labels
// (and its environment fallback) is the one way a process identity holds a
// label policy. A grant names the identity's own source exactly; a grant on
// any other source, a bad label or a malformed entry refuses the identity.
// The labels add policy IDs and change nothing else.
func TestIdentityLabelsGrantLabelPoliciesOnTheIdentitySource(t *testing.T) {
	explicit := []string{
		"-identity-subject", "subject",
		"-identity-actor", "actor",
		"-identity-domain", "domain",
		"-identity-source", "src/a",
		"-identity-policy", "grant",
	}
	none := func(string) string { return "" }
	secret, err := authorized.LabelPolicyID([]byte("src/a"), "secret")
	if err != nil {
		t.Fatal(err)
	}
	pii, err := authorized.LabelPolicyID([]byte("src/a"), "pii")
	if err != nil {
		t.Fatal(err)
	}
	policiesOf := func(t *testing.T, config commandConfig) [][]byte {
		t.Helper()
		provider, err := newProcessIdentity(config.identity, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		decision, err := provider.Decision(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return decision.PermittedPolicyIDs()
	}

	unlabelled, err := parseCommandConfig(explicit, io.Discard, none)
	if err != nil {
		t.Fatal(err)
	}
	if got := policiesOf(t, unlabelled); len(got) != 1 || string(got[0]) != "grant" {
		t.Fatalf("an identity without -identity-labels holds %q", got)
	}

	flagged, err := parseCommandConfig(
		append(append([]string(nil), explicit...),
			"-identity-labels", "src/a=secret,src/a=pii"),
		io.Discard, none)
	if err != nil {
		t.Fatal(err)
	}
	got := policiesOf(t, flagged)
	if len(got) != 3 || !containsID(got, secret) || !containsID(got, pii) ||
		!containsID(got, []byte("grant")) {
		t.Fatalf("-identity-labels policies = %q", got)
	}
	if len(flagged.identity.operations) != len(readOperations) {
		t.Fatalf("labels changed the operations: %v", flagged.identity.operations)
	}

	fromEnvironment, err := parseCommandConfig(explicit, io.Discard,
		func(name string) string {
			if name == "SHOAL_MCP_IDENTITY_LABELS" {
				return "src/a=secret"
			}
			return ""
		})
	if err != nil {
		t.Fatal(err)
	}
	if got := policiesOf(t, fromEnvironment); !containsID(got, secret) {
		t.Fatalf("SHOAL_MCP_IDENTITY_LABELS policies = %q", got)
	}

	for _, refused := range []string{
		"src/b=secret",  // another source
		"SRC/A=secret",  // the source is byte-exact
		"src/a:secret",  // ':' is a label character, not the separator
		"src/a=Secret!", // not a label
		"src/a=secret,", // empty entry
		"src/a=secret,src/a=secret",
	} {
		if _, err := parseCommandConfig(
			append(append([]string(nil), explicit...), "-identity-labels", refused),
			io.Discard, none); err == nil ||
			!strings.Contains(err.Error(), "-identity-labels") {
			t.Fatalf("-identity-labels %q = %v, want refusal", refused, err)
		}
	}

	development, err := parseCommandConfig(
		[]string{"-dev-auth", "-identity-labels",
			defaultSourceID + "=secret"}, io.Discard, none)
	if err != nil {
		t.Fatal(err)
	}
	devSecret, err := authorized.LabelPolicyID([]byte(defaultSourceID), "secret")
	if err != nil {
		t.Fatal(err)
	}
	if got := policiesOf(t, development); !containsID(got, devSecret) {
		t.Fatalf("-dev-auth -identity-labels policies = %q", got)
	}
}

func containsID(ids [][]byte, want []byte) bool {
	for _, id := range ids {
		if bytes.Equal(id, want) {
			return true
		}
	}
	return false
}
