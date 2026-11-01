package webapi

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/ontology"
)

func discoveryV3Fixture(t *testing.T) ontology.OntologyVersion {
	t.Helper()
	schema, err := ontology.NewOntologySchema("agency-schema", "Agency schema", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	concept, err := ontology.NewConceptDefinition("agency", "Agency", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	base, err := ontology.NewOntologyVersion(schema, "v1", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), []ontology.ConceptDefinition{concept}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return base
}

func v3Stop(t *testing.T, proposal discoveryProposalV3) string {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{"action": "stop", "proposal": proposal})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestDiscoveryV3ProposalMatchesV2Delta(t *testing.T) {
	base := discoveryV3Fixture(t)
	evidence := discoveryModelID("evidence")
	relation := OntologyProposalRelationshipDraft{Key: "spans_function", Name: "Spans function", FromConcepts: []string{"agency"}, ToConcepts: []string{"agency"}, Directed: true}
	proposal := discoveryProposalV3{Rationale: "Agency spans several functions", Changes: discoveryV3Changes{Relationships: []OntologyProposalRelationshipDraft{relation}}, Counterevidence: []string{}, OpenQuestions: []string{}, EvidenceIDs: []string{evidence}}
	adapted, err := discoveryAdaptAction(base, v3Stop(t, proposal), inference.ContextPack{})
	if err != nil {
		t.Fatal(err)
	}
	var stop struct {
		Claims []struct {
			Subject   string `json:"subject"`
			Predicate string `json:"predicate"`
			Object    struct {
				Value string `json:"value"`
			} `json:"object"`
			EvidenceIDs []string `json:"evidence_ids"`
		} `json:"claims"`
	}
	if err := json.Unmarshal([]byte(adapted), &stop); err != nil {
		t.Fatal(err)
	}
	if len(stop.Claims) != 1 || stop.Claims[0].Subject != discoveryModelID(base.ID()) || stop.Claims[0].Predicate != discoveryModelID(OntologyDiscoveryPredicate) || !reflect.DeepEqual(stop.Claims[0].EvidenceIDs, []string{evidence}) {
		t.Fatalf("adapted stop = %s", adapted)
	}
	var v3Candidate discoveryCandidate
	if err := decodeDiscoveryCandidate(stop.Claims[0].Object.Value, &v3Candidate); err != nil {
		t.Fatal(err)
	}
	v2Candidate := discoveryCandidate{Rationale: proposal.Rationale, Changes: &discoveryChanges{Relationships: []OntologyProposalRelationshipDraft{relation}}, Counterevidence: []string{}, OpenQuestions: []string{}}
	v2CandidateJSON, err := json.Marshal(v2Candidate)
	if err != nil {
		t.Fatal(err)
	}
	v2Stop, err := json.Marshal(map[string]any{"action": "stop", "correlation_id": discoveryModelID("v2"), "claims": []any{map[string]any{"subject": discoveryModelID(base.ID()), "predicate": discoveryModelID(OntologyDiscoveryPredicate), "object": map[string]any{"type": "string", "value": string(v2CandidateJSON)}, "confidence": 1, "evidence_ids": []string{evidence}}}})
	if err != nil {
		t.Fatal(err)
	}
	passed, err := discoveryAdaptAction(base, string(v2Stop), inference.ContextPack{})
	if err != nil || passed != string(v2Stop) {
		t.Fatalf("v2 answer changed: %v", err)
	}
	v3Draft, err := discoveryCandidateDraft(base, v3Candidate)
	if err != nil {
		t.Fatal(err)
	}
	v2Draft, err := discoveryCandidateDraft(base, v2Candidate)
	if err != nil {
		t.Fatal(err)
	}
	v3Version, err := discoveryVersionFromDraft(base, v3Draft)
	if err != nil {
		t.Fatal(err)
	}
	v2Version, err := discoveryVersionFromDraft(base, v2Draft)
	if err != nil || v2Version.ID() != v3Version.ID() {
		t.Fatalf("persisted candidate version differs: %v", err)
	}
}

func TestDiscoveryV3RejectsInvalidStops(t *testing.T) {
	base := discoveryV3Fixture(t)
	good := discoveryProposalV3{Rationale: "rationale", EvidenceIDs: []string{discoveryModelID("evidence")}}
	for _, tc := range []struct{ name, text, want string }{
		{"both", `{"action":"stop","proposal":{},"issue":{"input":"x","reason":"y"}}`, "exactly one"},
		{"duplicate evidence", func() string {
			p := good
			p.EvidenceIDs = []string{discoveryModelID("evidence"), discoveryModelID("evidence")}
			return v3Stop(t, p)
		}(), "duplicate evidence"},
		{"unknown remove", func() string {
			p := good
			p.Changes.Remove = []OntologyDefinitionReferenceDraft{{Namespace: "concept", Key: "missing"}}
			return v3Stop(t, p)
		}(), "not in the base"},
		{"unknown concept", func() string {
			p := good
			p.Changes.Relationships = []OntologyProposalRelationshipDraft{{Key: "spans", Name: "Spans", FromConcepts: []string{"agency"}, ToConcepts: []string{"unknown"}, Directed: true}}
			return v3Stop(t, p)
		}(), "invalid definition"},
		{"duplicate field", `{"action":"stop","proposal":{},"proposal":{}}`, "duplicate object keys"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := discoveryAdaptAction(base, tc.text, inference.ContextPack{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestDiscoveryV3SchemaAndLegacyPrompt(t *testing.T) {
	base := discoveryV3Fixture(t)
	request := OntologyDiscoveryRequest{SelectionPolicy: "fixture", SelectionReason: "fixture", PopulationSize: 1}
	legacy, host, err := discoveryPrompt(base, request, nil, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(host) == 0 {
		t.Fatal("v2 lost host context")
	}
	const legacyDigest = "sha256:22cb041b6fdd9bc292937fcc16fb747f5800e0b50c2167136dab99d9b36b78cb"
	if got := discoveryDigest([]byte(legacy)); got != legacyDigest {
		t.Fatalf("v2 prompt digest = %s", got)
	}
	v3, v3Host, err := discoveryPromptV3(base, request, nil, 1<<20)
	if err != nil || !strings.Contains(v3, `"ontology-discovery-v3"`) || strings.Contains(v3, `"candidate_shape"`) || string(v3Host) != string(host) {
		t.Fatalf("v3 prompt = %s err=%v", v3, err)
	}
	prompt := `{"budgets":{"MaxFanout":2},"remaining":{"ModelCalls":2,"Evidence":2},"evidence":[{"id":"` + discoveryModelID("visible") + `"}]}`
	schema, err := discoveryActionSchema(base, prompt)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(schema) || !strings.Contains(string(schema), `"proposal"`) || !strings.Contains(string(schema), `"agency"`) || !strings.Contains(string(schema), discoveryModelID("visible")) || strings.Contains(string(schema), discoveryModelID("hidden")) {
		t.Fatalf("schema misses pinned constraints: %s", schema)
	}
	var root struct {
		OneOf []map[string]any `json:"oneOf"`
	}
	if err := json.Unmarshal(schema, &root); err != nil {
		t.Fatal(err)
	}
	stops := 0
	for _, v := range root.OneOf {
		p := v["properties"].(map[string]any)
		if p["action"].(map[string]any)["const"] == "stop" {
			stops++
		}
	}
	if stops != 2 {
		t.Fatalf("v3 stop variants = %d", stops)
	}
}
