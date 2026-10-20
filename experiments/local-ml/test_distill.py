# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
import copy
import hashlib
import json
import os
import platform
import sys
import tempfile
import unittest
from pathlib import Path

import authorized_manifest as admission
import dataset
import distill
import weights_bundle


def export_fixture(cohort='cohort-1', task='synthetic-task', policy='policy'):
    rows = []
    for i, (split, label, x) in enumerate([("train", "no", -2), ("train", "yes", 2), ("train", "no", -3), ("calibration", "no", 100)]):
        rows.append({"id": f"row-{i}", "family": f"family-{i}", "content_sha256": hashlib.sha256(str(i).encode()).hexdigest(),
                     "source_revision": "synthetic-v1", "observed_at": "2026-01-01T00:00:00Z", "received_at": "2026-01-01T00:00:00Z",
                     "label_received_at": "2026-01-01T00:00:00Z", "label_status": "verified", "training_allowed": True,
                     "split": split, "features": [x, 1], "label": label})
    data = {"schema": 1, "kind": "numeric-training-dataset", "task_id": task, "question_id": "synthetic-question",
            "feature_schema_id": "numeric-v1", "labels": ["no", "yes"], "cutoff": "2026-01-02T00:00:00Z",
            "provenance": "trusted-export", "rows": rows}
    manifest_rows = []
    for index, row in enumerate(rows):
        receipt = 'adjudication-receipt:' + hashlib.sha256(str(index).encode()).hexdigest()
        source = {'ArtifactID': 'artifact', 'ID': 'source', 'RevisionID': 'revision', 'Digest': row['content_sha256'], 'OriginID': 'origin',
                  'AuthorityPolicyID': 'authority', 'AttestationID': '', 'Role': 'observation', 'Control': 'candidate_controlled',
                  'ObservedAt': row['observed_at'], 'ReceivedAt': row['received_at']}
        manifest_rows.append({'id': row['id'], 'request_id': 'request', 'prediction_id': 'prediction', 'picture_id': 'picture',
                              'target_id': f'target-{index}', 'selected_receipt_id': receipt, 'selected_version': 1,
                              'selected_basis_id': 'basis', 'selected_label_received_at': row['label_received_at'],
                              'current_head_id': receipt, 'current_head_version': 1, 'inventory_id': 'target-inventory',
                              'sources': [source], 'family_ids': [row['family']], 'feature_sha256': admission.feature_digest(row['features']),
                              'input_digest': 'a' * 64, 'split': row['split'], 'inclusion_probability': None,
                              'exclusion_reasons': [] if row['split'] == 'train' else ['split:' + row['split']]})
    raw = dataset.canonical_bytes(data)
    manifest = {'schema': 1, 'kind': 'authorized-numeric-training-export', 'cohort_id': cohort, 'cohort_sha256': 'b' * 64,
                'authority_revision_id': 'authority-revision', 'inventory_id': 'cohort-inventory', 'task_id': task,
                'question_id': 'synthetic-question', 'label_policy_id': policy, 'training_purpose_id': 'purpose',
                'feature_schema_id': 'numeric-v1', 'feature_builder_id': 'builder', 'cutoff': data['cutoff'], 'created_at': data['cutoff'],
                'provenance_mode': 'reconstructed', 'split_policy_id': 'split-policy', 'sampling_policy_id': 'sample-policy',
                'checkpoint_overlap': 'unknown', 'dataset_sha256': hashlib.sha256(raw).hexdigest(), 'dataset_bytes': len(raw), 'rows': manifest_rows}
    return raw, dataset.canonical_bytes(manifest)


def identity(provider, model, egress):
    return dict(provider=provider, model=model, version='v1', cache_identity=f'{provider}:{model}:v1', egress=egress)


def manifest_fixture(export_sha256, held_out=()):
    rule = dict(metric='dev_recall', direction='maximize', minimum=0.5, maximum=None)
    m = lambda v: [dict(metric='dev_recall', value=v, split='development', evidence_id='evidence:dev_recall')]
    return dict(schema=1, kind='distillation-manifest', training_manifest_sha256='1' * 64,
                teacher=dict(role='teacher', chosen='teacher-a', rule=rule, rejected=[dict(alias='teacher-b', reason='lower development recall')],
                             eligible=[dict(alias='teacher-a', identity=identity('local', 'big-a', 'loopback'), measurements=m(0.9)),
                                       dict(alias='teacher-b', identity=identity('hosted', 'big-b', 'off_host'), measurements=m(0.8))]),
                base=dict(role='base', chosen='base-small', rule=rule, rejected=[],
                          eligible=[dict(alias='base-small', identity=identity('local', 'small', 'loopback'), measurements=m(0.6))]),
                teacher_egress_at_generation='loopback',
                data=dict(tasks=[dict(task_id='synthetic-task', cohort_ids=['cohort-1'], label_policy_ids=['policy'])],
                          evidence_families=['code'], prompt_version='prompt:1', generation_parameters=dict(epochs=3),
                          held_out_example_ids=sorted(held_out)),
                exports=[dict(cohort_id='cohort-1', manifest_sha256=export_sha256)])


class DistillTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        data, manifest = export_fixture()
        (self.root / 'dataset.json').write_bytes(data)
        (self.root / 'export-manifest.json').write_bytes(manifest)
        self.export_sha = hashlib.sha256(manifest).hexdigest()
        self.exports = {'cohort-1': (str(self.root / 'dataset.json'), str(self.root / 'export-manifest.json'))}
        self.executor = dict(id='worker-1', attestation_id='attest:1', host='test')

    def write_manifest(self, manifest, name='manifest.json', canonical=True):
        raw = distill.canonical(manifest).encode() if canonical else json.dumps(manifest, indent=1).encode()
        (self.root / name).write_bytes(raw)
        return str(self.root / name), hashlib.sha256(raw).hexdigest()

    def run_job(self, manifest=None, out='out', **kw):
        path, pin = self.write_manifest(manifest or manifest_fixture(self.export_sha))
        return distill.distill(path, pin, self.exports, self.root / out, executor=self.executor, **kw)

    @unittest.skipUnless(platform.system() == 'Linux', 'exclusive publication requires renameat2')
    def test_job_emits_verified_bundle_receipt_and_predictions_deterministically(self):
        first = self.run_job(out='first')
        second = self.run_job(out='second')
        self.assertEqual(first['bundle_digest'], second['bundle_digest'])
        self.assertEqual(sorted(os.listdir(self.root / 'first')), ['bundle', 'distillation-receipt.json', 'teacher-predictions.json'])
        bundle, digest = weights_bundle.verify(self.root / 'first' / 'bundle')
        self.assertEqual(digest, first['bundle_digest'])
        self.assertEqual({f['role'] for f in bundle['files']}, {'weights', 'runtime'})
        self.assertEqual((self.root / 'first/bundle/model.json').read_bytes(), (self.root / 'second/bundle/model.json').read_bytes())
        receipt = json.loads((self.root / 'first/distillation-receipt.json').read_bytes())
        self.assertEqual(receipt, first)
        self.assertTrue(receipt['candidate_only']); self.assertFalse(receipt['improvement_claimed'])
        self.assertEqual(receipt['status'], 'completed'); self.assertEqual(receipt['deviations'], [])
        self.assertEqual(receipt['requested_cohorts'], ['cohort-1'])
        self.assertEqual(receipt['actual_cohorts'], ['cohort-1'])
        self.assertEqual(receipt['held_out_excluded_count'], 0)
        self.assertEqual([e['cohort_id'] for e in receipt['authorized_exports']], ['cohort-1'])
        self.assertEqual(receipt['authorized_exports'][0]['task_id'], 'synthetic-task')
        self.assertEqual(receipt['trained_example_count'], 3)
        self.assertNotIn('recall', json.dumps(receipt))
        predictions = json.loads((self.root / 'first/teacher-predictions.json').read_bytes())
        self.assertEqual(receipt['teacher_predictions_id'], distill.digest(predictions))
        self.assertTrue(all(p['kind'] == 'prediction' for p in predictions['predictions']))
        self.assertEqual(predictions['teacher_egress'], 'loopback')
        model = json.loads((self.root / 'first/bundle/model.json').read_bytes())
        self.assertEqual(model['distillation_manifest_pin'], receipt['manifest_pin'])
        self.assertEqual(model['teacher'], 'teacher-a')
        with self.assertRaises(FileExistsError):
            self.run_job(out='first')

    @unittest.skipUnless(platform.system() == 'Linux', 'exclusive publication requires renameat2')
    def test_checkpoint_fine_tune_starts_from_the_pinned_base_bundle(self):
        first = self.run_job(out='base')
        base_dir = self.root / 'base' / 'bundle'
        tuned = manifest_fixture(self.export_sha)
        tuned['base']['eligible'][0]['identity']['bundle_digest'] = first['bundle_digest']
        tuned['data']['generation_parameters'] = dict(epochs=2, learning_rate=0.5)
        receipt = self.run_job(tuned, out='tuned', student='checkpoint-fine-tune', base_bundle_dir=str(base_dir))
        self.assertEqual(receipt['status'], 'completed')
        model = json.loads((self.root / 'tuned/bundle/model.json').read_bytes())
        self.assertEqual(model['base_bundle_digest'], first['bundle_digest'])
        self.assertEqual((model['epochs'], model['learning_rate']), (2, 0.5))
        self.assertEqual(len(model['coefficients']), 2)
        self.assertEqual((receipt['runtime_identities']['runtime_id'], receipt['runtime_identities']['student']), ('distill:checkpoint-fine-tune:v1', 'checkpoint-fine-tune'))
        self.assertEqual(first['runtime_identities']['student'], 'averaged-perceptron')
        # A manifest that pins a base may only run the student that opens it:
        # the receipt attests the base, so a run from zero weights is refused.
        with self.assertRaisesRegex(distill.Refused, 'would attest a base it never opened'):
            self.run_job(tuned, out='zero', student='averaged-perceptron')
        again = self.run_job(tuned, out='tuned-again', student='checkpoint-fine-tune', base_bundle_dir=str(base_dir))
        self.assertEqual(again['bundle_digest'], receipt['bundle_digest'])
        # The base is a verified input: a wrong digest, an altered bundle, a
        # missing pin, or no bundle at all refuses before any export is read.
        with self.assertRaisesRegex(distill.Refused, 'pins no bundle digest'):
            self.run_job(manifest_fixture(self.export_sha), out='x1', student='checkpoint-fine-tune', base_bundle_dir=str(base_dir))
        with self.assertRaisesRegex(distill.Refused, 'no --base-bundle'):
            self.run_job(tuned, out='x2', student='checkpoint-fine-tune')
        wrong = copy.deepcopy(tuned)
        wrong['base']['eligible'][0]['identity']['bundle_digest'] = '0' * 64
        with self.assertRaisesRegex(distill.Refused, 'not the manifest'):
            self.run_job(wrong, out='x3', student='checkpoint-fine-tune', base_bundle_dir=str(base_dir))
        altered = self.root / 'altered-base'
        import shutil
        shutil.copytree(base_dir, altered)
        (altered / 'model.json').write_bytes(b'{}')
        with self.assertRaisesRegex(distill.Refused, 'does not verify.*altered model.json'):
            self.run_job(tuned, out='x4', student='checkpoint-fine-tune', base_bundle_dir=str(altered))
        with self.assertRaisesRegex(distill.Refused, 'does not verify'):
            self.run_job(tuned, out='x5', student='checkpoint-fine-tune', base_bundle_dir=str(self.root / 'nowhere'))
        # A base of the wrong width is refused before the teacher runs.
        marker = self.root / 'teacher-ran'
        wide = self.root / 'wide-base'
        wide.mkdir()
        wide_model = dict(json.loads((base_dir / 'model.json').read_bytes()), coefficients=[0.0, 0.0, 0.0])
        (wide / 'model.json').write_bytes(weights_bundle.canonical(wide_model))
        (wide / 'runtime.json').write_bytes((base_dir / 'runtime.json').read_bytes())
        wide_manifest = weights_bundle.manifest_for(wide, weights_bundle.SERVING_FILES)
        (wide / weights_bundle.MANIFEST).write_bytes(weights_bundle.canonical(wide_manifest))
        wide_pin = copy.deepcopy(tuned)
        wide_pin['base']['eligible'][0]['identity']['bundle_digest'] = weights_bundle.bundle_digest(wide_manifest)
        with self.assertRaisesRegex(distill.Refused, 'width 3 does not match'):
            self.run_job(wide_pin, out='x6', student='checkpoint-fine-tune', base_bundle_dir=str(wide),
                         teacher_cmd=[sys.executable, '-c', 'import pathlib,sys; pathlib.Path(%r).write_text("ran"); print("[]")' % str(marker)])
        self.assertFalse(marker.exists())
        # Non-finite or overflowing checkpoint numbers are refused, not crashed on.
        huge = self.root / 'huge-base'
        huge.mkdir()
        (huge / 'model.json').write_bytes(weights_bundle.canonical(dict(wide_model, coefficients=[10 ** 400, 1])))
        (huge / 'runtime.json').write_bytes((base_dir / 'runtime.json').read_bytes())
        huge_manifest = weights_bundle.manifest_for(huge, weights_bundle.SERVING_FILES)
        (huge / weights_bundle.MANIFEST).write_bytes(weights_bundle.canonical(huge_manifest))
        huge_pin = copy.deepcopy(tuned)
        huge_pin['base']['eligible'][0]['identity']['bundle_digest'] = weights_bundle.bundle_digest(huge_manifest)
        with self.assertRaisesRegex(distill.Refused, 'finite numeric'):
            self.run_job(huge_pin, out='x7', student='checkpoint-fine-tune', base_bundle_dir=str(huge))
        self.assertFalse(any((self.root / x).exists() for x in ('x1', 'x2', 'x3', 'x4', 'x5', 'x6', 'x7', 'zero')))

    @unittest.skipUnless(platform.system() == 'Linux', 'exclusive publication requires renameat2')
    def test_changed_selection_changes_pin_and_bundle_lineage(self):
        first = self.run_job(out='a')
        changed = manifest_fixture(self.export_sha)
        changed['data']['prompt_version'] = 'prompt:2'
        second = self.run_job(changed, out='b')
        self.assertNotEqual(first['manifest_pin'], second['manifest_pin'])
        self.assertNotEqual(first['bundle_digest'], second['bundle_digest'])

    @unittest.skipUnless(platform.system() == 'Linux', 'exclusive publication requires renameat2')
    def test_actual_parameters_deviation_is_recorded(self):
        receipt = self.run_job(parameters=dict(epochs=5))
        self.assertEqual(receipt['actual_parameters'], dict(epochs=5))
        self.assertEqual(receipt['deviations'], [dict(parameter='epochs', requested=3, actual=5)])
        with self.assertRaisesRegex(distill.Refused, 'was not requested'):
            self.run_job(out='claim', parameters=dict(held_out_recall=0.97))
        with self.assertRaisesRegex(distill.Refused, 'positive integer'):
            self.run_job(out='zero', parameters=dict(epochs=0))
        with self.assertRaisesRegex(distill.Refused, 'no greater than'):
            self.run_job(out='many', parameters=dict(epochs=distill.MAX_EPOCHS + 1))

    def test_refusals_happen_before_training(self):
        good = manifest_fixture(self.export_sha)
        path, pin = self.write_manifest(good)
        with self.assertRaisesRegex(distill.Refused, 'pin does not match'):
            distill.distill(path, '0' * 64, self.exports, self.root / 'x', executor=self.executor)
        path, pin = self.write_manifest(good, 'loose.json', canonical=False)
        with self.assertRaisesRegex(distill.Refused, 'canonical'):
            distill.distill(path, pin, self.exports, self.root / 'x', executor=self.executor)
        cases = {
            'teacher outside eligible': (lambda m: m['teacher'].update(chosen='teacher-z'), 'not in the eligible set'),
            'teacher fails rule': (lambda m: m['teacher']['eligible'][0]['measurements'][0].update(value=0.1), 'does not satisfy the recorded rule'),
            'held-out evidence selected': (lambda m: m['base']['eligible'][0]['measurements'][0].update(split='held_out'), 'non-development evidence'),
            'declared egress': (lambda m: m.update(teacher_egress_at_generation='off_host'), 'not the resolved posture'),
            'unadmitted cohort': (lambda m: m['exports'].__setitem__(0, dict(cohort_id='cohort-9', manifest_sha256=self.export_sha)), 'admitted cohorts'),
            're-pinned export': (lambda m: m['exports'][0].update(manifest_sha256='c' * 64), 'SHA-256 mismatch'),
            'cohort not admitted for task': (lambda m: m['data']['tasks'][0].update(cohort_ids=['cohort-2']), 'not admitted'),
            'label policy not admitted': (lambda m: m['data']['tasks'][0].update(label_policy_ids=['other']), 'label policy'),
            'task outside selection': (lambda m: m['data']['tasks'][0].update(task_id='other-task'), 'outside the selection'),
            'held-out leak': (lambda m: m['data'].update(held_out_example_ids=['row-1']), 'contains held-out example'),
            'zero epochs pinned': (lambda m: m['data']['generation_parameters'].update(epochs=0), 'positive integer'),
            'unbounded epochs': (lambda m: m['data']['generation_parameters'].update(epochs=10 ** 9), 'no greater than'),
            'teacher not an object': (lambda m: m.update(teacher='x'), 'selection shape'),
            'candidate without alias': (lambda m: m['teacher']['eligible'][0].pop('alias'), 'candidate shape'),
            'measurement value a string': (lambda m: m['teacher']['eligible'][0]['measurements'][0].update(value='0.9'), 'measurement shape'),
            'rule bound a string': (lambda m: m['teacher']['rule'].update(minimum='0.5'), 'rule bounds'),
            'extra top-level key': (lambda m: m.update(note='x'), 'exactly the contract'),
            'cohort id not a string': (lambda m: m['data']['tasks'][0].update(cohort_ids=['cohort-1', 1]), 'task selection shape'),
            'held-out id a list': (lambda m: m['data'].update(held_out_example_ids=[['row-1']]), 'held-out ids'),
            'export pin not hex': (lambda m: m['exports'][0].update(manifest_sha256='zz'), 'exports shape'),
            'base bundle digest not hex': (lambda m: m['base']['eligible'][0]['identity'].update(bundle_digest='abc'), 'bundle digest'),
        }
        for name, (mutate, want) in cases.items():
            with self.subTest(name):
                manifest = copy.deepcopy(good)
                mutate(manifest)
                path, pin = self.write_manifest(manifest, name.replace(' ', '-') + '.json')
                with self.assertRaisesRegex(ValueError, want):
                    distill.distill(path, pin, self.exports, self.root / 'never', executor=self.executor)
                self.assertFalse((self.root / 'never').exists())
        self.assertFalse([p for p in self.root.iterdir() if p.name.startswith('.distill-staging-')])

    def test_teacher_outputs_are_predictions_only(self):
        manifest = dict(manifest_fixture(self.export_sha), _pin='p' * 64)
        with self.assertRaisesRegex(distill.Refused, 'teacher outputs are predictions'):
            distill.record_predictions(manifest, [dict(example_id='row-0', kind='reviewer_assessment', teacher='teacher-a', output={})])
        with self.assertRaisesRegex(distill.Refused, 'not the chosen teacher'):
            distill.record_predictions(manifest, [dict(example_id='row-0', kind='prediction', teacher='teacher-b', output={})])
        manifest['data']['held_out_example_ids'] = ['row-0']
        with self.assertRaisesRegex(distill.Refused, 'held-out'):
            distill.record_predictions(manifest, [dict(example_id='row-0', kind='prediction', teacher='teacher-a', output={})])
        manifest['data']['held_out_example_ids'] = []
        manifest['_labels'] = ['no', 'yes']
        with self.assertRaisesRegex(distill.Refused, 'outside the task labels'):
            distill.record_predictions(manifest, [dict(example_id='row-0', kind='prediction', teacher='teacher-a', output=dict(label='maybe'))])

    def test_manifest_that_is_not_an_object_is_refused_not_crashed(self):
        raw = distill.canonical([]).encode()
        (self.root / 'list.json').write_bytes(raw)
        with self.assertRaisesRegex(distill.Refused, 'not a distillation manifest'):
            distill.distill(str(self.root / 'list.json'), hashlib.sha256(raw).hexdigest(), self.exports, self.root / 'x', executor=self.executor)


if __name__ == '__main__':
    unittest.main()
