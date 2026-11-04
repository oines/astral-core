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
