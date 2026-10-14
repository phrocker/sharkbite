// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied. See the License for the
// specific language governing permissions and limitations
// under the License.

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	admissionapi "github.com/phrocker/shoal-oss/pkg/admission/api"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// handlerProvider stands behind the real admission handler. Only the test
// binary links the fleet and webapi packages; the proxy itself does not.
type handlerProvider struct {
	mu        sync.Mutex
	outcome   fleet.AdmissionOutcome
	withhold  []shoal.ID
	reportErr error
	requests  []fleet.AdmissionRequest
	reports   []fleet.AdmissionReport
}

func (p *handlerProvider) Request(
	_ context.Context, request fleet.AdmissionRequest,
) (fleet.AdmissionGrant, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, request)
	return fleet.AdmissionGrant{
		Outcome: p.outcome,
		// The claim is created under the token ID the caller chose, so the
		// grant carries it back, as dispatch_service does.
		Token: fleet.AdmissionToken{
			ActionID: append([]byte{0, 0xff}, request.ID...), TokenID: request.TokenID,
			Version: 1, ExpiresAt: time.Now().Add(time.Minute),
		},
		Obligations: fleet.Obligations{Withhold: p.withhold},
	}, nil
}

func (p *handlerProvider) Report(
	_ context.Context, report fleet.AdmissionReport,
) (fleet.ActionRecord, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reports = append(p.reports, report)
	if p.reportErr != nil {
		return fleet.ActionRecord{}, p.reportErr
	}
	state := fleet.DispatchSucceeded
	if report.Failed {
		state = fleet.DispatchFailed
	}
	return fleet.ActionRecord{
		ID: report.Token.ActionID, Version: report.Token.Version + 1,
		State: state, UpdatedAt: time.Now(),
	}, nil
}

func (p *handlerProvider) Outstanding(
	context.Context, fleet.OutstandingAdmissionsRequest,
) (fleet.OutstandingAdmissionsPage, error) {
	return fleet.OutstandingAdmissionsPage{}, nil
}

// newHandlerProxy fronts the real webapi admission handler with a path prefix,
// the way a path-routed ingress would, and points a proxy at it.
func newHandlerProxy(
	t *testing.T, provider *handlerProvider, upstream *fakeUpstream,
) (*proxy, *[]string) {
	t.Helper()
	return newHandlerProxyOver(t, provider, upstream)
}

// newHandlerProxyOver is newHandlerProxy over any provider.
func newHandlerProxyOver(
	t *testing.T, provider webapi.AdmissionProvider, upstream *fakeUpstream,
) (*proxy, *[]string) {
	t.Helper()
	handler, err := webapi.NewAdmissionHandler(provider)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/shoal/", http.StripPrefix("/shoal", handler))
	plane := httptest.NewServer(mux)
	t.Cleanup(plane.Close)
	base, err := url.Parse(plane.URL + "/shoal/")
	if err != nil {
		t.Fatal(err)
	}
	planeClient := newHTTPClient(5 * time.Second)
	planeClient.Transport = plane.Client().Transport
	var logged []string
	var logMu sync.Mutex
	governed, err := newProxy(&admissionClient{
		base: base, http: planeClient,
		credential:      func() (string, error) { return "plane-token", nil },
		agentID:         "YWdlbnQ",
		agentGeneration: 1,
		capability:      "llm.gateway", action: "complete",
		lease: time.Minute,
	}, upstream.server.URL,
		func() (string, error) { return "upstream-key", nil },
		[]string{"example.test"}, []string{"gpt"}, 5*time.Second, time.Now,
		func(format string, values ...any) {
			logMu.Lock()
			defer logMu.Unlock()
			logged = append(logged, fmt.Sprintf(format, values...))
		})
	if err != nil {
		t.Fatal(err)
	}
	governed.client.Transport = upstream.server.Client().Transport
	return governed, &logged
}

// TestTheProxyInteroperatesWithTheRealAdmissionHandler drives the proxy against
// pkg/explorer/webapi's handler rather than a fake that shares the proxy's
// assumptions: allowed then reported with the token's exact bytes, denied with
// no egress, and an obligation the proxy cannot meet reported as failed.
func TestTheProxyInteroperatesWithTheRealAdmissionHandler(t *testing.T) {
	t.Run("allowed and reported", func(t *testing.T) {
		provider := &handlerProvider{outcome: fleet.AdmissionAllowed}
		upstream := newFakeUpstream(t)
		governed, logged := newHandlerProxy(t, provider, upstream)
		if recorder := post(t, governed, plainCall); recorder.Code != http.StatusOK {
			t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
		}
		if upstream.calls != 1 {
			t.Fatalf("upstream calls = %d", upstream.calls)
		}
		if len(provider.requests) != 1 || len(provider.reports) != 1 {
			t.Fatalf("requests = %d reports = %d logs = %q",
				len(provider.requests), len(provider.reports), *logged)
		}
		request, report := provider.requests[0], provider.reports[0]
		if !bytes.Equal(report.Token.ActionID, append([]byte{0, 0xff}, request.ID...)) ||
			!bytes.Equal(report.Token.TokenID, request.TokenID) || report.Failed ||
			len(report.Outcome) == 0 {
			t.Fatalf("report = %#v", report)
		}
		if request.SourceID != nil || request.PolicyID != nil {
			t.Fatalf("nil source/policy arrived as %#v / %#v", request.SourceID, request.PolicyID)
		}
		for _, line := range *logged {
			if strings.Contains(line, "report failed") {
				t.Fatalf("report was not acknowledged: %q", line)
			}
		}
	})
	t.Run("denied", func(t *testing.T) {
		provider := &handlerProvider{outcome: fleet.AdmissionDenied}
		upstream := newFakeUpstream(t)
		governed, _ := newHandlerProxy(t, provider, upstream)
		if recorder := post(t, governed, plainCall); recorder.Code != http.StatusForbidden {
			t.Fatalf("status = %d", recorder.Code)
		}
		if upstream.calls != 0 || len(provider.reports) != 0 {
			t.Fatalf("upstream = %d reports = %d", upstream.calls, len(provider.reports))
		}
	})
	t.Run("obligation the proxy cannot meet", func(t *testing.T) {
		provider := &handlerProvider{
			outcome: fleet.AdmissionObligated, withhold: []shoal.ID{"never-declared"},
		}
		upstream := newFakeUpstream(t)
		governed, _ := newHandlerProxy(t, provider, upstream)
		if recorder := post(t, governed, plainCall); recorder.Code != http.StatusForbidden {
			t.Fatalf("status = %d", recorder.Code)
		}
		if upstream.calls != 0 || len(provider.reports) != 1 ||
			!provider.reports[0].Failed ||
			provider.reports[0].ErrorCode != "obligation_unsatisfiable" {
			t.Fatalf("upstream = %d reports = %#v", upstream.calls, provider.reports)
		}
	})
	t.Run("spent token on report is logged", func(t *testing.T) {
		provider := &handlerProvider{
			outcome: fleet.AdmissionAllowed, reportErr: fleet.ErrAdmissionSpent,
		}
		upstream := newFakeUpstream(t)
		governed, logged := newHandlerProxy(t, provider, upstream)
		if recorder := post(t, governed, plainCall); recorder.Code != http.StatusOK {
			t.Fatalf("status = %d", recorder.Code)
		}
		found := false
		for _, line := range *logged {
			found = found || strings.Contains(line, "returned 409")
		}
		if !found {
			t.Fatalf("a 409 on report was not surfaced: %q", *logged)
		}
	})
}

// TestPlaneFailuresKeepTheirWording pins the adapter's mapping: every way of
// not getting an answer is ErrPlaneUnreachable, never ErrDenied, and the
// plane refusing this proxy's own credential says so.
func TestPlaneFailuresKeepTheirWording(t *testing.T) {
	for status, want := range map[int]string{
		http.StatusUnauthorized:       "proxy credential rejected",
		http.StatusForbidden:          "proxy credential rejected",
		http.StatusConflict:           "admission request returned 409",
		http.StatusServiceUnavailable: "admission request returned 503",
		http.StatusFound:              "admission request returned 302",
	} {
		plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
		plane.status = status
		upstream := newFakeUpstream(t)
		governed, _ := newTestProxy(t, plane, upstream)
		_, err := governed.admission.request(context.Background(), callerIdentity{
			AdmissionID: "YQ", TokenID: "dA", RequestID: "cg", ObjectID: "bw",
		}, []byte(`{}`), nil, time.Now())
		if !errors.Is(err, ErrPlaneUnreachable) || errors.Is(err, ErrDenied) ||
			!strings.Contains(err.Error(), want) {
			t.Errorf("%d: err = %v, want %q", status, err, want)
		}
	}
}
