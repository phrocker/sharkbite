// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file distributed
// with this work for additional information regarding copyright ownership.
package teamoverview

import (
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	KindTeam     = "team"
	KindPerson   = "person"
	KindAgent    = "agent"
	KindWorkItem = "work_item"
	KindActivity = "activity"

	RelationMemberOf   = "member_of"
	RelationAssignedTo = "assigned_to"
	RelationBlockedBy  = "blocked_by"

	PropertyName       = "name"
	PropertyTitle      = "title"
	PropertySubjectID  = "subject_id"
	PropertyAgentID    = "agent_id"
	PropertyStatus     = "status"
	PropertyRecordedAt = "recorded_at"
	PropertyActorID    = "actor_id"
	PropertyOperation  = "operation"
	PropertyManual     = "manual"

	DefaultHistoryDays uint32 = 14
	MaxHistoryDays     uint32 = 31
	DefaultPageSize    uint32 = 50
	MaxPageSize        uint32 = 100

	MaxGraphNodes   uint32 = 256
	MaxGraphEdges   uint32 = 2048
	MaxGraphScanned uint32 = 2048
	MaxAgents              = 64
	MaxActions             = 1000
	MaxInteractions        = 1000
)

var relationTypes = []string{
	RelationMemberOf,
	RelationAssignedTo,
	RelationBlockedBy,
}

// Request identifies one configured graph-backed team and the bounded history
// window to summarize. Source and policy IDs are untrusted narrowing inputs;
// they never grant access.
type Request struct {
	TeamID      shoal.ID `json:"team_id"`
	SourceID    []byte   `json:"source_id"`
	PolicyID    []byte   `json:"policy_id"`
	HistoryDays uint32   `json:"history_days,omitempty"`
	Limit       uint32   `json:"limit,omitempty"`
	Cursor      string   `json:"cursor,omitempty"`
}

type Team struct {
	ID       shoal.ID `json:"id"`
	Name     string   `json:"name"`
	NodeID   shoal.ID `json:"node_id"`
	SourceID string   `json:"source_id"`
	PolicyID string   `json:"policy_id"`
}

type Person struct {
	ID        shoal.ID `json:"id"`
	Name      string   `json:"name"`
	SubjectID shoal.ID `json:"subject_id,omitempty"`
	NodeID    shoal.ID `json:"node_id"`
}

type Agent struct {
	ID             shoal.ID  `json:"id"`
	Name           string    `json:"name"`
	NodeID         shoal.ID  `json:"node_id"`
	Generation     int64     `json:"generation,omitempty"`
	Active         bool      `json:"active"`
	LeaseExpiresAt time.Time `json:"lease_expires_at,omitempty"`
}

type WorkItem struct {
	ID         shoal.ID   `json:"id"`
	Title      string     `json:"title"`
	Status     string     `json:"status,omitempty"`
	NodeID     shoal.ID   `json:"node_id"`
	AssignedTo []shoal.ID `json:"assigned_to"`
	BlockedBy  []shoal.ID `json:"blocked_by"`
}

type Activity struct {
	ID              string     `json:"id"`
	Kind            string     `json:"kind"`
	RecordedAt      time.Time  `json:"recorded_at"`
	ActorID         shoal.ID   `json:"actor_id,omitempty"`
	Operation       string     `json:"operation,omitempty"`
	Manual          bool       `json:"manual"`
	State           string     `json:"state,omitempty"`
	QueueAgeSeconds int64      `json:"queue_age_seconds,omitempty"`
	Evidence        []Evidence `json:"evidence"`
}

type Evidence struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	SourceID string `json:"source_id,omitempty"`
	PolicyID string `json:"policy_id,omitempty"`
}

type ActionStateCounts struct {
	Queued    uint64 `json:"queued"`
	Claimed   uint64 `json:"claimed"`
	Succeeded uint64 `json:"succeeded"`
	Failed    uint64 `json:"failed"`
	Canceled  uint64 `json:"canceled"`
}

type QueueMetrics struct {
	Count            uint64 `json:"count"`
	MedianAgeSeconds int64  `json:"median_age_seconds"`
	OldestAgeSeconds int64  `json:"oldest_age_seconds"`
	AgedCount        uint64 `json:"aged_count"`
	AgedAfterSeconds int64  `json:"aged_after_seconds"`
}

type ActorActivity struct {
	ActorID shoal.ID `json:"actor_id"`
	Count   uint64   `json:"count"`
}

type DailyBucket struct {
	Date         string `json:"date"`
	Activities   uint64 `json:"activities"`
	Manual       uint64 `json:"manual"`
	Interactions uint64 `json:"interactions"`
	Actions      uint64 `json:"actions"`
	Completed    uint64 `json:"completed"`
	Failed       uint64 `json:"failed"`
}

type PeriodMetrics struct {
	Started                time.Time `json:"started"`
	Ended                  time.Time `json:"ended"`
	Completed              uint64    `json:"completed"`
	Failed                 uint64    `json:"failed"`
	FailureRate            float64   `json:"failure_rate"`
	MedianCycleTimeSeconds int64     `json:"median_cycle_time_seconds"`
	TerminalActions        uint64    `json:"terminal_actions"`
}

type SevenDayComparison struct {
	Current  PeriodMetrics `json:"current"`
	Previous PeriodMetrics `json:"previous"`
}

type Metrics struct {
	ActiveAgents           uint64             `json:"active_agents"`
	ActionStates           ActionStateCounts  `json:"action_states"`
	Completed              uint64             `json:"completed"`
	Failed                 uint64             `json:"failed"`
	MedianCycleTimeSeconds int64              `json:"median_cycle_time_seconds"`
	Queue                  QueueMetrics       `json:"queue"`
	ActivityByActor        []ActorActivity    `json:"activity_by_actor"`
	Daily                  []DailyBucket      `json:"daily"`
	SevenDay               SevenDayComparison `json:"seven_day_comparison"`
}

type Insight struct {
	Kind        string     `json:"kind"`
	Label       string     `json:"label"`
	Severity    string     `json:"severity"`
	Description string     `json:"description"`
	Heuristic   bool       `json:"heuristic"`
	Evidence    []Evidence `json:"evidence"`
}

type Bounds struct {
	AsOf                  time.Time `json:"as_of"`
	Since                 time.Time `json:"since"`
	Complete              bool      `json:"complete"`
	GraphTruncated        bool      `json:"graph_truncated"`
	AgentsTruncated       bool      `json:"agents_truncated"`
	ActionsTruncated      bool      `json:"actions_truncated"`
	InteractionsTruncated bool      `json:"interactions_truncated"`
	MaxGraphNodes         uint32    `json:"max_graph_nodes"`
	MaxGraphEdges         uint32    `json:"max_graph_edges"`
	MaxAgents             uint32    `json:"max_agents"`
	MaxActions            uint32    `json:"max_actions"`
	MaxInteractions       uint32    `json:"max_interactions"`
}

type Response struct {
	Team       Team       `json:"team"`
	People     []Person   `json:"people"`
	Agents     []Agent    `json:"agents"`
	WorkItems  []WorkItem `json:"work_items"`
	Metrics    Metrics    `json:"metrics"`
	Activities []Activity `json:"activities"`
	Insights   []Insight  `json:"insights"`
	Bounds     Bounds     `json:"bounds"`
	NextCursor string     `json:"next_cursor,omitempty"`
}

func actionStateCounts(records []fleet.ActionRecord) ActionStateCounts {
	var result ActionStateCounts
	for _, record := range records {
		switch record.State {
		case fleet.DispatchQueued:
			result.Queued++
		case fleet.DispatchClaimed:
			result.Claimed++
		case fleet.DispatchSucceeded:
			result.Succeeded++
		case fleet.DispatchFailed:
			result.Failed++
		case fleet.DispatchCanceled:
			result.Canceled++
		}
	}
	return result
}
