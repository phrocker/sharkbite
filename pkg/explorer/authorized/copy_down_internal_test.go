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
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/graph"
)

// TestApplicationEdgeRequiresBothEndpointRules pins the binding that keeps a
// Connect edge's weaker stored rule from admitting anyone (#663): an
// application edge whose own rule allows is still denied when either
// endpoint's rule denies or an endpoint is unresolved. Existing endpoint tests
// use the zero Kind, so without this a bypass for RegistrationApplication
// alone would go unnoticed (the neighborhood filter also hides such an edge).
func TestApplicationEdgeRequiresBothEndpointRules(t *testing.T) {
	now := time.Date(2026, time.October, 13, 12, 0, 0, 0, time.UTC)
	allowedRule := batchTestRule(t, "source", "policy")
	deniedRule := batchTestRule(t, "source", "other-policy")
	decision := batchTestDecision(t, now)
	registration := EdgeRegistration{
		Edge: graph.Edge{
			ID: "edge", From: "node-from", To: "node-to", Type: "link", Weight: 1,
		},
		Rule: allowedRule,
		Kind: RegistrationApplication,
	}
	allowed := NodeRegistration{Rule: allowedRule}
	denied := NodeRegistration{Rule: deniedRule}
	for name, resolved := range map[string]registeredNodes{
		"denied source":  {"node-from": denied, "node-to": allowed},
		"denied target":  {"node-from": allowed, "node-to": denied},
		"missing target": {"node-from": allowed},
	} {
		got, err := edgeAllowsResolved(
			resolved, registration, decision, auth.OperationNeighborhood, now)
		if err != nil || got {
			t.Fatalf("%s: edgeAllowsResolved = %v, %v, want false, nil", name, got, err)
		}
		got, err = edgeEndpointsAllow(
			resolved, registration, decision, auth.OperationNeighborhood, now)
		if err != nil || got {
			t.Fatalf("%s: edgeEndpointsAllow = %v, %v, want false, nil", name, got, err)
		}
	}
	// The control: with both endpoints allowed the edge is allowed, so the
	// denials above are the endpoints' doing.
	got, err := edgeAllowsResolved(registeredNodes{
		"node-from": allowed, "node-to": allowed,
	}, registration, decision, auth.OperationNeighborhood, now)
	if err != nil || !got {
		t.Fatalf("both endpoints allowed: %v, %v, want true, nil", got, err)
	}
}
