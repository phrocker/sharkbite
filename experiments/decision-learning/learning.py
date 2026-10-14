#!/usr/bin/env python3
# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
"""Offline, provider-neutral learning receipts. No serving or authorization API."""
import argparse
import hashlib
import json
import math
from pathlib import Path


def digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(',', ':'), allow_nan=False).encode()).hexdigest()


def seal(kind, **fields):
    record = dict(schema=1, kind=kind, **fields)
    return dict(record, id=digest(record))


def verify(record, kind):
    body = {k: v for k, v in record.items() if k != 'id'}
    if record.get('kind') != kind or record.get('schema') != 1 or record.get('id') != digest(body):
        raise ValueError('invalid ' + kind + ' identity')
    return record


def index(rows):
    result = {row['id']: row for row in rows}
    if len(result) != len(rows):
        raise ValueError('duplicate IDs')
    return result


def validate_ledger(ledger):
    verify(ledger, 'ledger')
    task = ledger['task']
    if len(set(task['labels'])) < 2 or task['positive_label'] not in task['labels']:
        raise ValueError('invalid task labels')
    pictures = index(ledger['pictures'])
    examples = index(ledger['examples'])
    index(ledger['assessments'])
    for picture in pictures.values():
        if not picture['source_digest'] or not picture['builder_digest'] or not picture['ontology_version']:
            raise ValueError('unpinned picture')
        # Unknown denominators remain null; zero is not completeness.
        for coverage in picture['coverage']:
            n, d = coverage['observed'], coverage['total']
            if type(n) is not int or n < 0 or (d is not None and (type(d) is not int or d < n)):
                raise ValueError('invalid coverage')
        if picture['mode'] not in ('reconstructed', 'prospective'):
            raise ValueError('invalid observation mode')
    for example in examples.values():
        if example['picture_id'] not in pictures or not example['family'] or not example['content_digests']:
            raise ValueError('incomplete example provenance')
    for assessment in ledger['assessments']:
        if assessment['example_id'] not in examples or assessment['label'] not in task['labels'] + ['unknown']:
            raise ValueError('invalid assessment')
        if assessment['kind'] not in ('reviewer_assessment', 'adjudication', 'observed_outcome'):
            raise ValueError('predictions cannot be training labels')
        if not assessment['evidence_digest'] or not assessment['assessor']:
            raise ValueError('unattributed assessment')
    return examples


def dataset(ledger, policy):
    """Freeze explicit family splits; quarantine conflicts and exact test overlaps."""
    examples = validate_ledger(ledger)
    verify(policy, 'dataset_policy')
    if policy['task_id'] != ledger['task']['id']:
        raise ValueError('task mismatch')
    splits = policy['family_splits']
    if set(splits.values()) - {'train', 'validation', 'test'}:
        raise ValueError('invalid split')
    if set(splits) != {e['family'] for e in examples.values()}:
        raise ValueError('every family needs exactly one split')
    assessments = {}
    all_assessments = {}
    for a in ledger['assessments']:
        all_assessments.setdefault(a['example_id'], []).append(a)
        if a['assessor'] in policy['assessors'] and a['status'] in policy['statuses']:
            assessments.setdefault(a['example_id'], []).append(a)
    held = {h for e in examples.values() if splits[e['family']] != 'train' for h in e['content_digests']}
    rows, excluded = [], []
    for eid, e in sorted(examples.items()):
        labels = {a['label'] for a in assessments.get(eid, [])}
        reason = None
        if any(a['assessor'] not in policy['assessors'] for a in all_assessments.get(eid, [])):
            reason = 'unauthorized_assessment'
        elif not labels or 'unknown' in labels:
            reason = 'missing_or_unknown_label'
        elif len(labels) != 1:
            reason = 'disputed_label'
        elif splits[e['family']] == 'train' and eid not in policy.get('training_eligible_ids', examples):
            reason = 'ineligible_training_features'
        elif splits[e['family']] == 'train' and held.intersection(e['content_digests']):
            reason = 'heldout_content_overlap'
        if reason:
            excluded.append(dict(example_id=eid, reason=reason))
        else:
            rows.append(dict(example_id=eid, family=e['family'], split=splits[e['family']],
                             label=next(iter(labels)), assessment_ids=sorted(a['id'] for a in assessments[eid])))
    return seal('dataset', ledger_id=ledger['id'], policy_id=policy['id'], task_id=policy['task_id'],
                rows=rows, excluded=excluded, family_splits=dict(splits), provenance='reconstructed offline manifest; not temporal validation',
                optimization_enabled=False)


def audit_plan(ledger, predictions, seed, random_count, targeted_count):
    """Fixed-size uniform hash sample, plus disjoint targeted samples."""
    examples = validate_ledger(ledger)
    verify(predictions, 'predictions')
    if predictions['ledger_id'] != ledger['id']:
        raise ValueError('prediction ledger mismatch')
    rows = index(predictions['rows'])
    if set(rows) != set(examples):
        raise ValueError('prediction coverage mismatch')
    if type(random_count) is not int or type(targeted_count) is not int or min(random_count, targeted_count) < 0:
        raise ValueError('invalid audit budget')
    ordered = sorted(examples, key=lambda eid: digest([seed, eid]))
    random_ids = ordered[:random_count]
    remaining = [eid for eid in ordered if eid not in random_ids]
    # Priority is caller-supplied policy data, never a calibrated probability.
    for row in rows.values():
        if not math.isfinite(row['audit_priority']):
            raise ValueError('nonfinite audit priority')
    targeted = sorted(remaining, key=lambda eid: (-rows[eid]['audit_priority'], eid))[:targeted_count]
    return seal('audit_plan', ledger_id=ledger['id'], predictions_id=predictions['id'], seed=seed,
                random_ids=random_ids, targeted_ids=targeted,
                random_inclusion_probability=len(random_ids) / len(examples) if examples else None,
                population=len(examples), limitation='Seed must be selected independently before labels; targeted sample is biased.')


def evaluate(ledger, data, policy, candidate, predictions):
    """Shadow promotion only. No production authority can be emitted."""
    examples = validate_ledger(ledger)
    verify(data, 'dataset'); verify(policy, 'evaluation_policy')
    verify(candidate, 'candidate'); verify(predictions, 'predictions')
    if data['ledger_id'] != ledger['id'] or candidate['dataset_id'] != data['id']:
        raise ValueError('training lineage mismatch')
    if policy['task_id'] != ledger['task']['id'] or data['task_id'] != policy['task_id']:
        raise ValueError('task mismatch')
    if predictions['ledger_id'] != ledger['id'] or predictions['candidate_id'] != candidate['id']:
        raise ValueError('prediction lineage mismatch')
    if candidate['evaluation_policy_id'] != policy['id']:
        raise ValueError('candidate not bound to evaluation policy')
    train = {r['example_id'] for r in data['rows'] if r['split'] == 'train'}
    if set(candidate['training_ids']) != train or len(candidate['training_ids']) != len(train):
        raise ValueError('training membership mismatch')
    # Evaluation membership includes disputed/unlabeled examples, not only resolved dataset rows.
    test = set(policy['evaluation_ids'])
    if not test or len(test) != len(policy['evaluation_ids']) or not test <= examples.keys():
        raise ValueError('invalid evaluation membership')
    if any(data['family_splits'].get(examples[x]['family']) != 'test' for x in test):
        raise ValueError('evaluation must use reserved test families')
    validation_bodies = {h for e in examples.values() if data['family_splits'].get(e['family']) == 'validation' for h in e['content_digests']}
    if validation_bodies & {h for x in test for h in examples[x]['content_digests']}:
        raise ValueError('validation/evaluation content overlap')
    if {examples[x]['family'] for x in train} & {examples[x]['family'] for x in test}:
        raise ValueError('training/evaluation family overlap')
    if {h for x in train for h in examples[x]['content_digests']} & {h for x in test for h in examples[x]['content_digests']}:
        raise ValueError('training/evaluation content overlap')
    rows = index(predictions['rows'])
    if set(rows) != test:
        raise ValueError('incomplete or extra evaluation predictions')
    for row in rows.values():
        if row['proposal'] not in ('retain', 'lower_priority', 'abstain'):
            raise ValueError('invalid decision')
    target = policy['minimum_recall']
    if not math.isfinite(target) or not 0 < target <= 1:
        raise ValueError('invalid recall target')
    if type(policy['minimum_positive_count']) is not int or policy['minimum_positive_count'] < 1:
        raise ValueError('invalid sample requirement')
    if type(policy['minimum_families']) is not int or policy['minimum_families'] < 1 or not policy['assessors']:
        raise ValueError('invalid evaluation requirements')
    results, reasons = [], []
    for assessor in policy['assessors']:
        assessments = [a for a in ledger['assessments'] if a['assessor'] == assessor and a['example_id'] in test]
        labels = {a['example_id']: a['label'] for a in assessments if a['status'] in policy['statuses']}
        if len(assessments) != len(labels) or set(labels) != test:
            reasons.append(assessor + ': incomplete/ambiguous assessment coverage')
            continue
        positives = {eid for eid, label in labels.items() if label == ledger['task']['positive_label']}
        missed = sorted(eid for eid in positives if rows[eid]['proposal'] == 'lower_priority')
        unknown = sorted(eid for eid, label in labels.items() if label == 'unknown' and rows[eid]['proposal'] == 'lower_priority')
        recall = (len(positives) - len(missed)) / len(positives) if positives else None
        results.append(dict(assessor=assessor, positives=len(positives), missed_ids=missed,
                            recall=recall, unknown_lowered_ids=unknown))
        if len(positives) < policy['minimum_positive_count'] or recall is None or recall < target or unknown:
            reasons.append(assessor + ': quality/sample gate failed')
    families = len({examples[x]['family'] for x in test})
    if families < policy['minimum_families']:
        reasons.append('insufficient independent families')
    if candidate['policy_predeclared'] is not True:
        reasons.append('evaluation policy reconstructed after experiment')
    return seal('promotion_record', candidate_id=candidate['id'], predictions_id=predictions['id'],
                evaluation_policy_id=policy['id'], ledger_id=ledger['id'], dataset_id=data['id'],
                metrics=results, families=families, reasons=reasons,
                disposition='shadow_candidate' if not reasons else 'hold',
                permitted_action='full_review', optimization_enabled=False,
                limitation='Relevance assessments are not defect truth. No quality-parity or exclusion authorization.')


def promote_candidate(candidate, evaluation, artifact, approval, predecessor_release=None):
    """Create an explicit active release after a clean shadow evaluation.

    This is an append-only record constructor. It does not mutate a serving
    pointer or authenticate the approval; an authorized service must perform
    those operations around this structural check.
    """
    verify(candidate, 'candidate')
    verify(evaluation, 'promotion_record')
    verify(artifact, 'candidate_artifact')
    verify(approval, 'promotion_approval')
    if evaluation['candidate_id'] != candidate['id'] or evaluation['dataset_id'] != candidate['dataset_id'] or evaluation['evaluation_policy_id'] != candidate['evaluation_policy_id'] or artifact['candidate_id'] != candidate['id']:
        raise ValueError('candidate lineage mismatch')
    if artifact['evaluation_id'] != evaluation['id'] or artifact['dataset_id'] != candidate['dataset_id']:
        raise ValueError('artifact evaluation lineage mismatch')
    if not artifact.get('runtime_digest') or not artifact.get('model_digest') or artifact['model_digest'] != candidate.get('model_digest'):
        raise ValueError('artifact model mismatch')
    if evaluation['disposition'] != 'shadow_candidate' or evaluation.get('reasons'):
        raise ValueError('candidate did not pass shadow evaluation')
    if approval.get('approved') is not True or not approval.get('owner') or approval.get('candidate_id') != candidate['id'] or approval.get('evaluation_id') != evaluation['id']:
        raise ValueError('promotion approval is not bound')
    predecessor_release_id = None
    if predecessor_release is not None:
        verify(predecessor_release, 'model_release')
        if predecessor_release['state'] != 'active' or any(predecessor_release[field] != candidate[field] for field in ('dataset_id', 'evaluation_policy_id')) or predecessor_release['ledger_id'] != evaluation['ledger_id']:
            raise ValueError('invalid predecessor release')
        predecessor_release_id = predecessor_release['id']
    return seal('model_release', state='active', candidate_id=candidate['id'], artifact_id=artifact['id'],
                evaluation_id=evaluation['id'], dataset_id=candidate['dataset_id'],
                evaluation_policy_id=candidate['evaluation_policy_id'], ledger_id=evaluation['ledger_id'],
                predecessor_release_id=predecessor_release_id, rollback_of=None,
                approval_id=approval['id'], limitation='Structural release record; serving pointer mutation is external.')


def rollback_release(active_release, prior_release, approval):
    """Emit an immutable rollback release; never overwrite the active record."""
    verify(active_release, 'model_release')
    verify(prior_release, 'model_release')
    verify(approval, 'rollback_approval')
    if active_release['state'] != 'active' or prior_release['state'] != 'active':
        raise ValueError('rollback requires active releases')
    for field in ('dataset_id', 'evaluation_policy_id', 'ledger_id'):
        if active_release[field] != prior_release[field]:
            raise ValueError('rollback lineage mismatch')
    if approval.get('approved') is not True or not approval.get('owner') or approval.get('active_release_id') != active_release['id'] or approval.get('prior_release_id') != prior_release['id']:
        raise ValueError('rollback approval is not bound')
    return seal('model_release', state='active', candidate_id=prior_release['candidate_id'], artifact_id=prior_release['artifact_id'],
                evaluation_id=prior_release['evaluation_id'], dataset_id=prior_release['dataset_id'],
                evaluation_policy_id=prior_release['evaluation_policy_id'], ledger_id=prior_release['ledger_id'],
                predecessor_release_id=active_release['id'], rollback_of=active_release['id'],
                approval_id=approval['id'], limitation='Structural rollback record; serving pointer mutation is external.')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('command', choices=['dataset', 'audit', 'evaluate'])
    parser.add_argument('--ledger', type=Path, required=True)
    parser.add_argument('--policy', type=Path)
    parser.add_argument('--dataset', type=Path)
    parser.add_argument('--candidate', type=Path)
    parser.add_argument('--predictions', type=Path)
    parser.add_argument('--seed', default='0')
    parser.add_argument('--random-count', type=int, default=10)
    parser.add_argument('--targeted-count', type=int, default=10)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    def read(name):
        path = getattr(args, name)
        if path is None: parser.error('--' + name + ' required for ' + args.command)
        return json.loads(path.read_text())
    if args.command == 'dataset':
        result = dataset(read('ledger'), read('policy'))
    elif args.command == 'audit':
        result = audit_plan(read('ledger'), read('predictions'), args.seed, args.random_count, args.targeted_count)
    else:
        result = evaluate(read('ledger'), read('dataset'), read('policy'), read('candidate'), read('predictions'))
    with args.output.open('x') as out:
        json.dump(result, out, indent=2, allow_nan=False); out.write('\n')


if __name__ == '__main__':
    main()
