#!/usr/bin/env bash
#
# Licensed to the Apache Software Foundation (ASF) under one or more
# contributor license agreements. See the NOTICE file distributed with
# this work for additional information regarding copyright ownership.
# The ASF licenses this file to you under the Apache License, Version 2.0.

# Builds Dockerfile.shoal-effects-ssh and checks what the chart relies on: the
# entrypoint the Deployment and the journal-init Job name, the non-root
# account their securityContext names, and a binary that runs under a
# read-only root with every capability dropped. The journal is the smoke:
# `journal init` on a fresh directory prints a 32-hex-digit identity, a second
# init refuses, `journal list` reads it, and `serve` with nothing to serve
# exits as a usage error rather than listening.
#
# The image's base layers are the ones Dockerfile.shoal-gateway uses, so the
# CI job that builds both pulls each once (#638).

set -euo pipefail

IMAGE="${IMAGE:-shoal-effects-ssh:smoke}"
VERSION="${VERSION:-smoke}"
REVISION="${REVISION:-$(git rev-parse HEAD)}"
CREATED="${CREATED:-$(git show -s --format=%cI HEAD)}"

docker build \
  --file Dockerfile.shoal-effects-ssh \
  --build-arg "VERSION=${VERSION}" \
  --build-arg "REVISION=${REVISION}" \
  --build-arg "CREATED=${CREATED}" \
  --tag "${IMAGE}" \
  .

check() {
  local description="$1" actual="$2" expected="$3"
  if [ "${actual}" != "${expected}" ]; then
    echo "shoal-effects-ssh image: ${description}: got ${actual}, want ${expected}" >&2
    exit 1
  fi
}

check "user" "$(docker image inspect "${IMAGE}" --format '{{.Config.User}}')" "65532:65532"
check "entrypoint" "$(docker image inspect "${IMAGE}" --format '{{json .Config.Entrypoint}}')" '["/usr/local/bin/shoal-effects-ssh"]'
check "version label" "$(docker image inspect "${IMAGE}" --format '{{index .Config.Labels "org.opencontainers.image.version"}}')" "${VERSION}"
check "revision label" "$(docker image inspect "${IMAGE}" --format '{{index .Config.Labels "org.opencontainers.image.revision"}}')" "${REVISION}"

# Run as the chart runs it: read-only root, every capability dropped, the
# journal on a writable mount owned by the runtime account.
journal="$(mktemp -d)"
# The files the container writes are owned by uid 65532; the directory is
# world-writable, so the runner can still remove them.
trap 'rm -rf "${journal}" 2>/dev/null || true' EXIT
chmod 0777 "${journal}"
run() {
  docker run --rm --read-only --cap-drop ALL --security-opt no-new-privileges \
    -v "${journal}:/var/lib/effects-ssh" "${IMAGE}" "$@"
}
run help >/dev/null

# serve refuses a configuration it cannot read before it listens (exit 2).
code=0
run serve -config /nonexistent -listen 127.0.0.1:8443 -allow-plaintext-loopback \
  -caller-token-file /nonexistent -journal-dir /var/lib/effects-ssh \
  -journal-id 0123456789abcdef0123456789abcdef -journal-retention 48h >/dev/null 2>&1 || code=$?
check "serve without a configuration exits" "${code}" "2"

id="$(run journal init -journal-dir /var/lib/effects-ssh)"
if ! [[ "${id}" =~ ^[0-9a-f]{32}$ ]]; then
  echo "shoal-effects-ssh image: journal init printed ${id}, not a 32-hex-digit identity" >&2
  exit 1
fi
code=0
run journal init -journal-dir /var/lib/effects-ssh >/dev/null 2>&1 || code=$?
check "a second journal init exits" "${code}" "1"
# The chart's init Job runs init with -print-existing: on an existing journal
# it prints the same identity and exits 0.
check "journal init -print-existing on the journal" "$(run journal init -print-existing -journal-dir /var/lib/effects-ssh)" "${id}"
listed="$(run journal list -journal-dir /var/lib/effects-ssh)"
if ! grep -qF "${id}" <<<"${listed}"; then
  echo "shoal-effects-ssh image: journal list does not show the identity ${id}: ${listed}" >&2
  exit 1
fi

echo "shoal-effects-ssh container smoke test passed (${IMAGE})"
