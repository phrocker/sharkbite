package accumulo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apache/thrift/lib/go/thrift"
	"github.com/phrocker/shoal-oss/internal/managerclient"
	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/zk"
)

// ExecuteStatus and UpdateTabletMergeability keep the shared
// fakeManagerAdapter (accumulo/table_admin_test.go) satisfying
// managerclient.Adapter after split support widened the interface. Split
// tests use fakeSplitManager below instead.
func (m *fakeManagerAdapter) ExecuteStatus(
	ctx context.Context,
	address string,
	req managerclient.Request,
) (string, error) {
	return managerclient.SplitSucceeded, m.Execute(ctx, address, req)
}

func (m *fakeManagerAdapter) UpdateTabletMergeability(
	_ context.Context,
	address, _ string,
	_ []managerclient.MergeabilityUpdate,
) ([]managerclient.TabletExtent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.address = address
	return nil, m.err
}

type fakeSplitCall struct {
	address string
	request managerclient.Request
}

type fakeMergeabilityCall struct {
	address   string
	tableName string
	updates   []managerclient.MergeabilityUpdate
}

// fakeSplitManager is a managerclient.Adapter whose split calls are fully
// scriptable, so tests can drive per-group status, partial failures and
// mergeability results.
type fakeSplitManager struct {
	mu        sync.Mutex
	splits    []fakeSplitCall
	merges    []fakeMergeabilityCall
	statusFn  func(int, managerclient.Request) (string, error)
	mergeFn   func(int, []managerclient.MergeabilityUpdate) ([]managerclient.TabletExtent, error)
	closes    int
	flushes   int
	propCalls int
}

func (m *fakeSplitManager) Execute(
	ctx context.Context,
	address string,
	req managerclient.Request,
) error {
	_, err := m.ExecuteStatus(ctx, address, req)
	return err
}

func (m *fakeSplitManager) ExecuteStatus(
	_ context.Context,
	address string,
	req managerclient.Request,
) (string, error) {
	m.mu.Lock()
	index := len(m.splits)
	m.splits = append(m.splits, fakeSplitCall{address: address, request: req})
	fn := m.statusFn
	m.mu.Unlock()
	if fn == nil {
		return managerclient.SplitSucceeded, nil
	}
	return fn(index, req)
}

func (m *fakeSplitManager) UpdateTabletMergeability(
	_ context.Context,
	address, tableName string,
	updates []managerclient.MergeabilityUpdate,
) ([]managerclient.TabletExtent, error) {
	m.mu.Lock()
	index := len(m.merges)
	m.merges = append(m.merges, fakeMergeabilityCall{
		address:   address,
		tableName: tableName,
		updates:   updates,
	})
	fn := m.mergeFn
	m.mu.Unlock()
	if fn == nil {
		accepted := make([]managerclient.TabletExtent, 0, len(updates))
		for _, update := range updates {
			accepted = append(accepted, update.Extent)
		}
		return accepted, nil
	}
	return fn(index, updates)
}

func (m *fakeSplitManager) GetVersionedTableProperties(
	context.Context,
	string,
	string,
) (managerclient.VersionedProperties, error) {
	return managerclient.VersionedProperties{}, nil
}

func (m *fakeSplitManager) ModifyTableProperties(
	context.Context,
	string,
	string,
	managerclient.VersionedProperties,
) error {
	return nil
}

func (m *fakeSplitManager) FlushTable(
	context.Context,
	string,
	string,
	[]byte,
	[]byte,
	bool,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.flushes++
	return nil
}

func (m *fakeSplitManager) GetTableConfiguration(
	context.Context,
	string,
	string,
) (map[string]string, error) {
	return map[string]string{}, nil
}

func (m *fakeSplitManager) GetNamespaceConfiguration(
	ctx context.Context,
	address, namespace string,
) (map[string]string, error) {
	return m.GetTableConfiguration(ctx, address, namespace)
}

func (m *fakeSplitManager) GetNamespaceProperties(
	ctx context.Context,
	address, namespace string,
) (map[string]string, error) {
	return m.GetTableConfiguration(ctx, address, namespace)
}

func (m *fakeSplitManager) GetVersionedNamespaceProperties(
	ctx context.Context,
	address, namespace string,
) (managerclient.VersionedProperties, error) {
	properties, err := m.GetNamespaceProperties(ctx, address, namespace)
	return managerclient.VersionedProperties{Properties: properties}, err
}

func (m *fakeSplitManager) SetTableProperty(context.Context, string, string, string, string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.propCalls++
	return nil
}

func (m *fakeSplitManager) SetNamespaceProperty(
	ctx context.Context,
	address, namespace, property, value string,
) error {
	return m.SetTableProperty(ctx, address, namespace, property, value)
}

func (m *fakeSplitManager) RemoveTableProperty(context.Context, string, string, string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.propCalls++
	return nil
}

func (m *fakeSplitManager) RemoveNamespaceProperty(
	ctx context.Context,
	address, namespace, property string,
) error {
	return m.RemoveTableProperty(ctx, address, namespace, property)
}

func (m *fakeSplitManager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closes++
	return nil
}

func (m *fakeSplitManager) splitCalls() []fakeSplitCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]fakeSplitCall(nil), m.splits...)
}

func (m *fakeSplitManager) mergeCalls() []fakeMergeabilityCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]fakeMergeabilityCall(nil), m.merges...)
}

// scriptedTabletWalker returns a different tablet list per call so tests can
// simulate tablets moving or splitting between rounds.
type scriptedTabletWalker struct {
	mu     sync.Mutex
	rounds [][]metadata.TabletInfo
	calls  int
}

func (w *scriptedTabletWalker) LocateTable(
	ctx context.Context,
	tableID string,
) ([]metadata.TabletInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	index := w.calls
	w.calls++
	if index >= len(w.rounds) {
		index = len(w.rounds) - 1
	}
	if index < 0 {
		return nil, nil
	}
	return append([]metadata.TabletInfo(nil), w.rounds[index]...), nil
}

func (w *scriptedTabletWalker) callCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls
}

type fakeTableStateReader struct {
	states  map[string]zk.TableStateResult
	missing *zk.TableStateResult
	err     error
}

func (r *fakeTableStateReader) TableState(
	ctx context.Context,
	tableID string,
) (zk.TableStateResult, error) {
	if err := ctx.Err(); err != nil {
		return zk.TableStateResult{}, err
	}
	if r != nil && r.err != nil {
		return zk.TableStateResult{}, r.err
	}
	if r != nil {
		if state, ok := r.states[tableID]; ok {
			return state, nil
		}
		if r.missing != nil {
			return *r.missing, nil
		}
	}
	return zk.TableStateResult{Exists: true, State: "ONLINE"}, nil
}

func splitTestTablets() []metadata.TabletInfo {
	return []metadata.TabletInfo{
		{TableID: "1", EndRow: []byte("k")},
		{TableID: "1", PrevRow: []byte("k"), EndRow: []byte("p")},
		{TableID: "1", PrevRow: []byte("p")},
	}
}

func splitTestConnector(
	t *testing.T,
	walker interface {
		LocateTable(context.Context, string) ([]metadata.TabletInfo, error)
	},
	names *fakeTableNames,
) (*Connector, *fakeSplitManager) {
	return splitTestConnectorWithState(t, walker, names, nil)
}

func splitTestConnectorWithState(
	t *testing.T,
	walker interface {
		LocateTable(context.Context, string) ([]metadata.TabletInfo, error)
	},
	names *fakeTableNames,
	state tableStateReader,
) (*Connector, *fakeSplitManager) {
	t.Helper()
	connector := testConnectorWithDiscoveryAndState(t, walker, names, state)
	manager := &fakeSplitManager{}
	connector.manager = manager
	connector.managerAddr = fakeManagerAddress{address: "manager:9997"}
	return connector, manager
}

func splitTestNames() *fakeTableNames {
	return &fakeTableNames{
		byName: map[string]string{"events": "1", "accumulo.audit": "+a"},
		byID:   map[string]string{"1": "events", "+a": "accumulo.audit"},
	}
}

func payload(t *testing.T, row string) string {
	t.Helper()
	encoded, err := encodeSplitMergeability([]byte(row), neverMergeable())
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func argumentStrings(arguments [][]byte) []string {
	out := make([]string, len(arguments))
	for i, argument := range arguments {
		out[i] = string(argument)
	}
	return out
}

func TestAddTableSplitsSubmitsExactFateArgumentsPerTablet(t *testing.T) {
	walker := &scriptedTabletWalker{rounds: [][]metadata.TabletInfo{splitTestTablets()}}
	connector, manager := splitTestConnector(t, walker, splitTestNames())

	splits := [][]byte{
		[]byte("m"),
		[]byte("z"),
		[]byte("c"),
		[]byte("n"),
	}
	if err := connector.AddTableSplits(context.Background(), "events", splits); err != nil {
		t.Fatal(err)
	}

	calls := manager.splitCalls()
	if len(calls) != 3 {
		t.Fatalf("split calls = %d, want 3", len(calls))
	}
	wantArguments := [][]string{
		{"1", "k", "", payload(t, "c")},
		{"1", "p", "k", payload(t, "m"), payload(t, "n")},
		{"1", "", "p", payload(t, "z")},
	}
	for i, call := range calls {
		if call.address != "manager:9997" {
			t.Fatalf("call %d address = %q", i, call.address)
		}
		if call.request.Operation != managerclient.TableSplit {
			t.Fatalf("call %d operation = %v, want TableSplit", i, call.request.Operation)
		}
		if call.request.Instance != managerclient.FateUser {
			t.Fatalf("call %d FATE instance = %v, want user", i, call.request.Instance)
		}
		if len(call.request.Options) != 0 {
			t.Fatalf("call %d options = %#v, want empty", i, call.request.Options)
		}
		got := argumentStrings(call.request.Arguments)
		if len(got) != len(wantArguments[i]) {
			t.Fatalf("call %d arguments = %q, want %q", i, got, wantArguments[i])
		}
		for j := range got {
			if got[j] != wantArguments[i][j] {
				t.Fatalf("call %d argument %d = %q, want %q", i, j, got[j], wantArguments[i][j])
			}
		}
		for j := 1; j <= 2; j++ {
			if call.request.Arguments[j] == nil {
				t.Fatalf("call %d boundary argument %d is nil, want empty slice", i, j)
			}
		}
	}
	if len(manager.mergeCalls()) != 0 {
		t.Fatalf("unexpected mergeability calls: %#v", manager.mergeCalls())
	}
}

func TestAddTableSplitsSortsDeduplicatesAndPreservesBinaryRows(t *testing.T) {
	walker := &scriptedTabletWalker{rounds: [][]metadata.TabletInfo{{{TableID: "1"}}}}
	connector, manager := splitTestConnector(t, walker, splitTestNames())

	binary := []byte{0x00, 0xff}
	high := []byte{0x80}
	splits := [][]byte{
		[]byte("b"),
		high,
		[]byte("a"),
		binary,
		[]byte("b"),
		[]byte("a"),
	}
	if err := connector.AddTableSplits(context.Background(), "events", splits); err != nil {
		t.Fatal(err)
	}
	calls := manager.splitCalls()
	if len(calls) != 1 {
		t.Fatalf("split calls = %d, want 1", len(calls))
	}
	// Unsigned lexicographic order: 0x00 0xff < "a" (0x61) < "b" (0x62) < 0x80.
	want := []string{
		"1", "", "",
		payload(t, "\x00\xff"),
		payload(t, "a"),
		payload(t, "b"),
		payload(t, "\x80"),
	}
	got := argumentStrings(calls[0].request.Arguments)
	if len(got) != len(want) {
		t.Fatalf("arguments = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("argument %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestAddTableSplitsCopiesCallerRowsDefensively(t *testing.T) {
	walker := &scriptedTabletWalker{rounds: [][]metadata.TabletInfo{{{TableID: "1"}}}}
	connector, manager := splitTestConnector(t, walker, splitTestNames())

	row := []byte("m")
	splits := [][]byte{row}
	if err := connector.AddTableSplits(context.Background(), "events", splits); err != nil {
		t.Fatal(err)
	}
	row[0] = 'z'
	splits[0] = []byte("q")

	calls := manager.splitCalls()
	if len(calls) != 1 {
		t.Fatalf("split calls = %d, want 1", len(calls))
	}
	if got := string(calls[0].request.Arguments[3]); got != payload(t, "m") {
		t.Fatalf("submitted payload = %q, want %q", got, payload(t, "m"))
	}
	if string(row) != "z" || string(splits[0]) != "q" {
		t.Fatal("caller slice was rewritten by AddTableSplits")
	}
}

func TestAddTableSplitsGroupsTabletBoundaryRowsIntoTheContainingTablet(t *testing.T) {
	walker := &scriptedTabletWalker{rounds: [][]metadata.TabletInfo{splitTestTablets()}}
	connector, manager := splitTestConnector(t, walker, splitTestNames())

	// "k\x00" is the first row of the (k, p] tablet; "p" is an existing
	// split point and must not be resubmitted as a new split.
	splits := [][]byte{[]byte("k\x00"), []byte("p")}
	if err := connector.AddTableSplits(context.Background(), "events", splits); err != nil {
		t.Fatal(err)
	}
	calls := manager.splitCalls()
	if len(calls) != 1 {
		t.Fatalf("split calls = %d, want 1", len(calls))
	}
	want := []string{"1", "p", "k", payload(t, "k\x00")}
	got := argumentStrings(calls[0].request.Arguments)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("arguments = %q, want %q", got, want)
	}

	merges := manager.mergeCalls()
	if len(merges) != 1 {
		t.Fatalf("mergeability calls = %d, want 1", len(merges))
	}
	if merges[0].tableName != "events" || merges[0].address != "manager:9997" {
		t.Fatalf("mergeability call = %+v", merges[0])
	}
	if len(merges[0].updates) != 1 {
		t.Fatalf("mergeability updates = %#v", merges[0].updates)
	}
	update := merges[0].updates[0]
	if update.Extent.TableID != "1" ||
		string(update.Extent.EndRow) != "p" ||
		string(update.Extent.PrevEndRow) != "k" {
		t.Fatalf("mergeability extent = %+v", update.Extent)
	}
	if !update.Mergeability.Never || update.Mergeability.DelayNanos != -1 {
		t.Fatalf("mergeability = %+v, want never with delay -1", update.Mergeability)
	}
}

func TestAddTableSplitsExistingSplitOnlyRefreshesMergeability(t *testing.T) {
	walker := &scriptedTabletWalker{rounds: [][]metadata.TabletInfo{splitTestTablets()}}
	connector, manager := splitTestConnector(t, walker, splitTestNames())

	if err := connector.AddTableSplits(
		context.Background(),
		"events",
		[][]byte{[]byte("k"), []byte("p")},
	); err != nil {
		t.Fatal(err)
	}
	if calls := manager.splitCalls(); len(calls) != 0 {
		t.Fatalf("split calls = %#v, want none", calls)
	}
	merges := manager.mergeCalls()
	if len(merges) != 1 || len(merges[0].updates) != 2 {
		t.Fatalf("mergeability calls = %#v", merges)
	}
	if string(merges[0].updates[0].Extent.EndRow) != "k" ||
		merges[0].updates[0].Extent.PrevEndRow != nil {
		t.Fatalf("first extent = %+v, want unbounded prev end row", merges[0].updates[0].Extent)
	}
}

func TestAddTableSplitsRejectsOfflineTableBeforeAnyManagerWork(t *testing.T) {
	walker := &scriptedTabletWalker{rounds: [][]metadata.TabletInfo{splitTestTablets()}}
	state := &fakeTableStateReader{states: map[string]zk.TableStateResult{
		"1": {Exists: true, State: zk.TableStateOffline},
	}}
	connector, manager := splitTestConnectorWithState(t, walker, splitTestNames(), state)

	err := connector.AddTableSplits(
		context.Background(),
		"events",
		[][]byte{[]byte("k"), []byte("m")},
	)
	if !errors.Is(err, ErrTableOffline) {
		t.Fatalf("error = %v, want ErrTableOffline", err)
	}
	if calls := manager.splitCalls(); len(calls) != 0 {
		t.Fatalf("offline table still started split FATE: %#v", calls)
	}
	if merges := manager.mergeCalls(); len(merges) != 0 {
		t.Fatalf("offline table still refreshed mergeability: %#v", merges)
	}
	if walker.callCount() != 0 {
		t.Fatalf("offline table still resolved tablets %d time(s)", walker.callCount())
	}
}

func TestAddTableSplitsRetriesMergeabilityTabletsTheManagerRejected(t *testing.T) {
	walker := &scriptedTabletWalker{rounds: [][]metadata.TabletInfo{splitTestTablets()}}
	connector, manager := splitTestConnector(t, walker, splitTestNames())
	manager.mergeFn = func(
		call int,
		updates []managerclient.MergeabilityUpdate,
	) ([]managerclient.TabletExtent, error) {
		if call == 0 {
			return nil, nil
		}
		accepted := make([]managerclient.TabletExtent, 0, len(updates))
		for _, update := range updates {
			accepted = append(accepted, update.Extent)
		}
		return accepted, nil
	}

	if err := connector.AddTableSplits(
		context.Background(),
		"events",
		[][]byte{[]byte("k")},
	); err != nil {
		t.Fatal(err)
	}
	if merges := manager.mergeCalls(); len(merges) != 2 {
		t.Fatalf("mergeability calls = %d, want 2 after a rejected update", len(merges))
	}
	if walker.callCount() != 2 {
		t.Fatalf("tablet lookups = %d, want 2", walker.callCount())
	}
}

func TestAddTableSplitsUsesMetaFateForSystemTables(t *testing.T) {
	walker := &scriptedTabletWalker{rounds: [][]metadata.TabletInfo{{{TableID: "+a"}}}}
	connector, manager := splitTestConnector(t, walker, splitTestNames())

	if err := connector.AddTableSplits(
		context.Background(),
		"accumulo.audit",
		[][]byte{[]byte("m")},
	); err != nil {
		t.Fatal(err)
	}
	calls := manager.splitCalls()
	if len(calls) != 1 {
		t.Fatalf("split calls = %d, want 1", len(calls))
	}
	if calls[0].request.Instance != managerclient.FateMeta {
		t.Fatalf("FATE instance = %v, want meta", calls[0].request.Instance)
	}
	if string(calls[0].request.Arguments[0]) != "+a" {
		t.Fatalf("table ID argument = %q, want %q", calls[0].request.Arguments[0], "+a")
	}
}

func TestAddTableSplitsRetriesGroupsAfterTabletMovement(t *testing.T) {
	moved := []metadata.TabletInfo{
		{TableID: "1", EndRow: []byte("k")},
		{TableID: "1", PrevRow: []byte("k"), EndRow: []byte("n")},
		{TableID: "1", PrevRow: []byte("n")},
	}
	walker := &scriptedTabletWalker{rounds: [][]metadata.TabletInfo{splitTestTablets(), moved}}
	connector, manager := splitTestConnector(t, walker, splitTestNames())
	manager.statusFn = func(call int, _ managerclient.Request) (string, error) {
		if call == 0 {
			// Accumulo returns an empty status when the requested extent no
			// longer exists.
			return "", nil
		}
		return managerclient.SplitSucceeded, nil
	}

	if err := connector.AddTableSplits(
		context.Background(),
		"events",
		[][]byte{[]byte("m")},
	); err != nil {
		t.Fatal(err)
	}
	calls := manager.splitCalls()
	if len(calls) != 2 {
		t.Fatalf("split calls = %d, want 2", len(calls))
	}
	first := argumentStrings(calls[0].request.Arguments)
	second := argumentStrings(calls[1].request.Arguments)
	if first[1] != "p" || first[2] != "k" {
		t.Fatalf("first attempt extent = %q", first)
	}
	if second[1] != "n" || second[2] != "k" {
		t.Fatalf("retry did not use the re-resolved extent: %q", second)
	}
	if walker.callCount() != 2 {
		t.Fatalf("tablet lookups = %d, want 2", walker.callCount())
	}
}

func TestAddTableSplitsFailsAfterBoundedRetries(t *testing.T) {
	walker := &scriptedTabletWalker{rounds: [][]metadata.TabletInfo{splitTestTablets()}}
	connector, manager := splitTestConnector(t, walker, splitTestNames())
	manager.statusFn = func(int, managerclient.Request) (string, error) {
		return "", nil
	}

	target := splitTarget{
		tableName: "events",
		tableID:   "1",
		address:   "manager:9997",
		manager:   manager,
		discovery: connector.discovery,
		retry: splitRetryPolicy{
			attempts:       3,
			initialBackoff: time.Millisecond,
			backoffStep:    time.Millisecond,
			maxBackoff:     2 * time.Millisecond,
			backoffFactor:  1,
		},
	}
	err := addSplits(context.Background(), target, [][]byte{[]byte("m")})
	if !errors.Is(err, ErrTableSplitsIncomplete) {
		t.Fatalf("error = %v, want ErrTableSplitsIncomplete", err)
	}
	if calls := manager.splitCalls(); len(calls) != target.retry.attempts {
		t.Fatalf("split calls = %d, want %d", len(calls), target.retry.attempts)
	}
	if walker.callCount() != target.retry.attempts {
		t.Fatalf("tablet lookups = %d, want %d", walker.callCount(), target.retry.attempts)
	}
}

func TestAddSplitsSkipsTheFinalRetryDelay(t *testing.T) {
	walker := &scriptedTabletWalker{rounds: [][]metadata.TabletInfo{splitTestTablets()}}
	connector, manager := splitTestConnector(t, walker, splitTestNames())
	manager.statusFn = func(int, managerclient.Request) (string, error) {
		return "", nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	target := splitTarget{
		tableName: "events",
		tableID:   "1",
		address:   "manager:9997",
		manager:   manager,
		discovery: connector.discovery,
		retry: splitRetryPolicy{
			attempts:       1,
			initialBackoff: 200 * time.Millisecond,
			backoffStep:    200 * time.Millisecond,
			maxBackoff:     200 * time.Millisecond,
			backoffFactor:  1,
		},
	}
	err := addSplits(ctx, target, [][]byte{[]byte("m")})
	if !errors.Is(err, ErrTableSplitsIncomplete) {
		t.Fatalf("error = %v, want ErrTableSplitsIncomplete without a final delay", err)
	}
}

func TestAddTableSplitsDefaultRetryPolicyIsBounded(t *testing.T) {
	policy := defaultSplitRetryPolicy()
	if policy.attempts != splitRetryAttempts ||
		policy.initialBackoff != splitRetryInitialBackoff ||
		policy.backoffStep != splitRetryBackoffStep ||
		policy.maxBackoff != splitRetryMaxBackoff ||
		policy.backoffFactor != splitRetryBackoffFactor ||
		policy.jitter == nil {
		t.Fatalf("default retry policy = %+v", policy)
	}
	if policy.attempts <= 0 || policy.initialBackoff <= 0 {
		t.Fatalf("default retry policy would busy-loop: %+v", policy)
	}
}

func TestAddTableSplitsKeepsSucceededGroupsOnPartialFailure(t *testing.T) {
	walker := &scriptedTabletWalker{rounds: [][]metadata.TabletInfo{splitTestTablets()}}
	connector, manager := splitTestConnector(t, walker, splitTestNames())
	manager.statusFn = func(call int, _ managerclient.Request) (string, error) {
		if call == 1 {
			return "", &managerclient.Error{Kind: managerclient.ErrorSecurity}
		}
		return managerclient.SplitSucceeded, nil
	}

	err := connector.AddTableSplits(
		context.Background(),
		"events",
		[][]byte{[]byte("c"), []byte("m"), []byte("z")},
	)
	if !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("error = %v, want ErrPermissionDenied", err)
	}
	calls := manager.splitCalls()
	if len(calls) != 3 {
		t.Fatalf("split calls = %d, want all 3 groups attempted", len(calls))
	}
	if got := argumentStrings(calls[2].request.Arguments); got[1] != "" || got[2] != "p" {
		t.Fatalf("third group was skipped after the failure: %q", got)
	}
}

func TestAddTableSplitsSurfacesCleanupErrorsWithoutResubmitting(t *testing.T) {
	walker := &scriptedTabletWalker{rounds: [][]metadata.TabletInfo{splitTestTablets()}}
	connector, manager := splitTestConnector(t, walker, splitTestNames())
	cleanup := errors.New("finish failed")
	manager.statusFn = func(int, managerclient.Request) (string, error) {
		return managerclient.SplitSucceeded, cleanup
	}

	err := connector.AddTableSplits(context.Background(), "events", [][]byte{[]byte("m")})
	if !errors.Is(err, cleanup) {
		t.Fatalf("error = %v, want the joined cleanup error", err)
	}
	if calls := manager.splitCalls(); len(calls) != 1 {
		t.Fatalf("split calls = %d, want 1 — the split succeeded", len(calls))
	}
}

func TestApplySplitPlanRetainsSucceededGroupOnEndpointCleanupError(t *testing.T) {
	walker := &scriptedTabletWalker{rounds: [][]metadata.TabletInfo{splitTestTablets()}}
	connector, manager := splitTestConnector(t, walker, splitTestNames())
	cleanup := thrift.NewTTransportExceptionFromError(errors.New("wait transport close failed"))
	manager.statusFn = func(int, managerclient.Request) (string, error) {
		return managerclient.SplitSucceeded, cleanup
	}
	target := splitTarget{
		tableName: "events",
		tableID:   "1",
		address:   "manager:9997",
		manager:   manager,
		discovery: connector.discovery,
		retry:     defaultSplitRetryPolicy(),
	}
	row := []byte("m")

	completed, err := applySplitPlan(context.Background(), target, splitPlan{
		groups: []splitGroup{{
			extent: TabletExtent{TableID: "1", PrevRow: []byte("k"), EndRow: []byte("p")},
			rows:   [][]byte{row},
		}},
	})
	if !errors.Is(err, ErrManagerUnavailable) || !errors.Is(err, cleanup) {
		t.Fatalf("error = %v, want manager unavailable with cleanup chain", err)
	}
	if len(completed) != 1 || !bytes.Equal(completed[0], row) {
		t.Fatalf("completed = %q, want successful split row retained", completed)
	}
	if calls := manager.splitCalls(); len(calls) != 1 {
		t.Fatalf("split calls = %d, want no replay", len(calls))
	}
}

func TestAddTableSplitsKeepsCleanupErrorsJoinedWithMappedSentinels(t *testing.T) {
	walker := &scriptedTabletWalker{rounds: [][]metadata.TabletInfo{splitTestTablets()}}
	connector, manager := splitTestConnector(t, walker, splitTestNames())
	cleanup := errors.New("finish failed")
	manager.statusFn = func(int, managerclient.Request) (string, error) {
		return "", errors.Join(&managerclient.Error{
			Kind:      managerclient.ErrorSecurity,
			TableName: "events",
		}, cleanup)
	}

	err := connector.AddTableSplits(context.Background(), "events", [][]byte{[]byte("m")})
	if !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("error = %v, want ErrPermissionDenied", err)
	}
	if !errors.Is(err, cleanup) {
		t.Fatalf("error = %v, want the cleanup failure preserved", err)
	}
}

func TestAddTableSplitsStopsRemainingGroupsOnCancellation(t *testing.T) {
	walker := &scriptedTabletWalker{rounds: [][]metadata.TabletInfo{splitTestTablets()}}
	connector, manager := splitTestConnector(t, walker, splitTestNames())
	ctx, cancel := context.WithCancel(context.Background())
	manager.statusFn = func(call int, _ managerclient.Request) (string, error) {
		if call == 0 {
			cancel()
			return managerclient.SplitSucceeded, nil
		}
		return managerclient.SplitSucceeded, nil
	}

	err := connector.AddTableSplits(ctx, "events", [][]byte{[]byte("c"), []byte("m"), []byte("z")})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if calls := manager.splitCalls(); len(calls) != 1 {
		t.Fatalf("split calls = %d, want the remaining groups skipped after cancellation", len(calls))
	}
}

func TestAddTableSplitsMapsManagerErrors(t *testing.T) {
	tests := []struct {
		kind managerclient.ErrorKind
		want error
	}{
		{managerclient.ErrorTableNotFound, ErrTableNotFound},
		{managerclient.ErrorTableOffline, ErrTableOffline},
		{managerclient.ErrorSecurity, ErrPermissionDenied},
		{managerclient.ErrorNamespaceNotFound, ErrNamespaceNotFound},
		{managerclient.ErrorInvalidName, ErrInvalidTableName},
		{managerclient.ErrorNotActive, ErrManagerUnavailable},
	}
	for _, tt := range tests {
		walker := &scriptedTabletWalker{rounds: [][]metadata.TabletInfo{splitTestTablets()}}
		connector, manager := splitTestConnector(t, walker, splitTestNames())
		manager.statusFn = func(int, managerclient.Request) (string, error) {
			return "", &managerclient.Error{Kind: tt.kind, TableName: "events"}
		}
		err := connector.AddTableSplits(context.Background(), "events", [][]byte{[]byte("m")})
		if !errors.Is(err, tt.want) {
			t.Fatalf("kind %d error = %v, want %v", tt.kind, err, tt.want)
		}
	}
}

func TestAddTableSplitsMapsRetryableEndpointFailures(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{
			name: "transport",
			err:  thrift.NewTTransportExceptionFromError(errors.New("reset")),
		},
		{
			name: "network",
			err:  &net.DNSError{Err: "lookup failed", Name: "manager", IsTemporary: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			walker := &scriptedTabletWalker{rounds: [][]metadata.TabletInfo{splitTestTablets()}}
			connector, manager := splitTestConnector(t, walker, splitTestNames())
			manager.statusFn = func(int, managerclient.Request) (string, error) {
				return "", tt.err
			}

			err := connector.AddTableSplits(
				context.Background(),
				"events",
				[][]byte{[]byte("m")},
			)
			if !errors.Is(err, ErrManagerUnavailable) {
				t.Fatalf("error = %v, want ErrManagerUnavailable", err)
			}
			switch tt.name {
			case "transport":
				var transportErr thrift.TTransportException
				if !errors.As(err, &transportErr) {
					t.Fatalf("error = %v, want transport cause preserved", err)
				}
			case "network":
				var networkErr net.Error
				if !errors.As(err, &networkErr) {
					t.Fatalf("error = %v, want network cause preserved", err)
				}
			}
		})
	}
}

func TestAddTableSplitsMapsMissingTableAndDiscoveryFailures(t *testing.T) {
	walker := &scriptedTabletWalker{rounds: [][]metadata.TabletInfo{splitTestTablets()}}
	connector, _ := splitTestConnector(t, walker, splitTestNames())

	err := connector.AddTableSplits(context.Background(), "missing", [][]byte{[]byte("m")})
	if !errors.Is(err, ErrTableNotFound) {
		t.Fatalf("unknown table error = %v, want ErrTableNotFound", err)
	}

	empty := &scriptedTabletWalker{rounds: [][]metadata.TabletInfo{{}}}
	noTablets, _ := splitTestConnector(t, empty, splitTestNames())
	err = noTablets.AddTableSplits(context.Background(), "events", [][]byte{[]byte("m")})
	if !errors.Is(err, ErrTableNotFound) {
		t.Fatalf("empty metadata error = %v, want ErrTableNotFound", err)
	}
}

func TestAddTableSplitsRefreshesTableIDBeforeResolve(t *testing.T) {
	walker := &scriptedTabletWalker{rounds: [][]metadata.TabletInfo{{{TableID: "fresh"}}}}
	names := &fakeTableNames{
		byName: map[string]string{"events": "stale"},
		byID:   map[string]string{"stale": "events"},
	}
	refreshed := false
	names.onInvalidate = func() {
		names.mu.Lock()
		defer names.mu.Unlock()
		if refreshed {
			return
		}
		refreshed = true
		names.byName["events"] = "fresh"
		delete(names.byID, "stale")
		names.byID["fresh"] = "events"
	}
	missing := zk.TableStateResult{Exists: false}
	state := &fakeTableStateReader{
		states:  map[string]zk.TableStateResult{"fresh": {Exists: true, State: "ONLINE"}},
		missing: &missing,
	}
	connector, manager := splitTestConnectorWithState(t, walker, names, state)

	if err := connector.AddTableSplits(
		context.Background(),
		"events",
		[][]byte{[]byte("m")},
	); err != nil {
		t.Fatal(err)
	}
	calls := manager.splitCalls()
	if len(calls) != 1 {
		t.Fatalf("split calls = %d, want 1", len(calls))
	}
	if string(calls[0].request.Arguments[0]) != "fresh" {
		t.Fatalf("table ID argument = %q, want refreshed ID", calls[0].request.Arguments[0])
	}
}

// TestAddTableSplitsForTableSucceedsWhenPinMatchesFreshResolve proves the
// happy path: when table.ID already names the same table a fresh
// resolve of table.Name returns, AddTableSplitsForTable behaves exactly
// like AddTableSplits and reaches the manager normally.
func TestAddTableSplitsForTableSucceedsWhenPinMatchesFreshResolve(t *testing.T) {
	walker := &scriptedTabletWalker{rounds: [][]metadata.TabletInfo{splitTestTablets()}}
	connector, manager := splitTestConnector(t, walker, splitTestNames())

	err := connector.AddTableSplitsForTable(
		context.Background(),
		Table{Name: "events", ID: "1"},
		[][]byte{[]byte("m")},
	)
	if err != nil {
		t.Fatal(err)
	}
	if calls := manager.splitCalls(); len(calls) != 1 {
		t.Fatalf("split calls = %d, want 1", len(calls))
	}
}

// TestAddTableSplitsForTableRejectsMismatchedPinBeforeAnyManagerCall is
// the core regression test for round 11's suppressed finding: table.ID
// pins a table identity that no longer matches what a fresh resolve of
// table.Name returns (simulating a delete-and-recreate under the same
// name in the window between an external caller's own pin and this
// call). AddTableSplitsForTable must fail closed with
// ErrTableIdentityChanged *before* any tablet locate, split, or
// mergeability call -- proven here by asserting the tablet walker, the
// split manager, and the mergeability manager were never invoked at
// all, not just that the overall call failed.
func TestAddTableSplitsForTableRejectsMismatchedPinBeforeAnyManagerCall(t *testing.T) {
	walker := &scriptedTabletWalker{rounds: [][]metadata.TabletInfo{splitTestTablets()}}
	connector, manager := splitTestConnector(t, walker, splitTestNames())

	err := connector.AddTableSplitsForTable(
		context.Background(),
		Table{Name: "events", ID: "stale-id-from-before-a-delete-and-recreate"},
		[][]byte{[]byte("m")},
	)
	if !errors.Is(err, ErrTableIdentityChanged) {
		t.Fatalf("mismatched pin error = %v, want ErrTableIdentityChanged", err)
	}
	if calls := manager.splitCalls(); len(calls) != 0 {
		t.Fatalf("split calls = %d, want 0 (must fail before any split FATE submission)", len(calls))
	}
	if calls := manager.mergeCalls(); len(calls) != 0 {
		t.Fatalf("mergeability calls = %d, want 0 (must fail before any mergeability RPC)", len(calls))
	}
	if walker.callCount() != 0 {
		t.Fatalf("tablet locate calls = %d, want 0 (must fail before planning touches tablets at all)", walker.callCount())
	}
}

// TestAddTableSplitsForTableRejectsEmptyExpectedTableID proves an empty
// table.ID -- which would silently defeat the whole point of calling
// this instead of AddTableSplits -- is rejected outright, before even
// reaching addTableSplits's shared core (so table name validation,
// tablet discovery, etc. never run either).
func TestAddTableSplitsForTableRejectsEmptyExpectedTableID(t *testing.T) {
	walker := &scriptedTabletWalker{rounds: [][]metadata.TabletInfo{splitTestTablets()}}
	connector, manager := splitTestConnector(t, walker, splitTestNames())

	err := connector.AddTableSplitsForTable(
		context.Background(),
		Table{Name: "events", ID: ""},
		[][]byte{[]byte("m")},
	)
	if !errors.Is(err, ErrInvalidTableName) {
		t.Fatalf("empty table.ID error = %v, want ErrInvalidTableName", err)
	}
	if calls := manager.splitCalls(); len(calls) != 0 {
		t.Fatalf("split calls = %d, want 0", len(calls))
	}
	if walker.callCount() != 0 {
		t.Fatalf("tablet locate calls = %d, want 0", walker.callCount())
	}
}

func TestAddTableSplitsValidatesInputs(t *testing.T) {
	walker := &scriptedTabletWalker{rounds: [][]metadata.TabletInfo{splitTestTablets()}}
	connector, manager := splitTestConnector(t, walker, splitTestNames())

	if err := connector.AddTableSplits(
		context.Background(),
		"",
		[][]byte{[]byte("m")},
	); !errors.Is(err, ErrInvalidTableName) {
		t.Fatalf("empty table name error = %v, want ErrInvalidTableName", err)
	}
	invalid := [][][]byte{
		nil,
		{},
		{[]byte("m"), nil},
		{[]byte("m"), {}},
	}
	for i, splits := range invalid {
		if err := connector.AddTableSplits(
			context.Background(),
			"events",
			splits,
		); !errors.Is(err, ErrInvalidTableSplit) {
			t.Fatalf("invalid splits %d error = %v, want ErrInvalidTableSplit", i, err)
		}
	}
	if calls := manager.splitCalls(); len(calls) != 0 {
		t.Fatalf("rejected input still reached the manager: %#v", calls)
	}
}

func TestAddTableSplitsRejectsMalformedExistingTableNames(t *testing.T) {
	walker := &scriptedTabletWalker{rounds: [][]metadata.TabletInfo{splitTestTablets()}}
	connector, manager := splitTestConnector(t, walker, splitTestNames())

	for _, name := range []string{"bad name", ".events", "analytics.bad-name"} {
		if err := connector.AddTableSplits(
			context.Background(),
			name,
			[][]byte{[]byte("m")},
		); !errors.Is(err, ErrInvalidTableName) {
			t.Fatalf("table %q error = %v, want ErrInvalidTableName", name, err)
		}
	}
	if calls := manager.splitCalls(); len(calls) != 0 {
		t.Fatalf("invalid table name still reached the manager: %#v", calls)
	}
	if walker.callCount() != 0 {
		t.Fatalf("invalid table name still resolved tablets %d time(s)", walker.callCount())
	}
}

func TestAddTableSplitsCancellationAndLifecycle(t *testing.T) {
	walker := &scriptedTabletWalker{rounds: [][]metadata.TabletInfo{splitTestTablets()}}
	connector, manager := splitTestConnector(t, walker, splitTestNames())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := connector.AddTableSplits(
		ctx,
		"events",
		[][]byte{[]byte("m")},
	); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled error = %v, want context.Canceled", err)
	}
	if calls := manager.splitCalls(); len(calls) != 0 {
		t.Fatalf("canceled call reached the manager: %#v", calls)
	}

	inflight, cancelInflight := context.WithCancel(context.Background())
	manager.statusFn = func(int, managerclient.Request) (string, error) {
		cancelInflight()
		return "", context.Canceled
	}
	if err := connector.AddTableSplits(
		inflight,
		"events",
		[][]byte{[]byte("m")},
	); !errors.Is(err, context.Canceled) {
		t.Fatalf("in-flight cancellation error = %v, want context.Canceled", err)
	}

	static, _ := NewStaticInstance("accumulo", "uuid-1")
	credentials, _ := PasswordCredentials("root", []byte("secret"))
	noDiscovery, err := NewConnector(static, credentials, ConnectorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := noDiscovery.AddTableSplits(
		context.Background(),
		"events",
		[][]byte{[]byte("m")},
	); !errors.Is(err, ErrDiscoveryUnavailable) {
		t.Fatalf("static instance error = %v, want ErrDiscoveryUnavailable", err)
	}
	if err := noDiscovery.Close(); err != nil {
		t.Fatal(err)
	}
	if err := noDiscovery.AddTableSplits(
		context.Background(),
		"events",
		[][]byte{[]byte("m")},
	); !errors.Is(err, ErrConnectorClosed) {
		t.Fatalf("closed connector error = %v, want ErrConnectorClosed", err)
	}
}

func TestAddTableSplitsRequiresManagerAddress(t *testing.T) {
	walker := &scriptedTabletWalker{rounds: [][]metadata.TabletInfo{splitTestTablets()}}
	connector, _ := splitTestConnector(t, walker, splitTestNames())
	connector.managerAddr = fakeManagerAddress{err: fmt.Errorf("no manager: %w", errManagerLookup)}

	err := connector.AddTableSplits(context.Background(), "events", [][]byte{[]byte("m")})
	if !errors.Is(err, errManagerLookup) {
		t.Fatalf("error = %v, want the resolver failure", err)
	}
}

var errManagerLookup = errors.New("manager lookup failed")

func TestAddTableSplitsInvalidatesDiscoveryAfterAttempts(t *testing.T) {
	walker := &fakeTabletWalker{tablets: map[string][]metadata.TabletInfo{
		"1": splitTestTablets(),
		"2": {
			{TableID: "2", EndRow: []byte("n")},
			{TableID: "2", PrevRow: []byte("n")},
		},
	}}
	names := splitTestNames()
	names.byName["reports"] = "2"
	names.byID["2"] = "reports"
	connector, manager := splitTestConnector(t, walker, names)
	if _, err := connector.discovery.tablets.LocateTable(context.Background(), "2"); err != nil {
		t.Fatal(err)
	}
	if len(connector.discovery.tablets.Snapshot("2")) == 0 {
		t.Fatal("expected the unrelated table cache to be primed")
	}

	if err := connector.AddTableSplits(
		context.Background(),
		"events",
		[][]byte{[]byte("m")},
	); err != nil {
		t.Fatal(err)
	}
	if names.invalidates != 1 {
		t.Fatalf("name invalidations = %d, want 1", names.invalidates)
	}
	if len(connector.discovery.tablets.Snapshot("1")) != 0 {
		t.Fatal("tablet cache survived a completed split")
	}
	if len(connector.discovery.tablets.Snapshot("2")) == 0 {
		t.Fatal("completed split evicted an unrelated table cache")
	}

	manager.statusFn = func(int, managerclient.Request) (string, error) {
		return "", &managerclient.Error{Kind: managerclient.ErrorSecurity}
	}
	if err := connector.AddTableSplits(
		context.Background(),
		"events",
		[][]byte{[]byte("m")},
	); !errors.Is(err, ErrPermissionDenied) {
		t.Fatal("expected the scripted permission failure")
	}
	if names.invalidates != 2 {
		t.Fatalf("name invalidations = %d, want 2 after a failed split", names.invalidates)
	}
	if len(connector.discovery.tablets.Snapshot("2")) == 0 {
		t.Fatal("failed split evicted an unrelated table cache")
	}
}

func TestPlanTableSplitsHandlesUnresolvedRows(t *testing.T) {
	gapped := []metadata.TabletInfo{
		{TableID: "1", PrevRow: []byte("k"), EndRow: []byte("p")},
	}
	plan := planTableSplits("1", gapped, [][]byte{[]byte("a"), []byte("m"), []byte("z")})
	if len(plan.groups) != 1 || len(plan.groups[0].rows) != 1 {
		t.Fatalf("groups = %#v", plan.groups)
	}
	if string(plan.groups[0].rows[0]) != "m" {
		t.Fatalf("grouped row = %q, want m", plan.groups[0].rows[0])
	}
	if len(plan.unresolved) != 2 {
		t.Fatalf("unresolved = %q, want the rows outside every extent", plan.unresolved)
	}
	if len(plan.existing) != 0 {
		t.Fatalf("existing = %#v, want none", plan.existing)
	}
}

func TestNormalizeSplitRowsSortsDeduplicatesAndCopies(t *testing.T) {
	source := [][]byte{[]byte("b"), []byte("a"), []byte("b"), {0x00}}
	rows, err := normalizeSplitRows(source)
	if err != nil {
		t.Fatal(err)
	}
	want := [][]byte{{0x00}, []byte("a"), []byte("b")}
	if len(rows) != len(want) {
		t.Fatalf("rows = %q, want %q", rows, want)
	}
	for i := range want {
		if !bytes.Equal(rows[i], want[i]) {
			t.Fatalf("row %d = %q, want %q", i, rows[i], want[i])
		}
	}
	source[0][0] = 'z'
	if string(rows[2]) != "b" {
		t.Fatalf("normalized row aliased the caller slice: %q", rows[2])
	}
}

func TestRemoveSplitRowsKeepsPendingOrder(t *testing.T) {
	pending := [][]byte{[]byte("a"), []byte("b"), []byte("c")}
	remaining := removeSplitRows(pending, [][]byte{[]byte("b")})
	if len(remaining) != 2 ||
		string(remaining[0]) != "a" ||
		string(remaining[1]) != "c" {
		t.Fatalf("remaining = %q", remaining)
	}
	if got := removeSplitRows(remaining, nil); len(got) != 2 {
		t.Fatalf("no-op removal = %q", got)
	}
}

func TestSplitRetryScheduleMatchesAccumuloWithoutJitter(t *testing.T) {
	policy := defaultSplitRetryPolicy()
	policy.jitter = func() float64 { return 0.5 }
	retry := newSplitRetrySchedule(policy)

	got := []time.Duration{retry.currentBackoff()}
	for range 8 {
		retry.advance()
		got = append(got, retry.currentBackoff())
	}

	want := []time.Duration{
		100 * time.Millisecond,
		250 * time.Millisecond,
		325 * time.Millisecond,
		438 * time.Millisecond,
		607 * time.Millisecond,
		860 * time.Millisecond,
		1240 * time.Millisecond,
		1809 * time.Millisecond,
		2 * time.Second,
	}
	if len(got) != len(want) {
		t.Fatalf("backoff count = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("backoff[%d] = %v, want %v (all=%v)", i, got[i], want[i], got)
		}
	}
}

func TestSplitRetryScheduleIsBounded(t *testing.T) {
	policy := defaultSplitRetryPolicy()
	policy.jitter = func() float64 { return 1 }
	retry := newSplitRetrySchedule(policy)
	for range 100 {
		if retry.currentBackoff() > policy.maxBackoff {
			t.Fatalf("backoff = %v, exceeded %v", retry.currentBackoff(), policy.maxBackoff)
		}
		retry.advance()
	}
	if retry.currentBackoff() != policy.maxBackoff {
		t.Fatalf("backoff = %v, want %v", retry.currentBackoff(), policy.maxBackoff)
	}
}
