# Installed Mini CLI QA manifest

Bug `wi_5209b017e66bbf20` revision 1; source order 5969; Mini release order 6142.

- Absolute installed binary: `/Users/stephenspeicher/.local/bin/tt`
- Installed binary: 6,919,410 bytes, mode `0755`, SHA-256 `979ba2216c2167cf7500cbfc60dffd4d9e33a2413707a1f8b582c8c12238f4c2`
- Frozen package binary: identical bytes and SHA-256.
- Preserved rollback: `/Users/stephenspeicher/.local/bin/tt-before-allocation-context5209674-6142`, mode `0755`, SHA-256 `2c23f9975462afb0bb587d7e213ae6272cae6062e6cf9a09bfba451133b1b378`.
- Installation receipt SHA-256: `3487d8c9c33f026186907974692c82d1fa54cf986cee470f86df0ae5541e5744`.
- Source provenance: `5209674b61ce5f91d826e34908adf375379c4fe6`, tree `5064fd0b9abb73c5fe8bbc8eccaca925689e9f76`, parent `5793de112e1b4c0a08d55fba17a7736e0bf2e0bf`.

The retained root smoke operator is byte-identical to the frozen package operator. I ran it against the absolute installed path with only its private temporary `HOME`/`XDG_CONFIG_HOME` and a loopback HTTP server.

- [installed-smoke-independent.json](installed-smoke-independent.json), SHA-256 `98d4f7a9051804d97b4f57c2c9525636fde35f85f72345bd2cdb5a926a677c32`
- [installed-smoke-independent.log](installed-smoke-independent.log), SHA-256 `98d4f7a9051804d97b4f57c2c9525636fde35f85f72345bd2cdb5a926a677c32`

Both valid file and inline contexts made one canonical request retaining frozen identities. Board-only, null, mismatched, missing-history, and malformed contexts made zero allocation requests. A dropped response and unchanged retry had equal transport payloads. This verifies installed CLI preflight and transport only; it does not validate digest-only API bytes, durable store replay, or broader Bug completion.
