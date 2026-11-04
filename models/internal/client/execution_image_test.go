package client

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	core "github.com/oines/astral-core"
)

func TestExecutionImageBorrowsPixelsByFinalContentAddress(t *testing.T) {
	c := &Client{}
	raw := json.RawMessage(`"` + strings.Repeat("eHh4", 512) + `"`)
	request := core.Request{Messages: []core.Message{{Role: core.RoleUser, Content: []core.Content{{Type: "text", Text: "keep"}, {Type: "image", MediaType: "image/png", Data: raw}, {Type: "image", MediaType: "image/jpeg", Data: raw}}}}}
	images := make(ExecutionImages)
	wire, err := c.WireRequest(request, false, images)
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 2 {
		t.Fatalf("deferred images=%d", len(images))
	}
	for i := 1; i <= 2; i++ {
		part := &wire.Messages[0].Content[i]
		value, ok := images.URL(part).(executionImageValue)
		if !ok || &value.data[0] != &raw[1] || part.URL != value.prefix || len(part.Data) != 0 {
			t.Fatal("execution materialized pixels or lost its final COW address")
		}
		copy := *part
		if _, ok := images.URL(&copy).(executionImageValue); ok {
			t.Fatal("deferred image lookup depends on content/string identity instead of the selected container")
		}
	}
	if string(request.Messages[0].Content[1].Data) != string(raw) || request.Messages[0].Content[1].URL != "" {
		t.Fatal("deferred projection mutated the frozen source")
	}
	public, err := c.WireRequest(request, false)
	if err != nil || public.Messages[0].Content[1].URL != "data:image/png;base64,"+string(raw[1:len(raw)-1]) {
		t.Fatalf("ordinary wire projection changed: %v", err)
	}
}

func TestExecutionImageKeepsExceptionalPayloadsOnOriginalPath(t *testing.T) {
	plain := strings.Repeat("eHh4", 512)
	for _, part := range []core.Content{
		{Type: "image", MediaType: "image/png", Data: json.RawMessage(`"\u0065` + plain[1:] + `"`)},
		{Type: "image", MediaType: "image/png", Data: json.RawMessage(`"` + plain + `\r\n"`)},
		{Type: "image", MediaType: "image/pn<>&g", Data: json.RawMessage(`"` + plain + `"`)},
		{Type: "image", MediaType: "image/pn\"g", Data: json.RawMessage(`"` + plain + `"`)},
		{Type: "image", MediaType: "image/png", Data: json.RawMessage(`"eA=="`)},
		{Type: "image", URL: "https://example.invalid/image.png"},
	} {
		c := &Client{}
		request := core.Request{Messages: []core.Message{{Role: core.RoleUser, Content: []core.Content{part}}}}
		want, oldErr := c.WireRequest(request, false)
		images := make(ExecutionImages)
		got, err := c.WireRequest(request, false, images)
		if err != nil || oldErr != nil || !reflect.DeepEqual(got, want) || len(images) != 0 {
			t.Fatalf("exceptional image no longer takes the original path: %v / %v", err, oldErr)
		}
	}
}

func TestDeferredExecutionImageEncodingAndFallbackMatchStrings(t *testing.T) {
	data := []byte(strings.Repeat("eHh4", 512))
	for _, prefix := range []string{"data:image/png;base64,", ""} {
		image := executionImageValue{prefix: prefix, data: data}
		for name, extra := range map[string]any{
			"ordinary":      "<>&\u2028中文\xff",
			"collision":     "\x00astral-image-0\x00",
			"raw collision": json.RawMessage(`"\u0000astral-image-0\u0000"`),
			"unknown tree":  map[int]string{1: "<>&"},
		} {
			t.Run(prefix+name, func(t *testing.T) {
				body := map[string]any{"image": image, "extra": extra}
				want, err := json.Marshal(map[string]any{"image": prefix + string(data), "extra": extra})
				if err != nil {
					t.Fatal(err)
				}
				got, err := marshalExecutionBody(body)
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("deferred encoding/fallback changed bytes: %v", err)
				}
			})
		}
	}
}
