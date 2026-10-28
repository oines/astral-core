package astral

import (
	"encoding/json"
	"errors"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

func TestFrozenRequestKeepsOpaqueContentByReference(t *testing.T) {
	image := strings.Repeat("image-payload-", 10000)
	base := Context{HeadID: "head", ConfigurationID: "config", Request: Request{ModelID: "model", Messages: []Message{
		User("duplicate"), User("duplicate"),
		{Role: RoleAssistant, Content: []Content{{Type: "reasoning", Data: json.RawMessage(`{"signature":"opaque","number":9007199254740993}`)}, {Type: "image", MediaType: "image/png", Data: json.RawMessage(`"` + image + `"`)}, {Type: "text", Text: "answer"}}},
	}}}
	actual := clone(base.Request)
	actual.Messages = actual.Messages[1:]
	actual.Messages[1].Content = actual.Messages[1].Content[1:]
	actual.SystemPrompt, actual.MaxOutputTokens = "frozen environment", 1000
	input, err := FreezeRequest("rollout", base, actual, ModelSpec{ID: "model", ContextWindow: 100000})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(input)
	if strings.Contains(string(raw), "image-payload-") || len(raw) > 2000 {
		t.Fatalf("unchanged image was copied into the input recipe (%d bytes)", len(raw))
	}
	rebuilt, err := input.Apply(base.Request)
	if err != nil || !reflect.DeepEqual(rebuilt, actual) {
		t.Fatalf("rebuild differs: %v", err)
	}
	input.Digest = strings.Repeat("0", 64)
	if _, err = input.Apply(base.Request); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("tampered digest accepted: %v", err)
	}
}

func TestFrozenRequestOwnershipRequiresUnchangedCompleteContent(t *testing.T) {
	original := Message{Role: RoleUser, ContextKind: "im.observation", Content: Text("original observation"), Metadata: json.RawMessage(`{"event":"original"}`)}
	base := Context{HeadID: "head", ConfigurationID: "config", Sources: []string{"original-entry"}, Request: Request{ModelID: "model", Messages: []Message{original}}}
	for _, test := range []struct {
		name    string
		message Message
		source  string
	}{
		{"replacement", Message{Role: RoleUser, ContextKind: original.ContextKind, Content: Text("entirely unrelated"), Metadata: json.RawMessage(`{"event":"replacement"}`)}, ""},
		{"metadata-only", Message{Role: original.Role, ContextKind: original.ContextKind, Content: original.Content, Metadata: json.RawMessage(`{"provider":"prepared"}`)}, "original-entry"},
		{"merge", Message{Role: original.Role, ContextKind: original.ContextKind, Content: append(append([]Content(nil), original.Content...), Text("another message")...)}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			actual := clone(base.Request)
			actual.Messages = []Message{test.message}
			input, err := FreezeRequest("rollout", base, actual, ModelSpec{ID: "model"})
			if err != nil {
				t.Fatal(err)
			}
			if len(input.Sources) != 1 || input.Sources[0] != test.source {
				t.Fatalf("unproven ownership: %v want %q", input.Sources, test.source)
			}
			if rebuilt, err := input.Apply(base.Request); err != nil || !reflect.DeepEqual(rebuilt, actual) {
				t.Fatalf("ownership policy changed reconstruction: %v", err)
			}
		})
	}
}

func TestRequestChangesRoundTripStructuralEdits(t *testing.T) {
	random := rand.New(rand.NewSource(7))
	var value func(int) any
	value = func(depth int) any {
		if depth == 0 {
			return random.Int63()
		}
		switch random.Intn(5) {
		case 0:
			return nil
		case 1:
			return "opaque~key/path"
		case 2:
			return random.Intn(2) == 0
		case 3:
			a := make([]any, random.Intn(5))
			for i := range a {
				a[i] = value(depth - 1)
			}
			return a
		default:
			m := map[string]any{}
			for i := 0; i < random.Intn(5); i++ {
				m[string(rune('a'+i))] = value(depth - 1)
			}
			return m
		}
	}
	for n := 0; n < 500; n++ {
		before, _ := json.Marshal(value(3))
		after, _ := json.Marshal(value(3))
		rebuilt := json.RawMessage(before)
		for _, change := range diffRequestJSON(nil, before, after) {
			var err error
			rebuilt, err = applyRequestChange(rebuilt, change.Path, change)
			if err != nil {
				t.Fatalf("case %d: %v", n, err)
			}
		}
		if !equalRequestJSON(rebuilt, after) {
			t.Fatalf("case %d: %s != %s", n, rebuilt, after)
		}
	}
	if canonicalDigest(json.RawMessage(`{"n":9007199254740992}`)) == canonicalDigest(json.RawMessage(`{"n":9007199254740993}`)) {
		t.Fatal("large integer precision was lost")
	}
}
