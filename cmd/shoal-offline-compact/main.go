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

// shoal-offline-compact runs a standalone full-major compaction over the
// tablets of an OFFLINE Accumulo table. It reads the tablet's input RFiles,
// applies the resolved table.iterator.majc.* stack, writes one output RFile
// per tablet, verifies each output, and emits an inspection-only plan.
//
// See docs/offline-compaction-design.md for the safety model. The OFFLINE
// fence (§3.1) is established before any compaction and re-verified
// before plan emission: if the table is brought back ONLINE in between, the
// run aborts without touching metadata.
//
// Release workflows are dry-run/plan-only until a manager/coordinator/FATE
// commit API exists. Legacy direct and non-dry-run paths are rejected.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/phrocker/shoal-oss/internal/cred"
	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/offlinecompact"
	"github.com/phrocker/shoal-oss/internal/shadow/itercfg"
	"github.com/phrocker/shoal-oss/internal/storage"
	"github.com/phrocker/shoal-oss/internal/storage/azure"
	"github.com/phrocker/shoal-oss/internal/storage/gcs"
	"github.com/phrocker/shoal-oss/internal/storage/hdfs"
	"github.com/phrocker/shoal-oss/internal/storage/local"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
	"github.com/phrocker/shoal-oss/internal/storage/s3"
	"github.com/phrocker/shoal-oss/internal/zk"
)

var version = "dev"

func main() {
	os.Exit(run())
}

func run() int {
	showVersion := flag.Bool("version", false, "print version and exit")
	zkServers := flag.String("zk", "", "comma-separated ZK quorum (REQUIRED)")
	instanceName := flag.String("instance", "accumulo", "Accumulo instance name")
	table := flag.String("table", "", "table name or id to compact (REQUIRED)")
	rangeSpec := flag.String("range", "", "restrict to tablets in startRow:endRow (inclusive; either side may be empty for unbounded)")
	dryRun := flag.Bool("dry-run", true, "plan + verify only; false is disabled until an authority-preserving commit API exists")
	commitModeStr := flag.String("commit-mode", "plan", "plan only; direct is deprecated and disabled")
	doVerify := flag.Bool("verify", true, "run the §5 verification on every compacted tablet")
	outDir := flag.String("out", ".", "directory to write the commit plan into (Mode P)")
	storageScheme := flag.String("storage", "gs", "RFile storage backend: gs, s3, azure, hdfs, local, memory")
	user := flag.String("user", "root", "principal for metadata reads")
	password := flag.String("password", "", "password (prefer SHOAL_PASSWORD env); also used as the ZK instance secret")
	accVersion := flag.String("accumulo-version", "4.0.0-SNAPSHOT", "server major.minor must match")
	zkTimeout := flag.Duration("zk-timeout", 30*time.Second, "ZK session timeout")
	itercfgTTL := flag.Duration("itercfg-ttl", 30*time.Second, "TTL for the resolved iterator-stack cache")
	logLevel := flag.String("log-level", "info", "slog level: debug, info, warn, error")
	flag.Parse()

	if *showVersion {
		fmt.Println("shoal-offline-compact", version)
		return 0
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: parseLogLevel(*logLevel)}))
	slog.SetDefault(logger)

	if *zkServers == "" {
		return fail(logger, "-zk is required")
	}
	if *table == "" {
		return fail(logger, "-table is required")
	}
	if *password == "" {
		*password = os.Getenv("SHOAL_PASSWORD")
	}
	if *password == "" {
		return fail(logger, "password required (-password or SHOAL_PASSWORD env)")
	}

	commitMode, err := offlinecompact.ParseCommitMode(*commitModeStr)
	if err != nil {
		return fail(logger, "%v", err)
	}
	if err := validateReleaseCommitOptions(*dryRun, commitMode); err != nil {
		return fail(logger, "%v", err)
	}

	start, end, err := parseRange(*rangeSpec)
	if err != nil {
		return fail(logger, "%v", err)
	}

	ctx := context.Background()

	// ZK + metadata + iterator-config wiring. Table /config znodes carry
	// ACLs requiring digest auth as accumulo:<instance-secret>; on our
	// deployments that equals the root password (same as the shadow
	// service).
	loc, err := zk.NewWithAuth(strings.Split(*zkServers, ","), *instanceName, *zkTimeout, *password)
	if err != nil {
		return fail(logger, "zk.New: %v", err)
	}
	defer loc.Close()
	logger.Info("zk connected", slog.String("instance_id", loc.InstanceID()))

	creds := cred.NewPasswordCreds(*user, *password, loc.InstanceID())
	walker := metadata.NewWalker(loc, creds, *accVersion).WithLogger(
		logger.With(slog.String("subsystem", "walker")))
	resolver := itercfg.NewResolver(loc, *itercfgTTL, logger.With(slog.String("subsystem", "itercfg")))

	// Resolve table name → id. Accept a literal id too: if the name
	// lookup fails, fall through and let the fence validate the id.
	tableID := *table
	if id, rerr := resolver.ResolveTableID(ctx, *table); rerr == nil {
		tableID = id
	} else {
		logger.Info("table name not resolved; treating -table as a literal id",
			slog.String("table", *table), slog.String("err", rerr.Error()))
	}

	// Storage backend for RFile reads + output writes.
	backend, closeBackend, err := openBackend(ctx, *storageScheme)
	if err != nil {
		return fail(logger, "storage backend %q: %v", *storageScheme, err)
	}
	defer closeBackend()

	// Establish the OFFLINE fence BEFORE compacting. This both enforces
	// the precondition (table must be OFFLINE) and mints the continuity
	// token re-checked at commit time.
	fence := offlinecompact.NewZKTableFence(loc, tableID)
	minted, err := fence.Fence(ctx)
	if err != nil {
		return fail(logger, "offline fence: %v", err)
	}
	logger.Info("table is OFFLINE; fence established",
		slog.String("table_id", tableID), slog.Int64("state_version", int64(minted.Version)))

	deps := offlinecompact.Deps{
		Tablets: rangeEnumerator{inner: walker, start: start, end: end, apply: *rangeSpec != ""},
		Stacks:  resolver,
		Files:   offlinecompact.NewBackendStore(backend),
	}
	opts := offlinecompact.Options{Verify: *doVerify, Logger: logger}

	plan, err := offlinecompact.Run(ctx, tableID, deps, opts)
	if err != nil {
		return fail(logger, "compaction: %v", err)
	}
	logger.Info("compaction complete",
		slog.String("table_id", tableID),
		slog.Int("compacted", len(plan.Results)),
		slog.Int("no_op", len(plan.NoOp)),
	)

	// Release workflows are validated as dry-run/plan-only above. Commit
	// retains its legacy modes for package-level regression tests.
	commitPlan, commitErr := offlinecompact.Commit(ctx, plan, fence, minted, commitMode, *dryRun, nil)

	// Always emit the commit plan artifact when one was produced — even
	// on a commit error (e.g. ErrDirectCommitUnavailable, or a mid-run
	// Mode D failure) — so the operator can inspect or resume from it.
	var planPath string
	if commitPlan != nil {
		planPath, err = writeCommitPlan(*outDir, tableID, commitPlan)
		if err != nil {
			return fail(logger, "write commit plan: %v", err)
		}
	}
	if commitErr != nil {
		if planPath != "" {
			logger.Error("commit plan written despite commit failure",
				slog.String("commit_plan", planPath))
		}
		return fail(logger, "commit: %v", commitErr)
	}

	switch {
	case *dryRun:
		logger.Info("DRY RUN — no metadata written",
			slog.String("commit_plan", planPath),
			slog.Int("tablets", len(commitPlan.Tablets)))
	}
	return 0
}

func validateReleaseCommitOptions(dryRun bool, mode offlinecompact.CommitMode) error {
	if !dryRun {
		return errors.New("--dry-run=false is disabled until a manager/coordinator/FATE commit API exists")
	}
	if mode != offlinecompact.ModePlan {
		return errors.New("--commit-mode=direct is deprecated and disabled")
	}
	return nil
}

// rangeEnumerator wraps the metadata walker to restrict enumeration to a
// tablet row range. When apply is false it is a pass-through.
type rangeEnumerator struct {
	inner      offlinecompact.TabletEnumerator
	start, end []byte
	apply      bool
}

func (e rangeEnumerator) LocateTable(ctx context.Context, tableID string) ([]metadata.TabletInfo, error) {
	tablets, err := e.inner.LocateTable(ctx, tableID)
	if err != nil || !e.apply {
		return tablets, err
	}
	return offlinecompact.SelectTablets(tablets, e.start, e.end), nil
}

// parseRange splits "start:end" into inclusive bounds. Empty side → nil
// (unbounded). An empty spec returns (nil, nil, nil). A spec without a
// colon is an error (ambiguous).
func parseRange(spec string) (start, end []byte, err error) {
	if spec == "" {
		return nil, nil, nil
	}
	i := strings.IndexByte(spec, ':')
	if i < 0 {
		return nil, nil, fmt.Errorf("-range %q must contain ':' (startRow:endRow)", spec)
	}
	s, e := spec[:i], spec[i+1:]
	if s != "" {
		start = []byte(s)
	}
	if e != "" {
		end = []byte(e)
	}
	return start, end, nil
}

// writeCommitPlan serializes the plan into outDir and returns its path.
func writeCommitPlan(outDir, tableID string, cp *offlinecompact.CommitPlan) (string, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", outDir, err)
	}
	raw, err := offlinecompact.MarshalCommitPlan(cp)
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("offline-compact-%s-%s.json",
		sanitize(tableID), time.Now().UTC().Format("20060102T150405Z"))
	p := filepath.Join(outDir, name)
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", p, err)
	}
	return p, nil
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, s)
}

// openBackend selects the RFile storage backend and returns a closer.
func openBackend(ctx context.Context, scheme string) (storage.Backend, func(), error) {
	noop := func() {}
	switch scheme {
	case "gs", "gcs":
		be, err := gcs.New(ctx)
		if err != nil {
			return nil, nil, err
		}
		return be, func() { _ = be.Close() }, nil
	case "s3":
		be, err := s3.New(ctx)
		if err != nil {
			return nil, nil, err
		}
		return be, func() { _ = be.Close() }, nil
	case "azure", "azblob", "az":
		be, err := azure.New(ctx)
		if err != nil {
			return nil, nil, err
		}
		return be, func() { _ = be.Close() }, nil
	case "hdfs":
		be, err := hdfs.NewContext(ctx, os.Getenv("SHOAL_HDFS_NAMENODE"))
		if err != nil {
			return nil, nil, err
		}
		return be, func() { _ = be.Close() }, nil
	case "local":
		return local.New(), noop, nil
	case "memory":
		return memory.New(), noop, nil
	default:
		return nil, nil, fmt.Errorf("unknown scheme (expected gs, s3, azure, hdfs, local, or memory)")
	}
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

func fail(logger *slog.Logger, format string, args ...any) int {
	logger.Error("shoal-offline-compact: " + fmt.Sprintf(format, args...))
	return 1
}
