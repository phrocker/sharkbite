# ShoalEmbed Python client

`shoal-embed-client` is the pure-Python gRPC client for the Sharkbite
`ShoalEmbed` service. It includes versioned protobuf stubs and the canonical
`embed.proto`; it does not load the native `sharkbite` C ABI package.

Install the wheel and import its separate namespace:

```bash
python -m pip install shoal-embed-client
```

```python
from shoal_embed_client import EmbedClient, PROTOCOL_VERSION, __version__, embed_pb2
```

`__version__` follows the Sharkbite release tag (`1.4.4` for this source
version); `PROTOCOL_VERSION` is `shoal.embed.v1`. The wheel declares
`grpcio>=1.74.0,<2` and `protobuf>=6.31.1,<7` as runtime dependencies.
Generated modules are also available as `embed_pb2` and `embed_pb2_grpc`, but
direct use of their raw stub skips the negotiation safeguards below.

`EmbedClient` accepts a caller-owned gRPC channel. Construct a secure channel
with your deployment's CA and authentication credential; the client never
creates an insecure channel for you. See
[`examples/secure_client.py`](examples/secure_client.py) for a runnable Status
preflight and vector scan.
The generated messages use the standard protobuf descriptor pool for normal
imports and fall back to a private pool if an older hand-generated `embed.proto`
is already registered in the process.
An operation's `timeout` covers both the Status preflight and the requested RPC.

## Compatibility

| Server | Plain v1 RPCs | Safety-critical options |
| --- | --- | --- |
| Older v1, no Status feature fields | Allowed after Status | Refused with a named missing capability |
| v1 with required features | Allowed | Uses `CreateTableV2`, `ConditionalWrite`, or `ScanV2` |
| v1 missing a required feature | Allowed | Refused before the effect-bearing call |
| Features advertised without protocol version | Allowed | Refused: protocol cannot be verified |
| Explicit different protocol version | Refused after Status | Refused after Status |

For vector search, the client requires `rpc.scan_v2`,
`safety.scan_v2.vector_identity`, and `safety.scan_v2.vector_search`. For
conditional writes it requires `rpc.conditional_write` and
`safety.conditional_write.conditions`; for an embedded table default it
requires `rpc.create_table_v2` and
`safety.create_table_v2.default_embedding`. `scan` also uses `ScanV2` for term,
edge, score, and as-of options; `create_table` uses `CreateTableV2` for an
explicit workload or file format. Those fields predate the V2 RPCs, so a server
implementing V2 understands them. `scan`, `write`, and `create_table` route to
the versioned RPC whenever those fields are set. They never retry such a
request through an older RPC. A load balancer
that selects an older replica after Status returns `UNIMPLEMENTED`; the client
reports that as `IncompatibleServer`. Status is checked on every operation,
so a cached capability list cannot outlive a deployment change.

Targeted compaction (`CompactRequest.workload` or `file_format`) has no
versioned RPC. `EmbedClient.compact` refuses those options because a mixed
fleet could silently ignore them. Plain compaction remains available; direct
use of the generated stub for targeted compaction requires the caller to
enforce a homogeneous server deployment.

New fields and unknown feature identifiers from a later `shoal.embed.v1`
server are ignored; clients require exact identifiers for the behavior they
use. A server that advertises another protocol version is rejected. Server
authentication, authorization, and effect idempotency remain server and
target responsibilities.

## Reproduce the stubs and wheel

From the repository root:

```bash
python -m pip install -r python/embed-client/requirements-codegen.txt
python python/embed-client/scripts/generate.py --check
python -m build python/embed-client
```

Without `--check`, `generate.py` regenerates the checked-in modules and the
packaged proto from `proto/embed.proto`. The generator refuses dependency
versions other than those pinned in `requirements-codegen.txt`. CI checks for
drift, builds the wheel, installs it in clean Python environments, and runs
the mixed-version client tests.

For the first PyPI release, register a pending Trusted Publisher for project
`shoal-embed-client` with GitHub owner `phrocker`, repository `sharkbite`,
workflow `python-embed-client.yml`, and environment `pypi`. Then run that
workflow manually on the matching release tag with `publish_to_pypi=true`.
The workflow builds and tests the wheel before requesting the `pypi`
environment's approval. The existing `sharkbite` publisher is a separate
PyPI project and does not authorize this package.
