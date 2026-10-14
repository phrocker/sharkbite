# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
import copy
import unittest
from context_classifier import fit, score, bundle, restore, representation

class ClassifierTest(unittest.TestCase):
    def test_data_only_roundtrip_and_invalid_parameters_fail_closed(self):
        rows=[{'unit_id':str(i),'label':label,'text':{'code':text}} for i,(label,text) in enumerate([
            ('review','Authorize principal tenant visibility deny'),('review','Check delegation caller identity'),
            ('routine','Format string JSON serialize output'),('routine','Compute sample median latency')])]
        model=fit(rows,('code','char',1.));data=bundle(model,('code','char',1.))
        self.assertEqual(score(model,rows,'code'),score(restore(data),rows,'code'))
        for mutation in ('classes','nan','shape','vocabulary'):
            bad=copy.deepcopy(data)
            if mutation=='classes':bad['classes']=[True,False]
            elif mutation=='nan':bad['coef'][0][0]=float('nan')
            elif mutation=='shape':bad['coef'][0].pop()
            else:bad['vectorizers'][0]['vocabulary']['duplicate']=0
            with self.assertRaises(ValueError):restore(bad)

    def test_receipt_hashes_are_not_learned_features(self):
        unit={'path':'a.go','symbol':'F','before':None,'after':{'text':'func F() {}'}}
        context={'resolution':'unresolved receivers','sides':{'after':{'revision':'sensitive_revision_hash','disposition':'partial','files_total':2,'files_parsed':1,'omitted_files':1,'callers':{'omitted_count':2,'evidence':[{'path':'caller.go','symbol':'Caller','text':'func Caller() { F() }','source_sha256':'sensitive_source_hash'}]},'callees':{'omitted_count':0,'evidence':[]}}}}
        text=representation(unit,context)['context']
        self.assertNotIn('sensitive_revision_hash',text);self.assertNotIn('sensitive_source_hash',text)
        self.assertIn('func Caller()',text);self.assertIn('omitted_files',text);self.assertIn('unresolved receivers',text)

if __name__=='__main__':unittest.main()
