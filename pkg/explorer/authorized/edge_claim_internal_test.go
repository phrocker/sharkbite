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

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/graph"
)

func TestCommittedEdgeReleasesReservation(t *testing.T) {
	policy, err := auth.NewPolicy(auth.PolicyConfig{
		AuthorizationDomain: []byte("domain"),
		SourceID:            []byte("source"),
		GrantPolicyID:       []byte("policy"),
		Epoch:               1,
	})
	if err != nil {
		t.Fatal(err)
	}
	rule, err := NewAccessRule(policy)
	if err != nil {
		t.Fatal(err)
	}
	registration := EdgeRegistration{
		Edge: graph.Edge{
			ID: "edge", From: "from", To: "to", Type: "link", Weight: 1,
		},
		Rule: rule,
	}
	store := NewMemoryPolicyStore()
	if err := store.ReserveEdge(context.Background(), registration); err != nil {
		t.Fatal(err)
	}
	if err := store.PutEdge(context.Background(), registration); err != nil {
		t.Fatal(err)
	}
	if len(store.edgeClaims) != 0 {
		t.Fatalf("committed edge retained %d reservations", len(store.edgeClaims))
	}
}
