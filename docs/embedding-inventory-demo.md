# Embedding inventory demo

The optional inventory view is a read-only operator surface for one Accumulo
table. It displays a file as `has_embeddings` or `no_embeddings` only when the
existing sidecar resolver verifies the exact live metadata entry, source
footer, immutable candidate, and authenticated publication marker. A footer
hint is shown separately and never changes the verified count.

## See the UI with sample data

```sh
go run ./cmd/shoalctl embedding-inventory --demo --listen 127.0.0.1:8080
```

Open `http://127.0.0.1:8080/`. The sample banner remains visible throughout
the page. `GET /api/inventory` returns the same sample report as JSON. The
demo needs no Accumulo, ZooKeeper, or storage credentials.

## Inspect a cluster

Set `SHOAL_PASSWORD` for the metadata reader. For HDFS, also set
`SHOAL_HDFS_NAMENODE`. The sidecar keyring must be the owner-only keyring for
the configured authority namespace. For example:

```sh
go run ./cmd/shoalctl embedding-inventory \
  --zk zk.example:2181 --instance accumulo --table 7 \
  --storage hdfs --sidecar-prefix shoal/embedding-state \
  --sidecar-namespace cluster-a --keyring /secure/embedding-keyring.json \
  --listen 127.0.0.1:8080
```

Without `--listen`, the command writes one JSON report to stdout. The browser
view binds only to a numeric loopback address and refuses requests with a
different `Host` header. Refreshing re-reads live tablet metadata after
checking each file, so a file replaced or split during the check is shown as
unknown. The browser and JSON routes accept only `GET`.

The assessment names why each file remains unknown and what an operator needs
to investigate. It does not apply changes. Write-mode sidecar backfill and
production verification of `no_embeddings` need the remaining #171 backend
gates; the page therefore offers no apply action. An unreadable file or
unverifiable marker stays unknown even if its footer suggests a state.
