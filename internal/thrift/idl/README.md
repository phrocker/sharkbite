# Vendored Apache Accumulo Thrift IDLs

These files are copied without modification from Apache Accumulo 4 source
revision `1a716b2c1bb5762ead4b46d2bc4f53e13873b314`:

<https://github.com/apache/accumulo/tree/1a716b2c1bb5762ead4b46d2bc4f53e13873b314/core/src/main/thrift>

That revision identifies itself as `4.0.0-SNAPSHOT` and pins
`version.thrift` to `0.17.0`. Its IDLs match the Accumulo 4 RPC surface shoal
currently implements. Shoal vendors only the transitive closure required by
its tablet scan/ingest/management and compaction coordinator surfaces:

- `tabletscan.thrift`
- `compaction-coordinator.thrift`
- `tabletmgmt.thrift`
- `tabletserver.thrift`
- `manager.thrift`
- `data.thrift`
- `client.thrift`
- `security.thrift`

The IDLs retain their Apache Software Foundation license headers. `NOTICE` is
the corresponding Accumulo notice from the pinned revision. The repository
root `LICENSE` contains the Apache License 2.0.

Regenerate the internal Go bindings with Apache Thrift 0.17.0:

```sh
make thrift-gen
make thrift-verify
```

The generator package prefix is
`github.com/phrocker/shoal-oss/internal/thrift/gen/`; generated types remain
inside Go's `internal` boundary.
