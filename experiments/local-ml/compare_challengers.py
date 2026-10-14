#!/usr/bin/env python3
"""Compare bounded challengers on the same predeclared pictures/splits."""
import json
from pathlib import Path

import challenger_manifest as contract
import catboost_challenger
import unixcoder_challenger


def _metrics(rows, scores):
    labels = {row['id']: int(row['label']) for row in rows}
    if set(labels) != set(scores):
        raise ValueError('comparison prediction coverage mismatch')
    decisions = {row_id: int(score >= 0.5) for row_id, score in scores.items()}
    positives = sum(labels.values())
    predicted_positive = sum(decisions.values())
    true_positive = sum(decisions[row_id] and labels[row_id] for row_id in labels)
    false_positive = sum(decisions[row_id] and not labels[row_id] for row_id in labels)
    return {'examples': len(labels), 'positive_examples': positives,
            'accuracy': sum(decisions[row_id] == labels[row_id] for row_id in labels) / len(labels),
            'positive_recall': true_positive / positives if positives else None,
            'false_positive_rate': false_positive / (len(labels) - positives) if len(labels) != positives else None,
            'threshold': 0.5, 'threshold_predeclared': True, 'predicted_positive': predicted_positive}


def compare(train_rows, test_rows, manifest, embedder, output_dir):
    contract.validate(manifest)
    train_ids = {row.get('id') for row in train_rows}
    test_ids = {row.get('id') for row in test_rows}
    if not train_ids or not test_ids or train_ids & test_ids or any(row.get('split') != 'train' for row in train_rows) or any(row.get('split') != 'test' for row in test_rows):
        raise ValueError('comparison requires disjoint predeclared train/test splits')
    destination = Path(output_dir)
    destination.mkdir(parents=True, exist_ok=True)
    cat_model = destination / 'catboost.json'
    cat_artifact = catboost_challenger.train(train_rows, manifest, cat_model)
    cat_scores = catboost_challenger.predict(test_rows, manifest, cat_artifact, cat_model)['scores']
    unix_artifact = unixcoder_challenger.train(train_rows, manifest, embedder)
    unix_scores = unixcoder_challenger.predict(test_rows, manifest, unix_artifact, embedder)['scores']
    (destination / 'catboost.artifact.json').write_text(json.dumps(cat_artifact, indent=2) + '\n')
    (destination / 'unixcoder.artifact.json').write_text(json.dumps(unix_artifact, indent=2) + '\n')
    report = contract.seal('challenger_comparison', manifest_id=manifest['id'], train_ids=sorted(train_ids),
                           test_ids=sorted(test_ids), candidates={'catboost': cat_artifact['id'], 'unixcoder': unix_artifact['id']},
                           metrics={'catboost': _metrics(test_rows, cat_scores), 'unixcoder': _metrics(test_rows, unix_scores)},
                           disposition='measurement_only', optimization_enabled=False,
                           limitation='Synthetic or externally authorized labels only; no winner, promotion, exclusion, or quality generalization.')
    (destination / 'comparison.json').write_text(json.dumps(report, indent=2) + '\n')
    return report
