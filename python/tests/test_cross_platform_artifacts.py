from __future__ import annotations

import importlib.util
import unittest
from pathlib import Path


SCRIPT = Path(__file__).resolve().parents[2] / "scripts" / "cross_platform_artifacts.py"
SPEC = importlib.util.spec_from_file_location("cross_platform_artifacts", SCRIPT)
assert SPEC is not None and SPEC.loader is not None
artifact_evidence = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(artifact_evidence)


class CrossPlatformArtifactTests(unittest.TestCase):
    def test_declared_export_inventory_contains_discovery_and_lifecycle(self):
        exports = artifact_evidence.declared_exports()
        self.assertGreaterEqual(len(exports), 300)
        self.assertIn("shoal_abi_version_packed", exports)
        self.assertIn("shoal_abi_has_capability", exports)
        self.assertIn("shoal_connector_close", exports)
        self.assertIn("shoal_connector_free", exports)

    def test_cross_targets_remain_explicitly_arch_specific(self):
        self.assertEqual(
            artifact_evidence.TARGETS,
            (
                ("darwin", "amd64", "macosx_11_0_x86_64"),
                ("darwin", "arm64", "macosx_11_0_arm64"),
            ),
        )


if __name__ == "__main__":
    unittest.main()
