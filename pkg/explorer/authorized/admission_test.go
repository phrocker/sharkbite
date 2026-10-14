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

package authorized_test

import (
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// listOnlyOperations is the posture of a caller that may enumerate the corpus
// and not read it.
var listOnlyOperations = []auth.Operation{auth.OperationList}

// TestRestrictDisclosureWithholdsCrossDomainReference proves the co-occurrence
// budget can be expressed as an obligation over a caller-declared set, not only
// as a document quietly missing from a result this package assembled. Without
// it an external caller can only be refused outright where an internal one is
// narrowed.
func TestRestrictDisclosureWithholdsCrossDomainReference(t *testing.T) {
	f := newFixture(t)
	visible, hidden := ingestVisibleAndHidden(t, f)

	client := f.mosaicClient(t, f.store, authorized.MosaicBudget{
		MaxDomains: 1, Window: time.Hour,
	})
	allowed, err := client.RestrictDisclosure(
		f.admin(t),
		[]shoal.ID{hidden.Document.ID, visible.Document.ID},
	)
	if err != nil {
		t.Fatal(err)
	}
	// Admin is individually authorized for both compartments, so neither is a
	// plain denial. A budget of one admits the first domain in the declared
	// order and obliges the caller to withhold the second.
	if len(allowed) != 1 || allowed[0] != hidden.Document.ID {
		t.Fatalf("allowed = %v, want only %s", allowed, hidden.Document.ID)
	}
}

// TestRestrictDisclosureIsIdempotentForOneCaller proves a retried admission is
// not charged twice. A proxy whose response was lost has to be able to ask
// again, and a budget that shrank on every retry would turn a network failure
// into a policy outcome.
func TestRestrictDisclosureIsIdempotentForOneCaller(t *testing.T) {
	f := newFixture(t)
	visible, hidden := ingestVisibleAndHidden(t, f)

	client := f.mosaicClient(t, f.store, authorized.MosaicBudget{
		MaxDomains: 2, Window: time.Hour,
	})
	references := []shoal.ID{hidden.Document.ID, visible.Document.ID}
	first, err := client.RestrictDisclosure(f.admin(t), references)
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.RestrictDisclosure(f.admin(t), references)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || len(second) != 2 {
		t.Fatalf("repeat charged the budget: %v then %v", first, second)
	}
}

// TestRestrictDisclosureWithholdsUnauthorizedAndUnknownAlike proves the two
// withholding reasons are indistinguishable from outside. A caller that could
// tell "you may not read this" from "this does not exist" would have an
// authorization oracle over document identities it merely guessed.
func TestRestrictDisclosureWithholdsUnauthorizedAndUnknownAlike(t *testing.T) {
	f := newFixture(t)
	visible, hidden := ingestVisibleAndHidden(t, f)

	client := f.mosaicClient(t, f.store, authorized.MosaicBudget{})
	allowed, err := client.RestrictDisclosure(
		f.alice(t),
		[]shoal.ID{visible.Document.ID, hidden.Document.ID, "never-ingested"},
	)
	if err != nil {
		t.Fatal(err)
	}
	// Alice holds source-a only. The source-b document and the identity that
	// was never ingested are both simply absent from the result.
	if len(allowed) != 1 || allowed[0] != visible.Document.ID {
		t.Fatalf("allowed = %v, want only %s", allowed, visible.Document.ID)
	}
}

// TestRestrictDisclosureRequiresRetrieveAuthority proves the operation checked
// is the one that governs putting content into a payload. A caller that may
// enumerate the corpus but not read it must not be admitted.
func TestRestrictDisclosureRequiresRetrieveAuthority(t *testing.T) {
	f := newFixture(t)
	visible, _ := ingestVisibleAndHidden(t, f)

	client := f.mosaicClient(t, f.store, authorized.MosaicBudget{})
	listOnly := f.context(t, f.decision(
		t, "lister", [][]byte{f.sourceA}, [][]byte{f.policyA},
		listOnlyOperations))
	if _, err := client.RestrictDisclosure(
		listOnly, []shoal.ID{visible.Document.ID},
	); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("list-only restriction = %v", err)
	}
}

// TestRestrictDisclosureBoundsItsInput proves the declared set is bounded, so a
// caller cannot make the decision plane resolve an unbounded number of
// registrations by asking one question.
func TestRestrictDisclosureBoundsItsInput(t *testing.T) {
	f := newFixture(t)
	client := f.mosaicClient(t, f.store, authorized.MosaicBudget{})
	references := make(
		[]shoal.ID, authorized.MaxRestrictedReferences+1)
	for i := range references {
		references[i] = shoal.ID("doc-" + string(rune('a'+i%26)))
	}
	if _, err := client.RestrictDisclosure(
		f.admin(t), references,
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("oversized declaration = %v", err)
	}
	if allowed, err := client.RestrictDisclosure(
		f.admin(t), nil); err != nil || allowed != nil {
		t.Fatalf("empty declaration = %v, %v", allowed, err)
	}
}
