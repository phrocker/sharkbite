// shoal-tserver is the manager-assigned Shoal tablet server.
package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/apache/thrift/lib/go/thrift"

	"github.com/phrocker/shoal-oss/internal/cache"
	"github.com/phrocker/shoal-oss/internal/cred"
	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/hostedingest"
	"github.com/phrocker/shoal-oss/internal/ingestclient"
	"github.com/phrocker/shoal-oss/internal/ingestrouter"
	"github.com/phrocker/shoal-oss/internal/ingestservice"
	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/metadatacas"
	"github.com/phrocker/shoal-oss/internal/protocol"
	"github.com/phrocker/shoal-oss/internal/roleops"
	"github.com/phrocker/shoal-oss/internal/scanserver"
	"github.com/phrocker/shoal-oss/internal/shadow/itercfg"
	"github.com/phrocker/shoal-oss/internal/storage"
	"github.com/phrocker/shoal-oss/internal/storage/azure"
	"github.com/phrocker/shoal-oss/internal/storage/gcs"
	"github.com/phrocker/shoal-oss/internal/storage/hdfs"
	"github.com/phrocker/shoal-oss/internal/storage/local"
	"github.com/phrocker/shoal-oss/internal/storage/s3"
	"github.com/phrocker/shoal-oss/internal/tabletloader"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/security"
	"github.com/phrocker/shoal-oss/internal/tlsserver"
	"github.com/phrocker/shoal-oss/internal/transportpool"
	"github.com/phrocker/shoal-oss/internal/tserver"
	"github.com/phrocker/shoal-oss/internal/tserverprocess"
	"github.com/phrocker/shoal-oss/internal/tserverrpc"
	"github.com/phrocker/shoal-oss/internal/walauthority"
	"github.com/phrocker/shoal-oss/internal/zk"
)

var version = "dev"

type managerResolver struct{ locator zk.LockReader }

func (r managerResolver) ManagerAddress(ctx context.Context) (string, error) {
	return zk.ManagerAddress(ctx, r.locator)
}

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	listen := flag.String("listen", ":9997", "Thrift listen address")
	advertise := flag.String("advertise", "", "manager-dialable host:port (required)")
	group := flag.String("group", tserver.DefaultResourceGroup, "tablet-server resource group")
	zkServers := flag.String("zk", "", "comma-separated ZooKeeper quorum")
	instance := flag.String("instance", "accumulo", "Accumulo instance name")
	instanceSecret := flag.String("instance-secret", "", "ZooKeeper instance secret (prefer ACCUMULO_INSTANCE_SECRET)")
	accVersion := flag.String("accumulo-version", "4.0.0-SNAPSHOT", "Accumulo protocol version")
	user := flag.String("user", "root", "principal for metadata and manager configuration reads")
	password := flag.String("password", "", "password (prefer SHOAL_PASSWORD)")
	systemPrincipal := flag.String("system-principal", "!SYSTEM", "manager system principal")
	systemToken := flag.String("system-token-base64", "", "base64 system token (prefer SHOAL_SYSTEM_TOKEN_BASE64)")
	systemTokenClass := flag.String("system-token-class", "org.apache.accumulo.server.security.SystemCredentials$SystemToken", "system token class")
	storageScheme := flag.String("storage", "gs", "RFile storage: gs, s3, azure, hdfs, or local")
	zkTimeout := flag.Duration("zk-timeout", 30*time.Second, "ZooKeeper session timeout")
	managerPoll := flag.Duration("manager-poll", time.Second, "manager lock observation interval")
	lockVerify := flag.Duration("lock-verify", 5*time.Second, "ServiceLock verification interval")
	drainTimeout := flag.Duration("drain-timeout", 30*time.Second, "scan drain deadline")
	metricsAddress := flag.String("metrics-address", ":9998", "health/readiness/metrics address; empty disables")
	walRoot := flag.String("wal-root", "/var/lib/shoal/wal", "durable local WAL directory")
	mincRoot := flag.String("minc-root", "shoal/minc", "minor-compaction object prefix")
	stateRoot := flag.String("state-root", "/var/lib/shoal/minc-state", "durable minor-compaction state directory")
	flushCells := flag.Int("flush-cells", 1, "memtable cells that trigger minor compaction")
	defaultEmbedding := flag.String("default-embedding", "",
		"embedding state recorded for a minor compaction that declares none: "+
			"no_embeddings, unknown, or has_embeddings:<identity>; empty means unknown")
	enableIngest := flag.Bool("enable-ingest", false, "advertise TABLET_INGEST after all write authorities initialize")
	tlsCert := flag.String("tls-cert", "", "server TLS certificate for Thrift and operations listeners")
	tlsKey := flag.String("tls-key", "", "server TLS private key")
	tlsClientCA := flag.String("tls-client-ca", "", "client CA enabling mutual TLS")
	flag.Parse()

	if *showVersion {
		fmt.Println("shoal-tserver", version)
		return
	}
	*instanceSecret = valueOrEnv(*instanceSecret, "ACCUMULO_INSTANCE_SECRET")
	*password = valueOrEnv(*password, "SHOAL_PASSWORD")
	*systemToken = valueOrEnv(*systemToken, "SHOAL_SYSTEM_TOKEN_BASE64")
	*tlsCert = valueOrEnv(*tlsCert, "SHOAL_TLS_CERT")
	*tlsKey = valueOrEnv(*tlsKey, "SHOAL_TLS_KEY")
	*tlsClientCA = valueOrEnv(*tlsClientCA, "SHOAL_TLS_CLIENT_CA")
	if *advertise == "" || *zkServers == "" || *instanceSecret == "" || *password == "" || *systemToken == "" {
		die("-advertise, -zk, instance secret, password, and system token are required")
	}
	token, err := base64.StdEncoding.DecodeString(*systemToken)
	if err != nil || len(token) == 0 {
		die("invalid empty -system-token-base64: %v", err)
	}
	var tlsConfig *tls.Config
	if *tlsCert != "" || *tlsKey != "" || *tlsClientCA != "" {
		if *tlsCert == "" || *tlsKey == "" {
			die("-tls-cert and -tls-key must be set together")
		}
		tlsConfig, err = tlsserver.Build(*tlsCert, *tlsKey, *tlsClientCA)
		if err != nil {
			die("TLS configuration: %v", err)
		}
	}

	signalCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	loc, err := zk.NewWithAuth(strings.Split(*zkServers, ","), *instance, *zkTimeout, *instanceSecret)
	if err != nil {
		die("ZooKeeper: %v", err)
	}
	defer loc.Close()
	session, err := loc.SharedSession()
	if err != nil {
		die("ZooKeeper shared session: %v", err)
	}
	resolver := managerResolver{locator: loc}
	outbound := cred.NewPasswordCreds(*user, *password, loc.InstanceID())
	systemCredentials := &security.TCredentials{
		Principal: *systemPrincipal, TokenClassName: *systemTokenClass,
		Token: append([]byte(nil), token...), InstanceId: loc.InstanceID(),
	}
	sessionGeneration := strconv.FormatUint(uint64(session.SessionID()), 16)
	if sessionGeneration == "0" {
		die("ZooKeeper session has no identity")
	}
	lockPath, err := tserver.TabletServerLockPath(loc.InstancePath(), *group, *advertise)
	if err != nil {
		die("tablet-server lock path: %v", err)
	}
	lockIDPath, err := tserver.TabletServerLockIDPath(*group, *advertise)
	if err != nil {
		die("tablet-server lock ID path: %v", err)
	}
	pool, err := transportpool.New(transportpool.Config{IdleTimeout: time.Minute, MaxIdlePerEndpoint: 4})
	if err != nil {
		die("transport pool: %v", err)
	}
	defer pool.Close()
	conditionalAPI, err := ingestclient.NewPooled(
		pool, loc.InstanceID(), *accVersion, systemCredentials, 10*time.Second,
	)
	if err != nil {
		die("conditional metadata client: %v", err)
	}
	defer conditionalAPI.Close()

	walker := metadata.NewWalker(loc, outbound, *accVersion).WithLogger(logger)
	tableConfig := itercfg.NewResolver(loc, 0, logger)
	host := tserver.NewHost()
	files, closeStorage, err := openStorage(runCtx, *storageScheme)
	if err != nil {
		die("storage: %v", err)
	}
	defer closeStorage()
	metadataFactory, err := metadatacas.NewFactory(metadatacas.Config{
		Reader: walker, RootLocator: loc, Conditional: conditionalAPI,
		RootStore: session, Host: host, InstancePath: loc.InstancePath(),
		Address: *advertise, Group: *group, Session: sessionGeneration,
	})
	if err != nil {
		die("metadata authority: %v", err)
	}
	loader, err := tabletloader.New(tabletloader.Config{
		Authority: tserverprocess.HostAuthority{
			Host: host, Generation: tabletloader.Generation(sessionGeneration),
		},
		Metadata: tserverprocess.MetadataSource{Locator: walker, Address: *advertise},
		Config:   tserverprocess.ZKConfigSource{Resolver: tableConfig},
		Files:    tabletloader.StrictReferenceResolver{},
		Logs:     tabletloader.StrictReferenceResolver{},
		Retry:    tabletloader.DefaultRetryPolicy(),
	})
	if err != nil {
		die("tablet loader: %v", err)
	}
	mincDefaultEmbedding, err := embeddingspace.Parse(*defaultEmbedding)
	if err != nil {
		die("-default-embedding: %v", err)
	}
	tabletFactory, err := hostedingest.NewFactory(hostedingest.Config{
		Host: host, ServerAddress: *advertise,
		WALRoot: *walRoot, MincRoot: *mincRoot, StateRoot: *stateRoot,
		WALStore: walauthority.NewLocalStore(), Outputs: files,
		Metadata: metadataFactory, FlushCells: *flushCells,
		DefaultEmbedding: mincDefaultEmbedding,
		FlushID: func(ctx context.Context, tableID string) (int64, error) {
			raw, _, err := session.Get(path.Join(
				loc.InstancePath(), "tables", tableID, "flush-id",
			))
			if err != nil {
				return 0, err
			}
			flushID, err := strconv.ParseInt(string(raw), 10, 64)
			if err != nil {
				return 0, fmt.Errorf("parse flush ID for table %s: %w", tableID, err)
			}
			return flushID, nil
		},
	})
	if err != nil {
		die("hosted ingest: %v", err)
	}
	store, err := tserverprocess.NewWritableStore(loader, tabletFactory)
	if err != nil {
		die("tablet store: %v", err)
	}
	authenticator := tserverprocess.ExactAuthenticator{
		Identities: []*security.TCredentials{outbound, systemCredentials},
		Writers:    []*security.TCredentials{outbound, systemCredentials},
	}
	scans, err := scanserver.NewServer(scanserver.Options{
		Locator: store, Storage: files, BlockCache: cache.NewBlockCache(256 << 20), Logger: logger,
		Credentials: authenticator,
	})
	if err != nil {
		die("scan server: %v", err)
	}
	router, err := ingestrouter.New(store, ingestrouter.DefaultLimits())
	if err != nil {
		die("ingest router: %v", err)
	}
	ingest, err := ingestservice.New(ingestservice.Config{
		Router: router, Authenticator: authenticator,
		ConditionalReader: scans,
		Logger:            logger,
		TserverLock: func() string {
			lock, ok := host.Lock()
			if !ok {
				return ""
			}
			return path.Join(lockIDPath, lock.String()) + "$" + sessionGeneration
		},
	})
	if err != nil {
		die("ingest service: %v", err)
	}

	reporter := &tserverrpc.RetryingReporter{
		Connector: tserverprocess.ReportConnector{
			Resolver: resolver, Credentials: systemCredentials, InstanceID: loc.InstanceID(),
			AccumuloVersion: *accVersion,
		},
		Server: *advertise,
	}
	adapter, err := tserverrpc.New(runCtx, tserverrpc.Config{
		Host: host, Backend: store,
		Credentials: tserverprocess.ExactCredentials{
			Principal: *systemPrincipal, Token: token, TokenType: *systemTokenClass,
		},
		Reporter: reporter, InstanceID: loc.InstanceID(),
		ManagerLockPath: "/managers/lock",
		Name:            *advertise, Version: version, Stop: cancelRun,
		OnError: func(err error) { logger.Error("tserver operation", "error", err) },
	})
	if err != nil {
		die("manager adapter: %v", err)
	}
	defer adapter.Close()

	mux := thrift.NewTMultiplexedProcessor()
	services := tserverprocess.Services{Manager: adapter, Scans: scans}
	if *enableIngest {
		services.Ingest = ingest
	}
	if err := services.Register(mux); err != nil {
		die("register processors: %v", err)
	}
	var socket thrift.TServerTransport
	if tlsConfig != nil {
		socket, err = thrift.NewTSSLServerSocket(*listen, tlsConfig.Clone())
	} else {
		socket, err = thrift.NewTServerSocket(*listen)
	}
	if err != nil {
		die("listen %s: %v", *listen, err)
	}
	server := thrift.NewTSimpleServer4(
		mux, socket,
		thrift.NewTFramedTransportFactoryConf(thrift.NewTBufferedTransportFactory(8192), &thrift.TConfiguration{}),
		protocol.NewServerFactory(loc.InstanceID(), *accVersion),
	)

	serverErr := make(chan error, 1)
	thriftDone := make(chan error, 1)
	go func() {
		err := server.Serve()
		thriftDone <- err
		select {
		case serverErr <- err:
		default:
		}
	}()
	var operations *roleops.Server
	if *metricsAddress != "" {
		operations, err = roleops.Start(
			*metricsAddress,
			tserverprocess.OperationsHandlerWithWriteTier(host, scans, ingest, tabletFactory, *enableIngest),
			tlsConfig,
		)
		if err != nil {
			die("operations listener: %v", err)
		}
		go func() {
			<-operations.Done()
			if err := operations.Err(); err != nil {
				select {
				case serverErr <- err:
				default:
				}
			}
		}()
	}
	watchErr := make(chan error, 1)
	go func() {
		watchErr <- tserver.WatchManagerLock(
			runCtx, session, host, *managerPoll,
			func(err error) { logger.Error("manager lock observation", "error", err) },
		)
	}()
	supervisorErr := make(chan error, 1)
	supervisor := tserverprocess.Supervisor{
		Host: host, Release: adapter.ReleaseDropped, RetryBackoff: time.Second,
		OnError: func(err error) { logger.Warn("ServiceLock generation ended", "error", err) },
		NewGeneration: func() (*tserver.ServiceLock, tserver.ServiceLockData, error) {
			lock, err := tserver.NewServiceLock(zk.LockSession{SharedSession: session}, tserver.ServiceLockOptions{
				Path: lockPath, VerifyInterval: *lockVerify,
			})
			if err != nil {
				return nil, tserver.ServiceLockData{}, err
			}
			if _, err := adapter.LockData(lock, *advertise, *group); err != nil {
				return nil, tserver.ServiceLockData{}, err
			}
			data, err := tserver.TabletServerLockData(lock.UUID(), *advertise, *group, services.LockServices()...)
			return lock, data, err
		},
	}
	go func() { supervisorErr <- supervisor.Run(runCtx) }()

	logger.Info("shoal-tserver serving",
		"listen", *listen, "advertise", *advertise, "group", *group,
		"tablet_ingest", *enableIngest,
	)
	var reason error
	select {
	case <-signalCtx.Done():
		reason = signalCtx.Err()
	case reason = <-serverErr:
	case reason = <-watchErr:
	case reason = <-supervisorErr:
	case <-runCtx.Done():
		reason = runCtx.Err()
	}
	logger.Info("shoal-tserver draining", "reason", reason)
	ingest.BeginDrain()
	scans.BeginDrain()
	// Withdraw ownership before waiting on retained continuations. Existing
	// sessions are already materialized by scanserver and can finish without
	// keeping a ServiceLock that would invite new manager assignments.
	cancelRun()
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), *drainTimeout)
	var result scanserver.DrainResult
	if err := roleops.RunBounded(
		drainCtx,
		ingest.Drain,
		func(ctx context.Context) error {
			result = scans.Drain(ctx)
			return ctx.Err()
		},
	); err != nil {
		logger.Warn("bounded write-tier drain ended", "error", err)
	}
	cancelDrain()
	if result.Forced() > 0 {
		logger.Warn("forced scan session drain", "sessions", result.Forced())
	}
	_ = server.Stop()
	serverStopCtx, cancelServerStop := context.WithTimeout(context.Background(), 5*time.Second)
	select {
	case <-thriftDone:
	case <-serverStopCtx.Done():
		logger.Warn("Thrift serve loop did not stop before deadline")
	}
	cancelServerStop()
	if operations != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := operations.Shutdown(shutdownCtx); err != nil {
			logger.Warn("operations shutdown", "error", err)
		}
		cancel()
	}
}

func openStorage(ctx context.Context, scheme string) (storage.Backend, func(), error) {
	switch scheme {
	case "gs", "gcs":
		b, err := gcs.New(ctx)
		return b, func() {}, err
	case "s3":
		b, err := s3.New(ctx)
		return b, func() {}, err
	case "azure", "az":
		b, err := azure.New(ctx)
		return b, func() {}, err
	case "hdfs":
		b, err := hdfs.NewContext(ctx, os.Getenv("SHOAL_HDFS_NAMENODE"))
		if err != nil {
			return nil, func() {}, err
		}
		return b, func() { _ = b.Close() }, nil
	case "local":
		return local.New(), func() {}, nil
	default:
		return nil, func() {}, fmt.Errorf("unknown backend %q", scheme)
	}
}

func valueOrEnv(value, name string) string {
	if value != "" {
		return value
	}
	return os.Getenv(name)
}

func die(format string, args ...any) {
	slog.Error(fmt.Sprintf("shoal-tserver: "+format, args...))
	os.Exit(1)
}
