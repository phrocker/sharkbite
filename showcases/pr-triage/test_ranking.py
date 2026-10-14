# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
import unittest
from ranking import evaluate, select_threshold, model_scores


class RankingTest(unittest.TestCase):
    def test_missing_inputs_retain_and_ties_stay_together(self):
        scores = {'a': None, 'b': .1, 'c': .1, 'd': .8}
        labels = {'a': 'review', 'b': 'review', 'c': 'routine', 'd': 'review'}
        selected = select_threshold(scores, labels, .95)
        self.assertEqual(selected['proposed_dropped'], 0)
        result = evaluate(scores, labels, .2)
        self.assertEqual(result['dropped_ids'], ['b', 'c'])
        self.assertEqual(result['relevance_recall'], 2/3)

    def test_unknown_reference_is_not_a_free_negative(self):
        scores = {'a': .1, 'b': .8}
        labels = {'a': 'unknown', 'b': 'review'}
        self.assertEqual(select_threshold(scores, labels, .95)['proposed_dropped'], 0)
        self.assertEqual(evaluate(scores, labels, .2)['dropped_unknown_ids'], ['a'])

    def test_invalid_coverage_and_values_rejected(self):
        with self.assertRaises(ValueError):
            evaluate({'a': .2}, {}, .5)
        with self.assertRaises(ValueError):
            select_threshold({'a': .2}, {'a': 'routine'}, .95)
        with self.assertRaises(ValueError):
            model_scores({'predictions': []}, {'a': {}})


if __name__ == '__main__':
    unittest.main()
