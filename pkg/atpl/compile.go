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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/phrocker/shoal-oss/pkg/executorref"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// DigestPrefix namespaces policy digests. The digest is the policy's identity:
// it is recorded as the reason detail of every registration apply makes.
const DigestPrefix = "atpl:policy:v1:"

// LiveParents supplies registered agents a policy may delegate from without
// declaring them. A nil value means no registry was consulted, so every parent
// must be declared in the policy.
type LiveParents map[shoal.ID]fleet.Descriptor

// ExecutorBound is a compiled executor assertion: canonical effect sets.
type ExecutorBound struct {
	Ref        string
	MaxEffects fleet.Effects
	MinEffects fleet.Effects
}

// Policy is a compiled policy: canonical registrations in apply order, their
// lease TTLs, the executor assertions they were checked against, and the
// content digest that identifies them.
type Policy struct {
	specs     []fleet.Spec
	ttls      map[shoal.ID]time.Duration
	executors []ExecutorBound
	canonical []byte
	digest    string
}

// Agents returns the compiled registrations, parents before children and
// otherwise by ID. Each is the canonical form Register stores, with an
// absolute lease of the shared compile time plus the agent's TTL.
func (p *Policy) Agents() []fleet.Spec {
	result := make([]fleet.Spec, len(p.specs))
	for i := range p.specs {
		result[i] = cloneSpec(p.specs[i])
	}
	return result
}

// LeaseTTL returns the declared TTL of a compiled agent.
func (p *Policy) LeaseTTL(id shoal.ID) (time.Duration, bool) {
	ttl, ok := p.ttls[id]
	return ttl, ok
}

// Executors returns the executor assertions, by reference.
func (p *Policy) Executors() []ExecutorBound {
	result := make([]ExecutorBound, len(p.executors))
	for i, executor := range p.executors {
		result[i] = ExecutorBound{
			Ref:        executor.Ref,
			MaxEffects: append(fleet.Effects(nil), executor.MaxEffects...),
			MinEffects: append(fleet.Effects(nil), executor.MinEffects...),
		}
	}
	return result
}

// Digest is the policy's content identity, "atpl:policy:v1:<sha256 hex>".
//
// It covers the version, the executor assertions, and every registration
// without its absolute lease but with its TTL, so the same files compile to the
// same digest at any time and in any file order. It is computed over the Go
// encoding/json form of fixed structs, as pkg/decision identities are: stable
// within this contract, not a language-independent canonical JSON.
func (p *Policy) Digest() string { return p.digest }

// CanonicalJSON returns the exact bytes the digest is computed over.
func (p *Policy) CanonicalJSON() []byte { return append([]byte(nil), p.canonical...) }

// Compile validates documents together and compiles them to registrations.
//
// Every lease is computed from the one now, so a child and its parent compare
// their TTLs exactly as the registry compares their absolute leases. The first
// refusal is returned, deterministically: files by name, executors by
// reference, agents parents first and then by ID.
func Compile(documents []Document, now time.Time, live LiveParents) (*Policy, error) {
	if now.IsZero() {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "compile time is required")
	}
	now = now.UTC().Round(0)
	if len(documents) > MaxFiles {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument,
			fmt.Sprintf("policy spans more than %d files", MaxFiles))
	}
	ordered := append([]Document(nil), documents...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].name < ordered[j].name })
	names := make(map[string]struct{}, len(ordered))
	for _, document := range ordered {
		if _, duplicate := names[document.name]; duplicate {
			return nil, refuse(document.name, "", "policy file name appears twice")
		}
		names[document.name] = struct{}{}
		if document.ATPL != Version {
			return nil, refuse(document.name, "atpl", fmt.Sprintf(
				"unsupported format version %q; this build reads %s", document.ATPL, Version))
		}
		if document.Origin != Origin {
			return nil, refuse(document.name, "origin", fmt.Sprintf(
				"must credit the format's origin verbatim: %q", Origin))
		}
	}

	executors, err := compileExecutors(ordered)
	if err != nil {
		return nil, err
	}
	declared, order, err := orderAgents(ordered)
	if err != nil {
		return nil, err
	}

	compiler := &compiler{
		now: now, executors: executors, live: live,
		declared: declared, compiled: make(map[shoal.ID]compiledAgent, len(order)),
	}
	policy := &Policy{
		ttls: make(map[shoal.ID]time.Duration, len(order)),
	}
	for _, id := range order {
		result, err := compiler.agent(declared[id])
		if err != nil {
			return nil, err
		}
		compiler.compiled[id] = result
		policy.specs = append(policy.specs, result.spec)
		policy.ttls[id] = result.ttl
	}
	refs := make([]string, 0, len(executors))
	for ref := range executors {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	for _, ref := range refs {
		policy.executors = append(policy.executors, executors[ref])
	}
	policy.canonical, err = canonicalPolicyJSON(policy)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(policy.canonical)
	policy.digest = DigestPrefix + hex.EncodeToString(sum[:])
	return policy, nil
}

// declaredAgent is a file agent with the file it came from and its depth in
// the policy's own delegation forest.
type declaredAgent struct {
	agent Agent
	file  string
	depth int
}

// orderAgents indexes agents across files, refusing duplicates and cycles, and
// returns them parents first and then by ID.
func orderAgents(documents []Document) (map[shoal.ID]declaredAgent, []shoal.ID, error) {
	declared := make(map[shoal.ID]declaredAgent)
	for _, document := range documents {
		for i, agent := range document.Agents {
			path := "agents" + selector("id", agent.ID, i)
			if agent.ID == "" {
				return nil, nil, refuse(document.name, path+".id", "agent ID is required")
			}
			if agent.Parent == agent.ID {
				return nil, nil, refuse(document.name, path+".parent",
					"an agent cannot delegate from itself")
			}
			if previous, duplicate := declared[shoal.ID(agent.ID)]; duplicate {
				return nil, nil, refuse(document.name, path,
					"agent is also declared in "+previous.file)
			}
			if len(declared) == MaxAgents {
				return nil, nil, refuse(document.name, path,
					fmt.Sprintf("policy declares more than %d agents", MaxAgents))
			}
			declared[shoal.ID(agent.ID)] = declaredAgent{agent: agent, file: document.name}
		}
	}
	ids := make([]shoal.ID, 0, len(declared))
	for id := range declared {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return shoal.CompareID(ids[i], ids[j]) < 0 })
	for _, id := range ids {
		entry := declared[id]
		depth := 0
		current := entry.agent
		for current.Parent != "" {
			parent, inPolicy := declared[shoal.ID(current.Parent)]
			if !inPolicy {
				break
			}
			depth++
			if depth > len(declared) {
				return nil, nil, refuse(entry.file, agentPath(id)+".parent",
					"delegation forms a cycle")
			}
			current = parent.agent
		}
		if depth >= fleet.MaxDelegationDepth {
			return nil, nil, refuse(entry.file, agentPath(id)+".parent", fmt.Sprintf(
				"delegation depth exceeds %d", fleet.MaxDelegationDepth))
		}
		entry.depth = depth
		declared[id] = entry
	}
	sort.SliceStable(ids, func(i, j int) bool {
		return declared[ids[i]].depth < declared[ids[j]].depth
	})
	return declared, ids, nil
}

func compileExecutors(documents []Document) (map[string]ExecutorBound, error) {
	result := make(map[string]ExecutorBound)
	files := make(map[string]string)
	for _, document := range documents {
		for i, executor := range document.Executors {
			path := "executors" + selector("ref", executor.Ref, i)
			if err := executorref.ValidExecutorRef(executor.Ref); err != nil {
				// An invalid ref is not echoed into the selector: it may be
				// invisible, or not valid UTF-8.
				return nil, refuse(document.name, "executors"+selector("", "", i)+".ref",
					err.Error())
			}
			if previous, duplicate := files[executor.Ref]; duplicate {
				return nil, refuse(document.name, path, "executor is also declared in "+previous)
			}
			if len(result) == MaxExecutors {
				return nil, refuse(document.name, path,
					fmt.Sprintf("policy declares more than %d executors", MaxExecutors))
			}
			ceiling, err := compileEffects(document.name, path+".max_effects", executor.MaxEffects)
			if err != nil {
				return nil, err
			}
			floor, err := compileEffects(document.name, path+".min_effects", executor.MinEffects)
			if err != nil {
				return nil, err
			}
			for _, required := range floor {
				if !containsEffect(ceiling, required) {
					return nil, refuse(document.name, path+".min_effects", fmt.Sprintf(
						"%s is not within max_effects; no action could register against this executor",
						required))
				}
			}
			files[executor.Ref] = document.name
			result[executor.Ref] = ExecutorBound{Ref: executor.Ref, MaxEffects: ceiling, MinEffects: floor}
		}
	}
	return result, nil
}

// canonicalPolicy is the digest body. Its field order and encoding are part
// of the digest contract; changing either requires a new DigestPrefix.
type canonicalPolicy struct {
	ATPL      string              `json:"atpl"`
	Executors []canonicalExecutor `json:"executors"`
	Agents    []canonicalAgent    `json:"agents"`
}

type canonicalExecutor struct {
	Ref        string   `json:"ref"`
	MaxEffects []string `json:"max_effects"`
	MinEffects []string `json:"min_effects"`
}

type canonicalAgent struct {
	ID                  string                `json:"id"`
	Parent              string                `json:"parent,omitempty"`
	AuthorizationDomain string                `json:"authorization_domain"`
	Scopes              []canonicalScope      `json:"scopes"`
	ExecutorRef         string                `json:"executor_ref"`
	LeaseTTLNanos       int64                 `json:"lease_ttl_ns"`
	Capabilities        []canonicalCapability `json:"capabilities"`
}

type canonicalScope struct {
	SourceID string `json:"source_id"`
	PolicyID string `json:"policy_id"`
}

type canonicalCapability struct {
	Name    string            `json:"name"`
	Actions []canonicalAction `json:"actions"`
}

type canonicalAction struct {
	Name         string          `json:"name"`
	Effects      []string        `json:"effects"`
	InputSchema  json.RawMessage `json:"input_schema"`
	OutputSchema json.RawMessage `json:"output_schema"`
	// Approval is omitted unless required, so every policy without an
	// approval requirement keeps the digest it had before approval could be
	// declared, under the same DigestPrefix, while a requirement still
	// changes the digest: two policies differing only in it never share one.
	Approval *Approval `json:"approval,omitempty"`
	// Attestation likewise: omitted unless required, so the digest of every
	// policy without it is unchanged.
	Attestation *Attestation `json:"attestation,omitempty"`
}

func canonicalPolicyJSON(policy *Policy) ([]byte, error) {
	body := canonicalPolicy{
		ATPL:      Version,
		Executors: make([]canonicalExecutor, 0, len(policy.executors)),
		Agents:    make([]canonicalAgent, 0, len(policy.specs)),
	}
	for _, executor := range policy.executors {
		body.Executors = append(body.Executors, canonicalExecutor{
			Ref:        executor.Ref,
			MaxEffects: effectStrings(executor.MaxEffects),
			MinEffects: effectStrings(executor.MinEffects),
		})
	}
	for _, spec := range policy.specs {
		agent := canonicalAgent{
			ID: string(spec.ID), Parent: string(spec.ParentID),
			AuthorizationDomain: string(spec.AuthorizationDomain),
			Scopes:              make([]canonicalScope, 0, len(spec.Scopes)),
			ExecutorRef:         spec.ExecutorRef,
			LeaseTTLNanos:       int64(policy.ttls[spec.ID]),
			Capabilities:        make([]canonicalCapability, 0, len(spec.Capabilities)),
		}
		for _, scope := range spec.Scopes {
			agent.Scopes = append(agent.Scopes, canonicalScope{
				SourceID: string(scope.SourceID), PolicyID: string(scope.PolicyID),
			})
		}
		for _, capability := range spec.Capabilities {
			compiled := canonicalCapability{
				Name: capability.Name, Actions: make([]canonicalAction, 0, len(capability.Actions)),
			}
			for _, action := range capability.Actions {
				canonical := canonicalAction{
					Name: action.Name, Effects: effectStrings(action.Effects),
					InputSchema: action.InputSchema, OutputSchema: action.OutputSchema,
				}
				if action.RequiresApproval {
					canonical.Approval = &Approval{Required: true}
				}
				if action.RequiresAttestation {
					canonical.Attestation = &Attestation{Required: true}
				}
				compiled.Actions = append(compiled.Actions, canonical)
			}
			agent.Capabilities = append(agent.Capabilities, compiled)
		}
		body.Agents = append(body.Agents, agent)
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, shoal.WrapError(shoal.ErrorInternal, "policy could not be encoded", err)
	}
	return encoded, nil
}

func effectStrings(effects fleet.Effects) []string {
	result := make([]string, 0, len(effects))
	for _, effect := range effects {
		result = append(result, string(effect))
	}
	return result
}

func cloneSpec(spec fleet.Spec) fleet.Spec {
	result := spec
	result.AuthorizationDomain = append([]byte(nil), spec.AuthorizationDomain...)
	result.Scopes = make([]fleet.Scope, len(spec.Scopes))
	for i, scope := range spec.Scopes {
		result.Scopes[i] = fleet.Scope{
			SourceID: append([]byte(nil), scope.SourceID...),
			PolicyID: append([]byte(nil), scope.PolicyID...),
		}
	}
	result.Capabilities = cloneCapabilities(spec.Capabilities)
	return result
}

func cloneCapabilities(input []fleet.Capability) []fleet.Capability {
	result := make([]fleet.Capability, len(input))
	for i, capability := range input {
		result[i].Name = capability.Name
		result[i].Actions = make([]fleet.Action, len(capability.Actions))
		for j, action := range capability.Actions {
			result[i].Actions[j] = fleet.Action{
				Name:         action.Name,
				Effects:      append(fleet.Effects(nil), action.Effects...),
				InputSchema:  append(json.RawMessage(nil), action.InputSchema...),
				OutputSchema: append(json.RawMessage(nil), action.OutputSchema...),
				// A clone without the flag would hand Agents, Writes and
				// apply a registration that silently drops the requirement.
				RequiresApproval:    action.RequiresApproval,
				RequiresAttestation: action.RequiresAttestation,
			}
		}
	}
	return result
}
