# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
"""Bounded offline numeric dataset admission, not an authenticated exporter.

Unknown/disputed labels must be null. Permission and provenance are assertions
from a trusted offline producer, never evidence of production authorization.
"""
import datetime
import json
import math
import re
from decimal import Decimal

MAX_BYTES = 16 * 1024 * 1024
MAX_ROWS = 4096
MAX_FEATURES = 65536
MAX_SCALARS = 1_000_000
MAX_ABS_FEATURE = 1_000_000
MAX_TEXT_BYTES = 1024
DATA_KEYS = frozenset(('schema', 'kind', 'task_id', 'question_id',
                       'feature_schema_id', 'labels', 'cutoff', 'provenance', 'rows'))
ROW_KEYS = frozenset(('id', 'family', 'content_sha256', 'source_revision',
                      'observed_at', 'received_at', 'label_received_at',
                      'label_status', 'training_allowed', 'split', 'features', 'label'))
STAMP = re.compile(r'(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(\.\d+)?(?:Z|\+00:00)\Z', re.ASCII)
HEX = re.compile(r'[0-9a-f]{64}\Z', re.ASCII)


class DatasetError(ValueError):
    """The export does not meet the offline admission contract."""


def canonical_bytes(data):
    """Exact canonical JSON encoding used by dataset and artifact digests."""
    return json.dumps(data, sort_keys=True, separators=(',', ':'),
                      ensure_ascii=False, allow_nan=False).encode('utf-8')


def _text(value, where):
    if type(value) is not str or not value.strip():
        raise DatasetError(f'{where}: expected nonempty string')
    try:
        size = len(value.encode('utf-8'))
    except UnicodeError as exc:
        raise DatasetError(f'{where}: invalid UTF-8') from exc
    if size > MAX_TEXT_BYTES:
        raise DatasetError(f'{where}: string exceeds {MAX_TEXT_BYTES} bytes')
    return value


def _keys(value, expected, where):
    if type(value) is not dict or value.keys() != expected:
        raise DatasetError(f'{where}: expected exact keys {sorted(expected)}')


def _stamp(value, where):
    _text(value, where)
    match = STAMP.fullmatch(value)
    if not match:
        raise DatasetError(f'{where}: expected UTC RFC3339 timestamp')
    try:
        base = datetime.datetime.strptime(match[1], '%Y-%m-%dT%H:%M:%S')
    except ValueError as exc:
        raise DatasetError(f'{where}: invalid timestamp') from exc
    return base, Decimal(match[2] or '0')


def validate_dataset(data):
    """Validate all rows including quarantined rows; return the supplied dict.

    Family and content isolation applies across every split, regardless of row
    eligibility. All observations, feature receipts and label receipts must be
    available by cutoff. The verified label receipt cannot precede observation.
    """
    _keys(data, DATA_KEYS, 'dataset')
    if type(data['schema']) is not int or data['schema'] != 1:
        raise DatasetError('schema: expected integer 1')
    if data['kind'] != 'numeric-training-dataset':
        raise DatasetError('kind: expected numeric-training-dataset')
    for key in ('task_id', 'question_id', 'feature_schema_id'):
        _text(data[key], key)
    labels = data['labels']
    if type(labels) is not list or len(labels) != 2:
        raise DatasetError('labels: expected two labels')
    for label in labels:
        _text(label, 'labels')
    if labels[0] == labels[1]:
        raise DatasetError('labels: must be distinct')
    if data['provenance'] not in ('synthetic', 'trusted-export'):
        raise DatasetError('provenance: expected synthetic or trusted-export')
    cutoff = _stamp(data['cutoff'], 'cutoff')
    rows = data['rows']
    if type(rows) is not list or not 1 <= len(rows) <= MAX_ROWS:
        raise DatasetError(f'rows: expected 1..{MAX_ROWS} rows')
    ids, families, contents = set(), {}, {}
    width, scalars = None, 0
    for index, row in enumerate(rows):
        where = f'rows[{index}]'
        _keys(row, ROW_KEYS, where)
        for key in ('id', 'family', 'source_revision'):
            _text(row[key], f'{where}.{key}')
        if row['id'] in ids:
            raise DatasetError(f'{where}: duplicate row id')
        ids.add(row['id'])
        digest = row['content_sha256']
        if type(digest) is not str or not HEX.fullmatch(digest):
            raise DatasetError(f'{where}: invalid content_sha256')
        split = row['split']
        if split not in ('train', 'calibration', 'validation', 'test'):
            raise DatasetError(f'{where}: invalid split')
        for value, mapping, name in ((row['family'], families, 'family'),
                                     (digest, contents, 'content')):
            if value in mapping and mapping[value] != split:
                raise DatasetError(f'{where}: cross-split {name} leakage')
            mapping[value] = split
        observed = _stamp(row['observed_at'], f'{where}.observed_at')
        received = _stamp(row['received_at'], f'{where}.received_at')
        labeled = _stamp(row['label_received_at'], f'{where}.label_received_at')
        if observed > received or received > cutoff or labeled > cutoff:
            raise DatasetError(f'{where}: temporal leakage or invalid receipt order')
        status = row['label_status']
        if status not in ('verified', 'unknown', 'disputed'):
            raise DatasetError(f'{where}: invalid label_status')
        if type(row['training_allowed']) is not bool:
            raise DatasetError(f'{where}: training_allowed must be boolean')
        if status == 'verified':
            if type(row['label']) is not str or row['label'] not in labels:
                raise DatasetError(f'{where}: verified label must match labels')
            if labeled < observed:
                raise DatasetError(f'{where}: label receipt precedes observation')
        elif row['label'] is not None:
            raise DatasetError(f'{where}: unknown/disputed labels must be null')
        features = row['features']
        if type(features) is not list or not 1 <= len(features) <= MAX_FEATURES:
            raise DatasetError(f'{where}: invalid feature count')
        if width is None:
            width = len(features)
        if len(features) != width:
            raise DatasetError(f'{where}: feature width mismatch')
        scalars += len(features)
        if scalars > MAX_SCALARS:
            raise DatasetError('dataset: scalar limit exceeded')
        for value in features:
            if type(value) not in (int, float):
                raise DatasetError(f'{where}: feature must be numeric, not boolean')
            try:
                finite = math.isfinite(value)
            except OverflowError:
                finite = False
            if not finite:
                raise DatasetError(f'{where}: feature must be finite float64')
            if abs(value) > MAX_ABS_FEATURE:
                raise DatasetError(
                    f'{where}: feature magnitude exceeds {MAX_ABS_FEATURE}; '
                    'apply a versioned external feature transform before export')
    try:
        size = len(canonical_bytes(data))
    except (ValueError, TypeError, UnicodeError) as exc:
        raise DatasetError('dataset: invalid JSON encoding') from exc
    if size > MAX_BYTES:
        raise DatasetError('dataset: encoded byte limit exceeded')
    return data


def _object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise DatasetError(f'duplicate JSON key: {key}')
        result[key] = value
    return result


def _constant(value):
    raise DatasetError(f'nonfinite JSON number: {value}')


def load_dataset(path):
    """Read at most MAX_BYTES+1 bytes and admit a strict UTF-8 JSON export."""
    with open(path, 'rb') as stream:
        raw = stream.read(MAX_BYTES + 1)
    if len(raw) > MAX_BYTES:
        raise DatasetError('dataset: encoded byte limit exceeded')
    try:
        data = json.loads(raw.decode('utf-8'), object_pairs_hook=_object,
                          parse_constant=_constant)
    except (ValueError, UnicodeError, RecursionError) as exc:
        raise DatasetError(f'dataset: invalid JSON: {exc}') from exc
    return validate_dataset(data)


def eligible_training_rows(data):
    """Return (eligible_rows, exclusions), both sorted by id.

    Exclusions are {'id': str, 'reasons': [str, ...]}; reasons accumulate in
    split/status/permission order. Missing labels never become negative labels.
    """
    validate_dataset(data)
    eligible, exclusions = [], []
    for row in sorted(data['rows'], key=lambda item: item['id']):
        reasons = []
        if row['split'] != 'train':
            reasons.append('split:' + row['split'])
        if row['label_status'] != 'verified':
            reasons.append('label_status:' + row['label_status'])
        if not row['training_allowed']:
            reasons.append('training_not_allowed')
        if reasons:
            exclusions.append({'id': row['id'], 'reasons': reasons})
        else:
            eligible.append(row)
    return eligible, exclusions
