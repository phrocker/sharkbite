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

package healthsurface

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"
)

// listener exists. The workspace handler refuses any request whose Host is not
// an exactly configured authority, and an orchestrator probe addresses the pod
// by an authority no static configuration can name. Here -allowed-host names an
// external authority, as a real deployment behind a Service must, and the probe
// arrives addressed to the socket instead.
//
// The two assertions are one claim: the same request that the workspace port
// answers 421 is answered 200 by the health port.
// exists on the workspace handler must not exist here, or the health listener
// becomes an unauthenticated way to reach the workspace.
// operator who does not ask for a second listener does not get one.
// than degraded into a workspace that serves while every probe is refused
// because nothing is listening.
// TestHealthHandlerSeparatesLivenessFromReadiness covers the three states the
// integration tests cannot hold still: not-yet-ready, ready, and draining.
//
// The draining case is the one that matters operationally. Readiness must drop
// so the endpoints controller stops routing new work, while liveness must not,
// because a pod shedding traffic on purpose has not failed and restarting it
// throws away the graceful close.
func TestHealthHandlerSeparatesLivenessFromReadiness(t *testing.T) {
	state := &State{}
	handler := NewHandler(state)

	for _, probe := range []struct {
		name       string
		transition func()
		readiness  int
		liveness   int
	}{
		{"before serving", func() {}, http.StatusServiceUnavailable, http.StatusOK},
		{"serving", state.MarkReady, http.StatusOK, http.StatusOK},
		{"draining", state.MarkDraining, http.StatusServiceUnavailable, http.StatusOK},
	} {
		probe.transition()
		if got := handlerStatus(t, handler, "/readyz"); got != probe.readiness {
			t.Fatalf("%s: /readyz = %d, want %d", probe.name, got, probe.readiness)
		}
		if got := handlerStatus(t, handler, "/healthz"); got != probe.liveness {
			t.Fatalf("%s: /healthz = %d, want %d", probe.name, got, probe.liveness)
		}
	}
}

// TestHealthResponsesCarryNoWorkspaceState pins the disclosure property: the
// bodies are fixed strings. A future readiness check that consults the corpus
// or the policy catalog must not reach an unauthenticated prober through here.
func TestHealthResponsesCarryNoWorkspaceState(t *testing.T) {
	state := &State{}
	state.MarkReady()
	handler := NewHandler(state)
	for path, want := range map[string]string{"/healthz": "ok\n", "/readyz": "ready\n"} {
		recorder := recordHealth(t, handler, path)
		if body := recorder.body; body != want {
			t.Fatalf("%s body = %q, want %q", path, body, want)
		}
		if got := recorder.header.Get("Content-Type"); got != "text/plain; charset=utf-8" {
			t.Fatalf("%s content type = %q", path, got)
		}
	}
}

// TestDrainDropsReadinessBeforeTheWorkspaceStopsAccepting tests the wiring,
// not the bit. TestHealthHandlerSeparatesLivenessFromReadiness sets the state
// by hand and would keep passing if shutdown never touched it; this asserts the
// order the shutdown path actually executes, by reading readiness at the moment
// the workspace is asked to stop.
func TestDrainDropsReadinessBeforeTheWorkspaceStopsAccepting(t *testing.T) {
	state := &State{}
	state.MarkReady()
	workspace := &recordingServer{state: state}

	if err := Drain(context.Background(), state, workspace, nil); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if !workspace.calledShutdown {
		t.Fatal("drain never shut the workspace down")
	}
	if workspace.readyAtShutdown {
		t.Fatal("the workspace was shut down while still reporting ready: " +
			"the endpoints controller had no not-ready reading to act on, so " +
			"requests routed during the gap arrive at a closed listener")
	}
}

// TestDrainReportsTheWorkspaceErrorOverTheHealthClose pins which failure wins.
func TestDrainReportsTheWorkspaceErrorOverTheHealthClose(t *testing.T) {
	state := &State{}
	state.MarkReady()
	want := errors.New("workspace close failed")
	err := Drain(context.Background(), state, &recordingServer{state: state, err: want}, nil)
	if !errors.Is(err, want) {
		t.Fatalf("drain error = %v, want %v", err, want)
	}
}

type recordingServer struct {
	state           *State
	err             error
	calledShutdown  bool
	readyAtShutdown bool
}

func (s *recordingServer) Shutdown(context.Context) error {
	s.calledShutdown = true
	s.readyAtShutdown = s.state.ready.Load()
	return s.err
}

// TestHealthServerReportsAServeLoopThatDiedOnItsOwn covers the failure the
// roleops shutdown path had (#382): a serve loop that returns an unexpected
// error, where the error was consumed by whichever reader won a race and never
// reached the caller.
//
// Closing the listener directly, rather than through Shutdown, is what makes
// the error survive: once Shutdown has been called, net/http reports every
// subsequent accept failure as ErrServerClosed, which is exactly the graceful
// case this must not be confused with.
func TestHealthServerReportsAServeLoopThatDiedOnItsOwn(t *testing.T) {
	health, err := Start("127.0.0.1:0", &State{})
	if err != nil {
		t.Fatal(err)
	}
	if err := health.listener.Close(); err != nil {
		t.Fatal(err)
	}
	<-health.done

	shutdownErr := health.Shutdown(context.Background())
	if shutdownErr == nil {
		t.Fatal("shutdown reported success for a serve loop that failed: " +
			"the error was recorded by the goroutine and never returned")
	}
	if errors.Is(shutdownErr, http.ErrServerClosed) {
		t.Fatalf("shutdown reported the graceful close instead of the failure: %v", shutdownErr)
	}
}

// TestHealthShutdownJoinsTheServeLoop is the other half, and the one the test
// above cannot make: that shutdown waits for the goroutine rather than reading
// a field the goroutine has not written yet.
//
// The listener blocks in Accept for a fixed delay, so the serve loop is
// provably still running when Shutdown returns. A shutdown that joins cannot
// return until the loop has finished; one that does not returns while the
// goroutine is still in flight, which is both a lost error and a data race on
// serveErr.
func TestHealthShutdownJoinsTheServeLoop(t *testing.T) {
	restore := stubHealthListener(t, &slowFailingListener{
		delay: 250 * time.Millisecond, err: errors.New("health listener failed"),
	})
	defer restore()

	health, err := Start("127.0.0.1:0", &State{})
	if err != nil {
		t.Fatal(err)
	}
	if err := health.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	select {
	case <-health.done:
	default:
		t.Fatal("shutdown returned while the serve loop was still running: " +
			"serveErr is read without waiting for the goroutine that writes it")
	}
}

// slowFailingListener blocks in Accept for a fixed delay and then fails. The
// error is a plain error rather than a net.Error, so net/http treats it as
// fatal and returns instead of retrying.
type slowFailingListener struct {
	delay time.Duration
	err   error
}

func (l *slowFailingListener) Accept() (net.Conn, error) {
	time.Sleep(l.delay)
	return nil, l.err
}
func (l *slowFailingListener) Close() error   { return nil }
func (l *slowFailingListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

func stubHealthListener(t *testing.T, listener net.Listener) func() {
	t.Helper()
	previous := ListenTCP
	ListenTCP = func(string, string) (net.Listener, error) { return listener, nil }
	return func() { ListenTCP = previous }
}

// ports and returns their URLs plus a stop function that waits for a clean
// shutdown.
// differs from a browser request: the authority names the socket rather than
// the external name the deployment is configured for.
type healthRecorder struct {
	status int
	body   string
	header http.Header
}

func recordHealth(t *testing.T, handler http.Handler, path string) healthRecorder {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()

	response, err := http.Get(fmt.Sprintf("http://%s%s", listener.Addr(), path))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body := make([]byte, 64)
	read, _ := response.Body.Read(body)
	return healthRecorder{
		status: response.StatusCode,
		body:   string(body[:read]),
		header: response.Header,
	}
}

func handlerStatus(t *testing.T, handler http.Handler, path string) int {
	t.Helper()
	return recordHealth(t, handler, path).status
}
