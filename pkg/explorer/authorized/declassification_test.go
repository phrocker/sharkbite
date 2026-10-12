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

// Relabel narrowing (declassification) audit, #630.

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/devbackfill"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func (f *fixture) declassificationClient(
	t testing.TB,
	store authorized.PolicyStore,
	requireReason bool,
) *authorized.Client {
	t.Helper()
	return f.declassificationClientOver(t, f.base, store, requireReason)
}

func (f *fixture) declassificationClientOver(
	t testing.TB,
	base explorer.Client,
	store authorized.PolicyStore,
	requireReason bool,
) *authorized.Client {
	t.Helper()
	selector, err := authorized.NewStaticPolicySelector(f.sourceA, f.policyA)
	if err != nil {
		t.Fatal(err)
	}
	client, err := authorized.NewClient(authorized.Config{
		Base:                          base,
		VectorScorer:                  trustedVectorScorer(f.base),
		InteractionWriter:             trustedInteractionWriter(f.base),
		InteractionReader:             trustedInteractionReader(f.base),
		SnapshotValidator:             trustedSnapshotValidator(f.base),
		DerivedAssertionReader:        trustedDerivedAssertionReader(f.base),
		FoldStore:                     trustedFoldStore(f.base),
		Resolver:                      f.authority.Resolver(),
		PolicySelector:                selector,
		PolicyStore:                   store,
		GenerationReader:              f.reader,
		Clock:                         f.clock.Now,
		RequireDeclassificationReason: requireReason,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func declassifications(
	t *testing.T,
	store authorized.PolicyStore,
) []authorized.DeclassificationRecord {
	t.Helper()
	records, err := store.Declassifications(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return records
}

func withReason(t *testing.T, ctx context.Context, reason string) context.Context {
	t.Helper()
	ctx, err := authorized.WithDeclassificationReason(ctx, reason)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

// TestDeclassificationAuditsLabelRemoval pins the audit an ingest that drops
// a label leaves: one intent and one commit record of one attempt, carrying
// the principal, request, document, revisions and both rules.
func TestDeclassificationAuditsLabelRemoval(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		f := newFixture(t)
		client := f.declassificationClient(t, store, false)
		const uri = "file:///declassify/removal.txt"
		first, err := client.Ingest(f.labelIngester(t, "owner", "x", "secret"),
			labelledSource(uri, "labelled x and secret", "x&secret"))
		if err != nil {
			t.Fatal(err)
		}
		if records := declassifications(t, store); len(records) != 0 {
			t.Fatalf("a first ingest was audited as a declassification: %+v", records)
		}
		ctx := withReason(t, f.labelIngester(t, "declassifier", "x", "secret"),
			"ticket:SEC-1")
		second, err := client.Ingest(ctx,
			labelledSource(uri, "now only x", "x"))
		if err != nil {
			t.Fatal(err)
		}
		records := declassifications(t, store)
		if len(records) != 2 {
			t.Fatalf("declassification records = %d, want intent and commit: %+v",
				len(records), records)
		}
		intent, commit := records[0], records[1]
		if intent.Phase != authorized.DeclassificationIntent ||
			commit.Phase != authorized.DeclassificationPreCommit {
			t.Fatalf("phases = %q, %q", intent.Phase, commit.Phase)
		}
		if intent.AttemptID == "" || intent.AttemptID != commit.AttemptID {
			t.Fatalf("attempt IDs = %q, %q, want one shared attempt",
				intent.AttemptID, commit.AttemptID)
		}
		for _, record := range records {
			if record.Subject != "declassifier" ||
				record.Actor != "declassifier-actor" ||
				record.RequestID != "declassifier-request" ||
				record.SourceURI != uri ||
				record.ReasonCode != "ticket:SEC-1" ||
				!record.OccurredAt.Equal(f.clock.Now()) {
				t.Fatalf("%s record attribution = %+v", record.Phase, record)
			}
			if got := record.RemovedLabels(); !reflect.DeepEqual(got, []string{"secret"}) {
				t.Fatalf("%s removed labels = %v, want [secret]", record.Phase, got)
			}
			if len(record.PriorRules) != 1 ||
				record.PriorRules[0].String() == record.Rule.String() {
				t.Fatalf("%s rules = %v -> %v", record.Phase, record.PriorRules, record.Rule)
			}
		}
		if commit.DocumentID != second.Document.ID ||
			commit.RevisionID != second.Revision.ID ||
			commit.PriorRevisionID != first.Revision.ID {
			t.Fatalf("commit identities = %+v, want document %s revision %s prior %s",
				commit, second.Document.ID, second.Revision.ID, first.Revision.ID)
		}
		decision, err := f.authority.Resolver().Resolve(ctx)
		if err != nil {
			t.Fatal(err)
		}
		fingerprint, err := auth.AuthorizationFingerprint(decision)
		if err != nil {
			t.Fatal(err)
		}
		if commit.AuthorizationFingerprint != fingerprint {
			t.Fatal("commit record does not carry the principal's fingerprint")
		}
	})
}

// TestDeclassificationAuditsLabelSwap pins that replacing one label with
// another is a narrowing: holders of the new label who lack the old one gain
// access.
func TestDeclassificationAuditsLabelSwap(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		f := newFixture(t)
		client := f.declassificationClient(t, store, false)
		const uri = "file:///declassify/swap.txt"
		if _, err := client.Ingest(f.labelIngester(t, "owner", "x"),
			labelledSource(uri, "labelled x", "x")); err != nil {
			t.Fatal(err)
		}
		if _, err := client.Ingest(f.labelIngester(t, "both", "x", "Secret"),
			labelledSource(uri, "labelled Secret", "Secret")); err != nil {
			t.Fatal(err)
		}
		records := declassifications(t, store)
		if len(records) != 2 {
			t.Fatalf("swap records = %d, want 2", len(records))
		}
		if got := records[1].RemovedLabels(); !reflect.DeepEqual(got, []string{"x"}) {
			t.Fatalf("removed = %v, want [x]", got)
		}
		// Case is never folded: secret is a different label from Secret,
		// so this is a narrowing too.
		if _, err := client.Ingest(f.labelIngester(t, "case", "Secret", "secret"),
			labelledSource(uri, "labelled secret", "secret")); err != nil {
			t.Fatal(err)
		}
		records = declassifications(t, store)
		if len(records) != 4 {
			t.Fatalf("case swap records = %d, want 4", len(records))
		}
		if got := records[3].RemovedLabels(); !reflect.DeepEqual(got, []string{"Secret"}) {
			t.Fatalf("removed = %v, want [Secret]", got)
		}
	})
}

// TestDeclassificationFullyUnlabels pins that dropping every label, the
// plainest declassification, is audited.
func TestDeclassificationFullyUnlabels(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		f := newFixture(t)
		client := f.declassificationClient(t, store, false)
		const uri = "file:///declassify/unlabel.txt"
		if _, err := client.Ingest(f.labelIngester(t, "owner", "a", "b"),
			labelledSource(uri, "labelled a b", "a&b")); err != nil {
			t.Fatal(err)
		}
		second, err := client.Ingest(f.labelIngester(t, "owner", "a", "b"),
			labelledSource(uri, "public now", ""))
		if err != nil {
			t.Fatal(err)
		}
		records := declassifications(t, store)
		if len(records) != 2 {
			t.Fatalf("records = %d, want 2", len(records))
		}
		if got := records[1].RemovedLabels(); !reflect.DeepEqual(got, []string{"a", "b"}) {
			t.Fatalf("removed = %v, want [a b]", got)
		}
		if _, err := client.Document(f.labelIngester(t, "public-reader"),
			second.Document.ID, ""); err != nil {
			t.Fatalf("declassified document unreadable to a source holder: %v", err)
		}
	})
}

// TestDeclassificationIgnoresOrdinaryAndWideningIngests pins that a first
// ingest, a content change under the same labels, an unchanged re-ingest, and
// adding labels are never audited.
func TestDeclassificationIgnoresOrdinaryAndWideningIngests(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		f := newFixture(t)
		client := f.declassificationClient(t, store, true)
		const uri = "file:///declassify/ordinary.txt"
		owner := f.labelIngester(t, "owner", "x", "y")
		for _, step := range []explorer.Source{
			labelledSource(uri, "unlabelled", ""),
			labelledSource(uri, "unlabelled, edited", ""),
			labelledSource(uri, "unlabelled, edited", ""),
			labelledSource(uri, "x", "x"),
			labelledSource(uri, "x, edited", "x"),
			labelledSource(uri, "x and y", "y&x"),
			labelledSource(uri, "x and y, reordered", "x&y&x"),
		} {
			// The reason policy is on and no reason is given, so any of
			// these being treated as a narrowing would also fail it.
			if _, err := client.Ingest(owner, step); err != nil {
				t.Fatalf("ingest %q: %v", step.Content, err)
			}
		}
		if records := declassifications(t, store); len(records) != 0 {
			t.Fatalf("ordinary or widening ingests were audited: %+v", records)
		}
		// Another document, unrelated to this one, is not a narrowing of it.
		if _, err := client.Ingest(owner,
			labelledSource("file:///declassify/other.txt", "other", "")); err != nil {
			t.Fatal(err)
		}
		if records := declassifications(t, store); len(records) != 0 {
			t.Fatalf("an unrelated first ingest was audited: %+v", records)
		}
	})
}

// TestDeclassificationReasonPolicy pins the optional operator policy: with
// it on, a narrowing without a reason is refused before anything is written,
// and one with a reason proceeds and records it.
func TestDeclassificationReasonPolicy(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		f := newFixture(t)
		client := f.declassificationClient(t, store, true)
		const uri = "file:///declassify/reason.txt"
		first, err := client.Ingest(f.labelIngester(t, "owner", "secret"),
			labelledSource(uri, "secret", "secret"))
		if err != nil {
			t.Fatal(err)
		}
		before := baseDocumentCount(t, f)
		holder := f.labelIngester(t, "holder", "secret")
		if _, err := client.Ingest(holder, labelledSource(uri, "public", "")); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
			t.Fatalf("narrowing without a reason = %v, want invalid argument", err)
		}
		if records := declassifications(t, store); len(records) != 0 {
			t.Fatalf("a refused narrowing was recorded: %+v", records)
		}
		summaries, err := client.Documents(f.labelIngester(t, "lister", "secret"))
		if err != nil {
			t.Fatal(err)
		}
		if len(summaries) != 1 || summaries[0].Revision.ID != first.Revision.ID ||
			baseDocumentCount(t, f) != before {
			t.Fatalf("a refused narrowing changed the document: %+v", summaries)
		}
		if _, err := client.Document(f.labelIngester(t, "reader"), first.Document.ID, ""); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			t.Fatalf("refused narrowing exposed the document: %v", err)
		}
		// A principal without the label is refused as before, and learns
		// nothing about the reason policy.
		if _, err := client.Ingest(f.labelIngester(t, "outsider"),
			labelledSource(uri, "public", "")); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			t.Fatalf("outsider narrowing = %v, want not found", err)
		}
		if _, err := client.Ingest(withReason(t, holder, "legal-hold-release"),
			labelledSource(uri, "public", "")); err != nil {
			t.Fatalf("narrowing with a reason: %v", err)
		}
		records := declassifications(t, store)
		if len(records) != 2 || records[0].ReasonCode != "legal-hold-release" ||
			records[1].ReasonCode != "legal-hold-release" {
			t.Fatalf("records = %+v, want the reason on both", records)
		}
	})
}

func TestDeclassificationReasonValidation(t *testing.T) {
	for _, reason := range []string{"", "has space", "semi;colon", string(make([]byte, authorized.MaxDeclassificationReasonBytes+1))} {
		if _, err := authorized.WithDeclassificationReason(context.Background(), reason); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
			t.Fatalf("reason %q = %v, want invalid argument", reason, err)
		}
	}
}

// failingDeclassificationStore fails RecordDeclassification on the listed
// call numbers (1-based) and delegates everything else.
type failingDeclassificationStore struct {
	authorized.PolicyStore
	mu    sync.Mutex
	calls int
	fail  map[int]bool
}

func (s *failingDeclassificationStore) RecordDeclassification(
	ctx context.Context,
	record authorized.DeclassificationRecord,
) error {
	s.mu.Lock()
	s.calls++
	fail := s.fail[s.calls]
	s.mu.Unlock()
	if fail {
		return shoal.NewError(shoal.ErrorUnavailable, "audit sink down")
	}
	return s.PolicyStore.RecordDeclassification(ctx, record)
}

// TestDeclassificationIntentFailureBlocksTheRelabel pins audit-before-commit
// at the source claim: if the intent cannot be recorded, nothing is written.
func TestDeclassificationIntentFailureBlocksTheRelabel(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, inner authorized.PolicyStore) {
		f := newFixture(t)
		store := &failingDeclassificationStore{
			PolicyStore: inner, fail: map[int]bool{1: true}}
		client := f.declassificationClient(t, store, false)
		const uri = "file:///declassify/intent-failure.txt"
		first, err := client.Ingest(f.labelIngester(t, "owner", "secret"),
			labelledSource(uri, "secret", "secret"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Ingest(f.labelIngester(t, "owner", "secret"),
			labelledSource(uri, "public", "")); err == nil {
			t.Fatal("a narrowing whose audit failed succeeded")
		}
		// A narrowing re-ingest adds a revision, not a document, so the
		// base's current revision is what must be unchanged.
		summaries, err := f.base.Documents(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(summaries) != 1 || summaries[0].Revision.ID != first.Revision.ID {
			t.Fatalf("a narrowing whose intent audit failed reached the base: %+v", summaries)
		}
		claim, ok, err := inner.SourceClaim(context.Background(), uri)
		if err != nil || !ok || claim.Pending {
			t.Fatalf("claim after a failed intent audit = %+v, %v, %v", claim, ok, err)
		}
		if _, err := client.Document(f.labelIngester(t, "reader"), first.Document.ID, ""); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			t.Fatalf("failed narrowing exposed the document: %v", err)
		}
		if records := declassifications(t, inner); len(records) != 0 {
			t.Fatalf("records = %+v", records)
		}
	})
}

// TestDeclassificationCommitFailureBlocksTheRegistration pins
// audit-before-commit at the visibility commit, and that the commit check is
// independent of the claim: after a failed attempt has already swapped the
// claim to the narrow rule, a retry is still audited against the current
// registration before it is let in.
func TestDeclassificationCommitFailureBlocksTheRegistration(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, inner authorized.PolicyStore) {
		f := newFixture(t)
		store := &failingDeclassificationStore{
			PolicyStore: inner, fail: map[int]bool{2: true}}
		client := f.declassificationClient(t, store, false)
		const uri = "file:///declassify/commit-failure.txt"
		first, err := client.Ingest(f.labelIngester(t, "owner", "secret"),
			labelledSource(uri, "secret", "secret"))
		if err != nil {
			t.Fatal(err)
		}
		public := labelledSource(uri, "public", "")
		if _, err := client.Ingest(f.labelIngester(t, "owner", "secret"), public); err == nil {
			t.Fatal("a narrowing whose commit audit failed succeeded")
		}
		current, ok, err := inner.CurrentRevision(context.Background(), first.Document.ID)
		if err != nil || !ok || current.RevisionID != first.Revision.ID {
			t.Fatalf("current registration after a failed commit audit = %+v, %v, %v",
				current, ok, err)
		}
		reader := f.labelIngester(t, "reader")
		records := declassifications(t, inner)
		if len(records) != 1 || records[0].Phase != authorized.DeclassificationIntent {
			t.Fatalf("records after a failed commit audit = %+v", records)
		}

		// The claim now holds the narrow rule, so the claim-time check sees
		// no narrowing on a retry. The commit-time check still does, against
		// the current registration.
		second, err := client.Ingest(f.labelIngester(t, "retrier", "secret"), public)
		if err != nil {
			t.Fatalf("retry: %v", err)
		}
		records = declassifications(t, inner)
		if len(records) != 2 {
			t.Fatalf("records after the retry = %+v", records)
		}
		retry := records[1]
		if retry.Phase != authorized.DeclassificationPreCommit ||
			retry.Subject != "retrier" ||
			retry.AttemptID == records[0].AttemptID ||
			retry.DocumentID != second.Document.ID ||
			retry.RevisionID != second.Revision.ID ||
			retry.PriorRevisionID != first.Revision.ID ||
			!reflect.DeepEqual(retry.RemovedLabels(), []string{"secret"}) {
			t.Fatalf("retry record = %+v", retry)
		}
		if _, err := client.Document(reader, second.Document.ID, ""); err != nil {
			t.Fatalf("committed narrowing: %v", err)
		}
	})
}

// TestDeclassificationAuditsLegacyUnclaimedDocument covers a document
// registered without a source claim (the development backfill): its current
// registration is the replaced rule.
func TestDeclassificationAuditsLegacyUnclaimedDocument(t *testing.T) {
	f := newFixture(t)
	const uri = "file:///declassify/legacy.txt"
	if _, err := f.base.Ingest(context.Background(),
		labelledSource(uri, "labelled before the catalog", "secret")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.clientA.BackfillExistingDocumentsForDevelopment(
		f.labelIngester(t, "operator", "secret"), devbackfill.NewCapability()); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := f.store.SourceClaim(context.Background(), uri); err != nil || ok {
		t.Fatalf("backfill created a claim (%v): the legacy path is not exercised", err)
	}
	if _, err := f.clientA.Ingest(f.labelIngester(t, "owner", "secret"),
		labelledSource(uri, "public", "")); err != nil {
		t.Fatal(err)
	}
	records := declassifications(t, f.store)
	if len(records) != 2 || records[0].Phase != authorized.DeclassificationIntent ||
		records[0].DocumentID == "" || records[0].PriorRevisionID == "" ||
		!reflect.DeepEqual(records[0].RemovedLabels(), []string{"secret"}) {
		t.Fatalf("legacy records = %+v", records)
	}
}

// TestDeclassificationRecordsSurviveReopen pins durability.
func TestDeclassificationRecordsSurviveReopen(t *testing.T) {
	dir := t.TempDir()
	store, err := authorized.OpenDurablePolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t)
	client := f.declassificationClient(t, store, false)
	const uri = "file:///declassify/durable.txt"
	if _, err := client.Ingest(f.labelIngester(t, "owner", "secret"),
		labelledSource(uri, "secret", "secret")); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Ingest(withReason(t, f.labelIngester(t, "owner", "secret"), "r"),
		labelledSource(uri, "public", "")); err != nil {
		t.Fatal(err)
	}
	want := declassifications(t, store)
	if len(want) != 2 {
		t.Fatalf("records = %d", len(want))
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := authorized.OpenDurablePolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	got := declassifications(t, reopened)
	if len(got) != len(want) {
		t.Fatalf("reopened records = %d, want %d", len(got), len(want))
	}
	for index := range want {
		if got[index].AttemptID != want[index].AttemptID ||
			got[index].Phase != want[index].Phase ||
			got[index].RevisionID != want[index].RevisionID ||
			got[index].ReasonCode != want[index].ReasonCode ||
			got[index].AuthorizationFingerprint != want[index].AuthorizationFingerprint ||
			!got[index].OccurredAt.Equal(want[index].OccurredAt) ||
			got[index].Rule.String() != want[index].Rule.String() ||
			!reflect.DeepEqual(got[index].RemovedLabels(), want[index].RemovedLabels()) {
			t.Fatalf("record %d after reopen = %+v, want %+v", index, got[index], want[index])
		}
	}
}

// TestDeclassificationStoreRefusesNonNarrowingRecords pins that a store
// cannot be handed a record that describes no narrowing.
func TestDeclassificationStoreRefusesNonNarrowingRecords(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		policy, err := auth.NewPolicy(auth.PolicyConfig{AuthorizationDomain: []byte("domain-one"), SourceID: []byte("source-a"), GrantPolicyID: []byte("policy-a"), Epoch: 1})
		if err != nil {
			t.Fatal(err)
		}
		rule, err := authorized.NewAccessRule(policy)
		if err != nil {
			t.Fatal(err)
		}
		labelled, err := authorized.LabelRule(policy, []string{"secret"})
		if err != nil {
			t.Fatal(err)
		}
		record := authorized.DeclassificationRecord{
			AttemptID: "a", Phase: authorized.DeclassificationIntent,
			OccurredAt: time.Date(2026, time.August, 26, 18, 0, 0, 0, time.UTC), SourceURI: "file:///x",
			PriorRules: []authorized.AccessRule{rule}, Rule: labelled,
		}
		if err := store.RecordDeclassification(context.Background(), record); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
			t.Fatalf("widening record = %v, want invalid argument", err)
		}
		record.PriorRules, record.Rule = []authorized.AccessRule{labelled}, rule
		if err := store.RecordDeclassification(context.Background(), record); err != nil {
			t.Fatal(err)
		}
	})
}

// TestDeclassificationAuditsPendingClaimRetry covers an interrupted
// narrowing: the base committed but reported an indeterminate outcome, so the
// claim is pending with the narrow Rule and the replaced PreviousRule. A
// retry must be recognised as a narrowing at claim time from PreviousRule, so
// the reason policy refuses it before the base is touched again.
func TestDeclassificationAuditsPendingClaimRetry(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		f := newFixture(t)
		const uri = "file:///declassify/pending.txt"
		client := f.declassificationClient(t, store, true)
		if _, err := client.Ingest(f.labelIngester(t, "owner", "secret"),
			labelledSource(uri, "secret", "secret")); err != nil {
			t.Fatal(err)
		}
		indeterminate := &ingestOverrideClient{
			Client: f.base,
			ingest: func(ctx context.Context, source explorer.Source) (explorer.IngestResult, error) {
				if _, err := f.base.Ingest(ctx, source); err != nil {
					return explorer.IngestResult{}, err
				}
				return explorer.IngestResult{}, explorer.MarkIndeterminateCommit(
					shoal.NewError(shoal.ErrorUnavailable, "indeterminate"))
			},
		}
		flaky := f.declassificationClientOver(t, indeterminate, store, true)
		public := labelledSource(uri, "public", "")
		holder := f.labelIngester(t, "holder", "secret")
		if _, err := flaky.Ingest(withReason(t, holder, "first"), public); !explorer.IsIndeterminateCommit(err) {
			t.Fatalf("interrupted narrowing = %v", err)
		}
		claim, ok, err := store.SourceClaim(context.Background(), uri)
		if err != nil || !ok || !claim.Pending || claim.PreviousRule == nil {
			t.Fatalf("claim = %+v %v %v, want pending with a previous rule", claim, ok, err)
		}
		if records := declassifications(t, store); len(records) != 1 ||
			records[0].Phase != authorized.DeclassificationIntent {
			t.Fatalf("records after the interrupted narrowing = %+v", records)
		}
		// Without a reason the retry is refused at claim time.
		if _, err := client.Ingest(holder, public); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
			t.Fatalf("retry without a reason = %v, want invalid argument", err)
		}
		if records := declassifications(t, store); len(records) != 1 {
			t.Fatalf("a refused retry was recorded: %+v", records)
		}
		if _, err := client.Ingest(withReason(t, holder, "retry"), public); err != nil {
			t.Fatalf("retry with a reason: %v", err)
		}
		records := declassifications(t, store)
		if len(records) != 3 || records[1].Phase != authorized.DeclassificationIntent ||
			records[1].ReasonCode != "retry" ||
			!reflect.DeepEqual(records[1].RemovedLabels(), []string{"secret"}) ||
			records[2].Phase != authorized.DeclassificationPreCommit ||
			records[2].AttemptID != records[1].AttemptID {
			t.Fatalf("records after the retry = %+v", records)
		}
	})
}

// TestDeclassificationReconciliationIsRecordedNotRefused covers the state a
// failed commit leaves behind: the claim holds the narrow rule while the
// registration keeps the labelled one. The claim is the mutation authority,
// so a claim holder may complete it (as before #630), even one outside the
// labelled rule. With the reason policy on, that completion must not be
// refused, since a refusal would tell such a principal the registration
// carries labels; the narrowing was decided, reason-checked and recorded at
// the earlier intent. The completion is recorded, attributed to whoever
// made it current.
func TestDeclassificationReconciliationIsRecordedNotRefused(t *testing.T) {
	f := newFixture(t)
	inner := authorized.NewMemoryPolicyStore()
	store := &failingDeclassificationStore{
		PolicyStore: inner, fail: map[int]bool{2: true}}
	client := f.declassificationClient(t, store, true)
	const uri = "file:///declassify/reconcile.txt"
	first, err := client.Ingest(f.labelIngester(t, "owner", "secret"),
		labelledSource(uri, "secret", "secret"))
	if err != nil {
		t.Fatal(err)
	}
	public := labelledSource(uri, "public", "")
	if _, err := client.Ingest(withReason(t, f.labelIngester(t, "owner", "secret"), "r1"), public); err == nil {
		t.Fatal("commit audit failure was ignored")
	}
	current, ok, err := inner.CurrentRevision(context.Background(), first.Document.ID)
	if err != nil || !ok || current.RevisionID != first.Revision.ID {
		t.Fatalf("failed commit audit registered the narrowing: %+v %v %v", current, ok, err)
	}
	reconciled, err := client.Ingest(f.labelIngester(t, "nobody"), public)
	if err != nil {
		t.Fatalf("claim holder reconciliation without a reason = %v", err)
	}
	records := declassifications(t, inner)
	if len(records) != 2 ||
		records[0].Phase != authorized.DeclassificationIntent ||
		records[0].Subject != "owner" || records[0].ReasonCode != "r1" ||
		records[1].Phase != authorized.DeclassificationPreCommit ||
		records[1].Subject != "nobody" || records[1].ReasonCode != "" ||
		records[1].AttemptID == records[0].AttemptID ||
		records[1].RevisionID != reconciled.Revision.ID ||
		records[1].PriorRevisionID != first.Revision.ID {
		t.Fatalf("records = %+v", records)
	}
}

// TestDeclassificationRecordsAreAppendOnly pins that a second record for one
// (attempt, phase) is refused rather than shadowing the first on disk.
func TestDeclassificationRecordsAreAppendOnly(t *testing.T) {
	dir := t.TempDir()
	durableStore, err := authorized.OpenDurablePolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	durableDirs := map[*authorized.DurablePolicyStore]string{durableStore: dir}
	for _, store := range []authorized.PolicyStore{
		authorized.NewMemoryPolicyStore(), durableStore,
	} {
		policy, err := auth.NewPolicy(auth.PolicyConfig{AuthorizationDomain: []byte("domain-one"), SourceID: []byte("source-a"), GrantPolicyID: []byte("policy-a"), Epoch: 1})
		if err != nil {
			t.Fatal(err)
		}
		rule, err := authorized.NewAccessRule(policy)
		if err != nil {
			t.Fatal(err)
		}
		labelled, err := authorized.LabelRule(policy, []string{"secret"})
		if err != nil {
			t.Fatal(err)
		}
		record := authorized.DeclassificationRecord{
			AttemptID: "a", Phase: authorized.DeclassificationIntent,
			OccurredAt: time.Date(2026, time.August, 26, 18, 0, 0, 0, time.UTC),
			SourceURI:  "file:///x", ReasonCode: "first",
			PriorRules: []authorized.AccessRule{labelled}, Rule: rule,
		}
		if err := store.RecordDeclassification(context.Background(), record); err != nil {
			t.Fatal(err)
		}
		record.ReasonCode = "overwrite"
		if err := store.RecordDeclassification(context.Background(), record); !shoal.IsErrorCode(err, shoal.ErrorConflict) {
			t.Fatalf("duplicate record = %v, want conflict", err)
		}
		records := declassifications(t, store)
		if len(records) != 1 || records[0].ReasonCode != "first" {
			t.Fatalf("records = %+v", records)
		}
		durable, ok := store.(*authorized.DurablePolicyStore)
		if !ok {
			continue
		}
		// The refused duplicate must not have reached the disk either,
		// where it would shadow the original on the next open.
		dir := durableDirs[durable]
		if err := durable.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := authorized.OpenDurablePolicyStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = reopened.Close() })
		records = declassifications(t, reopened)
		if len(records) != 1 || records[0].ReasonCode != "first" {
			t.Fatalf("records after reopen = %+v", records)
		}
	}
}

// TestDeclassificationDivergedCatalogIsAnIntent covers a URI with no source
// claim and no base document whose catalog still holds a current labelled
// registration (the catalog and base diverged, for example a base restored
// from backup). The claim-time check has nothing to compare with, so the
// pre-commit check is the narrowing's first check: it must authorize the
// principal under the registration it replaces and apply the reason policy,
// not treat it as a reconciliation.
func TestDeclassificationDivergedCatalogIsAnIntent(t *testing.T) {
	f := newFixture(t)
	const uri = "file:///declassify/diverged.txt"
	labelled, err := f.base.Ingest(context.Background(),
		labelledSource(uri, "labelled before the catalog", "secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.clientA.BackfillExistingDocumentsForDevelopment(
		f.labelIngester(t, "operator", "secret"), devbackfill.NewCapability()); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := f.store.SourceClaim(context.Background(), uri); err != nil || ok {
		t.Fatalf("backfill created a claim (%v)", err)
	}
	// A fresh base that does not know the document: the catalog diverges.
	restored, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restored.Close() })
	client := f.declassificationClientOver(t, restored, f.store, true)
	public := labelledSource(uri, "public", "")
	registered := func() shoal.ID {
		t.Helper()
		current, ok, err := f.store.CurrentRevision(context.Background(), labelled.Document.ID)
		if err != nil || !ok {
			t.Fatalf("current registration: %v %v", ok, err)
		}
		return current.RevisionID
	}

	if _, err := client.Ingest(f.labelIngester(t, "outsider"), public); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("outsider replacing a diverged labelled registration = %v, want not found", err)
	}
	// The outsider's refused attempt still left a committed claim under the
	// narrow rule (the claim commits on the base write, #665). That claim
	// passed no reason check, so it must not turn the holder's narrowing
	// into a reason-free reconciliation.
	if claim, ok, err := f.store.SourceClaim(context.Background(), uri); err != nil || !ok || claim.Pending {
		t.Fatalf("claim after the outsider = %+v %v %v", claim, ok, err)
	}
	if _, err := client.Ingest(f.labelIngester(t, "holder", "secret"), public); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("holder without a reason = %v, want invalid argument", err)
	}
	if got := registered(); got != labelled.Revision.ID {
		t.Fatalf("a refused narrowing replaced the registration: %s", got)
	}
	if records := declassifications(t, f.store); len(records) != 0 {
		t.Fatalf("refused narrowings were recorded: %+v", records)
	}
	result, err := client.Ingest(withReason(t, f.labelIngester(t, "holder", "secret"), "restore"), public)
	if err != nil {
		t.Fatalf("holder with a reason: %v", err)
	}
	records := declassifications(t, f.store)
	if len(records) != 1 || records[0].Phase != authorized.DeclassificationPreCommit ||
		records[0].Subject != "holder" || records[0].ReasonCode != "restore" ||
		records[0].RevisionID != result.Revision.ID ||
		records[0].PriorRevisionID != labelled.Revision.ID ||
		!reflect.DeepEqual(records[0].RemovedLabels(), []string{"secret"}) {
		t.Fatalf("records = %+v", records)
	}
}

// TestDeclassificationStaleIntentNeverReconciles pins F1 of the third review:
// an intent on file that the current claim does not descend from (left by a
// CAS conflict, a failed base write or an old completed narrowing) never makes
// a pre-commit narrowing a reconciliation, even when it names exactly this
// URI, rule and registration. In the diverged-catalog state an outsider is
// refused as absent and a label holder still needs a reason.
func TestDeclassificationStaleIntentNeverReconciles(t *testing.T) {
	policy, err := auth.NewPolicy(auth.PolicyConfig{
		AuthorizationDomain: []byte("domain-one"), SourceID: []byte("source-a"),
		GrantPolicyID: []byte("policy-a"), Epoch: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	rule := func(labels ...string) authorized.AccessRule {
		t.Helper()
		built, err := authorized.LabelRule(policy, labels)
		if err != nil {
			t.Fatal(err)
		}
		return built
	}
	const uri = "file:///declassify/stale-intent.txt"
	for _, seed := range []struct {
		name        string
		prior, next authorized.AccessRule
	}{
		{"exactly matching", rule("secret"), rule()},
		{"other prior rule", rule("x"), rule()},
		{"other new rule", rule("secret"), rule("y")},
	} {
		t.Run(seed.name, func(t *testing.T) {
			f := newFixture(t)
			if err := f.store.RecordDeclassification(context.Background(),
				authorized.DeclassificationRecord{
					AttemptID: "stale", Phase: authorized.DeclassificationIntent,
					OccurredAt: f.clock.Now(), SourceURI: uri,
					PriorRules: []authorized.AccessRule{seed.prior}, Rule: seed.next,
				}); err != nil {
				t.Fatal(err)
			}
			if _, err := f.base.Ingest(context.Background(),
				labelledSource(uri, "labelled before the catalog", "secret")); err != nil {
				t.Fatal(err)
			}
			if _, err := f.clientA.BackfillExistingDocumentsForDevelopment(
				f.labelIngester(t, "operator", "secret"), devbackfill.NewCapability()); err != nil {
				t.Fatal(err)
			}
			restored, err := explorer.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = restored.Close() })
			client := f.declassificationClientOver(t, restored, f.store, true)
			public := labelledSource(uri, "public", "")
			if _, err := client.Ingest(f.labelIngester(t, "outsider"), public); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
				t.Fatalf("outsider with a stale intent on file = %v, want not found", err)
			}
			if _, err := client.Ingest(f.labelIngester(t, "holder", "secret"), public); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
				t.Fatalf("holder without a reason, stale intent on file = %v, want invalid argument", err)
			}
		})
	}
}

// TestDeclassificationStricterRetryReconciles pins F2 of the third review: a
// retry completing an earlier narrowing (#665) may choose a stricter rule
// than the one its intent recorded, and is still that narrowing's
// reconciliation. The claim lineage carries forward to the retry's claim
// version, including across a durable reopen.
func TestDeclassificationStricterRetryReconciles(t *testing.T) {
	dir := t.TempDir()
	durable, err := authorized.OpenDurablePolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t)
	store := &failingDeclassificationStore{
		PolicyStore: durable, fail: map[int]bool{2: true}}
	client := f.declassificationClient(t, store, true)
	const uri = "file:///declassify/stricter-retry.txt"
	first, err := client.Ingest(f.labelIngester(t, "owner", "secret", "x"),
		labelledSource(uri, "secret and x", "secret&x"))
	if err != nil {
		t.Fatal(err)
	}
	// The owner drops x with a reason; the pre-commit audit fails, so the
	// claim holds {secret} while the registration keeps {secret, x}.
	if _, err := client.Ingest(withReason(t, f.labelIngester(t, "owner", "secret", "x"), "drop-x"),
		labelledSource(uri, "secret only", "secret")); err == nil {
		t.Fatal("pre-commit audit failure was ignored")
	}
	if err := durable.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := authorized.OpenDurablePolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	client = f.declassificationClient(t, reopened, true)

	// A retrier holding secret and y but not x, with no reason, completes
	// it under the stricter {secret, y}.
	retried, err := client.Ingest(f.labelIngester(t, "retrier", "secret", "y"),
		labelledSource(uri, "secret and y", "secret&y"))
	if err != nil {
		t.Fatalf("stricter retry of a recorded narrowing = %v, want reconciliation", err)
	}
	current, ok, err := reopened.CurrentRevision(context.Background(), first.Document.ID)
	if err != nil || !ok || current.RevisionID != retried.Revision.ID {
		t.Fatalf("current = %+v %v %v", current, ok, err)
	}
	records := declassifications(t, reopened)
	if len(records) != 2 || records[1].Phase != authorized.DeclassificationPreCommit ||
		records[1].Subject != "retrier" ||
		!reflect.DeepEqual(records[1].RemovedLabels(), []string{"x"}) {
		t.Fatalf("records = %+v", records)
	}
}

// TestDeclassificationBoundIntentMustCoverTheNarrowing pins the match a bound
// intent must make: the selected rule keeps every component of the intent's
// rule, and the current registration's rule is exactly one of its replaced
// rules. A claim bound to an intent that fails either is not a
// reconciliation.
func TestDeclassificationBoundIntentMustCoverTheNarrowing(t *testing.T) {
	policy, err := auth.NewPolicy(auth.PolicyConfig{
		AuthorizationDomain: []byte("domain-one"), SourceID: []byte("source-a"),
		GrantPolicyID: []byte("policy-a"), Epoch: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	rule := func(labels ...string) authorized.AccessRule {
		t.Helper()
		built, err := authorized.LabelRule(policy, labels)
		if err != nil {
			t.Fatal(err)
		}
		return built
	}
	const uri = "file:///declassify/bound-intent.txt"
	for _, seed := range []struct {
		name        string
		prior, next authorized.AccessRule
		reconciles  bool
	}{
		{"covers", rule("secret"), rule(), true},
		{"other prior rule", rule("x"), rule(), false},
		{"intent rule not kept", rule("secret"), rule("y"), false},
	} {
		t.Run(seed.name, func(t *testing.T) {
			f := newFixture(t)
			if _, err := f.base.Ingest(context.Background(),
				labelledSource(uri, "labelled before the catalog", "secret")); err != nil {
				t.Fatal(err)
			}
			if _, err := f.clientA.BackfillExistingDocumentsForDevelopment(
				f.labelIngester(t, "operator", "secret"), devbackfill.NewCapability()); err != nil {
				t.Fatal(err)
			}
			restored, err := explorer.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = restored.Close() })
			client := f.declassificationClientOver(t, restored, f.store, true)
			public := labelledSource(uri, "public", "")
			// The outsider's refused attempt leaves an unbound claim under
			// the public rule. Bind it to the seeded intent directly.
			if _, err := client.Ingest(f.labelIngester(t, "outsider"), public); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
				t.Fatalf("outsider = %v, want not found", err)
			}
			claim, ok, err := f.store.SourceClaim(context.Background(), uri)
			if err != nil || !ok {
				t.Fatalf("claim = %v %v", ok, err)
			}
			if err := f.store.RecordDeclassification(context.Background(),
				authorized.DeclassificationRecord{
					AttemptID: "bound", Phase: authorized.DeclassificationIntent,
					OccurredAt: f.clock.Now(), SourceURI: uri,
					PriorRules: []authorized.AccessRule{seed.prior}, Rule: seed.next,
				}); err != nil {
				t.Fatal(err)
			}
			if err := f.store.BindDeclassificationClaim(
				context.Background(), uri, claim.Version, "bound"); err != nil {
				t.Fatal(err)
			}
			_, err = client.Ingest(f.labelIngester(t, "holder", "secret"), public)
			if seed.reconciles {
				if err != nil {
					t.Fatalf("covered narrowing = %v, want reconciliation", err)
				}
				return
			}
			if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
				t.Fatalf("uncovered narrowing without a reason = %v, want invalid argument", err)
			}
		})
	}
}

// TestDeclassificationClaimBindingRequiresItsIntent pins that a claim version
// can be bound only to a recorded intent for the same source, and only once.
func TestDeclassificationClaimBindingRequiresItsIntent(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		policy, err := auth.NewPolicy(auth.PolicyConfig{
			AuthorizationDomain: []byte("domain-one"), SourceID: []byte("source-a"),
			GrantPolicyID: []byte("policy-a"), Epoch: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		public, err := authorized.NewAccessRule(policy)
		if err != nil {
			t.Fatal(err)
		}
		secret, err := authorized.LabelRule(policy, []string{"secret"})
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		if err := store.BindDeclassificationClaim(ctx, "file:///a", 1, "missing"); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
			t.Fatalf("binding to no intent = %v", err)
		}
		for _, id := range []shoal.ID{"one", "two"} {
			if err := store.RecordDeclassification(ctx, authorized.DeclassificationRecord{
				AttemptID: id, Phase: authorized.DeclassificationIntent,
				OccurredAt: time.Date(2026, time.August, 26, 18, 0, 0, 0, time.UTC),
				SourceURI:  "file:///a",
				PriorRules: []authorized.AccessRule{secret}, Rule: public,
			}); err != nil {
				t.Fatal(err)
			}
		}
		if err := store.BindDeclassificationClaim(ctx, "file:///b", 1, "one"); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
			t.Fatalf("binding to another source's intent = %v", err)
		}
		if err := store.BindDeclassificationClaim(ctx, "file:///a", 1, "one"); err != nil {
			t.Fatal(err)
		}
		if err := store.BindDeclassificationClaim(ctx, "file:///a", 1, "one"); err != nil {
			t.Fatalf("idempotent rebinding = %v", err)
		}
		if err := store.BindDeclassificationClaim(ctx, "file:///a", 1, "two"); !shoal.IsErrorCode(err, shoal.ErrorConflict) {
			t.Fatalf("rebinding to another intent = %v", err)
		}
		intent, ok, err := store.DeclassificationClaimIntent(ctx, "file:///a", 1)
		if err != nil || !ok || intent.AttemptID != "one" {
			t.Fatalf("bound intent = %+v %v %v", intent, ok, err)
		}
		if _, ok, err := store.DeclassificationClaimIntent(ctx, "file:///a", 2); err != nil || ok {
			t.Fatalf("unbound version = %v %v", ok, err)
		}
	})
}

// TestDeclassificationOwnIntentDoesNotAuthorizeTheRegistration pins D1 of
// the fourth review. In the diverged-catalog state, an outsider who holds
// only some of the registration's labels can first leave a claim under its
// own labels (refused as absent, but the claim commits on the base write,
// #665) and then narrow that claim with a reason. Its own intent authorized
// it under the claim, not under the registration, so the pre-commit check
// must still authorize it under the registration and refuse it as absent.
func TestDeclassificationOwnIntentDoesNotAuthorizeTheRegistration(t *testing.T) {
	for _, requireReason := range []bool{false, true} {
		f := newFixture(t)
		const uri = "file:///declassify/two-step.txt"
		labelled, err := f.base.Ingest(context.Background(),
			labelledSource(uri, "labelled before the catalog", "secret&conf"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.clientA.BackfillExistingDocumentsForDevelopment(
			f.labelIngester(t, "operator", "secret", "conf"), devbackfill.NewCapability()); err != nil {
			t.Fatal(err)
		}
		restored, err := explorer.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = restored.Close() })
		client := f.declassificationClientOver(t, restored, f.store, requireReason)
		outsider := withReason(t, f.labelIngester(t, "outsider", "conf"), "r")
		if _, err := client.Ingest(outsider, labelledSource(uri, "conf", "conf")); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			t.Fatalf("reason=%v step 1 = %v, want not found", requireReason, err)
		}
		if _, err := client.Ingest(outsider, labelledSource(uri, "public", "")); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			t.Fatalf("reason=%v step 2 = %v, want not found", requireReason, err)
		}
		current, ok, err := f.store.CurrentRevision(context.Background(), labelled.Document.ID)
		if err != nil || !ok || current.RevisionID != labelled.Revision.ID {
			t.Fatalf("reason=%v: the outsider replaced the registration: %+v %v %v",
				requireReason, current, ok, err)
		}
		for _, record := range declassifications(t, f.store) {
			if record.Phase == authorized.DeclassificationPreCommit {
				t.Fatalf("reason=%v: refused narrowing has a pre-commit record: %+v", requireReason, record)
			}
		}
	}
}

// bindCountingStore counts claim-lineage bindings.
type bindCountingStore struct {
	authorized.PolicyStore
	mu    sync.Mutex
	binds int
}

func (s *bindCountingStore) BindDeclassificationClaim(
	ctx context.Context, uri string, version uint64, attemptID shoal.ID,
) error {
	s.mu.Lock()
	s.binds++
	s.mu.Unlock()
	return s.PolicyStore.BindDeclassificationClaim(ctx, uri, version, attemptID)
}

func (s *bindCountingStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.binds
}

// TestDeclassificationOrdinaryReingestWritesNoLineage pins D2 of the fourth
// review: once a narrowing has completed, later ordinary ingests of the URI
// (edits, unchanged re-ingests, added labels) write no lineage binding, so
// they cannot grow the catalog or fail on it.
func TestDeclassificationOrdinaryReingestWritesNoLineage(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, inner authorized.PolicyStore) {
		f := newFixture(t)
		store := &bindCountingStore{PolicyStore: inner}
		client := f.declassificationClient(t, store, false)
		const uri = "file:///declassify/no-lineage.txt"
		owner := f.labelIngester(t, "owner", "secret", "y")
		if _, err := client.Ingest(owner, labelledSource(uri, "secret", "secret")); err != nil {
			t.Fatal(err)
		}
		if _, err := client.Ingest(owner, labelledSource(uri, "public", "")); err != nil {
			t.Fatal(err)
		}
		if got := store.count(); got != 1 {
			t.Fatalf("binds after one narrowing = %d, want 1", got)
		}
		for _, step := range []explorer.Source{
			labelledSource(uri, "public, edited", ""),
			labelledSource(uri, "public, edited", ""),
			labelledSource(uri, "public, edited again", ""),
			labelledSource(uri, "now y", "y"),
			labelledSource(uri, "y, edited", "y"),
		} {
			if _, err := client.Ingest(owner, step); err != nil {
				t.Fatalf("ingest %q: %v", step.Content, err)
			}
		}
		if got := store.count(); got != 1 {
			t.Fatalf("binds after ordinary re-ingests = %d, want 1", got)
		}
	})
}

// TestDeclassificationReconcilingRetryKeepsLineage pins that a reconciling
// retry binds its own claim version before it records anything, so when that
// retry fails too, a further retry still completes the narrowing.
func TestDeclassificationReconcilingRetryKeepsLineage(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, inner authorized.PolicyStore) {
		f := newFixture(t)
		store := &failingDeclassificationStore{
			PolicyStore: inner, fail: map[int]bool{2: true, 3: true}}
		client := f.declassificationClient(t, store, true)
		const uri = "file:///declassify/retry-chain.txt"
		first, err := client.Ingest(f.labelIngester(t, "owner", "secret"),
			labelledSource(uri, "secret", "secret"))
		if err != nil {
			t.Fatal(err)
		}
		public := labelledSource(uri, "public", "")
		if _, err := client.Ingest(withReason(t, f.labelIngester(t, "owner", "secret"), "r"), public); err == nil {
			t.Fatal("first pre-commit failure was ignored")
		}
		// Different content, so the base applies it and the claim commits
		// at a new version; only that version's binding can carry lineage.
		if _, err := client.Ingest(f.labelIngester(t, "retrier-1"),
			labelledSource(uri, "public, retry 1", "")); err == nil {
			t.Fatal("second pre-commit failure was ignored")
		}
		second, err := client.Ingest(f.labelIngester(t, "retrier-2"),
			labelledSource(uri, "public, retry 2", ""))
		if err != nil {
			t.Fatalf("third attempt = %v, want reconciliation", err)
		}
		current, ok, err := inner.CurrentRevision(context.Background(), first.Document.ID)
		if err != nil || !ok || current.RevisionID != second.Revision.ID {
			t.Fatalf("current = %+v %v %v", current, ok, err)
		}
	})
}

// failNthBindStore fails BindDeclassificationClaim on the listed call numbers.
type failNthBindStore struct {
	authorized.PolicyStore
	mu    sync.Mutex
	calls int
	fail  map[int]bool
}

func (s *failNthBindStore) BindDeclassificationClaim(
	ctx context.Context, uri string, version uint64, attemptID shoal.ID,
) error {
	s.mu.Lock()
	s.calls++
	fail := s.fail[s.calls]
	s.mu.Unlock()
	if fail {
		return shoal.NewError(shoal.ErrorUnavailable, "bind down")
	}
	return s.PolicyStore.BindDeclassificationClaim(ctx, uri, version, attemptID)
}

// flakyDocumentsBase fails Documents once, when armed.
type flakyDocumentsBase struct {
	explorer.Client
	mu    sync.Mutex
	armed bool
}

func (b *flakyDocumentsBase) Documents(ctx context.Context) ([]explorer.DocumentSummary, error) {
	b.mu.Lock()
	armed := b.armed
	b.armed = false
	b.mu.Unlock()
	if armed {
		return nil, shoal.NewError(shoal.ErrorUnavailable, "base flake")
	}
	return b.Client.Documents(ctx)
}

// TestDeclassificationFailedInheritingRetryKeepsLineage pins R5-1 of the
// fifth review. A retry that may inherit lineage from the claim it replaced
// must not commit its new claim version before that version is bound. A
// failure after an applied base write (the carry bind itself, or a base read
// before pre-commit) rolls the claim back to the bound version, so the next
// retry still reconciles.
func TestDeclassificationFailedInheritingRetryKeepsLineage(t *testing.T) {
	for _, failure := range []string{"carry bind", "base read"} {
		t.Run(failure, func(t *testing.T) {
			withPolicyStores(t, func(t *testing.T, inner authorized.PolicyStore) {
				f := newFixture(t)
				records := &failingDeclassificationStore{
					PolicyStore: inner, fail: map[int]bool{2: true}}
				var store authorized.PolicyStore = records
				if failure == "carry bind" {
					store = &failNthBindStore{
						PolicyStore: records, fail: map[int]bool{2: true}}
				}
				base := &flakyDocumentsBase{Client: f.base}
				client := f.declassificationClientOver(t, base, store, true)
				const uri = "file:///declassify/inheriting-retry.txt"
				first, err := client.Ingest(f.labelIngester(t, "owner", "secret"),
					labelledSource(uri, "secret", "secret"))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := client.Ingest(withReason(t, f.labelIngester(t, "owner", "secret"), "r"),
					labelledSource(uri, "public", "")); err == nil {
					t.Fatal("pre-commit failure ignored")
				}
				if failure == "base read" {
					base.mu.Lock()
					base.armed = true
					base.mu.Unlock()
				}
				if _, err := client.Ingest(f.labelIngester(t, "retrier-1"),
					labelledSource(uri, "public, retry 1", "")); err == nil {
					t.Fatalf("%s failure ignored", failure)
				}
				second, err := client.Ingest(f.labelIngester(t, "retrier-2"),
					labelledSource(uri, "public, retry 2", ""))
				if err != nil {
					t.Fatalf("retry after a failed inheriting retry = %v, want reconciliation", err)
				}
				current, ok, err := inner.CurrentRevision(context.Background(), first.Document.ID)
				if err != nil || !ok || current.RevisionID != second.Revision.ID {
					t.Fatalf("current = %+v %v %v", current, ok, err)
				}
			})
		})
	}
}

// TestDeclassificationOrdinaryIngestKeepsClaimCommitOnApply pins that the
// deferred claim commit applies only to a retry that inherits lineage. An
// ordinary re-ingest of a URI with no declassification lineage still commits
// its claim as soon as the base applies it, exactly as before #630, even when
// a later step fails.
func TestDeclassificationOrdinaryIngestKeepsClaimCommitOnApply(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		f := newFixture(t)
		base := &flakyDocumentsBase{Client: f.base}
		client := f.declassificationClientOver(t, base, store, false)
		const uri = "file:///declassify/ordinary-commit.txt"
		owner := f.labelIngester(t, "owner", "x", "y")
		if _, err := client.Ingest(owner, labelledSource(uri, "x", "x")); err != nil {
			t.Fatal(err)
		}
		before, _, err := store.SourceClaim(context.Background(), uri)
		if err != nil {
			t.Fatal(err)
		}
		base.mu.Lock()
		base.armed = true
		base.mu.Unlock()
		if _, err := client.Ingest(owner, labelledSource(uri, "x and y", "x&y")); err == nil {
			t.Fatal("base flake ignored")
		}
		after, ok, err := store.SourceClaim(context.Background(), uri)
		if err != nil || !ok || after.Pending || after.Version == before.Version ||
			after.Rule.String() == before.Rule.String() {
			t.Fatalf("claim after an applied ordinary ingest failed later = %+v (before %+v) %v %v",
				after, before, ok, err)
		}
	})
}

// TestDeclassificationPendedInheritingRetryKeepsLineage pins D1 of the sixth
// review: an inheriting retry whose base write is indeterminate pends its
// claim version, and that version is already bound (before the base write),
// so any claim holder can still complete the narrowing afterwards.
func TestDeclassificationPendedInheritingRetryKeepsLineage(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, inner authorized.PolicyStore) {
		f := newFixture(t)
		store := &failingDeclassificationStore{
			PolicyStore: inner, fail: map[int]bool{2: true}}
		client := f.declassificationClient(t, store, true)
		const uri = "file:///declassify/pended-retry.txt"
		first, err := client.Ingest(f.labelIngester(t, "owner", "secret"),
			labelledSource(uri, "secret", "secret"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Ingest(withReason(t, f.labelIngester(t, "owner", "secret"), "r"),
			labelledSource(uri, "public", "")); err == nil {
			t.Fatal("pre-commit failure ignored")
		}
		indeterminate := &ingestOverrideClient{
			Client: f.base,
			ingest: func(ctx context.Context, source explorer.Source) (explorer.IngestResult, error) {
				if _, err := f.base.Ingest(ctx, source); err != nil {
					return explorer.IngestResult{}, err
				}
				return explorer.IngestResult{}, explorer.MarkIndeterminateCommit(
					shoal.NewError(shoal.ErrorUnavailable, "indeterminate"))
			},
		}
		flaky := f.declassificationClientOver(t, indeterminate, store, true)
		if _, err := flaky.Ingest(f.labelIngester(t, "retrier-1"),
			labelledSource(uri, "public, retry 1", "")); !explorer.IsIndeterminateCommit(err) {
			t.Fatalf("indeterminate retry = %v", err)
		}
		if claim, ok, err := inner.SourceClaim(context.Background(), uri); err != nil || !ok || !claim.Pending {
			t.Fatalf("claim after the indeterminate retry = %+v %v %v, want pending", claim, ok, err)
		}
		second, err := client.Ingest(f.labelIngester(t, "retrier-2"),
			labelledSource(uri, "public, retry 1", ""))
		if err != nil {
			t.Fatalf("retry after a pended inheriting retry = %v, want reconciliation", err)
		}
		current, ok, err := inner.CurrentRevision(context.Background(), first.Document.ID)
		if err != nil || !ok || current.RevisionID != second.Revision.ID {
			t.Fatalf("current = %+v %v %v", current, ok, err)
		}
	})
}

// TestDeclassificationCompletedNarrowingEndsLineage pins D2 of the sixth
// review: once a narrowing is registered its lineage is closed, so the next
// ordinary ingest is not an inheriting retry. It commits its claim on apply,
// as before #630, even when a later step fails, and writes no binding.
func TestDeclassificationCompletedNarrowingEndsLineage(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, inner authorized.PolicyStore) {
		f := newFixture(t)
		store := &bindCountingStore{PolicyStore: inner}
		base := &flakyDocumentsBase{Client: f.base}
		client := f.declassificationClientOver(t, base, store, false)
		const uri = "file:///declassify/lineage-ends.txt"
		owner := f.labelIngester(t, "owner", "secret", "x")
		if _, err := client.Ingest(owner, labelledSource(uri, "secret", "secret")); err != nil {
			t.Fatal(err)
		}
		if _, err := client.Ingest(owner, labelledSource(uri, "public", "")); err != nil {
			t.Fatal(err)
		}
		binds := store.count()
		before, _, err := inner.SourceClaim(context.Background(), uri)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok, err := inner.DeclassificationClaimIntent(context.Background(), uri, before.Version); err != nil || ok {
			t.Fatalf("lineage of a completed narrowing is still open: %v %v", ok, err)
		}
		base.mu.Lock()
		base.armed = true
		base.mu.Unlock()
		if _, err := client.Ingest(owner, labelledSource(uri, "x", "x")); err == nil {
			t.Fatal("base flake ignored")
		}
		after, ok, err := inner.SourceClaim(context.Background(), uri)
		if err != nil || !ok || after.Pending || after.Version == before.Version ||
			after.Rule.String() == before.Rule.String() {
			t.Fatalf("claim after an applied ordinary ingest failed later = %+v (before %+v) %v %v",
				after, before, ok, err)
		}
		if store.count() != binds {
			t.Fatalf("an ordinary ingest after a completed narrowing wrote a binding")
		}
	})
}

// TestDeclassificationClosedLineageSurvivesReopen pins that a closed lineage
// stays closed across a durable reopen, while an open one stays open.
func TestDeclassificationClosedLineageSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	durable, err := authorized.OpenDurablePolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t)
	client := f.declassificationClient(t, durable, false)
	const done, lagging = "file:///declassify/closed.txt", "file:///declassify/open.txt"
	owner := f.labelIngester(t, "owner", "secret")
	for _, uri := range []string{done, lagging} {
		if _, err := client.Ingest(owner, labelledSource(uri, "secret "+uri, "secret")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := client.Ingest(owner, labelledSource(done, "public", "")); err != nil {
		t.Fatal(err)
	}
	failing := &failingDeclassificationStore{
		PolicyStore: durable, fail: map[int]bool{2: true}}
	if _, err := f.declassificationClient(t, failing, false).Ingest(owner,
		labelledSource(lagging, "public", "")); err == nil {
		t.Fatal("pre-commit failure ignored")
	}
	versions := map[string]uint64{}
	for _, uri := range []string{done, lagging} {
		claim, ok, err := durable.SourceClaim(context.Background(), uri)
		if err != nil || !ok {
			t.Fatalf("claim %s: %v %v", uri, ok, err)
		}
		versions[uri] = claim.Version
	}
	if err := durable.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := authorized.OpenDurablePolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if _, ok, err := reopened.DeclassificationClaimIntent(context.Background(), done, versions[done]); err != nil || ok {
		t.Fatalf("completed narrowing's lineage reopened open: %v %v", ok, err)
	}
	if _, ok, err := reopened.DeclassificationClaimIntent(context.Background(), lagging, versions[lagging]); err != nil || !ok {
		t.Fatalf("lagging narrowing's lineage reopened closed: %v %v", ok, err)
	}
}

// failCloseOnceStore fails the first CloseDeclassificationClaim.
type failCloseOnceStore struct {
	authorized.PolicyStore
	mu     sync.Mutex
	failed bool
}

func (s *failCloseOnceStore) CloseDeclassificationClaim(
	ctx context.Context, uri string, version uint64,
) error {
	s.mu.Lock()
	first := !s.failed
	s.failed = true
	s.mu.Unlock()
	if first {
		return shoal.NewError(shoal.ErrorUnavailable, "close down")
	}
	return s.PolicyStore.CloseDeclassificationClaim(ctx, uri, version)
}

// TestDeclassificationFailedCloseHealsAndGrantsNothing pins L1 of the
// seventh review: when the lineage close fails after the narrowed revision is
// registered, the ingest reports the catalog failure and the lineage stays
// open. The next ingest on it closes it, and the open lineage cannot be used
// to narrow the (now narrow) registration further without its own intent.
func TestDeclassificationFailedCloseHealsAndGrantsNothing(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, inner authorized.PolicyStore) {
		f := newFixture(t)
		store := &failCloseOnceStore{PolicyStore: inner}
		client := f.declassificationClient(t, store, true)
		const uri = "file:///declassify/failed-close.txt"
		owner := f.labelIngester(t, "owner", "secret", "x")
		first, err := client.Ingest(owner, labelledSource(uri, "secret and x", "secret&x"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Ingest(withReason(t, owner, "r"),
			labelledSource(uri, "x only", "x")); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
			t.Fatalf("narrowing whose close failed = %v, want unavailable", err)
		}
		current, ok, err := inner.CurrentRevision(context.Background(), first.Document.ID)
		if err != nil || !ok || current.RevisionID == first.Revision.ID {
			t.Fatalf("narrowed revision not registered: %+v %v %v", current, ok, err)
		}
		claim, _, err := inner.SourceClaim(context.Background(), uri)
		if err != nil {
			t.Fatal(err)
		}
		if _, open, err := inner.DeclassificationClaimIntent(context.Background(), uri, claim.Version); err != nil || !open {
			t.Fatalf("lineage after a failed close = %v %v, want still open", open, err)
		}
		// Dropping x too narrows the claim, so it needs its own intent and a
		// reason; the open lineage does not stand in for it.
		if _, err := client.Ingest(owner, labelledSource(uri, "public", "")); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
			t.Fatalf("further narrowing on an open lineage without a reason = %v, want invalid argument", err)
		}
		// An ordinary ingest heals it.
		if _, err := client.Ingest(owner, labelledSource(uri, "x, edited", "x")); err != nil {
			t.Fatal(err)
		}
		claim, _, err = inner.SourceClaim(context.Background(), uri)
		if err != nil {
			t.Fatal(err)
		}
		if _, open, err := inner.DeclassificationClaimIntent(context.Background(), uri, claim.Version); err != nil || open {
			t.Fatalf("lineage after the healing ingest = %v %v, want closed", open, err)
		}
	})
}
