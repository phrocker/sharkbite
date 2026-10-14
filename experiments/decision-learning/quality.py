#!/usr/bin/env python3
"""Bounded, provider-neutral quality reports for local decision challengers.

The evaluator measures a declared labeled cohort; it never turns a targeted or
unweighted sample into a population claim and never authorizes promotion.
"""
import hashlib
import json
import math


SCHEMA = 1


def _digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(',', ':'), allow_nan=False).encode()).hexdigest()


def _digest_id(value, name):
    if not isinstance(value, str) or len(value) != 64 or any(char not in '0123456789abcdef' for char in value):
        raise ValueError('invalid ' + name)


def seal(kind, **fields):
    body = dict(schema=SCHEMA, kind=kind, **fields)
    return dict(body, id=_digest(body))


def verify(report):
    if not isinstance(report, dict) or report.get('schema') != SCHEMA or report.get('kind') != 'quality_report':
        raise ValueError('invalid quality report')
    body = {key: value for key, value in report.items() if key != 'id'}
    if report.get('id') != _digest(body):
        raise ValueError('quality report identity mismatch')
    if report.get('promotion_eligible') is not False:
        raise ValueError('quality report cannot authorize promotion')
    _verify_structure(report)
    return report


def _finite(value, name, low=0.0, high=1.0):
    if type(value) not in (int, float) or not math.isfinite(value) or not low <= value <= high:
        raise ValueError('invalid ' + name)


def _count(value, name):
    if type(value) is not int or value < 0:
        raise ValueError('invalid ' + name)


def _optional_rate(value, name):
    if value is not None:
        _finite(value, name)


def _verify_sample(sample):
    if not isinstance(sample, dict):
        raise ValueError('invalid quality sample')
    keys = {'kind', 'population', 'split', 'predeclared', 'future_inputs'}
    if sample.get('kind') in ('random', 'targeted'):
        keys.add('inclusion_probability')
    if set(sample) != keys:
        raise ValueError('quality sample schema mismatch')
    if sample.get('kind') not in ('uniform', 'random', 'targeted') or sample.get('split') != 'test':
        raise ValueError('invalid quality sample')
    if type(sample.get('population')) is not int or not 0 < sample['population'] <= 1_000_000_000:
        raise ValueError('invalid sample population')
    if sample.get('predeclared') is not True or sample.get('future_inputs') is not False:
        raise ValueError('quality sample is not predeclared')
    if sample['kind'] != 'uniform':
        probability = sample.get('inclusion_probability')
        if type(probability) not in (int, float) or not math.isfinite(probability) or not 0 < probability <= 1:
            raise ValueError('invalid sample inclusion probability')


def _verify_structure(report):
    for name in ('label_digest', 'score_digest', 'group_digest'):
        _digest_id(report.get(name), name)
    _finite(report.get('threshold'), 'threshold')
    _verify_sample(report.get('sample'))
    counts = report.get('counts')
    count_keys = {'total', 'resolved', 'unknown', 'disputed', 'positive', 'negative',
                  'true_positive', 'true_negative', 'false_positive', 'false_negative'}
    if not isinstance(counts, dict) or set(counts) != count_keys:
        raise ValueError('quality count schema mismatch')
    for name, value in counts.items():
        _count(value, name)
    if counts['total'] == 0 or counts['resolved'] == 0:
        raise ValueError('quality report has no measured rows')
    if report['sample']['population'] < counts['total']:
        raise ValueError('sample population below report denominator')
    if counts['total'] != counts['resolved'] + counts['unknown'] + counts['disputed']:
        raise ValueError('quality total denominator mismatch')
    if counts['resolved'] != counts['positive'] + counts['negative']:
        raise ValueError('quality label denominator mismatch')
    if counts['resolved'] != (counts['true_positive'] + counts['true_negative'] +
                              counts['false_positive'] + counts['false_negative']):
        raise ValueError('quality confusion denominator mismatch')
    if counts['true_positive'] + counts['false_negative'] != counts['positive']:
        raise ValueError('quality positive counts mismatch')
    if counts['true_negative'] + counts['false_positive'] != counts['negative']:
        raise ValueError('quality negative counts mismatch')
    metrics = report.get('metrics')
    metric_keys = {'accuracy', 'recall', 'specificity', 'brier', 'ece', 'recall_wilson_95'}
    if not isinstance(metrics, dict) or set(metrics) != metric_keys:
        raise ValueError('quality metric schema mismatch')
    for name in ('accuracy', 'brier', 'ece'):
        _finite(metrics[name], name)
    _optional_rate(metrics['recall'], 'recall')
    _optional_rate(metrics['specificity'], 'specificity')
    expected_accuracy = ((counts['true_positive'] + counts['true_negative']) /
                         counts['resolved'])
    if not math.isclose(metrics['accuracy'], expected_accuracy, rel_tol=1e-12, abs_tol=1e-12):
        raise ValueError('quality accuracy mismatch')
    expected_recall = (counts['true_positive'] / counts['positive']
                       if counts['positive'] else None)
    expected_specificity = (counts['true_negative'] / counts['negative']
                            if counts['negative'] else None)
    if ((metrics['recall'] is None) != (expected_recall is None) or
            metrics['recall'] is not None and not math.isclose(metrics['recall'], expected_recall, rel_tol=1e-12, abs_tol=1e-12) or
            (metrics['specificity'] is None) != (expected_specificity is None) or
            metrics['specificity'] is not None and not math.isclose(metrics['specificity'], expected_specificity, rel_tol=1e-12, abs_tol=1e-12)):
        raise ValueError('quality rate mismatch')
    interval = metrics['recall_wilson_95']
    expected_interval = _wilson(counts['true_positive'], counts['positive'])
    if interval != expected_interval:
        raise ValueError('quality uncertainty mismatch')
    if interval is not None:
        if (not isinstance(interval, list) or len(interval) != 2 or
                any(type(value) not in (int, float) or not math.isfinite(value) or not 0 <= value <= 1 for value in interval) or
                interval[0] > interval[1]):
            raise ValueError('invalid recall uncertainty interval')
    calibration = report.get('calibration')
    if not isinstance(calibration, list) or len(calibration) > 20:
        raise ValueError('invalid calibration bins')
    calibration_total = 0
    previous_lower = -1.0
    expected_ece = 0.0
    for bucket in calibration:
        if not isinstance(bucket, dict) or set(bucket) != {'lower', 'upper', 'count', 'mean_score', 'positive_rate'}:
            raise ValueError('calibration bucket schema mismatch')
        _finite(bucket['lower'], 'calibration lower')
        _finite(bucket['upper'], 'calibration upper')
        _count(bucket['count'], 'calibration count')
        _finite(bucket['mean_score'], 'calibration mean score')
        _finite(bucket['positive_rate'], 'calibration positive rate')
        if (bucket['count'] == 0 or bucket['lower'] >= bucket['upper'] or
                bucket['lower'] < previous_lower):
            raise ValueError('invalid calibration bucket')
        calibration_total += bucket['count']
        expected_ece += bucket['count'] * abs(bucket['mean_score'] - bucket['positive_rate'])
        previous_lower = bucket['lower']
    if calibration_total != counts['resolved']:
        raise ValueError('calibration denominator mismatch')
    if not math.isclose(metrics['ece'], expected_ece / calibration_total,
                        rel_tol=1e-12, abs_tol=1e-12):
        raise ValueError('quality calibration error mismatch')
    if type(report.get('families')) is not int or not 0 < report['families'] <= counts['total']:
        raise ValueError('invalid family count')
    costs = report.get('costs')
    if not isinstance(costs, dict) or set(costs) != {'inference_seconds', 'training_seconds', 'label_seconds'}:
        raise ValueError('quality cost schema mismatch')
    for name, value in costs.items():
        if type(value) not in (int, float) or not math.isfinite(value) or value < 0:
            raise ValueError('invalid cost ' + name)
    if type(report.get('population_claim')) is not bool:
        raise ValueError('invalid population claim')
    expected_claim = (report['sample']['kind'] in ('uniform', 'random') and
                      report['sample']['population'] == counts['total'] and
                      counts['unknown'] == 0 and counts['disputed'] == 0)
    if report['population_claim'] != expected_claim:
        raise ValueError('quality population claim mismatch')
    limitation = report.get('limitation')
    if not isinstance(limitation, str) or 'measurement' not in limitation.lower():
        raise ValueError('quality limitation is missing')


def _wilson(successes, total):
    if total == 0:
        return None
    z = 1.959963984540054
    p = successes / total
    denominator = 1 + z * z / total
    center = (p + z * z / (2 * total)) / denominator
    spread = z * math.sqrt((p * (1 - p) + z * z / (4 * total)) / total) / denominator
    return [max(0.0, center - spread), min(1.0, center + spread)]


def evaluate(labels, scores, groups, *, sample, threshold=0.5, bins=10, costs=None):
    if not isinstance(labels, dict) or not labels or len(labels) > 1_000_000 or set(labels) != set(scores) or set(labels) != set(groups):
        raise ValueError('labels, scores and groups must have identical nonempty IDs')
    if type(bins) is not int or not 2 <= bins <= 20:
        raise ValueError('invalid calibration bins')
    _finite(threshold, 'threshold')
    if not isinstance(sample, dict) or sample.get('split') != 'test' or sample.get('predeclared') is not True:
        raise ValueError('quality sample must be predeclared test data')
    kind = sample.get('kind')
    if kind not in ('uniform', 'random', 'targeted'):
        raise ValueError('invalid sample kind')
    inclusion_probability = sample.get('inclusion_probability')
    if kind == 'uniform' and inclusion_probability not in (None, 1, 1.0):
        raise ValueError('uniform sample must have unit inclusion probability')
    if kind in ('random', 'targeted'):
        if type(inclusion_probability) not in (int, float) or not math.isfinite(inclusion_probability) or not 0 < inclusion_probability <= 1:
            raise ValueError('sample inclusion probability is required')
    if sample.get('future_inputs') is not False:
        raise ValueError('future inputs are not permitted')
    if type(sample.get('population')) is not int or not 0 < sample['population'] <= 1_000_000_000 or sample['population'] < len(labels):
        raise ValueError('invalid sample population')
    resolved = []
    unknown = disputed = 0
    for row_id, label in labels.items():
        score = scores[row_id]
        _finite(score, 'score')
        if (not isinstance(row_id, str) or not row_id or len(row_id) > 512
                or not isinstance(groups[row_id], str) or not groups[row_id] or len(groups[row_id]) > 512):
            raise ValueError('invalid row identity or family')
        if label in ('unknown', 'disputed'):
            unknown += label == 'unknown'; disputed += label == 'disputed'
            continue
        if type(label) is not int or label not in (0, 1):
            raise ValueError('invalid label')
        resolved.append((row_id, label, score))
    if not resolved:
        raise ValueError('no resolved labels')
    predicted = {row_id: int(score >= threshold) for row_id, _, score in resolved}
    positives = sum(label for _, label, _ in resolved)
    negatives = len(resolved) - positives
    tp = sum(predicted[row_id] == label == 1 for row_id, label, _ in resolved)
    tn = sum(predicted[row_id] == label == 0 for row_id, label, _ in resolved)
    fp = sum(predicted[row_id] == 1 and label == 0 for row_id, label, _ in resolved)
    fn = sum(predicted[row_id] == 0 and label == 1 for row_id, label, _ in resolved)
    recall = tp / positives if positives else None
    specificity = tn / negatives if negatives else None
    brier = sum((score - label) ** 2 for _, label, score in resolved) / len(resolved)
    calibration = []
    for index in range(bins):
        lower, upper = index / bins, (index + 1) / bins
        bucket = [(label, score) for _, label, score in resolved if lower <= score < upper or index == bins - 1 and score == upper]
        if bucket:
            calibration.append({'lower': lower, 'upper': upper, 'count': len(bucket),
                                'mean_score': sum(score for _, score in bucket) / len(bucket),
                                'positive_rate': sum(label for label, _ in bucket) / len(bucket)})
    ece = sum(row['count'] * abs(row['mean_score'] - row['positive_rate']) for row in calibration) / len(resolved)
    cost = {'inference_seconds': 0.0, 'training_seconds': 0.0, 'label_seconds': 0.0}
    if costs is not None:
        if not isinstance(costs, dict):
            raise ValueError('invalid costs')
        for key in cost:
            value = costs.get(key, 0.0)
            if type(value) not in (int, float) or not math.isfinite(value) or value < 0:
                raise ValueError('invalid cost ' + key)
            cost[key] = float(value)
    return seal('quality_report', sample=sample, threshold=threshold,
                label_digest=_digest(labels), score_digest=_digest(scores),
                group_digest=_digest(groups),
                counts={'total': len(labels), 'resolved': len(resolved), 'unknown': unknown,
                        'disputed': disputed, 'positive': positives, 'negative': negatives,
                        'true_positive': tp, 'true_negative': tn, 'false_positive': fp, 'false_negative': fn},
                metrics={'accuracy': (tp + tn) / len(resolved), 'recall': recall,
                         'specificity': specificity, 'brier': brier, 'ece': ece,
                         'recall_wilson_95': _wilson(tp, positives)},
                calibration=calibration, families=len(set(groups.values())), costs=cost,
                population_claim=(kind in ('uniform', 'random') and sample['population'] == len(labels)
                                  and unknown == 0 and disputed == 0),
                promotion_eligible=False,
                limitation='Measurement only; unknown/disputed rows excluded, targeted samples are not population estimates.')
