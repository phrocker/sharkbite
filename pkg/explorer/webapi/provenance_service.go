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

package webapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	DefaultProvenancePageSize = 100
	MaxProvenancePageSize     = 1000
)

type ProvenanceListRequest struct {
	Limit  uint32 `json:"limit,omitempty"`
	Cursor string `json:"cursor,omitempty"`
}

type ProvenanceListResponse struct {
	Interactions []ProvenanceSummary `json:"interactions"`
	Folds        []ProvenanceFold    `json:"folds"`
	NextCursor   string              `json:"next_cursor,omitempty"`
}

type ProvenanceSummary struct {
	SessionID                string                `json:"session_id"`
	InferenceID              string                `json:"inference_id,omitempty"`
	RecordedAt               time.Time             `json:"recorded_at"`
	SnapshotID               string                `json:"snapshot_id"`
	SnapshotAsOf             time.Time             `json:"snapshot_as_of"`
	AuthorizationFingerprint string                `json:"authorization_fingerprint"`
	AuthorizationExpiresAt   time.Time             `json:"authorization_expires_at"`
	EmbeddingSpaceID         string                `json:"embedding_space_id,omitempty"`
	EmbeddingSpaceIDs        []string              `json:"embedding_space_ids,omitempty"`
	Operation                interaction.Operation `json:"operation"`
	Actor                    ProvenanceActor       `json:"actor"`
	Reason                   interaction.Reason    `json:"reason"`
	// CorrelationID is the recording decision's correlation (#532), shown
	// beside the actor it travelled with and only where the actor is.
	CorrelationID    string `json:"correlation_id,omitempty"`
	OutputVisibility string `json:"output_visibility"`
	NodeCount        int    `json:"node_count"`
	EdgeCount        int    `json:"edge_count"`
}

type ProvenanceActor struct {
	SubjectID  string   `json:"subject_id"`
	ActorID    string   `json:"actor_id"`
	ClientID   string   `json:"client_id,omitempty"`
	OnBehalfOf []string `json:"on_behalf_of"`
}

type ProvenanceSession struct {
	ProvenanceSummary
	RequestID     string                 `json:"request_id,omitempty"`
	ContextPackID string                 `json:"context_pack_id,omitempty"`
	ResultID      string                 `json:"result_id,omitempty"`
	QueryDigest   string                 `json:"query_digest,omitempty"`
	StopReason    string                 `json:"stop_reason,omitempty"`
	Model         interaction.Provenance `json:"model"`
	RetrievedIDs  []string               `json:"retrieved_source_ids"`
	CitedIDs      []string               `json:"cited_source_ids"`
	Turns         []ProvenanceTurn       `json:"turns"`
}

type ProvenanceTurn struct {
	Index        int      `json:"index"`
	Decision     string   `json:"decision"`
	InputTokens  int      `json:"input_tokens"`
	OutputTokens int      `json:"output_tokens"`
	Failed       bool     `json:"failed"`
	ToolKind     string   `json:"tool_kind,omitempty"`
	RetrievedIDs []string `json:"retrieved_source_ids"`
}

type ProvenanceFoldRequest struct {
	SessionIDs    []string `json:"session_ids"`
	SummaryDigest string   `json:"summary_digest,omitempty"`
}

type ProvenanceUnfoldRequest struct {
	FoldID string `json:"fold_id"`
}

type ProvenanceFold struct {
	FoldID           string                 `json:"fold_id"`
	Created          bool                   `json:"created,omitempty"`
	FoldedAt         time.Time              `json:"folded_at"`
	OutputVisibility string                 `json:"output_visibility"`
	SummaryDigest    string                 `json:"summary_digest,omitempty"`
	MemberCount      int                    `json:"member_count"`
	RetrievedCount   int                    `json:"retrieved_count,omitempty"`
	CitedCount       int                    `json:"cited_count,omitempty"`
	Members          []ProvenanceFoldMember `json:"members,omitempty"`
}

type ProvenanceFoldMember struct {
	SessionID        string   `json:"session_id"`
	RetrievedIDs     []string `json:"retrieved_source_ids"`
	CitedIDs         []string `json:"cited_source_ids"`
	OutputVisibility []string `json:"output_visibility"`
}

// InteractionProvider is shared by HTTP and MCP transports. Implementations
// must perform current authorization before exposing derived provenance.
type InteractionProvider interface {
	ListProvenance(context.Context, ProvenanceListRequest) (ProvenanceListResponse, error)
	InspectProvenance(context.Context, shoal.ID) (ProvenanceSession, error)
	FoldProvenance(context.Context, ProvenanceFoldRequest) (ProvenanceFold, error)
	UnfoldProvenance(context.Context, ProvenanceUnfoldRequest) (ProvenanceFold, error)
}

type provenanceClient interface {
	InteractionRecordsPage(
		context.Context, shoal.ID, uint32,
	) (explorer.InteractionRecordPage, error)
	Interaction(context.Context, shoal.ID) (interaction.Session, error)
	FoldsPage(
		context.Context, shoal.ID, uint32,
	) (explorer.FoldSummaryPage, error)
	FoldInteractions(context.Context, explorer.FoldRequest) (explorer.FoldResult, error)
	RehydrateFold(context.Context, shoal.ID) (interaction.Fold, error)
}

type InteractionService struct {
	client provenanceClient
}

func NewInteractionService(client *authorized.Client) (*InteractionService, error) {
	if client == nil {
		return nil, chatInvalid("authorized provenance client is required")
	}
	return &InteractionService{client: client}, nil
}

func (s *InteractionService) ListProvenance(
	ctx context.Context, request ProvenanceListRequest,
) (ProvenanceListResponse, error) {
	limit := request.Limit
	if limit == 0 {
		limit = DefaultProvenancePageSize
	}
	if limit > MaxProvenancePageSize {
		return ProvenanceListResponse{}, chatInvalid(
			"provenance list limit exceeds the public bound")
	}
	cursor, err := decodeProvenanceCursor(request.Cursor)
	if err != nil {
		return ProvenanceListResponse{}, err
	}
	response := ProvenanceListResponse{}
	next := provenanceCursor{
		InteractionAfter: cursor.InteractionAfter,
		FoldAfter:        cursor.FoldAfter,
		InteractionsDone: cursor.InteractionsDone,
		FoldsDone:        cursor.FoldsDone,
	}
	remaining := limit
	if !cursor.InteractionsDone {
		page, err := s.client.InteractionRecordsPage(
			ctx, cursor.InteractionAfter, remaining)
		if err != nil {
			return ProvenanceListResponse{}, err
		}
		for _, value := range page.Records {
			response.Interactions = append(
				response.Interactions, provenanceSummary(value.Summary))
		}
		next.InteractionAfter = page.NextAfter
		next.InteractionsDone = page.NextAfter == ""
		remaining -= uint32(len(page.Records))
	}
	if next.InteractionsDone && remaining > 0 && !cursor.FoldsDone {
		page, err := s.client.FoldsPage(ctx, cursor.FoldAfter, remaining)
		if err != nil {
			return ProvenanceListResponse{}, err
		}
		for _, value := range page.Folds {
			response.Folds = append(response.Folds, ProvenanceFold{
				FoldID: encodeID(value.FoldID), FoldedAt: value.FoldedAt,
				OutputVisibility: value.Visibility,
				SummaryDigest:    value.SummaryDigest,
				MemberCount:      value.MemberCount,
			})
		}
		next.FoldAfter = page.NextAfter
		next.FoldsDone = page.NextAfter == ""
	}
	if !next.InteractionsDone || !next.FoldsDone {
		response.NextCursor, err = encodeProvenanceCursor(next)
		if err != nil {
			return ProvenanceListResponse{}, err
		}
	}
	return response, nil
}

type provenanceCursor struct {
	InteractionAfter shoal.ID
	FoldAfter        shoal.ID
	InteractionsDone bool
	FoldsDone        bool
}

type provenanceCursorWire struct {
	InteractionAfter string `json:"interaction_after,omitempty"`
	FoldAfter        string `json:"fold_after,omitempty"`
	InteractionsDone bool   `json:"interactions_done,omitempty"`
	FoldsDone        bool   `json:"folds_done,omitempty"`
}

func encodeProvenanceCursor(cursor provenanceCursor) (string, error) {
	wire := provenanceCursorWire{
		InteractionAfter: encodeOptionalID(
			cursor.InteractionAfter),
		FoldAfter:        encodeOptionalID(cursor.FoldAfter),
		InteractionsDone: cursor.InteractionsDone,
		FoldsDone:        cursor.FoldsDone,
	}
	encoded, err := json.Marshal(wire)
	if err != nil {
		return "", chatInvalid("provenance list cursor is invalid")
	}
	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

func decodeProvenanceCursor(encoded string) (provenanceCursor, error) {
	if encoded == "" {
		return provenanceCursor{}, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || base64.RawURLEncoding.EncodeToString(decoded) != encoded {
		return provenanceCursor{}, chatInvalid(
			"provenance list cursor is invalid")
	}
	var wire provenanceCursorWire
	if err := json.Unmarshal(decoded, &wire); err != nil {
		return provenanceCursor{}, chatInvalid(
			"provenance list cursor is invalid")
	}
	result := provenanceCursor{
		InteractionsDone: wire.InteractionsDone,
		FoldsDone:        wire.FoldsDone,
	}
	result.InteractionAfter, err = decodeOptionalCursorID(
		wire.InteractionAfter)
	if err != nil {
		return provenanceCursor{}, err
	}
	result.FoldAfter, err = decodeOptionalCursorID(wire.FoldAfter)
	if err != nil {
		return provenanceCursor{}, err
	}
	return result, nil
}

func decodeOptionalCursorID(encoded string) (shoal.ID, error) {
	id, err := decodeOptionalID(encoded)
	if err != nil {
		return "", chatInvalid("provenance list cursor is invalid")
	}
	return id, nil
}

func (s *InteractionService) InspectProvenance(
	ctx context.Context, sessionID shoal.ID,
) (ProvenanceSession, error) {
	session, err := s.client.Interaction(ctx, sessionID)
	if err != nil {
		return ProvenanceSession{}, err
	}
	return provenanceSession(session), nil
}

func (s *InteractionService) FoldProvenance(
	ctx context.Context, input ProvenanceFoldRequest,
) (ProvenanceFold, error) {
	ids := make([]shoal.ID, 0, len(input.SessionIDs))
	for _, encoded := range input.SessionIDs {
		id, err := decodeID(encoded)
		if err != nil {
			return ProvenanceFold{}, chatInvalid("session_ids contains an invalid ID")
		}
		ids = append(ids, id)
	}
	result, err := s.client.FoldInteractions(ctx, explorer.FoldRequest{
		SessionIDs: ids, SummaryDigest: strings.TrimSpace(input.SummaryDigest),
	})
	if err != nil {
		return ProvenanceFold{}, err
	}
	return ProvenanceFold{
		FoldID: encodeID(result.FoldID), Created: result.Created,
		FoldedAt: result.FoldedAt, OutputVisibility: result.Visibility,
		MemberCount: result.MemberCount, RetrievedCount: result.RetrievedCount,
		CitedCount: result.CitedCount,
	}, nil
}

func (s *InteractionService) UnfoldProvenance(
	ctx context.Context, input ProvenanceUnfoldRequest,
) (ProvenanceFold, error) {
	foldID, err := decodeID(input.FoldID)
	if err != nil {
		return ProvenanceFold{}, chatInvalid("fold_id is invalid")
	}
	value, err := s.client.RehydrateFold(ctx, foldID)
	if err != nil {
		return ProvenanceFold{}, err
	}
	response := ProvenanceFold{
		FoldID: encodeID(foldID), FoldedAt: value.FoldedAt,
		SummaryDigest: value.SummaryDigest, MemberCount: len(value.Members),
	}
	for _, member := range value.Members {
		response.Members = append(response.Members, ProvenanceFoldMember{
			SessionID:        encodeID(member.SessionID),
			RetrievedIDs:     encodeIDs(member.RetrievedNodeIDs),
			CitedIDs:         encodeIDs(member.CitedNodeIDs),
			OutputVisibility: append([]string(nil), member.Visibility...),
		})
		response.RetrievedCount += len(member.RetrievedNodeIDs)
		response.CitedCount += len(member.CitedNodeIDs)
	}
	return response, nil
}

func provenanceSummary(value explorer.InteractionSummary) ProvenanceSummary {
	return ProvenanceSummary{
		SessionID: encodeID(value.SessionID), InferenceID: encodeOptionalID(value.InferenceID),
		RecordedAt: value.RecordedAt, SnapshotID: encodeID(value.SnapshotID),
		SnapshotAsOf:             value.SnapshotAsOf,
		AuthorizationFingerprint: encodeID(value.AuthorizationFingerprint),
		AuthorizationExpiresAt:   value.AuthorizationExpiresAt,
		EmbeddingSpaceID:         encodeOptionalID(value.EmbeddingSpaceID),
		Operation:                value.Operation, Actor: provenanceActor(value.Actor),
		Reason:           value.Reason,
		CorrelationID:    encodeOptionalID(value.CorrelationID),
		OutputVisibility: value.Visibility,
		NodeCount:        value.NodeCount, EdgeCount: value.EdgeCount,
	}
}

func provenanceSession(value interaction.Session) ProvenanceSession {
	retrieved := append([]shoal.ID(nil), value.SeedNodeIDs...)
	response := ProvenanceSession{
		ProvenanceSummary: ProvenanceSummary{
			SessionID: encodeID(value.ID), RecordedAt: value.RecordedAt,
			SnapshotID: encodeID(value.SnapshotID), SnapshotAsOf: value.SnapshotAsOf,
			AuthorizationFingerprint: encodeID(value.AuthorizationFingerprint),
			AuthorizationExpiresAt:   value.AuthorizationExpiresAt,
			EmbeddingSpaceID:         encodeOptionalID(value.EmbeddingSpaceID),
			Operation:                value.Operation, Actor: provenanceActor(value.Actor),
			Reason:           value.Reason,
			CorrelationID:    encodeOptionalID(value.CorrelationID),
			OutputVisibility: interaction.Expression(value.RequiredVisibility),
		},
		RequestID:     encodeOptionalID(value.RequestID),
		ContextPackID: encodeOptionalID(value.ContextPackID),
		ResultID:      encodeOptionalID(value.ResultID), QueryDigest: value.QueryDigest,
		StopReason: value.StopReason, Model: value.Provenance,
		CitedIDs: encodeIDs(value.CitedNodeIDs),
	}
	for _, turn := range value.Turns {
		item := ProvenanceTurn{
			Index: turn.Index, Decision: turn.Decision,
			InputTokens: turn.InputTokens, OutputTokens: turn.OutputTokens,
			Failed: turn.Failed,
		}
		if turn.ToolCall != nil {
			item.ToolKind = turn.ToolCall.Kind
			item.RetrievedIDs = encodeIDs(turn.ToolCall.RetrievedNodeIDs)
			retrieved = append(retrieved, turn.ToolCall.RetrievedNodeIDs...)
		}
		response.Turns = append(response.Turns, item)
	}
	response.RetrievedIDs = encodeIDs(dedupeOpaqueIDs(retrieved))
	return response
}

func provenanceActor(value interaction.ActorContext) ProvenanceActor {
	return ProvenanceActor{
		SubjectID:  encodeOptionalID(value.SubjectID),
		ActorID:    encodeOptionalID(value.ActorID),
		ClientID:   encodeOptionalID(value.ClientID),
		OnBehalfOf: encodeIDs(value.OnBehalfOf),
	}
}

func dedupeOpaqueIDs(values []shoal.ID) []shoal.ID {
	seen := make(map[shoal.ID]struct{}, len(values))
	result := make([]shoal.ID, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}
