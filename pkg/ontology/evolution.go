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

import "github.com/phrocker/shoal-oss/pkg/shoal"

func validateProposalEvolution(
	base, proposed OntologyVersion,
	morphisms []OntologyMorphism,
) error {
	if err := base.Validate(); err != nil {
		return err
	}
	if err := proposed.Validate(); err != nil {
		return err
	}
	if base.Schema().ID() != proposed.Schema().ID() {
		return invalid("proposal versions belong to different schemas")
	}

	targetProperties := make(map[shoal.ID]PropertyDefinition, len(proposed.properties))
	for _, property := range proposed.properties {
		targetProperties[property.ID()] = property
	}
	for _, property := range base.properties {
		target, retained := targetProperties[property.ID()]
		mapped, explicit, err := mappedDefinitionTargets(
			property.ID(), "property", morphisms)
		if err != nil {
			return err
		}
		if explicit && !identityOnlyMapping(property.ID(), mapped) {
			if !allDefinitionsExist(proposed, mapped) {
				return invalid("property morphism targets are absent from proposed version")
			}
			continue
		}
		if !retained {
			return invalid("removed property requires an explicit morphism")
		}
		if property.canonical() != target.canonical() {
			return invalid(
				"retained property changed meaning without a supported morphism")
		}
	}

	targetConcepts := make(map[shoal.ID]ConceptDefinition, len(proposed.concepts))
	for _, concept := range proposed.concepts {
		targetConcepts[concept.ID()] = concept
	}
	for _, concept := range base.concepts {
		target, retained := targetConcepts[concept.ID()]
		mapped, explicit, err := mappedDefinitionTargets(
			concept.ID(), "concept", morphisms)
		if err != nil {
			return err
		}
		if explicit && !identityOnlyMapping(concept.ID(), mapped) {
			if !allDefinitionsExist(proposed, mapped) {
				return invalid("concept morphism targets are absent from proposed version")
			}
			continue
		}
		if !retained {
			return invalid("removed concept requires an explicit morphism")
		}
		mappedProperties, err := mappedDefinitionSet(
			concept.properties, "property", morphisms)
		if err != nil {
			return err
		}
		if concept.key != target.key || concept.name != target.name ||
			concept.description != target.description ||
			canonicalMetadata(concept.metadata) != canonicalMetadata(target.metadata) ||
			!safePropertyAddition(
				mappedProperties, target.properties, targetProperties) {
			return invalid(
				"retained concept changed meaning without a supported morphism")
		}
	}

	targetRelationships := make(
		map[shoal.ID]RelationshipDefinition, len(proposed.relationships))
	for _, relationship := range proposed.relationships {
		targetRelationships[relationship.ID()] = relationship
	}
	for _, relationship := range base.relationships {
		target, retained := targetRelationships[relationship.ID()]
		mapped, explicit, err := mappedDefinitionTargets(
			relationship.ID(), "relationship", morphisms)
		if err != nil {
			return err
		}
		if explicit && !identityOnlyMapping(relationship.ID(), mapped) {
			if !allDefinitionsExist(proposed, mapped) {
				return invalid(
					"relationship morphism targets are absent from proposed version")
			}
			continue
		}
		if !retained {
			return invalid("removed relationship requires an explicit morphism")
		}
		mappedProperties, err := mappedDefinitionSet(
			relationship.properties, "property", morphisms)
		if err != nil {
			return err
		}
		mappedFrom, err := mappedDefinitionSet(
			relationship.fromConcepts, "concept", morphisms)
		if err != nil {
			return err
		}
		mappedTo, err := mappedDefinitionSet(
			relationship.toConcepts, "concept", morphisms)
		if err != nil {
			return err
		}
		if relationship.key != target.key ||
			relationship.name != target.name ||
			relationship.description != target.description ||
			relationship.directed != target.directed ||
			canonicalMetadata(relationship.metadata) != canonicalMetadata(target.metadata) ||
			!safePropertyAddition(
				mappedProperties, target.properties, targetProperties) {
			return invalid(
				"retained relationship changed unsupported semantics")
		}
		if equalIDs(mappedFrom, target.fromConcepts) &&
			equalIDs(mappedTo, target.toConcepts) {
			continue
		}
		required := MorphismKind("")
		switch {
		case idSubset(mappedFrom, target.fromConcepts) &&
			idSubset(mappedTo, target.toConcepts):
			required = MorphismWiden
		case idSubset(target.fromConcepts, mappedFrom) &&
			idSubset(target.toConcepts, mappedTo):
			required = MorphismNarrow
		default:
			return invalid(
				"relationship endpoint change requires separate supported morphisms")
		}

		if !hasRelationshipMorphism(morphisms, relationship.ID(), required) {
			return invalid(
				"relationship endpoint change requires an explicit morphism")
		}
	}
	if err := validatePropertyOwnershipEvolution(base, proposed, morphisms); err != nil {
		return err
	}
	return nil
}

func validatePropertyOwnershipEvolution(
	base, proposed OntologyVersion,
	morphisms []OntologyMorphism,
) error {
	for _, property := range base.properties {
		targets, _, err := mappedDefinitionTargets(
			property.ID(), "property", morphisms)
		if err != nil {
			return err
		}
		sourceOwners := propertyOwners(base, property.ID())
		mappedOwners, err := mappedOwnerSet(sourceOwners, morphisms)
		if err != nil {
			return err
		}
		for _, targetID := range targets {
			if !definitionExists(proposed, targetID) {
				continue
			}
			targetOwners := propertyOwners(proposed, targetID)
			if len(sourceOwners) == 0 && len(targetOwners) > 0 {
				return invalid(
					"unowned property cannot become owner-restricted without an explicit supported transformation")
			}
			if len(sourceOwners) > 0 && len(targetOwners) > 0 &&
				!idSubset(mappedOwners, targetOwners) {
				return invalid(
					"property ownership cannot narrow without an explicit supported transformation")
			}
		}
	}
	return nil
}

func propertyOwners(version OntologyVersion, propertyID shoal.ID) []shoal.ID {
	var owners []shoal.ID
	for _, concept := range version.concepts {
		if containsID(concept.properties, propertyID) {
			owners = append(owners, concept.ID())
		}
	}
	for _, relationship := range version.relationships {
		if containsID(relationship.properties, propertyID) {
			owners = append(owners, relationship.ID())
		}
	}
	return canonicalUniqueIDs(owners)
}

func mappedOwnerSet(
	owners []shoal.ID,
	morphisms []OntologyMorphism,
) ([]shoal.ID, error) {
	var mapped []shoal.ID
	for _, owner := range owners {
		namespace := IDNamespace(owner)
		if namespace != "concept" && namespace != "relationship" {
			return nil, invalid("property owner has an unexpected namespace")
		}
		targets, _, err := mappedDefinitionTargets(owner, namespace, morphisms)
		if err != nil {
			return nil, err
		}
		mapped = append(mapped, targets...)
	}
	return canonicalUniqueIDs(mapped), nil
}

func mappedDefinitionSet(
	values []shoal.ID,
	namespace string,
	morphisms []OntologyMorphism,
) ([]shoal.ID, error) {
	var mapped []shoal.ID
	for _, id := range values {
		targets, _, err := mappedDefinitionTargets(id, namespace, morphisms)
		if err != nil {
			return nil, err
		}
		mapped = append(mapped, targets...)
	}
	return canonicalUniqueIDs(mapped), nil
}

func mappedDefinitionTargets(
	id shoal.ID,
	namespace string,
	morphisms []OntologyMorphism,
) ([]shoal.ID, bool, error) {
	if IDNamespace(id) != namespace {
		return nil, false, invalid("definition has an unexpected namespace")
	}
	var matched *OntologyMorphism
	for index := range morphisms {
		morphism := &morphisms[index]
		if !containsID(morphism.sources, id) {
			continue
		}
		if matched != nil {
			return nil, false, invalid(
				"multiple morphisms map the same source definition")
		}
		matched = morphism
	}
	if matched == nil {
		return []shoal.ID{id}, false, nil
	}
	targets := canonicalizeIDs(matched.targets)
	for _, target := range targets {
		if IDNamespace(target) != namespace {
			return nil, false, invalid("morphism changes definition kind")
		}
	}
	return targets, true, nil
}

func identityOnlyMapping(source shoal.ID, targets []shoal.ID) bool {
	return len(targets) == 1 && targets[0] == source
}

func allDefinitionsExist(version OntologyVersion, ids []shoal.ID) bool {
	for _, id := range ids {
		if !definitionExists(version, id) {
			return false
		}
	}
	return true
}

func canonicalUniqueIDs(values []shoal.ID) []shoal.ID {
	ordered := canonicalizeIDs(values)
	if len(ordered) < 2 {
		return ordered
	}
	result := ordered[:0]
	for _, id := range ordered {
		if len(result) > 0 && result[len(result)-1] == id {
			continue
		}
		result = append(result, id)
	}
	return result
}

func safePropertyAddition(
	before, after []shoal.ID,
	targetProperties map[shoal.ID]PropertyDefinition,
) bool {
	if !idSubset(before, after) {
		return false
	}
	for _, propertyID := range after {
		if containsID(before, propertyID) {
			continue
		}
		property, ok := targetProperties[propertyID]
		if !ok || propertyRequiresPresence(property) {
			return false
		}
	}
	return true
}

func propertyRequiresPresence(property PropertyDefinition) bool {
	for _, constraint := range property.constraints {
		switch constraint.Kind() {
		case ConstraintRequired:
			return true
		case ConstraintMinimumCount:
			count, _ := constraint.Count()
			if count > 0 {
				return true
			}
		}
	}
	return false
}

func equalIDs(left, right []shoal.ID) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func hasRelationshipMorphism(
	morphisms []OntologyMorphism,
	relationshipID shoal.ID,
	kind MorphismKind,
) bool {
	for _, morphism := range morphisms {
		if morphism.Kind() == kind &&
			len(morphism.sources) == 1 &&
			len(morphism.targets) == 1 &&
			morphism.sources[0] == relationshipID &&
			morphism.targets[0] == relationshipID {
			return true
		}
	}
	return false
}
