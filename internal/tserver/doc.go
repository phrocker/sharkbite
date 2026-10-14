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

// Package tserver holds the Accumulo tablet-server participation logic: the
// state a Shoal process must keep so an unmodified Accumulo manager can
// assign, monitor, migrate, and unassign the tablets it hosts.
//
// The manager is the only assignment authority. Nothing here decides what to
// host: Host is a reactive state machine that applies manager-directed
// transitions, rejects the ones that cannot be safely applied, and reports
// what it currently hosts. It never assigns, migrates, or unassigns a tablet
// on its own, and it never overrides a live manager's decision. The live
// manager lock that defines that authority is observed externally rather than
// inferred from request history.
//
// What it does own is the fence. Every transition carries the ServiceLock
// generation it was minted under, and anything that cannot be proven current
// fails closed — a stale assignment is refused rather than applied, because a
// wrongly accepted assignment means a multiply hosted tablet. See
// docs/tserver-hosting-lifecycle.md for the state machine and fencing rules.
//
// The generation is not invented here either. ServiceLock implements
// Accumulo's lock protocol against ZooKeeper — the ephemeral sequential
// zlock node, the service descriptors the manager reads, and the loss
// detection that ends a generation — and Participate is what binds a real
// ZooKeeper generation to a Host, so the fence is stamped with a lock this
// process demonstrably holds. WatchManagerLock supplies the other half by
// reading the manager's own lock, which is how manager authority reaches a
// Host without ever being asserted by a caller.
package tserver
