import unittest
import challenger_manifest as c


def recipe(name):
    base = {'name': name, 'runtime_id': 'runtime-v1', 'weights_digest': 'a' * 64,
            'tokenizer_digest': 'b' * 64, 'license_id': 'license-v1'}
    if name == 'catboost-structured-v1':
        base.update(library_version='1.2.8', categorical_features=['operation_kind'])
    else:
        base.update(model_repo='microsoft/unixcoder-base', model_revision='c' * 40,
                    tokenizer_revision='d' * 40, max_context_tokens=512, chunking='reject')
    return base


def manifest(**overrides):
    values = dict(task_id='task-v1', dataset_id='dataset-v1', split_policy_id='family-v1',
                  feature_schema_id='structured-v1', feature_names=['reachability', 'freshness', 'operation_kind'],
                  label_policy_id='labels-v1', heldout_horizon='2026-12-01T00:00:00Z',
                  primary_metrics=['recall_at_budget', 'selective_risk'], search_budget={'max_trials': 4},
                  recipes=[recipe('catboost-structured-v1'), recipe('unixcoder-linear-v1')])
    values.update(overrides)
    return c.build_manifest(**values)


class ChallengerManifestTests(unittest.TestCase):
    def test_manifest_is_pinned_and_replayable(self):
        value = manifest()
        self.assertEqual(value['optimization_enabled'], False)
        self.assertEqual(c.validate(value), value)

    def test_forbidden_label_or_future_feature_rejected(self):
        with self.assertRaisesRegex(ValueError, 'forbidden'):
            manifest(feature_names=['reachability', 'post_cutoff_outcome'])

    def test_missing_challenger_or_artifact_pin_rejected(self):
        with self.assertRaisesRegex(ValueError, 'both challenger'):
            manifest(recipes=[recipe('catboost-structured-v1')])
        bad = recipe('unixcoder-linear-v1'); bad['weights_digest'] = 'not-a-digest'
        with self.assertRaisesRegex(ValueError, 'digest'):
            manifest(recipes=[recipe('catboost-structured-v1'), bad])

    def test_manifest_tampering_rejected(self):
        value = manifest(); value['search_budget']['max_trials'] = 100
        with self.assertRaisesRegex(ValueError, 'identity'):
            c.validate(value)

    def test_resealed_malformed_recipe_and_metric_metadata_rejected(self):
        value = manifest()
        for mutation in (
                lambda item: item.update(feature_names=['reachability', 'freshness', 'operation_kind']),
                lambda item: item.update(primary_metrics=['']),
                lambda item: item['recipes'][0].update(categorical_features=['operation_kind', 'operation_kind']),
                lambda item: item['recipes'][0].update(categorical_features=[1])):
            bad = manifest()
            mutation(bad)
            bad['id'] = c._digest({key: item for key, item in bad.items() if key != 'id'})
            with self.assertRaises(ValueError):
                c.validate(bad)


if __name__ == '__main__': unittest.main()
