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

package inference

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

var testTime = time.Date(2026, 8, 28, 8, 0, 0, 0, time.UTC)

func TestContextPackDocumentGraphAndMixedEvidence(t *testing.T) {
	documentAnchor := mustDocumentAnchor(t, "alpha")
	graphAnchor := mustGraphAnchor(t)
	snapshot, auth := mustPins(t)

	for name, anchors := range map[string][]EvidenceAnchor{
		"document": {documentAnchor},
		"graph":    {graphAnchor},
		"mixed":    {graphAnchor, documentAnchor},
	} {
		t.Run(name, func(t *testing.T) {
			pack, err := NewContextPack(
				"  explain\n grounded\tfacts ",
				anchors,
				nil,
				snapshot,
				auth,
				shoal.Metadata{"tenant": "example"},
			)
			if err != nil {
				t.Fatal(err)
			}
			if pack.Query() != "explain grounded facts" {
				t.Fatalf("normalized query = %q", pack.Query())
			}
			if err := pack.Validate(); err != nil {
				t.Fatal(err)
			}
			for _, anchor := range pack.Evidence() {
				switch anchor.Kind() {
				case AnchorDocument:
					if _, _, ok := anchor.Document(); !ok {
						t.Fatal("document anchor did not expose document variant")
					}
					if path, ok := anchor.Path(); ok || pathPresent(path) {
						t.Fatal("document anchor exposed graph variant")
					}
				case AnchorGraph:
					citation, quote, ok := anchor.Document()
					if ok || citationPresent(citation) || quote != "" {
						t.Fatal("graph anchor exposed fake citation")
					}
				default:
					t.Fatalf("unexpected anchor kind %q", anchor.Kind())
				}
			}
		})
	}
}

func TestEvidenceAnchorRejectsZeroDualAndNestedFailures(t *testing.T) {
	assertInvalid(t, (EvidenceAnchor{}).Validate())

	documentAnchor := mustDocumentAnchor(t, "alpha")
	documentAnchor.path = testPath()
	assertInvalid(t, documentAnchor.Validate())

	graphAnchor := mustGraphAnchor(t)
	graphAnchor.citation = testCitation("alpha")
	assertInvalid(t, graphAnchor.Validate())

	_, err := NewDocumentAnchor(document.Citation{}, "alpha")
	assertInvalid(t, err)
	_, err = NewDocumentAnchor(testCitation("alpha"), "no")
	assertInvalid(t, err)

	broken := testPath()
	broken.Edges[0].To = "wrong"
	_, err = NewGraphAnchor(broken)
	assertInvalid(t, err)
}

func TestContextPackCanonicalityDuplicatesAndBounds(t *testing.T) {
	first := mustDocumentAnchor(t, "alpha")
	second := mustDocumentAnchorAt(t, "bravo", 10)
	snapshot, auth := mustPins(t)
	left, err := NewContextPack(
		"same query", []EvidenceAnchor{second, first}, nil, snapshot, auth, nil)
	if err != nil {
		t.Fatal(err)
	}
	right, err := NewContextPack(
		"same   query", []EvidenceAnchor{first, second}, nil, snapshot, auth, nil)
	if err != nil {
		t.Fatal(err)
	}
	if left.ID() != right.ID() {
		t.Fatal("input order changed context pack identity")
	}
	if shoal.CompareID(left.Evidence()[0].ID(), left.Evidence()[1].ID()) >= 0 {
		t.Fatal("context evidence is not canonically ordered")
	}

	_, err = NewContextPack(
		"query", []EvidenceAnchor{first, first}, nil, snapshot, auth, nil)
	assertInvalid(t, err)
	_, err = NewContextPack(
		"query", nil, nil, snapshot, auth, nil)
	assertInvalid(t, err)
	_, err = NewContextPack(
		strings.Repeat("q", MaxQueryBytes+1), []EvidenceAnchor{first},
		nil, snapshot, auth, nil)
	assertInvalid(t, err)
	tooMany := make([]EvidenceAnchor, MaxEvidenceAnchors+1)
	for index := range tooMany {
		tooMany[index] = first
	}
	_, err = NewContextPack("query", tooMany, nil, snapshot, auth, nil)
	assertInvalid(t, err)
}

func TestGraphPathAndQuoteBounds(t *testing.T) {
	_, err := NewDocumentAnchor(
		testCitation(strings.Repeat("q", MaxQuoteBytes+1)),
		strings.Repeat("q", MaxQuoteBytes+1),
	)
	assertInvalid(t, err)

	path := graph.Path{Nodes: make([]graph.Node, MaxPathNodes+1)}
	_, err = NewGraphAnchor(path)
	assertInvalid(t, err)

	path = testPath()
	path.Nodes[0].Labels = []string{"duplicate", "duplicate"}
	_, err = NewGraphAnchor(path)
	assertInvalid(t, err)
}

func TestOntologyIdentity(t *testing.T) {
	schema, err := ontology.NewOntologySchema("test", "Test", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	version, err := ontology.NewOntologyVersion(
		schema, "v1", testTime, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := NewOntologyIdentity(version)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, auth := mustPins(t)
	pack, err := NewContextPack(
		"query", []EvidenceAnchor{mustGraphAnchor(t)}, &identity,
		snapshot, auth, nil)
	if err != nil {
		t.Fatal(err)
	}
	actual, ok := pack.Ontology()
	if !ok || actual.SchemaID() != schema.ID() || actual.VersionID() != version.ID() {
		t.Fatal("context pack lost ontology identity")
	}
	_, err = NewOntologyIdentityFromIDs(version.ID(), schema.ID())
	assertInvalid(t, err)
}

func TestClaimAndResultCanonicalityAndValidation(t *testing.T) {
	documentAnchor := mustDocumentAnchor(t, "alpha")
	graphAnchor := mustGraphAnchor(t)
	snapshot, auth := mustPins(t)
	pack, err := NewContextPack(
		"query", []EvidenceAnchor{graphAnchor, documentAnchor},
		nil, snapshot, auth, nil)
	if err != nil {
		t.Fatal(err)
	}

	model, prompt := mustProvenance(t)
	object, err := ontology.NewStringValue("value")
	if err != nil {
		t.Fatal(err)
	}
	first, err := NewClaim(
		"subject-a", "predicate", object, 0.75,
		[]shoal.ID{graphAnchor.ID(), documentAnchor.ID()},
		ClaimInferred, model, prompt, nil)
	if err != nil {
		t.Fatal(err)
	}
	reordered, err := NewClaim(
		"subject-a", "predicate", object, 0.75,
		[]shoal.ID{documentAnchor.ID(), graphAnchor.ID()},
		ClaimInferred, model, prompt, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID() != reordered.ID() {
		t.Fatal("evidence order changed claim identity")
	}
	second, err := NewClaim(
		"subject-b", "predicate", object, 1,
		[]shoal.ID{documentAnchor.ID()}, ClaimObserved, model, prompt, nil)
	if err != nil {
		t.Fatal(err)
	}
	unresolved, err := NewIssue(
		IssueUnresolved, "missing fact", "evidence is ambiguous",
		[]shoal.ID{graphAnchor.ID()})
	if err != nil {
		t.Fatal(err)
	}
	unsupported, err := NewIssue(
		IssueUnsupported, "requested action", "not represented by this contract", nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewInferenceResult(
		pack, []Claim{second, first}, []Issue{unsupported, unresolved},
		testTime.Add(time.Minute), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := result.ValidateFor(pack); err != nil {
		t.Fatal(err)
	}
	if len(result.Claims()) != 2 ||
		shoal.CompareID(result.Claims()[0].ID(), result.Claims()[1].ID()) >= 0 {
		t.Fatal("claims are not canonically ordered")
	}
	if len(result.Unresolved()) != 1 || len(result.Unsupported()) != 1 {
		t.Fatal("result did not classify unresolved and unsupported entries")
	}

	_, err = NewClaim(
		"subject", "predicate", object, shoal.Score(math.NaN()),
		[]shoal.ID{documentAnchor.ID()}, ClaimInferred, model, prompt, nil)
	assertInvalid(t, err)
	_, err = NewClaim(
		"subject", "predicate", object, 0.5,
		[]shoal.ID{documentAnchor.ID(), documentAnchor.ID()},
		ClaimInferred, model, prompt, nil)
	assertInvalid(t, err)
	largeObject, err := ontology.NewStringValue(strings.Repeat("v", MaxClaimValueBytes+1))
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewClaim(
		"subject", "predicate", largeObject, 0.5,
		[]shoal.ID{documentAnchor.ID()},
		ClaimInferred, model, prompt, nil)
	assertInvalid(t, err)
	_, err = NewInferenceResult(pack, []Claim{first, first}, nil, testTime, nil)
	assertInvalid(t, err)
	_, err = NewInferenceResult(pack, nil, nil, testTime, nil)
	assertInvalid(t, err)
	tooManyClaims := make([]Claim, MaxClaims+1)
	for index := range tooManyClaims {
		tooManyClaims[index] = first
	}
	_, err = NewInferenceResult(pack, tooManyClaims, nil, testTime, nil)
	assertInvalid(t, err)

	outside := mustDocumentAnchorAt(t, "outside", 30)
	outsideClaim, err := NewClaim(
		"subject", "predicate", object, 0.5,
		[]shoal.ID{outside.ID()}, ClaimInferred, model, prompt, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewInferenceResult(pack, []Claim{outsideClaim}, nil, testTime, nil)
	assertInvalid(t, err)
}

func TestExtendedInferenceResultRetainsVerifiedEvidence(t *testing.T) {
	baseAnchor := mustDocumentAnchor(t, "base")
	addition := mustDocumentAnchorAt(t, "added", 10)
	snapshot, auth := mustPins(t)
	pack, err := NewContextPack("query", []EvidenceAnchor{baseAnchor}, nil, snapshot, auth, nil)
	if err != nil {
		t.Fatal(err)
	}
	model, prompt := mustProvenance(t)
	value, _ := ontology.NewStringValue("value")
	claim, err := NewClaim(
		"subject", "predicate", value, 1, []shoal.ID{addition.ID()},
		ClaimInferred, model, prompt, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewExtendedInferenceResult(
		pack, []EvidenceAnchor{addition}, []Claim{claim}, nil, testTime, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := result.ValidateFor(pack); err != nil {
		t.Fatal(err)
	}
	got := result.EvidenceAdditions()
	got[0] = baseAnchor
	if result.EvidenceAdditions()[0].ID() != addition.ID() {
		t.Fatal("evidence additions were not defensively copied")
	}
	if _, err := NewExtendedInferenceResult(
		pack, []EvidenceAnchor{baseAnchor}, []Claim{claim}, nil, testTime, nil,
	); err == nil {
		t.Fatal("overlapping evidence addition accepted")
	}
}

func TestMutationLeaksArePrevented(t *testing.T) {
	path := testPath()
	graphAnchor, err := NewGraphAnchor(path)
	if err != nil {
		t.Fatal(err)
	}
	path.Nodes[0].Labels[0] = "mutated"
	path.Nodes[0].Properties["key"] = "mutated"
	returnedPath, _ := graphAnchor.Path()
	returnedPath.Nodes[0].Labels[0] = "returned mutation"
	returnedPath.Nodes[0].Properties["key"] = "returned mutation"
	if err := graphAnchor.Validate(); err != nil {
		t.Fatalf("anchor leaked mutable path: %v", err)
	}

	snapshot, auth := mustPins(t)
	metadata := shoal.Metadata{"tenant": "original"}
	input := []EvidenceAnchor{graphAnchor}
	pack, err := NewContextPack("query", input, nil, snapshot, auth, metadata)
	if err != nil {
		t.Fatal(err)
	}
	input[0] = EvidenceAnchor{}
	metadata["tenant"] = "mutated"
	returned := pack.Evidence()
	returned[0] = EvidenceAnchor{}
	returnedMetadata := pack.Metadata()
	returnedMetadata["tenant"] = "returned mutation"
	if err := pack.Validate(); err != nil {
		t.Fatalf("context pack leaked mutable input: %v", err)
	}

	modelParameters := shoal.Metadata{"temperature": "0"}
	seed := int64(7)
	model, err := NewModelProvenance("provider", "model", "v1", modelParameters, &seed)
	if err != nil {
		t.Fatal(err)
	}
	modelParameters["temperature"] = "1"
	returnedParameters := model.Parameters()
	returnedParameters["temperature"] = "2"
	if model.Parameters()["temperature"] != "0" {
		t.Fatal("model provenance leaked parameters")
	}

	prompt, err := NewPromptProvenance(
		"template", "v1", "sha256:"+strings.Repeat("0", 64))
	if err != nil {
		t.Fatal(err)
	}
	value, err := ontology.NewStringValue("value")
	if err != nil {
		t.Fatal(err)
	}
	claim, err := NewClaim(
		"subject", "predicate", value, 1, []shoal.ID{graphAnchor.ID()},
		ClaimObserved, model, prompt, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewInferenceResult(
		pack, []Claim{claim}, nil, testTime.Add(time.Minute),
		shoal.Metadata{"request": "original"})
	if err != nil {
		t.Fatal(err)
	}
	claims := result.Claims()
	claims[0] = Claim{}
	evidenceIDs := claim.EvidenceIDs()
	evidenceIDs[0] = "mutated"
	resultMetadata := result.Metadata()
	resultMetadata["request"] = "mutated"
	if err := result.ValidateFor(pack); err != nil {
		t.Fatalf("inference result leaked mutable output: %v", err)
	}
}

func TestPinsProvenanceAndUTF8Validation(t *testing.T) {
	_, err := NewSnapshotPin("", testTime)
	assertInvalid(t, err)
	_, err = NewAuthPin("auth", time.Time{})
	assertInvalid(t, err)

	snapshot, err := NewSnapshotPin("snapshot", testTime)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := NewAuthPin("auth", testTime.Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewContextPack(
		"query", []EvidenceAnchor{mustGraphAnchor(t)}, nil, snapshot, auth, nil)
	assertInvalid(t, err)

	_, err = NewModelProvenance(
		string([]byte{0xff}), "model", "", nil, nil)
	assertInvalid(t, err)
	_, err = NewPromptProvenance(
		"template", "v1", string([]byte{0xff}))
	assertInvalid(t, err)
	_, err = NewPromptProvenance("template", "v1", "raw prompt")
	assertInvalid(t, err)
	_, err = NewIssue(
		IssueUnresolved, strings.Repeat("x", MaxIssueInputBytes+1), "reason", nil)
	assertInvalid(t, err)
}

func TestAggregateByteBounds(t *testing.T) {
	quote := strings.Repeat("q", MaxQuoteBytes)
	anchors := make([]EvidenceAnchor, 128)
	for index := range anchors {
		anchors[index] = mustDocumentAnchorAt(
			t, quote, int64(index)*(MaxQuoteBytes+1))
		anchors[index].citation.DocumentID = shoal.ID(fmt.Sprintf("document-%03d", index))
		id, err := anchorID(anchors[index])
		if err != nil {
			t.Fatal(err)
		}
		anchors[index].id = id
	}
	snapshot, auth := mustPins(t)
	if _, err := NewContextPack(
		"query", anchors[:127], nil, snapshot, auth, nil,
	); err != nil {
		t.Fatalf("near-limit context pack rejected: %v", err)
	}
	if _, err := NewContextPack(
		"query", anchors, nil, snapshot, auth, nil,
	); err == nil {
		t.Fatal("over-limit context pack accepted")
	}

	pack, err := NewContextPack(
		"query", []EvidenceAnchor{anchors[0]}, nil, snapshot, auth, nil)
	if err != nil {
		t.Fatal(err)
	}
	inputSuffix := strings.Repeat("i", MaxIssueInputBytes-8)
	reason := strings.Repeat("r", MaxIssueReasonBytes)
	issues := make([]Issue, 205)
	for index := range issues {
		issues[index], err = NewIssue(
			IssueUnsupported,
			fmt.Sprintf("%07d%s", index, inputSuffix),
			reason,
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := NewInferenceResult(
		pack, nil, issues[:200], testTime, nil,
	); err != nil {
		t.Fatalf("near-limit inference result rejected: %v", err)
	}
	if _, err := NewInferenceResult(
		pack, nil, issues, testTime, nil,
	); err == nil {
		t.Fatal("over-limit inference result accepted")
	}
}

type fakeGenerator struct {
	generate func(context.Context, ContextPack) (InferenceResult, error)
}

func (f fakeGenerator) Generate(
	ctx context.Context, pack ContextPack,
) (InferenceResult, error) {
	return f.generate(ctx, pack)
}

var _ Generator = fakeGenerator{}

func TestGeneratorContractWithFake(t *testing.T) {
	anchor := mustDocumentAnchor(t, "alpha")
	snapshot, auth := mustPins(t)
	pack, err := NewContextPack(
		"query", []EvidenceAnchor{anchor}, nil, snapshot, auth, nil)
	if err != nil {
		t.Fatal(err)
	}
	model, prompt := mustProvenance(t)
	value, err := ontology.NewStringValue("answer")
	if err != nil {
		t.Fatal(err)
	}
	generator := fakeGenerator{generate: func(
		ctx context.Context, received ContextPack,
	) (InferenceResult, error) {
		if err := ctx.Err(); err != nil {
			return InferenceResult{}, err
		}
		claim, err := NewClaim(
			"subject", "predicate", value, 0.9,
			[]shoal.ID{received.Evidence()[0].ID()},
			ClaimInferred, model, prompt, nil)
		if err != nil {
			return InferenceResult{}, err
		}
		return NewInferenceResult(received, []Claim{claim}, nil, testTime, nil)
	}}
	result, err := generator.Generate(context.Background(), pack)
	if err != nil {
		t.Fatal(err)
	}
	if err := result.ValidateFor(pack); err != nil {
		t.Fatal(err)
	}
}

func TestStableIDGoldenVectors(t *testing.T) {
	documentAnchor := mustDocumentAnchor(t, "alpha")
	graphAnchor := mustGraphAnchor(t)
	snapshot, auth := mustPins(t)
	pack, err := NewContextPack(
		"golden query",
		[]EvidenceAnchor{graphAnchor, documentAnchor},
		nil,
		snapshot,
		auth,
		shoal.Metadata{"tenant": "golden"},
	)
	if err != nil {
		t.Fatal(err)
	}
	model, prompt := mustProvenance(t)
	value, err := ontology.NewStringValue("golden value")
	if err != nil {
		t.Fatal(err)
	}
	claim, err := NewClaim(
		"golden-subject",
		"golden-predicate",
		value,
		0.875,
		[]shoal.ID{graphAnchor.ID(), documentAnchor.ID()},
		ClaimInferred,
		model,
		prompt,
		shoal.Metadata{"source": "golden"},
	)
	if err != nil {
		t.Fatal(err)
	}
	unresolved, err := NewIssue(
		IssueUnresolved,
		"golden unresolved",
		"insufficient evidence",
		[]shoal.ID{documentAnchor.ID()},
	)
	if err != nil {
		t.Fatal(err)
	}
	unsupported, err := NewIssue(
		IssueUnsupported,
		"golden unsupported",
		"unsupported operation",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewInferenceResult(
		pack,
		[]Claim{claim},
		[]Issue{unsupported, unresolved},
		testTime.Add(time.Minute),
		shoal.Metadata{"request": "golden"},
	)
	if err != nil {
		t.Fatal(err)
	}

	actual := map[string]shoal.ID{
		"document anchor": documentAnchor.ID(),
		"graph anchor":    graphAnchor.ID(),
		"context pack":    pack.ID(),
		"claim":           claim.ID(),
		"unresolved":      unresolved.ID(),
		"unsupported":     unsupported.ID(),
		"result":          result.ID(),
	}
	expected := map[string]shoal.ID{
		"document anchor": "evidence-anchor:c6399bc8a9e324ee23192615e98a68821f137d3665a5439f58d2d1c8e4e55678",
		"graph anchor":    "evidence-anchor:75f03143bbb81fe5dacd7d829adf446113422f972c91c587a05f7d2b1e7a4616",
		"context pack":    "context-pack:1cb209ea14257fa30268f8ba7a2ebf7dac373f7a148bae62ce151cfe05bd6776",
		"claim":           "claim:cd447ab969ed650e6a80e5a1e821112496a8ae303301f1710b5e6c4bf248d59a",
		"unresolved":      "inference-issue:76ff4d15a9f207d3a572d5c551f87bb2c0ddd6c3bcc4f1c66da391efb7e9fb1f",
		"unsupported":     "inference-issue:051599545ba0e3112e95a925f11fe4f9e88f52f0dd7c0fbc098b4a8b748cf33e",
		"result":          "inference-result:f62c64ef24d290e26ac21d2077bf4e6ce93edc4b63d93057a92882e7bc46d969",
	}
	for name, want := range expected {
		if got := actual[name]; got != want {
			t.Errorf("%s ID = %q, want %q", name, got, want)
		}
	}
}

func FuzzDocumentAnchorRejectsCorruptQuotes(f *testing.F) {
	f.Add("alpha")
	f.Add(string([]byte{0xff}))
	f.Fuzz(func(t *testing.T, quote string) {
		citation := testCitation(quote)
		anchor, err := NewDocumentAnchor(citation, quote)
		if utf8ValidNonemptyWithinBound(quote) {
			if err != nil {
				t.Fatalf("valid-shaped quote rejected: %v", err)
			}
			if err := anchor.Validate(); err != nil {
				t.Fatalf("constructed anchor invalid: %v", err)
			}
			return
		}
		if err == nil {
			t.Fatal("invalid quote accepted")
		}
	})
}

func mustDocumentAnchor(t *testing.T, quote string) EvidenceAnchor {
	t.Helper()
	return mustDocumentAnchorAt(t, quote, 0)
}

func mustDocumentAnchorAt(t *testing.T, quote string, start int64) EvidenceAnchor {
	t.Helper()
	citation := testCitationAt(quote, start)
	anchor, err := NewDocumentAnchor(citation, quote)
	if err != nil {
		t.Fatal(err)
	}
	return anchor
}

func testCitation(quote string) document.Citation {
	return testCitationAt(quote, 0)
}

func testCitationAt(quote string, start int64) document.Citation {
	return document.Citation{
		DocumentID: "document",
		RevisionID: "revision",
		SectionID:  "section",
		SpanID:     "span",
		Range: document.SourceRange{
			Start: document.SourcePosition{Offset: start},
			End:   document.SourcePosition{Offset: start + int64(len(quote))},
		},
	}
}

func mustGraphAnchor(t *testing.T) EvidenceAnchor {
	t.Helper()
	anchor, err := NewGraphAnchor(testPath())
	if err != nil {
		t.Fatal(err)
	}
	return anchor
}

func testPath() graph.Path {
	return graph.Path{
		Nodes: []graph.Node{
			{
				ID: "node-a", Kind: "entity",
				Labels:     []string{"person"},
				Properties: shoal.Metadata{"key": "value"},
			},
			{ID: "node-b", Kind: "entity", Labels: []string{"place"}},
		},
		Edges: []graph.Edge{{
			ID: "edge", From: "node-a", To: "node-b",
			Type: "located_in", Weight: 1,
		}},
	}
}

func mustPins(t *testing.T) (SnapshotPin, AuthPin) {
	t.Helper()
	snapshot, err := NewSnapshotPin("snapshot-1", testTime)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := NewAuthPin("auth-sha256:test", testTime.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return snapshot, auth
}

func mustProvenance(t *testing.T) (ModelProvenance, PromptProvenance) {
	t.Helper()
	seed := int64(7)
	model, err := NewModelProvenance(
		"provider", "model", "v1",
		shoal.Metadata{"temperature": "0"}, &seed)
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := NewPromptProvenance(
		"grounded-claim", "v1", "sha256:"+strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	return model, prompt
}

func assertInvalid(t *testing.T, err error) {
	t.Helper()
	if err == nil || !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("error = %v, want invalid_argument", err)
	}
}

func utf8ValidNonemptyWithinBound(value string) bool {
	return len(value) > 0 && len(value) <= MaxQuoteBytes &&
		utf8.ValidString(value)
}
