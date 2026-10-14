# Authorized dataset export API

A host can mount `webapi.Handler.MountDatasetExports(provider, resolver)` alongside
its configured authenticator and request binder. `internal/decisiondatasethttp`
adapts the authorized cohort exporter. There is no default source or training
purpose authority: the host must supply one. Cohorts must already be registered.

`POST /api/v1/dataset-exports` accepts exactly
`{"cohort_id":"<canonical raw URL base64 ID>"}` with a bearer token and JSON
content type. It returns a schema-1 envelope containing the cohort ID, canonical
base64 dataset and manifest bytes, and the manifest SHA-256. The transport checks
raw byte hashes, sizes, and cohort binding. The trusted exporter checks source
access, training purpose, inventory, and current eligibility; the pinned trainer
checks the full training manifest. A digest alone is not authorization.

The Go SDK's `api.Client.ExportDataset(ctx, cohortID)` returns the original bytes
and manifest pin. Requests neither create jobs nor train or promote models.
Retries perform a new authorization check and may produce a newer snapshot.
Failures return no export and never indicate an uncertain write. The handler
checks authentication again after serialization and sets `Cache-Control: no-store`.

Bounds are 4 KiB for requests, 16 MiB for dataset bytes, 4 MiB for manifest bytes,
and 28 MiB for the response envelope. A host workspace response limit can impose
a smaller limit. The SDK does not follow redirects, send cookies, or install a
replayable request body.

## Save an export for training

Run `go run ./examples/decision-dataset-client --base-url https://HOST
--cohort-id COHORT --output-dir NEW_DIRECTORY` with `SHOAL_BEARER_TOKEN` set.
The example creates a new private directory, writes `dataset.json` and
`manifest.json`, and prints the manifest pin only after both files are synced.
An error can leave a partial directory; use a new directory on retry. Existing
files are never overwritten.

Pass the received manifest and pin to the trainer with `--authorized-manifest`
and `--authorized-manifest-sha256`, as described in
[the adjudicated training showcase](../experiments/local-ml/ADJUDICATED_TRAINING.md).
Preserve the pin through a trusted channel. A saved export does not authorize a
future training job after permission changes; obtain a fresh authorized export.

The source and review integration tests exercise the real exporter, authenticated
HTTP host, and SDK together, including source and training access revocation.
The showcase remains a synthetic pipeline proof, not a model quality benchmark.
