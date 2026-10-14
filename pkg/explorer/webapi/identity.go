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
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/ontology"
)

// IdentityResponse projects the trusted per-request decision into the browser
// so the workspace can show a caller who they are and therefore why a result
// set is scoped the way it is.
//
// It is a read-only projection of the decision the transport already
// established for the request. It confers no authority and is never consulted
// to make an authorization choice: the authorized Explorer client remains the
// only enforcement point. Only accessors the auth package already exposes
// publicly are copied, and the raw source and policy grant identifiers are
// deliberately omitted so the browser cannot reconstruct another principal's
// visibility from its own identity view.
type IdentityResponse struct {
	Authenticated         bool              `json:"authenticated"`
	Subject               string            `json:"subject,omitempty"`
	Actor                 string            `json:"actor,omitempty"`
	AuthorizationDomain   string            `json:"authorization_domain,omitempty"`
	Operations            []string          `json:"operations"`
	PolicyGeneration      int64             `json:"policy_generation,omitempty"`
	AuthenticationExpires time.Time         `json:"authentication_expires,omitempty"`
	AuditPurpose          string            `json:"audit_purpose,omitempty"`
	RequestID             string            `json:"request_id,omitempty"`
	SelectedOntology      *IdentityOntology `json:"selected_ontology,omitempty"`
}

// IdentityOntology is the immutable read-time lens currently in force.
type IdentityOntology struct {
	SchemaID  string `json:"schema_id"`
	VersionID string `json:"version_id"`
}

type identityContextKey struct{}

// withIdentity stores the browser-safe projection of a trusted decision on the
// request context so the identity endpoint can surface it without re-reading
// transport state.
func withIdentity(ctx context.Context, decision auth.Decision) context.Context {
	return context.WithValue(ctx, identityContextKey{}, projectDecision(decision))
}

// identityFromContext returns the projection stored by withIdentity. The second
// result is false on a transport that established no identity, which the
// endpoint reports as an explicitly unauthenticated caller rather than a blank.
func identityFromContext(ctx context.Context) (IdentityResponse, bool) {
	identity, ok := ctx.Value(identityContextKey{}).(IdentityResponse)
	return identity, ok
}

// projectDecision copies only the browser-safe fields of a trusted decision.
func projectDecision(decision auth.Decision) IdentityResponse {
	operations := decision.AllowedOperations()
	names := make([]string, 0, len(operations))
	for _, operation := range operations {
		names = append(names, string(operation))
	}
	identity := IdentityResponse{
		Authenticated:         true,
		Subject:               string(decision.Subject()),
		Actor:                 string(decision.Actor()),
		Operations:            names,
		PolicyGeneration:      decision.PolicyGeneration(),
		AuthenticationExpires: decision.AuthenticationExpires(),
		AuditPurpose:          decision.AuditPurpose(),
		RequestID:             string(decision.RequestID()),
	}
	if domain := decision.AuthorizationDomain(); len(domain) > 0 {
		identity.AuthorizationDomain = string(domain)
	}
	if selected, ok := decision.SelectedOntology(); ok {
		identity.SelectedOntology = identityOntology(selected)
	}
	return identity
}

func identityOntology(value ontology.OntologyIdentity) *IdentityOntology {
	return &IdentityOntology{
		SchemaID:  encodeID(value.SchemaID()),
		VersionID: encodeID(value.VersionID()),
	}
}

// unauthenticatedIdentity is the projection for a transport that established no
// per-request decision. Operations is a non-nil empty slice so the browser
// always receives a JSON array.
func unauthenticatedIdentity() IdentityResponse {
	return IdentityResponse{Authenticated: false, Operations: []string{}}
}
