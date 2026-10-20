# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
import json
import os
import platform
import tempfile
import unittest
from pathlib import Path

import weights_bundle as wb


class WeightsBundleTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)

    def candidate(self, name, intercept=0.0):
        d = self.root / name
        d.mkdir()
        for file, value in {'model.json': {'schema': 1, 'kind': 'linear-svm', 'intercept': intercept},
                            'recipe.json': {'kind': 'recipe'}, 'runtime.json': {'python': platform.python_version()},
                            'training-receipt.json': {'candidate_only': True}}.items():
            (d / file).write_bytes(wb.canonical(value))
        return d

    @unittest.skipUnless(platform.system() == 'Linux', 'exclusive publication requires renameat2')
    def test_same_candidate_stages_same_digest_and_output_is_never_replaced(self):
        first = self.candidate('first')
        m1, d1 = wb.stage(first, self.root / 'bundle-1')
        m2, d2 = wb.stage(first, self.root / 'bundle-2')
        self.assertEqual(d1, d2)
        self.assertEqual({f['path'] for f in m1['files']}, {'model.json', 'runtime.json'})
        self.assertEqual(sorted(os.listdir(self.root / 'bundle-1')), ['bundle.json', 'model.json', 'runtime.json'])
        self.assertEqual(wb.verify(self.root / 'bundle-1'), (m1, d1))
        with self.assertRaises(FileExistsError):
            wb.stage(first, self.root / 'bundle-1')
        self.assertFalse([p for p in self.root.iterdir() if p.name.startswith('.weights-bundle-staging-')])

    @unittest.skipUnless(platform.system() == 'Linux', 'exclusive publication requires renameat2')
    def test_verify_names_missing_extra_and_altered(self):
        wb.stage(self.candidate('c'), self.root / 'b')
        (self.root / 'b' / 'extra.bin').write_bytes(b'x')
        with self.assertRaisesRegex(ValueError, 'extra extra.bin'):
            wb.verify(self.root / 'b')
        (self.root / 'b' / 'extra.bin').unlink()
        (self.root / 'b' / 'model.json').write_bytes(b'{}')
        with self.assertRaisesRegex(ValueError, 'altered model.json'):
            wb.verify(self.root / 'b')
        (self.root / 'b' / 'model.json').unlink()
        with self.assertRaisesRegex(ValueError, 'missing model.json'):
            wb.verify(self.root / 'b')

    def test_compare_names_diverging_files(self):
        first = wb.manifest_for(self.candidate('a'), {'model.json': 'weights', 'runtime.json': 'runtime', 'recipe.json': 'preprocessing', 'training-receipt.json': 'calibration'})
        second = wb.manifest_for(self.candidate('b', intercept=1.0), {'model.json': 'weights', 'runtime.json': 'runtime', 'recipe.json': 'preprocessing', 'training-receipt.json': 'calibration'})
        result = wb.compare(first, second)
        self.assertFalse(result['reproducible'])
        self.assertEqual(result['diverging'], ['model.json'])
        self.assertTrue(wb.compare(first, first)['reproducible'])

    def test_manifest_refuses_strays_and_bad_roles(self):
        c = self.candidate('c')
        with self.assertRaisesRegex(ValueError, 'extra'):
            wb.manifest_for(c, {'model.json': 'weights'})
        with self.assertRaisesRegex(ValueError, 'missing'):
            wb.manifest_for(c, dict(wb.SERVING_FILES, **{'tokenizer.json': 'tokenizer', 'recipe.json': 'preprocessing', 'training-receipt.json': 'calibration'}))
        with self.assertRaisesRegex(ValueError, 'unknown role'):
            wb.manifest_for(c, {'model.json': 'weights', 'runtime.json': 'notes', 'recipe.json': 'preprocessing', 'training-receipt.json': 'calibration'})

    def test_digest_matches_decision_learning_prototype(self):
        # Same manifest, same digest as learning.bundle_digest in experiments/decision-learning.
        manifest = dict(schema=1, kind='weights-bundle', files=[dict(path='model.json', role='weights', size=10, sha256='a' * 64),
                                                               dict(path='runtime.json', role='runtime', size=5, sha256='b' * 64)])
        chain = json.loads((Path(__file__).resolve().parents[2] / 'internal/weightsbundle/testdata/python-chain.json').read_text())
        self.assertEqual(wb.bundle_digest(manifest), chain['bundle_digest'])


if __name__ == '__main__':
    unittest.main()
