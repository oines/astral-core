# Reasoning capabilities and API contracts

Reasoning controls come from two independent inputs: each model's declared
capabilities, and the API contract of the selected connection and protocol.
The same provider can expose models with effort levels, a switch, a budget,
mandatory thinking, or combinations of these. Selecting a provider never gives
all of its models the same controls.

`ResolveReasoningSemantics` binds those inputs before the shared control and
request encoder runs. `NormalizeDiscoveryReasoning` performs the same binding
after catalog enrichment. Both preserve explicit unknown values (`null`),
negative capability declarations (`false`), and empty lists. They do not create
`GenerationOptions` defaults. Selecting interface default sends no reasoning
parameters, even when the metadata describes an API default.

## Verified API rules

| Selected connection and API | Binding | Model facts still required |
| --- | --- | --- |
| MiMo / MiMo Token Plan Responses | `none` disables; `high` enables. Non-none spellings do not imply distinct intensities. | Declared switch / disable capability. |
| DeepSeek Responses | Native active levels are `low`, `high`, `max`; API default is `high`; `none` disables. A named declared default can bind switch-on. | Declared reasoning; explicit model option lists and switch availability remain authoritative. |
| Google GenerateContent budget mode | Dynamic budget is `thinkingBudget: -1`; disabling uses `0` only when the model permits disabling. | A budget capability. Level-only models keep `thinkingLevel`; `minimal` is not treated as off. |
| Fireworks Messages | Uses adaptive thinking plus an explicit `output_config.effort`. A source's active `default_effort` can bind switch-on; the first list entry is never selected automatically. | Declared effort values / switch. Exact token budgets cannot be represented by this API. |

Sources: [MiMo Responses](https://mimo.mi.com/docs/en-US/api/chat/responses),
[DeepSeek thinking mode](https://api-docs.deepseek.com/guides/thinking_mode/),
[Google GenerateContent thinking](https://ai.google.dev/gemini-api/docs/generate-content/thinking),
[Fireworks Anthropic compatibility](https://docs.fireworks.ai/tools-sdks/anthropic-compatibility).
These are provider API rules, never model ID tables. They do not apply to those
manufacturers' models behind another provider or an arbitrary compatible API.

Fireworks converts manual token budgets to effort bands. The default adaptive
format rejects an exact budget instead of promising that a token count will be
enforced. An explicit conflicting capability or override remains visible in
metadata, produces an unavailable-control diagnostic, and fails validation.
`Config.RequireAdaptiveEffort` preserves the adaptive effort requirement in
credential-free snapshots. Native Anthropic's independent adaptive behavior is
unaffected.

## Dynamic metadata

Live records may declare canonical capability fields under `reasoning`,
`reasoning_capabilities`, or `capabilities.reasoning`: `supported`, `toggle`,
`disable`, `default_on`, `default_effort`, `efforts`, `budget`, `dynamic_budget`,
`min_budget`, `max_budget`, `on_effort`, and `off_effort`. `mandatory: true`
declares that switching and disabling are unavailable. Existing provider-native
record parsers, including Floway, OpenRouter, Copilot and Codex, remain in use.
An API's unrelated `adaptive` flag does not establish Google's budget sentinel.

A live model record can also declare a supported `reasoning_format` (and a
custom `reasoning_mapping`). That declaration is validated against the model's
actual endpoint. Without it, the selected API's format is used. Public fallback
metadata never selects a manufacturer's request format for a custom provider.

models.dev supplies model support and `reasoning_options`, with optional budget
bounds. Its published options are a complete control declaration when present:
`[]` means no caller controls; omission means unknown. Its current v1 fields do
not contain the wire paths in source TOML comments or generic `on_effort`,
`off_effort`, `default_on`, and dynamic-budget bindings. See the upstream
[catalog contribution contract](https://github.com/anomalyco/models.dev/blob/dev/AGENTS.md).

## Deliberate limits

A gateway's toggle declaration alone does not tell a Responses adapter which
effort enables or disables it. Such routes retain their declared capability and
show unavailable-option diagnostics until live metadata or explicit settings
provide the binding. In particular, a MiMo entry served by Command Code does
not inherit MiMo's direct API's `high` / `none` bindings.

Model-specific protocol exceptions, streaming restrictions, or effort aliases
cannot be reconstructed from a bare model ID and a boolean reasoning flag.
They require current endpoint metadata, a documented provider API rule, or an
explicit configuration. Unknown facts are not replaced with model-name tests,
invented effort lists, guessed token budgets, or silently dropped controls.
