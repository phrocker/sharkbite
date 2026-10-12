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
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	developmentSubject         shoal.ID = "development-principal@localhost"
	developmentActor           shoal.ID = "shoal-mcp-dev-auth"
	defaultAuthorizationDomain          = "shoal-explore-web"
	defaultSourceID                     = "shoal-explore-web/workspace"
	defaultPolicyID                     = "shoal-explore-web/workspace-grant"
	processTemplateRequestID   shoal.ID = "mcp-process-identity-template"
)

var (
	readOperations = []auth.Operation{
		auth.OperationList,
		auth.OperationRead,
		auth.OperationNeighborhood,
		auth.OperationRetrieve,
		auth.OperationValidate,
	}
	developmentOperations = []auth.Operation{
		auth.OperationIngest,
		auth.OperationList,
		auth.OperationRead,
		auth.OperationConnect,
		auth.OperationNeighborhood,
		auth.OperationRetrieve,
		auth.OperationValidate,
	}
)

type identityOptions struct {
	development bool
	subject     string
	actor       string
	clientID    string
	domain      string
	sourceID    string
	policyID    string
	// labels is -identity-labels: comma-separated <source>=<label> grants
	// on the identity's own source (#570).
	labels           string
	operations       string
	policyGeneration int64
	lifetime         time.Duration
	auditPurpose     string
}

type identityConfig struct {
	development bool
	subject     shoal.ID
	actor       shoal.ID
	clientID    shoal.ID
	domain      []byte
	sourceID    []byte
	policyID    []byte
	// labelPolicyIDs are the label policies -identity-labels grants. They
	// add visibility only; operations and the source are unchanged.
	labelPolicyIDs   [][]byte
	operations       []auth.Operation
	policyGeneration int64
	lifetime         time.Duration
	auditPurpose     string
}

type processIdentity struct {
	config identityConfig
	clock  func() time.Time
}

// configureIdentity builds the process identity and refuses, for both the
// explicit -identity-policy and the -dev-auth identity, a policy identity in
// the label namespace: label grants come only from the operator's label grant
// file, never from a process identity, and the identity policy doubles as the
// static ingest source policy, which must never be a label policy.
func configureIdentity(options identityOptions) (identityConfig, error) {
	config, err := configureIdentityFields(options)
	if err != nil {
		return identityConfig{}, err
	}
	if auth.IsLabelPolicyID(config.policyID) {
		return identityConfig{}, fmt.Errorf(
			"identity policy %q is in the label namespace %q; label grants come "+
				"only from the label grant file",
			config.policyID, auth.LabelPolicyNamespace)
	}
	// Labels come only from -identity-labels, and only on the identity's
	// own source: the process decision permits that one source, so a grant
	// on any other could never be used and is refused as unknown.
	labels, err := authorized.ParseLabelGrantList(
		options.labels, [][]byte{config.sourceID})
	if err != nil {
		return identityConfig{}, fmt.Errorf("-identity-labels: %w", err)
	}
	config.labelPolicyIDs = labels
	return config, nil
}

func configureIdentityFields(options identityOptions) (identityConfig, error) {
	var zero identityConfig
	if options.policyGeneration <= 0 {
		return zero, fmt.Errorf("identity generation must be positive")
	}
	if options.lifetime <= 0 {
		return zero, fmt.Errorf("identity lifetime must be positive")
	}
	explicit := []string{
		options.subject, options.actor, options.clientID, options.domain,
		options.sourceID, options.policyID, options.operations,
		options.auditPurpose,
	}
	if options.development {
		for _, value := range explicit {
			if strings.TrimSpace(value) != "" {
				return zero, fmt.Errorf(
					"-dev-auth cannot be combined with explicit identity fields")
			}
		}
		return identityConfig{
			development:      true,
			subject:          developmentSubject,
			actor:            developmentActor,
			domain:           []byte(defaultAuthorizationDomain),
			sourceID:         []byte(defaultSourceID),
			policyID:         []byte(defaultPolicyID),
			operations:       append([]auth.Operation(nil), developmentOperations...),
			policyGeneration: options.policyGeneration,
			lifetime:         options.lifetime,
			auditPurpose:     "local stdio development process",
		}, nil
	}

	required := []struct {
		name  string
		value string
	}{
		{"identity subject", options.subject},
		{"identity actor", options.actor},
		{"identity domain", options.domain},
		{"identity source", options.sourceID},
		{"identity policy", options.policyID},
	}
	for _, field := range required {
		if strings.TrimSpace(field.value) == "" {
			return zero, fmt.Errorf(
				"%s is required unless -dev-auth is enabled", field.name)
		}
	}
	operations, err := parseOperations(options.operations)
	if err != nil {
		return zero, err
	}
	auditPurpose := strings.TrimSpace(options.auditPurpose)
	if auditPurpose == "" {
		auditPurpose = "stdio process identity"
	}
	return identityConfig{
		subject:          shoal.ID(strings.TrimSpace(options.subject)),
		actor:            shoal.ID(strings.TrimSpace(options.actor)),
		clientID:         shoal.ID(strings.TrimSpace(options.clientID)),
		domain:           []byte(strings.TrimSpace(options.domain)),
		sourceID:         []byte(strings.TrimSpace(options.sourceID)),
		policyID:         []byte(strings.TrimSpace(options.policyID)),
		operations:       operations,
		policyGeneration: options.policyGeneration,
		lifetime:         options.lifetime,
		auditPurpose:     auditPurpose,
	}, nil
}

func parseOperations(value string) ([]auth.Operation, error) {
	if strings.TrimSpace(value) == "" {
		return append([]auth.Operation(nil), readOperations...), nil
	}
	parts := strings.Split(value, ",")
	operations := make([]auth.Operation, 0, len(parts))
	for _, part := range parts {
		operation := auth.Operation(strings.TrimSpace(part))
		if err := operation.Validate(); err != nil {
			return nil, fmt.Errorf("invalid identity operation %q", part)
		}
		// Collector admin (#446) is refused here although it is a valid
		// operation: this process serves no collector route, so the grant
		// could buy nothing but a decision that holds it. It is minted only
		// by a host that mounts the collector admin routes itself.
		//
		// Approval sweep (#655) likewise: it is the host's system observer's
		// operation, minted only by the host that runs the approval sweep,
		// for one approval at a time.
		if operation == auth.OperationCollectorAdmin ||
			operation == auth.OperationApprovalSweep {
			return nil, fmt.Errorf(
				"identity operation %q cannot be granted to an MCP process", part)
		}
		operations = append(operations, operation)
	}
	if len(operations) == 0 {
		return nil, fmt.Errorf("at least one identity operation is required")
	}
	return operations, nil
}

func newProcessIdentity(
	config identityConfig,
	clock func() time.Time,
) (*processIdentity, error) {
	if clock == nil {
		return nil, fmt.Errorf("identity clock is required")
	}
	provider := &processIdentity{config: cloneIdentityConfig(config), clock: clock}
	if _, err := provider.Decision(context.Background()); err != nil {
		return nil, fmt.Errorf("invalid process identity: %w", err)
	}
	return provider, nil
}

// Decision mints a fresh short-lived template for every tool call. The MCP
// server replaces its placeholder RequestID before binding it to the matching
// authority, so every invocation receives an independently generated ID.
func (p *processIdentity) Decision(ctx context.Context) (auth.Decision, error) {
	if p == nil || p.clock == nil {
		return auth.Decision{}, shoal.NewError(
			shoal.ErrorUnauthorized, "authorization denied")
	}
	if err := ctx.Err(); err != nil {
		return auth.Decision{}, err
	}
	now := p.clock()
	if now.IsZero() {
		return auth.Decision{}, shoal.NewError(
			shoal.ErrorUnavailable, "process identity clock is unavailable")
	}
	return auth.NewDecision(auth.DecisionConfig{
		Subject:               p.config.subject,
		Actor:                 p.config.actor,
		ClientID:              p.config.clientID,
		AuthorizationDomain:   p.config.domain,
		AllowedOperations:     p.config.operations,
		PermittedSourceIDs:    [][]byte{p.config.sourceID},
		PermittedPolicyIDs:    append([][]byte{p.config.policyID}, p.config.labelPolicyIDs...),
		PolicyGeneration:      p.config.policyGeneration,
		AuthenticationExpires: now.Add(p.config.lifetime),
		RequestID:             processTemplateRequestID,
		AuditPurpose:          p.config.auditPurpose,
	})
}

type configuredGenerationReader struct {
	domain     []byte
	generation int64
}

func (r configuredGenerationReader) CurrentPolicyGeneration(
	ctx context.Context,
	domain []byte,
) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if !bytes.Equal(domain, r.domain) {
		return 0, nil
	}
	return r.generation, nil
}

func cloneIdentityConfig(config identityConfig) identityConfig {
	config.domain = append([]byte(nil), config.domain...)
	config.sourceID = append([]byte(nil), config.sourceID...)
	config.policyID = append([]byte(nil), config.policyID...)
	labels := make([][]byte, 0, len(config.labelPolicyIDs))
	for _, label := range config.labelPolicyIDs {
		labels = append(labels, append([]byte(nil), label...))
	}
	config.labelPolicyIDs = labels
	config.operations = append([]auth.Operation(nil), config.operations...)
	return config
}
