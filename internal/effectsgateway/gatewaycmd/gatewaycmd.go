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

// Package gatewaycmd is cmd/shoal-gateway (#391, PR6): it composes the
// effects gateway's worker from its flags and runs it, and it is the
// operator's tool for the unrecorded-report log.
//
// It is a package rather than the command's main so the end-to-end test in
// cmd/shoal-explore-web can run the command's own composition, in-process,
// against the real explorer handlers.
package gatewaycmd

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/phrocker/shoal-oss/internal/effectsgateway"
	"github.com/phrocker/shoal-oss/internal/roleops"
)

// Exit codes. Each outcome an operator or the kubelet may need to tell apart
// has its own.
const (
	// ExitOK: the drain finished with nothing left in hand.
	ExitOK = 0
	// ExitFailure: startup refused, or the worker failed.
	ExitFailure = 1
	// ExitUsage: the command line itself was wrong.
	ExitUsage = 2
	// ExitDrainAbandoned: the drain bound ran out with work unfinished
	// (effectsgateway.ErrDrainAbandoned). Every abandoned run whose request
	// may have reached the target is in the unrecorded log.
	ExitDrainAbandoned = 3
	// ExitHardStop: a second signal stopped the gateway without draining,
	// and every sent request is accounted for: on the record, or written to
	// the unrecorded log. Unsent claims lapse and are re-claimed. A hard stop
	// whose write failed exits ExitFailure instead.
	ExitHardStop = 4
)

// Env is what the command reads from its process. Main fills nothing in;
// cmd/shoal-gateway passes the real process's.
type Env struct {
	Stdout io.Writer
	Stderr io.Writer
	// Signals delivers SIGTERM and SIGINT. The first drains; the second is
	// a hard stop. Nil means no signal ever arrives.
	Signals <-chan os.Signal
	// Clock is the worker's clock, and the dispatch client's. Nil is the
	// system clock; a test passes the explorer harness's.
	Clock effectsgateway.WorkerClock
	// Started, when set, is called once the worker is about to run, with
	// the health listener's address ("" without one). A test seam.
	Started func(healthAddress string)
}

type systemClock struct{}

func (systemClock) Now() time.Time                         { return time.Now() }
func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

const usage = `Usage:
  shoal-gateway run [flags]
      Run the effects gateway. See -h for the flags.
  shoal-gateway unrecorded list -unrecorded-dir DIR
      Print the held unrecorded reports, one JSON object per line.
  shoal-gateway unrecorded ack -unrecorded-dir DIR (ACTION_ID[:FENCE]... | -all)
      Clear reports the operator has reconciled.
  shoal-gateway grace-period [-operation-timeout T] [-plane-timeout P]
      Print the minimum terminationGracePeriodSeconds for T and P.

The unrecorded subcommands take the directory's lock, so they refuse while a
gateway runs on that directory.
`

// Main runs one command line and returns the process's exit code.
func Main(args []string, env Env) int {
	if env.Stdout == nil {
		env.Stdout = io.Discard
	}
	if env.Stderr == nil {
		env.Stderr = io.Discard
	}
	if env.Clock == nil {
		env.Clock = systemClock{}
	}
	if len(args) == 0 {
		fmt.Fprint(env.Stderr, usage)
		return ExitUsage
	}
	switch args[0] {
	case "run":
		return Run(args[1:], env)
	case "unrecorded":
		return unrecorded(args[1:], env)
	case "grace-period":
		return gracePeriod(args[1:], env)
	case "-h", "-help", "--help", "help":
		fmt.Fprint(env.Stdout, usage)
		return ExitOK
	}
	fmt.Fprintf(env.Stderr, "shoal-gateway: unknown command %q\n\n%s", args[0], usage)
	return ExitUsage
}

func failf(env Env, code int, format string, values ...any) int {
	fmt.Fprintf(env.Stderr, "shoal-gateway: "+format+"\n", values...)
	return code
}

// gateway is one run's shared state: what the signal handler, the health
// surface and the run itself all read.
type gateway struct {
	mu       sync.Mutex
	worker   *effectsgateway.Worker
	log      *effectsgateway.UnrecordedLog
	stopped  bool
	hardStop bool
	grace    int64
}

// readiness is the worker's own answer once it exists, "starting" before,
// and "stopped" after Run returns.
func (g *gateway) readiness() (bool, effectsgateway.NotReadyReason) {
	g.mu.Lock()
	worker, stopped := g.worker, g.stopped
	g.mu.Unlock()
	switch {
	case stopped:
		return false, effectsgateway.NotReadyStopped
	case worker == nil:
		return false, effectsgateway.NotReadyStarting
	}
	return worker.Ready()
}

// Run is `shoal-gateway run`. The order is the contract:
//
//  1. Parse and validate every flag; nothing has touched the network.
//  2. Open the unrecorded log, which takes the directory's lock. A second
//     gateway on the directory stops here, before any network I/O, so two
//     replicas never both claim.
//  3. Build the dispatch client with the executor credential, bind it to
//     -executor-ref, and resolve the gateway's own descriptor. A refused or
//     unavailable resolve is a refused start.
//  4. Check the descriptor against the route table (VerifyDescriptor), and
//     the attestation flags against what its actions require. Present the
//     attestation when any action requires it.
//  5. Run the worker. The first SIGTERM or SIGINT drains within DrainBound;
//     a second is a hard stop.
func Run(args []string, env Env) int {
	if env.Clock == nil {
		env.Clock = systemClock{}
	}
	config, err := effectsgateway.ParseFlags(args, env.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return ExitOK
	}
	if err != nil {
		return failf(env, ExitUsage, "%v", err)
	}
	g := &gateway{grace: config.GracePeriodSeconds()}

	log, err := effectsgateway.OpenUnrecordedLog(config.UnrecordedDir, env.Clock.Now)
	if err != nil {
		return failf(env, ExitFailure, "-unrecorded-dir: %v", err)
	}
	defer log.Close()
	g.log = log

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopSignals := g.watchSignals(env.Signals, cancel)
	defer stopSignals()

	healthAddress := ""
	if config.HealthAddress != "" {
		listener, err := net.Listen("tcp", config.HealthAddress)
		if err != nil {
			return failf(env, ExitFailure, "-health-address: the listener cannot be opened")
		}
		healthAddress = listener.Addr().String()
		server := &http.Server{Handler: g.operationsHandler(), ReadHeaderTimeout: 5 * time.Second}
		served := make(chan struct{})
		go func() {
			defer close(served)
			_ = server.Serve(listener)
		}()
		defer func() {
			shutdown, done := context.WithTimeout(context.Background(), 2*time.Second)
			defer done()
			if server.Shutdown(shutdown) != nil {
				_ = server.Close()
			}
			<-served
		}()
	}

	worker, err := g.compose(ctx, config, log, env)
	if err != nil {
		return failf(env, ExitFailure, "%v", err)
	}
	fmt.Fprintf(env.Stdout, "shoal-gateway: serving capability %s of agent %s as executor %s; "+
		"terminationGracePeriodSeconds must be at least %d\n",
		config.Capability, config.AgentIDEncoded, config.ExecutorRef, g.grace)

	g.mu.Lock()
	g.worker = worker
	hard := g.hardStop
	g.mu.Unlock()
	if hard {
		worker.HardStop()
	}
	if env.Started != nil {
		env.Started(healthAddress)
	}
	err = worker.Run(ctx)
	g.mu.Lock()
	g.stopped = true
	hard = g.hardStop
	g.mu.Unlock()
	// A drain that finished, or gave up, before a second signal's hard stop took
	// effect is reported as what it was.
	switch {
	case err == nil:
		return ExitOK
	case errors.Is(err, effectsgateway.ErrUnrecordedUnwritten):
		// Ahead of the hard stop: exit 4 promises every sent request is
		// accounted for, and here one is not.
		return failf(env, ExitFailure, "%v; an effect that may have happened is on no record "+
			"and in no log: reconcile from the gateway's dispatch_error events", err)
	case errors.Is(err, effectsgateway.ErrDrainAbandoned):
		return failf(env, ExitDrainAbandoned, "%v; %d reports await reconciliation in the "+
			"unrecorded log", err, log.Len())
	case hard:
		return failf(env, ExitHardStop, "stopped by a second signal without draining; %d reports await "+
			"reconciliation in the unrecorded log", log.Len())
	default:
		return failf(env, ExitFailure, "%v", err)
	}
}

// watchSignals: the first signal cancels ctx, which drains; every later one
// is a hard stop. The returned function stops watching.
func (g *gateway) watchSignals(signals <-chan os.Signal, drain context.CancelFunc) func() {
	if signals == nil {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		received := 0
		for {
			select {
			case <-done:
				return
			case _, ok := <-signals:
				if !ok {
					return
				}
				received++
				if received == 1 {
					drain()
					continue
				}
				g.mu.Lock()
				g.hardStop = true
				worker := g.worker
				g.mu.Unlock()
				if worker != nil {
					worker.HardStop()
				}
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}

// compose builds the worker. Everything here after the lock is the first
// network I/O the gateway does.
func (g *gateway) compose(
	ctx context.Context, config *effectsgateway.Config, log *effectsgateway.UnrecordedLog, env Env,
) (*effectsgateway.Worker, error) {
	client, err := effectsgateway.NewDispatchClient(config.DispatchURL,
		effectsgateway.NewExplorerClient(config.PlaneTimeout), config.DispatchCredential,
		env.Clock.Now)
	if err != nil {
		return nil, err
	}
	bound, err := client.BindExecutor(config.ExecutorRef)
	if err != nil {
		return nil, fmt.Errorf("-executor-ref: %v", err)
	}
	requestID, err := effectsgateway.NewRequestID(rand.Reader)
	if err != nil {
		return nil, err
	}
	resolveCtx, done := context.WithTimeout(ctx, config.PlaneTimeout)
	descriptor, err := bound.Resolve(resolveCtx, config.AgentID, effectsgateway.RequestContext{
		RequestID: requestID, ReasonCode: "gateway_startup",
		Deadline: env.Clock.Now().Add(config.PlaneTimeout).UTC(),
	})
	done()
	if err != nil {
		return nil, fmt.Errorf("resolving descriptor %s: %v; refusing to start",
			config.AgentIDEncoded, err)
	}
	actions, ok := descriptor.Actions(config.Capability)
	if !ok {
		return nil, fmt.Errorf("descriptor %s does not declare capability %s; refusing to start",
			config.AgentIDEncoded, config.Capability)
	}
	if err := effectsgateway.VerifyDescriptor(config.Routes, actions); err != nil {
		return nil, fmt.Errorf("the route table does not match descriptor %s: %v; refusing to start",
			config.AgentIDEncoded, err)
	}
	requiresAttestation := effectsgateway.AttestationRequirements(descriptor, config.Capability)
	anyRequired := false
	for _, action := range config.Routes.Actions() {
		anyRequired = anyRequired || requiresAttestation(action)
	}
	attestationConfigured := config.AttestationStatementFile != ""
	switch {
	case anyRequired && !attestationConfigured:
		return nil, errors.New("an action of the capability requires attestation and " +
			"-attestation-statement-file is not set; refusing to start")
	case !anyRequired && attestationConfigured:
		return nil, errors.New("-attestation-statement-file is set but no action of the " +
			"capability requires attestation; a presentation nothing relies on suggests " +
			"the descriptor was meant to")
	case anyRequired:
		attestCtx, done := context.WithTimeout(ctx, config.PlaneTimeout)
		_, err := bound.PresentAttestation(attestCtx, config.AttestationStatementFile,
			config.AttestationKeyFile)
		done()
		if err != nil {
			return nil, fmt.Errorf("presenting the attestation: %v; refusing to start", err)
		}
	}

	policy, err := effectsgateway.NewEgressPolicy(config.TargetBaseURL, config.TargetAllowPrivate, nil)
	if err != nil {
		return nil, fmt.Errorf("-target-base-url: %v", err)
	}
	if certificate := config.TargetTLS.ClientCertificate; certificate != nil {
		certificate.OnReloadFailure(func() {
			fmt.Fprintln(env.Stderr, "shoal-gateway: -target-client-cert-file and "+
				"-target-client-key-file cannot be reloaded as a pair; presenting the last "+
				"pair that loaded until they can")
		})
	}
	target, err := effectsgateway.NewTargetClientTLS(policy, targetHeaderBytes, config.TargetTLS)
	if err != nil {
		return nil, err
	}
	binder, err := effectsgateway.NewBinder(config.TargetBaseURL, config.IdempotencyHeader)
	if err != nil {
		return nil, fmt.Errorf("-target-base-url: %v", err)
	}
	return effectsgateway.NewWorker(effectsgateway.WorkerConfig{
		Dispatch: bound, Target: target, Binder: binder, Routes: config.Routes,
		TargetAuthorization: config.TargetAuthorization,
		AgentID:             config.AgentID, Capability: config.Capability,
		SurfaceName: config.SurfaceName, Pod: config.Pod,
		IdempotencyRetention: config.IdempotencyRetention,
		ClaimLease:           config.ClaimLease, OperationTimeout: config.OperationTimeout,
		PlaneTimeout: config.PlaneTimeout, MaxResponseBytes: config.MaxResponseBytes,
		PullLimit: config.PullLimit, PullInterval: config.PullInterval,
		Renew: config.Renew, MaxInFlight: config.MaxInFlight,
		RequiresAttestation:      requiresAttestation,
		AttestationStatementFile: config.AttestationStatementFile,
		AttestationKeyFile:       config.AttestationKeyFile,
		Unrecorded:               log,
		Logger:                   effectsgateway.NewLogger(env.Stdout, env.Clock.Now),
		Clock:                    env.Clock,
	})
}

// targetHeaderBytes bounds a target response's headers.
const targetHeaderBytes = 64 << 10

// operationsHandler is /healthz (liveness: the process answers), /readyz
// (the worker's readiness and, when not ready, why), and /metrics.
func (g *gateway) operationsHandler() http.Handler {
	dependencies := roleops.NewDependencies("worker")
	dependencies.SetStarted(true)
	handler := roleops.Handler(dependencies, func(b *strings.Builder) {
		g.mu.Lock()
		worker := g.worker
		g.mu.Unlock()
		inFlight := 0
		if worker != nil {
			inFlight = worker.InFlight()
		}
		fmt.Fprintf(b,
			"# HELP shoal_effects_gateway_unrecorded_entries Reports the explorer did not record, awaiting reconciliation.\n"+
				"# TYPE shoal_effects_gateway_unrecorded_entries gauge\nshoal_effects_gateway_unrecorded_entries %d\n"+
				"# HELP shoal_effects_gateway_in_flight Claims the worker holds.\n"+
				"# TYPE shoal_effects_gateway_in_flight gauge\nshoal_effects_gateway_in_flight %d\n"+
				"# HELP shoal_effects_gateway_grace_period_seconds The minimum terminationGracePeriodSeconds for this configuration.\n"+
				"# TYPE shoal_effects_gateway_grace_period_seconds gauge\nshoal_effects_gateway_grace_period_seconds %d\n",
			g.log.Len(), inFlight, g.grace)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ready, reason := g.readiness()
		detail := string(reason)
		if ready {
			detail = "ready"
		}
		dependencies.Set("worker", ready, detail)
		handler.ServeHTTP(w, r)
	})
}

// gracePeriod is `shoal-gateway grace-period`: the formula from the deploy
// guide, from the same constants the worker spends.
func gracePeriod(args []string, env Env) int {
	flags := flag.NewFlagSet("shoal-gateway grace-period", flag.ContinueOnError)
	flags.SetOutput(env.Stderr)
	operation := flags.Duration("operation-timeout", effectsgateway.DefaultOperationTimeout,
		"Bound T on one request to the target, as given to run")
	plane := flags.Duration("plane-timeout", effectsgateway.DefaultPlaneTimeout,
		"Bound on one call to the explorer, as given to run")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if flags.NArg() != 0 {
		return failf(env, ExitUsage, "unexpected argument %q", flags.Arg(0))
	}
	if *operation <= 0 || *plane <= 0 {
		return failf(env, ExitUsage, "-operation-timeout and -plane-timeout must be positive")
	}
	fmt.Fprintln(env.Stdout, effectsgateway.GracePeriodSeconds(*operation, *plane))
	return ExitOK
}
