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

package auth

import (
	"bytes"
	"sort"
	"time"

	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// DecisionConfig supplies trusted identity and grant material to NewDecision.
type DecisionConfig struct {
	Subject                shoal.ID
	Actor                  shoal.ID
	ClientID               shoal.ID
	OnBehalfOf             []shoal.ID
	AuthorizationDomain    []byte
	AllowedOperations      []Operation
	PermittedSourceIDs     [][]byte
	PermittedPolicyIDs     [][]byte
	PolicyGeneration       int64
	AuthenticationExpires  time.Time
	RequestID              shoal.ID
	CorrelationID          shoal.ID
	AuditPurpose           string
	ServiceRole            ServiceRole
	ServiceCeilingIdentity shoal.ID
	SelectedOntology       ontology.OntologyIdentity
	// GrantProvenance is set only by an authenticator that granted the
	// decision's authority through an operator mapping (#451). It is
	// additive: a decision without one fingerprints exactly as before.
	GrantProvenance GrantProvenance
	// ExecutorBinding names the one executor reference an action-execution
	// decision may act for (#391). It is required with, and valid only with,
	// ServiceRoleActionExecution. It is additive: a decision without one
	// fingerprints exactly as before. A bound decision must carry an empty
	// OnBehalfOf chain: an executor acts as itself.
	ExecutorBinding string
}

// Decision is an immutable trusted authorization decision.
type Decision struct {
	subject                shoal.ID
	actor                  shoal.ID
	clientID               shoal.ID
	onBehalfOf             []shoal.ID
	domain                 []byte
	operations             []Operation
	sources                [][]byte
	policies               [][]byte
	policyGeneration       int64
	expiresAt              time.Time
	requestID              shoal.ID
	correlationID          shoal.ID
	auditPurpose           string
	serviceRole            ServiceRole
	serviceCeilingIdentity shoal.ID
	selectedOntology       ontology.OntologyIdentity
	grantProvenance        GrantProvenance
	executorBinding        string
}

// NewDecision validates, canonicalizes, and defensively owns a trusted
// decision. Authentication expiry is checked when the decision is bound,
// resolved, or authorized.
func NewDecision(config DecisionConfig) (Decision, error) {
	if err := shoal.ValidateRequiredID("subject", config.Subject); err != nil {
		return Decision{}, err
	}
	if err := shoal.ValidateRequiredID("actor", config.Actor); err != nil {
		return Decision{}, err
	}
	if err := shoal.ValidateRequiredID("request ID", config.RequestID); err != nil {
		return Decision{}, err
	}
	if err := shoal.ValidateOptionalID("client ID", config.ClientID); err != nil {
		return Decision{}, err
	}
	if err := shoal.ValidateOptionalID("correlation ID", config.CorrelationID); err != nil {
		return Decision{}, err
	}
	if err := shoal.ValidateOptionalID(
		"service ceiling", config.ServiceCeilingIdentity,
	); err != nil {
		return Decision{}, err
	}
	if len(config.OnBehalfOf) > MaxOnBehalfOfEntries {
		return Decision{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "on-behalf-of chain exceeds the public bound")
	}
	onBehalfOf := append([]shoal.ID(nil), config.OnBehalfOf...)
	for _, identity := range onBehalfOf {
		if err := shoal.ValidateRequiredID("on-behalf-of identity", identity); err != nil {
			return Decision{}, err
		}
	}
	if err := validatePolicyComponent(
		"authorization domain", config.AuthorizationDomain, true,
	); err != nil {
		return Decision{}, err
	}
	if len(config.PermittedSourceIDs) > MaxDecisionGrantIDs ||
		len(config.PermittedPolicyIDs) > MaxDecisionGrantIDs {
		return Decision{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "decision grants exceed the public ID bound")
	}
	operations, err := normalizeOperations(config.AllowedOperations)
	if err != nil {
		return Decision{}, err
	}
	sources, err := sortedUniqueBytes(
		"permitted source identity", config.PermittedSourceIDs, true)
	if err != nil {
		return Decision{}, err
	}
	policies, err := sortedUniqueBytes(
		"permitted policy identity", config.PermittedPolicyIDs, true)
	if err != nil {
		return Decision{}, err
	}
	for _, policy := range policies {
		if IsReservedLabelPolicyID(policy) {
			return Decision{}, shoal.NewError(
				shoal.ErrorInvalidArgument,
				"permitted policy identity is in the reserved label namespace "+
					"and can never be granted",
			)
		}
	}
	if config.PolicyGeneration <= 0 {
		return Decision{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "policy generation must be positive")
	}
	if config.AuthenticationExpires.IsZero() {
		return Decision{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "authentication expiry is required")
	}
	expiresAt := config.AuthenticationExpires.UTC()
	if year := expiresAt.Year(); year < 1 || year > 9999 {
		return Decision{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "authentication expiry is outside the supported range")
	}
	if err := validateOptionalSemantic("audit purpose", config.AuditPurpose); err != nil {
		return Decision{}, err
	}
	if config.SelectedOntology.Known() {
		if err := config.SelectedOntology.Validate(); err != nil {
			return Decision{}, err
		}
	}
	if err := config.GrantProvenance.Validate(); err != nil {
		return Decision{}, err
	}
	if err := validateExecutorBinding(
		config.ServiceRole, config.ExecutorBinding, onBehalfOf,
	); err != nil {
		return Decision{}, err
	}
	if config.ServiceRole == "" {
		if config.ServiceCeilingIdentity != "" {
			return Decision{}, shoal.NewError(
				shoal.ErrorInvalidArgument,
				"service ceiling identity requires a service role",
			)
		}
	} else {
		if err := config.ServiceRole.Validate(); err != nil {
			return Decision{}, err
		}
		if err := shoal.ValidateRequiredID(
			"service ceiling identity", config.ServiceCeilingIdentity,
		); err != nil {
			return Decision{}, err
		}
		for _, operation := range operations {
			if !config.ServiceRole.Allows(operation) {
				return Decision{}, shoal.NewError(
					shoal.ErrorInvalidArgument,
					"allowed operation exceeds the service role ceiling",
				)
			}
		}
	}

	return Decision{
		subject:                config.Subject,
		actor:                  config.Actor,
		clientID:               config.ClientID,
		onBehalfOf:             onBehalfOf,
		domain:                 cloneBytes(config.AuthorizationDomain),
		operations:             operations,
		sources:                sources,
		policies:               policies,
		policyGeneration:       config.PolicyGeneration,
		expiresAt:              expiresAt,
		requestID:              config.RequestID,
		correlationID:          config.CorrelationID,
		auditPurpose:           config.AuditPurpose,
		serviceRole:            config.ServiceRole,
		serviceCeilingIdentity: config.ServiceCeilingIdentity,
		selectedOntology:       config.SelectedOntology,
		grantProvenance:        config.GrantProvenance.Clone(),
		executorBinding:        config.ExecutorBinding,
	}, nil
}

func normalizeOperations(operations []Operation) ([]Operation, error) {
	if len(operations) == 0 {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "at least one allowed operation is required")
	}
	if len(operations) > MaxDecisionOperations {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "allowed operations exceed the public bound")
	}
	normalized := append([]Operation(nil), operations...)
	for _, operation := range normalized {
		if err := operation.Validate(); err != nil {
			return nil, err
		}
	}
	sort.Slice(normalized, func(left, right int) bool {
		return normalized[left] < normalized[right]
	})
	result := normalized[:0]
	for _, operation := range normalized {
		if len(result) > 0 && result[len(result)-1] == operation {
			continue
		}
		result = append(result, operation)
	}
	return result, nil
}

// Subject returns the authenticated subject.
func (d Decision) Subject() shoal.ID { return d.subject }

// Actor returns the authenticated actor or calling application identity.
func (d Decision) Actor() shoal.ID { return d.actor }

// ClientID returns the optional client identity.
func (d Decision) ClientID() shoal.ID { return d.clientID }

// OnBehalfOf returns an independent copy of the delegation chain.
func (d Decision) OnBehalfOf() []shoal.ID {
	return append([]shoal.ID(nil), d.onBehalfOf...)
}

// AuthorizationDomain returns an independent copy of the authorization domain.
func (d Decision) AuthorizationDomain() []byte { return cloneBytes(d.domain) }

// AllowedOperations returns the canonical operation set.
func (d Decision) AllowedOperations() []Operation {
	return append([]Operation(nil), d.operations...)
}

// PermittedSourceIDs returns independent copies in canonical byte order.
func (d Decision) PermittedSourceIDs() [][]byte { return cloneByteSlices(d.sources) }

// PermittedPolicyIDs returns independent copies in canonical byte order.
func (d Decision) PermittedPolicyIDs() [][]byte { return cloneByteSlices(d.policies) }

// PolicyGeneration returns the positive signed policy generation.
func (d Decision) PolicyGeneration() int64 { return d.policyGeneration }

// AuthenticationExpires returns the UTC authentication expiry.
func (d Decision) AuthenticationExpires() time.Time { return d.expiresAt }

// RequestID returns the trusted request identifier.
func (d Decision) RequestID() shoal.ID { return d.requestID }

// CorrelationID returns the optional trusted correlation identifier.
func (d Decision) CorrelationID() shoal.ID { return d.correlationID }

// AuditPurpose returns the optional trusted audit purpose.
func (d Decision) AuditPurpose() string { return d.auditPurpose }

// ServiceRole returns the optional trusted service-account role.
func (d Decision) ServiceRole() ServiceRole { return d.serviceRole }

// ServiceCeilingIdentity returns the optional configured ceiling identity.
func (d Decision) ServiceCeilingIdentity() shoal.ID {
	return d.serviceCeilingIdentity
}

// SelectedOntology returns the immutable read-time ontology lens selected for
// this decision. The boolean is false when reads retain legacy no-lens behavior.
func (d Decision) SelectedOntology() (ontology.OntologyIdentity, bool) {
	return d.selectedOntology, d.selectedOntology.Known()
}

// TrustedService reports whether the decision carries a validated service
// role and ceiling identity.
func (d Decision) TrustedService() bool {
	return d.serviceRole != "" && d.serviceCeilingIdentity != ""
}

// String renders only the authorization fingerprint.
func (d Decision) String() string {
	fingerprint, err := AuthorizationFingerprint(d)
	if err != nil {
		return "authorization-decision{invalid}"
	}
	return "authorization-decision{" + fingerprint.String() + "}"
}

// Authorize performs exact fail-closed operation, expiry, domain, source, and
// policy checks. Metadata, scope, object identity, and claimed ownership never
// increase authority.
func (d Decision) Authorize(
	operation Operation,
	resource ResourceRequest,
	now time.Time,
) error {
	_, err := d.authorize(operation, resource, now)
	return err
}

// AuthorizeObject performs authorization for a direct object operation and
// converts only source/policy projection denial to the same not_found shape as
// an absent object. Whole-request failures remain unauthorized.
func (d Decision) AuthorizeObject(
	operation Operation,
	resource ResourceRequest,
	now time.Time,
) error {
	cloned, err := d.preflight(operation, now)
	if err != nil {
		return err
	}
	normalized, err := resource.Normalize()
	if err != nil {
		return err
	}
	if err := shoal.ValidateRequiredID("object ID", normalized.ObjectID); err != nil {
		return err
	}
	denial, err := cloned.authorizeResource(normalized)
	if err == nil {
		return nil
	}
	if denial == denialResource {
		return ObjectNotFound()
	}
	return err
}

// ObjectNotFound returns the non-disclosing direct-object absence shape.
func ObjectNotFound() error {
	return shoal.NewError(shoal.ErrorNotFound, "object not found")
}

type denialClass uint8

const (
	denialNone denialClass = iota
	denialRequest
	denialResource
)

func (d Decision) authorize(
	operation Operation,
	resource ResourceRequest,
	now time.Time,
) (denialClass, error) {
	cloned, err := d.preflight(operation, now)
	if err != nil {
		return denialRequest, err
	}
	normalized, err := resource.Normalize()
	if err != nil {
		return denialNone, err
	}
	return cloned.authorizeResource(normalized)
}

func (d Decision) preflight(operation Operation, now time.Time) (Decision, error) {
	if operation.Validate() != nil || now.IsZero() ||
		(d.serviceRole != "" && !d.serviceRole.Allows(operation)) {
		return Decision{}, unauthorized()
	}
	cloned, err := d.cloneValidated()
	if err != nil || !now.Before(cloned.expiresAt) ||
		!cloned.operationAllowed(operation) ||
		(cloned.serviceRole != "" && !cloned.serviceRole.Allows(operation)) {
		return Decision{}, unauthorized()
	}
	return cloned, nil
}

func (d Decision) authorizeResource(
	resource ResourceRequest,
) (denialClass, error) {
	if !bytes.Equal(d.domain, resource.AuthorizationDomain) {
		return denialRequest, unauthorized()
	}
	if len(resource.SourceID) > 0 && !containsBytes(d.sources, resource.SourceID) {
		return denialResource, unauthorized()
	}
	if len(resource.PolicyID) > 0 && !containsBytes(d.policies, resource.PolicyID) {
		return denialResource, unauthorized()
	}
	return denialNone, nil
}

// IntersectSourceIDs authorizes the whole request and returns the exact
// canonical intersection of requested and permitted source identities. An
// empty requested set means all permitted sources; hidden sources are omitted.
func (d Decision) IntersectSourceIDs(
	operation Operation,
	authorizationDomain []byte,
	requested [][]byte,
	now time.Time,
) ([][]byte, error) {
	cloned, err := d.preflight(operation, now)
	if err != nil {
		return nil, err
	}
	if err := validatePolicyComponent(
		"authorization domain", authorizationDomain, true,
	); err != nil {
		return nil, err
	}
	if !bytes.Equal(cloned.domain, authorizationDomain) {
		return nil, unauthorized()
	}
	if len(requested) == 0 {
		return cloneByteSlices(cloned.sources), nil
	}
	if len(requested) > MaxDecisionGrantIDs {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "requested sources exceed the public ID bound")
	}
	normalized, err := sortedUniqueBytes("requested source identity", requested, true)
	if err != nil {
		return nil, err
	}
	intersection := make([][]byte, 0, len(normalized))
	for _, source := range normalized {
		if containsBytes(cloned.sources, source) {
			intersection = append(intersection, cloneBytes(source))
		}
	}
	return intersection, nil
}

// IntersectPolicyIDs is the grant-policy counterpart to IntersectSourceIDs.
func (d Decision) IntersectPolicyIDs(
	operation Operation,
	authorizationDomain []byte,
	requested [][]byte,
	now time.Time,
) ([][]byte, error) {
	cloned, err := d.preflight(operation, now)
	if err != nil {
		return nil, err
	}
	if err := validatePolicyComponent(
		"authorization domain", authorizationDomain, true,
	); err != nil {
		return nil, err
	}
	if !bytes.Equal(cloned.domain, authorizationDomain) {
		return nil, unauthorized()
	}
	if len(requested) == 0 {
		return cloneByteSlices(cloned.policies), nil
	}
	if len(requested) > MaxDecisionGrantIDs {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "requested policies exceed the public ID bound")
	}
	normalized, err := sortedUniqueBytes("requested policy identity", requested, true)
	if err != nil {
		return nil, err
	}
	intersection := make([][]byte, 0, len(normalized))
	for _, policy := range normalized {
		if containsBytes(cloned.policies, policy) {
			intersection = append(intersection, cloneBytes(policy))
		}
	}
	return intersection, nil
}

func (d Decision) operationAllowed(operation Operation) bool {
	index := sort.Search(len(d.operations), func(index int) bool {
		return d.operations[index] >= operation
	})
	return index < len(d.operations) && d.operations[index] == operation
}

func (d Decision) cloneValidated() (Decision, error) {
	return NewDecision(DecisionConfig{
		Subject:                d.subject,
		Actor:                  d.actor,
		ClientID:               d.clientID,
		OnBehalfOf:             d.onBehalfOf,
		AuthorizationDomain:    d.domain,
		AllowedOperations:      d.operations,
		PermittedSourceIDs:     d.sources,
		PermittedPolicyIDs:     d.policies,
		PolicyGeneration:       d.policyGeneration,
		AuthenticationExpires:  d.expiresAt,
		RequestID:              d.requestID,
		CorrelationID:          d.correlationID,
		AuditPurpose:           d.auditPurpose,
		ServiceRole:            d.serviceRole,
		ServiceCeilingIdentity: d.serviceCeilingIdentity,
		SelectedOntology:       d.selectedOntology,
		GrantProvenance:        d.grantProvenance,
		ExecutorBinding:        d.executorBinding,
	})
}

func unauthorized() error {
	return shoal.NewError(shoal.ErrorUnauthorized, "authorization denied")
}
