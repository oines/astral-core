# Attribution and third-party notices

## Models / Twilight AI

The model integration packages under `models/` are an Astral-focused fork of
[Twilight AI](https://github.com/felinics/twilight), built on the original work
of its [authors and contributors](https://github.com/felinics/twilight/graphs/contributors).
HTTP/SSE utilities, provider error handling, wire structures, and reasoning
replay code derive from Twilight. The API has intentionally diverged from
upstream.

The Twilight-derived code is licensed under the Apache License, Version 2.0.
The original license is preserved in [models/LICENSE](models/LICENSE), and
existing copyright notices and attribution are preserved unchanged. Moving
these packages into this monorepo does not replace the upstream authorship.
Further package information is in [models/README.md](models/README.md).

## Other source references and adaptations

The existing Pi and OpenAI Codex references, adaptations, copyright attribution,
and license-file locations are recorded in
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md). The root project license is in
[LICENSE](LICENSE). Third-party adaptations and Go dependencies retain their
respective licenses and attribution.
