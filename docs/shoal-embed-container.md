# shoal-embed container

`shoal-embed` is published on version tags as a multi-architecture
(`linux/amd64`, `linux/arm64`) image:

```text
ghcr.io/phrocker/sharkbite/shoal-embed
```

Use an immutable version tag or digest in production:

```bash
docker run --rm \
  -p 9876:9876 -p 9877:9877 \
  -v shoal-data:/var/lib/shoal \
  ghcr.io/phrocker/sharkbite/shoal-embed:1.2.3
```

The non-root image starts `shoal-embed serve` by default, persists data under
`/var/lib/shoal`, exposes gRPC on `9876`, and exposes `/healthz`, `/readyz`,
`/stats`, and `/metrics` over HTTP on `9877`. Pass normal `shoal-embed`
arguments after the image name to override the default command.

## gRPC clients and the protocol definition

Non-Go consumers only need the version-matched `embed.proto`. It is included
in every image at `/usr/share/shoal/proto/embed.proto`:

```bash
id=$(docker create ghcr.io/phrocker/sharkbite/shoal-embed:1.2.3)
docker cp "$id:/usr/share/shoal/proto/embed.proto" ./embed.proto
docker rm "$id"
protoc --python_out=. --grpc_python_out=. embed.proto
```

The same file can be downloaded without a container runtime from
`https://raw.githubusercontent.com/phrocker/sharkbite/v1.2.3/proto/embed.proto`.
The publication workflow removes the leading `v` from image tags: source tag
`v1.2.3` publishes image tag `1.2.3`. Use the same semantic version in both
places.

Call `Status` before using a capability that an older server might lack. Its
`build_version`, `build_commit`, `source_date`, and `source_modified` identify
the running binary; `protocol_version` is `shoal.embed.v1`. The `features`
list is sorted and uses exact identifiers. For example, vector searches need
`rpc.scan_v2`, `safety.scan_v2.vector_identity`, and
`safety.scan_v2.vector_search`. Conditional writes need `rpc.conditional_write` and
`safety.conditional_write.conditions`. A client must treat a missing field or
feature as unsupported. Older servers return the original two Status fields,
so this rule makes mixed-version clients fail closed. Older protobuf clients
can still read the table fields and ignore the new fields.

For a vector request, a client using stubs generated from this proto can
preflight with a useful error:

```python
required = {
    "rpc.scan_v2",
    "safety.scan_v2.vector_identity",
    "safety.scan_v2.vector_search",
}
missing = required.difference(status.features)
if missing:
    raise RuntimeError(
        f"ShoalEmbed {status.build_version or 'older build'} lacks "
        f"{', '.join(sorted(missing))}"
    )
```

The image passes its revision and source commit date into the binary, separately
from the OCI image creation time. A local Go build reads the same values from
VCS build metadata when available. `build_commit` is `unknown` when neither
source provides a revision.
`source_modified` is `unknown` when the build has no VCS state, as in the
container build, rather than claiming its input tree was clean.

## Reproducible local build and smoke test

The build pins both base images by digest and records the version, revision,
and creation timestamp in OCI labels:

```bash
make container-build
make container-smoke
```

Override `IMAGE`, `VERSION`, `REVISION`, `CREATED`, or `SOURCE_DATE` when reproducing a
release build. The smoke test verifies the non-root runtime configuration,
OCI metadata, embedded proto, version, and default readiness endpoint.

Version tags matching `v*` publish semver tags to GHCR with BuildKit
provenance and an SBOM. The publication workflow does not publish branch or
pull-request images.

The image path follows `ghcr.io/<owner>/<repository>/shoal-embed`, and its OCI
source label links it to the publishing repository. Releases use that
repository's `GITHUB_TOKEN` with `packages: write`; an existing package must
also grant the repository Actions write access in its GHCR package settings.
