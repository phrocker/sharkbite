// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package main

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/decisionartifacts"
	"github.com/phrocker/shoal-oss/internal/decisionoutcomes"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/explorercoord"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// This fixed test registry allows reports about one retained prediction. It is
// deliberately not a production outcome registry or an adjudicator: it checks
// the real source catalog and only admits already retained evidence anchors.
type frozenOutcomeAuthority struct {
	catalog    *decisionartifacts.Catalog
	prediction decision.PredictionRecord
	resource   auth.ResourceRequest
	evidence   []shoal.ID
}

func (a frozenOutcomeAuthority) Resolve(ctx context.Context, d auth.Decision, request, prediction shoal.ID, op auth.Operation) (decision.PredictionRecord, error) {
	if request != a.prediction.Request().ID() || prediction != a.prediction.ID() {
		return decision.PredictionRecord{}, auth.ObjectNotFound()
	}
	if err := d.AuthorizeObject(op, a.resource, time.Now()); err != nil {
		return decision.PredictionRecord{}, auth.ObjectNotFound()
	}
	if _, err := a.catalog.LoadAuthorized(ctx, d, request); err != nil {
		return decision.PredictionRecord{}, err
	}
	return a.prediction, nil
}

func (a frozenOutcomeAuthority) Verify(ctx context.Context, d auth.Decision, observations []decision.OutcomeObservation) error {
	if len(observations) == 0 {
		return auth.ObjectNotFound()
	}
	for _, observation := range observations {
		if err := a.verifyObservation(ctx, d, observation); err != nil {
			return err
		}
	}
	// Every fixture reference belongs to the same authorized retained source;
	// there are no additional cross-source joint-disclosure rules in this fixture.
	return nil
}

func (a frozenOutcomeAuthority) verifyObservation(ctx context.Context, d auth.Decision, observation decision.OutcomeObservation) error {
	if _, err := a.catalog.LoadAuthorized(ctx, d, observation.RequestID()); err != nil {
		return err
	}
	c := observation.Config()
	if c.Kind != decision.OutcomeCorrectness {
		// The fixture has no independently retained action execution evidence.
		return auth.ObjectNotFound()
	}
	for _, id := range c.EvidenceIDs {
		found := false
		for _, retained := range a.evidence {
			found = found || retained == id
		}
		if !found {
			return auth.ObjectNotFound()
		}
	}
	return nil
}

func outcomeTestDecision(t *testing.T, d auth.Decision, canReport bool) auth.Decision {
	t.Helper()
	ops := d.AllowedOperations()
	if canReport {
		ops = append(ops, auth.OperationIngest)
	}
	result, err := auth.NewDecision(auth.DecisionConfig{Subject: d.Subject(), Actor: d.Actor(), ClientID: d.ClientID(), AuthorizationDomain: d.AuthorizationDomain(), AllowedOperations: ops, PermittedSourceIDs: d.PermittedSourceIDs(), PermittedPolicyIDs: d.PermittedPolicyIDs(), PolicyGeneration: 1, AuthenticationExpires: time.Now().Add(time.Hour), RequestID: "outcome-fixture"})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestFrozenSourceOutcomeAttributionAndRevocation(t *testing.T) {
	dir, mh, ph, sh := serviceFixture(t)
	task, _, _, err := serviceDefinitions()
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := loadBundle(filepath.Join(dir, "numeric"), mh, ph, task, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer bundle.Close()
	sources, err := loadSources(dir, sh, bundle)
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	saved, err := loadServiceState(stateDir, mh, ph, sh, bundle.Provider.Identity().ID())
	if err != nil {
		t.Fatal(err)
	}
	session, err := openServiceSession(bundle, sources, saved, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer session.engine.Close()
	bind := func(d auth.Decision) context.Context {
		ctx, err := session.authority.Binder().Bind(context.Background(), d)
		if err != nil {
			t.Fatal(err)
		}
		return ctx
	}
	ctx := bind(session.decision)
	record := session.records[0]
	if err := session.catalog.Retain(ctx, record); err != nil {
		t.Fatal(err)
	}
	response, err := session.service.Evaluate(ctx, record.Bundle.Request.ID(), []byte("outcome-prediction"))
	if err != nil {
		t.Fatal(err)
	}
	prediction, err := decision.NewPredictionRecord(record.Bundle.Request, *response.Receipt.Result)
	if err != nil {
		t.Fatal(err)
	}
	request := prediction.Request()
	evidence := request.Picture().Config().Subjects[0].EvidenceIDs
	if err := session.engine.CreateTable(decisionoutcomes.Table, engine.TableOptions{}); err != nil {
		t.Fatal(err)
	}
	backend, err := explorercoord.NewEngineStore(session.engine, decisionoutcomes.Table)
	if err != nil {
		t.Fatal(err)
	}
	store, err := decisionoutcomes.New(decisionoutcomes.Config{Backend: backend, Resolver: session.authority.Resolver(), Authority: frozenOutcomeAuthority{session.catalog, prediction, record.Bundle.TaskResource, evidence}, Clock: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	c := decision.OutcomeObservationConfig{RequestID: request.ID(), PredictionID: prediction.ID(), SubjectID: request.Config().SubjectIDs[0], Kind: decision.OutcomeCorrectness, QuestionID: question, Label: "retain", EvidenceIDs: evidence, ObservedAt: time.Now().UTC(), AssertedProvenance: decision.OutcomeProvenance{ReporterID: "asserted-admin", ModelID: "asserted-frontier-model"}}
	key := []byte("reported-observation")
	// Prediction invocation permission is not reporting permission.
	if _, err = store.Append(ctx, request.ID(), prediction.ID(), key, c); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("caller without reporting right admitted: %v", err)
	}
	reporter := outcomeTestDecision(t, session.decision, true)
	reportCtx := bind(reporter)
	first, err := store.Append(reportCtx, request.ID(), prediction.ID(), key, c)
	if err != nil {
		t.Fatal(err)
	}
	if first.SubmitterID != reporter.Subject() || first.SubmitterID == c.AssertedProvenance.ReporterID || first.State != "proposed" || first.ReceivedAt.Before(c.ObservedAt) {
		t.Fatalf("assertions promoted to authentication or verification: %+v", first)
	}
	again, err := store.Append(reportCtx, request.ID(), prediction.ID(), key, c)
	if err != nil || !reflect.DeepEqual(first, again) {
		t.Fatalf("outcome retry changed receipt: %v", err)
	}
	correction := c
	correction.Label = "lower_priority"
	correction.Supersedes = first.ID
	corrected, err := store.Append(reportCtx, request.ID(), prediction.ID(), []byte("correction"), correction)
	if err != nil || corrected.State != "proposed" {
		t.Fatalf("correction was not retained as a proposal: %v", err)
	}
	original, err := store.Read(reportCtx, request.ID(), prediction.ID(), key)
	if err != nil || !reflect.DeepEqual(original, first) {
		t.Fatal("correction rewrote original observation")
	}
	bad := c
	bad.EvidenceIDs = []shoal.ID{"unretained-evidence"}
	if _, err := store.Append(reportCtx, request.ID(), prediction.ID(), []byte("untrusted-evidence"), bad); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("unretained evidence admitted: %v", err)
	}
	denied, err := localDecision(sources, map[string]bool{sources[0].ID: true})
	if err != nil {
		t.Fatal(err)
	}
	deniedCtx := bind(outcomeTestDecision(t, denied, true))
	if _, err := store.Read(deniedCtx, request.ID(), prediction.ID(), key); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("revoked source outcome disclosed: %v", err)
	}
	if _, err := store.Append(deniedCtx, request.ID(), prediction.ID(), []byte("after-revocation"), c); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("revoked source report admitted: %v", err)
	}
	if session.provider.calls != 1 {
		t.Fatal("outcome recording reinvoked predictor")
	}
}
