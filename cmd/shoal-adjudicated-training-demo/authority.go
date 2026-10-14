// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package main

import (
	"context"
	"path/filepath"
	"reflect"
	"time"

	adjud "github.com/phrocker/shoal-oss/internal/decisionadjudication"
	journal "github.com/phrocker/shoal-oss/internal/decisionadjudicationstore"
	"github.com/phrocker/shoal-oss/internal/decisionartifacts"
	"github.com/phrocker/shoal-oss/internal/decisiondatasets"
	admission "github.com/phrocker/shoal-oss/internal/decisioninventoryadmission"
	inventoryset "github.com/phrocker/shoal-oss/internal/decisioninventoryset"
	inventory "github.com/phrocker/shoal-oss/internal/decisioninventorystore"
	outcomes "github.com/phrocker/shoal-oss/internal/decisionoutcomes"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type registered struct {
	fixture      fixture
	record       decisionartifacts.Record
	prediction   decision.PredictionRecord
	outcome      outcomes.Receipt
	witness      decision.BasisWitness
	witnessBytes []byte
	target       shoal.ID
	admitted     []journal.Receipt
	basis        decision.AdjudicationBasis
}
type registry struct {
	roleBytes []byte

	dir                string
	def                taskDefinition
	rows               map[shoal.ID]*registered
	sealed             bool
	readable, training bool
	cohort             decisiondatasets.Cohort
	service            *adjud.Service
	reporterCtx        context.Context
	outcomes           *outcomes.Store
	bases              map[shoal.ID]decision.AdjudicationBasis
	inventories        *inventory.Store
	inventorySets      *inventoryset.Reader
	registeredOutcomes *outcomes.RegisteredReader
	admission          *admission.Adapter
	resolver           auth.Resolver
	now                func() time.Time
}

func identity(id string) decision.BasisIdentity {
	return decision.BasisIdentity{SubjectID: []byte(id), ActorID: []byte(id + "-actor"), ClientID: []byte(id + "-client")}
}
func (r *registry) permission(d auth.Decision, op auth.Operation, id shoal.ID) error {
	now := r.now()
	if !r.readable {
		return auth.ObjectNotFound()
	}
	return d.AuthorizeObject(op, auth.ResourceRequest{AuthorizationDomain: []byte(domain), SourceID: []byte("fixtures"), PolicyID: []byte("fixture-policy"), ObjectID: id}, now)
}
func (r *registry) byPrediction(request, pred shoal.ID) *registered {
	for _, v := range r.rows {
		if v.prediction.ID() == pred && v.prediction.Request().ID() == request {
			return v
		}
	}
	return nil
}
func (r *registry) byTarget(target shoal.ID) *registered {
	for _, v := range r.rows {
		if v.target == target {
			return v
		}
	}
	return nil
}
func (r *registry) source(v *registered) error {
	b, err := inquiryRead(filepath.Join(r.dir, "sources", string(v.fixture.ID)+".txt"), len(v.fixture.Raw))
	if err != nil || hash(b) != hash(v.fixture.Raw) || !reflect.DeepEqual(b, v.record.Sources[0].Bytes) {
		return auth.ObjectNotFound()
	}
	return nil
}
func (r *registry) allCurrent(d auth.Decision, v *registered, op auth.Operation) error {
	if v == nil {
		return auth.ObjectNotFound()
	}
	roles, e := inquiryRead(filepath.Join(r.dir, "roles.json"), len(r.roleBytes))
	if e != nil || !reflect.DeepEqual(roles, r.roleBytes) {
		return auth.ObjectNotFound()
	}
	if err := r.source(v); err != nil {
		return err
	}
	if v.witness.ID != "" {
		b, e := inquiryRead(filepath.Join(r.dir, "witnesses", string(v.fixture.ID)+".json"), len(v.witnessBytes))
		if e != nil || hash(b) != v.witness.Digest || !reflect.DeepEqual(b, v.witnessBytes) {
			return auth.ObjectNotFound()
		}
	}
	return r.permission(d, op, v.fixture.ID)
}

type artifactAuthority struct{ r *registry }

func (a artifactAuthority) AuthorizeRequest(_ context.Context, d auth.Decision, id shoal.ID) error {
	for _, v := range a.r.rows {
		if v.record.Bundle.Request.ID() == id {
			return a.r.permission(d, auth.OperationRead, v.fixture.ID)
		}
	}
	return auth.ObjectNotFound()
}
func (a artifactAuthority) Verify(_ context.Context, d auth.Decision, record decisionartifacts.Record) error {
	for _, v := range a.r.rows {
		if reflect.DeepEqual(v.record, record) {
			return a.r.allCurrent(d, v, auth.OperationRetrieve)
		}
	}
	return auth.ObjectNotFound()
}

type outcomeAuthority struct{ r *registry }

func (a outcomeAuthority) Resolve(_ context.Context, d auth.Decision, request, pred shoal.ID, op auth.Operation) (decision.PredictionRecord, error) {
	v := a.r.byPrediction(request, pred)
	if v == nil || !reportingRole(d) {
		return decision.PredictionRecord{}, auth.ObjectNotFound()
	}
	if err := a.r.allCurrent(d, v, op); err != nil {
		return decision.PredictionRecord{}, err
	}
	return v.prediction, nil
}
func (a outcomeAuthority) Verify(_ context.Context, d auth.Decision, chain []decision.OutcomeObservation) error {
	for _, o := range chain {
		c := o.Config()
		v := a.r.byPrediction(c.RequestID, c.PredictionID)
		if v == nil || !reportingRole(d) || c.AssertedProvenance.ReporterID != d.Subject() || !reflect.DeepEqual(c.EvidenceIDs, []shoal.ID{v.fixture.ID}) {
			return auth.ObjectNotFound()
		}
		if err := a.r.allCurrent(d, v, auth.OperationRead); err != nil {
			return err
		}
	}
	return nil
}

type adjudicationAuthority struct{ r *registry }

func (a adjudicationAuthority) Resolve(_ context.Context, d auth.Decision, request, pred shoal.ID, op auth.Operation) (adjud.Binding, error) {
	v := a.r.byPrediction(request, pred)
	if err := a.r.allCurrent(d, v, op); err != nil {
		return adjud.Binding{}, err
	}
	return adjud.Binding{Policy: a.r.def.Labels, Prediction: v.prediction}, nil
}
func (a adjudicationAuthority) AuthorizeTarget(_ context.Context, d auth.Decision, target shoal.ID, op auth.Operation) error {
	return a.r.allCurrent(d, a.r.byTarget(target), op)
}
func (a adjudicationAuthority) Capture(ctx context.Context, d auth.Decision, p decision.AdjudicationProposal, _ []adjud.Material) (decision.AdjudicationBasis, error) {
	v := a.r.byTarget(p.TargetID())
	if v == nil || a.r.sealed || d.Subject() != "fixture-judge" {
		return decision.AdjudicationBasis{}, auth.ObjectNotFound()
	}
	capture, e := a.r.inventorySets.CaptureSet(ctx, inventory.Scope{Domain: d.AuthorizationDomain()}, []inventory.Binding{a.r.inventoryBinding(v)})
	if e != nil {
		return decision.AdjudicationBasis{}, auth.ObjectNotFound()
	}
	receipts, e := a.r.publishedReceipts(ctx, d, v, capture.Snapshots[0])
	if e != nil {
		return decision.AdjudicationBasis{}, e
	}
	again, e := a.r.inventorySets.CaptureSet(ctx, inventory.Scope{Domain: d.AuthorizationDomain()}, []inventory.Binding{a.r.inventoryBinding(v)})
	if e != nil || again.VectorID != capture.VectorID {
		return decision.AdjudicationBasis{}, auth.ObjectNotFound()
	}
	enumeration, e := a.r.retainCapture(capture)
	if e != nil {
		return decision.AdjudicationBasis{}, e
	}
	cutoff := a.r.now().Round(0).UTC()
	if cutoff.Before(capture.Window.CompletedAt) || cutoff.Before(capture.Snapshots[0].UpdatedAt) {
		return decision.AdjudicationBasis{}, auth.ObjectNotFound()
	}
	c := decision.AdjudicationBasisConfig{AuthorityID: domain, AuthorityRevisionID: "fixture-authority-revision:1", EnumerationID: enumeration, CapturedAt: cutoff, Cutoff: cutoff, InventoryComplete: true, ControllersComplete: true, Witnesses: []decision.BasisWitness{v.witness}, SourceControllers: []decision.BasisIdentity{identity("fixture-source-controller")}, RoleEvidenceIDs: []shoal.ID{"fixture-judge-role-grant:v1"}}
	for _, receipt := range receipts {
		c.Outcomes = append(c.Outcomes, basisOutcome(a.r, v, receipt))
	}
	basis, e := decision.NewAdjudicationBasis(p, c)
	if e != nil {
		return basis, e
	}
	a.r.bases[basis.ID()] = basis
	return basis, nil
}
func (a adjudicationAuthority) Verify(ctx context.Context, d auth.Decision, op auth.Operation, target shoal.ID, history []adjud.Material, candidate *adjud.Candidate) error {
	v := a.r.byTarget(target)
	if v == nil {
		return auth.ObjectNotFound()
	}
	for _, m := range history {
		expected, ok := a.r.bases[m.Basis.ID()]
		if !ok || !reflect.DeepEqual(expected.Config(), m.Basis.Config()) || m.Proposal.ID() != expected.Proposal().ID() {
			return auth.ObjectNotFound()
		}
		if e := a.r.verifyHistoricalCapture(ctx, d, v, m.Basis); e != nil {
			return e
		}
	}
	if candidate != nil {
		expected, ok := a.r.bases[candidate.Basis.ID()]
		if !ok || !reflect.DeepEqual(expected.Config(), candidate.Basis.Config()) || d.Subject() != "fixture-judge" || !reflect.DeepEqual(candidate.Adjudicator, identity("fixture-judge")) {
			return auth.ObjectNotFound()
		}
		if e := a.r.verifyHistoricalCapture(ctx, d, v, candidate.Basis); e != nil {
			return e
		}
		for _, role := range candidate.RequiredRoles {
			if role != "fixture-judge-role" && role != "fixture-resolver-role" {
				return auth.ObjectNotFound()
			}
		}
		label, err := oracle(a.r.def.Name, v.fixture.Raw)
		if err != nil {
			return err
		}
		if candidate.Proposal.Config().Disposition == decision.AdjudicationVerified && candidate.Proposal.Config().Label != label {
			return auth.ObjectNotFound()
		}
	}
	// Read all retained inputs before the final current permission check.
	return a.r.allCurrent(d, v, op)
}

type cohortAuthority struct {
	r                            *registry
	cohort                       decisiondatasets.Cohort
	initialCapture, finalCapture inventoryset.Capture
}

func (a *cohortAuthority) Resolve(_ context.Context, d auth.Decision, id shoal.ID) (decisiondatasets.Cohort, error) {
	if !a.r.sealed || id != a.cohort.ID || !a.r.training || d.Subject() != "fixture-exporter" {
		return decisiondatasets.Cohort{}, auth.ObjectNotFound()
	}
	for _, v := range a.r.rows {
		if e := a.r.permission(d, auth.OperationRead, v.fixture.ID); e != nil {
			return decisiondatasets.Cohort{}, e
		}
	}
	return cloneCohort(a.cohort), nil
}
func (a *cohortAuthority) Load(ctx context.Context, d auth.Decision, c decisiondatasets.Cohort, m decisiondatasets.Member) (decisiondatasets.Target, error) {
	var zero decisiondatasets.Target
	if !reflect.DeepEqual(c, a.cohort) || !a.r.sealed {
		return zero, auth.ObjectNotFound()
	}
	valid := false
	for _, member := range c.Members {
		valid = valid || reflect.DeepEqual(m, member)
	}
	v := a.r.rows[m.ID]
	if !valid || v == nil || v.target != m.TargetID {
		return zero, auth.ObjectNotFound()
	}
	if e := a.r.allCurrent(d, v, auth.OperationRead); e != nil {
		return zero, e
	}
	var snapshot inventory.Snapshot
	for _, s := range a.initialCapture.Snapshots {
		if s.Binding.TargetID == v.target {
			snapshot = s
		}
	}
	if snapshot.ID == "" {
		return zero, auth.ObjectNotFound()
	}
	receipts, e := a.r.publishedReceipts(ctx, d, v, snapshot)
	if e != nil {
		return zero, e
	}
	var history []journal.Receipt
	if len(v.admitted) > 0 {
		history, e = a.r.service.History(ctx, v.target)
		if e != nil {
			return zero, e
		}
		if !reflect.DeepEqual(history, v.admitted) {
			return zero, auth.ObjectNotFound()
		}
	}
	fs, e := features(a.r.def.Name, v.fixture.Raw)
	if e != nil {
		return zero, e
	}
	ids := []shoal.ID{}
	for _, receipt := range receipts {
		ids = append(ids, receipt.ID)
	}
	return decisiondatasets.Target{Prediction: v.prediction, History: history, SelectedBasis: v.basis, Features: fs, FeatureInputDigest: v.record.Bundle.Request.Picture().Config().InputDigest, FeatureReceivedAt: v.record.Bundle.Request.Picture().Config().Sources[0].ReceivedAt, InventoryID: snapshot.ID, OutcomeReceiptIDs: ids, InventoryComplete: true, SourceAvailable: true, TrainingAllowed: a.r.training}, nil
}
func (a *cohortAuthority) Verify(ctx context.Context, d auth.Decision, c decisiondatasets.Cohort, targets []decisiondatasets.Target) error {
	if !a.r.sealed || !a.r.training || !reflect.DeepEqual(c, a.cohort) || len(targets) != len(c.Members) || d.Subject() != "fixture-exporter" {
		return auth.ObjectNotFound()
	}
	pins := map[shoal.ID]shoal.ID{}
	for _, s := range a.initialCapture.Snapshots {
		pins[s.Binding.TargetID] = s.ID
	}
	for i, t := range targets {
		v := a.r.rows[c.Members[i].ID]
		if v == nil || t.InventoryID != pins[v.target] || t.Prediction.ID() != v.prediction.ID() {
			return auth.ObjectNotFound()
		}
		if e := a.r.allCurrent(d, v, auth.OperationRead); e != nil {
			return e
		}
	}
	final, e := a.r.inventorySets.CaptureSet(ctx, inventory.Scope{Domain: d.AuthorizationDomain()}, a.r.inventoryBindings())
	if e != nil || final.VectorID != a.initialCapture.VectorID {
		return auth.ObjectNotFound()
	}
	// All source/receipt/history/inventory IO is complete. These are current host
	// grants over the synthetic closed registry; no filesystem or engine reads follow.
	for _, v := range a.r.rows {
		if e := a.r.permission(d, auth.OperationRead, v.fixture.ID); e != nil {
			return e
		}
		if e := d.AuthorizeObject(auth.OperationIngest, auth.ResourceRequest{AuthorizationDomain: []byte(domain), SourceID: []byte("training"), PolicyID: []byte("fixture-training-purpose"), ObjectID: c.ID}, a.r.now()); e != nil {
			return e
		}
	}
	if ctx.Err() != nil || !a.r.sealed || !a.r.readable || !a.r.training || !reflect.DeepEqual(c, a.cohort) {
		return auth.ObjectNotFound()
	}
	a.finalCapture = final
	return nil
}

func cloneCohort(c decisiondatasets.Cohort) decisiondatasets.Cohort {
	c.Members = append([]decisiondatasets.Member(nil), c.Members...)
	for i := range c.Members {
		c.Members[i].FamilyIDs = append([]shoal.ID(nil), c.Members[i].FamilyIDs...)
		if c.Members[i].InclusionProbability != nil {
			p := *c.Members[i].InclusionProbability
			c.Members[i].InclusionProbability = &p
		}
	}
	return c
}
