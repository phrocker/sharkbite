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

// Package promtext holds the few pieces of the Prometheus text exposition
// format (version 0.0.4) that every hand-rolled /metrics surface in Shoal
// needs to agree on: the content type, and how label values and HELP text are
// escaped.
//
// It is deliberately not a metrics library. Producers keep writing their own
// samples into a strings.Builder; this package only makes sure the bytes they
// write are ones a scraper parses.
package promtext

import "strings"

// ContentType is the media type of the text exposition format.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// labelValueEscaper implements the label-value escaping the format defines:
// backslash, double quote and line feed, and nothing else.
var labelValueEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

// helpEscaper implements HELP-text escaping: backslash and line feed only. A
// double quote is literal in HELP.
var helpEscaper = strings.NewReplacer(`\`, `\\`, "\n", `\n`)

// EscapeLabelValue returns value escaped for use between the double quotes of
// a label pair.
//
// Go's %q is not a substitute. It also turns tabs, control characters and
// non-printable runes into \t, \x.. and \u.... sequences that the exposition
// format does not define, so a scraper either rejects the line or records a
// different value than the producer meant. Invalid UTF-8 is replaced with
// U+FFFD, because the format is UTF-8 and one malformed value would otherwise
// fail the whole scrape.
func EscapeLabelValue(value string) string {
	return labelValueEscaper.Replace(strings.ToValidUTF8(value, "�"))
}

// EscapeHelp returns help escaped for the text of a "# HELP" line.
func EscapeHelp(help string) string {
	return helpEscaper.Replace(strings.ToValidUTF8(help, "�"))
}

// Label renders one name="value" pair with the value escaped. The name is
// written as given: label names are fixed by the producer, not by data.
func Label(name, value string) string {
	return name + `="` + EscapeLabelValue(value) + `"`
}
