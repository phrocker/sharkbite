import copy
import unittest

import operation_picture as op


def base():
    policy = op.seal('operation_policy', version='policy-v1', allow={'restart': True},
                     hazards={'restart': True}, required_prerequisites={'restart': ['capacity']})
    guide = op.seal('troubleshooting_guide', version='tsg-v1', approved=True, content_digest='g' * 64)
    source = op.seal('source_revision', revision='src-v1', content_digest='s' * 64)
    deployment = op.seal('deployment_mapping', deployment='deploy-v1', source_revision='src-v1', environment='prod', expires_at=20)
    observations = [{'id': 'o1', 'key': 'capacity', 'value': True, 'observed_at': 10}]
    history = [{'id': 'h1', 'key': 'last_outcome', 'value': 'success', 'observed_at': 10}]
    picture = op.picture(policy=policy, guide=guide, deployment=deployment, source=source,
                        observations=observations, history=history, operation='restart',
                        target='svc-a', actor='alice', requested_at=11, availability_cutoff=11)
    current = {'policy_id': policy['id'], 'source_revision': source['revision'], 'target': 'svc-a', 'actor': 'alice',
               'deployment_id': deployment['id'], 'environment': 'prod', 'now': 11,
               'permission': True, 'policy': policy, 'guide': guide}
    return picture, current


class OperationPictureTests(unittest.TestCase):
    def test_allowed_case_is_shadow_only_and_inspection(self):
        picture, current = base()
        result = op.assess(picture, current)
        op.verify(result, 'operation_assessment')
        self.assertEqual(result['decision'], 'inspect')
        self.assertEqual(result['disposition'], 'shadow_only')
        self.assertFalse(result['policy_effect'])
        self.assertFalse(result['prediction']['causal_proof'])

    def test_missing_prerequisite_is_explicit(self):
        picture, current = base()
        picture['observations'][0]['value'] = False
        picture['id'] = op._digest({key: value for key, value in picture.items() if key != 'id'})
        result = op.assess(picture, current)
        op.verify(result, 'operation_assessment')
        self.assertEqual(result['missing_prerequisites'], ['capacity'])

    def test_substitution_and_revocation_abstain(self):
        picture, current = base()
        for mutation, reason in (({'target': 'svc-b'}, 'operation_substitution'),
                                 ({'actor': 'mallory'}, 'operation_substitution'),
                                 ({'permission': False}, 'permission_revoked'),
                                 ({'source_revision': 'src-old'}, 'stale_deployment_mapping'),
                                 ({'now': 21}, 'stale_deployment_mapping'),
                                 ({'environment': 'staging'}, 'stale_deployment_mapping')):
            changed = dict(current); changed.update(mutation)
            result = op.assess(picture, changed)
            op.verify(result, 'operation_assessment')
            self.assertEqual(result['abstention'], reason)

    def test_unapproved_guide_and_conflicting_history_abstain(self):
        picture, current = base()
        changed = dict(current); changed['guide'] = dict(current['guide'], approved=False)
        result = op.assess(picture, changed)
        op.verify(result, 'operation_assessment')
        self.assertEqual(result['abstention'], 'substituted_evidence')
        unapproved = op.seal('troubleshooting_guide', version='tsg-v2', approved=False, content_digest='g' * 64)
        valid_unapproved = copy.deepcopy(picture)
        valid_unapproved['guide_id'] = unapproved['id']
        valid_unapproved['id'] = op._digest({key: value for key, value in valid_unapproved.items() if key != 'id'})
        changed['guide'] = unapproved
        result = op.assess(valid_unapproved, changed)
        op.verify(result, 'operation_assessment')
        self.assertEqual(result['abstention'], 'misleading_or_unapproved_guide')
        conflict = copy.deepcopy(picture)
        conflict['history'].append({'id': 'h2', 'key': 'last_outcome', 'value': 'failure', 'observed_at': 11})
        conflict['id'] = op._digest({key: value for key, value in conflict.items() if key != 'id'})
        result = op.assess(conflict, current)
        op.verify(result, 'operation_assessment')
        self.assertEqual(result['abstention'], 'conflicting_history')

    def test_cutoff_and_identity_tampering_rejected(self):
        picture, _ = base()
        with self.assertRaises(ValueError):
            op.picture(**{**base_inputs(), 'availability_cutoff': 9})
        changed = dict(picture); changed['target'] = 'svc-b'
        with self.assertRaises(ValueError):
            op.assess(changed, base()[1])

    def test_duplicate_evidence_and_malformed_policy_fail_closed(self):
        picture, current = base()
        duplicate = copy.deepcopy(picture)
        duplicate['history'][0]['id'] = duplicate['observations'][0]['id']
        with self.assertRaises(ValueError):
            op.picture(**{**base_inputs(), 'history': duplicate['history'], 'availability_cutoff': 11})
        malformed = dict(current)
        malformed['policy'] = op.seal('operation_policy', version='policy-v2', allow={'restart': True}, hazards=[], required_prerequisites={})
        malformed['policy_id'] = malformed['policy']['id']
        result = op.assess(picture, malformed)
        op.verify(result, 'operation_assessment')
        self.assertEqual(result['abstention'], 'policy_substitution')

    def test_oversized_evidence_is_rejected(self):
        with self.assertRaisesRegex(ValueError, 'evidence value'):
            op.picture(**{**base_inputs(), 'availability_cutoff': 11,
                          'observations': [{'id': 'o1', 'key': 'capacity', 'value': 'x' * 5000, 'observed_at': 10}]})


def base_inputs():
    picture, _ = base()
    # Reconstruct the inputs only to exercise the cutoff boundary without
    # duplicating the fixture's source records in the assertion.
    policy = op.seal('operation_policy', version='policy-v1', allow={'restart': True}, hazards={'restart': True}, required_prerequisites={'restart': ['capacity']})
    guide = op.seal('troubleshooting_guide', version='tsg-v1', approved=True, content_digest='g' * 64)
    source = op.seal('source_revision', revision='src-v1', content_digest='s' * 64)
    deployment = op.seal('deployment_mapping', deployment='deploy-v1', source_revision='src-v1', environment='prod', expires_at=20)
    return dict(policy=policy, guide=guide, deployment=deployment, source=source,
                observations=[{'id': 'o1', 'key': 'capacity', 'value': True, 'observed_at': 10}],
                history=[{'id': 'h1', 'key': 'last_outcome', 'value': 'success', 'observed_at': 10}],
                operation='restart', target='svc-a', actor='alice', requested_at=11)


if __name__ == '__main__':
    unittest.main()
