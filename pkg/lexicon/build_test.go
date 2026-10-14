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

package lexicon_test

import (
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/lexicon"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func buildNodes(nodes ...graph.Node) (*lexicon.Bundle, error) {
	return lexicon.Build(lexicon.Input{Snapshot: fixedSnapshot, Nodes: nodes}, lexicon.Limits{})
}

func TestBuildAcceptsCherokeeInEitherCase(t *testing.T) {
	bundle, err := buildNodes(graph.Node{ID: "tsalagi", Properties: shoal.Metadata{
		"name": "ᏣᎳᎩ", "shoal.lexicon.alias.0": "ꮳꮃꭹ ꮧꮂꮝꮧ",
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"ᏣᎳᎩ", "ꮳꮃꭹ", "ᏣᎳᎩ ᏗᏂᏍᏗ"} {
		if got := bundle.Candidates(text); len(got) == 0 || got[0].NodeIDs[0] != "tsalagi" {
			t.Fatalf("Candidates(%q) = %#v", text, got)
		}
	}
}

func TestBuildFailsNamingNodeAndProperty(t *testing.T) {
	cases := map[string]struct {
		node     graph.Node
		property string
	}{
		"invalid utf8 name": {graph.Node{ID: "bad-name", Properties: shoal.Metadata{
			"name": "pay\xffments"}}, "name"},
		"invalid utf8 alias": {graph.Node{ID: "bad-alias", Properties: shoal.Metadata{
			"name": "ok", "shoal.lexicon.alias.3": "\xc3"}}, "shoal.lexicon.alias.3"},
		"name and title yield nothing": {graph.Node{ID: "empty", Properties: shoal.Metadata{
			"name": "🚀", "title": "--"}}, "name"},
		"title only yields nothing": {graph.Node{ID: "empty-title", Properties: shoal.Metadata{
			"title": "!!"}}, "title"},
		"alias yields nothing": {graph.Node{ID: "empty-alias", Properties: shoal.Metadata{
			"name": "fine", "shoal.lexicon.alias.0": "..."}}, "shoal.lexicon.alias.0"},
	}
	for name, tc := range cases {
		_, err := buildNodes(tc.node)
		if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) ||
			!strings.Contains(err.Error(), string(tc.node.ID)) ||
			!strings.Contains(err.Error(), tc.property) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
}

func TestNameWithoutTokensFallsBackToTitle(t *testing.T) {
	bundle, err := buildNodes(graph.Node{ID: "n", Properties: shoal.Metadata{
		"name": "🚀🚀", "title": "Launch Pad",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := bundle.Candidates("launch pad"); len(got) != 1 || got[0].NodeIDs[0] != "n" {
		t.Fatalf("title fallback = %#v", got)
	}
	// A node with no name, title, key or alias contributes nothing and is not
	// an error.
	if _, err := buildNodes(graph.Node{ID: "plain", Properties: shoal.Metadata{
		"owner": "x"}}); err != nil {
		t.Fatal(err)
	}
}

func TestDuplicateRelationshipKeysWithDifferentDirectednessFail(t *testing.T) {
	concept, err := ontology.NewConceptDefinition("thing", "Thing", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ids := []shoal.ID{concept.ID()}
	directed, err := ontology.NewRelationshipDefinition(
		"links", "links", "", ids, ids, nil, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	undirected, err := ontology.NewRelationshipDefinition(
		"links", "links", "", ids, ids, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lexicon.DeriveTemplates([]ontology.RelationshipDefinition{
		directed, undirected,
	}); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("mixed-directedness duplicate: err = %v", err)
	}
}
