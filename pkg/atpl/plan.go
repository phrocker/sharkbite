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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// PlanDigestPrefix namespaces plan digests. v2 binds the registry and live
// content rather than live generations, which heartbeats move.
const PlanDigestPrefix = "atpl:plan:v2:"

// LiveDigestPrefix namespaces the digest of a live registration's
// policy-relevant content.
const LiveDigestPrefix = "atpl:live:v1:"

// Kind classifies what applying a policy would do to one agent.
type Kind string

const (
	// KindCreate registers an agent the registry does not show.
	KindCreate Kind = "create"
	// KindNarrow re-registers a live agent within what it already holds.
	KindNarrow Kind = "narrow"
	// KindExecutorChange re-registers a live agent bound to a different
	// executor, possibly narrowing it too. The registry treats it as an
	// update like any other, but it changes what runs the agent's actions, so
	// plan output marks it apart from a narrowing.
	KindExecutorChange Kind = "executor-change"
	// KindUnchanged leaves a live agent as it is. Leases are not compared:
	// they are runtime state that heartbeats maintain.
	KindUnchanged Kind = "unchanged"
	// KindRefusedWidening would give a live agent more than it holds, which
	// the registry denies.
	KindRefusedWidening Kind = "refused-widening"
	// KindRefusedParentMigration would move a live agent to another parent,
	// which the registry denies.
	KindRefusedParentMigration Kind = "refused-parent-migration"
	// KindRefusedDelegation would leave a delegation link exceeding its
	// parent at some point during or after apply: a write against a parent as
	// it stands when the write lands, or a rewrite that a live child this plan
	// does not rewrite would then exceed. The registry refuses the first and
	// silently hides the child in the second.
	KindRefusedDelegation Kind = "refused-delegation"
	// KindUnmanaged is a live agent the policy does not declare. Apply leaves
	// it alone; it does not revoke.
	KindUnmanaged Kind = "unmanaged"
)

// Symbol is the marker plan output uses for the kind.
func (k Kind) Symbol() string {
	switch k {
	case KindCreate:
		return "+"
	case KindNarrow:
		return "~"
	case KindExecutorChange:
		return "*"
	case KindUnchanged:
		return "="
	case KindUnmanaged:
		return "?"
	default:
		return "!"
	}
}

// Refused reports whether the kind blocks apply.
func (k Kind) Refused() bool {
	return k == KindRefusedWidening || k == KindRefusedParentMigration ||
		k == KindRefusedDelegation
}

// Writes reports whether apply registers the agent.
func (k Kind) Writes() bool { return k == KindCreate || k.Updates() }

// Updates reports whether apply re-registers a live agent.
func (k Kind) Updates() bool { return k == KindNarrow || k == KindExecutorChange }

// Change is one field-level difference: "+" added, "-" removed, "~" changed.
type Change struct {
	Op     string
	Path   string
	Detail string
}

// Entry is the plan for one agent.
type Entry struct {
	ID shoal.ID
	// Depth is the agent's depth in the policy's own delegation forest.
	Depth int
	Kind  Kind
	// LiveGeneration is the generation the plan saw. Apply writes against the
	// generation it reads when it applies, not this one; see LiveContent.
	LiveGeneration int64
	// LiveContent is ContentDigest of the live registration, or "" when there
	// is none.
	LiveContent string
	// Spec is the compiled registration; zero for an unmanaged agent.
	Spec    fleet.Spec
	Reason  string
	Changes []Change
	// TwoStep reports that apply writes this update twice: first with its
	// lease clamped to its parent's live lease, then, once the parent's own
	// update has landed, with its full lease. See orderWrites.
	TwoStep bool
}

// Plan is the difference between a compiled policy and the live registry.
type Plan struct {
	PolicyDigest string
	Registry     string
	Entries      []Entry
	// Digest identifies the plan: the policy digest, the registry it was
	// computed against, each managed agent's kind and live content, and the
	// IDs of unmanaged agents. Generations, leases and update times are left
	// out because heartbeats move them without changing anything a policy
	// governs. The write order is bound too, since it is part of what was
	// reviewed. Apply recomputes the digest and refuses on a mismatch, so it
	// only writes a plan whose substance was reviewed.
	Digest string

	writes []Write
}

// Refusals returns the entries that block apply.
func (p Plan) Refusals() []Entry {
	var result []Entry
	for _, entry := range p.Entries {
		if entry.Kind.Refused() {
			result = append(result, entry)
		}
	}
	return result
}

// Writes returns the registrations apply makes, in order. It is empty when
// the plan has refusals. See orderWrites for how the order is chosen.
func (p Plan) Writes() []Write {
	result := make([]Write, len(p.writes))
	for i, write := range p.writes {
		result[i] = write
		result[i].Spec = cloneSpec(write.Spec)
	}
	return result
}

// Diff compares a compiled policy with the live registry.
//
// live is what the caller can see. The registry lists only active
// registrations, so a revoked or expired agent is invisible: a policy agent
// with such an ID plans as a create, and the registry then refuses it, because
// a revoked or expired ID can never be registered again.
//
// registry names where live was read; it is bound into the plan digest so a
// plan reviewed against one registry cannot be applied to another.
func Diff(policy *Policy, live map[shoal.ID]fleet.Descriptor, registry string) Plan {
	specs := policy.Agents()
	plan := Plan{
		PolicyDigest: policy.Digest(), Registry: registry,
		Entries: make([]Entry, 0, len(specs)+len(live)),
	}
	managed := make(map[shoal.ID]int, len(specs))
	for _, spec := range specs {
		entry := Entry{ID: spec.ID, Spec: spec}
		if spec.ParentID != "" {
			if position, ok := managed[spec.ParentID]; ok {
				entry.Depth = plan.Entries[position].Depth + 1
			}
		}
		current, exists := live[spec.ID]
		ttl, _ := policy.LeaseTTL(spec.ID)
		switch {
		case !exists:
			entry.Kind = KindCreate
			entry.Changes = creationChanges(spec, ttl)
		case current.ParentID != spec.ParentID:
			entry.Kind = KindRefusedParentMigration
			entry.Reason = fmt.Sprintf("live parent is %s, policy parent is %s; the registry denies parent migration",
				parentLabel(current.ParentID), parentLabel(spec.ParentID))
		default:
			if widening := wideningChanges(spec, current); len(widening) > 0 {
				entry.Kind = KindRefusedWidening
				entry.Reason = "the registry only narrows a live agent; widening requires a new agent ID"
				entry.Changes = widening
			} else if narrowing := narrowingChanges(spec, current); len(narrowing) > 0 {
				entry.Kind = KindNarrow
				if spec.ExecutorRef != current.ExecutorRef {
					entry.Kind = KindExecutorChange
				}
				entry.Changes = narrowing
			} else {
				entry.Kind = KindUnchanged
			}
		}
		if exists {
			entry.LiveGeneration = current.Generation
			entry.LiveContent = ContentDigest(current)
		}
		managed[spec.ID] = len(plan.Entries)
		plan.Entries = append(plan.Entries, entry)
	}

	// What each agent will be after apply: its compiled spec if apply writes
	// it, otherwise what is live.
	effective := make(map[shoal.ID]link, len(specs)+len(live))
	for id, descriptor := range live {
		effective[id] = linkFromDescriptor(descriptor)
	}
	for _, entry := range plan.Entries {
		if entry.Kind.Writes() {
			effective[entry.ID] = linkFromSpec(entry.Spec)
		}
	}
	children := make(map[shoal.ID][]shoal.ID)
	for id, descriptor := range live {
		if descriptor.ParentID != "" {
			children[descriptor.ParentID] = append(children[descriptor.ParentID], id)
		}
	}
	kinds := make(map[shoal.ID]Kind, len(plan.Entries))
	for _, entry := range plan.Entries {
		kinds[entry.ID] = entry.Kind
	}
	for index := range plan.Entries {
		entry := &plan.Entries[index]
		if !entry.Kind.Writes() {
			continue
		}
		self := effective[entry.ID]
		if parentID := entry.Spec.ParentID; parentID != "" {
			if parent, ok := effective[parentID]; ok {
				if reason := exceeds(self, parent); reason != "" {
					entry.Kind = KindRefusedDelegation
					entry.Reason = fmt.Sprintf("would exceed parent %s after apply: %s",
						agentPath(parentID), reason)
					continue
				}
			}
		}
		ids := children[entry.ID]
		sort.Slice(ids, func(i, j int) bool { return shoal.CompareID(ids[i], ids[j]) < 0 })
		for _, child := range ids {
			if kinds[child].Writes() {
				continue
			}
			if reason := exceeds(effective[child], self); reason != "" {
				entry.Kind = KindRefusedDelegation
				entry.Reason = fmt.Sprintf("live child %s, which this plan does not rewrite, "+
					"would exceed this agent after apply (%s) and stop resolving",
					agentPath(child), reason)
				break
			}
		}
	}

	unmanaged := make([]shoal.ID, 0, len(live))
	for id := range live {
		if _, ok := managed[id]; !ok {
			unmanaged = append(unmanaged, id)
		}
	}
	sort.Slice(unmanaged, func(i, j int) bool { return shoal.CompareID(unmanaged[i], unmanaged[j]) < 0 })
	for _, id := range unmanaged {
		plan.Entries = append(plan.Entries, Entry{
			ID: id, Kind: KindUnmanaged, LiveGeneration: live[id].Generation,
			LiveContent: ContentDigest(live[id]),
			Reason:      "live but not declared; apply leaves it as it is",
		})
	}
	orderWrites(&plan, live)
	plan.Digest = planDigest(plan)
	return plan
}

// ContentDigest identifies what a policy governs in a live registration:
// parent, domain, scopes, executor and capabilities, approval requirements
// included. Generation, subject,
// actor, lease and update time are left out, because a heartbeat moves them
// without changing any of that. Every value is hashed as hex of its bytes,
// since a live value need not be UTF-8 and encoding/json would silently
// replace invalid bytes.
func ContentDigest(descriptor fleet.Descriptor) string {
	type action struct {
		Name    string   `json:"name"`
		Effects []string `json:"effects"`
		Input   string   `json:"input_schema"`
		Output  string   `json:"output_schema"`
		// Omitted unless set, so a registration without approval keeps the
		// content digest it had before approval existed. When set, it moves
		// the digest: a plan reviewed against an agent without the
		// requirement no longer applies once someone adds it, or removes it,
		// and apply's retry stops instead of writing over the change.
		RequiresApproval bool `json:"requires_approval,omitempty"`
		// Omitted unless set, for the same reasons.
		RequiresAttestation bool `json:"requires_attestation,omitempty"`
	}
	type capability struct {
		Name    string   `json:"name"`
		Actions []action `json:"actions"`
	}
	type scope struct {
		Source string `json:"source_id"`
		Policy string `json:"policy_id"`
	}
	text := func(value []byte) string { return hex.EncodeToString(value) }
	body := struct {
		Parent       string       `json:"parent"`
		Domain       string       `json:"authorization_domain"`
		Scopes       []scope      `json:"scopes"`
		ExecutorRef  string       `json:"executor_ref"`
		Capabilities []capability `json:"capabilities"`
	}{
		Parent: text([]byte(descriptor.ParentID)), Domain: text(descriptor.AuthorizationDomain),
		ExecutorRef: text([]byte(descriptor.ExecutorRef)),
		Scopes:      make([]scope, 0, len(descriptor.Scopes)),
	}
	for _, value := range descriptor.Scopes {
		body.Scopes = append(body.Scopes, scope{text(value.SourceID), text(value.PolicyID)})
	}
	for _, value := range descriptor.Capabilities {
		hashed := capability{Name: text([]byte(value.Name)), Actions: make([]action, 0, len(value.Actions))}
		for _, declared := range value.Actions {
			effects := make([]string, 0, len(declared.Effects))
			for _, effect := range declared.Effects {
				effects = append(effects, text([]byte(effect)))
			}
			hashed.Actions = append(hashed.Actions, action{
				Name: text([]byte(declared.Name)), Effects: effects,
				Input: text(declared.InputSchema), Output: text(declared.OutputSchema),
				RequiresApproval:    declared.RequiresApproval,
				RequiresAttestation: declared.RequiresAttestation,
			})
		}
		body.Capabilities = append(body.Capabilities, hashed)
	}
	encoded, _ := json.Marshal(body)
	sum := sha256.Sum256(encoded)
	return LiveDigestPrefix + hex.EncodeToString(sum[:])
}

func planDigest(plan Plan) string {
	type managedEntry struct {
		ID      string `json:"id"`
		Kind    Kind   `json:"kind"`
		Live    string `json:"live"`
		TwoStep bool   `json:"two_step"`
	}
	body := struct {
		Policy    string         `json:"policy"`
		Registry  string         `json:"registry"`
		Managed   []managedEntry `json:"managed"`
		Unmanaged []string       `json:"unmanaged"`
		Order     []string       `json:"order"`
	}{
		Policy: plan.PolicyDigest, Registry: hex.EncodeToString([]byte(plan.Registry)),
		Managed: []managedEntry{}, Unmanaged: []string{}, Order: []string{},
	}
	for _, write := range plan.writes {
		body.Order = append(body.Order, fmt.Sprintf("%s/%d/%d",
			hex.EncodeToString([]byte(write.ID)), write.Step, write.Steps))
	}
	for _, entry := range plan.Entries {
		// Hex, not the raw ID: a live ID need not be UTF-8.
		id := hex.EncodeToString([]byte(entry.ID))
		if entry.Kind == KindUnmanaged {
			body.Unmanaged = append(body.Unmanaged, id)
			continue
		}
		body.Managed = append(body.Managed, managedEntry{
			ID: id, Kind: entry.Kind, Live: entry.LiveContent, TwoStep: entry.TwoStep,
		})
	}
	encoded, _ := json.Marshal(body)
	sum := sha256.Sum256(encoded)
	return PlanDigestPrefix + hex.EncodeToString(sum[:])
}

// link is the part of a registration a delegation check reads.
type link struct {
	domain       []byte
	scopes       []fleet.Scope
	capabilities []fleet.Capability
	lease        time.Time
}

func linkFromSpec(spec fleet.Spec) link {
	return link{spec.AuthorizationDomain, spec.Scopes, spec.Capabilities, spec.LeaseExpiresAt}
}

func linkFromDescriptor(descriptor fleet.Descriptor) link {
	return link{descriptor.AuthorizationDomain, descriptor.Scopes,
		descriptor.Capabilities, descriptor.LeaseExpiresAt}
}

// exceeds reports how child exceeds parent under the registry's delegation
// conditions, or "" when it does not.
func exceeds(child, parent link) string {
	switch {
	case !bytes.Equal(child.domain, parent.domain):
		return "authorization domain differs"
	case !fleet.ScopesSubset(child.scopes, parent.scopes):
		return "scopes exceed the parent's"
	case !fleet.CapabilitiesSubset(child.capabilities, parent.capabilities):
		return "capabilities exceed the parent's"
	case child.lease.After(parent.lease):
		return fmt.Sprintf("lease %s outlasts the parent's %s",
			child.lease.Format(time.RFC3339), parent.lease.Format(time.RFC3339))
	}
	return ""
}

func parentLabel(id shoal.ID) string {
	if id == "" {
		return "none"
	}
	return agentPath(id)
}

func scopeLabel(scope fleet.Scope) string {
	return fmt.Sprintf("scopes[source_id=%s,policy_id=%s]",
		pathValue(string(scope.SourceID)), pathValue(string(scope.PolicyID)))
}

func capabilityLabel(capability string) string {
	return "capabilities" + selector("name", capability, 0)
}

func actionLabel(capability, action string) string {
	return capabilityLabel(capability) + ".actions" + selector("name", action, 0)
}

func creationChanges(spec fleet.Spec, ttl time.Duration) []Change {
	var changes []Change
	add := func(path, detail string) { changes = append(changes, Change{Op: "+", Path: path, Detail: detail}) }
	if spec.ParentID != "" {
		add("parent", agentPath(spec.ParentID))
	}
	add("authorization_domain", pathValue(string(spec.AuthorizationDomain)))
	for _, scope := range spec.Scopes {
		add(scopeLabel(scope), "")
	}
	add("executor_ref", pathValue(spec.ExecutorRef))
	add("lease_ttl", ttl.String())
	for _, capability := range spec.Capabilities {
		for _, action := range capability.Actions {
			add(actionLabel(capability.Name, action.Name), "effects "+effectList(action.Effects))
			if action.RequiresApproval {
				add(actionLabel(capability.Name, action.Name)+".approval", "required")
			}
			if action.RequiresAttestation {
				add(actionLabel(capability.Name, action.Name)+".attestation", "required")
			}
		}
	}
	return changes
}

// wideningChanges lists what spec holds that live does not. Any entry makes
// the update a widening the registry denies.
func wideningChanges(spec fleet.Spec, live fleet.Descriptor) []Change {
	var changes []Change
	if !bytes.Equal(spec.AuthorizationDomain, live.AuthorizationDomain) {
		changes = append(changes, Change{Op: "~", Path: "authorization_domain", Detail: fmt.Sprintf("%s -> %s",
			pathValue(string(live.AuthorizationDomain)), pathValue(string(spec.AuthorizationDomain)))})
	}
	for _, scope := range spec.Scopes {
		if !fleet.ScopesSubset([]fleet.Scope{scope}, live.Scopes) {
			changes = append(changes, Change{Op: "+", Path: scopeLabel(scope)})
		}
	}
	for _, capability := range spec.Capabilities {
		held := findCapability(live.Capabilities, capability.Name)
		if held == nil {
			changes = append(changes, Change{Op: "+", Path: capabilityLabel(capability.Name)})
			continue
		}
		for _, action := range capability.Actions {
			path := actionLabel(capability.Name, action.Name)
			current := findAction(held, action.Name)
			switch {
			case current == nil:
				changes = append(changes, Change{Op: "+", Path: path})
				continue
			case !bytes.Equal(action.InputSchema, current.InputSchema):
				changes = append(changes, Change{Op: "~", Path: path + ".input_schema"})
			case !bytes.Equal(action.OutputSchema, current.OutputSchema):
				changes = append(changes, Change{Op: "~", Path: path + ".output_schema"})
			}
			for _, effect := range action.Effects {
				if !containsEffect(current.Effects, effect) {
					changes = append(changes, Change{Op: "+", Path: path + ".effects", Detail: string(effect)})
				}
			}
			// Dropping a live requirement widens: the registry refuses it
			// (capabilitiesSubset), and plan says so rather than letting a
			// policy that omits approval rewrite the agent without it.
			if current.RequiresApproval && !action.RequiresApproval {
				changes = append(changes, Change{Op: "-", Path: path + ".approval",
					Detail: "required live; the policy omits it, which would remove the control"})
			}
			if current.RequiresAttestation && !action.RequiresAttestation {
				changes = append(changes, Change{Op: "-", Path: path + ".attestation",
					Detail: "required live; the policy omits it, which would remove the control"})
			}
		}
	}
	return changes
}

// narrowingChanges lists what an update that does not widen changes.
func narrowingChanges(spec fleet.Spec, live fleet.Descriptor) []Change {
	var changes []Change
	if spec.ExecutorRef != live.ExecutorRef {
		changes = append(changes, Change{Op: "~", Path: "executor_ref", Detail: fmt.Sprintf("%s -> %s",
			pathValue(live.ExecutorRef), pathValue(spec.ExecutorRef))})
	}
	for _, scope := range live.Scopes {
		if !fleet.ScopesSubset([]fleet.Scope{scope}, spec.Scopes) {
			changes = append(changes, Change{Op: "-", Path: scopeLabel(scope)})
		}
	}
	for _, capability := range live.Capabilities {
		kept := findCapability(spec.Capabilities, capability.Name)
		if kept == nil {
			changes = append(changes, Change{Op: "-", Path: capabilityLabel(capability.Name)})
			continue
		}
		for _, action := range capability.Actions {
			path := actionLabel(capability.Name, action.Name)
			current := findAction(kept, action.Name)
			if current == nil {
				changes = append(changes, Change{Op: "-", Path: path})
				continue
			}
			for _, effect := range action.Effects {
				if !containsEffect(current.Effects, effect) {
					changes = append(changes, Change{Op: "-", Path: path + ".effects", Detail: string(effect)})
				}
			}
			// Adding a requirement narrows: the registry accepts it as an
			// update. It holds every new request for this action, and the
			// new generation stops records queued before it from resolving.
			if current.RequiresApproval && !action.RequiresApproval {
				changes = append(changes, Change{Op: "+", Path: path + ".approval", Detail: "required"})
			}
			// Adding attestation narrows too: only new claims are affected,
			// though registering it is a new generation (see docs/atpl.md).
			if current.RequiresAttestation && !action.RequiresAttestation {
				changes = append(changes, Change{Op: "+", Path: path + ".attestation", Detail: "required"})
			}
		}
	}
	return changes
}
