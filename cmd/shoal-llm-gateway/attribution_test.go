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

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	admissionapi "github.com/phrocker/shoal-oss/pkg/admission/api"
)

// withheldMaterial is M: over 200 bytes, multibyte, and bounded on both sides
// by multibyte runes. The edges matter. An off-by-one-rune offset leaks exactly
// one edge rune, and a test that only looked for the whole of M would pass it.
// Every non-ASCII rune of M is absent from the rest of the fixture, so finding
// any of them in what was forwarded is a leak, not a coincidence.
var withheldMaterial = "機密" + strings.Repeat(
	"報告書の第三章は公開されない。Ωμέγα σχέδιο φάση. ", 4) + "終"

const (
	toolPrefix = "Lookup result: "
	toolSuffix = " -- end of result."
)

func digestOf(text string) string {
	sum := sha256.Sum256([]byte(text))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

type attributionSpec struct {
	Reference string `json:"reference"`
	Message   int    `json:"message"`
	Part      *int   `json:"part,omitempty"`
	Start     int    `json:"start"`
	End       int    `json:"end"`
	SHA256    string `json:"sha256"`
}

// attributeSpan attributes text[start:end] of a message to a reference with a
// correct digest.
func attributeSpan(reference string, message int, text string, start, end int) attributionSpec {
	return attributionSpec{
		Reference: reference, Message: message, Start: start, End: end,
		SHA256: digestOf(text[start:end]),
	}
}

// toolContent is the string the material sits in, and the span it occupies.
func toolContent() (string, int, int) {
	text := toolPrefix + withheldMaterial + toolSuffix
	return text, len(toolPrefix), len(toolPrefix) + len(withheldMaterial)
}

// acceptanceMessages is a realistic tool round trip: a system prompt, the
// user's request, an assistant turn that called a tool, and the tool's result,
// which carries M.
func acceptanceMessages() []any {
	content, _, _ := toolContent()
	return []any{
		map[string]any{"role": "system", "content": "You are a careful assistant."},
		map[string]any{"role": "user", "content": "Summarise what the lookup tool returned."},
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
			map[string]any{"id": "call_1", "type": "function", "function": map[string]any{
				"name": "lookup", "arguments": `{"q":"report"}`,
			}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "call_1", "content": content},
	}
}

func acceptanceAttribution() []attributionSpec {
	content, start, end := toolContent()
	return []attributionSpec{attributeSpan(docB, 3, content, start, end)}
}

func encodeBody(t testing.TB, body map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// acceptanceBody declares docA and docB, attributes M to docB, and lets a case
// change anything.
func acceptanceBody(t testing.TB, change func(map[string]any)) string {
	t.Helper()
	body := map[string]any{
		"model":             "gpt",
		"messages":          acceptanceMessages(),
		"shoal_references":  []string{docA, docB},
		"shoal_attribution": acceptanceAttribution(),
	}
	if change != nil {
		change(body)
	}
	return encodeBody(t, body)
}

// forwardedStrings is every string, key or value, the provider would decode.
func forwardedStrings(forwarded []byte) ([]string, error) {
	decoder := json.NewDecoder(bytes.NewReader(forwarded))
	decoder.UseNumber()
	var found []string
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return found, nil
		}
		if err != nil {
			return nil, err
		}
		if text, ok := token.(string); ok {
			found = append(found, text)
		}
	}
}

// escapeRunes renders text the way a JSON encoder may: every non-ASCII rune as
// \\uXXXX, in either case.
func escapeRunes(text string, upper bool) string {
	format := `\u%04x`
	if upper {
		format = `\u%04X`
	}
	var builder strings.Builder
	for _, r := range text {
		if r < utf8.RuneSelf {
			builder.WriteRune(r)
			continue
		}
		for _, unit := range utf16.Encode([]rune{r}) {
			fmt.Fprintf(&builder, format, unit)
		}
	}
	return builder.String()
}

// withholdViolations is the acceptance assertion, and every mutation is run
// through it too: a regression has to fail this function, not a weaker copy.
//
// M must be absent from the raw bytes, plain and \u-escaped, and from every
// decoded string. So must every multibyte rune of M, because a withhold that
// selected the wrong span leaks an edge of M rather than all of it. And the
// placeholder must be present, so "absent" cannot be satisfied by dropping the
// message or the call.
func withholdViolations(forwarded []byte, material string) []string {
	var violations []string
	raw := string(forwarded)
	marshalled, _ := json.Marshal(material)
	forms := map[string]string{
		"plain":         material,
		"json-encoded":  strings.Trim(string(marshalled), `"`),
		`\u lower-case`: escapeRunes(material, false),
		`\u upper-case`: escapeRunes(material, true),
	}
	for name, form := range forms {
		if strings.Contains(raw, form) {
			violations = append(violations, "the raw body carries the material ("+name+")")
		}
	}
	strings_, err := forwardedStrings(forwarded)
	if err != nil {
		return append(violations, "the forwarded body does not decode: "+err.Error())
	}
	placeholder := false
	for _, text := range strings_ {
		if strings.Contains(text, material) {
			violations = append(violations, "a decoded string carries the material")
		}
		placeholder = placeholder || strings.Contains(text, withheldPlaceholder)
	}
	seen := map[rune]bool{}
	for _, r := range material {
		if r < utf8.RuneSelf || seen[r] {
			continue
		}
		seen[r] = true
		single := string(r)
		if strings.Contains(raw, single) || strings.Contains(raw, escapeRunes(single, false)) ||
			strings.Contains(raw, escapeRunes(single, true)) {
			violations = append(violations, fmt.Sprintf("the raw body carries a rune of the material (%U)", r))
		}
		for _, text := range strings_ {
			if strings.Contains(text, single) {
				violations = append(violations, fmt.Sprintf("a decoded string carries a rune of the material (%U)", r))
				break
			}
		}
	}
	if !placeholder {
		violations = append(violations, "the placeholder is absent")
	}
	return violations
}

func assertWithheld(t *testing.T, forwarded []byte, material string) {
	t.Helper()
	if violations := withholdViolations(forwarded, material); len(violations) != 0 {
		t.Fatalf("withheld material reached the provider: %v\n%s", violations, forwarded)
	}
}

func messageAt(t *testing.T, forwarded []byte, index int) map[string]json.RawMessage {
	t.Helper()
	var body struct {
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(forwarded, &body); err != nil {
		t.Fatal(err)
	}
	if index >= len(body.Messages) {
		t.Fatalf("forwarded %d messages, wanted index %d", len(body.Messages), index)
	}
	return body.Messages[index]
}

func stringField(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		t.Fatalf("%s is not a string: %v", raw, err)
	}
	return text
}

// TestWithheldMaterialNeverReachesTheProvider is #426's acceptance test.
//
// M sits inside a tool message and is attributed to docB, which the plane
// withholds. What reaches the provider must carry neither M nor any rune of it,
// in any encoding, and must carry the placeholder where M was.
func TestWithheldMaterialNeverReachesTheProvider(t *testing.T) {
	plane := newFakePlane(t, admissionapi.OutcomeObligated, []string{docB})
	upstream := newFakeUpstream(t)
	governed, logged := newTestProxy(t, plane, upstream)

	recorder := post(t, governed, acceptanceBody(t, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if upstream.calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", upstream.calls)
	}
	assertWithheld(t, upstream.received, withheldMaterial)

	// Replaced in place, with the surrounding text intact and nothing else in
	// the string: the exact value, not a containment check.
	tool := messageAt(t, upstream.received, 3)
	if got, want := stringField(t, tool["content"]), toolPrefix+withheldPlaceholder+toolSuffix; got != want {
		t.Fatalf("tool content = %q, want %q", got, want)
	}
	if got := stringField(t, tool["tool_call_id"]); got != "call_1" {
		t.Fatalf("tool_call_id = %q", got)
	}
	// The messages nobody attributed are untouched.
	if got := stringField(t, messageAt(t, upstream.received, 1)["content"]); got !=
		"Summarise what the lookup tool returned." {
		t.Fatalf("an unattributed message changed: %q", got)
	}
	if !strings.Contains(string(messageAt(t, upstream.received, 2)["tool_calls"]), "lookup") {
		t.Fatalf("the assistant's tool call was lost: %s", upstream.received)
	}
	// Neither extension, nor the IDs, nor the digest travel to the provider.
	content, start, end := toolContent()
	for _, absent := range []string{
		shoalAttributionField, shoalReferencesField, docA, docB,
		digestOf(content[start:end]),
	} {
		if strings.Contains(string(upstream.received), absent) {
			t.Fatalf("%q reached the provider: %s", absent, upstream.received)
		}
	}
	// Nor do they travel to the plane, whose report is the ordinary success.
	everything, _ := json.Marshal(plane.requests)
	if strings.Contains(string(everything), digestOf(content[start:end])) ||
		strings.Contains(string(everything), withheldMaterial) {
		t.Fatalf("the plane received a digest or the material: %s", everything)
	}
	if len(plane.reports) != 1 || plane.reports[0].Failed {
		t.Fatalf("reports = %+v, want one success", plane.reports)
	}
	assertLogsCountsOnly(t, *logged)
}

// TestTheMutationsTheAcceptanceTestMustCatch runs each regression #426 names
// through withholdViolations, and requires it to fail there. Each first checks
// that the real gateway does not produce the mutated outcome on the same
// fixture, so the case is about the mutation and not about the fixture.
func TestTheMutationsTheAcceptanceTestMustCatch(t *testing.T) {
	parse := func(t *testing.T, body string) (chatRequest, []string) {
		t.Helper()
		parsed, err := parseChatRequest([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		references, err := parsed.references()
		if err != nil {
			t.Fatal(err)
		}
		return parsed, references
	}

	t.Run("stripping only the ID", func(t *testing.T) {
		// The behaviour #426 was filed against: the withheld ID removed from
		// the declaration, every message forwarded.
		parsed, _ := parse(t, acceptanceBody(t, nil))
		mutated, err := json.Marshal(parsed.outbound())
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(mutated), docB) {
			t.Fatal("the mutation did not even strip the ID")
		}
		if len(withholdViolations(mutated, withheldMaterial)) == 0 {
			t.Fatal("the acceptance assertion passed a body that stripped only the ID")
		}
	})

	t.Run("no digest check, offsets off by one rune", func(t *testing.T) {
		// The caller's offsets are one rune late at both ends; its digest is
		// of the real material. The span still lies on rune boundaries, so
		// only the digest can tell.
		content, start, end := toolContent()
		_, first := utf8.DecodeRuneInString(content[start:])
		_, next := utf8.DecodeRuneInString(content[end:])
		shifted := attributionSpec{
			Reference: docB, Message: 3, Start: start + first, End: end + next,
			SHA256: digestOf(withheldMaterial),
		}
		body := acceptanceBody(t, func(body map[string]any) {
			body["shoal_attribution"] = []attributionSpec{shifted}
		})

		plane := newFakePlane(t, admissionapi.OutcomeObligated, []string{docB})
		upstream := newFakeUpstream(t)
		governed, _ := newTestProxy(t, plane, upstream)
		if recorder := post(t, governed, body); recorder.Code != http.StatusBadRequest {
			t.Fatalf("the real gateway answered %d, want 400", recorder.Code)
		}
		if len(plane.requests) != 0 || upstream.calls != 0 {
			t.Fatal("a digest mismatch reached the plane or the provider")
		}

		parsed, references := parse(t, body)
		located, err := parsed.locateAttribution(references) // verifyDigests skipped
		if err != nil {
			t.Fatalf("the shifted span should pass every check but the digest: %v", err)
		}
		applied := parsed.applyObligations([]string{docB}, references, located)
		if applied.refusal != "" {
			t.Fatalf("the mutation refused (%s); it should forward", applied.refusal)
		}
		if len(withholdViolations(applied.body, withheldMaterial)) == 0 {
			t.Fatal("the acceptance assertion passed a body whose span was one rune off")
		}
	})

	t.Run("no residue check", func(t *testing.T) {
		// The RAG duplicate: M attributed in the tool message and pasted, not
		// attributed, into the user's turn.
		body := acceptanceBody(t, func(body map[string]any) {
			messages := body["messages"].([]any)
			messages[1] = map[string]any{
				"role": "user", "content": "As quoted earlier: " + withheldMaterial,
			}
		})

		plane := newFakePlane(t, admissionapi.OutcomeObligated, []string{docB})
		upstream := newFakeUpstream(t)
		governed, _ := newTestProxy(t, plane, upstream)
		if recorder := post(t, governed, body); recorder.Code != http.StatusForbidden {
			t.Fatalf("the real gateway answered %d, want 403", recorder.Code)
		}
		if upstream.calls != 0 {
			t.Fatal("a residue hit reached the provider")
		}

		parsed, references := parse(t, body)
		segments, err := parsed.attribution(references)
		if err != nil {
			t.Fatal(err)
		}
		mutated, _, err := parsed.withhold(map[string]struct{}{docB: {}}, segments) // residueFound skipped
		if err != nil {
			t.Fatal(err)
		}
		if len(withholdViolations(mutated, withheldMaterial)) == 0 {
			t.Fatal("the acceptance assertion passed a body with an unattributed copy")
		}
	})
}

// TestAnAllowedReferenceIsForwardedUnchanged: attribution to a reference the
// plane did not withhold changes nothing about its content.
func TestAnAllowedReferenceIsForwardedUnchanged(t *testing.T) {
	const allowed = "The public appendix lists every published chapter, and it may be quoted freely in full. " +
		"It is attributed to docA, which the plane allows, so it must arrive byte for byte as sent."
	userText := "Context: " + allowed
	for _, withhold := range [][]string{nil, {docB}} {
		plane := newFakePlane(t, admissionapi.OutcomeAllowed, withhold)
		if withhold != nil {
			plane.outcome = admissionapi.OutcomeObligated
		}
		upstream := newFakeUpstream(t)
		governed, _ := newTestProxy(t, plane, upstream)
		body := acceptanceBody(t, func(body map[string]any) {
			messages := body["messages"].([]any)
			messages[1] = map[string]any{"role": "user", "content": userText}
			body["shoal_attribution"] = append(acceptanceAttribution(),
				attributeSpan(docA, 1, userText, len("Context: "), len(userText)))
		})
		recorder := post(t, governed, body)
		if recorder.Code != http.StatusOK {
			t.Fatalf("withhold=%v: status = %d: %s", withhold, recorder.Code, recorder.Body.String())
		}
		if got := stringField(t, messageAt(t, upstream.received, 1)["content"]); got != userText {
			t.Fatalf("withhold=%v: the allowed segment changed: %q", withhold, got)
		}
		tool := stringField(t, messageAt(t, upstream.received, 3)["content"])
		if withhold == nil && !strings.Contains(tool, withheldMaterial) {
			t.Fatalf("nothing was withheld, yet the tool content changed: %q", tool)
		}
		if withhold != nil {
			assertWithheld(t, upstream.received, withheldMaterial)
		}
		if strings.Contains(string(upstream.received), shoalAttributionField) {
			t.Fatalf("withhold=%v: the attribution reached the provider", withhold)
		}
	}
}

// TestAnUnmodifiedClientKeepsTheAllowPath: no attribution means the parser and
// the forwarded body are exactly what they were before #426, including the
// permissive decode — strictness is the price of attribution, not of the call.
func TestAnUnmodifiedClientKeepsTheAllowPath(t *testing.T) {
	for _, body := range []string{
		plainCall,
		`{"model":"gpt","messages":[{"role":"user","content":"hi"}],"shoal_references":["` + docA + `"]}`,
		// A duplicate key is tolerated without attribution, as it always was.
		`{"model":"gpt","messages":[{"role":"user","content":"a","content":"b"}]}`,
	} {
		plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
		upstream := newFakeUpstream(t)
		governed, logged := newTestProxy(t, plane, upstream)
		if recorder := post(t, governed, body); recorder.Code != http.StatusOK {
			t.Fatalf("%s: status = %d: %s", body, recorder.Code, recorder.Body.String())
		}
		if upstream.calls != 1 {
			t.Fatalf("%s: upstream calls = %d", body, upstream.calls)
		}
		for _, line := range *logged {
			if strings.Contains(line, "obligation") {
				t.Fatalf("an allowed call logged an obligation: %q", line)
			}
		}
	}
}

// TestAMalformedAttributionIsRefusedBeforeAdmission is every 400: each spends
// no decision and reaches no provider.
func TestAMalformedAttributionIsRefusedBeforeAdmission(t *testing.T) {
	content, start, end := toolContent()
	good := acceptanceAttribution()[0]
	with := func(change func(*attributionSpec)) func(map[string]any) {
		return func(body map[string]any) {
			entry := good
			change(&entry)
			body["shoal_attribution"] = []attributionSpec{entry}
		}
	}
	raw := func(value string) func(map[string]any) {
		return func(body map[string]any) { body["shoal_attribution"] = json.RawMessage(value) }
	}
	partContent := []any{
		map[string]any{"type": "text", "text": content},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.test/a.png"}},
	}
	zero, one, two := 0, 1, 2
	// One-byte segments of an ASCII message, each valid on its own, so the
	// count is the only thing wrong with the larger list.
	filler := strings.Repeat("x", maxAttributions+1)
	entries := func(count int) []attributionSpec {
		list := make([]attributionSpec, count)
		for i := range list {
			list[i] = attributeSpan(docB, 1, filler, i, i+1)
		}
		return list
	}
	withFiller := func(count int) func(map[string]any) {
		return func(body map[string]any) {
			body["messages"].([]any)[1] = map[string]any{"role": "user", "content": filler}
			body["shoal_attribution"] = entries(count)
		}
	}
	_, firstRune := utf8.DecodeRuneInString(content[start:])

	cases := []struct {
		name   string
		change func(map[string]any)
		body   string
	}{
		{name: "attribution without shoal_references", change: func(body map[string]any) {
			delete(body, "shoal_references")
		}},
		{name: "attribution with empty shoal_references", change: func(body map[string]any) {
			body["shoal_references"] = []string{}
		}},
		{name: "attribution is not an array", change: raw(`{"reference":"x"}`)},
		{name: "attribution is null", change: raw(`null`)},
		{name: "more than the entry bound", change: withFiller(maxAttributions + 1)},
		{name: "an unknown field", change: raw(`[{"reference":"` + docB + `","message":3,"start":` +
			fmt.Sprint(start) + `,"end":` + fmt.Sprint(end) + `,"sha256":"` + good.SHA256 + `","note":"x"}]`)},
		{name: "a key that matches only up to case", change: raw(`[{"Reference":"` + docB + `","message":3,"start":` +
			fmt.Sprint(start) + `,"end":` + fmt.Sprint(end) + `,"sha256":"` + good.SHA256 + `"}]`)},
		{name: "a missing digest", change: raw(`[{"reference":"` + docB + `","message":3,"start":` +
			fmt.Sprint(start) + `,"end":` + fmt.Sprint(end) + `}]`)},
		{name: "a missing message", change: raw(`[{"reference":"` + docB + `","start":` +
			fmt.Sprint(start) + `,"end":` + fmt.Sprint(end) + `,"sha256":"` + good.SHA256 + `"}]`)},
		{name: "a fractional offset", change: raw(`[{"reference":"` + docB + `","message":3,"start":1.5,"end":` +
			fmt.Sprint(end) + `,"sha256":"` + good.SHA256 + `"}]`)},
		{name: "an undeclared reference", change: with(func(e *attributionSpec) { e.Reference = docNeverDeclared })},
		{name: "a negative message", change: with(func(e *attributionSpec) { e.Message = -1 })},
		{name: "a message out of range", change: with(func(e *attributionSpec) { e.Message = 4 })},
		{name: "a message with null content", change: with(func(e *attributionSpec) { e.Message = 2 })},
		{name: "a message with no string role", change: func(body map[string]any) {
			delete(body["messages"].([]any)[3].(map[string]any), "role")
		}},
		{name: "a part on string content", change: with(func(e *attributionSpec) { e.Part = &zero })},
		{name: "no part on array content", change: func(body map[string]any) {
			body["messages"].([]any)[3].(map[string]any)["content"] = partContent
		}},
		{name: "a part that is not text", change: func(body map[string]any) {
			body["messages"].([]any)[3].(map[string]any)["content"] = partContent
			entry := good
			entry.Part = &one
			body["shoal_attribution"] = []attributionSpec{entry}
		}},
		{name: "a part out of range", change: func(body map[string]any) {
			body["messages"].([]any)[3].(map[string]any)["content"] = partContent
			entry := good
			entry.Part = &two
			body["shoal_attribution"] = []attributionSpec{entry}
		}},
		{name: "an empty range", change: with(func(e *attributionSpec) { e.End = e.Start })},
		{name: "a reversed range", change: with(func(e *attributionSpec) { e.Start, e.End = e.End, e.Start })},
		{name: "a negative start", change: with(func(e *attributionSpec) { e.Start = -1 })},
		{name: "an end past the text", change: with(func(e *attributionSpec) { e.End = len(content) + 1 })},
		{name: "a start inside a rune", change: with(func(e *attributionSpec) {
			e.Start = start + 1
			e.SHA256 = digestOf(content[e.Start:e.End])
		})},
		{name: "an end inside a rune", change: with(func(e *attributionSpec) {
			e.End = start + firstRune - 1
			e.SHA256 = digestOf(content[e.Start:e.End])
		})},
		{name: "a digest of other bytes", change: with(func(e *attributionSpec) { e.SHA256 = digestOf("something else") })},
		{name: "a padded digest", change: with(func(e *attributionSpec) { e.SHA256 += "=" })},
		{name: "a short digest", change: with(func(e *attributionSpec) {
			e.SHA256 = base64.RawURLEncoding.EncodeToString([]byte("short"))
		})},
		{name: "overlap with another reference", change: func(body map[string]any) {
			body["shoal_attribution"] = []attributionSpec{good,
				attributeSpan(docA, 3, content, end-3, len(content))}
		}},
		{name: "overlap with the same reference", change: func(body map[string]any) {
			body["shoal_attribution"] = []attributionSpec{good, good}
		}},
		// The parser differential: the gateway reads one value, the provider
		// may read the other. With attribution present the body is refused.
		{name: "a duplicate key inside a message", body: `{"model":"gpt","messages":[` +
			`{"role":"user","content":"` + withheldMaterial + `","content":"hi"},` +
			`{"role":"tool","content":` + mustJSON(content) + `}],` +
			`"shoal_references":["` + docB + `"],"shoal_attribution":[{"reference":"` + docB +
			`","message":1,"start":` + fmt.Sprint(start) + `,"end":` + fmt.Sprint(end) +
			`,"sha256":"` + good.SHA256 + `"}]}`},
		{name: "a duplicate attribution field", body: `{"model":"gpt","messages":[{"role":"tool","content":` +
			mustJSON(content) + `}],"shoal_references":["` + docB + `"],` +
			`"shoal_attribution":[],"shoal_attribution":[{"reference":"` + docB +
			`","message":0,"start":` + fmt.Sprint(start) + `,"end":` + fmt.Sprint(end) +
			`,"sha256":"` + good.SHA256 + `"}]}`},
	}
	for _, probe := range cases {
		t.Run(probe.name, func(t *testing.T) {
			body := probe.body
			if body == "" {
				body = acceptanceBody(t, probe.change)
			}
			plane := newFakePlane(t, admissionapi.OutcomeObligated, []string{docB})
			upstream := newFakeUpstream(t)
			governed, logged := newTestProxy(t, plane, upstream)
			recorder := post(t, governed, body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", recorder.Code, recorder.Body.String())
			}
			if len(plane.requests) != 0 || len(plane.reports) != 0 {
				t.Fatalf("a malformed attribution made %d plane calls",
					len(plane.requests)+len(plane.reports))
			}
			if upstream.calls != 0 {
				t.Fatal("a malformed attribution reached the provider")
			}
			if strings.Contains(recorder.Body.String(), withheldMaterial) {
				t.Fatalf("the refusal echoes the material: %s", recorder.Body.String())
			}
			if len(*logged) != 0 {
				t.Fatalf("a 400 was logged: %q", *logged)
			}
		})
	}

	// The bound itself is reachable, or it refuses the feature.
	plane := newFakePlane(t, admissionapi.OutcomeObligated, []string{docB})
	upstream := newFakeUpstream(t)
	governed, _ := newTestProxy(t, plane, upstream)
	recorder := post(t, governed, acceptanceBody(t, withFiller(maxAttributions)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("exactly %d entries = %d: %s", maxAttributions, recorder.Code, recorder.Body.String())
	}
}

func mustJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

// TestAWithholdThatCannotBeMetIsRefusedBeforeTheProvider is every refusal:
// obligation_unsatisfiable, reported to the plane, zero provider hits.
func TestAWithholdThatCannotBeMetIsRefusedBeforeTheProvider(t *testing.T) {
	content, start, end := toolContent()
	cases := []struct {
		name, class string
		withhold    []string
		change      func(map[string]any)
	}{
		{"a withheld reference the caller never declared", refusalUndeclared,
			[]string{docNeverDeclared}, nil},
		{"a withheld reference with no attribution field", refusalUnattributed,
			[]string{docB}, func(body map[string]any) { delete(body, "shoal_attribution") }},
		{"a withheld reference with an empty attribution", refusalUnattributed,
			[]string{docB}, func(body map[string]any) { body["shoal_attribution"] = []attributionSpec{} }},
		{"a withheld reference with no attribution of its own", refusalUnattributed,
			[]string{docA, docB}, nil},
		{"the material pasted again in another message", refusalResidue,
			[]string{docB}, func(body map[string]any) {
				body["messages"].([]any)[1] = map[string]any{
					"role": "user", "content": "Quoting: " + withheldMaterial}
			}},
		{"the material again in the same string, outside the span", refusalResidue,
			[]string{docB}, func(body map[string]any) {
				twice := content + " again: " + withheldMaterial
				body["messages"].([]any)[3].(map[string]any)["content"] = twice
				body["shoal_attribution"] = []attributionSpec{attributeSpan(docB, 3, twice, start, end)}
			}},
		{"the material in a tool call's arguments", refusalResidue,
			[]string{docB}, func(body map[string]any) {
				call := body["messages"].([]any)[2].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
				call["function"].(map[string]any)["arguments"] = mustJSON(map[string]string{"q": withheldMaterial})
			}},
		{"the material in a field the gateway does not know", refusalResidue,
			[]string{docB}, func(body map[string]any) {
				body["a_future_provider_field"] = map[string]any{"nested": []any{withheldMaterial}}
			}},
		{"the material in a text part", refusalResidue,
			[]string{docB}, func(body map[string]any) {
				body["messages"].([]any)[1] = map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "text", "text": withheldMaterial[3:]}}}
			}},
		{"the material under an allowed reference's attribution", refusalResidue,
			[]string{docB}, func(body map[string]any) {
				pasted := "Again: " + withheldMaterial
				body["messages"].([]any)[1] = map[string]any{"role": "user", "content": pasted}
				body["shoal_attribution"] = append(acceptanceAttribution(),
					attributeSpan(docA, 1, pasted, len("Again: "), len(pasted)))
			}},
	}
	for _, probe := range cases {
		t.Run(probe.name, func(t *testing.T) {
			plane := newFakePlane(t, admissionapi.OutcomeObligated, probe.withhold)
			upstream := newFakeUpstream(t)
			governed, logged := newTestProxy(t, plane, upstream)
			recorder := post(t, governed, acceptanceBody(t, probe.change))
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403: %s", recorder.Code, recorder.Body.String())
			}
			if upstream.calls != 0 {
				t.Fatalf("a refused call reached the provider: %s", upstream.received)
			}
			if !strings.Contains(recorder.Body.String(), "obligation_unsatisfiable") {
				t.Fatalf("refusal = %s", recorder.Body.String())
			}
			assertRefusalNamesNothing(t, recorder.Body.String())
			if len(plane.reports) != 1 || !plane.reports[0].Failed ||
				plane.reports[0].ErrorCode != "obligation_unsatisfiable" {
				t.Fatalf("reports = %+v", plane.reports)
			}
			if joined := strings.Join(*logged, "\n"); !strings.Contains(joined, "class="+probe.class) {
				t.Fatalf("logged %q, want class %s", joined, probe.class)
			}
			assertLogsCountsOnly(t, *logged)
		})
	}
}

// TestTheResidueThresholdIsNinetySixBytesAtEveryAlignment checks the sampling
// arithmetic rather than trusting it: a 96-byte copy of a withheld segment is
// found wherever it starts, and a 95-byte one is not.
func TestTheResidueThresholdIsNinetySixBytesAtEveryAlignment(t *testing.T) {
	var builder strings.Builder
	state := uint32(2463534242)
	for builder.Len() < 400 {
		state ^= state << 13
		state ^= state >> 17
		state ^= state << 5
		builder.WriteByte(byte('a' + state%26))
	}
	segment := builder.String()
	for start := 0; start+residueRun <= len(segment); start++ {
		for _, length := range []int{residueRun - 1, residueRun} {
			if start+length > len(segment) {
				continue
			}
			body := mustJSON(map[string]any{"messages": []any{map[string]any{
				"role": "user", "content": "<" + segment[start:start+length] + ">"}}})
			got := residueFound([]byte(body), []string{segment})
			want := residueClean
			if length >= residueRun {
				want = residueHit
			}
			if got != want {
				t.Fatalf("a %d-byte copy at offset %d: result %d, want %d", length, start, got, want)
			}
		}
	}
}

// TestTheResidueCheckIsBounded: the cost is bounded by body size, and a body
// that exhausts the bound is refused rather than forwarded unexamined.
func TestTheResidueCheckIsBounded(t *testing.T) {
	// 256 withheld segments with the same 64 bytes, and a long run of those
	// bytes elsewhere. Every position of the run hashes to every sample, and
	// no confirmation ever reaches 96 bytes, so without a bound this is
	// 256 comparisons per byte of the run.
	block := strings.Repeat("a", residueWindow)
	tool := strings.Repeat(block+"|", maxAttributions)
	attribution := make([]attributionSpec, 0, maxAttributions)
	for i := range maxAttributions {
		offset := i * (residueWindow + 1)
		attribution = append(attribution, attributeSpan(docB, 1, tool, offset, offset+residueWindow))
	}
	body := encodeBody(t, map[string]any{
		"model": "gpt",
		"messages": []any{
			map[string]any{"role": "user", "content": strings.Repeat("a", 1<<20)},
			map[string]any{"role": "tool", "content": tool},
		},
		"shoal_references":  []string{docB},
		"shoal_attribution": attribution,
	})
	plane := newFakePlane(t, admissionapi.OutcomeObligated, []string{docB})
	upstream := newFakeUpstream(t)
	governed, logged := newTestProxy(t, plane, upstream)
	began := time.Now()
	recorder := post(t, governed, body)
	if elapsed := time.Since(began); elapsed > 10*time.Second {
		t.Fatalf("the residue check took %v", elapsed)
	}
	if recorder.Code != http.StatusForbidden || upstream.calls != 0 {
		t.Fatalf("status = %d, upstream calls = %d", recorder.Code, upstream.calls)
	}
	if joined := strings.Join(*logged, "\n"); !strings.Contains(joined, "class="+refusalResidueBound) {
		t.Fatalf("logged %q", joined)
	}

	// And the budget is what decides: with none, any candidate exhausts it.
	if got := residueFoundWithin([]byte(mustJSON(block+"x")), []string{block + "y"}, 0); got != residueExhausted {
		t.Fatalf("a zero budget gave %d", got)
	}
}

// TestThePlaceholderIsConstant: segments of different lengths, in string
// content and in a text part, all become exactly the placeholder — no length,
// no ID, no index.
func TestThePlaceholderIsConstant(t *testing.T) {
	short := "短い秘密の断片"
	long := strings.Repeat("長い秘密の段落。", 12)
	stringContent := "A " + short + " B " + long + " C"
	partText := "D " + long + short + " E"
	body := encodeBody(t, map[string]any{
		"model": "gpt",
		"messages": []any{
			map[string]any{"role": "tool", "content": stringContent},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.test/x.png"}},
				map[string]any{"type": "text", "text": partText},
			}},
		},
		"shoal_references": []string{docB},
		"shoal_attribution": []attributionSpec{
			attributeSpan(docB, 0, stringContent, 2, 2+len(short)),
			attributeSpan(docB, 0, stringContent, 2+len(short)+3, len(stringContent)-2),
			func() attributionSpec {
				one := 1
				spec := attributeSpan(docB, 1, partText, 2, len(partText)-2)
				spec.Part = &one
				return spec
			}(),
		},
	})
	plane := newFakePlane(t, admissionapi.OutcomeObligated, []string{docB})
	upstream := newFakeUpstream(t)
	governed, _ := newTestProxy(t, plane, upstream)
	if recorder := post(t, governed, body); recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if got, want := stringField(t, messageAt(t, upstream.received, 0)["content"]),
		"A "+withheldPlaceholder+" B "+withheldPlaceholder+" C"; got != want {
		t.Fatalf("string content = %q, want %q", got, want)
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(messageAt(t, upstream.received, 1)["content"], &parts); err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 || !strings.Contains(string(parts[0]["image_url"]), "x.png") {
		t.Fatalf("the image part changed: %+v", parts)
	}
	if got, want := stringField(t, parts[1]["text"]), "D "+withheldPlaceholder+" E"; got != want {
		t.Fatalf("text part = %q, want %q", got, want)
	}
	if withheldPlaceholder != "[shoal: content withheld]" {
		t.Fatalf("the placeholder changed: %q", withheldPlaceholder)
	}
}

// logLine is every line the attribution paths may write. Counts and a class:
// no content, no digest, no reference ID, no offset.
var logLine = regexp.MustCompile(
	`^(obligation applied request_id=[A-Za-z0-9_-]+ references=\d+ segments=\d+ bytes=\d+` +
		`|obligation unsatisfiable request_id=[A-Za-z0-9_-]+ withheld=\d+ class=[a-z_]+)$`)

func assertLogsCountsOnly(t *testing.T, logged []string) {
	t.Helper()
	content, start, end := toolContent()
	for _, line := range logged {
		if !logLine.MatchString(line) {
			t.Fatalf("a log line outside the counts-only shape: %q", line)
		}
		for _, absent := range []string{
			docA, docB, docNeverDeclared, digestOf(content[start:end]),
			fmt.Sprintf("=%d ", start), fmt.Sprintf("=%d ", end),
		} {
			if strings.Contains(line, absent) {
				t.Fatalf("a log line carries %q: %q", absent, line)
			}
		}
		for _, r := range withheldMaterial {
			if r >= utf8.RuneSelf && strings.ContainsRune(line, r) {
				t.Fatalf("a log line carries the material: %q", line)
			}
		}
	}
}
