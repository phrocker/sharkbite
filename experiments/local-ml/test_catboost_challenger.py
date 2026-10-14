import json
import importlib.util
import sys
import tempfile
import unittest
from pathlib import Path

import challenger_manifest as contract
import catboost_challenger as challenger


def make_manifest():
    def recipe(name):
        base = {'name': name, 'runtime_id': 'catboost-runtime-v1', 'weights_digest': 'a' * 64,
                'tokenizer_digest': 'b' * 64, 'license_id': 'license-v1'}
        if name == 'catboost-structured-v1':
            base.update(library_version='1.2.8', categorical_features=['operation_kind'])
        else:
            base.update(model_repo='microsoft/unixcoder-base', model_revision='c' * 40,
                        tokenizer_revision='d' * 40, max_context_tokens=512, chunking='reject')
        return base
    return contract.build_manifest('task-v1', 'dataset-v1', 'split-v1', 'features-v1',
                                   ['reachability', 'freshness', 'operation_kind'], 'labels-v1',
                                   '2026-12-01T00:00:00Z', ['recall_at_budget'], {'max_trials': 1},
                                   [recipe('catboost-structured-v1'), recipe('unixcoder-linear-v1')])


class CatBoostChallengerTests(unittest.TestCase):
    @unittest.skipUnless('catboost' in sys.modules or importlib.util.find_spec('catboost'),
                         'optional CatBoost dependency is not installed')
    def test_train_and_replay_pinned_structured_candidate(self):
        rows = [{'id': str(i), 'split': 'train', 'label': i % 2,
                 'features': {'reachability': float(i), 'freshness': 1.0 - i / 6, 'operation_kind': 'write' if i % 2 else 'read'}} for i in range(6)]
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'model.json'
            manifest = make_manifest()
            artifact = challenger.train(rows, manifest, path)
            result = challenger.predict(rows, manifest, artifact, path)
            self.assertEqual(set(result['scores']), {str(i) for i in range(6)})
            self.assertTrue(all(0 <= value <= 1 for value in result['scores'].values()))

    def test_training_rejects_nontraining_and_schema_leakage(self):
        manifest = make_manifest()
        row = {'id': '0', 'split': 'test', 'label': 1,
               'features': {'reachability': 1.0, 'freshness': 1.0, 'operation_kind': 'read'}}
        with self.assertRaisesRegex(ValueError, 'training'):
            challenger._rows([row], manifest, training=True)


if __name__ == '__main__': unittest.main()
