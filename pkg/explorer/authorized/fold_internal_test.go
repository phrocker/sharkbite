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
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package authorized

import (
	"context"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestFoldStoreNeverPromotesBase(t *testing.T) {
	base, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	trusted, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer trusted.Close()
	client := &Client{base: base, foldSource: trusted}
	store, err := client.foldStore()
	if err != nil {
		t.Fatal(err)
	}
	if store != trusted {
		t.Fatal("untrusted base was promoted to the fold store")
	}
	if _, err := store.Folds(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestValidateDurableFoldResultAcceptsLegacyIdentity(t *testing.T) {
	foldedAt := time.Unix(1700000000, 0).UTC()
	fold := interaction.Fold{
		Members: []interaction.FoldMember{{
			SessionID:        interaction.DerivedID("session", "legacy-result"),
			RetrievedNodeIDs: []shoal.ID{"source-a"},
			CitedNodeIDs:     []shoal.ID{"source-b"},
			TouchedEdgeIDs:   []shoal.ID{"source-edge"},
			Visibility:       []string{"internal"},
		}},
		FoldedAt: foldedAt,
	}
	canonical, err := fold.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	legacy := canonical
	legacy.Members = append([]interaction.FoldMember(nil), canonical.Members...)
	legacy.Members[0].TouchedEdgeIDs = nil
	legacyID, err := legacy.ID()
	if err != nil {
		t.Fatal(err)
	}
	result := explorer.FoldResult{
		FoldID: legacyID, FoldedAt: foldedAt,
		Visibility: "internal", MemberCount: 1,
		RetrievedCount: 1, CitedCount: 1,
	}
	if err := validateDurableFoldResult(result, canonical); err != nil {
		t.Fatalf("legacy durable result = %v", err)
	}
	result.FoldID = interaction.DerivedID("fold", "different")
	if err := validateDurableFoldResult(result, canonical); err == nil {
		t.Fatal("unrelated durable fold identity was accepted")
	}
}
