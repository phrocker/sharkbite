# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
import importlib.util
import math
from pathlib import Path
import tempfile
import unittest

from local_model import load_bundle
from lexical import recipe_fingerprint, representation_fingerprint
from shadow import digest, write_new


@unittest.skipUnless(importlib.util.find_spec('sklearn'), 'requires optional pinned sklearn experiment environment')
class LocalModelTest(unittest.TestCase):
    def bundle(self):
        import sklearn
        return {'schema': 2, 'kind': 'char-wb-3-5-tfidf-logistic-C1-balanced',
                'representation_sha256': representation_fingerprint(),
                'recipe_id': recipe_fingerprint(),
                'representation': 'original_state-v1', 'sklearn_version': sklearn.__version__,
                'classes': [False, True], 'optimization_enabled': False, 'threshold': .5,
                'vocabulary': {'abc': 0, 'def': 1}, 'idf': [1., 1.], 'coef': [[1., -1.]], 'intercept': [0.]}

    def test_loaded_numeric_model_preserves_class_order(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)/'model.json'
            bundle = self.bundle(); bundle['id'] = digest(bundle); write_new(path, bundle)
            _, model = load_bundle(path)
            self.assertAlmostEqual(model.predict_proba(['abc'])[0, 1], 1/(1+math.exp(-1)))
            self.assertAlmostEqual(model.predict_proba(['def'])[0, 1], 1/(1+math.exp(1)))

    def test_tampering_malformed_shapes_and_nonfinite_parameters_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            for i, changes in enumerate(({'coef': [[float('nan'), 0.]]}, {'coef': [[1.]]},
                                         {'vocabulary': {'abc': 0, 'def': 0}}, {'classes': [True, False]},
                                         {'optimization_enabled': True}, {'representation_sha256': 'changed'},
                                         {'recipe_id': 'changed'})):
                bundle = {**self.bundle(), **changes}; bundle['id'] = digest(bundle)
                path = Path(directory)/f'{i}.json'; write_new(path, bundle)
                with self.assertRaises(ValueError): load_bundle(path)
            bundle = self.bundle(); bundle['id'] = digest(bundle); bundle['threshold'] = .6
            path = Path(directory)/'tampered.json'; write_new(path, bundle)
            with self.assertRaises(ValueError): load_bundle(path)


if __name__ == '__main__':
    unittest.main()
