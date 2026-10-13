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

// An authorized ingest whose content resolves to an earlier, non-current
// revision is refused with ErrHistoricalRevision (#674). Before, it was
// reported as success while the document kept serving its current revision,
// so an operator undoing a relabel was told it worked.

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// revisionCountingStore counts registrations, so a test can show a refused
// ingest registered nothing.
type revisionCountingStore struct {
	authorized.PolicyStore
	mu   sync.Mutex
	puts int
}

func (s *revisionCountingStore) PutRevision(
	ctx context.Context, registration authorized.RevisionRegistration,
) error {
	s.mu.Lock()
	s.puts++
	s.mu.Unlock()
	return s.PolicyStore.PutRevision(ctx, registration)
}

func (s *revisionCountingStore) revisionPuts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.puts
}

// catalogState is what a refused ingest must leave exactly as it was.
type catalogState struct {
	claimRule         authorized.AccessRule
	claimPrevious     *authorized.AccessRule
	claimPending      bool
	claimVersion      uint64
	current           authorized.RevisionRegistration
	declassifications int
}

func readCatalogState(
	t *testing.T, store authorized.PolicyStore, uri string, documentID shoal.ID,
) catalogState {
	t.Helper()
	ctx := context.Background()
	claim, ok, err := store.SourceClaim(ctx, uri)
	if err != nil || !ok {
		t.Fatalf("source claim = %+v ok=%v err=%v", claim, ok, err)
	}
	current, ok, err := store.CurrentRevision(ctx, documentID)
	if err != nil || !ok {
		t.Fatalf("current registration = %+v ok=%v err=%v", current, ok, err)
	}
	records, err := store.Declassifications(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return catalogState{
		claimRule: claim.Rule, claimPrevious: claim.PreviousRule,
		claimPending: claim.Pending, claimVersion: claim.Version,
		current: current, declassifications: len(records),
	}
}

func requireHistoricalRefusal(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, authorized.ErrHistoricalRevision) ||
		!shoal.IsErrorCode(err, shoal.ErrorConflict) ||
		shoal.IsErrorCode(err, shoal.ErrorNotFound) ||
		explorer.IsIndeterminateCommit(err) {
		t.Fatalf("historical ingest error = %v, want ErrHistoricalRevision (conflict)", err)
	}
}

func requireBaseCurrent(t *testing.T, f *fixture, documentID, revisionID shoal.ID) {
	t.Helper()
	summaries, err := f.base.Documents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].Document.ID != documentID ||
		summaries[0].Revision.ID != revisionID {
		t.Fatalf("base current = %+v, want %s", summaries, revisionID)
	}
}

// TestReingestingADeclassifiedRevisionIsRefused is the reported scenario:
// "same" labelled secret (R1), declassified to public (R2), then "same"
// labelled secret again, which is R1's exact content. The base keeps R2
// current, so the request is refused, the document is still public at R2,
// and the claim, the registrations and the audit are exactly as before.
func TestReingestingADeclassifiedRevisionIsRefused(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		f := newFixture(t)
		counting := &revisionCountingStore{PolicyStore: store}
		client := f.newClient(t, f.base, counting, f.sourceA, f.policyA, nil)
		const uri = "file:///historical/declassified.txt"
		owner := f.labelIngester(t, "owner", "secret")
		r1, err := client.Ingest(owner, labelledSource(uri, "same", "secret"))
		if err != nil {
			t.Fatal(err)
		}
		r2, err := client.Ingest(owner, labelledSource(uri, "same", ""))
		if err != nil {
			t.Fatal(err)
		}
		if r1.Revision.ID == r2.Revision.ID {
			t.Fatal("the relabel did not create a new revision")
		}
		before := readCatalogState(t, store, uri, r2.Document.ID)
		if before.current.RevisionID != r2.Revision.ID {
			t.Fatalf("registration before = %+v, want R2", before.current)
		}
		puts := counting.revisionPuts()

		result, err := client.Ingest(owner, labelledSource(uri, "same", "secret"))
		requireHistoricalRefusal(t, err)
		if !reflect.DeepEqual(result, explorer.IngestResult{}) {
			t.Fatalf("a refused ingest returned a result: %+v", result)
		}

		requireBaseCurrent(t, f, r2.Document.ID, r2.Revision.ID)
		if after := readCatalogState(t, store, uri, r2.Document.ID); !reflect.DeepEqual(after, before) {
			t.Fatalf("a refused ingest changed the catalog:\nbefore %+v\nafter  %+v", before, after)
		}
		if got := counting.revisionPuts(); got != puts {
			t.Fatalf("a refused ingest registered %d revisions", got-puts)
		}
		// The document is still public: a reader without the label reads R2.
		reader := f.labelIngester(t, "reader")
		view, err := client.Document(reader, r2.Document.ID, "")
		if err != nil || view.Revision.ID != r2.Revision.ID {
			t.Fatalf("unlabelled reader after the refusal = %+v, %v, want R2", view.Revision, err)
		}
		if _, err := client.Document(reader, r1.Document.ID, r1.Revision.ID); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			t.Fatalf("unlabelled reader of secret R1 = %v, want not found", err)
		}

		// The documented workaround still works: content that differs by a
		// byte is a new revision, relabelled under the requested rule.
		r3, err := client.Ingest(owner, labelledSource(uri, "same.", "secret"))
		if err != nil || r3.Disposition != explorer.IngestApplied {
			t.Fatalf("relabel with changed content = %+v, %v", r3, err)
		}
		if _, err := client.Document(reader, r3.Document.ID, ""); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			t.Fatalf("unlabelled reader after the relabel = %v, want not found", err)
		}
	})
}

// TestReingestingAHistoricalRevisionCannotDeclassify is the reverse
// direction: R1 public, R2 secret, then R1's public content again. That
// request is a narrowing, so its intent is audited before the base is
// written (an attempt record, by design). But nothing else is: no pre-commit
// record, no registration, and the document stays secret.
func TestReingestingAHistoricalRevisionCannotDeclassify(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		f := newFixture(t)
		counting := &revisionCountingStore{PolicyStore: store}
		client := f.newClient(t, f.base, counting, f.sourceA, f.policyA, nil)
		const uri = "file:///historical/reclassified.txt"
		owner := f.labelIngester(t, "owner", "secret")
		if _, err := client.Ingest(owner, labelledSource(uri, "same", "")); err != nil {
			t.Fatal(err)
		}
		r2, err := client.Ingest(owner, labelledSource(uri, "same", "secret"))
		if err != nil {
			t.Fatal(err)
		}
		before := readCatalogState(t, store, uri, r2.Document.ID)
		records, err := store.Declassifications(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		puts := counting.revisionPuts()

		_, err = client.Ingest(owner, labelledSource(uri, "same", ""))
		requireHistoricalRefusal(t, err)

		requireBaseCurrent(t, f, r2.Document.ID, r2.Revision.ID)
		after := readCatalogState(t, store, uri, r2.Document.ID)
		if got := counting.revisionPuts(); got != puts {
			t.Fatalf("a refused ingest registered %d revisions", got-puts)
		}
		audit, err := store.Declassifications(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(audit) != len(records)+1 ||
			audit[len(records)].Phase != authorized.DeclassificationIntent {
			t.Fatalf("refused narrowing audit = %+v, want exactly one intent", audit[len(records):])
		}
		after.declassifications = before.declassifications
		if !reflect.DeepEqual(after, before) {
			t.Fatalf("a refused ingest changed the catalog:\nbefore %+v\nafter  %+v", before, after)
		}
		if _, err := client.Document(f.labelIngester(t, "reader"), r2.Document.ID, ""); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			t.Fatalf("unlabelled reader after a refused declassification = %v, want not found", err)
		}
	})
}

// dispositionBase rewrites the base's ingest disposition, standing in for a
// base that predates IngestHistorical or reports inconsistent data.
type dispositionBase struct {
	explorer.Client
	rewrite func(explorer.IngestResult) explorer.IngestDisposition
}

func (b *dispositionBase) Ingest(
	ctx context.Context, source explorer.Source,
) (explorer.IngestResult, error) {
	result, err := b.Client.Ingest(ctx, source)
	if err == nil {
		result.Disposition = b.rewrite(result)
	}
	return result, err
}

// TestAuthorizedIngestChecksTheDispositionAgainstTheCurrentRevision pins
// that the authorized client does not take the base's word for it: content
// that is not current is refused even when reported unchanged (a base that
// predates IngestHistorical), and a historical report for the current
// revision, or an unknown disposition, is an inconsistent base.
func TestAuthorizedIngestChecksTheDispositionAgainstTheCurrentRevision(t *testing.T) {
	f := newFixture(t)
	const uri = "file:///historical/disposition.txt"
	first := explorer.Source{URI: uri, MediaType: explorer.MediaTypeText, Content: "first"}
	second := explorer.Source{URI: uri, MediaType: explorer.MediaTypeText, Content: "second"}
	if _, err := f.clientA.Ingest(f.admin(t), first); err != nil {
		t.Fatal(err)
	}
	current, err := f.clientA.Ingest(f.admin(t), second)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		source  explorer.Source
		rewrite explorer.IngestDisposition
		check   func(error) bool
	}{
		{"unchanged but not current", first, explorer.IngestUnchanged,
			func(err error) bool { return errors.Is(err, authorized.ErrHistoricalRevision) }},
		{"historical but current", second, explorer.IngestHistorical,
			func(err error) bool {
				return shoal.IsErrorCode(err, shoal.ErrorInternal) &&
					!errors.Is(err, authorized.ErrHistoricalRevision)
			}},
		{"unknown disposition", second, "restored",
			func(err error) bool { return shoal.IsErrorCode(err, shoal.ErrorInternal) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := readCatalogState(t, f.store, uri, current.Document.ID)
			base := &dispositionBase{Client: f.base,
				rewrite: func(explorer.IngestResult) explorer.IngestDisposition { return test.rewrite }}
			client := f.newClient(t, base, f.store, f.sourceA, f.policyA, nil)
			if _, err := client.Ingest(f.admin(t), test.source); !test.check(err) {
				t.Fatalf("ingest = %v", err)
			}
			requireBaseCurrent(t, f, current.Document.ID, current.Revision.ID)
			if after := readCatalogState(t, f.store, uri, current.Document.ID); !reflect.DeepEqual(after, before) {
				t.Fatalf("a refused ingest changed the catalog:\nbefore %+v\nafter  %+v", before, after)
			}
		})
	}
	// The honest base still reports the current revision as unchanged.
	unchanged, err := f.clientA.Ingest(f.admin(t), second)
	if err != nil || unchanged.Disposition != explorer.IngestUnchanged ||
		unchanged.Revision.ID != current.Revision.ID {
		t.Fatalf("current retry = %+v, %v, want unchanged", unchanged, err)
	}
}
