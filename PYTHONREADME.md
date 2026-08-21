# Sharkbite compatibility package

`sharkbite` is now a thin compatibility package. The maintained implementation,
including the `sharkbite` and `pysharkbite` import packages and bundled native C
ABI, is published by the Shoal project as `shoal-sharkbite`.

Existing installation commands remain valid:

```console
pip install sharkbite
```

The compatibility distribution installs no Python modules or native files of
its own. It depends exactly on the tested Shoal implementation release, so
there is a single owner for every installed import and native library.

For implementation documentation and issue reporting, see
[Shoal](https://github.com/phrocker/shoal-oss).

Release maintainers must follow [RELEASE.md](RELEASE.md). In particular,
`shoal-sharkbite` must be published and verified before the corresponding
`sharkbite` compatibility release.
