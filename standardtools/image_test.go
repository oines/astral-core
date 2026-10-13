package standardtools

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"image"
	"image/color"
	"image/gif"
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
			mime := imageMIME(encoded.Bytes())
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
