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

package interaction_test

import (
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestRequiredVisibilityCanOnlyNarrowRecordedOutput(t *testing.T) {
	session := interaction.Session{
		ID:                 interaction.DerivedID("session", "required-visibility"),
		RecordedAt:         time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC),
		Operation:          interaction.OperationChat,
		RequiredVisibility: []string{"tenant", "ops", "tenant"},
		SeedNodeIDs:        []shoal.ID{"source"},
		CitedNodeIDs:       []shoal.ID{"source"},
	}
	subgraph, err := session.Subgraph(func(id shoal.ID) ([]string, error) {
		if id != "source" {
			t.Fatalf("unexpected source ID %q", id)
		}
		return []string{"secret", "ops"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	const want = "ops&secret&tenant"
	if got := interaction.Expression(subgraph.Visibility); got != want {
		t.Fatalf("visibility = %q, want %q", got, want)
	}
	canonical, err := session.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if got := interaction.Expression(canonical.RequiredVisibility); got !=
		"ops&tenant" {
		t.Fatalf("required visibility = %q", got)
	}
	canonical.RequiredVisibility[0] = "mutated"
	repeated, err := session.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if repeated.RequiredVisibility[0] != "ops" {
		t.Fatal("canonical session leaked required visibility mutation")
	}
	invalid := session
	invalid.RequiredVisibility = []string{"not allowed"}
	if err := invalid.Validate(); err == nil {
		t.Fatal("invalid required visibility was accepted")
	}
	requested := repeated
	persisted := repeated
	persisted.RequiredVisibility = []string{"different"}
	if err := interaction.ValidateRecordedSession(
		requested, persisted); err == nil {
		t.Fatal("persisted required visibility mismatch was accepted")
	}
}
