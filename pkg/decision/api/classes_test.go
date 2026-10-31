// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package api

import (
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func classRecordsFixture(t *testing.T) ClassRecords {
	t.Helper()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	class := decision.DecisionClass{TaskID: "task", QuestionID: "q", PolicyID: "policy", Label: "inspect"}
	id, err := class.ID()
	if err != nil {
		t.Fatal(err)
	}
	p := 0.5
	return ClassRecords{CohortID: "cohort", TaskID: "task", QuestionID: "q", PolicyID: "policy", Cutoff: now, From: now.Add(-time.Hour), To: now, Members: 3, InWindow: 2, Outside: 1, Classes: []ClassRecord{{ClassID: id, Class: class, Targets: 2, Adjudication: ClassAdjudication{VerifiedAgree: 1, VerifiedDisagree: 1}, Outcomes: ClassOutcomes{Receipts: 2, Correctness: 2}, Precision: &p, FirstObserved: now.Add(-time.Minute), LastObserved: now.Add(-time.Minute)}}}
}

func TestClassRecordsRoundTripAndValidation(t *testing.T) {
	r := classRecordsFixture(t)
	b, err := EncodeClassRecords(r)
	if err != nil {
		t.Fatal(err)
	}
	back, err := DecodeClassRecords(b)
	if err != nil || back.CohortID != r.CohortID || len(back.Classes) != 1 || back.Classes[0].ClassID != r.Classes[0].ClassID || *back.Classes[0].Precision != 0.5 || !back.Classes[0].FirstObserved.Equal(r.Classes[0].FirstObserved) {
		t.Fatalf("%v %+v", err, back)
	}
	for name, change := range map[string]func(*ClassRecords){
		"sum":                 func(r *ClassRecords) { r.InWindow = 3 },
		"counts":              func(r *ClassRecords) { r.Classes[0].Adjudication.None = 1 },
		"precision":           func(r *ClassRecords) { v := 0.75; r.Classes[0].Precision = &v },
		"missing precision":   func(r *ClassRecords) { r.Classes[0].Precision = nil },
		"foreign class":       func(r *ClassRecords) { r.Classes[0].Class.TaskID = "other" },
		"forged id":           func(r *ClassRecords) { r.Classes[0].ClassID = shoal.ID("decision-class:" + strings.Repeat("0", 64)) },
		"window past cutoff":  func(r *ClassRecords) { r.To = r.Cutoff.Add(time.Second) },
		"observation outside": func(r *ClassRecords) { r.Classes[0].LastObserved = r.To.Add(time.Second) },
		"negative":            func(r *ClassRecords) { r.Outside = -1; r.Members = 1 },
		"stale beyond adjudicated": func(r *ClassRecords) {
			r.Classes[0].Adjudication = ClassAdjudication{None: 2, Stale: 1}
			r.Classes[0].Precision = nil
		},
		"basis beyond inventory": func(r *ClassRecords) { r.Classes[0].Outcomes.Correctness = 3 },
	} {
		bad := classRecordsFixture(t)
		change(&bad)
		if _, err := EncodeClassRecords(bad); err == nil {
			t.Fatalf("%s encoded", name)
		}
	}
	for _, raw := range []string{`null`, `{}`, `{"schema":2}`, strings.Replace(string(b), `"schema":1`, `"schema":1,"extra":1`, 1), strings.Replace(string(b), `"members":3`, `"members":3,"members":3`, 1)} {
		if _, err := DecodeClassRecords([]byte(raw)); err == nil {
			t.Fatalf("decoded %s", raw)
		}
	}
}

func TestClassRecordRequestCodec(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	req := ClassRecordRequest{CohortID: "cohort", From: now.Add(-time.Hour), To: now}
	b, err := EncodeClassRecordRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	back, err := DecodeClassRecordRequest(b)
	if err != nil || back.CohortID != "cohort" || !back.From.Equal(req.From) || !back.To.Equal(req.To) {
		t.Fatalf("%v %+v", err, back)
	}
	open, err := DecodeClassRecordRequest([]byte(`{"cohort_id":"Y29ob3J0","from":"2026-10-06T11:00:00Z"}`))
	if err != nil || !open.To.IsZero() || open.CohortID != "cohort" {
		t.Fatalf("open-ended window: %v %+v", err, open)
	}
	if b, err := EncodeClassRecordRequest(open); err != nil || strings.Contains(string(b), `"to"`) {
		t.Fatalf("open-ended window encodes a to: %v %s", err, b)
	}
	for _, raw := range []string{
		`{}`, `{"cohort_id":"Y29ob3J0"}`, `{"cohort_id":"Y29ob3J0","from":"x","to":"2026-10-06T12:00:00Z"}`, `{"cohort_id":"Y29ob3J0","from":"2026-10-06T11:00:00Z","to":"x"}`,
		`{"cohort_id":"Y29ob3J0","from":"2026-10-06T12:00:00Z","to":"2026-10-06T11:00:00Z"}`,
		`{"cohort_id":"Y29ob3J0","from":"2026-10-06T12:00:00+01:00","to":"2026-10-06T12:00:00Z"}`,
		`{"cohort_id":"bad=","from":"2026-10-06T11:00:00Z","to":"2026-10-06T12:00:00Z"}`,
		`{"cohort_id":"Y29ob3J0","from":"2026-10-06T11:00:00Z","to":"2026-10-06T12:00:00Z","x":1}`,
		`{"cohort_id":"Y29ob3J0","from":"2026-10-06T11:00:00Z","to":""}`,
		`{"cohort_id":"Y29ob3J0","from":"2026-10-06T11:00:00Z","to":null}`,
	} {
		if _, err := DecodeClassRecordRequest([]byte(raw)); err == nil {
			t.Fatalf("decoded %s", raw)
		}
	}
	if _, err := EncodeClassRecordRequest(ClassRecordRequest{CohortID: "cohort"}); err == nil {
		t.Fatal("undeclared window encoded")
	}
}
