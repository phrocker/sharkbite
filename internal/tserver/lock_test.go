// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//   https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package tserver

import (
	"errors"
	"math"
	"strings"
	"testing"
)

// TestLockIDValidReflectsRealLockNodes checks that only identities that could
// name an actual "zlock#<uuid>#<sequence>" node are trusted, matching the
// node parser in internal/zk. Anything else could never be a lock this
// process holds, so treating it as fencing authority would fence nothing.
func TestLockIDValidReflectsRealLockNodes(t *testing.T) {
	tests := []struct {
		name string
		in   LockID
		want bool
	}{
		{"held lock", LockID{UUID: serverUUID, Sequence: 3}, true},
		{"first sequence", LockID{UUID: serverUUID, Sequence: 0}, true},
		{"last sequence the counter can reach", LockID{UUID: serverUUID, Sequence: math.MaxInt32}, true},
		{"zero value", LockID{}, false},
		{"no uuid", LockID{Sequence: 3}, false},
		{"negative sequence", LockID{UUID: serverUUID, Sequence: -1}, false},
		{"sequence past the 32-bit counter", LockID{UUID: serverUUID, Sequence: math.MaxInt32 + 1}, false},
		{"not a uuid", LockID{UUID: "someone-else", Sequence: 3}, false},
		{"uuid carrying the node separator", LockID{UUID: "x#y", Sequence: 3}, false},
		{"truncated uuid", LockID{UUID: serverUUID[:20], Sequence: 3}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.in.Valid(); got != tt.want {
				t.Fatalf("Valid() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestAdoptLockRefusesUnusableIdentities is the behavioural consequence: an
// identity that cannot name a lock node never becomes fencing authority.
func TestAdoptLockRefusesUnusableIdentities(t *testing.T) {
	for _, bad := range []LockID{
		{},
		{UUID: "someone-else", Sequence: 3},
		{UUID: "x#y", Sequence: 3},
		{UUID: serverUUID, Sequence: -1},
		{UUID: serverUUID, Sequence: math.MaxInt32 + 1},
	} {
		host := NewHost()
		if err := host.AdoptLock(bad); !errors.Is(err, ErrInvalidLock) {
			t.Fatalf("AdoptLock(%q#%d): want ErrInvalidLock, got %v", bad.UUID, bad.Sequence, err)
		}
		if _, held := host.Lock(); held {
			t.Fatalf("AdoptLock(%q#%d) left a lock held", bad.UUID, bad.Sequence)
		}
	}
}

func TestLockIDString(t *testing.T) {
	if got, want := serverLock(7).String(), "zlock#"+serverUUID+"#0000000007"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
	if got, want := (LockID{}).String(), "zlock#<none>"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
	// A malformed identity keeps its raw fields so a refusal can be diagnosed.
	got := LockID{UUID: "someone-else", Sequence: 2}.String()
	if !strings.Contains(got, "invalid") || !strings.Contains(got, "someone-else") {
		t.Fatalf("String() = %q, want the raw identity", got)
	}
}

func TestLockIDSupersedes(t *testing.T) {
	older, newer := serverLock(3), serverLock(4)
	if !newer.Supersedes(older) {
		t.Fatal("a higher sequence must supersede a lower one")
	}
	if older.Supersedes(newer) || older.Supersedes(older) {
		t.Fatal("only a strictly higher sequence supersedes")
	}
	if !older.Equal(serverLock(3)) || older.Equal(newer) {
		t.Fatal("Equal must compare both holder and sequence")
	}
	if older.Equal(LockID{UUID: otherUUID, Sequence: older.Sequence}) {
		t.Fatal("the same sequence from another holder is not the same lock")
	}
}
