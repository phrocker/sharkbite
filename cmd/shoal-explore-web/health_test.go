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
	"net"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestHealthSurfaceAnswersTheProbeTheWorkspaceRefuses is the reason this
func TestHealthSurfaceAnswersTheProbeTheWorkspaceRefuses(t *testing.T) {
	workspaceURL, healthURL, stop := startWorkspaceWithHealth(t,
		"-allowed-host", "shoal.example.test")
	defer stop()

	workspaceHost := strings.TrimPrefix(workspaceURL, "http://")
	if status := probeStatus(t, workspaceURL+"/api/v1/auth-config", workspaceHost); status != http.StatusMisdirectedRequest {
		t.Fatalf("workspace port answered a socket-addressed probe with %d, want %d: "+
			"the host-authority gate is not refusing it, so this test no longer "+
			"demonstrates why the health listener exists",
			status, http.StatusMisdirectedRequest)
	}
	healthHost := strings.TrimPrefix(healthURL, "http://")
	if status := probeStatus(t, healthURL+"/readyz", healthHost); status != http.StatusOK {
		t.Fatalf("health port answered a socket-addressed probe with %d, want 200", status)
	}
}

// TestHealthSurfaceServesOnlyItsTwoRoutes pins the surface closed. A path that
func TestHealthSurfaceServesOnlyItsTwoRoutes(t *testing.T) {
	_, healthURL, stop := startWorkspaceWithHealth(t)
	defer stop()

	host := strings.TrimPrefix(healthURL, "http://")
	for _, path := range []string{
		"/", "/api/v1/meta", "/api/v1/auth-config", "/api/v1/documents",
		"/assets/app.js", "/healthz/../api/v1/meta",
	} {
		if status := probeStatus(t, healthURL+path, host); status != http.StatusNotFound {
			t.Fatalf("health port served %q with %d, want 404", path, status)
		}
	}
}

// TestHealthSurfaceIsAbsentUnlessRequested keeps the default unchanged: an
func TestHealthSurfaceIsAbsentUnlessRequested(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	output := &lockedBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{
			"-data", t.TempDir(), "-listen", "127.0.0.1:0", "-dev-auth",
		}, output)
	}()
	waitForListeningURL(t, output)
	if strings.Contains(output.String(), "Health surface") {
		t.Fatalf("health surface started without -health-address: %q", output.String())
	}
	cancel()
	waitForRunToReturn(t, done)
}

// TestHealthAddressBindFailureIsFatal proves the failure is reported rather
func TestHealthAddressBindFailureIsFatal(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{
			"-data", t.TempDir(), "-listen", "127.0.0.1:0", "-dev-auth",
			"-health-address", occupied.Addr().String(),
		}, &lockedBuffer{})
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("run returned nil for an unbindable health address")
		}
		if !strings.Contains(err.Error(), occupied.Addr().String()) {
			t.Fatalf("error does not name the address that failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run kept serving with no health listener")
	}
}

var healthSurfacePattern = regexp.MustCompile(`Health surface listening at (http://\S+)`)

// startWorkspaceWithHealth runs a workspace with both listeners on ephemeral
func startWorkspaceWithHealth(t *testing.T, extraArgs ...string) (string, string, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	output := &lockedBuffer{}
	done := make(chan error, 1)
	args := append([]string{
		"-data", t.TempDir(), "-listen", "127.0.0.1:0", "-dev-auth",
		"-health-address", "127.0.0.1:0",
	}, extraArgs...)
	go func() { done <- run(ctx, args, output) }()

	workspaceURL := waitForListeningURL(t, output)
	healthURL := ""
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if match := healthSurfacePattern.FindStringSubmatch(output.String()); match != nil {
			healthURL = match[1]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if healthURL == "" {
		cancel()
		t.Fatalf("health surface never reported an address: %q", output.String())
	}
	return workspaceURL, healthURL, func() {
		cancel()
		waitForRunToReturn(t, done)
	}
}

func waitForRunToReturn(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return")
	}
}

// probeStatus issues a GET with an explicit Host, which is how a kubelet probe
func probeStatus(t *testing.T, url, host string) int {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = host
	client := http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("GET %s with Host %s: %v", url, host, err)
	}
	defer response.Body.Close()
	return response.StatusCode
}
