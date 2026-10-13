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
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"
	"time"

	"github.com/phrocker/shoal-oss/internal/lexiconscope"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/lexicon"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	// MaxMentionBytes bounds the text ResolveMentions accepts.
	MaxMentionBytes = 4096
	// MaxMentionTokens bounds the tokens ResolveMentions accepts after
	// normalization.
	MaxMentionTokens = 32
	// MaxCandidateIDs is the fixed size of the one policy-store batch every
	// ResolveMentions call issues. A bundle is usable only when no text of
	// MaxMentionTokens tokens can produce more distinct candidates; with the
	// default lexicon limits (8 tokens per term, 8 postings per term) the
	// worst case is 228 spans x 8 = 1824.
	MaxCandidateIDs = 2048
)

// lexiconSentinels pad every authorization batch to MaxCandidateIDs. They use
// lexicon.SentinelIDPrefix, which lexicon.Build refuses for any node, so no
// sentinel is ever a candidate, and a registration the store returns for one
// is never read.
var lexiconSentinels = func() []shoal.ID {
	ids := make([]shoal.ID, MaxCandidateIDs)
	for index := range ids {
		ids[index] = shoal.ID(fmt.Sprintf("%s%04d", lexicon.SentinelIDPrefix, index))
	}
	return ids
}()

func mentionOverBound() error {
	return shoal.NewError(shoal.ErrorInvalidArgument,
		fmt.Sprintf("mention text exceeds %d bytes or %d tokens",
			MaxMentionBytes, MaxMentionTokens))
}

// ResolveMentions links mentions in text to the nodes of b that the caller may
// currently see, under the neighborhood operation.
//
// Every candidate the bundle reports is checked against the caller's current
// node rules before any choice is made, and an allowed document-section node
// must also pass the canonical-revision check Neighborhood applies to a seed. A node the caller may not see is
// dropped silently, exactly as a node absent from the catalog is, so a hidden
// entity matches as nothing: the result, the error, and the policy-store
// traffic are the same as for an unknown name. Selection (leftmost-longest,
// ambiguity) runs only over visible nodes.
//
// The store is read in exactly one Nodes call of exactly MaxCandidateIDs
// identifiers, padded with sentinels, whatever the text and whatever is
// hidden. Errors come only from the caller's own authorization, the input
// bound (one generic error for every caller), a bundle too large to bound, or
// the policy store.
func (c *Client) ResolveMentions(
	ctx context.Context,
	b *lexicon.Bundle,
	text string,
) ([]lexicon.Mention, error) {
	if b == nil {
		return nil, dependencyRequired("lexicon bundle")
	}
	decision, guard, now, err := c.begin(ctx, auth.OperationNeighborhood)
	if err != nil {
		return nil, err
	}
	if b.CandidateBound(MaxMentionTokens) > MaxCandidateIDs {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument,
			"lexicon bundle exceeds the mention candidate bound")
	}
	if len(text) > MaxMentionBytes {
		return nil, mentionOverBound()
	}
	tokens := lexicon.Tokenize(text)
	if len(tokens) > MaxMentionTokens {
		return nil, mentionOverBound()
	}
	candidates := b.CandidatesForTokens(tokens)

	distinct := make(map[shoal.ID]struct{})
	for _, candidate := range candidates {
		for _, id := range candidate.NodeIDs {
			distinct[id] = struct{}{}
		}
	}
	if len(distinct) > MaxCandidateIDs {
		// Unreachable: CandidateBound was checked above. Failing closed with
		// the store untouched is still the only safe answer.
		return nil, inconsistentBase()
	}
	batch := make([]shoal.ID, 0, MaxCandidateIDs)
	for id := range distinct {
		batch = append(batch, id)
	}
	sort.Slice(batch, func(i, j int) bool { return batch[i] < batch[j] })
	batch = append(batch, lexiconSentinels[len(batch):]...)

	registrations, err := c.policyStore.Nodes(ctx, batch)
	if err != nil {
		return nil, policyCatalogReadError(ctx, err)
	}
	registrations, err = c.effectiveNodeRegistrations(ctx, registrations,
		readerFor(decision, auth.OperationNeighborhood, now))
	if err != nil {
		return nil, err
	}
	allowed, err := allowedRegistrations(
		registrations, batch[:len(distinct)], decision, now)
	if err != nil {
		return nil, err
	}
	allowed, err = c.dropNonCanonical(ctx, allowed)
	if err != nil {
		return nil, err
	}
	mentions := lexicon.Select(candidates, func(id shoal.ID) bool {
		_, ok := allowed[id]
		return ok
	})
	if err := guard.Check(ctx); err != nil {
		return nil, err
	}
	return mentions, nil
}

// allowedRegistrations keeps the registrations among ids whose current rule
// allows the neighborhood operation. Registrations for other identifiers
// (sentinels included) are never read.
func allowedRegistrations(
	registrations map[shoal.ID]NodeRegistration,
	ids []shoal.ID,
	decision auth.Decision,
	now time.Time,
) (map[shoal.ID]NodeRegistration, error) {
	allowed := make(map[shoal.ID]NodeRegistration)
	for _, id := range ids {
		registration, ok := registrations[id]
		if !ok {
			continue
		}
		ok, err := ruleAllows(registration.Rule, decision, auth.OperationNeighborhood, now)
		if err != nil {
			return nil, err
		}
		if ok {
			allowed[id] = registration
		}
	}
	return allowed, nil
}

// dropNonCanonical applies the check authorizedNode applies to a
// document-section node: its catalog revision must still be the base's
// current revision. A node failing it is dropped, as a stale seed is not
// found by Neighborhood. Only nodes the caller may already see reach this
// check, so the base reads it makes depend only on what the caller can see,
// never on hidden or unknown names. Registered graph nodes need no base read.
func (c *Client) dropNonCanonical(
	ctx context.Context,
	allowed map[shoal.ID]NodeRegistration,
) (map[shoal.ID]NodeRegistration, error) {
	var documentNodes []shoal.ID
	for id, registration := range allowed {
		if registration.Node.ID == "" {
			documentNodes = append(documentNodes, id)
		}
	}
	if len(documentNodes) == 0 {
		return allowed, nil
	}
	sort.Slice(documentNodes, func(i, j int) bool {
		return documentNodes[i] < documentNodes[j]
	})
	indexed, err := c.withCanonicalDocumentIndex(ctx)
	if err != nil {
		return nil, err
	}
	canonical := make(map[shoal.ID]*canonicalRetrievalDocument)
	for _, id := range documentNodes {
		_, err := c.canonicalRegisteredNodesCached(
			indexed, map[shoal.ID]NodeRegistration{id: allowed[id]}, canonical)
		if shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			delete(allowed, id)
			continue
		}
		if err != nil {
			return nil, err
		}
	}
	return allowed, nil
}

// lexiconScopeDigest names one authorization scope for a pinned bundle: the
// caller's exact authorization fingerprint, its policy generation and the
// snapshot. Two callers with different grants, or one caller at another
// generation or snapshot, get different digests.
func lexiconScopeDigest(
	decision auth.Decision,
	snapshot lexicon.Snapshot,
) ([32]byte, error) {
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		return [32]byte{}, authorizationDenied()
	}
	hash := sha256.New()
	writeString := func(value string) {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		hash.Write(length[:])
		hash.Write([]byte(value))
	}
	writeUint := func(value uint64) {
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], value)
		hash.Write(buf[:])
	}
	writeString("shoal-lexicon-scope-v1")
	hash.Write(fingerprint[:])
	// The fingerprint already covers the generation; it is written again so
	// the digest states its inputs on its own.
	writeUint(uint64(decision.PolicyGeneration()))
	writeString(snapshot.ID)
	writeUint(snapshot.Frontier)
	writeUint(uint64(snapshot.AsOf.UTC().UnixNano()))
	var digest [32]byte
	copy(digest[:], hash.Sum(nil))
	return digest, nil
}

// LexiconScopeNodes filters a builder's node set to the nodes the caller may
// currently see under the neighborhood operation, and seals the result as
// lexicon.ScopedNodes: the only input from which a pinned, shippable bundle
// can be built. Its scope digest is computed from the caller's authorization
// fingerprint, policy generation and snapshot, so it is never a caller's
// claim, and two callers' scoped bundles carry different scopes.
//
// Visibility is the same as for ResolveMentions: the current node rule, and
// for a document-section node the canonical-revision check. The catalog is
// read in one batch; only the node data itself is the builder's.
func (c *Client) LexiconScopeNodes(
	ctx context.Context,
	snapshot lexicon.Snapshot,
	nodes []graph.Node,
) (lexicon.ScopedNodes, error) {
	decision, guard, now, err := c.begin(ctx, auth.OperationNeighborhood)
	if err != nil {
		return lexicon.ScopedNodes{}, err
	}
	if snapshot.ID == "" || snapshot.AsOf.IsZero() {
		return lexicon.ScopedNodes{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "lexicon scope snapshot is required")
	}
	ids := make([]shoal.ID, len(nodes))
	for index, node := range nodes {
		if err := shoal.ValidateRequiredID("graph node ID", node.ID); err != nil {
			return lexicon.ScopedNodes{}, err
		}
		ids[index] = node.ID
	}
	resolved, err := c.resolveNodes(
		ctx, ids, readerFor(decision, auth.OperationNeighborhood, now))
	if err != nil {
		return lexicon.ScopedNodes{}, err
	}
	allowed, err := allowedRegistrations(resolved, ids, decision, now)
	if err != nil {
		return lexicon.ScopedNodes{}, err
	}
	allowed, err = c.dropNonCanonical(ctx, allowed)
	if err != nil {
		return lexicon.ScopedNodes{}, err
	}
	var visible []graph.Node
	for _, node := range nodes {
		if _, ok := allowed[node.ID]; ok {
			visible = append(visible, node)
		}
	}
	digest, err := lexiconScopeDigest(decision, snapshot)
	if err != nil {
		return lexicon.ScopedNodes{}, err
	}
	if err := guard.Check(ctx); err != nil {
		return lexicon.ScopedNodes{}, err
	}
	scoped, ok := lexiconscope.Seal(
		visible, snapshot.ID, snapshot.AsOf, snapshot.Frontier, digest,
	).(lexicon.ScopedNodes)
	if !ok {
		return lexicon.ScopedNodes{}, inconsistentBase()
	}
	return scoped, nil
}
