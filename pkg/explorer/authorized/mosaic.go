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
	"sort"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// MosaicBudget configures the sensitivity-domain co-occurrence control that
// defends against the mosaic effect: individually authorized results that, in
// aggregate, disclose something an identity is not cleared to infer.
//
// Every authorized result belongs to a sensitivity domain derived from the
// access rule that governs it (see AccessRule.sensitivityDomain). A single
// domain is always disclosed once authorized; the budget bounds only how many
// *distinct* domains one identity may observe together within a window.
// MaxDomains is that bound. Once an identity has locked in MaxDomains distinct
// domains, further results from a not-yet-observed domain are withheld until
// the window elapses.
//
// A zero MaxDomains disables the control, preserving the pre-mosaic behavior.
// Window must be positive when the control is enabled.
type MosaicBudget struct {
	MaxDomains uint32
	Window     time.Duration
}

func (b MosaicBudget) enabled() bool {
	return b.MaxDomains > 0
}

// Disclosure separates the two reason classes by which the authorized layer
// withholds current documents from an identity. Suppressed counts documents an
// identity is not authorized to read at all (no grant, or the rule denies the
// operation). Restricted counts documents the identity *is* individually
// authorized to read but that the mosaic co-occurrence budget withheld to keep
// the identity within its distinct-domain budget.
//
// The two are deliberately kept apart so a caller can distinguish a plain
// authorization denial from a co-occurrence restriction. Both are counts only;
// neither ever names a domain, document, source, or policy.
type Disclosure struct {
	Suppressed uint32
	Restricted uint32
}

// CoOccurrenceRecord is the persisted per-identity mosaic state: the instant the
// current window opened and the sorted, distinct sensitivity domains observed
// within it. It is the entire state the co-occurrence budget needs, and it is
// stored through Shoal's own storage engine like every other policy record.
type CoOccurrenceRecord struct {
	WindowStart time.Time
	Domains     []string
}

// CoOccurrenceLedger is an optional PolicyStore extension that durably persists
// per-identity co-occurrence state. A store that implements it can enforce the
// mosaic budget across process restarts through the engine; the authorized
// client requires it whenever a MosaicBudget is enabled, so a misconfigured
// deployment fails closed at construction rather than silently disabling the
// control.
//
// Load reports (record, found, error). A not-found identity has no observed
// domains yet. Store overwrites the identity's record atomically.
type CoOccurrenceLedger interface {
	LoadCoOccurrence(context.Context, string) (CoOccurrenceRecord, bool, error)
	StoreCoOccurrence(context.Context, string, CoOccurrenceRecord) error
}

// sensitivityDomain returns a stable, opaque identifier for the sensitivity
// compartment this rule governs. Documents sharing a rule share a compartment;
// distinct grants are distinct compartments for co-occurrence accounting. The
// value is a one-way digest of the rule's canonical logical identity, so it
// never discloses the underlying domain, source, or policy.
func (r AccessRule) sensitivityDomain() string {
	if len(r.keys) == 0 {
		return ""
	}
	return auth.DigestBytes(
		"explorer-sensitivity-domain-v1", logicalRuleBytes(r.keys),
	).String()
}

// mosaicSelection is the outcome of applying the co-occurrence budget to one
// identity's ordered set of authorized documents: the documents that remain
// visible and how many were withheld to stay within the budget.
type mosaicSelection struct {
	allowed    map[shoal.ID]struct{}
	restricted uint32
}

// restrictCoOccurrence applies the co-occurrence budget to an ordered set of
// authorized documents and reports which remain visible. When the control is
// disabled it admits every document without touching the ledger, so the
// pre-mosaic read path is byte-for-byte unchanged. The domain function resolves
// each document's sensitivity domain lazily, so callers do not compute domains
// when the control is off.
func (c *Client) restrictCoOccurrence(
	ctx context.Context,
	decision auth.Decision,
	now time.Time,
	order []shoal.ID,
	domainOf func(shoal.ID) string,
) (mosaicSelection, error) {
	if !c.mosaic.enabled() {
		allowed := make(map[shoal.ID]struct{}, len(order))
		for _, documentID := range order {
			allowed[documentID] = struct{}{}
		}
		return mosaicSelection{allowed: allowed}, nil
	}
	domains := make(map[shoal.ID]string, len(order))
	for _, documentID := range order {
		domains[documentID] = domainOf(documentID)
	}
	return c.applyMosaicBudget(ctx, decision, now, order, domains)
}

// applyMosaicBudget charges an identity's sensitivity-domain co-occurrence
// budget for one read and reports which authorized documents remain visible.
//
// order lists the authorized document IDs in the deterministic base order the
// caller already established; domains maps each to its sensitivity domain. The
// greedy admission walks that order: a document whose domain is already
// observed is free; a document introducing a new domain is admitted only while
// the observed distinct-domain count is below the budget; every later new
// domain is withheld and counted as restricted.
//
// The charge is a set union over observed domains, so repeating an identical
// read never double-charges: admitted domains are already present and re-admit
// for free, and withheld domains remain withheld. The whole update runs under
// the accumulator lock and persists through the ledger so concurrent reads by
// one identity serialize and survive restarts.
func (c *Client) applyMosaicBudget(
	ctx context.Context,
	decision auth.Decision,
	now time.Time,
	order []shoal.ID,
	domains map[shoal.ID]string,
) (mosaicSelection, error) {
	selection := mosaicSelection{allowed: make(map[shoal.ID]struct{}, len(order))}
	_, err := c.accumulator.Update(ctx, decision, now, func(previous []string) ([]string, error) {
		observed := make(map[string]struct{}, len(previous))
		for _, domain := range previous {
			observed[domain] = struct{}{}
		}
		budget := int(c.mosaic.MaxDomains)
		for _, documentID := range order {
			domain := domains[documentID]
			if domain == "" {
				// Every authorized document is governed by a nonempty rule, so an
				// empty sensitivity domain is an internal inconsistency. Fail
				// closed rather than collapse ungoverned documents into one shared
				// compartment.
				return nil, inconsistentBase()
			}
			if _, seen := observed[domain]; seen {
				selection.allowed[documentID] = struct{}{}
				continue
			}
			if len(observed) < budget {
				observed[domain] = struct{}{}
				selection.allowed[documentID] = struct{}{}
				continue
			}
			// Load-bearing: the withholding branch is pinned by
			// TestMosaicBudgetWithholdsCrossDomainResults; deleting the increment
			// or the continue would let a restricted document through.
			selection.restricted++
		}

		domainsOut := make([]string, 0, len(observed))
		for domain := range observed {
			domainsOut = append(domainsOut, domain)
		}
		sort.Strings(domainsOut)
		return domainsOut, nil
	})
	if err != nil {
		return mosaicSelection{}, err
	}
	return selection, nil
}

// mosaicLedger adapts the existing record without changing its persisted schema.
type mosaicLedger struct{ CoOccurrenceLedger }

func (l mosaicLedger) Load(ctx context.Context, key string) (AccumulatorRecord[[]string], bool, error) {
	record, found, err := l.LoadCoOccurrence(ctx, key)
	return AccumulatorRecord[[]string]{WindowStart: record.WindowStart, State: record.Domains}, found, err
}

func (l mosaicLedger) Store(ctx context.Context, key string, record AccumulatorRecord[[]string]) error {
	return l.StoreCoOccurrence(ctx, key, CoOccurrenceRecord{WindowStart: record.WindowStart, Domains: record.State})
}

func (b MosaicBudget) accumulator(store PolicyStore) (*Accumulator[[]string], error) {
	if !b.enabled() {
		return nil, nil
	}
	ledger, _ := store.(CoOccurrenceLedger)
	// Do not hide a nil underlying ledger inside a nonnil adapter.
	var adapter AccumulatorLedger[[]string]
	if !isNilDependency(ledger) {
		adapter = mosaicLedger{ledger}
	}
	accumulator, err := NewAccumulator(adapter, b.Window, "explorer-mosaic-identity-v1")
	if err != nil {
		// Preserve the existing construction errors as well as validation order.
		if b.Window <= 0 {
			return nil, dependencyRequired("mosaic co-occurrence window")
		}
		return nil, dependencyRequired("co-occurrence ledger")
	}
	return accumulator, nil
}
