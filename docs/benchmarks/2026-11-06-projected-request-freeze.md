# Projected request freezing

The new non-visual image projection exposed an existing expensive fallback in request freezing. Projection itself borrowed immutable pixels, but selecting source messages indexed and canonicalized the full original history. Generic JSON diff then repeatedly encoded changed messages containing images that would never be sent.

The fix selects the original position first when the projected message retains its large fields at the corresponding positions, or in order when the content count changes. Reordered large images still use the existing source search. Content changes are planned before JSON encoding: removed parts need only removal records, a small changed content type encodes only its replacement, and ordered unchanged large parts remain anchors between changed spans. Small spans may be rewritten. There is no new cache, storage format, or weakened frozen-request digest check.

## Measurement

Apple M2, 24 GiB RAM, darwin/arm64, Go 1.26.3. Measurements run serially with the shared measurement lock. The same public synthetic seed contains 1,031 messages and 227 images with 18,610,802 encoded image bytes, approximately 900k mixed tokens with images enabled (899,581 in the saved seed; 899,597 after the first Host input). Its SHA-256 is `760ff61f76a91ffd8c84559dded23cbc26b74446ebccaaa79631f61cf39dbd60`. Non-visual projection produces 629,905 estimated tokens, no images, and an identical 2,702,649-byte HTTP body before and after this fix.

The before implementation is commit `21a7608`; the initial after samples below use the implementation in `3d5e717`. Later changes strengthen source-selection guards and retain ordered large anchors between several changed spans; those changes require their own final regression run. Each full-path sample creates a real SQLite Session, then measures `Prompt` through durable Run completion. The offline HTTP transport consumes the complete request body. This includes Core and SDK preparation, encoding and persistence; it excludes Astral Host, actual networking, provider inference and fixture initialization. CPU profiles are enabled in both versions. These numbers must not be presented as isolated projection cost or complete Host latency.

| Measurement | Before | After |
| --- | --- | --- |
| Full-path wall time, independent fresh Sessions | 1,851.1–1,871.8 ms, 3 samples | 25.55–26.18 ms, 4 samples |
| Full-path cumulative allocation | 1,044.4–1,047.4 MB | 16.60–16.96 MB |
| Isolated `FreezeRequest` wall time | 1,498.1 ms, initial profile | 4.96–5.09 ms |
| Isolated `FreezeRequest` cumulative allocation | 739,883,872 bytes, initial profile | 466,208 bytes |

MB in this table means 1,000,000 bytes. Cumulative allocation is not live heap or RSS. The old SDK rejected this non-visual image-bearing request; that rejection time is not a valid before measurement for this newly successful path. The before numbers above include the new image projection and precede only the freezing algorithm change.

The initial before full-path CPU profile sampled 1,650 ms: `prepareCandidate` accumulated 1,500 ms, `freezeRequest` 1,090 ms, `selectRequestMessages` 790 ms, and `diffPreparedRequest` 700 ms. Cumulative entries overlap and must not be added. `canonicalDigest` accumulated 1,070 ms across these paths. The observed bottleneck is repeated handling of original image content, rather than image projection or network activity.

Regression coverage includes same-role image swaps, unchanged large opaque fields and tool arguments, adjacent image collapse, nil/empty content, exact large-number and JSON behavior, source immutability, and randomized round trips. Normal supported-image Host latency, repeated live heap and storage growth are measured separately against the fixed previous release source; they are not inferred from this non-visual speedup.
