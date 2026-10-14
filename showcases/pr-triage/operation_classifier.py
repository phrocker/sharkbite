#!/usr/bin/env python3
# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
"""Supervised syntax-operation experiment; no resolved data-flow claims."""
import argparse
import gzip
import json
import math
from pathlib import Path
import subprocess
import time
import context_classifier as cc
from candidates import checked, original_state, change_text
from ranking import load_labels, select_threshold
from shadow import canonical, digest, write_new

RECIPES=[(view,family) for view in ('code','operations','combined') for family in ('logistic','linear_svm')]


def contract(extractor):
    return {'operation_extractor_sha256':digest(extractor.read_bytes()),'runner_sha256':digest(Path(__file__).read_bytes()),'base_classifier_sha256':digest(Path(cc.__file__).read_bytes()),'candidate_helpers_sha256':digest(Path(__file__).with_name('candidates.py').read_bytes()),'source_helpers_sha256':digest(Path(__file__).with_name('shadow.py').read_bytes())}


def operations(text,extractor):
    if not text:return {'facts':[],'resolution':'absent','omitted':0}
    proc=subprocess.run([str(extractor)],input=json.dumps({'content':text}),text=True,capture_output=True,check=True)
    return json.loads(proc.stdout)


def build(a):
    config=json.loads(a.config.read_text());rows=[];sources=[]
    for spec in config['datasets']:
        manifest=checked(Path(spec['manifest']));units={u['id']:u for c in manifest['cases'] for u in c['units'] if u['kind']=='function'}
        _,labels=load_labels(Path(spec['assessment']),units) if spec.get('assessment') else (None,{uid:'unknown' for uid in units})
        sources.append({'manifest_id':manifest['id'],'assessment_sha256':digest(Path(spec['assessment']).read_bytes()) if spec.get('assessment') else None})
        for uid,u in units.items():
            state=original_state(u);facts={side:operations(state[side],a.extractor) for side in ('before','after')}
            code=canonical(state);op=canonical({'path':u['path'],'declaration':u['symbol'],'facts':facts})
            rows.append({'unit_id':uid,'pr':u['pr'],'path':u['path'],'symbol':u['symbol'],'label':labels[uid],
                         'text':{'code':code,'operations':op,'combined':code+'\n'+op},'operation_evidence':facts,
                         'operation_complete':all(not f.get('error') and not f['omitted'] for f in facts.values()),
                         'target_body_sha256':[digest(state[s].encode()) for s in ('before','after') if state[s]],
                         'code_state_sha256':digest(state),'diff_state_sha256':digest(change_text(u)),
                         'source_bytes':sum(len(state[s].encode()) for s in ('before','after')),'diff_bytes':len(change_text(u).encode())})
        print(json.dumps({'manifest':manifest['id'],'rows':len(units),'total':len(rows)}),flush=True)
    if len({r['unit_id'] for r in rows})!=len(rows):raise ValueError('duplicate units')
    out={'status':config['status'],'evidence_contract':contract(a.extractor),'rows':rows,'sources':sources,'optimization_enabled':False};out['id']=digest(out);write_new(a.output,out)


def fit(rows,recipe):
    from sklearn.linear_model import LogisticRegression
    from sklearn.svm import LinearSVC
    view,family=recipe;eligible=[r for r in rows if r['label']!='unknown' and (view=='code' or r['operation_complete'])]
    v=cc.vectorizer('mixed');x=v.fit_transform([r['text'][view] for r in eligible]);labels=[r['label']=='review' for r in eligible]
    m=LogisticRegression(C=3,class_weight='balanced',max_iter=1000,random_state=42,solver='liblinear') if family=='logistic' else LinearSVC(C=1,class_weight='balanced',max_iter=3000,random_state=42,dual='auto')
    return v,m.fit(x,labels)


def score(model,rows,recipe):
    import numpy as np
    from scipy.special import expit
    view,_=recipe;v,m=model;eligible=[r for r in rows if view=='code' or r['operation_complete']];result={r['unit_id']:None for r in rows}
    if eligible:
        values=expit(m.decision_function(v.transform([r['text'][view] for r in eligible])))
        if not np.isfinite(values).all():raise ValueError('nonfinite classifier scores')
        result.update({r['unit_id']:float(x) for r,x in zip(eligible,values)})
    return result


def restore(artifact):
    # Both learners have a linear decision function. Sigmoid is only a bounded
    # ranking transform for SVM, never a calibrated probability.
    return cc.restore({**artifact['parameters'],'recipe':['code','mixed',3.]})


def train(a):
    import sklearn
    if sklearn.__version__!='1.7.2':raise ValueError('pinned sklearn required')
    data=checked(a.inputs);reserved=checked(a.reserved_inputs)
    if data['evidence_contract']!=reserved['evidence_contract'] or data['evidence_contract']!=contract(a.extractor):raise ValueError('evidence contract mismatch')
    if any(r['label']!='unknown' for r in reserved['rows']):raise ValueError('reserved labels supplied')
    if {r['pr'] for r in data['rows']}&{r['pr'] for r in reserved['rows']}:raise ValueError('reserved PR overlap')
    bodies={h for r in reserved['rows'] for h in r['target_body_sha256']};diffs={r['diff_state_sha256'] for r in reserved['rows']}
    rows=[r for r in data['rows'] if not set(r['target_body_sha256'])&bodies and r['diff_state_sha256'] not in diffs]
    prs=sorted({r['pr'] for r in rows});folds={pr:i%5 for i,pr in enumerate(prs)};a.output.mkdir()
    config={'inputs_id':data['id'],'reserved_inputs_id':reserved['id'],'training_ids':[r['unit_id'] for r in rows],'purged_reserved_body_overlap':[r['unit_id'] for r in data['rows'] if set(r['target_body_sha256'])&bodies or r['diff_state_sha256'] in diffs],'recipes':RECIPES,'folds':folds,'target_recall':.98,'utility':'source bytes proposed for reduction at98% OOF relevance, zero unknowns dropped','evidence_contract':data['evidence_contract'],'status':'prior V8 evaluation retired to development','optimization_enabled':False};write_new(a.output/'config.json',config)
    results=[];labels={r['unit_id']:r['label'] for r in rows}
    for idx,recipe in enumerate(RECIPES):
        start=time.perf_counter();scores={};audit=[]
        for fold in range(5):
            test=[r for r in rows if folds[r['pr']]==fold];body={h for r in test for h in r['target_body_sha256']};diff={r['diff_state_sha256'] for r in test}
            training=[r for r in rows if folds[r['pr']]!=fold and not set(r['target_body_sha256'])&body and r['diff_state_sha256'] not in diff]
            model=fit(training,recipe);scores.update(score(model,test,recipe));audit.append({'fold':fold,'training_ids':[r['unit_id'] for r in training],'test_ids':[r['unit_id'] for r in test]})
        selection=select_threshold(scores,labels,.98);drops=set(selection['dropped_ids']);reduced=sum(r['source_bytes'] for r in rows if r['unit_id'] in drops)
        result={'recipe':recipe,'scores':scores,'selection':selection,'source_bytes_reduced':reduced,'source_bytes_total':sum(r['source_bytes'] for r in rows),'fold_audit':audit,'elapsed_seconds':time.perf_counter()-start};result['id']=digest(result);write_new(a.output/f'candidate-{idx}.json',result);results.append(result)
        print(json.dumps({'recipe':recipe,'recall':selection['relevance_recall'],'proposed_functions':selection['proposed_dropped'],'source_byte_reduction':reduced/result['source_bytes_total']}),flush=True)
    best=max(results,key=lambda r:r['source_bytes_reduced']);model=fit(rows,best['recipe']);params=cc.bundle(model,('code','mixed',3.));params.pop('recipe')
    artifact={'schema':1,'kind':'operation-linear-classifier','recipe':best['recipe'],'parameters':params,'threshold':best['selection']['threshold'],'selected_result_id':best['id'],'development_inputs_id':data['id'],'evidence_contract':data['evidence_contract'],'sklearn_version':sklearn.__version__,'calibrated':False,'optimization_enabled':False};artifact['id']=digest(artifact);write_new(a.output/'model.json',artifact)
    x=score(model,rows,best['recipe']);y=score(restore(artifact),rows,best['recipe']);delta=max(abs(x[k]-y[k]) for k in x if x[k] is not None)
    if delta>1e-12:raise ValueError('model roundtrip mismatch')
    write_new(a.output/'replay.json',{'model_id':artifact['id'],'rows':len(rows),'maximum_score_delta':delta,'missing_scores_match':[k for k in x if x[k] is None]==[k for k in y if y[k] is None]})


def predict(a):
    import sklearn
    data=checked(a.inputs);model=checked(a.model);selection=checked(a.selection)
    if selection['model_id']!=model['id'] or selection['inputs_id']!=data['id']:raise ValueError('frozen selection mismatch')
    if model['evidence_contract']!=data['evidence_contract'] or model['evidence_contract']!=contract(a.extractor) or model['sklearn_version']!=sklearn.__version__:raise ValueError('model/evidence contract mismatch')
    if not math.isfinite(model['threshold']) or tuple(model['recipe']) not in RECIPES:raise ValueError('invalid recipe or threshold')
    start=time.perf_counter();scores=score(restore(model),data['rows'],model['recipe']);elapsed=time.perf_counter()-start
    out={'inputs_id':data['id'],'model_id':model['id'],'threshold':model['threshold'],'scores':scores,'elapsed_seconds':elapsed,'predictions':[{'unit_id':uid,'review_score':s,'disposition':'predicted' if s is not None else 'incomplete_operation_evidence','proposal':'lower_priority' if s is not None and s<model['threshold'] else 'retain','action':'full_review','optimization_eligible':False} for uid,s in scores.items()],'optimization_enabled':False};out['id']=digest(out);write_new(a.output,out)


def main():
    p=argparse.ArgumentParser(description=__doc__);s=p.add_subparsers(dest='command',required=True)
    b=s.add_parser('build')
    for key in ('config','extractor','output'):b.add_argument('--'+key,type=Path,required=True)
    b=s.add_parser('train')
    for key in ('inputs','reserved-inputs','extractor','output'):b.add_argument('--'+key,type=Path,required=True)
    b=s.add_parser('predict')
    for key in ('inputs','model','selection','extractor','output'):b.add_argument('--'+key,type=Path,required=True)
    a=p.parse_args();globals()[a.command](a)
if __name__=='__main__':main()
