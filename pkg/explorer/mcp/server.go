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
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// ProtocolVersion is the newest initialization-handshake MCP revision this
// server implements. Newer sessionless MCP revisions use a different lifecycle
// and are intentionally not guessed at by this stdio server.
const ProtocolVersion = "2025-11-25"

const (
	defaultServerName    = "shoal"
	defaultServerVersion = "1"
	// DefaultContextBudgetBytes bounds the duplicate text rendering of a
	// structured tool result. StructuredContent remains complete.
	DefaultContextBudgetBytes = 1 << 20
	maxContextBudgetBytes     = int(webapi.MaxResponseBytes)
	// DefaultToolCallsPerMinute bounds work accepted by one server process.
	// HTTP sessions share this limiter so opening new sessions cannot bypass it.
	DefaultToolCallsPerMinute = 120
	MaxToolCallsPerMinute     = 100_000
)

// Implementation identifies an MCP client or server implementation.
type Implementation struct {
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	Icons       []Icon `json:"icons,omitempty"`
	WebsiteURL  string `json:"websiteUrl,omitempty"`
}

// DecisionProvider resolves trusted caller identity for one tool invocation.
// The server invokes it for every tools/call request, then constructs a new
// auth.Decision with a server-generated RequestID before binding that decision
// through Config.Authority. Providers must not derive grants from untrusted
// tool arguments.
type DecisionProvider interface {
	Decision(context.Context) (auth.Decision, error)
}

// DecisionProviderFunc adapts a function to DecisionProvider.
type DecisionProviderFunc func(context.Context) (auth.Decision, error)

// Decision calls the trusted provider function.
func (f DecisionProviderFunc) Decision(ctx context.Context) (auth.Decision, error) {
	if f == nil {
		return auth.Decision{}, shoal.NewError(
			shoal.ErrorUnauthorized, "authorization denied")
	}
	return f(ctx)
}

// OptionalToolProvider is the extension seam for optional MCP capabilities. A
// provider is advertised only when it is supplied at construction.
//
// Call receives the same freshly authorized context as built-in tools. The
// returned value must encode as a JSON object so it can be carried as MCP
// structuredContent. InputSchema is restricted to the validated subset in
// parseOptionalToolInputSchema. Optional tools must omit outputSchema until
// this server can validate results against it, and execution must be nil or
// explicitly forbid task mode.
type OptionalToolProvider interface {
	Tool() Tool
	Call(context.Context, json.RawMessage) (any, error)
}

// SnapshotProvider returns the exact authorized corpus snapshot observed by a
// tool call. The authorized Explorer client implements this interface.
type SnapshotProvider interface {
	Snapshot(context.Context) (explorer.Snapshot, error)
}

// ToolObservation carries the complete provenance exposed by one tool result.
// The node and evidence sets are never presentation-capped. Optional snapshot
// and authorization pins let adapters prove that an already-recorded provider
// response was produced under the same bound request.
type ToolObservation struct {
	SnapshotID               shoal.ID
	SnapshotAsOf             time.Time
	AuthorizationFingerprint shoal.ID
	AuthorizationExpiresAt   time.Time
	RequestID                shoal.ID
	EmbeddingSpaceID         shoal.ID
	EmbeddingSpaceIDs        []shoal.ID
	RetrievedNodeIDs         []shoal.ID
	RetrievedEvidence        []interaction.EvidenceReference
	CitedNodeIDs             []shoal.ID
	CitedEvidence            []interaction.EvidenceReference
	RequiredVisibility       []string
}

// OptionalToolObservationProvider lets an extension report every source and
// complete evidence reference its full structured result exposed.
type OptionalToolObservationProvider interface {
	ObserveToolResult(any) (ToolObservation, error)
}

// OptionalToolAuthorizationProvider declares the exact canonical auth
// operation an extension performs. The dispatcher enforces its collection/
// domain gate before calling the provider; target-bound providers must also
// use Decision.AuthorizeObject through their existing capability seam.
type OptionalToolAuthorizationProvider interface {
	ToolAuthorizationOperation() auth.Operation
}

// Config constructs one transport-neutral MCP dispatcher with one stdio
// protocol session. HTTP sessions clone only the lifecycle state; tools,
// recorder, authorization capabilities, and limits remain shared.
type Config struct {
	Service   webapi.Service
	Authority *auth.Authority
	Decisions DecisionProvider
	Recorder  *interaction.Recorder
	// InteractionSink supports HTTP, where no process-global principal exists
	// at construction. Each tool call creates and verifies a recorder against
	// this sink only after that HTTP request has been authenticated and bound.
	InteractionSink interaction.ResultSink
	Snapshots       SnapshotProvider
	// WorkspaceSettings applies an HTTP-selected workspace under each tool's
	// exact authorization operation. Stdio requests never carry a workspace.
	WorkspaceSettings webapi.WorkspaceSettingsApplier
	ServerInfo        Implementation
	OptionalTools     []OptionalToolProvider
	Instructions      string
	// ContextCompressor controls the compatibility text rendering of structured
	// results. Nil selects NativeContextCompressor.
	ContextCompressor ContextCompressor
	// ContextBudgetBytes defaults to DefaultContextBudgetBytes and cannot exceed
	// the web API's public response bound.
	ContextBudgetBytes int
	// OutputBudgetBytes defaults to the web API's public response bound.
	OutputBudgetBytes uint64
	// ToolCallsPerMinute defaults to DefaultToolCallsPerMinute.
	ToolCallsPerMinute int
	requestIDFactory   func() (shoal.ID, error)
	toolCallClock      func() time.Time
	interactionClock   func() time.Time
}

type serverState uint8

const (
	stateAwaitInitialize serverState = iota
	stateAwaitInitialized
	stateReady
)

// Server implements the initialization-handshake MCP lifecycle over
// newline-delimited JSON-RPC.
type Server struct {
	binder            auth.Binder
	resolver          auth.Resolver
	decisions         DecisionProvider
	serverInfo        Implementation
	instructions      string
	requestID         func() (shoal.ID, error)
	compressor        ContextCompressor
	contextBudget     int
	outputBudget      uint64
	toolCallLimit     *fixedWindowLimiter
	recorder          *interaction.Recorder
	interactionSink   interaction.ResultSink
	snapshots         SnapshotProvider
	workspaceSettings webapi.WorkspaceSettingsApplier
	interactionNow    func() time.Time
	tools             []registeredTool
	toolsByName       map[string]registeredTool
	stateMu           sync.Mutex
	state             serverState
}

type registeredTool struct {
	definition             Tool
	inputSchema            *optionalToolInputSchema
	authorizationOperation auth.Operation
	call                   func(context.Context, json.RawMessage) (any, error)
	observe                func(any) (ToolObservation, error)
}

// NewServer validates and snapshots the available tool surface. Optional tools
// cannot appear later without constructing a new server, so listChanged is
// truthfully advertised as false.
func NewServer(config Config) (*Server, error) {
	if isAbsent(config.Service) {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "MCP workspace service is required")
	}
	if config.Authority == nil {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "MCP authorization authority is required")
	}
	if isAbsent(config.Decisions) {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "MCP decision provider is required")
	}
	if config.Recorder == nil && isAbsent(config.InteractionSink) {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"MCP interaction recorder or authorized sink is required")
	}
	if isAbsent(config.Snapshots) {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "MCP snapshot provider is required")
	}
	serverInfo := cloneImplementation(config.ServerInfo)
	if serverInfo.Name == "" {
		serverInfo.Name = defaultServerName
	}
	if serverInfo.Version == "" {
		serverInfo.Version = defaultServerVersion
	}
	if strings.TrimSpace(serverInfo.Name) == "" ||
		strings.TrimSpace(serverInfo.Version) == "" {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "MCP server name and version are required")
	}
	requestID := config.requestIDFactory
	if requestID == nil {
		requestID = randomRequestID
	}
	compressor := config.ContextCompressor
	if compressor == nil {
		compressor = NativeContextCompressor{}
	} else if isAbsent(compressor) {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "MCP context compressor is required")
	}
	contextBudget := config.ContextBudgetBytes
	if contextBudget == 0 {
		contextBudget = DefaultContextBudgetBytes
	}
	if contextBudget < 0 || contextBudget > maxContextBudgetBytes {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "MCP context budget is invalid")
	}
	outputBudget := config.OutputBudgetBytes
	if outputBudget == 0 {
		outputBudget = webapi.MaxResponseBytes
	}
	if outputBudget > webapi.MaxResponseBytes {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"MCP output budget exceeds the public response bound",
		)
	}
	toolCallsPerMinute := config.ToolCallsPerMinute
	if toolCallsPerMinute == 0 {
		toolCallsPerMinute = DefaultToolCallsPerMinute
	}
	if toolCallsPerMinute < 0 ||
		toolCallsPerMinute > MaxToolCallsPerMinute {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "MCP tool call rate limit is invalid")
	}
	toolCallClock := config.toolCallClock
	if toolCallClock == nil {
		toolCallClock = time.Now
	}
	interactionClock := config.interactionClock
	if interactionClock == nil {
		interactionClock = time.Now
	}
	tools, err := serviceTools(config.Service, config.OptionalTools)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]registeredTool, len(tools))
	for _, tool := range tools {
		byName[tool.definition.Name] = tool
	}
	instructions := strings.TrimSpace(config.Instructions)
	if instructions != "" {
		instructions += " "
	}
	instructions += "Shoal tools are authorized independently and durably recorded for every call. " +
		"Context compression is applied to large compatibility text results and is distinct " +
		"from Shoal provenance fold. Generic tool-call records do not create inference nodes."
	return &Server{
		binder: config.Authority.Binder(), resolver: config.Authority.Resolver(),
		decisions:    config.Decisions,
		serverInfo:   serverInfo,
		instructions: instructions, requestID: requestID,
		compressor: compressor, contextBudget: contextBudget,
		outputBudget:      outputBudget,
		recorder:          config.Recorder,
		interactionSink:   config.InteractionSink,
		snapshots:         config.Snapshots,
		workspaceSettings: config.WorkspaceSettings,
		interactionNow:    interactionClock,
		toolCallLimit: newFixedWindowLimiter(
			toolCallsPerMinute, time.Minute, toolCallClock),
		tools: tools, toolsByName: byName, state: stateAwaitInitialize,
	}, nil
}

// newProtocolSession clones one dispatcher with independent lifecycle state.
// The process-wide rate limiter remains shared so opening sessions cannot
// bypass it. No identity or authorization decision is retained; HTTP
// authenticates and binds every request afresh.
func (s *Server) newProtocolSession() *Server {
	return s.newProtocolSessionWithOutputBudget(0)
}

func (s *Server) newProtocolSessionWithOutputBudget(limit uint64) *Server {
	if s == nil {
		return nil
	}
	contextBudget := s.contextBudget
	outputBudget := s.outputBudget
	if limit > 0 && limit < outputBudget {
		outputBudget = limit
	}
	if uint64(contextBudget) > outputBudget {
		contextBudget = int(outputBudget)
	}
	return &Server{
		binder: s.binder, resolver: s.resolver, decisions: s.decisions,
		serverInfo:   cloneImplementation(s.serverInfo),
		instructions: s.instructions, requestID: s.requestID,
		compressor: s.compressor, contextBudget: contextBudget,
		outputBudget:      outputBudget,
		recorder:          s.recorder,
		interactionSink:   s.interactionSink,
		snapshots:         s.snapshots,
		workspaceSettings: s.workspaceSettings,
		interactionNow:    s.interactionNow,
		toolCallLimit:     s.toolCallLimit,
		tools:             s.tools, toolsByName: s.toolsByName,
		state: stateAwaitInitialize,
	}
}

func cloneImplementation(value Implementation) Implementation {
	cloned := value
	cloned.Icons = append([]Icon(nil), value.Icons...)
	for index := range cloned.Icons {
		cloned.Icons[index].Sizes = append(
			[]string(nil), value.Icons[index].Sizes...)
	}
	return cloned
}

// Serve processes newline-delimited JSON-RPC until input closes, the context is
// canceled between messages, or a transport error prevents continued framing.
func (s *Server) Serve(ctx context.Context, input io.Reader, output io.Writer) error {
	if s == nil {
		return shoal.NewError(shoal.ErrorInvalidArgument, "MCP server is required")
	}
	if ctx == nil {
		return shoal.NewError(shoal.ErrorInvalidArgument, "MCP context is required")
	}
	if isAbsent(input) || isAbsent(output) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "MCP input and output are required")
	}
	codec := newCodec(input, output)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		raw, err := codec.readMessage()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			if errors.Is(err, errMessageTooLarge) {
				writeErr := codec.writeMessage(newErrorResponse(
					json.RawMessage("null"),
					newError(codeInvalidRequest, "message exceeds maximum size"),
				))
				return errors.Join(err, writeErr)
			}
			return err
		}
		request, failure := decodeRequest(raw)
		if failure != nil {
			id, respond := protocolFailureID(raw, failure)
			if respond {
				if err := codec.writeMessage(newErrorResponse(id, failure)); err != nil {
					return err
				}
			}
			continue
		}
		response := s.handle(ctx, request)
		if response == nil {
			continue
		}
		if err := codec.writeMessage(*response); err != nil {
			return err
		}
	}
}

func protocolFailureID(raw []byte, failure *Error) (json.RawMessage, bool) {
	if failure == nil {
		return nil, false
	}
	if failure.Code == codeParseError {
		return json.RawMessage("null"), true
	}
	var envelope struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return json.RawMessage("null"), true
	}
	if len(envelope.ID) == 0 {
		return json.RawMessage("null"), true
	}
	if string(envelope.ID) == "null" {
		return envelope.ID, true
	}
	if !validRequestID(envelope.ID) {
		return json.RawMessage("null"), true
	}
	return envelope.ID, true
}

func (s *Server) handle(ctx context.Context, request Request) *Response {
	if request.isNotification() {
		s.handleNotification(request)
		return nil
	}
	switch request.Method {
	case "initialize":
		return s.initialize(ctx, request)
	case "ping":
		if !validEmptyParams(request.Params) {
			response := newErrorResponse(
				request.ID, newError(codeInvalidParams, "invalid ping params"))
			return &response
		}
		response := newResponse(request.ID, struct{}{})
		return &response
	case "notifications/initialized":
		response := newErrorResponse(
			request.ID, newError(codeInvalidRequest, "initialized must be a notification"))
		return &response
	}
	if !s.ready() {
		response := newErrorResponse(
			request.ID, newError(codeInvalidRequest, "server is not initialized"))
		return &response
	}
	switch request.Method {
	case "tools/list":
		return s.listTools(request)
	case "tools/call":
		return s.callTool(ctx, request)
	default:
		response := newErrorResponse(
			request.ID, newError(codeMethodNotFound, "method not found"))
		return &response
	}
}

func (s *Server) initialize(ctx context.Context, request Request) *Response {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.state != stateAwaitInitialize {
		response := newErrorResponse(
			request.ID, newError(codeInvalidRequest, "server is already initialized"))
		return &response
	}
	var params InitializeParams
	if err := strictDecode(request.Params, &params); err != nil ||
		params.ProtocolVersion == "" ||
		params.Capabilities == nil ||
		!validClientCapabilities(params.Capabilities) ||
		strings.TrimSpace(params.ClientInfo.Name) == "" ||
		strings.TrimSpace(params.ClientInfo.Version) == "" {
		response := newErrorResponse(
			request.ID, newError(codeInvalidParams, "invalid initialize params"))
		return &response
	}
	negotiated := negotiateProtocolVersion(params.ProtocolVersion)
	s.state = stateAwaitInitialized
	response := newResponse(request.ID, InitializeResult{
		ProtocolVersion: negotiated,
		Capabilities: ServerCapabilities{
			Tools: &ToolsCapability{ListChanged: false},
		},
		ServerInfo:   s.serverInfo,
		Instructions: s.instructions,
		Meta:         workspaceInitializeMeta(ctx),
	})
	return &response
}

func negotiateProtocolVersion(requested string) string {
	if requested == ProtocolVersion {
		return requested
	}
	return ProtocolVersion
}

func validClientCapabilities(capabilities map[string]json.RawMessage) bool {
	standard := map[string]struct{}{
		"elicitation":  {},
		"experimental": {},
		"roots":        {},
		"sampling":     {},
		"tasks":        {},
	}
	for name, raw := range capabilities {
		if _, known := standard[name]; !known {
			continue
		}
		var object map[string]json.RawMessage
		if err := strictDecode(raw, &object); err != nil || object == nil {
			return false
		}
	}
	return true
}

func (s *Server) handleNotification(request Request) {
	if request.Method != "notifications/initialized" ||
		!validEmptyParams(request.Params) {
		return
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.state == stateAwaitInitialized {
		s.state = stateReady
	}
}

func (s *Server) ready() bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.state == stateReady
}

func (s *Server) listTools(request Request) *Response {
	var params ListToolsParams
	if err := strictDecodeOptional(request.Params, &params); err != nil ||
		params.Cursor != "" {
		response := newErrorResponse(
			request.ID, newError(codeInvalidParams, "invalid tools/list params"))
		return &response
	}
	definitions := make([]Tool, len(s.tools))
	for index := range s.tools {
		definitions[index] = s.tools[index].definition
	}
	response := newResponse(request.ID, ListToolsResult{Tools: definitions})
	return &response
}

func (s *Server) requestMutates(request Request) bool {
	if request.Method != "tools/call" {
		return false
	}
	var params CallToolParams
	if strictDecode(request.Params, &params) != nil {
		return false
	}
	tool, ok := s.toolsByName[params.Name]
	return ok && toolMutates(tool.definition)
}

func (s *Server) callTool(ctx context.Context, request Request) *Response {
	var params CallToolParams
	if err := strictDecode(request.Params, &params); err != nil ||
		strings.TrimSpace(params.Name) == "" ||
		!argumentsObject(params.Arguments) {
		response := newErrorResponse(
			request.ID, newError(codeInvalidParams, "invalid tools/call params"))
		return &response
	}
	tool, ok := s.toolsByName[params.Name]
	if !ok {
		response := newErrorResponse(
			request.ID, newError(codeInvalidParams, "unknown tool"))
		return &response
	}
	if !s.toolCallLimit.Allow() {
		response := newResponse(request.ID, s.toolErrorResult(shoal.NewError(
			shoal.ErrorUnavailable, "tool call rate limit exceeded")))
		return &response
	}

	bound, decision, err := s.authorizedContext(ctx)
	if err != nil {
		response := newResponse(request.ID, s.toolErrorResult(err))
		return &response
	}
	contextBudget, outputBudget := s.contextBudget, s.outputBudget
	bound, decision, contextBudget, outputBudget, err =
		s.applyWorkspaceForOperation(
			bound, decision, tool.authorizationOperation)
	if err != nil {
		response := newResponse(request.ID, s.toolErrorResult(err))
		return &response
	}
	authorizationNow := s.interactionNow()
	if authorizationNow.IsZero() ||
		decision.Authorize(
			tool.authorizationOperation,
			auth.ResourceRequest{
				AuthorizationDomain: decision.AuthorizationDomain(),
			},
			authorizationNow,
		) != nil {
		response := newResponse(request.ID, s.toolErrorResult(
			shoal.NewError(shoal.ErrorUnauthorized, "authorization denied")))
		return &response
	}
	snapshot, err := s.interactionSnapshot(bound)
	if err != nil {
		response := newResponse(request.ID, s.toolErrorResult(err))
		return &response
	}
	arguments := params.Arguments
	if len(arguments) == 0 {
		arguments = json.RawMessage("{}")
	}
	if tool.inputSchema != nil {
		if err := validateOptionalToolArguments(arguments, *tool.inputSchema); err != nil {
			recordErr := s.recordToolOutcome(
				bound, decision, snapshot, tool.authorizationOperation,
				params.Name, arguments,
				ToolObservation{}, true, "invalid_arguments", false,
			)
			if recordErr != nil {
				err = recordErr
			} else {
				err = invalidToolArguments(params.Name)
			}
			response := newResponse(
				request.ID, s.toolErrorResult(err))
			return &response
		}
	}
	limitedArguments, limitErr := applyWorkspaceToolLimits(
		bound, params.Name, arguments)
	if limitErr != nil {
		recordErr := s.recordToolOutcome(
			bound, decision, snapshot, tool.authorizationOperation,
			params.Name, arguments, ToolObservation{}, true,
			toolStopReason(limitErr), false,
		)
		if recordErr != nil {
			limitErr = recordErr
		}
		response := newResponse(request.ID, s.toolErrorResult(limitErr))
		return &response
	}
	arguments = limitedArguments
	mutating := toolMutates(tool.definition)
	if mutating {
		if err := s.recordToolAdmission(
			bound, decision, snapshot, tool.authorizationOperation,
			params.Name, arguments,
		); err != nil {
			response := newResponse(request.ID, s.toolErrorResult(
				shoal.NewError(
					shoal.ErrorUnavailable,
					"tool admission could not be recorded; tool was not executed",
				),
			))
			return &response
		}
	}
	value, err := tool.call(bound, arguments)
	// An ingest batch in which every file was tried and has a known outcome
	// (#687: applied, unchanged, historical, or refused with nothing
	// committed) is reported as its definite error with the per-file
	// outcomes, partial or not. A batch stopped by a failure stays
	// indeterminate here, like every other post-dispatch failure.
	if mutating && err != nil &&
		!isPreEffectToolError(err) &&
		!webapi.IngestOutcomeKnown(err) &&
		!explorer.IsIndeterminateCommit(err) {
		err = explorer.MarkIndeterminateCommit(shoal.WrapError(
			shoal.ErrorUnavailable,
			"mutating tool outcome is indeterminate; verify current state before retrying",
			err,
		))
	}
	effectSucceeded := err == nil
	observation, observeErr := observeToolResult(tool, params.Name, value)
	if effectSucceeded && observeErr == nil {
		observation, observeErr = canonicalToolObservation(
			bound, observation, decision)
	}
	if effectSucceeded && observeErr != nil {
		if mutating {
			err = explorer.MarkIndeterminateCommit(shoal.WrapError(
				shoal.ErrorInternal,
				"tool effect committed but source observation failed",
				observeErr,
			))
		} else {
			err = observeErr
		}
	}
	outcomeSnapshot := snapshot
	if observation.SnapshotID != "" || !observation.SnapshotAsOf.IsZero() {
		outcomeSnapshot = explorer.Snapshot{
			ID: string(observation.SnapshotID), AsOf: observation.SnapshotAsOf,
		}
	} else if mutating {
		observed, snapshotErr := s.interactionSnapshot(bound)
		if snapshotErr == nil {
			outcomeSnapshot = observed
		} else if effectSucceeded {
			err = explorer.MarkIndeterminateCommit(shoal.WrapError(
				shoal.ErrorUnavailable,
				"tool effect committed but its resulting snapshot could not be observed",
				snapshotErr,
			))
		}
	}
	if err == nil && params.Name == ToolRetrieve {
		if recordErr := s.recordRetrievalOutcome(
			bound, decision, outcomeSnapshot, arguments, observation,
		); recordErr != nil {
			err = recordErr
		}
	}
	var result ToolResult
	if err == nil {
		result, err = s.toolSuccessResultWithBudgets(
			request.ID, value, contextBudget, outputBudget)
		if err != nil && mutating {
			err = explorer.MarkIndeterminateCommit(shoal.WrapError(
				shoal.ErrorInternal,
				"tool effect committed but its response could not be encoded",
				err,
			))
		}
	}
	recordErr := s.recordToolOutcome(
		context.WithoutCancel(bound), decision, outcomeSnapshot, tool.authorizationOperation,
		params.Name, arguments, observation,
		err != nil, toolStopReason(err), mutating,
	)
	if recordErr != nil {
		if mutating {
			recordErr = explorer.MarkIndeterminateCommit(shoal.WrapError(
				shoal.ErrorUnavailable,
				"tool effect may have committed but its durable outcome record failed; verify state before retrying",
				recordErr,
			))
		} else {
			recordErr = shoal.NewError(
				shoal.ErrorUnavailable,
				"tool result was withheld because durable recording failed",
			)
		}
		response := newResponse(request.ID, s.toolErrorResult(recordErr))
		return &response
	}

	if err != nil {
		response := newResponse(request.ID, s.toolErrorResult(err))
		return &response
	}
	response := newResponse(request.ID, result)
	return &response
}

func (s *Server) interactionSnapshot(
	ctx context.Context,
) (explorer.Snapshot, error) {
	if provider, ok := s.snapshots.(interface {
		InteractionSnapshot(context.Context) (explorer.Snapshot, error)
	}); ok && !isAbsent(provider) {
		return provider.InteractionSnapshot(ctx)
	}
	return s.snapshots.Snapshot(ctx)
}

type fixedWindowLimiter struct {
	mu          sync.Mutex
	limit       int
	window      time.Duration
	now         func() time.Time
	windowStart time.Time
	count       int
}

func newFixedWindowLimiter(
	limit int,
	window time.Duration,
	now func() time.Time,
) *fixedWindowLimiter {
	return &fixedWindowLimiter{limit: limit, window: window, now: now}
}

func (l *fixedWindowLimiter) Allow() bool {
	if l == nil || l.limit <= 0 || l.window <= 0 || l.now == nil {
		return false
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.windowStart.IsZero() ||
		now.Before(l.windowStart) ||
		now.Sub(l.windowStart) >= l.window {
		l.windowStart = now
		l.count = 0
	}
	if l.count >= l.limit {
		return false
	}
	l.count++
	return true
}

type boundHTTPDecisionKey struct{}

func withBoundHTTPDecision(
	ctx context.Context, decision auth.Decision,
) context.Context {
	return context.WithValue(ctx, boundHTTPDecisionKey{}, decision)
}

func (s *Server) bindHTTPSessionDecision(
	ctx context.Context,
	decision auth.Decision,
	correlationID shoal.ID,
) (context.Context, error) {
	if s == nil || isAbsent(s.binder) {
		return nil, shoal.NewError(
			shoal.ErrorUnauthorized, "authorization denied")
	}
	if err := shoal.ValidateRequiredID(
		"MCP session correlation ID", correlationID); err != nil {
		return nil, shoal.NewError(
			shoal.ErrorUnauthorized, "authorization denied")
	}
	selectedOntology, _ := decision.SelectedOntology()
	correlated, err := auth.NewDecision(auth.DecisionConfig{
		Subject:                decision.Subject(),
		Actor:                  decision.Actor(),
		ClientID:               decision.ClientID(),
		OnBehalfOf:             decision.OnBehalfOf(),
		AuthorizationDomain:    decision.AuthorizationDomain(),
		AllowedOperations:      decision.AllowedOperations(),
		PermittedSourceIDs:     decision.PermittedSourceIDs(),
		PermittedPolicyIDs:     decision.PermittedPolicyIDs(),
		PolicyGeneration:       decision.PolicyGeneration(),
		AuthenticationExpires:  decision.AuthenticationExpires(),
		RequestID:              decision.RequestID(),
		CorrelationID:          correlationID,
		AuditPurpose:           decision.AuditPurpose(),
		ServiceRole:            decision.ServiceRole(),
		ServiceCeilingIdentity: decision.ServiceCeilingIdentity(),
		SelectedOntology:       selectedOntology,
		GrantProvenance:        decision.GrantProvenance(),
		ExecutorBinding:        decision.ExecutorBinding(),
	})
	if err != nil {
		return nil, shoal.NewError(
			shoal.ErrorUnauthorized, "authorization denied")
	}
	bound, err := s.binder.Bind(ctx, correlated)
	if err != nil || bound == nil {
		return nil, shoal.NewError(
			shoal.ErrorUnauthorized, "authorization denied")
	}
	return withBoundHTTPDecision(bound, correlated), nil
}

func (s *Server) authorizedContext(
	ctx context.Context,
) (context.Context, auth.Decision, error) {
	if err := ctx.Err(); err != nil {
		return nil, auth.Decision{}, err
	}
	if decision, ok := ctx.Value(boundHTTPDecisionKey{}).(auth.Decision); ok {
		if _, err := auth.AuthorizationFingerprint(decision); err != nil {
			return nil, auth.Decision{}, shoal.NewError(
				shoal.ErrorUnauthorized, "authorization denied")
		}
		return ctx, decision, nil
	}
	template, err := s.decisions.Decision(ctx)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, auth.Decision{}, contextErr
		}
		return nil, auth.Decision{}, shoal.NewError(
			shoal.ErrorUnauthorized, "authorization denied")
	}
	requestID, err := s.requestID()
	if err != nil {
		return nil, auth.Decision{}, shoal.NewError(
			shoal.ErrorUnavailable, "request identity unavailable")
	}
	if err := shoal.ValidateRequiredID("MCP request ID", requestID); err != nil {
		return nil, auth.Decision{}, shoal.NewError(
			shoal.ErrorUnavailable, "request identity unavailable")
	}
	selectedOntology, _ := template.SelectedOntology()
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject:                template.Subject(),
		Actor:                  template.Actor(),
		ClientID:               template.ClientID(),
		OnBehalfOf:             template.OnBehalfOf(),
		AuthorizationDomain:    template.AuthorizationDomain(),
		AllowedOperations:      template.AllowedOperations(),
		PermittedSourceIDs:     template.PermittedSourceIDs(),
		PermittedPolicyIDs:     template.PermittedPolicyIDs(),
		PolicyGeneration:       template.PolicyGeneration(),
		AuthenticationExpires:  template.AuthenticationExpires(),
		RequestID:              requestID,
		CorrelationID:          template.CorrelationID(),
		AuditPurpose:           template.AuditPurpose(),
		ServiceRole:            template.ServiceRole(),
		ServiceCeilingIdentity: template.ServiceCeilingIdentity(),
		SelectedOntology:       selectedOntology,
		GrantProvenance:        template.GrantProvenance(),
		ExecutorBinding:        template.ExecutorBinding(),
	})
	if err != nil {
		return nil, auth.Decision{}, shoal.NewError(
			shoal.ErrorUnauthorized, "authorization denied")
	}
	bound, err := s.binder.Bind(ctx, decision)
	if err != nil || bound == nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, auth.Decision{}, contextErr
		}
		return nil, auth.Decision{}, shoal.NewError(
			shoal.ErrorUnauthorized, "authorization denied")
	}
	return bound, decision, nil
}

func randomRequestID() (shoal.ID, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return shoal.ID("mcp-" + hex.EncodeToString(random[:])), nil
}

func strictDecode(raw json.RawMessage, value any) error {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return errors.New("missing params")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func strictDecodeOptional(raw json.RawMessage, value any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	return strictDecode(raw, value)
}

func validEmptyParams(raw json.RawMessage) bool {
	var params EmptyParams
	return strictDecodeOptional(raw, &params) == nil
}

func argumentsObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || (len(trimmed) > 1 && trimmed[0] == '{')
}

func isAbsent(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice, reflect.UnsafePointer:
		return reflected.IsNil()
	default:
		return false
	}
}

func serviceTools(
	service webapi.Service,
	optional []OptionalToolProvider,
) ([]registeredTool, error) {
	tools := mandatoryServiceTools(service)
	tools = append(tools, optionalServiceTools(service)...)
	for _, provider := range optional {
		if isAbsent(provider) {
			return nil, shoal.NewError(
				shoal.ErrorInvalidArgument, "optional MCP tool provider is required")
		}
		definition := provider.Tool()
		inputSchema, err := validateTool(definition)
		if err != nil {
			return nil, err
		}
		if _, reserved := reservedServiceToolNames[definition.Name]; reserved {
			return nil, shoal.NewError(
				shoal.ErrorInvalidArgument, "reserved MCP tool name")
		}
		provider := provider
		var observe func(any) (ToolObservation, error)
		if observer, ok := provider.(OptionalToolObservationProvider); ok {
			observe = observer.ObserveToolResult
		}
		operationProvider, ok := provider.(OptionalToolAuthorizationProvider)
		if !ok || isAbsent(operationProvider) {
			return nil, shoal.NewError(
				shoal.ErrorInvalidArgument,
				"optional MCP tool authorization operation is required",
			)
		}
		authorizationOperation := operationProvider.ToolAuthorizationOperation()
		if err := authorizationOperation.Validate(); err != nil {
			return nil, shoal.NewError(
				shoal.ErrorInvalidArgument,
				"optional MCP tool authorization operation is invalid",
			)
		}
		tools = append(tools, registeredTool{
			definition:             cloneTool(definition),
			inputSchema:            inputSchema,
			authorizationOperation: authorizationOperation,
			call: func(ctx context.Context, arguments json.RawMessage) (any, error) {
				return provider.Call(ctx, append(json.RawMessage(nil), arguments...))
			},
			observe: observe,
		})
	}
	sort.Slice(tools, func(left, right int) bool {
		return tools[left].definition.Name < tools[right].definition.Name
	})
	for index := range tools {
		if err := tools[index].authorizationOperation.Validate(); err != nil {
			return nil, shoal.NewError(
				shoal.ErrorInvalidArgument,
				"MCP tool authorization operation is invalid",
			)
		}
		if index > 0 &&
			tools[index-1].definition.Name == tools[index].definition.Name {
			return nil, shoal.NewError(
				shoal.ErrorInvalidArgument, "duplicate MCP tool name")
		}
	}
	return tools, nil
}
