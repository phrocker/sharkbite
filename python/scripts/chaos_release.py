"""Fail-closed chaos checks for a built Sharkbite wheel.

The checks deliberately mutate only a disposable virtual environment. They are
intended to run inside a clean release container before publication.
"""
from __future__ import annotations

import argparse
import os
import shutil
import subprocess
import sys
import tempfile
import venv
from pathlib import Path


def run(*args: str, env: dict[str, str] | None = None) -> subprocess.CompletedProcess[str]:
    return subprocess.run(args, text=True, capture_output=True, env=env)


def expect_failure(label: str, *code: str, env: dict[str, str]) -> None:
    result = run(env["PYTHON"], "-c", *code, env=env)
    output = result.stdout + result.stderr
    if result.returncode == 0:
        raise SystemExit(f"chaos case {label} unexpectedly succeeded")
    print(f"PASS {label}: {output.strip().splitlines()[-1] if output.strip() else 'failed closed'}")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--wheel", type=Path, required=True)
    args = parser.parse_args()
    wheel = args.wheel.resolve()
    if not wheel.is_file() or wheel.suffix != ".whl":
        raise SystemExit(f"wheel not found: {wheel}")

    with tempfile.TemporaryDirectory(prefix="sharkbite-chaos-") as root_text:
        root = Path(root_text)
        venv_dir = root / "venv"
        venv.EnvBuilder(with_pip=True, clear=True).create(venv_dir)
        python = venv_dir / "bin" / "python"
        base = os.environ.copy()
        base.update({"PYTHON": str(python), "PYTHONPATH": ""})
        installed = run(str(python), "-m", "pip", "install", "--no-index", "--no-deps", str(wheel), env=base)
        if installed.returncode:
            print(installed.stdout, end="")
            print(installed.stderr, end="", file=sys.stderr)
            return installed.returncode

        smoke = run(
            str(python),
            "-c",
            "import sharkbite, pysharkbite; assert pysharkbite.__version__ == sharkbite.__version__",
            env=base,
        )
        if smoke.returncode:
            print(smoke.stdout, end="")
            print(smoke.stderr, end="", file=sys.stderr)
            return smoke.returncode
        print("PASS clean install/import")

        package = Path(
            run(str(python), "-c", "import pathlib, sharkbite; print(pathlib.Path(sharkbite.__file__).parent)", env=base).stdout.strip()
        )
        libs = package / ".libs"
        manifest = libs / "_shoal_bundle.json"
        native = next((p for p in libs.iterdir() if p.suffix.lower() in {".so", ".dylib", ".dll"}), None)
        if native is None or not manifest.is_file():
            raise SystemExit("wheel has no bundled native library and manifest")

        relative_env = dict(base, SHOAL_LIBRARY="relative-shoal.so")
        expect_failure(
            "relative library override",
            "from sharkbite._native import library_candidates; list(library_candidates())",
            env=relative_env,
        )

        original_manifest = manifest.read_bytes()
        manifest.write_text("{}", encoding="utf-8")
        try:
            expect_failure("corrupt checksum manifest", "from sharkbite import NativeAPI; NativeAPI()", env=base)
        finally:
            manifest.write_bytes(original_manifest)

        original_native = native.read_bytes()
        native.write_bytes(original_native + b"chaos")
        try:
            expect_failure("tampered native library", "from sharkbite import NativeAPI; NativeAPI()", env=base)
        finally:
            native.write_bytes(original_native)

        for index in range(20):
            repeated = run(str(python), "-c", "import sharkbite, pysharkbite", env=base)
            if repeated.returncode:
                raise SystemExit(f"repeated interpreter start failed at iteration {index}")
        print("PASS repeated interpreter starts (20)")

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
