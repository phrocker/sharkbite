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
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// These tests pin issue #273: content visibility must be resolved at retrieval
// time, not frozen at write time. Every assertion is made through a public read
// path (Interactions, InteractionSubgraph, Folds, FoldSubgraph, RehydrateFold,
// InteractionsTouching, RelatedInteractions), never by calling an unexported
// helper, so a fail-open regression is observable exactly where a caller would
// observe it.

// tighten re-ingests the same URI and content with a stricter visibility
// expression. The span node IDs are derived from content and document identity,
// not from visibility, so re-ingesting identical content keeps the same span
// IDs but raises the label the current revision requires. This is the
// tightening / reclassification the defect failed to honour on read.
func tighten(
	t *testing.T, corpus *explorer.Explorer, uri, content, visibility string,
) {
	t.Helper()
	spans := ingestVisible(t, corpus, uri, content, visibility)
	if len(spans) == 0 {
		t.Fatalf("re-ingest of %q produced no spans", uri)
	}
}

func containsSession(
	summaries []explorer.InteractionSummary, id shoal.ID,
) *explorer.InteractionSummary {
	for i := range summaries {
		if summaries[i].SessionID == id {
			return &summaries[i]
		}
	}
	return nil
}

func containsFold(
	summaries []explorer.FoldSummary, id shoal.ID,
) *explorer.FoldSummary {
	for i := range summaries {
		if summaries[i].FoldID == id {
			return &summaries[i]
		}
	}
	return nil
}

// TestRevokedSourceHidesSessionAtReadTime is the core proof: a session recorded
// over a source that is later tightened is no longer readable through any
// session read path, even though the stored record is untouched.
func TestRevokedSourceHidesSessionAtReadTime(t *testing.T) {
	ctx := context.Background()
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()

	open := ingestVisible(t, corpus, "file:///runbook.md", publicMarkdown, "ops")
	recordedSession(t, corpus, "interaction.session_ops", open[:1], open[:1])

	// Before tightening the session is fully readable and labelled "ops".
	summaries, err := corpus.Interactions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := containsSession(summaries, "interaction.session_ops"); got == nil {
		t.Fatal("interaction.session_ops was not listed before tightening")
	} else if got.Visibility != "ops" {
		t.Fatalf("interaction.session_ops visibility = %q, want ops", got.Visibility)
	}
	if _, err := corpus.InteractionSubgraph(ctx, "interaction.session_ops"); err != nil {
		t.Fatalf("InteractionSubgraph before tightening = %v", err)
	}
	touching, err := corpus.InteractionsTouching(ctx, open[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(touching) != 1 || touching[0].InteractionID != "interaction.session_ops" {
		t.Fatalf("InteractionsTouching before tightening = %+v", touching)
	}
	if _, err := corpus.RelatedInteractions(ctx, "interaction.session_ops"); err != nil {
		t.Fatalf("RelatedInteractions before tightening = %v", err)
	}

	// Access to the source is revoked by adding a label the recorded session's
	// visibility does not carry.
	tighten(t, corpus, "file:///runbook.md", publicMarkdown, "ops&secret")

	// Every session read path now fails closed.
	summaries, err = corpus.Interactions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := containsSession(summaries, "interaction.session_ops"); got != nil {
		t.Fatalf("interaction.session_ops still listed after tightening: %+v", got)
	}
	if _, err := corpus.InteractionSubgraph(ctx, "interaction.session_ops"); !shoal.IsErrorCode(
		err, shoal.ErrorUnavailable,
	) {
		t.Fatalf("InteractionSubgraph after tightening = %v, want Unavailable", err)
	}
	touching, err = corpus.InteractionsTouching(ctx, open[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(touching) != 0 {
		t.Fatalf("InteractionsTouching after tightening = %+v, want empty", touching)
	}
	if _, err := corpus.RelatedInteractions(ctx, "interaction.session_ops"); !shoal.IsErrorCode(
		err, shoal.ErrorUnavailable,
	) {
		t.Fatalf("RelatedInteractions after tightening = %v, want Unavailable", err)
	}
}

// TestReclassifiedSourceDeniesPreviouslyAuthorizedReader states the reader
// consequence explicitly: a reader whose grant covered the old label "ops" was
// authorized to read the session; after the source is reclassified to a
// strictly higher label, the read path refuses, so that reader is denied.
func TestReclassifiedSourceDeniesPreviouslyAuthorizedReader(t *testing.T) {
	ctx := context.Background()
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()

	open := ingestVisible(t, corpus, "file:///runbook.md", publicMarkdown, "ops")
	recordedSession(t, corpus, "interaction.session_ops", open[:1], open[:1])

	sub, err := corpus.InteractionSubgraph(ctx, "interaction.session_ops")
	if err != nil {
		t.Fatalf("read before reclassification = %v", err)
	}
	if len(sub.Nodes) == 0 {
		t.Fatal("read before reclassification returned no nodes")
	}

	// Reclassify to a higher label the previously-authorized "ops" reader does
	// not hold.
	tighten(t, corpus, "file:///runbook.md", publicMarkdown, "ops&classified")

	if _, err := corpus.InteractionSubgraph(ctx, "interaction.session_ops"); !shoal.IsErrorCode(
		err, shoal.ErrorUnavailable,
	) {
		t.Fatalf(
			"read after reclassification = %v, want Unavailable (reader denied)",
			err,
		)
	}
}

// TestReclassifiedSourceHidesFold proves the fold read paths honour tightening
// as well: a fold derived over a source is withheld once that source is
// reclassified, through Folds, FoldSubgraph and RehydrateFold.
func TestReclassifiedSourceHidesFold(t *testing.T) {
	ctx := context.Background()
	corpus, restricted, open := foldedCorpus(t, t.TempDir())
	defer corpus.Close()

	recordedSession(
		t, corpus, "interaction.session_restricted",
		[]shoal.ID{restricted[0], open[0]}, []shoal.ID{open[0]})
	recordedSession(
		t, corpus, "interaction.session_open", []shoal.ID{open[0]}, []shoal.ID{open[0]})

	fold := foldOf(t, corpus, "interaction.session_restricted", "interaction.session_open")

	// The fold is readable and carries the conjunction before tightening.
	folds, err := corpus.Folds(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := containsFold(folds, fold.FoldID); got == nil {
		t.Fatal("fold was not listed before tightening")
	}
	if _, err := corpus.FoldSubgraph(ctx, fold.FoldID); err != nil {
		t.Fatalf("FoldSubgraph before tightening = %v", err)
	}
	if _, err := corpus.RehydrateFold(ctx, fold.FoldID); err != nil {
		t.Fatalf("RehydrateFold before tightening = %v", err)
	}

	// Reclassify the open source that the fold folded over.
	tighten(t, corpus, "file:///runbook.md", publicMarkdown, "ops&classified")

	folds, err = corpus.Folds(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := containsFold(folds, fold.FoldID); got != nil {
		t.Fatalf("fold still listed after tightening: %+v", got)
	}
	if _, err := corpus.FoldSubgraph(ctx, fold.FoldID); !shoal.IsErrorCode(
		err, shoal.ErrorUnavailable,
	) {
		t.Fatalf("FoldSubgraph after tightening = %v, want Unavailable", err)
	}
	if _, err := corpus.RehydrateFold(ctx, fold.FoldID); !shoal.IsErrorCode(
		err, shoal.ErrorUnavailable,
	) {
		t.Fatalf("RehydrateFold after tightening = %v, want Unavailable", err)
	}
}

// TestTighteningInvalidatesAPriorPermissiveRead deliberately constructs the
// stale-cache case the defect depends on: it performs a successful, permissive
// read first (the moment a naive cache would memoize the answer), then tightens,
// then reads again. Because visibility is re-derived from the live graph on
// every read, the second read refuses; and a cold reopen with no in-memory
// state refuses identically, proving nothing permissive was cached across the
// tightening.
func TestTighteningInvalidatesAPriorPermissiveRead(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	corpus, err := explorer.Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	open := ingestVisible(t, corpus, "file:///runbook.md", publicMarkdown, "ops")
	recordedSession(t, corpus, "interaction.session_ops", open[:1], open[:1])

	// A permissive read happens first; any cache would be warm and permissive.
	if _, err := corpus.InteractionSubgraph(ctx, "interaction.session_ops"); err != nil {
		t.Fatalf("permissive read before tightening = %v", err)
	}

	tighten(t, corpus, "file:///runbook.md", publicMarkdown, "ops&secret")

	if _, err := corpus.InteractionSubgraph(ctx, "interaction.session_ops"); !shoal.IsErrorCode(
		err, shoal.ErrorUnavailable,
	) {
		t.Fatalf("warm read after tightening = %v, want Unavailable", err)
	}
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}

	// A cold reopen has no warm in-memory decision at all and must also refuse.
	reopened, err := explorer.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.InteractionSubgraph(ctx, "interaction.session_ops"); !shoal.IsErrorCode(
		err, shoal.ErrorUnavailable,
	) {
		t.Fatalf("cold read after tightening = %v, want Unavailable", err)
	}
}

// TestTombstonedRecordKeepsOriginalVisibilityAfterTightening pins the issue's
// tombstone constraint: a deleted record is never re-derived, so it keeps the
// visibility it was published with even after its former source is tightened. A
// deletion must never leak the fact that a once-restricted record existed by
// relabelling or withholding its tombstone.
func TestTombstonedRecordKeepsOriginalVisibilityAfterTightening(t *testing.T) {
	ctx := context.Background()
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()

	open := ingestVisible(t, corpus, "file:///runbook.md", publicMarkdown, "ops")
	recordedSession(t, corpus, "interaction.session_deleted", open[:1], open[:1])

	if _, err := corpus.DeleteInteraction(ctx, "interaction.session_deleted"); err != nil {
		t.Fatal(err)
	}

	// Tightening the former source must not change what the tombstone reports.
	tighten(t, corpus, "file:///runbook.md", publicMarkdown, "ops&secret")

	summaries, err := corpus.Interactions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := containsSession(summaries, "interaction.session_deleted")
	if got == nil {
		t.Fatal("tombstoned record dropped from listing after tightening")
	}
	if !got.Deleted {
		t.Fatalf("tombstoned record not marked deleted: %+v", got)
	}
	if got.Visibility != "ops" {
		t.Fatalf("tombstone visibility = %q, want ops (original)", got.Visibility)
	}
	sub, err := corpus.InteractionSubgraph(ctx, "interaction.session_deleted")
	if err != nil {
		t.Fatalf("InteractionSubgraph for tombstone = %v", err)
	}
	if len(sub.Nodes) != 1 {
		t.Fatalf("tombstone subgraph nodes = %+v", sub.Nodes)
	}
}

// TestDeclassifiedSourceStillServesRecord pins the Issue #273 follow-up: a
// source that is later *loosened* (a label dropped, i.e. declassification) must
// not make a previously derived record unreadable. The stored record keeps its
// stricter label, which still covers everything the now-weaker source requires,
// so the read paths must keep serving it. Comparing by string equality here
// would refuse and silently lose the record; comparing by authorization
// coverage (current label set is a subset of the stored set) serves it.
func TestDeclassifiedSourceStillServesRecord(t *testing.T) {
	ctx := context.Background()
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()

	open := ingestVisible(
		t, corpus, "file:///incident.md", restrictedMarkdown, "incident&secret")
	recordedSession(t, corpus, "interaction.session_secret", open[:1], open[:1])

	// Baseline: the session is readable and carries the conjunction.
	summaries, err := corpus.Interactions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := containsSession(summaries, "interaction.session_secret"); got == nil {
		t.Fatal("interaction.session_secret was not listed before declassification")
	} else if got.Visibility != "incident&secret" {
		t.Fatalf("interaction.session_secret visibility = %q, want incident&secret", got.Visibility)
	}

	// Declassify: re-ingest identical content dropping the "secret" label. The
	// current revision now only requires "incident", which the stored
	// "incident&secret" already covers.
	ingestVisible(t, corpus, "file:///incident.md", restrictedMarkdown, "incident")

	summaries, err = corpus.Interactions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := containsSession(summaries, "interaction.session_secret")
	if got == nil {
		t.Fatal("interaction.session_secret lost from listing after declassification (data loss)")
	}
	// The record is still labelled with its stricter stored visibility; it is
	// served under the label it was derived at, never downgraded.
	if got.Visibility != "incident&secret" {
		t.Fatalf(
			"interaction.session_secret visibility = %q after declassification, want incident&secret",
			got.Visibility,
		)
	}
	if _, err := corpus.InteractionSubgraph(ctx, "interaction.session_secret"); err != nil {
		t.Fatalf("InteractionSubgraph after declassification = %v, want served", err)
	}
	touching, err := corpus.InteractionsTouching(ctx, open[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(touching) != 1 || touching[0].InteractionID != "interaction.session_secret" {
		t.Fatalf("InteractionsTouching after declassification = %+v", touching)
	}
}

// TestMixedRelabelWithNewLabelStillRefuses guards the sharp edge of the Issue
// #273 follow-up: coverage is not "any weaker-or-different label serves". If a
// relabel drops one label but introduces another the stored record does not
// carry, the new label is a tightening on that axis and the record must be
// withheld. This proves the coverage check is a genuine subset test, not a
// cardinality or "looks looser" shortcut that would reopen the fail-open.
func TestMixedRelabelWithNewLabelStillRefuses(t *testing.T) {
	ctx := context.Background()
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()

	open := ingestVisible(
		t, corpus, "file:///incident.md", restrictedMarkdown, "incident&secret")
	recordedSession(t, corpus, "interaction.session_secret", open[:1], open[:1])

	if _, err := corpus.InteractionSubgraph(ctx, "interaction.session_secret"); err != nil {
		t.Fatalf("read before relabel = %v", err)
	}

	// Drop "secret" but add "classified": stored {incident,secret} does not
	// cover current {incident,classified} because "classified" is new.
	tighten(t, corpus, "file:///incident.md", restrictedMarkdown, "incident&classified")

	summaries, err := corpus.Interactions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := containsSession(summaries, "interaction.session_secret"); got != nil {
		t.Fatalf("interaction.session_secret still listed after mixed relabel: %+v", got)
	}
	if _, err := corpus.InteractionSubgraph(ctx, "interaction.session_secret"); !shoal.IsErrorCode(
		err, shoal.ErrorUnavailable,
	) {
		t.Fatalf(
			"InteractionSubgraph after mixed relabel = %v, want Unavailable", err)
	}
}
