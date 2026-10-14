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

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
)

// mosaicConfig builds a read client Config wired to the fixture with the given
// policy store and mosaic budget. Reads never consult the client's own policy
// selector, so any valid source/policy pair serves; the store is the variable
// under test.
func (f *fixture) mosaicConfig(
	t *testing.T,
	store authorized.PolicyStore,
	budget authorized.MosaicBudget,
) authorized.Config {
	t.Helper()
	selector, err := authorized.NewStaticPolicySelector(f.sourceA, f.policyA)
	if err != nil {
		t.Fatal(err)
	}
	return authorized.Config{
		Base:             f.base,
		VectorScorer:     trustedVectorScorer(f.base),
		Resolver:         f.authority.Resolver(),
		PolicySelector:   selector,
		PolicyStore:      store,
		GenerationReader: f.reader,
		Clock:            f.clock.Now,
		Mosaic:           budget,
	}
}

// mosaicClient constructs a read client that enforces the mosaic co-occurrence
// budget against the supplied store.
func (f *fixture) mosaicClient(
	t *testing.T,
	store authorized.PolicyStore,
	budget authorized.MosaicBudget,
) *authorized.Client {
	t.Helper()
	client, err := authorized.NewClient(f.mosaicConfig(t, store, budget))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func summaryIDs(summaries []explorer.DocumentSummary) []string {
	ids := make([]string, 0, len(summaries))
	for _, summary := range summaries {
		ids = append(ids, string(summary.Document.ID))
	}
	return ids
}

// TestMosaicBudgetDocumentsWithholdsCrossDomain proves a document the identity
// is individually authorized for is withheld once its sensitivity domain would
// push the identity past its distinct-domain budget, and that the withholding
// is reported as Restricted, not Suppressed.
func TestMosaicBudgetDocumentsWithholdsCrossDomain(t *testing.T) {
	f := newFixture(t)
	_, hidden := ingestVisibleAndHidden(t, f)

	mc := f.mosaicClient(t, f.store, authorized.MosaicBudget{
		MaxDomains: 1,
		Window:     time.Hour,
	})
	summaries, disclosure, err := mc.DocumentsWithDisclosure(f.admin(t))
	if err != nil {
		t.Fatal(err)
	}
	// Admin is authorized for both compartments, so nothing is a plain denial;
	// exactly one cross-domain document is withheld by the budget of one.
	if disclosure.Suppressed != 0 {
		t.Fatalf("admin suppressed = %d, want 0", disclosure.Suppressed)
	}
	if disclosure.Restricted != 1 {
		t.Fatalf("admin restricted = %d, want 1", disclosure.Restricted)
	}
	if len(summaries) != 1 {
		t.Fatalf("admin summaries = %v, want exactly one", summaryIDs(summaries))
	}
	// The greedy admission keeps the first domain in base order (the hidden
	// document) and withholds the later, distinct one.
	if summaries[0].Document.ID != hidden.Document.ID {
		t.Fatalf("admitted %s, want the first base-order document %s",
			summaries[0].Document.ID, hidden.Document.ID)
	}
}

// TestMosaicBudgetRetrieveWithholdsCrossDomain proves the restricted document
// is dropped from the retrieval projection so the base scorer never searches
// it, and the drop is reported as Restricted.
func TestMosaicBudgetRetrieveWithholdsCrossDomain(t *testing.T) {
	f := newFixture(t)
	visible, _ := ingestVisibleAndHidden(t, f)

	mc := f.mosaicClient(t, f.store, authorized.MosaicBudget{
		MaxDomains: 1,
		Window:     time.Hour,
	})
	response, disclosure, err := mc.RetrieveWithDisclosure(
		f.admin(t), retrieval.Request{Text: "alpha beta", TopK: 2})
	if err != nil {
		t.Fatal(err)
	}
	if disclosure.Suppressed != 0 {
		t.Fatalf("admin retrieve suppressed = %d, want 0", disclosure.Suppressed)
	}
	if disclosure.Restricted != 1 {
		t.Fatalf("admin retrieve restricted = %d, want 1", disclosure.Restricted)
	}
	// The withheld (visible) document must not surface in any evidence citation:
	// restriction removes it from the searched projection entirely.
	for _, result := range response.Results {
		for _, evidence := range result.Evidence {
			if evidence.Citation.DocumentID == visible.Document.ID {
				t.Fatalf("restricted document leaked into results: %#v", response)
			}
		}
	}
}

// TestMosaicBudgetIdempotentAcrossRepeatedReads proves an identical repeated
// read does not double-charge the budget: the co-occurrence charge is a set
// union over observed domains, so the second read reports the same Restricted
// count and the same visible set as the first.
func TestMosaicBudgetIdempotentAcrossRepeatedReads(t *testing.T) {
	f := newFixture(t)
	ingestVisibleAndHidden(t, f)

	mc := f.mosaicClient(t, f.store, authorized.MosaicBudget{
		MaxDomains: 1,
		Window:     time.Hour,
	})
	first, firstDisclosure, err := mc.DocumentsWithDisclosure(f.admin(t))
	if err != nil {
		t.Fatal(err)
	}
	second, secondDisclosure, err := mc.DocumentsWithDisclosure(f.admin(t))
	if err != nil {
		t.Fatal(err)
	}
	if firstDisclosure.Restricted != 1 {
		t.Fatalf("first restricted = %d, want 1", firstDisclosure.Restricted)
	}
	// The crux: a second identical read is still 1, never 2. A double-charge
	// (e.g. appending domains instead of union) would make this 2.
	if secondDisclosure.Restricted != 1 {
		t.Fatalf("second restricted = %d, want 1 (idempotent)",
			secondDisclosure.Restricted)
	}
	firstIDs := summaryIDs(first)
	secondIDs := summaryIDs(second)
	if len(firstIDs) != 1 || len(secondIDs) != 1 ||
		firstIDs[0] != secondIDs[0] {
		t.Fatalf("visible set changed across repeated reads: %v then %v",
			firstIDs, secondIDs)
	}
}

// TestMosaicBudgetPerIdentityIsolation proves one identity's budget consumption
// cannot be observed by or affect another. Alice and Bob share the same store
// and ledger, yet each sees only its own single compartment with no
// restriction. Under a shared/global ledger key, Bob would inherit Alice's
// observed domain, exhaust the budget of one, and be restricted to nothing.
func TestMosaicBudgetPerIdentityIsolation(t *testing.T) {
	f := newFixture(t)
	visible, hidden := ingestVisibleAndHidden(t, f)

	mc := f.mosaicClient(t, f.store, authorized.MosaicBudget{
		MaxDomains: 1,
		Window:     time.Hour,
	})
	aliceSummaries, aliceDisclosure, err := mc.DocumentsWithDisclosure(f.alice(t))
	if err != nil {
		t.Fatal(err)
	}
	if aliceDisclosure.Restricted != 0 {
		t.Fatalf("alice restricted = %d, want 0", aliceDisclosure.Restricted)
	}
	if len(aliceSummaries) != 1 ||
		aliceSummaries[0].Document.ID != visible.Document.ID {
		t.Fatalf("alice summaries = %v, want the visible document",
			summaryIDs(aliceSummaries))
	}

	// Bob reads after Alice against the same ledger. His distinct compartment
	// must be admitted on its own budget, untouched by Alice's consumption.
	bobSummaries, bobDisclosure, err := mc.DocumentsWithDisclosure(f.bob(t))
	if err != nil {
		t.Fatal(err)
	}
	if bobDisclosure.Restricted != 0 {
		t.Fatalf("bob restricted = %d, want 0 (isolation)",
			bobDisclosure.Restricted)
	}
	if len(bobSummaries) != 1 ||
		bobSummaries[0].Document.ID != hidden.Document.ID {
		t.Fatalf("bob summaries = %v, want the hidden document",
			summaryIDs(bobSummaries))
	}
}

// TestMosaicRestrictionDistinctFromDenial proves the two withholding reason
// classes are surfaced independently: a plain authorization denial populates
// Suppressed with Restricted zero, while a co-occurrence restriction populates
// Restricted with Suppressed zero. A caller can therefore tell the two apart.
func TestMosaicRestrictionDistinctFromDenial(t *testing.T) {
	f := newFixture(t)
	ingestVisibleAndHidden(t, f)

	mc := f.mosaicClient(t, f.store, authorized.MosaicBudget{
		MaxDomains: 1,
		Window:     time.Hour,
	})
	// Alice lacks a grant for the hidden document: a plain denial.
	_, aliceDisclosure, err := mc.DocumentsWithDisclosure(f.alice(t))
	if err != nil {
		t.Fatal(err)
	}
	if aliceDisclosure.Suppressed == 0 || aliceDisclosure.Restricted != 0 {
		t.Fatalf("denial reason class wrong: suppressed=%d restricted=%d",
			aliceDisclosure.Suppressed, aliceDisclosure.Restricted)
	}
	// Admin is authorized for both but exceeds the budget: a restriction.
	_, adminDisclosure, err := mc.DocumentsWithDisclosure(f.admin(t))
	if err != nil {
		t.Fatal(err)
	}
	if adminDisclosure.Restricted == 0 || adminDisclosure.Suppressed != 0 {
		t.Fatalf("restriction reason class wrong: suppressed=%d restricted=%d",
			adminDisclosure.Suppressed, adminDisclosure.Restricted)
	}
}

// TestMosaicBudgetResetsAfterWindow proves two things at once: within a window,
// already-observed domains stay free while a newly appearing distinct domain is
// restricted (stickiness); and once the window elapses the observed set is
// cleared, so a fresh greedy admission flips which document survives. The base
// document order is [hidden(domainB), visible(domainA)]; pre-observing domainA
// makes the flip after reset unmistakable.
func TestMosaicBudgetResetsAfterWindow(t *testing.T) {
	f := newFixture(t)
	window := time.Minute
	start := f.clock.Now()

	visible, err := f.clientA.Ingest(f.admin(t), explorer.Source{
		URI:       "file:///visible.txt",
		Title:     "Visible",
		MediaType: explorer.MediaTypeText,
		Content:   "alpha",
	})
	if err != nil {
		t.Fatal(err)
	}

	mc := f.mosaicClient(t, f.store, authorized.MosaicBudget{
		MaxDomains: 1,
		Window:     window,
	})
	// Only the visible document exists: admin observes domainA, nothing withheld.
	initial, initialDisclosure, err := mc.DocumentsWithDisclosure(f.admin(t))
	if err != nil {
		t.Fatal(err)
	}
	if initialDisclosure.Restricted != 0 || len(initial) != 1 {
		t.Fatalf("initial read: restricted=%d summaries=%v",
			initialDisclosure.Restricted, summaryIDs(initial))
	}

	hidden, err := f.clientB.Ingest(f.admin(t), explorer.Source{
		URI:       "file:///hidden.txt",
		Title:     "Hidden alpha beta",
		MediaType: explorer.MediaTypeText,
		Content:   "alpha beta",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Still inside the window: domainA is already observed (free); the newly
	// appearing domainB is the one restricted, so the visible document survives.
	sticky, stickyDisclosure, err := mc.DocumentsWithDisclosure(f.admin(t))
	if err != nil {
		t.Fatal(err)
	}
	if stickyDisclosure.Restricted != 1 {
		t.Fatalf("sticky restricted = %d, want 1", stickyDisclosure.Restricted)
	}
	if len(sticky) != 1 || sticky[0].Document.ID != visible.Document.ID {
		t.Fatalf("sticky summaries = %v, want the pre-observed visible document",
			summaryIDs(sticky))
	}

	// Advance beyond the window: the observed set is cleared, so a fresh greedy
	// admission over [hidden(domainB), visible(domainA)] keeps the hidden
	// document and restricts the visible one — the flip proves the reset.
	f.clock.Set(start.Add(2 * window))
	reset, resetDisclosure, err := mc.DocumentsWithDisclosure(f.admin(t))
	if err != nil {
		t.Fatal(err)
	}
	if resetDisclosure.Restricted != 1 {
		t.Fatalf("reset restricted = %d, want 1", resetDisclosure.Restricted)
	}
	if len(reset) != 1 || reset[0].Document.ID != hidden.Document.ID {
		t.Fatalf("reset summaries = %v, want the hidden document after reset",
			summaryIDs(reset))
	}
}

// TestMosaicDisabledByDefault proves a zero budget leaves the pre-mosaic read
// path unchanged: every authorized document is returned and nothing is reported
// as restricted.
func TestMosaicDisabledByDefault(t *testing.T) {
	f := newFixture(t)
	ingestVisibleAndHidden(t, f)

	summaries, disclosure, err := f.clientA.DocumentsWithDisclosure(f.admin(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 2 {
		t.Fatalf("admin summaries = %v, want both documents",
			summaryIDs(summaries))
	}
	if disclosure.Restricted != 0 {
		t.Fatalf("disabled restricted = %d, want 0", disclosure.Restricted)
	}
}

// ledgerlessStore embeds only the PolicyStore interface, so it deliberately
// does not satisfy CoOccurrenceLedger even though the value beneath it does.
type ledgerlessStore struct {
	authorized.PolicyStore
}

// TestMosaicClientRequiresLedgerAndWindow proves an enabled-but-unbacked budget
// fails closed at construction rather than silently disabling the control.
func TestMosaicClientRequiresLedgerAndWindow(t *testing.T) {
	f := newFixture(t)

	// A store that cannot persist co-occurrence state must be rejected.
	ledgerless := ledgerlessStore{PolicyStore: authorized.NewMemoryPolicyStore()}
	if _, err := authorized.NewClient(f.mosaicConfig(
		t, ledgerless, authorized.MosaicBudget{MaxDomains: 1, Window: time.Hour},
	)); err == nil {
		t.Fatal("expected NewClient to reject a store without a ledger")
	}

	// A non-positive window with the control enabled must be rejected.
	if _, err := authorized.NewClient(f.mosaicConfig(
		t, authorized.NewMemoryPolicyStore(),
		authorized.MosaicBudget{MaxDomains: 1, Window: 0},
	)); err == nil {
		t.Fatal("expected NewClient to reject a non-positive window")
	}
}

// TestMosaicBudgetSurvivesDurableRestart proves the co-occurrence ledger is
// persisted through Shoal's own storage engine: after consuming budget,
// closing, and reopening the durable store, the previously observed domain is
// still free while the later distinct domain remains restricted.
func TestMosaicBudgetSurvivesDurableRestart(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()

	durable, err := authorized.OpenDurablePolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	ingestA := f.newClient(t, f.base, durable, f.sourceA, f.policyA, nil)
	ingestB := f.newClient(t, f.base, durable, f.sourceB, f.policyB, nil)

	visible, err := ingestA.Ingest(f.admin(t), explorer.Source{
		URI:       "file:///visible.txt",
		Title:     "Visible",
		MediaType: explorer.MediaTypeText,
		Content:   "alpha",
	})
	if err != nil {
		t.Fatal(err)
	}

	budget := authorized.MosaicBudget{MaxDomains: 1, Window: time.Hour}
	before := f.mosaicClient(t, durable, budget)
	if _, disclosure, err := before.DocumentsWithDisclosure(f.admin(t)); err != nil {
		t.Fatal(err)
	} else if disclosure.Restricted != 0 {
		t.Fatalf("pre-restart restricted = %d, want 0", disclosure.Restricted)
	}

	if _, err := ingestB.Ingest(f.admin(t), explorer.Source{
		URI:       "file:///hidden.txt",
		Title:     "Hidden alpha beta",
		MediaType: explorer.MediaTypeText,
		Content:   "alpha beta",
	}); err != nil {
		t.Fatal(err)
	}
	if err := durable.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := authorized.OpenDurablePolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()

	after := f.mosaicClient(t, reopened, budget)
	summaries, disclosure, err := after.DocumentsWithDisclosure(f.admin(t))
	if err != nil {
		t.Fatal(err)
	}
	// The reloaded ledger still holds domainA as observed, so the visible
	// document is free and the later, distinct hidden document is restricted.
	// Had the ledger not survived the restart, a fresh greedy admission would
	// instead keep the hidden document (first in base order).
	if disclosure.Restricted != 1 {
		t.Fatalf("post-restart restricted = %d, want 1", disclosure.Restricted)
	}
	if len(summaries) != 1 || summaries[0].Document.ID != visible.Document.ID {
		t.Fatalf("post-restart summaries = %v, want the pre-observed visible document",
			summaryIDs(summaries))
	}
}
