# Shoal Python binding

This is an independently usable **incremental binding**, not a complete
Sharkbite replacement and not a release under the reserved `sharkbite`
distribution name. The distribution is named `shoal-sharkbite`; it installs
the import-compatible modules `sharkbite` and `pysharkbite`.
The [normative scope ADR](../docs/sharkbite-client-scope.md) currently records
394 required rows, 390 satisfied rows, and 4 explicit core gaps; optional
Torch, pandas, embedded, historical C++, and deferred native macOS surfaces do
not block this package.

Supported now:

- deterministic Shoal shared-library discovery and ABI/capability negotiation;
- stable status-to-exception mapping, including Sharkbite `ClientException`;
- owned handle/result cleanup and idempotent context-manager close;
- `Connector`, `Client`, `Scanner`, `BatchScanner`, restartable sync/async
  `Results`, `Key`, and bounded scans through the streaming C ABI;
- binary-safe `Mutation` and `BatchWriter` APIs with deterministic
  flush/close and structured write-failure status mapping;
- table, namespace, and security administration through both direct
  connector helpers and Sharkbite-shaped `tableOps`, `namespaceOps`, and
  `securityOps` objects;
- context-managed HDFS clients and typed/raw streams, using Hadoop
  configuration rather than accepting credentials in Python;
- context-managed RFile sequential readers/writers, including named locality
  groups.

Unsupported legacy entry points are present only where useful for discovery and
raise `NotImplementedError` with a stable message. They never fabricate data.

Python loads Shoal with `ctypes.CDLL`, so blocking native calls release the GIL;
Python result copying and exception construction run only after the GIL is
reacquired. The native handle concurrency rules still apply: supported shared
operations may run from multiple Python threads, while close/free must not race
with arbitrary use of the same wrapper.

On Unix, inherited native state is intentionally unusable after `fork()`.
Every bound native function and `NativeAPI` construction checks the process
before entering Go and raises `ForkSafetyError` in a fork child. Do not close,
free, or otherwise reuse inherited objects there. Use `subprocess`, the
`spawn`/`forkserver` multiprocessing start methods, or immediate `exec()` and
construct fresh Shoal objects in the new interpreter. The parent process and
fresh exec-created subprocesses remain supported.

## Install and load

```text
python -m pip install ./python
set SHOAL_LIBRARY=C:\path\to\shoal.dll
```

On Linux/macOS set `SHOAL_LIBRARY` to `libshoal.so`/`libshoal.dylib`.
Platform wheels load their checksum-verified private `.libs` library. Source
installs require an explicit absolute `SHOAL_LIBRARY` path. System loader
search is disabled unless `SHOAL_ALLOW_SYSTEM_LIBRARY=1`; the current working
directory and repository build paths are never searched implicitly.

```python
from sharkbite import Client

with Client("accumulo", "zk1:2181", "root", "secret", table="events") as client:
    with client.scanner() as scanner:
        for key, value in scanner.scan(b"a", b"z"):
            print(key.row, value)
```

The library must expose ABI major 1. APIs check their exact capability set:
5/21/22 for scans, 6/7/8 for writes, and 9/10/11/12/19 for administration.
Storage uses capabilities 16 (RFile), 27 (HDFS), and 28 (named locality
groups). Capability 29 adds the exact buffered-writer queue accessor and
process-wide logging control.

`ScannerOptions.HedgedReads`, `ScannerOptions.RFileScanOnly`, and
`PythonIterator` remain import-compatible but raise stable `NotImplementedError`
messages when applied. These are approved divergences: use normal RPC scans,
`RFileOperations` for explicit RFile access, and Shoal's Go iterator runtime.
Shoal accepts Accumulo 4 configurations only, never exposes password read-back
or generated Thrift exceptions, and scopes transport pools per connector.

`tableOps(name).createScanner(auths=(), threads=10)` returns a deferred
`BatchScanner`. Add copied ranges with `addRange`/`withRange`, then call
`getResultSet()`; each synchronous or asynchronous iteration opens a fresh
cursor, and scanner/result context managers close blocked work safely.
Live-cluster server timeout/consistency and retry parity remain tracked by
[issue #74](https://github.com/phrocker/shoal-oss/issues/74); unit and compiled
ABI tests do not claim that external evidence.

```python
from sharkbite import Hdfs, Key, KeyValue, RFileOperations

with Hdfs("namenode", 8020) as hdfs:
    with hdfs.write("/tmp/value") as out:
        out.writeString("hello")
    with hdfs.read("/tmp/value") as source:
        assert source.readString() == "hello"

with RFileOperations.openForWrite("example.rf") as writer:
    writer.append(KeyValue(Key(b"z", b"default", b"", b"", 1), b"d"))
    writer.addLocalityGroup("named")
    writer.append(KeyValue(Key(b"a", b"named", b"", b"", 1), b"n"))
```

```python
from sharkbite import Connector, Mutation, TablePermissions

with Connector("accumulo", "zk1:2181", "root", "secret") as connector:
    table = connector.tableOps("events")
    table.create(recreate=True)
    with table.createWriter([], 4) as writer:
        with Mutation(b"row", _api=connector._api) as mutation:
            mutation.put(b"cf", b"cq", b"A", 7, b"value")
            writer.addMutation(mutation)
    connector.securityOps().grant_table_permission(
        "analyst", "events", TablePermissions.READ
    )
```

Online compaction, legacy chunked iterators, and other surfaces without an
equivalent stable C ABI remain explicit `NotImplementedError` paths.

See the repository's
[release policy](https://github.com/phrocker/shoal-oss/blob/main/docs/python-release.md)
for platform status, reproducible build commands, artifact verification,
checksums, and signing.
