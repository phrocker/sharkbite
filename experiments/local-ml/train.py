#!/usr/bin/env python3
# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
"""Candidate-only deterministic numeric LinearSVC training; never promotes a model."""
import os

THREAD_ENV = {name: "1" for name in (
    "OMP_NUM_THREADS", "OPENBLAS_NUM_THREADS", "MKL_NUM_THREADS",
    "VECLIB_MAXIMUM_THREADS", "NUMEXPR_NUM_THREADS", "BLIS_NUM_THREADS",
)}
# Set before importing any numerical dependencies (including indirectly).
os.environ.update(THREAD_ENV)

import argparse
import ctypes
import hashlib
import json
from pathlib import Path
import platform
import shutil
import sys
import tempfile
import warnings

import numpy as np
import scipy
import sklearn
from sklearn.exceptions import ConvergenceWarning
from sklearn.svm import LinearSVC
import threadpoolctl
from threadpoolctl import threadpool_info, threadpool_limits

import dataset
import authorized_manifest

PINNED_VERSIONS = {"numpy": "2.4.6", "scipy": "1.17.1", "scikit-learn": "1.7.2", "threadpoolctl": "3.7.0"}
RECIPE = {
    "schema": 1, "kind": "numeric-linear-svm-training", "fit_split": "train",
    "row_order": "id-ascending", "feature_transform": "none", "threshold": 0.0,
    "parameters": {"penalty": "l2", "loss": "squared_hinge", "dual": False,
                   "tol": 1e-8, "C": 1.0, "multi_class": "ovr",
                   "fit_intercept": True, "intercept_scaling": 1.0,
                   "class_weight": None, "verbose": 0, "random_state": 1729,
                   "max_iter": 100000},
}
ARTIFACT_LIMITS = {"model.json": 4 * 1024 * 1024, "recipe.json": 65536,
                   "runtime.json": 65536, "training-receipt.json": 16 * 1024 * 1024}


class PublishedDurabilityError(RuntimeError):
    """Publication happened, but persistence across a crash is indeterminate."""

    def __init__(self, output, model_sha256):
        self.output_dir = str(output)
        self.model_sha256 = model_sha256
        super().__init__(f"candidate published at {output}; parent directory sync failed; "
                         "crash durability is indeterminate; published candidate retained")


def fsync_directory(path):
    descriptor = os.open(path, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def sha256(value):
    return hashlib.sha256(value).hexdigest()


def runtime_metadata():
    versions = {"numpy": np.__version__, "scipy": scipy.__version__,
                "scikit-learn": sklearn.__version__, "threadpoolctl": threadpoolctl.__version__}
    if versions != PINNED_VERSIONS:
        raise ValueError(f"dependency versions must equal {PINNED_VERSIONS}; got {versions}")
    pools = []
    for pool in threadpool_info():
        if pool["num_threads"] != 1:
            raise ValueError("numerical runtime must use exactly one thread")
        pools.append({k: pool.get(k) for k in
                      ("user_api", "internal_api", "prefix", "version", "num_threads",
                       "threading_layer", "architecture")})
    pools.sort(key=lambda item: dataset.canonical_bytes(item))
    return {"schema": 1, "python": sys.version, "implementation": platform.python_implementation(),
            "system": platform.system(), "machine": platform.machine(),
            "dependencies": versions, "thread_environment": THREAD_ENV,
            "thread_pools": pools,
            "sources": {"train.py": sha256(Path(__file__).read_bytes()),
                        "dataset.py": sha256(Path(dataset.__file__).read_bytes()),
                        "authorized_manifest.py": sha256(Path(authorized_manifest.__file__).read_bytes())}}


def publish_exclusive(staging, output):
    """Linux atomic no-replace directory publication; unsupported hosts fail closed."""
    libc = ctypes.CDLL(None, use_errno=True)
    rename = getattr(libc, "renameat2", None)
    if rename is None:
        raise RuntimeError("atomic exclusive publication requires Linux renameat2")
    rename.argtypes = [ctypes.c_int, ctypes.c_char_p, ctypes.c_int, ctypes.c_char_p, ctypes.c_uint]
    rename.restype = ctypes.c_int
    if rename(-100, os.fsencode(staging), -100, os.fsencode(output), 1) != 0:
        error = ctypes.get_errno()
        raise OSError(error, os.strerror(error), str(output))


def train(dataset_path, output_dir, *, authorized_manifest_path=None,
          authorized_manifest_sha256=None):
    if (authorized_manifest_path is None) != (authorized_manifest_sha256 is None):
        raise ValueError('authorized manifest path and SHA-256 pin are required together')
    output = Path(os.path.abspath(output_dir))
    if os.path.lexists(output):
        raise FileExistsError(f"output already exists: {output}")
    if not output.parent.is_dir():
        raise ValueError("output parent must already exist")
    authorization = None
    if authorized_manifest_path is not None:
        data, authorization = authorized_manifest.load(
            dataset_path, authorized_manifest_path, authorized_manifest_sha256)
    else:
        data = dataset.load_dataset(dataset_path)
    eligible, exclusions = dataset.eligible_training_rows(data)
    labels = data["labels"]
    if {row["label"] for row in eligible} != set(labels):
        raise ValueError("eligible training rows must include both labels")
    features = np.asarray([row["features"] for row in eligible], dtype=np.float64)
    targets = np.asarray([labels.index(row["label"]) for row in eligible], dtype=np.int64)
    with threadpool_limits(limits=1):
        runtime = runtime_metadata()
        estimator = LinearSVC(**RECIPE["parameters"])
        with warnings.catch_warnings(record=True) as observed:
            warnings.simplefilter("always")
            estimator.fit(features, targets)
        if any(issubclass(w.category, ConvergenceWarning) for w in observed):
            raise ValueError("LinearSVC failed to converge")
        if int(estimator.n_iter_) >= RECIPE["parameters"]["max_iter"]:
            raise ValueError("LinearSVC reached iteration limit")
    coefficients = estimator.coef_[0].tolist()
    intercept = float(estimator.intercept_[0])
    if not np.isfinite(coefficients).all() or not np.isfinite(intercept):
        raise ValueError("nonfinite fitted model")
    recipe_bytes = dataset.canonical_bytes(RECIPE)
    runtime_bytes = dataset.canonical_bytes(runtime)
    model = {"schema": 1, "kind": "linear-svm",
             **{k: data[k] for k in ("task_id", "question_id", "feature_schema_id", "labels")},
             "dataset_sha256": sha256(dataset.canonical_bytes(data)),
             "recipe_sha256": sha256(recipe_bytes), "training_runtime_sha256": sha256(runtime_bytes),
             "coefficients": coefficients, "intercept": intercept, "threshold": 0.0}
    model_bytes = dataset.canonical_bytes(model)
    receipt = {"schema": 1, "kind": "candidate-training-receipt", "candidate_only": True,
               "model_sha256": sha256(model_bytes), "dataset_sha256": model["dataset_sha256"],
               "recipe_sha256": model["recipe_sha256"],
               "training_runtime_sha256": model["training_runtime_sha256"],
               "fit_row_ids": [row["id"] for row in eligible], "excluded_rows": exclusions,
               "iterations": int(estimator.n_iter_), "converged": True,
               "warnings": [{"category": w.category.__name__, "message": str(w.message)} for w in observed]}
    if authorization is not None:
        receipt['authorized_export'] = authorization
    artifacts = {"model.json": model_bytes, "recipe.json": recipe_bytes,
                 "runtime.json": runtime_bytes, "training-receipt.json": dataset.canonical_bytes(receipt)}
    for name, content in artifacts.items():
        if len(content) > ARTIFACT_LIMITS[name]:
            raise ValueError(f"artifact exceeds byte limit: {name}")
    staging = Path(tempfile.mkdtemp(prefix=".local-ml-staging-", dir=output.parent))
    try:
        for name, content in artifacts.items():
            with (staging / name).open("xb") as stream:
                stream.write(content)
                stream.flush()
                os.fsync(stream.fileno())
        fsync_directory(staging)
        publish_exclusive(staging, output)
        try:
            fsync_directory(output.parent)
        except OSError as error:
            raise PublishedDurabilityError(output, receipt["model_sha256"]) from error
    finally:
        if staging.exists():
            shutil.rmtree(staging)
    return receipt


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--dataset", required=True)
    parser.add_argument("--output-dir", required=True)
    parser.add_argument("--authorized-manifest")
    parser.add_argument("--authorized-manifest-sha256")
    args = parser.parse_args()
    try:
        receipt = train(args.dataset, args.output_dir,
                        authorized_manifest_path=args.authorized_manifest,
                        authorized_manifest_sha256=args.authorized_manifest_sha256)
    except PublishedDurabilityError as error:
        parser.exit(2, json.dumps({"status": "published-durability-indeterminate",
                                  "candidate_only": True, "output_dir": error.output_dir,
                                  "model_sha256": error.model_sha256,
                                  "message": str(error)}, sort_keys=True) + "\n")
    except (ValueError, OSError, RuntimeError) as error:
        parser.exit(1, f"training failed: {error}\n")
    print(json.dumps({"candidate_only": True, "model_sha256": receipt["model_sha256"]}, sort_keys=True))


if __name__ == "__main__":
    main()
