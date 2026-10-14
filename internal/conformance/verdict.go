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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

const (
	SchemaVersion    = 3
	AccumuloVersion  = "4.0.0-SNAPSHOT"
	AccumuloRevision = "1a716b2c1bb5762ead4b46d2bc4f53e13873b314"
)

var Roles = []string{"tserver", "scanserver", "compactor", "promotion", "client"}

type State string

const (
	Pass        State = "pass"
	Fail        State = "fail"
	Unsupported State = "unsupported"
	Skipped     State = "skipped"
)

type Environment struct {
	Mode   string `json:"mode"`
	GOOS   string `json:"goos"`
	GOARCH string `json:"goarch"`
	Docker string `json:"docker"`
}

type Metadata struct {
	ShoalCommit      string      `json:"shoal_commit"`
	AccumuloVersion  string      `json:"accumulo_version"`
	AccumuloRevision string      `json:"accumulo_revision"`
	Environment      Environment `json:"environment"`
}

type Evidence struct {
	Gate          string   `json:"gate"`
	Kind          string   `json:"kind"`
	Reference     string   `json:"reference"`
	SourceSHA256  string   `json:"source_sha256"`
	ReplayCommand []string `json:"replay_command,omitempty"`
}

type Gate struct {
	Role                 string     `json:"role"`
	Adapter              string     `json:"adapter"`
	Required             bool       `json:"required"`
	State                State      `json:"state"`
	EvidenceState        State      `json:"evidence_state"`
	Summary              string     `json:"summary"`
	MissingRequiredGates []string   `json:"missing_required_gates"`
	Evidence             []Evidence `json:"evidence"`
	ReplayFixture        string     `json:"replay_fixture"`
	FixtureSHA256        string     `json:"fixture_sha256"`
}

type Verdict struct {
	SchemaVersion int      `json:"schema_version"`
	Metadata      Metadata `json:"metadata"`
	Gates         []Gate   `json:"gates"`
}

type Fixture struct {
	SchemaVersion    int        `json:"schema_version"`
	Role             string     `json:"role"`
	Adapter          string     `json:"adapter"`
	AccumuloVersion  string     `json:"accumulo_version"`
	AccumuloRevision string     `json:"accumulo_revision"`
	Summary          string     `json:"summary"`
	Evidence         []Evidence `json:"evidence"`
}

type adapterSpec struct {
	Name          string
	RequiredGates []string
	LiveGate      string
}

var adapters = map[string]adapterSpec{
	"client": {
		Name:          "client-contract-v2",
		RequiredGates: []string{"crud", "visibility", "range", "shoal-java-write-flush-scan"},
		LiveGate:      "shoal-java-write-flush-scan",
	},
	"promotion": {
		Name:          "promotion-equivalence-v2",
		RequiredGates: []string{"before-after-equivalence", "java-readability", "live-destination-equivalence"},
		LiveGate:      "live-destination-equivalence",
	},
	"scanserver": {
		Name:          "stateful-scan-continuation-v2",
		RequiredGates: []string{"stateful-continuation", "cancel-resume", "live-shoal-continuation"},
		LiveGate:      "live-shoal-continuation",
	},
	"tserver": {
		Name: "tserver-production-v2",
		RequiredGates: []string{
			"service-lock-lifecycle", "assignment-lifecycle", "ingest-minor-compaction",
			"wal-recovery", "restart-fencing", "live-shoal-tserver",
		},
		LiveGate: "live-shoal-tserver",
	},
	"compactor": {
		Name: "compactor-production-v2",
		RequiredGates: []string{
			"publication", "completion", "cancellation", "restart-reconciliation",
			"fenced-input", "live-shoal-compactor",
		},
		LiveGate: "live-shoal-compactor",
	},
}

type CommandResult struct {
	ExitCode int
	Output   string
}

type Executor interface {
	Run(context.Context, string, []string) CommandResult
}

type OSExecutor struct{}

func (OSExecutor) Run(ctx context.Context, root string, command []string) CommandResult {
	if len(command) == 0 {
		return CommandResult{ExitCode: 1, Output: "empty replay command"}
	}
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Dir = root
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := cmd.Run()
	if err == nil {
		return CommandResult{Output: output.String()}
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return CommandResult{ExitCode: exitErr.ExitCode(), Output: output.String()}
	}
	return CommandResult{ExitCode: 1, Output: err.Error()}
}

type Options struct {
	Root     string
	Mode     string
	Required []string
	Commit   string
	Executor Executor
}

func Run(ctx context.Context, opts Options) (Verdict, int, error) {
	if opts.Root == "" {
		return Verdict{}, 1, errors.New("repository root is required")
	}
	if opts.Mode != "replay" && opts.Mode != "live" {
		return Verdict{}, 1, fmt.Errorf("unsupported mode %q", opts.Mode)
	}
	if opts.Executor == nil {
		opts.Executor = OSExecutor{}
	}
	required, err := requiredRoles(opts.Required)
	if err != nil {
		return Verdict{}, 1, err
	}
	commit := opts.Commit
	if commit == "" {
		result := opts.Executor.Run(ctx, opts.Root, []string{"git", "rev-parse", "HEAD"})
		if result.ExitCode != 0 {
			return Verdict{}, 1, fmt.Errorf("resolve Shoal commit: %s", strings.TrimSpace(result.Output))
		}
		commit = strings.TrimSpace(result.Output)
	}

	verdict := Verdict{
		SchemaVersion: SchemaVersion,
		Metadata: Metadata{
			ShoalCommit:      commit,
			AccumuloVersion:  AccumuloVersion,
			AccumuloRevision: AccumuloRevision,
			Environment: Environment{
				Mode:   opts.Mode,
				GOOS:   runtime.GOOS,
				GOARCH: runtime.GOARCH,
				Docker: "not_checked",
			},
		},
	}

	for _, role := range Roles {
		fixturePath := filepath.Join("test", "conformance", "fixtures", role+".json")
		gate, fixture, loadErr := loadFixture(opts.Root, fixturePath)
		gate.Required = required[role]
		if loadErr != nil {
			gate.Role = role
			gate.State = Fail
			gate.EvidenceState = Fail
			gate.Summary = loadErr.Error()
			verdict.Gates = append(verdict.Gates, gate)
			continue
		}
		if fixture.Role != role {
			gate.Role = role
			gate.State = Fail
			gate.EvidenceState = Fail
			gate.Summary = fmt.Sprintf("replay fixture declares role %q", fixture.Role)
			verdict.Gates = append(verdict.Gates, gate)
			continue
		}
		spec := adapters[role]
		if fixture.Adapter != spec.Name {
			gate.State = Fail
			gate.EvidenceState = Fail
			gate.Summary = fmt.Sprintf("replay fixture declares adapter %q, want %q", fixture.Adapter, spec.Name)
			verdict.Gates = append(verdict.Gates, gate)
			continue
		}
		if !gate.Required {
			gate.State = Skipped
			gate.EvidenceState = Skipped
			gate.Summary = "gate not selected as required"
		} else {
			runAdapter(ctx, opts, fixture, spec, &gate)
			if gate.EvidenceState == Pass && opts.Mode == "live" {
				runLive(ctx, opts, spec, &verdict, &gate)
			}
		}
		verdict.Gates = append(verdict.Gates, gate)
	}
	return verdict, ExitCode(verdict), nil
}

func requiredRoles(names []string) (map[string]bool, error) {
	if len(names) == 0 {
		names = Roles
	}
	result := make(map[string]bool, len(names))
	for _, name := range names {
		if !slices.Contains(Roles, name) {
			return nil, fmt.Errorf("unknown required role %q", name)
		}
		result[name] = true
	}
	return result, nil
}

func loadFixture(root, relative string) (Gate, Fixture, error) {
	path := filepath.Join(root, relative)
	data, err := os.ReadFile(path)
	gate := Gate{ReplayFixture: filepath.ToSlash(relative)}
	if err != nil {
		return gate, Fixture{}, fmt.Errorf("read replay fixture %s: %w", relative, err)
	}
	sum := sha256.Sum256(data)
	gate.FixtureSHA256 = hex.EncodeToString(sum[:])
	var fixture Fixture
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fixture); err != nil {
		return gate, Fixture{}, fmt.Errorf("decode replay fixture %s: %w", relative, err)
	}
	if fixture.SchemaVersion != SchemaVersion {
		return gate, Fixture{}, fmt.Errorf("replay fixture %s has schema version %d", relative, fixture.SchemaVersion)
	}
	if fixture.AccumuloVersion != AccumuloVersion || fixture.AccumuloRevision != AccumuloRevision {
		return gate, Fixture{}, fmt.Errorf("replay fixture %s targets a different Accumulo build", relative)
	}
	if fixture.Role == "" || fixture.Adapter == "" || fixture.Summary == "" || len(fixture.Evidence) == 0 {
		return gate, Fixture{}, fmt.Errorf("replay fixture %s is incomplete", relative)
	}
	for _, evidence := range fixture.Evidence {
		if evidence.Gate == "" || evidence.Kind == "" || evidence.Reference == "" ||
			evidence.SourceSHA256 == "" || len(evidence.ReplayCommand) == 0 {
			return gate, Fixture{}, fmt.Errorf("replay fixture %s contains incomplete evidence", relative)
		}
		referencePath := strings.SplitN(evidence.Reference, "#", 2)[0]
		if filepath.IsAbs(referencePath) || strings.HasPrefix(filepath.Clean(referencePath), "..") {
			return gate, Fixture{}, fmt.Errorf("replay fixture %s contains a non-repository evidence path", relative)
		}
		content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(referencePath)))
		if err != nil {
			return gate, Fixture{}, fmt.Errorf("read evidence %s: %w", evidence.Reference, err)
		}
		if fragment := strings.SplitN(evidence.Reference, "#", 2); len(fragment) == 2 &&
			!bytes.Contains(content, []byte(fragment[1])) {
			return gate, Fixture{}, fmt.Errorf("evidence symbol %s is absent", evidence.Reference)
		}
		sum := sha256.Sum256(normalizeSource(content))
		if actual := hex.EncodeToString(sum[:]); actual != evidence.SourceSHA256 {
			return gate, Fixture{}, fmt.Errorf(
				"stale evidence %s: source sha256 %s, fixture has %s",
				evidence.Reference, actual, evidence.SourceSHA256,
			)
		}
	}
	gate.Role = fixture.Role
	gate.Adapter = fixture.Adapter
	gate.Summary = fixture.Summary
	gate.Evidence = fixture.Evidence
	return gate, fixture, nil
}

func runAdapter(ctx context.Context, opts Options, fixture Fixture, spec adapterSpec, gate *Gate) {
	covered := make(map[string]bool, len(fixture.Evidence))
	for _, evidence := range fixture.Evidence {
		if !slices.Contains(spec.RequiredGates, evidence.Gate) {
			gate.State = Fail
			gate.EvidenceState = Fail
			gate.Summary = fmt.Sprintf("adapter %s does not declare evidence gate %q", spec.Name, evidence.Gate)
			return
		}
		covered[evidence.Gate] = true
		result := opts.Executor.Run(ctx, opts.Root, evidence.ReplayCommand)
		if result.ExitCode != 0 {
			gate.State = Fail
			gate.EvidenceState = Fail
			gate.Summary = fmt.Sprintf(
				"replay failed for %s (exit %d): %s",
				evidence.Reference,
				result.ExitCode,
				strings.TrimSpace(result.Output),
			)
			return
		}
	}
	gate.EvidenceState = Pass
	gate.MissingRequiredGates = missingGates(spec.RequiredGates, covered)
	if len(gate.MissingRequiredGates) != 0 {
		gate.State = Unsupported
		gate.Summary = "adapter evidence passed; required production gates are not wired"
		return
	}
	gate.State = Pass
}

func missingGates(required []string, covered map[string]bool) []string {
	var missing []string
	for _, name := range required {
		if !covered[name] {
			missing = append(missing, name)
		}
	}
	return missing
}

func runLive(ctx context.Context, opts Options, spec adapterSpec, verdict *Verdict, gate *Gate) {
	command := []string{"python", "test/accumulo/harness.py", "role", gate.Role}
	harnessSHA, err := sourceSHA256(opts.Root, "test/accumulo/harness.py")
	if err != nil {
		gate.State = Fail
		gate.Summary = err.Error()
		return
	}
	result := opts.Executor.Run(ctx, opts.Root, command)
	switch result.ExitCode {
	case 0:
		marker := "SHOAL_EVIDENCE role=" + gate.Role + " status=pass"
		if !strings.Contains(result.Output, marker) {
			gate.State = Fail
			gate.Summary = "live role harness exited zero without required evidence marker " + marker
			verdict.Metadata.Environment.Docker = "available"
			break
		}
		gate.MissingRequiredGates = slices.DeleteFunc(gate.MissingRequiredGates, func(name string) bool {
			return name == spec.LiveGate
		})
		if len(gate.MissingRequiredGates) == 0 {
			gate.State = Pass
			gate.Summary = "adapter evidence and pinned Shoal live role gate passed"
		} else {
			gate.State = Unsupported
			gate.Summary = "live role gate passed; required adapter gates remain unwired"
		}
		verdict.Metadata.Environment.Docker = "available"
	case 2:
		gate.State = Unsupported
		gate.Summary = "live Docker environment unavailable: " + strings.TrimSpace(result.Output)
		verdict.Metadata.Environment.Docker = "unavailable"
	default:
		gate.State = Fail
		gate.Summary = fmt.Sprintf("live Accumulo Shoal role harness failed (exit %d): %s", result.ExitCode, strings.TrimSpace(result.Output))
		verdict.Metadata.Environment.Docker = "available"
	}
	gate.Evidence = append(gate.Evidence, Evidence{
		Gate:          spec.LiveGate,
		Kind:          "live-command",
		Reference:     "test/accumulo/harness.py#role_run",
		SourceSHA256:  harnessSHA,
		ReplayCommand: command,
	})
}

func sourceSHA256(root, relative string) (string, error) {
	content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
	if err != nil {
		return "", fmt.Errorf("read evidence %s: %w", relative, err)
	}
	sum := sha256.Sum256(normalizeSource(content))
	return hex.EncodeToString(sum[:]), nil
}

func normalizeSource(content []byte) []byte {
	return bytes.ReplaceAll(content, []byte("\r\n"), []byte("\n"))
}

func ExitCode(verdict Verdict) int {
	incomplete := false
	for _, gate := range verdict.Gates {
		if !gate.Required {
			continue
		}
		if gate.State == Fail {
			return 1
		}
		if gate.State != Pass {
			incomplete = true
		}
	}
	if incomplete {
		return 2
	}
	return 0
}

func Marshal(verdict Verdict) ([]byte, error) {
	data, err := json.MarshalIndent(verdict, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}
