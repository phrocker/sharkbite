import copy
import unittest

import calibration


class CalibrationTests(unittest.TestCase):
    def fixture(self):
        scores = {'a': .99, 'b': .8, 'c': .2, 'd': .01, 'e': .7, 'f': .3}
        labels = {'a': 1, 'b': 0, 'c': 0, 'd': 1, 'e': 1, 'f': 0}
        return scores, labels

    def test_validation_only_artifact_replays_on_disjoint_test_ids(self):
        scores, labels = self.fixture()
        artifact = calibration.fit(scores, labels, model_id='model-v1', runtime_id='runtime-v1', validation_manifest_id='manifest-v1')
        calibration.verify(artifact)
        result = calibration.apply(artifact, {'g': .9, 'h': .1}, model_id='model-v1')
        self.assertEqual(result['artifact_id'], artifact['id'])
        self.assertFalse(result['optimization_enabled'])

    def test_overlap_model_mismatch_and_bad_split_rejected(self):
        scores, labels = self.fixture()
        artifact = calibration.fit(scores, labels, model_id='model-v1', runtime_id='runtime-v1', validation_manifest_id='manifest-v1')
        with self.assertRaises(ValueError):
            calibration.apply(artifact, {'a': .5}, model_id='model-v1')
        with self.assertRaises(ValueError):
            calibration.apply(artifact, {'g': .5}, model_id='other')
        with self.assertRaises(ValueError):
            calibration.apply(artifact, {'g': .5}, model_id='model-v1', split='validation')

    def test_fit_rejects_one_class_nonfinite_and_tampered_artifact(self):
        scores, labels = self.fixture()
        with self.assertRaises(ValueError):
            calibration.fit(scores, {key: 1 for key in labels}, model_id='m', runtime_id='r', validation_manifest_id='manifest')
        bad_scores = dict(scores); bad_scores['a'] = float('nan')
        with self.assertRaises(ValueError):
            calibration.fit(bad_scores, labels, model_id='m', runtime_id='r', validation_manifest_id='manifest')
        artifact = calibration.fit(scores, labels, model_id='m', runtime_id='r', validation_manifest_id='manifest')
        tampered = copy.deepcopy(artifact); tampered['temperature'] = 20
        with self.assertRaises(ValueError):
            calibration.verify(tampered)

    def test_exact_probability_extrema_are_calibrated(self):
        artifact = calibration.fit({'a': 0.0, 'b': 1.0, 'c': .2, 'd': .8},
                                   {'a': 0, 'b': 1, 'c': 0, 'd': 1},
                                   model_id='m', runtime_id='r', validation_manifest_id='manifest')
        result = calibration.apply(artifact, {'e': 0.0, 'f': 1.0}, model_id='m')
        self.assertLessEqual(result['scores']['e'], .5)
        self.assertGreaterEqual(result['scores']['f'], .5)

    def test_resealed_provenance_and_limitation_rejected(self):
        scores, labels = self.fixture()
        artifact = calibration.fit(scores, labels, model_id='m', runtime_id='r', validation_manifest_id='manifest')
        for field, value in (('validation_scores_digest', 'bad'),
                             ('validation_labels_digest', '0' * 63 + 'z'),
                             ('limitation', 'promotion enabled')):
            tampered = copy.deepcopy(artifact)
            tampered[field] = value
            tampered['id'] = calibration._digest({key: val for key, val in tampered.items() if key != 'id'})
            with self.assertRaises(ValueError):
                calibration.verify(tampered)


if __name__ == '__main__':
    unittest.main()
