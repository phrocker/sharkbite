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
	"context"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
)

// ingestVisibleAndHidden ingests one document alice may read (through the
// source-a client) and one she may not (through the source-b client), returning
// both summaries. Alice is authorized for source-a/policy-a only, so the
// source-b document is denied by her rule at the authorization gate.
func ingestVisibleAndHidden(
	t *testing.T,
	f *fixture,
) (visible, hidden explorer.IngestResult) {
	t.Helper()
	var err error
	visible, err = f.clientA.Ingest(f.admin(t), explorer.Source{
		URI:       "file:///visible.txt",
		Title:     "Visible",
		MediaType: explorer.MediaTypeText,
		Content:   "alpha",
	})
	if err != nil {
		t.Fatal(err)
	}
	hidden, err = f.clientB.Ingest(f.admin(t), explorer.Source{
		URI:       "file:///hidden.txt",
		Title:     "Hidden alpha beta",
		MediaType: explorer.MediaTypeText,
		Content:   "alpha beta",
	})
	if err != nil {
		t.Fatal(err)
	}
	return visible, hidden
}

func TestDocumentsWithSuppressedCountsAuthorizationDrops(t *testing.T) {
	f := newFixture(t)
	visible, _ := ingestVisibleAndHidden(t, f)

	summaries, suppressed, err := f.clientA.DocumentsWithSuppressed(f.alice(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].Document.ID != visible.Document.ID {
		t.Fatalf("alice summaries = %#v", summaries)
	}
	// Alice is denied exactly the one source-b document: the count must be
	// exactly one, not merely positive. A mutation that forces the count to 0
	// fails here.
	if suppressed != 1 {
		t.Fatalf("alice suppressed = %d, want 1", suppressed)
	}
}

func TestDocumentsWithSuppressedIsZeroWhenNothingWithheld(t *testing.T) {
	f := newFixture(t)
	ingestVisibleAndHidden(t, f)

	summaries, suppressed, err := f.clientA.DocumentsWithSuppressed(f.admin(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 2 {
		t.Fatalf("admin summaries = %#v", summaries)
	}
	// Admin is authorized for every source and policy, so nothing is withheld.
	// The count must be exactly zero. A mutation that forces a nonzero count
	// when nothing was withheld fails here.
	if suppressed != 0 {
		t.Fatalf("admin suppressed = %d, want 0", suppressed)
	}
}

func TestRetrieveWithSuppressedCountsAuthorizationDrops(t *testing.T) {
	f := newFixture(t)
	visible, hidden := ingestVisibleAndHidden(t, f)

	response, suppressed, err := f.clientA.RetrieveWithSuppressed(
		f.alice(t), retrieval.Request{Text: "alpha beta", TopK: 2})
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range response.Results {
		for _, evidence := range result.Evidence {
			if evidence.Citation.DocumentID == hidden.Document.ID {
				t.Fatalf("hidden document leaked into results: %#v", response)
			}
			if evidence.Citation.DocumentID != visible.Document.ID {
				t.Fatalf("unexpected document in results: %#v", response)
			}
		}
	}
	if suppressed != 1 {
		t.Fatalf("alice retrieve suppressed = %d, want 1", suppressed)
	}

	_, adminSuppressed, err := f.clientA.RetrieveWithSuppressed(
		f.admin(t), retrieval.Request{Text: "alpha beta", TopK: 2})
	if err != nil {
		t.Fatal(err)
	}
	if adminSuppressed != 0 {
		t.Fatalf("admin retrieve suppressed = %d, want 0", adminSuppressed)
	}
}

func TestRetrieveWithSuppressedReportsWithheldWhenNoResults(t *testing.T) {
	f := newFixture(t)
	ingestVisibleAndHidden(t, f)

	// "beta" appears only in the hidden document's content; the visible
	// document ("alpha") does not match. Alice therefore gets zero results yet
	// one document is withheld from her — the state that previously read as a
	// flat empty answer.
	response, suppressed, err := f.clientA.RetrieveWithSuppressed(
		f.alice(t), retrieval.Request{
			Text:  "beta",
			Modes: []retrieval.Mode{retrieval.ModeLexical},
			TopK:  5,
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 0 {
		t.Fatalf("expected no results, got %#v", response)
	}
	if suppressed != 1 {
		t.Fatalf("no-results suppressed = %d, want 1", suppressed)
	}
}

// TestSuppressedCountsDocumentsWithNoPolicyGrant is the regression lock for a
// lost or empty policy catalog: the corpus is intact but the policy store holds
// zero registrations, so every document drops through the missing-grant (!ok)
// branch. The count must equal the document total, not zero — otherwise a fully
// withheld corpus would reassure the user that nothing was withheld at the
// moment the system is most wrong.
func TestSuppressedCountsDocumentsWithNoPolicyGrant(t *testing.T) {
	f := newFixture(t)
	ingestVisibleAndHidden(t, f)

	baseSummaries, err := f.base.Documents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	total := uint32(len(baseSummaries))
	if total == 0 {
		t.Fatal("expected the base corpus to hold documents")
	}

	// A client over the same intact corpus but a fresh, empty policy store: no
	// document has any registration, exactly as a detached policy volume looks.
	orphaned := f.newClient(
		t, f.base, authorized.NewMemoryPolicyStore(), f.sourceA, f.policyA, nil)

	summaries, suppressed, err := orphaned.DocumentsWithSuppressed(f.admin(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 0 {
		t.Fatalf("expected no visible documents, got %#v", summaries)
	}
	if suppressed != total {
		t.Fatalf("empty-catalog documents suppressed = %d, want %d",
			suppressed, total)
	}

	response, retrieveSuppressed, err := orphaned.RetrieveWithSuppressed(
		f.admin(t), retrieval.Request{
			Text:  "alpha beta",
			Modes: []retrieval.Mode{retrieval.ModeLexical},
			TopK:  5,
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 0 {
		t.Fatalf("expected no results, got %#v", response)
	}
	if retrieveSuppressed != total {
		t.Fatalf("empty-catalog retrieve suppressed = %d, want %d",
			retrieveSuppressed, total)
	}
}
