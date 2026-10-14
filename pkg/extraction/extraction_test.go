package extraction

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/model"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type scriptedGenerator struct {
	text string
	err  error
	req  model.GenerateRequest
}

func (g *scriptedGenerator) Generate(_ context.Context, request model.GenerateRequest) (model.GenerateResult, error) {
	g.req = request
	if g.err != nil {
		return model.GenerateResult{}, g.err
	}
	return model.GenerateResult{
		Text:       g.text,
		Provenance: model.Provenance{Provider: "test-provider", Model: "test-model"},
	}, nil
}

type fixture struct {
	request        Request
	person         ontology.ConceptDefinition
	organization   ontology.ConceptDefinition
	name           ontology.PropertyDefinition
	age            ontology.PropertyDefinition
	worksAt        ontology.RelationshipDefinition
	associatedWith ontology.RelationshipDefinition
	thing          ontology.ConceptDefinition
	anchor         inference.EvidenceAnchor
	graphAnchor    inference.EvidenceAnchor
	opaqueAnchor   inference.EvidenceAnchor
	opaqueNodeID   shoal.ID
}

func TestExtractValidMultiEntityRelationAndStableRetry(t *testing.T) {
	f := newFixture(t)
	output := validOutput(f)
	first := extractWith(t, f.request, output)
	second := extractWith(t, f.request, output)

	plan := first.PublicationPlan()
	if len(plan.Entities) != 2 || len(plan.Edges) != 1 || len(plan.ClaimIDs) != 6 {
		t.Fatalf("unexpected plan sizes: %+v", plan)
	}
	if plan.State != StateProposed || plan.Origin != OriginInferred {
		t.Fatalf("publication state/origin = %q/%q", plan.State, plan.Origin)
	}
	for _, entity := range plan.Entities {
		if entity.State != StateProposed || entity.Origin != OriginInferred ||
			len(entity.EvidenceIDs) != 1 || entity.EvidenceIDs[0] != f.anchor.ID() {
			t.Fatalf("invalid entity grounding: %+v", entity)
		}
	}
	if plan.Edges[0].EvidenceIDs[0] != f.anchor.ID() {
		t.Fatalf("edge evidence = %v", plan.Edges[0].EvidenceIDs)
	}
	if err := first.OntologyResult().Validate(); err != nil {
		t.Fatalf("ontology result: %v", err)
	}
	if err := first.InferenceResult().ValidateFor(f.request.Context); err != nil {
		t.Fatalf("inference result: %v", err)
	}
	for _, claim := range first.InferenceResult().Claims() {
		if ids := claim.EvidenceIDs(); len(ids) != 1 || ids[0] != f.anchor.ID() {
			t.Fatalf("claim evidence = %v", ids)
		}
	}
	again := second.PublicationPlan()
	for i := range plan.Entities {
		if plan.Entities[i].ID != again.Entities[i].ID {
			t.Fatal("entity IDs changed across retry")
		}
	}
	if plan.Edges[0].ID != again.Edges[0].ID {
		t.Fatal("edge ID changed across retry")
	}
	for i := range plan.ClaimIDs {
		if plan.ClaimIDs[i] != again.ClaimIDs[i] {
			t.Fatal("claim IDs changed across retry")
		}
	}
}

func TestPromptDeterministicAndOnlyContainsCitedContent(t *testing.T) {
	f := newFixture(t)
	first, err := BuildPrompt(f.request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := BuildPrompt(f.request)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("prompt is not deterministic")
	}
	if strings.Contains(first, "UNCITED-SECRET") {
		t.Fatal("prompt leaked the context query")
	}
	if !strings.Contains(first, "Alice works at Acme.") ||
		!strings.Contains(first, string(f.anchor.ID())) {
		t.Fatal("prompt omitted exact cited evidence")
	}
}

func TestStrictParsingAndBounds(t *testing.T) {
	f := newFixture(t)
	tests := map[string]string{
		"unknown field":   `{"entities":[],"relations":[],"extra":true}`,
		"nested unknown":  `{"entities":[{"key":"alice","type_id":"` + string(f.person.ID()) + `","properties":[],"confidence":1,"evidence_anchor_ids":["` + string(f.anchor.ID()) + `"],"extra":true}],"relations":[]}`,
		"malformed":       `{"entities":[`,
		"trailing":        `{"entities":[],"relations":[]} {}`,
		"duplicate field": `{"entities":[],"entities":[],"relations":[]}`,
		"case alias":      `{"Entities":[],"relations":[]}`,
		"non finite":      `{"entities":[{"key":"alice","type_id":"` + string(f.person.ID()) + `","properties":[],"confidence":1e999,"evidence_anchor_ids":["` + string(f.anchor.ID()) + `"]}],"relations":[]}`,
	}
	for name, output := range tests {
		t.Run(name, func(t *testing.T) {
			assertExtractError(t, f.request, output)
		})
	}
	limited := f.request
	limited.Limits = DefaultLimits()
	limited.Limits.MaxOutputBytes = 8
	assertExtractError(t, limited, validOutput(f))

	deep := `{"entities":[],"relations":` + strings.Repeat("[", 30) + strings.Repeat("]", 30) + `}`
	assertExtractError(t, f.request, deep)
}

func TestOntologyAndGroundingFailures(t *testing.T) {
	f := newFixture(t)
	base := validOutput(f)
	tests := map[string]string{
		"unknown type":          strings.Replace(base, string(f.person.ID()), "concept:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 1),
		"invalid property type": strings.Replace(base, `{"type":"integer","value":42}`, `{"type":"string","value":"42"}`, 1),
		"cardinality": strings.Replace(base,
			`{"property_id":"`+string(f.name.ID())+`","value":{"type":"string","value":"Alice"}}`,
			`{"property_id":"`+string(f.name.ID())+`","value":{"type":"string","value":"Alice"}},{"property_id":"`+string(f.name.ID())+`","value":{"type":"string","value":"Alicia"}}`, 1),
		"hallucinated anchor":      strings.Replace(base, string(f.anchor.ID()), "evidence-anchor:missing", 1),
		"unsupported graph anchor": strings.Replace(base, string(f.anchor.ID()), string(f.graphAnchor.ID()), 1),
		"hallucinated node":        strings.Replace(base, `"existing_node_id":""`, `"existing_node_id":"node:missing"`, 1),
		"omitted node anchor":      strings.Replace(base, `"existing_node_id":""`, `"existing_node_id":"`+graphNodeToken("node-1")+`"`, 1),
		"cross domain":             strings.Replace(base, `"from_entity_key":"alice","to_entity_key":"acme"`, `"from_entity_key":"acme","to_entity_key":"alice"`, 1),
		"invalid edge":             strings.Replace(base, `"to_entity_key":"acme"`, `"to_entity_key":"missing"`, 1),
		"uppercase key":            strings.Replace(base, `"key":"alice"`, `"key":"Alice"`, 1),
	}
	for name, output := range tests {
		t.Run(name, func(t *testing.T) { assertExtractError(t, f.request, output) })
	}
}

func TestDuplicateCanonicalKeysAndCanonicalOrdering(t *testing.T) {
	f := newFixture(t)
	duplicate := strings.Replace(validOutput(f), `"key":"acme"`, `"key":"alice"`, 1)
	assertExtractError(t, f.request, duplicate)

	duplicateNode := strings.ReplaceAll(
		validOutput(f),
		`"existing_node_id":""`,
		`"existing_node_id":"`+graphNodeToken("node-1")+`"`,
	)
	duplicateNode = strings.ReplaceAll(
		duplicateNode,
		`"evidence_anchor_ids":["`+string(f.anchor.ID())+`"]`,
		`"evidence_anchor_ids":["`+string(f.anchor.ID())+`","`+string(f.graphAnchor.ID())+`"]`,
	)
	assertExtractError(t, f.request, duplicateNode)

	result := extractWith(t, f.request, validOutput(f))
	plan := result.PublicationPlan()
	if string(plan.Entities[0].ID) >= string(plan.Entities[1].ID) {
		t.Fatal("entities are not canonically ordered")
	}
}

func TestOntologyProvenanceUsesPromptTemplateAndHashMetadata(t *testing.T) {
	f := newFixture(t)
	result := extractWith(t, f.request, validOutput(f))
	provenance := result.OntologyResult().Provenance()
	if provenance.Prompt() != PromptTemplateID {
		t.Fatalf("ontology prompt = %q", provenance.Prompt())
	}
	hash := provenance.Metadata()["prompt_hash"]
	if !strings.HasPrefix(hash, "sha256:") {
		t.Fatalf("ontology prompt hash metadata = %#v", provenance.Metadata()["prompt_hash"])
	}
}

// TestOntologyAssertionsRecordThePinnedOntologyVersion covers issue #279: the
// request pins a version, and every assertion the pipeline emits must record
// it, so the version is recoverable from the assertion rather than only
// through the result -> request chain.
func TestOntologyAssertionsRecordThePinnedOntologyVersion(t *testing.T) {
	f := newFixture(t)
	identity, err := ontology.NewOntologyIdentity(f.request.Version)
	if err != nil {
		t.Fatal(err)
	}
	// The heuristic extractor emits entities and no assertions, so the model
	// path is the only one with an ontology identity to record.
	for name, result := range map[string]Result{
		"model": extractWith(t, f.request, validOutput(f)),
	} {
		ontologyResult := result.OntologyResult()
		assertions := ontologyResult.Assertions()
		if len(assertions) == 0 {
			t.Fatalf("%s: extraction produced no assertions", name)
		}
		for _, assertion := range assertions {
			recorded, known := assertion.Ontology()
			if !known {
				t.Fatalf("%s: assertion %s recorded no ontology version",
					name, assertion.ID())
			}
			if recorded != identity {
				t.Fatalf("%s: assertion %s recorded %v, want %v",
					name, assertion.ID(), recorded, identity)
			}
			if reading := assertion.ReadUnder(identity); reading != ontology.OntologySameVersion {
				t.Fatalf("%s: assertion %s reads as %q under the pinned version",
					name, assertion.ID(), reading)
			}
		}
		if unresolved := ontologyResult.UnresolvedOntologyAssertions(); len(unresolved) != 0 {
			t.Fatalf("%s: unresolved assertions %v", name, unresolved)
		}
	}
}

func TestProviderFailureHasNoSilentFallback(t *testing.T) {
	f := newFixture(t)
	for _, providerErr := range []error{model.ErrUnavailable, model.ErrTimeout} {
		generator := &scriptedGenerator{err: providerErr}
		_, err := (Orchestrator{Generator: generator}).Extract(context.Background(), f.request)
		if !errors.Is(err, providerErr) {
			t.Fatalf("error %v does not preserve %v", err, providerErr)
		}
	}
}

func TestHeuristicIsExplicitAndMutationIsolated(t *testing.T) {
	f := newFixture(t)
	result, err := (HeuristicExtractor{ConceptType: f.thing.ID()}).Extract(context.Background(), f.request)
	if err != nil {
		t.Fatal(err)
	}

	plan := result.PublicationPlan()
	if plan.Provenance.Provider != HeuristicProvider || plan.Provenance.Model != HeuristicModel {
		t.Fatalf("heuristic provenance = %+v", plan.Provenance)
	}
	originalID := plan.Entities[0].ID
	plan.Entities[0].ID = "mutated"
	plan.Entities[0].EvidenceIDs[0] = "mutated"
	again := result.PublicationPlan()
	if again.Entities[0].ID != originalID || again.Entities[0].EvidenceIDs[0] != f.anchor.ID() {
		t.Fatal("publication plan aliases caller mutation")
	}
}

func TestPropertylessEntityProducesGroundedTypeClaim(t *testing.T) {
	f := newFixture(t)
	output := `{"entities":[{"key":"event","type_id":"` + string(f.thing.ID()) +
		`","properties":[],"confidence":0.8,"evidence_anchor_ids":["` +
		string(f.anchor.ID()) + `"]}],"relations":[]}`
	result := extractWith(t, f.request, output)
	if len(result.PublicationPlan().Entities) != 1 ||
		len(result.OntologyResult().Assertions()) != 0 ||
		len(result.InferenceResult().Claims()) != 1 {
		t.Fatal("propertyless entity did not produce a type outcome")
	}
}

func TestUndirectedRelationNormalizesWholePayload(t *testing.T) {
	f := newFixture(t)
	forward := strings.Replace(validOutput(f), string(f.worksAt.ID()), string(f.associatedWith.ID()), 1)
	reverse := strings.Replace(forward,
		`"from_entity_key":"alice","to_entity_key":"acme"`,
		`"from_entity_key":"acme","to_entity_key":"alice"`, 1)
	first := extractWith(t, f.request, forward).PublicationPlan()
	second := extractWith(t, f.request, reverse).PublicationPlan()
	if first.Edges[0].ID != second.Edges[0].ID ||
		first.Edges[0].From != second.Edges[0].From ||
		first.Edges[0].To != second.Edges[0].To {
		t.Fatal("undirected edge payload is not canonical")
	}
	for i := range first.ClaimIDs {
		if first.ClaimIDs[i] != second.ClaimIDs[i] {
			t.Fatal("undirected claim IDs changed with endpoint order")
		}
	}
}

func TestEntityIdentityIgnoresPromptScopeForResolution(t *testing.T) {
	f := newFixture(t)
	first := extractWith(t, f.request, validOutput(f)).PublicationPlan()
	changed := f.request
	changed.Instructions = "Extract only explicitly named work relationships."
	second := extractWith(t, changed, validOutput(f)).PublicationPlan()
	if first.Entities[0].ID != second.Entities[0].ID {
		t.Fatalf("entity identity changed across prompt scopes: %s != %s",
			first.Entities[0].ID, second.Entities[0].ID)
	}
}

func TestExistingNodeRequiresAndRetainsGroundingAnchor(t *testing.T) {
	f := newFixture(t)
	output := strings.Replace(validOutput(f), `"existing_node_id":""`, `"existing_node_id":"`+graphNodeToken("node-1")+`"`, 1)
	output = strings.Replace(output,
		`"evidence_anchor_ids":["`+string(f.anchor.ID())+`"]`,
		`"evidence_anchor_ids":["`+string(f.anchor.ID())+`","`+string(f.graphAnchor.ID())+`"]`, 1)
	plan := extractWith(t, f.request, output).PublicationPlan()
	var found bool
	for _, entity := range plan.Entities {
		if entity.ID == "node-1" {
			found = entity.Action == ActionReference && len(entity.EvidenceIDs) == 2
		}
	}
	if !found {
		t.Fatal("existing node did not retain its graph grounding")
	}
}

func TestOpaqueGraphNodeIDRoundTripsThroughPromptToken(t *testing.T) {
	f := newFixture(t)
	token := graphNodeToken(f.opaqueNodeID)
	if !strings.Contains(mustPrompt(t, f.request), token) {
		t.Fatal("prompt omitted reversible opaque node token")
	}
	output := strings.Replace(validOutput(f), `"existing_node_id":""`, `"existing_node_id":"`+token+`"`, 1)
	output = strings.Replace(output,
		`"evidence_anchor_ids":["`+string(f.anchor.ID())+`"]`,
		`"evidence_anchor_ids":["`+string(f.anchor.ID())+`","`+string(f.opaqueAnchor.ID())+`"]`, 1)
	plan := extractWith(t, f.request, output).PublicationPlan()
	for _, entity := range plan.Entities {
		if entity.ID == f.opaqueNodeID {
			if entity.ContractID == entity.ID {
				t.Fatal("opaque node did not receive a transport-safe contract ID")
			}
			return
		}
	}
	t.Fatal("opaque graph node ID did not round trip")
}

func TestOntologyEvidenceLimitTracksContextPack(t *testing.T) {
	f := newFixture(t)
	anchors := make([]inference.EvidenceAnchor, 257)
	for i := range anchors {
		quote := "x"
		citation := document.Citation{
			DocumentID: shoal.ID("doc-" + strconv.Itoa(i)),
			RevisionID: "revision", SpanID: "span",
			Range: document.SourceRange{
				Start: document.SourcePosition{Offset: int64(i)},
				End:   document.SourcePosition{Offset: int64(i + 1)},
			},
		}
		anchor, err := inference.NewDocumentAnchor(citation, quote)
		if err != nil {
			t.Fatal(err)
		}
		anchors[i] = anchor
	}
	identity, _ := f.request.Context.Ontology()
	pack, err := inference.NewContextPack(
		"bounded evidence", anchors, &identity, f.request.Context.Snapshot(),
		f.request.Context.Authorization(), nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	request := f.request
	request.Context = pack
	output := strings.Replace(validOutput(f), string(f.anchor.ID()), string(pack.Evidence()[0].ID()), -1)
	if _, err := (Orchestrator{Generator: &scriptedGenerator{text: output}}).Extract(context.Background(), request); err != nil {
		t.Fatalf("257-anchor request failed after model generation: %v", err)
	}
}

func TestHeuristicHonorsEntityLimit(t *testing.T) {
	f := newFixture(t)
	f.request.Limits.MaxEntities = 1
	if _, err := (HeuristicExtractor{ConceptType: f.thing.ID()}).Extract(context.Background(), f.request); err == nil {
		t.Fatal("expected heuristic entity limit failure")
	}
}

func TestIntegrationFakeModelRealContracts(t *testing.T) {
	f := newFixture(t)
	generator := &scriptedGenerator{text: validOutput(f)}
	result, err := (Orchestrator{Generator: generator}).Extract(context.Background(), f.request)
	if err != nil {
		t.Fatal(err)
	}
	if generator.req.Prompt != result.Prompt() || generator.req.MaxOutputTokens != DefaultLimits().MaxOutputTokens {
		t.Fatalf("unexpected model request: %+v", generator.req)
	}
	if len(result.OntologyResult().Assertions()) != 4 ||
		len(result.InferenceResult().Claims()) != 6 {
		t.Fatal("real contracts did not receive all extracted claims")
	}
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	required, err := ontology.NewFlagConstraint(ontology.ConstraintRequired)
	if err != nil {
		t.Fatal(err)
	}
	maxOne, err := ontology.NewCountConstraint(ontology.ConstraintMaximumCount, 1)
	if err != nil {
		t.Fatal(err)
	}
	minAge, err := ontology.NewValueConstraint(ontology.ConstraintMinimumValue, ontology.NewIntegerValue(0))
	if err != nil {
		t.Fatal(err)
	}
	name, err := ontology.NewPropertyDefinition("name", "Name", "", ontology.ValueString, []ontology.Constraint{required, maxOne}, nil)
	if err != nil {
		t.Fatal(err)
	}
	age, err := ontology.NewPropertyDefinition("age", "Age", "", ontology.ValueInteger, []ontology.Constraint{maxOne, minAge}, nil)
	if err != nil {
		t.Fatal(err)
	}
	person, err := ontology.NewConceptDefinition("person", "Person", "", []shoal.ID{name.ID(), age.ID()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	organization, err := ontology.NewConceptDefinition("organization", "Organization", "", []shoal.ID{name.ID()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	thing, err := ontology.NewConceptDefinition("thing", "Thing", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	worksAt, err := ontology.NewRelationshipDefinition("works_at", "Works at", "", []shoal.ID{person.ID()}, []shoal.ID{organization.ID()}, nil, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	associatedWith, err := ontology.NewRelationshipDefinition("associated_with", "Associated with", "", []shoal.ID{person.ID()}, []shoal.ID{organization.ID()}, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	schema, err := ontology.NewOntologySchema("work", "Work", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	version, err := ontology.NewOntologyVersion(schema, "1", time.Unix(10, 0), []ontology.ConceptDefinition{organization, person, thing}, []ontology.RelationshipDefinition{worksAt, associatedWith}, []ontology.PropertyDefinition{age, name}, nil)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := inference.NewOntologyIdentity(version)
	if err != nil {
		t.Fatal(err)
	}
	quote := "Alice works at Acme."
	citation := document.Citation{
		DocumentID: "doc-1", RevisionID: "rev-1", SpanID: "span-1",
		Range: document.SourceRange{Start: document.SourcePosition{Offset: 0}, End: document.SourcePosition{Offset: int64(len(quote))}},
	}
	anchor, err := inference.NewDocumentAnchor(citation, quote)
	if err != nil {
		t.Fatal(err)
	}
	graphAnchor, err := inference.NewGraphAnchor(graph.Path{Nodes: []graph.Node{{ID: "node-1", Kind: "event"}}})
	if err != nil {
		t.Fatal(err)
	}
	opaqueNodeID := shoal.ID(string([]byte{0xff, 0x00, 'x'}))
	opaqueAnchor, err := inference.NewGraphAnchor(graph.Path{Nodes: []graph.Node{{ID: opaqueNodeID, Kind: "event"}}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := inference.NewSnapshotPin("snapshot-1", time.Unix(20, 0))
	if err != nil {
		t.Fatal(err)
	}
	auth, err := inference.NewAuthPin("auth-1", time.Unix(30, 0))
	if err != nil {
		t.Fatal(err)
	}
	pack, err := inference.NewContextPack("UNCITED-SECRET", []inference.EvidenceAnchor{graphAnchor, opaqueAnchor, anchor}, &identity, snapshot, auth, nil)
	if err != nil {
		t.Fatal(err)
	}
	return fixture{
		request: Request{Version: version, Context: pack, Instructions: "Extract grounded work relationships.", Limits: DefaultLimits()},
		person:  person, organization: organization, name: name, age: age,
		worksAt: worksAt, associatedWith: associatedWith, thing: thing,
		anchor: anchor, graphAnchor: graphAnchor, opaqueAnchor: opaqueAnchor,
		opaqueNodeID: opaqueNodeID,
	}
}

func validOutput(f fixture) string {
	return `{"entities":[` +
		`{"key":"acme","type_id":"` + string(f.organization.ID()) + `","existing_node_id":"","properties":[{"property_id":"` + string(f.name.ID()) + `","value":{"type":"string","value":"Acme"}}],"confidence":0.91,"evidence_anchor_ids":["` + string(f.anchor.ID()) + `"]},` +
		`{"key":"alice","type_id":"` + string(f.person.ID()) + `","existing_node_id":"","properties":[{"property_id":"` + string(f.name.ID()) + `","value":{"type":"string","value":"Alice"}},{"property_id":"` + string(f.age.ID()) + `","value":{"type":"integer","value":42}}],"confidence":0.95,"evidence_anchor_ids":["` + string(f.anchor.ID()) + `"]}` +
		`],"relations":[{"type_id":"` + string(f.worksAt.ID()) + `","from_entity_key":"alice","to_entity_key":"acme","properties":[],"confidence":0.9,"evidence_anchor_ids":["` + string(f.anchor.ID()) + `"]}]}`
}

func extractWith(t *testing.T, request Request, output string) Result {
	t.Helper()
	result, err := (Orchestrator{Generator: &scriptedGenerator{text: output}}).Extract(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func assertExtractError(t *testing.T, request Request, output string) {
	t.Helper()
	if _, err := (Orchestrator{Generator: &scriptedGenerator{text: output}}).Extract(context.Background(), request); err == nil {
		t.Fatal("expected extraction error")
	}
}

func mustPrompt(t *testing.T, request Request) string {
	t.Helper()
	prompt, err := BuildPrompt(request)
	if err != nil {
		t.Fatal(err)
	}
	return prompt
}
