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
	"fmt"
	"sort"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Write is one registration apply makes. An update written in two steps
// appears twice: Step 1 of 2 with its lease clamped to its parent's live
// lease, Step 2 of 2 with its full lease.
type Write struct {
	Entry
	Step  int
	Steps int
}

// orderWrites chooses the order apply registers a plan in, so that every
// state between two writes is one the registry accepts and resolves.
//
// Creates come last, parents first: a created agent's parent is either live
// and not rewritten, or rewritten earlier in the same apply.
//
// Updates are ordered pair by pair, for each child updated together with its
// parent:
//
//	(a) the child's new registration, lease included, fits within the
//	    parent's live one: write the child first;
//	(b) else the child's live registration fits within the parent's new one:
//	    write the parent first;
//	(c) else only the lease stands in the way of (a), since a narrowed child
//	    always fits its parent's content: write the child with its lease
//	    clamped to the parent's live lease, then the parent, then the child
//	    again with its full lease;
//	(d) otherwise no order exists, and the child is refused.
//
// The pairwise choices form a forest of constraints, so they never conflict;
// the result is sorted topologically. Then the whole sequence is replayed
// against the live state, checking every delegation link each write touches,
// in both directions. That replay, not the pairwise rule, is what admits a
// plan: a write that would leave any link exceeding its parent, even briefly,
// refuses the plan.
//
// The order depends on leases, so on time and on heartbeats. A plan reviewed
// long before apply may order differently by then; the order is part of the
// plan digest, so apply refuses rather than writing an order nobody reviewed.
func orderWrites(plan *Plan, live map[shoal.ID]fleet.Descriptor) {
	plan.writes = nil
	if len(plan.Refusals()) > 0 {
		return
	}
	position := make(map[shoal.ID]int, len(plan.Entries))
	for i, entry := range plan.Entries {
		position[entry.ID] = i
	}
	updated := func(id shoal.ID) bool {
		i, ok := position[id]
		return ok && plan.Entries[i].Kind.Updates()
	}
	refuse := func(id shoal.ID, reason string) {
		entry := &plan.Entries[position[id]]
		entry.Kind = KindRefusedDelegation
		entry.Reason = reason
		plan.writes = nil
	}

	// Pairwise choices.
	type pairChoice int
	const (
		childFirst pairChoice = iota
		parentFirst
		twoStep
	)
	choices := make(map[shoal.ID]pairChoice)
	clamps := make(map[shoal.ID]time.Time)
	for _, entry := range plan.Entries {
		parentID := entry.Spec.ParentID
		if !entry.Kind.Updates() || parentID == "" || !updated(parentID) {
			continue
		}
		parent := plan.Entries[position[parentID]]
		childNew, childLive := linkFromSpec(entry.Spec), linkFromDescriptor(live[entry.ID])
		parentNew, parentLive := linkFromSpec(parent.Spec), linkFromDescriptor(live[parentID])
		switch {
		case exceeds(childNew, parentLive) == "":
			choices[entry.ID] = childFirst
		case exceeds(childLive, parentNew) == "":
			choices[entry.ID] = parentFirst
		case exceeds(withLease(childNew, parentLive.lease), parentLive) == "" &&
			parentLive.lease.Before(childNew.lease):
			choices[entry.ID] = twoStep
			clamps[entry.ID] = parentLive.lease
		default:
			refuse(entry.ID, fmt.Sprintf("no write order keeps the link to parent %s valid: "+
				"the new registration exceeds the parent's live one (%s) and the live one "+
				"exceeds the parent's new one (%s)", agentPath(parentID),
				exceeds(childNew, parentLive), exceeds(childLive, parentNew)))
			return
		}
	}

	// Nodes: every update has a start; a two-step update also has an end.
	type node struct {
		id   shoal.ID
		step int
	}
	start := func(id shoal.ID) node { return node{id, 1} }
	end := func(id shoal.ID) node {
		if _, ok := clamps[id]; ok {
			return node{id, 2}
		}
		return node{id, 1}
	}
	var nodes []node
	edges := make(map[node][]node)
	indegree := make(map[node]int)
	edge := func(from, to node) {
		edges[from] = append(edges[from], to)
		indegree[to]++
	}
	for _, entry := range plan.Entries {
		if !entry.Kind.Updates() {
			continue
		}
		nodes = append(nodes, start(entry.ID))
		indegree[start(entry.ID)] += 0
		if end(entry.ID) != start(entry.ID) {
			nodes = append(nodes, end(entry.ID))
			edge(start(entry.ID), end(entry.ID))
		}
	}
	for child, choice := range choices {
		parent := plan.Entries[position[child]].Spec.ParentID
		switch choice {
		case childFirst:
			edge(end(child), start(parent))
		case parentFirst:
			edge(end(parent), start(child))
		case twoStep:
			edge(start(child), start(parent))
			edge(end(parent), end(child))
		}
	}
	less := func(a, b node) bool {
		if position[a.id] != position[b.id] {
			return position[a.id] < position[b.id]
		}
		return a.step < b.step
	}
	var ready []node
	for _, candidate := range nodes {
		if indegree[candidate] == 0 {
			ready = append(ready, candidate)
		}
	}
	var ordered []node
	for len(ready) > 0 {
		sort.Slice(ready, func(i, j int) bool { return less(ready[i], ready[j]) })
		next := ready[0]
		ready = ready[1:]
		ordered = append(ordered, next)
		for _, successor := range edges[next] {
			if indegree[successor]--; indegree[successor] == 0 {
				ready = append(ready, successor)
			}
		}
	}
	if len(ordered) != len(nodes) {
		// Unreachable: the constraints follow delegation edges, which form a
		// forest. Refused rather than assumed.
		for _, candidate := range nodes {
			if indegree[candidate] > 0 {
				refuse(candidate.id, "no write order satisfies every delegation link")
				return
			}
		}
	}

	var writes []Write
	for _, step := range ordered {
		entry := plan.Entries[position[step.id]]
		write := Write{Entry: entry, Step: step.step, Steps: 1}
		if clamp, ok := clamps[step.id]; ok {
			write.Steps = 2
			if step.step == 1 {
				write.Spec.LeaseExpiresAt = clamp
			} else {
				// The second write follows the first, so it expects the
				// content the first one wrote.
				write.LiveContent = ContentDigest(fleet.Descriptor{
					ID: entry.Spec.ID, ParentID: entry.Spec.ParentID,
					AuthorizationDomain: entry.Spec.AuthorizationDomain, Scopes: entry.Spec.Scopes,
					ExecutorRef: entry.Spec.ExecutorRef, Capabilities: entry.Spec.Capabilities,
				})
			}
		}
		writes = append(writes, write)
	}
	for _, entry := range plan.Entries {
		if entry.Kind == KindCreate {
			writes = append(writes, Write{Entry: entry, Step: 1, Steps: 1})
		}
	}

	// Replay against live state, checking every link each write touches.
	state := make(map[shoal.ID]link, len(live)+len(writes))
	children := make(map[shoal.ID][]shoal.ID)
	// A live agent keeps its live parent: parent migration is refused before
	// this runs. Creates add their compiled parent.
	for id, descriptor := range live {
		state[id] = linkFromDescriptor(descriptor)
		if descriptor.ParentID != "" {
			children[descriptor.ParentID] = append(children[descriptor.ParentID], id)
		}
	}
	for _, entry := range plan.Entries {
		if entry.Kind == KindCreate && entry.Spec.ParentID != "" {
			children[entry.Spec.ParentID] = append(children[entry.Spec.ParentID], entry.ID)
		}
	}
	for _, write := range writes {
		self := linkFromSpec(write.Spec)
		state[write.ID] = self
		label := agentPath(write.ID)
		if write.Steps == 2 {
			label = fmt.Sprintf("%s (step %d of 2)", label, write.Step)
		}
		if parentID := write.Spec.ParentID; parentID != "" {
			if parent, ok := state[parentID]; ok {
				if reason := exceeds(self, parent); reason != "" {
					refuse(write.ID, fmt.Sprintf("no write order keeps every link valid: writing %s "+
						"would exceed parent %s as it stands then (%s)", label, agentPath(parentID), reason))
					return
				}
			}
		}
		ids := children[write.ID]
		sort.Slice(ids, func(i, j int) bool { return shoal.CompareID(ids[i], ids[j]) < 0 })
		for _, child := range ids {
			if current, ok := state[child]; ok {
				if reason := exceeds(current, self); reason != "" {
					refuse(write.ID, fmt.Sprintf("no write order keeps every link valid: writing %s "+
						"would leave child %s exceeding it (%s)", label, agentPath(child), reason))
					return
				}
			}
		}
	}
	for id := range clamps {
		plan.Entries[position[id]].TwoStep = true
		entry := &plan.Entries[position[id]]
		entry.Changes = append(entry.Changes, Change{Op: "~", Path: "lease",
			Detail: fmt.Sprintf("written twice: first clamped to parent %s's live lease %s, "+
				"then in full after that parent's update", agentPath(entry.Spec.ParentID),
				clamps[id].Format(time.RFC3339))})
	}
	for i := range writes {
		writes[i].TwoStep = plan.Entries[position[writes[i].ID]].TwoStep
		writes[i].Changes = plan.Entries[position[writes[i].ID]].Changes
	}
	plan.writes = writes
}

func withLease(value link, lease time.Time) link {
	value.lease = lease
	return value
}
