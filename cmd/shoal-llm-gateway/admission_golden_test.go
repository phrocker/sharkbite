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
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	admissionapi "github.com/phrocker/shoal-oss/pkg/admission/api"
)

// The gateway golden fixtures pin exactly what this proxy puts on the wire to
// the admission plane: method, path (under a path-prefixed base URL), every
// header, and the body bytes. They were captured before the proxy moved onto
// pkg/admission/api (#446 slice 2), so that move is provably byte-compatible
// for deployed planes. Regenerate deliberately with
//
//	go test ./cmd/shoal-llm-gateway -run TestGatewayAdmissionWireGolden -update
var updateGatewayGolden = flag.Bool(
	"update", false, "rewrite the gateway admission wire golden fixtures")

const gatewayGoldenDir = "../../pkg/admission/api/testdata/wire/gateway"

type capturedPlaneCall struct {
	method  string
	uri     string
	headers string
	body    []byte
}

// capturingPlane answers like the real handler would and records every byte it
// was sent.
type capturingPlane struct {
	mu     sync.Mutex
	calls  []capturedPlaneCall
	server *httptest.Server
}

func newCapturingPlane(t *testing.T, expires time.Time) *capturingPlane {
	t.Helper()
	plane := &capturingPlane{}
	plane.server = httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			raw, _ := io.ReadAll(request.Body)
			names := make([]string, 0, len(request.Header))
			for name := range request.Header {
				names = append(names, name)
			}
			sort.Strings(names)
			var headers strings.Builder
			for _, name := range names {
				for _, value := range request.Header[name] {
					// The port is the test server's and varies per run.
					if name == "Host" {
						continue
					}
					fmt.Fprintf(&headers, "header %s: %s\n", name, value)
				}
			}
			plane.mu.Lock()
			plane.calls = append(plane.calls, capturedPlaneCall{
				method: request.Method, uri: request.RequestURI,
				headers: headers.String(), body: raw,
			})
			plane.mu.Unlock()
			writer.Header().Set("Content-Type", "application/json; charset=utf-8")
			if strings.HasSuffix(request.URL.Path, "/request") {
				var sent struct {
					TokenID string `json:"token_id"`
				}
				_ = json.Unmarshal(raw, &sent)
				fmt.Fprintf(writer,
					`{"outcome":"allowed","token":{"action_id":"YQD_","token_id":%q,"version":2,"expires_at":%q},"withhold":[]}`+"\n",
					sent.TokenID, expires.Format(time.RFC3339Nano))
				return
			}
			fmt.Fprint(writer,
				`{"action_id":"YQD_","version":3,"state":"succeeded","reported_at":"2026-09-06T12:00:01+02:00"}`+"\n")
		}))
	t.Cleanup(plane.server.Close)
	return plane
}

func (p *capturingPlane) take(t *testing.T) capturedPlaneCall {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.calls) != 1 {
		t.Fatalf("plane calls = %d, want 1", len(p.calls))
	}
	call := p.calls[0]
	p.calls = nil
	return call
}

func (c capturedPlaneCall) String() string {
	return fmt.Sprintf("%s %s\n%sbody: %q\n", c.method, c.uri, c.headers, c.body)
}

func goldenGatewayClient(t *testing.T, plane *capturingPlane, now time.Time) *admissionClient {
	t.Helper()
	base, err := url.Parse(plane.server.URL + "/shoal/")
	if err != nil {
		t.Fatal(err)
	}
	planeClient := newHTTPClient(5 * time.Second)
	planeClient.Transport = plane.server.Client().Transport
	return &admissionClient{
		base: base, http: planeClient,
		credential:      func() (string, error) { return "plane-token", nil },
		clock:           func() time.Time { return now },
		agentID:         "YWdlbnQ",
		agentGeneration: 7,
		capability:      "llm.gateway", action: "complete",
		effects: []string{"egresses-content", "reads-corpus"},
		lease:   90 * time.Second,
	}
}

// TestGatewayAdmissionWireGolden is the compatibility contract of the bytes
// this proxy sends. See updateGatewayGolden.
func TestGatewayAdmissionWireGolden(t *testing.T) {
	// A fixed, non-UTC clock: the proxy derives deadlines from it and the wire
	// carries them in the clock's own zone.
	now := time.Date(2026, 9, 6, 14, 0, 0, 123456789,
		time.FixedZone("CEST", 2*3600))
	plane := newCapturingPlane(t, now.Add(time.Minute))
	client := goldenGatewayClient(t, plane, now)
	identity := callerIdentity{
		AdmissionID: "YWRtaXNzaW9u", TokenID: "dG9rZW4",
		RequestID: "cmVxdWVzdA", CorrelationID: "Y29ycmVsYXRpb24",
		ObjectID: "b2JqZWN0",
	}
	ctx := context.Background()
	declaration := json.RawMessage(`{"model":"gpt","messages":2}`)

	// nil source and policy, no disclosures.
	granted, err := client.request(ctx, identity, declaration, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	checkGatewayGolden(t, "request_minimal", plane.take(t).String())

	client.sourceID, client.policyID = []byte("source"), []byte{0, 0xff}
	if _, err := client.request(
		ctx, identity, declaration, []string{"ZG9jLWE", "ZG9jLWI"}, now,
	); err != nil {
		t.Fatal(err)
	}
	checkGatewayGolden(t, "request_with_disclosures", plane.take(t).String())

	if err := client.report(ctx, granted.token, identity,
		json.RawMessage(`{"usage":{"total_tokens":12}}`), "", nil, now); err != nil {
		t.Fatal(err)
	}
	checkGatewayGolden(t, "report_outcome", plane.take(t).String())

	if err := client.report(ctx, granted.token, identity,
		nil, "upstream_unreachable", nil, now); err != nil {
		t.Fatal(err)
	}
	checkGatewayGolden(t, "report_failed", plane.take(t).String())

	// A failure after a partial egress (#427). The field appears here and
	// nowhere else: the fixtures above are unchanged, which is the evidence
	// that a report with nothing to say about volume sends the bytes it sent
	// before the field existed.
	if err := client.report(ctx, granted.token, identity,
		nil, "response_truncated", &admissionapi.Effected{Bytes: 2048, Chunks: 3},
		now); err != nil {
		t.Fatal(err)
	}
	checkGatewayGolden(t, "report_failed_effected", plane.take(t).String())
}

func checkGatewayGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join(gatewayGoldenDir, name+".golden")
	if *updateGatewayGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing golden fixture (run with -update): %v", err)
	}
	if !bytes.Equal(want, []byte(got)) {
		t.Fatalf("gateway admission wire changed for %s\n--- want\n%s\n--- got\n%s",
			name, want, got)
	}
}
