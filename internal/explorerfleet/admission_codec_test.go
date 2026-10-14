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

package explorerfleet

import (
	"crypto/sha256"
	"reflect"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
)

// TestAdmittedDeclarationSurvivesTheDurableCodec pins that what an admission
// declared is still on the record after a round trip.
//
// The declaration is part of the record's retry identity, and ApplyAction
// compares a freshly encoded record against the decoded stored one with
// reflect.DeepEqual. A field that is validated on the way in and dropped on
// encode would make every admission look like a different request on its next
// read, so a caller could never replay its own grant — and, worse, the record
// would stop saying what was admitted while still reading as a live token.
func TestAdmittedDeclarationSurvivesTheDurableCodec(t *testing.T) {
	digest := sha256.Sum256([]byte("declared-references"))
	for _, declared := range []fleet.Effects{
		nil,
		{fleet.EffectEgressesContent},
		{fleet.EffectEgressesContent, fleet.EffectReadsCorpus},
	} {
		record := testActionRecord()
		record.AdmittedEffects = declared
		if len(declared) > 0 {
			record.AdmittedDisclosures = digest[:]
			// The obligation rides the same round trip. It is the decision the
			// token was granted under, and a replay rebuilds the answer from
			// it, so losing it on encode would silently turn every retry into a
			// re-adjudication.
			record.AdmittedObligation = []byte{0b0000_0101}
			// The scheme marker rides the same round trip. It is what the
			// migration verdict reads instead of the identity, so losing it on
			// encode would make every new admission look like one written
			// under the superseded scheme and refuse the whole surface.
			record.AdmittedIdentityScheme =
				fleet.AdmittedIdentitySchemeDerived
		}
		encoded, err := encodeAction(record)
		if err != nil {
			t.Fatalf("encode %v: %v", declared, err)
		}
		decoded, err := decodeAction(encoded)
		if err != nil {
			t.Fatalf("decode %v: %v", declared, err)
		}
		if !reflect.DeepEqual(decoded, record) {
			t.Fatalf("declaration %v did not survive: %#v", declared, decoded)
		}
	}
}

// TestActionRecordsWrittenBeforeAdmissionDecodeUnchanged pins the upgrade.
//
// gob omits a zero-valued field, so the bytes an older build wrote for a
// dispatched action are exactly the bytes this build writes for one with no
// declaration. Those records must decode to an empty declaration rather than
// failing validation or acquiring one, or every action queued before this
// change would stop replaying on its idempotency key.
func TestActionRecordsWrittenBeforeAdmissionDecodeUnchanged(t *testing.T) {
	legacy := testActionRecord()
	encoded, err := encodeAction(legacy)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeAction(encoded)
	if err != nil {
		t.Fatalf("a record written before admission must still decode: %v", err)
	}
	if len(decoded.AdmittedEffects) != 0 ||
		len(decoded.AdmittedDisclosures) != 0 ||
		len(decoded.AdmittedObligation) != 0 ||
		decoded.AdmittedIdentityScheme != 0 {
		t.Fatalf("a dispatched action acquired a declaration: %#v", decoded)
	}
	if !reflect.DeepEqual(decoded, legacy) {
		t.Fatalf("round trip changed the record: %#v", decoded)
	}
}
