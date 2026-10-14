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

import "github.com/phrocker/shoal-oss/pkg/shoal"

// ExternalEffectBinding is the host's declaration that a named executor
// reference may serve actions whose consequences land outside Shoal.
//
// It is the point at which Shoal stops containing an effect, and it is the
// only thing in this package that says so. Everything else here narrows: the
// ceiling refuses what a host did not grant, the floor refuses what a
// declaration understates, and an executor that declares neither permits
// nothing at all (see EffectBounded and executorCeiling). That default is
// right, and it is also why a gateway could not be registered before this
// existed — a reference the host merely *named* resolved to a placeholder
// implementing neither interface, so `exceeds(nil)` refused every non-empty
// declaration. An operator had no way to say "this reference is for work I
// accept Shoal will not perform".
//
// Two properties make it safe to have at all, and both are load-bearing.
//
// It performs nothing. There is deliberately no Execute method, so this type
// does not satisfy ActionExecutor and in-process invocation fails closed
// exactly as an unbound reference does. #381 makes the execution boundary
// enforceable by refusing to run external work in process; this binding names
// the alternative rather than reopening the door. Work reaching a reference
// bound here goes out over the dispatch queue — enqueue, pull, claim, perform
// elsewhere, report — and the report is what Shoal records.
//
// It has no floor. ExternalEffectBinding implements EffectBounded and *not*
// EffectFloored, which is the opposite choice from AskExecutor and for the
// opposite reason. For AskExecutor the bound is a description: Shoal knows
// what invoking it does, because it is the thing invoking it. Here the bound
// is a permission envelope and nothing more. One reference serves many
// registered actions performed by a worker Shoal does not run, so the host
// cannot honestly say what every one of them always does — and a floor equal
// to the ceiling would force every action to restate the whole ceiling, which
// is the failure executorFloor's comment describes: a declaration carrying no
// information. A descriptor may therefore declare less than this permits, and
// that is the point.
//
// What it does not do is make the effect contained. It is a declaration seam
// with the same limit as every other one here: a host that binds a reference
// for external mutation and points a worker at the wrong operational surface
// has misdescribed its own configuration, and no invariant in this package can
// detect that.
type ExternalEffectBinding struct {
	ceiling Effects
}

// ExternalEffectBinding is a ceiling and nothing else. The assertions pin both
// halves of that: it declares a ceiling, and it is not an executor.
//
// The negative cannot be written as a compile-time assertion, so
// TestAnExternalBindingPerformsNothingInProcess carries it. A future Execute
// method on this type would turn every reference an operator bound for
// dispatch into an in-process external executor, silently, and nothing in the
// enforcement path would notice.
var _ EffectBounded = (*ExternalEffectBinding)(nil)

// NewExternalEffectBinding declares a ceiling for a dispatch-only reference.
//
// The ceiling must name EffectMutatesExternal. That is not a tidiness check:
// this constructor is the one explicit opt-in an operator has to the only
// binding in the process that admits external work, and a call that reached it
// with a narrower set would produce something indistinguishable from an
// unbound placeholder while reading, at the call site and in the operator's
// values file, as a decision that had been made. A host wanting an
// evidence-only ceiling wants a different executor — one that can actually
// perform the work — not this.
//
// The set is canonicalised rather than taken as written, for the same reason
// registration canonicalises a declaration: an unrecognised class is refused
// here instead of reaching exceeds, where an unrecognised *ceiling* member
// permits nothing and would turn a typo into a reference that silently refuses
// everything registered against it.
func NewExternalEffectBinding(ceiling Effects) (*ExternalEffectBinding, error) {
	canonical, err := canonicalEffects(ceiling)
	if err != nil {
		return nil, err
	}
	if !canonical.contains(EffectMutatesExternal) {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"an external effect binding must declare external mutation; a "+
				"narrower ceiling permits nothing this reference could not "+
				"already serve unbound")
	}
	return &ExternalEffectBinding{ceiling: canonical}, nil
}

// MaxEffects reports the declared ceiling.
//
// The nil receiver reports the empty set, which permits nothing. A typed nil
// satisfies this interface and would otherwise panic inside registration,
// after the reference has already resolved; reporting the most restrictive
// reading instead keeps the failure a refusal.
//
// The result is a copy. The caller is enforcement code that only reads it
// today, but the ceiling is the host's declaration and handing out the backing
// array would let anything downstream widen it in place for every subsequent
// registration and resolution in the process.
func (b *ExternalEffectBinding) MaxEffects() Effects {
	if b == nil {
		return nil
	}
	return b.ceiling.clone()
}
