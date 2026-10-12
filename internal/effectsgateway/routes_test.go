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
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"

	dispatchapi "github.com/phrocker/shoal-oss/pkg/dispatch/api"
)

var remoteEffects = dispatchapi.Effects{dispatchapi.EffectEgressesContent, dispatchapi.EffectMutatesExternal}

const remoteEffectsJSON = `["external","egresses-content"]`

func mustParseRoutes(t *testing.T, raw string) *RouteTable {
	t.Helper()
	table, err := ParseRoutes([]byte(raw), remoteEffects)
	if err != nil {
		t.Fatalf("ParseRoutes: %v", err)
	}
	return table
}

func mustRoute(t *testing.T, table *RouteTable, action string) *Route {
	t.Helper()
	route, ok := table.Lookup(action)
	if !ok {
		t.Fatalf("route %q missing", action)
	}
	return route
}

func testKey(t *testing.T) ExecutorKey {
	t.Helper()
	key, err := ParseExecutorKey(base64.RawURLEncoding.EncodeToString(keyBytes))
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// route renders one route object with a valid default for every field the
// case does not override.
func route(overrides map[string]any) string {
	fields := map[string]any{
		"action": "charge", "method": "POST", "path": "/v1/charges",
		"effects": json.RawMessage(remoteEffectsJSON), "idempotency": "key",
	}
	for key, value := range overrides {
		if value == nil {
			delete(fields, key)
			continue
		}
		fields[key] = value
	}
	encoded, _ := json.Marshal(fields)
	return string(encoded)
}

func TestParseRoutesRefusals(t *testing.T) {
	for _, refused := range []struct {
		name, raw, cites string
	}{
		{"not JSON", `[`, "not valid JSON"},
		{"an object, not an array", `{"action":"charge"}`, "JSON array"},
		{"empty table", `[]`, "at least one route"},
		{"trailing data", `[` + route(nil) + `] []`, "JSON array"},
		{"unknown field", `[` + route(map[string]any{"timeout": 5}) + `]`, "spelled exactly"},
		// encoding/json folds case (and Unicode: ſ folds to s) when matching
		// fields, so each of these decoded with the second spelling winning.
		{"case-folded method", `[{"action":"charge","method":"GET","METHOD":"POST","path":"/v1/c","effects":` + remoteEffectsJSON + `,"idempotency":"key"}]`, "spelled exactly"},
		{"case-folded idempotency", `[{"action":"charge","method":"POST","path":"/v1/c","effects":` + remoteEffectsJSON + `,"idempotency":"key","Idempotency":"unprotected"}]`, "spelled exactly"},
		{"capitalized field alone", `[{"Action":"charge","method":"POST","path":"/v1/c","effects":` + remoteEffectsJSON + `,"idempotency":"key"}]`, "spelled exactly"},
		{"unicode-folded field", `[{"action":"charge","method":"POST","path":"/v1/c","effects":` + remoteEffectsJSON + `,"idempotency":"key","effectſ":["external"]}]`, "spelled exactly"},
		{"case-folded conflict field", `[` + route(map[string]any{"conflict": map[string]any{"status": []int{400}, "pointer": "/a", "equals": []string{"x"}, "Equals": []string{"y"}}}) + `]`, "spelled exactly"},
		{"unicode-folded conflict field", `[` + route(map[string]any{"conflict": map[string]any{"ſtatus": []int{400}, "pointer": "/a", "equals": []string{"x"}}}) + `]`, "spelled exactly"},
		{"case-folded reference field", `[` + route(map[string]any{"reference": map[string]any{"pointer": "/id", "Pattern": "x", "pattern": "y"}}) + `]`, "spelled exactly"},
		{"status-only conflict", `[` + route(map[string]any{"conflict": map[string]any{"status": []int{409}}}) + `]`, "pointer and equals are required"},
		{"duplicate key", `[{"action":"charge","method":"GET","method":"POST","path":"/v1/c","effects":` + remoteEffectsJSON + `,"idempotency":"key"}]`, "repeats a key"},
		{"duplicate action", `[` + route(nil) + `,` + route(nil) + `]`, "declared twice"},
		{"action missing", `[` + route(map[string]any{"action": nil}) + `]`, "action is required"},
		{"action with space", `[` + route(map[string]any{"action": "char ge"}) + `]`, "letters, digits"},
		{"GET", `[` + route(map[string]any{"method": "GET"}) + `]`, "performs none"},
		{"HEAD", `[` + route(map[string]any{"method": "HEAD"}) + `]`, "performs none"},
		{"lower-case method", `[` + route(map[string]any{"method": "post"}) + `]`, "upper case"},
		{"CONNECT", `[` + route(map[string]any{"method": "CONNECT"}) + `]`, "upper case"},
		{"method missing", `[` + route(map[string]any{"method": nil}) + `]`, "method is required"},
		{"natural POST", `[` + route(map[string]any{"idempotency": "natural"}) + `]`, "PUT or DELETE"},
		{"natural PATCH", `[` + route(map[string]any{"idempotency": "natural", "method": "PATCH"}) + `]`, "PUT or DELETE"},
		{"idempotency missing", `[` + route(map[string]any{"idempotency": nil}) + `]`, "idempotency is required"},
		{"idempotency unknown", `[` + route(map[string]any{"idempotency": "maybe"}) + `]`, "key, natural or unprotected"},
		{"path missing", `[` + route(map[string]any{"path": nil}) + `]`, "path is required"},
		{"path relative", `[` + route(map[string]any{"path": "v1/charges"}) + `]`, "begin with /"},
		{"path with query", `[` + route(map[string]any{"path": "/v1/charges?x=1"}) + `]`, "query or fragment"},
		{"path with fragment", `[` + route(map[string]any{"path": "/v1/charges#x"}) + `]`, "query or fragment"},
		{"empty interior segment", `[` + route(map[string]any{"path": "/v1//charges"}) + `]`, "empty segment"},
		{"dot segment", `[` + route(map[string]any{"path": "/v1/./charges"}) + `]`, ". or .."},
		{"dot-dot segment", `[` + route(map[string]any{"path": "/v1/../charges"}) + `]`, ". or .."},
		{"partial parameter", `[` + route(map[string]any{"path": "/v1/ch{id}"}) + `]`, "whole segments"},
		{"two parameters in a segment", `[` + route(map[string]any{"path": "/v1/{a}{b}"}) + `]`, "{name}"},
		{"unclosed parameter", `[` + route(map[string]any{"path": "/v1/{id"}) + `]`, "whole segments"},
		{"empty parameter name", `[` + route(map[string]any{"path": "/v1/{}"}) + `]`, "{name}"},
		{"parameter name with dash", `[` + route(map[string]any{"path": "/v1/{a-b}"}) + `]`, "{name}"},
		{"parameter name leading digit", `[` + route(map[string]any{"path": "/v1/{1a}"}) + `]`, "{name}"},
		{"repeated parameter", `[` + route(map[string]any{"path": "/v1/{id}/x/{id}"}) + `]`, "appears twice"},
		{"literal needing escape", `[` + route(map[string]any{"path": "/v1/a b"}) + `]`, "escaping"},
		{"effects missing", `[` + route(map[string]any{"effects": nil}) + `]`, "at least one effect"},
		{"effects understated", `[` + route(map[string]any{"effects": []string{"external"}}) + `]`, "do not match"},
		{"effects overstated with corpus", `[` + route(map[string]any{"effects": []string{"external", "egresses-content", "reads-corpus"}}) + `]`, "reads-corpus is refused"},
		{"effects unknown", `[` + route(map[string]any{"effects": []string{"external", "teleports"}}) + `]`, "unknown effect"},
		{"effects duplicated", `[` + route(map[string]any{"effects": []string{"external", "external", "egresses-content"}}) + `]`, "declared twice"},
		{"query name invalid", `[` + route(map[string]any{"query": []string{"a b"}}) + `]`, "query names"},
		{"query name duplicated", `[` + route(map[string]any{"query": []string{"a", "a"}}) + `]`, "declared twice"},
		{"retryable 2xx", `[` + route(map[string]any{"retryable": []int{200}}) + `]`, "400-599"},
		{"retryable 3xx", `[` + route(map[string]any{"retryable": []int{302}}) + `]`, "400-599"},
		{"retryable duplicated", `[` + route(map[string]any{"retryable": []int{503, 503}}) + `]`, "declared twice"},
		{"conflict on unprotected", `[` + route(map[string]any{"idempotency": "unprotected", "conflict": map[string]any{"status": []int{409}}}) + `]`, "nothing a conflict"},
		{"conflict without status", `[` + route(map[string]any{"conflict": map[string]any{}}) + `]`, "at least one status"},
		{"conflict 2xx", `[` + route(map[string]any{"conflict": map[string]any{"status": []int{200}}}) + `]`, "400-599"},
		{"conflict status duplicated", `[` + route(map[string]any{"conflict": map[string]any{"status": []int{409, 409}}}) + `]`, "declared twice"},
		{"conflict equals without pointer", `[` + route(map[string]any{"conflict": map[string]any{"status": []int{400}, "equals": []string{"x"}}}) + `]`, "pointer and equals are required"},
		{"conflict pointer without equals", `[` + route(map[string]any{"conflict": map[string]any{"status": []int{400}, "pointer": "/error/type"}}) + `]`, "at least one value"},
		{"conflict pointer malformed", `[` + route(map[string]any{"conflict": map[string]any{"status": []int{400}, "pointer": "error/type", "equals": []string{"x"}}}) + `]`, "beginning with /"},
		{"conflict pointer bad escape", `[` + route(map[string]any{"conflict": map[string]any{"status": []int{400}, "pointer": "/a~2", "equals": []string{"x"}}}) + `]`, "~ must be followed"},
		{"conflict empty equals value", `[` + route(map[string]any{"conflict": map[string]any{"status": []int{400}, "pointer": "/a", "equals": []string{""}}}) + `]`, "must not be empty"},
		{"conflict unknown field", `[` + route(map[string]any{"conflict": map[string]any{"status": []int{400}, "regex": "x"}}) + `]`, "spelled exactly"},
		{"reference both sources", `[` + route(map[string]any{"reference": map[string]any{"pointer": "/id", "header": "Location", "pattern": "x"}}) + `]`, "exactly one"},
		{"reference neither source", `[` + route(map[string]any{"reference": map[string]any{"pattern": "x"}}) + `]`, "exactly one"},
		{"reference without pattern", `[` + route(map[string]any{"reference": map[string]any{"pointer": "/id"}}) + `]`, "pattern is required"},
		{"reference bad pattern", `[` + route(map[string]any{"reference": map[string]any{"pointer": "/id", "pattern": "("}}) + `]`, "regular expression"},
		{"reference pattern matching empty", `[` + route(map[string]any{"reference": map[string]any{"pointer": "/id", "pattern": "[a-z]*"}}) + `]`, "empty string"},
		{"reference bad header", `[` + route(map[string]any{"reference": map[string]any{"header": "Bad Header", "pattern": "x"}}) + `]`, "header name"},
	} {
		_, err := ParseRoutes([]byte(refused.raw), remoteEffects)
		if err == nil {
			t.Errorf("%s: accepted", refused.name)
			continue
		}
		if !strings.Contains(err.Error(), refused.cites) {
			t.Errorf("%s: refused, but not by the rule under test: %v", refused.name, err)
		}
	}
}

// TestEncodingJSONFoldsFieldNames pins the premise the exact-key check exists
// for. If encoding/json ever stops folding, the check is still right, but this
// test says why it was needed.
func TestEncodingJSONFoldsFieldNames(t *testing.T) {
	var decoded struct {
		Method string `json:"method"`
		Status []int  `json:"status"`
	}
	if err := json.Unmarshal([]byte(`{"method":"GET","METHOD":"POST","ſtatus":[409]}`), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Method != "POST" || len(decoded.Status) != 1 {
		t.Fatalf("encoding/json no longer folds field names: %#v", decoded)
	}
}

func TestParseRoutesBoundsTheTable(t *testing.T) {
	routes := make([]string, MaxRoutes+1)
	for i := range routes {
		routes[i] = route(map[string]any{"action": "a" + strconv.Itoa(i)})
	}
	if _, err := ParseRoutes([]byte("["+strings.Join(routes, ",")+"]"), remoteEffects); err == nil ||
		!strings.Contains(err.Error(), "at most") {
		t.Fatalf("an oversized table = %v", err)
	}
}

func TestDerivedEffectsFollowTheTarget(t *testing.T) {
	for _, probe := range []struct {
		target string
		want   dispatchapi.Effects
	}{
		{"http://localhost:8080", dispatchapi.Effects{dispatchapi.EffectMutatesExternal}},
		{"http://LOCALHOST.:8080", dispatchapi.Effects{dispatchapi.EffectMutatesExternal}},
		{"http://127.0.0.1:8080", dispatchapi.Effects{dispatchapi.EffectMutatesExternal}},
		{"http://[::1]:8080", dispatchapi.Effects{dispatchapi.EffectMutatesExternal}},
		{"https://api.example.com", remoteEffects},
		{"https://10.0.0.5", remoteEffects},
		// Resolving to loopback is not being loopback; overstating here is
		// the explicit choice the dialer's -target-allow-private makes visible.
		{"https://loopback.example.com", remoteEffects},
	} {
		parsed, err := url.Parse(probe.target)
		if err != nil {
			t.Fatal(err)
		}
		if got := DerivedEffects(parsed); !reflect.DeepEqual(got, probe.want) {
			t.Errorf("%s: derived %v, want %v", probe.target, got, probe.want)
		}
	}
	// A loopback target's routes must declare exactly {external}.
	loopback := dispatchapi.Effects{dispatchapi.EffectMutatesExternal}
	if _, err := ParseRoutes([]byte(`[`+route(map[string]any{"effects": []string{"external"}})+`]`), loopback); err != nil {
		t.Fatalf("a loopback route declaring {external}: %v", err)
	}
	if _, err := ParseRoutes([]byte(`[`+route(nil)+`]`), loopback); err == nil {
		t.Fatal("a loopback route declaring egress was accepted; over-declaring is " +
			"silently denied by egress-forbidding policy, not safe")
	}
}

const bindTable = `[
 {"action":"charge","method":"POST","path":"/v1/accounts/{account}/charges",
  "effects":["external","egresses-content"],"idempotency":"key","query":["expand","mode"]},
 {"action":"replace","method":"PUT","path":"/v1/items/{id}",
  "effects":["egresses-content","external"],"idempotency":"natural"},
 {"action":"remove","method":"DELETE","path":"/v1/items/{id}/",
  "effects":["external","egresses-content"],"idempotency":"natural"},
 {"action":"notify","method":"PATCH","path":"/v1/notify",
  "effects":["external","egresses-content"],"idempotency":"unprotected"}
]`

func mustBinder(t *testing.T, base string) *Binder {
	t.Helper()
	parsed, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	binder, err := NewBinder(parsed, "Idempotency-Key")
	if err != nil {
		t.Fatal(err)
	}
	return binder
}

func TestBindProducesTheExactRequest(t *testing.T) {
	table := mustParseRoutes(t, bindTable)
	binder := mustBinder(t, "https://api.example.com/base/")
	key := testKey(t)
	input := json.RawMessage(`{
		"path": {"account": "acct/1 ?#%2e"},
		"query": {"mode": "fast & loose", "expand": "a"},
		"body": { "amount" : 100, "note": "x" }
	}`)
	bound, err := binder.Bind(mustRoute(t, table, "charge"), input, key)
	if err != nil {
		t.Fatal(err)
	}
	want := BoundRequest{
		Method: http.MethodPost,
		URL: "https://api.example.com/base/v1/accounts/acct%2F1%20%3F%23%252e/charges" +
			"?expand=a&mode=fast+%26+loose",
		Header: http.Header{
			"User-Agent":      {UserAgent},
			"Accept":          {"application/json"},
			"Content-Type":    {"application/json"},
			"Idempotency-Key": {base64.RawURLEncoding.EncodeToString(keyBytes)},
		},
		Body: []byte(`{"amount":100,"note":"x"}`),
	}
	if !reflect.DeepEqual(bound, want) {
		t.Fatalf("bound request\n got %#v\nwant %#v", bound, want)
	}
	// The URL parses back to the same escaped path: the account value is one
	// segment and cannot have become three.
	parsed, err := url.Parse(bound.URL)
	if err != nil || parsed.EscapedPath() != "/base/v1/accounts/acct%2F1%20%3F%23%252e/charges" {
		t.Fatalf("bound URL does not round-trip: %v %v", parsed, err)
	}
	for name := range bound.Header {
		switch name {
		case "Date", "X-Request-Id", "Traceparent":
			t.Fatalf("binder wrote a per-attempt header %q", name)
		}
	}
}

// TestBindIsPure is the property the idempotency-conflict rule rests on: the
// second claim's request must be the first claim's request, byte for byte.
func TestBindIsPure(t *testing.T) {
	table := mustParseRoutes(t, bindTable)
	binder := mustBinder(t, "https://api.example.com")
	key := testKey(t)
	// Many query keys so map iteration order would show if it leaked.
	route := mustRoute(t, table, "charge")
	input := json.RawMessage(`{"path":{"account":"a"},"query":{"mode":"1","expand":"2"},"body":{"b":[1,2,{"c":null}]}}`)
	first, err := binder.Bind(route, input, key)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		again, err := binder.Bind(route, input, key)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(first, again) {
			t.Fatalf("attempt %d bound a different request:\n%#v\n%#v", i, first, again)
		}
	}
	// Whitespace in the stored input does not change the body.
	spaced := json.RawMessage(`{ "body" : { "b" : [ 1 , 2 , { "c" : null } ] } , "query":{"expand":"2","mode":"1"}, "path":{"account":"a"} }`)
	respaced, err := binder.Bind(route, spaced, key)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, respaced) {
		t.Fatalf("formatting of the input changed the request:\n%#v\n%#v", first, respaced)
	}
	// And a different key is a different request: the key is an input.
	other := append([]byte(nil), keyBytes...)
	other[0] ^= 1
	otherKey, err := ParseExecutorKey(base64.RawURLEncoding.EncodeToString(other))
	if err != nil {
		t.Fatal(err)
	}
	rekeyed, err := binder.Bind(route, input, otherKey)
	if err != nil {
		t.Fatal(err)
	}
	if rekeyed.Header.Get("Idempotency-Key") == first.Header.Get("Idempotency-Key") {
		t.Fatal("two keys produced one idempotency header")
	}
}

func TestBindModes(t *testing.T) {
	table := mustParseRoutes(t, bindTable)
	binder := mustBinder(t, "https://api.example.com")
	key := testKey(t)

	put, err := binder.Bind(mustRoute(t, table, "replace"),
		json.RawMessage(`{"path":{"id":"7"},"body":{"name":"n"}}`), key)
	if err != nil {
		t.Fatal(err)
	}
	if put.Header.Get("Idempotency-Key") != "" {
		t.Fatal("a natural route sent the idempotency header")
	}
	if put.URL != "https://api.example.com/v1/items/7" || put.Method != http.MethodPut {
		t.Fatalf("natural PUT = %s %s", put.Method, put.URL)
	}

	del, err := binder.Bind(mustRoute(t, table, "remove"),
		json.RawMessage(`{"path":{"id":"7"}}`), ExecutorKey{})
	if err != nil {
		t.Fatalf("a natural route needs no key: %v", err)
	}
	if del.Body != nil || del.Header.Get("Content-Type") != "" {
		t.Fatalf("a request with no body declared one: %#v", del)
	}
	if del.URL != "https://api.example.com/v1/items/7/" {
		t.Fatalf("trailing slash was not kept: %s", del.URL)
	}

	patch, err := binder.Bind(mustRoute(t, table, "notify"),
		json.RawMessage(`{"body":"hello"}`), ExecutorKey{})
	if err != nil {
		t.Fatal(err)
	}
	if patch.Header.Get("Idempotency-Key") != "" || string(patch.Body) != `"hello"` {
		t.Fatalf("unprotected PATCH = %#v", patch)
	}
}

func TestBindRefusesInputItCannotBindExactly(t *testing.T) {
	table := mustParseRoutes(t, bindTable)
	binder := mustBinder(t, "https://api.example.com")
	key := testKey(t)
	charge := mustRoute(t, table, "charge")
	for _, refused := range []struct {
		name  string
		input string
		key   ExecutorKey
	}{
		{"key route without a key", `{"path":{"account":"a"}}`, ExecutorKey{}},
		{"not JSON", `{`, key},
		{"not an object", `[1]`, key},
		{"unknown top-level field", `{"path":{"account":"a"},"headers":{"X":"y"}}`, key},
		{"duplicate key", `{"path":{"account":"a","account":"b"}}`, key},
		{"duplicate top-level key", `{"path":{"account":"a"},"path":{"account":"b"}}`, key},
		{"trailing data", `{"path":{"account":"a"}} {}`, key},
		{"missing path parameter", `{"path":{}}`, key},
		{"no path at all", `{}`, key},
		{"undeclared path parameter", `{"path":{"account":"a","extra":"b"}}`, key},
		{"empty path value", `{"path":{"account":""}}`, key},
		{"dot path value", `{"path":{"account":"."}}`, key},
		{"dot-dot path value", `{"path":{"account":".."}}`, key},
		{"non-string path value", `{"path":{"account":7}}`, key},
		{"undeclared query", `{"path":{"account":"a"},"query":{"debug":"1"}}`, key},
		{"non-string query value", `{"path":{"account":"a"},"query":{"mode":["a","b"]}}`, key},
		{"null body", `{"path":{"account":"a"},"body":null}`, key},
		// Case-folded envelope keys: encoding/json would have decoded the
		// second spelling into the field, sending a body nobody read as "body".
		{"case-folded body", `{"path":{"account":"a"},"body":{"amount":1},"BODY":{"amount":1000}}`, key},
		{"capitalized path", `{"Path":{"account":"a"}}`, key},
		{"mixed-case body", `{"path":{"account":"a"},"bOdY":{"amount":1000}}`, key},
	} {
		_, err := binder.Bind(charge, json.RawMessage(refused.input), refused.key)
		var inputErr *InputError
		if !errors.As(err, &inputErr) {
			t.Errorf("%s: %v, want an InputError", refused.name, err)
			continue
		}
		// The refusal names what was wrong and never repeats a value.
		if strings.Contains(err.Error(), `"b"`) || strings.Contains(err.Error(), "debug") {
			t.Errorf("%s: refusal repeats input: %v", refused.name, err)
		}
	}
}

func TestBindRefusesABodyOnDelete(t *testing.T) {
	table := mustParseRoutes(t, bindTable)
	binder := mustBinder(t, "https://api.example.com")
	remove := mustRoute(t, table, "remove")
	_, err := binder.Bind(remove, json.RawMessage(`{"path":{"id":"7"},"body":{"cascade":true}}`), ExecutorKey{})
	var inputErr *InputError
	if !errors.As(err, &inputErr) {
		t.Fatalf("a DELETE body = %v; InputSchema admits none, so the binder must not send one", err)
	}
}

// TestConflictStatusMayAlsoBeRetryable: one status for "already done" and
// "still in flight" is configured by listing it in both.
func TestConflictStatusMayAlsoBeRetryable(t *testing.T) {
	table := mustParseRoutes(t, `[`+route(map[string]any{
		"retryable": []int{409},
		"conflict":  map[string]any{"status": []int{409}, "pointer": "/error/code", "equals": []string{"already_done"}},
	})+`]`)
	if _, ok := table.Lookup("charge"); !ok {
		t.Fatal("route missing")
	}
}

// TestBindRefusalMessagesAreFixed fails if parser text — which can quote the
// input — is reintroduced into the duplicate-key or decode refusals.
func TestBindRefusalMessagesAreFixed(t *testing.T) {
	table := mustParseRoutes(t, bindTable)
	binder := mustBinder(t, "https://api.example.com")
	charge := mustRoute(t, table, "charge")
	for _, row := range []struct {
		name, input, want string
	}{
		{"duplicate key", `{"path":{"account":"s3cret","account":"s3cret2"}}`,
			"input is invalid: input must be one JSON object with no repeated key"},
		{"truncated", `{"path":{"account":"s3cret`,
			"input is invalid: input must be one JSON object with no repeated key"},
		{"non-string path value", `{"path":{"account":12345}}`,
			"input is invalid: input must be an object with only path, query and body, " +
				"and path and query values must be strings"},
		{"non-object path", `{"path":"s3cret"}`,
			"input is invalid: input must be an object with only path, query and body, " +
				"and path and query values must be strings"},
		{"unknown key", `{"s3cret":1}`,
			"input is invalid: input must be an object whose only fields are path, " +
				"query and body, spelled exactly"},
	} {
		_, err := binder.Bind(charge, json.RawMessage(row.input), testKey(t))
		if err == nil || err.Error() != row.want {
			t.Errorf("%s: %v\nwant %q", row.name, err, row.want)
		}
	}
}

func TestBindRefusalsCarryNoParserText(t *testing.T) {
	table := mustParseRoutes(t, bindTable)
	binder := mustBinder(t, "https://api.example.com")
	_, err := binder.Bind(mustRoute(t, table, "charge"), json.RawMessage(`{"path":{"account":"sekrit`), testKey(t))
	if err == nil || strings.Contains(err.Error(), "sekrit") || strings.Contains(err.Error(), "unexpected") {
		t.Fatalf("refusal = %v", err)
	}
}

func TestNewBinderRefusesABaseItCannotJoin(t *testing.T) {
	for _, raw := range []string{
		"/relative", "https://api.example.com?x=1", "https://api.example.com#f",
		"https://user:pass@api.example.com",
	} {
		parsed, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := NewBinder(parsed, ""); err == nil {
			t.Errorf("%s: accepted", raw)
		}
	}
	parsed, _ := url.Parse("https://api.example.com")
	if _, err := NewBinder(parsed, "Bad Header"); err == nil {
		t.Error("an invalid idempotency header name was accepted")
	}
}

func TestVerifyDescriptor(t *testing.T) {
	table := mustParseRoutes(t, bindTable)
	good := func() []DescriptorAction {
		var actions []DescriptorAction
		for _, name := range table.Actions() {
			actions = append(actions, DescriptorAction{
				Name: name, Effects: dispatchapi.Effects{dispatchapi.EffectMutatesExternal, dispatchapi.EffectEgressesContent},
				InputSchema:  mustRoute(t, table, name).InputSchema(),
				OutputSchema: OutputSchema(),
			})
		}
		return actions
	}
	if err := VerifyDescriptor(table, good()); err != nil {
		t.Fatalf("a matching descriptor was refused: %v", err)
	}
	// Formatting of the registered schema is not a mismatch.
	reformatted := good()
	var schema any
	_ = json.Unmarshal(OutputSchema(), &schema)
	pretty, _ := json.MarshalIndent(schema, "", "  ")
	reformatted[0].OutputSchema = pretty
	if err := VerifyDescriptor(table, reformatted); err != nil {
		t.Fatalf("a reformatted canonical schema was refused: %v", err)
	}

	for _, refused := range []struct {
		name   string
		mutate func([]DescriptorAction) []DescriptorAction
		cites  string
	}{
		{"route with no action", func(a []DescriptorAction) []DescriptorAction { return a[1:] }, "no registered action"},
		{"action with no route", func(a []DescriptorAction) []DescriptorAction {
			return append(a, DescriptorAction{Name: "extra", Effects: remoteEffects, InputSchema: a[0].InputSchema, OutputSchema: OutputSchema()})
		}, "registered with no route"},
		{"duplicate action", func(a []DescriptorAction) []DescriptorAction { return append(a, a[0]) }, "twice"},
		{"understated effects", func(a []DescriptorAction) []DescriptorAction {
			a[0].Effects = dispatchapi.Effects{dispatchapi.EffectMutatesExternal}
			return a
		}, "declares effects"},
		{"no effects", func(a []DescriptorAction) []DescriptorAction { a[0].Effects = nil; return a }, "effects"},
		{"open output schema", func(a []DescriptorAction) []DescriptorAction {
			a[0].OutputSchema = json.RawMessage(`{"type":"object"}`)
			return a
		}, "canonical closed schema"},
		{"open input schema", func(a []DescriptorAction) []DescriptorAction {
			a[0].InputSchema = json.RawMessage(`{"type":"object"}`)
			return a
		}, "input_schema"},
		{"missing input schema", func(a []DescriptorAction) []DescriptorAction {
			a[0].InputSchema = nil
			return a
		}, "input_schema"},
		{"another route's input schema", func(a []DescriptorAction) []DescriptorAction {
			a[0].InputSchema = a[1].InputSchema
			return a
		}, "input_schema"},
		{"widened output schema", func(a []DescriptorAction) []DescriptorAction {
			a[0].OutputSchema = json.RawMessage(strings.Replace(string(OutputSchema()),
				`"additionalProperties":false`, `"additionalProperties":true`, 1))
			return a
		}, "canonical closed schema"},
	} {
		err := VerifyDescriptor(table, refused.mutate(good()))
		if err == nil || !strings.Contains(err.Error(), refused.cites) {
			t.Errorf("%s: %v", refused.name, err)
		}
	}
}

func TestInputSchemaIsClosed(t *testing.T) {
	table := mustParseRoutes(t, bindTable)
	var schema map[string]any
	if err := json.Unmarshal(mustRoute(t, table, "charge").InputSchema(), &schema); err != nil {
		t.Fatal(err)
	}
	if schema["additionalProperties"] != false {
		t.Fatal("input schema admits fields the binder refuses")
	}
	properties := schema["properties"].(map[string]any)
	path := properties["path"].(map[string]any)
	if path["additionalProperties"] != false ||
		!reflect.DeepEqual(path["required"], []any{"account"}) {
		t.Fatalf("path schema = %v", path)
	}
	if _, ok := properties["query"]; !ok {
		t.Fatal("declared query names are not in the schema")
	}
	var del map[string]any
	if err := json.Unmarshal(mustRoute(t, table, "remove").InputSchema(), &del); err != nil {
		t.Fatal(err)
	}
	if _, ok := del["properties"].(map[string]any)["body"]; ok {
		t.Fatal("a DELETE route's schema admits a body")
	}
}

func TestResolvePointer(t *testing.T) {
	document, ok := decodeBody([]byte(`{"a":{"b/c":[10,{"~d":"x"}]},"":"empty"}`))
	if !ok {
		t.Fatal("fixture did not decode")
	}
	for _, probe := range []struct {
		pointer string
		want    any
		found   bool
	}{
		{"/a/b~1c/1/~0d", "x", true},
		{"/", "empty", true},
		{"/a/b~1c/0", json.Number("10"), true},
		{"/a/b~1c/01", nil, false},
		{"/a/b~1c/2", nil, false},
		{"/a/b~1c/-1", nil, false},
		{"/a/missing", nil, false},
		{"/a/b~1c/1/~0d/deeper", nil, false},
	} {
		got, found := resolvePointer(document, probe.pointer)
		if found != probe.found || !reflect.DeepEqual(got, probe.want) {
			t.Errorf("%s = %v, %v; want %v, %v", probe.pointer, got, found, probe.want, probe.found)
		}
	}
}
