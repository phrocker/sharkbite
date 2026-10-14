// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package explorer

import (
	"context"
	"fmt"
	"time"

	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// OntologyProposalBoundedTransitionStore admits a transition only when its
// result fits the caller's projection limits, checked before persistence.
type OntologyProposalBoundedTransitionStore interface {
	TransitionOntologyProposalWithLimits(
		context.Context, shoal.ID, ontology.ProposalState,
		string, string, time.Time, OntologyProjectionLimits,
	) (ontology.GovernedProposal, error)
}

// OntologyProjectionLimits optionally narrows domain bounds for a consumer.
// A zero field leaves that domain bound unchanged.
type OntologyProjectionLimits struct {
	MaxConcepts                 uint32
	MaxRelationships            uint32
	MaxProperties               uint32
	MaxDefinitionProperties     uint32
	MaxRelationshipEndpointSets uint32
	MaxConstraintsPerProperty   uint32
	MaxAllowedValues            uint32
	MaxTransitions              uint32
	MaxMorphismEvidence         uint32
	MaxDiscriminatorChoices     uint32
}

// ValidateVersion checks the size of an immutable version without projecting it.
func (l OntologyProjectionLimits) ValidateVersion(version ontology.OntologyVersion) error {
	concepts := version.Concepts()
	relationships := version.Relationships()
	properties := version.Properties()
	for _, check := range []struct {
		name  string
		count int
		limit uint32
	}{
		{"concept", len(concepts), l.MaxConcepts},
		{"relationship", len(relationships), l.MaxRelationships},
		{"property", len(properties), l.MaxProperties},
	} {
		if err := ontologyProjectionBound(check.name, check.count, check.limit); err != nil {
			return err
		}
	}
	for _, concept := range concepts {
		if err := ontologyProjectionBound(
			"concept property reference", len(concept.Properties()), l.MaxDefinitionProperties,
		); err != nil {
			return err
		}
	}
	for _, relationship := range relationships {
		for _, check := range []struct {
			name  string
			count int
			limit uint32
		}{
			{"relationship source concept reference", len(relationship.FromConcepts()), l.MaxRelationshipEndpointSets},
			{"relationship target concept reference", len(relationship.ToConcepts()), l.MaxRelationshipEndpointSets},
			{"relationship property reference", len(relationship.Properties()), l.MaxDefinitionProperties},
		} {
			if err := ontologyProjectionBound(check.name, check.count, check.limit); err != nil {
				return err
			}
		}
	}
	for _, property := range properties {
		constraints := property.Constraints()
		if err := ontologyProjectionBound(
			"property constraint", len(constraints), l.MaxConstraintsPerProperty,
		); err != nil {
			return err
		}
		for _, constraint := range constraints {
			if err := ontologyProjectionBound(
				"allowed value", len(constraint.AllowedValues()), l.MaxAllowedValues,
			); err != nil {
				return err
			}
		}
	}
	return nil
}

// ValidateProposal checks the complete prospective mutation response.
func (l OntologyProjectionLimits) ValidateProposal(proposal ontology.GovernedProposal) error {
	if err := l.ValidateVersion(proposal.ProposedVersion()); err != nil {
		return err
	}
	if err := ontologyProjectionBound(
		"proposal transition", len(proposal.Transitions()), l.MaxTransitions,
	); err != nil {
		return err
	}
	for _, morphism := range proposal.Morphisms() {
		if err := ontologyProjectionBound(
			"morphism evidence", len(morphism.Evidence()), l.MaxMorphismEvidence,
		); err != nil {
			return err
		}
		if morphism.Kind() == ontology.MorphismSplit {
			if err := ontologyProjectionBound(
				"morphism discriminator choice",
				len(morphism.Discriminator().Choices()), l.MaxDiscriminatorChoices,
			); err != nil {
				return err
			}
		}
	}
	return nil
}

func ontologyProjectionBound(name string, count int, limit uint32) error {
	if limit == 0 || uint64(count) <= uint64(limit) {
		return nil
	}
	return shoal.NewError(shoal.ErrorUnavailable, fmt.Sprintf(
		"active ontology %s count %d exceeds max_ontology bound %d",
		name, count, limit,
	))
}
