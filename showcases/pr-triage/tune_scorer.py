#!/usr/bin/env python3
# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
"""Experimental scorer-only Laya adaptation, with leave-one-PR-out development predictions.

Encoder and transformer head stay pinned. Cached option representations feed the
existing scorer. Reference labels remain relevance proposals, not bug truth.
This uses supervised cross entropy, not the upstream RLCD training recipe.
"""
import argparse
import copy
import importlib.metadata
import json
import os
from pathlib import Path
import random
import time

from candidates import checked
from ranking import load_labels, select_threshold
from shadow import digest, exact_fit, write_new


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--config', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--cache', type=Path, required=True)
    args = parser.parse_args()
    config = json.loads(args.config.read_text())
    os.environ.update(HF_HOME=str(args.cache), HF_HUB_OFFLINE='1', TRANSFORMERS_OFFLINE='1',
                      HF_HUB_DISABLE_TELEMETRY='1', TOKENIZERS_PARALLELISM='false', USE_TF='0')
    import torch
    from laya import Router
    from laya.common import collate_items
    from safetensors.torch import save_file
    if importlib.metadata.version('laya') != config['runtime'] or not torch.cuda.is_available():
        raise ValueError('pinned GPU runtime required')
    torch.set_num_threads(4)
    args.output.mkdir(parents=True, exist_ok=False)
    write_new(args.output / 'config.json', config)
    router = Router(device='cuda', revision=config['revision'])
    agent = router.load('english')
    agent.model.eval()
    initial = copy.deepcopy(agent.model.scorer).float()
    question = config['question']
    internal = {'boundary': agent._to_internal(question)}
    option_names = list(question['criteria'])
    if set(option_names) != {'review', 'routine'}:
        raise ValueError('binary question required')
    captured = []
    handle = agent.model.scorer.register_forward_pre_hook(lambda module, inputs: captured.append(inputs[0].detach().float().cpu()))
    datasets = {}
    for split in ('development', 'exploratory'):
        spec = config[split]
        manifest = checked(Path(spec['manifest']))
        inputs = checked(Path(spec['inputs']))
        if inputs['manifest_id'] != manifest['id']:
            raise ValueError('input/manifest identity mismatch')
        funcs = {u['id']: u for c in manifest['cases'] for u in c['units'] if u['kind'] == 'function'}
        if spec.get('assessment'):
            assessment, labels = load_labels(Path(spec['assessment']), funcs)
        elif split == 'exploratory':
            labels = {uid: 'unknown' for uid in funcs}
        else:
            raise ValueError('development labels required')
        records = []
        for uid, unit in funcs.items():
            state = inputs['representations'][uid]['original']
            row = {'unit_id': uid, 'pr': unit['pr'], 'label': labels[uid], 'state_sha256': digest(state)}
            fit = exact_fit(agent, state, question, config['max_len'], config['head_max_len'])
            row['preflight'] = fit
            if fit['fits']:
                batch = collate_items([agent._encode_state(state, ['boundary'], internal,
                                                          config['max_len'], config['head_max_len'])], agent.tok.pad_token_id)
                captured.clear()
                start = time.perf_counter()
                with torch.inference_mode():
                    agent._infer(batch)
                row['feature_ms'] = (time.perf_counter() - start) * 1000
                if len(captured) != 1 or str(agent.device) != 'cuda' or getattr(agent, 'cpu_fallback_count', 0):
                    raise ValueError('unexpected scorer capture or CPU fallback')
                row['features'] = captured[0][0]
            records.append(row)
        datasets[split] = records
        print(json.dumps({'split': split, 'units': len(records), 'features': sum('features' in r for r in records)}), flush=True)
    handle.remove()
    feature_tensors = {r['unit_id']: r['features'] for rows in datasets.values() for r in rows if 'features' in r}
    save_file(feature_tensors, str(args.output / 'option-features.safetensors'))
    router.unload()
    del agent
    torch.cuda.empty_cache()

    def fit(rows):
        torch.manual_seed(config['seed'])
        scorer = copy.deepcopy(initial).cuda().train()
        eligible = [r for r in rows if 'features' in r and r['label'] in option_names]
        if len({r['label'] for r in eligible}) != 2:
            raise ValueError('training fold needs both classes')
        optimizer = torch.optim.AdamW(scorer.parameters(), lr=config['learning_rate'], weight_decay=config['weight_decay'])
        generator = random.Random(config['seed'])
        for epoch in range(config['epochs']):
            generator.shuffle(eligible)
            for offset in range(0, len(eligible), config['batch_size']):
                batch = eligible[offset:offset + config['batch_size']]
                x = torch.stack([r['features'] for r in batch]).cuda()
                y = torch.tensor([option_names.index(r['label']) for r in batch], device='cuda')
                optimizer.zero_grad(set_to_none=True)
                loss = torch.nn.functional.cross_entropy(scorer(x).squeeze(-1), y)
                if not torch.isfinite(loss):
                    raise ValueError('nonfinite training loss')
                loss.backward()
                torch.nn.utils.clip_grad_norm_(scorer.parameters(), 1.)
                optimizer.step()
        return scorer.eval()

    def predict(scorer, rows):
        scores = {}
        for row in rows:
            scores[row['unit_id']] = None
            if 'features' in row:
                with torch.inference_mode():
                    logits = scorer(row['features'].cuda()).squeeze(-1)
                    scores[row['unit_id']] = float(logits.softmax(-1)[option_names.index('review')])
        return scores

    oof, folds = {}, []
    development = datasets['development']
    for pr in sorted({r['pr'] for r in development}):
        train = [r for r in development if r['pr'] != pr]
        test = [r for r in development if r['pr'] == pr]
        # Exact rendered-state duplicates across PRs cannot leak into training.
        hashes = {r['state_sha256'] for r in test}
        train = [r for r in train if r['state_sha256'] not in hashes]
        scorer = fit(train)
        oof.update(predict(scorer, test))
        folds.append({'held_out_pr': pr, 'training_units': [r['unit_id'] for r in train], 'test_units': [r['unit_id'] for r in test]})
        del scorer
    labels = {r['unit_id']: r['label'] for r in development}
    selection = select_threshold(oof, labels, config['target_recall'])
    # Persist selection before scoring the reserved cohort. A subsequent label file
    # cannot change this threshold or the predetermined training recipe.
    write_new(args.output / 'selection.json', {'config_sha256': digest(config), 'selection': selection})
    scorer = fit(development)
    save_file({k: v.detach().cpu().contiguous() for k, v in scorer.state_dict().items()}, str(args.output / 'scorer.safetensors'))
    result = {'config_sha256': digest(config), 'feature_sha256': digest((args.output / 'option-features.safetensors').read_bytes()),
              'scorer_sha256': digest((args.output / 'scorer.safetensors').read_bytes()), 'selection': selection,
              'oof_scores': oof, 'folds': folds, 'exploratory_scores': predict(scorer, datasets['exploratory']),
              'optimization_enabled': False, 'action': 'full_review', 'calibrated': False,
              'records': {split: [{k: v for k, v in r.items() if k != 'features'} for r in rows] for split, rows in datasets.items()}}
    result['id'] = digest(result)
    write_new(args.output / 'result.json', result)
    print(json.dumps({k: v for k, v in selection.items() if not k.endswith('ids')}), flush=True)


if __name__ == '__main__':
    main()
