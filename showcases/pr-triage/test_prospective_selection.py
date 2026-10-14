# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
import copy
import json
from pathlib import Path
import tempfile
import unittest
import test_prospective as fixtures
from prospective_snapshot import project
from verify_prospective_selection import compare_projection, verify_inventory
from evaluate_prospective import gate_reasons
from shadow import digest


class SelectionTests(unittest.TestCase):
    def setUp(self):
        fixture=fixtures.SnapshotTests();fixture.setUp()
        self.full,self.protocol,self.inventory=fixture.m,fixture.p,fixture.i
    def test_rehashed_cherry_pick_fails_reconstruction(self):
        pic,sample=project(self.full,self.protocol,self.inventory)
        compare_projection(self.full,self.protocol,self.inventory,pic,sample)
        sample['cases'][0]['units']=[];pic['sample_ids']=[];pic['coverage'][0]['sampled_functions']=0
        pic.pop('id');pic['id']=digest(pic);sample['picture_id']=pic['id'];sample.pop('id');sample['id']=digest(sample)
        with self.assertRaisesRegex(ValueError,'full-manifest reconstruction'):
            compare_projection(self.full,self.protocol,self.inventory,pic,sample)
    def test_unlisted_excluded_and_misclassified_selection_rejected(self):
        for change in ('unlisted','excluded','head','stratum'):
            inv=copy.deepcopy(self.inventory)
            if change=='unlisted':inv['candidates']=[]
            elif change=='excluded':inv['candidates'][0]['disposition']='excluded_draft'
            elif change=='head':inv['candidates'][0]['head']='different'
            else:inv['selected'][0]['stratum']='created_after_registration'
            with self.subTest(change=change),self.assertRaises(ValueError):project(self.full,self.protocol,inv)
    def test_no_changed_go_rejected(self):
        self.full['cases'][0]['files'][0]['path']='README.md'
        with self.assertRaisesRegex(ValueError,'no changed Go'):project(self.full,self.protocol,self.inventory)
    def test_unknown_builder_rejected(self):
        pic,sample=project(self.full,self.protocol,self.inventory);pic['builder_sha256']='untrusted'
        with self.assertRaisesRegex(ValueError,'unrecognized'):compare_projection(self.full,self.protocol,self.inventory,pic,sample)
    def test_empty_cohort_stays_pending(self):
        reasons=gate_reasons({}, {'minimum_families':4,'minimum_positive_count_per_assessor':50,'minimum_recall':.98})
        self.assertTrue(reasons);self.assertIn('pending',reasons[0])
    def test_summary_cannot_override_raw_draft_head_or_creation_time(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp)
            raw=[{'number':1,'state':'open','draft':False,'head':{'sha':'h'},
                   'base':{'sha':'b'},'created_at':'2026-10-01T00:00:00+00:00'}]
            def write_raw():
                b=json.dumps(raw).encode();(root/'raw-inventory.json').write_bytes(b);self.inventory['raw_inventory_sha256']=digest(b)
            (root/'raw-files-1.json').write_text(json.dumps([{'filename':'x.go'}]));write_raw()
            verify_inventory(root,self.inventory,self.protocol)
            # base is included because the selected pin is now bound to it. The
            # stub omitted it, which is the reason that binding could not have
            # existed: project() compares the pin's base only against the
            # collected manifest, so a base substituted consistently in both
            # agreed with itself and with nothing external.
            for field,value in [('state','closed'),('draft',True),('head',{'sha':'different'}),
                                ('base',{'sha':'substituted'}),('created_at','2026-10-03T00:00:00+00:00')]:
                saved=raw[0][field];raw[0][field]=value;write_raw()
                with self.subTest(field=field),self.assertRaises(ValueError):verify_inventory(root,self.inventory,self.protocol)
                raw[0][field]=saved
            write_raw();(root/'raw-files-1.json').write_text(json.dumps([{'filename':'README.md'}]))
            with self.assertRaisesRegex(ValueError,'eligibility'):verify_inventory(root,self.inventory,self.protocol)


if __name__=='__main__':unittest.main()
