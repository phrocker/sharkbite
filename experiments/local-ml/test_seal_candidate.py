# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
import hashlib
import json
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

import distill
import seal_candidate
import weights_bundle
from test_distill import export_fixture, manifest_fixture

POLICY = dict(id='fixture-evaluation-policy', split='test', positive_label='yes', minimum_recall=0.5,
              minimum_positives=1, minimum_families=1)
TESTDATA = Path(__file__).resolve().parents[2] / 'internal' / 'distillation' / 'testdata'


class SealCandidateTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.data, self.export_manifest = export_fixture()
        (self.root / 'dataset.json').write_bytes(self.data)
        (self.root / 'export-manifest.json').write_bytes(self.export_manifest)
        export_sha = hashlib.sha256(self.export_manifest).hexdigest()
        self.manifest = manifest_fixture(export_sha, held_out=['row-99'])
        raw = distill.canonical(self.manifest).encode()
        (self.root / 'manifest.json').write_bytes(raw)
        self.pin = hashlib.sha256(raw).hexdigest()
        self.receipt = distill.distill(str(self.root / 'manifest.json'), self.pin,
                                       {'cohort-1': (str(self.root / 'dataset.json'), str(self.root / 'export-manifest.json'))},
                                       self.root / 'job', executor=dict(id='worker-1', attestation_id='attest:1', host='fixture-host'))
        self.predictions = json.loads((self.root / 'job' / 'teacher-predictions.json').read_bytes())
        self.bundle = self.root / 'job' / 'bundle'

    def build(self, policy=POLICY, **overrides):
        args = dict(manifest=self.manifest, pin=self.pin, receipt=self.receipt, predictions=self.predictions,
                    bundle_dir=self.bundle, export_raw=self.export_manifest, dataset_raw=self.data, policy=policy)
        args.update(overrides)
        return seal_candidate.build_candidate_set(**args)

    def test_seals_the_go_fixture_byte_for_byte(self):
        records, digest = self.build()
        self.assertEqual(records['evaluation']['disposition'], 'shadow_candidate')
        self.assertEqual(records['evaluation']['reasons'], [])
        self.assertEqual(records['artifact']['bundle_digest'], digest)
        expected = json.loads((TESTDATA / 'python-candidate-set.json').read_bytes())
        self.assertEqual(distill.canonical(records), distill.canonical(expected))

    def test_inputs_must_belong_together(self):
        with self.assertRaises(distill.Refused):
            self.build(pin='0' * 64)
        with self.assertRaises(distill.Refused):
            self.build(receipt=dict(self.receipt, bundle_digest='1' * 64))
        with self.assertRaises(distill.Refused):
            self.build(receipt=dict(self.receipt, status='failed'))
        with self.assertRaises(distill.Refused):
            self.build(predictions=dict(self.predictions, predictions=self.predictions['predictions'][:1]))
        with self.assertRaises(distill.Refused):
            self.build(export_raw=self.export_manifest[:-1])
        with self.assertRaises(distill.Refused):
            self.build(dataset_raw=self.data + b' ')
        for broken in (dict(POLICY, split=''), dict(POLICY, minimum_recall=1.5), dict(POLICY, positive_label='maybe'),
                       dict(POLICY, split='nope'), dict(POLICY, minimum_positives=0), {k: v for k, v in POLICY.items() if k != 'id'}):
            with self.assertRaises(distill.Refused):
                self.build(policy=broken)

    def test_gates_and_leaks_hold(self):
        records, _ = self.build(policy=dict(POLICY, minimum_positives=5))
        self.assertEqual(records['evaluation']['disposition'], 'hold')
        self.assertIn('sample gate', records['evaluation']['reasons'][0])
        model = seal_candidate.decode_student((self.bundle / 'model.json').read_bytes())
        leaked = seal_candidate.evaluate(model, self.data, self.export_manifest, ['row-4', 'row-0'], None, POLICY)
        self.assertEqual(leaked['disposition'], 'hold')
        self.assertIn('membership overlap: row-4', ';'.join(leaked['reasons']))
        self.assertEqual(leaked['comparisons'], [])

    def test_cli_publishes_without_replacing(self):
        serving = self.root / 'serving'
        out = self.root / 'sealed'
        argv = ['--manifest', str(self.root / 'manifest.json'), '--manifest-pin', self.pin,
                '--receipt', str(self.root / 'job' / 'distillation-receipt.json'),
                '--predictions', str(self.root / 'job' / 'teacher-predictions.json'), '--bundle-dir', str(self.bundle),
                '--export-manifest', str(self.root / 'export-manifest.json'), '--dataset', str(self.root / 'dataset.json'),
                '--policy', str(self.root / 'policy.json'), '--output-dir', str(out), '--serving-root', str(serving)]
        (self.root / 'policy.json').write_text(json.dumps(POLICY))
        first = subprocess.run([sys.executable, 'seal_candidate.py'] + argv, cwd=Path(__file__).parent, capture_output=True, text=True)
        self.assertEqual(first.returncode, 0, first.stderr)
        summary = json.loads(first.stdout)
        self.assertEqual(summary['bundle'], 'published')
        _, digest = weights_bundle.verify(serving / summary['bundle_digest'])
        self.assertEqual(digest, summary['bundle_digest'])
        second = subprocess.run([sys.executable, 'seal_candidate.py'] + argv, cwd=Path(__file__).parent, capture_output=True, text=True)
        self.assertEqual(json.loads(second.stdout)['bundle'], 'present')
        self.assertEqual(json.loads((out / 'candidate.json').read_text())['candidate']['id'], summary['candidate_id'])
        # An occupant under the digest that is not this bundle is refused.
        shutil.rmtree(serving / summary['bundle_digest'])
        (serving / summary['bundle_digest']).mkdir()
        (serving / summary['bundle_digest'] / 'bundle.json').write_text('{}')
        third = subprocess.run([sys.executable, 'seal_candidate.py'] + argv, cwd=Path(__file__).parent, capture_output=True, text=True)
        self.assertNotEqual(third.returncode, 0)


if __name__ == '__main__':
    unittest.main()
