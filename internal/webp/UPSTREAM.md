# Encoder provenance

This package uses the mature libwebp encoder, not a new codec or an application
quality-search implementation. It exposes static-image encoding only. Importing
the package neither registers a decoder nor loads native shared libraries.

Upstream wrapper: https://github.com/gen2brain/webp/tree/v0.6.4

- Module version: `github.com/gen2brain/webp v0.6.4`.
- Git commit: `3dcf526886104be4bfe266de93ebcd22923bcc70`.
- Underlying codec: libwebp `v1.6.0`, selected by upstream `lib/Makefile`.
- Wrapper license: `LICENSE.gen2brain-webp` (MIT, copied verbatim).
- Codec license: `LICENSE.libwebp` (BSD, copied verbatim).

Unmodified upstream files and their SHA-256 hashes:

| Local file | Upstream file | SHA-256 |
| --- | --- | --- |
| `libwebp.go` | `libwebp.go` | `854dffeb8d261bc0c7925510f3a375beb986e9ad6319c34394168f0e9156fa18` |
| `lib/webp.c` | `lib/webp.c` | `309229e7f527e84e030cdccc10aeaa7a28f384aec48a8b7c8f083973699a38e1` |
| `lib/Makefile` | `lib/Makefile` | `e94c3199ce6eb556d8e3cc877130ad454d1dbc8732ae9e58ab18a11ce291e7de` |

`libwebp.go` is the complete, unmodified upstream wasm2go-generated engine
(2,693,605 bytes), including code that is not exposed by this package. Do not edit
its generated codec functions manually.

`encode.go` extracts the static encoder and memory/WASI adapters from upstream
`webp_wasm2go.go` (lines 185–353) and the necessary image conversion from
`webp.go`. Local integration differences are limited to:

- One `Encode(io.Writer, image.Image, int) error` entry point, method 4, lossy
  encoding, quality 0–100, and alpha preservation.
- `MaxDimension` exposes libwebp's hard format limit (`WEBP_MAX_DIMENSION`,
  16383 pixels) so callers can honor it without duplicating the constant.
- Packed straight-alpha RGBA input, normalizing strided subimages and
  premultiplied pixels with Go's standard `image/draw` implementation.
- Reading the output `size_t` as wasm32's four-byte value; checking short writes.
- Omitting decoder APIs, animation APIs, EXIF handling, dynamic loading and all
  global decoder registration. No extra Go module dependency is required.

## Generation and updates

The unmodified `lib/webp.c` and `lib/Makefile` preserve upstream's build recipe:

1. Install a WASI SDK, CMake, Make and `github.com/ncruces/wasm2go`'s generator.
2. Run `cd internal/webp/lib && make WASI_SDK_PATH=/path/to/wasi-sdk`.
3. The recipe fetches libwebp `v1.6.0`, disables native threads/SIMD, builds the
   C wrapper as a wasm32 reactor, then runs
   `wasm2go -pkg webp -unsafe -o ../libwebp.go webp.wasm` and upstream's private-name
   substitutions.

Upstream does not pin the WASI SDK, CMake or wasm2go versions in this recipe, so
rebuilding with arbitrary tool versions is not a claim of byte-identical output.
The pinned upstream generated file and hash above are the source of truth for
this import. When updating, record the new upstream commit, codec/generator/tool
versions, hashes and licenses, and run functional/native-platform and affected
image-path performance checks. Ordinary Go builds use the checked-in engine and
do not require CGo, WASI SDK, CMake or native libwebp.
