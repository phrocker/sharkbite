# Shoal

Shoal is an evidence and enforcement layer for AI systems. It keeps the chain
from **what was observed** to **what an agent may do** inspectable:

- authorized retrieval returns exact evidence and provenance;
- snapshots make the source state reproducible;
- decisions, outcomes, and adjudications are append-only records;
- actions are checked against declared effects and host policy;
- missing authority or evidence fails closed.

Shoal is a set of Go libraries, local commands, and services. It does not try
to be an agent runtime or a model vendor.

## Choose a path

| You want to… | Start here |
|---|---|
| Explore a cited local corpus | [Explorer quickstart](#explorer) |
| Use the browser workspace | [Web workspace](#web-workspace) |
| Measure local model decisions | [Local decisions](docs/local-decisions.md) |
| Build against the knowledge contracts | [Public packages](#public-packages) |
| Run a local sorted KV engine | [Embedded engine](#embedded-engine) |
| Evaluate Accumulo compatibility | [Compatibility harness](test/accumulo/README.md) |
| Understand the product direction | [Platform product plan](docs/platform-product-plan.md) |

Capability status and validation evidence are tracked in
[`FEATURES.md`](FEATURES.md). Proposed or shadow-only workflows are labeled as
such in their design documents; they are not production guarantees.

## Explorer

The Explorer is the easiest way to try Shoal. It ingests Markdown or plain text,
builds deterministic document and graph structure, and returns claims with
revision-specific citations.

```bash
go run ./cmd/shoal-explore ingest \
  -data .shoal/explorer \
  -file docs/platform-product-plan.md

go run ./cmd/shoal-explore ask \
  -data .shoal/explorer \
  -question "what gates local to cluster promotion?"
```

The answer includes source ranges, graph paths, a snapshot identity, and an
execution trace. When the corpus does not support an answer, Explorer returns a
grounded no-answer with a logged issue.

The default provider is deterministic and offline. Select a configured local or
compatible provider when needed:

```bash
go run ./cmd/shoal-explore ask \
  -data .shoal/explorer \
  -provider ollama \
  -question "summarize the authorization boundary"
```

Explorer's retrieval and inference contracts are provider-neutral. See
[`docs/explorer-demo-walkthrough.md`](docs/explorer-demo-walkthrough.md) for a
complete local walkthrough.

## Web workspace

`shoal-explore-web` puts an authenticated browser workspace over an existing
corpus:

```bash
go run ./cmd/shoal-explore-web \
  -data .shoal/explorer \
  -listen 127.0.0.1:8080 \
  -dev-auth
```

The workspace provides document browsing, retrieval with exact citations, graph
navigation, and ontology views. When a host configures the local decision
providers, it also exposes a **Decisions** tab for receipts, registrations,
cohort exports, outcomes, and adjudications. These records support measurable
shadow decisions; the console does not promote or exclude a model. Provider
wiring for the default host is tracked in
[issue #645](https://github.com/phrocker/shoal-oss/issues/645).

Every request carries an authorization decision enforced below the transport.
Unauthenticated requests receive `401`; the service does not guess an identity.
`-dev-auth` is limited to loopback. Deployment and OIDC configuration are
documented in
[`docs/shoal-explore-web-deploy.md`](docs/shoal-explore-web-deploy.md).

## Why the evidence boundary matters

Shoal derives citation identities from the evidence it actually resolves, so a
caller cannot invent an anchor ID. Unauthorized documents are removed before
ranking and do not influence results. A restricted term and a nonexistent term
use the same encoded response, preventing authorization probing.

Snapshots, revisions, `as_of`, ontology versions, and authorization fingerprints
make a result inspectable later. A valid record still describes what was known at
that point; it does not make a changed world true.

Execution has a separate boundary. Actions declare their effects, hosts declare
what they permit, and mismatches fail closed. Reading a corpus, transmitting
content, and mutating an external system are distinct effects rather than a
single privilege ladder.

## Public packages

The main contracts are organized by concern:

- [`pkg/document`](pkg/document), [`pkg/graph`](pkg/graph), and
  [`pkg/ontology`](pkg/ontology) model source structure and relationships.
- [`pkg/retrieval`](pkg/retrieval) and [`pkg/contextpack`](pkg/contextpack) build
  bounded, authorized evidence.
- [`pkg/inference`](pkg/inference) defines provider-neutral grounded inference.
- [`pkg/decision`](pkg/decision) defines measured task, prediction, outcome, and
  evaluation records.
- [`pkg/explorer`](pkg/explorer) provides the authorized workspace and Explorer
  integration.
- [`pkg/interaction`](pkg/interaction) and
  [`pkg/explorer/fleet`](pkg/explorer/fleet) record bounded execution.

The local-decision contracts, provenance rules, dataset boundaries, and
shadow-learning loop are described in
[`docs/local-decisions.md`](docs/local-decisions.md) and
[`docs/decision-contracts.md`](docs/decision-contracts.md).

## Embedded engine

`shoal-embed` is a standalone sorted key/value engine. It has no ZooKeeper or
Accumulo dependency and stores durable RFile, Parquet, or mixed-format tablets.

```bash
go run ./cmd/shoal-embed init \
  --table graph \
  --splits "entity:,event:,knowledge:" \
  --data ~/.shoal/data

go run ./cmd/shoal-embed write \
  --table graph --data ~/.shoal/data < mutations.jsonl

go run ./cmd/shoal-embed scan \
  --table graph --data ~/.shoal/data
```

Use the default RFile profile for point and range lookups or Accumulo
interoperability. Use `--workload analytical` for Parquet-oriented scans and
aggregation. The storage and SQL details are in
[`docs/shoalql-accumulo.md`](docs/shoalql-accumulo.md) and
[`docs/explorer-accumulo-adapter.md`](docs/explorer-accumulo-adapter.md).

Shoal also includes a read-optimized Accumulo-compatible serving role,
tablet-server and compactor roles, and a C ABI/Python compatibility layer.
Those roles have separate validation gates; see
[`ARCHITECTURE.md`](ARCHITECTURE.md), [`FEATURES.md`](FEATURES.md), and
[`python/README.md`](python/README.md).

## Build and test

Requirements for the normal Go workflow are Go 1.25+, Python 3.9+ for the
compatibility package, `make`, and a native C toolchain.

```bash
git clone https://github.com/phrocker/shoal-oss.git
cd shoal-oss

make build
make test
```

Useful targets:

```bash
make capi                  # build the stable C connector ABI
make thrift-gen            # regenerate checked-in Thrift bindings
make thrift-verify         # verify generated bindings are current
make test-accumulo-static  # validate the pinned harness without Docker
make test-accumulo        # run the live Accumulo harness
make conformance-live      # run live release conformance checks
```

The live Accumulo targets are opt-in. Docker absence is reported as unsupported,
not as a passing conformance result. See
[`test/accumulo/README.md`](test/accumulo/README.md).

## Documentation map

- [Explorer demo](docs/explorer-demo-walkthrough.md)
- [Explorer public contract](docs/explorer-public-contract.md)
- [Inference contracts](docs/inference-contracts.md)
- [Local decisions and learning](docs/local-decisions.md)
- [Decision HTTP APIs](docs/decision-http-api.md)
- [Adjudication](docs/decision-adjudication.md)
- [Deployment and effects](docs/effects-gateway-deploy.md)
- [Embedded engine](docs/shoal-embed-container.md)
- [Iterator capability registry](docs/iterator-capability-registry.md)
- [Offline compaction design](docs/offline-compaction-design.md)
- [Offline compaction runbook](docs/offline-compaction.md)
- [C ABI and Python compatibility](capi/README.md)
- [Distributed serving architecture](ARCHITECTURE.md)
- [Accumulo test harness](test/accumulo/README.md)

For generated API or compatibility details, start from the package README or
the relevant document above rather than this overview.

## Repository layout

```
cmd/         user-facing commands and services
pkg/         public knowledge, inference, decision, and Explorer contracts
internal/    engine, storage, protocol, serving, and compatibility internals
docs/        architecture, validation, deployment, and operator guides
test/        conformance and integration harnesses
python/      Sharkbite import-compatible package
```

## License

Apache License, Version 2.0.
