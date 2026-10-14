#!/usr/bin/env bash
#
# Licensed to the Apache Software Foundation (ASF) under one or more
# contributor license agreements. See the NOTICE file distributed with
# this work for additional information regarding copyright ownership.
# The ASF licenses this file to you under the Apache License, Version 2.0.

# Builds Dockerfile.shoal-gateway and checks what the chart relies on: the
# entrypoint the Deployment's command names, the non-root account its
# securityContext names, and a binary that runs. `grace-period` is the smoke
# because it needs no explorer and is the figure the chart computes.

set -euo pipefail

IMAGE="${IMAGE:-shoal-gateway:smoke}"
VERSION="${VERSION:-smoke}"
REVISION="${REVISION:-$(git rev-parse HEAD)}"
CREATED="${CREATED:-$(git show -s --format=%cI HEAD)}"

docker build \
  --file Dockerfile.shoal-gateway \
  --build-arg "VERSION=${VERSION}" \
  --build-arg "REVISION=${REVISION}" \
  --build-arg "CREATED=${CREATED}" \
  --tag "${IMAGE}" \
  .

check() {
  local description="$1" actual="$2" expected="$3"
  if [ "${actual}" != "${expected}" ]; then
    echo "shoal-gateway image: ${description}: got ${actual}, want ${expected}" >&2
    exit 1
  fi
}

check "user" "$(docker image inspect "${IMAGE}" --format '{{.Config.User}}')" "65532:65532"
check "entrypoint" "$(docker image inspect "${IMAGE}" --format '{{json .Config.Entrypoint}}')" '["/usr/local/bin/shoal-gateway"]'
check "version label" "$(docker image inspect "${IMAGE}" --format '{{index .Config.Labels "org.opencontainers.image.version"}}')" "${VERSION}"
check "revision label" "$(docker image inspect "${IMAGE}" --format '{{index .Config.Labels "org.opencontainers.image.revision"}}')" "${REVISION}"

# Run as the chart runs it: read-only root, every capability dropped.
run() { docker run --rm --read-only --cap-drop ALL --security-opt no-new-privileges "${IMAGE}" "$@"; }
check "grace-period at the defaults" "$(run grace-period)" "225"
check "grace-period for T=10m, P=15s" "$(run grace-period -operation-timeout 10m -plane-timeout 15s)" "660"
run help >/dev/null

echo "shoal-gateway container smoke test passed (${IMAGE})"
