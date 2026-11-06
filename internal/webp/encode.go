// Package webp encodes static WebP images using the libwebp 1.6.0 engine.
// It does not register an image decoder or load native shared libraries.
package webp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"io"
)

// MaxDimension is libwebp's maximum supported width or height, in pixels.
const MaxDimension = 16383

// Encode writes a lossy WebP image at quality 0–100, using libwebp method 4.
// Alpha is preserved. Pixels hidden by fully transparent alpha need not retain
// their original RGB values. The input image is not modified.
func Encode(w io.Writer, src image.Image, quality int) error {
	if quality < 0 || quality > 100 {
		return fmt.Errorf("webp: quality %d is outside 0–100", quality)
	}
	if src == nil {
		return errors.New("webp: nil image")
	}
	b := src.Bounds()
	width, height := b.Dx(), b.Dy()
	// The bitstream's maximum dimension is WEBP_MAX_DIMENSION (16383).
	if width <= 0 || height <= 0 || width > MaxDimension || height > MaxDimension {
		return fmt.Errorf("webp: unsupported dimensions %dx%d", width, height)
	}

	// The C binding consumes packed, straight-alpha RGBA. Normalize strides,
	// subimages and premultiplied RGBA through the standard image/draw package.
	pixels, ok := src.(*image.NRGBA)
	if !ok || pixels.Stride != width*4 {
		pixels = image.NewNRGBA(image.Rect(0, 0, width, height))
		draw.Draw(pixels, pixels.Bounds(), src, b.Min, draw.Src)
	}
	data := pixels.Pix[:width*height*4]

	mod := newModuleRaw(&wasiHost{})
	mod.X_initialize()
	inPtr := mod.Xmalloc(int32(len(data)))
	defer mod.Xfree(inPtr)
	if !mod.write(inPtr, data) {
		return errors.New("webp: input memory write failed")
	}

	// size_t in this wasm32 engine is four bytes.
	sizePtr := mod.Xmalloc(4)
	defer mod.Xfree(sizePtr)
	outPtr := mod.Xencode(inPtr, int32(width), int32(height), sizePtr,
		0, int32(quality), 4, 0, 0)
	defer mod.Xfree(outPtr)
	size, ok := mod.readUint32(sizePtr)
	if !ok {
		return errors.New("webp: output size read failed")
	}
	if size == 0 {
		return errors.New("webp: encode failed")
	}
	out, ok := mod.read(outPtr, int32(size))
	if !ok {
		return errors.New("webp: output memory read failed")
	}
	n, err := w.Write(out)
	if err != nil {
		return fmt.Errorf("webp: write: %w", err)
	}
	if n != len(out) {
		return io.ErrShortWrite
	}
	return nil
}

// The memory and WASI adapters below are extracted from gen2brain/webp v0.6.4.
// See UPSTREAM.md for provenance and the unchanged generated engine's hash.
func (m *module) write(ptr int32, data []byte) bool {
	if ptr < 0 || int(ptr)+len(data) > len(m.memory) {
		return false
	}
	copy(m.memory[ptr:], data)
	return true
}

func (m *module) read(ptr, size int32) ([]byte, bool) {
	if ptr < 0 || size < 0 || int(ptr)+int(size) > len(m.memory) {
		return nil, false
	}
	return m.memory[ptr : ptr+size : ptr+size], true
}

func (m *module) readUint32(ptr int32) (uint32, bool) {
	if ptr < 0 || int(ptr)+4 > len(m.memory) {
		return 0, false
	}
	return binary.LittleEndian.Uint32(m.memory[ptr:]), true
}

type wasiHost struct {
	mod *module
}

func (h *wasiHost) Init(m any) {
	h.mod = m.(*module)
}

func (h *wasiHost) Xfd_close(int32) int32 {
	return 0
}

// libwebpmux diagnostics are discarded, as in the upstream adapter.
func (h *wasiHost) Xfd_write(fd, iovs, iovsLen, nwrittenPtr int32) int32 {
	mem := h.mod.memory
	var written uint32
	for i := int32(0); i < iovsLen; i++ {
		written += load32(mem[iovs+i*8+4:])
	}
	store32(mem[nwrittenPtr:], written)
	return 0
}
