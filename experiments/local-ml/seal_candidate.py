#!/usr/bin/env python3
# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
"""Seal the candidate, evaluation, and artifact records for a completed
distillation run, exactly as the Go host (internal/distillation) seals them.

The run leaves a bundle and a receipt. The host needs three sealed records
before the bundle can be a candidate: the candidate (what was trained, on
which export, under which manifest), the evaluation (how the student did on
the export's reserved split under a policy declared before the outputs were
looked at), and the artifact (the bundle's files bound to both). This script
builds them from the run's outputs and, optionally, publishes the bundle on
a serving root. It decides nothing: the evaluation's disposition is
shadow_candidate or hold, never an authorization.

Usage:
  seal_candidate.py --manifest M --manifest-pin PIN --receipt R --predictions P \
    --bundle-dir student/bundle --export-manifest export-manifest.json --dataset dataset.json \
    [--export-manifest E2 --dataset D2 ...] [--evaluation-cohort COHORT] \
    --policy evaluation-policy.json --output-dir sealed [--serving-root DIR]

The policy is the sealed evaluation_policy record the host declared for the
job before its teacher predictions and receipt (GET /api/v1/learning/jobs/{pin} returns it): {"name",
"split", "positive_label", "minimum_recall", "minimum_specificity",
"minimum_positives", "minimum_families", "schema", "kind", "id"}. It is
addressed by its gates alone; the host records that it was declared for the
job, and accepts the sealed candidate only under that policy. Every export
the run trained on is given, paired with its dataset in order; the evaluation
cohort may be omitted when there is one.
"""
import argparse
import hashlib
import json
import math
import shutil
import sys
import tempfile
from pathlib import Path

import weights_bundle
from distill import canonical, Refused, _finite_number

STUDENT_KIND = 'linear-student'
MAX_STUDENT_FEATURES = 65536
PERMITTED_ACTION = 'full_review'
EVALUATION_LIMITATION = ('Adjudicated labels on a reserved split are not defect truth. No quality-parity or exclusion '
                         'authorization; promotion grants shadow-candidate status only.')


def sha256(data):
    return hashlib.sha256(data).hexdigest()


def seal(kind, fields):
    record = dict(fields)
    for reserved in ('id', 'kind', 'schema'):
        if reserved in record:
            raise Refused('reserved field %s' % reserved)
    record['schema'] = 1
    record['kind'] = kind
    record['id'] = sha256(canonical(record).encode())
    return record


def load_json(path, duplicates_refused=True):
    raw = Path(path).read_bytes()

    def pairs(items):
        seen = set()
        for key, _ in items:
            if key in seen:
                raise Refused('duplicate key %r in %s' % (key, path))
            seen.add(key)
        return dict(items)
    return json.loads(raw, object_pairs_hook=pairs if duplicates_refused else None), raw


def _loads(raw, what):
    try:
        return json.loads(raw)
    except ValueError as error:
        raise Refused('%s: %s' % (what, error))


def decode_student(raw):
    model = _loads(raw, 'student weights')
    if not isinstance(model, dict) or model.get('schema') != 1 or model.get('kind') != STUDENT_KIND:
        raise Refused('student weights are not a schema-1 %s' % STUDENT_KIND)
    coefficients = model.get('coefficients')
    if not isinstance(coefficients, list) or not 1 <= len(coefficients) <= MAX_STUDENT_FEATURES:
        raise Refused('student declares an unbounded coefficient width')
    for value in list(coefficients) + [model.get('intercept'), model.get('threshold')]:
        if type(value) not in (int, float) or isinstance(value, bool) or not _finite_number(value):
            raise Refused('student coefficients, intercept, and threshold must be finite')
    labels = model.get('labels')
    if not isinstance(labels, list) or len(labels) != 2 or not all(isinstance(l, str) and l for l in labels) or labels[0] == labels[1]:
        raise Refused('student must name two distinct labels')
    if not model.get('task_id') or not _is_digest(model.get('distillation_manifest_pin')):
        raise Refused('student must name its task and distillation manifest pin')
    return model


def _is_digest(value):
    return isinstance(value, str) and len(value) == 64 and all(c in '0123456789abcdef' for c in value)


def predict(model, features):
    if len(features) != len(model['coefficients']):
        raise Refused('feature width %d does not match the student\'s %d' % (len(features), len(model['coefficients'])))
    margin = 0.0
    for w, x in zip(model['coefficients'], features):
        if type(x) not in (int, float) or isinstance(x, bool) or not _finite_number(x):
            raise Refused('features must be finite')
        margin += w * x
    margin += model['intercept']
    if not math.isfinite(margin):
        raise Refused('margin overflowed')
    return model['labels'][1] if margin >= model['threshold'] else model['labels'][0]


POLICY_FIELDS = {'name', 'split', 'positive_label', 'minimum_recall', 'minimum_specificity', 'minimum_positives',
                 'minimum_families'}


def validate_policy(policy):
    if not isinstance(policy, dict) or set(policy) != POLICY_FIELDS:
        raise Refused('evaluation policy must carry exactly ' + ', '.join(sorted(POLICY_FIELDS)))
    if not isinstance(policy['name'], str) or not policy['name'].strip() or len(policy['name']) > 256:
        raise Refused('evaluation policy needs a name')
    if not isinstance(policy['split'], str) or not policy['split'].strip() or any(c in policy['split'] for c in ': \t\n'):
        raise Refused('evaluation policy must name one reserved split')
    if not isinstance(policy['positive_label'], str) or not policy['positive_label']:
        raise Refused('evaluation policy must name the positive label')
    for key in ('minimum_recall', 'minimum_specificity'):
        value = policy[key]
        if type(value) not in (int, float) or isinstance(value, bool) or not math.isfinite(value) or not 0 < value <= 1:
            raise Refused('%s must be in (0, 1]' % key)
    for key in ('minimum_positives', 'minimum_families'):
        if type(policy[key]) is not int or policy[key] < 1:
            raise Refused('minimum_positives and minimum_families must be at least 1')
    return policy


def seal_policy(policy):
    """Seal a validated policy as the host does (evaluation_policy record)."""
    return seal('evaluation_policy', validate_policy(policy))


def decode_policy_record(record):
    """Verify a sealed evaluation_policy record; return (policy fields, id)."""
    if not isinstance(record, dict) or record.get('kind') != 'evaluation_policy' or record.get('schema') != 1:
        raise Refused('invalid evaluation_policy identity')
    body = {k: v for k, v in record.items() if k != 'id'}
    if record.get('id') != sha256(canonical(body).encode()):
        raise Refused('invalid evaluation_policy identity')
    fields = {k: v for k, v in body.items() if k not in ('kind', 'schema')}
    return validate_policy(fields), record['id']


def _decode_export(export_raw, dataset_raw):
    data = _loads(dataset_raw, 'dataset')
    export = _loads(export_raw, 'export manifest')
    if data.get('schema') != 1 or export.get('schema') != 1 or not data.get('rows'):
        raise Refused('dataset and export manifest must be schema 1 with rows')
    if sha256(dataset_raw) != export.get('dataset_sha256'):
        raise Refused("dataset bytes do not hash to the export manifest's dataset_sha256")
    if data['task_id'] != export['task_id']:
        raise Refused('dataset and export tasks must agree')
    if not export.get('cohort_id'):
        raise Refused('export manifest names no cohort')
    return export, data, sha256(export_raw)


def evaluate(model, exports, evaluation_cohort, training_ids, predictions, policy):
    """Measure the student on the evaluation cohort's reserved split; mirror internal/distillation.Evaluate exactly.

    exports is a list of (export_manifest_bytes, dataset_bytes), one per cohort the run trained on."""
    validate_policy(policy)
    if not exports:
        raise Refused('no export to evaluate on')
    rows, family_of, content_of, evaluation = {}, {}, {}, None
    for export_raw, dataset_raw in exports:
        export, data, _ = _decode_export(export_raw, dataset_raw)
        if model['task_id'] != data['task_id']:
            raise Refused('student and export tasks must agree')
        if data['labels'] != model['labels']:
            raise Refused('student labels %r are not the dataset\'s %r' % (model['labels'], data['labels']))
        for row in data['rows']:
            if not row.get('id') or row['id'] in rows:
                raise Refused('dataset rows must carry unique ids')
            rows[row['id']] = row
        # Content identity is the source digest behind a row, never its feature
        # vector: a coarse feature space maps distinct content to the same point.
        for row in export['rows']:
            if not row.get('id') or row['id'] in family_of:
                raise Refused('export rows must carry unique ids')
            family_of[row['id']] = row.get('family_ids') or []
            content_of[row['id']] = [s['Digest'] for s in (row.get('sources') or []) if s.get('Digest')]
        if export['cohort_id'] == evaluation_cohort:
            if evaluation is not None:
                raise Refused('cohort %r is presented twice' % evaluation_cohort)
            evaluation = export
    if evaluation is None:
        raise Refused('no export presented is for evaluation cohort %r' % evaluation_cohort)
    if policy['positive_label'] not in model['labels']:
        raise Refused('policy positive label %r is not a dataset label' % policy['positive_label'])
    marker = 'split:' + policy['split']
    test_ids, families = [], set()
    for row in evaluation['rows']:
        if marker in (row.get('exclusion_reasons') or []):
            test_ids.append(row['id'])
            families.update(row.get('family_ids') or [])
    test_ids.sort()
    if not test_ids:
        raise Refused('the export reserved no rows for split %r' % policy['split'])
    training = set(training_ids)
    train_families = {f for i in training for f in family_of.get(i, [])}
    train_content = {d for i in training for d in content_of.get(i, [])}
    reasons = []
    leaked_ids = [i for i in test_ids if i in training]
    leaked_families = [i for i in test_ids if any(f in train_families for f in family_of.get(i, []))]
    leaked_content = [i for i in test_ids if any(d in train_content for d in content_of.get(i, []))]
    if leaked_ids:
        reasons.append('training/evaluation membership overlap: ' + ','.join(leaked_ids))
    if leaked_families:
        reasons.append('training/evaluation family overlap: ' + ','.join(leaked_families))
    if leaked_content:
        reasons.append('training/evaluation content overlap: ' + ','.join(leaked_content))
    assessor = evaluation.get('label_policy_id') or 'labels'
    positives, negatives, missed, false_positives, unknown_lowered = 0, 0, [], [], []
    for i in test_ids:
        if i not in rows:
            raise Refused('reserved row %r is not in the dataset' % i)
        row = rows[i]
        lowered = predict(model, row['features']) != policy['positive_label']
        if row.get('label_status') != 'verified' or row.get('label') is None:
            if lowered:
                unknown_lowered.append(i)
            continue
        if row['label'] == policy['positive_label']:
            positives += 1
            if lowered:
                missed.append(i)
        else:
            negatives += 1
            if not lowered:
                false_positives.append(i)
    recall = (positives - len(missed)) / positives if positives > 0 else None
    specificity = (negatives - len(false_positives)) / negatives if negatives > 0 else None
    metric = dict(assessor=assessor, positives=positives, negatives=negatives, missed_ids=missed, false_positive_ids=false_positives,
                  unknown_lowered_ids=unknown_lowered, evaluated_ids=test_ids, recall=recall, specificity=specificity)
    if positives < policy['minimum_positives']:
        reasons.append('%s: sample gate failed: %d positives, %d required' % (assessor, positives, policy['minimum_positives']))
    if recall is None or recall < policy['minimum_recall']:
        reasons.append('%s: quality gate failed: recall below %s' % (assessor, _go_float(policy['minimum_recall'])))
    if specificity is None or specificity < policy['minimum_specificity']:
        reasons.append('%s: quality gate failed: specificity below %s' % (assessor, _go_float(policy['minimum_specificity'])))
    if unknown_lowered:
        reasons.append('%s: unresolved examples lowered: %s' % (assessor, ','.join(unknown_lowered)))
    if len(families) < policy['minimum_families']:
        reasons.append('insufficient independent families: %d, %d required' % (len(families), policy['minimum_families']))
    comparisons = []
    if predictions is not None:
        examples, agreed, disagreed = 0, 0, []
        for p in predictions['predictions']:
            row = rows.get(p['example_id'])
            if row is None:
                continue
            examples += 1
            if predict(model, row['features']) == p['output'].get('label'):
                agreed += 1
            else:
                disagreed.append(p['example_id'])
        disagreed.sort()
        comparisons.append(dict(teacher=predictions['teacher'], teacher_egress=predictions['teacher_egress'], examples=examples,
                                agreement=agreed / examples if examples > 0 else None, disagreement_ids=disagreed))
    return dict(disposition='shadow_candidate' if not reasons else 'hold', reasons=reasons, metrics=[metric],
                comparisons=comparisons, families=len(families), split=policy['split'], evaluated_ids=test_ids)


def _go_float(value):
    """Render a number the way Go's %v renders a float64 in a reason string."""
    if isinstance(value, int):
        return str(value)
    text = repr(float(value))
    return text[:-2] if text.endswith('.0') else text


def build_candidate_set(manifest, pin, receipt, predictions, bundle_dir, exports, policy_record, evaluation_cohort=None):
    """Seal candidate, evaluation, and artifact. exports is [(export_manifest_bytes, dataset_bytes), ...], one per
    cohort the run trained on; policy_record is the sealed evaluation_policy record the host declared."""
    policy, policy_id = decode_policy_record(policy_record)
    if receipt.get('manifest_pin') != pin or receipt.get('status') != 'completed':
        raise Refused('the receipt is not a completed run under manifest %s' % pin)
    bundle, bundle_digest = weights_bundle.verify(bundle_dir)
    if receipt.get('bundle_digest') != bundle_digest:
        raise Refused('the receipt names bundle %s, not the bundle presented %s' % (receipt.get('bundle_digest'), bundle_digest))
    if predictions.get('manifest_pin') != pin:
        raise Refused('prediction set belongs to manifest %s, not %s' % (predictions.get('manifest_pin'), pin))
    predictions_id = sha256(canonical({k: v for k, v in predictions.items() if k != 'id'}).encode())
    if receipt.get('teacher_predictions_id') != predictions_id:
        raise Refused('the receipt names prediction set %s, not the set presented %s' % (receipt.get('teacher_predictions_id'), predictions_id))
    files = {f['role']: f for f in bundle['files']}
    if 'weights' not in files or 'runtime' not in files:
        raise Refused('bundle needs a weights file and a runtime file')
    weights_raw = (Path(bundle_dir) / files['weights']['path']).read_bytes()
    model = decode_student(weights_raw)
    if model['distillation_manifest_pin'] != pin:
        raise Refused('student was trained under manifest %s, not %s' % (model['distillation_manifest_pin'], pin))
    task_ids = sorted(t['task_id'] for t in manifest['data']['tasks'])
    if model['task_id'] not in task_ids:
        raise Refused('student task %r is outside the selection %r' % (model['task_id'], task_ids))
    # Every cohort the run trained on must be presented, as the export the
    # manifest pinned, so no training row is invisible to the leak checks.
    pinned = {e['cohort_id']: e['manifest_sha256'] for e in manifest['exports']}
    actual = receipt.get('actual_cohorts') or []
    presented = {}
    for export_raw, _ in exports:
        export = _loads(export_raw, 'export manifest')
        export_pin = sha256(export_raw)
        if pinned.get(export['cohort_id']) != export_pin:
            raise Refused('export %s for cohort %r is not the one the manifest pinned' % (export_pin, export['cohort_id']))
        if export['cohort_id'] not in actual:
            raise Refused('the run did not train on cohort %r' % export['cohort_id'])
        if export['cohort_id'] in presented:
            raise Refused('cohort %r is presented twice' % export['cohort_id'])
        presented[export['cohort_id']] = export_pin
    for cohort in actual:
        if cohort not in presented:
            raise Refused('the run trained on cohort %r but its export was not presented' % cohort)
    training_exports = [dict(cohort_id=c, manifest_sha256=presented[c]) for c in sorted(presented)]
    if not evaluation_cohort:
        if len(presented) != 1:
            raise Refused('the run trained on %d cohorts; name the one to evaluate on' % len(presented))
        evaluation_cohort = next(iter(presented))
    if evaluation_cohort not in presented:
        raise Refused('the run did not train on cohort %r' % evaluation_cohort)
    export_pin = presented[evaluation_cohort]
    training_ids = sorted(p['example_id'] for p in predictions['predictions'])
    evaluation = evaluate(model, exports, evaluation_cohort, training_ids, predictions, policy)
    candidate = seal('candidate', dict(dataset_id=evaluation_cohort, evaluation_policy_id=policy_id, training_ids=training_ids,
                                       model_digest=files['weights']['sha256'], policy_predeclared=True, task_ids=task_ids,
                                       dataset_manifest_sha256=export_pin, training_manifest_sha256=pin, training_exports=training_exports,
                                       teacher_predictions_id=predictions_id, student_kind=model['kind']))
    sealed_evaluation = seal('promotion_record', dict(
        candidate_id=candidate['id'], dataset_id=evaluation_cohort, evaluation_policy_id=policy_id, policy=dict(policy),
        dataset_manifest_sha256=export_pin, predictions_id=predictions_id, split=evaluation['split'],
        evaluated_ids=evaluation['evaluated_ids'], metrics=evaluation['metrics'], comparisons=evaluation['comparisons'],
        families=evaluation['families'], reasons=evaluation['reasons'], disposition=evaluation['disposition'],
        permitted_action=PERMITTED_ACTION, optimization_enabled=False, limitation=EVALUATION_LIMITATION))
    artifact = seal('candidate_artifact', dict(
        candidate_id=candidate['id'], evaluation_id=sealed_evaluation['id'], dataset_id=evaluation_cohort,
        model_digest=files['weights']['sha256'], runtime_digest=files['runtime']['sha256'], bundle_digest=bundle_digest,
        bundle_files=[dict(path=f['path'], role=f['role'], size=f['size'], sha256=f['sha256']) for f in bundle['files']]))
    return dict(candidate=candidate, evaluation=sealed_evaluation, artifact=artifact), bundle_digest


def publish(bundle_dir, serving_root, digest):
    """Publish the verified bundle at <serving-root>/<digest>, never replacing an occupant.

    Staging is a directory this process created and nobody else can name, so
    two publishers of one digest cannot interleave their copies; whichever
    renames first wins and the other finds the same bundle present."""
    dest = Path(serving_root) / digest
    if dest.exists():
        _, existing = weights_bundle.verify(dest)
        if existing != digest:
            raise Refused('serving root already holds a different bundle under %s' % digest)
        return 'present'
    Path(serving_root).mkdir(parents=True, exist_ok=True)
    staging = Path(tempfile.mkdtemp(dir=serving_root, prefix='.staging-'))
    try:
        for entry in sorted(Path(bundle_dir).iterdir()):
            if not entry.is_file() or entry.is_symlink():
                raise Refused('bundle entry %s is not a regular file' % entry.name)
            shutil.copyfile(entry, staging / entry.name)
        _, staged = weights_bundle.verify(staging)
        if staged != digest:
            raise Refused('staged bundle hashes to %s, not %s' % (staged, digest))
        try:
            weights_bundle.publish_exclusive(staging, dest)
        except FileExistsError:
            _, existing = weights_bundle.verify(dest)
            if existing != digest:
                raise Refused('serving root already holds a different bundle under %s' % digest)
            return 'present'
        return 'published'
    finally:
        shutil.rmtree(staging, ignore_errors=True)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--manifest', required=True)
    parser.add_argument('--manifest-pin', required=True)
    parser.add_argument('--receipt', required=True)
    parser.add_argument('--predictions', required=True)
    parser.add_argument('--bundle-dir', required=True)
    parser.add_argument('--export-manifest', required=True, action='append', help='repeat per cohort the run trained on')
    parser.add_argument('--dataset', required=True, action='append', help='one per --export-manifest, in order')
    parser.add_argument('--evaluation-cohort', help='the cohort whose reserved split is measured (required with several exports)')
    parser.add_argument('--policy', required=True, help='the sealed evaluation_policy record the host declared for the job')
    parser.add_argument('--output-dir', required=True)
    parser.add_argument('--serving-root', help='publish the bundle at <serving-root>/<digest> before sealing')
    args = parser.parse_args(argv)
    try:
        manifest, manifest_raw = load_json(args.manifest)
        if sha256(manifest_raw) != args.manifest_pin:
            raise Refused('manifest bytes do not hash to the declared pin')
        receipt, _ = load_json(args.receipt)
        predictions, _ = load_json(args.predictions)
        if len(args.export_manifest) != len(args.dataset):
            parser.error('one --dataset per --export-manifest')
        exports = [(Path(e).read_bytes(), Path(d).read_bytes()) for e, d in zip(args.export_manifest, args.dataset)]
        policy_record, _ = load_json(args.policy)
        records, digest = build_candidate_set(manifest, args.manifest_pin, receipt, predictions, args.bundle_dir, exports, policy_record,
                                              args.evaluation_cohort)
        placement = publish(args.bundle_dir, args.serving_root, digest) if args.serving_root else 'not published'
    except Refused as error:
        parser.exit(3, 'sealing refused: %s\n' % error)
    except (ValueError, OSError, KeyError, TypeError) as error:
        parser.exit(1, 'sealing failed: %s\n' % error)
    out = Path(args.output_dir)
    out.mkdir(parents=True, exist_ok=True)
    (out / 'candidate-set.json').write_text(canonical(records))
    (out / 'candidate.json').write_text(canonical(dict(candidate=records['candidate'], artifact=records['artifact'])))
    (out / 'evaluation.json').write_text(canonical(records['evaluation']))
    (out / 'promote.json').write_text(canonical(dict(candidate_id=records['candidate']['id'], evaluation_id=records['evaluation']['id'])))
    print(json.dumps(dict(bundle_digest=digest, candidate_id=records['candidate']['id'], evaluation_id=records['evaluation']['id'],
                          disposition=records['evaluation']['disposition'], reasons=records['evaluation']['reasons'],
                          bundle=placement), sort_keys=True))


if __name__ == '__main__':
    main()
