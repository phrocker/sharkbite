package main

import (
	"bytes"
	"fmt"
	"sync"
	"testing"

	"github.com/phrocker/shoal-oss/accumulo"
)

func TestOwnedDataDescriptorConstructionIsConcurrent(t *testing.T) {
	start := &accumulo.Key{
		Row:              []byte{'s', 0, 'r'},
		ColumnFamily:     []byte("cf"),
		ColumnQualifier:  []byte("cq"),
		ColumnVisibility: []byte("A&B"),
		Timestamp:        17,
	}
	end := &accumulo.Key{
		Row:              []byte{'t', 0, 'r'},
		ColumnFamily:     []byte("ef"),
		ColumnQualifier:  []byte("eq"),
		ColumnVisibility: []byte("C|D"),
		Timestamp:        23,
	}
	scanRange, err := accumulo.NewKeyRange(start, true, end, false)
	if err != nil {
		t.Fatal(err)
	}
	setting, err := accumulo.NewIteratorSetting(
		"age", "com.example.Age", 19,
		map[string]string{"zeta": "last", "alpha": "first"},
	)
	if err != nil {
		t.Fatal(err)
	}
	sharedRangeResult, err := buildRangeResult(scanRange, rangeBoundKey, rangeBoundKey)
	if err != nil {
		t.Fatal(err)
	}
	defer freeBuiltRangeResult(sharedRangeResult)
	sharedIteratorResult, err := buildIteratorSettingResult(setting)
	if err != nil {
		t.Fatal(err)
	}
	defer freeBuiltIteratorSettingResult(sharedIteratorResult)

	var wg sync.WaitGroup
	errors := make(chan error, 64)
	for worker := 0; worker < 64; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for read := 0; read < 16; read++ {
				rangeSnapshot, err := snapshotRangeResult(sharedRangeResult)
				if err != nil {
					errors <- err
					return
				}
				if err := validateRangeSnapshot(rangeSnapshot, start, end); err != nil {
					errors <- err
					return
				}
				iteratorSnapshot, err := snapshotIteratorSettingResult(sharedIteratorResult)
				if err != nil {
					errors <- err
					return
				}
				if err := validateIteratorSnapshot(iteratorSnapshot); err != nil {
					errors <- err
					return
				}
			}

			rangeResult, err := buildRangeResult(scanRange, rangeBoundKey, rangeBoundKey)
			if err != nil {
				errors <- err
				return
			}
			rangeSnapshot, err := snapshotRangeResult(rangeResult)
			freeBuiltRangeResult(rangeResult)
			if err != nil {
				errors <- err
				return
			}
			if err := validateRangeSnapshot(rangeSnapshot, start, end); err != nil {
				errors <- err
				return
			}

			iteratorResult, err := buildIteratorSettingResult(setting)
			if err != nil {
				errors <- err
				return
			}
			iteratorSnapshot, err := snapshotIteratorSettingResult(iteratorResult)
			freeBuiltIteratorSettingResult(iteratorResult)
			if err != nil {
				errors <- err
				return
			}
			if err := validateIteratorSnapshot(iteratorSnapshot); err != nil {
				errors <- err
			}
		}()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
}

func validateRangeSnapshot(snapshot rangeResultSnapshot, start, end *accumulo.Key) error {
	if snapshot.startKind != rangeBoundKey ||
		snapshot.endKind != rangeBoundKey ||
		!snapshot.hasStart || !snapshot.hasEnd ||
		!snapshot.startInclusive || snapshot.endInclusive ||
		!bytes.Equal(snapshot.start.Row, start.Row) ||
		!bytes.Equal(snapshot.start.ColumnFamily, start.ColumnFamily) ||
		!bytes.Equal(snapshot.start.ColumnQualifier, start.ColumnQualifier) ||
		!bytes.Equal(snapshot.start.ColumnVisibility, start.ColumnVisibility) ||
		snapshot.start.Timestamp != start.Timestamp ||
		!bytes.Equal(snapshot.end.Row, end.Row) ||
		!bytes.Equal(snapshot.end.ColumnFamily, end.ColumnFamily) ||
		!bytes.Equal(snapshot.end.ColumnQualifier, end.ColumnQualifier) ||
		!bytes.Equal(snapshot.end.ColumnVisibility, end.ColumnVisibility) ||
		snapshot.end.Timestamp != end.Timestamp {
		return fmt.Errorf("unexpected range snapshot: %#v", snapshot)
	}
	return nil
}

func validateIteratorSnapshot(snapshot iteratorSettingResultSnapshot) error {
	if snapshot.name != "age" ||
		snapshot.className != "com.example.Age" ||
		snapshot.priority != 19 ||
		snapshot.options["alpha"] != "first" ||
		snapshot.options["zeta"] != "last" {
		return fmt.Errorf("unexpected iterator snapshot: %#v", snapshot)
	}
	return nil
}
