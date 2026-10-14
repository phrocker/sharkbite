#!/usr/bin/env python3
# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
"""Replay a frozen experimental Laya adapter with explicit source coverage."""
import argparse
import importlib.metadata
import inspect
import json
import math
import os
from pathlib import Path
import time

from candidates import checked, change_text, render
from shadow import canonical, digest, exact_fit, write_new


def representation_id():
    return digest(inspect.getsource(render) + inspect.getsource(change_text) + inspect.getsource(canonical))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--selection', type=Path, required=True)
    parser.add_argument('--training', type=Path, required=True)
    parser.add_argument('--manifest', type=Path, required=True)
    parser.add_argument('--cache', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    selection = checked(args.selection)
    manifest = checked(args.manifest)
    recipe = json.loads((args.training/'config.json').read_text())
    weights_path = args.training/'adapter.safetensors'
    if (manifest['id'] != selection['validation_manifest_id'] or digest(recipe) != selection['training_recipe_sha256']
            or digest(weights_path.read_bytes()) != selection['weights_sha256']
            or representation_id() != selection['representation_sha256'] or recipe['representation'] != 'diff'
            or digest(Path(__file__).read_bytes()) != selection['runner_sha256']):
        raise ValueError('frozen candidate identity mismatch')
    os.environ.update(HF_HOME=str(args.cache), HF_HUB_OFFLINE='1', TRANSFORMERS_OFFLINE='1',
                      HF_HUB_DISABLE_TELEMETRY='1', TOKENIZERS_PARALLELISM='false', USE_TF='0')
    import torch
    from laya import Router
    from laya.common import collate_items
    from safetensors.torch import load_file
    if importlib.metadata.version('laya') != recipe['runtime'] or not torch.cuda.is_available():
        raise ValueError('pinned GPU runtime required')
    router = Router(device='cuda', revision=recipe['revision'])
    agent = router.load('english')
    model = agent.model

    class LowRankLinear(torch.nn.Module):
        def __init__(self, base):
            super().__init__()
            self.base = base
            self.lora_a = torch.nn.Linear(base.in_features, recipe['rank'], bias=False, device=base.weight.device, dtype=base.weight.dtype)
            self.lora_b = torch.nn.Linear(recipe['rank'], base.out_features, bias=False, device=base.weight.device, dtype=base.weight.dtype)

        def forward(self, value):
            return self.base(value) + self.lora_b(self.lora_a(value)) * (recipe['alpha']/recipe['rank'])

    modules = [(name, module) for name, module in model.encoder.named_modules() if isinstance(module, torch.nn.Linear)]
    expected_modules = json.loads((args.training/'adapter-layout.json').read_text())['linear_modules']
    if [name for name, _ in modules] != expected_modules:
        raise ValueError('adapter module layout mismatch')
    for name, module in modules:
        parent_name, _, leaf = name.rpartition('.')
        parent = model.encoder.get_submodule(parent_name) if parent_name else model.encoder
        setattr(parent, leaf, LowRankLinear(module))
    weights = load_file(str(weights_path))
    expected = {name for name, _ in model.named_parameters()
                if (('.lora_a.' in name or '.lora_b.' in name) if name.startswith('encoder.') else not name.startswith('act_head.'))}
    if set(weights) != expected or any(not torch.isfinite(value).all() for value in weights.values()):
        raise ValueError('incomplete or invalid adapter weights')
    model.load_state_dict(weights, strict=False)
    model.eval()
    question = recipe['question']
    options = list(question['criteria'])
    internal = {'boundary': agent._to_internal(question)}
    predictions = []
    for case in manifest['cases']:
        for unit in case['units']:
            row = {'unit_id': unit['id'], 'pr': unit['pr'], 'action': 'full_review', 'optimization_eligible': False,
                   'disposition': 'unsupported_unit', 'review_score': None}
            if unit['kind'] == 'function':
                state = render(unit, 'diff')
                row['state_sha256'] = digest(state)
                row['preflight'] = exact_fit(agent, state, question, recipe['max_len'], recipe['head_max_len'])
                row['disposition'] = 'token_limit'
                if row['preflight']['fits']:
                    batch = collate_items([agent._encode_state(state, ['boundary'], internal, recipe['max_len'], recipe['head_max_len'])], agent.tok.pad_token_id)
                    start = time.perf_counter()
                    with torch.no_grad(), torch.autocast('cuda', dtype=torch.float16):
                        logits, _ = model(batch['input_ids'].cuda(), batch['attention_mask'].cuda(), batch['marker_pos'].cuda(), batch['marker_mask'].cuda(), batch['qtype'].cuda())
                    score = float(logits.softmax(-1)[0, options.index('review')])
                    if not math.isfinite(score):
                        raise ValueError('nonfinite adapter score')
                    row.update(review_score=score, disposition='predicted', elapsed_ms=(time.perf_counter()-start)*1000)
            row['proposal'] = 'lower_priority' if row['review_score'] is not None and row['review_score'] < selection['threshold'] else 'retain'
            predictions.append(row)
    result = {'manifest_id': manifest['id'], 'selection_id': selection['id'], 'predictions': predictions,
              'optimization_enabled': False, 'runner_sha256': digest(Path(__file__).read_bytes())}
    result['id'] = digest(result)
    write_new(args.output, result)


if __name__ == '__main__':
    main()
