#!/usr/bin/env python3
"""Verify a poisoning-corpus receipt without rerunning its attacks.

Verification checks the sealed report identity, attack order, denominators and
the fail-closed result. It does not turn structural evidence into a model
quality, truth, influence or unlearning claim.
"""
import argparse
import json
from pathlib import Path

import learning as l
import poisoning


def verify(report):
    if not isinstance(report, dict):
        raise ValueError("report must be an object")
    l.verify(report, "poisoning_attack_report")
    if report.get("corpus_id") != poisoning.CORPUS_ID or report.get("corpus_version") != poisoning.CORPUS_VERSION:
        raise ValueError("unsupported corpus identity")
    attacks = report.get("attacks")
    if not isinstance(attacks, list) or len(attacks) != len(poisoning.ATTACKS):
        raise ValueError("attack set is incomplete")
    for row, expected in zip(attacks, poisoning.ATTACKS):
        if not isinstance(row, dict):
            raise ValueError("attack row must be an object")
        name, description, disposition = expected
        if (row.get("name"), row.get("description"), row.get("expected")) != (name, description, disposition):
            raise ValueError("attack identity changed")
        if row.get("status") != ("quarantined" if disposition == "quarantine" else "rejected"):
            raise ValueError("attack did not fail closed")
    clean = report.get("clean_control")
    if not isinstance(clean, dict) or clean.get("status") != "accepted" or not clean.get("scenario_id"):
        raise ValueError("clean control is not accepted")
    expected_counts = {
        "attacks": len(attacks),
        "rejected": sum(row["status"] == "rejected" for row in attacks),
        "quarantined": sum(row["status"] == "quarantined" for row in attacks),
        "unsafe": 0,
        "failed": 0,
    }
    if report.get("denominators") != expected_counts:
        raise ValueError("denominators do not match attack rows")
    limitation = report.get("limitation")
    if not isinstance(limitation, str) or "structural" not in limitation.lower():
        raise ValueError("report must retain structural-evidence limitation")
    # Re-sealing catches edits to fields excluded from the checks above while
    # remaining independent of the report's supplied id.
    resealed = l.seal("poisoning_attack_report", **{
        key: value for key, value in report.items() if key not in ("id", "kind", "schema")
    })
    if resealed["id"] != report.get("id"):
        raise ValueError("report digest mismatch")
    return {"id": report["id"], "corpus_id": poisoning.CORPUS_ID, "verified": True}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("report", type=Path)
    args = parser.parse_args()
    with args.report.open() as source:
        report = json.load(source)
    print(json.dumps(verify(report), sort_keys=True))


if __name__ == "__main__":
    main()
