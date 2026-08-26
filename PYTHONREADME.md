# Sharkbite compatibility package

`sharkbite` is now a compatibility package for the Shoal implementation. The
Shoal project publishes the maintained modules, `pysharkbite` package, and
bundled native C ABI as `shoal-sharkbite`.

Existing installation commands remain valid:

```console
pip install sharkbite
```

The compatibility distribution depends exactly on the tested Shoal
implementation release. It installs only the final `sharkbite/__init__.py`
entry point so that a normal upgrade from historical `sharkbite` releases
cannot delete Shoal's newly installed initializer. All implementation modules
and native files remain supplied by `shoal-sharkbite`.

For implementation documentation and issue reporting, see
[Shoal](https://github.com/phrocker/shoal-oss).

This install-name bridge is packaging evidence for the authoritative
[Sharkbite migration gate](https://github.com/phrocker/sharkbite/issues/108);
it does not complete that gate. The compatibility implementation already lives
in Shoal. Issue #108 remains open until its four required live Accumulo rows
pass: `SB-SCAN-022`, `SB-SCAN-026`, `SB-XCUT-015`, and `SB-XCUT-016`.

Release maintainers must follow [RELEASE.md](RELEASE.md). In particular,
`shoal-sharkbite` must be published and verified before the corresponding
`sharkbite` compatibility release.
