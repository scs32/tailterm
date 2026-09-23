# Package QA manifest

Bug `wi_5209b017e66bbf20` revision 1; source order 5969; Mini package release order 6142.

- Candidate: `5209674b61ce5f91d826e34908adf375379c4fe6`
- Tree: `5064fd0b9abb73c5fe8bbc8eccaca925689e9f76`
- Parent: `5793de112e1b4c0a08d55fba17a7736e0bf2e0bf`
- Packaged `tt`: 6,919,410 bytes; SHA-256 `979ba2216c2167cf7500cbfc60dffd4d9e33a2413707a1f8b582c8c12238f4c2`
- Package manifest SHA-256: `db7df6fc388a458a8b829e1675ee3e1501ffcd8e1b92ac74e1ce9fc9adbab81f`
- Package checksum manifest SHA-256: `631f44ae596cf7e2f343106c3be4e595d59a523823a08cb392df26efc8c9ef90`
- `tt-repro` SHA-256 matches the packaged CLI and `cmp` is byte-identical.
- Clean standalone source VCS metadata: commit above, `vcs.modified=false`, Go `go1.26.6`, `CGO_ENABLED=0`, `GOOS=darwin`, `GOARCH=arm64`, `-trimpath`, `-buildvcs=true`, `-ldflags=-s -w -buildid=`.

Independent synthetic-loopback execution used only a temporary private `HOME` and `XDG_CONFIG_HOME`.

- [package-smoke-independent.json](package-smoke-independent.json), SHA-256 `d004b16ad7b1e72c585cf61043432e0b8fdf86c3c62dc6b7e3939403aab342df`
- [package-smoke-independent.log](package-smoke-independent.log), SHA-256 `d004b16ad7b1e72c585cf61043432e0b8fdf86c3c62dc6b7e3939403aab342df`

The exact external binary passed valid file and inline contexts with canonical digest and frozen identity tuple; Board-only, null, mismatched, missing-history and malformed contexts made zero allocation POSTs; a dropped response and unchanged retry had equal transport payloads. This is package transport qualification only; it does not claim durable store replay or installed-CLI qualification.
