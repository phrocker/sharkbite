#!/usr/bin/env python3
# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
"""Loopback student runtime: serve one promoted weights bundle, nothing else.

The runtime verifies every file digest and the bundle digest against the
release it was started for before it listens, binds to loopback only, needs
no network and no model hub, refuses any other model alias or release, and
reports the exact release, bundle digest, and device in every response.
It serves the `linear-student` model that distill.py produces; a checkpoint
runtime implements the same two routes.
"""
import argparse
import json
import math
import platform
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import weights_bundle

MAX_REQUEST_BYTES = 1 << 20
MAX_STATE_FEATURES = 65536
# A client that opens a connection and never finishes its request holds a
# handler thread; the socket timeout bounds that.
SOCKET_TIMEOUT_SECONDS = 30


def _refuse_constant(name):
    raise ValueError('non-finite number %s' % name)


def parse_request(raw):
    """Strict JSON object: NaN and Infinity are refused, as is any non-object body."""
    request = json.loads(raw, parse_constant=_refuse_constant)
    if not isinstance(request, dict):
        raise ValueError('request must be a JSON object')
    return request


def load(bundle_dir, release_id, bundle_digest, model_alias, device='cpu'):
    """Verify and load the bundle; a mismatch with the release refuses to serve."""
    manifest, digest = weights_bundle.verify(bundle_dir)
    if digest != bundle_digest:
        raise ValueError('bundle digest %s does not match release %s digest %s' % (digest, release_id, bundle_digest))
    weights = next(f for f in manifest['files'] if f['role'] == 'weights')
    model = json.loads((weights_bundle.Path(bundle_dir) / weights['path']).read_bytes())
    if model.get('kind') != 'linear-student' or model.get('schema') != 1:
        raise ValueError('bundle weights are not a linear-student model')
    return dict(release_id=release_id, bundle_digest=bundle_digest, model_alias=model_alias, device=device,
                model=model, fallback_events=[], ready=True)


def predict(served, request):
    """Answer typed questions from the student's linear decision; refuse foreign identities."""
    if request.get('model') != served['model_alias']:
        return 404, dict(error='unknown model alias')
    if request.get('release_id') != served['release_id'] or request.get('bundle_digest') != served['bundle_digest']:
        return 409, dict(error='request names a different release or bundle')
    state = request.get('state')
    features = state.get('features') if isinstance(state, dict) else None
    if (not isinstance(features, list) or not features or len(features) > MAX_STATE_FEATURES
            or any(type(x) not in (int, float) or not math.isfinite(x) for x in features)):
        return 400, dict(error='state.features must be a bounded list of finite numbers')
    model = served['model']
    if len(features) != len(model['coefficients']):
        return 400, dict(error='feature width %d does not match model %d' % (len(features), len(model['coefficients'])))
    margin = sum(w * x for w, x in zip(model['coefficients'], features)) + model['intercept']
    positive = model['labels'][1] if margin >= model['threshold'] else model['labels'][0]
    answers = {}
    questions = request.get('questions') or {}
    if not isinstance(questions, dict) or not all(isinstance(q, dict) for q in questions.values()):
        return 400, dict(error='questions must be an object of question objects')
    for qid, question in questions.items():
        options = question.get('options') or []
        if not isinstance(options, list) or not all(isinstance(o, str) for o in options):
            return 400, dict(error='question %r options must be a list of strings' % qid)
        if question.get('type') == 'choice' and set(options) == set(model['labels']) and len(options) == 2:
            answers[qid] = dict(type='choice', choice=positive, confidence=1.0,
                                probabilities={label: (1.0 if label == positive else 0.0) for label in options})
        elif question.get('type') == 'proposition':
            answers[qid] = dict(type='proposition', p_true=1.0 if margin >= model['threshold'] else 0.0, confidence=1.0)
        else:
            return 200, dict(release_id=served['release_id'], bundle_digest=served['bundle_digest'], model_alias=served['model_alias'],
                             device=served['device'], status='abstained', reason='question %r is outside the student task' % qid)
    return 200, dict(release_id=served['release_id'], bundle_digest=served['bundle_digest'], model_alias=served['model_alias'],
                     device=served['device'], status='completed', answers=answers)


def handler_for(served, token=None):
    class Handler(BaseHTTPRequestHandler):
        timeout = SOCKET_TIMEOUT_SECONDS

        def log_message(self, *_):
            pass

        def _send(self, status, body):
            raw = json.dumps(body, sort_keys=True).encode()
            self.send_response(status)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

        def _authorized(self):
            return token is None or self.headers.get('Authorization') == 'Bearer ' + token

        def do_GET(self):
            if self.path != '/health':
                return self._send(404, dict(error='not found'))
            if not self._authorized():
                return self._send(401, dict(error='unauthorized'))
            self._send(200, dict(status='ready' if served['ready'] else 'loading', release_id=served['release_id'],
                                 bundle_digest=served['bundle_digest'], model_alias=served['model_alias'],
                                 device=served['device'], fallback_events=served['fallback_events']))

        def do_POST(self):
            if self.path != '/v1/predict':
                return self._send(404, dict(error='not found'))
            if not self._authorized():
                return self._send(401, dict(error='unauthorized'))
            try:
                length = int(self.headers.get('Content-Length') or 0)
            except ValueError:
                return self._send(400, dict(error='malformed Content-Length'))
            if length <= 0 or length > MAX_REQUEST_BYTES:
                return self._send(413, dict(error='request exceeds bound'))
            try:
                body = self.rfile.read(length)
            except OSError:
                return
            if len(body) != length:
                return self._send(400, dict(error='truncated request'))
            try:
                request = parse_request(body)
                status, reply = predict(served, request)
            except (ValueError, TypeError, AttributeError, KeyError):
                return self._send(400, dict(error='malformed request'))
            self._send(status, reply)

    return Handler


def serve(bundle_dir, release_id, bundle_digest, model_alias, port=0, token=None, device='cpu'):
    served = load(bundle_dir, release_id, bundle_digest, model_alias, device)
    server = ThreadingHTTPServer(('127.0.0.1', port), handler_for(served, token))
    return server


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--bundle-dir', required=True)
    parser.add_argument('--release-id', required=True)
    parser.add_argument('--bundle-digest', required=True)
    parser.add_argument('--model-alias', required=True)
    parser.add_argument('--port', type=int, default=8701)
    parser.add_argument('--token')
    args = parser.parse_args(argv)
    try:
        server = serve(args.bundle_dir, args.release_id, args.bundle_digest, args.model_alias, args.port, args.token)
    except (ValueError, OSError, KeyError, StopIteration) as error:
        parser.exit(2, 'student runtime refused to start: %s\n' % error)
    print(json.dumps(dict(listening='127.0.0.1:%d' % server.server_address[1], release_id=args.release_id,
                          bundle_digest=args.bundle_digest, device='cpu', python=platform.python_version()), sort_keys=True), flush=True)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass


if __name__ == '__main__':
    main()
