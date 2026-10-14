#!/usr/bin/env python3
"""Sealed leave-one-out influence measurements for challenger artifacts."""
import hashlib
import json
import math


SCHEMA = 1
MAX_ROWS = 1_000_000
MAX_TEXT = 512


def _digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(',', ':'), allow_nan=False).encode()).hexdigest()


def seal(kind, **fields):
    body = dict(schema=SCHEMA, kind=kind, **fields)
    return dict(body, id=_digest(body))


def _ids(ids):
    if not isinstance(ids, list) or not ids or len(ids) > MAX_ROWS or any(not isinstance(row_id, str) or not row_id for row_id in ids):
        raise ValueError('invalid test IDs')
    if ids != sorted(ids) or len(set(ids)) != len(ids):
        raise ValueError('test IDs must be sorted and unique')


def _text(value, name):
    if not isinstance(value, str) or not value or len(value) > MAX_TEXT:
        raise ValueError('invalid ' + name)


def _scores(scores, ids, name):
    if not isinstance(scores, dict) or set(scores) != set(ids):
        raise ValueError(name + ' coverage mismatch')
    for row_id in ids:
        value = scores[row_id]
        if type(value) not in (int, float) or not math.isfinite(value) or not 0 <= value <= 1:
            raise ValueError('invalid ' + name + ' score')


def _score_digest(scores):
    return _digest(scores)


def _digest_id(value, name):
    if not isinstance(value, str) or len(value) != 64 or any(char not in '0123456789abcdef' for char in value):
        raise ValueError('invalid ' + name)


def verify(report):
    if not isinstance(report, dict) or report.get('schema') != SCHEMA or report.get('kind') != 'influence_report':
        raise ValueError('invalid influence report')
    body = {key: value for key, value in report.items() if key != 'id'}
    if report.get('id') != _digest(body):
        raise ValueError('influence report identity mismatch')
    _ids(report.get('test_ids'))
    for name in ('baseline_model_id', 'perturbed_model_id', 'baseline_manifest_id',
                 'perturbed_manifest_id', 'removed_training_id', 'limitation'):
        _text(report.get(name), name)
    for name in ('baseline_scores_digest', 'perturbed_scores_digest'):
        _digest_id(report.get(name), name)
    if report['baseline_model_id'] == report['perturbed_model_id']:
        raise ValueError('influence report reuses model identity')
    if report['baseline_manifest_id'] == report['perturbed_manifest_id']:
        raise ValueError('influence report reuses manifest identity')
    threshold = report.get('threshold')
    if type(threshold) not in (int, float) or not math.isfinite(threshold) or not 0 <= threshold <= 1:
        raise ValueError('invalid threshold')
    metrics = report.get('metrics')
    if not isinstance(metrics, dict) or set(metrics) != {'mean_absolute_delta', 'max_absolute_delta', 'decision_flips', 'flipped_ids'}:
        raise ValueError('invalid influence metrics')
    for name in ('mean_absolute_delta', 'max_absolute_delta'):
        value = metrics[name]
        if type(value) not in (int, float) or not math.isfinite(value) or not 0 <= value <= 1:
            raise ValueError('invalid ' + name)
    if metrics['mean_absolute_delta'] > metrics['max_absolute_delta']:
        raise ValueError('influence metric ordering mismatch')
    flips = metrics['decision_flips']
    if type(flips) is not int or not 0 <= flips <= len(report['test_ids']):
        raise ValueError('invalid decision flip count')
    _ids(metrics['flipped_ids']) if metrics['flipped_ids'] else None
    if metrics['flipped_ids'] != sorted(metrics['flipped_ids']) or len(metrics['flipped_ids']) != flips or any(row_id not in report['test_ids'] for row_id in metrics['flipped_ids']):
        raise ValueError('invalid flipped IDs')
    if report.get('optimization_enabled') is not False or report.get('unlearning_claim') is not False:
        raise ValueError('influence report has unsafe disposition')
    return report


def measure(baseline, perturbed, *, baseline_model_id, perturbed_model_id,
            baseline_manifest_id, perturbed_manifest_id, removed_training_id,
            threshold=0.5, test_ids=None):
    if test_ids is None:
        test_ids = sorted(baseline) if isinstance(baseline, dict) else []
    _ids(test_ids)
    _scores(baseline, test_ids, 'baseline')
    _scores(perturbed, test_ids, 'perturbed')
    for name, value in (('baseline_model_id', baseline_model_id), ('perturbed_model_id', perturbed_model_id),
                        ('baseline_manifest_id', baseline_manifest_id), ('perturbed_manifest_id', perturbed_manifest_id),
                        ('removed_training_id', removed_training_id)):
        _text(value, name)
    if baseline_model_id == perturbed_model_id or baseline_manifest_id == perturbed_manifest_id:
        raise ValueError('perturbation must produce a distinct artifact and manifest')
    if type(threshold) not in (int, float) or not math.isfinite(threshold) or not 0 <= threshold <= 1:
        raise ValueError('invalid threshold')
    deltas = {row_id: perturbed[row_id] - baseline[row_id] for row_id in test_ids}
    absolute = [abs(value) for value in deltas.values()]
    baseline_decisions = {row_id: int(baseline[row_id] >= threshold) for row_id in test_ids}
    perturbed_decisions = {row_id: int(perturbed[row_id] >= threshold) for row_id in test_ids}
    flips = sorted(row_id for row_id in test_ids if baseline_decisions[row_id] != perturbed_decisions[row_id])
    return seal('influence_report', test_ids=test_ids,
                baseline_model_id=baseline_model_id, perturbed_model_id=perturbed_model_id,
                baseline_manifest_id=baseline_manifest_id, perturbed_manifest_id=perturbed_manifest_id,
                baseline_scores_digest=_score_digest(baseline),
                perturbed_scores_digest=_score_digest(perturbed),
                removed_training_id=removed_training_id, threshold=threshold,
                metrics={'mean_absolute_delta': sum(absolute) / len(absolute),
                         'max_absolute_delta': max(absolute), 'decision_flips': len(flips),
                         'flipped_ids': flips}, optimization_enabled=False,
                unlearning_claim=False,
                limitation='Influence measurement only; no truth, quality, causal, or machine-unlearning claim.')
