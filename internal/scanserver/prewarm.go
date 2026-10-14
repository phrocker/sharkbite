package scanserver

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Prewarm pulls every RFile referenced by the given tablet locations
// into the file cache, in parallel up to `parallelism` workers. Caps
// at the file cache budget — once full, further fetches are no-ops.
//
// Designed to be called once at daemon startup, in a background
// goroutine, so the readiness probe doesn't wait. Without prewarming,
// the FIRST scan after pod restart pays a 600-800ms GCS pull; with
// prewarming, the relevant files are already in memory and the first
// real scan hits the warm path.
//
// Skipping logic: any path with no extension or that already lives in
// the cache is silently skipped. Errors on individual file pulls are
// logged but don't abort the warmup — a single bad file shouldn't
// block the rest.
func (s *Server) Prewarm(ctx context.Context, tableIDs []string, parallelism int) {
	if s.files == nil || s.locator == nil {
		s.logger.Info("prewarm skipped — file cache or locator not configured")
		return
	}
	if parallelism < 1 {
		parallelism = 4
	}
	t0 := time.Now()
	s.logger.Info("prewarm starting", slog.Any("tables", tableIDs), slog.Int("parallelism", parallelism))

	// Collect every (tableID, file) pair to warm.
	type job struct {
		tableID string
		path    string
	}
	jobs := make([]job, 0, 256)
	for _, tableID := range tableIDs {
		tablets, err := s.locator.LocateTable(ctx, tableID)
		if err != nil {
			s.logger.Warn("prewarm: locate table", slog.String("table", tableID), slog.Any("err", err))
			continue
		}
		for _, t := range tablets {
			for _, f := range t.Files {
				jobs = append(jobs, job{tableID: tableID, path: f.Path})
			}
		}
	}
	s.logger.Info("prewarm: collected files", slog.Int("count", len(jobs)))

	// Bounded-parallel fetch. Stop early if cache is full.
	sem := make(chan struct{}, parallelism)
	var wg sync.WaitGroup
	var ok, skipped, errs int64
	var mu sync.Mutex
	for _, j := range jobs {
		// Stop scheduling more fetches once cache is full — further
		// inserts would just evict each other.
		if hits, _, _, used, _ := s.files.Stats(); used >= s.files.cap {
			_ = hits
			break
		}
		// Skip already-cached files.
		if _, hit := s.files.Get(j.path); hit {
			skipped++
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(jj job) {
			defer wg.Done()
			defer func() { <-sem }()
			if _, _, err := s.openRFile(ctx, jj.path); err != nil {
				mu.Lock()
				errs++
				mu.Unlock()
				s.logger.Debug("prewarm fetch failed",
					slog.String("path", jj.path), slog.Any("err", err))
				return
			}
			mu.Lock()
			ok++
			mu.Unlock()
		}(j)
	}
	wg.Wait()

	hits, misses, evicts, bytesUsed, entries := s.files.Stats()
	s.logger.Info("prewarm complete",
		slog.Duration("dur", time.Since(t0)),
		slog.Int64("fetched", ok),
		slog.Int64("skipped_already_cached", skipped),
		slog.Int64("errors", errs),
		slog.Int64("cache_bytes", bytesUsed),
		slog.Int("cache_entries", entries),
		slog.Int64("cache_hits", hits),
		slog.Int64("cache_misses", misses),
		slog.Int64("cache_evicts", evicts),
	)
}

// ParseTableIDs splits a comma-separated list of table IDs from a flag.
// Trims whitespace and drops empty entries. Useful in cmd/shoal main.
func ParseTableIDs(csv string) []string {
	if csv == "" {
		return nil
	}
	parts := strings.Split(csv, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
