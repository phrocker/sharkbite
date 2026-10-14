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

package gatewaycmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/internal/effectsgateway"
	dispatchapi "github.com/phrocker/shoal-oss/pkg/dispatch/api"
)

const secretReference = "ch_9876543210"

// seedLog writes entries to a real log directory, as a gateway would have,
// and releases it.
func seedLog(t *testing.T, entries ...effectsgateway.UnrecordedEntry) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "unrecorded")
	log, err := effectsgateway.OpenUnrecordedLog(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if err := log.Append(entry); err != nil {
			t.Fatal(err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func entry(action string, fence uint64) effectsgateway.UnrecordedEntry {
	return effectsgateway.UnrecordedEntry{
		ActionID: []byte(action), Fence: fence, ClaimNonce: effectsgateway.ClaimNonce{1, 2, 3},
		RouteAction: "status", Method: "POST", PathTemplate: "/v1/status",
		Outcome: dispatchapi.AmbiguityEffectObserved, Target: "api.stripe.test",
		Reference: secretReference, CorrelationID: []byte("trace-" + action),
		Status: 404, DispatchError: effectsgateway.DispatchNotFound,
	}
}

func held(t *testing.T, dir string) []effectsgateway.UnrecordedEntry {
	t.Helper()
	log, err := effectsgateway.OpenUnrecordedLog(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	return log.Entries()
}

func operator(args ...string) (int, string, string) {
	stdout, stderr := &syncBuffer{}, &syncBuffer{}
	code := Main(append([]string{"unrecorded"}, args...), Env{Stdout: stdout, Stderr: stderr})
	return code, stdout.String(), stderr.String()
}

// TestUnrecordedListPrintsClosedFieldsOnly: one JSON object per entry, with
// the fields the logging policy admits, and never the reference, the target
// or the correlation.
func TestUnrecordedListPrintsClosedFieldsOnly(t *testing.T) {
	dir := seedLog(t, entry("act-1", 3), entry("act-2", 1))
	code, stdout, stderr := operator("list", "-unrecorded-dir", dir)
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines: %q", lines)
	}
	for _, leaked := range []string{secretReference, "api.stripe.test", "trace-act", b64([]byte("trace-act-1"))} {
		if strings.Contains(stdout, leaked) {
			t.Fatalf("list printed %q:\n%s", leaked, stdout)
		}
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{
		"id": true, "action_id": true, "fence": true, "claim_nonce": true, "route_action": true,
		"method": true, "path_template": true, "outcome": true, "has_reference": true,
		"status": true, "dispatch_error": true, "first_at": true, "last_at": true, "attempts": true,
	}
	for field := range first {
		if !allowed[field] {
			t.Fatalf("list printed field %q", field)
		}
	}
	if first["id"] != b64([]byte("act-1"))+":3" || first["outcome"] != "effect_observed" ||
		first["has_reference"] != true || first["path_template"] != "/v1/status" {
		t.Fatalf("first = %v", first)
	}
}

func TestUnrecordedAck(t *testing.T) {
	dir := seedLog(t, entry("act-1", 3), entry("act-1", 4), entry("act-2", 1))

	// A bare action ID naming two fences is refused, and nothing is removed.
	if code, _, stderr := operator("ack", "-unrecorded-dir", dir, b64([]byte("act-1"))); code != ExitFailure ||
		!strings.Contains(stderr, "different fences") {
		t.Fatalf("ambiguous ack: exit %d %s", code, stderr)
	}
	// One unknown ID among known ones refuses the whole ack.
	if code, _, _ := operator("ack", "-unrecorded-dir", dir, b64([]byte("act-2")), b64([]byte("act-9"))); code != ExitFailure {
		t.Fatalf("unknown ack: exit %d", code)
	}
	if got := held(t, dir); len(got) != 3 {
		t.Fatalf("a refused ack removed entries: %d left", len(got))
	}
	// Flags after the positional argument are read too.
	code, stdout, stderr := operator("ack", b64([]byte("act-1"))+":3", b64([]byte("act-2")), "-unrecorded-dir", dir)
	if code != ExitOK {
		t.Fatalf("ack: exit %d %s", code, stderr)
	}
	if strings.Count(stdout, `"event":"unrecorded_cleared"`) != 2 {
		t.Fatalf("ack events: %s", stdout)
	}
	if got := held(t, dir); len(got) != 1 || got[0].Fence != 4 {
		t.Fatalf("after ack: %+v", got)
	}
	if code, _, _ := operator("ack", "-unrecorded-dir", dir, "-all", b64([]byte("act-1"))); code != ExitUsage {
		t.Fatalf("-all with IDs: exit %d", code)
	}
	if code, _, _ := operator("ack", "-unrecorded-dir", dir); code != ExitUsage {
		t.Fatalf("neither: exit %d", code)
	}
	if code, _, _ := operator("ack", "-unrecorded-dir", dir, "not base64!"); code != ExitUsage {
		t.Fatalf("bad ID: exit %d", code)
	}
	if code, _, stderr := operator("ack", "-unrecorded-dir", dir, "-all"); code != ExitOK {
		t.Fatalf("-all: exit %d %s", code, stderr)
	}
	if got := held(t, dir); len(got) != 0 {
		t.Fatalf("after -all: %+v", got)
	}
}

// TestUnrecordedRefusesWhileAGatewayRuns: the gateway owns the log while it
// runs, so list and ack both refuse rather than read or rewrite under it, and
// the entry is still there when the gateway lets go.
func TestUnrecordedRefusesWhileAGatewayRuns(t *testing.T) {
	dir := seedLog(t, entry("act-1", 3))
	gateway, err := effectsgateway.OpenUnrecordedLog(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"ack", "-unrecorded-dir", dir, "-all"},
		{"ack", "-unrecorded-dir", dir, b64([]byte("act-1")) + ":3"},
		{"list", "-unrecorded-dir", dir},
	} {
		if code, _, stderr := operator(args...); code != ExitFailure || !strings.Contains(stderr, "stop it first") {
			t.Fatalf("%q while locked: exit %d %s", args, code, stderr)
		}
	}
	if gateway.Len() != 1 {
		t.Fatal("the running gateway's log changed")
	}
	if err := gateway.Close(); err != nil {
		t.Fatal(err)
	}
	if got := held(t, dir); len(got) != 1 {
		t.Fatalf("an ack refused under the lock removed the entry: %+v", got)
	}
	if code, _, _ := operator("list"); code != ExitUsage {
		t.Fatalf("list without a directory: exit %d", code)
	}
}

// TestOperatorCommandsNeverCreateTheDirectory: a mistyped directory is an
// error, not an empty log that says nothing awaits reconciliation, and
// neither list nor ack leaves a directory or lock behind. A file is refused
// the same way.
func TestOperatorCommandsNeverCreateTheDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nonexistent")
	notADirectory := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADirectory, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{missing, notADirectory} {
		for _, args := range [][]string{
			{"list", "-unrecorded-dir", dir},
			{"ack", "-unrecorded-dir", dir, "-all"},
			{"ack", "-unrecorded-dir", dir, b64([]byte("act-1")) + ":1"},
		} {
			code, stdout, stderr := operator(args...)
			if code != ExitFailure || stdout != "" || !strings.Contains(stderr, "not an existing directory") {
				t.Fatalf("%q: exit %d, stdout %q, stderr %q", args, code, stdout, stderr)
			}
		}
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("an operator command created the directory: %v", err)
	}
}
