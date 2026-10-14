package accumulo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/phrocker/shoal-oss/internal/zk"
)

// constraintConnector wires a connector whose table properties and property
// mutations are served by one fake manager, which is what constraint
// administration reads and writes.
func constraintConnector(t *testing.T, properties map[string]string) (*Connector, *fakeManagerAdapter) {
	t.Helper()
	names := &fakeTableNames{byName: map[string]string{"events": "1"}}
	connector := testConnectorWithDiscovery(t, &fakeTabletWalker{}, names)
	manager := &fakeManagerAdapter{configuration: properties}
	connector.manager = manager
	connector.managerAddr = fakeManagerAddress{address: "manager:9997"}
	connector.clientAddr = fakeClientServiceAddresses{addresses: []string{"tserver:9997"}}
	return connector, manager
}

func TestAddConstraintAssignsTheLowestFreeNumber(t *testing.T) {
	connector, manager := constraintConnector(t, map[string]string{
		"table.constraint.1":    "org.example.First",
		"table.constraint.3":    "org.example.Third",
		"table.split.threshold": "1G",
	})

	number, err := connector.AddConstraint(context.Background(), "events", "org.example.Second")
	if err != nil {
		t.Fatal(err)
	}
	if number != 2 {
		t.Fatalf("assigned number = %d, want the lowest free number 2", number)
	}

	manager.mu.Lock()
	defer manager.mu.Unlock()
	if len(manager.modifyRequests) != 1 {
		t.Fatalf("versioned writes = %d, want 1", len(manager.modifyRequests))
	}
	written := manager.modifyRequests[0]
	if written.Version != 0 {
		t.Fatalf("write presented version %d, want the version it read", written.Version)
	}
	if got := written.Properties["table.constraint.2"]; got != "org.example.Second" {
		t.Fatalf("written constraint = %q", got)
	}
	for _, kept := range []string{"table.constraint.1", "table.constraint.3", "table.split.threshold"} {
		if _, present := written.Properties[kept]; !present {
			t.Fatalf("versioned write dropped %q: %#v", kept, written.Properties)
		}
	}
}

func TestAddConstraintStartsAtOneAndIsIdempotent(t *testing.T) {
	connector, manager := constraintConnector(t, map[string]string{})
	number, err := connector.AddConstraint(context.Background(), "events", "org.example.Only")
	if err != nil {
		t.Fatal(err)
	}
	if number != 1 {
		t.Fatalf("first constraint number = %d, want 1", number)
	}

	// Reinstalling the same class must not add a second number: the same check
	// would then run twice on every mutation.
	connector, manager = constraintConnector(t, map[string]string{
		"table.constraint.7": "org.example.Only",
	})
	number, err = connector.AddConstraint(context.Background(), "events", "org.example.Only")
	if err != nil {
		t.Fatal(err)
	}
	if number != 7 {
		t.Fatalf("existing constraint number = %d, want 7", number)
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if len(manager.modifyRequests) != 0 {
		t.Fatalf("versioned writes = %d, want none for an installed class", len(manager.modifyRequests))
	}
}

func TestListConstraintsOrdersByNumberAndIgnoresNonConstraints(t *testing.T) {
	connector, _ := constraintConnector(t, map[string]string{
		"table.constraint.10":      "org.example.Ten",
		"table.constraint.2":       "org.example.Two",
		"table.constraint.0":       "org.example.Zero",
		"table.constraint.-1":      "org.example.Negative",
		"table.constraint.notanum": "org.example.NotANumber",
		"table.constraint.4":       "",
		"table.majc.maxopen":       "10",
	})

	constraints, err := connector.ListConstraints(context.Background(), "events")
	if err != nil {
		t.Fatal(err)
	}
	if len(constraints) != 2 {
		t.Fatalf("constraints = %#v, want the two numbered entries", constraints)
	}
	if constraints[0].Number != 2 || constraints[0].ClassName != "org.example.Two" {
		t.Fatalf("first constraint = %#v", constraints[0])
	}
	if constraints[1].Number != 10 || constraints[1].ClassName != "org.example.Ten" {
		t.Fatalf("second constraint = %#v", constraints[1])
	}
}

func TestRemoveConstraintRemovesTheNumberedProperty(t *testing.T) {
	connector, manager := constraintConnector(t, map[string]string{
		"table.constraint.5": "org.example.Fifth",
	})
	if err := connector.RemoveConstraint(context.Background(), "events", 5); err != nil {
		t.Fatal(err)
	}
	// Removing a number the table does not carry is the state the caller asked
	// for, so it is not an error.
	if err := connector.RemoveConstraint(context.Background(), "events", 9); err != nil {
		t.Fatalf("removing an absent constraint = %v, want nil", err)
	}

	manager.mu.Lock()
	defer manager.mu.Unlock()
	if len(manager.propertyRequests) != 2 {
		t.Fatalf("property requests = %d, want 2", len(manager.propertyRequests))
	}
	for i, want := range []string{"table.constraint.5", "table.constraint.9"} {
		request := manager.propertyRequests[i]
		if !request.remove || request.property != want {
			t.Fatalf("request %d = %#v, want a removal of %q", i, request, want)
		}
	}
}

func TestConstraintOperationsValidateTheirArguments(t *testing.T) {
	connector, manager := constraintConnector(t, map[string]string{})

	if _, err := connector.AddConstraint(context.Background(), "", "org.example.C"); !errors.Is(err, ErrInvalidTableName) {
		t.Fatalf("empty table = %v, want ErrInvalidTableName", err)
	}
	if _, err := connector.AddConstraint(context.Background(), "events", ""); !errors.Is(err, ErrInvalidProperty) {
		t.Fatalf("empty class = %v, want ErrInvalidProperty", err)
	}
	if _, err := connector.AddConstraint(context.Background(), "events", "org.example C"); !errors.Is(err, ErrInvalidProperty) {
		t.Fatalf("class with whitespace = %v, want ErrInvalidProperty", err)
	}
	if err := connector.RemoveConstraint(context.Background(), "events", 0); !errors.Is(err, ErrInvalidProperty) {
		t.Fatalf("zero number = %v, want ErrInvalidProperty", err)
	}
	if err := connector.RemoveConstraint(context.Background(), "", 1); !errors.Is(err, ErrInvalidTableName) {
		t.Fatalf("empty table = %v, want ErrInvalidTableName", err)
	}
	if _, err := connector.ListConstraints(context.Background(), ""); !errors.Is(err, ErrInvalidTableName) {
		t.Fatalf("empty table = %v, want ErrInvalidTableName", err)
	}

	manager.mu.Lock()
	defer manager.mu.Unlock()
	if len(manager.propertyRequests)+len(manager.modifyRequests) != 0 {
		t.Fatalf(
			"rejected arguments still wrote %d properties",
			len(manager.propertyRequests)+len(manager.modifyRequests),
		)
	}
}

func TestConstraintOperationsHonorCancellationAndClose(t *testing.T) {
	connector, _ := constraintConnector(t, map[string]string{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := connector.AddConstraint(ctx, "events", "org.example.C"); !errors.Is(err, context.Canceled) {
		t.Fatalf("AddConstraint = %v, want context.Canceled", err)
	}
	if _, err := connector.ListConstraints(ctx, "events"); !errors.Is(err, context.Canceled) {
		t.Fatalf("ListConstraints = %v, want context.Canceled", err)
	}
	if err := connector.RemoveConstraint(ctx, "events", 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("RemoveConstraint = %v, want context.Canceled", err)
	}

	closed, _ := constraintConnector(t, map[string]string{})
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := closed.AddConstraint(context.Background(), "events", "org.example.C"); !errors.Is(err, ErrConnectorClosed) {
		t.Fatalf("AddConstraint after Close = %v, want ErrConnectorClosed", err)
	}
	if _, err := closed.ListConstraints(context.Background(), "events"); !errors.Is(err, ErrConnectorClosed) {
		t.Fatalf("ListConstraints after Close = %v, want ErrConnectorClosed", err)
	}
	if err := closed.RemoveConstraint(context.Background(), "events", 1); !errors.Is(err, ErrConnectorClosed) {
		t.Fatalf("RemoveConstraint after Close = %v, want ErrConnectorClosed", err)
	}
}

func TestConstraintReadsAreSafeForConcurrentUse(t *testing.T) {
	connector, _ := constraintConnector(t, map[string]string{
		"table.constraint.1": "org.example.First",
		"table.constraint.2": "org.example.Second",
	})
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				constraints, err := connector.ListConstraints(context.Background(), "events")
				if err != nil {
					t.Error(err)
					return
				}
				if len(constraints) != 2 {
					t.Errorf("constraints = %d, want 2", len(constraints))
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestFlushTableRangeSendsRowBounds(t *testing.T) {
	names := &fakeTableNames{byName: map[string]string{"events": "1"}}
	connector := testConnectorWithDiscovery(t, &fakeTabletWalker{}, names)
	manager := &fakeManagerAdapter{}
	connector.manager = manager
	connector.managerAddr = fakeManagerAddress{address: "manager:9997"}

	start := []byte("row2")
	end := []byte("row8")
	if err := connector.FlushTableRange(context.Background(), "events", start, end, true); err != nil {
		t.Fatal(err)
	}
	// A caller may reuse its slices: the bounds must have been copied.
	start[0] = 'X'
	end[0] = 'X'

	if err := connector.FlushTable(context.Background(), "events", false); err != nil {
		t.Fatal(err)
	}

	manager.mu.Lock()
	defer manager.mu.Unlock()
	if len(manager.flushRequests) != 2 {
		t.Fatalf("flush requests = %d, want 2", len(manager.flushRequests))
	}
	bounded := manager.flushRequests[0]
	if !bytes.Equal(bounded.startRow, []byte("row2")) || !bytes.Equal(bounded.endRow, []byte("row8")) {
		t.Fatalf("bounded flush rows = %q/%q", bounded.startRow, bounded.endRow)
	}
	if bounded.tableID != "1" || !bounded.wait {
		t.Fatalf("bounded flush = %#v", bounded)
	}
	whole := manager.flushRequests[1]
	if len(whole.startRow) != 0 || len(whole.endRow) != 0 {
		t.Fatalf("whole-table flush carried rows %q/%q", whole.startRow, whole.endRow)
	}
	if whole.wait {
		t.Fatalf("whole-table flush waited")
	}
}

func TestFlushTableRangeRejectsReversedAndInvalidBounds(t *testing.T) {
	names := &fakeTableNames{byName: map[string]string{"events": "1"}}
	connector := testConnectorWithDiscovery(t, &fakeTabletWalker{}, names)
	manager := &fakeManagerAdapter{}
	connector.manager = manager
	connector.managerAddr = fakeManagerAddress{address: "manager:9997"}

	err := connector.FlushTableRange(context.Background(), "events", []byte("row8"), []byte("row2"), false)
	if !errors.Is(err, ErrInvalidTableRange) {
		t.Fatalf("reversed bounds = %v, want ErrInvalidTableRange", err)
	}
	if errors.Is(err, ErrInvalidTableSplit) {
		t.Fatal("a reversed flush range must not report a split error")
	}
	if err := connector.FlushTableRange(context.Background(), "", nil, nil, false); !errors.Is(err, ErrInvalidTableName) {
		t.Fatalf("empty table = %v, want ErrInvalidTableName", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := connector.FlushTableRange(ctx, "events", nil, nil, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context = %v, want context.Canceled", err)
	}

	manager.mu.Lock()
	defer manager.mu.Unlock()
	if len(manager.flushRequests) != 0 {
		t.Fatalf("rejected flushes still reached the manager: %d", len(manager.flushRequests))
	}
}

// TestFlushTableRangeAcceptsEqualBounds pins that a single-row flush is legal:
// Accumulo flushes the tablets covering [row, row].
func TestFlushTableRangeAcceptsEqualBounds(t *testing.T) {
	names := &fakeTableNames{byName: map[string]string{"events": "1"}}
	connector := testConnectorWithDiscovery(t, &fakeTabletWalker{}, names)
	manager := &fakeManagerAdapter{}
	connector.manager = manager
	connector.managerAddr = fakeManagerAddress{address: "manager:9997"}

	if err := connector.FlushTableRange(context.Background(), "events", []byte("row"), []byte("row"), false); err != nil {
		t.Fatalf("equal bounds = %v, want nil", err)
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if len(manager.flushRequests) != 1 {
		t.Fatalf("flush requests = %d, want 1", len(manager.flushRequests))
	}
}

func TestConstraintPropertyPrefixMatchesAccumulo(t *testing.T) {
	if !strings.HasPrefix(ConstraintPropertyPrefix, "table.") {
		t.Fatalf("prefix = %q, want a table property", ConstraintPropertyPrefix)
	}
	if ConstraintPropertyPrefix != "table.constraint." {
		t.Fatalf("prefix = %q", ConstraintPropertyPrefix)
	}
}

// TestConcurrentAddConstraintAllocatesDistinctNumbers pins that the
// check-then-write sequence is serialized: two classes added at the same time
// must land on different numbers and both must survive.
func TestConcurrentAddConstraintAllocatesDistinctNumbers(t *testing.T) {
	connector, manager := constraintConnector(t, map[string]string{})

	const writers = 8
	numbers := make([]int32, writers)
	errs := make([]error, writers)
	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)
	for i := 0; i < writers; i++ {
		done.Add(1)
		go func(index int) {
			defer done.Done()
			start.Wait()
			numbers[index], errs[index] = connector.AddConstraint(
				context.Background(),
				"events",
				fmt.Sprintf("org.example.Constraint%d", index),
			)
		}(i)
	}
	start.Done()
	done.Wait()

	seen := make(map[int32]bool, writers)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
		if numbers[i] < 1 {
			t.Fatalf("writer %d got number %d", i, numbers[i])
		}
		if seen[numbers[i]] {
			t.Fatalf("number %d was assigned twice", numbers[i])
		}
		seen[numbers[i]] = true
	}

	installed, err := connector.ListConstraints(context.Background(), "events")
	if err != nil {
		t.Fatal(err)
	}
	if len(installed) != writers {
		t.Fatalf("installed = %d, want %d: a concurrent add overwrote another", len(installed), writers)
	}
	for i := 0; i < writers; i++ {
		want := fmt.Sprintf("org.example.Constraint%d", i)
		if _, found := constraintNumberOf(installed, want); !found {
			t.Fatalf("%s is not installed: %#v", want, installed)
		}
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if len(manager.modifyRequests) != writers {
		t.Fatalf("versioned writes = %d, want exactly one per class", len(manager.modifyRequests))
	}
}

// constraintConnectorOver builds a connector over an existing fake manager, so
// two connectors can share one server's property state.
func constraintConnectorOver(t *testing.T, manager *fakeManagerAdapter) *Connector {
	t.Helper()
	names := &fakeTableNames{byName: map[string]string{"events": "1"}}
	connector := testConnectorWithDiscovery(t, &fakeTabletWalker{}, names)
	connector.manager = manager
	connector.managerAddr = fakeManagerAddress{address: "manager:9997"}
	connector.clientAddr = fakeClientServiceAddresses{addresses: []string{"tserver:9997"}}
	return connector
}

// TestAddConstraintRetriesWhenAnotherConnectorWinsTheVersion pins the
// cross-connector case: a per-connector lock cannot order two clients, so the
// write is a compare-and-set. The loser must retry against the winner's state
// rather than overwrite it.
func TestAddConstraintRetriesWhenAnotherConnectorWinsTheVersion(t *testing.T) {
	shared := &fakeManagerAdapter{configuration: map[string]string{}}
	first := constraintConnectorOver(t, shared)
	second := constraintConnectorOver(t, shared)

	// A plain guard rather than sync.Once: the nested call re-enters the hook,
	// and Once.Do is not reentrant.
	var hookMu sync.Mutex
	fired := false
	var winnerNumber int32
	var winnerErr error
	shared.mu.Lock()
	shared.beforeModify = func() {
		hookMu.Lock()
		if fired {
			hookMu.Unlock()
			return
		}
		fired = true
		hookMu.Unlock()
		// Runs while the first connector still holds the version it read.
		winnerNumber, winnerErr = second.AddConstraint(
			context.Background(), "events", "org.example.Winner",
		)
	}
	shared.mu.Unlock()

	loserNumber, err := first.AddConstraint(context.Background(), "events", "org.example.Loser")
	if err != nil {
		t.Fatal(err)
	}
	if winnerErr != nil {
		t.Fatal(winnerErr)
	}
	if winnerNumber != 1 {
		t.Fatalf("winner number = %d, want 1", winnerNumber)
	}
	if loserNumber != 2 {
		t.Fatalf("loser number = %d, want 2 after retrying against the winner's state", loserNumber)
	}

	shared.mu.Lock()
	rejected := shared.rejectedModifies
	shared.mu.Unlock()
	if rejected != 1 {
		t.Fatalf("rejected writes = %d, want the stale write to be rejected once", rejected)
	}

	installed, err := first.ListConstraints(context.Background(), "events")
	if err != nil {
		t.Fatal(err)
	}
	if len(installed) != 2 {
		t.Fatalf("installed = %#v, want both classes", installed)
	}
	for _, want := range []string{"org.example.Winner", "org.example.Loser"} {
		if _, found := constraintNumberOf(installed, want); !found {
			t.Fatalf("%s was overwritten: %#v", want, installed)
		}
	}
}

// TestAddConstraintReadsThroughClientServiceEndpoints pins that the versioned
// read is a ClientService call: it resolves client-service addresses, retries a
// dead endpoint, and only the write goes to the manager.
func TestAddConstraintReadsThroughClientServiceEndpoints(t *testing.T) {
	names := &fakeTableNames{byName: map[string]string{"events": "1"}}
	connector := testConnectorWithDiscovery(t, &fakeTabletWalker{}, names)
	manager := &fakeManagerAdapter{configuration: map[string]string{}}
	connector.manager = manager
	connector.managerAddr = fakeManagerAddress{address: "manager:9997"}
	connector.clientAddr = fakeClientServiceAddresses{addresses: []string{"tserver-a:9997", "tserver-b:9997"}}

	number, err := connector.AddConstraint(context.Background(), "events", "org.example.C")
	if err != nil {
		t.Fatal(err)
	}
	if number != 1 {
		t.Fatalf("number = %d, want 1", number)
	}

	manager.mu.Lock()
	defer manager.mu.Unlock()
	if len(manager.configurationRPC) == 0 {
		t.Fatal("no client-service endpoint was contacted for the property reads")
	}
	for _, address := range manager.configurationRPC {
		if address == "manager:9997" {
			t.Fatalf("a property read went to the manager: %v", manager.configurationRPC)
		}
	}
	if len(manager.modifyRequests) != 1 {
		t.Fatalf("versioned writes = %d, want 1", len(manager.modifyRequests))
	}
}

// TestAddConstraintFailsWhenClientServiceIsUnavailable pins that a missing
// client service is reported as such rather than as a manager failure.
func TestAddConstraintFailsWhenClientServiceIsUnavailable(t *testing.T) {
	names := &fakeTableNames{byName: map[string]string{"events": "1"}}
	connector := testConnectorWithDiscovery(t, &fakeTabletWalker{}, names)
	connector.manager = &fakeManagerAdapter{configuration: map[string]string{}}
	connector.managerAddr = fakeManagerAddress{address: "manager:9997"}
	connector.clientAddr = fakeClientServiceAddresses{err: zk.ErrClientServiceUnavailable}

	if _, err := connector.AddConstraint(context.Background(), "events", "org.example.C"); !errors.Is(err, ErrClientServiceUnavailable) {
		t.Fatalf("AddConstraint = %v, want ErrClientServiceUnavailable", err)
	}
}
