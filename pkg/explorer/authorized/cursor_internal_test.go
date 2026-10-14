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

package authorized

import (
	"bytes"
	"testing"
)

// TestChangeCursorSealUsesFreshNoncePerCall pins the invariant the whole AEAD
// construction rests on: a fresh random nonce for every seal.
//
// This must never be "optimised" into a deterministic seal. AES-GCM requires a
// unique nonce per (key, message); reusing one is the "forbidden attack" that
// leaks the GHASH authentication subkey and lets an attacker forge arbitrary
// authenticated ciphertexts -- which would reopen the cursor-forgery oracle the
// sealed cursor exists to close. Determinism would also reopen the differencing
// oracle: if (incarnation, sequence) always sealed to the same token, a caller
// could build a token->sequence dictionary by advancing one visible change at a
// time and then read every future cursor by lookup. So sealing the same
// position twice MUST produce different tokens, and both MUST still open to that
// same position. If a future change wants deterministic cursors (for response
// caching, test stability, or token de-duplication), this test is why it cannot
// have them.
func TestChangeCursorSealUsesFreshNoncePerCall(t *testing.T) {
	sealer, err := newCursorSealer(bytes.Repeat([]byte{0x2a}, 32))
	if err != nil {
		t.Fatal(err)
	}

	const incarnation = "corpus-incarnation-fixed"
	const sequence = uint64(7)

	first, err := sealer.seal(incarnation, sequence)
	if err != nil {
		t.Fatal(err)
	}
	second, err := sealer.seal(incarnation, sequence)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("sealing the same position twice produced identical tokens; " +
			"the nonce is being reused, which breaks AES-GCM confidentiality " +
			"and integrity")
	}

	// Both tokens, though distinct, must decrypt to the identical position: the
	// nonce varies the ciphertext without changing the resume point.
	seq1, inc1, err := sealer.open(first)
	if err != nil {
		t.Fatalf("open first token: %v", err)
	}
	seq2, inc2, err := sealer.open(second)
	if err != nil {
		t.Fatalf("open second token: %v", err)
	}
	if seq1 != sequence || seq2 != sequence {
		t.Fatalf("opened sequences = %d,%d want %d,%d",
			seq1, seq2, sequence, sequence)
	}
	if inc1 != incarnation || inc2 != incarnation {
		t.Fatalf("opened incarnations = %q,%q want %q",
			inc1, inc2, incarnation)
	}
}
