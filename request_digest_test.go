package astral

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

func oldCanonicalRequestBytes(request Request) ([]byte, error) {
	raw, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func checkCanonicalRequest(t *testing.T, request Request) {
	t.Helper()
	// Both persisted JSON representations must stay byte-identical.
	typed, typedErr := json.Marshal(request)
	var typedOut bytes.Buffer
	streamErr := writeTypedRequest(&typedOut, request, nil)
	if (typedErr == nil) != (streamErr == nil) || typedErr == nil && !bytes.Equal(typed, typedOut.Bytes()) {
		t.Fatalf("typed JSON changed: old=%v new=%v\nold=%s\nnew=%s", typedErr, streamErr, typed, typedOut.Bytes())
	}
	if typedErr == nil {
		n, sizeErr := typedRequestSize(request)
		if sizeErr != nil || n != len(typed) {
			t.Fatalf("typed size changed: got=%d want=%d err=%v", n, len(typed), sizeErr)
		}
	}
	want, oldErr := oldCanonicalRequestBytes(request)
	var out bytes.Buffer
	err := writeCanonicalRequest(&out, request)
	if oldErr != nil {
		if err == nil || canonicalRequestDigest(request) != "" {
			t.Fatalf("invalid request acquired a digest: old=%v new=%v", oldErr, err)
		}
		return
	}
	if err != nil || !bytes.Equal(out.Bytes(), want) {
		t.Fatalf("canonical bytes changed: err=%v\nold=%s\nnew=%s", err, want, out.Bytes())
	}
	sum := sha256.Sum256(want)
	if got := canonicalRequestDigest(request); got != hex.EncodeToString(sum[:]) || got != canonicalDigest(request) {
		t.Fatalf("canonical digest changed: old=%s new=%s", hex.EncodeToString(sum[:]), got)
	}
}

func TestCanonicalRequestDigestMatchesLegacyBoundaries(t *testing.T) {
	cases := map[string]Request{
		"zero":          {},
		"all-fields":    completeCopyRequest(),
		"empty-arrays":  {Messages: []Message{}, Tools: []ToolDefinition{}},
		"empty-content": {Messages: []Message{{Content: []Content{}}}},
		"large-image":   {Messages: []Message{{Role: RoleUser, Content: []Content{{Type: "image", Data: json.RawMessage(`"` + strings.Repeat("Ab09+/=", 4096) + `"`)}}}}},
		"invalid-UTF8":  {ModelID: "bad\xff", ModelRevision: "\xfe", SystemPrompt: "\xff\xfe", Messages: []Message{{Role: Role("\xff"), Name: "\xfe", Content: []Content{{Type: "\xff", Text: "\xff", URL: "\xff", MediaType: "\xff"}}, FinishReason: "\xff", ToolCallID: "\xff", ContextKind: "\xff", ErrorMessage: "\xff"}}},
		"UTF8-map-key-collision": {Options: GenerationOptions{ProviderOptions: map[string]json.RawMessage{
			"\xfe": json.RawMessage(`1`), "\xff": json.RawMessage(`2`), "�": json.RawMessage(`3`),
		}}},
		"nonfinite-options":  {Options: GenerationOptions{Temperature: copyPointer(math.NaN())}},
		"nil-required-raw":   {Messages: []Message{{Content: []Content{{ToolCall: &ToolCall{}}}}}, Tools: []ToolDefinition{{}}},
		"empty-required-raw": {Messages: []Message{{Content: []Content{{ToolCall: &ToolCall{Arguments: json.RawMessage{}}}}}}},
	}
	for name, request := range cases {
		t.Run(name, func(t *testing.T) { checkCanonicalRequest(t, request) })
	}
	for i, raw := range digestRawCases() {
		t.Run("opaque-"+string(rune('A'+i)), func(t *testing.T) {
			request := completeCopyRequest()
			request.Messages[0].Metadata = raw
			request.Messages[0].Content[0].Data = raw
			request.Messages[0].Content[0].Metadata = raw
			request.Messages[0].Content[1].ToolCall.Arguments = raw
			request.Messages[0].Content[1].ToolCall.Metadata = raw
			request.Tools[0].Parameters = raw
			request.Options.ProviderOptions["opaque"] = raw
			checkCanonicalRequest(t, request)
		})
	}
	// Every byte exercises UTF-8 replacement and JSON escaping independently
	// from the random corpus, including control characters and DEL.
	for value := range 256 {
		request := Request{ModelID: string([]byte{byte(value)}), Messages: []Message{{Content: []Content{{Type: string([]byte{byte(value)})}}}}}
		checkCanonicalRequest(t, request)
	}
}

func digestRawCases() []json.RawMessage {
	return []json.RawMessage{
		nil, {}, json.RawMessage(`null`), json.RawMessage(`false`), json.RawMessage(`true`),
		json.RawMessage(`0`), json.RawMessage(`-0`), json.RawMessage(`1.00`), json.RawMessage(`1e+03`),
		json.RawMessage(`900719925474099312345678901234567890`), json.RawMessage(`1e99999`),
		json.RawMessage(`""`), json.RawMessage(`"Ab09+/="`), json.RawMessage(` "escaped\n\u0061<>&\u2028\u2029" `),
		json.RawMessage{'"', 0xff, '"'}, json.RawMessage(`"\ud800\udfff"`),
		json.RawMessage(` {"z":1,"a":2,"z":3,"\u0061":4} `),
		json.RawMessage(`{"\ufffd":1,"\ud800":2,"�":3}`),
		json.RawMessage(`{"nested":[{"z":0,"a":1},[],null,-0,1.0,1E-2]}`),
		json.RawMessage(`{"a":1`), json.RawMessage(`{} {}`), json.RawMessage(`"unterminated`),
	}
}

func TestCanonicalRequestDigestRandomDifferential(t *testing.T) {
	rng := rand.New(rand.NewSource(84283920))
	strings := []string{"", "plain", "<>&", "quote\"\\\n", "汉字🙂\u2028\u2029", "invalid\xff\xfe", "�"}
	raws := digestRawCases()
	text := func() string { return strings[rng.Intn(len(strings))] }
	raw := func() json.RawMessage { return raws[rng.Intn(len(raws))] }
	for sample := 0; sample < 600; sample++ {
		request := Request{ModelID: text(), ModelRevision: text(), SystemPrompt: text(), MaxOutputTokens: rng.Intn(3000) - 1000}
		if rng.Intn(2) == 0 {
			request.Messages = []Message{}
		}
		for range rng.Intn(5) {
			message := Message{Role: Role(text()), Name: text(), ContextKind: text(), ToolCallID: text(), IsError: rng.Intn(2) == 0, Metadata: raw(), FinishReason: FinishReason(text()), ErrorMessage: text()}
			if rng.Intn(2) == 0 {
				message.Content = []Content{}
			}
			for range rng.Intn(5) {
				part := Content{Type: text(), Text: text(), MediaType: text(), URL: text(), Data: raw(), Metadata: raw()}
				if rng.Intn(2) == 0 {
					part.ToolCall = &ToolCall{ID: text(), Name: text(), Arguments: raw(), Metadata: raw()}
				}
				message.Content = append(message.Content, part)
			}
			request.Messages = append(request.Messages, message)
		}
		if rng.Intn(2) == 0 {
			request.Options = completeCopyRequest().Options
			request.Options.ProviderOptions[text()] = raw()
		}
		if rng.Intn(2) == 0 {
			request.Tools = []ToolDefinition{{Name: text(), Description: text(), Parameters: raw(), Version: text(), Sequential: rng.Intn(2) == 0, Exclusive: rng.Intn(2) == 0, MayEndRun: rng.Intn(2) == 0}}
		}
		checkCanonicalRequest(t, request)
	}
}

type failedDigestWriter struct{ err error }

func (w failedDigestWriter) Write([]byte) (int, error) { return 0, w.err }

func TestCanonicalRequestWriterPropagatesOutputFailure(t *testing.T) {
	want := errors.New("output failed")
	if err := writeCanonicalRequest(failedDigestWriter{err: want}, completeCopyRequest()); !errors.Is(err, want) {
		t.Fatalf("writer failure was lost: %v", err)
	}
}

func TestCanonicalRequestDigestFieldCoverage(t *testing.T) {
	for typ, fields := range map[reflect.Type]string{
		reflect.TypeFor[Request]():  "ModelID ModelRevision SystemPrompt Messages Tools MaxOutputTokens Options",
		reflect.TypeFor[Message]():  "Role Content ToolCallID Name IsError ContextKind Metadata FinishReason ErrorMessage",
		reflect.TypeFor[Content]():  "Type Text MediaType URL Data Metadata ToolCall",
		reflect.TypeFor[ToolCall](): "ID Name Arguments Metadata",
	} {
		var got []string
		for i := 0; i < typ.NumField(); i++ {
			got = append(got, typ.Field(i).Name)
		}
		if strings.Join(got, " ") != fields {
			t.Fatalf("%s fields changed; update the canonical writer to preserve persisted digests", typ)
		}
	}
}
