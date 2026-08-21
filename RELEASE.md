# Compatibility package release policy

## Architecture

The PyPI project `sharkbite` is a metadata-only compatibility distribution.
It must never install the `sharkbite` or `pysharkbite` packages, extension
modules, shared libraries, headers, or command-line programs. Those files are
owned solely by the `shoal-sharkbite` implementation distribution.

Sharkbite issue
[#108](https://github.com/phrocker/sharkbite/issues/108) is the authoritative
compatibility-layer migration gate. This repository provides only the
install-name and release bridge; publishing it is not evidence that the
compatibility implementation or live-cluster conformance is complete. Keep
#108 open until all four required live Accumulo rows pass.

PyPI already contains historical `sharkbite` releases through `1.2.0.3`.
The first coordinated version is therefore `1.3.0`, which is unused and valid
for both projects. Every coordinated release uses one PEP 440 version in both
projects and pins the implementation with exact equality:

```text
sharkbite 1.3.0 -> shoal-sharkbite == 1.3.0
```

Do not loosen this to an unbounded or compatible-release specifier. A new
Shoal implementation version requires the same reviewed `sharkbite` metadata
version.

## Required ordering

1. Merge the Shoal implementation and release-workflow changes.
2. Tag and publish Shoal GitHub Release `v<VERSION>`. Wait for
   `shoal-sharkbite==<VERSION>` on PyPI, then verify its hashes/provenance,
   clean `--no-deps` install, both imports, native ABI, and capabilities.
3. Set both this project version and the exact dependency to `<VERSION>`.
4. Run this repository's build, metadata, dependency, file-ownership, install,
   import, native-file ownership, and `pip check` smoke tests.
5. Merge the compatibility PR only after steps 1-4 establish the version
   contract.
6. Create and publish a GitHub release whose tag is exactly `v<project
   version>`. The protected `pypi` environment must approve publishing.
7. Verify `pip install sharkbite==<VERSION>` in a clean environment, run
   `pip check`, prove imports/native files are owned by `shoal-sharkbite`, and
   retain the checksummed GitHub release assets and artifact attestation.

Never upload from a workstation. Configure PyPI Trusted Publishing for:

- owner/repository: `phrocker/sharkbite`
- workflow: `.github/workflows/publish-pypi.yml`
- environment: `pypi`

Protect the `pypi` environment with required reviewers and restrict it to
release tags. The workflow receives `id-token: write` only in the publish and
attestation jobs; no long-lived PyPI token is used.

Historical implementation source remains in this repository for audit and
migration analysis, but packaging configuration and release verification
exclude it from both wheel and sdist. Removing that source requires a separate
consumer-impact review.
