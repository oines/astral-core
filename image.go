package astral

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"math"
	"net/http"

	_ "golang.org/x/image/bmp"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// ImageOptions bounds inline images. MaxBytes counts the base64 payload, like
// Pi's default 4.5 MiB limit. MaxPixels bounds decoding memory before allocation.
// Provider-specific limits can be supplied by the host; Core is provider-neutral.
type ImageOptions struct {
	DisableResize bool
	MaxWidth      int
	MaxHeight     int
	MaxBytes      int
	MaxPixels     int64
	MaxPatches    int
}

func (o *ImageOptions) defaults() error {
	if o.MaxWidth == 0 {
		o.MaxWidth = 2048
	}
	if o.MaxHeight == 0 {
		o.MaxHeight = 2048
	}
	if o.MaxBytes == 0 {
		o.MaxBytes = 9 * 1024 * 1024 / 2
	}
	if o.MaxPatches == 0 {
		o.MaxPatches = 2500
	}
	if o.MaxPixels == 0 {
		o.MaxPixels = 40_000_000
	}
	if o.MaxWidth < 1 || o.MaxHeight < 1 || o.MaxBytes < 128 || o.MaxPixels < 1 || o.MaxPatches < 1 {
		return fmt.Errorf("invalid image limits")
	}
	return nil
}

// ImageMediaType recognizes supported raster image formats.
func ImageMediaType(data []byte) string {
	switch mime := http.DetectContentType(data); mime {
	case "image/png", "image/jpeg", "image/gif", "image/webp", "image/bmp":
		return mime
	default:
		return ""
	}
}

// ProcessImage freezes a provider-neutral model image in memory. Call it once at
// admission, then persist the returned content for deterministic replay.
func ProcessImage(ctx context.Context, data []byte, options ImageOptions) (ToolResult, error) {
	if err := options.defaults(); err != nil {
		return ToolResult{}, err
	}
	return readImage(ctx, data, ImageMediaType(data), options)
}

// Validate checks image limits without changing the caller's options.
func (o ImageOptions) Validate() error { return o.defaults() }

func readImage(ctx context.Context, data []byte, mime string, options ImageOptions) (ToolResult, error) {
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return ToolResult{Content: Text("Read image file [" + mime + "]\n[Image omitted: could not decode image.]"), IsError: true}, nil
	}
	if config.Width < 1 || config.Height < 1 || int64(config.Width) > options.MaxPixels/int64(config.Height) {
		return ToolResult{Content: Text("[Image omitted: dimensions exceed the configured decode limit.]"), IsError: true}, nil
	}
	orientation := jpegOrientation(data)
	ow, oh := config.Width, config.Height
	if orientation >= 5 && orientation <= 8 {
		ow, oh = oh, ow
	}
	if orientation == 1 && mime != "image/bmp" && (options.DisableResize || ow <= options.MaxWidth && oh <= options.MaxHeight && imagePatches(ow, oh) <= options.MaxPatches && base64.StdEncoding.EncodedLen(len(data)) <= options.MaxBytes) {
		return imageResult(data, mime, ow, oh, ow, oh, ""), nil
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return ToolResult{}, fmt.Errorf("decode image: %w", err)
	}
	decoded = orientImage(decoded, orientation)
	w, h := ow, oh
	if !options.DisableResize {
		scale := math.Min(1, math.Min(float64(options.MaxWidth)/float64(w), float64(options.MaxHeight)/float64(h)))
		scale = math.Min(scale, math.Sqrt(float64(options.MaxPatches*32*32)/float64(w*h)))
		w, h = max(1, int(math.Floor(float64(w)*scale))), max(1, int(math.Floor(float64(h)*scale)))
		for imagePatches(w, h) > options.MaxPatches {
			scale *= 0.99
			w, h = max(1, int(float64(ow)*scale)), max(1, int(float64(oh)*scale))
		}
	}
	for {
		if err := ctx.Err(); err != nil {
			return ToolResult{}, err
		}
		canvas := image.NewNRGBA(image.Rect(0, 0, w, h))
		draw.CatmullRom.Scale(canvas, canvas.Bounds(), decoded, decoded.Bounds(), draw.Src, nil)
		var encoded bytes.Buffer
		if err := png.Encode(&encoded, canvas); err != nil {
			return ToolResult{}, err
		}
		if options.DisableResize || base64.StdEncoding.EncodedLen(encoded.Len()) < options.MaxBytes {
			return imageResult(encoded.Bytes(), "image/png", ow, oh, w, h, mime), nil
		}
		for _, quality := range []int{85, 80, 70, 55, 40} {
			if !canvas.Opaque() {
				break
			}
			encoded.Reset()
			if err := jpeg.Encode(&encoded, canvas, &jpeg.Options{Quality: quality}); err != nil {
				return ToolResult{}, err
			}
			if base64.StdEncoding.EncodedLen(encoded.Len()) < options.MaxBytes {
				return imageResult(encoded.Bytes(), "image/jpeg", ow, oh, w, h, mime), nil
			}
		}
		if w == 1 && h == 1 {
			break
		}
		w, h = max(1, w*3/4), max(1, h*3/4)
	}
	return ToolResult{Content: Text("[Image omitted: could not be resized below the inline image size limit.]"), IsError: true}, nil
}

func imageResult(data []byte, mime string, ow, oh, w, h int, from string) ToolResult {
	text := "Read image file [" + mime + "]"
	if from != "" && from != mime {
		text += fmt.Sprintf("\n[Image converted from %s to %s.]", from, mime)
	}
	if w != ow || h != oh {
		text += fmt.Sprintf("\n[Image: original %dx%d, displayed at %dx%d. Multiply coordinates by %.2f to map to original image.]", ow, oh, w, h, float64(ow)/float64(w))
	}
	encoded, _ := json.Marshal(base64.StdEncoding.EncodeToString(data))
	dimensions, _ := json.Marshal(map[string]int{"original_width": ow, "original_height": oh, "width": w, "height": h})
	return ToolResult{Content: []Content{{Type: "text", Text: text}, {Type: "image", MediaType: mime, Data: encoded, Metadata: dimensions}}}
}

// JPEG EXIF orientation is applied before resizing, avoiding rotated output and
// incorrect coordinate hints. Unrecognized/malformed metadata is ignored.
func jpegOrientation(data []byte) int {
	if len(data) < 4 || data[0] != 0xff || data[1] != 0xd8 {
		return 1
	}
	for pos := 2; pos+4 <= len(data); {
		if data[pos] != 0xff {
			break
		}
		marker := data[pos+1]
		if marker == 0xda || marker == 0xd9 {
			break
		}
		if marker == 0xff {
			pos++
			continue
		}
		length := int(binary.BigEndian.Uint16(data[pos+2 : pos+4]))
		if length < 2 || length > len(data)-pos-2 {
			break
		}
		payload := data[pos+4 : pos+2+length]
		if marker == 0xe1 && bytes.HasPrefix(payload, []byte("Exif\x00\x00")) {
			return tiffOrientation(payload[6:])
		}
		pos += length + 2
	}
	return 1
}

func tiffOrientation(data []byte) int {
	if len(data) < 8 {
		return 1
	}
	var order binary.ByteOrder
	switch string(data[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 1
	}
	if order.Uint16(data[2:4]) != 42 {
		return 1
	}
	pos := uint64(order.Uint32(data[4:8]))
	if pos+2 > uint64(len(data)) {
		return 1
	}
	count := int(order.Uint16(data[pos : pos+2]))
	pos += 2
	for i := 0; i < count && pos+12 <= uint64(len(data)); i, pos = i+1, pos+12 {
		field := data[pos : pos+12]
		if order.Uint16(field[:2]) == 0x112 && order.Uint16(field[2:4]) == 3 && order.Uint32(field[4:8]) == 1 {
			value := int(order.Uint16(field[8:10]))
			if value >= 1 && value <= 8 {
				return value
			}
		}
	}
	return 1
}

func orientImage(source image.Image, orientation int) image.Image {
	if orientation < 2 || orientation > 8 {
		return source
	}
	b := source.Bounds()
	w, h := b.Dx(), b.Dy()
	dw, dh := w, h
	if orientation >= 5 {
		dw, dh = h, w
	}
	out := image.NewNRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			dx, dy := x, y
			switch orientation {
			case 2:
				dx = w - 1 - x
			case 3:
				dx, dy = w-1-x, h-1-y
			case 4:
				dy = h - 1 - y
			case 5:
				dx, dy = y, x
			case 6:
				dx, dy = h-1-y, x
			case 7:
				dx, dy = h-1-y, w-1-x
			case 8:
				dx, dy = y, w-1-x
			}
			out.Set(dx, dy, source.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return out
}

func imagePatches(w, h int) int { return ((w + 31) / 32) * ((h + 31) / 32) }
