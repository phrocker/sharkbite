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

POLICY = dict(name='fixture-evaluation-policy', split='test', positive_label='yes', minimum_recall=0.5,
              minimum_specificity=0.5, minimum_positives=1, minimum_families=1)
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
        self.policy = self.sealed_policy()

    def sealed_policy(self, **changes):
        policy = dict(POLICY)
        policy.update(changes)
        return seal_candidate.seal_policy(policy)

    def build(self, policy=None, **overrides):
        args = dict(manifest=self.manifest, pin=self.pin, receipt=self.receipt, predictions=self.predictions,
                    bundle_dir=self.bundle, exports=[(self.export_manifest, self.data)], policy_record=policy or self.policy)
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
            self.build(exports=[(self.export_manifest[:-1], self.data)])
        with self.assertRaises(distill.Refused):
            self.build(exports=[(self.export_manifest, self.data + b' ')])
        with self.assertRaises(distill.Refused):
            self.build(exports=[])
        with self.assertRaises(distill.Refused):
            self.build(exports=[(self.export_manifest, self.data)] * 2)
        with self.assertRaises(distill.Refused):
            self.build(evaluation_cohort='cohort-9')
        doubled = json.loads(self.export_manifest)
        doubled['rows'].append(doubled['rows'][-1])
        with self.assertRaises(distill.Refused):
            self.build(exports=[(json.dumps(doubled, sort_keys=True, separators=(',', ':')).encode(), self.data)])
        for changes in (dict(split=''), dict(minimum_recall=1.5), dict(minimum_specificity=0), dict(positive_label='maybe'),
                        dict(split='nope'), dict(minimum_positives=0)):
            with self.assertRaises(distill.Refused):
                self.build(policy=self.sealed_policy(**changes))
        tampered = dict(self.policy, minimum_positives=1, name='edited after sealing')
        with self.assertRaises(distill.Refused):
            self.build(policy=tampered)
        with self.assertRaises(distill.Refused):
            self.build(policy=dict(POLICY))  # unsealed

    def test_gates_and_leaks_hold(self):
        strict = self.sealed_policy(minimum_positives=5)
        records, _ = self.build(policy=strict)
        self.assertEqual(records['evaluation']['disposition'], 'hold')
        self.assertIn('sample gate', records['evaluation']['reasons'][0])
        self.assertEqual(records['evaluation']['policy']['minimum_positives'], 5)
        self.assertEqual(records['evaluation']['evaluation_policy_id'], strict['id'])
        policy, _ = seal_candidate.decode_policy_record(self.policy)
        model = seal_candidate.decode_student((self.bundle / 'model.json').read_bytes())
        exports = [(self.export_manifest, self.data)]
        leaked = seal_candidate.evaluate(model, exports, 'cohort-1', ['row-4', 'row-0'], None, policy)
        self.assertEqual(leaked['disposition'], 'hold')
        self.assertIn('membership overlap: row-4', ';'.join(leaked['reasons']))
        self.assertEqual(leaked['comparisons'], [])
        # A constant student clears recall or specificity, never both.
        always_yes = dict(model, coefficients=[0] * len(model['coefficients']), intercept=100, threshold=0)
        yes = seal_candidate.evaluate(always_yes, exports, 'cohort-1', [], None, policy)
        self.assertEqual(yes['disposition'], 'hold')
        self.assertIn('specificity below', ';'.join(yes['reasons']))
        self.assertEqual(yes['metrics'][0]['false_positive_ids'], ['row-5'])
        # A training row of another cohort carrying the reserved row's source content is a leak.
        export = json.loads(self.export_manifest)
        data = json.loads(self.data)
        row4 = next(r for r in export['rows'] if r['id'] == 'row-4')
        data4 = next(r for r in data['rows'] if r['id'] == 'row-4')
        other_data = dict(data, rows=[dict(data4, id='b-row-9', split='train')])
        other_data_raw = json.dumps(other_data, sort_keys=True, separators=(',', ':')).encode()
        other_export = dict(export, cohort_id='cohort-2', dataset_sha256=hashlib.sha256(other_data_raw).hexdigest(),
                            rows=[dict(row4, id='b-row-9', family_ids=['family-b'], exclusion_reasons=[])])
        other_export_raw = json.dumps(other_export, sort_keys=True, separators=(',', ':')).encode()
        training = ['row-0', 'row-1', 'row-2', 'b-row-9']
        self.assertEqual(seal_candidate.evaluate(model, exports, 'cohort-1', training, None, policy)['disposition'], 'shadow_candidate')
        seen = seal_candidate.evaluate(model, exports + [(other_export_raw, other_data_raw)], 'cohort-1', training, None, policy)
        self.assertEqual(seen['disposition'], 'hold')
        self.assertIn('content overlap: row-4', ';'.join(seen['reasons']))

    def test_cli_publishes_without_replacing(self):
        serving = self.root / 'serving'
        out = self.root / 'sealed'
        argv = ['--manifest', str(self.root / 'manifest.json'), '--manifest-pin', self.pin,
                '--receipt', str(self.root / 'job' / 'distillation-receipt.json'),
                '--predictions', str(self.root / 'job' / 'teacher-predictions.json'), '--bundle-dir', str(self.bundle),
                '--export-manifest', str(self.root / 'export-manifest.json'), '--dataset', str(self.root / 'dataset.json'),
                '--policy', str(self.root / 'policy.json'), '--output-dir', str(out), '--serving-root', str(serving)]
        (self.root / 'policy.json').write_text(distill.canonical(self.policy))
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

    def test_publish_stages_privately_and_tolerates_a_concurrent_publisher(self):
        serving = self.root / 'serving'
        _, digest = weights_bundle.verify(self.bundle)
        # Another publisher's staging directory for the same digest is not
        # touched, and a loser of the rename finds the same bundle present.
        serving.mkdir()
        foreign = serving / ('.staging-' + digest)
        foreign.mkdir()
        (foreign / 'half.json').write_text('{}')
        self.assertEqual(seal_candidate.publish(self.bundle, serving, digest), 'published')
        self.assertTrue((foreign / 'half.json').exists())
        self.assertEqual(seal_candidate.publish(self.bundle, serving, digest), 'present')
        leftovers = [p for p in serving.iterdir() if p.name.startswith('.staging-') and p != foreign]
        self.assertEqual(leftovers, [])


if __name__ == '__main__':
    unittest.main()
