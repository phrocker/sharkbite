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

    def test_container_campaign_requires_image_digest_and_exact_passes(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            events = root / "events.jsonl"
            stderr = root / "stderr.log"
            manifest = root / "manifest.json"
            stderr.write_text("", encoding="utf-8")
            case = evidence.CONTAINER_CASES[0]
            line = json.dumps({
                "Action": "pass",
                "Package": "github.com/phrocker/shoal-oss/" + case["package"],
                "Test": case["test"],
            })
            events.write_text(line + "\n", encoding="utf-8")
            args = (events, stderr, 0, 1, 1729341212,
                    "a" * 40, "go1.25.0", "local-runner", "local", "1", manifest)
            self.assertFalse(evidence.verify(*args, suite="container"))
            image = "ubuntu@sha256:" + "a" * 64
            (root / "image-pull.log").write_text("pulled\n", encoding="utf-8")
            (root / "image-digest.txt").write_text(image + "\n", encoding="utf-8")
            (root / "image-pull-exit.txt").write_text("0\n", encoding="utf-8")
            (root / "exit-code.txt").write_text("0\n", encoding="utf-8")
            self.assertFalse(evidence.verify(*args, suite="container", image_ref=image))
            marker = json.dumps({
                "Action": "output",
                "Package": "github.com/phrocker/shoal-oss/" + case["package"],
                "Test": case["test"],
                "Output": "gateway_claim_child_mode=container\n",
            })
            events.write_text(line + "\n" + marker + "\n", encoding="utf-8")
            self.assertTrue(evidence.verify(*args, suite="container", image_ref=image))
            result = json.loads(manifest.read_text())
            self.assertEqual(result["deployment_mode"], "standalone_two_gateway_containers")
            self.assertEqual(result["image_digests"], {"gateway_test_runtime": "sha256:" + "a" * 64})
            self.assertTrue(result["image_verified"])
            self.assertEqual(result["cases"][0]["container_mode_markers"], 1)
            self.assertEqual(set(result["artifact_sha256"]), {
                "events.jsonl", "stderr.log", "image-pull.log", "image-digest.txt",
                "image-pull-exit.txt", "exit-code.txt",
            })
            (root / "image-digest.txt").write_text("wrong\n", encoding="utf-8")
            self.assertFalse(evidence.verify(*args, suite="container", image_ref=image))
            self.assertFalse(json.loads(manifest.read_text())["image_verified"])
            (root / "image-digest.txt").write_text(image + "\n", encoding="utf-8")
            self.assertFalse(evidence.verify(events, stderr, 0, 2, 1729341212,
                                             "a" * 40, "go1.25.0", "local-runner",
                                             "local", "1", manifest,
                                             suite="container", image_ref=image))

    @staticmethod
    def verify(events: Path, stderr: Path, manifest: Path, exit_code: int = 0) -> bool:
        return evidence.verify(events, stderr, exit_code, 1, 1729341212,
                               "a" * 40, "go1.26.4", "local-runner", "local", "1", manifest)


if __name__ == "__main__":
    unittest.main()
