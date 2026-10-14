// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied. See the License for the
// specific language governing permissions and limitations
// under the License.

package authorized_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/analytics"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type countingDocumentsAnalyticsBase struct {
	*explorer.Explorer
	calls         int
	snapshotCalls int
}

type omittingAssertionsAnalyticsBase struct {
	*explorer.Explorer
}

func (b *countingDocumentsAnalyticsBase) Documents(
	ctx context.Context,
) ([]explorer.DocumentSummary, error) {
	b.calls++
	return b.Explorer.Documents(ctx)
}

func (b *countingDocumentsAnalyticsBase) Snapshot(
	ctx context.Context,
) (explorer.Snapshot, error) {
	b.snapshotCalls++
	return b.Explorer.Snapshot(ctx)
}

func (b *omittingAssertionsAnalyticsBase) BoundedNeighborhood(
	ctx context.Context,
	request explorer.BoundedNeighborhoodRequest,
) (explorer.BoundedNeighborhood, error) {
	result, err := b.Explorer.BoundedNeighborhood(ctx, request)
	if err == nil {
		result.Neighborhood.Assertions = nil
	}
	return result, err
}

func TestAnalyticsRecorderReauthorizesExtractedRelationshipEvidence(t *testing.T) {
	f := newFixture(t)
	version := authorizedSkillsOntologyVersion(t)
	document := ingestAuthorizedSkill(
		t, f.clientA, f.admin(t), "analytics", "derived-cli")
	extracted, err := f.clientA.ExtractDocument(
		f.alice(t),
		explorer.ExtractionRequest{
			DocumentID: document.Document.ID,
			RevisionID: document.Revision.ID,
			Version:    version,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(extracted.RelationshipEdgeIDs) == 0 {
		t.Fatal("extraction produced no relationship evidence")
	}
	var relationship graph.Edge
	for _, edge := range extracted.GraphEdges {
		for _, edgeID := range extracted.RelationshipEdgeIDs {
			if edge.ID == edgeID {
				relationship = edge
				break
			}
		}
		if relationship.ID != "" {
			break
		}
	}
	if relationship.ID == "" {
		t.Fatal("extraction relationship edge was not materialized")
	}
	omitting := &omittingAssertionsAnalyticsBase{Explorer: f.base}
	omittingClient := f.newClient(
		t, omitting, f.store, f.sourceA, f.policyA, nil)
	omittingService, err := analytics.NewService(analytics.Config{
		Source: omittingClient, Limits: analytics.DefaultLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	omittingContext := f.context(t, f.decision(
		t,
		"analytics-reader",
		[][]byte{f.sourceA},
		[][]byte{f.policyA},
		[]auth.Operation{auth.OperationAnalyticsRead, auth.OperationRetrieve},
	))
	_, err = omittingService.Run(omittingContext, analytics.Request{
		Scope: analytics.Scope{
			NodeIDs: []shoal.ID{relationship.From},
			Depth:   1, Direction: explorer.GraphDirectionBoth,
			Fanout: 10, MaxNodes: 50, MaxEdges: 50,
			MaxScannedEdgesPerNode: 1024,
			EdgeTypes:              []string{relationship.Type},
		},
	})
	if !shoal.IsErrorCode(err, shoal.ErrorInternal) {
		t.Fatalf("omitted relationship assertion error = %v", err)
	}
	counted := &countingDocumentsAnalyticsBase{Explorer: f.base}
	client := f.newClient(
		t, counted, f.store, f.sourceA, f.policyA, nil)
	snapshot, err := f.base.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Set(snapshot.AsOf.Add(time.Second))
	decision := f.decision(
		t,
		"analytics-reader",
		[][]byte{f.sourceA},
		[][]byte{f.policyA},
		[]auth.Operation{auth.OperationAnalyticsRead, auth.OperationRetrieve},
	)
	ctx := f.context(t, decision)
	sink := client.AnalyticsInteractionSink()
	if sink == nil {
		t.Fatal("analytics interaction sink is unavailable")
	}
	shared, err := interaction.NewRecorder(context.Background(), sink)
	if err != nil {
		t.Fatal(err)
	}
	recorder, err := analytics.NewInteractionRecorder(shared, f.clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	service, err := analytics.NewService(analytics.Config{
		Source: client, Limits: analytics.DefaultLimits(),
		Recorder: recorder, RequireRecording: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Run(ctx, analytics.Request{
		Scope: analytics.Scope{
			NodeIDs: []shoal.ID{relationship.From},
			Depth:   1, Direction: explorer.GraphDirectionBoth,
			Fanout: 10, MaxNodes: 50, MaxEdges: 50,
			MaxScannedEdgesPerNode: 1024,
			EdgeTypes:              []string{relationship.Type},
		},
	})
	if err != nil {
		t.Fatal(interactionErrorChain(err))
	}
	recorded, err := f.base.Interaction(
		context.Background(), result.Recording.InteractionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(recorded.Turns) != 1 || recorded.Turns[0].ToolCall == nil ||
		len(recorded.Turns[0].ToolCall.RetrievedEvidence) == 0 ||
		len(recorded.TouchedEdgeIDs()) == 0 {
		t.Fatalf("recorded extracted evidence = %+v", recorded)
	}
	if len(recorded.TouchedEdgeIDs()) != int(result.Scope.EdgeCount) {
		t.Fatalf("recorded edge IDs = %d, result edge count = %d",
			len(recorded.TouchedEdgeIDs()), result.Scope.EdgeCount)
	}
	assertionCount := 0
	for _, evidence := range recorded.Turns[0].ToolCall.RetrievedEvidence {
		assertionCount += len(evidence.Assertions)
	}
	if assertionCount == 0 {
		t.Fatal("recorded analytics evidence omitted extracted assertions")
	}
	if counted.calls != 1 {
		t.Fatalf("analytics documents scans = %d, want 1", counted.calls)
	}
	if counted.snapshotCalls != 4 {
		t.Fatalf(
			"analytics and recording snapshot calls = %d, want 4",
			counted.snapshotCalls,
		)
	}
	readDecision := f.decision(
		t,
		"analytics-reader",
		[][]byte{f.sourceA},
		[][]byte{f.policyA},
		[]auth.Operation{auth.OperationRead},
	)
	readContext := f.context(t, readDecision)
	counted.calls = 0
	if _, err := client.InteractionRecords(readContext); err != nil {
		t.Fatal(err)
	}
	if counted.calls != 0 {
		t.Fatalf("analytics list documents scans = %d, want 0", counted.calls)
	}
	counted.calls = 0
	if _, err := client.Interaction(
		readContext, result.Recording.InteractionID,
	); err != nil {
		t.Fatal(err)
	}
	if counted.calls != 0 {
		t.Fatalf("analytics point documents scans = %d, want 0", counted.calls)
	}
}

func TestAnalyticsForgedSinkResultIsIndeterminate(t *testing.T) {
	f := newFixture(t)
	document := ingestAuthorizedSkill(
		t, f.clientA, f.admin(t), "analytics-indeterminate", "bounded")
	view, err := f.clientA.Document(
		f.alice(t), document.Document.ID, document.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.base.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Set(snapshot.AsOf.Add(time.Second))
	wrapped := &forgedResultInteractionBase{Explorer: f.base}
	client := f.newClient(
		t, wrapped, f.store, f.sourceA, f.policyA, nil)
	decision := f.decision(
		t,
		"analytics-indeterminate",
		[][]byte{f.sourceA},
		[][]byte{f.policyA},
		[]auth.Operation{auth.OperationAnalyticsRead, auth.OperationRetrieve},
	)
	ctx := f.context(t, decision)
	shared, err := interaction.NewRecorder(
		context.Background(), client.AnalyticsInteractionSink())
	if err != nil {
		t.Fatal(err)
	}
	recorder, err := analytics.NewInteractionRecorder(shared, f.clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	service, err := analytics.NewService(analytics.Config{
		Source: client, Limits: analytics.DefaultLimits(),
		Recorder: recorder, RequireRecording: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Run(ctx, analytics.Request{
		Scope: analytics.Scope{
			NodeIDs: []shoal.ID{firstSpanID(t, view)},
			Depth:   1, Direction: explorer.GraphDirectionBoth,
			Fanout: 4, MaxNodes: 8, MaxEdges: 8,
			MaxScannedEdgesPerNode: 16,
		},
	})
	if !explorer.IsIndeterminateCommit(err) {
		t.Fatalf("forged analytics result error = %v", err)
	}
}

func TestAnalyticsCommittedSinkFailureIsIndeterminate(t *testing.T) {
	f := newFixture(t)
	document := ingestAuthorizedSkill(
		t, f.clientA, f.admin(t), "analytics-committed", "bounded")
	view, err := f.clientA.Document(
		f.alice(t), document.Document.ID, document.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.base.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Set(snapshot.AsOf.Add(time.Second))
	wrapped := &analyticsCommittedFailureInteractionBase{Explorer: f.base}
	client := f.newClient(
		t, wrapped, f.store, f.sourceA, f.policyA, nil)
	decision := f.decision(
		t,
		"analytics-committed",
		[][]byte{f.sourceA},
		[][]byte{f.policyA},
		[]auth.Operation{auth.OperationAnalyticsRead, auth.OperationRetrieve},
	)
	ctx := f.context(t, decision)
	shared, err := interaction.NewRecorder(
		context.Background(), client.AnalyticsInteractionSink())
	if err != nil {
		t.Fatal(err)
	}
	recorder, err := analytics.NewInteractionRecorder(shared, f.clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	service, err := analytics.NewService(analytics.Config{
		Source: client, Limits: analytics.DefaultLimits(),
		Recorder: recorder, RequireRecording: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Run(ctx, analytics.Request{
		Scope: analytics.Scope{
			NodeIDs: []shoal.ID{firstSpanID(t, view)},
			Depth:   1, Direction: explorer.GraphDirectionBoth,
			Fanout: 4, MaxNodes: 8, MaxEdges: 8,
			MaxScannedEdgesPerNode: 16,
		},
	})
	if !explorer.IsIndeterminateCommit(err) {
		t.Fatalf("committed analytics sink error = %v", err)
	}
}

type analyticsCommittedFailureInteractionBase struct {
	*explorer.Explorer
}

func (b *analyticsCommittedFailureInteractionBase) RecordInteractionResult(
	ctx context.Context, session interaction.Session,
) (interaction.Session, error) {
	recorded, err := b.Explorer.RecordInteractionResult(ctx, session)
	if err != nil {
		return recorded, err
	}
	return recorded, explorer.MarkCommittedInteraction(
		shoal.NewError(shoal.ErrorUnavailable, "post-commit failure"))
}

func interactionErrorChain(err error) string {
	var values []string
	for current := err; current != nil; current = errors.Unwrap(current) {
		values = append(values, current.Error())
	}
	return fmt.Sprint(values)
}
