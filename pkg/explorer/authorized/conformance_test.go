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

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/explorerconformance"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestAuthorizationConformance(t *testing.T) {
	explorerconformance.RunAuthorization(t, authorizationConformanceFactory)
}

func TestUnauthorizedBackendResultConformance(t *testing.T) {
	fixtures, err := explorerconformance.DefaultAuthorizationFixtures().Normalize()
	if err != nil {
		t.Fatal(err)
	}
	setup, err := newAuthorizationConformanceSetup(t, fixtures)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := setup.close(); err != nil {
			t.Errorf("close conformance setup: %v", err)
		}
	}()
	client, err := setup.newClient(setup.base)
	if err != nil {
		t.Fatal(err)
	}
	adminIngest := setup.mustIssue(
		t,
		explorerconformance.ContextRequest{
			Principal: explorerconformance.PrincipalAdmin,
			Operations: []auth.Operation{
				auth.OperationIngest,
			},
			Authority: explorerconformance.AuthorityPrimary,
			Domain:    explorerconformance.DomainPrimary,
		},
	)
	visibleFixture := mustAuthorizationSource(
		t, fixtures, explorerconformance.SourceRankingVisible)
	visible, err := client.Ingest(adminIngest.Context, visibleFixture.Source)
	if err != nil {
		t.Fatal(err)
	}
	hiddenFixture := mustAuthorizationSource(
		t, fixtures, explorerconformance.SourceRankingHidden)
	hidden, err := client.Ingest(adminIngest.Context, hiddenFixture.Source)
	if err != nil {
		t.Fatal(err)
	}
	alpha := setup.mustIssue(
		t,
		explorerconformance.ContextRequest{
			Principal: explorerconformance.PrincipalAlpha,
			Operations: []auth.Operation{
				auth.OperationRetrieve,
			},
			Authority: explorerconformance.AuthorityPrimary,
			Domain:    explorerconformance.DomainPrimary,
		},
	)
	beta := setup.mustIssue(
		t,
		explorerconformance.ContextRequest{
			Principal: explorerconformance.PrincipalBeta,
			Operations: []auth.Operation{
				auth.OperationRetrieve,
			},
			Authority: explorerconformance.AuthorityPrimary,
			Domain:    explorerconformance.DomainPrimary,
		},
	)
	request := retrieval.Request{
		Text: "alpha omega", TopK: 1, Explain: true,
	}
	hiddenResponse, err := client.Retrieve(beta.Context, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(hiddenResponse.Results) != 1 ||
		len(hiddenResponse.Results[0].Evidence) == 0 ||
		hiddenResponse.Results[0].Evidence[0].Citation.DocumentID !=
			hidden.Document.ID {
		t.Fatalf("hidden response = %+v", hiddenResponse)
	}
	citationEscape, err := setup.newClient(&maliciousResultBase{
		Client:   setup.base,
		response: hiddenResponse,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Run("citation", func(t *testing.T) {
		explorerconformance.AssertUnauthorizedBackendResult(
			t, citationEscape, alpha.Context, request)
	})

	adminConnect := setup.mustIssue(
		t,
		explorerconformance.ContextRequest{
			Principal: explorerconformance.PrincipalAdmin,
			Operations: []auth.Operation{
				auth.OperationConnect,
			},
			Authority: explorerconformance.AuthorityPrimary,
			Domain:    explorerconformance.DomainPrimary,
		},
	)
	edgeFixture, ok := fixtures.Edge(explorerconformance.EdgeGraphAB)
	if !ok {
		t.Fatal("missing graph edge fixture")
	}
	edge := edgeFixture.Edge
	edge.From = visible.Document.ID
	edge.To = hidden.Document.ID
	edge.ID = appEdgeID(t, client, adminConnect.Context, edge.ID)
	if err := client.Connect(adminConnect.Context, edge); err != nil {
		t.Fatal(err)
	}
	adminGraph := setup.mustIssue(
		t,
		explorerconformance.ContextRequest{
			Principal: explorerconformance.PrincipalAdmin,
			Operations: []auth.Operation{
				auth.OperationNeighborhood,
			},
			Authority: explorerconformance.AuthorityPrimary,
			Domain:    explorerconformance.DomainPrimary,
		},
	)
	neighborhood, err := client.Neighborhood(
		adminGraph.Context,
		explorer.NeighborhoodRequest{
			NodeIDs:   []shoal.ID{visible.Document.ID},
			EdgeTypes: []string{edge.Type},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	visibleNode := graphNode(t, neighborhood.Nodes, visible.Document.ID)
	hiddenNode := graphNode(t, neighborhood.Nodes, hidden.Document.ID)
	pathRequest := retrieval.Request{
		Text: "ranking alpha",
		TopK: 1,
		Scope: retrieval.Scope{DocumentIDs: []shoal.ID{
			visible.Document.ID,
		}},
		Explain: true,
	}
	pathResponse, err := client.Retrieve(alpha.Context, pathRequest)
	if err != nil {
		t.Fatal(err)
	}
	if len(pathResponse.Results) != 1 ||
		len(pathResponse.Results[0].Evidence) == 0 {
		t.Fatalf("visible response = %+v", pathResponse)
	}
	pathResponse.Results[0].Evidence[0].Path = graph.Path{
		Nodes: []graph.Node{visibleNode, hiddenNode},
		Edges: []graph.Edge{edge},
	}
	if err := pathResponse.ValidateFor(pathRequest); err != nil {
		t.Fatalf("malicious path fixture is structurally invalid: %v", err)
	}
	pathEscape, err := setup.newClient(&maliciousResultBase{
		Client:   setup.base,
		response: pathResponse,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Run("path", func(t *testing.T) {
		explorerconformance.AssertUnauthorizedBackendResult(
			t, pathEscape, alpha.Context, pathRequest)
	})
}

func authorizationConformanceFactory(
	t testing.TB,
	fixtures explorerconformance.AuthorizationFixtures,
) (explorerconformance.AuthorizationLifecycle, error) {
	normalized, err := fixtures.Normalize()
	if err != nil {
		return explorerconformance.AuthorizationLifecycle{}, err
	}
	setup, err := newAuthorizationConformanceSetup(t, normalized)
	if err != nil {
		return explorerconformance.AuthorizationLifecycle{}, err
	}
	client, err := setup.newClient(setup.base)
	if err != nil {
		_ = setup.close()
		return explorerconformance.AuthorizationLifecycle{}, err
	}
	return explorerconformance.AuthorizationLifecycle{
		Client: client,
		Issue:  setup.issue,
		Restart: func(ctx context.Context) (explorer.Client, error) {
			// This reuses the same M2 MemoryPolicyStore across a wrapped-client
			// restart; it intentionally makes no process-durability claim.
			if err := contextError(ctx); err != nil {
				return nil, err
			}
			if err := setup.base.Close(); err != nil {
				return nil, err
			}
			reopened, err := explorer.Open(setup.dataDir)
			if err != nil {
				return nil, err
			}
			setup.base = reopened
			return setup.newClient(reopened)
		},
		AdvancePolicyGeneration: setup.advance,
		Cleanup:                 setup.close,
	}, nil
}

type authorizationConformanceSetup struct {
	dataDir   string
	base      *explorer.Explorer
	fixtures  explorerconformance.AuthorizationFixtures
	store     *authorized.MemoryPolicyStore
	clock     func() time.Time
	authority *auth.Authority
	foreign   *auth.Authority
	state     *conformanceGenerationReader
	selector  *conformancePolicySelector
}

func newAuthorizationConformanceSetup(
	t testing.TB,
	fixtures explorerconformance.AuthorizationFixtures,
) (*authorizationConformanceSetup, error) {
	normalized, err := fixtures.Normalize()
	if err != nil {
		return nil, err
	}
	now := time.Date(2026, time.August, 26, 20, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	authority, err := auth.NewAuthorityWithClock(clock)
	if err != nil {
		return nil, err
	}
	foreign, err := auth.NewAuthorityWithClock(clock)
	if err != nil {
		return nil, err
	}
	dataDir := t.TempDir()
	base, err := explorer.Open(dataDir)
	if err != nil {
		return nil, err
	}
	state, err := newConformanceGenerationReader(
		normalized.Domain, normalized.InitialGrants)
	if err != nil {
		_ = base.Close()
		return nil, err
	}
	selector, err := newConformancePolicySelector(normalized)
	if err != nil {
		_ = base.Close()
		return nil, err
	}
	return &authorizationConformanceSetup{
		dataDir:   dataDir,
		base:      base,
		fixtures:  normalized,
		store:     authorized.NewMemoryPolicyStore(),
		clock:     clock,
		authority: authority,
		foreign:   foreign,
		state:     state,
		selector:  selector,
	}, nil
}

func (s *authorizationConformanceSetup) newClient(
	base explorer.Client,
) (*authorized.Client, error) {
	return authorized.NewClient(authorized.Config{
		Base: base,
		Resolver: conformanceDomainResolver{
			Resolver: s.authority.Resolver(),
			domain:   append([]byte(nil), s.fixtures.Domain...),
		},
		PolicySelector:     s.selector,
		EdgePolicySelector: s.selector,
		PolicyStore:        s.store,
		GenerationReader:   s.state,
		Clock:              s.clock,
	})
}

func (s *authorizationConformanceSetup) issue(
	ctx context.Context,
	request explorerconformance.ContextRequest,
) (explorerconformance.BoundAuthorization, error) {
	if err := contextError(ctx); err != nil {
		return explorerconformance.BoundAuthorization{}, err
	}
	if len(request.Operations) == 0 {
		return explorerconformance.BoundAuthorization{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "fixture operations are required")
	}
	for _, operation := range request.Operations {
		if err := operation.Validate(); err != nil {
			return explorerconformance.BoundAuthorization{}, err
		}
	}
	generation, grant, sequence, err := s.state.issue(request.Principal)
	if err != nil {
		return explorerconformance.BoundAuthorization{}, err
	}
	domain := s.fixtures.Domain
	switch request.Domain {
	case "", explorerconformance.DomainPrimary:
	case explorerconformance.DomainForeign:
		domain = s.fixtures.ForeignDomain
	default:
		return explorerconformance.BoundAuthorization{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "unknown fixture domain")
	}
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject:               shoal.ID("m2-" + string(request.Principal)),
		Actor:                 shoal.ID("m2-" + string(request.Principal) + "-actor"),
		AuthorizationDomain:   domain,
		AllowedOperations:     append([]auth.Operation(nil), request.Operations...),
		PermittedSourceIDs:    cloneByteSlices(grant.PermittedSourceIDs),
		PermittedPolicyIDs:    cloneByteSlices(grant.PermittedPolicyIDs),
		PolicyGeneration:      generation,
		AuthenticationExpires: s.clock().Add(24 * time.Hour),
		RequestID: shoal.ID(fmt.Sprintf(
			"m2-%s-request-%d", request.Principal, sequence)),
	})
	if err != nil {
		return explorerconformance.BoundAuthorization{}, err
	}
	binder := s.authority.Binder()
	switch request.Authority {
	case "", explorerconformance.AuthorityPrimary:
	case explorerconformance.AuthorityForeign:
		binder = s.foreign.Binder()
	default:
		return explorerconformance.BoundAuthorization{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "unknown fixture authority")
	}
	bound, err := binder.Bind(ctx, decision)
	if err != nil {
		return explorerconformance.BoundAuthorization{}, err
	}
	return explorerconformance.BoundAuthorization{
		Context:  bound,
		Decision: decision,
	}, nil
}

func (s *authorizationConformanceSetup) mustIssue(
	t testing.TB,
	request explorerconformance.ContextRequest,
) explorerconformance.BoundAuthorization {
	t.Helper()
	bound, err := s.issue(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	return bound
}

func (s *authorizationConformanceSetup) advance(
	ctx context.Context,
	advance explorerconformance.PolicyGenerationAdvance,
) error {
	return s.state.advance(ctx, advance)
}

func (s *authorizationConformanceSetup) close() error {
	if s == nil || s.base == nil {
		return nil
	}
	return s.base.Close()
}

type conformanceDomainResolver struct {
	auth.Resolver
	domain []byte
}

func (r conformanceDomainResolver) Resolve(
	ctx context.Context,
) (auth.Decision, error) {
	decision, err := r.Resolver.Resolve(ctx)
	if err != nil {
		return auth.Decision{}, err
	}
	if !bytes.Equal(decision.AuthorizationDomain(), r.domain) {
		return auth.Decision{}, shoal.NewError(
			shoal.ErrorUnauthorized, "authorization denied")
	}
	return decision, nil
}

type conformanceGenerationReader struct {
	mu         sync.Mutex
	domain     []byte
	generation int64
	grants     map[explorerconformance.AuthorizationPrincipal]explorerconformance.PrincipalGrant
	pending    map[explorerconformance.AuthorizationPrincipal]explorerconformance.PrincipalGrant
	sequence   uint64
}

func newConformanceGenerationReader(
	domain []byte,
	grants []explorerconformance.PrincipalGrant,
) (*conformanceGenerationReader, error) {
	normalized, err := conformanceGrantMap(grants)
	if err != nil {
		return nil, err
	}
	return &conformanceGenerationReader{
		domain:     append([]byte(nil), domain...),
		generation: 1,
		grants:     normalized,
	}, nil
}

func (r *conformanceGenerationReader) CurrentPolicyGeneration(
	ctx context.Context,
	domain []byte,
) (int64, error) {
	if err := contextError(ctx); err != nil {
		return 0, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !bytes.Equal(domain, r.domain) {
		return 0, shoal.NewError(
			shoal.ErrorUnauthorized, "authorization denied")
	}
	if r.pending != nil {
		r.generation++
		r.grants = r.pending
		r.pending = nil
	}
	return r.generation, nil
}

func (r *conformanceGenerationReader) issue(
	principal explorerconformance.AuthorizationPrincipal,
) (int64, explorerconformance.PrincipalGrant, uint64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	grant, ok := r.grants[principal]
	if !ok {
		return 0, explorerconformance.PrincipalGrant{}, 0, shoal.NewError(
			shoal.ErrorUnauthorized, "authorization denied")
	}
	r.sequence++
	return r.generation, clonePrincipalGrant(grant), r.sequence, nil
}

func (r *conformanceGenerationReader) advance(
	ctx context.Context,
	advance explorerconformance.PolicyGenerationAdvance,
) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	grants, err := conformanceGrantMap(advance.Grants)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending != nil {
		return shoal.NewError(
			shoal.ErrorConflict, "policy generation advance is already pending")
	}
	switch advance.Timing {
	case explorerconformance.AdvanceImmediately:
		r.generation++
		r.grants = grants
	case explorerconformance.AdvanceBeforeSerialization:
		r.pending = grants
	default:
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "unknown generation advance timing")
	}
	return nil
}

func conformanceGrantMap(
	grants []explorerconformance.PrincipalGrant,
) (map[explorerconformance.AuthorizationPrincipal]explorerconformance.PrincipalGrant, error) {
	if len(grants) == 0 {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "principal grants are required")
	}
	normalized := make(
		map[explorerconformance.AuthorizationPrincipal]explorerconformance.PrincipalGrant,
		len(grants),
	)
	for _, grant := range grants {
		switch grant.Principal {
		case explorerconformance.PrincipalAdmin,
			explorerconformance.PrincipalAlpha,
			explorerconformance.PrincipalBeta:
		default:
			return nil, shoal.NewError(
				shoal.ErrorInvalidArgument, "unknown fixture principal")
		}
		if _, duplicate := normalized[grant.Principal]; duplicate {
			return nil, shoal.NewError(
				shoal.ErrorInvalidArgument, "duplicate fixture principal grants")
		}
		if len(grant.PermittedSourceIDs) == 0 ||
			len(grant.PermittedPolicyIDs) == 0 {
			return nil, shoal.NewError(
				shoal.ErrorInvalidArgument, "fixture grants cannot be empty")
		}
		normalized[grant.Principal] = clonePrincipalGrant(grant)
	}
	return normalized, nil
}

type conformanceSourceKey struct {
	title     string
	mediaType string
	content   string
}

type conformanceEdgeKey struct {
	id       shoal.ID
	edgeType string
	weight   shoal.Score
}

type conformancePolicyIdentity struct {
	source []byte
	policy []byte
}

type conformancePolicySelector struct {
	sources map[conformanceSourceKey]conformancePolicyIdentity
	edges   map[conformanceEdgeKey]conformancePolicyIdentity
}

func newConformancePolicySelector(
	fixtures explorerconformance.AuthorizationFixtures,
) (*conformancePolicySelector, error) {
	selector := &conformancePolicySelector{
		sources: make(
			map[conformanceSourceKey]conformancePolicyIdentity,
			len(fixtures.Sources),
		),
		edges: make(
			map[conformanceEdgeKey]conformancePolicyIdentity,
			len(fixtures.Edges),
		),
	}
	for _, fixture := range fixtures.Sources {
		key := conformanceSourceKey{
			title:     fixture.Source.Title,
			mediaType: fixture.Source.MediaType,
			content:   fixture.Source.Content,
		}
		if _, duplicate := selector.sources[key]; duplicate {
			return nil, shoal.NewError(
				shoal.ErrorInvalidArgument,
				"fixture source selector identities must be unique",
			)
		}
		selector.sources[key] = conformancePolicyIdentity{
			source: append([]byte(nil), fixture.SourceID...),
			policy: append([]byte(nil), fixture.GrantPolicyID...),
		}
	}
	for _, fixture := range fixtures.Edges {
		key := conformanceEdgeKey{
			id:       fixture.Edge.ID,
			edgeType: fixture.Edge.Type,
			weight:   fixture.Edge.Weight,
		}
		if _, duplicate := selector.edges[key]; duplicate {
			return nil, shoal.NewError(
				shoal.ErrorInvalidArgument,
				"fixture edge selector identities must be unique",
			)
		}
		selector.edges[key] = conformancePolicyIdentity{
			source: append([]byte(nil), fixture.SourceID...),
			policy: append([]byte(nil), fixture.GrantPolicyID...),
		}
	}
	return selector, nil
}

func (s *conformancePolicySelector) SelectPolicy(
	ctx context.Context,
	decision auth.Decision,
	source explorer.Source,
) (auth.Policy, error) {
	if err := contextError(ctx); err != nil {
		return auth.Policy{}, err
	}
	identity, ok := s.sources[conformanceSourceKey{
		title:     source.Title,
		mediaType: source.MediaType,
		content:   source.Content,
	}]
	if !ok {
		return auth.Policy{}, shoal.NewError(
			shoal.ErrorUnauthorized, "authorization denied")
	}
	return selectedConformancePolicy(decision, identity)
}

func (s *conformancePolicySelector) SelectEdgePolicy(
	ctx context.Context,
	decision auth.Decision,
	edge graph.Edge,
) (auth.Policy, error) {
	if err := contextError(ctx); err != nil {
		return auth.Policy{}, err
	}
	// Connect accepts only IDs in the caller's namespace (#699); the
	// fixtures name edges by the local ID the harness chose.
	local, ok := authorized.ApplicationEdgeLocalID(decision, edge.ID)
	if !ok {
		return auth.Policy{}, shoal.NewError(
			shoal.ErrorUnauthorized, "authorization denied")
	}
	identity, ok := s.edges[conformanceEdgeKey{
		id:       local,
		edgeType: edge.Type,
		weight:   edge.Weight,
	}]
	if !ok {
		return auth.Policy{}, shoal.NewError(
			shoal.ErrorUnauthorized, "authorization denied")
	}
	return selectedConformancePolicy(decision, identity)
}

func selectedConformancePolicy(
	decision auth.Decision,
	identity conformancePolicyIdentity,
) (auth.Policy, error) {
	return auth.NewPolicy(auth.PolicyConfig{
		AuthorizationDomain: decision.AuthorizationDomain(),
		SourceID:            append([]byte(nil), identity.source...),
		GrantPolicyID:       append([]byte(nil), identity.policy...),
		Epoch:               decision.PolicyGeneration(),
	})
}

type maliciousResultBase struct {
	explorer.Client
	response retrieval.Response
}

func (b *maliciousResultBase) Retrieve(
	context.Context,
	retrieval.Request,
) (retrieval.Response, error) {
	return b.response, nil
}

func mustAuthorizationSource(
	t testing.TB,
	fixtures explorerconformance.AuthorizationFixtures,
	name explorerconformance.AuthorizationSourceName,
) explorerconformance.AuthorizationSource {
	t.Helper()
	source, ok := fixtures.Source(name)
	if !ok {
		t.Fatalf("missing authorization source %s", name)
	}
	return source
}

func graphNode(
	t testing.TB,
	nodes []graph.Node,
	id shoal.ID,
) graph.Node {
	t.Helper()
	for _, node := range nodes {
		if node.ID == id {
			return node
		}
	}
	t.Fatalf("missing graph node %s in %+v", id, nodes)
	return graph.Node{}
}

func clonePrincipalGrant(
	grant explorerconformance.PrincipalGrant,
) explorerconformance.PrincipalGrant {
	grant.PermittedSourceIDs = cloneByteSlices(grant.PermittedSourceIDs)
	grant.PermittedPolicyIDs = cloneByteSlices(grant.PermittedPolicyIDs)
	return grant
}

func cloneByteSlices(values [][]byte) [][]byte {
	cloned := make([][]byte, len(values))
	for index, value := range values {
		cloned[index] = append([]byte(nil), value...)
	}
	return cloned
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return shoal.NewError(shoal.ErrorInvalidArgument, "context is required")
	}
	switch ctx.Err() {
	case context.Canceled:
		return shoal.NewError(shoal.ErrorCanceled, "operation canceled")
	case context.DeadlineExceeded:
		return shoal.NewError(shoal.ErrorDeadline, "operation deadline exceeded")
	default:
		return nil
	}
}

var (
	_ auth.Resolver                 = conformanceDomainResolver{}
	_ auth.GenerationReader         = (*conformanceGenerationReader)(nil)
	_ authorized.PolicySelector     = (*conformancePolicySelector)(nil)
	_ authorized.EdgePolicySelector = (*conformancePolicySelector)(nil)
	_ explorer.Client               = (*maliciousResultBase)(nil)
)
