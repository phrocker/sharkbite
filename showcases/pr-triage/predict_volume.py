#!/usr/bin/env python3
# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
"""Fit and score one previously frozen volume-oriented lexical recipe."""
import argparse
import json
from pathlib import Path
import statistics
import time

from candidates import checked, render
from shadow import canonical, digest, write_new
from volume_candidates import callers


def main():
    import numpy as np
    from sklearn.feature_extraction.text import TfidfVectorizer
    from sklearn.linear_model import LogisticRegression
    from sklearn.pipeline import make_pipeline
    import sklearn
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--selection', type=Path, required=True)
    parser.add_argument('--inputs', type=Path, required=True)
    parser.add_argument('--manifest', type=Path, required=True)
    parser.add_argument('--repo', type=Path, required=True)
    parser.add_argument('--extractor', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    selection, inputs, manifest = checked(args.selection), checked(args.inputs), checked(args.manifest)
    if (sklearn.__version__ != '1.7.2' or selection['inputs_id'] != inputs['id']
            or selection['validation_manifest_id'] != manifest['id']
            or selection['runner_sha256'] != digest(Path(__file__).read_bytes())
            or selection['training_runner_sha256'] != digest(Path(__file__).with_name('volume_candidates.py').read_bytes())
            or selection['renderer_sha256'] != digest(Path(__file__).with_name('candidates.py').read_bytes())
            or selection.get('extractor_sha256') != digest(args.extractor.read_bytes())
            or selection.get('source_helpers_sha256') != digest(Path(__file__).with_name('shadow.py').read_bytes())):
        raise ValueError('frozen volume candidate mismatch')
    rows = [r for r in inputs['rows'] if r['label'] != 'unknown']
    model = make_pipeline(TfidfVectorizer(analyzer='char_wb', ngram_range=(3, 5), max_features=20000, sublinear_tf=True),
                          LogisticRegression(C=1., class_weight='balanced', max_iter=1000, random_state=42))
    median = statistics.median(r['diff_bytes'] for r in rows)
    weights = np.clip(np.sqrt([r['diff_bytes']/max(1, median) for r in rows]), .25, 4.)
    model.fit([canonical(inputs['states'][r['unit_id']]['diff_callers']) for r in rows],
              [r['label']=='review' for r in rows], logisticregression__sample_weight=weights)
    cache, predictions = {}, []
    for case in manifest['cases']:
        for unit in case['units']:
            row = {'unit_id': unit['id'], 'pr': unit['pr'], 'action': 'full_review', 'optimization_eligible': False,
                   'disposition': 'unsupported_unit', 'review_score': None}
            if unit['kind'] == 'function':
                state = {**render(unit, 'diff'), 'caller_candidates': callers(unit, args.repo, args.extractor, cache)}
                start = time.perf_counter()
                score = float(model.predict_proba([canonical(state)])[0, 1])
                row.update(review_score=score, disposition='predicted', state_sha256=digest(state),
                           elapsed_ms=(time.perf_counter()-start)*1000)
            row['proposal'] = 'lower_priority' if row['review_score'] is not None and row['review_score'] < selection['threshold'] else 'retain'
            predictions.append(row)
    result = {'manifest_id': manifest['id'], 'selection_id': selection['id'], 'predictions': predictions,
              'optimization_enabled': False, 'runner_sha256': digest(Path(__file__).read_bytes())}
    result['id'] = digest(result)
    write_new(args.output, result)


if __name__ == '__main__':
    main()
