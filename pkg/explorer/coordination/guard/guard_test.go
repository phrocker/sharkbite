package guard

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
)

type fault uint8

const (
	noFault fault = iota
	unknownBefore
	unknownAfter
	partialFirst
	reject
)

type memoryStore struct {
	mu       sync.Mutex
	cells    map[string][]version
	faults   []fault
	captured []allocator.Mutation
}

type version struct {
	cell   allocator.Cell
	delete bool
}

func newMemoryStore() *memoryStore {
	return &memoryStore{cells: make(map[string][]version)}
}

func coordinateKey(c allocator.Coordinate) string {
	return string(c.Row) + "\x00" + string(c.Family) + "\x00" +
		string(c.Qualifier) + "\x00" + string(c.Visibility)
}

func (s *memoryStore) ReadExact(
	ctx context.Context,
	coordinates []allocator.Coordinate,
) ([]allocator.Cell, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]allocator.Cell, 0, len(coordinates))
	for _, coordinate := range coordinates {
		if value, ok := s.visible(coordinate); ok {
			result = append(result, cloneCell(value))
		}
	}
	return result, nil
}

func (s *memoryStore) CompareAndMutate(
	ctx context.Context,
	mutation allocator.Mutation,
) (allocator.Status, error) {
	if err := ctx.Err(); err != nil {
		return allocator.StatusUnknown, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.captured = append(s.captured, cloneMutation(mutation))
	currentFault := noFault
	if len(s.faults) != 0 {
		currentFault, s.faults = s.faults[0], s.faults[1:]
	}
	if currentFault == unknownBefore {
		return allocator.StatusUnknown, allocator.ErrConditionalUnknown
	}
	for _, condition := range mutation.Conditions {
		cell, found := s.visible(condition.Coordinate)
		if condition.Absent {
			if found {
				return allocator.StatusRejected, nil
			}
			continue
		}
		if !found || !bytes.Equal(cell.Value, condition.Value) ||
			condition.TimestampSet && cell.Timestamp != condition.Timestamp {
			return allocator.StatusRejected, nil
		}
	}
	if currentFault == reject {
		return allocator.StatusRejected, nil
	}
	updates := mutation.Updates
	if currentFault == partialFirst {
		updates = updates[:1]
	}
	for _, update := range updates {
		s.put(update)
	}
	if currentFault == unknownAfter || currentFault == partialFirst {
		return allocator.StatusUnknown, allocator.ErrConditionalUnknown
	}
	return allocator.StatusAccepted, nil
}

func (s *memoryStore) put(update allocator.Update) {
	key := coordinateKey(update.Coordinate)
	for _, existing := range s.cells[key] {
		if existing.cell.Timestamp == update.Timestamp &&
			(existing.delete != update.Delete || !bytes.Equal(existing.cell.Value, update.Value)) {
			panic("different values at one timestamp")
		}
	}
	s.cells[key] = append(s.cells[key], version{
		cell: allocator.Cell{
			Coordinate: cloneCoordinate(update.Coordinate),
			Value:      append([]byte(nil), update.Value...), Timestamp: update.Timestamp,
		},
		delete: update.Delete,
	})
}

func (s *memoryStore) visible(coordinate allocator.Coordinate) (allocator.Cell, bool) {
	versions := s.cells[coordinateKey(coordinate)]
	if len(versions) == 0 {
		return allocator.Cell{}, false
	}
	selected := versions[0]
	for _, candidate := range versions[1:] {
		if candidate.cell.Timestamp > selected.cell.Timestamp {
			selected = candidate
		}
	}
	if selected.delete {
		return allocator.Cell{}, false
	}
	return selected.cell, true
}

func cloneCoordinate(c allocator.Coordinate) allocator.Coordinate {
	return allocator.Coordinate{
		Row: append([]byte(nil), c.Row...), Family: append([]byte(nil), c.Family...),
		Qualifier:  append([]byte(nil), c.Qualifier...),
		Visibility: append([]byte(nil), c.Visibility...),
	}
}

func cloneCell(c allocator.Cell) allocator.Cell {
	return allocator.Cell{
		Coordinate: cloneCoordinate(c.Coordinate),
		Value:      append([]byte(nil), c.Value...), Timestamp: c.Timestamp,
	}
}

func cloneMutation(m allocator.Mutation) allocator.Mutation {
	result := allocator.Mutation{Row: append([]byte(nil), m.Row...)}
	for _, condition := range m.Conditions {
		result.Conditions = append(result.Conditions, allocator.Condition{
			Coordinate: cloneCoordinate(condition.Coordinate),
			Value:      append([]byte(nil), condition.Value...), Absent: condition.Absent,
			Timestamp: condition.Timestamp, TimestampSet: condition.TimestampSet,
		})
	}
	for _, update := range m.Updates {
		result.Updates = append(result.Updates, allocator.Update{
			Coordinate: cloneCoordinate(update.Coordinate),
			Value:      append([]byte(nil), update.Value...), Delete: update.Delete,
			Timestamp: update.Timestamp,
		})
	}
	return result
}

type fixedAuthority struct {
	value Authority
	err   error
}

func (a *fixedAuthority) Current(context.Context, coordination.DomainID) (Authority, error) {
	return a.value, a.err
}

type fixedRetirement struct {
	retired    bool
	generation coordination.Generation
	err        error
}

func (r *fixedRetirement) Retired(
	context.Context,
	coordination.DomainID,
	Entity,
) (bool, coordination.Generation, error) {
	return r.retired, r.generation, r.err
}

type fixedTransactions struct {
	status TxnDisposition
}

func (s *fixedTransactions) Status(
	context.Context,
	coordination.DomainID,
	coordination.TXN,
) (TxnDisposition, error) {
	return s.status, nil
}

type reconcileCounter struct {
	count int
}

func (r *reconcileCounter) ReconcileCommitted(
	context.Context,
	coordination.DomainID,
	Entity,
	Pending,
) error {
	r.count++
	return nil
}

var testNow = time.Date(2026, 8, 27, 16, 0, 0, 0, time.UTC)

func digest(value string) coordination.Digest {
	return coordination.Sum([]byte(value))
}

func testIntent(id, txn, owner string, mode Mode) Intent {
	return Intent{
		Entity: Entity{Kind: 2, ID: coordination.EntityID(id)},
		TXN:    coordination.TXN(txn), Owner: coordination.OwnerID(owner),
		LeaseUntil: testNow.Add(time.Minute), Fence: 7,
		AuthorityGeneration: 3, AuthorityFence: 5,
		RetentionGeneration: 4, RetirementGeneration: 4, HistoryFloor: 1, Mode: mode,
		DesiredState: StateLive, DesiredWinnerID: []byte(id + "-winner"),
		DesiredDigest: digest(id + "-logical"), LPART: coordination.LPART(id + "-lpart"),
		LogicalPolicyID: []byte(id + "-policy"), PhysicalDigest: digest(id + "-physical"),
	}
}

func newClient(t *testing.T, store *memoryStore) (*Client, *fixedAuthority, *fixedRetirement) {
	t.Helper()
	authority := &fixedAuthority{value: Authority{
		Generation: 3, Fence: 5, RetentionGeneration: 4, HistoryFloor: 1,
	}}
	retirement := &fixedRetirement{generation: 4}
	client, err := New(Config{
		Domain: coordination.DomainID("domain"), ControlVisibility: []byte("CONTROL"),
		Store: store, Authority: authority, Retirement: retirement,
		Transactions: &fixedTransactions{status: TxnCommitted},
		Clock:        func() time.Time { return testNow }, MaxRetries: 2, RetryBackoff: time.Nanosecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client, authority, retirement
}

func published(intent Intent, epoch coordination.Epoch) Published {
	return Published{
		TXN: intent.TXN, Epoch: epoch, Fence: intent.Fence,
		AuthorityGeneration: intent.AuthorityGeneration,
		LogicalDigest:       intent.DesiredDigest, LPART: intent.LPART,
		LogicalPolicyID: intent.LogicalPolicyID, State: intent.DesiredState,
		WinnerID: intent.DesiredWinnerID,
	}
}

func commitAcquisition(t *testing.T, client *Client, acquisition Acquisition, epoch coordination.Epoch) Head {
	t.Helper()
	value := published(acquisition.Pending.Intent, epoch)
	prepared, err := client.Prepare(context.Background(), acquisition.Pending, value, testNow.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	head, err := client.Commit(context.Background(), prepared, value, testNow.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return head
}

func TestCodecsDeterministicAndRejectCorruption(t *testing.T) {
	intent := testIntent("node", "txn", "owner", ModeAbsentOrIdentical)
	pending := Pending{
		Generation: 1, UpdatedAt: testNow, Active: true,
		Decision: DecisionCreate, Intent: intent,
	}
	first, err := MarshalPending(pending)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := MarshalPending(pending)
	if !bytes.Equal(first, second) {
		t.Fatal("pending encoding is not deterministic")
	}
	if len(first) != 312 ||
		coordination.Sum(first).String() != "ac1858f3b8776655223c174cb1c0dde15aadd3cc71be793097bd143e93033973" {
		t.Fatalf("pending codec golden changed: length=%d sha256=%s", len(first), coordination.Sum(first))
	}
	decoded, err := UnmarshalPending(first)
	if err != nil || !pendingEqual(decoded, pending) {
		t.Fatalf("pending round trip failed: %v", err)
	}
	corrupt := append([]byte(nil), first...)
	corrupt[len(corrupt)/2] ^= 0x80
	if _, err := UnmarshalPending(corrupt); err == nil {
		t.Fatal("corrupt pending accepted")
	}
	head := Head{
		Generation: 2, UpdatedAt: testNow, State: StateLive,
		WinnerID: []byte("winner"), Epoch: 9, TXN: coordination.TXN("txn"),
		LogicalDigest: digest("logical"), LPART: coordination.LPART("lpart"),
		LogicalPolicyID: []byte("policy"), RetirementGeneration: 4,
	}
	headBytes, err := MarshalHead(head)
	if err != nil {
		t.Fatal(err)
	}
	if len(headBytes) != 147 ||
		coordination.Sum(headBytes).String() != "6d6a875b2b2f7e63780ebe807042e541c15203ca71a8b74f2d7bbe4f34bfb25d" {
		t.Fatalf("head codec golden changed: length=%d sha256=%s", len(headBytes), coordination.Sum(headBytes))
	}
	if got, err := UnmarshalHead(headBytes); err != nil || !headEqual(got, head) {
		t.Fatalf("head round trip failed: %v", err)
	}
}

func TestGuardMutationUsesTrustedExactCoordinatesAndCopies(t *testing.T) {
	store := newMemoryStore()
	client, _, _ := newClient(t, store)
	intent := testIntent("opaque\x00id", "txn", "owner", ModeAppend)
	expectedRow, err := coordination.EntityHeadRow(
		coordination.DomainID("domain"), intent.Entity.Kind, intent.Entity.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	acquisition, err := client.Acquire(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	intent.Entity.ID[0] ^= 0xff
	intent.Owner[0] ^= 0xff
	if len(store.captured) != 1 {
		t.Fatalf("captured mutations = %d", len(store.captured))
	}
	mutation := store.captured[0]
	if !bytes.Equal(mutation.Row, expectedRow) || len(mutation.Conditions) != 2 ||
		len(mutation.Updates) != 1 {
		t.Fatalf("unexpected mutation shape: %#v", mutation)
	}
	update := mutation.Updates[0]
	if string(update.Coordinate.Family) != "s" ||
		string(update.Coordinate.Qualifier) != "pending" ||
		string(update.Coordinate.Visibility) != "CONTROL" ||
		update.Timestamp != int64(acquisition.Pending.Generation) ||
		!bytes.Equal(update.Coordinate.Row, expectedRow) {
		t.Fatalf("unexpected pending cell mapping: %#v", update)
	}
	decoded, err := UnmarshalPending(update.Value)
	if err != nil || string(decoded.Intent.Entity.ID) != "opaque\x00id" ||
		string(decoded.Intent.Owner) != "owner" {
		t.Fatalf("mutation did not defensively copy intent: %#v, %v", decoded, err)
	}
}

func TestAtomicCreateReuseAppendAndConflicts(t *testing.T) {
	store := newMemoryStore()
	client, _, _ := newClient(t, store)
	ctx := context.Background()
	createIntent := testIntent("shared", "txn-create", "owner-a", ModeAbsentOrIdentical)

	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, owner := range []string{"owner-a", "owner-b"} {
		wg.Add(1)
		go func(owner string) {
			defer wg.Done()
			intent := createIntent
			intent.Owner = coordination.OwnerID(owner)
			_, err := client.Acquire(ctx, intent)
			results <- err
		}(owner)
	}
	wg.Wait()
	close(results)
	success, busy := 0, 0
	for err := range results {
		switch {
		case err == nil:
			success++
		case errors.Is(err, ErrBusy):
			busy++
		default:
			t.Fatalf("unexpected concurrent result: %v", err)
		}
	}
	if success != 1 || busy != 1 {
		t.Fatalf("create contention = success %d busy %d", success, busy)
	}
	_, pending, err := client.Read(ctx, createIntent.Entity)
	if err != nil || pending == nil {
		t.Fatal("winning pending guard missing")
	}
	head := commitAcquisition(t, client, Acquisition{Pending: *pending}, 11)
	if head.Epoch != 11 {
		t.Fatal("create head was not committed")
	}

	reuseIntent := testIntent("shared", "txn-reuse", "owner-c", ModeAbsentOrIdentical)
	reuse, err := client.Acquire(ctx, reuseIntent)
	if err != nil || reuse.Decision != DecisionReuse {
		t.Fatalf("reuse = %#v, %v", reuse, err)
	}
	reuseHead := commitAcquisition(t, client, reuse, 12)
	if !headEqual(reuseHead, head) {
		t.Fatal("reuse rewrote canonical head")
	}

	different := testIntent("shared", "txn-different", "owner-d", ModeAbsentOrIdentical)
	different.DesiredDigest = digest("different")
	if _, err := client.Acquire(ctx, different); !errors.Is(err, ErrConflict) {
		t.Fatalf("different bytes = %v", err)
	}

	appendIntent := testIntent("document", "txn-append", "owner", ModeAppend)
	appendAcquisition, err := client.Acquire(ctx, appendIntent)
	if err != nil || appendAcquisition.Decision != DecisionAppend {
		t.Fatalf("append = %#v, %v", appendAcquisition, err)
	}
	_ = commitAcquisition(t, client, appendAcquisition, 13)

	mutate := testIntent("document", "txn-mutate", "owner", ModeMutate)
	mutate.ExpectedEpoch, mutate.ExpectedDigest = 13, appendIntent.DesiredDigest
	mutate.DesiredDigest = digest("document-v2")
	acquired, err := client.Acquire(ctx, mutate)
	if err != nil || acquired.Decision != DecisionMutate {
		t.Fatalf("mutate = %#v, %v", acquired, err)
	}
	_ = commitAcquisition(t, client, acquired, 14)

	retire := testIntent("document", "txn-retire", "owner", ModeRetire)
	retire.ExpectedEpoch, retire.ExpectedDigest = 14, mutate.DesiredDigest
	retire.DesiredState, retire.DesiredDigest = StateTombstone, digest("document-tombstone")
	retiredAcquisition, err := client.Acquire(ctx, retire)
	if err != nil || retiredAcquisition.Decision != DecisionRetire {
		t.Fatalf("retire = %#v, %v", retiredAcquisition, err)
	}
	_ = commitAcquisition(t, client, retiredAcquisition, 15)
	appendAfterTombstone := testIntent("document", "txn-late", "owner", ModeAppend)
	if _, err := client.Acquire(ctx, appendAfterTombstone); !errors.Is(err, ErrConflict) {
		t.Fatalf("append after tombstone = %v", err)
	}
}

func TestReuseCommitIsIdempotentAndRejectsChangedHead(t *testing.T) {
	store := newMemoryStore()
	client, _, _ := newClient(t, store)
	ctx := context.Background()

	createIntent := testIntent("reuse-retry", "txn-create", "owner-create", ModeAbsentOrIdentical)
	create, err := client.Acquire(ctx, createIntent)
	if err != nil {
		t.Fatal(err)
	}
	originalHead := commitAcquisition(t, client, create, 40)

	reuseIntent := testIntent("reuse-retry", "txn-reuse-one", "owner-reuse-one", ModeAbsentOrIdentical)
	reuse, err := client.Acquire(ctx, reuseIntent)
	if err != nil || reuse.Decision != DecisionReuse {
		t.Fatalf("first reuse acquisition = %#v, %v", reuse, err)
	}
	reusePublished := published(reuse.Pending.Intent, 41)
	prepared, err := client.Prepare(ctx, reuse.Pending, reusePublished, testNow.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	first, err := client.Commit(ctx, prepared, reusePublished, testNow.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.Commit(ctx, prepared, reusePublished, testNow.Add(3*time.Second))
	if err != nil {
		t.Fatalf("second reuse commit failed: %v", err)
	}
	if !headEqual(first, originalHead) || !headEqual(second, originalHead) {
		t.Fatal("repeated reuse commit did not return the preserved canonical head")
	}

	reuseAgainIntent := testIntent("reuse-retry", "txn-reuse-two", "owner-reuse-two", ModeAbsentOrIdentical)
	reuseAgain, err := client.Acquire(ctx, reuseAgainIntent)
	if err != nil || reuseAgain.Decision != DecisionReuse {
		t.Fatalf("second reuse acquisition = %#v, %v", reuseAgain, err)
	}
	reuseAgainPublished := published(reuseAgain.Pending.Intent, 42)
	preparedAgain, err := client.Prepare(
		ctx, reuseAgain.Pending, reuseAgainPublished, testNow.Add(4*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	store.faults = []fault{unknownAfter}
	unknownResult, err := client.Commit(
		ctx, preparedAgain, reuseAgainPublished, testNow.Add(5*time.Second),
	)
	if err != nil {
		t.Fatalf("unknown-after reuse commit did not reconcile: %v", err)
	}
	if !headEqual(unknownResult, originalHead) {
		t.Fatal("unknown-after reuse commit did not return the preserved head")
	}

	changed := cloneHead(originalHead)
	changed.Generation++
	changed.UpdatedAt = testNow.Add(6 * time.Second)
	changed.Epoch++
	changed.TXN = coordination.TXN("different-canonical-txn")
	changed.LogicalDigest = digest("changed-logical-state")
	row, _ := coordination.EntityHeadRow(
		coordination.DomainID("domain"),
		reuseAgain.Pending.Intent.Entity.Kind,
		reuseAgain.Pending.Intent.Entity.ID,
	)
	headBytes, err := MarshalHead(changed)
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.put(allocator.Update{
		Coordinate: client.coordinate(row, qualifierHead),
		Value:      headBytes, Timestamp: int64(changed.Generation),
	})
	store.mu.Unlock()
	if _, err := client.Commit(
		ctx, preparedAgain, reuseAgainPublished, testNow.Add(7*time.Second),
	); !errors.Is(err, ErrConflict) && !errors.Is(err, ErrCorruption) {
		t.Fatalf("changed head accepted by reuse retry: %v", err)
	}
}

func TestAcquireManyOrdersDeduplicatesAndRollsBack(t *testing.T) {
	store := newMemoryStore()
	client, _, _ := newClient(t, store)
	first := testIntent("z", "txn-z", "owner", ModeAbsentOrIdentical)
	second := testIntent("a", "txn-a", "owner", ModeAbsentOrIdentical)
	results, err := client.AcquireMany(context.Background(), []Intent{first, second, first})
	if err != nil || len(results) != 2 {
		t.Fatalf("acquire many = %#v, %v", results, err)
	}
	if bytes.Compare(store.captured[0].Row, store.captured[1].Row) >= 0 {
		t.Fatal("guards were not acquired in binary row order")
	}
	for _, result := range results {
		if err := client.Abort(context.Background(), result.Pending, false); err != nil {
			t.Fatal(err)
		}
	}

	low := testIntent("rollback-a", "txn-low", "owner", ModeAbsentOrIdentical)
	high := testIntent("rollback-b", "txn-high", "owner", ModeAbsentOrIdentical)
	lowRow, _ := coordination.EntityHeadRow(coordination.DomainID("domain"), low.Entity.Kind, low.Entity.ID)
	highRow, _ := coordination.EntityHeadRow(coordination.DomainID("domain"), high.Entity.Kind, high.Entity.ID)
	if bytes.Compare(lowRow, highRow) > 0 {
		low, high = high, low
	}
	created, err := client.Acquire(context.Background(), high)
	if err != nil {
		t.Fatal(err)
	}
	_ = commitAcquisition(t, client, created, 20)
	conflict := high
	conflict.TXN, conflict.Owner, conflict.DesiredDigest =
		coordination.TXN("conflict"), coordination.OwnerID("other"), digest("different")
	if _, err := client.AcquireMany(context.Background(), []Intent{conflict, low}); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected definite conflict, got %v", err)
	}
	_, pending, err := client.Read(context.Background(), low.Entity)
	if err != nil || pending == nil || pending.Active {
		t.Fatalf("earlier guard was not rolled back: %#v, %v", pending, err)
	}
}

func TestRenewTakeoverAndTerminalOwnerHandling(t *testing.T) {
	store := newMemoryStore()
	client, _, _ := newClient(t, store)
	client.transactions = &fixedTransactions{status: TxnNonterminal}
	intent := testIntent("takeover", "txn", "owner-a", ModeAppend)
	acquisition, err := client.Acquire(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	renewed, err := client.Renew(
		context.Background(), acquisition.Pending,
		intent.LeaseUntil.Add(time.Minute), testNow.Add(time.Second),
	)
	if err != nil || renewed.Generation != acquisition.Pending.Generation+1 {
		t.Fatalf("renew = %#v, %v", renewed, err)
	}
	if _, err := client.Renew(context.Background(), renewed, renewed.Intent.LeaseUntil, testNow.Add(2*time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatalf("nonextending renewal = %v", err)
	}
	expiredAt := renewed.Intent.LeaseUntil
	takeover, err := client.Takeover(
		context.Background(), renewed, coordination.OwnerID("owner-b"),
		expiredAt.Add(time.Minute), renewed.Intent.Fence+1, expiredAt,
	)
	if err != nil || !takeover.TakenOver || takeover.Pending.Generation != renewed.Generation+1 {
		t.Fatalf("takeover = %#v, %v", takeover, err)
	}
	if _, err := client.Takeover(
		context.Background(), takeover.Pending, coordination.OwnerID("owner-c"),
		takeover.Pending.Intent.LeaseUntil, takeover.Pending.Intent.Fence+1,
		takeover.Pending.Intent.LeaseUntil,
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("expired replacement lease = %v", err)
	}
	if _, err := client.Renew(
		context.Background(), renewed, expiredAt.Add(2*time.Minute), expiredAt,
	); !errors.Is(err, ErrExpired) && !errors.Is(err, ErrConflict) {
		t.Fatalf("stale owner renewal = %v", err)
	}

	client.transactions = &fixedTransactions{status: TxnCommitted}
	reconciler := &reconcileCounter{}
	client.reconciler = reconciler
	takeover.Pending.Intent.LeaseUntil = expiredAt
	result, err := client.Takeover(
		context.Background(), takeover.Pending, coordination.OwnerID("owner-c"),
		expiredAt.Add(time.Minute), takeover.Pending.Intent.Fence+1, expiredAt,
	)
	if err != nil || !result.Reconciled || reconciler.count != 1 {
		t.Fatalf("committed owner reconciliation = %#v, %v", result, err)
	}
}

func TestUnknownReadbackAndStaleFences(t *testing.T) {
	store := newMemoryStore()
	client, authority, retirement := newClient(t, store)
	store.faults = []fault{unknownBefore}
	intent := testIntent("unknown-before", "txn", "owner", ModeAppend)
	if _, err := client.Acquire(context.Background(), intent); err != nil {
		t.Fatalf("unknown-before retry failed: %v", err)
	}
	store.faults = []fault{unknownAfter}
	after := testIntent("unknown-after", "txn", "owner", ModeAppend)
	acquisition, err := client.Acquire(context.Background(), after)
	if err != nil {
		t.Fatalf("unknown-after readback failed: %v", err)
	}
	publishedValue := published(after, 30)
	store.faults = []fault{unknownBefore}
	prepared, err := client.Prepare(context.Background(), acquisition.Pending, publishedValue, testNow.Add(time.Second))
	if err != nil {
		t.Fatalf("prepare unknown-before retry failed: %v", err)
	}
	store.faults = []fault{partialFirst}
	if _, err := client.Commit(context.Background(), prepared, publishedValue, testNow.Add(2*time.Second)); !errors.Is(err, ErrCorruption) {
		t.Fatalf("mixed commit result = %v", err)
	}

	staleAuthority := testIntent("stale-authority", "txn", "owner", ModeAppend)
	authority.value.Generation++
	if _, err := client.Acquire(context.Background(), staleAuthority); !errors.Is(err, ErrStaleAuthority) {
		t.Fatalf("stale authority = %v", err)
	}
	authority.value.Generation--
	finalizeIntent := testIntent("stale-finalize", "txn", "owner", ModeAppend)
	finalizeAcquisition, err := client.Acquire(context.Background(), finalizeIntent)
	if err != nil {
		t.Fatal(err)
	}
	authority.value.RetentionGeneration++
	if _, err := client.Prepare(
		context.Background(), finalizeAcquisition.Pending, published(finalizeIntent, 31),
		testNow.Add(time.Second),
	); !errors.Is(err, ErrStaleRetention) {
		t.Fatalf("stale finalization retention = %v", err)
	}
	authority.value.RetentionGeneration--
	staleRetention := testIntent("stale-retention", "txn", "owner", ModeAppend)
	retirement.generation++
	if _, err := client.Acquire(context.Background(), staleRetention); !errors.Is(err, ErrStaleRetention) {
		t.Fatalf("stale retirement generation = %v", err)
	}
	retirement.generation--
	authority.value.HistoryFloor = 10
	staleFloor := testIntent("stale-floor", "txn", "owner", ModeMutate)
	staleFloor.ExpectedEpoch, staleFloor.ExpectedDigest = 2, digest("old")
	if _, err := client.Acquire(context.Background(), staleFloor); !errors.Is(err, ErrStaleRetention) {
		t.Fatalf("stale history floor = %v", err)
	}
}
