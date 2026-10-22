#!/usr/bin/env python3
# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
"""Run one reference distillation job and write the records the Go contract
tests read (internal/distillation/testdata). The manifest is written without
a trailing newline so its raw bytes are its pin, exactly as the trainer
requires. Usage: distill_fixture.py OUTPUT_DIR
"""
import hashlib
import json
import shutil
import sys
from pathlib import Path

import distill
import seal_candidate
from test_distill import export_fixture, manifest_fixture


def main(argv):
    out = Path(argv[1])
    out.mkdir(parents=True, exist_ok=True)
    data, export_manifest = export_fixture()
    (out / 'dataset.json').write_bytes(data)
    (out / 'export-manifest.json').write_bytes(export_manifest)
    export_sha = hashlib.sha256(export_manifest).hexdigest()
    manifest = manifest_fixture(export_sha, held_out=['row-99'])
    raw = distill.canonical(manifest).encode()
    pin = hashlib.sha256(raw).hexdigest()
    (out / 'python-manifest.json').write_bytes(raw)
    receipt = distill.distill(str(out / 'python-manifest.json'), pin,
                              {'cohort-1': (str(out / 'dataset.json'), str(out / 'export-manifest.json'))},
                              out / 'job', executor=dict(id='worker-1', attestation_id='attest:1', host='fixture-host'))
    (out / 'python-receipt.json').write_bytes(distill.canonical(receipt).encode())
    (out / 'python-predictions.json').write_bytes((out / 'job' / 'teacher-predictions.json').read_bytes())
    rows = json.loads(data)['rows']
    descriptor = dict(cohort_id='cohort-1', task_id='synthetic-task', label_policy_id='policy', manifest_sha256=export_sha,
                      example_ids=sorted(r['id'] for r in rows if r['split'] == 'train'))
    (out / 'python-export.json').write_bytes(distill.canonical(descriptor).encode())
    # The sealing contract: the same run's outputs plus a predeclared policy
    # must seal to the same candidate, evaluation, and artifact in Go.
    (out / 'python-dataset.json').write_bytes(data)
    (out / 'python-export-manifest.json').write_bytes(export_manifest)
    bundle_out = out / 'python-bundle'
    if bundle_out.exists():
        shutil.rmtree(bundle_out)
    shutil.copytree(out / 'job' / 'bundle', bundle_out)
    # The policy is the sealed record the host declares for the job before
    # its receipt; the fixture seals it the same way.
    policy = seal_candidate.seal_policy(dict(name='fixture-evaluation-policy', split='test', positive_label='yes',
                                             minimum_recall=0.5, minimum_specificity=0.5, minimum_positives=1, minimum_families=1))
    (out / 'python-policy.json').write_bytes(distill.canonical(policy).encode())
    records, _ = seal_candidate.build_candidate_set(manifest, pin, receipt, json.loads((out / 'python-predictions.json').read_bytes()),
                                                    bundle_out, [(export_manifest, data)], policy)
    (out / 'python-candidate-set.json').write_bytes(distill.canonical(records).encode())
    shutil.rmtree(out / 'job')
    print(json.dumps(dict(pin=pin, bundle_digest=receipt['bundle_digest'], disposition=records['evaluation']['disposition']), sort_keys=True))


if __name__ == '__main__':
    main(sys.argv)
