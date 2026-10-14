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

package conformance

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

type fakeExecutor struct {
	results map[string]CommandResult
}

func (f fakeExecutor) Run(_ context.Context, _ string, command []string) CommandResult {
	if result, ok := f.results[strings.Join(command, "\x00")]; ok {
		return result
	}
	return CommandResult{}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func TestReplayFixturesProduceDeterministicEvidenceWithoutClaimingReplacement(t *testing.T) {
	opts := Options{
		Root:     repositoryRoot(t),
		Mode:     "replay",
		Commit:   strings.Repeat("a", 40),
		Executor: fakeExecutor{},
	}
	first, code, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if code != 2 {
		t.Fatalf("exit code = %d, want 2: %#v", code, first.Gates)
	}
	for _, gate := range first.Gates {
		if gate.State != Unsupported || gate.EvidenceState != Pass || !gate.Required ||
			gate.Adapter == "" || len(gate.MissingRequiredGates) != 1 ||
			gate.ReplayFixture == "" || gate.FixtureSHA256 == "" {
			t.Fatalf("dishonest or incomplete adapter gate: %#v", gate)
		}
	}
	second, _, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := Marshal(first)
	b, _ := Marshal(second)
	if string(a) != string(b) {
		t.Fatalf("replay output is not deterministic:\n%s\n%s", a, b)
	}
}

func TestReplayFailureMakesRequiredGateFail(t *testing.T) {
	command := []string{"go", "test", "./internal/tserver", "-run", "^TestAcquireCreatesAnAccumuloCompatibleLockNode$", "-count=1"}
	verdict, code, err := Run(context.Background(), Options{
		Root:   repositoryRoot(t),
		Mode:   "replay",
		Commit: strings.Repeat("b", 40),
		Executor: fakeExecutor{results: map[string]CommandResult{
			strings.Join(command, "\x00"): {ExitCode: 1, Output: "fixture mismatch"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if code != 1 || verdict.Gates[0].State != Fail || verdict.Gates[0].EvidenceState != Fail {
		t.Fatalf("code=%d gate=%#v", code, verdict.Gates[0])
	}
}

func TestUnavailableLiveEnvironmentIsUnsupportedNotPass(t *testing.T) {
	command := []string{"python", "test/accumulo/harness.py", "role", "client"}
	verdict, code, err := Run(context.Background(), Options{
		Root:     repositoryRoot(t),
		Mode:     "live",
		Required: []string{"client"},
		Commit:   strings.Repeat("c", 40),
		Executor: fakeExecutor{results: map[string]CommandResult{
			strings.Join(command, "\x00"): {ExitCode: 2, Output: "SKIP (needs-docker)"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	for _, gate := range verdict.Gates {
		if gate.Role == "client" && gate.State != Unsupported {
			t.Fatalf("client state = %q, want unsupported", gate.State)
		}
	}
}

func TestLiveRoleCommandsPromoteOnlyTheirCompletedProductionGate(t *testing.T) {
	for _, role := range Roles {
		t.Run(role, func(t *testing.T) {
			command := []string{"python", "test/accumulo/harness.py", "role", role}
			verdict, code, err := Run(context.Background(), Options{
				Root:     repositoryRoot(t),
				Mode:     "live",
				Required: []string{role},
				Commit:   strings.Repeat("f", 40),
				Executor: fakeExecutor{results: map[string]CommandResult{
					strings.Join(command, "\x00"): {
						Output: "SHOAL_EVIDENCE role=" + role + " status=pass",
					},
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			index := slices.Index(Roles, role)
			gate := verdict.Gates[index]
			if code != 0 || gate.State != Pass || gate.EvidenceState != Pass ||
				len(gate.MissingRequiredGates) != 0 {
				t.Fatalf("code=%d gate=%#v", code, gate)
			}
			live := gate.Evidence[len(gate.Evidence)-1]
			if live.Kind != "live-command" || !slices.Equal(live.ReplayCommand, command) {
				t.Fatalf("live evidence = %#v", live)
			}
		})
	}
}

func TestLiveRoleZeroExitWithoutEvidenceMarkerFailsClosed(t *testing.T) {
	command := []string{"python", "test/accumulo/harness.py", "role", "tserver"}
	verdict, code, err := Run(context.Background(), Options{
		Root:     repositoryRoot(t),
		Mode:     "live",
		Required: []string{"tserver"},
		Commit:   strings.Repeat("0", 40),
		Executor: fakeExecutor{results: map[string]CommandResult{
			strings.Join(command, "\x00"): {Output: "commands completed"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if code != 1 || verdict.Gates[0].State != Fail ||
		!strings.Contains(verdict.Gates[0].Summary, "without required evidence marker") {
		t.Fatalf("code=%d gate=%#v", code, verdict.Gates[0])
	}
}

func TestMalformedFixtureFailsClosed(t *testing.T) {
	root := t.TempDir()
	fixtures := filepath.Join(root, "test", "conformance", "fixtures")
	if err := os.MkdirAll(fixtures, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, role := range Roles {
		fixture := Fixture{
			SchemaVersion:    SchemaVersion,
			Role:             role,
			Adapter:          adapters[role].Name,
			AccumuloVersion:  AccumuloVersion,
			AccumuloRevision: AccumuloRevision,
			Summary:          "fixture",
			Evidence: []Evidence{{
				Gate:          adapters[role].RequiredGates[0],
				Kind:          "go-test",
				Reference:     "missing_test.go#TestMissing",
				SourceSHA256:  strings.Repeat("0", 64),
				ReplayCommand: []string{"go", "test"},
			}},
		}
		data, _ := json.Marshal(fixture)
		if err := os.WriteFile(filepath.Join(fixtures, role+".json"), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	verdict, code, err := Run(context.Background(), Options{
		Root:     root,
		Mode:     "replay",
		Commit:   strings.Repeat("d", 40),
		Executor: fakeExecutor{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if code != 1 || verdict.Gates[0].State != Fail {
		t.Fatalf("code=%d gate=%#v", code, verdict.Gates[0])
	}
}

type goldenReplay struct {
	Name              string `json:"name"`
	SourceSHA256      string `json:"source_sha256"`
	CommandExit       int    `json:"command_exit"`
	WantEvidenceState State  `json:"want_evidence_state"`
	WantState         State  `json:"want_state"`
	WantExit          int    `json:"want_exit"`
}

func TestAdapterGoldenReplayContracts(t *testing.T) {
	for _, name := range []string{"success", "failure", "stale"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(repositoryRoot(t), "test", "conformance", "golden", name+".json"))
			if err != nil {
				t.Fatal(err)
			}

			var golden goldenReplay
			if err := json.Unmarshal(data, &golden); err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "evidence.go"), []byte("package fixture\nfunc TestEvidence() {}\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			fixtures := filepath.Join(root, "test", "conformance", "fixtures")
			if err := os.MkdirAll(fixtures, 0o755); err != nil {
				t.Fatal(err)
			}
			command := []string{"go", "test", "./fixture"}
			for _, role := range Roles {
				spec := adapters[role]
				fixture := Fixture{
					SchemaVersion:    SchemaVersion,
					Role:             role,
					Adapter:          spec.Name,
					AccumuloVersion:  AccumuloVersion,
					AccumuloRevision: AccumuloRevision,
					Summary:          "golden " + golden.Name,
					Evidence: []Evidence{{
						Gate:          spec.RequiredGates[0],
						Kind:          "go-test",
						Reference:     "evidence.go#TestEvidence",
						SourceSHA256:  golden.SourceSHA256,
						ReplayCommand: command,
					}},
				}
				encoded, err := json.Marshal(fixture)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(fixtures, role+".json"), encoded, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			verdict, code, err := Run(context.Background(), Options{
				Root:     root,
				Mode:     "replay",
				Required: []string{"promotion"},
				Commit:   strings.Repeat("e", 40),
				Executor: fakeExecutor{results: map[string]CommandResult{
					strings.Join(command, "\x00"): {ExitCode: golden.CommandExit, Output: golden.Name},
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			gate := verdict.Gates[3]
			if code != golden.WantExit || gate.State != golden.WantState ||
				gate.EvidenceState != golden.WantEvidenceState {
				t.Fatalf("code=%d gate=%#v, want exit=%d state=%s evidence=%s",
					code, gate, golden.WantExit, golden.WantState, golden.WantEvidenceState)
			}
		})
	}
}

func TestEvidenceDigestIsStableAcrossLineEndings(t *testing.T) {
	lf := sha256.Sum256(normalizeSource([]byte("one\ntwo\n")))
	crlf := sha256.Sum256(normalizeSource([]byte("one\r\ntwo\r\n")))
	if lf != crlf {
		t.Fatalf("source digest differs across line endings: %x != %x", lf, crlf)
	}
}
