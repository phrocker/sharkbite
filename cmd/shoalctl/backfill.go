// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file distributed
// with this work for additional information regarding copyright ownership.
// The ASF licenses this file to you under the Apache License, Version 2.0.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/phrocker/shoal-oss/internal/cred"
	"github.com/phrocker/shoal-oss/internal/embedbackfill"
	"github.com/phrocker/shoal-oss/internal/ingestclient"
	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/metadatacas"
	"github.com/phrocker/shoal-oss/internal/storage"
	"github.com/phrocker/shoal-oss/internal/storage/azure"
	"github.com/phrocker/shoal-oss/internal/storage/gcs"
	"github.com/phrocker/shoal-oss/internal/storage/hdfs"
	"github.com/phrocker/shoal-oss/internal/storage/local"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
	"github.com/phrocker/shoal-oss/internal/storage/s3"
	"github.com/phrocker/shoal-oss/internal/transportpool"
	"github.com/phrocker/shoal-oss/internal/zk"
)

// runEmbeddingBackfill is the operator-facing half of issue #274.
//
// Since a missing file.embedding column reads as unknown rather than as
// the fabricated no_embeddings, existing files need their real state
// recorded once. This command establishes that state from each file's
// own footer and writes the column, and reports — file by file — the
// ones whose footer is absent too, because those cannot be established
// from metadata and must not be guessed at.
//
// It defaults to --dry-run, matching shoal-offline-compact: an operator
// gets the size and the shape of the migration before anything is
// written.
func runEmbeddingBackfill(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("embedding-backfill", flag.ContinueOnError)
	flags.SetOutput(stderr)
	zkServers := flags.String("zk", "", "comma-separated ZooKeeper quorum (required)")
	instanceName := flags.String("instance", "accumulo", "Accumulo instance name")
	table := flags.String("table", "", "table id to backfill (required)")
	storageScheme := flags.String("storage", "gs", "file storage backend: gs, s3, azure, hdfs, local, memory")
	user := flags.String("user", "root", "principal for metadata reads and writes")
	password := flags.String("password", "", "password (prefer the SHOAL_PASSWORD environment variable)")
	accVersion := flags.String("accumulo-version", "4.0.0-SNAPSHOT", "server major.minor must match")
	zkTimeout := flags.Duration("zk-timeout", 30*time.Second, "ZooKeeper session timeout")
	dialTimeout := flags.Duration("dial-timeout", 10*time.Second, "tablet-server dial timeout")
	dryRun := flags.Bool("dry-run", true, "report what would be written without writing it")
	trustNoEmbeddings := flags.Bool("trust-no-embeddings", false,
		"accept a no_embeddings footer or column as truth; required because one written "+
			"before issue #274 may have fabricated that claim")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("embedding-backfill accepts no positional arguments")
	}
	if *zkServers == "" {
		return errors.New("--zk is required")
	}
	if *table == "" {
		return errors.New("--table is required")
	}
	if *password == "" {
		*password = os.Getenv("SHOAL_PASSWORD")
	}
	if *password == "" {
		return errors.New("password required (--password or SHOAL_PASSWORD)")
	}

	ctx := context.Background()
	loc, err := zk.NewWithAuth(strings.Split(*zkServers, ","), *instanceName, *zkTimeout, *password)
	if err != nil {
		return fmt.Errorf("zookeeper: %w", err)
	}
	defer loc.Close()

	creds := cred.NewPasswordCreds(*user, *password, loc.InstanceID())
	walker := metadata.NewWalker(loc, creds, *accVersion)

	backend, closeBackend, err := openBackfillBackend(ctx, *storageScheme)
	if err != nil {
		return fmt.Errorf("storage backend %q: %w", *storageScheme, err)
	}
	defer closeBackend()

	cfg := embedbackfill.Config{
		Files:   embedbackfill.MetadataFiles{Reader: walker, TableID: *table},
		Footers: embedbackfill.StorageFooters{Backend: backend},
		DryRun:  *dryRun,

		TrustNoEmbeddings: *trustNoEmbeddings,
	}

	if !*dryRun {
		pool, err := transportpool.New(transportpool.Config{
			IdleTimeout: time.Minute, MaxIdlePerEndpoint: 4,
		})
		if err != nil {
			return fmt.Errorf("transport pool: %w", err)
		}
		defer pool.Close()
		conditional, err := ingestclient.NewPooled(pool, loc.InstanceID(), *accVersion, creds, *dialTimeout)
		if err != nil {
			return fmt.Errorf("conditional metadata client: %w", err)
		}
		defer conditional.Close()
		columns, err := metadatacas.NewBackfillWriter(walker, loc, conditional)
		if err != nil {
			return fmt.Errorf("backfill writer: %w", err)
		}
		cfg.Columns = embedbackfill.CASColumns{Writer: columns}
	}

	summary, err := embedbackfill.Run(ctx, cfg)
	if err != nil {
		return err
	}
	fmt.Fprint(stdout, embedbackfill.Report(summary))
	if !summary.Complete() {
		// A non-zero exit is the point: a migration that left files
		// outstanding has not converged, and a script that treated this
		// run as success would move on from a table that still refuses
		// compaction.
		return errors.New("backfill incomplete; see the outstanding entries above")
	}
	return nil
}

func openBackfillBackend(ctx context.Context, scheme string) (storage.Backend, func(), error) {
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
		return nil, nil, errors.New("unknown scheme (expected gs, s3, azure, hdfs, local, or memory)")
	}
}
