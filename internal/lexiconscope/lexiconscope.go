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

// Package lexiconscope carries the one capability that turns an authorized
// node filter's output into lexicon.ScopedNodes. pkg/lexicon installs the
// sealer once, at init; pkg/explorer/authorized calls Seal. Being internal to
// the module, it is not reachable by API consumers, and
// TestOnlyLexiconAndAuthorizedImportLexiconScope keeps every other package in
// the module from importing it, so a pinned lexicon bundle cannot be built from
// a node set that did not pass the authorized filter.
package lexiconscope

import (
	"time"

	"github.com/phrocker/shoal-oss/pkg/graph"
)

// Sealer builds a lexicon.ScopedNodes, returned as any to avoid an import
// cycle.
type Sealer func(
	nodes []graph.Node, snapshotID string, asOf time.Time, frontier uint64,
	digest [32]byte,
) any

var sealer Sealer

// Install sets the sealer. pkg/lexicon calls it from init; any second call
// panics, so nothing can replace the sealer afterwards.
func Install(seal Sealer) {
	if seal == nil {
		panic("lexiconscope: nil sealer")
	}
	if sealer != nil {
		panic("lexiconscope: sealer already installed")
	}
	sealer = seal
}

// Seal returns a lexicon.ScopedNodes. Importing pkg/lexicon guarantees its
// init, and so Install, ran first.
func Seal(
	nodes []graph.Node, snapshotID string, asOf time.Time, frontier uint64,
	digest [32]byte,
) any {
	if sealer == nil {
		panic("lexiconscope: pkg/lexicon is not initialized")
	}
	return sealer(nodes, snapshotID, asOf, frontier, digest)
}
