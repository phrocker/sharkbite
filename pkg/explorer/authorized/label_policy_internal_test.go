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
	"context"
	"math/rand"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/accumulo"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const labelCharset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-.:"

func labelTestPolicy(t *testing.T, domain, source, grant string, epoch int64) auth.Policy {
	t.Helper()
	policy, err := auth.NewPolicy(auth.PolicyConfig{
		AuthorizationDomain: []byte(domain),
		SourceID:            []byte(source),
		GrantPolicyID:       []byte(grant),
		Epoch:               epoch,
	})
	if err != nil {
		t.Fatalf("NewPolicy(%q, %q, %q, %d) = %v", domain, source, grant, epoch, err)
	}
	return policy
}

func mustLabelPolicyID(t *testing.T, source, label string) []byte {
	t.Helper()
	id, err := LabelPolicyID([]byte(source), label)
	if err != nil {
		t.Fatalf("LabelPolicyID(%q, %q) = %v", source, label, err)
	}
	return id
}

// TestLabelPolicyIDGoldenEncoding pins the wire form so a change to the
// scheme is a visible, deliberate break rather than a silent one.
func TestLabelPolicyIDGoldenEncoding(t *testing.T) {
	for _, tc := range []struct{ source, label, want string }{
		{"ab", "c", "shoal.label/v1/2/ab/c"},
		{"12", "x", "shoal.label/v1/2/12/x"},
		{"a/b", "secret", "shoal.label/v1/3/a/b/secret"},
		{strings.Repeat("s", 10), "x", "shoal.label/v1/10/" + strings.Repeat("s", 10) + "/x"},
	} {
		if got := string(mustLabelPolicyID(t, tc.source, tc.label)); got != tc.want {
			t.Fatalf("LabelPolicyID(%q, %q) = %q, want %q", tc.source, tc.label, got, tc.want)
		}
	}
}

// TestLabelPolicyIDProbeTable lists pairs that a careless scheme (case
// folding, separator normalization, unprefixed concatenation) would merge.
// Every pair must produce a different ID, and every ID must round-trip.
func TestLabelPolicyIDProbeTable(t *testing.T) {
	probes := []struct{ source, label string }{
		{"src", "secret"}, {"src", "Secret"}, {"src", "SECRET"},
		{"src", "a.b"}, {"src", "a-b"}, {"src", "a_b"}, {"src", "ab"},
		{"src", "x:y"}, {"src", "x"}, {"src", "y"}, {"src", "x:"},
		{"ab", "c"}, {"a", "bc"}, {"abc", "d"}, {"a", "b"},
		{"1", "2"}, {"12", "x"}, {"1", "2x"}, {"2", "x"}, {"21", "x"},
		{"2/ab", "c"}, {"ab", "c2"}, {"2/12", "x"}, {"12/x", "y"},
		{"1/", "x"}, {"/1", "x"}, {"/", "x"}, {"//", "x"}, {"1", "x"},
		{"11", "x"}, {"a/b", "c"}, {"a", "b.c"}, {"9", "x"}, {"10", "x"},
		{"1/2", "3"}, {"1", "23"}, {"12", "3"},
		{"!untranslatable", "x"}, {"!", "untranslatable"},
		{"src\x00", "x"}, {"src\xff", "x"},
	}
	seen := make(map[string]int, len(probes))
	for index, probe := range probes {
		id := mustLabelPolicyID(t, probe.source, probe.label)
		if previous, duplicate := seen[string(id)]; duplicate {
			t.Fatalf("probes %v and %v share ID %q", probes[previous], probe, id)
		}
		seen[string(id)] = index
		source, label, err := ParseLabelPolicyID(id)
		if err != nil || string(source) != probe.source || label != probe.label {
			t.Fatalf("ParseLabelPolicyID(%q) = (%q, %q, %v), want (%q, %q)",
				id, source, label, err, probe.source, probe.label)
		}
		if IsReservedLabelPolicyID(id) {
			t.Fatalf("probe %v produced reserved ID %q", probe, id)
		}
	}
}

// TestLabelPolicyIDLengthLimit checks the ID bound at the exact limit and one
// past it, for one- and two-digit source lengths.
func TestLabelPolicyIDLengthLimit(t *testing.T) {
	for _, source := range []string{"s", strings.Repeat("s", 10), "1/2/3/4/5/"} {
		overhead := len(auth.LabelPolicyIDPrefix) + len(strconv.Itoa(len(source))) + 1 +
			len(source) + 1
		limit := auth.MaxPolicyComponentBytes - overhead
		atLimit := strings.Repeat("L", limit)
		id, err := LabelPolicyID([]byte(source), atLimit)
		if err != nil || len(id) != auth.MaxPolicyComponentBytes {
			t.Fatalf("label at limit (%d) on %q: len=%d err=%v", limit, source, len(id), err)
		}
		if _, _, err := ParseLabelPolicyID(id); err != nil {
			t.Fatalf("ParseLabelPolicyID(at limit) = %v", err)
		}
		if _, err := LabelPolicyID([]byte(source), atLimit+"L"); !shoal.IsErrorCode(
			err, shoal.ErrorInvalidArgument) {
			t.Fatalf("label one past the limit on %q = %v, want refusal", source, err)
		}
		if _, _, err := ParseLabelPolicyID(append(id, 'L')); err == nil {
			t.Fatalf("ParseLabelPolicyID accepted an over-long ID")
		}
	}
	// A source that alone fills the component bound leaves no room for a label.
	if _, err := LabelPolicyID(
		bytes.Repeat([]byte{'s'}, auth.MaxPolicyComponentBytes), "x",
	); err == nil {
		t.Fatal("LabelPolicyID accepted a source at the component bound")
	}
}

// TestLabelPolicyIDRefusals: empty source, and labels that are not already
// exactly valid. Nothing is folded, trimmed or sanitized into validity.
func TestLabelPolicyIDRefusals(t *testing.T) {
	if _, err := LabelPolicyID(nil, "secret"); err == nil {
		t.Fatal("nil source accepted")
	}
	if _, err := LabelPolicyID([]byte{}, "secret"); err == nil {
		t.Fatal("empty source accepted")
	}
	for _, label := range []string{
		"", " secret", "secret ", "se cret", "a/b", "/", "!", "!untranslatable",
		"a!b", "a&b", "a|b", "(a)", "\"a\"", "a\nb", "é", "a\x00",
		strings.Repeat("a", interaction.MaxVisibilityLabelSz+1),
	} {
		if _, err := LabelPolicyID([]byte("src"), label); err == nil {
			t.Fatalf("LabelPolicyID accepted invalid label %q", label)
		}
	}
}

// TestLabelCharsetExcludesSeparators pins the property the reserved ID and
// the trailing-label position rely on: '/' and '!' are not label characters,
// and the charset is exactly [A-Za-z0-9_.:-].
func TestLabelCharsetExcludesSeparators(t *testing.T) {
	for b := 0; b < 256; b++ {
		character := byte(b)
		want := strings.IndexByte(labelCharset, character) >= 0
		got := interaction.ValidateLabel(string([]byte{character})) == nil
		if got != want {
			t.Fatalf("ValidateLabel(%q) accepted=%v, want %v", character, got, want)
		}
	}
	for _, character := range []string{"/", "!"} {
		if interaction.ValidateLabel("a"+character+"b") == nil {
			t.Fatalf("label charset admits %q", character)
		}
	}
}

// TestParseLabelPolicyIDRefusesNonCanonical makes the inverse strict: only an
// ID LabelPolicyID would emit is accepted.
func TestParseLabelPolicyIDRefusesNonCanonical(t *testing.T) {
	for _, id := range []string{
		"", "shoal.label/v1/", "shoal.label/v1/2", "shoal.label/v1/2/",
		"shoal.label/v1/2/ab", "shoal.label/v1/2/ab/", "shoal.label/v1/02/ab/c",
		"shoal.label/v1/+2/ab/c", "shoal.label/v1/-2/ab/c", "shoal.label/v1/ 2/ab/c",
		"shoal.label/v1/0//c", "shoal.label/v1/3/ab/c", "shoal.label/v1/1/ab/c",
		"shoal.label/v1/2/ab/c/d", "shoal.label/v1/2/ab/C!", "shoal.label/v1/2/ab/ c",
		"shoal.label/v2/2/ab/c", "Shoal.label/v1/2/ab/c", "shoal.label/v1//ab/c",
		"shoal.label/v1/99999999999999999999/ab/c",
		UntranslatableLabelPolicyID, "shoal.label/v1/!", "shoal.label/v1/!2/ab/c",
	} {
		if source, label, err := ParseLabelPolicyID([]byte(id)); err == nil {
			t.Fatalf("ParseLabelPolicyID(%q) = (%q, %q), want refusal", id, source, label)
		}
	}
}

// TestLabelPolicyIDPropertyDistinctAndRoundTrip checks injectivity and the
// round trip two ways: exhaustively over a tiny alphabet chosen to provoke
// boundary shifts (digits and '/' in sources, ':' and digits in labels), and
// over a seeded random sample of arbitrary source bytes and the full label
// charset.
func TestLabelPolicyIDPropertyDistinctAndRoundTrip(t *testing.T) {
	type pair struct {
		source string
		label  string
	}
	ids := make(map[string]pair)
	check := func(source, label string) {
		t.Helper()
		id, err := LabelPolicyID([]byte(source), label)
		if err != nil {
			if len(auth.LabelPolicyIDPrefix)+len(strconv.Itoa(len(source)))+
				len(source)+len(label)+2 <= auth.MaxPolicyComponentBytes {
				t.Fatalf("LabelPolicyID(%q, %q) refused within bounds: %v", source, label, err)
			}
			return
		}
		if previous, seen := ids[string(id)]; seen && previous != (pair{source, label}) {
			t.Fatalf("collision: %v and %v both give %q", previous, pair{source, label}, id)
		}
		ids[string(id)] = pair{source, label}
		gotSource, gotLabel, err := ParseLabelPolicyID(id)
		if err != nil || string(gotSource) != source || gotLabel != label {
			t.Fatalf("round trip of %q = (%q, %q, %v), want (%q, %q)",
				id, gotSource, gotLabel, err, source, label)
		}
		if IsReservedLabelPolicyID(id) || string(id) == UntranslatableLabelPolicyID {
			t.Fatalf("(%q, %q) produced reserved ID %q", source, label, id)
		}
	}

	// words returns every non-empty string over alphabet up to maxLen.
	words := func(alphabet string, maxLen int) []string {
		var out []string
		frontier := []string{""}
		for length := 1; length <= maxLen; length++ {
			next := make([]string, 0, len(frontier)*len(alphabet))
			for _, prefix := range frontier {
				for index := 0; index < len(alphabet); index++ {
					next = append(next, prefix+alphabet[index:index+1])
				}
			}
			out = append(out, next...)
			frontier = next
		}
		return out
	}
	sources := words("1/a!", 4)
	labels := words("1a:", 3)
	for _, source := range sources {
		for _, label := range labels {
			check(source, label)
		}
	}

	random := rand.New(rand.NewSource(570))
	for iteration := 0; iteration < 50000; iteration++ {
		source := make([]byte, 1+random.Intn(60))
		for index := range source {
			switch random.Intn(3) {
			case 0:
				source[index] = byte(random.Intn(256))
			case 1:
				source[index] = "0123456789"[random.Intn(10)]
			default:
				source[index] = "/!a"[random.Intn(3)]
			}
		}
		label := make([]byte, 1+random.Intn(70))
		for index := range label {
			label[index] = labelCharset[random.Intn(len(labelCharset))]
		}
		check(string(source), string(label))
	}
	if len(ids) < 50000 {
		t.Fatalf("property test produced only %d distinct IDs", len(ids))
	}
}

// TestReservedLabelPolicyIDIsUnreachable: the reserved ID sits where every
// canonical ID has a decimal length, and '!' is not a label character, so no
// (source, label) can produce it, including sources built to imitate it.
func TestReservedLabelPolicyIDIsUnreachable(t *testing.T) {
	if UntranslatableLabelPolicyID != "shoal.label/v1/!untranslatable" {
		t.Fatalf("reserved ID = %q", UntranslatableLabelPolicyID)
	}
	if !IsReservedLabelPolicyID([]byte(UntranslatableLabelPolicyID)) {
		t.Fatal("reserved ID is not reported reserved")
	}
	if _, _, err := ParseLabelPolicyID([]byte(UntranslatableLabelPolicyID)); err == nil {
		t.Fatal("reserved ID parses as a label policy ID")
	}
	for _, source := range []string{
		"!untranslatable", "!", "!untranslatable/x", "untranslatable", "1", "/",
	} {
		for _, label := range []string{"untranslatable", "x", "1", "untranslatable:x"} {
			id := mustLabelPolicyID(t, source, label)
			if IsReservedLabelPolicyID(id) || string(id) == UntranslatableLabelPolicyID {
				t.Fatalf("(%q, %q) produced reserved ID %q", source, label, id)
			}
		}
	}
	if _, err := LabelPolicyID([]byte("src"), "!untranslatable"); err == nil {
		t.Fatal("label !untranslatable accepted")
	}
	for _, id := range []string{"shoal.label/v1/!other", "shoal.label/v1/!"} {
		if !IsReservedLabelPolicyID([]byte(id)) {
			t.Fatalf("IsReservedLabelPolicyID(%q) = false", id)
		}
	}
	for _, id := range []string{"shoal.label/v1/3/!ab/c", "policy", "!untranslatable"} {
		if IsReservedLabelPolicyID([]byte(id)) {
			t.Fatalf("IsReservedLabelPolicyID(%q) = true", id)
		}
	}
}

func TestNewLabelPolicyCopiesSourceFields(t *testing.T) {
	source := labelTestPolicy(t, "domain", "source/1", "policy", 7)
	policy, err := newLabelPolicy(source, "secret")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(policy.AuthorizationDomain(), []byte("domain")) ||
		!bytes.Equal(policy.SourceID(), []byte("source/1")) ||
		!bytes.Equal(policy.GrantPolicyID(), mustLabelPolicyID(t, "source/1", "secret")) ||
		policy.Epoch() != 7 || policy.ServiceRole() != "" {
		t.Fatalf("newLabelPolicy fields = %q %q %q %d %q",
			policy.AuthorizationDomain(), policy.SourceID(), policy.GrantPolicyID(),
			policy.Epoch(), policy.ServiceRole())
	}
	untranslatable, err := UntranslatablePolicy(source)
	if err != nil {
		t.Fatal(err)
	}
	if string(untranslatable.GrantPolicyID()) != UntranslatableLabelPolicyID ||
		!bytes.Equal(untranslatable.SourceID(), []byte("source/1")) ||
		untranslatable.Epoch() != 7 || untranslatable.ServiceRole() != "" {
		t.Fatalf("UntranslatablePolicy = %v", untranslatable)
	}
	if _, err := newLabelPolicy(source, "a/b"); err == nil {
		t.Fatal("newLabelPolicy accepted an invalid label")
	}
	if _, err := newLabelPolicy(auth.Policy{}, "secret"); err == nil {
		t.Fatal("newLabelPolicy accepted a zero source policy")
	}
	if _, err := UntranslatablePolicy(auth.Policy{}); err == nil {
		t.Fatal("UntranslatablePolicy accepted a zero source policy")
	}
	// A label policy cannot anchor further label policies.
	if _, err := newLabelPolicy(policy, "other"); err == nil {
		t.Fatal("newLabelPolicy accepted a label policy as its source")
	}
	if _, err := UntranslatablePolicy(untranslatable); err == nil {
		t.Fatal("UntranslatablePolicy accepted the reserved policy as its source")
	}
}

func servicePolicyForTest(t *testing.T, base auth.Policy, role auth.ServiceRole) auth.Policy {
	t.Helper()
	encoded, err := base.Encode()
	if err != nil {
		t.Fatal(err)
	}
	visibility, err := accumulo.NewColumnVisibility(
		append(encoded, []byte("&svc:"+string(role))...))
	if err != nil {
		t.Fatal(err)
	}
	policy, err := auth.DecodePolicy(visibility.Flatten())
	if err != nil {
		t.Fatalf("DecodePolicy(service) = %v", err)
	}
	if policy.ServiceRole() != role {
		t.Fatalf("service role = %q", policy.ServiceRole())
	}
	return policy
}

// TestCheckLabelPolicyRefusesEachFieldMismatch drives the post-construction
// assertion directly with a policy that differs from the expected one in
// exactly one field. Each case isolates one check: removing that check lets
// its case through.
func TestCheckLabelPolicyRefusesEachFieldMismatch(t *testing.T) {
	source := labelTestPolicy(t, "domain", "A", "policy", 3)
	idA := mustLabelPolicyID(t, "A", "secret")
	idB := mustLabelPolicyID(t, "B", "secret")
	good := labelTestPolicy(t, "domain", "A", string(idA), 3)
	if err := checkLabelPolicy(source, idA, good); err != nil {
		t.Fatalf("checkLabelPolicy(good) = %v", err)
	}
	reserved := labelTestPolicy(t, "domain", "A", UntranslatableLabelPolicyID, 3)
	if err := checkLabelPolicy(source, []byte(UntranslatableLabelPolicyID), reserved); err != nil {
		t.Fatalf("checkLabelPolicy(reserved) = %v", err)
	}

	for _, tc := range []struct {
		name   string
		grant  []byte
		policy auth.Policy
	}{
		{"zero policy", idA, auth.Policy{}},
		{"domain", idA, labelTestPolicy(t, "other-domain", "A", string(idA), 3)},
		// Source differs while the ID agrees with the built policy's source,
		// so only the SourceID equality catches it.
		{"source", idB, labelTestPolicy(t, "domain", "B", string(idB), 3)},
		// Source agrees while the ID names another source, so only the
		// encoded-source check catches it.
		{"encoded source", idB, labelTestPolicy(t, "domain", "A", string(idB), 3)},
		{"grant", idA, labelTestPolicy(t, "domain", "A",
			string(mustLabelPolicyID(t, "A", "Secret")), 3)},
		{"grant defaulted to source grant", idA, labelTestPolicy(t, "domain", "A", "policy", 3)},
		{"epoch", idA, labelTestPolicy(t, "domain", "A", string(idA), 4)},
		{"service role", idA, servicePolicyForTest(t, good, auth.ServiceRoleCoordination)},
		{"other reserved ID", []byte("shoal.label/v1/!other"),
			labelTestPolicy(t, "domain", "A", "shoal.label/v1/!other", 3)},
		{"non-canonical label ID", []byte("shoal.label/v1/01/A/secret"),
			labelTestPolicy(t, "domain", "A", "shoal.label/v1/01/A/secret", 3)},
	} {
		if err := checkLabelPolicy(source, tc.grant, tc.policy); err == nil {
			t.Fatalf("checkLabelPolicy accepted a %s mismatch", tc.name)
		}
	}
}

func TestLabelRuleEmptyEqualsSourceRule(t *testing.T) {
	source := labelTestPolicy(t, "domain", "source", "policy", 1)
	plain, err := NewAccessRule(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, labels := range [][]string{nil, {}} {
		rule, err := LabelRule(source, labels)
		if err != nil {
			t.Fatal(err)
		}
		if !rule.equal(plain) || rule.String() != plain.String() {
			t.Fatalf("LabelRule(%v) = %v, want %v", labels, rule, plain)
		}
	}
}

func TestLabelRuleDeduplicatesAndCaps(t *testing.T) {
	source := labelTestPolicy(t, "domain", "source", "policy", 1)
	rule, err := LabelRule(source, []string{"secret", "Secret", "secret", "a.b", "a-b"})
	if err != nil {
		t.Fatal(err)
	}
	reordered, err := LabelRule(source, []string{"a-b", "Secret", "a.b", "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if !rule.equal(reordered) {
		t.Fatal("LabelRule depends on label order or duplicates")
	}
	got := map[string]bool{}
	for _, policy := range rule.components() {
		if !bytes.Equal(policy.SourceID(), []byte("source")) || policy.Epoch() != 1 {
			t.Fatalf("component %v has the wrong source or epoch", policy)
		}
		got[string(policy.GrantPolicyID())] = true
	}
	want := map[string]bool{"policy": true}
	for _, label := range []string{"secret", "Secret", "a.b", "a-b"} {
		want[string(mustLabelPolicyID(t, "source", label))] = true
	}
	if len(got) != len(want) {
		t.Fatalf("rule grants = %v, want %v", got, want)
	}
	for key := range want {
		if !got[key] {
			t.Fatalf("rule is missing %q", key)
		}
	}

	labels := make([]string, 0, MaxLabelsPerRule+1)
	for index := 0; index < MaxLabelsPerRule; index++ {
		labels = append(labels, "l"+strconv.Itoa(index))
	}
	full, err := LabelRule(source, append(labels, labels...))
	if err != nil {
		t.Fatalf("LabelRule(%d distinct, duplicated) = %v", MaxLabelsPerRule, err)
	}
	if len(full.components()) != MaxLabelsPerRule+1 {
		t.Fatalf("full rule has %d components", len(full.components()))
	}
	if _, err := LabelRule(source, append(labels, "one-more")); err == nil {
		t.Fatalf("LabelRule accepted %d distinct labels", MaxLabelsPerRule+1)
	}
	if _, err := LabelRule(source, []string{"ok", "not ok"}); err == nil {
		t.Fatal("LabelRule accepted an invalid label")
	}
	if _, err := LabelRule(auth.Policy{}, nil); err == nil {
		t.Fatal("LabelRule accepted a zero source policy")
	}
}

func distinctLabels(count int) []string {
	labels := make([]string, 0, count)
	for index := 0; index < count; index++ {
		labels = append(labels, "l"+strconv.Itoa(index))
	}
	return labels
}

func requireRefusalNaming(t *testing.T, err error, bound string) {
	t.Helper()
	if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("error = %v, want invalid_argument naming %s", err, bound)
	}
	if !strings.Contains(err.Error(), bound) {
		t.Fatalf("error %q does not name %s", err, bound)
	}
}

// TestLabelRuleFlattenedTermBound: one source flattens to d:, s: and g: plus
// one g: per label, so 61 labels give exactly auth.MaxPolicyTerms terms and
// 62 are refused. A service-role source carries a fourth term, so there the
// term bound itself is what refuses 61 labels.
func TestLabelRuleFlattenedTermBound(t *testing.T) {
	if MaxLabelsPerRule != auth.MaxPolicyTerms-3 {
		t.Fatalf("MaxLabelsPerRule = %d", MaxLabelsPerRule)
	}
	source := labelTestPolicy(t, "domain", "source", "policy", 1)
	rule, err := LabelRule(source, distinctLabels(61))
	if err != nil {
		t.Fatalf("LabelRule(61 labels) = %v", err)
	}
	expression, err := auth.ConjoinPolicies(rule.components()...)
	if err != nil {
		t.Fatalf("ConjoinPolicies(61-label rule) = %v", err)
	}
	if terms := len(bytes.Split(expression, []byte{'&'})); terms != auth.MaxPolicyTerms {
		t.Fatalf("61-label rule flattens to %d terms, want %d", terms, auth.MaxPolicyTerms)
	}
	_, err = LabelRule(source, distinctLabels(62))
	requireRefusalNaming(t, err, "MaxLabelsPerRule")

	service := servicePolicyForTest(t, source, auth.ServiceRoleDataWrite)
	if _, err := LabelRule(service, distinctLabels(60)); err != nil {
		t.Fatalf("LabelRule(service source, 60 labels) = %v", err)
	}
	_, err = LabelRule(service, distinctLabels(61))
	requireRefusalNaming(t, err, "MaxPolicyTerms")
}

// TestLabelRuleFlattenedByteBound: with a long source every label term is
// about 200 bytes, so the 4 KiB expression bound binds long before the term
// bound. The last accepted rule must flatten; the next label is refused.
func TestLabelRuleFlattenedByteBound(t *testing.T) {
	source := labelTestPolicy(t, "domain", strings.Repeat("s", 100), "policy", 1)
	accepted := -1
	for count := 1; count <= MaxLabelsPerRule; count++ {
		rule, err := LabelRule(source, distinctLabels(count))
		if err != nil {
			requireRefusalNaming(t, err, "MaxPolicyExpressionBytes")
			break
		}
		expression, err := auth.ConjoinPolicies(rule.components()...)
		if err != nil {
			t.Fatalf("LabelRule accepted %d labels that do not flatten: %v", count, err)
		}
		if len(expression) > auth.MaxPolicyExpressionBytes {
			t.Fatalf("flattened %d-label rule is %d bytes", count, len(expression))
		}
		accepted = count
	}
	if accepted < 1 || accepted >= MaxLabelsPerRule {
		t.Fatalf("byte bound never refused; last accepted = %d", accepted)
	}
	// The refusal sits at the bound: the accepted rule is within one label
	// term of it.
	rule, err := LabelRule(source, distinctLabels(accepted))
	if err != nil {
		t.Fatal(err)
	}
	expression, err := auth.ConjoinPolicies(rule.components()...)
	if err != nil {
		t.Fatal(err)
	}
	labelTerm := len("g:") + (auth.MaxPolicyComponentBytes*8+4)/5 + len(":e:1") + 1
	if len(expression)+labelTerm <= auth.MaxPolicyExpressionBytes {
		t.Fatalf("refused %d labels at %d bytes, well below the bound",
			accepted+1, len(expression))
	}
}

// TestStaticPolicySelectorRefusesLabelNamespace: a source policy must never
// be a label policy, so a static selector cannot be configured with one, in
// any version of the namespace or in its reserved part.
func TestStaticPolicySelectorRefusesLabelNamespace(t *testing.T) {
	for _, grant := range []string{
		string(mustLabelPolicyID(t, "source", "secret")),
		UntranslatableLabelPolicyID,
		"shoal.label/v2/anything",
		"shoal.label/",
	} {
		if _, err := NewStaticPolicySelector([]byte("source"), []byte(grant)); !shoal.IsErrorCode(
			err, shoal.ErrorInvalidArgument) {
			t.Fatalf("NewStaticPolicySelector(%q) = %v, want refusal", grant, err)
		}
	}
	if _, err := NewStaticPolicySelector([]byte("source"), []byte("shoal.labels")); err != nil {
		t.Fatalf("NewStaticPolicySelector(outside the namespace) = %v", err)
	}
}

// TestLabelRuleAuthorizeRequiresTheLabelOnThatSource is a smoke test of the
// mechanism: holding the source is not enough, and holding the same label on
// another source does not help.
func TestLabelRuleAuthorizeRequiresTheLabelOnThatSource(t *testing.T) {
	now := time.Date(2026, time.October, 9, 12, 0, 0, 0, time.UTC)
	source := labelTestPolicy(t, "domain", "A", "policy", 1)
	rule, err := LabelRule(source, []string{"secret"})
	if err != nil {
		t.Fatal(err)
	}
	decide := func(policies ...[]byte) auth.Decision {
		decision, err := auth.NewDecision(auth.DecisionConfig{
			Subject:               "subject",
			Actor:                 "actor",
			AuthorizationDomain:   []byte("domain"),
			AllowedOperations:     []auth.Operation{auth.OperationRead},
			PermittedSourceIDs:    [][]byte{[]byte("A"), []byte("B")},
			PermittedPolicyIDs:    append([][]byte{[]byte("policy")}, policies...),
			PolicyGeneration:      1,
			AuthenticationExpires: now.Add(time.Hour),
			RequestID:             "request",
		})
		if err != nil {
			t.Fatal(err)
		}
		return decision
	}
	if err := rule.Authorize(decide(), auth.OperationRead, now); err == nil {
		t.Fatal("source-only reader was authorized for a labelled document")
	}
	if err := rule.Authorize(
		decide(mustLabelPolicyID(t, "B", "secret")), auth.OperationRead, now,
	); err == nil {
		t.Fatal("a grant for (B, secret) opened (A, secret)")
	}
	if err := rule.Authorize(
		decide(mustLabelPolicyID(t, "A", "Secret")), auth.OperationRead, now,
	); err == nil {
		t.Fatal("a grant for Secret opened secret")
	}
	if err := rule.Authorize(
		decide(mustLabelPolicyID(t, "A", "secret")), auth.OperationRead, now,
	); err != nil {
		t.Fatalf("holder of (A, secret) was denied: %v", err)
	}
	untranslatable, err := UntranslatablePolicy(source)
	if err != nil {
		t.Fatal(err)
	}
	closed, err := NewAccessRule(source, untranslatable)
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Authorize(
		decide(mustLabelPolicyID(t, "A", "secret")), auth.OperationRead, now,
	); err == nil {
		t.Fatal("untranslatable rule was authorized")
	}
	// Nor can any decision hold the reserved ID that would open it.
	if _, err := auth.NewDecision(auth.DecisionConfig{
		Subject:               "subject",
		Actor:                 "actor",
		AuthorizationDomain:   []byte("domain"),
		AllowedOperations:     []auth.Operation{auth.OperationRead},
		PermittedSourceIDs:    [][]byte{[]byte("A")},
		PermittedPolicyIDs:    [][]byte{[]byte("policy"), []byte(UntranslatableLabelPolicyID)},
		PolicyGeneration:      1,
		AuthenticationExpires: now.Add(time.Hour),
		RequestID:             "request",
	}); err == nil {
		t.Fatal("a decision was minted holding the untranslatable policy")
	}
}

// TestLabelRuleDurableRoundTrip persists label rules through the durable
// policy store's codec and a reopen, and requires the identical rule back.
func TestLabelRuleDurableRoundTrip(t *testing.T) {
	source := labelTestPolicy(t, "domain", "source/12", "policy", 5)
	labelled, err := LabelRule(source, []string{"secret", "Secret", "x:y", "a.b"})
	if err != nil {
		t.Fatal(err)
	}
	untranslatable, err := UntranslatablePolicy(source)
	if err != nil {
		t.Fatal(err)
	}
	closed, err := NewAccessRule(source, untranslatable)
	if err != nil {
		t.Fatal(err)
	}

	for _, rule := range []AccessRule{labelled, closed} {
		record, err := ruleToPersisted(rule)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := ruleFromPersisted(record)
		if err != nil {
			t.Fatal(err)
		}
		if !decoded.equal(rule) {
			t.Fatalf("persisted rule round trip = %v, want %v", decoded, rule)
		}
	}

	dir := t.TempDir()
	store, err := OpenDurablePolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, tc := range []struct {
		document shoal.ID
		rule     AccessRule
	}{{"labelled", labelled}, {"closed", closed}} {
		if err := store.PutRevision(ctx, RevisionRegistration{
			DocumentID:     tc.document,
			RevisionID:     "revision-" + tc.document,
			NodeIDs:        []shoal.ID{tc.document},
			IntrinsicEdges: []graph.Edge{},
			ContentDigest:  auth.DigestBytes("test-content", []byte(tc.document)),
			Rule:           tc.rule,
			Current:        true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenDurablePolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	for _, tc := range []struct {
		document shoal.ID
		rule     AccessRule
	}{{"labelled", labelled}, {"closed", closed}} {
		registration, ok, err := reopened.Revision(ctx, tc.document, "revision-"+tc.document)
		if err != nil || !ok {
			t.Fatalf("Revision(%q) = %v, %v", tc.document, ok, err)
		}
		if !registration.Rule.equal(tc.rule) {
			t.Fatalf("reloaded rule for %q = %v, want %v",
				tc.document, registration.Rule, tc.rule)
		}
		for _, policy := range registration.Rule.components() {
			if policy.ServiceRole() != "" || policy.Epoch() != 5 ||
				!bytes.Equal(policy.SourceID(), []byte("source/12")) {
				t.Fatalf("reloaded component %v lost a field", policy)
			}
		}
	}
}
