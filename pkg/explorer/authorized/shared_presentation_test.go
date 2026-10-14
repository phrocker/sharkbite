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

package authorized_test

// Co-ownership of a shared entity (#700) never depends on how an extractor
// presented it: the surface text a display name came from, or a confidence.
// Otherwise the second extraction would be refused whenever a hidden
// document presented the entity differently, which is the oracle again.

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/model"
)

// TestSharedEntityIgnoresSurfaceText is the review repro with the default
// heuristic extractor: the hidden document names "zeta", the attacker
// "Zeta". Same entity key, so the same nodes; the attacker's extraction
// succeeds and shows all of its nodes, like a control entity.
func TestSharedEntityIgnoresSurfaceText(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		w := newExtractedEdgeWorld(t, store)
		body := skillBody("zeta", "zeta-cli")
		hidden := w.extract(w.ingest(sharedHiddenURI, body, nil))
		w.relabelSecret(sharedHiddenURI, body)

		attacker := w.f.labelIngester(t, "attacker")
		own, err := w.extractAs(attacker,
			w.ingestAs(attacker, sharedAttackerURI, skillBody("Zeta", "Zeta-cli"), nil))
		if err != nil {
			t.Fatalf("the attacker's extraction of \"Zeta\" failed: %v", err)
		}
		if !reflect.DeepEqual(own.EntityNodeIDs, hidden.EntityNodeIDs) {
			t.Fatalf("fixture: \"Zeta\" and \"zeta\" are not the same entities: %v vs %v",
				own.EntityNodeIDs, hidden.EntityNodeIDs)
		}
		control, err := w.extractAs(attacker,
			w.ingestAs(attacker, sharedControlURI, skillBody("Omega", "Omega-cli"), nil))
		if err != nil {
			t.Fatal(err)
		}
		reader := w.f.labelIngester(t, "attacker")
		if got := visibleCount(t, w.client, reader, own.EntityNodeIDs); got != len(own.EntityNodeIDs) {
			t.Fatalf("attacker sees %d/%d of its own nodes", got, len(own.EntityNodeIDs))
		}
		if got := visibleCount(t, w.client, reader, control.EntityNodeIDs); got != len(control.EntityNodeIDs) {
			t.Fatalf("attacker sees %d/%d of the control nodes", got, len(control.EntityNodeIDs))
		}
	})
}

// presentingGenerator answers an extraction prompt with one "zeta" skill
// entity, presented under its own name and confidence.
type presentingGenerator struct {
	name       string
	confidence float64
}

func (g presentingGenerator) Generate(
	_ context.Context, request model.GenerateRequest,
) (model.GenerateResult, error) {
	_, input, ok := strings.Cut(request.Prompt, "INPUT=")
	if !ok {
		return model.GenerateResult{}, fmt.Errorf("no prompt input")
	}
	var prompt struct {
		Concepts []struct {
			ID, Key string
		} `json:"concepts"`
		Properties []struct {
			ID, Key string
		} `json:"properties"`
		Relations []struct {
			ID, Key string
		} `json:"relations"`
		Evidence []struct {
			ID, Kind string
		} `json:"evidence"`
	}
	if err := json.Unmarshal([]byte(input), &prompt); err != nil {
		return model.GenerateResult{}, err
	}
	var concept, tool, relation, property, anchor string
	for _, c := range prompt.Concepts {
		switch c.Key {
		case "skill":
			concept = c.ID
		case "tool":
			tool = c.ID
		}
	}
	for _, r := range prompt.Relations {
		if r.Key == "provides_tool" {
			relation = r.ID
		}
	}
	for _, p := range prompt.Properties {
		if p.Key == "name" {
			property = p.ID
		}
	}
	for _, e := range prompt.Evidence {
		if e.Kind == "document" {
			anchor = e.ID
		}
	}
	output := map[string]any{
		"entities": []map[string]any{{
			"key": "zeta", "type_id": concept,
			"properties": []map[string]any{{
				"property_id": property,
				"value":       map[string]any{"type": "string", "value": g.name},
			}},
			"confidence":          g.confidence,
			"evidence_anchor_ids": []string{anchor},
		}, {
			"key": "zeta-cli", "type_id": tool,
			"properties": []map[string]any{{
				"property_id": property,
				"value":       map[string]any{"type": "string", "value": g.name + " cli"},
			}},
			"confidence":          g.confidence,
			"evidence_anchor_ids": []string{anchor},
		}},
		"relations": []map[string]any{{
			"type_id": relation, "from_entity_key": "zeta", "to_entity_key": "zeta-cli",
			"properties": []any{}, "confidence": g.confidence,
			"evidence_anchor_ids": []string{anchor},
		}},
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		return model.GenerateResult{}, err
	}
	return model.GenerateResult{
		Text:       string(encoded),
		Provenance: model.Provenance{Provider: "test", Model: "presenting"},
	}, nil
}

// TestSharedEntityIgnoresGeneratorPresentation: the same with a model
// generator that presents the entity under another name and confidence.
func TestSharedEntityIgnoresGeneratorPresentation(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		w := newExtractedEdgeWorld(t, store)
		extract := func(ctx context.Context, result explorer.IngestResult,
			generator presentingGenerator) (explorer.ExtractionResult, error) {
			return w.client.ExtractDocument(ctx, explorer.ExtractionRequest{
				DocumentID: result.Document.ID, RevisionID: result.Revision.ID,
				Version: authorizedSkillsOntologyVersion(t), Generator: generator,
			})
		}
		body := "# zeta notes\n\nzeta is here.\n"
		hidden, err := extract(w.reader, w.ingest(sharedHiddenURI, body, nil),
			presentingGenerator{name: "zeta", confidence: 0.9})
		if err != nil {
			t.Fatal(err)
		}
		w.relabelSecret(sharedHiddenURI, body)
		attacker := w.f.labelIngester(t, "attacker")
		own, err := extract(attacker, w.ingestAs(attacker, sharedAttackerURI, body, nil),
			presentingGenerator{name: "ZETA (the attacker's)", confidence: 0.25})
		if err != nil {
			t.Fatalf("the attacker's differently presented extraction failed: %v", err)
		}
		if len(own.EntityNodeIDs) != 2 || !reflect.DeepEqual(own.EntityNodeIDs, hidden.EntityNodeIDs) {
			t.Fatalf("fixture: entities %v vs %v", own.EntityNodeIDs, hidden.EntityNodeIDs)
		}
		if len(own.RelationshipEdgeIDs) != 1 ||
			!reflect.DeepEqual(own.RelationshipEdgeIDs, hidden.RelationshipEdgeIDs) {
			t.Fatalf("fixture: relations %v vs %v", own.RelationshipEdgeIDs, hidden.RelationshipEdgeIDs)
		}
		reader := w.f.labelIngester(t, "attacker")
		if !nodeVisible(t, w.client, reader, own.EntityNodeIDs[0]) {
			t.Fatal("the attacker lost its own entity")
		}
		for _, edge := range own.GraphEdges {
			if edge.ID == own.RelationshipEdgeIDs[0] && edge.Weight != 1 {
				t.Fatalf("a shared relation carries the extractor's confidence: %v", edge.Weight)
			}
		}
		for _, node := range own.GraphNodes {
			for _, label := range node.Labels {
				if strings.Contains(label, "attacker") {
					t.Fatalf("a shared node carries extractor presentation: %v", node.Labels)
				}
			}
			if _, ok := node.Properties["confidence"]; ok {
				t.Fatalf("a shared node carries a confidence: %v", node.Properties)
			}
		}
	})
}
