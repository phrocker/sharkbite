<!--
Licensed to the Apache Software Foundation (ASF) under one
or more contributor license agreements. See the NOTICE file
distributed with this work for additional information
regarding copyright ownership. The ASF licenses this file
to you under the Apache License, Version 2.0 (the
"License"); you may not use this file except in compliance
with the License. You may obtain a copy of the License at

  https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing,
software distributed under the License is distributed on an
"AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
KIND, either express or implied. See the License for the
specific language governing permissions and limitations
under the License.
-->

# Object-store integration tests

From the repository root, with Docker Compose v2, curl, and Go installed:

```sh
make test-objectstore
```

The target starts these pinned, disposable emulators (ports bind only to loopback),
waits up to 120 seconds for readiness, runs the integration tests with a five-minute
test timeout, and tears down containers and volumes. Failures print all service logs.

| Service | Image | Port |
| --- | --- | --- |
| MinIO | `ghcr.io/coollabsio/minio:RELEASE.2025-10-15T17-29-55Z` | 9000 |
| fake-gcs-server | `fsouza/fake-gcs-server:1.54.0` | 4443 (HTTP) |
| Azurite (blob only) | `mcr.microsoft.com/azure-storage/azurite:3.35.0` | 10000 |

MinIO uses the community source build because the upstream Docker Hub images
are no longer publicly available.

The target sets `SHOAL_S3_INTEGRATION=1`, `SHOAL_GCS_INTEGRATION=1`, and
`SHOAL_AZURE_INTEGRATION=1`. Each live test skips unless its flag equals `1`.
Tests create and delete unique buckets/containers; no cloud credentials are needed.

* S3: `AWS_ENDPOINT_URL=http://127.0.0.1:9000`,
  `AWS_ACCESS_KEY_ID=shoaladmin`, `AWS_SECRET_ACCESS_KEY=shoal-emulator-only`,
  `AWS_REGION=us-east-1`, and `SHOAL_S3_PATH_STYLE=1`. The live test forwards
  that opt-in flag through the backend's existing `WithClientOptions`
  (`UsePathStyle`); normal backend defaults are unchanged.
* GCS: `STORAGE_EMULATOR_HOST=http://127.0.0.1:4443`, read by the GCS SDK.
* Azure: `AZURE_STORAGE_CONNECTION_STRING` uses `DefaultEndpointsProtocol=http`,
  `AccountName=shoal`, the public test-only
  `AccountKey=c2hvYWwtZW11bGF0b3Itb25seS1ub3QtYS1zZWNyZXQ=`, and
  `BlobEndpoint=http://127.0.0.1:10000/shoal`. Compose configures this same
  local account through `AZURITE_ACCOUNTS`. API-version checks are disabled
  because the Go SDK can emit a newer API version than Azurite supports.

Ordinary `go test ./internal/storage/...` does not start or need emulators.
