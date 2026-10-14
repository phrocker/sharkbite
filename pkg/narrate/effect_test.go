// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package narrate

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
)

// EffectPossible on a record, as #538 defines it and the renderer reads it:
//
//   - claimed, set: an effect may already have happened (unchanged);
//   - any terminal state, set: an effect may have occurred. Never that it
//     did: the flag is monotonic, so even a request that never left reads
//     set;
//   - succeeded or failed, clear: the action DECLARED no external or egress
//     effect. Not that none happened: a descriptor may declare less than its
//     binding permits, so a remote worker on a non-declaring action can still
//     do real work. Every sentence is attributed to the declaration and
//     conditional on it. Only a success with no code says its whole outcome
//     is in the record; a failure keeps reconciliation (fleet assigns even
//     its own codes after the worker acted), and a gateway code contradicts
//     the declaration.
//     No record written before #538 has a clear flag here: Validate required
//     it on both states;
//   - canceled, clear: nothing is said. The record may never have been
//     claimed, and a clear flag there says only that no claim set it.
//
// Only the flag can make a record anything but possible. A declaration on the
// record (an admission's admitted effects) is never read for this.

const (
	effectDeclaredKey     = "dispatch.effect.declared_none"
	effectUnsureKey       = "dispatch.effect.declared_none_unsure"
	effectContradictedKey = "dispatch.gap.effect_contradicted"
	effectMayKey          = "dispatch.gap.effect_may"
	effectOpenKey         = "dispatch.gap.effect_possible"
)

// sourceCodeFamilies maps every gateway and fleet error code, read from their
// owners' source rather than from this package's mirror, to its family.
func sourceCodeFamilies(t *testing.T) map[string]string {
	t.Helper()
	families := map[string]string{}
	codes, prefix := sourceGatewayCodes(t)
	for status := 400; status <= 599; status++ {
		codes = append(codes, fmt.Sprintf("%s%03d", prefix, status))
	}
	for _, code := range codes {
		families[code] = "gateway"
	}
	for _, code := range sourceFleetErrorCodes(t) {
		families[code] = "fleet"
	}
	return families
}

// effectKeys returns which effect sentences a rendering carries.
func effectKeys(sentences []Sentence) []string {
	var keys []string
	for _, s := range sentences {
		switch s.Key {
		case effectDeclaredKey, effectUnsureKey, effectContradictedKey, effectMayKey, effectOpenKey:
			keys = append(keys, s.Key)
		}
	}
	sort.Strings(keys)
	return keys
}

func TestEffectPossibleOnEveryState(t *testing.T) {
	r := New(nil)
	for _, state := range sourceDispatchStates(t) {
		for _, kind := range []string{"action", "admission"} {
			build := action
			if kind == "admission" {
				if state == string(fleet.DispatchQueued) {
					continue // an admission is never queued
				}
				build = admission
			}
			for _, flag := range []bool{true, false} {
				what := fmt.Sprintf("%s %s effect_possible=%v", kind, state, flag)
				record := build(fleet.DispatchState(state))
				record.EffectPossible = flag
				sentences, err := r.Action(record, Options{})
				if err != nil {
					t.Fatal(err)
				}
				var want []string
				switch fleet.DispatchState(state) {
				case fleet.DispatchClaimed:
					if flag {
						want = []string{effectOpenKey}
					}
				case fleet.DispatchSucceeded:
					want = []string{effectDeclaredKey}
					if flag {
						want = []string{effectMayKey}
					}
				case fleet.DispatchFailed:
					// The fixture fails with outcome_unknown, a gateway
					// code, which contradicts a clear flag.
					want = []string{effectContradictedKey}
					if flag {
						want = []string{effectMayKey}
					}
				case fleet.DispatchCanceled:
					if flag {
						want = []string{effectMayKey}
					}
				}
				if got := effectKeys(sentences); strings.Join(got, ",") != strings.Join(want, ",") {
					t.Errorf("%s: effect sentences %v, want %v", what, got, want)
				}
				for _, s := range sentences {
					text := strings.ToLower(s.Text)
					if flag && strings.Contains(text, "declares no external") {
						t.Errorf("%s: a set flag is rendered as a declaration of none: %s", what, s.Text)
					}
					if s.Key == effectMayKey && !strings.Contains(text, "may have occurred") {
						t.Errorf("%s: a possible effect is not hedged: %s", what, s.Text)
					}
					if s.Key == effectDeclaredKey && !strings.Contains(text, "if that declaration is accurate") {
						t.Errorf("%s: the declaration is not conditional: %s", what, s.Text)
					}
				}
			}
		}
	}
}

// TestEffectIsNeverInferredFromDeclarations: only the flag can say the action
// declared no effect. A record whose declared effects include neither
// external mutation nor egress, with the flag set, still reads "may" — a
// pre-#538 record of a non-declaring action looks exactly like this, and so
// does any record whose declaration the renderer cannot see (a dispatched
// action's record does not carry its action's effects at all).
func TestEffectIsNeverInferredFromDeclarations(t *testing.T) {
	r := New(nil)
	declarations := map[string]fleet.Effects{
		"none recorded":     nil,
		"reads-corpus only": {fleet.EffectReadsCorpus},
	}
	for name, effects := range declarations {
		for _, state := range []fleet.DispatchState{fleet.DispatchSucceeded, fleet.DispatchFailed} {
			record := action(state)
			if state == fleet.DispatchFailed {
				record.ErrorCode, record.ErrorCodeOrigin = "executor_error", fleet.ErrorCodeOriginService
			}
			record.AdmittedEffects = effects
			record.EffectPossible = true
			what := fmt.Sprintf("%s declared=%s", state, name)
			sentences, err := r.Action(record, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if got := effectKeys(sentences); strings.Join(got, ",") != effectMayKey {
				t.Errorf("%s: effect sentences %v, want only %s", what, got, effectMayKey)
			}
			for _, s := range sentences {
				text := strings.ToLower(s.Text)
				if strings.Contains(text, "declares no external") || strings.Contains(text, "no reconciliation") {
					t.Errorf("%s: no effect inferred from the declaration: %s", what, s.Text)
				}
			}
		}
	}
}

// pages renders every action page this package can be asked for that bears
// on EffectPossible: every state as an action and an admission with the flag
// set and clear, and every failure code (gateway, fleet and free text) under
// every origin with the flag set and clear — which includes the shape #541's
// review reproduced: effects nil, claimed, then failed by the executor with
// target_rejected_409, stored as failed/executor/EffectPossible=false.
func pages(t *testing.T, r *Renderer) map[string]fleet.ActionRecord {
	t.Helper()
	out := map[string]fleet.ActionRecord{}
	for _, state := range DispatchStates {
		for _, build := range []func(fleet.DispatchState) fleet.ActionRecord{action, admission} {
			for _, flag := range []bool{true, false} {
				record := build(state)
				record.EffectPossible = flag
				out[fmt.Sprintf("%s admission=%v effect_possible=%v", state, isAdmission(record), flag)] = record
			}
		}
	}
	for _, origin := range errorCodeOriginCases(t) {
		for _, code := range append(sourceErrorCodes(t), "made up by an executor") {
			for _, flag := range []bool{true, false} {
				record := failedWith(code, origin)
				record.EffectPossible = flag
				out[fmt.Sprintf("failed code=%s origin=%s effect_possible=%v", code, origin, flag)] = record
			}
		}
	}
	return out
}

// TestNoPageBothContactsATargetAndDropsReconciliation: a gateway code says a
// request to a target was bound or attempted, so a page carrying one may not
// also say no reconciliation is needed, whatever the flag says. More broadly,
// nothing is dropped except on a success with no code: every failure keeps
// reconciliation, because even fleet's own codes are assigned after the
// worker acted (an output refused after a reported success, a worker's code
// refused as malformed, a failure reported without a code). And only a
// success says "its whole outcome is in this record": a refused output or
// evidence is discarded, so no failure's outcome is wholly in it.
func TestNoPageBothContactsATargetAndDropsReconciliation(t *testing.T) {
	r := New(nil)
	families := sourceCodeFamilies(t)
	failures, contacts := 0, 0
	for what, record := range pages(t, r) {
		sentences, err := r.Action(record, Options{Now: t0.Add(30 * time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
		contact := record.State == fleet.DispatchFailed && families[record.ErrorCode] == "gateway"
		if contact {
			contacts++
		}
		allowed := !record.EffectPossible && record.State == fleet.DispatchSucceeded && record.ErrorCode == ""
		reconciles := false
		for _, s := range sentences {
			text := strings.ToLower(s.Text)
			if s.Role == RoleNext && strings.Contains(text, "reconcile with the target") {
				reconciles = true
			}
			if strings.Contains(text, "whole outcome is in this record") && !allowed {
				t.Errorf("%s: %s says the whole outcome is in the record: %s", what, s.Key, s.Text)
			}
			if !strings.Contains(text, "no reconciliation") {
				continue
			}
			if contact {
				t.Errorf("%s: the code says a target was contacted, and %s drops reconciliation: %s",
					what, s.Key, s.Text)
			}
			if !allowed {
				t.Errorf("%s: %s drops reconciliation on a record that does not allow it: %s",
					what, s.Key, s.Text)
			}
		}
		if record.State == fleet.DispatchFailed {
			failures++
			if !reconciles {
				t.Errorf("%s: a failure's next step does not reconcile with the target", what)
			}
		}
		if contact && !record.EffectPossible && !hasKey(sentences, effectContradictedKey) {
			t.Errorf("%s: a gateway code on a clear flag does not say the declaration is contradicted", what)
		}
	}
	if failures == 0 || contacts == 0 {
		t.Fatalf("%d failures and %d target contacts checked", failures, contacts)
	}
}

// effectVerb finds a clause that says an effect, a request or content did
// something outside Shoal. hedge finds what makes such a clause not an
// assertion that it happened.
var (
	effectVerb = regexp.MustCompile(`\b(occurred|happened|took place|took effect|was (sent|written|transmitted|applied|performed|delivered|egressed)|were (sent|written|transmitted|applied|performed)|been (sent|written|transmitted|applied|performed)|reached|left the host|egressed)\b`)
	hedge      = regexp.MustCompile(`\b(may|might|whether|if|no|nothing|not|never|could not|cannot)\b`)
	clauseEnd  = regexp.MustCompile(`[.;:,—]`)
	// noEffect finds a clause saying no external effect was or could have
	// been had, or that nothing needs reconciling. Such a clause must be
	// attributed to the declaration or conditional on it: a clear flag says
	// only what the action declared.
	noEffect    = regexp.MustCompile(`\b(could not have|cannot have|couldn’t have|no (external|egress)( or egress)? effect|no reconciliation)\b`)
	conditioned = regexp.MustCompile(`\b(if|declares|declared|declaration)\b`)
	sentenceEnd = regexp.MustCompile(`[.;:—]`)
)

// affirmsEffect returns the clause of text that says an effect happened, or
// that one could not have, if any.
func affirmsEffect(text string) string {
	lower := strings.ToLower(text)
	for _, clause := range clauseEnd.Split(lower, -1) {
		if effectVerb.MatchString(clause) && !hedge.MatchString(clause) {
			return strings.TrimSpace(clause)
		}
	}
	for _, clause := range sentenceEnd.Split(lower, -1) {
		if noEffect.MatchString(clause) && !conditioned.MatchString(clause) {
			return strings.TrimSpace(clause)
		}
	}
	return ""
}

func TestEffectScannerCatchesAffirmations(t *testing.T) {
	for _, text := range []string{
		"An external effect occurred; reconcile with the target.",
		"The effect happened at the target.",
		"The request was sent, so reconcile with the target.",
		"Content left the host.",
		"This action could not have had an external effect; its whole outcome is in this record.",
		"The action had no external effect.",
		"No reconciliation with the target is needed before requesting the work again.",
	} {
		if affirmsEffect(text) == "" {
			t.Errorf("not caught: %q", text)
		}
	}
	for _, text := range []string{
		"An external effect may have occurred; reconcile with the target.",
		"This action declares no external or egress effect, so if that declaration is accurate its whole outcome is in this record.",
		"Fix the executor before requesting the work again; if the action’s declaration is accurate, no reconciliation with the target is needed.",
		"which, if accurate, means no byte of the request reached a connection.",
		"nothing establishes whether the effect happened.",
	} {
		if clause := affirmsEffect(text); clause != "" {
			t.Errorf("hedged text flagged: %q (%q)", text, clause)
		}
	}
}

// TestNoSentenceSaysAnEffectHappened scans every sentence the value goldens,
// the scenario goldens and every page above render, at every clock. Nothing
// Shoal records establishes that an external effect happened — a set flag
// says only that one may have — and nothing establishes that one could not
// have: a clear flag says only what the action declared. So no sentence may
// say either unconditionally.
func TestNoSentenceSaysAnEffectHappened(t *testing.T) {
	r := New(nil)
	scanned := 0
	scan := func(what string, sentences []Sentence, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		for _, s := range sentences {
			scanned++
			if clause := affirmsEffect(s.Text); clause != "" {
				t.Errorf("%s: %s claims an effect, or its absence, unconditionally (%q): %s",
					what, s.Key, clause, s.Text)
			}
		}
	}
	for _, family := range valueFamilies(t) {
		for _, c := range family.cases {
			sentences, err := c.run(r)
			scan(family.name+": "+c.name, sentences, err)
		}
	}
	for name, run := range goldenScenarios() {
		sentences, err := run(t, r)
		scan(name, sentences, err)
	}
	for what, record := range pages(t, r) {
		for _, now := range times() {
			sentences, err := r.Action(record, Options{Now: now, QuoteInput: true, QuoteOutput: true})
			scan(what, sentences, err)
		}
	}
	if scanned < 1000 {
		t.Fatalf("only %d sentences scanned", scanned)
	}
}
