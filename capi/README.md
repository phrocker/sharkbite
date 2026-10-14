# Shoal C ABI

`capi/include/shoal.h` is the stable, versioned C surface for external
language bindings. Build the shared library and installable header with:

```text
make capi
```

The output is placed in `bin/capi/`. On Windows without `make`, the equivalent
library build is:

```text
go build -buildmode=c-shared -o bin\capi\shoal.dll .\cmd\shoal-capi
```

Use the checked-in `capi/include/shoal.h`, not the implementation header that
Go emits next to a `c-shared` library.

For deterministic host and cross-platform artifact evidence, run:

```text
python scripts/cross_platform_artifacts.py
```

On a supported native host this builds both `c-shared` and `c-archive`
artifacts, compiles/links/runs the public C11 and C++11 clients, executes the
dynamic ABI query, and checks every declared `shoal_*` export in the native
object format. Cross-built non-CGO commands and unbundled wheel previews are
recorded separately and never counted as C ABI runtime evidence.

Windows consumers linking the `c-archive` artifact must define
`SHOAL_STATIC` before including `shoal.h`; shared-library consumers leave it
unset and receive the normal `__declspec(dllimport)` declarations.

## ABI version and capability discovery

Shoal separates **ABI compatibility** from **feature availability**:

- `SHOAL_ABI_VERSION` and `shoal_abi_version()` remain the legacy
  compatibility-major for existing callers. They stay at `1` for every
  backward-compatible ABI update.
- `SHOAL_ABI_VERSION_MAJOR`, `_MINOR`, `_PATCH`,
  `SHOAL_ABI_VERSION_PACKED`, and the matching runtime queries provide a
  stable allocation-free version tuple that works before connector creation.
  `SHOAL_ABI_VERSION_PACKED` uses
  `SHOAL_ABI_PACK_VERSION(major, minor, patch)` with a hexadecimal
  `0x00MMmmpp` layout, so ABI `1.19.0` is `0x00011300`.
- Capability identifiers are append-only. Existing IDs and bits never change
  meaning. `shoal_abi_capability_word_count()` reports how many 64-bit words
  the current library uses, `shoal_abi_capability_word(i)` returns `0` for
  `i >= word_count`, and `shoal_abi_has_capability(id)` returns `0` for both
  unsupported and unknown IDs.

Current capability assignments (`word 0 == 0x000000007fffffff`):

| ID | Mask | Surface |
| --- | --- | --- |
| `SHOAL_ABI_CAPABILITY_CONNECTOR` | `0x0001` | connector handle lifecycle |
| `SHOAL_ABI_CAPABILITY_BOOTSTRAP` | `0x0002` | static/ZooKeeper bootstrap configuration |
| `SHOAL_ABI_CAPABILITY_ERROR` | `0x0004` | owned `shoal_error` objects and stable code/message access |
| `SHOAL_ABI_CAPABILITY_SCANNER` | `0x0008` | single-range scanner creation, close, and scan |
| `SHOAL_ABI_CAPABILITY_BATCH_SCANNER` | `0x0010` | multi-range batch scanner creation, close, and scan |
| `SHOAL_ABI_CAPABILITY_OWNED_SCAN_RESULT` | `0x0020` | owned scan results and borrowed key/value views |
| `SHOAL_ABI_CAPABILITY_MUTATION` | `0x0040` | mutation creation, updates, size, and free |
| `SHOAL_ABI_CAPABILITY_BATCH_WRITER` | `0x0080` | batch writer creation, add, flush, close, and free |
| `SHOAL_ABI_CAPABILITY_STRUCTURED_WRITE_FAILURE` | `0x0100` | owned structured write failure details |
| `SHOAL_ABI_CAPABILITY_TABLE_ADMIN` | `0x0200` | list/exists/create/delete/rename/flush/property administration |
| `SHOAL_ABI_CAPABILITY_NAMESPACE_ADMIN` | `0x0400` | namespace discovery, lifecycle, and property administration |
| `SHOAL_ABI_CAPABILITY_SECURITY_ADMIN` | `0x0800` | users, authorizations, and permission administration |
| `SHOAL_ABI_CAPABILITY_TABLE_SPLITS` | `0x1000` | binary-safe split listing and creation |
| `SHOAL_ABI_CAPABILITY_CONNECTOR_IDENTITY` | `0x2000` | owned connector instance-name, instance-ID, and principal discovery |
| `SHOAL_ABI_CAPABILITY_DATA_DESCRIPTORS` | `0x4000` | owned range and iterator-setting descriptors with versioned borrowed views |
| `SHOAL_ABI_CAPABILITY_CONFIGURATION_TOPOLOGY` | `0x8000` | binary-safe configuration handles and owned instance-topology snapshots |
| `SHOAL_ABI_CAPABILITY_RFILE` | `0x10000` | owned standalone RFile readers, writers, seekable relocations, and copied results |
| `SHOAL_ABI_CAPABILITY_DATA_VALUES` | `0x20000` | copied key/range/authorization operations and owned key/value results |
| `SHOAL_ABI_CAPABILITY_BUFFERED_WRITER` | `0x40000` | owned lazy row-buffered high-level writer with close coordination |
| `SHOAL_ABI_CAPABILITY_TABLE_MAINTENANCE` | `0x80000` | row-bounded table flush and owned constraint administration |
| `SHOAL_ABI_CAPABILITY_CONNECTOR_CONTROL` | `0x100000` | one-shot scan cancellation and connector cache invalidation |
| `SHOAL_ABI_CAPABILITY_HIGH_LEVEL_CLIENT` | `0x200000` | owned mutable high-level client facade and scanner/writer construction |
| `SHOAL_ABI_CAPABILITY_HIGH_LEVEL_SCANNER` | `0x400000` | copied column selection and direct owned-result client scans |
| `SHOAL_ABI_CAPABILITY_COMPATIBILITY_ERRORS` | `0x800000` | stable Sharkbite source and Python exception classification for owned errors |
| `SHOAL_ABI_CAPABILITY_STREAMING_SCAN_CURSOR` | `0x1000000` | owned bounded-memory scan cursors for scanner, batch-scanner, and high-level client streams |
| `SHOAL_ABI_CAPABILITY_COLUMN_VISIBILITY` | `0x2000000` | copied binary visibility expressions, immutable owned trees/terms, synchronized evaluators, and structured parse errors |
| `SHOAL_ABI_CAPABILITY_OWNED_KEY` | `0x4000000` | copied mutable key components, synchronized getters/setters/comparisons, and independently owned byte results |
| `SHOAL_ABI_CAPABILITY_HDFS` | `0x8000000` | Hadoop-configured HDFS client, owned streams, metadata, list/stat/remove/rename, cancellation, and deadlines |
| `SHOAL_ABI_CAPABILITY_RFILE_LOCALITY_GROUPS` | `0x10000000` | named locality-group transitions on owned RFile writers |
| `SHOAL_ABI_CAPABILITY_CLIENT_PARITY_CONTROLS` | `0x20000000` | exact batch-writer buffered-mutation size and process-wide logging level control |
| `SHOAL_ABI_CAPABILITY_STORAGE_ERROR_PARITY` | `0x40000000` | HDFS mkdir/chown, synchronized structured-log callbacks, and explicit Sharkbite compatibility error codes |
| `SHOAL_ABI_CAPABILITY_ZOOKEEPER_INSTANCE` | `0x80000000` | credential-free ZooKeeper instance identity resolution for compatibility constructors |

Shoal does **not** advertise instance status, compaction/import/export,
Python/wheel, or any other unimplemented surface until the API exists and has
coverage.

Compatibility rules:

- Older headers with newer libraries are safe: additive symbols, capability
  IDs, and trailing struct fields do not change existing numeric values or
  layouts.
- Newer headers with older libraries require dynamic resolution of every
  optional additive symbol before use, followed by the corresponding
  capability check where one exists. A capability check alone cannot make a
  hard symbol reference load-safe on Windows or with eager ELF binding.
- Libraries that predate this discovery surface do not export the new version
  and capability symbols, so those queries must also be resolved dynamically
  when mixing library vintages. A missing discovery symbol means
  "pre-discovery library", not "feature supported".

All forward-compatible input structs start with `uint32_t struct_size`. The
existing `*_init` helpers initialize and advertise only their V1 prefix, so a
newer library never writes beyond an older caller's allocation. Callers using
fields appended after V1 must initialize their full structure and set
`struct_size = sizeof(struct)` from the header they compiled against. Future
ABI revisions may append fields only at the end. Libraries must continue
accepting the smallest prefix they actually read (today the `*_V1_SIZE`
constants) and must ignore trailing bytes from newer callers. Existing fields
must never be reordered, removed, or assigned new meanings.

Version numbers change only when the public ABI contract changes:

- bump **major** (and therefore `SHOAL_ABI_VERSION`) for breaking ABI changes
  such as symbol removal/rename, signature or ownership contract changes,
  reordered/shrunk structs, or renumbered status/enum values
- bump **minor** for backward-compatible ABI growth such as new exported
  functions, appended struct fields guarded by `struct_size`, or new capability
  IDs/bits
- bump **patch** for compatible fixes and clarifications that do not change
  layout, numeric assignments, or advertised capability coverage

## Ownership and lifetime

- `shoal_connector_config` and all memory it references are borrowed only
  during `shoal_connector_create`; Shoal copies retained strings and bytes.
- `shoal_connector_create` returns an opaque, library-owned connector handle.
  Call `shoal_connector_close` to observe shutdown errors, then
  `shoal_connector_free` exactly once with the address of the handle variable.
- `shoal_client_create` copies its connector configuration, table name, and
  binary authorization labels and returns an owned facade with a default thread
  count of 10. Setters affect only subsequently created scanners and writers.
  `shoal_client_create_scanner` returns an owned scanner; the lazy
  `shoal_client_create_batch_writer` returns an owned
  `shoal_accumulo_writer`. Client close cancels and joins active facade calls;
  child operations coordinate with the same connector lifecycle and reject
  work once client close begins.
- `shoal_client_select_column` copies binary family/qualifier selections into
  the client. Direct single/multi-range client scans take an atomic snapshot of
  table, authorizations, columns, and thread count, return owned scan results,
  support per-call deadlines and one-shot cancellation, and are canceled and
  joined by client close.
  Freeing also performs best-effort close and sets the variable to `NULL`.
- A connector owns the bootstrap instance created for it. Callers do not
  separately close ZooKeeper resources.
- Connector identity and administration calls (`get_identity`, `list_tables`, `table_exists`, `create`,
  `delete`, `rename`, `flush`, property mutation, and effective property
  reads; namespace discovery/lifecycle/property reads; security operations;
  and table split listing/creation) accept per-call deadlines. Connector close
  prevents new calls, cancels and joins active administration calls, waits for any
  already-started scanner or batch-scanner calls to finish before final
  teardown, and remains idempotent while the handle stays alive.
- `shoal_connector_get_identity` returns one owned result containing copied
  instance-name, instance-ID, and principal strings. Initialize
  `shoal_connector_identity_view` with its init helper; the view borrows its
  pointers only until `shoal_connector_identity_free`, which is idempotent
  when called with the address of a `NULL` result variable.
- `shoal_range_create` and `shoal_iterator_setting_create` validate and copy
  every nested byte/string input into owned results. Their versioned views
  borrow result memory and expose range bound kinds, keys, inclusivity, and
  unboundedness plus iterator name/class/priority/options. The bound kind keeps
  empty row bounds distinct from unbounded bounds and preserves ROW versus KEY
  semantics for lossless reuse. These are local, non-blocking
  descriptor operations, so connector close, deadlines, and cancellation do
  not apply. Concurrent getters are safe while free is externally serialized.
- Scanner configuration, ranges, and all nested arrays/bytes are borrowed only
  for the creating or scan call. Shoal copies every value it retains.
- Configuration names, values, and defaults are length-delimited and copied,
  preserving embedded NUL bytes. Configuration handles and topology results
  are owned and released with NULL-safe, idempotent free functions.
- Root-tablet, manager, and server discovery use per-call deadlines and are
  cancelled by connector close. Root, ZooKeeper, and configuration getters
  are coordinated with close and return owned handles or snapshots.
- RFile readers, writers, and seekable relocations are owned opaque handles.
  Path, key, value, and column-family inputs are copied; top entries, values,
  ranges, and family accessors return owned results. Operations accept
  per-call deadlines, close cancels and joins active work, and every free is
  NULL-safe, idempotent, and clears the caller's handle.
- Key and range formatting/predicates are local copied-value operations.
  Key/value results own every nested byte slice. Authorization handles copy,
  sort, and deduplicate labels at construction and are immutable, making
  concurrent getters safe while free remains externally serialized.
- Scanner and batch-scanner handles support concurrent scan calls. Close
  cancels and joins in-flight calls and is idempotent while the handle remains
  alive; free performs best-effort close and sets the handle variable to
  `NULL`. Once connector close/free starts, new scan calls fail with
  `SHOAL_STATUS_CLOSED`, but already-started scans are allowed to finish
  before connector teardown.
- Streaming calls return an owned `shoal_scan_cursor`. Each
  `shoal_scan_cursor_next` call returns a separately owned scan result containing
  at most `max_entries`; that result remains valid after cursor close/free.
  Cursor close is idempotent, free is NULL-safe, and scanner, client, or
  connector close cancels and joins live cursor operations. Deadline and
  one-shot cancellation variants apply for the cursor's entire lifetime.
- Column-visibility constructors copy binary expressions. Visibility, node,
  and node-expression handles are immutable; tree, child, normalized, term,
  expression, and flatten results are independently owned. Evaluators clone
  authorizations and synchronize evaluation with replacement. Parse errors
  expose C-owned reason/terms/offset through a versioned borrowed view.
- One-shot cancellation handles can interrupt single or batch scans without
  closing the scanner or connector. Cancel is thread-safe and idempotent;
  free cancels and joins registered scans and clears the caller's handle.
  Already-canceled handles immediately cancel later scans.
- Table-cache and full-discovery invalidation are connector-scoped local
  operations. Table IDs are copied before return, calls coordinate with
  connector close, and successful invalidation does not perform network I/O.
- Table list and effective-property results own all returned strings. Views
  from `shoal_table_list_get` and `shoal_table_properties_get` remain valid
  until the matching result is freed. Effective properties preserve explicit
  empty string values.
- Row-bounded flush distinguishes a `NULL` bound pointer (unbounded) from a
  non-NULL zero-length bound (the empty row) and copies binary bounds before
  invoking Go. Constraint listing returns an owned, stably ordered snapshot;
  its caller-sized views borrow class names until list free. All live
  maintenance calls accept deadlines and participate in connector-close
  cancellation and active-call joining.
- Namespace lists and property results own all returned strings. Versioned
  property results additionally preserve the Accumulo property-store version.
- Authorization and split results own binary-safe byte arrays. Their borrowed
  views remain valid until `shoal_bytes_list_free`.
- Passwords, authorization arrays, and split arrays are copied before calls
  return. Empty passwords are represented by a non-NULL `shoal_bytes` input
  whose length is zero.
- Security failures retain the stable Accumulo user and code on the owned
  `shoal_error`; `shoal_error_security_user` and
  `shoal_error_security_code` return borrowed strings or `NULL`.
- Scan results own binary-safe key/value storage. Views returned by
  `shoal_scan_result_get` remain valid until `shoal_scan_result_free`.
- Mutations copy every row, column, visibility, and value input. BatchWriter
  add snapshots the mutation, so callers may immediately reuse or free it.
- BatchWriter operations accept per-call deadlines. Close prevents new calls,
  cancels and joins active calls, and flushes the remaining buffer; free uses a
  bounded best-effort close.
- The owned buffered writer copies its batch-writer configuration and every
  binary update, creates the underlying BatchWriter lazily on the first
  update, combines adjacent updates for one row, and submits the pending
  mutation on row change or close. `put` replaces timestamp zero with the
  current Unix time in milliseconds; `put_delete` preserves an explicit zero.
  Calls are serialized, connector close cancels active work, and writer close
  cancels, joins, flushes, and closes with the caller's deadline.
- Write failures optionally return an owned `shoal_write_failure` containing
  ambiguous-commit/retry flags plus failed extents, constraint violations,
  authorization failures, and cleanup failures. Borrowed views remain valid
  until `shoal_write_failure_free`.
- Failed calls can return an owned `shoal_error`. Its message is borrowed from
  that object and remains valid until `shoal_error_free`.
- Compatibility error getters distinguish Sharkbite's source category from
  the Python-facing exception. Application failures map to
  `ClientException`; closed and cancelled operations retain
  `IllegalStateException` and `IterationInterruptedException` source names
  while mapping to `RuntimeError`, matching pybind11's unregistered-exception
  behavior. The immutable names are library-owned and concurrent getters are
  safe until the owned error is freed.
- Status mapping is stable across the ABI: duplicate table names return
  `SHOAL_STATUS_ALREADY_EXISTS`, invalid table/property inputs return
  `SHOAL_STATUS_INVALID_ARGUMENT`, and missing manager/client-service
  endpoints return `SHOAL_STATUS_UNAVAILABLE`.
- Do not copy opaque handles, access them after free, or free/use the same
  handle concurrently. No Go pointer or Go-managed buffer is exposed through
  the ABI.

## Concurrency, process, and failure contract

- Native calls may run concurrently on distinct handles. The concurrency
  guarantees listed above apply to shared scanner, connector, cancellation,
  writer, and immutable-result handles. Freeing a handle must be externally
  serialized with every use of that same handle.
- A zero timeout means no per-call deadline. A positive timeout bounds the
  operation and returns `SHOAL_STATUS_DEADLINE_EXCEEDED`; close cancels and
  joins active work as documented for each handle. Retriable writes are
  replayed only when non-acceptance is proved. Retry exhaustion and ambiguous
  commit remain distinct, structured terminal outcomes.
- Allocation failure returns `SHOAL_STATUS_OUT_OF_MEMORY`. Output handles stay
  `NULL`, partially built owned results are destroyed, and existing input
  handles remain valid unless the operation's normal close contract says
  otherwise.
- Unix `fork()` does not clone a reusable Shoal/Go runtime. After a process has
  loaded the library, the child must not call any `shoal_*` function, create a
  new Shoal handle, close/free an inherited handle, or run a finalizer before
  `exec()`. Inherited handles remain valid only in the parent. The supported
  child path is immediate `exec()` of a fresh process, which may then load the
  library and create independent handles. This explicit unsupported contract
  replaces undefined post-fork handle reuse.

## Bootstrap modes

- `SHOAL_BOOTSTRAP_STATIC` uses `instance_name` and `instance_id`. It is useful
  for connector lifecycle and operations that do not require ZooKeeper
  discovery.
- `SHOAL_BOOTSTRAP_ZOOKEEPER` uses `instance_name` and a comma-separated
  `zookeeper_servers` list. A zero session or bootstrap timeout selects the
  30-second default. `instance_secret` is optional.

The ABI currently exposes connector bootstrap/lifecycle, synchronous and
streaming Scanner/BatchScanner reads, chunked high-level client streams,
Mutation/BatchWriter writes with owned structured
failures, table and namespace administration, complete merged security
administration, and binary-safe table split listing/creation. Instance status,
compaction, bulk import/export, and Python wheel APIs remain deferred.

`docs/sharkbite-compatibility.md` enumerates the full Sharkbite compatibility
contract, maps every element to this ABI, and defines the implementation-entry
and release gates for any Sharkbite-compatible Python wheel.
