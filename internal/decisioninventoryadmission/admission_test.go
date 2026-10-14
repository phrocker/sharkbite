// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisioninventoryadmission_test

import (
	"context"
	admission "github.com/phrocker/shoal-oss/internal/decisioninventoryadmission"
	inventory "github.com/phrocker/shoal-oss/internal/decisioninventorystore"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type bindingAuthority struct {
	integrationAdmissionAuthority
	change func(*inventory.Binding)
	calls  int
}

func (a *bindingAuthority) Resolve(ctx context.Context, d auth.Decision, p decision.PredictionRecord, o decision.OutcomeObservation) (inventory.Binding, error) {
	a.calls++
	b, e := a.integrationAdmissionAuthority.Resolve(ctx, d, p, o)
	if a.change != nil {
		a.change(&b)
	}
	return b, e
}
func TestAdapterRejectsMissingAuthorityAndSubstitutedBinding(t *testing.T) {
	f := openIntegration(t, filepath.Join(t.TempDir(), "engine"), true)
	defer f.eng.Close()
	var nilAuthority *bindingAuthority
	if _, e := admission.New(admission.Config{Store: f.inv, Authority: nilAuthority}); e == nil {
		t.Fatal("typed nil authority admitted")
	}
	if _, e := admission.New(admission.Config{Authority: integrationAdmissionAuthority{f.policy}}); e == nil {
		t.Fatal("nil store admitted")
	}
	o, e := decision.NewOutcomeObservation(f.policy.prediction, f.cfg)
	if e != nil {
		t.Fatal(e)
	}
	for _, change := range []func(*inventory.Binding){func(b *inventory.Binding) { b.CoverageID = "" }, func(b *inventory.Binding) { b.TargetID = "substitute" }, func(b *inventory.Binding) { b.TaskID = "substitute" }, func(b *inventory.Binding) { b.PictureID = "substitute" }, func(b *inventory.Binding) { b.SubjectID = "substitute" }, func(b *inventory.Binding) { b.QuestionID = "substitute" }} {
		a, e := admission.New(admission.Config{Store: f.inv, Authority: &bindingAuthority{integrationAdmissionAuthority: integrationAdmissionAuthority{f.policy}, change: change}})
		if e != nil {
			t.Fatal(e)
		}
		if e = a.Begin(f.ctx, f.caller, f.policy.prediction, o, shoal.ID("outcome-receipt:"+strings.Repeat("a", 64))); !shoal.IsErrorCode(e, shoal.ErrorNotFound) {
			t.Fatalf("substituted binding admitted: %v", e)
		}
	}
	if s := f.snapshot(t); s.Version != 1 || !s.Complete() {
		t.Fatal("invalid bindings created inventory intents")
	}
}
func TestAdapterRejectsExecutionAndUnboundPrediction(t *testing.T) {
	f := openIntegration(t, filepath.Join(t.TempDir(), "engine"), true)
	defer f.eng.Close()
	authority := &bindingAuthority{integrationAdmissionAuthority: integrationAdmissionAuthority{f.policy}}
	a, e := admission.New(admission.Config{Store: f.inv, Authority: authority})
	if e != nil {
		t.Fatal(e)
	}
	cfg := f.cfg
	cfg.Kind = decision.OutcomeExecution
	cfg.QuestionID = ""
	cfg.Label = ""
	cfg.ExecutionStatus = decision.ExecutionSucceeded
	cfg.ActionID = "action"
	o, e := decision.NewOutcomeObservation(f.policy.prediction, cfg)
	if e != nil {
		t.Fatal(e)
	}
	if e = a.Begin(f.ctx, f.caller, f.policy.prediction, o, shoal.ID("outcome-receipt:"+strings.Repeat("a", 64))); !shoal.IsErrorCode(e, shoal.ErrorNotFound) {
		t.Fatal("execution observation entered correctness inventory", e)
	}
	if authority.calls != 0 {
		t.Fatal("unsupported execution reached authority")
	}
	o, e = decision.NewOutcomeObservation(f.policy.prediction, f.cfg)
	if e != nil {
		t.Fatal(e)
	}
	if e = a.Begin(f.ctx, f.caller, decision.PredictionRecord{}, o, shoal.ID("outcome-receipt:"+strings.Repeat("a", 64))); e == nil {
		t.Fatal("invalid prediction admitted")
	}
}
func TestAdapterBindsOriginalReceiptAndAllowsGrantRefresh(t *testing.T) {
	f := openIntegration(t, filepath.Join(t.TempDir(), "engine"), true)
	defer f.eng.Close()
	r, e := f.append(f.ctx)
	if e != nil {
		t.Fatal(e)
	}
	original := f.snapshot(t)
	o, e := decision.NewOutcomeObservation(f.policy.prediction, f.cfg)
	if e != nil {
		t.Fatal(e)
	}
	a, e := admission.New(admission.Config{Store: f.inv, Authority: integrationAdmissionAuthority{f.policy}})
	if e != nil {
		t.Fatal(e)
	}
	forged := r
	forged.ActorID = "imposter"
	if e = a.Publish(f.ctx, f.caller, f.policy.prediction, o, forged); !shoal.IsErrorCode(e, shoal.ErrorNotFound) {
		t.Fatal("substituted actual reporter accepted", e)
	}
	forged = r
	forged.ObservationConfig.Label = "ordinary"
	if e = a.Publish(f.ctx, f.caller, f.policy.prediction, o, forged); e == nil {
		t.Fatal("substituted canonical observation admitted")
	}
	refreshed, e := auth.NewDecision(auth.DecisionConfig{Subject: f.caller.Subject(), Actor: f.caller.Actor(), ClientID: f.caller.ClientID(), AuthorizationDomain: f.caller.AuthorizationDomain(), AllowedOperations: f.caller.AllowedOperations(), PermittedSourceIDs: f.caller.PermittedSourceIDs(), PermittedPolicyIDs: f.caller.PermittedPolicyIDs(), PolicyGeneration: 2, AuthenticationExpires: f.clock().Add(2 * time.Hour), RequestID: "refreshed"})
	if e != nil {
		t.Fatal(e)
	}
	if e = a.Publish(f.ctx, refreshed, f.policy.prediction, o, r); e != nil {
		t.Fatal("grant refresh rejected original receipt", e)
	}
	after := f.snapshot(t)
	if after.ID != original.ID || after.Entries[0].ReceiptDigest != original.Entries[0].ReceiptDigest {
		t.Fatal("refresh rewrote original publication")
	}
}

func TestAdapterPreservesOpaqueAuthenticatedReporter(t *testing.T) {
	f := openIntegration(t, filepath.Join(t.TempDir(), "engine"), true)
	defer f.eng.Close()
	subject := shoal.ID(string([]byte{255, 0}))
	actor := shoal.ID(string([]byte{254, 1}))
	delegates := []shoal.ID{shoal.ID(string([]byte{253, 2}))}
	d, e := auth.NewDecision(auth.DecisionConfig{Subject: subject, Actor: actor, ClientID: shoal.ID(string([]byte{252})), OnBehalfOf: delegates, AuthorizationDomain: f.caller.AuthorizationDomain(), AllowedOperations: f.caller.AllowedOperations(), PermittedSourceIDs: f.caller.PermittedSourceIDs(), PermittedPolicyIDs: f.caller.PermittedPolicyIDs(), PolicyGeneration: 1, AuthenticationExpires: f.clock().Add(time.Hour), RequestID: "opaque-reporter"})
	if e != nil {
		t.Fatal(e)
	}
	ctx, e := f.authn.Binder().Bind(context.Background(), d)
	if e != nil {
		t.Fatal(e)
	}
	receipt, e := f.append(ctx)
	if e != nil {
		t.Fatal(e)
	}
	s := f.snapshot(t)
	if len(s.Entries) != 1 || s.Entries[0].Intent.ReceiptID != receipt.ID || s.Entries[0].Intent.Reporter.SubjectID != subject || s.Entries[0].Intent.Reporter.ActorID != actor || s.Entries[0].Intent.Reporter.OnBehalfOf[0] != delegates[0] {
		t.Fatal("authenticated bytes corrupted")
	}
}
