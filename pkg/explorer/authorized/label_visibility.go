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

// The reader label evaluator (#564).
//
// Stored, unscanned label sets (dispatch evidence, lifecycle envelopes,
// interaction and fold records) are decided here, once, for every consumer:
// fleet and fleetevents take LabelVisibility through the
// evidencelabels.Visibility seam, and this package's own interaction and fold
// reads call the same evaluation.
//
// A stored label set is a conjunction of structured grant labels (d:, s:,
// g:<policy>:e:<epoch>, svc:<role>). A user decision may see it when its
// grants cover every term (auth.VisibilityPermittedForUser); a trusted
// service additionally needs every term inside its configured ceiling
// (auth.VisibilityPermittedForService). Free-form ingest labels such as
// "secret" are held by nobody. Since #570 each one is enforced as a structured
// policy per (source, label) in the node's AccessRule, and the label
// translation below rewrites a free-form term into that policy's grant labels
// wherever the nodes it came from are known.

import (
	"context"

	"sort"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/evidencelabels"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// CeilingResolver returns the configured ceiling for a trusted-service
// decision. A service with no configured ceiling resolves to the zero
// ServiceCeiling and a nil error; the evaluator then answers false. An error
// is the lookup failing, and is returned rather than read as a refusal.
//
// It has the method set of workspace.CeilingResolver, so one configured
// resolver serves output-policy authorization and label visibility alike.
type CeilingResolver interface {
	ResolveServiceCeiling(context.Context, auth.Decision) (auth.ServiceCeiling, error)
}

// StaticCeilingResolver is a CeilingResolver over a fixed set of configured
// service accounts, keyed by ceiling identity. It never caches a decision.
type StaticCeilingResolver struct {
	ceilings map[shoal.ID]auth.ServiceCeiling
}

// NewStaticCeilingResolver validates every configured ceiling through
// auth.NewServiceCeiling and refuses two ceilings with one identity, because
// a decision names its ceiling by identity alone. No configuration is valid:
// every trusted service then sees only unlabelled data.
func NewStaticCeilingResolver(
	configs ...auth.ServiceCeilingConfig,
) (*StaticCeilingResolver, error) {
	ceilings := make(map[shoal.ID]auth.ServiceCeiling, len(configs))
	for _, config := range configs {
		ceiling, err := auth.NewServiceCeiling(config)
		if err != nil {
			return nil, err
		}
		if _, duplicate := ceilings[ceiling.Identity()]; duplicate {
			return nil, shoal.NewError(
				shoal.ErrorInvalidArgument,
				"service ceiling identity is configured more than once")
		}
		ceilings[ceiling.Identity()] = ceiling
	}
	return &StaticCeilingResolver{ceilings: ceilings}, nil
}

// ResolveServiceCeiling returns the ceiling the decision names, or the zero
// ceiling when none is configured for it.
func (r *StaticCeilingResolver) ResolveServiceCeiling(
	_ context.Context, decision auth.Decision,
) (auth.ServiceCeiling, error) {
	if r == nil || decision.ServiceCeilingIdentity() == "" {
		return auth.ServiceCeiling{}, nil
	}
	return r.ceilings[decision.ServiceCeilingIdentity()], nil
}

// LabelVisibilityConfig supplies the evaluator's dependencies.
type LabelVisibilityConfig struct {
	// Resolver establishes the reader's decision from the request context.
	Resolver auth.Resolver
	// Ceilings resolves a trusted service's ceiling. Nil means no service
	// account has one, so every trusted service is refused labelled data.
	Ceilings CeilingResolver
	// Clock supplies the evaluation time.
	Clock func() time.Time
}

// LabelVisibility implements evidencelabels.Visibility from the decision bound
// to the request context. It keeps no state between calls: every answer is
// computed from the decision resolved for that call, the ceiling resolved for
// it, and the labels asked about.
type LabelVisibility struct {
	resolver auth.Resolver
	ceilings CeilingResolver
	clock    func() time.Time
}

var _ evidencelabels.Visibility = (*LabelVisibility)(nil)

// NewLabelVisibility requires a resolver and a clock.
func NewLabelVisibility(config LabelVisibilityConfig) (*LabelVisibility, error) {
	if isNilDependency(config.Resolver) {
		return nil, dependencyRequired("authorization resolver")
	}
	if config.Clock == nil {
		return nil, dependencyRequired("clock")
	}
	ceilings := config.Ceilings
	if isNilDependency(ceilings) {
		ceilings = nil
	}
	return &LabelVisibility{
		resolver: config.Resolver, ceilings: ceilings, clock: config.Clock,
	}, nil
}

// VisibleToReader reports whether the reader behind ctx may see data stored
// with the conjunction visibility. A resolver or ceiling-resolver failure,
// and an invalid or expired decision, is an error. A malformed or free-form
// term, and a trusted service with no configured ceiling, is false.
func (v *LabelVisibility) VisibleToReader(
	ctx context.Context, visibility []string,
) (bool, error) {
	if v == nil || isNilDependency(v.resolver) || v.clock == nil {
		return false, shoal.NewError(
			shoal.ErrorUnavailable, "label visibility evaluator is unavailable")
	}
	if err := contextFailure(ctx); err != nil {
		return false, err
	}
	decision, err := v.resolver.Resolve(ctx)
	if err != nil {
		return false, resolverFailure(ctx, err)
	}
	return v.permits(ctx, decision, visibility, v.clock())
}

// evaluatorFor resolves the reader's ceiling once and returns the evaluation
// for that decision, so a batch of label sets costs one ceiling resolution.
func (v *LabelVisibility) evaluatorFor(
	ctx context.Context, decision auth.Decision, now time.Time,
) (func([]string) (bool, error), error) {
	if !decision.TrustedService() {
		return func(labels []string) (bool, error) {
			if len(labels) == 0 {
				return true, nil
			}
			return auth.VisibilityPermittedForUser(decision, labels, now)
		}, nil
	}
	var ceiling auth.ServiceCeiling
	if v.ceilings != nil {
		resolved, err := v.ceilings.ResolveServiceCeiling(ctx, decision)
		if err != nil {
			if contextErr := contextFailure(ctx); contextErr != nil {
				return nil, contextErr
			}
			return nil, shoal.NewError(
				shoal.ErrorUnavailable, "service ceiling resolution unavailable")
		}
		ceiling = resolved
	}
	return func(labels []string) (bool, error) {
		if len(labels) == 0 {
			return true, nil
		}
		return auth.VisibilityPermittedForService(decision, labels, ceiling, now)
	}, nil
}

// permits is the one evaluation: the exported seam above and this package's
// interaction and fold reads both reach it, with the decision each has
// already resolved.
func (v *LabelVisibility) permits(
	ctx context.Context, decision auth.Decision, labels []string, now time.Time,
) (bool, error) {
	if len(labels) == 0 {
		// Unlabelled: nothing to lack, for every reader, as
		// evidencelabels.Filter keeps unlabelled values without asking. The
		// service primitive refuses a missing ceiling before it looks at the
		// labels, which would withhold unlabelled records from a service
		// with none configured.
		return true, nil
	}
	if !decision.TrustedService() {
		return auth.VisibilityPermittedForUser(decision, labels, now)
	}
	var ceiling auth.ServiceCeiling
	if v.ceilings != nil {
		resolved, err := v.ceilings.ResolveServiceCeiling(ctx, decision)
		if err != nil {
			if contextErr := contextFailure(ctx); contextErr != nil {
				return false, contextErr
			}
			return false, shoal.NewError(
				shoal.ErrorUnavailable, "service ceiling resolution unavailable")
		}
		ceiling = resolved
	}
	return auth.VisibilityPermittedForService(decision, labels, ceiling, now)
}

// LabelVisibility returns this client's evaluator, built from its resolver,
// clock and configured CeilingResolver. Hosts pass it to the dispatch and
// event planes so that they and this client's own interaction and fold reads
// answer the label question identically.
func (c *Client) LabelVisibility() *LabelVisibility {
	return c.labelVisibility
}

// NodeGate decides whether the reader behind ctx may see graph members under
// their current access rules, the same effective rules the interaction read
// gate applies (resolveNodes and resolveEdges, including the document-bound
// rule of extracted entities and relations, #570/#585): every node, every
// edge, and both endpoints of every edge. Every term of every policy in those
// rules (d:, s:, g:, svc:) must pass the reader label evaluator, which for a
// user is exactly the domain, source and policy check AccessRule.Authorize
// makes, and for a trusted service is additionally bounded by its ceiling. A
// node or edge the catalog does not know is not visible. It implements
// evidencelabels.NodeGate for the dispatch and event planes.
type NodeGate struct {
	client *Client
}

var _ evidencelabels.NodeGate = (*NodeGate)(nil)

// NodeGate returns the current-rule node gate over this client's catalog.
func (c *Client) NodeGate() *NodeGate {
	return &NodeGate{client: c}
}

// GraphsVisibleToReader implements evidencelabels.NodeGate. The whole batch
// costs one decision resolution, one ceiling resolution, chunked edge, node
// and current-revision lookups for the distinct members of every graph, and
// one revision lookup per distinct cited (document, revision): never a
// round trip per reference.
func (g *NodeGate) GraphsVisibleToReader(
	ctx context.Context, graphs []evidencelabels.Graph,
) ([]bool, error) {
	if g == nil || g.client == nil {
		return nil, shoal.NewError(
			shoal.ErrorUnavailable, "node gate is unavailable")
	}
	c := g.client
	if err := contextFailure(ctx); err != nil {
		return nil, err
	}
	decision, err := c.resolver.Resolve(ctx)
	if err != nil {
		return nil, resolverFailure(ctx, err)
	}
	now := c.clock()
	permits, err := c.labelVisibility.evaluatorFor(ctx, decision, now)
	if err != nil {
		return nil, err
	}
	// Shared extracted nodes and relations are projected onto an owner this
	// reader can read (#700), so the terms below are a readable owner's.
	reader := readerFor(decision, auth.OperationRead, now)

	// Gather the distinct members of every graph.
	var edgeIDs []shoal.ID
	seenEdges := map[shoal.ID]bool{}
	type revisionKey struct{ document, revision shoal.ID }
	revisions := map[revisionKey]*RevisionRegistration{}
	for _, graph := range graphs {
		for _, id := range graph.EdgeIDs {
			if !seenEdges[id] {
				seenEdges[id] = true
				edgeIDs = append(edgeIDs, id)
			}
		}
		if graph.DocumentID != "" || graph.RevisionID != "" {
			revisions[revisionKey{graph.DocumentID, graph.RevisionID}] = nil
		}
	}
	edges := registeredEdges{}
	for start := 0; start < len(edgeIDs); {
		end := chunkEnd(start, len(edgeIDs), maxInteractionAuthorizationIDs)
		resolved, err := c.resolveEdges(ctx, edgeIDs[start:end], reader)
		if err != nil {
			return nil, err
		}
		for id, registration := range resolved {
			edges[id] = registration
		}
		start = end
	}
	var nodeIDs []shoal.ID
	seenNodes := map[shoal.ID]bool{}
	addNode := func(id shoal.ID) {
		if !seenNodes[id] {
			seenNodes[id] = true
			nodeIDs = append(nodeIDs, id)
		}
	}
	for _, graph := range graphs {
		for _, id := range graph.NodeIDs {
			addNode(id)
		}
	}
	for _, registration := range edges {
		addNode(registration.Edge.From)
		addNode(registration.Edge.To)
	}
	nodes := registeredNodes{}
	for start := 0; start < len(nodeIDs); {
		end := chunkEnd(start, len(nodeIDs), maxInteractionAuthorizationIDs)
		resolved, err := c.resolveNodes(ctx, nodeIDs[start:end], reader)
		if err != nil {
			return nil, err
		}
		for id, registration := range resolved {
			nodes[id] = registration
		}
		start = end
	}
	// The cited revision's own rule and, for a historical revision, the
	// current revision's: revisionAllows, decided by terms (#585).
	var documents []shoal.ID
	for key := range revisions {
		registration, ok, err := c.policyStore.Revision(ctx, key.document, key.revision)
		if err != nil {
			return nil, policyCatalogReadError(ctx, err)
		}
		if ok && registration.DocumentID == key.document {
			found := registration
			revisions[key] = &found
			if !registration.Current {
				documents = append(documents, key.document)
			}
		}
	}
	current := currentRevisions{}
	for start := 0; start < len(documents); {
		end := chunkEnd(start, len(documents), maxInteractionAuthorizationIDs)
		resolved, err := c.resolveCurrentRevisions(ctx, documents[start:end])
		if err != nil {
			return nil, err
		}
		for id, registration := range resolved {
			current[id] = registration
		}
		start = end
	}

	verdicts := make([]bool, len(graphs))
	for index, graph := range graphs {
		var terms []string
		add := func(rule AccessRule) bool {
			if len(rule.policies) == 0 {
				return false
			}
			for _, policy := range rule.policies {
				policyTerms, err := policy.VisibilityTerms()
				if err != nil {
					return false
				}
				terms = append(terms, policyTerms...)
			}
			return true
		}
		known := true
		members := append([]shoal.ID(nil), graph.NodeIDs...)
		for _, id := range graph.EdgeIDs {
			registration, ok := edges[id]
			if !ok || registration.Edge.ID != id || !add(registration.Rule) {
				known = false
				break
			}
			members = append(members, registration.Edge.From, registration.Edge.To)
		}
		for _, id := range members {
			if !known {
				break
			}
			registration, ok := nodes[id]
			if !ok || !add(registration.Rule) {
				known = false
			}
		}
		if known && (graph.DocumentID != "" || graph.RevisionID != "") {
			revision := revisions[revisionKey{graph.DocumentID, graph.RevisionID}]
			switch {
			case revision == nil || !add(revision.Rule):
				known = false
			case !revision.Current:
				latest, ok := current[graph.DocumentID]
				if !ok || latest.DocumentID != graph.DocumentID || !add(latest.Rule) {
					known = false
				}
			}
		}
		if !known || len(terms) == 0 {
			continue
		}
		visible, err := permits(terms)
		if err != nil {
			return nil, err
		}
		verdicts[index] = visible
	}
	return verdicts, nil
}

// GraphEvidenceValid implements evidencelabels.NodeGate. Each graph
// reference's edge i must run from node i to node i+1 as the catalog records
// it; then every graph reference is validated by the trusted evidence
// snapshot validator at the executor's pinned snapshot, exactly as the
// interaction recorder validates graph evidence: the path, the anchor
// identity, and the assertions, which must be precisely the ones the corpus
// records on the path's edges. A reference, snapshot or validator the corpus
// cannot vouch for is not valid.
func (g *NodeGate) GraphEvidenceValid(
	ctx context.Context, snapshotID shoal.ID, snapshotAsOf time.Time,
	references []interaction.EvidenceReference,
) (bool, error) {
	if g == nil || g.client == nil {
		return false, shoal.NewError(
			shoal.ErrorUnavailable, "node gate is unavailable")
	}
	var graphs []interaction.EvidenceReference
	var nodeIDs, edgeIDs []shoal.ID
	for _, reference := range references {
		if reference.Kind != interaction.EvidenceGraph {
			continue
		}
		joined, err := g.pathJoins(ctx, reference.NodeIDs, reference.EdgeIDs)
		if err != nil || !joined {
			return false, err
		}
		graphs = append(graphs, reference)
		nodeIDs = append(nodeIDs, reference.NodeIDs...)
		edgeIDs = append(edgeIDs, reference.EdgeIDs...)
	}
	if len(graphs) == 0 {
		return true, nil
	}
	validator, ok := g.client.snapshotValidator.(EvidenceSnapshotValidator)
	if !ok || isNilDependency(g.client.snapshotValidator) {
		return false, nil
	}
	err := validator.ValidateEvidenceSnapshot(
		ctx, snapshotID, snapshotAsOf, nodeIDs, edgeIDs, graphs)
	switch {
	case err == nil:
		return true, nil
	case shoal.IsErrorCode(err, shoal.ErrorConflict),
		shoal.IsErrorCode(err, shoal.ErrorInvalidArgument),
		shoal.IsErrorCode(err, shoal.ErrorNotFound):
		return false, nil
	default:
		return false, directBaseError(err)
	}
}

// pathJoins reports whether edgeIDs join nodeIDs in sequence in the catalog.
func (g *NodeGate) pathJoins(
	ctx context.Context, nodeIDs, edgeIDs []shoal.ID,
) (bool, error) {
	if len(edgeIDs) != len(nodeIDs)-1 {
		return false, nil
	}
	for start := 0; start < len(edgeIDs); {
		end := chunkEnd(start, len(edgeIDs), maxInteractionAuthorizationIDs)
		resolved, err := g.client.resolveEdges(ctx, edgeIDs[start:end], nil)
		if err != nil {
			return false, err
		}
		for index := start; index < end; index++ {
			registration, ok := resolved[edgeIDs[index]]
			if !ok || registration.Edge.From != nodeIDs[index] ||
				registration.Edge.To != nodeIDs[index+1] {
				return false, nil
			}
		}
		start = end
	}
	return true, nil
}

// LabelTranslator rewrites a stored label set's free-form ingest terms into
// the structured grant labels of the label policies enforcing them (#570).
// It implements evidencelabels.Translator for the dispatch plane, which calls
// it when an executor's evidence is recorded.
//
// It reads only the policy catalog and returns only labels derived from the
// rules of the nodes it is given. It is a trusted host seam, not a reader
// API: it does not authorize the caller.
type LabelTranslator struct {
	client *Client
}

var _ evidencelabels.Translator = (*LabelTranslator)(nil)

// LabelTranslator returns the record-time translator over this client's
// policy catalog.
func (c *Client) LabelTranslator() *LabelTranslator {
	return &LabelTranslator{client: c}
}

// StructuredVisibility implements evidencelabels.Translator.
func (t *LabelTranslator) StructuredVisibility(
	ctx context.Context, nodeIDs, edgeIDs []shoal.ID, visibility []string,
) ([]string, error) {
	if t == nil || t.client == nil {
		return nil, shoal.NewError(
			shoal.ErrorUnavailable, "label translator is unavailable")
	}
	// A trusted seam with no reader: a shared extracted node is translated
	// over its first contributing owner, as before #700.
	return t.client.structuredLabels(ctx, nil, visibility, nodeIDs, edgeIDs)
}

// structuredLabels translates labels over the current rules of nodeIDs and
// of the endpoints of edgeIDs, each shared extracted one projected for reader
// (nil for a reader-free use). A set with no free-form term is returned as
// is, without a catalog read.
func (c *Client) structuredLabels(
	ctx context.Context, reader *registrationReader,
	labels []string, nodeIDs, edgeIDs []shoal.ID,
) ([]string, error) {
	if !hasFreeFormTerm(labels) {
		return append([]string(nil), labels...), nil
	}
	complete := true
	nodes := append([]shoal.ID(nil), nodeIDs...)
	for start := 0; start < len(edgeIDs); {
		end := chunkEnd(start, len(edgeIDs), maxInteractionAuthorizationIDs)
		chunk := edgeIDs[start:end]
		start = end
		edges, err := c.resolveEdges(ctx, chunk, reader)
		if err != nil {
			return nil, err
		}
		for _, edgeID := range chunk {
			registration, ok := edges[edgeID]
			if !ok {
				complete = false
				continue
			}
			nodes = append(nodes, registration.Edge.From, registration.Edge.To)
		}
	}
	registrations := make(registeredNodes, len(nodes))
	for start := 0; start < len(nodes); {
		end := chunkEnd(start, len(nodes), maxInteractionAuthorizationIDs)
		resolved, err := c.resolveNodes(ctx, nodes[start:end], reader)
		if err != nil {
			return nil, err
		}
		for id, registration := range resolved {
			registrations[id] = registration
		}
		start = end
	}
	return structuredLabelTerms(labels, nodes, registrations, complete), nil
}

func hasFreeFormTerm(labels []string) bool {
	for _, label := range labels {
		if !auth.IsStructuredVisibilityTerm(label) {
			return true
		}
	}
	return false
}

// structuredLabelTerms is the translation, the same at record time and at
// read time. It reads the nodes' current AccessRules, never their
// shoal.visibility property: the rule is what every read gate enforces, and
// the policy catalog does not keep node properties.
//
//   - A structured term is kept.
//   - A free-form term L is replaced by the grant labels (d:, s:, g:) of every
//     label policy for L (LabelPolicyID(source, L)) in the nodes' rules. That
//     is exactly the policy each node's own read gate enforces for L (#570),
//     at the rule's own epoch.
//   - L is kept unchanged, so that the set stays unsatisfiable, when the
//     mapping cannot be made: a node is unresolved, or no rule has a label
//     policy for L. Keeping it withholds; dropping it would open.
//   - A rule carrying a reserved label policy (an untranslatable document)
//     contributes that policy's grant labels, which no decision can hold, so
//     a set translated over such a node is never satisfiable.
//
// A malformed stored term is not structured and names no label policy, so it
// is kept and the evaluator answers false for it.
func structuredLabelTerms(
	labels []string,
	nodeIDs []shoal.ID,
	registrations registeredNodes,
	complete bool,
) []string {
	byLabel := make(map[string][]string)
	terms := make(map[string]struct{}, len(labels))
	seen := make(map[shoal.ID]struct{}, len(nodeIDs))
	for _, nodeID := range nodeIDs {
		if _, duplicate := seen[nodeID]; duplicate {
			continue
		}
		seen[nodeID] = struct{}{}
		registration, ok := registrations[nodeID]
		if !ok {
			complete = false
			continue
		}
		for _, policy := range registration.Rule.policies {
			id := policy.GrantPolicyID()
			if !auth.IsLabelPolicyID(id) {
				continue
			}
			policyTerms, err := policy.VisibilityTerms()
			if err != nil {
				complete = false
				continue
			}
			if auth.IsReservedLabelPolicyID(id) {
				for _, term := range policyTerms {
					terms[term] = struct{}{}
				}
				continue
			}
			_, label, err := ParseLabelPolicyID(id)
			if err != nil {
				complete = false
				continue
			}
			byLabel[label] = append(byLabel[label], policyTerms...)
		}
	}
	if len(seen) == 0 {
		complete = false
	}
	for _, label := range labels {
		if auth.IsStructuredVisibilityTerm(label) {
			terms[label] = struct{}{}
			continue
		}
		translated := byLabel[label]
		if !complete || len(translated) == 0 {
			terms[label] = struct{}{}
			continue
		}
		for _, term := range translated {
			terms[term] = struct{}{}
		}
	}
	if len(terms) == 0 {
		return nil
	}
	result := make([]string, 0, len(terms))
	for term := range terms {
		result = append(result, term)
	}
	sort.Strings(result)
	return result
}
