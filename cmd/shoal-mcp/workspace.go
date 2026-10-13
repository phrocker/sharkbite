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
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/phrocker/shoal-oss/internal/labelmigration"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/explorer/mcp"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
)

func buildApplication(
	ctx context.Context,
	config commandConfig,
) (*application, error) {
	authority, err := auth.NewAuthorityWithClock(time.Now)
	if err != nil {
		return nil, err
	}
	identity, err := newProcessIdentity(config.identity, time.Now)
	if err != nil {
		return nil, err
	}
	service, client, closeWorkspace, err := openWorkspace(
		ctx, config, authority, time.Now)
	if err != nil {
		return nil, err
	}
	var optionalTools []mcp.OptionalToolProvider
	if identityAllows(config.identity, auth.OperationAnalyticsRead) {
		limits, available := service.AnalyticsLimits()
		if !available {
			return nil, errors.Join(
				fmt.Errorf("analytics was authorized but its provider is unavailable"),
				closeWorkspace(),
			)
		}
		provider, providerErr := mcp.NewAnalyticsTool(service, limits)
		if providerErr != nil {
			return nil, errors.Join(providerErr, closeWorkspace())
		}
		optionalTools = append(optionalTools, provider)
	}
	server, err := mcp.NewServer(mcp.Config{
		Service:         service,
		Authority:       authority,
		Decisions:       identity,
		InteractionSink: client,
		Snapshots:       client,
		OptionalTools:   optionalTools,
		ServerInfo: mcp.Implementation{
			Name:        "shoal-mcp",
			Title:       "Shoal Explorer MCP",
			Version:     "1",
			Description: "Authorized embedded Shoal Explorer over stdio",
		},
		Instructions: "This stdio v1 process uses one trusted launcher-configured " +
			"identity for every caller connected to it. A fresh decision and " +
			"RequestID are bound for each tools/call; independently authenticated " +
			"remote callers use the Streamable HTTP /mcp transport. " +
			"shoal.analytics requires durable interaction recording before success; " +
			"other stdio tool-call recording is not implemented.",
		ContextBudgetBytes: config.contextBudgetBytes,
		ToolCallsPerMinute: config.toolCallsPerMinute,
	})
	if err != nil {
		return nil, errors.Join(err, closeWorkspace())
	}
	app, err := newApplication(server, closeWorkspace)
	if err != nil {
		return nil, errors.Join(err, closeWorkspace())
	}
	return app, nil
}

func identityAllows(config identityConfig, operation auth.Operation) bool {
	for _, allowed := range config.operations {
		if allowed == operation {
			return true
		}
	}
	return false
}

func openWorkspace(
	ctx context.Context,
	config commandConfig,
	authority *auth.Authority,
	clock func() time.Time,
) (*webapi.EmbeddedService, *authorized.Client, func() error, error) {
	noClose := func() error { return nil }
	if ctx == nil {
		return nil, nil, noClose, fmt.Errorf("context is required")
	}
	if stringsBlank(config.corpusDir) || stringsBlank(config.policyDir) {
		return nil, nil, noClose, fmt.Errorf("corpus and policy directories are required")
	}
	if authority == nil {
		return nil, nil, noClose, fmt.Errorf("authorization authority is required")
	}
	if clock == nil {
		return nil, nil, noClose, fmt.Errorf("workspace clock is required")
	}

	corpus, err := explorer.Open(config.corpusDir)
	if err != nil {
		return nil, nil, noClose, err
	}
	if err := corpus.EnsureInteractionSink(ctx); err != nil {
		_ = corpus.Close()
		return nil, nil, noClose, err
	}
	closeCorpus := func() error { return corpus.Close() }
	store, err := authorized.OpenDurablePolicyStore(config.policyDir)
	if err != nil {
		return nil, nil, noClose, errors.Join(err, closeCorpus())
	}
	closeWorkspace := func() error {
		return errors.Join(store.Close(), corpus.Close())
	}
	// A catalog whose write-through failed refuses every request until the
	// process reopens it (#665). There is no readiness surface here, so say
	// it once on diagnostics.
	if diagnostics := config.diagnostics; diagnostics != nil {
		store.OnDiverged(func() {
			fmt.Fprintln(diagnostics, "shoal-mcp: policy catalog write-through "+
				"failed; the catalog now refuses every request. Restart the "+
				"process to reload it from disk.")
		})
	}
	if err := refuseUnregisteredCorpus(
		ctx, corpus, store, config.corpusDir, config.policyDir); err != nil {
		return nil, nil, noClose, errors.Join(err, closeWorkspace())
	}
	selector, err := authorized.NewStaticPolicySelector(
		config.identity.sourceID, config.identity.policyID)
	if err != nil {
		return nil, nil, noClose, errors.Join(err, closeWorkspace())
	}
	scorer, _ := any(corpus).(authorized.VectorScorer)
	client, err := authorized.NewClient(authorized.Config{
		Base:                  corpus,
		VectorScorer:          scorer,
		OntologyInterpreter:   corpus,
		OntologyProposalStore: corpus,
		InteractionWriter:     corpus,
		InteractionReader:     corpus,
		SnapshotValidator:     corpus,
		Resolver:              authority.Resolver(),
		PolicySelector:        selector,
		PolicyStore:           store,
		GenerationReader: configuredGenerationReader{
			domain:     append([]byte(nil), config.identity.domain...),
			generation: config.identity.policyGeneration,
		},
		Clock:                   clock,
		MaxBoundedNodeAdjacency: config.maxNodeAdjacency,
	})
	if err != nil {
		return nil, nil, noClose, errors.Join(err, closeWorkspace())
	}
	// Documents labelled before labels were enforced carry the bare source
	// rule until this narrows them (#570). It runs before the first tool
	// call is served, and any failure refuses to start.
	if err := migrateLabels(ctx, client, config.diagnostics); err != nil {
		return nil, nil, noClose, errors.Join(err, closeWorkspace())
	}
	service, err := webapi.NewEmbeddedService(client)
	if err != nil {
		return nil, nil, noClose, errors.Join(err, closeWorkspace())
	}
	return service, client, closeWorkspace, nil
}

func refuseUnregisteredCorpus(
	ctx context.Context,
	corpus *explorer.Explorer,
	store *authorized.DurablePolicyStore,
	corpusDir string,
	policyDir string,
) error {
	if store.HasRegistrations() {
		return nil
	}
	documents, err := corpus.Documents(ctx)
	if err != nil {
		return err
	}
	if len(documents) == 0 {
		return nil
	}
	return fmt.Errorf(
		"refusing to serve corpus %s with %d document(s) because policy "+
			"catalog %s has no authorization registrations; restore the matching "+
			"policy directory or ingest through an authorized workspace",
		corpusDir, len(documents), policyDir,
	)
}

func stringsBlank(value string) bool {
	for _, char := range value {
		if char != ' ' && char != '\t' && char != '\r' && char != '\n' {
			return false
		}
	}
	return true
}

// migrateLabels runs the startup label migration (#570) and reports its
// counts and untranslatable documents on diagnostics. It fails closed.
func migrateLabels(
	ctx context.Context,
	client *authorized.Client,
	diagnostics io.Writer,
) error {
	if diagnostics == nil {
		diagnostics = io.Discard
	}
	// One of the two mint sites; see TestLabelMigrationCapabilityMintSites
	// in cmd/shoal-explore-web.
	record, err := client.MigrateLabelledDocuments(
		ctx, labelmigration.NewCapability())
	if err != nil {
		return fmt.Errorf(
			"refusing to serve: the startup label migration (#570) failed, so "+
				"documents labelled before labels were enforced could still be "+
				"readable by every holder of their source: %w", err)
	}
	// stderr stays silent unless the migration changed something or left
	// documents unreadable: a stdio launcher may treat any output as noise.
	if record.Tightened == 0 && record.HistoricalTightened == 0 &&
		len(record.Untranslatable) == 0 && len(record.Drift) == 0 {
		return nil
	}
	fmt.Fprintf(diagnostics,
		"shoal-mcp: label migration v%d: %d document(s), %d tightened, %d "+
			"already labelled, %d historical revision(s) narrowed, %d "+
			"untranslatable\n",
		record.Version, record.Documents, record.Tightened,
		record.AlreadyTightened, record.HistoricalTightened,
		len(record.Untranslatable))
	for _, entry := range record.Untranslatable {
		fmt.Fprintf(diagnostics,
			"shoal-mcp: unreadable until relabelled: document %s revision %s "+
				"source %q label %s: %s\n",
			entry.DocumentID, entry.RevisionID, entry.SourceURI,
			entry.EscapedLabel, entry.Reason)
	}
	for _, entry := range record.Drift {
		fmt.Fprintf(diagnostics,
			"shoal-mcp: corpus revision %s of document %s (source %q) is not "+
				"registered (catalog has %s); retry its ingest\n",
			entry.BaseRevisionID, entry.DocumentID, entry.SourceURI,
			entry.CatalogRevisionID)
	}
	return nil
}
