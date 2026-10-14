# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
import tempfile
from pathlib import Path
import unittest
import paid_review as paid
from prospective_snapshot import project


class BudgetTests(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.path=Path(self.tmp.name)/'budget.sqlite'
        self.p={'id':'frozen','budget':{'call_reservation_usd':3,'cli_max_budget_usd':1.5,'limit_usd':5,'maximum_calls':2}}
        self.db=paid.connect(self.path,self.p)
    def tearDown(self):self.db.close();self.tmp.cleanup()
    def test_unpriced_call_keeps_reservation_after_restart(self):
        paid.reserve(self.db,self.p,'a','prompt');self.db.close();self.db=paid.connect(self.path,self.p)
        with self.assertRaisesRegex(ValueError,'exhausted'):paid.reserve(self.db,self.p,'b','prompt')
        self.assertEqual(paid.receipt(self.db,self.p)['charged_or_reserved_usd'],3)
    def test_settlement_releases_only_actual_spend_and_blocks_retries(self):
        paid.reserve(self.db,self.p,'a','prompt');paid.settle(self.db,'a',.1)
        paid.reserve(self.db,self.p,'b','prompt');self.assertEqual(paid.receipt(self.db,self.p)['charged_or_reserved_usd'],3.1)
        with self.assertRaisesRegex(ValueError,'already'):paid.reserve(self.db,self.p,'a','changed')
    def test_overspend_stops_new_calls(self):
        paid.reserve(self.db,self.p,'a','prompt');paid.settle(self.db,'a',3.01)
        with self.assertRaisesRegex(ValueError,'exceeded'):paid.reserve(self.db,self.p,'b','prompt')
    def test_different_protocol_cannot_reuse_budget(self):
        with self.assertRaisesRegex(ValueError,'different'):paid.connect(self.path,dict(self.p,id='changed'))
    def test_reporting_cannot_create_missing_zero_cost_ledger(self):
        import sqlite3
        from evaluate_prospective import read_budget
        missing=Path(self.tmp.name)/'missing'
        missing.mkdir()
        with self.assertRaises(sqlite3.OperationalError):read_budget(missing,self.p,{})
        self.assertFalse((missing/'budget.sqlite').exists())

    def test_reporting_requires_every_assessed_call_in_ledger(self):
        import json
        from evaluate_prospective import read_budget
        root=Path(self.tmp.name)
        path=root/'calls'/'absent';path.mkdir(parents=True)
        (path/'call.json').write_text(json.dumps({'prompt_sha256':'prompt'}))
        with self.assertRaisesRegex(ValueError,'absent from budget'):
            read_budget(root,self.p,{'a':{'source_calls':[{'call_id':'absent'}]}})

    def test_bad_cost_does_not_release_reservation(self):
        paid.reserve(self.db,self.p,'a','prompt')
        for value in [-1,float('nan'),float('inf')]:
            with self.assertRaises(ValueError):paid.settle(self.db,'a',value)
        self.assertEqual(paid.receipt(self.db,self.p)['charged_or_reserved_usd'],3)


class SnapshotTests(unittest.TestCase):
    def setUp(self):
        self.p={'id':'p','registered_at_utc':'2026-10-02T00:00:00+00:00','excluded_families':[]}
        self.m={'id':'m','observed_at_unix':1790899201,'cases':[{'pr':1,'base':'b','head':'h','patch_sha256':'patch',
          'files':[{'path':'x.go','disposition':'parsed_go'}],'units':[{'id':'u','kind':'function','symbol':'F','path':'x.go','before':None,'after':{'text':'func F() {}'}}]}]}
        self.i={'id':'i','observed_at_utc':'2026-10-02T00:00:00.500000+00:00','registered_protocol_id':'p','selected':[{'pr':1,'base':'b','head':'h','stratum':'existing_at_registration'}],
                 'candidates':[{'pr':1,'head':'h','created_at':'2026-10-01T00:00:00+00:00','disposition':'selected'}]}
    def test_snapshot_keeps_unknown_coverage_and_all_dispositions(self):
        pic,sample=project(self.m,self.p,self.i)
        self.assertIsNone(pic['coverage'][0]['dependency_coverage_denominator'])
        self.assertEqual(pic['sample_ids'],['u']);self.assertEqual(sample['parent_manifest_id'],'m')
        self.assertEqual(len(pic['relationships']),2);self.assertFalse(pic['optimization_enabled'])
    def test_pre_registration_snapshot_rejected(self):
        self.m['observed_at_unix']=0
        with self.assertRaisesRegex(ValueError,'after'):project(self.m,self.p,self.i)
    def test_head_drift_and_seen_families_rejected(self):
        self.i['selected'][0]['head']='different'
        with self.assertRaisesRegex(ValueError,'revision|selection head'):project(self.m,self.p,self.i)
        self.i['selected'][0]['head']='h';self.p['excluded_families']=[1]
        with self.assertRaisesRegex(ValueError,'seen'):project(self.m,self.p,self.i)


class PromptTests(unittest.TestCase):
    def test_batches_preserve_complete_units(self):
        from prepare_references import prompts
        units=[{'id':str(i),'path':'x.go','symbol':'F','before':None,'after':{'text':'x'*1000}} for i in range(3)]
        manifest={'cases':[{'pr':1,'base':'b','head':'h','units':units}]}
        policy={'id':'p','labels':{'rubric':'review boundaries'},'budget':{'prompt_byte_limit':2400}}
        batches=prompts(manifest,policy)
        self.assertEqual([u['unit_id'] for _,rows,_ in batches for u in rows],['0','1','2'])
        self.assertTrue(all(len(text.encode())<=2400 for _,_,text in batches))
        self.assertTrue(all(u['after']=='x'*1000 for _,rows,_ in batches for u in rows))
        policy['budget']['prompt_byte_limit']=100
        with self.assertRaisesRegex(ValueError,'single unit'):prompts(manifest,policy)


if __name__=='__main__':unittest.main()
