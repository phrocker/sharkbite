// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package explorerfleet

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"reflect"
	"strings"
	"time"

	"github.com/phrocker/shoal-oss/internal/explorercoord"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/guard"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/transaction"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Approval records live in the dispatch table under their own row prefix and
// guard kind, so no action scan, transition scan or action read can return
// one. That separation is what keeps a held request from ever being work: the
// dispatch surfaces read "action/" rows and nothing else.
const approvalKind byte = 'P'

var (
	approvalRowPrefix = []byte("approval/")
	approvalPolicy    = []byte("fleet/approval/v1")
	approvalMagic     = []byte{'S', 'F', 'P', 1}
)

// ApprovalStore is the durable approval key space over the shared embedded
// runtime.
type ApprovalStore struct {
	runtime    DispatchRuntime
	visibility []byte
}

func NewApprovalStore(
	runtime DispatchRuntime, visibility []byte,
) (*ApprovalStore, error) {
	if runtime == nil {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "fleet approval runtime is required")
	}
	if len(visibility) > coordination.MaxCoordinateBytes {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "fleet approval visibility exceeds its bound")
	}
	return &ApprovalStore{
		runtime: runtime, visibility: append([]byte(nil), visibility...),
	}, nil
}

func (s *ApprovalStore) GetApproval(
	ctx context.Context, id []byte,
) (fleet.ApprovalRecord, error) {
	record, _, err := s.readApproval(ctx, id)
	return record, err
}

func (s *ApprovalStore) readApproval(
	ctx context.Context, id []byte,
) (fleet.ApprovalRecord, *guard.Head, error) {
	if err := dispatchID(id); err != nil {
		return fleet.ApprovalRecord{}, nil, err
	}
	// Read together and re-read while the cell for the head's epoch is not
	// visible; see readAction for why the lag is closed here rather than
	// reported. approvalEntity and approvalRow are both keyed on id alone, so
	// a non-nil head proves this row exists.
	//
	// ErrApprovalNotFound carries extra weight on this route: five call sites
	// match it with errors.Is and several mean "absent, so create", so a
	// transient that changed identity would break them the way it broke
	// materializeRead.
	var head *guard.Head
	var cell explorercoord.CommittedCell
	for attempt := 0; ; attempt++ {
		var err error
		head, _, err = s.runtime.ReadEntity(ctx, approvalEntity(id))
		if err != nil {
			if errors.Is(err, guard.ErrNotFound) ||
				errors.Is(err, transaction.ErrNotFound) {
				return fleet.ApprovalRecord{}, nil, fleet.ErrApprovalNotFound
			}
			return fleet.ApprovalRecord{}, nil, publicError("approval", err)
		}
		if head == nil {
			return fleet.ApprovalRecord{}, nil, fleet.ErrApprovalNotFound
		}
		var ok bool
		cell, ok, err = s.runtime.ReadCommittedCell(
			ctx, DispatchTable, approvalRow(id), dispatchFamily,
			dispatchQualifier, s.visibility, head.Epoch)
		if err != nil {
			return fleet.ApprovalRecord{}, nil, publicError("approval", err)
		}
		if ok {
			break
		}
		if attempt == maxCommittedReadAttempts-1 {
			return fleet.ApprovalRecord{}, nil,
				committedValueNotVisible("approval")
		}
	}
	record, err := decodeApproval(cell.Cell.Value)
	if err != nil {
		return fleet.ApprovalRecord{}, nil, shoal.WrapError(
			shoal.ErrorInternal, "invalid committed fleet approval", err)
	}
	return record, head, nil
}

// ApplyApproval is one compare-and-set. An identical retry of a write already
// applied returns the stored record; anything else at a version other than the
// expected one is ErrApprovalConflict.
func (s *ApprovalStore) ApplyApproval(
	ctx context.Context, mutation fleet.ApprovalMutation,
) (fleet.ApprovalRecord, error) {
	if err := dispatchID(mutation.Record.ID); err != nil {
		return fleet.ApprovalRecord{}, err
	}
	if err := dispatchID(mutation.Token); err != nil {
		return fleet.ApprovalRecord{}, err
	}
	if mutation.Record.Version != mutation.ExpectedVersion+1 {
		return fleet.ApprovalRecord{}, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"fleet approval version transition is invalid")
	}
	value, err := encodeApproval(mutation.Record)
	if err != nil {
		return fleet.ApprovalRecord{}, err
	}
	canonical, err := decodeApproval(value)
	if err != nil {
		return fleet.ApprovalRecord{}, shoal.WrapError(
			shoal.ErrorInternal, "canonicalize fleet approval", err)
	}
	lpart, err := explorercoord.Partition(
		coordination.DomainID("fleet-approval"), canonical.ID)
	if err != nil {
		return fleet.ApprovalRecord{}, publicError("approval", err)
	}
	// The transition's event row (#653), written in the same publish as the
	// record, so a committed transition always has one.
	var event *approvalEventWrite
	if mutation.Publisher.Set() {
		event, err = newApprovalEventWrite(canonical, mutation.Publisher)
		if err != nil {
			return fleet.ApprovalRecord{}, err
		}
	}
	intentGuard := explorercoord.GuardIntent{
		Entity: approvalEntity(canonical.ID), DesiredState: guard.StateLive,
		// The winner is the content, not the identity. A first write is
		// guarded absent-or-identical, and with the identity as winner two
		// different first writes at one ID would both read as identical.
		DesiredWinnerID: transitionWinner(value), LPART: lpart,
		LogicalPolicyID: approvalPolicy, RetirementGeneration: 1,
	}
	current, head, readErr := s.readApproval(ctx, canonical.ID)
	switch {
	case readErr == nil && reflect.DeepEqual(current, canonical):
		if err := s.ensureApprovalEvent(ctx, event, lpart); err != nil {
			return fleet.ApprovalRecord{}, err
		}
		return current, nil
	case readErr == nil && current.Version != mutation.ExpectedVersion:
		return fleet.ApprovalRecord{}, fleet.ErrApprovalConflict
	case readErr == nil:
		if err := refuseRewrittenApproval(current, canonical); err != nil {
			return fleet.ApprovalRecord{}, err
		}
		// The lifecycle is a table (#655): nothing leaves a final state,
		// and nothing materializes except from approved. The service builds
		// every transition to fit; the store refuses one that does not, so
		// no writer can re-enable a withdrawn, expired, refused or stranded
		// request by writing the next version. After the immutability
		// check, so a rewrite is named for the field it rewrote.
		if !fleet.ApprovalTransitionAllowed(current.State, canonical.State) {
			return fleet.ApprovalRecord{}, shoal.NewError(
				shoal.ErrorInternal,
				"fleet approval transition from "+string(current.State)+
					" to "+string(canonical.State)+" is not allowed")
		}
		intentGuard.Mode = guard.ModeMutate
		intentGuard.ExpectedEpoch = head.Epoch
		intentGuard.ExpectedDigest = head.LogicalDigest
	case errors.Is(readErr, fleet.ErrApprovalNotFound):
		if mutation.ExpectedVersion != 0 {
			return fleet.ApprovalRecord{}, fleet.ErrApprovalConflict
		}
		intentGuard.Mode = guard.ModeAbsentOrIdentical
	default:
		return fleet.ApprovalRecord{}, readErr
	}
	intent := explorercoord.Intent{
		Operation: []byte("fleet.approval.apply.v1"),
		Token:     append([]byte(nil), mutation.Token...),
		Cells: []explorercoord.Cell{{
			Table: DispatchTable, Row: approvalRow(canonical.ID),
			Family: dispatchFamily, Qualifier: dispatchQualifier,
			Visibility: s.visibility, Value: value, EpochTimestamp: true,
			LPART: lpart, CopyGeneration: 1,
		}},
		Guards: []explorercoord.GuardIntent{intentGuard},
		Results: []explorercoord.ResultIdentity{{
			Kind: []byte("fleet-approval"),
			ID:   append([]byte(nil), canonical.ID...),
		}},
	}
	if event != nil {
		cell, eventGuard, result := s.approvalEventIntent(event, lpart)
		intent.Cells = append(intent.Cells, cell)
		intent.Guards = append(intent.Guards, eventGuard)
		intent.Results = append(intent.Results, result)
	}
	_, publishErr := s.runtime.Publish(ctx, explorercoord.Request{Intent: intent})
	if publishErr != nil {
		// The write may have landed. Whether it did is answered by what is
		// stored, never by the error: this record is ours if and only if it is
		// byte-for-byte what we wrote.
		resolve := context.WithoutCancel(ctx)
		for attempt := 0; attempt < 10; attempt++ {
			stored, getErr := s.GetApproval(resolve, canonical.ID)
			if getErr == nil {
				if reflect.DeepEqual(stored, canonical) {
					if err := s.ensureApprovalEvent(
						resolve, event, lpart); err != nil {
						return fleet.ApprovalRecord{}, err
					}
					return stored, nil
				}
				if stored.Version >= canonical.Version {
					return fleet.ApprovalRecord{}, fleet.ErrApprovalConflict
				}
			}
			time.Sleep(time.Millisecond)
		}
		if errors.Is(publishErr, explorercoord.ErrIndeterminatePublication) {
			return fleet.ApprovalRecord{}, shoal.WrapError(
				shoal.ErrorUnavailable,
				"fleet approval publication is indeterminate", publishErr)
		}
		return fleet.ApprovalRecord{}, publicError("approval", publishErr)
	}
	stored, err := s.GetApproval(ctx, canonical.ID)
	if err != nil {
		return fleet.ApprovalRecord{}, err
	}
	if !reflect.DeepEqual(stored, canonical) {
		return fleet.ApprovalRecord{}, fleet.ErrApprovalConflict
	}
	return stored, nil
}

// refuseRewrittenApproval is the approval record's counterpart of
// refuseRewrittenIdentity (#461), and exists for the same reason: the version
// compare-and-set stops two concurrent writers, not one writer that reads
// version N and writes N+1 with a field it should never have touched. The
// service builds every transition by cloning the stored record, so nothing
// rewrites these today; this makes that a property of the store rather than
// of the absence of an assignment.
//
// Creation-time state, immutable from the first write:
//
//   - ID, and Request in its entirety: the request is the exact record that
//     materializes, and it carries the requester's subject, actor, client and
//     delegation chain, which are who the approver is judged independent of.
//     refuseRewrittenIdentity names the field for the identity and scope
//     fields; anything else in the request is caught by the full comparison.
//   - RequestDigest, PolicyGeneration: what the approver reviews and names.
//   - RequestedAt, ExpiresAt: the window. Moving ExpiresAt would extend the
//     time an approval may become work.
//   - IdentityScheme (#526): the scheme the requester was named under. It is
//     what makes the requester comparable with an approver at all; a write
//     that changed it could make a pre-switch request decidable under the
//     new scheme.
//
// Set-once state, immutable once written: the decision (Verdict, the
// approver fields, DecidedAt, the decision request and correlation IDs, the
// mapping digest and provenance), because an approval is judged on who gave
// it; and MaterializedAt, because the action is built from it.
//
// State, Version, UpdatedAt and the first writes of the set-once fields are
// the transitions themselves and are not listed.
//
// ErrorInternal, as refuseRewrittenIdentity's is: only a service that rewrote
// its own record reaches it, and it must be distinguishable from Validate.
func refuseRewrittenApproval(current, next fleet.ApprovalRecord) error {
	switch {
	case !bytes.Equal(current.ID, next.ID):
		return rewrittenApproval("ID")
	case current.IdentityScheme != next.IdentityScheme:
		return rewrittenApproval("identity scheme")
	}
	// The request before its digest and generation: Validate binds both to
	// the request, so a rewritten request arrives with them rewritten too,
	// and the refusal should name what was rewritten.
	if err := refuseRewrittenIdentity(current.Request, next.Request); err != nil {
		// Restated as the approval's, naming the request field.
		message := err.Error()
		var invariant *shoal.Error
		if errors.As(err, &invariant) {
			message = strings.TrimPrefix(invariant.Message, "fleet action ")
		}
		return shoal.WrapError(
			shoal.ErrorInternal, "fleet approval request "+message, err)
	}
	if !reflect.DeepEqual(current.Request, next.Request) {
		return rewrittenApproval("request")
	}
	switch {
	case !bytes.Equal(current.RequestDigest, next.RequestDigest):
		return rewrittenApproval("request digest")
	case current.PolicyGeneration != next.PolicyGeneration:
		return rewrittenApproval("policy generation")
	case !current.RequestedAt.Equal(next.RequestedAt):
		return rewrittenApproval("request time")
	case !current.ExpiresAt.Equal(next.ExpiresAt):
		return rewrittenApproval("expiry")
	}
	if current.Verdict != "" {
		switch {
		case current.Verdict != next.Verdict:
			return rewrittenApproval("verdict")
		case current.ApproverSubject != next.ApproverSubject:
			return rewrittenApproval("approver subject")
		case current.ApproverActor != next.ApproverActor:
			return rewrittenApproval("approver actor")
		case current.ApproverClientID != next.ApproverClientID:
			return rewrittenApproval("approver client ID")
		case current.ApproverFingerprint != next.ApproverFingerprint:
			return rewrittenApproval("approver fingerprint")
		case current.ApproverPolicyGeneration != next.ApproverPolicyGeneration:
			return rewrittenApproval("approver policy generation")
		case !current.DecidedAt.Equal(next.DecidedAt):
			return rewrittenApproval("decision time")
		case current.DecisionRequestID != next.DecisionRequestID:
			return rewrittenApproval("decision request ID")
		case current.DecisionCorrelationID != next.DecisionCorrelationID:
			return rewrittenApproval("decision correlation ID")
		case current.ApproverMappingDigest != next.ApproverMappingDigest:
			return rewrittenApproval("approver mapping digest")
		case !current.ApproverProvenance.Equal(next.ApproverProvenance):
			return rewrittenApproval("approver provenance")
		}
	}
	if !current.MaterializedAt.IsZero() &&
		!current.MaterializedAt.Equal(next.MaterializedAt) {
		return rewrittenApproval("materialization time")
	}
	// Withdrawal and stranding (#655) are set once, by the transition that
	// makes the record final, and the table already refuses any write after
	// that one. Stated here as well, so the immutability of what a final
	// record says does not rest on the table alone.
	switch {
	case !current.WithdrawnAt.IsZero() &&
		!current.WithdrawnAt.Equal(next.WithdrawnAt):
		return rewrittenApproval("withdrawal time")
	case current.WithdrawalReason.Code != "" &&
		current.WithdrawalReason != next.WithdrawalReason:
		return rewrittenApproval("withdrawal reason")
	case current.StrandedCause != "" &&
		current.StrandedCause != next.StrandedCause:
		return rewrittenApproval("stranding cause")
	}
	return nil
}

func rewrittenApproval(field string) error {
	return shoal.NewError(
		shoal.ErrorInternal, "fleet approval "+field+" is immutable")
}

func (s *ApprovalStore) ScanApprovals(
	ctx context.Context, after []byte, limit int,
) (fleet.ApprovalPage, error) {
	return s.scanApprovals(ctx, after, limit, false)
}

// ScanApprovalsTolerant is ScanApprovals that skips, and counts, a stored
// record that does not decode instead of failing the page (#655): corrupt,
// or written by a newer build. The approval sweep uses it, so one such
// record cannot end every pass at its page. Pending keeps the strict scan.
func (s *ApprovalStore) ScanApprovalsTolerant(
	ctx context.Context, after []byte, limit int,
) (fleet.ApprovalPage, error) {
	return s.scanApprovals(ctx, after, limit, true)
}

func (s *ApprovalStore) scanApprovals(
	ctx context.Context, after []byte, limit int, tolerant bool,
) (fleet.ApprovalPage, error) {
	if len(after) > 0 {
		if err := dispatchID(after); err != nil {
			return fleet.ApprovalPage{}, err
		}
	}
	if limit <= 0 || limit > fleet.MaxApprovalListResults {
		return fleet.ApprovalPage{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "approval scan limit is outside its bound")
	}
	head, err := s.runtime.CurrentHead(ctx)
	if err != nil {
		return fleet.ApprovalPage{}, publicError("approval", err)
	}
	var startAfter []byte
	if len(after) > 0 {
		startAfter = approvalRow(after)
	}
	page, err := s.runtime.ScanCommitted(ctx, explorercoord.CommittedScanRequest{
		Table: DispatchTable, RowPrefix: approvalRowPrefix,
		StartAfterRow: startAfter, Family: dispatchFamily,
		Qualifier: dispatchQualifier, Visibility: s.visibility,
		Frontier: head.Frontier, Limit: limit,
		MaxScanned: explorercoord.MaxCommittedScanCells,
	})
	if err != nil {
		return fleet.ApprovalPage{}, publicError("approval", err)
	}
	result := fleet.ApprovalPage{
		Approvals: make([]fleet.ApprovalRecord, 0, len(page.Cells)),
	}
	for _, cell := range page.Cells {
		record, decodeErr := decodeApproval(cell.Cell.Value)
		if decodeErr != nil && tolerant {
			result.Undecodable++
			continue
		}
		if decodeErr != nil {
			return fleet.ApprovalPage{}, shoal.WrapError(
				shoal.ErrorInternal, "invalid committed fleet approval", decodeErr)
		}
		result.Approvals = append(result.Approvals, record)
	}
	if len(page.NextRow) > len(approvalRowPrefix) &&
		bytes.HasPrefix(page.NextRow, approvalRowPrefix) {
		result.Next = append(
			[]byte(nil), page.NextRow[len(approvalRowPrefix):]...)
	}
	return result, nil
}

func approvalEntity(id []byte) guard.Entity {
	return guard.Entity{
		Kind: approvalKind, ID: coordination.EntityID(append([]byte(nil), id...)),
	}
}

func approvalRow(id []byte) []byte {
	return append(append([]byte(nil), approvalRowPrefix...), id...)
}

func encodeApproval(record fleet.ApprovalRecord) ([]byte, error) {
	if err := record.Validate(); err != nil {
		return nil, err
	}
	var buffer bytes.Buffer
	buffer.Write(approvalMagic)
	if err := gob.NewEncoder(&buffer).Encode(record); err != nil {
		return nil, shoal.WrapError(
			shoal.ErrorInternal, "encode fleet approval", err)
	}
	if buffer.Len() > 3*fleet.MaxActionPayloadBytes {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "fleet approval record exceeds its bound")
	}
	return buffer.Bytes(), nil
}

func decodeApproval(value []byte) (fleet.ApprovalRecord, error) {
	if len(value) < len(approvalMagic) ||
		!bytes.Equal(value[:len(approvalMagic)], approvalMagic) {
		return fleet.ApprovalRecord{}, errors.New("unknown fleet approval encoding")
	}
	var record fleet.ApprovalRecord
	reader := bytes.NewReader(value[len(approvalMagic):])
	if err := gob.NewDecoder(reader).Decode(&record); err != nil {
		return fleet.ApprovalRecord{}, err
	}
	if reader.Len() != 0 {
		return fleet.ApprovalRecord{}, errors.New("trailing fleet approval bytes")
	}
	if err := record.Validate(); err != nil {
		return fleet.ApprovalRecord{}, err
	}
	return record, nil
}

var (
	_ fleet.ApprovalStore           = (*ApprovalStore)(nil)
	_ fleet.ApprovalTolerantScanner = (*ApprovalStore)(nil)
)
