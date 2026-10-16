from __future__ import annotations

import hashlib
import json
import os
import tomllib
import unittest
from pathlib import Path
from unittest import mock

import pysharkbite
import sharkbite
from sharkbite._native import _verify_bundled_library, library_candidates


class ReleasePolicyTests(unittest.TestCase):
    def test_workflow_uses_guarded_tokenless_pypi_publishing(self):
        workflow = (
            Path(__file__).parents[2]
            / ".github"
            / "workflows"
            / "python-manylinux-release.yml"
        ).read_text(encoding="utf-8")
        self.assertIn("publish_to_pypi:", workflow)
        self.assertIn("default: false", workflow)
        self.assertIn("environment:\n      name: pypi", workflow)
        self.assertIn("url: https://pypi.org/p/sharkbite", workflow)
        self.assertIn("id-token: write", workflow)
        self.assertIn("pypa/gh-action-pypi-publish@", workflow)
        self.assertIn("Verify exact publish subjects", workflow)
        self.assertIn("github.ref_type == 'tag'", workflow)
        self.assertNotIn("PYPI_API_TOKEN", workflow)
        self.assertNotRegex(workflow, r"(?m)^\s+password:")

    def test_import_names_share_version_and_public_api(self):
        self.assertEqual(sharkbite.__version__, "1.3.0")
        self.assertEqual(pysharkbite.__version__, sharkbite.__version__)
        self.assertIs(pysharkbite.Connector, sharkbite.Connector)
        self.assertIn("__version__", sharkbite.__all__)
        self.assertEqual(pysharkbite.__all__, sharkbite.__all__)
        for name in sharkbite.__all__:
            self.assertIs(getattr(pysharkbite, name), getattr(sharkbite, name))

    def test_distribution_metadata_contract(self):
        project = tomllib.loads(
            (Path(__file__).parents[1] / "pyproject.toml").read_text(encoding="utf-8")
        )["project"]
        self.assertEqual(project["name"], "sharkbite")
        self.assertEqual(project["version"], sharkbite.__version__)
        self.assertEqual(project["requires-python"], ">=3.9")

    def test_discovery_does_not_search_working_directory_by_default(self):
        with mock.patch.dict(os.environ, {}, clear=True):
            candidates = list(library_candidates())
        self.assertTrue(candidates)
        package_libs = Path(sharkbite.__file__).resolve().parent / ".libs"
        self.assertTrue(all(Path(value).parent == package_libs for value in candidates))

    def test_relative_environment_override_is_rejected(self):
        with mock.patch.dict(os.environ, {"SHOAL_LIBRARY": "shoal.dll"}, clear=True):
            with self.assertRaisesRegex(ImportError, "absolute path"):
                list(library_candidates())

    def test_explicit_library_precedes_environment_and_bundle(self):
        explicit = Path("explicit-shoal").resolve()
        configured = Path("configured-shoal").resolve()
        with mock.patch.dict(
            os.environ, {"SHOAL_LIBRARY": str(configured)}, clear=True
        ):
            candidates = list(library_candidates(explicit))
        self.assertEqual(candidates[:2], [str(explicit), str(configured)])

    def test_bundled_checksum_mismatch_is_rejected(self):
        package_libs = Path(sharkbite.__file__).resolve().parent / ".libs"
        package_libs.mkdir(exist_ok=True)
        library = package_libs / "test-shoal.dll"
        manifest = package_libs / "_shoal_bundle.json"
        original_manifest = manifest.read_bytes() if manifest.exists() else None
        try:
            library.write_bytes(b"library")
            manifest.write_text(
                json.dumps(
                    {
                        "library": {
                            "filename": library.name,
                            "sha256": hashlib.sha256(b"different").hexdigest(),
                        }
                    }
                ),
                encoding="utf-8",
            )
            with self.assertRaisesRegex(ImportError, "checksum"):
                _verify_bundled_library(library)
        finally:
            library.unlink(missing_ok=True)
            if original_manifest is None:
                manifest.unlink(missing_ok=True)
            else:
                manifest.write_bytes(original_manifest)
            try:
                package_libs.rmdir()
            except OSError:
                pass


if __name__ == "__main__":
    unittest.main()


class ChaosGateTests(unittest.TestCase):
    """The chaos helper must accept only the documented ImportError."""

    @classmethod
    def setUpClass(cls):
        import importlib.util
        import sys

        path = Path(__file__).parents[1] / "scripts" / "chaos_release.py"
        spec = importlib.util.spec_from_file_location("chaos_release", path)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        cls.chaos = module
        cls.env = dict(os.environ, PYTHON=sys.executable, PYTHONPATH="")

    def expect(self, code, message="documented refusal"):
        return self.chaos.expect_failure("case", code, env=self.env, message=message)

    def test_documented_import_error_passes(self):
        self.expect("raise ImportError('documented refusal: detail')")

    def test_success_fails_the_gate(self):
        with self.assertRaisesRegex(SystemExit, "unexpectedly succeeded"):
            self.expect("pass")

    def test_unrelated_exception_fails_the_gate(self):
        with self.assertRaisesRegex(SystemExit, "wrong reason"):
            self.expect("raise RuntimeError('documented refusal')")

    def test_wrong_message_fails_the_gate(self):
        with self.assertRaisesRegex(SystemExit, "wrong reason"):
            self.expect("raise ImportError('something else')")

    def test_signal_exit_fails_the_gate(self):
        with self.assertRaisesRegex(SystemExit, "died from signal"):
            self.expect("import os, signal; os.kill(os.getpid(), signal.SIGSEGV)")

    def test_plain_nonzero_exit_fails_the_gate(self):
        with self.assertRaisesRegex(SystemExit, "wrong reason"):
            self.expect("raise SystemExit(3)")
