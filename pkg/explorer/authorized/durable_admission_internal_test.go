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
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// lowerPolicyRecordBounds lowers the persistence bound to persist bytes and
// the admission headroom to 1 KiB, so an oversize record that skipped
// admission would really fail to persist (and diverge the store).
func lowerPolicyRecordBounds(t *testing.T, persist uint64) {
	t.Helper()
	previousPersist, previousHeadroom := policyRecordPersistBytes, policyRecordAdmissionHeadroom
	policyRecordPersistBytes, policyRecordAdmissionHeadroom = persist, 1<<10
	t.Cleanup(func() {
		policyRecordPersistBytes, policyRecordAdmissionHeadroom = previousPersist, previousHeadroom
	})
}

// TestOversizeRecordsAreRefusedWithoutDiverging pins that a record too large
// to persist is refused before memory changes, as an ordinary per-request
// error. The store stays healthy and holds nothing of it.
func TestOversizeRecordsAreRefusedWithoutDiverging(t *testing.T) {
	ctx := context.Background()
	rule := batchTestRule(t, "source", "policy")
	many := func(prefix string, count int) []shoal.ID {
		ids := make([]shoal.ID, count)
		for index := range ids {
			ids[index] = shoal.ID(prefix + strings.Repeat("x", 40) + string(rune('a'+index%26)) +
				strings.Repeat("y", index%7))
		}
		return ids
	}
	// Each value stays under the graph property bound; together they exceed
	// the lowered record bound.
	big := shoal.Metadata{
		"a": strings.Repeat("v", 3000), "b": strings.Repeat("v", 3000),
		"c": strings.Repeat("v", 3000),
	}
	// A revision that fits the persistence bound but not the admission bound
	// (inside the headroom kept for later rule growth).
	inHeadroom := func(t *testing.T) RevisionRegistration {
		t.Helper()
		for count := 1; count < 1000; count++ {
			registration := RevisionRegistration{
				DocumentID: "headroom", RevisionID: "headroom-rev",
				NodeIDs:       append([]shoal.ID{"headroom"}, many("node-", count)...),
				ContentDigest: auth.DigestBytes("test-content", []byte("headroom")),
				Rule:          rule, Current: true,
			}
			encoded, err := encodePolicyRecord(policyKindRevision, persistedRevision{
				DocumentID: "headroom", RevisionID: "headroom-rev",
				NodeIDs: stringsFromIDs(registration.NodeIDs),
				Rule:    ruleToPersistedOrEmpty(rule),
			})
			if err != nil {
				break
			}
			size := uint64(len(encoded) - policyEnvelopeHeader)
			if size > policyRecordAdmissionBytes()+64 && size < policyRecordPersistBytes-64 {
				return registration
			}
		}
		t.Fatal("no registration size falls inside the headroom")
		return RevisionRegistration{}
	}
	for _, test := range []struct {
		name  string
		write func(store *DurablePolicyStore) error
		check func(t *testing.T, store *DurablePolicyStore)
	}{
		{"revision", func(store *DurablePolicyStore) error {
			return store.PutRevision(ctx, RevisionRegistration{
				DocumentID: "big", RevisionID: "big-rev",
				NodeIDs:       append([]shoal.ID{"big"}, many("node-", 400)...),
				ContentDigest: auth.DigestBytes("test-content", []byte("big")),
				Rule:          rule, Current: true,
			})
		}, func(t *testing.T, store *DurablePolicyStore) {
			if _, ok, err := store.CurrentRevision(ctx, "big"); err != nil || ok {
				t.Fatalf("refused revision registered: %v %v", ok, err)
			}
		}},
		{"revision inside the headroom", func(store *DurablePolicyStore) error {
			return store.PutRevision(ctx, inHeadroom(t))
		}, func(t *testing.T, store *DurablePolicyStore) {
			if _, ok, err := store.CurrentRevision(ctx, "headroom"); err != nil || ok {
				t.Fatalf("refused revision registered: %v %v", ok, err)
			}
		}},
		{"application edge", func(store *DurablePolicyStore) error {
			return store.PutEdge(ctx, EdgeRegistration{
				Edge: graph.Edge{ID: "big-edge", From: "document", To: "node-a",
					Type: "links", Weight: 1, Properties: big},
				Rule: rule,
			})
		}, func(t *testing.T, store *DurablePolicyStore) {
			if _, ok, err := store.Edge(ctx, "big-edge"); err != nil || ok {
				t.Fatalf("refused edge registered: %v %v", ok, err)
			}
		}},
		{"edge reservation", func(store *DurablePolicyStore) error {
			return store.ReserveEdge(ctx, EdgeRegistration{
				Edge: graph.Edge{ID: "big-reserved", From: "document", To: "node-a",
					Type: "links", Weight: 1, Properties: big},
				Rule: rule,
			})
		}, func(t *testing.T, store *DurablePolicyStore) {
			// A refused reservation leaves the ID free.
			if err := store.ReserveEdge(ctx, EdgeRegistration{
				Edge: graph.Edge{ID: "big-reserved", From: "document", To: "node-a",
					Type: "links", Weight: 1},
				Rule: rule,
			}); err != nil {
				t.Fatalf("reserving the ID after a refusal: %v", err)
			}
		}},
		{"extracted node", func(store *DurablePolicyStore) error {
			return store.PutNode(ctx, "big-node", NodeRegistration{
				DocumentID: "document", RevisionID: "revision",
				Node: graph.Node{ID: "big-node", Kind: "entity", Properties: big},
				Rule: rule, Kind: RegistrationExtracted,
			})
		}, func(t *testing.T, store *DurablePolicyStore) {
			if _, ok, err := store.Node(ctx, "big-node"); err != nil || ok {
				t.Fatalf("refused node registered: %v %v", ok, err)
			}
		}},
		{"co-occurrence", func(store *DurablePolicyStore) error {
			domains := make([]string, 400)
			for index := range domains {
				domains[index] = strings.Repeat("d", 30) + string(rune('a'+index%26))
			}
			return store.StoreCoOccurrence(ctx, "identity", CoOccurrenceRecord{
				WindowStart: time.Date(2026, time.August, 26, 18, 0, 0, 0, time.UTC),
				Domains:     domains,
			})
		}, func(t *testing.T, store *DurablePolicyStore) {
			if _, ok, err := store.LoadCoOccurrence(ctx, "identity"); err != nil || ok {
				t.Fatalf("refused co-occurrence stored: %v %v", ok, err)
			}
		}},
		{"label migration marker", func(store *DurablePolicyStore) error {
			record := LabelMigrationRecord{Version: 1}
			for index := 0; index < 200; index++ {
				record.Untranslatable = append(record.Untranslatable, UntranslatableLabel{
					DocumentID: "document", RevisionID: "revision",
					SourceURI: "file:///x", EscapedLabel: strings.Repeat("l", 40),
					Reason: "too long",
				})
			}
			return store.PutLabelMigration(ctx, record)
		}, func(t *testing.T, store *DurablePolicyStore) {
			if _, ok, err := store.LabelMigration(ctx); err != nil || ok {
				t.Fatalf("refused migration marker stored: %v %v", ok, err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			lowerPolicyRecordBounds(t, 5<<10)
			store, err := OpenDurablePolicyStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			seedDurableStore(t, store)
			fired := 0
			store.OnDiverged(func() { fired++ })
			if err := test.write(store); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
				t.Fatalf("oversize write = %v, want invalid argument", err)
			}
			if store.Diverged() || fired != 0 {
				t.Fatalf("an oversize write diverged the store (fired=%d)", fired)
			}
			if _, ok, err := store.CurrentRevision(ctx, "document"); err != nil || !ok {
				t.Fatalf("store unhealthy after an oversize write: %v %v", ok, err)
			}
			test.check(t, store)
		})
	}
}

// TestOversizeRegistrationFailsOnlyThatIngest drives an ingest whose
// revision registration is too large to persist. That ingest fails. The
// catalog stays healthy for every other request. The claim is left pending,
// as for any failure after the base write (#665). A retry once the record
// fits completes it.
func TestOversizeRegistrationFailsOnlyThatIngest(t *testing.T) {
	now := time.Date(2026, time.September, 2, 18, 0, 0, 0, time.UTC)
	store, err := OpenDurablePolicyStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	base, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = base.Close() })
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "subject", Actor: "actor",
		AuthorizationDomain: []byte("domain"),
		AllowedOperations: []auth.Operation{
			auth.OperationIngest, auth.OperationList, auth.OperationRead,
		},
		PermittedSourceIDs:    [][]byte{[]byte("source")},
		PermittedPolicyIDs:    [][]byte{[]byte("policy")},
		PolicyGeneration:      1,
		AuthenticationExpires: now.Add(time.Hour),
		RequestID:             "request",
	})
	if err != nil {
		t.Fatal(err)
	}
	selector, err := NewStaticPolicySelector([]byte("source"), []byte("policy"))
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(Config{
		Base: base,
		Resolver: resolverFunc(func(context.Context) (auth.Decision, error) {
			return decision, nil
		}),
		PolicySelector: selector,
		PolicyStore:    store,
		GenerationReader: generationReaderFunc(
			func(context.Context, []byte) (int64, error) { return 1, nil }),
		Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	small := explorer.Source{URI: "file:///small.txt", MediaType: explorer.MediaTypeText, Content: "small"}
	if _, err := client.Ingest(ctx, small); err != nil {
		t.Fatal(err)
	}
	// Many sections and spans: one revision record holding every node ID.
	dense := explorer.Source{
		URI: "file:///dense.md", MediaType: explorer.MediaTypeMarkdown,
		Content: strings.Repeat("# h\n\nb\n\n", 200),
	}
	previousPersist, previousHeadroom := policyRecordPersistBytes, policyRecordAdmissionHeadroom
	lowerPolicyRecordBounds(t, 9<<10)
	if _, err := client.Ingest(ctx, dense); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("oversize registration = %v, want invalid argument", err)
	}
	if store.Diverged() {
		t.Fatal("an oversize registration diverged the catalog")
	}
	// Every other request still works.
	if _, err := client.Ingest(ctx, explorer.Source{
		URI: "file:///other.txt", MediaType: explorer.MediaTypeText, Content: "other",
	}); err != nil {
		t.Fatalf("an unrelated ingest after the refusal: %v", err)
	}
	if documents, err := client.Documents(ctx); err != nil || len(documents) != 2 {
		t.Fatalf("documents after the refusal = %d %v, want the two registered", len(documents), err)
	}
	claim, ok, err := store.SourceClaim(ctx, dense.URI)
	if err != nil || !ok || !claim.Pending {
		t.Fatalf("claim after the refusal = %+v %v %v, want pending", claim, ok, err)
	}
	if _, ok, err := store.CurrentRevision(ctx, explorer.SourceDocumentID(dense.URI)); err != nil || ok {
		t.Fatalf("the refused registration exists: %v %v", ok, err)
	}
	policyRecordPersistBytes, policyRecordAdmissionHeadroom = previousPersist, previousHeadroom
	if _, err := client.Ingest(ctx, dense); err != nil {
		t.Fatalf("retry once the record fits: %v", err)
	}
	if claim, _, err := store.SourceClaim(ctx, dense.URI); err != nil || claim.Pending {
		t.Fatalf("claim after the retry = %+v %v, want committed", claim, err)
	}
}

// TestOversizeSourceURIIsRefusedWithoutDiverging pins the claim side of the
// admission rule: the source URI is caller input carried by the claim
// record, so a URI too long to persist is refused before the claim CAS
// changes memory, and the catalog stays healthy.
func TestOversizeSourceURIIsRefusedWithoutDiverging(t *testing.T) {
	lowerPolicyRecordBounds(t, 5<<10)
	store, err := OpenDurablePolicyStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	seedDurableStore(t, store)
	ctx := context.Background()
	uri := "file:///" + strings.Repeat("u", 6<<10)
	rule := batchTestRule(t, "source", "policy")
	if _, err := store.CompareAndSwapSourceClaim(ctx, uri, nil, rule); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("oversize claim = %v, want invalid argument", err)
	}
	if store.Diverged() {
		t.Fatal("an oversize claim diverged the catalog")
	}
	if _, ok, err := store.SourceClaim(ctx, uri); err != nil || ok {
		t.Fatalf("refused claim recorded: %v %v", ok, err)
	}
	if _, err := store.CompareAndSwapSourceClaim(ctx, "file:///fits", nil, rule); err != nil {
		t.Fatalf("an ordinary claim after the refusal: %v", err)
	}
}
