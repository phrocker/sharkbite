#!/usr/bin/env python3
# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
"""Compare shadow observations to proposed blinded assessments, never ground truth."""
import argparse
from collections import Counter
import json
from pathlib import Path
import statistics
from shadow import digest, write_new


def load_assessment(path, units):
    assessment=json.loads(path.read_text())
    if assessment.get('status')!='proposed':
        raise ValueError('pilot accepts only explicitly proposed assessment labels')
    indexed={}
    for row in assessment['labels']:
        uid=row.get('unit_id')
        if uid not in units or uid in indexed:
            raise ValueError('missing/duplicate/foreign unit ID in assessment')
        if row.get('label') not in ('review','routine','unknown') or not row.get('witness'):
            raise ValueError('invalid assessment label or missing witness')
        if row.get('pr') != units[uid]['pr']:
            raise ValueError('assessment PR does not match unit')
        indexed[uid]=row
    if set(indexed)!=set(units):
        raise ValueError('incomplete assessment; every unit must have a disposition')
    return assessment,indexed


def build(run, assessment_paths):
    manifest=json.loads((run/'manifest.json').read_text())
    predictions=json.loads((run/'predictions.json').read_text())
    protocol=json.loads((run/'protocol.json').read_text())
    if manifest['id'] != digest({k:v for k,v in manifest.items() if k!='id'}):
        raise ValueError('manifest digest mismatch')
    if predictions['id'] != digest({k:v for k,v in predictions.items() if k!='id'}):
        raise ValueError('predictions digest mismatch')
    if predictions['manifest_id'] != manifest['id'] or digest(protocol)!=manifest['protocol_sha256']:
        raise ValueError('mismatched experiment records')
    units={u['id']:u for c in manifest['cases'] for u in c['units']}
    if len(units)!=sum(len(c['units']) for c in manifest['cases']):
        raise ValueError('duplicate manifest unit')
    indexed={p['unit_id']:p for p in predictions['predictions']}
    if set(indexed)!=set(units) or len(indexed)!=len(predictions['predictions']):
        raise ValueError('prediction dispositions do not cover manifest exactly')
    if any(p['action']!='full_review' or p['optimization_eligible'] for p in indexed.values()):
        raise ValueError('pilot must preserve full review for every unit')
    assessors=[]
    comparisons=[]
    assessor_names=set()
    assessment_digests=set()
    for path in assessment_paths:
        assessment,labels=load_assessment(path,units)
        name=assessment.get('assessor',path.stem)
        assessment_digest=digest(assessment)
        if name in assessor_names or assessment_digest in assessment_digests:
            raise ValueError('duplicate assessor name or assessment digest')
        assessor_names.add(name)
        assessment_digests.add(assessment_digest)
        eligible=[p for p in indexed.values() if p['disposition']=='predicted']
        cross=Counter((p['answer']['choice'],labels[p['unit_id']]['label']) for p in eligible)
        base=Counter((p['baseline'],labels[p['unit_id']]['label']) for p in eligible)
        assessors.append({'assessor':name,'sha256':digest(assessment),
                          'label_counts':dict(Counter(x['label'] for x in labels.values())),
                          'eligible_cross_tab':[{'model':m,'proposed_reviewer':r,'count':n} for (m,r),n in sorted(cross.items())],
                          'eligible_baseline_cross_tab':[{'baseline':m,'proposed_reviewer':r,'count':n} for (m,r),n in sorted(base.items())],
                          'limitations':assessment.get('limitations',[])})
        comparisons.append((name,labels))
    observations=[]
    for uid,p in indexed.items():
        observations.append({'unit_id':uid,'pr':units[uid]['pr'],'path':units[uid]['path'],'symbol':units[uid]['symbol'],
                             'disposition':p['disposition'],'model':p.get('answer',{}).get('choice'),
                             'baseline':p['baseline'],'assessments':{name:labels[uid] for name,labels in comparisons}})
    times=[p['elapsed_ms'] for p in indexed.values() if 'elapsed_ms'in p]
    report={'schema':1,'protocol_id':protocol['id'],'mode':protocol['mode'],
            'manifest_id':manifest['id'],'predictions_id':predictions['id'],'exclusion_enabled':False,
            'population':{'prs':len(manifest['cases']),'files':sum(len(c['files']) for c in manifest['cases']),
                          'units':len(units),'functions':sum(u['kind']=='function' for u in units.values())},
            'dispositions':dict(Counter(p['disposition'] for p in indexed.values())),
            'model_labels':dict(Counter(p['answer']['choice'] for p in indexed.values() if 'answer'in p)),
            'latency_ms':{'count':len(times),'first':times[0] if times else None,
                          'median':statistics.median(times) if times else None,'max':max(times) if times else None},
            'assessors':assessors,'observations':observations,
            'limitations':['Retrospective convenience cohort; not held-out quality evidence.',
                           'Proposed frontier relevance labels; no independently confirmed defect oracle.',
                           'Function input excludes dependency bodies; no unit is eligible for exclusion.',
                           'No paired reduced-context downstream reviews, realized token savings, or total-cost estimate.',
                           'Local content-addressed experiment artifacts are not production Shoal decision/API receipts.']}
    if len(comparisons)>=2:
        report['reviewer_agreement']={'count':sum(len({labels[uid]['label'] for _,labels in comparisons})==1 for uid in units),
                                      'denominator':len(units),'meaning':'agreement of proposed labels, not correctness'}
    report['id']=digest(report)
    return report


def markdown(report):
    pop=report['population'];disp=report['dispositions'];lat=report['latency_ms']
    median='n/a' if lat['median'] is None else f"{lat['median']:.2f} ms"
    first='n/a' if lat['first'] is None else f"{lat['first']:.2f} ms"
    text=f'''# Retrospective authorization shadow pilot

Protocol: `{report['protocol_id']}`. Report: `{report['id']}`.

This run preserves full review for every input. Frontier assessments are proposed
relevance labels; the tables below measure agreement, not defect recall or accuracy.

| Observation | Count |
| --- | ---: |
| Pinned PRs | {pop['prs']} |
| Changed files accounted for | {pop['files']} |
| Changed/disposition units | {pop['units']} |
| Changed Go functions/methods | {pop['functions']} |
| Complete function inputs sent to Laya | {disp.get('predicted',0)} |
| Functions withheld from Laya due to token limit | {disp.get('token_limit',0)} |
| Non-function/unsupported units retained for full review | {disp.get('unsupported_unit',0)} |

Raw Laya labels: `{json.dumps(report['model_labels'],sort_keys=True)}`.
Median inference time: {median} across {lat['count']} requests;
first request: {first}. These are local calls, not service throughput.

'''
    for assessor in report['assessors']:
        text+=f"## Proposed assessment: {assessor['assessor']}\n\n"
        text+="All-unit labels: `"+json.dumps(assessor['label_counts'],sort_keys=True)+"`.\n\n"
        text+="Only the inputs that fit the model are compared below. Unknowns remain visible.\n\n| Laya answer | Proposed reviewer label | Count |\n| --- | --- | ---: |\n"
        for row in assessor['eligible_cross_tab']:
            text+=f"| {row['model']} | {row['proposed_reviewer']} | {row['count']} |\n"
        text+="\nBaseline on the same inputs:\n\n| Path/term baseline | Proposed reviewer label | Count |\n| --- | --- | ---: |\n"
        for row in assessor['eligible_baseline_cross_tab']:
            text+=f"| {row['baseline']} | {row['proposed_reviewer']} | {row['count']} |\n"
        text+='\n'
    if 'reviewer_agreement'in report:
        a=report['reviewer_agreement'];text+=f"Reviewers agree on {a['count']}/{a['denominator']} proposed labels. Agreement does not verify those labels.\n\n"
    text+='## Limits\n\n'+'\n'.join('- '+x for x in report['limitations'])+'\n'
    return text


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--run',type=Path,required=True)
    parser.add_argument('--assessment',type=Path,action='append',default=[])
    args=parser.parse_args()
    report=build(args.run,args.assessment)
    write_new(args.run/'report.json',report)
    with (args.run/'report.md').open('x') as stream:stream.write(markdown(report))
    print(json.dumps({'report':report['id'],'population':report['population'],'assessors':len(report['assessors'])}))

if __name__=='__main__':main()
