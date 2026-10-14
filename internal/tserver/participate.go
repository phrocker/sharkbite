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
	"context"
	"errors"
	"fmt"
)

// Participate holds one tablet-server ServiceLock generation on behalf of a
// Host, and keeps the two in step for exactly as long as the lock lasts.
//
// It acquires the lock, hands the generation to the host, watches it, and when
// the generation ends — lost, or given up because ctx ended — drops every
// tablet the host was claiming under it and passes them to release so the
// caller can close them.
//
// The order on the way out is deliberate: the host stops claiming tablets
// before the lock node is deleted. Deleting the node first would tell the
// manager it may place those tablets elsewhere while this process still
// claimed them.
//
// It covers one generation and returns when that generation is over. A process
// that means to rejoin builds a new ServiceLock and calls Participate again.
// Host.AdoptLock takes the new generation only when its sequence is above
// every generation that host has already used, which is what makes a rejoin
// distinguishable from a replay of the generation that just ended.
//
// That ordering is ZooKeeper's for as long as the lock directory survives, but
// it does not hold across one being deleted and recreated: the sequential
// counter lives on the parent, so a recreated directory hands out numbers from
// zero again. It is the same case ServiceLock.Verify reports as
// LossSuperseded, seen from the other side — a rejoin into a recreated
// directory can be handed a sequence the host has already used, and AdoptLock
// refuses it with ErrLockNotNewer.
//
// Recovering means a fresh Host and a fresh lock identity, and the second is
// the one that would be easy to miss. The high-water mark AdoptLock compares
// against is per-host state, so only a host that has used nothing can accept a
// generation numbered below the one it lost — but a host that has used nothing
// also has nothing left to tell two generations apart with. A process that
// rejoined under the same UUID after the counter restarted would be handed
// back the identity it already had: LockID is the UUID and the sequence, both
// match, and a manager request stamped with the generation that died would be
// indistinguishable from one stamped with the generation that is live. It
// cannot: NewServiceLock mints a UUID of its own for every ServiceLock, so the
// dead generation is nameable and therefore refusable, and because every
// descriptor has to carry its lock's UUID the advertisement is necessarily
// replaced along with it.
//
// A caller that rebuilds the lock without rebuilding the host has to read
// ErrLockNotNewer as that instruction rather than as a transient failure to
// retry, because retrying against the same host will refuse it every time.
//
// Returns an error wrapping ErrLockLost when the lock ended on its own, or
// ctx.Err() when the caller ended it. Both mean the same thing to the host:
// nothing is hosted here any more.
func Participate(
	ctx context.Context,
	lock *ServiceLock,
	host *Host,
	data ServiceLockData,
	release func([]Extent),
) error {
	if lock == nil {
		return errors.New("tserver: nil service lock")
	}
	if host == nil {
		return errors.New("tserver: nil host")
	}
	id, err := lock.Acquire(ctx, data)
	if err != nil {
		return err
	}
	if err := host.AdoptLock(id); err != nil {
		// The lock is held but the host will not fence with it, so this
		// process would sit in ZooKeeper looking like a live tablet server
		// while refusing everything the manager asked of it. Give the lock
		// back instead, so the manager can place the work on a server that
		// will take it.
		if releaseErr := lock.Release(); releaseErr != nil {
			return fmt.Errorf("adopt %s: %w (releasing it also failed: %w)", id, err, releaseErr)
		}
		return fmt.Errorf("adopt %s: %w", id, err)
	}

	maintainErr := lock.Maintain(ctx)

	dropped := host.LoseLock(id)
	if release != nil && len(dropped) > 0 {
		release(dropped)
	}
	if releaseErr := lock.Release(); releaseErr != nil {
		// Both facts matter: why the generation ended, and that its node may
		// still be in ZooKeeper. Joining them keeps errors.Is working for the
		// first while the second stays visible to an operator.
		return errors.Join(maintainErr, releaseErr)
	}
	return maintainErr
}
