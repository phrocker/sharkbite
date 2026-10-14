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

// Package fleet defines the durable, authorization-enforcing product-agent
// registry. Descriptors contain only declarative schemas and an opaque
// host-owned executor reference; they can never carry commands, environment,
// working directories, caller URLs, or transferable sessions.
package fleet

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/phrocker/shoal-oss/pkg/executorref"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	MaxScopes             = 64
	MaxCapabilities       = 64
	MaxActions            = 256
	MaxNameBytes          = 128
	MaxExecutorRefBytes   = executorref.MaxExecutorRefBytes
	MaxSchemaBytes        = 64 << 10
	MaxDescriptorBytes    = 1 << 20
	MaxLease              = 24 * time.Hour
	MaxReasonCodeBytes    = 64
	MaxRegistrationKeyLen = 1024
	MaxDelegationDepth    = 64
)

type Scope struct {
	SourceID []byte `json:"source_id"`
	PolicyID []byte `json:"policy_id"`
}

// Effect classifies where an action's consequences land. It is the boundary
// between what Shoal will run itself and what it will only dispatch.
//
// This is a declaration check, not a sandbox. Nothing prevents Go code bound
// as an in-process executor from opening a socket, and nothing here tries to.
// What it prevents is the mismatch: an action declaring an external effect
// cannot resolve to an executor the host bound as evidence-only, so external
// work cannot be performed in-process by accident or by a descriptor that
// claims otherwise. A host that binds an executor doing external work under an
// evidence-only ceiling has misdeclared its own configuration, and no
// invariant here can detect that.
type Effect string

// The classes are a set, not a ladder, because the risks they name do not
// order. Writing a local file mutates without transmitting. Streaming a
// compartmented corpus to a hosted model transmits without mutating. Neither
// is a subset of the other, so there is no answer to "does egress outrank
// mutation" — and a ladder forces one.
//
// A set also matches machinery that already exists: capabilitiesSubset and
// scopesSubset are subset semantics, and delegation narrowing falls out
// unchanged.
const (
	// EffectReadsCorpus is work that reads Shoal's own evidence record.
	EffectReadsCorpus Effect = "reads-corpus"
	// EffectEgressesContent is work that transmits corpus content off the host
	// running Shoal.
	//
	// It is not a property of the code. The same executor is egress-free
	// against a loopback model provider and egress-bearing against a hosted
	// one, so an executor declaring this must derive it from the provider it
	// was actually configured with.
	EffectEgressesContent Effect = "egresses-content"
	// EffectMutatesExternal is work that changes something outside Shoal's
	// evidence record: writing to another system, changing a host, sending a
	// message someone acts on. Shoal dispatches it and records the outcome; it
	// does not run it.
	//
	// Its wire value is deliberately the string the superseded two-value
	// taxonomy used for the same meaning, so a descriptor written before the
	// set existed decodes to exactly this and hashes identically. See
	// Effects.digestBytes.
	EffectMutatesExternal Effect = "external"
)

func (e Effect) validate() error {
	switch e {
	case EffectReadsCorpus, EffectEgressesContent, EffectMutatesExternal:
		return nil
	default:
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "action effect is not a known class")
	}
}

// Effects is a declared set of effect classes.
//
// The empty set is the zero value and means the action's consequences land
// nowhere this taxonomy names — it reads nothing, transmits nothing and
// changes nothing outside Shoal. That is also what a descriptor written before
// this field existed decodes to, so old records keep their previous meaning.
type Effects []Effect

// canonicalEffects sorts and deduplicates a declared set, rejecting any class
// it does not recognise.
//
// Canonical order is what makes the set comparable and hashable: two
// descriptors declaring the same classes in different order are the same
// declaration and must not produce different digests.
func canonicalEffects(declared Effects) (Effects, error) {
	if len(declared) == 0 {
		return nil, nil
	}
	seen := make(map[Effect]struct{}, len(declared))
	result := make(Effects, 0, len(declared))
	for _, effect := range declared {
		if err := effect.validate(); err != nil {
			return nil, err
		}
		if _, duplicate := seen[effect]; duplicate {
			continue
		}
		seen[effect] = struct{}{}
		result = append(result, effect)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result, nil
}

// contains reports set membership.
func (e Effects) contains(effect Effect) bool {
	for _, declared := range e {
		if declared == effect {
			return true
		}
	}
	return false
}

// exceeds reports whether this declaration asks for anything a ceiling does not
// permit — that is, whether it is not a subset of the ceiling.
//
// Unrecognised values fail closed, and asymmetrically, because the two sides
// mean opposite things. Registration validates a declaration, but the durable
// decoder reads whatever strings are stored, so a malformed or tampered
// descriptor can reach resolution without ever having passed validation.
//
// An unrecognised *declaration* is beyond every ceiling: it is an unproven
// claim and gets the most restrictive reading. An unrecognised *ceiling*
// permits nothing: it is a host that failed to declare its own configuration,
// and the least permissive reading is the empty set. Both directions resolve to
// refusing more, never less.
//
// The two guards overlap on purpose, and no single mutation of either is
// observable: subset semantics already refuse an unrecognised declaration
// against a valid ceiling, and an unrecognised ceiling member against a valid
// declaration. What neither the subset check nor one guard alone catches is a
// declaration and a ceiling that name the *same* unrecognised value, where
// containment would report it permitted. Removing both is caught;
// TestUnknownEffectsFailClosedAtResolution pins that case explicitly.
func (e Effects) exceeds(ceiling Effects) bool {
	for _, declared := range e {
		if declared.validate() != nil {
			return true
		}
	}
	for _, permitted := range ceiling {
		if permitted.validate() != nil {
			return len(e) > 0
		}
	}
	for _, declared := range e {
		if !ceiling.contains(declared) {
			return true
		}
	}
	return false
}

// digestBytes returns what this set contributes to the v1 registry mutation
// digest, or nil to contribute nothing at all. v1 is read, never written (see
// registryMutationDigestV1); v2 hashes digestOrder as a counted list instead.
//
// Two cases must reproduce exactly what the superseded encoding produced,
// because a v1 digest is embedded in the QueryDigest of every lifecycle
// receipt written before v2, where a changed digest reads as a divergent
// mutation. A heartbeat or revoke retry that spans an upgrade would then be
// rejected.
//
//   - The empty set contributes nothing. That is the explicit branch below.
//     Hashing an empty value is not the same thing: it still writes an
//     eight-byte length prefix and would change every pre-existing digest.
//   - Exactly {EffectMutatesExternal} must contribute the bare string the old
//     external class wrote. There is no branch for it, and deliberately so:
//     EffectMutatesExternal *is* that string, and joining a one-element set
//     yields it unchanged. The compatibility lives in the constant's wire
//     value, not in a special case here — which is the more robust place for
//     it, since a special case can be removed without anything failing.
//
// Any other set is new — no descriptor could have declared it before this
// change — so it is free to hash as its canonical comma-joined form.
func (e Effects) digestBytes() []byte {
	ordered := e.digestOrder()
	if len(ordered) == 0 {
		return nil
	}
	return []byte(strings.Join(ordered, ","))
}

// digestOrder returns the declared classes sorted and deduplicated, as both
// registry digest versions hash them.
//
// Sorted and deduplicated here rather than trusting the caller. The same
// classes declared in a different order are the same declaration and must
// not hash differently, and this is reached from the registry digests, which
// are handed a Mutation that has not necessarily been through
// canonicalCapabilities yet. Unknown values are not rejected here — they are
// refused at registration and at resolution — but they are ordered, so a
// tampered record still hashes deterministically.
func (e Effects) digestOrder() []string {
	ordered := make([]string, 0, len(e))
	seen := make(map[Effect]struct{}, len(e))
	for _, effect := range e {
		if _, duplicate := seen[effect]; duplicate {
			continue
		}
		seen[effect] = struct{}{}
		ordered = append(ordered, string(effect))
	}
	sort.Strings(ordered)
	return ordered
}

// equalEffects reports exact set equality over two canonical declarations.
//
// Order-sensitive on purpose. Every declaration that reaches a durable record
// has been through canonicalEffects, and ActionRecord.Validate refuses a stored
// one that is not canonically ordered, so two equal sets always compare equal
// here. Making it order-insensitive instead would mean accepting a record whose
// ordering says it was assembled by something that skipped canonicalisation.
func equalEffects(left, right Effects) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

// clone returns an independent copy, so a returned descriptor cannot be used
// to mutate registry state.
func (e Effects) clone() Effects {
	if len(e) == 0 {
		return nil
	}
	return append(Effects(nil), e...)
}

type Action struct {
	Name         string          `json:"name"`
	InputSchema  json.RawMessage `json:"input_schema"`
	OutputSchema json.RawMessage `json:"output_schema"`
	// Effects declares where this action's consequences land. The empty set
	// means they land nowhere this taxonomy names.
	Effects Effects `json:"effects,omitempty"`
	// RequiresApproval holds every new request for this action for a human
	// decision (#451). Enqueue and invoke refuse it outright, the pre-call
	// admission seam denies it durably, and the only way in is the approval
	// route, which materializes the queued record once an approver distinct
	// from the requester has approved that exact request.
	//
	// It gates new work only. A record queued before the flag was registered
	// is not reached by it — but registering the flag is a new generation, and
	// a queued record names the generation it was enqueued against, so such
	// records stop resolving for Pull, Claim and completion the moment the
	// flag lands. Delegation and re-registration cannot clear it: dropping it
	// widens authority, which capabilitiesSubset refuses.
	RequiresApproval bool `json:"requires_approval,omitempty"`
	// RequiresAttestation admits a claimant to this action only while it holds
	// a current verified executor attestation for the descriptor's executor
	// ref, covering the whole lease it is granted (#446). Every path that
	// grants a claim — dispatch Claim, ExtendClaim and the born-claimed
	// admission grant — goes through one gate (attestationCovers, applied in
	// applyClaim); completion, ambiguity reports, status and cancel are never
	// gated.
	//
	// It is accepted only on an action whose effects include
	// EffectMutatesExternal, and Register refuses it for an executor ref the
	// host has no attestation trust root for. Like approval it narrows in one
	// direction: a delegate or a later generation may add it and never drop
	// it (capabilitiesSubset). Registering it is a new generation; see
	// docs/executor-attestation.md, "Policy flip".
	RequiresAttestation bool `json:"requires_attestation,omitempty"`
}

// legacyEffect projects a set onto the superseded scalar spelling.
//
// That taxonomy had one axis — does this mutate anything outside Shoal — so
// the projection is exactly that question, with one case it cannot answer.
//
//	{}                              ""          declares nothing
//	{reads-corpus}                  ""          reading mutates nothing outside
//	{mutates-external}              "external"
//	{reads-corpus, mutates-external} "external"
//
// A set containing egress has no legacy value at all, and the two candidates
// are not equally wrong. Reporting "" would tell a client that an action
// shipping corpus content to a third party mutates nothing outside — true on
// the old axis, and exactly the silence this whole change exists to end.
// Reporting "external" overstates, and a client reading it treats the action as
// work Shoal dispatches rather than performs. Overstating is the direction that
// fails safe, so egress projects to "external".
func (e Effects) legacyEffect() Effect {
	if e.contains(EffectMutatesExternal) || e.contains(EffectEgressesContent) {
		return EffectMutatesExternal
	}
	return ""
}

// MarshalJSON emits both spellings.
//
// Accepting the old request spelling does not preserve the wire API on its own.
// The registry wire embeds []Capability directly, so a response that carries
// only "effects" is silently lossy to a client that has not been rebuilt: it
// unmarshals into its old model, finds no "effect" key, and reads the zero
// value — concluding that an action mutating or transmitting does neither. That
// is the safe-looking direction and therefore the dangerous one.
//
// Emitting the projection alongside the set means such a client is wrong only
// in the cautious direction, and a current client reads "effects" and is not
// wrong at all.
func (a Action) MarshalJSON() ([]byte, error) {
	type actionFields struct {
		Name         string          `json:"name"`
		InputSchema  json.RawMessage `json:"input_schema"`
		OutputSchema json.RawMessage `json:"output_schema"`
		Effects      Effects         `json:"effects,omitempty"`
		LegacyEffect Effect          `json:"effect,omitempty"`
		// Omitted when false, so a descriptor that does not require
		// approval marshals byte-for-byte as it did before the field
		// existed and descriptorDigest is unchanged for it.
		RequiresApproval bool `json:"requires_approval,omitempty"`
		// Omitted when false for the same reason, so every descriptor
		// without the requirement keeps its bytes and digest.
		RequiresAttestation bool `json:"requires_attestation,omitempty"`
	}
	return json.Marshal(actionFields{
		Name: a.Name, InputSchema: a.InputSchema,
		OutputSchema: a.OutputSchema, Effects: a.Effects,
		LegacyEffect:        a.Effects.legacyEffect(),
		RequiresApproval:    a.RequiresApproval,
		RequiresAttestation: a.RequiresAttestation,
	})
}

// UnmarshalJSON accepts the superseded scalar spelling of the effect field
// alongside the current set.
//
// The registry wire embeds []Capability directly, so these struct tags are the
// HTTP contract, not an internal detail. Renaming the field outright would
// reject every pre-upgrade registration at the transport — decodeRequest
// disallows unknown fields — before it could reach the durable compatibility
// path. A client that has not been rebuilt is not a malformed client.
//
// "effect": "external" becomes {EffectMutatesExternal} and "effect": "" becomes
// the empty set, which is what the same values decode to from a version-2
// durable record. Supplying both spellings is refused rather than merged: they
// would be two declarations of the same thing, and picking a winner silently
// would let a client believe it declared something it did not.
//
// Unknown fields stay refused. The decoder below re-applies the strictness the
// outer decoder cannot reach through a custom unmarshaler.
func (a *Action) UnmarshalJSON(data []byte) error {
	type actionFields struct {
		Name                string          `json:"name"`
		InputSchema         json.RawMessage `json:"input_schema"`
		OutputSchema        json.RawMessage `json:"output_schema"`
		Effects             Effects         `json:"effects"`
		LegacyEffect        *Effect         `json:"effect"`
		RequiresApproval    bool            `json:"requires_approval"`
		RequiresAttestation bool            `json:"requires_attestation"`
	}
	var fields actionFields
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fields); err != nil {
		return err
	}
	if fields.LegacyEffect != nil {
		if fields.Effects != nil {
			// Both spellings are accepted together only when they agree,
			// because that is what a client echoing one of our own responses
			// sends back — MarshalJSON emits the projection alongside the set.
			// Disagreement is two different declarations of the same thing, and
			// picking a winner silently would let a client believe it declared
			// something it did not.
			if *fields.LegacyEffect != fields.Effects.legacyEffect() {
				return shoal.NewError(shoal.ErrorInvalidArgument,
					"action declares effect and effects inconsistently; "+
						"supply only effects")
			}
		} else if *fields.LegacyEffect != "" {
			fields.Effects = Effects{*fields.LegacyEffect}
		}
	}
	*a = Action{
		Name: fields.Name, InputSchema: fields.InputSchema,
		OutputSchema: fields.OutputSchema, Effects: fields.Effects,
		RequiresApproval:    fields.RequiresApproval,
		RequiresAttestation: fields.RequiresAttestation,
	}
	return nil
}

type Capability struct {
	Name    string   `json:"name"`
	Actions []Action `json:"actions"`
}

// Descriptor is the immutable-at-a-generation durable agent description.
// Actor and Subject are supplied exclusively by the trusted auth decision.
type Descriptor struct {
	ID                  shoal.ID     `json:"id"`
	Generation          int64        `json:"generation"`
	Subject             shoal.ID     `json:"subject"`
	Actor               shoal.ID     `json:"actor"`
	ParentID            shoal.ID     `json:"parent_id,omitempty"`
	AuthorizationDomain []byte       `json:"authorization_domain"`
	Scopes              []Scope      `json:"scopes"`
	ExecutorRef         string       `json:"executor_ref"`
	Capabilities        []Capability `json:"capabilities"`
	LeaseExpiresAt      time.Time    `json:"lease_expires_at"`
	UpdatedAt           time.Time    `json:"updated_at"`
	RevokedAt           time.Time    `json:"revoked_at,omitempty"`
}

type Spec struct {
	ID                  shoal.ID     `json:"id"`
	ParentID            shoal.ID     `json:"parent_id,omitempty"`
	AuthorizationDomain []byte       `json:"authorization_domain"`
	Scopes              []Scope      `json:"scopes"`
	ExecutorRef         string       `json:"executor_ref"`
	Capabilities        []Capability `json:"capabilities"`
	LeaseExpiresAt      time.Time    `json:"lease_expires_at"`
}

type RequestContext struct {
	RequestID     shoal.ID  `json:"request_id"`
	CorrelationID shoal.ID  `json:"correlation_id,omitempty"`
	ReasonCode    string    `json:"reason_code"`
	ReasonDetail  string    `json:"reason_detail,omitempty"`
	Deadline      time.Time `json:"deadline"`
}

type RegisterRequest struct {
	Context            RequestContext `json:"context"`
	RegistrationKey    shoal.ID       `json:"registration_key"`
	ExpectedGeneration int64          `json:"expected_generation"`
	Spec               Spec           `json:"descriptor"`
}

type HeartbeatRequest struct {
	Context            RequestContext `json:"context"`
	RegistrationKey    shoal.ID       `json:"registration_key"`
	ID                 shoal.ID       `json:"id"`
	ExpectedGeneration int64          `json:"expected_generation"`
	LeaseExpiresAt     time.Time      `json:"lease_expires_at"`
}

type RevokeRequest struct {
	Context            RequestContext `json:"context"`
	RegistrationKey    shoal.ID       `json:"registration_key"`
	ID                 shoal.ID       `json:"id"`
	ExpectedGeneration int64          `json:"expected_generation"`
}

type ResolveRequest struct {
	Context RequestContext `json:"context"`
	ID      shoal.ID       `json:"id"`
}

type ListRequest struct {
	Context   RequestContext `json:"context"`
	SourceIDs [][]byte       `json:"source_ids,omitempty"`
	PolicyIDs [][]byte       `json:"policy_ids,omitempty"`
	Cursor    []byte         `json:"cursor,omitempty"`
	Limit     int            `json:"limit"`
}

const MaxListResults = 16

type ListPage struct {
	Descriptors []Descriptor
	Next        []byte
}

type Executor interface{}

// EffectBounded is the host's declaration of what an executor is permitted to
// do. An executor that does not implement it permits nothing, which is the
// conservative reading: a host that wants an executor to read the corpus,
// transmit content, or perform external work has to say so.
type EffectBounded interface {
	MaxEffects() Effects
}

// EffectFloored is an executor's declaration of what invoking it causes
// *regardless of the action*, and it exists because a ceiling alone cannot
// express that.
//
// A ceiling is an upper bound, so subset semantics permit an action to declare
// less than the truth. A reasoning executor configured against a hosted model
// transmits corpus content on every invocation, but an action declaring only
// {EffectReadsCorpus} is a subset of its ceiling and resolves happily — leaving
// a descriptor that reads as non-transmitting while every call transmits. That
// is the wrong direction to be wrong in, because egress leaves no trace in
// Shoal's own record for anyone to reconcile against later.
//
// An executor implementing this requires every action resolving to it to
// declare at least these classes.
type EffectFloored interface {
	MinEffects() Effects
}

// executorCeiling reports the effect classes an executor may serve.
func executorCeiling(executor Executor) Effects {
	if bounded, ok := executor.(EffectBounded); ok {
		return bounded.MaxEffects()
	}
	return nil
}

// executorFloor reports the effect classes invoking an executor always causes.
//
// The default is the empty set, not the ceiling. Defaulting to the ceiling
// would be the more suspicious reading, but it would also be wrong for the
// common case: a host binds a general-purpose external executor and declares
// the widest thing it permits, while individual actions legitimately do less.
// Forcing every action to restate the whole ceiling would make the declaration
// carry no information at all.
//
// So this is a declaration seam like the ceiling, and it has the same limit: an
// executor that transmits and declares no floor is a host misdescribing its own
// configuration, which no invariant here can detect. What it does close is the
// case where Shoal itself knows better — AskExecutor derives both bounds from
// the provider it was configured with, so it cannot be bound as transmitting
// and then have a non-transmitting action resolve to it.
func executorFloor(executor Executor) Effects {
	if floored, ok := executor.(EffectFloored); ok {
		return floored.MinEffects()
	}
	return nil
}

// omits reports whether this declaration leaves out anything a floor requires.
//
// It is the mirror of exceeds and fails closed the same way: an unrecognised
// value in the floor cannot be matched by any valid declaration, so it refuses.
// missingFrom lists the floor classes this declaration leaves out, for an
// error message that tells a registrant what to add.
func (e Effects) missingFrom(floor Effects) []string {
	var missing []string
	for _, required := range floor {
		if required.validate() != nil || !e.contains(required) {
			missing = append(missing, string(required))
		}
	}
	sort.Strings(missing)
	return missing
}

// externalClasses lists the declared classes that reach outside Shoal, sorted.
//
// The two of them are the whole of what EffectPossible is about: an action
// that neither mutates externally nor transmits leaves its whole outcome in
// Shoal's own record, and one that does either may have left a consequence
// somewhere Shoal cannot read. Reading the pair through one helper keeps the
// claim-time write, the registration requirement and the effect ceiling from
// drifting to different answers about which classes count.
func (e Effects) externalClasses() []string {
	var external []string
	for _, declared := range e {
		if declared == EffectMutatesExternal ||
			declared == EffectEgressesContent {
			external = append(external, string(declared))
		}
	}
	sort.Strings(external)
	return external
}

func (e Effects) omits(floor Effects) bool {
	for _, required := range floor {
		if required.validate() != nil {
			return true
		}
		if !e.contains(required) {
			return true
		}
	}
	return false
}

type ExecutorRegistry interface {
	ResolveExecutor(string) (Executor, bool)
}

type Resolved struct {
	Descriptor Descriptor
	Executor   Executor
}

func (r RequestContext) validate(now time.Time) error {
	if err := shoal.ValidateRequiredID("request ID", r.RequestID); err != nil {
		return err
	}
	if err := shoal.ValidateOptionalID("correlation ID", r.CorrelationID); err != nil {
		return err
	}
	if r.Deadline.IsZero() || r.Deadline.Location() != time.UTC ||
		!now.Before(r.Deadline) {
		return shoal.NewError(shoal.ErrorDeadline, "request deadline has elapsed")
	}
	if !time.Unix(0, r.Deadline.UnixNano()).UTC().Equal(r.Deadline) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"request deadline is outside the durable timestamp range",
		)
	}
	if r.ReasonCode == "" || len(r.ReasonCode) > MaxReasonCodeBytes ||
		strings.TrimSpace(r.ReasonCode) != r.ReasonCode {
		return shoal.NewError(shoal.ErrorInvalidArgument, "reason code is outside its bound")
	}
	return nil
}

func (s Spec) canonical(now time.Time) (Spec, error) {
	if err := shoal.ValidateRequiredID("agent ID", s.ID); err != nil {
		return Spec{}, err
	}
	if err := shoal.ValidateOptionalID("parent agent ID", s.ParentID); err != nil {
		return Spec{}, err
	}
	if len(s.AuthorizationDomain) == 0 || len(s.AuthorizationDomain) > 1024 {
		return Spec{}, shoal.NewError(shoal.ErrorInvalidArgument, "authorization domain is outside its bound")
	}
	if len(s.Scopes) == 0 || len(s.Scopes) > MaxScopes {
		return Spec{}, shoal.NewError(shoal.ErrorInvalidArgument, "agent scopes are outside their bound")
	}
	if err := executorref.ValidExecutorRef(s.ExecutorRef); err != nil {
		return Spec{}, shoal.NewError(shoal.ErrorInvalidArgument, err.Error())
	}
	if s.LeaseExpiresAt.IsZero() || s.LeaseExpiresAt.Location() != time.UTC ||
		!now.Before(s.LeaseExpiresAt) || s.LeaseExpiresAt.Sub(now) > MaxLease {
		return Spec{}, shoal.NewError(shoal.ErrorInvalidArgument, "agent lease is outside its bound")
	}
	result := s
	result.AuthorizationDomain = append([]byte(nil), s.AuthorizationDomain...)
	result.Scopes = make([]Scope, len(s.Scopes))
	for i, scope := range s.Scopes {
		if len(scope.SourceID) == 0 || len(scope.SourceID) > 1024 ||
			len(scope.PolicyID) == 0 || len(scope.PolicyID) > 1024 {
			return Spec{}, shoal.NewError(shoal.ErrorInvalidArgument, "agent scope identity is outside its bound")
		}
		result.Scopes[i] = Scope{
			SourceID: append([]byte(nil), scope.SourceID...),
			PolicyID: append([]byte(nil), scope.PolicyID...),
		}
	}
	sort.Slice(result.Scopes, func(i, j int) bool {
		if compared := bytes.Compare(result.Scopes[i].SourceID, result.Scopes[j].SourceID); compared != 0 {
			return compared < 0
		}
		return bytes.Compare(result.Scopes[i].PolicyID, result.Scopes[j].PolicyID) < 0
	})
	for i := 1; i < len(result.Scopes); i++ {
		if bytes.Equal(result.Scopes[i-1].SourceID, result.Scopes[i].SourceID) &&
			bytes.Equal(result.Scopes[i-1].PolicyID, result.Scopes[i].PolicyID) {
			return Spec{}, shoal.NewError(shoal.ErrorInvalidArgument, "agent scopes must be unique")
		}
	}
	capabilities, err := canonicalCapabilities(s.Capabilities)
	if err != nil {
		return Spec{}, err
	}
	result.Capabilities = capabilities
	encoded, err := json.Marshal(result)
	if err != nil || len(encoded) > MaxDescriptorBytes {
		return Spec{}, shoal.NewError(shoal.ErrorInvalidArgument, "agent descriptor exceeds its bound")
	}
	return result, nil
}

func canonicalCapabilities(input []Capability) ([]Capability, error) {
	if len(input) == 0 || len(input) > MaxCapabilities {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "capabilities are outside their bound")
	}
	result := make([]Capability, len(input))
	totalActions := 0
	for i, capability := range input {
		if err := validateName("capability", capability.Name); err != nil {
			return nil, err
		}
		if len(capability.Actions) == 0 {
			return nil, shoal.NewError(shoal.ErrorInvalidArgument, "capability actions are required")
		}
		result[i].Name = capability.Name
		result[i].Actions = make([]Action, len(capability.Actions))
		totalActions += len(capability.Actions)
		if totalActions > MaxActions {
			return nil, shoal.NewError(shoal.ErrorInvalidArgument, "actions exceed their bound")
		}
		for j, action := range capability.Actions {
			if err := validateName("action", action.Name); err != nil {
				return nil, err
			}
			inputSchema, err := canonicalSchema(action.InputSchema)
			if err != nil {
				return nil, err
			}
			outputSchema, err := canonicalSchema(action.OutputSchema)
			if err != nil {
				return nil, err
			}
			effects, err := canonicalEffects(action.Effects)
			if err != nil {
				return nil, err
			}
			// Attestation is a requirement on who may perform an external
			// effect. On an action that declares none it would gate nothing
			// Shoal does not already perform itself, and accepting it there
			// would let a descriptor read as attested while asserting nothing.
			if action.RequiresAttestation && !effects.contains(EffectMutatesExternal) {
				return nil, shoal.NewError(shoal.ErrorInvalidArgument,
					"action requires attestation but does not declare the "+
						"external effect; attestation applies only to external work")
			}
			result[i].Actions[j] = Action{
				Name: action.Name, InputSchema: inputSchema,
				OutputSchema: outputSchema, Effects: effects,
				RequiresApproval:    action.RequiresApproval,
				RequiresAttestation: action.RequiresAttestation,
			}
		}
		sort.Slice(result[i].Actions, func(a, b int) bool {
			return result[i].Actions[a].Name < result[i].Actions[b].Name
		})
		for j := 1; j < len(result[i].Actions); j++ {
			if result[i].Actions[j-1].Name == result[i].Actions[j].Name {
				return nil, shoal.NewError(shoal.ErrorInvalidArgument, "action names must be unique")
			}
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	for i := 1; i < len(result); i++ {
		if result[i-1].Name == result[i].Name {
			return nil, shoal.NewError(shoal.ErrorInvalidArgument, "capability names must be unique")
		}
	}
	return result, nil
}

func canonicalSchema(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || len(raw) > MaxSchemaBytes {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "action schema is outside its bound")
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "action schema is invalid JSON")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "action schema contains trailing JSON")
	}
	if _, ok := value.(map[string]any); !ok {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "action schema must be an object")
	}
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) > MaxSchemaBytes {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "action schema exceeds its bound")
	}
	return json.RawMessage(encoded), nil
}

func validateName(kind, value string) error {
	if value == "" || len(value) > MaxNameBytes || strings.TrimSpace(value) != value {
		return shoal.NewError(shoal.ErrorInvalidArgument, kind+" name is outside its bound")
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '_' || character == '-' || character == '.' || character == ':' {
			continue
		}
		return shoal.NewError(shoal.ErrorInvalidArgument, kind+" name contains an unsupported character")
	}
	return nil
}

func cloneDescriptor(input Descriptor) Descriptor {
	result := input
	result.AuthorizationDomain = append([]byte(nil), input.AuthorizationDomain...)
	result.Scopes = make([]Scope, len(input.Scopes))
	for i := range input.Scopes {
		result.Scopes[i] = Scope{
			SourceID: append([]byte(nil), input.Scopes[i].SourceID...),
			PolicyID: append([]byte(nil), input.Scopes[i].PolicyID...),
		}
	}
	result.Capabilities = make([]Capability, len(input.Capabilities))
	for i := range input.Capabilities {
		result.Capabilities[i].Name = input.Capabilities[i].Name
		result.Capabilities[i].Actions = make([]Action, len(input.Capabilities[i].Actions))
		for j := range input.Capabilities[i].Actions {
			action := input.Capabilities[i].Actions[j]
			result.Capabilities[i].Actions[j] = Action{
				Name:         action.Name,
				Effects:      action.Effects.clone(),
				InputSchema:  append(json.RawMessage(nil), action.InputSchema...),
				OutputSchema: append(json.RawMessage(nil), action.OutputSchema...),
				// A clone that dropped the flag would hand every reader of
				// a descriptor an action that no longer requires approval.
				RequiresApproval: action.RequiresApproval,
				// Likewise: a clone without it would hand every reader an
				// action whose claims no longer need an attestation.
				RequiresAttestation: action.RequiresAttestation,
			}
		}
	}
	return result
}

func descriptorDigest(descriptor Descriptor) [sha256.Size]byte {
	encoded, _ := json.Marshal(descriptor)
	return sha256.Sum256(encoded)
}

// RetrievalConfiner is an executor that confines what it retrieves to the
// scope of the invocation it was handed.
//
// A Descriptor declares Scopes as (SourceID, PolicyID) pairs and resolveAction
// gates every invocation on the action's scope matching one, which makes the
// scope read as a bound on what the agent can reach. For an in-process
// executor that retrieves, it was not: Invocation carries SourceID, PolicyID
// and ObjectID and nothing required an executor to use them, so an action
// scoped to one source could record evidence anchors, a snapshot pin and node
// identifiers from every source the *invoking principal* may read (#370).
//
// Declaring this is a promise about retrieval, not about authorization. It
// does not widen anything: the principal already could read what it reads.
// What it asserts is that the record will name only what the action's own
// scope covers, which is what an operator reading the descriptor believes.
//
// An executor that does not implement this, or returns false, is treated as
// unable to confine — so fleet refuses the invocation rather than running it,
// but only where the confinement would otherwise be a lie. See
// refuseUnconfinedRetrieval.
type RetrievalConfiner interface {
	ConfinesRetrievalToScope() bool
}
