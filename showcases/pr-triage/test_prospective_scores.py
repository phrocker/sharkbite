# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
import copy
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
from verify_prospective_scores import validate_inputs, verify_scores, compare_replay
from evaluate_prospective import temporal_groups, read_budget
from candidates import original_state, change_text
from shadow import canonical, digest
import paid_review


class ScoreVerificationTests(unittest.TestCase):
    def test_rehashed_score_and_disposition_substitution_rejected(self):
        original={'scores':{'u':.7},'predictions':[{'unit_id':'u','proposal':'retain'}]}
        for changed in [dict(original,scores={'u':.1}),dict(original,predictions=[])]:
            changed['id']=digest(changed)
            with self.assertRaisesRegex(ValueError,'recomputed'):compare_replay(changed,original)
        compare_replay(original,copy.deepcopy(original))
    def test_replay_runner_substitution_rejected_before_execution(self):
        with patch('verify_prospective_scores.subprocess.run') as run:
            with self.assertRaisesRegex(ValueError,'runner mismatch'):
                verify_scores(Path('/unused'),Path('/unused/model'),{'replay_runner_sha256':'changed'})
            run.assert_not_called()
    def test_source_features_metadata_and_contract_bound(self):
        u={'id':'u','pr':1,'path':'x.go','symbol':'F','before':None,'after':{'text':'func F() {}'}}
        m={'id':'m','cases':[{'units':[u]}]};state=original_state(u)
        row={'unit_id':'u','pr':1,'path':'x.go','symbol':'F','label':'unknown','text':{'code':canonical(state)},
             'target_body_sha256':[digest(state[s].encode()) for s in ('before','after') if state[s]],
             'code_state_sha256':digest(state),'diff_state_sha256':digest(change_text(u)),
             'source_bytes':sum(len(state[s].encode()) for s in ('before','after')),'diff_bytes':len(change_text(u).encode())}
        inputs={'sources':[{'manifest_id':'m','assessment_sha256':None}],'evidence_contract':{'version':'pinned'},'rows':[row]}
        model={'recipe':['code','linear_svm'],'evidence_contract':inputs['evidence_contract']}
        validate_inputs(m,inputs,model)
        for field,value in [('source_bytes',0),('target_body_sha256',[]),('text',{'code':'changed'})]:
            bad=copy.deepcopy(inputs);bad['rows'][0][field]=value
            with self.assertRaisesRegex(ValueError,'sampled source'):validate_inputs(m,bad,model)
        for field,value in [('sources',[]),('evidence_contract',{})]:
            bad=dict(inputs,**{field:value})
            with self.assertRaises(ValueError):validate_inputs(m,bad,model)


class StratificationTests(unittest.TestCase):
    def test_existing_and_future_families_not_combined(self):
        manifest={'cases':[{'pr':1,'units':[{'id':'a'}]},{'pr':2,'units':[{'id':'b'}]}]}
        picture={'coverage':[{'family':1,'stratum':'existing_at_registration'},
                             {'family':2,'stratum':'created_after_registration'}]}
        groups=temporal_groups(manifest,picture)
        self.assertEqual(groups['existing_at_registration']['ids'],{'a'})
        self.assertEqual(groups['created_after_registration']['ids'],{'b'})
        picture['coverage'][1]['stratum']='unclassified'
        with self.assertRaisesRegex(ValueError,'unknown temporal'):temporal_groups(manifest,picture)


class ReconciliationTests(unittest.TestCase):
    def test_priced_pending_response_cannot_report_zero_paid(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);protocol={'id':'p','budget':{'limit_usd':25,'call_reservation_usd':3,'cli_max_budget_usd':1.5,'maximum_calls':8}}
            db=paid_review.connect(root/'budget.sqlite',protocol);paid_review.reserve(db,protocol,'call','prompt')
            folder=root/'calls/call';folder.mkdir(parents=True)
            (folder/'call.json').write_text(json.dumps({'prompt_sha256':'prompt'}))
            (folder/'response.json').write_text(json.dumps({'total_cost_usd':.4}))
            refs={'a':{'source_calls':[{'call_id':'call'}]}}
            with self.assertRaisesRegex(ValueError,'requires settled'):read_budget(root,protocol,refs)
            paid_review.settle(db,'call',.4);db.close()
            self.assertEqual(read_budget(root,protocol,refs)['reported_paid_usd'],.4)


if __name__=='__main__':unittest.main()
