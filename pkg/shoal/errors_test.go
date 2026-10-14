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

package shoal_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type nonComparableCycleError struct {
	values []string
}

func (e nonComparableCycleError) Error() string {
	return "cycle"
}

func (e nonComparableCycleError) Unwrap() error {
	return e
}

func TestErrorCodeSurvivesWrapping(t *testing.T) {
	cause := errors.New("backend unavailable")
	err := shoal.WrapError(shoal.ErrorUnavailable, "knowledge store unavailable", cause)
	wrapped := fmt.Errorf("retrieve: %w", err)

	if !shoal.IsErrorCode(wrapped, shoal.ErrorUnavailable) {
		t.Fatalf("expected unavailable error, got %v", wrapped)
	}
	if !errors.Is(wrapped, cause) {
		t.Fatalf("expected wrapped cause %v, got %v", cause, wrapped)
	}
}

func TestIsErrorCodeTraversesNestedShoalErrors(t *testing.T) {
	inner := shoal.NewError(shoal.ErrorUnavailable, "backend unavailable")
	outer := shoal.WrapError(shoal.ErrorInternal, "retrieve failed", inner)

	if !shoal.IsErrorCode(outer, shoal.ErrorUnavailable) {
		t.Fatalf("expected nested unavailable error, got %v", outer)
	}
	if !shoal.IsErrorCode(outer, shoal.ErrorInternal) {
		t.Fatalf("expected outer internal error, got %v", outer)
	}
}

func TestIsErrorCodeTraversesJoinedErrors(t *testing.T) {
	err := errors.Join(
		shoal.NewError(shoal.ErrorNotFound, "document"),
		fmt.Errorf("remote: %w", shoal.NewError(shoal.ErrorConflict, "revision")),
	)

	if !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("expected joined conflict error, got %v", err)
	}
	if shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("unexpected unauthorized match in %v", err)
	}
}

func TestIsErrorCodeRejectsTypedNilError(t *testing.T) {
	var typedNil *shoal.Error
	var err error = typedNil

	if shoal.IsErrorCode(err, shoal.ErrorInternal) {
		t.Fatal("typed-nil error unexpectedly matched")
	}
}

func TestIsErrorCodeStopsAtNonComparableValueCycle(t *testing.T) {
	err := nonComparableCycleError{values: []string{"cycle"}}

	if shoal.IsErrorCode(err, shoal.ErrorInternal) {
		t.Fatal("cyclic error unexpectedly matched")
	}
}

func TestErrorCodeLiteralsAreStable(t *testing.T) {
	codes := map[shoal.ErrorCode]string{
		shoal.ErrorInvalidArgument: "invalid_argument",
		shoal.ErrorNotFound:        "not_found",
		shoal.ErrorConflict:        "conflict",
		shoal.ErrorUnauthorized:    "unauthorized",
		shoal.ErrorUnavailable:     "unavailable",
		shoal.ErrorCanceled:        "canceled",
		shoal.ErrorDeadline:        "deadline_exceeded",
		shoal.ErrorInternal:        "internal",
	}
	for code, literal := range codes {
		if string(code) != literal {
			t.Fatalf("error code %q changed from %q", code, literal)
		}
	}
}
