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

// Command shoal-explore ingests, navigates, and retrieves cited evidence from
// a local Explorer corpus.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/model"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "shoal-explore: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, output io.Writer) error {
	if len(args) == 0 {
		return errors.New(
			"usage: shoal-explore <ingest|list|outline|query|ask|connect|" +
				"neighbors|fold|unfold|provenance>")
	}
	switch args[0] {
	case "ingest":
		return runIngest(ctx, args[1:], output)
	case "list":
		return runList(ctx, args[1:], output)
	case "outline":
		return runOutline(ctx, args[1:], output)
	case "query":
		return runQuery(ctx, args[1:], output)
	case "ask":
		return runAsk(ctx, args[1:], output)
	case "connect":
		return runConnect(ctx, args[1:], output)
	case "neighbors":
		return runNeighbors(ctx, args[1:], output)
	case "fold":
		return runFold(ctx, args[1:], output)
	case "unfold":
		return runUnfold(ctx, args[1:], output)
	case "provenance":
		return runProvenance(ctx, args[1:], output)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runIngest(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("ingest", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	data := flags.String("data", ".shoal", "Explorer data directory")
	file := flags.String("file", "", "Markdown or text file")
	title := flags.String("title", "", "document title")
	embedding := addEmbeddingFlags(flags)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *file == "" {
		return errors.New("ingest requires -file")
	}
	content, err := os.ReadFile(*file)
	if err != nil {
		return fmt.Errorf("read %s: %w", *file, err)
	}
	absolute, err := filepath.Abs(*file)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", *file, err)
	}
	mediaType := explorer.MediaTypeText
	switch strings.ToLower(filepath.Ext(*file)) {
	case ".md", ".markdown":
		mediaType = explorer.MediaTypeMarkdown
	}
	embedder, err := embedding.embedder()
	if err != nil {
		return err
	}
	corpus, err := explorer.OpenWithOptions(*data, explorer.Options{
		Embedder: embedder,
	})
	if err != nil {
		return err
	}
	defer corpus.Close()
	result, err := corpus.Ingest(ctx, explorer.Source{
		URI: "file://" + filepath.ToSlash(absolute), Title: *title,
		MediaType: mediaType, Content: string(content),
	})
	if err != nil {
		return err
	}
	return writeJSON(output, result)
}

func runList(ctx context.Context, args []string, output io.Writer) error {
	data, err := dataFlag("list", args)
	if err != nil {
		return err
	}
	corpus, err := explorer.Open(data)
	if err != nil {
		return err
	}
	defer corpus.Close()
	documents, err := corpus.Documents(ctx)
	if err != nil {
		return err
	}
	return writeJSON(output, documents)
}

func runOutline(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("outline", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	data := flags.String("data", ".shoal", "Explorer data directory")
	documentID := flags.String("document", "", "document ID")
	revisionID := flags.String("revision", "", "revision ID; newest when omitted")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *documentID == "" {
		return errors.New("outline requires -document")
	}
	corpus, err := explorer.Open(*data)
	if err != nil {
		return err
	}
	defer corpus.Close()
	view, err := corpus.Document(ctx, shoal.ID(*documentID), shoal.ID(*revisionID))
	if err != nil {
		return err
	}
	return writeJSON(output, view)
}

func runQuery(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("query", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	data := flags.String("data", ".shoal", "Explorer data directory")
	text := flags.String("text", "", "retrieval query")
	topK := flags.Uint("top", 5, "maximum results")
	modesValue := flags.String("modes", "lexical,tree,graph", "retrieval modes")
	documentID := flags.String("document", "", "optional document scope")
	explain := flags.Bool("explain", true, "include score explanations")
	embedding := addEmbeddingFlags(flags)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *text == "" {
		return errors.New("query requires -text")
	}
	if uint64(*topK) > uint64(^uint32(0)) {
		return errors.New("query -top exceeds uint32")
	}
	modes, err := parseModes(*modesValue)
	if err != nil {
		return err
	}
	request := retrieval.Request{
		Text: *text, TopK: uint32(*topK), Modes: modes, Explain: *explain,
	}
	if *documentID != "" {
		request.Scope.DocumentIDs = []shoal.ID{shoal.ID(*documentID)}
	}
	embedder, err := embedding.embedder()
	if err != nil {
		return err
	}
	corpus, err := explorer.OpenWithOptions(*data, explorer.Options{
		Embedder: embedder,
	})
	if err != nil {
		return err
	}
	defer corpus.Close()
	response, err := corpus.Retrieve(ctx, request)
	if err != nil {
		return err
	}
	return writeJSON(output, response)
}

func runConnect(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("connect", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	data := flags.String("data", ".shoal", "Explorer data directory")
	edgeID := flags.String("id", "", "stable edge ID")
	from := flags.String("from", "", "source node ID")
	to := flags.String("to", "", "target node ID")
	edgeType := flags.String("type", "", "relationship type")
	weight := flags.Float64("weight", 1, "relationship weight")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *edgeID == "" || *from == "" || *to == "" || *edgeType == "" {
		return errors.New("connect requires -id, -from, -to, and -type")
	}
	corpus, err := explorer.Open(*data)
	if err != nil {
		return err
	}
	defer corpus.Close()
	edge := graph.Edge{
		ID: shoal.ID(*edgeID), From: shoal.ID(*from), To: shoal.ID(*to),
		Type: *edgeType, Weight: shoal.Score(*weight),
	}
	if err := corpus.Connect(ctx, edge); err != nil {
		return err
	}
	return writeJSON(output, edge)
}

func runNeighbors(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("neighbors", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	data := flags.String("data", ".shoal", "Explorer data directory")
	nodeID := flags.String("node", "", "graph node ID")
	depth := flags.Uint("depth", 1, "expansion depth")
	edgeTypes := flags.String("edge-types", "", "optional comma-separated edge types")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *nodeID == "" {
		return errors.New("neighbors requires -node")
	}
	if uint64(*depth) > uint64(^uint32(0)) {
		return errors.New("neighbors -depth exceeds uint32")
	}
	request := explorer.NeighborhoodRequest{
		NodeIDs: []shoal.ID{shoal.ID(*nodeID)}, Depth: uint32(*depth),
	}
	if *edgeTypes != "" {
		request.EdgeTypes = splitNonempty(*edgeTypes)
	}
	corpus, err := explorer.Open(*data)
	if err != nil {
		return err
	}
	defer corpus.Close()
	neighborhood, err := corpus.Neighborhood(ctx, request)
	if err != nil {
		return err
	}
	return writeJSON(output, neighborhood)
}

func dataFlag(name string, args []string) (string, error) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	data := flags.String("data", ".shoal", "Explorer data directory")
	if err := flags.Parse(args); err != nil {
		return "", err
	}
	return *data, nil
}

type embeddingFlags struct {
	provider   *string
	model      *string
	baseURL    *string
	apiKeyEnv  *string
	dimensions *int
}

func addEmbeddingFlags(flags *flag.FlagSet) embeddingFlags {
	return embeddingFlags{
		provider: flags.String(
			"embedding-provider", "",
			"Optional vector provider for ingest/query: fake, lexical, ollama, openai, or voyage",
		),
		model: flags.String(
			"embedding-model", "",
			"Embedding model name for -embedding-provider",
		),
		baseURL: flags.String(
			"embedding-base-url", "",
			"Embedding provider base URL for ollama/openai/voyage",
		),
		apiKeyEnv: flags.String(
			"embedding-api-key-env", "OPENAI_API_KEY",
			"Environment variable read at request time for openai/voyage credentials",
		),
		dimensions: flags.Int(
			"embedding-dimensions", 0,
			"Embedding dimensions; required for ollama/openai, zero uses fake default",
		),
	}
}

func (f embeddingFlags) embedder() (model.Embedder, error) {
	switch *f.provider {
	case "":
		return nil, nil
	case "fake":
		return model.FakeEmbedder{
			Model: *f.model, Dimensions: *f.dimensions,
		}, nil
	case "lexical":
		if *f.dimensions <= 0 {
			return nil, fmt.Errorf("embedding dimensions are required for lexical")
		}
		return model.NewLexicalEmbedder(model.LexicalConfig{
			Dimensions: *f.dimensions,
			Model:      *f.model,
		})
	case "voyage":
		if *f.dimensions <= 0 {
			return nil, fmt.Errorf("embedding dimensions are required for voyage")
		}
		return model.NewVoyageEmbedder(model.VoyageConfig{
			BaseURL:          *f.baseURL,
			Model:            *f.model,
			Dimensions:       *f.dimensions,
			APICredentialEnv: *f.apiKeyEnv,
		})
	case "ollama":
		if *f.dimensions <= 0 {
			return nil, fmt.Errorf("embedding dimensions are required for ollama")
		}
		return model.NewOllamaEmbedder(model.OllamaConfig{
			BaseURL:    *f.baseURL,
			Model:      *f.model,
			Dimensions: *f.dimensions,
		})
	case "openai":
		if *f.dimensions <= 0 {
			return nil, fmt.Errorf("embedding dimensions are required for openai")
		}
		envName := *f.apiKeyEnv
		return model.NewOpenAIEmbedder(model.OpenAIConfig{
			BaseURL:             *f.baseURL,
			EmbeddingModel:      *f.model,
			EmbeddingDimensions: *f.dimensions,
			Credentials:         envCredentialResolver(envName),
		})
	default:
		return nil, fmt.Errorf("unknown embedding provider %q", *f.provider)
	}
}

type envCredentialResolver string

func (r envCredentialResolver) ResolveCredential(context.Context) ([]byte, error) {
	if r == "" {
		return nil, model.ErrInvalidConfig
	}
	value := os.Getenv(string(r))
	if value == "" {
		return nil, model.ErrCredential
	}
	return []byte(value), nil
}

func (r envCredentialResolver) CacheIdentity() (string, error) {
	if r == "" {
		return "", model.ErrInvalidConfig
	}
	return "env:" + string(r), nil
}

func parseModes(value string) ([]retrieval.Mode, error) {
	values := splitNonempty(value)
	modes := make([]retrieval.Mode, 0, len(values))
	for _, value := range values {
		mode := retrieval.Mode(value)
		switch mode {
		case retrieval.ModeLexical, retrieval.ModeTree,
			retrieval.ModeGraph, retrieval.ModeVector:
			modes = append(modes, mode)
		default:
			return nil, fmt.Errorf("unknown retrieval mode %q", value)
		}
	}
	return modes, nil
}

func splitNonempty(value string) []string {
	var values []string
	for _, candidate := range strings.Split(value, ",") {
		if candidate = strings.TrimSpace(candidate); candidate != "" {
			values = append(values, candidate)
		}
	}
	return values
}

func writeJSON(output io.Writer, value any) error {
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
