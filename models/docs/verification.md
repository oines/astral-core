# Provider verification

The normal suite runs against local HTTP fixtures and needs no credentials.
It covers request encoding, opaque reasoning, ordered Responses replay, stable
stream indices, incomplete calls, transport/provider errors, cancellation, and
Core session persistence with SQLite.

On 2026-10-20, the opt-in live suite passed against `deepseek-flash` at
`https://api.deepseek.com` for both Chat Completions and Responses:

| Scenario | Chat Completions | Responses |
| --- | --- | --- |
| One real function call and result continuation | Passed | Passed |
| Native reasoning retained and replayed | Passed | Passed |
| SQLite close/reopen and subsequent request | Passed | Passed |
| `no_reply` ends without an extra model request | Passed | Passed |
| New activation after `no_reply` | Passed | Passed |
| Usage reported | 5 / 5 requests | 5 / 5 requests |

Each route made five model requests, used `reasoning` effort `low`, and
executed the test `echo` tool once. This validates the tested model/endpoint
combination, not every DeepSeek model or every provider.

To repeat with your own credential in the process environment:

```sh
go test -tags=integration ./models/tests -run TestDeepSeekLive -v -count=1
```

The test skips without `DEEPSEEK_API_KEY`. It does not load `.env`, print keys,
dump response bodies on errors, or retain its temporary session database.
OpenAI, Qwen, and OpenRouter have fixture coverage; live verification for those
routes is not claimed here.

The 2026-10-21 fixture pass also covers Anthropic Messages and Google
GenerateContent in JSON and SSE modes, native signed thinking replay, ordered
tool calls and tool results, large integer arguments, truncated streams, and
scope mismatch rejection. Responses recovery removes rejected encrypted
reasoning from a request copy while preserving tool calls and results. Native
thinking/signature recovery is covered for Anthropic and Google as well.

The Core integration fixture verifies one recovery attempt, persistence of
each actual request/response, unchanged original history, and no repeated tool
execution. These are local protocol tests, not live account verification for
Anthropic, Google, Codex, Copilot or the coding-plan providers.

On 2026-10-22, the full Go suite and race checks passed against Core's direct
`NewSession`/`OpenSession` API and format 9 SQLite store. Chat Completions,
Responses, Anthropic and Google fixtures preserve ordered environment snapshots
and stable request prefixes. Recovery remains a single request-copy repair
within Core's total retry budget; adapters do not run retry loops. This refactor
check used local protocol fixtures and did not repeat the live-account suite.


The monorepo refactor adds local fixtures for Responses → Chat → Responses
switches, all four protocol target projections, paired portable tool IDs,
unchanged source history, continued native reasoning accumulation, credential-free
snapshot restoration and Core frozen-image encoding. These tests do not make
real provider requests. Historical live results above describe the versions
that were tested on those dates; the refactor did not repeat them.
