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

package explorer

import (
	"context"

	"github.com/phrocker/shoal-oss/pkg/ontology"
)

// OntologyInterpreter is the optional read-time lens capability used after
// authorization has selected the assertions a caller may observe.
type OntologyInterpreter interface {
	InterpretAssertions(
		context.Context, []ontology.Assertion, ontology.OntologyIdentity,
	) ([]ontology.AssertionInterpretation, error)
}

func (e *Explorer) InterpretAssertions(
	ctx context.Context,
	assertions []ontology.Assertion,
	selected ontology.OntologyIdentity,
) ([]ontology.AssertionInterpretation, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if err := selected.Validate(); err != nil {
		return nil, err
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	if err := e.requireOpen(); err != nil {
		return nil, err
	}
	if err := e.requireCertainOntologyMutationLocked(); err != nil {
		return nil, err
	}
	var target ontology.OntologyVersion
	var morphisms []ontology.OntologyMorphism
	transitions := make([]ontology.OntologyTransition, 0)
	for _, record := range e.ontologyProposals {
		proposal, err := record.proposal()
		if err != nil {
			return nil, err
		}
		if proposal.State() != ontology.ProposalPublished {
			continue
		}
		if proposal.Schema().ID() != selected.SchemaID() {
			continue
		}
		if identity, _ := ontology.NewOntologyIdentity(proposal.ProposedVersion()); identity == selected {
			target = proposal.ProposedVersion()
		}
		if record.BaseVersion != nil {
			base, err := restoreOntologyVersion(proposal.Schema(), *record.BaseVersion)
			if err != nil {
				return nil, err
			}
			if identity, _ := ontology.NewOntologyIdentity(base); identity == selected {
				target = base
			}
			transition, transitionErr := ontology.NewOntologyTransition(
				base, proposal.ProposedVersion(), proposal.Morphisms())
			if transitionErr == nil {
				transitions = append(transitions, transition)
			}
		}
		morphisms = append(morphisms, proposal.Morphisms()...)
	}
	out := make([]ontology.AssertionInterpretation, 0, len(assertions))
	if target.ID() == "" {
		for _, assertion := range assertions {
			out = append(out, ontology.ReadAssertionUnder(assertion, selected))
		}
		return out, nil
	}
	lens, err := ontology.NewOntologyLensWithTransitions(
		target, transitions, morphisms)
	if err != nil {
		return nil, err
	}
	for _, assertion := range assertions {
		out = append(out, lens.Read(assertion))
	}
	return out, nil
}
