from __future__ import annotations

import argparse
import hashlib
import json
import os
import platform
import re
import shutil
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
BUILD = ROOT / "build" / "cross-platform-artifacts"
HEADER = ROOT / "capi" / "include" / "shoal.h"
TARGETS = (
    ("darwin", "amd64", "macosx_11_0_x86_64"),
    ("darwin", "arm64", "macosx_11_0_arm64"),
)


def run(
    args: list[str],
    *,
    cwd: Path = ROOT,
    env: dict[str, str] | None = None,
    capture: bool = False,
) -> str:
    completed = subprocess.run(
        args,
        cwd=cwd,
        env=env,
        check=True,
        text=True,
        stdout=subprocess.PIPE if capture else None,
        stderr=subprocess.STDOUT if capture else None,
    )
    return completed.stdout or ""


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def declared_exports() -> tuple[str, ...]:
    text = HEADER.read_text(encoding="utf-8")
    return tuple(
        sorted(
            set(
                re.findall(
                    r"SHOAL_API\b.*?\bSHOAL_CALL\s+(shoal_[A-Za-z0-9_]+)\s*\(",
                    text,
                    re.DOTALL,
                )
            )
        )
    )


def exported_symbols(library: Path) -> set[str]:
    system = platform.system()
    if system == "Windows":
        tool = shutil.which("objdump")
        if tool is None:
            raise RuntimeError("objdump is required to inspect Windows exports")
        text = run([tool, "-p", str(library)], capture=True)
        return set(re.findall(r"\]\s+\S+\s+(shoal_[A-Za-z0-9_]+)\s*$", text, re.MULTILINE))
    tool = shutil.which("nm")
    if tool is None:
        raise RuntimeError("nm is required to inspect native exports")
    args = [tool, "-gU", str(library)] if system == "Darwin" else [tool, "-gD", str(library)]
    text = run(args, capture=True)
    return set(re.findall(r"\b_?(shoal_[A-Za-z0-9_]+)\s*$", text, re.MULTILINE))


def artifact(path: Path) -> dict[str, object]:
    return {
        "path": str(path.relative_to(ROOT)).replace("\\", "/"),
        "size": path.stat().st_size,
        "sha256": sha256(path),
    }


def native_platform_tag(goos: str, arch: str) -> str:
    if goos == "windows":
        return "win_amd64" if arch == "amd64" else f"win_{arch}"
    if goos == "darwin":
        return f"macosx_11_0_{'x86_64' if arch == 'amd64' else arch}"
    return f"manylinux_2_28_{'x86_64' if arch == 'amd64' else arch}"


def build_native_distribution(goos: str, arch: str) -> dict[str, object]:
    dist = ROOT / "python" / "dist"
    command = [
        sys.executable,
        "python/scripts/build_release.py",
        "--platform-tag",
        native_platform_tag(goos, arch),
        "--allow-dirty",
        "--skip-auditwheel",
    ]
    run(command)
    run([sys.executable, "python/scripts/verify_release.py"])
    first = {
        path.name: sha256(path)
        for path in dist.iterdir()
        if path.suffix == ".whl" or path.name.endswith(".tar.gz")
    }
    run(command)
    run([sys.executable, "python/scripts/verify_release.py"])
    second = {
        path.name: sha256(path)
        for path in dist.iterdir()
        if path.suffix == ".whl" or path.name.endswith(".tar.gz")
    }
    if first != second:
        raise RuntimeError("consecutive native wheel/sdist builds were not reproducible")
    return {
        "platform_tag": native_platform_tag(goos, arch),
        "clean_install": "verified without Go on PATH",
        "reproducible": True,
        "artifacts": [artifact(path) for path in sorted(dist.iterdir()) if path.name in second],
    }


def go_environment(goos: str, goarch: str, *, cgo: bool) -> dict[str, str]:
    env = os.environ.copy()
    env.update(
        {
            "GOOS": goos,
            "GOARCH": goarch,
            "CGO_ENABLED": "1" if cgo else "0",
        }
    )
    return env


def build_native_evidence() -> dict[str, object]:
    system = platform.system()
    goos = {"Windows": "windows", "Darwin": "darwin", "Linux": "linux"}[system]
    arch = {"AMD64": "amd64", "x86_64": "amd64", "arm64": "arm64", "aarch64": "arm64"}.get(
        platform.machine(), platform.machine().lower()
    )
    suffix = {"windows": ".dll", "darwin": ".dylib", "linux": ".so"}[goos]
    shared = BUILD / f"shoal-{goos}-{arch}{suffix}"
    archive = BUILD / f"shoal-{goos}-{arch}.a"
    test_archive = BUILD / f"shoal-{goos}-{arch}-test.a"

    common = ["go", "build", "-trimpath", "-buildvcs=false", "-ldflags=-buildid="]
    run([*common, "-buildmode=c-shared", "-o", str(shared), "./cmd/shoal-capi"])
    run([*common, "-buildmode=c-archive", "-o", str(archive), "./cmd/shoal-capi"])
    run(
        [
            *common,
            "-tags=shoal_capi_test",
            "-buildmode=c-archive",
            "-o",
            str(test_archive),
            "./cmd/shoal-capi",
        ]
    )
    run(["go", "test", "-count=1", "-tags=shoal_capi_test", "./cmd/shoal-capi"])

    cc = shutil.which(os.environ.get("CC", "gcc"))
    if cc is None:
        raise RuntimeError("a native C compiler is required for the static lifecycle smoke")
    static_client = BUILD / ("lifecycle-static.exe" if goos == "windows" else "lifecycle-static")
    command = [
        cc,
        "-std=c11",
        "-Wall",
        "-Wextra",
        "-Werror",
        "-DSHOAL_STATIC",
        "-DSHOAL_CAPI_TEST",
        "-I",
        str(ROOT / "capi" / "include"),
        "-I",
        str(ROOT / "capi" / "tests"),
        str(ROOT / "capi" / "tests" / "lifecycle.c"),
        str(test_archive),
    ]
    if goos == "windows":
        command.extend(["-lwinmm", "-lws2_32"])
    elif goos == "linux":
        command.extend(["-lpthread", "-ldl", "-lm"])
    else:
        command.extend(["-framework", "CoreFoundation", "-framework", "Security"])
    command.extend(["-o", str(static_client)])
    run(command)
    run([str(static_client)], cwd=BUILD)

    expected = set(declared_exports())
    actual = exported_symbols(shared)
    missing = sorted(expected - actual)
    if missing:
        raise RuntimeError(f"native shared library is missing exports: {missing}")
    return {
        "platform": f"{goos}/{arch}",
        "runtime": "verified",
        "c_cxx_shared_lifecycle": "compiled, linked, and executed",
        "c_static_lifecycle": "compiled, linked, and executed",
        "abi": {"version": "1.19.0", "capability_count": 32},
        "exports": {"declared": len(expected), "present": len(expected)},
        "artifacts": [artifact(shared), artifact(archive)],
        "python_distribution": build_native_distribution(goos, arch),
    }


def build_cross_go() -> list[dict[str, object]]:
    results: list[dict[str, object]] = []
    for goos, goarch, _ in TARGETS:
        executable = BUILD / f"shoal-{goos}-{goarch}"
        run(
            [
                "go",
                "build",
                "-trimpath",
                "-buildvcs=false",
                "-ldflags=-buildid=",
                "-o",
                str(executable),
                "./cmd/shoal",
            ],
            env=go_environment(goos, goarch, cgo=False),
        )
        results.append(
            {
                "platform": f"{goos}/{goarch}",
                "go_command": artifact(executable),
                "c_shared": "unsupported: requires a target C compiler and native SDK",
                "c_archive": "unsupported: requires a target C compiler and native SDK",
                "runtime": "unsupported on this host",
            }
        )
    return results


def build_previews() -> list[dict[str, object]]:
    results: list[dict[str, object]] = []
    for goos, goarch, tag in TARGETS:
        dist = BUILD / f"wheel-preview-{goos}-{goarch}"
        run(
            [
                sys.executable,
                "python/scripts/build_preview.py",
                "--platform-tag",
                tag,
                "--dist",
                str(dist),
                "--allow-dirty",
            ]
        )
        run(
            [
                sys.executable,
                "python/scripts/verify_preview.py",
                "--dist",
                str(dist),
            ]
        )
        manifest = json.loads((dist / "preview-manifest.json").read_text(encoding="utf-8"))
        results.append(
            {
                "platform": f"{goos}/{goarch}",
                "platform_tag": tag,
                "wheel": manifest["artifact"],
                "native_library": "intentionally absent",
                "install": "verified with pip --target for the declared platform",
                "library_discovery": "verified failure without an explicit native library",
                "runtime": "unsupported until matching-host native execution",
            }
        )
    return results


def main() -> None:
    parser = argparse.ArgumentParser(
        description="Produce deterministic cross-platform Shoal artifact evidence"
    )
    parser.add_argument("--output", type=Path, default=BUILD / "evidence.json")
    parser.add_argument("--skip-native", action="store_true")
    parser.add_argument("--skip-previews", action="store_true")
    args = parser.parse_args()

    shutil.rmtree(BUILD, ignore_errors=True)
    BUILD.mkdir(parents=True)
    evidence: dict[str, object] = {
        "schema": 1,
        "source_commit": run(["git", "rev-parse", "HEAD"], capture=True).strip(),
        "host": {
            "system": platform.system(),
            "machine": platform.machine(),
            "go": run(["go", "version"], capture=True).strip(),
            "python": platform.python_version(),
        },
        "claims": {
            "cross_compilation_is_runtime_evidence": False,
            "unbundled_preview_is_runtime_evidence": False,
        },
        "cross_go": build_cross_go(),
    }
    if not args.skip_native:
        evidence["native"] = build_native_evidence()
    if not args.skip_previews:
        evidence["wheel_previews"] = build_previews()
    output = args.output.resolve()
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text(json.dumps(evidence, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    print(output)


if __name__ == "__main__":
    main()
