#!/usr/bin/env python3
# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
"""Bounded retrospective shadow pilot. Local artifacts are not Shoal API receipts."""
import argparse
import base64
from collections import Counter
import hashlib
import importlib.metadata
import json
import math
import os
from pathlib import Path
import re
import subprocess
import time
import warnings

HERE = Path(__file__).resolve().parent


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False)


def digest(value):
    return hashlib.sha256(value if isinstance(value, bytes) else canonical(value).encode()).hexdigest()


def write_new(path, value):
    """Never overwrite an earlier experiment observation."""
    with path.open("x") as stream:
        stream.write(json.dumps(value, indent=2, ensure_ascii=False, sort_keys=True) + "\n")


def git(repo, *args):
    return subprocess.run(["git", "-C", str(repo), *args], check=True, capture_output=True).stdout


def changed_paths(raw):
    parts = raw.split(b"\0")
    items = []
    i = 0
    while i < len(parts) and parts[i]:
        status = parts[i].decode("ascii"); i += 1
        old = parts[i]; i += 1
        new = old
        if status[0] in "RC":
            new = parts[i]; i += 1
        item = {"status": status}
        encoded = {}
        for field, value in (("before_path", None if status == "A" else old),
                             ("after_path", None if status == "D" else new)):
            try:
                item[field] = value.decode("utf-8") if value is not None else None
            except UnicodeDecodeError:
                encoded[field] = base64.b64encode(value).decode("ascii")
                item[field] = "[non-UTF-8 path: " + encoded[field] + "]"
        if encoded:
            item["non_utf8_paths_base64"] = encoded
        items.append(item)
    return items


def source(repo, revision, path, maximum):
    if path is None:
        return {"text": "", "disposition": "absent", "sha256": digest(b""), "bytes": 0}
    object_name = revision + ":" + path
    kind = git(repo, "cat-file", "-t", object_name).decode().strip()
    if kind != "blob":
        return {"disposition": "unsupported_object", "kind": kind}
    size = int(git(repo, "cat-file", "-s", object_name))
    if size > maximum:
        return {"disposition": "file_byte_limit", "bytes": size}
    content = git(repo, "cat-file", "blob", object_name)
    result = {"bytes": len(content), "sha256": digest(content)}
    try:
        result.update(text=content.decode("utf-8"), disposition="available")
    except UnicodeDecodeError:
        result["disposition"] = "non_utf8"
    return result


def parsed(extractor, path, src):
    if src["disposition"] == "absent":
        return {"declarations": [], "residue": ""}
    proc = subprocess.run([str(extractor)], input=json.dumps({"path": path, "content": src["text"]}),
                          text=True, capture_output=True, check=True)
    return json.loads(proc.stdout)


def pair_declarations(before, after):
    b = {d["key"]: d for d in before["declarations"]}
    a = {d["key"]: d for d in after["declarations"]}
    if len(b) != len(before["declarations"]) or len(a) != len(after["declarations"]):
        raise ValueError("duplicate declaration key")
    pairs = []
    # Ordinals distinguish legal repeated init declarations within one parse, but
    # are not identities across revisions. Cancel identical bodies first.
    base_key = lambda key: re.sub(r"#\d+$", "", key)
    for key in sorted({base_key(k) for k in b.keys() | a.keys()}):
        lefts = [d for k, d in b.items() if base_key(k) == key]
        rights = [d for k, d in a.items() if base_key(k) == key]
        duplicate = len(lefts) > 1 or len(rights) > 1
        unmatched = []
        for left in lefts:
            match = next((i for i, right in enumerate(rights) if left["text"] == right["text"]), None)
            if match is None:
                unmatched.append(left)
            else:
                rights.pop(match)
        for index in range(max(len(unmatched), len(rights))):
            left = unmatched[index] if index < len(unmatched) else None
            right = rights[index] if index < len(rights) else None
            symbol = key if index == 0 else f"{key}#{index + 1}"
            pair = {"symbol": symbol, "kind": (right or left)["kind"], "before": left, "after": right}
            if duplicate:
                pair["pairing"] = "unchanged_duplicates_by_content_then_remaining_source_order"
            pairs.append(pair)
    # Unchanged initializers can still change behavior when reordered. Compare
    # relative order of retained bodies, so a new init does not relabel every
    # existing one, while swapping registration and serving remains visible.
    for category in ("init", "var"):
        def initializers(document):
            return [(base_key(d["key"]), digest(d["text"])) for d in document["declarations"]
                    if (base_key(d["key"]) == "function:init" if category == "init" else d["kind"] == "var")]
        left_order, right_order = initializers(before), initializers(after)
        common = Counter(left_order) & Counter(right_order)
        def retained(order):
            remaining = common.copy()
            result = []
            for identity in order:
                if remaining[identity]:
                    result.append(identity)
                    remaining[identity] -= 1
            return result
        if retained(left_order) != retained(right_order):
            pairs.append({"symbol": category + "_initialization_order", "kind": "file_context",
                          "before": None, "after": None, "before_order": left_order, "after_order": right_order})
    if before["residue"] != after["residue"]:
        pairs.append({"symbol": "file_context", "kind": "file_context", "before": None, "after": None})
    return pairs


def collect(args):
    protocol_bytes = args.protocol.read_bytes()
    protocol = json.loads(protocol_bytes)
    if args.output.exists():
        raise ValueError("output directory already exists; use a new observation directory")
    anchor = protocol["source_anchor"]
    if not re.fullmatch(r"[0-9a-f]{7,40}", anchor):
        raise ValueError("source anchor must be a commit hash")
    resolved = git(args.repo, "rev-parse", "--verify", anchor + "^{commit}").decode().strip()
    selected = []
    if "snapshots" in protocol:
        for snapshot in protocol["snapshots"]:
            if not isinstance(snapshot["pr"], int) or snapshot["pr"] <= 0:
                raise ValueError("invalid PR number")
            revisions = []
            for field in ("head", "base"):
                revision = snapshot[field]
                if not re.fullmatch(r"[0-9a-f]{40}", revision):
                    raise ValueError("snapshot revisions must be full commit hashes")
                revisions.append(git(args.repo, "rev-parse", "--verify", revision + "^{commit}").decode().strip())
            selected.append((revisions[0], snapshot["pr"], revisions[1]))
        if len({pr for _, pr, _ in selected}) != len(selected):
            raise ValueError("duplicate PR snapshot")
    else:
        commits = git(args.repo, "log", resolved, "--first-parent", "--format=%H%x09%s", "-40").decode().splitlines()
        for row in commits:
            sha, title = row.split("\t", 1)
            match = re.search(r"\(#(\d+)\)$", title)
            if match:
                selected.append((sha, int(match.group(1)), git(args.repo, "rev-parse", sha + "^").decode().strip()))
            if len(selected) == protocol["cohort_size"]:
                break
    if len(selected) != protocol["cohort_size"]:
        raise ValueError("cohort size mismatch")
    args.output.mkdir(parents=True)
    (args.output / "review-packets").mkdir()
    write_new(args.output / "protocol.json", protocol)
    cases = []
    for head, pr, base in selected:
        paths = changed_paths(git(args.repo, "diff", "--no-ext-diff", "--name-status", "-z", "-M", base, head))
        case = {"pr": pr, "base": base, "head": head, "files": [], "units": []}
        for index, item in enumerate(paths):
            name = item["after_path"] or item["before_path"]
            file_record = {**item, "path": name}
            case["files"].append(file_record)
            if item.get("non_utf8_paths_base64"):
                file_record["disposition"] = "non_utf8_path"
            elif index >= protocol["limits"]["max_files_per_pr"]:
                file_record["disposition"] = "file_count_limit"
            elif not name.endswith(".go"):
                file_record["disposition"] = "non_go"
            else:
                srcs = [source(args.repo, rev, path, protocol["limits"]["max_file_bytes"])
                        for rev, path in ((base, item["before_path"]), (head, item["after_path"]))]
                file_record["sources"] = [{k: v for k, v in src.items() if k != "text"} for src in srcs]
                if any(src["disposition"] not in ("available", "absent") for src in srcs):
                    file_record["disposition"] = "source_unavailable"
                elif any(re.search(r"(?m)^// Code generated .* DO NOT EDIT\.", src.get("text", "")) for src in srcs):
                    file_record["disposition"] = "generated_go"
                else:
                    parses = [parsed(args.extractor, name, src) for src in srcs]
                    errors = [parsed_src["error"] for parsed_src in parses if parsed_src.get("error")]
                    if errors:
                        file_record.update(disposition="parse_error", errors=errors)
                    else:
                        file_record["disposition"] = "parsed_go"
                        pairs = pair_declarations(*parses)
                        if item["before_path"] and item["after_path"] and item["before_path"] != item["after_path"]:
                            pairs.append({"symbol": "file_path", "kind": "file_context", "before": None, "after": None})
                        for pair in pairs:
                            unit = {**pair, "path": name, "pr": pr, "base": base, "head": head,
                                    "before_path": item["before_path"], "after_path": item["after_path"],
                                    "optimization_eligible": False, "context": "syntax_only_no_dependency_bodies"}
                            unit["id"] = digest(unit)
                            case["units"].append(unit)
            if file_record["disposition"] != "parsed_go":
                unit = {"path": name, "pr": pr, "base": base, "head": head, "symbol": "file",
                        "kind": file_record["disposition"], "before": None, "after": None,
                        "optimization_eligible": False, "context": "unsupported_file"}
                if item.get("non_utf8_paths_base64"):
                    unit["non_utf8_paths_base64"] = item["non_utf8_paths_base64"]
                unit["id"] = digest(unit)
                case["units"].append(unit)
        patch = git(args.repo, "diff", "--no-ext-diff", "--no-textconv", "--unified=5", base, head)
        case["patch_sha256"] = digest(patch)
        packet_path = args.output / "review-packets" / f"pr-{pr}.json"
        try:
            patch_text = patch.decode("utf-8")
        except UnicodeDecodeError:
            patch_text = None
        if len(patch) > protocol["limits"]["max_review_packet_bytes"]:
            case["review_packet"] = {"disposition": "packet_byte_limit", "bytes": len(patch)}
        elif patch_text is None:
            case["review_packet"] = {"disposition": "non_utf8_patch", "bytes": len(patch), "sha256": digest(patch)}
        else:
            packet = {"pr": pr, "base": base, "head": head, "diff": patch_text,
                      "units": [{k: u[k] for k in ("id", "path", "symbol", "kind")} for u in case["units"]],
                      "limitations": protocol["context_limitations"]}
            write_new(packet_path, packet)
            case["review_packet"] = {"disposition": "available", "bytes": len(patch), "sha256": digest(packet)}
        case["id"] = digest(case)
        cases.append(case)
    manifest = {"schema": 1, "protocol_sha256": digest(protocol), "source_anchor": resolved,
                "mode": protocol["mode"], "observed_at_unix": time.time(), "cases": cases,
                "extractor_sha256": digest(args.extractor.read_bytes()),
                "collector_sha256": digest(Path(__file__).read_bytes()), "optimization_enabled": False}
    manifest["id"] = digest(manifest)
    write_new(args.output / "manifest.json", manifest)
    print(json.dumps({"manifest": manifest["id"], "prs": [c["pr"] for c in cases],
                      "files": sum(len(c["files"]) for c in cases), "units": sum(len(c["units"]) for c in cases)}))


def exact_fit(agent, state, question, max_len, head_max_len):
    """Pinned upstream encoding; check state AND instruction/option truncation."""
    from laya.common import build_sequence, encode_text, render_options, serialize_state
    internal = agent._to_internal(question)
    opts = render_options(internal)
    option_tokens = [encode_text(agent.tok, " " + opt.replace(agent.tok.mask_token, " "),
                                 add_special_tokens=False)["input_ids"] for opt in opts]
    instruction_tokens = encode_text(agent.tok, internal["t"] + " question: " + internal["ins"].replace(agent.tok.mask_token, " "),
                                     add_special_tokens=False)["input_ids"]
    header_room = head_max_len - sum(1 + len(ids) for ids in option_tokens)
    if any(len(ids) > 48 for ids in option_tokens) or header_room < 16 or len(instruction_tokens) > max(8, header_room):
        raise ValueError("task question/options would be truncated")
    empty, markers, stats = build_sequence(agent.tok, "", internal, max_len, head_max_len,
                                           state_ids=[], return_stats=True)
    if stats["options_distinct"] != len(opts) or len(markers) != len(opts):
        raise ValueError("task option encoding collapsed")
    state_tokens = encode_text(agent.tok, serialize_state(state).replace(agent.tok.mask_token, " "),
                               add_special_tokens=False)["input_ids"]
    full_length = len(state_tokens) + len(empty)
    return {"state_tokens": len(state_tokens), "header_tokens": len(empty),
            "full_tokens": full_length, "limit": max_len, "fits": full_length <= max_len}


def validate_answer(result, criteria):
    if set(result.get("answers", {})) != {"boundary"}:
        raise ValueError("unexpected question IDs")
    answer = result["answers"]["boundary"]
    probs = answer.get("probabilities", {})
    if answer.get("type") != "choice" or set(probs) != set(criteria) or answer.get("choice") not in criteria:
        raise ValueError("unexpected labels/type")
    if any(isinstance(p, bool) or not isinstance(p, (int,float)) or not math.isfinite(p) or not 0 <= p <= 1 for p in probs.values()):
        raise ValueError("invalid probabilities")
    if abs(sum(probs.values()) - 1) > 0.00005 * len(probs) + 1e-9:
        raise ValueError("invalid probability mass")
    return answer


def predict(args):
    protocol = json.loads((args.run / "protocol.json").read_text())
    manifest = json.loads((args.run / "manifest.json").read_text())
    if digest(protocol) != manifest["protocol_sha256"]:
        raise ValueError("protocol changed after collection")
    if digest({k:v for k,v in manifest.items() if k != "id"}) != manifest["id"]:
        raise ValueError("manifest content changed")
    if (args.run / "predictions.json").exists():
        raise ValueError("predictions already recorded")
    os.environ.update(HF_HOME=str(args.cache), HF_HUB_OFFLINE="1", TRANSFORMERS_OFFLINE="1",
                      HF_HUB_DISABLE_TELEMETRY="1", TOKENIZERS_PARALLELISM="false", USE_TF="0")
    import torch
    from laya import Router
    if importlib.metadata.version("laya") != protocol["model"]["runtime"]:
        raise ValueError("wrong Laya runtime")
    if not torch.cuda.is_available():
        raise ValueError("GPU required; no fallback")
    started = time.perf_counter()
    with warnings.catch_warnings(record=True) as captured:
        warnings.simplefilter("always", RuntimeWarning)
        router = Router(device="cuda", revision=protocol["model"]["revision"], max_loaded=1)
        agent = router.load("english")
    if agent.revision != protocol["model"]["revision"] or str(agent.device) != "cuda":
        raise ValueError("runtime identity mismatch")
    report = {"manifest_id": manifest["id"], "protocol_sha256": manifest["protocol_sha256"],
              "mode": protocol["mode"], "optimization_enabled": False,
              "startup_seconds": time.perf_counter()-started, "model_revision": agent.revision,
              "runtime_versions": {x: importlib.metadata.version(x) for x in ("laya","torch","transformers","tokenizers")},
              "device": str(agent.device), "warnings": [str(w.message) for w in captured], "predictions": []}
    for case in manifest["cases"]:
        for unit in case["units"]:
            item = {"unit_id": unit["id"], "pr": case["pr"], "action": "full_review", "optimization_eligible": False}
            terms = protocol["baseline_terms"]
            searchable = unit["path"] + " " + " ".join((unit[side] or {}).get("text", "") for side in ("before", "after"))
            item["baseline"] = "review" if any(term in searchable.lower() for term in terms) else "routine"
            if unit["kind"] != "function":
                item["disposition"] = "unsupported_unit"
            else:
                state = {"path": unit["path"], "declaration": unit["symbol"],
                         "before": (unit["before"] or {}).get("text", ""),
                         "after": (unit["after"] or {}).get("text", ""),
                         "context": "Dependency bodies and caller context are not supplied."}
                item["state_sha256"] = digest(state)
                budget = exact_fit(agent, state, protocol["question"], protocol["model"]["max_len"], protocol["model"]["head_max_len"])
                item["token_preflight"] = budget
                if not budget["fits"]:
                    item["disposition"] = "token_limit"
                else:
                    start = time.perf_counter()
                    result = agent.predict(state, {"boundary": protocol["question"]},
                                           max_len=protocol["model"]["max_len"], head_max_len=protocol["model"]["head_max_len"])
                    item["elapsed_ms"] = (time.perf_counter()-start)*1000
                    if str(agent.device) != "cuda" or getattr(agent, "cpu_fallback_count", 0):
                        raise ValueError("inference used CPU fallback")
                    item["answer"] = validate_answer(result, protocol["question"]["criteria"])
                    item["usage"] = result.get("usage")
                    item["disposition"] = "predicted"
            report["predictions"].append(item)
    report["id"] = digest(report)
    write_new(args.run / "predictions.json", report)
    print(json.dumps({"predictions_id": report["id"], "dispositions": dict(Counter(p["disposition"] for p in report["predictions"]))}))


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    commands=parser.add_subparsers(dest="command",required=True)
    collect_parser=commands.add_parser("collect")
    collect_parser.add_argument("--repo",type=Path,required=True)
    collect_parser.add_argument("--extractor",type=Path,required=True)
    collect_parser.add_argument("--protocol",type=Path,default=HERE/"protocol.json")
    collect_parser.add_argument("--output",type=Path,required=True)
    collect_parser.set_defaults(func=collect)
    predict_parser=commands.add_parser("predict")
    predict_parser.add_argument("--run",type=Path,required=True)
    predict_parser.add_argument("--cache",type=Path,required=True)
    predict_parser.set_defaults(func=predict)
    args=parser.parse_args();args.func(args)


if __name__ == "__main__":
    main()
