# Native v4 header oracle

`native_v4_no_crypto_header.bin` was written by `NativeHeaderFixture.java` with
the pinned Accumulo source revision
`1a716b2c1bb5762ead4b46d2bc4f53e13873b314` used by
`test/accumulo/image/Dockerfile`. The downloaded source archive matched that
Dockerfile's SHA-512:
`bf2cac6f7b5f759358ac306bba2342dc74c51bf63d70905da1004fc1eb0cfa66f6a73023ab250824a67f17d410e9b3351f097ddfd742bbc4b14f7a7feb6c4b2e`.

The generator compiled that revision's `NoCryptoService.java`,
`NoFileEncrypter.java`, and `CryptoUtils.java`, then called
`CryptoUtils.writeParams(new NoFileEncrypter().getDecryptionParameters(), out)`.
Its trailing `fa` byte is an entry-position marker. This is a **header-only**
oracle; it does not validate mutation decoding or recovery semantics.

The complete fixture bytes are the v4 magic, `00 00 00 07`, ASCII `U+1F47B`,
and `fa`. The exact seven-byte parameter is the native no-crypto sentinel;
zero-length parameters were only a synthetic Go assumption.
