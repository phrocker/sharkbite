// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// MaxClassRecordBytes bounds one class record response.
const MaxClassRecordBytes = 1 << 20

// ClassRecordRequest names a registered cohort and the observation window
// the record is declared over. From is required. A zero To declares the
// window to end at the cohort's own cutoff, which the provider resolves and
// the response states; a record is never computed over an undeclared span.
type ClassRecordRequest struct {
	CohortID shoal.ID
	From, To time.Time
}

// ClassAdjudication counts, per target in the class, the adjudication in
// force at the cohort cutoff. Unknown, disputed, and unadjudicated targets are
// in every denominator; Withdrawn, Stale, and Later are qualifiers of counts
// already made, not extra targets.
type ClassAdjudication struct {
	VerifiedAgree    int `json:"verified_agree"`
	VerifiedDisagree int `json:"verified_disagree"`
	Disputed         int `json:"disputed"`
	Unresolved       int `json:"unresolved"`
	None             int `json:"none"`
	Withdrawn        int `json:"withdrawn"`
	Stale            int `json:"stale"`
	Later            int `json:"later"`
}

// ClassOutcomes counts the outcome inventory and the basis receipts by kind,
// a superseded receipt counted once under its successor.
type ClassOutcomes struct {
	Receipts    int `json:"receipts"`
	Execution   int `json:"execution"`
	Correctness int `json:"correctness"`
	Superseded  int `json:"superseded"`
}

// ClassRecord is the measured record of one decision class.
type ClassRecord struct {
	ClassID       shoal.ID
	Class         decision.DecisionClass
	Targets       int
	Adjudication  ClassAdjudication
	Outcomes      ClassOutcomes
	Precision     *float64
	FirstObserved time.Time
	LastObserved  time.Time
}

// ClassRecords is every class of one cohort with a target in the window.
type ClassRecords struct {
	CohortID                     shoal.ID
	TaskID, QuestionID, PolicyID shoal.ID
	Cutoff                       time.Time
	From, To                     time.Time
	Members, InWindow, Outside   int
	Classes                      []ClassRecord
}

// ClassRecordProvider computes records under the caller's current authority.
// The provider must refuse a cohort the caller may not read in full; a record
// is never thinned to the visible part of a cohort.
type ClassRecordProvider interface {
	ClassRecords(context.Context, ClassRecordRequest) (ClassRecords, error)
}

type classRequestWire struct {
	CohortID string `json:"cohort_id"`
	From     string `json:"from"`
	To       string `json:"to,omitempty"`
}
type classWire struct {
	ID         string `json:"class_id"`
	TaskID     string `json:"task_id"`
	QuestionID string `json:"question_id"`
	PolicyID   string `json:"policy_id"`
	Label      string `json:"label"`
	Abstained  bool   `json:"abstained"`
}
type classRecordWire struct {
	Class         classWire         `json:"class"`
	Targets       int               `json:"targets"`
	Adjudication  ClassAdjudication `json:"adjudication"`
	Outcomes      ClassOutcomes     `json:"outcomes"`
	Precision     *float64          `json:"precision"`
	FirstObserved string            `json:"first_observed"`
	LastObserved  string            `json:"last_observed"`
}
type classRecordsWire struct {
	Schema     int               `json:"schema"`
	CohortID   string            `json:"cohort_id"`
	TaskID     string            `json:"task_id"`
	QuestionID string            `json:"question_id"`
	PolicyID   string            `json:"policy_id"`
	Cutoff     string            `json:"cutoff"`
	From       string            `json:"from"`
	To         string            `json:"to"`
	Members    int               `json:"members"`
	InWindow   int               `json:"in_window"`
	Outside    int               `json:"outside_window"`
	Classes    []classRecordWire `json:"classes"`
}

func classInvalid() error { return errors.New("invalid class record") }
func classTime(t time.Time) bool {
	return !t.IsZero() && t.Year() >= 1 && t.Year() <= 9999
}
func classStamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
func parseClassStamp(s string) (time.Time, error) {
	t, e := time.Parse(time.RFC3339Nano, s)
	if e != nil || !classTime(t) || classStamp(t) != s {
		return time.Time{}, classInvalid()
	}
	return t.UTC(), nil
}

// Validate checks the request's shape only; the window's relation to the
// cohort cutoff is the provider's to check.
func (r ClassRecordRequest) Validate() error {
	if !datasetID(r.CohortID) || !classTime(r.From) || (!r.To.IsZero() && (!classTime(r.To) || r.To.Before(r.From))) {
		return classInvalid()
	}
	return nil
}
func optionalClassStamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return classStamp(t)
}
func parseOptionalClassStamp(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return parseClassStamp(s)
}
func EncodeClassRecordRequest(r ClassRecordRequest) ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	b, err := json.Marshal(classRequestWire{EncodeID(r.CohortID), classStamp(r.From), optionalClassStamp(r.To)})
	if err != nil {
		return nil, classInvalid()
	}
	return b, nil
}
func DecodeClassRecordRequest(raw []byte) (ClassRecordRequest, error) {
	var out ClassRecordRequest
	if len(raw) > 4096 {
		return out, classInvalid()
	}
	var w classRequestWire
	if err := decodeStrict(raw, &w); err != nil {
		return out, classInvalid()
	}
	id, err := DecodeID(w.CohortID)
	if err != nil {
		return out, classInvalid()
	}
	from, err := parseClassStamp(w.From)
	if err != nil {
		return out, err
	}
	to, err := parseOptionalClassStamp(w.To)
	if err != nil {
		return out, err
	}
	out = ClassRecordRequest{id, from, to}
	if err = out.Validate(); err != nil {
		return ClassRecordRequest{}, err
	}
	return out, nil
}

func (r ClassRecord) validate(records ClassRecords) error {
	id, err := r.Class.ID()
	if err != nil || id != r.ClassID || r.Class.TaskID != records.TaskID || r.Class.QuestionID != records.QuestionID || r.Class.PolicyID != records.PolicyID {
		return classInvalid()
	}
	a, o := r.Adjudication, r.Outcomes
	counts := []int{r.Targets, a.VerifiedAgree, a.VerifiedDisagree, a.Disputed, a.Unresolved, a.None, a.Withdrawn, a.Stale, a.Later, o.Receipts, o.Execution, o.Correctness, o.Superseded}
	for _, n := range counts {
		if n < 0 {
			return classInvalid()
		}
	}
	if r.Targets < 1 || a.VerifiedAgree+a.VerifiedDisagree+a.Disputed+a.Unresolved+a.None != r.Targets || a.Withdrawn > r.Targets || a.Stale > r.Targets || a.Later > r.Targets {
		return classInvalid()
	}
	verified := a.VerifiedAgree + a.VerifiedDisagree
	if (r.Precision == nil) != (verified == 0) {
		return classInvalid()
	}
	if r.Precision != nil && (math.IsNaN(*r.Precision) || *r.Precision != float64(a.VerifiedAgree)/float64(verified)) {
		return classInvalid()
	}
	if !classTime(r.FirstObserved) || !classTime(r.LastObserved) || r.LastObserved.Before(r.FirstObserved) || r.FirstObserved.Before(records.From) || r.LastObserved.After(records.To) {
		return classInvalid()
	}
	return nil
}

// Validate checks internal consistency: every class belongs to the cohort's
// task, question, and policy, its counts add up, and its observations lie in
// the declared window, which lies at or before the cutoff.
func (r ClassRecords) Validate() error {
	for _, id := range []shoal.ID{r.CohortID, r.TaskID, r.QuestionID, r.PolicyID} {
		if !datasetID(id) {
			return classInvalid()
		}
	}
	if !classTime(r.Cutoff) || !classTime(r.From) || !classTime(r.To) || r.To.Before(r.From) || r.To.After(r.Cutoff) || r.Members < 0 || r.InWindow < 0 || r.Outside < 0 || r.InWindow+r.Outside != r.Members || len(r.Classes) > 512 {
		return classInvalid()
	}
	total := 0
	var previous shoal.ID
	for i, c := range r.Classes {
		if err := c.validate(r); err != nil {
			return err
		}
		if i > 0 && c.ClassID <= previous {
			return classInvalid()
		}
		previous = c.ClassID
		total += c.Targets
	}
	if total != r.InWindow {
		return classInvalid()
	}
	return nil
}

func EncodeClassRecords(r ClassRecords) ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	w := classRecordsWire{Schema: 1, CohortID: EncodeID(r.CohortID), TaskID: EncodeID(r.TaskID), QuestionID: EncodeID(r.QuestionID), PolicyID: EncodeID(r.PolicyID), Cutoff: classStamp(r.Cutoff), From: classStamp(r.From), To: classStamp(r.To), Members: r.Members, InWindow: r.InWindow, Outside: r.Outside, Classes: []classRecordWire{}}
	for _, c := range r.Classes {
		var p *float64
		if c.Precision != nil {
			v := *c.Precision
			p = &v
		}
		w.Classes = append(w.Classes, classRecordWire{Class: classWire{EncodeID(c.ClassID), EncodeID(c.Class.TaskID), EncodeID(c.Class.QuestionID), EncodeID(c.Class.PolicyID), c.Class.Label, c.Class.Abstained}, Targets: c.Targets, Adjudication: c.Adjudication, Outcomes: c.Outcomes, Precision: p, FirstObserved: classStamp(c.FirstObserved), LastObserved: classStamp(c.LastObserved)})
	}
	b, err := json.Marshal(w)
	if err != nil || len(b) > MaxClassRecordBytes {
		return nil, classInvalid()
	}
	return b, nil
}

func DecodeClassRecords(raw []byte) (ClassRecords, error) {
	var out ClassRecords
	if len(raw) > MaxClassRecordBytes {
		return out, classInvalid()
	}
	var w classRecordsWire
	if err := decodeStrict(raw, &w); err != nil || w.Schema != 1 {
		return out, classInvalid()
	}
	ids := [4]shoal.ID{}
	for i, s := range []string{w.CohortID, w.TaskID, w.QuestionID, w.PolicyID} {
		id, err := DecodeID(s)
		if err != nil {
			return out, classInvalid()
		}
		ids[i] = id
	}
	times := [3]time.Time{}
	for i, s := range []string{w.Cutoff, w.From, w.To} {
		t, err := parseClassStamp(s)
		if err != nil {
			return out, err
		}
		times[i] = t
	}
	out = ClassRecords{CohortID: ids[0], TaskID: ids[1], QuestionID: ids[2], PolicyID: ids[3], Cutoff: times[0], From: times[1], To: times[2], Members: w.Members, InWindow: w.InWindow, Outside: w.Outside}
	for _, c := range w.Classes {
		var rec ClassRecord
		var err error
		if rec.ClassID, err = DecodeID(c.Class.ID); err != nil {
			return ClassRecords{}, classInvalid()
		}
		if rec.Class.TaskID, err = DecodeID(c.Class.TaskID); err != nil {
			return ClassRecords{}, classInvalid()
		}
		if rec.Class.QuestionID, err = DecodeID(c.Class.QuestionID); err != nil {
			return ClassRecords{}, classInvalid()
		}
		if rec.Class.PolicyID, err = DecodeID(c.Class.PolicyID); err != nil {
			return ClassRecords{}, classInvalid()
		}
		rec.Class.Label, rec.Class.Abstained = c.Class.Label, c.Class.Abstained
		rec.Targets, rec.Adjudication, rec.Outcomes = c.Targets, c.Adjudication, c.Outcomes
		if c.Precision != nil {
			v := *c.Precision
			rec.Precision = &v
		}
		if rec.FirstObserved, err = parseClassStamp(c.FirstObserved); err != nil {
			return ClassRecords{}, err
		}
		if rec.LastObserved, err = parseClassStamp(c.LastObserved); err != nil {
			return ClassRecords{}, err
		}
		out.Classes = append(out.Classes, rec)
	}
	if err := out.Validate(); err != nil {
		return ClassRecords{}, err
	}
	return out, nil
}
