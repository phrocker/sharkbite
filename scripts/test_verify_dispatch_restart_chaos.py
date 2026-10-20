"""Evidence corruption must not certify the scheduled server restart campaign."""

from __future__ import annotations

import argparse
import json
from pathlib import Path
import tempfile
import unittest

from scripts import verify_dispatch_restart_chaos as campaign


class RestartEvidenceTest(unittest.TestCase):
    def test_exact_pass_and_fail_closed_variants(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            image = "ubuntu@sha256:" + "a" * 64
            for name, value in {
                "stderr.log": "", "exit-code.txt": "0\n", "image-pull.log": "pulled\n",
                "image-pull-exit.txt": "0\n", "image-digest.txt": image + "\n",
            }.items():
                (root / name).write_text(value, encoding="utf-8")
            observation = {
                "action_id": campaign.ACTION_ID,
                "server_mode": "two_container_replacement",
                "fault": campaign.FAULT,
                "first_response": "repull",
                "recovered_state": "claimed",
                "recovered_version": 2,
                "recovered_fence": 1,
                "protocol_version": 1,
                "exact_replay": True,
                "material_bound": True,
                "prepare_committed": True,
                "prepare_version": 3,
                "rival_material_refused": True,
                "rival_prepare_refused": True,
                "drift_prepare_refused": True,
                "key_prepare_refused": True,
                "rival_refused": True,
            }
            event = lambda action, output=None: json.dumps({
                "Package": campaign.PACKAGE, "Test": campaign.TEST, "Action": action,
                **({"Output": output} if output is not None else {}),
            })
            good = [
                event("run"),
                event("output", "CHAOS_SERVER_START role=first\n"),
                event("output", "CHAOS_FAULT role=first container_sigkill_confirmed\n"),
                event("output", "CHAOS_SERVER_START role=replacement\n"),
                event("output", "CHAOS_RESULT " + json.dumps(observation) + "\n"),
                event("pass"),
            ]
            events = root / "events.jsonl"
            args = argparse.Namespace(
                events=events, manifest=root / "manifest.json", exit_code=0, repeats=1,
                seed=7, commit="b" * 40, image_ref=image, go_version="go1.25",
                runner_image="ubuntu-24.04/202610", run_id="123", run_attempt="1",
            )
            events.write_text("\n".join(good) + "\n", encoding="utf-8")
            self.assertTrue(campaign.verify(args))
            self.assertEqual(json.loads(args.manifest.read_text())["observed_outcome"], "pass")

            for bad in (good[:-1], good + [good[4]], good + [event("fail")],
                        good[:2] + good[3:],
                        good + ["{broken"],
                        good[:4] + [event("output", "CHAOS_RESULT " + json.dumps({
                            **observation, "prepare_committed": False}) + "\n"), good[5]],
                        good[:4] + [event("output", "CHAOS_RESULT " + json.dumps({
                            **observation, "prepare_committed": 1}) + "\n"), good[5]],
                        good[:4] + [event("output", "CHAOS_RESULT " + json.dumps({
                            **observation, "recovered_version": True}) + "\n"), good[5]],
                        good[:4] + [event("output", "CHAOS_RESULT {}\n"), good[5]]):
                events.write_text("\n".join(bad) + "\n", encoding="utf-8")
                self.assertFalse(campaign.verify(args))
                self.assertEqual(json.loads(args.manifest.read_text())["observed_outcome"], "fail")

            events.write_text("\n".join(good) + "\n", encoding="utf-8")
            (root / "image-digest.txt").write_text("wrong\n", encoding="utf-8")
            self.assertFalse(campaign.verify(args))
            (root / "image-digest.txt").write_text(image + "\n", encoding="utf-8")
            args.exit_code = 124
            self.assertFalse(campaign.verify(args))


if __name__ == "__main__":
    unittest.main()
