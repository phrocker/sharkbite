// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied. See the License for the
// specific language governing permissions and limitations
// under the License.

// Package atpl compiles reviewable, versioned policy files to fleet registry
// registrations.
//
// # Origin
//
// The format is derived from the Agent Trust Policy Language,
// github.com/SentriusLLC/atpl (Apache-2.0). It keeps the ATPL name and the
// idea — policy an operator can review as a diff and reproduce from source —
// but not ATPL's schema, which does not fit Shoal's semantics. Every file
// carries that credit in its required "origin" field (see Origin).
//
// # What compiles
//
// A file declares executors and agents. An agent compiles to exactly one
// fleet.Spec: identity, parent, authorization domain, scopes, executor
// reference, capabilities with declared effects and approval requirements,
// and a lease TTL that becomes an absolute lease at one shared compile time.
//
// An action may declare "approval": {"required": true}, which compiles to
// fleet.Action.RequiresApproval (#451). It is accepted on an action only;
// omission means not required, and an explicit false is refused. Inheritance
// carries a parent's requirement, and a delegated action may add one but
// never drop its parent's. Plan treats adding a requirement as a narrowing and
// dropping one as a refused widening; Export writes it. Validation calls the registry's
// own validators (pkg/explorer/fleet/policy_export.go) and re-composes the
// delegation conditions Register applies, so a file is refused for what the
// API would refuse, with the offending path, before anything is applied.
//
// Executor ceilings and floors are host configuration with no read API, so a
// file declares them as assertions. Compilation checks every action against
// them; the server remains authoritative and refuses on its own binding.
//
// # What was dropped, and why
//
// ATPL's weighted trust_score and its fixed behavior thresholds are not part of
// this format and are refused by name. A hand-weighted sum that gates admission
// is uncalibrated authority; how far to trust an agent is a typed decision
// evaluated against attributed outcomes, not a policy field. See
// docs/gateways.md, "What comes from ATPL".
//
// ATPL's approval (marginal outcome) is adopted as the per-action
// requirement above. Executor attestation is adopted as a per-action
// requirement too: "attestation": {"required": true} compiles to
// fleet.Action.RequiresAttestation (#446) with exactly approval's strictness,
// and is refused on an action that does not declare the external effect.
// ATPL's "runtime" section is not compiled: what a runtime must attest to is
// host trust configuration. Admission obligations are
// computed per request at admission rather than declared per agent, and how a
// policy would constrain them is deferred. Those fields are refused by name, so
// a file never appears to grant something it does not.
package atpl
