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

// Ingest-side enforcement of free-form visibility labels (#570, PR2): the
// ingester must hold every label it writes, relabelling needs the old and the
// new labels, older revisions close behind a relabel, the development
// backfill never registers a labelled document unlabelled, and a graph
// materialization cannot carry labels it would not enforce.

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/internal/devbackfill"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// labelIngester mints a context holding source A, source B and exactly the
// given (source A) label grants.
func (f *fixture) labelIngester(t testing.TB, subject string, labels ...string) context.Context {
	t.Helper()
	policies := [][]byte{f.policyA, f.policyB}
	for _, label := range labels {
		policies = append(policies, labelPolicy(t, f.sourceA, label))
	}
	return f.context(t, f.decision(t, subject,
		[][]byte{f.sourceA, f.sourceB}, policies, labelReaderOperations))
}

func labelledSource(uri, content, visibility string) explorer.Source {
	source := explorer.Source{
		URI: uri, MediaType: explorer.MediaTypeText, Content: content,
	}
	if visibility != "" {
		source.Metadata = shoal.Metadata{interaction.PropertyVisibility: visibility}
	}
	return source
}

func baseDocumentCount(t *testing.T, f *fixture) int {
	t.Helper()
	summaries, err := f.base.Documents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return len(summaries)
}

func TestLabelIngestRequiresEveryLabelGrant(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		f := newFixture(t)
		client := f.newClient(t, f.base, store, f.sourceA, f.policyA, nil)
		source := labelledSource("file:///label/needs-both.txt", "needs both labels", "secret&x")
		for name, ctx := range map[string]context.Context{
			"no label grant":    f.labelIngester(t, "ingester-none"),
			"one of two labels": f.labelIngester(t, "ingester-secret", "secret"),
			"other label":       f.labelIngester(t, "ingester-x", "x"),
			// A grant for the label on another source does not count.
			"label on source B": f.context(t, f.decision(t, "ingester-b",
				[][]byte{f.sourceA, f.sourceB},
				[][]byte{f.policyA, f.policyB,
					labelPolicy(t, f.sourceB, "secret"), labelPolicy(t, f.sourceB, "x")},
				labelReaderOperations)),
		} {
			if _, err := client.Ingest(ctx, source); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
				t.Fatalf("%s: ingest error = %v, want unauthorized", name, err)
			}
		}
		if count := baseDocumentCount(t, f); count != 0 {
			t.Fatalf("a refused labelled ingest reached the base: %d documents", count)
		}
		if _, err := client.Ingest(f.labelIngester(t, "ingester-both", "secret", "x"), source); err != nil {
			t.Fatalf("ingester holding every label: %v", err)
		}
	})
}

func TestLabelIngestRefusesUntranslatableLabelSets(t *testing.T) {
	f := newFixture(t)
	ctx := f.labelIngester(t, "ingester")
	tooMany := make([]string, authorized.MaxLabelsPerRule+1)
	for index := range tooMany {
		tooMany[index] = "l" + strconv.Itoa(index)
	}
	longLabel := strings.Repeat("a", 100)
	longLabels := make([]string, 30)
	for index := range longLabels {
		longLabels[index] = longLabel[:90] + strconv.Itoa(index)
	}
	for _, test := range []struct {
		name, visibility, bound string
	}{
		{"unparseable", "secret&bad label", ""},
		{"empty term", "secret&&x", ""},
		{"over MaxLabelsPerRule", strings.Join(tooMany, "&"), "MaxLabelsPerRule"},
		{"over the flattened byte bound", strings.Join(longLabels, "&"), "MaxPolicyExpressionBytes"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := f.clientA.Ingest(ctx, labelledSource(
				"file:///label/"+strings.ReplaceAll(test.name, " ", "-")+".txt",
				"untranslatable", test.visibility))
			if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
				t.Fatalf("ingest error = %v, want invalid argument", err)
			}
			if test.bound != "" && !strings.Contains(err.Error(), test.bound) {
				t.Fatalf("refusal %q does not name the %s bound", err, test.bound)
			}
		})
	}
	if count := baseDocumentCount(t, f); count != 0 {
		t.Fatalf("an untranslatable label set reached the base: %d documents", count)
	}
	// MaxLabelsPerRule labels exactly is accepted (the cap refuses, never
	// truncates, and is not off by one).
	exact := tooMany[:authorized.MaxLabelsPerRule]
	holder := f.labelIngester(t, "ingester-exact", exact...)
	if _, err := f.clientA.Ingest(holder, labelledSource(
		"file:///label/exact.txt", "exactly at the cap", strings.Join(exact, "&"))); err != nil {
		t.Fatalf("ingest at MaxLabelsPerRule: %v", err)
	}
}

// TestLabelIngestRefusesLabelNamespaceSourcePolicy closes the host
// PolicySelectorFunc gap: a selector that returns a policy in the label
// namespace as the source policy is refused even when the caller holds it.
func TestLabelIngestRefusesLabelNamespaceSourcePolicy(t *testing.T) {
	f := newFixture(t)
	labelID := labelPolicy(t, f.sourceA, "secret")
	selector := authorized.PolicySelectorFunc(func(
		_ context.Context, decision auth.Decision, _ explorer.Source,
	) (auth.Policy, error) {
		return auth.NewPolicy(auth.PolicyConfig{
			AuthorizationDomain: decision.AuthorizationDomain(),
			SourceID:            f.sourceA,
			GrantPolicyID:       labelID,
			Epoch:               decision.PolicyGeneration(),
		})
	})
	static := f.staticSelector(t, f.sourceA, f.policyA)
	client, err := authorized.NewClient(authorized.Config{
		Base: f.base, Resolver: f.authority.Resolver(),
		PolicySelector: selector, EdgePolicySelector: static,
		PolicyStore: f.store, GenerationReader: f.reader, Clock: f.clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := f.labelIngester(t, "ingester", "secret")
	for _, visibility := range []string{"", "secret"} {
		_, err := client.Ingest(ctx, labelledSource(
			"file:///label/namespace-"+visibility+".txt", "namespace", visibility))
		if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) ||
			!strings.Contains(err.Error(), auth.LabelPolicyNamespace) {
			t.Fatalf("label-namespace source policy (%q) error = %v", visibility, err)
		}
	}
	if count := baseDocumentCount(t, f); count != 0 {
		t.Fatalf("a label-namespace source policy reached the base: %d documents", count)
	}
}

// TestConnectRefusesLabelNamespaceEdgePolicy closes the same gap for edges:
// a host EdgePolicySelectorFunc returning a lone label policy is refused even
// when the caller holds it, so no rule is ever a label policy alone.
func TestConnectRefusesLabelNamespaceEdgePolicy(t *testing.T) {
	f := newFixture(t)
	labelID := labelPolicy(t, f.sourceA, "secret")
	static := f.staticSelector(t, f.sourceA, f.policyA)
	edges := authorized.EdgePolicySelectorFunc(func(
		_ context.Context, decision auth.Decision, _ graph.Edge,
	) (auth.Policy, error) {
		return auth.NewPolicy(auth.PolicyConfig{
			AuthorizationDomain: decision.AuthorizationDomain(),
			SourceID:            f.sourceA,
			GrantPolicyID:       labelID,
			Epoch:               decision.PolicyGeneration(),
		})
	})
	client, err := authorized.NewClient(authorized.Config{
		Base: f.base, Resolver: f.authority.Resolver(),
		PolicySelector: static, EdgePolicySelector: edges,
		PolicyStore: f.store, GenerationReader: f.reader, Clock: f.clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := f.labelIngester(t, "connector", "secret")
	from, err := client.Ingest(ctx, labelledSource("file:///label/edge-from.txt", "from", ""))
	if err != nil {
		t.Fatal(err)
	}
	to, err := client.Ingest(ctx, labelledSource("file:///label/edge-to.txt", "to", ""))
	if err != nil {
		t.Fatal(err)
	}
	err = client.Connect(ctx, graph.Edge{
		ID:   appEdgeID(t, client, ctx, "label-namespace-edge"),
		From: from.Document.ID, To: to.Document.ID,
		Type: "related_to", Weight: 1,
	})
	if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) ||
		!strings.Contains(err.Error(), auth.LabelPolicyNamespace) {
		t.Fatalf("label-namespace edge policy error = %v", err)
	}
}

func TestLabelRelabelRequiresOldAndNewLabels(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		f := newFixture(t)
		client := f.newClient(t, f.base, store, f.sourceA, f.policyA, nil)
		const uri = "file:///label/relabel.txt"
		first, err := client.Ingest(f.labelIngester(t, "owner", "x"),
			labelledSource(uri, "labelled x", "x"))
		if err != nil {
			t.Fatal(err)
		}
		relabel := labelledSource(uri, "relabelled secret", "secret")
		// Holding only the old label: the new rule is not authorized.
		if _, err := client.Ingest(f.labelIngester(t, "old-only", "x"), relabel); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
			t.Fatalf("relabel with only the old label = %v, want unauthorized", err)
		}
		// Holding only the new label: the existing claim is invisible, and
		// the refusal is the absent-object shape.
		if _, err := client.Ingest(f.labelIngester(t, "new-only", "secret"), relabel); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			t.Fatalf("relabel with only the new label = %v, want not found", err)
		}
		// A source holder with neither label cannot relabel to unlabelled.
		if _, err := client.Ingest(f.labelIngester(t, "neither"),
			labelledSource(uri, "declassified", "")); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			t.Fatalf("declassify without the old label = %v, want not found", err)
		}
		summaries, err := client.Documents(f.labelIngester(t, "lister", "x"))
		if err != nil {
			t.Fatal(err)
		}
		if len(summaries) != 1 || summaries[0].Revision.ID != first.Revision.ID {
			t.Fatalf("a refused relabel changed the document: %+v", summaries)
		}
		second, err := client.Ingest(f.labelIngester(t, "both", "x", "secret"), relabel)
		if err != nil {
			t.Fatalf("relabel holding old and new labels: %v", err)
		}

		// The current revision now needs secret; the older one needs its own
		// x and the current secret.
		for _, check := range []struct {
			name       string
			ctx        context.Context
			current    bool
			historical bool
		}{
			{"source only", f.labelIngester(t, "r-none"), false, false},
			{"old label only", f.labelIngester(t, "r-x", "x"), false, false},
			{"new label only", f.labelIngester(t, "r-secret", "secret"), true, false},
			{"both labels", f.labelIngester(t, "r-both", "x", "secret"), true, true},
		} {
			_, err := client.Document(check.ctx, second.Document.ID, second.Revision.ID)
			if (err == nil) != check.current {
				t.Fatalf("%s: current revision error = %v, want visible=%v", check.name, err, check.current)
			}
			_, err = client.Document(check.ctx, first.Document.ID, first.Revision.ID)
			if (err == nil) != check.historical {
				t.Fatalf("%s: historical revision error = %v, want visible=%v", check.name, err, check.historical)
			}
			if err != nil && !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
				t.Fatalf("%s: historical denial = %v, want not found", check.name, err)
			}
		}
	})
}

// TestLabelRelabelClosesPublicHistoricalRevision pins that content public
// under an old revision does not stay public after the document is labelled.
func TestLabelRelabelClosesPublicHistoricalRevision(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		f := newFixture(t)
		client := f.newClient(t, f.base, store, f.sourceA, f.policyA, nil)
		const uri = "file:///label/declassified-then-secret.txt"
		public, err := client.Ingest(f.labelIngester(t, "owner"),
			labelledSource(uri, "public first revision", ""))
		if err != nil {
			t.Fatal(err)
		}
		sourceOnly := f.labelIngester(t, "reader")
		if _, err := client.Document(sourceOnly, public.Document.ID, public.Revision.ID); err != nil {
			t.Fatalf("public revision before relabel: %v", err)
		}
		secret, err := client.Ingest(f.labelIngester(t, "owner", "secret"),
			labelledSource(uri, "secret second revision", "secret"))
		if err != nil {
			t.Fatal(err)
		}

		sourceOnly = f.labelIngester(t, "reader")
		if _, err := client.Document(sourceOnly, public.Document.ID, public.Revision.ID); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			t.Fatalf("public historical revision after relabel = %v, want not found", err)
		}
		page, err := client.Changes(sourceOnly, authorized.ChangeFeedRequest{})
		if err != nil {
			t.Fatal(err)
		}
		for _, change := range page.Changes {
			if change.Document.ID == public.Document.ID {
				t.Fatalf("relabelled document's old publication stayed in the feed: %+v", change)
			}
		}
		holder := f.labelIngester(t, "holder", "secret")
		if _, err := client.Document(holder, public.Document.ID, public.Revision.ID); err != nil {
			t.Fatalf("label holder lost the historical revision: %v", err)
		}
		if _, err := client.Document(holder, secret.Document.ID, ""); err != nil {
			t.Fatalf("label holder lost the current revision: %v", err)
		}
		page, err = client.Changes(holder, authorized.ChangeFeedRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Changes) != 2 {
			t.Fatalf("label holder sees %d publications, want 2", len(page.Changes))
		}
	})
}

func TestLabelBackfillReadsLabelsFromTheStoredDocument(t *testing.T) {
	f := newFixture(t)
	labelled, err := f.base.Ingest(context.Background(), labelledSource(
		"file:///label/backfill-secret.txt", "labelled before the catalog", "secret"))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := f.base.Ingest(context.Background(), labelledSource(
		"file:///label/backfill-plain.txt", "unlabelled before the catalog", ""))
	if err != nil {
		t.Fatal(err)
	}

	// An operator without the label grant cannot backfill the labelled
	// document, and the refusal registers nothing, not even the plain one.
	if _, err := f.clientA.BackfillExistingDocumentsForDevelopment(
		f.labelIngester(t, "operator-without-label"), devbackfill.NewCapability(),
	); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("backfill without the label grant = %v, want unauthorized", err)
	}
	if summaries, err := f.clientA.Documents(f.labelIngester(t, "admin", "secret")); err != nil || len(summaries) != 0 {
		t.Fatalf("refused backfill registered %d documents (%v)", len(summaries), err)
	}

	registered, err := f.clientA.BackfillExistingDocumentsForDevelopment(
		f.labelIngester(t, "operator", "secret"), devbackfill.NewCapability())
	if err != nil {
		t.Fatal(err)
	}
	if registered != 2 {
		t.Fatalf("registered = %d, want 2", registered)
	}
	sourceOnly := f.labelIngester(t, "reader")
	if _, err := f.clientA.Document(sourceOnly, labelled.Document.ID, ""); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("backfilled labelled document readable without the label: %v", err)
	}
	if _, err := f.clientA.Document(sourceOnly, plain.Document.ID, ""); err != nil {
		t.Fatalf("backfilled unlabelled document: %v", err)
	}
	if _, err := f.clientA.Document(f.labelIngester(t, "holder", "secret"), labelled.Document.ID, ""); err != nil {
		t.Fatalf("label holder cannot read the backfilled labelled document: %v", err)
	}
}

// labelForgingBase reports a stored document node whose visibility disagrees
// with the revision metadata. The backfill must refuse rather than pick one.
type labelForgingBase struct {
	*explorer.Explorer
	forged shoal.ID
}

func (b *labelForgingBase) Neighborhood(
	ctx context.Context, request explorer.NeighborhoodRequest,
) (explorer.Neighborhood, error) {
	neighborhood, err := b.Explorer.Neighborhood(ctx, request)
	for index, node := range neighborhood.Nodes {
		if node.ID == b.forged {
			properties := cloneMetadata(node.Properties)
			delete(properties, interaction.PropertyVisibility)
			neighborhood.Nodes[index].Properties = properties
		}
	}
	return neighborhood, err
}

func TestLabelBackfillRefusesDisagreeingLabels(t *testing.T) {
	f := newFixture(t)
	labelled, err := f.base.Ingest(context.Background(), labelledSource(
		"file:///label/backfill-forged.txt", "labelled", "secret"))
	if err != nil {
		t.Fatal(err)
	}
	forging := &labelForgingBase{Explorer: f.base, forged: labelled.Document.ID}
	client := f.newClient(t, forging, f.store, f.sourceA, f.policyA, nil)
	if _, err := client.BackfillExistingDocumentsForDevelopment(
		f.labelIngester(t, "operator", "secret"), devbackfill.NewCapability(),
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("backfill with a node that drops its labels = %v, want invalid argument", err)
	}
	if _, err := f.clientA.Document(f.labelIngester(t, "reader"), labelled.Document.ID, ""); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("labelled document was registered unlabelled: %v", err)
	}
}

func TestLabelGraphMaterializationRefusesDeclaredVisibility(t *testing.T) {
	f := newFixture(t)
	snapshot, err := f.base.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	writer := f.labelIngester(t, "writer", "secret")
	for _, key := range []string{
		interaction.PropertyVisibility,
		interaction.PropertyVisibilityDigest,
		interaction.PropertyVisibilityCount,
	} {
		for _, onRelation := range []bool{false, true} {
			request := explorer.GraphMaterializationRequest{
				Namespace: []byte("labelled"), MutationID: "labelled-materialization",
				SourceID: f.sourceA, PolicyID: f.policyA, ExpectedSnapshot: snapshot,
				Nodes: []explorer.GraphNodeSpec{
					{Key: []byte("a"), Kind: "team", Properties: shoal.Metadata{"name": "A"}},
					{Key: []byte("b"), Kind: "team", Properties: shoal.Metadata{"name": "B"}},
				},
				Relations: []explorer.GraphRelationSpec{{
					Key: []byte("ab"), From: []byte("a"), To: []byte("b"), Type: "works_with",
				}},
			}
			if onRelation {
				request.Relations[0].Properties = shoal.Metadata{key: "secret"}
			} else {
				request.Nodes[0].Properties[key] = "secret"
			}
			if _, err := f.clientA.MaterializeGraph(writer, request); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
				t.Fatalf("materialization declaring %s (relation=%v) = %v, want invalid argument", key, onRelation, err)
			}
		}
	}
}

// TestLabelGraphMaterializationDoesNotLeakLabelledContent pins that the
// derived output registered under the bare source rule exposes nothing of a
// labelled document: a materialization reads nothing from the corpus, and a
// later edge from its node to a labelled document is filtered by the
// document's own rule.
func TestLabelGraphMaterializationDoesNotLeakLabelledContent(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		f := newFixture(t)
		client := f.newClient(t, f.base, store, f.sourceA, f.policyA, nil)
		admin := f.labelIngester(t, "admin", "secret")
		secret, err := client.Ingest(admin, labelledSource(
			"file:///label/materialized-secret.txt", "secret content", "secret"))
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := f.base.Snapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		result, err := client.MaterializeGraph(admin, explorer.GraphMaterializationRequest{
			Namespace: []byte("teams"), MutationID: "teams",
			SourceID: f.sourceA, PolicyID: f.policyA, ExpectedSnapshot: snapshot,
			Nodes: []explorer.GraphNodeSpec{{
				Key: []byte("team"), Kind: "team", Properties: shoal.Metadata{"name": "Team"},
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		team := result.Nodes[0].ID
		if err := client.Connect(admin, graph.Edge{
			ID:   appEdgeID(t, client, admin, "team-to-secret"),
			From: team, To: secret.Document.ID,
			Type: "owns", Weight: 1,
		}); err != nil {
			t.Fatal(err)
		}
		sourceOnly := f.labelIngester(t, "reader")
		around, err := client.Neighborhood(sourceOnly, explorer.NeighborhoodRequest{
			NodeIDs: []shoal.ID{team}, Depth: 2,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !hasNode(around, team) {
			t.Fatal("materialized node is not visible to the source holder")
		}
		if hasNode(around, secret.Document.ID) || len(around.Edges) != 0 {
			t.Fatalf("labelled content leaked through a materialized node: %+v", around)
		}
		for _, node := range around.Nodes {
			if node.Properties[interaction.PropertyVisibility] != "" {
				t.Fatalf("a labelled node reached a source-only reader: %+v", node)
			}
		}
		holder, err := client.Neighborhood(admin, explorer.NeighborhoodRequest{
			NodeIDs: []shoal.ID{team}, Depth: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !hasNode(holder, secret.Document.ID) {
			t.Fatal("label holder does not see the labelled document beside the materialized node")
		}
	})
}
