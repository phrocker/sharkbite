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

package webapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type ontologyFixtureService struct {
	gateStubService
	version    ontology.OntologyVersion
	configured bool
	err        error
}

func (s ontologyFixtureService) ActiveOntology(
	context.Context,
) (ontology.OntologyVersion, bool, error) {
	if s.err != nil {
		return ontology.OntologyVersion{}, false, s.err
	}
	return s.version, s.configured, nil
}

func TestOntologyEndpointRequiresAuthentication(t *testing.T) {
	fixture := newAuthnFixture(t)
	response := gateGet(t, fixture, "/api/v1/ontology", "")
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated ontology status = %d, want 401", response.StatusCode)
	}
	if got := response.Header.Get("WWW-Authenticate"); !strings.Contains(
		strings.ToLower(got), "bearer") {
		t.Fatalf("unauthenticated ontology must carry bearer challenge, got %q", got)
	}
}

func TestOntologyEndpointDistinguishesNoConfigEmptyAndError(t *testing.T) {
	noConfig := getOntologyResponse(t, gateStubService{})
	if noConfig.Configured {
		t.Fatalf("unconfigured ontology configured = true, want false: %+v", noConfig)
	}
	if noConfig.Identity.Known ||
		noConfig.Identity.Reading != string(ontology.OntologyUnresolved) ||
		noConfig.Schema != nil ||
		noConfig.Version != nil ||
		len(noConfig.Concepts) != 0 ||
		len(noConfig.Relationships) != 0 ||
		len(noConfig.Properties) != 0 {
		t.Fatalf("unconfigured ontology did not render explicit empty state: %+v", noConfig)
	}

	empty := getOntologyResponse(t, ontologyFixtureService{
		version:    emptyOntologyVersion(t),
		configured: true,
	})
	if !empty.Configured || !empty.Identity.Known ||
		empty.Identity.Reading != string(ontology.OntologySameVersion) ||
		empty.Schema == nil || empty.Version == nil ||
		len(empty.Concepts) != 0 ||
		len(empty.Relationships) != 0 ||
		len(empty.Properties) != 0 {
		t.Fatalf("configured empty ontology was not distinguishable: %+v", empty)
	}

	server := ontologyServer(t, ontologyFixtureService{
		err: shoal.NewError(shoal.ErrorUnavailable, "ontology store unavailable"),
	})
	response, err := server.Client().Get(server.URL + "/api/v1/ontology")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("ontology provider error status = %d, want 503", response.StatusCode)
	}
}

func TestOntologyEndpointPropagatesAuthorizationDenial(t *testing.T) {
	authority := auth.NewAuthority()
	server := httptest.NewUnstartedServer(nil)
	handler, err := webapi.NewAuthenticatedHandler(
		ontologyFixtureService{
			err: shoal.NewError(
				shoal.ErrorUnauthorized,
				"operation describe ontology is not authorized",
			),
		},
		webapi.AuthenticatorFunc(authnAuthenticate),
		authority.Binder(),
		server.Listener.Addr().String(),
	)
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	server.Config.Handler = handler
	server.Start()
	defer server.Close()

	request, err := http.NewRequest(http.MethodGet, server.URL+"/api/v1/ontology", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(authnPrincipalHeader, "granted")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("ontology denial status = %d, want 401", response.StatusCode)
	}
	if got := response.Header.Get("WWW-Authenticate"); got != "" {
		t.Fatalf("ontology authorization denial must not carry bearer challenge, got %q", got)
	}
	var body struct {
		Code    shoal.ErrorCode `json:"code"`
		Message string          `json:"message"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Code != shoal.ErrorUnauthorized ||
		!strings.Contains(body.Message, "describe ontology") {
		t.Fatalf("ontology denial body = %+v, want specific unauthorized reason", body)
	}
}

func TestOntologyEndpointShapeAndBounds(t *testing.T) {
	response := getOntologyResponse(t, ontologyFixtureService{
		version:    richOntologyVersion(t),
		configured: true,
	})
	if !response.Configured ||
		response.Identity.SchemaID == "" ||
		response.Identity.VersionID == "" ||
		response.Identity.Reading != string(ontology.OntologySameVersion) {
		t.Fatalf("ontology identity = %+v", response.Identity)
	}
	if response.Schema == nil || response.Schema.Key != "workspace" ||
		response.Version == nil || response.Version.Version != "v1" {
		t.Fatalf("ontology schema/version = %+v %+v", response.Schema, response.Version)
	}
	if len(response.Concepts) != 2 ||
		len(response.Relationships) != 1 ||
		len(response.Properties) != 3 {
		t.Fatalf("ontology counts = concepts %d relationships %d properties %d",
			len(response.Concepts), len(response.Relationships), len(response.Properties))
	}
	if response.Limits.MaxConcepts != webapi.MaxOntologyConcepts ||
		response.Limits.MaxRelationships != webapi.MaxOntologyRelationships ||
		response.Limits.MaxProperties != webapi.MaxOntologyProperties ||
		response.Limits.MaxConstraintsPerProperty != webapi.MaxOntologyConstraintsPerProperty ||
		response.Limits.MaxAllowedValues != webapi.MaxOntologyAllowedValues {
		t.Fatalf("ontology limits = %+v", response.Limits)
	}
	relationship := response.Relationships[0]
	if !relationship.Directed ||
		relationship.Key != "member_of" ||
		len(relationship.FromConcepts) != 1 ||
		len(relationship.ToConcepts) != 1 {
		t.Fatalf("relationship projection lost declared join direction: %+v", relationship)
	}
	if relationship.FromConcepts[0] == relationship.ToConcepts[0] {
		t.Fatalf("relationship endpoints collapsed: %+v", relationship)
	}
	constraints := constraintsByKind(response.Properties)
	if constraints["required"].Kind != "required" ||
		constraints["minimum_value"].Value == nil ||
		constraints["minimum_value"].Value.Type != "integer" ||
		constraints["minimum_value"].Value.Value != "0" ||
		len(constraints["allowed_values"].AllowedValues) != 2 {
		t.Fatalf("constraint projection = %+v", constraints)
	}

	encoded := getOntologyJSON(t, ontologyFixtureService{
		version:    richOntologyVersion(t),
		configured: true,
	})
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &raw); err != nil {
		t.Fatal(err)
	}
	if got, want := sortedKeys(raw), []string{
		"concepts", "configured", "identity", "limits",
		"properties", "relationships", "schema", "version",
	}; !equalStrings(got, want) {
		t.Fatalf("top-level ontology JSON keys = %v, want %v\n%s", got, want, encoded)
	}
	if bytes.Contains(encoded, []byte("metadata")) {
		t.Fatalf("ontology JSON leaked unsupported metadata field: %s", encoded)
	}
}

func TestOntologyEndpointUsesEmbeddedServiceConfiguredOntology(t *testing.T) {
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	service, err := webapi.NewEmbeddedService(corpus)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.SetOntologyVersion(richOntologyVersion(t)); err != nil {
		t.Fatal(err)
	}

	response := getOntologyResponse(t, service)
	if !response.Configured {
		t.Fatalf("embedded service ontology configured = false, want true")
	}
	if response.Identity.Reading != string(ontology.OntologySameVersion) ||
		len(response.Concepts) == 0 ||
		len(response.Relationships) == 0 ||
		len(response.Properties) == 0 {
		t.Fatalf("embedded service ontology response = %+v", response)
	}
}

func TestOntologyEndpointRejectsOversizedOntology(t *testing.T) {
	server := ontologyServer(t, ontologyFixtureService{
		version:    oversizedOntologyVersion(t),
		configured: true,
	})
	response, err := server.Client().Get(server.URL + "/api/v1/ontology")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("oversized ontology status = %d, want 503", response.StatusCode)
	}
	var body struct {
		Code    shoal.ErrorCode `json:"code"`
		Message string          `json:"message"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Code != shoal.ErrorUnavailable ||
		!strings.Contains(body.Message, "concept count") ||
		!strings.Contains(body.Message, "max_ontology") {
		t.Fatalf("oversized ontology error = %+v", body)
	}
}

func ontologyServer(t *testing.T, service webapi.Service) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(nil)
	handler, err := webapi.NewHandler(service, server.Listener.Addr().String())
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	server.Config.Handler = handler
	server.Start()
	t.Cleanup(server.Close)
	return server
}

func getOntologyResponse(t *testing.T, service webapi.Service) webapi.OntologyResponse {
	t.Helper()
	var response webapi.OntologyResponse
	if err := json.Unmarshal(getOntologyJSON(t, service), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func getOntologyJSON(t *testing.T, service webapi.Service) []byte {
	t.Helper()
	server := ontologyServer(t, service)
	response, err := server.Client().Get(server.URL + "/api/v1/ontology")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var data bytes.Buffer
	if _, err := data.ReadFrom(response.Body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("ontology status = %s: %s", response.Status, data.String())
	}
	return data.Bytes()
}

func richOntologyVersion(t *testing.T) ontology.OntologyVersion {
	t.Helper()
	required, err := ontology.NewFlagConstraint(ontology.ConstraintRequired)
	if err != nil {
		t.Fatal(err)
	}
	minAge, err := ontology.NewValueConstraint(
		ontology.ConstraintMinimumValue, ontology.NewIntegerValue(0))
	if err != nil {
		t.Fatal(err)
	}
	maxAge, err := ontology.NewValueConstraint(
		ontology.ConstraintMaximumValue, ontology.NewIntegerValue(130))
	if err != nil {
		t.Fatal(err)
	}
	pattern, err := ontology.NewPatternConstraint(`^[A-Za-z ]+$`)
	if err != nil {
		t.Fatal(err)
	}
	alice, err := ontology.NewStringValue("Alice")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := ontology.NewStringValue("Bob")
	if err != nil {
		t.Fatal(err)
	}
	allowed, err := ontology.NewAllowedValuesConstraint([]ontology.Value{bob, alice})
	if err != nil {
		t.Fatal(err)
	}

	name, err := ontology.NewPropertyDefinition(
		"name", "Name", "Display name", ontology.ValueString,
		[]ontology.Constraint{required, pattern, allowed}, nil)
	if err != nil {
		t.Fatal(err)
	}
	age, err := ontology.NewPropertyDefinition(
		"age", "Age", "Age in years", ontology.ValueInteger,
		[]ontology.Constraint{minAge, maxAge}, nil)
	if err != nil {
		t.Fatal(err)
	}
	role, err := ontology.NewPropertyDefinition(
		"role", "Role", "Membership role", ontology.ValueString,
		[]ontology.Constraint{required}, nil)
	if err != nil {
		t.Fatal(err)
	}
	person, err := ontology.NewConceptDefinition(
		"person", "Person", "A person", []shoal.ID{name.ID(), age.ID()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	organization, err := ontology.NewConceptDefinition(
		"organization", "Organization", "An organization", []shoal.ID{name.ID()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	memberOf, err := ontology.NewRelationshipDefinition(
		"member_of", "Member of", "Person belongs to organization",
		[]shoal.ID{person.ID()}, []shoal.ID{organization.ID()},
		[]shoal.ID{role.ID()}, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	schema, err := ontology.NewOntologySchema(
		"workspace", "Workspace", "Workspace ontology", nil)
	if err != nil {
		t.Fatal(err)
	}
	version, err := ontology.NewOntologyVersion(
		schema, "v1", time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC),
		[]ontology.ConceptDefinition{organization, person},
		[]ontology.RelationshipDefinition{memberOf},
		[]ontology.PropertyDefinition{role, age, name},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	return version
}

func emptyOntologyVersion(t *testing.T) ontology.OntologyVersion {
	t.Helper()
	schema, err := ontology.NewOntologySchema("empty", "Empty", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	version, err := ontology.NewOntologyVersion(
		schema, "v1", time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC),
		nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return version
}

func oversizedOntologyVersion(t *testing.T) ontology.OntologyVersion {
	t.Helper()
	concepts := make([]ontology.ConceptDefinition, 0, webapi.MaxOntologyConcepts+1)
	for i := uint32(0); i <= webapi.MaxOntologyConcepts; i++ {
		concept, err := ontology.NewConceptDefinition(
			"concept-"+strconvUint(i), "Concept "+strconvUint(i), "", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		concepts = append(concepts, concept)
	}
	schema, err := ontology.NewOntologySchema("oversized", "Oversized", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	version, err := ontology.NewOntologyVersion(
		schema, "v1", time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC),
		concepts, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return version
}

func constraintsByKind(
	properties []webapi.OntologyPropertyProjection,
) map[string]webapi.OntologyConstraintProjection {
	result := make(map[string]webapi.OntologyConstraintProjection)
	for _, property := range properties {
		for _, constraint := range property.Constraints {
			result[constraint.Kind] = constraint
		}
	}
	return result
}

func sortedKeys(values map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func equalStrings(left, right []string) bool {
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

func strconvUint(value uint32) string {
	return strconv.FormatUint(uint64(value), 10)
}
