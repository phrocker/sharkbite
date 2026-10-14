#!/usr/bin/env python3
# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
"""Evaluate a frozen classifier against separately supplied proposed relevance."""
import argparse
from pathlib import Path
from candidates import checked
from ranking import load_labels, evaluate
from shadow import digest, write_new


def main():
    p=argparse.ArgumentParser(description=__doc__)
    for name in ('inputs','model','predictions','manifest','assessment','output'):p.add_argument('--'+name,type=Path,required=True)
    a=p.parse_args();inputs=checked(a.inputs);model=checked(a.model);pred=checked(a.predictions);manifest=checked(a.manifest)
    if pred['inputs_id']!=inputs['id'] or pred['model_id']!=model['id'] or pred['threshold']!=model['threshold']:raise ValueError('frozen prediction identity mismatch')
    units={u['id']:u for c in manifest['cases'] for u in c['units'] if u['kind']=='function'}
    if set(units)!={r['unit_id'] for r in inputs['rows']} or manifest['id'] not in {s['manifest_id'] for s in inputs['sources']}:raise ValueError('manifest coverage mismatch')
    _,labels=load_labels(a.assessment,units);metrics=evaluate(pred['scores'],labels,model['threshold']);dropped=set(metrics['dropped_ids'])
    volumes={}
    for key in ('source_bytes','diff_bytes'):
        total=sum(r[key] for r in inputs['rows']);reduced=sum(r[key] for r in inputs['rows'] if r['unit_id'] in dropped);volumes[key]={'total':total,'proposed_lower_priority':reduced,'fraction':reduced/total if total else 0}
    result={'metrics':metrics,'volumes':volumes,'inputs_id':inputs['id'],'model_id':model['id'],'prediction_id':pred['id'],'assessment_sha256':digest(a.assessment.read_bytes()),'optimization_enabled':False,'limitations':['Agent-assessed relevance, not verified defects.','Retrospective reserved cohort; no production source exclusions.','Bytes are not downstream token/cost savings.']};result['id']=digest(result);write_new(a.output,result)

if __name__=='__main__':main()
