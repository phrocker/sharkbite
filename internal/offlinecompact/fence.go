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

package offlinecompact

import (
	"context"
	"fmt"

	"github.com/phrocker/shoal-oss/internal/zk"
)

// FenceToken is an opaque continuity token for a table's OFFLINE guard.
// It is minted by StateFence.Fence and handed back to StateFence.Verify
// immediately before a commit. Verify trips (returns *FenceTrip) unless
// the table is still OFFLINE, the znode version is unchanged, and the ZK
// session is the same one that minted the token.
type FenceToken struct {
	// Offline records that the table was OFFLINE when the token was
	// minted. A token is only valid if this is true.
	Offline bool
	// Version is the state znode's data version at mint time. Any state
	// write (including an OFFLINE->ONLINE->OFFLINE round-trip) bumps it,
	// so an unchanged version is proof the fence held continuously.
	Version int32
	// Session is the ZK session id at mint time. A changed session means
	// the connection dropped and any watches are gone: fail closed.
	Session int64
}

// FenceTrip is returned by StateFence.Verify when the OFFLINE guard no
// longer holds. It is a hard stop: the commit must abort without touching
// metadata.
type FenceTrip struct {
	Reason string
}

func (e *FenceTrip) Error() string { return "offline fence tripped: " + e.Reason }

// StateFence guards a commit against the table being brought back ONLINE
// (and thus re-hosted by a tserver) between planning and metadata mutation.
//
// Fence performs the initial fenced read; Verify re-reads immediately
// before the commit and confirms nothing changed. Splitting the two lets
// the caller run the (potentially long) compaction between them while
// still catching any state transition at commit time.
type StateFence interface {
	// Fence reads the table state and returns a continuity token. It
	// errors if the table is not currently OFFLINE.
	Fence(ctx context.Context) (FenceToken, error)
	// Verify re-reads the state and returns *FenceTrip if the guard no
	// longer matches the minted token.
	Verify(ctx context.Context, minted FenceToken) error
}

// requireOffline validates a fresh table-state read for minting a token.
func requireOffline(r zk.TableStateResult, tableID string) (FenceToken, error) {
	if !r.Exists {
		return FenceToken{}, fmt.Errorf("table %s has no state znode (unknown table id?)", tableID)
	}
	if r.State != zk.TableStateOffline {
		return FenceToken{}, fmt.Errorf("table %s is %q, must be %s before offline compaction",
			tableID, r.State, zk.TableStateOffline)
	}
	return FenceToken{Offline: true, Version: r.Version}, nil
}

// verifyContinuity is the pure re-check: it compares a fresh read (and the
// current session id) against the minted token. Returns *FenceTrip on any
// mismatch so callers can distinguish a fence trip from a transport error.
func verifyContinuity(minted FenceToken, current zk.TableStateResult, currentSession int64) error {
	if !minted.Offline {
		return &FenceTrip{Reason: "token was never OFFLINE"}
	}
	if !current.Exists {
		return &FenceTrip{Reason: "state znode disappeared"}
	}
	if current.State != zk.TableStateOffline {
		return &FenceTrip{Reason: fmt.Sprintf("table is now %q (expected %s)", current.State, zk.TableStateOffline)}
	}
	if current.Version != minted.Version {
		return &FenceTrip{Reason: fmt.Sprintf("state znode version changed %d->%d (ONLINE round-trip?)", minted.Version, current.Version)}
	}
	if currentSession != minted.Session {
		return &FenceTrip{Reason: fmt.Sprintf("zk session changed %d->%d (connection dropped)", minted.Session, currentSession)}
	}
	return nil
}

// ZKTableFence is the production StateFence backed by a *zk.Locator.
type ZKTableFence struct {
	loc     *zk.Locator
	tableID string
}

// NewZKTableFence builds a StateFence for tableID over the given locator.
func NewZKTableFence(loc *zk.Locator, tableID string) *ZKTableFence {
	return &ZKTableFence{loc: loc, tableID: tableID}
}

// Fence implements StateFence.
func (f *ZKTableFence) Fence(ctx context.Context) (FenceToken, error) {
	r, err := f.loc.TableState(ctx, f.tableID)
	if err != nil {
		return FenceToken{}, fmt.Errorf("read table state: %w", err)
	}
	tok, err := requireOffline(r, f.tableID)
	if err != nil {
		return FenceToken{}, err
	}
	tok.Session = f.loc.SessionID()
	return tok, nil
}

// Verify implements StateFence.
func (f *ZKTableFence) Verify(ctx context.Context, minted FenceToken) error {
	r, err := f.loc.TableState(ctx, f.tableID)
	if err != nil {
		return fmt.Errorf("re-read table state: %w", err)
	}
	return verifyContinuity(minted, r, f.loc.SessionID())
}
