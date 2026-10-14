#!/usr/bin/env python3
# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
"""Run sealed source/review export, pinned training and durable CPU inquiry.

Synthetic conformance only. This measures pipeline behavior, not code-review
quality, population risk, LLM savings, or a production source authority.
"""
import argparse
import hashlib
import json
from pathlib import Path
import re
import subprocess
import sys
import time

from dataset import _stamp


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def command(args):
    return subprocess.check_output([str(arg) for arg in args], text=True).strip()


def validate_window(window):
    times = [_stamp(window[key], 'inventory window ' + key)
             for key in ('StartedAt', 'FirstPassFinishedAt',
                         'SecondPassStartedAt', 'CompletedAt')]
    if times != sorted(times):
        raise RuntimeError('invalid measured inventory window')


def run(binary, output):
    output.mkdir(mode=0o700)  # Exclusive; never replace earlier evidence.
    trainer = Path(__file__).resolve().with_name('train.py')
    results = []
    for task in ('source', 'review'):
        folder = output / task
        start = time.monotonic()
        # The trusted producer's stdout is the independent job input; do not
        # manufacture a trust pin by reading an otherwise untrusted manifest.
        manifest_pin = command([binary, 'prepare', '--task', task, '--out', folder])
        if not re.fullmatch('[0-9a-f]{64}', manifest_pin):
            raise RuntimeError('producer did not return a manifest pin')
        prepare_seconds = time.monotonic() - start
        fitted = []
        timings = []
        for name in ('candidate', 'candidate-repeat'):
            start = time.monotonic()
            receipt = json.loads(command([
                sys.executable, trainer, '--dataset', folder / 'dataset.json',
                '--authorized-manifest', folder / 'manifest.json',
                '--authorized-manifest-sha256', manifest_pin,
                '--output-dir', folder / name]))
            timings.append(time.monotonic() - start)
            fitted.append(receipt['model_sha256'])
        if fitted[0] != fitted[1]:
            raise RuntimeError('repeat fit changed the candidate model')
        model = folder / 'candidate' / 'model.json'
        if digest(model) != fitted[0]:
            raise RuntimeError('trainer output did not match the published model')
        inquiries = []
        for _ in range(2):
            inquiries.append(json.loads(command([
                binary, 'inquire', '--task', task, '--model', model,
                '--model-sha256', fitted[0], '--state-dir', folder / 'inquiry'])))
        if [row['provider_calls_this_process'] for row in inquiries] != [1, 0]:
            raise RuntimeError('restart did not replay without a model call')
        for key in ('request_id', 'receipt_id', 'prediction_id', 'label'):
            if inquiries[0][key] != inquiries[1][key]:
                raise RuntimeError('restart changed ' + key)
        if not all(row['replay_matched'] for row in inquiries):
            raise RuntimeError('immediate replay did not match')
        data = json.loads((folder / 'dataset.json').read_text())
        manifest = json.loads((folder / 'manifest.json').read_text())
        training = json.loads((folder / 'candidate' / 'training-receipt.json').read_text())
        capture_path = folder / 'cohort-capture.json'
        capture = json.loads(capture_path.read_text())
        if (capture.get('schema') != 1
                or capture.get('kind') != 'registered-inventory-export-capture'):
            raise RuntimeError('invalid inventory capture proof')
        initial, final = capture['initial'], capture['final']
        if (initial['VectorID'] != manifest['inventory_id']
                or final['VectorID'] != initial['VectorID']
                or initial['Pins'] != final['Pins']):
            raise RuntimeError('inventory changed across export')
        pins = {pin['TargetID']: pin for pin in initial['Pins']}
        if (len(pins) != len(initial['Pins']) or len(pins) != len(manifest['rows'])
                or any(row['target_id'] not in pins
                       or row['inventory_id'] != pins[row['target_id']]['SnapshotID']
                       for row in manifest['rows'])):
            raise RuntimeError('manifest lost target inventory bindings')
        for recorded in (initial, final):
            validate_window(recorded['Window'])
        if (training['authorized_export']['manifest_sha256'] != manifest_pin
                or training['authorized_export']['dataset_sha256'] != digest(folder / 'dataset.json')
                or training['model_sha256'] != fitted[0]):
            raise RuntimeError('training receipt lost the authorized export binding')
        # Keep the complete receipt in the output directory; the digest below
        # binds its provenance and recipe/runtime links into this run report.
        counts = {state: sum(row['label_status'] == state for row in data['rows'])
                  for state in ('verified', 'unknown', 'disputed')}
        eligible = sum(row['split'] == 'train' and row['label_status'] == 'verified'
                       and row['training_allowed'] for row in data['rows'])
        results.append({
            'task': task, 'task_id': data['task_id'], 'rows': len(data['rows']),
            'label_counts': counts, 'eligible_train_rows': eligible,
            'split_counts': {split: sum(row['split'] == split for row in data['rows'])
                             for split in ('train', 'calibration', 'validation', 'test')},
            'manifest_sha256': manifest_pin, 'dataset_sha256': digest(folder / 'dataset.json'),
            'cohort_sha256': manifest['cohort_sha256'], 'model_sha256': fitted[0],
            'training_receipt_sha256': digest(folder / 'candidate' / 'training-receipt.json'),
            'repeat_model_identical': True, 'prepare_seconds': prepare_seconds,
            'fit_seconds': timings, 'inquiries': inquiries,
            'inventory_vector_id': initial['VectorID'],
            'inventory_capture_sha256': digest(capture_path),
            'inventory_target_count': len(pins),
            'initial_inventory_window': initial['Window'],
            'final_inventory_window': final['Window'],
            'inventory_unchanged_during_export': True,
        })
    report = {'schema': 1, 'kind': 'adjudicated-training-conformance',
              'synthetic': True, 'population_quality_claim': False,
              'promotion_enabled': False, 'paid_inference_calls': 0, 'tasks': results}
    with (output / 'summary.json').open('x') as stream:
        json.dump(report, stream, indent=2, sort_keys=True)
        stream.write('\n')
    print(json.dumps(report, sort_keys=True))


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', required=True, type=Path)
    parser.add_argument('--output-dir', required=True, type=Path)
    args = parser.parse_args()
    run(args.binary.resolve(), args.output_dir.resolve())
