"""Fail-closed checks for execution campaign evidence verification."""

from __future__ import annotations

import json
from pathlib import Path
import tempfile
import unittest

from scripts import execution_fault_evidence as evidence


class EvidenceTest(unittest.TestCase):
    def test_missing_or_malformed_pass_cannot_certify_campaign(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            events = root / "events.jsonl"
            stderr = root / "stderr.log"
            manifest = root / "manifest.json"
            stderr.write_text("", encoding="utf-8")
            lines = [json.dumps({
                "Action": action,
                "Package": "github.com/phrocker/shoal-oss/" + case["package"],
                "Test": case["test"],
            }) for case in evidence.CASES for action in ("run", "pass")]

            events.write_text("\n".join(lines) + "\n", encoding="utf-8")
            self.assertTrue(self.verify(events, stderr, manifest))
            self.assertEqual(json.loads(manifest.read_text())["observed_outcome"], "pass")
            self.assertEqual(
                json.loads(manifest.read_text())["seed_scope"],
                "Go test shuffle; concurrent process scheduling is nondeterministic",
            )

            events.write_text("\n".join(lines[:-1]) + "\n", encoding="utf-8")
            self.assertFalse(self.verify(events, stderr, manifest))
            self.assertEqual(json.loads(manifest.read_text())["observed_outcome"], "fail")

            events.write_text("\n".join(lines[1::2]) + "\n", encoding="utf-8")
            self.assertFalse(self.verify(events, stderr, manifest))
            self.assertEqual(json.loads(manifest.read_text())["cases"][0]["runs"], 0)

            events.write_text("\n".join(lines) + "\n{truncated\n", encoding="utf-8")
            self.assertFalse(self.verify(events, stderr, manifest))
            self.assertEqual(json.loads(manifest.read_text())["invalid_json_lines"], 1)

            events.write_text("\n".join(lines) + "\nnull\n", encoding="utf-8")
            self.assertFalse(self.verify(events, stderr, manifest))
            self.assertEqual(json.loads(manifest.read_text())["invalid_json_lines"], 1)

            events.write_text("\n".join(lines) + "\n", encoding="utf-8")
            self.assertFalse(self.verify(events, stderr, manifest, exit_code=1))

    @staticmethod
    def verify(events: Path, stderr: Path, manifest: Path, exit_code: int = 0) -> bool:
        return evidence.verify(events, stderr, exit_code, 1, 1729341212,
                               "a" * 40, "go1.26.4", "local-runner", "local", "1", manifest)


if __name__ == "__main__":
    unittest.main()
