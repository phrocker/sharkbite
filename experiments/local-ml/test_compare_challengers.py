import importlib.util
import tempfile
import unittest
from pathlib import Path

import challenger_manifest as contract
import compare_challengers
import verify_comparison


def manifest():
    cat = {'name': 'catboost-structured-v1', 'runtime_id': 'runtime-v1', 'weights_digest': 'a' * 64,
           'tokenizer_digest': 'b' * 64, 'license_id': 'license-v1', 'library_version': '1.2.8',
           'categorical_features': ['operation_kind']}
    unix = {'name': 'unixcoder-linear-v1', 'runtime_id': 'runtime-v1', 'weights_digest': 'a' * 64,
            'tokenizer_digest': 'b' * 64, 'license_id': 'license-v1', 'input_field': 'code',
            'model_repo': 'microsoft/unixcoder-base', 'model_revision': 'c' * 40,
            'tokenizer_revision': 'd' * 40, 'max_context_tokens': 512, 'chunking': 'reject'}
    return contract.build_manifest('task-v1', 'dataset-v1', 'split-v1', 'features-v1',
                                   ['reachability', 'freshness', 'operation_kind'], 'labels-v1',
                                   '2026-12-01T00:00:00Z', ['recall_at_budget'], {'max_trials': 1}, [cat, unix])


def rows():
    result = []
    for i in range(8):
        result.append({'id': str(i), 'split': 'train' if i < 4 else 'test', 'label': i % 2,
                       'code': 'removed authorization check' if i % 2 else 'comment-only change',
                       'features': {'reachability': float(i % 3), 'freshness': 1.0 - i / 10,
                                    'operation_kind': 'write' if i % 2 else 'read'}})
    return result


@unittest.skipUnless(importlib.util.find_spec('catboost'), 'optional CatBoost dependency is not installed')
class ComparisonTests(unittest.TestCase):
    def test_same_picture_comparison_is_measurement_only(self):
        data = rows(); value = manifest()
        embedder = lambda texts: [[float('authorization' in text), float('comment' in text), float(len(text))] for text in texts]
        with tempfile.TemporaryDirectory() as directory:
            report = compare_challengers.compare(data[:4], data[4:], value, embedder, directory)
            self.assertEqual(report['disposition'], 'measurement_only')
            self.assertEqual(set(report['metrics']), {'catboost', 'unixcoder'})
            self.assertEqual(report['train_ids'], ['0', '1', '2', '3'])
            self.assertEqual(report['test_ids'], ['4', '5', '6', '7'])


class ComparisonContractTests(unittest.TestCase):
    def test_overlapping_splits_rejected_without_optional_dependencies(self):
        data = rows(); value = manifest()
        with self.assertRaisesRegex(ValueError, 'disjoint'):
            compare_challengers.compare(data[:4], data[3:5], value, lambda _: [], tempfile.mkdtemp())

    def test_report_verifier_rejects_tampering(self):
        report = contract.seal(
            'challenger_comparison', manifest_id='a' * 64,
            train_ids=['0', '1'], test_ids=['2', '3'],
            candidates={'catboost': 'b' * 64, 'unixcoder': 'c' * 64},
            metrics={name: {'examples': 2, 'positive_examples': 1, 'accuracy': 1.0,
                            'positive_recall': 1.0, 'false_positive_rate': 0.0,
                            'threshold': 0.5, 'threshold_predeclared': True,
                            'predicted_positive': 1} for name in ('catboost', 'unixcoder')},
            disposition='measurement_only', optimization_enabled=False,
            limitation='Measurement only; no promotion or quality generalization.')
        self.assertTrue(verify_comparison.verify(report)['verified'])
        tampered = dict(report)
        tampered['test_ids'] = ['1', '2']
        with self.assertRaises(ValueError):
            verify_comparison.verify(tampered)

    def test_resealed_impossible_comparison_rejected(self):
        report = contract.seal(
            'challenger_comparison', manifest_id='a' * 64,
            train_ids=['0', '1'], test_ids=['2', '3'],
            candidates={'catboost': 'b' * 64, 'unixcoder': 'c' * 64},
            metrics={name: {'examples': 2, 'positive_examples': 2, 'accuracy': 1.0,
                            'positive_recall': 1.0, 'false_positive_rate': None,
                            'threshold': 0.5, 'threshold_predeclared': True,
                            'predicted_positive': 2} for name in ('catboost', 'unixcoder')},
            disposition='measurement_only', optimization_enabled=False,
            limitation='Measurement only; no promotion or quality generalization.')
        bad_identity = dict(report)
        bad_identity['candidates'] = dict(report['candidates'], unixcoder='b' * 64)
        bad_identity['id'] = contract._digest({key: value for key, value in bad_identity.items() if key != 'id'})
        with self.assertRaises(ValueError):
            verify_comparison.verify(bad_identity)
        bad_rate = dict(report)
        bad_rate['metrics'] = {name: dict(values, positive_recall=None)
                               for name, values in report['metrics'].items()}
        bad_rate['id'] = contract._digest({key: value for key, value in bad_rate.items() if key != 'id'})
        with self.assertRaises(ValueError):
            verify_comparison.verify(bad_rate)


if __name__ == '__main__': unittest.main()
