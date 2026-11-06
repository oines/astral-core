package astral

import (
	"encoding/json"
	"fmt"
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
			for _, at := range []int{len(before.Messages)} {
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
	projectionFreezeSelection(t, input, []int{0, 0, 1, 2})
	projectionFreezeSmallRecipe(t, input, image.Data, opaque, arguments, metadata)
}

func TestFrozenRequestSmallHistoryShiftsKeepReferences(t *testing.T) {
	before := Request{ModelID: "model", Messages: make([]Message, 1000)}
	for i := range before.Messages {
		before.Messages[i] = User(fmt.Sprintf("historical record %04d: %s", i, strings.Repeat("small-history-text-", 24)))
	}
	for _, test := range []struct {
		name   string
		at     int
		remove bool
	}{
		{"prepend", 0, false},
		{"insert-middle", 500, false},
		{"remove-first", 0, true},
		{"remove-middle", 500, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			actual := projectionFreezeCopy(before)
			if test.remove {
				actual.Messages = append(actual.Messages[:test.at], actual.Messages[test.at+1:]...)
			} else {
				actual.Messages = append(actual.Messages[:test.at:test.at], append([]Message{User("new short observation")}, actual.Messages[test.at:]...)...)
			}
			base := Context{HeadID: "shift-head", ConfigurationID: "shift-config", Request: before}
			input, err := FreezeRequest("shift-rollout", base, actual, ModelSpec{ID: "model", ContextWindow: 1000000})
			if err != nil {
				t.Fatal(err)
			}
			if input.Selection == nil {
				t.Fatal("shifted history lost its source selection")
			}
			for i, source := range *input.Selection {
				want := i
				if test.remove && i >= test.at {
					want++
				} else if !test.remove && i > test.at {
					want--
				} else if !test.remove && i == test.at {
					continue // The new observation needs a small change, not a particular old source.
				}
				if source != want {
					t.Fatalf("retained history message %d references %d, want %d", i, source, want)
				}
			}
			changes, err := json.Marshal(input.Changes)
			if err != nil {
				t.Fatal(err)
			}
			if len(changes) > 1024 || strings.Contains(string(changes), "small-history-text-") {
				t.Fatalf("small history shift copied source text into changes (%d bytes)", len(changes))
			}
			projectionFreezeRoundTrip(t, before, actual)
		})
	}
}

func TestFrozenRequestBoundedSmallTailUsesLiterals(t *testing.T) {
	image := projectionFreezeImage("VEFJ")
	before := Request{ModelID: "model", Messages: []Message{{Role: RoleUser, Content: []Content{image}}, User("retained")}}
	for _, count := range []int{2, 3} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			actual := projectionFreezeCopy(before)
			for i := range count {
				actual.Messages = append(actual.Messages, User(fmt.Sprintf("short tail %d: %s", i, strings.Repeat("new literal ", 30))))
			}
			input := projectionFreezeRoundTrip(t, before, actual)
			want := []int{0, 1}
			for range count {
				want = append(want, -1)
			}
			projectionFreezeSelection(t, input, want)
			projectionFreezeSmallRecipe(t, input, image.Data)
		})
	}
}
