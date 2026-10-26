package astral

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"testing"

	"golang.org/x/image/bmp"
)

func TestImageFormatsAndDecodeLimit(t *testing.T) {
	options := ImageOptions{}
	if err := options.defaults(); err != nil {
		t.Fatal(err)
	}
	source := image.NewRGBA(image.Rect(0, 0, 4, 2))
	source.Set(0, 0, color.White)
	for _, format := range []string{"gif", "bmp"} {
		t.Run(format, func(t *testing.T) {
			var encoded bytes.Buffer
			var err error
			if format == "gif" {
				err = gif.Encode(&encoded, source, nil)
			} else {
				err = bmp.Encode(&encoded, source)
			}
			if err != nil {
				t.Fatal(err)
			}
			mime := ImageMediaType(encoded.Bytes())
			if mime != "image/"+format {
				t.Fatalf("MIME %q", mime)
			}
			result, err := readImage(context.Background(), encoded.Bytes(), mime, options)
			if err != nil || result.IsError || len(result.Content) != 2 {
				t.Fatalf("format failed: %+v %v", result, err)
			}
			if format == "bmp" && result.Content[1].MediaType != "image/png" {
				t.Fatal("BMP not converted to portable image")
			}
			var base64 string
			if err := json.Unmarshal(result.Content[1].Data, &base64); err != nil || base64 == "" {
				t.Fatal("missing image payload")
			}
			limited := options
			limited.MaxPixels = 1
			result, err = readImage(context.Background(), encoded.Bytes(), mime, limited)
			if err != nil || !result.IsError {
				t.Fatal("decode pixel limit ignored")
			}
		})
	}
	result, err := readImage(context.Background(), []byte("invalid"), "image/png", options)
	if err != nil || !result.IsError {
		t.Fatal("corrupt image was silently returned")
	}
}

func TestJPEGOrientationBeforeResize(t *testing.T) {
	// A minimal valid TIFF orientation field, embedded in a JPEG APP1 segment.
	tiff := make([]byte, 26)
	copy(tiff, "II")
	binary.LittleEndian.PutUint16(tiff[2:], 42)
	binary.LittleEndian.PutUint32(tiff[4:], 8)
	binary.LittleEndian.PutUint16(tiff[8:], 1)
	binary.LittleEndian.PutUint16(tiff[10:], 0x112)
	binary.LittleEndian.PutUint16(tiff[12:], 3)
	binary.LittleEndian.PutUint32(tiff[14:], 1)
	binary.LittleEndian.PutUint16(tiff[18:], 6)
	data := []byte{0xff, 0xd8, 0xff, 0xe1, 0, 34}
	data = append(data, []byte("Exif\x00\x00")...)
	data = append(data, tiff...)
	if got := jpegOrientation(data); got != 6 {
		t.Fatalf("orientation=%d", got)
	}
	source := image.NewNRGBA(image.Rect(0, 0, 2, 3))
	source.SetNRGBA(0, 0, color.NRGBA{R: 255, A: 255})
	rotated := orientImage(source, 6)
	if rotated.Bounds().Dx() != 3 || rotated.Bounds().Dy() != 2 {
		t.Fatal("orientation dimensions not rotated")
	}
	r, _, _, _ := rotated.At(2, 0).RGBA()
	if r != 65535 {
		t.Fatal("orientation pixels not rotated")
	}
}

func imagePixels(t *testing.T, result ToolResult) image.Image {
	t.Helper()
	if result.IsError || len(result.Content) != 2 {
		t.Fatalf("image failed: %+v", result)
	}
	var text string
	if err := json.Unmarshal(result.Content[1].Data, &text); err != nil {
		t.Fatal(err)
	}
	data, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		t.Fatal(err)
	}
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}
func TestProcessImagePatchAndPayloadLimitsKeepTransparency(t *testing.T) {
	for _, tt := range []struct {
		name    string
		size    int
		options ImageOptions
	}{
		{"patch limit", 2048, ImageOptions{}},
		{"base64 limit", 256, ImageOptions{MaxBytes: 4096}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			source := image.NewNRGBA(image.Rect(0, 0, tt.size, tt.size))
			for y := 0; y < tt.size; y++ {
				for x := 0; x < tt.size; x++ {
					source.SetNRGBA(x, y, color.NRGBA{R: uint8(x*37 + y*19), G: uint8(x*11 + y*53), B: uint8(x*23 + y*7), A: 128})
				}
			}
			var raw bytes.Buffer
			if err := png.Encode(&raw, source); err != nil {
				t.Fatal(err)
			}
			result, err := ProcessImage(context.Background(), raw.Bytes(), tt.options)
			if err != nil {
				t.Fatal(err)
			}
			got := imagePixels(t, result)
			bounds := got.Bounds()
			if bounds.Dx() > 2048 || bounds.Dy() > 2048 || ((bounds.Dx()+31)/32)*((bounds.Dy()+31)/32) > 2500 {
				t.Fatalf("image budget exceeded: %v", bounds)
			}
			if _, _, _, alpha := got.At(0, 0).RGBA(); alpha == 65535 || alpha == 0 {
				t.Fatal("alpha was flattened")
			}
			var encoded string
			_ = json.Unmarshal(result.Content[1].Data, &encoded)
			limit := tt.options.MaxBytes
			if limit == 0 {
				limit = 9 * 1024 * 1024 / 2
			}
			if len(encoded) > limit {
				t.Fatalf("base64 budget exceeded: %d", len(encoded))
			}
			var dimensions map[string]int
			_ = json.Unmarshal(result.Content[1].Metadata, &dimensions)
			if dimensions["original_width"] != tt.size || dimensions["width"] != bounds.Dx() {
				t.Fatalf("dimensions not retained: %v", dimensions)
			}
		})
	}
}
func TestProcessImageAppliesEXIFEvenBelowResizeThreshold(t *testing.T) {
	var raw bytes.Buffer
	if err := jpeg.Encode(&raw, image.NewRGBA(image.Rect(0, 0, 80, 40)), nil); err != nil {
		t.Fatal(err)
	}
	tiff := make([]byte, 26)
	copy(tiff, "II")
	binary.LittleEndian.PutUint16(tiff[2:], 42)
	binary.LittleEndian.PutUint32(tiff[4:], 8)
	binary.LittleEndian.PutUint16(tiff[8:], 1)
	binary.LittleEndian.PutUint16(tiff[10:], 0x112)
	binary.LittleEndian.PutUint16(tiff[12:], 3)
	binary.LittleEndian.PutUint32(tiff[14:], 1)
	binary.LittleEndian.PutUint16(tiff[18:], 6)
	data := append([]byte{0xff, 0xd8, 0xff, 0xe1, 0, 34}, []byte("Exif\x00\x00")...)
	data = append(data, tiff...)
	data = append(data, raw.Bytes()[2:]...)
	result, err := ProcessImage(context.Background(), data, ImageOptions{})
	if err != nil {
		t.Fatal(err)
	}
	bounds := imagePixels(t, result).Bounds()
	if bounds.Dx() != 40 || bounds.Dy() != 80 {
		t.Fatalf("small EXIF image not oriented: %v", bounds)
	}
}
