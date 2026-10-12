// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/phrocker/shoal-oss/internal/healthsurface"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// The approval sweep's system observer (#655).
//
// fleet.ApprovalService.Sweep records, as neither party, what a held request
// nobody will touch again has become. It acts under a decision this host mints
// for one approval at a time and binds with its own binder, exactly as the
// HTTP transport binds a request's decision:
//
//   - subject, actor and client the system observer's
//     (fleet.ApprovalSweeperSubject, fleet.ApprovalSweeperClientID);
//   - auth.OperationApprovalSweep and nothing else;
//   - exactly the approval's source and policy;
//   - the policy generation in force, and a one-minute expiry;
//   - a request and correlation ID of its own, so each write's audit and its
//     event's publication audit name the sweep that made it.
//
// This file is the one place in the command that denotes the operation
// (approval_sweep_grant_test.go holds that), no authenticator mints it, and
// the decision never leaves this process: nothing outside the host can
// present the observer.
type approvalSweepMinter struct {
	binder      auth.Binder
	generations auth.GenerationReader
	clock       func() time.Time
	instance    string
	sequence    atomic.Uint64
}

// approvalSweepDecisionTTL bounds one minted decision. A row's write and its
// event's publication run well inside it.
const approvalSweepDecisionTTL = time.Minute

func newApprovalSweepMinter(
	binder auth.Binder, generations auth.GenerationReader, clock func() time.Time,
) (*approvalSweepMinter, error) {
	if binder == nil || generations == nil || clock == nil {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "approval sweep dependencies are required")
	}
	var instance [8]byte
	if _, err := rand.Read(instance[:]); err != nil {
		return nil, err
	}
	return &approvalSweepMinter{
		binder: binder, generations: generations, clock: clock,
		instance: hex.EncodeToString(instance[:]),
	}, nil
}

// bind is the fleet.ApprovalSweepBinder.
func (m *approvalSweepMinter) bind(
	ctx context.Context, scope fleet.ApprovalSweepScope,
) (context.Context, error) {
	generation, err := m.generations.CurrentPolicyGeneration(
		ctx, workspaceAuthorizationDomain)
	if err != nil {
		return nil, err
	}
	id := shoal.ID(fmt.Sprintf(
		"approval-sweep-%s-%d", m.instance, m.sequence.Add(1)))
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: fleet.ApprovalSweeperSubject, Actor: fleet.ApprovalSweeperSubject,
		ClientID:              fleet.ApprovalSweeperClientID,
		AuthorizationDomain:   workspaceAuthorizationDomain,
		AllowedOperations:     []auth.Operation{auth.OperationApprovalSweep},
		PermittedSourceIDs:    [][]byte{scope.SourceID},
		PermittedPolicyIDs:    [][]byte{scope.PolicyID},
		PolicyGeneration:      generation,
		AuthenticationExpires: m.clock().UTC().Add(approvalSweepDecisionTTL),
		RequestID:             id, CorrelationID: id,
	})
	if err != nil {
		return nil, err
	}
	return m.binder.Bind(ctx, decision)
}

// maxApprovalSweepPages bounds one pass, so a store whose continuation never
// ends cannot keep a pass running forever. At fleet.MaxApprovalListResults a
// page, that is a million approvals.
const maxApprovalSweepPages = 1 << 14

// approvalSweeper walks the approval key space across passes.
//
// A pass resumes from where the last one stopped and wraps to the beginning
// at the end, so a store too large for one pass's budget is still covered,
// over several passes, rather than having its tail never reached. A pass ends
// when it reaches the end of the key space (the next starts again at the
// beginning), when its budget runs out, or on a page that fails; the cursor
// is then the last page that succeeded, so nothing is skipped.
//
// A page that keeps failing at the same cursor (a scan error that persists)
// would otherwise hold every later pass there, so after
// maxApprovalSweepPageFailures in a row the cursor wraps to the beginning:
// the rest of the key space is still swept, the failing page is retried each
// cycle, and the health surface counts each failure.
type approvalSweeper struct {
	service   *fleet.ApprovalService
	pageLimit int
	maxPages  int
	cursor    []byte
	failures  int
}

// maxApprovalSweepPageFailures is how many passes in a row may fail at one
// cursor before the sweep wraps past it.
const maxApprovalSweepPageFailures = 3

// minApprovalSweepInterval is the shortest interval the host accepts: a
// pass is bounded by the interval, and a budget too short for a page would
// make no progress worth the work.
const minApprovalSweepInterval = 10 * time.Second

func newApprovalSweeper(service *fleet.ApprovalService) *approvalSweeper {
	return &approvalSweeper{
		service: service, pageLimit: fleet.MaxApprovalListResults,
		maxPages: maxApprovalSweepPages,
	}
}

// pass runs one bounded pass from the cursor and returns its totals.
func (w *approvalSweeper) pass(ctx context.Context) (fleet.ApprovalSweepResult, error) {
	var total fleet.ApprovalSweepResult
	for page := 0; page < w.maxPages; page++ {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		result, err := w.service.Sweep(ctx, fleet.ApprovalSweepRequest{
			After: w.cursor, Limit: w.pageLimit,
		})
		total.Examined += result.Examined
		total.Expired += result.Expired
		total.Stranded += result.Stranded
		total.Unpublished += result.Unpublished
		total.Raced += result.Raced
		total.Failed += result.Failed
		total.Undecodable += result.Undecodable
		total.Settled += result.Settled
		if err != nil {
			if ctx.Err() != nil {
				// Out of budget, or stopping: keep what this page finished.
				if len(result.Next) > 0 {
					w.cursor = append([]byte(nil), result.Next...)
					w.failures = 0
				}
				return total, err
			}
			w.failures++
			if w.failures >= maxApprovalSweepPageFailures {
				w.cursor, w.failures = nil, 0
			}
			return total, err
		}
		w.failures = 0
		w.cursor = append([]byte(nil), result.Next...)
		if len(w.cursor) == 0 {
			// The whole key space has been covered; the next pass wraps.
			return total, nil
		}
	}
	return total, nil
}

// sweepApprovals runs one complete cycle over the approval key space from
// its beginning and returns its totals.
func sweepApprovals(
	ctx context.Context, service *fleet.ApprovalService,
) (fleet.ApprovalSweepResult, error) {
	return newApprovalSweeper(service).pass(ctx)
}

// startApprovalSweeps runs a pass every interval until ctx ends, and returns
// a function that stops it and waits for a pass in flight to finish. Each pass
// is bounded by the interval and resumes where the last stopped. Only a pass
// that recorded something or failed is reported.
func startApprovalSweeps(
	ctx context.Context, service *fleet.ApprovalService,
	interval time.Duration, output io.Writer,
) func() {
	ctx, cancel := context.WithCancel(ctx)
	sweeper := newApprovalSweeper(service)
	var done sync.WaitGroup
	done.Add(1)
	go func() {
		defer done.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			pass, cancelPass := context.WithTimeout(ctx, interval)
			total, err := sweeper.pass(pass)
			cancelPass()
			if err != nil && ctx.Err() == nil && !errors.Is(err, context.DeadlineExceeded) {
				fmt.Fprintf(output, "approval sweep pass failed after %d "+
					"approval(s): %v\n", total.Examined, err)
				continue
			}
			if total.Expired+total.Stranded+total.Failed+total.Unpublished+
				total.Undecodable > 0 {
				fmt.Fprintf(output, "approval sweep: examined %d, expired %d, "+
					"stranded %d, event unpublished %d, raced %d, failed %d, "+
					"undecodable %d\n",
					total.Examined, total.Expired, total.Stranded,
					total.Unpublished, total.Raced, total.Failed, total.Undecodable)
			}
		}
	}()
	return func() {
		cancel()
		done.Wait()
	}
}

// approvalSweepMetrics serves the sweep's error counters on the health
// surface: records it skipped as undecodable and records it could not judge
// or write. Both count this process's own failures, never tenant workload,
// as the surface requires (healthsurface.Config). Nil without a service.
func approvalSweepMetrics(service *fleet.ApprovalService) healthsurface.MetricsWriter {
	if service == nil {
		return nil
	}
	return func(b *strings.Builder) {
		undecodable, failed, pageFailed := service.ApprovalSweepCounters()
		fmt.Fprintf(b,
			"# HELP shoal_fleet_approval_sweep_undecodable_total Stored "+
				"approval records the sweep skipped because they do not decode.\n"+
				"# TYPE shoal_fleet_approval_sweep_undecodable_total counter\n"+
				"shoal_fleet_approval_sweep_undecodable_total %d\n"+
				"# HELP shoal_fleet_approval_sweep_failed_total Approval records "+
				"the sweep could not judge or write.\n"+
				"# TYPE shoal_fleet_approval_sweep_failed_total counter\n"+
				"shoal_fleet_approval_sweep_failed_total %d\n"+
				"# HELP shoal_fleet_approval_sweep_page_failed_total Approval "+
				"sweep pages whose scan failed.\n"+
				"# TYPE shoal_fleet_approval_sweep_page_failed_total counter\n"+
				"shoal_fleet_approval_sweep_page_failed_total %d\n",
			undecodable, failed, pageFailed)
	}
}

// combineMetrics serves every non-nil writer, in order; nil if there are none.
func combineMetrics(writers ...healthsurface.MetricsWriter) healthsurface.MetricsWriter {
	var present []healthsurface.MetricsWriter
	for _, writer := range writers {
		if writer != nil {
			present = append(present, writer)
		}
	}
	if len(present) == 0 {
		return nil
	}
	return func(b *strings.Builder) {
		for _, writer := range present {
			writer(b)
		}
	}
}
