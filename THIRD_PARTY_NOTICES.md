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

Go dependencies retain their respective licenses in their modules. These notices
do not select a license for the remainder of astral-core.
