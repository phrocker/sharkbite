# shoal-embed container

`shoal-embed` is published on version tags as a multi-architecture
(`linux/amd64`, `linux/arm64`) image:

```text
ghcr.io/phrocker/shoal-oss/shoal-embed
```

Use an immutable version tag or digest in production:

```bash
docker run --rm \
  -p 9876:9876 -p 9877:9877 \
  -v shoal-data:/var/lib/shoal \
  ghcr.io/phrocker/shoal-oss/shoal-embed:1.2.3
```

The non-root image starts `shoal-embed serve` by default, persists data under
`/var/lib/shoal`, exposes gRPC on `9876`, and exposes `/healthz`, `/readyz`,
`/stats`, and `/metrics` over HTTP on `9877`. Pass normal `shoal-embed`
arguments after the image name to override the default command.

## gRPC clients and the protocol definition

Non-Go consumers only need the version-matched `embed.proto`. It is included
in every image at `/usr/share/shoal/proto/embed.proto`:

```bash
id=$(docker create ghcr.io/phrocker/shoal-oss/shoal-embed:1.2.3)
docker cp "$id:/usr/share/shoal/proto/embed.proto" ./embed.proto
docker rm "$id"
protoc --python_out=. --grpc_python_out=. embed.proto
```

The same file can be downloaded without a container runtime from
`https://raw.githubusercontent.com/phrocker/shoal-oss/v1.2.3/proto/embed.proto`.
The publication workflow removes the leading `v` from image tags: source tag
`v1.2.3` publishes image tag `1.2.3`. Use the same semantic version in both
places.

## Reproducible local build and smoke test

The build pins both base images by digest and records the version, revision,
and creation timestamp in OCI labels:

```bash
make container-build
make container-smoke
```

Override `IMAGE`, `VERSION`, `REVISION`, or `CREATED` when reproducing a
release build. The smoke test verifies the non-root runtime configuration,
OCI metadata, embedded proto, version, and default readiness endpoint.

Version tags matching `v*` publish semver tags to GHCR with BuildKit
provenance and an SBOM. The publication workflow does not publish branch or
pull-request images.
