from __future__ import annotations

import argparse
import hashlib
import json
import os
import shutil
import subprocess
import sys
from pathlib import Path

PYTHON_DIR = Path(__file__).resolve().parents[1]
ROOT = PYTHON_DIR.parent
LIBS_DIR = PYTHON_DIR / "src" / "sharkbite" / ".libs"
BUILD_DIR = PYTHON_DIR / "build"
EGG_INFO_DIR = PYTHON_DIR / "src" / "shoal_sharkbite.egg-info"
DEFAULT_DIST = PYTHON_DIR / "preview-dist"


def output(*args: str) -> str:
    return subprocess.check_output(args, cwd=ROOT, text=True).strip()


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def clean_build_state() -> None:
    shutil.rmtree(BUILD_DIR, ignore_errors=True)
    shutil.rmtree(EGG_INFO_DIR, ignore_errors=True)


def build_once(dist: Path, env: dict[str, str]) -> Path:
    clean_build_state()
    subprocess.run(
        [
            sys.executable,
            "-m",
            "build",
            "--no-isolation",
            "--wheel",
            "--outdir",
            str(dist),
        ],
        cwd=PYTHON_DIR,
        env=env,
        check=True,
    )
    wheels = sorted(dist.glob("*.whl"))
    if len(wheels) != 1:
        raise RuntimeError("expected exactly one preview wheel")
    return wheels[0]


def main() -> None:
    parser = argparse.ArgumentParser(
        description="Build an installable, unbundled platform-tagged wheel preview"
    )
    parser.add_argument("--platform-tag", required=True)
    parser.add_argument("--dist", type=Path, default=DEFAULT_DIST)
    parser.add_argument("--allow-dirty", action="store_true")
    args = parser.parse_args()
    dist = args.dist.resolve()

    dirty = bool(output("git", "status", "--porcelain", "--untracked-files=no"))
    if dirty and not args.allow_dirty:
        raise RuntimeError("preview builds require a clean tracked worktree")

    source_commit = output("git", "rev-parse", "HEAD")
    source_epoch = int(
        os.environ.get(
            "SOURCE_DATE_EPOCH", output("git", "show", "-s", "--format=%ct", "HEAD")
        )
    )
    env = os.environ.copy()
    env.update(
        {
            "PYTHONHASHSEED": "0",
            "SOURCE_DATE_EPOCH": str(source_epoch),
            "SHOAL_WHEEL_PLATFORM_TAG": args.platform_tag,
            "SHOAL_WHEEL_PREVIEW": "1",
        }
    )

    shutil.rmtree(dist, ignore_errors=True)
    shutil.rmtree(LIBS_DIR, ignore_errors=True)
    dist.mkdir(parents=True)
    LIBS_DIR.mkdir(parents=True)
    preview = {
        "schema": 1,
        "platform_tag": args.platform_tag,
        "native_library": "not bundled",
        "runtime_status": "unsupported until verified on a matching native host",
    }
    (LIBS_DIR / "_shoal_preview.json").write_text(
        json.dumps(preview, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    try:
        first = build_once(dist, env)
        first_digest = sha256(first)
        first.unlink()
        second = build_once(dist, env)
        second_digest = sha256(second)
        if second_digest != first_digest:
            raise RuntimeError("consecutive preview wheel builds were not reproducible")
        manifest = {
            **preview,
            "source": {
                "commit": source_commit,
                "dirty": dirty,
                "source_date_epoch": source_epoch,
            },
            "artifact": {
                "filename": second.name,
                "sha256": second_digest,
                "size": second.stat().st_size,
            },
        }
        (dist / "preview-manifest.json").write_text(
            json.dumps(manifest, indent=2, sort_keys=True) + "\n", encoding="utf-8"
        )
        print(f"built unbundled preview {second.name}")
    finally:
        shutil.rmtree(LIBS_DIR, ignore_errors=True)
        clean_build_state()


if __name__ == "__main__":
    main()
