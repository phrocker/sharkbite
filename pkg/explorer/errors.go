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

package explorer

import (
	"errors"

	"github.com/phrocker/shoal-oss/pkg/interaction"
)

type indeterminateCommitError struct {
	cause error
}

func (e *indeterminateCommitError) Error() string {
	return e.cause.Error()
}

func (e *indeterminateCommitError) Unwrap() error {
	return e.cause
}

// MarkIndeterminateCommit marks a storage error whose durable commit outcome
// cannot be determined. The marker preserves the original error and its public
// Shoal error code through normal unwrapping.
func MarkIndeterminateCommit(err error) error {
	if err == nil || IsIndeterminateCommit(err) {
		return err
	}
	return &indeterminateCommitError{cause: err}
}

// IsIndeterminateCommit reports whether an error chain contains an
// indeterminate durable-commit marker.
func IsIndeterminateCommit(err error) bool {
	var marked *indeterminateCommitError
	return errors.As(err, &marked)
}

type committedInteractionError struct {
	cause error
}

func (e *committedInteractionError) Error() string {
	return e.cause.Error()
}

func (e *committedInteractionError) Unwrap() error {
	return e.cause
}

// MarkCommittedInteraction marks a post-commit failure: the interaction is
// durable even though a later authorization/generation check prevented the
// caller from receiving success. Callers must not retry the same stable ID as
// though the write rolled back.
func MarkCommittedInteraction(err error) error {
	if err == nil || IsCommittedInteraction(err) {
		return err
	}
	return interaction.MarkCommittedRecord(err)
}

// IsCommittedInteraction reports whether an error occurred after the durable
// interaction write completed.
func IsCommittedInteraction(err error) bool {
	var marked *committedInteractionError
	return errors.As(err, &marked) || interaction.IsCommittedRecord(err)
}

type refusedWithoutEffectError struct {
	cause error
}

func (e *refusedWithoutEffectError) Error() string {
	return e.cause.Error()
}

func (e *refusedWithoutEffectError) Unwrap() error {
	return e.cause
}

// MarkRefusedWithoutEffect marks a refusal after which its producer
// guarantees that the call left nothing committed: no base write, no claim,
// no registration (#687). An audit record of the attempt, such as the
// authorized client's declassification intent, is not an effect on the
// document in this sense. A batch caller may then report the refusal for
// that item and go on with the rest. Only the producer that performed the
// rollback may apply it; the marker preserves the original error and its
// public Shoal error code through normal unwrapping.
func MarkRefusedWithoutEffect(err error) error {
	if err == nil {
		return nil
	}
	return &refusedWithoutEffectError{cause: err}
}

// IsRefusedWithoutEffect reports whether err carries the
// MarkRefusedWithoutEffect guarantee and nothing contradicts it. It is
// strict: the marker must be reached through single-error unwrapping only,
// since an error joined into the chain (errors.Join) may report a second
// failure the guarantee does not cover, and any indeterminate-commit marker
// in the chain wins.
func IsRefusedWithoutEffect(err error) bool {
	if err == nil || IsIndeterminateCommit(err) {
		return false
	}
	for current := err; current != nil; {
		if _, ok := current.(*refusedWithoutEffectError); ok {
			return true
		}
		// A joined error has only Unwrap() []error, so the walk ends there.
		single, ok := current.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		current = single.Unwrap()
	}
	return false
}

// concealedRefusalError hides a MarkRefusedWithoutEffect guarantee from
// IsRefusedWithoutEffect while keeping the error's text, code and every
// errors.Is / errors.As match: it unwraps only as a multi-error, where that
// walk stops.
type concealedRefusalError struct {
	causes [1]error
}

func (e *concealedRefusalError) Error() string { return e.causes[0].Error() }

func (e *concealedRefusalError) Unwrap() []error { return e.causes[:] }

// ConcealRefusedWithoutEffect returns err so that IsRefusedWithoutEffect
// reports false for it, changing nothing else. A producer applies it to an
// error it passes on but did not mark itself (#687), and an aggregate applies
// it where one item's guarantee does not cover the whole.
func ConcealRefusedWithoutEffect(err error) error {
	if !IsRefusedWithoutEffect(err) {
		return err
	}
	return &concealedRefusalError{causes: [1]error{err}}
}
