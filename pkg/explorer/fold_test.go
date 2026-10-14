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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// foldedCorpus ingests one restricted and one open source, records two
// sessions over them, and returns the corpus plus the span IDs.
func foldedCorpus(t *testing.T, dir string) (
	*explorer.Explorer, []shoal.ID, []shoal.ID,
) {
	t.Helper()
	corpus, err := explorer.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	restricted := ingestVisible(
		t, corpus, "file:///incident.md", restrictedMarkdown, "secret&incident")
	open := ingestVisible(
		t, corpus, "file:///runbook.md", publicMarkdown, "ops")
	return corpus, restricted, open
}

func foldOf(
	t *testing.T, corpus *explorer.Explorer, sessions ...shoal.ID,
) explorer.FoldResult {
	t.Helper()
	result, err := corpus.FoldInteractions(
		context.Background(), explorer.FoldRequest{
			SessionIDs:    sessions,
			SummaryDigest: interaction.Digest("an out-of-band summary"),
		})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// TestFoldVisibilityIsConjunctionOfEverythingFolded pins requirement 2:
// summarizing must never widen visibility. A fold over a restricted session
// and an open session requires the labels of both.
func TestFoldVisibilityIsConjunctionOfEverythingFolded(t *testing.T) {
	ctx := context.Background()
	corpus, restricted, open := foldedCorpus(t, t.TempDir())
	defer corpus.Close()

	// The restricted session is shown the restricted span but cites only the
	// open one, so its own visibility already covers what it was shown.
	recordedSession(
		t, corpus, "interaction.session_restricted",
		[]shoal.ID{restricted[0], open[0]}, []shoal.ID{open[0]})
	recordedSession(
		t, corpus, "interaction.session_open",
		[]shoal.ID{open[0]}, []shoal.ID{open[0]})

	sessions, err := corpus.Interactions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[shoal.ID]explorer.InteractionSummary, len(sessions))
	for _, summary := range sessions {
		byID[summary.SessionID] = summary
	}

	result := foldOf(t, corpus, "interaction.session_restricted", "interaction.session_open")
	if result.Visibility != "incident&ops&secret" {
		t.Fatalf("fold visibility = %q, want %q",
			result.Visibility, "incident&ops&secret")
	}
	// The fold is never readable by anyone who could not read a part.
	for _, sessionID := range []shoal.ID{"interaction.session_restricted", "interaction.session_open"} {
		for _, label := range strings.Split(byID[sessionID].Visibility, "&") {
			if label == "" {
				continue
			}
			if !strings.Contains(result.Visibility, label) {
				t.Fatalf("fold dropped label %q required by %s",
					label, sessionID)
			}
		}
	}
}

// TestFoldIsExcludedFromDefaultRetrieval pins requirement 1: a fold is derived
// content and is never returned as source evidence, neither by retrieval nor
// by expanding a source span's neighborhood.
func TestFoldIsExcludedFromDefaultRetrieval(t *testing.T) {
	ctx := context.Background()
	corpus, restricted, open := foldedCorpus(t, t.TempDir())
	defer corpus.Close()

	recordedSession(
		t, corpus, "interaction.session_one",
		[]shoal.ID{restricted[0], open[0]}, []shoal.ID{open[0]})
	fold := foldOf(t, corpus, "interaction.session_one")

	result, err := corpus.Retrieve(ctx, retrieval.Request{
		Text:  "retry budget exhausted",
		TopK:  50,
		Modes: []retrieval.Mode{retrieval.ModeLexical},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Results) == 0 {
		t.Fatal("retrieval returned no results at all")
	}
	for _, evidence := range result.Results {
		if evidence.ID == fold.FoldID {
			t.Fatal("retrieval returned a fold as source evidence")
		}
		if strings.HasPrefix(string(evidence.ID), interaction.KindPrefix) {
			t.Fatalf("retrieval returned reserved node %q", evidence.ID)
		}
	}

	// Expansion from a touched span must not discover the fold either.
	neighborhood, err := corpus.Neighborhood(ctx, explorer.NeighborhoodRequest{
		NodeIDs: []shoal.ID{open[0]}, Depth: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range neighborhood.Nodes {
		if node.ID == fold.FoldID {
			t.Fatal("neighborhood expansion discovered a fold")
		}
	}
	bounded, err := corpus.BoundedNeighborhood(
		ctx, explorer.BoundedNeighborhoodRequest{
			NodeIDs:  []shoal.ID{open[0]},
			Depth:    16,
			Fanout:   1024,
			MaxNodes: 1024,
		})
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range bounded.Neighborhood.Nodes {
		if node.ID == fold.FoldID {
			t.Fatal("bounded expansion discovered a fold")
		}
	}
}

// TestFoldCannotBeCitedAsSourceEvidence pins requirement 1's second half: a
// later inference cannot cite a fold as though it were a source document, and
// the existing resolver guard still holds for the new node kind.
func TestFoldCannotBeCitedAsSourceEvidence(t *testing.T) {
	ctx := context.Background()
	corpus, restricted, open := foldedCorpus(t, t.TempDir())
	defer corpus.Close()

	recordedSession(
		t, corpus, "interaction.session_one",
		[]shoal.ID{restricted[0], open[0]}, []shoal.ID{open[0]})
	fold := foldOf(t, corpus, "interaction.session_one")

	later := interaction.Session{
		ID:           "interaction.session_citing-a-fold",
		RecordedAt:   time.Unix(1700000100, 0).UTC(),
		SeedNodeIDs:  []shoal.ID{open[0]},
		CitedNodeIDs: []shoal.ID{fold.FoldID},
	}
	err := corpus.RecordInteraction(ctx, later)
	if err == nil {
		t.Fatal("expected citing a fold as source evidence to be refused")
	}
	if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("unexpected error kind: %v", err)
	}

	shown := interaction.Session{
		ID:          "interaction.session_shown-a-fold",
		RecordedAt:  time.Unix(1700000200, 0).UTC(),
		SeedNodeIDs: []shoal.ID{fold.FoldID},
	}
	if err := corpus.RecordInteraction(ctx, shown); err == nil {
		t.Fatal("expected retrieving a fold as source evidence to be refused")
	}

	// Folding a fold is refused for the same reason: a fold is not a session.
	if _, err := corpus.FoldInteractions(ctx, explorer.FoldRequest{
		SessionIDs: []shoal.ID{fold.FoldID},
	}); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("expected folding a fold to be refused, got %v", err)
	}
}

// TestRehydrateFoldPreservesRetrievedAndCitedDistinction pins requirement 3:
// folding and then rehydrating loses no provenance, and in particular does not
// collapse what the model was shown into what it cited.
func TestRehydrateFoldPreservesRetrievedAndCitedDistinction(t *testing.T) {
	ctx := context.Background()
	corpus, restricted, open := foldedCorpus(t, t.TempDir())
	defer corpus.Close()

	// The session is shown the restricted span but cites only the open one.
	recordedSession(
		t, corpus, "interaction.session_wide",
		[]shoal.ID{restricted[0], open[0]}, []shoal.ID{open[0]})
	recordedSession(
		t, corpus, "interaction.session_narrow",
		[]shoal.ID{open[0]}, []shoal.ID{open[0]})

	fold := foldOf(t, corpus, "interaction.session_wide", "interaction.session_narrow")
	rehydrated, err := corpus.RehydrateFold(ctx, fold.FoldID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rehydrated.Members) != 2 {
		t.Fatalf("rehydrated %d members, want 2", len(rehydrated.Members))
	}
	byID := make(map[shoal.ID]interaction.FoldMember, len(rehydrated.Members))
	for _, member := range rehydrated.Members {
		byID[member.SessionID] = member
	}

	wide, ok := byID["interaction.session_wide"]
	if !ok {
		t.Fatal("rehydration lost interaction.session_wide")
	}
	if !containsNodeID(wide.RetrievedNodeIDs, restricted[0]) {
		t.Fatal("rehydration lost a retrieved-only node")
	}
	if containsNodeID(wide.CitedNodeIDs, restricted[0]) {
		t.Fatal("rehydration promoted a retrieved-only node to cited")
	}
	if !containsNodeID(wide.CitedNodeIDs, open[0]) {
		t.Fatal("rehydration lost a cited node")
	}
	narrow := byID["interaction.session_narrow"]
	if containsNodeID(narrow.RetrievedNodeIDs, restricted[0]) {
		t.Fatal("rehydration leaked another session's retrieved node")
	}

	// The fold's own edges keep the distinction too, so the graph alone
	// answers the question without consulting the stored record.
	subgraph, err := corpus.FoldSubgraph(ctx, fold.FoldID)
	if err != nil {
		t.Fatal(err)
	}
	touched := interaction.TouchedNodes(subgraph.Nodes, subgraph.Edges)
	if !containsNodeID(touched.RetrievedNodeIDs, restricted[0]) {
		t.Fatal("fold subgraph lost a retrieved node")
	}
	if containsNodeID(touched.CitedNodeIDs, restricted[0]) {
		t.Fatal("fold subgraph promoted a retrieved-only node to cited")
	}
}

// TestFoldIsContentAddressedAndIdempotent pins requirement 4 through the
// corpus: refolding the same sessions returns the same vertex rather than
// minting a second one, and a different member set is a different vertex.
func TestFoldIsContentAddressedAndIdempotent(t *testing.T) {
	ctx := context.Background()
	corpus, restricted, open := foldedCorpus(t, t.TempDir())
	defer corpus.Close()

	recordedSession(
		t, corpus, "interaction.session_a", []shoal.ID{restricted[0]}, []shoal.ID{restricted[0]})
	recordedSession(
		t, corpus, "interaction.session_b", []shoal.ID{open[0]}, []shoal.ID{open[0]})

	first := foldOf(t, corpus, "interaction.session_a", "interaction.session_b")
	if !first.Created {
		t.Fatal("the first fold was not created")
	}
	// Reversed order, same input.
	second := foldOf(t, corpus, "interaction.session_b", "interaction.session_a")
	if second.FoldID != first.FoldID {
		t.Fatalf("fold is not content addressed: %q != %q",
			second.FoldID, first.FoldID)
	}
	if second.Created {
		t.Fatal("refolding the same input created a second vertex")
	}
	third := foldOf(t, corpus, "interaction.session_a")
	if third.FoldID == first.FoldID {
		t.Fatal("a different member set produced the same fold")
	}

	folds, err := corpus.Folds(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(folds) != 2 {
		t.Fatalf("stored %d folds, want 2", len(folds))
	}
}

// TestFoldSurvivesReopen pins that folds are durable and reload with their
// derived visibility and members intact.
func TestFoldSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	corpus, restricted, open := foldedCorpus(t, dir)
	recordedSession(
		t, corpus, "interaction.session_one",
		[]shoal.ID{restricted[0], open[0]}, []shoal.ID{open[0]})
	fold := foldOf(t, corpus, "interaction.session_one")
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := explorer.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	folds, err := reopened.Folds(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(folds) != 1 || folds[0].FoldID != fold.FoldID {
		t.Fatalf("reopened corpus lost the fold: %+v", folds)
	}
	if folds[0].Visibility != fold.Visibility {
		t.Fatalf("reopened fold visibility = %q, want %q",
			folds[0].Visibility, fold.Visibility)
	}
	rehydrated, err := reopened.RehydrateFold(ctx, fold.FoldID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rehydrated.Members) != 1 ||
		rehydrated.Members[0].SessionID != "interaction.session_one" {
		t.Fatalf("reopened fold lost its members: %+v", rehydrated.Members)
	}
}

// TestFoldRetentionIsExplicitAndTombstoned pins that decision 5 extends to
// folds, and that a folded session cannot be deleted out from under the fold
// that would otherwise keep a rehydratable copy of it.
func TestFoldRetentionIsExplicitAndTombstoned(t *testing.T) {
	ctx := context.Background()
	corpus, restricted, open := foldedCorpus(t, t.TempDir())
	defer corpus.Close()

	recordedSession(
		t, corpus, "interaction.session_one",
		[]shoal.ID{restricted[0], open[0]}, []shoal.ID{open[0]})
	fold := foldOf(t, corpus, "interaction.session_one")

	if _, err := corpus.DeleteInteraction(ctx, "interaction.session_one"); !shoal.IsErrorCode(
		err, shoal.ErrorConflict,
	) {
		t.Fatalf("expected deleting a folded session to be refused, got %v", err)
	}

	tombstone, err := corpus.DeleteFold(ctx, fold.FoldID)
	if err != nil {
		t.Fatal(err)
	}
	if tombstone.SessionID != fold.FoldID {
		t.Fatalf("tombstone names %q, want %q", tombstone.SessionID, fold.FoldID)
	}
	if interaction.Expression(tombstone.Visibility) != fold.Visibility {
		t.Fatalf("tombstone visibility = %q, want %q",
			interaction.Expression(tombstone.Visibility), fold.Visibility)
	}
	if _, err := corpus.RehydrateFold(ctx, fold.FoldID); !shoal.IsErrorCode(
		err, shoal.ErrorConflict,
	) {
		t.Fatalf("expected rehydrating a deleted fold to be refused, got %v", err)
	}
	subgraph, err := corpus.FoldSubgraph(ctx, fold.FoldID)
	if err != nil {
		t.Fatal(err)
	}
	if len(subgraph.Nodes) != 1 ||
		subgraph.Nodes[0].Kind != interaction.KindTombstone {
		t.Fatalf("deleted fold did not leave a tombstone: %+v", subgraph.Nodes)
	}
	// With the fold gone the session may be deleted.
	if _, err := corpus.DeleteInteraction(ctx, "interaction.session_one"); err != nil {
		t.Fatal(err)
	}
}

// TestFoldRefusesDeletedSession pins that a tombstoned session cannot be
// resurrected into a summary.
func TestFoldRefusesDeletedSession(t *testing.T) {
	ctx := context.Background()
	corpus, restricted, open := foldedCorpus(t, t.TempDir())
	defer corpus.Close()

	recordedSession(
		t, corpus, "interaction.session_one",
		[]shoal.ID{restricted[0], open[0]}, []shoal.ID{open[0]})
	if _, err := corpus.DeleteInteraction(ctx, "interaction.session_one"); err != nil {
		t.Fatal(err)
	}
	if _, err := corpus.FoldInteractions(ctx, explorer.FoldRequest{
		SessionIDs: []shoal.ID{"interaction.session_one"},
	}); !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("expected folding a deleted session to be refused, got %v", err)
	}
	if _, err := corpus.FoldInteractions(ctx, explorer.FoldRequest{
		SessionIDs: []shoal.ID{"interaction.session_missing"},
	}); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("expected folding an unknown session to be refused, got %v", err)
	}
}

// TestFoldRefusesNonDigestSummary pins the redaction discipline at the corpus
// boundary: summary text can never reach a node payload.
func TestFoldRefusesNonDigestSummary(t *testing.T) {
	ctx := context.Background()
	corpus, restricted, open := foldedCorpus(t, t.TempDir())
	defer corpus.Close()

	recordedSession(
		t, corpus, "interaction.session_one",
		[]shoal.ID{restricted[0], open[0]}, []shoal.ID{open[0]})
	if _, err := corpus.FoldInteractions(ctx, explorer.FoldRequest{
		SessionIDs:    []shoal.ID{"interaction.session_one"},
		SummaryDigest: "the retry budget was exhausted during the outage",
	}); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("expected free-text summary to be refused, got %v", err)
	}
}

// TestSessionIDCannotCollideWithFoldID pins that folds and interaction
// sessions do not share an identity. Both are written into the same corpus
// node namespace, so a collision would drop one of them at the next graph
// rebuild and leave two records claiming one node. Session IDs are
// caller-supplied, and fold IDs are content-addressed and therefore
// predictable, so the collision is reachable rather than theoretical.
func TestSessionIDCannotCollideWithFoldID(t *testing.T) {
	ctx := context.Background()
	corpus, restricted, open := foldedCorpus(t, t.TempDir())
	defer corpus.Close()

	recordedSession(
		t, corpus, "interaction.session_a", []shoal.ID{open[0]}, []shoal.ID{open[0]})
	recordedSession(
		t, corpus, "interaction.session_b",
		[]shoal.ID{restricted[0], open[0]}, []shoal.ID{open[0]})
	fold := foldOf(t, corpus, "interaction.session_a", "interaction.session_b")

	template := recordedSession(
		t, corpus, "interaction.session_c", []shoal.ID{open[0]}, []shoal.ID{open[0]})
	colliding := template
	colliding.ID = fold.FoldID

	err := corpus.RecordInteraction(ctx, colliding)
	if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("recording a session under a reserved fold ID = %v", err)
	}

	// The fold must still resolve to the fold, not to the session.
	if _, err := corpus.RehydrateFold(ctx, fold.FoldID); err != nil {
		t.Fatalf("fold no longer rehydrates after collision attempt: %v", err)
	}
}

// TestInteractionsTouchingWalksAcrossSessions pins requirement 5: provenance
// can be walked from a source node to every session and fold that touched it,
// with the retrieved/cited distinction intact.
func TestInteractionsTouchingWalksAcrossSessions(t *testing.T) {
	ctx := context.Background()
	corpus, restricted, open := foldedCorpus(t, t.TempDir())
	defer corpus.Close()

	recordedSession(
		t, corpus, "interaction.session_cites",
		[]shoal.ID{restricted[0], open[0]}, []shoal.ID{open[0]})
	recordedSession(
		t, corpus, "interaction.session_shows",
		[]shoal.ID{restricted[0]}, []shoal.ID{})
	fold := foldOf(t, corpus, "interaction.session_cites")

	touches, err := corpus.InteractionsTouching(ctx, restricted[0])
	if err != nil {
		t.Fatal(err)
	}
	found := make(map[shoal.ID]explorer.InteractionTouch, len(touches))
	for _, touch := range touches {
		found[touch.InteractionID] = touch
	}
	if len(found) != 3 {
		t.Fatalf("walked %d interactions, want 3: %+v", len(found), touches)
	}
	if !found["interaction.session_cites"].Retrieved || found["interaction.session_cites"].Cited {
		t.Fatalf("interaction.session_cites touch = %+v", found["interaction.session_cites"])
	}
	if found[fold.FoldID].Kind != interaction.KindFold {
		t.Fatalf("fold touch = %+v", found[fold.FoldID])
	}

	openTouches, err := corpus.InteractionsTouching(ctx, open[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, touch := range openTouches {
		if touch.InteractionID == "interaction.session_cites" && !touch.Cited {
			t.Fatal("a cited node was not reported as cited")
		}
		if touch.InteractionID == "interaction.session_shows" {
			t.Fatal("a session that never saw the node was reported")
		}
	}

	// Cross-session overlap.
	overlaps, err := corpus.RelatedInteractions(ctx, "interaction.session_shows")
	if err != nil {
		t.Fatal(err)
	}
	var sawCites bool
	for _, overlap := range overlaps {
		if overlap.InteractionID != "interaction.session_cites" {
			continue
		}
		sawCites = true
		if !containsNodeID(overlap.SharedNodeIDs, restricted[0]) {
			t.Fatalf("overlap missed the shared node: %+v", overlap)
		}
		if containsNodeID(overlap.SharedNodeIDs, open[0]) {
			t.Fatalf("overlap claimed an unshared node: %+v", overlap)
		}
	}
	if !sawCites {
		t.Fatalf("cross-session walk missed interaction.session_cites: %+v", overlaps)
	}
}

// TestInteractionsTouchingRefusesInteractionNode pins that provenance is
// walked from source evidence outward, never from a derived node.
func TestInteractionsTouchingRefusesInteractionNode(t *testing.T) {
	ctx := context.Background()
	corpus, restricted, open := foldedCorpus(t, t.TempDir())
	defer corpus.Close()

	recordedSession(
		t, corpus, "interaction.session_one",
		[]shoal.ID{restricted[0], open[0]}, []shoal.ID{open[0]})
	fold := foldOf(t, corpus, "interaction.session_one")

	if _, err := corpus.InteractionsTouching(ctx, fold.FoldID); !shoal.IsErrorCode(
		err, shoal.ErrorInvalidArgument,
	) {
		t.Fatalf("expected walking from a fold node to be refused, got %v", err)
	}
	if _, err := corpus.RelatedInteractions(ctx, "interaction.session_missing"); !shoal.IsErrorCode(
		err, shoal.ErrorNotFound,
	) {
		t.Fatalf("expected an unknown interaction to be refused, got %v", err)
	}
}

// TestFoldIsSafeUnderConcurrentReads pins that folding while provenance is
// being read shares no mutable state. Capture and traversal sit on the
// concurrent serving path.
func TestFoldIsSafeUnderConcurrentReads(t *testing.T) {
	ctx := context.Background()
	corpus, restricted, open := foldedCorpus(t, t.TempDir())
	defer corpus.Close()

	recordedSession(
		t, corpus, "interaction.session_one",
		[]shoal.ID{restricted[0], open[0]}, []shoal.ID{open[0]})
	recordedSession(
		t, corpus, "interaction.session_two",
		[]shoal.ID{open[0]}, []shoal.ID{open[0]})

	var group sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if _, err := corpus.FoldInteractions(
				ctx, explorer.FoldRequest{
					SessionIDs: []shoal.ID{"interaction.session_one", "interaction.session_two"},
				}); err != nil {
				t.Error(err)
			}
		}()
		group.Add(1)
		go func() {
			defer group.Done()
			if _, err := corpus.InteractionsTouching(ctx, open[0]); err != nil {
				t.Error(err)
			}
			if _, err := corpus.RelatedInteractions(ctx, "interaction.session_one"); err != nil {
				t.Error(err)
			}
			if _, err := corpus.Folds(ctx); err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()

	folds, err := corpus.Folds(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(folds) != 1 {
		t.Fatalf("concurrent folding minted %d vertices, want 1", len(folds))
	}
}

func containsNodeID(ids []shoal.ID, target shoal.ID) bool {
	for _, id := range ids {
		if id == target {
			return true
		}
	}
	return false
}
