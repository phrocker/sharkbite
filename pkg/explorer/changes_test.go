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

package explorer_test

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func changeSource(n int) explorer.Source {
	return explorer.Source{
		URI:       fmt.Sprintf("file:///change-%02d.md", n),
		MediaType: explorer.MediaTypeMarkdown,
		Content: fmt.Sprintf(
			"# Change %02d\n\nSynthetic body for publication %02d.\n", n, n),
	}
}

func mustIngest(t *testing.T, corpus *explorer.Explorer, n int) explorer.IngestResult {
	t.Helper()
	result, err := corpus.Ingest(context.Background(), changeSource(n))
	if err != nil {
		t.Fatalf("ingest %d: %v", n, err)
	}
	if result.Disposition != explorer.IngestApplied {
		t.Fatalf("ingest %d disposition = %q, want applied", n, result.Disposition)
	}
	return result
}

func requireConflict(t *testing.T, err error, wantSubstring string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected conflict error containing %q, got nil", wantSubstring)
	}
	if !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("error code = %v, want conflict (err=%v)", err, err)
	}
	if !strings.Contains(err.Error(), wantSubstring) {
		t.Fatalf("error %q does not contain %q", err.Error(), wantSubstring)
	}
}

func TestChangesOrderInPublicationSequence(t *testing.T) {
	ctx := context.Background()
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()

	ids := make([]shoal.ID, 0, 3)
	for i := 1; i <= 3; i++ {
		ids = append(ids, mustIngest(t, corpus, i).Document.ID)
	}

	feed, err := corpus.Changes(ctx, explorer.ChangeRequest{Since: 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(feed.Changes) != 3 {
		t.Fatalf("changes = %d, want 3", len(feed.Changes))
	}
	for i, change := range feed.Changes {
		if change.Kind != explorer.ChangeKindDocumentPublished {
			t.Fatalf("change %d kind = %q", i, change.Kind)
		}
		if change.Sequence != uint64(i+1) {
			t.Fatalf("change %d sequence = %d, want %d", i, change.Sequence, i+1)
		}
		if change.Document.ID != ids[i] {
			t.Fatalf("change %d document = %s, want %s", i, change.Document.ID, ids[i])
		}
	}
	if feed.More {
		t.Fatalf("More = true, want false for a fully drained feed")
	}
	if feed.Cursor != 3 {
		t.Fatalf("Cursor = %d, want 3", feed.Cursor)
	}
	if feed.Head != 3 {
		t.Fatalf("Head = %d, want 3", feed.Head)
	}
	if feed.Floor != 1 {
		t.Fatalf("Floor = %d, want 1", feed.Floor)
	}
	if feed.Incarnation == "" {
		t.Fatalf("Incarnation is empty")
	}
}

func TestChangesResumeDeliversCommitDuringGap(t *testing.T) {
	ctx := context.Background()
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()

	mustIngest(t, corpus, 1)

	first, err := corpus.Changes(ctx, explorer.ChangeRequest{Since: 0, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Changes) != 1 || first.Cursor != 1 {
		t.Fatalf("first page = %+v", first)
	}

	// A change committed while the client is "disconnected" must be delivered
	// exactly once when it resumes from its cursor.
	gapDoc := mustIngest(t, corpus, 2).Document.ID

	resumed, err := corpus.Changes(ctx, explorer.ChangeRequest{
		Since:               first.Cursor,
		ExpectedIncarnation: first.Incarnation,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed.Changes) != 1 {
		t.Fatalf("resumed changes = %d, want 1", len(resumed.Changes))
	}
	if resumed.Changes[0].Sequence != 2 || resumed.Changes[0].Document.ID != gapDoc {
		t.Fatalf("gap change = %+v, want sequence 2 doc %s", resumed.Changes[0], gapDoc)
	}
}

func TestChangesTruncatedPageResumesExactlyAtFirstUndelivered(t *testing.T) {
	ctx := context.Background()
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()

	for i := 1; i <= 5; i++ {
		mustIngest(t, corpus, i)
	}

	var delivered []uint64
	since := uint64(0)
	incarnation := ""
	pages := 0
	for {
		feed, err := corpus.Changes(ctx, explorer.ChangeRequest{
			Since:               since,
			Limit:               2,
			ExpectedIncarnation: incarnation,
		})
		if err != nil {
			t.Fatal(err)
		}
		incarnation = feed.Incarnation
		for _, change := range feed.Changes {
			delivered = append(delivered, change.Sequence)
		}
		pages++
		if !feed.More {
			if len(feed.Changes) > 2 {
				t.Fatalf("page returned %d changes, exceeds limit 2", len(feed.Changes))
			}
			break
		}
		if len(feed.Changes) != 2 {
			t.Fatalf("non-final page returned %d changes, want 2", len(feed.Changes))
		}
		since = feed.Cursor
		if pages > 10 {
			t.Fatal("paging did not terminate")
		}
	}
	want := []uint64{1, 2, 3, 4, 5}
	if fmt.Sprint(delivered) != fmt.Sprint(want) {
		t.Fatalf("delivered = %v, want %v (no gap, no overlap)", delivered, want)
	}
}

func TestChangesCursorAheadOfCorpusResynchronises(t *testing.T) {
	ctx := context.Background()
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()

	mustIngest(t, corpus, 1)

	_, err = corpus.Changes(ctx, explorer.ChangeRequest{Since: 99})
	requireConflict(t, err, "ahead of the corpus")
}

func TestChangesForeignIncarnationResynchronises(t *testing.T) {
	ctx := context.Background()
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()

	mustIngest(t, corpus, 1)

	_, err = corpus.Changes(ctx, explorer.ChangeRequest{
		Since:               0,
		ExpectedIncarnation: "0000000000000000000000000000000000000000000000000000000000000000",
	})
	requireConflict(t, err, "another corpus")
}

func TestChangesCursorSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	corpus, err := explorer.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	mustIngest(t, corpus, 1)

	before, err := corpus.Changes(ctx, explorer.ChangeRequest{Since: 0})
	if err != nil {
		t.Fatal(err)
	}
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := explorer.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()

	// A cursor minted before restart must remain valid and its incarnation
	// stable, and a change committed after restart must be delivered.
	afterDoc := mustIngest(t, reopened, 2).Document.ID

	resumed, err := reopened.Changes(ctx, explorer.ChangeRequest{
		Since:               before.Cursor,
		ExpectedIncarnation: before.Incarnation,
	})
	if err != nil {
		t.Fatalf("resume across restart: %v", err)
	}
	if len(resumed.Changes) != 1 || resumed.Changes[0].Document.ID != afterDoc {
		t.Fatalf("post-restart change = %+v, want doc %s", resumed.Changes, afterDoc)
	}
	if resumed.Incarnation != before.Incarnation {
		t.Fatalf("incarnation changed across restart: %q -> %q",
			before.Incarnation, resumed.Incarnation)
	}
}

func TestChangesConcurrentWritersPageWithoutGapOrDuplicate(t *testing.T) {
	ctx := context.Background()
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()

	const writers = 8
	var wg sync.WaitGroup
	wg.Add(writers)
	for w := 0; w < writers; w++ {
		go func(base int) {
			defer wg.Done()
			for i := 0; i < 4; i++ {
				if _, err := corpus.Ingest(ctx, changeSource(base*4+i+1)); err != nil {
					t.Errorf("concurrent ingest: %v", err)
					return
				}
			}
		}(w)
	}

	// Page the feed while writers are still committing.
	seen := make(map[uint64]bool)
	var order []uint64
	since := uint64(0)
	incarnation := ""
	for {
		feed, err := corpus.Changes(ctx, explorer.ChangeRequest{
			Since:               since,
			Limit:               3,
			ExpectedIncarnation: incarnation,
		})
		if err != nil {
			t.Fatal(err)
		}
		incarnation = feed.Incarnation
		for _, change := range feed.Changes {
			if seen[change.Sequence] {
				t.Fatalf("duplicate delivery of sequence %d", change.Sequence)
			}
			seen[change.Sequence] = true
			order = append(order, change.Sequence)
		}
		since = feed.Cursor
		if !feed.More {
			// Writers may still be in flight; loop until both drained and done.
			done := make(chan struct{})
			go func() { wg.Wait(); close(done) }()
			select {
			case <-done:
				final, err := corpus.Changes(ctx, explorer.ChangeRequest{
					Since:               since,
					ExpectedIncarnation: incarnation,
				})
				if err != nil {
					t.Fatal(err)
				}
				for _, change := range final.Changes {
					if seen[change.Sequence] {
						t.Fatalf("duplicate delivery of sequence %d", change.Sequence)
					}
					seen[change.Sequence] = true
					order = append(order, change.Sequence)
				}
			default:
			}
			if len(seen) == writers*4 {
				break
			}
		}
	}

	if len(order) != writers*4 {
		t.Fatalf("delivered %d changes, want %d", len(order), writers*4)
	}
	if !sort.SliceIsSorted(order, func(i, j int) bool { return order[i] < order[j] }) {
		t.Fatalf("delivered order is not monotonic: %v", order)
	}
	for i := 1; i <= writers*4; i++ {
		if !seen[uint64(i)] {
			t.Fatalf("sequence %d never delivered (gap)", i)
		}
	}
}
