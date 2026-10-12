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
	"bytes"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"net/url"
	"os"
	"strings"
	"time"

	dispatchapi "github.com/phrocker/shoal-oss/pkg/dispatch/api"
	"github.com/phrocker/shoal-oss/pkg/executorref"
)

// The defaults are named so a test can assert they satisfy ValidateDurations.
// The LLM gateway shipped a pair that did not, and reading a flag block does
// not reveal it.
const (
	DefaultClaimLease       = 4 * time.Minute
	DefaultOperationTimeout = 3 * time.Minute
	DefaultPlaneTimeout     = 10 * time.Second
	DefaultMaxResponseBytes = 64 << 10
	DefaultPullLimit        = 32
	DefaultPullInterval     = 2 * time.Second
	DefaultCapability       = "effects.http"
	DefaultAuthHeader       = "Authorization"

	// MaxResponseBytesLimit keeps the worker's read bound below the
	// explorer's output bound, so a hostile target cannot make the worker
	// buffer more than the record could ever hold.
	MaxResponseBytesLimit = dispatchapi.MaxActionOutputBytes
	// minPullInterval keeps an idle worker from spinning on the explorer.
	minPullInterval = 100 * time.Millisecond
)

// ErrNoCredential says no credential was supplied, as distinct from one that
// was supplied and could not be read. Only the env form can be absent; naming
// a file is configuring a credential, so every failure there is breakage.
var ErrNoCredential = errors.New("no credential is configured")

// Config is the validated configuration. Every field has passed the rules in
// ParseFlags; nothing here is a raw flag value.
type Config struct {
	DispatchURL        *url.URL
	DispatchCredential func() (string, error)
	AgentID            []byte
	AgentIDEncoded     string
	Capability         string
	SurfaceName        string

	TargetBaseURL    *url.URL
	TargetCredential func() (string, error)
	// TargetCredentialNamed is true when -target-credential-env or
	// -target-credential-file was passed: a credential was configured, so
	// its absence is breakage even beside a client certificate.
	TargetCredentialNamed bool
	TargetAuthHeader      string
	TargetAllowPrivate    bool
	IdempotencyHeader     string
	IdempotencyRetention  time.Duration
	// TargetTLS is the client certificate presented to the target and the
	// roots it is verified against (#662); the zero value is no certificate
	// and the system roots.
	TargetTLS TargetTLS

	Routes  *RouteTable
	Effects dispatchapi.Effects

	ClaimLease       time.Duration
	OperationTimeout time.Duration
	PlaneTimeout     time.Duration
	MaxResponseBytes int64
	PullLimit        int
	PullInterval     time.Duration
	HealthAddress    string
	Renew            bool

	// ExecutorRef is the ref the dispatch credential is minted for; the
	// client is bound to it and resolves only the descriptor that names it.
	ExecutorRef string
	// UnrecordedDir holds the unrecorded log and the single-replica lock.
	UnrecordedDir string
	MaxInFlight   int
	// Pod names this replica in its claim IDs.
	Pod string
	// AttestationStatementFile and AttestationKeyFile are presented when an
	// action of the capability requires attestation; both or neither.
	AttestationStatementFile string
	AttestationKeyFile       string
}

// GracePeriod is the minimum terminationGracePeriodSeconds for this
// configuration: T + max(ReportWindow, 3×planeTimeout) + 2×ReportWindow + 5s.
func (c *Config) GracePeriod() time.Duration {
	return GracePeriod(c.OperationTimeout, c.PlaneTimeout)
}

// GracePeriodSeconds is GracePeriod rounded up to whole seconds: the
// terminationGracePeriodSeconds this configuration needs.
func (c *Config) GracePeriodSeconds() int64 {
	return GracePeriodSeconds(c.OperationTimeout, c.PlaneTimeout)
}

// SendGate is the gate this configuration implies.
func (c *Config) SendGate() SendGate {
	return SendGate{
		OperationTimeout: c.OperationTimeout, Renew: c.Renew,
		RenewAfter: c.ClaimLease / 2,
	}
}

// TargetAuthorization resolves the target credential for one request.
//
// An absent credential (the env form, variable unset) is a legitimate
// configuration only for a loopback target, which commonly has no notion of
// one, or for a target the gateway authenticates to with a client
// certificate (#662), where the certificate is the credential. Anywhere else
// absence is refused rather than sent unauthenticated: the request would go
// out, be rejected, and be recorded as a target rejection of an action that
// was never really attempted. A configured-but-broken credential is refused
// everywhere, client certificate or not.
func (c *Config) TargetAuthorization() (header, value string, err error) {
	value, err = c.TargetCredential()
	switch {
	case err == nil:
		return c.TargetAuthHeader, value, nil
	case errors.Is(err, ErrNoCredential) && isLoopbackHost(c.TargetBaseURL.Hostname()):
		return "", "", nil
	case errors.Is(err, ErrNoCredential) && c.TargetTLS.ClientCertificate != nil &&
		!c.TargetCredentialNamed:
		// Only the default variable, unset: no credential was configured.
		// An env variable the operator named is a credential they meant to
		// send, and its absence (a typo, a Secret never mounted) is refused.
		return "", "", nil
	default:
		return "", "", errors.New("target credential is unavailable")
	}
}

// ParseFlags parses and validates the gateway's flags. Usage and flag-parse
// errors go to output; every validation refusal names the flag it concerns.
func ParseFlags(args []string, output io.Writer) (*Config, error) {
	flags := flag.NewFlagSet("shoal-gateway", flag.ContinueOnError)
	flags.SetOutput(output)

	dispatchURL := flags.String("dispatch-url", "",
		"Base URL of the Shoal explorer whose fleet dispatch queue this gateway "+
			"pulls from. Required. https, or http addressing loopback")
	allowPlaintextDispatch := flags.Bool("allow-plaintext-dispatch", false,
		"Accept a remote http:// -dispatch-url, for a mesh that already "+
			"authenticates the hop. Over plaintext the bearer token, every "+
			"action's input and every completion cross the network in the clear")
	dispatchTokenEnv := flags.String("dispatch-token-env", "SHOAL_DISPATCH_TOKEN",
		"Environment variable read per call holding the explorer bearer token")
	dispatchTokenFile := flags.String("dispatch-token-file", "",
		"File read per call holding the explorer bearer token, instead of an "+
			"environment variable; the form a projected token rotates through. "+
			"Mutually exclusive with -dispatch-token-env")
	agentID := flags.String("agent-id", "",
		"Registered descriptor this gateway performs work for, as unpadded base64url")
	capability := flags.String("capability", DefaultCapability,
		"Registered capability whose actions the route table implements")
	surfaceName := flags.String("surface-name", "",
		"Operator name for the operational surface, recorded as the target of "+
			"an ambiguity report")

	targetBaseURL := flags.String("target-base-url", "",
		"Base URL of the operational surface. Required. https, or http "+
			"addressing loopback; there is no plaintext opt-out, because this "+
			"hop carries the target credential to whatever is on the path")
	targetCredentialEnv := flags.String("target-credential-env", "SHOAL_TARGET_CREDENTIAL",
		"Environment variable read per request holding the target credential, "+
			"sent verbatim as the value of -target-auth-header")
	targetCredentialFile := flags.String("target-credential-file", "",
		"File read per request holding the target credential, instead of an "+
			"environment variable. Mutually exclusive with -target-credential-env")
	targetAuthHeader := flags.String("target-auth-header", DefaultAuthHeader,
		"Header that carries the target credential. The credential is the "+
			"whole value, so a bearer token is stored as \"Bearer <token>\"")
	idempotencyHeader := flags.String("idempotency-header", "",
		"Header that carries the action's ExecutorKey on key routes, e.g. "+
			"Idempotency-Key. Required when any route uses idempotency \"key\"")
	idempotencyRetention := flags.Duration("idempotency-retention", 0,
		"How long the target deduplicates a key. Required with key routes and "+
			"must exceed -operation-timeout by the report window; an action "+
			"whose deadline outlives it is skipped rather than performed under a "+
			"guarantee that has lapsed")
	targetClientCert := flags.String("target-client-cert-file", "",
		"PEM certificate chain presented to the target when it asks for a client "+
			"certificate (mTLS, #662). Requires -target-client-key-file and an "+
			"https target. Read again at each TLS handshake, so a rotated Secret "+
			"takes effect on the next connection")
	targetClientKey := flags.String("target-client-key-file", "",
		"PEM private key for -target-client-cert-file")
	targetCA := flags.String("target-ca-file", "",
		"PEM bundle of CA certificates the target's certificate is verified "+
			"against, instead of the system roots, for the target only. Requires "+
			"an https target. Read once at startup")
	targetAllowPrivate := flags.Bool("target-allow-private", false,
		"Allow the target to resolve to private or loopback addresses. "+
			"Link-local and cloud metadata addresses are refused regardless")

	routes := flags.String("routes", "",
		"Route table, as a JSON array. Strict: unknown fields and duplicate keys "+
			"are refused, and an action with no route is never performed")

	claimLease := flags.Duration("claim-lease", DefaultClaimLease,
		"Claim lease L. At most "+dispatchapi.MaxActionClaimTTL.String()+"; the "+
			"explorer refuses a longer one rather than shortening it")
	operationTimeout := flags.Duration("operation-timeout", DefaultOperationTimeout,
		"Bound T on one request to the target")
	planeTimeout := flags.Duration("plane-timeout", DefaultPlaneTimeout,
		"Bound on one call to the explorer. At most L/4")
	maxResponseBytes := flags.Int64("max-response-bytes", DefaultMaxResponseBytes,
		"Bytes of a target response the worker reads. A larger body is "+
			"treated as success without a reference, never buffered")
	pullLimit := flags.Int("pull-limit", DefaultPullLimit,
		fmt.Sprintf("Actions requested per pull, 1 to %d", dispatchapi.MaxDispatchListResults))
	pullInterval := flags.Duration("pull-interval", DefaultPullInterval,
		"Pause between pulls when the queue offered nothing")
	healthAddress := flags.String("health-address", "",
		"Optional listener for GET /healthz, GET /readyz and GET /metrics")
	renew := flags.Bool("renew", false,
		"Extend the claim every L/2 (#430). The lease is then a silence "+
			"interval rather than the bound on the whole call, so "+
			"-operation-timeout may exceed it; each action's deadline still "+
			"bounds the operation")
	executorRef := flags.String("executor-ref", "",
		"Executor reference the dispatch credential is minted for and the "+
			"descriptor registers. Required. The gateway resolves only the "+
			"descriptor that names it")
	unrecordedDir := flags.String("unrecorded-dir", "",
		"Directory holding the unrecorded-report log and the lock that keeps "+
			"one gateway per surface. Required; a persistent volume, or a report "+
			"the explorer refused is lost with the pod")
	maxInFlight := flags.Int("max-in-flight", DefaultMaxInFlight,
		fmt.Sprintf("Claims held at once, 1 to %d", MaxInFlightLimit))
	podName := flags.String("pod-name", "",
		"Name of this replica in its claim IDs. Defaults to the host name, "+
			"which in Kubernetes is the pod name")
	attestationStatement := flags.String("attestation-statement-file", "",
		"Attestation statement presented when an action requires attestation. "+
			"Requires -attestation-key-file")
	attestationKey := flags.String("attestation-key-file", "",
		"Idempotency key for the attestation presentation. Requires "+
			"-attestation-statement-file")

	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	if flags.NArg() != 0 {
		return nil, fmt.Errorf("unexpected argument %q; every setting is a flag",
			flags.Arg(0))
	}
	chosen := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { chosen[f.Name] = true })

	config := &Config{
		TargetAllowPrivate:   *targetAllowPrivate,
		IdempotencyRetention: *idempotencyRetention,
		ClaimLease:           *claimLease,
		OperationTimeout:     *operationTimeout,
		PlaneTimeout:         *planeTimeout,
		MaxResponseBytes:     *maxResponseBytes,
		PullLimit:            *pullLimit,
		PullInterval:         *pullInterval,
		Renew:                *renew,
	}
	var err error

	if strings.TrimSpace(*dispatchURL) == "" {
		return nil, errors.New("-dispatch-url is required")
	}
	if config.DispatchURL, err = dispatchBaseURL(*dispatchURL, *allowPlaintextDispatch); err != nil {
		return nil, fmt.Errorf("-dispatch-url %v", err)
	}
	if config.DispatchCredential, err = credentialSource("-dispatch-token",
		*dispatchTokenEnv, *dispatchTokenFile, chosen["dispatch-token-env"]); err != nil {
		return nil, err
	}

	config.AgentIDEncoded = strings.TrimSpace(*agentID)
	if config.AgentIDEncoded == "" {
		return nil, errors.New("-agent-id is required")
	}
	if config.AgentID, err = base64.RawURLEncoding.Strict().DecodeString(
		config.AgentIDEncoded); err != nil || len(config.AgentID) == 0 {
		return nil, errors.New("-agent-id must be unpadded base64url: it is the " +
			"encoded descriptor ID, not a display name")
	}
	if err := fleetName("-capability", *capability); err != nil {
		return nil, err
	}
	config.Capability = *capability
	if err := fleetName("-surface-name", *surfaceName); err != nil {
		return nil, err
	}
	config.SurfaceName = *surfaceName

	if strings.TrimSpace(*targetBaseURL) == "" {
		return nil, errors.New("-target-base-url is required")
	}
	if config.TargetBaseURL, err = targetURL(*targetBaseURL); err != nil {
		return nil, fmt.Errorf("-target-base-url %v", err)
	}
	if isLoopbackHost(config.TargetBaseURL.Hostname()) && !config.TargetAllowPrivate {
		return nil, errors.New("-target-base-url addresses loopback, which the " +
			"target dialer refuses unless -target-allow-private is set")
	}
	if config.TargetCredential, err = credentialSource("-target-credential",
		*targetCredentialEnv, *targetCredentialFile,
		chosen["target-credential-env"]); err != nil {
		return nil, err
	}
	config.TargetCredentialNamed = chosen["target-credential-env"] || chosen["target-credential-file"]
	if config.TargetTLS, err = targetTLSFlags(config.TargetBaseURL,
		strings.TrimSpace(*targetClientCert), strings.TrimSpace(*targetClientKey),
		strings.TrimSpace(*targetCA)); err != nil {
		return nil, err
	}

	config.Effects = DerivedEffects(config.TargetBaseURL)
	if strings.TrimSpace(*routes) == "" {
		return nil, errors.New("-routes is required")
	}
	if config.Routes, err = ParseRoutes([]byte(*routes), config.Effects); err != nil {
		return nil, err
	}

	if err := validateHeaders(config, *targetAuthHeader, *idempotencyHeader); err != nil {
		return nil, err
	}
	if err := ValidateDurations(config.ClaimLease, config.OperationTimeout,
		config.PlaneTimeout, config.Renew); err != nil {
		return nil, err
	}
	if err := ValidateRetention(config.Routes.RequiresKey(),
		config.IdempotencyRetention, config.OperationTimeout); err != nil {
		return nil, err
	}
	if config.MaxResponseBytes <= 0 || config.MaxResponseBytes > MaxResponseBytesLimit {
		return nil, fmt.Errorf("-max-response-bytes must be 1 to %d",
			MaxResponseBytesLimit)
	}
	if config.PullLimit <= 0 || config.PullLimit > dispatchapi.MaxDispatchListResults {
		return nil, fmt.Errorf("-pull-limit must be 1 to %d", dispatchapi.MaxDispatchListResults)
	}
	if config.PullInterval < minPullInterval {
		return nil, fmt.Errorf("-pull-interval must be at least %s", minPullInterval)
	}
	if address := strings.TrimSpace(*healthAddress); address != "" {
		if _, _, err := net.SplitHostPort(address); err != nil {
			return nil, errors.New("-health-address must be host:port")
		}
		config.HealthAddress = address
	}
	if err := executorref.ValidExecutorRef(*executorRef); err != nil {
		return nil, fmt.Errorf("-executor-ref %v", err)
	}
	config.ExecutorRef = *executorRef
	if config.UnrecordedDir = strings.TrimSpace(*unrecordedDir); config.UnrecordedDir == "" {
		return nil, errors.New("-unrecorded-dir is required")
	}
	if *maxInFlight <= 0 || *maxInFlight > MaxInFlightLimit {
		return nil, fmt.Errorf("-max-in-flight must be 1 to %d", MaxInFlightLimit)
	}
	config.MaxInFlight = *maxInFlight
	if config.Pod = *podName; config.Pod == "" {
		if config.Pod, err = os.Hostname(); err != nil {
			return nil, errors.New("-pod-name is required where the host name cannot be read")
		}
	}
	if _, _, err := NewClaimID(config.Pod, bytes.NewReader(make([]byte, ClaimNonceBytes))); err != nil {
		return nil, fmt.Errorf("-pod-name: %v", err)
	}
	config.AttestationStatementFile = strings.TrimSpace(*attestationStatement)
	config.AttestationKeyFile = strings.TrimSpace(*attestationKey)
	if (config.AttestationStatementFile == "") != (config.AttestationKeyFile == "") {
		return nil, errors.New("-attestation-statement-file and -attestation-key-file " +
			"are required together")
	}
	return config, nil
}

// MaxInFlightLimit bounds -max-in-flight. Each claim held reserves one entry
// of the unrecorded log, so the bound sits far below the log's.
const MaxInFlightLimit = 64

// ValidateDurations is the lease arithmetic, as a function so it can be
// tested without parsing flags.
//
//	0 < L ≤ MaxActionClaimTTL      the explorer refuses a longer lease outright
//	0 < planeTimeout ≤ L/4         one renewal retry fits before L/2 is overdue
//	without renewal:
//	  L > T + ReportWindow + planeTimeout
//
// Without renewal the lease is the bound on the whole call: the claim round
// trip, the operation, and the report all have to fit inside it, or an action
// is performed and then cannot be reported — an unreportable effect guaranteed
// by configuration, which no runtime check recovers. With renewal the lease is
// a silence interval and the operation is bounded by the action's deadline
// instead, which the send gate checks per action.
func ValidateDurations(lease, operation, plane time.Duration, renew bool) error {
	if lease <= 0 {
		return errors.New("-claim-lease must be positive")
	}
	if lease > dispatchapi.MaxActionClaimTTL {
		return fmt.Errorf("-claim-lease must not exceed %s; the explorer refuses "+
			"a longer lease rather than shortening it, so every claim would fail",
			dispatchapi.MaxActionClaimTTL)
	}
	if operation <= 0 {
		return errors.New("-operation-timeout must be positive")
	}
	if plane <= 0 {
		return errors.New("-plane-timeout must be positive")
	}
	if plane > lease/4 {
		return fmt.Errorf("-plane-timeout must be at most a quarter of "+
			"-claim-lease (%s), so a failed renewal can be retried once before "+
			"the lease is half spent", lease/4)
	}
	if !renew && lease <= operation+ReportWindow+plane {
		return fmt.Errorf("-claim-lease must exceed -operation-timeout + %s + "+
			"-plane-timeout (%s) without renewal: the lease is then the bound on "+
			"the claim, the operation and its report together",
			ReportWindow, operation+ReportWindow+plane)
	}
	return nil
}

// ValidateRetention checks the idempotency window against the operation.
//
// The two claim-time rules
//
//	T + ReportWindow < deadline − now   and   deadline − created_at ≤ retention
//
// are jointly unsatisfiable when retention ≤ T + ReportWindow: nothing is
// ever claimable, and the operator sees a gateway that never takes work with
// no sign that the pair is the cause. So the relationship is refused where
// the configuration is read, not discovered per claim.
func ValidateRetention(requiresKey bool, retention, operation time.Duration) error {
	if !requiresKey {
		if retention != 0 {
			return errors.New("-idempotency-retention is set but no route uses " +
				"idempotency \"key\"; a retention nothing relies on suggests a " +
				"route was meant to")
		}
		return nil
	}
	if retention <= 0 {
		return errors.New("-idempotency-retention is required when any route " +
			"uses idempotency \"key\"")
	}
	if retention <= operation+ReportWindow {
		return fmt.Errorf("-idempotency-retention must exceed -operation-timeout "+
			"+ %s (%s), or no action could ever be claimed", ReportWindow,
			operation+ReportWindow)
	}
	return nil
}

// binderHeaders are written by the binder or the transport, and neither
// credential nor key may be configured to land on one.
var binderHeaders = map[string]bool{
	"User-Agent": true, "Accept": true, "Content-Type": true,
	"Content-Length": true, "Host": true, "Transfer-Encoding": true,
	"Connection": true, "Accept-Encoding": true, "Te": true, "Trailer": true,
	"Upgrade": true, "Cookie": true,
}

func validateHeaders(config *Config, authHeader, idempotencyHeader string) error {
	if !validHeaderName(authHeader) {
		return errors.New("-target-auth-header must be a valid HTTP header name")
	}
	auth := textproto.CanonicalMIMEHeaderKey(authHeader)
	if binderHeaders[auth] {
		return fmt.Errorf("-target-auth-header must not be %s, which the "+
			"gateway writes itself", auth)
	}
	config.TargetAuthHeader = auth
	if idempotencyHeader == "" {
		if config.Routes.RequiresKey() {
			return errors.New("-idempotency-header is required when any route " +
				"uses idempotency \"key\"")
		}
		return nil
	}
	if !config.Routes.RequiresKey() {
		return errors.New("-idempotency-header is set but no route uses " +
			"idempotency \"key\"")
	}
	if !validHeaderName(idempotencyHeader) {
		return errors.New("-idempotency-header must be a valid HTTP header name")
	}
	key := textproto.CanonicalMIMEHeaderKey(idempotencyHeader)
	if binderHeaders[key] {
		return fmt.Errorf("-idempotency-header must not be %s, which the "+
			"gateway writes itself", key)
	}
	if key == auth {
		return errors.New("-idempotency-header and -target-auth-header must " +
			"differ; the credential would overwrite the key or the key the credential")
	}
	config.IdempotencyHeader = key
	return nil
}

// absoluteURL is the transport rule shared with the LLM gateway: an absolute
// URL with a host, https, or http addressing loopback. Userinfo, a query and a
// fragment are refused: none can be part of a base URL, and userinfo is a
// credential in the configuration where no rotation reaches it.
func absoluteURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !parsed.IsAbs() || parsed.Hostname() == "" {
		return nil, errors.New("must be an absolute URL with a host")
	}
	if parsed.User != nil {
		return nil, errors.New("must not carry userinfo; use the credential flags")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return nil, errors.New("must not carry a query or fragment")
	}
	if parsed.Scheme != "https" &&
		!(parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname())) {
		return nil, errors.New("must use https, or http addressing loopback")
	}
	return parsed, nil
}

// dispatchBaseURL applies absoluteURL to the explorer, with the one
// acknowledged exception: an explicit opt-in for a remote http:// URL behind a
// mesh that authenticates the hop.
func dispatchBaseURL(raw string, allowPlaintext bool) (*url.URL, error) {
	parsed, err := absoluteURL(raw)
	if err == nil || !allowPlaintext {
		return parsed, err
	}
	relaxed, parseErr := url.Parse(strings.TrimSpace(raw))
	if parseErr != nil || !relaxed.IsAbs() || relaxed.Hostname() == "" ||
		relaxed.Scheme != "http" || relaxed.User != nil ||
		relaxed.RawQuery != "" || relaxed.ForceQuery || relaxed.Fragment != "" {
		return nil, err
	}
	return relaxed, nil
}

// targetURL is absoluteURL with no plaintext opt-out. The target hop carries
// the target credential and the action's input, and a mesh that
// authenticates the hop to the explorer says nothing about the hop to an
// operational surface.
func targetURL(raw string) (*url.URL, error) {
	parsed, err := absoluteURL(raw)
	if err != nil {
		return nil, err
	}
	if parsed.Opaque != "" {
		return nil, errors.New("must be a hierarchical URL")
	}
	return parsed, nil
}

// targetTLSFlags validates and loads the target's TLS flags. Both are https
// only: over loopback plaintext there is no handshake for a certificate to be
// presented in or a root to verify, and a setting nothing relies on suggests
// the target URL was meant to be https.
func targetTLSFlags(target *url.URL, certFile, keyFile, caFile string) (TargetTLS, error) {
	var settings TargetTLS
	if (certFile == "") != (keyFile == "") {
		return settings, errors.New("-target-client-cert-file and -target-client-key-file " +
			"are required together")
	}
	if (certFile != "" || caFile != "") && target.Scheme != "https" {
		return settings, errors.New("-target-client-cert-file and -target-ca-file " +
			"require an https -target-base-url: a plaintext target has no TLS handshake")
	}
	if certFile != "" {
		certificate, err := LoadClientCertificate(certFile, keyFile)
		if err != nil {
			return settings, fmt.Errorf("-target-client-cert-file: %v", err)
		}
		settings.ClientCertificate = certificate
	}
	if caFile != "" {
		pool, err := LoadRootCAs(caFile)
		if err != nil {
			return settings, fmt.Errorf("-target-ca-file: %v", err)
		}
		settings.RootCAs = pool
	}
	return settings, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}

// fleetName applies the fleet's name grammar at startup. It mirrors
// validateName in pkg/explorer/fleet/model.go, which is unexported, and takes
// the bound from dispatchapi.MaxNameBytes so at least the number cannot drift.
func fleetName(flagName, value string) error {
	if value == "" {
		return fmt.Errorf("%s is required", flagName)
	}
	if len(value) > dispatchapi.MaxNameBytes {
		return fmt.Errorf("%s must be at most %d bytes", flagName, dispatchapi.MaxNameBytes)
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
			return fmt.Errorf("%s may use only letters, digits, and _-.: (found %q)",
				flagName, character)
		}
	}
	return nil
}

// credentialSource picks the env or file form and refuses both, using
// whether the env flag was passed rather than whether it differs from its
// default — an operator who types the default beside a file must be refused
// too, or the effective credential is invisible in the configuration.
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

// credentialFromEnv reads at use time. A container's environment is fixed at
// start, so this neither rotates nor keeps the value out of process memory;
// use the file form where either is wanted.
func credentialFromEnv(name string) func() (string, error) {
	return func() (string, error) {
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" {
			return "", fmt.Errorf("%s: %w", name, ErrNoCredential)
		}
		return value, nil
	}
}

// credentialFromFile reads at use time, which is what makes a rotating
// projected token or a Secret volume work. It never wraps ErrNoCredential.
func credentialFromFile(path string) func() (string, error) {
	return func() (string, error) {
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("%s is unreadable", path)
		}
		value := strings.TrimSpace(string(raw))
		if value == "" {
			return "", fmt.Errorf("%s is empty", path)
		}
		return value, nil
	}
}
