// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/decisiondatasets"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
)

func TestSealedReferenceExportUsesRealReceipts(t *testing.T) {
	for _, name := range []string{"source", "review"} {
		t.Run(name, func(t *testing.T) {
			r, e, a, ctx, err := prepareRegistry(filepath.Join(t.TempDir(), "bundle"), name)
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			_, ca, err := exportSession(ctx, r, a.Resolver(), r.cohort.ID)
			if err != nil {
				t.Fatal(err)
			}
			bundle, err := freshExport(ctx, r, a.Resolver(), r.cohort.ID)
			if err != nil {
				t.Fatal(err)
			}
			var data struct {
				Rows []struct {
					Label  string `json:"label"`
					Status string `json:"label_status"`
				}
			}
			if err = json.Unmarshal(bundle.Dataset, &data); err != nil {
				t.Fatal(err)
			}
			if len(data.Rows) != 16 {
				t.Fatalf("got %d admitted rows", len(data.Rows))
			}
			counts := map[string]int{}
			for _, row := range data.Rows {
				counts[row.Label]++
			}
			if counts["negative"] != 7 || counts["positive"] != 7 {
				t.Fatalf("labels %+v", counts)
			}
			for _, v := range r.rows {
				if v.prediction.Validate() != nil {
					t.Fatal("invalid actual prediction")
				}
				if v.fixture.Status != "unknown" {
					if v.outcome.State != "proposed" || len(v.admitted) != 1 || v.admitted[0].BasisID != v.basis.ID() {
						t.Fatal("missing real admission chain")
					}
				}
			}
			d, err := a.Resolver().Resolve(ctx)
			if err != nil {
				t.Fatal(err)
			}
			clone, err := ca.Resolve(ctx, d, r.cohort.ID)
			if err != nil {
				t.Fatal(err)
			}
			clone.Members[0].FamilyIDs[0] = "substituted"
			if reflect.DeepEqual(clone, r.cohort) {
				t.Fatal("cohort returned mutable membership")
			}
			if _, err = ca.Resolve(ctx, d, "unregistered"); err == nil {
				t.Fatal("unknown cohort accepted")
			}
			bad := r.cohort.Members[0]
			bad.TargetID = "unregistered"
			if _, err = ca.Load(ctx, d, ca.cohort, bad); err == nil {
				t.Fatal("unregistered target accepted")
			}
			r.training = false
			if _, err = freshExport(ctx, r, a.Resolver(), r.cohort.ID); err == nil {
				t.Fatal("source read substituted for training permission")
			}
			r.training = true
			r.readable = false
			if _, err = freshExport(ctx, r, a.Resolver(), r.cohort.ID); err == nil {
				t.Fatal("revoked sources exported")
			}
			r.readable = true
			// A real newly ingested outcome invalidates the old admitted inventory.
			v := r.rows[r.cohort.Members[0].ID]
			cfg := v.outcome.ObservationConfig
			cfg.ObservedAt = time.Now().UTC()
			_, err = r.outcomes.Append(r.reporterCtx, cfg.RequestID, cfg.PredictionID, []byte("late-report"), cfg)
			if err != nil {
				t.Fatal(err)
			}
			bundle, err = freshExport(ctx, r, a.Resolver(), r.cohort.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err = json.Unmarshal(bundle.Dataset, &data); err != nil {
				t.Fatal(err)
			}
			verified := 0
			for _, row := range data.Rows {
				if row.Status == "verified" {
					verified++
				}
			}
			if verified != 13 {
				t.Fatalf("late report failed quarantine: %d", verified)
			}
			// Original source corruption is not a new negative example.
			path := filepath.Join(r.dir, "sources", string(v.fixture.ID)+".txt")
			if err = os.WriteFile(path, []byte("substitute"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err = freshExport(ctx, r, a.Resolver(), r.cohort.ID); err == nil {
				t.Fatal("substituted source exported")
			}
		})
	}
}

type changingAuthority struct {
	*cohortAuthority
	change func()
}

func (a changingAuthority) Verify(ctx context.Context, d auth.Decision, c decisiondatasets.Cohort, targets []decisiondatasets.Target) error {
	a.change()
	return a.cohortAuthority.Verify(ctx, d, c, targets)
}
func TestFinalInventoryAndTrainingRecheck(t *testing.T) {
	r, e, a, ctx, err := prepareRegistry(filepath.Join(t.TempDir(), "bundle"), "source")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	for _, change := range []func(){func() {
		v := r.rows[r.cohort.Members[0].ID]
		cfg := v.outcome.ObservationConfig
		cfg.ObservedAt = time.Now().UTC()
		if _, e := r.outcomes.Append(r.reporterCtx, cfg.RequestID, cfg.PredictionID, []byte("during-final-verify"), cfg); e != nil {
			t.Fatal(e)
		}
	}, func() { r.training = false }, func() { r.readable = false }, func() {
		calls := 0
		r.now = func() time.Time {
			calls++
			if calls == 48 {
				r.readable = false
			}
			return time.Now()
		}
	}, func() {
		calls := 0
		r.now = func() time.Time {
			calls++
			if calls == 48 {
				r.training = false
			}
			return time.Now()
		}
	}} {
		r.training = true
		r.readable = true
		r.now = time.Now
		_, ca, err := exportSession(ctx, r, a.Resolver(), r.cohort.ID)
		if err != nil {
			t.Fatal(err)
		}
		s, err := decisiondatasets.New(decisiondatasets.Config{Resolver: a.Resolver(), Authority: changingAuthority{ca, change}, Clock: time.Now})
		if err != nil {
			t.Fatal(err)
		}
		b, err := s.Export(ctx, r.cohort.ID)
		if err == nil || len(b.Dataset) != 0 {
			t.Fatal("changed final inventory/authority disclosed dataset")
		}
	}
}
func TestSeparateTaskBindingsAndOriginalFeatureOnly(t *testing.T) {
	source, err := definitions("source")
	if err != nil {
		t.Fatal(err)
	}
	review, err := definitions("review")
	if err != nil {
		t.Fatal(err)
	}
	if source.Task.ID() == review.Task.ID() || source.FeatureSchema == review.FeatureSchema {
		t.Fatal("task/model binding collision")
	}
	for _, name := range []string{"source", "review"} {
		for _, f := range fixtures(name) {
			values, err := features(name, f.Raw)
			if err != nil || len(values) != 3 {
				t.Fatal(err)
			}
			label, err := oracle(name, f.Raw)
			if err != nil || (label != "positive" && label != "negative") {
				t.Fatal(err)
			}
		}
	}
	if _, err = definitions("unregistered"); err == nil {
		t.Fatal("unknown task")
	}
}
