#!/usr/bin/env python3
# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
"""Reconstruct learning receipts and optionally refit the frozen V9 winner."""
import argparse
import hashlib
import importlib.util
import json
from pathlib import Path
import sys

CORE = Path(__file__).resolve().parents[2] / 'experiments/decision-learning/learning.py'
spec = importlib.util.spec_from_file_location('learning', CORE)
learning = importlib.util.module_from_spec(spec)
spec.loader.exec_module(learning)


def run(root, output, refit=False):
    def read(path):return json.loads((root/path).read_text())
    def sha(path):return hashlib.sha256((root/path).read_bytes()).hexdigest()
    dev=read('development.json'); held=read('reserved/inputs-frozen.json')
    model=read('trained/model.json'); config=read('trained/config.json')
    from candidates import checked
    for name in ('development.json','reserved/inputs-frozen.json','trained/model.json','reserved/predictions.json'):
        checked(root/name)
    if config['inputs_id']!=dev['id'] or config['reserved_inputs_id']!=held['id']:
        raise ValueError('source configuration mismatch')
    if model['development_inputs_id']!=dev['id'] or model['evidence_contract']!=dev['evidence_contract'] or held['evidence_contract']!=dev['evidence_contract']:
        raise ValueError('model development/evidence lineage mismatch')
    eligible={r['unit_id'] for r in dev['rows'] if model['recipe'][0]=='code' or r['operation_complete']}
    pictures=[];examples=[];assessments=[]
    for name,source in [('development',dev),('reserved',held)]:
        pictures.append(dict(id=source['id'],source_digest=source['id'],builder_digest=learning.digest(source['evidence_contract']),
            ontology_version='none: code plus unresolved syntax-operation observations',mode='reconstructed',
            coverage=[dict(observed=len(source['rows']),total=len(source['rows']),unit='enumerated functions'),
                      dict(observed=sum(r['operation_complete'] for r in source['rows']),total=len(source['rows']),unit='complete operation extraction')],
            gaps=['Dependency completeness unknown','Snapshot is retained input observations, not a resolved ontology graph']))
        for row in source['rows']:
            examples.append(dict(id=row['unit_id'],picture_id=source['id'],family=str(row['pr']),
                                 content_digests=row['target_body_sha256']+[row['diff_state_sha256']]))
            if name=='development':
                assessments.append(dict(id=learning.digest(['development',row['unit_id'],row['label']]),
                    example_id=row['unit_id'],kind='reviewer_assessment',assessor='development-reference',
                    label=row['label'],status='proposed',evidence_digest=source['id']))
    for assessor,path in [('frontier','frontier.json'),('claude','reserved/claude/assessment-normalized.json')]:
        for row in read(path)['records']:
            assessments.append(dict(id=learning.digest([assessor,row]),example_id=row['unit_id'],
                kind='reviewer_assessment',assessor=assessor,label=row['label'],status='proposed',evidence_digest=sha(path)))
    task=dict(id='authorization-relevance-v9',labels=['review','routine'],positive_label='review')
    ledger=learning.seal('ledger',task=task,pictures=pictures,examples=examples,assessments=assessments,
                         provenance='Retrospective import. Content hashes are not authentication or historical receipt times.')
    splits={str(r['pr']):'train' for r in dev['rows']}
    if set(splits)&{str(r['pr']) for r in held['rows']}:raise ValueError('family overlap')
    splits.update({str(r['pr']):'test' for r in held['rows']})
    dp=learning.seal('dataset_policy',task_id=task['id'],family_splits=splits,
                    assessors=['development-reference','frontier','claude'],statuses=['proposed'],training_eligible_ids=sorted(eligible))
    data=learning.dataset(ledger,dp)
    training_ids=sorted(r['example_id'] for r in data['rows'] if r['split']=='train')
    expected=sorted(r['unit_id'] for r in dev['rows'] if r['unit_id'] in config['training_ids'] and r['label']!='unknown' and r['unit_id'] in eligible)
    if training_ids!=expected:raise ValueError('learning manifest differs from frozen fitting membership')
    ep=learning.seal('evaluation_policy',task_id=task['id'],evaluation_ids=sorted(r['unit_id'] for r in held['rows']),
        assessors=['frontier','claude'],statuses=['proposed'],minimum_recall=.98,minimum_positive_count=50,minimum_families=10)
    candidate=learning.seal('candidate',dataset_id=data['id'],evaluation_policy_id=ep['id'],training_ids=training_ids,
        model_digest=sha('trained/model.json'),model_id=model['id'],policy_predeclared=False,
        provenance='New learning-loop gate is post hoc; original frozen V9 protocol retained separately.')
    source_predictions=read('reserved/predictions.json')
    if source_predictions['model_id']!=model['id'] or source_predictions['inputs_id']!=held['id']:
        raise ValueError('source prediction lineage mismatch')
    predictions=learning.seal('predictions',ledger_id=ledger['id'],candidate_id=candidate['id'],
        source_digest=sha('reserved/predictions.json'),rows=[dict(id=r['unit_id'],proposal=r['proposal'],
        audit_priority=1.0 if r['review_score'] is None else 1-abs(r['review_score']-model['threshold'])) for r in source_predictions['predictions']])
    promotion=learning.evaluate(ledger,data,ep,candidate,predictions)
    # Audit only the new cohort; never pretend this retrospective plan selected the already acquired labels.
    audit_ledger=learning.seal('ledger',task=task,pictures=[pictures[1]],
        examples=[e for e in examples if e['picture_id']==held['id']],assessments=[])
    audit_predictions=learning.seal('predictions',ledger_id=audit_ledger['id'],candidate_id=candidate['id'],rows=predictions['rows'])
    audit=learning.audit_plan(audit_ledger,audit_predictions,'v9-replay-only-not-prospective',12,12)
    output.mkdir(parents=True,exist_ok=False)
    artifacts=dict(ledger=ledger,dataset_policy=dp,dataset=data,evaluation_policy=ep,candidate=candidate,
                   predictions=predictions,promotion=promotion,audit_ledger=audit_ledger,audit_predictions=audit_predictions,audit=audit)
    if refit:
        import operation_classifier as op
        import sklearn
        if sklearn.__version__!=model['sklearn_version']:raise ValueError('runtime mismatch')
                # Preserve original ordering: order is part of reproducible fitting.
        train=[r for r in dev['rows'] if r['unit_id'] in set(training_ids)]
        fitted=op.fit(train,model['recipe'])
        scores=op.score(fitted,held['rows'],model['recipe'])
        frozen=source_predictions['scores']
        if set(scores)!=set(frozen) or {k for k,v in scores.items() if v is None}!={k for k,v in frozen.items() if v is None}:
            raise ValueError('refit score coverage/missing mask differs')
        delta=max((abs(scores[k]-frozen[k]) for k in scores if scores[k] is not None),default=0.0)
        if delta>1e-10:raise ValueError('refit differs from frozen candidate')
        artifacts['refit']=learning.seal('refit',dataset_id=data['id'],candidate_id=candidate['id'],
            evaluated_rows=len(scores),training_rows=len(train),maximum_score_delta=delta,
            learner=list(model['recipe']),threshold=model['threshold'],
            runner_digest=hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),optimization_enabled=False)
    for name,value in artifacts.items():
        (output/(name+'.json')).write_text(json.dumps(value,indent=2,allow_nan=False)+'\n')
    print(json.dumps(dict(training_rows=len(training_ids),quarantined=len(data['excluded']),
        evaluation_rows=len(ep['evaluation_ids']),promotion=promotion['disposition'],reasons=promotion['reasons'],
        metrics=promotion['metrics'],refit=artifacts.get('refit')),indent=2))


if __name__=='__main__':
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--artifacts',type=Path,required=True);p.add_argument('--output',type=Path,required=True)
    p.add_argument('--refit',action='store_true');a=p.parse_args();run(a.artifacts,a.output,a.refit)
