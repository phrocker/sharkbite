# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
"""Verify an independently pinned authorized export before candidate training.

The pin must come from the trusted exporter/job authority. A file and its digest
do not establish roles, training permission, or permission after revocation.
"""
import hashlib
import json
import struct

import dataset

MAX_BYTES = 4 * 1024 * 1024
MAX_DEPTH = 16
TOP_KEYS = frozenset(('schema', 'kind', 'cohort_id', 'cohort_sha256',
    'authority_revision_id', 'inventory_id', 'task_id', 'question_id',
    'label_policy_id', 'training_purpose_id', 'feature_schema_id',
    'feature_builder_id', 'cutoff', 'created_at', 'provenance_mode',
    'split_policy_id', 'sampling_policy_id', 'checkpoint_overlap',
    'dataset_sha256', 'dataset_bytes', 'rows'))
ROW_KEYS = frozenset(('id', 'request_id', 'prediction_id', 'picture_id',
    'target_id', 'selected_receipt_id', 'selected_version', 'selected_basis_id',
    'selected_label_received_at', 'current_head_id', 'current_head_version',
    'inventory_id', 'sources', 'family_ids', 'feature_sha256', 'input_digest',
    'split', 'inclusion_probability', 'exclusion_reasons'))
SOURCE_KEYS = frozenset(('ArtifactID', 'ID', 'RevisionID', 'Digest', 'OriginID',
    'AuthorityPolicyID', 'AttestationID', 'Role', 'Control', 'ObservedAt', 'ReceivedAt'))


def _fail(message):
    raise dataset.DatasetError('authorized manifest: ' + message)


def _read(path, limit):
    with open(path, 'rb') as stream:
        raw = stream.read(limit + 1)
    if len(raw) > limit:
        _fail('encoded byte limit exceeded')
    return raw


def _json(raw):
    """Bound nesting before the JSON decoder allocates nested containers."""
    try:
        text = raw.decode('utf-8')
    except UnicodeError as exc:
        raise dataset.DatasetError('authorized manifest: invalid UTF-8') from exc
    depth, quoted, escaped = 0, False, False
    for char in text:
        if quoted:
            if escaped:
                escaped = False
            elif char == '\\':
                escaped = True
            elif char == '"':
                quoted = False
        elif char == '"':
            quoted = True
        elif char in '[{':
            depth += 1
            if depth > MAX_DEPTH:
                _fail('JSON nesting limit exceeded')
        elif char in ']}':
            depth -= 1
    try:
        value = json.loads(text, object_pairs_hook=dataset._object,
                           parse_constant=dataset._constant,
                           # Go's JSON float64 encoder emits negative zero as
                           # -0. Preserve it for the binary feature commitment.
                           parse_int=lambda token: -0.0 if token == '-0' else int(token))
        # Reject escaped surrogate code points and overflowed JSON numbers even
        # in fields that would otherwise be rejected later as unknown metadata.
        dataset.canonical_bytes(value)
    except (ValueError, TypeError, UnicodeError, RecursionError) as exc:
        raise dataset.DatasetError('authorized manifest: invalid JSON') from exc
    return value


def _digest(value, where):
    if type(value) is not str or not dataset.HEX.fullmatch(value):
        _fail(where + ': expected lowercase SHA-256')


def _integer(value, where, minimum=0):
    if type(value) is not int or not minimum <= value <= 2**63 - 1:
        _fail(where + ': expected bounded integer')


def _texts(value, where, *, nonempty=False):
    if type(value) is not list or len(value) > 4096 or (nonempty and not value):
        _fail(where + ': invalid reference list')
    for item in value:
        dataset._text(item, where)
    if value != sorted(set(value)):
        _fail(where + ': expected sorted unique references')


def feature_digest(values):
    h = hashlib.sha256(b'shoal.numeric.features.v1\x00')
    h.update(struct.pack('<I', len(values)))
    for value in values:
        h.update(struct.pack('<d', value))
    return h.hexdigest()


def _receipt(value, where):
    dataset._text(value, where)
    prefix = 'adjudication-receipt:'
    if not value.startswith(prefix):
        _fail(where + ': invalid receipt reference')
    _digest(value[len(prefix):], where)


def validate(manifest, data, raw):
    dataset._keys(manifest, TOP_KEYS, 'authorized manifest')
    if type(manifest['schema']) is not int or manifest['schema'] != 1:
        _fail('schema must be integer 1')
    if manifest['kind'] != 'authorized-numeric-training-export':
        _fail('unexpected kind')
    for key in ('cohort_id', 'authority_revision_id', 'inventory_id', 'task_id',
                'question_id', 'label_policy_id', 'training_purpose_id',
                'feature_schema_id', 'feature_builder_id', 'split_policy_id', 'sampling_policy_id'):
        dataset._text(manifest[key], key)
    for key in ('dataset_sha256', 'cohort_sha256'):
        _digest(manifest[key], key)
    _integer(manifest['dataset_bytes'], 'dataset_bytes', 1)
    if manifest['dataset_bytes'] != len(raw) or manifest['dataset_sha256'] != hashlib.sha256(raw).hexdigest():
        _fail('raw dataset byte binding mismatch')
    for key in ('task_id', 'question_id', 'feature_schema_id', 'cutoff'):
        if manifest[key] != data[key]:
            _fail(key + ': dataset binding mismatch')
    cutoff = dataset._stamp(manifest['cutoff'], 'cutoff')
    created = dataset._stamp(manifest['created_at'], 'created_at')
    if created < cutoff:
        _fail('export precedes cutoff')
    if data['provenance'] != 'trusted-export':
        _fail('dataset must be a trusted export')
    if manifest['provenance_mode'] not in ('prospective', 'reconstructed') or manifest['checkpoint_overlap'] != 'unknown':
        _fail('unsupported provenance or checkpoint overlap')
    rows = manifest['rows']
    if type(rows) is not list or not 1 <= len(rows) <= 256 or len(rows) != len(data['rows']):
        _fail('row membership mismatch')
    data_rows = {row['id']: row for row in data['rows']}
    seen, targets, families, contents = set(), set(), {}, {}
    for row in rows:
        dataset._keys(row, ROW_KEYS, 'manifest row')
        for key in ('id', 'request_id', 'prediction_id', 'picture_id', 'target_id', 'inventory_id'):
            dataset._text(row[key], 'manifest row.' + key)
        if row['id'] in seen or row['id'] not in data_rows or row['target_id'] in targets:
            _fail('duplicate or substituted row membership')
        seen.add(row['id'])
        targets.add(row['target_id'])
        original = data_rows[row['id']]
        if row['split'] != original['split']:
            _fail('row split mismatch')
        _texts(row['family_ids'], 'family_ids', nonempty=True)
        for family in row['family_ids']:
            if family in families and families[family] != row['split']:
                _fail('cross-split family leakage')
            families[family] = row['split']
        for key in ('feature_sha256', 'input_digest'):
            _digest(row[key], key)
        if row['feature_sha256'] != feature_digest(original['features']):
            _fail('feature digest mismatch')
        probability = row['inclusion_probability']
        if probability is not None and (type(probability) not in (int, float) or not 0 < probability <= 1):
            _fail('invalid inclusion probability')
        _integer(row['selected_version'], 'selected_version')
        _integer(row['current_head_version'], 'current_head_version')
        if row['selected_version'] > row['current_head_version'] or row['current_head_version'] > 128:
            _fail('invalid history versions')
        if row['current_head_version']:
            _receipt(row['current_head_id'], 'current_head_id')
        elif row['current_head_id'] != '':
            _fail('head reference without version')
        if row['selected_version']:
            _receipt(row['selected_receipt_id'], 'selected_receipt_id')
            dataset._text(row['selected_basis_id'], 'selected_basis_id')
            selected_time = dataset._stamp(row['selected_label_received_at'], 'selected_label_received_at')
            if selected_time > cutoff or row['selected_label_received_at'] != original['label_received_at']:
                _fail('selected label receipt time mismatch')
            if (row['selected_version'] == row['current_head_version']) != (row['selected_receipt_id'] == row['current_head_id']):
                _fail('selected and current history head mismatch')
        elif (row['selected_receipt_id'] != '' or row['selected_basis_id'] != '' or
              row['selected_label_received_at'] is not None or original['label_status'] != 'unknown' or
              original['label_received_at'] != manifest['cutoff']):
            _fail('missing selected label has inconsistent metadata')
        reasons = row['exclusion_reasons']
        allowed = {'unadjudicated_inventory', 'later_adjudication', 'no_label_at_cutoff',
                   'label_status:unknown', 'label_status:disputed', 'training_not_allowed',
                   'label_withdrawn', 'split:calibration', 'split:validation', 'split:test'}
        if type(reasons) is not list or any(type(v) is not str or v not in allowed for v in reasons) or len(set(reasons)) != len(reasons):
            _fail('invalid exclusion reasons')
        splits = [v for v in reasons if v.startswith('split:')]
        if splits != ([] if row['split'] == 'train' else ['split:' + row['split']]):
            _fail('exclusion split mismatch')
        non_split = [v for v in reasons if not v.startswith('split:')]
        if original['label_status'] == 'verified' and non_split:
            _fail('excluded label remains usable')
        if original['label_status'] != 'verified' and not non_split:
            _fail('unverified row missing exclusion')
        if ('training_not_allowed' in reasons) == original['training_allowed']:
            _fail('training permission/exclusion mismatch')
        if (row['selected_version'] == 0) != ('no_label_at_cutoff' in reasons):
            _fail('missing-label exclusion mismatch')
        if row['selected_version'] and row['selected_version'] < row['current_head_version'] and 'later_adjudication' not in reasons:
            _fail('superseded label missing exclusion')
        sources = row['sources']
        if type(sources) is not list or not 1 <= len(sources) <= 4096:
            _fail('invalid source inventory')
        source_ids, observed = set(), []
        for source in sources:
            dataset._keys(source, SOURCE_KEYS, 'source')
            for key in ('ArtifactID', 'ID', 'RevisionID', 'OriginID', 'AuthorityPolicyID'):
                dataset._text(source[key], key)
            if source['ID'] in source_ids:
                _fail('duplicate source')
            source_ids.add(source['ID'])
            if source['AttestationID'] != '':
                dataset._text(source['AttestationID'], 'AttestationID')
            _digest(source['Digest'], 'source digest')
            if source['Role'] not in ('normative', 'observation') or source['Control'] not in ('candidate_controlled', 'external_controlled', 'registry_controlled', 'unknown'):
                _fail('invalid feature source role/control')
            observation = dataset._stamp(source['ObservedAt'], 'source observed_at')
            receipt = dataset._stamp(source['ReceivedAt'], 'source received_at')
            if observation > receipt or receipt > dataset._stamp(original['received_at'], 'row received_at'):
                _fail('source receipt ordering mismatch')
            observed.append(observation)
            digest = source['Digest']
            if digest in contents and contents[digest] != row['split']:
                _fail('cross-split source content leakage')
            contents[digest] = row['split']
        if max(observed) != dataset._stamp(original['observed_at'], 'row observed_at'):
            _fail('source observation mismatch')


def load(dataset_path, manifest_path, expected_sha256):
    """Read once, verify the independent raw-byte pins, then admit both inputs."""
    _digest(expected_sha256, 'manifest pin')
    manifest_raw = _read(manifest_path, MAX_BYTES)
    if hashlib.sha256(manifest_raw).hexdigest() != expected_sha256:
        _fail('manifest SHA-256 mismatch')
    manifest = _json(manifest_raw)
    dataset._keys(manifest, TOP_KEYS, 'authorized manifest')
    _digest(manifest['dataset_sha256'], 'dataset_sha256')
    _integer(manifest['dataset_bytes'], 'dataset_bytes', 1)
    raw = _read(dataset_path, dataset.MAX_BYTES)
    if len(raw) != manifest['dataset_bytes'] or hashlib.sha256(raw).hexdigest() != manifest['dataset_sha256']:
        _fail('raw dataset byte binding mismatch')
    data = dataset.validate_dataset(_json(raw))
    validate(manifest, data, raw)
    return data, { 'manifest_sha256': expected_sha256,
                  **{key: manifest[key] for key in (
                      'cohort_id', 'cohort_sha256', 'authority_revision_id',
                      'inventory_id', 'training_purpose_id', 'dataset_sha256',
                      'dataset_bytes', 'provenance_mode', 'checkpoint_overlap')}}
