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

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Base/catalog drift (#570 review, round 2). Client.Ingest commits to the
// base before it registers the revision, so a failed registration (or a
// crash) leaves the base one revision ahead of the catalog. A retried ingest
// repairs that. The every-start migration must never turn the drift into the
// permanent untranslatable lock: it narrows each registered revision by that
// revision's own labels and reports the drift.
func TestLabelMigrationDriftNeverLocksTheDocument(t *testing.T) {
	for _, c := range []struct {
		name      string
		oldLabels string
		newLabels string
		// holders are the label sets the ingester holds for the retry.
		retryHolds []string
		// unserved makes the base refuse the registered revision, as a base
		// that lost it would: still drift, still no lock.
		unserved bool
	}{
		{"same labels", "secret", "secret", []string{"secret"}, false},
		// A relabel needs the old and the new labels.
		{"different labels", "secret", "x", []string{"secret", "x"}, false},
		{"registered revision not served", "secret", "secret", []string{"secret"}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
				f := newFixture(t)
				legacy := &legacyStore{PolicyStore: store}
				selector := f.staticSelector(t, f.sourceA, f.policyA)
				client := f.labelClient(t, f.base, legacy, selector, selector)
				const uri = "file:///label/drift.txt"
				owner := f.labelIngester(t, "owner", c.retryHolds...)
				first, err := client.Ingest(owner, labelledSource(uri, "first revision", c.oldLabels))
				if err != nil {
					t.Fatal(err)
				}
				migrate(t, client)

				// The second registration fails after the base committed.
				legacy.mu.Lock()
				legacy.failPutRevisions = 1
				legacy.mu.Unlock()
				second := labelledSource(uri, "second revision", c.newLabels)
				if _, err := client.Ingest(owner, second); err == nil {
					t.Fatal("the injected registration failure did not surface")
				}
				current, _, err := store.CurrentRevision(context.Background(), first.Document.ID)
				if err != nil || current.RevisionID != first.Revision.ID {
					t.Fatalf("the catalog moved: %+v, %v", current, err)
				}

				// The next start sees the drift.
				migrator := client
				if c.unserved {
					migrator = f.labelClient(t, &unservedRevisionBase{
						Client: f.base, revisionID: first.Revision.ID,
					}, legacy, selector, selector)
				}
				record := migrate(t, migrator)
				if len(record.Untranslatable) != 0 {
					t.Fatalf("drift was reported untranslatable: %+v", record.Untranslatable)
				}
				if len(record.Drift) != 1 || record.Drift[0].DocumentID != first.Document.ID ||
					record.Drift[0].CatalogRevisionID != first.Revision.ID ||
					record.Drift[0].SourceURI != uri {
					t.Fatalf("drift report = %+v", record.Drift)
				}
				stored, ok, err := store.LabelMigration(context.Background())
				if err != nil || !ok || len(stored.Drift) != 1 {
					t.Fatalf("stored drift = %+v, %v, %v", stored.Drift, ok, err)
				}
				// The catalog revision carries its own labels, so it is not
				// open to a source-only reader in the meantime.
				if _, err := client.Document(f.labelIngester(t, "reader"),
					first.Document.ID, first.Revision.ID); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
					t.Fatalf("source-only read during the drift = %v, want not found", err)
				}

				if c.oldLabels != c.newLabels {
					// Holding only the new labels is not enough to relabel.
					if _, err := client.Ingest(f.labelIngester(t, "new-only", c.newLabels), second); err == nil {
						t.Fatal("a principal without the old labels relabelled the document")
					}
				}
				// The retry repairs the document.
				retried, err := client.Ingest(owner, second)
				if err != nil {
					t.Fatalf("the retried ingest after the migration: %v", err)
				}
				current, _, err = store.CurrentRevision(context.Background(), first.Document.ID)
				if err != nil || current.RevisionID != retried.Revision.ID {
					t.Fatalf("the retry did not register: %+v, %v", current, err)
				}
				read := func(ctx context.Context) error {
					_, err := client.Document(ctx, retried.Document.ID, retried.Revision.ID)
					return err
				}
				if err := read(f.labelIngester(t, "holder", c.newLabels)); err != nil {
					t.Fatalf("a holder of the new labels cannot read the repaired document: %v", err)
				}
				if err := read(f.labelIngester(t, "reader")); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
					t.Fatalf("source-only read of the repaired document = %v, want not found", err)
				}
				if c.oldLabels != c.newLabels {
					if err := read(f.labelIngester(t, "old-only", c.oldLabels)); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
						t.Fatalf("a holder of only the old labels reads the relabelled document: %v", err)
					}
				}
				// And the next start finds nothing to report.
				if again := migrate(t, client); len(again.Drift) != 0 || len(again.Untranslatable) != 0 {
					t.Fatalf("report after the repair = %+v", again)
				}
			})
		})
	}

	// From a legacy catalog, with no prior migration: the base has rolled
	// back so its current revision is R1 and it no longer serves the
	// registered current R2. R2 cannot be read or labelled, but the
	// historical R1 the base still serves must be narrowed by its own label.
	t.Run("legacy catalog, registered current not served", func(t *testing.T) {
		withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
			f := newFixture(t)
			legacy := &legacyStore{PolicyStore: store}
			legacy.setStrip(true)
			selector := f.staticSelector(t, f.sourceA, f.policyA)
			client := f.labelClient(t, f.base, legacy, selector, selector)
			owner := f.labelIngester(t, "owner", "secret", "x")
			const uri = "file:///label/rolled-back.txt"
			older, err := client.Ingest(owner, labelledSource(uri, "older, labelled x", "x"))
			if err != nil {
				t.Fatal(err)
			}
			newer, err := client.Ingest(owner, labelledSource(uri, "newer, labelled secret", "secret"))
			if err != nil {
				t.Fatal(err)
			}
			legacy.setStrip(false)
			read := func(ctx context.Context) error {
				_, err := client.Document(ctx, older.Document.ID, older.Revision.ID)
				return err
			}
			if err := read(f.labelIngester(t, "before")); err != nil {
				t.Fatalf("the legacy historical revision is not open before the migration: %v", err)
			}

			rolledBack := f.labelClient(t, &rolledBackBase{
				Client: f.base, lost: newer.Revision.ID, current: older.Revision,
			}, legacy, selector, selector)
			record := migrate(t, rolledBack)
			if len(record.Untranslatable) != 0 {
				t.Fatalf("drift was reported untranslatable: %+v", record.Untranslatable)
			}
			if len(record.Drift) != 1 || record.Drift[0].CatalogRevisionID != newer.Revision.ID ||
				record.Drift[0].BaseRevisionID != older.Revision.ID {
				t.Fatalf("drift report = %+v", record.Drift)
			}
			if record.HistoricalTightened != 1 {
				t.Fatalf("the served historical revision was not narrowed: %+v", record)
			}
			if err := read(f.labelIngester(t, "reader")); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
				t.Fatalf("source-only read of the served historical revision = %v, want not found", err)
			}
			if err := read(f.labelIngester(t, "holder", "x")); err != nil {
				t.Fatalf("a holder of the revision's own label cannot read it: %v", err)
			}
		})
	})
}

// rolledBackBase is a base rolled back past one revision: it no longer
// serves lost, and reports current as the document's current revision.
type rolledBackBase struct {
	explorer.Client
	lost    shoal.ID
	current document.Revision
}

func (b *rolledBackBase) Documents(ctx context.Context) ([]explorer.DocumentSummary, error) {
	summaries, err := b.Client.Documents(ctx)
	for index := range summaries {
		if summaries[index].Revision.ID == b.lost {
			summaries[index].Revision = b.current
			summaries[index].Document.RevisionID = b.current.ID
		}
	}
	return summaries, err
}

func (b *rolledBackBase) Document(
	ctx context.Context, documentID, revisionID shoal.ID,
) (explorer.DocumentView, error) {
	if revisionID == b.lost {
		return explorer.DocumentView{}, shoal.NewError(shoal.ErrorNotFound, "revision rolled back")
	}
	return b.Client.Document(ctx, documentID, revisionID)
}

// unservedRevisionBase answers NotFound for one revision.
type unservedRevisionBase struct {
	explorer.Client
	revisionID shoal.ID
}

func (b *unservedRevisionBase) Document(
	ctx context.Context, documentID, revisionID shoal.ID,
) (explorer.DocumentView, error) {
	if revisionID == b.revisionID {
		return explorer.DocumentView{}, shoal.NewError(shoal.ErrorNotFound, "revision not served")
	}
	return b.Client.Document(ctx, documentID, revisionID)
}
