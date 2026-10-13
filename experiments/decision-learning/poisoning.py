#!/usr/bin/env python3
"""Deterministic poisoning attacks against the offline learning boundary.

The corpus measures structural rejection and quarantine only. It does not
estimate model quality, truth, or the influence of an attack on weights.
"""
import argparse
import copy
import json
from pathlib import Path

import learning as l


CORPUS_ID = 'decision-poisoning-corpus-v2'
CORPUS_VERSION = 2
ATTACKS = (
    ('duplicate-example-flood', 'duplicate example identity', 'reject'),
    ('prediction-as-label', 'misrepresent a model prediction as an assessment', 'reject'),
    ('heldout-content-overlap', 'copy held-out content into training evidence', 'quarantine'),
    ('failed-shadow-evaluation', 'promote a candidate whose reserved evaluation fails', 'reject'),
    ('artifact-substitution', 'bind a candidate to a different model artifact', 'reject'),
    ('unauthorized-promotion', 'submit an unapproved active release', 'reject'),
    ('unauthorized-assessment', 'submit a label from an unlisted assessor', 'quarantine'),
    ('replayed-assessment-identity', 'reuse one assessment identity for multiple examples', 'reject'),
    ('targeted-training-trigger', 'inject a reserved evaluation example into training membership', 'reject'),
)


def reseal(record):
    return l.seal(record['kind'], **{key: value for key, value in record.items() if key not in ('id', 'kind', 'schema')})


def scenario():
    ledger = l.seal('ledger', task={'id': 'task-v1', 'labels': ['review', 'routine'], 'positive_label': 'review'},
                    pictures=[{'id': 'p', 'source_digest': 'source', 'builder_digest': 'builder', 'ontology_version': 'none',
                               'mode': 'reconstructed', 'coverage': [{'observed': 3, 'total': None}]}],
                    examples=[{'id': str(i), 'picture_id': 'p', 'family': str(i), 'content_digests': [str(i)]} for i in range(3)],
                    assessments=[{'id': 'a' + str(i), 'example_id': str(i), 'kind': 'reviewer_assessment', 'assessor': 'r',
                                  'status': 'proposed', 'label': 'review', 'evidence_digest': 'witness'} for i in range(3)])
    dataset_policy = l.seal('dataset_policy', task_id='task-v1', family_splits={'0': 'train', '1': 'test', '2': 'test'},
                             assessors=['r'], statuses=['proposed'])
    data = l.dataset(ledger, dataset_policy)
    evaluation_policy = l.seal('evaluation_policy', task_id='task-v1', evaluation_ids=['1', '2'], assessors=['r'],
                               statuses=['proposed'], minimum_recall=1., minimum_positive_count=2, minimum_families=2)
    candidate = l.seal('candidate', dataset_id=data['id'], evaluation_policy_id=evaluation_policy['id'],
                       training_ids=['0'], model_digest='model', policy_predeclared=True)
    predictions = l.seal('predictions', ledger_id=ledger['id'], candidate_id=candidate['id'],
                         rows=[{'id': i, 'proposal': 'retain', 'audit_priority': 0.5} for i in ['1', '2']])
    evaluation = l.evaluate(ledger, data, evaluation_policy, candidate, predictions)
    artifact = l.seal('candidate_artifact', candidate_id=candidate['id'], evaluation_id=evaluation['id'],
                      dataset_id=data['id'], model_digest='model', runtime_digest='runtime')
    approval = l.seal('promotion_approval', candidate_id=candidate['id'], evaluation_id=evaluation['id'],
                      owner='owner', approved=True)
    return locals()


def _rejected(fn):
    try:
        fn()
    except (KeyError, TypeError, ValueError):
        return True
    return False


def run():
    base = scenario()
    outcomes = []

    def duplicate_flood():
        ledger = copy.deepcopy(base['ledger'])
        ledger['examples'].append(copy.deepcopy(ledger['examples'][0]))
        l.dataset(reseal(ledger), base['dataset_policy'])

    def prediction_as_label():
        ledger = copy.deepcopy(base['ledger'])
        ledger['assessments'][0]['kind'] = 'prediction'
        l.dataset(reseal(ledger), base['dataset_policy'])

    def heldout_overlap():
        ledger = copy.deepcopy(base['ledger'])
        ledger['examples'][0]['content_digests'] = ['1']
        result = l.dataset(reseal(ledger), base['dataset_policy'])
        if not any(row['example_id'] == '0' and row['reason'] == 'heldout_content_overlap' for row in result['excluded']):
            raise ValueError('held-out overlap was not quarantined')

    def failed_shadow_evaluation():
        predictions = copy.deepcopy(base['predictions'])
        predictions['rows'][0]['proposal'] = 'lower_priority'
        predictions = reseal(predictions)
        evaluation = l.evaluate(base['ledger'], base['data'], base['evaluation_policy'], base['candidate'], predictions)
        artifact = l.seal('candidate_artifact', candidate_id=base['candidate']['id'], evaluation_id=evaluation['id'],
                          dataset_id=base['data']['id'], model_digest='model', runtime_digest='runtime')
        approval = l.seal('promotion_approval', candidate_id=base['candidate']['id'], evaluation_id=evaluation['id'],
                          owner='owner', approved=True)
        l.promote_candidate(base['candidate'], evaluation, artifact, approval)

    def artifact_substitution():
        artifact = l.seal('candidate_artifact', candidate_id=base['candidate']['id'], evaluation_id=base['evaluation']['id'],
                          dataset_id=base['data']['id'], model_digest='attacker-model', runtime_digest='runtime')
        l.promote_candidate(base['candidate'], base['evaluation'], artifact, base['approval'])

    def unauthorized_promotion():
        approval = l.seal('promotion_approval', candidate_id=base['candidate']['id'], evaluation_id=base['evaluation']['id'],
                          owner='owner', approved=False)
        l.promote_candidate(base['candidate'], base['evaluation'], base['artifact'], approval)

    def unauthorized_assessment():
        ledger = copy.deepcopy(base['ledger'])
        ledger['assessments'][0]['assessor'] = 'attacker'
        result = l.dataset(reseal(ledger), base['dataset_policy'])
        if not any(row['reason'] == 'unauthorized_assessment' for row in result['excluded']):
            raise ValueError('unauthorized assessment was not quarantined')

    def replayed_assessment_identity():
        ledger = copy.deepcopy(base['ledger'])
        ledger['assessments'][1]['id'] = ledger['assessments'][0]['id']
        l.dataset(reseal(ledger), base['dataset_policy'])

    def targeted_training_trigger():
        candidate = copy.deepcopy(base['candidate'])
        candidate['training_ids'].append('1')
        l.evaluate(base['ledger'], base['data'], base['evaluation_policy'],
                   reseal(candidate), base['predictions'])

    functions = [duplicate_flood, prediction_as_label, heldout_overlap, failed_shadow_evaluation,
                 artifact_substitution, unauthorized_promotion, unauthorized_assessment,
                 replayed_assessment_identity, targeted_training_trigger]
    for (name, description, expected), function in zip(ATTACKS, functions):
        if expected == 'quarantine':
            try:
                function()
            except (KeyError, TypeError, ValueError) as error:
                outcomes.append({'name': name, 'description': description, 'expected': expected, 'status': 'failed', 'error': str(error)})
            else:
                outcomes.append({'name': name, 'description': description, 'expected': expected, 'status': 'quarantined'})
        else:
            prevented = _rejected(function)
            outcomes.append({'name': name, 'description': description, 'expected': expected,
                             'status': 'rejected' if prevented else 'unsafe'})
    clean = scenario()
    clean['ledger']['id']  # force a clean-control access before reporting
    rejected = sum(row['status'] == 'rejected' for row in outcomes)
    quarantined = sum(row['status'] == 'quarantined' for row in outcomes)
    unsafe = sum(row['status'] == 'unsafe' for row in outcomes)
    failed = sum(row['status'] == 'failed' for row in outcomes)
    return l.seal('poisoning_attack_report', corpus_id=CORPUS_ID, corpus_version=CORPUS_VERSION,
                  clean_control={'status': 'accepted', 'scenario_id': clean['ledger']['id']}, attacks=outcomes,
                  denominators={'attacks': len(outcomes), 'rejected': rejected, 'quarantined': quarantined,
                                'unsafe': unsafe, 'failed': failed},
                  limitation='Structural rejection/quarantine only; no model-quality, truth, influence, or unlearning claim.')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    with args.output.open('x') as output:
        json.dump(run(), output, indent=2, allow_nan=False)
        output.write('\n')


if __name__ == '__main__':
    main()
