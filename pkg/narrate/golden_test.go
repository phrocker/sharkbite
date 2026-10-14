// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package narrate

import (
	"flag"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/router"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

var update = flag.Bool("update", false, "rewrite golden files")

type scenario func(t *testing.T, r *Renderer) ([]Sentence, error)

func goldenScenarios() map[string]scenario {
	return map[string]scenario{
		"action_queued": func(t *testing.T, r *Renderer) ([]Sentence, error) {
			return r.Action(action(fleet.DispatchQueued), Options{})
		},
		"action_queued_deadline_passed": func(t *testing.T, r *Renderer) ([]Sentence, error) {
			return r.Action(action(fleet.DispatchQueued), Options{Now: t0.Add(3 * time.Hour)})
		},
		"action_claimed_untimed": func(t *testing.T, r *Renderer) ([]Sentence, error) {
			return r.Action(action(fleet.DispatchClaimed), Options{})
		},
		"action_claimed_live": func(t *testing.T, r *Renderer) ([]Sentence, error) {
			return r.Action(action(fleet.DispatchClaimed), Options{Now: t0.Add(2 * time.Minute)})
		},
		"action_claimed_lapsed_reclaimed": func(t *testing.T, r *Renderer) ([]Sentence, error) {
			record := action(fleet.DispatchClaimed)
			record.ClaimFence = 3
			return r.Action(record, Options{Now: t0.Add(30 * time.Minute)})
		},
		"action_succeeded_with_evidence": func(t *testing.T, r *Renderer) ([]Sentence, error) {
			record := action(fleet.DispatchSucceeded)
			record.EvidenceSnapshotID = "snap-4"
			record.EvidenceSnapshotAsOf = t0.Add(2 * time.Minute)
			record.Evidence = []fleet.EvidenceRef{{AnchorID: "anchor-1"}, {AnchorID: "anchor-2"}}
			return r.Action(record, Options{QuoteInput: true, QuoteOutput: true})
		},
		"action_succeeded_approved": func(t *testing.T, r *Renderer) ([]Sentence, error) {
			record := action(fleet.DispatchSucceeded)
			record.ApprovalRequestDigest = make([]byte, 32)
			record.ApprovalPolicyGeneration = 9
			record.ApproverSubject, record.ApproverActor = "bob", "bob"
			record.ApprovedAt = t0.Add(-5 * time.Minute)
			return r.Action(record, Options{})
		},
		"action_failed_target_rejected": func(t *testing.T, r *Renderer) ([]Sentence, error) {
			record := action(fleet.DispatchFailed)
			record.ErrorCode = "target_rejected_409"
			return r.Action(record, Options{})
		},
		"action_failed_unrecognized_code": func(t *testing.T, r *Renderer) ([]Sentence, error) {
			record := action(fleet.DispatchFailed)
			record.ErrorCode = "upstream said: retry later"
			return r.Action(record, Options{})
		},
		"action_canceled_after_lapse": func(t *testing.T, r *Renderer) ([]Sentence, error) {
			return r.Action(action(fleet.DispatchCanceled), Options{})
		},
		"admission_claimed_live": func(t *testing.T, r *Renderer) ([]Sentence, error) {
			return r.Action(admission(fleet.DispatchClaimed), Options{Now: t0.Add(2 * time.Minute)})
		},
		"admission_claimed_lapsed": func(t *testing.T, r *Renderer) ([]Sentence, error) {
			return r.Action(admission(fleet.DispatchClaimed), Options{Now: t0.Add(time.Hour)})
		},
		"admission_denied": func(t *testing.T, r *Renderer) ([]Sentence, error) {
			return r.Action(admission(fleet.DispatchCanceled), Options{})
		},
		"action_history": func(t *testing.T, r *Renderer) ([]Sentence, error) {
			queued := action(fleet.DispatchQueued)
			claim1 := action(fleet.DispatchClaimed)
			claim2 := claim1
			claim2.Version, claim2.ClaimFence = 3, 2
			claim2.ClaimantActor, claim2.ClaimantSubject = "worker-2", "worker-2"
			claim2.UpdatedAt = t0.Add(7 * time.Minute)
			claim3 := claim2
			claim3.Version, claim3.ClaimFence = 4, 3
			claim3.UpdatedAt = t0.Add(13 * time.Minute)
			failed := claim3
			failed.Version, failed.State = 5, fleet.DispatchFailed
			failed.ErrorCode = GatewayRetryExhausted
			failed.UpdatedAt = t0.Add(15 * time.Minute)
			return r.ActionHistory([]fleet.ActionTransition{
				{ID: []byte("t1"), Kind: "action.enqueued", Record: queued},
				{ID: []byte("t2"), Kind: "action.claimed", Record: claim1},
				{ID: []byte("t3"), Kind: "action.claimed", Record: claim2},
				{ID: []byte("t4"), Kind: "action.claimed", Record: claim3},
				{ID: []byte("t5"), Kind: "action.failed", Record: failed},
			}, Options{})
		},
		"approval_pending_timed": func(t *testing.T, r *Renderer) ([]Sentence, error) {
			return r.Approval(fleet.ApprovalStatus{
				Approval: approvalRecord(fleet.ApprovalPending, ""), State: fleet.ApprovalPending,
			}, Options{Now: t0.Add(15 * time.Minute)})
		},
		"approval_approved": func(t *testing.T, r *Renderer) ([]Sentence, error) {
			return r.Approval(fleet.ApprovalStatus{
				Approval: approvalRecord(fleet.ApprovalApproved, fleet.ApprovalVerdictApprove), State: fleet.ApprovalApproved,
			}, Options{})
		},
		"approval_refused": func(t *testing.T, r *Renderer) ([]Sentence, error) {
			return r.Approval(fleet.ApprovalStatus{
				Approval: approvalRecord(fleet.ApprovalRefused, fleet.ApprovalVerdictRefuse), State: fleet.ApprovalRefused,
			}, Options{})
		},
		"approval_expired_unused": func(t *testing.T, r *Renderer) ([]Sentence, error) {
			return r.Approval(fleet.ApprovalStatus{
				Approval: approvalRecord(fleet.ApprovalExpired, fleet.ApprovalVerdictApprove), State: fleet.ApprovalExpired,
			}, Options{})
		},
		"approval_window_closed": func(t *testing.T, r *Renderer) ([]Sentence, error) {
			return r.Approval(fleet.ApprovalStatus{
				Approval:  approvalRecord(fleet.ApprovalPending, ""),
				State:     fleet.ApprovalExpired,
				Condition: fleet.ApprovalConditionWindowClosed,
			}, Options{})
		},
		"approval_enqueued": func(t *testing.T, r *Renderer) ([]Sentence, error) {
			return r.Approval(fleet.ApprovalStatus{
				Approval: approvalRecord(fleet.ApprovalEnqueued, fleet.ApprovalVerdictApprove), State: fleet.ApprovalEnqueued,
			}, Options{})
		},
		"approval_awaiting_action": func(t *testing.T, r *Renderer) ([]Sentence, error) {
			return r.Approval(fleet.ApprovalStatus{
				Approval:  approvalRecord(fleet.ApprovalEnqueued, fleet.ApprovalVerdictApprove),
				State:     fleet.ApprovalEnqueued,
				Condition: fleet.ApprovalConditionAwaitingAction,
			}, Options{})
		},
		"approval_target_moved": func(t *testing.T, r *Renderer) ([]Sentence, error) {
			return r.Approval(fleet.ApprovalStatus{
				Approval:  approvalRecord(fleet.ApprovalApproved, fleet.ApprovalVerdictApprove),
				State:     fleet.ApprovalUnresolvable,
				Condition: fleet.ApprovalConditionTargetMoved,
			}, Options{})
		},
		"prediction_completed": func(t *testing.T, r *Renderer) ([]Sentence, error) {
			req := request(t, taskConfig(), []shoal.ID{"subject1"}, nil)
			return r.Prediction(prediction(t, req, decision.Completed, "", completedAnswers("subject1")), Options{})
		},
		"prediction_completed_with_gaps": func(t *testing.T, r *Renderer) ([]Sentence, error) {
			req := request(t, taskConfig(), []shoal.ID{"subject1", "subject2", "subject3"}, func(pc *decision.PictureConfig) {
				pc.Subjects[1].Disposition = decision.Missing
				pc.Subjects[1].Reason = "lockfile not collected"
				pc.Subjects[1].EvidenceIDs = nil
				pc.Subjects[2].Disposition = decision.Unsupported
				pc.Subjects[2].Reason = "generated code"
				pc.Subjects[2].EvidenceIDs = nil
				three := uint64(3)
				pc.Measurements = []decision.Measurement{
					{ID: "coverage", Unit: "subjects", MethodID: "count:1", Numerator: 1, Denominator: &three},
					{ID: "dependencies", Unit: "packages", MethodID: "count:2", Numerator: 12},
				}
			})
			answers := completedAnswers("subject1")
			for _, s := range []shoal.ID{"subject2", "subject3"} {
				for _, q := range []shoal.ID{"priority", "relevance", "risk"} {
					answers = append(answers, decision.Answer{SubjectID: s, QuestionID: q, Status: decision.AnswerAbstained, Reason: "unavailable"})
				}
			}
			return r.Prediction(prediction(t, req, decision.Completed, "", answers), Options{MaxAnswers: 4})
		},
		"prediction_abstained_ineligible": func(t *testing.T, r *Renderer) ([]Sentence, error) {
			req := request(t, taskConfig(), []shoal.ID{"subject1"}, func(pc *decision.PictureConfig) {
				pc.Truncated = true
				pc.InputTokens = 2048
				pc.TokenBudget = 2048
			})
			return r.Prediction(prediction(t, req, decision.Abstained, "evidence_ineligible", nil), Options{})
		},
		"prediction_failed_deadline": func(t *testing.T, r *Renderer) ([]Sentence, error) {
			req := request(t, taskConfig(), []shoal.ID{"subject1"}, nil)
			return r.Prediction(prediction(t, req, decision.Failed, "deadline_exceeded", nil), Options{})
		},
		"prediction_abstained_predictor_reason": func(t *testing.T, r *Renderer) ([]Sentence, error) {
			req := request(t, taskConfig(), []shoal.ID{"subject1"}, nil)
			return r.Prediction(prediction(t, req, decision.Abstained, "model is unsure", nil), Options{})
		},
		"inspection_reasons": func(t *testing.T, r *Renderer) ([]Sentence, error) {
			b := r.begin("eligibility:e-1", Options{})
			b.inspection("decision.eligibility", []inspection{
				{subject: "subject1"},
				{subject: "subject2", reasons: []decision.InspectionReason{decision.SourceStale, decision.SubjectUnsupported}},
				{subject: "subject3", reasons: []decision.InspectionReason{decision.SourceStale}},
			}, Ref{Kind: "decision_request", ID: "dr-1"})
			return b.finish()
		},
		"router_proposal_action_approval": func(t *testing.T, r *Renderer) ([]Sentence, error) {
			return r.Proposal(routerProposal(t, router.KindAction, true), Options{})
		},
		"router_abstain_missing_slot": func(t *testing.T, r *Renderer) ([]Sentence, error) {
			return r.Proposal(routerAbstention(t, router.ReasonMissingSlot), Options{})
		},
		"asserted_reason": func(t *testing.T, r *Renderer) ([]Sentence, error) {
			return r.AssertedReason(interaction.Session{
				ID:    "session-1",
				Actor: interaction.ActorContext{SubjectID: "alice", ActorID: "alice"},
				CallerAssertedReason: interaction.CallerAssertedReason{
					Code: "atpl-apply", Source: "atpl:policy:v1:" + "ab12",
				},
			}, Options{})
		},
	}
}

func TestGolden(t *testing.T) {
	r := New(nil)
	scenarios := goldenScenarios()
	names := make([]string, 0, len(scenarios))
	for name := range scenarios {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			sentences, err := scenarios[name](t, r)
			if err != nil {
				t.Fatal(err)
			}
			got := dump(sentences)
			path := filepath.Join("testdata", "golden", name+".txt")
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
				t.Errorf("wording changed for %s\n--- got\n%s\n--- want\n%s", name, got, want)
			}
		})
	}
}

// TestRenderingIsDeterministic renders every golden scenario twice and
// compares, so map iteration or clock reads cannot creep in.
func TestRenderingIsDeterministic(t *testing.T) {
	r := New(nil)
	for name, run := range goldenScenarios() {
		first, err := run(t, r)
		if err != nil {
			t.Fatal(name, err)
		}
		for i := 0; i < 5; i++ {
			again, err := run(t, r)
			if err != nil {
				t.Fatal(name, err)
			}
			if dump(again) != dump(first) {
				t.Fatalf("%s rendered differently on repeat", name)
			}
		}
	}
}
