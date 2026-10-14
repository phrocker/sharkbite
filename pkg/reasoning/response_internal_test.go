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

package reasoning

import (
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestResponseFingerprintIncludesPerSourceVisibility(t *testing.T) {
	data := responseData{
		sources: []SourceReference{{
			id:         "source",
			anchorIDs:  []shoal.ID{"anchor"},
			visibility: []string{"internal"},
		}},
	}
	internal, err := responseFingerprint(data)
	if err != nil {
		t.Fatal(err)
	}
	data.sources[0].visibility = []string{"secret"}
	secret, err := responseFingerprint(data)
	if err != nil {
		t.Fatal(err)
	}
	if secret == internal {
		t.Fatal("per-source visibility did not change response fingerprint")
	}
}

func TestCanonicalResponseIdentityRejectsMalformedEvidence(t *testing.T) {
	identity := ResponseIdentity{
		RetrievedEvidence: []interaction.EvidenceReference{{
			AnchorID: "anchor",
			Kind:     interaction.EvidenceDocument,
		}},
	}
	if _, err := ResponseFingerprint(identity); err == nil ||
		!strings.Contains(err.Error(), "retrieved evidence") {
		t.Fatalf("fingerprint error = %v", err)
	}
	if _, err := CanonicalResponseID(
		"session", time.Unix(1, 0), identity,
	); err == nil || !strings.Contains(err.Error(), "retrieved evidence") {
		t.Fatalf("response ID error = %v", err)
	}
}

func TestInteractionEvidenceEqualityPreservesGraphOrder(t *testing.T) {
	base := interaction.EvidenceReference{
		AnchorID: "anchor",
		Kind:     interaction.EvidenceGraph,
		NodeIDs:  []shoal.ID{"node-a", "node-b", "node-c"},
		EdgeIDs:  []shoal.ID{"edge-a", "edge-b"},
	}
	reorderedNodes := base
	reorderedNodes.NodeIDs = []shoal.ID{"node-c", "node-b", "node-a"}
	if interactionEvidenceEqual(base, reorderedNodes) {
		t.Fatal("reordered graph nodes compared equal")
	}
	reorderedEdges := base
	reorderedEdges.EdgeIDs = []shoal.ID{"edge-b", "edge-a"}
	if interactionEvidenceEqual(base, reorderedEdges) {
		t.Fatal("reordered graph edges compared equal")
	}
}

func TestResponseFingerprintCanonicalizesEmbeddingSpaceSet(t *testing.T) {
	canonical, err := interaction.NewEmbeddingSpaceSet(
		[]string{"space-b", "space-a"})
	if err != nil {
		t.Fatal(err)
	}
	left, err := ResponseFingerprint(ResponseIdentity{
		EmbeddingSpaces: canonical,
	})
	if err != nil {
		t.Fatal(err)
	}
	right, err := ResponseFingerprint(ResponseIdentity{
		EmbeddingSpaces: interaction.EmbeddingSpaceSet{
			Identities: []string{"space-b", "space-a", "space-b"},
			Digest:     canonical.Digest,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if left != right {
		t.Fatal("semantically identical embedding-space sets changed fingerprint")
	}
}
