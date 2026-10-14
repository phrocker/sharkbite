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

package explorer_test

import (
	"errors"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestIndeterminateCommitMarkerPreservesErrorChain(t *testing.T) {
	cause := errors.New("storage outcome unknown")
	public := shoal.WrapError(
		shoal.ErrorUnavailable, "write explorer record", cause)
	marked := explorer.MarkIndeterminateCommit(public)
	if !explorer.IsIndeterminateCommit(marked) {
		t.Fatal("marked error was not detected")
	}

	if !shoal.IsErrorCode(marked, shoal.ErrorUnavailable) {
		t.Fatalf("marked error lost public code: %v", marked)
	}
	if !errors.Is(marked, cause) {
		t.Fatalf("marked error lost cause: %v", marked)
	}
	if explorer.MarkIndeterminateCommit(nil) != nil {
		t.Fatal("nil error acquired a marker")
	}
	if explorer.IsIndeterminateCommit(errors.New("ordinary failure")) {
		t.Fatal("ordinary error reported indeterminate")
	}
}

func TestCommittedInteractionMarkerPreservesErrorChain(t *testing.T) {
	cause := errors.New("authorization generation changed after commit")
	public := shoal.WrapError(
		shoal.ErrorUnauthorized, "authorization changed", cause)
	marked := explorer.MarkCommittedInteraction(public)
	if !explorer.IsCommittedInteraction(marked) {
		t.Fatal("committed interaction error was not detected")
	}
	if !shoal.IsErrorCode(marked, shoal.ErrorUnauthorized) {
		t.Fatalf("committed interaction error lost public code: %v", marked)
	}
	if !errors.Is(marked, cause) {
		t.Fatalf("committed interaction error lost cause: %v", marked)
	}
	if explorer.MarkCommittedInteraction(nil) != nil {
		t.Fatal("nil error acquired a committed marker")
	}
}

// TestRefusedWithoutEffectMarkerIsStrict pins the #687 guarantee marker:
// detected through wrapping and kept with its code, but not through a join
// (which may carry a second failure) and never beside an indeterminate
// marker.
func TestRefusedWithoutEffectMarkerIsStrict(t *testing.T) {
	refusal := shoal.NewError(shoal.ErrorConflict, "refused")
	marked := explorer.MarkRefusedWithoutEffect(refusal)
	if !explorer.IsRefusedWithoutEffect(marked) ||
		!shoal.IsErrorCode(marked, shoal.ErrorConflict) ||
		!errors.Is(marked, refusal) || marked.Error() != refusal.Error() {
		t.Fatalf("marked refusal = %v", marked)
	}
	wrapped := shoal.WrapError(shoal.ErrorConflict, "upload", marked)
	if !explorer.IsRefusedWithoutEffect(wrapped) {
		t.Fatal("wrapping hid the marker")
	}
	for name, err := range map[string]error{
		"unmarked":      refusal,
		"nil":           nil,
		"joined":        errors.Join(marked, errors.New("rollback failed")),
		"wrapped join":  shoal.WrapError(shoal.ErrorConflict, "upload", errors.Join(marked)),
		"indeterminate": explorer.MarkIndeterminateCommit(marked),
		"marked indeterminate": explorer.MarkRefusedWithoutEffect(
			explorer.MarkIndeterminateCommit(refusal)),
	} {
		if explorer.IsRefusedWithoutEffect(err) {
			t.Fatalf("%s: %v reported as refused without effect", name, err)
		}
	}
	if explorer.MarkRefusedWithoutEffect(nil) != nil {
		t.Fatal("marking nil returned an error")
	}
}

// TestConcealRefusedWithoutEffectKeepsEverythingElse pins that concealing
// hides only the guarantee: text, code, errors.Is and the indeterminate
// marker all survive, and an unmarked error is returned as it is.
func TestConcealRefusedWithoutEffectKeepsEverythingElse(t *testing.T) {
	cause := errors.New("cause")
	refusal := shoal.WrapError(shoal.ErrorConflict, "refused", cause)
	concealed := explorer.ConcealRefusedWithoutEffect(
		explorer.MarkRefusedWithoutEffect(refusal))
	if explorer.IsRefusedWithoutEffect(concealed) ||
		!shoal.IsErrorCode(concealed, shoal.ErrorConflict) ||
		!errors.Is(concealed, cause) || concealed.Error() != refusal.Error() {
		t.Fatalf("concealed = %v", concealed)
	}
	if got := explorer.ConcealRefusedWithoutEffect(refusal); got != error(refusal) {
		t.Fatalf("an unmarked error was rewrapped: %v", got)
	}
	if explorer.ConcealRefusedWithoutEffect(nil) != nil {
		t.Fatal("concealing nil returned an error")
	}
}
