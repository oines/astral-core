package astral

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

func projectionFreezeImage(marker string) Content {
	// Valid JSON/base64 well above the 4 KiB large-leaf threshold, without any
	// image decoder or provider dependency in these frozen-recipe tests.
	return Content{Type: "image", MediaType: "image/png", Data: json.RawMessage(`"` + strings.Repeat(marker, 2048) + `"`)}
}

// Keep the fixture's native RawMessage bytes and nil/empty distinctions.
// Public CopyRequest intentionally takes a JSON ownership boundary and is not
// an appropriate byte-for-byte snapshot oracle for these history assertions.
func projectionFreezeCopy(source Request) Request {
	out := source
	if source.Messages != nil {
		out.Messages = make([]Message, len(source.Messages))
	}
	for i, message := range source.Messages {
		out.Messages[i] = message
		out.Messages[i].Metadata = bytes.Clone(message.Metadata)
		if message.Content != nil {
			out.Messages[i].Content = make([]Content, len(message.Content))
		}
		for j, content := range message.Content {
			out.Messages[i].Content[j] = content
			out.Messages[i].Content[j].Data = bytes.Clone(content.Data)
			out.Messages[i].Content[j].Metadata = bytes.Clone(content.Metadata)
			if content.ToolCall != nil {
				call := *content.ToolCall
				call.Arguments = bytes.Clone(call.Arguments)
				call.Metadata = bytes.Clone(call.Metadata)
				out.Messages[i].Content[j].ToolCall = &call
			}
		}
	}
	return out
}

func projectionFreezeRoundTrip(t *testing.T, before, actual Request) RequestInput {
	t.Helper()
	savedBefore, savedActual := projectionFreezeCopy(before), projectionFreezeCopy(actual)
	if !reflect.DeepEqual(before, savedBefore) || !reflect.DeepEqual(actual, savedActual) {
		t.Fatal("fixture snapshot changed its original representation before Freeze")
	}
	base := Context{HeadID: "projection-head", ConfigurationID: "projection-config", Request: before}
	input, err := FreezeRequest("projection-rollout", base, actual, ModelSpec{ID: actual.ModelID, Revision: actual.ModelRevision, ContextWindow: 100000, MaxOutputTokens: 4096})
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err := input.Apply(before)
	if err != nil {
		t.Fatalf("frozen projection failed to apply: %v", err)
	}
	// The pre-optimization JSON implementation is an independent Apply oracle:
	// retain its RawMessage byte behavior, duplicate keys and numeric precision.
	legacy, err := legacyRequestInputApply(input, before)
	if err != nil || !reflect.DeepEqual(rebuilt, legacy) {
		t.Fatalf("projection changed legacy Apply semantics: %v", err)
	}
	rebuiltWire, err := json.Marshal(rebuilt)
	if err != nil {
		t.Fatal(err)
	}
	legacyWire, _ := json.Marshal(legacy)
	if !bytes.Equal(rebuiltWire, legacyWire) || requestDigest(rebuilt) != requestDigest(actual) {
		t.Fatal("projection lost request bytes or changed its frozen digest")
	}
	if !reflect.DeepEqual(before, savedBefore) || !reflect.DeepEqual(actual, savedActual) {
		t.Fatal("freezing/applying a projection mutated the original request/history")
	}
	return input
}

func projectionFreezeSelection(t *testing.T, input RequestInput, want []int) {
	t.Helper()
	got := make([]int, len(want))
	for i := range got {
		got[i] = i // nil Selection is the identity selection.
	}
	if input.Selection != nil {
		got = *input.Selection
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("wrong original-message references: got %v, want %v", got, want)
	}
}

func projectionFreezeSmallRecipe(t *testing.T, input RequestInput, large ...json.RawMessage) {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > 4096 {
		t.Fatalf("small projection copied large context into its recipe: %d bytes", len(raw))
	}
	for _, payload := range large {
		// Every supplied payload is >4 KiB; a 128-byte interior marker detects
		// embedding even when JSON escaping changes its surrounding envelope.
		if len(payload) > 256 && bytes.Contains(raw, payload[64:192]) {
			t.Fatal("unchanged/removed large payload was embedded in frozen changes")
		}
	}
}

func TestProjectionFreezeSelectsSwappedLargeImages(t *testing.T) {
	a, b := projectionFreezeImage("QUFB"), projectionFreezeImage("QkJC")
	for _, leading := range []bool{false, true} {
		t.Run(fmt.Sprintf("leading=%v", leading), func(t *testing.T) {
			before := Request{ModelID: "model", Messages: []Message{{Role: RoleUser, Content: []Content{a}}, {Role: RoleUser, Content: []Content{b}}}}
			want := []int{1, 0}
			if leading {
				before.Messages = append([]Message{User("retained header")}, before.Messages...)
				want = []int{0, 2, 1}
			}
			actual := projectionFreezeCopy(before)
			n := len(actual.Messages)
			actual.Messages[n-2], actual.Messages[n-1] = actual.Messages[n-1], actual.Messages[n-2]
			input := projectionFreezeRoundTrip(t, before, actual)
			projectionFreezeSelection(t, input, want)
			projectionFreezeSmallRecipe(t, input, a.Data, b.Data)
		})
	}
}

func TestProjectionFreezeReplacesImageWithSmallPlaceholderAtOriginalPosition(t *testing.T) {
	image := projectionFreezeImage("Q0ND")
	before := Request{ModelID: "model", Messages: []Message{User("header"), {Role: RoleUser, Content: []Content{image}}, User("tail")}}
	actual := projectionFreezeCopy(before)
	actual.Messages[1].Content[0] = Content{Type: "text", Text: "[image omitted]"}
	input := projectionFreezeRoundTrip(t, before, actual)
	projectionFreezeSelection(t, input, []int{0, 1, 2})
	projectionFreezeSmallRecipe(t, input, image.Data)
}

func TestProjectionFreezeCollapsesImagesWithoutCopyingOtherLargeLeaves(t *testing.T) {
	a, b := projectionFreezeImage("RERF"), projectionFreezeImage("RkZH")
	opaque := json.RawMessage(` {"signature":"` + strings.Repeat("opaque-native-", 512) + `","integer":9007199254740993,"duplicates":{"x":1,"x":2},"binary":"\u0000\u00ff\ud83e\ude90"} `)
	arguments := json.RawMessage(` {"path":"file","content":"` + strings.Repeat("unchanged-tool-arguments-", 256) + `","integer":9007199254740995} `)
	metadata := json.RawMessage(` {"token":"` + strings.Repeat("unchanged-metadata-", 256) + `","integer":9007199254740997} `)
	before := Request{ModelID: "model", Messages: []Message{
		User("header"),
		{Role: RoleAssistant, FinishReason: FinishToolCalls, Content: []Content{
			{Type: "text", Text: "unchanged prefix"},
			{Type: "provider_opaque", Data: opaque},
			a, b,
			{Type: "provider_opaque", Data: opaque, Metadata: metadata},
			{Type: "tool_call", ToolCall: &ToolCall{ID: "opaque-call", Name: "write", Arguments: arguments, Metadata: json.RawMessage(`{"integer":9007199254740999}`)}},
			{Type: "text", Text: "unchanged suffix"},
		}},
	}}
	actual := projectionFreezeCopy(before)
	actual.Messages[1].Content = append(append(append([]Content(nil), actual.Messages[1].Content[:2]...), Content{Type: "text", Text: "[2 images omitted]"}), actual.Messages[1].Content[4:]...)
	input := projectionFreezeRoundTrip(t, before, actual)
	projectionFreezeSelection(t, input, []int{0, 1})
	projectionFreezeSmallRecipe(t, input, a.Data, b.Data, opaque, arguments, metadata)
	for _, change := range input.Changes {
		if len(change.Path) < 4 || change.Path[0] != "messages" || change.Path[1] != "1" || change.Path[2] != "content" {
			t.Fatalf("content-only collapse rewrote a message/array envelope: %+v", change.Path)
		}
	}
}

func TestProjectionFreezePreservesChangedOpaquePrecisionAndJSONBytes(t *testing.T) {
	image := projectionFreezeImage("SElK")
	before := Request{ModelID: "model", Messages: []Message{{Role: RoleAssistant, Content: []Content{
		{Type: "provider_opaque", Data: json.RawMessage(` {"b":2,"a":9007199254740993,"dup":1,"dup":2,"binary":"\u0000\u00ff"} `)},
		image,
		{Type: "tool_call", ToolCall: &ToolCall{ID: "call", Name: "read", Arguments: json.RawMessage(` {"path":"x","integer":9007199254740993,"dup":1,"dup":2} `), Metadata: json.RawMessage(`{"m":9007199254740993}`)}},
	}, FinishReason: FinishToolCalls}}}
	actual := projectionFreezeCopy(before)
	actual.Messages[0].Content[0].Data = json.RawMessage(` {"b":2,"a":9007199254740995,"dup":1,"dup":2,"binary":"\u0000\u00ff"} `)
	actual.Messages[0].Content[2].ToolCall.Arguments = json.RawMessage(` {"path":"x","integer":9007199254740997,"dup":1,"dup":2} `)
	actual.Messages[0].Content[2].ToolCall.Metadata = json.RawMessage(`{"m":9007199254740999}`)
	input := projectionFreezeRoundTrip(t, before, actual)
	projectionFreezeSmallRecipe(t, input, image.Data)
	rebuilt, err := input.Apply(before)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		raw  json.RawMessage
		want string
	}{
		{rebuilt.Messages[0].Content[0].Data, "9007199254740995"},
		{rebuilt.Messages[0].Content[2].ToolCall.Arguments, "9007199254740997"},
		{rebuilt.Messages[0].Content[2].ToolCall.Metadata, "9007199254740999"},
	} {
		if !bytes.Contains(test.raw, []byte(test.want)) {
			t.Fatalf("opaque/tool-call integer lost precision: %s", test.raw)
		}
	}
}

func TestProjectionFreezeEmptyAndPresentContentRoundTrip(t *testing.T) {
	for _, test := range []struct {
		name          string
		before, after []Content
	}{
		{"nil-to-text", nil, Text("new content")},
		{"empty-to-text", []Content{}, Text("new content")},
		{"text-to-nil", Text("old content"), nil},
		{"text-to-empty", Text("old content"), []Content{}},
		{"nil-to-empty", nil, []Content{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := Request{ModelID: "model", Messages: []Message{{Role: RoleUser, Content: test.before}}}
			actual := Request{ModelID: "model", Messages: []Message{{Role: RoleUser, Content: test.after}}}
			projectionFreezeRoundTrip(t, before, actual)
		})
	}
}

func TestProjectionFreezeRandomSmallRequestsRoundTrip(t *testing.T) {
	random := rand.New(rand.NewSource(20261106))
	message := func(index int) Message {
		result := User(fmt.Sprintf("text-%d-%d", index, random.Intn(10)))
		result.ContextKind = []string{"", "passive", "observation"}[random.Intn(3)]
		result.Metadata = json.RawMessage(fmt.Sprintf(`{"large":%d,"key":"%d"}`, int64(9007199254740993)+int64(index), random.Intn(5)))
		for range random.Intn(4) {
			result.Content = append(result.Content, Content{Type: "provider_opaque", Data: json.RawMessage(fmt.Sprintf(`{"b":%d,"a":%d,"dup":1,"dup":2}`, random.Intn(10), int64(9007199254740993)+int64(index)))})
		}
		switch random.Intn(6) {
		case 0:
			result.Content = nil
		case 1:
			result.Content = []Content{}
		}
		return result
	}
	for n := range 150 {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			before := Request{ModelID: "model", SystemPrompt: "system", MaxOutputTokens: 100}
			for i := range random.Intn(6) + 1 {
				before.Messages = append(before.Messages, message(i))
			}
			actual := projectionFreezeCopy(before)
			if random.Intn(2) == 0 {
				random.Shuffle(len(actual.Messages), func(i, j int) { actual.Messages[i], actual.Messages[j] = actual.Messages[j], actual.Messages[i] })
			}
			if len(actual.Messages) > 1 && random.Intn(2) == 0 {
				i := random.Intn(len(actual.Messages))
				actual.Messages = append(actual.Messages[:i], actual.Messages[i+1:]...)
			}
			if random.Intn(2) == 0 {
				actual.Messages = append(actual.Messages, message(n+10))
			}
			for i := range actual.Messages {
				content := actual.Messages[i].Content
				switch random.Intn(6) {
				case 0:
					part := Content{Type: "text", Text: fmt.Sprintf("replacement-%d-%d", n, i)}
					if len(content) == 0 {
						content = []Content{part}
					} else {
						content[random.Intn(len(content))] = part
					}
				case 1:
					at := random.Intn(len(content) + 1)
					content = append(content[:at:at], append([]Content{{Type: "text", Text: "inserted"}}, content[at:]...)...)
				case 2:
					if len(content) > 1 {
						content = content[:len(content)-1]
					}
				case 3:
					content = nil
				case 4:
					content = []Content{}
				}
				actual.Messages[i].Content = content
			}
			actual.SystemPrompt += fmt.Sprint(n)
			actual.MaxOutputTokens += n
			projectionFreezeRoundTrip(t, before, actual)
		})
	}
}
