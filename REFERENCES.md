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
# Source references for the shoal port

Authoritative Java + sharkbite source files referenced during V0
implementation. Quote files here when porting; keep this list current as new
cribs land.

## Accumulo (this repo)

### ZK paths and constants
- `core/src/main/java/org/apache/accumulo/core/Constants.java`
  - `ZROOT = "/accumulo"`, `ZINSTANCES = "/instances"`
  - `ZTABLES`, `ZTABLE_STATE`, `ZTABLE_FLUSH_ID`, `ZTABLE_NAMESPACE`,
    `ZTABLE_DELETE_MARKER` (no `/loaded` — original spec was wrong)
- `core/src/main/java/org/apache/accumulo/core/metadata/RootTable.java`
  - `ZROOT_TABLET = "/root_tablet"`
  - `EXTENT.toMetaRow() = "+r<"` (root tablet self-row in metadata)
- `core/src/main/java/org/apache/accumulo/core/zookeeper/ZooUtil.java:144-145`
  - Path assembly: `ZROOT + "/" + instanceId + ZROOT_TABLET`
  - `:199` instance-name → instance-UUID lookup

### Root tablet znode parsing (JSON in 4.0)
- `core/.../metadata/schema/RootTabletMetadata.java` — JSON shape
- `core/.../client/clientImpl/RootClientTabletCache.java:115-148` — exact
  bootstrap chain we mirror: read znode → parse JSON → extract `loc` →
  return `Location(host, port)`

### Metadata table schema
- `core/src/main/java/org/apache/accumulo/core/metadata/schema/MetadataSchema.java`
  - `:63-76` `TabletsSection.encodeRow(tableId, endRow)`:
    `tableId + ';' + endRow`, or `tableId + '<'` if endRow is null
  - `:327` `CurrentLocationColumnFamily.STR_NAME = "loc"` (qualifier =
    encoded lock id, value = `host:port`)
  - `:381-387` `DataFileColumnFamily.STR_NAME = "file"` (qualifier = file
    path, value = `DataFileValue`)
  - `:361` `BulkFileColumnFamily.STR_NAME = "loaded"` (column family in
    metadata table, NOT a ZK znode)
- `core/.../metadata/schema/DataFileValue.java:48-89`
  - Encoded as `size,numEntries[,time]` (comma-separated longs)

### Lock id encoding
- `core/.../zookeeper/ZooUtil.java:61-81`
  - Lock-id format: `<path>/<node>$<hex_eid>`

### Scan server reference (read path)
- `server/tserver/.../ScanServer.java:148-180`
  - `TabletMetadataLoader.load()` → `ample.readTablet(keyExtent)`
  - `loadAll()` → batch via `ample.readTablets()`
  - Cache TTL: `SSERV_CACHED_TABLET_METADATA_EXPIRATION` (we use exception-
    driven invalidation instead of TTL — matches sharkbite, pairs better
    with hedged reads)
- `core/.../client/clientImpl/ClientTabletCacheImpl.java` — multi-level
  cache for user tables

### Mutation and ingest reference (write path)
- `core/.../data/Mutation.java`
  - `VALUE_SIZE_COPY_CUTOFF = 1 << 15`
  - column updates use Hadoop `writeVLong` lengths, an explicit
    `hasTimestamp` byte, a delete byte, and negative indexes into
    `TMutation.values` for large values
- `core/.../clientImpl/TabletServerBatchWriter.java`
  - one tablet-server client carries `startUpdate` → one-way
    `applyUpdates` batches → `closeUpdate`
  - independent tablet servers are submitted through a bounded send pool;
    each server is queued to only one send task at a time
  - maximum-latency processing runs on a scheduled task synchronized with
    add/flush/close, and close shuts down the scheduler
  - `UpdateErrors.failedExtents` values are committed-mutation counts used
    to invalidate the failed tablet and retry only
    `mutations[committed:]`
  - shoal follows that explicit-prefix retry, but does not replay ambiguous
    `applyUpdates` or `closeUpdate` transport failures
- `core/.../client/BatchWriterConfig.java`
  - zero maximum latency means no latency bound
  - shoal also treats zero as disabled, rather than adopting Accumulo's
    configured 120-second default
- `core/.../rpc/clients/ThriftClientTypes.java`
  - `TabletIngestClientService` multiplex service name is `ingest`
  - `TabletManagementClientService` multiplex service name is `tablet`
  - `TabletServerClientService` multiplex service name is `tserver`

### Thrift IDL
- Vendored snapshot: `internal/thrift/idl` (Accumulo source revision
  `1a716b2c1bb5762ead4b46d2bc4f53e13873b314`)
- `core/src/main/thrift/tabletscan.thrift:77-95` — `startScan` signature
- `core/src/main/thrift/tabletingest.thrift` — `startUpdate`,
  `applyUpdates`, `closeUpdate`, and `cancelUpdate`
- `core/src/main/thrift/tabletmgmt.thrift` — manager-directed `loadTablet`,
  `unloadTablet`, and `flushTablet`
- `core/src/main/thrift/tabletserver.thrift` — manager-visible status,
  flush, and halt control
- `core/src/main/thrift/data.thrift` — `TKeyExtent`, `TRange`, `TKey`,
  `TKeyValue`, `TColumn`, `IterInfo`, `TMutation`, and `UpdateErrors`
- `core/src/main/thrift/security.thrift` — `TCredentials`
- `core/src/main/thrift/client.thrift` — `TInfo`

### Security administration (Accumulo revision `317c288568e9c46e7854aafb8bb8c4fda6260b12`)
- `core/src/main/java/org/apache/accumulo/core/clientImpl/SecurityOperationsImpl.java`
  - all user, authorization, and permission operations use the multiplexed
    `ClientService` (`client`) rather than the Manager service
  - RPC argument order is trace info, connector credentials, principal, then
    password/authorizations/name/permission as applicable
  - changing the authenticated principal's own password replaces the client
    context credentials used by subsequent RPCs
  - Shoal refreshes its scan, ingest, and manager adapter credential copies
    before `ChangePassword` returns successfully; RPCs already in flight may
    retain the prior snapshot. Adapter update failures are returned explicitly;
    the cross-adapter refresh does not claim atomic swap or rollback semantics.
  - table permission calls translate `NAMESPACE_DOESNT_EXIST` to
    `TABLE_DOESNT_EXIST`
- `core/src/main/java/org/apache/accumulo/core/security/SystemPermission.java`
  - valid wire IDs are 0 through 11 (`GRANT` through
    `OBTAIN_DELEGATION_TOKEN`)
- `core/src/main/java/org/apache/accumulo/core/security/TablePermission.java`
  - valid wire IDs are 2 through 8; 0 and 1 are intentionally unused legacy
    gaps and must not be accepted
- `core/src/main/java/org/apache/accumulo/core/security/NamespacePermission.java`
  - valid wire IDs are 0 through 8
- `core/src/main/java/org/apache/accumulo/core/security/Authorizations.java`
  - authorization values are non-empty byte strings; collections are
    duplicate-free, sorted, and defensively copied
- `core/src/main/java/org/apache/accumulo/core/util/Validators.java`
  - existing table and namespace segments use `\w+`; qualified table names
    contain one namespace segment and one table segment
  - the empty string is the valid, always-existing default namespace name
- `core/src/main/thrift/client.thrift`
  - authoritative Accumulo 4 RPC names, field IDs, argument order,
    `SecurityErrorCode` values, and declared security/table exceptions

### Namespace administration (Accumulo revision `317c288568e9c46e7854aafb8bb8c4fda6260b12`)
- `core/.../clientImpl/NamespaceOperationsImpl.java`
  - `create`, `delete`, and `rename` submit `NAMESPACE_CREATE`,
    `NAMESPACE_DELETE`, and `NAMESPACE_RENAME` through the shared table FATE
    helper with UTF-8 namespace-name arguments and an empty options map
  - FATE instance selection uses
    `FateInstanceType.fromNamespaceOrTableName`: names beginning with
    `accumulo` use META; all others (including the default namespace's empty
    name) use USER
  - property mutations call ManagerService `setNamespaceProperty` and
    `removeNamespaceProperty`
  - effective, local, and versioned property reads call ClientService
    `getNamespaceConfiguration`, `getNamespaceProperties`, and
    `getVersionedNamespaceProperties`
  - `delete` resolves the namespace ID and checks
    `context.getTableMapping(namespaceId).getIdToNameMap().isEmpty()` before
    submitting `NAMESPACE_DELETE`, throwing `NamespaceNotEmptyException` when
    any table remains
- `server/manager/.../FateServiceHandler.java`
  - namespace create/delete take exactly one name argument; rename takes old
    and new names
  - server validators enforce new/existing/built-in namespace rules, so Shoal
    sends default and reserved names unchanged and maps the returned status
- `server/manager/.../tableOps/namespace/delete/DeleteNamespace.java` and
  `NamespaceCleanUp.java`
  - FATE reserves the namespace and cleanup removes its ZooKeeper mapping and
    permissions, but does not repeat the client-side table-emptiness check;
    Shoal therefore mirrors the client preflight as a best-effort snapshot
    while preserving FATE as the mutation and authorization authority
- `core/src/main/thrift/manager.thrift`
  - `NAMESPACE_CREATE = 13`, `NAMESPACE_DELETE = 14`,
    `NAMESPACE_RENAME = 15`
  - ManagerService namespace property RPC signatures and declared property,
    security, namespace/table-operation, and not-active exceptions
- `core/src/main/thrift/client.thrift`
  - all three namespace property read RPCs take the namespace name; versioned
    reads return `TVersionedProperties { version, properties }`

### Wire protocol
- `core/.../rpc/AccumuloProtocolFactory.java`
  - `:49` `MAGIC_NUMBER = 0x41434355` ("ACCU" — A=41, C=43, C=43, U=55)
  - `:50` `PROTOCOL_VERSION = 1` (changes only when header format changes)
  - `:67-76` client-side `writeMessageBegin` prepends header before message
  - `:91-98` server-side `readMessageBegin` validates header, then proceeds
  - `:103-108` header layout (encoded via TCompactProtocol's own writers,
    NOT raw bytes): `i32` magic + `byte` version + `string` accumulo-version
    (e.g. `Constants.VERSION` = `"4.0.0-SNAPSHOT"`) + `string` instance-id
    (UUID canonical)
  - `:115-150` server-side validation: magic match → protocol version == 1
    → accumulo major.minor match → instance id match
  - `:184-190` major.minor extraction: substring before last `.` (so
    `"4.0.0-SNAPSHOT"` becomes `"4.0"`)
- `core/.../rpc/AccumuloTFramedTransportFactory.java:29` — TFramedTransport

### Table splits (`TABLE_SPLIT`, port target for #88)

Accumulo source revision `317c288568e9c46e7854aafb8bb8c4fda6260b12`.

- `core/.../clientImpl/TableOperationsImpl.java`
  - `addSplits` = `putSplits(tableName,
    TabletMergeabilityUtil.userDefaultSplits(splits))`, i.e. every
    user-requested split point carries `TabletMergeability.never()`
  - `putSplits` first validates `EXISTING_TABLE_NAME`, resolves the table
    ID, and calls `requireNotOffline(tableId, tableName)` before any
    tablet mapping, so malformed names and OFFLINE tables fail before
    either `updateTabletMergeability` or `TABLE_SPLIT`
  - `putSplits` loops until nothing is pending: invalidate the tablet
    cache, map each pending split row to the tablet whose
    `(prevEndRow, endRow]` range contains it, then run one FATE
    `TABLE_SPLIT` per tablet. FATE argument order is exactly
    `[tableId.canonical(), endRow or EMPTY, prevEndRow or EMPTY,
    encoded split…]`, options `Map.of()`, and `EMPTY` is a zero-length
    buffer (`ByteBuffer.allocate(0)`) for an unbounded boundary
  - pending-row mapping uses `Retry.builder().infiniteRetries()`
    `.retryAfter(100ms).incrementBy(100ms).maxWait(2s).backOffFactor(1.5)`;
    because `backOffFactor > 1`, `Retry.waitForNextAttempt` produces an
    exponential schedule with ±5% jitter around the factor-derived
    increment, capped at 2 seconds
  - `SPLIT_SUCCESS_MSG = "SPLIT_SUCCEEDED"` — only when
    `waitForFateOperation` returns this string are the tablet's splits
    removed from the pending set; any other value (the server returns the
    empty string when the requested extent no longer exists) means retry
    against re-resolved tablets. The FATE op is finished in a `finally`
    block even when the wait failed
  - `mapSplitsToTablets` routes a split whose row already equals the
    containing tablet's `endRow` to `existingSplits` instead of a new FATE
    op; those are sent to the manager's `updateTabletMergeability` and only
    the extents the manager returns are treated as done
  - `FateInstanceType.fromNamespaceOrTableName` (see
    `core/.../fate/FateInstanceType.java` + `clientImpl/Namespace.java`)
    picks `META` when the qualified name starts with the literal
    `"accumulo"`, else `USER`
- `core/.../clientImpl/TabletMergeabilityUtil.java`
  - split payload = `gson.toJson(GSonData(byte[] split, boolean never,
    Long delay))` using
    `ByteArrayToBase64TypeAdapter.createBase64Gson()`. Record component
    order fixes the JSON key order (`split`, `never`, `delay`), the Base64
    is URL-safe **and padded** (`Base64.getUrlEncoder()`, Go's
    `base64.URLEncoding`), HTML escaping is disabled, and Gson's default
    null-suppression drops `delay` entirely for `never()`. Byte-for-byte:
    `{"split":"YQ==","never":true}` for row `a`, and
    `{"split":"YQ==","never":false,"delay":86400000000000}` for
    `after(1 day)`. Reproduced as goldens in
    `accumulo/split_payload_test.go`
  - `toThrift(never())` = `TTabletMergeability(never=true, delay=-1)`
- `server/manager/.../FateServiceHandler.java`
  - `TABLE_SPLIT` case: `SPLIT_OFFSET = 3`, requires at least 4 arguments,
    validates argument 0 as a table **ID** (not a name — same as
    `TABLE_BULK_IMPORT2`), treats zero-length arguments 1 and 2 as null
    extent boundaries, decodes arguments 3..n with
    `TabletMergeabilityUtil::decode`, and rejects any split outside the
    extent or equal to its end row
- `server/manager/.../ManagerClientServiceHandler.java`
  - `updateTabletMergeability(tinfo, creds, tableName, Map<TKeyExtent,
    TTabletMergeability>)` takes the **qualified table name**, requires
    `canSplitTablet`, and returns only the extents whose conditional
    mutation was `ACCEPTED`; rejected tablets must be retried by the client
- `server/manager/.../tableOps/split/DeleteOperationIds.java`
  - `getReturn()` is what surfaces `SPLIT_SUCCEEDED` to the client

Shoal mapping: `accumulo.Connector.AddTableSplits` →
`managerclient.TableSplit` / `managerclient.Pooled.ExecuteStatus` and
`managerclient.Pooled.UpdateTabletMergeability`. Accumulo 4 removed the
legacy tablet-server `splitTablet` RPC; Shoal never calls it. Unlike
Accumulo, Shoal submits a table's groups sequentially (not through two
thread pools) and bounds the outer retry loop to 10 attempts, failing
with `ErrTableSplitsIncomplete` instead of looping forever, while
mirroring Accumulo's per-round retry schedule.

### RFile (port target for V0)
- `core/src/main/java/org/apache/accumulo/core/file/rfile/RFile.java`
- `core/.../iterators/system/VisibilityFilter.java`
- `core/.../client/rfile/RFileScanner.java` — Marc's prior art (RFile reader
  upstream); reference, do not depend on

### Bulk Import V2 (promotion, port target for #70)
- `core/.../clientImpl/bulk/Bulk.java`
  - Gson-serialized shape: `Mapping{tablet: Tablet{endRow, prevEndRow},
    files: Collection<FileInfo>}`, `FileInfo{name, estSize, estEntries}` —
    private field names are used verbatim as JSON keys (no Gson naming
    policy)
- `core/.../clientImpl/bulk/LoadMappingIterator.java`
  - the load mapping file is a top-level JSON **array**, streamed via
    `JsonReader.beginArray()`, one `Bulk.Mapping` per element
  - entries must be in ascending `KeyExtent` order — throws
    `IllegalStateException` otherwise
  - `tableId` is supplied to the iterator's constructor separately; it is
    not embedded in the per-entry JSON
- `core/.../clientImpl/bulk/ByteArrayToBase64TypeAdapter.java`
  - `endRow`/`prevEndRow` `byte[]` fields are serialized with
    `Base64.getUrlEncoder()`/`getUrlDecoder()` (URL-safe, padded — Go's
    `base64.URLEncoding`); Gson's default null-suppression omits the JSON
    key entirely for a nil field rather than emitting `null`
- `core/src/main/java/org/apache/accumulo/core/Constants.java`
  - `BULK_LOAD_MAPPING = "loadmap.json"` — fixed filename at the bulk
    directory root
  - `BULK_RENAME_FILE = "renames.json"` — a separate, server-side-only
    artifact from a later FATE phase; not a client concern
- `core/.../dataImpl/KeyExtent.java`
  - `compareTo` ordering: ascending by `endRow`, with a null (unbounded)
    `endRow` sorting **last** (`Comparator.nullsLast`) — this is the order
    `LoadMappingIterator` requires
  - `contains(row)` = `(prevEndRow == null || prevEndRow < row) &&
    (endRow == null || endRow >= row)`, i.e. `(prevEndRow, endRow]` —
    exclusive start, inclusive end. `rowAfterPrevRow()` (append a trailing
    `0x00` byte) is Accumulo's own "successor" helper for this convention;
    there is no symmetric "predecessor" helper, which is why Shoal's
    opposite-convention local tablets (`[start, end)`, see
    `internal/engine/table.go`'s `routeTablet`) cannot be translated to
    exact `KeyExtent`s via simple byte math (`docs/promotion.md` §3)
- `core/.../clientImpl/bulk/BulkImport.java`
  - `computeMappingFromFiles` — the default path: opens each RFile's index
    and queries the destination table's live tablet metadata to compute a
    load mapping. Shoal does not use this path (see `docs/promotion.md`
    §3): it uses the caller-supplied-partition path instead
    (`LoadPlan`/`RangeType.TABLE`), building KeyExtents directly from
    Shoal's own `RFileExportManifest` tablet partitioning
- `server/manager/.../FateServiceHandler.java` and
  `server/manager/.../tableOps/bulkVer2/PrepBulkImport.java`
  - FATE argument shape for `TABLE_BULK_IMPORT2` (tableId, bulk dir,
    setTime), plus `validateLoadMapping`, the server-side split
    **validation** step run before any file is loaded
    (`managerclient.TableBulkImport` / `accumulo.Connector.BulkImport`
    mirror the client-side call shape only; validation itself is entirely
    server-side and out of scope here). `validateLoadMapping` does **not**
    create or reconcile splits: it walks the destination's real, current
    tablets and requires each load-mapping entry's `prevEndRow`/`endRow` to
    individually match some real tablet boundary (a file may span several
    destination tablets), rejecting the whole FATE operation with
    `BULK_CONCURRENT_MERGE` ("Concurrent merge happened") if that walk
    fails. `validateLoadMapping` walks a `PeekingIterator` over the
    destination's tablets whose position only ever advances forward
    (`pi.next()`), never re-winds, and is shared across load-mapping
    entries — so, for entry `i` (i>=1) to validate, the real tablet the
    iterator is left resting on after entry `i-1` succeeds must itself
    have a `prevEndRow` equal to entry `i`'s own declared `prevEndRow`.
    That is **not**, in general, the same thing as "entry `i`'s
    `prevEndRow` equals entry `i-1`'s own `endRow`": the iterator's
    left-over tablet's own `prevEndRow` is one boundary further back than
    that. A single entry may still legally span more than one real
    destination tablet. Shoal's promotion slice exploits exactly this by
    widening each multi-tablet manifest chain's per-tablet extents so
    every entry after the first declares `prevEndRow` as the *previous*
    chain tablet's own `StartRow` — one boundary further back than that
    chain tablet's own `endRow` (see
    `internal/promotion/loadmapping.go`'s `resolveManifestTablets` and
    `docs/promotion.md` §3.2 for the derivation). That widened
    `prevEndRow` matches the iterator's left-over position only when the
    destination's real splits, at or before the mapping's last boundary,
    are **exactly** the manifest's own chain boundaries — no fewer (a
    missing split leaves the iterator one or more real tablets short of
    where an entry's `endRow` search expects it to stop) and, just as
    importantly, no more: an extra split anywhere before the last
    boundary permanently displaces the iterator one real tablet further
    forward than a later entry's `prevEndRow` search can recover from,
    since the iterator never rewinds, and validation is rejected with
    `BULK_CONCURRENT_MERGE` even though nothing actually merged. Shoal
    calls `accumulo.Connector.AddTableSplitsForTable` (the `TABLE_SPLIT` FATE op)
    to reconcile the destination's real splits to match the manifest's
    chain boundaries — guaranteeing the "no fewer" half — and then, since
    `AddTableSplitsForTable` cannot itself remove an unrelated pre-existing split,
    calls `accumulo.Connector.ListTableSplits` to confirm the "no more"
    half directly (see `internal/promotion/promote.go`'s
    `verifyNoUnexpectedDestinationSplits`), both *before*
    staging/submitting `TABLE_BULK_IMPORT2` — it does not create or
    reconcile splits itself, and does not use `computeMappingFromFiles`'s
    RFile-index-based rewrite path. A concurrent split or merge between
    that verification and bulk-import submission can still legitimately
    trigger `BULK_CONCURRENT_MERGE`; this slice narrows that race but does
    not eliminate it (`docs/promotion.md` §3.3, §5).

## Sharkbite

C++ Accumulo client (https://github.com/phrocker/sharkbite). Used as a
pattern reference, not a code dependency.

### What carries over
- `src/data/client/MetaDataLocationObtainer.cpp:95-135` — metadata-row
  decoding (`file:`, `loc:` column family loop)
- `include/data/client/LocatorCache.h:26-60` — exception-driven invalidation
  pattern (we reuse this; no TTL)
- `include/interconnect/transport/CachedTransport.h:44-91` — transport-level
  error → cache eviction
- `include/writer/impl/WriterHeuristic.h:60-217` — fixed writer worker count
  and bounded submission queue (shoal uses per-flush workers that are always
  joined instead of persistent threads)
- `include/writer/Sink.h:89-115` and
  `src/writer/impl/SinkImpl.cpp:70-177` — sharkbite flushes on queue thresholds,
  explicit flush, and close; it has no latency-based writer scheduler
- `src/interconnect/accumulo/AccumuloServerOne.cpp:181-247` — single-shot
  `startScan` invocation shape
- `include/data/constructs/client/zookeeper/zookeepers.h:35-41` — ZK path
  constants and bootstrap sequence (path assembly carries; parsing does not
  — see below)

### What does NOT carry over
- **No AccumuloProtocol header wrapper.** Sharkbite targets pre-2.1
  Accumulo, before the magic header existed. We write that piece from
  scratch.
- **Pipe-delimited root znode parsing.** `RootTabletLocator.cpp:32-48` parses
  `host:port|sessionId` — that's the old format. Modern Accumulo (4.0) uses
  JSON `RootTabletMetadata`. Reuse the bootstrap *sequence*, not the parser.
- **No `DataFileValue` decoding.** Sharkbite ignores the `file:` value
  bytes. We need to parse `size,numEntries[,time]` for accurate split
  decisions and stats. (Optional in V0; required by V1.)

## Go runtime

- `time.Timer.Stop` and `time.Timer.Reset` documentation for Go 1.23+
  - channel-based timers do not deliver stale values after Stop or Reset
  - `go.mod` requires Go 1.25, so this guarantee is part of the supported
    runtime baseline
  - shoal reuses one channel-based timer and joins its owning goroutine
