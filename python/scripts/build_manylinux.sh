#!/usr/bin/env bash
set -euo pipefail

readonly GO_VERSION="1.25.0"
readonly GO_ARCHIVE="go${GO_VERSION}.linux-amd64.tar.gz"
readonly GO_SHA256="2852af0cb20a13139b3448992e69b868e50ed0f8a1e5940ee1de9e19a123b613"
readonly PYTHON_BIN="/opt/python/cp311-cp311/bin/python"
readonly TOOLCHAIN_ROOT="/opt/shoal-release-toolchain"

if [[ "$(uname -m)" != "x86_64" ]]; then
  echo "manylinux release builds currently require x86_64" >&2
  exit 1
fi
if [[ ! -x "${PYTHON_BIN}" ]]; then
  echo "missing controlled CPython interpreter: ${PYTHON_BIN}" >&2
  exit 1
fi

rm -rf "${TOOLCHAIN_ROOT}"
mkdir -p "${TOOLCHAIN_ROOT}"
curl --fail --location --proto '=https' --tlsv1.2 --silent --show-error \
  "https://go.dev/dl/${GO_ARCHIVE}" \
  --output "${TOOLCHAIN_ROOT}/${GO_ARCHIVE}"
echo "${GO_SHA256}  ${TOOLCHAIN_ROOT}/${GO_ARCHIVE}" | sha256sum --check -
tar -C "${TOOLCHAIN_ROOT}" -xzf "${TOOLCHAIN_ROOT}/${GO_ARCHIVE}"
rm "${TOOLCHAIN_ROOT}/${GO_ARCHIVE}"

export PATH="${TOOLCHAIN_ROOT}/go/bin:${PYTHON_BIN%/*}:/usr/local/bin:/usr/bin:/bin"
export GOWORK=off
export PYTHONHASHSEED=0
export PIP_DISABLE_PIP_VERSION_CHECK=1
export PIP_NO_INPUT=1

git config --global --add safe.directory "$(pwd)"
"${PYTHON_BIN}" -m pip install \
  --only-binary=:all: \
  --require-hashes \
  --requirement python/requirements-release.txt

go version
"${PYTHON_BIN}" --version
auditwheel --version
"${PYTHON_BIN}" python/scripts/build_release.py \
  --platform-tag manylinux_2_28_x86_64
"${PYTHON_BIN}" python/scripts/verify_release.py
