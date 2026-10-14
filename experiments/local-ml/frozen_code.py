#!/usr/bin/env python3
# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
"""Import the retained V9 SVM into numeric serving artifacts; never train or promote."""
import argparse
import io
import json
import math
import os
from pathlib import Path
import shutil
import sys
import tarfile
import tempfile

import train
import dataset
from threadpoolctl import threadpool_limits

ROOT = Path(__file__).resolve().parents[2]
HELPERS = ROOT / 'showcases/pr-triage'
sys.path.insert(0, str(HELPERS))
import operation_classifier as op
import context_classifier as cc
from shadow import digest

ARCHIVE_SHA256 = '1e6b0fcc6025912d1181602830a0632d72e179d4c055f7784faacc8b9a437f11'
MEMBERS = ('trained/model.json', 'reserved/inputs-frozen.json',
           'reserved/selection.json', 'reserved/predictions.json')
MAX_ARCHIVE = 16 * 1024 * 1024
MAX_TOTAL = 80 * 1024 * 1024
MAX_MEMBER = 32 * 1024 * 1024


def strict_json(raw):
    def pairs(items):
        result = {}
        for key, value in items:
            if key in result:
                raise ValueError('duplicate JSON key')
            result[key] = value
        return result
    return json.loads(raw.decode('utf-8'), object_pairs_hook=pairs,
                      parse_constant=lambda value: (_ for _ in ()).throw(ValueError('nonfinite JSON')))


def bounded_read(path, limit):
    with Path(path).open('rb') as stream:
        data = stream.read(limit + 1)
    if len(data) > limit:
        raise ValueError('file exceeds byte limit')
    return data


def read_evidence(archive_path, manifest_path):
    raw = bounded_read(archive_path, MAX_ARCHIVE)
    manifest = strict_json(bounded_read(manifest_path, 256 * 1024))
    if train.sha256(raw) != ARCHIVE_SHA256 or manifest['archive_sha256'] != ARCHIVE_SHA256:
        raise ValueError('frozen archive digest mismatch')
    specs = {}
    for entry in manifest['files']:
        name = entry['path']
        if name in specs or type(entry['bytes']) is not int or not 0 <= entry['bytes'] <= MAX_MEMBER:
            raise ValueError('invalid manifest member')
        specs[name] = entry
    if sum(entry['bytes'] for entry in specs.values()) > MAX_TOTAL:
        raise ValueError('archive expanded size exceeds limit')
    found, seen, total = {}, set(), 0
    with tarfile.open(fileobj=io.BytesIO(raw), mode='r:gz') as archive:
        for member in archive:
            if not member.isfile() or member.name in seen or member.name not in specs:
                raise ValueError('unexpected archive member or type')
            seen.add(member.name)
            spec = specs[member.name]
            total += member.size
            if member.size != spec['bytes'] or total > MAX_TOTAL:
                raise ValueError('archive member size mismatch')
            content = archive.extractfile(member).read(member.size + 1)
            if len(content) != member.size or train.sha256(content) != spec['sha256']:
                raise ValueError('archive member digest mismatch')
            if member.name in MEMBERS:
                value = strict_json(content)
                if value.get('id') != digest({k: v for k, v in value.items() if k != 'id'}):
                    raise ValueError('content identity mismatch')
                found[member.name] = (value, content)
    if seen != set(specs) or set(found) != set(MEMBERS):
        raise ValueError('missing archive members')
    return found, {name: specs[name] for name in MEMBERS}


def validate_bindings(model, inputs, selection, predictions):
    if model['recipe'] != ['code', 'linear_svm'] or model['sklearn_version'] != train.sklearn.__version__:
        raise ValueError('unsupported frozen model')
    threshold = model['threshold']
    if type(threshold) not in (int, float) or not 0 < threshold < 1:
        raise ValueError('invalid threshold')
    for record in (selection, predictions):
        if (record['model_id'] != model['id'] or record['inputs_id'] != inputs['id']
                or record['threshold'] != threshold):
            raise ValueError('frozen selection/prediction binding mismatch')
    if selection['labels_inspected'] is not False:
        raise ValueError('selection is not frozen before labels')
    if any(record['optimization_enabled'] is not False for record in (model, inputs, predictions)):
        raise ValueError('optimization must remain disabled')
    if model['evidence_contract'] != inputs['evidence_contract']:
        raise ValueError('evidence contract mismatch')
    expected = {'runner_sha256': 'operation_classifier.py', 'base_classifier_sha256': 'context_classifier.py',
                'candidate_helpers_sha256': 'candidates.py', 'source_helpers_sha256': 'shadow.py'}
    if any(train.sha256((HELPERS / file).read_bytes()) != model['evidence_contract'][key]
           for key, file in expected.items()):
        raise ValueError('frozen helper source mismatch')
    rows = inputs['rows']
    ids = [row['unit_id'] for row in rows]
    if len(rows) != 188 or len(set(ids)) != len(ids) or any(not isinstance(x, str) or not x for x in ids):
        raise ValueError('invalid subject identities')
    cached = predictions['predictions']
    if len(cached) != len(ids) or {r['unit_id'] for r in cached} != set(ids) or set(predictions['scores']) != set(ids):
        raise ValueError('prediction subjects mismatch')
    for row in cached:
        score = predictions['scores'][row['unit_id']]
        if (type(score) not in (int, float) or not math.isfinite(score) or not 0 <= score <= 1
                or row['review_score'] != score or row['action'] != 'full_review'
                or row['optimization_eligible'] is not False or row['disposition'] != 'predicted'
                or row['proposal'] != ('lower_priority' if score < threshold else 'retain')):
            raise ValueError('invalid frozen prediction')
    return threshold


def export(archive_path, evidence_manifest_path, task_id, output_dir):
    if not isinstance(task_id, str) or not task_id or len(task_id.encode('utf-8')) > 1024:
        raise ValueError('invalid task identity')
    output = Path(os.path.abspath(output_dir))
    if os.path.lexists(output):
        raise FileExistsError(output)
    if not output.parent.is_dir():
        raise ValueError('output parent must exist')
    evidence, members = read_evidence(archive_path, evidence_manifest_path)
    model, inputs, selection, predictions = [evidence[name][0] for name in MEMBERS]
    threshold = validate_bindings(model, inputs, selection, predictions)
    feature_schema = 'frozen-v9-code-tfidf-60000-v1'
    recipe = {'schema': 1, 'kind': 'frozen-v9-numeric-import', 'original_model_id': model['id'],
              'original_threshold': threshold, 'intercept_transform': 'original_intercept - log(threshold / (1-threshold))',
              'feature_transform': 'frozen mixed char-word TF-IDF', 'decision': 'margin >= 0 means retain',
              'training_performed': False, 'optimization_enabled': False}
    with threadpool_limits(limits=1):
        runtime = {**train.runtime_metadata(), 'kind': 'import-runtime',
                   'original_training_runtime_verified': False,
                   'note': 'training_runtime_sha256 binds this import runtime, not the unavailable original training runtime'}
        runtime['sources']['frozen_code.py'] = train.sha256(Path(__file__).read_bytes())
        vectorizer, estimator = op.restore(model)
        features = vectorizer.transform([row['text']['code'] for row in inputs['rows']])
        scores = op.score((vectorizer, estimator), inputs['rows'], model['recipe'])
    coefficients = estimator.coef_[0].tolist()
    intercept = float(estimator.intercept_[0]) - math.log(threshold / (1 - threshold))
    if len(coefficients) != 60000 or not all(math.isfinite(x) for x in coefficients) or not math.isfinite(intercept):
        raise ValueError('invalid linear parameters')
    rb, rt = dataset.canonical_bytes(recipe), dataset.canonical_bytes(runtime)
    serving_model = {'schema': 1, 'kind': 'linear-svm', 'task_id': task_id, 'question_id': 'priority',
                     'feature_schema_id': feature_schema, 'labels': ['lower_priority', 'retain'],
                     'dataset_sha256': train.sha256(evidence[MEMBERS[1]][1]), 'recipe_sha256': train.sha256(rb),
                     'training_runtime_sha256': train.sha256(rt), 'coefficients': coefficients,
                     'intercept': intercept, 'threshold': 0.0}
    mb = dataset.canonical_bytes(serving_model)
    if len(mb) > 4 * 1024 * 1024:
        raise ValueError('serving model exceeds limit')
    manifest = {'schema': 1, 'model_sha256': train.sha256(mb), 'task_id': task_id, 'question_id': 'priority',
                'feature_schema_id': feature_schema, 'archive_sha256': ARCHIVE_SHA256, 'original_members': members,
                'original_model_id': model['id'], 'original_inputs_id': inputs['id'], 'original_threshold': threshold,
                'original_prediction_id': predictions['id'], 'original_selection_id': selection['id'],
                'training_performed': False, 'original_training_runtime_verified': False,
                'runtime_binding': 'import runtime only; original training runtime unavailable',
                'optimization_enabled': False, 'action': 'full_review', 'rows': []}
    manifest = {**{key: manifest[key] for key in ('schema', 'model_sha256', 'task_id', 'question_id', 'feature_schema_id', 'rows')},
                'provenance': {key: value for key, value in manifest.items() if key not in ('schema', 'model_sha256', 'task_id', 'question_id', 'feature_schema_id', 'rows')}}
    staging = Path(tempfile.mkdtemp(prefix='.frozen-code-', dir=output.parent))
    def write(name, content):
        with (staging / name).open('xb') as stream:
            stream.write(content)
            stream.flush()
            os.fsync(stream.fileno())
    try:
        (staging / 'inputs').mkdir()
        for i, row in enumerate(inputs['rows']):
            uid = row['unit_id']
            original_score = predictions['scores'][uid]
            if scores[uid] != original_score:
                raise ValueError('original score replay mismatch')
            expected = 'lower_priority' if original_score < threshold else 'retain'
            dense = features.getrow(i).toarray()[0].tolist()
            if len(dense) != len(coefficients) or any(not math.isfinite(x) or abs(x) > 1e6 for x in dense):
                raise ValueError('invalid transformed features')
            margin = intercept
            for coefficient, value in zip(coefficients, dense):
                margin += coefficient * value
            if not math.isfinite(margin) or ('retain' if margin >= 0 else 'lower_priority') != expected:
                raise ValueError('threshold transformation changed a decision')
            content = dataset.canonical_bytes({'schema': 1, 'feature_schema_id': feature_schema,
                                              'subjects': [{'id': uid, 'features': dense}]})
            if len(content) > 8 * 1024 * 1024:
                raise ValueError('input exceeds serving limit')
            name = f'inputs/{i:04d}.json'
            write(name, content)
            manifest['rows'].append({'id': uid, 'input_file': name, 'input_sha256': train.sha256(content),
                                     'expected_label': expected, 'original_score': original_score})
        for name, content in [('model.json', mb), ('recipe.json', rb), ('runtime.json', rt),
                              ('manifest.json', dataset.canonical_bytes(manifest))]:
            write(name, content)
        train.fsync_directory(staging / 'inputs')
        train.fsync_directory(staging)
        train.publish_exclusive(staging, output)
        try:
            train.fsync_directory(output.parent)
        except OSError as error:
            raise train.PublishedDurabilityError(output, manifest['model_sha256']) from error
    finally:
        if staging.exists():
            shutil.rmtree(staging)
    return manifest


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--archive', type=Path, default=HELPERS / 'runs/operations-v9/evidence.tar.gz')
    parser.add_argument('--evidence-manifest', type=Path, default=HELPERS / 'runs/operations-v9/evidence-manifest.json')
    parser.add_argument('--task-id', required=True)
    parser.add_argument('--output-dir', required=True)
    args = parser.parse_args()
    try:
        result = export(args.archive, args.evidence_manifest, args.task_id, args.output_dir)
    except train.PublishedDurabilityError as error:
        parser.exit(2, json.dumps({'status': 'published-durability-indeterminate',
                                  'output_dir': error.output_dir, 'model_sha256': error.model_sha256,
                                  'optimization_enabled': False}) + '\n')
    print(json.dumps({'model_sha256': result['model_sha256'],
                      'manifest_sha256': train.sha256(dataset.canonical_bytes(result)), 'rows': len(result['rows']),
                      'optimization_enabled': False, 'action': 'full_review'}))


if __name__ == '__main__':
    main()
