#!/usr/bin/env python3
# Licensed to the Apache Software Foundation (ASF) under one or more
# contributor license agreements. See the NOTICE file distributed with
# this work for additional information regarding copyright ownership.
# The ASF licenses this file to You under the Apache License, Version 2.0
# (the "License"); you may not use this file except in compliance with
# the License. You may obtain a copy at
# http://www.apache.org/licenses/LICENSE-2.0
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
"""Integration probe for the local-decisions architecture; not a quality eval.

Uses only synthetic inputs, pinned published weights, and an in-process ASGI
client. Run in a separate environment; this is not a Shoal runtime dependency.
--offline requires artifacts already cached and disables Hugging Face downloads.
"""

import argparse
import hashlib
import importlib.metadata
import json
import math
import os
from pathlib import Path
import platform
import re
import time
import warnings


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def digest_file(path):
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--revision", required=True)
    parser.add_argument("--device", choices=("cuda", "cpu"), default="cuda")
    parser.add_argument("--cache", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--offline", action="store_true")
    args = parser.parse_args()
    require(re.fullmatch(r"[0-9a-f]{40}", args.revision), "revision must be an immutable SHA")
    os.environ["HF_HOME"] = str(args.cache)
    os.environ["HF_HUB_DISABLE_TELEMETRY"] = "1"
    os.environ["TOKENIZERS_PARALLELISM"] = "false"
    os.environ["USE_TF"] = "0"
    os.environ["LAYA_API_KEY"] = "synthetic-probe-token"
    if args.offline:
        os.environ["HF_HUB_OFFLINE"] = "1"
        os.environ["TRANSFORMERS_OFFLINE"] = "1"

    import torch
    from fastapi.testclient import TestClient
    from laya import Router
    from laya.common import serialize_state
    from laya.serve import create_app

    if args.device == "cuda":
        require(torch.cuda.is_available(), "GPU-required probe cannot fall back to CPU")
    router = Router(device=args.device, revision=args.revision, max_loaded=1)
    started = time.perf_counter()
    with warnings.catch_warnings(record=True) as load_warnings:
        warnings.simplefilter("always", RuntimeWarning)
        agent = router.load("english")
    load_seconds = time.perf_counter() - started
    require(str(agent.device).startswith(args.device), "unexpected effective device")
    require(agent.revision == args.revision, "unexpected loaded model revision")
    questions = {
        "boundary": {
            "type": "choice",
            "instructions": "Does this Go change affect an authorization boundary?",
            "criteria": {
                "review": "Changes permission checks, tenant isolation, or denial handling.",
                "routine": "Changes comments or presentation without changing access control.",
                "insufficient": "Required code or caller context is unavailable.",
            },
        },
        "priority": {
            "type": "score",
            "instructions": "How much authorization review does this change need?",
            "criteria": ["routine", "inspect", "urgent"],
        },
        "removed_check": {
            "type": "noul",
            "instructions": "Does the change remove an explicit permission check?",
            "criteria": {
                "true": "An explicit permission check present before is absent after.",
                "false": "No explicit permission check is removed by this change.",
            },
        },
    }
    samples = [
        {"id": "removed-check", "before": 'if !allowed(user, object) { return ErrDenied }; return read(object)',
         "after": 'return read(object)'},
        {"id": "comment-only", "before": '// Read returns the object.\nreturn read(object)',
         "after": '// Read returns the requested object.\nreturn read(object)'},
        {"id": "missing-helper", "before": 'return authorizeAndRead(user, object)',
         "after": 'return newRead(user, object)', "unavailable": "newRead implementation and caller context"},
    ]
    report = {
        "purpose": "synthetic runtime/HTTP-contract probe, not accuracy or calibration evidence",
        "offline": args.offline,
        "python": platform.python_version(),
        "versions": {name: importlib.metadata.version(name) for name in
                     ("laya", "torch", "transformers", "huggingface-hub", "fastapi", "httpx", "tokenizers")},
        "revision": agent.revision,
        "device": str(agent.device),
        "gpu": torch.cuda.get_device_name(0) if args.device == "cuda" else None,
        "load_seconds": load_seconds,
        "load_warnings": [str(w.message) for w in load_warnings],
        "probability_rounding_tolerance": "0.00005 * label_count + 1e-9; values retained without renormalization",
        "model_limits": {key: agent.cfg.get(key) for key in ("max_len", "head_max_len", "encoder")},
        "questions": questions,
        "samples": [],
        "checks": {},
    }
    headers = {"Authorization": "Bearer synthetic-probe-token"}
    with TestClient(create_app(router=router)) as client:
        request = {"model": "english", "state": samples[0], "questions": questions}
        require(client.post("/v1/systemone", json=request).status_code == 401, "missing auth accepted")
        report["checks"]["missing_auth"] = 401
        for name, payload, expected in (
            ("missing_questions", {"state": "example"}, 400),
            ("null_state", {"state": None, "questions": questions}, 400),
            ("invalid_token_budget", {**request, "max_len": 0}, 422),
            ("unknown_question_type", {**request, "questions": {"x": {"type": "invalid"}}}, 422),
        ):
            response = client.post("/v1/systemone", json=payload, headers=headers)
            require(response.status_code == expected, f"{name}: got {response.status_code}, expected {expected}")
            report["checks"][name] = response.status_code
        for sample in samples:
            timings = []
            outputs = []
            for _ in range(2):
                started = time.perf_counter()
                response = client.post("/v1/systemone", json={**request, "state": sample}, headers=headers)
                timings.append((time.perf_counter() - started) * 1000)
                require(response.status_code == 200, f"inference failed: HTTP {response.status_code}")
                body = response.json()
                require(set(body["answers"]) == set(questions), "answer IDs differ")
                answer = body["answers"]["boundary"]
                probabilities = answer["probabilities"]
                require(set(probabilities) == set(questions["boundary"]["criteria"]), "label set differs")
                require(all(math.isfinite(p) and 0 <= p <= 1 for p in probabilities.values()), "invalid probabilities")
                # Upstream rounds each probability to four decimal places.
                require(abs(sum(probabilities.values()) - 1) <= 0.00005 * len(probabilities) + 1e-9,
                        "probability mass outside upstream rounding tolerance")
                require(answer["choice"] in probabilities, "invalid choice")
                score = body["answers"]["priority"]
                require(score["type"] == "score" and math.isfinite(score["score"]) and 0 <= score["score"] <= 2,
                        "invalid ordinal score")
                require(score["legend"] == {str(i): label for i, label in enumerate(questions["priority"]["criteria"])},
                        "ordinal legend differs")
                score_probs = score["probabilities"]
                require(set(score_probs) == {"0", "1", "2"}, "ordinal distribution labels differ")
                require(all(math.isfinite(p) and 0 <= p <= 1 for p in score_probs.values()), "invalid ordinal probabilities")
                require(abs(sum(score_probs.values()) - 1) <= 0.00015 + 1e-9, "invalid ordinal mass")
                proposition = body["answers"]["removed_check"]
                require(proposition["type"] == "noul" and math.isfinite(proposition["noul"]) and 0 <= proposition["noul"] <= 1,
                        "invalid proposition probability")
                require(body["routing"]["model"] == "english", "unexpected routing")
                outputs.append(body)
            report["samples"].append({"input": sample, "elapsed_ms": timings, "outputs": outputs})
        # This is a compatibility hazard to document, not desired Shoal behavior.
        response = client.post("/v1/systemone", json={**request, "model": "unknown-shoal-model"}, headers=headers)
        require(response.status_code == 200, "upstream unknown-model behavior changed; reassess adapter")
        report["checks"]["unknown_model"] = {"status": response.status_code, "routing": response.json().get("routing")}
        long_state = "ordinary code and comments " * 300 + " authorization check removed at end"
        token_count = len(agent.tok(serialize_state(long_state), add_special_tokens=False)["input_ids"])
        response = client.post("/v1/systemone", json={**request, "state": long_state}, headers=headers)
        require(response.status_code == 200, f"upstream long-input behavior changed: HTTP {response.status_code}; reassess adapter")
        require(token_count > agent.cfg["max_len"], "long input did not exceed model limit")
        report["checks"]["oversized_state"] = {
            "raw_state_tokens": token_count,
            "status": response.status_code,
            "response": response.json(),
        }
        report["health"] = client.get("/health").json()
    snapshot = args.cache / "hub" / "models--convaiinnovations--laya" / "snapshots" / args.revision
    report["effective_artifacts_sha256"] = {
        str(path.relative_to(snapshot)): digest_file(path)
        for path in sorted(snapshot.rglob("*")) if path.is_file()
    }
    require(report["effective_artifacts_sha256"], "missing artifact manifest")
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(report, indent=2, sort_keys=True) + "\n")
    print(json.dumps({"output": str(args.output), "device": report["device"],
                      "revision": report["revision"], "samples": len(samples),
                      "checks": list(report["checks"])}))


if __name__ == "__main__":
    main()
