package client

import (
	"bytes"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
)

func executionTestImage(mime, tail string) string {
	return "data:" + mime + ";base64," + strings.Repeat("eHh4", 512) + tail
}

func TestExecutionJSONMatchesStandardEncoder(t *testing.T) {
	image := executionTestImage("image/png", "")
	for name, body := range map[string]map[string]any{
		"ordinary without optimization": {"text": "<>&\u2028\u2029\n\""},
		"image and escaping":            {"image": image, "html": "<>&", "utf8": "中文😀\u2028\u2029", "invalid_utf8": string([]byte{0xff, 'x'}), "controls": "\x00\b\t\n\r\"\\", "number": json.Number("9007199254740993")},
		"nested containers":             {"image": image, "array": []any{nil, []any{}, []any(nil), map[string]any{}, map[string]any(nil), []any{image, true, 1.2}}, "strings": []string{"x", "<>&"}, "bytes": []byte{1, 2, 3}},
		"raw native payload":            {"image": image, "native": json.RawMessage(` { "opaque": "<>&", "value": 9007199254740993 } `)},
		"padding bits":                  {"image": executionTestImage("image/jpeg", "AB=="), "other": executionTestImage("image/webp", "AAB=")},
		"safe MIME":                     {"image": executionTestImage("image/svg+xml", ""), "other": executionTestImage("image/vnd.custom-test", "")},
		"unsafe MIME falls back":        {"image": image, "html": executionTestImage("image/png<script>", ""), "quote": executionTestImage("image/pn\"g", ""), "control": executionTestImage("image/pn\ng", "")},
		"escaped base64 falls back":     {"image": image, "escaped": executionTestImage("image/png", `\u0065A==`), "newlines": executionTestImage("image/png", "\r\n\r\n"), "non_ascii": executionTestImage("image/png", "中文")},
		"marker collision":              {"image": image, "literal": "\x00astral-image-0\x00"},
		"key collision":                 {"image": image, "\x00astral-image-0\x00": "literal"},
		"raw collision":                 {"image": image, "raw": json.RawMessage(`"\u0000astral-image-0\u0000"`)},
		"similar markers":               {"image": image, "short": "\x00astral-image-\x00", "suffix": "\x00astral-image-0x\x00", "leading zero": "\x00astral-image-00\x00", "unknown": "\x00astral-image-99\x00", "overflow": "\x00astral-image-999999999999999999999999999999\x00"},
		"unknown Go value":              {"image": image, "typed": map[int]string{2: "two", 1: "<one>"}},
		"invalid raw":                   {"image": image, "raw": json.RawMessage(`{`)},
		"nonfinite":                     {"image": image, "number": math.NaN()},
	} {
		t.Run(name, func(t *testing.T) {
			want, wantErr := json.Marshal(body)
			got, gotErr := marshalExecutionBody(body)
			if (wantErr == nil) != (gotErr == nil) || wantErr == nil && !bytes.Equal(want, got) || wantErr != nil && gotErr != nil && (reflect.TypeOf(wantErr) != reflect.TypeOf(gotErr) || wantErr.Error() != gotErr.Error()) {
				t.Fatalf("wire JSON changed: got %q/%v want %q/%v", got, gotErr, want, wantErr)
			}
			after, afterErr := json.Marshal(body)
			if (wantErr == nil) != (afterErr == nil) || wantErr == nil && !bytes.Equal(want, after) {
				t.Fatal("encoding changed the public body tree")
			}
		})
	}
}

func TestExecutionJSONImageUsesExactFinalCapacity(t *testing.T) {
	image := executionTestImage("image/png", "")
	body := map[string]any{"z": image, "a": []any{map[string]any{"html": "<>&", "image": image}}}
	raw, err := marshalExecutionBody(body)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(body)
	if err != nil || !bytes.Equal(raw, want) || cap(raw) != len(raw) {
		t.Fatalf("final body was not allocated once at its exact encoded size: len=%d cap=%d err=%v", len(raw), cap(raw), err)
	}
}

type executionCustomJSON struct{ calls *int }

func (v executionCustomJSON) MarshalJSON() ([]byte, error) {
	*v.calls++
	return []byte(`"custom"`), nil
}

func TestExecutionJSONUnknownAndDeepValuesUseOriginalEncoder(t *testing.T) {
	calls := 0
	body := map[string]any{"image": executionTestImage("image/png", ""), "custom": executionCustomJSON{calls: &calls}}
	if _, err := marshalExecutionBody(body); err != nil || calls != 1 {
		t.Fatalf("unknown marshaler was invoked more than once: calls=%d err=%v", calls, err)
	}
	var nested any = "end"
	for range 70 {
		nested = []any{nested}
	}
	body = map[string]any{"image": executionTestImage("image/png", ""), "deep": nested}
	want, _ := json.Marshal(body)
	got, err := marshalExecutionBody(body)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("deep fallback changed: %v", err)
	}
	cycle := map[string]any{"image": executionTestImage("image/png", "")}
	cycle["cycle"] = cycle
	_, wantErr := json.Marshal(cycle)
	_, gotErr := marshalExecutionBody(cycle)
	if wantErr == nil || gotErr == nil || wantErr.Error() != gotErr.Error() {
		t.Fatalf("cycle fallback changed: got %v want %v", gotErr, wantErr)
	}
}

func TestExecutionJSONOptimizesOnlyValidatedASCIIImageURLs(t *testing.T) {
	for _, test := range []struct {
		value string
		valid bool
	}{
		{executionTestImage("image/png", ""), true},
		{executionTestImage("image/svg+xml", "AB=="), true},
		{executionTestImage("image/png", "\r\n\r\n"), false},
		{executionTestImage("image/png", `\u0041`), false},
		{executionTestImage("image/png<>&", ""), false},
		{executionTestImage("image/png;extra", ""), false},
		{executionTestImage("image/"+strings.Repeat("a", 70), ""), false},
		{"data:image/png;base64,eA==", false},
	} {
		if got := executionASCIIImageURL(test.value); got != test.valid {
			t.Fatalf("unexpected direct-write eligibility: got=%t want=%t", got, test.valid)
		}
	}
}

func TestExecutionJSONLargeTextMatchesStandardEncoder(t *testing.T) {
	plain := strings.Repeat("Ordinary tool output 123 /._- !? ", 80)
	for name, value := range map[string]string{
		"plain":            plain,
		"html":             plain + "<>&",
		"quote":            plain + `"`,
		"backslash":        plain + `\`,
		"newline":          plain + "\n",
		"controls":         plain + "\x00\b\t\r\x1f",
		"unicode":          plain + "中文🙂\u2028\u2029",
		"invalid-utf8":     plain + string([]byte{0xff, 0xfe}),
		"marker-collision": "\x00astral-image-0\x00" + plain,
	} {
		t.Run(name, func(t *testing.T) {
			body := map[string]any{"system": plain, "messages": []any{map[string]any{"role": "tool", "content": value}, map[string]any{"role": "assistant", "content": plain}}, "raw": json.RawMessage(`{"html":"<>&","number":9007199254740993}`)}
			want, wantErr := json.Marshal(body)
			got, err := marshalExecutionBody(body)
			if wantErr != nil || err != nil || !bytes.Equal(got, want) {
				t.Fatalf("large text changed JSON bytes: old=%v new=%v", wantErr, err)
			}
			after, _ := json.Marshal(body)
			if !bytes.Equal(after, want) {
				t.Fatal("large text encoding mutated the public input")
			}
		})
	}
	for _, size := range []int{1023, 1024, 1025, 4096} {
		value := strings.Repeat("x", size)
		var replacements []executionImage
		_, changed, supported := executionImageMetadata(value, &replacements, 0)
		if !supported || changed != (size >= 1024) {
			t.Fatalf("large text cutoff changed at %d: changed=%v supported=%v", size, changed, supported)
		}
		raw, err := marshalExecutionBody(map[string]any{"content": value})
		want, _ := json.Marshal(map[string]any{"content": value})
		if err != nil || !bytes.Equal(raw, want) || changed && cap(raw) != len(raw) {
			t.Fatalf("large text final allocation/bytes changed at %d: %v", size, err)
		}
	}
}

func TestExecutionJSONLargeTextEveryByteMatchesStandardEncoder(t *testing.T) {
	for _, position := range []int{0, 7, 8, 15, 16, 1023, 1024, 2047} {
		value := bytes.Repeat([]byte{'a'}, 2048)
		for b := range 256 {
			value[position] = byte(b)
			body := map[string]any{"content": string(value)}
			want, _ := json.Marshal(body)
			got, err := marshalExecutionBody(body)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("byte %02x at %d changed JSON output: %v", b, position, err)
			}
		}
	}
}
