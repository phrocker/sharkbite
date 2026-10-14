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
	"context"
	"errors"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
)

type verificationSink struct {
	transform func([]TrustedCell) []TrustedCell
}

func (*verificationSink) WriteTrusted(context.Context, []TrustedCell) error { return nil }

func (s *verificationSink) ReadTrusted(_ context.Context, wanted []TrustedCell) ([]TrustedCell, error) {
	result := cloneTrustedCells(wanted)
	if s.transform != nil {
		result = s.transform(result)
	}
	return result, nil
}

func cloneTrustedCells(values []TrustedCell) []TrustedCell {
	result := make([]TrustedCell, len(values))
	for i, value := range values {
		result[i] = TrustedCell{
			Table: append([]byte(nil), value.Table...), Row: append([]byte(nil), value.Row...),
			Family: append([]byte(nil), value.Family...), Qualifier: append([]byte(nil), value.Qualifier...),
			Visibility: append([]byte(nil), value.Visibility...), Timestamp: value.Timestamp,
			Value: append([]byte(nil), value.Value...), Delete: value.Delete,
		}
	}
	return result
}

func verificationCells() []PhysicalCell {
	makeCell := func(id, value string) PhysicalCell {
		entry := coordination.ManifestEntry{
			Table: []byte("table"), Row: []byte("row-" + id), ColumnFamily: []byte("f"),
			ColumnQualifier: []byte("q-" + id), EpochSlot: coordination.EpochSlotContent,
			ValueLength: uint32(len(value)), ValueDigest: coordination.Sum([]byte(value)),
			LPART: coordination.LPART("policy"), CopyGeneration: 1,
			VisibilityDigest:   coordination.Sum([]byte("A")),
			LogicalDigest:      coordination.Sum([]byte("logical-" + id)),
			PhysicalCopyDigest: coordination.Sum([]byte("physical-" + id)),
		}
		return PhysicalCell{Entry: entry, Value: []byte(value), Visibility: []byte("A")}
	}
	return []PhysicalCell{
		makeCell("z", "value-z"),
		makeCell("a", "value-a"),
		makeCell("m", "value-m"),
	}
}

func TestAccumuloPhysicalVerifyUsesSetSemantics(t *testing.T) {
	tests := []struct {
		name      string
		transform func([]TrustedCell) []TrustedCell
		wantError bool
	}{
		{
			name: "different order",
			transform: func(values []TrustedCell) []TrustedCell {
				return []TrustedCell{values[1], values[2], values[0]}
			},
		},
		{
			name: "duplicate",
			transform: func(values []TrustedCell) []TrustedCell {
				return append(values, values[1])
			},
			wantError: true,
		},
		{
			name: "missing",
			transform: func(values []TrustedCell) []TrustedCell {
				return values[:len(values)-1]
			},
			wantError: true,
		},
		{
			name: "extra",
			transform: func(values []TrustedCell) []TrustedCell {
				extra := values[0]
				extra.Row = []byte("unexpected")
				return append(values, extra)
			},
			wantError: true,
		},
		{
			name: "timestamp mismatch",
			transform: func(values []TrustedCell) []TrustedCell {
				values[1].Timestamp++
				return values
			},
			wantError: true,
		},
		{
			name: "value digest mismatch",
			transform: func(values []TrustedCell) []TrustedCell {
				values[2].Value = []byte("different")
				return values
			},
			wantError: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			adapter, err := NewAccumuloPhysicalAdapter(&verificationSink{transform: test.transform})
			if err != nil {
				t.Fatal(err)
			}
			err = adapter.Verify(context.Background(), 9, verificationCells())
			if test.wantError != errors.Is(err, ErrInternal) {
				t.Fatalf("Verify error = %v, want internal=%v", err, test.wantError)
			}
		})
	}
}

func TestTrustedCellKeyIsUnambiguous(t *testing.T) {
	left := TrustedCell{Table: []byte("a\x00b"), Row: []byte("c"), Timestamp: 1}
	right := TrustedCell{Table: []byte("a"), Row: []byte("b\x00c"), Timestamp: 1}
	if trustedCellKey(left) == trustedCellKey(right) {
		t.Fatal("length-delimited physical keys collided")
	}
}

func TestAccumuloPhysicalVerifyRejectsManifestDigestMismatch(t *testing.T) {
	adapter, err := NewAccumuloPhysicalAdapter(&verificationSink{})
	if err != nil {
		t.Fatal(err)
	}
	cells := verificationCells()
	cells[1].Entry.ValueDigest = coordination.Sum([]byte("wrong"))
	if err := adapter.Verify(context.Background(), 9, cells); !errors.Is(err, ErrInternal) {
		t.Fatalf("manifest digest mismatch = %v", err)
	}
}

func TestMemoryPhysicalVerifyUsesSameSetSemantics(t *testing.T) {
	store := NewMemoryStore()
	cells := verificationCells()
	if err := store.Write(context.Background(), 9, cells); err != nil {
		t.Fatal(err)
	}
	reordered := []PhysicalCell{cells[2], cells[0], cells[1]}
	if err := store.Verify(context.Background(), 9, reordered); err != nil {
		t.Fatalf("reordered verification = %v", err)
	}
	duplicate := append(append([]PhysicalCell(nil), cells...), cells[0])
	if err := store.Verify(context.Background(), 9, duplicate); !errors.Is(err, ErrInternal) {
		t.Fatalf("duplicate expected key = %v", err)
	}
	store.DeletePhysical(9, cells[1])
	if err := store.Verify(context.Background(), 9, cells); !errors.Is(err, ErrInternal) {
		t.Fatalf("missing memory key = %v", err)
	}
}

func TestPhysicalTombstoneCommitmentAndVerification(t *testing.T) {
	emptyLength, emptyDigest, err := PhysicalValueCommitment(false, nil)
	if err != nil {
		t.Fatal(err)
	}
	deleteLength, deleteDigest, err := PhysicalValueCommitment(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if emptyLength != 0 || deleteLength != 0 || emptyDigest == deleteDigest {
		t.Fatalf(
			"empty/delete commitments = %d %x, %d %x",
			emptyLength,
			emptyDigest,
			deleteLength,
			deleteDigest,
		)
	}
	if _, _, err := PhysicalValueCommitment(
		true,
		[]byte("forbidden"),
	); !errors.Is(err, ErrInvalid) {
		t.Fatalf("valued tombstone = %v", err)
	}

	cell := verificationCells()[0]
	cell.Value = nil
	cell.Delete = true
	cell.Entry.ValueLength = deleteLength
	cell.Entry.ValueDigest = deleteDigest
	trusted := mapTrusted(9, []PhysicalCell{cell})
	if len(trusted) != 1 || !trusted[0].Delete {
		t.Fatalf("trusted tombstone mapping = %#v", trusted)
	}
	adapter, err := NewAccumuloPhysicalAdapter(&verificationSink{})
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.Verify(
		context.Background(),
		9,
		[]PhysicalCell{cell},
	); err != nil {
		t.Fatal(err)
	}
	adapter, err = NewAccumuloPhysicalAdapter(&verificationSink{
		transform: func(values []TrustedCell) []TrustedCell {
			values[0].Delete = false
			return values
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.Verify(
		context.Background(),
		9,
		[]PhysicalCell{cell},
	); !errors.Is(err, ErrInternal) {
		t.Fatalf("tombstone changed to empty value = %v", err)
	}
}
