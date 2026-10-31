// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisiondatasets

import (
	"context"
	"reflect"
	"sort"
	"time"

	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// A class sample is the per-target material a class record is computed from:
// which class each registered member's prediction falls in, when its evidence
// was observed, and what the adjudication journal and outcome inventory say
// about it as of the cohort cutoff. It is taken under the same authority as a
// dataset export, so a count can never include a target an export would
// refuse, and it carries no features, sources, or receipts.
//
// The cutoff is the cohort's declared label time; the window is declared by
// the reader over observation times and never moves the cutoff. Unknown,
// disputed, and unadjudicated targets stay in every denominator.

const (
	StatusVerified   = "verified"
	StatusDisputed   = "disputed"
	StatusUnresolved = "unresolved"
	StatusNone       = "none"
)

// SampleOutcome is one outcome receipt in the selected adjudication's basis.
// Supersedes names the receipt it replaces, if any; a superseded receipt is
// counted once, under its successor.
type SampleOutcome struct {
	ReceiptID  shoal.ID
	Kind       decision.OutcomeKind
	Supersedes shoal.ID
}

// SampleRow is one registered member of the cohort.
type SampleRow struct {
	MemberID, TargetID shoal.ID
	Class              decision.DecisionClass
	ObservedAt         time.Time
	// Status is the disposition of the adjudication in force at the cutoff;
	// VerifiedLabel is set only when Status is verified.
	Status        string
	VerifiedLabel string
	// These qualifiers are the conditions under which an export withholds a
	// verified label, and the sample demotes the same label to unresolved:
	// Withdrawn, a label the authority withdrew; Stale, an outcome inventory
	// the selected adjudication's basis does not match; Later, an
	// adjudication received after the cutoff; TrainingDenied, a label the
	// pinned policy does not permit for training. A record never applies a
	// label an export would null.
	Later, Withdrawn, Stale, TrainingDenied bool
	OutcomeReceipts                         int
	BasisOutcomes                           []SampleOutcome
}

// Sample is the complete validated per-target material of one cohort.
type Sample struct {
	CohortID                     shoal.ID
	TaskID, QuestionID, PolicyID shoal.ID
	Labels                       []string
	Cutoff                       time.Time
	Rows                         []SampleRow
}

// Window bounds the observations a record counts: inclusive, and never past
// the cutoff.
type Window struct{ From, To time.Time }

// Adjudication counts the adjudication in force at the cutoff for each target.
type Adjudication struct {
	VerifiedAgree, VerifiedDisagree, Disputed, Unresolved, None int
	Withdrawn, Stale, Later, TrainingDenied                     int
}

// Outcomes counts the outcome inventory and the selected bases' correctness
// receipts, a superseded receipt counted once under its successor. A basis
// carries only correctness receipts (NewAdjudicationBasis refuses any
// other kind), so execution receipts are in Receipts and nowhere else.
type Outcomes struct {
	Receipts, Correctness, Superseded int
}

// ClassRecord is the measured record of one decision class over a window.
type ClassRecord struct {
	ClassID      shoal.ID
	Class        decision.DecisionClass
	Targets      int
	Adjudication Adjudication
	Outcomes     Outcomes
	// Precision is verified agreements over verified adjudications, and nil
	// when nothing is verified. It says nothing about the unverified share,
	// which Targets and Adjudication report.
	Precision                   *float64
	FirstObserved, LastObserved time.Time
}

// Records is every class of a cohort with at least one target in the window.
type Records struct {
	CohortID                     shoal.ID
	TaskID, QuestionID, PolicyID shoal.ID
	Cutoff                       time.Time
	Window                       Window
	Members, InWindow, Outside   int
	Classes                      []ClassRecord
}

func predictedClass(c Cohort, m Member, p decision.PredictionRecord, labels []string) (decision.DecisionClass, error) {
	class := decision.DecisionClass{TaskID: c.Task.ID(), QuestionID: c.QuestionID, PolicyID: c.Policy.ID()}
	cfg := p.Config()
	switch cfg.Status {
	case decision.Completed:
	case decision.Abstained:
		class.Abstained = true
		return class, nil
	default:
		// A failed prediction decided nothing; it is in no class.
		return class, denied()
	}
	found := false
	for _, a := range cfg.Answers {
		if a.SubjectID != m.SubjectID || a.QuestionID != c.QuestionID {
			continue
		}
		if found {
			return class, denied()
		}
		found = true
		switch a.Status {
		case decision.Answered:
			if a.Label != labels[0] && a.Label != labels[1] {
				return class, denied()
			}
			class.Label = a.Label
		case decision.AnswerAbstained:
			class.Abstained = true
		default:
			return class, denied()
		}
	}
	if !found {
		return class, denied()
	}
	return class, nil
}

func sampleRow(c Cohort, m Member, t Target, labels []string, now time.Time) (SampleRow, error) {
	var zero SampleRow
	r := t.Prediction.Request()
	pc := r.Picture().Config()
	target, e := decision.AdjudicationTargetID(c.Task.ID(), r.Picture().ID(), m.SubjectID, c.QuestionID)
	if e != nil || t.Prediction.Validate() != nil || t.Prediction.ID() != m.PredictionID || r.ID() != m.RequestID || r.Task().ID() != c.Task.ID() || target != m.TargetID || !t.SourceAvailable || !t.InventoryComplete || !idOK(t.InventoryID) || len(pc.Sources) == 0 || pc.Cutoff.After(c.Cutoff) {
		return zero, denied()
	}
	subject := false
	for _, id := range r.Config().SubjectIDs {
		subject = subject || id == m.SubjectID
	}
	if !subject {
		return zero, denied()
	}
	observed := pc.Sources[0].ObservedAt
	for _, source := range pc.Sources {
		if source.Role == decision.ReportedOutcome || source.Role == decision.Prediction || source.ObservedAt.After(c.Cutoff) || source.ReceivedAt.After(c.Cutoff) || !stamp(source.ObservedAt) {
			return zero, denied()
		}
		if source.ObservedAt.After(observed) {
			observed = source.ObservedAt
		}
	}
	if len(t.History) > 128 || len(t.OutcomeReceiptIDs) > 256 {
		return zero, denied()
	}
	seen := map[shoal.ID]bool{}
	for _, id := range t.OutcomeReceiptIDs {
		if !receiptID(id, "outcome-receipt:") || seen[id] {
			return zero, denied()
		}
		seen[id] = true
	}
	selected, _, e := validateHistory(c, m, target, r.Picture().ID(), t.History, now)
	if e != nil {
		return zero, e
	}
	class, e := predictedClass(c, m, t.Prediction, labels)
	if e != nil {
		return zero, e
	}
	row := SampleRow{MemberID: m.ID, TargetID: target, Class: class, ObservedAt: observed.UTC(), Status: StatusNone, OutcomeReceipts: len(t.OutcomeReceiptIDs), Withdrawn: t.LabelWithdrawn, TrainingDenied: !t.TrainingAllowed}
	if selected < 0 {
		return row, nil
	}
	h := t.History[selected]
	b := t.SelectedBasis
	p := b.Proposal()
	if b.Validate() != nil || b.ID() != h.BasisID || p.ID() != h.ProposalID || p.TargetID() != target || p.PolicyID() != c.Policy.ID() || b.Config().CapturedAt.After(h.ReceivedAt) || b.Config().Cutoff.After(c.Cutoff) || h.ReceivedAt.Before(observed) {
		return zero, denied()
	}
	row.Later = selected != len(t.History)-1
	switch h.ProposalConfig.Disposition {
	case decision.AdjudicationVerified:
		label := h.ProposalConfig.Label
		if label != labels[0] && label != labels[1] {
			return zero, denied()
		}
		row.Status, row.VerifiedLabel = StatusVerified, label
	case decision.AdjudicationDisputed:
		row.Status = StatusDisputed
	case decision.AdjudicationUnresolved:
		row.Status = StatusUnresolved
	default:
		return zero, denied()
	}
	inventory := []shoal.ID{}
	for _, o := range b.Config().Outcomes {
		inventory = append(inventory, o.ReceiptID)
		row.BasisOutcomes = append(row.BasisOutcomes, SampleOutcome{ReceiptID: o.ReceiptID, Kind: o.Kind, Supersedes: o.Supersedes})
	}
	// A basis that does not match the inventory, in either direction, is
	// stale: the export quarantines the row rather than refusing it.
	row.Stale = !idsEqual(inventory, t.OutcomeReceiptIDs)
	if (row.Withdrawn || row.Stale || row.Later || row.TrainingDenied) && row.Status == StatusVerified {
		row.Status, row.VerifiedLabel = StatusUnresolved, ""
	}
	return row, nil
}

// ClassSample takes the per-target material of one registered cohort under
// the caller's current authority. Every member is loaded and jointly
// verified, as an export is; a member the caller may not read refuses the
// whole sample rather than thinning it.
func (s *Service) ClassSample(ctx context.Context, cohortID shoal.ID) (Sample, error) {
	var zero Sample
	if !idOK(cohortID) {
		return zero, denied()
	}
	d, e := s.caller(ctx, nil)
	if e != nil {
		return zero, e
	}
	c, e := s.config.Authority.Resolve(ctx, d, cohortID)
	if e != nil {
		return zero, denied()
	}
	if _, ok := hydratedSize(c, 4<<20); !ok {
		return zero, denied()
	}
	c = cloneCohort(c)
	now := s.config.Clock().Round(0).UTC()
	labels, e := cohortValid(c, cohortID, now)
	if e != nil {
		return zero, e
	}
	targets := make([]Target, len(c.Members))
	hydrated := 0
	for i, m := range c.Members {
		loaded, err := s.config.Authority.Load(ctx, d, cloneCohort(c), cloneMember(m))
		if err != nil {
			return zero, denied()
		}
		n, ok := preflightTarget(loaded, MaxHydratedBytes-hydrated)
		if !ok {
			return zero, denied()
		}
		hydrated += n
		targets[i] = cloneTarget(loaded)
	}
	sample := Sample{CohortID: c.ID, TaskID: c.Task.ID(), QuestionID: c.QuestionID, PolicyID: c.Policy.ID(), Labels: append([]string(nil), labels...), Cutoff: c.Cutoff.UTC()}
	for i, t := range targets {
		row, err := sampleRow(c, c.Members[i], t, labels, now)
		if err != nil {
			return zero, err
		}
		sample.Rows = append(sample.Rows, row)
	}
	sort.Slice(sample.Rows, func(i, j int) bool { return sample.Rows[i].MemberID < sample.Rows[j].MemberID })
	verifiedCohort, verifiedTargets := cloneCohort(c), cloneTargets(targets)
	if e = s.config.Authority.Verify(ctx, d, verifiedCohort, verifiedTargets); e != nil || !reflect.DeepEqual(c, verifiedCohort) || !reflect.DeepEqual(targets, verifiedTargets) {
		return zero, denied()
	}
	if _, e = s.caller(ctx, &d); e != nil {
		return zero, e
	}
	return sample, nil
}

func windowInvalid() error {
	return shoal.NewError(shoal.ErrorInvalidArgument, "class record window must be declared, ordered, and end at or before the cohort cutoff")
}

// Aggregate computes the measured record of every class in the sample with a
// target observed inside the window. It is deterministic and reads nothing.
func Aggregate(sample Sample, w Window) (Records, error) {
	var zero Records
	w.From, w.To = w.From.Round(0).UTC(), w.To.Round(0).UTC()
	if !stamp(w.From) || !stamp(w.To) || w.To.Before(w.From) || !stamp(sample.Cutoff) || w.To.After(sample.Cutoff) {
		return zero, windowInvalid()
	}
	if !idOK(sample.CohortID) || !idOK(sample.TaskID) || !idOK(sample.QuestionID) || !idOK(sample.PolicyID) || len(sample.Labels) != 2 || len(sample.Rows) > 256 {
		return zero, shoal.NewError(shoal.ErrorInvalidArgument, "invalid class sample")
	}
	out := Records{CohortID: sample.CohortID, TaskID: sample.TaskID, QuestionID: sample.QuestionID, PolicyID: sample.PolicyID, Cutoff: sample.Cutoff, Window: w, Members: len(sample.Rows)}
	byID := map[shoal.ID]*ClassRecord{}
	seen := map[shoal.ID]bool{}
	for _, row := range sample.Rows {
		if !idOK(row.MemberID) || seen[row.MemberID] || !stamp(row.ObservedAt) || row.ObservedAt.After(sample.Cutoff) {
			return zero, shoal.NewError(shoal.ErrorInvalidArgument, "invalid class sample")
		}
		seen[row.MemberID] = true
		id, err := row.Class.ID()
		if err != nil || row.Class.TaskID != sample.TaskID || row.Class.QuestionID != sample.QuestionID || row.Class.PolicyID != sample.PolicyID {
			return zero, shoal.NewError(shoal.ErrorInvalidArgument, "invalid class sample")
		}
		if row.ObservedAt.Before(w.From) || row.ObservedAt.After(w.To) {
			out.Outside++
			continue
		}
		out.InWindow++
		rec := byID[id]
		if rec == nil {
			rec = &ClassRecord{ClassID: id, Class: row.Class, FirstObserved: row.ObservedAt, LastObserved: row.ObservedAt}
			byID[id] = rec
		}
		rec.Targets++
		if row.ObservedAt.Before(rec.FirstObserved) {
			rec.FirstObserved = row.ObservedAt
		}
		if row.ObservedAt.After(rec.LastObserved) {
			rec.LastObserved = row.ObservedAt
		}
		switch row.Status {
		case StatusVerified:
			if row.Withdrawn || row.Stale || row.Later || row.TrainingDenied {
				return zero, shoal.NewError(shoal.ErrorInvalidArgument, "invalid class sample")
			}
			if row.Class.Abstained || row.VerifiedLabel != row.Class.Label {
				rec.Adjudication.VerifiedDisagree++
			} else {
				rec.Adjudication.VerifiedAgree++
			}
		case StatusDisputed:
			rec.Adjudication.Disputed++
		case StatusUnresolved:
			rec.Adjudication.Unresolved++
		case StatusNone:
			rec.Adjudication.None++
		default:
			return zero, shoal.NewError(shoal.ErrorInvalidArgument, "invalid class sample")
		}
		if row.Withdrawn {
			rec.Adjudication.Withdrawn++
		}
		if row.Stale {
			rec.Adjudication.Stale++
		}
		if row.Later {
			rec.Adjudication.Later++
		}
		if row.TrainingDenied {
			rec.Adjudication.TrainingDenied++
		}
		rec.Outcomes.Receipts += row.OutcomeReceipts
		superseded := map[shoal.ID]bool{}
		for _, o := range row.BasisOutcomes {
			if o.Supersedes != "" {
				superseded[o.Supersedes] = true
			}
		}
		for _, o := range row.BasisOutcomes {
			if superseded[o.ReceiptID] {
				rec.Outcomes.Superseded++
				continue
			}
			if o.Kind != decision.OutcomeCorrectness {
				return zero, shoal.NewError(shoal.ErrorInvalidArgument, "invalid class sample")
			}
			rec.Outcomes.Correctness++
		}
	}
	for _, rec := range byID {
		if n := rec.Adjudication.VerifiedAgree + rec.Adjudication.VerifiedDisagree; n > 0 {
			p := float64(rec.Adjudication.VerifiedAgree) / float64(n)
			rec.Precision = &p
		}
		out.Classes = append(out.Classes, *rec)
	}
	sort.Slice(out.Classes, func(i, j int) bool { return out.Classes[i].ClassID < out.Classes[j].ClassID })
	return out, nil
}
