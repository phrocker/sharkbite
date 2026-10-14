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

package disclosureconformance

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Terms a probe can ask about. They are chosen so each appears in exactly one
// place, which is what makes a comparison meaningful.
const (
	// OpenTerm appears only in the document every principal may read.
	OpenTerm = "seagrass"
	// RestrictedTerm appears only in the document the uncleared principal may
	// not read. Its matches exist and are withheld.
	RestrictedTerm = "thunderbolt"
	// AbsentTerm appears nowhere in the corpus. Its matches do not exist.
	//
	// The whole question this package exists to answer is whether an uncleared
	// principal can tell RestrictedTerm from AbsentTerm.
	AbsentTerm = "zzzzqqqq"
)

var fixtureDomain = []byte("disclosure-conformance")

// Corpus is a two-compartment corpus with two principals of different
// clearance, built from exported APIs only.
type Corpus struct {
	// Client reads the whole corpus. What a caller sees is decided by the
	// decision bound into the context, not by which client is used.
	Client *authorized.Client

	authority *auth.Authority
	clock     func() time.Time
	// restricted names a real node in the compartment the uncleared principal
	// cannot read. Probing it is the point: the interesting question is
	// whether asking about something real-but-forbidden looks different from
	// asking about something that was never there.
	restricted shoal.ID
	// open names a node every principal may read. A path probe needs it as the
	// source, because a request whose source is forbidden is rejected at seed
	// authorization before target resolution runs, and would only re-test the
	// neighborhood negative path.
	open    shoal.ID
	sourceA []byte
	policyA []byte
	sourceB []byte
	policyB []byte
}

type fixtureGenerations struct {
	mu          sync.RWMutex
	generations map[string]int64
}

func (r *fixtureGenerations) CurrentPolicyGeneration(
	ctx context.Context, domain []byte,
) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.generations[string(domain)], nil
}

// NewCorpus builds the fixture: one document in an open compartment and one in
// a restricted compartment, each containing a term that appears nowhere else.
func NewCorpus(t *testing.T) *Corpus {
	t.Helper()
	// The engine stamps snapshots from the wall clock, and a recording whose
	// snapshot postdates it is rejected before any probe runs. A frozen date
	// captured before ingest is therefore always in the past by the time a
	// question is asked. Determinism comes from comparing two calls in one
	// run, not from pinning the clock.
	clock := func() time.Time { return time.Now().UTC() }
	authority, err := auth.NewAuthorityWithClock(clock)
	if err != nil {
		t.Fatal(err)
	}
	base, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = base.Close() })

	corpus := &Corpus{
		authority: authority, clock: clock,
		sourceA: []byte("source-open"), policyA: []byte("policy-open"),
		sourceB: []byte("source-restricted"), policyB: []byte("policy-restricted"),
	}
	generations := &fixtureGenerations{
		generations: map[string]int64{string(fixtureDomain): 1},
	}
	store := authorized.NewMemoryPolicyStore()

	open := corpus.newClient(t, base, store, generations, corpus.sourceA, corpus.policyA)
	restricted := corpus.newClient(t, base, store, generations, corpus.sourceB, corpus.policyB)
	corpus.Client = open

	admin := corpus.Cleared(t)
	visible, err := open.Ingest(admin, explorer.Source{
		URI: "file:///open.txt", Title: "Open", MediaType: explorer.MediaTypeText,
		Content: "the " + OpenTerm + " bed is shallow",
	})
	if err != nil {
		t.Fatalf("ingest open document: %v", err)
	}
	corpus.open = visible.Document.ID
	hidden, err := restricted.Ingest(admin, explorer.Source{
		URI: "file:///restricted.txt", Title: "Restricted",
		MediaType: explorer.MediaTypeText,
		Content:   "project " + RestrictedTerm + " ships in spring",
	})
	if err != nil {
		t.Fatalf("ingest restricted document: %v", err)
	}
	corpus.restricted = hidden.Document.ID
	// A directed edge from the readable document to the restricted one, so a
	// path probe can hold the source constant and vary only the target. The
	// cleared principal can resolve this path; the uncleared one must not be
	// able to distinguish it from a path to a node that does not exist.
	probeEdge, err := open.ApplicationEdgeID(admin, "disclosure_probe")
	if err != nil {
		t.Fatalf("probe edge ID: %v", err)
	}
	if err := open.Connect(admin, graph.Edge{
		ID: probeEdge, From: corpus.open, To: corpus.restricted,
		Type: "references", Weight: 1,
	}); err != nil {
		t.Fatalf("connect the probe edge: %v", err)
	}
	return corpus
}

func (c *Corpus) newClient(
	t *testing.T,
	base explorer.Client,
	store authorized.PolicyStore,
	generations auth.GenerationReader,
	source, policy []byte,
) *authorized.Client {
	t.Helper()
	selector, err := authorized.NewStaticPolicySelector(source, policy)
	if err != nil {
		t.Fatal(err)
	}
	scorer, _ := base.(authorized.VectorScorer)
	interactionWriter, _ := base.(explorer.InteractionWriter)
	interactionReader, _ := base.(explorer.InteractionReader)
	validator, _ := base.(authorized.SnapshotValidator)
	assertions, _ := base.(authorized.DerivedAssertionReader)
	folds, _ := base.(authorized.FoldStore)
	client, err := authorized.NewClient(authorized.Config{
		Base: base, VectorScorer: scorer,
		InteractionWriter: interactionWriter, InteractionReader: interactionReader,
		SnapshotValidator: validator, DerivedAssertionReader: assertions,
		FoldStore: folds, Resolver: c.authority.Resolver(),
		PolicySelector: selector, PolicyStore: store,
		GenerationReader: generations,
		Clock:            c.clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// OpenNodeID is a node every principal may read. Path probes use it as the
// source so that only the target varies.
func (c *Corpus) OpenNodeID() shoal.ID { return c.open }

// RestrictedNodeID is a node that exists in the corpus and that the uncleared
// principal holds no grant for.
func (c *Corpus) RestrictedNodeID() shoal.ID { return c.restricted }

// AbsentNodeID is well-formed and names nothing. It is the control for every
// probe about the restricted node.
func (c *Corpus) AbsentNodeID() shoal.ID {
	return shoal.ID("doc_00000000000000000000000000000000")
}

// Clock is the fixture's clock, for a service that must agree with it.
func (c *Corpus) Clock() func() time.Time { return c.clock }

// Resolver returns the fixture's trusted decision resolver, for a service that
// binds its own authorization rather than taking a context alone.
func (c *Corpus) Resolver() auth.Resolver { return c.authority.Resolver() }

// Cleared returns a context for a principal granted both compartments.
func (c *Corpus) Cleared(t *testing.T) context.Context {
	t.Helper()
	return c.bind(t, "cleared",
		[][]byte{c.sourceA, c.sourceB}, [][]byte{c.policyA, c.policyB})
}

// Uncleared returns a context for a principal granted only the open
// compartment. Every probe that matters is run as this principal.
func (c *Corpus) Uncleared(t *testing.T) context.Context {
	t.Helper()
	return c.bind(t, "uncleared", [][]byte{c.sourceA}, [][]byte{c.policyA})
}

func (c *Corpus) bind(
	t *testing.T, subject string, sources, policies [][]byte,
) context.Context {
	t.Helper()
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: shoal.ID(subject), Actor: shoal.ID(subject),
		AuthorizationDomain: fixtureDomain,
		AllowedOperations: []auth.Operation{
			auth.OperationIngest, auth.OperationRead, auth.OperationList,
			auth.OperationRetrieve, auth.OperationNeighborhood,
			auth.OperationValidate, auth.OperationConnect,
		},
		PermittedSourceIDs:    sources,
		PermittedPolicyIDs:    policies,
		PolicyGeneration:      1,
		AuthenticationExpires: c.clock().Add(24 * time.Hour),
		RequestID:             shoal.ID(subject + "-request"),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := c.authority.Binder().Bind(context.Background(), decision)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}
