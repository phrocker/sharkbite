from __future__ import annotations

import os
import unittest
from pathlib import Path
from unittest import mock

from sharkbite._native import library_candidates


class ArtifactPreviewPolicyTests(unittest.TestCase):
    def test_preview_marker_is_never_a_library_candidate(self):
        with mock.patch.dict(os.environ, {}, clear=True):
            candidates = tuple(library_candidates())
        self.assertTrue(candidates)
        self.assertTrue(
            all(Path(candidate).suffix.lower() in {".dll", ".dylib", ".so"} for candidate in candidates)
        )

    def test_system_discovery_requires_explicit_opt_in(self):
        with mock.patch.dict(os.environ, {}, clear=True):
            without_system = tuple(library_candidates())
        with (
            mock.patch.dict(
                os.environ, {"SHOAL_ALLOW_SYSTEM_LIBRARY": "1"}, clear=True
            ),
            mock.patch("ctypes.util.find_library", return_value="system-shoal"),
        ):
            with_system = tuple(library_candidates())
        self.assertNotIn("system-shoal", without_system)
        self.assertIn("system-shoal", with_system)


if __name__ == "__main__":
    unittest.main()
