// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package narrate

import (
	"encoding/json"
	"math"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Each source analyzer is run against synthetic code that hides a value the
// way a later change might, so a reader that misses the form fails here.

func TestAnalyzerTypedConstsReadsConversions(t *testing.T) {
	got, problems := typedConstsIn(parseSource(t, `package p
type T string
const (
	A T = "a"
	B   = T("b")
	C   = "untyped, not T"
)
const D = T("d")
`), "T")
	if len(problems) != 0 || len(got) != 3 || got["A"] != "a" || got["B"] != "b" || got["D"] != "d" {
		t.Fatalf("got %v, problems %v", got, problems)
	}
	_, problems = typedConstsIn(parseSource(t, `package p
type T string
const base = "x"
const E T = base + "y"
`), "T")
	if len(problems) != 1 {
		t.Fatalf("an unreadable constant was skipped: %v", problems)
	}
}

func TestAnalyzerErrorCodesResolvesConstants(t *testing.T) {
	codes, problems := errorCodesIn(parseSource(t, `package p
type R struct{ ErrorCode string }
const held = "held_by_const"
type Code string
const typed Code = "typed_const"
func f(r, other *R) {
	r.ErrorCode = "literal"
	r.ErrorCode = held
	r.ErrorCode = string(typed)
	r.ErrorCode = other.ErrorCode
	_ = R{ErrorCode: held}
}
`))
	sort.Strings(codes)
	if strings.Join(codes, ",") != "held_by_const,literal" {
		t.Errorf("codes %v", codes)
	}
	// string(typed) is a conversion of a constant through a call: unresolvable.
	if len(problems) != 1 {
		t.Errorf("problems %v", problems)
	}
	_, problems = errorCodesIn(parseSource(t, `package p
type R struct{ ErrorCode string }
func f(r *R, x string) {
	r.ErrorCode = x
	r.ErrorCode = "a" + x
	_ = R{ErrorCode: x}
}
`))
	if len(problems) != 3 {
		t.Errorf("unresolvable assignments were not reported: %v", problems)
	}
}

func TestAnalyzerClockCannotBeHidden(t *testing.T) {
	for name, src := range map[string]string{
		"direct":     `package p; import "time"; func f() { _ = time.Now() }`,
		"alias":      `package p; import clock "time"; func f() { _ = clock.Date(1, 1, 1, 0, 0, 0, 0, clock.UTC) }`,
		"alias call": `package p; import clock "time"; func f() { _ = clock.Now() }`,
		"dot":        `package p; import . "time"; func f() { _ = Now() }`,
		"since":      `package p; import "time"; func f(t time.Time) { _ = time.Since(t) }`,
		"timer":      `package p; import "time"; func f() { _ = time.NewTimer(1) }`,
		"afterfunc":  `package p; import "time"; func f() { time.AfterFunc(1, nil) }`,
		"ticker":     `package p; import "time"; func f() { _ = time.NewTicker(1) }`,
		"zone":       `package p; import "time"; func f() { _, _ = time.LoadLocation("x") }`,
		"local var":  `package p; import "time"; func f() { _ = time.Local }`,
		"to local":   `package p; import "time"; func f(t time.Time) { _ = t.Local() }`,
	} {
		if v := clockViolations(parseSource(t, src)); len(v) == 0 {
			t.Errorf("%s: clock read not detected", name)
		}
	}
	clean := `package p; import "time"; func f(t time.Time) { _ = time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC); _ = t.UTC() }`
	if v := clockViolations(parseSource(t, clean)); len(v) != 0 {
		t.Errorf("clean source flagged: %v", v)
	}
	// Selectors resolve by import path, not by the name at the call site: a
	// different package imported under the name "time" is not the clock, and
	// the time package under any other name is refused by the alias rule and
	// resolved by path as well.
	other := `package p; import time "example.com/notclock"; func f() { _ = time.Now() }`
	if v := clockViolations(parseSource(t, other)); len(v) != 0 {
		t.Errorf("selector resolved by name, not path: %v", v)
	}
	if v := clockViolations(parseSource(t, `package p; import clock "time"; func f() { _ = clock.Now() }`)); len(v) != 2 {
		t.Errorf("aliased clock read should be refused twice (alias and call): %v", v)
	}
}

func TestAnalyzerEffectiveStatePicksUpNewPairs(t *testing.T) {
	src := `package p
type ApprovalState string
type ApprovalCondition string
const (
	Pending ApprovalState = "pending"
	Refused ApprovalState = "refused"
	Held ApprovalState = "held"
	Unresolvable ApprovalState = "unresolvable"
)
const (
	None ApprovalCondition = ""
	Moved ApprovalCondition = "moved"
	Stuck ApprovalCondition = "stuck"
)
type S struct{}
func (s *S) unreachable() (ApprovalCondition, error) {
	if true { return Moved, nil }
	return None, nil
}
func (s *S) effectiveState(current struct{ State ApprovalState }) (ApprovalState, ApprovalCondition, error) {
	switch current.State {
	case Refused:
		return current.State, None, nil
	case Held:
		return Held, Stuck, nil
	}
	condition, err := s.unreachable()
	if err != nil { return "", "", err }
	if condition != None { return Unresolvable, condition, nil }
	return current.State, None, nil
}
`
	pairs, problems := effectiveStatePairs(parseSource(t, src))
	var got []string
	for _, p := range pairs {
		got = append(got, p[0]+"/"+p[1])
	}
	sort.Strings(got)
	if len(problems) != 0 || strings.Join(got, ",") != "held/stuck,pending/,refused/,unresolvable/moved" {
		t.Errorf("pairs %v, problems %v", got, problems)
	}
}

func TestAnalyzerDispatchDestinations(t *testing.T) {
	destinations, kinds, problems := dispatchDestinations(parseSource(t, `package p
type DispatchState string
const (
	DispatchQueued DispatchState = "queued"
	DispatchHeld   DispatchState = "held"
)
type ActionRecord struct{ State DispatchState }
func f(r *ActionRecord) {
	r.State = DispatchHeld
	_ = ActionRecord{State: DispatchQueued}
}
func actionEventKind(r ActionRecord) string {
	switch r.State {
	case DispatchQueued:
		return "action.enqueued"
	}
	return ""
}
`))
	if len(problems) != 0 || !destinations["held"] || !destinations["queued"] || kinds["queued"] != "action.enqueued" {
		t.Errorf("destinations %v kinds %v problems %v", destinations, kinds, problems)
	}
}

// Engine edge cases.

func TestEngineExtremes(t *testing.T) {
	if got := format(t, "{n, plural, one {#} other {# x}}", Args{"n": int64(math.MinInt64)}); got != "-9,223,372,036,854,775,808 x" {
		t.Errorf("MinInt64 plural: %q", got)
	}
	if got := format(t, "{n, number}", Args{"n": int64(math.MinInt64)}); got != "-9,223,372,036,854,775,808" {
		t.Errorf("MinInt64 number: %q", got)
	}
	if got := format(t, "{n, plural, one {one} other {other}}", Args{"n": int64(-1)}); got != "one" {
		t.Errorf("-1 plural: %q", got)
	}
	c := English()
	for _, d := range []time.Duration{-time.Nanosecond, -time.Hour, math.MinInt64} {
		if _, err := c.duration(d); err == nil {
			t.Errorf("negative duration %d rendered", d)
		}
	}
	// A record carrying a negative lease is refused, not misstated.
	record := action(fleet.DispatchClaimed)
	record.ClaimLease = -time.Minute
	if _, err := New(nil).Action(record, Options{}); err == nil {
		t.Error("a negative lease was narrated")
	}
}

func TestCatalogRefusesPluralOnProbability(t *testing.T) {
	var file CatalogFile
	if err := json.Unmarshal(englishCatalog, &file); err != nil {
		t.Fatal(err)
	}
	file.Messages["decision.answer.probability"] = "For {subject}, {question}: {p, plural, one {#} other {#}}."
	if _, err := ParseCatalog(mustJSON(t, file)); err == nil {
		t.Error("a plural over a probability was accepted")
	}
	// A count read as a number is still fine.
	file.Messages["decision.answer.probability"] = "For {subject}, {question}: {p, number}."
	file.Messages["dispatch.history.claims"] = "{claims, number} claims, {lapsed, number} lapsed."
	if _, err := ParseCatalog(mustJSON(t, file)); err != nil {
		t.Errorf("a count read as a number was refused: %v", err)
	}
}

// Quote.By and identifier spans.

func TestQuoteByIsEscapedAndBounded(t *testing.T) {
	by := "mallory‮\n\x1b[31m" + strings.Repeat("x", 500)
	fr := quoted(AttributedToCaller, by, "v", 0)
	got := fr.quotes[0].By
	if strings.ContainsAny(got, "‮\n\x1b") || len([]rune(got)) > maxByRunes+20 {
		t.Errorf("By not escaped or bounded: %q", got)
	}
	if fr.spans[0].By != got {
		t.Errorf("span By %q differs from quote By %q", fr.spans[0].By, got)
	}
	sentences, err := New(nil).AssertedReason(interaction.Session{
		ID:                   "s",
		Actor:                interaction.ActorContext{ActorID: shoal.ID(by)},
		CallerAssertedReason: interaction.CallerAssertedReason{Code: "c"},
	}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sentences {
		checkSentence(t, s)
	}
}

func TestIdentifierSpans(t *testing.T) {
	record := action(fleet.DispatchQueued)
	record.Actor, record.Action = "approved", "Nothing"
	sentences, err := New(nil).Action(record, Options{})
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, s := range sentences {
		checkSentence(t, s)
		for _, span := range s.Spans {
			if span.Kind == SpanIdentifier {
				found[s.Text[span.Start:span.End]] = true
			}
		}
	}
	for _, id := range []string{"approved", "Nothing", "agent-7"} {
		if !found[id] {
			t.Errorf("identifier %q has no span (found %v)", id, found)
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// A translation may read an argument only under the select arms English
// reads it under: outside them the renderer does not supply it, or supplies
// a placeholder.
func TestCatalogRefusesArgumentsOutsideTheirArm(t *testing.T) {
	load := func(key, pattern string) error {
		var file CatalogFile
		if err := json.Unmarshal(englishCatalog, &file); err != nil {
			t.Fatal(err)
		}
		file.Messages[key] = pattern
		_, err := ParseCatalog(mustJSON(t, file))
		return err
	}
	for key, pattern := range map[string]string{
		// {left} exists only when timed=yes: a render would fail.
		"approval.blocked.pending": "Waiting; {left, duration} remain.",
		// p is a placeholder 0 unless reported=yes: a fabricated probability.
		"decision.answer.choice": "{subject} {question}: {label} ({p, number}).",
		// Under the wrong arm of the right selector.
		"approval.blocked.approved": "Waiting{timed, select, yes {} other { {left, duration}}}.",
	} {
		if err := load(key, pattern); err == nil {
			t.Errorf("%s: %q was accepted", key, pattern)
		}
	}
	for key, pattern := range map[string]string{
		// Same arm, reworded, and nested more deeply still encloses it.
		"approval.blocked.pending":  "Waiting for an approver{timed, select, yes { ({left, duration} left)} other {}}.",
		"decision.answer.choice":    "{subject}/{question}: {label}{reported, select, yes {{reported, select, yes { p={p, number}} other {}}} other {}}.",
		"approval.blocked.approved": "Waiting.",
	} {
		if err := load(key, pattern); err != nil {
			t.Errorf("%s: %q was refused: %v", key, pattern, err)
		}
	}
}

func TestAnalyzersFailClosedOnUnreadForms(t *testing.T) {
	header := `package p
type ApprovalState string
type ApprovalCondition string
const (
	Pending ApprovalState = "pending"
	Unresolvable ApprovalState = "unresolvable"
)
const (
	None ApprovalCondition = ""
	Moved ApprovalCondition = "moved"
)
type S struct{}
`
	for name, body := range map[string]string{
		"helper return": `
func (s *S) unreachable() (ApprovalCondition, error) { return Moved, nil }
func (s *S) helper() (ApprovalState, ApprovalCondition, error) { return Pending, None, nil }
func (s *S) effectiveState(x bool) (ApprovalState, ApprovalCondition, error) {
	if x {
		return s.helper()
	}
	return Pending, None, nil
}
`,
		"unreachable variable": `
func (s *S) unreachable() (ApprovalCondition, error) { c := Moved; return c, nil }
func (s *S) effectiveState() (ApprovalState, ApprovalCondition, error) { return Pending, None, nil }
`,
		"unreachable helper": `
func (s *S) other() (ApprovalCondition, error) { return Moved, nil }
func (s *S) unreachable() (ApprovalCondition, error) { return s.other() }
func (s *S) effectiveState() (ApprovalState, ApprovalCondition, error) { return Pending, None, nil }
`,
	} {
		if _, problems := effectiveStatePairs(parseSource(t, header+body)); len(problems) == 0 {
			t.Errorf("effectiveState %s: no problem reported", name)
		}
	}
	dispatchHeader := `package p
type DispatchState string
const DispatchQueued DispatchState = "queued"
type ActionRecord struct{ State DispatchState }
func actionEventKind(r ActionRecord) string { switch r.State { case DispatchQueued: return "action.enqueued" }; return "" }
func pick() DispatchState { return DispatchQueued }
`
	for name, body := range map[string]string{
		"assign variable":  `func f(r *ActionRecord, next DispatchState) { r.State = next }`,
		"literal variable": `func f(next DispatchState) { _ = ActionRecord{State: next} }`,
		"assign call":      `func f(r *ActionRecord) { r.State = pick() }`,
		"elided literal":   `func f(next DispatchState) { _ = []ActionRecord{{State: next}} }`,
	} {
		if _, _, problems := dispatchDestinations(parseSource(t, dispatchHeader+body)); len(problems) == 0 {
			t.Errorf("dispatch %s: no problem reported", name)
		}
	}
	// Approval states assigned in the same package are another vocabulary
	// and are not problems; a copy of another State field carries a value
	// read where it was set.
	clean := dispatchHeader + `
type ApprovalState string
const Approved ApprovalState = "approved"
type ApprovalRecord struct{ State ApprovalState }
type ApprovalStatus struct{ State ApprovalState }
func f(r *ActionRecord, a *ApprovalRecord, other ActionRecord, s ApprovalState) {
	a.State = Approved
	r.State = other.State
	_ = ApprovalStatus{State: s}
	_ = ApprovalRecord{State: Approved}
}
`
	if _, _, problems := dispatchDestinations(parseSource(t, clean)); len(problems) != 0 {
		t.Errorf("clean dispatch source reported %v", problems)
	}
}

// An argument English reads with the same kind in every arm of a select is
// not restricted by it: a translation may hoist it out.
func TestCatalogAllowsHoistingSharedArguments(t *testing.T) {
	parse := func(edits map[string]string) (*Catalog, error) {
		var file CatalogFile
		if err := json.Unmarshal(englishCatalog, &file); err != nil {
			t.Fatal(err)
		}
		for k, v := range edits {
			file.Messages[k] = v
		}
		return ParseCatalog(mustJSON(t, file))
	}
	hoisted := map[string]string{
		"approval.outcome.expired":   "The request to run action {action} on agent {agent} {verdict, select, approve {was approved but expired at {expires, time} before it became work} other {expired at {expires, time} without a decision}}.",
		"dispatch.history.requested": "{requester} {admission, select, yes {requested admission} other {enqueued it}} at {created, time}, with a deadline of {deadline, time}.",
		"dispatch.outcome.canceled":  "{admission, select, yes {Admission of action} other {Action}} {action} on agent {agent} was {admission, select, yes {refused} other {canceled}} at {at, time}{admission, select, yes {; nothing was permitted} other {}}.",
		"dispatch.outcome.claimed":   "Action {action} on agent {agent} {admission, select, yes {was admitted for {effects, list} and has not reported an outcome} other {is claimed and has not reported an outcome}}.",
	}
	c, err := parse(hoisted)
	if err != nil {
		t.Fatalf("hoisted shared arguments refused: %v", err)
	}
	// It renders identically to English.
	english, translated := New(nil), New(c)
	cases := map[string]func(*Renderer) ([]Sentence, error){
		"approval expired undecided": func(r *Renderer) ([]Sentence, error) {
			return r.Approval(fleet.ApprovalStatus{Approval: approvalRecord(fleet.ApprovalExpired, ""), State: fleet.ApprovalExpired}, Options{})
		},
		"approval expired unused": func(r *Renderer) ([]Sentence, error) {
			return r.Approval(fleet.ApprovalStatus{Approval: approvalRecord(fleet.ApprovalExpired, fleet.ApprovalVerdictApprove), State: fleet.ApprovalExpired}, Options{})
		},
	}
	for _, state := range []fleet.DispatchState{fleet.DispatchClaimed, fleet.DispatchCanceled} {
		for _, build := range []func(fleet.DispatchState) fleet.ActionRecord{action, admission} {
			record := build(state)
			cases[string(state)+" admission="+string(yesNo(isAdmission(record)))] = func(r *Renderer) ([]Sentence, error) {
				return r.Action(record, Options{})
			}
		}
	}
	for name, run := range cases {
		want, err := run(english)
		if err != nil {
			t.Fatal(err)
		}
		got, err := run(translated)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if dump(got) != dump(want) {
			t.Errorf("%s renders differently:\n%s\nwant\n%s", name, dump(got), dump(want))
		}
	}
	// An argument English reads in only some arms stays restricted, and the
	// refusal names the arm.
	for key, tc := range map[string]struct{ pattern, want string }{
		"dispatch.outcome.claimed": {
			"Action {action} on agent {agent}, effects {effects, list}{admission, select, yes {.} other {.}}",
			`argument "effects": it is read outside select arm admission=yes, the only place English reads it`},
		"approval.blocked.pending": {
			"Waiting; {left, duration} remain.",
			`argument "left": it is read outside select arm timed=yes, the only place English reads it`},
		"dispatch.outcome.succeeded": {
			"{at, duration}",
			`argument "at": it is read as duration, but English reads it as time`},
	} {
		_, err := parse(map[string]string{key: tc.pattern})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %v, want %q", key, err, tc.want)
		}
	}
}
