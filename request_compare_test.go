package astral

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
)

func TestFreezeRecordsToolMetadataOnlyChange(t *testing.T) {
	call := ToolCall{ID: "call", Name: "read", Arguments: json.RawMessage(`{"path":"x"}`), Metadata: json.RawMessage(`{"revision":1}`)}
	before := Message{Role: RoleAssistant, Content: []Content{{Type: "tool_call", ToolCall: &call}}, FinishReason: FinishToolCalls}
	base := Context{HeadID: "head", ConfigurationID: "config", Request: Request{ModelID: "model", Messages: []Message{before}}}
	after := cloneRequest(base.Request)
	after.Messages[0].Content[0].ToolCall.Metadata = json.RawMessage(`{"revision":2}`)
	input, err := FreezeRequest("rollout", base, after, ModelSpec{ID: "model"})
	if err != nil {
		t.Fatal(err)
	}
	actual, err := input.Apply(base.Request)
	if err != nil || !sameJSON(actual, after) {
		t.Fatalf("metadata change lost: %v", err)
	}
}

func TestMessageComparisonMatchesJSON(t *testing.T) {
	values := []Message{
		User("text"), User("<>&\u2028"), User(string([]byte{255})), User("\ufffd"),
		{Role: RoleUser}, {Role: RoleUser, Content: []Content{}},
		{Role: RoleUser, Content: []Content{{Type: "image", Data: json.RawMessage(`"YWJj"`)}}},
		{Role: RoleUser, Content: []Content{{Type: "image", Data: json.RawMessage(` "YWJj" `)}}},
		{Role: RoleUser, Metadata: json.RawMessage(`{"x":1,"y":2}`)},
		{Role: RoleUser, Metadata: json.RawMessage(`{ "x":1, "y":2 }`)},
		{Role: RoleUser, Metadata: json.RawMessage(`{"y":2,"x":1}`)},
	}
	for i, a := range values {
		for j, b := range values {
			if got, want := sameRequestMessage(a, b), sameJSON(a, b); got != want {
				t.Fatalf("%d/%d: comparison=%t JSON=%t", i, j, got, want)
			}
		}
	}
}

func TestContextDigestStreamingMatchesLegacy(t *testing.T) {
	for _, messages := range [][]Message{nil, {}, {User("<>&\u2028")}, {{Role: RoleUser, Content: []Content{{Type: "image", Data: json.RawMessage(` "YWJj" `)}}}}} {
		request := Request{ModelID: "m", SystemPrompt: `x,"messages":[]`, Messages: messages, MaxOutputTokens: 77}
		old := request
		old.MaxOutputTokens = 0
		raw, err := json.Marshal(old)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := contextDigest(request), testContextDigestBytes(raw); got != want {
			t.Fatalf("streamed=%s legacy=%s", got, want)
		}
	}
}

func testContextDigestBytes(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
