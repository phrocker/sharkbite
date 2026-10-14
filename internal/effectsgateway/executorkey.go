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

package effectsgateway

import (
	"crypto/subtle"
	"encoding/base64"

	dispatchapi "github.com/phrocker/shoal-oss/pkg/dispatch/api"
)

// ExecutorKeyBytes is the width of an action's ExecutorKey: a SHA-256 digest
// the fleet derives at enqueue over a domain tag, the action ID and the
// caller's idempotency key, each length-prefixed.
const ExecutorKeyBytes = dispatchapi.ExecutorKeyBytes

// ExecutorKey is the idempotency key the gateway presents to a target.
//
// It is the action's own ExecutorKey, carried on the claim response, and never
// a digest this package computes. A hand-written digest over the same inputs
// is easy to get subtly wrong — without length prefixes it is not injective,
// and a collision makes a provider return a cached success without performing
// the effect, which nothing then records.
//
// The zero value is not a key; HasKey distinguishes "the claim carried none".
type ExecutorKey struct {
	bytes [ExecutorKeyBytes]byte
	set   bool
}

// ErrExecutorKeyMissing says the claim response carried no executor key. It is
// the public client's sentinel, so either matches with errors.Is.
var ErrExecutorKeyMissing = dispatchapi.ErrExecutorKeyMissing

// ParseExecutorKey decodes an ExecutorKey from either spelling the platform
// emits for it, and refuses everything else (dispatchapi.ParseExecutorKey,
// which the dispatch client applies to every claim response).
//
// The dispatch HTTP wire spells it unpadded base64url. The MCP tools return
// fleet.ActionRecord whole, and that type has no JSON tags, so encoding/json
// spells the same 32 bytes as padded standard base64 under "ExecutorKey". Both
// decode to one key, so two keys are compared as bytes (Equal) and never as
// strings: comparing spellings would call one key two different keys.
//
// Whitespace is refused rather than trimmed. encoding/base64 silently skips
// '\r' and '\n' inside its input, so without the explicit check a key with an
// embedded newline would decode to the same bytes as the clean one and pass —
// which is harmless for this value and still not something a strict decoder
// should accept from a wire that never produces it.
func ParseExecutorKey(encoded string) (ExecutorKey, error) {
	decoded, err := dispatchapi.ParseExecutorKey(encoded)
	if err != nil {
		return ExecutorKey{}, err
	}
	var key ExecutorKey
	copy(key.bytes[:], decoded)
	key.set = true
	return key, nil
}

// HasKey reports whether the key was decoded from a claim at all.
func (k ExecutorKey) HasKey() bool { return k.set }

// Equal compares decoded bytes. Two keys that were never set are not equal to
// anything, including each other: an absent key must not match an absent key.
func (k ExecutorKey) Equal(other ExecutorKey) bool {
	if !k.set || !other.set {
		return false
	}
	return subtle.ConstantTimeCompare(k.bytes[:], other.bytes[:]) == 1
}

// HeaderValue is the canonical spelling sent to the target: unpadded
// base64url, 43 characters, every one a valid HTTP header token character.
func (k ExecutorKey) HeaderValue() string {
	if !k.set {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(k.bytes[:])
}

// String never prints the key. A key in a log line is a key a reader of the
// log can replay against the target's idempotency namespace.
func (k ExecutorKey) String() string {
	if !k.set {
		return "ExecutorKey(none)"
	}
	return "ExecutorKey(set)"
}

// GoString keeps %#v from printing the bytes through the struct fields.
func (k ExecutorKey) GoString() string { return k.String() }
