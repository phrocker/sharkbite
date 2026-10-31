// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/phrocker/shoal-oss/internal/adjudicatedfixture"
	"github.com/phrocker/shoal-oss/internal/decisiondatasets"
	"github.com/phrocker/shoal-oss/internal/decisionlearning"
	"github.com/phrocker/shoal-oss/internal/engine"
	decisionapi "github.com/phrocker/shoal-oss/pkg/decision/api"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// fixtureCohorts is the development cohort authority and exporter: the
// sealed synthetic fixture census of the adjudicated training demo, prepared
// at startup for each named task and exported through the same authorized
// dataset service the demo uses. It lets the Learning console's dataset
// export run end to end on one host. It is a finite synthetic conformance
// fixture, not a repository sampling authority, and every export it
// produces says so in its provenance mode.
type fixtureCohorts struct {
	mu      sync.Mutex
	entries map[shoal.ID]*fixtureCohort
	order   []shoal.ID
}

type fixtureCohort struct {
	engine *engine.Engine
	cohort decisiondatasets.Cohort
	// sealed is the export taken at startup under the demo's own
	// fixture-exporter decision (which expires an hour after it is minted,
	// so it is used once, immediately) and returned for every request: the
	// census is immutable, and the recorded manifest pin must be
	// re-obtainable for an offline bundle export. The registry's authority
	// governed source permission; the web caller's authority governs who
	// may ask for the bytes.
	sealed decisiondatasets.Bundle
	// sample is the per-target class material taken at the same time under
	// the same session, from which a class record over any declared window
	// is aggregated on request without re-reading the registry.
	sample decisiondatasets.Sample
}

// fixtureCohortTasks are the demo's compiled task fixtures.
var fixtureCohortTasks = map[string]bool{"source": true, "review": true}

func parseFixtureCohortTasks(spec string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, item := range strings.Split(spec, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if !fixtureCohortTasks[item] || seen[item] {
			return nil, fmt.Errorf("-learning-fixture-cohorts task %q must be source or review, each at most once", item)
		}
		seen[item] = true
		out = append(out, item)
	}
	return out, nil
}

// mountFixtureCohorts prepares one sealed registry per task under
// <stateDir>/fixture-cohorts and takes each cohort's export at once. The
// census is synthetic and regenerated on every start, so the previous
// preparation is removed first; every preparation gets a fresh nonce in its
// cohort identity because its cutoffs and request ids, and therefore its
// export pin, are new, and a recorded export from an earlier start must not
// look attached to this one.
func mountFixtureCohorts(stateDir string, tasks []string, output io.Writer) (*fixtureCohorts, error) {
	root := filepath.Join(stateDir, "fixture-cohorts")
	if err := os.RemoveAll(root); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	var seed [8]byte
	if _, err := rand.Read(seed[:]); err != nil {
		return nil, err
	}
	nonce := hex.EncodeToString(seed[:])
	f := &fixtureCohorts{entries: map[shoal.ID]*fixtureCohort{}}
	for _, task := range tasks {
		registry, eng, authority, ctx, err := adjudicatedfixture.PrepareSealed(filepath.Join(root, task), task, nonce)
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("fixture cohort %s: %w", task, err)
		}
		cohort := registry.Cohort()
		bundle, err := adjudicatedfixture.FreshExport(ctx, registry, authority.Resolver(), cohort.ID)
		if err != nil {
			eng.Close()
			f.Close()
			return nil, fmt.Errorf("fixture cohort %s export: %w", task, err)
		}
		sample, err := adjudicatedfixture.FreshClassSample(ctx, registry, authority.Resolver(), cohort.ID)
		if err != nil {
			eng.Close()
			f.Close()
			return nil, fmt.Errorf("fixture cohort %s class sample: %w", task, err)
		}
		f.entries[cohort.ID] = &fixtureCohort{engine: eng, cohort: cohort, sealed: bundle, sample: sample}
		f.order = append(f.order, cohort.ID)
		fmt.Fprintf(output, "learning: fixture cohort %s prepared and exported at pin %s (%d members, synthetic conformance fixture, not a sampling authority)\n", cohort.ID, bundle.ManifestSHA256, len(cohort.Members))
	}
	return f, nil
}

// ListCohorts lists the prepared fixture cohorts.
func (f *fixtureCohorts) ListCohorts(context.Context) ([]decisionlearning.Cohort, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]decisionlearning.Cohort, 0, len(f.order))
	for _, id := range f.order {
		c := f.entries[id].cohort
		out = append(out, decisionlearning.Cohort{ID: c.ID, TaskID: string(c.Task.ID()), LabelPolicyID: string(c.Policy.ID()), Members: len(c.Members), Mode: c.Mode, Cutoff: c.Cutoff.UTC().Format(time.RFC3339Nano)})
	}
	return out, nil
}

// bundle returns the sealed export of one fixture cohort.
func (f *fixtureCohorts) bundle(ctx context.Context, id shoal.ID) (decisiondatasets.Bundle, error) {
	if ctx.Err() != nil {
		return decisiondatasets.Bundle{}, ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	entry, ok := f.entries[id]
	if !ok {
		return decisiondatasets.Bundle{}, shoal.NewError(shoal.ErrorNotFound, "cohort "+string(id)+" is not a prepared fixture cohort")
	}
	return decisiondatasets.Bundle{Dataset: append([]byte(nil), entry.sealed.Dataset...), Manifest: append([]byte(nil), entry.sealed.Manifest...), ManifestSHA256: entry.sealed.ManifestSHA256}, nil
}

// Export is the learning service's exporter: the sealed export of one
// fixture cohort, identical on every call.
func (f *fixtureCohorts) Export(ctx context.Context, id shoal.ID) (decisionlearning.Export, error) {
	b, err := f.bundle(ctx, id)
	if err != nil {
		return decisionlearning.Export{}, err
	}
	return decisionlearning.Export{Dataset: b.Dataset, Manifest: b.Manifest}, nil
}

// fixtureDatasetProvider serves the same sealed bytes through the
// dataset-exports route, so a trainer can fetch exactly the export whose
// pin the learning service recorded.
type fixtureDatasetProvider struct{ f *fixtureCohorts }

func (p fixtureDatasetProvider) Export(ctx context.Context, id shoal.ID) (decisiondatasets.Bundle, error) {
	return p.f.bundle(ctx, id)
}

// fixtureClassProvider aggregates the sealed class sample of one fixture
// cohort over the window the caller declares. The sample was taken under the
// demo's exporter session; the web caller's authority governs who may ask.
type fixtureClassProvider struct{ f *fixtureCohorts }

func (p fixtureClassProvider) ClassRecords(ctx context.Context, req decisionapi.ClassRecordRequest) (decisionapi.ClassRecords, error) {
	var zero decisionapi.ClassRecords
	if err := req.Validate(); err != nil {
		return zero, shoal.NewError(shoal.ErrorInvalidArgument, "invalid class record request")
	}
	if ctx.Err() != nil {
		return zero, ctx.Err()
	}
	p.f.mu.Lock()
	entry, ok := p.f.entries[req.CohortID]
	var sample decisiondatasets.Sample
	if ok {
		sample = entry.sample
	}
	p.f.mu.Unlock()
	if !ok {
		return zero, shoal.NewError(shoal.ErrorNotFound, "cohort "+string(req.CohortID)+" is not a prepared fixture cohort")
	}
	w := decisiondatasets.Window{From: req.From, To: req.To}
	if w.To.IsZero() {
		w.To = sample.Cutoff
	}
	records, err := decisiondatasets.Aggregate(sample, w)
	if err != nil {
		return zero, err
	}
	return classRecordsToAPI(records), nil
}

func classRecordsToAPI(r decisiondatasets.Records) decisionapi.ClassRecords {
	out := decisionapi.ClassRecords{CohortID: r.CohortID, TaskID: r.TaskID, QuestionID: r.QuestionID, PolicyID: r.PolicyID, Cutoff: r.Cutoff, From: r.Window.From, To: r.Window.To, Members: r.Members, InWindow: r.InWindow, Outside: r.Outside}
	for _, c := range r.Classes {
		rec := decisionapi.ClassRecord{ClassID: c.ClassID, Class: c.Class, Targets: c.Targets, FirstObserved: c.FirstObserved, LastObserved: c.LastObserved}
		rec.Adjudication = decisionapi.ClassAdjudication{VerifiedAgree: c.Adjudication.VerifiedAgree, VerifiedDisagree: c.Adjudication.VerifiedDisagree, Disputed: c.Adjudication.Disputed, Unresolved: c.Adjudication.Unresolved, None: c.Adjudication.None, Withdrawn: c.Adjudication.Withdrawn, Stale: c.Adjudication.Stale, Later: c.Adjudication.Later, TrainingDenied: c.Adjudication.TrainingDenied}
		rec.Outcomes = decisionapi.ClassOutcomes{Receipts: c.Outcomes.Receipts, Correctness: c.Outcomes.Correctness, Superseded: c.Outcomes.Superseded}
		if c.Precision != nil {
			v := *c.Precision
			rec.Precision = &v
		}
		out.Classes = append(out.Classes, rec)
	}
	return out
}

// Close releases every prepared registry's engine.
func (f *fixtureCohorts) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	var errs []error
	for _, id := range f.order {
		if e := f.entries[id].engine; e != nil {
			errs = append(errs, e.Close())
		}
	}
	return errors.Join(errs...)
}
