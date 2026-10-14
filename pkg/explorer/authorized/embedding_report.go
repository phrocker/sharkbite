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
	"bytes"
	"context"
	"math"
	"sort"
	"sync"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// EmbeddingSpaceStatus is the bounded outcome for one authorized candidate
// space. The identifier beside it is a process-keyed opaque pseudonym, never
// the persisted provider/model identity.
type EmbeddingSpaceStatus string

const (
	EmbeddingSpaceAvailable    EmbeddingSpaceStatus = "available"
	EmbeddingSpaceUnavailable  EmbeddingSpaceStatus = "unavailable"
	EmbeddingSpaceNotAttempted EmbeddingSpaceStatus = "not_attempted"
	EmbeddingSpaceNotCompleted EmbeddingSpaceStatus = "not_completed"
)

// EmbeddingSpaceReport describes one distinct authorized candidate space.
type EmbeddingSpaceReport struct {
	ID     shoal.ID
	Status EmbeddingSpaceStatus
}

// EmbeddingQueryReport is the request-local authorized projection of core
// embedding observability. Spaces contains only candidate spaces reached after
// source authorization and mosaic restriction. Suppressed and Restricted are
// booleans derived from the separately reported, explicitly permitted
// document counts; they never identify a withheld space.
type EmbeddingQueryReport struct {
	Spaces         []EmbeddingSpaceReport
	FanoutLimit    uint32
	CacheHits      uint32
	ProviderCalls  uint32
	Observed       bool
	Suppressed     bool
	Restricted     bool
	Degraded       bool
	FanoutExceeded bool
}

// RetrievalReport combines the existing document-level disclosure with vector
// space reporting. Embedding is nil for non-vector retrieval.
type RetrievalReport struct {
	Disclosure Disclosure
	Embedding  *EmbeddingQueryReport
}

// EmbeddingQueryObserver receives one finalized authorized report for a vector
// retrieval. It is request-local and receives no raw prompt, credentials,
// vectors, provider/model metadata, or unauthorized space identity.
type EmbeddingQueryObserver func(EmbeddingQueryReport)

type embeddingQueryObservation struct {
	observer EmbeddingQueryObserver
}

type embeddingQueryObservationKey struct{}

// WithEmbeddingQueryObserver binds an authorized report callback to ctx. It
// does not grant retrieval or diagnostic permission and cannot widen the
// Decision resolved for the request.
func WithEmbeddingQueryObserver(
	ctx context.Context,
	observer EmbeddingQueryObserver,
) context.Context {
	return context.WithValue(
		ctx,
		embeddingQueryObservationKey{},
		embeddingQueryObservation{observer: observer},
	)
}

func notifyEmbeddingQueryObserver(
	ctx context.Context,
	report EmbeddingQueryReport,
) {
	observation, ok := ctx.Value(embeddingQueryObservationKey{}).(embeddingQueryObservation)
	if !ok || observation.observer == nil {
		return
	}
	observation.observer(cloneEmbeddingQueryReport(report))
}

func cloneEmbeddingQueryReport(report EmbeddingQueryReport) EmbeddingQueryReport {
	report.Spaces = append([]EmbeddingSpaceReport(nil), report.Spaces...)
	return report
}

type embeddingQueryCollector struct {
	mu             sync.Mutex
	spaces         map[string]struct{}
	attempted      map[string]struct{}
	completed      map[string]struct{}
	unavailable    map[string]struct{}
	fanoutLimit    uint64
	cacheHits      uint64
	providerCalls  uint64
	observed       bool
	fanoutExceeded bool
	invalid        bool
}

func newEmbeddingQueryCollector() *embeddingQueryCollector {
	return &embeddingQueryCollector{
		spaces:      make(map[string]struct{}),
		attempted:   make(map[string]struct{}),
		completed:   make(map[string]struct{}),
		unavailable: make(map[string]struct{}),
	}
}

func (c *embeddingQueryCollector) observe(event explorer.EmbeddingQueryEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.observed = true
	if event.FanoutLimit < 0 || event.CacheHits < 0 || event.ProviderCalls < 0 {
		c.invalid = true
		return
	}
	if event.FanoutLimit > 0 {
		limit := uint64(event.FanoutLimit)
		if c.fanoutLimit == 0 || limit < c.fanoutLimit {
			c.fanoutLimit = limit
		}
	}
	c.cacheHits += uint64(event.CacheHits)
	c.providerCalls += uint64(event.ProviderCalls)
	c.fanoutExceeded = c.fanoutExceeded || event.FanoutExceeded
	for _, identity := range event.SpaceIdentities {
		if identity == "" {
			c.invalid = true
			continue
		}
		c.spaces[identity] = struct{}{}
	}
	for _, identity := range event.Unavailable {
		if identity == "" {
			c.invalid = true
			continue
		}
		c.spaces[identity] = struct{}{}
		c.unavailable[identity] = struct{}{}
	}
	for _, identity := range event.Attempted {
		if identity == "" {
			c.invalid = true
			continue
		}
		c.spaces[identity] = struct{}{}
		c.attempted[identity] = struct{}{}
	}
	for _, identity := range event.Completed {
		if identity == "" {
			c.invalid = true
			continue
		}
		c.spaces[identity] = struct{}{}
		c.attempted[identity] = struct{}{}
		c.completed[identity] = struct{}{}
		if _, unavailable := c.unavailable[identity]; unavailable {
			c.invalid = true
		}
	}
	if c.cacheHits > math.MaxUint32 ||
		c.providerCalls > math.MaxUint32 ||
		c.fanoutLimit > math.MaxUint32 {
		c.invalid = true
	}
}

func (c *embeddingQueryCollector) err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.invalid {
		return nil
	}
	return shoal.NewError(
		shoal.ErrorInternal,
		"trusted vector backend returned invalid embedding query observability",
	)
}

func (c *embeddingQueryCollector) report(
	disclosure Disclosure,
	authorizedCandidates bool,
	requestErr error,
	discloseIdentities bool,
) EmbeddingQueryReport {
	c.mu.Lock()
	defer c.mu.Unlock()

	report := EmbeddingQueryReport{
		FanoutLimit:    uint32(c.fanoutLimit),
		Observed:       c.observed,
		Suppressed:     disclosure.Suppressed > 0,
		Restricted:     disclosure.Restricted > 0,
		FanoutExceeded: c.fanoutExceeded,
	}
	report.Degraded = requestErr != nil ||
		c.invalid ||
		c.fanoutExceeded ||
		len(c.unavailable) > 0 ||
		(authorizedCandidates && !c.observed)

	if !discloseIdentities {
		report.Spaces = nil
		report.CacheHits = 0
		report.ProviderCalls = 0
		report.Observed = false
		report.Suppressed = false
		report.Restricted = false
		report.FanoutExceeded = false
		report.Degraded = true
		return report
	}

	report.Spaces = make([]EmbeddingSpaceReport, 0, len(c.spaces))
	for identity := range c.spaces {
		status := EmbeddingSpaceAvailable
		if _, unavailable := c.unavailable[identity]; unavailable {
			status = EmbeddingSpaceUnavailable
		} else if _, completed := c.completed[identity]; completed {
			status = EmbeddingSpaceAvailable
		} else if len(c.attempted) > 0 {
			if _, attempted := c.attempted[identity]; attempted {
				status = EmbeddingSpaceNotCompleted
			} else {
				status = EmbeddingSpaceNotAttempted
			}
		} else if c.fanoutExceeded {
			status = EmbeddingSpaceNotAttempted
		} else if requestErr != nil {
			status = EmbeddingSpaceNotCompleted
		}
		report.Spaces = append(report.Spaces, EmbeddingSpaceReport{
			ID:     opaqueEmbeddingSpaceID(identity),
			Status: status,
		})
	}
	sort.Slice(report.Spaces, func(left, right int) bool {
		return bytes.Compare(
			[]byte(report.Spaces[left].ID),
			[]byte(report.Spaces[right].ID),
		) < 0
	})
	return report
}

func opaqueEmbeddingSpaceID(identity string) shoal.ID {
	digest := auth.Redact([]byte(identity)).Digest()
	return shoal.ID(digest.Bytes())
}
