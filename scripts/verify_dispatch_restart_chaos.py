"""Fail-closed evidence verifier for the scheduled gateway server restart campaign."""

from __future__ import annotations

import argparse
from collections import Counter
import hashlib
import json
from pathlib import Path
import re


TEST = "TestDispatchClaimSurvivesLostResponseAndServerSIGKILL"
PACKAGE = "github.com/phrocker/shoal-oss/internal/explorerfleet"
ACTION_ID = "server-restart-claim-action"
FAULT = "prepared_claim_answer_dropped_then_server_sigkill"
IMAGE = re.compile(r"^[a-z0-9./_-]+@sha256:[0-9a-f]{64}$")


def digest(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def verify(args: argparse.Namespace) -> bool:
    evidence = args.events.parent
    artifacts = {name: evidence / name for name in (
        "events.jsonl", "stderr.log", "exit-code.txt", "image-pull.log",
        "image-pull-exit.txt", "image-digest.txt")}
    present = all(path.is_file() for path in artifacts.values())
    counts: Counter[str] = Counter()
    fault_counts: Counter[str] = Counter()
    observations: list[dict] = []
    invalid_lines = 0
    if args.events.is_file():
        for line in args.events.read_text(encoding="utf-8", errors="replace").splitlines():
            try:
                event = json.loads(line)
            except json.JSONDecodeError:
                invalid_lines += 1
                continue
            if not isinstance(event, dict):
                invalid_lines += 1
                continue
            if event.get("Package") != PACKAGE or event.get("Test") != TEST:
                continue
            action = event.get("Action")
            if action in ("run", "pass", "fail"):
                counts[action] += 1
            elif action == "output" and isinstance(event.get("Output"), str):
                output = event["Output"]
                for marker in (
                    "CHAOS_SERVER_START role=first",
                    "CHAOS_SERVER_START role=replacement",
                    "CHAOS_FAULT role=first container_sigkill_confirmed",
                ):
                    if marker in output:
                        fault_counts[marker] += 1
                if "CHAOS_RESULT " in output:
                    try:
                        observations.append(json.loads(output.split("CHAOS_RESULT ", 1)[1]))
                    except json.JSONDecodeError:
                        invalid_lines += 1

    expected = {
        "action_id": ACTION_ID,
        "server_mode": "two_container_replacement",
        "fault": FAULT,
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
    # Python considers True == 1. A forged JSON number must not certify a
    # boolean refusal marker, nor a boolean certify a recovered version.
    exact_observation = lambda value: (
        isinstance(value, dict) and value.keys() == expected.keys()
        and all(type(value[key]) is type(want) and value[key] == want
                for key, want in expected.items())
    )
    image_verified = bool(
        present and IMAGE.fullmatch(args.image_ref)
        and artifacts["image-digest.txt"].read_text().strip() == args.image_ref
        and artifacts["image-pull-exit.txt"].read_text().strip() == "0"
        and artifacts["exit-code.txt"].read_text().strip() == str(args.exit_code)
    )
    valid_args = bool(
        args.repeats > 0 and args.seed > 0
        and re.fullmatch(r"[0-9a-f]{40}", args.commit)
    )
    passed = bool(
        valid_args and present and image_verified and args.exit_code == 0
        and invalid_lines == 0 and counts == {"run": args.repeats, "pass": args.repeats}
        and fault_counts == {
            "CHAOS_SERVER_START role=first": args.repeats,
            "CHAOS_SERVER_START role=replacement": args.repeats,
            "CHAOS_FAULT role=first container_sigkill_confirmed": args.repeats,
        }
        and len(observations) == args.repeats
        and all(exact_observation(observation) for observation in observations)
    )
    manifest = {
        "schema_version": 1,
        "commit": args.commit,
        "run_id": args.run_id,
        "run_attempt": args.run_attempt,
        "seed": args.seed,
        "seed_scope": "Go test shuffle; network and process scheduling are nondeterministic",
        "repeats": args.repeats,
        "deployment_mode": "standalone_two_container_server_replacement",
        "image_digests": {"server_test_runtime": args.image_ref} if image_verified else {},
        "image_verified": image_verified,
        "injected_fault": FAULT,
        "action_identity": ACTION_ID,
        "expected_invariant": (
            "a lost prepared-claim answer grants no executable material; after SIGKILL "
            "the replacement retains one claim fence, replays the exact prepared claim, "
            "retrieves claimant-only material, durably prepares one bound request, and "
            "refuses rival material/prepare, request drift and key substitution; target delivery and "
            "post-Prepare crash recovery are not exercised"
        ),
        "observed_outcome": "pass" if passed else "fail",
        "go_exit_code": args.exit_code,
        "invalid_json_lines": invalid_lines,
        "test_events": dict(counts),
        "fault_events": dict(fault_counts),
        "observations": observations,
        "go_version": args.go_version,
        "runner_image": args.runner_image,
        "artifact_sha256": {name: digest(path) for name, path in artifacts.items() if path.is_file()},
    }
    args.manifest.write_text(json.dumps(manifest, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    return passed


def main() -> int:
    parser = argparse.ArgumentParser()
    for name in ("events", "manifest"):
        parser.add_argument("--" + name, type=Path, required=True)
    for name in ("exit-code", "repeats", "seed"):
        parser.add_argument("--" + name, type=int, required=True)
    for name in ("commit", "image-ref", "go-version", "runner-image", "run-id", "run-attempt"):
        parser.add_argument("--" + name, required=True)
    args = parser.parse_args()
    passed = verify(args)
    print("gateway restart chaos:", "pass" if passed else "fail")
    return 0 if passed else 1


if __name__ == "__main__":
    raise SystemExit(main())
