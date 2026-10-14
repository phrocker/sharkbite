// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file distributed
// with this work for additional information regarding copyright ownership.
package teamoverview_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/explorer/teamoverview"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestOverviewSharesAuthorizedTeamAndSuppressesUnrelatedEvidence(t *testing.T) {
	now := time.Date(2026, 9, 7, 16, 0, 0, 0, time.UTC)
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	source := &overviewFixture{now: now}
	service, err := teamoverview.NewService(teamoverview.Config{
		Graph: source, Agents: source, Actions: source, Interactions: source,
		Resolver: authority.Resolver(), Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	request := teamoverview.Request{
		TeamID: "team-1", SourceID: []byte("shared-source"),
		PolicyID: []byte("shared-policy"), HistoryDays: 14, Limit: 2,
	}
	alice := bindOverviewDecision(
		t, authority, overviewDecision(t, now, "alice", "alice-request",
			[]byte("shared-source"), []byte("shared-policy")))
	bob := bindOverviewDecision(
		t, authority, overviewDecision(t, now, "bob", "bob-request",
			[]byte("shared-source"), []byte("shared-policy")))
	aliceResponse, err := service.Overview(alice, request)
	if err != nil {
		t.Fatal(err)
	}
	bobResponse, err := service.Overview(bob, request)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(aliceResponse, bobResponse) {
		t.Fatalf("shared team responses differ:\nalice=%#v\nbob=%#v",
			aliceResponse, bobResponse)
	}
	if aliceResponse.Team.ID != "team-1" ||
		len(aliceResponse.People) != 2 ||
		len(aliceResponse.Agents) != 1 ||
		!aliceResponse.Agents[0].Active ||
		len(aliceResponse.WorkItems) != 3 {
		t.Fatalf("roster = %#v", aliceResponse)
	}
	if got := aliceResponse.Metrics.ActionStates; got.Queued != 1 ||
		got.Succeeded != 2 || got.Failed != 2 {
		t.Fatalf("action states = %#v", got)
	}
	if aliceResponse.Metrics.Queue.AgedCount != 1 ||
		aliceResponse.Metrics.Queue.MedianAgeSeconds != int64((48*time.Hour)/time.Second) ||
		aliceResponse.Metrics.MedianCycleTimeSeconds != int64((150*time.Minute)/time.Second) {
		t.Fatalf("metrics = %#v", aliceResponse.Metrics)
	}
	if aliceResponse.Metrics.Completed != 2 ||
		aliceResponse.Metrics.Failed != 2 ||
		len(aliceResponse.Metrics.Daily) != 14 {
		t.Fatalf("window metrics = %#v", aliceResponse.Metrics)
	}
	if aliceResponse.Metrics.ActiveAgents != 1 {
		t.Fatalf("active agents = %d", aliceResponse.Metrics.ActiveAgents)
	}
	if len(aliceResponse.Activities) != 2 ||
		aliceResponse.NextCursor == "" {
		t.Fatalf("first page = %#v", aliceResponse.Activities)
	}
	activities := append([]teamoverview.Activity(nil), aliceResponse.Activities...)
	next := aliceResponse.NextCursor
	for next != "" {
		nextRequest := request
		nextRequest.Cursor = next
		page, pageErr := service.Overview(alice, nextRequest)
		if pageErr != nil {
			t.Fatal(pageErr)
		}
		activities = append(activities, page.Activities...)
		next = page.NextCursor
	}
	seen := make(map[string]bool)
	for _, activity := range activities {
		key := activity.Kind + "/" + activity.ID
		if seen[key] {
			t.Fatalf("duplicate paged activity %q", key)
		}
		seen[key] = true
		for _, evidence := range activity.Evidence {
			if evidence.ID == "hidden-node" ||
				evidence.ID == "hidden-interaction" ||
				evidence.ID == "aGlkZGVuLWFjdGlvbg" {
				t.Fatalf("unauthorized evidence leaked: %#v", evidence)
			}
		}
	}
	wantLabels := map[string]bool{
		"aged_queue": true, "repeated_blocks": true,
		"high_failure_rate": true, "concentrated_assignments": true,
		"repeated_manual_operation": true,
	}
	for _, insight := range aliceResponse.Insights {
		if !insight.Heuristic {
			t.Fatalf("insight is not labeled heuristic: %#v", insight)
		}
		delete(wantLabels, insight.Label)
	}
	if len(wantLabels) != 0 {
		t.Fatalf("missing heuristic labels: %#v", wantLabels)
	}
}

func TestOverviewDeniesTeamBeforeReadingSources(t *testing.T) {
	now := time.Date(2026, 9, 7, 16, 0, 0, 0, time.UTC)
	authority, _ := auth.NewAuthorityWithClock(func() time.Time { return now })
	source := &overviewFixture{now: now}
	service, _ := teamoverview.NewService(teamoverview.Config{
		Graph: source, Agents: source, Actions: source, Interactions: source,
		Resolver: authority.Resolver(), Clock: func() time.Time { return now },
	})
	ctx := bindOverviewDecision(
		t, authority, overviewDecision(t, now, "mallory", "mallory-request",
			[]byte("other-source"), []byte("other-policy")))
	_, err := service.Overview(ctx, teamoverview.Request{
		TeamID: "team-1", SourceID: []byte("shared-source"),
		PolicyID: []byte("shared-policy"),
	})
	if !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("Overview() error = %v, want non-disclosing not_found", err)
	}
	if source.graphCalls != 0 {
		t.Fatalf("unauthorized request reached graph source %d times",
			source.graphCalls)
	}
}

type overviewFixture struct {
	now        time.Time
	graphCalls int
}

func (f *overviewFixture) BoundedNeighborhood(
	context.Context, explorer.BoundedNeighborhoodRequest,
) (explorer.BoundedNeighborhood, error) {
	f.graphCalls++
	nodes := []graph.Node{
		{ID: "team-1", Kind: teamoverview.KindTeam, Properties: shoal.Metadata{"name": "Demo Team"}},
		{ID: "person-alice", Kind: teamoverview.KindPerson, Properties: shoal.Metadata{"name": "Alice", "subject_id": "alice"}},
		{ID: "person-bob", Kind: teamoverview.KindPerson, Properties: shoal.Metadata{"name": "Bob", "subject_id": "bob"}},
		{ID: "agent-node", Kind: teamoverview.KindAgent, Properties: shoal.Metadata{"name": "Build Agent", "agent_id": "agent-1"}},
		{ID: "work-1", Kind: teamoverview.KindWorkItem, Properties: shoal.Metadata{"title": "Release", "status": "active"}},
		{ID: "work-2", Kind: teamoverview.KindWorkItem, Properties: shoal.Metadata{"title": "Tests", "status": "blocked"}},
		{ID: "work-3", Kind: teamoverview.KindWorkItem, Properties: shoal.Metadata{"title": "Docs", "status": "blocked"}},
		{ID: "activity-1", Kind: teamoverview.KindActivity, Properties: shoal.Metadata{
			"recorded_at": f.now.Add(-time.Hour).Format(time.RFC3339Nano),
			"actor_id":    "alice", "operation": "triage", "manual": "true",
		}},
		{ID: "activity-2", Kind: teamoverview.KindActivity, Properties: shoal.Metadata{
			"recorded_at": f.now.Add(-2 * time.Hour).Format(time.RFC3339Nano),
			"actor_id":    "alice", "operation": "triage", "manual": "true",
		}},
		{ID: "activity-3", Kind: teamoverview.KindActivity, Properties: shoal.Metadata{
			"recorded_at": f.now.Add(-3 * time.Hour).Format(time.RFC3339Nano),
			"actor_id":    "alice", "operation": "triage", "manual": "true",
		}},
	}
	edges := []graph.Edge{
		{ID: "member-alice", From: "person-alice", To: "team-1", Type: teamoverview.RelationMemberOf},
		{ID: "member-bob", From: "person-bob", To: "team-1", Type: teamoverview.RelationMemberOf},
		{ID: "member-agent", From: "agent-node", To: "team-1", Type: teamoverview.RelationMemberOf},
		{ID: "member-work-1", From: "work-1", To: "team-1", Type: teamoverview.RelationMemberOf},
		{ID: "member-work-2", From: "work-2", To: "team-1", Type: teamoverview.RelationMemberOf},
		{ID: "member-work-3", From: "work-3", To: "team-1", Type: teamoverview.RelationMemberOf},
		{ID: "member-activity-1", From: "activity-1", To: "team-1", Type: teamoverview.RelationMemberOf},
		{ID: "member-activity-2", From: "activity-2", To: "team-1", Type: teamoverview.RelationMemberOf},
		{ID: "member-activity-3", From: "activity-3", To: "team-1", Type: teamoverview.RelationMemberOf},
		{ID: "assign-1", From: "work-1", To: "person-alice", Type: teamoverview.RelationAssignedTo},
		{ID: "assign-2", From: "work-2", To: "person-alice", Type: teamoverview.RelationAssignedTo},
		{ID: "assign-3", From: "work-3", To: "person-alice", Type: teamoverview.RelationAssignedTo},
		{ID: "block-2", From: "work-2", To: "work-1", Type: teamoverview.RelationBlockedBy},
		{ID: "block-3", From: "work-3", To: "work-1", Type: teamoverview.RelationBlockedBy},
	}
	return explorer.BoundedNeighborhood{
		Neighborhood: explorer.Neighborhood{Nodes: nodes, Edges: edges},
	}, nil
}

func (f *overviewFixture) List(
	context.Context, fleet.ListRequest,
) (fleet.ListPage, error) {
	return fleet.ListPage{Descriptors: []fleet.Descriptor{{
		ID: "agent-1", Generation: 2,
		LeaseExpiresAt: f.now.Add(time.Hour),
	}}}, nil
}

func (f *overviewFixture) TeamActions(
	_ context.Context, request fleet.TeamActionListRequest,
) (fleet.ActionPage, error) {
	action := func(
		id string, state fleet.DispatchState, created, updated time.Time,
	) fleet.ActionRecord {
		return fleet.ActionRecord{
			ID: []byte(id), State: state, AgentID: "agent-1",
			Capability: "work", Action: "process",
			SourceID: []byte("shared-source"), PolicyID: []byte("shared-policy"),
			ObjectID: "work-1", Actor: "agent-1",
			CreatedAt: created, UpdatedAt: updated,
		}
	}
	return fleet.ActionPage{Actions: []fleet.ActionRecord{
		action("queued", fleet.DispatchQueued, f.now.Add(-48*time.Hour), f.now.Add(-48*time.Hour)),
		action("success-1", fleet.DispatchSucceeded, f.now.Add(-5*time.Hour), f.now.Add(-time.Hour)),
		action("success-2", fleet.DispatchSucceeded, f.now.Add(-4*time.Hour), f.now.Add(-2*time.Hour)),
		action("failure-1", fleet.DispatchFailed, f.now.Add(-4*time.Hour), f.now.Add(-time.Hour)),
		action("failure-2", fleet.DispatchFailed, f.now.Add(-3*time.Hour), f.now.Add(-time.Hour)),
		{
			ID: []byte("hidden-action"), State: fleet.DispatchFailed,
			AgentID: "agent-1", SourceID: []byte("hidden-source"),
			PolicyID: []byte("hidden-policy"), ObjectID: "work-1",
			CreatedAt: f.now.Add(-time.Hour), UpdatedAt: f.now,
		},
	}}, nil
}

func (f *overviewFixture) InteractionRecordsPage(
	context.Context, shoal.ID, uint32,
) (explorer.InteractionRecordPage, error) {
	return explorer.InteractionRecordPage{Records: []explorer.InteractionRecord{
		{
			Summary: explorer.InteractionSummary{
				SessionID:  "visible-interaction",
				RecordedAt: f.now.Add(-30 * time.Minute),
				Operation:  interaction.OperationChat,
				Actor: interaction.ActorContext{
					SubjectID: "bob", ActorID: "bob",
				},
			},
			TouchedNodeIDs: []shoal.ID{"work-1"},
		},
		{
			Summary: explorer.InteractionSummary{
				SessionID:  "hidden-interaction",
				RecordedAt: f.now.Add(-15 * time.Minute),
				Operation:  interaction.OperationChat,
				Actor: interaction.ActorContext{
					SubjectID: "mallory", ActorID: "mallory",
				},
			},
			TouchedNodeIDs: []shoal.ID{"hidden-node"},
		},
	}}, nil
}

func overviewDecision(
	t *testing.T, now time.Time, subject string, requestID shoal.ID,
	sourceID, policyID []byte,
) auth.Decision {
	t.Helper()
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: shoal.ID(subject), Actor: shoal.ID(subject),
		AuthorizationDomain: []byte("domain"),
		AllowedOperations: []auth.Operation{
			auth.OperationTeamOverviewRead, auth.OperationNeighborhood,
			auth.OperationAgentResolve, auth.OperationRead,
		},
		PermittedSourceIDs: [][]byte{sourceID},
		PermittedPolicyIDs: [][]byte{policyID},
		PolicyGeneration:   1, AuthenticationExpires: now.Add(time.Hour),
		RequestID: requestID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return decision
}

func bindOverviewDecision(
	t *testing.T, authority *auth.Authority, decision auth.Decision,
) context.Context {
	t.Helper()
	ctx, err := authority.Binder().Bind(context.Background(), decision)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}
