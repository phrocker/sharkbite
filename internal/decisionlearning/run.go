// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisionlearning

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/phrocker/shoal-oss/internal/storage"
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
// it got, so the console can show it and a worker can find it. The newest
// attempt of each job and the newest sequence of each claim are kept in an
// in-memory index built from the store once and maintained on every
// append, so a read costs at most two record reads and an append one
// exclusive write, however many attempts a job has seen.
const (
	kindRunRequest  = "distillation-run-request"
	kindRunClaim    = "distillation-run-claim"
	kindRunProgress = "distillation-run-progress"

	// maxRunAttempts bounds the claims recorded for one job. A job past it
	// is exhausted: no worker can claim it again and the view says so.
	maxRunAttempts = 100
	// maxRunProgress bounds the progress records of one claim; a worker
	// reports a handful of phases.
	maxRunProgress = 64

	defaultLeaseSeconds = 600
	minLeaseSeconds     = 30
	maxLeaseSeconds     = 6 * 3600
	maxProgressMessage  = 1000
	maxProgressDetail   = 2 << 10
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

// runIndex is the newest attempt per job and the newest sequence per
// claim, built from the store once and maintained on every append. A
// write the store refuses as already present means another writer got
// there first; the index is then rebuilt before the next use.
type runIndex struct {
	mu       sync.Mutex
	built    bool
	attempts map[string]int
	seqs     map[string]int
}

func recordInt(r weightsbundle.Record, key string) int {
	n, _ := strconv.Atoi(fmt.Sprint(r[key]))
	return n
}

// ensureRunIndex builds the index when it is absent or was invalidated.
// The caller holds idx.mu.
func (s *Service) ensureRunIndex(ctx context.Context) error {
	if s.runs.built {
		return nil
	}
	attempts, seqs := map[string]int{}, map[string]int{}
	claims, err := s.list(ctx, kindRunClaim)
	if err != nil {
		return s.classifyLogged("run index", err)
	}
	for _, claim := range claims {
		pin, attempt := str(claim, "manifest_pin"), recordInt(claim, "attempt")
		if attempt > attempts[pin] {
			attempts[pin] = attempt
		}
	}
	progress, err := s.list(ctx, kindRunProgress)
	if err != nil {
		return s.classifyLogged("run index", err)
	}
	for _, p := range progress {
		claim, seq := str(p, "claim_id"), recordInt(p, "seq")
		if seq > seqs[claim] {
			seqs[claim] = seq
		}
	}
	s.runs.attempts, s.runs.seqs, s.runs.built = attempts, seqs, true
	return nil
}

// latestAttempt returns the newest attempt number of a job (0 when none).
func (s *Service) latestAttempt(ctx context.Context, pin string) (int, error) {
	s.runs.mu.Lock()
	defer s.runs.mu.Unlock()
	if err := s.ensureRunIndex(ctx); err != nil {
		return 0, err
	}
	return s.runs.attempts[pin], nil
}

// latestSeq returns the newest progress sequence of a claim (0 when none).
func (s *Service) latestSeq(ctx context.Context, claim string) (int, error) {
	s.runs.mu.Lock()
	defer s.runs.mu.Unlock()
	if err := s.ensureRunIndex(ctx); err != nil {
		return 0, err
	}
	return s.runs.seqs[claim], nil
}

// appendRun writes a run record exclusively and advances the index, or
// invalidates the index when the store already holds the record.
func (s *Service) appendRun(ctx context.Context, kind, id string, record map[string]any, advance func()) error {
	err := s.put(ctx, kind, id, record)
	s.runs.mu.Lock()
	defer s.runs.mu.Unlock()
	if err != nil {
		if errors.Is(err, storage.ErrExists) || errors.Is(err, weightsbundle.ErrExists) {
			s.runs.built = false
			return refused(fmt.Errorf("%s %s was recorded by another writer; retry", kind, id))
		}
		return s.classified(err)
	}
	if s.runs.built {
		advance()
	}
	return nil
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
	if err := s.noReceipt(ctx, pin); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, err := s.get(ctx, kindRunRequest, pin); err == nil {
		return marshal(existing)
	} else if !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		return nil, s.classified(err)
	}
	record := map[string]any{"schema": 1, "kind": kindRunRequest, "manifest_pin": pin, "requested_at": s.cfg.Now().UTC().Format(time.RFC3339Nano), "requested_by": actor}
	if err := s.appendRun(ctx, kindRunRequest, pin, record, func() {}); err != nil {
		return nil, err
	}
	return marshal(record)
}

func (s *Service) noReceipt(ctx context.Context, pin string) error {
	if _, err := s.get(ctx, kindReceipt, pin); err == nil {
		return refused(fmt.Errorf("job %s already has a receipt; nothing is left to run", pin))
	} else if !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		return s.classified(err)
	}
	return nil
}

// ClaimRun lets a worker take a requested job under a lease. It is refused
// while another claim is live, when no run was requested, once a receipt
// exists, and once the job's attempts are exhausted. The response carries
// the claim and the job's exports so the worker knows which cohorts to
// fetch.
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
	if body.LeaseSeconds < minLeaseSeconds || body.LeaseSeconds > maxLeaseSeconds {
		return nil, invalid(fmt.Sprintf("claim: lease_seconds must be between %d and %d", minLeaseSeconds, maxLeaseSeconds))
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
	if err := s.noReceipt(ctx, pin); err != nil {
		return nil, err
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
		return nil, refused(fmt.Errorf("job %s is exhausted: it has been claimed %d times and no further attempt is recorded", pin, attempt))
	}
	attempt++
	id := claimID(pin, attempt)
	record := map[string]any{
		"schema": 1, "kind": kindRunClaim, "id": id, "manifest_pin": pin, "attempt": attempt, "worker_id": body.WorkerID,
		"claimed_at": now.Format(time.RFC3339Nano), "expires_at": now.Add(time.Duration(body.LeaseSeconds) * time.Second).Format(time.RFC3339Nano),
		"lease_seconds": body.LeaseSeconds,
	}
	if err := s.appendRun(ctx, kindRunClaim, id, record, func() { s.runs.attempts[pin] = attempt }); err != nil {
		return nil, err
	}
	return marshal(map[string]any{"claim": record, "exports": job["exports"]})
}

// ReportProgress records a worker's phase under its live claim: the
// newest claim of the job, unexpired, with no terminal progress. A
// terminal phase (done, failed) releases the claim; done is accepted only
// once the job's receipt is recorded, so a claim cannot end as done
// without the run having been recorded.
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
		if len(body.Detail) > maxProgressDetail {
			return nil, invalid(fmt.Sprintf("progress: detail exceeds %d bytes", maxProgressDetail))
		}
		var object map[string]any
		if err := json.Unmarshal(body.Detail, &object); err != nil {
			return nil, invalid("progress: detail must be a JSON object")
		}
		detail = object
	}
	if body.Phase == "done" {
		if _, err := s.get(ctx, kindReceipt, pin); err != nil {
			if shoal.IsErrorCode(err, shoal.ErrorNotFound) {
				return nil, refused(fmt.Errorf("job %s has no receipt; a run is not done until its receipt is recorded", pin))
			}
			return nil, s.classified(err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.cfg.Now().UTC()
	latest, _, err := s.latestClaim(ctx, pin)
	if err != nil {
		return nil, err
	}
	if latest == nil || str(latest, "id") != body.ClaimID {
		return nil, refused(fmt.Errorf("claim %s is not the current claim of job %s", body.ClaimID, pin))
	}
	if expires, err := time.Parse(time.RFC3339Nano, str(latest, "expires_at")); err != nil || !now.Before(expires) {
		return nil, refused(fmt.Errorf("claim %s expired at %s; the job may be claimed again", body.ClaimID, str(latest, "expires_at")))
	}
	last, seq, err := s.latestProgress(ctx, body.ClaimID)
	if err != nil {
		return nil, err
	}
	if last != nil && runTerminal(str(last, "phase")) {
		return nil, refused(fmt.Errorf("claim %s already ended with %s", body.ClaimID, str(last, "phase")))
	}
	if seq >= maxRunProgress {
		return nil, refused(fmt.Errorf("claim %s has %d progress records; no further report is recorded", body.ClaimID, seq))
	}
	seq++
	id := progressID(body.ClaimID, seq)
	record := map[string]any{
		"schema": 1, "kind": kindRunProgress, "id": id, "manifest_pin": pin, "claim_id": body.ClaimID, "seq": seq,
		"phase": body.Phase, "message": body.Message, "at": now.Format(time.RFC3339Nano),
	}
	if detail != nil {
		record["detail"] = detail
	}
	claim := body.ClaimID
	if err := s.appendRun(ctx, kindRunProgress, id, record, func() { s.runs.seqs[claim] = seq }); err != nil {
		return nil, err
	}
	return marshal(record)
}

// latestClaim returns the newest claim of a job and its attempt number
// (nil and 0 when none), through the index.
func (s *Service) latestClaim(ctx context.Context, pin string) (weightsbundle.Record, int, error) {
	attempt, err := s.latestAttempt(ctx, pin)
	if err != nil || attempt == 0 {
		return nil, 0, err
	}
	record, err := s.get(ctx, kindRunClaim, claimID(pin, attempt))
	if err != nil {
		return nil, 0, s.classified(err)
	}
	return record, attempt, nil
}

// latestProgress returns the newest progress record of a claim and its
// sequence number (nil and 0 when none), through the index.
func (s *Service) latestProgress(ctx context.Context, claim string) (weightsbundle.Record, int, error) {
	seq, err := s.latestSeq(ctx, claim)
	if err != nil || seq == 0 {
		return nil, 0, err
	}
	record, err := s.get(ctx, kindRunProgress, progressID(claim, seq))
	if err != nil {
		return nil, 0, s.classified(err)
	}
	return record, seq, nil
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
// the current claim and whether it is live, the latest progress, and
// whether a worker may claim the job now.
func (s *Service) runView(ctx context.Context, pin string) (map[string]any, error) {
	view := map[string]any{"requested": false, "claim": nil, "progress": nil, "attempts": 0, "runnable": false, "exhausted": false}
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
	exhausted := attempt >= maxRunAttempts
	view["exhausted"] = exhausted
	live := false
	if claim != nil {
		progress, _, err := s.latestProgress(ctx, str(claim, "id"))
		if err != nil {
			return nil, err
		}
		expires, parseErr := time.Parse(time.RFC3339Nano, str(claim, "expires_at"))
		live = parseErr == nil && s.cfg.Now().UTC().Before(expires) && (progress == nil || !runTerminal(str(progress, "phase")))
		claimView := map[string]any{}
		for k, v := range claim {
			claimView[k] = v
		}
		claimView["live"] = live
		view["claim"] = claimView
		if progress != nil {
			view["progress"] = progress
		}
	}
	receipt := false
	if _, err := s.get(ctx, kindReceipt, pin); err == nil {
		receipt = true
	} else if !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		return nil, s.classified(err)
	}
	view["runnable"] = !live && !receipt && !exhausted
	return view, nil
}
