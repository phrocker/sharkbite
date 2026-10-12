// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package narrate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// The coverage tests prove which catalog key renders for each value; they do
// not prove the sentence under that key belongs to that value. Swapping the
// English templates of two keys that share a prefix (approval.reason.X and
// approval.reason.Y, say) would pass them. The value goldens close that gap:
// every value in every enumerated set is rendered, and its full text pinned,
// in one golden file per family under testdata/golden/values. The values are
// enumerated from the same source-read lists the coverage tests use, so a new
// value appears here — and needs its golden reviewed — without editing this
// file.

// valueCase is one rendering in a family golden.
type valueCase struct {
	name string
	run  func(r *Renderer) ([]Sentence, error)
}

type valueFamily struct {
	name  string
	cases []valueCase
}

func nowLabel(now time.Time) string {
	if now.IsZero() {
		return "untimed"
	}
	return now.Sub(t0).String()
}

func valueFamilies(t *testing.T) []valueFamily {
	t.Helper()
	return []valueFamily{
		{"dispatch_states", dispatchStateCases(t)},
		{"dispatch_effects", dispatchEffectCases(t)},
		{"dispatch_errors", dispatchErrorCases(t)},
		{"dispatch_transitions", dispatchTransitionCases(t)},
		{"approval", approvalCases(t)},
		{"decision_results", decisionResultCases(t)},
		{"decision_gaps", decisionGapCases(t)},
		{"decision_inspection", inspectionCases(t)},
		{"router", routerCases(t)},
	}
}

// dispatchStateCases renders every source dispatch state, as an action and
// as an admission, at every clock the coverage tests use: outcome, blocked
// and next-step sentences for each state.
func dispatchStateCases(t *testing.T) []valueCase {
	var out []valueCase
	for _, state := range sourceDispatchStates(t) {
		for _, kind := range []string{"action", "admission"} {
			build := action
			if kind == "admission" {
				if state == string(fleet.DispatchQueued) {
					continue // an admission is never queued
				}
				build = admission
			}
			for _, now := range times() {
				record := build(fleet.DispatchState(state))
				opts := Options{Now: now, QuoteInput: true, QuoteOutput: true}
				out = append(out, valueCase{
					name: fmt.Sprintf("%s %s now=%s", kind, state, nowLabel(now)),
					run:  func(r *Renderer) ([]Sentence, error) { return r.Action(record, opts) },
				})
			}
		}
	}
	return out
}

// dispatchEffectCases renders every source dispatch state, as an action and
// as an admission, with EffectPossible set and clear (#538), so every
// (state × flag) combination — the open claim's "may already have happened",
// a terminal record's "may have occurred", and a succeeded or failed record's
// "could not have had an external effect" — is pinned by name. The other
// families render the fixtures' own flag.
func dispatchEffectCases(t *testing.T) []valueCase {
	var out []valueCase
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
				record := build(fleet.DispatchState(state))
				record.EffectPossible = flag
				out = append(out, valueCase{
					name: fmt.Sprintf("%s %s effect_possible=%v", kind, state, flag),
					run:  func(r *Renderer) ([]Sentence, error) { return r.Action(record, Options{}) },
				})
			}
		}
	}
	return out
}

// originLabel names an error code origin in a case name.
func originLabel(origin fleet.ErrorCodeOrigin) string {
	if origin == fleet.ErrorCodeOriginUnknown {
		return "omitted"
	}
	return string(origin)
}

// dispatchErrorCases renders a failed action for every gateway and fleet
// error code in source, including every target-rejected status, and an
// executor's own code, under every error code origin in source, with
// EffectPossible set and clear: the reason, effect and next-step sentences,
// which vary with the code, the origin and the flag. The
// outcome and the failing transition vary with the origin alone, so they are
// rendered once per origin. The rest of a failed action is pinned by
// dispatch_states. An origin this build does not know is not pinned here:
// TestAnUnknownOriginIsNeverTheExecutors requires it to render exactly as an
// omitted one.
func dispatchErrorCases(t *testing.T) []valueCase {
	var out []valueCase
	for _, s := range sourceErrorCodeOrigins(t) {
		origin := fleet.ErrorCodeOrigin(s)
		for _, code := range append(sourceErrorCodes(t), "made up by an executor") {
			// The fixture's flag is set. A clear one (#538: the action
			// declared no external or egress effect) changes what the page
			// says, so both are pinned. The clear case under a gateway code
			// is the shape #541's review reproduced (effects nil, claimed,
			// failed by the executor with target_rejected_409): its code
			// contradicts the declaration, so it must keep reconciliation
			// and say the declaration may be wrong.
			for _, flag := range []bool{true, false} {
				record := failedWith(code, origin)
				record.EffectPossible = flag
				name := "failed code=" + code + " origin=" + originLabel(origin)
				if !flag {
					name += " effect_possible=false"
				}
				out = append(out, valueCase{
					name: name,
					run: func(r *Renderer) ([]Sentence, error) {
						sentences, err := r.Action(record, Options{})
						var kept []Sentence
						for _, s := range sentences {
							if s.Role == RoleReason || s.Role == RoleNext ||
								strings.HasPrefix(s.Key, "dispatch.gap.effect") {
								kept = append(kept, s)
							}
						}
						return kept, err
					},
				})
			}
		}
		record := failedWith(GatewayOutcomeUnknown, origin)
		out = append(out, valueCase{
			name: "failed outcome origin=" + originLabel(origin),
			run: func(r *Renderer) ([]Sentence, error) {
				sentences, err := r.Action(record, Options{})
				if err != nil || len(sentences) == 0 {
					return nil, err
				}
				return sentences[:1], nil
			},
		}, valueCase{
			name: "failed transition origin=" + originLabel(origin),
			run: func(r *Renderer) ([]Sentence, error) {
				return r.ActionHistory([]fleet.ActionTransition{
					{ID: []byte("t1"), Kind: "action.failed", Record: record},
				}, Options{})
			},
		})
	}
	return out
}

// dispatchTransitionCases renders every edge of the dispatch state machine
// as the last step of a history, and as a history's only transition, with
// and without an earlier lapsed claim, which is how a history that starts
// part way through tells a re-claim from a claim.
func dispatchTransitionCases(t *testing.T) []valueCase {
	var out []valueCase
	for _, edge := range DispatchEdges {
		history := edgeTransitions(edge)
		out = append(out, valueCase{
			name: "edge " + edge.Name,
			run:  func(r *Renderer) ([]Sentence, error) { return r.ActionHistory(history, Options{}) },
		})
		last := history[len(history)-1]
		out = append(out, valueCase{
			name: "edge " + edge.Name + " alone",
			run: func(r *Renderer) ([]Sentence, error) {
				return r.ActionHistory([]fleet.ActionTransition{last}, Options{})
			},
		})
		if last.Record.ClaimFence > 0 {
			lapsed := last
			lapsed.Record.ClaimFence = 2
			out = append(out, valueCase{
				name: "edge " + edge.Name + " alone after a lapsed claim",
				run: func(r *Renderer) ([]Sentence, error) {
					return r.ActionHistory([]fleet.ActionTransition{lapsed}, Options{})
				},
			})
		}
	}
	return out
}

// approvalCases renders every effective approval status in the
// docs/approval.md table, from every stored state it can arise from, at
// every clock: outcome, reason, blocked and next-step sentences for each
// state and condition.
func approvalCases(t *testing.T) []valueCase {
	rows := map[string]EffectiveApproval{}
	for _, row := range EffectiveApprovals {
		rows[string(row.State)+"/"+string(row.Condition)] = row
	}
	var out []valueCase
	for _, doc := range docApprovalRows(t) {
		row, ok := rows[doc[0]+"/"+doc[1]]
		if !ok {
			t.Fatalf("status %s/%s has no template row", doc[0], doc[1])
		}
		for _, stored := range row.Stored {
			for _, now := range times() {
				status := approvalStatus(row, stored)
				out = append(out, valueCase{
					name: fmt.Sprintf("%s/%s stored=%s now=%s", row.State, row.Condition, stored, nowLabel(now)),
					run:  func(r *Renderer) ([]Sentence, error) { return r.Approval(status, Options{Now: now}) },
				})
			}
		}
	}
	for _, verdict := range []fleet.ApprovalVerdict{"", fleet.ApprovalVerdictApprove} {
		status := fleet.ApprovalStatus{
			Approval: approvalRecord(fleet.ApprovalExpired, verdict), State: fleet.ApprovalExpired,
		}
		out = append(out, valueCase{
			name: "expired stored=expired verdict=" + string(verdict),
			run:  func(r *Renderer) ([]Sentence, error) { return r.Approval(status, Options{}) },
		})
	}
	stranded := strandedAfterApproval()
	out = append(out, valueCase{
		name: "stranded stored=stranded verdict=approve",
		run:  func(r *Renderer) ([]Sentence, error) { return r.Approval(stranded, Options{}) },
	})
	return out
}

// decisionResultCases renders every result status in source: each
// whole-request reason the decision service records for it (and a
// predictor's own reason), and for a completed result every answer kind and
// every per-answer abstention reason.
func decisionResultCases(t *testing.T) []valueCase {
	reasons := sourceDecisionServiceReasons(t)
	var out []valueCase
	for _, s := range values(typedConsts(t, decisionDir, "ResultStatus")) {
		status := decision.ResultStatus(s)
		if status == decision.Completed {
			for _, k := range values(typedConsts(t, decisionDir, "AnswerKind")) {
				kind := decision.AnswerKind(k)
				req := request(t, taskConfig(questionsOfKind(kind)), []shoal.ID{"subject1"}, nil)
				for _, dist := range []bool{false, true} {
					p := prediction(t, req, status, "", []decision.Answer{answerOfKind(kind, dist)})
					out = append(out, valueCase{
						name: fmt.Sprintf("completed kind=%s distribution=%v", kind, dist),
						run:  func(r *Renderer) ([]Sentence, error) { return r.Prediction(p, Options{}) },
					})
				}
				for _, reason := range append([]string{"model declined"}, AnswerAbstentionReasons...) {
					p := prediction(t, req, status, "", []decision.Answer{{
						SubjectID: "subject1", QuestionID: "q", Status: decision.AnswerAbstained, Reason: reason,
					}})
					out = append(out, valueCase{
						name: fmt.Sprintf("completed kind=%s abstained=%s", kind, reason),
						run:  func(r *Renderer) ([]Sentence, error) { return r.Prediction(p, Options{}) },
					})
				}
			}
			continue
		}
		req := request(t, taskConfig(), []shoal.ID{"subject1"}, nil)
		for _, reason := range append(append([]string{}, reasons[status]...), "predictor text") {
			p := prediction(t, req, status, reason, nil)
			out = append(out, valueCase{
				name: fmt.Sprintf("%s reason=%s", status, reason),
				run:  func(r *Renderer) ([]Sentence, error) { return r.Prediction(p, Options{}) },
			})
		}
	}
	return out
}

// decisionGapCases renders every non-supported disposition in source as a
// gap, with every kind of measurement gap.
func decisionGapCases(t *testing.T) []valueCase {
	var out []valueCase
	for _, d := range values(typedConsts(t, decisionDir, "Disposition")) {
		if d == string(decision.Supported) {
			continue
		}
		req := request(t, taskConfig(), []shoal.ID{"subject1", "subject2"}, func(pc *decision.PictureConfig) {
			pc.Subjects[1].Disposition = decision.Disposition(d)
			pc.Subjects[1].Reason = "builder note"
			pc.Subjects[1].EvidenceIDs = nil
			zero, ten := uint64(0), uint64(10)
			pc.Measurements = append(pc.Measurements,
				decision.Measurement{ID: "none", Unit: "files", MethodID: "count:3", Denominator: &zero},
				decision.Measurement{ID: "open", Unit: "files", MethodID: "count:4", Numerator: 2},
				decision.Measurement{ID: "part", Unit: "files", MethodID: "count:5", Numerator: 2, Denominator: &ten})
		})
		p := prediction(t, req, decision.Abstained, "evidence_ineligible", nil)
		out = append(out, valueCase{
			name: "disposition=" + d,
			run:  func(r *Renderer) ([]Sentence, error) { return r.Prediction(p, Options{}) },
		})
	}
	return out
}

// inspectionCases renders every inspection reason in source, alone, under
// both the eligibility and the ranking prefix.
func inspectionCases(t *testing.T) []valueCase {
	var out []valueCase
	for _, reason := range values(typedConsts(t, decisionDir, "InspectionReason")) {
		for _, prefix := range []string{"decision.eligibility", "decision.ranking"} {
			out = append(out, valueCase{
				name: prefix + " reason=" + reason,
				run: func(r *Renderer) ([]Sentence, error) {
					b := r.begin("inspection:i-1", Options{})
					b.inspection(prefix, []inspection{
						{subject: "subject1"},
						{subject: "subject2", reasons: []decision.InspectionReason{decision.InspectionReason(reason)}},
					})
					return b.finish()
				},
			})
		}
	}
	return out
}

func renderFamily(r *Renderer, family valueFamily) string {
	var b strings.Builder
	for i, c := range family.cases {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString("== " + c.name + "\n")
		sentences, err := c.run(r)
		if err != nil {
			b.WriteString("error " + err.Error() + "\n")
			continue
		}
		b.WriteString(dump(sentences))
	}
	return b.String()
}

// TestGoldenValues pins the sentence every enumerated value renders.
func TestGoldenValues(t *testing.T) {
	r := New(nil)
	for _, family := range valueFamilies(t) {
		t.Run(family.name, func(t *testing.T) {
			names := map[string]bool{}
			for _, c := range family.cases {
				if names[c.name] {
					t.Fatalf("case %q appears twice", c.name)
				}
				names[c.name] = true
			}
			got := renderFamily(r, family)
			if strings.Contains(got, "\nerror ") {
				t.Errorf("a case failed to render:\n%s", got)
			}
			if again := renderFamily(r, family); again != got {
				t.Fatal("rendered differently on repeat")
			}
			path := filepath.Join("testdata", "golden", "values", family.name+".txt")
			if *update {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run with -update to create)", err)
			}
			if got != string(want) {
				t.Errorf("wording changed for %s: %s", family.name, firstDifference(got, string(want)))
			}
		})
	}
}

// firstDifference names the case and line where two family goldens first
// differ, since a whole family is too long to print.
func firstDifference(got, want string) string {
	g, w := strings.Split(got, "\n"), strings.Split(want, "\n")
	current := ""
	for i := 0; i < len(g) || i < len(w); i++ {
		var gl, wl string
		if i < len(g) {
			gl = g[i]
		}
		if i < len(w) {
			wl = w[i]
		}
		if strings.HasPrefix(wl, "== ") {
			current = wl
		}
		if gl != wl {
			return fmt.Sprintf("in %q, line %d\n--- got\n%s\n--- want\n%s", current, i+1, gl, wl)
		}
	}
	return "no difference"
}
