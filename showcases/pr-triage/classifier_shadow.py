#!/usr/bin/env python3
# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
"""Bind classifier outputs to a complete external shadow-review manifest."""
import argparse
from pathlib import Path
from candidates import checked
from shadow import digest, write_new


def main():
    p=argparse.ArgumentParser(description=__doc__)
    for key in ('manifest','inputs','model','predictions','output'):p.add_argument('--'+key,type=Path,required=True)
    a=p.parse_args();m=checked(a.manifest);i=checked(a.inputs);model=checked(a.model);pred=checked(a.predictions)
    if pred['inputs_id']!=i['id'] or pred['model_id']!=model['id'] or pred['threshold']!=model['threshold']:raise ValueError('classifier identity mismatch')
    if m['id'] not in {s['manifest_id'] for s in i['sources']}:raise ValueError('source manifest mismatch')
    funcs={u['id'] for c in m['cases'] for u in c['units'] if u['kind']=='function'}
    indexed={r['unit_id']:r for r in pred['predictions']}
    if funcs!=set(indexed) or len(indexed)!=len(pred['predictions']) or funcs!={r['unit_id'] for r in i['rows']}:raise ValueError('classifier coverage mismatch')
    a.output.mkdir()
    selection={'model_id':model['id'],'parent_prediction_id':pred['id'],'validation_manifest_id':m['id'],'threshold':model['threshold'],'optimization_enabled':False};selection['id']=digest(selection);write_new(a.output/'selection.json',selection)
    rows=[]
    for case in m['cases']:
        for u in case['units']:
            row=indexed.get(u['id'],{'unit_id':u['id'],'disposition':'unsupported_unit','review_score':None,'proposal':'retain','action':'full_review','optimization_eligible':False})
            if row['action']!='full_review' or row['optimization_eligible']:raise ValueError('shadow policy changed')
            rows.append({**row,'pr':case['pr']})
    result={'manifest_id':m['id'],'selection_id':selection['id'],'classifier_prediction_id':pred['id'],'predictions':rows,'optimization_enabled':False};result['id']=digest(result);write_new(a.output/'predictions.json',result)

if __name__=='__main__':main()
