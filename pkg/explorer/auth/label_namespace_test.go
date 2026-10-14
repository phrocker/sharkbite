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
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestLabelNamespacePrefixes(t *testing.T) {
	if auth.LabelPolicyNamespace != "shoal.label/" ||
		auth.LabelPolicyIDPrefix != "shoal.label/v1/" ||
		auth.ReservedLabelPolicyIDPrefix != "shoal.label/v1/!" {
		t.Fatalf("label namespace prefixes = %q %q %q", auth.LabelPolicyNamespace,
			auth.LabelPolicyIDPrefix, auth.ReservedLabelPolicyIDPrefix)
	}
	for _, tc := range []struct {
		id              string
		label, reserved bool
	}{
		{"shoal.label/v1/1/s/secret", true, false},
		{"shoal.label/v2/anything", true, false},
		{"shoal.label/", true, false},
		{"shoal.label/v1/!untranslatable", true, true},
		{"shoal.label/v1/!", true, true},
		{"shoal.label", false, false},
		{"policy", false, false},
		{"x/shoal.label/v1/!untranslatable", false, false},
	} {
		if got := auth.IsLabelPolicyID([]byte(tc.id)); got != tc.label {
			t.Fatalf("IsLabelPolicyID(%q) = %v", tc.id, got)
		}
		if got := auth.IsReservedLabelPolicyID([]byte(tc.id)); got != tc.reserved {
			t.Fatalf("IsReservedLabelPolicyID(%q) = %v", tc.id, got)
		}
	}
}

// TestNewDecisionRefusesReservedLabelPolicyIDs: nobody may ever hold an ID in
// the reserved label namespace, whatever path builds the decision. Ordinary
// label policy IDs remain grantable (they come from the label grant file).
func TestNewDecisionRefusesReservedLabelPolicyIDs(t *testing.T) {
	for _, reserved := range []string{
		"shoal.label/v1/!untranslatable", "shoal.label/v1/!", "shoal.label/v1/!other",
	} {
		config := baseDecisionConfig()
		config.PermittedPolicyIDs = append(config.PermittedPolicyIDs, []byte(reserved))
		if _, err := auth.NewDecision(config); !shoal.IsErrorCode(
			err, shoal.ErrorInvalidArgument) {
			t.Fatalf("NewDecision(grant %q) = %v, want refusal", reserved, err)
		}
	}
	config := baseDecisionConfig()
	config.PermittedPolicyIDs = append(config.PermittedPolicyIDs,
		[]byte("shoal.label/v1/8/source-a/secret"))
	if _, err := auth.NewDecision(config); err != nil {
		t.Fatalf("NewDecision(ordinary label grant) = %v", err)
	}
}
