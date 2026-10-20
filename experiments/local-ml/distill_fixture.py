#!/usr/bin/env python3
# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
"""Run one reference distillation job and write the records the Go contract
tests read (internal/distillation/testdata). The manifest is written without
a trailing newline so its raw bytes are its pin, exactly as the trainer
requires. Usage: distill_fixture.py OUTPUT_DIR
"""
import hashlib
import json
import sys
from pathlib import Path

import distill
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
    print(json.dumps(dict(pin=pin, bundle_digest=receipt['bundle_digest']), sort_keys=True))


if __name__ == '__main__':
    main(sys.argv)
