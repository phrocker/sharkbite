from __future__ import annotations

import argparse
import hashlib
import inspect
import json
import os
import shutil
import subprocess
import sys
import tarfile
import venv
import zipfile
from pathlib import Path

PYTHON_DIR = Path(__file__).resolve().parents[1]


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def verify_archives(dist: Path, manifest: dict[str, object]) -> Path:
    artifacts = manifest["artifacts"]
    for item in artifacts:
        path = dist / item["filename"]
        if not path.is_file():
            raise RuntimeError(f"missing artifact {path.name}")
        if path.stat().st_size != item["size"] or sha256(path) != item["sha256"]:
            raise RuntimeError(f"artifact digest mismatch: {path.name}")
    sums = {
        name: digest
        for digest, name in (
            line.split("  ", 1)
            for line in (dist / "SHA256SUMS").read_text(encoding="ascii").splitlines()
        )
    }
    if sums != {item["filename"]: item["sha256"] for item in artifacts}:
        raise RuntimeError("SHA256SUMS does not match release-manifest.json")

    wheel = next(dist.glob("*.whl"))
    expected_tag = manifest["platform_tag"]
    if not wheel.name.endswith(f"-{expected_tag}.whl"):
        raise RuntimeError(f"wheel tag does not match manifest: {wheel.name}")
    with zipfile.ZipFile(wheel) as archive:
        names = set(archive.namelist())
        required_suffixes = {
            "sharkbite/__init__.py",
            "pysharkbite/__init__.py",
            "sharkbite/.libs/_shoal_bundle.json",
        }
        missing = [
            suffix
            for suffix in required_suffixes
            if not any(name.endswith(suffix) for name in names)
        ]
        if missing:
            raise RuntimeError(f"wheel is missing {sorted(missing)}")
        libraries = [
            name
            for name in names
            if "/sharkbite/.libs/" in f"/{name}"
            and name.lower().endswith((".dll", ".dylib", ".so"))
        ]
        if len(libraries) != 1:
            raise RuntimeError("wheel must contain exactly one Shoal shared library")
        bundle_name = next(
            name
            for name in names
            if name.endswith("sharkbite/.libs/_shoal_bundle.json")
        )
        bundle = json.loads(archive.read(bundle_name))
        if hashlib.sha256(archive.read(libraries[0])).hexdigest() != bundle["library"]["sha256"]:
            raise RuntimeError("bundled library digest mismatch")
        wheel_metadata = archive.read(
            next(name for name in names if name.endswith(".dist-info/WHEEL"))
        ).decode()
        if "Root-Is-Purelib: false" not in wheel_metadata:
            raise RuntimeError("wheel is incorrectly marked pure")
        metadata = archive.read(
            next(name for name in names if name.endswith(".dist-info/METADATA"))
        ).decode()
        package = manifest["package"]
        if f"Name: {package['name']}" not in metadata or f"Version: {package['version']}" not in metadata:
            raise RuntimeError("wheel metadata does not match release manifest")

    sdist = next(dist.glob("*.tar.gz"))
    with tarfile.open(sdist, "r:gz") as archive:
        names = archive.getnames()
        if not any(name.endswith("/src/sharkbite/__init__.py") for name in names):
            raise RuntimeError("sdist is missing sharkbite sources")
        if not any(name.endswith("/src/pysharkbite/__init__.py") for name in names):
            raise RuntimeError("sdist is missing pysharkbite sources")
        if any(name.lower().endswith((".dll", ".dylib", ".so")) for name in names):
            raise RuntimeError("source distribution must not contain native binaries")
    return wheel


def safe_path(environment: Path) -> tuple[Path, str]:
    if os.name == "nt":
        python = environment / "Scripts" / "python.exe"
        path = os.pathsep.join(
            [str(environment / "Scripts"), str(Path(os.environ["SystemRoot"]) / "System32")]
        )
    else:
        python = environment / "bin" / "python"
        path = str(environment / "bin")
    return python, path


def pip_install(python: Path, artifact: Path) -> None:
    env = os.environ.copy()
    env.pop("PYTHONPATH", None)
    env.pop("SHOAL_LIBRARY", None)
    env.pop("SHOAL_ALLOW_SYSTEM_LIBRARY", None)
    subprocess.run(
        [
            str(python),
            "-m",
            "pip",
            "install",
            "--no-index",
            "--no-deps",
            "--force-reinstall",
            str(artifact),
        ],
        cwd=artifact.parent,
        env=env,
        check=True,
    )


def install_smoke(wheel: Path, sdist: Path) -> None:
    verification_root = PYTHON_DIR / "build" / "artifact-verification"
    shutil.rmtree(verification_root, ignore_errors=True)
    verification_root.mkdir(parents=True)
    try:
        external_native = verification_root / "external-native"
        external_native.mkdir()
        with zipfile.ZipFile(wheel) as archive:
            native_name = next(
                name
                for name in archive.namelist()
                if "/sharkbite/.libs/" in f"/{name}"
                and name.lower().endswith((".dll", ".dylib", ".so"))
            )
            native_path = external_native / Path(native_name).name
            native_path.write_bytes(archive.read(native_name))

        environment = verification_root / "wheel-venv"
        venv.EnvBuilder(with_pip=True, clear=True).create(environment)
        python, restricted_path = safe_path(environment)
        pip_install(python, wheel)
        code = """
import importlib.metadata
import importlib.util
import shutil
import pysharkbite
import sharkbite
import shoal
from shoal.document import Document
from sharkbite import Mutation, NativeAPI
assert shutil.which("go") is None
distribution = importlib.metadata.distribution("shoal-sharkbite")
assert distribution.metadata["Name"] == "shoal-sharkbite"
assert distribution.version == sharkbite.__version__
owners = importlib.metadata.packages_distributions()
assert "shoal-sharkbite" in owners["sharkbite"]
assert "shoal-sharkbite" in owners["pysharkbite"]
assert "shoal-sharkbite" in owners["shoal"]
assert importlib.util.find_spec("sharkbite").origin == sharkbite.__file__
assert importlib.util.find_spec("pysharkbite").origin == pysharkbite.__file__
assert importlib.util.find_spec("shoal").origin == shoal.__file__
assert Document.__module__ == "shoal.document"
assert pysharkbite.__all__ == sharkbite.__all__
for name in sharkbite.__all__:
    assert getattr(pysharkbite, name) is getattr(sharkbite, name)
assert pysharkbite.__version__ == sharkbite.__version__
api = NativeAPI()
assert api.info.version >= (1, 19, 0)
api.require(6, 7, 8, 9, 10, 11, 12, 16, 19, 21, 22, 27, 28, 29)
with Mutation(b"release-smoke", _api=api) as mutation:
    mutation.put(b"cf", b"cq", b"A", 1, b"value")
    assert mutation.size() > 0
print(sharkbite.__version__, api.info.version, api.path)
"""
        env = os.environ.copy()
        env.pop("PYTHONPATH", None)
        env.pop("SHOAL_LIBRARY", None)
        env.pop("SHOAL_ALLOW_SYSTEM_LIBRARY", None)
        env["PATH"] = restricted_path
        subprocess.run([str(python), "-I", "-c", code], env=env, check=True)

        preload_code = """
import ctypes
import os
native = ctypes.CDLL(os.environ["RELEASE_NATIVE_LIBRARY"])
assert native.shoal_abi_version_major() == 1
import pysharkbite
import sharkbite
from sharkbite import NativeAPI
assert pysharkbite.Client is sharkbite.Client
api = NativeAPI(os.environ["RELEASE_NATIVE_LIBRARY"])
assert api.info.version >= (1, 17, 0)
"""
        preload_env = env.copy()
        preload_env["RELEASE_NATIVE_LIBRARY"] = str(native_path.resolve())
        subprocess.run(
            [str(python), "-I", "-c", preload_code],
            env=preload_env,
            check=True,
        )

        source_root = verification_root / "sdist"
        source_root.mkdir()
        with tarfile.open(sdist, "r:gz") as archive:
            for member in archive.getmembers():
                target = (source_root / member.name).resolve()
                try:
                    target.relative_to(source_root.resolve())
                except ValueError as exc:
                    raise RuntimeError("sdist contains an unsafe path") from exc
                if member.issym() or member.islnk():
                    raise RuntimeError("sdist must not contain links")
            extract_options = (
                {"filter": "fully_trusted"}
                if "filter" in inspect.signature(archive.extractall).parameters
                else {}
            )
            archive.extractall(source_root, **extract_options)
        source_tree = next(path for path in source_root.iterdir() if path.is_dir())
        source_wheels = verification_root / "sdist-wheel"
        source_wheels.mkdir()
        subprocess.run(
            [
                sys.executable,
                "-m",
                "build",
                "--no-isolation",
                "--wheel",
                "--outdir",
                str(source_wheels),
            ],
            cwd=source_tree,
            check=True,
        )
        source_wheel = next(source_wheels.glob("*-py3-none-any.whl"))

        source_environment = verification_root / "sdist-venv"
        venv.EnvBuilder(with_pip=True, clear=True).create(source_environment)
        source_python, source_path = safe_path(source_environment)
        pip_install(source_python, source_wheel)
        source_code = """
import importlib.metadata
import os
import shutil
from pathlib import Path
import pysharkbite
import sharkbite
import shoal
from shoal.document import Document
from sharkbite import Mutation, NativeAPI
assert shutil.which("go") is None
distribution = importlib.metadata.distribution("shoal-sharkbite")
assert distribution.metadata["Name"] == "shoal-sharkbite"
assert distribution.version == sharkbite.__version__
assert pysharkbite.__all__ == sharkbite.__all__
assert Document.__module__ == "shoal.document"
for name in sharkbite.__all__:
    assert getattr(pysharkbite, name) is getattr(sharkbite, name)
assert not any((Path(sharkbite.__file__).parent / ".libs").glob("*"))
api = NativeAPI(os.environ["RELEASE_NATIVE_LIBRARY"])
assert api.info.version >= (1, 17, 0)
with Mutation(b"sdist-smoke", _api=api) as mutation:
    mutation.put(b"cf", b"cq", b"", 1, b"value")
    assert mutation.size() > 0
"""
        source_env = os.environ.copy()
        source_env.pop("PYTHONPATH", None)
        source_env.pop("SHOAL_LIBRARY", None)
        source_env.pop("SHOAL_ALLOW_SYSTEM_LIBRARY", None)
        source_env["PATH"] = source_path
        source_env["RELEASE_NATIVE_LIBRARY"] = str(native_path.resolve())
        subprocess.run(
            [str(source_python), "-I", "-c", source_code],
            env=source_env,
            check=True,
        )
    finally:
        shutil.rmtree(verification_root, ignore_errors=True)


def main() -> None:
    parser = argparse.ArgumentParser(description="Verify Shoal Python artifacts")
    parser.add_argument("--dist", type=Path, default=PYTHON_DIR / "dist")
    parser.add_argument("--skip-install", action="store_true")
    args = parser.parse_args()
    manifest = json.loads(
        (args.dist / "release-manifest.json").read_text(encoding="utf-8")
    )
    wheel = verify_archives(args.dist, manifest)
    if not args.skip_install:
        install_smoke(wheel, next(args.dist.glob("*.tar.gz")))
    print("release artifacts verified")


if __name__ == "__main__":
    main()
