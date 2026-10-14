# Exact Accumulo 4 local conformance harness

This opt-in harness starts a disposable ZooKeeper, HDFS, and Accumulo cluster
for local conformance work. Accumulo is built from the same exact source
revision as Shoal's vendored Thrift IDLs:

- Accumulo `4.0.0-SNAPSHOT`
- revision `1a716b2c1bb5762ead4b46d2bc4f53e13873b314`
- Accumulo Access revision `5d7a94492832121f507029d9d7e7627fd88e95ba`
- Hadoop `3.4.2`
- ZooKeeper `3.9.5`
- Thrift `0.17.0` (in the pinned Accumulo build)

The image build verifies SHA-512 hashes for the Accumulo and Accumulo Access
source archives and copies Hadoop and ZooKeeper from digest-pinned official
images. Accumulo Access is built from the last upstream revision compatible
with the pinned Accumulo source, avoiding the moving `1.0.0-SNAPSHOT`
artifact. The build requires network access to Maven Central, Docker Hub, and
GitHub.

The local harness pulls the resulting public GHCR image by immutable digest,
so normal test runs do not rebuild Accumulo or download Hadoop from the Apache
archive server.

## Commands

```bash
make test-accumulo-static  # config and command tests; Docker is not required
make accumulo-up           # clean old state, build, start, and wait
make accumulo-smoke        # run the Java client smoke against a running cluster
make accumulo-down         # remove containers, orphans, and named volumes
make test-accumulo         # full lifecycle with guaranteed cleanup
make conformance-replay    # deterministic per-gate JSON from replay fixtures
make conformance-live      # live JSON; invokes this Docker harness for client
```

`make test-accumulo` first removes stale project resources, starts the exact
cluster, polls with an unmodified Accumulo 4 Java client until the metadata
table is readable and a tablet server is registered, then:

1. creates a deterministic temporary table;
2. writes one mutation with `BatchWriter`;
3. explicitly flushes the writer and table;
4. scans and verifies the exact key/value;
5. deletes the table in a Java `finally` block.

The lifecycle runner prints Compose logs on failure and executes `down
--volumes --remove-orphans` on success, failure, Ctrl+C, and termination.
Container ports bind only to loopback. Set
`SHOAL_ACCUMULO_READY_TIMEOUT=<seconds>` to change the 300-second readiness
deadline.

## Docker availability

The static target always runs without Docker. Live targets require the Docker
CLI, Compose v2, and a reachable daemon. If any are unavailable, the runner
prints:

```text
SKIP (needs-docker): ...; live Accumulo 4 test was not run
```

and exits with status 2. This is an explicit unexecuted live test, not a
passing result.

## Machine-readable replacement verdicts

`cmd/shoal-conformance` emits schema-versioned JSON for the `tserver`,
`scanserver`, `compactor`, `promotion`, and `client` gates. Output order is stable and includes the Shoal commit, exact Accumulo version
and revision, GOOS/GOARCH, execution mode, adapter identity, every evidence
gate, selector, source-file SHA-256, and replay command, the checked-in fixture
path and digest, and any missing required production gates.

Replay mode executes the commands in
`test/conformance/fixtures/<gate>.json`. The concrete adapters cover client
CRUD/visibility/range contracts, promotion artifact before/after equivalence,
stateful scan continuation/cancel-resume, tserver lock/assignment, ingest,
minor-compaction, WAL recovery and fencing, and compactor
publication/completion/cancellation/restart/fencing. Fixtures bind every result to a named test and the
exact digest of its source file instead of treating package-wide success or a
stale selector as evidence. Run:

```bash
go build -o bin/shoal-conformance ./cmd/shoal-conformance
bin/shoal-conformance -mode replay -output verdict.json
```

Live mode builds the repository's `shoal-tserver` and `shoal-compactor`, starts
them beside the exact pinned Accumulo manager, ZooKeeper, and HDFS services,
and runs unmodified Accumulo Java API calls through the Shoal endpoints.
The tserver path verifies ServiceLock registration/manager assignment,
write/flush/scan, one-cell stateful continuation, minor-compaction visibility,
WAL recovery after `SIGKILL`, and lock fencing/re-registration. The compactor
path verifies coordinator publication/completion, Java readability and
canonical before/after equivalence, cancellation, durable completion replay,
and restart.

On a Docker host, run the exact independent gates:

```bash
python test/accumulo/harness.py validate
python -m unittest -v test/accumulo/test_harness.py
go test ./internal/conformance ./cmd/shoal-conformance
go build -o bin/shoal-conformance ./cmd/shoal-conformance
bin/shoal-conformance -mode replay -output replay-verdict.json
bin/shoal-conformance -mode live -required tserver,scanserver,client \
  -output tserver-live-verdict.json
bin/shoal-conformance -mode live -required compactor,promotion \
  -output compactor-live-verdict.json
```

Keep the JSON verdicts and complete Compose logs. A live pass must contain
`SHOAL_EVIDENCE` lines for ServiceLock/assignment, Java ingest, continuation,
minor compaction, WAL recovery, fencing, external-compaction publication and
completion, Java readability, promotion equivalence, cancellation, and
restart. Any missing Docker daemon returns exit 2 and `unsupported`; it is
never converted into a pass.

### Release-gate semantics

Each selected gate is `required` and has exactly one state:

- `pass`: every adapter requirement, including required live process wiring,
  ran successfully;
- `fail`: evidence was malformed or a required command ran and failed;
- `unsupported`: the environment or live adapter cannot execute the gate;
- `skipped`: the gate was omitted with `-required`.

Exit status is `0` only when every required gate passes, `1` when any required
gate or its exact evidence fails, and `2` when no required gate failed but at
least one is unsupported or skipped. Consequently a successful replay,
Docker absence, and unwired live roles cannot authorize a release. To
evaluate a slice independently, pass a
comma-separated list such as `-required scanserver,client`; all gates remain
present in the JSON so consumers do not confuse omission with success.

## Debugging and overrides

The Compose project is always `shoal-accumulo4-test`, so cleanup is isolated:

```bash
docker compose --project-name shoal-accumulo4-test \
  --file test/accumulo/docker-compose.yml ps
docker compose --project-name shoal-accumulo4-test \
  --file test/accumulo/docker-compose.yml logs --no-color
```

The Accumulo image is deliberately not environment-overridable: a successful
run must exercise the repository's pinned source revision. Run
`make test-accumulo-static` after any harness configuration change.
