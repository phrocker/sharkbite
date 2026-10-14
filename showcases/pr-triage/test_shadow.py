# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
import importlib.util
import base64
import json
from pathlib import Path
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import patch

spec=importlib.util.spec_from_file_location('shadow',Path(__file__).with_name('shadow.py'))
shadow=importlib.util.module_from_spec(spec);spec.loader.exec_module(shadow)

class ShadowTest(unittest.TestCase):
    def test_paths_preserve_rename_delete_and_unusual_names(self):
        paths=shadow.changed_paths(b'R100\0old name.go\0new\nname.go\0D\0deleted.go\0A\0new.go\0M\0policy.yaml\0')
        self.assertEqual(paths[0]['before_path'],'old name.go')
        self.assertEqual(paths[0]['after_path'],'new\nname.go')
        self.assertIsNone(paths[1]['after_path']);self.assertIsNone(paths[2]['before_path'])
        self.assertEqual(len(paths),4)
    def test_unchanged_function_omitted_but_changed_constant_retained(self):
        fn={'key':'function:F','kind':'function','text':'func F() {}'}
        const={'key':'const:Role','kind':'const','text':'const Role=1'}
        before={'declarations':[fn,const],'residue':'package p'}
        after={'declarations':[fn,{**const,'text':'const Role=2'}],'residue':'package p'}
        paired=shadow.pair_declarations(before,after)
        self.assertEqual(len(paired),1);self.assertEqual(paired[0]['kind'],'const')
    def test_duplicate_keys_rejected(self):
        decl={'key':'function:init','kind':'function','text':'func init(){}'}
        with self.assertRaises(ValueError):
            shadow.pair_declarations({'declarations':[decl,decl],'residue':''},{'declarations':[],'residue':''})
    def test_inserted_and_removed_init_do_not_shift_unchanged_functions(self):
        def declarations(bodies):
            return {'declarations': [{'key': 'function:init' + (f'#{i+1}' if i else ''),
                                     'kind': 'function', 'text': body} for i, body in enumerate(bodies)], 'residue': ''}
        before = declarations(['first', 'second'])
        after = declarations(['new', 'first', 'second'])
        pairs = shadow.pair_declarations(before, after)
        self.assertEqual(len(pairs), 1)
        self.assertIsNone(pairs[0]['before'])
        self.assertEqual(pairs[0]['after']['text'], 'new')
        reverse = shadow.pair_declarations(after, before)
        self.assertEqual(len(reverse), 1)
        self.assertEqual(reverse[0]['before']['text'], 'new')
        self.assertIsNone(reverse[0]['after'])
    def test_non_utf8_paths_round_trip_losslessly(self):
        raw = b'bad-\xff.go'
        item = shadow.changed_paths(b'A\0' + raw + b'\0')[0]
        self.assertEqual(base64.b64decode(item['non_utf8_paths_base64']['after_path']), raw)
        # Safe JSON text, without a replacement character or surrogate encoding.
        json.dumps(item, ensure_ascii=False).encode('utf-8')
    def test_reordered_initializers_remain_visible(self):
        for kind, keys in [('function', ['function:init', 'function:init#2']), ('var', ['var:A', 'var:B'])]:
            first={'key':keys[0],'kind':kind,'text':'registerPolicy()'}
            second={'key':keys[1],'kind':kind,'text':'serveRequests()'}
            before={'declarations':[first,second],'residue':'unchanged'}
            # init keys are source-order ordinals in each parse; var names persist.
            after={'declarations':[{**second,'key':keys[0] if kind=='function' else keys[1]},
                                   {**first,'key':keys[1] if kind=='function' else keys[0]}],'residue':'unchanged'}
            pairs=shadow.pair_declarations(before,after)
            self.assertEqual(len(pairs),1)
            self.assertEqual(pairs[0]['kind'],'file_context')
            self.assertIn('initialization_order',pairs[0]['symbol'])
    def test_collect_accounts_for_rename_invalid_path_and_invalid_patch(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            extractor = root/'extract'; extractor.write_bytes(b'fixture')
            protocol = {'source_anchor':'abcdef0','cohort_size':1,'mode':'retrospective_reconstruction',
                        'limits':{'max_files_per_pr':100,'max_file_bytes':1000,'max_review_packet_bytes':10000},
                        'context_limitations':'fixture'}
            protocol_path=root/'protocol.json';protocol_path.write_text(json.dumps(protocol))
            def git(repo, *args):
                if args[0]=='rev-parse':return b'abcdef012345\n'
                if args[0]=='log':return b'abcdef012345\tFixture (#1)\n'
                if '--name-status' in args:return b'R100\0old.go\0new.go\0A\0bad-\xff.go\0A\0invalid.go\0'
                if args[0]=='diff':return b'diff\n+invalid \xff text\n'
                raise AssertionError(args)
            def source(repo, revision, path, maximum):
                if path=='invalid.go':return {'disposition':'non_utf8'}
                return {'disposition':'available','text':'package p\nfunc F() {}'}
            parsed={'declarations':[{'key':'function:F','kind':'function','text':'func F() {}'}],'residue':'package p'}
            with patch.object(shadow,'git',side_effect=git), patch.object(shadow,'source',side_effect=source), patch.object(shadow,'parsed',return_value=parsed):
                shadow.collect(SimpleNamespace(protocol=protocol_path,output=root/'run',repo=root,extractor=extractor))
            case=json.loads((root/'run/manifest.json').read_text())['cases'][0]
            self.assertEqual([u['kind'] for u in case['units']],['file_context','non_utf8_path','source_unavailable'])
            self.assertEqual(case['units'][0]['symbol'],'file_path')
            self.assertEqual(case['review_packet']['disposition'],'non_utf8_patch')
            self.assertEqual(len(case['files']),3)
    def test_pinned_snapshot_uses_explicit_base_not_first_parent(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);extractor=root/'extract';extractor.write_bytes(b'fixture')
            head='a'*40;base='b'*40
            protocol={'source_anchor':head,'snapshots':[{'pr':9,'head':head,'base':base}],
                      'cohort_size':1,'mode':'existing_open_pr_shadow',
                      'limits':{'max_files_per_pr':100,'max_review_packet_bytes':1000},'context_limitations':'fixture'}
            path=root/'protocol.json';path.write_text(json.dumps(protocol))
            calls=[]
            def git(repo,*args):
                calls.append(args)
                if args[0]=='rev-parse':return args[-1].split('^')[0].encode()+b'\n'
                if args[0]=='diff':return b''
                raise AssertionError(args)
            with patch.object(shadow,'git',side_effect=git):
                shadow.collect(SimpleNamespace(protocol=path,output=root/'run',repo=root,extractor=extractor))
            case=json.loads((root/'run/manifest.json').read_text())['cases'][0]
            self.assertEqual(case['base'],base);self.assertEqual(case['head'],head)
            self.assertTrue(all(base in call and head in call for call in calls if call[0]=='diff'))

    def test_rounding_accepted_invalid_mass_and_nan_rejected(self):
        def response(probs):return {'answers':{'boundary':{'type':'choice','choice':'a','probabilities':probs}}}
        self.assertEqual(shadow.validate_answer(response({'a':.3333,'b':.3333,'c':.3333}),{'a':1,'b':2,'c':3})['choice'],'a')
        for probabilities in ({'a':.9,'b':.9,'c':.1},{'a':float('nan'),'b':0,'c':0},{'a':True,'b':0,'c':0},{'a':1,'b':0}):
            with self.assertRaises(ValueError):shadow.validate_answer(response(probabilities),{'a':1,'b':2,'c':3})
    def test_observation_cannot_be_overwritten(self):
        with tempfile.TemporaryDirectory() as d:
            path=Path(d)/'record.json';shadow.write_new(path,{'value':1})
            with self.assertRaises(FileExistsError):shadow.write_new(path,{'value':2})

if __name__=='__main__':unittest.main()
