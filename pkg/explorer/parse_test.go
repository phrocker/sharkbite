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

package explorer

import (
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestParseSourceEnforcesBoundsIncrementally(t *testing.T) {
	tests := map[string]struct {
		source Source
		limits parserLimits
	}{
		"source bytes": {
			source: Source{
				URI: "file:///bytes.txt", MediaType: MediaTypeText,
				Content: strings.Repeat("x", 17),
			},
			limits: parserLimits{
				maxSourceBytes: 16, maxSourceLines: 100,
				maxSections: 100, maxSpans: 100,
			},
		},
		"line fragments": {
			source: Source{
				URI: "file:///lines.txt", MediaType: MediaTypeText,
				Content: "one\ntwo\nthree\nfour",
			},
			limits: parserLimits{
				maxSourceBytes: 100, maxSourceLines: 3,
				maxSections: 100, maxSpans: 100,
			},
		},
		"sections": {
			source: Source{
				URI: "file:///sections.md", MediaType: MediaTypeMarkdown,
				Content: "# one\n# two\n# three\n",
			},
			limits: parserLimits{
				maxSourceBytes: 100, maxSourceLines: 100,
				maxSections: 3, maxSpans: 100,
			},
		},
		"spans": {
			source: Source{
				URI: "file:///spans.txt", MediaType: MediaTypeText,
				Content: "one\n\ntwo\n\nthree",
			},
			limits: parserLimits{
				maxSourceBytes: 100, maxSourceLines: 100,
				maxSections: 100, maxSpans: 2,
			},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			parsed, err := parseSourceWithLimits(test.source, time.Time{}, test.limits)
			if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
				t.Fatalf("error = %v", err)
			}
			if parsed.sections != nil || parsed.spans != nil || parsed.nodes != nil {
				t.Fatalf("partially materialized result = %+v", parsed)
			}
		})
	}
}

func TestSplitSourceLinesStopsAtInjectedBound(t *testing.T) {
	lines, err := splitSourceLines(strings.Repeat("x\n", 10_000), 4)
	if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("error = %v", err)
	}
	if lines != nil {
		t.Fatalf("partial line index escaped: %d entries", len(lines))
	}
}
