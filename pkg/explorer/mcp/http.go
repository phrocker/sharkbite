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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	SessionHeader          = "MCP-Session-Id"
	ProtocolVersionHeader  = "MCP-Protocol-Version"
	DefaultHTTPSessionTTL  = 30 * time.Minute
	DefaultMaxHTTPSessions = 1024
	maxHTTPResponseBytes   = int64(webapi.MaxResponseBytes) + 64<<10
)

// HTTPConfig configures the MCP 2025-11-25 Streamable HTTP transport. The
// handler expects every request context to have already passed through the
// matching webapi.Authenticator and auth.Binder; production mounts it inside
// webapi.Handler so the existing Host gate and authentication path remain the
// only external boundary.
type HTTPConfig struct {
	Server         *Server
	AllowedOrigins []string
	SessionTTL     time.Duration
	MaxSessions    int
	// RequireWorkspaceSettings requires every stateful request to carry one
	// canonical Shoal-Workspace-ID that the outer authenticated Handler has
	// already resolved and applied.
	RequireWorkspaceSettings bool
	Clock                    func() time.Time
}

// HTTPHandler implements the synchronous application/json subset of
// Streamable HTTP. GET returns 405 because this server emits no unsolicited
// server messages and therefore has no SSE stream to expose.
type HTTPHandler struct {
	server                   *Server
	origins                  map[string]struct{}
	ttl                      time.Duration
	maxSessions              int
	requireWorkspaceSettings bool
	now                      func() time.Time
	sessionsMu               sync.Mutex
	sessions                 map[string]*httpSession
}

type httpSession struct {
	dispatcher   *Server
	owner        auth.Fingerprint
	correlation  shoal.ID
	version      string
	expiresAt    time.Time
	workspace    workspaceBinding
	hasWorkspace bool
	mu           sync.Mutex
	active       map[string]context.CancelFunc
	closed       bool
}

var (
	errHTTPSessionClosed = errors.New("mcp: HTTP session is closed")
	errHTTPRequestActive = errors.New("mcp: HTTP request ID is already active")
)

// NewHTTPHandler constructs a stateful Streamable HTTP endpoint. Allowed
// origins are exact scheme-and-authority values; requests without Origin
// remain valid for non-browser MCP clients.
func NewHTTPHandler(config HTTPConfig) (*HTTPHandler, error) {
	if config.Server == nil || config.Server.resolver == nil {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "MCP HTTP server is required")
	}
	if config.RequireWorkspaceSettings &&
		isAbsent(config.Server.workspaceSettings) {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"MCP workspace settings provider is required",
		)
	}
	origins := make(map[string]struct{}, len(config.AllowedOrigins))
	for _, raw := range config.AllowedOrigins {
		normalized, err := normalizeHTTPOrigin(raw)
		if err != nil {
			return nil, err
		}
		origins[normalized] = struct{}{}
	}
	ttl := config.SessionTTL
	if ttl == 0 {
		ttl = DefaultHTTPSessionTTL
	}
	if ttl < 0 {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "MCP HTTP session TTL is invalid")
	}
	maxSessions := config.MaxSessions
	if maxSessions == 0 {
		maxSessions = DefaultMaxHTTPSessions
	}
	if maxSessions < 0 {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "MCP HTTP session bound is invalid")
	}
	now := config.Clock
	if now == nil {
		now = time.Now
	}
	return &HTTPHandler{
		server: config.Server, origins: origins, ttl: ttl,
		maxSessions:              maxSessions,
		requireWorkspaceSettings: config.RequireWorkspaceSettings,
		now:                      now,
		sessions:                 make(map[string]*httpSession),
	}, nil
}

// OriginsForAuthorities returns exact HTTP and HTTPS origins for configured
// Host authorities. The outer webapi host gate remains authoritative.
func OriginsForAuthorities(authorities []string) []string {
	origins := make([]string, 0, 2*len(authorities))
	for _, authority := range authorities {
		authority = strings.TrimSpace(authority)
		if authority == "" {
			continue
		}
		origins = append(origins, "http://"+authority, "https://"+authority)
	}
	return origins
}

// ValidatePreAuthentication lets webapi.Handler reject a hostile browser
// Origin after its Host check but before invoking an authenticator.
func (h *HTTPHandler) ValidatePreAuthentication(request *http.Request) int {
	if h == nil || request == nil ||
		!h.originAllowed(request.Header.Get("Origin")) {
		return http.StatusForbidden
	}
	return 0
}

func (h *HTTPHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	if request == nil || request.Context() == nil {
		h.writeHTTPError(writer, http.StatusBadRequest,
			newError(codeInvalidRequest, "invalid request"))
		return
	}
	if !h.originAllowed(request.Header.Get("Origin")) {
		h.writeHTTPError(writer, http.StatusForbidden,
			newError(codeInvalidRequest, "origin is not allowed"))
		return
	}
	decision, err := h.server.resolver.Resolve(request.Context())
	if err != nil {
		writer.Header().Set("WWW-Authenticate", "Bearer")
		h.writeHTTPError(writer, http.StatusUnauthorized,
			newError(codeInvalidRequest, "authentication required"))
		return
	}
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		writer.Header().Set("WWW-Authenticate", "Bearer")
		h.writeHTTPError(writer, http.StatusUnauthorized,
			newError(codeInvalidRequest, "authentication required"))
		return
	}
	workspace, hasWorkspace, err := workspaceBindingForRequest(
		request, h.requireWorkspaceSettings)
	if err != nil {
		h.writeHTTPError(writer, http.StatusBadRequest,
			newError(codeInvalidRequest, "workspace settings are invalid"))
		return
	}

	switch request.Method {
	case http.MethodPost:
		h.servePost(
			writer, request, decision, fingerprint, workspace, hasWorkspace)
	case http.MethodGet:
		writer.Header().Set("Allow", "POST, DELETE")
		h.writeHTTPError(writer, http.StatusMethodNotAllowed,
			newError(codeMethodNotFound, "SSE stream is not supported"))
	case http.MethodDelete:
		h.serveDelete(
			writer, request, fingerprint, workspace, hasWorkspace)
	default:
		writer.Header().Set("Allow", "POST, GET, DELETE")
		h.writeHTTPError(writer, http.StatusMethodNotAllowed,
			newError(codeMethodNotFound, "HTTP method is not supported"))
	}
}

func (h *HTTPHandler) servePost(
	writer http.ResponseWriter,
	request *http.Request,
	decision auth.Decision,
	fingerprint auth.Fingerprint,
	workspace workspaceBinding,
	hasWorkspace bool,
) {
	if !acceptsMediaType(request.Header.Values("Accept"), "application/json") ||
		!acceptsMediaType(request.Header.Values("Accept"), "text/event-stream") {
		h.writeHTTPError(writer, http.StatusNotAcceptable,
			newError(codeInvalidRequest,
				"Accept must include application/json and text/event-stream"))
		return
	}
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		h.writeHTTPError(writer, http.StatusUnsupportedMediaType,
			newError(codeInvalidRequest, "Content-Type must be application/json"))
		return
	}
	raw, err := readHTTPMessage(writer, request)
	if err != nil {
		h.writeHTTPError(writer, http.StatusBadRequest,
			newError(codeInvalidRequest, "invalid request body"))
		return
	}
	message, failure := decodeRequest(raw)
	if failure != nil {
		id, _ := protocolFailureID(raw, failure)
		h.writeHTTPErrorResponse(writer, http.StatusBadRequest,
			newErrorResponse(id, failure))
		return
	}

	sessionID := singleHeader(request.Header, SessionHeader)
	if message.Method == "initialize" {
		if len(request.Header.Values(SessionHeader)) != 0 ||
			sessionID != "" || message.isNotification() {
			h.writeHTTPError(writer, http.StatusBadRequest,
				newError(codeInvalidRequest, "invalid initialization request"))
			return
		}
		versionValues := request.Header.Values(ProtocolVersionHeader)
		if len(versionValues) > 1 {
			h.writeHTTPError(writer, http.StatusBadRequest,
				newError(codeInvalidRequest, "unsupported protocol version"))
			return
		}
		if version := singleHeader(request.Header, ProtocolVersionHeader); version != "" && version != ProtocolVersion {
			h.writeHTTPError(writer, http.StatusBadRequest,
				newError(codeInvalidRequest, "unsupported protocol version"))
			return
		}
		h.initializeHTTP(
			writer, request, message, decision, fingerprint,
			workspace, hasWorkspace)
		return
	}

	session, ok := h.authorizedSession(
		writer, request, fingerprint, workspace, hasWorkspace)
	if !ok {
		return
	}
	if message.Method == "notifications/cancelled" && message.isNotification() {
		session.cancel(message.Params)
		writer.WriteHeader(http.StatusAccepted)
		return
	}
	if !h.revalidate(
		request.Context(), fingerprint, workspace, hasWorkspace) {
		writer.Header().Set("WWW-Authenticate", "Bearer")
		h.writeHTTPError(writer, http.StatusUnauthorized,
			newError(codeInvalidRequest, "authorization expired"))
		return
	}
	mutating := session.dispatcher.requestMutates(message)
	ctx, err := session.dispatcher.bindHTTPSessionDecision(
		request.Context(), decision, session.correlation)
	if err != nil {
		writer.Header().Set("WWW-Authenticate", "Bearer")
		h.writeHTTPError(writer, http.StatusUnauthorized,
			newError(codeInvalidRequest, "authentication required"))
		return
	}
	var cancel context.CancelFunc
	if !message.isNotification() {
		ctx, cancel, err = session.register(ctx, message.ID)
		if errors.Is(err, errHTTPSessionClosed) {
			h.writeHTTPError(writer, http.StatusNotFound,
				newError(codeInvalidRequest, "MCP session was not found"))
			return
		}
		if err != nil {
			h.writeHTTPError(writer, http.StatusConflict,
				newError(codeInvalidRequest, "request ID is already active"))
			return
		}
		defer session.unregister(message.ID)
		defer cancel()
	}
	response := session.dispatcher.handle(ctx, message)
	if message.isNotification() {
		writer.WriteHeader(http.StatusAccepted)
		return
	}
	if response == nil {
		h.writeHTTPError(writer, http.StatusInternalServerError,
			newError(codeInternalError, "missing response"))
		return
	}
	if !mutating && !h.revalidate(
		request.Context(), fingerprint, workspace, hasWorkspace) {
		writer.Header().Set("WWW-Authenticate", "Bearer")
		h.writeHTTPError(writer, http.StatusUnauthorized,
			newError(codeInvalidRequest, "authorization expired"))
		return
	}
	h.writeBoundedHTTPResponse(
		writer, http.StatusOK, *response,
		httpResponseLimit(workspace, hasWorkspace),
		message.Method == "tools/call",
	)
}

func (h *HTTPHandler) initializeHTTP(
	writer http.ResponseWriter,
	request *http.Request,
	message Request,
	decision auth.Decision,
	fingerprint auth.Fingerprint,
	workspace workspaceBinding,
	hasWorkspace bool,
) {
	outputBudget := uint64(0)
	if hasWorkspace {
		outputBudget = workspace.limits.OutputBytes
	}
	dispatcher := h.server.newProtocolSessionWithOutputBudget(outputBudget)
	response := dispatcher.handle(
		withBoundHTTPDecision(request.Context(), decision), message)
	if response == nil || response.Error != nil {
		if response == nil {
			h.writeHTTPError(writer, http.StatusBadRequest,
				newError(codeInvalidRequest, "invalid initialization request"))
			return
		}
		h.writeBoundedHTTPResponse(
			writer, http.StatusOK, *response,
			httpResponseLimit(workspace, hasWorkspace), false,
		)
		return
	}
	if !h.revalidate(
		request.Context(), fingerprint, workspace, hasWorkspace) {
		writer.Header().Set("WWW-Authenticate", "Bearer")
		h.writeHTTPError(writer, http.StatusUnauthorized,
			newError(codeInvalidRequest, "authorization expired"))
		return
	}
	sessionID, err := randomHTTPSessionID()
	if err != nil {
		h.writeHTTPError(writer, http.StatusInternalServerError,
			newError(codeInternalError, "session could not be created"))
		return
	}
	correlation := httpSessionCorrelationID(
		sessionID, fingerprint, workspace, hasWorkspace)
	session := &httpSession{
		dispatcher: dispatcher, owner: fingerprint,
		correlation: correlation,
		version:     ProtocolVersion, expiresAt: h.now().Add(h.ttl),
		workspace: workspace, hasWorkspace: hasWorkspace,
		active: make(map[string]context.CancelFunc),
	}
	h.sessionsMu.Lock()
	h.pruneExpiredLocked()
	if len(h.sessions) >= h.maxSessions {
		h.sessionsMu.Unlock()
		h.writeHTTPError(writer, http.StatusServiceUnavailable,
			newError(codeInternalError, "MCP session capacity is exhausted"))
		return
	}
	h.sessions[sessionID] = session
	h.sessionsMu.Unlock()
	writer.Header().Set(SessionHeader, sessionID)
	h.writeBoundedHTTPResponse(
		writer, http.StatusOK, *response,
		httpResponseLimit(workspace, hasWorkspace), false,
	)
}

func (h *HTTPHandler) serveDelete(
	writer http.ResponseWriter,
	request *http.Request,
	fingerprint auth.Fingerprint,
	workspace workspaceBinding,
	hasWorkspace bool,
) {
	session, ok := h.authorizedSession(
		writer, request, fingerprint, workspace, hasWorkspace)
	if !ok {
		return
	}
	sessionID := singleHeader(request.Header, SessionHeader)
	h.sessionsMu.Lock()
	delete(h.sessions, sessionID)
	h.sessionsMu.Unlock()
	session.close()
	writer.WriteHeader(http.StatusNoContent)
}

func (h *HTTPHandler) authorizedSession(
	writer http.ResponseWriter,
	request *http.Request,
	fingerprint auth.Fingerprint,
	workspace workspaceBinding,
	hasWorkspace bool,
) (*httpSession, bool) {
	sessionID := singleHeader(request.Header, SessionHeader)
	if sessionID == "" || len(request.Header.Values(SessionHeader)) != 1 {
		h.writeHTTPError(writer, http.StatusBadRequest,
			newError(codeInvalidRequest, "MCP session ID is required"))
		return nil, false
	}
	version := singleHeader(request.Header, ProtocolVersionHeader)
	if version == "" || len(request.Header.Values(ProtocolVersionHeader)) != 1 ||
		version != ProtocolVersion {
		h.writeHTTPError(writer, http.StatusBadRequest,
			newError(codeInvalidRequest, "unsupported protocol version"))
		return nil, false
	}
	h.sessionsMu.Lock()
	h.pruneExpiredLocked()
	session := h.sessions[sessionID]
	h.sessionsMu.Unlock()
	if session == nil || session.owner != fingerprint ||
		session.version != version ||
		session.hasWorkspace != hasWorkspace ||
		(hasWorkspace && !session.workspace.equal(workspace)) {
		h.writeHTTPError(writer, http.StatusNotFound,
			newError(codeInvalidRequest, "MCP session was not found"))
		return nil, false
	}
	return session, true
}

func (h *HTTPHandler) revalidate(
	ctx context.Context, expected auth.Fingerprint,
	workspace workspaceBinding,
	hasWorkspace bool,
) bool {
	decision, err := h.server.resolver.Resolve(ctx)
	if err != nil {
		return false
	}
	current, err := auth.AuthorizationFingerprint(decision)
	if err != nil || current != expected {
		return false
	}
	currentWorkspace, currentHasWorkspace, err :=
		effectiveWorkspaceBinding(ctx)
	return err == nil &&
		currentHasWorkspace == hasWorkspace &&
		(!hasWorkspace || currentWorkspace.equal(workspace))
}

func (h *HTTPHandler) pruneExpiredLocked() {
	now := h.now()
	for id, session := range h.sessions {
		if !now.Before(session.expiresAt) {
			delete(h.sessions, id)
			session.close()
		}
	}
}

func (h *HTTPHandler) originAllowed(raw string) bool {
	if raw == "" {
		return true
	}
	normalized, err := normalizeHTTPOrigin(raw)
	if err != nil {
		return false
	}
	_, ok := h.origins[normalized]
	return ok
}

func normalizeHTTPOrigin(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User != nil || parsed.Host == "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") ||
		(parsed.Path != "" && parsed.Path != "/") ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", shoal.NewError(
			shoal.ErrorInvalidArgument, "MCP allowed origin is invalid")
	}
	scheme := strings.ToLower(parsed.Scheme)
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if host == "" {
		return "", shoal.NewError(
			shoal.ErrorInvalidArgument, "MCP allowed origin is invalid")
	}
	port := parsed.Port()
	if (scheme == "http" && port == "80") ||
		(scheme == "https" && port == "443") {
		port = ""
	}
	authority := host
	if port != "" {
		authority = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		authority = "[" + host + "]"
	}
	return scheme + "://" + authority, nil
}

func readHTTPMessage(
	writer http.ResponseWriter, request *http.Request,
) ([]byte, error) {
	request.Body = http.MaxBytesReader(writer, request.Body, maxMessageBytes)
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 || len(body) > maxMessageBytes {
		return nil, errors.New("invalid request body")
	}
	return body, nil
}

func acceptsMediaType(values []string, want string) bool {
	for _, value := range values {
		for _, item := range strings.Split(value, ",") {
			mediaType, parameters, err := mime.ParseMediaType(strings.TrimSpace(item))
			if err != nil || mediaType != want {
				continue
			}
			if quality := parameters["q"]; quality != "" {
				parsed, err := strconv.ParseFloat(quality, 64)
				if err != nil || parsed <= 0 {
					continue
				}
			}
			return true
		}
	}
	return false
}

func singleHeader(header http.Header, name string) string {
	values := header.Values(name)
	if len(values) != 1 {
		return ""
	}
	return strings.TrimSpace(values[0])
}

func randomHTTPSessionID() (string, error) {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func httpSessionCorrelationID(
	sessionID string,
	owner auth.Fingerprint,
	workspace workspaceBinding,
	hasWorkspace bool,
) shoal.ID {
	hash := sha256.New()
	writeCorrelationPart(hash, "shoal-mcp-http-session-correlation-v1")
	writeCorrelationPart(hash, sessionID)
	writeCorrelationPart(hash, owner.String())
	if hasWorkspace {
		writeCorrelationPart(hash, workspace.headerValue())
		writeCorrelationPart(hash, string(workspace.settingsID))
		writeCorrelationPart(hash, strconv.FormatUint(workspace.revision, 10))
	}
	return shoal.ID("mcp-session-" + hex.EncodeToString(hash.Sum(nil)))
}

type correlationHash interface {
	Write([]byte) (int, error)
}

func writeCorrelationPart(hash correlationHash, value string) {
	_, _ = hash.Write([]byte(strconv.Itoa(len(value))))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(value))
}

func (h *HTTPHandler) writeHTTPError(
	writer http.ResponseWriter, status int, failure *Error,
) {
	h.writeHTTPErrorResponse(
		writer, status,
		newErrorResponse(json.RawMessage("null"), failure),
	)
}

func (h *HTTPHandler) writeHTTPErrorResponse(
	writer http.ResponseWriter, status int, response Response,
) {
	h.writeHTTPResponse(writer, status, response)
}

func (h *HTTPHandler) writeHTTPResponse(
	writer http.ResponseWriter, status int, response Response,
) {
	h.writeBoundedHTTPResponse(
		writer, status, response, maxHTTPResponseBytes, false,
	)
}

func (h *HTTPHandler) writeBoundedHTTPResponse(
	writer http.ResponseWriter,
	status int,
	response Response,
	limit int64,
	indeterminateOnOverflow bool,
) {
	var body boundedHTTPBuffer
	body.limit = limit
	if err := json.NewEncoder(&body).Encode(response); err != nil {
		status = http.StatusInternalServerError
		message := "response exceeds output byte limit"
		if indeterminateOnOverflow {
			writer.Header().Set(
				webapi.CommitOutcomeHeader,
				webapi.CommitOutcomeIndeterminate,
			)
			status = http.StatusServiceUnavailable
			message = "response exceeds output byte limit; operation outcome is indeterminate"
		}
		failure := newErrorResponse(
			response.ID, newError(codeInternalError, message))
		body.Reset()
		if err := json.NewEncoder(&body).Encode(failure); err != nil {
			body.Reset()
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(status)
		_, _ = writer.Write(body.Bytes())
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_, _ = writer.Write(body.Bytes())
}

func httpResponseLimit(
	workspace workspaceBinding,
	hasWorkspace bool,
) int64 {
	if !hasWorkspace ||
		workspace.limits.OutputBytes == 0 ||
		workspace.limits.OutputBytes >= uint64(maxHTTPResponseBytes) {
		return maxHTTPResponseBytes
	}
	return int64(workspace.limits.OutputBytes)
}

type boundedHTTPBuffer struct {
	bytes.Buffer
	limit int64
}

func (b *boundedHTTPBuffer) Write(value []byte) (int, error) {
	if int64(len(value)) > b.limit-int64(b.Len()) {
		return 0, errors.New("MCP HTTP response exceeds maximum size")
	}
	return b.Buffer.Write(value)
}

func (s *httpSession) register(
	ctx context.Context, id json.RawMessage,
) (context.Context, context.CancelFunc, error) {
	key, ok := requestIDKey(id)
	if !ok {
		return nil, nil, errors.New("mcp: invalid request ID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, nil, errHTTPSessionClosed
	}
	if _, exists := s.active[key]; exists {
		return nil, nil, errHTTPRequestActive
	}
	active, cancel := context.WithCancel(ctx)
	s.active[key] = cancel
	return active, cancel, nil
}

func (s *httpSession) unregister(id json.RawMessage) {
	key, ok := requestIDKey(id)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.active, key)
}

func (s *httpSession) cancel(raw json.RawMessage) {
	var params struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if strictDecode(raw, &params) != nil || !validRequestID(params.RequestID) {
		return
	}
	key, ok := requestIDKey(params.RequestID)
	if !ok {
		return
	}
	s.mu.Lock()
	cancel := s.active[key]
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *httpSession) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for id, cancel := range s.active {
		cancel()
		delete(s.active, id)
	}
}

func requestIDKey(id json.RawMessage) (string, bool) {
	if !validRequestID(id) || len(id) == 0 {
		return "", false
	}
	if id[0] == '"' {
		var value string
		if json.Unmarshal(id, &value) != nil {
			return "", false
		}
		return "s:" + value, true
	}
	value, ok := new(big.Rat).SetString(string(id))
	if !ok || !value.IsInt() {
		return "", false
	}
	return "n:" + value.Num().String(), true
}
