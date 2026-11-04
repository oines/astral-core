package astral

import (
	"bytes"
	"math/rand"
	"testing"
)

func scalarSimpleRequestJSONString(raw []byte) bool {
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return false
	}
	for _, b := range raw[1 : len(raw)-1] {
		if b < 0x20 || b >= 0x80 || b == '"' || b == '\\' || b == '<' || b == '>' || b == '&' {
			return false
		}
	}
	return true
}

func TestSimpleRequestJSONStringMatchesScalar(t *testing.T) {
	check := func(raw []byte) {
		t.Helper()
		if got, want := simpleRequestJSONString(raw), scalarSimpleRequestJSONString(raw); got != want {
			t.Fatalf("%x: got %v want %v", raw, got, want)
		}
		if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
			if got, want := simpleRequestStringContent(string(raw[1:len(raw)-1])), scalarSimpleRequestJSONString(raw); got != want {
				t.Fatalf("string %x: got %v want %v", raw, got, want)
			}
		}
	}
	for size := 0; size < 40; size++ {
		raw := append([]byte{'"'}, bytes.Repeat([]byte{'A'}, size)...)
		raw = append(raw, '"')
		check(raw)
		for pos := 1; pos <= size; pos++ {
			for b := 0; b < 256; b++ {
				raw[pos] = byte(b)
				check(raw)
			}
			raw[pos] = 'A'
		}
	}
	rng := rand.New(rand.NewSource(931))
	for i := 0; i < 10000; i++ {
		raw := make([]byte, rng.Intn(129))
		_, _ = rng.Read(raw)
		if len(raw) > 1 {
			raw[0] = '"'
			raw[len(raw)-1] = '"'
		}
		check(raw)
	}
}

func TestLargeRequestJSONStringMatchesScalar(t *testing.T) {
	for size := 1020; size < 1040; size++ {
		raw := append([]byte{'"'}, bytes.Repeat([]byte{'A'}, size)...)
		raw = append(raw, '"')
		check := func() {
			t.Helper()
			if got, want := simpleRequestJSONString(raw), scalarSimpleRequestJSONString(raw); got != want {
				t.Fatalf("size=%d: got %v want %v", size, got, want)
			}
		}
		check()
		for _, pos := range []int{1, 2, size / 2, size - 1, size} {
			for b := 0; b < 256; b++ {
				raw[pos] = byte(b)
				check()
			}
			raw[pos] = 'A'
		}
		for _, tail := range []string{"=", "==", "AAA=", "AA==", "\r\nAA", `\\u0041`, "&&&&"} {
			copy(raw[len(raw)-1-len(tail):], tail)
			check()
			copy(raw[len(raw)-1-len(tail):], bytes.Repeat([]byte{'A'}, len(tail)))
		}
	}
}

func BenchmarkSimpleRequestJSONString(b *testing.B) {
	raw := append([]byte{'"'}, bytes.Repeat([]byte{'A'}, 1<<20)...)
	raw = append(raw, '"')
	for _, tc := range []struct {
		name string
		fn   func([]byte) bool
	}{{"word", simpleRequestJSONString}, {"scalar", scalarSimpleRequestJSONString}} {
		b.Run(tc.name, func(b *testing.B) {
			b.SetBytes(int64(len(raw)))
			b.ReportAllocs()
			for b.Loop() {
				if !tc.fn(raw) {
					b.Fatal("invalid")
				}
			}
		})
	}
}
