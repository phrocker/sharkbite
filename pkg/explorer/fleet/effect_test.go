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

package fleet

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

var anyObject = json.RawMessage(`{"type":"object"}`)

type ceilingExecutor struct{ ceiling Effects }

func (e ceilingExecutor) MaxEffects() Effects { return e.ceiling }

type unboundedExecutor struct{}

// everyEffect is the widest ceiling a host can declare.
func everyEffect() Effects {
	return Effects{
		EffectEgressesContent, EffectMutatesExternal, EffectReadsCorpus,
	}
}

func externalAction() []Capability {
	return []Capability{{Name: "deploy", Actions: []Action{{
		Name: "ship", Effects: Effects{EffectMutatesExternal},
		InputSchema: anyObject, OutputSchema: anyObject,
	}}}}
}

func evidenceAction() []Capability {
	return []Capability{{Name: "explorer.reason", Actions: []Action{{
		Name: "ask", InputSchema: anyObject, OutputSchema: anyObject,
	}}}}
}

// TestExternalEffectNeedsAnExternalCeiling is the boundary the README states:
// Shoal runs evidence-only work and dispatches the rest. An action declaring an
// external effect cannot resolve to an executor the host bound for evidence.
func TestExternalEffectNeedsAnExternalCeiling(t *testing.T) {
	if err := validateDeclaredEffects(externalAction(), nil, nil); err == nil {
		t.Fatal("an external action must not register against an evidence executor")
	}
	if err := validateDeclaredEffects(externalAction(), nil, everyEffect()); err != nil {
		t.Fatalf("an external ceiling must admit an external action: %v", err)
	}
	if err := validateDeclaredEffects(evidenceAction(), nil, nil); err != nil {
		t.Fatalf("evidence work must run under an evidence ceiling: %v", err)
	}
}

// TestUndeclaredExecutorIsEvidenceOnly pins the conservative default. A host
// that wants an executor to perform external work has to say so; forgetting to
// declare must not widen what an executor may run.
func TestUndeclaredExecutorIsEvidenceOnly(t *testing.T) {
	if got := executorCeiling(unboundedExecutor{}); len(got) != 0 {
		t.Fatalf("undeclared executor ceiling = %q", got)
	}
	if got := executorCeiling(ceilingExecutor{ceiling: everyEffect()}); got.exceeds(everyEffect()) {
		t.Fatalf("declared ceiling = %q", got)
	}
	if err := validateDeclaredEffects(externalAction(),
		executorFloor(unboundedExecutor{}),
		executorCeiling(unboundedExecutor{})); err == nil {
		t.Fatal("an undeclared executor must not admit external work")
	}
}

// TestAbsentEffectKeepsItsPreviousMeaning proves the zero value is
// evidence-only, so descriptors registered before this field existed continue
// to resolve exactly as they did.
func TestAbsentEffectKeepsItsPreviousMeaning(t *testing.T) {
	if len(Effects(nil)) != 0 {
		t.Fatal("evidence must be the zero value or existing descriptors change meaning")
	}
	canonical, err := canonicalCapabilities(evidenceAction())
	if err != nil {
		t.Fatal(err)
	}
	if len(canonical[0].Actions[0].Effects) != 0 {
		t.Fatalf("absent effect canonicalized to %q", canonical[0].Actions[0].Effects)
	}
}

// TestUnknownEffectFailsClosed proves a class nobody recognizes is refused
// rather than silently treated as the permissive or the restrictive one.
func TestUnknownEffectFailsClosed(t *testing.T) {
	_, err := canonicalCapabilities([]Capability{{
		Name: "c", Actions: []Action{{
			Name: "a", Effects: Effects{Effect("whatever")},
			InputSchema: anyObject, OutputSchema: anyObject,
		}},
	}})
	if err == nil {
		t.Fatal("an unknown effect class must be refused")
	}
}

// TestDelegationCannotWidenEffect proves effect is part of what delegation may
// narrow. Without it a child, or a later generation, could turn an
// evidence-only action into an external one and still pass the subset check
// that exists to stop widening, whenever its executor had an external ceiling.
func TestDelegationCannotWidenEffect(t *testing.T) {
	evidence := evidenceAction()
	external := []Capability{{Name: "explorer.reason", Actions: []Action{{
		Name: "ask", Effects: Effects{EffectMutatesExternal},
		InputSchema: anyObject, OutputSchema: anyObject,
	}}}}

	if capabilitiesSubset(external, evidence) {
		t.Fatal("a child must not widen an evidence-only action to external")
	}
	if !capabilitiesSubset(evidence, external) {
		t.Fatal("a child may narrow an external action to evidence-only")
	}
	if !capabilitiesSubset(evidence, evidence) ||
		!capabilitiesSubset(external, external) {
		t.Fatal("an unchanged effect must remain a subset of itself")
	}
}

// TestEffectSurvivesCloning proves the field is carried by cloneDescriptor.
// It was not, so a registered external action was returned to callers, and
// re-resolved, as evidence-only: the boundary was enforced once at
// registration and then silently dropped.
func TestEffectSurvivesCloning(t *testing.T) {
	descriptor := Descriptor{Capabilities: []Capability{{
		Name: "deploy", Actions: []Action{{
			Name: "ship", Effects: Effects{EffectMutatesExternal},
			InputSchema: anyObject, OutputSchema: anyObject,
		}},
	}}}
	clone := cloneDescriptor(descriptor)
	if got := clone.Capabilities[0].Actions[0].Effects; !got.equalForTest(Effects{EffectMutatesExternal}) {
		t.Fatalf("cloned effects = %v, want %v", got, Effects{EffectMutatesExternal})
	}
}

// TestMutationDigestSeparatesEffects proves an otherwise identical
// registration with a different effect is a different mutation. The digest
// omitted the field, so an exact replay could swap an evidence-only action for
// an external one under the same mutation identity and be treated as the same
// request.
func TestMutationDigestSeparatesEffects(t *testing.T) {
	mutation := func(effects Effects) Mutation {
		return Mutation{Descriptor: Descriptor{
			ID: "agent", Generation: 1,
			Capabilities: []Capability{{Name: "deploy", Actions: []Action{{
				Name: "ship", Effects: effects,
				InputSchema: anyObject, OutputSchema: anyObject,
			}}}},
		}}
	}
	for _, version := range registryDigestVersions {
		digest := version.digest
		evidence := digest(mutation(nil))
		external := digest(mutation(Effects{EffectMutatesExternal}))
		if evidence == external {
			t.Fatalf("%s: effect must change the mutation digest", version.name)
		}
		if digest(mutation(Effects{EffectMutatesExternal})) != external {
			t.Fatalf("%s: the digest must stay stable for an unchanged effect",
				version.name)
		}
		// Distinct classes are distinct mutations, and a wider set is
		// distinct from either — otherwise a replay could widen a declaration
		// under the same mutation identity.
		egress := digest(mutation(Effects{EffectEgressesContent}))
		both := digest(mutation(Effects{
			EffectEgressesContent, EffectMutatesExternal,
		}))
		if egress == external || both == external || both == egress ||
			egress == evidence {
			t.Fatalf("%s: distinct effect sets collided in the mutation digest",
				version.name)
		}
		// Declaration order is not part of the declaration.
		reordered := digest(mutation(Effects{
			EffectMutatesExternal, EffectEgressesContent,
		}))
		if reordered != both {
			t.Fatalf("%s: declaration order changed the mutation digest; "+
				"canonicalization must happen before hashing", version.name)
		}
	}
}

// TestExternalMutationDigestIsUnchangedAcrossTheSetUpgrade is the cross-upgrade
// half of the golden test below.
//
// Before this change an external action hashed the bare string "external".
// That value is embedded in the lifecycle QueryDigest, where a changed digest
// reads as a divergent mutation, so a heartbeat or revoke retry spanning the
// upgrade would be rejected. EffectMutatesExternal therefore keeps that exact
// wire string, and a set holding only it contributes exactly those bytes.
func TestExternalMutationDigestIsUnchangedAcrossTheSetUpgrade(t *testing.T) {
	declared := Effects{EffectMutatesExternal}
	if got := string(declared.digestBytes()); got != "external" {
		t.Fatalf("external-only digest bytes = %q, want %q: a heartbeat or "+
			"revoke retry spanning the upgrade would be rejected as a "+
			"divergent mutation", got, "external")
	}
	if Effects(nil).digestBytes() != nil {
		t.Fatal("an empty declaration must contribute no bytes at all: hashing " +
			"an empty value still writes an eight-byte length prefix and would " +
			"change every pre-existing digest")
	}
	// A set that could not have existed before is free to hash as itself.
	wider := Effects{EffectEgressesContent, EffectMutatesExternal}
	if got := string(wider.digestBytes()); got != "egresses-content,external" {
		t.Fatalf("wider set digest bytes = %q", got)
	}
}

// TestUnknownEffectsFailClosedAtResolution is the regression test for a
// high-severity hole. Registration validates a declared effect, but the durable
// decoder reads whatever string is stored, so a malformed or tampered
// descriptor can reach resolution having never passed validation. Comparing
// only against the exact external string made every unrecognized value
// permitted under an evidence-only ceiling, which inverts the purpose of the
// class.
func TestUnknownEffectsFailClosedAtResolution(t *testing.T) {
	unknown := Effects{Effect("something-nobody-defined")}
	external := Effects{EffectMutatesExternal}

	// An unrecognized declaration is an unproven claim: beyond every ceiling.
	for _, ceiling := range []Effects{nil, everyEffect(), unknown} {
		if !unknown.exceeds(ceiling) {
			t.Fatalf("unknown declaration was permitted under ceiling %v", ceiling)
		}
	}

	// The case neither subset containment nor either guard alone catches: a
	// declaration and a ceiling naming the same unrecognized value. Containment
	// alone would report it permitted, because the ceiling does contain it.
	if !unknown.exceeds(unknown) {
		t.Fatal("a declaration was permitted by a ceiling naming the same " +
			"unrecognized class: containment cannot distinguish an agreed-upon " +
			"unknown from a validated one")
	}

	// An unrecognized ceiling is a host that failed to declare itself: it
	// permits the empty set and nothing more.
	if Effects(nil).exceeds(unknown) {
		t.Fatal("a declaration of nothing must remain permitted under an unknown ceiling")
	}
	if !external.exceeds(unknown) {
		t.Fatal("an unknown ceiling must not authorize external work")
	}
	readsCorpus := Effects{EffectReadsCorpus}
	if !readsCorpus.exceeds(unknown) {
		t.Fatal("an unknown ceiling must not authorize corpus reads either")
	}

	// The recognized comparisons are plain subset semantics.
	if external.exceeds(nil) != true ||
		external.exceeds(external) != false ||
		Effects(nil).exceeds(nil) != false ||
		Effects(nil).exceeds(external) != false ||
		external.exceeds(everyEffect()) != false {
		t.Fatal("the recognized comparisons changed")
	}
}

// TestAnActionCannotUnderstateWhatItsExecutorAlwaysDoes is the acceptance
// criterion a ceiling alone cannot satisfy: an action declaring no egress must
// not resolve to an executor that transmits on every invocation.
//
// The ceiling is an upper bound, so subset semantics happily permit an action
// to declare less than the truth. The result would be a descriptor that reads
// as non-transmitting while every call transmits — and because egress leaves
// no trace in Shoal's own record, that descriptor is the only place anyone
// could have noticed.
func TestAnActionCannotUnderstateWhatItsExecutorAlwaysDoes(t *testing.T) {
	transmitting := flooredExecutor{
		floor: Effects{EffectEgressesContent, EffectReadsCorpus},
	}
	readsOnly := []Capability{{Name: "explorer.reason", Actions: []Action{{
		Name: "ask", Effects: Effects{EffectReadsCorpus},
		InputSchema: anyObject, OutputSchema: anyObject,
	}}}}

	// The ceiling check alone permits this: {reads-corpus} is a subset.
	if readsOnly[0].Actions[0].Effects.exceeds(transmitting.MaxEffects()) {
		t.Fatal("this test assumes the ceiling check permits the understatement")
	}
	if err := validateDeclaredEffects(readsOnly,
		executorFloor(transmitting), executorCeiling(transmitting)); err == nil {
		t.Fatal("an action declaring no egress registered against an executor " +
			"that transmits on every invocation")
	}

	// Declaring the truth is accepted.
	honest := []Capability{{Name: "explorer.reason", Actions: []Action{{
		Name: "ask", Effects: Effects{EffectEgressesContent, EffectReadsCorpus},
		InputSchema: anyObject, OutputSchema: anyObject,
	}}}}
	if err := validateDeclaredEffects(honest,
		executorFloor(transmitting), executorCeiling(transmitting)); err != nil {
		t.Fatalf("an honest declaration was refused: %v", err)
	}

	// An executor declaring no floor imposes none. That is a deliberate
	// default, documented at executorFloor: a host binding a general-purpose
	// executor declares the widest thing it permits, and forcing every action
	// to restate the whole ceiling would make the declaration carry nothing.
	if err := validateDeclaredEffects(readsOnly,
		executorFloor(ceilingExecutor{ceiling: everyEffect()}),
		everyEffect()); err != nil {
		t.Fatalf("a ceiling-only executor imposed a floor: %v", err)
	}
}

// TestOmitsFailsClosedOnAnUnrecognizedFloor mirrors exceeds: a floor value
// nobody recognizes cannot be satisfied by any declaration.
func TestOmitsFailsClosedOnAnUnrecognizedFloor(t *testing.T) {
	unknown := Effects{Effect("invented-by-a-tampered-record")}
	if !everyEffect().omits(unknown) {
		t.Fatal("an unrecognized floor was treated as satisfied")
	}
	// The case containment alone cannot catch: a declaration naming the same
	// unrecognized value as the floor. Membership would report it satisfied,
	// so only validating the floor's own values refuses it.
	if !unknown.omits(unknown) {
		t.Fatal("a declaration satisfied a floor by naming the same " +
			"unrecognized class: containment cannot distinguish an agreed-upon " +
			"unknown from a validated one")
	}
	if !Effects(nil).omits(Effects{EffectReadsCorpus}) {
		t.Fatal("an empty declaration satisfied a non-empty floor")
	}
	if everyEffect().omits(nil) {
		t.Fatal("an empty floor was treated as unsatisfiable")
	}
}

// TestTheLegacyEffectScalarIsStillAccepted covers the HTTP contract, not the
// durable one. The registry wire embeds []Capability directly, so these struct
// tags are the API: renaming the field outright rejects every pre-upgrade
// registration at the transport, because the decoder disallows unknown fields.
func TestTheLegacyEffectScalarIsStillAccepted(t *testing.T) {
	for _, probe := range []struct {
		name string
		body string
		want Effects
	}{
		{"legacy external", `{"name":"ship","effect":"external"}`,
			Effects{EffectMutatesExternal}},
		{"legacy evidence zero value", `{"name":"ship","effect":""}`, nil},
		{"current spelling", `{"name":"ship","effects":["external"]}`,
			Effects{EffectMutatesExternal}},
		{"neither", `{"name":"ship"}`, nil},
	} {
		var action Action
		if err := json.Unmarshal([]byte(probe.body), &action); err != nil {
			t.Fatalf("%s: %v", probe.name, err)
		}
		if !action.Effects.equalForTest(probe.want) {
			t.Fatalf("%s: effects = %v, want %v",
				probe.name, action.Effects, probe.want)
		}
	}

	// Both spellings at once is refused rather than merged. They would be two
	// declarations of the same thing, and picking a winner silently would let a
	// client believe it declared something it did not.
	var action Action
	err := json.Unmarshal(
		[]byte(`{"name":"ship","effect":"external","effects":["reads-corpus"]}`),
		&action)
	if err == nil {
		t.Fatal("a registration supplying both spellings was accepted")
	}

	// Unknown fields stay refused: a custom unmarshaler would otherwise lose
	// the strictness the outer decoder cannot reach through it.
	if err := json.Unmarshal([]byte(`{"name":"ship","effct":"external"}`), &action); err == nil {
		t.Fatal("an unknown field was accepted inside an action")
	}
}

// TestResponsesStillCarryTheLegacyEffectKey covers the other direction, which
// accepting the old request spelling does not address on its own.
//
// The registry wire embeds []Capability directly, so a response carrying only
// "effects" is silently lossy to a client that has not been rebuilt: it finds
// no "effect" key and reads the zero value, concluding that an action which
// mutates or transmits does neither. That is the safe-looking direction and
// therefore the dangerous one.
func TestResponsesStillCarryTheLegacyEffectKey(t *testing.T) {
	for _, probe := range []struct {
		name    string
		effects Effects
		legacy  string
	}{
		{"declares nothing", nil, ""},
		// Reading the corpus mutates nothing outside, which is exactly what the
		// old evidence value meant, so this projection is lossless.
		{"reads corpus", Effects{EffectReadsCorpus}, ""},
		{"external mutation", Effects{EffectMutatesExternal}, "external"},
		{"reads and mutates",
			Effects{EffectMutatesExternal, EffectReadsCorpus}, "external"},
		// Egress has no legacy value. Reporting "" would tell an old client
		// that an action shipping corpus content to a third party mutates
		// nothing outside — true on the old axis, and exactly the silence this
		// change exists to end. Overstating fails safe; understating does not.
		{"egress alone", Effects{EffectEgressesContent}, "external"},
		{"reads and egresses",
			Effects{EffectEgressesContent, EffectReadsCorpus}, "external"},
	} {
		encoded, err := json.Marshal(Action{Name: "ask", Effects: probe.effects})
		if err != nil {
			t.Fatalf("%s: %v", probe.name, err)
		}
		var decoded struct {
			Effect  *string  `json:"effect"`
			Effects []string `json:"effects"`
		}
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatalf("%s: %v", probe.name, err)
		}
		got := ""
		if decoded.Effect != nil {
			got = *decoded.Effect
		}
		if got != probe.legacy {
			t.Fatalf("%s: legacy effect = %q, want %q (body %s)",
				probe.name, got, probe.legacy, encoded)
		}
		if len(decoded.Effects) != len(probe.effects) {
			t.Fatalf("%s: current spelling = %v, want %v",
				probe.name, decoded.Effects, probe.effects)
		}
	}
}

// TestOurOwnResponseRoundTrips is why disagreement rather than presence is what
// the decoder refuses. MarshalJSON emits both spellings, so a client echoing a
// response back sends both, and refusing that outright would make our own
// output unusable as input.
func TestOurOwnResponseRoundTrips(t *testing.T) {
	original := Action{
		Name: "ask", InputSchema: anyObject, OutputSchema: anyObject,
		Effects: Effects{EffectEgressesContent, EffectReadsCorpus},
	}
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var returned Action
	if err := json.Unmarshal(encoded, &returned); err != nil {
		t.Fatalf("our own response was refused as input: %v", err)
	}
	if !returned.Effects.equalForTest(original.Effects) {
		t.Fatalf("round trip = %v, want %v", returned.Effects, original.Effects)
	}

	// Disagreement is still refused: two different declarations of the same
	// thing, where picking a winner silently would let a client believe it
	// declared something it did not.
	var mismatched Action
	err = json.Unmarshal(
		[]byte(`{"name":"ask","effect":"","effects":["external"]}`), &mismatched)
	if err == nil {
		t.Fatal("a response contradicting itself was accepted")
	}
}

// TestALegacyDescriptorStopsResolvingAgainstAFlooredExecutor pins a breaking
// change rather than a bug.
//
// A descriptor written under the superseded taxonomy declared the evidence zero
// value, which decodes to the empty set. Against an executor that now declares
// a floor it omits that floor and stops resolving until it is re-registered.
//
// That is the intended outcome. Such a descriptor genuinely understates what
// invoking it does, and it says nothing only because the taxonomy it was
// written under could not say anything else. Grandfathering it would keep
// exactly the descriptors the floor exists to reject — so this is tested, not
// worked around, and the PR says so instead of claiming a migration-free
// upgrade.
func TestALegacyDescriptorStopsResolvingAgainstAFlooredExecutor(t *testing.T) {
	legacy := []Capability{{Name: "explorer.reason", Actions: []Action{{
		Name: "ask", InputSchema: anyObject, OutputSchema: anyObject,
	}}}}
	transmitting := flooredExecutor{
		floor: Effects{EffectEgressesContent, EffectReadsCorpus},
	}

	err := validateDeclaredEffects(legacy,
		executorFloor(transmitting), executorCeiling(transmitting))
	if err == nil {
		t.Fatal("a descriptor declaring nothing resolved against an executor " +
			"that transmits on every invocation")
	}
	// The refusal has to name what is missing, or re-registering is guesswork.
	for _, class := range []string{"egresses-content", "reads-corpus"} {
		if !strings.Contains(err.Error(), class) {
			t.Fatalf("refusal does not name the missing %q: %v", class, err)
		}
	}

	// An executor that declares no floor is still unaffected *here*, and that
	// is deliberate rather than an oversight. A legacy descriptor declaring
	// nothing against a floorless external ceiling is now refused — but only
	// at registration, which is what
	// TestRegisterRefusesADispatchOnlyActionThatDeclaresNothing covers.
	// Resolution does not share this function: resolveAction in
	// dispatch_service.go checks exceeds and omits directly,
	// so already-stored descriptors keep resolving and are refused the next
	// time they are registered or updated. Breaking them at resolution would
	// strand in-flight work on an upgrade for a declaration the operator
	// cannot amend without re-registering.
	if err := validateDeclaredEffects(legacy,
		executorFloor(ceilingExecutor{ceiling: Effects{EffectReadsCorpus}}),
		Effects{EffectReadsCorpus}); err != nil {
		t.Fatalf("a legacy descriptor broke against an executor that reaches "+
			"outside nothing: %v", err)
	}
}

// TestAStoredLegacyDescriptorStillResolves is the other half of the migration
// promise above. The declares-nothing rule is a registration boundary, so an
// agent already in the store must keep running: an operator upgrading Shoal
// does not get a fleet that stops mid-action, only registrations that start
// being refused.
func TestAStoredLegacyDescriptorStillResolves(t *testing.T) {
	binding, err := NewExternalEffectBinding(everyEffect())
	if err != nil {
		t.Fatal(err)
	}
	stored := Action{Name: "ask", InputSchema: anyObject, OutputSchema: anyObject}
	if len(stored.Effects) > 0 {
		t.Fatal("the fixture declares effects, so it is not the legacy shape")
	}
	// These two are what resolveAction applies. Neither may refuse, or every
	// queued action against this binding fails on the upgrade.
	if stored.Effects.exceeds(executorCeiling(binding)) {
		t.Fatal("a stored descriptor declaring nothing stopped resolving " +
			"against an external ceiling, so in-flight work breaks on upgrade")
	}
	if stored.Effects.omits(executorFloor(binding)) {
		t.Fatal("a stored descriptor declaring nothing stopped resolving " +
			"against an unfloored binding")
	}
	// And registering the same shape is refused, which is the boundary that
	// moved. Without this the test would pass if the rule vanished entirely.
	if err := validateDeclaredEffects(
		[]Capability{{Name: "explorer.reason", Actions: []Action{stored}}},
		executorFloor(binding), executorCeiling(binding)); err == nil {
		t.Fatal("registering an action that declares nothing against an " +
			"external ceiling was accepted")
	}
}

type flooredExecutor struct{ floor Effects }

func (e flooredExecutor) MaxEffects() Effects { return e.floor }
func (e flooredExecutor) MinEffects() Effects { return e.floor }

// TestEffectClassesDoNotOrder is why this is a set rather than a ladder.
//
// Writing a local file mutates without transmitting; streaming a compartmented
// corpus to a hosted model transmits without mutating. Neither is a subset of
// the other, so there is no answer to "does egress outrank mutation" — and any
// ladder has to invent one.
func TestEffectClassesDoNotOrder(t *testing.T) {
	egress := Effects{EffectEgressesContent}
	mutation := Effects{EffectMutatesExternal}

	if !egress.exceeds(mutation) {
		t.Fatal("egress was permitted by a ceiling that only allows mutation")
	}
	if !mutation.exceeds(egress) {
		t.Fatal("mutation was permitted by a ceiling that only allows egress")
	}
	// Both are permitted by a ceiling that names both.
	both := Effects{EffectEgressesContent, EffectMutatesExternal}
	if egress.exceeds(both) || mutation.exceeds(both) || both.exceeds(both) {
		t.Fatal("a ceiling naming both classes refused one of them")
	}
	// And a ceiling naming both does not thereby permit a third class.
	readsCorpus := Effects{EffectReadsCorpus}
	if !readsCorpus.exceeds(both) {
		t.Fatal("a ceiling naming egress and mutation also permitted corpus reads")
	}
}

// TestCanonicalEffectsSortsAndDeduplicates pins the property the digest
// depends on: the same declared classes in a different order are the same
// declaration, and must not hash differently.
func TestCanonicalEffectsSortsAndDeduplicates(t *testing.T) {
	canonical, err := canonicalEffects(Effects{
		EffectMutatesExternal, EffectReadsCorpus, EffectMutatesExternal,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := Effects{EffectMutatesExternal, EffectReadsCorpus}
	if !canonical.equalForTest(want) {
		t.Fatalf("canonical = %v, want %v", canonical, want)
	}
	reordered, err := canonicalEffects(Effects{EffectReadsCorpus, EffectMutatesExternal})
	if err != nil {
		t.Fatal(err)
	}
	if !reordered.equalForTest(canonical) {
		t.Fatalf("declaration order changed the canonical form: %v vs %v",
			reordered, canonical)
	}
	if _, err := canonicalEffects(Effects{EffectReadsCorpus, "invented"}); err == nil {
		t.Fatal("an unknown class in a set was accepted")
	}
}

// TestUnknownEffectFromStorageIsRefusedAtRegistration pins the same rule
// through the narrowing comparison, which is the other place a stored value is
// compared rather than validated.
func TestUnknownEffectFromStorageIsRefusedAtRegistration(t *testing.T) {
	unknownCapabilities := []Capability{{Name: "explorer.reason", Actions: []Action{{
		Name: "ask", Effects: Effects{Effect("decoded-from-a-tampered-record")},
		InputSchema: anyObject, OutputSchema: anyObject,
	}}}}
	if err := validateDeclaredEffects(unknownCapabilities, nil, nil); err == nil {
		t.Fatal("an unknown declared effect must not register")
	}
	if err := validateDeclaredEffects(unknownCapabilities, nil, everyEffect()); err == nil {
		t.Fatal("an unknown declared effect must not register even at the widest ceiling")
	}
	if capabilitiesSubset(unknownCapabilities, evidenceAction()) {
		t.Fatal("an unknown effect must not pass the narrowing check")
	}
}

// externalRegisterRequest is registerRequest with the action declaring an
// external effect, so the request reaches Service.Register and exercises the
// real enforcement call rather than the validator underneath it.
func externalRegisterRequest(now time.Time, requestID, id, source string) RegisterRequest {
	request := registerRequest(now, requestID, id, "", source)
	request.Spec.Capabilities[0].Actions[0].Effects = Effects{EffectMutatesExternal}
	return request
}

// TestServiceRegisterEnforcesTheEffectCeiling reaches the enforcement through
// Service.Register. The other effect tests call validateDeclaredEffects and
// exceeds directly, so removing the call from Register would leave them all
// green while the boundary stopped existing.
func TestServiceRegisterEnforcesTheEffectCeiling(t *testing.T) {
	now := time.Date(2026, 9, 6, 8, 0, 0, 0, time.UTC)
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	newService := func(executor Executor) *Service {
		service, err := NewService(Config{
			Store: newMemoryStore(), Resolver: authority.Resolver(),
			Recorder: &memoryRecorder{}, Snapshots: fixedSnapshot{now},
			Executors: executorMap{"exec": executor},
			Clock:     func() time.Time { return now },
		})
		if err != nil {
			t.Fatal(err)
		}
		return service
	}
	decision := testDecision(
		t, "owner", "owner-actor", "effect-request", [][]byte{[]byte("source-a")})
	ctx := bindDecision(t, authority, decision)

	// An executor the host bound for evidence-only work must refuse an
	// external declaration at registration.
	evidenceOnly := newService(ceilingExecutor{ceiling: nil})
	if _, err := evidenceOnly.Register(ctx,
		externalRegisterRequest(now, "effect-request", "agent", "source-a"),
	); err == nil {
		t.Fatal("Register admitted an external action under an evidence ceiling")
	}

	// An undeclared executor is read as evidence-only, so it must refuse too.
	undeclared := newService(struct{}{})
	if _, err := undeclared.Register(ctx,
		externalRegisterRequest(now, "effect-request", "agent", "source-a"),
	); err == nil {
		t.Fatal("Register admitted an external action under an undeclared executor")
	}

	// A host that declared the wider ceiling admits it.
	external := newService(ceilingExecutor{ceiling: everyEffect()})
	descriptor, err := external.Register(ctx,
		externalRegisterRequest(now, "effect-request", "agent", "source-a"))
	if err != nil {
		t.Fatalf("Register refused an external action under an external ceiling: %v", err)
	}
	if got := descriptor.Capabilities[0].Actions[0].Effects; !got.equalForTest(Effects{EffectMutatesExternal}) {
		t.Fatalf("registered effects = %v, want %v", got, Effects{EffectMutatesExternal})
	}
}

// TestResolveActionRefusesAfterARebindToANarrowerCeiling is the case that
// motivated checking at resolution as well as registration. A descriptor
// registered while the host permitted external work must stop resolving once
// the reference is rebound to an evidence-only executor, without the executor
// being invoked.
func TestResolveActionRefusesAfterARebindToANarrowerCeiling(t *testing.T) {
	now := time.Date(2026, 9, 6, 8, 0, 0, 0, time.UTC)
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	store := newMemoryStore()
	// The registry is mutable, standing in for a host that rebinds a reference
	// between process starts while durable descriptors survive.
	executors := executorMap{"exec": ceilingExecutor{ceiling: everyEffect()}}
	service, err := NewService(Config{
		Store: store, Resolver: authority.Resolver(),
		Recorder: &memoryRecorder{}, Snapshots: fixedSnapshot{now},
		Executors: executors, Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	decision := effectDecision(t, "effect-request")
	ctx := bindDecision(t, authority, decision)
	descriptor, err := service.Register(ctx,
		externalRegisterRequest(now, "effect-request", "agent", "source-a"))
	if err != nil {
		t.Fatalf("register under the wider ceiling: %v", err)
	}

	// Rebind to an executor that both implements execution and is bound for
	// evidence-only work, so the refusal cannot be attributed to a missing
	// ActionExecutor.
	tracked := &countingActionExecutor{ceiling: nil}
	executors["exec"] = tracked

	_, _, _, err = service.resolveAction(
		ctx, decision, descriptor.ID, descriptor.Generation,
		"search", "query", []byte("source-a"), []byte("policy"), "object",
		auth.OperationInvoke, now)
	if err == nil {
		t.Fatal("resolution succeeded against a narrower ceiling")
	}
	if tracked.calls != 0 {
		t.Fatalf("executor was invoked %d times despite the refusal", tracked.calls)
	}

	// The floor is re-checked at resolution for the same reason, and it is the
	// direction registration cannot have caught: rebinding to an executor that
	// now transmits on every invocation leaves a live descriptor whose
	// declaration understates what running it does. Its ceiling is wide enough
	// to keep the action within bounds, so only the floor refuses it.
	widened := &countingActionExecutor{
		ceiling: everyEffect(),
		floor:   Effects{EffectEgressesContent},
	}
	executors["exec"] = widened

	_, _, _, err = service.resolveAction(
		ctx, decision, descriptor.ID, descriptor.Generation,
		"search", "query", []byte("source-a"), []byte("policy"), "object",
		auth.OperationInvoke, now)
	if err == nil {
		t.Fatal("resolution succeeded against an executor that now transmits " +
			"on every invocation, under a descriptor declaring no egress")
	}
	if widened.calls != 0 {
		t.Fatalf("executor was invoked %d times despite the refusal", widened.calls)
	}
}

// countingActionExecutor implements execution and records whether it ran, so a
// refusal can be distinguished from a silent success.
type countingActionExecutor struct {
	ceiling Effects
	floor   Effects
	calls   int
}

func (e *countingActionExecutor) MaxEffects() Effects { return e.ceiling }
func (e *countingActionExecutor) MinEffects() Effects { return e.floor }

func (e *countingActionExecutor) Execute(
	context.Context, Invocation,
) (ExecutionResult, error) {
	e.calls++
	return ExecutionResult{}, nil
}

// effectDecision is testDecision plus OperationInvoke, which resolveAction
// authorizes and the shared helper does not grant.
func effectDecision(t *testing.T, requestID string) auth.Decision {
	t.Helper()
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "owner", Actor: "owner-actor",
		AuthorizationDomain: []byte("domain"),
		AllowedOperations: []auth.Operation{
			auth.OperationAgentRegister, auth.OperationAgentResolve,
			auth.OperationInvoke, auth.OperationDispatch,
		},
		PermittedSourceIDs:    [][]byte{[]byte("source-a")},
		PermittedPolicyIDs:    [][]byte{[]byte("policy")},
		PolicyGeneration:      1,
		AuthenticationExpires: time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC),
		RequestID:             shoal.ID(requestID),
	})
	if err != nil {
		t.Fatal(err)
	}
	return decision
}

// TestEvidenceMutationDigestIsUnchangedAcrossTheUpgrade pins the byte-level
// stability of an evidence-only v1 mutation digest. The value below was
// computed from main before the effect field existed.
//
// It matters because a v1 digest is embedded in the QueryDigest of every
// receipt written before v2 (#521), where a changed digest reads as a
// divergent mutation, so the legacy reader must reproduce it exactly. Hashing
// the zero value would have changed every existing mutation, since an empty
// field still contributes its eight-byte length prefix.
func TestEvidenceMutationDigestIsUnchangedAcrossTheUpgrade(t *testing.T) {
	const beforeTheEffectField = "990a0fe870e3c27d873e281441bcb7cb" +
		"cf299cf6dd5c6a5060d3dda992d5512f"
	digest := registryMutationDigestV1(Mutation{Descriptor: Descriptor{
		ID: "agent", Generation: 1,
		Capabilities: []Capability{{Name: "deploy", Actions: []Action{{
			Name: "ship", InputSchema: anyObject, OutputSchema: anyObject,
		}}}},
	}})
	if got := hex.EncodeToString(digest[:]); got != beforeTheEffectField {
		t.Fatalf("evidence-only mutation digest changed\n got  %s\n want %s",
			got, beforeTheEffectField)
	}
}

// equalForTest compares two declarations elementwise. Production code never
// needs this — sets are compared by exceeds — so it lives here.
func (e Effects) equalForTest(other Effects) bool {
	if len(e) != len(other) {
		return false
	}
	for i := range e {
		if e[i] != other[i] {
			return false
		}
	}
	return true
}

// TestRegisterRefusesADispatchOnlyActionThatDeclaresNothing reaches the
// declares-nothing requirement through Service.Register, because that is the
// boundary that matters: the repro in #510 was a descriptor that registered
// cleanly, resolved to a reference the host had bound for external effects,
// and completed with EffectPossible false while a remote worker did real
// external work. Registration is where that becomes stored state that looks
// operable, so refusing anywhere later would leave the record already written.
//
// The requirement is narrow on purpose. It asks the action to declare
// *something*, not to declare an external class: a dispatch-only reference may
// legitimately reach outside nothing — a remote worker reading Shoal's own
// corpus — and forcing an external declaration there would make EffectPossible
// true where it should be false, which is the false claim #538 removed.
func TestRegisterRefusesADispatchOnlyActionThatDeclaresNothing(t *testing.T) {
	now := time.Date(2026, 9, 6, 8, 0, 0, 0, time.UTC)
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	// A dispatch-only binding: a ceiling that reaches outside, and no floor.
	binding, err := NewExternalEffectBinding(everyEffect())
	if err != nil {
		t.Fatal(err)
	}
	if floor := executorFloor(binding); len(floor) > 0 {
		t.Fatalf("the binding grew a floor (%v), so this test no longer "+
			"covers the unfloored case it was written for", floor)
	}
	register := func(effects Effects) error {
		service, err := NewService(Config{
			Store: newMemoryStore(), Resolver: authority.Resolver(),
			Recorder: &memoryRecorder{}, Snapshots: fixedSnapshot{now},
			Executors: executorMap{"exec": binding},
			Clock:     func() time.Time { return now },
		})
		if err != nil {
			t.Fatal(err)
		}
		decision := testDecision(t, "owner", "owner-actor", "effect-request",
			[][]byte{[]byte("source-a")})
		request := registerRequest(now, "effect-request", "agent", "", "source-a")
		request.Spec.Capabilities[0].Actions[0].Effects = effects
		_, err = service.Register(
			bindDecision(t, authority, decision), request)
		return err
	}

	if err := register(nil); err == nil {
		t.Fatal("Register admitted an action declaring no effects against a " +
			"reference the host bound for external work, so its completions " +
			"will report EffectPossible false for work Shoal never performed")
	} else if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("refusal = %v, want invalid argument", err)
	}

	// Each class on its own is accepted, including the one that reaches
	// outside nothing. This is what keeps the requirement from being a
	// disguised "must declare external".
	for _, alone := range []Effect{
		EffectMutatesExternal, EffectEgressesContent, EffectReadsCorpus,
	} {
		if err := register(Effects{alone}); err != nil {
			t.Fatalf("Register refused an action declaring only %q against a "+
				"ceiling that permits it: %v", alone, err)
		}
	}
}
