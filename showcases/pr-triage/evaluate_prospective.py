#!/usr/bin/env python3
# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
"""Evaluate frozen prospective samples without merging reviewer disagreements."""
import argparse
import json
import math
import sqlite3
from pathlib import Path
from candidates import checked
from shadow import digest, write_new
from paid_review import micros, receipt
import prepare_references
import paid_review
from prospective_snapshot import validate_observation_order
from verify_prospective_scores import validate_inputs, verify_scores
from verify_prospective_selection import verify_selection


def assessment(root,manifest,protocol,pass_name,batches):
    expected={u['id'] for c in manifest['cases'] for u in c['units']}
    records=[];sources=[]
    for n,batch in enumerate(batches['batches']):
        call_id=f"pr{batch['pr']}-{pass_name}-b{n}"
        response=root/'calls'/call_id/'response.json';call=json.loads((response.parent/'call.json').read_text())
        if call.get('call_id')!=call_id or call.get('runner_sha256')!=digest(Path(paid_review.__file__).read_bytes()):
            raise ValueError('review call identity or runner mismatch')
        if call['protocol_id']!=protocol['id'] or call['prompt_sha256']!=batch['sha256'] or call['response_sha256']!=digest(response.read_bytes()):
            raise ValueError('review response provenance mismatch')
        if call['started_at_unix'] <= manifest['observed_at_unix']:
            raise ValueError('review predates snapshot acquisition')
        raw=json.loads(response.read_text())
        if raw.get('is_error') or call['exit_code']:raise ValueError('failed assessment')
        text=raw['result'].strip()
        if text.startswith('```json') and text.endswith('```'):text=text[7:-3].strip()
        value=json.loads(text)
        if value.get('status')!='proposed':raise ValueError('unexpected assessment status')
        rows=value['records'];ids=[r['unit_id'] for r in rows]
        if len(ids)!=len(set(ids)) or set(ids)!=set(batch['unit_ids']):raise ValueError('assessment batch coverage mismatch')
        if any(r['label'] not in ('review','routine','unknown') or not r['witness'].strip() for r in rows):raise ValueError('invalid assessment label or witness')
        records.extend(rows);sources.append({'call_id':call_id,'response_sha256':digest(response.read_bytes()),'model_usage':raw.get('modelUsage')})
    if len(records)!=len(expected) or {r['unit_id'] for r in records}!=expected:raise ValueError('incomplete assessment')
    out={'status':'proposed','assessor':'claude-pass-'+pass_name,'protocol_id':protocol['id'],'manifest_id':manifest['id'],
         'records':records,'source_calls':sources,'normalization':'Only optional JSON fence removed; IDs and labels unmodified.'}
    out['id']=digest(out);return out


def metric(labels,pred,ids,inputs):
    positive={i for i in ids if labels[i]=='review'};lower={i for i in ids if pred['scores'][i] is not None and pred['scores'][i]<pred['threshold']}
    missed=positive&lower;unknown={i for i in lower if labels[i]=='unknown'}
    volumes={r['unit_id']:r['source_bytes'] for r in inputs['rows']}
    total=sum(volumes[i] for i in ids)
    return {'units':len(ids),'positive':len(positive),'retained_positive':len(positive-missed),'recall':len(positive-missed)/len(positive) if positive else None,
            'lower_priority_ids':sorted(lower),'missed_ids':sorted(missed),'unknown_lowered_ids':sorted(unknown),
            'source_bytes':total,'proposed_lower_priority_source_bytes':sum(volumes[i] for i in lower),
            'proposed_source_byte_reduction':sum(volumes[i] for i in lower)/total if total else None}


def read_budget(root,protocol,refs):
    path=root/'budget.sqlite'
    # URI read-only mode rejects missing files instead of creating a $0 ledger.
    db=sqlite3.connect(path.resolve().as_uri()+'?mode=ro',uri=True)
    try:
        if db.execute('SELECT id FROM experiment').fetchall()!=[(protocol['id'],)]:
            raise ValueError('budget protocol mismatch')
        budget=receipt(db,protocol)
    finally:db.close()
    rows={r['id']:r for r in budget['calls']}
    for ref in refs.values():
        for source in ref['source_calls']:
            cid=source['call_id'];call=json.loads((root/'calls'/cid/'call.json').read_text())
            if cid not in rows or rows[cid]['prompt_sha256']!=call['prompt_sha256']:
                raise ValueError('assessed call absent from budget or prompt mismatch')
            row=rows[cid]
            raw=json.loads((root/'calls'/cid/'response.json').read_text())
            if row['status']=='reserved':
                if 'total_cost_usd' in raw:
                    raise ValueError('priced response requires settled budget entry')
                if row['charged_microusd']!=micros(protocol['budget']['call_reservation_usd']):
                    raise ValueError('unpriced call lost its reservation')
            else:
                raw=json.loads((root/'calls'/cid/'response.json').read_text())
                if row['charged_microusd']!=micros(raw['total_cost_usd']):
                    raise ValueError('reported cost differs from settled ledger')
    return budget


def unique_ids(values, name):
    ids=set(values)
    if len(values)!=len(ids):raise ValueError('duplicate '+name)
    return ids


def validate_membership(manifest,picture,inputs,predictions):
    unique_ids([c['pr'] for c in manifest['cases']], 'PR cases')
    ids=unique_ids([u['id'] for c in manifest['cases'] for u in c['units']], 'manifest unit IDs')
    populations=[unique_ids(picture['sample_ids'], 'sample IDs'),
        unique_ids([r['unit_id'] for r in inputs['rows']], 'input IDs'),
        unique_ids([r['unit_id'] for r in predictions['predictions']], 'prediction IDs'),set(predictions['scores'])]
    if any(population!=ids for population in populations):raise ValueError('input/prediction membership mismatch')
    for value in predictions['scores'].values():
        if value is not None and (type(value) not in (int,float) or not math.isfinite(value) or not 0<=value<=1):
            raise ValueError('invalid frozen score')
    if predictions['optimization_enabled'] is not False or any(r['action']!='full_review' or r['optimization_eligible'] is not False for r in predictions['predictions']):
        raise ValueError('non-shadow action in experiment')
    return ids


def validate_prompts(root,manifest,protocol,batches):
    if batches['runner_sha256']!=digest(Path(prepare_references.__file__).read_bytes()):
        raise ValueError('reference preparer identity mismatch')
    rendered=prepare_references.prompts(manifest,protocol)
    expected=[]
    for n,(pr,units,prompt) in enumerate(rendered):
        raw=prompt.encode()
        expected.append({'path':f'pr-{pr}-batch-{n}.txt','pr':pr,'unit_ids':[u['unit_id'] for u in units],
                         'sha256':digest(raw),'bytes':len(raw)})
    if batches['batches']!=expected:raise ValueError('noncanonical reference batches')
    # Read only the reconstructed paths, after validating the entire receipt.
    for batch,(_,_,prompt) in zip(expected,rendered):
        if (root/'prompts'/batch['path']).read_bytes()!=prompt.encode():
            raise ValueError('noncanonical reference prompt')


def temporal_groups(manifest,picture):
    families={c['pr'] for c in manifest['cases']}
    coverage=picture['coverage']
    if unique_ids([c['family'] for c in coverage],'coverage families')!=families:
        raise ValueError('temporal coverage membership mismatch')
    strata={c['family']:c['stratum'] for c in coverage}
    if set(strata.values())-{'existing_at_registration','created_after_registration'}:
        raise ValueError('unknown temporal stratum')
    result={}
    for case in manifest['cases']:
        group=result.setdefault(strata[case['pr']],{'families':[],'ids':set()})
        group['families'].append(case['pr']);group['ids'].update(u['id'] for u in case['units'])
    return result


def gate_reasons(strata,gate):
    reasons=[]
    if not strata:reasons.append('Empty eligible cohort; collection remains pending')
    for stratum,group in strata.items():
        if len(group['families'])<gate['minimum_families']:reasons.append(stratum+': insufficient family count')
        for name,result in group['assessments'].items():
            m=result['without_exact_development_overlap']
            if m['positive']<gate['minimum_positive_count_per_assessor']:reasons.append(stratum+'/'+name+': insufficient novel positives')
            if m['recall'] is None or m['recall']<gate['minimum_recall'] or m['unknown_lowered_ids']:reasons.append(stratum+'/'+name+': quality gate unmet')
    return reasons


def main():
    p=argparse.ArgumentParser(description=__doc__)
    for k in ('run','protocol','model','development','output'):p.add_argument('--'+k,type=Path,required=True)
    a=p.parse_args();root=a.run;protocol=checked(a.protocol);model=checked(a.model);inputs=checked(root/'inputs.json')
    manifest=checked(root/'snapshot/sampled-manifest.json');picture=checked(root/'snapshot/picture.json')
    predictions=checked(root/'predictions.json');batches=checked(root/'prompts/batches.json');dev=checked(a.development)
    if digest(a.model.read_bytes())!=protocol['model_sha256'] or model['id']!=protocol['model_id'] or model['threshold']!=protocol['threshold']:
        raise ValueError('registered model mismatch')
    if dev['id']!=protocol['training_inputs_id']:raise ValueError('development identity mismatch')
    if predictions['model_id']!=model['id'] or predictions['inputs_id']!=inputs['id'] or predictions['threshold']!=protocol['threshold']:
        raise ValueError('prediction lineage mismatch')
    inventory=checked(root/'inventory.json')
    if inventory['id']!=picture['inventory_id'] or inventory['registered_protocol_id']!=protocol['id']:
        raise ValueError('snapshot inventory provenance mismatch')
    validate_observation_order(manifest,protocol,inventory)
    verify_selection(root,protocol,inventory,picture,manifest)
    ids=validate_membership(manifest,picture,inputs,predictions)
    if picture['protocol_id']!=protocol['id'] or manifest['picture_id']!=picture['id'] or batches['protocol_id']!=protocol['id'] or batches['manifest_id']!=manifest['id']:
        raise ValueError('snapshot/prompt provenance mismatch')
    validate_inputs(manifest,inputs,model)
    verify_scores(root,a.model,predictions)
    validate_prompts(root,manifest,protocol,batches)
    bodies={h for r in dev['rows'] for h in r['target_body_sha256']};diffs={r['diff_state_sha256'] for r in dev['rows']}
    overlap={r['unit_id'] for r in inputs['rows'] if set(r['target_body_sha256'])&bodies or r['diff_state_sha256'] in diffs}
    results={};refs={}
    for pass_name in ('a','b'):
        ref=assessment(root,manifest,protocol,pass_name,batches);refs[pass_name]=ref
        labels={r['unit_id']:r['label'] for r in ref['records']}
        results[pass_name]={'all':metric(labels,predictions,ids,inputs),'without_exact_development_overlap':metric(labels,predictions,ids-overlap,inputs)}
    strata={}
    for name,group in temporal_groups(manifest,picture).items():
        strata[name]={'families':group['families'],'assessments':{}}
        for assessor,ref in refs.items():
            labels={r['unit_id']:r['label'] for r in ref['records']}
            strata[name]['assessments'][assessor]={'all':metric(labels,predictions,group['ids'],inputs),
                'without_exact_development_overlap':metric(labels,predictions,group['ids']-overlap,inputs)}
    la={r['unit_id']:r['label'] for r in refs['a']['records']};lb={r['unit_id']:r['label'] for r in refs['b']['records']}
    disagreements=sorted(i for i in ids if la[i]!=lb[i]);families=len(manifest['cases']);gate=protocol['evaluation']
    reasons=gate_reasons(strata,gate)
    budget=read_budget(root,protocol,refs)
    out={'protocol_id':protocol['id'],'picture_id':picture['id'],'manifest_id':manifest['id'],'model_id':model['id'],
        'prediction_id':predictions['id'],'assessment_ids':{name:ref['id'] for name,ref in refs.items()},'assessments':results,'disagreement_ids':disagreements,'exact_development_overlap_ids':sorted(overlap),
        'temporal_strata':strata,'aggregate_metrics_scope':'Descriptive totals only; quality gates applied separately per temporal stratum.',
        'scores_recomputed':True,'selection_rebuilt':True,'family_count':families,'budget':budget,'status':'hold' if reasons else 'shadow_evidence_only','reasons':reasons,
        'real_action':'full_review','optimization_enabled':False,
        'limitations':['Same-provider repeated assessments are correlated, not two independent model families.',
          'Temporal strata present: '+', '.join(sorted(strata))+'. Existing snapshots do not establish future-created-PR generalization.',
          'Only sampled functions assessed; no full-PR defect recall or downstream token savings measured.',
          'Exact hashes do not detect near-duplicates or policy-family dependence.']}
    out['id']=digest(out);a.output.mkdir()
    for name,ref in refs.items():write_new(a.output/('assessment-'+name+'.json'),ref)
    write_new(a.output/'report.json',out)
    print(json.dumps({'status':out['status'],'metrics':results,'budget':budget,'disagreements':len(disagreements),'overlap':len(overlap),'reasons':reasons},indent=2))


if __name__=='__main__':main()
