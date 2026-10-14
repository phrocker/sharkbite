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

package explorerconformance_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/explorerconformance"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestFakeClockNormalizesWithoutReordering(t *testing.T) {
	future := time.Date(
		2099, time.January, 2, 3, 4, 5, 0, time.FixedZone("future", 3600))
	past := time.Date(
		2001, time.January, 2, 3, 4, 5, 0, time.FixedZone("past", -3600))
	clock := explorerconformance.FakeClock{
		Instants: []time.Time{future, past},
	}
	normalized, err := clock.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if len(normalized.Instants) != 2 ||
		normalized.Instants[0].Location() != time.UTC ||
		normalized.Instants[1].Location() != time.UTC ||
		!normalized.Instants[0].After(normalized.Instants[1]) {
		t.Fatalf("normalized clock = %#v", normalized)
	}
	normalized.Instants[0] = time.Time{}
	if clock.Instants[0].IsZero() {
		t.Fatal("Normalize mutated the caller clock")
	}
	if _, err := (explorerconformance.FakeClock{}).Normalize(); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("empty clock error = %v", err)
	}
}

func TestFaultScriptValidationAndDeterministicOrdering(t *testing.T) {
	script := explorerconformance.FaultScript{Steps: []explorerconformance.FaultStep{
		{
			Order: 2, Point: explorerconformance.FaultAfterOperation,
			Occurrence: 1, Code: shoal.ErrorUnavailable,
		},
		{
			Order: 1, Point: explorerconformance.FaultBeforeOperation,
			Occurrence: 1, Code: shoal.ErrorInternal,
		},
	}}
	normalized, err := script.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if normalized.Steps[0].Order != 1 || normalized.Steps[1].Order != 2 {
		t.Fatalf("fault order = %#v", normalized.Steps)
	}
	if script.Steps[0].Order != 2 {
		t.Fatal("Normalize mutated the caller script")
	}
	duplicate := script
	duplicate.Steps = append([]explorerconformance.FaultStep(nil), script.Steps...)
	duplicate.Steps[1].Order = 2
	if _, err := duplicate.Normalize(); !shoal.IsErrorCode(
		err, shoal.ErrorInvalidArgument,
	) {
		t.Fatalf("duplicate order error = %v", err)
	}
}

func TestWriterAuthorityValidationAndDeterministicOrdering(t *testing.T) {
	history := explorerconformance.WriterAuthorityHistory{
		{
			Generation: 2,
			Mode:       explorerconformance.WriterAuthorityAccumuloPrimary,
			Holder:     "accumulo-adapter",
			Fence:      9,
		},
		{
			Generation: 1,
			Mode:       explorerconformance.WriterAuthorityEmbeddedPrimary,
			Holder:     "embedded-adapter",
			Fence:      4,
		},
	}
	normalized, err := history.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if normalized[0].Generation != 1 || normalized[1].Generation != 2 {
		t.Fatalf("authority order = %#v", normalized)
	}
	if history[0].Generation != 2 {
		t.Fatal("Normalize mutated caller authority history")
	}
	if _, err := (explorerconformance.WriterAuthorityHistory{{
		Generation: 1,
		Mode:       explorerconformance.WriterAuthorityWriteClosed,
		Holder:     "unexpected",
	}}).Normalize(); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("write-closed holder error = %v", err)
	}
}

func TestFixtureControlsOwnNestedSlices(t *testing.T) {
	controls := explorerconformance.FixtureControls{
		Clock: explorerconformance.FakeClock{Instants: []time.Time{
			time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC),
		}},
		Faults: explorerconformance.FaultScript{Steps: []explorerconformance.FaultStep{{
			Order: 1, Point: explorerconformance.FaultBeforeOperation,
			Occurrence: 1, Code: shoal.ErrorUnavailable,
		}}},
		Authorities: explorerconformance.WriterAuthorityHistory{{
			Generation: 1,
			Mode:       explorerconformance.WriterAuthorityEmbeddedPrimary,
			Holder:     "embedded",
			Fence:      1,
		}},
	}
	normalized, err := controls.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	want, err := controls.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	normalized.Clock.Instants[0] = time.Time{}
	normalized.Faults.Steps[0].Order = 2
	normalized.Authorities[0].Generation = 2
	again, err := controls.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again, want) {
		t.Fatalf("controls changed through normalized aliases: %#v != %#v", again, want)
	}
}
