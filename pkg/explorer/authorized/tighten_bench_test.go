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

package authorized

import (
	"context"
	"strconv"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// The cost of the label migration's catalog work (#570 review). A catalog of
// 1M extracted nodes and 1M edges is spread over 20,000 documents (50 nodes
// and 50 edges each: 25 extracted relations bound to the document and 25
// legacy application edges between its entities). The benchmarks measure one
// TightenRule call:
//
//   - Scan: no index, so every call walks the whole catalog (the cost before
//     the review fix);
//   - Indexed: the index one migration run builds once, so a call visits only
//     its document's registrations.
//
// BenchmarkTighteningIndexBuild is the once-per-run cost of the index. A run
// over L labelled documents costs about L calls plus one build.
//
//	go test -run '^$' -bench 'Tighten' -benchtime 20x ./pkg/explorer/authorized/

const (
	benchDocuments    = 20_000
	benchNodesPerDoc  = 50
	benchEdgesPerDoc  = 50
	benchLabelledDocs = 10_000
)

type tightenBench struct {
	store  *MemoryPolicyStore
	bare   AccessRule
	secret AccessRule
}

func newTightenBench(b *testing.B) tightenBench {
	b.Helper()
	source, err := auth.NewPolicy(auth.PolicyConfig{
		AuthorizationDomain: []byte("domain"), SourceID: []byte("source-a"),
		GrantPolicyID: []byte("policy-a"), Epoch: 1,
	})
	if err != nil {
		b.Fatal(err)
	}
	bare, err := NewAccessRule(source)
	if err != nil {
		b.Fatal(err)
	}
	secret, err := LabelRule(source, []string{"secret"})
	if err != nil {
		b.Fatal(err)
	}
	store := NewMemoryPolicyStore()
	for document := 0; document < benchDocuments; document++ {
		documentID := shoal.ID("doc-" + strconv.Itoa(document))
		revisionID := shoal.ID("rev-" + strconv.Itoa(document))
		key := revisionKey{documentID: documentID, revisionID: revisionID}
		store.revisions[key] = RevisionRegistration{
			DocumentID: documentID, RevisionID: revisionID,
			NodeIDs:       []shoal.ID{documentID},
			ContentDigest: auth.DigestBytes("bench", []byte(documentID)), Rule: bare,
		}
		store.revisionIDs[revisionID] = key
		store.current[documentID] = key
		store.nodes[documentID] = NodeRegistration{
			DocumentID: documentID, RevisionID: revisionID, Rule: bare,
			Kind: RegistrationDocument,
		}
		prefix := string(documentID) + "/"
		for node := 0; node < benchNodesPerDoc; node++ {
			store.nodes[shoal.ID(prefix+"entity-"+strconv.Itoa(node))] = NodeRegistration{
				DocumentID: documentID, RevisionID: revisionID, Rule: bare,
				Kind: RegistrationExtracted,
			}
		}
		for edge := 0; edge < benchEdgesPerDoc; edge++ {
			id := shoal.ID(prefix + "edge-" + strconv.Itoa(edge))
			registration := EdgeRegistration{
				Edge: graph.Edge{
					ID:   id,
					From: shoal.ID(prefix + "entity-" + strconv.Itoa(edge%benchNodesPerDoc)),
					To:   shoal.ID(prefix + "entity-" + strconv.Itoa((edge+1)%benchNodesPerDoc)),
					Type: "uses", Weight: 1,
				},
				Rule: bare, Kind: RegistrationApplication,
			}
			if edge%2 == 0 {
				registration.Kind = RegistrationExtracted
				registration.DocumentID, registration.RevisionID = documentID, revisionID
			}
			store.edges[id] = registration
		}
	}
	return tightenBench{store: store, bare: bare, secret: secret}
}

func (t tightenBench) tighten(b *testing.B, index *TighteningIndex, document int) {
	documentID := shoal.ID("doc-" + strconv.Itoa(document%benchLabelledDocs))
	if _, err := t.store.TightenRule(context.Background(), index, RuleTightening{
		DocumentID: documentID,
		RevisionID: shoal.ID("rev-" + strconv.Itoa(document%benchLabelledDocs)),
		SourceURI:  "file:///" + string(documentID),
		From:       t.bare, To: t.secret,
	}); err != nil {
		b.Fatal(err)
	}
}

// BenchmarkTightenRulePreFixScan reproduces the scope selection TightenRule
// used before the review fix: one walk over every revision, node and edge of
// the catalog per call, selecting by document and kind.
func BenchmarkTightenRulePreFixScan(b *testing.B) {
	bench := newTightenBench(b)
	delta := bench.secret.components()[:0]
	for index, key := range bench.secret.keys {
		if !ruleHasKey(bench.bare, key) {
			delta = append(delta, bench.secret.policies[index])
		}
	}
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		documentID := shoal.ID("doc-" + strconv.Itoa(iteration%benchLabelledDocs))
		bench.store.mu.Lock()
		matched := 0
		for key, registration := range bench.store.revisions {
			if key.documentID == documentID {
				if _, _, err := conjoinDelta(registration.Rule, delta); err != nil {
					b.Fatal(err)
				}
				matched++
			}
		}
		for _, registration := range bench.store.nodes {
			if registration.Kind == RegistrationExtracted && registration.DocumentID == documentID {
				if _, _, err := conjoinDelta(registration.Rule, delta); err != nil {
					b.Fatal(err)
				}
				matched++
			}
		}
		for _, registration := range bench.store.edges {
			if registration.Kind == RegistrationExtracted && registration.DocumentID == documentID {
				if _, _, err := conjoinDelta(registration.Rule, delta); err != nil {
					b.Fatal(err)
				}
				matched++
			}
		}
		bench.store.mu.Unlock()
		if matched == 0 {
			b.Fatal("scan matched nothing")
		}
	}
}

// BenchmarkTightenRuleScan is a call with no index: it builds one itself.
func BenchmarkTightenRuleScan(b *testing.B) {
	bench := newTightenBench(b)
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		bench.tighten(b, nil, iteration)
	}
}

// BenchmarkTightenRuleIndexedRerun is a later start: every document is
// already tight, so each call only confirms it.
func BenchmarkTightenRuleIndexedRerun(b *testing.B) {
	bench := newTightenBench(b)
	index, err := bench.store.TighteningIndex(context.Background())
	if err != nil {
		b.Fatal(err)
	}
	for iteration := 0; iteration < b.N; iteration++ {
		bench.tighten(b, index, iteration)
	}
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		bench.tighten(b, index, iteration)
	}
}

func BenchmarkTightenRuleIndexed(b *testing.B) {
	bench := newTightenBench(b)
	index, err := bench.store.TighteningIndex(context.Background())
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		bench.tighten(b, index, iteration)
	}
}

func BenchmarkTighteningIndexBuild(b *testing.B) {
	bench := newTightenBench(b)
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		if _, err := bench.store.TighteningIndex(context.Background()); err != nil {
			b.Fatal(err)
		}
	}
}
