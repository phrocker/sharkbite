#!/usr/bin/env bash
set -euo pipefail

compose=(docker compose -f test/hdfs/docker-compose.yml)

for attempt in $(seq 1 60); do
  if report="$("${compose[@]}" exec -T namenode hdfs dfsadmin -report 2>/dev/null)" &&
    grep -q "Live datanodes (1)" <<<"$report"; then
    exit 0
  fi
  sleep 2
done

echo "HDFS did not report a live DataNode within 120 seconds" >&2
exit 1
