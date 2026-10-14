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
	"fmt"
	"math"
)

// LockID identifies one held Accumulo ServiceLock. It mirrors the ephemeral
// ZooKeeper node a lock holder creates — "zlock#<uuid>#<sequence>" — where
// UUID is the holder's lock identity and Sequence is the ZooKeeper sequence
// number of the node.
//
// Sequence is the generation counter the fence relies on. ZooKeeper hands out
// strictly increasing sequence numbers per parent znode, so a lock that was
// lost and re-acquired always carries a higher sequence than the one it
// replaced, and a request stamped with a lower sequence is provably stale.
type LockID struct {
	UUID     string
	Sequence int64
}

// Valid reports whether the lock identity could name a real ServiceLock node,
// and so is usable for fencing.
//
// The checks are the ones Accumulo's ServiceLock.validateAndSort makes: the
// UUID must be the 36-character dashed form Java's UUID.fromString accepts,
// and the sequence must fit the signed 32-bit counter it reads with
// Integer.parseInt. internal/zk's node parser is looser — it takes whatever
// uuid.Parse accepts, including the undashed and URN spellings Java rejects —
// so this is parity with Accumulo rather than with that parser.
// An identity outside that shape could never appear as a "zlock#<uuid>#<seq>"
// node, so it cannot be a lock this process holds — trusting it as fencing
// authority would be fencing against nothing.
func (l LockID) Valid() bool {
	if l.Sequence < 0 || l.Sequence > math.MaxInt32 {
		return false
	}
	return validAccumuloUUID(l.UUID)
}

// Equal reports whether two lock identities are the same held lock.
func (l LockID) Equal(other LockID) bool {
	return l.UUID == other.UUID && l.Sequence == other.Sequence
}

// Supersedes reports whether l is a later generation than other.
func (l LockID) Supersedes(other LockID) bool {
	return l.Sequence > other.Sequence
}

// String renders the lock in its ZooKeeper node form. An identity that could
// not name a real lock node keeps its raw fields so a refusal can be
// diagnosed from the error text.
func (l LockID) String() string {
	switch {
	case l.Valid():
		return fmt.Sprintf("zlock#%s#%010d", l.UUID, l.Sequence)
	case l.UUID == "" && l.Sequence == 0:
		return "zlock#<none>"
	default:
		return fmt.Sprintf("zlock#<invalid %q#%d>", l.UUID, l.Sequence)
	}
}

// Fence is the authority stamp that must accompany every manager-directed
// lifecycle transition.
//
// Server is the tablet-server ServiceLock the manager believed this process
// held when it made the decision. It must match the lock actually held now:
// a request minted against an earlier generation was made against a view of
// the cluster that no longer exists, and applying it could re-host a tablet
// the manager has since given to somebody else.
//
// Manager is the ServiceLock of the manager that issued the request. It exists
// to keep a superseded manager from countermanding the live one — not to
// second-guess the live manager, whose decisions are always applied. The live
// manager lock is observed externally, and requests are only accepted when
// this lock matches that authoritative observation.
type Fence struct {
	Server  LockID
	Manager LockID
}
