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

package shoal

import (
	"fmt"
	"reflect"
)

// ErrorCode identifies a stable transport-independent failure category.
//
// Invalid values use ErrorInvalidArgument; absence uses ErrorNotFound;
// optimistic concurrency and idempotency mismatches use ErrorConflict;
// whole-operation access denial uses ErrorUnauthorized; unsupported shapes,
// transient failures, and exhausted runtime budgets use ErrorUnavailable;
// cancellation and deadlines use their corresponding codes; and detectable
// committed-data corruption or implementation faults use ErrorInternal.
type ErrorCode string

const (
	ErrorInvalidArgument ErrorCode = "invalid_argument"
	ErrorNotFound        ErrorCode = "not_found"
	ErrorConflict        ErrorCode = "conflict"
	ErrorUnauthorized    ErrorCode = "unauthorized"
	ErrorUnavailable     ErrorCode = "unavailable"
	ErrorCanceled        ErrorCode = "canceled"
	ErrorDeadline        ErrorCode = "deadline_exceeded"
	ErrorInternal        ErrorCode = "internal"
)

// Error is a transport-independent Shoal failure.
type Error struct {
	Code    ErrorCode
	Message string
	cause   error
}

const maxErrorTraversalNodes = 10_000

// NewError constructs a public error without an underlying cause.
func NewError(code ErrorCode, message string) *Error {
	return &Error{Code: code, Message: message}
}

// WrapError constructs a public error that retains its underlying cause.
func WrapError(code ErrorCode, message string, cause error) *Error {
	return &Error{Code: code, Message: message, cause: cause}
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Message == "" {
		return string(e.Code)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// IsErrorCode reports whether err or any wrapped error has code.
func IsErrorCode(err error, code ErrorCode) bool {
	pending := []error{err}
	seenComparable := make(map[error]struct{})
	seenReference := make(map[errorReference]struct{})
	visited := 0
	for len(pending) > 0 {
		if visited >= maxErrorTraversalNodes {
			return false
		}
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if current == nil {
			continue
		}
		visited++
		if errorAlreadyVisited(current, seenComparable, seenReference) {
			continue
		}
		if shoalErr, ok := current.(*Error); ok && shoalErr != nil &&
			shoalErr.Code == code {
			return true
		}
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			pending = append(pending, wrapped.Unwrap()...)
		case interface{ Unwrap() error }:
			pending = append(pending, wrapped.Unwrap())
		}
	}
	return false
}

type errorReference struct {
	typ     reflect.Type
	pointer uintptr
}

func errorAlreadyVisited(
	err error,
	comparable map[error]struct{},
	references map[errorReference]struct{},
) bool {
	value := reflect.ValueOf(err)
	if value.Type().Comparable() {
		if _, ok := comparable[err]; ok {
			return true
		}
		comparable[err] = struct{}{}
		return false
	}
	var pointer uintptr
	switch value.Kind() {
	case reflect.Chan, reflect.Map, reflect.Pointer, reflect.Slice,
		reflect.UnsafePointer:
		pointer = uintptr(value.UnsafePointer())
	case reflect.Func:
		pointer = value.Pointer()
	default:
		return false
	}
	reference := errorReference{typ: value.Type(), pointer: pointer}
	if _, ok := references[reference]; ok {
		return true
	}
	references[reference] = struct{}{}
	return false
}
