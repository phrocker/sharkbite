"""Fail-closed chaos checks for a built Sharkbite wheel.

The checks deliberately mutate only a disposable virtual environment. They are
intended to run inside a clean release container before publication.
"""
from __future__ import annotations

import argparse
import os
import subprocess
import sys
import tempfile
import venv
from pathlib import Path


def run(*args: str, env: dict[str, str] | None = None) -> subprocess.CompletedProcess[str]:
    return subprocess.run(args, text=True, capture_output=True, env=env)


def expect_failure(label: str, code: str, *, env: dict[str, str], message: str) -> None:
    """Require the documented fail-closed refusal, not merely a nonzero exit.

    A crash, a signal, or an unrelated exception would also exit nonzero; none
    of those proves the loader refused for the injected reason, so each is a
    gate failure.
    """
    result = run(env["PYTHON"], "-c", code, env=env)
    output = (result.stdout + result.stderr).strip()
    last = output.splitlines()[-1] if output else ""
    if result.returncode < 0:
        raise SystemExit(
            f"chaos case {label} died from signal {-result.returncode}; a crash is not a fail-closed refusal"
        )
    if result.returncode == 0:
        raise SystemExit(f"chaos case {label} unexpectedly succeeded")
    if not last.startswith("ImportError:") or message not in last:
        raise SystemExit(
            f"chaos case {label} failed for the wrong reason; expected ImportError containing "
            f"{message!r}, got: {last or 'no diagnostic'}"
        )
    print(f"PASS {label}: {last}")


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
        native_smoke = run(
            str(python),
            "-c",
            "from sharkbite import NativeAPI; api = NativeAPI(); print(api.info.version)",
            env=base,
        )
        if native_smoke.returncode:
            print(native_smoke.stdout, end="")
            print(native_smoke.stderr, end="", file=sys.stderr)
            return native_smoke.returncode
        print(f"PASS clean native load: {native_smoke.stdout.strip()}")

        package = Path(
            run(str(python), "-c", "import pathlib, sharkbite; print(pathlib.Path(sharkbite.__file__).parent)", env=base).stdout.strip()
        )
        libs = package / ".libs"
        manifest = libs / "_shoal_bundle.json"
        native = next((p for p in libs.iterdir() if p.suffix.lower() in {".so", ".dylib", ".dll"}), None)
        if native is None or not manifest.is_file():
            raise SystemExit("wheel has no bundled native library and manifest")

        load = "from sharkbite import NativeAPI; NativeAPI()"

        relative_env = dict(base, SHOAL_LIBRARY="relative-shoal.so")
        expect_failure(
            "relative library override",
            load,
            env=relative_env,
            message="SHOAL_LIBRARY must be an absolute path",
        )

        original_manifest = manifest.read_bytes()
        manifest.write_text("{}", encoding="utf-8")
        try:
            expect_failure(
                "corrupt checksum manifest",
                load,
                env=base,
                message="bundled Shoal library manifest is missing or invalid",
            )
        finally:
            manifest.write_bytes(original_manifest)

        original_native = native.read_bytes()
        native.write_bytes(original_native + b"chaos")
        try:
            expect_failure(
                "tampered native library",
                load,
                env=base,
                message="bundled Shoal library checksum does not match its manifest",
            )
        finally:
            native.write_bytes(original_native)

        removed = native.with_name(native.name + ".removed")
        native.rename(removed)
        try:
            expect_failure(
                "removed native library",
                load,
                env=base,
                message="unable to load the Shoal shared library",
            )
        finally:
            removed.rename(native)

        for index in range(20):
            repeated = run(str(python), "-c", "import sharkbite, pysharkbite", env=base)
            if repeated.returncode:
                raise SystemExit(f"repeated interpreter start failed at iteration {index}")
        print("PASS repeated interpreter starts (20)")

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
