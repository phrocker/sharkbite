# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
import json
import unittest
from unittest.mock import patch
from paired_review import prepare, context_patch, PROMPT
from shadow import digest

class PairedReviewTest(unittest.TestCase):
    def test_file_context_survives_lower_priority_proposal(self):
        unit={'id':'f','pr':1,'path':'policy.go','symbol':'F','kind':'function','before':None,'after':{'text':'func F() {}'}}
        context={**unit,'id':'c','symbol':'file_context','kind':'file_context','after':None}
        manifest={'id':'m','cases':[{'pr':1,'base':'b','head':'h','units':[unit,context],'files':[{'path':'policy.go','before_path':None,'after_path':'policy.go'}]}]}
        rows=[{'unit_id':u['id'],'action':'full_review','optimization_eligible':False,'proposal':'lower_priority' if u['id']=='f' else 'retain'} for u in (unit,context)]
        predictions={'manifest_id':'m','selection_id':'s','predictions':rows}
        selection={'id':'s','validation_manifest_id':'m','threshold':0.1}
        for row in rows:row.update(disposition='predicted' if row['unit_id']=='f' else 'unsupported_unit',review_score=0.05 if row['unit_id']=='f' else None)
        with patch('paired_review.git',return_value=b'+policy context and F'):
            full,candidate=prepare('.',manifest,predictions,selection=selection)
        self.assertEqual(full['prompt_sha256'],digest(full['prompt'].encode()))
        a=json.loads(full['prompt'][len(PROMPT):]);b=json.loads(candidate['prompt'][len(PROMPT):])
        self.assertEqual(a['shared_file_context'],b['shared_file_context'])
        self.assertEqual(len(a['declarations']),1);self.assertEqual(b['declarations'],[])
        self.assertIn('policy context and F',candidate['prompt'])
        rows[0]['disposition']='token_limit'
        with self.assertRaises(ValueError):prepare('.',manifest,predictions,selection=selection)
        rows[0]['disposition']='predicted'
        predictions['predictions']=rows[:1]
        with self.assertRaises(ValueError):prepare('.',manifest,predictions,selection=selection)

    def test_pure_rename_retains_raw_path_evidence(self):
        unit={'id':'rename','pr':1,'path':'new.go','symbol':'file_path','kind':'file_context','before':None,'after':None}
        manifest={'id':'m','cases':[{'pr':1,'base':'b','head':'h','units':[unit],'files':[{'path':'new.go','before_path':'old.go','after_path':'new.go','disposition':'parsed_go'}]}]}
        selection={'id':'s','validation_manifest_id':'m','threshold':0.1}
        prediction={'manifest_id':'m','selection_id':'s','predictions':[{'unit_id':'rename','action':'full_review','optimization_eligible':False,'disposition':'unsupported_unit','review_score':None,'proposal':'retain'}]}
        with patch('paired_review.git',return_value=b'rename from old.go\nrename to new.go'),patch('paired_review.context_patch',side_effect=AssertionError('must not split rename')):
            packets=prepare('.',manifest,prediction,extractor='fixture',selection=selection)
        for packet in packets:self.assertIn('rename from old.go',packet['prompt'])

    def test_context_split_removes_changed_body_but_preserves_other_evidence(self):
        before='package p\n// π\nfunc F() { oldPolicy() }\nconst Scope=1\n'
        after='package p\n// π\nfunc F() { newPolicy() }\nconst Scope=2\n'
        bodies=['func F() { oldPolicy() }','func F() { newPolicy() }']
        case={'base':'b','head':'h','units':[{'path':'p.go','before':{'text':bodies[0]},'after':{'text':bodies[1]}}]}
        def source(repo,revision,path,maximum):return {'disposition':'available','text':before if revision=='b' else after}
        def parsed(extractor,path,src):
            raw=src['text'].encode();body=bodies[0] if src['text']==before else bodies[1];start=raw.index(body.encode())
            return {'declarations':[{'key':'function:F','text':body,'start_byte':start,'end_byte':start+len(body.encode())}]}
        with patch('paired_review.source',side_effect=source),patch('paired_review.parsed',side_effect=parsed):
            result=context_patch('.',None,case,{'path':'p.go','before_path':'p.go','after_path':'p.go'})
        self.assertNotIn('oldPolicy',result);self.assertNotIn('newPolicy',result)
        self.assertIn('-const Scope=1',result);self.assertIn('+const Scope=2',result)
        self.assertIn('π',result)

if __name__=='__main__':unittest.main()
