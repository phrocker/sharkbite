# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
import copy
import json
from pathlib import Path
import shutil
import tempfile
import unittest
from unittest.mock import patch

import frozen_service_bundle as bundle


class FrozenServiceBundleTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temp = tempfile.TemporaryDirectory()
        cls.addClassCleanup(cls.temp.cleanup)
        cls.root = Path(cls.temp.name)
        cls.archive = bundle.fc.HELPERS / 'runs/operations-v9/evidence.tar.gz'
        cls.evidence_manifest = bundle.fc.HELPERS / 'runs/operations-v9/evidence-manifest.json'
        cls.published = cls.root / 'published'
        cls.result = bundle.export(cls.archive, cls.evidence_manifest, 'test-task', cls.published)
        cls.evidence, _ = bundle.fc.read_evidence(cls.archive, cls.evidence_manifest)
        cls.numeric = json.loads((cls.published / 'numeric/manifest.json').read_text())

    def staged_export(self, archive, manifest, task, output):
        shutil.copytree(self.published / 'numeric', output)
        return copy.deepcopy(self.numeric)

    def test_full_export_hashes_and_exact_cached_bytes(self):
        raw = (self.published / 'source-manifest.json').read_bytes()
        manifest = json.loads(raw)
        self.assertEqual(bundle.fc.train.sha256(raw), self.result['source_manifest_sha256'])
        self.assertEqual(bundle.fc.train.sha256((self.published / 'numeric/manifest.json').read_bytes()),
                         self.result['manifest_sha256'])
        self.assertEqual(bundle.fc.train.sha256((self.published / 'numeric/model.json').read_bytes()),
                         self.result['model_sha256'])
        self.assertEqual(len(manifest['rows']), 188)
        self.assertFalse(manifest['historical_timestamps_verified'])
        self.assertFalse(manifest['optimization_enabled'])
        originals = self.evidence['reserved/inputs-frozen.json'][0]['rows']
        for row, original in zip(manifest['rows'], originals):
            content = (self.published / row['source_file']).read_bytes()
            self.assertEqual(content, original['text']['code'].encode('utf-8'))
            self.assertEqual(len(content), row['bytes'])
            self.assertEqual(bundle.fc.train.sha256(content), row['source_sha256'])
            self.assertEqual(row['id'], original['unit_id'])

    def test_existing_output_is_never_replaced(self):
        with self.assertRaises(FileExistsError):
            bundle.export(self.archive, self.evidence_manifest, 'test-task', self.published)
        self.assertTrue((self.published / 'source-manifest.json').is_file())

    def test_reordered_or_missing_numeric_rows_rejected(self):
        for mutation in (lambda n: n['rows'].reverse(), lambda n: n['rows'].pop()):
            numeric = copy.deepcopy(self.numeric)
            mutation(numeric)
            with self.assertRaisesRegex(ValueError, 'cohort or order'):
                bundle.source_records(self.evidence, numeric)

    def test_source_provenance_and_limit_rejected(self):
        numeric = copy.deepcopy(self.numeric)
        numeric['provenance']['original_inputs_id'] = 'substituted'
        with self.assertRaisesRegex(ValueError, 'provenance'):
            bundle.source_records(self.evidence, numeric)
        with patch.object(bundle, 'MAX_SOURCE_BYTES', 1):
            with self.assertRaisesRegex(ValueError, 'byte limit'):
                bundle.source_records(self.evidence, self.numeric)

    def test_publish_failure_leaves_no_bundle_or_staging(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / 'bundle'
            with patch.object(bundle.fc, 'export', side_effect=self.staged_export), \
                 patch.object(bundle.fc.train, 'publish_exclusive', side_effect=OSError('publish failure')):
                with self.assertRaisesRegex(OSError, 'publish failure'):
                    bundle.export(self.archive, self.evidence_manifest, 'test-task', output)
            self.assertEqual(list(Path(directory).iterdir()), [])

    def test_post_publish_sync_failure_keeps_bundle_and_reports_uncertainty(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / 'bundle'
            real_sync = bundle.fc.train.fsync_directory
            def sync(path):
                if Path(path) == Path(directory):
                    raise OSError('parent sync failure')
                real_sync(path)
            with patch.object(bundle.fc, 'export', side_effect=self.staged_export), \
                 patch.object(bundle.fc.train, 'fsync_directory', side_effect=sync):
                with self.assertRaises(bundle.fc.train.PublishedDurabilityError) as raised:
                    bundle.export(self.archive, self.evidence_manifest, 'test-task', output)
            self.assertEqual(raised.exception.output_dir, str(output))
            self.assertTrue((output / 'source-manifest.json').is_file())
            self.assertEqual(list(Path(directory).iterdir()), [output])

    def test_nested_staging_sync_failure_is_not_reported_as_published_bundle(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / 'bundle'
            with patch.object(bundle.fc, 'export', side_effect=bundle.fc.train.PublishedDurabilityError('private', '0' * 64)):
                with self.assertRaisesRegex(RuntimeError, 'bundle was not published'):
                    bundle.export(self.archive, self.evidence_manifest, 'test-task', output)
            self.assertEqual(list(Path(directory).iterdir()), [])


if __name__ == '__main__':
    unittest.main()
