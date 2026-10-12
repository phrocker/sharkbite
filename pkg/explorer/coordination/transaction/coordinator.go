/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements. See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership. The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License. You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package transaction

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/guard"
)

type Config struct {
	Domain            coordination.DomainID
	ControlVisibility []byte
	Store             Store
	Allocator         Allocator
	Guards            Guards
	Materializer      Materializer
	Writer            PhysicalWriter
	Verifier          PhysicalVerifier
	Pins              PinValidator
	Quarantine        Quarantine
	Clock             func() time.Time
	MaxRetries        int
	RetryBackoff      time.Duration
}

type Coordinator struct {
	domain       coordination.DomainID
	visibility   []byte
	store        Store
	allocator    Allocator
	guards       Guards
	materializer Materializer
	writer       PhysicalWriter
	verifier     PhysicalVerifier
	pins         PinValidator
	quarantine   Quarantine
	clock        func() time.Time
	maxRetries   int
	backoff      time.Duration
	lockMu       sync.Mutex
	txnLocks     map[string]*txnLock
}

type txnLock struct {
	mu   sync.Mutex
	refs int
}

func New(config Config) (*Coordinator, error) {
	if err := config.Domain.Validate(); err != nil {
		return nil, errors.Join(ErrInvalid, err)
	}
	if config.Store == nil || config.Allocator == nil || config.Guards == nil ||
		config.Materializer == nil || config.Writer == nil || config.Verifier == nil {
		return nil, fmt.Errorf("%w: all transaction dependencies are required", ErrInvalid)
	}
	if config.MaxRetries < 0 || config.MaxRetries > 100 ||
		config.RetryBackoff < 0 || config.RetryBackoff > time.Minute {
		return nil, fmt.Errorf("%w: retry configuration is outside its bound", ErrInvalid)
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	if config.MaxRetries == 0 {
		config.MaxRetries = 3
	}
	if config.RetryBackoff == 0 {
		config.RetryBackoff = 10 * time.Millisecond
	}
	return &Coordinator{
		domain: append(coordination.DomainID(nil), config.Domain...), visibility: append([]byte(nil), config.ControlVisibility...),
		store: config.Store, allocator: config.Allocator, guards: config.Guards,
		materializer: config.Materializer, writer: config.Writer, verifier: config.Verifier,
		pins: config.Pins, quarantine: config.Quarantine, clock: config.Clock,
		maxRetries: config.MaxRetries, backoff: config.RetryBackoff,
		txnLocks: make(map[string]*txnLock),
	}, nil
}

func (c *Coordinator) Publish(ctx context.Context, request Publication) (Result, error) {
	if err := c.validatePublication(request, true); err != nil {
		return Result{}, err
	}
	if err := c.validateAuthority(ctx, request.Authority); err != nil {
		return Result{}, err
	}
	unlock := c.lockTxn(request.TXN)
	defer unlock()
	stored, unchanged, err := c.claim(ctx, request)
	if err != nil {
		return Result{}, err
	}
	for attempt := 0; ; attempt++ {
		result, resumeErr := c.resume(ctx, request, stored, unchanged)
		if resumeErr == nil || !errors.Is(resumeErr, ErrConflict) || attempt >= c.maxRetries {
			return result, resumeErr
		}
		stored, err = c.readTxn(ctx, request.TXN)
		if err != nil {
			return Result{}, err
		}
		if stored.root.TokenHash != coordination.Sum(request.Token) ||
			stored.root.LogicalDigest != request.LogicalDigest {
			return Result{}, ErrConflict
		}
		if stored.root.State == coordination.StateConflicted {
			// A guard refusing a head another store owns is permanent. It
			// is returned as it is, because the bare conflict a caller
			// would otherwise get cannot be told from a race.
			if errors.Is(resumeErr, guard.ErrForeignHead) {
				return result, resumeErr
			}
			return Result{}, ErrConflict
		}
		unchanged = unchanged || stored.root.State == coordination.StateCommitted
		if err := c.wait(ctx); err != nil {
			return Result{}, err
		}
	}
}

// Recover takes ownership of an expired transaction and resumes from its
// authoritative state. Token bytes and status hints are not required.
func (c *Coordinator) Recover(
	ctx context.Context,
	txn coordination.TXN,
	owner coordination.OwnerID,
	leaseUntil time.Time,
	authority Authority,
) (Result, error) {
	request := Publication{TXN: txn, Owner: owner, LeaseUntil: leaseUntil, Authority: authority}
	if err := c.validatePublication(request, false); err != nil {
		return Result{}, err
	}
	if err := c.validateAuthority(ctx, authority); err != nil {
		return Result{}, err
	}
	unlock := c.lockTxn(txn)
	defer unlock()
	stored, err := c.readTxn(ctx, txn)
	if err != nil {
		return Result{}, err
	}
	request.LogicalDigest = stored.root.LogicalDigest
	if stored.root.State.Nonterminal() && (!bytes.Equal(stored.root.Owner, stored.lease.Owner) || stored.root.Fence != stored.lease.Fence) {
		return Result{}, fmt.Errorf("%w: transaction ownership is corrupt", ErrInternal)
	}
	if stored.root.State.Nonterminal() && !stored.lease.LeaseUntil.After(c.now()) &&
		(!bytes.Equal(stored.root.Owner, owner) || !stored.lease.LeaseUntil.Equal(leaseUntil)) {
		stored, err = c.takeover(ctx, txn, stored, owner, leaseUntil)
		if err != nil {
			return Result{}, err
		}
	}
	if stored.root.State.Nonterminal() && stored.lease.LeaseUntil.After(c.now()) &&
		!bytes.Equal(stored.root.Owner, owner) {
		return Result{}, ErrUnavailable
	}
	return c.resume(ctx, request, stored, false)
}

func (c *Coordinator) Status(
	ctx context.Context,
	domain coordination.DomainID,
	txn coordination.TXN,
) (guard.TxnDisposition, error) {
	if !bytes.Equal(domain, c.domain) {
		return 0, ErrNotFound
	}
	stored, err := c.readTxn(ctx, txn)
	if err != nil {
		return 0, err
	}
	switch stored.root.State {
	case coordination.StateCommitted:
		return guard.TxnCommitted, nil
	case coordination.StateAborted, coordination.StateConflicted, coordination.StatePoisoned:
		return guard.TxnTerminal, nil
	default:
		return guard.TxnNonterminal, nil
	}
}

func (c *Coordinator) Inspect(ctx context.Context, txn coordination.TXN) (Snapshot, error) {
	stored, err := c.readTxn(ctx, txn)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Root: stored.root, Lease: stored.lease}, nil
}

func (c *Coordinator) validatePublication(request Publication, requireToken bool) error {
	if err := request.TXN.Validate(); err != nil {
		return errors.Join(ErrInvalid, err)
	}
	if requireToken && (len(request.Token) == 0 || len(request.Token) > coordination.MaxOpaqueIDBytes) {
		return fmt.Errorf("%w: idempotency token is outside its bound", ErrInvalid)
	}
	if requireToken {
		if err := request.LogicalDigest.Validate("logical digest"); err != nil {
			return errors.Join(ErrInvalid, err)
		}
	}
	if err := request.Owner.Validate(); err != nil {
		return errors.Join(ErrInvalid, err)
	}
	if request.LeaseUntil.IsZero() || request.LeaseUntil.Location() != time.UTC || !request.LeaseUntil.After(c.now()) {
		return fmt.Errorf("%w: transaction lease must be a future UTC time", ErrInvalid)
	}
	if err := request.Authority.Generation.Validate(); err != nil {
		return errors.Join(ErrInvalid, err)
	}
	if err := request.Authority.Fence.Validate(); err != nil {
		return errors.Join(ErrInvalid, err)
	}
	if err := request.Authority.Holder.Validate(); err != nil {
		return errors.Join(ErrInvalid, err)
	}
	if err := request.Authority.Mode.Validate(); err != nil {
		return errors.Join(ErrInvalid, err)
	}
	if err := request.Authority.RetentionGeneration.Validate(); err != nil {
		return errors.Join(ErrInvalid, err)
	}
	return nil
}

func (c *Coordinator) claim(ctx context.Context, request Publication) (storedTxn, bool, error) {
	rootCoord, leaseCoord, _ := c.txnCoordinates(request.TXN)
	root := coordination.TxnRootV3{
		State: coordination.StateClaimed, LogicalDigest: request.LogicalDigest,
		TokenHash: coordination.Sum(request.Token), Owner: append(coordination.OwnerID(nil), request.Owner...),
		Fence: 1, StateGeneration: 1,
		WriterAuthorityGeneration: request.Authority.Generation,
		RetentionGeneration:       request.Authority.RetentionGeneration,
	}
	lease := coordination.TxnLeaseV1{
		Generation: 1, Owner: append(coordination.OwnerID(nil), request.Owner...),
		Fence: root.Fence, LeaseUntil: request.LeaseUntil,
	}
	rootBytes, rootErr := coordination.MarshalTxnRootV3(root)
	leaseBytes, leaseErr := coordination.MarshalTxnLeaseV1(lease)
	if rootErr != nil || leaseErr != nil {
		return storedTxn{}, false, errors.Join(ErrInvalid, rootErr, leaseErr)
	}
	mutation := allocator.Mutation{
		Row: rootCoord.Row,
		Conditions: []allocator.Condition{
			{Coordinate: rootCoord, Absent: true}, {Coordinate: leaseCoord, Absent: true},
		},
		Updates: []allocator.Update{
			{Coordinate: rootCoord, Value: rootBytes, Timestamp: 1},
			{Coordinate: leaseCoord, Value: leaseBytes, Timestamp: 1},
		},
	}
	for attempt := 0; ; attempt++ {
		status, writeErr := c.store.CompareAndMutate(ctx, mutation)
		if status == allocator.StatusAccepted {
			return storedTxn{root: root, lease: lease}, false, nil
		}
		if status != allocator.StatusRejected && !errors.Is(writeErr, allocator.ErrConditionalUnknown) &&
			!resumeAfterDefiniteFailure(writeErr) {
			return storedTxn{}, false, errors.Join(ErrUnavailable, writeErr)
		}
		existing, readErr := c.readTxn(ctx, request.TXN)
		if readErr == nil {
			if existing.root.TokenHash != root.TokenHash || existing.root.LogicalDigest != root.LogicalDigest {
				return storedTxn{}, false, ErrConflict
			}
			if existing.root.State == coordination.StateCommitted {
				return existing, true, nil
			}
			if existing.root.State.Terminal() {
				return storedTxn{}, false, stateError(existing.root.State)
			}
			if bytes.Equal(existing.root.Owner, request.Owner) && existing.root.Fence == existing.lease.Fence {
				return existing, false, nil
			}
			if existing.lease.LeaseUntil.After(c.now()) {
				return storedTxn{}, false, ErrUnavailable
			}
			existing, readErr = c.takeover(ctx, request.TXN, existing, request.Owner, request.LeaseUntil)
			return existing, false, readErr
		}
		if !errors.Is(readErr, ErrNotFound) {
			return storedTxn{}, false, readErr
		}
		if attempt >= c.maxRetries {
			return storedTxn{}, false, errors.Join(ErrUnavailable, allocator.ErrConditionalUnknown)
		}
		if err := c.wait(ctx); err != nil {
			return storedTxn{}, false, err
		}
	}
}

func (c *Coordinator) takeover(
	ctx context.Context,
	txn coordination.TXN,
	before storedTxn,
	owner coordination.OwnerID,
	leaseUntil time.Time,
) (storedTxn, error) {
	if before.root.State.Terminal() || before.lease.LeaseUntil.After(c.now()) ||
		before.root.Fence == coordination.Fence(math.MaxInt64) {
		return storedTxn{}, ErrConflict
	}
	after := before
	after.root.Owner = append(coordination.OwnerID(nil), owner...)
	after.root.Fence++
	after.root.StateGeneration++
	after.lease.Generation++
	after.lease.Owner = append(coordination.OwnerID(nil), owner...)
	after.lease.Fence = after.root.Fence
	after.lease.LeaseUntil = leaseUntil
	return c.mutateRootAndLease(ctx, txn, before, after)
}

func (c *Coordinator) mutateRootAndLease(
	ctx context.Context,
	txn coordination.TXN,
	before, after storedTxn,
) (storedTxn, error) {
	rootCoord, leaseCoord, _ := c.txnCoordinates(txn)
	beforeRoot, _ := coordination.MarshalTxnRootV3(before.root)
	beforeLease, _ := coordination.MarshalTxnLeaseV1(before.lease)
	afterRoot, rootErr := coordination.MarshalTxnRootV3(after.root)
	afterLease, leaseErr := coordination.MarshalTxnLeaseV1(after.lease)
	if rootErr != nil || leaseErr != nil {
		return storedTxn{}, errors.Join(ErrInternal, rootErr, leaseErr)
	}
	mutation := allocator.Mutation{
		Row: rootCoord.Row,
		Conditions: []allocator.Condition{
			exactCondition(rootCoord, beforeRoot, int64(before.root.StateGeneration)),
			exactCondition(leaseCoord, beforeLease, int64(before.lease.Generation)),
		},
		Updates: []allocator.Update{
			{Coordinate: rootCoord, Value: afterRoot, Timestamp: int64(after.root.StateGeneration)},
			{Coordinate: leaseCoord, Value: afterLease, Timestamp: int64(after.lease.Generation)},
		},
	}
	return c.applyTxnMutation(ctx, txn, mutation, before, after)
}

func (c *Coordinator) resume(ctx context.Context, request Publication, stored storedTxn, unchanged bool) (Result, error) {
	// TxnRoot does not store TXN, so all mutations use the request identity.
	txn := request.TXN
	plan, err := c.materializer.Materialize(ctx, MaterializeRequest{TXN: txn, LogicalDigest: stored.root.LogicalDigest})
	if err != nil {
		return Result{}, classify(err)
	}
	plan = clonePlan(plan)
	summary, err := plan.Validate()
	if err != nil {
		return Result{}, err
	}
	if stored.root.State == coordination.StateClaimed {
		if err := c.writeChunks(ctx, txn, plan.Chunks); err != nil {
			return Result{}, err
		}
		if err := c.verifyWrittenChunks(ctx, txn, plan.Chunks); err != nil {
			return Result{}, c.poison(ctx, txn, stored, err)
		}
		next := stored.root
		next.State = coordination.StatePlanned
		next.StateGeneration++
		next.ManifestRoot, next.ChunkCount = summary.RootDigest, summary.ChunkCount
		next.TotalEntries, next.TotalEncodedBytes = summary.TotalEntries, summary.TotalEncodedBytes
		next.LPARTs, next.ResultIdentities = sortedLPARTs(plan.Copies), copyResults(plan.Results)
		stored, err = c.transition(ctx, txn, stored, next)
		if err != nil {
			return Result{}, err
		}
	} else if err := c.verifyStoredPlan(ctx, txn, stored.root, plan); err != nil {
		return Result{}, c.poison(ctx, txn, stored, err)
	}
	if stored.root.State.Terminal() && stored.root.State != coordination.StateCommitted {
		return Result{}, stateError(stored.root.State)
	}
	if stored.root.State == coordination.StateCommitted {
		if err := c.verifyPublishedCopies(ctx, txn, stored.root, plan.Copies); err != nil {
			return Result{}, c.poison(ctx, txn, stored, err)
		}
		if err := c.verifier.Verify(ctx, stored.root.Epoch, plan.Cells); err != nil {
			return Result{}, c.poison(ctx, txn, stored, err)
		}
	}
	if err := c.validatePins(ctx, stored.root, plan); err != nil {
		return Result{}, err
	}
	acquired, err := c.ensureGuards(ctx, request, stored, plan)
	if err != nil {
		return Result{}, c.failBeforeCommit(ctx, txn, stored, nil, acquired, err)
	}
	if stored.root.State == coordination.StatePlanned {
		next := stored.root
		next.State, next.StateGeneration = coordination.StateGuardsAcquired, next.StateGeneration+1
		stored, err = c.transition(ctx, txn, stored, next)
		if err != nil {
			return Result{}, err
		}
	}
	var reservation coordination.ReservationV1
	if stored.root.Epoch != 0 {
		reservation, err = c.allocator.Reservation(ctx, stored.root.Epoch)
		if err != nil && !stored.root.State.Terminal() {
			return Result{}, classify(err)
		}
		if err == nil && reservation.Fence != stored.root.Fence && !reservation.State.Terminal() {
			reservation, err = c.allocator.TakeoverReservation(
				ctx, reservation, stored.root.Owner, stored.lease.LeaseUntil, stored.root.Fence, c.now(),
			)
			if err != nil {
				return Result{}, classify(err)
			}
		}
	}
	if stored.root.State == coordination.StateGuardsAcquired {
		if err := c.validatePins(ctx, stored.root, plan); err != nil {
			return Result{}, err
		}
		head, headErr := c.allocator.CurrentHead(ctx)
		if headErr != nil {
			return Result{}, classify(headErr)
		}
		reservation, err = c.allocator.Reserve(ctx, allocator.ReserveRequest{
			Predecessor: head, TXN: txn, Owner: stored.root.Owner, LeaseUntil: stored.lease.LeaseUntil,
			Authority: allocator.Authority{
				Generation: request.Authority.Generation, Mode: request.Authority.Mode,
				Holder: request.Authority.Holder, Fence: request.Authority.Fence,
			},
			OwnerFence: stored.root.Fence, RetentionGeneration: request.Authority.RetentionGeneration,
		})
		if err != nil {
			return Result{}, c.failBeforeCommit(ctx, txn, stored, nil, acquired, classify(err))
		}
		next := stored.root
		next.State, next.StateGeneration, next.Epoch =
			coordination.StateEpochReserved, next.StateGeneration+1, reservation.Epoch
		stored, err = c.transition(ctx, txn, stored, next)
		if err != nil {
			return Result{}, err
		}
	}
	if stored.root.State == coordination.StateEpochReserved {
		if err := c.validatePins(ctx, stored.root, plan); err != nil {
			return Result{}, err
		}
		if err := c.writer.Write(ctx, stored.root.Epoch, plan.Cells); err != nil {
			classified := classify(err)
			if errors.Is(classified, ErrInternal) {
				return Result{}, c.poison(ctx, txn, stored, classified)
			}
			return Result{}, classified
		}
		next := stored.root
		next.State, next.StateGeneration = coordination.StateWriting, next.StateGeneration+1
		stored, err = c.transition(ctx, txn, stored, next)
		if err != nil {
			return Result{}, err
		}
	}
	if stored.root.State == coordination.StateWriting {
		if err := c.verifier.Verify(ctx, stored.root.Epoch, plan.Cells); err != nil {
			classified := classify(err)
			if !errors.Is(classified, ErrInternal) {
				return Result{}, classified
			}
			return Result{}, c.poison(ctx, txn, stored, classified)
		}
		next := stored.root
		next.State, next.StateGeneration = coordination.StateVerified, next.StateGeneration+1
		stored, err = c.transition(ctx, txn, stored, next)
		if err != nil {
			return Result{}, err
		}
	}
	if stored.root.State == coordination.StateVerified {
		if err := c.validatePins(ctx, stored.root, plan); err != nil {
			return Result{}, err
		}
		for i := range acquired {
			published := publishedFrom(acquired[i].Pending, stored.root)
			acquired[i].Pending, err = c.guards.Prepare(ctx, acquired[i].Pending, published, c.now())
			if err != nil {
				return Result{}, classify(err)
			}
		}
		next := stored.root
		next.State, next.StateGeneration = coordination.StatePrepared, next.StateGeneration+1
		stored, err = c.transition(ctx, txn, stored, next)
		if err != nil {
			return Result{}, err
		}
	}
	if stored.root.State == coordination.StatePrepared {
		if err := c.validatePins(ctx, stored.root, plan); err != nil {
			return Result{}, err
		}
		stored, err = c.publishCopies(ctx, txn, stored, plan.Copies)
		if err != nil {
			return Result{}, err
		}
	}
	if stored.root.State != coordination.StateCommitted {
		return Result{}, fmt.Errorf("%w: unsupported transaction state", ErrInternal)
	}
	if reservation.Epoch == 0 {
		reservation, err = c.allocator.Reservation(ctx, stored.root.Epoch)
		if err != nil {
			if errors.Is(err, allocator.ErrNotFound) {
				return Result{}, c.poison(ctx, txn, stored, errors.New("committed transaction reservation is missing"))
			}
			return Result{}, classify(err)
		}
	}
	if !reservation.State.Terminal() {
		_, _, err = c.allocator.Terminalize(ctx, reservation, coordination.StateCommitted)
		if err != nil {
			classified := classify(err)
			if errors.Is(classified, ErrInternal) {
				return Result{}, c.poison(ctx, txn, stored, classified)
			}
			return Result{}, classified
		}
	} else if reservation.State != coordination.StateCommitted {
		return Result{}, c.poison(ctx, txn, stored, errors.New("committed root has non-committed reservation"))
	}
	if err := c.waitForCheckpoint(ctx, stored.root.Epoch); err != nil {
		return Result{}, err
	}
	for i := range acquired {
		_, err = c.guards.Commit(ctx, acquired[i].Pending, publishedFrom(acquired[i].Pending, stored.root), c.now())
		if err != nil {
			return Result{}, classify(err)
		}
	}
	return Result{Epoch: stored.root.Epoch, Identities: copyResults(stored.root.ResultIdentities), Unchanged: unchanged}, nil
}

func (c *Coordinator) verifyPublishedCopies(
	ctx context.Context,
	txn coordination.TXN,
	root coordination.TxnRootV3,
	copies []CommitCopy,
) error {
	rootCoord, _, _ := c.txnCoordinates(txn)
	coordinates := make([]allocator.Coordinate, len(copies))
	expected := make([][]byte, len(copies))
	for i, item := range copies {
		value := coordination.PartitionCommitCopyV1{
			State: coordination.StateCommitted, TXN: txn, Epoch: root.Epoch,
			LPART: item.LPART, CopyGeneration: item.CopyGeneration,
			VisibilityDigest: item.VisibilityDigest, LogicalDigest: item.LogicalDigest,
			PhysicalCopyDigest:    item.PhysicalCopyDigest,
			RequiredIndexFamilies: item.RequiredIndexFamilies,
		}
		encoded, err := coordination.MarshalPartitionCommitCopyV1(value)
		if err != nil {
			return err
		}
		qualifier := append([]byte(nil), coordination.E(item.LPART)...)
		qualifier = append(qualifier, coordination.U64(uint64(item.CopyGeneration))...)
		qualifier = append(qualifier, item.VisibilityDigest[:]...)
		coordinates[i] = coordinate(rootCoord.Row, familyPublish, qualifier, item.Visibility)
		expected[i] = encoded
	}
	cells, err := c.store.ReadExact(ctx, coordinates)
	if err != nil {
		return errors.Join(ErrUnavailable, err)
	}
	if len(cells) != len(coordinates) {
		return fmt.Errorf("%w: committed partition copy is missing", ErrInternal)
	}
	for i, wanted := range coordinates {
		cell, found := findCell(cells, wanted)
		if !found || cell.Timestamp != int64(root.Epoch) || !bytes.Equal(cell.Value, expected[i]) {
			return fmt.Errorf("%w: committed partition copy is contradictory", ErrInternal)
		}
	}
	return nil
}

func (c *Coordinator) transition(
	ctx context.Context,
	txn coordination.TXN,
	stored storedTxn,
	next coordination.TxnRootV3,
) (storedTxn, error) {
	if err := coordination.ValidateTransition(stored.root.State, next.State); err != nil {
		return storedTxn{}, errors.Join(ErrInternal, err)
	}
	if next.StateGeneration != stored.root.StateGeneration+1 {
		return storedTxn{}, fmt.Errorf("%w: state generation did not advance exactly once", ErrInternal)
	}
	before, after := stored, stored
	after.root = next
	return c.mutateRoot(ctx, txn, before, after)
}

func (c *Coordinator) mutateRoot(ctx context.Context, txn coordination.TXN, before, after storedTxn) (storedTxn, error) {
	rootCoord, _, _ := c.txnCoordinates(txn)
	beforeBytes, _ := coordination.MarshalTxnRootV3(before.root)
	afterBytes, err := coordination.MarshalTxnRootV3(after.root)
	if err != nil {
		return storedTxn{}, errors.Join(ErrInternal, err)
	}
	mutation := allocator.Mutation{
		Row:        rootCoord.Row,
		Conditions: []allocator.Condition{exactCondition(rootCoord, beforeBytes, int64(before.root.StateGeneration))},
		Updates:    []allocator.Update{{Coordinate: rootCoord, Value: afterBytes, Timestamp: int64(after.root.StateGeneration)}},
	}
	return c.applyTxnMutation(ctx, txn, mutation, before, after)
}

func (c *Coordinator) applyTxnMutation(
	ctx context.Context,
	txn coordination.TXN,
	mutation allocator.Mutation,
	before, after storedTxn,
) (storedTxn, error) {
	for attempt := 0; ; attempt++ {
		status, writeErr := c.store.CompareAndMutate(ctx, mutation)
		if status == allocator.StatusAccepted {
			return after, nil
		}
		if status != allocator.StatusRejected && !errors.Is(writeErr, allocator.ErrConditionalUnknown) &&
			!resumeAfterDefiniteFailure(writeErr) {
			return storedTxn{}, errors.Join(ErrUnavailable, writeErr)
		}
		got, readErr := c.readTxn(ctx, txn)
		if readErr == nil {
			if rootsEqual(got.root, after.root) && leasesEqual(got.lease, after.lease) {
				return got, nil
			}
			if !rootsEqual(got.root, before.root) || !leasesEqual(got.lease, before.lease) {
				return storedTxn{}, ErrConflict
			}
		} else if !errors.Is(readErr, ErrNotFound) {
			return storedTxn{}, readErr
		}
		if attempt >= c.maxRetries {
			return storedTxn{}, errors.Join(ErrUnavailable, allocator.ErrConditionalUnknown)
		}
		if err := c.wait(ctx); err != nil {
			return storedTxn{}, err
		}
	}
}

func (c *Coordinator) writeChunks(ctx context.Context, txn coordination.TXN, chunks []coordination.ManifestChunkV2) error {
	for _, chunk := range chunks {
		row, _ := coordination.ManifestRow(c.domain, txn, chunk.Index)
		cell := coordinate(row, familyManifest, qualifierChunk, c.visibility)
		value, err := coordination.MarshalManifestChunkV2(chunk)
		if err != nil {
			return errors.Join(ErrInvalid, err)
		}
		mutation := allocator.Mutation{
			Row: row, Conditions: []allocator.Condition{{Coordinate: cell, Absent: true}},
			Updates: []allocator.Update{{Coordinate: cell, Value: value, Timestamp: int64(chunk.Index) + 1}},
		}
		for attempt := 0; ; attempt++ {
			status, writeErr := c.store.CompareAndMutate(ctx, mutation)
			if status == allocator.StatusAccepted {
				break
			}
			if status != allocator.StatusRejected && !errors.Is(writeErr, allocator.ErrConditionalUnknown) &&
				!resumeAfterDefiniteFailure(writeErr) {
				return errors.Join(ErrUnavailable, writeErr)
			}
			cells, readErr := c.store.ReadExact(ctx, []allocator.Coordinate{cell})
			if readErr != nil {
				return errors.Join(ErrUnavailable, readErr)
			}
			if len(cells) == 1 {
				if cells[0].Timestamp != int64(chunk.Index)+1 || !bytes.Equal(cells[0].Value, value) {
					return fmt.Errorf("%w: contradictory immutable manifest chunk", ErrInternal)
				}
				break
			}
			if attempt >= c.maxRetries {
				return errors.Join(ErrUnavailable, allocator.ErrConditionalUnknown)
			}
			if err := c.wait(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *Coordinator) readChunks(
	ctx context.Context,
	txn coordination.TXN,
	count uint32,
) ([]coordination.ManifestChunkV2, error) {
	chunks := make([]coordination.ManifestChunkV2, count)
	for index := uint32(0); index < count; index++ {
		row, _ := coordination.ManifestRow(c.domain, txn, index)
		cell := coordinate(row, familyManifest, qualifierChunk, c.visibility)
		cells, err := c.store.ReadExact(ctx, []allocator.Coordinate{cell})
		if err != nil {
			return nil, errors.Join(ErrUnavailable, err)
		}
		if len(cells) != 1 {
			return nil, fmt.Errorf("%w: manifest chunk is missing", ErrInternal)
		}
		chunks[index], err = coordination.UnmarshalManifestChunkV2(cells[0].Value)
		if err != nil || cells[0].Timestamp != int64(index)+1 {
			return nil, fmt.Errorf("%w: manifest chunk is corrupt", ErrInternal)
		}
	}
	return chunks, nil
}

func (c *Coordinator) verifyWrittenChunks(
	ctx context.Context,
	txn coordination.TXN,
	expected []coordination.ManifestChunkV2,
) error {
	got, err := c.readChunks(ctx, txn, uint32(len(expected)))
	if err != nil {
		return err
	}
	for i := range expected {
		left, _ := coordination.MarshalManifestChunkV2(expected[i])
		right, _ := coordination.MarshalManifestChunkV2(got[i])
		if !bytes.Equal(left, right) {
			return fmt.Errorf("%w: immutable manifest readback differs", ErrInternal)
		}
	}
	return nil
}

func (c *Coordinator) verifyStoredPlan(
	ctx context.Context,
	txn coordination.TXN,
	root coordination.TxnRootV3,
	plan Plan,
) error {
	chunks, err := c.readChunks(ctx, txn, root.ChunkCount)
	if err != nil {
		return err
	}
	if err := coordination.VerifyManifestRoot(root, chunks); err != nil {
		return errors.Join(ErrInternal, err)
	}
	if len(chunks) != len(plan.Chunks) {
		return fmt.Errorf("%w: reconstructed manifest differs", ErrInternal)
	}
	for i := range chunks {
		left, _ := coordination.MarshalManifestChunkV2(chunks[i])
		right, _ := coordination.MarshalManifestChunkV2(plan.Chunks[i])
		if !bytes.Equal(left, right) {
			return fmt.Errorf("%w: reconstructed manifest differs", ErrInternal)
		}
	}
	return nil
}

func (c *Coordinator) ensureGuards(
	ctx context.Context,
	request Publication,
	stored storedTxn,
	plan Plan,
) ([]guard.Acquisition, error) {
	if stored.root.State < coordination.StatePlanned ||
		(stored.root.State.Terminal() && stored.root.State != coordination.StateCommitted) {
		return nil, nil
	}
	intents := make([]guard.Intent, len(plan.Guards))
	for i, item := range plan.Guards {
		intents[i] = guard.Intent{
			Entity: item.Entity, TXN: request.TXN, Owner: stored.root.Owner,
			LeaseUntil: stored.lease.LeaseUntil, Fence: stored.root.Fence,
			AuthorityGeneration: request.Authority.Generation, AuthorityFence: request.Authority.Fence,
			RetentionGeneration:  request.Authority.RetentionGeneration,
			RetirementGeneration: item.RetirementGeneration, HistoryFloor: request.Authority.HistoryFloor,
			Mode: item.Mode, ExpectedEpoch: item.ExpectedEpoch, ExpectedDigest: item.ExpectedDigest,
			DesiredState: item.DesiredState, DesiredWinnerID: item.DesiredWinnerID,
			DesiredDigest: item.DesiredDigest, LPART: item.LPART, LogicalPolicyID: item.LogicalPolicyID,
			ManifestChunk: item.ManifestChunk, ManifestEntry: item.ManifestEntry,
			Ordinal: item.Ordinal, PhysicalDigest: item.PhysicalDigest,
		}
	}
	if stored.root.State == coordination.StatePlanned {
		values, err := c.guards.AcquireMany(ctx, intents)
		return values, classify(err)
	}
	acquired := make([]guard.Acquisition, 0, len(intents))
	for _, intent := range intents {
		head, pending, err := c.guards.Read(ctx, intent.Entity)
		if err != nil {
			return acquired, classify(err)
		}
		if stored.root.State == coordination.StateCommitted &&
			(pending == nil || !pending.Active) && head != nil &&
			head.Epoch == stored.root.Epoch && head.LogicalDigest == intent.DesiredDigest &&
			bytes.Equal(head.WinnerID, intent.DesiredWinnerID) &&
			bytes.Equal(head.LPART, intent.LPART) &&
			bytes.Equal(head.LogicalPolicyID, intent.LogicalPolicyID) {
			continue
		}
		if pending != nil && pending.Active && bytes.Equal(pending.Intent.TXN, request.TXN) &&
			pending.Intent.Fence < stored.root.Fence {
			takeover, takeoverErr := c.guards.Takeover(
				ctx, *pending, stored.root.Owner, stored.lease.LeaseUntil, stored.root.Fence, c.now(),
			)
			if takeoverErr != nil {
				return acquired, classify(takeoverErr)
			}
			if takeover.Reconciled {
				continue
			}
			pending = &takeover.Pending
		}
		if pending != nil && pending.Active && bytes.Equal(pending.Intent.TXN, request.TXN) &&
			pending.Intent.Fence == stored.root.Fence {
			acquired = append(acquired, guard.Acquisition{
				Entity: intent.Entity, Decision: pending.Decision, Pending: *pending, Head: head,
			})
			continue
		}
		values, acquireErr := c.guards.AcquireMany(ctx, []guard.Intent{intent})
		if acquireErr != nil {
			return acquired, classify(acquireErr)
		}
		acquired = append(acquired, values...)
	}
	return acquired, nil
}

func publishedFrom(pending guard.Pending, root coordination.TxnRootV3) guard.Published {
	return guard.Published{
		TXN: rootOwnerTxn(pending), Epoch: root.Epoch, Fence: root.Fence,
		AuthorityGeneration: root.WriterAuthorityGeneration,
		LogicalDigest:       pending.Intent.DesiredDigest, LPART: pending.Intent.LPART,
		LogicalPolicyID: pending.Intent.LogicalPolicyID, State: pending.Intent.DesiredState,
		WinnerID: pending.Intent.DesiredWinnerID,
	}
}

func rootOwnerTxn(pending guard.Pending) coordination.TXN {
	return append(coordination.TXN(nil), pending.Intent.TXN...)
}

func (c *Coordinator) publishCopies(
	ctx context.Context,
	txn coordination.TXN,
	stored storedTxn,
	copies []CommitCopy,
) (storedTxn, error) {
	if len(copies) > coordination.MaxLPARTs {
		return storedTxn{}, errors.Join(ErrInvalid, errors.New("too many commit copies"))
	}
	rootCoord, _, _ := c.txnCoordinates(txn)
	beforeBytes, _ := coordination.MarshalTxnRootV3(stored.root)
	after := stored
	after.root.State = coordination.StateCommitted
	after.root.StateGeneration++
	afterBytes, err := coordination.MarshalTxnRootV3(after.root)
	if err != nil {
		return storedTxn{}, errors.Join(ErrInternal, err)
	}
	mutation := allocator.Mutation{
		Row:        rootCoord.Row,
		Conditions: []allocator.Condition{exactCondition(rootCoord, beforeBytes, int64(stored.root.StateGeneration))},
		Updates:    []allocator.Update{{Coordinate: rootCoord, Value: afterBytes, Timestamp: int64(after.root.StateGeneration)}},
	}
	expected := make([]allocator.Cell, 0, len(copies))
	size := len(afterBytes)
	for _, item := range copies {
		value := coordination.PartitionCommitCopyV1{
			State: coordination.StateCommitted, TXN: txn, Epoch: stored.root.Epoch,
			LPART: item.LPART, CopyGeneration: item.CopyGeneration,
			VisibilityDigest: item.VisibilityDigest, LogicalDigest: item.LogicalDigest,
			PhysicalCopyDigest:    item.PhysicalCopyDigest,
			RequiredIndexFamilies: item.RequiredIndexFamilies,
		}
		encoded, encodeErr := coordination.MarshalPartitionCommitCopyV1(value)
		if encodeErr != nil {
			return storedTxn{}, errors.Join(ErrInvalid, encodeErr)
		}
		qualifier := append([]byte(nil), coordination.E(item.LPART)...)
		qualifier = append(qualifier, coordination.U64(uint64(item.CopyGeneration))...)
		qualifier = append(qualifier, item.VisibilityDigest[:]...)
		cell := coordinate(rootCoord.Row, familyPublish, qualifier, item.Visibility)
		mutation.Conditions = append(mutation.Conditions, allocator.Condition{Coordinate: cell, Absent: true})
		mutation.Updates = append(mutation.Updates, allocator.Update{
			Coordinate: cell, Value: encoded, Timestamp: int64(stored.root.Epoch),
		})
		expected = append(expected, allocator.Cell{Coordinate: cell, Value: encoded, Timestamp: int64(stored.root.Epoch)})
		size += len(qualifier) + len(item.Visibility) + len(encoded)
	}
	if size > MaxCommitMutationBytes {
		return storedTxn{}, fmt.Errorf("%w: commit mutation exceeds 8 MiB", ErrInvalid)
	}
	for attempt := 0; ; attempt++ {
		status, writeErr := c.store.CompareAndMutate(ctx, mutation)
		if status == allocator.StatusAccepted {
			return after, nil
		}
		if status != allocator.StatusRejected && !errors.Is(writeErr, allocator.ErrConditionalUnknown) &&
			!resumeAfterDefiniteFailure(writeErr) {
			return storedTxn{}, errors.Join(ErrUnavailable, writeErr)
		}
		decision, got, reconcileErr := c.reconcilePublication(ctx, txn, stored, after, expected)
		switch decision {
		case publicationApplied:
			return got, nil
		case publicationRetry:
			if attempt < c.maxRetries {
				if err := c.wait(ctx); err != nil {
					return storedTxn{}, err
				}
				continue
			}
			return storedTxn{}, errors.Join(ErrUnavailable, allocator.ErrConditionalUnknown)
		default:
			return storedTxn{}, reconcileErr
		}
	}
}

type publicationDecision uint8

const (
	publicationApplied publicationDecision = iota + 1
	publicationRetry
	publicationCorrupt
)

func (c *Coordinator) reconcilePublication(
	ctx context.Context,
	txn coordination.TXN,
	before, after storedTxn,
	expected []allocator.Cell,
) (publicationDecision, storedTxn, error) {
	rootCoord, _, _ := c.txnCoordinates(txn)
	coordinates := []allocator.Coordinate{rootCoord}
	for _, cell := range expected {
		coordinates = append(coordinates, cell.Coordinate)
	}
	cells, err := c.store.ReadExact(ctx, coordinates)
	if err != nil {
		return publicationCorrupt, storedTxn{}, errors.Join(ErrUnavailable, err)
	}
	rootCell, rootFound := findCell(cells, rootCoord)
	if !rootFound {
		return publicationCorrupt, storedTxn{}, fmt.Errorf("%w: transaction root disappeared", ErrInternal)
	}
	root, decodeErr := coordination.UnmarshalTxnRootV3(rootCell.Value)
	if decodeErr != nil || rootCell.Timestamp != int64(root.StateGeneration) {
		return publicationCorrupt, storedTxn{}, fmt.Errorf("%w: transaction root is corrupt", ErrInternal)
	}
	matched, absent := 0, 0
	for _, wanted := range expected {
		cell, found := findCell(cells, wanted.Coordinate)
		if !found {
			absent++
			continue
		}
		if cell.Timestamp != wanted.Timestamp || !bytes.Equal(cell.Value, wanted.Value) {
			return publicationCorrupt, storedTxn{}, fmt.Errorf("%w: contradictory commit copy", ErrInternal)
		}
		matched++
	}
	if rootsEqual(root, after.root) && matched == len(expected) {
		got := after
		got.root = root
		return publicationApplied, got, nil
	}
	if rootsEqual(root, before.root) && absent == len(expected) {
		return publicationRetry, storedTxn{}, nil
	}
	return publicationCorrupt, storedTxn{}, fmt.Errorf("%w: mixed commit-copy readback", ErrInternal)
}

func (c *Coordinator) waitForCheckpoint(ctx context.Context, epoch coordination.Epoch) error {
	for attempt := 0; ; attempt++ {
		head, err := c.allocator.CurrentHead(ctx)
		if err != nil {
			return classify(err)
		}
		if head.Frontier >= epoch {
			return nil
		}
		_, err = c.allocator.AdvanceFrontier(ctx)
		if err != nil && !errors.Is(err, allocator.ErrNotFound) {
			return classify(err)
		}
		if attempt >= c.maxRetries {
			return ErrUnavailable
		}
		if err := c.wait(ctx); err != nil {
			return err
		}
	}
}

func (c *Coordinator) failBeforeCommit(
	ctx context.Context,
	txn coordination.TXN,
	stored storedTxn,
	reservation *coordination.ReservationV1,
	acquired []guard.Acquisition,
	cause error,
) error {
	state := coordination.StateAborted
	conflicted := errors.Is(cause, ErrConflict)
	if conflicted {
		state = coordination.StateConflicted
	}
	if errors.Is(cause, ErrInternal) {
		state = coordination.StatePoisoned
	}
	if stored.root.State >= coordination.StateCommitted || errors.Is(cause, ErrUnavailable) ||
		errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		return cause
	}
	for i := len(acquired) - 1; i >= 0; i-- {
		_ = c.guards.Abort(ctx, acquired[i].Pending, conflicted)
	}
	next := stored.root
	next.State, next.StateGeneration = state, next.StateGeneration+1
	updated, err := c.transition(ctx, txn, stored, next)
	if err != nil {
		return errors.Join(cause, err)
	}
	if reservation == nil && updated.root.Epoch != 0 {
		value, readErr := c.allocator.Reservation(ctx, updated.root.Epoch)
		if readErr == nil {
			reservation = &value
		}
	}
	if reservation != nil && !reservation.State.Terminal() {
		_, _, err = c.allocator.Terminalize(ctx, *reservation, state)
		if err != nil {
			return errors.Join(cause, classify(err))
		}
	}
	return cause
}

func (c *Coordinator) poison(ctx context.Context, txn coordination.TXN, stored storedTxn, cause error) error {
	if stored.root.State == coordination.StateCommitted {
		c.recordQuarantine(ctx, txn, "committed transaction consistency failure")
		return errors.Join(ErrQuarantined, ErrInternal)
	}
	err := c.failBeforeCommit(ctx, txn, stored, nil, nil, errors.Join(ErrInternal, cause))
	c.recordQuarantine(ctx, txn, "transaction consistency failure")
	return errors.Join(ErrQuarantined, err)
}

func (c *Coordinator) recordQuarantine(ctx context.Context, txn coordination.TXN, reason string) {
	if c.quarantine != nil {
		_ = c.quarantine.Record(ctx, c.domain, txn, reason)
	}
}

func (c *Coordinator) validatePins(ctx context.Context, root coordination.TxnRootV3, plan Plan) error {
	if c.pins == nil {
		return nil
	}
	if err := c.pins.Validate(ctx, root, plan); err != nil {
		return classify(err)
	}
	return nil
}

func (c *Coordinator) validateAuthority(ctx context.Context, authority Authority) error {
	head, err := c.allocator.CurrentHead(ctx)
	if err != nil {
		return classify(err)
	}
	if head.WriterAuthorityGeneration != authority.Generation ||
		head.WriterFence != authority.Fence || head.WriterMode != authority.Mode ||
		!bytes.Equal(head.WriterHolder, authority.Holder) ||
		head.RetentionGeneration != authority.RetentionGeneration ||
		head.HistoryFloor != authority.HistoryFloor {
		return ErrConflict
	}
	return nil
}

func (c *Coordinator) wait(ctx context.Context) error {
	timer := time.NewTimer(c.backoff)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *Coordinator) now() time.Time { return c.clock().UTC() }

func (c *Coordinator) lockTxn(txn coordination.TXN) func() {
	key := string(txn)
	c.lockMu.Lock()
	lock := c.txnLocks[key]
	if lock == nil {
		lock = &txnLock{}
		c.txnLocks[key] = lock
	}
	lock.refs++
	c.lockMu.Unlock()
	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()
		c.lockMu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(c.txnLocks, key)
		}
		c.lockMu.Unlock()
	}
}

func rootsEqual(a, b coordination.TxnRootV3) bool {
	left, leftErr := coordination.MarshalTxnRootV3(a)
	right, rightErr := coordination.MarshalTxnRootV3(b)
	return leftErr == nil && rightErr == nil && bytes.Equal(left, right)
}

func leasesEqual(a, b coordination.TxnLeaseV1) bool {
	left, leftErr := coordination.MarshalTxnLeaseV1(a)
	right, rightErr := coordination.MarshalTxnLeaseV1(b)
	return leftErr == nil && rightErr == nil && bytes.Equal(left, right)
}

func findCell(cells []allocator.Cell, wanted allocator.Coordinate) (allocator.Cell, bool) {
	for _, cell := range cells {
		if sameCoordinate(cell.Coordinate, wanted) {
			return cell, true
		}
	}
	return allocator.Cell{}, false
}

func stateError(state coordination.TxnState) error {
	switch state {
	case coordination.StateConflicted:
		return ErrConflict
	case coordination.StatePoisoned:
		return errors.Join(ErrQuarantined, ErrInternal)
	case coordination.StateAborted:
		return ErrUnavailable
	default:
		return ErrInternal
	}
}

func classify(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrInvalid), errors.Is(err, ErrConflict),
		errors.Is(err, ErrUnavailable), errors.Is(err, ErrInternal),
		errors.Is(err, ErrQuarantined), errors.Is(err, context.Canceled),
		errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.Is(err, allocator.ErrConflict), errors.Is(err, guard.ErrConflict),
		errors.Is(err, guard.ErrStaleAuthority), errors.Is(err, guard.ErrStaleRetention):
		return errors.Join(ErrConflict, err)
	case errors.Is(err, allocator.ErrCorruption), errors.Is(err, guard.ErrCorruption):
		return errors.Join(ErrInternal, err)
	default:
		return errors.Join(ErrUnavailable, err)
	}
}

// resumeAfterDefiniteFailure reports whether a non-rejected, non-ambiguous
// CompareAndMutate error should resume through the read-back reconcile path
// rather than abort. It returns true to resume, and false only for context
// cancellation, which short-circuits the loop. The Store contract
// (allocator/store.go) withholds ErrConditionalUnknown unless a mutation may
// have applied, so any other error means the conditional mutation definitely
// did not apply and is safe to reconcile and retry - strictly safer than the
// ambiguous ErrConditionalUnknown case the loops already handle.
func resumeAfterDefiniteFailure(err error) bool {
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}
