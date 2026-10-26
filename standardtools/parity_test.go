package standardtools

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

// Expected results were generated from Pi a4d040d, not from this implementation.
func TestPiEditFixtures(t *testing.T) {
	data, err := os.ReadFile("testdata/pi_edits.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name, Original, Expected, Error string
		Edits                           []Edit
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			got, err := applyEdits(c.Original, c.Edits, "fixture.txt")
			if c.Error != "" {
				if err == nil {
					t.Fatalf("expected rejection, got %q", got)
				}
				return
			}
			if err != nil || got != c.Expected {
				t.Fatalf("got %q, %v; want %q", got, err, c.Expected)
			}
		})
	}
}

func TestPiTruncateFixtures(t *testing.T) {
	data, err := os.ReadFile("testdata/pi_truncate.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name, Text string
		Expected   headTruncation
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			if got := truncateHead(c.Text); got != c.Expected {
				t.Fatalf("truncation differs: got lines=%d bytes=%d by=%s; expected lines=%d bytes=%d by=%s", got.OutputLines, got.OutputBytes, got.TruncatedBy, c.Expected.OutputLines, c.Expected.OutputBytes, c.Expected.TruncatedBy)
			}
		})
	}
}

func TestBoundedOutputRetainsHeadTailAndValidUTF8(t *testing.T) {
	for _, text := range []string{strings.Repeat("abcdef", 100), strings.Repeat("你好🪐", 100)} {
		b := headTailBuffer{limit: 64}
		for i := 0; i < len(text); i += 7 {
			b.append([]byte(text[i:min(i+7, len(text))]))
		}
		got, omitted := b.text()
		if !utf8.ValidString(got) || omitted == 0 || !strings.Contains(got, "bytes omitted") || !strings.HasPrefix(got, text[:6]) || !strings.HasSuffix(got, text[len(text)-6:]) {
			t.Fatalf("bad bounded output: %q", got)
		}
		if len(b.head)+len(b.tail) > 64 || b.total != int64(len(text)) {
			t.Fatal("buffer not bounded or count lost")
		}
		zero := limitCommandOutput(got, (b.total+3)/4, omitted, 0)
		if !strings.Contains(zero, "tokens truncated") || !strings.Contains(zero, "bytes omitted") {
			t.Fatal("zero output budget hid truncation or collection omissions")
		}
	}
}
