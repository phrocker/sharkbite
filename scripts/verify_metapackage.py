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


ALLOWED_SDIST_FILES = {
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
ALLOWED_SDIST_EGG_INFO_FILES = {
    "sharkbite.egg-info/PKG-INFO",
    "sharkbite.egg-info/SOURCES.txt",
    "sharkbite.egg-info/dependency_links.txt",
    "sharkbite.egg-info/requires.txt",
    "sharkbite.egg-info/top_level.txt",
}


def metadata_from_wheel(path: Path):
    with zipfile.ZipFile(path) as archive:
        names = archive.namelist()
        metadata_names = [name for name in names if name.endswith(".dist-info/METADATA")]
        if len(metadata_names) != 1:
            raise AssertionError(f"expected one METADATA file, found {metadata_names}")
        disallowed = [
            name
            for name in names
            if ".dist-info/" not in name and not name.endswith(".dist-info")
        ]
        if disallowed:
            raise AssertionError(f"wheel contains installed files: {disallowed}")
        return email.message_from_bytes(archive.read(metadata_names[0]))


def verify_sdist(path: Path) -> None:
    with tarfile.open(path, "r:*") as archive:
        files = [
            member.name.split("/", 1)[-1]
            for member in archive.getmembers()
            if member.isfile()
        ]
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
    if not payload.get("urls"):
        raise AssertionError(f"{requirement} has no PyPI artifacts")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("wheel", type=Path)
    parser.add_argument("--sdist", type=Path)
    parser.add_argument("--require-pypi", action="store_true")
    args = parser.parse_args()

    metadata = metadata_from_wheel(args.wheel)
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
    print(f"verified metadata-only wheel with {requirements[0]}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
