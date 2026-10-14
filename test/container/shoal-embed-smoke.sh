#!/usr/bin/env bash
#
# Licensed to the Apache Software Foundation (ASF) under one or more
# contributor license agreements. See the NOTICE file distributed with
# this work for additional information regarding copyright ownership.
# The ASF licenses this file to you under the Apache License, Version 2.0.

set -euo pipefail

IMAGE="${IMAGE:-shoal-embed:smoke}"
VERSION="${VERSION:-smoke}"
REVISION="${REVISION:-$(git rev-parse HEAD)}"
CREATED="${CREATED:-$(git show -s --format=%cI HEAD)}"

docker build \
  --file Dockerfile.shoal-embed \
  --build-arg "VERSION=${VERSION}" \
  --build-arg "REVISION=${REVISION}" \
  --build-arg "CREATED=${CREATED}" \
  --tag "${IMAGE}" \
  .

test "$(docker image inspect "${IMAGE}" --format '{{.Config.User}}')" = "65532:65532"
test "$(docker image inspect "${IMAGE}" --format '{{json .Config.Entrypoint}}')" = '["/usr/local/bin/shoal-embed"]'
test "$(docker image inspect "${IMAGE}" --format '{{json .Config.Cmd}}')" = '["serve","--data=/var/lib/shoal","--address=0.0.0.0:9876","--metrics-address=0.0.0.0:9877"]'
test "$(docker image inspect "${IMAGE}" --format '{{index .Config.Labels "org.opencontainers.image.version"}}')" = "${VERSION}"
test "$(docker image inspect "${IMAGE}" --format '{{index .Config.Labels "org.opencontainers.image.revision"}}')" = "${REVISION}"

proto_container="$(docker create "${IMAGE}")"
container=""
proto_copy="build/container-smoke-proto-$$"
cleanup() {
  if [[ -n "${container}" ]]; then
    docker rm --force "${container}" >/dev/null 2>&1 || true
  fi
  docker rm "${proto_container}" >/dev/null 2>&1 || true
  rm -rf "${proto_copy}"
}
trap cleanup EXIT

rm -rf "${proto_copy}"
mkdir -p "${proto_copy}"
docker cp "${proto_container}:/usr/share/shoal/proto/embed.proto" "${proto_copy}/embed.proto"
cmp proto/embed.proto "${proto_copy}/embed.proto"
rm -rf "${proto_copy}"

test "$(docker run --rm --entrypoint /usr/local/bin/shoal-embed "${IMAGE}" version)" = "shoal-embed ${VERSION}"

container="$(docker run --detach --publish 127.0.0.1::9877 "${IMAGE}")"
host_port="$(docker port "${container}" 9877/tcp | awk -F: 'NR == 1 { print $NF }')"
for _ in $(seq 1 30); do
  if curl --fail --silent "http://127.0.0.1:${host_port}/readyz" >/dev/null \
    && curl --fail --silent "http://127.0.0.1:${host_port}/healthz" >/dev/null; then
    echo "shoal-embed container smoke test passed (${IMAGE})"
    exit 0
  fi
  sleep 1
done

docker logs "${container}" >&2
echo "shoal-embed did not become ready" >&2
exit 1
