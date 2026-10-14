# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
import copy
import unittest
import learning as l


def reseal(value):
    return l.seal(value['kind'], **{k: v for k, v in value.items() if k not in ('id', 'kind', 'schema')})


class LearningTests(unittest.TestCase):
    def setUp(self):
        self.ledger = l.seal('ledger', task={'id':'task-v1', 'labels':['review','routine'], 'positive_label':'review'},
            pictures=[{'id':'p', 'source_digest':'source', 'builder_digest':'builder', 'ontology_version':'none',
                       'mode':'reconstructed', 'coverage':[{'observed':3,'total':None}]}],
            examples=[{'id':str(i),'picture_id':'p','family':str(i),'content_digests':[str(i)]} for i in range(3)],
            assessments=[{'id':'a'+str(i),'example_id':str(i),'kind':'reviewer_assessment','assessor':'r',
                          'status':'proposed','label':'review','evidence_digest':'witness'} for i in range(3)])
        self.dp = l.seal('dataset_policy', task_id='task-v1', family_splits={'0':'train','1':'test','2':'test'},
                         assessors=['r'], statuses=['proposed'])
        self.data = l.dataset(self.ledger, self.dp)
        self.ep = l.seal('evaluation_policy', task_id='task-v1', evaluation_ids=['1','2'], assessors=['r'],
                         statuses=['proposed'], minimum_recall=1., minimum_positive_count=2, minimum_families=2)
        self.candidate = l.seal('candidate', dataset_id=self.data['id'], evaluation_policy_id=self.ep['id'],
                                training_ids=['0'], model_digest='model', policy_predeclared=True)
        self.predictions = l.seal('predictions', ledger_id=self.ledger['id'], candidate_id=self.candidate['id'],
                                  rows=[{'id':i,'proposal':'retain','audit_priority':0.5} for i in ['1','2']])

    def test_shadow_only_even_when_perfect(self):
        r = l.evaluate(self.ledger,self.data,self.ep,self.candidate,self.predictions)
        self.assertEqual(r['disposition'],'shadow_candidate')
        self.assertFalse(r['optimization_enabled'])
        self.assertEqual(r['permitted_action'],'full_review')

    def test_tampering_rejected(self):
        self.predictions['rows'][0]['proposal']='lower_priority'
        with self.assertRaisesRegex(ValueError,'identity'):
            l.evaluate(self.ledger,self.data,self.ep,self.candidate,self.predictions)

    def test_confident_miss_holds(self):
        self.predictions['rows'][0]['proposal']='lower_priority'
        r=l.evaluate(self.ledger,self.data,self.ep,self.candidate,reseal(self.predictions))
        self.assertEqual(r['disposition'],'hold')
        self.assertEqual(r['metrics'][0]['missed_ids'],['1'])

    def test_disputes_and_body_leakage_quarantined(self):
        ledger=copy.deepcopy(self.ledger)
        ledger['examples'][0]['content_digests']=['1']
        ledger['assessments'].append(dict(ledger['assessments'][2],id='conflict',label='routine'))
        result=l.dataset(reseal(ledger),self.dp)
        self.assertEqual({e['reason'] for e in result['excluded']}, {'heldout_content_overlap','disputed_label'})

    def test_predictions_not_labels(self):
        self.ledger['assessments'][0]['kind']='prediction'
        with self.assertRaisesRegex(ValueError,'predictions'):
            l.dataset(reseal(self.ledger),self.dp)

    def test_unauthorized_assessment_is_explicitly_quarantined(self):
        ledger = dict(self.ledger)
        ledger['assessments'] = [dict(row) for row in self.ledger['assessments']]
        ledger['assessments'][0]['assessor'] = 'untrusted'
        result = l.dataset(reseal(ledger), self.dp)
        self.assertIn('unauthorized_assessment', {row['reason'] for row in result['excluded']})

    def test_mixed_authority_assessments_are_quarantined(self):
        ledger = dict(self.ledger)
        ledger['assessments'] = [dict(row) for row in self.ledger['assessments']]
        ledger['assessments'].append(dict(ledger['assessments'][0], id='attacker', assessor='untrusted', label='routine'))
        result = l.dataset(reseal(ledger), self.dp)
        self.assertIn('unauthorized_assessment', {row['reason'] for row in result['excluded']})

    def test_missing_predictions_rejected(self):
        self.predictions['rows'].pop()
        with self.assertRaisesRegex(ValueError,'predictions'):
            l.evaluate(self.ledger,self.data,self.ep,self.candidate,reseal(self.predictions))

    def test_training_membership_cannot_change(self):
        self.candidate['training_ids']=['0','1']
        candidate=reseal(self.candidate)
        self.predictions['candidate_id']=candidate['id']
        with self.assertRaisesRegex(ValueError,'training membership'):
            l.evaluate(self.ledger,self.data,self.ep,candidate,reseal(self.predictions))

    def test_validation_cannot_be_reused_as_test(self):
        self.dp['family_splits']['1']='validation'
        data=l.dataset(self.ledger,reseal(self.dp))
        self.candidate['dataset_id']=data['id']
        candidate=reseal(self.candidate)
        self.predictions['candidate_id']=candidate['id']
        with self.assertRaisesRegex(ValueError,'reserved test'):
            l.evaluate(self.ledger,data,self.ep,candidate,reseal(self.predictions))

    def test_string_true_does_not_attest_predeclaration(self):
        self.candidate['policy_predeclared']='true'
        candidate=reseal(self.candidate)
        self.predictions['candidate_id']=candidate['id']
        result=l.evaluate(self.ledger,self.data,self.ep,candidate,reseal(self.predictions))
        self.assertEqual(result['disposition'],'hold')

    def test_unknown_cannot_be_lowered(self):
        self.ledger['assessments'][1]['label']='unknown'
        ledger=reseal(self.ledger)
        data=l.dataset(ledger,self.dp)
        self.candidate['dataset_id']=data['id']
        candidate=reseal(self.candidate)
        self.predictions['candidate_id']=candidate['id']
        self.predictions['ledger_id']=ledger['id']
        self.predictions['rows'][0]['proposal']='lower_priority'
        result=l.evaluate(ledger,data,self.ep,candidate,reseal(self.predictions))
        self.assertEqual(result['metrics'][0]['unknown_lowered_ids'],['1'])
        self.assertEqual(result['disposition'],'hold')

    def test_feature_ineligible_rows_excluded_from_fitting(self):
        self.dp['training_eligible_ids']=[]
        data=l.dataset(self.ledger,reseal(self.dp))
        self.assertEqual(data['excluded'],[{'example_id':'0','reason':'ineligible_training_features'}])

    def test_random_audit_covers_confident_cases_and_is_reproducible(self):
        pred=l.seal('predictions',ledger_id=self.ledger['id'],candidate_id='candidate',
                    rows=[{'id':str(i),'audit_priority':0.0} for i in range(3)])
        a=l.audit_plan(self.ledger,pred,'precommitted-seed',2,1)
        self.assertEqual(a,l.audit_plan(self.ledger,pred,'precommitted-seed',2,1))
        self.assertEqual(len(a['random_ids']),2)
        self.assertFalse(set(a['random_ids']) & set(a['targeted_ids']))
        self.assertEqual(a['random_inclusion_probability'],2/3)

    def test_posthoc_policy_cannot_promote(self):
        self.candidate['policy_predeclared']=False
        candidate=reseal(self.candidate)
        self.predictions['candidate_id']=candidate['id']
        result=l.evaluate(self.ledger,self.data,self.ep,candidate,reseal(self.predictions))
        self.assertEqual(result['disposition'],'hold')

    def test_promotion_requires_clean_evaluation_and_bound_approval(self):
        evaluation = l.evaluate(self.ledger, self.data, self.ep, self.candidate, self.predictions)
        artifact = l.seal('candidate_artifact', candidate_id=self.candidate['id'], evaluation_id=evaluation['id'], dataset_id=self.data['id'], model_digest='model', runtime_digest='runtime')
        approval = l.seal('promotion_approval', candidate_id=self.candidate['id'], evaluation_id=evaluation['id'], owner='owner', approved=True)
        release = l.promote_candidate(self.candidate, evaluation, artifact, approval)
        self.assertEqual(release['state'], 'active')
        self.assertIsNone(release['predecessor_release_id'])
        approval['approved'] = False
        with self.assertRaisesRegex(ValueError, 'identity'):
            l.promote_candidate(self.candidate, evaluation, artifact, approval)

    def test_failed_evaluation_cannot_promote(self):
        self.predictions['rows'][0]['proposal'] = 'lower_priority'
        evaluation = l.evaluate(self.ledger, self.data, self.ep, self.candidate, reseal(self.predictions))
        artifact = l.seal('candidate_artifact', candidate_id=self.candidate['id'], evaluation_id=evaluation['id'], dataset_id=self.data['id'], model_digest='model', runtime_digest='runtime')
        approval = l.seal('promotion_approval', candidate_id=self.candidate['id'], evaluation_id=evaluation['id'], owner='owner', approved=True)
        with self.assertRaisesRegex(ValueError, 'shadow evaluation'):
            l.promote_candidate(self.candidate, evaluation, artifact, approval)

    def test_rollback_is_append_only_and_requires_bound_approval(self):
        evaluation = l.evaluate(self.ledger, self.data, self.ep, self.candidate, self.predictions)
        artifact = l.seal('candidate_artifact', candidate_id=self.candidate['id'], evaluation_id=evaluation['id'], dataset_id=self.data['id'], model_digest='model', runtime_digest='runtime')
        approval = l.seal('promotion_approval', candidate_id=self.candidate['id'], evaluation_id=evaluation['id'], owner='owner', approved=True)
        first = l.promote_candidate(self.candidate, evaluation, artifact, approval)
        second = l.promote_candidate(self.candidate, evaluation, artifact, approval, predecessor_release=first)
        rollback_approval = l.seal('rollback_approval', active_release_id=second['id'], prior_release_id=first['id'], owner='owner', approved=True)
        rollback = l.rollback_release(second, first, rollback_approval)
        self.assertEqual(rollback['rollback_of'], second['id'])
        self.assertNotEqual(rollback['id'], second['id'])


if __name__ == '__main__':unittest.main()
