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

package explorer

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type stagedCancellationContext struct {
	context.Context
	mu          sync.Mutex
	calls       int
	cancelAfter int
}

func (c *stagedCancellationContext) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.calls >= c.cancelAfter {
		return context.Canceled
	}
	return nil
}

func TestFoldRevalidatesRetainedSourceEdgeVisibility(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	corpus, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = corpus.Close() })
	receipt, err := corpus.Ingest(ctx, Source{
		URI: "file:///fold-edge-visibility.txt", MediaType: MediaTypeText,
		Content:  "fold edge visibility",
		Metadata: shoal.Metadata{interaction.PropertyVisibility: "node-label"},
	})
	if err != nil {
		t.Fatal(err)
	}
	view, err := corpus.Document(ctx, receipt.Document.ID, receipt.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	edge := graph.Edge{
		ID: "fold-source-edge", From: receipt.Document.ID,
		To: view.Root.Spans[0].ID, Type: "supports", Weight: 1,
		Properties: shoal.Metadata{interaction.PropertyVisibility: "edge-label"},
	}
	if err := corpus.Connect(ctx, edge); err != nil {
		t.Fatal(err)
	}
	pin, err := corpus.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	corpus.mu.RLock()
	path := graph.Path{
		Nodes: []graph.Node{
			cloneNode(corpus.graphNodes[edge.From]),
			cloneNode(corpus.graphNodes[edge.To]),
		},
		Edges: []graph.Edge{cloneEdge(corpus.graphEdges[edge.ID])},
	}
	corpus.mu.RUnlock()
	anchor, err := inference.NewGraphAnchor(path)
	if err != nil {
		t.Fatal(err)
	}
	session := interaction.Session{
		ID:         interaction.DerivedID("session", "fold-edge-visibility"),
		RecordedAt: pin.AsOf.Add(time.Second),
		Operation:  interaction.OperationRetrieval,
		SnapshotID: shoal.ID(pin.ID), SnapshotAsOf: pin.AsOf,
		AuthorizationFingerprint: "auth-sha256:fold-edge",
		AuthorizationExpiresAt:   pin.AsOf.Add(time.Hour),
		SeedNodeIDs:              []shoal.ID{edge.From, edge.To},
		SeedEvidence: []interaction.EvidenceReference{{
			AnchorID: anchor.ID(), Kind: interaction.EvidenceGraph,
			NodeIDs: []shoal.ID{edge.From, edge.To},
			EdgeIDs: []shoal.ID{edge.ID},
		}},
	}
	if err := corpus.RecordInteraction(ctx, session); err != nil {
		t.Fatal(err)
	}
	fold, err := corpus.FoldInteractions(ctx, FoldRequest{
		SessionIDs: []shoal.ID{session.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	corpus.mu.Lock()
	edgeProvenanceComplete :=
		corpus.interactions[session.ID].EdgeProvenanceComplete
	corpus.interactions[session.ID].EdgeProvenanceComplete = false
	corpus.mu.Unlock()
	if _, err := corpus.FoldInteractions(ctx, FoldRequest{
		SessionIDs:    []shoal.ID{session.ID},
		SummaryDigest: interaction.Digest("unprovable legacy member"),
	}); !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("fold with unavailable typed member provenance = %v", err)
	}
	corpus.mu.Lock()
	corpus.interactions[session.ID].EdgeProvenanceComplete =
		edgeProvenanceComplete
	corpus.mu.Unlock()
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}
	corpus, err = Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	corpus.mu.Lock()
	edgeProvenanceComplete =
		corpus.interactions[session.ID].EdgeProvenanceComplete
	corpus.interactions[session.ID].EdgeProvenanceComplete = false
	corpus.mu.Unlock()
	interimRetry, err := corpus.FoldInteractions(ctx, FoldRequest{
		SessionIDs: []shoal.ID{session.ID},
	})
	if err != nil || interimRetry.Created || interimRetry.FoldID != fold.FoldID ||
		!interimRetry.FoldedAt.Equal(fold.FoldedAt) {
		t.Fatalf("edge-inclusive interim fold retry = %+v, %v", interimRetry, err)
	}
	corpus.mu.Lock()
	corpus.interactions[session.ID].EdgeProvenanceComplete =
		edgeProvenanceComplete
	corpus.mu.Unlock()
	corpus.mu.Lock()
	corpus.folds[fold.FoldID].Members[0].TouchedEdgeIDs = nil
	corpus.mu.Unlock()
	retried, err := corpus.FoldInteractions(ctx, FoldRequest{
		SessionIDs: []shoal.ID{session.ID},
	})
	if err != nil || retried.Created || retried.FoldID != fold.FoldID ||
		!retried.FoldedAt.Equal(fold.FoldedAt) {
		t.Fatalf("legacy fold retry = %+v, %v", retried, err)
	}
	corpus.mu.Lock()
	currentRecord := corpus.folds[fold.FoldID]
	legacyRecord := *currentRecord
	legacyRecord.Members = cloneFoldMembers(currentRecord.Members)
	legacyFold := interaction.Fold{
		Members: legacyRecord.Members, SummaryDigest: legacyRecord.SummaryDigest,
		FoldedAt: legacyRecord.FoldedAt,
	}
	legacyID, legacyErr := legacyFold.ID()
	if legacyErr != nil {
		corpus.mu.Unlock()
		t.Fatal(legacyErr)
	}
	delete(corpus.folds, fold.FoldID)
	legacyRecord.FoldID = legacyID
	corpus.folds[legacyID] = &legacyRecord
	edgeProvenanceComplete =
		corpus.interactions[session.ID].EdgeProvenanceComplete
	corpus.interactions[session.ID].EdgeProvenanceComplete = false
	corpus.mu.Unlock()
	legacyRetry, err := corpus.FoldInteractions(ctx, FoldRequest{
		SessionIDs: []shoal.ID{session.ID},
	})
	if err != nil || legacyRetry.Created || legacyRetry.FoldID != legacyID {
		t.Fatalf("pre-edge legacy fold retry = %+v, %v", legacyRetry, err)
	}
	corpus.mu.Lock()
	corpus.interactions[session.ID].EdgeProvenanceComplete =
		edgeProvenanceComplete
	delete(corpus.folds, legacyID)
	corpus.folds[fold.FoldID] = currentRecord
	corpus.mu.Unlock()
	rehydrated, err := corpus.RehydrateFold(ctx, fold.FoldID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rehydrated.Members) != 1 ||
		!reflect.DeepEqual(rehydrated.Members[0].TouchedEdgeIDs, []shoal.ID{edge.ID}) {
		t.Fatalf("rehydrated fold edges = %+v", rehydrated.Members)
	}
	corpus.mu.Lock()
	corrupted := *currentRecord
	corrupted.FoldID = legacyID
	corrupted.Members = cloneFoldMembers(currentRecord.Members)
	corrupted.Members[0].TouchedEdgeIDs = []shoal.ID{edge.ID}
	delete(corpus.folds, fold.FoldID)
	corpus.folds[legacyID] = &corrupted
	corpus.mu.Unlock()
	if _, err := corpus.RehydrateFold(
		ctx, legacyID); !shoal.IsErrorCode(err, shoal.ErrorInternal) {
		t.Fatalf("legacy ID accepted stored typed edge provenance: %v", err)
	}
	corpus.mu.Lock()
	delete(corpus.folds, legacyID)
	corpus.folds[fold.FoldID] = currentRecord
	corpus.mu.Unlock()
	corpus.mu.Lock()
	corpus.interactions[session.ID].Deleted = true
	corpus.mu.Unlock()
	if touches, err := corpus.InteractionsTouching(
		ctx, edge.From); err != nil || len(touches) != 0 {
		t.Fatalf("deleted member remained traversable: %+v, %v", touches, err)
	}
	if _, err := corpus.RelatedInteractions(
		ctx, fold.FoldID); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("deleted-member fold traversal = %v", err)
	}
	corpus.mu.Lock()
	corpus.interactions[session.ID].Deleted = false
	corpus.mu.Unlock()
	corpus.mu.Lock()
	changed := cloneEdge(corpus.graphEdges[edge.ID])
	changed.Properties[interaction.PropertyVisibility] = "edge-tightened"
	corpus.graphEdges[edge.ID] = changed
	corpus.mu.Unlock()
	if _, err := corpus.RehydrateFold(
		ctx, fold.FoldID); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("rehydrated fold after edge tightening = %v", err)
	}
	if folds, err := corpus.Folds(ctx); err != nil || len(folds) != 0 {
		t.Fatalf("fold list after edge tightening = %+v, %v", folds, err)
	}
	if _, err := corpus.FoldSubgraph(
		ctx, fold.FoldID); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("fold subgraph after edge tightening = %v", err)
	}
	if _, err := corpus.RelatedInteractions(
		ctx, fold.FoldID); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("fold traversal after edge tightening = %v", err)
	}
}

func TestIncompleteInteractionEdgeProvenanceIsNotReadable(t *testing.T) {
	ctx := context.Background()
	corpus, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	receipt, err := corpus.Ingest(ctx, Source{
		URI:       "file:///incomplete-edge-provenance.txt",
		MediaType: MediaTypeText,
		Content:   "incomplete provenance",
	})
	if err != nil {
		t.Fatal(err)
	}
	view, err := corpus.Document(ctx, receipt.Document.ID, receipt.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	session := interaction.Session{
		ID:          interaction.DerivedID("session", "incomplete-edge-provenance"),
		RecordedAt:  time.Unix(1700000000, 0).UTC(),
		Operation:   interaction.OperationRetrieval,
		SeedNodeIDs: []shoal.ID{view.Root.Spans[0].ID},
	}
	if err := corpus.RecordInteraction(ctx, session); err != nil {
		t.Fatal(err)
	}
	corpus.mu.Lock()
	if !corpus.interactions[session.ID].EdgeProvenanceComplete {
		corpus.mu.Unlock()
		t.Fatal("new interaction did not record complete edge provenance")
	}
	corpus.interactions[session.ID].EdgeProvenanceComplete = false
	corpus.mu.Unlock()

	if summaries, err := corpus.Interactions(ctx); err != nil || len(summaries) != 0 {
		t.Fatalf("incomplete interaction summaries = %+v, %v", summaries, err)
	}
	if records, err := corpus.InteractionRecords(ctx); err != nil || len(records) != 0 {
		t.Fatalf("incomplete interaction records = %+v, %v", records, err)
	}
	if _, err := corpus.InteractionRecord(
		ctx, session.ID); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("incomplete interaction record error = %v", err)
	}
	if _, err := corpus.Interaction(
		ctx, session.ID); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("incomplete interaction error = %v", err)
	}
	if _, err := corpus.InteractionSubgraph(
		ctx, session.ID); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("incomplete interaction subgraph error = %v", err)
	}
	if touching, err := corpus.InteractionsTouching(
		ctx, view.Root.Spans[0].ID); err != nil || len(touching) != 0 {
		t.Fatalf("incomplete interaction traversal = %+v, %v", touching, err)
	}
	if _, err := corpus.RelatedInteractions(
		ctx, session.ID); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("incomplete direct traversal error = %v", err)
	}
}

func TestInteractionWriteResolvesCommittedIndeterminateOutcome(t *testing.T) {
	ctx := context.Background()
	corpus, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	receipt, err := corpus.Ingest(ctx, Source{
		URI:       "file:///source.txt",
		MediaType: MediaTypeText,
		Content:   "durable source",
	})
	if err != nil {
		t.Fatal(err)
	}
	view, err := corpus.Document(ctx, receipt.Document.ID, receipt.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	spanID := view.Root.Spans[0].ID
	session := interaction.Session{
		ID:                       "interaction.session_indeterminate-committed",
		RecordedAt:               time.Unix(1700000000, 0).UTC(),
		SnapshotID:               "snapshot",
		SnapshotAsOf:             time.Unix(1699999990, 0).UTC(),
		AuthorizationFingerprint: "auth-sha256:test",
		AuthorizationExpiresAt:   time.Unix(1700003600, 0).UTC(),
		SeedNodeIDs:              []shoal.ID{spanID},
	}
	write := corpus.writeRecord
	corpus.interactionRecordWriter = func(
		row []byte, kind byte, value any,
	) error {
		if err := write(row, kind, value); err != nil {
			return err
		}
		return MarkIndeterminateCommit(errors.New("post-commit failure"))
	}
	if err := corpus.RecordInteraction(ctx, session); err != nil {
		t.Fatalf("committed indeterminate write was not reconciled: %v", err)
	}
	if _, err := corpus.Interaction(ctx, session.ID); err != nil {
		t.Fatalf("reconciled interaction is not hydrated: %v", err)
	}
}

func TestInteractionWriteConjoinsRequiredOutputVisibility(t *testing.T) {
	corpus, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	receipt, err := corpus.Ingest(context.Background(), Source{
		URI: "file:///source.txt", MediaType: MediaTypeText,
		Content: "durable source",
	})
	if err != nil {
		t.Fatal(err)
	}
	view, err := corpus.Document(
		context.Background(), receipt.Document.ID, receipt.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	session := interaction.Session{
		ID:                       "interaction.session_workspace-output",
		RecordedAt:               time.Unix(1700000000, 0).UTC(),
		SnapshotID:               "snapshot",
		SnapshotAsOf:             time.Unix(1699999990, 0).UTC(),
		AuthorizationFingerprint: "auth-sha256:test",
		AuthorizationExpiresAt:   time.Unix(1700003600, 0).UTC(),
		SeedNodeIDs:              []shoal.ID{view.Root.Spans[0].ID},
	}
	ctx, err := interaction.WithRequiredVisibility(
		context.Background(), []string{"policy:a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := corpus.RecordInteraction(ctx, session); err != nil {
		t.Fatal(err)
	}
	record := corpus.interactions[session.ID]
	if record == nil || record.Visibility != "policy:a" {
		t.Fatalf("recorded visibility = %#v", record)
	}
	stricter, err := interaction.WithRequiredVisibility(
		context.Background(), []string{"policy:a", "policy:b"})
	if err != nil {
		t.Fatal(err)
	}
	if err := corpus.RecordInteraction(
		stricter, session,
	); !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("stricter retry error = %v, want conflict", err)
	}
}

func TestInteractionWritePreservesUnresolvedIndeterminateOutcome(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	corpus, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = corpus.Close() })
	receipt, err := corpus.Ingest(ctx, Source{
		URI:       "file:///source.txt",
		MediaType: MediaTypeText,
		Content:   "durable source",
	})
	if err != nil {
		t.Fatal(err)
	}
	view, err := corpus.Document(ctx, receipt.Document.ID, receipt.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	session := interaction.Session{
		ID:                       "interaction.session_indeterminate-absent",
		RecordedAt:               time.Unix(1700000000, 0).UTC(),
		SnapshotID:               "snapshot",
		SnapshotAsOf:             time.Unix(1699999990, 0).UTC(),
		AuthorizationFingerprint: "auth-sha256:test",
		AuthorizationExpiresAt:   time.Unix(1700003600, 0).UTC(),
		SeedNodeIDs:              []shoal.ID{view.Root.Spans[0].ID},
	}
	corpus.interactionRecordWriter = func(
		[]byte, byte, any,
	) error {
		return MarkIndeterminateCommit(errors.New("unknown commit"))
	}
	err = corpus.RecordInteraction(ctx, session)
	if !IsIndeterminateCommit(err) {
		t.Fatalf("unresolved write error = %v", err)
	}
	if _, err := corpus.Interaction(ctx, session.ID); !IsIndeterminateCommit(err) ||
		!shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("indeterminate corpus did not fail closed: %v", err)
	}
	if _, err := corpus.Ingest(ctx, Source{
		URI:       "file:///blocked-after-indeterminate.txt",
		MediaType: MediaTypeText,
		Content:   "must not publish",
	}); !IsIndeterminateCommit(err) ||
		!shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("indeterminate corpus accepted a later mutation: %v", err)
	}
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}
	corpus, err = Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := corpus.Interaction(ctx, session.ID); !shoal.IsErrorCode(
		err, shoal.ErrorNotFound,
	) {
		t.Fatalf("absent interaction appeared after recovery: %v", err)
	}
	if _, err := corpus.Ingest(ctx, Source{
		URI:       "file:///allowed-after-recovery.txt",
		MediaType: MediaTypeText,
		Content:   "recovered",
	}); err != nil {
		t.Fatalf("reopened corpus stayed poisoned: %v", err)
	}
}

func TestInteractionResultMarksCancellationAfterDurableCommit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	corpus, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	defer corpus.Close()
	write := corpus.writeRecord
	corpus.interactionRecordWriter = func(
		row []byte, kind byte, value any,
	) error {
		if err := write(row, kind, value); err != nil {
			return err
		}
		cancel()
		return nil
	}
	session := interaction.Session{
		ID:         interaction.DerivedID("session", "cancel-after-commit"),
		RecordedAt: time.Unix(1700000000, 0).UTC(),
		Operation:  interaction.OperationToolCall,
	}
	accepted, err := corpus.RecordInteractionResult(ctx, session)
	if !IsCommittedInteraction(err) ||
		!shoal.IsErrorCode(err, shoal.ErrorCanceled) {
		t.Fatalf("post-commit cancellation error = %v", err)
	}
	if accepted.ID != session.ID {
		t.Fatalf("accepted session = %+v", accepted)
	}
	if _, err := corpus.Interaction(
		context.Background(), session.ID); err != nil {
		t.Fatalf("committed session is unavailable: %v", err)
	}
}

func TestInteractionResultDoesNotPerformPostCommitPublicRead(t *testing.T) {
	corpus, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	ctx := &stagedCancellationContext{
		Context: context.Background(), cancelAfter: 3,
	}
	session := interaction.Session{
		ID:         interaction.DerivedID("session", "single-lock-result"),
		RecordedAt: time.Unix(1700000000, 0).UTC(),
		Operation:  interaction.OperationToolCall,
	}
	accepted, err := corpus.RecordInteractionResult(ctx, session)
	if err != nil {
		t.Fatalf("atomic result performed a post-commit read: %v", err)
	}
	if accepted.ID != session.ID {
		t.Fatalf("accepted session = %+v", accepted)
	}
	ctx.mu.Lock()
	calls := ctx.calls
	ctx.mu.Unlock()
	if calls != 2 {
		t.Fatalf("context checks = %d, want admission and post-commit only", calls)
	}
}

func TestInteractionResultIsAtomicAgainstConcurrentDeletion(t *testing.T) {
	ctx := context.Background()
	corpus, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	session := interaction.Session{
		ID:         interaction.DerivedID("session", "delete-race"),
		RecordedAt: time.Unix(1700000000, 0).UTC(),
		Operation:  interaction.OperationToolCall,
	}
	written := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	write := corpus.writeRecord
	corpus.interactionRecordWriter = func(
		row []byte, kind byte, value any,
	) error {
		if err := write(row, kind, value); err != nil {
			return err
		}
		if record, ok := value.(persistedInteraction); ok &&
			!record.Deleted {
			once.Do(func() {
				close(written)
				<-release
			})
		}
		return nil
	}
	type result struct {
		session interaction.Session
		err     error
	}
	recorded := make(chan result, 1)
	go func() {
		accepted, recordErr := corpus.RecordInteractionResult(ctx, session)
		recorded <- result{session: accepted, err: recordErr}
	}()
	<-written
	deleteStarted := make(chan struct{})
	deleted := make(chan error, 1)
	go func() {
		close(deleteStarted)
		_, deleteErr := corpus.DeleteInteraction(ctx, session.ID)
		deleted <- deleteErr
	}()
	<-deleteStarted
	time.Sleep(20 * time.Millisecond)
	close(release)
	recordResult := <-recorded
	if recordResult.err != nil || recordResult.session.ID != session.ID {
		t.Fatalf("atomic record result = %+v, %v",
			recordResult.session, recordResult.err)
	}
	if err := <-deleted; err != nil {
		t.Fatalf("concurrent deletion failed: %v", err)
	}
}

func TestInteractionResultDoesNotRereadAfterVisibilityTightening(t *testing.T) {
	ctx := context.Background()
	corpus, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	source := Source{
		URI: "file:///visibility-race.txt", MediaType: MediaTypeText,
		Content: "stable source",
	}
	receipt, err := corpus.Ingest(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	view, err := corpus.Document(
		ctx, receipt.Document.ID, receipt.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	session := interaction.Session{
		ID:          interaction.DerivedID("session", "visibility-race"),
		RecordedAt:  time.Unix(1700000000, 0).UTC(),
		Operation:   interaction.OperationRetrieval,
		SeedNodeIDs: []shoal.ID{view.Root.Spans[0].ID},
	}
	written := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	write := corpus.writeRecord
	corpus.interactionRecordWriter = func(
		row []byte, kind byte, value any,
	) error {
		if err := write(row, kind, value); err != nil {
			return err
		}
		if record, ok := value.(persistedInteraction); ok &&
			!record.Deleted {
			once.Do(func() {
				close(written)
				<-release
			})
		}
		return nil
	}
	type result struct {
		session interaction.Session
		err     error
	}
	recorded := make(chan result, 1)
	go func() {
		accepted, recordErr := corpus.RecordInteractionResult(ctx, session)
		recorded <- result{session: accepted, err: recordErr}
	}()
	<-written
	ingestStarted := make(chan struct{})
	ingested := make(chan error, 1)
	go func() {
		close(ingestStarted)
		source.Metadata = shoal.Metadata{
			interaction.PropertyVisibility: "restricted",
		}
		_, ingestErr := corpus.Ingest(ctx, source)
		ingested <- ingestErr
	}()
	<-ingestStarted
	time.Sleep(20 * time.Millisecond)
	close(release)
	recordResult := <-recorded
	if recordResult.err != nil || recordResult.session.ID != session.ID {
		t.Fatalf("atomic record result = %+v, %v",
			recordResult.session, recordResult.err)
	}
	if err := <-ingested; err != nil {
		t.Fatalf("visibility tightening failed: %v", err)
	}
	if _, err := corpus.InteractionRecord(
		ctx, session.ID); err == nil {
		t.Fatal("tightened source left the interaction readable")
	}
}

func TestInteractionLoadRejectsConflictingLiveVersions(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	corpus, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	session := interaction.Session{
		ID:         interaction.DerivedID("session", "conflicting-live"),
		RecordedAt: time.Unix(1700000000, 0).UTC(),
		Operation:  interaction.OperationRetrieval,
	}
	if err := corpus.RecordInteraction(ctx, session); err != nil {
		t.Fatal(err)
	}
	conflicting := *corpus.interactions[session.ID]
	conflicting.Session.StopReason = "different"
	if err := validatePersistedInteraction(conflicting); err != nil {
		t.Fatal(err)
	}
	if err := corpus.writeRecord(
		interactionRecordRow(session.ID),
		embeddedRecordInteraction,
		conflicting,
	); err != nil {
		t.Fatal(err)
	}
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}
	if reopened, err := Open(dir); err == nil {
		_ = reopened.Close()
		t.Fatal("conflicting durable live interaction versions were accepted")
	}
}

func TestFoldRetryAdoptsCommittedRecord(t *testing.T) {
	ctx := context.Background()
	corpus, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	session := interaction.Session{
		ID:         interaction.DerivedID("session", "fold-retry"),
		RecordedAt: time.Unix(1700000000, 0).UTC(),
		Operation:  interaction.OperationRetrieval,
	}
	if err := corpus.RecordInteraction(ctx, session); err != nil {
		t.Fatal(err)
	}
	summaryDigest := interaction.Digest("fold retry")
	fold := interaction.Fold{
		Members:       []interaction.FoldMember{{SessionID: session.ID}},
		SummaryDigest: summaryDigest,
		FoldedAt:      time.Unix(1700000100, 0).UTC(),
	}
	subgraph, err := fold.Subgraph(func(shoal.ID) ([]string, error) {
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := fold.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	record := persistedFold{
		FoldID: subgraph.ID, Members: canonical.Members,
		SummaryDigest: canonical.SummaryDigest,
		Nodes:         subgraph.Nodes, Edges: subgraph.Edges,
		Visibility: interaction.Expression(subgraph.Visibility),
		FoldedAt:   fold.FoldedAt,
	}
	if err := corpus.writeRecord(
		foldRecordRow(record.FoldID), embeddedRecordFold, record,
	); err != nil {
		t.Fatal(err)
	}
	result, err := corpus.FoldInteractions(ctx, FoldRequest{
		SessionIDs: []shoal.ID{session.ID}, SummaryDigest: summaryDigest,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Created || !result.FoldedAt.Equal(record.FoldedAt) {
		t.Fatalf("reconciled fold result = %+v", result)
	}
}

func TestFoldRetryRejectsCommittedRecordAfterSourceChange(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*testing.T, *Explorer, Source, shoal.ID)
	}{
		{
			name: "visibility reclassification",
			change: func(
				t *testing.T, corpus *Explorer, source Source, _ shoal.ID,
			) {
				source.Metadata = shoal.Metadata{
					interaction.PropertyVisibility: "restricted",
				}
				if _, err := corpus.Ingest(
					context.Background(), source); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "member deletion",
			change: func(
				t *testing.T, corpus *Explorer, _ Source, sessionID shoal.ID,
			) {
				if _, err := corpus.DeleteInteraction(
					context.Background(), sessionID); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			corpus, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer corpus.Close()
			source := Source{
				URI:       "file:///fold-retry-source.txt",
				MediaType: MediaTypeText,
				Content:   "stable fold source",
				Metadata: shoal.Metadata{
					interaction.PropertyVisibility: "open",
				},
			}
			receipt, err := corpus.Ingest(ctx, source)
			if err != nil {
				t.Fatal(err)
			}
			view, err := corpus.Document(
				ctx, receipt.Document.ID, receipt.Revision.ID)
			if err != nil {
				t.Fatal(err)
			}
			session := interaction.Session{
				ID: interaction.DerivedID(
					"session", "fold-source-change", test.name),
				RecordedAt:  time.Unix(1700000000, 0).UTC(),
				Operation:   interaction.OperationRetrieval,
				SeedNodeIDs: []shoal.ID{view.Root.Spans[0].ID},
			}
			if err := corpus.RecordInteraction(ctx, session); err != nil {
				t.Fatal(err)
			}
			summaryDigest := interaction.Digest(
				"fold source change " + test.name)
			write := corpus.writeRecord
			foldWrites := 0
			var accepted persistedFold
			corpus.interactionRecordWriter = func(
				row []byte, kind byte, value any,
			) error {
				if fold, ok := value.(persistedFold); ok &&
					!fold.Deleted {
					foldWrites++
					if foldWrites > 1 {
						return errors.New("unexpected second fold write")
					}
					accepted = fold
					if err := write(row, kind, value); err != nil {
						return err
					}
					return errors.New("simulated committed fold error")
				}
				return write(row, kind, value)
			}
			if _, err := corpus.FoldInteractions(ctx, FoldRequest{
				SessionIDs:    []shoal.ID{session.ID},
				SummaryDigest: summaryDigest,
			}); err == nil {
				t.Fatalf("initial fold error = %v", err)
			}
			if accepted.FoldID == "" || foldWrites != 1 {
				t.Fatalf("accepted fold = %+v, writes = %d",
					accepted, foldWrites)
			}
			test.change(t, corpus, source, session.ID)
			_, err = corpus.FoldInteractions(ctx, FoldRequest{
				SessionIDs:    []shoal.ID{session.ID},
				SummaryDigest: summaryDigest,
			})
			if err == nil {
				t.Fatal("committed fold retry reused stale member evidence")
			}
			if foldWrites != 1 {
				t.Fatalf("fold writes = %d", foldWrites)
			}
		})
	}
}

func TestDeleteRetryAdoptsCommittedTombstone(t *testing.T) {
	ctx := context.Background()
	corpus, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	session := interaction.Session{
		ID:         interaction.DerivedID("session", "delete-retry"),
		RecordedAt: time.Unix(1700000000, 0).UTC(),
		Operation:  interaction.OperationRetrieval,
	}
	if err := corpus.RecordInteraction(ctx, session); err != nil {
		t.Fatal(err)
	}
	existing := corpus.interactions[session.ID]
	deletedAt := time.Unix(1700000200, 0).UTC()
	tombstone := interaction.Tombstone{
		SessionID: session.ID, DeletedAt: deletedAt,
		NodeCount: len(existing.Nodes), EdgeCount: len(existing.Edges),
	}
	node, err := tombstone.Node()
	if err != nil {
		t.Fatal(err)
	}
	record := persistedInteraction{
		SessionID: session.ID, Operation: existing.Operation,
		Nodes: []graph.Node{node}, RecordedAt: existing.RecordedAt,
		Deleted: true, DeletedAt: deletedAt,
	}
	if err := validatePersistedInteraction(record); err != nil {
		t.Fatal(err)
	}
	if err := corpus.writeRecord(
		interactionRecordRow(session.ID),
		embeddedRecordInteraction,
		record,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := corpus.DeleteInteraction(
		ctx, session.ID,
	); !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("reconciled delete error = %v", err)
	}
	if got := corpus.interactions[session.ID]; !got.Deleted ||
		!got.DeletedAt.Equal(deletedAt) {
		t.Fatalf("committed tombstone was not adopted: %+v", got)
	}
}

func TestFoldLoadRejectsConflictingLiveVersions(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	corpus, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	session := interaction.Session{
		ID:         interaction.DerivedID("session", "fold-live-conflict"),
		RecordedAt: time.Unix(1700000000, 0).UTC(),
		Operation:  interaction.OperationRetrieval,
	}
	if err := corpus.RecordInteraction(ctx, session); err != nil {
		t.Fatal(err)
	}
	result, err := corpus.FoldInteractions(ctx, FoldRequest{
		SessionIDs:    []shoal.ID{session.ID},
		SummaryDigest: interaction.Digest("conflicting fold"),
	})
	if err != nil {
		t.Fatal(err)
	}
	conflicting := *corpus.folds[result.FoldID]
	conflicting.FoldedAt = conflicting.FoldedAt.Add(time.Second)
	if err := validatePersistedFold(conflicting); err != nil {
		t.Fatal(err)
	}
	if err := corpus.writeRecord(
		foldRecordRow(result.FoldID), embeddedRecordFold, conflicting,
	); err != nil {
		t.Fatal(err)
	}
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}
	if reopened, err := Open(dir); err == nil {
		_ = reopened.Close()
		t.Fatal("conflicting durable live fold versions were accepted")
	}
}

func TestExactReadbackChecksOnlyCurrentVersion(t *testing.T) {
	corpus, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	older := persistedInteractionSink{
		CheckedAt: time.Unix(1700000000, 0).UTC(),
	}
	expected, err := encodeEmbeddedRecord(
		embeddedRecordInteractionSink, older)
	if err != nil {
		t.Fatal(err)
	}
	if err := corpus.writeRecord(
		interactionSinkRow, embeddedRecordInteractionSink, older,
	); err != nil {
		t.Fatal(err)
	}
	newer := persistedInteractionSink{
		CheckedAt: older.CheckedAt.Add(time.Second),
	}
	if err := corpus.writeRecord(
		interactionSinkRow, embeddedRecordInteractionSink, newer,
	); err != nil {
		t.Fatal(err)
	}
	committed, err := corpus.hasExactRecord(interactionSinkRow, expected)
	if err != nil {
		t.Fatal(err)
	}
	if committed {
		t.Fatal("historical matching value masked the current durable value")
	}
}

func TestConditionalInteractionCreateKeepsOneWinner(t *testing.T) {
	corpus, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	recordedAt := time.Unix(1700000000, 0).UTC()
	build := func(stopReason string) persistedInteraction {
		session := interaction.Session{
			ID:         interaction.DerivedID("session", "cas-winner"),
			RecordedAt: recordedAt, Operation: interaction.OperationRetrieval,
			StopReason: stopReason,
		}
		subgraph, err := session.Subgraph(
			func(shoal.ID) ([]string, error) { return nil, nil })
		if err != nil {
			t.Fatal(err)
		}
		return persistedInteraction{
			SessionID: session.ID, Session: session,
			Operation: session.Operation, Nodes: subgraph.Nodes,
			Edges: subgraph.Edges, RecordedAt: recordedAt,
		}
	}
	first := build("first")
	accepted, err := corpus.createInteractionRecord(
		interactionRecordRow(first.SessionID),
		embeddedRecordInteraction,
		first,
	)
	if err != nil || !accepted {
		t.Fatalf("first conditional create = %t, %v", accepted, err)
	}
	second := build("second")
	accepted, err = corpus.createInteractionRecord(
		interactionRecordRow(second.SessionID),
		embeddedRecordInteraction,
		second,
	)
	if err != nil {
		t.Fatal(err)
	}
	if accepted {
		t.Fatal("conflicting conditional create was accepted")
	}
	stored, found, err := corpus.lookupPersistedInteraction(first.SessionID)
	if err != nil || !found {
		t.Fatalf("lookup winner = %t, %v", found, err)
	}
	if stored.Session.StopReason != first.Session.StopReason {
		t.Fatalf("durable winner = %+v", stored.Session)
	}
}

func TestInteractionRetryCancellationRemainsCommitted(t *testing.T) {
	corpus, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	session := interaction.Session{
		ID:         interaction.DerivedID("session", "retry-cancellation"),
		RecordedAt: time.Unix(1700000000, 0).UTC(),
		Operation:  interaction.OperationToolCall,
	}
	first, err := corpus.RecordInteractionResult(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}
	// The exact record is already durable, so the retry observes it and only
	// then sees the cancellation.
	ctx := &stagedCancellationContext{
		Context: context.Background(), cancelAfter: 2,
	}
	retried, err := corpus.RecordInteractionResult(ctx, session)
	if err == nil {
		t.Fatal("cancellation after durable reconciliation was not reported")
	}
	if !IsCommittedInteraction(err) {
		t.Fatalf("durable retry cancellation reported as rollback: %v", err)
	}
	if !reflect.DeepEqual(retried, first) {
		t.Fatalf("reconciled session = %+v, want %+v", retried, first)
	}
}

// TestSnapshotObjectDigestSeparatesOpaqueBytes pins that the snapshot binding
// digest distinguishes graph objects whose opaque IDs, endpoints, or metadata
// differ only in bytes that a JSON encoding would fold onto U+FFFD.
func TestSnapshotObjectDigestSeparatesOpaqueBytes(t *testing.T) {
	pinnedEdge := graph.Edge{
		ID: "edge", From: "from", To: shoal.ID([]byte{0xFF}), Type: "cites",
	}
	mutatedEdge := pinnedEdge
	mutatedEdge.To = shoal.ID([]byte{0xFE})
	pinned, err := snapshotObjectDigest(pinnedEdge)
	if err != nil {
		t.Fatalf("digest pinned edge: %v", err)
	}
	mutated, err := snapshotObjectDigest(mutatedEdge)
	if err != nil {
		t.Fatalf("digest mutated edge: %v", err)
	}
	if pinned == mutated {
		t.Fatal("mutated edge endpoint kept its pinned snapshot digest")
	}
	pinnedNode := graph.Node{
		ID:         "node",
		Kind:       "entity",
		Properties: shoal.Metadata{"p": string([]byte{0xFF, 'A'})},
	}
	mutatedNode := graph.Node{
		ID:         "node",
		Kind:       "entity",
		Properties: shoal.Metadata{"p": string([]byte{0x80, 'A'})},
	}
	pinned, err = snapshotObjectDigest(pinnedNode)
	if err != nil {
		t.Fatalf("digest pinned node: %v", err)
	}
	mutated, err = snapshotObjectDigest(mutatedNode)
	if err != nil {
		t.Fatalf("digest mutated node: %v", err)
	}
	if pinned == mutated {
		t.Fatal("mutated node metadata kept its pinned snapshot digest")
	}
	if _, err := snapshotObjectDigest("unsupported"); err == nil {
		t.Fatal("expected an error for an unknown snapshot object")
	}
}

// TestDeletedFoldMemberProvenanceIsNotRehydratable pins that a fold whose
// create returned an indeterminate commit cannot be used to recover the
// provenance of a member session that was deleted before the fold was
// reconciled. The in-memory guard in DeleteInteraction cannot see that fold,
// so the retention contract has to hold at the exposure points too.
func TestDeletedFoldMemberProvenanceIsNotRehydratable(t *testing.T) {
	ctx := context.Background()
	corpus, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	receipt, err := corpus.Ingest(ctx, Source{
		URI:       "file:///deleted-fold-member.txt",
		MediaType: MediaTypeText,
		Content:   "folded member source",
		Metadata: shoal.Metadata{
			interaction.PropertyVisibility: "open",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	view, err := corpus.Document(ctx, receipt.Document.ID, receipt.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	session := interaction.Session{
		ID:          interaction.DerivedID("session", "deleted-fold-member"),
		RecordedAt:  time.Unix(1700000000, 0).UTC(),
		Operation:   interaction.OperationRetrieval,
		SeedNodeIDs: []shoal.ID{view.Root.Spans[0].ID},
	}
	if err := corpus.RecordInteraction(ctx, session); err != nil {
		t.Fatal(err)
	}
	request := FoldRequest{
		SessionIDs:    []shoal.ID{session.ID},
		SummaryDigest: interaction.Digest("deleted fold member"),
	}
	write := corpus.writeRecord
	var accepted persistedFold
	corpus.interactionRecordWriter = func(
		row []byte, kind byte, value any,
	) error {
		if fold, ok := value.(persistedFold); ok && !fold.Deleted {
			accepted = fold
			if err := write(row, kind, value); err != nil {
				return err
			}
			return errors.New("simulated committed fold error")
		}
		return write(row, kind, value)
	}
	if _, err := corpus.FoldInteractions(ctx, request); err == nil {
		t.Fatal("expected the simulated committed fold error")
	}
	if _, err := corpus.DeleteInteraction(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := corpus.FoldInteractions(ctx, request); err == nil {
		t.Fatal("committed fold retry reused a deleted member")
	}
	if _, err := corpus.RehydrateFold(ctx, accepted.FoldID); err == nil {
		t.Fatal("rehydrated the provenance of a deleted session")
	}
	if _, err := corpus.FoldSubgraph(ctx, accepted.FoldID); err == nil {
		t.Fatal("served the subgraph of a deleted session's provenance")
	}
}
