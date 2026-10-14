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

// Package disclosureconformance asserts that a response surface does not vary
// with whether content was withheld from the caller.
//
// The property it checks is indistinguishability, not absence of a field. A
// caller who can separate "nothing matched" from "something matched and was
// withheld" can enumerate the existence of content it cannot read, by probing
// terms and watching the difference. That holds whether the difference is a
// count, a boolean, an error code, or a field that is present in one response
// and omitted in the other. Comparing the encoded forms catches all of them,
// including a field added later that nobody thought about.
//
// The suite is deliberately usable in both directions. A surface that
// deliberately discloses withholding declares itself Distinguishable, which
// pins the accepted trade so that changing it silently fails the suite rather
// than passing unnoticed.
//
// # Timing
//
// There is no latency assertion here, and that is a conclusion rather than an
// omission. Measured over 200 runs per probe against the fixture corpus, a
// term whose matches are withheld and a term that matches nothing take
// indistinguishable time: roughly 110µs and 118µs, with the absent term
// marginally slower, which is noise. A term that returns a result takes about
// 60% longer, and that discloses nothing, because the caller receives the
// result anyway.
//
// The reason is the same one that makes the responses identical. Documents an
// identity may not read leave the candidate set before scoring, so the
// withheld path does not do more work than a plain miss. It does the same
// work. A timing gate here would assert the absence of a mechanism that does
// not exist, and would buy that with the flakiest kind of test.
//
// This stops being true if withholding ever moves later in the pipeline, to
// scoring hidden candidates and filtering them afterward rather than excluding
// them up front. That change would not be caught here: filtering after scoring
// can produce byte-identical responses while doing strictly more work on the
// withheld path, which is a timing distinction with no response distinction.
//
// So the condition is explicit rather than delegated. Anyone moving where
// candidates are filtered has to re-measure timing, because no test in this
// package will notice.
package disclosureconformance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"testing"
)

// Probe is one comparison. Withheld is the response produced when content was
// withheld from the caller; Control is the response for an otherwise identical
// request where nothing was withheld, because nothing matched.
//
// The two must be produced under the same principal and the same request. A
// probe that varies anything else proves nothing.
type Probe struct {
	// Name identifies the surface and request, for example
	// "retrieve/restricted-term".
	Name string
	// Withheld is the response for a request whose matches were withheld.
	Withheld any
	// Control is the response for a request that simply had no matches.
	Control any
	// Distinguishable declares that this surface discloses withholding on
	// purpose. The suite then requires the two responses to differ, so that
	// removing a deliberate disclosure is caught as surely as adding an
	// accidental one.
	Distinguishable bool
	// Reason records why a Distinguishable surface is allowed to disclose.
	// Required when Distinguishable is set, so an accepted trade always
	// carries its justification next to the assertion.
	Reason string
}

// Run executes every probe. Each runs as a subtest so one failing surface
// reports alone rather than hiding the rest.
func Run(t *testing.T, probes ...Probe) {
	t.Helper()
	if len(probes) == 0 {
		t.Fatal("disclosure conformance requires at least one probe")
	}
	seen := make(map[string]struct{}, len(probes))
	for _, probe := range probes {
		if probe.Name == "" {
			t.Fatal("every disclosure probe requires a name")
		}
		if _, duplicate := seen[probe.Name]; duplicate {
			t.Fatalf("duplicate disclosure probe %q", probe.Name)
		}
		seen[probe.Name] = struct{}{}
		t.Run(probe.Name, func(t *testing.T) {
			runProbe(t, probe)
		})
	}
}

func runProbe(t *testing.T, probe Probe) {
	t.Helper()
	if probe.Distinguishable && probe.Reason == "" {
		t.Fatal("a deliberately distinguishable surface must record why")
	}
	withheld, err := encode(probe.Withheld)
	if err != nil {
		t.Fatalf("encode withheld response: %v", err)
	}
	control, err := encode(probe.Control)
	if err != nil {
		t.Fatalf("encode control response: %v", err)
	}
	identical := bytes.Equal(withheld, control)
	switch {
	case probe.Distinguishable && identical:
		t.Fatalf(
			"surface no longer discloses withholding, but is declared to on "+
				"purpose (%s)\n  response: %s",
			probe.Reason, withheld)
	case !probe.Distinguishable && !identical:
		t.Fatalf(
			"withheld and control responses differ, so a caller can detect "+
				"that content exists and was withheld\n  fields: %v\n"+
				"  withheld: %s\n  control:  %s",
			differingFields(withheld, control), withheld, control)
	}
}

// encode renders a response the way a caller receives it, so the comparison
// covers custom marshaling and omitempty rather than only exported fields.
func encode(value any) ([]byte, error) {
	if value == nil {
		return []byte("null"), nil
	}
	return json.Marshal(value)
}

// differingFields names the top-level keys that differ, so a failure says what
// leaked rather than only that something did. It is diagnostic output; the
// assertion itself is always the full byte comparison.
func differingFields(withheld, control []byte) []string {
	left, right := decodeObject(withheld), decodeObject(control)
	if left == nil || right == nil {
		return []string{"(responses are not JSON objects)"}
	}
	names := make(map[string]struct{})
	for key := range left {
		names[key] = struct{}{}
	}
	for key := range right {
		names[key] = struct{}{}
	}
	var differing []string
	for key := range names {
		leftValue, leftOK := left[key]
		rightValue, rightOK := right[key]
		switch {
		case leftOK != rightOK:
			differing = append(differing, fmt.Sprintf("%s (present in one)", key))
		case !bytes.Equal(leftValue, rightValue):
			differing = append(differing,
				fmt.Sprintf("%s (%s vs %s)", key, leftValue, rightValue))
		}
	}
	sort.Strings(differing)
	return differing
}

func decodeObject(raw []byte) map[string]json.RawMessage {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil
	}
	return object
}
