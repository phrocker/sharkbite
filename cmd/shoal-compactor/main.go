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

// shoal-compactor is the Bet-1 external compactor binary (Phase C3 of
// platform/shoal/docs/compactor-and-wal-reads-design.md).
//
// Lifecycle (mirrors server/compactor/.../Compactor.java's main loop):
//
//  1. Connect to ZooKeeper, resolve the instance UUID.
//  2. Resolve the CompactionCoordinator address from the manager's
//     ServiceLock data in ZooKeeper (ThriftService.COORDINATOR under
//     /accumulo/<uuid>/managers/lock), the same lookup Java's
//     ExternalCompactionUtil.findCompactionCoordinator performs.
//  3. Dial that address over Thrift, multiplexed under "coordinator",
//     in the manager process. Both the handshake (-connect-timeout) and
//     every subsequent read/write (-rpc-timeout) are bounded, so a
//     manager that accepts the connection and then goes silent cannot
//     pin the pool to a dead primary.
//  4. Loop:
//     a. Generate a fresh externalCompactionId (ECID).
//     b. Call getCompactionJob(group, host:port, ecid).
//     c. If the returned job has no ECID set, sleep and retry.
//     d. Otherwise: capability-gate the job, resolve effective table and
//     storage configuration, execute it, durably publish the temporary
//     output, and call the manager-authoritative completion RPC.
//
// The coordinator address is re-resolved before every connection
// attempt, so a manager failover is tolerated without a restart: while
// no manager holds the lock the lookup returns
// zk.ErrCoordinatorUnavailable and the loop backs off; once the new
// primary manager publishes its descriptors the loop dials the new
// address; a wedged connection is dropped once its RPC timeout fires,
// which is what lets a mid-poll failover be picked up at all.
// Discovery never elects a coordinator — it only reads the
// address the manager itself published, so the manager stays the sole
// authority over which process coordinates compactions.
//
// Java-side completion boundary:
//
// The Accumulo manager owns metadata commits. After a compaction
// produces an output RFile, the new file must be inserted into
// accumulo.metadata for the tablet and the inputs must be dereferenced,
// atomically and under the manager's constraint-enforcement /
// accumulo.root write authority. Today the JVM external compactor
// reaches this via coordinator.compactionCompleted(...) which the
// manager wires to its Ample API.
//
// Inspection of Accumulo 4's manager implementation proves that existing
// coordinator.compactionCompleted(ecid, extent, stats) is sufficient. The
// coordinator reloads the CompactionMetadata stored under the ECID, which
// already contains the exact input references, temporary output path, kind,
// and FateId, then seeds RenameCompactionFile/CommitCompaction. Resending
// those authoritative fields would be redundant and weaker than using the
// manager's stored assignment. Shoal never writes metadata or ZooKeeper.
//
// The executor and completion adapter live in internal/compactexec. For every
// job it is handed, the worker decides up front whether shoal could
// reproduce that compaction cell-for-cell: internal/compactjob
// translates the assignment into an executable plan (inputs, iterator
// stack, output encoding) and refuses anything it cannot reproduce —
// a row-fenced input file, an iterator shoal has not ported, or an output
// feature/storage volume it cannot reproduce. Unsupported jobs are released
// with a class naming the exact reason so a Java compactor can pick them up.
//
// Translation is the gate that keeps execution honest. A durable local
// journal fences the single compactionCompleted attempt. After a timeout or
// restart, the worker reconciles the ECID through the coordinator's running
// and completed maps, retaining the temporary output while manager acceptance
// remains ambiguous.
//
// That is a bounded attempt, not a guarantee, and it has exactly two
// exceptions: a job whose own extent the coordinator could not act on
// (see unreleasableReason), and a hand-back that exhausts the budget.
// Both are logged at error level and left to the coordinator's
// dead-compaction sweep, which is the mechanism Accumulo already relies
// on for a compactor that dies mid-job. What shoal never does is keep an
// assignment silently, or take work it cannot reproduce.
package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/apache/thrift/lib/go/thrift"
	"github.com/google/uuid"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/compactexec"
	"github.com/phrocker/shoal-oss/internal/compactjob"
	"github.com/phrocker/shoal-oss/internal/cred"
	"github.com/phrocker/shoal-oss/internal/managerclient"
	"github.com/phrocker/shoal-oss/internal/namespaces"
	"github.com/phrocker/shoal-oss/internal/protocol"
	"github.com/phrocker/shoal-oss/internal/roleops"
	"github.com/phrocker/shoal-oss/internal/storage/hdfs"
	"github.com/phrocker/shoal-oss/internal/tablenames"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/client"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/compactioncoordinator"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/security"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/tabletserver"
	"github.com/phrocker/shoal-oss/internal/tlsserver"
	"github.com/phrocker/shoal-oss/internal/transportpool"
	"github.com/phrocker/shoal-oss/internal/tserver"
	"github.com/phrocker/shoal-oss/internal/tserverprocess"
	"github.com/phrocker/shoal-oss/internal/zk"
)

var version = "dev"

// ecidPrefix matches Accumulo's ExternalCompactionId format. The Java
// generator produces "ECID-" + UUID; shoal does the same so logs and
// metadata are interchangeable across the two compactor pools.
const ecidPrefix = compactjob.ECIDPrefix

// maxJobsPerDrain caps how many jobs one connection accepts before the loop
// returns to discovery/backoff, preventing a stream of refusals from spinning.
const maxJobsPerDrain = 4

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	advertiseAddr := flag.String("advertise", "", "host:port the coordinator records as this compactor's address (e.g. POD_IP:9810). REQUIRED.")
	listenAddr := flag.String("listen", "", "CompactorService listen address; defaults to -advertise")
	groupName := flag.String("group", "shoal_default", "compactor resource-group name; coordinator routes jobs by group")
	coordinatorAddr := flag.String("coordinator", "", "host:port override for the manager's CompactionCoordinator. Default (empty): discover it from the manager's ServiceLock data in /accumulo/<uuid>/managers/lock (ThriftService.COORDINATOR) and re-resolve it across manager failover.")
	zkServers := flag.String("zk", "", "comma-separated ZK quorum")
	instanceName := flag.String("instance", "accumulo", "Accumulo instance name")
	instanceSecret := flag.String("instance-secret", "", "ZooKeeper instance secret (prefer ACCUMULO_INSTANCE_SECRET)")
	accVersion := flag.String("accumulo-version", "4.0.0-SNAPSHOT", "server major.minor must match")
	user := flag.String("user", "root", "principal for the coordinator RPC (root-equivalent — same trust path Java compactor uses)")
	password := flag.String("password", "", "password (prefer SHOAL_PASSWORD env)")
	systemPrincipal := flag.String("system-principal", "!SYSTEM", "manager system principal")
	systemToken := flag.String("system-token-base64", "", "base64 system token (prefer SHOAL_SYSTEM_TOKEN_BASE64)")
	systemTokenClass := flag.String("system-token-class", "org.apache.accumulo.server.security.SystemCredentials$SystemToken", "system token class")
	zkTimeout := flag.Duration("zk-timeout", 30*time.Second, "ZK session timeout")
	connectTimeout := flag.Duration("connect-timeout", cclient.DefaultConnectTimeout, "cap on the TCP handshake to the coordinator; a manager that is unreachable at the network level fails fast so the address can be re-resolved")
	rpcTimeout := flag.Duration("rpc-timeout", cclient.DefaultRPCTimeout, "cap on each coordinator read/write; bounds getCompactionJob against a manager that accepts the connection and then goes silent (Java's general.rpc.timeout)")
	minWait := flag.Duration("min-wait", 1*time.Second, "minimum sleep when the coordinator has no job for this group")
	maxWait := flag.Duration("max-wait", 30*time.Second, "maximum sleep when idle (backoff cap)")
	releaseTimeout := flag.Duration("release-timeout", 15*time.Second, "best-effort budget for handing an accepted job back to the coordinator, including retries. Applied even during shutdown, so a job shoal accepted is returned promptly instead of waiting out the coordinator's dead-compaction sweep. If the budget runs out the failure is logged and the slot is left to that sweep.")
	maxInputFiles := flag.Int("max-input-files", compactjob.DefaultMaxInputFiles, "refuse jobs with more input files than this (0 = no limit); the composer merges every input in one pass")
	maxInputBytes := flag.Int64("max-input-bytes", compactjob.DefaultMaxTotalInputBytes, "refuse jobs whose declared inputs total more than this many bytes (0 = no limit); the composer reads whole RFile images into memory, so this bounds the read side only — see -max-output-bytes for the write side")
	maxOutputBytes := flag.Int64("max-output-bytes", compactjob.DefaultMaxOutputBytes, "abandon a compaction whose output image grows past this many bytes (0 = no limit); the output is retained in memory and is not bounded by the input total, since compressed inputs rewritten with codec \"none\" and stacks that emit extra cells both expand")
	hdfsNamenode := flag.String("hdfs-namenode", os.Getenv("SHOAL_HDFS_NAMENODE"), "HDFS namenode authority; defaults to SHOAL_HDFS_NAMENODE")
	stateFile := flag.String("state-file", filepath.Join(".shoal-compactor", "completion.json"), "durable completion-reconciliation journal")
	cancelInterval := flag.Duration("cancel-interval", time.Second, "interval for observing coordinator cancellation (0 disables)")
	reconcileGrace := flag.Duration("completion-reconcile-grace", 2*time.Minute, "minimum age before an ambiguous completion absent from both coordinator maps may be cleaned up")
	metricsAddress := flag.String("metrics-address", "", "HTTP health/readiness/metrics listener; empty disables")
	logLevel := flag.String("log-level", "info", "slog level: debug, info, warn, error")
	shutdownTimeout := flag.Duration("shutdown-timeout", 30*time.Second, "bounded listener shutdown budget after job cancellation and hand-back")
	tlsCert := flag.String("tls-cert", "", "server TLS certificate for CompactorService and operations")
	tlsKey := flag.String("tls-key", "", "server TLS private key")
	tlsClientCA := flag.String("tls-client-ca", "", "client CA enabling mutual TLS")
	flag.Parse()

	if *showVersion {
		fmt.Println("shoal-compactor", version)
		return
	}

	level := parseLogLevel(*logLevel)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	if *advertiseAddr == "" {
		die("shoal-compactor: -advertise is required (the address coordinator records on the running-compaction znode)")
	}
	if *zkServers == "" {
		die("shoal-compactor: -zk is required")
	}
	if *password == "" {
		*password = os.Getenv("SHOAL_PASSWORD")
	}
	if *instanceSecret == "" {
		*instanceSecret = os.Getenv("ACCUMULO_INSTANCE_SECRET")
	}
	if *systemToken == "" {
		*systemToken = os.Getenv("SHOAL_SYSTEM_TOKEN_BASE64")
	}
	if *tlsCert == "" {
		*tlsCert = os.Getenv("SHOAL_TLS_CERT")
	}
	if *tlsKey == "" {
		*tlsKey = os.Getenv("SHOAL_TLS_KEY")
	}
	if *tlsClientCA == "" {
		*tlsClientCA = os.Getenv("SHOAL_TLS_CLIENT_CA")
	}
	if *password == "" {
		die("shoal-compactor: password required (-password or SHOAL_PASSWORD env)")
	}
	if *instanceSecret == "" {
		die("shoal-compactor: instance secret required (-instance-secret or ACCUMULO_INSTANCE_SECRET env)")
	}
	token, decodeErr := base64.StdEncoding.DecodeString(*systemToken)
	if decodeErr != nil || len(token) == 0 {
		die("shoal-compactor: invalid empty -system-token-base64: %v", decodeErr)
	}
	if *releaseTimeout <= 0 {
		die("shoal-compactor: -release-timeout must be positive (a job shoal accepted has to be released)")
	}
	if err := validateLimits(*maxInputFiles, *maxInputBytes, *maxOutputBytes); err != nil {
		die("shoal-compactor: %v", err)
	}
	if *cancelInterval < 0 {
		die("shoal-compactor: -cancel-interval must not be negative")
	}
	if *reconcileGrace <= 0 {
		die("shoal-compactor: -completion-reconcile-grace must be positive")
	}
	if *shutdownTimeout <= 0 {
		die("shoal-compactor: -shutdown-timeout must be positive")
	}
	var (
		tlsConfig *tls.Config
		err       error
	)
	if *tlsCert != "" || *tlsKey != "" || *tlsClientCA != "" {
		if *tlsCert == "" || *tlsKey == "" {
			die("shoal-compactor: -tls-cert and -tls-key must be set together")
		}
		tlsConfig, err = tlsserver.Build(*tlsCert, *tlsKey, *tlsClientCA)
		if err != nil {
			die("shoal-compactor: TLS configuration: %v", err)
		}
	}

	coordinatorSource := *coordinatorAddr
	if coordinatorSource == "" {
		coordinatorSource = "<zk-discovery>"
	}
	logger.Info("shoal-compactor startup",
		slog.String("version", version),
		slog.String("group", *groupName),
		slog.String("coordinator", coordinatorSource),
		slog.String("advertise", *advertiseAddr),
		slog.String("zk", *zkServers),
		slog.String("instance", *instanceName),
	)

	servers := strings.Split(*zkServers, ",")
	loc, err := zk.NewWithAuth(servers, *instanceName, *zkTimeout, *instanceSecret)
	if err != nil {
		die("shoal-compactor: zk.New: %v", err)
	}
	defer loc.Close()
	logger.Info("zk connected", slog.String("instance_id", loc.InstanceID()))

	creds := cred.NewPasswordCreds(*user, *password, loc.InstanceID())
	systemCredentials := &security.TCredentials{
		Principal:      *systemPrincipal,
		TokenClassName: *systemTokenClass,
		Token:          append([]byte(nil), token...),
		InstanceId:     loc.InstanceID(),
	}

	hdfsBackend, err := hdfs.New(*hdfsNamenode)
	if err != nil {
		die("shoal-compactor: hdfs.New: %v", err)
	}
	defer hdfsBackend.Close()

	pool, err := transportpool.New(transportpool.Config{IdleTimeout: 30 * time.Second, MaxIdlePerEndpoint: 2})
	if err != nil {
		die("shoal-compactor: transport pool: %v", err)
	}
	manager, err := managerclient.NewPooled(pool, loc.InstanceID(), *accVersion, creds, *connectTimeout)
	if err != nil {
		die("shoal-compactor: manager client: %v", err)
	}
	defer manager.Close()

	namespaceNames := namespaces.NewResolver(loc)
	tableNames := tablenames.NewResolver(loc, namespaceNames)
	limits := compactjob.Limits{
		MaxInputFiles:      *maxInputFiles,
		MaxTotalInputBytes: *maxInputBytes,
		MaxOutputBytes:     *maxOutputBytes,
	}
	metrics := &workerMetrics{}
	metrics.storageReady.Store(true)
	metrics.accepting.Store(true)
	if err := os.MkdirAll(filepath.Dir(*stateFile), 0o700); err != nil {
		die("shoal-compactor: completion journal directory: %v", err)
	}
	metrics.journalReady.Store(true)
	role := &compactorRole{auth: tserverprocess.ExactAuthenticator{
		Identities: []*security.TCredentials{creds, systemCredentials},
	}}
	jobWorker := &worker{
		logger:         logger,
		creds:          creds,
		config:         effectiveTableOptions{locator: loc, names: tableNames, manager: manager, limits: limits},
		store:          compactexec.BackendStore{Backend: hdfsBackend},
		journal:        &fileCompletionJournal{path: *stateFile},
		cancelEvery:    *cancelInterval,
		cleanupTimeout: *releaseTimeout,
		reconcileGrace: *reconcileGrace,
		limits:         limits,
		metrics:        metrics,
		validateStore:  hdfsPlanValidator(*hdfsNamenode),
		role:           role,
	}
	jobWorker.newExecutor = func(reporter compactexec.Reporter) (executor, error) {
		return compactexec.New(jobWorker.store, compactexec.Options{Reporter: reporter, Logger: logger})
	}

	// An explicit -coordinator pins the address (useful for debugging a
	// specific manager); otherwise every dial attempt re-reads the
	// manager's published COORDINATOR descriptor.
	var resolver coordinatorResolver = zkCoordinatorResolver{locator: loc}
	if *coordinatorAddr != "" {
		logger.Warn("coordinator address pinned by -coordinator; ZK discovery and failover follow are disabled",
			slog.String("coordinator", *coordinatorAddr))
		resolver = staticCoordinatorResolver(*coordinatorAddr)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if *listenAddr == "" {
		*listenAddr = *advertiseAddr
	}
	multiplexed := thrift.NewTMultiplexedProcessor()
	multiplexed.RegisterProcessor("compactor", compactioncoordinator.NewCompactorServiceProcessor(role))
	transportFactory := thrift.NewTFramedTransportFactoryConf(
		thrift.NewTBufferedTransportFactory(8192),
		&thrift.TConfiguration{},
	)
	var serverSocket thrift.TServerTransport
	if tlsConfig != nil {
		serverSocket, err = thrift.NewTSSLServerSocket(*listenAddr, tlsConfig.Clone())
	} else {
		serverSocket, err = thrift.NewTServerSocket(*listenAddr)
	}
	if err != nil {
		die("shoal-compactor: CompactorService socket %s: %v", *listenAddr, err)
	}
	roleServer := thrift.NewTSimpleServer4(
		multiplexed,
		serverSocket,
		transportFactory,
		protocol.NewServerFactory(loc.InstanceID(), *accVersion),
	)
	if err := roleServer.Listen(); err != nil {
		die("shoal-compactor: listen CompactorService %s: %v", *listenAddr, err)
	}
	roleDone := make(chan error, 1)
	go func() {
		err := roleServer.Serve()
		roleDone <- err
		if err != nil && !errors.Is(err, net.ErrClosed) {
			logger.Error("CompactorService failed", slog.String("err", err.Error()))
			cancel()
		}
	}()
	metrics.roleServiceReady.Store(true)

	var operations *roleops.Server
	if *metricsAddress != "" {
		operations, err = roleops.Start(*metricsAddress, workerOperationsHandler(metrics), tlsConfig)
		if err != nil {
			die("shoal-compactor: operations listener: %v", err)
		}
		go func() {
			<-operations.Done()
			if err := operations.Err(); err != nil {
				logger.Error("metrics server failed", slog.String("err", err.Error()))
				cancel()
			}
		}()
	}

	sharedSession, err := loc.SharedSession()
	if err != nil {
		die("shoal-compactor: shared ZooKeeper session: %v", err)
	}
	lockPath, err := tserver.CompactorLockPath(loc.InstancePath(), *groupName, *advertiseAddr)
	if err != nil {
		die("shoal-compactor: ServiceLock path: %v", err)
	}
	serviceLock, err := tserver.NewServiceLock(
		zk.LockSession{SharedSession: sharedSession},
		tserver.ServiceLockOptions{Path: lockPath},
	)
	if err != nil {
		die("shoal-compactor: ServiceLock: %v", err)
	}
	lockData, err := tserver.CompactorLockData(
		serviceLock.UUID(), *advertiseAddr, *groupName,
	)
	if err != nil {
		die("shoal-compactor: ServiceLock data: %v", err)
	}
	lockID, err := serviceLock.Acquire(ctx, lockData)
	if err != nil {
		die("shoal-compactor: acquire ServiceLock: %v", err)
	}
	logger.Info("compactor ServiceLock acquired",
		slog.String("lock", lockID.String()),
		slog.String("path", lockPath))
	lockDone := make(chan error, 1)
	go func() {
		maintainErr := serviceLock.Maintain(ctx)
		lockDone <- maintainErr
		if ctx.Err() == nil {
			logger.Error("compactor ServiceLock ended",
				slog.Any("err", maintainErr))
			cancel()
		}
	}()

	stopCh := make(chan os.Signal, 1)
	signal.Notify(stopCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-stopCh
		logger.Info("shutdown signal", slog.String("sig", sig.String()))
		metrics.accepting.Store(false)
		cancel()
	}()

	dialOpts := cclient.DialOptions{
		ConnectTimeout: *connectTimeout,
		RPCTimeout:     *rpcTimeout,
	}
	jobWorker.isRunning = func(ctx context.Context, ecid string) (bool, error) {
		conn, err := redialCoordinator(ctx, pollConfig{
			resolver: resolver,
			dial: func(ctx context.Context, addr, instanceID, accumuloVersion string) (coordinatorConn, error) {
				return dialCoordinator(ctx, addr, instanceID, accumuloVersion, dialOpts)
			},
			instanceID: loc.InstanceID(), accumuloVersion: *accVersion,
		})
		if err != nil {
			return false, err
		}
		defer conn.Close()
		running, err := conn.Raw().GetRunningCompactions(ctx, client.NewTInfo(), creds)
		return containsCompaction(running, ecid), err
	}

	runPollLoop(ctx, logger, pollConfig{
		resolver: resolver,
		dial: func(ctx context.Context, addr, instanceID, accumuloVersion string) (coordinatorConn, error) {
			return dialCoordinator(ctx, addr, instanceID, accumuloVersion, dialOpts)
		},
		instanceID:      loc.InstanceID(),
		accumuloVersion: *accVersion,
		groupName:       *groupName,
		advertiseAddr:   *advertiseAddr,
		creds:           creds,
		minWait:         *minWait,
		maxWait:         *maxWait,
		releaseTimeout:  *releaseTimeout,
		worker:          jobWorker,
		metrics:         metrics,
		jobOptions: compactjob.Options{
			Limits: limits,
		},
	})

	if err := serviceLock.Release(); err != nil {
		logger.Warn("release compactor ServiceLock", slog.String("err", err.Error()))
	}
	if maintainErr := <-lockDone; maintainErr != nil && ctx.Err() == nil {
		logger.Warn("compactor ServiceLock maintenance ended",
			slog.String("err", maintainErr.Error()))
	}
	metrics.accepting.Store(false)
	metrics.ready.Store(false)
	metrics.roleServiceReady.Store(false)
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), *shutdownTimeout)
	defer shutdownCancel()
	_ = roleServer.Stop()
	select {
	case <-roleDone:
	case <-shutdownCtx.Done():
		logger.Warn("CompactorService shutdown deadline exceeded")
	}
	if operations != nil {
		if err := operations.Shutdown(shutdownCtx); err != nil {
			logger.Warn("operations shutdown", slog.String("err", err.Error()))
		}
	}
	logger.Info("shoal-compactor exit clean")
}

// coordinatorResolver yields the address of the manager process that
// currently hosts the CompactionCoordinator. Implementations must be
// safe to call repeatedly: the poll loop re-resolves before every
// connection attempt so manager failover is picked up automatically.
type coordinatorResolver interface {
	Address(ctx context.Context) (string, error)
}

// zkCoordinatorResolver reads the address the manager published on its
// ServiceLock znode. It is a pure reader — the manager remains the only
// process that decides where the coordinator lives.
type zkCoordinatorResolver struct {
	locator zk.LockReader
}

func (r zkCoordinatorResolver) Address(ctx context.Context) (string, error) {
	return zk.CoordinatorAddress(ctx, r.locator)
}

// staticCoordinatorResolver serves a fixed operator-supplied address.
type staticCoordinatorResolver string

func (r staticCoordinatorResolver) Address(context.Context) (string, error) {
	if r == "" {
		return "", errors.New("shoal-compactor: empty pinned coordinator address")
	}
	return string(r), nil
}

// coordinatorConn is the subset of *cclient.CoordinatorClient the poll
// loop uses, so tests can drive the loop without a live manager. Raw
// returns the generated service interface rather than the concrete
// client so a fake coordinator can be substituted.
type coordinatorConn interface {
	Raw() compactioncoordinator.CompactionCoordinatorService
	Close() error
}

// coordinatorClientConn adapts *cclient.CoordinatorClient (whose Raw
// returns the concrete generated client) to coordinatorConn.
type coordinatorClientConn struct {
	*cclient.CoordinatorClient
}

func (c coordinatorClientConn) Raw() compactioncoordinator.CompactionCoordinatorService {
	return c.CoordinatorClient.Raw()
}

func dialCoordinator(
	ctx context.Context,
	addr, instanceID, accumuloVersion string,
	opts cclient.DialOptions,
) (coordinatorConn, error) {
	cc, err := cclient.DialCoordinator(ctx, addr, instanceID, accumuloVersion, opts)
	if err != nil {
		return nil, err
	}
	return coordinatorClientConn{CoordinatorClient: cc}, nil
}

type pollConfig struct {
	resolver        coordinatorResolver
	dial            func(ctx context.Context, addr, instanceID, accumuloVersion string) (coordinatorConn, error)
	instanceID      string
	accumuloVersion string
	groupName       string
	advertiseAddr   string
	creds           *security.TCredentials
	minWait         time.Duration
	maxWait         time.Duration
	// releaseTimeout bounds the whole hand-back of one job, retries
	// included. It is deliberately independent of the poll cadence:
	// releasing is the one thing this binary owes the manager.
	releaseTimeout time.Duration
	// jobOptions carries the translation defaults and resource limits
	// applied to every job the coordinator assigns.
	jobOptions compactjob.Options
	worker     *worker
	metrics    *workerMetrics
}

// runPollLoop is the main service loop. It re-dials the coordinator on
// transport errors (matching Java's RetryableThriftCall semantics:
// coordinator restarts are tolerated), and uses exponential backoff
// between idle polls. Every attempt re-resolves the coordinator address
// first, so a manager failover moves the pool to the new primary without
// operator action. The loop only exits when ctx is cancelled.
func runPollLoop(ctx context.Context, logger *slog.Logger, cfg pollConfig) {
	wait := cfg.minWait
	lastAddr := ""
	// recovering tracks whether the previous attempt failed discovery or
	// dial, so the failure backoff is dropped once the pool is talking to
	// a manager again instead of leaking into the idle-poll cadence.
	recovering := false
	for {
		if ctx.Err() != nil {
			return
		}

		addr, err := cfg.resolver.Address(ctx)
		if err != nil {
			if cfg.metrics != nil {
				cfg.metrics.ready.Store(false)
			}
			if ctx.Err() != nil {
				return
			}
			recovering = true
			// zk.ErrCoordinatorUnavailable is the expected steady state
			// during a manager failover: no manager holds the lock, or the
			// new one has not republished its descriptors yet.
			level := slog.LevelWarn
			if errors.Is(err, zk.ErrCoordinatorUnavailable) {
				level = slog.LevelInfo
			}
			logger.Log(ctx, level, "coordinator discovery failed; backing off",
				slog.String("err", err.Error()),
				slog.Duration("retry_in", wait))
			if !sleepCtx(ctx, wait) {
				return
			}
			wait = nextWait(wait, cfg.maxWait)
			continue
		}

		if addr != lastAddr {
			if lastAddr == "" {
				logger.Info("coordinator resolved", slog.String("addr", addr))
			} else {
				logger.Info("coordinator address changed; following manager failover",
					slog.String("previous", lastAddr),
					slog.String("current", addr))
			}
			lastAddr = addr
		}

		cc, err := cfg.dial(ctx, addr, cfg.instanceID, cfg.accumuloVersion)
		if err != nil {
			if cfg.metrics != nil {
				cfg.metrics.ready.Store(false)
			}
			recovering = true
			logger.Warn("coordinator dial failed; backing off",
				slog.String("addr", addr),
				slog.String("err", err.Error()),
				slog.Duration("retry_in", wait))
			if !sleepCtx(ctx, wait) {
				return
			}
			wait = nextWait(wait, cfg.maxWait)
			continue
		}
		if recovering {
			wait = cfg.minWait
			recovering = false
		}
		if cfg.worker != nil {
			pending, reconcileErr := cfg.worker.reconcilePending(ctx, cc.Raw())
			if reconcileErr != nil {
				if cfg.metrics != nil {
					cfg.metrics.ready.Store(false)
				}
				logger.Warn("pending completion reconciliation failed; will reconnect",
					slog.String("err", reconcileErr.Error()))
				_ = cc.Close()
				if !sleepCtx(ctx, wait) {
					return
				}
				wait = nextWait(wait, cfg.maxWait)
				continue
			}
			if pending {
				if cfg.metrics != nil {
					cfg.metrics.ready.Store(false)
				}
				logger.Info("completion remains authoritative-manager pending; not accepting another job")
				_ = cc.Close()
				if !sleepCtx(ctx, wait) {
					return
				}
				wait = nextWait(wait, cfg.maxWait)
				continue
			}
		}
		if cfg.metrics != nil {
			cfg.metrics.ready.Store(true)
		}

		// One connection, drain jobs until the coordinator says "no work"
		// or the transport errors. On any transport failure, drop the
		// connection and reconnect.
		//
		// Thrift transports are not context-aware once a call is in
		// flight: an RPC to a manager that accepted the connection and
		// then went silent only unblocks when the socket timeout fires.
		// Closing the transport from a watcher makes cancellation
		// immediate, so SIGTERM never has to wait out -rpc-timeout.
		watcherDone := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				_ = cc.Close()
			case <-watcherDone:
			}
		}()
		idle := drainCoordinator(ctx, logger, cc, cfg)
		close(watcherDone)
		_ = cc.Close()

		if ctx.Err() != nil {
			return
		}
		if idle {
			if !sleepCtx(ctx, wait) {
				return
			}
			wait = nextWait(wait, cfg.maxWait)
		} else {
			wait = cfg.minWait
		}
	}
}

// drainCoordinator polls one open coordinator connection until either
// (a) it returns no job (idle = true), or (b) a transport error happens
// (idle = false). The bool drives the outer-loop sleep + reconnect.
//
// Accepting a job also ends the drain once maxJobsPerDrain of them have
// been handed back, reported as idle so the outer loop's backoff damps a
// coordinator that keeps offering work shoal declines.
func drainCoordinator(ctx context.Context, logger *slog.Logger, cc coordinatorConn, cfg pollConfig) bool {
	handled := 0
	for {
		if ctx.Err() != nil {
			return false
		}

		ecid := newECID()
		next, err := cc.Raw().GetCompactionJob(
			ctx,
			client.NewTInfo(),
			cfg.creds,
			cfg.groupName,
			cfg.advertiseAddr,
			ecid,
		)
		if err != nil {
			logger.Warn("getCompactionJob failed; will reconnect",
				slog.String("err", err.Error()))
			return false
		}

		job := next.GetJob()
		if job == nil || job.GetExternalCompactionId() == "" {
			// Java's Compactor.java checks !job.isSetExternalCompactionId();
			// the Go generator drops isSet on required fields, so an unset
			// id reaches us as the zero value. Either form (nil job or empty
			// id) means "no work for this group right now".
			logger.Debug("coordinator: no job for group",
				slog.String("group", cfg.groupName),
				slog.Int("compactor_count", int(next.GetCompactorCount())))
			return true
		}

		if job.GetExternalCompactionId() != ecid {
			logger.Error("coordinator handed back mismatched ecid; aborting drain",
				slog.String("expected", ecid),
				slog.String("got", job.GetExternalCompactionId()))
			return false
		}

		usable, released := executeJob(ctx, logger, cc, cfg, job)
		if !released {
			// The assignment is still active on the coordinator: either
			// the hand-back was one it could not process (see
			// unreleasableReason) or the release budget ran out. Asking
			// for another job now would spend a second slot the same way
			// and keep doing so for as long as the coordinator has work,
			// so end the drain and let the outer loop's backoff apply.
			// The connection is closed there either way.
			return true
		}
		if !usable {
			// The connection did not survive the hand-back (or shutdown
			// closed it). Reconnecting is cheaper than discovering that
			// with another doomed getCompactionJob.
			return false
		}

		handled++
		if handled >= maxJobsPerDrain {
			logger.Debug("released the per-drain job budget; backing off",
				slog.Int("jobs", handled),
				slog.String("group", cfg.groupName))
			return true
		}
	}
}

// executeJob capability-gates one assignment, then either runs it through the
// worker or hands it back with a structured refusal. It reports whether cc is
// still usable and whether the slot is no longer safe to poll against.
//
// Supported jobs resolve stable effective table configuration, validate the
// configured HDFS authority, execute, publish, and call compactionCompleted.
// Unsupported or failed jobs call compactionFailed. An ambiguous completion
// is the sole retained-slot case: its durable journal is reconciled before the
// process accepts more work.
//
// What never happens here is a metadata or ZooKeeper write. The manager
// remains the only process that decides a compaction happened.
//
// It reports both whether cc survived and whether the slot actually went
// back, because a job left assigned is the one condition under which
// asking for more work makes things worse.
func executeJob(
	ctx context.Context,
	logger *slog.Logger,
	cc coordinatorConn,
	cfg pollConfig,
	job *tabletserver.TExternalCompactionJob,
) (connUsable, released bool) {
	ecid := job.GetExternalCompactionId()

	plan, err := compactjob.Translate(job, cfg.jobOptions)
	if err != nil {
		refusal := compactjob.RefusalOf(err)
		if refusal == nil {
			// Translate only ever returns refusals; treat anything else as
			// malformed rather than dropping the job on the floor.
			refusal = &compactjob.Refusal{
				Class:  compactjob.ClassMalformedJob,
				Field:  "job",
				Detail: err.Error(),
			}
		}
		logger.Warn("compaction job refused",
			slog.String("ecid", ecid),
			slog.String("extent", compactjob.ExtentString(job.GetExtent())),
			slog.String("class", refusal.Class),
			slog.String("field", refusal.Field),
			slog.String("reason", refusal.Detail))
		return releaseJob(ctx, logger, cc, cfg, job, refusal.Class)
	}

	if cfg.worker == nil {
		logger.Info("compaction job translated; releasing (isolated executor is not wired to this worker)",
			slog.String("ecid", ecid),
			slog.Any("plan", plan))
		return releaseJob(ctx, logger, cc, cfg, job, compactjob.ClassExecutionUnavailable)
	}

	outcome := cfg.worker.process(ctx, cc.Raw(), job)
	switch {
	case outcome.completed:
		logger.Info("compaction completed through manager authority", slog.String("ecid", ecid))
		return true, true
	case outcome.coordinatorReleased:
		logger.Info("compaction cancelled and released by coordinator", slog.String("ecid", ecid))
		return true, true
	case outcome.ambiguous:
		logger.Warn("compaction completion remains ambiguous; slot retained for reconciliation",
			slog.String("ecid", ecid))
		return false, false
	default:
		return releaseJob(ctx, logger, cc, cfg, job, outcome.class)
	}
}

// unreleasableReason reports why the coordinator could not act on a
// hand-back for this job, or "" when the hand-back is worth attempting.
//
// CompactionCoordinator.compactionFailed converts the extent before it
// records anything — KeyExtent.fromThrift(extent) — and fromThrift
// starts with TableId.of(new String(tke.getTable(), UTF_8)). A null
// extent therefore throws inside the handler, and the call comes back as
// an application error no matter how often it is retried. The generated
// writer omits a nil struct field entirely, so the coordinator does see
// exactly that null.
//
// An extent carrying no table id is the quieter version of the same
// problem: it converts, so the RPC succeeds, but TableId.of("") names no
// tablet — the real assignment is never cleared while shoal logs the
// slot as released. Translate already treats every zero-length table id
// as missing, and a decoded Thrift binary can be a non-nil empty slice,
// so this uses the same length test rather than a nil check.
//
// The ECID could be a third way the same call is unanswerable —
// compactionFailed resolves the assignment through
// ExternalCompactionId.of, which throws on anything outside the
// "ECID-<uuid>" grammar — but it cannot be one here. drainCoordinator
// aborts on any job whose id differs from the one it generated, and
// newECID only ever emits the canonical spelling, so every id that
// reaches this point is one Java parses.
//
// The pinned protocol has no hand-back that takes the ECID alone, and
// substituting an extent shoal invented would tell the manager a
// compaction failed on a tablet it was never assigned to. So the slot is
// left to the coordinator's own dead-compaction sweep, and the reason is
// logged rather than buried under retries.
func unreleasableReason(job *tabletserver.TExternalCompactionJob) string {
	extent := job.GetExtent()
	switch {
	case extent == nil:
		return "the assignment carries no extent, and compactionFailed cannot convert a missing one"
	case len(extent.GetTable()) == 0:
		return "the assignment's extent carries no table id, so compactionFailed would name no tablet"
	case compactjob.ExtentBoundsInverted(extent):
		// KeyExtent.fromThrift runs the constructor's
		// "prevEndRow >= endRow" check, and compactionFailed converts the
		// extent before it resolves the id, so the RPC throws every time
		// and the assignment is never cleared. Retrying would spend the
		// whole release budget on a call that cannot succeed.
		return "the assignment's extent has prevEndRow at or after endRow, which KeyExtent.fromThrift rejects before compactionFailed clears the assignment"
	}
	return ""
}

// releaseJob hands one accepted job back to the coordinator so the slot
// is freed and a Java compactor can run it. It reports whether cc is
// still usable afterwards, and whether the slot actually went back.
//
// This is the one RPC this binary owes the manager, so it is the one
// that retries. A job the coordinator handed out stays assigned until it
// hears otherwise or its own dead-compactor sweep notices; both cost the
// tablet a compaction cycle. So:
//
//   - Shutdown does not skip it. ctx is already cancelled by the time a
//     SIGTERM reaches here, and the poll loop's watcher has closed the
//     connection, so the release runs on a detached context with its own
//     budget and dials a fresh connection.
//   - A broken connection does not end it. The address is re-resolved
//     (following a manager failover mid-job) and redialed until the
//     budget runs out.
//   - A silent coordinator does not extend it. Thrift I/O already in
//     flight ignores context cancellation, so the budget is enforced by
//     closing the socket rather than by the RPC's context alone.
//
// The one job it cannot hand back is one whose own extent is missing:
// see unreleasableReason. That case is surfaced, not retried.
//
// If the budget does run out the failure is logged loudly and the job is
// left to the coordinator's own timeout — the alternative, blocking
// shutdown indefinitely, is worse.
func releaseJob(
	ctx context.Context,
	logger *slog.Logger,
	cc coordinatorConn,
	cfg pollConfig,
	job *tabletserver.TExternalCompactionJob,
	class string,
) (connUsable, released bool) {
	ecid := job.GetExternalCompactionId()
	shuttingDown := ctx.Err() != nil
	failureState := compactioncoordinator.TCompactionState_FAILED
	if shuttingDown {
		// The job was accepted before the signal arrived; say so, so the
		// manager log distinguishes "shoal cannot do this" from "shoal
		// went away mid-assignment".
		class = compactjob.ClassShuttingDown
		failureState = compactioncoordinator.TCompactionState_CANCELLED
	} else if class == compactjob.ClassShuttingDown {
		failureState = compactioncoordinator.TCompactionState_CANCELLED
	}

	// A hand-back the coordinator cannot process is worse than no
	// hand-back: retrying it burns the whole release budget, and during
	// shutdown that budget is what bounds how long the process takes to
	// exit. Surface it once, loudly, instead.
	if why := unreleasableReason(job); why != "" {
		logger.Error("compaction slot cannot be released; leaving it to the coordinator's dead-compaction sweep",
			slog.String("ecid", ecid),
			slog.String("class", class),
			slog.String("reason", why))
		return true, false
	}

	// context.WithoutCancel keeps the RPC's deadline ours alone: a
	// cancelled parent must not abort the hand-back it caused.
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.releaseTimeout)
	defer cancel()

	// The first attempt reuses the open connection unless shutdown has
	// already closed it underneath us.
	conn := cc
	connUsable = true
	if shuttingDown {
		conn = nil
	}

	// owned is the connection this function opened, if any; the poll
	// loop owns cc and closes it itself.
	var owned coordinatorConn
	defer func() {
		if owned != nil {
			_ = owned.Close()
		}
	}()

	// releaseCtx bounds the retry loop, but not an RPC already on the
	// wire: cclient's transports are not context-aware once a call is in
	// flight, so a coordinator that accepts the connection and then goes
	// silent only unblocks CompactionFailed when the socket timeout
	// fires — which can outlast -release-timeout and stall shutdown.
	// Closing the socket in use is what actually enforces the budget.
	var (
		mu              sync.Mutex
		active          coordinatorConn
		closedByWatcher bool
		budgetSpent     bool
	)
	// setActive registers the connection an RPC is about to use, and
	// reports whether it is still safe to use it. Registration has to be
	// deadline-aware: the watcher runs once and then exits, so a
	// connection dialled after the budget expired would otherwise never
	// be closed by anyone and would block on the socket timeout instead.
	setActive := func(c coordinatorConn) (ok bool) {
		mu.Lock()
		defer mu.Unlock()
		if budgetSpent && c != nil {
			closedByWatcher = closedByWatcher || c == cc
			_ = c.Close()
			return false
		}
		active = c
		return true
	}
	watcherDone := make(chan struct{})
	watcherExited := make(chan struct{})
	go func() {
		defer close(watcherExited)
		select {
		case <-releaseCtx.Done():
			mu.Lock()
			budgetSpent = true
			if active != nil {
				// Only the caller's connection is reported spent; one
				// this function dialled is closed by its own defer.
				closedByWatcher = closedByWatcher || active == cc
				_ = active.Close()
			}
			mu.Unlock()
		case <-watcherDone:
		}
	}()
	defer func() {
		close(watcherDone)
		<-watcherExited
		if closedByWatcher {
			connUsable = false
		}
	}()

	wait := 100 * time.Millisecond
	for attempt := 1; ; attempt++ {
		if conn == nil {
			fresh, err := redialCoordinator(releaseCtx, cfg)
			if err != nil {
				logger.Warn("release: reconnect failed",
					slog.String("ecid", ecid),
					slog.Int("attempt", attempt),
					slog.String("err", err.Error()))
				if !sleepCtx(releaseCtx, wait) {
					break
				}
				wait = nextWait(wait, time.Second)
				continue
			}
			conn, owned = fresh, fresh
		}

		// Shutdown may have started while the previous attempt was on
		// the wire. The class is the manager's only record of why the
		// slot came back, so re-read it here rather than reporting a
		// stale reason for a job that is now being abandoned.
		if !shuttingDown && ctx.Err() != nil {
			shuttingDown = true
			class = compactjob.ClassShuttingDown
		}

		if !setActive(conn) {
			// The budget expired between the dial and now; the
			// connection is already closed, so there is nothing left to
			// try within this release.
			conn, owned = nil, nil
			break
		}
		err := conn.Raw().CompactionFailed(
			releaseCtx,
			client.NewTInfo(),
			cfg.creds,
			ecid,
			job.GetExtent(),
			class,
			failureState,
		)
		setActive(nil)
		if err == nil {
			logger.Info("compaction slot released to coordinator",
				slog.String("ecid", ecid),
				slog.String("class", class),
				slog.Int("attempt", attempt))
			return connUsable, true
		}

		logger.Warn("release: compactionFailed rpc failed",
			slog.String("ecid", ecid),
			slog.String("class", class),
			slog.Int("attempt", attempt),
			slog.String("err", err.Error()))

		// The transport is suspect now; the next attempt gets a new one.
		// Report the original connection as spent so the caller stops
		// polling on it.
		if conn == cc {
			connUsable = false
		} else if owned != nil {
			_ = owned.Close()
			owned = nil
		}
		conn = nil

		if !sleepCtx(releaseCtx, wait) {
			break
		}
		wait = nextWait(wait, time.Second)
	}

	logger.Error("release: gave up; job stays assigned until the coordinator's own timeout reclaims it",
		slog.String("ecid", ecid),
		slog.String("class", class),
		slog.String("extent", compactjob.ExtentString(job.GetExtent())),
		slog.Duration("budget", cfg.releaseTimeout))
	return connUsable, false
}

// redialCoordinator re-resolves the coordinator and opens a connection
// for a release attempt. Re-resolving (rather than reusing the address
// the job arrived on) is what lets a release land on the new primary
// when the manager failed over while the job was in hand.
func redialCoordinator(ctx context.Context, cfg pollConfig) (coordinatorConn, error) {
	addr, err := cfg.resolver.Address(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve coordinator: %w", err)
	}
	conn, err := cfg.dial(ctx, addr, cfg.instanceID, cfg.accumuloVersion)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	return conn, nil
}

// newECID generates an ExternalCompactionId in Accumulo's canonical
// "ECID-<uuid>" form. The coordinator echoes this back in the job
// (job.ExternalCompactionId), and we verify the echo to catch
// out-of-band assignments.
func newECID() string {
	return ecidPrefix + uuid.NewString()
}

func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

// validateLimits rejects negative resource budgets at startup.
//
// Zero already means "no limit" on every one of these, so a negative
// value can only be a typo — and it is the dangerous kind: both the
// admission check and compaction.Compact treat a non-positive budget as
// unlimited, so `-max-output-bytes -1` would quietly remove the guard
// the operator was trying to tighten.
func validateLimits(maxInputFiles int, maxInputBytes, maxOutputBytes int64) error {
	switch {
	case maxInputFiles < 0:
		return fmt.Errorf("-max-input-files must not be negative (use 0 for no limit), got %d", maxInputFiles)
	case maxInputBytes < 0:
		return fmt.Errorf("-max-input-bytes must not be negative (use 0 for no limit), got %d", maxInputBytes)
	case maxOutputBytes < 0:
		return fmt.Errorf("-max-output-bytes must not be negative (use 0 for no limit), got %d", maxOutputBytes)
	}
	return nil
}

// sleepCtx sleeps for d, returning true if it slept to completion or
// false if ctx was cancelled mid-sleep.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// nextWait doubles d up to a cap of maxWait. Matches Java's
// RetryableThriftCall exponential-backoff semantics.
func nextWait(d, maxWait time.Duration) time.Duration {
	d *= 2
	if d > maxWait {
		return maxWait
	}
	return d
}
