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

// A source claim commits only once its revision is registered current
// (#665). A failure after an applied base write leaves the claim pending,
// with the replaced rule, so nobody lacking that rule can finish the change.

import (
	"context"
	"testing"

	"github.com/phrocker/shoal-oss/internal/devbackfill"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func currentRegistration(
	t *testing.T, store authorized.PolicyStore, documentID shoal.ID,
) shoal.ID {
	t.Helper()
	current, ok, err := store.CurrentRevision(context.Background(), documentID)
	if err != nil || !ok {
		t.Fatalf("current registration: %v %v", ok, err)
	}
	return current.RevisionID
}

// TestFailedRegistrationLeavesThePendingClaimToBothRules: after a labelled
// document's relabel to public fails at registration, a holder of the new
// (public) rule alone cannot finish it, and the claim is not committed.
func TestFailedRegistrationLeavesThePendingClaimToBothRules(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		f := newFixture(t)
		failing := &failStore{PolicyStore: store, revisionFailures: 0}
		client := f.newClient(t, f.base, failing, f.sourceA, f.policyA, nil)
		const uri = "file:///claim-commit/relabel.txt"
		first, err := client.Ingest(f.labelIngester(t, "owner", "secret"),
			labelledSource(uri, "secret", "secret"))
		if err != nil {
			t.Fatal(err)
		}
		failing.mu.Lock()
		failing.revisionFailures = 1
		failing.mu.Unlock()
		public := labelledSource(uri, "public", "")
		if _, err := client.Ingest(f.labelIngester(t, "owner", "secret"), public); err == nil {
			t.Fatal("failed registration reported success")
		}
		claim, ok, err := store.SourceClaim(context.Background(), uri)
		if err != nil || !ok || !claim.Pending || claim.PreviousRule == nil {
			t.Fatalf("claim after a failed registration = %+v %v %v, want pending with the replaced rule",
				claim, ok, err)
		}
		if _, err := client.Ingest(f.labelIngester(t, "outsider"), public); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			t.Fatalf("outsider finishing the relabel = %v, want not found", err)
		}
		if got := currentRegistration(t, store, first.Document.ID); got != first.Revision.ID {
			t.Fatalf("outsider replaced the registration: %s", got)
		}
		// A pending claim also blocks a different relabel of the URI.
		if _, err := client.Ingest(f.labelIngester(t, "owner", "secret", "x"),
			labelledSource(uri, "x", "x")); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			t.Fatalf("different relabel of a pending claim = %v, want not found", err)
		}
		second, err := client.Ingest(f.labelIngester(t, "owner", "secret"), public)
		if err != nil {
			t.Fatalf("authorized retry: %v", err)
		}
		if got := currentRegistration(t, store, first.Document.ID); got != second.Revision.ID {
			t.Fatalf("retry did not register: %s", got)
		}
		claim, ok, err = store.SourceClaim(context.Background(), uri)
		if err != nil || !ok || claim.Pending {
			t.Fatalf("claim after the retry = %+v %v %v, want committed", claim, ok, err)
		}
	})
}

// TestFailedBaseReadAfterApplyPendsTheClaim: any failure between the applied
// base write and the registration (here a base read) pends the claim rather
// than committing it.
func TestFailedBaseReadAfterApplyPendsTheClaim(t *testing.T) {
	f := newFixture(t)
	failDocuments := false
	base := &documentsOverrideClient{Client: f.base, fail: &failDocuments}
	client := f.newClient(t, base, f.store, f.sourceA, f.policyA, nil)
	const uri = "file:///claim-commit/base-read.txt"
	if _, err := client.Ingest(f.labelIngester(t, "owner", "secret"),
		labelledSource(uri, "secret", "secret")); err != nil {
		t.Fatal(err)
	}
	failDocuments = true
	if _, err := client.Ingest(f.labelIngester(t, "owner", "secret"),
		labelledSource(uri, "public", "")); err == nil {
		t.Fatal("base read failure ignored")
	}
	failDocuments = false
	claim, ok, err := f.store.SourceClaim(context.Background(), uri)
	if err != nil || !ok || !claim.Pending || claim.PreviousRule == nil {
		t.Fatalf("claim after a failure past the base write = %+v %v %v, want pending", claim, ok, err)
	}
	if _, err := client.Ingest(f.labelIngester(t, "outsider"),
		labelledSource(uri, "public", "")); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("outsider finishing it = %v, want not found", err)
	}
}

// documentsOverrideClient fails Documents while *fail is set.
type documentsOverrideClient struct {
	explorer.Client
	fail *bool
}

func (c *documentsOverrideClient) Documents(ctx context.Context) ([]explorer.DocumentSummary, error) {
	if *c.fail {
		return nil, shoal.NewError(shoal.ErrorUnavailable, "base read failure")
	}
	return c.Client.Documents(ctx)
}

// TestLegacyIndeterminateRelabelNeedsTheRegistrationRule: an unclaimed,
// registered document whose relabel ends indeterminate leaves a pending claim
// with no previous rule (the claim-swap had none to record). Its registration
// still guards it: finishing the relabel needs that registration's rule.
func TestLegacyIndeterminateRelabelNeedsTheRegistrationRule(t *testing.T) {
	f := newFixture(t)
	const uri = "file:///claim-commit/legacy.txt"
	labelled, err := f.base.Ingest(context.Background(),
		labelledSource(uri, "labelled before the catalog", "secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.clientA.BackfillExistingDocumentsForDevelopment(
		f.labelIngester(t, "operator", "secret"), devbackfill.NewCapability()); err != nil {
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
	flaky := f.newClient(t, indeterminate, f.store, f.sourceA, f.policyA, nil)
	public := labelledSource(uri, "public", "")
	if _, err := flaky.Ingest(f.labelIngester(t, "owner", "secret"), public); !explorer.IsIndeterminateCommit(err) {
		t.Fatalf("indeterminate relabel = %v", err)
	}
	claim, ok, err := f.store.SourceClaim(context.Background(), uri)
	if err != nil || !ok || !claim.Pending || claim.PreviousRule != nil {
		t.Fatalf("claim = %+v %v %v, want pending with no previous rule", claim, ok, err)
	}
	baseRevision := func() shoal.ID {
		t.Helper()
		summaries, err := f.base.Documents(context.Background())
		if err != nil || len(summaries) != 1 {
			t.Fatalf("base documents = %+v %v", summaries, err)
		}
		return summaries[0].Revision.ID
	}
	beforeOutsider := baseRevision()
	if _, err := f.clientA.Ingest(f.labelIngester(t, "outsider"), public); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("outsider finishing a legacy relabel = %v, want not found", err)
	}
	// Refused before anything is written: other content reaches neither
	// the base nor the claim.
	if _, err := f.clientA.Ingest(f.labelIngester(t, "outsider"),
		labelledSource(uri, "the outsider's own content", "")); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("outsider writing other content = %v, want not found", err)
	}
	if got := baseRevision(); got != beforeOutsider {
		t.Fatalf("a refused outsider's content reached the base: %s", got)
	}
	if after, ok, err := f.store.SourceClaim(context.Background(), uri); err != nil || !ok ||
		after.Version != claim.Version {
		t.Fatalf("a refused outsider transitioned the claim: %+v %v %v", after, ok, err)
	}
	if got := currentRegistration(t, f.store, labelled.Document.ID); got != labelled.Revision.ID {
		t.Fatalf("outsider replaced the registration: %s", got)
	}
	if _, err := f.clientA.Ingest(f.labelIngester(t, "owner", "secret"), public); err != nil {
		t.Fatalf("authorized retry: %v", err)
	}
}

// TestDivergedRegistrationNeedsItsRule: a registration for a URI with no
// claim and no base document (the catalog and base diverged, for example a
// base restored from backup) is never replaced by a principal outside its
// rule, though the claim-time check had nothing to compare with.
func TestDivergedRegistrationNeedsItsRule(t *testing.T) {
	f := newFixture(t)
	const uri = "file:///claim-commit/diverged.txt"
	labelled, err := f.base.Ingest(context.Background(),
		labelledSource(uri, "labelled before the catalog", "secret"))
	if err != nil {
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
	client := f.newClient(t, restored, f.store, f.sourceA, f.policyA, nil)
	public := labelledSource(uri, "public", "")
	if _, err := client.Ingest(f.labelIngester(t, "outsider"), public); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("outsider replacing a diverged registration = %v, want not found", err)
	}
	// An outsider holding some other label cannot pin the URI under it
	// either: it is refused before any claim or base write.
	if _, err := client.Ingest(f.labelIngester(t, "outsider", "x"),
		labelledSource(uri, "x", "x")); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("outsider with its own label = %v, want not found", err)
	}
	if _, ok, err := f.store.SourceClaim(context.Background(), uri); err != nil || ok {
		t.Fatalf("refused outsiders left a claim: %v %v", ok, err)
	}
	if summaries, err := restored.Documents(context.Background()); err != nil || len(summaries) != 0 {
		t.Fatalf("refused outsiders wrote the base: %+v %v", summaries, err)
	}
	if got := currentRegistration(t, f.store, labelled.Document.ID); got != labelled.Revision.ID {
		t.Fatalf("outsider replaced the registration: %s", got)
	}
	if _, err := client.Ingest(f.labelIngester(t, "holder", "secret"), public); err != nil {
		t.Fatalf("label holder: %v", err)
	}
}

// TestBaseWritingAnotherDocumentIsRefused: the pre-write check authorized the
// registration of the document the Explorer derives from the URI, so a base
// that writes any other document is inconsistent and nothing is registered.
func TestBaseWritingAnotherDocumentIsRefused(t *testing.T) {
	f := newFixture(t)
	aliasing := &ingestOverrideClient{
		Client: f.base,
		ingest: func(ctx context.Context, source explorer.Source) (explorer.IngestResult, error) {
			source.URI += "-alias"
			return f.base.Ingest(ctx, source)
		},
	}
	client := f.newClient(t, aliasing, f.store, f.sourceA, f.policyA, nil)
	const uri = "file:///claim-commit/aliasing.txt"
	if _, err := client.Ingest(f.labelIngester(t, "owner"),
		labelledSource(uri, "content", "")); !shoal.IsErrorCode(err, shoal.ErrorInternal) {
		t.Fatalf("base writing another document = %v, want internal", err)
	}
	if _, ok, err := f.store.CurrentRevision(context.Background(),
		explorer.SourceDocumentID(uri+"-alias")); err != nil || ok {
		t.Fatalf("the other document was registered: %v %v", ok, err)
	}
}
