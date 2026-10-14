import copy
import unittest

import influence


class InfluenceTests(unittest.TestCase):
    def test_leave_one_out_report_records_deltas_and_flips(self):
        baseline = {'a': .4, 'b': .8, 'c': .2}
        perturbed = {'a': .6, 'b': .7, 'c': .2}
        report = influence.measure(baseline, perturbed, baseline_model_id='m0', perturbed_model_id='m1',
                                   baseline_manifest_id='d0', perturbed_manifest_id='d1', removed_training_id='row-7')
        influence.verify(report)
        self.assertEqual(report['metrics']['decision_flips'], 1)
        self.assertEqual(report['metrics']['flipped_ids'], ['a'])
        self.assertFalse(report['unlearning_claim'])

    def test_coverage_and_artifact_substitution_rejected(self):
        baseline = {'a': .4, 'b': .8}
        with self.assertRaises(ValueError):
            influence.measure(baseline, {'a': .4}, baseline_model_id='m0', perturbed_model_id='m1',
                                     baseline_manifest_id='d0', perturbed_manifest_id='d1', removed_training_id='row')
        with self.assertRaises(ValueError):
            influence.measure(baseline, baseline, baseline_model_id='m0', perturbed_model_id='m0',
                              baseline_manifest_id='d0', perturbed_manifest_id='d1', removed_training_id='row')

    def test_bad_score_threshold_and_tampered_report_rejected(self):
        report = influence.measure({'a': .4}, {'a': .6}, baseline_model_id='m0', perturbed_model_id='m1',
                                   baseline_manifest_id='d0', perturbed_manifest_id='d1', removed_training_id='row')
        with self.assertRaises(ValueError):
            influence.measure({'a': float('nan')}, {'a': .6}, baseline_model_id='m0', perturbed_model_id='m1',
                              baseline_manifest_id='d0', perturbed_manifest_id='d1', removed_training_id='row')
        with self.assertRaises(ValueError):
            influence.measure({'a': .4}, {'a': .6}, baseline_model_id='m0', perturbed_model_id='m1',
                              baseline_manifest_id='d0', perturbed_manifest_id='d1', removed_training_id='row', threshold=2)
        tampered = copy.deepcopy(report); tampered['metrics']['decision_flips'] = 99
        with self.assertRaises(ValueError):
            influence.verify(tampered)

    def test_resealed_structurally_invalid_report_rejected(self):
        report = influence.measure({'a': .4, 'b': .6}, {'a': .4, 'b': .6},
                                   baseline_model_id='m0', perturbed_model_id='m1',
                                   baseline_manifest_id='d0', perturbed_manifest_id='d1', removed_training_id='row')
        report['metrics']['decision_flips'] = 1
        report['id'] = influence._digest({key: value for key, value in report.items() if key != 'id'})
        with self.assertRaises(ValueError):
            influence.verify(report)

    def test_score_digests_are_required_and_bound(self):
        baseline = {'a': .4}
        perturbed = {'a': .6}
        report = influence.measure(baseline, perturbed, baseline_model_id='m0', perturbed_model_id='m1',
                                   baseline_manifest_id='d0', perturbed_manifest_id='d1', removed_training_id='row')
        self.assertEqual(report['baseline_scores_digest'], influence._score_digest(baseline))
        self.assertEqual(report['perturbed_scores_digest'], influence._score_digest(perturbed))
        missing = copy.deepcopy(report)
        del missing['baseline_scores_digest']
        with self.assertRaises(ValueError):
            influence.verify(missing)
        malformed = copy.deepcopy(report)
        malformed['perturbed_scores_digest'] = '0' * 63 + 'z'
        with self.assertRaises(ValueError):
            influence.verify(malformed)


if __name__ == '__main__':
    unittest.main()
