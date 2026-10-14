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

package ontology

import (
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const MaxPublishedOntologyVersions = 1024

// PublishedCatalog is the canonical read-only view of governable ontology
// choices rooted at one configured version. Only published proposals on the
// unique reachable chain contribute choices; drafts, rejected proposals, and
// disconnected histories never become selectable.
type PublishedCatalog struct {
	versions []OntologyVersion
}

// NewPublishedCatalog validates and replays the durable published proposal
// chain. Publication code owns CAS; callers use this view for discovery and
// eligibility rather than implementing another publication state machine.
func NewPublishedCatalog(
	configured OntologyVersion,
	proposals []GovernedProposal,
) (PublishedCatalog, error) {
	if err := configured.Validate(); err != nil {
		return PublishedCatalog{}, err
	}
	if len(proposals) > MaxPublishedOntologyVersions {
		return PublishedCatalog{}, invalid(
			"published ontology catalog exceeds the public bound")
	}
	outgoing := make(map[shoal.ID][]GovernedProposal)
	for _, proposal := range proposals {
		if err := proposal.Validate(); err != nil {
			return PublishedCatalog{}, err
		}
		if proposal.State() != ProposalPublished ||
			proposal.Schema().ID() != configured.Schema().ID() {
			continue
		}
		baseID, ok := proposal.BaseVersionID()
		if !ok {
			continue
		}
		outgoing[baseID] = append(outgoing[baseID], proposal)
	}
	catalog := PublishedCatalog{
		versions: []OntologyVersion{configured.clone()},
	}
	active := configured
	visited := map[shoal.ID]struct{}{active.ID(): {}}
	for len(catalog.versions) <= MaxPublishedOntologyVersions {
		next := outgoing[active.ID()]
		if len(next) == 0 {
			return catalog, nil
		}
		if len(next) != 1 {
			return PublishedCatalog{}, shoal.NewError(
				shoal.ErrorConflict, "published ontology history is ambiguous")
		}
		active = next[0].ProposedVersion()
		if _, cycle := visited[active.ID()]; cycle {
			return PublishedCatalog{}, shoal.NewError(
				shoal.ErrorConflict, "published ontology history contains a cycle")
		}
		visited[active.ID()] = struct{}{}
		catalog.versions = append(catalog.versions, active.clone())
	}
	return PublishedCatalog{}, shoal.NewError(
		shoal.ErrorUnavailable, "published ontology history exceeds the public bound")
}

// Active returns the durable active tip derived from the published chain.
func (c PublishedCatalog) Active() OntologyVersion {
	if len(c.versions) == 0 {
		return OntologyVersion{}
	}
	return c.versions[len(c.versions)-1].clone()
}

// ActiveIdentity returns the active schema+version identity.
func (c PublishedCatalog) ActiveIdentity() OntologyIdentity {
	active := c.Active()
	if active.ID() == "" {
		return UnknownOntology()
	}
	identity, _ := NewOntologyIdentity(active)
	return identity
}

// Versions returns configured history followed by each published successor.
func (c PublishedCatalog) Versions() []OntologyVersion {
	versions := make([]OntologyVersion, len(c.versions))
	for index, version := range c.versions {
		versions[index] = version.clone()
	}
	return versions
}

// Identities returns the caller-selectable identities in chain order.
func (c PublishedCatalog) Identities() []OntologyIdentity {
	identities := make([]OntologyIdentity, 0, len(c.versions))
	for _, version := range c.versions {
		identity, _ := NewOntologyIdentity(version)
		identities = append(identities, identity)
	}
	return identities
}

// Contains reports exact eligibility in this governed catalog.
func (c PublishedCatalog) Contains(identity OntologyIdentity) bool {
	if !identity.Known() || identity.Validate() != nil {
		return false
	}
	for _, candidate := range c.Identities() {
		if candidate == identity {
			return true
		}
	}
	return false
}
