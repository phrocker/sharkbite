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

// Command shoal-mcp serves an authorized embedded Explorer over MCP stdio.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/explorer/mcp"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
)

const (
	defaultCorpusDir        = ".shoal/explorer"
	defaultIdentityLifetime = 15 * time.Minute
)

type commandConfig struct {
	corpusDir          string
	policyDir          string
	contextBudgetBytes int
	toolCallsPerMinute int
	// maxNodeAdjacency is -max-bounded-node-adjacency (#698):
	// authorized.Config.MaxBoundedNodeAdjacency.
	maxNodeAdjacency uint32
	identity         identityConfig
	// diagnostics receives startup reports such as the label migration's
	// (#570). Nil discards them.
	diagnostics io.Writer
}

type runtimeDependencies struct {
	getenv func(string) string
	build  func(context.Context, commandConfig) (*application, error)
}

type stdioServer interface {
	Serve(context.Context, io.Reader, io.Writer) error
}

type application struct {
	server    stdioServer
	closeFn   func() error
	closeOnce sync.Once
	closeErr  error
}

func main() {
	ctx, stop := signal.NotifyContext(
		context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "shoal-mcp: %v\n", err)
		os.Exit(1)
	}
}

func run(
	ctx context.Context,
	args []string,
	input io.Reader,
	output io.Writer,
	diagnostics io.Writer,
) error {
	return runWith(ctx, args, input, output, diagnostics, runtimeDependencies{
		getenv: os.Getenv,
		build:  buildApplication,
	})
}

func runWith(
	ctx context.Context,
	args []string,
	input io.Reader,
	output io.Writer,
	diagnostics io.Writer,
	deps runtimeDependencies,
) error {
	if ctx == nil {
		return fmt.Errorf("context is required")
	}
	if isAbsent(input) || isAbsent(output) || isAbsent(diagnostics) {
		return fmt.Errorf("stdin, stdout, and stderr are required")
	}
	if deps.getenv == nil || deps.build == nil {
		return fmt.Errorf("runtime dependencies are required")
	}
	config, err := parseCommandConfig(args, diagnostics, deps.getenv)
	if err != nil {
		return err
	}
	app, err := deps.build(ctx, config)
	if err != nil {
		if app != nil {
			return errors.Join(err, app.Close())
		}
		return err
	}
	if app == nil {
		return fmt.Errorf("MCP application construction returned no application")
	}
	serveErr := serveStdio(ctx, app.server, input, output)
	return errors.Join(serveErr, app.Close())
}

func newApplication(
	server stdioServer,
	closeFn func() error,
) (*application, error) {
	if isAbsent(server) {
		return nil, fmt.Errorf("MCP stdio server is required")
	}
	if closeFn == nil {
		return nil, fmt.Errorf("MCP workspace close function is required")
	}
	return &application{server: server, closeFn: closeFn}, nil
}

func (a *application) Close() error {
	if a == nil {
		return nil
	}
	a.closeOnce.Do(func() {
		if a.closeFn == nil {
			a.closeErr = fmt.Errorf("MCP workspace close function is required")
			return
		}
		a.closeErr = a.closeFn()
	})
	return a.closeErr
}

func serveStdio(
	ctx context.Context,
	server stdioServer,
	input io.Reader,
	output io.Writer,
) error {
	if ctx == nil {
		return fmt.Errorf("context is required")
	}
	if isAbsent(server) {
		return fmt.Errorf("MCP stdio server is required")
	}
	closer, canClose := input.(io.Closer)
	if !canClose || ctx.Done() == nil {
		err := server.Serve(ctx, input, output)
		if ctx.Err() != nil && expectedShutdownError(err) {
			return nil
		}
		return err
	}

	done := make(chan error, 1)
	go func() {
		done <- server.Serve(ctx, input, output)
	}()
	select {
	case err := <-done:
		if ctx.Err() != nil && expectedShutdownError(err) {
			return nil
		}
		return err
	case <-ctx.Done():
		closeErr := closer.Close()
		serveErr := <-done
		if expectedShutdownError(closeErr) {
			closeErr = nil
		}
		if expectedShutdownError(serveErr) {
			serveErr = nil
		}
		return errors.Join(serveErr, closeErr)
	}
}

func expectedShutdownError(err error) bool {
	return err == nil ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, os.ErrClosed) ||
		errors.Is(err, io.ErrClosedPipe)
}

func parseCommandConfig(
	args []string,
	diagnostics io.Writer,
	getenv func(string) string,
) (commandConfig, error) {
	var zero commandConfig
	if diagnostics == nil || getenv == nil {
		return zero, fmt.Errorf("configuration dependencies are required")
	}
	developmentDefault, err := environmentBool(
		"SHOAL_MCP_DEV_AUTH", getenv("SHOAL_MCP_DEV_AUTH"))
	if err != nil {
		return zero, err
	}

	flags := flag.NewFlagSet("shoal-mcp", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	stateDir := flags.String(
		"state-dir", getenv("SHOAL_MCP_STATE_DIR"),
		"State root containing corpus/ and policy/ (environment: SHOAL_MCP_STATE_DIR)",
	)
	dataDir := flags.String(
		"data", firstNonEmpty(getenv("SHOAL_MCP_DATA"), defaultCorpusDir),
		"Embedded Explorer corpus directory when -state-dir is unset (environment: SHOAL_MCP_DATA)",
	)
	policyDir := flags.String(
		"policy-dir", getenv("SHOAL_MCP_POLICY_DIR"),
		"Durable authorization policy directory; overrides the derived location (environment: SHOAL_MCP_POLICY_DIR)",
	)
	development := flags.Bool(
		"dev-auth", developmentDefault,
		"Use the fixed local development process identity (environment: SHOAL_MCP_DEV_AUTH)",
	)
	subject := flags.String(
		"identity-subject", getenv("SHOAL_MCP_IDENTITY_SUBJECT"),
		"Trusted process subject (environment: SHOAL_MCP_IDENTITY_SUBJECT)",
	)
	actor := flags.String(
		"identity-actor", getenv("SHOAL_MCP_IDENTITY_ACTOR"),
		"Trusted process actor (environment: SHOAL_MCP_IDENTITY_ACTOR)",
	)
	clientID := flags.String(
		"identity-client-id", getenv("SHOAL_MCP_IDENTITY_CLIENT_ID"),
		"Optional trusted process client ID (environment: SHOAL_MCP_IDENTITY_CLIENT_ID)",
	)
	domain := flags.String(
		"identity-domain", getenv("SHOAL_MCP_IDENTITY_DOMAIN"),
		"Authorization domain (environment: SHOAL_MCP_IDENTITY_DOMAIN)",
	)
	sourceID := flags.String(
		"identity-source", getenv("SHOAL_MCP_IDENTITY_SOURCE"),
		"Permitted source and static policy source ID (environment: SHOAL_MCP_IDENTITY_SOURCE)",
	)
	policyID := flags.String(
		"identity-policy", getenv("SHOAL_MCP_IDENTITY_POLICY"),
		"Permitted policy and static grant policy ID (environment: SHOAL_MCP_IDENTITY_POLICY)",
	)
	labels := flags.String(
		"identity-labels", getenv("SHOAL_MCP_IDENTITY_LABELS"),
		"Comma-separated label grants (#570), each <source>=<label> with the "+
			"source exactly the identity source; without them the identity "+
			"holds no label and can neither ingest nor read labelled content. "+
			"Label policies cannot be named with -identity-policy "+
			"(environment: SHOAL_MCP_IDENTITY_LABELS)",
	)
	operations := flags.String(
		"identity-operations", getenv("SHOAL_MCP_IDENTITY_OPERATIONS"),
		"Comma-separated operations: ingest,list,read,connect,neighborhood,retrieve,validation (environment: SHOAL_MCP_IDENTITY_OPERATIONS)",
	)
	generation := flags.String(
		"identity-generation",
		firstNonEmpty(getenv("SHOAL_MCP_IDENTITY_GENERATION"), "1"),
		"Positive policy generation (environment: SHOAL_MCP_IDENTITY_GENERATION)",
	)
	lifetime := flags.String(
		"identity-lifetime",
		firstNonEmpty(
			getenv("SHOAL_MCP_IDENTITY_LIFETIME"),
			defaultIdentityLifetime.String(),
		),
		"Lifetime of each freshly minted process decision (environment: SHOAL_MCP_IDENTITY_LIFETIME)",
	)
	auditPurpose := flags.String(
		"identity-audit-purpose", getenv("SHOAL_MCP_IDENTITY_AUDIT_PURPOSE"),
		"Optional trusted audit purpose (environment: SHOAL_MCP_IDENTITY_AUDIT_PURPOSE)",
	)
	contextBudget := flags.String(
		"context-budget-bytes",
		firstNonEmpty(
			getenv("SHOAL_MCP_CONTEXT_BUDGET_BYTES"),
			strconv.Itoa(mcp.DefaultContextBudgetBytes),
		),
		"Positive compatibility-text context budget in bytes (environment: SHOAL_MCP_CONTEXT_BUDGET_BYTES)",
	)
	toolCallsPerMinuteValue := flags.String(
		"tool-calls-per-minute",
		firstNonEmpty(
			getenv("SHOAL_MCP_TOOL_CALLS_PER_MINUTE"),
			strconv.Itoa(mcp.DefaultToolCallsPerMinute),
		),
		"Positive per-process MCP tool-call limit (environment: SHOAL_MCP_TOOL_CALLS_PER_MINUTE)",
	)
	maxNodeAdjacencyValue := flags.String(
		"max-bounded-node-adjacency",
		firstNonEmpty(
			getenv("SHOAL_MCP_MAX_BOUNDED_NODE_ADJACENCY"),
			strconv.FormatUint(uint64(authorized.DefaultMaxBoundedNodeAdjacency), 10),
		),
		"Positive cap on the total adjacency of any node a bounded graph read "+
			"expands; a node above it fails uniformly for every reader (#698) "+
			"(environment: SHOAL_MCP_MAX_BOUNDED_NODE_ADJACENCY)",
	)
	if err := flags.Parse(args); err != nil {
		return zero, err
	}
	if flags.NArg() != 0 {
		return zero, fmt.Errorf("unexpected positional arguments: %s",
			strings.Join(flags.Args(), " "))
	}

	policyGeneration, err := strconv.ParseInt(
		strings.TrimSpace(*generation), 10, 64)
	if err != nil || policyGeneration <= 0 {
		return zero, fmt.Errorf("identity generation must be a positive integer")
	}
	decisionLifetime, err := time.ParseDuration(strings.TrimSpace(*lifetime))
	if err != nil || decisionLifetime <= 0 {
		return zero, fmt.Errorf("identity lifetime must be a positive duration")
	}
	contextBudgetBytes, err := strconv.ParseUint(
		strings.TrimSpace(*contextBudget), 10, 63)
	if err != nil || contextBudgetBytes == 0 ||
		contextBudgetBytes > webapi.MaxResponseBytes {
		return zero, fmt.Errorf(
			"context budget must be between 1 and %d bytes",
			webapi.MaxResponseBytes,
		)
	}
	toolCallsPerMinute, err := strconv.ParseInt(
		strings.TrimSpace(*toolCallsPerMinuteValue), 10, 32)
	if err != nil || toolCallsPerMinute <= 0 ||
		toolCallsPerMinute > mcp.MaxToolCallsPerMinute {
		return zero, fmt.Errorf(
			"tool calls per minute must be between 1 and %d",
			mcp.MaxToolCallsPerMinute,
		)
	}
	maxNodeAdjacency, err := strconv.ParseUint(
		strings.TrimSpace(*maxNodeAdjacencyValue), 10, 32)
	if err != nil || maxNodeAdjacency == 0 {
		return zero, fmt.Errorf(
			"max bounded node adjacency must be between 1 and %d", uint32(math.MaxUint32))
	}
	identity, err := configureIdentity(identityOptions{
		development:      *development,
		subject:          *subject,
		actor:            *actor,
		clientID:         *clientID,
		domain:           *domain,
		sourceID:         *sourceID,
		policyID:         *policyID,
		labels:           *labels,
		operations:       *operations,
		policyGeneration: policyGeneration,
		lifetime:         decisionLifetime,
		auditPurpose:     *auditPurpose,
	})
	if err != nil {
		return zero, err
	}
	corpus, policy, err := resolveStatePaths(*stateDir, *dataDir, *policyDir)
	if err != nil {
		return zero, err
	}
	return commandConfig{
		corpusDir:          corpus,
		policyDir:          policy,
		contextBudgetBytes: int(contextBudgetBytes),
		toolCallsPerMinute: int(toolCallsPerMinute),
		maxNodeAdjacency:   uint32(maxNodeAdjacency),
		identity:           identity,
		diagnostics:        diagnostics,
	}, nil
}

func resolveStatePaths(
	stateDir string,
	dataDir string,
	policyDir string,
) (string, string, error) {
	stateDir = strings.TrimSpace(stateDir)
	dataDir = strings.TrimSpace(dataDir)
	policyDir = strings.TrimSpace(policyDir)
	var corpus string
	if stateDir != "" {
		corpus = filepath.Join(stateDir, "corpus")
		if policyDir == "" {
			policyDir = filepath.Join(stateDir, "policy")
		}
	} else {
		if dataDir == "" {
			return "", "", fmt.Errorf("Explorer corpus directory is required")
		}
		corpus = dataDir
		if policyDir == "" {
			policyDir = filepath.Clean(dataDir) + "-policy"
		}
	}
	if strings.TrimSpace(corpus) == "" || strings.TrimSpace(policyDir) == "" {
		return "", "", fmt.Errorf("corpus and policy directories are required")
	}
	var err error
	corpus, err = canonicalStateDirectory("corpus", corpus)
	if err != nil {
		return "", "", err
	}
	policyDir, err = canonicalStateDirectory("policy", policyDir)
	if err != nil {
		return "", "", err
	}
	if err := requireSeparateStateDirectories(corpus, policyDir); err != nil {
		return "", "", err
	}
	return corpus, policyDir, nil
}

func canonicalStateDirectory(label string, directory string) (string, error) {
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return "", fmt.Errorf("resolve %s directory: %w", label, err)
	}
	current := filepath.Clean(absolute)
	var missing []string
	for {
		resolved, resolveErr := filepath.EvalSymlinks(current)
		if resolveErr == nil {
			for index := len(missing) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, missing[index])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(resolveErr, fs.ErrNotExist) {
			return "", fmt.Errorf(
				"resolve %s directory symlinks: %w", label, resolveErr)
		}
		if _, statErr := os.Lstat(current); statErr == nil {
			return "", fmt.Errorf(
				"resolve %s directory symlinks: %w", label, resolveErr)
		} else if !errors.Is(statErr, fs.ErrNotExist) {
			return "", fmt.Errorf(
				"inspect %s directory: %w", label, statErr)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf(
				"resolve %s directory symlinks: %w", label, resolveErr)
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func requireSeparateStateDirectories(corpusDir string, policyDir string) error {
	corpusContainsPolicy, err := pathContains(corpusDir, policyDir)
	if err != nil {
		return err
	}
	policyContainsCorpus, err := pathContains(policyDir, corpusDir)
	if err != nil {
		return err
	}
	if corpusContainsPolicy || policyContainsCorpus {
		return fmt.Errorf(
			"corpus and policy directories must be separate and neither equal nor nested")
	}
	return nil
}

func pathContains(parent string, candidate string) (bool, error) {
	relative, err := filepath.Rel(parent, candidate)
	if err != nil {
		return false, fmt.Errorf("compare state directories: %w", err)
	}
	if relative == "." {
		return true, nil
	}
	parentPrefix := ".." + string(filepath.Separator)
	return relative != ".." &&
		!strings.HasPrefix(relative, parentPrefix) &&
		!filepath.IsAbs(relative), nil
}

func environmentBool(name string, value string) (bool, error) {
	if strings.TrimSpace(value) == "" {
		return false, nil
	}
	parsed, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean", name)
	}
	return parsed, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func isAbsent(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice, reflect.UnsafePointer:
		return reflected.IsNil()
	default:
		return false
	}
}
