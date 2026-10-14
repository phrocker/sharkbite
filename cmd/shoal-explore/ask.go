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

package main

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/phrocker/shoal-oss/pkg/contextpack"
	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/inference/harness"
	"github.com/phrocker/shoal-oss/pkg/model"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	defaultAskTopK              = 3
	defaultAskMaxSteps          = 4
	defaultAskMaxElapsed        = 30 * time.Second
	defaultAskMaxInputTokens    = 262144
	defaultAskMaxOutputTokens   = 8192
	defaultAskMaxEvidence       = 32
	defaultAskMaxGraphHops      = 2
	defaultAskMaxGraphNodes     = 32
	defaultAskMaxFanout         = 3
	defaultAskMaxRepeatedAction = 1
	askPromptTemplateID         = "shoal-explore-ask"
	askPromptVersion            = "v1"
	askHarnessID                = "shoal-explore-ask-v1"
	askToolPolicyID             = "explorer-bounded-tools/v1"
	localAskPolicyID            = shoal.ID("policy:shoal-explore-local")
	localAskAuthFingerprint     = shoal.ID("auth:shoal-explore-local")
)

var errAskNoEvidence = errors.New("ask: no evidence matched the question")

var askOpenAIHTTPClient *http.Client

type askOutput struct {
	Question      string
	Answer        string
	StopReason    harness.StopReason
	Execution     askExecution
	Provenance    askProvenance
	Claims        []askClaim
	Issues        []askIssue
	Evidence      []askEvidence
	Trace         askTraceSummary
	DetailedTrace *askDetailedTrace `json:",omitempty"`
}

type askExecution struct {
	Mode                  string
	SnapshotPinned        bool
	AuthorizationEnforced bool
	Authorization         string
}

type askProvenance struct {
	Harness         string
	Provider        string
	Model           string
	PromptTemplate  string
	PromptVersion   string
	PromptHash      string
	ToolPolicy      string
	ModelParameters []askMetadataEntry `json:",omitempty"`
}

type askClaim struct {
	ID          askID
	Subject     askID
	Predicate   askID
	Object      askValue
	Confidence  shoal.Score
	EvidenceIDs []askID
}

type askIssue struct {
	ID          askID
	Kind        inference.IssueKind
	Input       string
	Reason      string
	EvidenceIDs []askID
}

type askValue struct {
	Type  ontology.ValueType
	Value any
}

type askEvidence struct {
	ID        askID
	Kind      inference.AnchorKind
	Citation  *askCitation          `json:",omitempty"`
	Quote     string                `json:",omitempty"`
	ByteRange *document.SourceRange `json:",omitempty"`
	Path      *askPath              `json:",omitempty"`
}

type askID string

type askCitation struct {
	DocumentID askID
	RevisionID askID
	SectionID  askID
	SpanID     askID
	Range      document.SourceRange
}

type askPath struct {
	Nodes []askNode
	Edges []askEdge
}

type askNode struct {
	ID         askID
	Kind       string
	Labels     []string
	Properties []askMetadataEntry
}

type askEdge struct {
	ID         askID
	From       askID
	To         askID
	Type       string
	Weight     shoal.Score
	Properties []askMetadataEntry
}

type askMetadataEntry struct {
	Key   askBytes
	Value askBytes
}

type askBytes string

type askTraceSummary struct {
	Iterations int
	Tools      []string
	Evidence   int
	Usage      harness.BudgetUsage
	Budgets    harness.Budgets
	StopReason harness.StopReason
}

type askDetailedTrace struct {
	Budgets    harness.Budgets
	Usage      harness.BudgetUsage
	Iterations []askIterationTrace
	StopReason harness.StopReason
	Failures   []harness.FailureTrace
}

type askIterationTrace struct {
	Index         int
	Decision      harness.ActionKind
	CorrelationID askID
	Usage         harness.Usage
	EvidenceIDs   []askID
	Budget        harness.BudgetUsage
	Failure       string
}

func runAsk(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("ask", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	data := flags.String("data", ".shoal", "Explorer data directory")
	readOnly := flags.Bool("read-only", false, "open the corpus read-only; ask refuses to run because it cannot record the interaction")
	questionFlag := flags.String("question", "", "question to answer; positional text is also accepted")
	provider := flags.String("provider", "fake", "model provider: fake, ollama, or openai-compatible")
	modelName := flags.String("model", "", "provider model name")
	ollamaURL := flags.String("ollama-url", envOrDefault("OLLAMA_HOST", model.DefaultOllamaBaseURL), "Ollama base URL")
	apiBaseURL := flags.String("api-base-url", envOrDefault("SHOAL_OPENAI_BASE_URL", os.Getenv("OPENAI_BASE_URL")), "OpenAI-compatible base URL")
	apiKeyEnv := flags.String("api-key-env", "SHOAL_OPENAI_API_KEY", "environment variable containing the OpenAI-compatible API key")
	apiOrg := flags.String("api-organization", os.Getenv("SHOAL_OPENAI_ORGANIZATION"), "optional OpenAI organization header")
	apiProject := flags.String("api-project", os.Getenv("SHOAL_OPENAI_PROJECT"), "optional OpenAI project header")
	initialTop := flags.Uint("top", defaultAskTopK, "initial retrieval results")
	modesValue := flags.String("modes", "lexical,tree,graph", "retrieval modes")
	maxSteps := flags.Int("max-steps", defaultAskMaxSteps, "maximum model-guided iterations")
	maxElapsed := flags.Duration("timeout", defaultAskMaxElapsed, "maximum end-to-end ask duration")
	maxInputTokens := flags.Int("max-input-tokens", defaultAskMaxInputTokens, "maximum model input tokens")
	maxOutputTokens := flags.Int("max-output-tokens", defaultAskMaxOutputTokens, "maximum model output tokens")
	maxEvidence := flags.Int("max-evidence", defaultAskMaxEvidence, "maximum evidence anchors gathered")
	maxGraphHops := flags.Int("max-graph-hops", defaultAskMaxGraphHops, "maximum graph hops")
	maxGraphNodes := flags.Int("max-graph-nodes", defaultAskMaxGraphNodes, "maximum graph nodes")
	maxFanout := flags.Int("max-fanout", defaultAskMaxFanout, "maximum retrieval or graph fanout per tool call")
	maxRepeatedAction := flags.Int("max-repeated-action", defaultAskMaxRepeatedAction, "maximum identical tool action repetitions")
	detailedTrace := flags.Bool("trace", false, "include per-iteration run trace")
	format := flags.String("format", "json", "output format: json or markdown")
	if err := flags.Parse(args); err != nil {
		return err
	}
	question, err := askQuestion(*questionFlag, flags.Args())
	if err != nil {
		return err
	}
	if uint64(*initialTop) > uint64(^uint32(0)) {
		return errors.New("ask -top exceeds uint32")
	}
	if *initialTop == 0 {
		return errors.New("ask -top must be positive")
	}
	modes, err := parseModes(*modesValue)
	if err != nil {
		return err
	}
	if *maxElapsed <= 0 {
		return errors.New("ask -timeout must be positive")
	}
	ctx, cancel := context.WithTimeout(ctx, *maxElapsed)
	defer cancel()
	budgets := harness.Budgets{
		MaxSteps:          *maxSteps,
		MaxElapsed:        *maxElapsed,
		MaxInputTokens:    *maxInputTokens,
		MaxOutputTokens:   *maxOutputTokens,
		MaxEvidence:       *maxEvidence,
		MaxGraphHops:      *maxGraphHops,
		MaxGraphNodes:     *maxGraphNodes,
		MaxFanout:         *maxFanout,
		MaxRepeatedAction: *maxRepeatedAction,
	}
	budgets, err = harness.NormalizeBudgets(budgets)
	if err != nil {
		return err
	}
	outputFormat, err := parseAskFormat(*format)
	if err != nil {
		return err
	}
	generator, providerName, resolvedModel, err := askTextGenerator(ctx, askProviderConfig{
		provider:   *provider,
		model:      *modelName,
		ollamaURL:  *ollamaURL,
		apiBaseURL: *apiBaseURL,
		apiKeyEnv:  *apiKeyEnv,
		apiOrg:     *apiOrg,
		apiProject: *apiProject,
	})
	if err != nil {
		return err
	}
	corpus, err := explorer.OpenWithOptions(*data, explorer.Options{ReadOnly: *readOnly})
	if err != nil {
		return err
	}
	defer corpus.Close()
	// Capture is part of serving an inference. Verify a writable interaction
	// sink here, at setup, so a read-only or offline corpus refuses ask
	// outright with a clear diagnostic instead of failing at first write or,
	// worse, silently answering without a durable record.
	recorder, err := harness.NewGraphRecorder(ctx, corpus)
	if err != nil {
		return fmt.Errorf(
			"ask requires a writable interaction sink in %s: %w", *data, err)
	}
	record, err := runGroundedAsk(ctx, corpus, question, uint32(*initialTop), modes, budgets, generator, providerName, resolvedModel, recorder)
	if errors.Is(err, errAskNoEvidence) {
		return writeAskOutput(output, noEvidenceAskOutput(question, providerName, resolvedModel, budgets, *detailedTrace), outputFormat)
	}
	if record.Request.ID() != "" || record.Trace.StopReason != "" {
		response := askResponse(question, record, *detailedTrace)
		if writeErr := writeAskOutput(output, response, outputFormat); writeErr != nil {
			if err != nil {
				return errors.Join(askRunError(err, record.Trace.StopReason), fmt.Errorf("write ask output: %w", writeErr))
			}
			return writeErr
		}
	}
	if err != nil {
		return askRunError(err, record.Trace.StopReason)
	}
	return nil
}

type askProviderConfig struct {
	provider   string
	model      string
	ollamaURL  string
	apiBaseURL string
	apiKeyEnv  string
	apiOrg     string
	apiProject string
}

func askTextGenerator(ctx context.Context, cfg askProviderConfig) (model.TextGenerator, string, string, error) {
	provider := strings.ToLower(strings.TrimSpace(cfg.provider))
	switch provider {
	case "", "fake":
		name := strings.TrimSpace(cfg.model)
		if name == "" {
			name = "deterministic"
		}
		return model.FakeGenerator{Model: name}, "fake", name, nil
	case "ollama":
		name := strings.TrimSpace(cfg.model)
		if name == "" {
			name = envOrDefault("SHOAL_OLLAMA_MODEL", model.DefaultOllamaGenerateModel)
		}
		generator, err := model.NewOllamaGenerator(model.OllamaConfig{BaseURL: cfg.ollamaURL, Model: name})
		if err != nil {
			return nil, "", "", fmt.Errorf("configure ask provider ollama: %w", err)
		}
		return generator, "ollama", name, nil
	case "openai", "openai-compatible", "api-key":
		baseURL := strings.TrimSpace(cfg.apiBaseURL)
		name := strings.TrimSpace(cfg.model)
		if name == "" {
			name = envOrDefault("SHOAL_OPENAI_MODEL", os.Getenv("OPENAI_MODEL"))
		}
		if baseURL == "" || name == "" {
			return nil, "", "", errors.New("ask provider openai-compatible requires -api-base-url and -model or SHOAL_OPENAI_MODEL")
		}
		keyEnv := strings.TrimSpace(cfg.apiKeyEnv)
		if keyEnv == "" {
			return nil, "", "", errors.New("ask provider openai-compatible requires -api-key-env")
		}
		generator, err := model.NewOpenAIGenerator(model.OpenAIConfig{
			BaseURL:         baseURL,
			GenerationModel: name,
			Organization:    cfg.apiOrg,
			Project:         cfg.apiProject,
			HTTPClient:      askOpenAIHTTPClient,
			Credentials: model.CredentialResolverFunc(func(context.Context) ([]byte, error) {
				value := os.Getenv(keyEnv)
				if value == "" && keyEnv == "SHOAL_OPENAI_API_KEY" {
					value = os.Getenv("OPENAI_API_KEY")
				}
				return []byte(value), nil
			}),
		})
		if err != nil {
			return nil, "", "", fmt.Errorf("configure ask provider openai-compatible: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return nil, "", "", err
		}
		return generator, "openai-compatible", name, nil
	default:
		return nil, "", "", fmt.Errorf("unknown ask provider %q", cfg.provider)
	}
}

func runGroundedAsk(
	ctx context.Context,
	client *explorer.Explorer,
	question string,
	initialTop uint32,
	modes []retrieval.Mode,
	budgets harness.Budgets,
	textGenerator model.TextGenerator,
	providerName string,
	modelName string,
	recorder harness.Recorder,
) (harness.Record, error) {
	snapshot, err := client.Snapshot(ctx)
	if err != nil {
		return harness.Record{}, err
	}
	snapshotPin, err := inference.NewSnapshotPin(shoal.ID(snapshot.ID), snapshot.AsOf)
	if err != nil {
		return harness.Record{}, err
	}
	authPin, err := inference.NewAuthPin(localAskAuthFingerprint, askAuthExpiry(snapshot.AsOf, budgets.MaxElapsed))
	if err != nil {
		return harness.Record{}, err
	}
	request := retrieval.Request{Text: question, TopK: initialTop, Modes: modes, Explain: true}
	response, err := client.Retrieve(ctx, request)
	if err != nil {
		return harness.Record{}, err
	}
	current, err := client.Snapshot(ctx)
	if err != nil {
		return harness.Record{}, err
	}
	if current.ID != snapshot.ID || !current.AsOf.UTC().Equal(snapshot.AsOf.UTC()) {
		return harness.Record{}, fmt.Errorf("ask snapshot changed before context assembly: %w", harness.ErrInvalid)
	}
	if len(response.Results) == 0 {
		return harness.Record{}, errAskNoEvidence
	}
	pinnedRequest := request
	pinnedRequest.AsOf = snapshotPin.AsOf()
	neighborhoods, err := askNeighborhoodsFromResponse(response)
	if err != nil {
		return harness.Record{}, err
	}
	builder := contextpack.Builder{Reader: askBoundedContextReader{
		documents: client,
		graph:     client,
		fanout:    budgets.MaxFanout,
		maxNodes:  budgets.MaxGraphNodes,
	}, Limits: contextpack.Limits{
		MaxResults:    askMaxResults(initialTop, budgets.MaxFanout),
		MaxAnchors:    budgets.MaxEvidence,
		MaxGraphNodes: budgets.MaxGraphNodes,
		MaxGraphEdges: budgets.MaxGraphNodes * budgets.MaxFanout,
		MaxPathNodes:  askPathNodeLimit(budgets.MaxGraphNodes),
	}}
	pack, err := builder.Build(ctx, contextpack.InitialRequest{
		Request:       pinnedRequest,
		Response:      response,
		Neighborhoods: neighborhoods,
		Pins: contextpack.Pins{
			Snapshot: snapshotPin, Authorization: authPin, PolicyID: localAskPolicyID,
		},
	})
	if err != nil {
		return harness.Record{}, fmt.Errorf("build ask context: %w", err)
	}
	current, err = client.Snapshot(ctx)
	if err != nil {
		return harness.Record{}, err
	}
	if current.ID != snapshot.ID || !current.AsOf.UTC().Equal(snapshot.AsOf.UTC()) {
		return harness.Record{}, fmt.Errorf("ask snapshot changed after context assembly: %w", harness.ErrInvalid)
	}
	modelProvenance, err := inference.NewModelProvenance(providerName, modelName, "", nil, nil)
	if err != nil {
		return harness.Record{}, err
	}
	promptProvenance, err := inference.NewPromptProvenance(askPromptTemplateID, askPromptVersion, askPromptHash())
	if err != nil {
		return harness.Record{}, err
	}
	provenance, err := harness.NewProvenance(askHarnessID, modelProvenance, promptProvenance, askToolPolicyID)
	if err != nil {
		return harness.Record{}, err
	}
	runner, err := harness.NewModelRunner(textGenerator, harness.ModelRunnerConfig{MaxOutputTokens: budgets.MaxOutputTokens})
	if err != nil {
		return harness.Record{}, err
	}
	host, err := harness.NewExplorerToolHost(client, builder)
	if err != nil {
		return harness.Record{}, err
	}
	host.PolicyID = localAskPolicyID
	host.RetrievalModes = modes
	host.RetrievalExplain = true
	generator, err := harness.NewGenerator(runner, host, budgets, provenance, recorder)
	if err != nil {
		return harness.Record{}, err
	}
	return generator.Run(ctx, pack)
}

func askMaxResults(initialTop uint32, maxFanout int) int {
	if int(initialTop) > maxFanout {
		return int(initialTop)
	}
	return maxFanout
}

type askBoundedContextReader struct {
	documents *explorer.Explorer
	graph     explorer.BoundedClient
	fanout    int
	maxNodes  int
}

func (r askBoundedContextReader) Document(ctx context.Context, documentID, revisionID shoal.ID) (explorer.DocumentView, error) {
	return r.documents.Document(ctx, documentID, revisionID)
}

func (r askBoundedContextReader) Neighborhood(ctx context.Context, request explorer.NeighborhoodRequest) (explorer.Neighborhood, error) {
	normalized, err := request.Normalize()
	if err != nil {
		return explorer.Neighborhood{}, err
	}
	fanout := r.fanout
	if fanout <= 0 {
		fanout = 1
	}
	maxNodes := r.maxNodes
	if maxNodes <= 0 {
		maxNodes = fanout + len(normalized.NodeIDs)
	}
	bounded, err := r.graph.BoundedNeighborhood(ctx, explorer.BoundedNeighborhoodRequest{
		NodeIDs:         normalized.NodeIDs,
		Depth:           normalized.Depth,
		Fanout:          uint32(fanout),
		MaxNodes:        uint32(maxNodes),
		MaxScannedEdges: uint32(askScanEdgeLimit(fanout, maxNodes)),
		EdgeTypes:       normalized.EdgeTypes,
		Direction:       explorer.GraphDirectionOutgoing,
	})
	if err != nil {
		return explorer.Neighborhood{}, err
	}
	return bounded.Neighborhood, nil
}

func askScanEdgeLimit(fanout, maxNodes int) int {
	if fanout <= 0 {
		fanout = 1
	}
	if maxNodes <= 0 {
		maxNodes = fanout + 1
	}
	return fanout * maxNodes
}

func askPathNodeLimit(maxGraphNodes int) int {
	if maxGraphNodes <= 0 || maxGraphNodes > inference.MaxPathNodes {
		return inference.MaxPathNodes
	}
	return maxGraphNodes
}

func askNeighborhoodsFromResponse(response retrieval.Response) ([]explorer.Neighborhood, error) {
	nodes := make(map[shoal.ID]graph.Node)
	edges := make(map[shoal.ID]graph.Edge)
	for _, result := range response.Results {
		for _, evidence := range result.Evidence {
			path := evidence.Path
			for _, node := range path.Nodes {
				if err := node.Validate(); err != nil {
					return nil, err
				}
				if existing, duplicate := nodes[node.ID]; duplicate && !reflect.DeepEqual(existing, node) {
					return nil, fmt.Errorf("ask retrieval graph path has duplicate node ID: %w", harness.ErrInvalid)
				}
				nodes[node.ID] = node
			}
			for _, edge := range path.Edges {
				if err := edge.Validate(); err != nil {
					return nil, err
				}
				if existing, duplicate := edges[edge.ID]; duplicate && !reflect.DeepEqual(existing, edge) {
					return nil, fmt.Errorf("ask retrieval graph path has duplicate edge ID: %w", harness.ErrInvalid)
				}
				edges[edge.ID] = edge
			}
		}
	}
	if len(nodes) == 0 && len(edges) == 0 {
		return nil, nil
	}
	neighborhood := explorer.Neighborhood{
		Nodes: make([]graph.Node, 0, len(nodes)),
		Edges: make([]graph.Edge, 0, len(edges)),
	}
	for _, node := range nodes {
		neighborhood.Nodes = append(neighborhood.Nodes, node)
	}
	for _, edge := range edges {
		neighborhood.Edges = append(neighborhood.Edges, edge)
	}
	sort.Slice(neighborhood.Nodes, func(i, j int) bool {
		return shoal.CompareID(neighborhood.Nodes[i].ID, neighborhood.Nodes[j].ID) < 0
	})
	sort.Slice(neighborhood.Edges, func(i, j int) bool {
		return shoal.CompareID(neighborhood.Edges[i].ID, neighborhood.Edges[j].ID) < 0
	})
	return []explorer.Neighborhood{neighborhood}, nil
}

func askQuestion(flagValue string, positional []string) (string, error) {
	fromFlag := strings.TrimSpace(flagValue)
	fromArgs := strings.TrimSpace(strings.Join(positional, " "))
	switch {
	case fromFlag != "" && fromArgs != "":
		return "", errors.New("ask accepts either -question or positional question, not both")
	case fromFlag != "":
		return fromFlag, nil
	case fromArgs != "":
		return fromArgs, nil
	default:
		return "", errors.New("ask requires -question or a positional question")
	}
}

func parseAskFormat(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "json":
		return "json", nil
	case "markdown", "md":
		return "markdown", nil
	default:
		return "", fmt.Errorf("unknown ask output format %q", value)
	}
}

func askAuthExpiry(asOf time.Time, maxElapsed time.Duration) time.Time {
	_ = maxElapsed
	asOf = asOf.UTC()
	if asOf.Year() < 9900 {
		return asOf.AddDate(100, 0, 0)
	}
	return time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
}

func askPromptHash() string {
	return harness.ModelPromptTemplateHash()
}

func askResponse(question string, record harness.Record, includeDetailedTrace bool) askOutput {
	result := record.Result
	claims := claimsForOutput(result.Claims())
	issues := issuesForOutput(append(result.Unresolved(), result.Unsupported()...))
	if len(claims) == 0 && len(issues) == 0 {
		issues = []askIssue{unresolvedStopIssue(question, record.Trace.StopReason)}
	}
	assembled := record.Transcript.Context().Evidence()
	assembled = append(assembled, result.EvidenceAdditions()...)
	evidence := evidenceForOutput(assembled)
	out := askOutput{
		Question:   question,
		Answer:     answerFromClaims(claims),
		StopReason: record.Trace.StopReason,
		Execution:  localAskExecution(),
		Provenance: provenanceForOutput(record.Request.Provenance()),
		Claims:     claims,
		Issues:     issues,
		Evidence:   evidence,
		Trace:      traceSummary(record.Trace, len(evidence)),
	}
	if includeDetailedTrace {
		trace := detailedTraceForOutput(record.Trace)
		out.DetailedTrace = &trace
	}
	return out
}

func detailedTraceForOutput(trace harness.RunTrace) askDetailedTrace {
	out := askDetailedTrace{
		Budgets:    trace.Budgets,
		Usage:      trace.Usage,
		Iterations: make([]askIterationTrace, len(trace.Iterations)),
		StopReason: trace.StopReason,
		Failures:   append([]harness.FailureTrace(nil), trace.Failures...),
	}
	for i, iteration := range trace.Iterations {
		out.Iterations[i] = askIterationTrace{
			Index:         iteration.Index,
			Decision:      iteration.Decision,
			CorrelationID: newAskID(iteration.CorrelationID),
			Usage:         iteration.Usage,
			EvidenceIDs:   askIDs(iteration.EvidenceIDs),
			Budget:        iteration.Budget,
			Failure:       iteration.Failure,
		}
	}
	return out
}

func noEvidenceAskOutput(question, providerName, modelName string, budgets harness.Budgets, includeDetailedTrace bool) askOutput {
	trace := harness.RunTrace{
		Budgets:    budgets,
		StopReason: harness.StopReasonStop,
	}
	output := askOutput{
		Question:   question,
		Answer:     "No grounded answer could be produced from the available evidence.",
		StopReason: harness.StopReasonStop,
		Execution:  localAskExecution(),
		Provenance: askProvenance{
			Harness:        askHarnessID,
			Provider:       providerName,
			Model:          modelName,
			PromptTemplate: askPromptTemplateID,
			PromptVersion:  askPromptVersion,
			PromptHash:     askPromptHash(),
			ToolPolicy:     askToolPolicyID,
		},
		Issues: []askIssue{{
			ID:     newAskID("issue:no-evidence"),
			Kind:   inference.IssueUnresolved,
			Input:  question,
			Reason: "initial retrieval returned no evidence for the question",
		}},
		Trace: traceSummary(trace, 0),
	}
	if includeDetailedTrace {
		detailed := detailedTraceForOutput(trace)
		output.DetailedTrace = &detailed
	}
	return output
}

func unresolvedStopIssue(question string, reason harness.StopReason) askIssue {
	text := "run stopped before producing a grounded answer"
	if reason != "" {
		text = fmt.Sprintf("run stopped with %s before producing a grounded answer", reason)
	}
	return askIssue{
		ID:     newAskID(shoal.ID("issue:" + string(reason))),
		Kind:   inference.IssueUnresolved,
		Input:  question,
		Reason: text,
	}
}

func localAskExecution() askExecution {
	return askExecution{
		Mode:                  "local-embedded",
		SnapshotPinned:        true,
		AuthorizationEnforced: false,
		Authorization:         "synthetic local pin; embedded Explorer has no authorization validator",
	}
}

func provenanceForOutput(provenance harness.Provenance) askProvenance {
	modelProvenance := provenance.Model()
	prompt := provenance.Prompt()
	return askProvenance{
		Harness:         provenance.Harness(),
		Provider:        modelProvenance.Provider(),
		Model:           modelProvenance.Model(),
		PromptTemplate:  prompt.TemplateID(),
		PromptVersion:   prompt.Version(),
		PromptHash:      prompt.Hash(),
		ToolPolicy:      provenance.ToolPolicy(),
		ModelParameters: metadataForOutput(modelProvenance.Parameters()),
	}
}

func claimsForOutput(claims []inference.Claim) []askClaim {
	out := make([]askClaim, 0, len(claims))
	for _, claim := range claims {
		out = append(out, askClaim{
			ID:          newAskID(claim.ID()),
			Subject:     newAskID(claim.Subject()),
			Predicate:   newAskID(claim.Predicate()),
			Object:      valueForOutput(claim.Object()),
			Confidence:  claim.Confidence(),
			EvidenceIDs: askIDs(claim.EvidenceIDs()),
		})
	}
	return out
}

func issuesForOutput(issues []inference.Issue) []askIssue {
	out := make([]askIssue, 0, len(issues))
	for _, issue := range issues {
		out = append(out, askIssue{
			ID:          newAskID(issue.ID()),
			Kind:        issue.Kind(),
			Input:       issue.Input(),
			Reason:      issue.Reason(),
			EvidenceIDs: askIDs(issue.EvidenceIDs()),
		})
	}
	return out
}

func valueForOutput(value ontology.Value) askValue {
	out := askValue{Type: value.Type()}
	switch value.Type() {
	case ontology.ValueString:
		out.Value, _ = value.StringValue()
	case ontology.ValueInteger:
		out.Value, _ = value.IntegerValue()
	case ontology.ValueNumber:
		out.Value, _ = value.NumberValue()
	case ontology.ValueBoolean:
		out.Value, _ = value.BooleanValue()
	case ontology.ValueTimestamp:
		stamp, _ := value.TimestampValue()
		out.Value = stamp.Format(time.RFC3339Nano)
	case ontology.ValueReference:
		ref, _ := value.ReferenceValue()
		out.Value = newAskID(ref)
	}
	return out
}

func answerFromClaims(claims []askClaim) string {
	if len(claims) == 0 {
		return "No grounded answer could be produced from the available evidence."
	}
	parts := make([]string, 0, len(claims))
	for _, claim := range claims {
		parts = append(parts, fmt.Sprint(claim.Object.Value))
	}
	return strings.Join(parts, "\n")
}

func evidenceForOutput(anchors []inference.EvidenceAnchor) []askEvidence {
	seen := make(map[shoal.ID]struct{}, len(anchors))
	out := make([]askEvidence, 0, len(anchors))
	for _, anchor := range anchors {
		if _, duplicate := seen[anchor.ID()]; duplicate {
			continue
		}
		seen[anchor.ID()] = struct{}{}
		item := askEvidence{ID: newAskID(anchor.ID()), Kind: anchor.Kind()}
		if citation, quote, ok := anchor.Document(); ok {
			rangeCopy := citation.Range
			item.Citation = &askCitation{
				DocumentID: newAskID(citation.DocumentID),
				RevisionID: newAskID(citation.RevisionID),
				SectionID:  newAskID(citation.SectionID),
				SpanID:     newAskID(citation.SpanID),
				Range:      citation.Range,
			}
			item.ByteRange = &rangeCopy
			item.Quote = quote
		}
		if path, ok := anchor.Path(); ok {
			pathCopy := askPathFrom(path)
			item.Path = &pathCopy
		}
		out = append(out, item)
	}
	return out
}

func newAskID(id shoal.ID) askID {
	if id == "" {
		return ""
	}
	return askID(base64.RawURLEncoding.EncodeToString([]byte(id)))
}

func askIDs(ids []shoal.ID) []askID {
	values := make([]askID, len(ids))
	for i, id := range ids {
		values[i] = newAskID(id)
	}
	return values
}

func askPathFrom(path graph.Path) askPath {
	out := askPath{
		Nodes: make([]askNode, len(path.Nodes)),
		Edges: make([]askEdge, len(path.Edges)),
	}
	for i, node := range path.Nodes {
		out.Nodes[i] = askNode{
			ID:         newAskID(node.ID),
			Kind:       node.Kind,
			Labels:     append([]string(nil), node.Labels...),
			Properties: metadataForOutput(node.Properties),
		}
	}
	for i, edge := range path.Edges {
		out.Edges[i] = askEdge{
			ID:         newAskID(edge.ID),
			From:       newAskID(edge.From),
			To:         newAskID(edge.To),
			Type:       edge.Type,
			Weight:     edge.Weight,
			Properties: metadataForOutput(edge.Properties),
		}
	}
	return out
}

func metadataForOutput(metadata shoal.Metadata) []askMetadataEntry {
	if len(metadata) == 0 {
		return nil
	}
	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]askMetadataEntry, 0, len(keys))
	for _, key := range keys {
		out = append(out, askMetadataEntry{
			Key:   newAskBytes(key),
			Value: newAskBytes(metadata[key]),
		})
	}
	return out
}

func newAskBytes(value string) askBytes {
	return askBytes(base64.RawURLEncoding.EncodeToString([]byte(value)))
}

func traceSummary(trace harness.RunTrace, evidenceCount int) askTraceSummary {
	tools := make([]string, 0, len(trace.Iterations))
	for _, iteration := range trace.Iterations {
		if iteration.Decision != "" && iteration.Decision != harness.ActionStop {
			tools = append(tools, string(iteration.Decision))
		}
	}
	return askTraceSummary{
		Iterations: len(trace.Iterations),
		Tools:      tools,
		Evidence:   evidenceCount,
		Usage:      trace.Usage,
		Budgets:    trace.Budgets,
		StopReason: trace.StopReason,
	}
}

func writeAskOutput(output io.Writer, response askOutput, format string) error {
	if format == "markdown" {
		return writeAskMarkdown(output, response)
	}
	return writeJSON(output, response)
}

func writeAskMarkdown(output io.Writer, response askOutput) error {
	write := func(format string, args ...any) error {
		_, err := fmt.Fprintf(output, format, args...)
		return err
	}
	if err := write("# Answer\n\n%s\n\n", markdownProse(response.Answer)); err != nil {
		return err
	}
	if err := write("## Run\n\n- stop reason: %s\n- mode: %s\n- snapshot pinned: %s\n- authorization enforced: %s\n- authorization: %s\n- provider: %s\n- model: %s\n- prompt: %s %s %s\n- tool policy: %s\n\n",
		markdownCode(response.StopReason), markdownCode(response.Execution.Mode), markdownCode(response.Execution.SnapshotPinned),
		markdownCode(response.Execution.AuthorizationEnforced), markdownCode(response.Execution.Authorization),
		markdownCode(response.Provenance.Provider), markdownCode(response.Provenance.Model),
		markdownCode(response.Provenance.PromptTemplate), markdownCode(response.Provenance.PromptVersion),
		markdownCode(response.Provenance.PromptHash), markdownCode(response.Provenance.ToolPolicy)); err != nil {
		return err
	}
	if err := write("## Claims\n\n"); err != nil {
		return err
	}
	if len(response.Claims) == 0 {
		if err := write("- none\n"); err != nil {
			return err
		}
	}
	for _, claim := range response.Claims {
		if err := write("- %s %s %s (confidence %.3g; evidence %s)\n",
			markdownCode(claim.Subject), markdownCode(claim.Predicate), markdownCode(claim.Object.Value),
			claim.Confidence, markdownCode(strings.Join(idsToStrings(claim.EvidenceIDs), ", "))); err != nil {
			return err
		}
	}
	if err := write("\n## Issues\n\n"); err != nil {
		return err
	}
	if len(response.Issues) == 0 {
		if err := write("- none\n"); err != nil {
			return err
		}
	}
	for _, issue := range response.Issues {
		if err := write("- %s: %s (%s)\n", markdownCode(issue.Kind), markdownCode(issue.Input), markdownCode(issue.Reason)); err != nil {
			return err
		}
	}
	if err := write("\n## Evidence\n\n"); err != nil {
		return err
	}
	if len(response.Evidence) == 0 {
		if err := write("- none\n"); err != nil {
			return err
		}
	}
	for _, evidence := range response.Evidence {
		if err := write("### %s\n\n- kind: %s\n", markdownCode(evidence.ID), markdownCode(evidence.Kind)); err != nil {
			return err
		}
		if evidence.Citation != nil && evidence.ByteRange != nil {
			if err := write("- citation: document %s, revision %s, section %s, span %s\n- byte range: [%d, %d)\n\n%s\n",
				markdownCode(evidence.Citation.DocumentID), markdownCode(evidence.Citation.RevisionID),
				markdownCode(evidence.Citation.SectionID), markdownCode(evidence.Citation.SpanID),
				evidence.ByteRange.Start.Offset, evidence.ByteRange.End.Offset,
				markdownBlock(evidence.Quote)); err != nil {
				return err
			}
		}
		if evidence.Path != nil {
			if err := write("- graph path nodes: %s\n- graph path edges: %s\n\n",
				markdownCode(pathNodeIDs(*evidence.Path)), markdownCode(pathEdgeIDs(*evidence.Path))); err != nil {
				return err
			}
		}
	}
	if err := write("## Trace\n\n- iterations: %d\n- tools: %s\n- evidence: %d\n- budget usage: %+v\n- budget limits: %+v\n",
		response.Trace.Iterations, strings.Join(response.Trace.Tools, ", "),
		response.Trace.Evidence, response.Trace.Usage, response.Trace.Budgets); err != nil {
		return err
	}
	if response.DetailedTrace == nil {
		return nil
	}
	if err := write("\n### Detailed trace\n\n"); err != nil {
		return err
	}
	for _, iteration := range response.DetailedTrace.Iterations {
		if err := write("- iteration %d: decision %s, correlation %s, usage %s, evidence %s, failure %s\n",
			iteration.Index, markdownCode(iteration.Decision), markdownCode(iteration.CorrelationID),
			markdownCode(fmt.Sprintf("%+v", iteration.Usage)), markdownCode(strings.Join(idsToStrings(iteration.EvidenceIDs), ", ")),
			markdownCode(iteration.Failure)); err != nil {
			return err
		}
	}
	if len(response.DetailedTrace.Failures) == 0 {
		return nil
	}
	if err := write("\n### Failures\n\n"); err != nil {
		return err
	}
	for _, failure := range response.DetailedTrace.Failures {
		if err := write("- iteration %d %s: %s\n", failure.Iteration, markdownCode(failure.Operation), markdownCode(failure.Error)); err != nil {
			return err
		}
	}
	return nil
}

func markdownProse(value any) string {
	lines := strings.Split(sanitizeMarkdownText(fmt.Sprint(value)), "\n")
	var builder strings.Builder
	for i, line := range lines {
		if i > 0 {
			builder.WriteByte('\n')
		}
		indent, line := markdownVisibleIndent(line)
		builder.WriteString(indent)
		blockMarker := markdownBlockMarker(line)
		for index, r := range line {
			if index == blockMarker || strings.ContainsRune("\\`*_<&[]|", r) {
				builder.WriteByte('\\')
			}
			builder.WriteRune(r)
		}
	}
	return builder.String()
}

func markdownVisibleIndent(line string) (string, string) {
	index := 0
	columns := 0
	for index < len(line) {
		switch line[index] {
		case ' ':
			columns++
		case '\t':
			columns += 4 - columns%4
		default:
			if columns < 4 {
				return "", line
			}
			return strings.Repeat("\u00a0", columns), line[index:]
		}
		index++
	}
	return "", line
}

func markdownBlockMarker(line string) int {
	start := 0
	for start < len(line) && start < 3 && line[start] == ' ' {
		start++
	}
	if start >= len(line) {
		return -1
	}
	if markdownSetextUnderline(line[start:]) {
		return start
	}
	if markdownHyphenThematicBreak(line[start:]) {
		return start
	}
	switch line[start] {
	case '>':
		return start
	case '#':
		if markdownATXHeading(line[start:]) {
			return start
		}
	case '+', '-':
		if start+1 == len(line) || line[start+1] == ' ' || line[start+1] == '\t' {
			return start
		}
	case '~':
		if strings.HasPrefix(line[start:], "~~~") {
			return start
		}
	}
	digitEnd := start
	for digitEnd < len(line) && digitEnd-start < 9 &&
		line[digitEnd] >= '0' && line[digitEnd] <= '9' {
		digitEnd++
	}
	if digitEnd > start && digitEnd < len(line) &&
		(line[digitEnd] == '.' || line[digitEnd] == ')') &&
		(digitEnd+1 == len(line) || line[digitEnd+1] == ' ' || line[digitEnd+1] == '\t') {
		return digitEnd
	}
	return -1
}

func markdownATXHeading(line string) bool {
	count := 0
	for count < len(line) && line[count] == '#' {
		count++
	}
	return count >= 1 && count <= 6 &&
		(count == len(line) || line[count] == ' ' || line[count] == '\t')
}

func markdownHyphenThematicBreak(line string) bool {
	count := 0
	for _, r := range line {
		switch r {
		case '-':
			count++
		case ' ', '\t':
		default:
			return false
		}
	}
	return count >= 3
}

func markdownSetextUnderline(line string) bool {
	if line == "" || (line[0] != '=' && line[0] != '-') {
		return false
	}
	marker := line[0]
	index := 0
	for index < len(line) && line[index] == marker {
		index++
	}
	for ; index < len(line); index++ {
		if line[index] != ' ' && line[index] != '\t' {
			return false
		}
	}
	return true
}

func markdownCode(value any) string {
	text := sanitizeMarkdownText(fmt.Sprint(value))
	text = strings.ReplaceAll(text, "\n", " ")
	if text == "" {
		return ""
	}
	maxTicks := 0
	current := 0
	for _, r := range text {
		if r == '`' {
			current++
			if current > maxTicks {
				maxTicks = current
			}
			continue
		}
		current = 0
	}
	fence := strings.Repeat("`", maxTicks+1)
	if strings.HasPrefix(text, "`") || strings.HasSuffix(text, "`") ||
		strings.HasPrefix(text, " ") || strings.HasSuffix(text, " ") {
		return fence + " " + text + " " + fence
	}
	return fence + text + fence
}

func markdownBlock(value string) string {
	lines := strings.Split(sanitizeMarkdownText(value), "\n")
	var builder strings.Builder
	for _, line := range lines {
		builder.WriteString("    ")
		builder.WriteString(line)
		builder.WriteByte('\n')
	}
	return builder.String()
}

func sanitizeMarkdownText(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	var builder strings.Builder
	for _, r := range value {
		switch {
		case r == '\n' || r == '\t':
			builder.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&builder, "\\u%04X", r)
		default:
			builder.WriteRune(r)
		}
	}
	return builder.String()
}

func idsToStrings(ids []askID) []string {
	values := make([]string, len(ids))
	for i, id := range ids {
		values[i] = string(id)
	}
	return values
}

func pathNodeIDs(path askPath) string {
	ids := make([]string, len(path.Nodes))
	for i, node := range path.Nodes {
		ids[i] = string(node.ID)
	}
	return strings.Join(ids, " -> ")
}

func pathEdgeIDs(path askPath) string {
	ids := make([]string, len(path.Edges))
	for i, edge := range path.Edges {
		ids[i] = string(edge.ID)
	}
	return strings.Join(ids, " -> ")
}

func askRunError(err error, reason harness.StopReason) error {
	if err == nil {
		return nil
	}
	prefix := "ask failed"
	switch {
	case errors.Is(err, harness.ErrBudgetExhausted):
		prefix = "ask budget exhausted"
	case errors.Is(err, context.Canceled):
		prefix = "ask canceled"
	case errors.Is(err, context.DeadlineExceeded):
		prefix = "ask deadline exceeded"
	case errors.Is(err, model.ErrCredential), errors.Is(err, model.ErrAuthentication):
		prefix = "ask provider authentication failed"
	case errors.Is(err, harness.ErrInvalid):
		prefix = "ask rejected invalid grounded output"
	case errors.Is(err, harness.ErrRunnerUnavailable), errors.Is(err, model.ErrUnavailable):
		prefix = "ask provider unavailable"
	}
	if reason != "" {
		return fmt.Errorf("%s (stop_reason=%s): %w", prefix, reason, err)
	}
	return fmt.Errorf("%s: %w", prefix, err)
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
