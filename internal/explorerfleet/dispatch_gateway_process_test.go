// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package explorerfleet

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/dispatch/api"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
)

type processClaimResult struct {
	Kind   api.ErrorKind     `json:"kind,omitempty"`
	Fence  uint64            `json:"fence,omitempty"`
	State  api.DispatchState `json:"state,omitempty"`
	Replay bool              `json:"replay,omitempty"`
}

// TestDispatchGatewayClaimAcrossProcesses exercises the worker-facing API
// from two independent processes. With SHOAL_TEST_GATEWAY_CONTAINER_IMAGE set,
// those processes run in separate containers against the same host HTTP server.
// Only the server holds the embedded-primary directory lock; gateway replicas
// share its durable action record over HTTP. This does not model two servers.
func TestDispatchGatewayClaimAcrossProcesses(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	runtime := openFleetDispatchRuntime(t, t.TempDir())
	defer runtime.Close()
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	registry, dispatch := composeIntegratedServices(t, runtime, authority,
		&integratedExecutor{maxEffects: fleet.Effects{fleet.EffectMutatesExternal}}, now)
	decision := integratedDecision(t, now)
	ctx, err := authority.Binder().Bind(context.Background(), decision)
	if err != nil {
		t.Fatal(err)
	}
	descriptor := registerIntegratedAgent(t, registry, ctx, now, fleet.EffectMutatesExternal)
	queued, err := dispatch.Enqueue(ctx, integratedEnqueue(now, descriptor,
		[]byte("process-race-action"), []byte("process-race-key")))
	if err != nil {
		t.Fatal(err)
	}
	handler, err := webapi.NewFleetDispatchHandler(dispatch)
	if err != nil {
		t.Fatal(err)
	}
	// Hold the first claim at the HTTP boundary until both processes arrive.
	// This starts competing claim attempts from the same queued version.
	var mu sync.Mutex
	arrived := 0
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/claim-prepared") {
			mu.Lock()
			arrived++
			if arrived == 2 {
				close(release)
			}
			mu.Unlock()
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		bound, bindErr := authority.Binder().Bind(r.Context(), decision)
		if bindErr != nil {
			http.Error(w, "binding failed", http.StatusInternalServerError)
			return
		}
		handler.ServeHTTP(w, r.WithContext(bound))
	}))
	defer server.Close()
	containerImage := os.Getenv("SHOAL_TEST_GATEWAY_CONTAINER_IMAGE")
	childTimeout := 10 * time.Second
	if containerImage != "" {
		t.Log("gateway_claim_child_mode=container")
		childTimeout = 45 * time.Second
	} else {
		t.Log("gateway_claim_child_mode=host")
	}
	childCtx, cancelChildren := context.WithTimeout(ctx, childTimeout)
	defer cancelChildren()

	type outcome struct {
		result processClaimResult
		err    error
	}
	results := make(chan outcome, 2)
	var testBinary string
	if containerImage != "" {
		testBinary, err = filepath.Abs(os.Args[0])
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"gateway-a", "gateway-b"} {
		id := id
		go func() {
			childEnv := []string{
				"SHOAL_TEST_GATEWAY_CHILD=1",
				"SHOAL_TEST_GATEWAY_URL=" + server.URL,
				"SHOAL_TEST_GATEWAY_ID=" + id,
				"SHOAL_TEST_GATEWAY_ACTION=" + base64.RawURLEncoding.EncodeToString(queued.ID),
			}
			var child *exec.Cmd
			if containerImage == "" {
				child = exec.CommandContext(childCtx, os.Args[0], "-test.run=^TestDispatchGatewayClaimChild$")
				child.Env = append(os.Environ(), childEnv...)
			} else {
				args := []string{
					"run", "--rm", "--pull=never", "--network=host", "--read-only",
					"--cap-drop=ALL", "--security-opt=no-new-privileges",
					"--user=65532:65532", "--tmpfs=/tmp:rw,nosuid,nodev,size=16m",
					"--mount=type=bind,source=" + testBinary + ",target=/usr/local/bin/fleet.test,readonly",
				}
				for _, entry := range childEnv {
					args = append(args, "--env="+entry)
				}
				args = append(args, containerImage, "/usr/local/bin/fleet.test", "-test.run=^TestDispatchGatewayClaimChild$")
				child = exec.CommandContext(childCtx, "docker", args...)
			}
			output, runErr := child.CombinedOutput()
			if runErr != nil {
				results <- outcome{err: errors.New(string(output) + ": " + runErr.Error())}
				return
			}
			var result processClaimResult
			if err := json.Unmarshal(bytes.SplitN(output, []byte("\n"), 2)[0], &result); err != nil {
				results <- outcome{err: errors.New(string(output) + ": " + err.Error())}
				return
			}
			results <- outcome{result: result}
		}()
	}
	claimed, refused := 0, 0
	for range 2 {
		select {
		case item := <-results:
			if item.err != nil {
				t.Fatal(item.err)
			}
			switch {
			case item.result.State == api.DispatchClaimed && item.result.Fence == 1 && item.result.Replay:
				claimed++
			case item.result.Kind == api.KindConflict:
				refused++
			default:
				t.Fatalf("unexpected claim result: %+v", item.result)
			}
		case <-time.After(childTimeout):
			t.Fatal("gateway claim processes did not finish")
		}
	}
	if claimed != 1 || refused != 1 {
		t.Fatalf("claimed=%d refused=%d, want one of each", claimed, refused)
	}
	stored, err := dispatch.Status(ctx, fleet.StatusRequest{
		ID: queued.ID, Context: integratedContext(now),
	})
	if err != nil || stored.State != fleet.DispatchClaimed || stored.ClaimFence != 1 || stored.Version != 2 {
		t.Fatalf("durable action after race = %+v, %v", stored, err)
	}
}

func TestDispatchGatewayClaimChild(t *testing.T) {
	if os.Getenv("SHOAL_TEST_GATEWAY_CHILD") != "1" {
		return
	}
	actionID, err := base64.RawURLEncoding.DecodeString(os.Getenv("SHOAL_TEST_GATEWAY_ACTION"))
	if err != nil {
		t.Fatal(err)
	}
	client, err := api.NewClient(api.Config{
		BaseURL: os.Getenv("SHOAL_TEST_GATEWAY_URL"),
		Token:   func(context.Context) (string, error) { return "test-token", nil },
		Clock:   func() time.Time { return time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	contextValue := api.Context{
		RequestID:     []byte("request"),
		CorrelationID: []byte("correlation"),
		ReasonCode:    "gateway_claim",
		Deadline:      time.Date(2026, 9, 6, 12, 1, 0, 0, time.UTC),
	}
	claim := api.Claim{
		Context: contextValue, ExpectedVersion: 1,
		ClaimID: []byte(os.Getenv("SHOAL_TEST_GATEWAY_ID")), Lease: time.Minute,
	}
	record, err := client.ClaimPrepared(context.Background(), actionID, claim)
	result := processClaimResult{Kind: api.Kind(err), Fence: record.ClaimFence, State: record.State}
	if err == nil {
		replayed, replayErr := client.ClaimPrepared(context.Background(), actionID, claim)
		if replayErr != nil || replayed.ClaimFence != record.ClaimFence ||
			replayed.Version != record.Version {
			t.Fatalf("claim replay = %+v, %v; first claim = %+v", replayed, replayErr, record)
		}
		result.Replay = true
	}
	encoded, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	// The parent decodes this one line. Go's test harness adds no output for a
	// passing test when -test.v is absent.
	_, _ = os.Stdout.Write(append(encoded, '\n'))
}
