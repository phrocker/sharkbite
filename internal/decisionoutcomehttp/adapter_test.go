// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisionoutcomehttp

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	outcomes "github.com/phrocker/shoal-oss/internal/decisionoutcomes"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/decision/api"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestProjectionPreservesOpaqueAttributionAndFalseTruth(t *testing.T) {
	no := false
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	r := outcomes.Receipt{ID: shoal.ID("outcome-receipt:" + strings.Repeat("a", 64)), ObservationID: shoal.ID("decision:outcome:v1:" + strings.Repeat("c", 64)), ObservationConfig: decision.OutcomeObservationConfig{RequestID: "request", PredictionID: "prediction", SubjectID: "subject", Kind: decision.OutcomeCorrectness, QuestionID: "question", Truth: &no, EvidenceIDs: []shoal.ID{"evidence"}, ObservedAt: now, AssertedProvenance: decision.OutcomeProvenance{ReporterID: "asserted-reporter"}}, SubmitterID: shoal.ID(string([]byte{0, 255})), ActorID: shoal.ID(string([]byte{254})), ClientID: "", OnBehalfOf: []shoal.ID{shoal.ID(string([]byte{253, 0}))}, AuthorizationFingerprint: "auth-sha256:" + strings.Repeat("b", 64), ReceivedAt: now, State: "proposed"}
	got, e := project(r)
	if e != nil {
		t.Fatal(e)
	}
	raw, e := api.EncodeOutcomeReceipt(got)
	if e != nil {
		t.Fatal(e)
	}
	decoded, e := api.DecodeOutcomeReceipt(raw)
	if e != nil || !reflect.DeepEqual(decoded, got) || decoded.Observation.Truth == nil || *decoded.Observation.Truth {
		t.Fatal("wire corrupted original receipt", e)
	}
	*got.Observation.Truth = true
	got.Observation.EvidenceIDs[0] = "changed"
	got.OnBehalfOf[0] = "changed"
	if *r.ObservationConfig.Truth || r.ObservationConfig.EvidenceIDs[0] != "evidence" || r.OnBehalfOf[0] == "changed" {
		t.Fatal("projection aliases original")
	}
}
func TestMissingStoreFailsClosed(t *testing.T) {
	if _, e := New(nil); e == nil {
		t.Fatal("nil store accepted")
	}
	var a *Adapter
	if _, e := a.AppendOutcome(context.Background(), api.OutcomeObservation{}, []byte("key")); e == nil {
		t.Fatal("missing append store")
	}
	if _, e := a.ReadOutcome(context.Background(), "request", "prediction", []byte("key")); e == nil {
		t.Fatal("missing read store")
	}
}
