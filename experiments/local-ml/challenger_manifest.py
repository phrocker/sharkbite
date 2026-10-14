#!/usr/bin/env python3
"""Provider-neutral manifests for bounded challenger-model experiments.

This module registers the experiment before fitting. It does not train models,
select a winner, calibrate scores, or authorize serving/exclusion.
"""
import hashlib
import json


SCHEMA = 1
CHALLENGERS = ('catboost-structured-v1', 'unixcoder-linear-v1')
FORBIDDEN_FEATURE_TERMS = ('label', 'finding', 'pr_id', 'post_cutoff', 'outcome', 'future')


def _digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(',', ':'), allow_nan=False).encode()).hexdigest()


def seal(kind, **fields):
    record = dict(schema=SCHEMA, kind=kind, **fields)
    return dict(record, id=_digest(record))


def _text(value, name):
    if not isinstance(value, str) or not value.strip() or len(value) > 512:
        raise ValueError('invalid ' + name)


def _digest_text(value, name):
    _text(value, name)
    if len(value) != 64 or any(char not in '0123456789abcdef' for char in value):
        raise ValueError('invalid ' + name + ' digest')


def _name_list(value, name, maximum):
    if (not isinstance(value, list) or not value or len(value) > maximum or
            any(not isinstance(item, str) or not item.strip() or len(item) > 512 for item in value) or
            len(set(value)) != len(value)):
        raise ValueError('invalid ' + name)


def build_manifest(task_id, dataset_id, split_policy_id, feature_schema_id, feature_names,
                   label_policy_id, heldout_horizon, primary_metrics, search_budget, recipes):
    _text(task_id, 'task_id'); _text(dataset_id, 'dataset_id'); _text(split_policy_id, 'split_policy_id')
    _text(feature_schema_id, 'feature_schema_id'); _text(label_policy_id, 'label_policy_id')
    _text(heldout_horizon, 'heldout_horizon')
    if not isinstance(feature_names, list) or not feature_names or len(feature_names) > 256 or any(not isinstance(x, str) or not x.strip() for x in feature_names):
        raise ValueError('invalid feature names')
    lowered = [name.lower() for name in feature_names]
    if len(set(lowered)) != len(lowered) or any(any(term in name for term in FORBIDDEN_FEATURE_TERMS) for name in lowered):
        raise ValueError('feature set includes forbidden label or future information')
    _name_list(primary_metrics, 'primary metrics', 16)
    if not isinstance(search_budget, dict) or type(search_budget.get('max_trials')) is not int or not 0 < search_budget['max_trials'] <= 100:
        raise ValueError('invalid search budget')
    if not isinstance(recipes, list) or len(recipes) != len(CHALLENGERS) or set(recipe.get('name') for recipe in recipes) != set(CHALLENGERS):
        raise ValueError('both challenger recipes are required exactly once')
    manifest = seal('challenger_experiment_manifest', task_id=task_id, dataset_id=dataset_id,
                    split_policy_id=split_policy_id, feature_schema_id=feature_schema_id,
                    feature_names=sorted(feature_names), label_policy_id=label_policy_id,
                    heldout_horizon=heldout_horizon, primary_metrics=primary_metrics,
                    search_budget=search_budget, recipes=recipes, optimization_enabled=False)
    validate(manifest)
    return manifest


def validate(manifest):
    if not isinstance(manifest, dict) or manifest.get('schema') != SCHEMA or manifest.get('kind') != 'challenger_experiment_manifest':
        raise ValueError('invalid challenger manifest kind')
    body = {key: value for key, value in manifest.items() if key != 'id'}
    if manifest.get('id') != _digest(body):
        raise ValueError('challenger manifest identity mismatch')
    if manifest.get('optimization_enabled') is not False:
        raise ValueError('challenger manifest enables optimization')
    for field in ('task_id', 'dataset_id', 'split_policy_id', 'feature_schema_id', 'label_policy_id', 'heldout_horizon'):
        _text(manifest.get(field), field)
    features = manifest.get('feature_names')
    if not isinstance(features, list) or not features or len(features) > 256 or len(set(features)) != len(features) or features != sorted(features):
        raise ValueError('invalid manifest feature names')
    lowered = [feature.lower() for feature in features]
    if any(any(term in feature for term in FORBIDDEN_FEATURE_TERMS) for feature in lowered):
        raise ValueError('manifest feature set includes forbidden label or future information')
    budget = manifest.get('search_budget')
    if not isinstance(budget, dict) or type(budget.get('max_trials')) is not int or not 0 < budget['max_trials'] <= 100:
        raise ValueError('invalid manifest search budget')
    _name_list(manifest.get('primary_metrics'), 'manifest primary metrics', 16)
    if len(manifest.get('recipes', [])) != len(CHALLENGERS) or set(recipe.get('name') for recipe in manifest.get('recipes', [])) != set(CHALLENGERS):
        raise ValueError('challenger recipe set mismatch')
    for recipe in manifest['recipes']:
        _text(recipe.get('name'), 'recipe name')
        _text(recipe.get('runtime_id'), 'runtime_id')
        _digest_text(recipe.get('weights_digest'), 'weights')
        _digest_text(recipe.get('tokenizer_digest'), 'tokenizer')
        _text(recipe.get('license_id'), 'license_id')
        input_field = recipe.get('input_field', 'text')
        _text(input_field, 'input_field')
        if any(term in input_field.lower() for term in FORBIDDEN_FEATURE_TERMS):
            raise ValueError('recipe input field includes forbidden label or future information')
        if recipe['name'] == 'catboost-structured-v1':
            _text(recipe.get('library_version'), 'CatBoost library version')
            _name_list(recipe.get('categorical_features'), 'CatBoost categorical features', 256)
        if recipe['name'] == 'unixcoder-linear-v1':
            _text(recipe.get('model_repo'), 'UniXcoder model repo')
            _text(recipe.get('model_revision'), 'UniXcoder model revision')
            _text(recipe.get('tokenizer_revision'), 'UniXcoder tokenizer revision')
            if recipe['model_repo'] != 'microsoft/unixcoder-base':
                raise ValueError('UniXcoder recipe must use microsoft/unixcoder-base')
            for field in ('model_revision', 'tokenizer_revision'):
                revision = recipe[field]
                if len(revision) != 40 or any(char not in '0123456789abcdef' for char in revision):
                    raise ValueError('UniXcoder revision must be an immutable SHA')
            if type(recipe.get('max_context_tokens')) is not int or not 0 < recipe['max_context_tokens'] <= 32768:
                raise ValueError('invalid UniXcoder context bound')
            if recipe.get('chunking') not in ('reject', 'fixed_nonoverlap'):
                raise ValueError('invalid UniXcoder chunking policy')
    return manifest
