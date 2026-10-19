"""Verify and summarize reproducible execution fault campaigns."""

from __future__ import annotations

import argparse
from collections import Counter
import hashlib
import json
from pathlib import Path
import re
import sys


CASES = (
    {
        "package": "internal/explorercoord",
        "test": "TestRuntimeCrashRecoveryAtEveryDurableStage",
        "fault": "crash at each durable publication stage",
        "invariant": "recovery converges without a silent commit",
        "action_identity": None,
    },
    {
        "package": "internal/explorercoord",
        "test": "TestExplorerRetryReusesDurableAttemptAcrossPublicationFaults",
        "fault": "publication failure and retry",
        "invariant": "retry uses the durable attempt identity",
        "action_identity": None,
    },
    {
        "package": "internal/explorerfleet",
        "test": "TestDispatchStoreCASReplayFenceAndRestart",
        "fault": "concurrent claims, stale fence, and runtime restart",
        "invariant": "one claim wins and the durable record survives restart",
        "action_identity": "YQD_",  # testActionRecord's opaque ID: b"a\\x00\\xff"
    },
    {
        "package": "internal/explorerfleet",
        "test": "TestDispatchGatewayClaimAcrossProcesses",
        "fault": "two gateway processes claim the same version",
        "invariant": "one fenced claim wins; exact retry reuses its receipt",
        "action_identity": "cHJvY2Vzcy1yYWNlLWFjdGlvbg",  # process-race-action
    },
    {
        "package": "internal/explorerfleet",
        "test": "TestDispatchStoreEnforcesMonotonicFenceAndEffect",
        "fault": "stale fence and effect-state rewrite",
        "invariant": "fence and effect metadata never move backward",
        "action_identity": None,
    },
)
CONTAINER_CASES = (CASES[3],)
SUITES = {"standalone": CASES, "container": CONTAINER_CASES}
IMAGE_REF = re.compile(r"^[a-z0-9./_-]+@sha256:([0-9a-f]{64})$")


def pattern(suite: str = "standalone") -> str:
    return "^(" + "|".join(re.escape(case["test"]) for case in SUITES[suite]) + ")$"


def sha256(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def verify(events: Path, stderr: Path, exit_code: int, repeats: int, seed: int,
           commit: str, go_version: str, runner_image: str, run_id: str,
           run_attempt: str,
           manifest: Path, suite: str = "standalone", image_ref: str = "") -> bool:
    started: Counter[tuple[str, str]] = Counter()
    passed: Counter[tuple[str, str]] = Counter()
    failed: Counter[tuple[str, str]] = Counter()
    parse_errors = 0
    for line in events.read_text(encoding="utf-8", errors="replace").splitlines():
        try:
            event = json.loads(line)
        except json.JSONDecodeError:
            parse_errors += 1
            continue
        if not isinstance(event, dict):
            parse_errors += 1
            continue
        name = event.get("Test")
        package = event.get("Package")
        if not isinstance(name, str) or not isinstance(package, str):
            continue
        key = (package, name)
        if event.get("Action") == "run":
            started[key] += 1
        elif event.get("Action") == "pass":
            passed[key] += 1
        elif event.get("Action") == "fail":
            failed[key] += 1

    observations = []
    image_match = IMAGE_REF.fullmatch(image_ref)
    artifact_paths = {"events.jsonl": events, "stderr.log": stderr}
    image_verified = True
    if suite == "container":
        for name in ("image-pull.log", "image-digest.txt", "image-pull-exit.txt", "exit-code.txt"):
            path = events.parent / name
            if path.is_file():
                artifact_paths[name] = path
            else:
                image_verified = False
        if image_verified:
            image_verified = (
                (events.parent / "image-digest.txt").read_text(encoding="utf-8", errors="replace").strip() == image_ref
                and (events.parent / "image-pull-exit.txt").read_text(encoding="utf-8", errors="replace").strip() == "0"
                and (events.parent / "exit-code.txt").read_text(encoding="utf-8", errors="replace").strip() == str(exit_code)
            )
    good = exit_code == 0 and parse_errors == 0 and (
        suite == "standalone" or (image_match is not None and image_verified))
    for case in SUITES[suite]:
        key = ("github.com/phrocker/shoal-oss/" + case["package"], case["test"])
        run_count, pass_count, fail_count = started[key], passed[key], failed[key]
        case_good = run_count == repeats and pass_count == repeats and fail_count == 0
        good = good and case_good
        observations.append({**case, "runs": run_count, "passes": pass_count, "failures": fail_count,
                             "outcome": "pass" if case_good else "fail"})

    result = {
        "schema_version": 1,
        "commit": commit,
        "run_id": run_id,
        "run_attempt": run_attempt,
        "seed": seed,
        "seed_scope": "Go test shuffle; concurrent process scheduling is nondeterministic",
        "repeats": repeats,
        "deployment_mode": (
            "standalone_embedded" if suite == "standalone"
            else "standalone_two_gateway_containers"),
        "go_version": go_version,
        "runner_image": runner_image,
        "image_digests": ({"gateway_test_runtime": "sha256:" + image_match.group(1)}
                          if suite == "container" and image_match else {}),
        "expected_invariant": "every selected case passes exactly the requested repeat count",
        "observed_outcome": "pass" if good else "fail",
        "go_exit_code": exit_code,
        "invalid_json_lines": parse_errors,
        "cases": observations,
        "artifact_sha256": {name: sha256(path) for name, path in artifact_paths.items()},
    }
    manifest.write_text(json.dumps(result, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    return good


def main() -> int:
    parser = argparse.ArgumentParser()
    sub = parser.add_subparsers(dest="command", required=True)
    names = sub.add_parser("pattern")
    names.add_argument("--suite", choices=SUITES, default="standalone")
    check = sub.add_parser("verify")
    check.add_argument("--suite", choices=SUITES, default="standalone")
    check.add_argument("--image-ref", default="")
    for name in ("events", "stderr", "manifest"):
        check.add_argument("--" + name, type=Path, required=True)
    for name in ("exit-code", "repeats", "seed"):
        check.add_argument("--" + name, type=int, required=True)
    for name in ("commit", "go-version", "runner-image", "run-id", "run-attempt"):
        check.add_argument("--" + name, required=True)
    args = parser.parse_args()
    if args.command == "pattern":
        print(pattern(args.suite))
        return 0
    if args.repeats < 1 or args.seed < 1 or not re.fullmatch(r"[0-9a-f]{40}", args.commit):
        parser.error("repeats, seed, or commit is invalid")
    good = verify(args.events, args.stderr, args.exit_code, args.repeats,
                  args.seed, args.commit, args.go_version, args.runner_image,
                  args.run_id, args.run_attempt, args.manifest, args.suite,
                  args.image_ref)
    print("execution fault campaign:", "pass" if good else "fail")
    return 0 if good else 1


if __name__ == "__main__":
    sys.exit(main())
