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

package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"reflect"
	"sort"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// OntologyChoices returns one caller-authorized governed snapshot and
// authorizes a settings-selected immutable ontology identity. Implementations
// must consult trusted governed state, never ambient request text.
type OntologyChoices interface {
	ListOntologyChoices(context.Context, auth.Decision) ([]OntologyChoice, error)
	AuthorizeOntology(context.Context, auth.Decision, ontology.OntologyIdentity) error
}

// OperationOntologyChoices authorizes a selected ontology under the exact
// operation whose decision is being narrowed.
type OperationOntologyChoices interface {
	OntologyChoices
	AuthorizeOntologyForOperation(
		context.Context,
		auth.Decision,
		ontology.OntologyIdentity,
		auth.Operation,
	) error
}

func authorizeOntologyForOperation(
	ctx context.Context,
	choices OntologyChoices,
	decision auth.Decision,
	identity ontology.OntologyIdentity,
	operation auth.Operation,
) error {
	if operationChoices, ok := choices.(OperationOntologyChoices); ok {
		return operationChoices.AuthorizeOntologyForOperation(
			ctx, decision, identity, operation)
	}
	return choices.AuthorizeOntology(ctx, decision, identity)
}

// CeilingResolver returns the configured ceiling for a trusted service
// decision. User decisions do not require a service ceiling.
type CeilingResolver interface {
	ResolveServiceCeiling(context.Context, auth.Decision) (auth.ServiceCeiling, error)
}

// StaticOntologyChoices is an exact, immutable set of governable identities.
type StaticOntologyChoices struct {
	identities map[ontology.OntologyIdentity]struct{}
	active     ontology.OntologyIdentity
}

// NewStaticOntologyChoices constructs an exact configured choice set.
func NewStaticOntologyChoices(
	identities ...ontology.OntologyIdentity,
) (StaticOntologyChoices, error) {
	result := StaticOntologyChoices{
		identities: make(map[ontology.OntologyIdentity]struct{}, len(identities)),
	}
	for _, identity := range identities {
		if err := identity.Validate(); err != nil {
			return StaticOntologyChoices{}, err
		}
		result.identities[identity] = struct{}{}
	}
	return result, nil
}

// NewStaticOntologyChoicesWithActive constructs an exact configured choice set
// and marks the trusted active pointer without owning or mutating it.
func NewStaticOntologyChoicesWithActive(
	active ontology.OntologyIdentity,
	identities ...ontology.OntologyIdentity,
) (StaticOntologyChoices, error) {
	if err := active.Validate(); err != nil {
		return StaticOntologyChoices{}, err
	}
	choices, err := NewStaticOntologyChoices(
		append([]ontology.OntologyIdentity{active}, identities...)...)
	if err != nil {
		return StaticOntologyChoices{}, err
	}
	choices.active = active
	return choices, nil
}

// ListOntologyChoices returns only configured governable identities.
func (c StaticOntologyChoices) ListOntologyChoices(
	ctx context.Context,
	_ auth.Decision,
) ([]OntologyChoice, error) {
	if err := validateContext(ctx); err != nil {
		return nil, err
	}
	choices := make([]OntologyChoice, 0, len(c.identities))
	for identity := range c.identities {
		choices = append(choices, OntologyChoice{
			Identity: identity,
			Active:   c.active.Known() && identity == c.active,
		})
	}
	sort.Slice(choices, func(i, j int) bool {
		if comparison := shoal.CompareID(
			choices[i].Identity.SchemaID(),
			choices[j].Identity.SchemaID(),
		); comparison != 0 {
			return comparison < 0
		}
		return shoal.CompareID(
			choices[i].Identity.VersionID(),
			choices[j].Identity.VersionID(),
		) < 0
	})
	return choices, nil
}

// AuthorizeOntology accepts only an exact configured identity.
func (c StaticOntologyChoices) AuthorizeOntology(
	ctx context.Context,
	_ auth.Decision,
	identity ontology.OntologyIdentity,
) error {
	if err := validateContext(ctx); err != nil {
		return err
	}
	if err := identity.Validate(); err != nil {
		return err
	}
	if _, ok := c.identities[identity]; !ok {
		return authDenied()
	}
	return nil
}

// ProviderOptions configures trusted decision, ceiling, ontology, and clock
// dependencies.
type ProviderOptions struct {
	Resolver         auth.Resolver
	GenerationReader auth.GenerationReader
	CeilingResolver  CeilingResolver
	OntologyChoices  OntologyChoices
	Clock            func() time.Time
}

// Provider adds ownership and authorization enforcement to a settings Store.
type Provider struct {
	store            Store
	resolver         auth.Resolver
	generationReader auth.GenerationReader
	ceilingResolver  CeilingResolver
	ontologyChoices  OntologyChoices
	clock            func() time.Time
}

// NewProvider constructs a usable authorized settings provider.
func NewProvider(store Store, options ProviderOptions) (*Provider, error) {
	if absent(store) {
		return nil, invalid("workspace settings store is required")
	}
	if absent(options.Resolver) {
		return nil, invalid("workspace settings decision resolver is required")
	}
	if absent(options.GenerationReader) {
		return nil, invalid("workspace settings generation reader is required")
	}
	if options.Clock == nil {
		options.Clock = time.Now
	}
	return &Provider{
		store: store, resolver: options.Resolver,
		generationReader: options.GenerationReader,
		ceilingResolver:  options.CeilingResolver,
		ontologyChoices:  options.OntologyChoices,
		clock:            options.Clock,
	}, nil
}

// Get authorizes and returns one owned workspace's current settings.
func (p *Provider) Get(
	ctx context.Context,
	workspaceID shoal.ID,
) (Settings, error) {
	check, err := p.authorize(
		ctx, auth.OperationWorkspaceSettingsRead, workspaceID)
	if err != nil {
		return Settings{}, err
	}
	settings, err := p.store.Load(ctx, workspaceID)
	if err != nil {
		return Settings{}, err
	}
	if settings.Owner != check.decision.Subject() {
		return Settings{}, auth.ObjectNotFound()
	}
	if !bytes.Equal(
		settings.AuthorizationDomain, check.decision.AuthorizationDomain()) {
		return Settings{}, auth.ObjectNotFound()
	}
	if err := p.recheck(ctx, check); err != nil {
		return Settings{}, err
	}
	return settings.clone(), nil
}

// Update authorizes, validates, and atomically writes one narrowing revision.
func (p *Provider) Update(
	ctx context.Context,
	workspaceID shoal.ID,
	request UpdateRequest,
) (Settings, error) {
	check, err := p.authorize(
		ctx, auth.OperationWorkspaceSettingsWrite, workspaceID)
	if err != nil {
		return Settings{}, err
	}
	if err := shoal.ValidateRequiredID(
		"settings mutation ID", request.MutationID); err != nil {
		return Settings{}, err
	}
	candidate, err := p.normalizeUpdate(
		ctx, check.decision, check.now, request.Narrowing)
	if err != nil {
		return Settings{}, err
	}
	current, loadErr := p.store.Load(ctx, workspaceID)
	switch {
	case loadErr == nil:
		if current.Owner != check.decision.Subject() {
			return Settings{}, auth.ObjectNotFound()
		}
		if !bytes.Equal(
			current.AuthorizationDomain, check.decision.AuthorizationDomain()) {
			return Settings{}, auth.ObjectNotFound()
		}
		if current.Revision == request.ExpectedRevision {
			if err := ensureMonotonic(current.Narrowing, candidate); err != nil {
				return Settings{}, err
			}
		}
	case shoal.IsErrorCode(loadErr, shoal.ErrorNotFound):
		if request.ExpectedRevision != 0 {
			return Settings{}, versionConflict()
		}
	default:
		return Settings{}, loadErr
	}
	if err := p.recheck(ctx, check); err != nil {
		return Settings{}, err
	}
	result, err := p.store.CompareAndSwap(
		ctx, workspaceID, check.decision.Subject(),
		check.decision.AuthorizationDomain(),
		request.ExpectedRevision, request.MutationID, candidate)
	if err != nil {
		return Settings{}, err
	}
	if err := p.recheck(ctx, check); err != nil {
		return result, explorer.MarkIndeterminateCommit(err)
	}
	return result, nil
}

// OntologyChoice is one caller-eligible immutable lens. Active is projected
// from the trusted ontology provider; settings never mutate that pointer.
type OntologyChoice struct {
	Identity ontology.OntologyIdentity
	Version  string
	Active   bool
}

// OntologyChoiceSet describes the choices visible to one caller for one
// workspace, together with that workspace's current selection.
type OntologyChoiceSet struct {
	WorkspaceID      shoal.ID
	SettingsID       shoal.ID
	SettingsRevision uint64
	SelectedOntology OntologySelection
	Choices          []OntologyChoice
}

// ListOntologyChoices returns only choices authorized for the current issuer
// decision. Existing workspace ownership is checked before choices are read.
func (p *Provider) ListOntologyChoices(
	ctx context.Context,
	workspaceID shoal.ID,
) (OntologyChoiceSet, error) {
	check, err := p.authorize(
		ctx, auth.OperationWorkspaceSettingsRead, workspaceID)
	if err != nil {
		return OntologyChoiceSet{}, err
	}
	if absent(p.ontologyChoices) {
		return OntologyChoiceSet{}, shoal.NewError(
			shoal.ErrorUnavailable, "workspace ontology choices are unavailable")
	}
	result := OntologyChoiceSet{WorkspaceID: workspaceID}
	settings, loadErr := p.store.Load(ctx, workspaceID)
	switch {
	case loadErr == nil:
		if settings.Owner != check.decision.Subject() ||
			!bytes.Equal(
				settings.AuthorizationDomain, check.decision.AuthorizationDomain()) {
			return OntologyChoiceSet{}, auth.ObjectNotFound()
		}
		result.SettingsID = settings.SettingsID
		result.SettingsRevision = settings.Revision
		result.SelectedOntology = settings.Narrowing.SelectedOntology
	case shoal.IsErrorCode(loadErr, shoal.ErrorNotFound):
	default:
		return OntologyChoiceSet{}, loadErr
	}
	choices, err := p.ontologyChoices.ListOntologyChoices(ctx, check.decision)
	if err != nil {
		return OntologyChoiceSet{}, err
	}
	choices, err = normalizeOntologyChoices(choices)
	if err != nil {
		return OntologyChoiceSet{}, err
	}
	if selected, ok := check.decision.SelectedOntology(); ok {
		filtered := choices[:0]
		for _, choice := range choices {
			if choice.Identity == selected {
				filtered = append(filtered, choice)
			}
		}
		if len(filtered) == 0 {
			return OntologyChoiceSet{}, authDenied()
		}
		choices = filtered
	}
	if result.SelectedOntology.Present {
		found := false
		for _, choice := range choices {
			if choice.Identity == result.SelectedOntology.Identity {
				found = true
				break
			}
		}
		if !found {
			return OntologyChoiceSet{}, authDenied()
		}
	}
	if err := p.recheck(ctx, check); err != nil {
		return OntologyChoiceSet{}, err
	}
	result.Choices = choices
	return result, nil
}

// SelectOntology atomically changes only the selected lens, preserving every
// existing operation, scope, budget, and output policy setting.
func (p *Provider) SelectOntology(
	ctx context.Context,
	workspaceID shoal.ID,
	expectedRevision uint64,
	mutationID shoal.ID,
	identity ontology.OntologyIdentity,
) (Settings, error) {
	check, err := p.authorize(
		ctx, auth.OperationWorkspaceSettingsWrite, workspaceID)
	if err != nil {
		return Settings{}, err
	}
	if err := shoal.ValidateRequiredID("settings mutation ID", mutationID); err != nil {
		return Settings{}, err
	}
	if absent(p.ontologyChoices) ||
		authorizeOntologyForOperation(
			ctx, p.ontologyChoices, check.decision, identity,
			auth.OperationWorkspaceSettingsWrite,
		) != nil {
		return Settings{}, authDenied()
	}
	var current Settings
	current, loadErr := p.store.Load(ctx, workspaceID)
	switch {
	case loadErr == nil:
		if current.Owner != check.decision.Subject() ||
			!bytes.Equal(
				current.AuthorizationDomain, check.decision.AuthorizationDomain()) {
			return Settings{}, auth.ObjectNotFound()
		}
	case shoal.IsErrorCode(loadErr, shoal.ErrorNotFound):
		if expectedRevision != 0 {
			return Settings{}, versionConflict()
		}
	default:
		return Settings{}, loadErr
	}
	update := updateNarrowing(current.Narrowing)
	update.SelectedOntology = OntologySelection{
		Present: true, Identity: identity,
	}
	candidate, err := p.normalizeUpdate(
		ctx, check.decision, check.now, update)
	if err != nil {
		return Settings{}, err
	}
	if loadErr == nil && current.Revision == expectedRevision {
		if err := ensureMonotonic(current.Narrowing, candidate); err != nil {
			return Settings{}, err
		}
	}
	if err := p.recheck(ctx, check); err != nil {
		return Settings{}, err
	}
	result, err := p.store.CompareAndSwap(
		ctx, workspaceID, check.decision.Subject(),
		check.decision.AuthorizationDomain(),
		expectedRevision, mutationID, candidate)
	if err != nil {
		return Settings{}, err
	}
	if err := p.recheck(ctx, check); err != nil {
		return result, explorer.MarkIndeterminateCommit(err)
	}
	return result, nil
}

// ApplyForOperation loads one owned settings revision and derives its complete
// effect under the exact operation the consuming request will execute.
func (p *Provider) ApplyForOperation(
	ctx context.Context,
	workspaceID shoal.ID,
	operation auth.Operation,
	baseLimits Limits,
	baseOutputPolicies []auth.Policy,
) (EffectiveDecision, error) {
	if err := operation.Validate(); err != nil {
		return EffectiveDecision{}, err
	}
	check, err := p.authorizeApplication(ctx, workspaceID, operation)
	if err != nil {
		return EffectiveDecision{}, err
	}
	return p.apply(ctx, check, baseLimits, baseOutputPolicies)
}

func (p *Provider) apply(
	ctx context.Context,
	check authorizationCheck,
	baseLimits Limits,
	baseOutputPolicies []auth.Policy,
) (EffectiveDecision, error) {
	workspaceID := check.workspaceID
	settings, err := p.store.Load(ctx, workspaceID)
	if err != nil {
		return EffectiveDecision{}, err
	}
	if settings.Owner != check.decision.Subject() {
		return EffectiveDecision{}, auth.ObjectNotFound()
	}
	if !bytes.Equal(
		settings.AuthorizationDomain, check.decision.AuthorizationDomain()) {
		return EffectiveDecision{}, auth.ObjectNotFound()
	}
	if err := p.recheck(ctx, check); err != nil {
		return EffectiveDecision{}, err
	}
	options := ApplyOptions{
		Now: p.clock(), Operation: check.operation,
		BaseLimits:         baseLimits,
		BaseOutputPolicies: baseOutputPolicies,
		OntologyChoices:    p.ontologyChoices,
	}
	if check.decision.TrustedService() {
		if absent(p.ceilingResolver) {
			return EffectiveDecision{}, authDenied()
		}
		ceiling, err := p.ceilingResolver.ResolveServiceCeiling(
			ctx, check.decision)
		if err != nil {
			return EffectiveDecision{}, authDenied()
		}
		options.ServiceCeiling = &ceiling
	}
	effective, err := DeriveEffectiveDecision(
		ctx, check.decision, settings, options)
	if err != nil {
		return EffectiveDecision{}, err
	}
	effectiveNow := p.clock()
	if effectiveNow.IsZero() ||
		effective.Decision().Authorize(
			check.operation,
			auth.ResourceRequest{
				AuthorizationDomain: effective.Decision().AuthorizationDomain(),
				ObjectID:            workspaceID,
			},
			effectiveNow,
		) != nil {
		return EffectiveDecision{}, authDenied()
	}
	if err := p.recheck(ctx, check); err != nil {
		return EffectiveDecision{}, err
	}
	return effective, nil
}

// ApplyDecisionForOperation returns the current caller decision with only the
// selected workspace's durable narrowing for the exact consuming operation.
func (p *Provider) ApplyDecisionForOperation(
	ctx context.Context,
	workspaceID shoal.ID,
	operation auth.Operation,
) (auth.Decision, error) {
	effective, err := p.ApplyForOperation(
		ctx, workspaceID, operation, MaximumLimits(), nil)
	if err != nil {
		return auth.Decision{}, err
	}
	return effective.Decision(), nil
}

type authorizationCheck struct {
	decision    auth.Decision
	guard       auth.GenerationGuard
	operation   auth.Operation
	workspaceID shoal.ID
	now         time.Time
}

func (p *Provider) authorize(
	ctx context.Context,
	operation auth.Operation,
	workspaceID shoal.ID,
) (authorizationCheck, error) {
	if err := shoal.ValidateRequiredID("workspace ID", workspaceID); err != nil {
		return authorizationCheck{}, err
	}
	decision, err := p.resolver.Resolve(ctx)
	if err != nil {
		return authorizationCheck{}, err
	}
	return p.authorizeDecision(ctx, decision, operation, workspaceID)
}

func (p *Provider) authorizeApplication(
	ctx context.Context,
	workspaceID shoal.ID,
	operation auth.Operation,
) (authorizationCheck, error) {
	if err := shoal.ValidateRequiredID("workspace ID", workspaceID); err != nil {
		return authorizationCheck{}, err
	}
	decision, err := p.resolver.Resolve(ctx)
	if err != nil {
		return authorizationCheck{}, err
	}
	if err := operation.Validate(); err != nil {
		return authorizationCheck{}, err
	}
	return p.authorizeDecision(
		ctx, decision, operation, workspaceID)
}

func (p *Provider) authorizeDecision(
	ctx context.Context,
	decision auth.Decision,
	operation auth.Operation,
	workspaceID shoal.ID,
) (authorizationCheck, error) {
	guard, err := auth.NewGenerationGuard(decision, p.generationReader)
	if err != nil {
		return authorizationCheck{}, authDenied()
	}
	if err := guard.Check(ctx); err != nil {
		return authorizationCheck{}, err
	}
	now := p.clock()
	if now.IsZero() {
		return authorizationCheck{}, authDenied()
	}
	if err := decision.Authorize(operation, auth.ResourceRequest{
		AuthorizationDomain: decision.AuthorizationDomain(),
		ObjectID:            workspaceID,
	}, now); err != nil {
		return authorizationCheck{}, err
	}
	return authorizationCheck{
		decision: decision, guard: guard, operation: operation,
		workspaceID: workspaceID, now: now,
	}, nil
}

func (p *Provider) recheck(
	ctx context.Context,
	check authorizationCheck,
) error {
	if err := check.guard.Check(ctx); err != nil {
		return err
	}
	now := p.clock()
	if now.IsZero() {
		return authDenied()
	}
	if err := check.decision.Authorize(
		check.operation,
		auth.ResourceRequest{
			AuthorizationDomain: check.decision.AuthorizationDomain(),
			ObjectID:            check.workspaceID,
		},
		now,
	); err != nil {
		return authDenied()
	}
	return nil
}

func (p *Provider) normalizeUpdate(
	ctx context.Context,
	decision auth.Decision,
	now time.Time,
	value UpdateNarrowing,
) (Narrowing, error) {
	operations, err := normalizeOperations(value.AllowedOperations)
	if err != nil {
		return Narrowing{}, err
	}
	sources, err := normalizeIDs("permitted source IDs", value.PermittedSourceIDs)
	if err != nil {
		return Narrowing{}, err
	}
	policies, err := normalizeIDs("permitted policy IDs", value.PermittedPolicyIDs)
	if err != nil {
		return Narrowing{}, err
	}
	budgets, err := normalizeBudgets(value.Budgets)
	if err != nil {
		return Narrowing{}, err
	}
	ontologySelection := value.SelectedOntology
	if err := validateOntology(ontologySelection); err != nil {
		return Narrowing{}, err
	}
	if err := validateDecisionSelections(
		decision, operations, sources, policies); err != nil {
		return Narrowing{}, err
	}
	if ontologySelection.Present {
		if selected, ok := decision.SelectedOntology(); ok &&
			selected != ontologySelection.Identity {
			return Narrowing{}, authDenied()
		}
		if absent(p.ontologyChoices) {
			return Narrowing{}, authDenied()
		}
		if err := authorizeOntologyForOperation(
			ctx, p.ontologyChoices, decision, ontologySelection.Identity,
			auth.OperationWorkspaceSettingsWrite,
		); err != nil {
			return Narrowing{}, authDenied()
		}
	}
	outputPolicies := make([]auth.Policy, 0, len(value.OutputPolicies))
	if len(value.OutputPolicies) > MaxOutputPolicies {
		return Narrowing{}, invalid("output policies exceed the public bound")
	}
	var ceiling *auth.ServiceCeiling
	if decision.TrustedService() {
		if absent(p.ceilingResolver) {
			return Narrowing{}, authDenied()
		}
		resolved, err := p.ceilingResolver.ResolveServiceCeiling(ctx, decision)
		if err != nil {
			return Narrowing{}, authDenied()
		}
		if resolved.Identity() != decision.ServiceCeilingIdentity() ||
			resolved.Role() != decision.ServiceRole() {
			return Narrowing{}, authDenied()
		}
		ceiling = &resolved
	}
	for _, spec := range value.OutputPolicies {
		config := auth.PolicyConfig{
			AuthorizationDomain: decision.AuthorizationDomain(),
			SourceID:            append([]byte(nil), spec.SourceID...),
			GrantPolicyID:       append([]byte(nil), spec.GrantPolicyID...),
			Epoch:               spec.Epoch,
		}
		policy, err := auth.NewPolicy(config)
		if err != nil {
			return Narrowing{}, err
		}
		if err := decision.Authorize(
			auth.OperationWorkspaceSettingsWrite,
			auth.ResourceRequest{
				AuthorizationDomain: policy.AuthorizationDomain(),
				SourceID:            policy.SourceID(),
				PolicyID:            policy.GrantPolicyID(),
			},
			now,
		); err != nil {
			return Narrowing{}, err
		}
		if ceiling != nil {
			servicePolicy, err := auth.NewServicePolicy(config, decision)
			if err != nil {
				return Narrowing{}, authDenied()
			}
			if _, err := auth.DeriveScannerAuthorizations(
				decision, auth.OperationWorkspaceSettingsWrite,
				servicePolicy, *ceiling, now,
			); err != nil {
				return Narrowing{}, authDenied()
			}
		}
		outputPolicies = append(outputPolicies, policy)
	}
	outputPolicies, err = normalizePolicies(outputPolicies)
	if err != nil {
		return Narrowing{}, err
	}
	return Narrowing{
		AllowedOperations:  operations,
		PermittedSourceIDs: sources,
		PermittedPolicyIDs: policies,
		Budgets:            budgets,
		OutputPolicies:     outputPolicies,
		SelectedOntology:   ontologySelection,
	}, nil
}

// ApplyOptions supplies trusted non-settings limits and output policies.
type ApplyOptions struct {
	Now                time.Time
	Operation          auth.Operation
	BaseLimits         Limits
	BaseOutputPolicies []auth.Policy
	ServiceCeiling     *auth.ServiceCeiling
	OntologyChoices    OntologyChoices
}

// EffectiveDecision is the complete result of applying one settings revision.
type EffectiveDecision struct {
	decision       auth.Decision
	settingsID     shoal.ID
	revision       uint64
	limits         Limits
	outputPolicies []auth.Policy
	cache          map[string]uint64
}

// Decision returns the fully preserved, narrowed authorization decision.
func (e EffectiveDecision) Decision() auth.Decision { return e.decision }

// SettingsID returns the stable settings identity.
func (e EffectiveDecision) SettingsID() shoal.ID { return e.settingsID }

// Revision returns the applied settings revision.
func (e EffectiveDecision) Revision() uint64 { return e.revision }

// Limits returns the concrete lowered budgets.
func (e EffectiveDecision) Limits() Limits { return e.limits }

// OutputPolicies returns independent immutable policy values whose labels must
// be conjoined with labels derived from output provenance.
func (e EffectiveDecision) OutputPolicies() []auth.Policy {
	return append([]auth.Policy(nil), e.outputPolicies...)
}

// CacheDimensions returns settings partition dimensions suitable for merging
// into auth.CacheKeyConfig.Limits.
func (e EffectiveDecision) CacheDimensions() map[string]uint64 {
	result := make(map[string]uint64, len(e.cache))
	for key, value := range e.cache {
		result[key] = value
	}
	return result
}

// OutputVisibility conjoins all base and settings-added output policies.
func (e EffectiveDecision) OutputVisibility() ([]byte, error) {
	if len(e.outputPolicies) == 0 {
		return nil, nil
	}
	return auth.ConjoinPolicies(e.outputPolicies...)
}

// DeriveEffectiveDecision applies settings to a current trusted decision. It
// never copies identity, delegation, purpose, expiry, generation, role, or
// ceiling from settings.
func DeriveEffectiveDecision(
	ctx context.Context,
	base auth.Decision,
	settings Settings,
	options ApplyOptions,
) (EffectiveDecision, error) {
	if err := validateContext(ctx); err != nil {
		return EffectiveDecision{}, err
	}
	if err := validateSettings(settings); err != nil {
		return EffectiveDecision{}, err
	}
	if settings.Owner != base.Subject() {
		return EffectiveDecision{}, authDenied()
	}
	if !bytes.Equal(
		settings.AuthorizationDomain, base.AuthorizationDomain()) {
		return EffectiveDecision{}, authDenied()
	}
	if options.Now.IsZero() {
		return EffectiveDecision{}, invalid("application time is required")
	}
	operation := options.Operation
	if operation == "" {
		operations := base.AllowedOperations()
		if len(operations) == 0 {
			return EffectiveDecision{}, authDenied()
		}
		operation = operations[0]
	}
	if err := base.Authorize(
		operation,
		auth.ResourceRequest{
			AuthorizationDomain: base.AuthorizationDomain(),
			ObjectID:            settings.WorkspaceID,
		},
		options.Now,
	); err != nil {
		return EffectiveDecision{}, err
	}
	limits, err := normalizeLimits(options.BaseLimits)
	if err != nil {
		return EffectiveDecision{}, err
	}
	narrowing, err := normalizeNarrowing(settings.Narrowing)
	if err != nil {
		return EffectiveDecision{}, err
	}
	if err := validateDecisionSelections(
		base, narrowing.AllowedOperations,
		narrowing.PermittedSourceIDs, narrowing.PermittedPolicyIDs,
	); err != nil {
		return EffectiveDecision{}, authDenied()
	}
	if base.TrustedService() {
		if options.ServiceCeiling == nil ||
			options.ServiceCeiling.Identity() != base.ServiceCeilingIdentity() ||
			options.ServiceCeiling.Role() != base.ServiceRole() {
			return EffectiveDecision{}, authDenied()
		}
	} else if options.ServiceCeiling != nil {
		return EffectiveDecision{}, invalid(
			"a service ceiling cannot be applied to a user decision")
	}
	selected, selectedSet := base.SelectedOntology()
	if narrowing.SelectedOntology.Present {
		if selectedSet && selected != narrowing.SelectedOntology.Identity {
			return EffectiveDecision{}, authDenied()
		}
		if absent(options.OntologyChoices) {
			return EffectiveDecision{}, authDenied()
		}
		if err := authorizeOntologyForOperation(
			ctx, options.OntologyChoices, base,
			narrowing.SelectedOntology.Identity, operation,
		); err != nil {
			return EffectiveDecision{}, authDenied()
		}
		selected = narrowing.SelectedOntology.Identity
		selectedSet = true
	}
	settingsOutputPolicies := make(
		[]auth.Policy, 0, len(narrowing.OutputPolicies))
	for _, stored := range narrowing.OutputPolicies {
		policy, err := neutralOutputPolicy(stored)
		if err != nil {
			return EffectiveDecision{}, err
		}
		if err := authorizeOutputPolicy(
			base, operation, policy, options.ServiceCeiling, options.Now); err != nil {
			return EffectiveDecision{}, authDenied()
		}
		settingsOutputPolicies = append(settingsOutputPolicies, policy)
	}
	outputPolicies := append(
		append([]auth.Policy(nil), options.BaseOutputPolicies...),
		settingsOutputPolicies...,
	)
	outputPolicies, err = normalizePolicies(outputPolicies)
	if err != nil {
		return EffectiveDecision{}, err
	}
	for _, policy := range options.BaseOutputPolicies {
		if err := authorizeOutputPolicy(
			base, operation, policy, options.ServiceCeiling, options.Now); err != nil {
			return EffectiveDecision{}, authDenied()
		}
	}

	operations := base.AllowedOperations()
	if narrowing.AllowedOperations.Present {
		operations = append([]auth.Operation(nil), narrowing.AllowedOperations.Values...)
	}
	sources := base.PermittedSourceIDs()
	if narrowing.PermittedSourceIDs.Present {
		sources = cloneByteSlices(narrowing.PermittedSourceIDs.Values)
	}
	policies := base.PermittedPolicyIDs()
	if narrowing.PermittedPolicyIDs.Present {
		policies = cloneByteSlices(narrowing.PermittedPolicyIDs.Values)
	}
	config := auth.DecisionConfig{
		Subject:                base.Subject(),
		Actor:                  base.Actor(),
		ClientID:               base.ClientID(),
		OnBehalfOf:             base.OnBehalfOf(),
		AuthorizationDomain:    base.AuthorizationDomain(),
		AllowedOperations:      operations,
		PermittedSourceIDs:     sources,
		PermittedPolicyIDs:     policies,
		PolicyGeneration:       base.PolicyGeneration(),
		AuthenticationExpires:  base.AuthenticationExpires(),
		RequestID:              base.RequestID(),
		CorrelationID:          base.CorrelationID(),
		AuditPurpose:           base.AuditPurpose(),
		ServiceRole:            base.ServiceRole(),
		ServiceCeilingIdentity: base.ServiceCeilingIdentity(),
		// Narrowing removes authority; it never changes who granted it.
		GrantProvenance: base.GrantProvenance(),
		// Nor which executor it is bound to (#391).
		ExecutorBinding: base.ExecutorBinding(),
	}
	if selectedSet {
		config.SelectedOntology = selected
	}
	decision, err := auth.NewDecision(config)
	if err != nil {
		return EffectiveDecision{}, err
	}
	limits = applyBudgets(limits, narrowing.Budgets)
	cache := map[string]uint64{
		"workspace_settings_revision": settings.Revision,
	}
	for key, value := range settingsIdentityDimensions(settings.SettingsID) {
		cache[key] = value
	}
	return EffectiveDecision{
		decision:       decision,
		settingsID:     settings.SettingsID,
		revision:       settings.Revision,
		limits:         limits,
		outputPolicies: outputPolicies,
		cache:          cache,
	}, nil
}

// Apply is the concise package-level form of DeriveEffectiveDecision.
func Apply(
	ctx context.Context,
	base auth.Decision,
	settings Settings,
	options ApplyOptions,
) (EffectiveDecision, error) {
	return DeriveEffectiveDecision(ctx, base, settings, options)
}

func authorizeOutputPolicy(
	decision auth.Decision,
	operation auth.Operation,
	policy auth.Policy,
	ceiling *auth.ServiceCeiling,
	now time.Time,
) error {
	if err := decision.Authorize(
		operation,
		auth.ResourceRequest{
			AuthorizationDomain: policy.AuthorizationDomain(),
			SourceID:            policy.SourceID(),
			PolicyID:            policy.GrantPolicyID(),
		},
		now,
	); err != nil {
		return err
	}
	if decision.TrustedService() {
		if ceiling == nil {
			return authDenied()
		}
		servicePolicy, err := auth.NewServicePolicy(auth.PolicyConfig{
			AuthorizationDomain: policy.AuthorizationDomain(),
			SourceID:            policy.SourceID(),
			GrantPolicyID:       policy.GrantPolicyID(),
			Epoch:               policy.Epoch(),
		}, decision)
		if err != nil {
			return authDenied()
		}
		_, err = auth.DeriveScannerAuthorizations(
			decision, operation,
			servicePolicy, *ceiling, now)
		return err
	}
	if policy.ServiceRole() != "" {
		return authDenied()
	}
	return nil
}

func neutralOutputPolicy(policy auth.Policy) (auth.Policy, error) {
	return auth.NewPolicy(auth.PolicyConfig{
		AuthorizationDomain: policy.AuthorizationDomain(),
		SourceID:            policy.SourceID(),
		GrantPolicyID:       policy.GrantPolicyID(),
		Epoch:               policy.Epoch(),
	})
}

func validateDecisionSelections(
	decision auth.Decision,
	operations OperationSelection,
	sources, policies IDSelection,
) error {
	if operations.Present &&
		!operationSubset(operations.Values, decision.AllowedOperations()) {
		return authDenied()
	}
	if sources.Present &&
		!byteSubset(sources.Values, decision.PermittedSourceIDs()) {
		return authDenied()
	}
	if policies.Present &&
		!byteSubset(policies.Values, decision.PermittedPolicyIDs()) {
		return authDenied()
	}
	return nil
}

func normalizeOntologyChoices(
	choices []OntologyChoice,
) ([]OntologyChoice, error) {
	if len(choices) > auth.MaxDecisionGrantIDs {
		return nil, invalid("ontology choices exceed the public ID bound")
	}
	result := append([]OntologyChoice(nil), choices...)
	for _, choice := range result {
		if err := choice.Identity.Validate(); err != nil {
			return nil, err
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if comparison := shoal.CompareID(
			result[i].Identity.SchemaID(),
			result[j].Identity.SchemaID(),
		); comparison != 0 {
			return comparison < 0
		}
		return shoal.CompareID(
			result[i].Identity.VersionID(),
			result[j].Identity.VersionID(),
		) < 0
	})
	normalized := result[:0]
	active := false
	for _, choice := range result {
		if len(normalized) > 0 &&
			normalized[len(normalized)-1].Identity == choice.Identity {
			return nil, invalid("ontology choices contain duplicate identities")
		}
		if choice.Active {
			if active {
				return nil, invalid("ontology choices contain multiple active identities")
			}
			active = true
		}
		normalized = append(normalized, choice)
	}
	return normalized, nil
}

func updateNarrowing(value Narrowing) UpdateNarrowing {
	result := UpdateNarrowing{
		AllowedOperations: OperationSelection{
			Present: value.AllowedOperations.Present,
			Values: append(
				[]auth.Operation(nil), value.AllowedOperations.Values...),
		},
		PermittedSourceIDs: IDSelection{
			Present: value.PermittedSourceIDs.Present,
			Values:  cloneByteSlices(value.PermittedSourceIDs.Values),
		},
		PermittedPolicyIDs: IDSelection{
			Present: value.PermittedPolicyIDs.Present,
			Values:  cloneByteSlices(value.PermittedPolicyIDs.Values),
		},
		Budgets:          cloneBudgets(value.Budgets),
		SelectedOntology: value.SelectedOntology,
	}
	for _, policy := range value.OutputPolicies {
		result.OutputPolicies = append(
			result.OutputPolicies,
			OutputPolicySpec{
				SourceID:      policy.SourceID(),
				GrantPolicyID: policy.GrantPolicyID(),
				Epoch:         policy.Epoch(),
			},
		)
	}
	return result
}

func ensureMonotonic(previous, next Narrowing) error {
	if widenedOperationSelection(previous.AllowedOperations, next.AllowedOperations) ||
		widenedIDSelection(previous.PermittedSourceIDs, next.PermittedSourceIDs) ||
		widenedIDSelection(previous.PermittedPolicyIDs, next.PermittedPolicyIDs) ||
		widenedBudgets(previous.Budgets, next.Budgets) ||
		!policySubset(previous.OutputPolicies, next.OutputPolicies) ||
		(previous.SelectedOntology.Present &&
			(!next.SelectedOntology.Present ||
				next.SelectedOntology.Identity !=
					previous.SelectedOntology.Identity)) {
		return authDenied()
	}
	return nil
}

func widenedOperationSelection(previous, next OperationSelection) bool {
	switch {
	case !previous.Present:
		return false
	case !next.Present:
		return true
	default:
		return !operationSubset(next.Values, previous.Values)
	}
}

func widenedIDSelection(previous, next IDSelection) bool {
	switch {
	case !previous.Present:
		return false
	case !next.Present:
		return true
	default:
		return !byteSubset(next.Values, previous.Values)
	}
}

func widenedBudgets(previous, next Budgets) bool {
	return widenedUint32(previous.RetrievalTopK, next.RetrievalTopK) ||
		widenedUint32(previous.GraphDepth, next.GraphDepth) ||
		widenedUint32(previous.GraphFanout, next.GraphFanout) ||
		widenedUint32(previous.GraphNodes, next.GraphNodes) ||
		widenedUint64(previous.OutputBytes, next.OutputBytes)
}

func widenedUint32(previous, next *uint32) bool {
	if previous == nil {
		return false
	}
	return next == nil || *next > *previous
}

func widenedUint64(previous, next *uint64) bool {
	if previous == nil {
		return false
	}
	return next == nil || *next > *previous
}

func operationSubset(subset, superset []auth.Operation) bool {
	allowed := make(map[auth.Operation]struct{}, len(superset))
	for _, operation := range superset {
		allowed[operation] = struct{}{}
	}
	for _, operation := range subset {
		if _, ok := allowed[operation]; !ok {
			return false
		}
	}
	return true
}

func byteSubset(subset, superset [][]byte) bool {
	for _, value := range subset {
		index := sort.Search(len(superset), func(i int) bool {
			return bytes.Compare(superset[i], value) >= 0
		})
		if index >= len(superset) || !bytes.Equal(superset[index], value) {
			return false
		}
	}
	return true
}

func policySubset(subset, superset []auth.Policy) bool {
	available := make(map[string]struct{}, len(superset))
	for _, policy := range superset {
		encoded, err := policy.Encode()
		if err != nil {
			return false
		}
		available[string(encoded)] = struct{}{}
	}
	for _, policy := range subset {
		encoded, err := policy.Encode()
		if err != nil {
			return false
		}
		if _, ok := available[string(encoded)]; !ok {
			return false
		}
	}
	return true
}

func applyBudgets(base Limits, narrowing Budgets) Limits {
	if narrowing.RetrievalTopK != nil && *narrowing.RetrievalTopK < base.RetrievalTopK {
		base.RetrievalTopK = *narrowing.RetrievalTopK
	}
	if narrowing.GraphDepth != nil && *narrowing.GraphDepth < base.GraphDepth {
		base.GraphDepth = *narrowing.GraphDepth
	}
	if narrowing.GraphFanout != nil && *narrowing.GraphFanout < base.GraphFanout {
		base.GraphFanout = *narrowing.GraphFanout
	}
	if narrowing.GraphNodes != nil && *narrowing.GraphNodes < base.GraphNodes {
		base.GraphNodes = *narrowing.GraphNodes
	}
	if narrowing.OutputBytes != nil && *narrowing.OutputBytes < base.OutputBytes {
		base.OutputBytes = *narrowing.OutputBytes
	}
	return base
}

func settingsIdentityDimensions(id shoal.ID) map[string]uint64 {
	sum := sha256.Sum256([]byte(id))
	return map[string]uint64{
		"workspace_settings_identity_0": binary.BigEndian.Uint64(sum[0:8]),
		"workspace_settings_identity_1": binary.BigEndian.Uint64(sum[8:16]),
		"workspace_settings_identity_2": binary.BigEndian.Uint64(sum[16:24]),
		"workspace_settings_identity_3": binary.BigEndian.Uint64(sum[24:32]),
	}
}

func absent(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
