<!--

    Licensed to the Apache Software Foundation (ASF) under one
    or more contributor license agreements.  See the NOTICE file
    distributed with this work for additional information
    regarding copyright ownership.  The ASF licenses this file
    to you under the Apache License, Version 2.0 (the
    "License"); you may not use this file except in compliance
    with the License.  You may obtain a copy of the License at

      https://www.apache.org/licenses/LICENSE-2.0

    Unless required by applicable law or agreed to in writing,
    software distributed under the License is distributed on an
    "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
    KIND, either express or implied.  See the License for the
    specific language governing permissions and limitations
    under the License.

-->
# Tablet hosting lifecycle and fencing

Status: **write-capable process wired**. The fenced lifecycle state machine
(`internal/tserver`) has landed, and so has the ZooKeeper registration
that makes it visible to a manager: the tablet-server ServiceLock, the
service descriptors the manager reads, and the manager-lock observation
that supplies live manager authority — see [§6](#6-registering-with-zookeeper).
The manager RPC adapter (`internal/tserverrpc`) now maps Accumulo 4
assignment, unload, flush, status, and halt calls onto that fence and
reports asynchronous lifecycle outcomes. Tablet metadata loading and the
hosted-tablet specification loader are now present. `cmd/shoal-tserver`
connects them to hosted stateful scans and WAL-backed ingest, one reusable
authenticated ZooKeeper session, manager failover discovery/reporting, and
bounded drain. Production conditional metadata CAS fences ownership, WAL
references, minor-compaction file installation, and unload. See
[§9](#9-ingest-authority). Tracking issue: #67.

## 1. Goal

Let an unmodified Accumulo manager assign, monitor, migrate, and unassign
tablets hosted by a Shoal process, without ever letting a tablet end up
hosted in two places at once.

`internal/tserver.Host` is the piece that holds that invariant. It tracks
which tablets this process claims and refuses any transition it cannot
prove is current.

## 2. The manager is the only authority

Nothing in `internal/tserver` decides what to host. `Host` has no policy,
no balancer, and no way to give itself a tablet: `Assign` and `Unassign`
exist only to apply what the manager asked for, and the manager's
decisions are always applied when they can be applied safely.

The one thing `Host` will refuse a manager is a request it can prove came
from a **superseded** manager — see the manager fence in
[§4](#4-the-fence). That is not a competing authority; it is how the live
manager's authority is protected from a predecessor that has not noticed
it lost the lock.

## 3. States

The happy path is one line — assigned, loaded, drained, released:

```
              Assign      LoadComplete   Unassign(graceful)  UnloadComplete
 UNASSIGNED ──────────► LOADING ──────► HOSTED ───────────► UNLOADING ──────────► UNASSIGNED
```

| State | Meaning | Routable |
| --- | --- | --- |
| `UNASSIGNED` | not tracked here (the zero value, so an unknown extent reports as this) | no |
| `LOADING` | assigned by the manager, being brought online | no |
| `HOSTED` | online and serving | **yes** |
| `UNLOADING` | draining before release; still claimed, so not re-assignable | no |

Every transition, including the ones that leave the happy path:

| From | Call | To |
| --- | --- | --- |
| `UNASSIGNED` | `Assign` | `LOADING` |
| `LOADING` | `LoadComplete` | `HOSTED` |
| `LOADING` | `LoadFailed` | `UNASSIGNED` |
| `LOADING` | `Unassign(UnloadGraceful)` | `UNLOADING` |
| `HOSTED` | `Unassign(UnloadGraceful)` | `UNLOADING` |
| `UNLOADING` | `UnloadComplete` | `UNASSIGNED` |
| any assigned state | `Unassign(UnloadImmediate)` | `UNASSIGNED` |
| any assigned state | `LoseLock` (all tablets at once) | `UNASSIGNED` |

`Hosted()` returns only `HOSTED` tablets — the set the manager may route
to. `Tablets()` returns every tracked tablet with its state, so a slow
handoff can be told apart from a stuck one.

Two paths deliberately do **not** exist. A tablet cannot go from
`LOADING` straight to `HOSTED` after the manager unassigned it: the
unassignment moves it to `UNLOADING`, `LoadComplete` then fails with
`ErrWrongState`, and the loader releases it with `UnloadComplete` rather
than publishing a tablet the manager has already placed elsewhere. And a
tablet cannot be re-assigned while it is `UNLOADING`, because this
process has not let go of it yet.

Each trip through this table is one *attempt*, identified by the handle
`Assign` returns. A tablet that reaches `UNASSIGNED` and is assigned here
again starts a new attempt, and the completions of the old one no longer
apply to it — see §4.

## 4. The fence

Every manager-directed transition carries a `Fence` of two ServiceLock
identities. A `LockID` mirrors the ephemeral ZooKeeper node a lock holder
creates — `zlock#<uuid>#<sequence>` — and the sequence is the generation
counter: ZooKeeper hands out strictly increasing sequence numbers, so a
lock that was lost and re-acquired always carries a higher one.

This is the Accumulo-connected implementation of the repository-wide
[coordination authority model](./coordination-authority.md): ZooKeeper and the
manager remain the sole coordination authority for hosted tablets. A
Kubernetes Lease may coordinate a separate Shoal-only table, but it must never
mirror or supplement ownership of an Accumulo-hosted tablet.

**Server lock** — the tablet-server lock the manager believed this
process held. It must equal the lock held right now. An older generation
was minted against a view of the cluster that no longer exists; a
generation we do not hold cannot be verified at all. Both are refused.

**Manager lock** — the lock held by the manager that issued the request.
The live manager lock is observed externally from ZooKeeper, and requests
are accepted only when this lock exactly matches that authoritative live
manager identity. A delayed predecessor RPC is therefore stale as soon as
the live-manager observation moves on, even before the successor has sent
this host any request. If live-manager knowledge is temporarily cleared,
requests fail closed until a manager lock is observed again — but the
highest manager epoch already seen is retained, so re-observation cannot
roll authority backward to an older epoch.

Local completions (`LoadComplete`, `LoadFailed`, `UnloadComplete`) carry
an `Attempt` instead: an opaque handle minted by `Assign`, naming one
assignment of one tablet under one server lock. They report the outcome
of work this process started, and what matters is whether that work
still belongs to us.

The server lock alone is not fine-grained enough to decide that. The
manager may unassign a tablet and assign it here again without the lock
ever changing — a migration that is rolled back, or a reassignment after
a forced unload — so a completion left over from the first assignment
would find the extent, the lock and even the state all matching a second
time. `LoadFailed` would release a tablet that is still loading, and
`LoadComplete` would publish one whose data is not loaded yet. The
attempt id is minted per assignment and never reused, so a completion
can only ever apply to the assignment it belongs to; anything else is
`ErrStaleAttempt`.

A graceful `Unassign` hands the same attempt back, so the drain it starts
can be finished with `UnloadComplete`, and a load that was unassigned
mid-flight can give the tablet back with the handle it already holds.
When there is nothing to finish — the tablet was not tracked, or
`UnloadImmediate` released it outright — the returned attempt is invalid.

### Fail closed, everywhere

Every refusal leaves hosting state exactly as it was, so a non-nil error
from any transition means nothing moved:

| Situation | Result |
| --- | --- |
| no lock held | `ErrNoLock` |
| stale or unheld server lock | `ErrStaleServerLock` |
| superseded or missing manager lock | `ErrStaleManagerLock` |
| tablet already assigned here | `ErrAlreadyAssigned` |
| extent overlaps an assigned tablet | `ErrOverlapping` |
| malformed extent | `ErrInvalidExtent` |
| completion for a tablet this host does not track | `ErrNotAssigned` |
| completion for an assignment that has already ended | `ErrStaleAttempt` |
| completion for a tablet in another state | `ErrWrongState` |
| unload mode this host does not implement | `ErrInvalidUnloadMode` |

`AdoptLock` fails closed the same way, leaving the lock held — and so the
tablets tracked under it — untouched:

| Situation | Result |
| --- | --- |
| lock identity that could not name a `zlock` node | `ErrInvalidLock` |
| lock no newer than one already used | `ErrLockNotNewer` |
| tablets still tracked from an earlier generation | `ErrTabletsAssigned` |

`ErrNotAssigned`, `ErrStaleAttempt` and `ErrWrongState` are deliberately
distinct: the first says the host never had the tablet (or has already
released it), the second says it has the extent but as a later assignment
that this completion knows nothing about, and the third says it has the
right assignment but the completion arrived for the wrong phase. Only the
third is worth retrying.

`ErrNotAssigned` and `ErrStaleAttempt` both count as `RejectedStale`: only
`Assign` mints attempts, so a handle that finds nothing tracked named a
tablet this host really had, and its assignment ended before the
completion arrived.

Overlap is what catches stale split metadata. A parent extent arriving
after its children are assigned — or a child arriving after its parent —
would host the same rows twice, so it is refused even though the extent
itself is not a duplicate.

Overlap is checked against the two neighbouring tablets of the extent's
place in a per-table range index, not against every tablet the host has.
Tracked extents in a table never overlap each other, so those two are the
only ones that can reach it — which keeps loading N tablets at
N log N comparisons instead of N².

The manager-facing `Unassign` is the one deliberate exception, but only
for an extent that is absent *and* has no overlapping coverage still
tracked here. In that case the requested end state already holds:
unassigning a tablet this host does not have succeeds without changing
anything. A stale split/merge parent or child is not treated as
idempotent, because returning success while another extent still covers
those rows would let the manager place overlapping coverage elsewhere.
The fence is still checked first, so a superseded manager cannot
unassign anything.

A row bound is *absent* when it is nil or empty; the two are the same
bound. That matches `cclient.KeyExtent` and Accumulo's Java `KeyExtent`,
where a null and an empty `Text` collapse on the wire. If the two were
kept distinct, one tablet could arrive under two identities — assigned
with a nil bound and unassigned with an empty one — and `Unassign`
would report success without ever releasing it.

## 5. Lock loss and restart

`LoseLock` drops **every** tracked tablet at once and returns them so the
caller can close them. Once the lock is gone the manager is free to place
those tablets elsewhere, so the host stops claiming them immediately
rather than waiting to be told. Any work still in flight then fails
closed, because its server lock no longer matches.

The notification is fenced to its own generation: `LoseLock` takes the
`LockID` that was lost and does nothing unless that is still the lock
held. A delayed or duplicated watcher callback for a generation that has
already been replaced would otherwise tear down its successor and drop
tablets the manager believes are being served.

`AdoptLock` refuses a generation that is not strictly newer than any this
host has already used — including ones released by `LoseLock`, whose
sequence is remembered for exactly this reason — and refuses to run at
all while tablets are still assigned. A restarted or reconnected process
therefore always starts from an empty hosted set under a fresh
generation, which is what makes "process restart and ServiceLock loss do
not leave a tablet multiply hosted" hold.

The lock the rejoin takes is a new `ServiceLock`, and it carries a lock
UUID of its own: `NewServiceLock` mints one and offers no way to supply
one, which is what `announceExistence` does with `UUID.randomUUID()` per
`ServiceLock`. Two generations of the same process therefore never share
a node prefix. That is what makes a stale shutdown of the generation
that ended harmless in a way the state machine could not have made it —
it deletes ZooKeeper nodes rather than calling `LoseLock`, so the host's
fence never sees it — and it is why the cleanup can identify its nodes
by prefix at all. See "The lock node".

A `LockID` is only usable as fencing authority if it could name a real
`zlock#<uuid>#<sequence>` node: the UUID must parse and the sequence must
fit the signed 32-bit counter Accumulo reads with `Integer.parseInt`.
`internal/zk`'s node parser applies the same selection rule but not the
same checks: it takes any spelling `uuid.Parse` accepts, while this one
requires the canonical 36-character form Java's `UUID.fromString` takes,
which is the only form Accumulo ever writes. The stricter side is the
right one for fencing, and the divergence is described where it is
observable, under "Reading the manager's lock".

## 6. Registering with ZooKeeper

A fenced state machine nobody can see is not a tablet server. Three
pieces turn it into one, and none of them decide anything: they publish
presence, and they read authority.

### The lock node

`ServiceLock` implements Accumulo's protocol as written, because the
manager and every Java client already implement the other half of it. It
creates an ephemeral sequential `zlock#<uuid>#<sequence>` node under
`<instance>/tservers/<group>/<address>` with the `ZooUtil.PUBLIC` ACL,
creating the resource-group path when it is the first server to use it,
the way `ServiceLockSupport.createNonHaServiceLockPath` does. The holder
is the lowest sequence that survives `validateAndSort`; a candidate that
is not the holder queues behind the holder's **lowest** node rather than
its immediate predecessor, so it wakes when that process leaves rather
than when it tidies up a duplicate of its own.

That scan stops where the prefix changes, which finds the holder's
lowest node only while that holder's nodes are contiguous. They need not
be — a create whose response was lost is retried against whatever the
sequence counter has reached by then, so a directory can read A, B, A —
and in that shape a candidate below parks on A's second node instead of
its first. `ServiceLock.findLowestPrevPrefix` stops in the same place,
and matching it is deliberate: which node a candidate waits on is a
private choice ZooKeeper does not arbitrate, so answering differently
from the implementation this one is read against buys nothing. The
error is always in the safe direction. Every node the scan can return
is ahead of the caller, and a node ahead of the caller has to go before
the caller can be lowest, so an off answer costs a pass over the
directory rather than a missed turn.

A queued candidate watches its own node as well. The node ahead says
when its turn arrives; its own node says when the turn will never
arrive. Watching only the node ahead leaves a candidate whose node was
deleted asleep on a queue it is no longer in until some unrelated holder
happens to leave.

Every UUID this process reads or writes must be the 36-character dashed
form. Go's `uuid.Parse` also accepts bare hex, `urn:uuid:`, and braced
forms; Java's `UUID.fromString` accepts none of them. Publishing one
would put a node name in the directory that `validateAndSort` throws on
and a payload the manager cannot deserialize — this process would queue
invisibly while Accumulo handed the lock to somebody else.

The resource group is held to Accumulo's grammar for the same reason,
`ResourceGroupId.GROUP_NAME_PATTERN` — `^[a-zA-Z]+(_?[a-zA-Z0-9])*$`.
`ServiceLockData.parse` builds a `ResourceGroupId` from every descriptor
it reads, and that constructor throws on anything outside the grammar,
so a group this process accepts and Accumulo does not makes the whole
znode unreadable. It also keeps the group out of a path it has no
business in: path segments are cleaned when the lock directory is built,
so a group like `../managers` would not be rejected by the join — it
would quietly register this server in another role's subtree, where
nothing looking for a tablet server would find it.

The address gets the same treatment, because it is the other variable
segment of that path as well as the endpoint written into the node. An
address carrying a separator — `../../managers/lock:9997` — is cleaned
into the manager's subtree by exactly the same join, so both segments
are checked before either is used. The check is the one the descriptor
already had to pass, which is what keeps the directory's name and the
advertised address the same address: a reader that finds them
disagreeing has no server it can reach.

Passing the same check is not the same as being the same value, so the
two are compared. The lock path and the payload arrive separately here —
Accumulo's `announceExistence` builds both from one address and one
resource group, but nothing about the API forces that — and they are
read for different things: the directory is how a server is enumerated,
the descriptor is what a client dials. A lock registered under one
address advertising another is a server the manager can see and nothing
can reach, so an advertisement whose address or group disagrees with the
directory is refused before the node is created. A lock path that names
no server, like the manager's `<instance>/managers/lock`, has no address
to be held to and is not checked this way.

The services are held to the path in the same way. `TabletServerLockData`
refuses the services other roles own, but `ServiceLockData` is an
ordinary struct that can be built without it, and a `MANAGER` descriptor
published from a tablet server's lock is an endpoint that role owns
pointing at a process which does not implement it. Under a
`.../tservers/...` path only the five a tablet server announces are
accepted; a lock that names no server carries its own role's services and
is left alone.

Duplicates are real: a create whose response was lost still created a
node, and a retry creates another. Both carry this process's UUID, so
the first is kept and the rest are deleted — otherwise one process would
hold several places in line and leave a node behind when it released.

A duplicate that cannot be deleted fails the acquisition. It is this
session's node, so it lives as long as the session does, holding a
second place in line that nothing maintains: when the node this process
adopted goes away, the survivor becomes the lowest in the directory and
therefore the holder the manager sees — a server that looks alive and
refuses every request, because the generation this process was fenced to
has ended. Failing instead hands it to the cleanup below, which sweeps
what it can and reports what it cannot.

A cancelled acquisition deletes whatever it created, and says so when it
cannot: the error also wraps `ErrLockNodeOrphaned`. An abandoned
ephemeral node keeps its place in the queue until the session ends —
it can become the holder of a lock nobody is waiting on — and only the
session's owner can close the session, so the owner has to be told
rather than left to discover a range nobody hosts for no reason.

Cancellation is honoured at the moment ownership would be taken, not
only while queued. Whether this process is first by the time the
cancellation lands is a race, and the outcome must not turn on it:
committing there would hand back a lock the caller stopped waiting for
and left nobody maintaining it, so both paths refuse and the node is
swept.

Both of those steps — adopting the lowest node under this process's
prefix, and sweeping everything under it — treat the prefix as a
statement of ownership. It carries the lock UUID, so that is only true
if a UUID belongs to one `ServiceLock` and never to two. Accumulo makes
it true by construction: `announceExistence` calls `UUID.randomUUID()`
immediately before building the `ServiceLock` and uses that value for
the node prefix and for every descriptor. `NewServiceLock` does the
same, and offers no option to supply one, so a caller keeping a server
identity in configuration cannot reintroduce the collision.

Both directions fail if two generations can share a prefix, and they
fail in opposite ways. The sweep would reach too far: a `Release`
arriving after its generation ended — a shutdown path that runs twice, a
goroutine that outlived its lock — would delete the live node of the
generation that replaced it, revoking a lock underneath a server the
manager had just seen take it, with no event to say so. The acquisition
would reach too far as well: it adopts the lowest node under the prefix,
so a rejoin would take over the node its predecessor left behind, hand
back the `LockID` the dead generation had, and publish the payload
already sitting in that node. A request stamped with the generation that
ended would then be accepted — the fence defeated by the mechanism meant
to complete it.

With the UUID minted per lock, neither is reachable and nothing else has
to be tracked. The sweep does not depend on how far the acquisition got,
on whether the generation is still held, or on a record of what was
created: it lists the directory and deletes what carries its prefix. So
a cancellation caught before the first create and a lock directory this
session is not yet allowed to make — both of which leave nothing behind
— are as safe as a held generation being released, which matters because
a caller whose shutdown path is `Release` calls it regardless and a
server that could not create the directory is a server that will retry.

Reaching far enough is the same property read the other way. A node this
lock created but never learned the name of — a create whose answer was
lost — still carries the prefix and is still swept. It has to be. A
generation can end with the session still alive: a read that fails
closed, or a node an operator removed. Anything left under the prefix
survives that, and once the adopted node goes it is the lowest in the
directory and so the holder the manager reads — the same
server-that-maintains-nothing this file refuses everywhere else.

The client is not expected to leave one. go-zookeeper does not replay
requests across a reconnect; it fails every pending one and resends only
auth. So a create either returns the node it made or an error, and the
error ends the acquisition through the same sweep. One that arrives some
other way is collapsed by the acquisition, which drops every duplicate
under the prefix, and goes with the session.

### What the node says

The payload is `ServiceLockData` in the exact Gson wire form the manager
reads — a `descriptors` array of `{uuid, service, address, group}`. A Java
tablet server publishes `CLIENT`, `TABLET_INGEST`, `TABLET_MANAGEMENT`,
`TABLET_SCAN`, and `TSERV`; `TabletServerLockData` can represent that full
set, but a Shoal process must pass only the services it actually serves.
`tserverrpc.Adapter.LockData` therefore advertises only
`TABLET_MANAGEMENT` and `TSERV`. Scan, ingest, and `ClientService` remain
absent until their processors share the same process and endpoint. The
adapter also refuses to build that lock data until both multiplexed
processors have been registered, so startup cannot publish ahead of the
RPC listener it describes.

The advertisement is refused rather than published when it would mislead
the manager: no descriptors, an address that is empty, unbound, or not
`host:port`, an unknown service, or two descriptors for one service —
which Accumulo's `EnumMap` would silently collapse to whichever arrived
last. A claim the manager cannot act on is worse than staying invisible,
because it draws work to a process that cannot do it.

Every descriptor has to carry the UUID the lock is held as. Accumulo's
tablet server announces itself with one UUID in both places, and the two
are read for different things: the node name is what a generation is
fenced by, the descriptor is what a client dials. Publishing one
server's descriptors from another's lock node splits those apart — the
generation this process would fence on and the server the manager reads
would not be the same server — so it is refused at acquisition rather
than left for the manager to find.

The order of the array is this process's own. Accumulo serializes its
descriptors through `Collectors.toSet()`, so a Java-written node is in
hash order and no reader can be relying on position; sorting by the
`ThriftService` declaration order makes the same advertisement the same
bytes every time, which is what makes it testable and diffable.

A wildcard is refused for the same reason. `0.0.0.0` and `::` are how a
process says which interfaces it binds; neither is an identity, and a
manager reading one out of this lock would have to substitute some host
of its own to dial, landing the work on whichever server it substituted.
What a server binds and what it advertises are different questions, and
only the second belongs in the lock.

A `+` anywhere in the address is refused too, and that one is about the
reader rather than the address. Everything on the Java side that turns a
descriptor into something dialable goes through
`AddressUtil.parseAddress`, which begins by replacing every `+` with `:`
— the encoding older Accumulo used to spell `host+port` inside a path
segment — before handing the result to `HostAndPort.fromString`. So an
address that Go reads as a perfectly good `host:port` can arrive as a
different string, and usually not an address at all:
`shoal-1.example:+9997` is read as `shoal-1.example::9997` and throws.
The throw surfaces in the manager's scan over the live servers, where it
ends the pass over all of them rather than the reading of this one — the
blast radius a missing `TSERV` descriptor has. Nothing that belongs in a
hostname, an IP literal or a port contains a `+`, so refusing it costs a
deployment nothing.

The port is read as its own grammar rather than as whatever the nearest
conversion function will accept. `strconv.Atoi` takes a sign, so `+9997`
and `-9997` are numbers to Go; Guava's `HostAndPort` refuses a port that
is not all digits. Checking the digits before the value is what keeps
the two ends agreeing on what was published.

That cuts both ways: a process should advertise what it can serve, not
what it intends to serve. `TabletServerServices()` names the full Java
set, but the caller chooses the subset, and until the Thrift endpoints
behind a service exist, advertising it routes work into a black hole.

The subset is a subset of those five and nothing else. `MANAGER`,
`COORDINATOR`, `GC`, `COMPACTOR` and the `FATE_*` services are defined
by the same enum and advertised on their own locks by the processes that
implement them; a tablet-server lock claiming one would point a client
at an endpoint this process does not serve, in a combination Accumulo
itself never writes and so has no reason to guard against.

`TSERV` is not one of the five the caller may drop. Every lock in the
tablet-server tree is read for it: `LiveTServerSet.checkServer` calls
`getAddress(ThriftService.TSERV)` on each held lock it finds and hands
the result straight to `new TServerInstance(address, …)`, which
dereferences it. A lock without that descriptor produces a null address
and a `NullPointerException` inside the scan — not a server the manager
skips, but a scan that stops, taking every other tablet server in that
pass with it. One Shoal registration would be enough, and it would repeat
on every scan for as long as the lock was held.

So it is refused in both places. `TabletServerLockData` will not build a
payload without it, which is where a caller finds out — at the call that
names the subset, rather than at an acquisition that fails much later
with a value it can no longer change. The publication gate refuses it
again, because `ServiceLockData` is an ordinary struct and a caller that
builds one directly never goes through the constructor: a lock that
registers a server and does not advertise `TSERV` is refused before it is
created.

That is the reason a process which cannot serve `TSERV` yet belongs
outside the tree rather than in it with a smaller advertisement.
Registering with the descriptor and no endpoint behind it is the ordinary
unreachable-server case, which Accumulo already handles by dooming the
instance; registering without it is a manager-wide failure.

### Holding it, and letting go

`Maintain` watches the node and returns when the generation ends.
Anything it cannot prove is treated as a loss: a watch that cannot be
established, a session that expired, a watch the client tore down. That
mirrors `LockWatcher.unableToMonitorLockNode`, where the Java tablet
server halts — a lock this process cannot monitor is one it cannot prove
it still holds, and hosting on an unprovable lock is exactly what
[§4](#4-the-fence) exists to stop.

Exactly one watch is outstanding at a time. A ZooKeeper watch is
one-shot, but it stays registered on the client and the server until it
fires, so a new one is armed only after the previous one has been
consumed. Arming per pass would make a healthy tablet server accumulate
a registration per verify interval for the life of its lock, and deliver
all of them at once when the node finally changed.

A node is watched by reading it, not by asking whether it exists. An
existence watch is set even when the node is missing — reporting a
creation is what it is for — and every path watched here is a sequential
lock node, a name ZooKeeper never hands out twice. A watch left on one
of those could not fire before the session ended, and the paths where
that happens are the ordinary ones: a predecessor that left between the
listing and the watch, an own node an operator removed, a held node a
release took away. A read sets its watch only when it succeeds, so a
node that is already gone costs nothing and says so.

The outstanding watch belongs to the lock, not to the call that armed
it. `Maintain` takes the caller's context and cancelling it does not
release the lock, so a watch scoped to the call would be left registered
with nobody to consume it and the next `Maintain` would arm another — a
supervisor that restarts its watcher accumulates one registration per
restart, which is the same leak as arming per pass. Concurrent watchers
share the one registration for the same reason.

Sharing a watch means only one watcher receives the event, so the ending
is broadcast as well: whoever observes it records it and closes a channel
every `Maintain` is selecting on, and they all return the recorded
ending rather than whatever woke them. Without that the watchers that did
not receive the event would go on watching a generation that had already
ended.

A watcher that arrives after the generation ended is told how it ended,
not that there is no lock. The two are different facts — a lock that
ended and one that never held anything — and reporting the second for the
first would make the answer depend on whether the caller reached the
watch before the loss landed. `ErrNotHeld` is kept for the lock that
genuinely never held a generation, where there is no ending to report.

The same holds while queued. The watch on this process's own node — the
one that says the turn will never come — is armed once and kept across
passes, so climbing a long queue does not leave a registration behind at
every place this process stood. The watch on the node ahead is a
different node on each pass, so it is not the same kind of accumulation:
each one belongs to the node it was armed on and is spent when that node
goes.

A watch cannot be taken back, which decides what an abandoned acquisition
may leave. go-zookeeper has no `removeWatches`, and it re-registers the
outstanding ones after a reconnect, so a watch is released only by
firing. The watch on this process's own node is not exposed to that — the
cleanup deletes the node it sits on, which fires it — but a watch on
another candidate's node outlives the acquisition that armed it and goes
only when that node does. A holder that stays put would collect one
registration from every attempt that gave up beneath it.

So an acquisition that is already over does not arm it: cancellation and
release are checked again immediately before, not only in the wait. What
remains is the watch an acquisition was parked on when it was told to
stop, one per attempt, released when the holder it names finally goes.
That is inherent to the primitive rather than to this design — upstream's
`ServiceLock` watches its predecessor the same way — and the alternative,
watching the lock directory instead, trades it for waking every candidate
on every change to the queue, which is the thundering herd that watching
one node ahead exists to avoid.

The own-node watch is kept across the handover too. Reaching the front of
the queue does not change which node this process is watching, so the
registration the wait armed is the generation's, and `Maintain` inherits
it rather than arming another — otherwise a candidate that queued would
carry a second registration on one node for as long as it held the lock,
which is the per-pass leak moved to the handover. An acquisition that was
first in line has nothing to hand over, and `Maintain` arms its own.

Inheriting it also closes the gap between deciding ownership and watching
it. The directory listing that says this process is first is a read of
the past by the time the caller acts on it, and the node can go in
between — an operator, a session that dropped just that node. A watch
already armed has already fired, so the ending is waiting to be
delivered; when there is none to inherit, the arming read finds the node
missing and ends the generation there. Both fail closed, which is what
lets `Participate` adopt a generation and be told immediately that it is
gone, rather than hosting against one that no longer exists.

An optional verify interval re-reads the directory on a timer. It catches
what a watch cannot: a watch dropped without an event, and a lock
directory deleted and recreated, which restarts ZooKeeper's sequence
counter and lets a later arrival take a lower number than the one held
here. The node is still there; it is simply no longer the holder.

A verification is about the moment it answers, not the moment it read.
The directory can be read before a release lands and the answer given
after it, and a release whose delete failed leaves the node exactly where
the checks look for it — so a reading that finds this process holding the
lock is confirmed against the recorded ending before it is reported. The
question a caller asks is whether it may still act, and by then it may
not.

`Participate` is the only place ZooKeeper reality meets the fence. It
acquires, hands the acquired generation to `AdoptLock`, watches it, and
on the way out calls `LoseLock` with **that** generation before deleting
the node. The order matters: deleting the node first would tell the
manager these tablets may be placed elsewhere while this process still
claimed them. If the host refuses the generation — one that is not newer
than a generation it already used — the lock is given back rather than
held, so the manager can place the work on a server that will take it.

`Release` sweeps every node under this lock's prefix, not just the one
it holds, and it is safe against an acquisition that has not finished:
the wait is woken and refused with `ErrLockReleased`. Releasing only the
held node would leave a caller that was told the lock was gone going on
to hold it, and would promote a surviving duplicate to holder of a lock
nobody is waiting on.

That refusal covers an acquisition that has finished waiting as well as
one still in the queue. Taking ownership and handing it back are not one
step — the generation is recorded under one lock and the watch handed
over under another — so a release can land in between, find a generation
held, end it, delete the node and return while the acquisition is still
on its way out. Reporting success there would hand back a `LockID` for a
generation that was over before the caller saw it, and nothing later
would contradict it, because the ending such a caller would wait for has
already been recorded. `Release` marks the lock released before it ends
anything, so a release that has returned is visible to the acquisition,
and reading that after the wait is what orders the two. A release
arriving after that read is an ordinary release of a lock the call
really did acquire, and `Maintain` reports it.

`Maintain` is woken by the release directly rather than by the node's
deletion coming back as a watch event. Ordinarily both happen; but a
delete that fails leaves the node in place, and with no verify interval
configured there is nothing else to wake the watch — it would go on
watching a generation the caller had already given up until the process
stopped.

The ending is recorded before the node is deleted, not after. The delete
is what a watching `Maintain` sees, and a watcher that got there first
would record an external `NODE_DELETED` for a node this process took
away on purpose. Since the first ending recorded is the one kept, that
would leave how a deliberate release is remembered depending on who won
the race; recording `RELEASED` up front makes the answer the same either
way.

A create that was already in flight is waited out before the sweep. The
two are mutually exclusive, so either the create happens and the sweep
that follows finds its node, or the release wins and there is no create
at all. Checking a flag alone leaves the window between the check and
the create, in which a release can read an empty directory, report
success, and be followed by the node it was supposed to remove.

Rejoining is ordinarily just another `ServiceLock`, whose node carries a
higher sequence than the one that ended. The exception is the recreated
directory above: the counter restarts, so a rejoining process can be
handed a generation its `Host` has already used, and `AdoptLock` refuses
it with `ErrLockNotNewer` — from the host's side that is
indistinguishable from a replay of a lock it no longer holds. The
high-water mark is per-host state, so recovery is a fresh `Host`, not a
retry; retrying against the same one fails identically every time.

A recreated directory is also why the lock UUID cannot be a caller's
choice. A generation is the UUID and the sequence together, so a rejoin
under the process's old UUID against a restarted counter would remint
the *same* generation the dead one had. A fresh `Host` has no history to
refuse it, and it would also accept a request fenced with the dead
generation that is still in flight — the fence satisfied by something
that is no longer true. `NewServiceLock` mints the UUID itself and takes
none, so the two generations are always distinguishable; the descriptors
follow without extra care, because `Acquire` refuses descriptors that
name a server other than the lock's own UUID, which is how
`announceExistence` keeps the node and its payload in agreement.

### Reading the manager's lock

`Host` refuses every manager-directed transition until it has been told
which manager is live, and [§2](#2-the-manager-is-the-only-authority) is
why: authority is read from ZooKeeper, never inferred from the requests
that arrive. `WatchManagerLock` supplies it by reading the manager's own
ServiceLock directory and taking the lowest node, so the standby managers
queued behind it are not mistaken for authority. That is the selection
rule `internal/zk` uses to resolve the manager's address, but the two do
not agree on which node names are legal: this parser requires the
canonical 36-character UUID, matching `UUID.fromString`, while
`internal/zk` accepts whatever `uuid.Parse` takes, including the undashed
and URN spellings Accumulo rejects. Only a writer other than Accumulo
could produce such a name, and the two would then disagree about who
holds the lock; the fix belongs in `internal/zk`, which is the lenient
one.

Two failures that look alike are kept apart. A directory that cannot be
read leaves the previous observation in place, because failing to reach
ZooKeeper is not evidence the manager changed and withdrawing authority
would refuse the live manager's assignments for nothing. A directory that
is readable and holds no lock is evidence, and clears it.

An observation the host refuses — a live holder whose epoch is older
than one already seen — is reported to the caller, and the watch goes on
polling. Two very different things read that way. The manager lock
directory may have been deleted and recreated, restarting the sequence
counter, which does not heal: polling through it leaves this host fenced
to a manager that no longer exists. Or the reading came from a ZooKeeper
server that had not caught up. ZooKeeper promises a client its own reads
never go backwards, but that is a promise about a session, and the reader
is free to use more than one: `internal/zk.Locator` opens a scoped
connection per read when the context can be cancelled, so consecutive
polls are consecutive sessions and any of them can land behind.

Nothing here can tell those apart. A recreated directory refuses every
poll after it, and so does a replica that stays behind; counting the
refusals makes the mistake less frequent and never impossible. So the
watch does not act on the guess. Safety never depended on it: the host
keeps the newer epoch whatever a reading says, so a stale one gains no
authority on the first refusal or the thousandth. Ending the watch would
add only the cost of being wrong — every tablet server reads the same
lagging ensemble, so ending on it restarts the fleet, discarding the
high-water marks that were doing the rejecting.

A recreated directory does need recovery, and it is the one `AdoptLock`'s
refusal already calls for: a fresh `Host`, which carries no epoch history
and takes the live manager on its first reading. That call is the
supervisor's, which can establish the cause; this package reports what it
saw and keeps observing, so a replica that catches up needs no
intervention at all.

The report is made once per run of refusals rather than once per poll,
because a condition that persists is one condition, and a single refusal
is not reported at all — one stale reading is ordinary and has already
been refused. Only a reading the host accepts ends a run. A poll that
could not be read is not evidence of anything — it leaves the previous
observation standing and tells the host nothing — so it leaves the run
where it was. Ending it there would let a recreated directory
interleaved with transient ZooKeeper failures refuse every reading and
never be reported.

The reader is expected to hold one session across polls, and the watch
is written to make that cheap: it reads the directory exactly once per
interval, so the only cost per interval is one `getChildren`. A reader
that connects per read turns each of those into a handshake, an
authentication and a close instead — per server, forever, against an
ensemble that is also serving every other client in the cluster.
`internal/zk.Locator` is that reader today on the cancellable path, and
reuses its session only on the path that cannot be cancelled, which is
not a poll a process can shut down. Wiring the watch into a running
server therefore waits on a cancellable reader that reuses its session,
or on one backed by a ZooKeeper watch instead of a poll; that is a
prerequisite of the process-wiring slice, recorded in
[§9](#9-what-is-not-here-yet), not something this package can fix from
behind the interface.

## 7. Manager RPC adapter

`internal/tserverrpc` vendors Accumulo 4's `tabletmgmt.thrift` and exposes
the multiplexed `tablet` and `tserver` processors. It validates the
credentials' instance ID through an injected system-credential verifier,
parses the manager's serialized `ZooUtil.LockID` (path, lock node, and
ZooKeeper session), requires that lock to match `Host.ManagerLock`, and
uses the currently held tablet-server ServiceLock as the server half of
the `Host` fence. A changed manager session for the same observed lock is
rejected.

Accepted loads and unloads run through an injected backend. Duplicate
loads and unloads are idempotent, unload cancels an in-flight load, and
`ReleaseDropped` is the callback passed to `Participate` so lock loss
cancels work and closes backend resources before the lock node is
released. Results are reported as `LOADED`, `LOAD_FAILURE`, `UNLOADED`,
`UNLOAD_FAILURE_NOT_SERVING`, or `UNLOAD_ERROR`. `RetryingReporter`
re-resolves and reconnects for every retry, so a status report follows a
manager failover instead of pinning a dead transport.

The `TSERV` processor provides status, tablet stats, flush, and fenced
halt/fast-halt control. Operations owned by data planes not present in
this slice return `ErrUnsupported`; the adapter does not advertise the
corresponding `TABLET_INGEST`, `TABLET_SCAN`, or `CLIENT` services.

`cmd/shoal-tserver` now owns all dependencies that `New` deliberately
requires: exact system-credential comparison, metadata/configuration loading,
failover-aware manager reporting, a reusable manager-lock reader, and the same
authenticated ZooKeeper session used by `ServiceLock`.

## 8. Hosted-tablet specification loader

`internal/tabletloader` resolves the immutable input to a hosted tablet
without owning manager RPCs or ingest. A load:

1. captures the exact metadata assignment generation from `Authority`
2. reads exactly one metadata row and requires its table/range to equal the
   manager-assigned `tserver.Extent`
3. requires exactly one complete future/current location whose session equals
   the captured generation, plus the mandatory `~tab:~pr`, `srv:dir`, and
   `srv:time` columns
4. reads a stable effective table configuration; `ManagerConfigSource`
   brackets two merged-configuration reads with versioned-property reads and
   retries the whole transaction if either view changes
5. resolves and validates every StoredTabletFile and LogEntry reference
6. rechecks the assignment fence between every external operation and before
   returning a deterministically ordered `Specification`

Missing rows/configuration and malformed metadata fail without retry.
Infrastructure failures explicitly marked with `tabletloader.Retryable` retry
the complete transaction with bounded, cancellable backoff. Unload, lock loss,
or reassignment must make `Authority.Validate` fail (and normally also cancel
the context), so a stale load can never return a publishable specification.

The package exposes narrow `MetadataSource`, `ConfigSource`, `FileResolver`,
`LogResolver`, and `Authority` interfaces. This keeps storage routing, exact
metadata scanning, and the manager adapter independently replaceable. The
default `StrictReferenceResolver` performs schema validation only; a production
file resolver can additionally probe the chosen `storage.Backend`.

Metadata aggregation now preserves future locations, mandatory-column
presence, directory/time values, and Accumulo 4 `log:` qualifiers. WAL
qualifiers are decoded using Java `LogEntry.fromMetaWalEntry`'s
`-/<path ending in host+port/UUID>` format.

## 9. Ingest authority

`cmd/shoal-tserver` is a write-capable tablet-server participant. With the
explicit `-enable-ingest` release gate, it registers the `TabletIngest`
processor and advertises `TABLET_INGEST` only after the ServiceLock-dependent
metadata CAS, WAL, loader, memtable, and minor-compaction authorities are
initialized. The flag defaults off while #74 gathers live mixed-fleet evidence.

The process refuses to publish a loaded tablet that still references WAL
segments. Serving that tablet without opening and replaying those references
would silently omit unflushed mutations. The update-session Thrift adapter, mutation decoding, stale-extent/retry maps,
WAL recovery, memtable application, timestamp assignment, minor-compaction
coordination, assignment-attempt plumbing, drain serialization, and
readiness/metrics composition now exist in `internal/ingestservice`,
`internal/hostedingest`, and `tserverprocess.NewWritableStore`.

`internal/metadatacas` implements the production authority. Root metadata uses
ZooKeeper version CAS. Other metadata uses exact Accumulo conditional mutations
covering current/future location, ServiceLock identity, previous row, WAL
qualifiers, and installed files. Ambiguous transport results are reconciled by
authoritative rereads, and durable minc checkpoints resume idempotently after
restart.

Live mixed Java/Shoal migration and rolling-replacement acceptance still need
an Accumulo cluster CI environment and remain tracked separately in #74.

## 10. Metrics

`Host.Metrics()` snapshots the operational surface #67 asks for:
`Loading` / `Hosted` / `Unloading` gauges, and counters for
`Assignments`, `Loads`, `LoadFailures`, `Unloads`, `ForcedUnloads`,
`RejectedStale`, `RejectedDuplicate`, `LockLosses`, and
`DroppedOnLockLoss`. The two rejection counters are the ones to alert on:
a healthy cluster refuses almost nothing, so a rising `RejectedStale`
means transitions are naming a superseded caller — assignments racing
lock churn, or completions arriving after their assignment ended — and a
rising `RejectedDuplicate` means the manager is working from stale tablet
metadata.
