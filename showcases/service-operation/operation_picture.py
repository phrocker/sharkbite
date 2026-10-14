#!/usr/bin/env python3
"""Sealed, shadow-only service-operation risk picture.

This showcase keeps policy, source possibilities, observations, history and
model-like assessment separate. It never grants permission or mutates policy.
"""
import hashlib
import json


SCHEMA = 1
MAX_ITEMS = 256


def _digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(',', ':'), allow_nan=False).encode()).hexdigest()


def seal(kind, **fields):
    body = dict(schema=SCHEMA, kind=kind, **fields)
    return dict(body, id=_digest(body))


def verify(record, kind):
    if not isinstance(record, dict) or record.get('schema') != SCHEMA or record.get('kind') != kind:
        raise ValueError('invalid ' + kind)
    body = {key: value for key, value in record.items() if key != 'id'}
    if record.get('id') != _digest(body):
        raise ValueError(kind + ' identity mismatch')
    return record


def _text(value, name):
    if not isinstance(value, str) or not value.strip() or len(value) > 512:
        raise ValueError('invalid ' + name)


def _time(value, name):
    if type(value) is not int or value < 0:
        raise ValueError('invalid ' + name)


def picture(*, policy, guide, deployment, source, observations, history, operation,
            target, actor, requested_at, availability_cutoff):
    for name, value in (('policy', policy), ('guide', guide), ('deployment', deployment), ('source', source)):
        if not isinstance(value, dict):
            raise ValueError(name + ' must be an object')
    for name, value in (('operation', operation), ('target', target), ('actor', actor)):
        _text(value, name)
    _time(requested_at, 'requested_at'); _time(availability_cutoff, 'availability_cutoff')
    verify(policy, 'operation_policy'); verify(guide, 'troubleshooting_guide')
    verify(deployment, 'deployment_mapping'); verify(source, 'source_revision')
    _text(source.get('revision'), 'source revision')
    _text(deployment.get('environment'), 'deployment environment')
    _time(deployment.get('expires_at'), 'deployment expiry')
    if not isinstance(observations, list) or len(observations) > MAX_ITEMS:
        raise ValueError('observations exceed bound')
    if not isinstance(history, list) or len(history) > MAX_ITEMS:
        raise ValueError('history exceeds bound')
    evidence_ids = set()
    for row in observations + history:
        if not isinstance(row, dict):
            raise ValueError('evidence row must be an object')
        _text(row.get('id'), 'evidence id'); _text(row.get('key'), 'evidence key')
        try:
            if len(json.dumps(row.get('value'), sort_keys=True, allow_nan=False)) > 4096:
                raise ValueError('evidence value exceeds bound')
        except (TypeError, ValueError):
            raise ValueError('invalid evidence value')
        if row['id'] in evidence_ids:
            raise ValueError('duplicate evidence id')
        evidence_ids.add(row['id'])
        _time(row.get('observed_at'), 'observed_at')
        if row['observed_at'] > availability_cutoff:
            raise ValueError('evidence is after availability cutoff')
    if deployment.get('source_revision') != source.get('revision'):
        raise ValueError('deployment/source mapping mismatch')
    return seal('operation_picture', policy_id=policy['id'], guide_id=guide['id'],
                deployment_id=deployment['id'], source_id=source['id'],
                source_revision=source['revision'],
                environment=deployment['environment'], deployment_expires_at=deployment['expires_at'],
                observations=observations, history=history, operation=operation,
                target=target, actor=actor, requested_at=requested_at,
                availability_cutoff=availability_cutoff)


def assess(picture_record, current):
    verify(picture_record, 'operation_picture')
    if not isinstance(current, dict):
        raise ValueError('current state must be an object')
    for key in ('policy_id', 'source_revision', 'target', 'actor', 'now', 'deployment_id', 'environment'):
        if key not in current:
            raise ValueError('current state missing ' + key)
    _time(current['now'], 'current now')
    if current['policy_id'] != picture_record['policy_id']:
        return _result(picture_record, 'abstain', 'policy_substitution')
    if current['source_revision'] != picture_record['source_revision']:
        return _result(picture_record, 'abstain', 'stale_deployment_mapping')
    if current['deployment_id'] != picture_record['deployment_id'] or current['environment'] != picture_record['environment']:
        return _result(picture_record, 'abstain', 'stale_deployment_mapping')
    if current['now'] > picture_record['deployment_expires_at']:
        return _result(picture_record, 'abstain', 'stale_deployment_mapping')
    if current['target'] != picture_record['target']:
        return _result(picture_record, 'abstain', 'operation_substitution')
    if current['actor'] != picture_record['actor']:
        return _result(picture_record, 'abstain', 'operation_substitution')
    if current.get('permission') is not True:
        return _result(picture_record, 'abstain', 'permission_revoked')
    policy = current.get('policy')
    guide = current.get('guide')
    if not isinstance(policy, dict) or policy.get('id') != picture_record['policy_id']:
        return _result(picture_record, 'abstain', 'policy_substitution')
    if not isinstance(guide, dict) or guide.get('id') != picture_record['guide_id']:
        return _result(picture_record, 'abstain', 'misleading_or_unapproved_guide')
    try:
        verify(policy, 'operation_policy'); verify(guide, 'troubleshooting_guide')
    except ValueError:
        return _result(picture_record, 'abstain', 'substituted_evidence')
    allow = policy.get('allow')
    hazards = policy.get('hazards')
    prerequisites = policy.get('required_prerequisites')
    if not isinstance(allow, dict) or allow.get(picture_record['operation']) is not True:
        return _result(picture_record, 'abstain', 'policy_denied')
    if not isinstance(hazards, dict) or not isinstance(prerequisites, dict):
        return _result(picture_record, 'abstain', 'policy_substitution')
    if not isinstance(guide, dict) or guide.get('approved') is not True:
        return _result(picture_record, 'abstain', 'misleading_or_unapproved_guide')
    rows = picture_record['observations'] + picture_record['history']
    values = {}
    for row in rows:
        values.setdefault(row['key'], set()).add(json.dumps(row.get('value'), sort_keys=True))
    if any(len(value) > 1 for value in values.values()):
        return _result(picture_record, 'abstain', 'conflicting_history')
    required = prerequisites.get(picture_record['operation'], [])
    if not isinstance(required, list) or any(not isinstance(key, str) or not key for key in required):
        return _result(picture_record, 'abstain', 'policy_substitution')
    missing = [key for key in required if values.get(key) != {'true'}]
    decision = 'inspect' if missing or hazards.get(picture_record['operation']) else 'routine'
    return _result(picture_record, decision, None, missing,
                   {'decision': decision, 'causal_proof': False})


def _result(record, decision, abstention, missing=None, prediction=None):
    return seal('operation_assessment', picture_id=record['id'], decision=decision,
                abstention=abstention, disposition='shadow_only', policy_effect=False,
                missing_prerequisites=[] if missing is None else missing,
                prediction=prediction)
