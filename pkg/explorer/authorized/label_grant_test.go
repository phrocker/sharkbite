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

package authorized

import (
	"bytes"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/interaction"
)

// TestLabelGrantSeparatorsAreNotLabelCharacters: the textual grant form is
// unambiguous only because no label can contain either separator.
func TestLabelGrantSeparatorsAreNotLabelCharacters(t *testing.T) {
	for _, separator := range []string{LabelGrantSeparator, LabelGrantListSeparator} {
		if err := interaction.ValidateLabel("a" + separator + "b"); err == nil {
			t.Fatalf("label charset admits the separator %q", separator)
		}
	}
	// ':' is a label character, which is why it is not the separator.
	if err := interaction.ValidateLabel("a:b"); err != nil {
		t.Fatalf("':' is no longer a label character: %v", err)
	}
}

func TestParseLabelGrant(t *testing.T) {
	sources := [][]byte{[]byte("ws/one"), []byte("a=b")}
	want := func(source, label string) []byte {
		id, err := LabelPolicyID([]byte(source), label)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	for _, accepted := range []struct{ text, source, label string }{
		{"ws/one=secret", "ws/one", "secret"},
		{"ws/one=a:b", "ws/one", "a:b"},
		// The source may contain the separator; the label is after the last.
		{"a=b=secret", "a=b", "secret"},
	} {
		id, err := ParseLabelGrant(accepted.text, sources)
		if err != nil {
			t.Fatalf("ParseLabelGrant(%q): %v", accepted.text, err)
		}
		if !bytes.Equal(id, want(accepted.source, accepted.label)) {
			t.Fatalf("ParseLabelGrant(%q) = %q", accepted.text, id)
		}
	}
	for _, refused := range []struct{ text, reason string }{
		{"ws/one", "form"},
		{"ws/one=", "label"},
		{"ws/one=Secret!", "unsupported character"},
		{"ws/two=secret", "not configured"},
		{"WS/ONE=secret", "not configured"},
		{" ws/one=secret", "not configured"},
		{"=secret", "not configured"},
		{"ws/one=" + strings.Repeat("x", 257), "label"},
	} {
		if _, err := ParseLabelGrant(refused.text, sources); err == nil ||
			!strings.Contains(err.Error(), refused.reason) {
			t.Fatalf("ParseLabelGrant(%q) = %v, want %q", refused.text, err, refused.reason)
		}
	}
}

func TestParseLabelGrantList(t *testing.T) {
	sources := [][]byte{[]byte("ws")}
	ids, err := ParseLabelGrantList("ws=secret,ws=pii", sources)
	if err != nil || len(ids) != 2 {
		t.Fatalf("ParseLabelGrantList = %q, %v", ids, err)
	}
	if ids, err := ParseLabelGrantList("", sources); err != nil || ids != nil {
		t.Fatalf("empty list = %q, %v", ids, err)
	}
	for _, refused := range []string{
		"ws=secret,", ",ws=secret", "ws=secret,,ws=pii", "ws=secret,ws=secret",
		"ws=secret, ws=pii",
	} {
		if _, err := ParseLabelGrantList(refused, sources); err == nil {
			t.Fatalf("ParseLabelGrantList(%q) was accepted", refused)
		}
	}
	many := make([]string, MaxLabelGrants+1)
	for i := range many {
		many[i] = "ws=l" + strings.Repeat("x", i%7) + string(rune('a'+i%26))
	}
	if _, err := ParseLabelGrantList(strings.Join(many, ","), sources); err == nil ||
		!strings.Contains(err.Error(), "bound") {
		t.Fatalf("an oversized list = %v", err)
	}
}

// TestAdmitLabelGrantRefusesReservedAndNonCanonicalIDs: even a constructor
// that returned a reserved or a foreign ID could not get it granted.
func TestAdmitLabelGrantRefusesReservedAndNonCanonicalIDs(t *testing.T) {
	sources := [][]byte{[]byte("ws")}
	reserved := func([]byte, string) ([]byte, error) {
		return []byte(UntranslatableLabelPolicyID), nil
	}
	if _, err := admitLabelGrant([]byte("ws"), "secret", sources, reserved); err == nil ||
		!strings.Contains(err.Error(), "reserved") {
		t.Fatalf("a reserved ID = %v, want the reserved refusal", err)
	}
	other := func([]byte, string) ([]byte, error) {
		return LabelPolicyID([]byte("ws"), "other")
	}
	if _, err := admitLabelGrant([]byte("ws"), "secret", sources, other); err == nil ||
		!strings.Contains(err.Error(), "canonical") {
		t.Fatalf("an ID for another label = %v, want the canonical refusal", err)
	}
	if _, err := AdmitLabelPolicyID(
		[]byte("shoal.label/v1/02/ws/secret"), []byte("ws"), "secret"); err == nil {
		t.Fatal("a non-canonical ID was admitted")
	}
}
