package webp

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"io"
	"testing"

	decoder "golang.org/x/image/webp"
)

func TestEncodePreservesAlphaAndDimensions(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 24, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 24; x++ {
			alpha := uint8((x / 6) * 85)
			src.SetNRGBA(x, y, color.NRGBA{R: 210, G: 70, B: 30, A: alpha})
		}
	}
	before := bytes.Clone(src.Pix)
	var encoded bytes.Buffer
	if err := Encode(&encoded, src, 75); err != nil {
		t.Fatal(err)
	}
	decoded, err := decoder.Decode(bytes.NewReader(encoded.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Bounds() != src.Bounds() {
		t.Fatalf("bounds = %v, want %v", decoded.Bounds(), src.Bounds())
	}
	for y := 0; y < 16; y++ {
		for x := 0; x < 24; x++ {
			_, _, _, got := decoded.At(x, y).RGBA()
			_, _, _, want := src.At(x, y).RGBA()
			if got != want {
				t.Fatalf("alpha at (%d,%d) = %d, want %d", x, y, got, want)
			}
		}
	}
	if !bytes.Equal(src.Pix, before) {
		t.Fatal("Encode modified the source pixels")
	}
}

func TestEncodeStridedSubimageMatchesPackedImage(t *testing.T) {
	for _, premultiplied := range []bool{false, true} {
		t.Run(map[bool]string{false: "NRGBA", true: "RGBA"}[premultiplied], func(t *testing.T) {
			bounds := image.Rect(-7, 5, 33, 35)
			var parent interface {
				image.Image
				Set(int, int, color.Color)
				SubImage(image.Rectangle) image.Image
			}
			if premultiplied {
				parent = image.NewRGBA(bounds)
			} else {
				parent = image.NewNRGBA(bounds)
			}
			for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
				for x := bounds.Min.X; x < bounds.Max.X; x++ {
					parent.Set(x, y, color.NRGBA{R: uint8(x * 13), G: uint8(y * 7), B: 80, A: uint8(100 + y)})
				}
			}
			crop := image.Rect(-2, 11, 22, 27)
			src := parent.SubImage(crop)
			packed := image.NewNRGBA(image.Rect(0, 0, crop.Dx(), crop.Dy()))
			for y := 0; y < crop.Dy(); y++ {
				for x := 0; x < crop.Dx(); x++ {
					packed.Set(x, y, src.At(crop.Min.X+x, crop.Min.Y+y))
				}
			}
			var actual, expected, repeated bytes.Buffer
			if err := Encode(&actual, src, 65); err != nil {
				t.Fatal(err)
			}
			if err := Encode(&expected, packed, 65); err != nil {
				t.Fatal(err)
			}
			if err := Encode(&repeated, src, 65); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(actual.Bytes(), expected.Bytes()) {
				t.Fatal("strided/subimage encoding differs from the same packed pixels")
			}
			if !bytes.Equal(actual.Bytes(), repeated.Bytes()) {
				t.Fatal("identical inputs produced different bytes")
			}
		})
	}
}

func TestEncodeRejectsInvalidInputAndReturnsWriterErrors(t *testing.T) {
	valid := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	for _, test := range []struct {
		name    string
		image   image.Image
		quality int
	}{
		{"nil", nil, 75},
		{"empty", image.NewNRGBA(image.Rectangle{}), 75},
		{"too-wide", image.NewUniform(color.Black), 75},
		{"quality-negative", valid, -1},
		{"quality-too-large", valid, 101},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			if err := Encode(&output, test.image, test.quality); err == nil {
				t.Fatal("Encode accepted invalid input")
			}
			if output.Len() != 0 {
				t.Fatal("invalid input wrote output")
			}
		})
	}
	want := errors.New("test writer failed")
	if err := Encode(failedWriter{want}, valid, 0); !errors.Is(err, want) {
		t.Fatalf("writer error = %v, want %v", err, want)
	}
	if err := Encode(shortWriter{}, valid, 100); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short writer error = %v, want %v", err, io.ErrShortWrite)
	}
}

type failedWriter struct{ err error }

func (w failedWriter) Write([]byte) (int, error) { return 0, w.err }

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }
