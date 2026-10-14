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

package ontology_test

import (
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestUndirectedRelationshipEndpointOrderDoesNotChangeVersionIdentity(t *testing.T) {
	fixture := newOntologyFixture(t)
	forward, err := ontology.NewRelationshipDefinition(
		"related", "Related", "", []shoal.ID{fixture.person.ID()},
		[]shoal.ID{fixture.project.ID()}, nil, false, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	reversed, err := ontology.NewRelationshipDefinition(
		"related", "Related", "", []shoal.ID{fixture.project.ID()},
		[]shoal.ID{fixture.person.ID()}, nil, false, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	createdAt := testTime.Add(time.Hour)
	first, err := ontology.NewOntologyVersion(
		fixture.schema, "undirected-identity", createdAt,
		fixture.version.Concepts(), []ontology.RelationshipDefinition{forward},
		fixture.version.Properties(), nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ontology.NewOntologyVersion(
		fixture.schema, "undirected-identity", createdAt,
		fixture.version.Concepts(), []ontology.RelationshipDefinition{reversed},
		fixture.version.Properties(), nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID() != second.ID() {
		t.Fatal("swapped undirected endpoints changed ontology version identity")
	}
}
