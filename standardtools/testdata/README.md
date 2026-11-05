# Pi reference fixtures

The `pi_*.json` fixtures contain expected results generated from
Pi commit `a4d040d30f2cb30fffbd841b3f0a7052158e2e21`. They exercise original-file
multi-edit matching, Unicode fallback, unchanged-line preservation, rejection
cases with exact error text, byte/line boundaries, complete read Content/Details,
file-tool descriptions and schemas, and edit argument preparation. Tests consume
these frozen results with Go only.

To regenerate deliberately after reviewing an upstream change:

```sh
node standardtools/testdata/generate_pi.mjs /path/to/pi-mono
go test ./standardtools
```

The generator reads the local checkout and loads audited TypeScript in memory.
Pure edit/truncation functions run unchanged. File definitions use a minimal
TypeBox schema builder; text read invocations receive injected byte buffers and
no filesystem access. It requires Node with `stripTypeScriptTypes`; it does not
install Pi dependencies, edit that checkout or invoke its test runner. Diff formatting,
image encoding and process execution have separate tests and documented limits.
