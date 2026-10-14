#!/usr/bin/env python3
"""Verify a same-picture challenger comparison receipt offline."""
import argparse
import json
import math
from pathlib import Path

import challenger_manifest as contract

_METRIC_KEYS = {'examples', 'positive_examples', 'accuracy', 'positive_recall',
                'false_positive_rate', 'threshold', 'threshold_predeclared', 'predicted_positive'}


def _ids(value, name):
    if not isinstance(value, list) or not value or len(value) > 1_000_000 or any(not isinstance(item, str) or not item for item in value):
        raise ValueError(name + ' must be a nonempty string list')
    if value != sorted(value) or len(set(value)) != len(value):
        raise ValueError(name + ' must be sorted and unique')
    return set(value)


def _digest(value, name):
    if not isinstance(value, str) or len(value) != 64 or any(char not in '0123456789abcdef' for char in value):
        raise ValueError(name + ' must be a SHA-256 digest')


def verify(report):
    if not isinstance(report, dict):
        raise ValueError('report must be an object')
    if report.get('schema') != contract.SCHEMA or report.get('kind') != 'challenger_comparison':
        raise ValueError('unexpected comparison kind')
    body = {key: value for key, value in report.items() if key != 'id'}
    if report.get('id') != contract._digest(body):
        raise ValueError('comparison identity mismatch')
    _digest(report.get('manifest_id'), 'manifest_id')
    train = _ids(report.get('train_ids'), 'train_ids')
    test = _ids(report.get('test_ids'), 'test_ids')
    if train & test:
        raise ValueError('comparison splits overlap')
    if report.get('disposition') != 'measurement_only' or report.get('optimization_enabled') is not False:
        raise ValueError('comparison is not measurement-only')
    candidates = report.get('candidates')
    if not isinstance(candidates, dict) or set(candidates) != {'catboost', 'unixcoder'}:
        raise ValueError('comparison candidate set mismatch')
    for name in candidates:
        _digest(candidates[name], name + ' artifact')
    if candidates['catboost'] == candidates['unixcoder']:
        raise ValueError('comparison candidates must be distinct artifacts')
    metrics = report.get('metrics')
    if not isinstance(metrics, dict) or set(metrics) != {'catboost', 'unixcoder'}:
        raise ValueError('comparison metric set mismatch')
    for name, values in metrics.items():
        if not isinstance(values, dict) or set(values) != _METRIC_KEYS:
            raise ValueError(name + ' metric schema mismatch')
        if type(values['examples']) is not int or values['examples'] != len(test):
            raise ValueError(name + ' example denominator mismatch')
        for field in ('positive_examples', 'predicted_positive'):
            if type(values[field]) is not int or not 0 <= values[field] <= len(test):
                raise ValueError(name + ' count is out of bounds')
        for field in ('accuracy', 'positive_recall', 'false_positive_rate'):
            value = values[field]
            if value is not None and (type(value) not in (int, float) or not math.isfinite(value) or not 0 <= value <= 1):
                raise ValueError(name + ' rate is out of bounds')
        if values['positive_examples'] == 0 and values['positive_recall'] is not None:
            raise ValueError(name + ' recall requires positive examples')
        if values['positive_examples'] == len(test) and values['false_positive_rate'] is not None:
            raise ValueError(name + ' false-positive rate requires negatives')
        if values['positive_examples'] > 0 and values['positive_recall'] is None:
            raise ValueError(name + ' recall is missing')
        if values['positive_examples'] < len(test) and values['false_positive_rate'] is None:
            raise ValueError(name + ' false-positive rate is missing')
        if values['threshold'] != 0.5 or values['threshold_predeclared'] is not True:
            raise ValueError(name + ' threshold is not pinned')
    limitation = report.get('limitation')
    if not isinstance(limitation, str) or 'measurement' not in limitation.lower():
        raise ValueError('comparison limitation is missing')
    return {'id': report['id'], 'verified': True}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('report', type=Path)
    args = parser.parse_args()
    with args.report.open() as source:
        report = json.load(source)
    print(json.dumps(verify(report), sort_keys=True))


if __name__ == '__main__':
    main()
