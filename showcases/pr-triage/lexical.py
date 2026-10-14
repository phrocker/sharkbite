#!/usr/bin/env python3
# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
"""Cheap trained baseline; vocabulary and weights are fitted inside each PR fold."""
import argparse
import json
import inspect
from pathlib import Path
import time

from candidates import checked, original_state
from ranking import evaluate, load_labels, select_threshold
from shadow import canonical, digest, write_new


RECIPE = {'vectorizer': {'analyzer': 'char_wb', 'ngram_range': (3, 5), 'max_features': 20000, 'sublinear_tf': True},
          'classifier': {'C': 1., 'class_weight': 'balanced', 'max_iter': 1000, 'random_state': 42}}


def representation_fingerprint():
    return digest(inspect.getsource(original_state) + inspect.getsource(canonical))


def fit_model(rows):
    from sklearn.feature_extraction.text import TfidfVectorizer
    from sklearn.linear_model import LogisticRegression
    from sklearn.pipeline import make_pipeline
    rows = [r for r in rows if r['label'] != 'unknown']
    model = make_pipeline(TfidfVectorizer(**RECIPE['vectorizer']), LogisticRegression(**RECIPE['classifier']))
    return model.fit([r['text'] for r in rows], [r['label'] == 'review' for r in rows])


def recipe_fingerprint():
    return digest({'parameters': RECIPE, 'fit_sha256': digest(inspect.getsource(fit_model)),
                   'representation_sha256': representation_fingerprint(), 'sklearn_version': '1.7.2'})


def main():
    import sklearn
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--config', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--selection', type=Path, help='Frozen threshold; reserved reference labels need not be supplied')
    parser.add_argument('--export', type=Path, help='Write data-only model bundle (no pickle or executable payload)')
    args = parser.parse_args()
    config = json.loads(args.config.read_text())
    if sklearn.__version__ != '1.7.2':
        raise ValueError('requires pinned scikit-learn 1.7.2')
    datasets = {}
    for split in ('development', 'exploratory'):
        manifest = checked(Path(config[split]['manifest']))
        funcs = {u['id']: u for c in manifest['cases'] for u in c['units'] if u['kind'] == 'function'}
        if config[split].get('assessment'):
            _, labels = load_labels(Path(config[split]['assessment']), funcs)
        elif split == 'exploratory' and args.selection:
            labels = {uid: 'unknown' for uid in funcs}
        else:
            raise ValueError('development labels required')
        datasets[split] = [{'unit_id': uid, 'pr': u['pr'], 'text': canonical(original_state(u)),
                            'label': labels[uid]} for uid, u in funcs.items()]

    def predict(model, rows):
        return {r['unit_id']: float(p) for r, p in zip(rows, model.predict_proba([r['text'] for r in rows])[:, 1])}

    development = datasets['development']
    oof = {}
    started = time.perf_counter()
    if args.selection:
        frozen = checked(args.selection)
        if frozen.get('recipe_id') != recipe_fingerprint() or frozen.get('model') != 'char-wb-3-5-tfidf-logistic-C1-balanced':
            raise ValueError('frozen training recipe mismatch; historical selections need an explicit replay attestation')
        if frozen['development_manifest_id'] != checked(Path(config['development']['manifest']))['id'] or frozen['validation_manifest_id'] != checked(Path(config['exploratory']['manifest']))['id']:
            raise ValueError('frozen manifest mismatch')
        if frozen['assessment_sha256'] != digest(json.loads(Path(config['development']['assessment']).read_text())):
            raise ValueError('frozen training label mismatch')
        selection = {'threshold': frozen['threshold'], 'selection_id': frozen['id']}
    else:
        for pr in sorted({r['pr'] for r in development}):
            test = [r for r in development if r['pr'] == pr]
            texts = {r['text'] for r in test}
            train = [r for r in development if r['pr'] != pr and r['text'] not in texts]
            oof.update(predict(fit_model(train), test))
        selection = select_threshold(oof, {r['unit_id']: r['label'] for r in development}, config['target_recall'])
    model = fit_model(development)
    if args.export:
        vectorizer, classifier = model.steps[0][1], model.steps[1][1]
        bundle = {'schema': 2, 'kind': 'char-wb-3-5-tfidf-logistic-C1-balanced',
                  'recipe_id': recipe_fingerprint(), 'representation_sha256': representation_fingerprint(),
                  'sklearn_version': sklearn.__version__, 'task': 'authorization-relevance-proposal',
                  'representation': 'original_state-v1', 'threshold': selection['threshold'],
                  'config_sha256': digest(config), 'selection': selection,
                  'development_manifest_id': checked(Path(config['development']['manifest']))['id'],
                  'assessment_sha256': digest(json.loads(Path(config['development']['assessment']).read_text())),
                  'vocabulary': {k: int(v) for k, v in vectorizer.vocabulary_.items()}, 'idf': vectorizer.idf_.tolist(),
                  'coef': classifier.coef_.tolist(), 'intercept': classifier.intercept_.tolist(),
                  'classes': classifier.classes_.tolist(), 'optimization_enabled': False,
                  'limitations': ['Agent-proposed relevance, not defect truth', 'Missing caller context',
                                  'Scores are not calibrated probabilities', 'Full review remains mandatory']}
        bundle['id'] = digest(bundle)
        write_new(args.export, bundle)
    start = time.perf_counter()
    scores = predict(model, datasets['exploratory'])
    elapsed = time.perf_counter() - start
    result = {'config_sha256': digest(config), 'implementation_sha256': digest(Path(__file__).read_bytes()),
              'recipe_id': recipe_fingerprint(),
              'model': 'char-wb-3-5-tfidf-logistic-C1-balanced', 'sklearn_version': sklearn.__version__,
              'selection': selection, 'oof_scores': oof, 'exploratory_scores': scores,
              'exploratory_metrics': evaluate(scores, {r['unit_id']: r['label'] for r in datasets['exploratory']}, selection['threshold']),
              'total_seconds': time.perf_counter() - started, 'exploratory_batch_ms': elapsed * 1000,
              'optimization_enabled': False, 'calibrated': False}
    result['id'] = digest(result)
    write_new(args.output, result)
    print(json.dumps({k: v for k, v in result['exploratory_metrics'].items() if not k.endswith('ids')}))


if __name__ == '__main__':
    main()
