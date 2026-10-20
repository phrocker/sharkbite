// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/phrocker/shoal-oss/internal/adjudicatedfixture"
	"github.com/phrocker/shoal-oss/internal/decisiondatasets"
	"github.com/phrocker/shoal-oss/internal/decisionlearning"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
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
	task     string
	registry *adjudicatedfixture.Registry
	engine   *engine.Engine
	resolver auth.Resolver
	// ctx carries the demo's own fixture-exporter decision: the registry's
	// authority governs source permission, the web caller's authority
	// governs who may ask for an export.
	ctx    context.Context
	cohort decisiondatasets.Cohort
	// sealed is the first export, returned for every later request: the
	// census is immutable, and the recorded manifest pin must be
	// re-obtainable for an offline bundle export.
	sealed *decisiondatasets.Bundle
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
// <stateDir>/fixture-cohorts. The census is synthetic and regenerated on
// every start, so the previous preparation is removed first; identities in
// it (cutoffs, request ids) are fresh each time.
func mountFixtureCohorts(stateDir string, tasks []string, output io.Writer) (*fixtureCohorts, error) {
	root := filepath.Join(stateDir, "fixture-cohorts")
	if err := os.RemoveAll(root); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	f := &fixtureCohorts{entries: map[shoal.ID]*fixtureCohort{}}
	for _, task := range tasks {
		registry, eng, authority, ctx, err := adjudicatedfixture.Prepare(filepath.Join(root, task), task)
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("fixture cohort %s: %w", task, err)
		}
		cohort := registry.Cohort()
		f.entries[cohort.ID] = &fixtureCohort{task: task, registry: registry, engine: eng, resolver: authority.Resolver(), ctx: ctx, cohort: cohort}
		f.order = append(f.order, cohort.ID)
		fmt.Fprintf(output, "learning: fixture cohort %s prepared (%d members, synthetic conformance fixture, not a sampling authority)\n", cohort.ID, len(cohort.Members))
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
		out = append(out, decisionlearning.Cohort{ID: c.ID, TaskID: string(c.Task.ID()), LabelPolicyID: string(c.Policy.ID()), Members: len(c.Members), Mode: c.Mode})
	}
	return out, nil
}

// Export produces the authorized export of one fixture cohort. The first
// request runs the demo's real export session; later requests return the
// same sealed bytes. The registry is single-owner, so exports are
// serialized.
func (f *fixtureCohorts) Export(ctx context.Context, id shoal.ID) (decisionlearning.Export, error) {
	if ctx.Err() != nil {
		return decisionlearning.Export{}, ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	entry, ok := f.entries[id]
	if !ok {
		return decisionlearning.Export{}, shoal.NewError(shoal.ErrorNotFound, "cohort "+string(id)+" is not a prepared fixture cohort")
	}
	if entry.sealed == nil {
		bundle, err := adjudicatedfixture.FreshExport(entry.ctx, entry.registry, entry.resolver, id)
		if err != nil {
			return decisionlearning.Export{}, err
		}
		entry.sealed = &bundle
	}
	if ctx.Err() != nil {
		return decisionlearning.Export{}, ctx.Err()
	}
	return decisionlearning.Export{Dataset: append([]byte(nil), entry.sealed.Dataset...), Manifest: append([]byte(nil), entry.sealed.Manifest...)}, nil
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
