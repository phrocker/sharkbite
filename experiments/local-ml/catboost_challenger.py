#!/usr/bin/env python3
"""Train/replay the pinned structured CatBoost challenger.

The caller supplies only a pre-registered challenger manifest and rows from
the declared training split. This module never selects a threshold, promotes
an artifact, or changes a serving pointer.
"""
import hashlib
import json
import math
from pathlib import Path

import challenger_manifest as contract


def _catboost():
    try:
        from catboost import CatBoostClassifier, __version__
    except ImportError as error:
        raise RuntimeError('CatBoost dependency is unavailable; install requirements-catboost.txt') from error
    return CatBoostClassifier, __version__


def _recipe(manifest):
    matches = [recipe for recipe in manifest['recipes'] if recipe['name'] == 'catboost-structured-v1']
    if len(matches) != 1:
        raise ValueError('CatBoost recipe is not uniquely registered')
    return matches[0]


def _rows(rows, manifest, training):
    if not isinstance(rows, list) or not rows:
        raise ValueError('training rows are required')
    columns = manifest['feature_names']
    recipe = _recipe(manifest)
    categorical = set(recipe['categorical_features'])
    if not categorical <= set(columns):
        raise ValueError('categorical feature is outside manifest')
    values, labels, ids = [], [], []
    for row in rows:
        if training and (row.get('split') != 'train' or row.get('label') not in (0, 1, False, True)):
            raise ValueError('rows must be labeled training examples')
        if not isinstance(row.get('id'), str) or not row['id'] or row['id'] in ids:
            raise ValueError('invalid or duplicate training row id')
        features = row.get('features')
        if not isinstance(features, dict) or set(features) != set(columns):
            raise ValueError('feature schema mismatch')
        encoded = []
        for column in columns:
            value = features[column]
            if column in categorical:
                if not isinstance(value, str) or not value:
                    raise ValueError('categorical feature must be nonempty text')
                encoded.append(value)
            else:
                if isinstance(value, bool) or not isinstance(value, (int, float)) or not math.isfinite(float(value)):
                    raise ValueError('numeric feature is not finite')
                encoded.append(float(value))
        ids.append(row['id']); values.append(encoded)
        if training:
            labels.append(int(row['label']))
    if training and len(set(labels)) != 2:
        raise ValueError('training rows need both classes')
    return values, labels, ids, [columns.index(name) for name in categorical]


def train(rows, manifest, output_path):
    contract.validate(manifest)
    values, labels, ids, categorical_indices = _rows(rows, manifest, training=True)
    CatBoostClassifier, version = _catboost()
    if version != _recipe(manifest).get('library_version'):
        raise ValueError('CatBoost runtime version does not match manifest')
    model = CatBoostClassifier(iterations=32, depth=4, learning_rate=0.1, loss_function='Logloss',
                               random_seed=0, thread_count=1, verbose=False, allow_writing_files=False)
    model.fit(values, labels, cat_features=categorical_indices)
    output = Path(output_path)
    model.save_model(str(output), format='json')
    digest = hashlib.sha256(output.read_bytes()).hexdigest()
    recipe = _recipe(manifest)
    return contract.seal('catboost_candidate_artifact', manifest_id=manifest['id'], task_id=manifest['task_id'],
                         dataset_id=manifest['dataset_id'], model_digest=digest, runtime_id=recipe['runtime_id'],
                         library_version=version, feature_schema_id=manifest['feature_schema_id'],
                         categorical_features=recipe['categorical_features'], training_row_ids=sorted(ids),
                         optimization_enabled=False)


def predict(rows, manifest, artifact, model_path):
    contract.validate(manifest)
    if artifact.get('kind') != 'catboost_candidate_artifact' or artifact.get('manifest_id') != manifest['id']:
        raise ValueError('CatBoost artifact/manifest mismatch')
    artifact_body = {key: value for key, value in artifact.items() if key != 'id'}
    if artifact.get('id') != contract._digest(artifact_body):
        raise ValueError('CatBoost artifact identity mismatch')
    path = Path(model_path)
    if hashlib.sha256(path.read_bytes()).hexdigest() != artifact.get('model_digest'):
        raise ValueError('CatBoost artifact digest mismatch')
    values, _, ids, categorical_indices = _rows(rows, manifest, training=False)
    CatBoostClassifier, version = _catboost()
    if version != artifact.get('library_version'):
        raise ValueError('CatBoost runtime version mismatch')
    model = CatBoostClassifier()
    model.load_model(str(path), format='json')
    scores = model.predict_proba(values)[:, 1]
    if len(scores) != len(ids) or any(not math.isfinite(float(score)) or not 0 <= float(score) <= 1 for score in scores):
        raise ValueError('invalid CatBoost scores')
    return {'artifact_id': artifact['id'], 'scores': {row_id: float(score) for row_id, score in zip(ids, scores)},
            'optimization_enabled': False}


def main():
    import argparse
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--manifest', type=Path, required=True)
    parser.add_argument('--rows', type=Path, required=True)
    parser.add_argument('--model', type=Path, required=True)
    args = parser.parse_args()
    manifest = json.loads(args.manifest.read_text())
    rows = json.loads(args.rows.read_text())
    artifact = train(rows, manifest, args.model)
    args.model.with_suffix('.artifact.json').write_text(json.dumps(artifact, indent=2) + '\n')


if __name__ == '__main__':
    main()
