# Pi reference fixtures

`pi_edits.json` and `pi_truncate.json` contain expected results generated from
Pi commit `a4d040d30f2cb30fffbd841b3f0a7052158e2e21`. They exercise original-file
multi-edit matching, Unicode fallback, unchanged-line preservation, rejection
cases and byte/line boundaries. Tests consume these frozen results with Go only.

To regenerate deliberately after reviewing an upstream change:

```sh
node standardtools/testdata/generate_pi.mjs /path/to/pi-mono
go test ./standardtools
```

The generator reads the local checkout and loads the two pure TypeScript exports
in memory. It requires Node with `stripTypeScriptTypes`; it does not install Pi
dependencies, edit that checkout or invoke its test runner. Diff formatting,
image encoding and process execution have separate tests and documented limits.
