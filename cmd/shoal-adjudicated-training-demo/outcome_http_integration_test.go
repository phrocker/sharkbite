// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/decisionoutcomehttp"
	outcomes "github.com/phrocker/shoal-oss/internal/decisionoutcomes"
	"github.com/phrocker/shoal-oss/internal/explorercoord"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/decision/api"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type lockedOutcomeProvider struct {
	mu sync.Mutex
	api.OutcomeProvider
}

func (p *lockedOutcomeProvider) AppendOutcome(ctx context.Context, o api.OutcomeObservation, k []byte) (api.OutcomeReceipt, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.OutcomeProvider.AppendOutcome(ctx, o, k)
}
func (p *lockedOutcomeProvider) ReadOutcome(ctx context.Context, r, pr shoal.ID, k []byte) (api.OutcomeReceipt, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.OutcomeProvider.ReadOutcome(ctx, r, pr, k)
}
func httpObservation(c decision.OutcomeObservationConfig) api.OutcomeObservation {
	return api.OutcomeObservation{RequestID: c.RequestID, PredictionID: c.PredictionID, SubjectID: c.SubjectID, Kind: string(c.Kind), QuestionID: c.QuestionID, Label: c.Label, Truth: c.Truth, ExecutionStatus: string(c.ExecutionStatus), ActionID: c.ActionID, EvidenceIDs: append([]shoal.ID(nil), c.EvidenceIDs...), ObservedAt: c.ObservedAt, AssertedProvenance: api.OutcomeProvenance{ReporterID: c.AssertedProvenance.ReporterID, ModelID: c.AssertedProvenance.ModelID, PromptID: c.AssertedProvenance.PromptID, ToolID: c.AssertedProvenance.ToolID}, Supersedes: c.Supersedes}
}
func TestOutcomeHTTPRealInventoryRecoveryAndSDK(t *testing.T) {
	for _, task := range []string{"source", "review"} {
		t.Run(task, func(t *testing.T) {
			r, eng, a, exportCtx, e := prepareRegistry(filepath.Join(t.TempDir(), task), task)
			if e != nil {
				t.Fatal(e)
			}
			defer eng.Close()
			backend, e := explorercoord.NewEngineStore(eng, outcomes.Table)
			if e != nil {
				t.Fatal(e)
			}
			publication := &durableFailedPublication{Admission: r.admission}
			store, e := outcomes.New(outcomes.Config{Backend: backend, Resolver: a.Resolver(), Authority: outcomeAuthority{r}, Admission: publication, Clock: time.Now})
			if e != nil {
				t.Fatal(e)
			}
			adapter, e := decisionoutcomehttp.New(store)
			if e != nil {
				t.Fatal(e)
			}
			provider := &lockedOutcomeProvider{OutcomeProvider: adapter}
			server := httptest.NewUnstartedServer(nil)
			host, e := webapi.NewAuthenticatedHandler(&datasetWorkspace{}, webapi.AuthenticatorFunc(func(req *http.Request) (auth.Decision, error) {
				role := "fixture-reporter"
				switch req.Header.Get("Authorization") {
				case "Bearer primary":
				case "Bearer secondary":
					role = "fixture-reporter-secondary"
				default:
					return auth.Decision{}, errors.New("unknown credential")
				}
				_, d, e := bindRole(a, role)
				return d, e
			}), a.Binder(), server.Listener.Addr().String())
			if e != nil {
				t.Fatal(e)
			}
			if e = host.MountOutcomes(provider, a.Resolver()); e != nil {
				t.Fatal(e)
			}
			server.Config.Handler = host
			server.Start()
			defer server.Close()
			client := func(token string) *api.Client {
				c, e := api.NewClient(api.Config{BaseURL: server.URL, HTTPClient: server.Client(), Token: func(context.Context) (string, error) { return token, nil }})
				if e != nil {
					t.Fatal(e)
				}
				return c
			}
			primary, secondary := client("primary"), client("secondary")
			v := r.rows[r.cohort.Members[0].ID]
			o := httpObservation(v.outcome.ObservationConfig)
			o.ObservedAt = time.Now().UTC()
			o.AssertedProvenance.ReporterID = "fixture-reporter"
			o.AssertedProvenance.ModelID = "asserted-model-only"
			key := []byte("http-late-report")
			got, e := primary.AppendOutcome(context.Background(), o, key)
			if e != nil {
				t.Fatal(e)
			}
			if got.State != "proposed" || got.SubmitterID != "fixture-reporter" || got.Observation.AssertedProvenance.ModelID != "asserted-model-only" {
				t.Fatal("attribution laundered")
			}
			read, e := primary.ReadOutcome(context.Background(), o.RequestID, o.PredictionID, key)
			if e != nil || !reflect.DeepEqual(got, read) {
				t.Fatal("GET changed receipt", e)
			}
			again, e := primary.AppendOutcome(context.Background(), o, key)
			if e != nil || !reflect.DeepEqual(got, again) {
				t.Fatal("POST retry changed original", e)
			}
			if durableVerified(durableStatuses(t, durableBundle(t, r, exportCtx))) != 13 {
				t.Fatal("HTTP outcome did not quarantine")
			}
			before := durableSnapshot(t, r, v)
			altered := o
			altered.Label = "negative"
			if altered.Label == o.Label {
				altered.Label = "positive"
			}
			if _, e = primary.AppendOutcome(context.Background(), altered, key); e == nil {
				t.Fatal("changed-key payload accepted")
			}
			if after := durableSnapshot(t, r, v); after.ID != before.ID {
				t.Fatal("conflict changed inventory")
			}
			if _, e = secondary.ReadOutcome(context.Background(), o.RequestID, o.PredictionID, key); e == nil || errors.Is(e, api.ErrIndeterminate) {
				t.Fatal("GET crossed principal or claimed write", e)
			}
			otherObservation := o
			otherObservation.AssertedProvenance.ReporterID = "fixture-reporter-secondary"
			other, e := secondary.AppendOutcome(context.Background(), otherObservation, key)
			if e != nil || other.ID == got.ID {
				t.Fatal("second reporter failed", e)
			}
			if after := durableSnapshot(t, r, v); len(after.Entries) != 3 {
				t.Fatal("reporters not unioned")
			}
			provider.mu.Lock()
			publication.fail = true
			provider.mu.Unlock()
			pendingKey := []byte("http-pending-publication")
			failed, e := primary.AppendOutcome(context.Background(), o, pendingKey)
			if !errors.Is(e, api.ErrIndeterminate) || failed.ID != "" {
				t.Fatal("publication uncertainty lost", e)
			}
			pending := durableSnapshot(t, r, v)
			if pending.Complete() {
				t.Fatal("pending appeared complete")
			}
			committed, e := primary.ReadOutcome(context.Background(), o.RequestID, o.PredictionID, pendingKey)
			if e != nil {
				t.Fatal(e)
			}
			if durableSnapshot(t, r, v).ID != pending.ID {
				t.Fatal("GET repaired inventory")
			}
			if b, e := freshExport(exportCtx, r, r.resolver, r.cohort.ID); e == nil || len(b.Dataset) != 0 {
				t.Fatal("pending inventory exported")
			}
			provider.mu.Lock()
			publication.fail = false
			provider.mu.Unlock()
			repaired, e := primary.AppendOutcome(context.Background(), o, pendingKey)
			if e != nil || !reflect.DeepEqual(repaired, committed) || !durableSnapshot(t, r, v).Complete() {
				t.Fatal("exact POST did not repair original", e)
			}
			provider.mu.Lock()
			r.readable = false
			provider.mu.Unlock()
			if receipt, e := primary.ReadOutcome(context.Background(), o.RequestID, o.PredictionID, key); e == nil || receipt.ID != "" || errors.Is(e, api.ErrIndeterminate) {
				t.Fatal("revoked GET disclosed", e)
			}
			if receipt, e := primary.AppendOutcome(context.Background(), o, []byte("denied")); e == nil || receipt.ID != "" {
				t.Fatal("revoked POST disclosed")
			}
		})
	}
}
