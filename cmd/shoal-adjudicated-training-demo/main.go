// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	adjud "github.com/phrocker/shoal-oss/internal/decisionadjudication"
	journal "github.com/phrocker/shoal-oss/internal/decisionadjudicationstore"
	"github.com/phrocker/shoal-oss/internal/decisionartifacts"
	bases "github.com/phrocker/shoal-oss/internal/decisionbasisstore"
	"github.com/phrocker/shoal-oss/internal/decisiondatasets"
	admission "github.com/phrocker/shoal-oss/internal/decisioninventoryadmission"
	inventoryset "github.com/phrocker/shoal-oss/internal/decisioninventoryset"
	inventory "github.com/phrocker/shoal-oss/internal/decisioninventorystore"
	outcomes "github.com/phrocker/shoal-oss/internal/decisionoutcomes"
	"github.com/phrocker/shoal-oss/internal/decisionservice"
	"github.com/phrocker/shoal-oss/internal/decisionstore"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/explorercoord"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func writeExclusive(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, we := f.Write(b)
	se := f.Sync()
	ce := f.Close()
	if err = errors.Join(we, se, ce); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}
func bindRole(a *auth.Authority, role string) (context.Context, auth.Decision, error) {
	d, err := auth.NewDecision(auth.DecisionConfig{Subject: shoal.ID(role), Actor: shoal.ID(role + "-actor"), ClientID: shoal.ID(role + "-client"), AuthorizationDomain: []byte(domain), AllowedOperations: []auth.Operation{auth.OperationRead, auth.OperationRetrieve, auth.OperationInvoke, auth.OperationIngest}, PermittedSourceIDs: [][]byte{[]byte("tasks"), []byte("fixtures"), []byte("training")}, PermittedPolicyIDs: [][]byte{[]byte("fixture-policy"), []byte("fixture-training-purpose")}, PolicyGeneration: 1, AuthenticationExpires: time.Now().UTC().Add(time.Hour), RequestID: shoal.ID(role + "-session")})
	if err != nil {
		return nil, d, err
	}
	ctx, err := a.Binder().Bind(context.Background(), d)
	return ctx, d, err
}

// prepareRegistry registers exactly the compiled fixture census. There is no
// public registration route or caller-provided completeness/role assertion.
func prepareRegistry(dir, name string) (*registry, *engine.Engine, *auth.Authority, context.Context, error) {
	var eng *engine.Engine
	fail := func(err error) (*registry, *engine.Engine, *auth.Authority, context.Context, error) {
		if eng != nil {
			eng.Close()
		}
		return nil, nil, nil, nil, err
	}
	def, err := definitions(name)
	if err != nil {
		return fail(err)
	}
	if err = os.Mkdir(dir, 0700); err != nil {
		return fail(err)
	}
	if err = inquirySyncDir(filepath.Dir(dir)); err != nil {
		return fail(err)
	}
	for _, sub := range []string{"sources", "witnesses", "captures", "state"} {
		if err = os.Mkdir(filepath.Join(dir, sub), 0700); err != nil {
			return fail(err)
		}
	}
	if err = inquirySyncDir(dir); err != nil {
		return fail(err)
	}
	created := time.Now().UTC()
	r := &registry{dir: dir, def: def, rows: map[shoal.ID]*registered{}, readable: true, training: true, bases: map[shoal.ID]decision.AdjudicationBasis{}, now: time.Now}
	authority := auth.NewAuthority()
	predictCtx, predictIdentity, err := bindRole(authority, "fixture-predictor")
	if err != nil {
		return fail(err)
	}
	reportCtx, _, err := bindRole(authority, "fixture-reporter")
	if err != nil {
		return fail(err)
	}
	judgeCtx, _, err := bindRole(authority, "fixture-judge")
	if err != nil {
		return fail(err)
	}
	exportCtx, _, err := bindRole(authority, "fixture-exporter")
	if err != nil {
		return fail(err)
	}
	r.reporterCtx = reportCtx
	r.resolver = authority.Resolver()
	provider, model, err := baseline(def)
	if err != nil {
		return fail(err)
	}
	if err = writeExclusive(filepath.Join(dir, "bootstrap-model.json"), model); err != nil {
		return fail(err)
	}
	roles, _ := json.Marshal(map[string]any{"kind": "trusted-synthetic-role-registry", "authority": domain, "human_independence_claimed": false, "roles": []string{"fixture-predictor", "fixture-reporter", "fixture-reporter-secondary", "fixture-judge", "fixture-witness", "fixture-source-controller", "fixture-exporter"}, "role_evidence_id": "fixture-judge-role-grant:v1"})
	r.roleBytes = append([]byte(nil), roles...)
	if err = writeExclusive(filepath.Join(dir, "roles.json"), roles); err != nil {
		return fail(err)
	}
	for _, f := range fixtures(name) {
		if err = writeExclusive(filepath.Join(dir, "sources", string(f.ID)+".txt"), f.Raw); err != nil {
			return fail(err)
		}
		record, e := buildRecord(def, f, provider, shoal.ID("fixture-bootstrap:"+hash(model)), predictIdentity, created)
		if e != nil {
			return fail(e)
		}
		r.rows[f.ID] = &registered{fixture: f, record: record}
	}
	eng, err = engine.Open(filepath.Join(dir, "state", "engine"), engine.Options{})
	if err != nil {
		return fail(err)
	}
	for _, table := range []string{decisionartifacts.Table, decisionstore.Table, outcomes.Table, journal.Table, bases.Table, inventory.Table} {
		if err = eng.CreateTable(table, engine.TableOptions{}); err != nil {
			return fail(err)
		}
	}
	ab, err := explorercoord.NewEngineStore(eng, decisionartifacts.Table)
	if err != nil {
		return fail(err)
	}
	catalog, err := decisionartifacts.New(decisionartifacts.Config{Backend: ab, Resolver: authority.Resolver(), Authority: artifactAuthority{r}, Clock: time.Now})
	if err != nil {
		return fail(err)
	}
	pb, err := explorercoord.NewEngineStore(eng, decisionstore.Table)
	if err != nil {
		return fail(err)
	}
	receipts, err := decisionstore.New(pb, nil, time.Now)
	if err != nil {
		return fail(err)
	}
	predict, err := decisionservice.New(decisionservice.Config{Resolver: authority.Resolver(), Artifacts: catalog, Providers: provider, Receipts: receipts, Clock: time.Now, Lease: 2 * time.Minute, MaxCall: 30 * time.Second, Settlement: 5 * time.Second})
	if err != nil {
		return fail(err)
	}
	ob, err := explorercoord.NewEngineStore(eng, outcomes.Table)
	if err != nil {
		return fail(err)
	}
	ib, err := explorercoord.NewEngineStore(eng, inventory.Table)
	if err != nil {
		return fail(err)
	}
	r.inventories, err = inventory.New(inventory.Config{Backend: ib, Clock: time.Now})
	if err != nil {
		return fail(err)
	}
	r.inventorySets, err = inventoryset.New(inventoryset.Config{Store: r.inventories, Clock: time.Now})
	if err != nil {
		return fail(err)
	}
	r.admission, err = admission.New(admission.Config{Store: r.inventories, Authority: inventoryAdmissionAuthority{r}})
	if err != nil {
		return fail(err)
	}
	r.registeredOutcomes, err = outcomes.NewRegisteredReader(outcomes.RegisteredReaderConfig{Backend: ob, Resolver: authority.Resolver(), Authority: registeredOutcomeAuthority{r}, Clock: time.Now})
	if err != nil {
		return fail(err)
	}
	r.outcomes, err = outcomes.New(outcomes.Config{Backend: ob, Resolver: authority.Resolver(), Authority: outcomeAuthority{r}, Admission: r.admission, Clock: time.Now})
	if err != nil {
		return fail(err)
	}
	jb, err := explorercoord.NewEngineStore(eng, journal.Table)
	if err != nil {
		return fail(err)
	}
	js, err := journal.New(journal.Config{Backend: jb, Clock: time.Now})
	if err != nil {
		return fail(err)
	}
	bb, err := explorercoord.NewEngineStore(eng, bases.Table)
	if err != nil {
		return fail(err)
	}
	bs, err := bases.New(bases.Config{Backend: bb})
	if err != nil {
		return fail(err)
	}
	r.service, err = adjud.New(adjud.Config{Resolver: authority.Resolver(), Authority: adjudicationAuthority{r}, Journal: js, Bases: bs, Clock: time.Now})
	if err != nil {
		return fail(err)
	}
	for _, f := range fixtures(name) {
		v := r.rows[f.ID]
		if err = catalog.Retain(predictCtx, v.record); err != nil {
			return fail(fmt.Errorf("retain %s: %w", f.ID, err))
		}
		response, e := predict.Evaluate(predictCtx, v.record.Bundle.Request.ID(), []byte(f.ID))
		if e != nil {
			return fail(fmt.Errorf("predict %s: %w", f.ID, e))
		}
		if response.Receipt.Result == nil {
			return fail(fmt.Errorf("missing prediction"))
		}
		v.prediction, e = decision.NewPredictionRecord(v.record.Bundle.Request, *response.Receipt.Result)
		if e != nil {
			return fail(e)
		}
		v.target, e = decision.AdjudicationTargetID(def.Task.ID(), v.prediction.Request().Picture().ID(), f.ID, question)
		if e != nil {
			return fail(e)
		}
		if _, e = r.inventories.Register(predictCtx, inventory.Scope{Domain: []byte(domain)}, r.inventoryBinding(v)); e != nil {
			return fail(e)
		}
		if f.Status == "unknown" {
			continue
		}
		label, e := oracle(name, f.Raw)
		if e != nil {
			return fail(e)
		}
		oc := decision.OutcomeObservationConfig{RequestID: v.prediction.Request().ID(), PredictionID: v.prediction.ID(), SubjectID: f.ID, QuestionID: question, Kind: decision.OutcomeCorrectness, Label: label, EvidenceIDs: []shoal.ID{f.ID}, ObservedAt: time.Now().UTC(), AssertedProvenance: decision.OutcomeProvenance{ReporterID: "fixture-reporter", ToolID: "fixture-oracle-v1"}}
		v.outcome, e = r.outcomes.Append(reportCtx, oc.RequestID, oc.PredictionID, []byte(f.ID), oc)
		if e != nil {
			return fail(fmt.Errorf("outcome %s: %w", f.ID, e))
		}
		witnessBytes, _ := json.Marshal(map[string]any{"kind": "synthetic-independent-role-oracle-record", "source_sha256": hash(f.Raw), "task_id": def.Task.ID(), "subject_id": f.ID, "label": label, "oracle": "independent-parser-v1", "human_independence_claimed": false})
		v.witnessBytes = witnessBytes
		when := time.Now().UTC()
		v.witness = decision.BasisWitness{ID: shoal.ID("witness:" + string(f.ID)), Digest: hash(witnessBytes), VerificationReceiptID: shoal.ID("fixture-verification:" + hash(witnessBytes)), OriginGroupID: shoal.ID("fixture-oracle-origin:" + string(f.ID)), Origin: identity("fixture-witness"), ControllerIdentities: []decision.BasisIdentity{identity("fixture-witness")}, ReceivedAt: when, VerifiedAt: when}
		if e = writeExclusive(filepath.Join(dir, "witnesses", string(f.ID)+".json"), witnessBytes); e != nil {
			return fail(e)
		}
		disposition := decision.AdjudicationVerified
		if f.Status == "disputed" {
			disposition = decision.AdjudicationDisputed
			label = ""
		}
		proposal := decision.AdjudicationProposalConfig{RequestID: oc.RequestID, PredictionID: oc.PredictionID, SubjectID: f.ID, QuestionID: question, ObservationReceiptIDs: []shoal.ID{v.outcome.ID}, WitnessIDs: []shoal.ID{v.witness.ID}, Disposition: disposition, Label: label, Reason: "Synthetic fixture conformance; no model quality claim"}
		if disposition == decision.AdjudicationVerified {
			proposal.Reason = ""
		}
		receipt, e := r.service.Adjudicate(judgeCtx, []byte(f.ID), proposal)
		if e != nil {
			return fail(fmt.Errorf("adjudicate %s: %w", f.ID, e))
		}
		v.admitted = []journal.Receipt{receipt}
		v.basis = r.bases[receipt.BasisID]
	}
	cutoff := time.Now().UTC()
	r.cohort = decisiondatasets.Cohort{ID: shoal.ID("sealed-fixture-cohort:" + name), AuthorityRevisionID: "fixture-authority-revision:1", InventoryID: "compiled-target-membership-v1", Task: def.Task, Policy: def.Labels, QuestionID: question, FeatureSchemaID: def.FeatureSchema, FeatureBuilderID: shoal.ID("original-bytes:" + string(def.FeatureSchema)), SplitPolicyID: "compiled-family-disjoint-v1", SamplingPolicyID: "finite-fixture-census-only-v1", Cutoff: cutoff, Mode: "reconstructed"}
	for _, f := range fixtures(name) {
		v := r.rows[f.ID]
		one := float64(1)
		r.cohort.Members = append(r.cohort.Members, decisiondatasets.Member{ID: f.ID, TargetID: v.target, RequestID: v.prediction.Request().ID(), PredictionID: v.prediction.ID(), SubjectID: f.ID, FamilyIDs: []shoal.ID{shoal.ID("synthetic-family:" + string(f.ID))}, Split: f.Split, InclusionProbability: &one})
	}
	r.sealed = true
	for _, table := range []string{decisionartifacts.Table, decisionstore.Table, outcomes.Table, journal.Table, bases.Table, inventory.Table} {
		if err = eng.Flush(table); err != nil {
			return fail(err)
		}
	}
	return r, eng, authority, exportCtx, nil
}
func prepare(dir, name string) error {
	r, eng, authority, ctx, err := prepareRegistry(dir, name)
	if err != nil {
		return err
	}
	defer eng.Close()
	exporter, session, err := exportSession(ctx, r, authority.Resolver(), r.cohort.ID)
	if err != nil {
		return err
	}
	bundle, err := exporter.Export(ctx, r.cohort.ID)
	if err != nil {
		return err
	}
	captureBytes, err := json.Marshal(struct {
		Schema  int          `json:"schema"`
		Kind    string       `json:"kind"`
		Initial captureProof `json:"initial"`
		Final   captureProof `json:"final"`
	}{1, "registered-inventory-export-capture", proof(session.initialCapture), proof(session.finalCapture)})
	if err != nil {
		return err
	}
	if err = writeExclusive(filepath.Join(dir, "cohort-capture.json"), captureBytes); err != nil {
		return err
	}
	if err = writeExclusive(filepath.Join(dir, "dataset.json"), bundle.Dataset); err != nil {
		return err
	}
	if err = writeExclusive(filepath.Join(dir, "manifest.json"), bundle.Manifest); err != nil {
		return err
	}
	fmt.Println(bundle.ManifestSHA256)
	return nil
}
func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("use prepare --task source|review --out DIR, or inquire")
	}
	switch args[0] {
	case "prepare":
		f := flag.NewFlagSet("prepare", flag.ContinueOnError)
		task := f.String("task", "source", "source or review")
		out := f.String("out", "", "new output directory")
		if err := f.Parse(args[1:]); err != nil {
			return err
		}
		if *out == "" || f.NArg() != 0 {
			return fmt.Errorf("--out new-directory required")
		}
		return prepare(*out, *task)
	case "inquire":
		return runInquiry(args[1:])
	default:
		return fmt.Errorf("unknown command")
	}
}
func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
