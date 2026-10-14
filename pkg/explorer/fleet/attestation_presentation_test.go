// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package fleet

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type fakePresenter struct {
	verifyErr, recordErr error
	verified, recorded   []AttestationPrincipal
}

func (p *fakePresenter) Verify(principal AttestationPrincipal, _ AttestationPresentation, now time.Time) (AttestationReceipt, error) {
	p.verified = append(p.verified, principal)
	if p.verifyErr != nil {
		return AttestationReceipt{}, p.verifyErr
	}
	return AttestationReceipt{AttestationID: "exattest:1", ExpiresAt: now.Add(time.Hour)}, nil
}

func (p *fakePresenter) Record(_ context.Context, principal AttestationPrincipal, _ AttestationPresentation, now time.Time) (AttestationReceipt, error) {
	p.recorded = append(p.recorded, principal)
	if p.recordErr != nil {
		return AttestationReceipt{}, p.recordErr
	}
	return AttestationReceipt{AttestationID: "exattest:1", ExpiresAt: now.Add(time.Hour)}, nil
}

type fakeAttestationRecorder struct {
	err    error
	audits []AttestationAudit
}

func (r *fakeAttestationRecorder) RecordAttestation(_ context.Context, audit AttestationAudit) error {
	r.audits = append(r.audits, audit)
	return r.err
}

func TestAttestationPresentation(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	setup := func() (*AttestationService, *fakePresenter, *fakeAttestationRecorder) {
		presenter, recorder := &fakePresenter{}, &fakeAttestationRecorder{}
		service, err := NewAttestationService(AttestationConfig{
			Presenter: presenter, Resolver: authority.Resolver(), Recorder: recorder,
			Clock: func() time.Time { return now },
		})
		if err != nil {
			t.Fatal(err)
		}
		return service, presenter, recorder
	}
	caller := func(onBehalfOf []shoal.ID, operations ...auth.Operation) context.Context {
		return bindDecision(t, authority, dispatchDecisionFor(t, principal{
			subject: "alpha", actor: "alpha-actor", request: "present",
			clientID: "alpha-client", onBehalfOf: onBehalfOf,
		}, operations...))
	}
	// bound is a worker since #391: execute only, bound to one executor ref.
	bound := func(binding string) context.Context {
		return bindDecision(t, authority, executorDecisionFor(t, principal{
			subject: "alpha", actor: "alpha-actor", request: "present",
			clientID: "alpha-client",
		}, binding))
	}
	presentation := AttestationPresentation{
		ExecutorRef: "exec", IdempotencyKey: []byte("key"), Report: []byte(`{}`),
	}
	refused := func(t *testing.T, err error) {
		t.Helper()
		if !errors.Is(err, ErrAttestationRefused) || !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) ||
			err.Error() != "unauthorized: attestation refused" {
			t.Fatalf("err = %v, want the one opaque refusal", err)
		}
	}

	t.Run("requires a correlation ID, like every execute route", func(t *testing.T) {
		service, presenter, _ := setup()
		decision, err := auth.NewDecision(auth.DecisionConfig{
			Subject: "alpha", Actor: "alpha-actor", ClientID: "alpha-client",
			AuthorizationDomain: []byte("domain"), AllowedOperations: []auth.Operation{auth.OperationExecute},
			PermittedSourceIDs: [][]byte{[]byte("source")}, PermittedPolicyIDs: [][]byte{[]byte("policy")},
			PolicyGeneration: 1, AuthenticationExpires: now.Add(time.Hour), RequestID: "no-correlation",
		})
		if err != nil {
			t.Fatal(err)
		}
		if decision.CorrelationID() != "" {
			t.Fatal("fixture minted a correlation ID")
		}
		_, err = service.Present(bindDecision(t, authority, decision), presentation)
		if err == nil || errors.Is(err, ErrAttestationRefused) || len(presenter.verified) != 0 {
			t.Fatalf("a decision without a correlation ID = %v", err)
		}
		// And the minting helper every other subtest uses does set one.
		if dispatchDecisionFor(t, principal{subject: "a", actor: "b", request: "c"},
			auth.OperationExecute).CorrelationID() == "" {
			t.Fatal("dispatchDecisionFor mints no correlation ID")
		}
	})
	t.Run("requires execute", func(t *testing.T) {
		service, presenter, _ := setup()
		_, err := service.Present(caller(nil, auth.OperationInvoke, auth.OperationDispatch), presentation)
		if !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) || errors.Is(err, ErrAttestationRefused) {
			t.Fatalf("err = %v", err)
		}
		if len(presenter.verified) != 0 {
			t.Fatal("verified for a caller without execute")
		}
	})
	t.Run("accepted, audited first, for the caller only", func(t *testing.T) {
		service, presenter, recorder := setup()
		receipt, err := service.Present(bound("exec"), presentation)
		if err != nil || receipt.AttestationID == "" {
			t.Fatalf("present = %+v, %v", receipt, err)
		}
		want := AttestationPrincipal{Domain: []byte("domain"), Subject: "alpha", ClientID: "alpha-client"}
		if len(presenter.recorded) != 1 || !bytes.Equal(presenter.recorded[0].Domain, want.Domain) ||
			presenter.recorded[0].Subject != want.Subject || presenter.recorded[0].ClientID != want.ClientID {
			t.Fatalf("row key %+v, want the decision's %+v", presenter.recorded, want)
		}
		if len(recorder.audits) != 1 || recorder.audits[0].Phase != "attestation_presented" {
			t.Fatalf("audits = %+v", recorder.audits)
		}
	})
	t.Run("an unrecordable acceptance stores nothing", func(t *testing.T) {
		service, presenter, recorder := setup()
		recorder.err = errors.New("down")
		_, err := service.Present(bound("exec"), presentation)
		if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) || len(presenter.recorded) != 0 {
			t.Fatalf("err = %v, recorded = %d", err, len(presenter.recorded))
		}
	})
	t.Run("a refusal is opaque, audited with its reason, and stands", func(t *testing.T) {
		service, presenter, recorder := setup()
		presenter.verifyErr = &AttestationRefusal{Reason: "signature"}
		recorder.err = errors.New("down")
		_, err := service.Present(bound("exec"), presentation)
		refused(t, err)
		if len(recorder.audits) != 1 || recorder.audits[0].Phase != "attestation_refused" ||
			recorder.audits[0].Reason != "signature" || len(presenter.recorded) != 0 {
			t.Fatalf("audits = %+v", recorder.audits)
		}
	})
	t.Run("a rollback at record time is refused", func(t *testing.T) {
		service, presenter, recorder := setup()
		presenter.recordErr = &AttestationRefusal{Reason: "rollback"}
		_, err := service.Present(bound("exec"), presentation)
		refused(t, err)
		if last := recorder.audits[len(recorder.audits)-1]; last.Reason != "rollback" {
			t.Fatalf("audits = %+v", recorder.audits)
		}
	})
	t.Run("a delegated caller is refused", func(t *testing.T) {
		service, presenter, _ := setup()
		_, err := service.Present(caller([]shoal.ID{"delegator"}, auth.OperationExecute), presentation)
		refused(t, err)
		if len(presenter.verified) != 0 {
			t.Fatal("verified a delegated presentation")
		}
	})
	// A worker attests only for the executor it is bound to (#391). Both
	// refusals are the one opaque refusal, audited with their reason, and
	// reach no verifier.
	for _, refusedCaller := range []struct {
		name string
		ctx  func() context.Context
	}{
		{"an unbound execute-holder is refused", func() context.Context {
			return caller(nil, auth.OperationExecute)
		}},
		{"a worker bound to another executor is refused", func() context.Context {
			return bound("other-exec")
		}},
	} {
		t.Run(refusedCaller.name, func(t *testing.T) {
			service, presenter, recorder := setup()
			_, err := service.Present(refusedCaller.ctx(), presentation)
			refused(t, err)
			if len(presenter.verified) != 0 || len(presenter.recorded) != 0 {
				t.Fatal("verified a presentation for a reference the caller is not bound to")
			}
			if len(recorder.audits) != 1 ||
				recorder.audits[0].Reason != "executor-binding" {
				t.Fatalf("audits = %+v", recorder.audits)
			}
		})
	}
	t.Run("a store failure is unavailable", func(t *testing.T) {
		service, presenter, _ := setup()
		presenter.recordErr = errors.New("down")
		_, err := service.Present(bound("exec"), presentation)
		if !errors.Is(err, ErrAttestationUnavailable) || errors.Is(err, ErrAttestationRefused) {
			t.Fatalf("err = %v", err)
		}
	})
}
