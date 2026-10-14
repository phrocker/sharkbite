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
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const restrictedMarkdown = `# Incident Log

The retry budget was exhausted during the outage.
`

const publicMarkdown = `# Runbook

Retry the failing request with exponential backoff.
`

// ingestVisible ingests one source with a declared visibility expression and
// returns the span node IDs it materialized.
func ingestVisible(
	t testing.TB, corpus *explorer.Explorer, uri, content, visibility string,
) []shoal.ID {
	t.Helper()
	source := explorer.Source{
		URI:       uri,
		MediaType: explorer.MediaTypeMarkdown,
		Content:   content,
	}
	if visibility != "" {
		source.Metadata = shoal.Metadata{
			interaction.PropertyVisibility: visibility,
		}
	}
	receipt, err := corpus.Ingest(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	neighborhood, err := corpus.Neighborhood(
		context.Background(), explorer.NeighborhoodRequest{
			NodeIDs: []shoal.ID{receipt.Document.ID},
			Depth:   16,
		})
	if err != nil {
		t.Fatal(err)
	}
	var spans []shoal.ID
	for _, node := range neighborhood.Nodes {
		if node.Kind == "span" {
			spans = append(spans, node.ID)
		}
	}
	if len(spans) == 0 {
		t.Fatalf("ingest of %q produced no span nodes", uri)
	}
	return spans
}

func exactGraphEvidence(
	t testing.TB, corpus *explorer.Explorer, edge graph.Edge,
) interaction.EvidenceReference {
	t.Helper()
	neighborhood, err := corpus.Neighborhood(
		context.Background(), explorer.NeighborhoodRequest{
			NodeIDs: []shoal.ID{edge.From}, Depth: 1,
		})
	if err != nil {
		t.Fatal(err)
	}
	nodes := make(map[shoal.ID]graph.Node, len(neighborhood.Nodes))
	for _, node := range neighborhood.Nodes {
		nodes[node.ID] = node
	}
	var exact graph.Edge
	for _, candidate := range neighborhood.Edges {
		if candidate.ID == edge.ID {
			exact = candidate
			break
		}
	}
	if exact.ID == "" || nodes[edge.From].ID == "" || nodes[edge.To].ID == "" {
		t.Fatal("exact graph evidence is unavailable")
	}
	anchor, err := inference.NewGraphAnchor(graph.Path{
		Nodes: []graph.Node{nodes[edge.From], nodes[edge.To]},
		Edges: []graph.Edge{exact},
	})
	if err != nil {
		t.Fatal(err)
	}
	reference, err := anchor.EvidenceReference()
	if err != nil {
		t.Fatal(err)
	}
	return reference
}

func recordedSession(
	t testing.TB,
	corpus *explorer.Explorer,
	id shoal.ID,
	retrieved []shoal.ID,
	cited []shoal.ID,
) interaction.Session {
	t.Helper()
	session := interaction.Session{
		ID:                       id,
		RecordedAt:               time.Unix(1700000000, 0).UTC(),
		SnapshotID:               "snapshot-observed",
		SnapshotAsOf:             time.Unix(1699999900, 0).UTC(),
		AuthorizationFingerprint: "auth-sha256:test",
		AuthorizationExpiresAt:   time.Unix(1700003600, 0).UTC(),
		EmbeddingSpaceID:         "embedding-space-test",
		Provenance: interaction.Provenance{
			Harness:  "shoal.harness.v1",
			Provider: "fake",
			Model:    "deterministic",
		},
		QueryDigest: interaction.Digest("why did the retry budget run out"),
		StopReason:  "answered",
		SeedNodeIDs: retrieved,
		Turns: []interaction.Turn{{
			Index:    0,
			Decision: "retrieve",
			ToolCall: &interaction.ToolCall{
				Kind:             "retrieve",
				RetrievedNodeIDs: retrieved,
			},
		}},
		CitedNodeIDs: cited,
	}
	if err := corpus.RecordInteraction(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	return session
}

// TestInteractionVisibilityIsConjunctionOfTouchedSpans pins binding decision 2:
// an interaction node carries the conjunction of every visibility label of
// every span it touched, never the asker's grant set.
func TestInteractionVisibilityIsConjunctionOfTouchedSpans(t *testing.T) {
	ctx := context.Background()
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	defer corpus.Close()

	restricted := ingestVisible(
		t, corpus, "file:///incident.md", restrictedMarkdown, "secret&incident")
	open := ingestVisible(
		t, corpus, "file:///runbook.md", publicMarkdown, "ops")

	// The session is shown the restricted span but only cites the open one.
	// Visibility must still account for what the model was shown.
	recordedSession(
		t, corpus, "interaction.session_conjunction",
		[]shoal.ID{restricted[0], open[0]},
		[]shoal.ID{open[0]},
	)

	summaries, err := corpus.Interactions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 {
		t.Fatalf("interactions = %d, want 1", len(summaries))
	}
	if summaries[0].Visibility != "incident&ops&secret" {
		t.Fatalf("session visibility = %q, want %q",
			summaries[0].Visibility, "incident&ops&secret")
	}

	sub, err := corpus.InteractionSubgraph(ctx, "interaction.session_conjunction")
	if err != nil {
		t.Fatal(err)
	}
	var sessionNode *graph.Node
	for i := range sub.Nodes {
		if sub.Nodes[i].Kind == interaction.KindSession {
			sessionNode = &sub.Nodes[i]
		}
	}
	if sessionNode == nil {
		t.Fatal("subgraph has no interaction.session node")
	}
	if got := sessionNode.Properties[interaction.PropertyVisibility]; got !=
		"incident&ops&secret" {
		t.Fatalf("session node visibility = %q", got)
	}

	// A cited-only projection would understate exposure. Confirm the retrieved
	// edge to the restricted span exists and is distinguished from citation.
	var retrievedRestricted, citedRestricted bool
	for _, edge := range sub.Edges {
		if edge.To != restricted[0] {
			continue
		}
		switch edge.Type {
		case interaction.EdgeRetrieved:
			retrievedRestricted = true
		case interaction.EdgeCited:
			citedRestricted = true
		}
	}
	if !retrievedRestricted {
		t.Fatal("no interaction.retrieved edge to the restricted span")
	}
	if citedRestricted {
		t.Fatal("restricted span was not cited but has a cited edge")
	}
}

func TestInteractionHydratesAllProvenanceWithoutMovingSnapshot(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	corpus, err := explorer.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()

	var source strings.Builder
	for index := 0; index < 25; index++ {
		source.WriteString("# Section ")
		source.WriteString(strconv.Itoa(index))
		source.WriteString("\n\nsource token ")
		source.WriteString(strconv.Itoa(index))
		source.WriteString("\n\n")
	}
	spans := ingestVisible(
		t, corpus, "file:///many.md", source.String(), "ops")
	if len(spans) < 21 {
		t.Fatalf("ingest produced %d spans, want at least 21", len(spans))
	}
	before, err := corpus.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	session := recordedSession(
		t, corpus, "interaction.session_durable", spans, []shoal.ID{spans[len(spans)-1]})
	after, err := corpus.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("interaction write moved content snapshot: before=%+v after=%+v",
			before, after)
	}
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := explorer.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	hydrated, err := reopened.Interaction(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if hydrated.SnapshotID != session.SnapshotID ||
		!hydrated.SnapshotAsOf.Equal(session.SnapshotAsOf) ||
		hydrated.AuthorizationFingerprint != session.AuthorizationFingerprint ||
		!hydrated.AuthorizationExpiresAt.Equal(session.AuthorizationExpiresAt) ||
		hydrated.EmbeddingSpaceID != session.EmbeddingSpaceID {
		t.Fatalf("hydrated pins = %+v, want %+v", hydrated, session)
	}
	if len(hydrated.SeedNodeIDs) != len(spans) ||
		hydrated.SeedNodeIDs[len(hydrated.SeedNodeIDs)-1] != spans[len(spans)-1] {
		t.Fatalf("hydrated retrieved IDs = %d, late source was lost",
			len(hydrated.SeedNodeIDs))
	}
	subgraph, err := reopened.InteractionSubgraph(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	retrieved, cited := 0, 0
	inferenceFound := false
	for _, node := range subgraph.Nodes {
		if node.ID == interaction.InferenceID(session.ID) &&
			node.Kind == interaction.KindInference {
			inferenceFound = true
		}
	}
	for _, edge := range subgraph.Edges {
		switch edge.Type {
		case interaction.EdgeRetrieved:
			retrieved++
		case interaction.EdgeCited:
			cited++
		}
	}
	if !inferenceFound || retrieved != len(spans)*2 || cited != 1 {
		t.Fatalf("inference=%t retrieved=%d cited=%d",
			inferenceFound, retrieved, cited)
	}
}

func TestGenericRecorderSurvivesRestartAndStaysSourceOnly(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	corpus, err := explorer.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = corpus.Close() })
	spans := ingestVisible(
		t, corpus, "file:///generic-retrieval.md", publicMarkdown, "ops")
	before, err := corpus.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	recordedAt := before.AsOf.Add(time.Second)
	recorder, err := interaction.NewRecorder(ctx, corpus)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.SetClock(func() time.Time { return recordedAt }); err != nil {
		t.Fatal(err)
	}
	sessionID, err := interaction.OperationSessionID(
		interaction.OperationRetrieval, "retrieval-request-1", recordedAt)
	if err != nil {
		t.Fatal(err)
	}
	reason, err := interaction.NewReason(
		"retrieve_context", "assemble grounded context")
	if err != nil {
		t.Fatal(err)
	}
	recorded, err := recorder.Record(ctx, interaction.Session{
		ID:        sessionID,
		Operation: interaction.OperationRetrieval,
		Actor: interaction.ActorContext{
			SubjectID:  "subject-1",
			ActorID:    "agent-1",
			ClientID:   "client-1",
			OnBehalfOf: []shoal.ID{"delegate-1", "delegate-2"},
		},
		Reason:                   reason,
		SnapshotID:               shoal.ID(before.ID),
		SnapshotAsOf:             before.AsOf,
		AuthorizationFingerprint: "auth-sha256:generic-recorder",
		AuthorizationExpiresAt:   before.AsOf.Add(time.Hour),
		SeedNodeIDs:              spans,
	})
	if err != nil {
		t.Fatal(err)
	}
	after, err := corpus.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("generic recorder moved snapshot: before=%+v after=%+v",
			before, after)
	}
	response, err := corpus.Retrieve(ctx, retrieval.Request{
		Text:  "exponential backoff",
		TopK:  50,
		Modes: []retrieval.Mode{retrieval.ModeLexical},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range response.Results {
		if interaction.IsInteractionID(result.ID) {
			t.Fatalf("default retrieval returned derived result %q", result.ID)
		}
	}
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := explorer.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	hydrated, err := reopened.Interaction(ctx, recorded.ID)
	if err != nil {
		t.Fatal(err)
	}
	summaries, err := reopened.Interactions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].InferenceID != "" {
		t.Fatalf("generic interaction summary = %+v", summaries)
	}
	if hydrated.Operation != interaction.OperationRetrieval ||
		hydrated.Actor.SubjectID != "subject-1" ||
		hydrated.Actor.ActorID != "agent-1" ||
		hydrated.Actor.ClientID != "client-1" ||
		len(hydrated.Actor.OnBehalfOf) != 2 ||
		hydrated.Reason != reason {
		t.Fatalf("restarted interaction = %+v", hydrated)
	}
	subgraph, err := reopened.InteractionSubgraph(ctx, recorded.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range subgraph.Nodes {
		if node.Kind == interaction.KindInference {
			t.Fatal("generic retrieval acquired an inference node after restart")
		}
	}

}

func TestInteractionLifecyclePreservesPinnedSourceReads(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	corpus, restricted, open := foldedCorpus(t, dir)
	t.Cleanup(func() { _ = corpus.Close() })
	snapshot, err := corpus.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Like ExplorerToolHost, pin the current frontier separately: embedded
	// Retrieve deliberately does not implement historical Request.AsOf reads.
	requests := []retrieval.Request{
		{
			Text: "retry budget exhausted", TopK: 50,
			Modes: []retrieval.Mode{retrieval.ModeLexical},
		},
		{
			Text: "retry budget exhausted", TopK: 50,
			Modes: []retrieval.Mode{
				retrieval.ModeLexical, retrieval.ModeTree, retrieval.ModeGraph,
			},
		},
	}
	baseline := make([]retrieval.Response, len(requests))
	for i, request := range requests {
		baseline[i], err = corpus.Retrieve(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		if len(baseline[i].Results) == 0 {
			t.Fatalf("baseline modes %v returned no source evidence", request.Modes)
		}
	}
	assertPinnedReads := func(stage string) {
		t.Helper()
		current, err := corpus.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if current != snapshot {
			t.Fatalf("%s moved the content snapshot: %+v != %+v",
				stage, current, snapshot)
		}
		if err := corpus.ValidateSnapshot(
			ctx, shoal.ID(snapshot.ID), snapshot.AsOf,
			[]shoal.ID{restricted[0], open[0]},
		); err != nil {
			t.Fatalf("%s invalidated pinned source evidence: %v", stage, err)
		}
		for i, request := range requests {
			got, err := corpus.Retrieve(ctx, request)
			if err != nil {
				t.Fatalf("%s modes %v: %v", stage, request.Modes, err)
			}
			if !reflect.DeepEqual(got, baseline[i]) {
				t.Fatalf("%s changed pinned source response for modes %v:\ngot %+v\nwant %+v",
					stage, request.Modes, got, baseline[i])
			}
			for _, result := range got.Results {
				if interaction.IsInteractionID(result.ID) {
					t.Fatalf("%s returned derived result %q", stage, result.ID)
				}
				for _, evidence := range result.Evidence {
					assertNoInteractionNodes(
						t, stage, evidence.Path.Nodes, evidence.Path.Edges)
				}
			}
		}
	}
	reopen := func() {
		t.Helper()
		if err := corpus.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := explorer.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		corpus = reopened
	}

	recorder, err := interaction.NewRecorder(ctx, corpus)
	if err != nil {
		t.Fatal(err)
	}
	session, err := recorder.Record(ctx, interaction.Session{
		ID:                       "interaction.session_pinned-lifecycle",
		Operation:                interaction.OperationRetrieval,
		SnapshotID:               shoal.ID(snapshot.ID),
		SnapshotAsOf:             snapshot.AsOf,
		AuthorizationFingerprint: "auth-sha256:pinned-lifecycle",
		AuthorizationExpiresAt:   snapshot.AsOf.Add(time.Hour),
		SeedNodeIDs:              []shoal.ID{restricted[0], open[0]},
		CitedNodeIDs:             []shoal.ID{open[0]},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertPinnedReads("capture")
	fold := foldOf(t, corpus, session.ID)
	assertPinnedReads("fold")
	reopen()
	assertPinnedReads("reopened fold")

	if _, err := corpus.DeleteFold(ctx, fold.FoldID); err != nil {
		t.Fatal(err)
	}
	assertPinnedReads("fold tombstone")
	if _, err := corpus.DeleteInteraction(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	assertPinnedReads("session tombstone")
	reopen()
	assertPinnedReads("reopened tombstones")
}

func TestRequiredVisibilitySurvivesRestart(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	corpus, err := explorer.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	spans := ingestVisible(
		t, corpus, "file:///required-visibility.md", publicMarkdown, "internal")
	snapshot, err := corpus.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	session := interaction.Session{
		ID:                       interaction.DerivedID("session", "required-visibility-restart"),
		RecordedAt:               snapshot.AsOf.Add(time.Second),
		Operation:                interaction.OperationRetrieval,
		SnapshotID:               shoal.ID(snapshot.ID),
		SnapshotAsOf:             snapshot.AsOf,
		AuthorizationFingerprint: "auth-sha256:required-visibility",
		AuthorizationExpiresAt:   snapshot.AsOf.Add(time.Hour),
		RequiredVisibility:       []string{"api", "api"},
		SeedNodeIDs:              spans,
	}
	accepted, err := corpus.RecordInteractionResult(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	if interaction.Expression(accepted.RequiredVisibility) != "api" {
		t.Fatalf("accepted required visibility = %v",
			accepted.RequiredVisibility)
	}
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := explorer.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	hydrated, err := reopened.Interaction(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if interaction.Expression(hydrated.RequiredVisibility) != "api" {
		t.Fatalf("restarted required visibility = %v",
			hydrated.RequiredVisibility)
	}
	summaries, err := reopened.Interactions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].Visibility != "api&internal" {
		t.Fatalf("restarted interaction summaries = %+v", summaries)
	}
}

func TestExactEdgeEvidenceSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	corpus, err := explorer.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	source := explorer.Source{
		URI: "file:///edge-evidence.txt", MediaType: explorer.MediaTypeText,
		Content: "exact edge evidence",
		Metadata: shoal.Metadata{
			interaction.PropertyVisibility: "ops",
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
	if len(view.Root.Spans) == 0 {
		t.Fatal("fixture produced no span")
	}
	edge := graph.Edge{
		ID:     "source-evidence-edge",
		From:   receipt.Document.ID,
		To:     view.Root.Spans[0].ID,
		Type:   "supports",
		Weight: 1,
		Properties: shoal.Metadata{
			interaction.PropertyVisibility: "edge-secret",
		},
	}
	if err := corpus.Connect(ctx, edge); err != nil {
		t.Fatal(err)
	}
	snapshot, err := corpus.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	embeddingSpaces, err := interaction.NewEmbeddingSpaceSet([]string{
		"18:embedding-space-v15:alpha",
		"18:embedding-space-v14:beta",
	})
	if err != nil {
		t.Fatal(err)
	}
	evidence := exactGraphEvidence(t, corpus, edge)
	session := interaction.Session{
		ID:                       interaction.DerivedID("session", "edge-restart"),
		RecordedAt:               snapshot.AsOf.Add(time.Second),
		Operation:                interaction.OperationRetrieval,
		SnapshotID:               shoal.ID(snapshot.ID),
		SnapshotAsOf:             snapshot.AsOf,
		AuthorizationFingerprint: "auth-sha256:edge-restart",
		AuthorizationExpiresAt:   snapshot.AsOf.Add(time.Hour),
		EmbeddingSpaces:          embeddingSpaces,
		SeedNodeIDs:              []shoal.ID{edge.From, edge.To},
		SeedEvidence:             []interaction.EvidenceReference{evidence},
	}
	forgedAnchor := session
	forgedAnchor.ID = interaction.DerivedID(
		"session", "forged-graph-anchor")
	forgedAnchor.SeedEvidence = cloneTestEvidence(session.SeedEvidence)
	forgedAnchor.SeedEvidence[0].AnchorID = "forged-anchor"
	if err := corpus.RecordInteraction(
		ctx, forgedAnchor,
	); !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("forged graph anchor recording error = %v", err)
	}
	forged := session
	forged.ID = interaction.DerivedID("session", "forged-assertion")
	forged.SeedEvidence = cloneTestEvidence(session.SeedEvidence)
	forged.SeedEvidence[0].Assertions =
		[]interaction.AssertionReference{{
			AssertionID: "forged-assertion",
			EdgeID:      edge.ID,
			Origin:      ontology.AssertionExplicit,
		}}
	if err := corpus.RecordInteraction(
		ctx, forged,
	); !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("forged assertion recording error = %v", err)
	}
	if err := corpus.RecordInteraction(ctx, session); err != nil {
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
	record, err := reopened.InteractionRecord(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(record.TouchedEdgeIDs) != 1 ||
		record.TouchedEdgeIDs[0] != edge.ID ||
		len(record.Session.SeedEvidence) != 1 ||
		record.Session.SeedEvidence[0].EdgeIDs[0] != edge.ID {
		t.Fatalf("hydrated exact evidence = %+v", record)
	}
	if !reflect.DeepEqual(
		record.Session.EmbeddingSpaces, embeddingSpaces,
	) || record.Summary.EmbeddingSpaceDigest != embeddingSpaces.Digest ||
		record.Summary.EmbeddingSpaceCount != 2 {
		t.Fatalf("hydrated embedding spaces = %+v", record)
	}
	if record.Summary.Visibility != "edge-secret&ops" {
		t.Fatalf("edge-aware visibility = %q", record.Summary.Visibility)
	}
}

func TestRecordInteractionRejectsForgedCitationAnchor(t *testing.T) {
	ctx := context.Background()
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	receipt, err := corpus.Ingest(ctx, explorer.Source{
		URI:       "file:///citation-evidence.txt",
		MediaType: explorer.MediaTypeText,
		Content:   "trusted citation bytes",
	})
	if err != nil {
		t.Fatal(err)
	}
	view, err := corpus.Document(
		ctx, receipt.Document.ID, receipt.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Root.Spans) == 0 {
		t.Fatal("fixture produced no span")
	}
	span := view.Root.Spans[0]
	citation := document.Citation{
		DocumentID: receipt.Document.ID,
		RevisionID: receipt.Revision.ID,
		SectionID:  span.SectionID,
		SpanID:     span.ID,
		Range:      span.Range,
	}
	anchor, err := inference.NewDocumentAnchor(citation, span.Text)
	if err != nil {
		t.Fatal(err)
	}
	reference, err := anchor.EvidenceReference()
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := corpus.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	session := interaction.Session{
		ID:                       interaction.DerivedID("session", "citation-anchor"),
		RecordedAt:               snapshot.AsOf.Add(time.Second),
		Operation:                interaction.OperationRetrieval,
		SnapshotID:               shoal.ID(snapshot.ID),
		SnapshotAsOf:             snapshot.AsOf,
		AuthorizationFingerprint: "auth-sha256:citation-anchor",
		AuthorizationExpiresAt:   snapshot.AsOf.Add(time.Hour),
		SeedNodeIDs:              reference.NodeIDs,
		SeedEvidence:             []interaction.EvidenceReference{reference},
	}
	forged := session
	forged.ID = interaction.DerivedID("session", "forged-citation-anchor")
	forged.SeedEvidence = cloneTestEvidence(session.SeedEvidence)
	forged.SeedEvidence[0].Citation.Range.Start.Offset++
	if err := corpus.RecordInteraction(
		ctx, forged,
	); !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("forged citation anchor recording error = %v", err)
	}
	if err := corpus.RecordInteraction(ctx, session); err != nil {
		t.Fatalf("authoritative citation was rejected: %v", err)
	}
}

func cloneTestEvidence(
	references []interaction.EvidenceReference,
) []interaction.EvidenceReference {
	cloned := make([]interaction.EvidenceReference, len(references))
	for index, reference := range references {
		cloned[index] = reference
		cloned[index].NodeIDs = append(
			[]shoal.ID(nil), reference.NodeIDs...)
		cloned[index].EdgeIDs = append(
			[]shoal.ID(nil), reference.EdgeIDs...)
		cloned[index].Assertions = append(
			[]interaction.AssertionReference(nil), reference.Assertions...)
	}
	return cloned
}

func TestRecordInteractionRejectsReclassifiedPinnedSource(t *testing.T) {
	ctx := context.Background()
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	source := explorer.Source{
		URI: "file:///reclassified-pin.txt", MediaType: explorer.MediaTypeText,
		Content: "stable evidence",
		Metadata: shoal.Metadata{
			interaction.PropertyVisibility: "restricted",
		},
	}
	first, err := corpus.Ingest(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	view, err := corpus.Document(
		ctx, first.Document.ID, first.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Root.Spans) == 0 {
		t.Fatal("fixture produced no span")
	}
	snapshot, err := corpus.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	source.Metadata[interaction.PropertyVisibility] = "public"
	if _, err := corpus.Ingest(ctx, source); err != nil {
		t.Fatal(err)
	}
	session := interaction.Session{
		ID:                       interaction.DerivedID("session", "reclassified-pin"),
		RecordedAt:               snapshot.AsOf.Add(time.Second),
		Operation:                interaction.OperationRetrieval,
		SnapshotID:               shoal.ID(snapshot.ID),
		SnapshotAsOf:             snapshot.AsOf,
		AuthorizationFingerprint: "auth-sha256:reclassified-pin",
		AuthorizationExpiresAt:   snapshot.AsOf.Add(time.Hour),
		SeedNodeIDs:              []shoal.ID{view.Root.Spans[0].ID},
	}
	if err := corpus.RecordInteraction(
		ctx, session,
	); !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("reclassified pinned source recording error = %v", err)
	}
	if _, err := corpus.InteractionRecord(
		ctx, session.ID,
	); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("reclassified source persisted interaction: %v", err)
	}
}

func TestRecordInteractionVerifiesResolvedCitationRoles(t *testing.T) {
	ctx := context.Background()
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()

	first, err := corpus.Ingest(ctx, explorer.Source{
		URI: "file:///resolved-role-a.txt", MediaType: explorer.MediaTypeText,
		Content: "first source evidence",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := corpus.Ingest(ctx, explorer.Source{
		URI: "file:///resolved-role-b.txt", MediaType: explorer.MediaTypeText,
		Content: "second source evidence",
	})
	if err != nil {
		t.Fatal(err)
	}
	firstView, err := corpus.Document(
		ctx, first.Document.ID, first.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	secondView, err := corpus.Document(
		ctx, second.Document.ID, second.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(firstView.Root.Spans) == 0 ||
		len(secondView.Root.Spans) == 0 {
		t.Fatal("fixture produced no source spans")
	}
	firstSpan := firstView.Root.Spans[0]
	secondSpan := secondView.Root.Spans[0]
	citation := document.Citation{
		DocumentID: firstSpan.DocumentID,
		RevisionID: firstSpan.RevisionID,
		SpanID:     firstSpan.ID,
		Range:      firstSpan.Range,
	}
	anchor, err := inference.NewDocumentAnchor(citation, firstSpan.Text)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := corpus.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	reference := interaction.EvidenceReference{
		AnchorID: anchor.ID(),
		Kind:     interaction.EvidenceDocument,
		Citation: citation,
		NodeIDs: []shoal.ID{
			firstSpan.DocumentID, firstSpan.SectionID, firstSpan.ID,
		},
	}
	sessionFor := func(
		id shoal.ID,
		evidence interaction.EvidenceReference,
	) interaction.Session {
		return interaction.Session{
			ID:                       id,
			RecordedAt:               snapshot.AsOf.Add(time.Second),
			Operation:                interaction.OperationRetrieval,
			SnapshotID:               shoal.ID(snapshot.ID),
			SnapshotAsOf:             snapshot.AsOf,
			AuthorizationFingerprint: "auth-sha256:resolved-role",
			AuthorizationExpiresAt:   snapshot.AsOf.Add(time.Hour),
			SeedNodeIDs:              append([]shoal.ID(nil), evidence.NodeIDs...),
			SeedEvidence: []interaction.EvidenceReference{
				evidence,
			},
		}
	}
	accepted, err := corpus.RecordInteractionResult(
		ctx,
		sessionFor(
			interaction.DerivedID("session", "resolved-role-valid"),
			reference,
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	if accepted.SeedEvidence[0].Citation.SectionID != "" ||
		!reflect.DeepEqual(
			accepted.SeedEvidence[0].NodeIDs,
			[]shoal.ID{
				firstSpan.DocumentID, firstSpan.SectionID, firstSpan.ID,
			},
		) {
		t.Fatalf("accepted citation roles = %+v", accepted.SeedEvidence[0])
	}

	forged := reference
	forged.NodeIDs = []shoal.ID{
		firstSpan.DocumentID, secondSpan.SectionID, firstSpan.ID,
	}
	forgedID := interaction.DerivedID("session", "resolved-role-forged")
	if _, err := corpus.RecordInteractionResult(
		ctx, sessionFor(forgedID, forged),
	); !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("forged resolved source role error = %v", err)
	}
	if _, err := corpus.InteractionRecord(
		ctx, forgedID,
	); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("forged resolved source role persisted: %v", err)
	}
}

func TestGeneratedInteractionNodeIDsCannotCollide(t *testing.T) {
	ctx := context.Background()
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	spans := ingestVisible(
		t, corpus, "file:///collision.md", publicMarkdown, "ops")

	const futureSessionID shoal.ID = "future-session"
	collidingID := interaction.InferenceID(futureSessionID)
	session := recordedSession(
		t, corpus, "interaction.session_template", spans[:1], spans[:1])
	session.ID = collidingID
	if err := corpus.RecordInteraction(
		ctx, session,
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("reserved inference-node ID accepted as a session: %v", err)
	}

	const deleteTargetID shoal.ID = "interaction.session_delete_target"
	recordedSession(t, corpus, deleteTargetID, spans[:1], spans[:1])
	tombstoneCollisionID := interaction.TombstoneID(deleteTargetID)
	session.ID = tombstoneCollisionID
	if err := corpus.RecordInteraction(
		ctx, session,
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("reserved tombstone-node ID accepted as a session: %v", err)
	}
	if _, err := corpus.DeleteInteraction(ctx, deleteTargetID); err != nil {
		t.Fatalf("reserved ID prevented deletion: %v", err)
	}
}

// TestFutureSourceCannotCollideWithRecordedSessionID pins the cross-writer
// safety property: session identities are confined to the reserved
// interaction.session_ namespace, which every source publication path rejects,
// so no session can squat an identity a later source ingestion will mint --
// even when the two writes happen in different processes that never observed
// each other.
func TestFutureSourceCannotCollideWithRecordedSessionID(t *testing.T) {
	ctx := context.Background()
	futureSource := explorer.Source{
		URI:       "file:///future-collision.txt",
		MediaType: explorer.MediaTypeText,
		Content:   "future source collision",
	}
	probe, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := probe.Ingest(ctx, futureSource)
	if err != nil {
		t.Fatal(err)
	}
	view, err := probe.Document(ctx, receipt.Document.ID, receipt.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	futureNodeID := view.Root.Spans[0].ID
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}

	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	if err := corpus.RecordInteraction(ctx, interaction.Session{
		ID:         futureNodeID,
		RecordedAt: time.Unix(1700000000, 0).UTC(),
		Operation:  interaction.OperationRetrieval,
	}); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("source node ID accepted as a session ID: %v", err)
	}
	if _, err := corpus.Ingest(ctx, futureSource); err != nil {
		t.Fatalf("rejected session damaged later ingestion: %v", err)
	}
}

func TestRecordInteractionExactRetryIsIdempotent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	corpus, err := explorer.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	session := interaction.Session{
		ID:         interaction.DerivedID("session", "idempotent"),
		RecordedAt: time.Unix(1700000000, 0).UTC(),
		Operation:  interaction.OperationRetrieval,
	}
	if err := corpus.RecordInteraction(ctx, session); err != nil {
		t.Fatal(err)
	}
	if err := corpus.RecordInteraction(ctx, session); err != nil {
		t.Fatalf("exact retry failed: %v", err)
	}
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}
	corpus, err = explorer.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	if err := corpus.RecordInteraction(ctx, session); err != nil {
		t.Fatalf("exact retry after restart failed: %v", err)
	}
	different := session
	different.StopReason = "different"
	if err := corpus.RecordInteraction(
		ctx, different,
	); !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("conflicting retry error = %v", err)
	}
}

func TestRecorderExactRetryReplaysAcceptedTimestamp(t *testing.T) {
	ctx := context.Background()
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	recorder, err := interaction.NewRecorder(ctx, corpus)
	if err != nil {
		t.Fatal(err)
	}
	firstTime := time.Unix(1700000000, 0).UTC()
	current := firstTime
	if err := recorder.SetClock(func() time.Time { return current }); err != nil {
		t.Fatal(err)
	}
	session := interaction.Session{
		ID:         interaction.DerivedID("session", "trusted-time-retry"),
		RecordedAt: firstTime.Add(-24 * time.Hour),
		Operation:  interaction.OperationToolCall,
		Turns: []interaction.Turn{{
			Index: 0, Decision: "tool_call",
			ToolCall: &interaction.ToolCall{Kind: "registry_create"},
		}},
	}
	first, err := recorder.Record(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	if !first.RecordedAt.Equal(firstTime) {
		t.Fatalf("first accepted time = %v", first.RecordedAt)
	}
	current = firstTime.Add(time.Minute)
	session.RecordedAt = firstTime.Add(24 * time.Hour)
	retried, err := recorder.Record(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	if !retried.RecordedAt.Equal(firstTime) {
		t.Fatalf("retry minted time %v, want %v",
			retried.RecordedAt, firstTime)
	}
	records, err := corpus.InteractionRecords(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 ||
		!records[0].Session.RecordedAt.Equal(firstTime) {
		t.Fatalf("durable retry records = %+v", records)
	}
}

func TestOversizedVisibilityPersistsAcrossRestart(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	corpus, err := explorer.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	const count = 70
	var spans []shoal.ID
	labels := make([]string, count)
	for index := 0; index < count; index++ {
		label := fmt.Sprintf(
			"label-%03d-%s", index, strings.Repeat("x", 56))
		labels[index] = label
		visible := ingestVisible(
			t,
			corpus,
			fmt.Sprintf("file:///visibility-%03d.md", index),
			fmt.Sprintf("# Source %d\n\nvalue %d\n", index, index),
			label,
		)
		spans = append(spans, visible[0])
	}
	expected := strings.Join(labels, "&")
	if len(expected) <= shoal.MaxMetadataValueBytes {
		t.Fatal("fixture did not exceed the graph metadata value bound")
	}
	recordedSession(
		t, corpus, "interaction.session_oversized-visibility", spans, spans[len(spans)-1:])
	subgraph, err := corpus.InteractionSubgraph(
		ctx, "interaction.session_oversized-visibility")
	if err != nil {
		t.Fatal(err)
	}
	var sessionNode graph.Node
	for _, node := range subgraph.Nodes {
		if node.Kind == interaction.KindSession {
			sessionNode = node
			break
		}
	}
	if sessionNode.Properties[interaction.PropertyVisibility] != "" ||
		sessionNode.Properties[interaction.PropertyVisibilityDigest] !=
			interaction.Digest(expected) ||
		sessionNode.Properties[interaction.PropertyVisibilityCount] !=
			strconv.Itoa(count) {
		t.Fatalf("oversized visibility markers = %+v", sessionNode.Properties)
	}
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := explorer.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	summaries, err := reopened.Interactions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 {
		t.Fatalf("reopened summaries = %+v", summaries)
	}
	if summaries[0].Visibility != expected {
		t.Fatalf("reopened visibility length=%d, want %d",
			len(summaries[0].Visibility), len(expected))
	}
}

// TestInteractionVisibilityIgnoresAskerGrants pins that visibility is derived
// from touched spans only. A session that touches nothing but declares rich
// provenance carries no labels at all.
func TestInteractionVisibilityIgnoresAskerGrants(t *testing.T) {
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	spans := ingestVisible(
		t, corpus, "file:///runbook.md", publicMarkdown, "ops")

	recordedSession(t, corpus, "interaction.session_open", spans[:1], spans[:1])
	summaries, err := corpus.Interactions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if summaries[0].Visibility != "ops" {
		t.Fatalf("visibility = %q, want %q", summaries[0].Visibility, "ops")
	}
}

// TestInteractionVisibilityFailsClosedOnUnknownNode pins that an interaction
// touching a node whose visibility cannot be resolved is refused rather than
// recorded with an empty (public) label set.
func TestInteractionVisibilityFailsClosedOnUnknownNode(t *testing.T) {
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	ingestVisible(t, corpus, "file:///runbook.md", publicMarkdown, "ops")

	err = corpus.RecordInteraction(context.Background(), interaction.Session{
		ID:          "interaction.session_unknown",
		RecordedAt:  time.Unix(1700000000, 0).UTC(),
		SeedNodeIDs: []shoal.ID{"no-such-node"},
	})
	if err == nil {
		t.Fatal("recording an interaction touching an unknown node succeeded")
	}
	if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("error code = %v", err)
	}
}

// TestInteractionNodesExcludedFromRetrieval pins binding decision 1: a model
// must never cite its own prior output as source evidence, so interaction
// nodes never appear in default retrieval results.
func TestInteractionNodesExcludedFromRetrieval(t *testing.T) {
	ctx := context.Background()
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	spans := ingestVisible(t, corpus, "file:///runbook.md", publicMarkdown, "")
	recordedSession(t, corpus, "interaction.session_retrieval", spans[:1], spans[:1])

	response, err := corpus.Retrieve(ctx, retrieval.Request{
		Text:  "exponential backoff",
		TopK:  50,
		Modes: []retrieval.Mode{retrieval.ModeLexical},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) == 0 {
		t.Fatal("retrieval returned no results at all")
	}
	for _, result := range response.Results {
		if strings.HasPrefix(string(result.ID), interaction.KindPrefix) {
			t.Fatalf("retrieval returned an interaction result: %+v", result)
		}
		for _, evidence := range result.Evidence {
			assertNoInteractionNodes(
				t, "Retrieve", evidence.Path.Nodes, evidence.Path.Edges)
		}
	}
}

// TestInteractionNodesExcludedFromExpansion pins that graph expansion from a
// cited span never crosses into an interaction node. This is the real leak
// vector: interaction nodes point at the spans they touched.
func TestInteractionNodesExcludedFromExpansion(t *testing.T) {
	ctx := context.Background()
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	spans := ingestVisible(t, corpus, "file:///runbook.md", publicMarkdown, "")
	recordedSession(t, corpus, "interaction.session_expansion", spans[:1], spans[:1])

	neighborhood, err := corpus.Neighborhood(ctx, explorer.NeighborhoodRequest{
		NodeIDs: []shoal.ID{spans[0]},
		Depth:   16,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertNoInteractionNodes(t, "Neighborhood", neighborhood.Nodes, neighborhood.Edges)

	bounded, err := corpus.BoundedNeighborhood(
		ctx, explorer.BoundedNeighborhoodRequest{
			NodeIDs:  []shoal.ID{spans[0]},
			Depth:    16,
			Fanout:   1024,
			MaxNodes: 1024,
		})
	if err != nil {
		t.Fatal(err)
	}
	assertNoInteractionNodes(
		t, "BoundedNeighborhood",
		bounded.Neighborhood.Nodes, bounded.Neighborhood.Edges)

	// The subgraph is still reachable by explicit, kind-scoped traversal.
	explicit, err := corpus.InteractionSubgraph(ctx, "interaction.session_expansion")
	if err != nil {
		t.Fatal(err)
	}
	if len(explicit.Nodes) == 0 {
		t.Fatal("explicit interaction traversal returned nothing")
	}
}

func assertNoInteractionNodes(
	t *testing.T, label string, nodes []graph.Node, edges []graph.Edge,
) {
	t.Helper()
	for _, node := range nodes {
		if interaction.IsInteractionKind(node.Kind) {
			t.Fatalf("%s leaked interaction node %q (%s)",
				label, node.ID, node.Kind)
		}
	}
	for _, edge := range edges {
		if interaction.IsInteractionEdgeType(edge.Type) {
			t.Fatalf("%s leaked interaction edge %q", label, edge.Type)
		}
	}
}

// TestInteractionDeletionLeavesTombstone pins binding decision 5: retention is
// explicit deletion only, and the deletion is itself auditable.
func TestInteractionDeletionLeavesTombstone(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	corpus, err := explorer.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	spans := ingestVisible(
		t, corpus, "file:///incident.md", restrictedMarkdown, "secret")
	recordedSession(t, corpus, "interaction.session_deleted", spans[:1], spans[:1])

	tombstone, err := corpus.DeleteInteraction(ctx, "interaction.session_deleted")
	if err != nil {
		t.Fatal(err)
	}
	if tombstone.NodeCount == 0 || tombstone.DeletedAt.IsZero() {
		t.Fatalf("tombstone = %+v", tombstone)
	}
	if interaction.Expression(tombstone.Visibility) != "secret" {
		t.Fatalf("tombstone visibility = %v", tombstone.Visibility)
	}

	assertTombstoneOnly := func(label string, c *explorer.Explorer) {
		t.Helper()
		sub, err := c.InteractionSubgraph(ctx, "interaction.session_deleted")
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if len(sub.Nodes) != 1 ||
			sub.Nodes[0].Kind != interaction.KindTombstone {
			t.Fatalf("%s: subgraph after deletion = %+v", label, sub.Nodes)
		}
		if len(sub.Edges) != 0 {
			t.Fatalf("%s: edges survived deletion: %+v", label, sub.Edges)
		}
		if got := sub.Nodes[0].Properties[interaction.PropertyVisibility]; got !=
			"secret" {
			t.Fatalf("%s: tombstone node visibility = %q", label, got)
		}
	}
	assertTombstoneOnly("live", corpus)

	// Deleting twice is refused, and the ID cannot be reused.
	if _, err := corpus.DeleteInteraction(ctx, "interaction.session_deleted"); err == nil {
		t.Fatal("second deletion succeeded")
	}
	err = corpus.RecordInteraction(ctx, interaction.Session{
		ID:          "interaction.session_deleted",
		RecordedAt:  time.Unix(1700000001, 0).UTC(),
		SeedNodeIDs: spans[:1],
	})
	if !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("reusing a deleted session ID = %v", err)
	}
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := explorer.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	assertTombstoneOnly("reopened", reopened)
}

// TestReadOnlyCorpusRejectsInteractionSink pins binding decision 4 at the
// storage layer: a read-only corpus refuses at setup, before any inference
// work, rather than at first write.
func TestReadOnlyCorpusRejectsInteractionSink(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	corpus, err := explorer.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	spans := ingestVisible(t, corpus, "file:///runbook.md", publicMarkdown, "")
	if err := corpus.EnsureInteractionSink(ctx); err != nil {
		t.Fatalf("writable corpus rejected the interaction sink: %v", err)
	}
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}

	readOnly, err := explorer.OpenWithOptions(
		dataDir, explorer.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()

	err = readOnly.EnsureInteractionSink(ctx)
	if err == nil {
		t.Fatal("read-only corpus accepted an interaction sink")
	}
	if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("error code = %v", err)
	}
	if !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("diagnostic is not clear about read-only: %v", err)
	}

	err = readOnly.RecordInteraction(ctx, interaction.Session{
		ID:          "interaction.session_read-only",
		RecordedAt:  time.Unix(1700000000, 0).UTC(),
		SeedNodeIDs: spans[:1],
	})
	if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("read-only record = %v", err)
	}
	if _, err := readOnly.DeleteInteraction(
		ctx, "interaction.session_read-only",
	); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("read-only delete = %v", err)
	}
	if _, err := readOnly.Ingest(ctx, explorer.Source{
		URI:       "file:///new.md",
		MediaType: explorer.MediaTypeMarkdown,
		Content:   publicMarkdown,
	}); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("read-only ingest = %v", err)
	}
}

// TestIngestRejectsReservedInteractionKinds pins that the interaction.*
// namespace is reserved: content may not claim it.
func TestIngestRejectsReservedInteractionKinds(t *testing.T) {
	ctx := context.Background()
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	spans := ingestVisible(t, corpus, "file:///runbook.md", publicMarkdown, "")
	recordedSession(t, corpus, "interaction.session_connect", spans[:1], spans[:1])

	err = corpus.Connect(ctx, graph.Edge{
		ID:   "edge-reserved",
		From: spans[0],
		To:   spans[0],
		Type: interaction.EdgeCited,
	})
	if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("reserved edge type accepted: %v", err)
	}
	err = corpus.Connect(ctx, graph.Edge{
		ID:   "edge-into-interaction",
		From: spans[0],
		To:   "interaction.session_connect",
		Type: "mentions",
	})
	if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("edge into an interaction node accepted: %v", err)
	}
}
