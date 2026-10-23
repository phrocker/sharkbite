// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/phrocker/shoal-oss/internal/embedpb"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/graphtable"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
)

// graphTableHostConfig is the -graph-table, -graph-table-label, and
// -graph-stores configuration.
type graphTableHostConfig struct {
	table  string
	label  string
	stores string
}

type closers []io.Closer

func (c closers) Close() error {
	var err error
	for i := len(c) - 1; i >= 0; i-- {
		err = errors.Join(err, c[i].Close())
	}
	return err
}

// mountGraphTable serves a described engine table as a graph. The rows are
// read through the Scan request the shoal-embed service takes: from the
// workspace's own engine when no store is named, or from the store
// processes -graph-stores names, each owning a row range. The descriptor
// is read from the same stores, which must agree on it. Nothing is served
// until every store is reachable and the descriptor is recorded.
func mountGraphTable(ctx context.Context, handler *webapi.Handler, resolver auth.Resolver, cfg graphTableHostConfig, eng *engine.Engine, output io.Writer) (io.Closer, error) {
	if cfg.table == "" {
		return nil, errors.New("-graph-table-label and -graph-stores need -graph-table")
	}
	if cfg.label == "" {
		return nil, errors.New("-graph-table needs -graph-table-label: a described table is exposed under one label")
	}
	policyID, err := authorized.LabelPolicyID(workspaceSourceID, cfg.label)
	if err != nil {
		return nil, fmt.Errorf("-graph-table-label: %w", err)
	}
	var closer closers
	var store graphtable.Store
	var described []webapi.GraphTableStore
	if cfg.stores == "" {
		if eng == nil {
			return nil, errors.New("-graph-table needs the embedded backend or -graph-stores")
		}
		store = graphtable.NewEngineStore(eng)
		described = []webapi.GraphTableStore{{Name: "embedded", Address: "embedded"}}
	} else {
		specs, err := graphtable.ParseShardSpecs(cfg.stores)
		if err != nil {
			return nil, fmt.Errorf("-graph-stores: %w", err)
		}
		var shards []graphtable.Shard
		for i, spec := range specs {
			conn, err := grpc.NewClient(spec.Address, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				closer.Close()
				return nil, fmt.Errorf("-graph-stores %s: %w", spec.Address, err)
			}
			closer = append(closer, conn)
			name := fmt.Sprintf("store-%d", i+1)
			shards = append(shards, graphtable.Shard{Name: name, Start: spec.Start, End: spec.End, Store: graphtable.NewRemoteStore(embedpb.NewShoalEmbedClient(conn))})
			described = append(described, webapi.GraphTableStore{Name: name, Address: spec.Address, Start: string(spec.Start), End: string(spec.End)})
		}
		if len(shards) == 1 {
			store = shards[0].Store
		} else {
			sharded, err := graphtable.NewSharded(shards)
			if err != nil {
				closer.Close()
				return nil, fmt.Errorf("-graph-stores: %w", err)
			}
			store = sharded
		}
	}
	reader, err := graphtable.Open(ctx, store, cfg.table)
	if err != nil {
		closer.Close()
		return nil, fmt.Errorf("-graph-table %q: %w", cfg.table, err)
	}
	if err := handler.MountGraphTable(webapi.GraphTableSurface{
		Resolver: resolver, Reader: reader, Label: cfg.label, LabelPolicyID: policyID, Stores: described,
	}); err != nil {
		closer.Close()
		return nil, err
	}
	where := "this workspace's engine"
	if len(described) > 0 && described[0].Address != "embedded" {
		var addresses []string
		for _, d := range described {
			addresses = append(addresses, fmt.Sprintf("%s=%s..%s", d.Address, d.Start, d.End))
		}
		where = strings.Join(addresses, ", ")
	}
	fmt.Fprintf(output, "Serving described table %q (%s@%d) at %s from %s under label %q\n",
		cfg.table, reader.Schema().GetSchemaId(), reader.Schema().GetVersion(), webapi.GraphTableRoute, where, cfg.label)
	return closer, nil
}
