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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"strings"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	ToolDocuments    = "shoal.documents"
	ToolDocument     = "shoal.document"
	ToolRetrieve     = "shoal.retrieve"
	ToolNeighborhood = "shoal.neighborhood"
	ToolPath         = "shoal.path"
	ToolIngest       = "shoal.ingest"
	ToolExtract      = "shoal.extract"
	ToolRecompute    = "shoal.recompute"
	ToolChanges      = "shoal.changes"

	// Keep this aligned with ingestSchema's name.maxLength.
	maxUploadFilenameRunes     = 4096
	maxOptionalToolSchemaBytes = 64 << 10
	maxOptionalToolSchemaDepth = 32
	maxOptionalToolProperties  = 256
	maxOptionalToolEnumValues  = 256
	maxToolFailureCodeBytes    = 128
	maxToolFailureMessageBytes = 4096
)

var reservedServiceToolNames = map[string]struct{}{
	ToolDocuments:    {},
	ToolDocument:     {},
	ToolRetrieve:     {},
	ToolNeighborhood: {},
	ToolPath:         {},
	ToolIngest:       {},
	ToolExtract:      {},
	ToolRecompute:    {},
	ToolChanges:      {},
}

var (
	documentsSchema = json.RawMessage(`{
		"type":"object",
		"properties":{
			"snapshot":{"$ref":"#/$defs/snapshot"},
			"page":{
				"type":"object",
				"properties":{
					"limit":{"type":"integer","minimum":0,"maximum":100},
					"cursor":{"type":"string"}
				},
				"additionalProperties":false
			}
		},
		"additionalProperties":false,
		"$defs":{
			"snapshot":{
				"type":"object",
				"properties":{
					"id":{"type":"string"},
					"as_of":{"type":"string","format":"date-time"},
					"frontier":{"type":"string","pattern":"^[0-9]+$"}
				},
				"additionalProperties":false
			}
		}
	}`)
	documentSchema = json.RawMessage(`{
		"type":"object",
		"properties":{
			"snapshot":{"$ref":"#/$defs/snapshot"},
			"document_id":{"type":"string","minLength":1,"maxLength":1366,"pattern":"^[A-Za-z0-9_-]+$","description":"Base64url Shoal document ID"},
			"revision_id":{"type":"string","maxLength":1366,"pattern":"^[A-Za-z0-9_-]*$","description":"Optional base64url revision ID"}
		},
		"required":["document_id"],
		"additionalProperties":false,
		"$defs":{
			"snapshot":{
				"type":"object",
				"properties":{
					"id":{"type":"string"},
					"as_of":{"type":"string","format":"date-time"},
					"frontier":{"type":"string","pattern":"^[0-9]+$"}
				},
				"additionalProperties":false
			}
		}
	}`)
	retrieveSchema = json.RawMessage(`{
		"type":"object",
		"properties":{
			"snapshot":{"$ref":"#/$defs/snapshot"},
			"query":{
				"type":"object",
				"properties":{
					"text":{"type":"string","description":"UTF-8 query; maximum 16384 bytes"},
					"top_k":{"type":"integer","minimum":0,"maximum":50},
					"modes":{
						"type":"array",
						"items":{"enum":["lexical","vector","tree","graph"]}
					},
					"scope":{
						"type":"object",
						"properties":{
							"document_ids":{"type":"array","items":{"type":"string","minLength":1,"maxLength":1366,"pattern":"^[A-Za-z0-9_-]+$"}},
							"node_ids":{"type":"array","items":{"type":"string","minLength":1,"maxLength":1366,"pattern":"^[A-Za-z0-9_-]+$"}}
						},
						"additionalProperties":false
					},
					"as_of":{"type":"string","format":"date-time"},
					"explain":{"type":"boolean"}
				},
				"required":["text"],
				"additionalProperties":false
			}
		},
		"required":["query"],
		"additionalProperties":false,
		"$defs":{
			"snapshot":{
				"type":"object",
				"properties":{
					"id":{"type":"string"},
					"as_of":{"type":"string","format":"date-time"},
					"frontier":{"type":"string","pattern":"^[0-9]+$"}
				},
				"additionalProperties":false
			}
		}
	}`)
	neighborhoodSchema = json.RawMessage(`{
		"type":"object",
		"properties":{
			"snapshot":{"$ref":"#/$defs/snapshot"},
			"node_ids":{"type":"array","items":{"type":"string","minLength":1,"maxLength":1366,"pattern":"^[A-Za-z0-9_-]+$"},"minItems":1},
			"depth":{"type":"integer","minimum":0,"maximum":4},
			"fanout":{"type":"integer","minimum":0,"maximum":50},
			"max_nodes":{"type":"integer","minimum":0,"maximum":250},
			"edge_types":{"type":"array","items":{"type":"string"}},
			"cursor":{"type":"string"}
		},
		"required":["node_ids"],
		"additionalProperties":false,
		"$defs":{
			"snapshot":{
				"type":"object",
				"properties":{
					"id":{"type":"string"},
					"as_of":{"type":"string","format":"date-time"},
					"frontier":{"type":"string","pattern":"^[0-9]+$"}
				},
				"additionalProperties":false
			}
		}
	}`)
	pathSchema = json.RawMessage(`{
		"type":"object",
		"properties":{
			"snapshot":{"$ref":"#/$defs/snapshot"},
			"from":{"type":"string","minLength":1,"maxLength":1366,"pattern":"^[A-Za-z0-9_-]+$"},
			"to":{"type":"string","minLength":1,"maxLength":1366,"pattern":"^[A-Za-z0-9_-]+$"},
			"max_depth":{"type":"integer","minimum":0,"maximum":4},
			"fanout":{"type":"integer","minimum":0,"maximum":50},
			"edge_types":{"type":"array","items":{"type":"string"}}
		},
		"required":["from","to"],
		"additionalProperties":false,
		"$defs":{
			"snapshot":{
				"type":"object",
				"properties":{
					"id":{"type":"string"},
					"as_of":{"type":"string","format":"date-time"},
					"frontier":{"type":"string","pattern":"^[0-9]+$"}
				},
				"additionalProperties":false
			}
		}
	}`)
	ingestSchema = json.RawMessage(`{
		"type":"object",
		"properties":{
			"files":{
				"type":"array",
				"items":{
					"type":"object",
					"properties":{
						"name":{"type":"string","minLength":1,"maxLength":4096},
						"content":{"type":"string","maxLength":1398104,"contentEncoding":"base64"}
					},
					"required":["name","content"],
					"additionalProperties":false
				},
				"minItems":1,
				"maxItems":8
			},
			"declassification_reason":{
				"type":"string",
				"minLength":1,
				"maxLength":128,
				"description":"Optional reason code recorded on the audit of a relabel that removes visibility labels; required by a workspace that requires one. ASCII letters, digits and . _ - : / only"
			}
		},
		"required":["files"],
		"additionalProperties":false
	}`)
	extractSchema = json.RawMessage(`{
		"type":"object",
		"properties":{
			"snapshot":{"$ref":"#/$defs/snapshot"},
			"document_id":{"type":"string","minLength":1,"maxLength":1366,"pattern":"^[A-Za-z0-9_-]+$"},
			"revision_id":{"type":"string","maxLength":1366,"pattern":"^[A-Za-z0-9_-]*$"},
			"instructions":{"type":"string","description":"UTF-8 instructions; maximum 65536 bytes"}
		},
		"required":["document_id"],
		"additionalProperties":false,
		"$defs":{
			"snapshot":{
				"type":"object",
				"properties":{
					"id":{"type":"string"},
					"as_of":{"type":"string","format":"date-time"},
					"frontier":{"type":"string","pattern":"^[0-9]+$"}
				},
				"additionalProperties":false
			}
		}
	}`)
	recomputeSchema = json.RawMessage(`{
		"type":"object",
		"properties":{
			"snapshot":{"$ref":"#/$defs/snapshot"},
			"assertion_id":{"type":"string","minLength":1,"maxLength":1366,"pattern":"^[A-Za-z0-9_-]+$"},
			"digest":{"type":"string"}
		},
		"required":["assertion_id"],
		"additionalProperties":false,
		"$defs":{
			"snapshot":{
				"type":"object",
				"properties":{
					"id":{"type":"string"},
					"as_of":{"type":"string","format":"date-time"},
					"frontier":{"type":"string","pattern":"^[0-9]+$"}
				},
				"additionalProperties":false
			}
		}
	}`)
	changesSchema = json.RawMessage(`{
		"type":"object",
		"properties":{
			"cursor":{
				"type":"string",
				"description":"Opaque authorized change-feed cursor; this is not Snapshot.frontier"
			},
			"limit":{"type":"integer","minimum":0,"maximum":100}
		},
		"additionalProperties":false
	}`)
)

func mandatoryServiceTools(service webapi.Service) []registeredTool {
	return []registeredTool{
		{
			authorizationOperation: auth.OperationList,
			definition: Tool{
				Name: ToolDocuments, Title: "List Shoal documents",
				Description: "List an authorized page of current Shoal documents.",
				InputSchema: cloneJSON(documentsSchema),
				Annotations: readOnlyAnnotations(),
			},
			call: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var request webapi.DocumentsRequest
				if err := decodeToolArguments(raw, &request, ToolDocuments); err != nil {
					return nil, err
				}
				if request.Page.Limit > webapi.MaxPageSize {
					return nil, invalidToolArguments(ToolDocuments)
				}
				return service.Documents(ctx, request)
			},
		},
		{
			authorizationOperation: auth.OperationRead,
			definition: Tool{
				Name: ToolDocument, Title: "Read a Shoal document",
				Description: "Read one authorized immutable Shoal document hierarchy.",
				InputSchema: cloneJSON(documentSchema),
				Annotations: readOnlyAnnotations(),
			},
			call: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var request webapi.DocumentRequest
				if err := decodeToolArguments(raw, &request, ToolDocument); err != nil {
					return nil, err
				}
				if err := shoal.ValidateRequiredID(
					"document ID", request.DocumentID,
				); err != nil {
					return nil, invalidToolArguments(ToolDocument)
				}
				if err := shoal.ValidateOptionalID(
					"revision ID", request.RevisionID,
				); err != nil {
					return nil, invalidToolArguments(ToolDocument)
				}
				return service.Document(ctx, request)
			},
		},
		{
			authorizationOperation: auth.OperationRetrieve,
			definition: Tool{
				Name: ToolRetrieve, Title: "Retrieve Shoal knowledge",
				Description: "Run an authorized bounded retrieval and return structured evidence.",
				InputSchema: cloneJSON(retrieveSchema),
				Annotations: readOnlyAnnotations(),
			},
			call: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var request webapi.RetrievalRequest
				if err := decodeToolArguments(raw, &request, ToolRetrieve); err != nil {
					return nil, err
				}
				if _, err := request.Query.Normalize(); err != nil ||
					request.Query.TopK > webapi.MaxTopK {
					return nil, invalidToolArguments(ToolRetrieve)
				}
				return service.Retrieve(ctx, request)
			},
		},
		{
			authorizationOperation: auth.OperationNeighborhood,
			definition: Tool{
				Name: ToolNeighborhood, Title: "Explore a Shoal neighborhood",
				Description: "Expand an authorized bounded graph neighborhood.",
				InputSchema: cloneJSON(neighborhoodSchema),
				Annotations: readOnlyAnnotations(),
			},
			call: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var request webapi.NeighborhoodRequest
				if err := decodeToolArguments(raw, &request, ToolNeighborhood); err != nil {
					return nil, err
				}
				if err := validateNeighborhoodArguments(request); err != nil {
					return nil, invalidToolArguments(ToolNeighborhood)
				}
				return service.Neighborhood(ctx, request)
			},
		},
		{
			authorizationOperation: auth.OperationNeighborhood,
			definition: Tool{
				Name: ToolPath, Title: "Find a Shoal path",
				Description: "Find one authorized bounded directed explanation path.",
				InputSchema: cloneJSON(pathSchema),
				Annotations: readOnlyAnnotations(),
			},
			call: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var request webapi.PathRequest
				if err := decodeToolArguments(raw, &request, ToolPath); err != nil {
					return nil, err
				}
				if err := validatePathArguments(request); err != nil {
					return nil, invalidToolArguments(ToolPath)
				}
				return service.Path(ctx, request)
			},
		},
	}
}

func optionalServiceTools(service webapi.Service) []registeredTool {
	var tools []registeredTool
	if provider, ok := service.(webapi.IngestProvider); ok && !isAbsent(provider) &&
		ingestionAvailable(service) {
		provider := provider
		tools = append(tools, registeredTool{
			authorizationOperation: auth.OperationIngest,
			definition: Tool{
				Name: ToolIngest, Title: "Ingest Shoal documents",
				Description: "Ingest a bounded batch when the workspace implements ingestion.",
				InputSchema: cloneJSON(ingestSchema),
				Annotations: mutatingAnnotations(true),
			},
			call: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var request webapi.IngestRequest
				if err := decodeToolArguments(raw, &request, ToolIngest); err != nil {
					return nil, err
				}
				if err := validateIngestArguments(request); err != nil {
					return nil, invalidToolArguments(ToolIngest)
				}
				return provider.Ingest(ctx, request)
			},
		})
	}
	if provider, ok := service.(webapi.ExtractionProvider); ok && !isAbsent(provider) &&
		extractionAvailable(service) {
		provider := provider
		tools = append(tools, registeredTool{
			authorizationOperation: auth.OperationIngest,
			definition: Tool{
				Name: ToolExtract, Title: "Extract Shoal knowledge",
				Description: "Run explicit extraction when the workspace implements it.",
				InputSchema: cloneJSON(extractSchema),
				Annotations: mutatingAnnotations(true),
			},
			call: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var request webapi.ExtractRequest
				if err := decodeToolArguments(raw, &request, ToolExtract); err != nil {
					return nil, err
				}
				if err := shoal.ValidateRequiredID(
					"document ID", request.DocumentID,
				); err != nil {
					return nil, invalidToolArguments(ToolExtract)
				}
				if err := shoal.ValidateOptionalID(
					"revision ID", request.RevisionID,
				); err != nil {
					return nil, invalidToolArguments(ToolExtract)
				}
				if uint64(len(request.Instructions)) >
					uint64(ontology.DefaultExtractionLimits().MaxInstructionBytes) {
					return nil, invalidToolArguments(ToolExtract)
				}
				return provider.Extract(ctx, request)
			},
		})
	}
	if provider, ok := service.(webapi.RecomputeProvider); ok && !isAbsent(provider) {
		provider := provider
		tools = append(tools, registeredTool{
			authorizationOperation: auth.OperationNeighborhood,
			definition: Tool{
				Name: ToolRecompute, Title: "Recompute a Shoal derivation",
				Description: "Recompute derivation evidence when the workspace implements it.",
				InputSchema: cloneJSON(recomputeSchema),
				Annotations: readOnlyAnnotations(),
			},
			call: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var request webapi.RecomputeDerivationRequest
				if err := decodeToolArguments(raw, &request, ToolRecompute); err != nil {
					return nil, err
				}
				if err := shoal.ValidateRequiredID(
					"assertion ID", request.AssertionID,
				); err != nil {
					return nil, invalidToolArguments(ToolRecompute)
				}
				return provider.Recompute(ctx, request)
			},
		})
	}
	if provider, ok := service.(webapi.ChangeProvider); ok && !isAbsent(provider) &&
		changesAvailable(service) {
		provider := provider
		tools = append(tools, registeredTool{
			authorizationOperation: auth.OperationList,
			definition: Tool{
				Name: ToolChanges, Title: "Read Shoal document changes",
				Description: "Read the authorized resumable document-publication feed. " +
					"Its opaque cursor is independent of the snapshot content frontier.",
				InputSchema: cloneJSON(changesSchema),
				Annotations: readOnlyAnnotations(),
			},
			call: func(ctx context.Context, raw json.RawMessage) (any, error) {
				var request webapi.ChangesRequest
				if err := decodeToolArguments(raw, &request, ToolChanges); err != nil {
					return nil, err
				}
				if request.Limit > webapi.MaxChangePageSize {
					return nil, invalidToolArguments(ToolChanges)
				}
				return provider.Changes(ctx, request)
			},
		})
	}
	return tools
}

func ingestionAvailable(service webapi.Service) bool {
	availability, ok := service.(webapi.IngestAvailabilityProvider)
	return !ok || (!isAbsent(availability) && availability.IngestAvailable())
}

func extractionAvailable(service webapi.Service) bool {
	availability, ok := service.(webapi.ExtractionAvailabilityProvider)
	return !ok || (!isAbsent(availability) && availability.ExtractionAvailable())
}

func changesAvailable(service webapi.Service) bool {
	availability, ok := service.(webapi.ChangeAvailabilityProvider)
	return !ok || (!isAbsent(availability) && availability.ChangesAvailable())
}

func readOnlyAnnotations() *ToolAnnotations {
	return &ToolAnnotations{
		ReadOnlyHint:    boolHint(true),
		DestructiveHint: boolHint(false),
		IdempotentHint:  boolHint(true),
		OpenWorldHint:   boolHint(false),
	}
}

func mutatingAnnotations(idempotent bool) *ToolAnnotations {
	return &ToolAnnotations{
		ReadOnlyHint:    boolHint(false),
		DestructiveHint: boolHint(true),
		IdempotentHint:  boolHint(idempotent),
		OpenWorldHint:   boolHint(false),
	}
}

func boolHint(value bool) *bool {
	return &value
}

func decodeToolArguments(
	raw json.RawMessage,
	value any,
	toolName string,
) error {
	if err := strictDecode(raw, value); err != nil {
		return invalidToolArguments(toolName)
	}
	return nil
}

type preEffectToolError struct {
	err error
}

func (e preEffectToolError) Error() string { return e.err.Error() }
func (e preEffectToolError) Unwrap() error { return e.err }

func invalidToolArguments(toolName string) error {
	return preEffectToolError{err: shoal.NewError(
		shoal.ErrorInvalidArgument, toolName+" arguments are invalid")}
}

func isPreEffectToolError(err error) bool {
	var target preEffectToolError
	return errors.As(err, &target)
}

func validateNeighborhoodArguments(request webapi.NeighborhoodRequest) error {
	normalized, err := (explorer.NeighborhoodRequest{
		NodeIDs:   request.NodeIDs,
		Depth:     request.Depth,
		EdgeTypes: request.EdgeTypes,
	}).Normalize()
	if err != nil {
		return err
	}
	if len(request.NodeIDs) == 0 ||
		request.Depth > webapi.MaxDepth ||
		request.Fanout > webapi.MaxFanout ||
		request.MaxNodes > webapi.MaxNodes ||
		uint32(len(normalized.EdgeTypes)) > webapi.MaxEdgeTypes {
		return errors.New("invalid neighborhood bounds")
	}
	maxNodes := request.MaxNodes
	if maxNodes == 0 {
		maxNodes = webapi.DefaultMaxNodes
	}
	if uint32(len(normalized.NodeIDs)) > maxNodes {
		return errors.New("too many neighborhood seeds")
	}
	return nil
}

func validatePathArguments(request webapi.PathRequest) error {
	if err := shoal.ValidateRequiredID("path source ID", request.From); err != nil {
		return err
	}
	if err := shoal.ValidateRequiredID("path target ID", request.To); err != nil {
		return err
	}
	normalized, err := (explorer.NeighborhoodRequest{
		NodeIDs:   []shoal.ID{request.From},
		Depth:     request.MaxDepth,
		EdgeTypes: request.EdgeTypes,
	}).Normalize()
	if err != nil {
		return err
	}
	if request.MaxDepth > webapi.MaxDepth ||
		request.Fanout > webapi.MaxFanout ||
		uint32(len(normalized.EdgeTypes)) > webapi.MaxEdgeTypes {
		return errors.New("invalid path bounds")
	}
	return nil
}

func validateIngestArguments(request webapi.IngestRequest) error {
	// Checked before the effect, so an invalid reason is an argument error
	// rather than an indeterminate mutating failure. The rule is the
	// authorized client's own; an empty reason means none.
	if request.DeclassificationReason != "" {
		if err := authorized.ValidateDeclassificationReason(
			request.DeclassificationReason); err != nil {
			return err
		}
	}
	if len(request.Files) == 0 ||
		uint32(len(request.Files)) > webapi.MaxUploadFiles {
		return errors.New("invalid upload file count")
	}
	var total uint64
	for _, file := range request.Files {
		size := uint64(len(file.Content))
		if strings.TrimSpace(file.Name) == "" ||
			utf8.RuneCountInString(file.Name) > maxUploadFilenameRunes ||
			size > webapi.MaxUploadFileBytes {
			return errors.New("invalid upload file")
		}
		total += size
		if total > webapi.MaxUploadTotalBytes {
			return errors.New("upload exceeds total bound")
		}
	}
	return nil
}

func (s *Server) toolSuccessResult(
	id json.RawMessage, value any,
) (ToolResult, error) {
	return s.toolSuccessResultWithBudgets(
		id, value, s.contextBudget, s.outputBudget)
}

func (s *Server) toolSuccessResultWithBudgets(
	id json.RawMessage,
	value any,
	contextBudget int,
	outputBudget uint64,
) (ToolResult, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ToolResult{}, shoal.NewError(
			shoal.ErrorInternal, "tool result could not be encoded")
	}
	trimmed := bytes.TrimSpace(encoded)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return ToolResult{}, shoal.NewError(
			shoal.ErrorInternal, "tool result must be a JSON object")
	}
	if uint64(len(encoded)) > outputBudget {
		return ToolResult{}, shoal.NewError(
			shoal.ErrorUnavailable, "tool result exceeds the effective output bound")
	}
	result, err := s.packToolResultWithBudget(encoded, contextBudget)
	if err != nil {
		result = ToolResult{
			Content:           []TextContent{},
			StructuredContent: append(json.RawMessage(nil), encoded...),
			IsError:           false,
		}
	}
	if s.toolResultFitsBudgetWithLimit(id, result, outputBudget) {
		return result, nil
	}
	result.Content = []TextContent{}
	if s.toolResultFitsBudgetWithLimit(id, result, outputBudget) {
		return result, nil
	}
	return ToolResult{}, shoal.NewError(
		shoal.ErrorUnavailable,
		"tool result exceeds the effective output bound",
	)
}

func (s *Server) toolResultFitsBudget(
	id json.RawMessage, result ToolResult,
) bool {
	return s.toolResultFitsBudgetWithLimit(id, result, s.outputBudget)
}

func (s *Server) toolResultFitsBudgetWithLimit(
	id json.RawMessage,
	result ToolResult,
	outputBudget uint64,
) bool {
	response := newResponse(id, result)
	encoded, err := json.Marshal(response)
	return err == nil && uint64(len(encoded)+1) <= outputBudget
}

type structuredToolFailure struct {
	Error toolFailure `json:"error"`
}

type toolFailure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (s *Server) toolErrorResult(err error) ToolResult {
	failure := boundedToolFailure(publicToolFailure(err))
	encoded, marshalErr := json.Marshal(structuredToolFailure{Error: failure})
	if marshalErr != nil || uint64(len(encoded)) > webapi.MaxResponseBytes {
		encoded = []byte(`{"error":{"code":"internal","message":"tool execution failed"}}`)
	}
	// Errors are never context-compressed. Their complete structured and text
	// forms are required so budget pressure cannot turn failure into
	// success-shaped or missing content.
	return ToolResult{
		Content:           []TextContent{{Type: "text", Text: string(encoded)}},
		StructuredContent: append(json.RawMessage(nil), encoded...),
		IsError:           true,
	}
}

func boundedToolFailure(failure toolFailure) toolFailure {
	if !utf8.ValidString(failure.Code) ||
		!utf8.ValidString(failure.Message) ||
		len(failure.Code) > maxToolFailureCodeBytes ||
		len(failure.Message) > maxToolFailureMessageBytes {
		return toolFailure{
			Code: string(shoal.ErrorInternal), Message: "tool execution failed"}
	}
	return failure
}

func (s *Server) packToolResult(encoded []byte) (ToolResult, error) {
	return s.packToolResultWithBudget(encoded, s.contextBudget)
}

func (s *Server) packToolResultWithBudget(
	encoded []byte,
	contextBudget int,
) (ToolResult, error) {
	if s == nil || isAbsent(s.compressor) || contextBudget < 0 {
		return ToolResult{}, shoal.NewError(
			shoal.ErrorInternal, "context compression is unavailable")
	}
	compressed, err := s.compressor.CompressContext(CompressionInput{
		BudgetBytes: contextBudget,
		Items: []CompressionItem{{
			ID:       "tool-result",
			Sequence: 1,
			Content: ContextContent{
				Type: ContextContentJSON,
				Data: string(encoded),
			},
		}},
	})
	if err != nil {
		return ToolResult{}, shoal.WrapError(
			shoal.ErrorInternal, "compress tool result", err)
	}
	if err := validatePackedToolResult(compressed, encoded, contextBudget); err != nil {
		return ToolResult{}, err
	}
	content := make([]TextContent, 0, 1)
	if !compressed.Items[0].Omitted {
		content = append(content, TextContent{
			Type: "text",
			Text: compressed.Items[0].Content.Data,
		})
	}
	return ToolResult{
		Content:           content,
		StructuredContent: append(json.RawMessage(nil), encoded...),
		IsError:           false,
	}, nil
}

func validatePackedToolResult(
	compressed CompressionOutput,
	encoded []byte,
	budget int,
) error {
	if len(compressed.Items) != 1 {
		return shoal.NewError(
			shoal.ErrorInternal, "context compressor returned an invalid result")
	}
	item := compressed.Items[0]
	if item.ID != "tool-result" || item.Sequence != 1 ||
		item.Required || item.IsError ||
		len(item.Visibility) != 0 ||
		len(item.RetrievedSourceIDs) != 0 ||
		len(item.CitedSourceIDs) != 0 ||
		len(compressed.Sources) != 0 ||
		len(compressed.RetrievedSourceIDs) != 0 ||
		len(compressed.CitedSourceIDs) != 0 ||
		len(compressed.Visibility) != 0 {
		return shoal.NewError(
			shoal.ErrorInternal, "context compressor changed tool result semantics")
	}
	if compressed.BudgetBytes != budget ||
		compressed.InputBytes != len(encoded) ||
		compressed.WasCompressed != item.Omitted {
		return shoal.NewError(
			shoal.ErrorInternal, "context compressor returned invalid accounting")
	}
	if item.Omitted {
		if item.Content.Type != "" || item.Content.Data != "" ||
			compressed.OutputBytes != 0 ||
			len(compressed.OmittedItemIDs) != 1 ||
			compressed.OmittedItemIDs[0] != "tool-result" {
			return shoal.NewError(
				shoal.ErrorInternal, "context compressor returned invalid omitted content")
		}
		return nil
	}
	if item.Content.Type != ContextContentJSON ||
		item.Content.Data != string(encoded) ||
		compressed.OutputBytes != len(encoded) ||
		len(compressed.OmittedItemIDs) != 0 {
		return shoal.NewError(
			shoal.ErrorInternal, "context compressor changed tool result content")
	}
	return nil
}

func publicToolFailure(err error) toolFailure {
	if explorer.IsIndeterminateCommit(err) {
		return toolFailure{
			Code: "indeterminate",
			Message: "tool effect may have committed; verify current state " +
				"before deciding whether a retry is safe",
		}
	}
	if errors.Is(err, context.Canceled) {
		return toolFailure{
			Code: string(shoal.ErrorCanceled), Message: "tool execution canceled"}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return toolFailure{
			Code: string(shoal.ErrorDeadline), Message: "tool execution deadline exceeded"}
	}
	var public *shoal.Error
	if errors.As(err, &public) && public != nil {
		switch public.Code {
		case shoal.ErrorUnauthorized:
			return toolFailure{
				Code: string(public.Code), Message: "authorization denied"}
		case shoal.ErrorInternal:
			return toolFailure{
				Code: string(public.Code), Message: "tool execution failed"}
		default:
			message := strings.TrimSpace(public.Message)
			if message == "" {
				message = string(public.Code)
			}
			return toolFailure{Code: string(public.Code), Message: message}
		}
	}
	return toolFailure{
		Code: string(shoal.ErrorInternal), Message: "tool execution failed"}
}

func validateTool(tool Tool) (*optionalToolInputSchema, error) {
	if len(tool.Name) == 0 || len(tool.Name) > 128 {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "optional MCP tool name is invalid")
	}
	for index := 0; index < len(tool.Name); index++ {
		character := tool.Name[index]
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '_' || character == '-' || character == '.' {
			continue
		}
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "optional MCP tool name is invalid")
	}
	if strings.TrimSpace(tool.Description) == "" {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "optional MCP tool description is required")
	}
	inputSchema, err := parseOptionalToolInputSchema(tool.InputSchema)
	if err != nil {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "optional MCP tool input schema is invalid")
	}
	if len(tool.OutputSchema) != 0 {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"optional MCP tool output schemas are not supported")
	}
	if tool.Execution != nil && tool.Execution.TaskSupport != "forbidden" {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"optional MCP tool task execution is not supported")
	}
	return &inputSchema, nil
}

// optionalToolInputSchema is the deliberately limited JSON Schema subset that
// extension providers may advertise. Strict decoding rejects unsupported
// keywords instead of forwarding a schema the server cannot validate.
type optionalToolInputSchema struct {
	Type                 string                             `json:"type"`
	Title                string                             `json:"title,omitempty"`
	Description          string                             `json:"description,omitempty"`
	Properties           map[string]optionalToolInputSchema `json:"properties,omitempty"`
	Required             []string                           `json:"required,omitempty"`
	AdditionalProperties *bool                              `json:"additionalProperties,omitempty"`
	Items                *optionalToolInputSchema           `json:"items,omitempty"`
	Enum                 []json.RawMessage                  `json:"enum,omitempty"`
	Minimum              *json.Number                       `json:"minimum,omitempty"`
	Maximum              *json.Number                       `json:"maximum,omitempty"`
	ExclusiveMaximum     *json.Number                       `json:"exclusiveMaximum,omitempty"`
	MinLength            *int                               `json:"minLength,omitempty"`
	MaxLength            *int                               `json:"maxLength,omitempty"`
	MinItems             *int                               `json:"minItems,omitempty"`
	MaxItems             *int                               `json:"maxItems,omitempty"`
}

func parseOptionalToolInputSchema(
	raw json.RawMessage,
) (optionalToolInputSchema, error) {
	var zero optionalToolInputSchema
	if len(raw) > maxOptionalToolSchemaBytes {
		return zero, errors.New("schema exceeds size limit")
	}
	var schema optionalToolInputSchema
	if err := strictDecode(raw, &schema); err != nil {
		return zero, err
	}
	if schema.Type != "object" {
		return zero, errors.New("top-level schema must describe an object")
	}
	if err := validateOptionalToolSchemaNode(schema, 1); err != nil {
		return zero, err
	}
	return schema, nil
}

func validateOptionalToolSchemaNode(
	schema optionalToolInputSchema,
	depth int,
) error {
	if depth > maxOptionalToolSchemaDepth ||
		len(schema.Properties) > maxOptionalToolProperties ||
		len(schema.Required) > maxOptionalToolProperties ||
		len(schema.Enum) > maxOptionalToolEnumValues {
		return errors.New("schema exceeds structural limits")
	}
	switch schema.Type {
	case "":
		if len(schema.Properties) != 0 || len(schema.Required) != 0 ||
			schema.AdditionalProperties != nil || schema.Items != nil ||
			schema.Enum != nil || hasScalarSchemaBounds(schema) {
			return errors.New("untyped schema has validation keywords")
		}
		return nil
	case "object":
		if schema.Items != nil || hasScalarSchemaBounds(schema) {
			return errors.New("object schema has incompatible keywords")
		}
		for name, property := range schema.Properties {
			if strings.TrimSpace(name) == "" {
				return errors.New("object schema has an empty property name")
			}
			if err := validateOptionalToolSchemaNode(property, depth+1); err != nil {
				return err
			}
		}
		required := make(map[string]struct{}, len(schema.Required))
		for _, name := range schema.Required {
			if _, ok := schema.Properties[name]; !ok {
				return errors.New("required property is not declared")
			}
			if _, duplicate := required[name]; duplicate {
				return errors.New("required property is duplicated")
			}
			required[name] = struct{}{}
		}
	case "array":
		if schema.Items == nil || len(schema.Properties) != 0 ||
			len(schema.Required) != 0 || schema.AdditionalProperties != nil ||
			hasStringOrNumberSchemaBounds(schema) {
			return errors.New("array schema has incompatible keywords")
		}
		if err := validateOptionalToolSchemaNode(*schema.Items, depth+1); err != nil {
			return err
		}
		if err := validateNonnegativeBounds(schema.MinItems, schema.MaxItems); err != nil {
			return err
		}
	case "string":
		if schema.Items != nil || len(schema.Properties) != 0 ||
			len(schema.Required) != 0 || schema.AdditionalProperties != nil ||
			schema.Minimum != nil || schema.Maximum != nil ||
			schema.ExclusiveMaximum != nil ||
			schema.MinItems != nil || schema.MaxItems != nil {
			return errors.New("string schema has incompatible keywords")
		}
		if err := validateNonnegativeBounds(schema.MinLength, schema.MaxLength); err != nil {
			return err
		}
	case "integer", "number":
		if schema.Items != nil || len(schema.Properties) != 0 ||
			len(schema.Required) != 0 || schema.AdditionalProperties != nil ||
			schema.MinLength != nil || schema.MaxLength != nil ||
			schema.MinItems != nil || schema.MaxItems != nil {
			return errors.New("numeric schema has incompatible keywords")
		}
		if schema.Maximum != nil && schema.ExclusiveMaximum != nil {
			return errors.New("numeric schema has conflicting upper bounds")
		}
		if schema.Minimum != nil &&
			(schema.Maximum != nil || schema.ExclusiveMaximum != nil) {
			minimum, err := jsonNumberRat(*schema.Minimum)
			if err != nil {
				return err
			}
			upper := schema.Maximum
			exclusive := false
			if upper == nil {
				upper = schema.ExclusiveMaximum
				exclusive = true
			}
			maximum, err := jsonNumberRat(*upper)
			if err != nil {
				return err
			}
			comparison := minimum.Cmp(maximum)
			if comparison > 0 || exclusive && comparison == 0 {
				return errors.New("numeric schema bounds are inverted")
			}
		}
	case "boolean":
		if schema.Items != nil || len(schema.Properties) != 0 ||
			len(schema.Required) != 0 || schema.AdditionalProperties != nil ||
			hasScalarSchemaBounds(schema) {
			return errors.New("boolean schema has incompatible keywords")
		}
	default:
		return errors.New("schema type is unsupported")
	}
	if schema.Enum != nil && len(schema.Enum) == 0 {
		return errors.New("schema enum is empty")
	}
	if schema.Enum != nil {
		withoutEnum := schema
		withoutEnum.Enum = nil
		for _, encoded := range schema.Enum {
			value, err := decodeOptionalToolValue(encoded)
			if err != nil {
				return err
			}
			if err := validateOptionalToolValue(value, withoutEnum); err != nil {
				return errors.New("schema enum value is invalid")
			}
		}
	}
	return nil
}

func hasScalarSchemaBounds(schema optionalToolInputSchema) bool {
	return hasStringOrNumberSchemaBounds(schema) ||
		schema.MinItems != nil || schema.MaxItems != nil
}

func hasStringOrNumberSchemaBounds(schema optionalToolInputSchema) bool {
	return schema.Minimum != nil || schema.Maximum != nil ||
		schema.ExclusiveMaximum != nil ||
		schema.MinLength != nil || schema.MaxLength != nil
}

func validateNonnegativeBounds(minimum *int, maximum *int) error {
	if (minimum != nil && *minimum < 0) ||
		(maximum != nil && *maximum < 0) ||
		(minimum != nil && maximum != nil && *minimum > *maximum) {
		return errors.New("schema bounds are invalid")
	}
	return nil
}

func validateOptionalToolArguments(
	raw json.RawMessage,
	schema optionalToolInputSchema,
) error {
	value, err := decodeOptionalToolValue(raw)
	if err != nil {
		return err
	}
	return validateOptionalToolValue(value, schema)
}

func decodeOptionalToolValue(raw json.RawMessage) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("multiple JSON values")
		}
		return nil, err
	}
	return value, nil
}

func validateOptionalToolValue(value any, schema optionalToolInputSchema) error {
	if schema.Enum != nil {
		matched := false
		for _, encoded := range schema.Enum {
			allowed, err := decodeOptionalToolValue(encoded)
			if err != nil {
				return err
			}
			if optionalToolValuesEqual(value, allowed) {
				matched = true
				break
			}
		}
		if !matched {
			return errors.New("value is outside the schema enum")
		}
	}
	switch schema.Type {
	case "":
		return nil
	case "object":
		object, ok := value.(map[string]any)
		if !ok {
			return errors.New("value is not an object")
		}
		for _, name := range schema.Required {
			if _, ok := object[name]; !ok {
				return errors.New("required property is missing")
			}
		}
		for name, item := range object {
			property, ok := schema.Properties[name]
			if !ok {
				if schema.AdditionalProperties != nil &&
					!*schema.AdditionalProperties {
					return errors.New("additional property is not allowed")
				}
				continue
			}
			if err := validateOptionalToolValue(item, property); err != nil {
				return err
			}
		}
	case "array":
		array, ok := value.([]any)
		if !ok {
			return errors.New("value is not an array")
		}
		if schema.MinItems != nil && len(array) < *schema.MinItems ||
			schema.MaxItems != nil && len(array) > *schema.MaxItems {
			return errors.New("array length is outside schema bounds")
		}
		for _, item := range array {
			if err := validateOptionalToolValue(item, *schema.Items); err != nil {
				return err
			}
		}
	case "string":
		text, ok := value.(string)
		if !ok {
			return errors.New("value is not a string")
		}
		length := utf8.RuneCountInString(text)
		if schema.MinLength != nil && length < *schema.MinLength ||
			schema.MaxLength != nil && length > *schema.MaxLength {
			return errors.New("string length is outside schema bounds")
		}
	case "integer", "number":
		number, ok := value.(json.Number)
		if !ok {
			return errors.New("value is not a number")
		}
		rational, err := jsonNumberRat(number)
		if err != nil {
			return err
		}
		if schema.Type == "integer" && !rational.IsInt() {
			return errors.New("value is not an integer")
		}
		if schema.Minimum != nil {
			minimum, err := jsonNumberRat(*schema.Minimum)
			if err != nil {
				return err
			}
			if rational.Cmp(minimum) < 0 {
				return errors.New("number is below schema minimum")
			}
		}
		if schema.Maximum != nil {
			maximum, err := jsonNumberRat(*schema.Maximum)
			if err != nil {
				return err
			}
			if rational.Cmp(maximum) > 0 {
				return errors.New("number is above schema maximum")
			}
		}
		if schema.ExclusiveMaximum != nil {
			maximum, err := jsonNumberRat(*schema.ExclusiveMaximum)
			if err != nil {
				return err
			}
			if rational.Cmp(maximum) >= 0 {
				return errors.New("number is at or above schema exclusive maximum")
			}
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return errors.New("value is not a boolean")
		}
	default:
		return errors.New("schema type is unsupported")
	}
	return nil
}

func optionalToolValuesEqual(left any, right any) bool {
	leftNumber, leftIsNumber := left.(json.Number)
	rightNumber, rightIsNumber := right.(json.Number)
	if leftIsNumber || rightIsNumber {
		if !leftIsNumber || !rightIsNumber {
			return false
		}
		leftRational, leftErr := jsonNumberRat(leftNumber)
		rightRational, rightErr := jsonNumberRat(rightNumber)
		return leftErr == nil && rightErr == nil &&
			leftRational.Cmp(rightRational) == 0
	}
	leftObject, leftIsObject := left.(map[string]any)
	rightObject, rightIsObject := right.(map[string]any)
	if leftIsObject || rightIsObject {
		if !leftIsObject || !rightIsObject || len(leftObject) != len(rightObject) {
			return false
		}
		for key, leftValue := range leftObject {
			rightValue, ok := rightObject[key]
			if !ok || !optionalToolValuesEqual(leftValue, rightValue) {
				return false
			}
		}
		return true
	}
	leftArray, leftIsArray := left.([]any)
	rightArray, rightIsArray := right.([]any)
	if leftIsArray || rightIsArray {
		if !leftIsArray || !rightIsArray || len(leftArray) != len(rightArray) {
			return false
		}
		for index := range leftArray {
			if !optionalToolValuesEqual(leftArray[index], rightArray[index]) {
				return false
			}
		}
		return true
	}
	return left == right
}

func jsonNumberRat(number json.Number) (*big.Rat, error) {
	rational, ok := new(big.Rat).SetString(number.String())
	if !ok {
		return nil, errors.New("invalid JSON number")
	}
	return rational, nil
}

func cloneTool(tool Tool) Tool {
	cloned := tool
	cloned.InputSchema = cloneJSON(tool.InputSchema)
	cloned.OutputSchema = cloneJSON(tool.OutputSchema)
	cloned.Icons = append([]Icon(nil), tool.Icons...)
	for index := range cloned.Icons {
		cloned.Icons[index].Sizes = append([]string(nil), tool.Icons[index].Sizes...)
	}
	if tool.Annotations != nil {
		annotations := *tool.Annotations
		annotations.ReadOnlyHint = cloneBool(tool.Annotations.ReadOnlyHint)
		annotations.DestructiveHint = cloneBool(tool.Annotations.DestructiveHint)
		annotations.IdempotentHint = cloneBool(tool.Annotations.IdempotentHint)
		annotations.OpenWorldHint = cloneBool(tool.Annotations.OpenWorldHint)
		cloned.Annotations = &annotations
	}
	if tool.Execution != nil {
		execution := *tool.Execution
		cloned.Execution = &execution
	}
	return cloned
}

func cloneBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneJSON(value json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), value...)
}
