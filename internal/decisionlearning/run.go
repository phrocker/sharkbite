// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisionlearning

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/phrocker/shoal-oss/internal/weightsbundle"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// A training run is driven from the console and executed by a learning
// worker, a separate process that speaks only this HTTP API. Three
// append-only record kinds carry it, each immutable once written:
//
//   - a run request under the manifest pin, recorded by the console once
//     the evaluation policy is declared and before any receipt exists;
//   - a claim per attempt, recorded by the worker that takes the job under
//     a lease; one claim is live at a time;
//   - progress per claim, recorded by the worker as it fetches, trains,
//     records, and seals; a terminal phase releases the claim.
//
// Nothing here moves a job forward by itself: predictions, the receipt,
// and the sealed candidate still arrive through their own routes and are
// verified there. The run records say who is doing the work and how far
// it got, so the console can show it and a worker can find it.
const (
	kindRunRequest  = "distillation-run-request"
	kindRunClaim    = "distillation-run-claim"
	kindRunProgress = "distillation-run-progress"

	// maxRunAttempts bounds the claims probed for one job.
	maxRunAttempts = 1000
	// maxRunProgress bounds the progress records probed for one claim.
	maxRunProgress = 10000

	defaultLeaseSeconds = 600
	maxLeaseSeconds     = 6 * 3600
	maxProgressMessage  = 2000
)

// RunPhases are the phases a worker may report, in the order a run takes
// them; done and failed are terminal.
var RunPhases = []string{"claimed", "fetching", "training", "recording_predictions", "recording_receipt", "sealing", "done", "failed"}

func runPhaseKnown(phase string) bool {
	for _, p := range RunPhases {
		if p == phase {
			return true
		}
	}
	return false
}

func runTerminal(phase string) bool { return phase == "done" || phase == "failed" }

func claimID(pin string, attempt int) string {
	return weightsbundle.SHA256Hex([]byte(pin + ":claim:" + strconv.Itoa(attempt)))
}

func progressID(claim string, seq int) string {
	return weightsbundle.SHA256Hex([]byte(claim + ":progress:" + strconv.Itoa(seq)))
}

// RequestRun records that the console asked for the job to be trained by a
// worker. It needs a declared evaluation policy and no receipt; a second
// request returns the first.
func (s *Service) RequestRun(ctx context.Context, pin string, raw json.RawMessage) (json.RawMessage, error) {
	actor, err := actorFor(ctx, "")
	if err != nil {
		return nil, err
	}
	if !recordID.MatchString(pin) {
		return nil, invalid("manifest pin")
	}
	if len(raw) > 0 {
		var body struct{}
		if err := weightsbundle.StrictUnmarshal(raw, &body); err != nil {
			return nil, invalid("run request takes no fields")
		}
	}
	if _, err := s.get(ctx, kindJob, pin); err != nil {
		return nil, s.classified(err)
	}
	if _, err := s.policyFor(ctx, pin); err != nil {
		if shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			return nil, refused(fmt.Errorf("declare the evaluation policy for job %s before requesting a run", pin))
		}
		return nil, s.classified(err)
	}
	if _, err := s.get(ctx, kindReceipt, pin); err == nil {
		return nil, refused(fmt.Errorf("job %s already has a receipt; nothing is left to run", pin))
	} else if !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		return nil, s.classified(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, err := s.get(ctx, kindRunRequest, pin); err == nil {
		return marshal(existing)
	} else if !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		return nil, s.classified(err)
	}
	record := map[string]any{"schema": 1, "kind": kindRunRequest, "manifest_pin": pin, "requested_at": s.cfg.Now().UTC().Format(time.RFC3339Nano), "requested_by": actor}
	if err := s.put(ctx, kindRunRequest, pin, record); err != nil {
		return nil, s.classified(err)
	}
	return marshal(record)
}

// ClaimRun lets a worker take a requested job under a lease. It is refused
// while another claim is live, when no run was requested, and once a
// receipt exists. The response carries the claim and the job's exports so
// the worker knows which cohorts to fetch.
func (s *Service) ClaimRun(ctx context.Context, pin string, raw json.RawMessage) (json.RawMessage, error) {
	if _, err := actorFor(ctx, ""); err != nil {
		return nil, err
	}
	if !recordID.MatchString(pin) {
		return nil, invalid("manifest pin")
	}
	var body struct {
		WorkerID     string `json:"worker_id"`
		LeaseSeconds int    `json:"lease_seconds"`
	}
	if err := weightsbundle.StrictUnmarshal(raw, &body); err != nil {
		return nil, invalid("claim: " + err.Error())
	}
	if !safeID.MatchString(body.WorkerID) {
		return nil, invalid("claim: worker_id must be 1-128 characters of [A-Za-z0-9._:-]")
	}
	if body.LeaseSeconds == 0 {
		body.LeaseSeconds = defaultLeaseSeconds
	}
	if body.LeaseSeconds < 1 || body.LeaseSeconds > maxLeaseSeconds {
		return nil, invalid(fmt.Sprintf("claim: lease_seconds must be between 1 and %d", maxLeaseSeconds))
	}
	job, err := s.get(ctx, kindJob, pin)
	if err != nil {
		return nil, s.classified(err)
	}
	if _, err := s.get(ctx, kindRunRequest, pin); err != nil {
		if shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			return nil, refused(fmt.Errorf("no run was requested for job %s", pin))
		}
		return nil, s.classified(err)
	}
	if _, err := s.get(ctx, kindReceipt, pin); err == nil {
		return nil, refused(fmt.Errorf("job %s already has a receipt; nothing is left to run", pin))
	} else if !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		return nil, s.classified(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.cfg.Now().UTC()
	latest, attempt, err := s.latestClaim(ctx, pin)
	if err != nil {
		return nil, err
	}
	if latest != nil {
		live, err := s.claimLive(ctx, latest, now)
		if err != nil {
			return nil, err
		}
		if live {
			return nil, refused(fmt.Errorf("job %s is claimed by worker %s until %s", pin, str(latest, "worker_id"), str(latest, "expires_at")))
		}
	}
	if attempt >= maxRunAttempts {
		return nil, refused(fmt.Errorf("job %s has been claimed %d times; no further attempt is recorded", pin, attempt))
	}
	attempt++
	id := claimID(pin, attempt)
	record := map[string]any{
		"schema": 1, "kind": kindRunClaim, "id": id, "manifest_pin": pin, "attempt": attempt, "worker_id": body.WorkerID,
		"claimed_at": now.Format(time.RFC3339Nano), "expires_at": now.Add(time.Duration(body.LeaseSeconds) * time.Second).Format(time.RFC3339Nano),
		"lease_seconds": body.LeaseSeconds,
	}
	if err := s.put(ctx, kindRunClaim, id, record); err != nil {
		return nil, s.classified(err)
	}
	return marshal(map[string]any{"claim": record, "exports": job["exports"]})
}

// ReportProgress records a worker's phase under its live claim. A terminal
// phase (done, failed) releases the claim; a later report under it is
// refused.
func (s *Service) ReportProgress(ctx context.Context, pin string, raw json.RawMessage) (json.RawMessage, error) {
	if _, err := actorFor(ctx, ""); err != nil {
		return nil, err
	}
	if !recordID.MatchString(pin) {
		return nil, invalid("manifest pin")
	}
	var body struct {
		ClaimID string          `json:"claim_id"`
		Phase   string          `json:"phase"`
		Message string          `json:"message"`
		Detail  json.RawMessage `json:"detail"`
	}
	if err := weightsbundle.StrictUnmarshal(raw, &body); err != nil {
		return nil, invalid("progress: " + err.Error())
	}
	if !recordID.MatchString(body.ClaimID) {
		return nil, invalid("progress: claim_id")
	}
	if !runPhaseKnown(body.Phase) || body.Phase == "claimed" {
		return nil, invalid("progress: phase must be one of fetching, training, recording_predictions, recording_receipt, sealing, done, failed")
	}
	if len(body.Message) > maxProgressMessage {
		return nil, invalid(fmt.Sprintf("progress: message exceeds %d characters", maxProgressMessage))
	}
	var detail any
	if len(body.Detail) > 0 {
		if len(body.Detail) > 8<<10 {
			return nil, invalid("progress: detail exceeds 8 KiB")
		}
		var object map[string]any
		if err := json.Unmarshal(body.Detail, &object); err != nil {
			return nil, invalid("progress: detail must be a JSON object")
		}
		detail = object
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	latest, _, err := s.latestClaim(ctx, pin)
	if err != nil {
		return nil, err
	}
	if latest == nil || str(latest, "id") != body.ClaimID {
		return nil, refused(fmt.Errorf("claim %s is not the current claim of job %s", body.ClaimID, pin))
	}
	last, seq, err := s.latestProgress(ctx, body.ClaimID)
	if err != nil {
		return nil, err
	}
	if last != nil && runTerminal(str(last, "phase")) {
		return nil, refused(fmt.Errorf("claim %s already ended with %s", body.ClaimID, str(last, "phase")))
	}
	seq++
	id := progressID(body.ClaimID, seq)
	record := map[string]any{
		"schema": 1, "kind": kindRunProgress, "id": id, "manifest_pin": pin, "claim_id": body.ClaimID, "seq": seq,
		"phase": body.Phase, "message": body.Message, "at": s.cfg.Now().UTC().Format(time.RFC3339Nano),
	}
	if detail != nil {
		record["detail"] = detail
	}
	if err := s.put(ctx, kindRunProgress, id, record); err != nil {
		return nil, s.classified(err)
	}
	return marshal(record)
}

// latestClaim probes the attempts recorded for a job and returns the
// newest claim and its attempt number (0 when none).
func (s *Service) latestClaim(ctx context.Context, pin string) (weightsbundle.Record, int, error) {
	var latest weightsbundle.Record
	attempt := 0
	for n := 1; n <= maxRunAttempts; n++ {
		record, err := s.get(ctx, kindRunClaim, claimID(pin, n))
		if err != nil {
			if shoal.IsErrorCode(err, shoal.ErrorNotFound) {
				break
			}
			return nil, 0, s.classified(err)
		}
		latest, attempt = record, n
	}
	return latest, attempt, nil
}

// latestProgress returns the newest progress record of a claim and its
// sequence number (0 when none).
func (s *Service) latestProgress(ctx context.Context, claim string) (weightsbundle.Record, int, error) {
	var latest weightsbundle.Record
	seq := 0
	for n := 1; n <= maxRunProgress; n++ {
		record, err := s.get(ctx, kindRunProgress, progressID(claim, n))
		if err != nil {
			if shoal.IsErrorCode(err, shoal.ErrorNotFound) {
				break
			}
			return nil, 0, s.classified(err)
		}
		latest, seq = record, n
	}
	return latest, seq, nil
}

// claimLive reports whether a claim still holds the job: its lease has not
// expired and no terminal progress was recorded under it.
func (s *Service) claimLive(ctx context.Context, claim weightsbundle.Record, now time.Time) (bool, error) {
	expires, err := time.Parse(time.RFC3339Nano, str(claim, "expires_at"))
	if err != nil || !now.Before(expires) {
		return false, nil
	}
	last, _, err := s.latestProgress(ctx, str(claim, "id"))
	if err != nil {
		return false, err
	}
	return last == nil || !runTerminal(str(last, "phase")), nil
}

// runView is the job view's "run" member: whether a run was requested,
// the current claim and whether it is live, and the latest progress.
func (s *Service) runView(ctx context.Context, pin string) (map[string]any, error) {
	view := map[string]any{"requested": false, "claim": nil, "progress": nil, "attempts": 0, "runnable": false}
	request, err := s.get(ctx, kindRunRequest, pin)
	if err != nil {
		if shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			return view, nil
		}
		return nil, s.classified(err)
	}
	view["requested"] = true
	view["requested_at"] = request["requested_at"]
	view["requested_by"] = request["requested_by"]
	claim, attempt, err := s.latestClaim(ctx, pin)
	if err != nil {
		return nil, err
	}
	view["attempts"] = attempt
	live := false
	if claim != nil {
		live, err = s.claimLive(ctx, claim, s.cfg.Now().UTC())
		if err != nil {
			return nil, err
		}
		claimView := map[string]any{}
		for k, v := range claim {
			claimView[k] = v
		}
		claimView["live"] = live
		view["claim"] = claimView
		if progress, _, err := s.latestProgress(ctx, str(claim, "id")); err != nil {
			return nil, err
		} else if progress != nil {
			view["progress"] = progress
		}
	}
	receipt := false
	if _, err := s.get(ctx, kindReceipt, pin); err == nil {
		receipt = true
	} else if !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		return nil, s.classified(err)
	}
	view["runnable"] = !live && !receipt
	return view, nil
}
