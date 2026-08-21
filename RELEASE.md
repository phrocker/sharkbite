# Compatibility package release policy

## Architecture

The PyPI project `sharkbite` is a metadata-only compatibility distribution.
It must never install the `sharkbite` or `pysharkbite` packages, extension
modules, shared libraries, headers, or command-line programs. Those files are
owned solely by the `shoal-sharkbite` implementation distribution.

The compatibility distribution has its own monotonically increasing version
because PyPI already contains historical `sharkbite` releases through
`1.2.0.3`, while Shoal uses an independent semantic version. Each compatibility
release pins one validated implementation release with exact equality:

```text
sharkbite 1.3.0 -> shoal-sharkbite == 0.5.0
```

Do not loosen this to an unbounded or compatible-release specifier. A new
Shoal implementation version requires a reviewed `sharkbite` metadata release.

## Required ordering

1. Merge the Shoal implementation and release-workflow changes.
2. Publish `shoal-sharkbite` and verify its PyPI wheel/sdist, checksums,
   provenance, native bundle, and both `import sharkbite` and
   `import pysharkbite`.
3. Update the exact dependency in `pyproject.toml`, if necessary.
4. Run this repository's build, metadata, dependency, file-ownership, install,
   and import smoke tests.
5. Merge the compatibility PR only after steps 1-4 establish the version
   contract.
6. Create and publish a GitHub release whose tag is exactly `v<project
   version>`. The protected `pypi` environment must approve publishing.
7. Verify `pip install sharkbite==<version>` in a clean environment and retain
   the checksummed GitHub release assets and GitHub artifact attestation.

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
