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

package promtext

import "testing"

func TestEscapeLabelValueFollowsTheExpositionFormat(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"plain", "plain"},
		{`back\slash`, `back\\slash`},
		{`a "quoted" word`, `a \"quoted\" word`},
		{"two\nlines", `two\nlines`},
		// Characters %q would escape are literal in the format.
		{"tab\there", "tab\there"},
		{"café", "café"},
		{"bad\xffbyte", "bad�byte"},
		// Escaping is a single pass: an escaped quote is not escaped again.
		{`\"`, `\\\"`},
	} {
		if got := EscapeLabelValue(tc.in); got != tc.want {
			t.Errorf("EscapeLabelValue(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestEscapeHelpLeavesQuotesLiteral(t *testing.T) {
	if got, want := EscapeHelp("say \"hi\"\\\nnow"), `say "hi"\\\nnow`; got != want {
		t.Fatalf("EscapeHelp = %q, want %q", got, want)
	}
}

func TestLabelQuotesTheEscapedValue(t *testing.T) {
	if got, want := Label("name", `x"y`), `name="x\"y"`; got != want {
		t.Fatalf("Label = %q, want %q", got, want)
	}
}
