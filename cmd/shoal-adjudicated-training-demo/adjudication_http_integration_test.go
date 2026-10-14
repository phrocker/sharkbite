// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package main

import (
	"context"
	"errors"
	"github.com/phrocker/shoal-oss/internal/decisionadjudicationhttp"
	"github.com/phrocker/shoal-oss/pkg/decision/api"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/shoal"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

type lockedAdjudicationProvider struct {
	mu sync.Mutex
	api.AdjudicationProvider
}

func (p *lockedAdjudicationProvider) Adjudicate(c context.Context, o api.AdjudicationProposal, k []byte) (api.AdjudicationReceipt, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.AdjudicationProvider.Adjudicate(c, o, k)
}
func (p *lockedAdjudicationProvider) AdjudicationHistory(c context.Context, id shoal.ID) (api.AdjudicationHistory, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.AdjudicationProvider.AdjudicationHistory(c, id)
}
func TestAdjudicationHTTPRealAuthorityAndSDK(t *testing.T) {
	for _, task := range []string{"source", "review"} {
		t.Run(task, func(t *testing.T) {
			r, eng, a, _, e := prepareRegistry(filepath.Join(t.TempDir(), task), task)
			if e != nil {
				t.Fatal(e)
			}
			defer eng.Close()
			adapter, e := decisionadjudicationhttp.New(r.service)
			if e != nil {
				t.Fatal(e)
			}
			provider := &lockedAdjudicationProvider{AdjudicationProvider: adapter}
			server := httptest.NewUnstartedServer(nil)
			host, e := webapi.NewAuthenticatedHandler(&datasetWorkspace{}, webapi.AuthenticatorFunc(func(req *http.Request) (auth.Decision, error) {
				role := "fixture-judge"
				switch req.Header.Get("Authorization") {
				case "Bearer judge":
				case "Bearer reporter":
					role = "fixture-reporter"
				default:
					return auth.Decision{}, errors.New("unknown credential")
				}
				_, d, e := bindRole(a, role)
				return d, e
			}), a.Binder(), server.Listener.Addr().String())
			if e != nil {
				t.Fatal(e)
			}
			if e = host.MountAdjudications(provider, a.Resolver()); e != nil {
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
			judge, reporter := client("judge"), client("reporter")
			ctx := context.Background()
			v := r.rows[r.cohort.Members[0].ID]
			history, e := judge.AdjudicationHistory(ctx, v.target)
			if e != nil || len(history.Receipts) != 1 {
				t.Fatal("initial history", e)
			}
			original := history.Receipts[0]
			retry, e := judge.Adjudicate(ctx, original.Proposal, []byte(v.fixture.ID))
			if e != nil || !reflect.DeepEqual(original, retry) {
				t.Fatal("original retry", e)
			}
			// Explicitly reopen this test's synthetic authority for a second admission.
			provider.mu.Lock()
			r.sealed = false
			provider.mu.Unlock()
			next := original.Proposal
			next.ExpectedHeadID = original.ID
			next.ExpectedVersion = original.Version
			next.Disposition = "unresolved"
			next.Label = ""
			next.Truth = nil
			next.Reason = "Additional independent evidence required"
			if got, e := reporter.Adjudicate(ctx, next, []byte("reporter-cannot-judge")); e == nil || got.ID != "" {
				t.Fatal("reporter adjudicated")
			}
			second, e := judge.Adjudicate(ctx, next, []byte("http-second"))
			if e != nil || second.Version != 2 || second.Adjudicator.SubjectID != "fixture-judge" {
				t.Fatal("second admission", e)
			}
			history, e = judge.AdjudicationHistory(ctx, v.target)
			if e != nil || len(history.Receipts) != 2 || !reflect.DeepEqual(history.Receipts[1], second) {
				t.Fatal("whole history", e)
			}
			retry, e = judge.Adjudicate(ctx, original.Proposal, []byte(v.fixture.ID))
			if e != nil || !reflect.DeepEqual(original, retry) {
				t.Fatal("old retry after advanced head", e)
			}
			if got, e := judge.Adjudicate(ctx, next, []byte("stale-head")); e == nil || got.ID != "" {
				t.Fatal("stale head accepted")
			}
			provider.mu.Lock()
			r.readable = false
			provider.mu.Unlock()
			if got, e := judge.AdjudicationHistory(ctx, v.target); e == nil || len(got.Receipts) != 0 || errors.Is(e, api.ErrIndeterminate) {
				t.Fatal("revoked history disclosure or uncertainty", e)
			}
			if got, e := judge.Adjudicate(ctx, next, []byte("http-second")); e == nil || got.ID != "" {
				t.Fatal("revoked retry disclosed")
			}
		})
	}
}
