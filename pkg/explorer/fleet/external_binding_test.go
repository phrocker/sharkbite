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
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// transmittingExternalAction declares the wider pair: an external operation
// that also carries corpus content off the host. It is the declaration the two
// ceilings have to separate, so it has to be expressible on its own.
func transmittingExternalAction() []Capability {
	return []Capability{{Name: "deploy", Actions: []Action{{
		Name: "ship",
		Effects: Effects{
			EffectEgressesContent, EffectMutatesExternal,
		},
		InputSchema: anyObject, OutputSchema: anyObject,
	}}}}
}

func readsCorpusAction() []Capability {
	return []Capability{{Name: "deploy", Actions: []Action{{
		Name: "ship", Effects: Effects{EffectReadsCorpus},
		InputSchema: anyObject, OutputSchema: anyObject,
	}}}}
}

// TestAnExternalBindingAdmitsExternalWork is the acceptance criterion #436
// exists for. Before this binding there was no way to configure the shipped
// explorer such that a descriptor declaring {external} could register at all:
// the only bound executor deliberately excludes external mutation, and a
// reference the host merely allowlisted implemented neither bound, so
// exceeds(nil) refused every non-empty declaration.
//
// The second half is the half that is easy to get wrong. The binding has no
// floor, so a descriptor may declare *less* than it permits — an action that
// changes nothing outside Shoal is admitted against a reference bound for
// external work. A floor equal to the ceiling would force every action
// resolving here to restate the whole ceiling, which is a declaration carrying
// no information at all.
func TestAnExternalBindingAdmitsExternalWork(t *testing.T) {
	binding, err := NewExternalEffectBinding(Effects{EffectMutatesExternal})
	if err != nil {
		t.Fatalf("declare an external ceiling: %v", err)
	}
	ceiling, floor := executorCeiling(binding), executorFloor(binding)
	if err := validateDeclaredEffects(
		externalAction(), floor, ceiling); err != nil {
		t.Fatalf("an external binding must admit external work: %v", err)
	}
	// Declaring *less* than the ceiling is still allowed, and this is where
	// that has room: a two-class ceiling admits an action declaring one.
	wider, err := NewExternalEffectBinding(Effects{
		EffectMutatesExternal, EffectEgressesContent,
	})
	if err != nil {
		t.Fatalf("declare a two-class ceiling: %v", err)
	}
	if err := validateDeclaredEffects(externalAction(),
		executorFloor(wider), executorCeiling(wider)); err != nil {
		t.Fatalf("a descriptor must be allowed to declare less than the "+
			"ceiling permits; the binding carries no floor: %v", err)
	}

	// Declaring *nothing* is not. This used to be allowed, with
	// evidenceAction() standing in for it, and that is what let a
	// non-declaring action resolve to a dispatch-only reference and complete
	// with EffectPossible false while a remote worker did real external work
	// (#510, #514). An action that declares nothing asserts its whole outcome
	// is in Shoal's record, which is exactly what a reference bound for
	// external effects cannot promise.
	//
	// Note what this does *not* require: an external class specifically. A
	// dispatch-only action may legitimately reach outside nothing — a remote
	// worker reading Shoal's own corpus — and forcing it to claim an external
	// effect would be the same false claim pointing the other way.
	if err := validateDeclaredEffects(
		evidenceAction(), floor, ceiling); err == nil {
		t.Fatal("an action declaring no effects at all resolved to a " +
			"reference bound for external mutation, so its record will say " +
			"no external effect was declared for work Shoal never performs")
	}
	// And it is a ceiling of exactly {external}, not a general widening. An
	// action that reads the corpus is not doing less external work, it is
	// doing a different thing, and this reference was not bound for it.
	if err := validateDeclaredEffects(
		readsCorpusAction(), floor, ceiling); err == nil {
		t.Fatal("an external ceiling admitted a corpus read it does not name")
	}
}

// TestATransmittingExternalBindingIsASeparateDeclaration pins why there are
// two configuration lists rather than one.
//
// Egress and mutation do not order — the comment on the effect constants says
// so, and this is the case that depends on it. A reference bound for external
// mutation must refuse an action that also transmits corpus content off the
// host, or an operator who declared "this gateway changes something outside
// Shoal" would have silently also declared "and it may carry the corpus out
// with it".
func TestATransmittingExternalBindingIsASeparateDeclaration(t *testing.T) {
	mutating, err := NewExternalEffectBinding(Effects{EffectMutatesExternal})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateDeclaredEffects(transmittingExternalAction(),
		executorFloor(mutating), executorCeiling(mutating)); err == nil {
		t.Fatal("a mutation-only ceiling admitted an action that also " +
			"transmits corpus content off the host")
	}
	transmitting, err := NewExternalEffectBinding(Effects{
		EffectEgressesContent, EffectMutatesExternal,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateDeclaredEffects(transmittingExternalAction(),
		executorFloor(transmitting),
		executorCeiling(transmitting)); err != nil {
		t.Fatalf("the transmitting ceiling must admit it: %v", err)
	}
	// Still no floor on the wider one either, so a gateway reference serving
	// several registered operations does not force the non-transmitting ones
	// to claim egress they do not cause.
	if err := validateDeclaredEffects(externalAction(),
		executorFloor(transmitting),
		executorCeiling(transmitting)); err != nil {
		t.Fatalf("the transmitting ceiling must admit mutation alone: %v", err)
	}
}

// TestAnExternalBindingPerformsNothingInProcess is the property that makes the
// binding safe to offer at all, and it is a negative that no compile-time
// assertion can express.
//
// #381 makes the execution boundary enforceable by refusing to run external
// work in process. This binding names the alternative — dispatch — and must
// not quietly become the thing #381 refuses. An Execute method added to it
// later would turn every reference an operator bound for a gateway into an
// in-process external executor, with no refusal anywhere in the enforcement
// path, because satisfying ActionExecutor is the whole of what in-process
// invocation checks for.
//
// The absent floor is asserted the same way and for a related reason:
// implementing EffectFloored would make every action resolving here restate
// the ceiling, breaking the descriptors this binding exists to admit.
func TestAnExternalBindingPerformsNothingInProcess(t *testing.T) {
	binding, err := NewExternalEffectBinding(Effects{EffectMutatesExternal})
	if err != nil {
		t.Fatal(err)
	}
	var executor Executor = binding
	if _, performs := executor.(ActionExecutor); performs {
		t.Fatal("an external effect binding must not implement action " +
			"execution; Shoal dispatches this work and does not run it")
	}
	if _, floored := executor.(EffectFloored); floored {
		t.Fatal("an external effect binding must not declare a floor; one " +
			"reference serves actions Shoal does not run and cannot describe")
	}
	if floor := executorFloor(binding); len(floor) != 0 {
		t.Fatalf("floor = %v, want the empty set", floor)
	}
}

// TestAnExternalBindingRefusesACeilingThatPermitsNothingNew pins the
// constructor's one guard.
//
// This constructor is the single explicit opt-in to external work in the
// process. A call reaching it with a narrower set would build something
// indistinguishable from the unbound placeholder while reading, in the
// operator's values file and at the call site, as a decision that had been
// made — so the refusal is what keeps "I configured a gateway" and "this
// reference permits nothing" from being the same configuration.
func TestAnExternalBindingRefusesACeilingThatPermitsNothingNew(t *testing.T) {
	for _, refused := range []struct {
		name    string
		ceiling Effects
	}{
		{"nothing at all", nil},
		{"the empty set", Effects{}},
		{"evidence only", Effects{EffectReadsCorpus}},
		{"egress without mutation", Effects{EffectEgressesContent}},
		{
			"everything but mutation",
			Effects{EffectEgressesContent, EffectReadsCorpus},
		},
		{
			// An unrecognised ceiling member permits nothing in exceeds, so a
			// typo reaching the registry would be a reference that silently
			// refuses everything registered against it.
			"an unrecognised class",
			Effects{EffectMutatesExternal, Effect("external-ish")},
		},
	} {
		if _, err := NewExternalEffectBinding(refused.ceiling); err == nil {
			t.Fatalf("%s: a ceiling that does not name external mutation "+
				"must be refused", refused.name)
		} else if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
			t.Fatalf("%s: error code = %v", refused.name, err)
		}
	}
	// Canonicalised, not taken as written: the ceiling is compared member by
	// member against a declaration, so a duplicate or an unsorted set must not
	// produce a different binding from the same declaration.
	binding, err := NewExternalEffectBinding(Effects{
		EffectMutatesExternal, EffectEgressesContent, EffectMutatesExternal,
	})
	if err != nil {
		t.Fatalf("a duplicated, unsorted ceiling: %v", err)
	}
	want := Effects{EffectEgressesContent, EffectMutatesExternal}
	if got := binding.MaxEffects(); !got.equalForTest(want) {
		t.Fatalf("ceiling = %v, want %v", got, want)
	}
}

// TestAnExternalBindingDoesNotHandOutItsCeiling proves MaxEffects returns a
// copy. The ceiling is the host's declaration, made once during composition;
// handing out the backing array would let anything downstream widen it in
// place for every registration and resolution afterwards in the process.
func TestAnExternalBindingDoesNotHandOutItsCeiling(t *testing.T) {
	binding, err := NewExternalEffectBinding(Effects{EffectMutatesExternal})
	if err != nil {
		t.Fatal(err)
	}
	handed := binding.MaxEffects()
	handed[0] = EffectEgressesContent
	if got := binding.MaxEffects(); !got.equalForTest(
		Effects{EffectMutatesExternal}) {
		t.Fatalf("ceiling after a caller wrote to it = %v", got)
	}
	// Appending is the other direction, and it is the one that widens: a
	// returned slice with spare capacity would let a caller add a class the
	// host never declared.
	widened := append(binding.MaxEffects(), EffectEgressesContent)
	if got := binding.MaxEffects(); !got.equalForTest(
		Effects{EffectMutatesExternal}) {
		t.Fatalf("ceiling after a caller appended %v = %v", widened, got)
	}
}

// TestANilExternalBindingPermitsNothing covers the typed nil. fleet.Executor
// is an empty interface, so a nil *ExternalEffectBinding resolves, satisfies
// EffectBounded, and reaches MaxEffects — which must report the most
// restrictive reading rather than panic inside registration.
func TestANilExternalBindingPermitsNothing(t *testing.T) {
	var binding *ExternalEffectBinding
	if got := executorCeiling(binding); len(got) != 0 {
		t.Fatalf("a nil binding reported the ceiling %v", got)
	}
	if err := validateDeclaredEffects(externalAction(),
		executorFloor(binding), executorCeiling(binding)); err == nil {
		t.Fatal("a nil binding admitted external work")
	}
}

// TestServiceAdmitsExternalWorkAgainstAnExternalBinding reaches the
// enforcement through Service.Register and resolveActionBinding, which is
// where it actually lives. Every other test in this file calls
// validateDeclaredEffects directly, so a binding that satisfied the validator
// but not the real registration or resolution path would leave them all green.
//
// Resolution is the less obvious half. The effect ceiling is re-checked there
// as well as at registration, so a descriptor registered against this binding
// has to satisfy both — and the two resolution paths are where #381's boundary
// is actually drawn. resolveActionBinding admits the work, because claiming,
// cancelling, inspecting and completing an action do not need a runnable
// executor. resolveAction, the in-process path, refuses it, because the
// binding supplies no Execute. That pair is the arrangement: the ceiling says
// the work may be served here, and the missing implementation says it will not
// be served *in this process*.
func TestServiceAdmitsExternalWorkAgainstAnExternalBinding(t *testing.T) {
	now := time.Date(2026, 9, 6, 8, 0, 0, 0, time.UTC)
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	binding, err := NewExternalEffectBinding(Effects{EffectMutatesExternal})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(Config{
		Store: newMemoryStore(), Resolver: authority.Resolver(),
		Recorder: &memoryRecorder{}, Snapshots: fixedSnapshot{now},
		Executors: executorMap{"exec": binding},
		Clock:     func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	decision := effectDecision(t, "external-request")
	ctx := bindDecision(t, authority, decision)
	descriptor, err := service.Register(ctx,
		externalRegisterRequest(now, "external-request", "agent", "source-a"))
	if err != nil {
		t.Fatalf("register external work against an external binding: %v", err)
	}
	if got := descriptor.Capabilities[0].Actions[0].Effects; !got.equalForTest(
		Effects{EffectMutatesExternal}) {
		t.Fatalf("registered effects = %v", got)
	}

	_, action, resolved, err := service.resolveActionBinding(
		ctx, decision, descriptor.ID, descriptor.Generation,
		"search", "query", []byte("source-a"), []byte("policy"), "object",
		auth.OperationDispatch, now, true)
	if err != nil {
		t.Fatalf("resolve external work for dispatch: %v", err)
	}
	if !action.Effects.equalForTest(Effects{EffectMutatesExternal}) {
		t.Fatalf("resolved effects = %v", action.Effects)
	}
	// Resolution hands back the binding itself, and the dispatch path reads
	// ActionExecutor off it to decide whether it can run the work here.
	if _, performs := resolved.(ActionExecutor); performs {
		t.Fatal("resolution produced an in-process executor for external work")
	}
	// Which is what the in-process path then refuses. The refusal is the
	// alternative #381 points at, arriving as unavailable rather than as a
	// successful in-process external effect: this reference is reachable, and
	// only over the queue.
	if _, _, _, err := service.resolveAction(
		ctx, decision, descriptor.ID, descriptor.Generation,
		"search", "query", []byte("source-a"), []byte("policy"), "object",
		auth.OperationInvoke, now); err == nil {
		t.Fatal("in-process invocation resolved against a dispatch-only " +
			"external binding")
	} else if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("in-process refusal code = %v", err)
	}
}

// TestAnUnboundReferenceStillPermitsNothing is the other half of the
// acceptance criteria, and it is the one that would be easiest to break while
// adding this feature. A reference an operator allowlisted but did not name
// for external work must keep refusing it, through the real Register path.
//
// This is what makes the new configuration an opt-in rather than a default. If
// it ever stopped holding, every allowlisted reference in every existing
// deployment would start admitting external declarations on upgrade.
func TestAnUnboundReferenceStillPermitsNothing(t *testing.T) {
	now := time.Date(2026, 9, 6, 8, 0, 0, 0, time.UTC)
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	// The placeholder the explorer's own registry uses for an allowlisted but
	// unbound reference: a value that resolves and implements neither bound.
	service, err := NewService(Config{
		Store: newMemoryStore(), Resolver: authority.Resolver(),
		Recorder: &memoryRecorder{}, Snapshots: fixedSnapshot{now},
		Executors: executorMap{"exec": unboundedExecutor{}},
		Clock:     func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := bindDecision(t, authority, effectDecision(t, "external-request"))
	if _, err := service.Register(ctx, externalRegisterRequest(
		now, "external-request", "agent", "source-a")); err == nil {
		t.Fatal("an allowlisted but unbound reference admitted external work")
	}
}
