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

package webapi

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	MaxOntologyConcepts                 uint32 = 256
	MaxOntologyRelationships            uint32 = 256
	MaxOntologyProperties               uint32 = 512
	MaxOntologyDefinitionProperties     uint32 = 128
	MaxOntologyRelationshipEndpointSets uint32 = 128
	MaxOntologyConstraintsPerProperty   uint32 = 32
	MaxOntologyAllowedValues            uint32 = 128
)

// ActiveOntologyProvider is the optional read-only service extension behind
// GET /api/v1/ontology. Services that do not implement it have no configured
// active ontology; the endpoint reports that state explicitly instead of
// failing or inventing an empty schema.
type ActiveOntologyProvider interface {
	ActiveOntology(context.Context) (ontology.OntologyVersion, bool, error)
}

// OntologyCatalogProvider exposes the canonical governed choice set and its
// durable active tip. Authorized backends enforce catalog access before this
// method returns; callers still enforce issuer-selected-lens constraints. The
// catalog prevents either layer from duplicating publication-chain/CAS
// semantics.
type OntologyCatalogProvider interface {
	OntologyCatalog(context.Context) (ontology.PublishedCatalog, bool, error)
}

// OntologySelectionAuthorizer checks one published identity under the exact
// operation whose decision is being narrowed without exposing the catalog.
type OntologySelectionAuthorizer interface {
	AuthorizeOntologySelection(
		context.Context,
		ontology.OntologyIdentity,
		auth.Operation,
	) error
}

// OntologyResponse is the stable browser contract for the currently active
// ontology. Configured distinguishes "no ontology was configured" from a
// configured ontology that legitimately contains no definitions.
type OntologyResponse struct {
	Configured    bool                         `json:"configured"`
	Identity      OntologyIdentityProjection   `json:"identity"`
	Schema        *OntologySchemaProjection    `json:"schema"`
	Version       *OntologyVersionProjection   `json:"version"`
	Concepts      []OntologyConceptProjection  `json:"concepts"`
	Relationships []OntologyRelationProjection `json:"relationships"`
	Properties    []OntologyPropertyProjection `json:"properties"`
	Limits        OntologyDescriptionLimits    `json:"limits"`
}

// OntologyIdentityProjection names the active snapshot whose definitions are
// being read. Reading is same_version only when both schema and version are
// known and validated; unresolved is the honest state for an unconfigured
// endpoint.
type OntologyIdentityProjection struct {
	Known     bool   `json:"known"`
	SchemaID  string `json:"schema_id,omitempty"`
	VersionID string `json:"version_id,omitempty"`
	Reading   string `json:"reading"`
}

// ProjectOntologyIdentity applies the public opaque-ID wire encoding to one
// validated ontology identity.
func ProjectOntologyIdentity(
	identity ontology.OntologyIdentity,
) (OntologyIdentityProjection, error) {
	if err := identity.Validate(); err != nil {
		return OntologyIdentityProjection{}, err
	}
	return OntologyIdentityProjection{
		Known: true, SchemaID: encodeID(identity.SchemaID()),
		VersionID: encodeID(identity.VersionID()),
		Reading:   string(ontology.OntologySameVersion),
	}, nil
}

// ParseOntologyIdentityProjection decodes one public identity projection
// without interpreting an unknown selection as the active version.
func ParseOntologyIdentityProjection(
	projection OntologyIdentityProjection,
) (ontology.OntologyIdentity, error) {
	if !projection.Known {
		if projection.SchemaID != "" || projection.VersionID != "" {
			return ontology.OntologyIdentity{}, shoal.NewError(
				shoal.ErrorInvalidArgument,
				"unknown ontology identity cannot carry IDs",
			)
		}
		return ontology.UnknownOntology(), nil
	}
	schemaID, err := decodeID(projection.SchemaID)
	if err != nil {
		return ontology.OntologyIdentity{}, shoal.WrapError(
			shoal.ErrorInvalidArgument, "decode ontology schema ID", err)
	}
	versionID, err := decodeID(projection.VersionID)
	if err != nil {
		return ontology.OntologyIdentity{}, shoal.WrapError(
			shoal.ErrorInvalidArgument, "decode ontology version ID", err)
	}
	return ontology.NewOntologyIdentityFromIDs(schemaID, versionID)
}

type OntologySchemaProjection struct {
	ID          string `json:"id"`
	Key         string `json:"key"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type OntologyVersionProjection struct {
	ID        string    `json:"id"`
	Version   string    `json:"version"`
	CreatedAt time.Time `json:"created_at"`
}

type OntologyConceptProjection struct {
	ID          string   `json:"id"`
	Key         string   `json:"key"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Properties  []string `json:"properties"`
}

type OntologyRelationProjection struct {
	ID           string   `json:"id"`
	Key          string   `json:"key"`
	Name         string   `json:"name"`
	Description  string   `json:"description,omitempty"`
	Directed     bool     `json:"directed"`
	FromConcepts []string `json:"from_concepts"`
	ToConcepts   []string `json:"to_concepts"`
	Properties   []string `json:"properties"`
}

type OntologyPropertyProjection struct {
	ID          string                         `json:"id"`
	Key         string                         `json:"key"`
	Name        string                         `json:"name"`
	Description string                         `json:"description,omitempty"`
	ValueType   string                         `json:"value_type"`
	Constraints []OntologyConstraintProjection `json:"constraints"`
}

type OntologyConstraintProjection struct {
	Kind          string                    `json:"kind"`
	Count         uint32                    `json:"count,omitempty"`
	Value         *OntologyValueProjection  `json:"value,omitempty"`
	Pattern       string                    `json:"pattern,omitempty"`
	AllowedValues []OntologyValueProjection `json:"allowed_values,omitempty"`
}

type OntologyValueProjection struct {
	Type  string `json:"type"`
	Value any    `json:"value"`
}

// OntologyDescriptionLimits documents the hard server-side bounds that keep
// this read-only description surface from becoming an unbounded schema dump.
type OntologyDescriptionLimits struct {
	MaxConcepts                 uint32 `json:"max_concepts"`
	MaxRelationships            uint32 `json:"max_relationships"`
	MaxProperties               uint32 `json:"max_properties"`
	MaxDefinitionProperties     uint32 `json:"max_definition_properties"`
	MaxRelationshipEndpointSets uint32 `json:"max_relationship_endpoint_sets"`
	MaxConstraintsPerProperty   uint32 `json:"max_constraints_per_property"`
	MaxAllowedValues            uint32 `json:"max_allowed_values"`
}

// SetOntologyVersion configures the immutable ontology snapshot surfaced by
// ActiveOntology. Passing only a validated ontology version preserves the
// package-level identity rules and keeps this transport read-only.
func (s *EmbeddedService) SetOntologyVersion(version ontology.OntologyVersion) error {
	if err := version.Validate(); err != nil {
		return err
	}
	cloned := version
	s.ontologyPublishMu.Lock()
	defer s.ontologyPublishMu.Unlock()
	s.ontologyMu.Lock()
	defer s.ontologyMu.Unlock()
	s.ontologyVersion = &cloned
	return nil
}

func (s *EmbeddedService) ActiveOntology(
	ctx context.Context,
) (ontology.OntologyVersion, bool, error) {
	if ctx == nil {
		return ontology.OntologyVersion{}, false, shoal.NewError(
			shoal.ErrorInvalidArgument, "context is required")
	}
	if err := ctx.Err(); err != nil {
		return ontology.OntologyVersion{}, false, err
	}
	s.ontologyMu.RLock()
	if s.ontologyVersion == nil {
		s.ontologyMu.RUnlock()
		return ontology.OntologyVersion{}, false, nil
	}
	configured := *s.ontologyVersion
	s.ontologyMu.RUnlock()
	if provider, ok := s.client.(explorer.OntologyActiveStateProvider); ok {
		active, err := provider.OntologyActiveState(ctx, configured)
		return active, true, err
	}
	catalog, _, err := s.OntologyCatalog(ctx)
	if err != nil {
		return ontology.OntologyVersion{}, false, err
	}
	return catalog.Active(), true, nil
}

func (s *EmbeddedService) OntologyCatalog(
	ctx context.Context,
) (ontology.PublishedCatalog, bool, error) {
	if ctx == nil {
		return ontology.PublishedCatalog{}, false, shoal.NewError(
			shoal.ErrorInvalidArgument, "context is required")
	}
	if err := ctx.Err(); err != nil {
		return ontology.PublishedCatalog{}, false, err
	}
	s.ontologyMu.RLock()
	if s.ontologyVersion == nil {
		s.ontologyMu.RUnlock()
		return ontology.PublishedCatalog{}, false, nil
	}
	configured := *s.ontologyVersion
	s.ontologyMu.RUnlock()
	if provider, ok := s.client.(explorer.PublishedOntologyCatalogProvider); ok {
		catalog, err := provider.PublishedOntologyCatalog(ctx, configured)
		return catalog, true, err
	}
	if _, exposesRawProposals := s.client.(ontologyProposalReadClient); exposesRawProposals {
		return ontology.PublishedCatalog{}, false, shoal.NewError(
			shoal.ErrorUnavailable,
			"workspace capability \"published ontology catalog\" is unavailable",
		)
	}
	catalog, err := boundedOntologyCatalog(configured, nil)
	return catalog, true, err
}

// AuthorizeOntologySelection checks one governed published identity without
// requiring the caller to have workspace-settings catalog read authority.
func (s *EmbeddedService) AuthorizeOntologySelection(
	ctx context.Context,
	identity ontology.OntologyIdentity,
	operation auth.Operation,
) error {
	if ctx == nil {
		return shoal.NewError(shoal.ErrorInvalidArgument, "context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := identity.Validate(); err != nil {
		return err
	}
	if err := operation.Validate(); err != nil {
		return err
	}
	s.ontologyMu.RLock()
	if s.ontologyVersion == nil {
		s.ontologyMu.RUnlock()
		return auth.ObjectNotFound()
	}
	configured := *s.ontologyVersion
	s.ontologyMu.RUnlock()
	provider, ok := s.client.(interface {
		AuthorizePublishedOntology(
			context.Context,
			ontology.OntologyVersion,
			ontology.OntologyIdentity,
			auth.Operation,
		) error
	})
	if ok {
		return provider.AuthorizePublishedOntology(
			ctx, configured, identity, operation)
	}
	catalog, configuredSet, err := s.OntologyCatalog(ctx)
	if err != nil {
		return err
	}
	if !configuredSet || !catalog.Contains(identity) {
		return auth.ObjectNotFound()
	}
	return nil
}

func boundedOntologyCatalog(
	configured ontology.OntologyVersion,
	proposals []ontology.GovernedProposal,
) (ontology.PublishedCatalog, error) {
	return explorer.NewPublishedOntologyCatalog(configured, proposals)
}

func replayPublishedOntology(
	configured ontology.OntologyVersion,
	proposals []ontology.GovernedProposal,
) (ontology.OntologyVersion, error) {
	catalog, err := boundedOntologyCatalog(configured, proposals)
	if err != nil {
		return ontology.OntologyVersion{}, err
	}
	return catalog.Active(), nil
}

func ontologyFor(ctx context.Context, service Service) (OntologyResponse, error) {
	response := emptyOntologyResponse()
	provider, ok := service.(ActiveOntologyProvider)
	if !ok {
		return response, nil
	}
	version, configured, err := provider.ActiveOntology(ctx)
	if err != nil {
		return OntologyResponse{}, err
	}
	if !configured {
		return response, nil
	}
	if err := version.Validate(); err != nil {
		return OntologyResponse{}, shoal.WrapError(
			shoal.ErrorInternal, "active ontology is invalid", err)
	}
	if err := enforceOntologyBounds(version); err != nil {
		return OntologyResponse{}, err
	}
	return projectOntology(version)
}

func emptyOntologyResponse() OntologyResponse {
	return OntologyResponse{
		Configured: false,
		Identity: OntologyIdentityProjection{
			Known: false, Reading: string(ontology.OntologyUnresolved),
		},
		Concepts:      []OntologyConceptProjection{},
		Relationships: []OntologyRelationProjection{},
		Properties:    []OntologyPropertyProjection{},
		Limits:        ontologyLimits(),
	}
}

func ontologyLimits() OntologyDescriptionLimits {
	return OntologyDescriptionLimits{
		MaxConcepts: MaxOntologyConcepts, MaxRelationships: MaxOntologyRelationships,
		MaxProperties:               MaxOntologyProperties,
		MaxDefinitionProperties:     MaxOntologyDefinitionProperties,
		MaxRelationshipEndpointSets: MaxOntologyRelationshipEndpointSets,
		MaxConstraintsPerProperty:   MaxOntologyConstraintsPerProperty,
		MaxAllowedValues:            MaxOntologyAllowedValues,
	}
}

func ontologyProjectionLimits() explorer.OntologyProjectionLimits {
	return explorer.OntologyProjectionLimits{
		MaxConcepts: MaxOntologyConcepts, MaxRelationships: MaxOntologyRelationships,
		MaxProperties: MaxOntologyProperties, MaxDefinitionProperties: MaxOntologyDefinitionProperties,
		MaxRelationshipEndpointSets: MaxOntologyRelationshipEndpointSets,
		MaxConstraintsPerProperty:   MaxOntologyConstraintsPerProperty,
		MaxAllowedValues:            MaxOntologyAllowedValues, MaxTransitions: MaxOntologyProposalTransitions,
		MaxMorphismEvidence: MaxEvidencePerResult, MaxDiscriminatorChoices: MaxOntologyConcepts,
	}
}

func enforceOntologyBounds(version ontology.OntologyVersion) error {
	return ontologyProjectionLimits().ValidateVersion(version)
}

func ontologyBoundError(name string, count int, limit uint32) error {
	return shoal.NewError(
		shoal.ErrorUnavailable,
		fmt.Sprintf(
			"active ontology %s count %d exceeds max_ontology bound %d",
			name, count, limit,
		),
	)
}

func projectOntology(version ontology.OntologyVersion) (OntologyResponse, error) {
	identity, err := ontology.NewOntologyIdentity(version)
	if err != nil {
		return OntologyResponse{}, err
	}
	schema := version.Schema()
	response := OntologyResponse{
		Configured: true,
		Identity: OntologyIdentityProjection{
			Known:     true,
			SchemaID:  encodeID(identity.SchemaID()),
			VersionID: encodeID(identity.VersionID()),
			Reading:   string(ontology.ReadOntologyUnder(identity, identity)),
		},
		Schema: &OntologySchemaProjection{
			ID: encodeID(schema.ID()), Key: schema.Key(), Name: schema.Name(),
			Description: schema.Description(),
		},
		Version: &OntologyVersionProjection{
			ID: encodeID(version.ID()), Version: version.Version(),
			CreatedAt: version.CreatedAt(),
		},
		Concepts:      []OntologyConceptProjection{},
		Relationships: []OntologyRelationProjection{},
		Properties:    []OntologyPropertyProjection{},
		Limits:        ontologyLimits(),
	}
	for _, concept := range version.Concepts() {
		response.Concepts = append(response.Concepts, OntologyConceptProjection{
			ID: encodeID(concept.ID()), Key: concept.Key(), Name: concept.Name(),
			Description: concept.Description(),
			Properties:  encodeOntologyIDs(concept.Properties()),
		})
	}
	for _, relationship := range version.Relationships() {
		response.Relationships = append(response.Relationships, OntologyRelationProjection{
			ID: encodeID(relationship.ID()), Key: relationship.Key(),
			Name: relationship.Name(), Description: relationship.Description(),
			Directed:     relationship.Directed(),
			FromConcepts: encodeOntologyIDs(relationship.FromConcepts()),
			ToConcepts:   encodeOntologyIDs(relationship.ToConcepts()),
			Properties:   encodeOntologyIDs(relationship.Properties()),
		})
	}
	for _, property := range version.Properties() {
		constraints, err := projectConstraints(property.Constraints())
		if err != nil {
			return OntologyResponse{}, err
		}
		response.Properties = append(response.Properties, OntologyPropertyProjection{
			ID: encodeID(property.ID()), Key: property.Key(), Name: property.Name(),
			Description: property.Description(), ValueType: string(property.ValueType()),
			Constraints: constraints,
		})
	}
	return response, nil
}

func projectConstraints(
	constraints []ontology.Constraint,
) ([]OntologyConstraintProjection, error) {
	projected := make([]OntologyConstraintProjection, 0, len(constraints))
	for _, constraint := range constraints {
		item := OntologyConstraintProjection{Kind: string(constraint.Kind())}
		if count, ok := constraint.Count(); ok {
			item.Count = count
		}
		if value, ok := constraint.Value(); ok {
			projectedValue, err := projectOntologyValue(value)
			if err != nil {
				return nil, err
			}
			item.Value = &projectedValue
		}
		if pattern, ok := constraint.Pattern(); ok {
			item.Pattern = pattern
		}
		allowed := constraint.AllowedValues()
		if len(allowed) > 0 {
			item.AllowedValues = make([]OntologyValueProjection, 0, len(allowed))
			for _, value := range allowed {
				projectedValue, err := projectOntologyValue(value)
				if err != nil {
					return nil, err
				}
				item.AllowedValues = append(item.AllowedValues, projectedValue)
			}
		}
		projected = append(projected, item)
	}
	return projected, nil
}

func projectOntologyValue(value ontology.Value) (OntologyValueProjection, error) {
	projected := OntologyValueProjection{Type: string(value.Type())}
	switch value.Type() {
	case ontology.ValueString:
		text, _ := value.StringValue()
		projected.Value = text
	case ontology.ValueInteger:
		integer, _ := value.IntegerValue()
		projected.Value = strconv.FormatInt(integer, 10)
	case ontology.ValueNumber:
		number, _ := value.NumberValue()
		projected.Value = number
	case ontology.ValueBoolean:
		boolean, _ := value.BooleanValue()
		projected.Value = boolean
	case ontology.ValueTimestamp:
		timestamp, _ := value.TimestampValue()
		projected.Value = timestamp
	case ontology.ValueReference:
		reference, _ := value.ReferenceValue()
		projected.Value = encodeID(reference)
	default:
		return OntologyValueProjection{}, shoal.NewError(
			shoal.ErrorInternal, "active ontology contains an invalid value type")
	}
	return projected, nil
}

func encodeOntologyIDs(values []shoal.ID) []string {
	encoded := make([]string, 0, len(values))
	for _, value := range values {
		encoded = append(encoded, encodeID(value))
	}
	return encoded
}
