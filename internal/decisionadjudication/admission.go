// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisionadjudication

import (
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// validateAdmission checks local separation and corroboration structure only.
// The trusted Authority must authenticate identities, complete controller and
// report inventories, role grants, and that verified evidence actually supports
// the proposed label. Passing these rules grants no training eligibility.
func validateAdmission(policy decision.LabelPolicy, proposal decision.AdjudicationProposal, basis decision.AdjudicationBasis, adjudicator decision.BasisIdentity) error {
	deny := auth.ObjectNotFound
	if policy.Validate() != nil || proposal.Validate() != nil || basis.Validate() != nil || policy.ID() != proposal.PolicyID() || basis.Proposal().ID() != proposal.ID() || !admissionIdentityValid(adjudicator) {
		return deny()
	}
	c := basis.Config()
	judges := make(map[string]bool)
	admissionIdentityEach(adjudicator, func(id string) { judges[id] = true })
	blocked := make(map[string]bool)
	for id := range judges {
		blocked[id] = true
	}
	separate := func(identity decision.BasisIdentity) bool {
		ok := true
		admissionIdentityEach(identity, func(id string) {
			if judges[id] {
				ok = false
			}
		})
		return ok
	}
	for _, o := range c.Outcomes {
		if !separate(o.Reporter) {
			return deny()
		}
		admissionIdentityEach(o.Reporter, func(id string) { blocked[id] = true })
	}
	for _, controller := range c.SourceControllers {
		if !separate(controller) {
			return deny()
		}
		admissionIdentityEach(controller, func(id string) { blocked[id] = true })
	}
	// Node zero represents all provenance controlled by a reporter or source.
	// Connecting contaminated witnesses to it propagates exclusion through
	// otherwise apparently independent witnesses that share a controller/group.
	parents := make([]int, len(c.Witnesses)+1)
	sizes := make([]int, len(parents))
	for i := range parents {
		parents[i], sizes[i] = i, 1
	}
	find := func(i int) int {
		for parents[i] != i {
			parents[i] = parents[parents[i]]
			i = parents[i]
		}
		return i
	}
	union := func(a, b int) {
		a, b = find(a), find(b)
		if a == b {
			return
		}
		if sizes[a] < sizes[b] {
			a, b = b, a
		}
		parents[b] = a
		sizes[a] += sizes[b]
	}
	origins := make(map[shoal.ID]int)
	digests := make(map[string]int)
	identities := make(map[string]int)
	for i, witness := range c.Witnesses {
		node := i + 1
		// Byte-identical evidence is one corroboration component even if an
		// upstream registry supplied different origin aliases.
		if old, ok := digests[witness.Digest]; ok {
			union(node, old)
		} else {
			digests[witness.Digest] = node
		}
		if old, ok := origins[witness.OriginGroupID]; ok {
			union(node, old)
		} else {
			origins[witness.OriginGroupID] = node
		}
		connect := func(identity decision.BasisIdentity) bool {
			if !separate(identity) {
				return false
			}
			admissionIdentityEach(identity, func(id string) {
				if blocked[id] {
					union(node, 0)
				}
				if old, ok := identities[id]; ok {
					union(node, old)
				} else {
					identities[id] = node
				}
			})
			return true
		}
		if !connect(witness.Origin) {
			return deny()
		}
		for _, controller := range witness.ControllerIdentities {
			if !connect(controller) {
				return deny()
			}
		}
	}
	if proposal.Config().Disposition == decision.AdjudicationVerified {
		if !c.ControllersComplete {
			return deny()
		}
		components := make(map[int]bool)
		contaminated := find(0)
		for i := range c.Witnesses {
			root := find(i + 1)
			if root != contaminated {
				components[root] = true
			}
		}
		if len(components) < policy.Config().MinIndependentWitnesses {
			return deny()
		}
	}
	return nil
}

// Cross-field identity equality is deliberate: acting as somebody's delegate,
// or changing only the client, cannot establish independence from that person.
func admissionIdentityEach(identity decision.BasisIdentity, visit func(string)) {
	visit(string(identity.SubjectID))
	visit(string(identity.ActorID))
	for _, delegate := range identity.OnBehalfOf {
		visit(string(delegate))
	}
}
func admissionIdentityValid(identity decision.BasisIdentity) bool {
	if len(identity.SubjectID) == 0 || len(identity.ActorID) == 0 || len(identity.SubjectID) > shoal.MaxIDBytes || len(identity.ActorID) > shoal.MaxIDBytes || len(identity.ClientID) > shoal.MaxIDBytes || len(identity.OnBehalfOf) > auth.MaxOnBehalfOfEntries {
		return false
	}
	for _, delegate := range identity.OnBehalfOf {
		if len(delegate) == 0 || len(delegate) > shoal.MaxIDBytes {
			return false
		}
	}
	return true
}
