from __future__ import annotations

import argparse
import gzip
import hashlib
import io
import json
import os
import platform
import re
import shutil
import subprocess
import sys
import tarfile
import tomllib
from pathlib import Path

PYTHON_DIR = Path(__file__).resolve().parents[1]
ROOT = PYTHON_DIR.parent
PACKAGE_DIR = PYTHON_DIR / "src" / "sharkbite"
LIBS_DIR = PACKAGE_DIR / ".libs"
DIST_DIR = PYTHON_DIR / "dist"
BUILD_DIR = PYTHON_DIR / "build"
EGG_INFO_DIR = PYTHON_DIR / "src" / "shoal_sharkbite.egg-info"
ABI_HEADER = ROOT / "capi" / "include" / "shoal_types.h"


def run(*args: str, env: dict[str, str] | None = None) -> None:
    subprocess.run(args, cwd=PYTHON_DIR, env=env, check=True)


def output(*args: str) -> str:
    return subprocess.check_output(args, cwd=ROOT, text=True).strip()


def package_version() -> str:
    with (PYTHON_DIR / "pyproject.toml").open("rb") as handle:
        version = tomllib.load(handle)["project"]["version"]
    version_source = PACKAGE_DIR / "_version.py"
    match = re.search(
        r'^__version__ = "([^"]+)"$',
        version_source.read_text(encoding="utf-8"),
        re.MULTILINE,
    )
    if not match or match.group(1) != version:
        raise RuntimeError("pyproject.toml and sharkbite._version disagree")
    return version


def abi_version() -> tuple[int, int, int]:
    text = ABI_HEADER.read_text(encoding="utf-8")

    def value(name: str) -> int:
        match = re.search(rf"^#define {name} (\d+)u$", text, re.MULTILINE)
        if not match:
            raise RuntimeError(f"cannot read {name} from {ABI_HEADER}")
        return int(match.group(1))

    return (
        value("SHOAL_ABI_VERSION_MAJOR"),
        value("SHOAL_ABI_VERSION_MINOR"),
        value("SHOAL_ABI_VERSION_PATCH"),
    )


def native_filename() -> str:
    if sys.platform == "win32":
        return "shoal.dll"
    if sys.platform == "darwin":
        return "libshoal.dylib"
    return "libshoal.so"


def default_platform_tag() -> str:
    machine = platform.machine().lower()
    arches = {"amd64": "x86_64", "x86_64": "x86_64", "arm64": "arm64", "aarch64": "aarch64"}
    arch = arches.get(machine, machine)
    if sys.platform == "win32":
        return "win_amd64" if arch == "x86_64" else f"win_{arch}"
    if sys.platform == "darwin":
        return f"macosx_11_0_{'arm64' if arch == 'arm64' else 'x86_64'}"
    return f"manylinux_2_28_{arch}"


def enforce_platform(platform_tag: str, skip_auditwheel: bool) -> None:
    if not platform_tag.startswith("manylinux_2_28_"):
        return
    if sys.platform != "linux":
        raise RuntimeError("manylinux wheels must be built on Linux")
    libc, version = platform.libc_ver()
    if libc != "glibc" or tuple(map(int, version.split(".")[:2])) > (2, 28):
        raise RuntimeError(
            f"{platform_tag} requires a glibc 2.28-or-older build environment; "
            f"found {libc} {version}"
        )
    if not skip_auditwheel and shutil.which("auditwheel") is None:
        raise RuntimeError("auditwheel is required for a release manylinux build")


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def normalize_sdist(path: Path, epoch: int) -> None:
    entries: list[tuple[tarfile.TarInfo, bytes | None]] = []
    with tarfile.open(path, "r:gz") as archive:
        for member in archive.getmembers():
            source = archive.extractfile(member) if member.isfile() else None
            entries.append((member, source.read() if source else None))
    tar_buffer = io.BytesIO()
    with tarfile.open(fileobj=tar_buffer, mode="w", format=tarfile.PAX_FORMAT) as archive:
        for member, data in sorted(entries, key=lambda item: item[0].name):
            member.uid = 0
            member.gid = 0
            member.uname = ""
            member.gname = ""
            member.mtime = epoch
            member.pax_headers = {}
            archive.addfile(member, io.BytesIO(data) if data is not None else None)
    with path.open("wb") as output_file:
        with gzip.GzipFile(
            filename="", mode="wb", fileobj=output_file, mtime=epoch, compresslevel=9
        ) as compressed:
            compressed.write(tar_buffer.getvalue())


def main() -> None:
    parser = argparse.ArgumentParser(description="Build Shoal Python release artifacts")
    parser.add_argument("--platform-tag", default=default_platform_tag())
    parser.add_argument("--allow-dirty", action="store_true")
    parser.add_argument("--skip-auditwheel", action="store_true")
    args = parser.parse_args()

    dirty = bool(output("git", "status", "--porcelain", "--untracked-files=no"))
    if dirty and not args.allow_dirty:
        raise RuntimeError("release builds require a clean tracked worktree")
    enforce_platform(args.platform_tag, args.skip_auditwheel)

    source_commit = output("git", "rev-parse", "HEAD")
    source_epoch = os.environ.get(
        "SOURCE_DATE_EPOCH", output("git", "show", "-s", "--format=%ct", "HEAD")
    )
    env = os.environ.copy()
    env.update(
        {
            "PYTHONHASHSEED": "0",
            "SOURCE_DATE_EPOCH": source_epoch,
            "SHOAL_WHEEL_PLATFORM_TAG": args.platform_tag,
        }
    )

    shutil.rmtree(DIST_DIR, ignore_errors=True)
    shutil.rmtree(BUILD_DIR, ignore_errors=True)
    shutil.rmtree(LIBS_DIR, ignore_errors=True)
    shutil.rmtree(EGG_INFO_DIR, ignore_errors=True)
    DIST_DIR.mkdir(parents=True)

    run(
        sys.executable,
        "-m",
        "build",
        "--no-isolation",
        "--sdist",
        "--outdir",
        str(DIST_DIR),
        env=env,
    )
    built_sdists = sorted(DIST_DIR.glob("*.tar.gz"))
    if len(built_sdists) != 1:
        raise RuntimeError("expected exactly one source distribution")
    normalize_sdist(built_sdists[0], int(source_epoch))

    LIBS_DIR.mkdir(parents=True)
    native_path = LIBS_DIR / native_filename()
    subprocess.run(
        [
            "go",
            "build",
            "-trimpath",
            "-buildvcs=false",
            "-ldflags=-buildid=",
            "-buildmode=c-shared",
            "-o",
            str(native_path),
            "./cmd/shoal-capi",
        ],
        cwd=ROOT,
        env=env,
        check=True,
    )
    generated_header = native_path.with_suffix(".h")
    generated_header.unlink(missing_ok=True)
    bundle = {
        "schema": 1,
        "library": {
            "filename": native_path.name,
            "sha256": sha256(native_path),
            "abi": ".".join(map(str, abi_version())),
        },
    }
    (LIBS_DIR / "_shoal_bundle.json").write_text(
        json.dumps(bundle, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )

    run(
        sys.executable,
        "-m",
        "build",
        "--no-isolation",
        "--wheel",
        "--outdir",
        str(DIST_DIR),
        env=env,
    )

    wheels = sorted(DIST_DIR.glob("*.whl"))
    sdists = sorted(DIST_DIR.glob("*.tar.gz"))
    if len(wheels) != 1 or len(sdists) != 1:
        raise RuntimeError("expected exactly one wheel and one sdist")
    if not args.skip_auditwheel and args.platform_tag.startswith("manylinux_2_28_"):
        run("auditwheel", "show", str(wheels[0]), env=env)

    artifacts = []
    for path in (sdists[0], wheels[0]):
        artifacts.append(
            {
                "filename": path.name,
                "sha256": sha256(path),
                "size": path.stat().st_size,
            }
        )
    manifest = {
        "schema": 1,
        "package": {"name": "shoal-sharkbite", "version": package_version()},
        "shoal_abi": ".".join(map(str, abi_version())),
        "platform_tag": args.platform_tag,
        "source": {
            "commit": source_commit,
            "dirty": dirty,
            "source_date_epoch": int(source_epoch),
        },
        "artifacts": artifacts,
        "signing": {
            "format": "sha256",
            "detached_signatures_expected": True,
        },
    }
    (DIST_DIR / "release-manifest.json").write_text(
        json.dumps(manifest, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    (DIST_DIR / "SHA256SUMS").write_text(
        "".join(f"{item['sha256']}  {item['filename']}\n" for item in artifacts),
        encoding="ascii",
    )
    print(f"built {wheels[0].name} and {sdists[0].name}")


if __name__ == "__main__":
    main()
