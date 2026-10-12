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

// main's own job is the process: real SIGTERM and SIGINT delivered to a real
// process, and its exit status. The test binary re-executes itself as the
// gateway; the composition behind it is tested in
// internal/effectsgateway/gatewaycmd.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/effectsgateway"
	"github.com/phrocker/shoal-oss/internal/effectsgateway/gatewaycmd"
	dispatchapi "github.com/phrocker/shoal-oss/pkg/dispatch/api"
)

const reexec = "SHOAL_GATEWAY_TEST_MAIN"

func TestMain(m *testing.M) {
	if os.Getenv(reexec) == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

const routes = `[{"action":"status","method":"POST","path":"/v1/status",` +
	`"effects":["external"],"idempotency":"key"}]`

// idleExplorer resolves the descriptor and offers nothing, and counts pulls.
func idleExplorer(t *testing.T, target *url.URL) (*httptest.Server, func() int) {
	t.Helper()
	table, err := effectsgateway.ParseRoutes([]byte(routes), effectsgateway.DerivedEffects(target))
	if err != nil {
		t.Fatal(err)
	}
	route, _ := table.Lookup("status")
	var mu sync.Mutex
	pulls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/resolve"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": base64.RawURLEncoding.EncodeToString([]byte("agent")), "generation": 1,
				"executor_ref": "stripe",
				"capabilities": []dispatchapi.Capability{{Name: effectsgateway.DefaultCapability,
					Actions: []dispatchapi.DescriptorAction{{Name: "status", Effects: route.Effects(),
						InputSchema: route.InputSchema(), OutputSchema: effectsgateway.OutputSchema()}}}},
			})
		case r.URL.Path == "/api/v1/fleet/actions/pull":
			mu.Lock()
			pulls++
			mu.Unlock()
			_, _ = w.Write([]byte(`{"actions":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server, func() int { mu.Lock(); defer mu.Unlock(); return pulls }
}

func gatewayProcess(t *testing.T, args ...string) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), reexec+"=1")
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd, &output
}

func exitCode(t *testing.T, cmd *exec.Cmd, bound time.Duration) int {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		if err != nil {
			t.Fatal(err)
		}
		return 0
	case <-time.After(bound):
		t.Fatal("the gateway process did not exit")
	}
	return -1
}

// TestSIGTERMDrainsTheProcessToACleanExit: the real process, a real signal.
func TestSIGTERMDrainsTheProcessToACleanExit(t *testing.T) {
	target := httptest.NewServer(http.NotFoundHandler())
	defer target.Close()
	targetURL, _ := url.Parse(target.URL)
	explorer, pulls := idleExplorer(t, targetURL)
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd, output := gatewayProcess(t, "run",
		"-dispatch-url="+explorer.URL, "-dispatch-token-file="+token,
		"-agent-id="+base64.RawURLEncoding.EncodeToString([]byte("agent")),
		"-executor-ref=stripe", "-surface-name=api.test",
		"-target-base-url="+target.URL, "-target-allow-private",
		"-routes="+routes, "-idempotency-header=Idempotency-Key", "-idempotency-retention=48h",
		"-unrecorded-dir="+filepath.Join(t.TempDir(), "unrecorded"), "-pod-name=gw-0",
		"-pull-interval=100ms")
	deadline := time.Now().Add(20 * time.Second)
	for pulls() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if pulls() == 0 {
		t.Fatalf("the gateway never pulled: %s", output)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := exitCode(t, cmd, 20*time.Second); code != gatewaycmd.ExitOK {
		t.Fatalf("exit %d: %s", code, output)
	}
}

func TestTheProcessExitsWithTheCommandsCode(t *testing.T) {
	cmd, output := gatewayProcess(t, "run", "-dispatch-url=https://explorer.test")
	if code := exitCode(t, cmd, 20*time.Second); code != gatewaycmd.ExitUsage {
		t.Fatalf("exit %d: %s", code, output)
	}
	cmd, output = gatewayProcess(t, "grace-period")
	if code := exitCode(t, cmd, 20*time.Second); code != 0 || strings.TrimSpace(output.String()) != "225" {
		t.Fatalf("exit %d: %q", code, output)
	}
}
