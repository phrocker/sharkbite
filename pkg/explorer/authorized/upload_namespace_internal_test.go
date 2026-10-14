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
	"context"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// unevaluableClaimStore holds one claim whose rule fails to evaluate (its
// policy has no authorization domain), as no valid catalog can.
type unevaluableClaimStore struct {
	PolicyStore
	uri string
}

func (s unevaluableClaimStore) SourceClaim(
	ctx context.Context, sourceURI string,
) (SourcePolicyClaim, bool, error) {
	if sourceURI == s.uri {
		return SourcePolicyClaim{
			SourceURI: sourceURI,
			Rule:      AccessRule{policies: []auth.Policy{{}}, keys: [][]byte{{1}}},
		}, true, nil
	}
	return s.PolicyStore.SourceClaim(ctx, sourceURI)
}

// TestUploadRouteSkipsARuleThatFailsToEvaluate: a candidate whose rule
// cannot be evaluated is skipped as if absent, never routed to: routing there
// would surface the evaluation error, so the outcome would depend on a rule
// the caller is not authorized under.
func TestUploadRouteSkipsARuleThatFailsToEvaluate(t *testing.T) {
	const uri = UploadSourceURIPrefix + "broken.md"
	now := time.Date(2026, time.August, 26, 18, 0, 0, 0, time.UTC)
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "prober", Actor: "prober-actor",
		AuthorizationDomain:   []byte("domain-one"),
		AllowedOperations:     []auth.Operation{auth.OperationIngest},
		PermittedSourceIDs:    [][]byte{[]byte("source-a")},
		PermittedPolicyIDs:    [][]byte{[]byte("policy-a")},
		PolicyGeneration:      1,
		AuthenticationExpires: now.Add(time.Hour),
		RequestID:             "prober-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	broken := AccessRule{policies: []auth.Policy{{}}, keys: [][]byte{{1}}}
	if _, err := ruleAllows(broken, decision, auth.OperationIngest, now); err == nil {
		t.Fatal("the broken rule evaluates: the test is vacuous")
	}
	client := &Client{policyStore: unevaluableClaimStore{
		PolicyStore: NewMemoryPolicyStore(), uri: uri,
	}}
	eligible, err := client.uploadRouteEligible(context.Background(), decision, now, uri)
	if err != nil || eligible {
		t.Fatalf("eligible = %v (%v), want skipped", eligible, err)
	}
	if _, err := client.routeUploadTarget(context.Background(), decision, now, uri); !shoal.IsErrorCode(
		err, shoal.ErrorInvalidArgument) {
		t.Fatalf("target with an unevaluable rule = %v, want the target refusal", err)
	}
}
