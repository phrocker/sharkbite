// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package narrate

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// The coverage rule: every outcome, error code, condition, state, transition
// and decision result in the owners' source has a template, and rendering it
// does not fall back to the "unrecognized" sentence reserved for values this
// package has never seen. Every enumeration below is read from source (see
// parity_test.go), never from this package's own lists.

// observed renders through a fresh English catalog that records every key it
// formats.
type observed struct {
	*Renderer
	keys map[string]bool
}

func newObserved(t *testing.T) *observed {
	t.Helper()
	c, err := ParseCatalog(englishCatalog)
	if err != nil {
		t.Fatal(err)
	}
	o := &observed{Renderer: New(c), keys: map[string]bool{}}
	c.observe = func(key string) { o.keys[key] = true }
	return o
}

func noFallback(t *testing.T, what string, sentences []Sentence, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if len(sentences) == 0 {
		t.Fatalf("%s: rendered nothing", what)
	}
	for _, s := range sentences {
		if strings.HasSuffix(s.Key, ".unrecognized") {
			t.Errorf("%s: fell back to %s: %s", what, s.Key, s.Text)
		}
		if s.Text == "" || s.RecordID == "" {
			t.Errorf("%s: sentence %s lacks text or record ID", what, s.Key)
		}
	}
	if sentences[0].Role != RoleOutcome && sentences[0].Role != RoleReason {
		t.Errorf("%s: first sentence is %s, not the outcome", what, sentences[0].Role)
	}
	hasNext := false
	for _, s := range sentences {
		hasNext = hasNext || s.Role == RoleNext
	}
	if !hasNext && !strings.HasPrefix(sentences[0].Key, "interaction.") {
		t.Errorf("%s: no next step", what)
	}
}

func hasKey(sentences []Sentence, key string) bool {
	for _, s := range sentences {
		if s.Key == key {
			return true
		}
	}
	return false
}

func times() []time.Time {
	return []time.Time{
		{},                        // untimed
		t0.Add(2 * time.Minute),   // lease live
		t0.Add(30 * time.Minute),  // lease lapsed
		t0.Add(3 * time.Hour),     // deadline passed
		t0.Add(-30 * time.Minute), // a clock behind the record
	}
}

func TestCoverageDispatchStates(t *testing.T) {
	r := newObserved(t)
	for _, state := range sourceDispatchStates(t) {
		for _, build := range []func(fleet.DispatchState) fleet.ActionRecord{action, admission} {
			if build(fleet.DispatchQueued).AdmittedEffects != nil && state == string(fleet.DispatchQueued) {
				continue // an admission is never queued
			}
			for _, now := range times() {
				record := build(fleet.DispatchState(state))
				what := fmt.Sprintf("%s admission=%v now=%v", state, isAdmission(record), now)
				sentences, err := r.Action(record, Options{Now: now, QuoteInput: true, QuoteOutput: true})
				noFallback(t, what, sentences, err)
				if !hasKey(sentences, "dispatch.outcome."+state) {
					t.Errorf("%s: no outcome sentence", what)
				}
			}
		}
	}
}

func sourceErrorCodes(t *testing.T) []string {
	codes, prefix := sourceGatewayCodes(t)
	for status := 400; status <= 599; status++ {
		codes = append(codes, fmt.Sprintf("%s%03d", prefix, status))
	}
	return append(codes, sourceFleetErrorCodes(t)...)
}

// futureOrigin is an origin no build of fleet writes today: what a record
// from a newer build could carry. It must render as an omitted origin does.
const futureOrigin = fleet.ErrorCodeOrigin("gateway")

// wantOrigin is the rendering each origin must take. It is written out here
// rather than read from errorCodeOrigins, so that changing a decision there —
// in particular, letting an omitted origin read as the executor's — fails.
var wantOrigin = map[fleet.ErrorCodeOrigin]Selector{
	fleet.ErrorCodeOriginUnknown:  OriginEither,
	fleet.ErrorCodeOriginService:  OriginService,
	fleet.ErrorCodeOriginExecutor: OriginExecutor,
	futureOrigin:                  OriginEither,
}

// errorCodeOriginCases is every ErrorCodeOrigin in fleet's source, read from
// source, then futureOrigin. An origin fleet adds fails here until wantOrigin
// (and errorCodeOrigins, by the parity test) decides how it is narrated.
func errorCodeOriginCases(t *testing.T) []fleet.ErrorCodeOrigin {
	var out []fleet.ErrorCodeOrigin
	for _, origin := range sourceErrorCodeOrigins(t) {
		if _, ok := wantOrigin[fleet.ErrorCodeOrigin(origin)]; !ok {
			t.Errorf("fleet defines error code origin %q, and nothing here says "+
				"how it is narrated: decide its templates", origin)
		}
		out = append(out, fleet.ErrorCodeOrigin(origin))
	}
	return append(out, futureOrigin)
}

func failedWith(code string, origin fleet.ErrorCodeOrigin) fleet.ActionRecord {
	record := action(fleet.DispatchFailed)
	record.ErrorCode = code
	record.ErrorCodeOrigin = origin
	return record
}

func TestCoverageErrorCodes(t *testing.T) {
	r := newObserved(t)
	for _, origin := range errorCodeOriginCases(t) {
		want := wantOrigin[origin]
		for _, code := range sourceErrorCodes(t) {
			what := fmt.Sprintf("%s origin=%q", code, origin)
			record := failedWith(code, origin)
			sentences, err := r.Action(record, Options{})
			noFallback(t, what, sentences, err)
			stem, _, _ := errorCodeKey(code)
			found := false
			for _, s := range sentences {
				if s.Role == RoleReason && strings.HasPrefix(s.Key, "dispatch.error.") {
					found = true
					if s.Key != stem+"."+string(want) {
						t.Errorf("%s: reason %s, want the %s template", what, s.Key, want)
					}
					if !strings.Contains(s.Text, code) {
						t.Errorf("%s: reason does not name the code: %s", what, s.Text)
					}
				}
			}
			if !found {
				t.Errorf("%s: no error reason", what)
			}
			history, err := r.ActionHistory([]fleet.ActionTransition{
				{ID: []byte("t1"), Kind: "action.failed", Record: record},
			}, Options{})
			noFallbackHistory(t, what, history, err)
		}
	}
}

func noFallbackHistory(t *testing.T, what string, sentences []Sentence, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	for _, s := range sentences {
		if strings.HasSuffix(s.Key, ".unrecognized") || len(s.Refs) == 0 {
			t.Errorf("%s: %s %q", what, s.Key, s.Text)
		}
	}
}

// edgeTransitions builds a history that ends by taking edge.
func edgeTransitions(edge Edge) []fleet.ActionTransition {
	build := action
	if edge.Applies == "admission" {
		build = admission
	}
	var out []fleet.ActionTransition
	if edge.From != "" {
		from := build(fleet.DispatchState(edge.From))
		from.Version = 1
		kind := "action.enqueued"
		if edge.From == string(fleet.DispatchClaimed) {
			kind = "action.claimed"
		}
		out = append(out, fleet.ActionTransition{ID: []byte("t-from"), Kind: kind, Record: from})
	}
	to := build(fleet.DispatchState(edge.To))
	to.Version = 5
	if edge.Name == "cancel" {
		to.ClaimFence, to.ClaimID, to.ClaimLease = 0, nil, 0
		to.ClaimLeaseUntil = time.Time{}
	}
	if edge.Name == "reclaim" {
		to.ClaimFence = 2
	}
	return append(out, fleet.ActionTransition{ID: []byte("t-edge"), Kind: edge.Kind, Record: to})
}

func TestCoverageTransitions(t *testing.T) {
	r := newObserved(t)
	for _, edge := range DispatchEdges {
		sentences, err := r.ActionHistory(edgeTransitions(edge), Options{})
		noFallbackHistory(t, edge.Name, sentences, err)
		want := "dispatch.transition." + edge.Name
		if edge.Name == "reclaim" {
			want = "dispatch.transition.claim_run"
		}
		if len(sentences) == 0 || sentences[len(sentences)-1].Key != want {
			t.Errorf("edge %s rendered %v", edge.Name, sentences)
		}
	}
	// Every kind in source is narrated as some edge.
	for _, kind := range sourceTransitionKinds(t) {
		found := false
		for _, edge := range DispatchEdges {
			if edge.Kind == kind && r.keys["dispatch.transition."+edge.Name] {
				found = true
			}
		}
		if !found {
			t.Errorf("transition kind %q has no rendered template", kind)
		}
	}
}

func TestCoverageApproval(t *testing.T) {
	r := newObserved(t)
	rows := map[string]EffectiveApproval{}
	for _, row := range EffectiveApprovals {
		rows[string(row.State)+"/"+string(row.Condition)] = row
	}
	for _, doc := range docApprovalRows(t) {
		row, ok := rows[doc[0]+"/"+doc[1]]
		if !ok {
			t.Errorf("status %s/%s has no template row", doc[0], doc[1])
			continue
		}
		for _, stored := range row.Stored {
			for _, now := range times() {
				status := approvalStatus(row, stored)
				what := fmt.Sprintf("%s/%s stored=%s now=%v", row.State, row.Condition, stored, now)
				sentences, err := r.Approval(status, Options{Now: now})
				noFallback(t, what, sentences, err)
				if sentences[0].Role != RoleOutcome {
					t.Errorf("%s: first sentence is not the outcome", what)
				}
			}
		}
	}
	// A stored expired record has two histories.
	for _, verdict := range []fleet.ApprovalVerdict{"", fleet.ApprovalVerdictApprove} {
		sentences, err := r.Approval(fleet.ApprovalStatus{
			Approval: approvalRecord(fleet.ApprovalExpired, verdict), State: fleet.ApprovalExpired,
		}, Options{})
		noFallback(t, "expired "+string(verdict), sentences, err)
	}
	// So does a record stranded while its window was open (#655): before
	// and after a decision.
	for _, verdict := range []fleet.ApprovalVerdict{"", fleet.ApprovalVerdictApprove} {
		record := approvalRecord(fleet.ApprovalStranded, verdict)
		record.StrandedCause = fleet.ApprovalConditionTargetMoved
		sentences, err := r.Approval(fleet.ApprovalStatus{
			Approval: record, State: fleet.ApprovalStranded,
			Condition: fleet.ApprovalConditionTargetMoved,
		}, Options{})
		noFallback(t, "stranded "+string(verdict), sentences, err)
	}
	// Every state and condition in source appears in some row.
	states, conditions := map[string]bool{}, map[string]bool{}
	for _, row := range EffectiveApprovals {
		states[string(row.State)] = true
		conditions[string(row.Condition)] = true
	}
	for _, s := range sourceApprovalStates(t) {
		if !states[s] {
			t.Errorf("approval state %q has no template row", s)
		}
	}
	for _, c := range sourceApprovalConditions(t) {
		if !conditions[c] {
			t.Errorf("approval condition %q has no template row", c)
		}
	}
	for _, edge := range ApprovalEdges {
		if !r.keys["approval.transition."+edge.Name] {
			t.Errorf("approval transition %s was never narrated", edge.Name)
		}
		if approvalEdgeOffered(edge) && !r.keys["approval.next.edge."+edge.Name] {
			t.Errorf("approval edge %s was never offered as a next step", edge.Name)
		}
	}
}

func questionsOfKind(kind decision.AnswerKind) decision.Question {
	q := decision.Question{ID: "q", Kind: kind, RubricID: "q:1", Labels: []string{"low", "high"}}
	if kind == decision.Probability {
		q.Labels = nil
	}
	return q
}

func answerOfKind(kind decision.AnswerKind, withDistribution bool) decision.Answer {
	a := decision.Answer{SubjectID: "subject1", QuestionID: "q", Status: decision.Answered}
	switch kind {
	case decision.Probability:
		p := 0.25
		a.Probability = &p
	default:
		a.Label = "high"
		if withDistribution {
			a.Distribution = []decision.LabelProbability{{Label: "low", Probability: 0.4}, {Label: "high", Probability: 0.6}}
		}
	}
	return a
}

func TestCoverageDecision(t *testing.T) {
	r := newObserved(t)
	reasons := sourceDecisionServiceReasons(t)
	for _, status := range values(typedConsts(t, decisionDir, "ResultStatus")) {
		status := decision.ResultStatus(status)
		if status == decision.Completed {
			for _, kind := range values(typedConsts(t, decisionDir, "AnswerKind")) {
				kind := decision.AnswerKind(kind)
				task := taskConfig(questionsOfKind(kind))
				req := request(t, task, []shoal.ID{"subject1"}, nil)
				for _, dist := range []bool{false, true} {
					p := prediction(t, req, status, "", []decision.Answer{answerOfKind(kind, dist)})
					sentences, err := r.Prediction(p, Options{})
					noFallback(t, string(kind), sentences, err)
					if !hasKey(sentences, "decision.answer."+string(kind)) {
						t.Errorf("%s: no answer sentence", kind)
					}
				}
				for _, reason := range append([]string{"model declined"}, AnswerAbstentionReasons...) {
					p := prediction(t, req, status, "", []decision.Answer{{
						SubjectID: "subject1", QuestionID: "q", Status: decision.AnswerAbstained, Reason: reason,
					}})
					sentences, err := r.Prediction(p, Options{})
					if err != nil {
						t.Fatal(err)
					}
					if !hasKey(sentences, "decision.answer.abstained."+reason) &&
						!hasKey(sentences, "decision.answer.abstained.unrecognized") {
						t.Errorf("abstention %q not narrated", reason)
					}
				}
			}
			continue
		}
		if len(reasons[status]) == 0 {
			t.Errorf("status %s has no reasons in source to cover", status)
		}
		req := request(t, taskConfig(), []shoal.ID{"subject1"}, nil)
		for _, reason := range reasons[status] {
			p := prediction(t, req, status, reason, nil)
			sentences, err := r.Prediction(p, Options{})
			noFallback(t, string(status)+"/"+reason, sentences, err)
			if !hasKey(sentences, "decision.reason."+string(status)+"."+reason) ||
				!hasKey(sentences, "decision.next."+string(status)+"."+reason) {
				t.Errorf("%s/%s: reason or next step missing", status, reason)
			}
		}
		// A predictor's own reason is quoted, not narrated as Shoal's.
		p := prediction(t, req, status, "predictor text", nil)
		sentences, err := r.Prediction(p, Options{})
		if err != nil || !hasKey(sentences, "decision.reason."+string(status)+".unrecognized") {
			t.Errorf("%s with a predictor reason: %v %v", status, sentences, err)
		}
	}
	// Every non-supported disposition is a gap.
	for _, d := range values(typedConsts(t, decisionDir, "Disposition")) {
		if d == string(decision.Supported) {
			continue
		}
		req := request(t, taskConfig(), []shoal.ID{"subject1", "subject2"}, func(pc *decision.PictureConfig) {
			pc.Subjects[1].Disposition = decision.Disposition(d)
			pc.Subjects[1].Reason = "builder note"
			pc.Subjects[1].EvidenceIDs = nil
			zero := uint64(0)
			pc.Measurements = append(pc.Measurements,
				decision.Measurement{ID: "none", Unit: "files", MethodID: "count:3", Denominator: &zero},
				decision.Measurement{ID: "open", Unit: "files", MethodID: "count:4", Numerator: 2})
		})
		p := prediction(t, req, decision.Abstained, "evidence_ineligible", nil)
		sentences, err := r.Prediction(p, Options{})
		noFallback(t, "disposition "+d, sentences, err)
		if !hasKey(sentences, "decision.gap.disposition."+d) {
			t.Errorf("disposition %s is not a gap", d)
		}
	}
	// Every inspection reason.
	for _, reason := range values(typedConsts(t, decisionDir, "InspectionReason")) {
		b := r.begin("eligibility:e", Options{})
		b.inspection("decision.eligibility", []inspection{
			{subject: "s1", reasons: []decision.InspectionReason{decision.InspectionReason(reason)}},
		})
		b.inspection("decision.ranking", []inspection{{subject: "s2"}})
		sentences, err := b.finish()
		noFallback(t, reason, sentences, err)
		if !hasKey(sentences, "decision.inspection."+reason) {
			t.Errorf("inspection reason %s not narrated", reason)
		}
	}
}

// TestCoverageEveryKeyIsReachable renders every coverage, golden, fallback
// and formatting case through one observed catalog and requires that every
// catalog key was used, so the catalog carries no template nothing can say.
func TestCoverageEveryKeyIsReachable(t *testing.T) {
	r := newObserved(t)
	coverRecords(t, r)
	for _, run := range goldenScenarios() {
		if _, err := run(t, r.Renderer); err != nil {
			t.Fatal(err)
		}
	}
	coverFallbacks(t, r)
	// Formatting paths.
	for _, d := range []time.Duration{0, time.Millisecond, 49 * time.Hour, 90 * time.Minute, 61 * time.Second} {
		if _, err := r.catalog.duration(d); err != nil {
			t.Fatal(err)
		}
	}
	r.catalog.time(time.Time{})
	if _, err := r.catalog.list(nil, "and"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.catalog.list([]Fragment{{text: "a"}, {text: "b"}, {text: "c"}}, "and"); err != nil {
		t.Fatal(err)
	}
	b := r.begin("x", Options{MaxListItems: 1})
	b.bounded([]Fragment{{text: "a"}, {text: "b"}})
	b.principal("", "")
	for _, key := range RequiredKeys() {
		if !r.keys[key] {
			t.Errorf("catalog key %s is never used", key)
		}
	}
}

func coverRecords(t *testing.T, r *observed) {
	for _, state := range DispatchStates {
		for _, now := range times() {
			for _, record := range []fleet.ActionRecord{action(state), admission(state)} {
				if _, err := r.Action(record, Options{Now: now, QuoteInput: true, QuoteOutput: true}); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	for _, state := range DispatchStates {
		for _, record := range []fleet.ActionRecord{action(state), admission(state)} {
			for _, flag := range []bool{true, false} {
				record.EffectPossible = flag
				if _, err := r.Action(record, Options{}); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	for _, origin := range errorCodeOriginCases(t) {
		for _, code := range append(sourceErrorCodes(t), "made up by an executor") {
			for _, flag := range []bool{true, false} {
				record := failedWith(code, origin)
				record.EffectPossible = flag
				if _, err := r.Action(record, Options{}); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	for _, edge := range DispatchEdges {
		if _, err := r.ActionHistory(edgeTransitions(edge), Options{}); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range EffectiveApprovals {
		for _, stored := range row.Stored {
			for _, now := range times() {
				if _, err := r.Approval(approvalStatus(row, stored), Options{Now: now}); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	for _, verdict := range []fleet.ApprovalVerdict{"", fleet.ApprovalVerdictApprove} {
		if _, err := r.Approval(fleet.ApprovalStatus{
			Approval: approvalRecord(fleet.ApprovalExpired, verdict), State: fleet.ApprovalExpired,
		}, Options{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Approval(strandedAfterApproval(), Options{}); err != nil {
		t.Fatal(err)
	}
	coverDecision(t, r)
	for _, c := range routerCases(t) {
		if _, err := c.run(r.Renderer); err != nil {
			t.Fatal(err)
		}
	}
}

func coverDecision(t *testing.T, r *observed) {
	for _, kind := range AnswerKinds {
		req := request(t, taskConfig(questionsOfKind(kind)), []shoal.ID{"subject1"}, nil)
		for _, dist := range []bool{false, true} {
			if _, err := r.Prediction(prediction(t, req, decision.Completed, "", []decision.Answer{answerOfKind(kind, dist)}), Options{}); err != nil {
				t.Fatal(err)
			}
		}
		for _, reason := range append([]string{"model declined"}, AnswerAbstentionReasons...) {
			p := prediction(t, req, decision.Completed, "", []decision.Answer{{
				SubjectID: "subject1", QuestionID: "q", Status: decision.AnswerAbstained, Reason: reason,
			}})
			if _, err := r.Prediction(p, Options{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	req := request(t, taskConfig(), []shoal.ID{"subject1"}, nil)
	for status, reasons := range DecisionServiceReasons {
		for _, reason := range append(append([]string{}, reasons...), "predictor text") {
			if _, err := r.Prediction(prediction(t, req, status, reason, nil), Options{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, d := range Dispositions {
		if d == decision.Supported {
			continue
		}
		many := []shoal.ID{"s1", "s2", "s3", "s4", "s5", "s6", "s7", "s8"}
		req := request(t, taskConfig(), many, func(pc *decision.PictureConfig) {
			for i := 1; i < len(pc.Subjects); i++ {
				pc.Subjects[i].Disposition = d
				pc.Subjects[i].Reason = "note"
				pc.Subjects[i].EvidenceIDs = nil
			}
			zero, ten := uint64(0), uint64(10)
			pc.Measurements = append(pc.Measurements,
				decision.Measurement{ID: "none", Unit: "files", MethodID: "count:3", Denominator: &zero},
				decision.Measurement{ID: "open", Unit: "files", MethodID: "count:4", Numerator: 2},
				decision.Measurement{ID: "part", Unit: "files", MethodID: "count:5", Numerator: 2, Denominator: &ten})
			pc.Truncated = true
		})
		if _, err := r.Prediction(prediction(t, req, decision.Abstained, "evidence_ineligible", nil), Options{}); err != nil {
			t.Fatal(err)
		}
	}
	many := []shoal.ID{"s1", "s2", "s3"}
	req = request(t, taskConfig(questionsOfKind(decision.Choice)), many, nil)
	var answers []decision.Answer
	for _, s := range many {
		a := answerOfKind(decision.Choice, false)
		a.SubjectID = s
		answers = append(answers, a)
	}
	if _, err := r.Prediction(prediction(t, req, decision.Completed, "", answers), Options{MaxAnswers: 1}); err != nil {
		t.Fatal(err)
	}
	for _, reason := range InspectionReasons {
		b := r.begin("e", Options{})
		b.inspection("decision.eligibility", []inspection{{subject: "s1", reasons: []decision.InspectionReason{reason}}})
		b.inspection("decision.ranking", []inspection{{subject: "s1"}})
		if _, err := b.finish(); err != nil {
			t.Fatal(err)
		}
	}
}

// coverFallbacks renders the values reserved for things this build has never
// seen, which only a record from a newer build or a corrupt one can carry.
func coverFallbacks(t *testing.T, r *observed) {
	record := action(fleet.DispatchState("held"))
	if _, err := r.Action(record, Options{}); err != nil {
		t.Fatal(err)
	}
	queued := action(fleet.DispatchQueued)
	if _, err := r.ActionHistory([]fleet.ActionTransition{
		{ID: []byte("t"), Kind: "action.paused", Record: queued},
	}, Options{}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Approval(fleet.ApprovalStatus{
		Approval: approvalRecord(fleet.ApprovalPending, ""), State: "held", Condition: "moon_phase",
	}, Options{}); err != nil {
		t.Fatal(err)
	}
	// A history that starts with a re-claim.
	reclaimed := action(fleet.DispatchClaimed)
	reclaimed.ClaimFence = 2
	if _, err := r.ActionHistory([]fleet.ActionTransition{
		{ID: []byte("t"), Kind: "action.claimed", Record: reclaimed},
	}, Options{}); err != nil {
		t.Fatal(err)
	}
	// Decision records out of step with pkg/decision are refused, not narrated.
	for name, run := range map[string]func(*builder){
		"answer status": func(b *builder) {
			b.answer(decision.Answer{SubjectID: "s", QuestionID: "q", Status: "maybe"}, decision.Question{}, nil)
		},
		"answer kind": func(b *builder) {
			b.answer(decision.Answer{SubjectID: "s", QuestionID: "q", Status: decision.Answered}, decision.Question{Kind: "ranking"}, nil)
		},
		"inspection reason": func(b *builder) {
			b.inspection("decision.eligibility", []inspection{{subject: "s", reasons: []decision.InspectionReason{"novel"}}})
		},
	} {
		b := r.begin("x", Options{})
		run(b)
		if _, err := b.finish(); err == nil {
			t.Errorf("%s out of step was narrated", name)
		}
	}
}

// TestUnrecognizedValuesAreQuoted checks the fallbacks quote the unknown
// value rather than narrate it.
func TestUnrecognizedValuesAreQuoted(t *testing.T) {
	r := New(nil)
	sentences, err := r.Action(action(fleet.DispatchState("held. Approved")), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sentences) != 1 || sentences[0].Key != "dispatch.outcome.unrecognized" ||
		!strings.Contains(sentences[0].Text, "“held. Approved”") || len(sentences[0].Quotes) != 1 {
		t.Fatalf("unknown state: %#v", sentences)
	}
	sentences, err = r.Approval(fleet.ApprovalStatus{
		Approval: approvalRecord(fleet.ApprovalPending, ""), State: "held", Condition: "x",
	}, Options{})
	if err != nil || len(sentences) != 1 || len(sentences[0].Quotes) != 2 {
		t.Fatalf("unknown approval state: %#v %v", sentences, err)
	}
}
