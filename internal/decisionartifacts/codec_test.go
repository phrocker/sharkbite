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

package decisionartifacts

import (
	"bytes"
	"encoding/json"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
	"strings"
	"testing"
)

func TestGraphOntologyAndAssertionRoundTrip(t *testing.T) {
	r, _, _, _, _ := fixture(t)
	path := graph.Path{Nodes: []graph.Node{{ID: "a", Kind: "entity", Labels: []string{"person"}, Properties: shoal.Metadata{"x": "y"}}, {ID: "b", Kind: "entity"}}, Edges: []graph.Edge{{ID: "edge", From: "a", To: "b", Type: "related", Weight: 1}}}
	anchor, err := inference.NewGraphAnchorWithAssertions(path, []interaction.AssertionReference{{AssertionID: "assertion", EdgeID: "edge", Origin: ontology.AssertionDerived}})
	if err != nil {
		t.Fatal(err)
	}
	old := r.Bundle.Request.Picture().ContextPack()
	o, err := inference.NewOntologyIdentityFromIDs(shoal.ID("schema:"+strings.Repeat("a", 64)), shoal.ID("ontology-version:"+strings.Repeat("b", 64)))
	if err != nil {
		t.Fatal(err)
	}
	pack, err := inference.NewContextPack(old.Query(), []inference.EvidenceAnchor{anchor}, &o, old.Snapshot(), old.Authorization(), shoal.Metadata{"snapshot": "measured"})
	if err != nil {
		t.Fatal(err)
	}
	pc := r.Bundle.Request.Picture().Config()
	pc.Subjects[0].EvidenceIDs = []shoal.ID{anchor.ID()}
	picture, err := decision.NewPictureManifest(pack, pc)
	if err != nil {
		t.Fatal(err)
	}
	r.Bundle.Request, err = decision.NewDecisionRequest(r.Bundle.Request.Task(), picture, r.Bundle.Request.Predictor(), r.Bundle.Request.Config())
	if err != nil {
		t.Fatal(err)
	}
	b, err := encode(r)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Bundle.Request.ID() != r.Bundle.Request.ID() {
		t.Fatal("changed request")
	}
	ref, err := restored.Bundle.Request.Picture().ContextPack().Evidence()[0].EvidenceReference()
	if err != nil {
		t.Fatal(err)
	}
	if len(ref.Assertions) != 1 || ref.Assertions[0].Origin != ontology.AssertionDerived {
		t.Fatal("assertion lineage lost")
	}
	b2, err := encode(restored)
	if err != nil || !bytes.Equal(b, b2) {
		t.Fatal("codec not exact", err)
	}
}
func TestStrictCodecRejectsRechecksummedSubstitution(t *testing.T) {
	r, _, _, _, _ := fixture(t)
	original, err := encode(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"unknown schema", "duplicate schema", "unknown field", "trailing json", "inactive graph variant", "changed request", "lost anchor", "wrong checksum"} {
		t.Run(mode, func(t *testing.T) {
			var e envelope
			if err := json.Unmarshal(original, &e); err != nil {
				t.Fatal(err)
			}
			var w recordWire
			if err := json.Unmarshal(e.Payload, &w); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "unknown schema":
				e.Schema = 2
			case "inactive graph variant":
				w.Pack.Evidence[0].Path = graph.Path{Nodes: []graph.Node{{ID: "forged", Kind: "entity"}}}
			case "changed request":
				w.Request.ReleaseID = "other-release"
			case "lost anchor":
				w.Pack.Evidence = nil
			}
			e.Payload, _ = json.Marshal(w)
			e.Checksum = digest(e.Payload)
			if mode == "wrong checksum" {
				e.Checksum = "wrong"
			}
			b, _ := json.Marshal(e)
			switch mode {
			case "duplicate schema":
				b = bytes.Replace(b, []byte(`"Schema":1`), []byte(`"Schema":1,"Schema":1`), 1)
			case "unknown field":
				b = append([]byte(`{"Unknown":1,`), b[1:]...)
			case "trailing json":
				b = append(b, []byte(`{}`)...)
			}
			if _, err := decode(b); err == nil {
				t.Fatal("corrupt artifact accepted")
			}
		})
	}
}
