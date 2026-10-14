/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements. See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership. The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License. You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package explorer

import (
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/interaction"
)

func TestExtractionRejectsReservedInteractionNamespaces(t *testing.T) {
	base := persistedExtraction{
		ID: "extraction", DocumentID: "document", RevisionID: "revision",
		OntologySchemaID: "schema", OntologyVersionID: "version",
		PublishedAt: time.Unix(1700000000, 0).UTC(),
	}
	withNode := base
	withNode.Nodes = []graph.Node{{
		ID: interaction.DerivedID("session", "squat"), Kind: "entity",
	}}
	if err := validatePersistedExtraction(withNode); err == nil {
		t.Fatal("extraction accepted a reserved interaction node ID")
	}
	withKind := base
	withKind.Nodes = []graph.Node{{
		ID: "ordinary-node", Kind: interaction.KindSession,
	}}
	if err := validatePersistedExtraction(withKind); err == nil {
		t.Fatal("extraction accepted a reserved interaction node kind")
	}
	withEdge := base
	withEdge.Nodes = []graph.Node{{ID: "ordinary-node", Kind: "entity"}}
	withEdge.Edges = []graph.Edge{{
		ID:   interaction.DerivedID("edge", "squat"),
		From: base.DocumentID, To: "ordinary-node", Type: "related",
	}}
	if err := validatePersistedExtraction(withEdge); err == nil {
		t.Fatal("extraction accepted a reserved interaction edge ID")
	}
}
