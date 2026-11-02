// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/phrocker/shoal-oss/pkg/contextpack"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/model"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, output io.Writer) (resultErr error) {
	flags := flag.NewFlagSet("shoal-ontology-discovery-demo", flag.ContinueOnError)
	flags.SetOutput(output)
	dir := flags.String("dir", "", "Required isolated directory for corpus and append-only discovery reports")
	provider := flags.String("provider", "scripted", "scripted (conformance only) or ollama (local model)")
	modelName := flags.String("model", "", "Installed Ollama model name; required with -provider ollama")
	ollamaURL := flags.String("ollama-url", "http://localhost:11434", "Local Ollama endpoint")
	publish := flags.Bool("publish", false, "Explicitly submit, approve, and publish this DEMO proposal after discovery")
	scenario := flags.String("scenario", "rename", "Scripted fixture: rename, additive, abstain, or unchanged")
	numCtx := flags.Int("num-ctx", 0, "Ollama context window in tokens; 0 derives it from the discovery budgets "+
		"(MaxInputTokens/2 + MaxOutputTokens), so no prompt the harness accepts is truncated")
	numGPU := flags.Int("num-gpu", -1, "Ollama layers offloaded to the GPU; -1 lets Ollama decide")
	temperature := flags.Float64("temperature", 0, "Ollama sampling temperature; negative leaves Ollama's default")
	seed := flags.Int("seed", 7, "Ollama sampling seed; negative leaves Ollama's default")
	format := flags.String("format", "", `Ollama output format: "" for v3 schema output, or "json" with -protocol v2`)
	protocol := flags.String("protocol", "", "Discovery protocol v2 or v3; defaults to v2 for scripted and v3 for Ollama")
	ollamaTimeout := flags.Duration("ollama-timeout", 2*time.Minute, "Timeout for one Ollama call")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *dir == "" || flags.NArg() != 0 {
		return fmt.Errorf("-dir must name an isolated local demo directory")
	}
	if *protocol == "" {
		if *provider == "scripted" {
			*protocol = "v2"
		} else {
			*protocol = "v3"
		}
	}
	if *protocol != "v2" && *protocol != "v3" {
		return fmt.Errorf("-protocol must be v2 or v3")
	}
	if *provider == "ollama" && *protocol == "v3" && *format != "" {
		return fmt.Errorf("-format json cannot be combined with the v3 action schema")
	}
	var generator model.TextGenerator
	switch *provider {
	case "scripted":
		switch *scenario {
		case "rename", "additive", "abstain", "unchanged":
		default:
			return fmt.Errorf("unknown scripted scenario %q", *scenario)
		}
		if *modelName != "" {
			return fmt.Errorf("-model is only valid with -provider ollama")
		}
		generator = scriptedModel{scenario: *scenario}
		*modelName = "ontology-conformance-v1"
	case "ollama":
		if *modelName == "" || *scenario != "rename" {
			return fmt.Errorf("Ollama requires -model and does not accept scripted scenarios")
		}
		var err error
		generator, err = model.NewOllamaGenerator(ollamaConfig(*ollamaURL, *modelName, *numCtx, *numGPU,
			*temperature, *seed, *format, *ollamaTimeout))
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown provider %q", *provider)
	}
	demo, err := newDemo(ctx, *dir)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, demo.close()) }()
	request, err := demo.request(ctx)
	if err != nil {
		return err
	}
	provenance, err := inference.NewModelProvenance(*provider, *modelName, "v1", nil, nil)
	if err != nil {
		return err
	}
	report, err := os.OpenFile(filepath.Join(*dir, "discovery-reports.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, report.Close()) }()
	config := webapi.OntologyDiscoveryConfig{
		Generator: generator, Model: provenance, Recorder: &reportRecorder{file: report},
		Evaluator:       compatibilityEvaluator{},
		ProtocolVersion: *protocol,
	}
	result, err := demo.service.DiscoverOntology(demo.ctx, request, config)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(output, "provider=%s status=%s evaluation=%s\n%s\n",
		*provider, result.Audit.Status, result.Audit.Evaluation.Status, result.Audit.Reason); err != nil {
		return err
	}
	if result.Proposal.ID() == "" {
		return nil
	}
	proposal := result.Proposal
	if _, err := fmt.Fprintf(output, "proposal=%s state=%s evidence=%d\n",
		wireID(proposal.ID()), proposal.State(), len(proposal.Evidence())); err != nil {
		return err
	}
	if *provider == "scripted" && !*publish {
		retry, err := demo.service.DiscoverOntology(demo.ctx, request, config)
		if err != nil {
			return err
		}
		if retry.Audit.Status != "duplicate" || retry.Proposal.ID() != proposal.ID() {
			return fmt.Errorf("semantic retry did not return the existing proposal")
		}
		if _, err := fmt.Fprintln(output, "retry=duplicate; no additional draft created"); err != nil {
			return err
		}
	}
	if *publish {
		for _, state := range []ontology.ProposalState{ontology.ProposalSubmitted, ontology.ProposalApproved, ontology.ProposalPublished} {
			proposal, err = demo.client.TransitionOntologyProposal(demo.ctx, proposal.ID(), state,
				"local-demo-reviewer", "Explicit demo -publish flag; synthetic compatibility evaluation only",
				time.Now().UTC())
			if err != nil {
				return err
			}
		}
	}
	if err := demo.close(); err != nil {
		return err
	}
	if err := demo.open(); err != nil {
		return err
	}
	proposals, err := demo.client.OntologyProposals(demo.ctx)
	if err != nil {
		return err
	}
	var stored ontology.GovernedProposal
	for _, candidate := range proposals {
		if candidate.ID() == proposal.ID() {
			stored = candidate
		}
	}
	if stored.ID() != proposal.ID() || stored.State() != proposal.State() ||
		len(stored.Evidence()) != len(proposal.Evidence()) {
		return fmt.Errorf("restart did not preserve proposal identity, state, and evidence")
	}
	identity, err := ontology.NewOntologyIdentity(stored.ProposedVersion())
	if err != nil {
		return err
	}
	interpretations, err := demo.corpus.InterpretAssertions(ctx, demo.assertions, identity)
	if err != nil {
		return err
	}
	resolved := 0
	for _, reading := range interpretations {
		if reading.Resolved() {
			resolved++
		}
	}
	if !*publish && resolved != 0 {
		return fmt.Errorf("unpublished proposal became an interpretation lens")
	}
	if *publish && resolved != len(demo.assertions) {
		return fmt.Errorf("published candidate did not interpret the immutable fixture assertions")
	}
	_, err = fmt.Fprintf(output, "restart=preserved state=%s selected_version_resolved=%d/%d\nreports=%s\n",
		stored.State(), resolved, len(demo.assertions), filepath.Join(*dir, "discovery-reports.jsonl"))
	return err
}

// ollamaConfig builds the demo's Ollama generator configuration. A zero
// numCtx is derived from the default discovery budgets: the harness accepts a
// first prompt of up to MaxInputTokens bytes, which OllamaGenerator counts as
// MaxInputTokens/2 tokens, plus up to MaxOutputTokens of output.
func ollamaConfig(baseURL, name string, numCtx, numGPU int, temperature float64, seed int, format string, timeout time.Duration) model.OllamaConfig {
	if numCtx == 0 {
		budgets := webapi.DefaultOntologyDiscoveryBudgets()
		numCtx = model.OllamaPromptTokenEstimate(budgets.MaxInputTokens) + budgets.MaxOutputTokens
	}
	cfg := model.OllamaConfig{BaseURL: baseURL, Model: name, Timeout: timeout, NumCtx: numCtx, Format: format, PromptBytesPerToken: 2.5}
	if numGPU >= 0 {
		cfg.NumGPU = &numGPU
	}
	if temperature >= 0 {
		cfg.Temperature = &temperature
	}
	if seed >= 0 {
		cfg.Seed = &seed
	}
	return cfg
}

type demoEnvironment struct {
	dir        string
	ctx        context.Context
	authority  *auth.Authority
	policies   *authorized.MemoryPolicyStore
	base       ontology.OntologyVersion
	corpus     *explorer.Explorer
	client     *authorized.Client
	service    *webapi.EmbeddedService
	assertions []ontology.Assertion
}

type demoGeneration struct{}

func (demoGeneration) CurrentPolicyGeneration(context.Context, []byte) (int64, error) {
	return 1, nil
}

func newDemo(ctx context.Context, dir string) (*demoEnvironment, error) {
	base, err := demoOntology()
	if err != nil {
		return nil, err
	}
	authority := auth.NewAuthority()
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "ontology-demo-user", Actor: "ontology-demo-user", AuthorizationDomain: []byte("ontology-demo"),
		AllowedOperations: []auth.Operation{auth.OperationIngest, auth.OperationRead, auth.OperationList,
			auth.OperationRetrieve, auth.OperationNeighborhood, auth.OperationValidate},
		PermittedSourceIDs: [][]byte{[]byte("ontology-demo-source")}, PermittedPolicyIDs: [][]byte{[]byte("ontology-demo-policy")},
		PolicyGeneration: 1, AuthenticationExpires: time.Now().UTC().Add(time.Hour), RequestID: "ontology-demo",
	})
	if err != nil {
		return nil, err
	}
	bound, err := authority.Binder().Bind(ctx, decision)
	if err != nil {
		return nil, err
	}
	demo := &demoEnvironment{dir: dir, ctx: bound, authority: authority, base: base, policies: authorized.NewMemoryPolicyStore()}
	if err := demo.open(); err != nil {
		return nil, err
	}
	return demo, nil
}

func (d *demoEnvironment) open() (resultErr error) {
	corpus, err := explorer.Open(filepath.Join(d.dir, "corpus"))
	if err != nil {
		return err
	}
	d.corpus = corpus
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, d.close())
		}
	}()
	selector, err := authorized.NewStaticPolicySelector([]byte("ontology-demo-source"), []byte("ontology-demo-policy"))
	if err != nil {
		return err
	}
	d.client, err = authorized.NewClient(authorized.Config{
		Base: corpus, InteractionWriter: corpus, InteractionReader: corpus, SnapshotValidator: corpus,
		OntologyInterpreter: corpus, OntologyProposalStore: corpus, Resolver: d.authority.Resolver(),
		PolicySelector: selector, PolicyStore: d.policies, GenerationReader: demoGeneration{}, Clock: time.Now,
	})
	if err != nil {
		return err
	}
	d.service, err = webapi.NewEmbeddedService(d.client)
	if err != nil {
		return err
	}
	return d.service.SetOntologyVersion(d.base)
}

func (d *demoEnvironment) close() error {
	if d.corpus == nil {
		return nil
	}
	err := d.corpus.Close()
	d.corpus = nil
	return err
}

func (d *demoEnvironment) request(ctx context.Context) (webapi.OntologyDiscoveryRequest, error) {
	source, err := d.client.Ingest(d.ctx, explorer.Source{
		URI: "file:///ontology-discovery-demo/SKILL.md", MediaType: explorer.MediaTypeMarkdown,
		Content: "# Graph Helper\n\nTools:\n- graph-cli\n\nCapabilities:\n- Graph interpretation\n",
	})
	if err != nil {
		return webapi.OntologyDiscoveryRequest{}, err
	}
	if _, err := d.client.ExtractDocument(d.ctx, explorer.ExtractionRequest{
		DocumentID: source.Document.ID, RevisionID: source.Revision.ID, Version: d.base,
	}); err != nil {
		return webapi.OntologyDiscoveryRequest{}, err
	}
	graph, err := d.client.Neighborhood(d.ctx, explorer.NeighborhoodRequest{NodeIDs: []shoal.ID{source.Document.ID}, Depth: 2})
	if err != nil {
		return webapi.OntologyDiscoveryRequest{}, err
	}
	d.assertions = graph.Assertions
	if len(d.assertions) == 0 {
		return webapi.OntologyDiscoveryRequest{}, fmt.Errorf("demo extraction produced no immutable assertions")
	}
	active, configured, err := d.service.ActiveOntology(d.ctx)
	if err != nil || !configured {
		return webapi.OntologyDiscoveryRequest{}, errors.Join(err, fmt.Errorf("active demo ontology is unavailable"))
	}
	identity, err := inference.NewOntologyIdentity(active)
	if err != nil {
		return webapi.OntologyDiscoveryRequest{}, err
	}
	snapshot, err := d.client.Snapshot(d.ctx)
	if err != nil {
		return webapi.OntologyDiscoveryRequest{}, err
	}
	pin, err := inference.NewSnapshotPin(shoal.ID(snapshot.ID), snapshot.AsOf)
	if err != nil {
		return webapi.OntologyDiscoveryRequest{}, err
	}
	decision, err := d.authority.Resolver().Resolve(d.ctx)
	if err != nil {
		return webapi.OntologyDiscoveryRequest{}, err
	}
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		return webapi.OntologyDiscoveryRequest{}, err
	}
	authorization, err := inference.NewAuthPin(shoal.ID(fingerprint.String()), decision.AuthenticationExpires())
	if err != nil {
		return webapi.OntologyDiscoveryRequest{}, err
	}
	search := retrieval.Request{Text: "graph-cli", TopK: 8, Modes: []retrieval.Mode{retrieval.ModeLexical}}
	response, err := d.client.Retrieve(d.ctx, search)
	if err != nil {
		return webapi.OntologyDiscoveryRequest{}, err
	}
	pack, err := (contextpack.Builder{Reader: d.client}).Build(d.ctx, contextpack.InitialRequest{
		Request: search, Response: response,
		Pins: contextpack.Pins{Snapshot: pin, Authorization: authorization, Ontology: &identity, PolicyID: "demo-explicit-selection-v1"},
	})
	if err != nil {
		return webapi.OntologyDiscoveryRequest{}, err
	}
	now := time.Now().UTC()
	session := interaction.Session{
		ID: interaction.SessionID(pack.ID(), now), RecordedAt: now, Operation: interaction.OperationRetrieval,
		SnapshotID: pin.ID(), SnapshotAsOf: pin.AsOf(), AuthorizationFingerprint: authorization.Fingerprint(),
		AuthorizationExpiresAt: authorization.ExpiresAt(), OntologySchemaID: active.Schema().ID(), OntologyVersionID: active.ID(),
		QueryDigest: interaction.Digest(search.Text), ContextPackID: pack.ID(), StopReason: "retrieved",
	}
	seen := make(map[shoal.ID]bool)
	for _, anchor := range pack.Evidence() {
		reference, err := anchor.EvidenceReference()
		if err != nil {
			return webapi.OntologyDiscoveryRequest{}, err
		}
		for _, id := range reference.NodeIDs {
			if !seen[id] {
				session.SeedNodeIDs = append(session.SeedNodeIDs, id)
				seen[id] = true
			}
		}
		session.SeedEvidence = append(session.SeedEvidence, reference)
	}
	if err := d.client.RecordInteraction(d.ctx, session); err != nil {
		return webapi.OntologyDiscoveryRequest{}, err
	}
	return webapi.OntologyDiscoveryRequest{
		Context: pack, UsageSessionIDs: []shoal.ID{session.ID}, SelectionPolicy: "demo-explicit-selection-v1",
		SelectionReason: "One locally recorded retrieval of the synthetic tool document",
		PopulationSize:  1, CoverageGaps: []string{"Synthetic single-session sample; not representative of production usage"},
	}, nil
}

func demoOntology() (ontology.OntologyVersion, error) {
	draft := webapi.OntologyProposalVersionDraft{
		Version:    "v1",
		Properties: []webapi.OntologyProposalPropertyDraft{{Key: "name", Name: "Name", ValueType: ontology.ValueString}},
		Concepts: []webapi.OntologyProposalConceptDraft{
			{Key: "skill", Name: "Skill", Properties: []string{"name"}},
			{Key: "tool", Name: "Tool", Properties: []string{"name"}},
			{Key: "capability", Name: "Capability", Properties: []string{"name"}},
		},
		Relationships: []webapi.OntologyProposalRelationshipDraft{
			{Key: "provides_tool", Name: "Provides tool", FromConcepts: []string{"skill"}, ToConcepts: []string{"tool"}, Directed: true},
			{Key: "provides_capability", Name: "Provides capability", FromConcepts: []string{"skill"}, ToConcepts: []string{"capability"}, Directed: true},
		},
	}
	name, err := ontology.NewPropertyDefinition("name", "Name", "", ontology.ValueString, nil, nil)
	if err != nil {
		return ontology.OntologyVersion{}, err
	}
	var concepts []ontology.ConceptDefinition
	ids := make(map[string]shoal.ID)
	for _, item := range draft.Concepts {
		concept, err := ontology.NewConceptDefinition(item.Key, item.Name, "", []shoal.ID{name.ID()}, nil)
		if err != nil {
			return ontology.OntologyVersion{}, err
		}
		concepts, ids[item.Key] = append(concepts, concept), concept.ID()
	}
	var relationships []ontology.RelationshipDefinition
	for _, item := range draft.Relationships {
		relationship, err := ontology.NewRelationshipDefinition(item.Key, item.Name, "",
			[]shoal.ID{ids[item.FromConcepts[0]]}, []shoal.ID{ids[item.ToConcepts[0]]}, nil, true, nil)
		if err != nil {
			return ontology.OntologyVersion{}, err
		}
		relationships = append(relationships, relationship)
	}
	schema, err := ontology.NewOntologySchema("ontology-discovery-demo", "Ontology discovery demo", "Synthetic skills fixture", nil)
	if err != nil {
		return ontology.OntologyVersion{}, err
	}
	return ontology.NewOntologyVersion(schema, "v1", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		concepts, relationships, []ontology.PropertyDefinition{name}, nil)
}

type reportRecorder struct{ file *os.File }

func (r *reportRecorder) RecordOntologyDiscovery(_ context.Context, audit webapi.OntologyDiscoveryAudit) error {
	encoded, err := json.Marshal(audit)
	if err != nil {
		return err
	}
	if _, err := r.file.Write(append(encoded, '\n')); err != nil {
		return err
	}
	return r.file.Sync()
}

type compatibilityEvaluator struct{}

func (compatibilityEvaluator) EvaluateOntology(_ context.Context, base, candidate ontology.OntologyVersion) (webapi.OntologyDiscoveryEvaluation, error) {
	// Held-out shapes are host-side only. This is a compatibility check, not
	// evidence that the scripted provider (or any LLM) learned a useful schema.
	cases := []struct{ from, relation, to string }{
		{"skill", "provides_capability", "capability"},
	}
	score := func(version ontology.OntologyVersion) int {
		keys := make(map[shoal.ID]string)
		for _, concept := range version.Concepts() {
			keys[concept.ID()] = concept.Key()
		}
		correct := 0
		for _, test := range cases {
			for _, relationship := range version.Relationships() {
				from, to := relationship.FromConcepts(), relationship.ToConcepts()
				if relationship.Key() == test.relation && len(from) == 1 && len(to) == 1 &&
					keys[from[0]] == test.from && keys[to[0]] == test.to {
					correct++
					break
				}
			}
		}
		return correct
	}
	return webapi.OntologyDiscoveryEvaluation{
		DatasetID: "sha256:" + interaction.Digest("heldout-shape-fixture-v1: capability-search"),
		PolicyID:  "heldout-endpoint-compatibility-v1", Total: len(cases),
		CaseIDs:         []string{interaction.Digest("heldout: capability-search")},
		EvidenceIDs:     []string{"fixture-sha256:" + interaction.Digest("heldout-shape-fixture-v1")},
		BaselineCorrect: score(base), CandidateCorrect: score(candidate),
		Limitations: []string{"One synthetic endpoint-compatibility case only; not retrieval quality, grounded-answer quality, or independent model validation"},
	}, nil
}

// scriptedModel answers with a delta ("changes") unless full is set, in which
// case it restates the complete proposed_version (the compatibility form).
type scriptedModel struct {
	scenario string
	full     bool
}

func (s scriptedModel) Generate(_ context.Context, request model.GenerateRequest) (model.GenerateResult, error) {
	var prompt struct {
		Query    string `json:"query"`
		Evidence []struct {
			ID       string `json:"id"`
			Citation *struct {
				DocumentID string `json:"document_id"`
			} `json:"citation"`
		} `json:"evidence"`
		Transcript  []json.RawMessage `json:"transcript"`
		HostContext struct {
			Base webapi.OntologyProposalVersionDraft `json:"base"`
		} `json:"host_context"`
	}
	if err := json.Unmarshal([]byte(request.Prompt), &prompt); err != nil {
		return model.GenerateResult{}, err
	}
	if len(prompt.Evidence) == 0 {
		return model.GenerateResult{}, fmt.Errorf("scripted fixture requires evidence")
	}
	var action any
	if len(prompt.Transcript) == 0 {
		for _, anchor := range prompt.Evidence {
			if anchor.Citation != nil {
				action = map[string]any{
					"action": "neighbors", "correlation_id": wireID("demo-walk"),
					"node_id": anchor.Citation.DocumentID, "hops": 2, "fanout": 16,
				}
				break
			}
		}
		if action == nil {
			return model.GenerateResult{}, fmt.Errorf("scripted fixture requires an original document")
		}
	} else if s.scenario == "abstain" {
		action = map[string]any{"action": "stop", "correlation_id": wireID("demo-abstain"),
			"unresolved": []any{map[string]any{"input": "ontology", "reason": "Fixture abstention: evidence does not justify a schema change"}}}
	} else {
		var query struct {
			BaseID    string `json:"base_version_id"`
			Predicate string `json:"predicate"`
		}
		if err := json.Unmarshal([]byte(prompt.Query), &query); err != nil {
			return model.GenerateResult{}, err
		}
		base := prompt.HostContext.Base
		if len(base.Concepts) == 0 {
			return model.GenerateResult{}, fmt.Errorf("scripted fixture requires the base ontology in host_context")
		}
		changes := map[string]any{
			"concepts": []any{}, "relationships": []any{}, "properties": []any{}, "remove": []any{},
		}
		var morphisms []any
		switch s.scenario {
		case "rename":
			for i := range base.Relationships {
				if base.Relationships[i].Key == "provides_tool" {
					renamed := base.Relationships[i]
					renamed.Key, renamed.Name = "offers_tool", "Offers tool"
					base.Relationships[i] = renamed
					changes["relationships"] = []any{renamed}
					changes["remove"] = []any{map[string]string{"namespace": "relationship", "key": "provides_tool"}}
					morphisms = append(morphisms, map[string]any{
						"kind": "rename", "sources": []any{map[string]string{"namespace": "relationship", "key": "provides_tool"}},
						"targets":      []any{map[string]string{"namespace": "relationship", "key": "offers_tool"}},
						"evidence_ids": []string{prompt.Evidence[0].ID}, "rationale": "Scripted semantics-preserving relationship rename",
					})
				}
			}
		case "additive":
			added := webapi.OntologyProposalConceptDraft{
				Key: "tool_collection", Name: "Tool collection", Properties: []string{"name"},
			}
			base.Concepts = append(base.Concepts, added)
			changes["concepts"] = []any{added}
		case "unchanged":
		default:
			return model.GenerateResult{}, fmt.Errorf("invalid scripted scenario")
		}
		body := map[string]any{
			"rationale": "Scripted conformance candidate; not a learned ontology or a demonstrated quality improvement",
			"changes":   changes, "morphisms": morphisms,
			"counterevidence": []string{"The existing schema already represents all source facts"},
			"open_questions":  []string{"Does an independent real task improve? This fixture does not establish that."},
		}
		if s.full {
			delete(body, "changes")
			body["proposed_version"] = base
		}
		candidate, err := json.Marshal(body)
		if err != nil {
			return model.GenerateResult{}, err
		}
		action = map[string]any{"action": "stop", "correlation_id": wireID("demo-candidate"),
			"claims": []any{map[string]any{
				"subject": query.BaseID, "predicate": query.Predicate, "confidence": 0.7,
				"object":       map[string]string{"type": "string", "value": string(candidate)},
				"evidence_ids": []string{prompt.Evidence[0].ID},
			}}}
	}
	encoded, err := json.Marshal(action)
	if err != nil {
		return model.GenerateResult{}, err
	}
	return model.GenerateResult{
		Text: string(encoded), Provenance: model.Provenance{Provider: "scripted", Model: "ontology-conformance-v1"},
		Usage: model.Usage{InputTokens: (len(request.Prompt) + 3) / 4, OutputTokens: (len(encoded) + 3) / 4},
	}, nil
}

func wireID(id shoal.ID) string {
	return base64.StdEncoding.EncodeToString([]byte(id))
}
