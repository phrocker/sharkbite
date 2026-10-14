//go:build !embed

// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//   https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package cclient

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/thrift/gen/client"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/security"
)

// DialCoordinator's happy path requires a live coordinator; we only
// exercise the input-validation surface and the I/O bounds here. The
// remaining wire-level behaviour is covered by integration tests against
// a running cluster (out of scope for the C3 groundwork pass).

func TestDialCoordinator_RejectsEmptyAddr(t *testing.T) {
	_, err := DialCoordinator(context.Background(), "", "inst-uuid", "4.0.0-SNAPSHOT", DialOptions{})
	if err == nil || !strings.Contains(err.Error(), "empty coordinator addr") {
		t.Fatalf("expected empty-addr error, got %v", err)
	}
}

func TestDialCoordinator_RejectsEmptyInstance(t *testing.T) {
	_, err := DialCoordinator(context.Background(), "manager:9999", "", "4.0.0-SNAPSHOT", DialOptions{})
	if err == nil || !strings.Contains(err.Error(), "empty instanceID") {
		t.Fatalf("expected empty-instance error, got %v", err)
	}
}

func TestDialCoordinator_RejectsEmptyVersion(t *testing.T) {
	_, err := DialCoordinator(context.Background(), "manager:9999", "inst-uuid", "", DialOptions{})
	if err == nil || !strings.Contains(err.Error(), "empty accumuloVersion") {
		t.Fatalf("expected empty-version error, got %v", err)
	}
}

// TestDialCoordinator_HonoursCanceledContext: the compactor cancels its
// context on SIGTERM, and a dial that ignores that would hold shutdown
// for the full connect timeout.
func TestDialCoordinator_HonoursCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := DialCoordinator(ctx, "203.0.113.1:9999", "inst-uuid", "4.0.0-SNAPSHOT", DialOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// TestDialCoordinator_BoundsRPCAgainstSilentCoordinator is the
// regression test for an unbounded thrift TConfiguration: TSocket only
// installs a read deadline when SocketTimeout is positive, so a manager
// that completes the TCP handshake and then stops answering would pin
// the compactor's poll loop forever — no re-resolution, no failover, no
// shutdown.
func TestDialCoordinator_BoundsRPCAgainstSilentCoordinator(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	// Accept the connection and then go silent, holding it open.
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		accepted <- conn
	}()
	defer func() {
		select {
		case conn := <-accepted:
			conn.Close()
		default:
		}
	}()

	cc, err := DialCoordinator(
		context.Background(),
		ln.Addr().String(),
		"inst-uuid",
		"4.0.0-SNAPSHOT",
		DialOptions{ConnectTimeout: 5 * time.Second, RPCTimeout: 150 * time.Millisecond},
	)
	if err != nil {
		t.Fatalf("DialCoordinator: %v", err)
	}
	defer cc.Close()

	rpcErr := make(chan error, 1)
	go func() {
		_, err := cc.Raw().GetCompactionJob(
			context.Background(),
			client.NewTInfo(),
			security.NewTCredentials(),
			"shoal_default",
			"compactor-1:9810",
			"ECID-00000000-0000-0000-0000-000000000000",
		)
		rpcErr <- err
	}()

	select {
	case err := <-rpcErr:
		if err == nil {
			t.Fatal("expected a transport error from a silent coordinator")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("GetCompactionJob outlasted RPCTimeout; the socket read is unbounded")
	}
}
