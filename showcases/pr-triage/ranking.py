# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
"""Development ranking utilities. Scores are not calibrated defect probabilities."""
import math
import argparse
import json
from pathlib import Path

from candidates import checked
from shadow import digest, write_new


def model_scores(report, units):
    rows = report['predictions']
    indexed = {r['unit_id']: r for r in rows}
    if len(indexed) != len(rows) or set(indexed) != set(units):
        raise ValueError('prediction coverage mismatch')
    scores = {}
    for uid, row in indexed.items():
        if row['action'] != 'full_review' or row['optimization_eligible']:
            raise ValueError('shadow actions changed')
        score = None
        if row['disposition'] == 'predicted':
            answer = row['answer']
            if answer['choice'] != 'insufficient':
                score = answer['probabilities']['review']
                if not math.isfinite(score) or not 0 <= score <= 1:
                    raise ValueError('invalid score')
        scores[uid] = score
    return scores


def term_scores(units, terms):
    """Fixed pilot terms; count distinct matches, no fitted vocabulary."""
    scores = {}
    for uid, unit in units.items():
        text = (unit['path'] + ' ' + ' '.join((unit[s] or {}).get('text', '') for s in ('before', 'after'))).lower()
        scores[uid] = sum(term in text for term in terms)
    return scores


def evaluate(scores, labels, threshold):
    if set(scores) != set(labels) or not math.isfinite(threshold):
        raise ValueError('label coverage mismatch or invalid threshold')
    if any(label not in ('review', 'routine', 'unknown') for label in labels.values()):
        raise ValueError('unknown label encoding')
    if any(x is not None and (isinstance(x, bool) or not math.isfinite(x) or x < 0) for x in scores.values()):
        raise ValueError('invalid ranking score')
    # Missing evidence always retains. Unknown reference labels are counted separately,
    # never treated as negatives or used to manufacture recall.
    dropped = [uid for uid, score in scores.items() if score is not None and score < threshold]
    positive = sum(label == 'review' for label in labels.values())
    missed = [uid for uid in dropped if labels[uid] == 'review']
    unknown = [uid for uid in dropped if labels[uid] == 'unknown']
    return {'units': len(labels), 'positive': positive, 'proposed_dropped': len(dropped),
            'missed_positive': len(missed), 'missed_ids': missed, 'dropped_unknown_ids': unknown,
            'relevance_recall': (positive - len(missed)) / positive if positive else None,
            'proposed_reduction': len(dropped) / len(labels) if labels else 0,
            'dropped_ids': dropped}


def select_threshold(scores, labels, recall):
    if not 0 < recall <= 1 or not any(v == 'review' for v in labels.values()):
        raise ValueError('positive labels and valid recall target required')
    finite = [x for x in scores.values() if x is not None]
    # Strict '<' keeps tied scores together. Include the all-retained operating point.
    thresholds = sorted({0, *finite, *[math.nextafter(x, math.inf) for x in finite]})
    feasible = []
    for threshold in thresholds:
        metrics = evaluate(scores, labels, threshold)
        if metrics['relevance_recall'] >= recall and not metrics['dropped_unknown_ids']:
            feasible.append((metrics['proposed_dropped'], -threshold, threshold, metrics))
    if not feasible:
        raise ValueError('no feasible operating point')
    _, _, threshold, metrics = max(feasible)
    return {'threshold': threshold, **metrics}


def load_labels(path, functions):
    assessment = json.loads(path.read_text())
    if assessment.get('status') not in ('proposed', 'proposed_relevance_assessment', 'adjudicated_relevance_proposal'):
        raise ValueError('reference must declare proposed relevance status')
    rows = assessment.get('records', assessment.get('labels', []))
    indexed = {r['unit_id']: r for r in rows}
    if len(indexed) != len(rows) or set(indexed) != set(functions):
        raise ValueError('reference must cover every function exactly once')
    for uid, row in indexed.items():
        if row['pr'] != functions[uid]['pr'] or not (row.get('witness') or row.get('witnesses')):
            raise ValueError('reference missing witness or mismatched PR')
    return assessment, {k: r['label'] for k, r in indexed.items()}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--run', type=Path, required=True)
    parser.add_argument('--predictions', type=Path, required=True)
    parser.add_argument('--assessment', type=Path, required=True)
    parser.add_argument('--selection', type=Path, required=True,
                        help='Previously frozen development selection; no threshold fitting here')
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    manifest = checked(args.run / 'manifest.json')
    selection = checked(args.selection)
    if manifest['id'] != selection['validation_manifest_id'] or manifest['id'] == selection['development_manifest_id']:
        raise ValueError('validation cohort differs from frozen selection')
    units = {u['id']: u for c in manifest['cases'] for u in c['units']}
    functions = {k: v for k, v in units.items() if v['kind'] == 'function'}
    assessment, labels = load_labels(args.assessment, functions)
    prediction = checked(args.predictions)
    if prediction['manifest_id'] != manifest['id'] or prediction['candidate'] != selection['selected_candidate']:
        raise ValueError('candidate or manifest differs from frozen selection')
    all_scores = model_scores(prediction, units)
    scores = {k: all_scores[k] for k in functions}
    threshold = selection['selected_threshold']
    protocol = json.loads((args.run / 'protocol.json').read_text())
    if digest(protocol) != manifest['protocol_sha256']:
        raise ValueError('protocol digest mismatch')
    baselines = term_scores(functions, protocol['baseline_terms'])
    result = {'manifest_id': manifest['id'], 'selection_id': selection['id'],
              'prediction_id': prediction['id'], 'assessment_sha256': digest(assessment),
              'status': 'retrospective_proposed_relevance_comparison', 'optimization_enabled': False,
              'all_units': len(units), 'model': evaluate(scores, labels, threshold),
              'frozen_term_baseline': evaluate(baselines, labels, selection['candidate_results']['distinct-pilot-terms']['threshold']),
              'any_term_baseline': evaluate(baselines, labels, 1),
              'per_pr': {str(pr): evaluate({k: v for k, v in scores.items() if functions[k]['pr'] == pr},
                                          {k: v for k, v in labels.items() if functions[k]['pr'] == pr}, threshold)
                         for pr in sorted({u['pr'] for u in functions.values()})}}
    result['id'] = digest(result)
    write_new(args.output, result)
    print(json.dumps({k: v for k, v in result['model'].items() if not k.endswith('ids')}))


if __name__ == '__main__':
    main()
