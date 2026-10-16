# Sharkbite Python client

Sharkbite is a Python client for Accumulo-compatible storage, backed by the
Shoal stable C ABI. The package keeps the established import names:

```python
import sharkbite
import pysharkbite

assert pysharkbite.Client is sharkbite.Client
```

## Install

For supported Linux x86-64 systems, install the published wheel:

```bash
python -m pip install sharkbite
```

That wheel includes the verified Shoal native library. You do **not** need to
build Shoal separately.

Source installs and platforms without a published native wheel use an existing
Shoal shared library. Give its absolute path explicitly:

```bash
python -m pip install sharkbite          # or: python -m pip install ./python
export SHOAL_LIBRARY=/opt/shoal/libshoal.so
```

On Windows, use an absolute path to `shoal.dll`; on macOS, use an absolute path
to `libshoal.dylib`. System-library discovery is disabled unless you explicitly
set `SHOAL_ALLOW_SYSTEM_LIBRARY=1`. The loader never searches the current
working directory or repository build directories.

The loader verifies the bundled library checksum, checks ABI major version 1,
checks the minimum supported ABI version, and resolves capabilities before an
operation is allowed.

## Quick example

```python
from sharkbite import Client

with Client("accumulo", "zk1:2181", "root", "secret", table="events") as client:
    with client.scanner() as scanner:
        for key, value in scanner.scan(b"a", b"z"):
            print(key.row, value)
```

## What is included

The current binding provides:

- deterministic native-library discovery and ABI/capability negotiation;
- stable status-to-exception mapping, including `ClientException`;
- owned handles and results with idempotent context-manager cleanup;
- `Connector`, `Client`, `Scanner`, `BatchScanner`, synchronous and
  asynchronous results, `Key`, and bounded scans;
- binary-safe `Mutation` and `BatchWriter` operations with structured failure
  reporting;
- table, namespace, and security administration through connector helpers and
  `tableOps`, `namespaceOps`, and `securityOps`;
- context-managed HDFS clients and typed or raw streams;
- context-managed RFile readers and writers, including named locality groups.

The package accepts Accumulo 4 configurations. It does not expose generated
Thrift exceptions or password read-back. Transport pools are scoped per
connector.

## Concurrency and process safety

Blocking native calls release the Python GIL. Shared operations may run from
multiple Python threads, but closing or freeing a wrapper must not race with
use of that wrapper.

A fork child cannot reuse inherited native state. In a fork child, construct
fresh objects after `exec()`, or use the `spawn` or `forkserver` multiprocessing
start method. The parent process and a fresh exec-created child remain
supported.

## Compatibility boundaries

Some historical entry points remain importable for discovery but raise
`NotImplementedError` with a stable message. This includes hedged reads,
RFile-only scanner mode, and Python iterators; use normal RPC scans,
`RFileOperations`, and Shoal's Go iterator runtime instead.

Online compaction, legacy chunked iterators, and other surfaces without an
equivalent stable C ABI remain outside the binding. Live-cluster timeout,
consistency, and retry parity are tracked in [issue #74](https://github.com/phrocker/sharkbite/issues/74).

See the [release policy](https://github.com/phrocker/sharkbite/blob/main/docs/python-release.md)
for supported platforms, reproducible builds, artifact verification, checksums,
attestations, and the pre-release container chaos gate.
