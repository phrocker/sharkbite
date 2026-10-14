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
	"time"

	"github.com/phrocker/shoal-oss/accumulo"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// ServiceCeilingConfig supplies one configured service account's immutable
// authorization ceiling. It contains no Accumulo credentials.
type ServiceCeilingConfig struct {
	Identity       shoal.ID
	Role           ServiceRole
	Authorizations *accumulo.Authorizations
}

// ServiceCeiling is a defensively owned service-account role and label ceiling.
type ServiceCeiling struct {
	identity       shoal.ID
	role           ServiceRole
	authorizations *accumulo.Authorizations
	set            bool
}

// NewServiceCeiling validates a structured label ceiling and requires the
// account's own svc:<role> label.
func NewServiceCeiling(config ServiceCeilingConfig) (ServiceCeiling, error) {
	if err := shoal.ValidateRequiredID("service ceiling identity", config.Identity); err != nil {
		return ServiceCeiling{}, err
	}
	if err := config.Role.Validate(); err != nil {
		return ServiceCeiling{}, err
	}
	if config.Authorizations == nil {
		return ServiceCeiling{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "service authorization ceiling is required")
	}
	authorizations := config.Authorizations.Clone()
	for _, label := range authorizations.List() {
		if err := validateStructuredLabel(label); err != nil {
			return ServiceCeiling{}, shoal.NewError(
				shoal.ErrorInvalidArgument,
				"service authorization ceiling contains a noncanonical label",
			)
		}
	}
	roleLabel := []byte("svc:" + string(config.Role))
	if !authorizations.Contains(roleLabel) {
		return ServiceCeiling{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "service authorization ceiling lacks its role label")
	}
	return ServiceCeiling{
		identity:       config.Identity,
		role:           config.Role,
		authorizations: authorizations,
		set:            true,
	}, nil
}

// Identity returns the configured non-credential ceiling identity.
func (c ServiceCeiling) Identity() shoal.ID { return c.identity }

// Role returns the configured least-privilege service role.
func (c ServiceCeiling) Role() ServiceRole { return c.role }

// String renders no raw identity, labels, or credentials.
func (c ServiceCeiling) String() string {
	identity := DigestBytes("explorer-service-ceiling-v1", []byte(c.identity))
	return "service-ceiling{role=" + string(c.role) + ",identity=" + identity.String() + "}"
}

// DeriveScannerAuthorizations derives only the labels required by policy,
// checks the exact caller decision and service role, and rejects any required
// label outside the configured service-account ceiling.
func DeriveScannerAuthorizations(
	decision Decision,
	operation Operation,
	policy Policy,
	ceiling ServiceCeiling,
	now time.Time,
) (*accumulo.Authorizations, error) {
	if !ceiling.set || ceiling.authorizations == nil {
		return nil, unauthorized()
	}
	clonedDecision, err := decision.cloneValidated()
	if err != nil {
		return nil, unauthorized()
	}
	if !clonedDecision.TrustedService() ||
		clonedDecision.serviceRole != ceiling.role ||
		clonedDecision.serviceCeilingIdentity != ceiling.identity ||
		!ceiling.role.Allows(operation) {
		return nil, unauthorized()
	}
	if err := policy.validate(); err != nil {
		return nil, err
	}
	if policy.serviceRole != "" && policy.serviceRole != ceiling.role {
		return nil, unauthorized()
	}
	if policy.serviceRole == "" && roleRequiresServiceVisibility(ceiling.role) {
		return nil, unauthorized()
	}
	resource := ResourceRequest{
		AuthorizationDomain: policy.domain,
		SourceID:            policy.source,
		PolicyID:            policy.grantPolicy,
	}
	if err := clonedDecision.Authorize(operation, resource, now); err != nil {
		return nil, err
	}
	if !bytes.Equal(clonedDecision.domain, policy.domain) {
		return nil, unauthorized()
	}
	labels, err := policy.labels()
	if err != nil {
		return nil, err
	}
	for _, label := range labels {
		if !ceiling.authorizations.Contains(label) {
			return nil, unauthorized()
		}
	}
	return accumulo.NewAuthorizations(labels...), nil
}
