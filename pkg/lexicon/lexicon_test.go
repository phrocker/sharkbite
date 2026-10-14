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

package lexicon_test

import (
	"bytes"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/lexicon"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

var updateGolden = flag.Bool("update", false, "rewrite testdata golden files")

func TestTokenizeNormalizationTable(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"ascii fold", "Payments API", []string{"payments", "api"}},
		{"fullwidth", "ＡＢＣ　Ｄｅｆ１２", []string{"abc", "def12"}},
		{"ligature", "ﬁle ﬂow", []string{"file", "flow"}},
		{"sharp s", "Straße STRASSE", []string{"strasse", "strasse"}},
		{"capital sharp s", "ẞ", []string{"ss"}},
		{"composed accent", "café", []string{"café"}},
		{"decomposed accent", "cafe\u0301", []string{"café"}},
		{"turkish dotted capital", "İstanbul", []string{"i\u0307stanbul"}},
		{"turkish dotless", "ılık", []string{"ılık"}},
		{"plain capital I", "ISTANBUL", []string{"istanbul"}},
		{"hyphenated identifier", "payments-api", []string{"payments", "api"}},
		{"mixed separators", "svc_v2.prod/eu:west", []string{"svc", "v2", "prod", "eu", "west"}},
		{"camel case is one token", "PaymentsAPI", []string{"paymentsapi"}},
		{"parenthesized digit", "⑴x", []string{"1", "x"}},
		{"roman numeral", "Ⅻ", []string{"xii"}},
		{"greek final sigma", "ΟΔΟΣ οδος", []string{"οδοσ", "οδοσ"}},
		{"punctuation only", "-- / ..", nil},
		{"invalid utf8", "ab\xffcd", []string{"ab", "cd"}},
		{"empty", "", nil},
		// x/text swaps Cherokee case on every fold; both cases map to the
		// form Unicode case folding chooses (the U+13A0 block).
		{"cherokee capitals", "ᏣᎳᎩ", []string{"ᏣᎳᎩ"}},
		{"cherokee small", "ꮳꮃꭹ", []string{"ᏣᎳᎩ"}},
		{"cherokee fuzz minimum", "ꮴ", []string{"Ꮴ"}},
		{"cherokee small ye", "ᏸ", []string{"Ᏸ"}},
		{"zero width joiner", "pay\u200dments", []string{"payments"}},
		{"zero width non-joiner", "pay\u200cments", []string{"payments"}},
		{"soft hyphen", "pay\u00adments", []string{"payments"}},
		{"byte order mark", "\ufeffpayments", []string{"payments"}},
		{"ignorable between base and mark", "cafe\u200d\u0301", []string{"café"}},
		{"variation selector", "snow\ufe0fman", []string{"snowman"}},
		{"emoji are dropped", "🚀 launch 👩\u200d💻 ☃\ufe0f", []string{"launch"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, token := range lexicon.Tokenize(tc.in) {
				got = append(got, token.Text)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Tokenize(%q) = %q, want %q", tc.in, got, tc.want)
			}
			// Tokenizing a token gives back exactly that token.
			for _, token := range got {
				again := lexicon.Tokenize(token)
				if len(again) != 1 || again[0].Text != token {
					t.Fatalf("Tokenize(%q) = %#v, not idempotent", token, again)
				}
			}
		})
	}
}

func TestTokenizeSpansSkipIgnorables(t *testing.T) {
	text := "x pay\u00adments\u200d y"
	tokens := lexicon.Tokenize(text)
	if len(tokens) != 3 || text[tokens[1].Start:tokens[1].End] != "pay\u00adments" {
		t.Fatalf("tokens = %#v", tokens)
	}
	if text[tokens[2].Start:tokens[2].End] != "y" {
		t.Fatalf("tokens = %#v", tokens)
	}
}

func TestTokenizeByteSpansCoverSource(t *testing.T) {
	text := "Ｈi ⑴, ﬁx-it"
	tokens := lexicon.Tokenize(text)
	want := []string{"Ｈi", "⑴", "ﬁx", "it"}
	if len(tokens) != len(want) {
		t.Fatalf("tokens = %#v", tokens)
	}
	for index, token := range tokens {
		if got := text[token.Start:token.End]; got != want[index] {
			t.Fatalf("token %d span = %q, want %q", index, got, want[index])
		}
	}
}

type fixture struct {
	nodes         []graph.Node
	relationships []ontology.RelationshipDefinition
}

func newFixture(t testing.TB) fixture {
	t.Helper()
	service, err := ontology.NewConceptDefinition("service", "Service", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	team, err := ontology.NewConceptDefinition("team", "Team", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	dependsOn, err := ontology.NewRelationshipDefinition(
		"depends_on", "depends on", "",
		[]shoal.ID{service.ID()}, []shoal.ID{service.ID()}, nil, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	owns, err := ontology.NewRelationshipDefinition(
		"owns", "owns", "",
		[]shoal.ID{team.ID()}, []shoal.ID{service.ID()}, nil, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	peers, err := ontology.NewRelationshipDefinition(
		"peers_with", "peers with", "",
		[]shoal.ID{team.ID()}, []shoal.ID{service.ID(), team.ID()}, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	return fixture{
		nodes: []graph.Node{
			{ID: "svc-payments", Kind: "service", Properties: shoal.Metadata{
				"name":                      "Payments API",
				"shoal.ontology.entity_key": "payments-api",
				"shoal.lexicon.alias.0":     "billing gateway",
				"shoal.lexicon.alias.1":     "PayGW",
				"owner":                     "not a name",
			}},
			{ID: "svc-ledger", Kind: "service", Properties: shoal.Metadata{
				"title":                 "Ledger",
				"shoal.lexicon.alias.0": "PayGW",
			}},
			{ID: "team-core", Kind: "team", Properties: shoal.Metadata{
				"name": "Core Platform", "title": "ignored when name is set",
			}},
			{ID: "no-names", Kind: "service", Properties: shoal.Metadata{
				"owner": "x",
			}},
		},
		relationships: []ontology.RelationshipDefinition{dependsOn, owns, peers},
	}
}

var fixedSnapshot = lexicon.Snapshot{
	ID:       "snapshot-1",
	AsOf:     time.Date(2026, time.October, 8, 12, 0, 0, 0, time.UTC),
	Frontier: 42,
}

var fixedDigest = [32]byte{1, 2, 3}

func (f fixture) input() lexicon.Input {
	return lexicon.Input{
		Snapshot: fixedSnapshot, Nodes: f.nodes, Relationships: f.relationships,
	}
}

// pinnedInput is the input for a pinned build of nodes. The scoped node set
// comes from the test-only seal; outside tests only the authorized filter
// can make one.
func pinnedInput(nodes []graph.Node, digest [32]byte) lexicon.Input {
	scoped := lexicon.SealForTest(nodes, fixedSnapshot, digest)
	return lexicon.Input{Scoped: &scoped}
}

func mustBuild(t testing.TB, in lexicon.Input) *lexicon.Bundle {
	t.Helper()
	bundle, err := lexicon.Build(in, lexicon.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

// shipped returns a bundle's canonical bytes, through Shippable for a pinned
// bundle and through the test-only accessor for a server-filtered one.
func shipped(t testing.TB, bundle *lexicon.Bundle) []byte {
	t.Helper()
	if shippable, ok := bundle.ForShipping(); ok {
		return shippable.Bytes()
	}
	return lexicon.BytesForTest(bundle)
}

// shuffled rebuilds every node and property map in a random order and shuffles
// the node and relationship slices.
func shuffled(f fixture, rng *rand.Rand) fixture {
	out := fixture{
		nodes:         make([]graph.Node, len(f.nodes)),
		relationships: append([]ontology.RelationshipDefinition(nil), f.relationships...),
	}
	for index, node := range f.nodes {
		keys := make([]string, 0, len(node.Properties))
		for key := range node.Properties {
			keys = append(keys, key)
		}
		rng.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
		properties := make(shoal.Metadata, len(keys))
		for _, key := range keys {
			properties[key] = node.Properties[key]
		}
		node.Properties = properties
		out.nodes[index] = node
	}
	rng.Shuffle(len(out.nodes), func(i, j int) {
		out.nodes[i], out.nodes[j] = out.nodes[j], out.nodes[i]
	})
	rng.Shuffle(len(out.relationships), func(i, j int) {
		out.relationships[i], out.relationships[j] =
			out.relationships[j], out.relationships[i]
	})
	return out
}

func TestRebuildIsByteIdenticalUnderShuffledInput(t *testing.T) {
	f := newFixture(t)
	reference := shipped(t, mustBuild(t, f.input()))
	pinnedReference := shipped(t, mustBuild(t, pinnedInput(f.nodes, fixedDigest)))
	rng := rand.New(rand.NewSource(7))
	for round := 0; round < 50; round++ {
		g := shuffled(f, rng)
		if got := shipped(t, mustBuild(t, g.input())); !bytes.Equal(got, reference) {
			t.Fatalf("round %d: rebuild differs", round)
		}
		if got := shipped(t, mustBuild(t, pinnedInput(g.nodes, fixedDigest))); !bytes.Equal(
			got, pinnedReference) {
			t.Fatalf("round %d: pinned rebuild differs", round)
		}
	}
}

func TestGoldenBundleDigest(t *testing.T) {
	bundle := mustBuild(t, newFixture(t).input())
	path := filepath.Join("testdata", "golden_bundle_id.txt")
	got := bundle.ID().String() + "\n"
	if *updateGolden {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("bundle ID = %s, golden = %s.\n"+
			"Either the canonical encoding or tokenization changed (bump "+
			"NormalizationVersion), or the Unicode tables did: x/text picks its "+
			"tables by Go toolchain version (15.0.0 before go1.27, 17.0.0 from "+
			"go1.27), and this build uses x/text norm %s, cases %s, stdlib unicode "+
			"%s. Every bundle ID changes with them, by design. If intended, rerun "+
			"with -args -update.",
			got, want, norm.Version, cases.UnicodeVersion, unicode.Version)
	}
}

func TestLoadRoundTrip(t *testing.T) {
	for name, built := range map[string]*lexicon.Bundle{
		"server-filtered": mustBuild(t, newFixture(t).input()),
		"pinned":          mustBuild(t, pinnedInput(newFixture(t).nodes, fixedDigest)),
	} {
		data := shipped(t, built)
		loaded, err := lexicon.Load(data)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if loaded.ID() != built.ID() || !bytes.Equal(shipped(t, loaded), data) {
			t.Fatalf("%s: round trip changed the bundle", name)
		}
		if !reflect.DeepEqual(loaded.Templates(), built.Templates()) ||
			!reflect.DeepEqual(loaded.Snapshot(), built.Snapshot()) ||
			!reflect.DeepEqual(loaded.Scope(), built.Scope()) {
			t.Fatalf("%s: round trip changed bundle metadata", name)
		}
		text := "Is the payments api behind PayGW or the billing gateway?"
		if !reflect.DeepEqual(loaded.Candidates(text), built.Candidates(text)) {
			t.Fatalf("%s: round trip changed matching", name)
		}
		verified, err := lexicon.LoadVerified(data, built.ID())
		if err != nil || verified.ID() != built.ID() {
			t.Fatalf("%s: LoadVerified = %v", name, err)
		}
		if _, err := lexicon.LoadVerified(data, lexicon.BundleID{}); err == nil {
			t.Fatalf("%s: LoadVerified accepted a wrong ID", name)
		}
	}
	pinned := mustBuild(t, pinnedInput(newFixture(t).nodes, fixedDigest))
	if scope, ok := pinned.Scope().(lexicon.ScopePinned); !ok || scope.Digest() != fixedDigest {
		t.Fatalf("pinned scope = %v", pinned.Scope())
	}
}

func TestLoadRefusesTamperedAndNonCanonicalBytes(t *testing.T) {
	built := mustBuild(t, newFixture(t).input())
	data := shipped(t, built)
	for length := 0; length < len(data); length++ {
		if _, err := lexicon.Load(data[:length]); err == nil {
			t.Fatalf("Load accepted a %d-byte truncation", length)
		}
	}
	if _, err := lexicon.Load(append(append([]byte(nil), data...), 0)); err == nil {
		t.Fatal("Load accepted trailing bytes")
	}
	// A flipped bit either breaks a canonical invariant or yields different
	// contents, which then carry a different ID; it never passes as the
	// original.
	for index := range data {
		tampered := append([]byte(nil), data...)
		tampered[index] ^= 0x01
		loaded, err := lexicon.Load(tampered)
		if err == nil && loaded.ID() == built.ID() {
			t.Fatalf("bit flip at %d kept the bundle ID", index)
		}
		if _, err := lexicon.LoadVerified(tampered, built.ID()); err == nil {
			t.Fatalf("LoadVerified accepted a bit flip at %d", index)
		}
	}
	// A token that is not in normalized form is refused even though it
	// decodes: "payments" is replaced by "PAYMENTS" in place.
	upper := bytes.Replace(data, []byte("payments"), []byte("PAYMENTS"), 1)
	if bytes.Equal(upper, data) {
		t.Fatal("fixture token not found")
	}
	if _, err := lexicon.Load(upper); err == nil {
		t.Fatal("Load accepted a non-normalized token")
	}
}

func TestPinChangesID(t *testing.T) {
	f := newFixture(t)
	base := mustBuild(t, f.input()).ID()
	variants := map[string]func(*lexicon.Input){
		"snapshot ID": func(in *lexicon.Input) { in.Snapshot.ID = "snapshot-2" },
		"frontier":    func(in *lexicon.Input) { in.Snapshot.Frontier++ },
		"as of": func(in *lexicon.Input) {
			in.Snapshot.AsOf = in.Snapshot.AsOf.Add(time.Nanosecond)
		},
	}
	for name, mutate := range variants {
		in := f.input()
		mutate(&in)
		if mustBuild(t, in).ID() == base {
			t.Fatalf("%s change kept the bundle ID", name)
		}
	}
	// The same instant in another location is the same pin.
	in := f.input()
	in.Snapshot.AsOf = in.Snapshot.AsOf.In(time.FixedZone("x", 3600))
	if mustBuild(t, in).ID() != base {
		t.Fatal("time zone changed the bundle ID")
	}
	// Scope kind and digest are part of the ID.
	unscoped := mustBuild(t, lexicon.Input{Snapshot: fixedSnapshot, Nodes: f.nodes}).ID()
	pinnedA := mustBuild(t, pinnedInput(f.nodes, fixedDigest)).ID()
	pinnedB := mustBuild(t, pinnedInput(f.nodes, [32]byte{9})).ID()
	if unscoped == pinnedA || pinnedA == pinnedB {
		t.Fatal("scope did not change the bundle ID")
	}
}

func TestOnlyScopedNodesBuildShippableBundles(t *testing.T) {
	f := newFixture(t)
	server := mustBuild(t, f.input())
	if _, ok := server.ForShipping(); ok {
		t.Fatal("server-filtered bundle is shippable")
	}
	if _, ok := server.Scope().(lexicon.ScopeServerFiltered); !ok {
		t.Fatalf("scope = %v", server.Scope())
	}
	pinned := mustBuild(t, pinnedInput(f.nodes, fixedDigest))
	shippable, ok := pinned.ForShipping()
	if !ok || shippable.Scope().Digest() != fixedDigest || shippable.ID() != pinned.ID() {
		t.Fatal("pinned bundle did not ship with its scope")
	}
	// A ScopedNodes not made by the authorized filter is the zero value,
	// which is refused; a ScopePinned literal cannot be passed to Build.
	if _, err := lexicon.Build(lexicon.Input{
		Snapshot: fixedSnapshot, Scoped: &lexicon.ScopedNodes{},
	}, lexicon.Limits{}); err == nil {
		t.Fatal("unsealed scoped nodes accepted")
	}
	zeroDigest := lexicon.SealForTest(f.nodes, fixedSnapshot, [32]byte{})
	if _, err := lexicon.Build(lexicon.Input{Scoped: &zeroDigest}, lexicon.Limits{}); err == nil {
		t.Fatal("scoped nodes without a digest accepted")
	}
	if (lexicon.ScopePinned{}).Digest() != ([32]byte{}) {
		t.Fatal("a ScopePinned literal carries a digest")
	}
	scoped := lexicon.SealForTest(f.nodes[:1], fixedSnapshot, fixedDigest)
	for name, in := range map[string]lexicon.Input{
		"extra unfiltered nodes": {Scoped: &scoped, Nodes: f.nodes},
		"templates":              {Scoped: &scoped, Relationships: f.relationships},
		"other snapshot": {Scoped: &scoped, Snapshot: lexicon.Snapshot{
			ID: "other", AsOf: fixedSnapshot.AsOf, Frontier: 1,
		}},
	} {
		if _, err := lexicon.Build(in, lexicon.Limits{}); !shoal.IsErrorCode(
			err, shoal.ErrorInvalidArgument) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
	matching := lexicon.Input{Scoped: &scoped, Snapshot: fixedSnapshot}
	if got := mustBuild(t, matching).ID(); got != mustBuild(t, lexicon.Input{Scoped: &scoped}).ID() {
		t.Fatal("an equal explicit snapshot changed the build")
	}
}

func TestFormattingNeverShowsContents(t *testing.T) {
	f := newFixture(t)
	server := mustBuild(t, f.input())
	pinned := mustBuild(t, pinnedInput(f.nodes, fixedDigest))
	shippable, _ := pinned.ForShipping()
	var nilBundle *lexicon.Bundle
	type wrapper struct {
		Bundle    *lexicon.Bundle
		Shippable lexicon.Shippable
	}
	values := []any{server, pinned, shippable, *server, nilBundle,
		wrapper{Bundle: server, Shippable: shippable}, []*lexicon.Bundle{server},
		struct {
			b lexicon.Bundle
			s lexicon.Shippable
		}{*server, shippable}}
	forbidden := []string{"payments", "Payments", "svc-payments", "billing", "paygw",
		"svc-ledger", "depends_on", "7061796d656e7473" /* hex of "payments" */}
	for _, value := range values {
		for _, verb := range []string{"%s", "%v", "%+v", "%#v", "%x", "%X", "%q", "%d", "%T %v"} {
			out := fmt.Sprintf(verb, value)
			for _, secret := range forbidden {
				if strings.Contains(out, secret) {
					t.Fatalf("%s of %T shows %q: %s", verb, value, secret, out)
				}
			}
		}
	}
	if got := server.String(); !strings.Contains(got, server.ID().String()) ||
		!strings.Contains(got, "server-filtered") {
		t.Fatalf("String = %q", got)
	}
	// Every verb prints exactly the safe description.
	for _, value := range []fmt.Stringer{server, *server, pinned, shippable} {
		for _, verb := range []string{"%s", "%v", "%+v", "%#v", "%x", "%q", "%d"} {
			if got := fmt.Sprintf(verb, value); got != value.String() {
				t.Fatalf("%s of %T = %q, want %q", verb, value, got, value.String())
			}
		}
	}
	if got := fmt.Sprint(shippable); !strings.Contains(got, "pinned:") {
		t.Fatalf("Shippable = %q", got)
	}
}

func TestCandidatesReportEveryOverlap(t *testing.T) {
	bundle := mustBuild(t, newFixture(t).input())
	text := "does the Billing-Gateway (payments api) call PayGW? pa"
	// "pa" would be the initialism of "Payments API", but derivation is off
	// by default and needs three tokens when on.
	got := bundle.Candidates(text)
	type summary struct {
		span  lexicon.Span
		text  string
		nodes []shoal.ID
	}
	var summaries []summary
	for _, candidate := range got {
		summaries = append(summaries, summary{
			span:  candidate.TokenSpan,
			text:  text[candidate.ByteSpan.Start:candidate.ByteSpan.End],
			nodes: candidate.NodeIDs,
		})
	}
	want := []summary{
		{lexicon.Span{Start: 2, End: 4}, "Billing-Gateway", []shoal.ID{"svc-payments"}},
		{lexicon.Span{Start: 4, End: 6}, "payments api", []shoal.ID{"svc-payments"}},
		{lexicon.Span{Start: 7, End: 8}, "PayGW", []shoal.ID{"svc-ledger", "svc-payments"}},
	}
	if !reflect.DeepEqual(summaries, want) {
		t.Fatalf("candidates = %#v\nwant %#v", summaries, want)
	}
}

func TestSelectFiltersBeforeChoosing(t *testing.T) {
	nodes := []graph.Node{
		{ID: "short", Kind: "service", Properties: shoal.Metadata{"name": "payments"}},
		{ID: "long-hidden", Kind: "service", Properties: shoal.Metadata{"name": "payments api"}},
		{ID: "shared-visible", Kind: "service", Properties: shoal.Metadata{"name": "gateway"}},
		{ID: "shared-hidden", Kind: "service", Properties: shoal.Metadata{"name": "gateway"}},
		{ID: "twin-a", Kind: "team", Properties: shoal.Metadata{"name": "core"}},
		{ID: "twin-b", Kind: "team", Properties: shoal.Metadata{"name": "core"}},
	}
	bundle := mustBuild(t, lexicon.Input{
		Snapshot: fixedSnapshot, Nodes: nodes,
	})
	hidden := map[shoal.ID]bool{"long-hidden": true, "shared-hidden": true}
	visible := func(id shoal.ID) bool { return !hidden[id] }
	got := lexicon.Select(bundle.Candidates("payments api via gateway for core"), visible)
	want := []lexicon.Mention{
		{
			TokenSpan: lexicon.Span{Start: 0, End: 1}, ByteSpan: lexicon.Span{Start: 0, End: 8},
			NodeIDs: []shoal.ID{"short"}, Kinds: []string{"service"},
		},
		{
			TokenSpan: lexicon.Span{Start: 3, End: 4}, ByteSpan: lexicon.Span{Start: 17, End: 24},
			NodeIDs: []shoal.ID{"shared-visible"}, Kinds: []string{"service"},
		},
		{
			TokenSpan: lexicon.Span{Start: 5, End: 6}, ByteSpan: lexicon.Span{Start: 29, End: 33},
			NodeIDs: []shoal.ID{"twin-a", "twin-b"}, Kinds: []string{"team", "team"},
			Ambiguous: true,
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Select = %#v\nwant %#v", got, want)
	}
	all := lexicon.Select(bundle.Candidates("payments api"), func(shoal.ID) bool { return true })
	if len(all) != 1 || all[0].NodeIDs[0] != "long-hidden" {
		t.Fatalf("leftmost-longest over all = %#v", all)
	}
}

func TestDeriveTemplates(t *testing.T) {
	templates := mustBuild(t, newFixture(t).input()).Templates()
	var ids []string
	for _, template := range templates {
		ids = append(ids, template.ID)
	}
	want := []string{
		"lookup:depends_on:in", "lookup:depends_on:out",
		"lookup:owns:in", "lookup:owns:out", "lookup:peers_with:both",
	}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("template IDs = %q", ids)
	}
	owns := templates[3]
	if owns.Direction != lexicon.DirectionOut || owns.RelationKey != "owns" ||
		owns.PhraseKey != "lexicon.lookup.owns.out" ||
		len(owns.SubjectConcepts) != 1 || len(owns.AnswerConcepts) != 1 ||
		owns.SubjectConcepts[0] == owns.AnswerConcepts[0] {
		t.Fatalf("owns:out = %#v", owns)
	}
	in := templates[2]
	if !reflect.DeepEqual(in.SubjectConcepts, owns.AnswerConcepts) ||
		!reflect.DeepEqual(in.AnswerConcepts, owns.SubjectConcepts) {
		t.Fatalf("owns:in does not reverse owns:out: %#v", in)
	}
	f := newFixture(t)
	duplicate := append(f.relationships, f.relationships[0])
	if _, err := lexicon.DeriveTemplates(duplicate); err == nil {
		t.Fatal("duplicate relationship keys accepted")
	}
}

func TestLimitsFailTheBuild(t *testing.T) {
	f := newFixture(t)
	cases := map[string]lexicon.Limits{
		"nodes":     {MaxNodes: 3},
		"terms":     {MaxTerms: 2},
		"tokens":    {MaxTokens: 2},
		"length":    {MaxTermTokens: 1},
		"postings":  {MaxPostingsPerTerm: 1},
		"bytes":     {MaxTokenBytes: 3},
		"templates": {MaxTemplates: 2},
		"bundle":    {MaxBundleBytes: 64},
	}
	for name, limits := range cases {
		if _, err := lexicon.Build(f.input(), limits); !shoal.IsErrorCode(
			err, shoal.ErrorInvalidArgument) {
			t.Fatalf("%s limit: err = %v", name, err)
		}
	}
	if _, err := lexicon.Build(f.input(), lexicon.Limits{
		MaxPostingsPerTerm: lexicon.HardMaxPostingsPerTerm + 1,
	}); err == nil {
		t.Fatal("limit above the hard bound accepted")
	}
}

func TestBuildRejectsReservedAndDuplicateIDs(t *testing.T) {
	for name, nodes := range map[string][]graph.Node{
		"sentinel": {{ID: shoal.ID(lexicon.SentinelIDPrefix + "x"),
			Properties: shoal.Metadata{"name": "x"}}},
		"duplicate": {
			{ID: "a", Properties: shoal.Metadata{"name": "x"}},
			{ID: "a", Properties: shoal.Metadata{"name": "y"}},
		},
		"invalid": {{ID: ""}},
	} {
		if _, err := lexicon.Build(lexicon.Input{
			Snapshot: fixedSnapshot, Nodes: nodes,
		}, lexicon.Limits{}); err == nil {
			t.Fatalf("%s node accepted", name)
		}
	}
	if _, err := lexicon.Build(lexicon.Input{
		Snapshot: lexicon.Snapshot{ID: "s"},
	}, lexicon.Limits{}); err == nil {
		t.Fatal("zero snapshot time accepted")
	}
}

// TestCandidatesMatchBruteForce compares the automaton with a direct scan of
// every span against the term set, on random vocabularies.
func TestCandidatesMatchBruteForce(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	words := []string{"a", "b", "c", "d", "e"}
	for round := 0; round < 200; round++ {
		termSet := map[string][]shoal.ID{}
		var nodes []graph.Node
		for index := 0; index < 1+rng.Intn(12); index++ {
			length := 1 + rng.Intn(4)
			parts := make([]string, length)
			for position := range parts {
				parts[position] = words[rng.Intn(len(words))]
			}
			name := strings.Join(parts, " ")
			id := shoal.ID(name + "#" + string(rune('A'+index)))
			nodes = append(nodes, graph.Node{ID: id, Properties: shoal.Metadata{
				"shoal.lexicon.alias.0": name,
			}})
			termSet[name] = append(termSet[name], id)
		}
		bundle, err := lexicon.Build(lexicon.Input{
			Snapshot: fixedSnapshot, Nodes: nodes,
		}, lexicon.Limits{MaxPostingsPerTerm: 64})
		if err != nil {
			t.Fatal(err)
		}
		textParts := make([]string, 1+rng.Intn(10))
		for position := range textParts {
			textParts[position] = append(words, "zz")[rng.Intn(len(words)+1)]
		}
		var want []lexicon.Span
		for start := range textParts {
			for end := start + 1; end <= len(textParts); end++ {
				if _, ok := termSet[strings.Join(textParts[start:end], " ")]; ok {
					want = append(want, lexicon.Span{Start: start, End: end})
				}
			}
		}
		var got []lexicon.Span
		for _, candidate := range bundle.Candidates(strings.Join(textParts, " ")) {
			got = append(got, candidate.TokenSpan)
			key := strings.Join(textParts[candidate.TokenSpan.Start:candidate.TokenSpan.End], " ")
			if len(candidate.NodeIDs) != len(termSet[key]) {
				t.Fatalf("round %d: postings for %q = %v", round, key, candidate.NodeIDs)
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("round %d: text %q got %v want %v", round, textParts, got, want)
		}
		bound := bundle.CandidateBound(len(textParts))
		distinct := map[shoal.ID]struct{}{}
		for _, candidate := range bundle.Candidates(strings.Join(textParts, " ")) {
			for _, id := range candidate.NodeIDs {
				distinct[id] = struct{}{}
			}
		}
		if len(distinct) > bound {
			t.Fatalf("round %d: %d candidates exceed bound %d", round, len(distinct), bound)
		}
	}
}

// sharedInitialNodes gives count nodes whose names have tokens tokens each and
// all share the same initials.
func sharedInitialNodes(count, tokens int) []graph.Node {
	nodes := make([]graph.Node, count)
	for index := range nodes {
		parts := make([]string, tokens)
		for position := range parts {
			parts[position] = fmt.Sprintf("%c%03d", 'p'+position, index)
		}
		nodes[index] = graph.Node{
			ID:         shoal.ID(fmt.Sprintf("node-%03d", index)),
			Properties: shoal.Metadata{"name": strings.Join(parts, " ")},
		}
	}
	return nodes
}

func TestInitialismsAreOptIn(t *testing.T) {
	limits := lexicon.Limits{MaxPostingsPerTerm: 8}
	// Common two-word collisions never fail the default build, and with the
	// option on, two-token names still derive nothing.
	for _, derive := range []bool{false, true} {
		bundle, err := lexicon.Build(lexicon.Input{
			Snapshot: fixedSnapshot,
			Nodes:    sharedInitialNodes(100, 2), DeriveInitialisms: derive,
		}, limits)
		if err != nil {
			t.Fatalf("derive=%v: two-word collisions failed the build: %v", derive, err)
		}
		if bundle.TermCount() != 100 || bundle.DerivesInitialisms() != derive {
			t.Fatalf("derive=%v: terms = %d", derive, bundle.TermCount())
		}
		if got := bundle.Candidates("pq"); got != nil {
			t.Fatalf("derive=%v: two-token initialism derived: %#v", derive, got)
		}
	}
	// Three-token names collide only when derivation is asked for, and then
	// a collision beyond the limit fails the build rather than drop.
	threeWord := sharedInitialNodes(9, 3)
	if _, err := lexicon.Build(lexicon.Input{
		Snapshot: fixedSnapshot, Nodes: threeWord,
	}, limits); err != nil {
		t.Fatalf("default build failed: %v", err)
	}
	if _, err := lexicon.Build(lexicon.Input{
		Snapshot: fixedSnapshot, Nodes: threeWord,
		DeriveInitialisms: true,
	}, limits); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("opt-in collision beyond the limit: err = %v", err)
	}
	within := sharedInitialNodes(8, 3)
	derived, err := lexicon.Build(lexicon.Input{
		Snapshot: fixedSnapshot, Nodes: within,
		DeriveInitialisms: true,
	}, limits)
	if err != nil {
		t.Fatal(err)
	}
	candidates := derived.Candidates("PQR")
	if len(candidates) != 1 || len(candidates[0].NodeIDs) != 8 {
		t.Fatalf("opt-in initialism = %#v", candidates)
	}
}

func TestInitialismOptionChangesID(t *testing.T) {
	nodes := []graph.Node{{ID: "n", Properties: shoal.Metadata{"name": "Core Platform"}}}
	in := lexicon.Input{Snapshot: fixedSnapshot, Nodes: nodes}
	off := mustBuild(t, in)
	in.DeriveInitialisms = true
	on := mustBuild(t, in)
	// Nothing is derived from a two-token name, so only the recorded option
	// differs, and it alone changes the ID.
	if off.TermCount() != on.TermCount() || off.ID() == on.ID() {
		t.Fatalf("option not in the ID: terms %d/%d", off.TermCount(), on.TermCount())
	}
	reloaded, err := lexicon.Load(shipped(t, on))
	if err != nil || !reloaded.DerivesInitialisms() {
		t.Fatalf("option lost on load: %v", err)
	}
}
