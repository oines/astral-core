package astral

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
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
			if format == "bmp" && result.Content[1].MediaType != "image/webp" {
				t.Fatal("BMP not converted to WebP")
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
	data := imageBytes(t, result)
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}

func imageBytes(t *testing.T, result ToolResult) []byte {
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
	return data
}

func TestProcessImagePatchAndPayloadLimitsKeepTransparency(t *testing.T) {
	for _, tt := range []struct {
		name    string
		size    int
		options ImageOptions
		noise   bool
	}{
		{"explicit patch limit", 2048, ImageOptions{MaxPatches: 2500, MaxBytes: 9 * 1024 * 1024 / 2}, false},
		{"base64 limit", 256, ImageOptions{MaxBytes: 4096}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			source := image.NewNRGBA(image.Rect(0, 0, tt.size, tt.size))
			for y := 0; y < tt.size; y++ {
				for x := 0; x < tt.size; x++ {
					pixel := color.NRGBA{R: 90, G: 60, B: 30, A: 128}
					if tt.noise {
						pixel = color.NRGBA{R: uint8(x*37 + y*19), G: uint8(x*11 + y*53), B: uint8(x*23 + y*7), A: 128}
					}
					source.SetNRGBA(x, y, pixel)
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
			if result.Content[1].MediaType != "image/webp" || ImageMediaType(imageBytes(t, result)) != "image/webp" {
				t.Fatal("reencoded image is not WebP")
			}
			bounds := got.Bounds()
			if tt.options.MaxPatches > 0 && ((bounds.Dx()+31)/32)*((bounds.Dy()+31)/32) > tt.options.MaxPatches {
				t.Fatalf("explicit patch budget exceeded: %v", bounds)
			}
			if _, _, _, alpha := got.At(0, 0).RGBA(); alpha == 65535 || alpha == 0 {
				t.Fatal("alpha was flattened")
			}
			var encoded string
			_ = json.Unmarshal(result.Content[1].Data, &encoded)
			limit := tt.options.MaxBytes
			if limit == 0 {
				limit = 150_000
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

func jpegWithOrientation(raw []byte, orientation int) []byte {
	tiff := make([]byte, 26)
	copy(tiff, "II")
	binary.LittleEndian.PutUint16(tiff[2:], 42)
	binary.LittleEndian.PutUint32(tiff[4:], 8)
	binary.LittleEndian.PutUint16(tiff[8:], 1)
	binary.LittleEndian.PutUint16(tiff[10:], 0x112)
	binary.LittleEndian.PutUint16(tiff[12:], 3)
	binary.LittleEndian.PutUint32(tiff[14:], 1)
	binary.LittleEndian.PutUint16(tiff[18:], uint16(orientation))
	data := append([]byte{0xff, 0xd8, 0xff, 0xe1, 0, 34}, []byte("Exif\x00\x00")...)
	data = append(data, tiff...)
	return append(data, raw[2:]...)
}

func TestProcessImageAppliesAllEXIFOrientationsBelowResizeThreshold(t *testing.T) {
	near := func(a, b uint8) bool { delta := int(a) - int(b); return delta >= -40 && delta <= 40 }
	colors := []color.NRGBA{{R: 255, A: 255}, {G: 255, A: 255}, {B: 255, A: 255}, {R: 255, G: 255, A: 255}}
	source := image.NewNRGBA(image.Rect(0, 0, 80, 48))
	for y := 0; y < 48; y++ {
		for x := 0; x < 80; x++ {
			source.SetNRGBA(x, y, colors[(y/24)*2+x/40])
		}
	}
	var raw bytes.Buffer
	if err := jpeg.Encode(&raw, source, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	// Independently specified source quadrants at output TL/TR/BL/BR.
	quadrants := [8][4]int{{0, 1, 2, 3}, {1, 0, 3, 2}, {3, 2, 1, 0}, {2, 3, 0, 1}, {0, 2, 1, 3}, {2, 0, 3, 1}, {3, 1, 2, 0}, {1, 3, 0, 2}}
	for orientation := 1; orientation <= 8; orientation++ {
		t.Run(fmt.Sprint(orientation), func(t *testing.T) {
			data := jpegWithOrientation(raw.Bytes(), orientation)
			result, err := ProcessImage(context.Background(), data, ImageOptions{})
			if err != nil {
				t.Fatal(err)
			}
			w, h := 80, 48
			if orientation >= 5 {
				w, h = h, w
			}
			got := imagePixels(t, result)
			if got.Bounds().Dx() != w || got.Bounds().Dy() != h {
				t.Fatalf("EXIF dimensions: %v, want %dx%d", got.Bounds(), w, h)
			}
			if orientation == 1 {
				if !bytes.Equal(imageBytes(t, result), data) || result.Content[1].MediaType != "image/jpeg" {
					t.Fatal("unrotated compliant JPEG was reencoded")
				}
			} else if result.Content[1].MediaType != "image/webp" {
				t.Fatal("oriented JPEG was not reencoded as WebP")
			}
			for i, quadrant := range quadrants[orientation-1] {
				pixel := color.NRGBAModel.Convert(got.At((1+2*(i%2))*w/4, (1+2*(i/2))*h/4)).(color.NRGBA)
				want := colors[quadrant]
				if !near(pixel.R, want.R) || !near(pixel.G, want.G) || !near(pixel.B, want.B) || pixel.A != 255 {
					t.Fatalf("quadrant %d: got %v want near %v", i, pixel, want)
				}
			}
			var dimensions map[string]int
			if err := json.Unmarshal(result.Content[1].Metadata, &dimensions); err != nil {
				t.Fatal(err)
			}
			if dimensions["original_width"] != w || dimensions["original_height"] != h || dimensions["width"] != w || dimensions["height"] != h {
				t.Fatalf("EXIF dimensions metadata: %v", dimensions)
			}
		})
	}
}

func TestProcessImageRetainsCompliantOriginalBytes(t *testing.T) {
	source := image.NewNRGBA(image.Rect(0, 0, 24, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 24; x++ {
			source.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 9), B: uint8(y * 13), A: 255})
		}
	}
	inputs := map[string][]byte{}
	for _, format := range []string{"png", "jpeg", "gif"} {
		var raw bytes.Buffer
		var err error
		switch format {
		case "png":
			err = png.Encode(&raw, source)
		case "jpeg":
			err = jpeg.Encode(&raw, source, nil)
		case "gif":
			err = gif.Encode(&raw, source, nil)
		}
		if err != nil {
			t.Fatal(err)
		}
		inputs[format] = append([]byte(nil), raw.Bytes()...)
	}
	// Generate a valid WebP, then test the distinct original-byte fast path.
	var bmpData bytes.Buffer
	if err := bmp.Encode(&bmpData, source); err != nil {
		t.Fatal(err)
	}
	webp, err := ProcessImage(context.Background(), bmpData.Bytes(), ImageOptions{})
	if err != nil {
		t.Fatal(err)
	}
	inputs["webp"] = imageBytes(t, webp)
	for format, input := range inputs {
		t.Run(format, func(t *testing.T) {
			before := append([]byte(nil), input...)
			result, err := ProcessImage(context.Background(), input, ImageOptions{Quality: 1})
			if err != nil {
				t.Fatal(err)
			}
			if result.Content[1].MediaType != "image/"+format || !bytes.Equal(imageBytes(t, result), input) || !bytes.Equal(input, before) {
				t.Fatal("compliant original image was changed")
			}
			if strings.Contains(result.Content[0].Text, "converted") || strings.Contains(result.Content[0].Text, "displayed") {
				t.Fatal("unchanged image reported a conversion or resize")
			}
		})
	}
}

func TestProcessImageWebPMetadataAndQuality(t *testing.T) {
	source := image.NewNRGBA(image.Rect(0, 0, 96, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 96; x++ {
			source.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 37), G: uint8(y * 53), B: uint8(x*23 + y*7), A: 255})
		}
	}
	var raw bytes.Buffer
	if err := png.Encode(&raw, source); err != nil {
		t.Fatal(err)
	}
	var encoded [][]byte
	for _, quality := range []int{1, 100} {
		result, err := ProcessImage(context.Background(), raw.Bytes(), ImageOptions{MaxWidth: 48, MaxHeight: 48, Quality: quality})
		if err != nil {
			t.Fatal(err)
		}
		data := imageBytes(t, result)
		config, format, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil || format != "webp" || config.Width != 48 || config.Height != 32 || result.Content[1].MediaType != "image/webp" {
			t.Fatalf("WebP output: %s %+v %v", format, config, err)
		}
		var dimensions map[string]int
		if err := json.Unmarshal(result.Content[1].Metadata, &dimensions); err != nil {
			t.Fatal(err)
		}
		if dimensions["original_width"] != 96 || dimensions["original_height"] != 64 || dimensions["width"] != 48 || dimensions["height"] != 32 {
			t.Fatalf("resize metadata: %v", dimensions)
		}
		if !strings.Contains(result.Content[0].Text, "converted from image/png to image/webp") || !strings.Contains(result.Content[0].Text, "displayed at 48x32") {
			t.Fatalf("conversion text: %s", result.Content[0].Text)
		}
		encoded = append(encoded, data)
	}
	if bytes.Equal(encoded[0], encoded[1]) {
		t.Fatal("explicit quality did not affect WebP encoding")
	}
}

func TestProcessImageExplicitDimensionLimitsAreIndependent(t *testing.T) {
	var raw bytes.Buffer
	if err := png.Encode(&raw, image.NewNRGBA(image.Rect(0, 0, 64, 48))); err != nil {
		t.Fatal(err)
	}
	for name, options := range map[string]ImageOptions{
		"width only":  {MaxWidth: 32},
		"height only": {MaxHeight: 24},
	} {
		t.Run(name, func(t *testing.T) {
			result, err := ProcessImage(context.Background(), raw.Bytes(), options)
			if err != nil {
				t.Fatal(err)
			}
			got := imagePixels(t, result).Bounds()
			if got.Dx() != 32 || got.Dy() != 24 {
				t.Fatalf("independent dimension limit: %v", got)
			}
		})
	}
}

func TestImageOptionsDefaultsAndValidation(t *testing.T) {
	options := ImageOptions{}
	if err := options.Validate(); err != nil || options.Quality != 0 {
		t.Fatalf("Validate mutated options or rejected defaults: %+v %v", options, err)
	}
	if err := options.defaults(); err != nil || options.Quality != 75 || options.MaxBytes != 150_000 || options.MaxPixels != 40_000_000 || options.MaxWidth != 0 || options.MaxHeight != 0 || options.MaxPatches != 0 {
		t.Fatalf("image defaults: %+v %v", options, err)
	}
	for _, quality := range []int{-1, 101} {
		if err := (ImageOptions{Quality: quality}).Validate(); err == nil {
			t.Fatalf("invalid quality accepted: %d", quality)
		}
	}
	for _, quality := range []int{1, 100} {
		if err := (ImageOptions{Quality: quality}).Validate(); err != nil {
			t.Fatalf("quality endpoint rejected: %d %v", quality, err)
		}
	}
	for name, options := range map[string]ImageOptions{
		"negative width":   {MaxWidth: -1},
		"negative height":  {MaxHeight: -1},
		"negative patches": {MaxPatches: -1},
		"negative pixels":  {MaxPixels: -1},
		"small byte limit": {MaxBytes: 127},
	} {
		t.Run(name, func(t *testing.T) {
			if err := options.Validate(); err == nil {
				t.Fatal("invalid image bounds accepted")
			}
		})
	}
}

func TestProcessImageDefaultRetainsLongCompliantOriginal(t *testing.T) {
	// Only 96k pixels, but wider than the former implicit dimension/patch limits
	// and WebP's format limit. A compliant PNG needs no WebP conversion.
	var raw bytes.Buffer
	if err := png.Encode(&raw, image.NewNRGBA(image.Rect(0, 0, 96_000, 1))); err != nil {
		t.Fatal(err)
	}
	if base64.StdEncoding.EncodedLen(raw.Len()) > 150_000 {
		t.Fatal("fixture must fit the default byte limit")
	}
	result, err := ProcessImage(context.Background(), raw.Bytes(), ImageOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Content[1].MediaType != "image/png" || !bytes.Equal(imageBytes(t, result), raw.Bytes()) {
		t.Fatal("default image processing resized a compliant original")
	}
}

func TestProcessImageKeepsOriginalDimensionsWhenCompressionFits(t *testing.T) {
	source := image.NewNRGBA(image.Rect(0, 0, 200, 200))
	for y := 0; y < 200; y++ {
		for x := 0; x < 200; x++ {
			source.SetNRGBA(x, y, color.NRGBA{R: 80, G: 120, B: 160, A: 128})
		}
	}
	var raw bytes.Buffer
	encoder := png.Encoder{CompressionLevel: png.NoCompression}
	if err := encoder.Encode(&raw, source); err != nil {
		t.Fatal(err)
	}
	if base64.StdEncoding.EncodedLen(raw.Len()) <= 150_000 {
		t.Fatal("fixture must exceed the default byte limit")
	}
	result, err := ProcessImage(context.Background(), raw.Bytes(), ImageOptions{})
	if err != nil {
		t.Fatal(err)
	}
	decoded := imagePixels(t, result)
	if decoded.Bounds().Dx() != 200 || decoded.Bounds().Dy() != 200 || result.Content[1].MediaType != "image/webp" {
		t.Fatalf("compression unnecessarily changed dimensions: %v %s", decoded.Bounds(), result.Content[1].MediaType)
	}
	var encoded string
	if err := json.Unmarshal(result.Content[1].Data, &encoded); err != nil || len(encoded) > 150_000 {
		t.Fatalf("default base64 limit exceeded: %d %v", len(encoded), err)
	}
	if strings.Contains(result.Content[0].Text, "displayed at") {
		t.Fatal("same-size compression reported a resize")
	}
}

func TestProcessImageReencodingHonorsWebPDimensionLimit(t *testing.T) {
	// A thin BMP is cheap to decode but requires conversion to a supported image.
	// Its 17k pixels are below MaxPixels; WebP's side limit still needs a fallback.
	var raw bytes.Buffer
	if err := bmp.Encode(&raw, image.NewNRGBA(image.Rect(0, 0, 17_000, 1))); err != nil {
		t.Fatal(err)
	}
	result, err := ProcessImage(context.Background(), raw.Bytes(), ImageOptions{})
	if err != nil {
		t.Fatal(err)
	}
	decoded := imagePixels(t, result)
	if decoded.Bounds().Dx() > 16383 || decoded.Bounds().Dy() != 1 || result.Content[1].MediaType != "image/webp" {
		t.Fatalf("WebP dimension fallback failed: %v", decoded.Bounds())
	}
	var dimensions map[string]int
	if err := json.Unmarshal(result.Content[1].Metadata, &dimensions); err != nil {
		t.Fatal(err)
	}
	if dimensions["original_width"] != 17_000 || dimensions["original_height"] != 1 || dimensions["width"] != decoded.Bounds().Dx() {
		t.Fatalf("format fallback lost original dimensions: %v", dimensions)
	}
}

func TestProcessImageDisableResizeStillChecksDecodeLimit(t *testing.T) {
	source := image.NewNRGBA(image.Rect(0, 0, 96, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 96; x++ {
			source.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 37), G: uint8(y * 53), B: uint8(x*23 + y*7), A: 255})
		}
	}
	for _, format := range []string{"png", "bmp"} {
		t.Run(format, func(t *testing.T) {
			var raw bytes.Buffer
			var err error
			if format == "png" {
				err = png.Encode(&raw, source)
			} else {
				err = bmp.Encode(&raw, source)
			}
			if err != nil {
				t.Fatal(err)
			}
			options := ImageOptions{DisableResize: true, MaxWidth: 1, MaxHeight: 1, MaxPatches: 1, MaxBytes: 128}
			result, err := ProcessImage(context.Background(), raw.Bytes(), options)
			if err != nil {
				t.Fatal(err)
			}
			pixels := imagePixels(t, result)
			if pixels.Bounds().Dx() != 96 || pixels.Bounds().Dy() != 64 {
				t.Fatalf("DisableResize changed dimensions: %v", pixels.Bounds())
			}
			if format == "png" && !bytes.Equal(imageBytes(t, result), raw.Bytes()) {
				t.Fatal("DisableResize changed portable original encoding")
			}
			if format == "bmp" && result.Content[1].MediaType != "image/webp" {
				t.Fatal("DisableResize did not convert BMP to WebP")
			}
			options.MaxPixels = 1
			limited, err := ProcessImage(context.Background(), raw.Bytes(), options)
			if err != nil || !limited.IsError || !strings.Contains(limited.Content[0].Text, "decode limit") {
				t.Fatalf("DisableResize bypassed decode limit: %+v %v", limited, err)
			}
		})
	}
}

func TestProcessImageCancelledBeforeConversion(t *testing.T) {
	var raw bytes.Buffer
	if err := png.Encode(&raw, image.NewNRGBA(image.Rect(0, 0, 16, 8))); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ProcessImage(ctx, raw.Bytes(), ImageOptions{MaxWidth: 8, MaxHeight: 8}); !errors.Is(err, context.Canceled) {
		t.Fatalf("image conversion ignored cancellation: %v", err)
	}
}
