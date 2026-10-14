from __future__ import annotations

import argparse
import hashlib
import json
import os
import shutil
import subprocess
import sys
import zipfile
from pathlib import Path

PYTHON_DIR = Path(__file__).resolve().parents[1]
DEFAULT_DIST = PYTHON_DIR / "preview-dist"


def sha256(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def main() -> None:
    parser = argparse.ArgumentParser(
        description="Verify and install an unbundled platform wheel preview"
    )
    parser.add_argument("--dist", type=Path, default=DEFAULT_DIST)
    args = parser.parse_args()
    dist = args.dist.resolve()

    manifest = json.loads(
        (dist / "preview-manifest.json").read_text(encoding="utf-8")
    )
    wheel = dist / manifest["artifact"]["filename"]
    if (
        not wheel.is_file()
        or wheel.stat().st_size != manifest["artifact"]["size"]
        or sha256(wheel) != manifest["artifact"]["sha256"]
    ):
        raise RuntimeError("preview wheel does not match its manifest")
    if not wheel.name.endswith(f"-{manifest['platform_tag']}.whl"):
        raise RuntimeError("preview wheel tag does not match its manifest")

    with zipfile.ZipFile(wheel) as archive:
        names = set(archive.namelist())
        if any(name.lower().endswith((".dll", ".dylib", ".so")) for name in names):
            raise RuntimeError("preview wheel must not contain a native library")
        preview_name = next(
            (
                name
                for name in names
                if name.endswith("sharkbite/.libs/_shoal_preview.json")
            ),
            None,
        )
        if preview_name is None:
            raise RuntimeError("preview wheel is missing its unsupported-runtime marker")
        embedded = json.loads(archive.read(preview_name))
        if embedded["platform_tag"] != manifest["platform_tag"]:
            raise RuntimeError("embedded preview marker has the wrong platform")
        wheel_metadata = archive.read(
            next(name for name in names if name.endswith(".dist-info/WHEEL"))
        ).decode()
        if "Root-Is-Purelib: false" not in wheel_metadata:
            raise RuntimeError("platform preview must install as a platform wheel")

    install_root = PYTHON_DIR / "build" / "preview-install"
    shutil.rmtree(install_root, ignore_errors=True)
    install_root.mkdir(parents=True)
    env = os.environ.copy()
    env.pop("PYTHONPATH", None)
    env.pop("SHOAL_LIBRARY", None)
    env.pop("SHOAL_ALLOW_SYSTEM_LIBRARY", None)
    try:
        subprocess.run(
            [
                sys.executable,
                "-m",
                "pip",
                "install",
                "--no-index",
                "--no-deps",
                "--target",
                str(install_root),
                "--platform",
                manifest["platform_tag"],
                "--implementation",
                "py",
                "--abi",
                "none",
                str(wheel),
            ],
            cwd=wheel.parent,
            env=env,
            check=True,
        )
        code = f"""
import os
import sys
sys.path.insert(0, {str(install_root)!r})
import pysharkbite
import sharkbite
from sharkbite import NativeAPI
assert pysharkbite.Client is sharkbite.Client
try:
    NativeAPI()
except ImportError as error:
    assert "unable to load the Shoal shared library" in str(error)
else:
    raise AssertionError("unbundled preview unexpectedly loaded a native library")
"""
        subprocess.run([sys.executable, "-I", "-c", code], env=env, check=True)
    finally:
        shutil.rmtree(install_root, ignore_errors=True)
    print("unbundled platform preview verified")


if __name__ == "__main__":
    main()
