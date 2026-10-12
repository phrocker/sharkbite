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

package authorized

import (
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
)

// TestDeclassificationRecordKindIsNew pins that the audit (#630) took a new
// record kind and row prefix and left every existing one as it was.
func TestDeclassificationRecordKindIsNew(t *testing.T) {
	existing := map[string]byte{
		"source claim": 1, "revision": 2, "current": 3, "edge": 4,
		"edge claim": 5, "source version": 6, "node": 7, "co-occurrence": 8,
		"label migration": 9,
	}
	actual := map[string]byte{
		"source claim": policyKindSourceClaim, "revision": policyKindRevision,
		"current": policyKindCurrent, "edge": policyKindEdge,
		"edge claim": policyKindEdgeClaim, "source version": policyKindSourceVersion,
		"node": policyKindNode, "co-occurrence": policyKindCoOccurrence,
		"label migration": policyKindLabelMigration,
	}
	for name, kind := range existing {
		if actual[name] != kind {
			t.Fatalf("%s kind = %d, want %d", name, actual[name], kind)
		}
		if kind == policyKindDeclassification ||
			kind == policyKindDeclassificationClaim {
			t.Fatalf("declassification kind collides with %s", name)
		}
	}
	if policyKindDeclassification != 10 ||
		policyRowDeclassification != "audit/declassification/" ||
		policyKindDeclassificationClaim != 11 ||
		policyRowDeclassificationClaim != "audit/declassification-claim/" {
		t.Fatalf("declassification kinds/prefixes = %d %q %d %q",
			policyKindDeclassification, policyRowDeclassification,
			policyKindDeclassificationClaim, policyRowDeclassificationClaim)
	}
	if strings.HasPrefix(policyRowDeclassificationClaim, policyRowDeclassification) ||
		strings.HasPrefix(policyRowDeclassification, policyRowDeclassificationClaim) {
		t.Fatal("declassification record and claim-binding prefixes overlap")
	}
	for _, prefix := range []string{
		policyRowSourceClaim, policyRowRevision, policyRowCurrent,
		policyRowEdge, policyRowEdgeClaim, policyRowNode,
		policyRowSourceVersion, policyRowCoOccurrence, policyRowLabelMigration,
	} {
		if strings.HasPrefix(policyRowDeclassification, prefix) ||
			strings.HasPrefix(prefix, policyRowDeclassification) ||
			strings.HasPrefix(policyRowDeclassificationClaim, prefix) ||
			strings.HasPrefix(prefix, policyRowDeclassificationClaim) {
			t.Fatalf("declassification prefix overlaps %q", prefix)
		}
	}
}

// TestRuleNarrowsComparesLogicalComponents pins the narrowing test: epochs
// never matter, any removed or swapped component does, additions never do.
func TestRuleNarrowsComparesLogicalComponents(t *testing.T) {
	source := func(epoch int64) auth.Policy {
		policy, err := auth.NewPolicy(auth.PolicyConfig{
			AuthorizationDomain: []byte("d"), SourceID: []byte("s"),
			GrantPolicyID: []byte("p"), Epoch: epoch,
		})
		if err != nil {
			t.Fatal(err)
		}
		return policy
	}
	rule := func(policy auth.Policy, labels ...string) AccessRule {
		built, err := LabelRule(policy, labels)
		if err != nil {
			t.Fatal(err)
		}
		return built
	}
	for _, check := range []struct {
		name        string
		prior, next AccessRule
		want        bool
	}{
		{"same", rule(source(1), "a"), rule(source(1), "a"), false},
		{"epoch only", rule(source(1), "a"), rule(source(2), "a"), false},
		{"added", rule(source(1), "a"), rule(source(1), "a", "b"), false},
		{"removed", rule(source(1), "a", "b"), rule(source(1), "a"), true},
		{"all removed", rule(source(1), "a"), rule(source(1)), true},
		{"swapped", rule(source(1), "a"), rule(source(1), "b"), true},
		{"case", rule(source(1), "a"), rule(source(1), "A"), true},
	} {
		if got := ruleNarrows(check.prior, check.next); got != check.want {
			t.Fatalf("%s: ruleNarrows = %v, want %v", check.name, got, check.want)
		}
	}
}
