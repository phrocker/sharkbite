from __future__ import annotations

import os
from pathlib import Path

from setuptools import setup
from setuptools.dist import Distribution
from wheel.bdist_wheel import bdist_wheel


def platform_distribution() -> bool:
    bundled = list((Path(__file__).parent / "src" / "sharkbite" / ".libs").glob("*"))
    has_native = any(
        path.suffix.lower() in {".dll", ".dylib", ".so"} for path in bundled
    )
    return has_native or os.environ.get("SHOAL_WHEEL_PREVIEW") == "1"


class PlatformDistribution(Distribution):
    def has_ext_modules(self) -> bool:
        return platform_distribution()


class PlatformWheel(bdist_wheel):
    def finalize_options(self) -> None:
        super().finalize_options()
        bundled = list((Path(__file__).parent / "src" / "sharkbite" / ".libs").glob("*"))
        self._has_bundled_native = any(
            path.suffix.lower() in {".dll", ".dylib", ".so"} for path in bundled
        )
        self._is_platform_preview = os.environ.get("SHOAL_WHEEL_PREVIEW") == "1"
        if self._is_platform_preview and self._has_bundled_native:
            raise RuntimeError("platform preview wheels must not bundle a native library")
        self.root_is_pure = not (self._has_bundled_native or self._is_platform_preview)

    def get_tag(self) -> tuple[str, str, str]:
        if not (self._has_bundled_native or self._is_platform_preview):
            return "py3", "none", "any"
        _, _, default_platform = super().get_tag()
        platform_tag = os.environ.get("SHOAL_WHEEL_PLATFORM_TAG", default_platform)
        return "py3", "none", platform_tag


setup(cmdclass={"bdist_wheel": PlatformWheel}, distclass=PlatformDistribution)
