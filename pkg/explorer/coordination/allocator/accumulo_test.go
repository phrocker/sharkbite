package allocator

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/phrocker/shoal-oss/accumulo"
	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
)

type fakeAccumuloScanner struct {
	values []accumulo.KeyValue
	ranges []*accumulo.Range
	err    error
}

func (s *fakeAccumuloScanner) Scan(_ context.Context, scanRange *accumulo.Range) ([]accumulo.KeyValue, error) {
	s.ranges = append(s.ranges, scanRange)
	return append([]accumulo.KeyValue(nil), s.values...), s.err
}

type fakeAccumuloWriter struct {
	status   accumulo.ConditionalStatus
	err      error
	mutation *accumulo.ConditionalMutation
}

func TestTrustedConditionalWriterOptionsOverrideAndCopyAuthorizations(t *testing.T) {
	trusted := [][]byte{[]byte("CONTROL")}
	options := trustedConditionalWriterOptions(accumulo.ConditionalWriterOptions{
		Authorizations: [][]byte{[]byte("request-override")},
	}, trusted)
	trusted[0][0] = 'X'
	if !reflect.DeepEqual(options.Authorizations, [][]byte{[]byte("CONTROL")}) {
		t.Fatalf("writer authorizations = %q", options.Authorizations)
	}
}

func (w *fakeAccumuloWriter) Write(_ context.Context, mutation *accumulo.ConditionalMutation) (accumulo.ConditionalStatus, error) {
	w.mutation = mutation
	return w.status, w.err
}

func TestAccumuloStoreExactReadUsesTrustedCoordinates(t *testing.T) {
	row := []byte("allocator-row")
	visibility := []byte("CONTROL")
	scanner := &fakeAccumuloScanner{values: []accumulo.KeyValue{
		{Key: accumulo.Key{Row: row, ColumnFamily: []byte("q"), ColumnQualifier: []byte("head"), ColumnVisibility: visibility, Timestamp: 7}, Value: []byte("head-value")},
		{Key: accumulo.Key{Row: row, ColumnFamily: []byte("q"), ColumnQualifier: []byte("head"), ColumnVisibility: visibility, Timestamp: 6}, Value: []byte("shadowed")},
		{Key: accumulo.Key{Row: row, ColumnFamily: []byte("r"), ColumnQualifier: []byte("other"), ColumnVisibility: visibility}, Value: []byte("ignored")},
	}}
	store := &AccumuloStore{scanner: scanner}
	coordinate := Coordinate{Row: row, Family: []byte("q"), Qualifier: []byte("head"), Visibility: visibility}
	cells, err := store.ReadExact(context.Background(), []Coordinate{coordinate})
	if err != nil || len(cells) != 1 || !cells[0].Coordinate.equal(coordinate) ||
		!bytes.Equal(cells[0].Value, []byte("head-value")) || cells[0].Timestamp != 7 {
		t.Fatalf("ReadExact = %#v, %v", cells, err)
	}

	if len(scanner.ranges) != 1 || !bytes.Equal(scanner.ranges[0].StartRow(), row) ||
		!bytes.Equal(scanner.ranges[0].EndRow(), row) {
		t.Fatalf("exact row range = %#v", scanner.ranges)
	}
}

func TestAccumuloStorePrefixSeekUsesExactColumnAndBoundedRange(t *testing.T) {
	prefix := []byte{1, 'L', 7, 6, 'd', 'o', 'm', 'a', 'i', 'n'}
	start := append(append([]byte(nil), prefix...), 'b')
	visibility := []byte("CONTROL")
	scanner := &fakeAccumuloScanner{values: []accumulo.KeyValue{
		{Key: accumulo.Key{Row: append(append([]byte(nil), prefix...), 'b'), ColumnFamily: []byte("l"), ColumnQualifier: []byte("lease"), ColumnVisibility: visibility, Timestamp: 2}, Value: []byte("b")},
		{Key: accumulo.Key{Row: append(append([]byte(nil), prefix...), 'b'), ColumnFamily: []byte("l"), ColumnQualifier: []byte("lease"), ColumnVisibility: visibility, Timestamp: 1}, Value: []byte("shadowed")},
		{Key: accumulo.Key{Row: append(append([]byte(nil), prefix...), 'c'), ColumnFamily: []byte("x"), ColumnQualifier: []byte("lease"), ColumnVisibility: visibility, Timestamp: 3}, Value: []byte("wrong-column")},
		{Key: accumulo.Key{Row: append(append([]byte(nil), prefix...), 'd'), ColumnFamily: []byte("l"), ColumnQualifier: []byte("lease"), ColumnVisibility: visibility, Timestamp: 4}, Value: []byte("d")},
	}}
	store := &AccumuloStore{scanner: scanner}
	cells, err := store.ScanPrefixFrom(context.Background(), prefix, start, []byte("l"), []byte("lease"), visibility, 3)
	if err != nil || len(cells) != 2 || string(cells[0].Value) != "b" || string(cells[1].Value) != "d" {
		t.Fatalf("ScanPrefixFrom = %#v, %v", cells, err)
	}
	if len(scanner.ranges) != 1 || !bytes.Equal(scanner.ranges[0].StartRow(), start) {
		t.Fatalf("prefix start range = %#v", scanner.ranges)
	}
	wantEnd := append([]byte(nil), prefix...)
	wantEnd[len(wantEnd)-1]++
	if !bytes.Equal(scanner.ranges[0].EndRow(), wantEnd) {
		t.Fatalf("prefix end = %x, want %x", scanner.ranges[0].EndRow(), wantEnd)
	}
}

func TestAccumuloStorePrefixSeekAcceptsUsableResultsAfterCleanupFailure(t *testing.T) {
	prefix := []byte("prefix")
	row := append(append([]byte(nil), prefix...), "-row"...)
	visibility := []byte("CONTROL")
	scanner := &fakeAccumuloScanner{
		values: []accumulo.KeyValue{{
			Key: accumulo.Key{
				Row: row, ColumnFamily: []byte("l"), ColumnQualifier: []byte("lease"),
				ColumnVisibility: visibility, Timestamp: 2,
			},
			Value: []byte("value"),
		}},
		err: &accumulo.CleanupError{ScanID: 17, Err: errors.New("close failed")},
	}
	store := &AccumuloStore{scanner: scanner}
	cells, err := store.ScanPrefixFrom(
		context.Background(), prefix, prefix, []byte("l"), []byte("lease"), visibility, 1,
	)
	if err != nil || len(cells) != 1 || !bytes.Equal(cells[0].Value, []byte("value")) {
		t.Fatalf("ScanPrefixFrom = %#v, %v", cells, err)
	}
}

func TestAccumuloStoreMapsAllocatorOperationTimestamps(t *testing.T) {
	memory := newMemoryStore()
	client := newUninitializedClient(t, memory)
	if _, err := client.EnsureInitialized(context.Background(), initializeOptions(8)); err != nil {
		t.Fatal(err)
	}
	reservation := reserveOne(t, client, "txn")
	if _, _, err := client.Terminalize(context.Background(), reservation, coordination.StateAborted); err != nil {
		t.Fatal(err)
	}
	if _, err := client.AdvanceFrontier(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Retire(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(memory.captured) != 6 {
		t.Fatalf("captured %d mutations, want initialize/reserve/terminal/outcome/checkpoint/retire", len(memory.captured))
	}
	tests := []struct {
		name       string
		mutation   Mutation
		timestamps map[string]struct {
			timestamp int64
			deleted   bool
		}
		conditionTimestamps map[string]int64
	}{
		{"initialize", memory.captured[0], map[string]struct {
			timestamp int64
			deleted   bool
		}{"q/head": {1, false}}, nil},
		{"reserve", memory.captured[1], map[string]struct {
			timestamp int64
			deleted   bool
		}{"q/head": {2, false}, "r/" + string(coordination.U64(1)): {1, false}}, map[string]int64{"q/head": 1}},
		{"terminal", memory.captured[2], map[string]struct {
			timestamp int64
			deleted   bool
		}{"r/" + string(coordination.U64(1)): {2, false}}, map[string]int64{"r/" + string(coordination.U64(1)): 1}},
		{"outcome", memory.captured[3], map[string]struct {
			timestamp int64
			deleted   bool
		}{"o/terminal": {1, false}}, nil},
		{"checkpoint", memory.captured[4], map[string]struct {
			timestamp int64
			deleted   bool
		}{"q/head": {3, false}, "f/*": {1, false}}, map[string]int64{"q/head": 2}},
		{"retire", memory.captured[5], map[string]struct {
			timestamp int64
			deleted   bool
		}{"q/head": {4, false}, "r/" + string(coordination.U64(1)): {2, true}}, map[string]int64{
			"q/head": 3, "r/" + string(coordination.U64(1)): 2,
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			writer := &fakeAccumuloWriter{status: accumulo.ConditionalAccepted}
			store := &AccumuloStore{writer: writer}
			status, err := store.CompareAndMutate(context.Background(), test.mutation)
			if status != StatusAccepted || err != nil {
				t.Fatalf("CompareAndMutate = %v, %v", status, err)
			}
			entries, conditions := decodeConditionalMutation(t, writer.mutation)
			for key, want := range test.timestamps {
				found := false
				for _, entry := range entries {
					entryKey := string(entry.ColFamily) + "/" + string(entry.ColQualifier)
					if key == "f/*" && string(entry.ColFamily) == "f" {
						entryKey = key
					}
					if entryKey == key {
						found = true
						if entry.Timestamp != want.timestamp || entry.Deleted != want.deleted || !entry.HasTimestamp {
							t.Fatalf("%s = timestamp %d delete=%v explicit=%v", key, entry.Timestamp, entry.Deleted, entry.HasTimestamp)
						}
					}
				}
				if !found {
					t.Fatalf("missing update %q", key)
				}
			}
			for key, timestamp := range test.conditionTimestamps {
				found := false
				for _, condition := range conditions {
					conditionKey := string(condition.Cf) + "/" + string(condition.Cq)
					if conditionKey == key {
						found = true
						if !condition.HasTimestamp || condition.Ts != timestamp {
							t.Fatalf("%s condition timestamp = %d explicit=%v", key, condition.Ts, condition.HasTimestamp)
						}
					}
				}
				if !found {
					t.Fatalf("missing timestamped condition %q", key)
				}
			}
		})
	}
	writer := &fakeAccumuloWriter{
		status: accumulo.ConditionalUnknown,
		err:    errors.Join(accumulo.ErrConditionalUnknown, errors.New("initialization response lost")),
	}
	store := &AccumuloStore{writer: writer}
	status, err := store.CompareAndMutate(context.Background(), memory.captured[0])
	if status != StatusUnknown || !errors.Is(err, ErrConditionalUnknown) {
		t.Fatalf("initialization unknown mapping = %v, %v", status, err)
	}
}

func decodeConditionalMutation(t *testing.T, mutation *accumulo.ConditionalMutation) ([]cclient.MutationEntry, []*data.TCondition) {
	t.Helper()
	value := reflect.ValueOf(mutation).Elem()
	wireMutation := value.FieldByName("mutation").Elem()
	valuesField := wireMutation.FieldByName("Values")
	values := make([][]byte, valuesField.Len())
	for i := range values {
		values[i] = append([]byte(nil), valuesField.Index(i).Bytes()...)
	}
	wire := &data.TMutation{
		Row:    append([]byte(nil), wireMutation.FieldByName("Row").Bytes()...),
		Data:   append([]byte(nil), wireMutation.FieldByName("Data").Bytes()...),
		Values: values, Entries: int32(wireMutation.FieldByName("Entries").Int()),
	}
	decoded, err := cclient.FromThrift(wire)
	if err != nil {
		t.Fatal(err)
	}
	conditionValues := value.FieldByName("conditions")
	conditions := make([]*data.TCondition, conditionValues.Len())
	for i := range conditions {
		condition := conditionValues.Index(i)
		conditions[i] = &data.TCondition{
			Cf:           append([]byte(nil), condition.FieldByName("columnFamily").Bytes()...),
			Cq:           append([]byte(nil), condition.FieldByName("columnQualifier").Bytes()...),
			Cv:           append([]byte(nil), condition.FieldByName("columnVisibility").Bytes()...),
			Val:          append([]byte(nil), condition.FieldByName("value").Bytes()...),
			Ts:           condition.FieldByName("timestamp").Int(),
			HasTimestamp: condition.FieldByName("timestampSet").Bool(),
		}
	}
	return decoded.Entries(), conditions
}

func TestAccumuloStoreConditionalMappingAndUnknownClassification(t *testing.T) {
	row := []byte("allocator-row")
	visibility := []byte("CONTROL")
	writer := &fakeAccumuloWriter{
		status: accumulo.ConditionalUnknown,
		err:    errors.Join(accumulo.ErrConditionalUnknown, errors.New("response lost")),
	}
	store := &AccumuloStore{writer: writer}
	request := Mutation{
		Row: row,
		Conditions: []Condition{
			{Coordinate: Coordinate{Row: row, Family: []byte("q"), Qualifier: []byte("head"), Visibility: visibility}, Value: []byte("before")},
			{Coordinate: Coordinate{Row: row, Family: []byte("r"), Qualifier: []byte{0, 0, 0, 1}, Visibility: visibility}, Absent: true},
		},
		Updates: []Update{
			{Coordinate: Coordinate{Row: row, Family: []byte("q"), Qualifier: []byte("head"), Visibility: visibility}, Value: []byte("after"), Timestamp: 2},
			{Coordinate: Coordinate{Row: row, Family: []byte("r"), Qualifier: []byte{0, 0, 0, 1}, Visibility: visibility}, Delete: true, Timestamp: 1},
		},
	}
	status, err := store.CompareAndMutate(context.Background(), request)
	if status != StatusUnknown || !errors.Is(err, ErrConditionalUnknown) {
		t.Fatalf("CompareAndMutate = %v, %v", status, err)
	}
	if writer.mutation == nil || !bytes.Equal(writer.mutation.Row(), row) {
		t.Fatal("conditional mutation did not retain exact row")
	}
	value := reflect.ValueOf(writer.mutation).Elem()
	conditions := value.FieldByName("conditions")
	if conditions.Len() != 2 {
		t.Fatalf("condition count = %d", conditions.Len())
	}
	assertConditionShape(t, conditions.Index(0), "q", "head", "CONTROL", "before", true)
	assertConditionShape(t, conditions.Index(1), "r", string([]byte{0, 0, 0, 1}), "CONTROL", "", false)
	wire := value.FieldByName("mutation").Elem()
	if got := int(wire.FieldByName("Entries").Int()); got != 2 {
		t.Fatalf("update count = %d", got)
	}
	values := wire.FieldByName("Values")
	if values.Len() > 1 || values.Len() == 1 && string(values.Index(0).Bytes()) != "after" {
		t.Fatalf("put values = %#v", values)
	}
	data := wire.FieldByName("Data").Bytes()
	for _, component := range [][]byte{[]byte("q"), []byte("head"), visibility, []byte("after"), []byte("r"), {0, 0, 0, 1}} {
		if !bytes.Contains(data, component) {
			t.Fatalf("encoded update data omits %x: %x", component, data)
		}
	}
}

func assertConditionShape(t *testing.T, value reflect.Value, family, qualifier, visibility, expected string, valueSet bool) {
	t.Helper()
	if string(value.FieldByName("columnFamily").Bytes()) != family ||
		string(value.FieldByName("columnQualifier").Bytes()) != qualifier ||
		string(value.FieldByName("columnVisibility").Bytes()) != visibility ||
		value.FieldByName("valueSet").Bool() != valueSet ||
		string(value.FieldByName("value").Bytes()) != expected {
		t.Fatalf("condition shape differs: %#v", value)
	}
}
