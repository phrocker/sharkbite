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

package explorerconformance

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// AuthorizationPrincipal names a trusted M2 fixture identity.
type AuthorizationPrincipal string

const (
	PrincipalAdmin AuthorizationPrincipal = "admin"
	PrincipalAlpha AuthorizationPrincipal = "alpha"
	PrincipalBeta  AuthorizationPrincipal = "beta"
)

// AuthorizationAuthority selects the matching or an unrelated context
// capability. It never contains a credential or caller-provided authority.
type AuthorizationAuthority string

const (
	AuthorityPrimary AuthorizationAuthority = "primary"
	AuthorityForeign AuthorizationAuthority = "foreign"
)

// AuthorizationDomain selects the fixture domain bound into a decision.
type AuthorizationDomain string

const (
	DomainPrimary AuthorizationDomain = "primary"
	DomainForeign AuthorizationDomain = "foreign"
)

// AuthorizationSourceName identifies a construction-time source fixture.
type AuthorizationSourceName string

const (
	SourcePublic               AuthorizationSourceName = "public"
	SourceAlpha                AuthorizationSourceName = "alpha"
	SourceBeta                 AuthorizationSourceName = "beta"
	SourceRankingVisible       AuthorizationSourceName = "ranking_visible"
	SourceRankingHidden        AuthorizationSourceName = "ranking_hidden"
	SourceRankingHiddenRemoved AuthorizationSourceName = "ranking_hidden_removed"
	SourceGraphA               AuthorizationSourceName = "graph_a"
	SourceGraphB               AuthorizationSourceName = "graph_b"
	SourceGraphC               AuthorizationSourceName = "graph_c"
	SourceGraphD               AuthorizationSourceName = "graph_d"
	SourceGraphE               AuthorizationSourceName = "graph_e"
	SourceGenerationPublic     AuthorizationSourceName = "generation_public"
)

// AuthorizationEdgeName identifies a construction-time edge fixture.
type AuthorizationEdgeName string

const (
	EdgeGraphAB      AuthorizationEdgeName = "graph_a_b"
	EdgeGraphBC      AuthorizationEdgeName = "graph_b_c"
	EdgeGraphHidden  AuthorizationEdgeName = "graph_hidden_rule"
	EdgeGraphVisible AuthorizationEdgeName = "graph_visible_rule"
)

// AuthorizationSource is one stable source-to-policy fixture mapping.
type AuthorizationSource struct {
	Name          AuthorizationSourceName
	Source        explorer.Source
	SourceID      []byte
	GrantPolicyID []byte
}

// AuthorizationEdge is one stable edge-to-policy fixture mapping. From and To
// are replaced by the suite with IDs returned by public ingest operations.
type AuthorizationEdge struct {
	Name          AuthorizationEdgeName
	Edge          graph.Edge
	SourceID      []byte
	GrantPolicyID []byte
}

// PrincipalGrant is one principal's source/policy projection.
type PrincipalGrant struct {
	Principal          AuthorizationPrincipal
	PermittedSourceIDs [][]byte
	PermittedPolicyIDs [][]byte
}

// AuthorizationFixtures contains only public Explorer, graph, and auth
// values. Adapters derive any physical representation behind their factory.
type AuthorizationFixtures struct {
	Domain        []byte
	ForeignDomain []byte
	Sources       []AuthorizationSource
	Edges         []AuthorizationEdge
	InitialGrants []PrincipalGrant
}

// ContextRequest asks the factory to issue one trusted, operation-bounded
// fixture context.
type ContextRequest struct {
	Principal  AuthorizationPrincipal
	Operations []auth.Operation
	Authority  AuthorizationAuthority
	Domain     AuthorizationDomain
}

// BoundAuthorization retains the public decision for fingerprint/cache
// assertions and its capability-bound request context.
type BoundAuthorization struct {
	Context  context.Context
	Decision auth.Decision
}

// GenerationAdvanceTiming controls whether the new generation is installed
// immediately or atomically when the in-flight request performs its final
// generation check before serialization.
type GenerationAdvanceTiming string

const (
	AdvanceImmediately         GenerationAdvanceTiming = "immediately"
	AdvanceBeforeSerialization GenerationAdvanceTiming = "before_serialization"
)

// PolicyGenerationAdvance supplies the complete grants for the next
// monotonically increasing generation.
type PolicyGenerationAdvance struct {
	Timing GenerationAdvanceTiming
	Grants []PrincipalGrant
}

// AuthorizationLifecycle owns one isolated authorized Explorer client. Issue
// reissues contexts after AdvancePolicyGeneration changes the active grants.
type AuthorizationLifecycle struct {
	Client                  explorer.Client
	Issue                   func(context.Context, ContextRequest) (BoundAuthorization, error)
	Restart                 func(context.Context) (explorer.Client, error)
	AdvancePolicyGeneration func(context.Context, PolicyGenerationAdvance) error
	Cleanup                 func() error
}

// AuthorizationClientFactory opens one isolated M2 lifecycle. Trusted source
// selectors may use the construction-time fixture mapping, but must not infer
// grants from caller Source.URI or Source.Metadata values.
type AuthorizationClientFactory func(
	testing.TB, AuthorizationFixtures,
) (AuthorizationLifecycle, error)

// DefaultAuthorizationFixtures returns independently owned stable M2 values.
func DefaultAuthorizationFixtures() AuthorizationFixtures {
	publicSource := []byte("m2-source-public")
	publicPolicy := []byte("m2-policy-public")
	alphaSource := []byte("m2-source-alpha")
	alphaPolicy := []byte("m2-policy-alpha")
	betaSource := []byte("m2-source-beta")
	betaPolicy := []byte("m2-policy-beta")
	source := func(
		name AuthorizationSourceName,
		uri, title, content string,
		sourceID, policyID []byte,
	) AuthorizationSource {
		return AuthorizationSource{
			Name: name,
			Source: explorer.Source{
				URI:       uri,
				Title:     title,
				MediaType: explorer.MediaTypeText,
				Content:   content,
			},
			SourceID:      append([]byte(nil), sourceID...),
			GrantPolicyID: append([]byte(nil), policyID...),
		}
	}
	sources := []AuthorizationSource{
		source(
			SourcePublic,
			"fixture:///m2/public.txt",
			"M2 Public",
			"public document shared fixture",
			publicSource,
			publicPolicy,
		),
		source(
			SourceAlpha,
			"fixture:///m2/claims-public-but-alpha.txt",
			"M2 Alpha",
			"alpha only document scope sentinel",
			alphaSource,
			alphaPolicy,
		),
		source(
			SourceBeta,
			"fixture:///m2/beta.txt",
			"M2 Beta",
			"beta only hidden scope sentinel",
			betaSource,
			betaPolicy,
		),
		source(
			SourceRankingVisible,
			"fixture:///m2/ranking-visible.txt",
			"M2 Ranking Visible",
			"ranking alpha visible",
			alphaSource,
			alphaPolicy,
		),
		source(
			SourceRankingHidden,
			"fixture:///m2/ranking-hidden.txt",
			"M2 Ranking Hidden",
			"ranking alpha omega hidden exact",
			betaSource,
			betaPolicy,
		),
		source(
			SourceRankingHiddenRemoved,
			"fixture:///m2/ranking-hidden.txt",
			"M2 Ranking Hidden",
			"ranking retired beta content",
			betaSource,
			betaPolicy,
		),
		source(
			SourceGraphA,
			"fixture:///m2/graph-a.txt",
			"M2 Graph A",
			"graph visible alpha node a",
			alphaSource,
			alphaPolicy,
		),
		source(
			SourceGraphB,
			"fixture:///m2/graph-b.txt",
			"M2 Graph B",
			"graph hidden beta node b",
			betaSource,
			betaPolicy,
		),
		source(
			SourceGraphC,
			"fixture:///m2/graph-c.txt",
			"M2 Graph C",
			"graph visible alpha node c",
			alphaSource,
			alphaPolicy,
		),
		source(
			SourceGraphD,
			"fixture:///m2/graph-d.txt",
			"M2 Graph D",
			"graph edge policy alpha node d",
			alphaSource,
			alphaPolicy,
		),
		source(
			SourceGraphE,
			"fixture:///m2/graph-e.txt",
			"M2 Graph E",
			"graph edge policy alpha node e",
			alphaSource,
			alphaPolicy,
		),
		source(
			SourceGenerationPublic,
			"fixture:///m2/generation-public.txt",
			"M2 Generation Public",
			"generation two public success",
			publicSource,
			publicPolicy,
		),
	}
	sources[1].Source.Metadata = shoal.Metadata{
		"authorization_domain": "m2-foreign-domain",
		"source_id":            "m2-source-public",
		"policy_id":            "m2-policy-public",
	}
	edge := func(
		name AuthorizationEdgeName,
		id, edgeType string,
		sourceID, policyID []byte,
	) AuthorizationEdge {
		return AuthorizationEdge{
			Name: name,
			Edge: graph.Edge{
				ID: shoal.ID(id), From: "fixture-from", To: "fixture-to",
				Type: edgeType, Weight: 1,
			},
			SourceID:      append([]byte(nil), sourceID...),
			GrantPolicyID: append([]byte(nil), policyID...),
		}
	}
	edges := []AuthorizationEdge{
		edge(EdgeGraphAB, "m2-edge-a-b", "m2_path", publicSource, publicPolicy),
		edge(EdgeGraphBC, "m2-edge-b-c", "m2_path", publicSource, publicPolicy),
		edge(
			EdgeGraphHidden,
			"m2-edge-hidden-rule",
			"m2_edge_rule",
			betaSource,
			betaPolicy,
		),
		edge(
			EdgeGraphVisible,
			"m2-edge-visible-rule",
			"m2_edge_rule",
			alphaSource,
			alphaPolicy,
		),
	}
	edges[2].Edge.Properties = shoal.Metadata{
		"label":     "m2-alpha",
		"policy_id": "m2-policy-alpha",
	}
	allSources := [][]byte{publicSource, alphaSource, betaSource}
	allPolicies := [][]byte{publicPolicy, alphaPolicy, betaPolicy}
	return AuthorizationFixtures{
		Domain:        []byte("m2-authorization-domain"),
		ForeignDomain: []byte("m2-foreign-domain"),
		Sources:       sources,
		Edges:         edges,
		InitialGrants: []PrincipalGrant{
			{
				Principal:          PrincipalAdmin,
				PermittedSourceIDs: cloneByteSlices(allSources),
				PermittedPolicyIDs: cloneByteSlices(allPolicies),
			},
			{
				Principal: PrincipalAlpha,
				PermittedSourceIDs: cloneByteSlices(
					[][]byte{publicSource, alphaSource}),
				PermittedPolicyIDs: cloneByteSlices(
					[][]byte{publicPolicy, alphaPolicy}),
			},
			{
				Principal: PrincipalBeta,
				PermittedSourceIDs: cloneByteSlices(
					[][]byte{publicSource, betaSource}),
				PermittedPolicyIDs: cloneByteSlices(
					[][]byte{publicPolicy, betaPolicy}),
			},
		},
	}
}

// Normalize validates and independently owns all fixture values.
func (f AuthorizationFixtures) Normalize() (AuthorizationFixtures, error) {
	if _, err := auth.NewPolicy(auth.PolicyConfig{
		AuthorizationDomain: f.Domain,
		SourceID:            []byte("fixture-source"),
		GrantPolicyID:       []byte("fixture-policy"),
		Epoch:               1,
	}); err != nil {
		return AuthorizationFixtures{}, err
	}
	if _, err := auth.NewPolicy(auth.PolicyConfig{
		AuthorizationDomain: f.ForeignDomain,
		SourceID:            []byte("fixture-source"),
		GrantPolicyID:       []byte("fixture-policy"),
		Epoch:               1,
	}); err != nil {
		return AuthorizationFixtures{}, err
	}
	normalized := AuthorizationFixtures{
		Domain:        append([]byte(nil), f.Domain...),
		ForeignDomain: append([]byte(nil), f.ForeignDomain...),
		Sources:       make([]AuthorizationSource, len(f.Sources)),
		Edges:         make([]AuthorizationEdge, len(f.Edges)),
		InitialGrants: clonePrincipalGrants(f.InitialGrants),
	}
	sourceNames := make(map[AuthorizationSourceName]struct{}, len(f.Sources))
	for index, fixture := range f.Sources {
		if strings.TrimSpace(string(fixture.Name)) == "" {
			return AuthorizationFixtures{}, shoal.NewError(
				shoal.ErrorInvalidArgument, "authorization source name is required")
		}
		if _, duplicate := sourceNames[fixture.Name]; duplicate {
			return AuthorizationFixtures{}, shoal.NewError(
				shoal.ErrorInvalidArgument, "authorization source names must be unique")
		}
		sourceNames[fixture.Name] = struct{}{}
		if _, err := auth.NewPolicy(auth.PolicyConfig{
			AuthorizationDomain: f.Domain,
			SourceID:            fixture.SourceID,
			GrantPolicyID:       fixture.GrantPolicyID,
			Epoch:               1,
		}); err != nil {
			return AuthorizationFixtures{}, err
		}
		normalized.Sources[index] = cloneAuthorizationSource(fixture)
	}
	edgeNames := make(map[AuthorizationEdgeName]struct{}, len(f.Edges))
	edgeIDs := make(map[shoal.ID]struct{}, len(f.Edges))
	for index, fixture := range f.Edges {
		if strings.TrimSpace(string(fixture.Name)) == "" {
			return AuthorizationFixtures{}, shoal.NewError(
				shoal.ErrorInvalidArgument, "authorization edge name is required")
		}
		if _, duplicate := edgeNames[fixture.Name]; duplicate {
			return AuthorizationFixtures{}, shoal.NewError(
				shoal.ErrorInvalidArgument, "authorization edge names must be unique")
		}
		if _, duplicate := edgeIDs[fixture.Edge.ID]; duplicate {
			return AuthorizationFixtures{}, shoal.NewError(
				shoal.ErrorInvalidArgument, "authorization edge IDs must be unique")
		}
		edgeNames[fixture.Name] = struct{}{}
		edgeIDs[fixture.Edge.ID] = struct{}{}
		if err := fixture.Edge.Validate(); err != nil {
			return AuthorizationFixtures{}, err
		}
		if _, err := auth.NewPolicy(auth.PolicyConfig{
			AuthorizationDomain: f.Domain,
			SourceID:            fixture.SourceID,
			GrantPolicyID:       fixture.GrantPolicyID,
			Epoch:               1,
		}); err != nil {
			return AuthorizationFixtures{}, err
		}
		normalized.Edges[index] = cloneAuthorizationEdge(fixture)
	}
	if _, err := normalizePrincipalGrants(f.InitialGrants); err != nil {
		return AuthorizationFixtures{}, err
	}
	return normalized, nil
}

// Source returns an independently owned named source fixture.
func (f AuthorizationFixtures) Source(
	name AuthorizationSourceName,
) (AuthorizationSource, bool) {
	for _, fixture := range f.Sources {
		if fixture.Name == name {
			return cloneAuthorizationSource(fixture), true
		}
	}
	return AuthorizationSource{}, false
}

// Edge returns an independently owned named edge fixture.
func (f AuthorizationFixtures) Edge(
	name AuthorizationEdgeName,
) (AuthorizationEdge, bool) {
	for _, fixture := range f.Edges {
		if fixture.Name == name {
			return cloneAuthorizationEdge(fixture), true
		}
	}
	return AuthorizationEdge{}, false
}

// Grants returns independently owned initial grants for a principal.
func (f AuthorizationFixtures) Grants(
	principal AuthorizationPrincipal,
) (PrincipalGrant, bool) {
	for _, grant := range f.InitialGrants {
		if grant.Principal == principal {
			return clonePrincipalGrant(grant), true
		}
	}
	return PrincipalGrant{}, false
}

// RunAuthorization executes the M2 authorization and noninterference suite.
func RunAuthorization(t *testing.T, factory AuthorizationClientFactory) {
	t.Helper()
	if factory == nil {
		t.Fatal("authorization conformance factory is required")
	}
	tests := []struct {
		name string
		run  func(*testing.T, AuthorizationClientFactory)
	}{
		{"trusted_context_boundary", trustedContextBoundary},
		{"visibility_and_direct_non_disclosure", visibilityAndDirectNonDisclosure},
		{"authorized_projection_ranking", authorizedProjectionRanking},
		{"scope_intersection", scopeIntersection},
		{"graph_noninterference", graphNoninterference},
		{"fingerprint_cache_and_audit", fingerprintCacheAndAudit},
		{"generation_revocation", generationRevocation},
		{"restart_retains_enforcement", restartRetainsEnforcement},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			test.run(t, factory)
		})
	}
}

// AssertUnauthorizedBackendResult verifies that a client rejects a
// structurally valid backend response that escaped its authorized projection.
func AssertUnauthorizedBackendResult(
	t testing.TB,
	client explorer.Client,
	ctx context.Context,
	request retrieval.Request,
) {
	t.Helper()
	if client == nil {
		t.Fatal("malicious-result probe requires a client")
	}
	if _, err := client.Retrieve(ctx, request); !shoal.IsErrorCode(
		err, shoal.ErrorInternal,
	) {
		t.Fatalf("unauthorized backend result error = %v, want internal", err)
	}
}

func trustedContextBoundary(
	t *testing.T,
	factory AuthorizationClientFactory,
) {
	lifecycle, _ := openAuthorizationLifecycle(t, factory)
	if _, err := lifecycle.Client.Documents(context.Background()); !shoal.IsErrorCode(
		err, shoal.ErrorUnauthorized,
	) {
		t.Fatalf("missing context error = %v", err)
	}
	foreign := issueAuthorization(
		t, lifecycle, PrincipalAlpha, []auth.Operation{auth.OperationList},
		AuthorityForeign, DomainPrimary,
	)
	if _, err := lifecycle.Client.Documents(foreign.Context); !shoal.IsErrorCode(
		err, shoal.ErrorUnauthorized,
	) {
		t.Fatalf("foreign authority error = %v", err)
	}
	wrongOperation := issueAuthorization(
		t, lifecycle, PrincipalAlpha, []auth.Operation{auth.OperationRead},
		AuthorityPrimary, DomainPrimary,
	)
	if _, err := lifecycle.Client.Documents(
		wrongOperation.Context,
	); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("wrong operation error = %v", err)
	}
	wrongDomain := issueAuthorization(
		t, lifecycle, PrincipalAlpha, []auth.Operation{auth.OperationList},
		AuthorityPrimary, DomainForeign,
	)
	if _, err := lifecycle.Client.Documents(
		wrongDomain.Context,
	); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("wrong domain error = %v", err)
	}

	alpha := issueAuthorization(
		t, lifecycle, PrincipalAlpha, []auth.Operation{auth.OperationList},
		AuthorityPrimary, DomainPrimary,
	)
	canceled, cancel := context.WithCancel(alpha.Context)
	cancel()
	if _, err := lifecycle.Client.Documents(canceled); !shoal.IsErrorCode(
		err, shoal.ErrorCanceled,
	) {
		t.Fatalf("canceled request error = %v", err)
	}
	expired, cancelDeadline := context.WithDeadline(
		alpha.Context, time.Now().Add(-time.Second))
	defer cancelDeadline()
	if _, err := lifecycle.Client.Documents(expired); !shoal.IsErrorCode(
		err, shoal.ErrorDeadline,
	) {
		t.Fatalf("expired request error = %v", err)
	}
}

func visibilityAndDirectNonDisclosure(
	t *testing.T,
	factory AuthorizationClientFactory,
) {
	lifecycle, fixtures := openAuthorizationLifecycle(t, factory)
	admin := issueAuthorization(
		t, lifecycle, PrincipalAdmin, []auth.Operation{auth.OperationIngest},
		AuthorityPrimary, DomainPrimary,
	)
	public := ingestFixture(t, lifecycle.Client, admin.Context, fixtures, SourcePublic)
	alphaOnly := ingestFixture(t, lifecycle.Client, admin.Context, fixtures, SourceAlpha)
	betaOnly := ingestFixture(t, lifecycle.Client, admin.Context, fixtures, SourceBeta)

	alphaList := issueAuthorization(
		t, lifecycle, PrincipalAlpha, []auth.Operation{auth.OperationList},
		AuthorityPrimary, DomainPrimary,
	)
	alphaDocuments, err := lifecycle.Client.Documents(alphaList.Context)
	if err != nil {
		t.Fatal(err)
	}
	assertDocumentSet(t, alphaDocuments, public.Document.ID, alphaOnly.Document.ID)
	betaList := issueAuthorization(
		t, lifecycle, PrincipalBeta, []auth.Operation{auth.OperationList},
		AuthorityPrimary, DomainPrimary,
	)
	betaDocuments, err := lifecycle.Client.Documents(betaList.Context)
	if err != nil {
		t.Fatal(err)
	}
	assertDocumentSet(t, betaDocuments, public.Document.ID, betaOnly.Document.ID)

	alphaRead := issueAuthorization(
		t, lifecycle, PrincipalAlpha, []auth.Operation{auth.OperationRead},
		AuthorityPrimary, DomainPrimary,
	)
	hiddenCurrent := documentProbe(
		lifecycle.Client, alphaRead.Context, betaOnly.Document.ID, "")
	absentCurrent := documentProbe(
		lifecycle.Client, alphaRead.Context, "m2-absent-document", "")
	assertSameNotFound(t, hiddenCurrent, absentCurrent)
	hiddenRevision := documentProbe(
		lifecycle.Client,
		alphaRead.Context,
		betaOnly.Document.ID,
		betaOnly.Revision.ID,
	)
	absentRevision := documentProbe(
		lifecycle.Client,
		alphaRead.Context,
		"m2-absent-document",
		"m2-absent-revision",
	)
	assertSameNotFound(t, hiddenRevision, absentRevision)
}

func authorizedProjectionRanking(
	t *testing.T,
	factory AuthorizationClientFactory,
) {
	lifecycle, fixtures := openAuthorizationLifecycle(t, factory)
	admin := issueAuthorization(
		t, lifecycle, PrincipalAdmin, []auth.Operation{auth.OperationIngest},
		AuthorityPrimary, DomainPrimary,
	)
	visible := ingestFixture(
		t, lifecycle.Client, admin.Context, fixtures, SourceRankingVisible)
	alpha := issueAuthorization(
		t, lifecycle, PrincipalAlpha, []auth.Operation{auth.OperationRetrieve},
		AuthorityPrimary, DomainPrimary,
	)
	modes := []retrieval.Mode{retrieval.ModeLexical}
	request := retrieval.Request{
		Text: "alpha omega", TopK: 1, Modes: modes, Explain: true,
	}
	requestCopy := cloneRetrievalRequest(request)
	before, err := lifecycle.Client.Retrieve(alpha.Context, request)
	if err != nil {
		t.Fatal(err)
	}
	assertSingleCitation(t, before, visible.Document.ID)
	beforeBits := math.Float64bits(float64(before.Results[0].Score))

	hidden := ingestFixture(
		t, lifecycle.Client, admin.Context, fixtures, SourceRankingHidden)
	beta := issueAuthorization(
		t, lifecycle, PrincipalBeta, []auth.Operation{auth.OperationRetrieve},
		AuthorityPrimary, DomainPrimary,
	)
	hiddenResponse, err := lifecycle.Client.Retrieve(beta.Context, request)
	if err != nil {
		t.Fatal(err)
	}
	assertSingleCitation(t, hiddenResponse, hidden.Document.ID)
	if hiddenResponse.Results[0].Score <= before.Results[0].Score {
		t.Fatalf(
			"hidden candidate score = %v, visible score = %v; fixture did not prove displacement",
			hiddenResponse.Results[0].Score,
			before.Results[0].Score,
		)
	}

	afterAdd, err := lifecycle.Client.Retrieve(alpha.Context, request)
	if err != nil {
		t.Fatal(err)
	}
	assertIdenticalRetrieval(t, before, afterAdd, beforeBits)
	removed := ingestFixture(
		t,
		lifecycle.Client,
		admin.Context,
		fixtures,
		SourceRankingHiddenRemoved,
	)
	if removed.Document.ID != hidden.Document.ID ||
		removed.Revision.ID == hidden.Revision.ID {
		t.Fatalf("hidden removal revision = %+v after %+v", removed, hidden)
	}
	afterRemove, err := lifecycle.Client.Retrieve(alpha.Context, request)
	if err != nil {
		t.Fatal(err)
	}
	assertIdenticalRetrieval(t, before, afterRemove, beforeBits)
	if !reflect.DeepEqual(request, requestCopy) {
		t.Fatalf("retrieval request was mutated: %+v != %+v", request, requestCopy)
	}
}

func scopeIntersection(
	t *testing.T,
	factory AuthorizationClientFactory,
) {
	lifecycle, fixtures := openAuthorizationLifecycle(t, factory)
	admin := issueAuthorization(
		t, lifecycle, PrincipalAdmin, []auth.Operation{auth.OperationIngest},
		AuthorityPrimary, DomainPrimary,
	)
	visible := ingestFixture(t, lifecycle.Client, admin.Context, fixtures, SourceAlpha)
	hidden := ingestFixture(t, lifecycle.Client, admin.Context, fixtures, SourceBeta)
	alphaRetrieve := issueAuthorization(
		t, lifecycle, PrincipalAlpha, []auth.Operation{auth.OperationRetrieve},
		AuthorityPrimary, DomainPrimary,
	)
	unscoped, err := lifecycle.Client.Retrieve(
		alphaRetrieve.Context,
		retrieval.Request{Text: "scope sentinel", TopK: 10},
	)
	if err != nil {
		t.Fatal(err)
	}
	assertSingleCitation(t, unscoped, visible.Document.ID)
	betaRead := issueAuthorization(
		t, lifecycle, PrincipalBeta, []auth.Operation{auth.OperationRead},
		AuthorityPrimary, DomainPrimary,
	)
	hiddenView, err := lifecycle.Client.Document(
		betaRead.Context, hidden.Document.ID, hidden.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	hiddenNode := firstViewNode(t, hiddenView)
	requests := []retrieval.Request{
		{
			Text: "scope sentinel",
			Scope: retrieval.Scope{
				DocumentIDs: []shoal.ID{hidden.Document.ID},
			},
		},
		{
			Text: "scope sentinel",
			Scope: retrieval.Scope{
				NodeIDs: []shoal.ID{hiddenNode},
			},
		},
		{
			Text: "scope sentinel",
			Scope: retrieval.Scope{
				DocumentIDs: []shoal.ID{visible.Document.ID},
				NodeIDs:     []shoal.ID{hiddenNode},
			},
		},
	}
	for index, request := range requests {
		response, err := lifecycle.Client.Retrieve(alphaRetrieve.Context, request)
		if err != nil {
			t.Fatalf("scoped request %d: %v", index, err)
		}
		if len(response.Results) != 0 {
			t.Fatalf("scoped request %d broadened grants: %+v", index, response)
		}
	}
}

func graphNoninterference(
	t *testing.T,
	factory AuthorizationClientFactory,
) {
	lifecycle, fixtures := openAuthorizationLifecycle(t, factory)
	adminIngest := issueAuthorization(
		t, lifecycle, PrincipalAdmin, []auth.Operation{auth.OperationIngest},
		AuthorityPrimary, DomainPrimary,
	)
	a := ingestFixture(t, lifecycle.Client, adminIngest.Context, fixtures, SourceGraphA)
	b := ingestFixture(t, lifecycle.Client, adminIngest.Context, fixtures, SourceGraphB)
	c := ingestFixture(t, lifecycle.Client, adminIngest.Context, fixtures, SourceGraphC)
	d := ingestFixture(t, lifecycle.Client, adminIngest.Context, fixtures, SourceGraphD)
	e := ingestFixture(t, lifecycle.Client, adminIngest.Context, fixtures, SourceGraphE)
	adminConnect := issueAuthorization(
		t, lifecycle, PrincipalAdmin, []auth.Operation{auth.OperationConnect},
		AuthorityPrimary, DomainPrimary,
	)
	ab := connectFixture(
		t, lifecycle.Client, adminConnect.Context, fixtures, EdgeGraphAB,
		a.Document.ID, b.Document.ID,
	)
	bc := connectFixture(
		t, lifecycle.Client, adminConnect.Context, fixtures, EdgeGraphBC,
		b.Document.ID, c.Document.ID,
	)
	hiddenRule := connectFixture(
		t, lifecycle.Client, adminConnect.Context, fixtures, EdgeGraphHidden,
		d.Document.ID, e.Document.ID,
	)
	visibleRule := connectFixture(
		t, lifecycle.Client, adminConnect.Context, fixtures, EdgeGraphVisible,
		d.Document.ID, e.Document.ID,
	)
	alphaGraph := issueAuthorization(
		t, lifecycle, PrincipalAlpha,
		[]auth.Operation{auth.OperationNeighborhood},
		AuthorityPrimary, DomainPrimary,
	)
	neighborhood, err := lifecycle.Client.Neighborhood(
		alphaGraph.Context,
		explorer.NeighborhoodRequest{
			NodeIDs:   []shoal.ID{a.Document.ID},
			Depth:     2,
			EdgeTypes: []string{"m2_path"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(neighborhood.Nodes) != 1 ||
		neighborhood.Nodes[0].ID != a.Document.ID ||
		len(neighborhood.Edges) != 0 {
		t.Fatalf("hidden bridge disclosed graph state: %+v", neighborhood)
	}
	rendered := fmt.Sprint(neighborhood)
	for _, hidden := range []string{
		string(b.Document.ID),
		string(c.Document.ID),
		string(ab.ID),
		string(bc.ID),
	} {
		if strings.Contains(rendered, hidden) {
			t.Fatalf("neighborhood disclosed hidden graph value %q: %s", hidden, rendered)
		}
	}
	hiddenSeed := neighborhoodProbe(
		lifecycle.Client,
		alphaGraph.Context,
		explorer.NeighborhoodRequest{NodeIDs: []shoal.ID{b.Document.ID}},
	)
	absentSeed := neighborhoodProbe(
		lifecycle.Client,
		alphaGraph.Context,
		explorer.NeighborhoodRequest{NodeIDs: []shoal.ID{"m2-absent-node"}},
	)
	assertSameNotFound(t, hiddenSeed, absentSeed)

	edgeProjection, err := lifecycle.Client.Neighborhood(
		alphaGraph.Context,
		explorer.NeighborhoodRequest{
			NodeIDs:   []shoal.ID{d.Document.ID},
			Depth:     1,
			EdgeTypes: []string{"m2_edge_rule"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(edgeProjection.Nodes) != 2 ||
		len(edgeProjection.Edges) != 1 ||
		edgeProjection.Edges[0].ID != visibleRule.ID {
		t.Fatalf("edge-rule projection = %+v", edgeProjection)
	}
	if edgeProjection.Edges[0].ID == hiddenRule.ID {
		t.Fatalf("hidden edge rule was returned: %+v", edgeProjection)
	}
}

func fingerprintCacheAndAudit(
	t *testing.T,
	factory AuthorizationClientFactory,
) {
	lifecycle, fixtures := openAuthorizationLifecycle(t, factory)
	admin := issueAuthorization(
		t, lifecycle, PrincipalAdmin, []auth.Operation{auth.OperationIngest},
		AuthorityPrimary, DomainPrimary,
	)
	alphaDocument := ingestFixture(
		t, lifecycle.Client, admin.Context, fixtures, SourceAlpha)
	alpha := issueAuthorization(
		t, lifecycle, PrincipalAlpha, []auth.Operation{auth.OperationRetrieve},
		AuthorityPrimary, DomainPrimary,
	)
	beta := issueAuthorization(
		t, lifecycle, PrincipalBeta, []auth.Operation{auth.OperationRetrieve},
		AuthorityPrimary, DomainPrimary,
	)
	request := retrieval.Request{
		Text: "scope sentinel",
		Scope: retrieval.Scope{DocumentIDs: []shoal.ID{
			alphaDocument.Document.ID,
		}},
	}
	positive, err := lifecycle.Client.Retrieve(alpha.Context, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(positive.Results) == 0 {
		t.Fatal("positive cache probe returned no result")
	}
	negative, err := lifecycle.Client.Retrieve(beta.Context, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(negative.Results) != 0 {
		t.Fatalf("negative cache probe returned %+v", negative)
	}
	alphaFingerprint, err := auth.AuthorizationFingerprint(alpha.Decision)
	if err != nil {
		t.Fatal(err)
	}
	betaFingerprint, err := auth.AuthorizationFingerprint(beta.Decision)
	if err != nil {
		t.Fatal(err)
	}
	if alphaFingerprint == betaFingerprint {
		t.Fatal("positive and negative decisions share an authorization fingerprint")
	}
	alphaKey := newFixtureCacheKey(t, fixtures, alpha.Decision, request)
	betaKey := newFixtureCacheKey(t, fixtures, beta.Decision, request)
	if alphaKey.AuthorizationFingerprint() != alphaFingerprint ||
		betaKey.AuthorizationFingerprint() != betaFingerprint {
		t.Fatal("cache keys did not use the core authorization fingerprint")
	}
	if alphaKey.PartitionDigest() != alphaKey.Digest() ||
		betaKey.PartitionDigest() != betaKey.Digest() {
		t.Fatal("positive/negative cache polarity changed the partition primitive")
	}
	if alphaKey.Digest() == betaKey.Digest() {
		t.Fatal("different authorized projections shared a cache partition")
	}

	alphaFixture := fixtureSource(t, fixtures, SourceAlpha)
	rawValues := []string{
		request.Text,
		alphaFixture.Source.Content,
		string(alphaDocument.Document.ID),
		string(alphaFixture.SourceID),
		string(alphaFixture.GrantPolicyID),
		string(fixtures.Domain),
		"m2-secret-label",
	}
	names := []auth.AuditAttributeName{
		auth.AuditAttributeRequest,
		auth.AuditAttributeSource,
		auth.AuditAttributeObject,
		auth.AuditAttributeCache,
		auth.AuditAttributePolicy,
		auth.AuditAttributeAuthorizationDomain,
		auth.AuditAttributeServiceCeiling,
	}
	attributes := make([]auth.AuditAttribute, len(rawValues))
	for index, raw := range rawValues {
		attribute, err := auth.NewAuditAttribute(names[index], []byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		attributes[index] = attribute
	}
	event, err := auth.NewAuditEvent(auth.AuditEventConfig{
		OccurredAt:               time.Date(2026, time.August, 26, 20, 0, 0, 0, time.UTC),
		Operation:                auth.OperationRetrieve,
		Outcome:                  auth.AuditAllowed,
		AuthorizationFingerprint: alphaFingerprint,
		RequestDigest:            alphaKey.RequestDigest(),
		Attributes:               attributes,
	})
	if err != nil {
		t.Fatal(err)
	}
	representations := []string{
		alpha.Decision.String(),
		alphaFingerprint.String(),
		alphaKey.String(),
		event.String(),
	}
	for _, attribute := range attributes {
		representations = append(
			representations,
			attribute.String(),
			attribute.Value().String(),
		)
	}
	for _, representation := range representations {
		for _, raw := range rawValues {
			if strings.Contains(representation, raw) {
				t.Fatalf("string representation exposed %q: %q", raw, representation)
			}
		}
	}
}

func generationRevocation(
	t *testing.T,
	factory AuthorizationClientFactory,
) {
	lifecycle, fixtures := openAuthorizationLifecycle(t, factory)
	admin := issueAuthorization(
		t, lifecycle, PrincipalAdmin, []auth.Operation{auth.OperationIngest},
		AuthorityPrimary, DomainPrimary,
	)
	revoked := ingestFixture(t, lifecycle.Client, admin.Context, fixtures, SourceAlpha)
	oldAlpha := issueAuthorization(
		t,
		lifecycle,
		PrincipalAlpha,
		[]auth.Operation{
			auth.OperationList,
			auth.OperationRead,
			auth.OperationRetrieve,
		},
		AuthorityPrimary,
		DomainPrimary,
	)
	advance := PolicyGenerationAdvance{
		Timing: AdvanceBeforeSerialization,
		Grants: revokedAlphaGrants(t, fixtures),
	}
	if err := lifecycle.AdvancePolicyGeneration(
		context.Background(), advance); err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.Client.Documents(oldAlpha.Context); !shoal.IsErrorCode(
		err, shoal.ErrorUnavailable,
	) {
		t.Fatalf("in-flight generation change error = %v", err)
	}
	if _, err := lifecycle.Client.Documents(oldAlpha.Context); !shoal.IsErrorCode(
		err, shoal.ErrorUnavailable,
	) {
		t.Fatalf("old bound context error = %v", err)
	}

	newAdmin := issueAuthorization(
		t, lifecycle, PrincipalAdmin, []auth.Operation{auth.OperationIngest},
		AuthorityPrimary, DomainPrimary,
	)
	if newAdmin.Decision.PolicyGeneration() <= oldAlpha.Decision.PolicyGeneration() {
		t.Fatalf(
			"new generation = %d, old generation = %d",
			newAdmin.Decision.PolicyGeneration(),
			oldAlpha.Decision.PolicyGeneration(),
		)
	}
	currentPublic := ingestFixture(
		t,
		lifecycle.Client,
		newAdmin.Context,
		fixtures,
		SourceGenerationPublic,
	)
	newAlpha := issueAuthorization(
		t,
		lifecycle,
		PrincipalAlpha,
		[]auth.Operation{
			auth.OperationList,
			auth.OperationRead,
			auth.OperationRetrieve,
		},
		AuthorityPrimary,
		DomainPrimary,
	)
	documents, err := lifecycle.Client.Documents(newAlpha.Context)
	if err != nil {
		t.Fatal(err)
	}
	assertDocumentSet(t, documents, currentPublic.Document.ID)
	if err := documentProbe(
		lifecycle.Client,
		newAlpha.Context,
		revoked.Document.ID,
		revoked.Revision.ID,
	); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("revoked document error = %v", err)
	}
	response, err := lifecycle.Client.Retrieve(
		newAlpha.Context,
		retrieval.Request{
			Text: "scope sentinel",
			Scope: retrieval.Scope{DocumentIDs: []shoal.ID{
				revoked.Document.ID,
			}},
			AsOf: time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
		},
	)
	if err != nil {
		t.Fatalf("revoked AsOf request = %v", err)
	}
	if len(response.Results) != 0 {
		t.Fatalf("AsOf restored revoked data: %+v", response)
	}
}

func restartRetainsEnforcement(
	t *testing.T,
	factory AuthorizationClientFactory,
) {
	lifecycle, fixtures := openAuthorizationLifecycle(t, factory)
	admin := issueAuthorization(
		t, lifecycle, PrincipalAdmin, []auth.Operation{auth.OperationIngest},
		AuthorityPrimary, DomainPrimary,
	)
	public := ingestFixture(t, lifecycle.Client, admin.Context, fixtures, SourcePublic)
	alpha := ingestFixture(t, lifecycle.Client, admin.Context, fixtures, SourceAlpha)
	beta := ingestFixture(t, lifecycle.Client, admin.Context, fixtures, SourceBeta)
	restarted, err := lifecycle.Restart(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if restarted == nil {
		t.Fatal("restart returned a nil authorized client")
	}
	lifecycle.Client = restarted
	alphaList := issueAuthorization(
		t, lifecycle, PrincipalAlpha, []auth.Operation{auth.OperationList},
		AuthorityPrimary, DomainPrimary,
	)
	documents, err := lifecycle.Client.Documents(alphaList.Context)
	if err != nil {
		t.Fatal(err)
	}
	assertDocumentSet(t, documents, public.Document.ID, alpha.Document.ID)
	alphaRead := issueAuthorization(
		t, lifecycle, PrincipalAlpha, []auth.Operation{auth.OperationRead},
		AuthorityPrimary, DomainPrimary,
	)
	hidden := documentProbe(
		lifecycle.Client, alphaRead.Context, beta.Document.ID, beta.Revision.ID)
	absent := documentProbe(
		lifecycle.Client,
		alphaRead.Context,
		"m2-restart-absent",
		"m2-restart-absent-revision",
	)
	assertSameNotFound(t, hidden, absent)
}

func openAuthorizationLifecycle(
	t *testing.T,
	factory AuthorizationClientFactory,
) (*AuthorizationLifecycle, AuthorizationFixtures) {
	t.Helper()
	fixtures, err := DefaultAuthorizationFixtures().Normalize()
	if err != nil {
		t.Fatalf("normalize authorization fixtures: %v", err)
	}
	lifecycle, err := factory(t, fixtures)
	if err != nil {
		t.Fatalf("open authorized Explorer client: %v", err)
	}
	if lifecycle.Client == nil || lifecycle.Issue == nil ||
		lifecycle.Restart == nil || lifecycle.AdvancePolicyGeneration == nil ||
		lifecycle.Cleanup == nil {
		t.Fatal(
			"authorization lifecycle requires client, issue, restart, generation, and cleanup hooks",
		)
	}
	t.Cleanup(func() {
		if err := lifecycle.Cleanup(); err != nil {
			t.Errorf("cleanup authorized Explorer client: %v", err)
		}
	})
	return &lifecycle, fixtures
}

func issueAuthorization(
	t *testing.T,
	lifecycle *AuthorizationLifecycle,
	principal AuthorizationPrincipal,
	operations []auth.Operation,
	authority AuthorizationAuthority,
	domain AuthorizationDomain,
) BoundAuthorization {
	t.Helper()
	bound, err := lifecycle.Issue(context.Background(), ContextRequest{
		Principal:  principal,
		Operations: append([]auth.Operation(nil), operations...),
		Authority:  authority,
		Domain:     domain,
	})
	if err != nil {
		t.Fatalf("issue %s authorization: %v", principal, err)
	}
	if bound.Context == nil {
		t.Fatalf("issue %s authorization returned nil context", principal)
	}
	return bound
}

func ingestFixture(
	t *testing.T,
	client explorer.Client,
	ctx context.Context,
	fixtures AuthorizationFixtures,
	name AuthorizationSourceName,
) explorer.IngestResult {
	t.Helper()
	fixture := fixtureSource(t, fixtures, name)
	result, err := client.Ingest(ctx, cloneExplorerSource(fixture.Source))
	if err != nil {
		t.Fatalf("ingest %s: %v", name, err)
	}
	return result
}

func connectFixture(
	t *testing.T,
	client explorer.Client,
	ctx context.Context,
	fixtures AuthorizationFixtures,
	name AuthorizationEdgeName,
	from, to shoal.ID,
) graph.Edge {
	t.Helper()
	fixture, ok := fixtures.Edge(name)
	if !ok {
		t.Fatalf("missing edge fixture %s", name)
	}
	edge := cloneGraphEdge(fixture.Edge)
	edge.From = from
	edge.To = to
	edge.ID = applicationEdgeID(t, client, ctx, edge.ID)
	if err := client.Connect(ctx, edge); err != nil {
		t.Fatalf("connect %s: %v", name, err)
	}
	return edge
}

// applicationEdgeNamer is a client whose Connect accepts only edge IDs in the
// caller's own namespace, such as the authorized client (#699).
type applicationEdgeNamer interface {
	ApplicationEdgeID(context.Context, shoal.ID) (shoal.ID, error)
}

// applicationEdgeID returns the ID under which the caller connects the edge
// it calls localID: localID itself unless the client namespaces edge IDs.
func applicationEdgeID(
	t *testing.T,
	client explorer.Client,
	ctx context.Context,
	localID shoal.ID,
) shoal.ID {
	t.Helper()
	namer, ok := client.(applicationEdgeNamer)
	if !ok {
		return localID
	}
	id, err := namer.ApplicationEdgeID(ctx, localID)
	if err != nil {
		t.Fatalf("application edge ID %s: %v", localID, err)
	}
	return id
}

func fixtureSource(
	t *testing.T,
	fixtures AuthorizationFixtures,
	name AuthorizationSourceName,
) AuthorizationSource {
	t.Helper()
	fixture, ok := fixtures.Source(name)
	if !ok {
		t.Fatalf("missing source fixture %s", name)
	}
	return fixture
}

func assertDocumentSet(
	t *testing.T,
	documents []explorer.DocumentSummary,
	want ...shoal.ID,
) {
	t.Helper()
	got := make([]shoal.ID, len(documents))
	for index, document := range documents {
		got[index] = document.Document.ID
	}
	sort.Slice(got, func(left, right int) bool {
		return shoal.CompareID(got[left], got[right]) < 0
	})
	want = append([]shoal.ID(nil), want...)
	sort.Slice(want, func(left, right int) bool {
		return shoal.CompareID(want[left], want[right]) < 0
	})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("document IDs = %v, want %v", got, want)
	}
}

func documentProbe(
	client explorer.Client,
	ctx context.Context,
	documentID, revisionID shoal.ID,
) error {
	_, err := client.Document(ctx, documentID, revisionID)
	return err
}

func neighborhoodProbe(
	client explorer.Client,
	ctx context.Context,
	request explorer.NeighborhoodRequest,
) error {
	_, err := client.Neighborhood(ctx, request)
	return err
}

func assertSameNotFound(t *testing.T, hidden, absent error) {
	t.Helper()
	if !shoal.IsErrorCode(hidden, shoal.ErrorNotFound) ||
		!shoal.IsErrorCode(absent, shoal.ErrorNotFound) ||
		hidden == nil || absent == nil ||
		hidden.Error() != absent.Error() {
		t.Fatalf("hidden error %v differs from absent error %v", hidden, absent)
	}
}

func assertSingleCitation(
	t *testing.T,
	response retrieval.Response,
	documentID shoal.ID,
) {
	t.Helper()
	if len(response.Results) != 1 ||
		len(response.Results[0].Evidence) == 0 ||
		response.Results[0].Evidence[0].Citation.DocumentID != documentID {
		t.Fatalf("retrieval response = %+v, want citation for %s", response, documentID)
	}
}

func assertIdenticalRetrieval(
	t *testing.T,
	want, got retrieval.Response,
	scoreBits uint64,
) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("authorized retrieval changed:\nwant %+v\ngot  %+v", want, got)
	}
	if len(got.Results) != 1 ||
		math.Float64bits(float64(got.Results[0].Score)) != scoreBits {
		t.Fatalf("authorized retrieval score bits changed: %+v", got)
	}
}

func firstViewNode(t *testing.T, view explorer.DocumentView) shoal.ID {
	t.Helper()
	var find func(explorer.SectionView) shoal.ID
	find = func(section explorer.SectionView) shoal.ID {
		if len(section.Spans) > 0 {
			return section.Spans[0].ID
		}
		for _, child := range section.Children {
			if id := find(child); id != "" {
				return id
			}
		}
		return ""
	}
	if id := find(view.Root); id != "" {
		return id
	}
	t.Fatal("document view contains no span node")
	return ""
}

func newFixtureCacheKey(
	t *testing.T,
	fixtures AuthorizationFixtures,
	decision auth.Decision,
	request retrieval.Request,
) auth.CacheKey {
	t.Helper()
	key, err := auth.NewCacheKey(auth.CacheKeyConfig{
		Decision:            decision,
		AuthorizationDomain: fixtures.Domain,
		PolicyCopyPin:       []byte("m2-policy-copy-pin"),
		SnapshotFrontier:    7,
		HistoryFloor:        1,
		RetentionGeneration: 1,
		Request:             request,
		Limits: map[string]uint64{
			"results": 1,
		},
		IndexGenerations: map[string][]byte{
			"lexical": []byte("m2-lexical-generation"),
		},
	})
	if err != nil {
		t.Fatalf("construct cache key: %v", err)
	}
	return key
}

func revokedAlphaGrants(
	t *testing.T,
	fixtures AuthorizationFixtures,
) []PrincipalGrant {
	t.Helper()
	admin, ok := fixtures.Grants(PrincipalAdmin)
	if !ok {
		t.Fatal("missing admin grants")
	}
	beta, ok := fixtures.Grants(PrincipalBeta)
	if !ok {
		t.Fatal("missing beta grants")
	}
	public := fixtureSource(t, fixtures, SourcePublic)
	return []PrincipalGrant{
		admin,
		{
			Principal:          PrincipalAlpha,
			PermittedSourceIDs: [][]byte{append([]byte(nil), public.SourceID...)},
			PermittedPolicyIDs: [][]byte{
				append([]byte(nil), public.GrantPolicyID...),
			},
		},
		beta,
	}
}

func normalizePrincipalGrants(
	grants []PrincipalGrant,
) ([]PrincipalGrant, error) {
	if len(grants) == 0 {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "principal grants are required")
	}
	normalized := clonePrincipalGrants(grants)
	seen := make(map[AuthorizationPrincipal]struct{}, len(normalized))
	for _, grant := range normalized {
		switch grant.Principal {
		case PrincipalAdmin, PrincipalAlpha, PrincipalBeta:
		default:
			return nil, shoal.NewError(
				shoal.ErrorInvalidArgument, "unknown authorization principal")
		}
		if _, duplicate := seen[grant.Principal]; duplicate {
			return nil, shoal.NewError(
				shoal.ErrorInvalidArgument, "principal grants must be unique")
		}
		seen[grant.Principal] = struct{}{}
		if len(grant.PermittedSourceIDs) == 0 ||
			len(grant.PermittedPolicyIDs) == 0 {
			return nil, shoal.NewError(
				shoal.ErrorInvalidArgument, "principal grants cannot be empty")
		}
	}
	return normalized, nil
}

func cloneAuthorizationSource(source AuthorizationSource) AuthorizationSource {
	source.Source = cloneExplorerSource(source.Source)
	source.SourceID = append([]byte(nil), source.SourceID...)
	source.GrantPolicyID = append([]byte(nil), source.GrantPolicyID...)
	return source
}

func cloneAuthorizationEdge(edge AuthorizationEdge) AuthorizationEdge {
	edge.Edge = cloneGraphEdge(edge.Edge)
	edge.SourceID = append([]byte(nil), edge.SourceID...)
	edge.GrantPolicyID = append([]byte(nil), edge.GrantPolicyID...)
	return edge
}

func clonePrincipalGrant(grant PrincipalGrant) PrincipalGrant {
	grant.PermittedSourceIDs = cloneByteSlices(grant.PermittedSourceIDs)
	grant.PermittedPolicyIDs = cloneByteSlices(grant.PermittedPolicyIDs)
	return grant
}

func clonePrincipalGrants(grants []PrincipalGrant) []PrincipalGrant {
	cloned := make([]PrincipalGrant, len(grants))
	for index, grant := range grants {
		cloned[index] = clonePrincipalGrant(grant)
	}
	return cloned
}

func cloneByteSlices(values [][]byte) [][]byte {
	cloned := make([][]byte, len(values))
	for index, value := range values {
		cloned[index] = append([]byte(nil), value...)
	}
	return cloned
}

func cloneExplorerSource(source explorer.Source) explorer.Source {
	source.Metadata = cloneMetadata(source.Metadata)
	return source
}

func cloneGraphEdge(edge graph.Edge) graph.Edge {
	edge.Properties = cloneMetadata(edge.Properties)
	return edge
}

func cloneMetadata(metadata shoal.Metadata) shoal.Metadata {
	if metadata == nil {
		return nil
	}
	cloned := make(shoal.Metadata, len(metadata))
	for key, value := range metadata {
		cloned[key] = value
	}
	return cloned
}

func cloneRetrievalRequest(request retrieval.Request) retrieval.Request {
	request.Modes = append([]retrieval.Mode(nil), request.Modes...)
	request.Scope.DocumentIDs = append(
		[]shoal.ID(nil), request.Scope.DocumentIDs...)
	request.Scope.NodeIDs = append([]shoal.ID(nil), request.Scope.NodeIDs...)
	return request
}
