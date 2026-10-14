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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Export writes live registrations back as a policy document.
//
// Every action is written out in full, never as inherit, so the export reads
// on its own.
//
// Each lease TTL is the lease remaining at now, the one export time, rounded to
// the second, then clamped to MaxLeaseTTL and to the parent's exported TTL.
// That is what the live fleet holds at export time, not the TTL each agent was
// first registered with: heartbeats move leases, and a heartbeat can leave a
// parent with less remaining than its child was registered with. Measuring
// every agent from one instant keeps each child within its parent, so a live
// fleet always exports to a policy that compiles. The clamp to the parent does
// two things: it absorbs rounding, and it absorbs a real inversion, where a
// parent's own heartbeat left it with less lease remaining than its child.
// In the second case the exported child TTL is shorter than what the child
// holds live.
//
// executors supplies the host's executor assertions. When it is nil there is
// nothing to read them from, so each referenced executor is exported with the
// union of its actions' declared effects as its ceiling and an empty floor: the
// narrowest assertion the live registrations are consistent with, not what the
// host actually binds. Callers should say so when they pass nil.
//
// The file format holds IDs, domains and scope identities as UTF-8 strings. A
// live value that is not UTF-8 cannot be written without changing it, so it is
// refused, naming the field.
func Export(live map[shoal.ID]fleet.Descriptor, executors []Executor, now time.Time) (Document, error) {
	if now.IsZero() {
		return Document{}, shoal.NewError(shoal.ErrorInvalidArgument, "export time is required")
	}
	document := Document{ATPL: Version, Origin: Origin}
	ids := make([]shoal.ID, 0, len(live))
	for id := range live {
		ids = append(ids, id)
	}
	if len(ids) > MaxAgents {
		return Document{}, refuse("", "agents", fmt.Sprintf("more than %d live agents", MaxAgents))
	}
	ordered, err := parentsFirst(live, ids)
	if err != nil {
		return Document{}, err
	}
	used := make(map[string]fleet.Effects)
	ttls := make(map[shoal.ID]time.Duration, len(ordered))
	for _, id := range ordered {
		descriptor := live[id]
		remaining := descriptor.LeaseExpiresAt.Sub(now)
		if remaining < time.Second {
			return Document{}, refuse("", exportPath(id)+".lease_ttl",
				"the live lease ends within a second of the export time")
		}
		ttl := remaining.Round(time.Second)
		if ttl > MaxLeaseTTL {
			ttl = MaxLeaseTTL
		}
		if parentTTL, ok := ttls[descriptor.ParentID]; ok && ttl > parentTTL {
			ttl = parentTTL
		}
		ttls[id] = ttl
		agent, err := exportAgent(descriptor, ttl)
		if err != nil {
			return Document{}, err
		}
		document.Agents = append(document.Agents, agent)
		effects := used[descriptor.ExecutorRef]
		for _, capability := range descriptor.Capabilities {
			for _, action := range capability.Actions {
				effects = append(effects, action.Effects...)
			}
		}
		used[descriptor.ExecutorRef] = effects
	}

	if executors == nil {
		refs := make([]string, 0, len(used))
		for ref := range used {
			refs = append(refs, ref)
		}
		sort.Strings(refs)
		for _, ref := range refs {
			ceiling, err := fleet.CanonicalEffects(used[ref])
			if err != nil {
				return Document{}, refuse("", "executors"+selector("ref", ref, 0), errorMessage(err))
			}
			document.Executors = append(document.Executors, Executor{
				Ref: ref, MaxEffects: effectStrings(ceiling), MinEffects: []string{},
			})
		}
		return document, nil
	}
	declared := make(map[string]struct{}, len(executors))
	for _, executor := range executors {
		declared[executor.Ref] = struct{}{}
		document.Executors = append(document.Executors, Executor{
			Ref:        executor.Ref,
			MaxEffects: append([]string{}, executor.MaxEffects...),
			MinEffects: append([]string{}, executor.MinEffects...),
		})
	}
	sort.Slice(document.Executors, func(i, j int) bool {
		return document.Executors[i].Ref < document.Executors[j].Ref
	})
	refs := make([]string, 0, len(used))
	for ref := range used {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	for _, ref := range refs {
		if _, ok := declared[ref]; !ok {
			return Document{}, refuse("", "executors", fmt.Sprintf(
				"live agents reference executor %s, which the executor manifest does not declare",
				pathValue(ref)))
		}
	}
	return document, nil
}

// parentsFirst orders live IDs so every parent precedes its children, then by
// ID, matching the order Compile emits.
func parentsFirst(live map[shoal.ID]fleet.Descriptor, ids []shoal.ID) ([]shoal.ID, error) {
	depth := make(map[shoal.ID]int, len(ids))
	for _, id := range ids {
		steps := 0
		current := live[id]
		for current.ParentID != "" {
			parent, ok := live[current.ParentID]
			if !ok {
				break
			}
			steps++
			if steps > len(ids) {
				return nil, refuse("", exportPath(id)+".parent", "live delegation forms a cycle")
			}
			current = parent
		}
		depth[id] = steps
	}
	sort.Slice(ids, func(i, j int) bool {
		if depth[ids[i]] != depth[ids[j]] {
			return depth[ids[i]] < depth[ids[j]]
		}
		return shoal.CompareID(ids[i], ids[j]) < 0
	})
	return ids, nil
}

func exportAgent(descriptor fleet.Descriptor, ttl time.Duration) (Agent, error) {
	path := exportPath(descriptor.ID)
	text := func(field string, value []byte) (string, error) {
		if !utf8.Valid(value) {
			return "", refuse("", path+"."+field,
				"is not valid UTF-8 and cannot be written to a policy file without changing it")
		}
		return string(value), nil
	}
	id, err := text("id", []byte(descriptor.ID))
	if err != nil {
		return Agent{}, err
	}
	parent, err := text("parent", []byte(descriptor.ParentID))
	if err != nil {
		return Agent{}, err
	}
	domain, err := text("authorization_domain", descriptor.AuthorizationDomain)
	if err != nil {
		return Agent{}, err
	}
	executorRef, err := text("executor_ref", []byte(descriptor.ExecutorRef))
	if err != nil {
		return Agent{}, err
	}
	agent := Agent{
		ID: id, Parent: parent, AuthorizationDomain: domain,
		ExecutorRef: executorRef, LeaseTTL: ttl.String(),
	}
	for i, scope := range descriptor.Scopes {
		source, err := text(fmt.Sprintf("scopes[%d].source_id", i), scope.SourceID)
		if err != nil {
			return Agent{}, err
		}
		policy, err := text(fmt.Sprintf("scopes[%d].policy_id", i), scope.PolicyID)
		if err != nil {
			return Agent{}, err
		}
		agent.Scopes = append(agent.Scopes, Scope{SourceID: source, PolicyID: policy})
	}
	for _, capability := range descriptor.Capabilities {
		exported := Capability{Name: capability.Name}
		for _, action := range capability.Actions {
			written := Action{
				Name:         action.Name,
				Effects:      effectStrings(action.Effects),
				InputSchema:  append(json.RawMessage(nil), action.InputSchema...),
				OutputSchema: append(json.RawMessage(nil), action.OutputSchema...),
			}
			// Written whenever the live action holds it. An export that left
			// it out would be a policy that, re-applied, describes the agent
			// without its control; plan would refuse that as a widening, but
			// the file itself would misstate the fleet.
			if action.RequiresApproval {
				written.Approval = &Approval{Required: true}
			}
			// Likewise: a re-applied export without it would describe the
			// agent without its control.
			if action.RequiresAttestation {
				written.Attestation = &Attestation{Required: true}
			}
			exported.Actions = append(exported.Actions, written)
		}
		agent.Capabilities = append(agent.Capabilities, exported)
	}
	return agent, nil
}

// exportPath names a live agent in a refusal. A non-UTF-8 ID is shown as
// unpadded base64url, the registry's wire spelling, so the operator can find
// it.
func exportPath(id shoal.ID) string {
	if utf8.ValidString(string(id)) {
		return agentPath(id)
	}
	return "agents[id(base64url)=" + base64.RawURLEncoding.EncodeToString([]byte(id)) + "]"
}
