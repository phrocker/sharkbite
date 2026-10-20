#!/usr/bin/env python3
# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
"""Selective distillation trainer: an external worker honoring the job contract.

The worker consumes a content-addressed distillation manifest and the
authorized cohort exports it names, refuses anything the manifest does not
admit, records the chosen teacher's outputs as predictions with provenance,
fits a candidate student, publishes it as a weights bundle, and emits a
receipt naming requested versus actual parameters, runtime identities, and
the executor. It never evaluates, never promotes, and never reports
completion as improvement.

Two students ship. `averaged-perceptron` fits a deterministic averaged
perceptron over the export's numeric features from zero weights, trained to
reproduce the teacher's predictions. `checkpoint-fine-tune` starts from the
chosen base's weights bundle instead: the bundle on disk must verify and its
digest must equal the `bundle_digest` the manifest pinned for the chosen
base, so the base is a recorded, verified input and the student records
which bundle it was fine-tuned from. Both run on CPU with no model hub.
"""
import argparse
import hashlib
import json
import math
import os
import platform
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

import authorized_manifest
import dataset
import weights_bundle

KIND = 'distillation-manifest'
RECEIPT_KIND = 'distillation-receipt'
PREDICTIONS_KIND = 'teacher-predictions'
MAX_MANIFEST_BYTES = 1 << 20
MAX_EPOCHS = 10000
MANIFEST_KEYS = {'schema', 'kind', 'training_manifest_sha256', 'teacher', 'base', 'teacher_egress_at_generation', 'data', 'exports'}
# One runtime id per student, so a receipt names which student ran.
RUNTIME_IDS = {'averaged-perceptron': 'distill:averaged-perceptron:v1', 'checkpoint-fine-tune': 'distill:checkpoint-fine-tune:v1'}
# Effective defaults, shared by every student and recorded in model.json.
DEFAULT_PARAMETERS = {'epochs': 10, 'learning_rate': 1.0}


class Refused(ValueError):
    """The manifest, an export, or a teacher output is outside the recorded selection."""


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(',', ':'), allow_nan=False)


def digest(value):
    return hashlib.sha256(canonical(value).encode()).hexdigest()


def _expect(condition, message):
    if not condition:
        raise Refused(message)


def _strings(values):
    return isinstance(values, list) and all(isinstance(v, str) and v for v in values)


def _number(value):
    return type(value) in (int, float) and not isinstance(value, bool)


def _hex64(value):
    return isinstance(value, str) and len(value) == 64 and not set(value) - set('0123456789abcdef')


def _shape(manifest):
    """Refuse a manifest whose shape is not the contract's before any field is trusted.

    Go's Decode is the authority on validity; this mirrors it closely enough
    that a malformed manifest is a refusal here, not a traceback.
    """
    _expect(isinstance(manifest, dict), 'manifest is not an object')
    _expect(set(manifest) == MANIFEST_KEYS, 'manifest keys must be exactly the contract\'s')
    _expect(_hex64(manifest['training_manifest_sha256']), 'training manifest pin must be 64-hex')
    _expect(manifest['teacher_egress_at_generation'] in ('loopback', 'off_host'), 'teacher egress at generation shape')
    for role in ('teacher', 'base'):
        selection = manifest.get(role)
        _expect(isinstance(selection, dict) and isinstance(selection.get('eligible'), list) and selection['eligible'], role + ' selection shape')
        _expect(isinstance(selection.get('rule'), dict) and isinstance(selection.get('chosen'), str), role + ' selection shape')
        rule = selection['rule']
        _expect(isinstance(rule.get('metric'), str) and rule.get('direction') in ('maximize', 'minimize'), role + ' rule shape')
        _expect(all(rule.get(k) is None or _number(rule.get(k)) for k in ('minimum', 'maximum')), role + ' rule bounds must be numbers or null')
        _expect(isinstance(selection.get('rejected'), list) and all(isinstance(r, dict) and isinstance(r.get('alias'), str) and isinstance(r.get('reason'), str) for r in selection['rejected']), role + ' rejections shape')
        for c in selection['eligible']:
            _expect(isinstance(c, dict) and isinstance(c.get('alias'), str) and c['alias'] and isinstance(c.get('identity'), dict) and isinstance(c.get('measurements'), list), role + ' candidate shape')
            identity = c['identity']
            _expect(all(isinstance(identity.get(k), str) and identity[k] for k in ('provider', 'model', 'version', 'cache_identity')), role + ' identity shape')
            _expect(identity.get('egress') in ('loopback', 'off_host'), role + ' identity egress shape')
            _expect('bundle_digest' not in identity or _hex64(identity['bundle_digest']), role + ' identity bundle digest must be 64-hex when present')
            _expect(all(isinstance(m, dict) and isinstance(m.get('metric'), str) and isinstance(m.get('split'), str) and _number(m.get('value')) and isinstance(m.get('evidence_id'), str) for m in c['measurements']), role + ' measurement shape')
    data = manifest.get('data')
    _expect(isinstance(data, dict) and isinstance(data.get('tasks'), list) and data['tasks'], 'data selection shape')
    _expect(all(isinstance(t, dict) and isinstance(t.get('task_id'), str) and t['task_id'] and _strings(t.get('cohort_ids')) and _strings(t.get('label_policy_ids')) for t in data['tasks']), 'task selection shape')
    _expect(_strings(data.get('evidence_families')) and isinstance(data.get('prompt_version'), str), 'evidence families and prompt version shape')
    _expect(isinstance(data.get('generation_parameters'), dict), 'generation parameters must be an object')
    _expect(_strings(data.get('held_out_example_ids')) or data.get('held_out_example_ids') == [], 'held-out ids must be a list of strings')
    _expect(isinstance(manifest.get('exports'), list) and all(isinstance(e, dict) and isinstance(e.get('cohort_id'), str) and _hex64(e.get('manifest_sha256')) for e in manifest['exports']), 'exports shape')
    epochs = data['generation_parameters'].get('epochs', 1)
    _expect(type(epochs) is int and 1 <= epochs <= MAX_EPOCHS, 'epochs must be a positive integer no greater than %d' % MAX_EPOCHS)


def load_manifest(path, expected_pin):
    """Read the manifest once and refuse it unless its raw bytes hash to the authorized pin."""
    raw = Path(path).read_bytes()
    if len(raw) > MAX_MANIFEST_BYTES:
        raise Refused('manifest exceeds bound')
    if hashlib.sha256(raw).hexdigest() != expected_pin:
        raise Refused('manifest pin does not match the authorized pin')
    manifest = json.loads(raw)
    if not isinstance(manifest, dict) or manifest.get('schema') != 1 or manifest.get('kind') != KIND:
        raise Refused('not a distillation manifest')
    if digest(manifest) != expected_pin:
        raise Refused('manifest is not in canonical form; its pin is not its content address')
    _shape(manifest)
    try:
        return _selections(manifest)
    except (KeyError, TypeError, IndexError) as error:
        raise Refused('manifest shape: %s' % error)


def _selections(manifest):
    for role in ('teacher', 'base'):
        selection = manifest[role]
        chosen = [c for c in selection['eligible'] if c['alias'] == selection['chosen']]
        if len(chosen) != 1:
            raise Refused(role + ' choice is not in the eligible set')
        rule = selection['rule']
        value = next((m['value'] for m in chosen[0]['measurements'] if m['metric'] == rule['metric']), None)
        if value is None or (rule.get('minimum') is not None and value < rule['minimum']) or (rule.get('maximum') is not None and value > rule['maximum']):
            raise Refused(role + ' choice does not satisfy the recorded rule')
        if any(m['split'] != 'development' for c in selection['eligible'] for m in c['measurements']):
            raise Refused(role + ' selection used non-development evidence')
    if manifest['teacher_egress_at_generation'] != chosen_identity(manifest, 'teacher')['egress']:
        raise Refused('teacher egress at generation is not the resolved posture')
    return manifest


def chosen_identity(manifest, role):
    selection = manifest[role]
    return next(c for c in selection['eligible'] if c['alias'] == selection['chosen'])['identity']


def task_selection(manifest, task_id):
    return next((t for t in manifest['data']['tasks'] if t['task_id'] == task_id), None)


def authorize_export(manifest, cohort_id, dataset_path, manifest_path):
    """Admit one export only if the manifest pinned it and every row is inside the selection."""
    pin = next((e for e in manifest['exports'] if e['cohort_id'] == cohort_id), None)
    if pin is None:
        raise Refused('cohort %r is not admitted by the manifest' % cohort_id)
    # One verified read: every identity below comes from the pinned bytes
    # authorized_manifest.load hashed, never from a second unverified read.
    data, authorization = authorized_manifest.load(dataset_path, manifest_path, pin['manifest_sha256'])
    if authorization['cohort_id'] != cohort_id:
        raise Refused('export names cohort %r, not %r' % (authorization['cohort_id'], cohort_id))
    task = task_selection(manifest, authorization['task_id'])
    if task is None:
        raise Refused('task %r is outside the selection' % authorization['task_id'])
    if cohort_id not in task['cohort_ids']:
        raise Refused('cohort %r is not admitted for task %r' % (cohort_id, task['task_id']))
    if authorization['label_policy_id'] not in task['label_policy_ids']:
        raise Refused('label policy %r is not admitted for task %r' % (authorization['label_policy_id'], task['task_id']))
    held = set(manifest['data']['held_out_example_ids'])
    leaked = sorted(r['id'] for r in data['rows'] if r['id'] in held)
    if leaked:
        raise Refused('export for cohort %r contains held-out example %r' % (cohort_id, leaked[0]))
    return data, authorization


def teacher_sign(rows, manifest):
    """Deterministic reference teacher: the sign of the first feature chooses the label."""
    labels = manifest['_labels']
    return [dict(example_id=r['id'], kind='prediction', teacher=manifest['teacher']['chosen'],
                 output=dict(label=labels[1] if r['features'][0] >= 0 else labels[0], margin=float(r['features'][0])))
            for r in rows]


def teacher_command(command):
    """Run an external teacher: rows as JSON on stdin, predictions as JSON on stdout."""
    def run(rows, manifest):
        payload = canonical(dict(manifest_pin=manifest['_pin'], prompt_version=manifest['data']['prompt_version'],
                                 generation_parameters=manifest['data']['generation_parameters'],
                                 rows=[dict(id=r['id'], features=r['features']) for r in rows]))
        result = subprocess.run(command, input=payload.encode(), capture_output=True, check=True)
        predictions = json.loads(result.stdout)
        return [dict(example_id=p['example_id'], kind='prediction', teacher=manifest['teacher']['chosen'], output=p['output']) for p in predictions]
    return run


def record_predictions(manifest, predictions):
    """Teacher outputs are predictions with provenance; a held-out example or a non-prediction kind is refused."""
    held = set(manifest['data']['held_out_example_ids'])
    seen = set()
    for p in predictions:
        if p['kind'] != 'prediction':
            raise Refused('teacher output for %r recorded as %r; teacher outputs are predictions' % (p['example_id'], p['kind']))
        if p['teacher'] != manifest['teacher']['chosen']:
            raise Refused('output from %r, not the chosen teacher' % p['teacher'])
        if p['example_id'] in held:
            raise Refused('held-out example %r was shown to the teacher' % p['example_id'])
        if p['example_id'] in seen or not isinstance(p['output'], dict):
            raise Refused('duplicate or malformed prediction for %r' % p['example_id'])
        if p['output'].get('label') not in manifest['_labels']:
            raise Refused('teacher output for %r carries label %r outside the task labels' % (p['example_id'], p['output'].get('label')))
        seen.add(p['example_id'])
    return dict(schema=1, kind=PREDICTIONS_KIND, manifest_pin=manifest['_pin'], teacher=manifest['teacher']['chosen'],
                teacher_egress=chosen_identity(manifest, 'teacher')['egress'],
                prompt_version=manifest['data']['prompt_version'], predictions=predictions)


def student_perceptron(rows, predictions, manifest, parameters):
    """Averaged perceptron on teacher labels; integer-exact arithmetic on scaled features keeps it deterministic."""
    labels = manifest['_labels']
    by_id = {p['example_id']: p['output']['label'] for p in predictions}
    width = len(rows[0]['features'])
    if width == 0 or any(len(r['features']) != width for r in rows):
        raise Refused('rows disagree on feature width or have none')
    epochs = _epochs(parameters)
    weights, bias = [0.0] * width, 0.0
    total_w, total_b = [0.0] * width, 0.0
    steps = 0
    for _ in range(epochs):
        for r in sorted(rows, key=lambda r: r['id']):
            y = 1.0 if by_id[r['id']] == labels[1] else -1.0
            score = sum(w * x for w, x in zip(weights, r['features'])) + bias
            if y * score <= 0:
                weights = [w + y * x for w, x in zip(weights, r['features'])]
                bias += y
            total_w = [t + w for t, w in zip(total_w, weights)]
            total_b += bias
            steps += 1
    return dict(schema=1, kind='linear-student', task_id=manifest['_task_id'], feature_schema_id=manifest['_feature_schema_id'],
                labels=labels, coefficients=[t / steps for t in total_w], intercept=total_b / steps, threshold=0.0,
                distillation_manifest_pin=manifest['_pin'], teacher=manifest['teacher']['chosen'],
                base=manifest['base']['chosen'], epochs=epochs)


def _finite_number(value):
    return type(value) in (int, float) and abs(value) < 1e300 and math.isfinite(value)


def _epochs(parameters):
    epochs = parameters.get('epochs', DEFAULT_PARAMETERS['epochs'])
    if type(epochs) is not int or not (1 <= epochs <= MAX_EPOCHS):
        raise Refused('epochs must be a positive integer no greater than %d' % MAX_EPOCHS)
    return epochs


def load_base_checkpoint(manifest, base_bundle_dir, labels=None, width=None):
    """Verify the base bundle on disk against the manifest's pinned base digest and load its weights.

    Everything about the base that can be checked before any export is read
    is checked here; labels and width are checked too once they are known.
    """
    base = chosen_identity(manifest, 'base')
    pinned = base.get('bundle_digest')
    if not pinned:
        raise Refused('the chosen base %r pins no bundle digest; a checkpoint fine-tune needs a local base bundle' % manifest['base']['chosen'])
    if base_bundle_dir is None:
        raise Refused('the chosen base is a local bundle %s but no --base-bundle directory was supplied' % pinned)
    try:
        bundle, digest = weights_bundle.verify(base_bundle_dir)
    except (OSError, ValueError) as error:
        raise Refused('base bundle %s does not verify: %s' % (base_bundle_dir, error))
    if digest != pinned:
        raise Refused('base bundle digest %s is not the manifest\'s pinned base %s' % (digest, pinned))
    weights = next(f for f in bundle['files'] if f['role'] == 'weights')
    model = json.loads((Path(base_bundle_dir) / weights['path']).read_bytes())
    if not isinstance(model, dict) or model.get('kind') != 'linear-student' or model.get('schema') != 1:
        raise Refused('base bundle weights are not a linear-student checkpoint')
    coefficients = model.get('coefficients')
    if (not isinstance(coefficients, list) or not coefficients or not all(_finite_number(c) for c in coefficients)
            or not _finite_number(model.get('intercept')) or not _finite_number(model.get('threshold', 0.0))):
        raise Refused('base checkpoint needs finite numeric coefficients, intercept, and threshold')
    if labels is not None and model.get('labels') not in (None, labels):
        raise Refused('base checkpoint labels %r are not the task labels %r' % (model.get('labels'), labels))
    if width is not None and len(coefficients) != width:
        raise Refused('base checkpoint width %d does not match the export feature width %d' % (len(coefficients), width))
    return model, digest


def student_checkpoint(rows, predictions, manifest, parameters):
    """Averaged perceptron fine-tuned from the chosen base's verified checkpoint.

    The base's coefficients and intercept are the starting point; `learning_rate`
    (a generation parameter, default 1.0) scales every update; the averaged
    weights are the student. The resulting model records the base bundle
    digest so the lineage names the checkpoint it came from.
    """
    labels = manifest['_labels']
    width = len(rows[0]['features'])
    if width == 0 or any(len(r['features']) != width for r in rows):
        raise Refused('rows disagree on feature width or have none')
    base, base_digest = load_base_checkpoint(manifest, manifest.get('_base_bundle_dir'), labels, width)
    epochs = _epochs(parameters)
    rate = parameters.get('learning_rate', DEFAULT_PARAMETERS['learning_rate'])
    if type(rate) not in (int, float) or not (0 < rate <= 100):
        raise Refused('learning_rate must be a number in (0, 100]')
    by_id = {p['example_id']: p['output']['label'] for p in predictions}
    weights, bias = [float(c) for c in base['coefficients']], float(base['intercept'])
    total_w, total_b = [0.0] * width, 0.0
    steps = 0
    for _ in range(epochs):
        for r in sorted(rows, key=lambda r: r['id']):
            y = 1.0 if by_id[r['id']] == labels[1] else -1.0
            score = sum(w * x for w, x in zip(weights, r['features'])) + bias
            if y * score <= 0:
                weights = [w + rate * y * x for w, x in zip(weights, r['features'])]
                bias += rate * y
            total_w = [t + w for t, w in zip(total_w, weights)]
            total_b += bias
            steps += 1
    return dict(schema=1, kind='linear-student', task_id=manifest['_task_id'], feature_schema_id=manifest['_feature_schema_id'],
                labels=labels, coefficients=[t / steps for t in total_w], intercept=total_b / steps, threshold=float(base.get('threshold', 0.0)),
                distillation_manifest_pin=manifest['_pin'], teacher=manifest['teacher']['chosen'],
                base=manifest['base']['chosen'], base_bundle_digest=base_digest, epochs=epochs, learning_rate=rate)


STUDENTS = {'averaged-perceptron': student_perceptron, 'checkpoint-fine-tune': student_checkpoint}
TEACHERS = {'sign': teacher_sign}


def runtime_identities(student):
    return {'python': platform.python_version(), 'implementation': platform.python_implementation(),
            'system': platform.system(), 'machine': platform.machine(), 'runtime_id': RUNTIME_IDS[student], 'student': student,
            'distill.py': hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
            'weights_bundle.py': hashlib.sha256(Path(weights_bundle.__file__).read_bytes()).hexdigest()}


def write_exclusive(path, data):
    with Path(path).open('xb') as stream:
        stream.write(data); stream.flush(); os.fsync(stream.fileno())


def distill(manifest_path, manifest_pin, exports, output_dir, *, executor, teacher='sign', student='averaged-perceptron',
            teacher_cmd=None, parameters=None, base_bundle_dir=None):
    """Run the whole job. exports: {cohort_id: (dataset_path, export_manifest_path)}."""
    manifest = load_manifest(manifest_path, manifest_pin)
    manifest['_pin'] = manifest_pin
    manifest['_base_bundle_dir'] = base_bundle_dir
    pinned_base = chosen_identity(manifest, 'base').get('bundle_digest')
    if student == 'checkpoint-fine-tune':
        # Refuse before any export is read: the base is an input, verified first.
        load_base_checkpoint(manifest, base_bundle_dir)
    elif pinned_base:
        # The receipt attests the chosen base, bundle digest included. A student
        # that would not open that bundle must not run under a manifest that
        # pins it, or the receipt would attest a base the run never used.
        raise Refused('the chosen base pins bundle %s, which only the checkpoint-fine-tune student consumes; the %s student would attest a base it never opened' % (pinned_base, student))
    output = Path(os.path.abspath(output_dir))
    if output.exists():
        raise FileExistsError(str(output))
    requested_cohorts = sorted(e['cohort_id'] for e in manifest['exports'])
    if sorted(exports) != requested_cohorts:
        raise Refused('admitted cohorts %r but supplied %r' % (requested_cohorts, sorted(exports)))
    rows, authorizations = [], []
    for cohort_id in requested_cohorts:
        data, authorization = authorize_export(manifest, cohort_id, *exports[cohort_id])
        authorizations.append(authorization)
        manifest.setdefault('_labels', data['labels'])
        manifest.setdefault('_feature_schema_id', data['feature_schema_id'])
        manifest.setdefault('_task_id', authorization['task_id'])
        if data['labels'] != manifest['_labels'] or data['feature_schema_id'] != manifest['_feature_schema_id'] or authorization['task_id'] != manifest['_task_id']:
            raise Refused('exports disagree on labels, feature schema, or task')
        rows.extend(r for r in data['rows'] if r['split'] == 'train' and r['training_allowed'] and r['label_status'] == 'verified')
    if not rows:
        raise Refused('no training rows inside the selection')
    if student == 'checkpoint-fine-tune':
        # Width and labels are known now; refuse before the teacher runs.
        width = len(rows[0]['features'])
        load_base_checkpoint(manifest, base_bundle_dir, manifest['_labels'], width if all(len(r['features']) == width for r in rows) else None)
    requested_parameters = manifest['data']['generation_parameters']
    unrequested = sorted(set(parameters or {}) - set(requested_parameters))
    if unrequested:
        raise Refused('parameter %r was not requested by the manifest; a receipt carries no new keys' % unrequested[0])
    actual_parameters = dict(requested_parameters, **(parameters or {}))
    _epochs(actual_parameters)
    # Deviations are the recomputed differences, in the structure the Go
    # verifier recomputes: parameter, requested, actual. Never prose.
    deviations = [dict(parameter=k, requested=requested_parameters[k], actual=actual_parameters[k])
                  for k in sorted(requested_parameters) if canonical(requested_parameters[k]) != canonical(actual_parameters[k])]
    generate = teacher_command(teacher_cmd) if teacher_cmd else TEACHERS[teacher]
    predictions = record_predictions(manifest, generate(rows, manifest))
    if {p['example_id'] for p in predictions['predictions']} != {r['id'] for r in rows}:
        raise Refused('teacher predictions do not cover exactly the training rows')
    model = STUDENTS[student](rows, predictions['predictions'], manifest, actual_parameters)
    runtime = runtime_identities(student)
    staging = Path(tempfile.mkdtemp(prefix='.distill-staging-', dir=output.parent))
    try:
        bundle_dir = staging / 'bundle'
        bundle_dir.mkdir()
        write_exclusive(bundle_dir / 'model.json', dataset.canonical_bytes(model))
        write_exclusive(bundle_dir / 'runtime.json', dataset.canonical_bytes(runtime))
        bundle = weights_bundle.manifest_for(bundle_dir, weights_bundle.SERVING_FILES)
        write_exclusive(bundle_dir / weights_bundle.MANIFEST, weights_bundle.canonical(bundle))
        bundle_digest = weights_bundle.bundle_digest(bundle)
        predictions_id = digest(predictions)
        write_exclusive(staging / 'teacher-predictions.json', canonical(predictions).encode())
        receipt = dict(schema=1, kind=RECEIPT_KIND, manifest_pin=manifest_pin,
                       requested_parameters=requested_parameters, actual_parameters=actual_parameters,
                       requested_cohorts=requested_cohorts, actual_cohorts=requested_cohorts,
                       requested_teacher=chosen_identity(manifest, 'teacher'), actual_teacher=chosen_identity(manifest, 'teacher'),
                       requested_base=chosen_identity(manifest, 'base'), actual_base=chosen_identity(manifest, 'base'),
                       runtime_identities=runtime, executor=executor, bundle_digest=bundle_digest,
                       # Held-out rows never reach the trainer: an export containing one is
                       # refused above, so nothing is excluded here and the count says so.
                       teacher_predictions_id=predictions_id, held_out_excluded_count=0,
                       trained_example_count=len(rows), status='completed', deviations=deviations,
                       candidate_only=True, improvement_claimed=False, evaluation_performed_by='',
                       authorized_exports=authorizations)
        write_exclusive(staging / 'distillation-receipt.json', canonical(receipt).encode())
        for d in (bundle_dir, staging):
            weights_bundle.fsync_directory(d)
        weights_bundle.publish_exclusive(staging, output)
        weights_bundle.fsync_directory(output.parent)
    finally:
        if staging.exists():
            shutil.rmtree(staging)
    return receipt


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--manifest', required=True)
    parser.add_argument('--manifest-pin', required=True)
    parser.add_argument('--export', action='append', default=[], metavar='COHORT=DATASET:MANIFEST')
    parser.add_argument('--output-dir', required=True)
    parser.add_argument('--executor-id', required=True)
    parser.add_argument('--executor-attestation', default='')
    parser.add_argument('--teacher', choices=sorted(TEACHERS), default='sign')
    parser.add_argument('--teacher-command', nargs='+')
    parser.add_argument('--student', choices=sorted(STUDENTS), default='averaged-perceptron')
    parser.add_argument('--parameter', action='append', default=[], metavar='KEY=JSON')
    parser.add_argument('--base-bundle', help='verified weights bundle of the chosen base, required by the checkpoint-fine-tune student')
    args = parser.parse_args(argv)
    exports = {}
    for item in args.export:
        cohort, _, paths = item.partition('=')
        data_path, _, manifest_path = paths.partition(':')
        exports[cohort] = (data_path, manifest_path)
    parameters = {k: json.loads(v) for k, _, v in (p.partition('=') for p in args.parameter)}
    try:
        receipt = distill(args.manifest, args.manifest_pin, exports, args.output_dir,
                          executor=dict(id=args.executor_id, attestation_id=args.executor_attestation, host=platform.node()),
                          teacher=args.teacher, student=args.student, teacher_cmd=args.teacher_command, parameters=parameters,
                          base_bundle_dir=args.base_bundle)
    except Refused as error:
        parser.exit(3, 'distillation refused: %s\n' % error)
    except (ValueError, OSError, RuntimeError, subprocess.CalledProcessError, KeyError, TypeError, AttributeError,
            IndexError, ArithmeticError, StopIteration) as error:
        parser.exit(1, 'distillation failed: %s\n' % error)
    print(json.dumps(dict(candidate_only=True, bundle_digest=receipt['bundle_digest'], status=receipt['status']), sort_keys=True))


if __name__ == '__main__':
    main()
