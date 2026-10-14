// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisionoutcomes

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type admissionHooks struct {
	begin   func(auth.Decision, decision.OutcomeObservation, shoal.ID) error
	publish func(Receipt) error
}

func (h *admissionHooks) Begin(_ context.Context, d auth.Decision, _ decision.PredictionRecord, o decision.OutcomeObservation, id shoal.ID) error {
	if h.begin != nil {
		return h.begin(d, o, id)
	}
	return nil
}
func (h *admissionHooks) Publish(_ context.Context, _ auth.Decision, _ decision.PredictionRecord, _ decision.OutcomeObservation, r Receipt) error {
	if h.publish != nil {
		return h.publish(r)
	}
	return nil
}
func TestAdmissionInterruptedBeforeOutcomeRemainsUncertain(t *testing.T) {
	for _, mode := range []string{"begin-error", "revocation"} {
		t.Run(mode, func(t *testing.T) {
			s, b, a, ctx, c := storeFixture(t)
			s.config.Admission = &admissionHooks{begin: func(d auth.Decision, o decision.OutcomeObservation, id shoal.ID) error {
				if d.Subject() != "principal" || o.Config().AssertedProvenance.ReporterID == d.Subject() || id == "" {
					t.Fatal("wrong trusted binding")
				}
				if mode == "begin-error" {
					return errors.New("lost pending ack")
				}
				a.deny.Store(true)
				return nil
			}}
			r, e := appendCfg(s, ctx, "key", c)
			if !errors.Is(e, ErrIndeterminate) || r.ID != "" || b.writes.Load() != 0 {
				t.Fatal("coordinated interruption misclassified", e)
			}
		})
	}
}
func TestAdmissionPublicationFailureRepairsOriginalReceipt(t *testing.T) {
	s, b, _, ctx, c := storeFixture(t)
	var original Receipt
	begins, publishes := 0, 0
	fail := true
	s.config.Admission = &admissionHooks{begin: func(auth.Decision, decision.OutcomeObservation, shoal.ID) error { begins++; return nil }, publish: func(r Receipt) error {
		publishes++
		if fail {
			original = r
			return errors.New("publication unavailable")
		}
		if !reflect.DeepEqual(r, original) {
			t.Fatal("retry changed original receipt")
		}
		r.ObservationConfig.EvidenceIDs[0] = "mutated"
		return nil
	}}
	if _, e := appendCfg(s, ctx, "key", c); !errors.Is(e, ErrIndeterminate) {
		t.Fatal(e)
	}
	fail = false
	got, e := appendCfg(s, ctx, "key", c)
	if e != nil || !reflect.DeepEqual(got, original) || b.writes.Load() != 1 || begins != 2 || publishes != 2 {
		t.Fatal("repair failed", e)
	}
	if _, e = s.Read(ctx, c.RequestID, c.PredictionID, []byte("key")); e != nil || begins != 2 || publishes != 2 {
		t.Fatal("Read invoked admission", e)
	}
	c.Label = "ordinary"
	if _, e = appendCfg(s, ctx, "key", c); !errors.Is(e, ErrConflict) || errors.Is(e, ErrIndeterminate) || begins != 2 {
		t.Fatal("conflict invoked admission", e)
	}
}
func TestAdmissionPublicationRevocationDeniesCommittedResult(t *testing.T) {
	s, b, a, ctx, c := storeFixture(t)
	s.config.Admission = &admissionHooks{publish: func(Receipt) error { a.deny.Store(true); return nil }}
	r, e := appendCfg(s, ctx, "key", c)
	if !errors.Is(e, ErrIndeterminate) || r.ID != "" || b.writes.Load() != 1 {
		t.Fatal("postpublication revocation escaped", e)
	}
}
func TestAdmissionUnauthorizedReplayDoesNotInvokeHook(t *testing.T) {
	s, _, a, ctx, c := storeFixture(t)
	if _, e := appendCfg(s, ctx, "key", c); e != nil {
		t.Fatal(e)
	}
	s.config.Admission = &admissionHooks{begin: func(auth.Decision, decision.OutcomeObservation, shoal.ID) error {
		t.Fatal("unauthorized pending write")
		return nil
	}}
	a.evidenceDenied.Store(shoal.ID("finding"), true)
	if _, e := appendCfg(s, ctx, "key", c); e == nil || errors.Is(e, ErrIndeterminate) {
		t.Fatal("unauthorized existence leaked", e)
	}
}
func TestAdmissionRacedConflictPreservesPendingUncertainty(t *testing.T) {
	s, b, _, ctx, c := storeFixture(t)
	legacy := *s
	other := c
	other.Label = "ordinary"
	s.config.Admission = &admissionHooks{begin: func(auth.Decision, decision.OutcomeObservation, shoal.ID) error {
		if _, e := appendCfg(&legacy, ctx, "key", other); e != nil {
			t.Fatal(e)
		}
		return nil
	}, publish: func(Receipt) error { t.Fatal("published losing observation"); return nil }}
	if _, e := appendCfg(s, ctx, "key", c); !errors.Is(e, ErrConflict) || !errors.Is(e, ErrIndeterminate) || b.writes.Load() != 2 {
		t.Fatal("pending conflict lost uncertainty", e)
	}
}

func TestAdmissionRejectsTypedNilCoordinator(t *testing.T) {
	s, _, _, _, _ := storeFixture(t)
	var missing *admissionHooks
	c := s.config
	c.Admission = missing
	if _, e := New(c); e == nil {
		t.Fatal("accepted typed nil coordinator")
	}
}
