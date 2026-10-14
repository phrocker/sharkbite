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

package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/phrocker/shoal-oss/internal/engine"
)

// eventSyncConfig configures the in-process event-driven RFile syncer.
type eventSyncConfig struct {
	Table       string
	BackendName string
	Opts        engine.RFileExportOptions
	StatePath   string
	Interval    time.Duration // safety-net poll (ships even with no events)
	Debounce    time.Duration // coalesce a burst of flush/compact events
	TickTimeout time.Duration
}

// runEventSync ships RFiles as soon as they land. It subscribes to the engine's
// RFile event bus and runs an incremental export shortly after each flush or
// compaction (debounced to coalesce bursts), with a periodic safety-net tick so
// shipping still happens if an event is ever dropped. This is the single-binary
// realization of compaction-event-driven (vs. fixed-interval) shipping: the one
// writing process ships its own deltas without a separate sync daemon.
//
// It blocks until ctx is cancelled, then returns nil.
func runEventSync(ctx context.Context, eng *engine.Engine, logger *slog.Logger, cfg eventSyncConfig) error {
	prior, err := engine.LoadSyncState(cfg.StatePath)
	if err != nil {
		return err
	}

	events, cancel := eng.Subscribe()
	defer cancel()

	tick := func(reason string) {
		tctx, cancelTick := context.WithTimeout(ctx, cfg.TickTimeout)
		defer cancelTick()
		dst, cleanup, err := openStorageBackend(tctx, cfg.BackendName, cfg.Opts.DestinationRoot)
		if err != nil {
			logger.Error("event-sync backend open failed", "err", err)
			return
		}
		defer cleanup()
		res, err := eng.ExportRFilesIncremental(tctx, cfg.Table, dst, cfg.Opts, prior)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			logger.Error("event-sync tick failed", "err", err)
			return
		}
		prior = res.State
		if err := engine.SaveSyncState(cfg.StatePath, res.State); err != nil {
			logger.Error("event-sync save state failed", "err", err)
			return
		}
		// Only log ticks that actually shipped something, to keep the steady
		// state quiet; the safety-net poll would otherwise log every interval.
		if len(res.Uploaded) > 0 || len(res.Retired) > 0 {
			logger.Info("event-sync shipped",
				"reason", reason,
				"table", cfg.Table,
				"sequence", res.State.Sequence,
				"uploaded", len(res.Uploaded),
				"retired", len(res.Retired),
				"rfiles", len(res.Manifest.RFiles),
				"manifest", res.ManifestPath,
			)
		}
	}

	// Debounce timer: armed when an event arrives, fires once the burst settles.
	debounce := time.NewTimer(time.Hour)
	if !debounce.Stop() {
		<-debounce.C
	}
	pending := false
	arm := func() {
		if pending {
			return
		}
		pending = true
		debounce.Reset(cfg.Debounce)
	}

	safety := time.NewTicker(cfg.Interval)
	defer safety.Stop()

	logger.Info("event-sync started",
		"table", cfg.Table,
		"dst", cfg.Opts.DestinationRoot,
		"backend", cfg.BackendName,
		"interval", cfg.Interval.String(),
		"debounce", cfg.Debounce.String(),
		"state", cfg.StatePath,
	)

	// Initial sync so a restart immediately ships any RFiles already on disk.
	tick("startup")

	for {
		select {
		case <-ctx.Done():
			logger.Info("event-sync stopped")
			return nil
		case _, ok := <-events:
			if !ok {
				return nil
			}
			arm()
		case <-debounce.C:
			pending = false
			tick("event")
		case <-safety.C:
			tick("interval")
		}
	}
}
