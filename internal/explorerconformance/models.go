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

// Package explorerconformance provides a storage-neutral, public-value test
// harness for Explorer client implementations.
package explorerconformance

import (
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// FakeClock is an ordered sequence of fixture instants. Instants may move
// backward because publication order and source CreatedAt are distinct.
type FakeClock struct {
	Instants []time.Time
}

// Normalize clones the clock and canonicalizes each instant to UTC.
func (c FakeClock) Normalize() (FakeClock, error) {
	if len(c.Instants) == 0 {
		return FakeClock{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "fake clock requires an instant")
	}
	normalized := FakeClock{Instants: make([]time.Time, len(c.Instants))}
	for index, instant := range c.Instants {
		if instant.IsZero() {
			return FakeClock{}, shoal.NewError(
				shoal.ErrorInvalidArgument, "fake clock instants cannot be zero")
		}
		year := instant.UTC().Year()
		if year < 1 || year > 9999 {
			return FakeClock{}, shoal.NewError(
				shoal.ErrorInvalidArgument,
				"fake clock instant is outside the supported range",
			)
		}
		normalized.Instants[index] = instant.UTC()
	}
	return normalized, nil
}

// At returns one normalized fixture instant.
func (c FakeClock) At(index int) (time.Time, error) {
	normalized, err := c.Normalize()
	if err != nil {
		return time.Time{}, err
	}
	if index < 0 || index >= len(normalized.Instants) {
		return time.Time{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "fake clock index is outside the script")
	}
	return normalized.Instants[index], nil
}

// FaultPoint names an additive high-level operation boundary. It deliberately
// does not name storage rows, scanners, transactions, or recovery internals.
type FaultPoint string

const (
	FaultBeforeOperation FaultPoint = "before_operation"
	FaultAfterOperation  FaultPoint = "after_operation"
)

// FaultStep describes a future public-operation fault injection. The M1
// harness validates and orders scripts but does not inject faults.
type FaultStep struct {
	Order      uint32
	Point      FaultPoint
	Occurrence uint32
	Code       shoal.ErrorCode
}

// FaultScript is a deterministic high-level fault sequence.
type FaultScript struct {
	Steps []FaultStep
}

// Normalize clones, validates, and orders a fault script by explicit Order.
func (s FaultScript) Normalize() (FaultScript, error) {
	normalized := FaultScript{Steps: append([]FaultStep(nil), s.Steps...)}
	orders := make(map[uint32]struct{}, len(normalized.Steps))
	type occurrenceKey struct {
		point      FaultPoint
		occurrence uint32
	}
	occurrences := make(map[occurrenceKey]struct{}, len(normalized.Steps))
	for _, step := range normalized.Steps {
		if step.Order == 0 || step.Occurrence == 0 {
			return FaultScript{}, shoal.NewError(
				shoal.ErrorInvalidArgument,
				"fault order and occurrence must be positive",
			)
		}
		if _, duplicate := orders[step.Order]; duplicate {
			return FaultScript{}, shoal.NewError(
				shoal.ErrorInvalidArgument, "fault orders must be unique")
		}
		orders[step.Order] = struct{}{}
		if !utf8.ValidString(string(step.Point)) ||
			strings.TrimSpace(string(step.Point)) == "" {
			return FaultScript{}, shoal.NewError(
				shoal.ErrorInvalidArgument, "fault point must be valid nonblank UTF-8")
		}
		if err := shoal.ValidateSemanticString("fault point", string(step.Point)); err != nil {
			return FaultScript{}, err
		}
		if !validErrorCode(step.Code) {
			return FaultScript{}, shoal.NewError(
				shoal.ErrorInvalidArgument, "fault error code is unknown")
		}
		key := occurrenceKey{point: step.Point, occurrence: step.Occurrence}
		if _, duplicate := occurrences[key]; duplicate {
			return FaultScript{}, shoal.NewError(
				shoal.ErrorInvalidArgument,
				"fault point occurrence must be unique",
			)
		}
		occurrences[key] = struct{}{}
	}
	sort.Slice(normalized.Steps, func(i, j int) bool {
		return normalized.Steps[i].Order < normalized.Steps[j].Order
	})
	return normalized, nil
}

// WriterAuthorityMode identifies which adapter may accept writes. This model
// is fixture vocabulary only; it does not implement fencing or routing.
type WriterAuthorityMode string

const (
	WriterAuthorityWriteClosed     WriterAuthorityMode = "write_closed"
	WriterAuthorityEmbeddedPrimary WriterAuthorityMode = "embedded_primary"
	WriterAuthorityAccumuloPrimary WriterAuthorityMode = "accumulo_primary"
)

// WriterAuthority is one generation of high-level writer ownership.
type WriterAuthority struct {
	Generation uint64
	Mode       WriterAuthorityMode
	Holder     string
	Fence      uint64
}

// Validate checks one writer-authority fixture value.
func (a WriterAuthority) Validate() error {
	if a.Generation == 0 {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "writer authority generation must be positive")
	}
	switch a.Mode {
	case WriterAuthorityWriteClosed:
		if a.Holder != "" || a.Fence != 0 {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"write-closed authority cannot have a holder or fence",
			)
		}
	case WriterAuthorityEmbeddedPrimary, WriterAuthorityAccumuloPrimary:
		if !utf8.ValidString(a.Holder) || strings.TrimSpace(a.Holder) == "" ||
			a.Fence == 0 {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"primary writer authority requires a valid holder and fence",
			)
		}
		if err := shoal.ValidateSemanticString(
			"writer authority holder", a.Holder,
		); err != nil {
			return err
		}
	default:
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "writer authority mode is unknown")
	}
	return nil
}

// WriterAuthorityHistory is an ordered authority-generation fixture.
type WriterAuthorityHistory []WriterAuthority

// Normalize clones, validates, and orders authorities by generation.
func (h WriterAuthorityHistory) Normalize() (WriterAuthorityHistory, error) {
	normalized := append(WriterAuthorityHistory(nil), h...)
	generations := make(map[uint64]struct{}, len(normalized))
	for _, authority := range normalized {
		if err := authority.Validate(); err != nil {
			return nil, err
		}
		if _, duplicate := generations[authority.Generation]; duplicate {
			return nil, shoal.NewError(
				shoal.ErrorInvalidArgument,
				"writer authority generations must be unique",
			)
		}
		generations[authority.Generation] = struct{}{}
	}
	sort.Slice(normalized, func(i, j int) bool {
		return normalized[i].Generation < normalized[j].Generation
	})
	return normalized, nil
}

// FixtureControls groups additive clock, fault, and writer-authority concepts
// without imposing M2 authorization or M3 recovery behavior.
type FixtureControls struct {
	Clock       FakeClock
	Faults      FaultScript
	Authorities WriterAuthorityHistory
}

// Normalize independently owns and deterministically orders fixture controls.
func (c FixtureControls) Normalize() (FixtureControls, error) {
	clock, err := c.Clock.Normalize()
	if err != nil {
		return FixtureControls{}, err
	}
	faults, err := c.Faults.Normalize()
	if err != nil {
		return FixtureControls{}, err
	}
	authorities, err := c.Authorities.Normalize()
	if err != nil {
		return FixtureControls{}, err
	}
	return FixtureControls{
		Clock: clock, Faults: faults, Authorities: authorities,
	}, nil
}

func validErrorCode(code shoal.ErrorCode) bool {
	switch code {
	case shoal.ErrorInvalidArgument, shoal.ErrorNotFound, shoal.ErrorConflict,
		shoal.ErrorUnauthorized, shoal.ErrorUnavailable, shoal.ErrorCanceled,
		shoal.ErrorDeadline, shoal.ErrorInternal:
		return true
	default:
		return false
	}
}
