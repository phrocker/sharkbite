#!/usr/bin/env python3
# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
"""Development-only low-rank Laya encoder adaptation on changed-code evidence.

Three whole-PR folds, fixed supervised recipe, no exclusion or promotion.
All previous evaluation cohorts have been retired before this experiment.
"""
import argparse
import importlib.metadata
import json
import math
import os
from pathlib import Path
import random
import time

from candidates import checked
from ranking import select_threshold
from shadow import digest, exact_fit, write_new


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--inputs', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--cache', type=Path, required=True)
    args = parser.parse_args()
    inputs = checked(args.inputs)
    config = json.loads((Path(__file__).parent/'experiments/candidates-v2.json').read_text())
    os.environ.update(HF_HOME=str(args.cache), HF_HUB_OFFLINE='1', TRANSFORMERS_OFFLINE='1',
                      HF_HUB_DISABLE_TELEMETRY='1', TOKENIZERS_PARALLELISM='false', USE_TF='0')
    import torch
    from laya import Router
    from laya.common import collate_items
    from safetensors.torch import save_file
    if importlib.metadata.version('laya') != config['runtime'] or not torch.cuda.is_available():
        raise ValueError('pinned Laya and GPU required')
    if torch.cuda.mem_get_info()[0] < 4 * 1024**3:
        raise ValueError('adapter training requires at least 4GiB free GPU memory')
    torch.cuda.set_per_process_memory_fraction(.5)
    torch.set_num_threads(4)
    args.output.mkdir(parents=True, exist_ok=False)
    prs = sorted({r['pr'] for r in inputs['rows']})
    folds = {pr: i % 3 for i, pr in enumerate(prs)}
    recipe = {'status': 'development_only', 'inputs_id': inputs['id'], 'folds': folds,
              'revision': config['revision'], 'runtime': config['runtime'], 'representation': 'diff',
              'question': config['binary_question'], 'max_len': 2048, 'head_max_len': 192,
              'epochs': 4, 'gradient_accumulation': 8, 'micro_batch': 1, 'seed': 42,
              'encoder_lr': .0001, 'head_lr': .0001, 'weight_decay': .01,
              'encoder_adaptation': 'rank8 alpha16 zero-initialized residual on every encoder Linear; frozen base weights',
              'rank': 8, 'alpha': 16, 'gpu_allocator_fraction_limit': .5,
              'objective': 'supervised cross entropy, not RLCD', 'target_relevance_recall': .95,
              'optimization_enabled': False, 'runner_sha256': digest(Path(__file__).read_bytes())}
    write_new(args.output/'config.json', recipe)
    router = Router(device='cuda', revision=config['revision'])
    agent = router.load('english')
    model = agent.model
    question = recipe['question']
    options = list(question['criteria'])
    internal = {'boundary': agent._to_internal(question)}
    rows = []
    for original in inputs['rows']:
        row = dict(original)
        state = inputs['states'][row['unit_id']][recipe['representation']]
        row['state_sha256'] = digest(state)
        row['preflight'] = exact_fit(agent, state, question, recipe['max_len'], recipe['head_max_len'])
        if row['preflight']['fits']:
            row['items'] = agent._encode_state(state, ['boundary'], internal, recipe['max_len'], recipe['head_max_len'])
        rows.append(row)
    write_new(args.output/'eligibility.json', {'rows': [{k: v for k, v in r.items() if k != 'items'} for r in rows]})
    example = next(r for r in rows if 'items' in r)
    parity_batch = collate_items([example['items']], agent.tok.pad_token_id)
    with torch.no_grad():
        base_logits = agent._infer(parity_batch)[0].detach().clone()

    class LowRankLinear(torch.nn.Module):
        def __init__(self, base):
            super().__init__()
            self.base = base
            self.lora_a = torch.nn.Linear(base.in_features, recipe['rank'], bias=False, device=base.weight.device, dtype=base.weight.dtype)
            self.lora_b = torch.nn.Linear(recipe['rank'], base.out_features, bias=False, device=base.weight.device, dtype=base.weight.dtype)
            torch.nn.init.zeros_(self.lora_b.weight)

        def forward(self, value):
            return self.base(value) + self.lora_b(self.lora_a(value)) * (recipe['alpha']/recipe['rank'])

    torch.manual_seed(recipe['seed'])
    replacements = [(name, module) for name, module in model.encoder.named_modules() if isinstance(module, torch.nn.Linear)]
    for name, module in replacements:
        parent_name, _, leaf = name.rpartition('.')
        parent = model.encoder.get_submodule(parent_name) if parent_name else model.encoder
        setattr(parent, leaf, LowRankLinear(module))
    with torch.no_grad():
        adapted_logits = agent._infer(parity_batch)[0]
    if not torch.equal(base_logits, adapted_logits):
        raise ValueError('zero-initialized adapters changed base inference')
    model.encoder.gradient_checkpointing_enable(gradient_checkpointing_kwargs={'use_reentrant': False})
    model.head_checkpointing = True
    for name, parameter in model.named_parameters():
        parameter.requires_grad_(('.lora_a.' in name or '.lora_b.' in name) if name.startswith('encoder.') else not name.startswith('act_head.'))
    initial = {k: v.detach().cpu().clone() for k, v in model.state_dict().items()}
    write_new(args.output/'adapter-layout.json', {'linear_modules': [n for n, _ in replacements],
                                                'trainable_parameters': sum(p.numel() for p in model.parameters() if p.requires_grad),
                                                'base_logits_exactly_preserved_at_initialization': True})

    def forward(row):
        b = collate_items([row['items']], agent.tok.pad_token_id)
        with torch.autocast('cuda', dtype=torch.float16):
            logits, _ = model(b['input_ids'].cuda(), b['attention_mask'].cuda(), b['marker_pos'].cuda(),
                              b['marker_mask'].cuda(), b['qtype'].cuda())
        return logits

    def train(training, tag):
        model.load_state_dict(initial, strict=True)
        torch.manual_seed(recipe['seed'])
        torch.cuda.manual_seed_all(recipe['seed'])
        model.train()
        eligible = [r for r in training if 'items' in r and r['label'] in options]
        if len({r['label'] for r in eligible}) != 2:
            raise ValueError('training needs both classes')
        encoder = [p for n, p in model.named_parameters() if n.startswith('encoder.') and p.requires_grad]
        head = [p for n, p in model.named_parameters() if not n.startswith('encoder.') and p.requires_grad]
        optimizer = torch.optim.AdamW([{'params': encoder, 'lr': recipe['encoder_lr']},
                                      {'params': head, 'lr': recipe['head_lr']}], weight_decay=recipe['weight_decay'])
        scheduler = torch.optim.lr_scheduler.CosineAnnealingLR(optimizer, T_max=math.ceil(len(eligible)/8)*recipe['epochs'], eta_min=.000001)
        scaler = torch.amp.GradScaler('cuda')
        for epoch in range(recipe['epochs']):
            random.Random(recipe['seed']+epoch).shuffle(eligible)
            total_loss = 0.
            start = time.perf_counter()
            for offset in range(0, len(eligible), recipe['gradient_accumulation']):
                batch = eligible[offset:offset+recipe['gradient_accumulation']]
                optimizer.zero_grad(set_to_none=True)
                for row in batch:
                    logits = forward(row)
                    target = torch.tensor([options.index(row['label'])], device='cuda')
                    loss = torch.nn.functional.cross_entropy(logits, target)
                    if not torch.isfinite(loss):
                        raise ValueError('nonfinite training loss')
                    total_loss += float(loss.detach())
                    scaler.scale(loss/len(batch)).backward()
                scaler.unscale_(optimizer)
                torch.nn.utils.clip_grad_norm_(model.parameters(), 1.)
                scaler.step(optimizer); scaler.update(); scheduler.step()
            print(json.dumps({'fit': tag, 'epoch': epoch+1, 'loss': total_loss/len(eligible),
                              'seconds': time.perf_counter()-start, 'training_units': len(eligible)}), flush=True)
        optimizer.zero_grad(set_to_none=True)
        del optimizer, scheduler, scaler
        model.eval()
        torch.cuda.empty_cache()

    def predict(test):
        scores = {}
        model.eval()
        for row in test:
            score = None
            if 'items' in row:
                with torch.no_grad():
                    score = float(forward(row).softmax(-1)[0, options.index('review')])
            scores[row['unit_id']] = score
        return scores

    oof = {}
    for fold in range(3):
        test = [r for r in rows if folds[r['pr']] == fold]
        hashes = {r['state_sha256'] for r in test}
        training = [r for r in rows if folds[r['pr']] != fold and r['state_sha256'] not in hashes]
        train(training, f'fold-{fold}')
        scores = predict(test)
        oof.update(scores)
        write_new(args.output/f'fold-{fold}.json', {'scores': scores, 'training_ids': [r['unit_id'] for r in training], 'held_out_prs': [pr for pr, f in folds.items() if f == fold]})
    selection = select_threshold(oof, {r['unit_id']: r['label'] for r in rows}, .95)
    selected = set(selection['dropped_ids'])
    volume = {'all_diff_bytes': sum(r['diff_bytes'] for r in rows),
              'proposed_lower_priority_diff_bytes': sum(r['diff_bytes'] for r in rows if r['unit_id'] in selected)}
    report = {'inputs_id': inputs['id'], 'config_sha256': digest(recipe), 'selection': selection, 'scores': oof,
              'volume': volume, 'optimization_enabled': False, 'status': 'threshold_selected_development_oof'}
    report['id'] = digest(report)
    write_new(args.output/'report.json', report)
    print(json.dumps({'oof_diff_byte_reduction': volume['proposed_lower_priority_diff_bytes']/volume['all_diff_bytes'],
                      'oof_recall': selection['relevance_recall']}), flush=True)
    train(rows, 'final-development-fit')
    save_file({k: v.detach().cpu().contiguous() for k, v in model.named_parameters() if v.requires_grad}, str(args.output/'adapter.safetensors'))
    write_new(args.output/'model-identity.json', {'weights_sha256': digest((args.output/'adapter.safetensors').read_bytes()),
                                               'base_revision': recipe['revision'], 'selection_report_id': report['id']})


if __name__ == '__main__':
    main()
