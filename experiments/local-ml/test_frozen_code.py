# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
import copy
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import frozen_code as fc


class FrozenCodeTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.archive = fc.HELPERS / 'runs/operations-v9/evidence.tar.gz'
        cls.manifest = fc.HELPERS / 'runs/operations-v9/evidence-manifest.json'
        cls.evidence, cls.members = fc.read_evidence(cls.archive, cls.manifest)

    def records(self):
        return [copy.deepcopy(self.evidence[name][0]) for name in fc.MEMBERS]

    def test_archive_and_manifest_are_independently_checked(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'manifest.json'
            manifest = json.loads(self.manifest.read_text())
            manifest['files'][0]['sha256'] = '0' * 64
            path.write_text(json.dumps(manifest))
            with self.assertRaisesRegex(ValueError, 'member digest'):
                fc.read_evidence(self.archive, path)
            other = Path(directory) / 'archive.gz'
            other.write_bytes(b'not the pinned archive')
            manifest['archive_sha256'] = fc.train.sha256(other.read_bytes())
            path.write_text(json.dumps(manifest))
            with self.assertRaisesRegex(ValueError, 'archive digest'):
                fc.read_evidence(other, path)

    def test_duplicate_manifest_and_json_rejected(self):
        with self.assertRaisesRegex(ValueError, 'duplicate'):
            fc.strict_json(b'{"id":1,"id":2}')
        with tempfile.TemporaryDirectory() as directory:
            manifest = json.loads(self.manifest.read_text())
            manifest['files'].append(manifest['files'][0])
            path = Path(directory) / 'manifest.json'
            path.write_text(json.dumps(manifest))
            with self.assertRaisesRegex(ValueError, 'manifest member'):
                fc.read_evidence(self.archive, path)

    def test_substituted_selection_scores_and_authority_rejected(self):
        for mutate in (
            lambda records: records[2].update(model_id='other'),
            lambda records: records[2].update(threshold=0.5),
            lambda records: records[3]['predictions'][0].update(review_score=0.5),
            lambda records: records[3]['predictions'][0].update(action='skip_review'),
            lambda records: records[3].update(optimization_enabled=True),
            lambda records: records[0].update(recipe=['operations', 'linear_svm']),
        ):
            records = self.records()
            mutate(records)
            with self.assertRaises(ValueError):
                fc.validate_bindings(*records)

    def test_source_substitution_rejected(self):
        records = self.records()
        records[0]['evidence_contract']['runner_sha256'] = '0' * 64
        records[1]['evidence_contract'] = records[0]['evidence_contract']
        with self.assertRaisesRegex(ValueError, 'helper source'):
            fc.validate_bindings(*records)

    def test_full_export_preserves_all_frozen_decisions_and_is_exclusive(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / 'candidate'
            result = fc.export(self.archive, self.manifest, 'test-task', output)
            self.assertEqual(len(result['rows']), 188)
            self.assertEqual(sum(r['expected_label'] == 'lower_priority' for r in result['rows']), 47)
            self.assertFalse(result['provenance']['optimization_enabled'])
            self.assertFalse(result['provenance']['original_training_runtime_verified'])
            model = json.loads((output / 'model.json').read_text())
            self.assertEqual(len(model['coefficients']), 60000)
            for row in result['rows']:
                raw = (output / row['input_file']).read_bytes()
                self.assertEqual(fc.train.sha256(raw), row['input_sha256'])
                data = json.loads(raw)
                self.assertEqual(data['subjects'][0]['id'], row['id'])
            with self.assertRaises(FileExistsError):
                fc.export(self.archive, self.manifest, 'test-task', output)

    def test_replay_mismatch_publishes_nothing(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / 'candidate'
            with patch.object(fc.op, 'score', return_value={r['unit_id']: 0 for r in self.records()[1]['rows']}):
                with self.assertRaisesRegex(ValueError, 'score replay mismatch'):
                    fc.export(self.archive, self.manifest, 'test-task', output)
            self.assertEqual(list(Path(directory).iterdir()), [])


if __name__ == '__main__':
    unittest.main()
