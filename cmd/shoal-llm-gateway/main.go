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

// Command shoal-llm-gateway governs an OpenAI-compatible endpoint by asking Shoal
// before each call and reporting what happened after it.
//
// It needs no cooperation from the caller: anything that speaks the API is
// governed by changing one base URL. That is the property nothing else in Shoal
// has — /api/v1/ask requires a caller built for Shoal.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/phrocker/shoal-oss/internal/healthsurface"
	admissionapi "github.com/phrocker/shoal-oss/pkg/admission/api"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "shoal-llm-gateway: %v\n", err)
		os.Exit(1)
	}
}

// The defaults are named so a test can assert they satisfy validateDurations.
// The previous pair did not, and reading the flag block does not reveal it:
// the two values are declared forty lines apart.
const (
	defaultLease          = 4 * time.Minute
	defaultRequestTimeout = 90 * time.Second
)

var listenTCP = net.Listen

// drain is a variable for the same reason listenTCP is: the window it is given
// is a correctness property, and the only way to observe one from outside is to
// read the deadline off the context it arrives on.
var drain = healthsurface.Drain

func run(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("shoal-llm-gateway", flag.ContinueOnError)
	flags.SetOutput(output)
	listen := flags.String("listen", "127.0.0.1:8100",
		"OpenAI-compatible listen address. Plaintext only on loopback; "+
			"anywhere else it needs -tls-cert-file and -tls-key-file, or "+
			"-allow-plaintext-listener")
	transport := listenerFlags(flags)
	allowedHost := flags.String("allowed-host", "",
		"Comma-separated exact-match allow-list of external authorities (host "+
			"or host:port) an inbound Host or :authority must match. No "+
			"wildcard, no suffix form, and X-Forwarded-Host is never trusted. "+
			"Defaults to the resolved listen address, which is what makes a "+
			"loopback default safe: without this gate a browser can DNS-rebind "+
			"a name it controls to the loopback listener and spend the "+
			"operator's upstream credential on a call nobody made")
	healthAddress := flags.String("health-address", "",
		"Optional separate listener serving GET /healthz, GET /readyz and GET /metrics for "+
			"orchestrator probes and scrapers. Empty disables it. It is a second listener "+
			"because the request surface answers only the completions route "+
			"and a probe must not be mistaken for a call")
	allowPlaintextAdmission := flags.Bool("allow-plaintext-admission", false,
		"Accept a remote http:// -admission-url. Off by default: over "+
			"plaintext the bearer token and the verdict both cross the "+
			"network in the clear, and anything on the path can rewrite a "+
			"deny into an allow — which removes the enforcement plane while "+
			"leaving every sign that it is running. This exists for a mesh "+
			"that already authenticates the hop, and makes accepting it an "+
			"explicit act. It does not apply to -upstream-base-url, which "+
			"carries the prompt itself")
	admissionURL := flags.String("admission-url", "",
		"Base URL of the Shoal workspace whose admission surface decides each "+
			"call. Required: without a decision plane this is a plain relay, "+
			"and a relay that cannot be told apart from an enforcement point "+
			"is worse than no enforcement point")
	admissionTokenEnv := flags.String("admission-token-env", "SHOAL_ADMISSION_TOKEN",
		"Environment variable read at request time holding the bearer token "+
			"this gateway presents to the workspace")
	admissionTokenFile := flags.String("admission-token-file", "",
		"File read at request time holding that bearer token, instead of an "+
			"environment variable. This is the form a projected "+
			"ServiceAccount token takes: the kubelet rewrites the file when it "+
			"rotates the token and never updates an environment variable, so "+
			"with only the env form a rotating token cannot be used at all. "+
			"Mutually exclusive with -admission-token-env")
	upstreamBaseURL := flags.String("upstream-base-url", "",
		"The real OpenAI-compatible provider this gateway forwards to")
	models := flags.String("model", "",
		"Comma-separated model names this deployment expects. The declaration "+
			"sent to the workspace reports the matched name, and reports any "+
			"other model as \"other\" — because the field is caller-controlled "+
			"and a prompt fits in it as readily as a model name. Empty means "+
			"every model is reported as \"other\", which keeps caller text out "+
			"of the declaration by default at the cost of model granularity in "+
			"policy. Naming models here does not restrict which may be called: "+
			"an unlisted model is still forwarded, and the plane may deny it")
	upstreamKeyEnv := flags.String("upstream-api-key-env", "SHOAL_UPSTREAM_API_KEY",
		"Environment variable read at request time holding the upstream "+
			"credential. A container's environment is fixed after start, so "+
			"this form cannot be rotated without replacing the pod")
	upstreamKeyFile := flags.String("upstream-api-key-file", "",
		"File read at request time holding the upstream credential, instead "+
			"of an environment variable. Use this where the credential "+
			"rotates: a Secret mounted as a volume is updated in place, where "+
			"the same Secret behind a secretKeyRef is not. Mutually "+
			"exclusive with -upstream-api-key-env")
	agentID := flags.String("agent-id", "",
		"Registered descriptor this gateway admits against, as unpadded "+
			"base64url. This is the encoded ID, not a descriptor's display "+
			"name: the workspace decodes the field, and a readable name "+
			"either fails to decode or decodes to bytes nothing was "+
			"registered under, which denies every call as a descriptor that "+
			"does not exist")
	agentGeneration := flags.Int64("agent-generation", 0,
		"Generation of the registered descriptor; must be positive")
	capability := flags.String("capability", "",
		"Registered capability name")
	action := flags.String("action", "",
		"Registered action name")
	sourceID := flags.String("source-id", "",
		"Scope source ID, unpadded base64url")
	policyID := flags.String("policy-id", "",
		"Scope policy ID, unpadded base64url")
	lease := flags.Duration("lease", defaultLease,
		"Admission lease. A call must be reported within it or the grant shows "+
			"as outstanding. The fleet refuses a lease above "+
			admissionapi.MaxLease.String()+" rather than shortening it, so a "+
			"larger value denies every call instead of degrading, and it must "+
			"exceed -request-timeout by at least "+
			minimumReportWindow.String()+" so the call can still be reported")
	requestTimeout := flags.Duration("request-timeout", defaultRequestTimeout,
		"Upstream request timeout. It must fit inside the lease with the "+
			"report window to spare")
	if err := flags.Parse(args); err != nil {
		return err
	}

	if strings.TrimSpace(*admissionURL) == "" {
		return errors.New("-admission-url is required")
	}
	if strings.TrimSpace(*upstreamBaseURL) == "" {
		return errors.New("-upstream-base-url is required")
	}
	if strings.TrimSpace(*agentID) == "" || *agentGeneration <= 0 {
		return errors.New("-agent-id and a positive -agent-generation are required")
	}
	// Checked here and still sent verbatim: the workspace wants the encoded
	// form, so this validates rather than converts. Without it a misencoded
	// agent ID surfaces as a descriptor that does not exist, once per call,
	// which names the wrong cause at the wrong time.
	//
	// It catches the encoding mistakes and not the whole class. A shoal.ID is
	// opaque and variable-length, so there is no width to check and a name that
	// happens to decode — "gateway" is five valid bytes — is indistinguishable
	// here from a real ID. Only the workspace can tell those apart, and this
	// proxy does not consult it until the first call (#390).
	// Validated and normalized together, because the two must not diverge.
	//
	// decodeID trims before decoding, so " YWdlbnQ " validated cleanly here
	// and was then stored and sent verbatim — and the workspace's decoder does
	// not trim, so it failed to decode there. Static configuration that passed
	// startup produced a 400 on every call, reported to the caller as a
	// retryable 503. The flag's value is replaced with the form that was
	// actually checked.
	//
	// This normalizes where -capability and -action refuse, and the difference
	// is deliberate: whitespace in a fleet name is invalid at the plane and the
	// operator needs to know, while whitespace around an opaque base64url blob
	// is transport noise that the local decoder already ignores. What is not
	// defensible is validating one form and sending another.
	*agentID = strings.TrimSpace(*agentID)
	if _, err := decodeID("-agent-id", *agentID); err != nil {
		return err
	}
	// What was passed, not what it resolved to. A flag left at its default is
	// not a choice, and treating it as one is what made the mutual-exclusion
	// rule miss the operator who typed the default.
	chosen := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { chosen[f.Name] = true })

	admissionCredential, err := credentialSource(
		"-admission-token", *admissionTokenEnv, *admissionTokenFile,
		chosen["admission-token-env"])
	if err != nil {
		return err
	}
	upstreamCredential, err := credentialSource(
		"-upstream-api-key", *upstreamKeyEnv, *upstreamKeyFile,
		chosen["upstream-api-key-env"])
	if err != nil {
		return err
	}
	if err := fleetName("-capability", *capability); err != nil {
		return err
	}
	if err := fleetName("-action", *action); err != nil {
		return err
	}
	if err := validateDurations(*lease, *requestTimeout); err != nil {
		return err
	}
	source, err := decodeID("-source-id", *sourceID)
	if err != nil {
		return err
	}
	policy, err := decodeID("-policy-id", *policyID)
	if err != nil {
		return err
	}
	base, err := planeURL(*admissionURL, *allowPlaintextAdmission)
	if err != nil {
		return fmt.Errorf("-admission-url %v", err)
	}

	// Credentials are read at the moment they are used rather than captured at
	// start. What that buys, precisely:
	//
	// For the -file forms, rotation. The kubelet rewrites a projected token in
	// place and a Secret mounted as a volume is updated too, so reading per
	// request is the whole mechanism by which a rotating credential works.
	//
	// For the env form, almost nothing. A process environment is fixed once
	// the container starts, so updating the Secret behind a secretKeyRef
	// leaves every running proxy on the old value until the pod is replaced,
	// and os.Getenv re-reads the same string forever. Nor does it keep the
	// value out of process memory: the environment block holds it for the
	// process lifetime, so a crash dump carries it whether this reads it once
	// or a thousand times. All per-request lookup avoids is one extra cached
	// copy.
	//
	// Two false claims have been corrected here, and the second was in the
	// same paragraph as the first — the rotation claim was fixed a round
	// before the crash-dump claim sitting beside it was noticed. A comment
	// asserting a security property is worth no more than a test asserting
	// one.
	logf := func(format string, values ...any) {
		fmt.Fprintf(output, format+"\n", values...)
	}
	admission := &admissionClient{
		base:       base,
		http:       newHTTPClient(*requestTimeout),
		credential: admissionCredential,
		agentID:    *agentID, agentGeneration: *agentGeneration,
		capability: *capability, action: *action,
		sourceID: source, policyID: policy, lease: *lease,
	}
	// Built now so a plane URL the admission client refuses (credentials, a
	// query or a fragment in it) stops startup instead of failing every call.
	if _, err := admission.client(); err != nil {
		return fmt.Errorf("-admission-url %v", err)
	}
	tlsConfig, err := transport.config(logf)
	if err != nil {
		return err
	}
	listener, err := listenTCP("tcp", *listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", *listen, err)
	}
	if err := transport.admit(*listen, listener.Addr(), tlsConfig); err != nil {
		listener.Close()
		return err
	}
	// Resolved from the listener, not the requested address: a wildcard bind
	// resolves to an authority real clients never send, so defaulting to it
	// refuses everything rather than admitting anything. That is the intended
	// outcome — a public bind must name its external authority explicitly.
	allowedHosts, err := authorities(*allowedHost, listener.Addr().String())
	if err != nil {
		listener.Close()
		return err
	}
	governed, err := newProxy(
		admission, *upstreamBaseURL, upstreamCredential,
		allowedHosts, strings.Split(*models, ","),
		*requestTimeout, time.Now, logf)
	if err != nil {
		listener.Close()
		return err
	}
	server := &http.Server{
		Handler:           governed.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0, // streamed responses have no useful write bound
		IdleTimeout:       60 * time.Second,
	}
	fmt.Fprintf(output, "Shoal LLM gateway listening at %s://%s\n",
		listenerScheme(tlsConfig), listener.Addr())
	fmt.Fprintf(output, "Admitting against %s as %s/%s\n",
		base.String(), *capability, *action)

	state := &healthsurface.State{}
	health := (*healthsurface.Server)(nil)
	if address := strings.TrimSpace(*healthAddress); address != "" {
		health, err = healthsurface.StartWithConfig(address, state, governed.healthConfig())
		if err != nil {
			listener.Close()
			return fmt.Errorf("listen on %s: %w", address, err)
		}
		fmt.Fprintf(output, "Health surface listening at http://%s\n", health.Address())
	}

	// The drain window is the lease, and deliberately not a number of its own.
	//
	// A ten-second drain abandoned in-flight connections, and an admitted call
	// may run for the whole request timeout before it has anything to report —
	// 90s by default. So every rolling update stranded the calls that were
	// mid-flight: the egress had happened, the grant was spent, and the report
	// that closes it was killed with the listener. An unreported grant is the
	// one outcome this proxy exists to prevent, and a rollout produced them on
	// purpose, on a schedule, invisibly.
	//
	// The lease is already the bound on an admitted call's entire lifetime
	// including its report — that is what validateDurations checks it against —
	// so it is the correct worst case for a call admitted a moment before the
	// signal, and it needs no second invariant to keep it honest. The cost is
	// explicit: a pod can take the lease to exit, so an operator who wants
	// faster rollouts chooses a shorter lease, which shortens the longest call
	// it will admit. The container's terminationGracePeriodSeconds must exceed
	// this, or the kubelet sends SIGKILL and the strandings come back.
	drainWindow := *lease
	// Ready before the watcher starts, not after it.
	//
	// The watcher can observe an already-cancelled context, run Drain, and mark
	// the surface draining before this line executes — after which MarkReady
	// flips readiness back to true while the listener is already closing. A
	// probe then gets a ready answer from a process that is shutting down,
	// which is the inverse of what the health surface exists for. Marking ready
	// first means every cancellation transition happens after it and wins.
	state.MarkReady()
	shutdownDone := make(chan error, 1)
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), drainWindow)
		defer cancel()
		shutdownDone <- drain(shutdown, state, server, health)
	}()
	err = serve(server, listener, tlsConfig)
	if errors.Is(err, http.ErrServerClosed) {
		return <-shutdownDone
	}
	shutdown, cancel := context.WithTimeout(context.Background(), drainWindow)
	defer cancel()
	_ = drain(shutdown, state, server, health)
	return err
}

// ErrNoCredential says that no credential was supplied, as distinct from one
// that was supplied and could not be read.
//
// The difference decides whether an unauthenticated request may be sent. A
// loopback model server generally has no notion of a credential, so absence
// there is a legitimate configuration — but "absent" and "configured and
// broken" reached the forward path as the same bare error, so a loopback
// upstream whose credential file had the wrong permissions forwarded the prompt
// with no Authorization header instead of refusing. The comment there claimed
// the opposite, and nothing in the code could have made it true.
var ErrNoCredential = errors.New("no credential is configured")

// credentialFromEnv reads a secret at use time.
//
// Per request, but a container's environment does not change after start, so
// this neither rotates nor keeps the value out of process memory. See the note
// in run; use the -file form where either property is wanted.
//
// An unset variable is absence rather than breakage: the chart renders no env
// entry at all when no Secret is named, which is how a loopback upstream is
// meant to be configured.
func credentialFromEnv(name string) func() (string, error) {
	return func() (string, error) {
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" {
			return "", fmt.Errorf("%s: %w", name, ErrNoCredential)
		}
		return value, nil
	}
}

// credentialFromFile reads a secret at use time, like credentialFromEnv.
//
// Reading per request is what makes a rotating token work: the kubelet replaces
// the file in place, so a value captured at start would be the one that expired.
func credentialFromFile(path string) func() (string, error) {
	return func() (string, error) {
		// Naming a file is configuring a credential, so every failure here is
		// breakage and never absence. It must not wrap ErrNoCredential: that
		// would let a loopback upstream treat an unreadable or empty file as
		// "none was wanted" and forward the prompt unauthenticated.
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("%s is unreadable: %w", path, err)
		}
		value := strings.TrimSpace(string(raw))
		if value == "" {
			return "", fmt.Errorf("%s is empty", path)
		}
		return value, nil
	}
}

// credentialSource picks between the env and file forms, and refuses both.
//
// Taking one silently when both are set would make the effective credential
// invisible: an operator who adds a file while a stale env var is still in the
// manifest cannot tell from the configuration which one is being presented, and
// the symptom of the wrong answer is an authentication failure that names
// neither.
//
// envChosen says whether the env flag was actually passed, which is the only
// form of that question with no hole in it. This compared the flag's value
// against its default instead, because the env flag ships with a non-empty
// default and "both non-empty" would otherwise make the file form unreachable.
// That let the exact case the rule exists to close straight through: an
// operator who writes -admission-token-env=SHOAL_ADMISSION_TOKEN explicitly
// beside a file got the file silently, having typed the default. flag.Visit
// reports what was set rather than what it ended up as, so the rule can be
// exact without costing reachability.
func credentialSource(
	name, fromEnv, fromFile string, envChosen bool,
) (func() (string, error), error) {
	env, file := strings.TrimSpace(fromEnv), strings.TrimSpace(fromFile)
	if file == "" {
		if env == "" {
			return nil, fmt.Errorf("%s-env or %s-file is required", name, name)
		}
		return credentialFromEnv(env), nil
	}
	if envChosen {
		return nil, fmt.Errorf("%s-env and %s-file are mutually exclusive", name, name)
	}
	return credentialFromFile(file), nil
}

// fleetName applies the grammar the admission service applies, at startup.
//
// It mirrors validateName in pkg/explorer/fleet/model.go, which is unexported —
// a duplication worth naming out loud, since the two can drift. The bound is
// taken from admissionapi.MaxNameBytes, whose parity with the fleet's is
// tested, so at least the number cannot.
//
// Without this, a name over the bound or carrying a stray space or an
// unsupported character is static configuration that starts cleanly, passes
// both probes, and then takes a 400 on every admission — which this client
// reports as plane_unavailable, so the caller is told to retry a configuration
// error and the operator sees what looks like an outage.
//
// The surrounding-whitespace case is the one a trim-based check actively hides.
// The previous check trimmed before testing for emptiness, so " complete"
// passed and was then sent with the space, which the plane refuses.
func fleetName(flagName, value string) error {
	if value == "" {
		return fmt.Errorf("%s is required", flagName)
	}
	if len(value) > admissionapi.MaxNameBytes {
		return fmt.Errorf("%s must be at most %d bytes", flagName, admissionapi.MaxNameBytes)
	}
	if strings.TrimSpace(value) != value {
		return fmt.Errorf("%s must not have leading or trailing whitespace", flagName)
	}
	for _, character := range value {
		switch {
		case character >= 'a' && character <= 'z',
			character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9',
			character == '_', character == '-', character == '.', character == ':':
		default:
			return fmt.Errorf(
				"%s may use only letters, digits, and _-.: (found %q)",
				flagName, character)
		}
	}
	return nil
}

// authorities resolves the host allow-list, falling back to the bound address.
func authorities(configured, resolved string) ([]string, error) {
	candidates := strings.Split(configured, ",")
	if strings.TrimSpace(configured) == "" {
		candidates = []string{resolved}
	}
	result := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		normalized, ok := normalizeAuthority(candidate)
		if !ok {
			return nil, fmt.Errorf(
				"-allowed-host %q is not a host or host:port", candidate)
		}
		result = append(result, normalized)
	}
	return result, nil
}

// validateDurations refuses a lease that cannot cover the call it admits.
//
// It is a function so the invariant can be tested without starting a listener.
// The two rules fail for opposite reasons and neither degrades gracefully:
//
// Too large and the fleet refuses the lease outright rather than shortening it,
// so every call is denied. Too small and the lease expires while the upstream
// call is still running, so the call is forwarded and its report is then
// rejected as expired — an unreportable grant guaranteed by configuration,
// which no runtime check can recover. The shipped defaults did exactly that: a
// one-minute lease against a two-minute timeout left every call over a minute
// unreportable.
func validateDurations(lease, requestTimeout time.Duration) error {
	if lease <= 0 || requestTimeout <= 0 {
		return errors.New("-lease and -request-timeout must be positive")
	}
	if lease > admissionapi.MaxLease {
		return fmt.Errorf("-lease must not exceed %s", admissionapi.MaxLease)
	}
	if lease <= requestTimeout+minimumReportWindow {
		return fmt.Errorf(
			"-lease must exceed -request-timeout by at least %s so the call "+
				"can still be reported", minimumReportWindow)
	}
	return nil
}
