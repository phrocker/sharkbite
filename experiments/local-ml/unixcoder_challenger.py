#!/usr/bin/env python3
"""Offline frozen UniXcoder embeddings with a deterministic linear head."""
import math

import challenger_manifest as contract


def _validate_device(device, cuda_available):
    if device not in ('cpu', 'cuda'):
        raise ValueError('unsupported device')
    if device == 'cuda' and not cuda_available:
        raise RuntimeError('requested cuda device is unavailable')


def _recipe(manifest):
    matches = [recipe for recipe in manifest['recipes'] if recipe['name'] == 'unixcoder-linear-v1']
    if len(matches) != 1:
        raise ValueError('UniXcoder recipe is not uniquely registered')
    return matches[0]


def _rows(rows, manifest, training):
    recipe = _recipe(manifest)
    field = recipe.get('input_field', 'text')
    if not isinstance(rows, list) or not rows:
        raise ValueError('rows are required')
    ids, texts, labels = [], [], []
    for row in rows:
        if not isinstance(row.get('id'), str) or not row['id'] or row['id'] in ids:
            raise ValueError('invalid or duplicate row id')
        if training and (row.get('split') != 'train' or row.get('label') not in (0, 1, False, True)):
            raise ValueError('rows must be labeled training examples')
        text = row.get(field)
        if not isinstance(text, str) or not text or len(text.encode()) > 1 << 20:
            raise ValueError('invalid or oversized encoder input')
        ids.append(row['id']); texts.append(text)
        if training:
            labels.append(int(row['label']))
    if training and len(set(labels)) != 2:
        raise ValueError('training rows need both classes')
    return ids, texts, labels


def _sigmoid(values):
    return [1.0 / (1.0 + math.exp(-max(-60.0, min(60.0, float(value))))) for value in values]


def fit_head(embeddings, labels, l2=1e-3):
    """Fit a deterministic ridge linear head; scores are not calibrated."""
    import numpy as np
    matrix = np.asarray(embeddings, dtype=float)
    target = np.asarray(labels, dtype=float)
    if matrix.ndim != 2 or matrix.shape[1] == 0 or matrix.shape[1] > 8192 or target.shape != (matrix.shape[0],) or matrix.shape[0] < 2 or len(set(target.tolist())) != 2:
        raise ValueError('invalid embedding training shape')
    if not np.isfinite(matrix).all() or not np.isfinite(target).all() or l2 <= 0:
        raise ValueError('invalid embedding values')
    augmented = np.column_stack((matrix, np.ones(matrix.shape[0])))
    regularizer = np.eye(augmented.shape[1]) * l2
    regularizer[-1, -1] = 0
    weights = np.linalg.solve(augmented.T @ augmented + regularizer, augmented.T @ target)
    if not np.isfinite(weights).all():
        raise ValueError('nonfinite linear head')
    return weights.tolist()


def score_head(embeddings, weights):
    import numpy as np
    matrix = np.asarray(embeddings, dtype=float)
    vector = np.asarray(weights, dtype=float)
    if matrix.ndim != 2 or vector.shape != (matrix.shape[1] + 1,) or not np.isfinite(matrix).all() or not np.isfinite(vector).all():
        raise ValueError('invalid linear head shape')
    return _sigmoid(np.column_stack((matrix, np.ones(matrix.shape[0]))) @ vector)


def train(rows, manifest, embedder):
    contract.validate(manifest)
    ids, texts, labels = _rows(rows, manifest, training=True)
    embeddings = embedder(texts)
    weights = fit_head(embeddings, labels)
    matrix = list(embeddings)
    if not matrix or len(matrix[0]) == 0:
        raise ValueError('empty embedding output')
    recipe = _recipe(manifest)
    return contract.seal('unixcoder_candidate_artifact', manifest_id=manifest['id'], task_id=manifest['task_id'],
                         dataset_id=manifest['dataset_id'], model_repo=recipe['model_repo'],
                         model_revision=recipe['model_revision'], tokenizer_revision=recipe['tokenizer_revision'],
                         weights_digest=recipe['weights_digest'], tokenizer_digest=recipe['tokenizer_digest'],
                         license_id=recipe['license_id'], max_context_tokens=recipe['max_context_tokens'],
                         chunking=recipe['chunking'],
                         runtime_id=recipe['runtime_id'], device=recipe.get('device', 'cpu'),
                         embedding_dimension=len(matrix[0]), head_weights=weights, training_row_ids=sorted(ids),
                         optimization_enabled=False)


def predict(rows, manifest, artifact, embedder):
    contract.validate(manifest)
    body = {key: value for key, value in artifact.items() if key != 'id'}
    if artifact.get('kind') != 'unixcoder_candidate_artifact' or artifact.get('id') != contract._digest(body) or artifact.get('manifest_id') != manifest['id']:
        raise ValueError('UniXcoder artifact/manifest mismatch')
    recipe = _recipe(manifest)
    for field in ('model_repo', 'model_revision', 'tokenizer_revision', 'weights_digest', 'tokenizer_digest', 'license_id', 'max_context_tokens', 'chunking', 'runtime_id'):
        if artifact.get(field) != recipe.get(field):
            raise ValueError('UniXcoder artifact provenance mismatch')
    ids, texts, _ = _rows(rows, manifest, training=False)
    scores = score_head(embedder(texts), artifact['head_weights'])
    if len(scores) != len(ids):
        raise ValueError('embedding score count mismatch')
    return {'artifact_id': artifact['id'], 'scores': dict(zip(ids, scores)), 'optimization_enabled': False}


class FrozenUnixCoder:
    """Load an explicitly pinned local checkpoint; network access is disabled."""

    def __init__(self, manifest, device='cpu'):
        contract.validate(manifest)
        recipe = _recipe(manifest)
        try:
            import torch
            from transformers import AutoModel, AutoTokenizer
        except ImportError as error:
            raise RuntimeError('transformers and torch are required for UniXcoder inference') from error
        _validate_device(device, torch.cuda.is_available())
        self._torch = torch
        self._device = device
        self._tokenizer = AutoTokenizer.from_pretrained(recipe['model_repo'], revision=recipe['tokenizer_revision'], local_files_only=True)
        self._model = AutoModel.from_pretrained(recipe['model_repo'], revision=recipe['model_revision'], local_files_only=True)
        self._model.to(device).eval()
        for parameter in self._model.parameters():
            parameter.requires_grad_(False)
        actual_device = str(next(self._model.parameters()).device)
        if not actual_device.startswith(device):
            raise ValueError('effective UniXcoder device differs from requested device')
        self._max_tokens = recipe['max_context_tokens']
        self._chunking = recipe['chunking']

    def __call__(self, texts):
        torch = self._torch
        encoded = self._tokenizer(list(texts), return_tensors='pt', padding=True, truncation=False)
        if encoded['input_ids'].shape[1] > self._max_tokens:
            if self._chunking != 'fixed_nonoverlap':
                raise ValueError('input exceeds pinned UniXcoder context; refusing silent truncation')
            raise ValueError('fixed_nonoverlap chunking is not implemented')
        encoded = {key: value.to(self._device) for key, value in encoded.items()}
        with torch.no_grad():
            hidden = self._model(**encoded).last_hidden_state
        mask = encoded['attention_mask'].unsqueeze(-1).to(hidden.dtype)
        pooled = (hidden * mask).sum(dim=1) / mask.sum(dim=1).clamp_min(1)
        return pooled.detach().cpu().tolist()
