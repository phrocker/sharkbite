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

package atpl

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// The checks here exist to say *where* a policy is wrong. Whether it is wrong
// is decided by the registry's own validators, called through
// pkg/explorer/fleet/policy_export.go one capability or action at a time so a
// refusal carries the offending path, and then once over the whole spec as
// Register calls them. The per-item checks may refuse first with a more precise
// message, but never refuse anything the whole-spec checks would accept: the
// parity test in this package runs the same fixtures through Register.

// knownEffects lists the classes for refusal messages, in the registry's wire
// spellings.
const knownEffects = "reads-corpus, egresses-content, external"

type compiledAgent struct {
	spec fleet.Spec
	ttl  time.Duration
	// chain is how many registrations Register's activeChain walks from this
	// agent to its root, this agent included.
	chain int
}

type compiler struct {
	now       time.Time
	executors map[string]ExecutorBound
	live      LiveParents
	declared  map[shoal.ID]declaredAgent
	compiled  map[shoal.ID]compiledAgent
}

// parentView is what delegation is checked against: a parent compiled from
// this policy, or a live registration it delegates from.
type parentView struct {
	label        string
	domain       []byte
	scopes       []fleet.Scope
	capabilities []fleet.Capability
	lease        time.Time
	ttl          time.Duration
	live         bool
	// chain is the parent's own chain length; Register refuses a child whose
	// parent chain has already reached fleet.MaxDelegationDepth.
	chain int
}

func (c *compiler) agent(entry declaredAgent) (compiledAgent, error) {
	agent := entry.agent
	id := shoal.ID(agent.ID)
	path := agentPath(id)
	fail := func(sub, message string) error {
		if sub == "" {
			return refuse(entry.file, path, message)
		}
		return refuse(entry.file, path+"."+sub, message)
	}

	if err := shoal.ValidateRequiredID("agent ID", id); err != nil {
		return compiledAgent{}, fail("id", errorMessage(err))
	}
	// Compile accepts documents built in code, which need not have come
	// through Decode. Every string that reaches the digest must be UTF-8, or
	// encoding/json would replace invalid bytes and two different policies
	// would share one digest.
	for _, field := range agentStrings(agent) {
		if !utf8.ValidString(field.value) {
			return compiledAgent{}, fail(field.path, "is not valid UTF-8")
		}
	}
	parentID := shoal.ID(agent.Parent)
	if err := shoal.ValidateOptionalID("parent agent ID", parentID); err != nil {
		return compiledAgent{}, fail("parent", errorMessage(err))
	}
	parent, err := c.parent(parentID)
	if err != nil {
		return compiledAgent{}, fail("parent", err.Error())
	}
	chain := 1
	if parent != nil {
		if parent.chain >= fleet.MaxDelegationDepth {
			return compiledAgent{}, fail("parent", fmt.Sprintf(
				"delegation depth exceeds %d: %s already heads a chain of %d registrations",
				fleet.MaxDelegationDepth, parent.label, parent.chain))
		}
		chain = parent.chain + 1
	}

	domain := []byte(agent.AuthorizationDomain)
	if len(domain) == 0 || len(domain) > 1024 {
		return compiledAgent{}, fail("authorization_domain", "authorization domain is outside its bound")
	}
	if parent != nil && !bytes.Equal(domain, parent.domain) {
		return compiledAgent{}, fail("authorization_domain", fmt.Sprintf(
			"differs from parent %s; delegation cannot change the authorization domain", parent.label))
	}

	if len(agent.Scopes) == 0 || len(agent.Scopes) > fleet.MaxScopes {
		return compiledAgent{}, fail("scopes", fmt.Sprintf(
			"must declare between 1 and %d scopes", fleet.MaxScopes))
	}
	scopes := make([]fleet.Scope, 0, len(agent.Scopes))
	for i, scope := range agent.Scopes {
		scopePath := fmt.Sprintf("scopes[%d]", i)
		compiled := fleet.Scope{SourceID: []byte(scope.SourceID), PolicyID: []byte(scope.PolicyID)}
		if len(compiled.SourceID) == 0 || len(compiled.SourceID) > 1024 ||
			len(compiled.PolicyID) == 0 || len(compiled.PolicyID) > 1024 {
			return compiledAgent{}, fail(scopePath, "agent scope identity is outside its bound")
		}
		for j, earlier := range scopes {
			if bytes.Equal(earlier.SourceID, compiled.SourceID) &&
				bytes.Equal(earlier.PolicyID, compiled.PolicyID) {
				return compiledAgent{}, fail(scopePath, fmt.Sprintf("repeats scopes[%d]", j))
			}
		}
		if parent != nil && !fleet.ScopesSubset([]fleet.Scope{compiled}, parent.scopes) {
			return compiledAgent{}, fail(scopePath, fmt.Sprintf(
				"source %s policy %s is not among parent %s's scopes",
				pathValue(scope.SourceID), pathValue(scope.PolicyID), parent.label))
		}
		scopes = append(scopes, compiled)
	}

	executor, ok := c.executors[agent.ExecutorRef]
	if !ok {
		return compiledAgent{}, fail("executor_ref", fmt.Sprintf(
			"%s is not declared in the policy's executors", pathValue(agent.ExecutorRef)))
	}

	ttl, err := time.ParseDuration(agent.LeaseTTL)
	if err != nil {
		return compiledAgent{}, fail("lease_ttl", "must be a duration such as \"12h\" or \"90m\"")
	}
	if ttl <= 0 || ttl > MaxLeaseTTL {
		return compiledAgent{}, fail("lease_ttl", fmt.Sprintf(
			"must be positive and at most %s (the registry's %s less %s for clock skew)",
			MaxLeaseTTL, fleet.MaxLease, SkewMargin))
	}
	lease := c.now.Add(ttl)
	if parent != nil && lease.After(parent.lease) {
		bound := "lease_ttl " + parent.ttl.String()
		if parent.live {
			bound = "lease expiring at " + parent.lease.Format(time.RFC3339Nano)
		}
		return compiledAgent{}, fail("lease_ttl", fmt.Sprintf(
			"%s outlasts parent %s's %s", ttl, parent.label, bound))
	}

	if len(agent.Capabilities) == 0 || len(agent.Capabilities) > fleet.MaxCapabilities {
		return compiledAgent{}, fail("capabilities", fmt.Sprintf(
			"must declare between 1 and %d capabilities", fleet.MaxCapabilities))
	}
	capabilities := make([]fleet.Capability, 0, len(agent.Capabilities))
	for i, capability := range agent.Capabilities {
		capabilityPath := "capabilities" + selector("name", capability.Name, i)
		if len(capability.Actions) == 0 {
			return compiledAgent{}, fail(capabilityPath+".actions", "capability actions are required")
		}
		var parentCapability *fleet.Capability
		if parent != nil {
			parentCapability = findCapability(parent.capabilities, capability.Name)
			if parentCapability == nil {
				return compiledAgent{}, fail(capabilityPath, fmt.Sprintf(
					"parent %s has no capability %s", parent.label, pathValue(capability.Name)))
			}
		}
		compiled := fleet.Capability{Name: capability.Name}
		for j, action := range capability.Actions {
			actionPath := capabilityPath + ".actions" + selector("name", action.Name, j)
			declared, err := c.action(parent, parentCapability, action)
			if err != nil {
				var located *locatedError
				if errors.As(err, &located) {
					return compiledAgent{}, fail(actionPath+"."+located.field, located.message)
				}
				return compiledAgent{}, fail(actionPath, err.Error())
			}
			canonical, err := fleet.CanonicalCapabilities([]fleet.Capability{{
				Name: capability.Name, Actions: []fleet.Action{declared},
			}})
			if err != nil {
				return compiledAgent{}, fail(actionPath, errorMessage(err))
			}
			if err := fleet.ValidateDeclaredEffects(
				canonical, executor.MinEffects, executor.MaxEffects,
			); err != nil {
				return compiledAgent{}, fail(actionPath+".effects", fmt.Sprintf(
					"%s (executor %s declares max_effects %s, min_effects %s)",
					errorMessage(err), pathValue(executor.Ref),
					effectList(executor.MaxEffects), effectList(executor.MinEffects)))
			}
			canonicalAction := canonical[0].Actions[0]
			if parentCapability != nil {
				if err := delegatedAction(parent, parentCapability, canonical[0]); err != nil {
					var located *locatedError
					if errors.As(err, &located) && located.field != "" {
						return compiledAgent{}, fail(actionPath+"."+located.field, located.message)
					}
					return compiledAgent{}, fail(actionPath, err.Error())
				}
			}
			compiled.Actions = append(compiled.Actions, canonicalAction)
		}
		capabilities = append(capabilities, compiled)
	}

	spec, err := fleet.Spec{
		ID: id, ParentID: parentID, AuthorizationDomain: domain, Scopes: scopes,
		ExecutorRef: agent.ExecutorRef, Capabilities: capabilities, LeaseExpiresAt: lease,
	}.Canonical(c.now)
	if err != nil {
		return compiledAgent{}, fail("", errorMessage(err))
	}
	// The whole-spec composition Register applies (service.go, the
	// delegated-agent check). Every part was already checked per item above;
	// this is the backstop that keeps the two from drifting apart.
	if parent != nil && (!bytes.Equal(spec.AuthorizationDomain, parent.domain) ||
		!fleet.ScopesSubset(spec.Scopes, parent.scopes) ||
		!fleet.CapabilitiesSubset(spec.Capabilities, parent.capabilities) ||
		spec.LeaseExpiresAt.After(parent.lease)) {
		return compiledAgent{}, fail("", "delegated agent exceeds its parent "+parent.label)
	}
	return compiledAgent{spec: spec, ttl: ttl, chain: chain}, nil
}

type stringField struct {
	path  string
	value string
}

// agentStrings lists every string field of an agent with its path.
func agentStrings(agent Agent) []stringField {
	fields := []stringField{
		{"id", agent.ID}, {"parent", agent.Parent},
		{"authorization_domain", agent.AuthorizationDomain},
		{"executor_ref", agent.ExecutorRef}, {"lease_ttl", agent.LeaseTTL},
	}
	for i, scope := range agent.Scopes {
		fields = append(fields,
			stringField{fmt.Sprintf("scopes[%d].source_id", i), scope.SourceID},
			stringField{fmt.Sprintf("scopes[%d].policy_id", i), scope.PolicyID})
	}
	for i, capability := range agent.Capabilities {
		capabilityPath := fmt.Sprintf("capabilities[%d]", i)
		fields = append(fields, stringField{capabilityPath + ".name", capability.Name})
		for j, action := range capability.Actions {
			actionPath := fmt.Sprintf("%s.actions[%d]", capabilityPath, j)
			fields = append(fields, stringField{actionPath + ".name", action.Name})
			for k, effect := range action.Effects {
				fields = append(fields, stringField{fmt.Sprintf("%s.effects[%d]", actionPath, k), effect})
			}
		}
	}
	return fields
}

func (c *compiler) parent(id shoal.ID) (*parentView, error) {
	if id == "" {
		return nil, nil
	}
	label := agentPath(id)
	if compiled, ok := c.compiled[id]; ok {
		return &parentView{
			label: label, domain: compiled.spec.AuthorizationDomain,
			scopes: compiled.spec.Scopes, capabilities: compiled.spec.Capabilities,
			lease: compiled.spec.LeaseExpiresAt, ttl: compiled.ttl, chain: compiled.chain,
		}, nil
	}
	if c.live == nil {
		return nil, fmt.Errorf("%s is not declared in the policy, and no live registry "+
			"was consulted; declare it, or plan against the registry", label)
	}
	descriptor, ok := c.live[id]
	if !ok {
		return nil, fmt.Errorf("%s is neither declared in the policy nor live in the registry", label)
	}
	chain, err := c.liveChain(id)
	if err != nil {
		return nil, err
	}
	return &parentView{
		label: label + " (live)", domain: descriptor.AuthorizationDomain,
		scopes: descriptor.Scopes, capabilities: descriptor.Capabilities,
		lease: descriptor.LeaseExpiresAt, live: true, chain: chain,
	}, nil
}

// liveChain re-composes Register's activeChain over the live registrations a
// policy delegates from: it walks from id to the root, refusing an ancestor
// that is missing, revoked or expired, a cycle, a chain at the depth bound,
// and any link that no longer narrows its parent (validateDelegationChain).
// It returns the chain's length.
func (c *compiler) liveChain(id shoal.ID) (int, error) {
	chain := make([]fleet.Descriptor, 0, fleet.MaxDelegationDepth)
	seen := make(map[shoal.ID]struct{}, fleet.MaxDelegationDepth)
	for current := id; ; {
		if len(chain) == fleet.MaxDelegationDepth {
			return 0, fmt.Errorf("the live delegation chain above %s exceeds %d registrations",
				agentPath(id), fleet.MaxDelegationDepth)
		}
		if _, cycle := seen[current]; cycle {
			return 0, fmt.Errorf("the live delegation chain above %s forms a cycle", agentPath(id))
		}
		seen[current] = struct{}{}
		descriptor, ok := c.live[current]
		if !ok {
			return 0, fmt.Errorf("live ancestor %s is not visible in the registry; "+
				"the registry refuses delegation through it", agentPath(current))
		}
		if !descriptor.RevokedAt.IsZero() || !c.now.Before(descriptor.LeaseExpiresAt) {
			return 0, fmt.Errorf("live ancestor %s is revoked or expired; "+
				"the registry refuses delegation through it", agentPath(current))
		}
		chain = append(chain, descriptor)
		if descriptor.ParentID == "" {
			break
		}
		current = descriptor.ParentID
	}
	for i := 0; i+1 < len(chain); i++ {
		child, parent := chain[i], chain[i+1]
		reason := exceeds(linkFromDescriptor(child), linkFromDescriptor(parent))
		if reason == "" && child.Subject != parent.Subject {
			reason = "subjects differ"
		}
		if reason != "" {
			return 0, fmt.Errorf("live link from %s to %s no longer narrows (%s); "+
				"the registry refuses delegation through it",
				agentPath(child.ID), agentPath(parent.ID), reason)
		}
	}
	return len(chain), nil
}

// locatedError is a refusal about one field of an action.
type locatedError struct {
	field   string
	message string
}

func (e *locatedError) Error() string { return e.field + ": " + e.message }

func located(field, message string) error {
	return &locatedError{field: field, message: message}
}

// action resolves one file action to the registry's form: inherited actions
// copy the parent's exactly, approval requirement included, and may add one;
// declared ones have each effect class checked so an unknown one is named.
func (c *compiler) action(
	parent *parentView, parentCapability *fleet.Capability, action Action,
) (fleet.Action, error) {
	// Decode refuses "required": false; a document built in code is held to
	// the same rule, so no policy, however it was produced, carries a spelling
	// that reads as switching a requirement off.
	if action.Approval != nil && !action.Approval.Required {
		return fleet.Action{}, located("approval.required", approvalNotRequired)
	}
	requiresApproval := action.Approval != nil
	if action.Attestation != nil && !action.Attestation.Required {
		return fleet.Action{}, located("attestation.required", attestationNotRequired)
	}
	requiresAttestation := action.Attestation != nil
	if action.Inherit {
		if parent == nil {
			return fleet.Action{}, located("inherit", "requires a parent to inherit from")
		}
		inherited := findAction(parentCapability, action.Name)
		if inherited == nil {
			return fleet.Action{}, located("inherit", fmt.Sprintf(
				"parent %s has no action %s in this capability", parent.label, pathValue(action.Name)))
		}
		return fleet.Action{
			Name:         action.Name,
			Effects:      append(fleet.Effects(nil), inherited.Effects...),
			InputSchema:  append(json.RawMessage(nil), inherited.InputSchema...),
			OutputSchema: append(json.RawMessage(nil), inherited.OutputSchema...),
			// The parent's requirement is inherited, never dropped; the
			// child may add one the parent lacks.
			RequiresApproval: inherited.RequiresApproval || requiresApproval,
			// Likewise attestation: inherited, never dropped, may be added.
			RequiresAttestation: inherited.RequiresAttestation || requiresAttestation,
		}, attestationNeedsExternal(inherited.Effects, requiresAttestation)
	}
	effects := make(fleet.Effects, 0, len(action.Effects))
	for _, effect := range action.Effects {
		if _, err := fleet.CanonicalEffects(fleet.Effects{fleet.Effect(effect)}); err != nil {
			return fleet.Action{}, located("effects", fmt.Sprintf(
				"%q is not a known effect class (%s)", effect, knownEffects))
		}
		effects = append(effects, fleet.Effect(effect))
	}
	return fleet.Action{
		Name: action.Name, Effects: effects,
		InputSchema:         append(json.RawMessage(nil), action.InputSchema...),
		OutputSchema:        append(json.RawMessage(nil), action.OutputSchema...),
		RequiresApproval:    requiresApproval,
		RequiresAttestation: requiresAttestation,
	}, attestationNeedsExternal(effects, requiresAttestation)
}

// attestationNeedsExternal is the registry's refusal (canonicalCapabilities)
// located on the field, so the policy author sees which declaration to fix.
func attestationNeedsExternal(effects fleet.Effects, required bool) error {
	if required && !containsEffect(effects, fleet.EffectMutatesExternal) {
		return located("attestation", "requires the action to declare the external effect; "+
			"attestation applies only to external work")
	}
	return nil
}

// delegatedAction re-composes capabilitiesSubset for one canonical action so
// the refusal names what differs.
func delegatedAction(parent *parentView, parentCapability *fleet.Capability, capability fleet.Capability) error {
	action := capability.Actions[0]
	allowed := findAction(parentCapability, action.Name)
	if allowed == nil {
		return fmt.Errorf("parent %s has no action %s in this capability",
			parent.label, pathValue(action.Name))
	}
	if !bytes.Equal(action.InputSchema, allowed.InputSchema) {
		return located("input_schema", fmt.Sprintf(
			"differs from parent %s's; a delegated action declares its parent's schema exactly", parent.label))
	}
	if !bytes.Equal(action.OutputSchema, allowed.OutputSchema) {
		return located("output_schema", fmt.Sprintf(
			"differs from parent %s's; a delegated action declares its parent's schema exactly", parent.label))
	}
	for _, effect := range action.Effects {
		if !containsEffect(allowed.Effects, effect) {
			return located("effects", fmt.Sprintf(
				"%s is not among parent %s's effects %s", effect, parent.label, effectList(allowed.Effects)))
		}
	}
	// capabilitiesSubset: a delegate may add approval, never drop its
	// parent's. Declare it, or inherit the action.
	if allowed.RequiresApproval && !action.RequiresApproval {
		return located("approval", fmt.Sprintf(
			"is required by parent %s's action; a delegated action keeps its parent's "+
				"approval requirement (declare \"approval\": {\"required\": true}, or inherit the action)",
			parent.label))
	}
	// The same for attestation.
	if allowed.RequiresAttestation && !action.RequiresAttestation {
		return located("attestation", fmt.Sprintf(
			"is required by parent %s's action; a delegated action keeps its parent's "+
				"attestation requirement (declare \"attestation\": {\"required\": true}, or inherit the action)",
			parent.label))
	}
	if !fleet.CapabilitiesSubset([]fleet.Capability{capability}, parent.capabilities) {
		return fmt.Errorf("exceeds parent %s", parent.label)
	}
	return nil
}

func compileEffects(file, path string, declared []string) (fleet.Effects, error) {
	effects := make(fleet.Effects, 0, len(declared))
	for _, effect := range declared {
		if _, err := fleet.CanonicalEffects(fleet.Effects{fleet.Effect(effect)}); err != nil {
			return nil, refuse(file, path, fmt.Sprintf(
				"%q is not a known effect class (%s)", effect, knownEffects))
		}
		effects = append(effects, fleet.Effect(effect))
	}
	canonical, err := fleet.CanonicalEffects(effects)
	if err != nil {
		return nil, refuse(file, path, errorMessage(err))
	}
	return canonical, nil
}

func findCapability(capabilities []fleet.Capability, name string) *fleet.Capability {
	for i := range capabilities {
		if capabilities[i].Name == name {
			return &capabilities[i]
		}
	}
	return nil
}

func findAction(capability *fleet.Capability, name string) *fleet.Action {
	if capability == nil {
		return nil
	}
	for i := range capability.Actions {
		if capability.Actions[i].Name == name {
			return &capability.Actions[i]
		}
	}
	return nil
}

func containsEffect(effects fleet.Effects, effect fleet.Effect) bool {
	for _, candidate := range effects {
		if candidate == effect {
			return true
		}
	}
	return false
}

func effectList(effects fleet.Effects) string {
	return "[" + strings.Join(effectStrings(effects), " ") + "]"
}

func agentPath(id shoal.ID) string {
	return "agents" + selector("id", string(id), 0)
}

func trimmed(value string) string { return strings.TrimSpace(value) }

// errorMessage returns a registry refusal's message without its code prefix,
// so it reads naturally after a path.
func errorMessage(err error) string {
	var shoalErr *shoal.Error
	if errors.As(err, &shoalErr) && shoalErr.Message != "" {
		return shoalErr.Message
	}
	return err.Error()
}
