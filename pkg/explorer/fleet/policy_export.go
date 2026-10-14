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

package fleet

import (
	"encoding/json"
	"time"
)

// The functions in this file expose the registry's own validators to the
// policy compiler in pkg/atpl, so a policy file is refused for exactly the
// reasons a registration would be, and to the language router in pkg/router,
// so a proposed action input is refused, and canonicalized, for exactly the
// reasons and in exactly the form an enqueue would. They are thin wrappers and
// change no behaviour: each calls the unexported function Register or Enqueue
// calls. A compiler or router that reimplemented these checks would drift from
// the registry the first time either changed.

// Canonical validates a spec against the registry's bounds at now and returns
// the canonical form Register stores: scopes sorted and unique, capabilities
// and actions sorted and unique, schemas re-encoded, effects sorted and
// deduplicated. See Spec.canonical.
func (s Spec) Canonical(now time.Time) (Spec, error) {
	return s.canonical(now)
}

// CanonicalCapabilities validates and canonicalizes capabilities exactly as
// Register does for a whole spec. See canonicalCapabilities.
func CanonicalCapabilities(input []Capability) ([]Capability, error) {
	return canonicalCapabilities(input)
}

// CanonicalEffects sorts and deduplicates a declared effect set, refusing any
// class it does not recognise. See canonicalEffects.
func CanonicalEffects(declared Effects) (Effects, error) {
	return canonicalEffects(declared)
}

// ValidateDeclaredEffects refuses an action whose declared effects exceed the
// executor's ceiling or omit its floor. See validateDeclaredEffects.
func ValidateDeclaredEffects(capabilities []Capability, floor, ceiling Effects) error {
	return validateDeclaredEffects(capabilities, floor, ceiling)
}

// ScopesSubset reports whether every child scope is one of the parent's. See
// scopesSubset.
func ScopesSubset(child, parent []Scope) bool {
	return scopesSubset(child, parent)
}

// CapabilitiesSubset reports whether every child action names a parent action
// with identical schemas and no wider effects. See capabilitiesSubset.
func CapabilitiesSubset(child, parent []Capability) bool {
	return capabilitiesSubset(child, parent)
}

// ValidateActionInput validates input against action's input schema exactly
// as Enqueue does, argument for argument, and returns the canonical bytes
// Enqueue would store on the action record: the document decoded and
// re-encoded, so duplicate keys collapse to the last value and object members
// are ordered by key. It grants nothing; the router uses it so that the input
// it proposes is byte for byte what an enqueue would store. See
// validateAgainstSchema.
func ValidateActionInput(action Action, input json.RawMessage) (json.RawMessage, error) {
	return validateAgainstSchema(action.InputSchema, input, "action input", MaxActionPayloadBytes)
}
