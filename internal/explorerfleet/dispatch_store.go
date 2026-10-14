// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package explorerfleet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/gob"
	"encoding/hex"
	"errors"
	"reflect"
	"time"

	"github.com/phrocker/shoal-oss/internal/explorercoord"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/guard"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/transaction"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	DispatchTable       = "_shoal_explorer_fleet_dispatch"
	dispatchKind   byte = 'D'
	transitionKind byte = 'T'
)

var (
	dispatchFamily    = []byte("r")
	dispatchQualifier = []byte("action")
	dispatchPolicy    = []byte("fleet/dispatch/v1")
	dispatchMagic     = []byte{'S', 'F', 'D', 1}
	transitionMagic   = []byte{'S', 'F', 'T', 1}
)

type storedTransition struct {
	Transition  fleet.ActionTransition
	CompletedAt time.Time
}

type DispatchStore struct {
	runtime    DispatchRuntime
	visibility []byte
}

type DispatchRuntime interface {
	Runtime
	CurrentHead(context.Context) (coordination.AllocatorHeadV1, error)
	ReadCommittedCell(
		context.Context,
		string,
		[]byte,
		[]byte,
		[]byte,
		[]byte,
		coordination.Epoch,
	) (explorercoord.CommittedCell, bool, error)
}

func NewDispatchStore(runtime DispatchRuntime, visibility []byte) (*DispatchStore, error) {
	if runtime == nil {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "fleet dispatch runtime is required")
	}
	if len(visibility) > coordination.MaxCoordinateBytes {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "fleet dispatch visibility exceeds its bound")
	}
	return &DispatchStore{runtime: runtime, visibility: append([]byte(nil), visibility...)}, nil
}

func (s *DispatchStore) ScanActions(ctx context.Context, after []byte, limit int) (fleet.ActionPage, error) {
	if len(after) > 0 {
		if err := dispatchID(after); err != nil {
			return fleet.ActionPage{}, err
		}
	}
	if limit <= 0 || limit > fleet.MaxDispatchListResults {
		return fleet.ActionPage{}, shoal.NewError(shoal.ErrorInvalidArgument, "dispatch scan limit is outside its bound")
	}
	head, err := s.runtime.CurrentHead(ctx)
	if err != nil {
		return fleet.ActionPage{}, publicError("action", err)
	}
	var startAfter []byte
	if len(after) > 0 {
		startAfter = dispatchRow(after)
	}
	page, err := s.runtime.ScanCommitted(ctx, explorercoord.CommittedScanRequest{
		Table: DispatchTable, RowPrefix: []byte("action/"), StartAfterRow: startAfter,
		Family: dispatchFamily, Qualifier: dispatchQualifier, Visibility: s.visibility,
		Frontier: head.Frontier, Limit: limit,
		MaxScanned: explorercoord.MaxCommittedScanCells,
	})
	if err != nil {
		return fleet.ActionPage{}, publicError("action", err)
	}
	result := fleet.ActionPage{Actions: make([]fleet.ActionRecord, 0, len(page.Cells))}
	for _, cell := range page.Cells {
		record, decodeErr := decodeAction(cell.Cell.Value)
		if decodeErr != nil {
			return fleet.ActionPage{}, shoal.WrapError(shoal.ErrorInternal, "invalid committed fleet action", decodeErr)
		}
		result.Actions = append(result.Actions, record)
	}
	if len(page.NextRow) > len("action/") && bytes.HasPrefix(page.NextRow, []byte("action/")) {
		result.Next = append([]byte(nil), page.NextRow[len("action/"):]...)
	}
	return result, nil
}

func DispatchPhysicalTable() string { return DispatchTable }

func (s *DispatchStore) GetAction(ctx context.Context, id []byte) (fleet.ActionRecord, error) {
	record, _, err := s.readAction(ctx, id)
	return record, err
}

func (s *DispatchStore) readAction(
	ctx context.Context,
	id []byte,
) (fleet.ActionRecord, *guard.Head, error) {
	if err := dispatchID(id); err != nil {
		return fleet.ActionRecord{}, nil, err
	}
	// Head and committed cell are read together, and re-read while the cell
	// for the head's epoch is not visible yet (#633).
	//
	// dispatchEntity and dispatchRow are both keyed on id alone, so a non-nil
	// head proves this row exists: a missing committed cell is a window, never
	// absence. Answering not-found there reported a live action as absent, and
	// the effects gateway treats that as definite on its first Complete
	// attempt. Answering a new error instead is not the fix either — the lag
	// already has a mechanism one layer up (fleet.materializeRead, which
	// classifies it as "look again shortly" and is retried by
	// ApprovalService.Request), and that mechanism keys on the error's
	// identity, so changing the identity silently disabled it.
	//
	// So the lag is closed here, where the epoch is known, and escapes only
	// as the bounded-exhaustion case below. The head is re-read too: if a
	// concurrent claim or extend advanced the epoch again, the newer head is
	// the one whose cell we want.
	var head *guard.Head
	var value []byte
	for attempt := 0; ; attempt++ {
		var err error
		head, _, err = s.runtime.ReadEntity(ctx, dispatchEntity(id))
		if err != nil {
			if errors.Is(err, guard.ErrNotFound) ||
				errors.Is(err, transaction.ErrNotFound) {
				return fleet.ActionRecord{}, nil, fleet.ErrActionNotFound
			}
			return fleet.ActionRecord{}, nil, publicError("action", err)
		}
		if head == nil {
			return fleet.ActionRecord{}, nil, fleet.ErrActionNotFound
		}
		value, err = s.readCommittedAction(ctx, dispatchRow(id), head.Epoch)
		if err == nil {
			break
		}
		if !errors.Is(err, transaction.ErrNotFound) {
			return fleet.ActionRecord{}, nil, publicError("action", err)
		}
		if attempt == maxCommittedReadAttempts-1 {
			return fleet.ActionRecord{}, nil, committedValueNotVisible("action")
		}
	}
	record, err := decodeAction(value)
	if err != nil {
		return fleet.ActionRecord{}, nil, shoal.WrapError(shoal.ErrorInternal, "invalid committed fleet action", err)
	}
	return record, head, nil
}

func (s *DispatchStore) readCommittedAction(
	ctx context.Context,
	row []byte,
	epoch coordination.Epoch,
) ([]byte, error) {
	cell, ok, err := s.runtime.ReadCommittedCell(
		ctx,
		DispatchTable,
		row,
		dispatchFamily,
		dispatchQualifier,
		s.visibility,
		epoch,
	)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, transaction.ErrNotFound
	}
	return append([]byte(nil), cell.Cell.Value...), nil
}

func (s *DispatchStore) ApplyAction(ctx context.Context, mutation fleet.DispatchMutation) (fleet.ActionRecord, error) {
	if err := dispatchID(mutation.Record.ID); err != nil {
		return fleet.ActionRecord{}, err
	}
	if err := dispatchID(mutation.Token); err != nil {
		return fleet.ActionRecord{}, err
	}
	if mutation.Record.Version != mutation.ExpectedVersion+1 {
		return fleet.ActionRecord{}, shoal.NewError(shoal.ErrorInvalidArgument, "fleet action version transition is invalid")
	}
	value, err := encodeAction(mutation.Record)
	if err != nil {
		return fleet.ActionRecord{}, err
	}
	canonical, err := decodeAction(value)
	if err != nil {
		return fleet.ActionRecord{}, shoal.WrapError(
			shoal.ErrorInternal, "canonicalize fleet action", err)
	}
	var transition fleet.ActionTransition
	var transitionValue []byte
	if mutation.TransitionKind != "" {
		transition, err = fleet.NewActionTransition(
			mutation.Token, mutation.TransitionKind, canonical)
		if err != nil {
			return fleet.ActionRecord{}, err
		}
		transitionValue, err = encodeTransition(storedTransition{
			Transition: transition,
		})
		if err != nil {
			return fleet.ActionRecord{}, err
		}
	}
	if current, readErr := s.GetAction(ctx, mutation.Record.ID); readErr == nil &&
		reflect.DeepEqual(current, canonical) {
		if mutation.TransitionKind != "" {
			if err := s.ensureTransition(ctx, transition, transitionValue); err != nil {
				return fleet.ActionRecord{}, err
			}
		}
		return current, nil
	}
	lpart, err := explorercoord.Partition(
		coordination.DomainID("fleet-dispatch"), canonical.ID)
	if err != nil {
		return fleet.ActionRecord{}, publicError("action", err)
	}
	actionGuard := explorercoord.GuardIntent{
		Entity: dispatchEntity(canonical.ID), DesiredState: guard.StateLive,
		DesiredWinnerID: append([]byte(nil), canonical.ID...), LPART: lpart,
		LogicalPolicyID: dispatchPolicy, RetirementGeneration: 1,
	}
	cells := []explorercoord.Cell{{
		Table: DispatchTable, Row: dispatchRow(canonical.ID),
		Family: dispatchFamily, Qualifier: dispatchQualifier,
		Visibility: s.visibility, Value: value, EpochTimestamp: true,
		LPART: lpart, CopyGeneration: 1,
	}}
	guards := []explorercoord.GuardIntent{actionGuard}
	results := []explorercoord.ResultIdentity{{
		Kind: []byte("fleet-action"), ID: append([]byte(nil), canonical.ID...),
	}}
	if mutation.TransitionKind != "" {
		transitionGuard := explorercoord.GuardIntent{
			Entity: transitionEntity(transition.ID), Mode: guard.ModeAbsentOrIdentical,
			DesiredState:    guard.StateLive,
			DesiredWinnerID: transitionWinner(transitionValue), LPART: lpart,
			LogicalPolicyID: dispatchPolicy, RetirementGeneration: 1,
		}
		cells = append(cells, explorercoord.Cell{
			Table: DispatchTable,
			Row: transitionRow(
				transition.Record.ID, transition.Record.Version, transition.ID),
			Family: dispatchFamily, Qualifier: dispatchQualifier,
			Visibility: s.visibility, Value: transitionValue, EpochTimestamp: true,
			LPART: lpart, CopyGeneration: 1,
		})
		guards = append(guards, transitionGuard)
		results = append(results, explorercoord.ResultIdentity{
			Kind: []byte("fleet-action-transition"),
			ID:   append([]byte(nil), transition.ID...),
		})
	}
	if mutation.ExpectedVersion == 0 {
		current, readErr := s.GetAction(ctx, canonical.ID)
		if readErr == nil {
			if bytes.Equal(current.IdempotencyKey, canonical.IdempotencyKey) &&
				reflect.DeepEqual(current, canonical) {
				return current, nil
			}
			return fleet.ActionRecord{}, fleet.ErrActionConflict
		}
		if !errors.Is(readErr, fleet.ErrActionNotFound) {
			return fleet.ActionRecord{}, readErr
		}
		actionGuard.Mode = guard.ModeAbsentOrIdentical
	} else {
		current, head, readErr := s.readAction(ctx, canonical.ID)
		if readErr != nil {
			return fleet.ActionRecord{}, readErr
		}
		if current.Version != mutation.ExpectedVersion ||
			current.ClaimFence != mutation.ExpectedFence {
			return fleet.ActionRecord{}, fleet.ErrActionConflict
		}
		if err := refuseRewrittenIdentity(current, canonical); err != nil {
			return fleet.ActionRecord{}, err
		}
		actionGuard.Mode = guard.ModeMutate
		actionGuard.ExpectedEpoch = head.Epoch
		actionGuard.ExpectedDigest = head.LogicalDigest
	}
	guards[0] = actionGuard
	intent := explorercoord.Intent{
		Operation: []byte("fleet.dispatch.apply.v1"),
		Token:     append([]byte(nil), mutation.Token...),
		Cells:     cells, Guards: guards, Results: results,
	}
	_, publishErr := s.runtime.Publish(ctx, explorercoord.Request{Intent: intent})
	if publishErr != nil {
		resolve := context.WithoutCancel(ctx)
		for attempt := 0; attempt < 10; attempt++ {
			current, readErr := s.GetAction(resolve, canonical.ID)
			if readErr == nil {
				if reflect.DeepEqual(current, canonical) {
					if mutation.TransitionKind != "" {
						if ensureErr := s.ensureTransition(
							resolve, transition, transitionValue,
						); ensureErr != nil {
							return fleet.ActionRecord{}, ensureErr
						}
					}
					return current, nil
				}
				if current.Version >= canonical.Version {
					return fleet.ActionRecord{}, fleet.ErrActionConflict
				}
			}
			time.Sleep(time.Millisecond)
		}
		if errors.Is(publishErr, explorercoord.ErrIndeterminatePublication) {
			return fleet.ActionRecord{}, shoal.WrapError(shoal.ErrorUnavailable, "fleet action publication is indeterminate", publishErr)
		}
		return fleet.ActionRecord{}, publicError("action", publishErr)
	}
	stored, err := s.GetAction(ctx, canonical.ID)
	if err != nil {
		return fleet.ActionRecord{}, err
	}
	if !reflect.DeepEqual(stored, canonical) {
		return fleet.ActionRecord{}, fleet.ErrActionConflict
	}
	return stored, nil
}

func (s *DispatchStore) PendingActionTransitions(
	ctx context.Context, actionID, after []byte, limit int,
) (fleet.ActionTransitionPage, error) {
	if err := dispatchID(actionID); err != nil {
		return fleet.ActionTransitionPage{}, err
	}
	if len(after) > 0 {
		if err := dispatchID(after); err != nil {
			return fleet.ActionTransitionPage{}, err
		}
	}
	if limit <= 0 || limit > fleet.MaxDispatchListResults {
		return fleet.ActionTransitionPage{}, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"transition scan limit is outside its bound")
	}
	head, err := s.runtime.CurrentHead(ctx)
	if err != nil {
		return fleet.ActionTransitionPage{}, publicError("action", err)
	}
	var startAfter []byte
	if len(after) > 0 {
		startAfter = append(transitionPrefix(actionID), after...)
	}
	prefix := transitionPrefix(actionID)
	page, err := s.runtime.ScanCommitted(ctx, explorercoord.CommittedScanRequest{
		Table: DispatchTable, RowPrefix: prefix,
		StartAfterRow: startAfter, Family: dispatchFamily,
		Qualifier: dispatchQualifier, Visibility: s.visibility,
		Frontier: head.Frontier, Limit: limit,
		MaxScanned: explorercoord.MaxCommittedScanCells,
	})
	if err != nil {
		return fleet.ActionTransitionPage{}, publicError("action", err)
	}
	result := fleet.ActionTransitionPage{
		Transitions: make([]fleet.ActionTransition, 0, len(page.Cells)),
	}
	for _, cell := range page.Cells {
		stored, decodeErr := decodeTransition(cell.Cell.Value)
		if decodeErr != nil {
			return fleet.ActionTransitionPage{}, shoal.WrapError(
				shoal.ErrorInternal, "invalid committed fleet transition", decodeErr)
		}
		if stored.CompletedAt.IsZero() {
			result.Transitions = append(
				result.Transitions, cloneTransition(stored.Transition))
		}
	}
	if len(page.NextRow) > len(prefix) &&
		bytes.HasPrefix(page.NextRow, prefix) {
		result.Next = append([]byte(nil), page.NextRow[len(prefix):]...)
	}
	return result, nil
}

func (s *DispatchStore) CompleteActionTransition(
	ctx context.Context, transition fleet.ActionTransition,
) error {
	if err := transition.Validate(); err != nil {
		return err
	}
	current, head, err := s.readTransition(
		ctx, transition.Record.ID, transition.Record.Version, transition.ID)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current.Transition, transition) {
		return fleet.ErrActionConflict
	}
	if !current.CompletedAt.IsZero() {
		return nil
	}
	current.CompletedAt = transition.Record.UpdatedAt
	value, err := encodeTransition(current)
	if err != nil {
		return err
	}
	lpart, err := explorercoord.Partition(
		coordination.DomainID("fleet-dispatch"), transition.Record.ID)
	if err != nil {
		return publicError("action", err)
	}
	_, err = s.runtime.Publish(ctx, explorercoord.Request{Intent: explorercoord.Intent{
		Operation: []byte("fleet.dispatch.complete-transition.v1"),
		Token:     transitionCompletionToken(transition.ID),
		Cells: []explorercoord.Cell{{
			Table: DispatchTable,
			Row: transitionRow(
				transition.Record.ID, transition.Record.Version, transition.ID),
			Family: dispatchFamily, Qualifier: dispatchQualifier,
			Visibility: s.visibility, Value: value, EpochTimestamp: true,
			LPART: lpart, CopyGeneration: 1,
		}},
		Guards: []explorercoord.GuardIntent{{
			Entity: transitionEntity(transition.ID), Mode: guard.ModeMutate,
			ExpectedEpoch: head.Epoch, ExpectedDigest: head.LogicalDigest,
			DesiredState:    guard.StateLive,
			DesiredWinnerID: transitionWinner(value), LPART: lpart,
			LogicalPolicyID: dispatchPolicy, RetirementGeneration: 1,
		}},
		Results: []explorercoord.ResultIdentity{{
			Kind: []byte("fleet-action-transition-complete"),
			ID:   append([]byte(nil), transition.ID...),
		}},
	}})
	if err != nil {
		resolve := context.WithoutCancel(ctx)
		replayed, _, readErr := s.readTransition(
			resolve, transition.Record.ID, transition.Record.Version, transition.ID)
		if readErr == nil && !replayed.CompletedAt.IsZero() &&
			reflect.DeepEqual(replayed.Transition, transition) {
			return nil
		}
		return publicError("action", err)
	}
	return nil
}

func (s *DispatchStore) ensureTransition(
	ctx context.Context, transition fleet.ActionTransition, value []byte,
) error {
	current, _, err := s.readTransition(
		ctx, transition.Record.ID, transition.Record.Version, transition.ID)
	if err == nil {
		if reflect.DeepEqual(current.Transition, transition) {
			return nil
		}
		return fleet.ErrActionConflict
	}
	if !errors.Is(err, fleet.ErrActionNotFound) {
		return err
	}
	lpart, err := explorercoord.Partition(
		coordination.DomainID("fleet-dispatch"), transition.Record.ID)
	if err != nil {
		return publicError("action", err)
	}
	_, err = s.runtime.Publish(ctx, explorercoord.Request{Intent: explorercoord.Intent{
		Operation: []byte("fleet.dispatch.repair-transition.v1"),
		Token:     transitionRepairToken(transition.ID),
		Cells: []explorercoord.Cell{{
			Table: DispatchTable,
			Row: transitionRow(
				transition.Record.ID, transition.Record.Version, transition.ID),
			Family: dispatchFamily, Qualifier: dispatchQualifier,
			Visibility: s.visibility, Value: value, EpochTimestamp: true,
			LPART: lpart, CopyGeneration: 1,
		}},
		Guards: []explorercoord.GuardIntent{{
			Entity: transitionEntity(transition.ID), Mode: guard.ModeAbsentOrIdentical,
			DesiredState:    guard.StateLive,
			DesiredWinnerID: transitionWinner(value), LPART: lpart,
			LogicalPolicyID: dispatchPolicy, RetirementGeneration: 1,
		}},
		Results: []explorercoord.ResultIdentity{{
			Kind: []byte("fleet-action-transition"),
			ID:   append([]byte(nil), transition.ID...),
		}},
	}})
	return publicError("action", err)
}

func (s *DispatchStore) readTransition(
	ctx context.Context, actionID []byte, version uint64, id []byte,
) (storedTransition, *guard.Head, error) {
	if err := dispatchID(actionID); err != nil {
		return storedTransition{}, nil, err
	}
	if err := dispatchID(id); err != nil {
		return storedTransition{}, nil, err
	}
	head, _, err := s.runtime.ReadEntity(ctx, transitionEntity(id))
	if err != nil {
		if errors.Is(err, guard.ErrNotFound) ||
			errors.Is(err, transaction.ErrNotFound) {
			return storedTransition{}, nil, fleet.ErrActionNotFound
		}
		return storedTransition{}, nil, publicError("action", err)
	}
	if head == nil {
		return storedTransition{}, nil, fleet.ErrActionNotFound
	}
	cell, ok, err := s.runtime.ReadCommittedCell(
		ctx, DispatchTable,
		transitionRow(actionID, version, id),
		dispatchFamily,
		dispatchQualifier, s.visibility, head.Epoch)
	if err != nil {
		return storedTransition{}, nil, publicError("action", err)
	}
	if !ok {
		// Deliberately still not-found, unlike the action and approval reads
		// above and in ApprovalStore, which answer Unavailable here (#633).
		//
		// This site differs because its keys differ: transitionEntity is keyed
		// on id alone while transitionRow composes (actionID, version, id). A
		// caller presenting a real transition id with a mismatched actionID or
		// version reaches here with a non-nil head, and that is genuine
		// absence, not a lag. Answering Unavailable would make such a request
		// retry forever, which is worse than the over-reported absence.
		//
		// Two conditions are mixed under this one !ok, and telling them apart
		// needs one extra read on this already-failed path: scan for the row
		// at the current committed frontier, then Unavailable if the row
		// exists and not-found if it does not. Not done because no client
		// reads transitions today — the effects gateway uses no transition
		// route — so a lagged not-found costs a human re-query rather than a
		// wrong decision. That is the fix if one ever depends on it.
		return storedTransition{}, nil, fleet.ErrActionNotFound
	}
	stored, err := decodeTransition(cell.Cell.Value)
	if err != nil {
		return storedTransition{}, nil, shoal.WrapError(
			shoal.ErrorInternal, "invalid committed fleet transition", err)
	}
	return stored, head, nil
}

func dispatchID(value []byte) error {
	if len(value) == 0 || len(value) > fleet.MaxActionIDBytes {
		return shoal.NewError(shoal.ErrorInvalidArgument, "fleet action identity is outside its byte bound")
	}
	return nil
}

func dispatchEntity(id []byte) guard.Entity {
	return guard.Entity{Kind: dispatchKind, ID: coordination.EntityID(append([]byte(nil), id...))}
}

func dispatchRow(id []byte) []byte {
	return append([]byte("action/"), id...)
}

func transitionPrefix(actionID []byte) []byte {
	result := append([]byte("transition/"), []byte(hex.EncodeToString(actionID))...)
	return append(result, '/')
}

func transitionRow(actionID []byte, version uint64, id []byte) []byte {
	row := transitionPrefix(actionID)
	var encodedVersion [8]byte
	binary.BigEndian.PutUint64(encodedVersion[:], version)
	row = append(row, encodedVersion[:]...)
	return append(row, []byte(hex.EncodeToString(id))...)
}

func transitionEntity(id []byte) guard.Entity {
	return guard.Entity{
		Kind: transitionKind,
		ID:   coordination.EntityID(hex.EncodeToString(id)),
	}
}

func transitionWinner(value []byte) []byte {
	digest := sha256.Sum256(value)
	return digest[:]
}

func transitionCompletionToken(id []byte) []byte {
	digest := sha256.New()
	_, _ = digest.Write([]byte("fleet.dispatch.transition-complete.v1"))
	_, _ = digest.Write(id)
	return digest.Sum(nil)
}

func transitionRepairToken(id []byte) []byte {
	digest := sha256.New()
	_, _ = digest.Write([]byte("fleet.dispatch.transition-repair.v1"))
	_, _ = digest.Write(id)
	return digest.Sum(nil)
}

func cloneTransition(value fleet.ActionTransition) fleet.ActionTransition {
	encoded, err := encodeTransition(storedTransition{Transition: value})
	if err != nil {
		return value
	}
	decoded, err := decodeTransition(encoded)
	if err != nil {
		return value
	}
	return decoded.Transition
}

func encodeAction(record fleet.ActionRecord) ([]byte, error) {
	if err := record.Validate(); err != nil {
		return nil, err
	}
	var buffer bytes.Buffer
	buffer.Write(dispatchMagic)
	if err := gob.NewEncoder(&buffer).Encode(record); err != nil {
		return nil, shoal.WrapError(shoal.ErrorInternal, "encode fleet action", err)
	}
	if buffer.Len() > 3*fleet.MaxActionPayloadBytes {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "fleet action record exceeds its bound")
	}
	return buffer.Bytes(), nil
}

func decodeAction(value []byte) (fleet.ActionRecord, error) {
	if len(value) < len(dispatchMagic) || !bytes.Equal(value[:len(dispatchMagic)], dispatchMagic) {
		return fleet.ActionRecord{}, errors.New("unknown fleet action encoding")
	}
	var record fleet.ActionRecord
	reader := bytes.NewReader(value[len(dispatchMagic):])
	if err := gob.NewDecoder(reader).Decode(&record); err != nil {
		return fleet.ActionRecord{}, err
	}
	if reader.Len() != 0 {
		return fleet.ActionRecord{}, errors.New("trailing fleet action bytes")
	}
	if err := record.Validate(); err != nil {
		return fleet.ActionRecord{}, err
	}
	return record, nil
}

func encodeTransition(value storedTransition) ([]byte, error) {
	if err := value.Transition.Validate(); err != nil {
		return nil, err
	}
	if !value.CompletedAt.IsZero() &&
		value.CompletedAt.Location() != time.UTC {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"fleet transition completion time must be UTC")
	}
	var buffer bytes.Buffer
	buffer.Write(transitionMagic)
	if err := gob.NewEncoder(&buffer).Encode(value); err != nil {
		return nil, shoal.WrapError(
			shoal.ErrorInternal, "encode fleet action transition", err)
	}
	if buffer.Len() > 3*fleet.MaxActionPayloadBytes {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"fleet action transition exceeds its bound")
	}
	return buffer.Bytes(), nil
}

func decodeTransition(value []byte) (storedTransition, error) {
	if len(value) < len(transitionMagic) ||
		!bytes.Equal(value[:len(transitionMagic)], transitionMagic) {
		return storedTransition{}, errors.New(
			"unknown fleet action transition encoding")
	}
	var stored storedTransition
	reader := bytes.NewReader(value[len(transitionMagic):])
	if err := gob.NewDecoder(reader).Decode(&stored); err != nil {
		return storedTransition{}, err
	}
	if reader.Len() != 0 {
		return storedTransition{}, errors.New(
			"trailing fleet action transition bytes")
	}
	if err := stored.Transition.Validate(); err != nil {
		return storedTransition{}, err
	}
	if !stored.CompletedAt.IsZero() &&
		stored.CompletedAt.Location() != time.UTC {
		return storedTransition{}, errors.New(
			"fleet action transition completion time is not UTC")
	}
	return stored, nil
}

// refuseRewrittenIdentity refuses a mutation that changes what an action *is*
// rather than what state it is in.
//
// The version and the fence give correct serialisation — one writer wins and a
// stale writer is refused — but they say nothing about what the winner is
// allowed to change. Before this, nothing below the service enforced any
// immutability invariant at all, so a service bug could rewrite a record's
// principal, its input, or its authorization provenance and the store would
// write it.
//
// That is not hypothetical. #443 shipped `record.Actor = decision.Actor()` in
// applyClaim. It was a no-op for as long as the claimant was by construction
// the enqueuer, and became a third party overwriting the record's own
// principal — permanently, since the completion path clones forward and never
// restores it. The enqueuer was then refused Status and Cancel on its own
// in-flight action. The only thing standing between the record's identity and
// an accidental rewrite was the absence of an assignment in one function,
// which is precisely the kind of guarantee that does not survive a refactor,
// and this one did not.
//
// It sits in the store rather than the service because the service is where
// the bug was. It covers every *mutating* path — Claim, ExecuteClaim,
// CompleteClaim, Cancel, ExtendClaim, ReportAmbiguity and the admission
// report, all of which build their next record with cloneActionRecord — and a
// mutating route added later gets it for free. It does not cover the three
// creating paths (enqueue, the admission grant and denial, and an approval's
// materialization), which pass ExpectedVersion 0 and necessarily have no
// stored record to compare against.
//
// Claim state is deliberately absent: the claimant fields and
// ClaimAttestationID (#446) move on every re-claim, and an extension under a
// newer attestation replaces the latter. They are #461's mutable list.
// ClaimExecutorRef (#391) is claim state of the same kind and sits in the
// deliberately-absent set listed at rewrittenIdentity, which says why.
//
// ErrorInternal rather than ErrorInvalidArgument, for two reasons. The only
// way to reach it is a service that rewrote its own record, so it is an
// implementation fault and not a caller's bad request — which is what
// shoal.ErrorInternal documents itself for. And it has to be distinguishable
// from ActionRecord.Validate, which runs first inside encodeAction and also
// returns ErrorInvalidArgument: sharing a code made two of this function's own
// tests vacuous, because the probe was refused by Validate for an unrelated
// reason and the assertion could not tell the difference.
func refuseRewrittenIdentity(current, next fleet.ActionRecord) error {
	// Short-circuiting rather than a table, so the common all-equal case does
	// not compare a maximal Input and Output on every mutating write.
	//
	// ID is compared first and is belt-and-braces: ApplyAction reads current
	// by canonical.ID, and the stored record's ID field always equals its row
	// key, so a differing ID is already refused upstream as not-found. It is
	// kept as a restatement of that binding rather than as a live check, and
	// it is the one entry here with no probe for that reason.
	switch {
	case !bytes.Equal(current.ID, next.ID):
		return rewrittenIdentity("ID")
	case !bytes.Equal(current.IdempotencyKey, next.IdempotencyKey):
		return rewrittenIdentity("idempotency key")

	// Who asked. The #443 rewrite was Actor.
	case current.Subject != next.Subject:
		return rewrittenIdentity("subject")
	case current.Actor != next.Actor:
		return rewrittenIdentity("actor")
	case current.ClientID != next.ClientID:
		return rewrittenIdentity("client ID")
	case !sameActionIDs(current.OnBehalfOf, next.OnBehalfOf):
		return rewrittenIdentity("delegation chain")

	// What was asked for, and within which scope.
	case current.ObjectID != next.ObjectID:
		return rewrittenIdentity("object ID")
	case !bytes.Equal(current.SourceID, next.SourceID):
		return rewrittenIdentity("source ID")
	case !bytes.Equal(current.PolicyID, next.PolicyID):
		return rewrittenIdentity("policy ID")
	case current.AgentID != next.AgentID:
		return rewrittenIdentity("agent ID")
	case current.AgentGeneration != next.AgentGeneration:
		return rewrittenIdentity("agent generation")
	case current.Capability != next.Capability:
		return rewrittenIdentity("capability")
	case current.Action != next.Action:
		return rewrittenIdentity("action")
	case !bytes.Equal(current.Input, next.Input):
		return rewrittenIdentity("input")
	case !bytes.Equal(current.ExecutorKey, next.ExecutorKey):
		return rewrittenIdentity("executor key")

	// Under what authority, and until when.
	case current.AuthorizationFingerprint != next.AuthorizationFingerprint:
		return rewrittenIdentity("authorization fingerprint")
	case current.PolicyGeneration != next.PolicyGeneration:
		return rewrittenIdentity("policy generation")
	case !current.AuthorizationExpiresAt.Equal(next.AuthorizationExpiresAt):
		return rewrittenIdentity("authorization expiry")

	// The admission grant, which is the authority an admission record carries
	// in place of a dispatcher's. AdmittedEffects is the sharpest of the four
	// and the closest structural analogue to #443: isAdmission keys on it
	// being non-empty, and Claim, Cancel, ExtendClaim and completeClaim each
	// refuse an admission on that basis — so a transition that cleared it
	// would silently convert a grant into ordinary dispatch work and unlock
	// all four routes. AdmittedObligation feeds obligationFromBitmap, so
	// rewriting it weakens a live withholding obligation.
	case !sameActionEffects(current.AdmittedEffects, next.AdmittedEffects):
		return rewrittenIdentity("admitted effects")
	case !bytes.Equal(current.AdmittedDisclosures, next.AdmittedDisclosures):
		return rewrittenIdentity("admitted disclosures")
	case !bytes.Equal(current.AdmittedObligation, next.AdmittedObligation):
		return rewrittenIdentity("admitted obligation")
	case current.AdmittedIdentityScheme != next.AdmittedIdentityScheme:
		return rewrittenIdentity("admitted identity scheme")

	// The approval an action was materialized under. Who approved it is as
	// much a part of what the action is as who asked for it, and #451's
	// separation-of-duty check is only as good as the record of it.
	case !bytes.Equal(current.ApprovalRequestDigest, next.ApprovalRequestDigest):
		return rewrittenIdentity("approval request digest")
	case current.ApprovalPolicyGeneration != next.ApprovalPolicyGeneration:
		return rewrittenIdentity("approval policy generation")
	case current.ApproverSubject != next.ApproverSubject:
		return rewrittenIdentity("approver subject")
	case current.ApproverActor != next.ApproverActor:
		return rewrittenIdentity("approver actor")
	case current.ApproverClientID != next.ApproverClientID:
		return rewrittenIdentity("approver client ID")
	case !current.ApprovedAt.Equal(next.ApprovedAt):
		return rewrittenIdentity("approval time")

	// When, and why. The enqueue request's own identifiers, not the
	// transition's: the per-transition equivalents exist as separate fields,
	// which is exactly why these must not move — a transition that overwrote
	// them would destroy the link back to the dispatch that created the
	// record and leave the fields meant to carry its own identity unused.
	case current.RequestID != next.RequestID:
		return rewrittenIdentity("request ID")
	case current.CorrelationID != next.CorrelationID:
		return rewrittenIdentity("correlation ID")
	case !current.CreatedAt.Equal(next.CreatedAt):
		return rewrittenIdentity("creation time")
	// A mutable deadline is the most dangerous of these: a transition that
	// extended it would let an action outlive the bound its dispatcher
	// accepted, and nothing below the service would notice.
	case !current.Deadline.Equal(next.Deadline):
		return rewrittenIdentity("deadline")
	case current.Reason != next.Reason:
		return rewrittenIdentity("reason")
	}

	// Monotonic rather than immutable, and asserted in the same place for the
	// same reason.
	//
	// The fence identifies which claim, and applyClaim increments it, so it
	// may rise and must never fall: a fence that went backwards would make a
	// stale completion look current. This is not subsumed by the ExpectedFence
	// compare-and-set above, which compares the *expected* value against the
	// stored one and never looks at the fence the incoming record carries — a
	// caller may pass a matching ExpectedFence and a lower Record.ClaimFence.
	//
	// EffectPossible says an effect may have happened; it may become true and
	// must never become false, because nothing can establish that an effect
	// did not occur after something has said it might have.
	if next.ClaimFence < current.ClaimFence {
		return shoal.NewError(
			shoal.ErrorInternal,
			"fleet action claim fence may not move backwards")
	}
	if current.EffectPossible && !next.EffectPossible {
		return shoal.NewError(
			shoal.ErrorInternal,
			"fleet action possible effect may not be withdrawn")
	}

	// Write-once, a third class alongside immutable and monotonic.
	//
	// Effected is how much of an irreversible egress happened before a
	// failure (#427). It is zero before the terminal transition, written
	// exactly once by it, and immutable after — so it is neither immutable
	// from creation (the terminal transition must be able to set it) nor
	// monotonic (a larger number is not more valid; a *different* number is a
	// rewrite of what was reported).
	//
	// Filing it as mutable would have been the easy answer and the wrong one.
	// The field exists so an operator reconciling a failed external call can
	// tell "nothing left" from "two megabytes left", and a value a later
	// transition may overwrite cannot carry that: the number an operator
	// reads would not be the number the worker reported. The replay
	// comparison in sameReportedOutcome also includes it, so a rewrite here
	// would let a caller's second report disagree with its first and still be
	// answered as the same report.
	if !current.Effected.Zero() && current.Effected != next.Effected {
		return shoal.NewError(
			shoal.ErrorInternal,
			"fleet action effected volume is written once")
	}
	return nil
}

// rewrittenIdentity names the field a mutation tried to change.
//
// Deliberately absent from the checks above, and listed here so the next
// reader does not add them: EvidenceSnapshotID, EvidenceSnapshotAsOf and
// Evidence are written by the completion path, which also *clears* them when
// the evidence fails validation; CancelKey is written by Cancel and by the
// admission denial; AuthorizedOperations is widened by Cancel and by the
// effect admission; TransitionOperation and the claimant fields move on every
// re-claim, which is what they are for; ClaimAttestationID and
// ClaimExecutorRef are claim-scoped in the same way — ClaimExecutorRef
// (#391) is the executor ref the *current* claim was taken under, so a
// re-claim after the descriptor is rebound must move it, and it is neither
// immutable (it changes with the claim) nor monotonic (a ref has no order),
// nor write-once (every claim writes it), so it fits none of the three
// classes above and is deliberately unchecked; TransitionRequestID and
// TransitionCorrelationID are per-transition by definition; the execution
// fingerprint, its generation and its expiry are re-derived at each execution
// boundary; the cancel authorization fields are written when a cancellation
// is authorized; Output, ErrorCode, State, Version and UpdatedAt are the
// outcome; and the claim history and the ambiguity reports grow.
func rewrittenIdentity(field string) error {
	return shoal.NewError(
		shoal.ErrorInternal, "fleet action "+field+" is immutable")
}

// sameActionEffects compares two declared effect sets as stored, which is
// order-sensitive on purpose: canonicalEffects sorts and deduplicates before
// anything reaches the store, so two sets that differ in order differ.
func sameActionEffects(left, right fleet.Effects) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func sameActionIDs(left, right []shoal.ID) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
