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

package webapi

import (
	"context"
	"reflect"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/workspace"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// GovernedOntologyChoices adapts the live RDO publication state to the
// workspace settings eligibility contract.
type GovernedOntologyChoices struct {
	source OntologyCatalogProvider
}

// NewGovernedOntologyChoices constructs a live, read-only choice adapter.
// source must expose a trusted caller-independent catalog; request-scoped
// authorization is applied separately to each workspace operation.
func NewGovernedOntologyChoices(
	source OntologyCatalogProvider,
) (*GovernedOntologyChoices, error) {
	if absentOntologyCatalogProvider(source) {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"governed ontology source is required",
		)
	}
	return &GovernedOntologyChoices{source: source}, nil
}

// ListOntologyChoices returns only the active ontology and its retained
// published ancestry. Disconnected or unpublished proposal versions are never
// selectable.
func (c *GovernedOntologyChoices) ListOntologyChoices(
	ctx context.Context,
	decision auth.Decision,
) ([]workspace.OntologyChoice, error) {
	catalog, configured, err := c.catalog(ctx)
	if err != nil {
		return nil, err
	}
	if !configured {
		return []workspace.OntologyChoice{}, nil
	}
	active := catalog.ActiveIdentity()
	versions := catalog.Versions()
	choices := make([]workspace.OntologyChoice, 0, len(versions))
	selected, selectedSet := decision.SelectedOntology()
	for index := len(versions) - 1; index >= 0; index-- {
		version := versions[index]
		identity, err := ontology.NewOntologyIdentity(version)
		if err != nil {
			return nil, err
		}
		if selectedSet && identity != selected {
			continue
		}
		choices = append(choices, workspace.OntologyChoice{
			Identity: identity,
			Version:  version.Version(),
			Active:   identity == active,
		})
	}
	return choices, nil
}

// AuthorizeOntology permits only identities returned by the current live
// eligibility snapshot.
func (c *GovernedOntologyChoices) AuthorizeOntology(
	ctx context.Context,
	decision auth.Decision,
	identity ontology.OntologyIdentity,
) error {
	if err := identity.Validate(); err != nil {
		return err
	}
	if selected, ok := decision.SelectedOntology(); ok && selected != identity {
		return shoal.NewError(shoal.ErrorUnauthorized, "authorization denied")
	}
	catalog, configured, err := c.catalog(ctx)
	if err != nil {
		return err
	}
	if configured && catalog.Contains(identity) {
		return nil
	}
	return shoal.NewError(shoal.ErrorUnauthorized, "authorization denied")
}

// AuthorizeOntologyForOperation checks one identity through the service's
// non-disclosing membership seam when available. Catalog listing remains
// reserved for explicit settings management reads and writes.
func (c *GovernedOntologyChoices) AuthorizeOntologyForOperation(
	ctx context.Context,
	decision auth.Decision,
	identity ontology.OntologyIdentity,
	operation auth.Operation,
) error {
	if err := identity.Validate(); err != nil {
		return err
	}
	if selected, ok := decision.SelectedOntology(); ok && selected != identity {
		return shoal.NewError(shoal.ErrorUnauthorized, "authorization denied")
	}
	if authorizer, ok := c.source.(OntologySelectionAuthorizer); ok {
		return authorizer.AuthorizeOntologySelection(ctx, identity, operation)
	}
	return c.AuthorizeOntology(ctx, decision, identity)
}

func (c *GovernedOntologyChoices) catalog(
	ctx context.Context,
) (ontology.PublishedCatalog, bool, error) {
	return c.source.OntologyCatalog(ctx)
}

func absentOntologyCatalogProvider(
	value OntologyCatalogProvider,
) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
