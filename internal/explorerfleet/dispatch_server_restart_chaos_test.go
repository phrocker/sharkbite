// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package explorerfleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/dispatch/api"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
)

func chaosClock() time.Time {
	return time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
}

// TestDispatchClaimSurvivesLostResponseAndServerSIGKILL is opt-in for the
// scheduled campaign. It starts two independent embedded-primary server
// processes in succession against the same durable directory. The first
// commits a claim but drops its HTTP answer, then dies without a clean close.
// The replacement must recover the one winning fence and refuse a rival.
func TestDispatchClaimSurvivesLostResponseAndServerSIGKILL(t *testing.T) {
	if os.Getenv("SHOAL_CHAOS_SERVER_RESTART") != "1" {
		t.Skip("scheduled server-restart chaos campaign")
	}
	now := chaosClock()
	directory := t.TempDir()
	runtime := openFleetDispatchRuntime(t, directory)
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	registry, dispatch := composeIntegratedServices(t, runtime, authority, &integratedExecutor{}, now)
	decision := integratedDecision(t, now)
	bound, err := authority.Binder().Bind(context.Background(), decision)
	if err != nil {
		t.Fatal(err)
	}
	descriptor := registerIntegratedAgent(t, registry, bound, now)
	actionID := []byte("server-restart-claim-action")
	queued, err := dispatch.Enqueue(bound, integratedEnqueue(now, descriptor,
		actionID, []byte("server-restart-claim-key")))
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}

	first := startChaosDispatchServer(t, directory, true)
	client := chaosDispatchClient(t, first.url)
	claim := api.Claim{
		Context: api.Context{
			RequestID: []byte("request"), CorrelationID: []byte("correlation"),
			ReasonCode: "gateway_claim", Deadline: now.Add(time.Minute),
		},
		ExpectedVersion: queued.Version, ClaimID: []byte("gateway-a"), Lease: time.Minute,
	}
	claimCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, err = client.Claim(claimCtx, actionID, claim)
	cancel()
	if api.Kind(err) != api.KindRepull {
		t.Fatalf("lost claim response = %v (%v), want repull and no executable claim", api.Kind(err), err)
	}
	first.kill(t)

	second := startChaosDispatchServer(t, directory, false)
	defer second.kill(t)
	recovered := chaosDispatchClient(t, second.url)
	status, err := recovered.Status(context.Background(), actionID, claim.Context)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != api.DispatchClaimed || status.Version != 2 || status.ClaimFence != 1 {
		t.Fatalf("recovered durable claim = %+v, want claimed/v2/fence1", status)
	}
	replay, err := recovered.Claim(context.Background(), actionID, claim)
	if err != nil || replay.State != api.DispatchClaimed || replay.Version != status.Version ||
		replay.ClaimFence != status.ClaimFence {
		t.Fatalf("exact claim replay = %+v, %v; status = %+v", replay, err, status)
	}
	rival := claim
	rival.ClaimID = []byte("gateway-b")
	_, err = recovered.Claim(context.Background(), actionID, rival)
	if api.Kind(err) != api.KindConflict {
		t.Fatalf("rival stale claim = %v (%v), want conflict", api.Kind(err), err)
	}
	final, err := recovered.Status(context.Background(), actionID, claim.Context)
	if err != nil || final.State != api.DispatchClaimed || final.Version != 2 || final.ClaimFence != 1 {
		t.Fatalf("durable claim after rival = %+v, %v", final, err)
	}
	result := struct {
		ActionID         string            `json:"action_id"`
		ServerMode       string            `json:"server_mode"`
		Fault            string            `json:"fault"`
		FirstResponse    string            `json:"first_response"`
		RecoveredState   api.DispatchState `json:"recovered_state"`
		RecoveredVersion uint64            `json:"recovered_version"`
		RecoveredFence   uint64            `json:"recovered_fence"`
		ExactReplay      bool              `json:"exact_replay"`
		RivalRefused     bool              `json:"rival_refused"`
	}{string(actionID), chaosServerMode(), "claim_answer_dropped_then_server_sigkill", string(api.KindRepull),
		final.State, final.Version, final.ClaimFence, true, true}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("CHAOS_RESULT " + string(encoded))
}

type chaosServerProcess struct {
	cmd           *exec.Cmd
	url           string
	output        *bytes.Buffer
	dead          bool
	containerName string
	role          string
}

func chaosServerMode() string {
	if os.Getenv("SHOAL_CHAOS_CONTAINER_IMAGE") != "" {
		return "two_container_replacement"
	}
	return "two_process_replacement"
}

func startChaosDispatchServer(t *testing.T, directory string, dropClaim bool) *chaosServerProcess {
	t.Helper()
	ready := filepath.Join(directory, fmt.Sprintf("ready-%d", time.Now().UnixNano()))
	mode := "0"
	role := "replacement"
	if dropClaim {
		mode = "1"
		role = "first"
	}
	childEnv := []string{
		"SHOAL_CHAOS_SERVER_CHILD=1", "SHOAL_CHAOS_SERVER_DIR=" + directory,
		"SHOAL_CHAOS_SERVER_READY=" + ready, "SHOAL_CHAOS_DROP_CLAIM=" + mode,
	}
	var cmd *exec.Cmd
	containerName := ""
	if image := os.Getenv("SHOAL_CHAOS_CONTAINER_IMAGE"); image != "" {
		binary, err := filepath.Abs(os.Args[0])
		if err != nil {
			t.Fatal(err)
		}
		containerName = fmt.Sprintf("shoal-chaos-%d", time.Now().UnixNano())
		args := []string{"run", "--rm", "--pull=never", "--name=" + containerName,
			"--network=host", "--read-only", "--cap-drop=ALL",
			"--security-opt=no-new-privileges",
			fmt.Sprintf("--user=%d:%d", os.Getuid(), os.Getgid()),
			"--tmpfs=/tmp:rw,nosuid,nodev,size=16m",
			"--mount=type=bind,source=" + binary + ",target=/usr/local/bin/fleet.test,readonly",
			"--mount=type=bind,source=" + directory + ",target=" + directory}
		for _, entry := range childEnv {
			args = append(args, "--env="+entry)
		}
		args = append(args, image, "/usr/local/bin/fleet.test",
			"-test.run=^TestDispatchChaosServerChild$", "-test.timeout=60s")
		cmd = exec.Command("docker", args...)
	} else {
		cmd = exec.Command(os.Args[0], "-test.run=^TestDispatchChaosServerChild$", "-test.timeout=60s")
		cmd.Env = append(os.Environ(), childEnv...)
	}
	output := &bytes.Buffer{}
	cmd.Stdout, cmd.Stderr = output, output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	process := &chaosServerProcess{cmd: cmd, output: output, containerName: containerName, role: role}
	t.Cleanup(func() {
		if !process.dead {
			process.kill(t)
		}
	})
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(ready)
		if err == nil && strings.HasPrefix(string(data), "http://") {
			process.url = strings.TrimSpace(string(data))
			t.Logf("CHAOS_SERVER_START role=%s", role)
			return process
		}
		time.Sleep(20 * time.Millisecond)
	}
	process.kill(t)
	t.Fatalf("server did not become ready: %s", output.String())
	return nil
}

func (p *chaosServerProcess) kill(t *testing.T) {
	t.Helper()
	if p.dead {
		return
	}
	if p.containerName != "" {
		output, err := exec.Command("docker", "kill", p.containerName).CombinedOutput()
		if err != nil {
			t.Fatalf("failed to SIGKILL server container %s: %v: %s; server output: %s",
				p.containerName, err, output, p.output.String())
		}
		deadline := time.Now().Add(10 * time.Second)
		for {
			running, err := exec.Command("docker", "ps", "--filter", "name=^/"+p.containerName+"$", "--format", "{{.ID}}").CombinedOutput()
			if err != nil {
				t.Fatalf("cannot confirm server container exit: %v: %s", err, running)
			}
			if len(bytes.TrimSpace(running)) == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("server container remained running after SIGKILL: %s", running)
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Logf("CHAOS_FAULT role=%s container_sigkill_confirmed", p.role)
	} else {
		if err := p.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Fatal(err)
		}
		t.Logf("CHAOS_FAULT role=%s process_sigkill_sent", p.role)
	}
	if err := p.cmd.Wait(); err == nil {
		t.Fatal("SIGKILL unexpectedly returned a clean server exit")
	}
	p.dead = true
}

func chaosDispatchClient(t *testing.T, url string) *api.Client {
	t.Helper()
	client, err := api.NewClient(api.Config{
		BaseURL: url, Token: func(context.Context) (string, error) { return "test-token", nil },
		Clock:      chaosClock,
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestDispatchChaosServerChild(t *testing.T) {
	if os.Getenv("SHOAL_CHAOS_SERVER_CHILD") != "1" {
		return
	}
	now := chaosClock()
	runtime := openFleetDispatchRuntime(t, os.Getenv("SHOAL_CHAOS_SERVER_DIR"))
	defer runtime.Close()
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	_, dispatch := composeIntegratedServices(t, runtime, authority, &integratedExecutor{}, now)
	decision := integratedDecision(t, now)
	handler, err := webapi.NewFleetDispatchHandler(dispatch)
	if err != nil {
		t.Fatal(err)
	}
	var dropped atomic.Bool
	dropClaim := os.Getenv("SHOAL_CHAOS_DROP_CLAIM") == "1"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bound, err := authority.Binder().Bind(r.Context(), decision)
		if err != nil {
			http.Error(w, "binding failed", http.StatusInternalServerError)
			return
		}
		r = r.WithContext(bound)
		if dropClaim && r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/claim") &&
			dropped.CompareAndSwap(false, true) {
			recorded := httptest.NewRecorder()
			handler.ServeHTTP(recorded, r)
			if recorded.Code != http.StatusOK {
				w.WriteHeader(recorded.Code)
				_, _ = w.Write(recorded.Body.Bytes())
				return
			}
			connection, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = connection.Close()
			}
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	ready := os.Getenv("SHOAL_CHAOS_SERVER_READY")
	if err := os.WriteFile(ready+".tmp", []byte(server.URL+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(ready+".tmp", ready); err != nil {
		t.Fatal(err)
	}
	select {}
}
