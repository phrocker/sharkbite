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
	"strconv"
	"time"

	"github.com/phrocker/shoal-oss/internal/explorercoord"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/guard"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/transaction"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// The approval outbox (#653): one row per approval transition, keyed by the
// approval ID and the version the transition wrote. ApplyApproval writes the
// row in the same publish as the approval record, so the two are one durable
// fact. Its prefix shares no bytes with "approval/" beyond the word: no
// approval scan can return one, and no action or transition scan can either.
//
// A row is only ever closed, never rewritten. Its transition is fixed at
// creation, and closing it sets an outcome once.
const approvalEventKind byte = 'V'

var (
	approvalEventRowPrefix = []byte("approval-event/")
	approvalEventMagic     = []byte{'S', 'F', 'V', 1}
)

// maxApprovalTransitions bounds one approval's rows. An approval has at most
// three transitions (requested; approved, refused or expired; materialized or
// expired), so a scan that returns more than this is a corrupt store.
const maxApprovalTransitions = 8

type storedApprovalEvent struct {
	Transition fleet.ApprovalTransition
	Outcome    fleet.ApprovalPublicationOutcome
	ClosedAt   time.Time
}

type approvalEventWrite struct {
	transition fleet.ApprovalTransition
	value      []byte
}

func newApprovalEventWrite(
	record fleet.ApprovalRecord, publisher fleet.ApprovalPublisher,
) (*approvalEventWrite, error) {
	transition, err := fleet.NewApprovalTransition(record, publisher)
	if err != nil {
		return nil, err
	}
	value, err := encodeApprovalEvent(storedApprovalEvent{Transition: transition})
	if err != nil {
		return nil, err
	}
	// Canonicalized through the codec, as every stored value is, so the
	// transition a reader sees is the one this write compares against.
	decoded, err := decodeApprovalEvent(value)
	if err != nil {
		return nil, shoal.WrapError(
			shoal.ErrorInternal, "canonicalize fleet approval event", err)
	}
	return &approvalEventWrite{transition: decoded.Transition, value: value}, nil
}

func (s *ApprovalStore) approvalEventIntent(
	event *approvalEventWrite, lpart coordination.LPART,
) (explorercoord.Cell, explorercoord.GuardIntent, explorercoord.ResultIdentity) {
	record := event.transition.Record
	return explorercoord.Cell{
			Table: DispatchTable, Row: approvalEventRow(record.ID, record.Version),
			Family: dispatchFamily, Qualifier: dispatchQualifier,
			Visibility: s.visibility, Value: append([]byte(nil), event.value...),
			EpochTimestamp: true, LPART: lpart, CopyGeneration: 1,
		}, explorercoord.GuardIntent{
			Entity: approvalEventEntity(record.ID, record.Version),
			Mode:   guard.ModeAbsentOrIdentical, DesiredState: guard.StateLive,
			DesiredWinnerID: transitionWinner(event.value), LPART: lpart,
			LogicalPolicyID: approvalPolicy, RetirementGeneration: 1,
		}, explorercoord.ResultIdentity{
			Kind: []byte("fleet-approval-event"),
			ID:   approvalEventResultID(record.ID, record.Version),
		}
}

// ensureApprovalEvent writes a transition's row when an identical retry finds
// its record already stored. A row that exists is the transition's own,
// whatever publisher it names: the record is identical, so it is the same
// transition, and a row is never rewritten. An absent row is the record of a
// build before #653, or of a write that carried no publisher. This retry is
// that transition's publisher, which NewApprovalTransition has checked.
func (s *ApprovalStore) ensureApprovalEvent(
	ctx context.Context, event *approvalEventWrite, lpart coordination.LPART,
) error {
	if event == nil {
		return nil
	}
	record := event.transition.Record
	if _, _, err := s.readApprovalEvent(
		ctx, record.ID, record.Version); err == nil {
		return nil
	} else if !errors.Is(err, fleet.ErrApprovalNotFound) {
		return err
	}
	cell, eventGuard, result := s.approvalEventIntent(event, lpart)
	_, err := s.runtime.Publish(ctx, explorercoord.Request{
		Intent: explorercoord.Intent{
			Operation: []byte("fleet.approval.repair-event.v1"),
			Token: approvalEventToken(
				"fleet.approval.event-repair.v1", record.ID, record.Version, nil),
			Cells:   []explorercoord.Cell{cell},
			Guards:  []explorercoord.GuardIntent{eventGuard},
			Results: []explorercoord.ResultIdentity{result},
		},
	})
	if err != nil {
		if _, _, readErr := s.readApprovalEvent(
			context.WithoutCancel(ctx), record.ID, record.Version); readErr == nil {
			return nil
		}
		return publicError("approval", err)
	}
	return nil
}

// ApprovalTransitions returns every outbox row of one approval, in version
// order.
func (s *ApprovalStore) ApprovalTransitions(
	ctx context.Context, id []byte,
) ([]fleet.StoredApprovalTransition, error) {
	if err := dispatchID(id); err != nil {
		return nil, err
	}
	head, err := s.runtime.CurrentHead(ctx)
	if err != nil {
		return nil, publicError("approval", err)
	}
	page, err := s.runtime.ScanCommitted(ctx, explorercoord.CommittedScanRequest{
		Table: DispatchTable, RowPrefix: approvalEventPrefix(id),
		Family: dispatchFamily, Qualifier: dispatchQualifier,
		Visibility: s.visibility, Frontier: head.Frontier,
		Limit:      maxApprovalTransitions + 1,
		MaxScanned: explorercoord.MaxCommittedScanCells,
	})
	if err != nil {
		return nil, publicError("approval", err)
	}
	if len(page.Cells) > maxApprovalTransitions || len(page.NextRow) > 0 &&
		bytes.HasPrefix(page.NextRow, approvalEventPrefix(id)) {
		return nil, shoal.NewError(
			shoal.ErrorInternal, "fleet approval has too many event rows")
	}
	result := make([]fleet.StoredApprovalTransition, 0, len(page.Cells))
	for _, cell := range page.Cells {
		stored, err := decodeApprovalEvent(cell.Cell.Value)
		if err != nil {
			return nil, shoal.WrapError(
				shoal.ErrorInternal, "invalid committed fleet approval event", err)
		}
		if !bytes.Equal(stored.Transition.Record.ID, id) ||
			!bytes.Equal(cell.Cell.Coordinate.Row, approvalEventRow(
				id, stored.Transition.Record.Version)) {
			return nil, shoal.NewError(
				shoal.ErrorInternal, "fleet approval event row is misplaced")
		}
		result = append(result, fleet.StoredApprovalTransition{
			Transition: fleet.CloneApprovalTransition(stored.Transition),
			Outcome:    stored.Outcome, ClosedAt: stored.ClosedAt,
		})
	}
	return result, nil
}

// CompleteApprovalTransition closes a pending row. Closing is set-once: a row
// already closed, with either outcome, answers success. Two drains racing to
// close one row with different outcomes, one published and one lapsed, both
// return and one outcome is recorded. Whichever it is, the row is closed and
// was never published twice.
func (s *ApprovalStore) CompleteApprovalTransition(
	ctx context.Context, transition fleet.ApprovalTransition,
	outcome fleet.ApprovalPublicationOutcome,
) error {
	if err := transition.Validate(); err != nil {
		return err
	}
	var closedAt time.Time
	switch outcome {
	case fleet.ApprovalEventPublished:
		closedAt = transition.Record.UpdatedAt
	case fleet.ApprovalEventLapsedOutcome:
		closedAt = transition.RetryUntil()
	default:
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "approval event outcome is invalid")
	}
	record := transition.Record
	current, head, err := s.readApprovalEvent(ctx, record.ID, record.Version)
	if err != nil {
		return err
	}
	if current.Outcome != "" {
		return nil
	}
	same, err := sameApprovalTransition(current.Transition, transition)
	if err != nil {
		return err
	}
	if !same {
		return fleet.ErrApprovalConflict
	}
	current.Outcome, current.ClosedAt = outcome, closedAt
	value, err := encodeApprovalEvent(current)
	if err != nil {
		return err
	}
	lpart, err := explorercoord.Partition(
		coordination.DomainID("fleet-approval"), record.ID)
	if err != nil {
		return publicError("approval", err)
	}
	_, err = s.runtime.Publish(ctx, explorercoord.Request{Intent: explorercoord.Intent{
		Operation: []byte("fleet.approval.close-event.v1"),
		Token: approvalEventToken(
			"fleet.approval.event-close.v1", record.ID, record.Version,
			[]byte(outcome)),
		Cells: []explorercoord.Cell{{
			Table: DispatchTable, Row: approvalEventRow(record.ID, record.Version),
			Family: dispatchFamily, Qualifier: dispatchQualifier,
			Visibility: s.visibility, Value: value, EpochTimestamp: true,
			LPART: lpart, CopyGeneration: 1,
		}},
		Guards: []explorercoord.GuardIntent{{
			Entity: approvalEventEntity(record.ID, record.Version),
			Mode:   guard.ModeMutate, ExpectedEpoch: head.Epoch,
			ExpectedDigest: head.LogicalDigest, DesiredState: guard.StateLive,
			DesiredWinnerID: transitionWinner(value), LPART: lpart,
			LogicalPolicyID: approvalPolicy, RetirementGeneration: 1,
		}},
		Results: []explorercoord.ResultIdentity{{
			Kind: []byte("fleet-approval-event-close"),
			ID:   approvalEventResultID(record.ID, record.Version),
		}},
	}})
	if err != nil {
		replayed, _, readErr := s.readApprovalEvent(
			context.WithoutCancel(ctx), record.ID, record.Version)
		if readErr == nil && replayed.Outcome != "" {
			return nil
		}
		return publicError("approval", err)
	}
	return nil
}

func (s *ApprovalStore) readApprovalEvent(
	ctx context.Context, id []byte, version uint64,
) (storedApprovalEvent, *guard.Head, error) {
	if err := dispatchID(id); err != nil {
		return storedApprovalEvent{}, nil, err
	}
	var head *guard.Head
	var cell explorercoord.CommittedCell
	for attempt := 0; ; attempt++ {
		var err error
		head, _, err = s.runtime.ReadEntity(ctx, approvalEventEntity(id, version))
		if err != nil {
			if errors.Is(err, guard.ErrNotFound) ||
				errors.Is(err, transaction.ErrNotFound) {
				return storedApprovalEvent{}, nil, fleet.ErrApprovalNotFound
			}
			return storedApprovalEvent{}, nil, publicError("approval", err)
		}
		if head == nil {
			return storedApprovalEvent{}, nil, fleet.ErrApprovalNotFound
		}
		var ok bool
		cell, ok, err = s.runtime.ReadCommittedCell(
			ctx, DispatchTable, approvalEventRow(id, version), dispatchFamily,
			dispatchQualifier, s.visibility, head.Epoch)
		if err != nil {
			return storedApprovalEvent{}, nil, publicError("approval", err)
		}
		if ok {
			break
		}
		// The entity and the row are both keyed on (id, version), so a head
		// without its cell is the committed-read lag, not absence (#633).
		if attempt == maxCommittedReadAttempts-1 {
			return storedApprovalEvent{}, nil,
				committedValueNotVisible("approval event")
		}
	}
	stored, err := decodeApprovalEvent(cell.Cell.Value)
	if err != nil {
		return storedApprovalEvent{}, nil, shoal.WrapError(
			shoal.ErrorInternal, "invalid committed fleet approval event", err)
	}
	return stored, head, nil
}

// sameApprovalTransition compares two rows by their canonical encoding. A
// row read from the store and one a caller holds after cloning can differ in
// nil versus empty slices and still be one transition.
func sameApprovalTransition(left, right fleet.ApprovalTransition) (bool, error) {
	leftValue, err := encodeApprovalEvent(storedApprovalEvent{Transition: left})
	if err != nil {
		return false, err
	}
	rightValue, err := encodeApprovalEvent(storedApprovalEvent{Transition: right})
	if err != nil {
		return false, err
	}
	return bytes.Equal(leftValue, rightValue), nil
}

func approvalEventPrefix(id []byte) []byte {
	prefix := append([]byte(nil), approvalEventRowPrefix...)
	prefix = append(prefix, hex.EncodeToString(id)...)
	return append(prefix, '/')
}

func approvalEventRow(id []byte, version uint64) []byte {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], version)
	return append(approvalEventPrefix(id), encoded[:]...)
}

func approvalEventEntity(id []byte, version uint64) guard.Entity {
	return guard.Entity{
		Kind: approvalEventKind,
		ID: coordination.EntityID(
			"approval-event/" + hex.EncodeToString(id) + "/" +
				strconv.FormatUint(version, 10)),
	}
}

func approvalEventResultID(id []byte, version uint64) []byte {
	return approvalEventToken("fleet.approval.event-result.v1", id, version, nil)
}

func approvalEventToken(tag string, id []byte, version uint64, extra []byte) []byte {
	digest := sha256.New()
	writeApprovalEventField(digest, []byte(tag))
	writeApprovalEventField(digest, id)
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], version)
	writeApprovalEventField(digest, encoded[:])
	writeApprovalEventField(digest, extra)
	return digest.Sum(nil)
}

func writeApprovalEventField(digest interface{ Write([]byte) (int, error) }, value []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = digest.Write(length[:])
	_, _ = digest.Write(value)
}

func encodeApprovalEvent(value storedApprovalEvent) ([]byte, error) {
	if err := value.Transition.Validate(); err != nil {
		return nil, err
	}
	if err := validateApprovalEventOutcome(value); err != nil {
		return nil, err
	}
	var buffer bytes.Buffer
	buffer.Write(approvalEventMagic)
	if err := gob.NewEncoder(&buffer).Encode(value); err != nil {
		return nil, shoal.WrapError(
			shoal.ErrorInternal, "encode fleet approval event", err)
	}
	if buffer.Len() > 4*fleet.MaxActionPayloadBytes {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "fleet approval event exceeds its bound")
	}
	return buffer.Bytes(), nil
}

func decodeApprovalEvent(value []byte) (storedApprovalEvent, error) {
	if len(value) < len(approvalEventMagic) ||
		!bytes.Equal(value[:len(approvalEventMagic)], approvalEventMagic) {
		return storedApprovalEvent{}, errors.New(
			"unknown fleet approval event encoding")
	}
	var stored storedApprovalEvent
	reader := bytes.NewReader(value[len(approvalEventMagic):])
	if err := gob.NewDecoder(reader).Decode(&stored); err != nil {
		return storedApprovalEvent{}, err
	}
	if reader.Len() != 0 {
		return storedApprovalEvent{}, errors.New(
			"trailing fleet approval event bytes")
	}
	if err := stored.Transition.Validate(); err != nil {
		return storedApprovalEvent{}, err
	}
	if err := validateApprovalEventOutcome(stored); err != nil {
		return storedApprovalEvent{}, err
	}
	return stored, nil
}

func validateApprovalEventOutcome(value storedApprovalEvent) error {
	switch value.Outcome {
	case "":
		if !value.ClosedAt.IsZero() {
			return shoal.NewError(
				shoal.ErrorInvalidArgument, "a pending approval event is closed")
		}
	case fleet.ApprovalEventPublished, fleet.ApprovalEventLapsedOutcome:
		if value.ClosedAt.IsZero() || value.ClosedAt.Location() != time.UTC {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"approval event close time must be UTC")
		}
	default:
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "approval event outcome is invalid")
	}
	return nil
}

var _ fleet.ApprovalTransitionStore = (*ApprovalStore)(nil)
