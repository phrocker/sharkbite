#!/usr/bin/env python3
# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
"""Replay frozen cached evidence without running or requiring its Go extractor."""
import argparse
import math
from pathlib import Path
import time
import operation_classifier as op
import context_classifier as cc
from candidates import checked
from shadow import digest, write_new


def main():
    import sklearn
    p=argparse.ArgumentParser(description=__doc__)
    for key in ('inputs','model','selection','output'):p.add_argument('--'+key,type=Path,required=True)
    a=p.parse_args();inputs=checked(a.inputs);model=checked(a.model);selection=checked(a.selection)
    if selection['model_id']!=model['id'] or selection['inputs_id']!=inputs['id']:raise ValueError('frozen evidence/model mismatch')
    if inputs['evidence_contract']!=model['evidence_contract']:raise ValueError('cached evidence contract mismatch')
    expected={'runner_sha256':Path(op.__file__),'base_classifier_sha256':Path(cc.__file__),'candidate_helpers_sha256':Path(__file__).with_name('candidates.py'),'source_helpers_sha256':Path(__file__).with_name('shadow.py')}
    if any(digest(path.read_bytes())!=model['evidence_contract'][key] for key,path in expected.items()):raise ValueError('inference dependency mismatch')
    if model['sklearn_version']!=sklearn.__version__ or tuple(model['recipe']) not in op.RECIPES or not math.isfinite(model['threshold']):raise ValueError('invalid model recipe')
    start=time.perf_counter();scores=op.score(op.restore(model),inputs['rows'],model['recipe']);elapsed=time.perf_counter()-start
    out={'inputs_id':inputs['id'],'model_id':model['id'],'threshold':model['threshold'],'scores':scores,'elapsed_seconds':elapsed,'predictions':[{'unit_id':uid,'review_score':score,'disposition':'predicted' if score is not None else 'incomplete_operation_evidence','proposal':'lower_priority' if score is not None and score<model['threshold'] else 'retain','action':'full_review','optimization_eligible':False} for uid,score in scores.items()],'optimization_enabled':False,'replay_runner_sha256':digest(Path(__file__).read_bytes()),'mode':'cached frozen representation; extractor fingerprint bound by inputs/model, extractor not executed'}
    out['id']=digest(out);write_new(a.output,out)

if __name__=='__main__':main()
