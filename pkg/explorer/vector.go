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

package explorer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/model"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	maxExplorerEmbeddingDimensions = 16 * 1024
	maxExplorerEmbeddingBytes      = 64 * 1024 * 1024
	maxExplorerEmbeddingSpans      = 4096
	vectorAvailabilityTTL          = time.Minute
	vectorCapabilityProbeText      = "shoal vector capability probe"
)

type embeddingQueryCacheKey struct {
	identity string
	query    string
}

type cachedQueryEmbedding struct {
	provenance persistedEmbeddingProvenance
	vector     []float32
}

type vectorAvailabilityCache struct {
	checkedAt time.Time
	available bool
}

type embeddingSpaceCache struct {
	provenance persistedEmbeddingProvenance
	found      bool
}

// VectorScoreRequest asks the embedded Explorer to recompute vector scores for
// exact citations already selected by a trusted wrapper.
type VectorScoreRequest struct {
	Text      string
	Citations []document.Citation
}

// VectorAvailable reports whether the current embedded corpus can serve vector
// retrieval without silently ignoring stale or missing embeddings.
func (e *Explorer) VectorAvailable(ctx context.Context) (bool, error) {
	if err := contextError(ctx); err != nil {
		return false, err
	}
	now := time.Now().UTC()
	e.mu.RLock()
	if err := e.requireOpen(); err != nil {
		e.mu.RUnlock()
		return false, err
	}
	if !e.vectorAvailability.checkedAt.IsZero() &&
		now.Sub(e.vectorAvailability.checkedAt) < vectorAvailabilityTTL {
		available := e.vectorAvailability.available
		e.mu.RUnlock()
		return available, nil
	}
	if e.embedder == nil {
		e.mu.RUnlock()
		if err := e.cacheVectorAvailability(false, now); err != nil {
			return false, err
		}
		return false, nil
	}
	e.mu.RUnlock()

	e.vectorProbeMu.Lock()
	defer e.vectorProbeMu.Unlock()

	now = time.Now().UTC()
	e.mu.RLock()
	if err := e.requireOpen(); err != nil {
		e.mu.RUnlock()
		return false, err
	}
	if !e.vectorAvailability.checkedAt.IsZero() &&
		now.Sub(e.vectorAvailability.checkedAt) < vectorAvailabilityTTL {
		available := e.vectorAvailability.available
		e.mu.RUnlock()
		return available, nil
	}
	if e.embedder == nil {
		e.mu.RUnlock()
		if err := e.cacheVectorAvailability(false, now); err != nil {
			return false, err
		}
		return false, nil
	}
	spaces, corpusReady, err := e.vectorAvailabilitySnapshotLocked()
	if err != nil {
		e.mu.RUnlock()
		return false, err
	}
	e.mu.RUnlock()

	if !corpusReady {
		if err := e.cacheVectorAvailability(false, now); err != nil {
			return false, err
		}
		return false, nil
	}
	available := true
	if len(spaces) == 0 {
		_, _, err = e.embedQuery(ctx, vectorCapabilityProbeText)
		if err != nil {
			if shoal.IsErrorCode(err, shoal.ErrorCanceled) ||
				shoal.IsErrorCode(err, shoal.ErrorDeadline) {
				return false, err
			}
			available = false
		}
	} else if len(spaces) > e.maxEmbeddingSpaceFanout {
		available = false
	} else {
		for _, space := range spaces {
			_, _, _, err := e.embedQueryInSpace(ctx, vectorCapabilityProbeText, space)
			if err != nil {
				if shoal.IsErrorCode(err, shoal.ErrorCanceled) ||
					shoal.IsErrorCode(err, shoal.ErrorDeadline) {
					return false, err
				}
				available = false
				break
			}
		}
	}
	if err := e.cacheVectorAvailability(available, now); err != nil {
		return false, err
	}
	return available, nil
}

func (e *Explorer) vectorAvailabilitySnapshotLocked() (
	[]persistedEmbeddingProvenance, bool, error,
) {
	corpusReady := true
	spacesByIdentity := map[string]persistedEmbeddingProvenance{}
	for _, revisions := range e.documents {
		for _, record := range revisions {
			if record == nil || record.Embeddings == nil {
				continue
			}
			if err := validateEmbeddingSet(record); err != nil {
				return nil, false, err
			}
			current := record.Embeddings.Provenance
			if existing, ok := spacesByIdentity[current.Identity]; ok &&
				existing != current {
				return nil, false, conflictingEmbeddingProvenanceError(
					existing, current)
			}
			spacesByIdentity[current.Identity] = current
		}
		record, err := latestRevision(revisions)
		if err != nil {
			return nil, false, err
		}
		if record == nil || len(record.Spans) == 0 {
			continue
		}
		if _, err := recordEmbeddingMap(record); err != nil {
			corpusReady = false
			break
		}
	}
	spaces := make([]persistedEmbeddingProvenance, 0, len(spacesByIdentity))
	for _, space := range spacesByIdentity {
		spaces = append(spaces, space)
	}
	sort.Slice(spaces, func(i, j int) bool {
		return spaces[i].Identity < spaces[j].Identity
	})
	return spaces, corpusReady, nil
}

// VectorScores recomputes vector scores for exact stored span citations. It is
// intentionally citation-addressed so authorization wrappers can project first
// and then verify ranking without receiving raw embeddings.
func (e *Explorer) VectorScores(
	ctx context.Context,
	request VectorScoreRequest,
) (map[shoal.ID]shoal.Score, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	for _, citation := range request.Citations {
		if err := citation.Validate(); err != nil {
			return nil, err
		}
	}
	e.mu.RLock()
	if err := e.requireOpen(); err != nil {
		e.mu.RUnlock()
		return nil, err
	}
	if len(request.Citations) == 0 {
		e.mu.RUnlock()
		_, _, event, err := e.embedQueriesForSpaces(ctx, request.Text, nil)
		e.observeEmbeddingQuery(ctx, event)
		if err != nil {
			return nil, err
		}
		return map[shoal.ID]shoal.Score{}, nil
	}
	var queryEvent EmbeddingQueryEvent
	var observeQuery bool
	defer func() {
		if observeQuery {
			e.observeEmbeddingQuery(ctx, queryEvent)
		}
	}()
	defer e.mu.RUnlock()
	type scoreTarget struct {
		span      document.Span
		embedding persistedSpanEmbedding
		space     persistedEmbeddingProvenance
	}
	embeddingMaps := make(
		map[documentRevisionKey]map[shoal.ID]persistedSpanEmbedding)
	spanMaps := make(map[documentRevisionKey]map[shoal.ID]document.Span)
	targets := make([]scoreTarget, 0, len(request.Citations))
	spacesByIdentity := map[string]persistedEmbeddingProvenance{}
	var err error
	for _, citation := range request.Citations {
		key := documentRevisionKey{
			documentID: citation.DocumentID,
			revisionID: citation.RevisionID,
		}
		record := e.documents[key.documentID][key.revisionID]
		if record == nil {
			return nil, shoal.NewError(
				shoal.ErrorNotFound, "vector score citation revision not found")
		}
		embeddings := embeddingMaps[key]
		if embeddings == nil {
			if record.Embeddings == nil {
				return nil, shoal.NewError(
					shoal.ErrorUnavailable,
					"vector scoring requires embeddings for every cited span",
				)
			}
			embeddings, err = recordEmbeddingMap(record)
			if err != nil {
				return nil, err
			}
			embeddingMaps[key] = embeddings
		}
		spans := spanMaps[key]
		if spans == nil {
			spans = make(map[shoal.ID]document.Span, len(record.Spans))
			for _, span := range record.Spans {
				spans[span.ID] = span
			}
			spanMaps[key] = spans
		}
		span, ok := spans[citation.SpanID]
		if !ok || span.SectionID != citation.SectionID ||
			span.Range != citation.Range {
			return nil, shoal.NewError(
				shoal.ErrorNotFound, "vector score citation span not found")
		}
		embedding, ok := embeddings[span.ID]
		if !ok || !embeddingMatchesSpan(embedding, span) {
			return nil, shoal.NewError(
				shoal.ErrorUnavailable,
				"stored span embeddings are stale or incomplete",
			)
		}
		current := record.Embeddings.Provenance
		if existing, ok := spacesByIdentity[current.Identity]; ok &&
			existing != current {
			return nil, conflictingEmbeddingProvenanceError(existing, current)
		}
		spacesByIdentity[current.Identity] = current
		targets = append(targets, scoreTarget{
			span: span, embedding: embedding, space: record.Embeddings.Provenance,
		})
	}
	queryVectors, _, event, err := e.embedQueriesForSpaces(
		ctx, request.Text, spacesByIdentity)
	queryEvent = event
	observeQuery = true
	if err != nil {
		return nil, err
	}
	scores := make(map[shoal.ID]shoal.Score, len(targets))
	rawBySpace := make(map[string][]rankedSpan)
	for _, target := range targets {
		score, err := vectorScore(queryVectors[target.space.Identity], target.embedding.Vector)
		if err != nil {
			return nil, err
		}
		if len(spacesByIdentity) > 1 {
			rawBySpace[target.space.Identity] = append(rawBySpace[target.space.Identity], rankedSpan{
				span:  target.span,
				score: score,
				space: target.space.Identity,
			})
		} else {
			scores[target.span.ID] = score
		}
	}
	if len(spacesByIdentity) > 1 {
		fused := make([]rankedSpan, 0, len(targets))
		for _, group := range rawBySpace {
			fused = append(fused, group...)
		}
		applyRankFusion(fused)
		for _, item := range fused {
			scores[item.span.ID] = item.score
		}
	}
	participating := make(map[string]struct{})
	for _, target := range targets {
		participating[target.space.Identity] = struct{}{}
	}
	queryEvent.Participating = make([]string, 0, len(participating))
	for identity := range participating {
		queryEvent.Participating = append(
			queryEvent.Participating, identity)
	}
	sort.Strings(queryEvent.Participating)
	return scores, nil
}

// VectorEmbeddingSpaceIDs returns the canonical embedding-space constituents
// used by VectorScores for the same trusted request.
func (e *Explorer) VectorEmbeddingSpaceIDs(
	ctx context.Context,
	request VectorScoreRequest,
) ([]shoal.ID, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	for _, citation := range request.Citations {
		if err := citation.Validate(); err != nil {
			return nil, err
		}
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	if err := e.requireOpen(); err != nil {
		return nil, err
	}
	if len(request.Citations) == 0 {
		return nil, nil
	}
	spaces := make(map[shoal.ID]struct{})
	embeddingMaps := make(
		map[documentRevisionKey]map[shoal.ID]persistedSpanEmbedding)
	spanMaps := make(map[documentRevisionKey]map[shoal.ID]document.Span)
	for _, citation := range request.Citations {
		key := documentRevisionKey{
			documentID: citation.DocumentID,
			revisionID: citation.RevisionID,
		}
		record := e.documents[key.documentID][key.revisionID]
		if record == nil || record.Embeddings == nil {
			return nil, shoal.NewError(
				shoal.ErrorUnavailable,
				"vector scoring requires embeddings for every cited span",
			)
		}
		embeddings := embeddingMaps[key]
		if embeddings == nil {
			var err error
			embeddings, err = recordEmbeddingMap(record)
			if err != nil {
				return nil, err
			}
			embeddingMaps[key] = embeddings
		}
		spans := spanMaps[key]
		if spans == nil {
			spans = make(map[shoal.ID]document.Span, len(record.Spans))
			for _, span := range record.Spans {
				spans[span.ID] = span
			}
			spanMaps[key] = spans
		}
		span, ok := spans[citation.SpanID]
		embedding, embedded := embeddings[citation.SpanID]
		if !ok || !embedded || span.SectionID != citation.SectionID ||
			span.Range != citation.Range || !embeddingMatchesSpan(embedding, span) {
			return nil, shoal.NewError(
				shoal.ErrorNotFound, "vector score citation span not found")
		}
		id, err := retrieval.EmbeddingSpaceIdentityID(
			record.Embeddings.Provenance.Identity)
		if err != nil {
			return nil, err
		}
		spaces[id] = struct{}{}
	}
	ids := make([]shoal.ID, 0, len(spaces))
	for id := range spaces {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		return shoal.CompareID(ids[i], ids[j]) < 0
	})
	return ids, nil
}

func (e *Explorer) cacheVectorAvailability(
	available bool,
	checkedAt time.Time,
) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return shoal.NewError(shoal.ErrorUnavailable, "explorer is closed")
	}
	e.vectorAvailability = vectorAvailabilityCache{
		checkedAt: checkedAt,
		available: available,
	}
	return nil
}

func (e *Explorer) invalidateVectorAvailabilityLocked() {
	e.vectorAvailability = vectorAvailabilityCache{}
}

func (e *Explorer) embedParsedSpans(
	ctx context.Context, spans []document.Span,
) (*persistedEmbeddingSet, error) {
	if e.embedder == nil || len(spans) == 0 {
		return nil, nil
	}
	if len(spans) > maxExplorerEmbeddingSpans {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"source has too many spans for embedded vector indexing",
		)
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	identity, err := e.embeddingIdentity()
	if err != nil {
		return nil, err
	}
	first, provenance, err := e.embedSpan(ctx, spans[0], identity)
	if err != nil {
		return nil, err
	}
	if err := validateEmbeddingAggregateBound(
		len(spans), provenance.Dimensions,
	); err != nil {
		return nil, err
	}
	e.mu.RLock()
	if err := e.ensureEmbeddingSpaceCompatibleLocked(provenance); err != nil {
		e.mu.RUnlock()
		return nil, err
	}
	e.mu.RUnlock()
	embedded := &persistedEmbeddingSet{
		Provenance: provenance,
		Spans:      make([]persistedSpanEmbedding, len(spans)),
	}
	embedded.Spans[0] = first
	for index := 1; index < len(spans); index++ {
		embedding, current, err := e.embedSpan(ctx, spans[index], identity)
		if err != nil {
			return nil, err
		}
		if current != provenance {
			return nil, shoal.NewError(
				shoal.ErrorConflict,
				"embedding provider returned incompatible provenance within one revision",
			)
		}
		embedded.Spans[index] = embedding
	}
	return embedded, nil
}

func (e *Explorer) embedSpan(
	ctx context.Context,
	span document.Span,
	identity string,
) (persistedSpanEmbedding, persistedEmbeddingProvenance, error) {
	result, err := e.embedder.Embed(ctx, model.EmbedRequest{Text: span.Text})
	if err != nil {
		return persistedSpanEmbedding{}, persistedEmbeddingProvenance{},
			embeddingProviderError("embed source span", err)
	}
	provenance, vector, err := normalizedEmbeddingResult(result, identity)
	if err != nil {
		return persistedSpanEmbedding{}, persistedEmbeddingProvenance{}, err
	}
	return persistedSpanEmbedding{
		SpanID:     span.ID,
		TextDigest: spanTextDigest(span),
		Range:      span.Range,
		Vector:     vector,
	}, provenance, nil
}

func (e *Explorer) embedQuery(
	ctx context.Context, text string,
) (persistedEmbeddingProvenance, []float32, error) {
	if e.embedder == nil {
		return persistedEmbeddingProvenance{}, nil, shoal.NewError(
			shoal.ErrorUnavailable,
			"vector retrieval is not configured for the embedded Explorer",
		)
	}
	identity, err := e.embeddingIdentity()
	if err != nil {
		return persistedEmbeddingProvenance{}, nil, err
	}
	result, err := e.embedder.Embed(ctx, model.EmbedRequest{Text: text})
	if err != nil {
		return persistedEmbeddingProvenance{}, nil,
			embeddingProviderError("embed retrieval query", err)
	}
	provenance, vector, err := normalizedEmbeddingResult(result, identity)
	if err != nil {
		return persistedEmbeddingProvenance{}, nil, err
	}
	return provenance, vector, nil
}

func (e *Explorer) embedQueryInSpace(
	ctx context.Context,
	text string,
	space persistedEmbeddingProvenance,
) (persistedEmbeddingProvenance, []float32, bool, error) {
	if err := validateEmbeddingProvenance(space); err != nil {
		return persistedEmbeddingProvenance{}, nil, false, err
	}
	embedder := e.embedders[space.Identity]
	if embedder == nil {
		if e.embedder != nil {
			currentIdentity, err := e.embeddingIdentity()
			if err != nil {
				return persistedEmbeddingProvenance{}, nil, false, err
			}
			if currentIdentity != space.Identity {
				return persistedEmbeddingProvenance{}, nil, false,
					incompatibleEmbeddingSpaceError(
						space,
						persistedEmbeddingProvenance{Identity: currentIdentity},
					)
			}
		}
		return persistedEmbeddingProvenance{}, nil, false, shoal.NewError(
			shoal.ErrorUnavailable,
			fmt.Sprintf("embedding provider for stored space %q is unavailable", space.Identity),
		)
	}
	identity, err := embeddingIdentityFor(embedder)
	if err != nil {
		return persistedEmbeddingProvenance{}, nil, false, err
	}
	if identity != space.Identity {
		return persistedEmbeddingProvenance{}, nil, false,
			incompatibleEmbeddingSpaceError(space, persistedEmbeddingProvenance{
				Provider: space.Provider, Model: space.Model,
				Identity: identity, Dimensions: space.Dimensions,
			})
	}
	result, err := embedder.Embed(ctx, model.EmbedRequest{Text: text})
	if err != nil {
		return persistedEmbeddingProvenance{}, nil, true,
			embeddingProviderError(
				fmt.Sprintf("embed retrieval query for space %q", space.Identity), err)
	}
	provenance, vector, err := normalizedEmbeddingResult(result, identity)
	if err != nil {
		return persistedEmbeddingProvenance{}, nil, true, err
	}
	if provenance != space {
		return persistedEmbeddingProvenance{}, nil, true,
			incompatibleEmbeddingSpaceError(space, provenance)
	}
	return provenance, vector, true, nil
}

func (e *Explorer) embedQueriesForSpaces(
	ctx context.Context,
	text string,
	spaces map[string]persistedEmbeddingProvenance,
) (
	map[string][]float32,
	[]persistedEmbeddingProvenance,
	EmbeddingQueryEvent,
	error,
) {
	keys := make([]string, 0, len(spaces))
	for identity := range spaces {
		keys = append(keys, identity)
	}
	sort.Strings(keys)
	event := EmbeddingQueryEvent{
		SpaceIdentities: append([]string(nil), keys...),
		FanoutLimit:     e.maxEmbeddingSpaceFanout,
	}
	if len(keys) > e.maxEmbeddingSpaceFanout {
		event.FanoutExceeded = true
		return nil, nil, event, shoal.NewError(
			shoal.ErrorUnavailable,
			fmt.Sprintf(
				"vector retrieval spans %d embedding spaces, exceeding fan-out limit %d",
				len(keys), e.maxEmbeddingSpaceFanout),
		)
	}

	if len(keys) == 0 {
		identity, identityErr := e.embeddingIdentity()
		if identityErr == nil {
			event.SpaceIdentities = []string{identity}
			event.Attempted = append(event.Attempted, identity)
		}
		provenance, _, cacheHit, providerCalled, err :=
			e.cachedEmbedQuery(ctx, text)
		if provenance.Identity != "" {
			event.SpaceIdentities = []string{provenance.Identity}
			if len(event.Attempted) == 0 {
				event.Attempted = append(event.Attempted, provenance.Identity)
			}
		}
		if cacheHit {
			event.CacheHits++
		}
		if providerCalled {
			event.ProviderCalls++
		}
		if err != nil {
			if identityErr == nil {
				event.SpaceIdentities = []string{identity}
				if shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
					event.Unavailable = []string{identity}
				}
			}
			return nil, nil, event, err
		}
		if provenance.Identity != "" {
			event.Completed = append(event.Completed, provenance.Identity)
		}
		return map[string][]float32{}, nil, event, nil
	}

	vectors := make(map[string][]float32, len(keys))
	participating := make([]persistedEmbeddingProvenance, 0, len(keys))
	for _, identity := range keys {
		space := spaces[identity]
		event.Attempted = append(event.Attempted, identity)
		_, vector, cacheHit, providerCalled, err :=
			e.cachedEmbedQueryInSpace(ctx, text, space)
		if cacheHit {
			event.CacheHits++
		}
		if providerCalled {
			event.ProviderCalls++
		}
		if err != nil {
			if shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
				event.Unavailable = append(event.Unavailable, identity)
			}
			return nil, nil, event, err
		}
		event.Completed = append(event.Completed, identity)
		vectors[identity] = vector
		participating = append(participating, space)
	}
	return vectors, participating, event, nil
}

func (e *Explorer) cachedEmbedQuery(
	ctx context.Context,
	text string,
) (persistedEmbeddingProvenance, []float32, bool, bool, error) {
	if e.embedder == nil {
		provenance, vector, err := e.embedQuery(ctx, normalizeEmbeddingQueryText(text))
		return provenance, vector, false, false, err
	}
	identity, err := e.embeddingIdentity()
	if err != nil {
		return persistedEmbeddingProvenance{}, nil, false, false, err
	}
	query := normalizeEmbeddingQueryText(text)
	if cached, ok := e.queryEmbedding(identity, query); ok {
		return cached.provenance, cached.vector, true, false, nil
	}
	provenance, vector, err := e.embedQuery(ctx, query)
	if err != nil {
		return persistedEmbeddingProvenance{}, nil, false, true, err
	}
	e.cacheQueryEmbedding(identity, query, provenance, vector)
	return provenance, vector, false, true, nil
}

func (e *Explorer) cachedEmbedQueryInSpace(
	ctx context.Context,
	text string,
	space persistedEmbeddingProvenance,
) (persistedEmbeddingProvenance, []float32, bool, bool, error) {
	if err := validateEmbeddingProvenance(space); err != nil {
		return persistedEmbeddingProvenance{}, nil, false, false, err
	}
	query := normalizeEmbeddingQueryText(text)
	if cached, ok := e.queryEmbedding(space.Identity, query); ok {
		if cached.provenance != space {
			return persistedEmbeddingProvenance{}, nil, true, false,
				conflictingEmbeddingProvenanceError(
					cached.provenance, space)
		}
		return cached.provenance, cached.vector, true, false, nil
	}
	provenance, vector, providerCalled, err :=
		e.embedQueryInSpace(ctx, query, space)
	if err != nil {
		return persistedEmbeddingProvenance{}, nil, false, providerCalled, err
	}
	e.cacheQueryEmbedding(space.Identity, query, provenance, vector)
	return provenance, vector, false, providerCalled, nil
}

func normalizeEmbeddingQueryText(text string) string {
	return strings.TrimSpace(text)
}

func (e *Explorer) queryEmbedding(
	identity, query string,
) (cachedQueryEmbedding, bool) {
	e.queryEmbeddingMu.Lock()
	defer e.queryEmbeddingMu.Unlock()
	cached, ok := e.queryEmbeddingCache[embeddingQueryCacheKey{
		identity: identity,
		query:    query,
	}]
	if !ok {
		return cachedQueryEmbedding{}, false
	}
	cached.vector = append([]float32(nil), cached.vector...)
	return cached, true
}

func (e *Explorer) cacheQueryEmbedding(
	identity, query string,
	provenance persistedEmbeddingProvenance,
	vector []float32,
) {
	key := embeddingQueryCacheKey{identity: identity, query: query}
	e.queryEmbeddingMu.Lock()
	defer e.queryEmbeddingMu.Unlock()
	if _, exists := e.queryEmbeddingCache[key]; exists {
		return
	}
	for len(e.queryEmbeddingOrder) >= e.maxQueryEmbeddingCache {
		evicted := e.queryEmbeddingOrder[0]
		e.queryEmbeddingOrder = e.queryEmbeddingOrder[1:]
		delete(e.queryEmbeddingCache, evicted)
	}
	e.queryEmbeddingCache[key] = cachedQueryEmbedding{
		provenance: provenance,
		vector:     append([]float32(nil), vector...),
	}
	e.queryEmbeddingOrder = append(e.queryEmbeddingOrder, key)
}

func (e *Explorer) observeEmbeddingQuery(
	ctx context.Context,
	event EmbeddingQueryEvent,
) {
	ReportEmbeddingQueryEvent(ctx, event)
	if e.embeddingQueryObserver != nil {
		e.embeddingQueryObserver(cloneEmbeddingQueryEvent(event))
	}
}

func conflictingEmbeddingProvenanceError(
	existing, incoming persistedEmbeddingProvenance,
) error {
	return shoal.WrapError(
		shoal.ErrorInternal,
		fmt.Sprintf(
			"embedding identity %q has conflicting persisted provenance",
			existing.Identity,
		),
		fmt.Errorf(
			"%w: identity %q maps to %s/%s/%d and %s/%s/%d",
			embeddingspace.ErrIntegrity,
			existing.Identity,
			existing.Provider, existing.Model, existing.Dimensions,
			incoming.Provider, incoming.Model, incoming.Dimensions,
		),
	)
}

func embeddingProviderMap(options Options) (map[string]model.Embedder, error) {
	providers := append([]model.Embedder(nil), options.EmbeddingProviders...)
	if options.Embedder != nil {
		providers = append([]model.Embedder{options.Embedder}, providers...)
	}
	if len(providers) == 0 {
		return nil, nil
	}
	byIdentity := make(map[string]model.Embedder, len(providers))
	for _, provider := range providers {
		if provider == nil {
			continue
		}
		identity, err := embeddingIdentityFor(provider)
		if err != nil {
			return nil, err
		}
		if existing := byIdentity[identity]; existing != nil && existing != provider {
			return nil, shoal.NewError(
				shoal.ErrorInvalidArgument,
				"duplicate embedding provider identity",
			)
		}
		byIdentity[identity] = provider
	}
	return byIdentity, nil
}

func normalizedEmbeddingResult(
	result model.EmbedResult, identity string,
) (persistedEmbeddingProvenance, []float32, error) {
	provider := strings.TrimSpace(result.Provenance.Provider)
	modelName := strings.TrimSpace(result.Provenance.Model)
	if provider == "" || modelName == "" ||
		!utf8.ValidString(provider) || !utf8.ValidString(modelName) ||
		len(provider) > shoal.MaxSemanticStringBytes ||
		len(modelName) > shoal.MaxSemanticStringBytes {
		return persistedEmbeddingProvenance{}, nil, shoal.NewError(
			shoal.ErrorUnavailable,
			"embedding provider did not return stable provenance",
		)
	}
	if len(result.Vector) == 0 ||
		len(result.Vector) > maxExplorerEmbeddingDimensions {
		return persistedEmbeddingProvenance{}, nil, shoal.NewError(
			shoal.ErrorUnavailable,
			"embedding provider returned an unsupported vector dimension",
		)
	}
	vector := append([]float32(nil), result.Vector...)
	var norm float64
	for _, value := range vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return persistedEmbeddingProvenance{}, nil, shoal.NewError(
				shoal.ErrorUnavailable,
				"embedding provider returned a non-finite vector value",
			)
		}
		norm += float64(value) * float64(value)
	}
	if norm == 0 {
		return persistedEmbeddingProvenance{}, nil, shoal.NewError(
			shoal.ErrorUnavailable,
			"embedding provider returned a zero vector",
		)
	}
	return persistedEmbeddingProvenance{
		Provider: provider, Model: modelName,
		Identity: identity, Dimensions: len(vector),
	}, vector, nil
}

func (e *Explorer) embeddingIdentity() (string, error) {
	return embeddingIdentityFor(e.embedder)
}

func embeddingIdentityFor(embedder model.Embedder) (string, error) {
	provider, ok := embedder.(model.EmbeddingSpaceIdentityProvider)
	if !ok {
		return "", shoal.NewError(
			shoal.ErrorInvalidArgument,
			"embedding provider must expose a stable embedding space identity",
		)
	}
	identity, err := provider.EmbeddingSpaceIdentity()
	if err != nil {
		return "", embeddingProviderError("identify embedding space", err)
	}
	identity = strings.TrimSpace(identity)
	if identity == "" || !utf8.ValidString(identity) ||
		len(identity) > shoal.MaxSemanticStringBytes {
		return "", shoal.NewError(
			shoal.ErrorInvalidArgument,
			"embedding provider identity is invalid",
		)
	}
	return identity, nil
}

func (e *Explorer) ensureEmbeddingSpaceCompatibleLocked(
	provenance persistedEmbeddingProvenance,
) error {
	if err := validateEmbeddingProvenance(provenance); err != nil {
		return err
	}
	existing, ok, err := e.validatedCachedEmbeddingSpaceLocked()
	if err != nil {
		return err
	}
	if ok && existing != provenance {
		return incompatibleEmbeddingSpaceError(existing, provenance)
	}
	return nil
}

func (e *Explorer) validatedCachedEmbeddingSpaceLocked() (
	persistedEmbeddingProvenance, bool, error,
) {
	if !e.embeddingSpace.found {
		return persistedEmbeddingProvenance{}, false, nil
	}
	if err := validateEmbeddingProvenance(e.embeddingSpace.provenance); err != nil {
		return persistedEmbeddingProvenance{}, false, err
	}
	return e.embeddingSpace.provenance, true, nil
}

func (e *Explorer) embeddingSpaceLocked() (
	persistedEmbeddingProvenance, bool, error,
) {
	var space persistedEmbeddingProvenance
	found := false
	for _, revisions := range e.documents {
		for _, record := range revisions {
			if record.Embeddings == nil {
				continue
			}
			if err := validateEmbeddingSet(record); err != nil {
				return persistedEmbeddingProvenance{}, false, err
			}
			current := record.Embeddings.Provenance
			if !found {
				space = current
				found = true
				continue
			}
			if space != current {
				return persistedEmbeddingProvenance{}, false, nil
			}
		}
	}
	return space, found, nil
}

func validateEmbeddingSet(record *persistedDocument) error {
	if record == nil || record.Embeddings == nil {
		return nil
	}
	if err := validateEmbeddingProvenance(record.Embeddings.Provenance); err != nil {
		return err
	}
	if err := validateEmbeddingAggregateBound(
		len(record.Embeddings.Spans),
		record.Embeddings.Provenance.Dimensions,
	); err != nil {
		return shoal.WrapError(
			shoal.ErrorInternal, "stored span embeddings exceed aggregate bound", err)
	}
	seen := make(map[shoal.ID]struct{}, len(record.Embeddings.Spans))
	for _, embedding := range record.Embeddings.Spans {
		if _, duplicate := seen[embedding.SpanID]; duplicate {
			return shoal.NewError(
				shoal.ErrorInternal,
				"stored span embeddings contain duplicate span IDs",
			)
		}
		seen[embedding.SpanID] = struct{}{}
		if embedding.TextDigest == "" || len(embedding.Vector) !=
			record.Embeddings.Provenance.Dimensions {
			return shoal.NewError(
				shoal.ErrorInternal,
				"stored span embedding is incomplete",
			)
		}
		if err := embedding.Range.Validate(); err != nil {
			return shoal.WrapError(
				shoal.ErrorInternal, "stored span embedding range is invalid", err)
		}
		var norm float64
		for _, value := range embedding.Vector {
			if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
				return shoal.NewError(
					shoal.ErrorInternal,
					"stored span embedding contains a non-finite vector value",
				)
			}
			norm += float64(value) * float64(value)
		}
		if norm == 0 {
			return shoal.NewError(
				shoal.ErrorInternal,
				"stored span embedding has zero magnitude",
			)
		}
	}
	return nil
}

func validateEmbeddingProvenance(provenance persistedEmbeddingProvenance) error {
	if strings.TrimSpace(provenance.Provider) == "" ||
		strings.TrimSpace(provenance.Model) == "" ||
		!utf8.ValidString(provenance.Provider) ||
		!utf8.ValidString(provenance.Model) ||
		len(provenance.Provider) > shoal.MaxSemanticStringBytes ||
		len(provenance.Model) > shoal.MaxSemanticStringBytes ||
		strings.TrimSpace(provenance.Identity) == "" ||
		!utf8.ValidString(provenance.Identity) ||
		len(provenance.Identity) > shoal.MaxSemanticStringBytes ||
		provenance.Dimensions <= 0 ||
		provenance.Dimensions > maxExplorerEmbeddingDimensions {
		return shoal.NewError(
			shoal.ErrorInternal, "stored embedding provenance is invalid")
	}
	return nil
}

func validateEmbeddingAggregateBound(spanCount, dimensions int) error {
	if spanCount < 0 || dimensions < 0 {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "embedding aggregate bound is invalid")
	}
	bytes := uint64(spanCount) * uint64(dimensions) * 4
	if bytes > maxExplorerEmbeddingBytes {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"span embeddings exceed the embedded vector index bound",
		)
	}
	return nil
}

func recordEmbeddingMap(
	record *persistedDocument,
) (map[shoal.ID]persistedSpanEmbedding, error) {
	if record == nil || record.Embeddings == nil {
		return nil, shoal.NewError(
			shoal.ErrorUnavailable,
			"vector retrieval requires embeddings for every eligible span",
		)
	}
	if err := validateEmbeddingSet(record); err != nil {
		return nil, err
	}
	bySpan := make(map[shoal.ID]persistedSpanEmbedding, len(record.Embeddings.Spans))
	for _, embedding := range record.Embeddings.Spans {
		bySpan[embedding.SpanID] = embedding
	}
	for _, span := range record.Spans {
		embedding, ok := bySpan[span.ID]
		if !ok || !embeddingMatchesSpan(embedding, span) {
			return nil, shoal.NewError(
				shoal.ErrorUnavailable,
				"stored span embeddings are stale or incomplete",
			)
		}
	}
	return bySpan, nil
}

func embeddingMatchesSpan(embedding persistedSpanEmbedding, span document.Span) bool {
	return embedding.SpanID == span.ID &&
		embedding.TextDigest == spanTextDigest(span) &&
		embedding.Range == span.Range
}

func spanTextDigest(span document.Span) string {
	sum := sha256.Sum256([]byte(span.Text))
	return hex.EncodeToString(sum[:])
}

func vectorScore(query, candidate []float32) (shoal.Score, error) {
	if len(query) == 0 || len(query) != len(candidate) {
		return 0, shoal.NewError(
			shoal.ErrorConflict, "embedding vector dimensions do not match")
	}
	var dot, queryNorm, candidateNorm float64
	for i := range query {
		q, c := float64(query[i]), float64(candidate[i])
		dot += q * c
		queryNorm += q * q
		candidateNorm += c * c
	}
	if queryNorm == 0 || candidateNorm == 0 {
		return 0, shoal.NewError(
			shoal.ErrorUnavailable,
			"embedding vector has zero magnitude",
		)
	}
	cosine := dot / (math.Sqrt(queryNorm) * math.Sqrt(candidateNorm))
	if math.IsNaN(cosine) || math.IsInf(cosine, 0) {
		return 0, shoal.NewError(
			shoal.ErrorUnavailable,
			"embedding vector score is non-finite",
		)
	}
	if cosine > 1 {
		cosine = 1
	}
	if cosine < -1 {
		cosine = -1
	}
	return shoal.Score((cosine + 1) / 2), nil
}

func embeddingProviderError(operation string, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled), errors.Is(err, model.ErrCanceled):
		return shoal.WrapError(shoal.ErrorCanceled, operation, err)
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, model.ErrTimeout):
		return shoal.WrapError(shoal.ErrorDeadline, operation, err)
	case errors.Is(err, model.ErrInvalidConfig), errors.Is(err, model.ErrInvalidRequest):
		return shoal.WrapError(shoal.ErrorInvalidArgument, operation, err)
	default:
		return shoal.WrapError(shoal.ErrorUnavailable, operation, err)
	}
}

func incompatibleEmbeddingSpaceError(
	existing, incoming persistedEmbeddingProvenance,
) error {
	return shoal.WrapError(
		shoal.ErrorConflict,
		fmt.Sprintf(
			"embedding space mismatch: existing %s/%s/%s/%d, incoming %s/%s/%s/%d",
			existing.Provider,
			existing.Model,
			existing.Identity,
			existing.Dimensions,
			incoming.Provider,
			incoming.Model,
			incoming.Identity,
			incoming.Dimensions,
		),
		&embeddingspace.MismatchError{
			Operation: "compare embedding spaces",
			Left:      embeddingspace.Has(existing.Identity),
			Right:     embeddingspace.Has(incoming.Identity),
		},
	)
}
