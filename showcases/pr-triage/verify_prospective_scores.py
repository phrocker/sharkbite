#!/usr/bin/env python3
# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
"""Bind evaluated code features to source and recompute frozen model outputs."""
from pathlib import Path
import subprocess
import sys
import tempfile
from candidates import checked, original_state, change_text
from shadow import canonical, digest


def validate_inputs(manifest, inputs, model):
    if model['recipe'][0]!='code':raise ValueError('source verification supports the frozen code-only model')
    if inputs['sources']!=[{'manifest_id':manifest['id'],'assessment_sha256':None}]:
        raise ValueError('input source manifest mismatch')
    if inputs['evidence_contract']!=model['evidence_contract']:
        raise ValueError('input evidence contract mismatch')
    units={u['id']:u for c in manifest['cases'] for u in c['units']}
    for row in inputs['rows']:
        unit=units[row['unit_id']];state=original_state(unit)
        expected={'pr':unit['pr'],'path':unit['path'],'symbol':unit['symbol'],'label':'unknown',
            'target_body_sha256':[digest(state[s].encode()) for s in ('before','after') if state[s]],
            'code_state_sha256':digest(state),'diff_state_sha256':digest(change_text(unit)),
            'source_bytes':sum(len(state[s].encode()) for s in ('before','after')),'diff_bytes':len(change_text(unit).encode())}
        if row['text']['code']!=canonical(state) or any(row.get(k)!=v for k,v in expected.items()):
            raise ValueError('input features or metadata differ from sampled source')


def compare_replay(predictions, replay):
    if predictions['scores']!=replay['scores'] or predictions['predictions']!=replay['predictions']:
        raise ValueError('predictions differ from independently recomputed frozen model')


def verify_scores(root, model_path, predictions):
    runner=Path(__file__).with_name('replay_operation_model.py')
    if predictions.get('replay_runner_sha256')!=digest(runner.read_bytes()):
        raise ValueError('prediction replay runner mismatch')
    with tempfile.TemporaryDirectory(prefix='shoal-score-check-') as tmp:
        output=Path(tmp)/'replay.json'
        # Existing pinned replay verifies numerical runtime and dependency hashes.
        subprocess.run([sys.executable,str(runner),'--inputs',str(root/'inputs.json'),
                        '--model',str(model_path),'--selection',str(root/'selection.json'),
                        '--output',str(output)],check=True,capture_output=True)
        compare_replay(predictions,checked(output))
