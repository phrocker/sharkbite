# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
import hashlib
import json
import io
import stat
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest import mock
import warnings

import train
import dataset
from sklearn.exceptions import ConvergenceWarning


def fixture():
    rows = []
    for i, (split, label, x) in enumerate([
        ("train", "no", -2), ("train", "yes", 2),
        ("calibration", "no", 100), ("validation", "yes", -100),
        ("test", "no", 500), ("train", None, -1000),
        ("train", None, 1000), ("train", "yes", -2000),
    ]):
        rows.append({"id": f"row-{i}", "family": f"family-{i}",
                     "content_sha256": hashlib.sha256(str(i).encode()).hexdigest(),
                     "source_revision": "synthetic-v1", "observed_at": "2026-01-01T00:00:00Z",
                     "received_at": "2026-01-01T00:00:00Z", "label_received_at": "2026-01-01T00:00:00Z",
                     "label_status": "unknown" if i == 5 else "disputed" if i == 6 else "verified",
                     "training_allowed": i != 7, "split": split, "features": [x, 1], "label": label})
    return {"schema": 1, "kind": "numeric-training-dataset", "task_id": "synthetic-task",
            "question_id": "synthetic-question", "feature_schema_id": "numeric-v1",
            "labels": ["no", "yes"], "cutoff": "2026-01-02T00:00:00Z",
            "provenance": "synthetic", "rows": rows}


class TrainingTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.path = self.root / "dataset.json"
        self.write(fixture())

    def write(self, data):
        self.path.write_bytes(dataset.canonical_bytes(data))

    def test_repeat_separate_processes_exact_artifacts_and_receipt_binding(self):
        for name in ("first", "second"):
            subprocess.run([sys.executable, str(Path(train.__file__)), "--dataset", str(self.path),
                            "--output-dir", str(self.root / name)], check=True, capture_output=True)
        self.assertEqual({p.name for p in (self.root / "first").iterdir()}, set(train.ARTIFACT_LIMITS))
        for name in train.ARTIFACT_LIMITS:
            raw = (self.root / "first" / name).read_bytes()
            self.assertEqual(raw, (self.root / "second" / name).read_bytes())
            self.assertEqual(raw, dataset.canonical_bytes(json.loads(raw)))
        model = json.loads((self.root / "first/model.json").read_bytes())
        receipt = json.loads((self.root / "first/training-receipt.json").read_bytes())
        self.assertEqual(receipt["model_sha256"], train.sha256((self.root / "first/model.json").read_bytes()))
        for key, name in (("recipe_sha256", "recipe.json"), ("training_runtime_sha256", "runtime.json")):
            self.assertEqual(model[key], train.sha256((self.root / "first" / name).read_bytes()))
        self.assertEqual(receipt["fit_row_ids"], ["row-0", "row-1"])
        self.assertEqual(len(receipt["excluded_rows"]), 6)
        runtime = json.loads((self.root / "first/runtime.json").read_bytes())
        self.assertEqual(runtime["sources"]["train.py"], train.sha256(Path(train.__file__).read_bytes()))
        self.assertEqual(runtime["sources"]["dataset.py"], train.sha256(Path(dataset.__file__).read_bytes()))
        self.assertTrue(all(pool["num_threads"] == 1 for pool in runtime["thread_pools"]))

    def test_excluded_features_and_labels_cannot_affect_fit(self):
        train.train(self.path, self.root / "first")
        data = fixture()
        for row in data["rows"][2:]:
            row["features"] = [-row["features"][0] * 3, 22]
            if row["label"] is not None:
                row["label"] = "yes" if row["label"] == "no" else "no"
        data["rows"].reverse()
        self.write(data)
        train.train(self.path, self.root / "second")
        first = json.loads((self.root / "first/model.json").read_bytes())
        second = json.loads((self.root / "second/model.json").read_bytes())
        self.assertEqual(first["coefficients"], second["coefficients"])
        self.assertEqual(first["intercept"], second["intercept"])

    def test_missing_training_class_leaves_no_output(self):
        data = fixture()
        data["rows"][1]["training_allowed"] = False
        self.write(data)
        with self.assertRaisesRegex(ValueError, "both labels"):
            train.train(self.path, self.root / "result")
        self.assertEqual(sorted(p.name for p in self.root.iterdir()), ["dataset.json"])

    def test_convergence_warning_fails_closed(self):
        def fail_fit(*args):
            warnings.warn("not converged", ConvergenceWarning)
        with mock.patch.object(train.LinearSVC, "fit", side_effect=fail_fit):
            with self.assertRaisesRegex(ValueError, "converge"):
                train.train(self.path, self.root / "result")
        self.assertFalse((self.root / "result").exists())

    def test_iteration_limit_fails_closed_without_warning(self):
        estimator = mock.Mock()
        estimator.n_iter_ = train.RECIPE["parameters"]["max_iter"]
        with mock.patch.object(train, "LinearSVC", return_value=estimator):
            with self.assertRaisesRegex(ValueError, "iteration limit"):
                train.train(self.path, self.root / "result")
        self.assertFalse((self.root / "result").exists())

    def test_existing_empty_directory_and_race_are_not_replaced(self):
        target = self.root / "result"
        target.mkdir()
        with self.assertRaises(FileExistsError):
            train.train(self.path, target)
        target.rmdir()
        publish = train.publish_exclusive
        def raced_publish(staging, output):
            output.mkdir()
            (output / "sentinel").write_text("retain")
            publish(staging, output)
        with mock.patch.object(train, "publish_exclusive", side_effect=raced_publish):
            with self.assertRaises(FileExistsError):
                train.train(self.path, target)
        self.assertEqual((target / "sentinel").read_text(), "retain")
        self.assertFalse(list(self.root.glob(".local-ml-staging-*")))

    def test_write_failure_removes_staging_and_does_not_publish(self):
        with mock.patch.object(train.os, "fsync", side_effect=OSError("synthetic disk failure")):
            with self.assertRaises(OSError):
                train.train(self.path, self.root / "result")
        self.assertEqual(sorted(p.name for p in self.root.iterdir()), ["dataset.json"])

    def test_publication_syscall_order_syncs_files_stage_rename_parent(self):
        events = []
        real_fsync = train.os.fsync
        real_publish = train.publish_exclusive
        def traced_fsync(fd):
            info = train.os.fstat(fd)
            if stat.S_ISDIR(info.st_mode):
                events.append("parent-sync" if info.st_ino == self.root.stat().st_ino else "stage-sync")
            else:
                events.append("file-sync")
            real_fsync(fd)
        def traced_publish(staging, output):
            events.append("rename")
            real_publish(staging, output)
        with mock.patch.object(train.os, "fsync", side_effect=traced_fsync), \
                mock.patch.object(train, "publish_exclusive", side_effect=traced_publish):
            train.train(self.path, self.root / "result")
        self.assertEqual(events, ["file-sync"] * 4 + ["stage-sync", "rename", "parent-sync"])

    def test_stage_directory_sync_failure_does_not_publish(self):
        real_fsync = train.os.fsync
        def failed_stage_sync(fd):
            if stat.S_ISDIR(train.os.fstat(fd).st_mode):
                raise OSError("synthetic staging directory sync failure")
            real_fsync(fd)
        with mock.patch.object(train.os, "fsync", side_effect=failed_stage_sync), \
                mock.patch.object(train, "publish_exclusive") as publish:
            with self.assertRaisesRegex(OSError, "staging directory"):
                train.train(self.path, self.root / "result")
            publish.assert_not_called()
        self.assertEqual(sorted(p.name for p in self.root.iterdir()), ["dataset.json"])

    def test_parent_sync_failure_reports_published_indeterminate_and_retains_artifacts(self):
        real_fsync = train.os.fsync
        def failed_parent_sync(fd):
            info = train.os.fstat(fd)
            if stat.S_ISDIR(info.st_mode) and info.st_ino == self.root.stat().st_ino:
                raise OSError("synthetic parent sync failure")
            real_fsync(fd)
        target = self.root / "result"
        stderr = io.StringIO()
        with mock.patch.object(train.os, "fsync", side_effect=failed_parent_sync), \
                mock.patch.object(sys, "argv", ["train.py", "--dataset", str(self.path),
                                                "--output-dir", str(target)]), \
                mock.patch.object(sys, "stderr", stderr):
            with self.assertRaises(SystemExit) as raised:
                train.main()
        self.assertEqual(raised.exception.code, 2)
        status = json.loads(stderr.getvalue())
        self.assertEqual(status["status"], "published-durability-indeterminate")
        self.assertEqual(status["output_dir"], str(target))
        self.assertEqual(status["model_sha256"], train.sha256((target / "model.json").read_bytes()))
        self.assertEqual({p.name for p in target.iterdir()}, set(train.ARTIFACT_LIMITS))
        self.assertFalse(list(self.root.glob(".local-ml-staging-*")))
        with self.assertRaises(FileExistsError):
            train.train(self.path, target)

    def test_wrong_dependency_version_fails_closed(self):
        with mock.patch.object(train.sklearn, "__version__", "0"):
            with self.assertRaisesRegex(ValueError, "dependency versions"):
                train.train(self.path, self.root / "result")
        self.assertFalse((self.root / "result").exists())

    def test_artifact_limit_fails_without_output(self):
        with mock.patch.dict(train.ARTIFACT_LIMITS, {"model.json": 1}):
            with self.assertRaisesRegex(ValueError, "byte limit"):
                train.train(self.path, self.root / "result")
        self.assertFalse((self.root / "result").exists())


if __name__ == "__main__":
    unittest.main()
