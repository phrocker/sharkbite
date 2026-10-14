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

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/phrocker/shoal-oss/internal/strictjson"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type ontologyFileConfig struct {
	Schema        ontologySchemaConfig         `json:"schema"`
	Version       ontologyVersionConfig        `json:"version"`
	Concepts      []ontologyConceptConfig      `json:"concepts"`
	Relationships []ontologyRelationshipConfig `json:"relationships"`
	Properties    []ontologyPropertyConfig     `json:"properties"`
}

type ontologySchemaConfig struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type ontologyVersionConfig struct {
	Version   string    `json:"version"`
	CreatedAt time.Time `json:"created_at"`
}

type ontologyConceptConfig struct {
	Key         string   `json:"key"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Properties  []string `json:"properties,omitempty"`
}

type ontologyRelationshipConfig struct {
	Key          string   `json:"key"`
	Name         string   `json:"name"`
	Description  string   `json:"description,omitempty"`
	FromConcepts []string `json:"from_concepts"`
	ToConcepts   []string `json:"to_concepts"`
	Properties   []string `json:"properties,omitempty"`
	Directed     bool     `json:"directed"`
}

type ontologyPropertyConfig struct {
	Key         string                     `json:"key"`
	Name        string                     `json:"name"`
	Description string                     `json:"description,omitempty"`
	ValueType   ontology.ValueType         `json:"value_type"`
	Constraints []ontologyConstraintConfig `json:"constraints,omitempty"`
}

type ontologyConstraintConfig struct {
	Kind          ontology.ConstraintKind `json:"kind"`
	Count         *uint32                 `json:"count,omitempty"`
	Value         *ontologyValueConfig    `json:"value,omitempty"`
	Pattern       string                  `json:"pattern,omitempty"`
	AllowedValues []ontologyValueConfig   `json:"allowed_values,omitempty"`
}

type ontologyValueConfig struct {
	Type  ontology.ValueType `json:"type"`
	Value json.RawMessage    `json:"value"`
}

func loadOntologyVersionFile(path string) (ontology.OntologyVersion, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ontology.OntologyVersion{}, fmt.Errorf("read ontology file %s: %w", path, err)
	}
	// strictjson refuses duplicate keys, keys matching a field only up to
	// case, unknown fields and trailing data at every level; encoding/json
	// alone would let a case-aliased key silently override another.
	var config ontologyFileConfig
	if err := strictjson.Decode(data, &config); err != nil {
		return ontology.OntologyVersion{}, fmt.Errorf("decode ontology file %s: %w", path, err)
	}
	return ontologyVersionFromConfig(config)
}

func ontologyVersionFromConfig(
	config ontologyFileConfig,
) (ontology.OntologyVersion, error) {
	schema, err := ontology.NewOntologySchema(
		config.Schema.Key, config.Schema.Name, config.Schema.Description, nil)
	if err != nil {
		return ontology.OntologyVersion{}, fmt.Errorf("ontology schema: %w", err)
	}
	properties, propertyIDs, err := ontologyPropertiesFromConfig(config.Properties)
	if err != nil {
		return ontology.OntologyVersion{}, err
	}
	concepts, conceptIDs, err := ontologyConceptsFromConfig(
		config.Concepts, propertyIDs)
	if err != nil {
		return ontology.OntologyVersion{}, err
	}
	relationships, err := ontologyRelationshipsFromConfig(
		config.Relationships, conceptIDs, propertyIDs)
	if err != nil {
		return ontology.OntologyVersion{}, err
	}
	version, err := ontology.NewOntologyVersion(
		schema, config.Version.Version, config.Version.CreatedAt,
		concepts, relationships, properties, nil,
	)
	if err != nil {
		return ontology.OntologyVersion{}, fmt.Errorf("ontology version: %w", err)
	}
	return version, nil
}

func ontologyPropertiesFromConfig(
	configs []ontologyPropertyConfig,
) ([]ontology.PropertyDefinition, map[string]shoal.ID, error) {
	properties := make([]ontology.PropertyDefinition, 0, len(configs))
	ids := make(map[string]shoal.ID, len(configs))
	for _, config := range configs {
		constraints, err := ontologyConstraintsFromConfig(config.Constraints)
		if err != nil {
			return nil, nil, fmt.Errorf("ontology property %q constraints: %w", config.Key, err)
		}
		property, err := ontology.NewPropertyDefinition(
			config.Key, config.Name, config.Description,
			config.ValueType, constraints, nil,
		)
		if err != nil {
			return nil, nil, fmt.Errorf("ontology property %q: %w", config.Key, err)
		}
		if _, duplicate := ids[config.Key]; duplicate {
			return nil, nil, fmt.Errorf("ontology property key %q is duplicated", config.Key)
		}
		ids[config.Key] = property.ID()
		properties = append(properties, property)
	}
	return properties, ids, nil
}

func ontologyConstraintsFromConfig(
	configs []ontologyConstraintConfig,
) ([]ontology.Constraint, error) {
	constraints := make([]ontology.Constraint, 0, len(configs))
	for _, config := range configs {
		constraint, err := ontologyConstraintFromConfig(config)
		if err != nil {
			return nil, err
		}
		constraints = append(constraints, constraint)
	}
	return constraints, nil
}

func ontologyConstraintFromConfig(
	config ontologyConstraintConfig,
) (ontology.Constraint, error) {
	switch config.Kind {
	case ontology.ConstraintRequired, ontology.ConstraintUnique:
		return ontology.NewFlagConstraint(config.Kind)
	case ontology.ConstraintMinimumCount, ontology.ConstraintMaximumCount:
		if config.Count == nil {
			return ontology.Constraint{}, fmt.Errorf("%s constraint requires count", config.Kind)
		}
		return ontology.NewCountConstraint(config.Kind, *config.Count)
	case ontology.ConstraintMinimumValue, ontology.ConstraintMaximumValue:
		if config.Value == nil {
			return ontology.Constraint{}, fmt.Errorf("%s constraint requires value", config.Kind)
		}
		value, err := ontologyValueFromConfig(*config.Value)
		if err != nil {
			return ontology.Constraint{}, err
		}
		return ontology.NewValueConstraint(config.Kind, value)
	case ontology.ConstraintPattern:
		return ontology.NewPatternConstraint(config.Pattern)
	case ontology.ConstraintAllowedValues:
		values := make([]ontology.Value, 0, len(config.AllowedValues))
		for _, item := range config.AllowedValues {
			value, err := ontologyValueFromConfig(item)
			if err != nil {
				return ontology.Constraint{}, err
			}
			values = append(values, value)
		}
		return ontology.NewAllowedValuesConstraint(values)
	default:
		return ontology.Constraint{}, fmt.Errorf("unknown constraint kind %q", config.Kind)
	}
}

func ontologyValueFromConfig(config ontologyValueConfig) (ontology.Value, error) {
	switch config.Type {
	case ontology.ValueString:
		var value string
		if err := json.Unmarshal(config.Value, &value); err != nil {
			return ontology.Value{}, fmt.Errorf("string ontology value: %w", err)
		}
		return ontology.NewStringValue(value)
	case ontology.ValueInteger:
		var value json.Number
		if err := json.Unmarshal(config.Value, &value); err != nil {
			var text string
			if textErr := json.Unmarshal(config.Value, &text); textErr != nil {
				return ontology.Value{}, fmt.Errorf("integer ontology value: %w", err)
			}
			value = json.Number(text)
		}
		integer, err := value.Int64()
		if err != nil {
			return ontology.Value{}, fmt.Errorf("integer ontology value: %w", err)
		}
		return ontology.NewIntegerValue(integer), nil
	case ontology.ValueNumber:
		var value json.Number
		if err := json.Unmarshal(config.Value, &value); err != nil {
			var text string
			if textErr := json.Unmarshal(config.Value, &text); textErr != nil {
				return ontology.Value{}, fmt.Errorf("number ontology value: %w", err)
			}
			value = json.Number(text)
		}
		number, err := value.Float64()
		if err != nil {
			return ontology.Value{}, fmt.Errorf("number ontology value: %w", err)
		}
		return ontology.NewNumberValue(number)
	case ontology.ValueBoolean:
		var value bool
		if err := json.Unmarshal(config.Value, &value); err != nil {
			return ontology.Value{}, fmt.Errorf("boolean ontology value: %w", err)
		}
		return ontology.NewBooleanValue(value), nil
	case ontology.ValueTimestamp:
		var value time.Time
		if err := json.Unmarshal(config.Value, &value); err != nil {
			return ontology.Value{}, fmt.Errorf("timestamp ontology value: %w", err)
		}
		return ontology.NewTimestampValue(value)
	case ontology.ValueReference:
		var value string
		if err := json.Unmarshal(config.Value, &value); err != nil {
			return ontology.Value{}, fmt.Errorf("reference ontology value: %w", err)
		}
		return ontology.NewReferenceValue(shoal.ID(value))
	default:
		return ontology.Value{}, fmt.Errorf("unknown ontology value type %q", config.Type)
	}
}

func ontologyConceptsFromConfig(
	configs []ontologyConceptConfig,
	propertyIDs map[string]shoal.ID,
) ([]ontology.ConceptDefinition, map[string]shoal.ID, error) {
	concepts := make([]ontology.ConceptDefinition, 0, len(configs))
	ids := make(map[string]shoal.ID, len(configs))
	for _, config := range configs {
		properties, err := resolveOntologyIDs("property", config.Properties, propertyIDs)
		if err != nil {
			return nil, nil, fmt.Errorf("ontology concept %q: %w", config.Key, err)
		}
		concept, err := ontology.NewConceptDefinition(
			config.Key, config.Name, config.Description, properties, nil)
		if err != nil {
			return nil, nil, fmt.Errorf("ontology concept %q: %w", config.Key, err)
		}
		if _, duplicate := ids[config.Key]; duplicate {
			return nil, nil, fmt.Errorf("ontology concept key %q is duplicated", config.Key)
		}
		ids[config.Key] = concept.ID()
		concepts = append(concepts, concept)
	}
	return concepts, ids, nil
}

func ontologyRelationshipsFromConfig(
	configs []ontologyRelationshipConfig,
	conceptIDs map[string]shoal.ID,
	propertyIDs map[string]shoal.ID,
) ([]ontology.RelationshipDefinition, error) {
	relationships := make([]ontology.RelationshipDefinition, 0, len(configs))
	seen := make(map[string]struct{}, len(configs))
	for _, config := range configs {
		from, err := resolveOntologyIDs("concept", config.FromConcepts, conceptIDs)
		if err != nil {
			return nil, fmt.Errorf("ontology relationship %q sources: %w", config.Key, err)
		}
		to, err := resolveOntologyIDs("concept", config.ToConcepts, conceptIDs)
		if err != nil {
			return nil, fmt.Errorf("ontology relationship %q targets: %w", config.Key, err)
		}
		properties, err := resolveOntologyIDs("property", config.Properties, propertyIDs)
		if err != nil {
			return nil, fmt.Errorf("ontology relationship %q properties: %w", config.Key, err)
		}
		relationship, err := ontology.NewRelationshipDefinition(
			config.Key, config.Name, config.Description,
			from, to, properties, config.Directed, nil,
		)
		if err != nil {
			return nil, fmt.Errorf("ontology relationship %q: %w", config.Key, err)
		}
		if _, duplicate := seen[config.Key]; duplicate {
			return nil, fmt.Errorf("ontology relationship key %q is duplicated", config.Key)
		}
		seen[config.Key] = struct{}{}
		relationships = append(relationships, relationship)
	}
	return relationships, nil
}

func resolveOntologyIDs(
	kind string,
	keys []string,
	ids map[string]shoal.ID,
) ([]shoal.ID, error) {
	resolved := make([]shoal.ID, 0, len(keys))
	for _, key := range keys {
		id, ok := ids[key]
		if !ok {
			return nil, fmt.Errorf("unknown %s key %q", kind, key)
		}
		resolved = append(resolved, id)
	}
	return resolved, nil
}
