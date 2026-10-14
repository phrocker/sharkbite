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

package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	admissionapi "github.com/phrocker/shoal-oss/pkg/admission/api"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// The declaration is what Shoal receives. The prompt is not.
//
// This is the single most important property of the proxy. Shoal's admission
// surface takes a declaration of what a call would do, deliberately, so that
// the plane deciding whether content may be transmitted does not itself hold a
// copy of that content. A proxy that forwarded the prompt for adjudication
// would defeat the design it is built on: the content would reach Shoal, be
// recorded in its audit trail, and the decision about transmitting it would be
// made by a system that had already received it.
//
// So the proxy sends shape, never substance: how many messages, how large,
// which model, and which corpus references the caller itself named. None of
// that reconstructs the prompt.

// callerIdentity is the per-request identity the proxy derives before it asks.
type callerIdentity struct {
	AdmissionID   string
	TokenID       string
	RequestID     string
	CorrelationID string
	ObjectID      string
}

// newCallerIdentity mints the identifiers one call needs.
//
// The admission ID is random rather than derived from the payload. A derived ID
// would make two identical prompts share an admission, so the second would
// replay the first's decision instead of being adjudicated — and an
// accumulator counting calls would see one. Randomness here is not for
// unguessability: the admission surface derives its own durable key from the
// principal and ignores whatever the caller suggests (#412).
func newCallerIdentity() (callerIdentity, error) {
	var buffer [48]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		return callerIdentity{}, err
	}
	return callerIdentity{
		AdmissionID:   encodeID(buffer[0:16]),
		TokenID:       encodeID(buffer[16:32]),
		RequestID:     encodeID(buffer[32:40]),
		CorrelationID: encodeID(buffer[40:48]),
		ObjectID:      encodeID(buffer[0:16]),
	}, nil
}

// mintCallerIdentity is what the proxy calls. A variable so a test can reach
// the identity_unavailable refusal, which crypto/rand otherwise never takes.
var mintCallerIdentity = newCallerIdentity

// chatRequest is the subset of the OpenAI-compatible body the proxy reads.
//
// Unknown fields are preserved, not rejected. The proxy is a pass-through for
// an API it does not own: refusing a field a newer client sends would make the
// proxy the reason an unmodified client stops working, which is the one thing
// it exists not to be. It therefore decodes into a map and reads only what it
// needs.
type chatRequest struct {
	body     map[string]json.RawMessage
	model    string
	messages []chatMessage
	stream   bool
}

type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// shoalReferences is the extension a caller uses to name the corpus references
// its payload carries.
//
// A caller that names nothing gets no obligations, and that is honest rather
// than permissive: the proxy cannot discover references inside opaque prompt
// text, and pretending otherwise would be content classification, which #390
// rules out. What it can do is enforce the obligation on references a caller
// does declare, which is what a Shoal-aware caller gets for declaring them.
const shoalReferencesField = "shoal_references"

func parseChatRequest(raw []byte) (chatRequest, error) {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return chatRequest{}, fmt.Errorf("malformed request body")
	}
	// A body carrying attribution is decoded again, strictly, before anything
	// reads it: see decodeStrictBody. A body without it is parsed exactly as
	// before, so an unmodified client meets no new refusal.
	if strictBodyRequired(body) {
		if err := decodeStrictBody(raw); err != nil {
			return chatRequest{}, err
		}
	}
	result := chatRequest{body: body}
	if encoded, ok := body["model"]; ok {
		if err := json.Unmarshal(encoded, &result.model); err != nil {
			return chatRequest{}, fmt.Errorf("model must be a string")
		}
	}
	if strings.TrimSpace(result.model) == "" {
		return chatRequest{}, fmt.Errorf("model is required")
	}
	encodedMessages, ok := body["messages"]
	if !ok {
		return chatRequest{}, fmt.Errorf("messages is required")
	}
	if err := json.Unmarshal(encodedMessages, &result.messages); err != nil {
		return chatRequest{}, fmt.Errorf("messages must be an array of objects")
	}
	if len(result.messages) == 0 {
		return chatRequest{}, fmt.Errorf("messages must not be empty")
	}
	if encoded, ok := body["stream"]; ok {
		if err := json.Unmarshal(encoded, &result.stream); err != nil {
			return chatRequest{}, fmt.Errorf("stream must be a boolean")
		}
	}
	return result, nil
}

// references returns what the caller declared, in order and deduplicated.
func (r chatRequest) references() ([]string, error) {
	encoded, ok := r.body[shoalReferencesField]
	if !ok {
		return nil, nil
	}
	var declared []string
	if err := json.Unmarshal(encoded, &declared); err != nil {
		return nil, fmt.Errorf("%s must be an array of strings", shoalReferencesField)
	}
	seen := make(map[string]struct{}, len(declared))
	result := make([]string, 0, len(declared))
	for _, reference := range declared {
		trimmed := strings.TrimSpace(reference)
		if trimmed == "" {
			return nil, fmt.Errorf("%s must not contain a blank entry", shoalReferencesField)
		}
		// Validated here because these travel to the plane as disclosures, and
		// it decodes them as unpadded base64url (webapi/wire.go). A reference
		// like "doc-a" is a permanently malformed request, and without this it
		// reached the plane as a 400 that the admission client turns into
		// ErrPlaneUnreachable — so the caller was told 503, which means retry,
		// for something no retry can fix. Classification is the whole point:
		// an infrastructural failure and a bad request must not be the same
		// answer, which is the criterion the plane-status table also covers.
		decoded, err := base64.RawURLEncoding.DecodeString(trimmed)
		if err != nil || len(decoded) == 0 {
			return nil, fmt.Errorf(
				"%s entries must be unpadded base64url document IDs",
				shoalReferencesField)
		}
		// The size bound the plane enforces, enforced here for the same reason
		// the encoding is: over it the plane answers 400, the admission client turns that
		// into ErrPlaneUnreachable, and the caller is told 503 — retry — for a
		// request no retry can fix. Checking the syntax and not the size left
		// exactly half of that closed.
		if len(decoded) > shoal.MaxIDBytes {
			return nil, fmt.Errorf(
				"%s entries must decode to at most %d bytes",
				shoalReferencesField, shoal.MaxIDBytes)
		}
		if _, duplicate := seen[trimmed]; duplicate {
			continue
		}
		seen[trimmed] = struct{}{}
		result = append(result, trimmed)
	}
	// The count bound, applied after de-duplication because that is the number
	// the plane actually receives.
	if len(result) > admissionapi.MaxDisclosures {
		return nil, fmt.Errorf(
			"%s must name at most %d documents",
			shoalReferencesField, admissionapi.MaxDisclosures)
	}
	return result, nil
}

// knownRoles is the finite vocabulary the OpenAI-compatible surface defines.
//
// It exists because a role is a free-form string on the wire, and a string the
// proxy copies into the declaration is a channel for exactly the content the
// declaration is supposed to exclude. A caller can put a prompt in
// messages[].role, and nothing about the field's name stops it.
var knownRoles = map[string]struct{}{
	"system": {}, "user": {}, "assistant": {},
	"tool": {}, "function": {}, "developer": {},
}

// roleOther is what an unrecognised role becomes in the declaration.
//
// Not the original string, and not an error either: refusing an unknown role
// would make the proxy the reason an unmodified client breaks when the upstream
// adds one, which is the thing it exists not to be. The request is forwarded
// with the caller's role intact; only the declaration is sanitised, because the
// declaration is the part that leaves for Shoal.
const roleOther = "other"

func classifyRole(role string) string {
	if _, ok := knownRoles[strings.ToLower(strings.TrimSpace(role))]; ok {
		return strings.ToLower(strings.TrimSpace(role))
	}
	return roleOther
}

// modelOther marks a model the operator has not named, the way roleOther marks
// a role the API does not define.
const modelOther = "other"

// classifyModel keeps caller text out of the declaration.
//
// model is a free-form string the caller controls, and it was copied verbatim —
// the same hole as role, which this proxy already closed, and I did not
// generalise the fix at the time. A prompt or a secret fits in model just as
// well, and nothing about the field's name prevents it.
//
// A finite vocabulary is not available here: model names are whatever a
// provider serves. So the operator names the ones this deployment expects
// (-model) and the declaration reports the matched name — operator text, never
// the caller's. Anything else becomes a marker.
//
// With no allow-list the declaration always says "other", which is the safe
// default rather than an oversight: it costs the plane model granularity until
// an operator opts in, and it closes the channel for every deployment that has
// not. An unlisted model is still forwarded with the caller's own string, and
// the plane can deny on "other" if it wants to; refusing here would make this
// model gating, which #390 lists as a non-goal.
func classifyModel(model string, allowed map[string]struct{}) string {
	trimmed := strings.TrimSpace(model)
	if _, ok := allowed[trimmed]; ok {
		return trimmed
	}
	return modelOther
}

// outbound is the body the provider receives: the caller's request with
// Shoal's own extension removed and everything else untouched.
//
// shoal_references is proxy metadata. It was forwarded verbatim, which is wrong
// in two directions at once.
//
// It is a disclosure. The IDs name governed documents the call concerns, and
// sending them to a third-party provider tells that provider which of them this
// request is about — the proxy declaring egresses-content and then adding a
// little more egress of its own, as metadata, to the one party the egress
// policy is about.
//
// And it is a compatibility hazard: a strict OpenAI-compatible server rejects
// an unknown top-level field, so the configuration most likely to be governed
// is the one most likely to break.
//
// Only the known extension is removed. Genuinely unknown fields are preserved,
// because the proxy must not be the reason a provider feature nobody here has
// heard of stops working — which is the whole reason the body is a map rather
// than a struct.
//
// shoal_attribution is removed for the same reasons and one more: its digests
// are derived from governed content.
func (r chatRequest) outbound() map[string]json.RawMessage {
	_, references := r.body[shoalReferencesField]
	_, attribution := r.body[shoalAttributionField]
	if !references && !attribution {
		return r.body
	}
	body := make(map[string]json.RawMessage, len(r.body))
	for key, value := range r.body {
		if key == shoalReferencesField || key == shoalAttributionField {
			continue
		}
		body[key] = value
	}
	return body
}

// declaration is the shape the proxy sends in place of the payload.
//
// Sizes, counts, and strings the operator chose. A reviewer checking that Shoal
// never receives prompt text should be able to confirm it by reading this
// function, which is why it builds the value explicitly rather than marshalling
// a struct that could later gain a field carrying content.
//
// The one caller-chosen thing that still travels is the reference list, and
// that is deliberate: the plane cannot authorise disclosures it is not told
// about. Those are constrained to decodable document IDs rather than free text,
// which is a different thing from model and role, where the field had no
// business carrying anything the caller composed.
func (r chatRequest) declaration(
	references []string, models map[string]struct{},
) json.RawMessage {
	roles := make([]string, 0, len(r.messages))
	total := 0
	for _, message := range r.messages {
		roles = append(roles, classifyRole(message.Role))
		total += len(message.Content)
	}
	encoded, _ := json.Marshal(map[string]any{
		"model":           classifyModel(r.model, models),
		"message_count":   len(r.messages),
		"message_roles":   roles,
		"content_bytes":   total,
		"stream":          r.stream,
		"reference_count": len(references),
	})
	return encoded
}
