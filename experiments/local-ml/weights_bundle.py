#!/usr/bin/env python3
# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
"""Stage a candidate's serving files as a weights bundle; compare two bundles.

The training job in train.py is unchanged: its output directory holds the
model, recipe, runtime, and receipt. A weights bundle is the subset a server
loads (model.json as weights, runtime.json as runtime settings) plus an
ordered manifest, bundle.json, with size and SHA-256 per file. The bundle
digest is the SHA-256 of the canonical manifest and matches
experiments/decision-learning/learning.py and internal/weightsbundle.
"""
import argparse
import ctypes
import hashlib
import json
import os
import shutil
import sys
import tempfile
from pathlib import Path

ROLES = ('weights', 'tokenizer', 'preprocessing', 'runtime', 'calibration')
MANIFEST = 'bundle.json'
SERVING_FILES = {'model.json': 'weights', 'runtime.json': 'runtime'}


def fsync_directory(path):
    fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def publish_exclusive(staging, output):
    """Linux atomic no-replace directory publication (RENAME_NOREPLACE); other hosts fail closed."""
    libc = ctypes.CDLL(None, use_errno=True)
    rename = getattr(libc, 'renameat2', None)
    if rename is None:
        raise RuntimeError('atomic exclusive publication requires Linux renameat2')
    rename.argtypes = [ctypes.c_int, ctypes.c_char_p, ctypes.c_int, ctypes.c_char_p, ctypes.c_uint]
    rename.restype = ctypes.c_int
    if rename(-100, os.fsencode(staging), -100, os.fsencode(output), 1) != 0:
        error = ctypes.get_errno()
        raise OSError(error, os.strerror(error), str(output))


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(',', ':'), allow_nan=False).encode()


def manifest_for(root, roles):
    """Build the ordered manifest for every file named in roles; refuse strays."""
    root = Path(root)
    present = {str(p.relative_to(root)) for p in root.rglob('*') if p.is_file()}
    present.discard(MANIFEST)
    if set(roles) - present:
        raise ValueError('missing ' + ','.join(sorted(set(roles) - present)))
    if present - set(roles):
        raise ValueError('extra ' + ','.join(sorted(present - set(roles))))
    files = []
    for path in sorted(roles):
        if roles[path] not in ROLES:
            raise ValueError('unknown role ' + roles[path])
        data = (root / path).read_bytes()
        files.append(dict(path=path, role=roles[path], size=len(data), sha256=hashlib.sha256(data).hexdigest()))
    if [f['role'] for f in files].count('weights') != 1 or len({f['role'] for f in files}) != len(files):
        raise ValueError('bundle needs exactly one weights file and unique roles')
    return dict(schema=1, kind='weights-bundle', files=files)


def bundle_digest(manifest):
    return hashlib.sha256(canonical(manifest)).hexdigest()


def _regular_files(root):
    """Every regular file under root; a symlink or other special entry anywhere is refused, as Go does."""
    present = set()

    def unreadable(error):
        raise ValueError('unreadable ' + str(Path(error.filename).relative_to(root)) if error.filename else 'unreadable directory')

    for dirpath, dirnames, filenames in os.walk(root, onerror=unreadable, followlinks=False):
        for name in dirnames:
            if os.path.islink(os.path.join(dirpath, name)):
                raise ValueError('symlink ' + str(Path(dirpath, name).relative_to(root)))
        for name in filenames:
            p = Path(dirpath, name)
            if p.is_symlink() or not p.is_file():
                raise ValueError('not a regular file ' + str(p.relative_to(root)))
            present.add(str(p.relative_to(root)))
    return present


def validate_manifest(manifest):
    """Refuse a manifest Go would refuse: wrong schema or kind, unknown keys, bad paths, bad roles."""
    if not isinstance(manifest, dict) or set(manifest) != {'schema', 'kind', 'files'}:
        raise ValueError('manifest keys')
    if type(manifest['schema']) is not int or manifest['schema'] != 1 or manifest['kind'] != 'weights-bundle':
        raise ValueError('not a weights bundle manifest')
    files = manifest['files']
    if not isinstance(files, list) or not files or len(files) > 256:
        raise ValueError('bundle needs 1..256 files')
    paths, roles = set(), set()
    for f in files:
        if not isinstance(f, dict) or set(f) != {'path', 'role', 'size', 'sha256'}:
            raise ValueError('bundle file needs path, role, size, sha256')
        path = f['path']
        parts = path.split('/') if isinstance(path, str) else []
        if (not parts or path == MANIFEST or path.startswith('/') or '\\' in path or os.path.normpath(path) != path
                or any(seg in ('', '.', '..') for seg in parts) or path in paths):
            raise ValueError('invalid bundle path ' + repr(path))
        if f['role'] not in ROLES or f['role'] in roles:
            raise ValueError('invalid or repeated bundle role ' + repr(f['role']))
        if type(f['size']) is not int or f['size'] < 0 or not isinstance(f['sha256'], str) or len(f['sha256']) != 64 or set(f['sha256']) - set('0123456789abcdef'):
            raise ValueError('invalid bundle file metadata for ' + repr(path))
        paths.add(path); roles.add(f['role'])
    if 'weights' not in roles:
        raise ValueError('bundle has no weights file')
    return manifest


def verify(root):
    """Verify every file against bundle.json; name the first missing, extra, altered, or non-regular file."""
    root = Path(root)
    if root.is_symlink() or not root.is_dir():
        raise ValueError('bundle root must be a directory, not a symlink')
    manifest = validate_manifest(json.loads((root / MANIFEST).read_bytes()))
    present = _regular_files(root)
    present.discard(MANIFEST)
    for f in manifest['files']:
        if f['path'] not in present:
            raise ValueError('missing ' + f['path'])
        data = (root / f['path']).read_bytes()
        if len(data) != f['size'] or hashlib.sha256(data).hexdigest() != f['sha256']:
            raise ValueError('altered ' + f['path'])
        present.discard(f['path'])
    if present:
        raise ValueError('extra ' + sorted(present)[0])
    return manifest, bundle_digest(manifest)


def stage(candidate_dir, output_dir, roles=SERVING_FILES):
    """Publish the serving subset of a candidate directory as a bundle, never replacing output."""
    candidate, output = Path(candidate_dir), Path(os.path.abspath(output_dir))
    if output.exists():
        raise FileExistsError(str(output))
    staging = Path(tempfile.mkdtemp(prefix='.weights-bundle-staging-', dir=output.parent))
    try:
        for name in roles:
            data = (candidate / name).read_bytes()
            with (staging / name).open('xb') as stream:
                stream.write(data); stream.flush(); os.fsync(stream.fileno())
        manifest = manifest_for(staging, roles)
        with (staging / MANIFEST).open('xb') as stream:
            stream.write(canonical(manifest)); stream.flush(); os.fsync(stream.fileno())
        fsync_directory(staging)
        publish_exclusive(staging, output)
        fsync_directory(output.parent)
    finally:
        if staging.exists():
            shutil.rmtree(staging)
    return manifest, bundle_digest(manifest)


def compare(first, second):
    """Name diverging files between two bundles; completion of two runs is not reproducibility."""
    a, b = bundle_digest(first), bundle_digest(second)
    if a == b:
        return dict(reproducible=True, first_digest=a, second_digest=b, diverging=[])
    fa = {f['path']: (i, f) for i, f in enumerate(first['files'])}
    fb = {f['path']: (i, f) for i, f in enumerate(second['files'])}
    return dict(reproducible=False, first_digest=a, second_digest=b,
                diverging=sorted(p for p in set(fa) | set(fb) if fa.get(p) != fb.get(p)))


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest='command', required=True)
    s = sub.add_parser('stage'); s.add_argument('--candidate-dir', required=True); s.add_argument('--output-dir', required=True)
    v = sub.add_parser('verify'); v.add_argument('--bundle-dir', required=True)
    c = sub.add_parser('compare'); c.add_argument('--first', required=True); c.add_argument('--second', required=True)
    args = parser.parse_args(argv)
    try:
        if args.command == 'stage':
            _, digest = stage(args.candidate_dir, args.output_dir)
            print(json.dumps(dict(bundle_digest=digest, candidate_only=True), sort_keys=True))
        elif args.command == 'verify':
            _, digest = verify(args.bundle_dir)
            print(json.dumps(dict(bundle_digest=digest, verified=True), sort_keys=True))
        else:
            print(json.dumps(compare(verify(args.first)[0], verify(args.second)[0]), sort_keys=True))
    except (ValueError, OSError, RuntimeError, KeyError) as error:
        parser.exit(1, f'weights bundle failed: {error}\n')


if __name__ == '__main__':
    main()
