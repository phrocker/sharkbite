import unittest
import challenger_manifest as contract
import unixcoder_challenger as challenger


def manifest():
    def recipe(name):
        base = {'name': name, 'runtime_id': 'runtime-v1', 'weights_digest': 'a' * 64,
                'tokenizer_digest': 'b' * 64, 'license_id': 'license-v1'}
        if name == 'catboost-structured-v1':
            base.update(library_version='1.2.8', categorical_features=['operation_kind'])
        else:
            base.update(input_field='code', model_repo='microsoft/unixcoder-base', model_revision='c' * 40,
                        tokenizer_revision='d' * 40, max_context_tokens=512, chunking='reject')
        return base
    return contract.build_manifest('task-v1', 'dataset-v1', 'split-v1', 'features-v1',
                                   ['reachability', 'freshness', 'operation_kind'], 'labels-v1',
                                   '2026-12-01T00:00:00Z', ['recall_at_budget'], {'max_trials': 1},
                                   [recipe('catboost-structured-v1'), recipe('unixcoder-linear-v1')])


class UnixCoderChallengerTests(unittest.TestCase):
    def test_device_guard_rejects_unavailable_cuda(self):
        with self.assertRaisesRegex(RuntimeError, 'cuda device is unavailable'):
            challenger._validate_device('cuda', False)

    def test_device_guard_accepts_available_cuda(self):
        challenger._validate_device('cuda', True)

    def test_device_guard_rejects_unknown_device(self):
        with self.assertRaisesRegex(ValueError, 'unsupported device'):
            challenger._validate_device('tpu', False)

    def test_fake_encoder_head_replays_without_network(self):
        rows = [{'id': str(i), 'split': 'train', 'label': i % 2, 'code': 'removed check' if i % 2 else 'comment'} for i in range(6)]
        def embed(texts):
            return [[float('removed check' in text), float('comment' in text), float(len(text))] for text in texts]
        value = manifest()
        artifact = challenger.train(rows, value, embed)
        result = challenger.predict([dict(row, split='test') for row in rows], value, artifact, embed)
        self.assertEqual(set(result['scores']), {str(i) for i in range(6)})
        self.assertTrue(all(0 <= score <= 1 for score in result['scores'].values()))

    def test_training_rejects_nontraining_rows(self):
        row = {'id': '0', 'split': 'test', 'label': 1, 'code': 'x'}
        with self.assertRaisesRegex(ValueError, 'training'):
            challenger._rows([row], manifest(), training=True)

    def test_head_rejects_nonfinite_embeddings(self):
        with self.assertRaisesRegex(ValueError, 'embedding'):
            challenger.fit_head([[float('nan')], [0.0]], [0, 1])


if __name__ == '__main__': unittest.main()
