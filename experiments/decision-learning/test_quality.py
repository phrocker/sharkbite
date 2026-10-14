import copy
import unittest

import quality


class QualityReportTests(unittest.TestCase):
    def fixture(self, sample=None):
        labels = {'a': 1, 'b': 1, 'c': 0, 'd': 0, 'u': 'unknown', 'x': 'disputed'}
        scores = {'a': .9, 'b': .2, 'c': .1, 'd': .8, 'u': .4, 'x': .6}
        groups = {key: ('family-' + key) for key in labels}
        return labels, scores, groups, sample or {'kind': 'uniform', 'population': 6, 'split': 'test', 'predeclared': True, 'future_inputs': False}

    def test_metrics_have_explicit_denominators_calibration_and_uncertainty(self):
        labels, scores, groups, sample = self.fixture()
        report = quality.evaluate(labels, scores, groups, sample=sample)
        quality.verify(report)
        self.assertEqual(report['counts']['resolved'], 4)
        self.assertEqual(report['counts']['unknown'], 1)
        self.assertEqual(report['counts']['disputed'], 1)
        self.assertEqual(report['counts']['true_positive'], 1)
        self.assertEqual(report['metrics']['recall'], .5)
        self.assertEqual(len(report['metrics']['recall_wilson_95']), 2)
        self.assertFalse(report['promotion_eligible'])
        self.assertFalse(report['population_claim'])

    def test_targeted_sample_cannot_make_population_claim(self):
        sample = {'kind': 'targeted', 'population': 100, 'inclusion_probability': .1,
                  'split': 'test', 'predeclared': True, 'future_inputs': False}
        labels, scores, groups, _ = self.fixture(sample)
        report = quality.evaluate(labels, scores, groups, sample=sample)
        self.assertFalse(report['population_claim'])

    def test_nonuniform_sampling_requires_inclusion_probability(self):
        sample = {'kind': 'random', 'population': 100, 'split': 'test', 'predeclared': True, 'future_inputs': False}
        labels, scores, groups, _ = self.fixture(sample)
        with self.assertRaises(ValueError):
            quality.evaluate(labels, scores, groups, sample=sample)

    def test_complete_resolved_uniform_cohort_can_claim_its_declared_population(self):
        labels, scores, groups, sample = self.fixture()
        labels = {key: value for key, value in labels.items() if isinstance(value, int)}
        scores = {key: scores[key] for key in labels}
        groups = {key: groups[key] for key in labels}
        sample = dict(sample, population=4)
        report = quality.evaluate(labels, scores, groups, sample=sample)
        self.assertTrue(report['population_claim'])

    def test_future_or_posthoc_samples_rejected(self):
        sample = self.fixture()[3]
        for mutation in ({'future_inputs': True}, {'predeclared': False}, {'split': 'validation'}):
            bad = dict(sample); bad.update(mutation)
            with self.assertRaises(ValueError):
                quality.evaluate(*self.fixture(bad)[:3], sample=bad)

    def test_bad_scores_and_tampered_receipts_rejected(self):
        labels, scores, groups, sample = self.fixture()
        scores = dict(scores); scores['a'] = float('nan')
        with self.assertRaises(ValueError):
            quality.evaluate(labels, scores, groups, sample=sample)
        labels, scores, groups, sample = self.fixture()
        report = quality.evaluate(labels, scores, groups, sample=sample)
        tampered = copy.deepcopy(report); tampered['counts']['resolved'] = 99
        with self.assertRaises(ValueError):
            quality.verify(tampered)

    def test_resealed_structurally_invalid_report_rejected(self):
        labels, scores, groups, sample = self.fixture()
        report = quality.evaluate(labels, scores, groups, sample=sample)
        mutations = []
        bad_counts = copy.deepcopy(report)
        bad_counts['counts']['resolved'] = 99
        mutations.append(bad_counts)
        bad_metrics = copy.deepcopy(report)
        bad_metrics['metrics']['accuracy'] = 2
        mutations.append(bad_metrics)
        bad_bins = copy.deepcopy(report)
        bad_bins['calibration'][0]['count'] = 0
        mutations.append(bad_bins)
        bad_digest = copy.deepcopy(report)
        del bad_digest['score_digest']
        mutations.append(bad_digest)
        bad_threshold = copy.deepcopy(report)
        bad_threshold['threshold'] = 2
        mutations.append(bad_threshold)
        for mutation in mutations:
            mutation['id'] = quality._digest({key: value for key, value in mutation.items() if key != 'id'})
            with self.assertRaises(ValueError):
                quality.verify(mutation)
        empty = copy.deepcopy(report)
        for key in empty['counts']:
            empty['counts'][key] = 0
        empty['id'] = quality._digest({key: value for key, value in empty.items() if key != 'id'})
        with self.assertRaises(ValueError):
            quality.verify(empty)

    def test_costs_are_bounded_and_recorded(self):
        labels, scores, groups, sample = self.fixture()
        report = quality.evaluate(labels, scores, groups, sample=sample, costs={'inference_seconds': 1.5, 'training_seconds': 2, 'label_seconds': 3})
        self.assertEqual(report['costs']['training_seconds'], 2.0)
        with self.assertRaises(ValueError):
            quality.evaluate(labels, scores, groups, sample=sample, costs={'inference_seconds': -1})


if __name__ == '__main__':
    unittest.main()
