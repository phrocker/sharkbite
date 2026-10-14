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
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/effectsgateway"
	dispatchapi "github.com/phrocker/shoal-oss/pkg/dispatch/api"
)

// loseFirstComplete delivers the first /complete and loses its answer, the
// way a connection that broke after the explorer committed would.
type loseFirstComplete struct {
	inner http.RoundTripper
	mu    sync.Mutex
	sent  int
}

func (l *loseFirstComplete) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := l.inner.RoundTrip(request)
	if err != nil || !strings.HasSuffix(request.URL.Path, "/complete") {
		return response, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sent++
	if l.sent > 1 {
		return response, nil
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	return nil, io.ErrUnexpectedEOF
}

// TestEffectsGatewayClientReportsAPartialSendOnce drives the volume of a
// partial send (#427) against the real /complete handler: committed on the
// first attempt, its answer lost, the identical resend answered from the
// replay branch with the recorded volume — read back, not re-derived. And the
// replay comparison is real: the same failure with a different volume is not
// accepted as this report, so the gateway's acceptance means it never sent one.
func TestEffectsGatewayClientReportsAPartialSendOnce(t *testing.T) {
	t.Parallel()
	h := newGatewayHarness(t)
	ctx := context.Background()
	base, err := url.Parse(h.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	explorer := effectsgateway.NewExplorerClient(10 * time.Second)
	lossy := &loseFirstComplete{inner: explorer.Transport}
	explorer.Transport = lossy
	client, err := effectsgateway.NewDispatchClient(base, explorer,
		func() (string, error) { return "gateway", nil }, time.Now)
	if err != nil {
		t.Fatal(err)
	}

	h.enqueue("action-partial", `{"path":{"account":"a"},"body":{"amount":1}}`)
	offered, _ := pulled(t, client, "action-partial")
	claimID, _, _ := effectsgateway.NewClaimID("pod-0", nil)
	claimed, err := client.Claim(ctx, offered.ID, effectsgateway.ClaimRequest{
		Context: offered.Correlate(requestContext(t, "gateway_claim")), ExpectedVersion: offered.Version,
		ClaimID: claimID, Lease: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	volume := dispatchapi.Effected{Bytes: 517, Chunks: 1}
	completion := effectsgateway.Completion{
		Context: claimed.Correlate(requestContext(t, "gateway_complete")), ExpectedVersion: claimed.Version, ClaimFence: claimed.ClaimFence,
		ClaimID: claimID, Failed: true, ErrorCode: effectsgateway.ErrorOutcomeUnknown,
		Effected: volume,
	}
	recorded, err := client.Complete(ctx, claimed.ID, completion)
	if err != nil {
		t.Fatalf("the resend was not accepted: %v", err)
	}
	if lossy.sent != 2 {
		t.Fatalf("completion requests = %d, want the first and one resend", lossy.sent)
	}
	if recorded.State != dispatchapi.DispatchFailed || recorded.Effected != volume {
		t.Fatalf("recorded = %#v, want a failure carrying %+v", recorded, volume)
	}

	// Any other volume under the same claim is a different report: the record
	// keeps the first number, and the client, comparing what it read back,
	// says the outcome was recorded otherwise rather than taking it as this
	// report.
	different := completion
	different.Context = claimed.Correlate(requestContext(t, "gateway_complete"))
	different.Effected = dispatchapi.Effected{Bytes: volume.Bytes + 1, Chunks: 1}
	action, err := client.Complete(ctx, claimed.ID, different)
	if effectsgateway.DispatchKind(err) != effectsgateway.DispatchRecordedOtherwise {
		t.Fatalf("a different volume = %#v, %v; want recorded_otherwise", action, err)
	}
	if action.Effected != volume {
		t.Fatalf("the record changed to %+v, want the write-once %+v", action.Effected, volume)
	}
}
