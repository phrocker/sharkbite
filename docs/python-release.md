# Python distribution and release artifacts

Shoal publishes the Python distribution as **`sharkbite`**. It provides
both compatibility import names:

```python
import sharkbite
import pysharkbite

assert pysharkbite.Client is sharkbite.Client
```

The `sharkbite` PyPI project is the canonical distribution. It owns the
import packages, native artifacts, ABI checks, and release provenance. The
existing pre-1.3 releases remain immutable; `1.3.0` is the migration point to
the Shoal-backed implementation.

The distribution version is independent of the native ABI version. Release
`1.3.0` requires Shoal ABI `1.19.0` or newer within ABI major 1. The package is
still an incremental compatibility layer: publishing it does not make the
uncovered required rows in `sharkbite-compatibility.md` compatible. Scope is
defined by `sharkbite-client-scope.md`, not by the raw count of historical C++
declarations.

## Platform status

| Platform | Artifact status | Policy |
| --- | --- | --- |
| Linux x86-64, glibc 2.28+ | First supported wheel target | Build as `manylinux_2_28_x86_64` in a glibc 2.28-or-older manylinux environment and require `auditwheel show` to pass. |
| Linux aarch64, glibc 2.28+ | Build-capable, not yet release-validated | The scripts emit and verify `manylinux_2_28_aarch64`; publication waits for native aarch64 CI evidence. |
| macOS x86-64/arm64 | Go cross-build and unbundled package preview verified; native runtime unsupported | `scripts/cross_platform_artifacts.py` cross-builds the non-CGO Shoal command and reproducibly builds each platform-tagged preview. Preview wheels intentionally contain no `.dylib`, install into a target-platform layout, and prove imports plus the expected native-library discovery error. Publication still requires a native `c-shared` build, linked C/C++ execution, and clean native installs on each architecture. |
| Windows amd64 | Host-verified shared/static and Python artifact path | The deterministic evidence script builds shared and static C ABI artifacts, links and executes C11/C++11 lifecycle clients, verifies all 323 `shoal_*` exports plus ABI 1.19.0/capability discovery, and repeats clean wheel/sdist builds to prove identical hashes. |

The source distribution is platform-neutral Python source and intentionally
contains no native binary. A platform wheel contains exactly one native Shoal
library.

## Native-library trust and discovery

The loader uses this order:

1. an explicit path passed to `NativeAPI`;
2. an absolute path in `SHOAL_LIBRARY`;
3. the wheel's private `sharkbite/.libs` library after checking its SHA-256
   against `_shoal_bundle.json`;
4. the operating-system loader only when
   `SHOAL_ALLOW_SYSTEM_LIBRARY=1` is explicitly set.

The current working directory and repository build directories are never
searched implicitly. This prevents accidental or attacker-controlled library
preloading. The loader resolves ABI discovery symbols first, rejects a major
other than 1, rejects versions older than 1.19.0, checks the packed and tuple
versions agree, and dynamically resolves capability symbols before use.

## Reproducible build

Build dependencies are pinned in `python/pyproject.toml`. `SOURCE_DATE_EPOCH`
defaults to the source commit timestamp, Python hash randomization is fixed,
Go paths/build VCS metadata/build IDs are removed, and archive metadata is
normalized by the Python build backend.

From a clean checkout:

```text
python -m pip install build==1.2.2.post1 setuptools==75.8.0 wheel==0.45.1
python python/scripts/build_release.py --platform-tag manylinux_2_28_x86_64
python python/scripts/verify_release.py
```

The manylinux command must run in a controlled manylinux_2_28 build image. The
script rejects newer glibc hosts and requires an installed `auditwheel` unless
`--skip-auditwheel` is used for non-release development checks. It never
downloads a native dependency.

Outputs under `python/dist/`:

- a platform wheel with the native library and internal bundle digest;
- a source-only sdist;
- `SHA256SUMS`;
- deterministic `release-manifest.json` containing source commit, epoch,
  package/ABI versions, platform tag, sizes, digests, and the detached-signing
  expectation.

Sign `release-manifest.json` and `SHA256SUMS` in the publication system. Do not
place private keys or credentials in the repository or build environment.

## Controlled manylinux release workflow

`.github/workflows/python-manylinux-release.yml` is the authoritative Linux
x86-64 build and publication lane. It runs for relevant pull requests, version
tags, published GitHub releases, and manual dispatches. The job uses an
immutable `manylinux_2_28_x86_64` image digest, downloads the exact Go 1.25.0
toolchain after verifying its published SHA-256 digest, installs a complete
hash-locked Python build-tool set from `python/requirements-release.txt`, and
builds the artifacts twice. Both `SHA256SUMS` and the complete release manifest
must be byte-identical before artifacts are retained.

Every build performs archive validation, a clean wheel install/import/native
ABI mutation smoke test, a preload-before-import test, and a clean source
distribution build/install/native smoke test with the Go toolchain absent from
the test environments. Non-pull-request runs also run the standalone execution
fault campaign before artifact attestation and publication. The campaign checks
durable crash, fence, and replay invariants plus a two-container claim race.
Its machine-readable evidence and the build artifacts are retained for 30 days.

To validate the current `main` revision without publishing:

1. Open **Actions → Publish Python manylinux artifacts → Run workflow**.
2. Select `main` and run the workflow.
3. Download `sharkbite-manylinux-<commit>` and verify it locally with
   `sha256sum --check SHA256SUMS`.
4. Verify provenance with
   `gh attestation verify <artifact> --repo phrocker/sharkbite`.

Publication is deliberately release-bound. Publishing a GitHub release for
`vMAJOR.MINOR.PATCH` starts the release workflow. Once its gates pass, the
workflow uploads the wheel, sdist, manifest, and checksums to that release,
then publishes the exact previously built wheel and sdist to the **`sharkbite`**
PyPI project. The GitHub release entry itself can exist before these gates pass.
The tag must exactly match the version in `python/pyproject.toml`. Tag pushes
alone build and validate but do not publish.

Manual dispatch is safe by default: `publish_to_pypi` defaults to false, so a
normal dispatch only validates and retains workflow artifacts. A manual PyPI
publication requires all three controls: select the matching version tag as
the workflow ref, explicitly enable `publish_to_pypi`, and receive approval
from the protected `pypi` GitHub environment. Selecting a branch with the
publish input enabled fails before publication.

The PyPI job downloads the run artifact produced by the read-only build job,
then independently verifies the exact wheel/sdist set, every size and SHA-256
digest in `release-manifest.json`, `SHA256SUMS`, source commit, package name,
version tag, and platform before copying only the two distributions into the
upload directory. PyPI authentication uses Trusted Publishing OIDC in the
isolated job with `id-token: write`; no PyPI username, API token, repository
secret, or environment secret is used. The pinned PyPA publish action also
uploads PyPI attestations.

GitHub release uploads do not use `--clobber`; an existing same-named asset
causes publication to fail rather than silently replacing released bytes. The
artifact build job has read-only repository permissions for every trigger.
Separate jobs receive only the GitHub attestation, GitHub release, or PyPI OIDC
permissions they require.

### One-time PyPI and GitHub configuration

The distribution/project name is **`sharkbite`**. It owns and installs the
`sharkbite`, `pysharkbite`, and `shoal` import packages, package data, native
library, and release metadata. The PyPI project is maintained by the Sharkbite
maintainers.

Configure the publisher once:

1. In repository **Settings → Environments**, create an environment named
   exactly `pypi`. Require designated maintainer approval, restrict deployment
   branches/tags to protected `v*` release tags, and prevent unreviewed
   environment bypass according to repository policy. Store no PyPI secret.
2. In the `sharkbite` PyPI project's **Publishing** settings, add a
   GitHub trusted publisher with owner `phrocker`, repository `sharkbite`,
   workflow `python-manylinux-release.yml`, and environment `pypi`.
3. Confirm that the PyPI project owner and the repository owner are the same
   release authority before enabling publication.

Trusted Publisher values are identity-sensitive; changing the repository,
workflow filename, or environment requires updating PyPI before the next
release. Validation runs must not be used to test public publication. Build,
install, import, and native ABI evidence is proved in the separate read-only
build job and can run without PyPI configuration.

### Release and chaos gate

Use one new PEP 440 version. The wheel and source distribution are publishable
only after the controlled manylinux build, provenance checks, container chaos
matrix, and standalone execution fault campaign all pass:

1. Build the wheel and source distribution from the matching `vVERSION` tag.
2. Run the release verification in clean containers for every supported Python
   version. Check import identity, ABI/capability discovery, checksums, and
   native mutation behavior.
3. Inject failures in disposable containers with `chaos_release.py`: set a
   relative `SHOAL_LIBRARY` path, corrupt the bundled checksum manifest, tamper
   with the bundled native library, remove it, and restart the interpreter
   twenty times. Every injected failure must produce the documented
   `ImportError` for that injection; a crash, a signal exit, or an unrelated
   exception fails the gate. Incompatible-library preloading, interrupted
   dependency resolution, and concurrent scanner/writer cleanup are not part
   of this gate.
4. Run the durable execution crash/fence/replay campaign and two-container
   claim race against the release commit. Both jobs must pass before attestation.
   This gate does not yet cover hybrid, Accumulo, or multi-server execution.
5. Attest the verified artifacts, then publish the exact distributions through
   the protected `pypi` environment. Never rebuild between verification and
   publication.

The `sharkbite` wheel owns all import roots and native artifacts. No second
package may install overlapping `sharkbite/`, `pysharkbite/`, or `shoal/` paths.
PyPI artifacts are immutable; correct a failed release with a new version.

`verify_release.py` validates both archives and manifests, then installs the
wheel into a fresh project-local virtual environment with `--no-index` and
with the Go toolchain absent from `PATH`. It also builds the source-only sdist
into an unbundled `py3-none-any` wheel and installs that in a second clean
environment. Both paths verify that the installed `sharkbite`
distribution owns the `sharkbite` and `pysharkbite` import roots, that their
complete implemented public exports have object identity, load a trusted
native library, check ABI/capabilities, and execute a native mutation smoke
test. A separate fresh interpreter preloads the library with `ctypes.CDLL`
before either import; the normal path imports first and uses package discovery,
so both legacy preload drivers and safe no-preload consumers are covered.

The build and verification scripts are host-portable. Cross-compilation is not
accepted as runtime evidence; macOS remains explicitly unverified until those
same commands run on a macOS host.

## Cross-platform evidence

Run the complete local evidence pass with:

```text
python scripts/cross_platform_artifacts.py
```

On Windows, `scripts\cross-platform-artifacts.ps1` is an equivalent wrapper.
The generated `build/cross-platform-artifacts/evidence.json` records host
tool versions, artifact hashes, export counts, ABI/capability checks, Go
cross-builds, and exact unsupported-runtime reasons. Generated binaries are
not committed.

The macOS previews are deliberately **unbundled**. They are packaging evidence,
not releasable wheels: `_shoal_preview.json` marks runtime unsupported,
`verify_preview.py` rejects any bundled native library, installs for the stated
target tag with `pip --target`, verifies both imports, and requires `NativeAPI`
to fail unless a trusted native library is explicitly supplied.
