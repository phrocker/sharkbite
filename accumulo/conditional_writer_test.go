package accumulo

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/ingestclient"
	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
)

func newConditionalTestWriter(
	t *testing.T,
	options ConditionalWriterOptions,
) *ConditionalWriter {
	t.Helper()
	connector := testConnectorWithDiscovery(
		t,
		&fakeTabletWalker{tablets: map[string][]metadata.TabletInfo{
			"1": discoveryTablets(),
		}},
		&fakeTableNames{
			byName: map[string]string{"events": "1"},
			byID:   map[string]string{"1": "events"},
		},
	)
	writer, err := connector.NewConditionalWriter(Table{Name: "events"}, options)
	if err != nil {
		t.Fatal(err)
	}
	return writer
}

func testConditionalMutation(t *testing.T, row string) *ConditionalMutation {
	t.Helper()
	mutation, err := NewMutation([]byte(row))
	if err != nil {
		t.Fatal(err)
	}
	mutation.PutLatest([]byte("state"), []byte("value"), nil, []byte("next"))
	condition, err := NewValueCondition(
		[]byte("state"), []byte("value"), nil, []byte("current"),
	)
	if err != nil {
		t.Fatal(err)
	}
	conditional, err := NewConditionalMutation(mutation, condition)
	if err != nil {
		t.Fatal(err)
	}
	return conditional
}

func TestConditionalWriterAcceptedRejectedAndCleanupErrors(t *testing.T) {
	cleanupErr := errors.New("close failed")
	tests := []struct {
		name       string
		internal   ingestclient.ConditionalStatus
		writeErr   error
		wantStatus ConditionalStatus
		wantErr    error
	}{
		{"accepted", ingestclient.ConditionalAccepted, nil, ConditionalAccepted, nil},
		{"rejected", ingestclient.ConditionalRejected, nil, ConditionalRejected, nil},
		{"accepted cleanup", ingestclient.ConditionalAccepted, cleanupErr, ConditionalAccepted, cleanupErr},
		{"rejected cleanup", ingestclient.ConditionalRejected, cleanupErr, ConditionalRejected, cleanupErr},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			writer := newConditionalTestWriter(t, ConditionalWriterOptions{
				Durability: DurabilityFlush,
			})
			var gotDurability ingestclient.Durability
			writer.write = func(
				_ context.Context,
				address, serverLock, tableID string,
				extent *data.TKeyExtent,
				mutation *data.TConditionalMutation,
				durability ingestclient.Durability,
				_ [][]byte,
			) (ingestclient.ConditionalOutcome, error) {
				gotDurability = durability
				if address != "ts1:9997" ||
					serverLock != "/accumulo/instance/tservers/default/ts1:9997/zlock#a$0000000001" ||
					tableID != "1" {
					t.Fatalf("route = %q/%q/%q", address, serverLock, tableID)
				}
				if string(extent.Table) != "1" || mutation.ID == 0 {
					t.Fatalf("wire identity = %#v, mutation ID %d", extent, mutation.ID)
				}
				return ingestclient.ConditionalOutcome{
					Status: test.internal, Submitted: true,
				}, test.writeErr
			}
			status, err := writer.Write(context.Background(), testConditionalMutation(t, "a"))
			if status != test.wantStatus || !errors.Is(err, test.wantErr) {
				t.Fatalf("Write = %s, %v; want %s, %v", status, err, test.wantStatus, test.wantErr)
			}
			if gotDurability != ingestclient.DurabilityFlush {
				t.Fatalf("durability = %v, want flush", gotDurability)
			}
		})
	}
}

func TestConditionalWriterUsesImmutableAuthorizationsForVisibleAbsenceChecks(t *testing.T) {
	authorizations := [][]byte{[]byte("control"), []byte("ops")}
	writer := newConditionalTestWriter(t, ConditionalWriterOptions{
		Authorizations: authorizations,
	})
	authorizations[0][0] = 'X'
	writer.write = func(
		_ context.Context,
		_, _, _ string,
		_ *data.TKeyExtent,
		_ *data.TConditionalMutation,
		_ ingestclient.Durability,
		got [][]byte,
	) (ingestclient.ConditionalOutcome, error) {
		if !equalAuthorizationLists(got, [][]byte{[]byte("control"), []byte("ops")}) {
			return ingestclient.ConditionalOutcome{
				Status: ingestclient.ConditionalAccepted, Submitted: true,
			}, nil
		}
		return ingestclient.ConditionalOutcome{
			Status: ingestclient.ConditionalRejected, Submitted: true,
		}, nil
	}
	status, err := writer.Write(context.Background(), testConditionalMutation(t, "a"))
	if err != nil || status != ConditionalRejected {
		t.Fatalf("visible existing condition appeared absent: %s, %v", status, err)
	}
}

func equalAuthorizationLists(left, right [][]byte) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if !bytes.Equal(left[i], right[i]) {
			return false
		}
	}
	return true
}

func TestConditionalWriterUnknownIsNeverRetried(t *testing.T) {
	writer := newConditionalTestWriter(t, ConditionalWriterOptions{})
	calls := 0
	transportErr := errors.New("response lost")
	writer.write = func(
		context.Context, string, string, string,
		*data.TKeyExtent, *data.TConditionalMutation, ingestclient.Durability,
		[][]byte,
	) (ingestclient.ConditionalOutcome, error) {
		calls++
		return ingestclient.ConditionalOutcome{Submitted: true}, transportErr
	}
	status, err := writer.Write(context.Background(), testConditionalMutation(t, "a"))
	if status != ConditionalUnknown || !errors.Is(err, ErrConditionalUnknown) ||
		!errors.Is(err, transportErr) {
		t.Fatalf("Write = %s, %v", status, err)
	}
	if calls != 1 {
		t.Fatalf("conditional submission called %d times, want 1", calls)
	}
}

func TestConditionalWriterRejectsUnsupportedServerStatusAsUnknown(t *testing.T) {
	writer := newConditionalTestWriter(t, ConditionalWriterOptions{})
	calls := 0
	writer.write = func(
		context.Context, string, string, string,
		*data.TKeyExtent, *data.TConditionalMutation, ingestclient.Durability,
		[][]byte,
	) (ingestclient.ConditionalOutcome, error) {
		calls++
		return ingestclient.ConditionalOutcome{
			Status: ingestclient.ConditionalStatus(99), Submitted: true,
		}, errors.New("unsupported status")
	}
	status, err := writer.Write(context.Background(), testConditionalMutation(t, "a"))
	if status != ConditionalUnknown || !errors.Is(err, ErrConditionalUnknown) || calls != 1 {
		t.Fatalf("Write = %s, %v, calls=%d", status, err, calls)
	}
}

func TestConditionalWriterRetriesOnlyBeforeSubmission(t *testing.T) {
	writer := newConditionalTestWriter(t, ConditionalWriterOptions{
		MaxRetries:   2,
		RetryBackoff: time.Nanosecond,
	})
	calls := 0
	invalidations := 0
	writer.invalidate = func(table Table, row []byte) error {
		invalidations++
		if table.ID != "1" || string(row) != "a" {
			t.Fatalf("invalidate = %#v %q", table, row)
		}
		return nil
	}
	writer.write = func(
		context.Context, string, string, string,
		*data.TKeyExtent, *data.TConditionalMutation, ingestclient.Durability,
		[][]byte,
	) (ingestclient.ConditionalOutcome, error) {
		calls++
		if calls == 1 {
			return ingestclient.ConditionalOutcome{}, errors.New("stale server")
		}
		return ingestclient.ConditionalOutcome{
			Status: ingestclient.ConditionalAccepted, Submitted: true,
		}, nil
	}
	status, err := writer.Write(context.Background(), testConditionalMutation(t, "a"))
	if status != ConditionalAccepted || err != nil {
		t.Fatalf("Write = %s, %v", status, err)
	}
	if calls != 2 || invalidations != 1 {
		t.Fatalf("calls=%d invalidations=%d, want 2/1", calls, invalidations)
	}
}

func TestConditionalWriterCancellationDuringRetry(t *testing.T) {
	writer := newConditionalTestWriter(t, ConditionalWriterOptions{
		MaxRetries:   2,
		RetryBackoff: time.Second,
	})
	ctx, cancel := context.WithCancel(context.Background())
	writer.write = func(
		context.Context, string, string, string,
		*data.TKeyExtent, *data.TConditionalMutation, ingestclient.Durability,
		[][]byte,
	) (ingestclient.ConditionalOutcome, error) {
		cancel()
		return ingestclient.ConditionalOutcome{}, errors.New("stale server")
	}
	status, err := writer.Write(ctx, testConditionalMutation(t, "a"))
	if status != ConditionalUnknown || !errors.Is(err, context.Canceled) {
		t.Fatalf("Write = %s, %v", status, err)
	}
}

func TestConditionalWriterRetriesMalformedAndStaleRouting(t *testing.T) {
	writer := newConditionalTestWriter(t, ConditionalWriterOptions{
		MaxRetries:   2,
		RetryBackoff: time.Nanosecond,
	})
	locates := 0
	writer.locate = func(context.Context, Table, []byte) (Tablet, error) {
		locates++
		if locates == 1 {
			return Tablet{
				Extent: TabletExtent{TableID: "other"},
				Server: &TabletServer{
					HostPort: "old:9997", Session: "old", ServerLock: "/locks/old$old",
				},
			}, nil
		}
		return Tablet{
			Extent: TabletExtent{TableID: "1", EndRow: []byte("k")},
			Server: &TabletServer{
				HostPort: "new:9997", Session: "new", ServerLock: "/locks/new$new",
			},
		}, nil
	}
	writer.invalidate = func(Table, []byte) error { return nil }
	writer.write = func(
		_ context.Context, address, serverLock, tableID string,
		_ *data.TKeyExtent, _ *data.TConditionalMutation, _ ingestclient.Durability,
		_ [][]byte,
	) (ingestclient.ConditionalOutcome, error) {
		if address != "new:9997" || serverLock != "/locks/new$new" || tableID != "1" {
			t.Fatalf("route = %q/%q/%q", address, serverLock, tableID)
		}
		return ingestclient.ConditionalOutcome{
			Status: ingestclient.ConditionalRejected, Submitted: true,
		}, nil
	}

	status, err := writer.Write(context.Background(), testConditionalMutation(t, "a"))
	if status != ConditionalRejected || err != nil || locates != 2 {
		t.Fatalf("Write = %s, %v, locates=%d", status, err, locates)
	}
}

func TestConditionalWriterMissingServerLockFailsBeforeSubmission(t *testing.T) {
	writer := newConditionalTestWriter(t, ConditionalWriterOptions{
		MaxRetries:   1,
		RetryBackoff: time.Nanosecond,
	})
	writer.locate = func(context.Context, Table, []byte) (Tablet, error) {
		return Tablet{
			Extent: TabletExtent{TableID: "1", EndRow: []byte("k")},
			Server: &TabletServer{HostPort: "ts1:9997", Session: "a"},
		}, nil
	}
	writer.invalidate = func(Table, []byte) error { return nil }
	submissions := 0
	writer.write = func(
		context.Context, string, string, string,
		*data.TKeyExtent, *data.TConditionalMutation, ingestclient.Durability,
		[][]byte,
	) (ingestclient.ConditionalOutcome, error) {
		submissions++
		return ingestclient.ConditionalOutcome{}, nil
	}
	status, err := writer.Write(context.Background(), testConditionalMutation(t, "a"))
	if status != ConditionalUnknown || !errors.Is(err, ErrUnsupportedOperation) ||
		!errors.Is(err, ErrConditionalRetryExhausted) {
		t.Fatalf("Write = %s, %v", status, err)
	}
	if submissions != 0 {
		t.Fatalf("submissions = %d, want 0", submissions)
	}
	if errors.Is(err, ErrConditionalUnknown) {
		t.Fatalf("pre-submission failure marked indeterminate: %v", err)
	}
}

func TestConditionalMutationCopiesInputsAndConvertsExactly(t *testing.T) {
	row := []byte("row")
	cf := []byte("family")
	cq := []byte("qualifier")
	cv := []byte("A&B")
	value := []byte("expected")
	update := []byte("next")
	mutation, err := NewMutation(row)
	if err != nil {
		t.Fatal(err)
	}
	mutation.Put(cf, cq, cv, 11, update)
	condition, err := NewValueCondition(cf, cq, cv, value)
	if err != nil {
		t.Fatal(err)
	}
	condition = condition.WithTimestamp(7)
	conditional, err := NewConditionalMutation(mutation, condition)
	if err != nil {
		t.Fatal(err)
	}

	row[0], cf[0], cq[0], cv[0], value[0], update[0] = 'X', 'X', 'X', 'X', 'X', 'X'
	mutation.PutLatest([]byte("later"), nil, nil, []byte("ignored"))
	wire, err := conditional.toThrift(42)
	if err != nil {
		t.Fatal(err)
	}
	if string(wire.Mutation.Row) != "row" || wire.Mutation.Entries != 1 || wire.ID != 42 {
		t.Fatalf("wire mutation = %#v", wire.Mutation)
	}
	if len(wire.Conditions) != 1 {
		t.Fatalf("conditions = %d", len(wire.Conditions))
	}
	got := wire.Conditions[0]
	if string(got.Cf) != "family" || string(got.Cq) != "qualifier" ||
		string(got.Cv) != "A&B" || string(got.Val) != "expected" ||
		!got.HasTimestamp || got.Ts != 7 || got.Iterators != nil {
		t.Fatalf("wire condition = %#v", got)
	}
	decoded, err := cclient.FromThrift(wire.Mutation)
	if err != nil {
		t.Fatal(err)
	}
	entries := decoded.Entries()
	if len(entries) != 1 || string(entries[0].Value) != "next" || entries[0].Timestamp != 11 {
		t.Fatalf("decoded entries = %#v", entries)
	}
	wire.Mutation.Row[0] = 'Z'
	wire.Conditions[0].Val[0] = 'Z'
	again, err := conditional.toThrift(43)
	if err != nil {
		t.Fatal(err)
	}
	if string(again.Mutation.Row) != "row" || string(again.Conditions[0].Val) != "expected" {
		t.Fatal("wire conversion exposed internal storage")
	}
}

func TestConditionalAbsentAndEmptyValueRemainDistinctOnWire(t *testing.T) {
	absent, err := NewAbsentCondition([]byte("cf"), []byte("cq"), nil)
	if err != nil {
		t.Fatal(err)
	}
	empty, err := NewValueCondition([]byte("cf"), []byte("cq"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	absentWire, _ := absent.toThrift()
	emptyWire, _ := empty.toThrift()
	if absentWire.Val != nil {
		t.Fatalf("absent value = %#v", absentWire.Val)
	}
	if emptyWire.Val == nil || len(emptyWire.Val) != 0 {
		t.Fatalf("empty exact value = %#v", emptyWire.Val)
	}
}

func TestConditionalWriterValidation(t *testing.T) {
	if _, err := NewAbsentCondition(nil, nil, nil); err == nil {
		t.Fatal("empty column family accepted")
	}
	if _, err := NewValueCondition([]byte("cf"), nil, []byte("A&"), nil); err == nil {
		t.Fatal("malformed visibility accepted")
	}
	tooLarge := bytes.Repeat([]byte{'x'}, maxConditionalComponentBytes+1)
	if _, err := NewAbsentCondition(tooLarge, nil, nil); err == nil {
		t.Fatal("oversized condition accepted")
	}
	if _, err := NewConditionalMutation(nil); err == nil {
		t.Fatal("nil mutation accepted")
	}
	emptyMutation, _ := NewMutation([]byte("row"))
	condition, _ := NewAbsentCondition([]byte("cf"), nil, nil)
	if _, err := NewConditionalMutation(emptyMutation, condition); err == nil {
		t.Fatal("empty mutation accepted")
	}
	fullMutation, _ := NewMutation([]byte("row"))
	fullMutation.PutLatest([]byte("cf"), nil, nil, nil)
	if _, err := NewConditionalMutation(fullMutation); err == nil {
		t.Fatal("zero conditions accepted")
	}
	many := make([]Condition, maxConditionalConditions+1)
	for index := range many {
		many[index] = condition
	}
	if _, err := NewConditionalMutation(fullMutation, many...); err == nil {
		t.Fatal("too many conditions accepted")
	}
	largeCondition, err := NewValueCondition(
		[]byte("cf"), nil, nil, bytes.Repeat([]byte{'v'}, maxConditionalComponentBytes),
	)
	if err != nil {
		t.Fatal(err)
	}
	largeConditions := make([]Condition, 17)
	for index := range largeConditions {
		largeConditions[index] = largeCondition
	}
	if _, err := NewConditionalMutation(fullMutation, largeConditions...); err == nil {
		t.Fatal("aggregate oversized conditions accepted")
	}

	connector := testConnectorWithDiscovery(
		t,
		&fakeTabletWalker{tablets: map[string][]metadata.TabletInfo{"1": discoveryTablets()}},
		&fakeTableNames{
			byName: map[string]string{"events": "1"},
			byID:   map[string]string{"1": "events"},
		},
	)
	for _, test := range []struct {
		table   Table
		options ConditionalWriterOptions
	}{
		{},
		{table: Table{Name: "events"}, options: ConditionalWriterOptions{MaxRetries: -1}},
		{table: Table{Name: "events"}, options: ConditionalWriterOptions{MaxRetries: 101}},
		{table: Table{Name: "events"}, options: ConditionalWriterOptions{RetryBackoff: -1}},
		{table: Table{Name: "events"}, options: ConditionalWriterOptions{RetryBackoff: time.Minute + 1}},
		{table: Table{Name: "events"}, options: ConditionalWriterOptions{Durability: Durability(99)}},
		{table: Table{Name: "events"}, options: ConditionalWriterOptions{Authorizations: [][]byte{[]byte("bad label")}}},
		{table: Table{Name: "events"}, options: ConditionalWriterOptions{Authorizations: [][]byte{nil}}},
	} {
		if _, err := connector.NewConditionalWriter(test.table, test.options); err == nil {
			t.Fatalf("NewConditionalWriter(%#v, %#v) succeeded", test.table, test.options)
		}
	}
	if _, err := (*Connector)(nil).NewConditionalWriter(Table{Name: "events"}, ConditionalWriterOptions{}); err == nil {
		t.Fatal("nil connector accepted")
	}
}

func TestConditionalWriterConnectorClosureAndTableIdentity(t *testing.T) {
	writer := newConditionalTestWriter(t, ConditionalWriterOptions{})
	if err := writer.connector.Close(); err != nil {
		t.Fatal(err)
	}
	status, err := writer.Write(context.Background(), testConditionalMutation(t, "a"))
	if status != ConditionalUnknown || !errors.Is(err, ErrConnectorClosed) {
		t.Fatalf("Write after connector Close = %s, %v", status, err)
	}

	writer = newConditionalTestWriter(t, ConditionalWriterOptions{})
	writer.table = Table{Name: "events", ID: "stale"}
	status, err = writer.Write(context.Background(), testConditionalMutation(t, "a"))
	if status != ConditionalUnknown || !errors.Is(err, ErrTableIdentityChanged) {
		t.Fatalf("stale table identity = %s, %v", status, err)
	}
}

func TestConditionalWriterRoutingRetryExhaustion(t *testing.T) {
	writer := newConditionalTestWriter(t, ConditionalWriterOptions{
		MaxRetries:   1,
		RetryBackoff: time.Nanosecond,
	})
	writer.locate = func(context.Context, Table, []byte) (Tablet, error) {
		return Tablet{}, ErrTabletNotLocated
	}
	writer.invalidate = func(Table, []byte) error { return nil }
	status, err := writer.Write(context.Background(), testConditionalMutation(t, "a"))
	if status != ConditionalUnknown || !errors.Is(err, ErrConditionalRetryExhausted) ||
		!errors.Is(err, ErrTabletNotLocated) {
		t.Fatalf("Write = %s, %v", status, err)
	}
}

func TestConditionalStatusString(t *testing.T) {
	if got := strings.Join([]string{
		ConditionalAccepted.String(),
		ConditionalRejected.String(),
		ConditionalUnknown.String(),
		ConditionalStatus(99).String(),
	}, ","); got != "Accepted,Rejected,Unknown,Unknown" {
		t.Fatalf("statuses = %q", got)
	}
}
