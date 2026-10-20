#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
image="$(python3 - "${root}/test/accumulo/harness.py" <<'PY'
import runpy
import sys

print(runpy.run_path(sys.argv[1])["ACCUMULO_IMAGE"])
PY
)"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
wal="${work}/wal/localhost+9997/00000000-0000-0000-0000-000000000172"
mkdir -p "$(dirname "${wal}")"
printf 'root:x:0:0:root:/root:/bin/bash\nproof:x:%s:%s:proof:/tmp:/bin/bash\n' \
  "$(id -u)" "$(id -g)" > "${work}/passwd"

cd "${root}"
GOWORK=off go run ./test/accumulo/native-wal-sorter/generate.go "${wal}"
docker run --rm --user "$(id -u):$(id -g)" --entrypoint /bin/bash \
  --mount "type=bind,src=${root}/test/accumulo/native-wal-sorter,dst=/proof,readonly" \
  --mount "type=bind,src=${work},dst=/input" \
  --mount "type=bind,src=${work}/passwd,dst=/etc/passwd,readonly" \
  "${image}" -euo pipefail -c '
    classes="$(mktemp -d)"
    trap '\''rm -rf "${classes}"'\'' EXIT
    classpath="$(accumulo classpath)"
    javac -proc:none -cp "${classpath}" -d "${classes}" /proof/NativeRecoveryProbe.java
    java -cp "${classes}:${classpath}" NativeRecoveryProbe \
      /input/wal/localhost+9997/00000000-0000-0000-0000-000000000172 /input
  '
