#!/usr/bin/env python3
"""Deterministic validation-only temperature calibration artifacts."""
import hashlib
import json
import math


SCHEMA = 1
MAX_ROWS = 1_000_000


def _digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(',', ':'), allow_nan=False).encode()).hexdigest()


def seal(kind, **fields):
    body = dict(schema=SCHEMA, kind=kind, **fields)
    return dict(body, id=_digest(body))


def _finite(value, name, low=0.0, high=1.0):
    if type(value) not in (int, float) or not math.isfinite(value) or not low <= value <= high:
        raise ValueError('invalid ' + name)


def _ids(ids, name):
    if not isinstance(ids, list) or not ids or len(ids) > MAX_ROWS or any(not isinstance(row_id, str) or not row_id for row_id in ids):
        raise ValueError('invalid ' + name)
    if ids != sorted(ids) or len(set(ids)) != len(ids):
        raise ValueError(name + ' must be sorted and unique')


def _text(value, name):
    if not isinstance(value, str) or not value or len(value) > 512:
        raise ValueError('invalid ' + name)


def _digest_id(value, name):
    if not isinstance(value, str) or len(value) != 64 or any(char not in '0123456789abcdef' for char in value):
        raise ValueError('invalid ' + name)


def verify(artifact):
    if not isinstance(artifact, dict) or artifact.get('schema') != SCHEMA or artifact.get('kind') != 'calibration_artifact':
        raise ValueError('invalid calibration artifact')
    body = {key: value for key, value in artifact.items() if key != 'id'}
    if artifact.get('id') != _digest(body):
        raise ValueError('calibration artifact identity mismatch')
    _ids(artifact.get('validation_ids'), 'validation_ids')
    _finite(artifact.get('temperature'), 'temperature', .05, 20.)
    if artifact.get('method') != 'temperature-v1' or artifact.get('optimization_enabled') is not False:
        raise ValueError('unsupported calibration artifact')
    for key in ('model_id', 'runtime_id', 'validation_manifest_id'):
        _text(artifact.get(key), key)
    for key in ('validation_scores_digest', 'validation_labels_digest'):
        _digest_id(artifact.get(key), key)
    _text(artifact.get('limitation'), 'limitation')
    if 'validation-only' not in artifact['limitation'].lower() or 'promotion' not in artifact['limitation'].lower():
        raise ValueError('calibration limitation is missing')
    return artifact


def _logit(value):
    bounded = min(1.0 - 1e-12, max(1e-12, value))
    return math.log(bounded / (1.0 - bounded))


def _loss(scores, labels, temperature):
    total = 0.0
    for score, label in zip(scores, labels):
        value = _logit(score) / temperature
        # Stable binary cross entropy from a logit.
        total += max(value, 0.0) - value * label + math.log1p(math.exp(-abs(value)))
    return total / len(scores)


def fit(scores, labels, *, model_id, runtime_id, validation_manifest_id):
    if not isinstance(scores, dict) or not scores or set(scores) != set(labels):
        raise ValueError('scores and labels must have identical IDs')
    if len(scores) > MAX_ROWS or not isinstance(labels, dict):
        raise ValueError('calibration cohort exceeds bound')
    ids = sorted(scores)
    values, targets = [], []
    for row_id in ids:
        if not isinstance(row_id, str) or not row_id:
            raise ValueError('invalid calibration row ID')
        _finite(scores[row_id], 'score')
        if type(labels[row_id]) is not int or labels[row_id] not in (0, 1):
            raise ValueError('invalid calibration label')
        values.append(float(scores[row_id])); targets.append(labels[row_id])
    if len(set(targets)) != 2:
        raise ValueError('calibration needs both classes')
    if (not isinstance(model_id, str) or not model_id or not isinstance(runtime_id, str) or not runtime_id
            or not isinstance(validation_manifest_id, str) or not validation_manifest_id):
        raise ValueError('model and runtime identities are required')
    candidates = (0.25 + i * 0.01 for i in range(376))
    temperature = min(candidates, key=lambda value: (_loss(values, targets, value), value))
    return seal('calibration_artifact', method='temperature-v1', model_id=model_id,
                runtime_id=runtime_id, validation_manifest_id=validation_manifest_id,
                validation_ids=ids, temperature=temperature,
                validation_scores_digest=_digest([[row_id, scores[row_id]] for row_id in ids]),
                validation_labels_digest=_digest([[row_id, labels[row_id]] for row_id in ids]), optimization_enabled=False,
                limitation='Validation-only calibration; no promotion or serving mutation.')


def apply(artifact, scores, *, model_id, ids=None, split='test'):
    verify(artifact)
    if artifact['model_id'] != model_id or split not in ('test', 'inference'):
        raise ValueError('calibration model or split mismatch')
    if not isinstance(scores, dict) or not scores or len(scores) > MAX_ROWS:
        raise ValueError('invalid score cohort')
    row_ids = sorted(scores) if ids is None else ids
    _ids(row_ids, 'inference_ids')
    if set(row_ids) != set(scores) or set(row_ids) & set(artifact['validation_ids']):
        raise ValueError('calibration validation/inference overlap')
    calibrated = {}
    for row_id in row_ids:
        _finite(scores[row_id], 'score')
        value = 1.0 / (1.0 + math.exp(-_logit(float(scores[row_id])) / artifact['temperature']))
        calibrated[row_id] = value
    return {'artifact_id': artifact['id'], 'model_id': model_id, 'split': split,
            'scores': calibrated, 'optimization_enabled': False}
