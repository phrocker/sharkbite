# Native WAL sorter proof for #172

`run.sh` builds an unsorted, two-tablet WAL with Sharkbite's Go `NativeWriter`.
It runs the pinned Accumulo 4 image by immutable digest, calls the pinned
`LogSorter.LogProcessor.sort` method, checks its RFile and finished marker,
then calls `ResolvedSortedLog.resolve` and `SortedLogRecovery.recover` for each
tablet. It checks exact rows and values, implicit system time, an explicit zero
timestamp and delete, and excludes a mutation before a completed compaction.
The WAL path uses Accumulo's required `wal/<server>/<uuid>` layout. The
`Native WAL sorter proof` workflow reruns this fixture on qwal and harness
changes; a failed assertion or Java error fails the job.

Run locally with Docker and Go:

```bash
bash test/accumulo/native-wal-sorter/run.sh
```

The pinned image is the same digest in `test/accumulo/harness.py` and
`test/accumulo/docker-compose.yml`. `make test-accumulo-static` verifies that
contract and the source revision/hash in `test/accumulo/image/Dockerfile`.
The Java probe uses a small local `ServerContext` adapter to run the unmodified
sorter and recovery classes without a live manager. It does not prove shared
durable publication, reassignment, split/merge handling, metadata ownership,
or safe WAL cleanup. In particular, reassignment copies non-UNREFERENCED WAL
markers, so shared logs must be filtered by `DEFINE_TABLET`; stale CLOSED
markers after RFile commit may re-add retired logs. Those remain live-gate
requirements of #172.
