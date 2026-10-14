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

package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	ToolAsk               = "shoal.ask"
	ToolProvenanceList    = "shoal.provenance.list"
	ToolProvenanceInspect = "shoal.provenance.inspect"
	ToolProvenanceFold    = "shoal.provenance.fold"
	ToolProvenanceUnfold  = "shoal.provenance.unfold"
)

var (
	askToolSchema = json.RawMessage(`{
		"type":"object",
		"properties":{
			"question":{"type":"string","minLength":1,"maxLength":32768},
			"top_k":{"type":"integer","minimum":0,"maximum":50}
		},
		"required":["question"],
		"additionalProperties":false
	}`)
	provenanceListToolSchema = json.RawMessage(`{
		"type":"object",
		"properties":{
			"limit":{"type":"integer","minimum":0,"maximum":1000},
			"cursor":{"type":"string"}
		},
		"additionalProperties":false
	}`)
	provenanceInspectToolSchema = json.RawMessage(`{
		"type":"object",
		"properties":{
			"session_id":{"type":"string","minLength":1,"maxLength":1366}
		},
		"required":["session_id"],
		"additionalProperties":false
	}`)
	provenanceFoldToolSchema = json.RawMessage(`{
		"type":"object",
		"properties":{
			"session_ids":{
				"type":"array",
				"items":{"type":"string","minLength":1,"maxLength":1366},
				"minItems":1,
				"maxItems":4096
			},
			"summary_digest":{"type":"string"}
		},
		"required":["session_ids"],
		"additionalProperties":false
	}`)
	provenanceUnfoldToolSchema = json.RawMessage(`{
		"type":"object",
		"properties":{
			"fold_id":{"type":"string","minLength":1,"maxLength":1366}
		},
		"required":["fold_id"],
		"additionalProperties":false
	}`)
)

type providerTool struct {
	definition Tool
	operation  auth.Operation
	call       func(context.Context, json.RawMessage) (any, error)
	observe    func(any) (ToolObservation, error)
}

func (p providerTool) Tool() Tool {
	return cloneTool(p.definition)
}

func (p providerTool) Call(
	ctx context.Context, arguments json.RawMessage,
) (any, error) {
	return p.call(ctx, arguments)
}

func (p providerTool) ToolAuthorizationOperation() auth.Operation {
	return p.operation
}

func (p providerTool) ObserveToolResult(
	value any,
) (ToolObservation, error) {
	if p.observe == nil {
		return ToolObservation{}, nil
	}
	return p.observe(value)
}

// NewAskTool adapts the shared, already-authorized and durably captured chat
// provider. It does not construct a second retrieval or reasoning pipeline.
func NewAskTool(provider webapi.AskProvider) (OptionalToolProvider, error) {
	if isAbsent(provider) {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "MCP ask provider is required")
	}
	return providerTool{
		definition: Tool{
			Name: ToolAsk, Title: "Ask Shoal",
			Description: "Answer a question from verified, authorized workspace evidence.",
			InputSchema: askToolSchema,
			Annotations: &ToolAnnotations{
				ReadOnlyHint: boolHint(false), DestructiveHint: boolHint(false),
				IdempotentHint: boolHint(false), OpenWorldHint: boolHint(false),
			},
			Execution: &ToolExecution{TaskSupport: "forbidden"},
		},
		operation: auth.OperationRetrieve,
		call: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var request webapi.AskRequest
			if err := strictDecode(raw, &request); err != nil {
				return nil, invalidToolArguments(ToolAsk)
			}
			return provider.Ask(ctx, request)
		},
		observe: observeCitationEnvelope,
	}, nil
}

// NewInteractionTools adapts the shared provenance provider. The returned
// tools preserve the provider's exact authorization and mutation semantics.
func NewInteractionTools(
	provider webapi.InteractionProvider,
) ([]OptionalToolProvider, error) {
	if isAbsent(provider) {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "MCP interaction provider is required")
	}
	definitions := []struct {
		name        string
		title       string
		description string
		schema      json.RawMessage
		method      webapi.InteractionProviderMethod
		call        func(context.Context, json.RawMessage) (any, error)
		observe     func(any) (ToolObservation, error)
	}{
		{
			name: ToolProvenanceList, title: "List Provenance",
			description: "List authorized durable interaction and fold records.",
			schema:      provenanceListToolSchema, method: webapi.InteractionMethodList,
			call: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var request webapi.ProvenanceListRequest
				if err := strictDecode(raw, &request); err != nil {
					return nil, invalidToolArguments(ToolProvenanceList)
				}
				return provider.ListProvenance(ctx, request)
			},
		},
		{
			name: ToolProvenanceInspect, title: "Inspect Provenance",
			description: "Inspect one authorized durable interaction record.",
			schema:      provenanceInspectToolSchema, method: webapi.InteractionMethodInspect,
			call: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var request struct {
					SessionID string `json:"session_id"`
				}
				if err := strictDecode(raw, &request); err != nil {
					return nil, invalidToolArguments(ToolProvenanceInspect)
				}
				sessionID, err := decodeProviderID(request.SessionID)
				if err != nil {
					return nil, invalidToolArguments(ToolProvenanceInspect)
				}
				return provider.InspectProvenance(ctx, sessionID)
			},
			observe: observeProvenanceSession,
		},
		{
			name: ToolProvenanceFold, title: "Fold Provenance",
			description: "Create or replay an idempotent provenance fold.",
			schema:      provenanceFoldToolSchema, method: webapi.InteractionMethodFold,
			call: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var request webapi.ProvenanceFoldRequest
				if err := strictDecode(raw, &request); err != nil {
					return nil, invalidToolArguments(ToolProvenanceFold)
				}
				return provider.FoldProvenance(ctx, request)
			},
		},
		{
			name: ToolProvenanceUnfold, title: "Unfold Provenance",
			description: "Read the authorized members and evidence of a provenance fold.",
			schema:      provenanceUnfoldToolSchema, method: webapi.InteractionMethodUnfold,
			call: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var request webapi.ProvenanceUnfoldRequest
				if err := strictDecode(raw, &request); err != nil {
					return nil, invalidToolArguments(ToolProvenanceUnfold)
				}
				return provider.UnfoldProvenance(ctx, request)
			},
			observe: observeProvenanceFold,
		},
	}
	tools := make([]OptionalToolProvider, 0, len(definitions))
	for _, definition := range definitions {
		semantics, err := webapi.InteractionSemantics(definition.method)
		if err != nil {
			return nil, err
		}
		annotations := &ToolAnnotations{
			ReadOnlyHint:    boolHint(semantics.ReadOnly),
			DestructiveHint: boolHint(false),
			IdempotentHint:  boolHint(semantics.Idempotent),
			OpenWorldHint:   boolHint(false),
		}
		tools = append(tools, providerTool{
			definition: Tool{
				Name: definition.name, Title: definition.title,
				Description: definition.description,
				InputSchema: definition.schema, Annotations: annotations,
				Execution: &ToolExecution{TaskSupport: "forbidden"},
			},
			operation: semantics.Operation,
			call:      definition.call,
			observe:   definition.observe,
		})
	}
	return tools, nil
}

func observeCitationEnvelope(value any) (ToolObservation, error) {
	envelope, ok := value.(webapi.CitationEnvelope)
	if !ok {
		return ToolObservation{}, shoal.NewError(
			shoal.ErrorInternal, "MCP ask provider returned an invalid result")
	}
	projection, err := envelope.EvidenceProjection()
	if err != nil {
		return ToolObservation{}, err
	}
	retrieved, cited, err := envelope.InteractionEvidence()
	if err != nil {
		return ToolObservation{}, err
	}
	return citationToolObservation(envelope, projection, retrieved, cited)
}

func citationToolObservation(
	envelope webapi.CitationEnvelope,
	projection webapi.CitationEvidenceProjection,
	retrieved []interaction.EvidenceReference,
	cited []interaction.EvidenceReference,
) (ToolObservation, error) {
	return ToolObservation{
		SnapshotID:               envelope.SnapshotID,
		SnapshotAsOf:             envelope.SnapshotAsOf,
		AuthorizationFingerprint: envelope.AuthorizationFingerprint,
		AuthorizationExpiresAt:   envelope.AuthorizationExpiresAt,
		RequestID:                envelope.RequestID,
		EmbeddingSpaceID:         envelope.EmbeddingSpaceID,
		EmbeddingSpaceIDs: append(
			[]shoal.ID(nil), projection.EmbeddingSpaceIDs...),
		RetrievedNodeIDs:   projection.RetrievedSourceIDs,
		RetrievedEvidence:  retrieved,
		CitedNodeIDs:       projection.CitedSourceIDs,
		CitedEvidence:      cited,
		RequiredVisibility: projection.EffectiveVisibility,
	}, nil
}

func observeProvenanceSession(value any) (ToolObservation, error) {
	session, ok := value.(webapi.ProvenanceSession)
	if !ok {
		return ToolObservation{}, shoal.NewError(
			shoal.ErrorInternal,
			"MCP provenance provider returned an invalid session",
		)
	}
	retrieved, err := decodeProviderIDs(session.RetrievedIDs)
	if err != nil {
		return ToolObservation{}, err
	}
	cited, err := decodeProviderIDs(session.CitedIDs)
	if err != nil {
		return ToolObservation{}, err
	}
	return ToolObservation{
		RetrievedNodeIDs: retrieved,
		CitedNodeIDs:     cited,
	}, nil
}

func observeProvenanceFold(value any) (ToolObservation, error) {
	fold, ok := value.(webapi.ProvenanceFold)
	if !ok {
		return ToolObservation{}, shoal.NewError(
			shoal.ErrorInternal,
			"MCP provenance provider returned an invalid fold",
		)
	}
	var retrieved []string
	var cited []string
	for _, member := range fold.Members {
		retrieved = append(retrieved, member.RetrievedIDs...)
		cited = append(cited, member.CitedIDs...)
	}
	retrievedIDs, err := decodeProviderIDs(retrieved)
	if err != nil {
		return ToolObservation{}, err
	}
	citedIDs, err := decodeProviderIDs(cited)
	if err != nil {
		return ToolObservation{}, err
	}
	return ToolObservation{
		RetrievedNodeIDs: retrievedIDs,
		CitedNodeIDs:     citedIDs,
	}, nil
}

func decodeProviderIDs(values []string) ([]shoal.ID, error) {
	result := make([]shoal.ID, 0, len(values))
	for _, value := range values {
		id, err := decodeProviderID(value)
		if err != nil {
			return nil, err
		}
		result = append(result, id)
	}
	return canonicalObservedIDs(result), nil
}

func decodeProviderID(value string) (shoal.ID, error) {
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil || base64.RawURLEncoding.EncodeToString(decoded) != value {
		return "", shoal.NewError(
			shoal.ErrorInvalidArgument, "invalid opaque ID")
	}
	id := shoal.ID(decoded)
	if err := shoal.ValidateRequiredID("opaque ID", id); err != nil {
		return "", err
	}
	return id, nil
}
