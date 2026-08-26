# Compatibility package release policy

## Architecture

The PyPI project `sharkbite` is a compatibility distribution. It installs only
`sharkbite/__init__.py`, after the exact same-version `shoal-sharkbite`
dependency. This final-write initializer is required because pip installs
dependencies before dependents: while upgrading historical `sharkbite`, pip
otherwise removes the initializer that Shoal just installed. The bridge
initializer extends the package path for split-site installations, then
matches Shoal's same-version initializer.

`shoal-sharkbite` owns every implementation module, the `pysharkbite` package,
extension and shared libraries, headers, data, and command-line programs. The
two distributions intentionally record the identical `sharkbite/__init__.py`
path; release verification enforces that this is their only overlap.

Sharkbite issue
[#108](https://github.com/phrocker/sharkbite/issues/108) is the authoritative
compatibility-layer migration gate. This repository provides only the
install-name and release bridge; publishing it is not evidence that the
compatibility implementation or live-cluster conformance is complete. Keep
#108 open until `SB-SCAN-022`, `SB-SCAN-026`, `SB-XCUT-015`, and
`SB-XCUT-016` pass.

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
4. Run this repository's build, metadata, dependency, file-ownership, clean
   install, historical in-place upgrade, import, native-file ownership, and
   `pip check` smoke tests.
5. Merge the compatibility PR only after steps 1-4 establish the version
   contract.
6. Create and publish a GitHub release whose tag is exactly `v<project
   version>`. The protected `pypi` environment must approve publishing.
7. Verify both a clean `pip install sharkbite==<VERSION>` and an ordinary
   upgrade from `sharkbite==1.2.0.3`. Run `pip check`, prove only the
   compatibility initializer is shared while implementation/native files are
   owned by `shoal-sharkbite`, and retain the checksummed GitHub release assets
   and artifact attestation.

Uninstalling only `sharkbite` removes the shared initializer while leaving its
dependency installed. Users intentionally switching to direct
`shoal-sharkbite` ownership must reinstall `shoal-sharkbite` afterward.

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

The legacy native workflow remains available for changes to the historical
C++ build surfaces. Packaging-only changes are validated by
`publish-pypi.yml` and do not invoke that unrelated native build.
