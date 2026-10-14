package agentmem

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/extraction"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/model"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type agentmemGenerator struct {
	output string
	calls  int
}

func (g *agentmemGenerator) Generate(context.Context, model.GenerateRequest) (model.GenerateResult, error) {
	g.calls++
	return model.GenerateResult{
		Text:       g.output,
		Provenance: model.Provenance{Provider: "test", Model: "structured"},
	}, nil
}

type countingLLM struct{ calls int }

func (l *countingLLM) Infer(context.Context, string) (string, error) {
	l.calls++
	return "causal entity", nil
}

func TestPlanEnrichmentPreservesValidatedPublicationPlan(t *testing.T) {
	version, concept, property, pack := agentmemExtractionFixture(t, "Alice")
	generator := &agentmemGenerator{}
	generator.output = `{"entities":[{"key":"alice","type_id":"` + string(concept.ID()) +
		`","properties":[{"property_id":"` + string(property.ID()) +
		`","value":{"type":"string","value":"Alice"}}],"confidence":0.9,"evidence_anchor_ids":["` +
		string(pack.Evidence()[0].ID()) + `"]}],"relations":[]}`
	client, err := New(Config{
		Store:             NewFakeStore(),
		OntologyExtractor: extraction.Orchestrator{Generator: generator},
		OntologyRequestFactory: func(context.Context, string) (extraction.Request, error) {
			return extraction.Request{
				Version: version, Context: pack, Instructions: "Extract entities.",
				Limits: extraction.DefaultLimits(),
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := client.PlanEnrichment(context.Background(), "Alice")
	if err != nil {
		t.Fatal(err)
	}
	if generator.calls != 1 || len(plan.Entities) != 1 ||
		plan.Entities[0].Key != "alice" ||
		plan.Entities[0].TypeID != concept.ID() ||
		plan.Entities[0].State != extraction.StateProposed ||
		len(plan.Entities[0].EvidenceIDs) != 1 {
		t.Fatalf("unexpected structured plan: %+v", plan)
	}
}

func TestNewKeepsLegacyWritesOnHeuristicEnricher(t *testing.T) {
	version, _, _, pack := agentmemExtractionFixture(t, "Alice")
	generator := &agentmemGenerator{output: `{"entities":[],"relations":[]}`}
	factory := func(context.Context, string) (extraction.Request, error) {
		return extraction.Request{
			Version: version, Context: pack, Instructions: "Extract entities.",
			Limits: extraction.DefaultLimits(),
		}, nil
	}
	client, err := New(Config{
		Store: NewFakeStore(), OntologyExtractor: extraction.Orchestrator{Generator: generator},
		OntologyRequestFactory: factory,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := client.cfg.Enricher.(HeuristicEnricher); !ok {
		t.Fatalf("configured enricher type = %T", client.cfg.Enricher)
	}
	if _, err := New(Config{
		Store: NewFakeStore(), OntologyExtractor: extraction.Orchestrator{Generator: generator},
	}); err == nil {
		t.Fatal("expected partial ontology extraction configuration to fail")
	}
}

func TestLegacyConsolidatorDoesNotUseSubstringModelOutput(t *testing.T) {
	llm := &countingLLM{}
	client := &Client{cfg: Config{LLM: llm}}
	if err := NewConsolidator(client, 1).Consolidate(context.Background(), "event"); err != nil {
		t.Fatal(err)
	}

	if llm.calls != 0 {
		t.Fatal("legacy consolidator called the substring-based LLM path")
	}
}

func TestConsolidatorRunPropagatesPublisherFailure(t *testing.T) {
	version, concept, property, pack := agentmemExtractionFixture(t, "Alice")
	generator := &agentmemGenerator{
		output: `{"entities":[{"key":"alice","type_id":"` + string(concept.ID()) +
			`","properties":[{"property_id":"` + string(property.ID()) +
			`","value":{"type":"string","value":"Alice"}}],"confidence":0.9,"evidence_anchor_ids":["` +
			string(pack.Evidence()[0].ID()) + `"]}],"relations":[]}`,
	}
	publishErr := errors.New("publish failed")
	client, err := New(Config{
		Store: NewFakeStore(), OntologyExtractor: extraction.Orchestrator{Generator: generator},
		OntologyRequestFactory: func(context.Context, string) (extraction.Request, error) {
			return extraction.Request{
				Version: version, Context: pack, Instructions: "Extract entities.",
				Limits: extraction.DefaultLimits(),
			}, nil
		},
		ConsolidationPublisher: func(context.Context, extraction.PublicationPlan) error {
			return publishErr
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	consolidator := NewConsolidator(client, 1)
	consolidator.Enqueue("event")
	if err := consolidator.Run(context.Background()); !errors.Is(err, publishErr) {
		t.Fatalf("Run error = %v, want publisher error", err)
	}
}

func agentmemExtractionFixture(t *testing.T, quote string) (
	ontology.OntologyVersion,
	ontology.ConceptDefinition,
	ontology.PropertyDefinition,
	inference.ContextPack,
) {
	t.Helper()
	required, err := ontology.NewFlagConstraint(ontology.ConstraintRequired)
	if err != nil {
		t.Fatal(err)
	}
	property, err := ontology.NewPropertyDefinition("name", "Name", "", ontology.ValueString, []ontology.Constraint{required}, nil)
	if err != nil {
		t.Fatal(err)
	}
	concept, err := ontology.NewConceptDefinition("person", "Person", "", []shoal.ID{property.ID()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	schema, err := ontology.NewOntologySchema("agentmem", "Agent memory", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	version, err := ontology.NewOntologyVersion(schema, "1", time.Unix(1, 0), []ontology.ConceptDefinition{concept}, nil, []ontology.PropertyDefinition{property}, nil)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := inference.NewOntologyIdentity(version)
	if err != nil {
		t.Fatal(err)
	}
	citation := document.Citation{
		DocumentID: "doc", RevisionID: "revision", SpanID: "span",
		Range: document.SourceRange{Start: document.SourcePosition{Offset: 0}, End: document.SourcePosition{Offset: int64(len(quote))}},
	}
	anchor, err := inference.NewDocumentAnchor(citation, quote)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := inference.NewSnapshotPin("snapshot", time.Unix(2, 0))
	if err != nil {
		t.Fatal(err)
	}
	auth, err := inference.NewAuthPin("auth", time.Unix(3, 0))
	if err != nil {
		t.Fatal(err)
	}
	pack, err := inference.NewContextPack("ignored", []inference.EvidenceAnchor{anchor}, &identity, snapshot, auth, nil)
	if err != nil {
		t.Fatal(err)
	}
	return version, concept, property, pack
}
