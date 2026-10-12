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

package effectsgateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"net/url"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	dispatchapi "github.com/phrocker/shoal-oss/pkg/dispatch/api"
)

// IdempotencyMode is how a route's target collapses a repeated request.
type IdempotencyMode string

const (
	// IdempotencyKey: the target deduplicates on the idempotency header,
	// which carries the action's ExecutorKey, for at least the declared
	// retention.
	IdempotencyKey IdempotencyMode = "key"
	// IdempotencyNatural: the method is idempotent by specification (PUT,
	// DELETE), so sending the identical request twice has the effect of
	// sending it once.
	IdempotencyNatural IdempotencyMode = "natural"
	// IdempotencyUnprotected: nothing collapses a repeat. A request that may
	// have been written is never re-sent.
	IdempotencyUnprotected IdempotencyMode = "unprotected"
)

const (
	// MaxRoutes bounds the table. A descriptor's capability carries one action
	// per route, and the startup check compares them one to one.
	MaxRoutes = 256
	// MaxReferenceBytes bounds a target-side identifier copied into Output.
	// Output is published on action.completed and rendered by the team
	// overview, and the fleet output schema has no maxLength, so this bound
	// is the only one there is.
	MaxReferenceBytes = 256
	// maxPathTemplateBytes and maxPatternBytes keep operator configuration
	// within sizes a reviewer can read.
	maxPathTemplateBytes = 2048
	maxPatternBytes      = 512
	maxQueryNames        = 64
	maxConflictEquals    = 32
)

// UserAgent is fixed. A version string that changed per build would make the
// second attempt of one action differ from the first across a rollout, which
// a target deduplicating on key plus body could read as a different request.
const UserAgent = "shoal-effects-gateway/1"

// Route is one validated entry of the route table. Its fields are set only by
// ParseRoutes, and nothing outside this package can construct one, so a Route
// a caller holds has passed every rule below.
type Route struct {
	action      string
	method      string
	template    string
	segments    []pathSegment
	params      []string
	effects     dispatchapi.Effects
	idempotency IdempotencyMode
	query       map[string]struct{}
	conflict    *conflictRule
	retryable   map[int]struct{}
	reference   *referenceRule
}

type pathSegment struct {
	literal string
	param   string
}

type conflictRule struct {
	status  map[int]struct{}
	pointer string
	equals  map[string]struct{}
}

type referenceRule struct {
	pointer string
	header  string
	pattern *regexp.Regexp
}

// Action is the fleet action name this route performs.
func (r *Route) Action() string { return r.action }

// Method is the HTTP method, always one of POST, PUT, PATCH, DELETE.
func (r *Route) Method() string { return r.method }

// PathTemplate is the configured template, never a filled path. It is what
// the logging policy permits.
func (r *Route) PathTemplate() string { return r.template }

// Idempotency is the route's mode.
func (r *Route) Idempotency() IdempotencyMode { return r.idempotency }

// Effects is the route's declared set, which ParseRoutes has already required
// to equal the set derived from the target.
func (r *Route) Effects() dispatchapi.Effects { return append(dispatchapi.Effects(nil), r.effects...) }

// RouteTable is the deny-by-default mapping from action name to route. An
// action with no entry is not performed.
type RouteTable struct {
	routes map[string]*Route
	order  []string
}

// Lookup returns the route for an action. A miss is the deny.
func (t *RouteTable) Lookup(action string) (*Route, bool) {
	if t == nil {
		return nil, false
	}
	route, ok := t.routes[action]
	return route, ok
}

// Actions lists the configured action names in table order.
func (t *RouteTable) Actions() []string {
	if t == nil {
		return nil
	}
	return append([]string(nil), t.order...)
}

// RequiresKey reports whether any route depends on the idempotency header,
// which is what makes -idempotency-header and -idempotency-retention
// mandatory.
func (t *RouteTable) RequiresKey() bool {
	if t == nil {
		return false
	}
	for _, route := range t.routes {
		if route.idempotency == IdempotencyKey {
			return true
		}
	}
	return false
}

// DerivedEffects is the effect set every route must declare, derived from the
// configured target rather than chosen.
//
// Every route mutates something outside Shoal, which is the point of the
// gateway. Whether it also transmits content off the host is a property of
// where the target is, not of this code: a loopback target receives the body
// without it leaving the host, and anything else does not. Over-declaring
// egress is not the safe direction — the fleet's effect floor refuses only
// understatement, so an overstated set is silently denied by any policy that
// forbids egress, on a deployment where nothing egresses (#390).
//
// Loopback is decided from the configured host literally ("localhost" or a
// loopback IP). A hostname that merely resolves to loopback is treated as
// remote, which overstates; the dialer refuses private resolution unless the
// operator allows it, so that case is already an explicit configuration.
func DerivedEffects(target *url.URL) dispatchapi.Effects {
	if target != nil && isLoopbackHost(target.Hostname()) {
		return dispatchapi.Effects{dispatchapi.EffectMutatesExternal}
	}
	return dispatchapi.Effects{dispatchapi.EffectEgressesContent, dispatchapi.EffectMutatesExternal}
}

// routeWire is the strict JSON shape of one route.
type routeWire struct {
	Action      string         `json:"action"`
	Method      string         `json:"method"`
	Path        string         `json:"path"`
	Effects     []string       `json:"effects"`
	Idempotency string         `json:"idempotency"`
	Query       []string       `json:"query,omitempty"`
	Conflict    *conflictWire  `json:"conflict,omitempty"`
	Retryable   []int          `json:"retryable,omitempty"`
	Reference   *referenceWire `json:"reference,omitempty"`
}

type conflictWire struct {
	Status  []int    `json:"status"`
	Pointer string   `json:"pointer,omitempty"`
	Equals  []string `json:"equals,omitempty"`
}

type referenceWire struct {
	Pointer string `json:"pointer,omitempty"`
	Header  string `json:"header,omitempty"`
	Pattern string `json:"pattern"`
}

// ParseRoutes decodes and validates the route table.
//
// The document is a JSON array of routes, decoded strictly: unknown fields,
// duplicate object keys, trailing data and an empty table are all refused.
// derived is the effect set every route must declare (DerivedEffects).
//
// Each refusal names the route by index and action so an operator can find
// it, and never repeats a value the operator did not write.
func ParseRoutes(raw []byte, derived dispatchapi.Effects) (*RouteTable, error) {
	if err := rejectDuplicateKeys(raw); err != nil {
		return nil, fmt.Errorf("-routes: %w", err)
	}
	if err := exactRouteKeys(raw); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var wires []routeWire
	if err := decoder.Decode(&wires); err != nil {
		return nil, fmt.Errorf("-routes must be a JSON array of routes: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("-routes must contain exactly one JSON array")
	}
	if len(wires) == 0 {
		return nil, errors.New("-routes must declare at least one route; " +
			"an empty table performs nothing and is refused rather than " +
			"started as a gateway that never takes work")
	}
	if len(wires) > MaxRoutes {
		return nil, fmt.Errorf("-routes declares %d routes; at most %d are allowed",
			len(wires), MaxRoutes)
	}
	want, err := canonicalEffectSet(derived)
	if err != nil {
		return nil, fmt.Errorf("derived effects: %w", err)
	}
	table := &RouteTable{routes: make(map[string]*Route, len(wires))}
	for index, wire := range wires {
		route, err := wire.validate(want)
		if err != nil {
			name := wire.Action
			if fleetName("action", name) != nil {
				name = "(invalid name)"
			}
			return nil, fmt.Errorf("-routes[%d] %s: %w", index, name, err)
		}
		if _, duplicate := table.routes[route.action]; duplicate {
			return nil, fmt.Errorf("-routes[%d] %s: action is declared twice; "+
				"one action maps to exactly one request", index, route.action)
		}
		table.routes[route.action] = route
		table.order = append(table.order, route.action)
	}
	return table, nil
}

func (w routeWire) validate(derived dispatchapi.Effects) (*Route, error) {
	if err := fleetName("action", w.Action); err != nil {
		return nil, err
	}
	route := &Route{action: w.Action, template: w.Path}

	switch w.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		route.method = w.Method
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		// A read performs no effect, and a gateway for external effects that
		// is configured to read is either a mistake or a way to exfiltrate
		// through the target credential. Either way it is refused.
		return nil, fmt.Errorf("method %s is refused: this gateway performs "+
			"effects, and a read performs none", w.Method)
	case "":
		return nil, errors.New("method is required")
	default:
		return nil, errors.New("method must be one of POST, PUT, PATCH, DELETE, " +
			"spelled in upper case")
	}

	switch IdempotencyMode(w.Idempotency) {
	case IdempotencyKey, IdempotencyUnprotected:
		route.idempotency = IdempotencyMode(w.Idempotency)
	case IdempotencyNatural:
		// The only verification possible for "natural": the method must be
		// idempotent by specification. It is a weak proxy for the property
		// that matters, and it refuses the commonest mistake, which is a
		// POST declared natural because it happened to be safe once.
		if w.Method != http.MethodPut && w.Method != http.MethodDelete {
			return nil, errors.New("idempotency \"natural\" requires PUT or " +
				"DELETE; no other method is idempotent by specification")
		}
		route.idempotency = IdempotencyNatural
	case "":
		return nil, errors.New("idempotency is required: key, natural or unprotected")
	default:
		return nil, errors.New("idempotency must be key, natural or unprotected")
	}

	segments, params, err := parsePathTemplate(w.Path)
	if err != nil {
		return nil, err
	}
	route.segments, route.params = segments, params

	declared := make(dispatchapi.Effects, 0, len(w.Effects))
	for _, effect := range w.Effects {
		declared = append(declared, dispatchapi.Effect(effect))
	}
	effects, err := canonicalEffectSet(declared)
	if err != nil {
		return nil, fmt.Errorf("effects: %w", err)
	}
	if !reflect.DeepEqual(effects, derived) {
		return nil, fmt.Errorf("effects %s do not match %s, which is derived "+
			"from the configured target; a route's effects are a property of "+
			"where the request goes, and declaring a different set is refused "+
			"in both directions", effectList(effects), effectList(derived))
	}
	route.effects = effects

	if len(w.Query) > maxQueryNames {
		return nil, fmt.Errorf("query declares %d names; at most %d are allowed",
			len(w.Query), maxQueryNames)
	}
	route.query = make(map[string]struct{}, len(w.Query))
	for _, name := range w.Query {
		if !isIdentifier(name) {
			return nil, errors.New("query names must match [A-Za-z_][A-Za-z0-9_.-]*")
		}
		if _, duplicate := route.query[name]; duplicate {
			return nil, fmt.Errorf("query name %q is declared twice", name)
		}
		route.query[name] = struct{}{}
	}

	route.retryable = make(map[int]struct{}, len(w.Retryable))
	for _, status := range w.Retryable {
		if status < 400 || status > 599 {
			return nil, fmt.Errorf("retryable status %d is outside 400-599", status)
		}
		if _, duplicate := route.retryable[status]; duplicate {
			return nil, fmt.Errorf("retryable status %d is declared twice", status)
		}
		route.retryable[status] = struct{}{}
	}

	if w.Conflict != nil {
		if route.idempotency == IdempotencyUnprotected {
			// Conflict-is-success is evidence about a key or a natural
			// repeat. An unprotected route sends neither, so a matching
			// response there proves nothing about this action.
			return nil, errors.New("conflict requires idempotency key or " +
				"natural; an unprotected route has nothing a conflict could " +
				"be evidence of")
		}
		rule, err := w.Conflict.validate()
		if err != nil {
			return nil, fmt.Errorf("conflict: %w", err)
		}
		route.conflict = rule
	}

	if w.Reference != nil {
		rule, err := w.Reference.validate()
		if err != nil {
			return nil, fmt.Errorf("reference: %w", err)
		}
		route.reference = rule
	}
	return route, nil
}

// A conflict status may also be retryable. The rule's body value is what
// says "already done"; a provider that answers one status for both that and
// "the original is still in flight" (409 with a done marker, and 409
// idempotency_key_in_use) is configured by listing the status in both, so
// the in-flight answer retries under the same key until the provider replays
// the original outcome.
func (w *conflictWire) validate() (*conflictRule, error) {
	if len(w.Status) == 0 {
		return nil, errors.New("status must name at least one status code")
	}
	rule := &conflictRule{status: make(map[int]struct{}, len(w.Status))}
	for _, status := range w.Status {
		if status < 400 || status > 599 {
			// A 2xx is already success, and a 3xx is never evidence the
			// effect happened.
			return nil, fmt.Errorf("status %d is outside 400-599", status)
		}
		if _, duplicate := rule.status[status]; duplicate {
			return nil, fmt.Errorf("status %d is declared twice", status)
		}
		rule.status[status] = struct{}{}
	}
	if w.Pointer == "" {
		// A status alone is not evidence the effect happened. The commonest
		// same-key conflict on a byte-identical retry is "the original is
		// still in flight" (409 idempotency_key_in_use and its relatives),
		// which arrives exactly on written → timeout → retry and means the
		// outcome is not yet known. Reading it as success records an effect
		// that may yet fail. So the body must name the conflict.
		return nil, errors.New("pointer and equals are required: a status " +
			"alone cannot tell \"already done\" from \"still in flight\"")
	}
	if err := validatePointer(w.Pointer); err != nil {
		return nil, fmt.Errorf("pointer: %w", err)
	}
	if len(w.Equals) == 0 {
		return nil, errors.New("a pointer requires at least one value in equals; " +
			"a pointer that matches anything is the status rule alone, spelled " +
			"less clearly")
	}
	if len(w.Equals) > maxConflictEquals {
		return nil, fmt.Errorf("equals names %d values; at most %d are allowed",
			len(w.Equals), maxConflictEquals)
	}
	rule.pointer = w.Pointer
	rule.equals = make(map[string]struct{}, len(w.Equals))
	for _, value := range w.Equals {
		if value == "" {
			return nil, errors.New("equals values must not be empty")
		}
		rule.equals[value] = struct{}{}
	}
	return rule, nil
}

func (w *referenceWire) validate() (*referenceRule, error) {
	if (w.Pointer == "") == (w.Header == "") {
		return nil, errors.New("exactly one of pointer and header is required")
	}
	rule := &referenceRule{}
	if w.Pointer != "" {
		if err := validatePointer(w.Pointer); err != nil {
			return nil, fmt.Errorf("pointer: %w", err)
		}
		rule.pointer = w.Pointer
	} else {
		if !validHeaderName(w.Header) {
			return nil, errors.New("header must be a valid HTTP header name")
		}
		rule.header = textproto.CanonicalMIMEHeaderKey(w.Header)
	}
	if w.Pattern == "" {
		return nil, errors.New("pattern is required; an unconstrained " +
			"reference would copy target-controlled text into the durable record")
	}
	if len(w.Pattern) > maxPatternBytes {
		return nil, fmt.Errorf("pattern exceeds %d bytes", maxPatternBytes)
	}
	// Anchored here rather than trusted to be anchored: an unanchored
	// pattern matches any value containing a match, which is not a
	// constraint on the value at all.
	pattern, err := regexp.Compile(`^(?:` + w.Pattern + `)$`)
	if err != nil {
		return nil, errors.New("pattern is not a valid regular expression")
	}
	if pattern.MatchString("") {
		return nil, errors.New("pattern must not match the empty string")
	}
	rule.pattern = pattern
	return rule, nil
}

// parsePathTemplate splits a template into literal and parameter segments.
//
// A parameter is a whole segment, "{name}", and never part of one. A partial
// parameter ("v{n}", "{a}{b}") has no unambiguous escaping: where the value
// ends and the literal resumes is decided by the target's router, not by this
// code, so the same input could address different resources on two targets.
func parsePathTemplate(template string) ([]pathSegment, []string, error) {
	if template == "" {
		return nil, nil, errors.New("path is required")
	}
	if len(template) > maxPathTemplateBytes {
		return nil, nil, fmt.Errorf("path exceeds %d bytes", maxPathTemplateBytes)
	}
	if !strings.HasPrefix(template, "/") {
		return nil, nil, errors.New("path must begin with /")
	}
	if strings.ContainsAny(template, "?#") {
		return nil, nil, errors.New("path must not carry a query or fragment; " +
			"declare query names in query[]")
	}
	parts := strings.Split(template[1:], "/")
	segments := make([]pathSegment, 0, len(parts))
	var params []string
	seen := map[string]struct{}{}
	for index, part := range parts {
		last := index == len(parts)-1
		switch {
		case part == "" && last:
			// A trailing slash ("/items/") is a distinct resource on many
			// routers and is kept exactly as written.
			segments = append(segments, pathSegment{literal: ""})
		case part == "":
			return nil, nil, errors.New("path must not contain an empty segment")
		case strings.HasPrefix(part, "{") && strings.HasSuffix(part, "}"):
			name := part[1 : len(part)-1]
			if !isParamName(name) {
				return nil, nil, errors.New("path parameters must be " +
					"{name} with name matching [A-Za-z_][A-Za-z0-9_]*")
			}
			if _, duplicate := seen[name]; duplicate {
				return nil, nil, fmt.Errorf("path parameter %q appears twice", name)
			}
			seen[name] = struct{}{}
			params = append(params, name)
			segments = append(segments, pathSegment{param: name})
		case strings.ContainsAny(part, "{}"):
			return nil, nil, errors.New("path parameters must be whole " +
				"segments; a parameter inside a segment has no unambiguous " +
				"escaping")
		case part == "." || part == "..":
			return nil, nil, errors.New("path must not contain . or .. segments")
		case url.PathEscape(part) != part:
			return nil, nil, errors.New("path literal segments must not need " +
				"escaping; write them exactly as they are sent")
		default:
			segments = append(segments, pathSegment{literal: part})
		}
	}
	return segments, params, nil
}

// Binder produces the exact request for a route. It holds the static
// configuration the request depends on — the target base URL and the
// idempotency header's name — and nothing that varies between attempts.
type Binder struct {
	base              *url.URL
	idempotencyHeader string
}

// NewBinder fixes the target and the idempotency header. The base URL is
// expected to have passed targetURL (Config does this); it is re-checked here
// for the properties the binder itself relies on.
func NewBinder(base *url.URL, idempotencyHeader string) (*Binder, error) {
	if base == nil || !base.IsAbs() || base.Host == "" {
		return nil, errors.New("binder requires an absolute target base URL")
	}
	if base.RawQuery != "" || base.Fragment != "" || base.User != nil {
		return nil, errors.New("binder target base URL must carry no query, " +
			"fragment or userinfo")
	}
	if idempotencyHeader != "" && !validHeaderName(idempotencyHeader) {
		return nil, errors.New("idempotency header must be a valid HTTP header name")
	}
	copied := *base
	return &Binder{base: &copied, idempotencyHeader: idempotencyHeader}, nil
}

// BoundRequest is everything that leaves the gateway for one attempt except
// the target credential, which is attached per request at send time and is
// deliberately not part of it: the credential may rotate between attempts,
// and a value captured here would also be a value a caller could log.
type BoundRequest struct {
	Method string
	URL    string
	Header http.Header
	Body   []byte
}

// InputError is a binding refusal. It maps to the input_invalid error code,
// and its message never contains an input value — only the name of what was
// wrong — so it is safe to record and to log.
type InputError struct{ reason string }

func (e *InputError) Error() string { return "input is invalid: " + e.reason }

func inputError(format string, args ...any) error {
	return &InputError{reason: fmt.Sprintf(format, args...)}
}

// inputWire is the closed shape of an action's input.
type inputWire struct {
	Path  map[string]string `json:"path,omitempty"`
	Query map[string]string `json:"query,omitempty"`
	Body  json.RawMessage   `json:"body,omitempty"`
}

// Bind is a pure function of (route, input, executor key): the same three
// arguments produce byte-identical method, URL, headers and body on every
// call, on every replica, across restarts.
//
// That property is load-bearing twice. A target that deduplicates on key plus
// body must see the re-claim's request as the same request, and the
// idempotency-conflict rule can only read a conflict as "the effect happened"
// if the conflicting request is the one already performed. So nothing here
// reads a clock, a random source, the environment or the process: no Date
// header, no request ID, no nonce, and a fixed User-Agent.
func (b *Binder) Bind(route *Route, input json.RawMessage, key ExecutorKey) (BoundRequest, error) {
	if b == nil || route == nil {
		return BoundRequest{}, errors.New("binder and route are required")
	}
	if route.idempotency == IdempotencyKey {
		if !key.HasKey() {
			return BoundRequest{}, inputError("route %s requires the action's "+
				"executor key and the claim carried none", route.action)
		}
		if b.idempotencyHeader == "" {
			return BoundRequest{}, errors.New("route requires an idempotency " +
				"header and none is configured")
		}
	}
	if err := rejectDuplicateKeys(input); err != nil {
		return BoundRequest{}, inputError("input must be one JSON object " +
			"with no repeated key")
	}
	if err := exactKeys(input, inputFields); err != nil {
		return BoundRequest{}, inputError("input must be an object whose " +
			"only fields are path, query and body, spelled exactly")
	}
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.DisallowUnknownFields()
	var wire inputWire
	if err := decoder.Decode(&wire); err != nil {
		return BoundRequest{}, inputError("input must be an object with only " +
			"path, query and body, and path and query values must be strings")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return BoundRequest{}, inputError("input must be exactly one JSON object")
	}

	var path strings.Builder
	path.WriteString(strings.TrimSuffix(b.base.EscapedPath(), "/"))
	used := 0
	for _, segment := range route.segments {
		path.WriteByte('/')
		if segment.param == "" {
			path.WriteString(segment.literal)
			continue
		}
		value, ok := wire.Path[segment.param]
		if !ok {
			return BoundRequest{}, inputError("path parameter %q is missing", segment.param)
		}
		switch value {
		case "":
			return BoundRequest{}, inputError("path parameter %q is empty", segment.param)
		case ".", "..":
			// Escaping does not touch dots, so these would arrive as
			// traversal segments and address a different resource.
			return BoundRequest{}, inputError(
				"path parameter %q must not be . or ..", segment.param)
		}
		path.WriteString(url.PathEscape(value))
		used++
	}
	if used != len(wire.Path) {
		return BoundRequest{}, inputError("input carries a path parameter the " +
			"route does not declare")
	}

	query := url.Values{}
	for name, value := range wire.Query {
		if _, declared := route.query[name]; !declared {
			return BoundRequest{}, inputError("input carries a query parameter " +
				"the route does not declare")
		}
		query.Set(name, value)
	}

	target := b.base.Scheme + "://" + b.base.Host + path.String()
	if len(query) > 0 {
		// Encode sorts by key, which is what makes the URL a function of the
		// input rather than of map iteration order.
		target += "?" + query.Encode()
	}
	parsed, err := url.Parse(target)
	if err != nil || parsed.EscapedPath() != path.String() {
		return BoundRequest{}, inputError("input does not produce a well-formed URL")
	}

	header := http.Header{}
	header.Set("User-Agent", UserAgent)
	header.Set("Accept", "application/json")
	var body []byte
	if wire.Body != nil && route.method == http.MethodDelete {
		// InputSchema admits no body for a DELETE, and the binder agrees with
		// it: a body the registered schema says cannot exist is not sent.
		return BoundRequest{}, inputError("a DELETE route sends no body")
	}
	if wire.Body != nil {
		if bytes.Equal(bytes.TrimSpace(wire.Body), []byte("null")) {
			return BoundRequest{}, inputError("body must not be null; omit it " +
				"to send no body")
		}
		var compacted bytes.Buffer
		if err := json.Compact(&compacted, wire.Body); err != nil {
			return BoundRequest{}, inputError("body is not valid JSON")
		}
		body = compacted.Bytes()
		header.Set("Content-Type", "application/json")
	}
	if route.idempotency == IdempotencyKey {
		header.Set(b.idempotencyHeader, key.HeaderValue())
	}
	return BoundRequest{Method: route.method, URL: target, Header: header, Body: body}, nil
}

// OutputSchema is the canonical closed schema of what the gateway records on
// success. Every registered action's output_schema must equal it, because the
// fleet validates Output against the schema the gateway itself registered and
// that schema language has no maxLength or pattern: whatever the schema admits,
// a hostile target could otherwise fill.
func OutputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{` +
		`"status":{"type":"integer"},` +
		`"idempotency":{"type":"string","enum":["key","natural","unprotected","replayed","conflict"]},` +
		`"reference":{"type":"string"}},` +
		`"required":["status","idempotency"],"additionalProperties":false}`)
}

// InputSchema is the closed input schema for a route, suitable for the
// action's input_schema at registration. Registering it makes the explorer
// refuse a malformed input at enqueue, before anything is claimed, which is
// strictly earlier than the binder can.
func (r *Route) InputSchema() json.RawMessage {
	pathProperties := map[string]any{}
	for _, name := range r.params {
		pathProperties[name] = map[string]any{"type": "string"}
	}
	queryProperties := map[string]any{}
	for name := range r.query {
		queryProperties[name] = map[string]any{"type": "string"}
	}
	properties := map[string]any{}
	required := []string{}
	if len(r.params) > 0 {
		params := append([]string(nil), r.params...)
		sort.Strings(params)
		properties["path"] = map[string]any{
			"type": "object", "properties": pathProperties,
			"required": params, "additionalProperties": false,
		}
		required = append(required, "path")
	}
	if len(r.query) > 0 {
		properties["query"] = map[string]any{
			"type": "object", "properties": queryProperties,
			"additionalProperties": false,
		}
	}
	if r.method != http.MethodDelete {
		properties["body"] = map[string]any{}
	}
	schema := map[string]any{
		"type": "object", "properties": properties,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	encoded, _ := json.Marshal(schema) // maps of strings: cannot fail
	return encoded
}

// DescriptorAction is the part of a registered action the startup check reads.
type DescriptorAction struct {
	Name         string
	Effects      dispatchapi.Effects
	InputSchema  json.RawMessage
	OutputSchema json.RawMessage
}

// VerifyDescriptor is the startup check against the resolved descriptor: the
// capability exists, its actions and the route table correspond one to one,
// each action's declared effects equal its route's, each input schema is the
// route's InputSchema, and each output schema is the canonical closed schema. Any mismatch fails closed, because each one is
// a configuration in which the explorer and the gateway disagree about what an
// action does.
func VerifyDescriptor(table *RouteTable, actions []DescriptorAction) error {
	if table == nil {
		return errors.New("route table is required")
	}
	var canonical any
	if err := json.Unmarshal(OutputSchema(), &canonical); err != nil {
		return err
	}
	registered := make(map[string]DescriptorAction, len(actions))
	for _, action := range actions {
		if _, duplicate := registered[action.Name]; duplicate {
			return fmt.Errorf("descriptor declares action %q twice", action.Name)
		}
		registered[action.Name] = action
	}
	for _, name := range table.order {
		action, ok := registered[name]
		if !ok {
			return fmt.Errorf("route %q has no registered action; the "+
				"explorer would never offer work for it", name)
		}
		effects, err := canonicalEffectSet(action.Effects)
		if err != nil {
			return fmt.Errorf("action %q effects: %w", name, err)
		}
		route := table.routes[name]
		if !reflect.DeepEqual(effects, route.effects) {
			return fmt.Errorf("action %q declares effects %s and its route "+
				"%s", name, effectList(effects), effectList(route.effects))
		}
		var schema any
		if err := json.Unmarshal(action.OutputSchema, &schema); err != nil ||
			!reflect.DeepEqual(schema, canonical) {
			return fmt.Errorf("action %q output_schema is not the gateway's "+
				"canonical closed schema", name)
		}
		// The input schema is what the explorer enforces at enqueue. One that
		// admits more than the binder accepts lets work be queued, claimed —
		// setting EffectPossible — and only then refused as input_invalid.
		var input, routeInput any
		if err := json.Unmarshal(action.InputSchema, &input); err != nil ||
			json.Unmarshal(route.InputSchema(), &routeInput) != nil ||
			!reflect.DeepEqual(input, routeInput) {
			return fmt.Errorf("action %q input_schema is not the route's "+
				"InputSchema", name)
		}
	}
	for name := range registered {
		if _, ok := table.routes[name]; !ok {
			return fmt.Errorf("action %q is registered with no route; work "+
				"enqueued for it would be pulled and never performed", name)
		}
	}
	return nil
}

// canonicalEffectSet sorts a set and refuses duplicates and unknown classes.
// Unlike the fleet's own canonicalization it refuses a duplicate rather than
// collapsing it: an operator who wrote one class twice probably meant another.
func canonicalEffectSet(declared dispatchapi.Effects) (dispatchapi.Effects, error) {
	if len(declared) == 0 {
		return nil, errors.New("at least one effect is required")
	}
	seen := make(map[dispatchapi.Effect]struct{}, len(declared))
	result := make(dispatchapi.Effects, 0, len(declared))
	for _, effect := range declared {
		switch effect {
		case dispatchapi.EffectMutatesExternal, dispatchapi.EffectEgressesContent:
		case dispatchapi.EffectReadsCorpus:
			return nil, errors.New("reads-corpus is refused: the gateway " +
				"never reads the corpus")
		default:
			return nil, fmt.Errorf("unknown effect %q", string(effect))
		}
		if _, duplicate := seen[effect]; duplicate {
			return nil, fmt.Errorf("effect %q is declared twice", string(effect))
		}
		seen[effect] = struct{}{}
		result = append(result, effect)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result, nil
}

func effectList(effects dispatchapi.Effects) string {
	names := make([]string, len(effects))
	for i, effect := range effects {
		names[i] = string(effect)
	}
	return "{" + strings.Join(names, ", ") + "}"
}

// validatePointer checks RFC 6901 syntax: empty (the whole document) is not
// useful here and is refused; otherwise every token begins with '/', and '~'
// appears only as ~0 or ~1.
func validatePointer(pointer string) error {
	if pointer == "" || pointer[0] != '/' {
		return errors.New("must be a JSON pointer beginning with /")
	}
	for i := 0; i < len(pointer); i++ {
		if pointer[i] == '~' {
			if i+1 >= len(pointer) || (pointer[i+1] != '0' && pointer[i+1] != '1') {
				return errors.New("~ must be followed by 0 or 1")
			}
		}
	}
	return nil
}

// resolvePointer evaluates an RFC 6901 pointer over a decoded document.
func resolvePointer(document any, pointer string) (any, bool) {
	current := document
	for _, raw := range strings.Split(pointer[1:], "/") {
		token := strings.ReplaceAll(strings.ReplaceAll(raw, "~1", "/"), "~0", "~")
		switch node := current.(type) {
		case map[string]any:
			next, ok := node[token]
			if !ok {
				return nil, false
			}
			current = next
		case []any:
			if token == "" || (len(token) > 1 && token[0] == '0') {
				return nil, false
			}
			index, err := strconv.Atoi(token)
			if err != nil || index < 0 || index >= len(node) {
				return nil, false
			}
			current = node[index]
		default:
			return nil, false
		}
	}
	return current, true
}

func isParamName(name string) bool {
	if name == "" {
		return false
	}
	for i, c := range name {
		switch {
		case c == '_', c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z':
		case i > 0 && c >= '0' && c <= '9':
		default:
			return false
		}
	}
	return true
}

func isIdentifier(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for i, c := range name {
		switch {
		case c == '_', c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z':
		case i > 0 && (c >= '0' && c <= '9' || c == '.' || c == '-'):
		default:
			return false
		}
	}
	return true
}

// validHeaderName applies the RFC 9110 token grammar.
func validHeaderName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return true
}

// The field names each strict object admits, spelled exactly.
var (
	routeFields     = []string{"action", "method", "path", "effects", "idempotency", "query", "conflict", "retryable", "reference"}
	conflictFields  = []string{"status", "pointer", "equals"}
	referenceFields = []string{"pointer", "header", "pattern"}
	inputFields     = []string{"path", "query", "body"}
)

// exactKeys refuses an object carrying any key that is not exactly one of
// allowed.
//
// encoding/json matches object keys to struct fields case-insensitively, with
// Unicode folding ("ſ" folds to "s"), and DisallowUnknownFields does not
// change that. So {"method":"GET","METHOD":"POST"} has no duplicate key in
// the exact sense rejectDuplicateKeys checks, decodes without complaint, and
// sends whichever spelling came last — a value a reviewer reading the first
// line never sees. The same shape in an action's input would let
// {"body":{"amount":1},"BODY":{"amount":1000}} pass a reading of "body" and
// send the other one. Requiring the exact spelling closes both.
func exactKeys(raw json.RawMessage, allowed []string) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return errors.New("must be a JSON object")
	}
	for key := range object {
		known := false
		for _, name := range allowed {
			if key == name {
				known = true
				break
			}
		}
		if !known {
			return fmt.Errorf("field %q is not one of %s, spelled exactly",
				key, strings.Join(allowed, ", "))
		}
	}
	return nil
}

// exactRouteKeys applies exactKeys at every level of the route table.
func exactRouteKeys(raw []byte) error {
	var routes []json.RawMessage
	if err := json.Unmarshal(raw, &routes); err != nil {
		return fmt.Errorf("-routes must be a JSON array of routes: %w", err)
	}
	for index, route := range routes {
		if err := exactKeys(route, routeFields); err != nil {
			return fmt.Errorf("-routes[%d]: %w", index, err)
		}
		var nested map[string]json.RawMessage
		_ = json.Unmarshal(route, &nested)
		for name, fields := range map[string][]string{
			"conflict": conflictFields, "reference": referenceFields,
		} {
			value, present := nested[name]
			if !present || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				continue
			}
			if err := exactKeys(value, fields); err != nil {
				return fmt.Errorf("-routes[%d] %s: %w", index, name, err)
			}
		}
	}
	return nil
}

// rejectDuplicateKeys walks a JSON document and refuses any object that
// repeats a key. encoding/json keeps the last of two duplicates silently, so
// without this a route table could read as GET to a reviewer and POST to the
// decoder, and an input could name one path value to a schema check and
// another to the binder.
func rejectDuplicateKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			keys := map[string]struct{}{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, _ := keyToken.(string)
				if _, duplicate := keys[key]; duplicate {
					return errors.New("a JSON object repeats a key")
				}
				keys[key] = struct{}{}
				if err := walk(); err != nil {
					return err
				}
			}
			_, err := decoder.Token()
			return err
		case '[':
			for decoder.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			_, err := decoder.Token()
			return err
		}
		return nil
	}
	if err := walk(); err != nil {
		if strings.Contains(err.Error(), "repeats a key") {
			return err
		}
		return errors.New("not valid JSON")
	}
	return nil
}
