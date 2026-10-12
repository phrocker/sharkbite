// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Record is an action record decoded from the wire: IDs as bytes and the
// executor key decoded. Input and ExecutorKey are populated on the claim
// response only; the pull page never carries them, and re-reading the action
// does not return them, so a worker keeps them from the claim.
type Record struct {
	ID              []byte
	Version         uint64
	State           DispatchState
	AgentID         []byte
	AgentGeneration int64
	Capability      string
	Action          string
	Input           json.RawMessage
	Output          json.RawMessage
	ErrorCode       string
	ErrorCodeOrigin string
	Deadline        time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
	// ExecutorKey is the decoded key, ExecutorKeyBytes long, or nil when the
	// response carried none.
	ExecutorKey     []byte
	ClaimID         []byte
	ClaimFence      uint64
	ClaimLeaseUntil time.Time
	EffectPossible  bool
	// Effected is the volume a failed record says escaped before the failure
	// (#427), zero when none was reported.
	Effected Effected
	// CorrelationID is the record's correlation_id, which every request about
	// this action carries (#527).
	CorrelationID []byte
	// AmbiguityReports are the lost-fence reports on the record (#438). The
	// reporter's identity and time are the explorer's and are not decoded.
	AmbiguityReports []RecordedAmbiguity
}

// RecordedAmbiguity is one ambiguity report as the record holds it.
type RecordedAmbiguity struct {
	ClaimFence uint64
	Outcome    AmbiguityOutcome
	Target     string
	Reference  string
}

// Correlate returns ctx carrying this record's correlation ID, which is what
// every claim, extend, complete and ambiguity request about it sends.
func (r Record) Correlate(ctx Context) Context {
	ctx.CorrelationID = append([]byte(nil), r.CorrelationID...)
	return ctx
}

// recordWire is the projection of an action record the client decodes: the
// fields a worker's decisions read, and nothing else. A response field outside
// it — evidence, the evidence snapshot, request_id, an ambiguity report's
// reporter — is passed over unread (decodeResponse ignores unknown keys and
// does not descend into them), so a later explorer that reshapes one cannot
// fail a decode and turn every completion into a resend. error_code_origin is
// carried raw and read only if it is a string. Action is the full wire type
// the explorer encodes; DecodeRecord reads one already decoded.
type recordWire struct {
	ID               string                  `json:"id"`
	Version          uint64                  `json:"version"`
	State            DispatchState           `json:"state"`
	AgentID          string                  `json:"agent_id"`
	AgentGeneration  int64                   `json:"agent_generation"`
	Capability       string                  `json:"capability"`
	Action           string                  `json:"action"`
	Input            json.RawMessage         `json:"input,omitempty"`
	Output           json.RawMessage         `json:"output,omitempty"`
	ErrorCode        string                  `json:"error_code,omitempty"`
	ErrorCodeOrigin  json.RawMessage         `json:"error_code_origin,omitempty"`
	Effected         *Effected               `json:"effected,omitempty"`
	CorrelationID    string                  `json:"correlation_id,omitempty"`
	Deadline         time.Time               `json:"deadline"`
	CreatedAt        time.Time               `json:"created_at"`
	UpdatedAt        time.Time               `json:"updated_at"`
	ExecutorKey      string                  `json:"executor_key,omitempty"`
	ClaimID          string                  `json:"claim_id,omitempty"`
	ClaimFence       uint64                  `json:"claim_fence,omitempty"`
	ClaimLeaseUntil  time.Time               `json:"claim_lease_until,omitempty"`
	EffectPossible   bool                    `json:"effect_possible"`
	AmbiguityReports []recordedAmbiguityWire `json:"ambiguity_reports,omitempty"`
}

// recordedAmbiguityWire is the part of a report a worker matches on.
type recordedAmbiguityWire struct {
	ClaimFence uint64           `json:"claim_fence"`
	Outcome    AmbiguityOutcome `json:"outcome"`
	Target     string           `json:"target,omitempty"`
	Reference  string           `json:"reference,omitempty"`
}

// pageWire is the projection of a pull page.
type pageWire struct {
	Actions []recordWire `json:"actions"`
	Next    string       `json:"next,omitempty"`
}

// DecodeRecord decodes a wire record. Every ID must be unpadded base64url and
// the action ID must be present; an executor key, when present, must be
// exactly ExecutorKeyBytes in either spelling ParseExecutorKey accepts.
func DecodeRecord(w Action) (Record, error) {
	reports := make([]recordedAmbiguityWire, len(w.AmbiguityReports))
	for i, report := range w.AmbiguityReports {
		reports[i] = recordedAmbiguityWire{
			ClaimFence: report.ClaimFence, Outcome: report.Outcome,
			Target: report.Target, Reference: report.Reference,
		}
	}
	if len(reports) == 0 {
		reports = nil
	}
	var origin json.RawMessage
	if w.ErrorCodeOrigin != "" {
		origin, _ = json.Marshal(w.ErrorCodeOrigin)
	}
	return decodeRecord(recordWire{
		ID: w.ID, Version: w.Version, State: w.State, AgentID: w.AgentID,
		AgentGeneration: w.AgentGeneration, Capability: w.Capability, Action: w.Action,
		Input: w.Input, Output: w.Output, ErrorCode: w.ErrorCode, ErrorCodeOrigin: origin,
		Effected: w.Effected, CorrelationID: w.CorrelationID, Deadline: w.Deadline,
		CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt, ExecutorKey: w.ExecutorKey,
		ClaimID: w.ClaimID, ClaimFence: w.ClaimFence, ClaimLeaseUntil: w.ClaimLeaseUntil,
		EffectPossible: w.EffectPossible, AmbiguityReports: reports,
	})
}

func decodeRecord(w recordWire) (Record, error) {
	id, err := DecodeID(w.ID)
	if err != nil || len(id) == 0 {
		return Record{}, errors.New("action ID is not unpadded base64url")
	}
	agent, err := DecodeID(w.AgentID)
	if err != nil {
		return Record{}, errors.New("agent ID is not unpadded base64url")
	}
	claimID, err := DecodeID(w.ClaimID)
	if err != nil {
		return Record{}, errors.New("claim ID is not unpadded base64url")
	}
	correlation, err := DecodeID(w.CorrelationID)
	if err != nil {
		return Record{}, errors.New("correlation ID is not unpadded base64url")
	}
	record := Record{
		ID: id, Version: w.Version, State: w.State, AgentID: agent,
		AgentGeneration: w.AgentGeneration, Capability: w.Capability,
		Action: w.Action, Input: w.Input, Output: w.Output,
		ErrorCode: w.ErrorCode, Deadline: w.Deadline, CreatedAt: w.CreatedAt,
		UpdatedAt: w.UpdatedAt, ClaimID: claimID, ClaimFence: w.ClaimFence,
		ClaimLeaseUntil: w.ClaimLeaseUntil, EffectPossible: w.EffectPossible,
	}
	// Informational only: read when it is a string, ignored otherwise.
	var origin string
	if json.Unmarshal(w.ErrorCodeOrigin, &origin) == nil {
		record.ErrorCodeOrigin = origin
	}
	if len(correlation) > 0 {
		record.CorrelationID = correlation
	}
	for _, report := range w.AmbiguityReports {
		record.AmbiguityReports = append(record.AmbiguityReports, RecordedAmbiguity{
			ClaimFence: report.ClaimFence, Outcome: report.Outcome,
			Target: report.Target, Reference: report.Reference,
		})
	}
	if w.Effected != nil {
		record.Effected = *w.Effected
	}
	if w.ExecutorKey != "" {
		key, err := ParseExecutorKey(w.ExecutorKey)
		if err != nil {
			return Record{}, fmt.Errorf("executor key: %w", err)
		}
		record.ExecutorKey = key
	}
	return record, nil
}
