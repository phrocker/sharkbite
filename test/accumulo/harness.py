#!/usr/bin/env python3
"""Deterministic local Accumulo 4 conformance harness lifecycle."""

from __future__ import annotations

import argparse
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import time
import xml.etree.ElementTree as ET

ROOT = Path(__file__).resolve().parents[2]
HARNESS = ROOT / "test" / "accumulo"
COMPOSE_FILE = HARNESS / "docker-compose.yml"
PROJECT = "shoal-accumulo4-test"
SHOAL_TSERVER = "shoal-tserver"
SHOAL_COMPACTOR = "shoal-compactor"
NATIVE_TSERVER = "tserver"
LIVE_DIR = HARNESS / ".live"
SYSTEM_TOKEN_FILE = LIVE_DIR / "system-token"
ACCUMULO_REVISION = "1a716b2c1bb5762ead4b46d2bc4f53e13873b314"
ACCUMULO_VERSION = "4.0.0-SNAPSHOT"
ACCUMULO_IMAGE = (
    "ghcr.io/phrocker/shoal-accumulo-test@"
    "sha256:bf9cbca5aaa8ada88d503e6cf178d41e72a6e0067cef47fe582f4b93c3d392db"
)
ACCUMULO_SOURCE_SHA512 = (
    "bf2cac6f7b5f759358ac306bba2342dc74c51bf63d70905da1004fc1eb0cfa66"
    "f6a73023ab250824a67f17d410e9b3351f097ddfd742bbc4b14f7a7feb6c4b2e"
)
ACCUMULO_ACCESS_REVISION = "5d7a94492832121f507029d9d7e7627fd88e95ba"
ACCUMULO_ACCESS_SOURCE_SHA512 = (
    "f519e992ff8f418f06a564c04313b93788460de6de7fee1dfd7503ad660149fd"
    "b3794dae61474369be1c671115f2ecda4d5b73d745cbd73972ed3d171875f243"
)
HADOOP_VERSION = "3.4.2"
HADOOP_IMAGE_DIGEST = (
    "sha256:0f79c18d27a2b15984868a41e245b09c98daa5638b573b4dbd848be650f05635"
)
ZOOKEEPER_VERSION = "3.9.5"
ZOOKEEPER_IMAGE_DIGEST = (
    "sha256:f5ce2c0274fd99c77bcb51505c748ac109fd5be36b4247b46c178ce59ea59e59"
)


class DockerUnavailable(RuntimeError):
    pass


def compose_command(*args: str) -> list[str]:
    return [
        "docker",
        "compose",
        "--project-name",
        PROJECT,
        "--file",
        str(COMPOSE_FILE),
        *args,
    ]


def client_command(mode: str) -> list[str]:
    return compose_command(
        "exec", "-T", "manager", "bash", "/opt/shoal-smoke/run.sh", mode
    )


def role_command(role: str) -> list[str]:
    return [sys.executable, "test/accumulo/harness.py", "role", role]


def compose_services(*services: str) -> list[str]:
    return compose_command(
        "--profile",
        "shoal",
        "up",
        "--detach",
        "--build",
        "--no-deps",
        *services,
    )


def _properties(path: Path) -> dict[str, str]:
    result: dict[str, str] = {}
    for raw in path.read_text(encoding="utf-8").splitlines():
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        key, separator, value = line.partition("=")
        if not separator or not key or not value:
            raise ValueError(f"{path}: invalid property line {raw!r}")
        if key in result:
            raise ValueError(f"{path}: duplicate property {key}")
        result[key] = value
    return result


def validate(root: Path = ROOT) -> list[str]:
    harness = root / "test" / "accumulo"
    errors: list[str] = []

    compose = (harness / "docker-compose.yml").read_text(encoding="utf-8")
    dockerfile = (harness / "image" / "Dockerfile").read_text(encoding="utf-8")
    shoal_dockerfile = (harness / "shoal.Dockerfile").read_text(encoding="utf-8")
    required_text = {
        "compose Accumulo image": (
            compose,
            ACCUMULO_IMAGE,
        ),
        "compose Hadoop image": (compose, f"apache/hadoop:{HADOOP_VERSION}"),
        "compose ZooKeeper image": (compose, f"zookeeper:{ZOOKEEPER_VERSION}"),
        "Dockerfile Accumulo revision": (
            dockerfile,
            f"ARG ACCUMULO_REVISION={ACCUMULO_REVISION}",
        ),
        "Dockerfile Accumulo checksum": (
            dockerfile,
            f"ARG ACCUMULO_SOURCE_SHA512={ACCUMULO_SOURCE_SHA512}",
        ),
        "Dockerfile Accumulo Access revision": (
            dockerfile,
            f"ARG ACCUMULO_ACCESS_REVISION={ACCUMULO_ACCESS_REVISION}",
        ),
        "Dockerfile Accumulo Access checksum": (
            dockerfile,
            f"ARG ACCUMULO_ACCESS_SOURCE_SHA512={ACCUMULO_ACCESS_SOURCE_SHA512}",
        ),
        "Dockerfile Hadoop image": (
            dockerfile,
            f"FROM apache/hadoop@{HADOOP_IMAGE_DIGEST} AS hadoop",
        ),
        "Dockerfile ZooKeeper image": (
            dockerfile,
            f"FROM zookeeper@{ZOOKEEPER_IMAGE_DIGEST} AS zookeeper",
        ),
        "Java smoke mount": (compose, "./smoke:/opt/shoal-smoke:ro"),
        "Shoal Go toolchain": (shoal_dockerfile, "FROM golang:1.25-bookworm AS build"),
        "Shoal image build": (
            compose,
            "dockerfile: test/accumulo/shoal.Dockerfile",
        ),
        "Shoal tserver service": (compose, f"{SHOAL_TSERVER}:"),
        "Shoal compactor service": (compose, f"{SHOAL_COMPACTOR}:"),
        "Shoal tserver exact target": (
            compose,
            "-accumulo-version 4.0.0-SNAPSHOT",
        ),
        "Shoal tserver ingest": (compose, "-enable-ingest"),
        "Shoal compactor group": (compose, "-group shoal_default"),
    }
    for description, (text, expected) in required_text.items():
        if expected not in text:
            errors.append(f"{description} is not pinned to {expected!r}")

    if "2.1.6" in compose or "2.1.6" in dockerfile:
        errors.append("legacy Accumulo 2.1.6 reference remains")
    if "SHOAL_ACCUMULO_IMAGE" in compose:
        errors.append("exact-target Accumulo image must not be environment-overridable")

    try:
        client = _properties(harness / "conf" / "accumulo-client.properties")
        expected_client = {
            "instance.name": "shoal",
            "instance.zookeepers": "zookeeper:2181",
            "auth.type": "password",
            "auth.principal": "root",
            "auth.token": "secret",
        }
        if client != expected_client:
            errors.append("accumulo-client.properties differs from the harness contract")
    except (OSError, ValueError) as exc:
        errors.append(str(exc))

    try:
        accumulo = _properties(harness / "conf" / "accumulo.properties")
        expected_accumulo = {
            "instance.volumes": "hdfs://namenode:8020/accumulo",
            "instance.zookeeper.host": "zookeeper:2181",
            "instance.secret": "shoal-test-instance-secret",
            "tserver.memory.maps.native.enabled": "false",
            "compaction.service.shoal.planner": (
                "org.apache.accumulo.core.spi.compaction.RatioBasedCompactionPlanner"
            ),
            "compaction.service.shoal.planner.opts.groups": (
                '[{"group":"shoal_default"}]'
            ),
        }
        if accumulo != expected_accumulo:
            errors.append("accumulo.properties differs from the exact-target contract")
    except (OSError, ValueError) as exc:
        errors.append(str(exc))

    for name in ("core-site.xml", "hdfs-site.xml", "hdfs-client-site.xml"):
        try:
            ET.parse(harness / "conf" / name)
        except (OSError, ET.ParseError) as exc:
            errors.append(f"{name}: {exc}")

    return errors


def require_docker() -> None:
    if shutil.which("docker") is None:
        raise DockerUnavailable("Docker CLI is not installed or not on PATH")
    result = subprocess.run(
        ["docker", "version", "--format", "{{.Server.Version}}"],
        text=True,
        capture_output=True,
        check=False,
    )
    if result.returncode != 0 or not result.stdout.strip():
        detail = result.stderr.strip() or "Docker daemon did not report a version"
        raise DockerUnavailable(detail)
    compose = subprocess.run(
        ["docker", "compose", "version"],
        text=True,
        capture_output=True,
        check=False,
    )
    if compose.returncode != 0:
        raise DockerUnavailable(
            compose.stderr.strip() or "Docker Compose v2 is unavailable"
        )


def run(command: list[str], *, check: bool = True) -> subprocess.CompletedProcess[str]:
    print("+", subprocess.list2cmdline(command), flush=True)
    return subprocess.run(command, text=True, check=check)


def run_compactor_smoke() -> None:
    timeout = int(os.environ.get("SHOAL_ACCUMULO_COMPACTOR_TIMEOUT", "600"))
    command = client_command("compactor")
    print("+", subprocess.list2cmdline(command), flush=True)
    try:
        subprocess.run(command, text=True, check=True, timeout=timeout)
    except subprocess.TimeoutExpired as exc:
        raise RuntimeError(
            f"compactor smoke did not complete within {timeout} seconds"
        ) from exc


def wait_ready() -> None:
    timeout = int(os.environ.get("SHOAL_ACCUMULO_READY_TIMEOUT", "300"))
    deadline = time.monotonic() + timeout
    attempt = 0
    while time.monotonic() < deadline:
        attempt += 1
        result = subprocess.run(
            client_command("ready"),
            text=True,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            check=False,
        )
        if result.returncode == 0:
            print(f"Accumulo {ACCUMULO_VERSION} ready after {attempt} probe(s)")
            return
        time.sleep(2)
    raise RuntimeError(f"Accumulo did not become ready within {timeout} seconds")


def down() -> None:
    run(
        compose_command(
            "--profile", "shoal", "down", "--volumes", "--remove-orphans"
        ),
        check=False,
    )
    shutil.rmtree(LIVE_DIR, ignore_errors=True)


def start_and_wait() -> None:
    down()
    try:
        run(compose_command("up", "--detach", "--build"))
        wait_ready()
    except BaseException:
        run(compose_command("logs", "--no-color"), check=False)
        down()
        raise


def full_run() -> None:
    require_docker()
    down()
    start_attempted = False
    succeeded = False
    previous_handlers: dict[int, object] = {}

    def terminate(signum: int, _frame: object) -> None:
        raise SystemExit(128 + signum)

    for signum in (signal.SIGINT, signal.SIGTERM):
        previous_handlers[signum] = signal.signal(signum, terminate)
    try:
        start_attempted = True
        run(compose_command("up", "--detach", "--build"))
        wait_ready()
        run(client_command("smoke"))
        succeeded = True
        print(f"Accumulo {ACCUMULO_VERSION} Java create/write/flush/scan smoke passed")
    finally:
        if start_attempted and not succeeded:
            run(compose_command("logs", "--no-color"), check=False)
        down()
        for signum, handler in previous_handlers.items():
            signal.signal(signum, handler)


def _capture(command: list[str]) -> str:
    print("+", subprocess.list2cmdline(command), flush=True)
    result = subprocess.run(command, text=True, capture_output=True, check=False)
    if result.returncode != 0:
        detail = (result.stdout + result.stderr).strip()
        raise subprocess.CalledProcessError(result.returncode, command, output=detail)
    if result.stderr:
        print(result.stderr, end="", file=sys.stderr)
    return result.stdout.strip()


def _wait_mode(mode: str, timeout: int = 180) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        result = subprocess.run(
            client_command(mode),
            text=True,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            check=False,
        )
        if result.returncode == 0:
            return
        time.sleep(2)
    raise RuntimeError(f"{mode} did not succeed within {timeout} seconds")


def _start_role_base() -> None:
    down()
    LIVE_DIR.mkdir(parents=True, exist_ok=True)
    run(
        compose_command(
            "up",
            "--detach",
            "--build",
            "zookeeper",
            "namenode",
            "datanode",
            "hdfs-init",
            "accumulo-init",
            "manager",
            "garbage-collector",
            "monitor",
        )
    )
    _wait_mode("ready-manager")
    token = _capture(client_command("system-token")).splitlines()[-1]
    if not token:
        raise RuntimeError("Accumulo system token helper returned no token")
    SYSTEM_TOKEN_FILE.write_text(token + "\n", encoding="ascii")


def _run_tserver_role() -> None:
    _start_role_base()
    run(compose_services(SHOAL_TSERVER))
    _wait_mode("shoal-ready")
    run(client_command("tserver"))
    run(client_command("recovery-prepare"))
    run(compose_command("kill", "--signal", "KILL", SHOAL_TSERVER))
    _wait_mode("shoal-absent")
    run(
        compose_command(
            "--profile",
            "shoal",
            "up",
            "--detach",
            "--no-deps",
            SHOAL_TSERVER,
        )
    )
    _wait_mode("shoal-ready")
    run(client_command("recovery-verify"))
    print(
        "SHOAL_EVIDENCE tserver=pass service-lock=verified assignment=verified "
        "java-ingest=verified continuation=verified minc=verified wal-recovery=verified "
        "restart-fencing=verified"
    )


def _run_compactor_role() -> None:
    _start_role_base()
    # Keep this gate focused on the Shoal compactor. A native tablet server
    # supplies Accumulo's independent metadata-driven job-planning surface;
    # the Shoal tablet-server path has its own live role gate.
    run(compose_services(NATIVE_TSERVER, SHOAL_COMPACTOR))
    _wait_mode("ready")
    run_compactor_smoke()
    run(compose_command("restart", SHOAL_COMPACTOR))
    run_compactor_smoke()
    print(
        "SHOAL_EVIDENCE compactor=pass publication=verified completion=verified "
        "java-readable=verified promotion-equivalence=verified "
        "cancellation-restart-fencing=verified"
    )


def role_run(role: str) -> None:
    require_docker()
    succeeded = False
    try:
        if role in ("tserver", "scanserver", "client"):
            _run_tserver_role()
        elif role in ("compactor", "promotion"):
            _run_compactor_role()
        else:
            raise ValueError(f"unknown role {role!r}")
        print(f"SHOAL_EVIDENCE role={role} status=pass")
        succeeded = True
    finally:
        if not succeeded:
            run(
                compose_command("--profile", "shoal", "logs", "--no-color"),
                check=False,
            )
        down()


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "action",
        choices=("validate", "up", "wait", "smoke", "down", "test", "role"),
    )
    parser.add_argument(
        "role",
        nargs="?",
        choices=("tserver", "scanserver", "compactor", "promotion", "client"),
    )
    args = parser.parse_args()

    errors = validate()
    if errors:
        for error in errors:
            print(f"ERROR: {error}", file=sys.stderr)
        return 1
    if args.action == "validate":
        print(
            f"Accumulo {ACCUMULO_VERSION} harness configuration is valid "
            f"(revision {ACCUMULO_REVISION})"
        )
        return 0

    try:
        require_docker()
        if args.action == "up":
            start_and_wait()
        elif args.action == "wait":
            wait_ready()
        elif args.action == "smoke":
            run(client_command("smoke"))
        elif args.action == "down":
            down()
        elif args.action == "role":
            if args.role is None:
                parser.error("role action requires a role")
            role_run(args.role)
        else:
            full_run()
    except DockerUnavailable as exc:
        print(
            f"SKIP (needs-docker): {exc}; live Accumulo 4 test was not run",
            file=sys.stderr,
        )
        return 2
    except (RuntimeError, subprocess.CalledProcessError) as exc:
        print(f"ERROR: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
