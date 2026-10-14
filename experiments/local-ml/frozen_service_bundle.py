#!/usr/bin/env python3
# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
"""Atomically register cached V9 code representations alongside numeric artifacts.

These bytes are the historical model's cached text, not complete repository files.
No historical observation or receipt timestamp is asserted by this export.
"""
import argparse
import json
import os
from pathlib import Path
import shutil
import tempfile

import frozen_code as fc

MAX_SOURCE_BYTES = 8 * 1024 * 1024


def source_records(evidence, numeric):
    """Bind the complete ordered cohort to the pinned input artifact."""
    inputs, raw = evidence['reserved/inputs-frozen.json']
    original_sha = fc.train.sha256(raw)
    provenance = numeric['provenance']
    if (provenance['archive_sha256'] != fc.ARCHIVE_SHA256
            or provenance['original_inputs_id'] != inputs['id']
            or provenance['original_members']['reserved/inputs-frozen.json']['sha256'] != original_sha):
        raise ValueError('numeric source provenance mismatch')
    rows = inputs['rows']
    ids = [row['unit_id'] for row in rows]
    if len(ids) != 188 or len(set(ids)) != 188 or [row['id'] for row in numeric['rows']] != ids:
        raise ValueError('numeric source cohort or order mismatch')
    total, records = 0, []
    for i, row in enumerate(rows):
        content = row['text']['code'].encode('utf-8')
        total += len(content)
        if len(content) > MAX_SOURCE_BYTES or total > MAX_SOURCE_BYTES:
            raise ValueError('cached sources exceed byte limit')
        if not isinstance(row['path'], str) or not row['path'] or type(row['pr']) is not int or row['pr'] <= 0:
            raise ValueError('invalid cached source metadata')
        records.append(({'id': row['unit_id'], 'source_file': f'sources/{i:04d}.txt',
                         'source_sha256': fc.train.sha256(content), 'bytes': len(content),
                         'path': row['path'], 'pr': row['pr'], 'representation': 'cached-v9-code-text'}, content))
    return original_sha, records


def export(archive_path, evidence_manifest_path, task_id, output_dir):
    output = Path(os.path.abspath(output_dir))
    if os.path.lexists(output):
        raise FileExistsError(output)
    if not output.parent.is_dir():
        raise ValueError('output parent must exist')
    evidence, _ = fc.read_evidence(archive_path, evidence_manifest_path)
    staging = Path(tempfile.mkdtemp(prefix='.frozen-service-', dir=output.parent))
    try:
        try:
            numeric = fc.export(archive_path, evidence_manifest_path, task_id, staging / 'numeric')
        except fc.train.PublishedDurabilityError as error:
            # The nested output is still private staging, not a published bundle.
            raise RuntimeError('numeric staging sync failed; bundle was not published') from error
        numeric_bytes = fc.bounded_read(staging / 'numeric/manifest.json', 1024 * 1024)
        if numeric_bytes != fc.dataset.canonical_bytes(numeric):
            raise ValueError('numeric manifest bytes mismatch')
        original_sha, records = source_records(evidence, numeric)
        manifest = {'schema': 1, 'kind': 'frozen-code-source-registration',
                    'numeric_manifest_sha256': fc.train.sha256(numeric_bytes),
                    'numeric_model_sha256': numeric['model_sha256'],
                    'archive_sha256': fc.ARCHIVE_SHA256, 'original_inputs_sha256': original_sha,
                    'builder_sha256': fc.train.sha256(Path(__file__).read_bytes()),
                    'rows': [record for record, _ in records],
                    'historical_timestamps_verified': False, 'optimization_enabled': False, 'action': 'full_review'}
        (staging / 'sources').mkdir()
        def write(name, content):
            with (staging / name).open('xb') as stream:
                stream.write(content)
                stream.flush()
                os.fsync(stream.fileno())
        for record, content in records:
            write(record['source_file'], content)
        manifest_bytes = fc.dataset.canonical_bytes(manifest)
        write('source-manifest.json', manifest_bytes)
        fc.train.fsync_directory(staging / 'sources')
        fc.train.fsync_directory(staging)
        fc.train.publish_exclusive(staging, output)
        try:
            fc.train.fsync_directory(output.parent)
        except OSError as error:
            raise fc.train.PublishedDurabilityError(output, numeric['model_sha256']) from error
    finally:
        if staging.exists():
            shutil.rmtree(staging)
    return {'model_sha256': numeric['model_sha256'], 'manifest_sha256': manifest['numeric_manifest_sha256'],
            'source_manifest_sha256': fc.train.sha256(manifest_bytes), 'rows': len(records),
            'optimization_enabled': False, 'action': 'full_review'}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--archive', type=Path, default=fc.HELPERS / 'runs/operations-v9/evidence.tar.gz')
    parser.add_argument('--evidence-manifest', type=Path, default=fc.HELPERS / 'runs/operations-v9/evidence-manifest.json')
    parser.add_argument('--task-id', required=True)
    parser.add_argument('--output-dir', required=True)
    args = parser.parse_args()
    try:
        result = export(args.archive, args.evidence_manifest, args.task_id, args.output_dir)
    except fc.train.PublishedDurabilityError as error:
        parser.exit(2, json.dumps({'status': 'published-durability-indeterminate',
                                  'output_dir': error.output_dir, 'model_sha256': error.model_sha256,
                                  'optimization_enabled': False}) + '\n')
    print(json.dumps(result))


if __name__ == '__main__':
    main()
