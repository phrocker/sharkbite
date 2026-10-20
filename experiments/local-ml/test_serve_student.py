# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
import json
import os
import tempfile
import threading
import unittest
import urllib.error
import urllib.request
from pathlib import Path

import serve_student
import weights_bundle


def bundle(root, intercept=0.0):
    d = root / 'bundle'
    d.mkdir()
    (d / 'model.json').write_bytes(weights_bundle.canonical(dict(schema=1, kind='linear-student', labels=['no', 'yes'],
                                                                 coefficients=[1.0, 0.0], intercept=intercept, threshold=0.0)))
    (d / 'runtime.json').write_bytes(weights_bundle.canonical(dict(max_input_tokens=256)))
    manifest = weights_bundle.manifest_for(d, weights_bundle.SERVING_FILES)
    (d / weights_bundle.MANIFEST).write_bytes(weights_bundle.canonical(manifest))
    return d, weights_bundle.bundle_digest(manifest)


class ServeStudentTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.dir, self.digest = bundle(self.root)

    def start(self, **kw):
        server = serve_student.serve(self.dir, 'release:7', self.digest, 'student-v1', **kw)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        self.addCleanup(server.shutdown)
        return 'http://127.0.0.1:%d' % server.server_address[1]

    def call(self, base, path, body=None, token=None):
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(base + path, data=data, headers={'Content-Type': 'application/json', **({'Authorization': 'Bearer ' + token} if token else {})})
        try:
            with urllib.request.urlopen(req) as resp:
                return resp.status, json.loads(resp.read())
        except urllib.error.HTTPError as error:
            return error.code, json.loads(error.read())

    def test_serves_only_the_pinned_release_on_loopback(self):
        base = self.start()
        self.assertTrue(base.startswith('http://127.0.0.1:'))
        status, health = self.call(base, '/health')
        self.assertEqual((status, health['status'], health['release_id'], health['bundle_digest'], health['device']), (200, 'ready', 'release:7', self.digest, 'cpu'))
        request = dict(model='student-v1', release_id='release:7', bundle_digest=self.digest, state=dict(features=[2.0, 1.0]),
                       questions=dict(risk=dict(type='choice', options=['no', 'yes']), sure=dict(type='proposition')))
        status, body = self.call(base, '/v1/predict', request)
        self.assertEqual(status, 200)
        self.assertEqual((body['release_id'], body['bundle_digest'], body['status']), ('release:7', self.digest, 'completed'))
        self.assertEqual(body['answers']['risk']['choice'], 'yes')
        self.assertEqual(body['answers']['sure']['p_true'], 1.0)
        self.assertEqual(self.call(base, '/v1/predict', dict(request, model='other'))[0], 404)
        self.assertEqual(self.call(base, '/v1/predict', dict(request, release_id='release:8'))[0], 409)
        self.assertEqual(self.call(base, '/v1/predict', dict(request, state=dict(features=[1.0])))[0], 400)
        status, body = self.call(base, '/v1/predict', dict(request, questions=dict(other=dict(type='score', options=['a', 'b']))))
        self.assertEqual((status, body['status']), (200, 'abstained'))
        self.assertIn('outside the student task', body['reason'])

    def test_malformed_requests_are_refused_not_crashed(self):
        base = self.start()
        good = dict(model='student-v1', release_id='release:7', bundle_digest=self.digest, state=dict(features=[2.0, 1.0]),
                    questions=dict(risk=dict(type='choice', options=['no', 'yes'])))
        for body in ([], dict(good, questions=[1]), dict(good, questions=dict(risk='x')), dict(good, questions=dict(risk=dict(type='choice', options=[[1], [2]])))):
            self.assertEqual(self.call(base, '/v1/predict', body)[0], 400, body)
        nan = b'{"model":"student-v1","release_id":"release:7","bundle_digest":"%s","state":{"features":[NaN,1.0]},"questions":{}}' % self.digest.encode()
        for raw in (b'null', nan):
            req = urllib.request.Request(base + '/v1/predict', data=raw, headers={'Content-Type': 'application/json'})
            with self.assertRaises(urllib.error.HTTPError) as caught:
                urllib.request.urlopen(req)
            self.assertEqual(caught.exception.code, 400, raw)
        for length in (b'abc', b'1e2'):
            req = urllib.request.Request(base + '/v1/predict', data=b'{}', headers={'Content-Type': 'application/json', 'Content-Length': length})
            try:
                with self.assertRaises(urllib.error.HTTPError) as caught:
                    urllib.request.urlopen(req)
                self.assertEqual(caught.exception.code, 400)
            except (ValueError, urllib.error.URLError):
                pass  # the client library itself refuses to send a non-numeric length
        self.assertEqual(self.call(base, '/health')[0], 200)
        self.assertEqual(self.call(base, '/v1/predict', good)[0], 200)
        self.assertEqual(serve_student.handler_for({}).timeout, serve_student.SOCKET_TIMEOUT_SECONDS)

    def test_bearer_token_when_configured(self):
        base = self.start(token='secret')
        self.assertEqual(self.call(base, '/health')[0], 401)
        self.assertEqual(self.call(base, '/health', token='secret')[0], 200)

    def test_altered_or_foreign_bundle_refuses_to_start(self):
        with self.assertRaisesRegex(ValueError, 'does not match release'):
            serve_student.load(self.dir, 'release:7', '0' + self.digest[1:], 'student-v1')
        (self.dir / 'model.json').write_bytes(b'{}')
        with self.assertRaisesRegex(ValueError, 'altered model.json'):
            serve_student.load(self.dir, 'release:7', self.digest, 'student-v1')


if __name__ == '__main__':
    unittest.main()
