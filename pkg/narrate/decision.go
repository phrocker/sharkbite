// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package narrate

import (
	"fmt"

	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func idRef(kind string, id shoal.ID) Ref { return Ref{Kind: kind, ID: string(id)} }

// Decision records are only constructed through validating constructors in
// pkg/decision, which refuse a status, kind, disposition or reason outside
// their closed sets. A value outside them therefore means this package is
// out of step with pkg/decision, and the renderer returns an error rather
// than narrate it. (Fleet records, by contrast, are decoded from storage that
// a newer build may have written, so their unknown values are quoted.)

// Prediction narrates a decision prediction record: the result, why a
// request abstained or failed, each answer (bounded by Options.MaxAnswers),
// the evidence gaps of the picture it was computed over, and the next step.
//
// Probabilities are reported as the predictor's native values. Nothing here
// claims they are calibrated, and no sentence turns a missing answer into a
// verdict.
func (r *Renderer) Prediction(p decision.PredictionRecord, opts Options) ([]Sentence, error) {
	result := p.Config()
	request := p.Request()
	rc := request.Config()
	task := request.Task().Config()
	picture := request.Picture()
	pc := picture.Config()
	b := r.begin("prediction:"+opaqueID([]byte(p.ID())), opts)
	baseRefs := []Ref{
		idRef("decision_request", request.ID()),
		idRef("picture", request.PictureID()),
		idRef("predictor", request.PredictorID()),
	}

	answered, abstained := 0, 0
	for _, a := range result.Answers {
		if a.Status == decision.Answered {
			answered++
		} else {
			abstained++
		}
	}
	args := Args{
		"task":      b.ident(task.Name),
		"version":   b.ident(task.Version),
		"at":        result.CompletedAt,
		"answered":  answered,
		"abstained": abstained,
		"subjects":  len(rc.SubjectIDs),
		"questions": len(task.Questions),
		"principal": b.ident(string(rc.PrincipalID)),
		"requested": rc.RequestedAt,
		"deadline":  rc.Deadline,
		"release":   b.ident(string(rc.ReleaseID)),
	}

	knownStatus := false
	for _, s := range ResultStatuses {
		knownStatus = knownStatus || s == result.Status
	}
	if !knownStatus {
		// Unreachable for a record built by decision.NewPredictionRecord,
		// which refuses any other status; refused rather than guessed at.
		return nil, fmt.Errorf("narrate: unrecognized decision result status %q", result.Status)
	}
	b.add(RoleOutcome, "decision.outcome."+string(result.Status), args, baseRefs...)

	// Why a whole request has no answers. Since #556 a predictor cannot
	// return one of the service's reasons, but a result written before then
	// could, and the record does not say which build wrote it, so the reason
	// is attributed to both (see DecisionServiceReasons).
	reasonKey := ""
	if result.Status != decision.Completed {
		if knownDecisionReason(result.Status, result.Reason) {
			reasonKey = string(result.Status) + "." + result.Reason
		} else {
			reasonKey = string(result.Status) + ".unrecognized"
		}
		b.add(RoleReason, "decision.reason."+reasonKey, withArgs(args, Args{
			"reason": b.quote(AttributedToPredictorOrService, "", result.Reason),
		}))
	}

	// Each answer.
	questions := map[shoal.ID]decision.Question{}
	for _, q := range task.Questions {
		questions[q.ID] = q
	}
	evidence := map[shoal.ID][]Ref{}
	for _, s := range pc.Subjects {
		for _, id := range s.EvidenceIDs {
			evidence[s.ID] = append(evidence[s.ID], idRef("evidence", id))
		}
	}
	limit := opts.answers()
	for i, a := range result.Answers {
		if i == limit {
			b.add(RoleReason, "decision.answers.more", Args{"n": len(result.Answers) - limit})
			break
		}
		b.answer(a, questions[a.QuestionID], append([]Ref{idRef("subject", a.SubjectID)}, evidence[a.SubjectID]...))
	}

	// What the picture could not establish.
	b.pictureGaps(picture)

	b.add(RoleHistory, "decision.history.requested", args,
		idRef("principal", rc.PrincipalID), idRef("release", rc.ReleaseID),
		idRef("correlation", rc.CorrelationID))

	// What next.
	if result.Status == decision.Completed {
		b.add(RoleNext, "decision.next.completed", args)
	} else {
		b.add(RoleNext, "decision.next."+reasonKey, args)
	}
	return b.finish()
}

func (b *builder) answer(a decision.Answer, q decision.Question, refs []Ref) {
	args := Args{
		"subject":  b.ident(string(a.SubjectID)),
		"question": b.ident(string(a.QuestionID)),
	}
	switch a.Status {
	case decision.AnswerAbstained:
		if knownAnswerReason(a.Reason) {
			b.add(RoleReason, "decision.answer.abstained."+a.Reason, args, refs...)
			return
		}
		b.add(RoleReason, "decision.answer.abstained.unrecognized", withArgs(args, Args{
			"reason": b.quote(AttributedToPredictor, "", a.Reason),
		}), refs...)
		return
	case decision.Answered:
	default:
		b.fail(fmt.Errorf("narrate: unrecognized answer status %q", a.Status))
		return
	}
	switch q.Kind {
	case decision.Probability:
		probability := 0.0
		if a.Probability != nil {
			probability = *a.Probability
		}
		b.add(RoleReason, "decision.answer.probability", withArgs(args, Args{"p": probability}), refs...)
		return
	case decision.Choice, decision.Ordinal:
		reported := false
		probability := 0.0
		for _, d := range a.Distribution {
			if d.Label == a.Label {
				reported, probability = true, d.Probability
			}
		}
		rank := 0
		for i, label := range q.Labels {
			if label == a.Label {
				rank = i + 1
			}
		}
		b.add(RoleReason, "decision.answer."+string(q.Kind), withArgs(args, Args{
			"label":    b.ident(a.Label),
			"reported": yesNo(reported),
			"p":        probability,
			"rank":     rank,
			"levels":   len(q.Labels),
		}), refs...)
		return
	}
	b.fail(fmt.Errorf("narrate: unrecognized answer kind %q", q.Kind))
}

// pictureGaps states what the evidence picture could not establish:
// truncation, subjects without usable evidence, and coverage measurements
// that are partial, unknown or not applicable.
func (b *builder) pictureGaps(picture decision.PictureManifest) {
	pc := picture.Config()
	if pc.Truncated {
		b.add(RoleGap, "decision.gap.truncated", Args{
			"tokens": pc.InputTokens, "budget": pc.TokenBudget,
		}, idRef("picture", picture.ID()))
	}
	for _, disposition := range Dispositions {
		if disposition == decision.Supported {
			continue
		}
		var names []Fragment
		var refs []Ref
		for _, s := range pc.Subjects {
			if s.Disposition == disposition {
				names = append(names, b.ident(string(s.ID)))
				refs = append(refs, idRef("subject", s.ID))
			}
		}
		if len(names) > 0 {
			b.add(RoleGap, "decision.gap.disposition."+string(disposition), Args{
				"n": len(names), "subjects": b.bounded(names),
			}, refs...)
		}
	}
	shown, hidden := 0, 0
	for _, s := range pc.Subjects {
		known := false
		for _, d := range Dispositions {
			known = known || d == s.Disposition
		}
		if !known {
			b.fail(fmt.Errorf("narrate: unrecognized subject disposition %q", s.Disposition))
			return
		}
		if s.Disposition == decision.Supported || s.Reason == "" {
			continue
		}
		if shown == b.opts.listItems() {
			hidden++
			continue
		}
		shown++
		b.add(RoleGap, "decision.gap.subject_reason", Args{
			"subject": b.ident(string(s.ID)),
			"reason":  b.quote(AttributedToEvidenceBuilder, "", s.Reason),
		}, idRef("subject", s.ID))
	}
	if hidden > 0 {
		b.add(RoleGap, "decision.gap.more_reasons", Args{"n": hidden})
	}
	for _, m := range pc.Measurements {
		args := Args{
			"measurement": b.ident(string(m.ID)),
			"unit":        b.ident(m.Unit),
			"n":           m.Numerator,
		}
		refs := []Ref{idRef("measurement", m.ID), idRef("method", m.MethodID)}
		switch {
		case m.Denominator == nil:
			b.add(RoleGap, "decision.gap.measurement.unknown", args, refs...)
		case *m.Denominator == 0:
			b.add(RoleGap, "decision.gap.measurement.not_applicable", args, refs...)
		case m.Numerator < *m.Denominator:
			b.add(RoleGap, "decision.gap.measurement.partial",
				withArgs(args, Args{"d": *m.Denominator}), refs...)
		}
	}
	if supported, total := picture.InventoryCoverage(); supported < total {
		b.add(RoleGap, "decision.gap.inventory", Args{
			"supported": supported, "total": total,
		}, idRef("enumeration", pc.EnumerationID))
	}
}

// Eligibility narrates an evidence-eligibility record: how many subjects may
// be scored and, for the rest, each reason with the subjects it applies to.
func (r *Renderer) Eligibility(e decision.EvidenceEligibility, opts Options) ([]Sentence, error) {
	entries := e.Entries()
	reasons := make([]inspection, len(entries))
	for i, entry := range entries {
		reasons[i] = inspection{subject: entry.SubjectID, reasons: entry.Reasons}
	}
	b := r.begin("eligibility:"+opaqueID([]byte(e.ID())), opts)
	b.inspection("decision.eligibility", reasons, idRef("decision_request", e.RequestID()))
	return b.finish()
}

// Ranking narrates an inspection ranking: how many subjects were scored, why
// each unscored subject was not, and that no position permits exclusion.
// Scores themselves are not narrated: they are an ordering, not a finding.
func (r *Renderer) Ranking(x decision.InspectionRanking, opts Options) ([]Sentence, error) {
	entries := x.Entries()
	reasons := make([]inspection, len(entries))
	for i, entry := range entries {
		reasons[i] = inspection{subject: entry.SubjectID, reasons: entry.Reasons}
	}
	b := r.begin("ranking:"+opaqueID([]byte(x.ID())), opts)
	b.inspection("decision.ranking", reasons,
		idRef("prediction", x.PredictionID()), idRef("picture", x.PictureID()))
	return b.finish()
}

type inspection struct {
	subject shoal.ID
	reasons []decision.InspectionReason
}

func (b *builder) inspection(prefix string, entries []inspection, refs ...Ref) {
	eligible := 0
	for _, e := range entries {
		if len(e.reasons) == 0 {
			eligible++
		}
	}
	args := Args{"eligible": eligible, "total": len(entries), "held": len(entries) - eligible}
	b.add(RoleOutcome, prefix+".outcome", args, refs...)
	for _, reason := range InspectionReasons {
		var names []Fragment
		var subjectRefs []Ref
		for _, e := range entries {
			for _, got := range e.reasons {
				if got == reason {
					names = append(names, b.ident(string(e.subject)))
					subjectRefs = append(subjectRefs, idRef("subject", e.subject))
					break
				}
			}
		}
		if len(names) > 0 {
			b.add(RoleReason, "decision.inspection."+string(reason), Args{
				"n": len(names), "subjects": b.bounded(names),
			}, subjectRefs...)
		}
	}
	for _, e := range entries {
		for _, got := range e.reasons {
			known := false
			for _, reason := range InspectionReasons {
				known = known || reason == got
			}
			if !known {
				b.fail(fmt.Errorf("narrate: unrecognized inspection reason %q", got))
				return
			}
		}
	}
	b.add(RoleNext, prefix+".next", args)
}
