package allocator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
)

type faultMode uint8

const (
	faultNone faultMode = iota
	faultUnknownBefore
	faultUnknownAfter
	faultReject
	faultPartialFirst
)

type memoryStore struct {
	mu       sync.Mutex
	cells    map[string][]storedCell
	faults   []faultMode
	captured []Mutation
}

type storedCell struct {
	cell    Cell
	deleted bool
}

func newMemoryStore() *memoryStore { return &memoryStore{cells: make(map[string][]storedCell)} }

func cellKey(c Coordinate) string {
	return string(c.Row) + "\x00" + string(c.Family) + "\x00" + string(c.Qualifier) + "\x00" + string(c.Visibility)
}

func (s *memoryStore) ReadExact(ctx context.Context, coordinates []Coordinate) ([]Cell, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]Cell, 0, len(coordinates))
	for _, coordinate := range coordinates {
		if cell, ok := s.visibleCell(coordinate); ok {
			result = append(result, cloneCell(cell))
		}
	}
	return result, nil
}

func (s *memoryStore) ScanRowPrefix(_ context.Context, row, family, start, visibility []byte, limit int) ([]Cell, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result []Cell
	for _, versions := range s.cells {
		cell, ok := highestCell(versions)
		if !ok {
			continue
		}
		if bytes.Equal(cell.Coordinate.Row, row) && bytes.Equal(cell.Coordinate.Family, family) &&
			bytes.Equal(cell.Coordinate.Visibility, visibility) &&
			bytes.Compare(cell.Coordinate.Qualifier, start) >= 0 {
			result = append(result, cloneCell(cell))
		}
	}
	sortCells(result)
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func sortCells(cells []Cell) {
	for i := range cells {
		for j := i + 1; j < len(cells); j++ {
			if bytes.Compare(cells[j].Coordinate.Qualifier, cells[i].Coordinate.Qualifier) < 0 {
				cells[i], cells[j] = cells[j], cells[i]
			}
		}
	}
}

func (s *memoryStore) CompareAndMutate(ctx context.Context, mutation Mutation) (Status, error) {
	if err := ctx.Err(); err != nil {
		return StatusUnknown, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.captured = append(s.captured, cloneMutation(mutation))
	fault := faultNone
	if len(s.faults) != 0 {
		fault, s.faults = s.faults[0], s.faults[1:]
	}
	if fault == faultUnknownBefore {
		return StatusUnknown, ErrConditionalUnknown
	}
	for _, condition := range mutation.Conditions {
		cell, found := s.visibleCell(condition.Coordinate)
		if condition.Absent {
			if found {
				return StatusRejected, nil
			}
		} else if !found || !bytes.Equal(cell.Value, condition.Value) ||
			condition.TimestampSet && cell.Timestamp != condition.Timestamp {
			return StatusRejected, nil
		}
	}
	if fault == faultReject {
		return StatusRejected, nil
	}
	updates := mutation.Updates
	if fault == faultPartialFirst {
		updates = updates[:1]
	}
	for _, update := range updates {
		s.put(update.Coordinate, update.Value, update.Timestamp, update.Delete)
	}
	if fault == faultUnknownAfter || fault == faultPartialFirst {
		return StatusUnknown, ErrConditionalUnknown
	}
	return StatusAccepted, nil
}

func cloneCell(cell Cell) Cell {
	return Cell{
		Coordinate: cell.Coordinate.clone(), Value: append([]byte(nil), cell.Value...),
		Timestamp: cell.Timestamp,
	}
}

func cloneMutation(value Mutation) Mutation {
	result := Mutation{Row: append([]byte(nil), value.Row...)}
	for _, condition := range value.Conditions {
		result.Conditions = append(result.Conditions, Condition{
			Coordinate: condition.Coordinate.clone(), Value: append([]byte(nil), condition.Value...),
			Absent: condition.Absent, Timestamp: condition.Timestamp, TimestampSet: condition.TimestampSet,
		})
	}
	for _, update := range value.Updates {
		result.Updates = append(result.Updates, Update{
			Coordinate: update.Coordinate.clone(), Value: append([]byte(nil), update.Value...),
			Delete: update.Delete, Timestamp: update.Timestamp,
		})
	}
	return result
}

func (s *memoryStore) put(coordinate Coordinate, value []byte, timestamp int64, deleted bool) {
	key := cellKey(coordinate)
	versions := s.cells[key]
	entry := storedCell{
		cell: Cell{
			Coordinate: coordinate.clone(), Value: append([]byte(nil), value...),
			Timestamp: timestamp,
		},
		deleted: deleted,
	}
	for i := range versions {
		if versions[i].cell.Timestamp == timestamp {
			versions[i] = entry
			s.cells[key] = versions
			return
		}
	}
	s.cells[key] = append(versions, entry)
}

func (s *memoryStore) visibleCell(coordinate Coordinate) (Cell, bool) {
	return highestCell(s.cells[cellKey(coordinate)])
}

func highestCell(versions []storedCell) (Cell, bool) {
	if len(versions) == 0 {
		return Cell{}, false
	}
	highest := versions[0]
	for _, version := range versions[1:] {
		if version.cell.Timestamp > highest.cell.Timestamp ||
			version.cell.Timestamp == highest.cell.Timestamp && version.deleted && !highest.deleted {
			highest = version
		}
	}
	if highest.deleted {
		return Cell{}, false
	}
	return highest.cell, true
}

func TestMemoryStoreModelsTimestampWinsAndDeletes(t *testing.T) {
	store := newMemoryStore()
	coordinate := Coordinate{Row: []byte("row"), Family: []byte("q"), Qualifier: []byte("head")}
	store.put(coordinate, []byte("newer"), 5, false)
	store.put(coordinate, []byte("shadowed"), 3, false)
	cells, err := store.ReadExact(context.Background(), []Coordinate{coordinate})
	if err != nil || len(cells) != 1 || string(cells[0].Value) != "newer" || cells[0].Timestamp != 5 {
		t.Fatalf("highest timestamp read = %#v, %v", cells, err)
	}
	store.put(coordinate, nil, 6, true)
	cells, err = store.ReadExact(context.Background(), []Coordinate{coordinate})
	if err != nil || len(cells) != 0 {
		t.Fatalf("delete did not hide older values: %#v, %v", cells, err)
	}
	store.put(coordinate, []byte("still-shadowed"), 5, false)
	cells, _ = store.ReadExact(context.Background(), []Coordinate{coordinate})
	if len(cells) != 0 {
		t.Fatalf("older put resurrected deleted coordinate: %#v", cells)
	}
	store.put(coordinate, []byte("visible"), 7, false)
	cells, _ = store.ReadExact(context.Background(), []Coordinate{coordinate})
	if len(cells) != 1 || string(cells[0].Value) != "visible" {
		t.Fatalf("newer put did not supersede delete: %#v", cells)
	}
}

func testHead(max uint32) coordination.AllocatorHeadV1 {
	return coordination.AllocatorHeadV1{
		HeadGeneration: 1, NextEpoch: 1, HistoryFloor: 1, RetentionGeneration: 2,
		WriterAuthorityGeneration: 3, WriterMode: coordination.WriterModeAccumuloPrimary,
		WriterHolder: coordination.OwnerID("authority"), WriterFence: 4, MaxActiveReservations: max,
	}
}

func newTestClient(t *testing.T, store *memoryStore, max uint32) *Client {
	t.Helper()
	client := newUninitializedClient(t, store)
	head := testHead(max)
	value, _ := coordination.MarshalAllocatorHeadV1(head)
	store.put(client.headCoordinate(), value, int64(head.HeadGeneration), false)
	return client
}

func newUninitializedClient(t *testing.T, store *memoryStore) *Client {
	t.Helper()
	client, err := New(Config{
		Domain: coordination.DomainID("domain"), ControlVisibility: []byte("CONTROL"), Store: store,
		Clock:      func() time.Time { return time.Date(2026, 8, 27, 15, 0, 0, 0, time.UTC) },
		MaxRetries: 2, RetryBackoff: time.Nanosecond, MaxCheckpointAdvance: 16, MaxRetireBatch: 16,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func initializeOptions(max uint32) InitializeOptions {
	return InitializeOptions{
		HistoryFloor: 1, RetentionGeneration: 2,
		Authority: Authority{
			Generation: 3, Mode: coordination.WriterModeAccumuloPrimary,
			Holder: coordination.OwnerID("authority"), Fence: 4,
		},
		MaxActiveReservations: max,
	}
}

func TestEnsureInitializedAcceptedUnknownIdempotentAndConflict(t *testing.T) {
	for _, faults := range [][]faultMode{nil, {faultUnknownBefore, faultNone}, {faultUnknownAfter}} {
		store := newMemoryStore()
		client := newUninitializedClient(t, store)
		store.faults = faults
		head, err := client.EnsureInitialized(context.Background(), initializeOptions(8))
		if err != nil || head.HeadGeneration != 1 || head.NextEpoch != 1 {
			t.Fatalf("EnsureInitialized faults %v = %#v, %v", faults, head, err)
		}
		if len(store.captured[0].Conditions) != 1 || !store.captured[0].Conditions[0].Absent ||
			len(store.captured[0].Updates) != 1 || store.captured[0].Updates[0].Timestamp != 1 {
			t.Fatalf("initialization mutation = %#v", store.captured[0])
		}
		retry, err := client.Initialize(context.Background(), initializeOptions(8))
		if err != nil || !allocatorHeadEqual(retry, head) {
			t.Fatalf("idempotent Initialize = %#v, %v", retry, err)
		}
		conflict := initializeOptions(7)
		if _, err := client.EnsureInitialized(context.Background(), conflict); !errors.Is(err, ErrConflict) {
			t.Fatalf("conflicting initialization = %v", err)
		}
	}
}

func TestEnsureInitializedRejectsInvalidAndCorruptExisting(t *testing.T) {
	store := newMemoryStore()
	client := newUninitializedClient(t, store)
	options := initializeOptions(8)
	options.HistoryFloor = 2
	if _, err := client.EnsureInitialized(context.Background(), options); err == nil {
		t.Fatal("invalid initial history floor accepted")
	}
	head := testHead(8)
	value, _ := coordination.MarshalAllocatorHeadV1(head)
	store.put(client.headCoordinate(), value, 2, false)
	if _, err := client.EnsureInitialized(context.Background(), initializeOptions(8)); !errors.Is(err, ErrCorruption) {
		t.Fatalf("corrupt existing initialization = %v", err)
	}
}

func reserveRequest(head coordination.AllocatorHeadV1, txn string) ReserveRequest {
	return ReserveRequest{
		Predecessor: head, TXN: coordination.TXN(txn), Owner: coordination.OwnerID("worker"),
		LeaseUntil:          time.Date(2026, 8, 27, 16, 0, 0, 0, time.UTC),
		Authority:           Authority{Generation: 3, Mode: coordination.WriterModeAccumuloPrimary, Holder: coordination.OwnerID("authority"), Fence: 4},
		RetentionGeneration: 2,
	}
}

func TestReservationTakeoverAdvancesOwnerFenceAndGeneration(t *testing.T) {
	store := newMemoryStore()
	client := newTestClient(t, store, 4)
	head, err := client.CurrentHead(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := client.Reserve(context.Background(), reserveRequest(head, "takeover"))
	if err != nil {
		t.Fatal(err)
	}
	now := reservation.LeaseUntil.Add(time.Second)
	next, err := client.TakeoverReservation(
		context.Background(), reservation, coordination.OwnerID("recovery"),
		now.Add(time.Minute), reservation.Fence+1, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if next.ReservationGeneration != reservation.ReservationGeneration+1 ||
		next.Fence != reservation.Fence+1 || string(next.Owner) != "recovery" {
		t.Fatalf("takeover = %#v", next)
	}
	retry, err := client.TakeoverReservation(
		context.Background(), reservation, coordination.OwnerID("recovery"),
		now.Add(time.Minute), reservation.Fence+1, now,
	)
	if err != nil || !reservationEqual(retry, next) {
		t.Fatalf("ambiguous-style takeover retry = %#v, %v", retry, err)
	}
}

func TestReserveAcceptedUnknownAndIdempotentConflict(t *testing.T) {
	for _, fault := range []faultMode{faultNone, faultUnknownBefore, faultUnknownAfter} {
		t.Run(fmt.Sprint(fault), func(t *testing.T) {
			store := newMemoryStore()
			client := newTestClient(t, store, 4)
			if fault != faultNone {
				store.faults = []faultMode{fault, faultNone}
			}
			head, _ := client.CurrentHead(context.Background())
			reservation, err := client.Reserve(context.Background(), reserveRequest(head, "txn"))
			if err != nil || reservation.Epoch != 1 {
				t.Fatalf("Reserve = %#v, %v", reservation, err)
			}
			retry, err := client.Reserve(context.Background(), reserveRequest(head, "txn"))
			if err != nil || !reservationEqual(retry, reservation) {
				t.Fatalf("idempotent retry = %#v, %v", retry, err)
			}
			conflict := reserveRequest(head, "other")
			if _, err := client.Reserve(context.Background(), conflict); !errors.Is(err, ErrConflict) {
				t.Fatalf("conflicting retry error = %v", err)
			}
			if len(store.captured[0].Conditions) != 2 || len(store.captured[0].Updates) != 2 {
				t.Fatalf("allocation mutation shape = %#v", store.captured[0])
			}
		})
	}
}

func TestReserveRejectsAuthorityRetentionWindowAndExhaustion(t *testing.T) {
	store := newMemoryStore()
	client := newTestClient(t, store, 1)
	head, _ := client.CurrentHead(context.Background())
	tests := []func(*ReserveRequest){
		func(r *ReserveRequest) { r.Authority.Generation++ },
		func(r *ReserveRequest) { r.Authority.Fence++ },
		func(r *ReserveRequest) { r.Authority.Holder = coordination.OwnerID("other") },
		func(r *ReserveRequest) { r.Authority.Mode = coordination.WriterModeEmbeddedPrimary },
		func(r *ReserveRequest) { r.RetentionGeneration++ },
	}
	for i, mutate := range tests {
		request := reserveRequest(head, fmt.Sprint(i))
		mutate(&request)
		if _, err := client.Reserve(context.Background(), request); !errors.Is(err, ErrConflict) {
			t.Fatalf("stale request %d = %v", i, err)
		}
	}
	if _, err := client.Reserve(context.Background(), reserveRequest(head, "one")); err != nil {
		t.Fatal(err)
	}
	full, _ := client.CurrentHead(context.Background())
	if _, err := client.Reserve(context.Background(), reserveRequest(full, "two")); !errors.Is(err, ErrWindowFull) {
		t.Fatalf("window full = %v", err)
	}
	exhausted := testHead(2)
	exhausted.NextEpoch = coordination.Epoch(^uint64(0) >> 1)
	exhausted.RetiredThrough = exhausted.NextEpoch - 1
	exhausted.Frontier = exhausted.RetiredThrough
	exhausted.VisibleAt = time.Date(2026, 8, 27, 15, 0, 0, 0, time.UTC)
	exhausted.CheckpointDigest = coordination.Sum([]byte("checkpoint"))
	value, _ := coordination.MarshalAllocatorHeadV1(exhausted)
	store.put(client.headCoordinate(), value, int64(exhausted.HeadGeneration), false)
	if _, err := client.Reserve(context.Background(), reserveRequest(exhausted, "max")); !errors.Is(err, ErrExhausted) {
		t.Fatalf("exhaustion = %v", err)
	}

	generationExhausted := testHead(2)
	generationExhausted.HeadGeneration = coordination.Generation(^uint64(0) >> 1)
	value, _ = coordination.MarshalAllocatorHeadV1(generationExhausted)
	store = newMemoryStore()
	client = newTestClient(t, store, 2)
	store.cells = make(map[string][]storedCell)
	store.put(client.headCoordinate(), value, int64(generationExhausted.HeadGeneration), false)
	if _, err := client.Reserve(context.Background(), reserveRequest(generationExhausted, "generation-max")); !errors.Is(err, ErrOverflow) {
		t.Fatalf("head generation exhaustion = %v", err)
	}
}

func TestCurrentHeadRejectsRecordTimestampMismatch(t *testing.T) {
	store := newMemoryStore()
	client := newTestClient(t, store, 2)
	head := testHead(2)
	value, _ := coordination.MarshalAllocatorHeadV1(head)
	store.cells = make(map[string][]storedCell)
	store.put(client.headCoordinate(), value, int64(head.HeadGeneration)+1, false)
	if _, err := client.CurrentHead(context.Background()); !errors.Is(err, ErrCorruption) {
		t.Fatalf("timestamp mismatch = %v", err)
	}
}

func TestAllocationOneSidedUnknownIsCorruption(t *testing.T) {
	store := newMemoryStore()
	client := newTestClient(t, store, 4)
	store.faults = []faultMode{faultPartialFirst}
	head, _ := client.CurrentHead(context.Background())
	if _, err := client.Reserve(context.Background(), reserveRequest(head, "txn")); !errors.Is(err, ErrCorruption) {
		t.Fatalf("one-sided allocation = %v", err)
	}
}

func reserveOne(t *testing.T, client *Client, txn string) coordination.ReservationV1 {
	t.Helper()
	head, err := client.CurrentHead(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := client.Reserve(context.Background(), reserveRequest(head, txn))
	if err != nil {
		t.Fatal(err)
	}
	return reservation
}

func TestTerminalizeCrashBoundariesAndContradictoryOutcome(t *testing.T) {
	for _, faults := range [][]faultMode{
		nil,
		{faultUnknownBefore, faultNone, faultNone},
		{faultUnknownAfter, faultNone},
		{faultNone, faultUnknownBefore, faultNone},
		{faultNone, faultUnknownAfter},
	} {
		store := newMemoryStore()
		client := newTestClient(t, store, 8)
		reservation := reserveOne(t, client, "txn")
		store.faults = faults
		completion, outcome, err := client.Terminalize(context.Background(), reservation, coordination.StateCommitted)
		if err != nil || completion != CompletionOutcomeDurable {
			t.Fatalf("Terminalize faults %v = %v %#v %v", faults, completion, outcome, err)
		}
		retry, got, err := client.Terminalize(context.Background(), coordination.ReservationV1{
			ReservationGeneration: reservation.ReservationGeneration + 1,
			Epoch:                 reservation.Epoch, TXN: reservation.TXN, Owner: reservation.Owner,
			LeaseUntil: reservation.LeaseUntil, Fence: reservation.Fence,
			AuthorityGeneration: reservation.AuthorityGeneration, State: coordination.StateCommitted,
		}, coordination.StateCommitted)
		if err != nil || retry != CompletionOutcomeDurable || !outcomeEqual(got, outcome) {
			t.Fatalf("terminal retry = %v %#v %v", retry, got, err)
		}
	}

	store := newMemoryStore()
	client := newTestClient(t, store, 8)
	reservation := reserveOne(t, client, "txn")
	other, _ := coordination.NewEpochOutcomeV1(reservation.Epoch, coordination.TXN("other"), coordination.StateAborted, reservation.Fence, reservation.AuthorityGeneration)
	value, _ := coordination.MarshalEpochOutcomeV1(other)
	store.put(client.outcomeCoordinate(reservation.Epoch), value, int64(reservation.Epoch), false)
	if _, _, err := client.Terminalize(context.Background(), reservation, coordination.StateCommitted); !errors.Is(err, ErrCorruption) {
		t.Fatalf("contradictory outcome = %v", err)
	}
}

func TestTerminalReservationUsesNewGenerationAndRetainedVersions(t *testing.T) {
	store := newMemoryStore()
	client := newTestClient(t, store, 8)
	reservation := reserveOne(t, client, "txn")
	if reservation.ReservationGeneration != 1 {
		t.Fatalf("active reservation generation = %d", reservation.ReservationGeneration)
	}
	if _, _, err := client.Terminalize(context.Background(), reservation, coordination.StateAborted); err != nil {
		t.Fatal(err)
	}
	terminal, err := client.Reservation(context.Background(), reservation.Epoch)
	if err != nil || terminal.ReservationGeneration != 2 || terminal.State != coordination.StateAborted {
		t.Fatalf("terminal reservation = %#v, %v", terminal, err)
	}
	versions := store.cells[cellKey(client.reservationCoordinate(reservation.Epoch))]
	if len(versions) != 2 || versions[0].cell.Timestamp == versions[1].cell.Timestamp {
		t.Fatalf("retained reservation versions = %#v", versions)
	}
}

func TestTerminalizeRejectsReservationGenerationOverflow(t *testing.T) {
	store := newMemoryStore()
	client := newTestClient(t, store, 8)
	reservation := reserveOne(t, client, "txn")
	reservation.ReservationGeneration = coordination.Generation(^uint64(0) >> 1)
	value, _ := coordination.MarshalReservationV1(reservation)
	store.put(client.reservationCoordinate(reservation.Epoch), value, int64(reservation.ReservationGeneration), false)
	if _, _, err := client.Terminalize(context.Background(), reservation, coordination.StateAborted); !errors.Is(err, ErrOverflow) {
		t.Fatalf("reservation generation overflow = %v", err)
	}
}

func TestFrontierContiguousDigestMonotonicUnknownAndRetirement(t *testing.T) {
	store := newMemoryStore()
	client := newTestClient(t, store, 8)
	first := reserveOne(t, client, "one")
	second := reserveOne(t, client, "two")
	if _, _, err := client.Terminalize(context.Background(), second, coordination.StateAborted); err != nil {
		t.Fatal(err)
	}
	if _, err := client.AdvanceFrontier(context.Background()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("frontier skipped hole: %v", err)
	}
	_, firstOutcome, err := client.Terminalize(context.Background(), first, coordination.StateCommitted)
	if err != nil {
		t.Fatal(err)
	}
	secondOutcome, _ := client.Outcome(context.Background(), second.Epoch)
	wantDigest := OutcomesDigest([]coordination.EpochOutcomeV1{firstOutcome, secondOutcome})
	if wantDigest != OutcomesDigest([]coordination.EpochOutcomeV1{secondOutcome, firstOutcome}) {
		t.Fatal("outcomes digest is order-dependent")
	}
	store.faults = []faultMode{faultUnknownAfter}
	checkpoint, err := client.AdvanceFrontier(context.Background())
	if err != nil || checkpoint.Frontier != 2 || checkpoint.OutcomesDigest != wantDigest {
		t.Fatalf("AdvanceFrontier = %#v, %v", checkpoint, err)
	}
	latest, err := client.LatestCheckpoint(context.Background())
	if err != nil || !checkpointEqual(latest, checkpoint) {
		t.Fatalf("LatestCheckpoint = %#v, %v", latest, err)
	}
	next := reserveOne(t, client, "three")
	if _, _, err := client.Terminalize(context.Background(), next, coordination.StatePoisoned); err != nil {
		t.Fatal(err)
	}
	checkpoint2, err := client.AdvanceFrontier(context.Background())
	if err != nil || !checkpoint2.VisibleAt.After(checkpoint.VisibleAt) {
		t.Fatalf("monotonic checkpoint = %#v, %v", checkpoint2, err)
	}
	retired, err := client.Retire(context.Background())
	if err != nil || retired.RetiredThrough != 3 || retired.ActiveReservations != 0 {
		t.Fatalf("Retire = %#v, %v", retired, err)
	}
	for epoch := coordination.Epoch(1); epoch <= 3; epoch++ {
		if _, err := client.Reservation(context.Background(), epoch); !errors.Is(err, ErrNotFound) {
			t.Fatalf("reservation %d remains: %v", epoch, err)
		}
		if _, err := client.Outcome(context.Background(), epoch); err != nil {
			t.Fatalf("outcome %d retired: %v", epoch, err)
		}
	}
}

func TestHeadGenerationPreventsLowerFrontierAndRetirementShadowing(t *testing.T) {
	store := newMemoryStore()
	client := newTestClient(t, store, 8)
	reservations := make([]coordination.ReservationV1, 4)
	for i := range reservations {
		reservations[i] = reserveOne(t, client, fmt.Sprintf("txn-%d", i))
	}
	for i := 0; i < 2; i++ {
		if _, _, err := client.Terminalize(context.Background(), reservations[i], coordination.StateAborted); err != nil {
			t.Fatal(err)
		}
	}
	checkpoint, err := client.AdvanceFrontier(context.Background())
	if err != nil || checkpoint.Frontier != 2 {
		t.Fatalf("AdvanceFrontier = %#v, %v", checkpoint, err)
	}
	afterCheckpoint, err := client.CurrentHead(context.Background())
	if err != nil || afterCheckpoint.NextEpoch != 5 || afterCheckpoint.Frontier != 2 ||
		afterCheckpoint.HeadGeneration != 6 {
		t.Fatalf("head after lower frontier = %#v, %v", afterCheckpoint, err)
	}
	retired, err := client.Retire(context.Background())
	if err != nil || retired.NextEpoch != 5 || retired.Frontier != 2 ||
		retired.RetiredThrough != 2 || retired.HeadGeneration != 7 {
		t.Fatalf("head after retirement = %#v, %v", retired, err)
	}
	reread, err := client.CurrentHead(context.Background())
	if err != nil || !allocatorHeadEqual(reread, retired) {
		t.Fatalf("reread retired head = %#v, %v", reread, err)
	}
}

func TestCheckpointAndRetirementPartialUnknownCorruption(t *testing.T) {
	store := newMemoryStore()
	client := newTestClient(t, store, 8)
	reservation := reserveOne(t, client, "txn")
	if _, _, err := client.Terminalize(context.Background(), reservation, coordination.StateCommitted); err != nil {
		t.Fatal(err)
	}
	store.faults = []faultMode{faultPartialFirst}
	if _, err := client.AdvanceFrontier(context.Background()); !errors.Is(err, ErrCorruption) {
		t.Fatalf("partial checkpoint = %v", err)
	}

	store = newMemoryStore()
	client = newTestClient(t, store, 8)
	reservation = reserveOne(t, client, "txn")
	if _, _, err := client.Terminalize(context.Background(), reservation, coordination.StateCommitted); err != nil {
		t.Fatal(err)
	}
	if _, err := client.AdvanceFrontier(context.Background()); err != nil {
		t.Fatal(err)
	}
	store.faults = []faultMode{faultPartialFirst}
	if _, err := client.Retire(context.Background()); !errors.Is(err, ErrCorruption) {
		t.Fatalf("partial retirement = %v", err)
	}
}

func TestCheckpointAndRetirementHonorBatchBounds(t *testing.T) {
	store := newMemoryStore()
	client := newTestClient(t, store, 8)
	client.maxCheckpointAdvance = 2
	client.maxRetireBatch = 1
	for i := 0; i < 3; i++ {
		reservation := reserveOne(t, client, fmt.Sprintf("txn-%d", i))
		if _, _, err := client.Terminalize(context.Background(), reservation, coordination.StateAborted); err != nil {
			t.Fatal(err)
		}
	}
	first, err := client.AdvanceFrontier(context.Background())
	if err != nil || first.Frontier != 2 {
		t.Fatalf("first bounded checkpoint = %#v, %v", first, err)
	}
	second, err := client.AdvanceFrontier(context.Background())
	if err != nil || second.Frontier != 3 {
		t.Fatalf("second bounded checkpoint = %#v, %v", second, err)
	}
	for epoch := coordination.Epoch(1); epoch <= 3; epoch++ {
		head, err := client.Retire(context.Background())
		if err != nil || head.RetiredThrough != epoch {
			t.Fatalf("bounded retirement %d = %#v, %v", epoch, head, err)
		}
	}
}

func TestConcurrentReservationsAreUniqueAndContiguous(t *testing.T) {
	store := newMemoryStore()
	client := newTestClient(t, store, 128)
	const count = 64
	var failures atomic.Int32
	epochs := make(chan coordination.Epoch, count)
	var wait sync.WaitGroup
	for i := 0; i < count; i++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			for attempt := 0; attempt < 1000; attempt++ {
				head, err := client.CurrentHead(context.Background())
				if err != nil {
					continue
				}
				reservation, err := client.Reserve(context.Background(), reserveRequest(head, fmt.Sprintf("txn-%d", index)))
				if err == nil {
					epochs <- reservation.Epoch
					return
				}
				if !errors.Is(err, ErrConflict) {
					failures.Add(1)
					return
				}
			}
			failures.Add(1)
		}(i)
	}
	wait.Wait()
	close(epochs)
	if failures.Load() != 0 {
		t.Fatalf("%d allocations failed", failures.Load())
	}
	seen := make(map[coordination.Epoch]bool, count)
	for epoch := range epochs {
		if seen[epoch] {
			t.Fatalf("duplicate epoch %d", epoch)
		}
		seen[epoch] = true
	}
	for epoch := coordination.Epoch(1); epoch <= count; epoch++ {
		if !seen[epoch] {
			t.Fatalf("missing epoch %d", epoch)
		}
	}
}
