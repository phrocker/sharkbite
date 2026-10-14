#!/usr/bin/env python3
# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
"""Replay a data-only lexical model locally; proposals never change review actions."""
import argparse
from pathlib import Path
import time

from candidates import checked, original_state
from lexical import RECIPE, recipe_fingerprint, representation_fingerprint
from shadow import canonical, digest, write_new


def load_bundle(path):
    import numpy as np
    import sklearn
    from sklearn.feature_extraction.text import TfidfVectorizer
    from sklearn.linear_model import LogisticRegression
    from sklearn.pipeline import make_pipeline
    bundle = checked(path)
    if (bundle['schema'] != 2 or bundle['kind'] != 'char-wb-3-5-tfidf-logistic-C1-balanced'
            or bundle['representation'] != 'original_state-v1' or bundle['sklearn_version'] != sklearn.__version__
            or bundle.get('representation_sha256') != representation_fingerprint()
            or bundle.get('recipe_id') != recipe_fingerprint()
            or bundle['classes'] != [False, True] or bundle['optimization_enabled'] is not False):
        raise ValueError('unsupported model contract')
    size = len(bundle['vocabulary'])
    if not 0 < size <= 20000 or sorted(bundle['vocabulary'].values()) != list(range(size)):
        raise ValueError('invalid vocabulary')
    idf = np.asarray(bundle['idf'], dtype=float)
    coef = np.asarray(bundle['coef'], dtype=float)
    intercept = np.asarray(bundle['intercept'], dtype=float)
    if idf.shape != (size,) or coef.shape != (1, size) or intercept.shape != (1,):
        raise ValueError('invalid parameter dimensions')
    if any(not np.isfinite(x).all() for x in (idf, coef, intercept)) or (idf <= 0).any():
        raise ValueError('invalid parameters')
    if not 0 <= bundle['threshold'] <= 1:
        raise ValueError('invalid threshold')
    vectorizer = TfidfVectorizer(**RECIPE['vectorizer'], vocabulary=bundle['vocabulary'])
    vectorizer.idf_ = idf
    classifier = LogisticRegression(**RECIPE['classifier'])
    classifier.classes_ = np.asarray([False, True])
    classifier.coef_, classifier.intercept_, classifier.n_features_in_ = coef, intercept, size
    return bundle, make_pipeline(vectorizer, classifier)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--bundle', type=Path, required=True)
    parser.add_argument('--manifest', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    started = time.perf_counter()
    bundle, model = load_bundle(args.bundle)
    load_ms = (time.perf_counter() - started) * 1000
    manifest = checked(args.manifest)
    predictions = []
    for case in manifest['cases']:
        for unit in case['units']:
            row = {'unit_id': unit['id'], 'pr': unit['pr'], 'action': 'full_review', 'optimization_eligible': False,
                   'disposition': 'unsupported_unit', 'proposal': 'retain'}
            if unit['kind'] == 'function':
                state = original_state(unit)
                start = time.perf_counter()
                score = float(model.predict_proba([canonical(state)])[0, 1])
                row.update(disposition='predicted', review_score=score,
                           proposal='lower_priority' if score < bundle['threshold'] else 'retain',
                           state_sha256=digest(state), elapsed_ms=(time.perf_counter() - start) * 1000)
            predictions.append(row)
    report = {'model_id': bundle['id'], 'manifest_id': manifest['id'], 'load_ms': load_ms,
              'predictions': predictions, 'optimization_enabled': False}
    report['id'] = digest(report)
    write_new(args.output, report)


if __name__ == '__main__':
    main()
