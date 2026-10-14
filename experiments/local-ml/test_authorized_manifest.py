# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
import copy
import hashlib
import json
from pathlib import Path
import struct
import tempfile
import unittest
from unittest import mock

import authorized_manifest as admission
import dataset
import train
from test_train import fixture


def exported_fixture():
    data = fixture()
    data['provenance'] = 'trusted-export'
    data['rows'] = data['rows'][:3]
    rows = []
    for index, row in enumerate(data['rows']):
        receipt = 'adjudication-receipt:' + hashlib.sha256(str(index).encode()).hexdigest()
        source = {'ArtifactID': 'artifact', 'ID': 'source', 'RevisionID': 'revision',
                  'Digest': row['content_sha256'], 'OriginID': 'origin',
                  'AuthorityPolicyID': 'authority', 'AttestationID': '',
                  'Role': 'observation', 'Control': 'candidate_controlled',
                  'ObservedAt': row['observed_at'], 'ReceivedAt': row['received_at']}
        rows.append({'id': row['id'], 'request_id': 'request', 'prediction_id': 'prediction',
                     'picture_id': 'picture', 'target_id': f'target-{index}',
                     'selected_receipt_id': receipt, 'selected_version': 1,
                     'selected_basis_id': 'basis', 'selected_label_received_at': row['label_received_at'],
                     'current_head_id': receipt, 'current_head_version': 1,
                     'inventory_id': 'target-inventory', 'sources': [source],
                     'family_ids': [row['family']], 'feature_sha256': admission.feature_digest(row['features']),
                     'input_digest': 'a' * 64, 'split': row['split'], 'inclusion_probability': None,
                     'exclusion_reasons': [] if row['split'] == 'train' else ['split:' + row['split']]})
    raw = dataset.canonical_bytes(data)
    manifest = {'schema': 1, 'kind': 'authorized-numeric-training-export',
                'cohort_id': 'cohort', 'cohort_sha256': 'b' * 64,
                'authority_revision_id': 'authority-revision', 'inventory_id': 'cohort-inventory',
                'task_id': data['task_id'], 'question_id': data['question_id'],
                'label_policy_id': 'policy', 'training_purpose_id': 'purpose',
                'feature_schema_id': data['feature_schema_id'], 'feature_builder_id': 'builder',
                'cutoff': data['cutoff'], 'created_at': data['cutoff'],
                'provenance_mode': 'reconstructed', 'split_policy_id': 'split-policy',
                'sampling_policy_id': 'sample-policy', 'checkpoint_overlap': 'unknown',
                'dataset_sha256': hashlib.sha256(raw).hexdigest(), 'dataset_bytes': len(raw), 'rows': rows}
    return data, manifest


class AuthorizedManifestTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.data_path = self.root / 'dataset.json'
        self.manifest_path = self.root / 'manifest.json'
        self.data, self.manifest = exported_fixture()
        self.write()

    def write(self):
        raw = dataset.canonical_bytes(self.data)
        self.data_path.write_bytes(raw)
        self.manifest['dataset_sha256'] = hashlib.sha256(raw).hexdigest()
        self.manifest['dataset_bytes'] = len(raw)
        self.manifest_path.write_bytes(dataset.canonical_bytes(self.manifest))
        self.pin = hashlib.sha256(self.manifest_path.read_bytes()).hexdigest()

    def load(self):
        return admission.load(self.data_path, self.manifest_path, self.pin)

    def test_valid_fit_binds_receipt_and_cli(self):
        receipt = train.train(self.data_path, self.root / 'result',
                              authorized_manifest_path=self.manifest_path,
                              authorized_manifest_sha256=self.pin)
        self.assertEqual(receipt['fit_row_ids'], ['row-0', 'row-1'])
        binding = receipt['authorized_export']
        self.assertEqual(binding['manifest_sha256'], self.pin)
        for key in ('cohort_id', 'cohort_sha256', 'authority_revision_id', 'inventory_id',
                    'training_purpose_id', 'dataset_sha256', 'dataset_bytes',
                    'provenance_mode', 'checkpoint_overlap'):
            self.assertEqual(binding[key], self.manifest[key])
        self.assertEqual(receipt, json.loads((self.root / 'result/training-receipt.json').read_bytes()))
        runtime = json.loads((self.root / 'result/runtime.json').read_bytes())
        self.assertEqual(runtime['sources']['authorized_manifest.py'],
                         hashlib.sha256(Path(admission.__file__).read_bytes()).hexdigest())
        with mock.patch('sys.argv', ['train.py', '--dataset', str(self.data_path),
                                    '--output-dir', str(self.root / 'cli'),
                                    '--authorized-manifest', str(self.manifest_path),
                                    '--authorized-manifest-sha256', self.pin]), mock.patch('builtins.print'):
            train.main()
        self.assertTrue((self.root / 'cli/model.json').is_file())

    def test_both_pins_required_and_digest_checked_before_parse(self):
        for kwargs in ({'authorized_manifest_path': self.manifest_path},
                       {'authorized_manifest_sha256': self.pin}):
            with self.assertRaisesRegex(ValueError, 'required together'):
                train.train(self.data_path, self.root / 'result', **kwargs)
        self.manifest_path.write_bytes(b'not JSON')
        with mock.patch.object(admission, '_json', side_effect=AssertionError('parsed before pin')):
            with self.assertRaisesRegex(ValueError, 'SHA-256 mismatch'):
                self.load()
        self.assertFalse((self.root / 'result').exists())

    def test_dataset_pin_is_raw_bytes_not_semantic_json(self):
        self.data_path.write_bytes(self.data_path.read_bytes() + b'\n')
        with self.assertRaisesRegex(ValueError, 'raw dataset byte binding'):
            self.load()
        # An independently repinned manifest can authorize these precise bytes.
        raw = self.data_path.read_bytes()
        self.manifest['dataset_sha256'] = hashlib.sha256(raw).hexdigest()
        self.manifest['dataset_bytes'] = len(raw)
        encoded = dataset.canonical_bytes(self.manifest)
        self.manifest_path.write_bytes(encoded)
        self.pin = hashlib.sha256(encoded).hexdigest()
        self.assertEqual(self.load()[0], self.data)

    def test_each_input_read_once_and_fit_uses_verified_bytes(self):
        actual = admission._read
        reads = []
        def replace_after_read(path, limit):
            result = actual(path, limit)
            reads.append(Path(path))
            Path(path).write_bytes(b'substituted after read')
            return result
        with mock.patch.object(admission, '_read', side_effect=replace_after_read), \
                mock.patch.object(dataset, 'load_dataset', side_effect=AssertionError('dataset reread')):
            data, binding = self.load()
        self.assertEqual(data, self.data)
        self.assertEqual(reads, [self.manifest_path, self.data_path])
        self.assertEqual(binding['manifest_sha256'], self.pin)

    def test_strict_json_rejects_malformed_pinned_manifests(self):
        for raw in (b'{"schema":1,"schema":1}', b'{"x":NaN}', b'{"x":Infinity}',
                    b'{"x":1e999}', b'{"x":"\\ud800"}', b'{"x":"\xff"}',
                    b'[' * 17 + b'0' + b']' * 17, b'{} {}'):
            with self.subTest(raw=raw[:30]):
                self.manifest_path.write_bytes(raw)
                self.pin = hashlib.sha256(raw).hexdigest()
                with self.assertRaises(ValueError):
                    self.load()
        with mock.patch.object(admission, 'MAX_BYTES', 1):
            with self.assertRaisesRegex(ValueError, 'byte limit'):
                self.load()

    def test_metadata_and_membership_substitution(self):
        original = copy.deepcopy(self.manifest)
        mutations = [lambda m: m.update(task_id='other'), lambda m: m.update(schema=True),
                     lambda m: m.update(cohort_sha256='not a digest'),
                     lambda m: m.update(checkpoint_overlap='none'),
                     lambda m: m['rows'].pop(),
                     lambda m: m['rows'][0].update(id='unknown'),
                     lambda m: m['rows'][1].update(id=m['rows'][0]['id']),
                     lambda m: m['rows'][1].update(target_id=m['rows'][0]['target_id']),
                     lambda m: m['rows'][0].update(split='test'),
                     lambda m: m['rows'][0].update(feature_sha256='a' * 64),
                     lambda m: m['rows'][0].update(selected_basis_id=''),
                     lambda m: m['rows'][0].update(selected_version=True),
                     lambda m: m['rows'][0].update(current_head_version=2),
                     lambda m: m['rows'][0].update(exclusion_reasons=['label_withdrawn']),
                     lambda m: m['rows'][0].update(inclusion_probability=0),
                     lambda m: m['rows'][2].update(family_ids=m['rows'][0]['family_ids']),
                     lambda m: m['rows'][2]['sources'][0].update(Digest=m['rows'][0]['sources'][0]['Digest'])]
        for mutation in mutations:
            self.manifest = copy.deepcopy(original)
            mutation(self.manifest)
            self.write()
            with self.subTest(manifest=self.manifest):
                with self.assertRaises(ValueError):
                    self.load()

    def test_feature_hash_is_binary_and_preserves_signed_zero(self):
        values = [1, 1e-7, -0.0]
        expected = hashlib.sha256(b'shoal.numeric.features.v1\0' + struct.pack('<I3d', 3, *values)).hexdigest()
        self.assertEqual(admission.feature_digest(values), expected)
        self.assertNotEqual(admission.feature_digest([-0.0]), admission.feature_digest([0.0]))

    def test_go_negative_zero_json_preserves_feature_commitment(self):
        self.data['rows'][0]['features'][0] = -0.0
        self.manifest['rows'][0]['feature_sha256'] = admission.feature_digest(self.data['rows'][0]['features'])
        self.write()
        raw = self.data_path.read_bytes().replace(b'-0.0', b'-0')
        self.data_path.write_bytes(raw)
        self.manifest['dataset_sha256'] = hashlib.sha256(raw).hexdigest()
        self.manifest['dataset_bytes'] = len(raw)
        encoded = dataset.canonical_bytes(self.manifest)
        self.manifest_path.write_bytes(encoded)
        self.pin = hashlib.sha256(encoded).hexdigest()
        loaded, _ = self.load()
        self.assertEqual(struct.pack('<d', loaded['rows'][0]['features'][0]), struct.pack('<d', -0.0))

    def test_unknown_label_is_retained_and_excluded(self):
        row = self.data['rows'][2]
        row.update(label=None, label_status='unknown', label_received_at=self.data['cutoff'])
        self.manifest['rows'][2].update(selected_receipt_id='', selected_basis_id='',
                                       selected_version=0, selected_label_received_at=None,
                                       current_head_id='', current_head_version=0,
                                       exclusion_reasons=['no_label_at_cutoff', 'label_status:unknown', 'split:calibration'])
        self.write()
        loaded, _ = self.load()
        eligible, exclusions = dataset.eligible_training_rows(loaded)
        self.assertEqual([v['id'] for v in eligible], ['row-0', 'row-1'])
        self.assertIn('label_status:unknown', exclusions[0]['reasons'])


if __name__ == '__main__':
    unittest.main()
