# Third-party references and adaptations

The file-tool matching, normalization, unchanged-line preservation, read
truncation and path behavior in `standardtools` are adapted from Pi at commit
`a4d040d30f2cb30fffbd841b3f0a7052158e2e21` (packages/coding-agent/src/core/tools
and image/path utilities). Copyright (c) 2025 Mario Zechner. Pi's MIT license is
included in [third_party/pi-LICENSE](third_party/pi-LICENSE).

The command tool schemas, yield rules, terminal interaction and bounded-output
behavior were implemented in Go with reference to the local OpenAI Codex source
at commit `af5d95f255`. Codex is licensed under Apache-2.0; its license is included
in [third_party/codex-LICENSE](third_party/codex-LICENSE). This SDK does not embed
the Codex application, sandbox or approval-policy implementation.

astral-core is licensed under Apache-2.0; see [LICENSE](LICENSE). Third-party
adaptations and Go dependencies retain their respective licenses and attribution.

Image Base64 validation and JSON-string validation use
[MoeForge/moego](https://github.com/MoeForge/moego), pinned at
`8aa874cd38e3d4f0579ff2d6c978220eccc1313e` (MIT, copyright 2026 Calcium-Ion).
Its license is provided with the Go module and included in Astral's generated
distribution notices. Non-SIMD Base64 validation and non-string JSON retain
the standard-library compatibility path.

Image encoding uses [gen2brain/vpx v0.2.1](https://github.com/gen2brain/vpx/tree/v0.2.1),
an independent Go/SIMD WebP implementation. Its BSD-3-Clause and webpkit MIT
license files are retained in the dependency module as `COPYING` and
`COPYING.webpkit` and must accompany binary distributions. Core does not copy
or modify the codec's source or install a native shared library.
