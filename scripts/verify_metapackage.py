from __future__ import annotations

import argparse
import email
import json
import sys
import tarfile
import urllib.error
import urllib.request
import zipfile
from pathlib import Path
from typing import Optional


ALLOWED_SDIST_FILES = {
    "compat_src/sharkbite/__init__.py",
    "LICENSE",
    "MANIFEST.in",
    "PKG-INFO",
    "PYTHONREADME.md",
    "README.md",
    "RELEASE.md",
    "pyproject.toml",
    "setup.cfg",
    "setup.py",
}
BRIDGE_INITIALIZER = "sharkbite/__init__.py"
BRIDGE_INITIALIZER_PREAMBLE = b'''"""Compatibility entry point layered over the Shoal implementation."""

from pkgutil import extend_path

__path__ = extend_path(__path__, __name__)

# The remainder of this file must match shoal-sharkbite's initializer exactly.
'''
ALLOWED_SDIST_EGG_INFO_FILES = {
    "compat_src/sharkbite.egg-info/PKG-INFO",
    "compat_src/sharkbite.egg-info/SOURCES.txt",
    "compat_src/sharkbite.egg-info/dependency_links.txt",
    "compat_src/sharkbite.egg-info/requires.txt",
    "compat_src/sharkbite.egg-info/top_level.txt",
}


def metadata_from_wheel(
    path: Path, implementation_wheel: Optional[Path] = None
):
    if not path.name.endswith("-py3-none-any.whl"):
        raise AssertionError(f"expected a universal py3-none-any wheel, found {path.name}")
    with zipfile.ZipFile(path) as archive:
        names = archive.namelist()
        metadata_names = [name for name in names if name.endswith(".dist-info/METADATA")]
        if len(metadata_names) != 1:
            raise AssertionError(f"expected one METADATA file, found {metadata_names}")
        metadata = email.message_from_bytes(archive.read(metadata_names[0]))
        expected_prefix = f"sharkbite-{metadata['Version']}.dist-info/"
        if metadata_names[0] != expected_prefix + "METADATA":
            raise AssertionError(f"unexpected dist-info directory: {metadata_names[0]}")
        disallowed = [
            name
            for name in names
            if name != BRIDGE_INITIALIZER and not name.startswith(expected_prefix)
        ]
        if disallowed:
            raise AssertionError(f"wheel contains installed files: {disallowed}")
        if names.count(BRIDGE_INITIALIZER) != 1:
            raise AssertionError(
                f"wheel must contain exactly one {BRIDGE_INITIALIZER}: {names}"
            )
        if implementation_wheel:
            with zipfile.ZipFile(implementation_wheel) as implementation_archive:
                try:
                    implementation_initializer = implementation_archive.read(
                        BRIDGE_INITIALIZER
                    )
                except KeyError as error:
                    raise AssertionError(
                        f"{implementation_wheel} does not contain {BRIDGE_INITIALIZER}"
                    ) from error
            expected_initializer = (
                BRIDGE_INITIALIZER_PREAMBLE + implementation_initializer
            ).replace(b"\r\n", b"\n")
            initializer = archive.read(BRIDGE_INITIALIZER).replace(b"\r\n", b"\n")
            if initializer != expected_initializer:
                raise AssertionError(
                    "bridge initializer does not match the Shoal initializer "
                    "after the required package-path preamble"
                )
        forbidden_dist_info_files = {"entry_points.txt"}
        entry_points = [
            name
            for name in names
            if Path(name).name in forbidden_dist_info_files
        ]
        if entry_points:
            raise AssertionError(f"wheel declares entry points/scripts: {entry_points}")
        return metadata


def verify_sdist(path: Path) -> None:
    with tarfile.open(path, "r:*") as archive:
        members = archive.getmembers()
        non_regular = [member.name for member in members if not (member.isfile() or member.isdir())]
        if non_regular:
            raise AssertionError(f"sdist contains non-regular members: {non_regular}")
        files = [member.name.split("/", 1)[-1] for member in members if member.isfile()]
    disallowed = [
        name
        for name in files
        if name not in ALLOWED_SDIST_FILES
        and name not in ALLOWED_SDIST_EGG_INFO_FILES
    ]
    if disallowed:
        raise AssertionError(f"sdist contains historical implementation files: {disallowed}")


def require_pypi_release(requirement: str) -> None:
    name, version = requirement.split("==", 1)
    url = f"https://pypi.org/pypi/{name}/{version}/json"
    try:
        with urllib.request.urlopen(url, timeout=30) as response:
            payload = json.load(response)
    except urllib.error.HTTPError as error:
        raise AssertionError(f"{requirement} is not published on PyPI") from error
    urls = payload.get("urls") or []
    if not urls:
        raise AssertionError(f"{requirement} has no PyPI artifacts")
    if not any(
        file.get("packagetype") == "bdist_wheel" and not file.get("yanked")
        for file in urls
    ):
        raise AssertionError(f"{requirement} has no non-yanked wheel published on PyPI")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("wheel", type=Path)
    parser.add_argument("--sdist", type=Path)
    parser.add_argument("--implementation-wheel", type=Path)
    parser.add_argument("--require-pypi", action="store_true")
    args = parser.parse_args()

    metadata = metadata_from_wheel(args.wheel, args.implementation_wheel)
    requirements = metadata.get_all("Requires-Dist", [])
    expected_requirement = f"shoal-sharkbite=={metadata['Version']}"
    if requirements != [expected_requirement]:
        raise AssertionError(
            f"expected same-version dependency {expected_requirement}: {requirements}"
        )
    if metadata["Requires-Python"] != ">=3.9":
        raise AssertionError(f"unexpected Requires-Python: {metadata['Requires-Python']}")
    if args.sdist:
        verify_sdist(args.sdist)
    if args.require_pypi:
        require_pypi_release(requirements[0])
    print(f"verified upgrade-safe compatibility wheel with {requirements[0]}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
