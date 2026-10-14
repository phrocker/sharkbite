# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
import copy
import hashlib
import json
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import dataset


def fixture():
    def row(index, split='train'):
        return {'id': str(index), 'family': f'family-{index}',
                'content_sha256': hashlib.sha256(str(index).encode()).hexdigest(),
                'source_revision': 'revision-1', 'observed_at': '2026-01-01T00:00:00Z',
                'received_at': '2026-01-02T00:00:00Z',
                'label_received_at': '2026-01-03T00:00:00Z',
                'label_status': 'verified', 'training_allowed': True,
                'split': split, 'features': [index, 1.0],
                'label': 'yes' if index % 2 else 'no'}
    return {'schema': 1, 'kind': 'numeric-training-dataset', 'task_id': 'task',
            'question_id': 'question', 'feature_schema_id': 'numeric-v1',
            'labels': ['no', 'yes'], 'cutoff': '2026-01-04T00:00:00Z',
            'provenance': 'synthetic', 'rows': [row(1), row(2), row(3, 'test')]}


class AdmissionTests(unittest.TestCase):
    def reject_row(self, key, value):
        data = fixture()
        data['rows'][0][key] = value
        with self.assertRaises(dataset.DatasetError):
            dataset.validate_dataset(data)

    def test_round_trip_and_canonical_encoding(self):
        data = fixture()
        raw = dataset.canonical_bytes(data)
        self.assertFalse(raw.endswith(b'\n'))
        self.assertEqual(raw, json.dumps(data, ensure_ascii=False, allow_nan=False,
                                        separators=(',', ':'), sort_keys=True).encode())
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'data.json'
            path.write_bytes(raw)
            self.assertEqual(dataset.load_dataset(path), data)

    def test_quarantine_and_holdout_never_fit(self):
        data = fixture()
        for index, status in enumerate(('unknown', 'disputed')):
            row = copy.deepcopy(data['rows'][0])
            row.update(id=f'q{index}', label_status=status, label=None)
            data['rows'].append(row)
        denied = copy.deepcopy(data['rows'][1])
        denied.update(id='denied', training_allowed=False)
        data['rows'].append(denied)
        eligible, exclusions = dataset.eligible_training_rows(data)
        self.assertEqual([row['id'] for row in eligible], ['1', '2'])
        self.assertEqual(exclusions, [
            {'id': '3', 'reasons': ['split:test']},
            {'id': 'denied', 'reasons': ['training_not_allowed']},
            {'id': 'q0', 'reasons': ['label_status:unknown']},
            {'id': 'q1', 'reasons': ['label_status:disputed']}])
        for status in ('unknown', 'disputed'):
            self.reject_row('label_status', status)
        self.reject_row('label', None)
        self.reject_row('label', 'injected')

    def test_cross_split_leakage_even_for_quarantined_rows(self):
        for field in ('family', 'content_sha256'):
            for split in ('calibration', 'validation', 'test'):
                with self.subTest(field=field, split=split):
                    data = fixture()
                    data['rows'][2].update(split=split, label_status='unknown', label=None,
                                           training_allowed=False)
                    data['rows'][2][field] = data['rows'][0][field]
                    with self.assertRaisesRegex(dataset.DatasetError, 'cross-split'):
                        dataset.validate_dataset(data)

    def test_temporal_leakage_and_timestamp_syntax(self):
        for key in ('received_at', 'label_received_at', 'observed_at'):
            self.reject_row(key, '2026-01-05T00:00:00Z')
        self.reject_row('label_received_at', '2025-12-31T00:00:00Z')
        for invalid in ('2026-02-30T00:00:00Z', '2026-01-01',
                        '2026-01-01T00:00:00-00:00', '2026-01-01T00:00:60Z',
                        '2026-01-01T00:00:00+01:00', '2026-01-01T00:00:00'):
            self.reject_row('observed_at', invalid)
        data = fixture()
        data['cutoff'] = '2026-01-04T00:00:00.000000001Z'
        data['rows'][0]['label_received_at'] = '2026-01-04T00:00:00.000000002Z'
        with self.assertRaises(dataset.DatasetError):
            dataset.validate_dataset(data)
        data['rows'][0]['label_received_at'] = '2026-01-04T00:00:00.0000000010+00:00'
        dataset.validate_dataset(data)

    def test_exact_keys_and_types(self):
        for where in ('dataset', 'row'):
            for mode in ('extra', 'missing'):
                data = fixture()
                target = data if where == 'dataset' else data['rows'][0]
                if mode == 'extra':
                    target['injected'] = True
                else:
                    target.pop(next(iter(target)))
                with self.assertRaises(dataset.DatasetError):
                    dataset.validate_dataset(data)
        for key, value in [('training_allowed', 1), ('training_allowed', 'true'),
                           ('id', ''), ('family', ' '), ('source_revision', None),
                           ('content_sha256', 'A' * 64), ('split', []),
                           ('label_status', {}), ('features', [True, 1]),
                           ('features', ['1', 1]), ('features', [float('nan'), 1]),
                           ('features', [float('inf'), 1]), ('features', [10**1000, 1]),
                           ('features', []), ('features', [1]), ('id', '\ud800'),
                           ('id', 'x' * 1025)]:
            with self.subTest(key=key, value=repr(value)[:50]):
                self.reject_row(key, value)
        for key, value in [('schema', True), ('schema', 1.0), ('labels', ['x', 'x']),
                           ('labels', ['x', None]), ('rows', []), ('provenance', 'api')]:
            data = fixture()
            data[key] = value
            with self.assertRaises(dataset.DatasetError):
                dataset.validate_dataset(data)
        self.reject_row('id', '2')

    def test_numeric_and_count_bounds(self):
        for constant, limit in [('MAX_ROWS', 2), ('MAX_FEATURES', 1),
                                ('MAX_SCALARS', 5), ('MAX_BYTES', 5)]:
            with patch.object(dataset, constant, limit):
                with self.assertRaises(dataset.DatasetError):
                    dataset.validate_dataset(fixture())

    def test_feature_magnitude_bound_rejects_native_solver_hang_inputs(self):
        data = fixture()
        data['rows'][0]['features'] = [-1_000_000, 1_000_000.0]
        original = copy.deepcopy(data)
        self.assertEqual(dataset.validate_dataset(data), original)
        for value in (-1e154, 1e154, -1_000_001, 1_000_001):
            with self.subTest(value=value):
                data['rows'][0]['features'] = [value, 1.0]
                with self.assertRaisesRegex(dataset.DatasetError,
                                            'versioned external feature transform'):
                    dataset.validate_dataset(data)
                self.assertEqual(data['rows'][0]['features'][0], value)

    def test_json_parser_attacks_and_bounded_read(self):
        raw = dataset.canonical_bytes(fixture())
        attacks = [raw.replace(b'"schema":1', b'"schema":1,"schema":1'),
                   raw.replace(b'"id":"1"', b'"id":"1","id":"2"'),
                   raw + b'{}', b'\xff', raw.replace(b'[1,1.0]', b'[NaN,1.0]'),
                   raw.replace(b'[1,1.0]', b'[Infinity,1.0]'),
                   b'[' * 2000 + b']' * 2000]
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'data.json'
            for attack in attacks:
                path.write_bytes(attack)
                with self.assertRaises(dataset.DatasetError):
                    dataset.load_dataset(path)
            path.write_bytes(raw)
            with patch.object(dataset, 'MAX_BYTES', len(raw) - 1):
                with self.assertRaisesRegex(dataset.DatasetError, 'byte limit'):
                    dataset.load_dataset(path)


if __name__ == '__main__':
    unittest.main()
