// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package narrate

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

var t0 = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

// action returns a record in state with the fields that state carries.
func action(state fleet.DispatchState) fleet.ActionRecord {
	r := fleet.ActionRecord{
		ID: []byte("act-1"), Version: 1, State: state,
		AgentID: "agent-7", AgentGeneration: 3,
		Capability: "tickets", Action: "close-ticket",
		Input:   json.RawMessage(`{"ticket":42}`),
		Subject: "alice", Actor: "alice",
		RequestID: "req-1", CorrelationID: "corr-1",
		CreatedAt: t0, UpdatedAt: t0, Deadline: t0.Add(2 * time.Hour),
	}
	if state == fleet.DispatchQueued {
		return r
	}
	r.Version = 2
	r.ClaimID = []byte("claim-1")
	r.ClaimFence = 1
	r.ClaimLease = 5 * time.Minute
	r.ClaimantSubject, r.ClaimantActor = "worker-1", "worker-1"
	r.UpdatedAt = t0.Add(time.Minute)
	r.ClaimLeaseUntil = r.UpdatedAt.Add(r.ClaimLease)
	r.EffectPossible = true
	switch state {
	case fleet.DispatchSucceeded:
		r.Version = 3
		r.UpdatedAt = t0.Add(3 * time.Minute)
		r.Output = json.RawMessage(`{"status":200}`)
	case fleet.DispatchFailed:
		r.Version = 3
		r.UpdatedAt = t0.Add(3 * time.Minute)
		r.ErrorCode = GatewayOutcomeUnknown
	case fleet.DispatchCanceled:
		r.Version = 3
		r.UpdatedAt = t0.Add(10 * time.Minute)
		r.CancelKey = []byte("cancel-1")
	}
	return r
}

func admission(state fleet.DispatchState) fleet.ActionRecord {
	r := action(state)
	r.AdmittedEffects = fleet.Effects{fleet.EffectEgressesContent, fleet.EffectMutatesExternal}
	// The claimant fields action() copies are cleared: an admission is
	// reported by the identity that requested it, and the renderer must name
	// that identity whether or not the claim recorded a claimant.
	r.ClaimantSubject, r.ClaimantActor = "", ""
	if state == fleet.DispatchCanceled {
		r.ClaimID, r.ClaimFence, r.ClaimLease = nil, 0, 0
		r.ClaimLeaseUntil = time.Time{}
		r.ClaimantSubject, r.ClaimantActor = "", ""
		r.EffectPossible = false
		r.UpdatedAt = t0
	}
	return r
}

func approvalRecord(state fleet.ApprovalState, verdict fleet.ApprovalVerdict) fleet.ApprovalRecord {
	request := action(fleet.DispatchQueued)
	request.PolicyGeneration = 9
	digest := sha256.Sum256([]byte("request"))
	r := fleet.ApprovalRecord{
		ID: request.ID, Version: 1, State: state, Request: request,
		RequestDigest: digest[:], PolicyGeneration: 9,
		RequestedAt: t0, ExpiresAt: t0.Add(time.Hour), UpdatedAt: t0,
	}
	if verdict != "" {
		r.Version = 2
		r.Verdict = verdict
		r.ApproverSubject, r.ApproverActor = "bob", "bob"
		r.DecidedAt = t0.Add(10 * time.Minute)
		r.DecisionRequestID = "req-2"
		r.UpdatedAt = r.DecidedAt
	}
	if state == fleet.ApprovalEnqueued {
		r.Version = 3
		r.MaterializedAt = t0.Add(20 * time.Minute)
		r.UpdatedAt = r.MaterializedAt
	}
	if state == fleet.ApprovalExpired {
		r.UpdatedAt = t0.Add(90 * time.Minute)
	}
	if state == fleet.ApprovalWithdrawn {
		r.Version = 2
		r.WithdrawnAt = t0.Add(5 * time.Minute)
		r.UpdatedAt = r.WithdrawnAt
	}
	if state == fleet.ApprovalStranded {
		r.Version++
		r.UpdatedAt = t0.Add(30 * time.Minute)
	}
	return r
}

// strandedAfterApproval is a request stranded while its window was open,
// after it was approved: the one stranded history the status table's
// fixtures, which show a request stranded undecided, do not reach (#655).
func strandedAfterApproval() fleet.ApprovalStatus {
	record := approvalRecord(fleet.ApprovalStranded, fleet.ApprovalVerdictApprove)
	record.StrandedCause = fleet.ApprovalConditionTargetMoved
	return fleet.ApprovalStatus{
		Approval: record, State: fleet.ApprovalStranded,
		Condition: fleet.ApprovalConditionTargetMoved,
	}
}

// approvalStatus builds a status for one row of EffectiveApprovals from one
// of its stored states.
func approvalStatus(row EffectiveApproval, stored fleet.ApprovalState) fleet.ApprovalStatus {
	verdict := fleet.ApprovalVerdict("")
	switch stored {
	case fleet.ApprovalApproved, fleet.ApprovalEnqueued:
		verdict = fleet.ApprovalVerdictApprove
	case fleet.ApprovalRefused:
		verdict = fleet.ApprovalVerdictRefuse
	}
	record := approvalRecord(stored, verdict)
	if stored == fleet.ApprovalStranded {
		// Stranded with the row's cause. A request stranded after it
		// committed to become work was approved and materialized; one
		// stranded while its window was open is shown undecided here, and
		// the coverage test renders it approved too.
		record.StrandedCause = row.Condition
		if row.Condition == fleet.ApprovalConditionIdentityTaken ||
			row.Condition == fleet.ApprovalConditionDeadlinePassed {
			record.Verdict = fleet.ApprovalVerdictApprove
			record.ApproverSubject, record.ApproverActor = "bob", "bob"
			record.DecidedAt = t0.Add(10 * time.Minute)
			record.DecisionRequestID = "req-2"
			record.MaterializedAt = t0.Add(20 * time.Minute)
			record.Version = 4
		}
	}
	return fleet.ApprovalStatus{
		Approval: record, State: row.State, Condition: row.Condition,
	}
}

func taskConfig(questions ...decision.Question) decision.TaskConfig {
	if len(questions) == 0 {
		questions = []decision.Question{
			{ID: "priority", Kind: decision.Ordinal, RubricID: "priority:1", Labels: []string{"low", "medium", "high"}},
			{ID: "relevance", Kind: decision.Choice, RubricID: "relevance:1", Labels: []string{"yes", "no"}},
			{ID: "risk", Kind: decision.Probability, RubricID: "risk:1"},
		}
	}
	return decision.TaskConfig{
		OwnerID: "owner", Name: "source-priority", Version: "v1", InputSchemaID: "schema:1",
		EvidencePolicyID: "evidence:1", LabelPolicyID: "labels:1", EvaluationPolicyID: "evaluation:1",
		PredictionUnit: "subject", LabelUnit: "finding", ActionUnit: "inspection",
		AggregationID: "rank:1", Questions: questions,
	}
}

type pictureEdit func(*decision.PictureConfig)

// request builds a decision request over subjects; edit adjusts the picture.
func request(t *testing.T, task decision.TaskConfig, subjects []shoal.ID, edit pictureEdit) decision.DecisionRequest {
	t.Helper()
	anchor, err := inference.NewDocumentAnchor(document.Citation{
		DocumentID: "source", RevisionID: "revision", SectionID: "section", SpanID: "span",
		Range: document.SourceRange{Start: document.SourcePosition{Offset: 0}, End: document.SourcePosition{Offset: 4}},
	}, "code")
	if err != nil {
		t.Fatal(err)
	}
	snap, err := inference.NewSnapshotPin("snapshot", t0.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	authPin, err := inference.NewAuthPin("auth", t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	pack, err := inference.NewContextPack("inspect", []inference.EvidenceAnchor{anchor}, nil, snap, authPin, nil)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := decision.NewTaskSpec(task)
	if err != nil {
		t.Fatal(err)
	}
	one := uint64(len(subjects))
	pc := decision.PictureConfig{
		TaskID: spec.ID(), ObservationID: "run1", EnumerationID: "inventory:1", ScopeID: "scope:1",
		BuilderID: "builder:1", OntologyProjectionID: "no-ontology:1", InputDigest: strings.Repeat("a", 64),
		TokenizerID: "tokenizer:1", InputTokens: 1, TokenBudget: 100, Cutoff: t0,
		Sources: []decision.Source{{
			ID: "source", ArtifactID: "source", RevisionID: "revision", Digest: strings.Repeat("b", 64),
			OriginID: "origin:1", AuthorityPolicyID: "authority:1", Role: decision.Observation,
			Control: decision.CandidateControlled, ObservedAt: t0.Add(-time.Minute), ReceivedAt: t0,
		}},
		Measurements: []decision.Measurement{{ID: "coverage", Unit: "subjects", MethodID: "count:1", Numerator: one, Denominator: &one}},
	}
	for _, id := range subjects {
		pc.Subjects = append(pc.Subjects, decision.Subject{
			ID: id, SourceID: "source", Disposition: decision.Supported, EvidenceIDs: []shoal.ID{anchor.ID()},
		})
	}
	if edit != nil {
		edit(&pc)
	}
	picture, err := decision.NewPictureManifest(pack, pc)
	if err != nil {
		t.Fatal(err)
	}
	predictor, err := decision.NewPredictorIdentity(decision.PredictorConfig{
		Provider: "local", RuntimeID: "runtime:1", WeightsDigest: strings.Repeat("a", 64),
		TokenizerDigest: strings.Repeat("b", 64), FormattingID: "format:1", PreprocessingID: "preprocessing:1",
		CalibrationID: "uncalibrated:1", EnvironmentDigest: strings.Repeat("c", 64), Device: "cpu",
		Precision: "float64", BatchPolicyID: "batch:1", DistributionTolerance: 0.001, ReplayTolerance: 0.000001,
	})
	if err != nil {
		t.Fatal(err)
	}
	r, err := decision.NewDecisionRequest(spec, picture, predictor, decision.RequestConfig{
		PrincipalID: "carol", ReleaseID: "release:1", CorrelationID: "call:1",
		RequestedAt: t0, Deadline: t0.Add(time.Minute), SubjectIDs: subjects,
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func prediction(t *testing.T, r decision.DecisionRequest, status decision.ResultStatus, reason string, answers []decision.Answer) decision.PredictionRecord {
	t.Helper()
	c := decision.ResultConfig{
		RequestID: r.ID(), PredictorID: r.PredictorID(), EffectiveDevice: "cpu",
		Status: status, Reason: reason, CompletedAt: t0.Add(time.Second), Answers: answers,
	}
	if status == decision.Failed {
		c.EffectiveDevice = ""
	}
	p, err := decision.NewPredictionRecord(r, c)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func completedAnswers(subject shoal.ID) []decision.Answer {
	p := 0.73
	return []decision.Answer{
		{SubjectID: subject, QuestionID: "priority", Status: decision.Answered, Label: "high",
			Distribution: []decision.LabelProbability{{Label: "low", Probability: 0.1}, {Label: "medium", Probability: 0.15}, {Label: "high", Probability: 0.75}}},
		{SubjectID: subject, QuestionID: "relevance", Status: decision.Answered, Label: "yes"},
		{SubjectID: subject, QuestionID: "risk", Status: decision.Answered, Probability: &p},
	}
}

// dump renders sentences one per block, for golden files.
func dump(sentences []Sentence) string {
	var b bytes.Buffer
	for _, s := range sentences {
		b.WriteString(string(s.Role) + " " + s.Key + "\n")
		b.WriteString("  " + s.Text + "\n")
		b.WriteString("  record=" + s.RecordID)
		for _, ref := range s.Refs {
			b.WriteString(" " + ref.Kind + "=" + ref.ID)
		}
		b.WriteString("\n")
		for _, q := range s.Quotes {
			b.WriteString("  quote " + string(q.Attribution))
			if q.By != "" {
				b.WriteString(" by=" + q.By)
			}
			if q.Truncated {
				b.WriteString(" truncated")
			}
			b.WriteString("\n")
		}
		var ids []string
		for _, span := range s.Spans {
			if span.Kind == SpanIdentifier {
				ids = append(ids, s.Text[span.Start:span.End])
			}
		}
		if len(ids) > 0 {
			b.WriteString("  identifiers " + strings.Join(ids, " ") + "\n")
		}
	}
	return b.String()
}
