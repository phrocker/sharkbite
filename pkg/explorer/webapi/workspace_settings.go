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
	"net/http"
	"strings"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/workspace"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// WorkspaceIDHeader selects the owned durable workspace settings applied to a
// non-settings API request. Its value is one canonical opaque wire ID.
const WorkspaceIDHeader = "Shoal-Workspace-ID"

// WorkspaceOutputVisibilityHeader carries the canonical conjunction every
// response or derived interaction produced under effective settings requires.
const WorkspaceOutputVisibilityHeader = "Shoal-Output-Visibility"

// WorkspaceSettingsProvider is the transport-neutral settings extension used
// by the HTTP endpoint and future chat/MCP adapters.
type WorkspaceSettingsProvider interface {
	Get(context.Context, shoal.ID) (workspace.Settings, error)
	Update(
		context.Context,
		shoal.ID,
		workspace.UpdateRequest,
	) (workspace.Settings, error)
	ListOntologyChoices(
		context.Context,
		shoal.ID,
	) (workspace.OntologyChoiceSet, error)
	SelectOntology(
		context.Context,
		shoal.ID,
		uint64,
		shoal.ID,
		ontology.OntologyIdentity,
	) (workspace.Settings, error)
	ApplyForOperation(
		context.Context,
		shoal.ID,
		auth.Operation,
		workspace.Limits,
		[]auth.Policy,
	) (workspace.EffectiveDecision, error)
}

type WorkspaceSettingsApplier interface {
	ApplyForOperation(
		context.Context,
		shoal.ID,
		auth.Operation,
		workspace.Limits,
		[]auth.Policy,
	) (workspace.EffectiveDecision, error)
}

// WorkspaceSettingsHTTPConfig configures the independently mountable settings
// management handler.
type WorkspaceSettingsHTTPConfig struct {
	Provider WorkspaceSettingsProvider
}

// NewWorkspaceSettingsHTTPHandler constructs the settings management subtree.
// The returned handler performs authorization through Provider but must be
// mounted behind the host's existing authentication and host/origin gates.
func NewWorkspaceSettingsHTTPHandler(
	config WorkspaceSettingsHTTPConfig,
) (http.Handler, error) {
	if isAbsentInterface(config.Provider) {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "workspace settings provider is required")
	}
	routes := &Handler{
		mux:               http.NewServeMux(),
		workspaceSettings: config.Provider,
	}
	routes.registerWorkspaceSettingsRoutes()
	return routes.mux, nil
}

type effectiveWorkspaceSettingsContextKey struct{}
type effectiveWorkspaceIDContextKey struct{}

// EffectiveWorkspaceSettings returns the authenticated workspace settings
// effect already applied by Handler.ServeHTTP. Additive mounted transports can
// consume its budgets, output policies, and cache dimensions without
// re-resolving settings or replacing the issuer decision.
func EffectiveWorkspaceSettings(
	ctx context.Context,
) (workspace.EffectiveDecision, bool) {
	effective, ok := ctx.Value(
		effectiveWorkspaceSettingsContextKey{}).(workspace.EffectiveDecision)
	return effective, ok
}

func EffectiveWorkspaceID(ctx context.Context) (shoal.ID, bool) {
	workspaceID, ok := ctx.Value(effectiveWorkspaceIDContextKey{}).(shoal.ID)
	return workspaceID, ok
}

type workspaceResponseWriter struct {
	http.ResponseWriter
	maxResponseBytes        uint64
	indeterminateOnOverflow bool
}

func (w workspaceResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func responseSupportsFlush(writer http.ResponseWriter) bool {
	for depth := 0; writer != nil && depth < 100; depth++ {
		if _, ok := writer.(http.Flusher); ok {
			return true
		}
		unwrapper, ok := writer.(interface {
			Unwrap() http.ResponseWriter
		})
		if !ok {
			return false
		}
		writer = unwrapper.Unwrap()
	}
	return false
}

// ConfigureWorkspaceSettings enables effective-decision application without
// registering management routes. It is intended for hosts that mount the
// handler returned by NewWorkspaceSettingsHTTPHandler through their existing
// authenticated routing seam.
func (h *Handler) ConfigureWorkspaceSettings(
	provider WorkspaceSettingsProvider,
) error {
	if h == nil {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "workspace handler is required")
	}
	if isAbsentInterface(provider) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "workspace settings provider is required")
	}
	if isAbsentInterface(h.authenticator) || isAbsentInterface(h.binder) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"workspace settings require an authenticated handler",
		)
	}
	if h.workspaceSettingsMounted {
		return shoal.NewError(
			shoal.ErrorConflict, "workspace settings routes are already mounted")
	}
	h.workspaceSettings = provider
	return nil
}

// MountWorkspaceSettings configures effective-decision application and mounts
// the settings management subtree behind this Handler's existing gates.
func (h *Handler) MountWorkspaceSettings(
	config WorkspaceSettingsHTTPConfig,
) error {
	if err := h.ConfigureWorkspaceSettings(config.Provider); err != nil {
		return err
	}
	settingsHandler, err := NewWorkspaceSettingsHTTPHandler(config)
	if err != nil {
		return err
	}
	h.mux.Handle("GET /api/v1/workspaces/", settingsHandler)
	h.mux.Handle("PUT /api/v1/workspaces/", settingsHandler)
	h.workspaceSettingsMounted = true
	return nil
}

// SetWorkspaceSettingsProvider preserves the original one-call integration
// surface and delegates to MountWorkspaceSettings.
func (h *Handler) SetWorkspaceSettingsProvider(
	provider WorkspaceSettingsProvider,
) error {
	return h.MountWorkspaceSettings(
		WorkspaceSettingsHTTPConfig{Provider: provider})
}

func (h *Handler) applyWorkspaceSettings(
	request *http.Request,
) (context.Context, error) {
	if isAbsentInterface(h.workspaceSettings) ||
		isWorkspaceSettingsManagementPath(request.URL.Path) {
		return request.Context(), nil
	}
	workspaceID, present, err := workspaceIDFromHeader(request.Header)
	if err != nil {
		return nil, err
	}
	if !present {
		return request.Context(), nil
	}
	if request.URL.Path == "/mcp" && h.mountedHandler("/mcp") != nil {
		return context.WithValue(
			request.Context(), effectiveWorkspaceIDContextKey{}, workspaceID), nil
	}
	operation, apply := workspaceOperationForRequest(
		request.Method, request.URL.Path)
	if !apply {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"workspace settings are not registered for this route",
		)
	}
	return ApplyWorkspaceSettingsForOperation(
		request.Context(), h.workspaceSettings, h.binder,
		workspaceID, operation, workspace.MaximumLimits(), nil)
}

func ApplyWorkspaceSettingsForOperation(
	ctx context.Context,
	provider WorkspaceSettingsApplier,
	binder auth.Binder,
	workspaceID shoal.ID,
	operation auth.Operation,
	baseLimits workspace.Limits,
	baseOutputPolicies []auth.Policy,
) (context.Context, error) {
	if ctx == nil || isAbsentInterface(provider) || isAbsentInterface(binder) {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"workspace settings application dependencies are required")
	}
	effective, err := provider.ApplyForOperation(
		ctx, workspaceID, operation, baseLimits, baseOutputPolicies)
	if err != nil {
		return nil, err
	}
	decision := effective.Decision()
	ctx, err = binder.Bind(ctx, decision)
	if err != nil {
		return nil, authenticationDenied()
	}
	if ctx == nil {
		return nil, authenticationDenied()
	}
	visibility, err := effective.OutputVisibility()
	if err != nil {
		return nil, err
	}
	labels, err := interaction.ParseVisibility(string(visibility))
	if err != nil {
		return nil, err
	}
	ctx, err = interaction.WithRequiredVisibility(ctx, labels)
	if err != nil {
		return nil, err
	}
	ctx = withEffectiveWorkspaceSettings(ctx, effective)
	return withIdentity(ctx, decision), nil
}

func workspaceOperationForRequest(
	method, path string,
) (auth.Operation, bool) {
	if method == http.MethodHead {
		method = http.MethodGet
	}
	switch {
	case method == http.MethodGet && path == "/api/v1/meta":
		return auth.OperationRead, true
	case method == http.MethodGet && path == "/api/v1/identity":
		return auth.OperationRead, true
	case method == http.MethodGet &&
		(path == "/api/v1/provenance" ||
			(strings.HasPrefix(path, "/api/v1/provenance/") &&
				len(strings.TrimPrefix(path, "/api/v1/provenance/")) > 0 &&
				!strings.Contains(strings.TrimPrefix(path, "/api/v1/provenance/"), "/"))):
		return auth.OperationRead, true
	case method == http.MethodPost && path == "/api/v1/provenance/fold":
		return auth.OperationConnect, true
	case method == http.MethodPost && path == "/api/v1/provenance/unfold":
		return auth.OperationRead, true
	case method == http.MethodGet && path == "/api/v1/ontology":
		return auth.OperationRead, true
	case method == http.MethodGet && path == "/api/v1/ontology/proposals":
		return auth.OperationRead, true
	case method == http.MethodGet &&
		strings.HasPrefix(path, "/api/v1/ontology/proposals/") &&
		strings.HasSuffix(path, "/blast-radius"):
		return auth.OperationRead, true
	case method == http.MethodPost && path == "/api/v1/ontology/proposals":
		return auth.OperationIngest, true
	case method == http.MethodPost &&
		strings.HasPrefix(path, "/api/v1/ontology/proposals/") &&
		strings.HasSuffix(path, "/transition"):
		return auth.OperationIngest, true
	case method == http.MethodPost &&
		(path == "/api/v1/ingest" ||
			path == "/api/v1/extract" ||
			path == "/api/v1/derivation/recompute"):
		return auth.OperationIngest, true
	case method == http.MethodPost && path == "/api/v1/graph/materialize":
		return auth.OperationGraphMaterialize, true
	case method == http.MethodPost &&
		(path == "/api/v1/changes" || path == "/api/v1/documents"):
		return auth.OperationList, true
	case method == http.MethodPost && path == "/api/v1/document":
		return auth.OperationRead, true
	case method == http.MethodPost &&
		(path == "/api/v1/retrieve" || path == "/api/v1/ask" ||
			path == "/api/v1/chat/stream"):
		return auth.OperationRetrieve, true
	case method == http.MethodPost &&
		(path == "/api/v1/neighborhood" || path == "/api/v1/path"):
		return auth.OperationNeighborhood, true
	case method == http.MethodPost && path == "/api/v1/analytics":
		return auth.OperationAnalyticsRead, true
	case method == http.MethodPost && path == TeamOverviewRoute:
		return auth.OperationTeamOverviewRead, true
	case method == http.MethodPost && path == "/api/v1/fleet/agents":
		return auth.OperationAgentRegister, true
	case method == http.MethodPost &&
		strings.HasPrefix(path, "/api/v1/fleet/agents/") &&
		strings.HasSuffix(path, "/heartbeat"):
		return auth.OperationAgentHeartbeat, true
	case method == http.MethodPost &&
		strings.HasPrefix(path, "/api/v1/fleet/agents/") &&
		strings.HasSuffix(path, "/revoke"):
		return auth.OperationAgentRevoke, true
	case method == http.MethodPost &&
		(path == "/api/v1/fleet/agents/resolve" ||
			(strings.HasPrefix(path, "/api/v1/fleet/agents/") &&
				strings.HasSuffix(path, "/resolve"))):
		return auth.OperationAgentResolve, true
	case method == http.MethodPost && path == "/api/v1/fleet/actions":
		return auth.OperationDispatch, true
	case method == http.MethodPost &&
		(path == "/api/v1/fleet/actions/invoke" ||
			path == "/api/v1/fleet/actions/pull" ||
			(strings.HasPrefix(path, "/api/v1/fleet/actions/") &&
				(strings.HasSuffix(path, "/claim") ||
					// The two routes a claim's holder uses between claiming
					// and completing, under the same authority as the claim
					// and the completion on either side of them.
					//
					// Unlisted, neither was a 404: applyWorkspaceSettings
					// consults this table before dispatching, so a caller
					// sending a workspace ID was refused "workspace settings
					// are not registered for this route" before the handler
					// ran — including on the one route whose whole purpose is
					// to let a worker report an effect it could not otherwise
					// report.
					//
					// Invoke, because beginClaimant prefers execute and falls
					// back to invoke, and because that is what /claim and
					// /complete are already listed under. Binding a different
					// operation here would break the sequence in the middle.
					//
					// What this does *not* do, because an earlier version of
					// this comment claimed it did: make the routes reachable
					// for an execute-only caller. ApplyForOperation calls
					// decision.Authorize with the operation named here, so a
					// workspace-scoped worker must actually hold invoke. An
					// execute-only worker is still refused — now at the
					// authorization step rather than as an unregistered route.
					// That is pre-existing and identical for /claim and
					// /complete, so the sequence is consistent; it is not
					// complete for the execute-only holder #437 introduced.
					strings.HasSuffix(path, "/ambiguity") ||
					strings.HasSuffix(path, "/extend") ||
					strings.HasSuffix(path, "/complete")))):
		return auth.OperationInvoke, true
	case method == http.MethodPost &&
		strings.HasPrefix(path, "/api/v1/fleet/actions/") &&
		(strings.HasSuffix(path, "/cancel") ||
			strings.HasSuffix(path, "/status")):
		return auth.OperationDispatch, true
	// Admission is invoke on all three. Asking whether a call may happen,
	// reporting what it did, and listing what is still outstanding are all the
	// same authority as performing the work would be — the caller is the one
	// that would act, and nothing here is a lesser right than dispatching.
	//
	// A route missing from this table is not a 404: ServeHTTP consults it
	// before dispatching, so a caller sending a workspace ID would be refused
	// as not registered and the admission handlers would never run at all.
	case method == http.MethodPost &&
		(path == "/api/v1/admission/request" ||
			path == "/api/v1/admission/report" ||
			path == "/api/v1/admission/outstanding"):
		return auth.OperationInvoke, true
	// Approvals (#451). Requesting, and reading a request back, is the
	// requester's dispatch authority; deciding and listing what is decidable
	// is the approver's. The bound approval service refuses a decision made
	// under workspace settings, because a workspace narrowing could otherwise
	// shed the dispatch an approver holds and pass the separation check it is
	// there to fail — so a decide route listed here resolves the workspace and
	// is then refused, rather than being refused as unregistered.
	//
	// Withdrawing (#655) is the requester's too: only the principal that
	// made a request may withdraw it, under the dispatch it made it under.
	case method == http.MethodPost &&
		(path == "/api/v1/fleet/approvals/request" ||
			(strings.HasPrefix(path, "/api/v1/fleet/approvals/") &&
				(strings.HasSuffix(path, "/status") ||
					strings.HasSuffix(path, "/withdraw")))):
		return auth.OperationDispatch, true
	case method == http.MethodPost &&
		(path == "/api/v1/fleet/approvals/pending" ||
			(strings.HasPrefix(path, "/api/v1/fleet/approvals/") &&
				strings.HasSuffix(path, "/decide"))):
		return auth.OperationActionApprove, true
	case method == http.MethodPost &&
		path == "/api/v1/fleet/events/subscriptions":
		return auth.OperationSubscriptionCreate, true
	case method == http.MethodPost &&
		strings.HasPrefix(path, "/api/v1/fleet/events/subscriptions/") &&
		strings.HasSuffix(path, "/pull"):
		return auth.OperationSubscriptionCreate, true
	case method == http.MethodDelete &&
		strings.HasPrefix(path, "/api/v1/fleet/events/subscriptions/"):
		return auth.OperationSubscriptionDelete, true
	case method == http.MethodPost && path == "/api/v1/fleet/events/publish":
		return auth.OperationEventPublish, true
	default:
		return "", false
	}
}

func workspaceIDFromHeader(
	header http.Header,
) (shoal.ID, bool, error) {
	values := header.Values(WorkspaceIDHeader)
	if len(values) == 0 {
		return "", false, nil
	}
	if len(values) != 1 || values[0] == "" ||
		strings.TrimSpace(values[0]) != values[0] {
		return "", false, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"workspace settings header must contain one canonical opaque ID",
		)
	}
	workspaceID, err := decodeWorkspaceOpaqueID(
		"workspace settings header", values[0])
	if err != nil {
		return "", false, err
	}
	return workspaceID, true, nil
}

func withEffectiveWorkspaceSettings(
	ctx context.Context,
	effective workspace.EffectiveDecision,
) context.Context {
	return context.WithValue(
		ctx, effectiveWorkspaceSettingsContextKey{}, effective)
}

func workspaceOutputVisibility(ctx context.Context) string {
	return interaction.Expression(interaction.RequiredVisibility(ctx))
}

func applyWorkspaceRequestLimits(ctx context.Context, request any) {
	effective, ok := EffectiveWorkspaceSettings(ctx)
	if !ok {
		return
	}
	limits := effective.Limits()
	switch value := request.(type) {
	case *RetrievalRequest:
		narrowed := ClampWorkspaceRequestLimits(
			workspace.Limits{RetrievalTopK: value.Query.TopK},
			workspace.Limits{RetrievalTopK: retrieval.DefaultTopK},
			limits,
		)
		value.Query.TopK = narrowed.RetrievalTopK
	case *NeighborhoodRequest:
		narrowed := ClampWorkspaceRequestLimits(
			workspace.Limits{
				GraphDepth:  value.Depth,
				GraphFanout: value.Fanout,
				GraphNodes:  value.MaxNodes,
			},
			workspace.Limits{
				GraphDepth:  DefaultDepth,
				GraphFanout: DefaultFanout,
				GraphNodes:  DefaultMaxNodes,
			},
			limits,
		)
		value.Depth = narrowed.GraphDepth
		value.Fanout = narrowed.GraphFanout
		value.MaxNodes = narrowed.GraphNodes
	case *PathRequest:
		narrowed := ClampWorkspaceRequestLimits(
			workspace.Limits{
				GraphDepth:  value.MaxDepth,
				GraphFanout: value.Fanout,
			},
			workspace.Limits{
				GraphDepth:  DefaultDepth,
				GraphFanout: DefaultFanout,
			},
			limits,
		)
		value.MaxDepth = narrowed.GraphDepth
		value.Fanout = narrowed.GraphFanout
	case *AnalyticsRequest:
		applyAnalyticsWorkspaceLimits(value, limits)
	}
}

func applyAnalyticsWorkspaceLimits(
	request *AnalyticsRequest,
	limits workspace.Limits,
) {
	if request == nil {
		return
	}
	narrowed := ClampWorkspaceRequestLimits(
		workspace.Limits{
			GraphDepth:  request.Scope.Depth,
			GraphFanout: request.Scope.Fanout,
			GraphNodes:  request.Scope.MaxNodes,
		},
		workspace.Limits{},
		limits,
	)
	request.Scope.Depth = narrowed.GraphDepth
	request.Scope.Fanout = narrowed.GraphFanout
	request.Scope.MaxNodes = narrowed.GraphNodes
}

// ClampWorkspaceRequestLimits replaces zero request values with their defaults
// and lowers every request dimension to its effective workspace maximum.
func ClampWorkspaceRequestLimits(
	requested workspace.Limits,
	defaults workspace.Limits,
	maximum workspace.Limits,
) workspace.Limits {
	return workspace.Limits{
		RetrievalTopK: lowerRequestLimit(
			requested.RetrievalTopK,
			defaults.RetrievalTopK,
			maximum.RetrievalTopK,
		),
		GraphDepth: lowerRequestLimit(
			requested.GraphDepth,
			defaults.GraphDepth,
			maximum.GraphDepth,
		),
		GraphFanout: lowerRequestLimit(
			requested.GraphFanout,
			defaults.GraphFanout,
			maximum.GraphFanout,
		),
		GraphNodes: lowerRequestLimit(
			requested.GraphNodes,
			defaults.GraphNodes,
			maximum.GraphNodes,
		),
		OutputBytes: lowerRequestByteLimit(
			requested.OutputBytes,
			defaults.OutputBytes,
			maximum.OutputBytes,
		),
	}
}

func lowerRequestLimit(value, defaultValue, maximum uint32) uint32 {
	if value == 0 {
		value = defaultValue
	}
	if value > maximum {
		return maximum
	}
	return value
}

func lowerRequestByteLimit(value, defaultValue, maximum uint64) uint64 {
	if value == 0 {
		value = defaultValue
	}
	if value > maximum {
		return maximum
	}
	return value
}

func effectiveGraphNodeLimit(ctx context.Context, fallback uint32) uint32 {
	effective, ok := EffectiveWorkspaceSettings(ctx)
	if !ok || effective.Limits().GraphNodes >= fallback {
		return fallback
	}
	return effective.Limits().GraphNodes
}

func applyWorkspaceMetadataLimits(
	ctx context.Context,
	metadata MetadataResponse,
) MetadataResponse {
	effective, ok := EffectiveWorkspaceSettings(ctx)
	if !ok {
		return metadata
	}
	limits := effective.Limits()
	metadata.MaxTopK = min(metadata.MaxTopK, limits.RetrievalTopK)
	metadata.MaxDepth = min(metadata.MaxDepth, limits.GraphDepth)
	metadata.MaxFanout = min(metadata.MaxFanout, limits.GraphFanout)
	metadata.MaxNodes = min(metadata.MaxNodes, limits.GraphNodes)
	metadata.MaxResponseBytes = min(
		metadata.MaxResponseBytes, limits.OutputBytes)
	if metadata.AnalyticsLimits != nil {
		analyticsLimits := *metadata.AnalyticsLimits
		analyticsLimits.MaxDepth = min(
			analyticsLimits.MaxDepth, limits.GraphDepth)
		analyticsLimits.MaxFanout = min(
			analyticsLimits.MaxFanout, limits.GraphFanout)
		analyticsLimits.MaxNodes = min(
			analyticsLimits.MaxNodes, limits.GraphNodes)
		analyticsLimits.MaxSeeds = min(
			analyticsLimits.MaxSeeds, analyticsLimits.MaxNodes)
		metadata.AnalyticsLimits = &analyticsLimits
	}
	return metadata
}

func responseLimitFor(writer http.ResponseWriter) uint64 {
	if limited, ok := writer.(workspaceResponseWriter); ok {
		return limited.maxResponseBytes
	}
	return MaxResponseBytes
}

func responseOverflowIsIndeterminate(writer http.ResponseWriter) bool {
	limited, ok := writer.(workspaceResponseWriter)
	return ok && limited.indeterminateOnOverflow
}

func responseLimitForContext(ctx context.Context) uint64 {
	effective, ok := EffectiveWorkspaceSettings(ctx)
	if !ok {
		return MaxResponseBytes
	}
	return effective.Limits().OutputBytes
}

func isWorkspaceSettingsManagementPath(path string) bool {
	return strings.HasPrefix(path, "/api/v1/workspaces/") &&
		(strings.HasSuffix(path, "/settings") ||
			strings.HasSuffix(path, "/settings/lens"))
}

func requestMayCommit(method, path string) bool {
	if method == http.MethodDelete {
		return strings.HasPrefix(
			path, "/api/v1/fleet/events/subscriptions/")
	}
	if method != http.MethodPost {
		return false
	}
	switch path {
	case "/api/v1/ingest",
		"/api/v1/decisions",
		"/api/v1/outcomes",
		"/api/v1/adjudications",
		// Collector writes commit before the response is encoded. Their
		// receipts are small and fixed-shape so a narrow budget still fits;
		// the GET read route commits nothing and is deliberately absent.
		"/api/v1/collectors/enroll",
		"/api/v1/collectors/artifacts",
		"/api/v1/collectors/observations",
		// Admin writes commit the registration and its change log entry
		// before the response is encoded; their receipts are fixed-shape.
		"/api/v1/collectors/admin/provision",
		"/api/v1/collectors/admin/revoke",
		"/api/v1/graph/materialize",
		"/api/v1/extract",
		"/api/v1/derivation/recompute",
		"/api/v1/ontology/proposals",
		"/api/v1/analytics",
		"/api/v1/fleet/agents",
		"/api/v1/fleet/actions",
		"/api/v1/fleet/actions/invoke",
		// Admission commits its ActionRecord before the response is encoded.
		// A request commits the grant or the refusal; a report commits the
		// terminal record for a call the caller has already made. If either
		// response is lost — to the workspace output limit like anything else
		// — the caller must be told the outcome is indeterminate rather than
		// that nothing committed. A deterministic failure on /request invites
		// a retry that will now conflict with the caller's own live grant, and
		// on /report it invites re-reporting an egress that already happened.
		"/api/v1/admission/request",
		"/api/v1/admission/report",
		// A request may write the held record or materialize the action,
		// so a lost response must read as indeterminate rather than as
		// nothing having happened.
		"/api/v1/fleet/approvals/request",
		"/api/v1/fleet/events/subscriptions",
		"/api/v1/fleet/events/publish":
		return true
	default:
		return (strings.HasPrefix(path, "/api/v1/ontology/proposals/") &&
			strings.HasSuffix(path, "/transition")) ||
			(strings.HasPrefix(path, "/api/v1/fleet/agents/") &&
				(strings.HasSuffix(path, "/heartbeat") ||
					strings.HasSuffix(path, "/revoke"))) ||
			(strings.HasPrefix(path, "/api/v1/fleet/actions/") &&
				(strings.HasSuffix(path, "/claim") ||
					strings.HasSuffix(path, "/complete") ||
					// A lost-fence report writes to the record before its
					// response is encoded. It does not transition the action,
					// but an over-budget response must still not be reported
					// as a clean failure — on a route whose entire purpose is
					// recording an ambiguity, telling the caller nothing
					// happened when the report landed would be the worst
					// possible answer.
					strings.HasSuffix(path, "/ambiguity") ||
					// A renewal writes the new lease before its response is
					// encoded. An over-budget response reported as a clean
					// failure would tell a worker its claim is gone while the
					// extension actually landed — so it would abandon work it
					// still holds the fence for.
					strings.HasSuffix(path, "/extend") ||
					strings.HasSuffix(path, "/cancel"))) ||
			// A decision and a withdrawal (#655) commit; pending only
			// reads. Status may record a final state it observes (#655),
			// but that write is a fact any later call re-derives, and a
			// lost Status response loses nothing a retry does not read
			// back, so it stays out: an over-budget Status is not an
			// indeterminate commit.
			(strings.HasPrefix(path, "/api/v1/fleet/approvals/") &&
				(strings.HasSuffix(path, "/decide") ||
					strings.HasSuffix(path, "/withdraw")))
	}
}
