package astral

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFrozenRequestSmallLiteralsSkipHistorySourceSearch(t *testing.T) {
	image := projectionFreezeImage("SElT")
	opaque := json.RawMessage(`{"signature":"` + strings.Repeat("opaque-source-", 512) + `","integer":9007199254740993}`)
	before := Request{ModelID: "model", Messages: []Message{
		{Role: RoleUser, Content: []Content{image}},
		{Role: RoleAssistant, Content: []Content{{Type: "provider_opaque", Data: opaque}}},
		{Role: RoleTool, ToolCallID: "call", Name: "read", Content: Text(strings.Repeat("tool-source-", 512))},
	}}
	for _, test := range []struct {
		name    string
		message Message
	}{
		{"user", User("A new short instruction.")},
		{"assistant", Message{Role: RoleAssistant, Content: Text("A short provider replacement.")}},
		{"tool", Message{Role: RoleTool, ToolCallID: "call", Name: "read", Content: Text("A new small tool result.")}},
		{"opaque", Message{Role: RoleAssistant, Content: []Content{{Type: "provider_opaque", Data: json.RawMessage(` {"n":9007199254740993,"dup":1,"dup":2} `)}}}},
		{"context", Message{Role: RoleUser, Content: Text("A new context observation."), Metadata: json.RawMessage(`{"n":9007199254740995}`)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, at := range []int{0, len(before.Messages)} {
				actual := projectionFreezeCopy(before)
				actual.Messages = append(actual.Messages[:at:at], append([]Message{test.message}, actual.Messages[at:]...)...)
				input := projectionFreezeRoundTrip(t, before, actual)
				want := make([]int, len(actual.Messages))
				for i := range want {
					switch {
					case i < at:
						want[i] = i
					case i == at:
						want[i] = -1
					default:
						want[i] = i - 1
					}
				}
				projectionFreezeSelection(t, input, want)
				projectionFreezeSmallRecipe(t, input, image.Data, opaque)
			}
		})
	}
}

func TestFrozenRequestLiteralSizeCountsTheWholeEncodedMessage(t *testing.T) {
	for _, test := range []struct {
		name    string
		message Message
		literal bool
	}{
		{"small", User("small literal"), true},
		// No individual field is large, but the aggregate message must retain its
		// history reference rather than embed many small parts again.
		{"many-small-parts", Message{Role: RoleUser, Content: []Content{
			{Type: "text", Text: strings.Repeat("a", 1500)},
			{Type: "text", Text: strings.Repeat("b", 1500)},
			{Type: "text", Text: strings.Repeat("c", 1500)},
		}}, false},
		// JSON HTML escaping makes this small raw string exceed the literal cap.
		{"escaped-text", User(strings.Repeat("<", 900)), false},
		{"large-header", Message{Role: RoleUser, Name: strings.Repeat("name", 1500), Content: Text("header")}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := Request{ModelID: "model", Messages: []Message{User("prefix"), test.message}}
			actual := before
			actual.Messages = before.Messages[1:]
			input := projectionFreezeRoundTrip(t, before, actual)
			want := 1
			if test.literal {
				want = -1
			}
			projectionFreezeSelection(t, input, []int{want})
			projectionFreezeSmallRecipe(t, input)
		})
	}
}

func TestFrozenRequestSmallInsertionKeepsMovedAndChangedLargeFieldsReferenced(t *testing.T) {
	image := projectionFreezeImage("TU9W")
	opaque := json.RawMessage(` {"signature":"` + strings.Repeat("opaque-moved-", 512) + `","integer":9007199254740993,"dup":1,"dup":2} `)
	arguments := json.RawMessage(`{"content":"` + strings.Repeat("tool-arguments-", 512) + `","integer":9007199254740995}`)
	metadata := json.RawMessage(`{"token":"` + strings.Repeat("message-metadata-", 512) + `","integer":9007199254740997}`)
	before := Request{ModelID: "model", Messages: []Message{
		User("header"),
		{Role: RoleAssistant, Metadata: metadata, FinishReason: FinishToolCalls, Content: []Content{
			{Type: "text", Text: "old small text"},
			image,
			{Type: "reasoning", Data: opaque},
			{Type: "tool_call", ToolCall: &ToolCall{ID: "call", Name: "write", Arguments: arguments}},
		}},
		User("retained tail"),
	}}
	actual := projectionFreezeCopy(before)
	actual.Messages = append([]Message{User("new small literal")}, actual.Messages...)
	actual.Messages[2].Content[0] = Content{Type: "text", Text: "changed small text"}
	input := projectionFreezeRoundTrip(t, before, actual)
	projectionFreezeSelection(t, input, []int{-1, -1, 1, -1})
	projectionFreezeSmallRecipe(t, input, image.Data, opaque, arguments, metadata)
}
