#!/usr/bin/env python3
# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
"""Development-only comparison after retiring v3; optimize code-volume utility."""
import argparse
import json
from pathlib import Path
import re
import statistics

from candidates import checked, original_state, render
from ranking import load_labels, select_threshold
from shadow import canonical, digest, parsed, source, write_new


def callers(unit, repo, extractor, cache):
    result = {'method': 'same-file syntactic name matches, not resolved call edges',
              'coverage': 'cross-file and dynamic callers unknown', 'sides': {}}
    for side, revision_key, path_key in (('before', 'base', 'before_path'), ('after', 'head', 'after_path')):
        declaration = unit.get(side)
        if not declaration:
            result['sides'][side] = {'disposition': 'absent', 'candidates': []}
            continue
        key = (unit[revision_key], unit[path_key])
        if key not in cache:
            src = source(repo, key[0], key[1], 250000)
            cache[key] = parsed(extractor, key[1], src) if src['disposition'] == 'available' else {'error': src['disposition']
            }
        document = cache[key]
        if document.get('error'):
            result['sides'][side] = {'disposition': 'unavailable', 'candidates': []}
            continue
        target_name = declaration['name'].split('.')[-1]
        candidates, omitted = [], 0
        for item in document['declarations']:
            if item['kind'] != 'function' or item['key'] == unit['symbol']:
                continue
            if not any(re.sub(r'\[.*\]$', '', call).split('.')[-1] == target_name for call in item['syntactic_calls']):
                continue
            if len(candidates) >= 3 or len(item['text'].encode()) > 5000:
                omitted += 1
                continue
            candidates.append({'symbol': item['key'], 'text': item['text'], 'source_sha256': digest(item['text'].encode()),
                               'lines': [item['start_line'], item['end_line']], 'resolution': 'syntactic_candidate_only'})
        result['sides'][side] = {'disposition': 'partial', 'revision': key[0], 'path': key[1],
                                'parsed_declarations': len(document['declarations']), 'omitted_candidates': omitted,
                                'candidates': candidates}
    return result


def main():
    import numpy as np
    import sklearn
    from sklearn.feature_extraction.text import TfidfVectorizer
    from sklearn.linear_model import LogisticRegression
    from sklearn.pipeline import make_pipeline
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--repo', type=Path, required=True)
    parser.add_argument('--extractor', type=Path, required=True)
    args = parser.parse_args()
    if sklearn.__version__ != '1.7.2':
        raise ValueError('pinned sklearn required')
    args.output.mkdir(parents=True, exist_ok=False)
    archive = Path(__file__).parent / 'runs'
    specs = [(archive/'pilot-v1/manifest.json', archive/'candidates-v2/adjudication.json'),
             (archive/'validation-v2/manifest.json', archive/'validation-v2/adjudication.json'),
             (archive/'reserved-v3/sampled-manifest.json', archive/'reserved-v3/adjudication.json')]
    rows, states, cache = [], {}, {}
    for manifest_path, label_path in specs:
        manifest = checked(manifest_path)
        units = {u['id']: u for c in manifest['cases'] for u in c['units'] if u['kind'] == 'function'}
        _, labels = load_labels(label_path, units)
        for uid, unit in units.items():
            changed = render(unit, 'diff')
            states[uid] = {'original': original_state(unit), 'diff': changed,
                           'diff_callers': {**changed, 'caller_candidates': callers(unit, args.repo, args.extractor, cache)}}
            rows.append({'unit_id': uid, 'pr': unit['pr'], 'label': labels[uid],
                         'diff_bytes': len(changed['change'].encode()), 'source_bytes': sum(len((unit[s] or {}).get('text', '').encode()) for s in ('before', 'after'))})
    if len({r['unit_id'] for r in rows}) != len(rows):
        raise ValueError('duplicate unit across development inputs')
    inputs = {'status': 'development_only_all_prior_evaluation_retired', 'rows': rows, 'states': states,
              'source_inputs': [{'manifest_id': checked(m)['id'], 'assessment_sha256': digest(json.loads(a.read_text()))} for m, a in specs],
              'renderer_sha256': digest(Path(__file__).read_bytes())}
    inputs['id'] = digest(inputs); write_new(args.output/'inputs.json', inputs)
    prs = sorted({r['pr'] for r in rows})
    folds = {pr: i % 5 for i, pr in enumerate(prs)}
    config = {'status': 'development_only', 'inputs_id': inputs['id'], 'folds': folds,
              'candidates': [('original', False), ('diff', False), ('diff_callers', False), ('diff_callers', True)],
              'target_relevance_recall': .95, 'utility': 'fraction of function-diff bytes proposed for reduced review',
              'recipe': 'char_wb3-5,max_features20000,sublinear_tf; logistic C1 balanced; seed42; optional sqrt(diff_bytes/median) clipped .25..4',
              'optimization_enabled': False}
    write_new(args.output/'config.json', config)
    labels = {r['unit_id']: r['label'] for r in rows}
    for representation, weighted in config['candidates']:
        scores = {}
        for fold in range(5):
            test = [r for r in rows if folds[r['pr']] == fold]
            hashes = {digest(states[r['unit_id']][representation]) for r in test}
            train = [r for r in rows if folds[r['pr']] != fold and r['label'] != 'unknown'
                     and digest(states[r['unit_id']][representation]) not in hashes]
            model = make_pipeline(TfidfVectorizer(analyzer='char_wb', ngram_range=(3, 5), max_features=20000, sublinear_tf=True),
                                  LogisticRegression(C=1., class_weight='balanced', max_iter=1000, random_state=42))
            median = statistics.median(r['diff_bytes'] for r in train)
            weights = np.clip(np.sqrt([r['diff_bytes']/max(1, median) for r in train]), .25, 4.) if weighted else np.ones(len(train))
            model.fit([canonical(states[r['unit_id']][representation]) for r in train], [r['label']=='review' for r in train],
                      logisticregression__sample_weight=weights)
            p = model.predict_proba([canonical(states[r['unit_id']][representation]) for r in test])[:, 1]
            scores.update({r['unit_id']: float(score) for r, score in zip(test, p)})
        selection = select_threshold(scores, labels, .95)
        selected = set(selection['dropped_ids'])
        volume = {'all_diff_bytes': sum(r['diff_bytes'] for r in rows),
                  'proposed_lower_priority_diff_bytes': sum(r['diff_bytes'] for r in rows if r['unit_id'] in selected)}
        result = {'representation': representation, 'volume_weighted_training': weighted, 'inputs_id': inputs['id'],
                  'scores': scores, 'selection': selection, 'volume': volume, 'status': 'threshold_selected_development_oof',
                  'optimization_enabled': False}
        result['id'] = digest(result)
        name = representation + ('-weighted' if weighted else '')
        write_new(args.output/(name+'.json'), result)
        print(json.dumps({'candidate': name, 'retained': selection['positive']-selection['missed_positive'],
                          'positive': selection['positive'], 'function_reduction': selection['proposed_reduction'],
                          'diff_byte_reduction': volume['proposed_lower_priority_diff_bytes']/volume['all_diff_bytes']}), flush=True)


if __name__ == '__main__':
    main()
