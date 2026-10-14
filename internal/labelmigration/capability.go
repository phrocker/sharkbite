// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package labelmigration holds the capability that admits the startup label
// migration (#570): authorized.Client.MigrateLabelledDocuments, which narrows
// the policy-catalog rules of documents labelled before labels were enforced.
//
// The migration is a system task. It runs once at startup, before anything is
// served, with no request and no principal, so it cannot be gated by an
// auth.Decision the way every other catalog write is. This package is the
// gate instead, following internal/devbackfill: it is module-internal, so no
// module outside this repository can name *Capability, and the method that
// takes one is uncallable from outside with anything but nil, which is
// refused.
//
// The migration only ever narrows rules (authorized.PolicyStore.TightenRule
// refuses anything else), so the capability confers no read access and no
// way to widen one.
package labelmigration

// Capability admits the startup label migration. Like
// devbackfill.Capability it carries no evidence about its caller; what it
// provides is a name no module outside this repository can write.
//
// Only NewCapability produces a granted one. The minting sites are the
// startup paths that serve the authorized store (cmd/shoal-explore-web and
// cmd/shoal-mcp), which TestLabelMigrationCapabilityMintSites enforces.
type Capability struct {
	granted bool
}

// NewCapability mints the capability. Callers must be a process's own
// startup path, holding the store before anything is served.
func NewCapability() *Capability {
	return &Capability{granted: true}
}

// Granted reports whether this capability came from NewCapability. A nil or
// zero-valued capability is not granted, so the migration fails closed.
func (c *Capability) Granted() bool {
	return c != nil && c.granted
}
