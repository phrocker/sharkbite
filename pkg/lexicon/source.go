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

// Package lexicon builds a graph-derived vocabulary bundle: the names,
// aliases and derived abbreviations of graph nodes, compiled into a token-level
// Aho-Corasick matcher that returns node IDs, plus lookup templates derived
// from ontology relationship definitions.
//
// A bundle is content-addressed (its ID is the SHA-256 of its canonical bytes)
// and pinned to the graph snapshot it was built from. It has one of two
// scopes. A server-filtered bundle may contain any node; every match must be
// filtered against the caller's current authorization before it is used or
// echoed, and the bundle's bytes cannot be exported. A pinned bundle is built
// from a node set that was already filtered for one authorization scope, names
// that scope's digest, and is the only kind that can be shipped.
//
// The package does no authorization itself: Candidates reports every overlap
// regardless of visibility, and Select must be given the caller's visibility.
// See pkg/explorer/authorized ResolveMentions for the composition.
package lexicon

import (
	"time"

	"github.com/phrocker/shoal-oss/internal/lexiconscope"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/ontology"
)

// Graph property keys the builder reads. Names come from PropertyName, else
// PropertyTitle; the ontology entity key and every alias key add further
// terms. Labels are not used in this version.
const (
	PropertyName      = "name"
	PropertyTitle     = "title"
	PropertyEntityKey = "shoal.ontology.entity_key"
	// PropertyAliasPrefix reserves "shoal.lexicon.alias.<n>" keys for
	// aliases. Any non-empty suffix is accepted; the suffix carries no
	// meaning beyond letting a node hold several aliases.
	PropertyAliasPrefix = "shoal.lexicon.alias."
)

// SentinelIDPrefix is reserved for padding identifiers that authorization
// lookups use to keep their batch size fixed. Build refuses any node whose ID
// starts with it, so a padding identifier can never be a lexicon candidate.
const SentinelIDPrefix = "\x00shoal.lexicon.sentinel/"

// Snapshot pins a bundle to one immutable graph frontier. It mirrors
// explorer.Snapshot without importing the explorer.
type Snapshot struct {
	ID       string
	AsOf     time.Time
	Frontier uint64
}

// Scope is the authorization scope a bundle was built for. It is either
// ScopeServerFiltered or ScopePinned.
type Scope interface {
	scopeKind() byte
}

// ScopeServerFiltered marks a bundle built from nodes of any visibility. Its
// matches must be filtered against the caller's current authorization, and
// it cannot be exported.
type ScopeServerFiltered struct{}

// ScopePinned marks a bundle built from ScopedNodes: nodes an authorized
// filter selected for one caller's authorization at one policy generation and
// snapshot, identified by Digest. Only such a bundle can be shipped. A
// ScopePinned value is read-only; it cannot be used to build a bundle.
type ScopePinned struct {
	digest [32]byte
}

// Digest identifies the authorization scope, policy generation and snapshot
// the bundle's nodes were filtered for.
func (s ScopePinned) Digest() [32]byte { return s.digest }

// ScopedNodes is a node set an authorized filter selected for one scope. It
// can be made only by that filter (pkg/explorer/authorized
// Client.LexiconScopeNodes), so a pinned bundle can be built only from nodes
// that were actually filtered, and its scope digest is computed rather than
// claimed. The zero value is refused by Build.
type ScopedNodes struct {
	nodes    []graph.Node
	snapshot Snapshot
	digest   [32]byte
}

// Len is the number of nodes in the scope.
func (s ScopedNodes) Len() int { return len(s.nodes) }

// Snapshot is the snapshot the scope was computed for.
func (s ScopedNodes) Snapshot() Snapshot { return s.snapshot }

// Scope is the pinned scope a bundle built from s will carry.
func (s ScopedNodes) Scope() ScopePinned { return ScopePinned{digest: s.digest} }

// sealScopedNodes is the only constructor of a non-zero ScopedNodes. It is
// reachable from outside this package only through the module-internal
// lexiconscope hook, which the authorized filter calls.
func sealScopedNodes(
	nodes []graph.Node, snapshot Snapshot, digest [32]byte,
) ScopedNodes {
	cloned := make([]graph.Node, len(nodes))
	for index, node := range nodes {
		node.Labels = append([]string(nil), node.Labels...)
		properties := make(map[string]string, len(node.Properties))
		for key, value := range node.Properties {
			properties[key] = value
		}
		node.Properties = properties
		cloned[index] = node
	}
	snapshot.AsOf = snapshot.AsOf.UTC()
	return ScopedNodes{nodes: cloned, snapshot: snapshot, digest: digest}
}

func init() {
	lexiconscope.Install(func(
		nodes []graph.Node, snapshotID string, asOf time.Time, frontier uint64,
		digest [32]byte,
	) any {
		return sealScopedNodes(nodes, Snapshot{
			ID: snapshotID, AsOf: asOf, Frontier: frontier,
		}, digest)
	})
}

const (
	scopeKindServerFiltered byte = 1
	scopeKindPinned         byte = 2
)

func (ScopeServerFiltered) scopeKind() byte { return scopeKindServerFiltered }
func (ScopePinned) scopeKind() byte         { return scopeKindPinned }

// Input is everything a build reads. Nothing in the graph lists every node, so
// the caller supplies the node set. Order of Nodes, of their properties and of
// Relationships does not affect the result.
//
// With Scoped nil the bundle is ScopeServerFiltered and built from Nodes.
// With Scoped set the bundle is ScopePinned: its nodes and snapshot come from
// Scoped only, Nodes must be empty, Snapshot must be zero or equal Scoped's,
// and Relationships must be empty, because lookup templates reveal relation
// types and are not yet filtered by scope.
type Input struct {
	Snapshot      Snapshot
	Nodes         []graph.Node
	Scoped        *ScopedNodes
	Relationships []ontology.RelationshipDefinition
	// DeriveInitialisms adds, for each name or title of at least
	// MinInitialismTokens tokens, the initialism of its tokens as a term with
	// OriginDerived. It is off by default: short initialisms collide across
	// many nodes, and since limits fail the build rather than drop, a large
	// graph would otherwise exceed MaxPostingsPerTerm on derived terms alone.
	// The setting is recorded in the bundle, so it is part of the ID.
	DeriveInitialisms bool
}

// MinInitialismTokens is the shortest name an initialism is derived from.
const MinInitialismTokens = 3

// Origin records where a term came from. Lower values are stronger: when one
// node reaches the same term several ways, the posting keeps the lowest.
type Origin uint8

const (
	OriginName      Origin = 1
	OriginTitle     Origin = 2
	OriginEntityKey Origin = 3
	OriginAlias     Origin = 4
	// OriginDerived is an initialism derived from a name or title of at
	// least MinInitialismTokens tokens, when Input.DeriveInitialisms is set.
	OriginDerived Origin = 5
)

func (o Origin) valid() bool {
	return o >= OriginName && o <= OriginDerived
}

func (o Origin) String() string {
	switch o {
	case OriginName:
		return "name"
	case OriginTitle:
		return "title"
	case OriginEntityKey:
		return "entity_key"
	case OriginAlias:
		return "alias"
	case OriginDerived:
		return "derived"
	default:
		return "invalid"
	}
}
