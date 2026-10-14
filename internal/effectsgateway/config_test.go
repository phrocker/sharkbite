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

package effectsgateway

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	dispatchapi "github.com/phrocker/shoal-oss/pkg/dispatch/api"
)

const configRoutes = `[{"action":"charge","method":"POST","path":"/v1/charges",` +
	`"effects":["external","egresses-content"],"idempotency":"key"}]`

const configUnprotectedRoutes = `[{"action":"notify","method":"POST","path":"/v1/notify",` +
	`"effects":["external","egresses-content"],"idempotency":"unprotected"}]`

// baseArgs is a complete, valid configuration. Each refusal case below
// replaces or removes exactly one flag, so a refusal is attributable to it.
func baseArgs() map[string]string {
	return map[string]string{
		"dispatch-url":          "https://explorer.shoal.svc",
		"agent-id":              "Z2F0ZXdheQ",
		"surface-name":          "payments",
		"target-base-url":       "https://api.example.com",
		"routes":                configRoutes,
		"idempotency-header":    "Idempotency-Key",
		"idempotency-retention": "24h",
		"executor-ref":          "stripe",
		"unrecorded-dir":        "/var/lib/shoal-gateway",
		"pod-name":              "gw-0",
	}
}

func argList(values map[string]string) []string {
	args := make([]string, 0, len(values))
	for name, value := range values {
		if name == "" {
			continue
		}
		args = append(args, "-"+name+"="+value)
	}
	return args
}

func with(changes map[string]string) []string {
	values := baseArgs()
	for name, value := range changes {
		if value == "<unset>" {
			delete(values, name)
			continue
		}
		values[name] = value
	}
	return argList(values)
}

func TestParseFlagsAcceptsTheBaseConfiguration(t *testing.T) {
	config, err := ParseFlags(with(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if config.DispatchURL.String() != "https://explorer.shoal.svc" ||
		string(config.AgentID) != "gateway" || config.AgentIDEncoded != "Z2F0ZXdheQ" ||
		config.Capability != DefaultCapability || config.SurfaceName != "payments" ||
		config.TargetAuthHeader != "Authorization" ||
		config.IdempotencyHeader != "Idempotency-Key" ||
		config.IdempotencyRetention != 24*time.Hour ||
		config.ClaimLease != DefaultClaimLease ||
		config.OperationTimeout != DefaultOperationTimeout ||
		config.PlaneTimeout != DefaultPlaneTimeout || config.Renew {
		t.Fatalf("config = %#v", config)
	}
	if !reflect.DeepEqual(config.Effects,
		dispatchapi.Effects{dispatchapi.EffectEgressesContent, dispatchapi.EffectMutatesExternal}) {
		t.Fatalf("effects = %v", config.Effects)
	}
	if _, ok := config.Routes.Lookup("charge"); !ok {
		t.Fatal("route missing")
	}
	// The default plane timeout (10s) makes the completion budget 30s.
	if config.GracePeriod() != DefaultOperationTimeout+30*time.Second+2*ReportWindow+5*time.Second {
		t.Fatalf("grace = %s", config.GracePeriod())
	}
	gate := config.SendGate()
	if gate.OperationTimeout != DefaultOperationTimeout || gate.Renew {
		t.Fatalf("gate = %#v", gate)
	}
}

// TestParseFlagsAcceptsRenewal: -renew is accepted and carried to the
// configuration and its send gate, and it is what admits an operation the
// lease alone could not hold — the design's example, T = 10m under L = 60s.
func TestParseFlagsAcceptsRenewal(t *testing.T) {
	args := map[string]string{
		"renew": "true", "claim-lease": "1m", "operation-timeout": "10m",
		"plane-timeout": "15s",
	}
	if _, err := ParseFlags(with(map[string]string{
		"claim-lease": "1m", "operation-timeout": "10m", "plane-timeout": "15s",
	}), io.Discard); err == nil || !strings.Contains(err.Error(), "without renewal") {
		t.Fatalf("without -renew the design example must be refused: %v", err)
	}
	config, err := ParseFlags(with(args), io.Discard)
	if err != nil {
		t.Fatalf("-renew refused: %v", err)
	}
	if !config.Renew || !config.SendGate().Renew ||
		config.SendGate().RenewAfter != 30*time.Second {
		t.Fatalf("renewal not carried: renew=%v gate=%#v", config.Renew, config.SendGate())
	}
	if config.GracePeriodSeconds() != 660 {
		t.Fatalf("grace = %ds, want 660s for T = 10m and a 15s plane timeout",
			config.GracePeriodSeconds())
	}
}

func TestParseFlagsCarriesTheCommandFlags(t *testing.T) {
	config, err := ParseFlags(with(map[string]string{
		"max-in-flight":              "8",
		"attestation-statement-file": "/run/attest/statement",
		"attestation-key-file":       "/run/attest/key",
	}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if config.ExecutorRef != "stripe" || config.UnrecordedDir != "/var/lib/shoal-gateway" ||
		config.Pod != "gw-0" || config.MaxInFlight != 8 ||
		config.AttestationStatementFile != "/run/attest/statement" ||
		config.AttestationKeyFile != "/run/attest/key" {
		t.Fatalf("config = %#v", config)
	}
	config, err = ParseFlags(with(map[string]string{"pod-name": "<unset>"}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if host, _ := os.Hostname(); config.Pod != host || config.MaxInFlight != DefaultMaxInFlight {
		t.Fatalf("pod %q (host %q), max in flight %d", config.Pod, host, config.MaxInFlight)
	}
}

func TestDefaultsSatisfyTheirOwnInvariants(t *testing.T) {
	if err := ValidateDurations(DefaultClaimLease, DefaultOperationTimeout,
		DefaultPlaneTimeout, false); err != nil {
		t.Fatalf("the shipped defaults are refused: %v", err)
	}
}

func TestParseFlagsRefusals(t *testing.T) {
	t.Setenv("SHOAL_DISPATCH_TOKEN", "")
	for _, refused := range []struct {
		name    string
		changes map[string]string
		cites   string
	}{
		{"dispatch URL missing", map[string]string{"dispatch-url": "<unset>"}, "-dispatch-url is required"},
		{"dispatch URL remote plaintext", map[string]string{"dispatch-url": "http://explorer.shoal.svc"}, "https, or http addressing loopback"},
		{"dispatch URL relative", map[string]string{"dispatch-url": "/api"}, "absolute URL"},
		{"dispatch URL with userinfo", map[string]string{"dispatch-url": "https://u:p@explorer"}, "userinfo"},
		{"dispatch URL with query", map[string]string{"dispatch-url": "https://explorer?x=1"}, "query or fragment"},
		{"dispatch plaintext opt-in does not admit ftp", map[string]string{"dispatch-url": "ftp://explorer", "allow-plaintext-dispatch": "true"}, "-dispatch-url"},
		{"dispatch token env and file", map[string]string{"dispatch-token-env": "X", "dispatch-token-file": "/tmp/t"}, "mutually exclusive"},
		{"dispatch token env typed as its default beside a file", map[string]string{"dispatch-token-env": "SHOAL_DISPATCH_TOKEN", "dispatch-token-file": "/tmp/t"}, "mutually exclusive"},
		{"dispatch token neither", map[string]string{"dispatch-token-env": ""}, "-dispatch-token-env or -dispatch-token-file is required"},
		{"agent ID missing", map[string]string{"agent-id": "<unset>"}, "-agent-id is required"},
		{"agent ID display name", map[string]string{"agent-id": "payments gateway"}, "unpadded base64url"},
		{"agent ID padded", map[string]string{"agent-id": "Z2F0ZXdheQ=="}, "unpadded base64url"},
		{"capability with space", map[string]string{"capability": "effects http"}, "-capability may use only"},
		{"capability untrimmed", map[string]string{"capability": " effects.http"}, "leading or trailing"},
		{"surface name missing", map[string]string{"surface-name": "<unset>"}, "-surface-name is required"},
		{"target missing", map[string]string{"target-base-url": "<unset>"}, "-target-base-url is required"},
		{"target remote plaintext", map[string]string{"target-base-url": "http://api.example.com"}, "https, or http addressing loopback"},
		{"target plaintext not admitted by the dispatch opt-in", map[string]string{"target-base-url": "http://api.example.com", "allow-plaintext-dispatch": "true"}, "-target-base-url"},
		{"target with fragment", map[string]string{"target-base-url": "https://api.example.com/#x"}, "query or fragment"},
		{"target with userinfo", map[string]string{"target-base-url": "https://key@api.example.com"}, "userinfo"},
		{"loopback target without allow-private", map[string]string{"target-base-url": "http://127.0.0.1:9000", "routes": strings.ReplaceAll(configRoutes, `"external","egresses-content"`, `"external"`)}, "-target-allow-private"},
		{"target credential env and file", map[string]string{"target-credential-env": "X", "target-credential-file": "/tmp/t"}, "-target-credential-env and -target-credential-file are mutually exclusive"},
		{"routes missing", map[string]string{"routes": "<unset>"}, "-routes is required"},
		{"routes invalid", map[string]string{"routes": `[{"action":"x"}]`}, "-routes[0]"},
		{"routes understate egress for a remote target", map[string]string{"routes": strings.ReplaceAll(configRoutes, `"external","egresses-content"`, `"external"`)}, "do not match"},
		{"key route without a header", map[string]string{"idempotency-header": "<unset>"}, "-idempotency-header is required"},
		{"key route without retention", map[string]string{"idempotency-retention": "<unset>"}, "-idempotency-retention is required"},
		{"retention inside the operation", map[string]string{"idempotency-retention": "3m5s"}, "no action could ever be claimed"},
		{"header with no key route", map[string]string{"routes": configUnprotectedRoutes, "idempotency-retention": "<unset>"}, "no route uses"},
		{"retention with no key route", map[string]string{"routes": configUnprotectedRoutes, "idempotency-header": "<unset>"}, "no route uses"},
		{"auth header invalid", map[string]string{"target-auth-header": "Bad Header"}, "-target-auth-header must be a valid"},
		{"auth header the binder writes", map[string]string{"target-auth-header": "content-type"}, "Content-Type"},
		{"idempotency header the binder writes", map[string]string{"idempotency-header": "User-Agent"}, "User-Agent"},
		{"idempotency header equals auth header", map[string]string{"idempotency-header": "authorization"}, "must differ"},
		{"lease over the fleet ceiling", map[string]string{"claim-lease": "5m1s"}, "must not exceed 5m0s"},
		{"lease zero", map[string]string{"claim-lease": "0s"}, "-claim-lease must be positive"},
		{"operation zero", map[string]string{"operation-timeout": "0s"}, "-operation-timeout must be positive"},
		{"plane zero", map[string]string{"plane-timeout": "0s"}, "-plane-timeout must be positive"},
		{"plane over L/4", map[string]string{"plane-timeout": "1m1s"}, "quarter"},
		{"lease cannot cover the call", map[string]string{"operation-timeout": "3m50s"}, "without renewal"},
		{"renewal does not lift the plane bound", map[string]string{"renew": "true", "claim-lease": "1m", "plane-timeout": "16s"}, "quarter"},
		{"executor ref missing", map[string]string{"executor-ref": "<unset>"}, "-executor-ref"},
		{"executor ref outside the charset", map[string]string{"executor-ref": "stripe ref"}, "-executor-ref"},
		{"unrecorded dir missing", map[string]string{"unrecorded-dir": "<unset>"}, "-unrecorded-dir is required"},
		{"max in flight zero", map[string]string{"max-in-flight": "0"}, "-max-in-flight"},
		{"max in flight over its bound", map[string]string{"max-in-flight": "65"}, "-max-in-flight"},
		{"pod name with a separator", map[string]string{"pod-name": "gw|0"}, "-pod-name"},
		{"attestation statement without a key", map[string]string{"attestation-statement-file": "/s"}, "required together"},
		{"attestation key without a statement", map[string]string{"attestation-key-file": "/k"}, "required together"},
		{"response bound zero", map[string]string{"max-response-bytes": "0"}, "-max-response-bytes"},
		{"response bound over the output bound", map[string]string{"max-response-bytes": "1048577"}, "-max-response-bytes"},
		{"pull limit zero", map[string]string{"pull-limit": "0"}, "-pull-limit"},
		{"pull limit over the page bound", map[string]string{"pull-limit": "257"}, "-pull-limit"},
		{"pull interval too short", map[string]string{"pull-interval": "1ms"}, "-pull-interval"},
		{"health address not host:port", map[string]string{"health-address": "8081"}, "-health-address"},
		{"positional argument", map[string]string{"": ""}, ""},
	} {
		args := with(refused.changes)
		if refused.name == "positional argument" {
			args = append(with(nil), "stray")
			refused.cites = "unexpected argument"
		}
		_, err := ParseFlags(args, io.Discard)
		if err == nil {
			t.Errorf("%s: accepted", refused.name)
			continue
		}
		if !strings.Contains(err.Error(), refused.cites) {
			t.Errorf("%s: refused, but not by the rule under test: %v", refused.name, err)
		}
	}
	if _, err := ParseFlags(append(with(nil), "-no-such-flag"), io.Discard); err == nil {
		t.Error("an unknown flag was accepted")
	}
}

func TestParseFlagsAcceptsTheDocumentedExceptions(t *testing.T) {
	// A remote http explorer with the explicit opt-in.
	if _, err := ParseFlags(with(map[string]string{
		"dispatch-url": "http://explorer.shoal.svc:8080", "allow-plaintext-dispatch": "true",
	}), io.Discard); err != nil {
		t.Fatalf("plaintext dispatch with opt-in: %v", err)
	}
	// A loopback target, which derives {external} and needs allow-private.
	config, err := ParseFlags(with(map[string]string{
		"target-base-url":      "http://127.0.0.1:9000/api",
		"target-allow-private": "true",
		"routes":               strings.ReplaceAll(configRoutes, `"external","egresses-content"`, `"external"`),
	}), io.Discard)
	if err != nil {
		t.Fatalf("loopback target: %v", err)
	}
	if !reflect.DeepEqual(config.Effects, dispatchapi.Effects{dispatchapi.EffectMutatesExternal}) {
		t.Fatalf("loopback effects = %v", config.Effects)
	}
	// An agent ID with surrounding whitespace is normalized, and the
	// normalized form is what is kept.
	config, err = ParseFlags(with(map[string]string{"agent-id": " Z2F0ZXdheQ "}), io.Discard)
	if err != nil || config.AgentIDEncoded != "Z2F0ZXdheQ" {
		t.Fatalf("untrimmed agent ID: %v, %q", err, config.AgentIDEncoded)
	}
	// A file credential with the env flag left at its default.
	config, err = ParseFlags(with(map[string]string{"dispatch-token-file": "/var/run/token"}), io.Discard)
	if err != nil || config.DispatchCredential == nil {
		t.Fatalf("file credential: %v", err)
	}
}

func TestValidateDurations(t *testing.T) {
	for _, row := range []struct {
		name                    string
		lease, operation, plane time.Duration
		renew                   bool
		ok                      bool
	}{
		{"defaults", DefaultClaimLease, DefaultOperationTimeout, DefaultPlaneTimeout, false, true},
		{"lease at the ceiling", 5 * time.Minute, 4 * time.Minute, 30 * time.Second, false, true},
		{"lease over the ceiling", 5*time.Minute + 1, time.Minute, time.Second, false, false},
		// L > T + 5s + plane, strictly.
		{"exactly at the bound", time.Minute, 45 * time.Second, 10 * time.Second, false, false},
		{"one nanosecond over the bound", time.Minute + 1, 45 * time.Second, 10 * time.Second, false, true},
		// planeTimeout ≤ L/4, inclusive.
		{"plane exactly L/4", 4 * time.Minute, time.Minute, time.Minute, false, true},
		{"plane over L/4", 4 * time.Minute, time.Minute, time.Minute + 1, false, false},
		// With renewal the lease is a silence interval, not the bound on the
		// call: the design's T=10m, L=60s example is valid.
		{"renewing: design example", time.Minute, 10 * time.Minute, 15 * time.Second, true, true},
		{"renewing: plane still bounded", time.Minute, 10 * time.Minute, 16 * time.Second, true, false},
		{"renewing: lease still capped", 6 * time.Minute, 10 * time.Minute, time.Second, true, false},
		{"not renewing: design example refused", time.Minute, 10 * time.Minute, 15 * time.Second, false, false},
		{"negative operation", time.Minute, -time.Second, time.Second, false, false},
	} {
		err := ValidateDurations(row.lease, row.operation, row.plane, row.renew)
		if (err == nil) != row.ok {
			t.Errorf("%s: %v, want ok=%v", row.name, err, row.ok)
		}
	}
}

func TestValidateRetention(t *testing.T) {
	const operation = 3 * time.Minute
	for _, row := range []struct {
		name        string
		requiresKey bool
		retention   time.Duration
		ok          bool
	}{
		{"no key routes, no retention", false, 0, true},
		{"no key routes, retention set", false, time.Hour, false},
		{"key routes, no retention", true, 0, false},
		{"key routes, retention exactly T+5s", true, operation + ReportWindow, false},
		{"key routes, retention just over", true, operation + ReportWindow + 1, true},
		{"key routes, a day", true, 24 * time.Hour, true},
	} {
		err := ValidateRetention(row.requiresKey, row.retention, operation)
		if (err == nil) != row.ok {
			t.Errorf("%s: %v, want ok=%v", row.name, err, row.ok)
		}
	}
}

func TestCredentialSources(t *testing.T) {
	t.Setenv("EFFECTS_TEST_TOKEN", "")
	env := credentialFromEnv("EFFECTS_TEST_TOKEN")
	if _, err := env(); !errors.Is(err, ErrNoCredential) {
		t.Fatalf("unset env = %v, want ErrNoCredential", err)
	}
	t.Setenv("EFFECTS_TEST_TOKEN", "  tok  ")
	if value, err := env(); err != nil || value != "tok" {
		t.Fatalf("env = %q, %v", value, err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	file := credentialFromFile(path)
	if _, err := file(); err == nil || errors.Is(err, ErrNoCredential) {
		t.Fatalf("missing file = %v; a configured file that is absent is breakage, not absence", err)
	}
	if err := os.WriteFile(path, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := file(); err == nil || errors.Is(err, ErrNoCredential) {
		t.Fatalf("empty file = %v", err)
	}
	// Read per call, so a rotated file is used on the next request.
	if err := os.WriteFile(path, []byte("v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, _ := file()
	if err := os.WriteFile(path, []byte("v2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, _ := file()
	if first != "v1" || second != "v2" {
		t.Fatalf("rotation: %q then %q", first, second)
	}
}

func TestTargetAuthorization(t *testing.T) {
	t.Setenv("EFFECTS_TEST_TARGET", "")
	remote, err := ParseFlags(with(map[string]string{
		"target-credential-env": "EFFECTS_TEST_TARGET",
	}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := remote.TargetAuthorization(); err == nil {
		t.Fatal("a remote target with no credential would be sent an unauthenticated request")
	}
	loopback, err := ParseFlags(with(map[string]string{
		"target-credential-env": "EFFECTS_TEST_TARGET",
		"target-base-url":       "http://localhost:9000",
		"target-allow-private":  "true",
		"routes":                strings.ReplaceAll(configRoutes, `"external","egresses-content"`, `"external"`),
	}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if header, value, err := loopback.TargetAuthorization(); err != nil || header != "" || value != "" {
		t.Fatalf("loopback with no credential = %q %q %v", header, value, err)
	}
	t.Setenv("EFFECTS_TEST_TARGET", "Bearer t")
	if header, value, err := remote.TargetAuthorization(); err != nil ||
		header != "Authorization" || value != "Bearer t" {
		t.Fatalf("remote with credential = %q %q %v", header, value, err)
	}

	broken, err := ParseFlags(with(map[string]string{
		"target-credential-file": filepath.Join(t.TempDir(), "absent"),
		"target-base-url":        "http://localhost:9000",
		"target-allow-private":   "true",
		"routes":                 strings.ReplaceAll(configRoutes, `"external","egresses-content"`, `"external"`),
	}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := broken.TargetAuthorization(); err == nil {
		t.Fatal("a configured, unreadable credential file was treated as absence on loopback")
	}
}
