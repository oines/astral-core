# Provider integration

`integration` provides provider API presets, complete account catalogs, optional
models.dev metadata, and configuration resolution without application storage.
It does not encode a static inventory or select behavior from model names.

- `ProviderPresets` / `FindPreset` describe API contracts and authorization modes.
- `Catalog.List` reads the complete connection inventory before returning. Failed,
  malformed or incomplete listings return an error, never a partial inventory.
- `NewModelCatalog(cachePath, publicHTTPClient)` caches models.dev independently
  of authenticated requests. A failed refresh preserves the last valid snapshot.
- `Catalog.Recommend` describes an arbitrary full model ID without contacting the
  connection. Public metadata never adds an ID to authenticated inventory.
- `Resolve(provider, model, spec)` produces the actual protocol, adapter config,
  effective values, executable reasoning controls, and diagnostics together.
- `Options(defaults, overrides, reasoning)` applies per-call choices. A nil
  reasoning pointer inherits model defaults; an empty object selects API defaults.
- `RebaseRecommendations` and `RebaseSources` clear old route facts after an
  explicit protocol change while leaving separately stored overrides intact.

The application supplies credential values and storage. Use `Headers` for API
credentials, or `models/auth.Client.HTTP` with a credential-store callback for
Codex and Copilot OAuth, then construct the adapter through `models/provider.New`.
Use `models/provider.Call` for one invocation without a Session or agent loop.

For persisted requests, `models/provider.Freeze` captures the final adapter
configuration without secrets. The application can keep immutable credential
references beside that snapshot and supply them to `models/provider.Restore`.
OAuth refresh rotates the captured credential in place; authorizing a new account
creates a different credential reference and replay identity.

Model IDs remain exact data. The current connection's declared metadata wins;
models.dev supplies optional recommendations; explicit user overrides remain
separate. Unknown and conflicting facts stay unknown. Native reasoning values
are preserved and validated against the resolved API encoding.

The reasoning capability and API-contract boundary, verified rules, metadata
fields, and remaining limits are documented in [reasoning.md](reasoning.md).

The SDK does not reconcile application inventory or choose deletion policy.
That transaction belongs to the application after a successful complete listing.
