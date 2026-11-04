package client

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"testing"

	core "github.com/oines/astral-core"
	"github.com/oines/astral-core/models"
)

func TestImagePayloadValidationKeepsJSONAndBase64Semantics(t *testing.T) {
	for _, test := range []struct {
		name, raw, want string
		valid           bool
	}{
		{"ordinary", `"eA=="`, "eA==", true},
		{"whitespace", " \n\t\"eA==\" \n", "eA==", true},
		{"escaped ASCII", `"\u0065A=="`, "eA==", true},
		{"escaped newline", `"eA==\n"`, "eA==\n", true},
		{"escaped slash", `"\/w=="`, "/w==", true},
		{"invalid raw newline", "\"eA==\n\"", "", false},
		{"invalid raw CRLF", "\"eA==\r\n\"", "", false},
		{"invalid raw tab", "\"eA==\t\"", "", false},
		{"invalid raw NUL", "\"eA==\x00\"", "", false},
		{"invalid raw quote", "\"eA==\"\"", "", false},
		{"non-JSON outer whitespace", "\u00a0\"eA==\"\u00a0", "", false},
		{"non-JSON vertical whitespace", "\v\"eA==\"", "", false},
		{"invalid UTF-8", string([]byte{'"', 0xff, '"'}), "", false},
		{"non-ASCII", `"中文"`, "", false},
		{"escaped surrogate", `"\ud800"`, "", false},
		{"escaped CRLF", `"eA==\r\n"`, "eA==\r\n", true},
		{"trailing data", `"eA==" null`, "", false},
		{"invalid base64", `"eA=!"`, "", false},
		{"empty", `""`, "", false},
		{"null", `null`, "", false},
		{"object", `{}`, "", false},
		{"invalid JSON", `"eA==`, "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := &Client{}
			part := core.Content{Type: "image", MediaType: "image/png", Data: json.RawMessage(test.raw)}
			data, err := c.imageData(part)
			if (err == nil) != test.valid || test.valid && string(data) != test.want {
				t.Fatalf("data=%q error=%v", data, err)
			}
			oldData, oldErr := legacyFrozenImagePayload(part)
			if (err == nil) != (oldErr == nil) || err == nil && !bytes.Equal(data, oldData) || err != nil && oldErr != nil && err.Error() != oldErr.Error() {
				t.Fatalf("legacy payload/error changed: got %q/%v want %q/%v", data, err, oldData, oldErr)
			}
			if string(part.Data) != test.raw {
				t.Fatal("validation changed frozen input")
			}
		})
	}
}

// The original unmarshal-first image path is the compatibility oracle for
// fallback error ordering and escaped/invalid UTF-8 handling.
func legacyFrozenImagePayload(part core.Content) ([]byte, error) {
	var encoded string
	if json.Unmarshal(part.Data, &encoded) != nil || encoded == "" || !strings.HasPrefix(part.MediaType, "image/") {
		return nil, fmt.Errorf("%w: image is missing its frozen pixel payload", core.ErrInvalid)
	}
	if base64.StdEncoding.DecodedLen(len(encoded)) > 20<<20 {
		return nil, fmt.Errorf("%w: image exceeds the 20 MiB model limit", core.ErrInvalid)
	}
	if _, err := io.Copy(io.Discard, base64.NewDecoder(base64.StdEncoding, strings.NewReader(encoded))); err != nil {
		return nil, fmt.Errorf("%w: image contains invalid base64", core.ErrInvalid)
	}
	return []byte(encoded), nil
}

func TestWireImagesCopyOnlyTheirChangedContainers(t *testing.T) {
	c := &Client{}
	request := core.Request{Messages: []core.Message{core.User("unchanged"), {Role: core.RoleUser, Content: []core.Content{{Type: "image", MediaType: "image/png", Data: json.RawMessage(`"eA=="`), URL: "host-file://original"}}}}}
	wire, err := c.WireRequest(request, false)
	if err != nil {
		t.Fatal(err)
	}
	if wire.Messages[1].Content[0].URL != "data:image/png;base64,eA==" || len(wire.Messages[1].Content[0].Data) != 0 {
		t.Fatal("wire pixels were not resolved")
	}
	if request.Messages[1].Content[0].URL != "host-file://original" || string(request.Messages[1].Content[0].Data) != `"eA=="` {
		t.Fatal("wire preparation mutated its source")
	}
	wire.Messages[1].Content[0].URL = "changed"
	if request.Messages[1].Content[0].URL != "host-file://original" {
		t.Fatal("wire preparation shares the modified content container")
	}
	// Estimate ignores bytes but retains both capability and payload checks.
	c.Config = models.Config{Spec: core.ModelSpec{ID: "target"}}
	c.Protocol = "openai-responses"
	request.ModelID = "target"
	if _, err := c.EstimateRequest(request); err != nil {
		t.Fatal(err)
	}
	c.Config.Images = "unsupported"
	if _, err := c.EstimateRequest(request); err == nil {
		t.Fatal("estimation accepted unsupported images")
	}
	c.Config.Images = "supported"
	request.Messages[1].Content[0].Data = json.RawMessage(`"` + strings.Repeat("A", (20<<20)*4/3+8) + `"`)
	if _, err := c.EstimateRequest(request); err == nil {
		t.Fatal("estimation accepted an oversized image")
	}
}

func TestBorrowedEstimationKeepsLegacyJSONNormalization(t *testing.T) {
	c := &Client{Protocol: "openai-responses", Config: models.Config{Spec: core.ModelSpec{ID: "target"}}}
	request := core.Request{ModelID: "target", Messages: []core.Message{{Role: core.RoleAssistant, Content: []core.Content{{Type: "tool_call", ToolCall: &core.ToolCall{ID: "call", Name: "lookup"}}}}}}
	if _, err := c.EstimateRequest(request); err != nil {
		t.Fatalf("required nil arguments no longer normalize to JSON null: %v", err)
	}
	if request.Messages[0].Content[0].ToolCall.Arguments != nil {
		t.Fatal("normalization mutated caller")
	}
	request.Messages[0].Content = []core.Content{
		{Type: "tool_call", ToolCall: &core.ToolCall{ID: string([]byte{0xff}), Name: "lookup", Arguments: json.RawMessage(`{}`)}},
		{Type: "tool_call", ToolCall: &core.ToolCall{ID: string([]byte{0xfe}), Name: "lookup", Arguments: json.RawMessage(`{}`)}},
	}
	if _, err := c.EstimateRequest(request); err == nil {
		t.Fatal("malformed UTF-8 identities bypassed normalized duplicate-call validation")
	}
}

func TestPlainBase64MatchesNonStrictStandardDecoder(t *testing.T) {
	check := func(encoded []byte) {
		t.Helper()
		_, err := base64.StdEncoding.DecodeString(string(encoded))
		want := len(encoded) != 0 && !bytes.ContainsAny(encoded, "\r\n") && err == nil
		if got := isPlainBase64(encoded); got != want {
			t.Fatalf("base64 classification changed for %q: got %t want %t (%v)", encoded, got, want, err)
		}
	}
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	// All possible last sextets retain the non-Strict decoder's acceptance of
	// nonzero discarded padding bits, including AB== and AAB=.
	for i := range alphabet {
		for j := range alphabet {
			check([]byte{alphabet[i], alphabet[j], '=', '='})
			check([]byte{'A', alphabet[i], alphabet[j], '='})
			check([]byte{'A', 'A', alphabet[i], alphabet[j]})
		}
	}
	for position := range 4 {
		for value := range 256 {
			encoded := []byte("AAAA")
			encoded[position] = byte(value)
			check(encoded)
		}
	}
	random := rand.New(rand.NewSource(611041))
	for i := range 100000 {
		encoded := make([]byte, random.Intn(129))
		if i%3 == 0 {
			_, _ = random.Read(encoded)
		} else {
			for k := range encoded {
				encoded[k] = alphabet[random.Intn(len(alphabet))]
			}
			if len(encoded) > 1 && i%3 == 1 {
				encoded[len(encoded)-1] = '='
				if i%2 == 0 {
					encoded[len(encoded)-2] = '='
				}
			}
			if len(encoded) > 0 && i%11 == 0 {
				encoded[random.Intn(len(encoded))] = byte(random.Intn(256))
			}
		}
		check(encoded)
	}
}

func TestImageDataRandomizedLegacyEquivalence(t *testing.T) {
	random := rand.New(rand.NewSource(611042))
	c := &Client{}
	for i := range 5000 {
		pixels := make([]byte, 1+random.Intn(96))
		_, _ = random.Read(pixels)
		encoded := []byte(base64.StdEncoding.EncodeToString(pixels))
		if i%3 == 0 {
			encoded[random.Intn(len(encoded))] = byte(random.Intn(256))
		}
		var raw []byte
		if i%2 == 0 {
			raw = append(append([]byte{'"'}, encoded...), '"')
		} else {
			raw, _ = json.Marshal(string(encoded))
		}
		switch i % 7 {
		case 0:
			raw = append(append([]byte("\r\n\t"), raw...), ' ')
		case 1:
			raw = append([]byte("\u00a0"), raw...)
		case 2:
			if len(raw) > 2 {
				raw = append(raw[:len(raw)-1], '\n', '"')
			}
		case 3:
			raw = append(raw, []byte(" null")...)
		}
		part := core.Content{Type: "image", MediaType: "image/png", Data: raw}
		if i%13 == 0 {
			part.MediaType = "application/octet-stream"
		}
		want, wantErr := legacyFrozenImagePayload(part)
		got, gotErr := c.imageData(part)
		if (wantErr == nil) != (gotErr == nil) || wantErr == nil && !bytes.Equal(want, got) || wantErr != nil && gotErr != nil && wantErr.Error() != gotErr.Error() {
			t.Fatalf("image fallback changed at seed case %d (%q): got %q/%v want %q/%v", i, raw, got, gotErr, want, wantErr)
		}
	}
}
