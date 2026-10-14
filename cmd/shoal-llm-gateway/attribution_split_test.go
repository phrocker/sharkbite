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
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	admissionapi "github.com/phrocker/shoal-oss/pkg/admission/api"
)

// splitOnRunes cuts text into adjacent pieces of at most limit bytes, each on
// rune boundaries, and returns their byte ranges.
func splitOnRunes(text string, limit int) [][2]int {
	var ranges [][2]int
	start := 0
	for start < len(text) {
		end := start
		for end < len(text) {
			_, size := utf8.DecodeRuneInString(text[end:])
			if end+size-start > limit {
				break
			}
			end += size
		}
		ranges = append(ranges, [2]int{start, end})
		start = end
	}
	return ranges
}

// TestSplittingTheAttributionDoesNotDisableTheResidueCheck: the residue check
// covers the union of withheld text, so material attributed as adjacent
// segments too short to sample on their own is still found when it is copied
// elsewhere — whether the pieces name one withheld reference or alternate
// between two. Pieces separated by an unattributed byte are not joined, and the
// last case pins that boundary rather than claiming more.
func TestSplittingTheAttributionDoesNotDisableTheResidueCheck(t *testing.T) {
	content, start, _ := toolContent()
	pieces := splitOnRunes(withheldMaterial, 60)
	if len(pieces) < 4 {
		t.Fatalf("the material split into only %d pieces", len(pieces))
	}
	split := func(references ...string) []attributionSpec {
		specs := make([]attributionSpec, 0, len(pieces))
		for i, piece := range pieces {
			specs = append(specs, attributeSpan(references[i%len(references)], 3, content,
				start+piece[0], start+piece[1]))
		}
		return specs
	}
	pasted := func(attribution []attributionSpec) func(map[string]any) {
		return func(body map[string]any) {
			body["messages"].([]any)[1] = map[string]any{
				"role": "user", "content": "Quoting: " + withheldMaterial}
			body["shoal_attribution"] = attribution
		}
	}
	for _, probe := range []struct {
		name     string
		withhold []string
		specs    []attributionSpec
	}{
		{"adjacent pieces under one reference", []string{docB}, split(docB)},
		{"adjacent pieces alternating between two withheld references",
			[]string{docA, docB}, split(docA, docB)},
	} {
		t.Run(probe.name, func(t *testing.T) {
			plane := newFakePlane(t, admissionapi.OutcomeObligated, probe.withhold)
			upstream := newFakeUpstream(t)
			governed, logged := newTestProxy(t, plane, upstream)
			recorder := post(t, governed, acceptanceBody(t, pasted(probe.specs)))
			if recorder.Code != http.StatusForbidden || upstream.calls != 0 {
				t.Fatalf("status = %d, upstream calls = %d: a split attribution let a copy through",
					recorder.Code, upstream.calls)
			}
			if joined := strings.Join(*logged, "\n"); !strings.Contains(joined, "class="+refusalResidue) {
				t.Fatalf("logged %q, want class %s", joined, refusalResidue)
			}
			assertLogsCountsOnly(t, *logged)
		})
	}

	t.Run("an unattributed byte between pieces is a boundary", func(t *testing.T) {
		// Two 60-byte withheld pieces with one caller byte between them. No
		// 96-byte run of withheld text exists, so a copy of all 121 bytes is
		// not a residue hit. This documents the limit; it is not a goal.
		text := strings.Repeat("abcdefghij", 6) + "|" + strings.Repeat("klmnopqrst", 6)
		body := encodeBody(t, map[string]any{
			"model": "gpt",
			"messages": []any{
				map[string]any{"role": "user", "content": "Quoting: " + text},
				map[string]any{"role": "tool", "content": text},
			},
			"shoal_references": []string{docB},
			"shoal_attribution": []attributionSpec{
				attributeSpan(docB, 1, text, 0, 60),
				attributeSpan(docB, 1, text, 61, 121),
			},
		})
		plane := newFakePlane(t, admissionapi.OutcomeObligated, []string{docB})
		upstream := newFakeUpstream(t)
		governed, _ := newTestProxy(t, plane, upstream)
		if recorder := post(t, governed, body); recorder.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
		}
		if got, want := stringField(t, messageAt(t, upstream.received, 1)["content"]),
			withheldPlaceholder+"|"+withheldPlaceholder; got != want {
			t.Fatalf("tool content = %q, want %q", got, want)
		}
	})
}
