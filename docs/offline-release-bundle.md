<!--

    Licensed to the Apache Software Foundation (ASF) under one
    or more contributor license agreements.  See the NOTICE file
    distributed with this work for additional information
    regarding copyright ownership.  The ASF licenses this file
    to you under the Apache License, Version 2.0 (the
    "License"); you may not use this file except in compliance
    with the License.  You may obtain a copy of the License at

      https://www.apache.org/licenses/LICENSE-2.0

    Unless required by applicable law or agreed to in writing,
    software distributed under the License is distributed on an
    "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
    KIND, either express or implied.  See the License for the
    specific language governing permissions and limitations
    under the License.

-->
# Offline release bundle

An offline release bundle moves a promoted student release to a cluster with
no model-hub access. It is plain files plus digests: container image
references by digest, the promoted weights bundle, its release record, the
dataset lineage and manifest pin, the evaluation record, and an index with a
SHA-256 for every entry. Export on a connected system; import on a
disconnected one. No object store or Kubernetes feature beyond a volume is
assumed.

```
release-bundle/
  index.json              every entry with size and sha256; images by digest
  release.json            sealed model_release (bundle digest, pins, approval)
  lineage.json            dataset manifest pin and source ids
  dataset-manifest.json   the authorized export manifest the release binds
  evaluation.json         sealed promotion_record, disposition shadow_candidate
  bundle/                 the weights bundle (files + bundle.json)
```

## Export (connected side)

```sh
shoalctl release export -registry /var/lib/shoal/learning/releases \
  -bundle-root /var/lib/shoal/releases/<bundle digest> \
  -dataset-manifest export-manifest.json -evaluation evaluation.json \
  -image student-runtime=ghcr.io/example/shoal-student-runtime@sha256:... \
  -output ./release-bundle
```

Export refuses a release that is not a complete promotion (active state,
bundle digest, artifact, evaluation, bound approval), a bundle root whose
digest is not the release's, an evaluation that is not the release's or is
not `shadow_candidate`, a dataset manifest that does not hash to the
release's pin, a release withdrawn by lineage, and an image given by tag.
The bundle is written to a staging directory and verified before it is
renamed into place.

## Verify and import (disconnected side)

```sh
shoalctl release verify -bundle ./release-bundle -index-digest <quoted digest>
shoalctl release import -bundle ./release-bundle \
  -registry /var/lib/shoal/learning/releases \
  -serving-root /var/lib/shoal/releases -importer operator@example \
  -index-digest <quoted digest>
```

Import verifies every digest before anything is written to the serving
path. One altered byte fails with the entry named, and the verification
result per entry is printed. The sealed release record binds the weights
bundle, the evaluation, and the dataset manifest (a release without a
dataset manifest pin cannot be exported); the lineage file must be exactly
what the pinned dataset manifest derives, so its source ids cannot be
edited to escape a later withdrawal. The index itself is bound by nothing
sealed, which is why its digest is quoted out of band: pass it as
`-index-digest` and a bundle whose index was rewritten (its image list,
its entries) is refused even when every entry matches the rewritten
index. A bundle whose release record is not a complete promotion is
refused. On success the weights bundle is published under
`<serving-root>/<bundle digest>` with no-replace publication; a directory
already at that path that is not exactly the verified bundle refuses the
import rather than being treated as already published. The release is
committed to the disconnected registry as the active release, and the
import is recorded as a provenance event naming the importer and the index
digest. A release whose lineage includes a source the disconnected
registry has already recorded as withdrawn is refused. Importing the
active release again is a no-op and records nothing.

## Rollback without a new import

Every imported release stays recorded and its bundle stays on the serving
path, so rollback works from what was already imported:

```sh
shoalctl release rollback -registry /var/lib/shoal/learning/releases \
  -prior <release id> -owner operator@example \
  -serving-root /var/lib/shoal/releases
```

This appends a new active release that restores the exact prior bundle
digest, bound to a rollback approval. The prior bundle must still load from
the serving root, or the rollback is refused rather than pointing the
runtime at nothing. Only a release the pointer once named is a prior
release: a record written but never pointed at cannot become active this
way. A release marked ineligible by a source withdrawal cannot be rolled
back to. A withdrawal recorded before any release used the source is kept
as its own event and refuses the first release that would use it.

The registry's pointer and event logs are append-only and gap-free. A
registry whose event log has a missing or renamed file refuses to open at
all (the explorer host then refuses to start) rather than opening with a
shorter history; restore the file from the durable store or move the
damaged registry aside before starting the host.

## Serving in the chart

`explorer.studentModel` in `deploy/helm/shoal/values.yaml` renders the
student runtime as a sidecar of the explorer pod. The image is pinned by
digest (a tag is refused), the sidecar mounts `<stateDir>/releases`
read-only and serves `releases/<bundleDigest>` for the configured
`releaseID` and `modelAlias`, and its only endpoint is loopback. The chart
refuses a non-loopback endpoint unless `allowNonLoopback: true` is given as
a boolean, refuses that acknowledgement when the endpoint is loopback, and
refuses plaintext to any off-host runtime even when acknowledged.
`deploy/helm/validate-chart.sh` covers each guard.
