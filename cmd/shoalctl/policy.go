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
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/phrocker/shoal-oss/pkg/atpl"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Policy commands compile ATPL policy files (pkg/atpl) and reconcile them
// with a live fleet registry over its HTTP routes
// (pkg/explorer/webapi/fleet_registry.go). See docs/atpl.md.

const (
	// applyReasonCode is recorded on every registration apply makes. The
	// policy digest is recorded beside it as the reason detail, so the
	// lifecycle record names the exact policy that produced each generation:
	// a hosted registry keeps it as the receipt's caller-asserted reason
	// source, attributed to the authenticated caller, and refuses any detail
	// that is not a policy digest.
	applyReasonCode = fleet.ReasonCodeATPLApply
	// maxTokenBytes bounds the bearer token file.
	maxTokenBytes = 64 << 10
	// maxResponseBytes bounds one registry response: a full list page of
	// maximum-size descriptors with room for encoding overhead.
	maxResponseBytes = 2 * fleet.MaxListResults * fleet.MaxDescriptorBytes
	// maxListPages bounds how many pages one read of the registry may take.
	// The registry may return an empty page with a continuation when its scan
	// budget runs out, so this is larger than MaxAgents/MaxListResults.
	maxListPages = 4096
)

const policyUsage = "expected policy command: compile, plan, apply, or export"

func runPolicy(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New(policyUsage)
	}
	switch args[0] {
	case "compile":
		return runPolicyCompile(args[1:], stdout, stderr)
	case "plan":
		return runPolicyPlan(args[1:], stdout, stderr)
	case "apply":
		return runPolicyApply(args[1:], stdout, stderr)
	case "export":
		return runPolicyExport(args[1:], stdout, stderr)
	default:
		return fmt.Errorf("unknown policy command %q; %s", args[0], strings.TrimPrefix(policyUsage, "expected policy command: "))
	}
}

// parseWithDirectory accepts the policy directory before or after the flags,
// since the flag package stops at the first positional argument.
func parseWithDirectory(flags *flag.FlagSet, args []string) (string, error) {
	var directory string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		directory, args = args[0], args[1:]
	}
	if err := flags.Parse(args); err != nil {
		return "", err
	}
	if directory == "" && flags.NArg() > 0 {
		directory = flags.Arg(0)
		if err := flags.Parse(flags.Args()[1:]); err != nil {
			return "", err
		}
	}
	if flags.NArg() != 0 {
		return "", fmt.Errorf("policy %s accepts one policy directory", flags.Name())
	}
	if directory == "" {
		return "", fmt.Errorf("policy %s requires a policy directory", flags.Name())
	}
	return directory, nil
}

func runPolicyCompile(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("compile", flag.ContinueOnError)
	flags.SetOutput(stderr)
	directory, err := parseWithDirectory(flags, args)
	if err != nil {
		return err
	}
	documents, err := loadPolicyDirectory(directory)
	if err != nil {
		return err
	}
	policy, err := atpl.Compile(documents, time.Now(), nil)
	if err != nil {
		return err
	}
	var indented bytes.Buffer
	if err := json.Indent(&indented, policy.CanonicalJSON(), "", "  "); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "policy digest: %s\n", policy.Digest())
	fmt.Fprintln(stdout, indented.String())
	return nil
}

type registryFlags struct {
	endpoint  *string
	tokenFile *string
	timeout   *time.Duration
}

func addRegistryFlags(flags *flag.FlagSet) registryFlags {
	return registryFlags{
		endpoint:  flags.String("endpoint", "", "fleet registry base URL (https, or http on loopback)"),
		tokenFile: flags.String("token-file", "", "file holding the bearer token"),
		timeout:   flags.Duration("timeout", time.Minute, "bound on the whole command"),
	}
}

func (f registryFlags) client() (*fleetClient, error) {
	if *f.endpoint == "" {
		return nil, errors.New("-endpoint is required")
	}
	if *f.tokenFile == "" {
		return nil, errors.New("-token-file is required")
	}
	if *f.timeout <= 0 {
		return nil, errors.New("-timeout must be positive")
	}
	return newFleetClient(*f.endpoint, *f.tokenFile)
}

// planAgainstLive compiles a directory against what the registry shows now
// and diffs the two. plan and apply both call it, so apply writes only what
// the same computation over the same inputs reviewed.
func planAgainstLive(
	ctx context.Context, client *fleetClient, directory string,
) (*atpl.Policy, atpl.Plan, error) {
	documents, err := loadPolicyDirectory(directory)
	if err != nil {
		return nil, atpl.Plan{}, err
	}
	live, err := client.list(ctx)
	if err != nil {
		return nil, atpl.Plan{}, err
	}
	policy, err := atpl.Compile(documents, time.Now(), atpl.LiveParents(live))
	if err != nil {
		return nil, atpl.Plan{}, err
	}
	return policy, atpl.Diff(policy, live, client.registry), nil
}

func runPolicyPlan(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("plan", flag.ContinueOnError)
	flags.SetOutput(stderr)
	registry := addRegistryFlags(flags)
	directory, err := parseWithDirectory(flags, args)
	if err != nil {
		return err
	}
	client, err := registry.client()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), *registry.timeout)
	defer cancel()
	_, plan, err := planAgainstLive(ctx, client, directory)
	if err != nil {
		return err
	}
	printPolicyPlan(stdout, plan)
	if refusals := plan.Refusals(); len(refusals) > 0 {
		return fmt.Errorf("plan has %d refused agent(s); apply would refuse it", len(refusals))
	}
	return nil
}

func runPolicyApply(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("apply", flag.ContinueOnError)
	flags.SetOutput(stderr)
	registry := addRegistryFlags(flags)
	reviewed := flags.String("plan-digest", "", "digest printed by policy plan; apply refuses any other plan")
	directory, err := parseWithDirectory(flags, args)
	if err != nil {
		return err
	}
	if *reviewed == "" {
		return errors.New("-plan-digest is required; run policy plan and review it first")
	}
	client, err := registry.client()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), *registry.timeout)
	defer cancel()
	policy, plan, err := planAgainstLive(ctx, client, directory)
	if err != nil {
		return err
	}
	if plan.Digest != *reviewed {
		printPolicyPlan(stdout, plan)
		return fmt.Errorf("plan digest mismatch: reviewed %s, current %s; "+
			"the policy or the registry changed since review, so re-run policy plan", *reviewed, plan.Digest)
	}
	if refusals := plan.Refusals(); len(refusals) > 0 {
		printPolicyPlan(stdout, plan)
		return fmt.Errorf("plan has %d refused agent(s); nothing was applied", len(refusals))
	}
	writes := plan.Writes()
	fmt.Fprintf(stdout, "policy digest: %s\n", policy.Digest())
	written := make(map[shoal.ID]int64, len(writes))
	// stopped describes where apply stopped. A child whose first, clamped step
	// landed but whose second did not keeps its parent's old lease, and a
	// re-plan does not compare leases, so it shows that child unchanged:
	// nothing raises it to its full TTL until a heartbeat or a later write.
	stopped := func(done int) string {
		message := fmt.Sprintf("stopped after %d of %d writes", done, len(writes))
		finished := make(map[shoal.ID]bool)
		for _, earlier := range writes[:done] {
			if earlier.Steps > 1 && earlier.Step == earlier.Steps {
				finished[earlier.ID] = true
			}
		}
		for _, earlier := range writes[:done] {
			if earlier.Steps > 1 && earlier.Step == 1 && !finished[earlier.ID] {
				message += fmt.Sprintf("; %s holds its clamped lease until %s, not its full lease_ttl, "+
					"and a re-plan will show it unchanged, so only a heartbeat or a later write raises it",
					agentLabel(earlier.ID), earlier.Spec.LeaseExpiresAt.Format(time.RFC3339))
			}
		}
		return message
	}
	for i, write := range writes {
		entry := write.Entry
		step := ""
		if write.Steps > 1 {
			step = fmt.Sprintf(" (step %d of %d)", write.Step, write.Steps)
			if write.Step > 1 {
				// A later step expects the generation the earlier one wrote.
				entry.LiveGeneration = written[entry.ID]
			}
		}
		descriptor, err := client.register(ctx, policy.Digest(), entry)
		var refused *registryError
		if err != nil && entry.Kind.Updates() && errors.As(err, &refused) &&
			refused.code == string(shoal.ErrorConflict) {
			// The generation moved between the read and this write. A
			// heartbeat does that without changing anything the policy
			// governs, so re-read, and retry once only if the content the plan
			// was reviewed against is still what is live.
			current, readErr := client.resolve(ctx, entry.ID)
			if readErr != nil {
				return fmt.Errorf("%s%s: %w; re-reading after it: %v; %s",
					agentLabel(entry.ID), step, err, readErr, stopped(i))
			}
			if atpl.ContentDigest(current) != entry.LiveContent {
				return fmt.Errorf("%s%s: %w, and its live content changed since the plan; "+
					"re-run policy plan; %s",
					agentLabel(entry.ID), step, err, stopped(i))
			}
			fmt.Fprintf(stdout, "%s%s: generation moved from %d to %d with no change to its content; retrying once\n",
				agentLabel(entry.ID), step, entry.LiveGeneration, current.Generation)
			entry.LiveGeneration = current.Generation
			descriptor, err = client.register(ctx, policy.Digest(), entry)
		}
		if err != nil {
			return fmt.Errorf("%s%s: %w; %s", agentLabel(entry.ID), step, err, stopped(i))
		}
		written[entry.ID] = descriptor.Generation
		fmt.Fprintf(stdout, "%s %s %s%s: generation %d\n",
			entry.Kind.Symbol(), agentLabel(entry.ID), entry.Kind, step, descriptor.Generation)
	}
	fmt.Fprintf(stdout, "applied %d of %d writes\n", len(writes), len(writes))
	return nil
}

func runPolicyExport(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("export", flag.ContinueOnError)
	flags.SetOutput(stderr)
	registry := addRegistryFlags(flags)
	out := flags.String("out", "", "directory to write policy files into; must hold no policy files")
	manifest := flags.String("executors", "", "policy file declaring the host's executors")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("policy export accepts no positional arguments")
	}
	if *out == "" {
		return errors.New("-out is required")
	}
	var executors []atpl.Executor
	if *manifest != "" {
		document, err := decodePolicyFile(*manifest, filepath.Base(*manifest))
		if err != nil {
			return err
		}
		if len(document.Agents) != 0 {
			return fmt.Errorf("%s: an executor manifest declares executors only", *manifest)
		}
		executors = append([]atpl.Executor{}, document.Executors...)
	}
	client, err := registry.client()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), *registry.timeout)
	defer cancel()
	live, err := client.list(ctx)
	if err != nil {
		return err
	}
	document, err := atpl.Export(live, executors, time.Now())
	if err != nil {
		return err
	}
	if executors == nil {
		fmt.Fprintln(stderr, "warning: no -executors manifest; each executor's max_effects is the "+
			"union of its live actions' effects and min_effects is empty. That is the narrowest "+
			"assertion the live registrations satisfy, not what the host binds.")
	}
	files, err := exportFiles(document, atpl.MaxPolicyBytes)
	if err != nil {
		return err
	}
	if err := writeExport(*out, files); err != nil {
		return err
	}
	decoded := make([]atpl.Document, 0, len(files))
	for _, file := range files {
		reread, err := atpl.Decode(file.name, bytes.NewReader(file.data))
		if err != nil {
			return fmt.Errorf("exported file does not decode: %w", err)
		}
		decoded = append(decoded, reread)
	}
	fmt.Fprintf(stdout, "wrote %d files to %s\n", len(files), *out)
	if policy, err := atpl.Compile(decoded, time.Now(), nil); err != nil {
		fmt.Fprintf(stderr, "warning: the export does not compile offline: %v\n", err)
	} else {
		fmt.Fprintf(stdout, "policy digest: %s\n", policy.Digest())
	}
	return nil
}

type exportFile struct {
	name string
	data []byte
}

var plainFileID = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,99}$`)

// agentFileName derives a stable file name from an agent ID, so a later export
// diffs against an earlier one file by file. IDs that are not short,
// lower-case and filesystem-safe are named by their hash instead, which also
// keeps two IDs that differ only in case apart on case-insensitive
// filesystems.
func agentFileName(id string) string {
	if plainFileID.MatchString(id) {
		return "agent." + id + atpl.FileSuffix
	}
	sum := sha256.Sum256([]byte(id))
	return "agent-sha256." + hex.EncodeToString(sum[:16]) + atpl.FileSuffix
}

func exportFiles(document atpl.Document, maxTotal int64) ([]exportFile, error) {
	encode := func(name string, value atpl.Document) (exportFile, error) {
		data, err := atpl.Encode(value)
		if err != nil {
			return exportFile{}, err
		}
		if len(data) > atpl.MaxFileBytes {
			return exportFile{}, fmt.Errorf("%s: exported file exceeds %d bytes", name, atpl.MaxFileBytes)
		}
		return exportFile{name: name, data: data}, nil
	}
	if len(document.Agents)+1 > atpl.MaxFiles {
		return nil, fmt.Errorf("export needs %d files, more than the %d one policy may span",
			len(document.Agents)+1, atpl.MaxFiles)
	}
	executors, err := encode("executors"+atpl.FileSuffix,
		atpl.Document{ATPL: document.ATPL, Origin: document.Origin, Executors: document.Executors})
	if err != nil {
		return nil, err
	}
	files := []exportFile{executors}
	total := int64(len(executors.data))
	for _, agent := range document.Agents {
		file, err := encode(agentFileName(agent.ID),
			atpl.Document{ATPL: document.ATPL, Origin: document.Origin, Agents: []atpl.Agent{agent}})
		if err != nil {
			return nil, err
		}
		// Checked before anything is written: a directory over the bound
		// would be refused by the very plan it was exported for.
		if total += int64(len(file.data)); total > maxTotal {
			return nil, fmt.Errorf("export needs more than %d bytes, more than one policy directory may hold",
				maxTotal)
		}
		files = append(files, file)
	}
	return files, nil
}

func writeExport(directory string, files []exportFile) error {
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	// Read the directory rather than globbing it: a directory name holding
	// glob metacharacters would make a glob match nothing and the guard pass.
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), atpl.FileSuffix) {
			return fmt.Errorf("%s already holds policy files; export into an empty directory", directory)
		}
	}
	for _, file := range files {
		handle, err := os.OpenFile(filepath.Join(directory, file.name),
			os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return err
		}
		if _, err := handle.Write(file.data); err != nil {
			_ = handle.Close()
			return err
		}
		if err := handle.Close(); err != nil {
			return err
		}
	}
	return nil
}

// loadPolicyDirectory reads every *.atpl.json file directly in directory.
// Symbolic links and other non-regular files are refused rather than
// followed, so the files reviewed are the files compiled.
func loadPolicyDirectory(directory string) ([]atpl.Document, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	var documents []atpl.Document
	var total int64
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), atpl.FileSuffix) {
			continue
		}
		if !entry.Type().IsRegular() {
			return nil, fmt.Errorf("%s: policy files must be regular files", entry.Name())
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if total += info.Size(); total > atpl.MaxPolicyBytes {
			return nil, fmt.Errorf("%s holds more than %d bytes of policy files", directory, atpl.MaxPolicyBytes)
		}
		if len(documents) == atpl.MaxFiles {
			return nil, fmt.Errorf("%s holds more than %d policy files", directory, atpl.MaxFiles)
		}
		document, err := decodePolicyFile(filepath.Join(directory, entry.Name()), entry.Name())
		if err != nil {
			return nil, err
		}
		documents = append(documents, document)
	}
	if len(documents) == 0 {
		return nil, fmt.Errorf("%s holds no *%s files", directory, atpl.FileSuffix)
	}
	return documents, nil
}

func decodePolicyFile(path, name string) (atpl.Document, error) {
	handle, err := os.Open(path)
	if err != nil {
		return atpl.Document{}, err
	}
	defer handle.Close()
	return atpl.Decode(name, handle)
}

func agentLabel(id shoal.ID) string {
	if text := string(id); isPrintablePathValue(text) {
		return "agents[id=" + text + "]"
	}
	return "agents[id=" + strconv.Quote(string(id)) + "]"
}

func isPrintablePathValue(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z') && !(character >= 'A' && character <= 'Z') &&
			!(character >= '0' && character <= '9') && character != '_' && character != '-' && character != ':' {
			return false
		}
	}
	return true
}

func printPolicyPlan(output io.Writer, plan atpl.Plan) {
	fmt.Fprintf(output, "policy digest: %s\n", plan.PolicyDigest)
	fmt.Fprintf(output, "registry: %s\n", plan.Registry)
	counts := map[string]int{}
	for _, entry := range plan.Entries {
		line := fmt.Sprintf("%s %s %s", entry.Kind.Symbol(), agentLabel(entry.ID), entry.Kind)
		if entry.Kind != atpl.KindCreate {
			line += fmt.Sprintf(" (live generation %d)", entry.LiveGeneration)
		}
		if entry.Reason != "" {
			line += ": " + entry.Reason
		}
		fmt.Fprintln(output, line)
		for _, change := range entry.Changes {
			detail := ""
			if change.Detail != "" {
				detail = ": " + change.Detail
			}
			fmt.Fprintf(output, "    %s %s%s\n", change.Op, change.Path, detail)
		}
		switch {
		case entry.Kind.Refused():
			counts["refused"]++
		default:
			counts[string(entry.Kind)]++
		}
	}
	fmt.Fprintf(output, "summary: %d create, %d narrow, %d executor-change, %d unchanged, %d refused, %d unmanaged\n",
		counts[string(atpl.KindCreate)], counts[string(atpl.KindNarrow)],
		counts[string(atpl.KindExecutorChange)], counts[string(atpl.KindUnchanged)],
		counts["refused"], counts[string(atpl.KindUnmanaged)])
	if writes := plan.Writes(); len(writes) > 0 {
		fmt.Fprintln(output, "apply order:")
		for i, write := range writes {
			step := ""
			if write.Steps > 1 {
				step = fmt.Sprintf(" (step %d of %d", write.Step, write.Steps)
				if write.Step < write.Steps {
					step += ", lease clamped to " + write.Spec.LeaseExpiresAt.Format(time.RFC3339)
				}
				step += ")"
			}
			fmt.Fprintf(output, "  %d. %s %s %s%s\n", i+1, write.Kind.Symbol(), agentLabel(write.ID), write.Kind, step)
		}
	}
	fmt.Fprintf(output, "plan digest: %s\n", plan.Digest)
}

// registrationKey derives the idempotency key for one apply write from the
// policy, the agent, the generation it expects and the lease it sets. A retry
// of the same write replays; any different write is a different key.
func registrationKey(policyDigest string, spec fleet.Spec, expectedGeneration int64) shoal.ID {
	digest := sha256.New()
	field := func(value []byte) {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		_, _ = digest.Write(size[:])
		_, _ = digest.Write(value)
	}
	var number [8]byte
	field([]byte("shoal.atpl.apply.v1"))
	field([]byte(policyDigest))
	field([]byte(spec.ID))
	binary.BigEndian.PutUint64(number[:], uint64(expectedGeneration))
	field(number[:])
	binary.BigEndian.PutUint64(number[:], uint64(spec.LeaseExpiresAt.UnixNano()))
	field(number[:])
	return shoal.ID("atpl-apply:" + hex.EncodeToString(digest.Sum(nil)))
}

// Wire structs mirror the registry's HTTP contract
// (pkg/explorer/webapi/fleet_registry.go). IDs travel as unpadded base64url;
// byte fields as standard base64, which encoding/json applies to []byte.

type fleetContextWire struct {
	RequestID    string    `json:"request_id"`
	ReasonCode   string    `json:"reason_code"`
	ReasonDetail string    `json:"reason_detail,omitempty"`
	Deadline     time.Time `json:"deadline"`
}

type fleetSpecWire struct {
	ID                  string             `json:"id"`
	ParentID            string             `json:"parent_id,omitempty"`
	AuthorizationDomain []byte             `json:"authorization_domain"`
	Scopes              []fleet.Scope      `json:"scopes"`
	ExecutorRef         string             `json:"executor_ref"`
	Capabilities        []fleet.Capability `json:"capabilities"`
	LeaseExpiresAt      time.Time          `json:"lease_expires_at"`
}

type fleetRegisterWire struct {
	Context            fleetContextWire `json:"context"`
	RegistrationKey    string           `json:"registration_key"`
	ExpectedGeneration int64            `json:"expected_generation"`
	Descriptor         fleetSpecWire    `json:"descriptor"`
}

type fleetListWire struct {
	Context fleetContextWire `json:"context"`
	Cursor  []byte           `json:"cursor,omitempty"`
	Limit   int              `json:"limit"`
}

type fleetDescriptorWire struct {
	ID                  string             `json:"id"`
	Generation          int64              `json:"generation"`
	Subject             string             `json:"subject"`
	Actor               string             `json:"actor"`
	ParentID            string             `json:"parent_id,omitempty"`
	AuthorizationDomain []byte             `json:"authorization_domain"`
	Scopes              []fleet.Scope      `json:"scopes"`
	ExecutorRef         string             `json:"executor_ref"`
	Capabilities        []fleet.Capability `json:"capabilities"`
	LeaseExpiresAt      time.Time          `json:"lease_expires_at"`
	UpdatedAt           time.Time          `json:"updated_at"`
	RevokedAt           time.Time          `json:"revoked_at,omitempty"`
}

type fleetPageWire struct {
	Agents []fleetDescriptorWire `json:"agents"`
	Next   []byte                `json:"next,omitempty"`
}

type fleetErrorWire struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func encodeWireID(id shoal.ID) string {
	if id == "" {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(id))
}

func decodeWireID(field, value string, required bool) (shoal.ID, error) {
	if value == "" {
		if required {
			return "", fmt.Errorf("registry response omitted %s", field)
		}
		return "", nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return "", fmt.Errorf("registry response %s is not unpadded base64url", field)
	}
	return shoal.ID(decoded), nil
}

func (w fleetDescriptorWire) decode() (fleet.Descriptor, error) {
	id, err := decodeWireID("agent ID", w.ID, true)
	if err != nil {
		return fleet.Descriptor{}, err
	}
	subject, err := decodeWireID("subject", w.Subject, false)
	if err != nil {
		return fleet.Descriptor{}, err
	}
	actor, err := decodeWireID("actor", w.Actor, false)
	if err != nil {
		return fleet.Descriptor{}, err
	}
	parent, err := decodeWireID("parent agent ID", w.ParentID, false)
	if err != nil {
		return fleet.Descriptor{}, err
	}
	return fleet.Descriptor{
		ID: id, Generation: w.Generation, Subject: subject, Actor: actor, ParentID: parent,
		AuthorizationDomain: w.AuthorizationDomain, Scopes: w.Scopes,
		ExecutorRef: w.ExecutorRef, Capabilities: w.Capabilities,
		LeaseExpiresAt: w.LeaseExpiresAt.UTC(), UpdatedAt: w.UpdatedAt.UTC(),
		RevokedAt: w.RevokedAt,
	}, nil
}

// fleetClient reads and writes the fleet registry with a bearer token.
type fleetClient struct {
	base string
	// registry is the normalized endpoint bound into plan digests.
	registry string
	token    string
	http     *http.Client
}

func newFleetClient(endpoint, tokenFile string) (*fleetClient, error) {
	parsed, err := url.Parse(endpoint)
	// ForceQuery catches a bare trailing "?", which leaves RawQuery empty but
	// would turn every route appended to the base into a query string.
	if err != nil || parsed.Host == "" || parsed.RawQuery != "" || parsed.ForceQuery ||
		parsed.Fragment != "" || parsed.RawFragment != "" || strings.Contains(endpoint, "#") ||
		parsed.User != nil || parsed.Opaque != "" {
		return nil, errors.New("-endpoint must be an absolute URL without credentials, query or fragment")
	}
	switch parsed.Scheme {
	case "https":
	case "http":
		// A bearer token sent in the clear is a credential handed to anyone
		// on the path. Loopback is the one place that path is the host.
		if !loopbackHost(parsed.Hostname()) {
			return nil, errors.New("-endpoint must use https unless it is a loopback address")
		}
	default:
		return nil, errors.New("-endpoint must use https")
	}
	handle, err := os.Open(tokenFile)
	if err != nil {
		return nil, err
	}
	defer handle.Close()
	raw, err := io.ReadAll(io.LimitReader(handle, maxTokenBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxTokenBytes {
		return nil, fmt.Errorf("token file exceeds %d bytes", maxTokenBytes)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return nil, errors.New("token file must hold one bearer token")
	}
	return &fleetClient{
		// Routes are appended to the normalized endpoint, built from its
		// parts, so nothing the operator typed beyond scheme, host, port and
		// path reaches a request.
		base: normalizeEndpoint(parsed), registry: normalizeEndpoint(parsed),
		token: token,
		http: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			// Following a redirect would resend the token to wherever it
			// points.
			return http.ErrUseLastResponse
		}},
	}, nil
}

// normalizeEndpoint spells one registry one way: lower-case scheme and host,
// default port dropped, no trailing slash. A plan is bound to this string, so
// it must not vary with how the operator typed the same URL.
func normalizeEndpoint(endpoint *url.URL) string {
	scheme := strings.ToLower(endpoint.Scheme)
	host := strings.ToLower(endpoint.Hostname())
	if strings.Contains(host, ":") {
		// Hostname() unescapes an IPv6 zone ("%25eth0" becomes "%eth0");
		// re-escape it, or every request URL built on this fails to parse.
		host = "[" + strings.ReplaceAll(host, "%", "%25") + "]"
	}
	if port := endpoint.Port(); port != "" &&
		!(scheme == "https" && port == "443") && !(scheme == "http" && port == "80") {
		host += ":" + port
	}
	return scheme + "://" + host + strings.TrimRight(endpoint.EscapedPath(), "/")
}

func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func newRequestID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return encodeWireID(shoal.ID("atpl-" + hex.EncodeToString(raw))), nil
}

func (c *fleetClient) context(reasonDetail string) (fleetContextWire, error) {
	requestID, err := newRequestID()
	if err != nil {
		return fleetContextWire{}, err
	}
	return fleetContextWire{
		RequestID: requestID, ReasonCode: applyReasonCode, ReasonDetail: reasonDetail,
		Deadline: time.Now().UTC().Add(30 * time.Second),
	}, nil
}

// list reads every registration the token can see, through the list route.
// The registry lists only active registrations: revoked and expired agents are
// invisible here, and can never be registered again under the same ID.
func (c *fleetClient) list(ctx context.Context) (map[shoal.ID]fleet.Descriptor, error) {
	result := make(map[shoal.ID]fleet.Descriptor)
	var cursor []byte
	seen := map[string]struct{}{}
	for page := 0; ; page++ {
		if page == maxListPages {
			return nil, fmt.Errorf("registry listing exceeds %d pages", maxListPages)
		}
		requestContext, err := c.context("")
		if err != nil {
			return nil, err
		}
		requestContext.ReasonCode = "atpl-read"
		var response fleetPageWire
		if err := c.post(ctx, "/api/v1/fleet/agents/resolve", fleetListWire{
			Context: requestContext, Cursor: cursor, Limit: fleet.MaxListResults,
		}, http.StatusOK, &response); err != nil {
			return nil, fmt.Errorf("list registry: %w", err)
		}
		for _, wire := range response.Agents {
			descriptor, err := wire.decode()
			if err != nil {
				return nil, err
			}
			if _, duplicate := result[descriptor.ID]; duplicate {
				return nil, fmt.Errorf("registry listed %s twice", agentLabel(descriptor.ID))
			}
			if len(result) == atpl.MaxAgents {
				return nil, fmt.Errorf("registry lists more than %d agents", atpl.MaxAgents)
			}
			result[descriptor.ID] = descriptor
		}
		if len(response.Next) == 0 {
			return result, nil
		}
		if _, repeated := seen[string(response.Next)]; repeated {
			return nil, errors.New("registry listing cursor did not advance")
		}
		seen[string(response.Next)] = struct{}{}
		cursor = response.Next
	}
}

// register writes one plan entry, expecting the live generation the plan was
// computed against, so a registration that moved since is refused by the
// registry's compare-and-swap rather than overwritten.
func (c *fleetClient) register(ctx context.Context, policyDigest string, entry atpl.Entry) (fleet.Descriptor, error) {
	requestContext, err := c.context(policyDigest)
	if err != nil {
		return fleet.Descriptor{}, err
	}
	spec := entry.Spec
	var response fleetDescriptorWire
	if err := c.post(ctx, "/api/v1/fleet/agents", fleetRegisterWire{
		Context:            requestContext,
		RegistrationKey:    encodeWireID(registrationKey(policyDigest, spec, entry.LiveGeneration)),
		ExpectedGeneration: entry.LiveGeneration,
		Descriptor: fleetSpecWire{
			ID: encodeWireID(spec.ID), ParentID: encodeWireID(spec.ParentID),
			AuthorizationDomain: spec.AuthorizationDomain, Scopes: spec.Scopes,
			ExecutorRef: spec.ExecutorRef, Capabilities: spec.Capabilities,
			LeaseExpiresAt: spec.LeaseExpiresAt,
		},
	}, http.StatusCreated, &response); err != nil {
		return fleet.Descriptor{}, err
	}
	return response.decode()
}

// resolve reads one registration through the resolve route.
func (c *fleetClient) resolve(ctx context.Context, id shoal.ID) (fleet.Descriptor, error) {
	requestContext, err := c.context("")
	if err != nil {
		return fleet.Descriptor{}, err
	}
	requestContext.ReasonCode = "atpl-read"
	var response fleetDescriptorWire
	if err := c.post(ctx, "/api/v1/fleet/agents/"+encodeWireID(id)+"/resolve",
		requestContext, http.StatusOK, &response); err != nil {
		return fleet.Descriptor{}, err
	}
	return response.decode()
}

// registryError is a refusal the registry answered with.
type registryError struct {
	status  int
	code    string
	message string
}

func (e *registryError) Error() string {
	if e.code == "" {
		return fmt.Sprintf("registry answered %d", e.status)
	}
	return fmt.Sprintf("registry refused (%d %s): %s", e.status, e.code, e.message)
}

func (c *fleetClient) post(ctx context.Context, route string, input any, want int, output any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+route, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxResponseBytes {
		return fmt.Errorf("registry response exceeds %d bytes", maxResponseBytes)
	}
	if response.StatusCode != want {
		var failure fleetErrorWire
		if json.Unmarshal(data, &failure) != nil {
			failure = fleetErrorWire{}
		}
		return &registryError{status: response.StatusCode, code: failure.Code, message: failure.Message}
	}
	if err := json.Unmarshal(data, output); err != nil {
		return fmt.Errorf("registry response is not the expected JSON: %w", err)
	}
	return nil
}
