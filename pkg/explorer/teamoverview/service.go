// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file distributed
// with this work for additional information regarding copyright ownership.
package teamoverview

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	agedQueueThreshold       = 24 * time.Hour
	repeatedBlockThreshold   = 2
	highFailureMinimum       = 4
	highFailureRate          = 0.25
	concentratedMinimumItems = 3
	automationThreshold      = 3
)

type GraphSource interface {
	BoundedNeighborhood(
		context.Context, explorer.BoundedNeighborhoodRequest,
	) (explorer.BoundedNeighborhood, error)
}

type AgentSource interface {
	List(context.Context, fleet.ListRequest) (fleet.ListPage, error)
}

type ActionSource interface {
	TeamActions(
		context.Context, fleet.TeamActionListRequest,
	) (fleet.ActionPage, error)
}

type InteractionSource interface {
	InteractionRecordsPage(
		context.Context, shoal.ID, uint32,
	) (explorer.InteractionRecordPage, error)
}

type Config struct {
	Graph        GraphSource
	Agents       AgentSource
	Actions      ActionSource
	Interactions InteractionSource
	Resolver     auth.Resolver
	Clock        func() time.Time
}

type Service struct {
	graph        GraphSource
	agents       AgentSource
	actions      ActionSource
	interactions InteractionSource
	resolver     auth.Resolver
	clock        func() time.Time
}

func NewService(config Config) (*Service, error) {
	if absent(config.Graph) || absent(config.Agents) || absent(config.Actions) ||
		absent(config.Interactions) || absent(config.Resolver) ||
		config.Clock == nil {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"team overview dependencies are required",
		)
	}
	return &Service{
		graph: config.Graph, agents: config.Agents, actions: config.Actions,
		interactions: config.Interactions, resolver: config.Resolver,
		clock: config.Clock,
	}, nil
}

func absent(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func (s *Service) Overview(
	ctx context.Context, request Request,
) (Response, error) {
	now := s.clock().UTC()
	normalized, cursor, err := normalizeRequest(request, now)
	if err != nil {
		return Response{}, err
	}
	decision, err := s.resolver.Resolve(ctx)
	if err != nil {
		return Response{}, err
	}
	asOf := normalizedAsOf(cursor, now)
	if err := decision.AuthorizeObject(
		auth.OperationTeamOverviewRead,
		auth.ResourceRequest{
			AuthorizationDomain: decision.AuthorizationDomain(),
			SourceID:            normalized.SourceID, PolicyID: normalized.PolicyID,
			ObjectID: normalized.TeamID,
		},
		now,
	); err != nil {
		return Response{}, err
	}
	since := dayStart(asOf).AddDate(
		0, 0, -int(normalized.HistoryDays-1))
	neighborhood, err := s.graph.BoundedNeighborhood(
		ctx,
		explorer.BoundedNeighborhoodRequest{
			NodeIDs: []shoal.ID{normalized.TeamID}, Depth: 3,
			Fanout: 64, MaxNodes: MaxGraphNodes,
			MaxScannedEdges: MaxGraphScanned,
			EdgeTypes:       relationTypes, Direction: explorer.GraphDirectionBoth,
		},
	)
	if err != nil {
		return Response{}, err
	}
	roster, err := parseRoster(
		normalized, neighborhood.Neighborhood.Nodes,
		neighborhood.Neighborhood.Edges)
	if err != nil {
		return Response{}, err
	}
	requestContext := fleet.RequestContext{
		RequestID: decision.RequestID(), CorrelationID: decision.CorrelationID(),
		ReasonCode: "team_overview", Deadline: requestDeadline(now, decision),
	}
	descriptors, agentsTruncated, err := s.loadAgents(
		ctx, normalized, requestContext, roster.agentIDs())
	if err != nil {
		return Response{}, err
	}
	records, actionsTruncated, err := s.loadActions(
		ctx, normalized, requestContext, roster.workItemIDs(), roster.agentIDs())
	if err != nil {
		return Response{}, err
	}
	interactions, interactionsTruncated, err := s.loadInteractions(ctx)
	if err != nil {
		return Response{}, err
	}
	records = filterActions(records, normalized, roster)
	interactions = filterInteractions(interactions, roster)
	agents := mergeAgents(roster.agents, descriptors, asOf)
	activities := buildActivities(
		normalized, roster, records, interactions, since, asOf)
	metrics := buildMetrics(records, activities, since, asOf)
	for _, agent := range agents {
		if agent.Active {
			metrics.ActiveAgents++
		}
	}
	insights := buildInsights(roster, records, activities, metrics, since, asOf)
	page, next, err := pageActivities(
		activities, normalized, cursor, asOf)
	if err != nil {
		return Response{}, err
	}
	bounds := Bounds{
		AsOf: asOf, Since: since,
		GraphTruncated:  neighborhood.Truncated,
		AgentsTruncated: agentsTruncated, ActionsTruncated: actionsTruncated,
		InteractionsTruncated: interactionsTruncated,
		MaxGraphNodes:         MaxGraphNodes, MaxGraphEdges: MaxGraphEdges,
		MaxAgents: MaxAgents, MaxActions: MaxActions,
		MaxInteractions: MaxInteractions,
	}
	bounds.Complete = !bounds.GraphTruncated && !bounds.AgentsTruncated &&
		!bounds.ActionsTruncated && !bounds.InteractionsTruncated
	return Response{
		Team: roster.team, People: roster.people, Agents: agents,
		WorkItems: roster.workItems, Metrics: metrics, Activities: page,
		Insights: insights, Bounds: bounds, NextCursor: next,
	}, nil
}

func requestDeadline(now time.Time, decision auth.Decision) time.Time {
	deadline := now.Add(time.Minute)
	if expires := decision.AuthenticationExpires(); expires.Before(deadline) {
		deadline = expires
	}
	return deadline.UTC()
}

type cursorValue struct {
	TeamID      shoal.ID  `json:"team_id"`
	SourceID    string    `json:"source_id"`
	PolicyID    string    `json:"policy_id"`
	HistoryDays uint32    `json:"history_days"`
	AsOf        time.Time `json:"as_of"`
	AfterAt     time.Time `json:"after_at"`
	AfterKind   string    `json:"after_kind"`
	AfterID     string    `json:"after_id"`
}

func normalizeRequest(
	request Request, now time.Time,
) (Request, cursorValue, error) {
	if err := shoal.ValidateRequiredID("team ID", request.TeamID); err != nil {
		return Request{}, cursorValue{}, err
	}
	if len(request.SourceID) == 0 || len(request.PolicyID) == 0 {
		return Request{}, cursorValue{}, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"team source and policy IDs are required",
		)
	}
	if request.HistoryDays == 0 {
		request.HistoryDays = DefaultHistoryDays
	}
	if request.HistoryDays > MaxHistoryDays {
		return Request{}, cursorValue{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "team history window exceeds its bound")
	}
	if request.Limit == 0 {
		request.Limit = DefaultPageSize
	}
	if request.Limit > MaxPageSize {
		return Request{}, cursorValue{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "team activity page exceeds its bound")
	}
	cursor, err := decodeCursor(request.Cursor)
	if err != nil {
		return Request{}, cursorValue{}, err
	}
	if request.Cursor != "" {
		if cursor.TeamID != request.TeamID ||
			cursor.SourceID != encodeBytes(request.SourceID) ||
			cursor.PolicyID != encodeBytes(request.PolicyID) ||
			cursor.HistoryDays != request.HistoryDays ||
			cursor.AsOf.IsZero() || cursor.AsOf.After(now) ||
			cursor.AfterAt.IsZero() || cursor.AfterKind == "" ||
			cursor.AfterID == "" {
			return Request{}, cursorValue{}, shoal.NewError(
				shoal.ErrorInvalidArgument,
				"team activity cursor does not match the request",
			)
		}
	}
	request.SourceID = append([]byte(nil), request.SourceID...)
	request.PolicyID = append([]byte(nil), request.PolicyID...)
	return request, cursor, nil
}

func normalizedAsOf(cursor cursorValue, now time.Time) time.Time {
	if !cursor.AsOf.IsZero() {
		return cursor.AsOf.UTC()
	}
	return now.UTC()
}

type parsedRoster struct {
	team       Team
	people     []Person
	agents     []Agent
	workItems  []WorkItem
	activities []graphActivity
	nodeIDs    map[shoal.ID]struct{}
	edgeIDs    map[shoal.ID]struct{}
}

type graphActivity struct {
	id         shoal.ID
	recordedAt time.Time
	actorID    shoal.ID
	operation  string
	manual     bool
}

func parseRoster(
	request Request, nodes []graph.Node, edges []graph.Edge,
) (parsedRoster, error) {
	if len(nodes) > int(MaxGraphNodes) || len(edges) > int(MaxGraphEdges) {
		return parsedRoster{}, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"team graph response exceeds the overview bound",
		)
	}
	result := parsedRoster{
		nodeIDs: make(map[shoal.ID]struct{}, len(nodes)),
		edgeIDs: make(map[shoal.ID]struct{}, len(edges)),
	}
	nodesByID := make(map[shoal.ID]graph.Node, len(nodes))
	for _, node := range nodes {
		nodesByID[node.ID] = node
	}
	teamNode, ok := nodesByID[request.TeamID]
	if !ok || teamNode.Kind != KindTeam {
		return parsedRoster{}, auth.ObjectNotFound()
	}
	result.team = Team{
		ID: teamNode.ID, Name: firstProperty(
			teamNode, PropertyName, PropertyTitle),
		NodeID: teamNode.ID, SourceID: encodeBytes(request.SourceID),
		PolicyID: encodeBytes(request.PolicyID),
	}
	result.nodeIDs[teamNode.ID] = struct{}{}
	members := make(map[shoal.ID]struct{})
	for _, edge := range edges {
		if edge.Type == RelationMemberOf && edge.To == request.TeamID {
			members[edge.From] = struct{}{}
			result.edgeIDs[edge.ID] = struct{}{}
		}
	}
	people := make(map[shoal.ID]*Person)
	agents := make(map[shoal.ID]*Agent)
	items := make(map[shoal.ID]*WorkItem)
	for _, node := range nodes {
		if _, member := members[node.ID]; !member {
			continue
		}
		result.nodeIDs[node.ID] = struct{}{}
		name := firstProperty(node, PropertyName, PropertyTitle)
		switch node.Kind {
		case KindPerson:
			person := Person{
				ID: node.ID, Name: name, NodeID: node.ID,
				SubjectID: shoal.ID(node.Properties[PropertySubjectID]),
			}
			people[node.ID] = &person
		case KindAgent:
			id := shoal.ID(node.Properties[PropertyAgentID])
			if id == "" {
				id = node.ID
			}
			agent := Agent{ID: id, Name: name, NodeID: node.ID}
			agents[id] = &agent
		case KindWorkItem:
			item := WorkItem{
				ID: node.ID, Title: name,
				Status: node.Properties[PropertyStatus],
				NodeID: node.ID, AssignedTo: []shoal.ID{}, BlockedBy: []shoal.ID{},
			}
			items[node.ID] = &item
		case KindActivity:
			recordedAt, err := time.Parse(
				time.RFC3339Nano, node.Properties[PropertyRecordedAt])
			if err != nil {
				continue
			}
			manual, _ := strconv.ParseBool(node.Properties[PropertyManual])
			result.activities = append(result.activities, graphActivity{
				id: node.ID, recordedAt: recordedAt.UTC(),
				actorID:   shoal.ID(node.Properties[PropertyActorID]),
				operation: node.Properties[PropertyOperation], manual: manual,
			})
		}
	}
	for _, edge := range edges {
		_, fromMember := members[edge.From]
		_, toMember := members[edge.To]
		if edge.Type == RelationMemberOf {
			continue
		}
		if !fromMember || !toMember {
			continue
		}
		result.edgeIDs[edge.ID] = struct{}{}
		switch edge.Type {
		case RelationAssignedTo:
			if item := items[edge.From]; item != nil {
				item.AssignedTo = appendUniqueID(item.AssignedTo, edge.To)
			}
		case RelationBlockedBy:
			if item := items[edge.From]; item != nil {
				if _, ok := items[edge.To]; ok {
					item.BlockedBy = appendUniqueID(item.BlockedBy, edge.To)
				}
			}
		}
	}
	for _, value := range people {
		result.people = append(result.people, *value)
	}
	for _, value := range agents {
		result.agents = append(result.agents, *value)
	}
	for _, value := range items {
		sortIDs(value.AssignedTo)
		sortIDs(value.BlockedBy)
		result.workItems = append(result.workItems, *value)
	}
	sort.Slice(result.people, func(i, j int) bool { return result.people[i].ID < result.people[j].ID })
	sort.Slice(result.agents, func(i, j int) bool { return result.agents[i].ID < result.agents[j].ID })
	sort.Slice(result.workItems, func(i, j int) bool { return result.workItems[i].ID < result.workItems[j].ID })
	return result, nil
}

func firstProperty(node graph.Node, keys ...string) string {
	for _, key := range keys {
		if value := node.Properties[key]; value != "" {
			return value
		}
	}
	return string(node.ID)
}

func appendUniqueID(values []shoal.ID, value shoal.ID) []shoal.ID {
	for _, present := range values {
		if present == value {
			return values
		}
	}
	return append(values, value)
}

func sortIDs(values []shoal.ID) {
	sort.Slice(values, func(i, j int) bool {
		return shoal.CompareID(values[i], values[j]) < 0
	})
}

func (r parsedRoster) agentIDs() []shoal.ID {
	result := make([]shoal.ID, len(r.agents))
	for i := range r.agents {
		result[i] = r.agents[i].ID
	}
	return result
}

func (r parsedRoster) workItemIDs() []shoal.ID {
	result := make([]shoal.ID, len(r.workItems))
	for i := range r.workItems {
		result[i] = r.workItems[i].ID
	}
	return result
}

func (s *Service) loadAgents(
	ctx context.Context, request Request, requestContext fleet.RequestContext,
	agentIDs []shoal.ID,
) ([]fleet.Descriptor, bool, error) {
	allowed := idSet(agentIDs)
	result := make([]fleet.Descriptor, 0, len(agentIDs))
	var cursor []byte
	for pages := 0; len(result) < MaxAgents && pages < 64; pages++ {
		page, err := s.agents.List(ctx, fleet.ListRequest{
			Context: requestContext, SourceIDs: [][]byte{request.SourceID},
			PolicyIDs: [][]byte{request.PolicyID}, Cursor: cursor,
			Limit: fleet.MaxListResults,
		})
		if err != nil {
			return nil, false, err
		}
		for _, descriptor := range page.Descriptors {
			if _, ok := allowed[descriptor.ID]; ok {
				result = append(result, descriptor)
				if len(result) == MaxAgents {
					return result, len(page.Next) > 0, nil
				}
			}
		}
		if len(page.Next) == 0 {
			return result, false, nil
		}
		cursor = page.Next
	}
	return result, true, nil
}

func (s *Service) loadActions(
	ctx context.Context, request Request, requestContext fleet.RequestContext,
	objectIDs, agentIDs []shoal.ID,
) ([]fleet.ActionRecord, bool, error) {
	result := make([]fleet.ActionRecord, 0)
	var cursor []byte
	for pages := 0; len(result) < MaxActions && pages < 64; pages++ {
		remaining := MaxActions - len(result)
		limit := fleet.MaxDispatchListResults
		if remaining < limit {
			limit = remaining
		}
		page, err := s.actions.TeamActions(ctx, fleet.TeamActionListRequest{
			After: cursor, Limit: limit, SourceIDs: [][]byte{request.SourceID},
			PolicyIDs: [][]byte{request.PolicyID}, ObjectIDs: objectIDs,
			AgentIDs: agentIDs, Context: requestContext,
		})
		if err != nil {
			return nil, false, err
		}
		result = append(result, page.Actions...)
		if len(page.Next) == 0 {
			return result, false, nil
		}
		cursor = page.Next
	}
	return result, true, nil
}

func (s *Service) loadInteractions(
	ctx context.Context,
) ([]explorer.InteractionRecord, bool, error) {
	result := make([]explorer.InteractionRecord, 0)
	var after shoal.ID
	for pages := 0; len(result) < MaxInteractions && pages < 64; pages++ {
		remaining := MaxInteractions - len(result)
		limit := uint32(remaining)
		if limit > explorer.MaxInteractionRecordPageSize {
			limit = explorer.MaxInteractionRecordPageSize
		}
		page, err := s.interactions.InteractionRecordsPage(ctx, after, limit)
		if err != nil {
			return nil, false, err
		}
		result = append(result, page.Records...)
		if page.NextAfter == "" {
			return result, false, nil
		}
		after = page.NextAfter
	}
	return result, true, nil
}

func filterActions(
	records []fleet.ActionRecord, request Request, roster parsedRoster,
) []fleet.ActionRecord {
	objects, agents := idSet(roster.workItemIDs()), idSet(roster.agentIDs())
	result := records[:0]
	for _, record := range records {
		_, objectOK := objects[record.ObjectID]
		_, agentOK := agents[record.AgentID]
		if objectOK && agentOK && bytes.Equal(record.SourceID, request.SourceID) &&
			bytes.Equal(record.PolicyID, request.PolicyID) {
			result = append(result, record)
		}
	}
	return result
}

func filterInteractions(
	records []explorer.InteractionRecord, roster parsedRoster,
) []explorer.InteractionRecord {
	subjects := make(map[shoal.ID]struct{})
	for _, person := range roster.people {
		if person.SubjectID != "" {
			subjects[person.SubjectID] = struct{}{}
		}
	}
	for _, agent := range roster.agents {
		subjects[agent.ID] = struct{}{}
	}
	result := records[:0]
	for _, record := range records {
		actor := record.Summary.Actor
		_, subjectMatch := subjects[actor.SubjectID]
		_, actorMatch := subjects[actor.ActorID]
		evidenceMatch := intersectsIDs(record.TouchedNodeIDs, roster.nodeIDs) ||
			intersectsIDs(record.TouchedEdgeIDs, roster.edgeIDs)
		if subjectMatch || actorMatch || evidenceMatch {
			result = append(result, record)
		}
	}
	return result
}

func intersectsIDs(values []shoal.ID, allowed map[shoal.ID]struct{}) bool {
	for _, value := range values {
		if _, ok := allowed[value]; ok {
			return true
		}
	}
	return false
}

func idSet(values []shoal.ID) map[shoal.ID]struct{} {
	result := make(map[shoal.ID]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

func mergeAgents(
	graphAgents []Agent, descriptors []fleet.Descriptor, asOf time.Time,
) []Agent {
	byID := make(map[shoal.ID]fleet.Descriptor, len(descriptors))
	for _, descriptor := range descriptors {
		byID[descriptor.ID] = descriptor
	}
	result := append([]Agent(nil), graphAgents...)
	for i := range result {
		descriptor, ok := byID[result[i].ID]
		if !ok {
			continue
		}
		result[i].Generation = descriptor.Generation
		result[i].LeaseExpiresAt = descriptor.LeaseExpiresAt
		result[i].Active = descriptor.RevokedAt.IsZero() &&
			asOf.Before(descriptor.LeaseExpiresAt)
	}
	return result
}

func buildActivities(
	request Request, roster parsedRoster, actions []fleet.ActionRecord,
	interactions []explorer.InteractionRecord, since, asOf time.Time,
) []Activity {
	source, policy := encodeBytes(request.SourceID), encodeBytes(request.PolicyID)
	result := make([]Activity, 0, len(roster.activities)+len(actions)+len(interactions))
	for _, value := range roster.activities {
		if !inWindow(value.recordedAt, since, asOf) {
			continue
		}
		result = append(result, Activity{
			ID: string(value.id), Kind: "graph_activity",
			RecordedAt: value.recordedAt, ActorID: value.actorID,
			Operation: value.operation, Manual: value.manual,
			Evidence: []Evidence{{Kind: "graph_node", ID: string(value.id), SourceID: source, PolicyID: policy}},
		})
	}
	for _, record := range actions {
		if !inWindow(record.UpdatedAt, since, asOf) {
			continue
		}
		age := time.Duration(0)
		if record.State == fleet.DispatchQueued {
			age = asOf.Sub(record.CreatedAt)
		}
		result = append(result, Activity{
			ID: encodeBytes(record.ID), Kind: "fleet_action",
			RecordedAt: record.UpdatedAt.UTC(), ActorID: record.Actor,
			Operation: record.Capability + "." + record.Action,
			State:     string(record.State), QueueAgeSeconds: durationSeconds(age),
			Evidence: []Evidence{
				{Kind: "fleet_action", ID: encodeBytes(record.ID), SourceID: source, PolicyID: policy},
				{Kind: "graph_node", ID: string(record.ObjectID), SourceID: source, PolicyID: policy},
				{Kind: "fleet_agent", ID: string(record.AgentID), SourceID: source, PolicyID: policy},
			},
		})
	}
	for _, record := range interactions {
		if !inWindow(record.Summary.RecordedAt, since, asOf) ||
			isFleetAuthorization(record.Summary.AuthorizationOperation) {
			continue
		}
		evidence := []Evidence{{
			Kind: "interaction", ID: string(record.Summary.SessionID),
		}}
		if record.Summary.SnapshotID != "" {
			evidence = append(evidence, Evidence{
				Kind: "snapshot", ID: string(record.Summary.SnapshotID),
			})
		}
		for _, id := range record.TouchedNodeIDs {
			if _, ok := roster.nodeIDs[id]; ok {
				evidence = append(evidence, Evidence{
					Kind: "graph_node", ID: string(id), SourceID: source, PolicyID: policy,
				})
			}
		}
		for _, id := range record.TouchedEdgeIDs {
			if _, ok := roster.edgeIDs[id]; ok {
				evidence = append(evidence, Evidence{
					Kind: "graph_edge", ID: string(id), SourceID: source, PolicyID: policy,
				})
			}
		}
		result = append(result, Activity{
			ID: string(record.Summary.SessionID), Kind: "interaction",
			RecordedAt: record.Summary.RecordedAt.UTC(),
			ActorID:    record.Summary.Actor.SubjectID,
			Operation:  string(record.Summary.Operation), Manual: true,
			Evidence: evidence,
		})
	}
	sort.Slice(result, func(i, j int) bool {
		return activityBefore(result[i], result[j])
	})
	return result
}

func isFleetAuthorization(value string) bool {
	switch auth.Operation(value) {
	case auth.OperationDispatch, auth.OperationInvoke,
		// Execute authorizes a claim or a completion since #437, so a
		// lifecycle audit taken under it is fleet activity like any other.
		// Omitting it classified every such audit as a user interaction, which
		// surfaced it in the activity feed beside its own fleet_action entry.
		auth.OperationExecute,
		// Deciding a held request is fleet activity: its audit is the
		// approval transition, not a user interaction with the corpus.
		auth.OperationActionApprove,
		auth.OperationAgentRegister, auth.OperationAgentHeartbeat,
		auth.OperationAgentRevoke, auth.OperationAgentResolve,
		auth.OperationEventPublish:
		return true
	default:
		return false
	}
}

func inWindow(value, since, asOf time.Time) bool {
	return !value.Before(since) && !value.After(asOf)
}

func dayStart(value time.Time) time.Time {
	utc := value.UTC()
	return time.Date(
		utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
}

func activityBefore(left, right Activity) bool {
	if !left.RecordedAt.Equal(right.RecordedAt) {
		return left.RecordedAt.After(right.RecordedAt)
	}
	if left.Kind != right.Kind {
		return left.Kind < right.Kind
	}
	return left.ID < right.ID
}

func buildMetrics(
	actions []fleet.ActionRecord, activities []Activity,
	since, asOf time.Time,
) Metrics {
	states := actionStateCounts(actions)
	queueAges := make([]time.Duration, 0)
	cycleTimes := make([]time.Duration, 0)
	var completed, failed uint64
	for _, record := range actions {
		switch record.State {
		case fleet.DispatchQueued:
			if !record.CreatedAt.After(asOf) {
				queueAges = append(queueAges, asOf.Sub(record.CreatedAt))
			}
		case fleet.DispatchSucceeded, fleet.DispatchFailed:
			if inWindow(record.UpdatedAt, since, asOf) &&
				!record.UpdatedAt.Before(record.CreatedAt) {
				cycleTimes = append(cycleTimes, record.UpdatedAt.Sub(record.CreatedAt))
			}
			if inWindow(record.UpdatedAt, since, asOf) {
				if record.State == fleet.DispatchSucceeded {
					completed++
				} else {
					failed++
				}
			}
		}
	}
	actors := make(map[shoal.ID]uint64)
	daily := dailyBuckets(since, asOf)
	for _, activity := range activities {
		if activity.ActorID != "" {
			actors[activity.ActorID]++
		}
		date := activity.RecordedAt.UTC().Format("2006-01-02")
		bucket := daily[date]
		if bucket == nil {
			continue
		}
		bucket.Activities++
		if activity.Manual {
			bucket.Manual++
		}
		switch activity.Kind {
		case "interaction":
			bucket.Interactions++
		case "fleet_action":
			bucket.Actions++
			if activity.State == string(fleet.DispatchSucceeded) {
				bucket.Completed++
			}
			if activity.State == string(fleet.DispatchFailed) {
				bucket.Failed++
			}
		}
	}
	actorValues := make([]ActorActivity, 0, len(actors))
	for id, count := range actors {
		actorValues = append(actorValues, ActorActivity{ActorID: id, Count: count})
	}
	sort.Slice(actorValues, func(i, j int) bool {
		if actorValues[i].Count != actorValues[j].Count {
			return actorValues[i].Count > actorValues[j].Count
		}
		return actorValues[i].ActorID < actorValues[j].ActorID
	})
	dailyValues := make([]DailyBucket, 0, len(daily))
	for day := dayStart(since); !day.After(asOf); day = day.AddDate(0, 0, 1) {
		dailyValues = append(dailyValues, *daily[day.Format("2006-01-02")])
	}
	queue := QueueMetrics{
		Count:            uint64(len(queueAges)),
		MedianAgeSeconds: durationSeconds(medianDuration(queueAges)),
		AgedAfterSeconds: durationSeconds(agedQueueThreshold),
	}
	var oldestAge time.Duration
	for _, age := range queueAges {
		if age > oldestAge {
			oldestAge = age
			queue.OldestAgeSeconds = durationSeconds(age)
		}
		if age >= agedQueueThreshold {
			queue.AgedCount++
		}
	}
	currentStart := asOf.AddDate(0, 0, -7)
	previousStart := asOf.AddDate(0, 0, -14)
	return Metrics{
		ActionStates: states, Completed: completed, Failed: failed,
		MedianCycleTimeSeconds: durationSeconds(medianDuration(cycleTimes)),
		Queue:                  queue,
		ActivityByActor:        actorValues, Daily: dailyValues,
		SevenDay: SevenDayComparison{
			Current:  periodMetrics(actions, currentStart, asOf),
			Previous: periodMetrics(actions, previousStart, currentStart),
		},
	}
}

func dailyBuckets(since, asOf time.Time) map[string]*DailyBucket {
	result := make(map[string]*DailyBucket)
	for day := dayStart(since); !day.After(asOf); day = day.AddDate(0, 0, 1) {
		date := day.Format("2006-01-02")
		result[date] = &DailyBucket{Date: date}
	}
	return result
}

func periodMetrics(
	actions []fleet.ActionRecord, start, end time.Time,
) PeriodMetrics {
	result := PeriodMetrics{Started: start.UTC(), Ended: end.UTC()}
	cycles := make([]time.Duration, 0)
	for _, record := range actions {
		if record.UpdatedAt.Before(start) || !record.UpdatedAt.Before(end) {
			continue
		}
		switch record.State {
		case fleet.DispatchSucceeded:
			result.Completed++
			result.TerminalActions++
		case fleet.DispatchFailed:
			result.Failed++
			result.TerminalActions++
		default:
			continue
		}
		if !record.UpdatedAt.Before(record.CreatedAt) {
			cycles = append(cycles, record.UpdatedAt.Sub(record.CreatedAt))
		}
	}
	if result.TerminalActions > 0 {
		result.FailureRate = float64(result.Failed) / float64(result.TerminalActions)
	}
	result.MedianCycleTimeSeconds = durationSeconds(medianDuration(cycles))
	return result
}

func medianDuration(values []time.Duration) time.Duration {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	middle := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[middle]
	}
	return (sorted[middle-1] + sorted[middle]) / 2
}

func durationSeconds(value time.Duration) int64 {
	return int64(value / time.Second)
}

func buildInsights(
	roster parsedRoster, actions []fleet.ActionRecord, activities []Activity,
	metrics Metrics, since, asOf time.Time,
) []Insight {
	result := make([]Insight, 0)
	if metrics.Queue.AgedCount > 0 {
		evidence := make([]Evidence, 0)
		for _, action := range actions {
			if action.State == fleet.DispatchQueued &&
				!action.CreatedAt.After(asOf) &&
				asOf.Sub(action.CreatedAt) >= agedQueueThreshold {
				evidence = appendBoundedEvidence(evidence, Evidence{
					Kind: "fleet_action", ID: encodeBytes(action.ID),
				})
			}
		}
		result = append(result, Insight{
			Kind: "bottleneck", Label: "aged_queue", Severity: "warning",
			Description: fmt.Sprintf(
				"%d queued action(s) are at least %s old",
				metrics.Queue.AgedCount, agedQueueThreshold),
			Heuristic: true, Evidence: evidence,
		})
	}
	type blockedSignal struct {
		count    int
		evidence []Evidence
	}
	blockedBy := make(map[shoal.ID]blockedSignal)
	for _, item := range roster.workItems {
		for _, blocker := range item.BlockedBy {
			signal := blockedBy[blocker]
			signal.count++
			signal.evidence = appendBoundedEvidence(
				signal.evidence,
				Evidence{Kind: "graph_node", ID: string(item.NodeID)},
				Evidence{Kind: "graph_node", ID: string(blocker)},
			)
			blockedBy[blocker] = signal
		}
	}
	for blocker, signal := range blockedBy {
		if signal.count < repeatedBlockThreshold {
			continue
		}
		result = append(result, Insight{
			Kind: "bottleneck", Label: "repeated_blocks", Severity: "warning",
			Description: fmt.Sprintf(
				"work item %s blocks at least %d team work items",
				blocker, signal.count),
			Heuristic: true, Evidence: signal.evidence,
		})
	}
	terminal := metrics.Completed + metrics.Failed
	if terminal >= highFailureMinimum &&
		float64(metrics.Failed)/float64(terminal) >= highFailureRate {
		evidence := actionEvidence(actions, fleet.DispatchFailed)
		result = append(result, Insight{
			Kind: "bottleneck", Label: "high_failure_rate", Severity: "warning",
			Description: fmt.Sprintf(
				"failed actions are %.0f%% of completed and failed actions",
				100*float64(metrics.Failed)/float64(terminal)),
			Heuristic: true, Evidence: evidence,
		})
	}
	assignments := make(map[shoal.ID][]Evidence)
	totalAssignments := 0
	for _, item := range roster.workItems {
		for _, assignee := range item.AssignedTo {
			totalAssignments++
			assignments[assignee] = appendBoundedEvidence(
				assignments[assignee],
				Evidence{Kind: "graph_node", ID: string(item.NodeID)},
			)
		}
	}
	for assignee, evidence := range assignments {
		if totalAssignments >= concentratedMinimumItems &&
			len(evidence)*2 > totalAssignments {
			result = append(result, Insight{
				Kind: "bottleneck", Label: "concentrated_assignments",
				Severity: "notice",
				Description: fmt.Sprintf(
					"%s owns %d of %d current assignments",
					assignee, len(evidence), totalAssignments),
				Heuristic: true, Evidence: evidence,
			})
		}
	}
	current, previous := metrics.SevenDay.Current, metrics.SevenDay.Previous
	if current.TerminalActions >= 2 && previous.TerminalActions >= 2 &&
		(current.Completed > previous.Completed ||
			current.FailureRate < previous.FailureRate ||
			current.MedianCycleTimeSeconds > 0 &&
				previous.MedianCycleTimeSeconds > 0 &&
				current.MedianCycleTimeSeconds <
					previous.MedianCycleTimeSeconds) {
		result = append(result, Insight{
			Kind: "improvement", Label: "seven_day_improvement",
			Severity:    "notice",
			Description: "the latest seven-day window improved on throughput, failure rate, or median cycle time",
			Heuristic:   true, Evidence: terminalActionEvidence(actions, since, asOf),
		})
	}
	type manualSignal struct {
		count    int
		evidence []Evidence
	}
	manual := make(map[string]manualSignal)
	for _, activity := range activities {
		if activity.Manual && activity.Operation != "" {
			signal := manual[activity.Operation]
			signal.count++
			signal.evidence = appendBoundedEvidence(
				signal.evidence, activity.Evidence...)
			manual[activity.Operation] = signal
		}
	}
	for operation, signal := range manual {
		if signal.count < automationThreshold {
			continue
		}
		result = append(result, Insight{
			Kind: "automation_candidate", Label: "repeated_manual_operation",
			Severity: "notice",
			Description: fmt.Sprintf(
				"manual operation %q appears repeatedly in the bounded window",
				operation),
			Heuristic: true, Evidence: signal.evidence,
		})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Kind != result[j].Kind {
			return result[i].Kind < result[j].Kind
		}
		if result[i].Label != result[j].Label {
			return result[i].Label < result[j].Label
		}
		return result[i].Description < result[j].Description
	})
	return result
}

func actionEvidence(
	actions []fleet.ActionRecord, state fleet.DispatchState,
) []Evidence {
	result := make([]Evidence, 0)
	for _, record := range actions {
		if record.State == state {
			result = appendBoundedEvidence(result, Evidence{
				Kind: "fleet_action", ID: encodeBytes(record.ID),
			})
		}
	}
	return result
}

func terminalActionEvidence(
	actions []fleet.ActionRecord, since, asOf time.Time,
) []Evidence {
	result := make([]Evidence, 0)
	for _, record := range actions {
		if (record.State == fleet.DispatchSucceeded ||
			record.State == fleet.DispatchFailed) &&
			inWindow(record.UpdatedAt, since, asOf) {
			result = appendBoundedEvidence(result, Evidence{
				Kind: "fleet_action", ID: encodeBytes(record.ID),
			})
		}
	}
	return result
}

func appendBoundedEvidence(
	values []Evidence, additions ...Evidence,
) []Evidence {
	const maxInsightEvidence = 32
	for _, addition := range additions {
		if len(values) == maxInsightEvidence {
			break
		}
		duplicate := false
		for _, present := range values {
			if present.Kind == addition.Kind && present.ID == addition.ID {
				duplicate = true
				break
			}
		}
		if !duplicate {
			values = append(values, addition)
		}
	}
	return values
}

func pageActivities(
	activities []Activity, request Request, cursor cursorValue, asOf time.Time,
) ([]Activity, string, error) {
	start := 0
	if !cursor.AfterAt.IsZero() {
		start = sort.Search(len(activities), func(i int) bool {
			value := activities[i]
			if value.RecordedAt.Before(cursor.AfterAt) {
				return true
			}
			if value.RecordedAt.After(cursor.AfterAt) {
				return false
			}
			if value.Kind > cursor.AfterKind {
				return true
			}
			return value.Kind == cursor.AfterKind && value.ID > cursor.AfterID
		})
	}
	end := start + int(request.Limit)
	if end > len(activities) {
		end = len(activities)
	}
	page := append([]Activity(nil), activities[start:end]...)
	if end == len(activities) || len(page) == 0 {
		return page, "", nil
	}
	last := page[len(page)-1]
	next, err := encodeCursor(cursorValue{
		TeamID: request.TeamID, SourceID: encodeBytes(request.SourceID),
		PolicyID:    encodeBytes(request.PolicyID),
		HistoryDays: request.HistoryDays, AsOf: asOf,
		AfterAt: last.RecordedAt, AfterKind: last.Kind, AfterID: last.ID,
	})
	return page, next, err
}

func encodeCursor(value cursorValue) (string, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return "", shoal.NewError(
			shoal.ErrorUnavailable, "team activity cursor could not be encoded")
	}
	return base64.RawURLEncoding.EncodeToString(payload), nil
}

func decodeCursor(value string) (cursorValue, error) {
	if value == "" {
		return cursorValue{}, nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || base64.RawURLEncoding.EncodeToString(payload) != value {
		return cursorValue{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "team activity cursor is invalid")
	}
	var result cursorValue
	if err := json.Unmarshal(payload, &result); err != nil {
		return cursorValue{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "team activity cursor is invalid")
	}
	return result, nil
}

func encodeBytes(value []byte) string {
	return base64.RawURLEncoding.EncodeToString(value)
}
