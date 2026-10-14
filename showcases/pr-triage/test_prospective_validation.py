# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
import copy
from pathlib import Path
import tempfile
import unittest
import prepare_references
from evaluate_prospective import validate_membership, validate_prompts
from prospective_snapshot import project
from shadow import digest


class MembershipTests(unittest.TestCase):
    def setUp(self):
        self.m={'cases':[{'pr':1,'units':[{'id':'u'}]}]}
        self.pic={'sample_ids':['u']};self.inputs={'rows':[{'unit_id':'u'}]}
        self.pred={'scores':{'u':.5},'optimization_enabled':False,'predictions':[
            {'unit_id':'u','action':'full_review','optimization_eligible':False}]}
    def check(self):return validate_membership(self.m,self.pic,self.inputs,self.pred)
    def test_numeric_scores_and_abstention_accepted(self):
        for value in (0,1,.5,None):
            self.pred['scores']['u']=value;self.assertEqual(self.check(),{'u'})
    def test_boolean_and_nonfinite_scores_rejected(self):
        for value in (True,False,float('nan'),float('inf'),-.1,1.1):
            with self.subTest(value=value):
                self.pred['scores']['u']=value
                with self.assertRaisesRegex(ValueError,'score'):self.check()
    def test_prediction_coverage_must_be_exact_and_unique(self):
        row=self.pred['predictions'][0]
        for rows in ([],[row,row],[dict(row,unit_id='other')]):
            with self.subTest(rows=rows):
                self.pred['predictions']=rows
                with self.assertRaises(ValueError):self.check()
    def test_duplicate_inputs_samples_and_manifest_units_rejected(self):
        for rows in (self.inputs['rows'],self.pic['sample_ids'],self.m['cases'][0]['units']):
            rows.append(copy.deepcopy(rows[0]))
            with self.assertRaisesRegex(ValueError,'duplicate'):self.check()
            rows.pop()
    def test_duplicate_family_with_distinct_units_rejected_at_both_boundaries(self):
        self.m['cases'].append({'pr':1,'units':[{'id':'other'}]})
        with self.assertRaisesRegex(ValueError,'duplicate PR'):self.check()
        self.m['observed_at_unix']=1790899201
        protocol={'id':'p','registered_at_utc':'2026-10-02T00:00:00+00:00'}
        with self.assertRaisesRegex(ValueError,'duplicate PR'):
            project(self.m,protocol,{'observed_at_utc':'2026-10-02T00:00:00.500000+00:00','registered_protocol_id':'p','selected':[]})


class PromptProvenanceTests(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.root=Path(self.tmp.name);(self.root/'prompts').mkdir()
        self.m={'cases':[{'pr':1,'base':'b','head':'h','units':[{'id':'u','path':'x.go','symbol':'F','before':None,'after':{'text':'func F() { enforce() }'}}]}]}
        self.p={'id':'p','labels':{'rubric':'review boundaries'},'budget':{'prompt_byte_limit':90000}}
        pr,units,text=prepare_references.prompts(self.m,self.p)[0]
        self.path=self.root/'prompts/pr-1-batch-0.txt';self.path.write_text(text)
        self.b={'runner_sha256':digest(Path(prepare_references.__file__).read_bytes()),'batches':[
            {'path':self.path.name,'pr':pr,'unit_ids':['u'],'sha256':digest(text.encode()),'bytes':len(text.encode())}]}
    def tearDown(self):self.tmp.cleanup()
    def check(self):validate_prompts(self.root,self.m,self.p,self.b)
    def test_canonical_prompt_passes(self):self.check()
    def test_rehashed_altered_source_rejected(self):
        altered=self.path.read_text().replace('enforce()', 'allow()');self.path.write_text(altered)
        self.b['batches'][0].update(sha256=digest(altered.encode()),bytes=len(altered.encode()))
        with self.assertRaisesRegex(ValueError,'noncanonical'):self.check()
    def test_altered_preparer_identity_rejected(self):
        self.b['runner_sha256']='changed'
        with self.assertRaisesRegex(ValueError,'preparer identity'):self.check()
    def test_reordered_units_paths_sizes_and_missing_batches_rejected(self):
        for key,value in [('unit_ids',[]),('path','../other'),('bytes',0)]:
            original=self.b['batches'][0][key];self.b['batches'][0][key]=value
            with self.assertRaisesRegex(ValueError,'noncanonical'):self.check()
            self.b['batches'][0][key]=original
        self.b['batches']=[]
        with self.assertRaisesRegex(ValueError,'noncanonical'):self.check()


class CallIdentityTests(unittest.TestCase):
    def test_substituted_call_and_changed_runner_rejected(self):
        import json
        import paid_review
        from evaluate_prospective import assessment
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);folder=root/'calls/pr1-b-b0';folder.mkdir(parents=True)
            raw=json.dumps({'result':json.dumps({'status':'proposed','records':[{'unit_id':'u','label':'review','witness':'enforce()'}]})}).encode()
            (folder/'response.json').write_bytes(raw)
            call={'call_id':'pr1-b-b0','runner_sha256':digest(Path(paid_review.__file__).read_bytes()),
                  'protocol_id':'p','prompt_sha256':'prompt','response_sha256':digest(raw),'started_at_unix':2,'exit_code':0}
            manifest={'id':'m','observed_at_unix':1,'cases':[{'units':[{'id':'u'}]}]}
            batches={'batches':[{'pr':1,'sha256':'prompt','unit_ids':['u']}]}
            for field,value in [('call_id','pr1-a-b0'),('runner_sha256','changed')]:
                (folder/'call.json').write_text(json.dumps(dict(call,**{field:value})))
                with self.assertRaisesRegex(ValueError,'identity or runner'):
                    assessment(root,manifest,{'id':'p'},'b',batches)
            (folder/'call.json').write_text(json.dumps(call))
            self.assertEqual(len(assessment(root,manifest,{'id':'p'},'b',batches)['records']),1)


class InventoryOrderTests(unittest.TestCase):
    def test_inventory_boundaries(self):
        from test_prospective import SnapshotTests
        fixture=SnapshotTests();fixture.setUp()
        for stamp in ('2026-10-01T23:59:59+00:00','2026-10-02T00:00:00+00:00',
                      '2026-10-02T00:00:02+00:00','2026-10-02T00:00:00.5'):
            fixture.i['observed_at_utc']=stamp
            with self.subTest(stamp=stamp),self.assertRaisesRegex(ValueError,'inventory must'):
                project(fixture.m,fixture.p,fixture.i)
        for stamp in ('2026-10-02T00:00:00.5+00:00','2026-10-02T00:00:01+00:00'):
            fixture.i['observed_at_utc']=stamp
            project(fixture.m,fixture.p,fixture.i)


if __name__=='__main__':unittest.main()
