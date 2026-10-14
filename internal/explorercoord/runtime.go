/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements. See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership. The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License. You may obtain a copy of the License at
 *
 *     https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package explorercoord

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/phrocker/shoal-oss/internal/dirlock"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/localwal"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/guard"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/transaction"
)

const DefaultCoordinationTable = "_shoal_explorer_coordination"

var ErrIndeterminatePublication = errors.New("explorer coordination: publication outcome is indeterminate")

type Config struct {
	Directory             string
	Domain                coordination.DomainID
	Owner                 coordination.OwnerID
	Authority             transaction.Authority
	ControlVisibility     []byte
	CoordinationTable     string
	PhysicalTables        []string
	EngineOptions         engine.Options
	Pins                  transaction.PinValidator
	Lease                 time.Duration
	Clock                 func() time.Time
	MaxActiveReservations uint32
	MaxRetries            int
	RetryBackoff          time.Duration
	RecoveryLimit         int
	RecoveryConcurrency   int
	RecoveryRounds        int
	RecoveryBackoff       time.Duration
	RecoveryMaxPages      int
	ContentionWait        time.Duration
	DisableRecoveryOnOpen bool
	testStageHook         func(recoveryStage) error
}

type Request struct {
	Intent     Intent
	Owner      coordination.OwnerID
	LeaseUntil time.Time
}

// Result identifies the committed transaction and its durable logical digest.
type Result struct {
	TXN           coordination.TXN
	LogicalDigest coordination.Digest
	Epoch         coordination.Epoch
	Identities    []coordination.ResultIdentity
	Unchanged     bool
}

// Runtime owns one embedded engine, one process-exclusive directory lock, and
// the complete transaction composition for a domain. Multiple agents may call
// Publish concurrently through one Runtime. Multiple processes may not open
// the same directory.
type Runtime struct {
	mu                  sync.RWMutex
	recoveryMu          sync.Mutex
	closed              bool
	engine              *engine.Engine
	lock                *dirlock.Lock
	store               *EngineStore
	protocolStore       coordinationStore
	intents             *IntentStore
	physical            *Physical
	physicalTables      map[string]struct{}
	legacyRecords       *EngineStore
	allocator           *allocator.Client
	guards              *guard.Client
	coordinator         *transaction.Coordinator
	domain              coordination.DomainID
	owner               coordination.OwnerID
	authority           transaction.Authority
	lease               time.Duration
	clock               func() time.Time
	recoveryLimit       int
	recoveryPages       int
	recoveryConcurrency int
	recoveryRounds      int
	recoveryBackoff     time.Duration
	contentionWait      time.Duration
	intentCursor        []byte
}

func Open(config Config) (*Runtime, error) {
	if strings.TrimSpace(config.Directory) == "" {
		return nil, errors.Join(transaction.ErrInvalid, errors.New("runtime directory is required"))
	}
	if err := config.Domain.Validate(); err != nil {
		return nil, errors.Join(transaction.ErrInvalid, err)
	}
	if err := config.Owner.Validate(); err != nil {
		return nil, errors.Join(transaction.ErrInvalid, err)
	}
	if err := validateRuntimeAuthority(config.Authority); err != nil {
		return nil, err
	}
	if config.Authority.Mode != coordination.WriterModeEmbeddedPrimary {
		return nil, errors.Join(transaction.ErrInvalid, errors.New("embedded runtime requires embedded-primary authority"))
	}
	if config.CoordinationTable == "" {
		config.CoordinationTable = DefaultCoordinationTable
	}
	if !validTableName(config.CoordinationTable) {
		return nil, errors.Join(transaction.ErrInvalid, errors.New("coordination table name is invalid"))
	}
	if len(config.ControlVisibility) > coordination.MaxCoordinateBytes {
		return nil, errors.Join(transaction.ErrInvalid, errors.New("control visibility exceeds its bound"))
	}
	if config.EngineOptions.WALSyncMode != localwal.SyncFull {
		return nil, errors.Join(
			transaction.ErrInvalid,
			errors.New("transaction runtime requires full per-write WAL sync"),
		)
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	if config.Lease == 0 {
		config.Lease = time.Minute
	}
	if config.Lease < time.Second || config.Lease > 24*time.Hour {
		return nil, errors.Join(transaction.ErrInvalid, errors.New("runtime lease is outside its bound"))
	}
	if config.MaxActiveReservations == 0 {
		config.MaxActiveReservations = 1024
	}
	if config.MaxActiveReservations > coordination.MaxActiveReservations {
		return nil, errors.Join(transaction.ErrInvalid, errors.New("active reservation bound is invalid"))
	}
	if config.RecoveryLimit == 0 {
		config.RecoveryLimit = 256
	}
	if config.RecoveryLimit < 1 || config.RecoveryLimit > 10_000 {
		return nil, errors.Join(transaction.ErrInvalid, errors.New("recovery limit is outside its bound"))
	}
	if config.RecoveryConcurrency == 0 {
		config.RecoveryConcurrency = 4
	}
	if config.RecoveryConcurrency < 1 || config.RecoveryConcurrency > 256 {
		return nil, errors.Join(transaction.ErrInvalid, errors.New("recovery concurrency is outside its bound"))
	}
	if config.RecoveryRounds == 0 {
		config.RecoveryRounds = 3
	}
	if config.RecoveryRounds < 1 || config.RecoveryRounds > 100 {
		return nil, errors.Join(transaction.ErrInvalid, errors.New("recovery rounds are outside their bound"))
	}
	if config.RecoveryBackoff == 0 {
		config.RecoveryBackoff = 25 * time.Millisecond
	}
	if config.RecoveryBackoff < 0 || config.RecoveryBackoff > time.Minute {
		return nil, errors.Join(transaction.ErrInvalid, errors.New("recovery backoff is outside its bound"))
	}
	if config.RecoveryMaxPages == 0 {
		config.RecoveryMaxPages = 4096
	}
	if config.RecoveryMaxPages < 1 {
		return nil, errors.Join(transaction.ErrInvalid, errors.New("recovery page bound is invalid"))
	}
	if config.ContentionWait == 0 {
		config.ContentionWait = 2 * time.Second
	}
	if config.ContentionWait < 0 || config.ContentionWait > time.Minute {
		return nil, errors.Join(transaction.ErrInvalid, errors.New("contention wait is outside its bound"))
	}
	physicalTables := make(map[string]struct{}, len(config.PhysicalTables))
	for _, table := range config.PhysicalTables {
		if !validTableName(table) {
			return nil, errors.Join(transaction.ErrInvalid, errors.New("physical table name is invalid"))
		}
		if table == config.CoordinationTable {
			return nil, errors.Join(
				transaction.ErrInvalid,
				errors.New("coordination table cannot be a physical target"),
			)
		}
		physicalTables[table] = struct{}{}
	}
	lock, err := dirlock.Acquire(config.Directory, ".shoal-explorer-runtime.lock")
	if err != nil {
		return nil, fmt.Errorf("explorer coordination: acquire runtime directory: %w", err)
	}
	eng, err := engine.Open(config.Directory, config.EngineOptions)
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	closeOnError := func(openErr error) (*Runtime, error) {
		_ = eng.Close()
		_ = lock.Close()
		return nil, openErr
	}
	tables := append([]string{config.CoordinationTable}, config.PhysicalTables...)
	sort.Strings(tables)
	for index, table := range tables {
		if index > 0 && table == tables[index-1] {
			continue
		}
		if err := ensureTable(eng, table); err != nil {
			return closeOnError(err)
		}
	}
	store, err := NewEngineStore(eng, config.CoordinationTable)
	if err != nil {
		return closeOnError(err)
	}
	intents, err := NewIntentStore(config.Domain, config.ControlVisibility, store)
	if err != nil {
		return closeOnError(err)
	}
	physical, err := NewPhysical(eng)
	if err != nil {
		return closeOnError(err)
	}
	var protocolStore coordinationStore = store
	var physicalWriter transaction.PhysicalWriter = physical
	if config.testStageHook != nil {
		protocolStore = &stageStore{inner: store, hook: config.testStageHook}
		physicalWriter = &stageWriter{inner: physical, hook: config.testStageHook}
	}
	allocatorClient, err := allocator.New(allocator.Config{
		Domain: config.Domain, ControlVisibility: config.ControlVisibility,
		Store: protocolStore, Clock: config.Clock, MaxRetries: config.MaxRetries,
		RetryBackoff: config.RetryBackoff,
	})
	if err != nil {
		return closeOnError(err)
	}
	head, err := allocatorClient.CurrentHead(context.Background())
	if errors.Is(err, allocator.ErrNotFound) {
		head, err = allocatorClient.EnsureInitialized(context.Background(), allocator.InitializeOptions{
			HistoryFloor:        config.Authority.HistoryFloor,
			RetentionGeneration: config.Authority.RetentionGeneration,
			Authority: allocator.Authority{
				Generation: config.Authority.Generation, Mode: config.Authority.Mode,
				Holder: config.Authority.Holder, Fence: config.Authority.Fence,
			},
			MaxActiveReservations: config.MaxActiveReservations,
		})
	}
	if err != nil {
		return closeOnError(err)
	}
	if !headMatchesAuthority(head, config.Authority) {
		return closeOnError(errors.Join(transaction.ErrConflict, errors.New("configured authority does not match durable allocator state")))
	}
	runtime := &Runtime{
		engine: eng, lock: lock, store: store, intents: intents, physical: physical,
		protocolStore: protocolStore, allocator: allocatorClient,
		physicalTables: physicalTables,
		domain:         append(coordination.DomainID(nil), config.Domain...),
		owner:          append(coordination.OwnerID(nil), config.Owner...), authority: cloneAuthority(config.Authority),
		lease: config.Lease, clock: config.Clock, recoveryLimit: config.RecoveryLimit,
		recoveryPages: config.RecoveryMaxPages, recoveryConcurrency: config.RecoveryConcurrency,
		recoveryRounds: config.RecoveryRounds, recoveryBackoff: config.RecoveryBackoff,
		contentionWait: config.ContentionWait,
	}
	if err := runtime.compose(config, physicalWriter); err != nil {
		return closeOnError(err)
	}
	if config.DisableRecoveryOnOpen {
		if err := runtime.intents.IndexPending(
			context.Background(),
			config.RecoveryLimit,
			config.RecoveryMaxPages,
		); err != nil {
			return closeOnError(err)
		}
	} else {
		if err := runtime.Recover(context.Background()); err != nil {
			return closeOnError(err)
		}
	}
	return runtime, nil
}

func (r *Runtime) compose(config Config, physicalWriter transaction.PhysicalWriter) error {
	proxy := &txnStatusProxy{}
	guards, err := guard.New(guard.Config{
		Domain: r.domain, ControlVisibility: config.ControlVisibility, Store: r.protocolStore,
		Authority:  allocatorAuthoritySource{domain: r.domain, allocator: r.allocator},
		Retirement: activeRetirementSource{}, Transactions: proxy,
		Clock: r.clock, MaxRetries: config.MaxRetries, RetryBackoff: config.RetryBackoff,
	})
	if err != nil {
		return err
	}
	coordinator, err := transaction.New(transaction.Config{
		Domain: r.domain, ControlVisibility: config.ControlVisibility,
		Store: r.protocolStore, Allocator: r.allocator, Guards: guards,
		Materializer: r.intents, Writer: physicalWriter, Verifier: r.physical,
		Pins: config.Pins, Quarantine: r.intents, Clock: r.clock,
		MaxRetries: config.MaxRetries, RetryBackoff: config.RetryBackoff,
	})
	if err != nil {
		return err
	}
	proxy.coordinator = coordinator
	r.guards, r.coordinator = guards, coordinator
	return nil
}

// Publish persists the canonical intent before entering the existing
// transaction protocol. Errors carrying ErrIndeterminatePublication must be
// retried with the same intent/token or resolved through recovery.
func (r *Runtime) Publish(ctx context.Context, request Request) (Result, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.publishLocked(ctx, request)
}

func (r *Runtime) publishLocked(
	ctx context.Context,
	request Request,
) (Result, error) {
	if r.closed {
		return Result{}, transaction.ErrUnavailable
	}
	head, err := r.allocator.CurrentHead(ctx)
	if err != nil {
		return Result{}, err
	}
	if !headMatchesAuthority(head, r.authority) {
		return Result{}, transaction.ErrConflict
	}
	owner := request.Owner
	if len(owner) == 0 {
		owner = r.owner
	}
	if err := owner.Validate(); err != nil {
		return Result{}, errors.Join(transaction.ErrInvalid, err)
	}
	leaseUntil := request.LeaseUntil
	if leaseUntil.IsZero() {
		leaseUntil = r.clock().UTC().Add(r.lease)
	}
	if leaseUntil.Location() != time.UTC || !leaseUntil.After(r.clock().UTC()) {
		return Result{}, errors.Join(transaction.ErrInvalid, errors.New("publication lease must be a future UTC time"))
	}
	if err := r.validateIntentTables(request.Intent); err != nil {
		return Result{}, err
	}
	record, err := r.persistIntent(ctx, request.Intent)
	if err != nil {
		return Result{}, err
	}
	if hook := runtimeStageHook(r.protocolStore); hook != nil {
		if err := hook(recoveryStageIntent); err != nil {
			return Result{}, errors.Join(ErrIndeterminatePublication, err)
		}
	}
	publication := transaction.Publication{
		TXN: record.TXN, Token: record.Intent.Token, LogicalDigest: record.LogicalDigest,
		Owner: owner, LeaseUntil: leaseUntil, Authority: cloneAuthority(r.authority),
	}
	result, err := r.coordinator.Publish(ctx, publication)
	if err != nil && errors.Is(err, transaction.ErrUnavailable) &&
		!errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded) {
		advanced, available, checkErr := r.waitForExpectedResolution(
			ctx, record.TXN, record.Intent,
		)
		if checkErr != nil {
			err = errors.Join(err, checkErr)
		} else if advanced || available {
			result, err = r.coordinator.Publish(ctx, publication)
		}
	}
	if err != nil {
		classified := r.classifyPublishFailure(ctx, record.TXN, err)
		if !errors.Is(classified, ErrIndeterminatePublication) {
			if settleErr := r.intents.Settle(
				ctx, record.TXN, record.LogicalDigest,
			); settleErr != nil {
				classified = errors.Join(classified, settleErr)
			}
		}
		return Result{}, classified
	}
	if err := r.intents.Complete(ctx, record.TXN, record.LogicalDigest, result.Epoch); err != nil {
		return Result{}, errors.Join(ErrIndeterminatePublication, err)
	}
	if err := r.intents.Settle(ctx, record.TXN, record.LogicalDigest); err != nil {
		return Result{}, errors.Join(ErrIndeterminatePublication, err)
	}
	if hook := runtimeStageHook(r.protocolStore); hook != nil {
		if err := hook(recoveryStageComplete); err != nil {
			return Result{}, errors.Join(ErrIndeterminatePublication, err)
		}
	}
	return Result{
		TXN: append(coordination.TXN(nil), record.TXN...), LogicalDigest: record.LogicalDigest,
		Epoch: result.Epoch, Identities: cloneResultIdentities(result.Identities),
		Unchanged: result.Unchanged,
	}, nil
}

func (r *Runtime) PublishRecord(
	ctx context.Context,
	request explorer.RecordPublication,
) (explorer.RecordPublicationResult, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return explorer.RecordPublicationResult{}, transaction.PublicError(transaction.ErrUnavailable)
	}
	if err := r.validatePhysicalTable(request.Table); err != nil {
		return explorer.RecordPublicationResult{}, transaction.PublicError(err)
	}
	recordKey := request.RecordKey
	if len(recordKey) == 0 {
		recordKey = request.Token
	}
	if _, err := r.intents.attemptCoordinate(recordKey); err != nil {
		return explorer.RecordPublicationResult{}, transaction.PublicError(err)
	}
	lpart, err := Partition(r.domain, request.Partition)
	if err != nil {
		return explorer.RecordPublicationResult{}, transaction.PublicError(err)
	}
	intent, err := r.recordIntent(ctx, request, recordKey, lpart)
	if err != nil {
		return explorer.RecordPublicationResult{}, transaction.PublicError(err)
	}
	txn, err := DeriveTXN(r.domain, intent.Operation, intent.Token)
	if err != nil {
		return explorer.RecordPublicationResult{}, transaction.PublicError(err)
	}
	if err := r.persistRecordAttempt(ctx, intent, recordKey); err != nil {
		return explorer.RecordPublicationResult{}, recordPublicationError(err)
	}
	result, err := r.publishLocked(ctx, Request{Intent: intent})
	if err != nil {
		return explorer.RecordPublicationResult{}, recordPublicationError(
			r.classifyPersistedPublicationFailure(ctx, txn, err),
		)
	}
	return explorer.RecordPublicationResult{Epoch: result.Epoch, Unchanged: result.Unchanged}, nil
}

func (r *Runtime) persistIntent(
	ctx context.Context,
	intent Intent,
) (storedIntent, error) {
	r.recoveryMu.Lock()
	defer r.recoveryMu.Unlock()
	record, _, err := r.intents.Put(ctx, intent)
	if err != nil {
		return storedIntent{}, classifyIntentPersistenceFailure(err)
	}
	return record, nil
}

func (r *Runtime) persistRecordAttempt(
	ctx context.Context,
	intent Intent,
	recordKey []byte,
) error {
	r.recoveryMu.Lock()
	defer r.recoveryMu.Unlock()

	stored, replayed, err := r.intents.Put(ctx, intent)
	if err != nil {
		return classifyIntentPersistenceFailure(err)
	}
	if err := r.bindRecordAttempt(ctx, recordKey, stored.TXN); err != nil {
		classified := classifyIntentPersistenceFailure(err)
		if !replayed && !errors.Is(classified, ErrIndeterminatePublication) {
			if settleErr := r.intents.Settle(
				ctx,
				stored.TXN,
				stored.LogicalDigest,
			); settleErr != nil {
				classified = errors.Join(
					ErrIndeterminatePublication,
					classified,
					settleErr,
				)
			}
		}
		return classified
	}
	return nil
}

func (r *Runtime) validateIntentTables(intent Intent) error {
	for index, cell := range intent.Cells {
		if err := r.validatePhysicalTable(cell.Table); err != nil {
			return fmt.Errorf("intent cell %d: %w", index, err)
		}
	}
	return nil
}

func (r *Runtime) validatePhysicalTable(table string) error {
	if _, allowed := r.physicalTables[table]; !allowed {
		return errors.Join(
			transaction.ErrInvalid,
			errors.New("physical table was not configured for the runtime"),
		)
	}
	return nil
}

func classifyIntentPersistenceFailure(err error) error {
	if errors.Is(err, allocator.ErrConditionalUnknown) {
		return errors.Join(ErrIndeterminatePublication, err)
	}
	return err
}

func recordPublicationError(err error) error {
	public := transaction.PublicError(err)
	if errors.Is(err, ErrIndeterminatePublication) {
		public = explorer.MarkIndeterminateCommit(public)
	}
	return public
}

func (r *Runtime) RecordCommitted(
	ctx context.Context,
	request explorer.RecordPublication,
) (bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return false, transaction.ErrUnavailable
	}
	recordKey := request.RecordKey
	if len(recordKey) == 0 {
		recordKey = request.Token
	}
	txn, found, err := r.intents.Attempt(ctx, recordKey)
	if err != nil {
		return false, err
	}
	if !found {
		return r.legacyRecordAllowed(ctx, request)
	}
	record, err := r.intents.Load(ctx, txn)
	if errors.Is(err, transaction.ErrNotFound) {
		return false, fmt.Errorf(
			"%w: record attempt binding has no durable intent",
			transaction.ErrInternal,
		)
	}
	if err != nil {
		return false, err
	}
	_, committed, err := r.committedLocked(
		ctx,
		txn,
		record.LogicalDigest,
	)
	if err != nil {
		return false, err
	}
	if !committed {
		return false, nil
	}
	if !intentContainsRecordCell(record.Intent, request) {
		return false, fmt.Errorf("%w: committed record disagrees with its durable intent", transaction.ErrInternal)
	}
	return true, nil
}

func (r *Runtime) recordIntent(
	ctx context.Context,
	request explorer.RecordPublication,
	recordKey []byte,
	lpart coordination.LPART,
) (Intent, error) {
	token, err := r.recordIntentToken(ctx, request, recordKey)
	if err != nil {
		return Intent{}, err
	}
	txn, err := DeriveTXN(r.domain, request.Operation, token)
	if err != nil {
		return Intent{}, err
	}
	if stored, loadErr := r.intents.Load(ctx, txn); loadErr == nil {
		if !r.intentMatchesRecord(stored.Intent, request) ||
			stored.Intent.Guards[0].ExpectedEpoch != request.ExpectedEpoch ||
			stored.Intent.Guards[0].ExpectedDigest != request.ExpectedDigest {
			return Intent{}, transaction.ErrConflict
		}
		return cloneIntent(stored.Intent), nil
	} else if !errors.Is(loadErr, transaction.ErrNotFound) {
		return Intent{}, loadErr
	}
	entity := guard.Entity{
		Kind: request.EntityKind,
		ID:   append(coordination.EntityID(nil), request.EntityID...),
	}
	head, _, err := r.guards.Read(ctx, entity)
	if err != nil {
		return Intent{}, err
	}
	mode := guard.ModeAbsentOrIdentical
	if head == nil {
		if request.ExpectedEpoch != 0 ||
			request.ExpectedDigest != (coordination.Digest{}) {
			return Intent{}, transaction.ErrConflict
		}
	} else {
		if request.ExpectedEpoch == 0 ||
			head.Epoch != request.ExpectedEpoch ||
			head.LogicalDigest != request.ExpectedDigest {
			return Intent{}, transaction.ErrConflict
		}
		mode = guard.ModeMutate
	}
	return Intent{
		Operation:   append([]byte(nil), request.Operation...),
		Token:       append([]byte(nil), token...),
		RecordKey:   append([]byte(nil), recordKey...),
		RecordToken: append([]byte(nil), request.Token...),
		Cells: []Cell{{
			Table: request.Table, Row: append([]byte(nil), request.Row...),
			Family:     append([]byte(nil), request.Family...),
			Qualifier:  append([]byte(nil), request.Qualifier...),
			Visibility: append([]byte(nil), request.Visibility...),
			Value:      append([]byte(nil), request.Value...), EpochTimestamp: true,
			LPART: lpart, CopyGeneration: 1,
		}},
		Guards: []GuardIntent{{
			Entity: guard.Entity{
				Kind: entity.Kind,
				ID:   append(coordination.EntityID(nil), entity.ID...),
			},
			Mode: mode, ExpectedEpoch: request.ExpectedEpoch,
			ExpectedDigest:  request.ExpectedDigest,
			DesiredState:    guard.StateLive,
			DesiredWinnerID: append([]byte(nil), request.WinnerID...),
			LPART:           lpart, LogicalPolicyID: append([]byte(nil), request.LogicalPolicyID...),
			RetirementGeneration: 1,
		}},
		Results: []ResultIdentity{{
			Kind: append([]byte(nil), request.ResultKind...),
			ID:   append([]byte(nil), request.ResultID...),
		}},
	}, nil
}

func (r *Runtime) recordIntentToken(
	ctx context.Context,
	request explorer.RecordPublication,
	recordKey []byte,
) ([]byte, error) {
	current, found, err := r.intents.Attempt(ctx, recordKey)
	if err != nil {
		return nil, err
	}
	if !found {
		return append([]byte(nil), request.Token...), nil
	}
	record, err := r.intents.Load(ctx, current)
	if err != nil {
		return nil, err
	}
	if r.intentMatchesRecord(record.Intent, request) {
		expected := record.Intent.Guards[0]
		if expected.ExpectedEpoch != request.ExpectedEpoch ||
			expected.ExpectedDigest != request.ExpectedDigest {
			return nil, transaction.ErrConflict
		}
		return append([]byte(nil), record.Intent.Token...), nil
	}
	snapshot, err := r.coordinator.Inspect(ctx, current)
	if err != nil {
		return nil, err
	}
	if !snapshot.Root.State.Terminal() ||
		snapshot.Root.State == coordination.StateCommitted {
		return nil, transaction.ErrConflict
	}
	if err := r.settleTerminalGuards(
		ctx,
		current,
		record.Intent,
		snapshot.Root.State,
	); err != nil {
		return nil, err
	}
	hash := sha256.New()
	writeDigestPart(hash, []byte("shoal-record-retry-v1"))
	writeDigestPart(hash, request.Token)
	writeDigestPart(hash, current)
	return hash.Sum(nil), nil
}

func (r *Runtime) RecordHead(
	ctx context.Context,
	kind byte,
	id []byte,
) (*explorer.RecordPublicationHead, error) {
	head, _, err := r.ReadEntity(ctx, guard.Entity{
		Kind: kind, ID: append(coordination.EntityID(nil), id...),
	})
	if err != nil {
		return nil, err
	}
	if head == nil {
		return nil, nil
	}
	return &explorer.RecordPublicationHead{
		Epoch: head.Epoch, LogicalDigest: head.LogicalDigest,
		WinnerID: append([]byte(nil), head.WinnerID...),
	}, nil
}

func (r *Runtime) RecordAttempt(
	ctx context.Context,
	request explorer.RecordPublication,
) (*explorer.RecordPublicationAttempt, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return nil, transaction.ErrUnavailable
	}
	recordKey := request.RecordKey
	if len(recordKey) == 0 {
		recordKey = request.Token
	}
	txn, found, err := r.intents.Attempt(ctx, recordKey)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	snapshot, inspectErr := r.coordinator.Inspect(ctx, txn)
	if inspectErr == nil && snapshot.Root.State.Terminal() &&
		snapshot.Root.State != coordination.StateCommitted {
		return nil, nil
	}
	if inspectErr != nil && !errors.Is(inspectErr, transaction.ErrNotFound) {
		return nil, inspectErr
	}
	record, err := r.intents.Load(ctx, txn)
	if errors.Is(err, transaction.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(record.Intent.Guards) != 1 {
		return nil, fmt.Errorf(
			"%w: record attempt guard count is invalid", transaction.ErrInternal,
		)
	}
	var value []byte
	foundValue := false
	for _, cell := range record.Intent.Cells {
		if cell.Table == request.Table &&
			bytes.Equal(cell.Row, request.Row) &&
			bytes.Equal(cell.Family, request.Family) &&
			bytes.Equal(cell.Qualifier, request.Qualifier) &&
			bytes.Equal(cell.Visibility, request.Visibility) {
			if foundValue {
				return nil, fmt.Errorf(
					"%w: record attempt has duplicate physical cells",
					transaction.ErrInternal,
				)
			}
			foundValue = true
			value = append([]byte(nil), cell.Value...)
		}
	}
	if !foundValue {
		return nil, transaction.ErrConflict
	}
	expected := record.Intent.Guards[0]
	return &explorer.RecordPublicationAttempt{
		Value: value, ExpectedEpoch: expected.ExpectedEpoch,
		ExpectedDigest: expected.ExpectedDigest,
	}, nil
}

func (r *Runtime) PendingPublications(
	ctx context.Context,
) (bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return false, transaction.ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return r.intents.HasPending(), nil
}

func (r *Runtime) bindRecordAttempt(
	ctx context.Context,
	key []byte,
	txn coordination.TXN,
) error {
	current, found, err := r.intents.readAttempt(ctx, key)
	if err != nil {
		return err
	}
	if !found {
		return r.intents.SetAttempt(ctx, key, nil, txn)
	}
	if bytes.Equal(current.txn, txn) {
		return nil
	}
	snapshot, err := r.coordinator.Inspect(ctx, current.txn)
	if err != nil {
		return err
	}
	if !snapshot.Root.State.Terminal() ||
		snapshot.Root.State == coordination.StateCommitted {
		return transaction.ErrConflict
	}
	return r.intents.setAttempt(ctx, key, current, txn)
}

func (r *Runtime) intentMatchesRecord(
	intent Intent,
	request explorer.RecordPublication,
) bool {
	if !bytes.Equal(intent.Operation, request.Operation) ||
		!bytes.Equal(intent.RecordToken, request.Token) ||
		len(intent.Cells) != 1 || len(intent.Guards) != 1 || len(intent.Results) != 1 {
		return false
	}
	recordKey := request.RecordKey
	if len(recordKey) == 0 {
		recordKey = request.Token
	}
	if !bytes.Equal(intent.RecordKey, recordKey) {
		return false
	}
	cell := intent.Cells[0]
	entity := intent.Guards[0]
	result := intent.Results[0]
	lpart, err := Partition(r.domain, request.Partition)
	if err != nil {
		return false
	}
	return cell.Table == request.Table &&
		bytes.Equal(cell.Row, request.Row) &&
		bytes.Equal(cell.Family, request.Family) &&
		bytes.Equal(cell.Qualifier, request.Qualifier) &&
		bytes.Equal(cell.Visibility, request.Visibility) &&
		bytes.Equal(cell.Value, request.Value) &&
		bytes.Equal(cell.LPART, lpart) &&
		entity.Entity.Kind == request.EntityKind &&
		bytes.Equal(entity.Entity.ID, request.EntityID) &&
		bytes.Equal(entity.DesiredWinnerID, request.WinnerID) &&
		bytes.Equal(entity.LPART, lpart) &&
		bytes.Equal(entity.LogicalPolicyID, request.LogicalPolicyID) &&
		bytes.Equal(result.Kind, request.ResultKind) &&
		bytes.Equal(result.ID, request.ResultID)
}

func intentContainsRecordCell(
	intent Intent,
	request explorer.RecordPublication,
) bool {
	for _, cell := range intent.Cells {
		if cell.Table == request.Table &&
			bytes.Equal(cell.Row, request.Row) &&
			bytes.Equal(cell.Family, request.Family) &&
			bytes.Equal(cell.Qualifier, request.Qualifier) &&
			bytes.Equal(cell.Visibility, request.Visibility) &&
			bytes.Equal(cell.Value, request.Value) {
			return true
		}
	}
	return false
}

// RecoverPage processes one bounded page of durable intents and transaction
// roots. Callers may resume by invoking it again while more is true.
func (r *Runtime) RecoverPage(ctx context.Context) (more bool, err error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return false, transaction.ErrUnavailable
	}
	r.recoveryMu.Lock()
	defer r.recoveryMu.Unlock()
	candidates, next, err := r.intents.Candidates(
		ctx, r.intentCursor, r.recoveryLimit,
	)
	if err != nil {
		return false, errors.Join(transaction.ErrUnavailable, err)
	}
	workerCount := min(r.recoveryConcurrency, len(candidates))
	jobs := make(chan coordination.TXN)
	errs := make(chan error, len(candidates))
	var workers sync.WaitGroup
	for range workerCount {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for txn := range jobs {
				if recoverErr := r.recoverPending(ctx, txn); recoverErr != nil {
					errs <- recoverErr
				}
			}
		}()
	}
send:
	for _, txn := range candidates {
		select {
		case jobs <- append(coordination.TXN(nil), txn...):
		case <-ctx.Done():
			errs <- ctx.Err()
			break send
		}
	}
	close(jobs)
	workers.Wait()
	close(errs)
	var combined error
	for recoverErr := range errs {
		combined = errors.Join(combined, recoverErr)
	}
	if combined != nil {
		return len(next) != 0, combined
	}
	if len(next) == 0 {
		if err := r.intents.IndexPending(
			ctx,
			r.recoveryLimit,
			r.recoveryPages,
		); err != nil {
			return false, errors.Join(transaction.ErrUnavailable, err)
		}
	}
	r.intentCursor = append(r.intentCursor[:0], next...)
	if len(next) == 0 {
		r.intentCursor = nil
	}
	return len(next) != 0, nil
}

func (r *Runtime) Recover(ctx context.Context) error {
	for page := 0; page < r.recoveryPages; page++ {
		more, err := r.RecoverPage(ctx)
		if err != nil {
			return err
		}
		if !more {
			return nil
		}
	}
	return errors.Join(transaction.ErrUnavailable, errors.New("recovery page bound reached"))
}

func (r *Runtime) recoverIntent(ctx context.Context, txn coordination.TXN) error {
	record, err := r.intents.Load(ctx, txn)
	if errors.Is(err, transaction.ErrNotFound) {
		return r.intents.removePending(ctx, txn)
	}
	if err != nil {
		return err
	}
	if err := r.validateIntentTables(record.Intent); err != nil {
		return err
	}
	if len(record.Intent.RecordKey) != 0 {
		if err := r.bindRecordAttempt(
			ctx,
			record.Intent.RecordKey,
			record.TXN,
		); err != nil {
			classified := classifyIntentPersistenceFailure(err)
			if errors.Is(classified, transaction.ErrConflict) &&
				!errors.Is(classified, ErrIndeterminatePublication) {
				return r.intents.Settle(
					ctx,
					record.TXN,
					record.LogicalDigest,
				)
			}
			return classified
		}
	}
	if _, complete, err := r.intents.Completed(ctx, txn, record.LogicalDigest); err != nil {
		return err
	} else if complete {
		return r.intents.Settle(ctx, txn, record.LogicalDigest)
	}
	leaseUntil := r.clock().UTC().Add(r.lease)
	snapshot, inspectErr := r.coordinator.Inspect(ctx, txn)
	var result transaction.Result
	switch {
	case inspectErr == nil && snapshot.Root.State.Terminal() &&
		snapshot.Root.State != coordination.StateCommitted:
		if err := r.settleTerminalGuards(
			ctx,
			txn,
			record.Intent,
			snapshot.Root.State,
		); err != nil {
			return err
		}
		return r.intents.Settle(ctx, txn, record.LogicalDigest)
	case inspectErr == nil && snapshot.Root.State.Nonterminal() &&
		snapshot.Lease.LeaseUntil.After(r.clock().UTC()) &&
		!bytes.Equal(snapshot.Root.Owner, r.owner):
		return nil
	case inspectErr == nil:
		result, err = r.coordinator.Recover(
			ctx, txn, r.owner, leaseUntil, cloneAuthority(r.authority),
		)
	case errors.Is(inspectErr, transaction.ErrNotFound):
		result, err = r.coordinator.Publish(ctx, transaction.Publication{
			TXN: record.TXN, Token: record.Intent.Token, LogicalDigest: record.LogicalDigest,
			Owner: r.owner, LeaseUntil: leaseUntil, Authority: cloneAuthority(r.authority),
		})
	default:
		return inspectErr
	}
	if err != nil {
		snapshot, inspectErr := r.coordinator.Inspect(ctx, txn)
		if inspectErr == nil && snapshot.Root.State.Terminal() &&
			snapshot.Root.State != coordination.StateCommitted {
			if settleErr := r.settleTerminalGuards(
				ctx,
				txn,
				record.Intent,
				snapshot.Root.State,
			); settleErr != nil {
				return errors.Join(err, settleErr)
			}
			return r.intents.Settle(ctx, txn, record.LogicalDigest)
		}
		if inspectErr != nil && !errors.Is(inspectErr, transaction.ErrNotFound) {
			return errors.Join(err, inspectErr)
		}
		return err
	}
	if err := r.intents.Complete(
		ctx, record.TXN, record.LogicalDigest, result.Epoch,
	); err != nil {
		return err
	}
	return r.intents.Settle(ctx, record.TXN, record.LogicalDigest)
}

func (r *Runtime) settleTerminalGuards(
	ctx context.Context,
	txn coordination.TXN,
	intent Intent,
	state coordination.TxnState,
) error {
	for _, expected := range intent.Guards {
		_, pending, err := r.guards.Read(ctx, expected.Entity)
		if err != nil {
			return err
		}
		if pending == nil || !pending.Active ||
			!bytes.Equal(pending.Intent.TXN, txn) {
			continue
		}
		if err := r.guards.Abort(
			ctx,
			*pending,
			state == coordination.StateConflicted,
		); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runtime) recoverPending(
	ctx context.Context,
	txn coordination.TXN,
) error {
	for round := 0; round < r.recoveryRounds; round++ {
		err := r.recoverIntent(ctx, txn)
		if err == nil || !errors.Is(err, transaction.ErrUnavailable) {
			return err
		}
		record, loadErr := r.intents.Load(ctx, txn)
		if loadErr != nil {
			return errors.Join(err, loadErr)
		}
		advanced, available, waitErr := r.waitForExpectedResolution(
			ctx, txn, record.Intent,
		)
		if waitErr != nil {
			return errors.Join(err, waitErr)
		}
		if advanced || available {
			continue
		}
		timer := time.NewTimer(r.recoveryBackoff << min(round, 8))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return transaction.ErrUnavailable
}

func (r *Runtime) waitForExpectedResolution(
	ctx context.Context,
	txn coordination.TXN,
	intent Intent,
) (advanced bool, available bool, err error) {
	if r.contentionWait == 0 {
		return false, false, nil
	}
	timeout := time.NewTimer(r.contentionWait)
	defer timeout.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		advanced, available, err = r.expectedHeadResolution(ctx, txn, intent)
		if err != nil || advanced || available {
			return advanced, available, err
		}
		select {
		case <-ctx.Done():
			return false, false, ctx.Err()
		case <-timeout.C:
			return false, false, nil
		case <-ticker.C:
		}
	}
}

func (r *Runtime) expectedHeadResolution(
	ctx context.Context,
	txn coordination.TXN,
	intent Intent,
) (advanced bool, available bool, err error) {
	foundExpected := false
	busy := false
	for _, expected := range intent.Guards {
		if expected.Mode != guard.ModeAbsentOrIdentical &&
			expected.Mode != guard.ModeMutate &&
			expected.Mode != guard.ModeRetire {
			continue
		}
		foundExpected = true
		head, pending, err := r.guards.Read(ctx, expected.Entity)
		if err != nil {
			return false, false, err
		}
		if expected.Mode == guard.ModeAbsentOrIdentical {
			if head != nil {
				return true, false, nil
			}
		} else {
			if head == nil || head.Epoch != expected.ExpectedEpoch ||
				head.LogicalDigest != expected.ExpectedDigest {
				return true, false, nil
			}
		}
		if pending != nil && pending.Active &&
			!bytes.Equal(pending.Intent.TXN, txn) {
			busy = true
		}
	}
	return false, foundExpected && !busy, nil
}

func (r *Runtime) classifyPublishFailure(
	ctx context.Context,
	txn coordination.TXN,
	err error,
) error {
	if errors.Is(err, transaction.ErrInvalid) || errors.Is(err, transaction.ErrConflict) {
		return err
	}
	snapshot, inspectErr := r.coordinator.Inspect(ctx, txn)
	if inspectErr == nil && snapshot.Root.State.Terminal() &&
		snapshot.Root.State != coordination.StateCommitted {
		return err
	}
	return errors.Join(ErrIndeterminatePublication, err)
}

func (r *Runtime) classifyPersistedPublicationFailure(
	ctx context.Context,
	txn coordination.TXN,
	err error,
) error {
	if errors.Is(err, ErrIndeterminatePublication) {
		return err
	}
	snapshot, inspectErr := r.coordinator.Inspect(ctx, txn)
	if inspectErr == nil &&
		snapshot.Root.State.Terminal() &&
		snapshot.Root.State != coordination.StateCommitted {
		return err
	}
	if inspectErr != nil && !errors.Is(inspectErr, transaction.ErrNotFound) {
		return errors.Join(ErrIndeterminatePublication, err, inspectErr)
	}
	return errors.Join(ErrIndeterminatePublication, err)
}

func (r *Runtime) Authority() transaction.Authority {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return cloneAuthority(r.authority)
}

// EmbeddedEngine returns the runtime-owned engine for in-process adapters that
// must share its WAL and directory lock. Callers must not close the engine and
// must release their adapters before closing the runtime.
func (r *Runtime) EmbeddedEngine() *engine.Engine {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return nil
	}
	return r.engine
}

// CurrentHead returns the authoritative allocator state for diagnostics and
// dependency code that needs the current publication frontier.
func (r *Runtime) CurrentHead(
	ctx context.Context,
) (coordination.AllocatorHeadV1, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return coordination.AllocatorHeadV1{}, transaction.ErrUnavailable
	}
	return r.allocator.CurrentHead(ctx)
}

// ReadEntity returns the committed and pending guard records for one entity.
func (r *Runtime) ReadEntity(
	ctx context.Context,
	entity guard.Entity,
) (*guard.Head, *guard.Pending, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return nil, nil, transaction.ErrUnavailable
	}
	return r.guards.Read(ctx, entity)
}

// Inspect returns the authoritative transaction root and lease.
func (r *Runtime) Inspect(
	ctx context.Context,
	txn coordination.TXN,
) (transaction.Snapshot, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return transaction.Snapshot{}, transaction.ErrUnavailable
	}
	return r.coordinator.Inspect(ctx, txn)
}

func (r *Runtime) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	engineErr := r.engine.Close()
	lockErr := r.lock.Close()
	return errors.Join(engineErr, lockErr)
}

type EmbeddedExplorer struct {
	Runtime  *Runtime
	Explorer *explorer.Explorer
}

// OpenExplorer opens the production embedded composition and wires
// deterministic document-revision record publication through it. Other
// Explorer record families retain their existing persistence path.
func OpenExplorer(
	config Config,
	options explorer.Options,
) (*EmbeddedExplorer, error) {
	config.PhysicalTables = append(
		append([]string(nil), config.PhysicalTables...),
		explorer.EmbeddedTableName,
	)
	runtime, err := Open(config)
	if err != nil {
		return nil, err
	}
	if err := runtime.enableExplorerLegacyCompatibility(
		context.Background(),
	); err != nil {
		_ = runtime.Close()
		return nil, err
	}
	corpus, err := explorer.OpenWithEmbeddedEngine(runtime.engine, options, runtime)
	if err != nil {
		_ = runtime.Close()
		return nil, err
	}
	return &EmbeddedExplorer{Runtime: runtime, Explorer: corpus}, nil
}

func (e *EmbeddedExplorer) Close() error {
	if e == nil {
		return nil
	}
	var explorerErr error
	if e.Explorer != nil {
		explorerErr = e.Explorer.Close()
	}
	var runtimeErr error
	if e.Runtime != nil {
		runtimeErr = e.Runtime.Close()
	}
	return errors.Join(explorerErr, runtimeErr)
}

type allocatorAuthoritySource struct {
	domain    coordination.DomainID
	allocator *allocator.Client
}

func (s allocatorAuthoritySource) Current(
	ctx context.Context,
	domain coordination.DomainID,
) (guard.Authority, error) {
	if s.allocator == nil || !bytes.Equal(domain, s.domain) {
		return guard.Authority{}, guard.ErrUnavailable
	}
	head, err := s.allocator.CurrentHead(ctx)
	if err != nil {
		return guard.Authority{}, err
	}
	return guard.Authority{
		Generation: head.WriterAuthorityGeneration, Fence: head.WriterFence,
		RetentionGeneration: head.RetentionGeneration, HistoryFloor: head.HistoryFloor,
	}, nil
}

type activeRetirementSource struct{}

func (activeRetirementSource) Retired(
	context.Context,
	coordination.DomainID,
	guard.Entity,
) (bool, coordination.Generation, error) {
	return false, 1, nil
}

type txnStatusProxy struct {
	coordinator *transaction.Coordinator
}

func (p *txnStatusProxy) Status(
	ctx context.Context,
	domain coordination.DomainID,
	txn coordination.TXN,
) (guard.TxnDisposition, error) {
	if p.coordinator == nil {
		return 0, transaction.ErrUnavailable
	}
	return p.coordinator.Status(ctx, domain, txn)
}

func ensureTable(eng *engine.Engine, table string) error {
	for _, existing := range eng.TableNames() {
		if existing == table {
			return nil
		}
	}
	return eng.CreateTable(table, engine.TableOptions{})
}

func validateRuntimeAuthority(authority transaction.Authority) error {
	if err := authority.Generation.Validate(); err != nil {
		return errors.Join(transaction.ErrInvalid, err)
	}
	if err := authority.Fence.Validate(); err != nil {
		return errors.Join(transaction.ErrInvalid, err)
	}
	if err := authority.Holder.Validate(); err != nil {
		return errors.Join(transaction.ErrInvalid, err)
	}
	if err := authority.Mode.Validate(); err != nil {
		return errors.Join(transaction.ErrInvalid, err)
	}
	if err := authority.RetentionGeneration.Validate(); err != nil {
		return errors.Join(transaction.ErrInvalid, err)
	}
	if err := authority.HistoryFloor.Validate(); err != nil {
		return errors.Join(transaction.ErrInvalid, err)
	}
	return nil
}

func headMatchesAuthority(
	head coordination.AllocatorHeadV1,
	authority transaction.Authority,
) bool {
	return head.WriterAuthorityGeneration == authority.Generation &&
		head.WriterFence == authority.Fence &&
		head.WriterMode == authority.Mode &&
		bytes.Equal(head.WriterHolder, authority.Holder) &&
		head.RetentionGeneration == authority.RetentionGeneration &&
		head.HistoryFloor == authority.HistoryFloor
}

func cloneAuthority(value transaction.Authority) transaction.Authority {
	value.Holder = append(coordination.OwnerID(nil), value.Holder...)
	return value
}

func cloneResultIdentities(values []coordination.ResultIdentity) []coordination.ResultIdentity {
	result := make([]coordination.ResultIdentity, len(values))
	for index := range values {
		result[index].Kind = append([]byte(nil), values[index].Kind...)
		result[index].ID = append([]byte(nil), values[index].ID...)
	}
	return result
}

var _ explorer.RecordPublicationAdapter = (*Runtime)(nil)

type coordinationStore interface {
	allocator.Store
	ScanPrefix(context.Context, []byte, []byte, []byte, []byte, int) ([]allocator.Cell, error)
	ScanPrefixFrom(context.Context, []byte, []byte, []byte, []byte, []byte, int) ([]allocator.Cell, error)
}

type recoveryStage uint8

const (
	recoveryStageIntent recoveryStage = iota + 1
	recoveryStagePhysical
	recoveryStagePrepared
	recoveryStageCommitted
	recoveryStageCheckpoint
	recoveryStageComplete
)

type stageStore struct {
	inner *EngineStore
	hook  func(recoveryStage) error
}

func (s *stageStore) ReadExact(
	ctx context.Context,
	coordinates []allocator.Coordinate,
) ([]allocator.Cell, error) {
	return s.inner.ReadExact(ctx, coordinates)
}

func (s *stageStore) ScanRowPrefix(
	ctx context.Context,
	row, family, qualifier, visibility []byte,
	limit int,
) ([]allocator.Cell, error) {
	return s.inner.ScanRowPrefix(ctx, row, family, qualifier, visibility, limit)
}

func (s *stageStore) ScanPrefix(
	ctx context.Context,
	row, family, qualifier, visibility []byte,
	limit int,
) ([]allocator.Cell, error) {
	return s.inner.ScanPrefix(ctx, row, family, qualifier, visibility, limit)
}

func (s *stageStore) ScanPrefixFrom(
	ctx context.Context,
	prefix, start, family, qualifier, visibility []byte,
	limit int,
) ([]allocator.Cell, error) {
	return s.inner.ScanPrefixFrom(ctx, prefix, start, family, qualifier, visibility, limit)
}

func (s *stageStore) CompareAndMutate(
	ctx context.Context,
	mutation allocator.Mutation,
) (allocator.Status, error) {
	status, err := s.inner.CompareAndMutate(ctx, mutation)
	if status != allocator.StatusAccepted || err != nil || s.hook == nil {
		return status, err
	}
	stage := recoveryStage(0)
	for _, update := range mutation.Updates {
		if root, decodeErr := coordination.UnmarshalTxnRootV3(update.Value); decodeErr == nil {
			switch root.State {
			case coordination.StatePrepared:
				stage = recoveryStagePrepared
			case coordination.StateCommitted:
				stage = recoveryStageCommitted
			}
		}
		if head, decodeErr := coordination.UnmarshalAllocatorHeadV1(update.Value); decodeErr == nil &&
			head.Frontier != 0 {
			stage = recoveryStageCheckpoint
		}
	}
	if stage != 0 {
		if hookErr := s.hook(stage); hookErr != nil {
			return allocator.StatusUnknown, hookErr
		}
	}
	return status, nil
}

type stageWriter struct {
	inner *Physical
	hook  func(recoveryStage) error
}

func (w *stageWriter) Write(
	ctx context.Context,
	epoch coordination.Epoch,
	cells []transaction.PhysicalCell,
) error {
	if err := w.inner.Write(ctx, epoch, cells); err != nil {
		return err
	}
	if w.hook != nil {
		return w.hook(recoveryStagePhysical)
	}
	return nil
}

func runtimeStageHook(store coordinationStore) func(recoveryStage) error {
	if value, ok := store.(*stageStore); ok {
		return value.hook
	}
	return nil
}
