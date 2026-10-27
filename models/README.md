# Models

The model integration packages in the [Astral Core monorepo](https://github.com/oines/astral-core). Each adapter implements `astral.Model` directly and accepts Core’s ordered messages. There is no second agent loop or SDK message model.

This is an Astral-focused fork of [Twilight AI](https://github.com/felinics/twilight), built on the original work of its [authors and contributors](https://github.com/felinics/twilight/graphs/contributors). HTTP/SSE utilities, provider error handling, wire structures, and reasoning replay code derive from Twilight. The original [Apache License 2.0](LICENSE) and existing copyright notices are preserved unchanged. The API has intentionally diverged from upstream.

## Supported routes

| Protocol | Dialects | Reasoning controls |
| --- | --- | --- |
| Chat Completions | OpenAI, DeepSeek, Qwen, OpenRouter, MiMo, compatible endpoints | Explicit effort/toggle/budget formats, including Together and Fireworks |
| Responses | OpenAI, DeepSeek | Native `reasoning.effort`, ordered native-item replay |
| Anthropic Messages | Anthropic-compatible endpoints | Native thinking blocks, budgets and signed replay |
| Google GenerateContent | Gemini-compatible endpoints | Native thinking configuration and thought signatures |

Text, Core frozen image pixels, resolved image URLs/data URLs, function tools, streaming and non-streaming replies are supported. Unsupported input parts and unsupported options fail explicitly. Embedding, audio/video generation, and Twilight’s agent runtime are outside this fork’s current scope. Compatibility is model/endpoint-specific; see [verification results](docs/verification.md) for the tested DeepSeek routes and the limits of fixture coverage.

## Use

Requires Go 1.25.7 or later.

```go
import (
    "context"
    "os"

    astral "github.com/oines/astral-core"
    models "github.com/oines/astral-core/models"
    "github.com/oines/astral-core/models/provider"
    "github.com/oines/astral-core/models/provider/openai/responses"
)

func generate(ctx context.Context) (astral.Reply, error) {
    model, err := provider.New(responses.Protocol, models.Config{
        Model: os.Getenv("MODEL_ID"),
        APIKey: os.Getenv("OPENAI_API_KEY"),
        ReplayScope: "my-connection:account-1",
    })
    if err != nil { return astral.Reply{}, err }
    return provider.Call(ctx, model, astral.Request{
        Messages: []astral.Message{astral.User("Hello")},
    }, func(event astral.ModelEvent) {
        // Observe progress. Only the returned Reply is durable content.
    })
}
```

Direct calls may use only a remote model ID, API endpoint and credentials. Unknown token limits remain zero; no window is invented and `Spec.ID` defaults to the remote model ID. Chat Completions, Responses and Google omit the output limit when `Request.MaxOutputTokens` is zero, allowing the API default. Anthropic Messages requires a caller-supplied positive output limit. Explicit positive limits are checked against the model maximum only when that maximum is known. Negative limits are rejected.

Core agent sessions still require verified positive context and maximum-output limits for bounded budgeting and compaction. Supply them through `Config.Spec` before using the adapter in a session. `ListModels` returns discovery identities; it does not assert reasoning, image, tool, or replay support.

`provider.Call` runs preparation, validation and one HTTP attempt. It needs no Session or Store and returns tool calls to the caller. Core's agent loop invokes the same adapter boundaries before freezing a request and executing tools.

`provider.Freeze(protocol, config)` returns a credential-free `core.ModelSnapshot` containing the normalized route, final reasoning configuration, compatibility options and stable replay scope. `provider.Restore(snapshot, provider.Credentials{...})` reconstructs that target without reapplying current catalog/default rules. API keys, headers and HTTP clients are excluded from serialization; the caller supplies credentials from its external credential reference. `SnapshotConfig` returns an owned, redacted copy.

## Reasoning controls

Bind `Config.ReasoningFormat` and `Config.Reasoning` to the exact connection, protocol and remote model. `ReasoningFormats(protocol)` lists compatible formats; `ResolveReasoningControls(protocol, config)` returns only selections the same request encoder can execute. The `models/integration` package combines connection configuration, live capability metadata, models.dev records and explicit overrides. Host code persists those inputs; Models resolves protocol behavior and executable controls. An empty `ReasoningOptions` reaches the provider without reasoning parameters, even when capability metadata describes an enabled API default.

Public metadata lookup first matches the complete model slug after the final `/` exactly. Only if no exact slug exists does it retry ignoring letter case; dates, versions and suffixes must still match. The persisted and requested model ID is never rewritten. Multiple matching records still merge field by field under the same source ranking: live connection data, the current provider's public records, an explicit namespace, maker hints, then other agreeing sources. Conflicts at the same priority remain unknown; a lookup does not promise one unique source.

For a verified Responses compatibility difference, set `Config.ResponsesCompatibility.SupportsInclude` or `SupportsStore` to `false`. The encoder then omits its automatic `include: ["reasoning.encrypted_content"]` or `store: false` field and rejects an explicit value for that key in `ProviderOptions`. When only the encrypted reasoning include value is unsupported, set `SupportsEncryptedReasoningInclude` to `false`; other native include values remain available. Nil retains standard Responses behavior. Native output items, including any encrypted reasoning the service returns, are still preserved and replayed without modification.

`Efforts` is a list of native strings: `nil` means unknown, while `[]` explicitly means no effort selector. `Toggle`, `Disable`, `Budget` and budget bounds describe independent controls. `DefaultEffort` and `DefaultOn` explain API defaults without adding request parameters. `OnEffort` and `OffEffort` explicitly bind on/off to native effort tokens when that route uses them. `ParseReasoningMetadata` extracts sparse capability fields from live model records, preserving explicit null, false and empty lists. It never chooses a request format from the model name.

The `custom` format accepts a bounded `ReasoningMapping` of literal on/off and effort JSON fragments. Fragments cannot modify messages, tools, credentials, routing, output limits or other request-owned fields. Every advertised selection is validated through `EncodeReasoning`, which also powers each provider's `RequestBody`. `ProviderOptions` cannot bypass the reasoning selection through another managed field; unrelated siblings such as `reasoning.summary` remain available. An omitted format uses `protocol-default` with the same strict capability validation. Dialect and model names never implicitly select a reasoning format.

## Contracts

- A `Stream` call makes one HTTP attempt and stops emitting before it returns. Core's single `Session` implementation owns retries, tool execution, input queues, compaction, and forks. Ordinary generation and summaries share a total attempt budget; adapters never retry independently.
- Host code owns configuration persistence, secure credential storage, permissions, catalog inventory policy and media acquisition. Models owns API/OAuth protocols, catalog parsing, provider presets, capability/default resolution and frozen-pixel wire encoding. Concrete model IDs are data, not hardcoded routing or capability rules. Bind an immutable adapter to each resolved configuration.
- `GenerationOptions` preserves unset versus explicit zero/false. Effort names remain native values; the adapter does not invent a universal low/medium/high scale or convert effort to a token budget.
- Protocol-specific JSON belongs under its protocol key in `ProviderOptions`: `openai-completions`, `openai-responses`, `anthropic-messages` or `google-generative-ai`. Conflicting fields and history/stream controls are rejected.
- Responses items retain original order and opaque JSON, including encrypted reasoning, item/call IDs, annotations, and unknown fields. Native item data is authoritative for replay; parsed text and calls support display and execution, without a second comparison during replay. Chat retains the native reasoning payload, including late encrypted details and structured thinking/text chunks returned by Mistral-compatible routes. Display text cannot replace replay data.
- Opaque state is bound to protocol, remote model, and replay scope. Every adapter implements `PrepareRequest`: accepted native content is retained; incompatible wrappers are removed from a copy while canonical text and paired tool calls/results survive. Private-only reasoning/provider blocks and empty assistant messages are omitted. Original history is unchanged, and newly returned native content continues to accumulate. Set `ReplayScope` to a stable connection identity that survives ordinary token refresh; otherwise the library derives it from endpoint, key, and configured headers. Raw `RequestBody`/`Stream` reject unprepared incompatible native data.
- If a provider explicitly rejects its own encrypted reasoning or signature, `RecoverRequest` offers Core a copy with that opaque replay state removed. Core permits one repair within the same total attempt budget, rechecks its budget, and records both actual requests. Plain text, tool calls and results remain intact; durable history is unchanged. Authentication failures, timeouts and generic validation errors never trigger this removal. Failed stream fragments and retry diagnostics never become subsequent conversation or summary input.
- Normalized `Usage.InputTokens` includes cached reads/writes; cache fields are breakdowns, not additional input. Provider context-limit errors are exposed to Core as `ModelError.ContextOverflow`, distinct from transient rate/server errors.
- `RequestBody` exposes the credential-free wire body. `EstimateTokens` estimates model-visible text, reasoning, tool arguments/schemas and system content. It uses roughly four ASCII characters per token, one token per non-ASCII character (two for supplementary characters), and 1200 tokens per image by default; `ImageTokenBudget` overrides the image approximation. Encoded image bytes and HTTP envelope keys are not text tokens. `ValidateRequest` separately checks the complete wire encoding and final output constraints. Core additionally enforces the conversation/tool batch contract. Full requests, empty requests and incremental message tails use the same estimator; a Host may supply a model-specific tokenizer.
- `Usage.Available` distinguishes a missing usage report from a present report. `InputMissing` and `OutputMissing` mark counts omitted by a present report, including an empty usage object; explicit zero remains known. Chat and Responses accept each other's input/output and detail field names as fallbacks, with native fields (including zero) taking precedence. Totals never supply missing counts by subtraction. Anthropic merges reported fields across stream events before adding cache reads/writes to input; Gemini adds reported thoughts to output but keeps output missing when the candidate count was omitted. Optional cache/reasoning counts remain optional. `models.APIError` remains available through `errors.As`, with Core retry classification around it.

## Development

These packages share the root module with Core; no nested module or developer-specific workspace is needed. Run commands from the repository root. The normal suite uses local fixtures; real-account tests require the explicit `integration` build tag.

```sh
go test -race ./models/...
go vet ./models/...
```

Tests cover native parameter mapping, encrypted reasoning replay, output order, stream indices, malformed/truncated/error responses, cancellation, multimodal tool output, and Core SQLite close/reopen plus `no_reply` boundaries.
